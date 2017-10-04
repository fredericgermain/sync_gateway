package rest

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/couchbase/sync_gateway/base"
	"github.com/couchbase/sync_gateway/channels"
	"github.com/couchbase/sync_gateway/db"
	"runtime/debug"
	"encoding/json"
	"github.com/couchbase/sg-bucket"
)

// Testing utilities that have been included in the rest package so that they
// are available to any package that imports rest.  (if they were in a _test.go
// file, they wouldn't be publicly exported to other packages)

var gBucketCounter = 0

type RestTester struct {
	RestTesterBucket        base.Bucket
	RestTesterServerContext *ServerContext
	noAdminParty            bool      // Unless this is true, Admin Party is in full effect
	distributedIndex        bool      // Test with walrus-based index bucket
	SyncFn                  string    // put the sync() function source in here (optional)
	DatabaseConfig          *DbConfig // Supports additional config options.  BucketConfig, Name, Sync, Unsupported will be ignored (overridden)
}

func (rt *RestTester) Bucket() base.Bucket {
	if rt.RestTesterBucket == nil {
		// Initialize the bucket.  For couchbase-backed tests, triggers with creation/flushing of the bucket
		base.GetBucketOrPanic() // side effect of creating/flushing bucket
		spec := base.GetTestBucketSpec(base.DataBucket)

		username, password, _ := spec.Auth.GetCredentials()

		server := spec.Server
		gBucketCounter++

		var syncFnPtr *string
		if len(rt.SyncFn) > 0 {
			syncFnPtr = &rt.SyncFn
		}

		corsConfig := &CORSConfig{
			Origin:      []string{"http://example.com", "*", "http://staging.example.com"},
			LoginOrigin: []string{"http://example.com"},
			Headers:     []string{},
			MaxAge:      1728000,
		}

		rt.RestTesterServerContext = NewServerContext(&ServerConfig{
			CORS:           corsConfig,
			Facebook:       &FacebookConfig{},
			AdminInterface: &DefaultAdminInterface,
		})

		useXattrs := base.TestUseXattrs()

		if rt.DatabaseConfig == nil {
			rt.DatabaseConfig = &DbConfig{}
		}

		rt.DatabaseConfig.BucketConfig = BucketConfig{
			Server:   &server,
			Bucket:   &spec.BucketName,
			Username: username,
			Password: password,
		}
		rt.DatabaseConfig.Name = "db"
		rt.DatabaseConfig.Sync = syncFnPtr
		rt.DatabaseConfig.EnableXattrs = &useXattrs

		_, err := rt.RestTesterServerContext.AddDatabaseFromConfig(rt.DatabaseConfig)
		if err != nil {
			panic(fmt.Sprintf("Error from AddDatabaseFromConfig: %v", err))
		}
		rt.RestTesterBucket = rt.RestTesterServerContext.Database("db").Bucket

		if !rt.noAdminParty {
			rt.SetAdminParty(true)
		}

	}
	return rt.RestTesterBucket
}

func (rt *RestTester) BucketAllowEmptyPassword() base.Bucket {

	//Create test DB with "AllowEmptyPassword" true
	server := "walrus:"
	bucketName := fmt.Sprintf("sync_gateway_test_%d", gBucketCounter)
	gBucketCounter++

	rt.RestTesterServerContext = NewServerContext(&ServerConfig{
		CORS:           &CORSConfig{},
		Facebook:       &FacebookConfig{},
		AdminInterface: &DefaultAdminInterface,
	})

	_, err := rt.RestTesterServerContext.AddDatabaseFromConfig(&DbConfig{
		BucketConfig: BucketConfig{
			Server: &server,
			Bucket: &bucketName},
		Name:               "db",
		AllowEmptyPassword: true,
	})

	if err != nil {
		panic(fmt.Sprintf("Error from AddDatabaseFromConfig: %v", err))
	}
	rt.RestTesterBucket = rt.RestTesterServerContext.Database("db").Bucket

	return rt.RestTesterBucket
}

func (rt *RestTester) ServerContext() *ServerContext {
	rt.Bucket()
	return rt.RestTesterServerContext
}

// Returns first database found for server context.
func (rt *RestTester) GetDatabase() *db.DatabaseContext {

	for _, database := range rt.ServerContext().AllDatabases() {
		return database
	}
	return nil
}

func (rt *RestTester) WaitForSequence(seq uint64) error {
	database := rt.GetDatabase()
	if database == nil {
		return fmt.Errorf("No database found")
	}
	return database.WaitForSequence(seq)
}

func (rt *RestTester) WaitForPendingChanges() error {
	database := rt.GetDatabase()
	if database == nil {
		return fmt.Errorf("No database found")
	}
	return database.WaitForPendingChanges()
}

func (rt *RestTester) SetAdminParty(partyTime bool) {
	a := rt.ServerContext().Database("db").Authenticator()
	guest, _ := a.GetUser("")
	guest.SetDisabled(!partyTime)
	var chans channels.TimedSet
	if partyTime {
		chans = channels.AtSequence(base.SetOf("*"), 1)
	}
	guest.SetExplicitChannels(chans)
	a.Save(guest)
}

func (rt *RestTester) Close() {
	if rt.RestTesterServerContext != nil {
		rt.RestTesterServerContext.Close()
	}
}

func (rt *RestTester) SendRequest(method, resource string, body string) *TestResponse {
	return rt.Send(request(method, resource, body))
}

func (rt *RestTester) SendRequestWithHeaders(method, resource string, body string, headers map[string]string) *TestResponse {
	req := request(method, resource, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return rt.Send(req)
}

func (rt *RestTester) SendUserRequestWithHeaders(method, resource string, body string, headers map[string]string, username string, password string) *TestResponse {
	req := request(method, resource, body)
	req.SetBasicAuth(username, password)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return rt.Send(req)
}
func (rt *RestTester) Send(request *http.Request) *TestResponse {
	response := &TestResponse{httptest.NewRecorder(), request}
	response.Code = 200 // doesn't seem to be initialized by default; filed Go bug #4188
	CreatePublicHandler(rt.ServerContext()).ServeHTTP(response, request)
	return response
}

type changesResults struct {
	Results []db.ChangeEntry
}

func (rt *RestTester) CreateWaitForChangesRetryWorker(numChangesExpected int, changesUrl, username string) (worker base.RetryWorker) {

	waitForChangesWorker := func() (shouldRetry bool, err error, value interface{}) {

		var changes changesResults

		response := rt.Send(requestByUser("GET", changesUrl, "", username))
		err = json.Unmarshal(response.Body.Bytes(), &changes)
		if err != nil {
			return false, err, nil
		}
		if len(changes.Results) < numChangesExpected {
			// not enough results, retry
			return true, nil, nil
		}
		// If it made it this far, there is no errors and it got enough changes
		return false, nil, changes
	}

	return waitForChangesWorker

}

func (rt *RestTester) WaitForChanges(numChangesExpected int, changesUrl, username string) (changes changesResults, err error) {

	waitForChangesWorker := rt.CreateWaitForChangesRetryWorker(numChangesExpected, changesUrl, username)

	sleeper :=  base.CreateDoublingSleeperFunc(20, 10)

	err, changesVal := base.RetryLoop("Wait for changes", waitForChangesWorker, sleeper)
	if err != nil {
		return changes, err
	}

	if changesVal == nil {
		return changes, fmt.Errorf("Got nil value for changes")
	}

	if changesVal != nil {
		changes = changesVal.(changesResults)
	}

	return changes, nil
}

func (rt *RestTester) SendAdminRequest(method, resource string, body string) *TestResponse {
	input := bytes.NewBufferString(body)
	request, _ := http.NewRequest(method, "http://localhost"+resource, input)
	response := &TestResponse{httptest.NewRecorder(), request}
	response.Code = 200 // doesn't seem to be initialized by default; filed Go bug #4188

	CreateAdminHandler(rt.ServerContext()).ServeHTTP(response, request)
	return response
}


// Wait for a certain number of results to be returned from a view query
// viewUrlPath: is the path to the view, including the db name.  Eg: "/db/_design/foo/_view/bar"
func (rt *RestTester) WaitForNViewResults(numResultsExpected int, viewUrlPath string) (viewResult sgbucket.ViewResult, err error) {

	worker := func() (shouldRetry bool, err error, value interface{}) {
		response := rt.SendAdminRequest("GET", viewUrlPath, ``)
		if response.Code != 200 {
			return false, fmt.Errorf("Got response code: %d from view call.  Expected 200.", response.Code), sgbucket.ViewResult{}
		}
		var result sgbucket.ViewResult
		json.Unmarshal(response.Body.Bytes(), &result)

		if len(result.Rows) >= numResultsExpected {
			// Got enough results, break out of retry loop
			return false, nil, result
		}

		// Not enough results, retry
		return true, nil, sgbucket.ViewResult{}

	}

	description := fmt.Sprintf("Wait for %d view results for query to %v", numResultsExpected, viewUrlPath)
	sleeper := base.CreateDoublingSleeperFunc(20, 10)
	err, returnVal := base.RetryLoop(description, worker, sleeper)

	if err != nil {
		return sgbucket.ViewResult{}, err
	}

	return returnVal.(sgbucket.ViewResult), nil


}

func (rt *RestTester) SendAdminRequestWithHeaders(method, resource string, body string, headers map[string]string) *TestResponse {
	input := bytes.NewBufferString(body)
	request, _ := http.NewRequest(method, "http://localhost"+resource, input)
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	response := &TestResponse{httptest.NewRecorder(), request}
	response.Code = 200 // doesn't seem to be initialized by default; filed Go bug #4188

	CreateAdminHandler(rt.ServerContext()).ServeHTTP(response, request)
	return response
}

type TestResponse struct {
	*httptest.ResponseRecorder
	Req *http.Request
}

func (r TestResponse) DumpBody() {
	log.Printf("%v", string(r.Body.Bytes()))
}

func request(method, resource, body string) *http.Request {
	request, err := http.NewRequest(method, "http://localhost"+resource, bytes.NewBufferString(body))
	request.RequestURI = resource // This doesn't get filled in by NewRequest
	FixQuotedSlashes(request)
	if err != nil {
		panic(fmt.Sprintf("http.NewRequest failed: %v", err))
	}
	return request
}

func requestByUser(method, resource, body, username string) *http.Request {
	r := request(method, resource, body)
	r.SetBasicAuth(username, "letmein")
	return r
}

func assertStatus(t *testing.T, response *TestResponse, expectedStatus int) {
	if response.Code != expectedStatus {
		debug.PrintStack()
		t.Fatalf("Response status %d (expected %d) for %s <%s> : %s",
			response.Code, expectedStatus, response.Req.Method, response.Req.URL, response.Body)
	}
}
