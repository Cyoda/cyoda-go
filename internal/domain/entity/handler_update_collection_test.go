package entity_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// Regression test for issue #92. PUT /api/entity/{format} (collection
// update) was a stub returning 501 with a wrong errorCode. The endpoint
// is in the route table and advertised — AI clients hit it and failed.

func doUpdateCollection(t *testing.T, base, format, body string) *http.Response {
	t.Helper()
	url := base + "/entity/" + format
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build update collection request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("update collection request failed: %v", err)
	}
	return resp
}

// TestUpdateCollection_HappyPath — bulk-update two entities; verify
// response shape matches the documented [{transactionId, entityIds}]
// EntityTransactionResponse array.
func TestUpdateCollection_HappyPath(t *testing.T) {
	srv := newTestServer(t)

	importAndLockModel(t, srv.URL, "UpdBatch", 1, `{"name":"x","v":0}`)

	// Seed two entities.
	id1 := doCreateAndGetID(t, srv.URL, "UpdBatch", 1, `{"name":"Alice","v":1}`)
	id2 := doCreateAndGetID(t, srv.URL, "UpdBatch", 1, `{"name":"Bob","v":2}`)

	body := fmt.Sprintf(`[
		{"id":"%s","payload":"{\"name\":\"Alice2\",\"v\":11}"},
		{"id":"%s","payload":"{\"name\":\"Bob2\",\"v\":22}"}
	]`, id1, id2)

	resp := doUpdateCollection(t, srv.URL, "JSON", body)
	defer resp.Body.Close()
	respBody := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, respBody)
	}

	var arr []map[string]any
	if err := json.Unmarshal(respBody, &arr); err != nil {
		t.Fatalf("parse response: %v; body: %s", err, respBody)
	}
	if len(arr) != 1 {
		t.Fatalf("expected single-element EntityTransactionResponse array, got %d", len(arr))
	}
	txID, _ := arr[0]["transactionId"].(string)
	if txID == "" {
		t.Errorf("missing transactionId; body: %s", respBody)
	}
	ids, _ := arr[0]["entityIds"].([]any)
	if len(ids) != 2 {
		t.Fatalf("expected 2 entityIds, got %d: %s", len(ids), respBody)
	}

	// Fetch each back and verify the update landed.
	for _, want := range []struct {
		id   string
		name string
	}{{id1, "Alice2"}, {id2, "Bob2"}} {
		getResp := doGetEntity(t, srv.URL, want.id)
		expectStatus(t, getResp, http.StatusOK)
		gb := readBody(t, getResp)
		if !strings.Contains(string(gb), want.name) {
			t.Errorf("entity %s did not receive update %q; body: %s", want.id, want.name, gb)
		}
	}
}

// TestUpdateCollection_AnyMissingRollsBackAll — per docs "If any entity
// in the collection is not found, the entire operation fails and no
// entities are updated." A valid entity + one bogus UUID must leave the
// valid entity unchanged.
func TestUpdateCollection_AnyMissingRollsBackAll(t *testing.T) {
	srv := newTestServer(t)

	importAndLockModel(t, srv.URL, "UpdBatchRB", 1, `{"name":"x","v":0}`)

	id1 := doCreateAndGetID(t, srv.URL, "UpdBatchRB", 1, `{"name":"Alice","v":1}`)
	bogus := "00000000-0000-0000-0000-000000000000"

	body := fmt.Sprintf(`[
		{"id":"%s","payload":"{\"name\":\"AliceShouldNotLand\",\"v\":999}"},
		{"id":"%s","payload":"{\"name\":\"never\",\"v\":0}"}
	]`, id1, bogus)

	resp := doUpdateCollection(t, srv.URL, "JSON", body)
	defer resp.Body.Close()
	rbody := readBody(t, resp)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (missing item); body: %s", resp.StatusCode, rbody)
	}

	// Valid entity must be unchanged.
	getResp := doGetEntity(t, srv.URL, id1)
	expectStatus(t, getResp, http.StatusOK)
	gb := readBody(t, getResp)
	if strings.Contains(string(gb), "AliceShouldNotLand") {
		t.Errorf("rollback violation: entity was modified despite a missing sibling; body: %s", gb)
	}
}

// TestUpdateCollection_PayloadMustBeString — per docs contract: payload
// must be a JSON-encoded string, not an object. An object payload is
// rejected with 400 BAD_REQUEST (matches CreateCollection's contract).
func TestUpdateCollection_PayloadMustBeString(t *testing.T) {
	srv := newTestServer(t)

	importAndLockModel(t, srv.URL, "UpdBatchStr", 1, `{"name":"x"}`)
	id1 := doCreateAndGetID(t, srv.URL, "UpdBatchStr", 1, `{"name":"Alice"}`)

	body := fmt.Sprintf(`[
		{"id":"%s","payload":{"name":"bogus"}}
	]`, id1)

	resp := doUpdateCollection(t, srv.URL, "JSON", body)
	defer resp.Body.Close()
	rbody := readBody(t, resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for object payload; body: %s", resp.StatusCode, rbody)
	}
	// Pin the operator-facing error text so a future refactor doesn't
	// silently regress the hint that payload must be a JSON-encoded string.
	if !strings.Contains(string(rbody), "payload must be") {
		t.Errorf("body does not explain the payload-must-be-string contract: %s", rbody)
	}
}

// TestUpdateCollection_EmptyArray — `UpdateEntityCollection` rejects an
// empty items list with 400 before opening a transaction. Pins that
// contract so a future caller can rely on it.
func TestUpdateCollection_EmptyArray(t *testing.T) {
	srv := newTestServer(t)

	resp := doUpdateCollection(t, srv.URL, "JSON", `[]`)
	defer resp.Body.Close()
	rbody := readBody(t, resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty array; body: %s", resp.StatusCode, rbody)
	}
}

// TestUpdateCollection_TransactionWindowValidation — transactionWindow must
// be in (0, 1000]. 0, negatives, and values > 1000 are rejected before any
// work starts. Bounds the per-transaction lock pressure regardless of how
// the batch will be split.
func TestUpdateCollection_TransactionWindowValidation(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "UpdBatchWinVal", 1, `{"name":"x"}`)

	// IDs do not need to exist — validation fires before any lookup.
	body := `[{"id":"00000000-0000-0000-0000-000000000001","payload":"{\"name\":\"x\"}"}]`

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
			req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
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

// TestUpdateCollection_BatchChunksAtWindowBoundary — a batch of 2*window
// items is split into exactly two transactional chunks per the documented
// contract: "Collections exceeding `transactionWindow` size are
// automatically split into multiple transactional batches". Both chunks
// must commit. Response is the EntityTransactionResponse array with one
// element per chunk in commit order. Issue #227.
func TestUpdateCollection_BatchChunksAtWindowBoundary(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "UpdBatchChunk", 1, `{"name":"x","v":0}`)

	// Use a small client-supplied window so the test stays fast.
	const window = 3
	const total = 2 * window

	// Seed `total` entities — one per item we'll subsequently update.
	ids := make([]string, total)
	for i := 0; i < total; i++ {
		ids[i] = doCreateAndGetID(t, srv.URL, "UpdBatchChunk", 1, fmt.Sprintf(`{"name":"orig-%d","v":%d}`, i, i))
	}

	parts := make([]string, total)
	for i := 0; i < total; i++ {
		parts[i] = fmt.Sprintf(`{"id":"%s","payload":"{\"name\":\"upd-%d\",\"v\":%d}"}`, ids[i], i, i*10)
	}
	body := "[" + strings.Join(parts, ",") + "]"

	url := srv.URL + "/entity/JSON?transactionWindow=" + fmt.Sprintf("%d", window)
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
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
		t.Fatalf("parse response: %v; body: %s", err, rbody)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 chunk results, got %d; body: %s", len(arr), rbody)
	}

	// Each chunk's result carries `window` entityIds and a distinct transactionId.
	tx1, _ := arr[0]["transactionId"].(string)
	tx2, _ := arr[1]["transactionId"].(string)
	if tx1 == "" || tx2 == "" || tx1 == tx2 {
		t.Errorf("expected two distinct non-empty transactionIds, got %q vs %q", tx1, tx2)
	}
	for ci, chunk := range arr {
		eids, _ := chunk["entityIds"].([]any)
		if len(eids) != window {
			t.Errorf("chunk %d: got %d entityIds, want %d", ci, len(eids), window)
		}
	}

	// All entities reflect the update.
	for i, id := range ids {
		getResp := doGetEntity(t, srv.URL, id)
		gb := readBody(t, getResp)
		want := fmt.Sprintf(`upd-%d`, i)
		if !strings.Contains(string(gb), want) {
			t.Errorf("entity %d: body does not contain %q: %s", i, want, gb)
		}
	}
}

// TestUpdateCollection_ChunkFailureLeavesEarlierChunksDurable — when a
// later chunk fails (here: chunk 2 contains a missing-entity id), earlier
// chunks remain committed. The response is HTTP 200 carrying the durable
// chunks plus an error element marking the failed chunk's index. Issue #227.
func TestUpdateCollection_ChunkFailureLeavesEarlierChunksDurable(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "UpdChunkFail", 1, `{"name":"x","v":0}`)

	const window = 2

	// Chunk 0 + chunk 1: 2 well-formed entities each, all updates land.
	// Chunk 2: 1 well-formed + 1 bogus id → entire chunk rolls back, but
	// chunks 0 and 1 stay durable.
	good := make([]string, 5)
	for i := 0; i < 5; i++ {
		good[i] = doCreateAndGetID(t, srv.URL, "UpdChunkFail", 1, fmt.Sprintf(`{"name":"orig-%d","v":%d}`, i, i))
	}
	bogus := "00000000-0000-0000-0000-000000000099"

	parts := make([]string, 0, 6)
	for i := 0; i < 5; i++ {
		parts = append(parts, fmt.Sprintf(`{"id":"%s","payload":"{\"name\":\"upd-%d\",\"v\":%d}"}`, good[i], i, i*10))
	}
	parts = append(parts, fmt.Sprintf(`{"id":"%s","payload":"{\"name\":\"never\",\"v\":0}"}`, bogus))
	body := "[" + strings.Join(parts, ",") + "]"

	url := srv.URL + "/entity/JSON?transactionWindow=" + fmt.Sprintf("%d", window)
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
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
	// First two are successful chunks.
	for ci := 0; ci < 2; ci++ {
		if _, hasErr := arr[ci]["error"]; hasErr {
			t.Errorf("chunk %d: did not expect error element; got %v", ci, arr[ci])
		}
		if tx, _ := arr[ci]["transactionId"].(string); tx == "" {
			t.Errorf("chunk %d: missing transactionId; got %v", ci, arr[ci])
		}
	}
	// Third element carries the failure with chunkIndex=2.
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

	// Entities in chunks 0 and 1 (indices 0..3) reflect the update.
	for i := 0; i < 4; i++ {
		gb := readBody(t, doGetEntity(t, srv.URL, good[i]))
		want := fmt.Sprintf(`upd-%d`, i)
		if !strings.Contains(string(gb), want) {
			t.Errorf("durable chunk: entity %d missing update %q; body: %s", i, want, gb)
		}
	}
	// Entity in chunk 2 (index 4) must be unchanged — its chunk rolled back.
	gb := readBody(t, doGetEntity(t, srv.URL, good[4]))
	if !strings.Contains(string(gb), `orig-4`) {
		t.Errorf("rolled-back chunk: entity 4 should still carry orig-4 payload; body: %s", gb)
	}
	if strings.Contains(string(gb), `upd-4`) {
		t.Errorf("rolled-back chunk: entity 4 leaked an update payload; body: %s", gb)
	}
}

// --- Issue #228: per-item ifMatch + per-chunk failed[] ---

// doGetEntityTxID issues GET /entity/{id} and returns meta.transactionId.
// Used by ifMatch tests to seed a fresh precondition token.
func doGetEntityTxID(t *testing.T, base, entityID string) string {
	t.Helper()
	resp := doGetEntity(t, base, entityID)
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusOK)
	rb := readBody(t, resp)
	var env map[string]any
	if err := json.Unmarshal(rb, &env); err != nil {
		t.Fatalf("parse get-entity: %v; body: %s", err, rb)
	}
	meta, _ := env["meta"].(map[string]any)
	tx, _ := meta["transactionId"].(string)
	if tx == "" {
		t.Fatalf("entity %s: missing meta.transactionId; body: %s", entityID, rb)
	}
	return tx
}

// TestUpdateCollection_IfMatch_PerItemFailedShape — pin the per-chunk
// response shape for a chunk where one item's ifMatch is stale: the response
// must carry transactionId, entityIds (only successful items), and a
// failed[] array with the conflict's entityId, code=ENTITY_MODIFIED, and a
// per-chunk-relative itemIndex.
func TestUpdateCollection_IfMatch_PerItemFailedShape(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "UpdIfMatchShape", 1, `{"name":"x","v":0}`)

	id1 := doCreateAndGetID(t, srv.URL, "UpdIfMatchShape", 1, `{"name":"A","v":1}`)
	id2 := doCreateAndGetID(t, srv.URL, "UpdIfMatchShape", 1, `{"name":"B","v":2}`)
	tx2 := doGetEntityTxID(t, srv.URL, id2)
	const stale = "00000000-0000-0000-0000-000000000000"

	body := fmt.Sprintf(`[
		{"id":"%s","payload":"{\"name\":\"A_STALE\",\"v\":99}","ifMatch":"%s"},
		{"id":"%s","payload":"{\"name\":\"B2\",\"v\":22}","ifMatch":"%s"}
	]`, id1, stale, id2, tx2)

	resp := doUpdateCollection(t, srv.URL, "JSON", body)
	defer resp.Body.Close()
	rb := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, rb)
	}

	var arr []map[string]any
	if err := json.Unmarshal([]byte(rb), &arr); err != nil {
		t.Fatalf("parse: %v; body: %s", err, rb)
	}
	if len(arr) != 1 {
		t.Fatalf("expected 1 chunk, got %d; body: %s", len(arr), rb)
	}
	chunk := arr[0]
	if tx, _ := chunk["transactionId"].(string); tx == "" {
		t.Errorf("missing transactionId; body: %s", rb)
	}
	ids, _ := chunk["entityIds"].([]any)
	if len(ids) != 1 || ids[0].(string) != id2 {
		t.Errorf("entityIds: expected [%s], got %v", id2, ids)
	}
	failed, _ := chunk["failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("failed: expected 1 entry, got %d; body: %s", len(failed), rb)
	}
	f := failed[0].(map[string]any)
	if got, _ := f["entityId"].(string); got != id1 {
		t.Errorf("failed[0].entityId: got %q, want %q", got, id1)
	}
	errObj, _ := f["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "ENTITY_MODIFIED" {
		t.Errorf("failed[0].error.code: got %q, want ENTITY_MODIFIED", code)
	}
	if msg, _ := errObj["message"].(string); msg == "" {
		t.Errorf("failed[0].error.message: empty; body: %s", rb)
	}
	if idx, _ := errObj["itemIndex"].(float64); int(idx) != 0 {
		t.Errorf("failed[0].error.itemIndex: got %v, want 0", errObj["itemIndex"])
	}
}

// TestUpdateCollection_IfMatch_NoFailedFieldOnSuccess — pin the absence of
// `failed` in the response when no per-item conflicts occurred. JSON
// `omitempty` semantics: the field must not appear in the marshaled body.
func TestUpdateCollection_IfMatch_NoFailedFieldOnSuccess(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "UpdIfMatchNoFailed", 1, `{"name":"x","v":0}`)

	id1 := doCreateAndGetID(t, srv.URL, "UpdIfMatchNoFailed", 1, `{"name":"A","v":1}`)
	tx1 := doGetEntityTxID(t, srv.URL, id1)

	body := fmt.Sprintf(`[
		{"id":"%s","payload":"{\"name\":\"A2\",\"v\":11}","ifMatch":"%s"}
	]`, id1, tx1)

	resp := doUpdateCollection(t, srv.URL, "JSON", body)
	defer resp.Body.Close()
	rb := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, rb)
	}
	// String-level check: `failed` substring must not appear in a fully
	// successful chunk's wire body. The marshaled key is exactly "failed".
	if strings.Contains(string(rb), `"failed"`) {
		t.Errorf("successful chunk leaked `failed` field; body: %s", rb)
	}
}

// TestUpdateCollection_IfMatch_AllStaleZeroWriteCommit — pin that a chunk in
// which every item's ifMatch is stale still emits a transactionId (the
// underlying tx commits as a zero-write read-only tx). entityIds is empty,
// failed[] holds every item, no chunk-wide error.
func TestUpdateCollection_IfMatch_AllStaleZeroWriteCommit(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "UpdIfMatchAllStale", 1, `{"name":"x","v":0}`)

	id1 := doCreateAndGetID(t, srv.URL, "UpdIfMatchAllStale", 1, `{"name":"A","v":1}`)
	id2 := doCreateAndGetID(t, srv.URL, "UpdIfMatchAllStale", 1, `{"name":"B","v":2}`)
	const stale = "00000000-0000-0000-0000-000000000000"

	body := fmt.Sprintf(`[
		{"id":"%s","payload":"{\"name\":\"A_STALE\",\"v\":99}","ifMatch":"%s"},
		{"id":"%s","payload":"{\"name\":\"B_STALE\",\"v\":99}","ifMatch":"%s"}
	]`, id1, stale, id2, stale)

	resp := doUpdateCollection(t, srv.URL, "JSON", body)
	defer resp.Body.Close()
	rb := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, rb)
	}

	var arr []map[string]any
	if err := json.Unmarshal([]byte(rb), &arr); err != nil {
		t.Fatalf("parse: %v; body: %s", err, rb)
	}
	if len(arr) != 1 {
		t.Fatalf("expected 1 chunk, got %d; body: %s", len(arr), rb)
	}
	if tx, _ := arr[0]["transactionId"].(string); tx == "" {
		t.Errorf("expected non-empty transactionId on zero-write commit; body: %s", rb)
	}
	if _, has := arr[0]["error"]; has {
		t.Errorf("did not expect chunk-wide `error`; body: %s", rb)
	}
	ids, _ := arr[0]["entityIds"].([]any)
	if len(ids) != 0 {
		t.Errorf("expected empty entityIds, got %d", len(ids))
	}
	failed, _ := arr[0]["failed"].([]any)
	if len(failed) != 2 {
		t.Errorf("expected 2 failed entries, got %d; body: %s", len(failed), rb)
	}
}

// doCreateAndGetID is a small helper used by the UpdateCollection tests:
// create one entity in a 1-element batch and return its UUID.
func doCreateAndGetID(t *testing.T, base, entityName string, version int, payload string) string {
	t.Helper()
	body := `[{"model":{"name":"` + entityName + `","version":` + fmt.Sprintf("%d", version) + `},"payload":` + strconv.Quote(payload) + `}]`
	resp := doCreateCollection(t, base, "JSON", body)
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusOK)
	rb := readBody(t, resp)
	var arr []map[string]any
	if err := json.Unmarshal(rb, &arr); err != nil {
		t.Fatalf("parse create resp: %v; body: %s", err, rb)
	}
	ids, _ := arr[0]["entityIds"].([]any)
	if len(ids) == 0 {
		t.Fatalf("no entity ids in: %s", rb)
	}
	id, _ := ids[0].(string)
	return id
}
