package entity_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Tests for the documented `transactionWindow` chunking contract on the
// single-create endpoint POST /api/entity/{format}/{entityName}/{modelVersion}
// when the request body is a JSON array. Issue #227 (pass 3).
//
// Contract: when the body is a JSON array, the same chunking semantics that
// apply to POST /api/entity/{format} (CreateCollection) must apply here —
// chunks of at most `transactionWindow` items committed in commit order with
// one EntityTransactionResponse element per chunk; chunks committed before any
// later failure stay durable; first-chunk-fail keeps the 4xx envelope.
//
// Single-object body (non-array) is unaffected: it still goes through
// CreateEntity, returns a single one-element response array.

// doCreateEntityWithWindow wraps doCreateEntity to attach a transactionWindow
// query param. The single-create endpoint accepts the same query params as
// CreateCollection per the OpenAPI spec.
func doCreateEntityWithWindow(t *testing.T, base, format, entityName string, version, window int, body string) *http.Response {
	t.Helper()
	url := fmt.Sprintf("%s/entity/%s/%s/%d?transactionWindow=%d", base, format, entityName, version, window)
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create entity request failed: %v", err)
	}
	return resp
}

// TestCreate_ArrayBody_HonorsTransactionWindow_Default100 — POST 200 items
// with no transactionWindow query param uses default (100); expect 2 chunks.
func TestCreate_ArrayBody_HonorsTransactionWindow_Default100(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "ArrCreateDefault", 1, `{"name":"x","v":0}`)

	const total = 200
	parts := make([]string, total)
	for i := 0; i < total; i++ {
		parts[i] = fmt.Sprintf(`{"name":"e%d","v":%d}`, i, i)
	}
	body := "[" + strings.Join(parts, ",") + "]"

	resp := doCreateEntity(t, srv.URL, "JSON", "ArrCreateDefault", 1, body)
	defer resp.Body.Close()
	rbody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, rbody)
	}

	var arr []map[string]any
	if err := json.Unmarshal(rbody, &arr); err != nil {
		t.Fatalf("parse: %v; body: %s", err, rbody)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 chunk results (100+100), got %d", len(arr))
	}
	for i, want := range []int{100, 100} {
		eids, _ := arr[i]["entityIds"].([]any)
		if len(eids) != want {
			t.Errorf("chunk %d: got %d entityIds, want %d", i, len(eids), want)
		}
	}
}

// TestCreate_ArrayBody_HonorsTransactionWindow_ClientSupplied — POST 7 items
// with ?transactionWindow=3 produces 3 chunks (3+3+1).
func TestCreate_ArrayBody_HonorsTransactionWindow_ClientSupplied(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "ArrCreateClient", 1, `{"name":"x","v":0}`)

	const total = 7
	const window = 3

	parts := make([]string, total)
	for i := 0; i < total; i++ {
		parts[i] = fmt.Sprintf(`{"name":"e%d","v":%d}`, i, i)
	}
	body := "[" + strings.Join(parts, ",") + "]"

	resp := doCreateEntityWithWindow(t, srv.URL, "JSON", "ArrCreateClient", 1, window, body)
	defer resp.Body.Close()
	rbody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, rbody)
	}

	var arr []map[string]any
	if err := json.Unmarshal(rbody, &arr); err != nil {
		t.Fatalf("parse: %v; body: %s", err, rbody)
	}
	if len(arr) != 3 {
		t.Fatalf("expected 3 chunks (3+3+1), got %d", len(arr))
	}
	wantSizes := []int{3, 3, 1}
	for i, want := range wantSizes {
		eids, _ := arr[i]["entityIds"].([]any)
		if len(eids) != want {
			t.Errorf("chunk %d: got %d entityIds, want %d", i, len(eids), want)
		}
	}
}

// TestCreate_ArrayBody_BatchChunksAtWindowBoundary — POST exactly window items
// produces exactly 1 chunk, behaving as the single-transaction case.
func TestCreate_ArrayBody_BatchChunksAtWindowBoundary(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "ArrCreateBoundary", 1, `{"name":"x"}`)

	const window = 5

	parts := make([]string, window)
	for i := 0; i < window; i++ {
		parts[i] = fmt.Sprintf(`{"name":"b%d"}`, i)
	}
	body := "[" + strings.Join(parts, ",") + "]"

	resp := doCreateEntityWithWindow(t, srv.URL, "JSON", "ArrCreateBoundary", 1, window, body)
	defer resp.Body.Close()
	rbody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, rbody)
	}

	var arr []map[string]any
	if err := json.Unmarshal(rbody, &arr); err != nil {
		t.Fatalf("parse: %v; body: %s", err, rbody)
	}
	if len(arr) != 1 {
		t.Fatalf("expected exactly 1 chunk, got %d", len(arr))
	}
	if eids, _ := arr[0]["entityIds"].([]any); len(eids) != window {
		t.Errorf("got %d entityIds, want %d", len(eids), window)
	}
}

// TestCreate_ArrayBody_TransactionWindowValidation — out-of-range
// transactionWindow values are rejected with 400 before any work starts,
// mirroring TestCreateCollection_TransactionWindowValidation.
func TestCreate_ArrayBody_TransactionWindowValidation(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "ArrCreateVal", 1, `{"name":"x"}`)

	body := `[{"name":"e"}]`

	for _, tc := range []struct {
		name   string
		window string
	}{
		{"zero", "0"},
		{"negative", "-1"},
		{"over-max", "1001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := fmt.Sprintf("%s/entity/JSON/ArrCreateVal/1?transactionWindow=%s", srv.URL, tc.window)
			resp, err := http.Post(url, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			rbody := readBody(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("window=%s: status = %d, want 400; body: %s", tc.window, resp.StatusCode, rbody)
			}
			if !strings.Contains(string(rbody), "transactionWindow") {
				t.Errorf("body does not reference transactionWindow: %s", rbody)
			}
		})
	}
}

// TestCreate_ArrayBody_ChunkFailureLeavesEarlierChunksDurable — when one
// item in a later chunk fails schema validation, earlier chunks remain
// durable and the failure surfaces as an error element with chunkIndex.
// Mirrors TestCreateCollection_ChunkFailureLeavesEarlierChunksDurable.
func TestCreate_ArrayBody_ChunkFailureLeavesEarlierChunksDurable(t *testing.T) {
	srv := newTestServer(t)
	// Model expects name:string. We will inject an item with name:123 in chunk 2.
	importAndLockModel(t, srv.URL, "ArrCreateChunkFail", 1, `{"name":"x"}`)

	const window = 2

	// 4 good items + 1 schema-violating item — chunks [0..1], [2..3], [4 (bad)].
	parts := []string{
		`{"name":"a"}`,
		`{"name":"b"}`,
		`{"name":"c"}`,
		`{"name":"d"}`,
		`{"name":123}`, // wrong type — INCOMPATIBLE_TYPE / BAD_REQUEST
	}
	body := "[" + strings.Join(parts, ",") + "]"

	resp := doCreateEntityWithWindow(t, srv.URL, "JSON", "ArrCreateChunkFail", 1, window, body)
	defer resp.Body.Close()
	rbody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with partial-success body; body: %s", resp.StatusCode, rbody)
	}

	var arr []map[string]any
	if err := json.Unmarshal(rbody, &arr); err != nil {
		t.Fatalf("parse: %v; body: %s", err, rbody)
	}
	if len(arr) != 3 {
		t.Fatalf("expected 3 chunk-result elements (2 success + 1 error), got %d; body: %s", len(arr), rbody)
	}
	totalCommitted := 0
	for ci := 0; ci < 2; ci++ {
		if _, has := arr[ci]["error"]; has {
			t.Errorf("chunk %d: did not expect error element; got %v", ci, arr[ci])
		}
		if tx, _ := arr[ci]["transactionId"].(string); tx == "" {
			t.Errorf("chunk %d: missing transactionId; got %v", ci, arr[ci])
		}
		eids, _ := arr[ci]["entityIds"].([]any)
		totalCommitted += len(eids)
	}
	if totalCommitted != 4 {
		t.Errorf("expected 4 entity IDs across chunks 0+1, got %d", totalCommitted)
	}
	errObj, ok := arr[2]["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object on element 2; got %v", arr[2])
	}
	if code, _ := errObj["code"].(string); code == "" {
		t.Errorf("error.code missing on failed chunk: %v", errObj)
	}
	if idx, _ := errObj["chunkIndex"].(float64); int(idx) != 2 {
		t.Errorf("error.chunkIndex: got %v, want 2", errObj["chunkIndex"])
	}
}

// TestCreate_SingleObjectBody_Unaffected — regression guard. A single-object
// (non-array) body still returns a one-element array result with the entity's
// transactionId and a single entityId. transactionWindow query param is
// irrelevant on this path and must not affect behaviour.
func TestCreate_SingleObjectBody_Unaffected(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "ArrCreateSingle", 1, `{"name":"Alice"}`)

	resp := doCreateEntity(t, srv.URL, "JSON", "ArrCreateSingle", 1, `{"name":"Bob"}`)
	defer resp.Body.Close()
	rbody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, rbody)
	}

	var arr []map[string]any
	if err := json.Unmarshal(rbody, &arr); err != nil {
		t.Fatalf("parse: %v; body: %s", err, rbody)
	}
	if len(arr) != 1 {
		t.Fatalf("expected 1-element response array, got %d", len(arr))
	}
	if tx, _ := arr[0]["transactionId"].(string); tx == "" {
		t.Errorf("missing transactionId; got %v", arr[0])
	}
	eids, _ := arr[0]["entityIds"].([]any)
	if len(eids) != 1 {
		t.Errorf("expected 1 entityId, got %d", len(eids))
	}
}
