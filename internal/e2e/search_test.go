package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// --- Search helpers ---

func directSearch(t *testing.T, entityName string, modelVersion int, condition string) (int, []map[string]any) {
	t.Helper()
	path := fmt.Sprintf("/api/search/direct/%s/%d", entityName, modelVersion)
	resp := doAuth(t, http.MethodPost, path, condition)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	// Per canonical openapi-entity-search.yml, sync search returns
	// application/x-ndjson — a stream of JSON objects, one per line.
	var results []map[string]any
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode ndjson line %q: %v", line, err)
		}
		results = append(results, entry)
	}
	return resp.StatusCode, results
}

func setupSearchModel(t *testing.T, model string) {
	t.Helper()
	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "search-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]},
				"APPROVED": {}
			}
		}]
	}`)
}

// --- Test 7.7: Search with string operators ---

func TestSearch_StringOperators(t *testing.T) {
	const model = "e2e-search-7"
	setupSearchModel(t, model)

	createEntityE2E(t, model, 1, `{"name":"Alice Johnson","amount":100,"status":"active"}`)
	createEntityE2E(t, model, 1, `{"name":"Bob Smith","amount":50,"status":"active"}`)
	createEntityE2E(t, model, 1, `{"name":"Alice Williams","amount":75,"status":"active"}`)

	// STARTS_WITH "Alice"
	cond := `{"type":"simple","jsonPath":"$.name","operatorType":"STARTS_WITH","value":"Alice"}`
	_, results := directSearch(t, model, 1, cond)
	if len(results) != 2 {
		t.Errorf("expected 2 results starting with Alice, got %d", len(results))
	}

	// CONTAINS "Smith"
	cond = `{"type":"simple","jsonPath":"$.name","operatorType":"CONTAINS","value":"Smith"}`
	_, results = directSearch(t, model, 1, cond)
	if len(results) != 1 {
		t.Errorf("expected 1 result containing Smith, got %d", len(results))
	}
}

// --- Test 7.8: Search with OR group ---

func TestSearch_ORGroup(t *testing.T) {
	const model = "e2e-search-8"
	setupSearchModel(t, model)

	createEntityE2E(t, model, 1, `{"name":"Alice","amount":10,"status":"draft"}`)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":200,"status":"active"}`)
	createEntityE2E(t, model, 1, `{"name":"Carol","amount":50,"status":"active"}`)

	// OR: name == "Alice" OR amount > 100
	cond := `{
		"type": "group",
		"operator": "OR",
		"conditions": [
			{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"Alice"},
			{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}
		]
	}`
	status, results := directSearch(t, model, 1, cond)
	if status != http.StatusOK {
		t.Fatalf("search: expected 200, got %d", status)
	}

	// Alice (name match) + Bob (amount > 100) = 2
	if len(results) != 2 {
		names := make([]string, 0)
		for _, r := range results {
			if d, ok := r["data"].(map[string]any); ok {
				if n, ok := d["name"].(string); ok {
					names = append(names, n)
				}
			}
		}
		t.Errorf("expected 2 results (Alice + Bob), got %d: %v", len(results), strings.Join(names, ", "))
	}
}

// TestSearch_UnknownFieldPath_Returns400_InvalidFieldPath verifies the
// pre-execution field-path validator surfaces a dedicated INVALID_FIELD_PATH
// errorCode (not the generic BAD_REQUEST) when a search condition references
// a JSONPath that is absent from the model's locked schema. Programmatic
// clients branch on this code to distinguish unknown-field errors from
// other 400s (malformed JSON, type mismatch). See PR #162 / issue #77.
func TestSearch_UnknownFieldPath_Returns400_InvalidFieldPath(t *testing.T) {
	const model = "e2e-search-invalid-field-path"
	setupSearchModel(t, model)

	// Seed at least one entity so the model schema is populated with the
	// known fields (name, amount, status). The seeded entity is irrelevant
	// to the assertion — the validator runs before any matching.
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)

	// Reference an unknown JSONPath. The validator must reject before
	// touching the storage layer.
	const badCondition = `{"type":"simple","jsonPath":"$.unknownField","operatorType":"EQUALS","value":"whatever"}`
	path := fmt.Sprintf("/api/search/direct/%s/1", model)
	resp := doAuth(t, http.MethodPost, path, badCondition)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_FIELD_PATH")

	// Body must name the offending path so clients can correct without
	// a support round-trip.
	body := readBody(t, resp)
	if !strings.Contains(body, "$.unknownField") {
		t.Errorf("expected response detail to name the unknown path; body: %s", body)
	}
}

// TestSearch_AsyncSubmit_UnknownFieldPath_Returns400_InvalidFieldPath
// verifies the async submission path applies the same field-path
// validator and surfaces the same dedicated INVALID_FIELD_PATH code.
func TestSearch_AsyncSubmit_UnknownFieldPath_Returns400_InvalidFieldPath(t *testing.T) {
	const model = "e2e-search-invalid-field-path-async"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":42,"status":"active"}`)

	const badCondition = `{"type":"simple","jsonPath":"$.absentField","operatorType":"EQUALS","value":"x"}`
	path := fmt.Sprintf("/api/search/async/%s/1", model)
	resp := doAuth(t, http.MethodPost, path, badCondition)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_FIELD_PATH")
}

// --- Async search lifecycle ---

// submitAsyncSearch submits an async search job and returns the job ID string.
func submitAsyncSearch(t *testing.T, entityName string, modelVersion int, condition string) string {
	t.Helper()
	path := fmt.Sprintf("/api/search/async/%s/%d", entityName, modelVersion)
	resp := doAuth(t, http.MethodPost, path, condition)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submitAsyncSearch %s/%d: expected 200, got %d: %s", entityName, modelVersion, resp.StatusCode, body)
	}
	// Response is a bare UUID string (possibly quoted JSON string).
	jobID := strings.Trim(strings.TrimSpace(body), `"`)
	if jobID == "" {
		t.Fatalf("submitAsyncSearch: got empty job ID")
	}
	return jobID
}

// waitForAsyncSearch polls getAsyncSearchStatus until the job is no longer
// RUNNING or the timeout elapses. Returns the final status string.
func waitForAsyncSearch(t *testing.T, jobID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		path := fmt.Sprintf("/api/search/async/%s/status", jobID)
		resp := doAuth(t, http.MethodGet, path, "")
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("getAsyncSearchStatus: expected 200, got %d: %s", resp.StatusCode, body)
		}
		var status map[string]any
		if err := json.Unmarshal([]byte(body), &status); err != nil {
			t.Fatalf("getAsyncSearchStatus: parse response: %v; body: %s", err, body)
		}
		s, _ := status["searchJobStatus"].(string)
		if s != "RUNNING" {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waitForAsyncSearch: job %s did not complete within %v", jobID, timeout)
	return ""
}

// TestAsyncSearch_SubmitAndGetResults exercises submitAsyncSearchJob,
// getAsyncSearchStatus, and getAsyncSearchResults in a single lifecycle.
func TestAsyncSearch_SubmitAndGetResults(t *testing.T) {
	const model = "e2e-async-search-lifecycle"
	setupSearchModel(t, model)

	createEntityE2E(t, model, 1, `{"name":"Alice","amount":100,"status":"active"}`)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":50,"status":"active"}`)

	// Submit: match-all group condition (empty body is not valid JSON; use
	// an always-true group with no sub-conditions instead).
	jobID := submitAsyncSearch(t, model, 1, `{"type":"group","operator":"AND","conditions":[]}`)

	// Poll status until done.
	finalStatus := waitForAsyncSearch(t, jobID, 10*time.Second)
	if finalStatus != "SUCCESSFUL" {
		t.Fatalf("expected SUCCESSFUL, got %q", finalStatus)
	}

	// Retrieve first page of results.
	path := fmt.Sprintf("/api/search/async/%s", jobID)
	resp := doAuth(t, http.MethodGet, path+"?pageSize=10&pageNumber=0", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getAsyncSearchResults: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var page map[string]any
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("getAsyncSearchResults: parse response: %v; body: %s", err, body)
	}
	content, ok := page["content"].([]any)
	if !ok {
		t.Fatalf("getAsyncSearchResults: expected 'content' array in response; body: %s", body)
	}
	if len(content) != 2 {
		t.Errorf("expected 2 results, got %d", len(content))
	}
	pageInfo, ok := page["page"].(map[string]any)
	if !ok {
		t.Fatalf("getAsyncSearchResults: expected 'page' object in response; body: %s", body)
	}
	if pageInfo["totalElements"] == nil {
		t.Errorf("expected 'page.totalElements' in response; page: %v", pageInfo)
	}
}

// TestAsyncSearch_GetStatus_NotFound verifies that requesting status for a
// non-existent job returns 404.
func TestAsyncSearch_GetStatus_NotFound(t *testing.T) {
	const fakeJobID = "00000000-0000-0000-0000-000000000001"
	path := fmt.Sprintf("/api/search/async/%s/status", fakeJobID)
	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent job, got %d: %s", resp.StatusCode, body)
	}
}

// TestAsyncSearch_GetResults_NotFound verifies that requesting results for a
// non-existent job returns 404.
func TestAsyncSearch_GetResults_NotFound(t *testing.T) {
	const fakeJobID = "00000000-0000-0000-0000-000000000002"
	path := fmt.Sprintf("/api/search/async/%s?pageSize=10&pageNumber=0", fakeJobID)
	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent job, got %d: %s", resp.StatusCode, body)
	}
}

// TestAsyncSearch_Cancel_AlreadyCompleted verifies that cancelling an already-
// completed job returns 400 with a structured body describing the current status.
func TestAsyncSearch_Cancel_AlreadyCompleted(t *testing.T) {
	const model = "e2e-async-search-cancel"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Charlie","amount":77,"status":"active"}`)

	// Submit and wait for completion.
	jobID := submitAsyncSearch(t, model, 1, `{"type":"group","operator":"AND","conditions":[]}`)
	finalStatus := waitForAsyncSearch(t, jobID, 10*time.Second)
	if finalStatus != "SUCCESSFUL" {
		t.Fatalf("expected job to complete SUCCESSFULLY before cancel test, got %q", finalStatus)
	}

	// Attempt to cancel the completed job — must get 400.
	path := fmt.Sprintf("/api/search/async/%s/cancel", jobID)
	resp := doAuth(t, http.MethodPut, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("cancelAsyncSearch on completed job: expected 400, got %d: %s", resp.StatusCode, body)
	}
	// Response must mention the current status.
	if !strings.Contains(body, "SUCCESSFUL") {
		t.Errorf("expected 400 body to contain current status 'SUCCESSFUL'; body: %s", body)
	}
}

// TestAsyncSearch_Cancel_NotFound verifies that cancelling a non-existent job
// returns 404.
func TestAsyncSearch_Cancel_NotFound(t *testing.T) {
	const fakeJobID = "00000000-0000-0000-0000-000000000003"
	path := fmt.Sprintf("/api/search/async/%s/cancel", fakeJobID)
	resp := doAuth(t, http.MethodPut, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cancelAsyncSearch non-existent: expected 404, got %d: %s", resp.StatusCode, body)
	}
}

// ---------------------------------------------------------------------------
// Sort tests
// ---------------------------------------------------------------------------

// matchAllCond is a condition that matches every entity.
const matchAllCond = `{"type":"group","operator":"AND","conditions":[]}`

// sortedSearchPath appends repeatable ?sort= params to a base URL path.
func sortedSearchPath(base string, sortKeys []string) string {
	if len(sortKeys) == 0 {
		return base
	}
	p := url.Values{}
	for _, k := range sortKeys {
		p.Add("sort", k)
	}
	return base + "?" + p.Encode()
}

// directSearchSorted performs a sync search with sort keys and returns the
// decoded NDJSON results. Returns the status code and nil results on non-200.
// The NDJSON parsing mirrors directSearch.
func directSearchSorted(t *testing.T, entityName string, modelVersion int, condition string, sortKeys []string) (int, []map[string]any) {
	t.Helper()
	base := fmt.Sprintf("/api/search/direct/%s/%d", entityName, modelVersion)
	path := sortedSearchPath(base, sortKeys)
	resp := doAuth(t, http.MethodPost, path, condition)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var results []map[string]any
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode ndjson line %q: %v", line, err)
		}
		results = append(results, entry)
	}
	return resp.StatusCode, results
}

// setupSortModelWithArrayField imports a model whose sample schema includes a
// string-array field "tags", locks it, and attaches a trivial workflow.
// Use for tests that need to exercise the "cannot sort by array field" path.
func setupSortModelWithArrayField(t *testing.T, model string) {
	t.Helper()
	importPath := fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model)
	resp := doAuth(t, http.MethodPost, importPath, `{"name":"Sample","tags":["a","b"]}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import model %s: expected 200, got %d: %s", model, resp.StatusCode, body)
	}
	lockModelE2E(t, model, 1)
	status, body := importWorkflowE2E(t, model, 1, `{
		"importMode": "REPLACE",
		"workflows": [{"version": "1.1", "name": "sort-wf", "initialState": "NONE", "active": true,
			"states": {"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
			           "CREATED": {}}}]
	}`)
	if status != http.StatusOK {
		t.Fatalf("workflow import for %s: expected 200, got %d: %s", model, status, body)
	}
}

// extractDataString returns a string field from a search-result data object.
func extractDataString(t *testing.T, result map[string]any, field string) string {
	t.Helper()
	data, ok := result["data"].(map[string]any)
	if !ok {
		t.Fatalf("result has no data object: %v", result)
	}
	v, _ := data[field].(string)
	return v
}

// extractDataStrings returns a slice of string field values from each result, in order.
func extractDataStrings(t *testing.T, results []map[string]any, field string) []string {
	t.Helper()
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = extractDataString(t, r, field)
	}
	return out
}

// resultMetaID returns the entity ID from a search result's meta object.
func resultMetaID(t *testing.T, result map[string]any) string {
	t.Helper()
	meta, ok := result["meta"].(map[string]any)
	if !ok {
		t.Fatalf("result has no meta object: %v", result)
	}
	id, _ := meta["id"].(string)
	return id
}

// resultMetaState returns the workflow state from a search result's meta object.
func resultMetaState(t *testing.T, result map[string]any) string {
	t.Helper()
	meta, ok := result["meta"].(map[string]any)
	if !ok {
		t.Fatalf("result has no meta object: %v", result)
	}
	state, _ := meta["state"].(string)
	return state
}

// --- Happy path: sort by data field asc ---

// TestSearchSort_DataField_Asc verifies that sort=name:asc returns entities in
// ascending lexicographic order on the "name" data field.
func TestSearchSort_DataField_Asc(t *testing.T) {
	const model = "e2e-search-sort-data-asc"
	setupSearchModel(t, model)

	createEntityE2E(t, model, 1, `{"name":"Charlie","amount":30,"status":"new"}`)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":10,"status":"new"}`)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":20,"status":"new"}`)

	status, results := directSearchSorted(t, model, 1, matchAllCond, []string{"name:asc"})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	names := extractDataStrings(t, results, "name")
	want := []string{"Alice", "Bob", "Charlie"}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("result[%d].name = %q, want %q", i, names[i], n)
		}
	}
}

// TestSearchSort_DataField_Desc verifies that sort=name:desc returns entities in
// descending lexicographic order on the "name" data field.
func TestSearchSort_DataField_Desc(t *testing.T) {
	const model = "e2e-search-sort-data-desc"
	setupSearchModel(t, model)

	createEntityE2E(t, model, 1, `{"name":"Charlie","amount":30,"status":"new"}`)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":10,"status":"new"}`)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":20,"status":"new"}`)

	status, results := directSearchSorted(t, model, 1, matchAllCond, []string{"name:desc"})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	names := extractDataStrings(t, results, "name")
	want := []string{"Charlie", "Bob", "Alice"}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("result[%d].name = %q, want %q", i, names[i], n)
		}
	}
}

// TestSearchSort_MetaCreationDate verifies that sort=@creationDate:asc returns
// entities in the order they were created.  A 10 ms pause between creates
// guarantees distinct millisecond-precision timestamps in Postgres.
func TestSearchSort_MetaCreationDate(t *testing.T) {
	const model = "e2e-search-sort-meta-date"
	setupSearchModel(t, model)

	id1 := createEntityE2E(t, model, 1, `{"name":"First","amount":1,"status":"new"}`)
	time.Sleep(50 * time.Millisecond)
	id2 := createEntityE2E(t, model, 1, `{"name":"Second","amount":2,"status":"new"}`)
	time.Sleep(50 * time.Millisecond)
	id3 := createEntityE2E(t, model, 1, `{"name":"Third","amount":3,"status":"new"}`)

	status, results := directSearchSorted(t, model, 1, matchAllCond, []string{"@creationDate:asc"})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	wantIDs := []string{id1, id2, id3}
	for i, wantID := range wantIDs {
		gotID := resultMetaID(t, results[i])
		if gotID != wantID {
			t.Errorf("result[%d] id = %q, want %q (creation order)", i, gotID, wantID)
		}
	}
}

// TestSearchSort_MetaState verifies that sort=@state:asc places APPROVED
// entities before CREATED entities (lexicographic order).
func TestSearchSort_MetaState(t *testing.T) {
	const model = "e2e-search-sort-meta-state"
	setupSearchModel(t, model)

	// Both entities start in CREATED state after the automatic init transition.
	aliceID := createEntityE2E(t, model, 1, `{"name":"Alice","amount":1,"status":"new"}`)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":2,"status":"new"}`)

	// Manually promote Alice to APPROVED.
	resp := doAuth(t, http.MethodPut,
		fmt.Sprintf("/api/entity/JSON/%s/approve", aliceID),
		`{"name":"Alice","amount":1,"status":"approved"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve transition: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// APPROVED < CREATED lexicographically, so Alice should be first.
	status, results := directSearchSorted(t, model, 1, matchAllCond, []string{"@state:asc"})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if s := resultMetaState(t, results[0]); s != "APPROVED" {
		t.Errorf("result[0] state = %q, want APPROVED", s)
	}
	if s := resultMetaState(t, results[1]); s != "CREATED" {
		t.Errorf("result[1] state = %q, want CREATED", s)
	}
}

// TestSearchSort_MultiKey verifies that a two-key sort (@state:asc, name:asc)
// groups by state first and then by name within each state group.
func TestSearchSort_MultiKey(t *testing.T) {
	const model = "e2e-search-sort-multi"
	setupSearchModel(t, model)

	// Charlie will be promoted to APPROVED; Alice and Bob stay CREATED.
	charlieID := createEntityE2E(t, model, 1, `{"name":"Charlie","amount":3,"status":"new"}`)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":1,"status":"new"}`)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":2,"status":"new"}`)

	resp := doAuth(t, http.MethodPut,
		fmt.Sprintf("/api/entity/JSON/%s/approve", charlieID),
		`{"name":"Charlie","amount":3,"status":"approved"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve Charlie: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Expected: Charlie (APPROVED) | Alice (CREATED, name asc) | Bob (CREATED, name asc)
	status, results := directSearchSorted(t, model, 1, matchAllCond, []string{"@state:asc", "name:asc"})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	wantNames := []string{"Charlie", "Alice", "Bob"}
	wantStates := []string{"APPROVED", "CREATED", "CREATED"}
	for i := range wantNames {
		name := extractDataString(t, results[i], "name")
		state := resultMetaState(t, results[i])
		if name != wantNames[i] {
			t.Errorf("result[%d] name = %q, want %q", i, name, wantNames[i])
		}
		if state != wantStates[i] {
			t.Errorf("result[%d] state = %q, want %q", i, state, wantStates[i])
		}
	}
}

// --- Error cases: sync direct search with invalid sort ---

// TestSearchSort_Sync_InvalidSort_Returns400 is a table-driven test covering
// every documented error sub-case for the ?sort= param on the sync search
// endpoint.  Every case must return HTTP 400 with errorCode INVALID_FIELD_PATH.
func TestSearchSort_Sync_InvalidSort_Returns400(t *testing.T) {
	// Standard model (name/amount/status scalar fields) for most error cases.
	const model = "e2e-search-sort-err-sync"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":1,"status":"new"}`)

	// Separate model with a string-array field "tags" for the array-field case.
	// The schema stores "tags" as $.tags[*] (IsArray=true); attempting to sort
	// by "tags" (lookup key $.tags) fails as "unknown sort field" — same
	// INVALID_FIELD_PATH code, confirming array fields are unsortable.
	const arrayModel = "e2e-search-sort-err-array"
	setupSortModelWithArrayField(t, arrayModel)
	createEntityE2E(t, arrayModel, 1, `{"name":"X","tags":["a","b"]}`)

	// tooManyKeys constructs 17 syntactically valid single-letter sort tokens,
	// exceeding the default cap of 16.
	tooManyKeys := make([]string, 17)
	for i := range tooManyKeys {
		tooManyKeys[i] = string(rune('a' + i))
	}

	tests := []struct {
		name       string
		entityName string
		sortKeys   []string
	}{
		// Semantic errors (schema-resolution layer returns INVALID_FIELD_PATH).
		{"unknown_data_field", model, []string{"unknownField"}},
		{"array_field", arrayModel, []string{"tags"}},
		{"unknown_meta_field", model, []string{"@unknownMeta"}},
		// Parse-time errors (handler returns INVALID_FIELD_PATH before schema lookup).
		{"malformed_direction_name_up", model, []string{"name:up"}},
		{"bare_at_sign", model, []string{"@"}},
		{"empty_sort_value", model, []string{""}},
		// Deduplication and cap errors (ParseSortParam).
		{"duplicate_key", model, []string{"name:asc", "name:desc"}},
		{"too_many_keys", model, tooManyKeys},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			base := fmt.Sprintf("/api/search/direct/%s/1", tc.entityName)
			path := sortedSearchPath(base, tc.sortKeys)
			resp := doAuth(t, http.MethodPost, path, matchAllCond)
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				body := readBody(t, resp)
				t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, body)
			}
			commontest.ExpectErrorCode(t, resp, "INVALID_FIELD_PATH")
		})
	}
}

// --- Async search: sort integration ---

// TestSearchSort_Async_Submit_Happy verifies that submitting an async search
// with a valid sort key returns HTTP 200 and a non-empty job ID — i.e. the
// sort param is accepted synchronously and the job is created.
func TestSearchSort_Async_Submit_Happy(t *testing.T) {
	const model = "e2e-search-sort-async-happy"
	setupSearchModel(t, model)

	createEntityE2E(t, model, 1, `{"name":"Alice","amount":1,"status":"new"}`)
	createEntityE2E(t, model, 1, `{"name":"Bob","amount":2,"status":"new"}`)

	base := fmt.Sprintf("/api/search/async/%s/1", model)
	path := sortedSearchPath(base, []string{"name:asc"})
	resp := doAuth(t, http.MethodPost, path, matchAllCond)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", resp.StatusCode, body)
	}
	jobID := strings.Trim(strings.TrimSpace(body), `"`)
	if jobID == "" {
		t.Fatalf("expected non-empty job ID; body: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Pushdown vs in-memory fallback agreement (isolated, single-backend)
// ---------------------------------------------------------------------------

// setupSortModelWithAmountAndArray imports a model whose sample schema includes
// a numeric field "amount", a string field "name", and a string-array field
// "tags". It locks the model and attaches a trivial two-state workflow.
//
// The "tags" array field makes "$.tags[*]" appear in the schema's FieldsMap,
// which is necessary for the fallback-path test below to pass path validation
// while still failing ConditionToFilter's stripDollarDot check.
func setupSortModelWithAmountAndArray(t *testing.T, model string) {
	t.Helper()
	importPath := fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model)
	resp := doAuth(t, http.MethodPost, importPath, `{"name":"Sample","amount":0,"tags":["a"]}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import model %s: expected 200, got %d: %s", model, resp.StatusCode, body)
	}
	lockModelE2E(t, model, 1)
	status, body := importWorkflowE2E(t, model, 1, `{
		"importMode": "REPLACE",
		"workflows": [{"version": "1.1", "name": "sort-wf", "initialState": "NONE", "active": true,
			"states": {"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
			           "CREATED": {}}}]
	}`)
	if status != http.StatusOK {
		t.Fatalf("workflow import for %s: expected 200, got %d: %s", model, status, body)
	}
}

// TestSearchSort_PushdownFallbackAgree verifies that the SQL pushdown path
// (spi.Searcher) and the in-memory fallback path (GetAll + sortEntities)
// produce identical entity-id sequences for the same sort key on the same
// entity set.
//
// Forcing the fallback: the Postgres plugin implements spi.Searcher, so the
// only HTTP-expressible way to bypass it is an untranslatable condition —
// specifically a SimpleCondition whose JSONPath contains a character that
// ConditionToFilter's stripDollarDot rejects (e.g. '[').  The condition
// "$.tags[*] NOT_NULL" satisfies all three requirements:
//
//	(1) passes path validation ($.tags[*] is in the schema FieldsMap for
//	    array-field models);
//	(2) fails ConditionToFilter (stripDollarDot rejects '[');
//	(3) match.Match handles it correctly: convertJSONPath("$.tags[*]") →
//	    gjson path "tags.#" (array count), which is NOT_NULL for any entity
//	    that carries the tags field.
//
// This is an isolated single-backend e2e test (Postgres only). It is not in
// the shared cross-backend parity suite because it asserts Postgres-specific
// pushdown-vs-fallback behaviour; the parity suite is for backend-agnostic
// behaviour that must hold consistently across all backends.
func TestSearchSort_PushdownFallbackAgree(t *testing.T) {
	const model = "e2e-search-sort-pushdown-fallback"

	// Model has name (string), amount (numeric, sortable), tags (array —
	// provides $.tags[*] in the FieldsMap for the fallback condition below).
	setupSortModelWithAmountAndArray(t, model)

	// Seed four entities with amounts chosen so that numeric order differs from
	// lexical order: numeric asc = 9,10,20,100; lexical asc = "10","100","20","9".
	// Any path that sorts by string comparison rather than numeric value will
	// produce a different sequence and fail the wantIDs assertion below.
	// All carry tags so that NOT_NULL on $.tags[*] returns true for each.
	id1 := createEntityE2E(t, model, 1, `{"name":"D","amount":100,"tags":["x"]}`)
	id2 := createEntityE2E(t, model, 1, `{"name":"B","amount":9,"tags":["x"]}`)
	id3 := createEntityE2E(t, model, 1, `{"name":"C","amount":20,"tags":["x"]}`)
	id4 := createEntityE2E(t, model, 1, `{"name":"A","amount":10,"tags":["x"]}`)

	sortKeys := []string{"amount:asc"}

	// --- Pushdown path ---
	// matchAllCond is an empty AND group, which ConditionToFilter translates
	// to a tautology filter. The Postgres Searcher executes
	// "ORDER BY (data->>'amount')::float ASC" directly in SQL.
	status, pushdownResults := directSearchSorted(t, model, 1, matchAllCond, sortKeys)
	if status != http.StatusOK {
		t.Fatalf("pushdown: expected 200, got %d", status)
	}
	if len(pushdownResults) != 4 {
		t.Fatalf("pushdown: expected 4 results, got %d", len(pushdownResults))
	}

	// --- Fallback path ---
	// "$.tags[*] NOT_NULL" passes path validation ($.tags[*] is in the
	// FieldsMap) but fails ConditionToFilter (stripDollarDot rejects '['),
	// forcing the GetAll + in-memory sortEntities path.
	const fallbackCond = `{"type":"simple","jsonPath":"$.tags[*]","operatorType":"NOT_NULL","value":null}`
	status, fallbackResults := directSearchSorted(t, model, 1, fallbackCond, sortKeys)
	if status != http.StatusOK {
		t.Fatalf("fallback: expected 200, got %d", status)
	}
	if len(fallbackResults) != 4 {
		t.Fatalf("fallback: expected 4 results, got %d", len(fallbackResults))
	}

	// Extract entity IDs in search-result order from both paths.
	pushdownIDs := make([]string, len(pushdownResults))
	for i, r := range pushdownResults {
		pushdownIDs[i] = resultMetaID(t, r)
	}
	fallbackIDs := make([]string, len(fallbackResults))
	for i, r := range fallbackResults {
		fallbackIDs[i] = resultMetaID(t, r)
	}

	// Both paths must agree on every position.
	for i := range pushdownIDs {
		if pushdownIDs[i] != fallbackIDs[i] {
			t.Errorf("result[%d] mismatch: pushdown=%s fallback=%s\n pushdownIDs: %v\n fallbackIDs:  %v",
				i, pushdownIDs[i], fallbackIDs[i], pushdownIDs, fallbackIDs)
		}
	}

	// Additionally verify that both paths return the expected numeric amount-ascending
	// order, so we catch "both wrong but consistently so" divergences.
	// amounts: id2=9, id4=10, id3=20, id1=100
	// Lexical order would be: id4("10"), id1("100"), id3("20"), id2("9") — different.
	wantIDs := []string{id2, id4, id3, id1}
	for i, wantID := range wantIDs {
		if pushdownIDs[i] != wantID {
			t.Errorf("pushdown result[%d]: got id %s, want %s (expected numeric amount-asc order: 9,10,20,100)",
				i, pushdownIDs[i], wantID)
		}
	}
}

// TestSearchSort_Async_InvalidSort_Returns400 is the regression guard for the
// async synchronous-resolution fix: an invalid sort key must return 400
// INVALID_FIELD_PATH before a job is ever created, not a job ID followed by a
// FAILED status poll.
func TestSearchSort_Async_InvalidSort_Returns400(t *testing.T) {
	const model = "e2e-search-sort-err-async"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Alice","amount":1,"status":"new"}`)

	tests := []struct {
		name     string
		sortKeys []string
	}{
		// Schema-level rejections: resolveSortKeys runs synchronously in
		// SubmitAsync before any job record is written.
		{"unknown_data_field", []string{"unknownField"}},
		{"unknown_meta_field", []string{"@unknownMeta"}},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			base := fmt.Sprintf("/api/search/async/%s/1", model)
			path := sortedSearchPath(base, tc.sortKeys)
			resp := doAuth(t, http.MethodPost, path, matchAllCond)
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				body := readBody(t, resp)
				t.Fatalf("expected synchronous 400, got %d; body: %s", resp.StatusCode, body)
			}
			commontest.ExpectErrorCode(t, resp, "INVALID_FIELD_PATH")
		})
	}
}
