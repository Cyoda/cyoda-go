package search_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	return newTestServerWithConfig(t, cfg)
}

func newTestServerWithConfig(t *testing.T, cfg app.Config) *httptest.Server {
	t.Helper()
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func importAndLockModel(t *testing.T, base, entityName string, version int, sampleData string) {
	t.Helper()
	url := base + "/model/import/JSON/SAMPLE_DATA/" + entityName + "/" + strconv.Itoa(version)
	resp, err := http.Post(url, "application/json", strings.NewReader(sampleData))
	if err != nil {
		t.Fatalf("import request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	lockURL := base + "/model/" + entityName + "/" + strconv.Itoa(version) + "/lock"
	req, _ := http.NewRequest(http.MethodPut, lockURL, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("lock request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func createEntity(t *testing.T, base, entityName string, version int, data string) string {
	t.Helper()
	url := base + "/entity/JSON/" + entityName + "/" + strconv.Itoa(version)
	resp, err := http.Post(url, "application/json", strings.NewReader(data))
	if err != nil {
		t.Fatalf("create entity request failed: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var results []map[string]any
	if err := json.Unmarshal(body, &results); err != nil {
		t.Fatalf("failed to parse create response: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least 1 element in create response")
	}
	ids, ok := results[0]["entityIds"].([]any)
	if !ok || len(ids) == 0 {
		t.Fatal("expected entityIds in create response")
	}
	return ids[0].(string)
}

func doDirectSearch(t *testing.T, base, entityName string, version int, condition string, extraParams ...string) *http.Response {
	t.Helper()
	url := base + "/search/direct/" + entityName + "/" + strconv.Itoa(version)
	if len(extraParams) > 0 {
		url += "?" + strings.Join(extraParams, "&")
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(condition))
	if err != nil {
		t.Fatalf("direct search request failed: %v", err)
	}
	return resp
}

func doSubmitAsync(t *testing.T, base, entityName string, version int, condition string) *http.Response {
	t.Helper()
	url := base + "/search/async/" + entityName + "/" + strconv.Itoa(version)
	resp, err := http.Post(url, "application/json", strings.NewReader(condition))
	if err != nil {
		t.Fatalf("submit async search request failed: %v", err)
	}
	return resp
}

func doGetAsyncStatus(t *testing.T, base, jobID string) *http.Response {
	t.Helper()
	url := base + "/search/async/" + jobID + "/status"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get async status request failed: %v", err)
	}
	return resp
}

func doGetAsyncResults(t *testing.T, base, jobID string, extraParams ...string) *http.Response {
	t.Helper()
	url := base + "/search/async/" + jobID
	if len(extraParams) > 0 {
		url += "?" + strings.Join(extraParams, "&")
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get async results request failed: %v", err)
	}
	return resp
}

func doCancelAsync(t *testing.T, base, jobID string) *http.Response {
	t.Helper()
	url := base + "/search/async/" + jobID + "/cancel"
	req, _ := http.NewRequest(http.MethodPut, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cancel async search request failed: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	return b
}

func expectStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected status %d, got %d; body: %s", want, resp.StatusCode, string(body))
	}
}

func parseArray(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var arr []map[string]any
	if err := json.Unmarshal(body, &arr); err != nil {
		t.Fatalf("failed to parse array: %v; body: %s", err, string(body))
	}
	return arr
}

// parseNDJSON parses an application/x-ndjson body — one JSON object per line.
func parseNDJSON(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var results []map[string]any
	for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode ndjson line %q: %v", line, err)
		}
		results = append(results, entry)
	}
	return results
}

func parseObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("failed to parse object: %v; body: %s", err, string(body))
	}
	return obj
}

// ---------------------------------------------------------------------------
// Direct search tests
// ---------------------------------------------------------------------------

func TestHandlerDirectSearchSimple(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)

	createEntity(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)
	createEntity(t, srv.URL, "Person", 1, `{"name":"Bob","age":25}`)
	createEntity(t, srv.URL, "Person", 1, `{"name":"Charlie","age":35}`)

	cond := `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`
	resp := doDirectSearch(t, srv.URL, "Person", 1, cond)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	results := parseNDJSON(t, body)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	envelope := results[0]
	if envelope["type"] != "ENTITY" {
		t.Errorf("expected type=ENTITY, got %v", envelope["type"])
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatal("expected data map in envelope")
	}
	if data["name"] != "Alice" {
		t.Errorf("expected name=Alice, got %v", data["name"])
	}
	meta, ok := envelope["meta"].(map[string]any)
	if !ok {
		t.Fatal("expected meta map in envelope")
	}
	if meta["id"] == nil || meta["id"] == "" {
		t.Error("expected non-empty meta.id")
	}
	if meta["state"] == nil || meta["state"] == "" {
		t.Error("expected non-empty meta.state")
	}
	mk, ok := meta["modelKey"].(map[string]any)
	if !ok {
		t.Fatal("expected modelKey map in meta")
	}
	if mk["name"] != "Person" {
		t.Errorf("expected modelKey.name=Person, got %v", mk["name"])
	}
}

func TestHandlerDirectSearchGroup(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)

	createEntity(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)
	createEntity(t, srv.URL, "Person", 1, `{"name":"Alice","age":20}`)
	createEntity(t, srv.URL, "Person", 1, `{"name":"Bob","age":35}`)

	cond := `{
		"type": "group",
		"operator": "AND",
		"conditions": [
			{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"},
			{"type":"simple","jsonPath":"$.age","operatorType":"GREATER_OR_EQUAL","value":25}
		]
	}`
	resp := doDirectSearch(t, srv.URL, "Person", 1, cond)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	results := parseNDJSON(t, body)

	if len(results) != 1 {
		t.Fatalf("expected 1 result (Alice age 30), got %d", len(results))
	}
	data := results[0]["data"].(map[string]any)
	if data["name"] != "Alice" {
		t.Errorf("expected Alice, got %v", data["name"])
	}
	age, _ := data["age"].(float64)
	if age != 30 {
		t.Errorf("expected age 30, got %v", age)
	}
}

func TestHandlerDirectSearchLifecycle(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)

	createEntity(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)
	createEntity(t, srv.URL, "Person", 1, `{"name":"Bob","age":25}`)

	cond := `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"}`
	resp := doDirectSearch(t, srv.URL, "Person", 1, cond)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	results := parseNDJSON(t, body)

	if len(results) != 2 {
		t.Fatalf("expected 2 results for state=CREATED, got %d", len(results))
	}
}

func TestHandlerDirectSearchEmpty(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)

	createEntity(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)

	cond := `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Nobody"}`
	resp := doDirectSearch(t, srv.URL, "Person", 1, cond)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	results := parseNDJSON(t, body)

	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

func TestHandlerDirectSearchPagination(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)

	for i := 0; i < 5; i++ {
		createEntity(t, srv.URL, "Person", 1, fmt.Sprintf(`{"name":"Person%d","age":%d}`, i, 20+i))
	}

	// Search all with lifecycle match, limit=2
	cond := `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"}`
	resp := doDirectSearch(t, srv.URL, "Person", 1, cond, "limit=2")
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	results := parseNDJSON(t, body)

	if len(results) != 2 {
		t.Fatalf("expected 2 results with limit=2, got %d", len(results))
	}
}

// TestHandlerDirectSearch_TrackingReadQueryParam_Accepted is an end-to-end
// sanity check that the sync search endpoint's router accepts the
// trackingRead query parameter (both true and false) over the real HTTP
// stack without error. The DTO-to-SearchOptions mapping itself is covered
// at a finer grain by TestHandlerDirectSearch_TrackingReadTrue_ReachesSearchOptions
// and TestHandlerDirectSearch_TrackingReadAbsent_DefaultsFalse in
// handler_tracking_read_test.go.
func TestHandlerDirectSearch_TrackingReadQueryParam_Accepted(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)
	createEntity(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)

	cond := `{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"}`

	resp := doDirectSearch(t, srv.URL, "Person", 1, cond, "trackingRead=true")
	expectStatus(t, resp, http.StatusOK)
	results := parseNDJSON(t, readBody(t, resp))
	if len(results) != 1 {
		t.Fatalf("trackingRead=true: expected 1 result, got %d", len(results))
	}

	resp = doDirectSearch(t, srv.URL, "Person", 1, cond, "trackingRead=false")
	expectStatus(t, resp, http.StatusOK)
	results = parseNDJSON(t, readBody(t, resp))
	if len(results) != 1 {
		t.Fatalf("trackingRead=false: expected 1 result, got %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// Async search tests
// ---------------------------------------------------------------------------

func TestHandlerAsyncLifecycle(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)

	createEntity(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)
	createEntity(t, srv.URL, "Person", 1, `{"name":"Bob","age":25}`)

	cond := `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"}`

	// Submit async job — response is a bare job ID string
	resp := doSubmitAsync(t, srv.URL, "Person", 1, cond)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var jobID string
	if err := json.Unmarshal(body, &jobID); err != nil {
		t.Fatalf("expected bare string job ID, got: %s", string(body))
	}
	if jobID == "" {
		t.Fatal("expected non-empty job ID")
	}

	// Poll status until SUCCESSFUL (with timeout)
	var status map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp = doGetAsyncStatus(t, srv.URL, jobID)
		expectStatus(t, resp, http.StatusOK)
		body = readBody(t, resp)
		status = parseObject(t, body)
		if status["searchJobStatus"] == "SUCCESSFUL" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status["searchJobStatus"] != "SUCCESSFUL" {
		t.Fatalf("expected SUCCESSFUL status, got %v", status["searchJobStatus"])
	}
	entCount, _ := status["entitiesCount"].(float64)
	if entCount != 2 {
		t.Errorf("expected entitiesCount=2, got %v", entCount)
	}

	// Get results — response is {content: [...], page: {...}}
	resp = doGetAsyncResults(t, srv.URL, jobID)
	expectStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)
	envelope := parseObject(t, body)

	content, ok := envelope["content"].([]any)
	if !ok {
		t.Fatalf("expected content array in response, got %T", envelope["content"])
	}
	if len(content) != 2 {
		t.Fatalf("expected 2 async results, got %d", len(content))
	}
	for _, item := range content {
		env, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("expected map in content, got %T", item)
		}
		if env["type"] != "ENTITY" {
			t.Errorf("expected type=ENTITY, got %v", env["type"])
		}
	}

	pageInfo, ok := envelope["page"].(map[string]any)
	if !ok {
		t.Fatalf("expected page object in response, got %T", envelope["page"])
	}
	if totalElements, _ := pageInfo["totalElements"].(float64); totalElements != 2 {
		t.Errorf("expected totalElements=2, got %v", totalElements)
	}
	if totalPages, _ := pageInfo["totalPages"].(float64); totalPages != 1 {
		t.Errorf("expected totalPages=1, got %v", totalPages)
	}
}

func TestHandlerAsyncCancel(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)

	// Create entities
	createEntity(t, srv.URL, "Person", 1, `{"name":"Alice","age":30}`)

	cond := `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"}`

	// Submit async job — response is a bare job ID string
	resp := doSubmitAsync(t, srv.URL, "Person", 1, cond)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	var jobID string
	if err := json.Unmarshal(body, &jobID); err != nil {
		t.Fatalf("expected bare string job ID, got: %s", string(body))
	}

	// Wait for completion so we can test cancel on a completed job
	var status map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp = doGetAsyncStatus(t, srv.URL, jobID)
		expectStatus(t, resp, http.StatusOK)
		body = readBody(t, resp)
		status = parseObject(t, body)
		if status["searchJobStatus"] != "RUNNING" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Cancel the completed job — should return 400
	resp = doCancelAsync(t, srv.URL, jobID)
	expectStatus(t, resp, http.StatusBadRequest)
	body = readBody(t, resp)
	cancelResult := parseObject(t, body)

	// Should match Cloud error shape
	if cancelResult["title"] != "Bad Request" {
		t.Errorf("expected title='Bad Request', got %v", cancelResult["title"])
	}
	if cancelResult["status"] == nil {
		t.Error("expected status field in cancel error response")
	}
	props, ok := cancelResult["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties object in cancel error response")
	}
	if props["currentStatus"] != "SUCCESSFUL" {
		t.Errorf("expected currentStatus=SUCCESSFUL, got %v", props["currentStatus"])
	}
}

// ---------------------------------------------------------------------------
// Sort param — grammar-level handler tests
// ---------------------------------------------------------------------------

// TestSearchEntities_SortMalformed400 verifies that a malformed sort token
// (invalid direction) is rejected with 400 INVALID_FIELD_PATH at the handler
// level, and that a well-formed token passes through to the service.
func TestSearchEntities_SortMalformed400(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "Widget", 1, `{"name":"x"}`)

	cond := `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"}`

	// Malformed direction "up" is not a valid direction token → 400 INVALID_FIELD_PATH
	resp := doDirectSearch(t, srv.URL, "Widget", 1, cond, "sort=name:up")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, string(body))
	}
	obj := parseObject(t, body)
	props, _ := obj["properties"].(map[string]any)
	if props == nil || props["errorCode"] != "INVALID_FIELD_PATH" {
		t.Errorf("expected properties.errorCode=INVALID_FIELD_PATH, got %v; body: %s", props, string(body))
	}

	// Well-formed token "name:asc" reaches the service without a parse error.
	resp2 := doDirectSearch(t, srv.URL, "Widget", 1, cond, "sort=name:asc")
	body2 := readBody(t, resp2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for well-formed sort, got %d; body: %s", resp2.StatusCode, string(body2))
	}
}

// ---------------------------------------------------------------------------
// Tenant isolation
// ---------------------------------------------------------------------------

func TestSearchTenantIsolation(t *testing.T) {
	cfgA := app.DefaultConfig()
	cfgA.ContextPath = ""
	cfgA.IAM.MockTenantID = "search-tenant-A"
	cfgA.IAM.MockTenantName = "Tenant A"
	srvA := newTestServerWithConfig(t, cfgA)

	cfgB := app.DefaultConfig()
	cfgB.ContextPath = ""
	cfgB.IAM.MockTenantID = "search-tenant-B"
	cfgB.IAM.MockTenantName = "Tenant B"
	srvB := newTestServerWithConfig(t, cfgB)

	// Set up model and entities on tenant A
	importAndLockModel(t, srvA.URL, "IsoTest", 1, `{"name":"Alice","age":30}`)
	createEntity(t, srvA.URL, "IsoTest", 1, `{"name":"TenantA","age":1}`)

	// Set up same model on tenant B (different store)
	importAndLockModel(t, srvB.URL, "IsoTest", 1, `{"name":"Alice","age":30}`)

	// Search from tenant B should find nothing
	cond := `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"}`
	resp := doDirectSearch(t, srvB.URL, "IsoTest", 1, cond)
	expectStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	results := parseNDJSON(t, body)

	if len(results) != 0 {
		t.Fatalf("expected 0 results for tenant B, got %d", len(results))
	}

	// Search from tenant A should find 1
	resp = doDirectSearch(t, srvA.URL, "IsoTest", 1, cond)
	expectStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)
	results = parseNDJSON(t, body)

	if len(results) != 1 {
		t.Fatalf("expected 1 result for tenant A, got %d", len(results))
	}
}
