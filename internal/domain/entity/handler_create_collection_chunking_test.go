package entity_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Tests for the documented `transactionWindow` chunking contract on
// CreateCollection (POST /api/entity/{format}). Issue #227.
//
// Contract (docs/cyoda/openapi.yml line 195-201, EntityCreateCollectionRequest
// schema): the collection is committed in transactional batches of at most
// `transactionWindow` items. Default 100, max 1000. Response is an array of
// EntityTransactionResponse with one element per committed chunk, in commit
// order. On per-chunk failure: chunks committed before the failure stay
// durable; the failure surfaces as an error element carrying chunkIndex.

// TestCreateCollection_HonorsTransactionWindow_Default100 — a 150-item
// batch with no transactionWindow query param uses the default (100) and
// produces 2 chunks: 100 + 50.
func TestCreateCollection_HonorsTransactionWindow_Default100(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "CreateChunkDefault", 1, `{"name":"x","v":0}`)

	const total = 150
	parts := make([]string, total)
	for i := 0; i < total; i++ {
		parts[i] = fmt.Sprintf(`{"model":{"name":"CreateChunkDefault","version":1},"payload":"{\"name\":\"e%d\",\"v\":%d}"}`, i, i)
	}
	body := "[" + strings.Join(parts, ",") + "]"

	resp := doCreateCollection(t, srv.URL, "JSON", body)
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
		t.Fatalf("expected 2 chunk results (100+50), got %d; body has %d bytes", len(arr), len(rbody))
	}
	if eids, _ := arr[0]["entityIds"].([]any); len(eids) != 100 {
		t.Errorf("chunk 0: got %d entityIds, want 100", len(eids))
	}
	if eids, _ := arr[1]["entityIds"].([]any); len(eids) != 50 {
		t.Errorf("chunk 1: got %d entityIds, want 50", len(eids))
	}
}

// TestCreateCollection_HonorsTransactionWindow_ClientSupplied — a client-
// supplied transactionWindow=4 against a 10-item batch produces 3 chunks
// (4 + 4 + 2).
func TestCreateCollection_HonorsTransactionWindow_ClientSupplied(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "CreateChunkClient", 1, `{"name":"x","v":0}`)

	const total = 10
	const window = 4

	parts := make([]string, total)
	for i := 0; i < total; i++ {
		parts[i] = fmt.Sprintf(`{"model":{"name":"CreateChunkClient","version":1},"payload":"{\"name\":\"e%d\",\"v\":%d}"}`, i, i)
	}
	body := "[" + strings.Join(parts, ",") + "]"

	url := srv.URL + fmt.Sprintf("/entity/JSON?transactionWindow=%d", window)
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
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
		t.Fatalf("expected 3 chunks (4+4+2), got %d", len(arr))
	}
	wantSizes := []int{4, 4, 2}
	for i, want := range wantSizes {
		eids, _ := arr[i]["entityIds"].([]any)
		if len(eids) != want {
			t.Errorf("chunk %d: got %d entityIds, want %d", i, len(eids), want)
		}
	}
}

// TestCreateCollection_BatchChunksAtWindowBoundary — a batch of exactly
// 2*window items splits into exactly two chunks.
func TestCreateCollection_BatchChunksAtWindowBoundary(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "CreateChunkBoundary", 1, `{"name":"x"}`)

	const window = 5
	const total = 2 * window

	parts := make([]string, total)
	for i := 0; i < total; i++ {
		parts[i] = fmt.Sprintf(`{"model":{"name":"CreateChunkBoundary","version":1},"payload":"{\"name\":\"b%d\"}"}`, i)
	}
	body := "[" + strings.Join(parts, ",") + "]"

	url := srv.URL + fmt.Sprintf("/entity/JSON?transactionWindow=%d", window)
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
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
		t.Fatalf("expected 2 chunks, got %d", len(arr))
	}
	// Distinct transactionIds per chunk.
	tx0, _ := arr[0]["transactionId"].(string)
	tx1, _ := arr[1]["transactionId"].(string)
	if tx0 == "" || tx1 == "" || tx0 == tx1 {
		t.Errorf("expected two distinct non-empty transactionIds, got %q vs %q", tx0, tx1)
	}
}

// TestCreateCollection_TransactionWindowValidation — transactionWindow
// out of the (0, 1000] range is rejected with 400 before any work starts.
func TestCreateCollection_TransactionWindowValidation(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "CreateChunkVal", 1, `{"name":"x"}`)

	body := `[{"model":{"name":"CreateChunkVal","version":1},"payload":"{\"name\":\"e\"}"}]`

	for _, tc := range []struct {
		name   string
		window string
	}{
		{"zero", "0"},
		{"negative", "-1"},
		{"over-max", "1001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := srv.URL + "/entity/JSON?transactionWindow=" + tc.window
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

// TestCreateCollection_ChunkFailureLeavesEarlierChunksDurable — when a
// later chunk fails its validation phase (here: the model is locked for
// chunks 0 and 1 but the request includes a chunk-2 item referencing an
// undeclared model), earlier chunks remain durable. Response is HTTP 200
// with an error element marking the failed chunk's index.
func TestCreateCollection_ChunkFailureLeavesEarlierChunksDurable(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "CreateChunkFailGood", 1, `{"name":"x"}`)
	// Note: NO model named "CreateChunkFailMissing" is registered. An item
	// referencing it will fail per-chunk validation with MODEL_NOT_FOUND.

	const window = 2

	// 4 good items + 1 bad in chunk 2: chunks [0..1], [2..3], [4 (bad)].
	parts := []string{
		`{"model":{"name":"CreateChunkFailGood","version":1},"payload":"{\"name\":\"a\"}"}`,
		`{"model":{"name":"CreateChunkFailGood","version":1},"payload":"{\"name\":\"b\"}"}`,
		`{"model":{"name":"CreateChunkFailGood","version":1},"payload":"{\"name\":\"c\"}"}`,
		`{"model":{"name":"CreateChunkFailGood","version":1},"payload":"{\"name\":\"d\"}"}`,
		`{"model":{"name":"CreateChunkFailMissing","version":1},"payload":"{\"name\":\"never\"}"}`,
	}
	body := "[" + strings.Join(parts, ",") + "]"

	url := srv.URL + fmt.Sprintf("/entity/JSON?transactionWindow=%d", window)
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
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
	// Successful chunks 0, 1.
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
	// Failed chunk 2.
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
