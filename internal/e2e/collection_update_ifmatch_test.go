package e2e_test

// Tests for issue #228 — UpdateCollection (PUT /api/entity/{format}) must
// honor an optional per-item `ifMatch` field, providing the same cross-request
// optimistic-concurrency precondition the single-item PUT endpoints already
// support via the If-Match header. Per-item failures driven by stale ifMatch
// are isolated within their chunk: ENTITY_MODIFIED items surface in a per-chunk
// `failed` array; their successful siblings still commit.
//
// Wire-format additions (see api/openapi.yaml EntityTransactionResponse):
//
//	{
//	  "transactionId": "<txid>",         // omitted on chunk-wide failure
//	  "entityIds":     ["..."],          // successful items only
//	  "failed":        [                 // optional, omitted when no per-item failures
//	    {"entityId": "<id>",
//	     "error": {"code": "ENTITY_MODIFIED", "message": "...", "itemIndex": <int>}}
//	  ]
//	}
//
// `failed` documents per-item ENTITY_MODIFIED outcomes that did NOT roll the
// chunk back. Other per-item failures (validation, missing entity, non-conflict
// engine errors) continue to roll the chunk back and surface as the chunk-wide
// `error` element on chunkIndex>0 chunks (or a 4xx response on chunk 0).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// getEntityTxID retrieves an entity and returns its current meta.transactionId.
// Used to build a fresh ifMatch token for the happy-path tests.
func getEntityTxID(t *testing.T, entityID string) string {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s", entityID)
	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getEntity %s: expected 200, got %d: %s", entityID, resp.StatusCode, body)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("parse entity: %v\nbody: %s", err, body)
	}
	meta, _ := result["meta"].(map[string]any)
	tx, _ := meta["transactionId"].(string)
	if tx == "" {
		t.Fatalf("entity %s: missing meta.transactionId in body: %s", entityID, body)
	}
	return tx
}

// updateCollectionItem builds a single update item literal with optional
// ifMatch. payload is JSON-escaped so it can be embedded as a string.
func updateCollectionItem(id, payload, ifMatch string) string {
	escaped := strings.ReplaceAll(payload, `"`, `\"`)
	if ifMatch == "" {
		return fmt.Sprintf(`{"id":"%s","payload":"%s"}`, id, escaped)
	}
	return fmt.Sprintf(`{"id":"%s","payload":"%s","ifMatch":"%s"}`, id, escaped, ifMatch)
}

// putUpdateCollection issues PUT /api/entity/JSON with the supplied items
// joined into a JSON array. Optional transactionWindow query parameter.
func putUpdateCollection(t *testing.T, items []string, transactionWindow string) (int, string) {
	t.Helper()
	body := "[" + strings.Join(items, ",") + "]"
	path := "/api/entity/JSON"
	if transactionWindow != "" {
		path += "?transactionWindow=" + transactionWindow
	}
	resp := doAuth(t, http.MethodPut, path, body)
	respBody := readBody(t, resp)
	return resp.StatusCode, respBody
}

// trivialModelWF returns a workflow that admits arbitrary loopback updates —
// initialState=CREATED with no transitions defined. Used in tests that focus
// on the ifMatch routing rather than workflow behavior.
func trivialModelWF() string {
	return `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "trivial-wf", "initialState": "CREATED", "active": true,
			"states": {
				"CREATED": {}
			}
		}]
	}`
}

// seedTwo creates two entities in the supplied model and returns their IDs.
func seedTwo(t *testing.T, model string) (string, string) {
	t.Helper()
	id1 := createEntityE2E(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)
	id2 := createEntityE2E(t, model, 1, `{"name":"B","amount":2,"status":"new"}`)
	return id1, id2
}

// --- Test 1: happy path — every item carries a fresh ifMatch ---
//
// Both items supply their current meta.transactionId as ifMatch. The chunk
// commits cleanly; response carries transactionId + entityIds and no
// `failed` field.
func TestUpdateCollection_IfMatch_HappyPath(t *testing.T) {
	const model = "e2e-upd-ifmatch-happy"
	setupModelWithWorkflow(t, model, trivialModelWF())

	id1, id2 := seedTwo(t, model)
	tx1 := getEntityTxID(t, id1)
	tx2 := getEntityTxID(t, id2)

	items := []string{
		updateCollectionItem(id1, `{"name":"A2","amount":11,"status":"upd"}`, tx1),
		updateCollectionItem(id2, `{"name":"B2","amount":22,"status":"upd"}`, tx2),
	}
	status, body := putUpdateCollection(t, items, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}

	var arr []map[string]any
	if err := json.Unmarshal([]byte(body), &arr); err != nil {
		t.Fatalf("parse: %v; body: %s", err, body)
	}
	if len(arr) != 1 {
		t.Fatalf("expected 1 chunk, got %d; body: %s", len(arr), body)
	}
	if _, has := arr[0]["failed"]; has {
		t.Errorf("expected no `failed` field on a fully-successful chunk; body: %s", body)
	}
	tx, _ := arr[0]["transactionId"].(string)
	if tx == "" {
		t.Errorf("missing transactionId; body: %s", body)
	}
	ids, _ := arr[0]["entityIds"].([]any)
	if len(ids) != 2 {
		t.Errorf("expected 2 entityIds, got %d; body: %s", len(ids), body)
	}

	// Both updates must have landed.
	if data := getEntityData(t, id1, ""); data["name"] != "A2" {
		t.Errorf("entity %s: expected name=A2, got %v", id1, data["name"])
	}
	if data := getEntityData(t, id2, ""); data["name"] != "B2" {
		t.Errorf("entity %s: expected name=B2, got %v", id2, data["name"])
	}
}

// --- Test 2: per-item stale ifMatch is isolated ---
//
// Item 1 carries a stale ifMatch; item 2 carries a fresh one. The chunk
// commits item 2's update; item 1 surfaces in `failed` with code
// ENTITY_MODIFIED and item 1's data on disk is unchanged.
//
// Audit-trail consistency (issue #228 reviewer S1): the failed item's audit
// log must contain BOTH an entry event (STATE_MACHINE_START) AND a
// compensating TRANSITION_ABORTED event with reason=ENTITY_MODIFIED so the
// audit trail is self-consistent — no orphaned start without a matching
// finish/abort. The abort event references the supplied (stale) txID as
// expectedTxId and the entity's actual current txID as actualTxId.
func TestUpdateCollection_IfMatch_PerItemStaleIsolated(t *testing.T) {
	const model = "e2e-upd-ifmatch-stale-iso"
	setupModelWithWorkflow(t, model, trivialModelWF())

	id1, id2 := seedTwo(t, model)
	const stale = "00000000-0000-0000-0000-000000000000"
	tx1Actual := getEntityTxID(t, id1) // captured for the abort event's actualTxId assertion below
	tx2 := getEntityTxID(t, id2)

	items := []string{
		updateCollectionItem(id1, `{"name":"A_STALE","amount":99,"status":"upd"}`, stale),
		updateCollectionItem(id2, `{"name":"B2","amount":22,"status":"upd"}`, tx2),
	}
	status, body := putUpdateCollection(t, items, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}

	var arr []map[string]any
	if err := json.Unmarshal([]byte(body), &arr); err != nil {
		t.Fatalf("parse: %v; body: %s", err, body)
	}
	if len(arr) != 1 {
		t.Fatalf("expected 1 chunk, got %d; body: %s", len(arr), body)
	}

	// Successful item must appear in entityIds.
	ids, _ := arr[0]["entityIds"].([]any)
	if len(ids) != 1 || ids[0].(string) != id2 {
		t.Errorf("entityIds: expected exactly [%s], got %v", id2, ids)
	}

	// Failed item must appear in `failed`.
	failed, _ := arr[0]["failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("failed: expected 1 entry, got %d; body: %s", len(failed), body)
	}
	f0 := failed[0].(map[string]any)
	if got, _ := f0["entityId"].(string); got != id1 {
		t.Errorf("failed[0].entityId: got %q, want %q", got, id1)
	}
	errObj, _ := f0["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "ENTITY_MODIFIED" {
		t.Errorf("failed[0].error.code: got %q, want ENTITY_MODIFIED; body: %s", code, body)
	}
	if msg, _ := errObj["message"].(string); msg == "" {
		t.Errorf("failed[0].error.message: empty; body: %s", body)
	}
	if idx, _ := errObj["itemIndex"].(float64); int(idx) != 0 {
		t.Errorf("failed[0].error.itemIndex: got %v, want 0", errObj["itemIndex"])
	}

	// transactionId still present (chunk committed).
	if tx, _ := arr[0]["transactionId"].(string); tx == "" {
		t.Errorf("missing transactionId on partially-successful chunk; body: %s", body)
	}

	// Item 1's data on disk must be unchanged (still A); item 2 must show B2.
	if data := getEntityData(t, id1, ""); data["name"] == "A_STALE" {
		t.Errorf("stale-ifMatch item 1 leaked an update: %v", data)
	}
	if data := getEntityData(t, id2, ""); data["name"] != "B2" {
		t.Errorf("fresh-ifMatch item 2 update did not land: %v", data)
	}

	// Audit-trail self-consistency: failed item must have STATE_MACHINE_START
	// paired with a TRANSITION_ABORTED carrying reason=ENTITY_MODIFIED.
	assertTransitionAbortedPaired(t, id1, stale, tx1Actual)
}

// assertTransitionAbortedPaired verifies that the entity's audit log contains
// both a STATE_MACHINE_START event and a matching TRANSITION_ABORTED event
// emitted by the engine or handler when a stale ifMatch precondition aborted
// the in-flight transition. The abort event's data must reference
// reason=ENTITY_MODIFIED, the supplied stale txID as expectedTxId, and the
// entity's actual row txID as actualTxId.
func assertTransitionAbortedPaired(t *testing.T, entityID, expectedStaleTx, actualEntityTx string) {
	t.Helper()
	events := getSMAuditEvents(t, entityID)
	var sawStart bool
	var abort map[string]any
	for _, ev := range events {
		switch ev["eventType"] {
		case "STATE_MACHINE_START":
			sawStart = true
		case "TRANSITION_ABORTED":
			abort = ev
		}
	}
	if !sawStart {
		t.Errorf("entity %s: missing STATE_MACHINE_START before abort; got events: %v", entityID, events)
	}
	if abort == nil {
		t.Fatalf("entity %s: missing TRANSITION_ABORTED event; got events: %v", entityID, events)
	}
	data, _ := abort["data"].(map[string]any)
	if data == nil {
		t.Fatalf("entity %s: TRANSITION_ABORTED has no data payload; ev=%v", entityID, abort)
	}
	if reason, _ := data["reason"].(string); reason != "ENTITY_MODIFIED" {
		t.Errorf("entity %s: abort reason = %q; want ENTITY_MODIFIED", entityID, reason)
	}
	if got, _ := data["expectedTxId"].(string); got != expectedStaleTx {
		t.Errorf("entity %s: abort expectedTxId = %q; want %q", entityID, got, expectedStaleTx)
	}
	if got, _ := data["actualTxId"].(string); got != actualEntityTx {
		t.Errorf("entity %s: abort actualTxId = %q; want %q", entityID, got, actualEntityTx)
	}
}

// --- Test 3: mixed presence — some items carry ifMatch, others don't ---
//
// Items without ifMatch behave exactly as before (no precondition). Items
// with ifMatch get the precondition. A stale ifMatch on item 1 is isolated;
// item 2 (no ifMatch) commits unconditionally; item 3 (fresh ifMatch) commits.
func TestUpdateCollection_IfMatch_MixedPresence(t *testing.T) {
	const model = "e2e-upd-ifmatch-mixed"
	setupModelWithWorkflow(t, model, trivialModelWF())

	id1 := createEntityE2E(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)
	id2 := createEntityE2E(t, model, 1, `{"name":"B","amount":2,"status":"new"}`)
	id3 := createEntityE2E(t, model, 1, `{"name":"C","amount":3,"status":"new"}`)
	tx3 := getEntityTxID(t, id3)
	const stale = "00000000-0000-0000-0000-000000000000"

	items := []string{
		updateCollectionItem(id1, `{"name":"A_STALE","amount":99,"status":"upd"}`, stale),
		updateCollectionItem(id2, `{"name":"B2","amount":22,"status":"upd"}`, ""),
		updateCollectionItem(id3, `{"name":"C2","amount":33,"status":"upd"}`, tx3),
	}
	status, body := putUpdateCollection(t, items, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}

	var arr []map[string]any
	json.Unmarshal([]byte(body), &arr)
	if len(arr) != 1 {
		t.Fatalf("expected 1 chunk, got %d; body: %s", len(arr), body)
	}
	failed, _ := arr[0]["failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("expected exactly 1 failed entry, got %d; body: %s", len(failed), body)
	}
	if got, _ := failed[0].(map[string]any)["entityId"].(string); got != id1 {
		t.Errorf("failed[0].entityId: got %q, want %q", got, id1)
	}
	ids, _ := arr[0]["entityIds"].([]any)
	if len(ids) != 2 {
		t.Errorf("expected 2 entityIds, got %d; body: %s", len(ids), body)
	}

	if data := getEntityData(t, id1, ""); data["name"] == "A_STALE" {
		t.Errorf("stale-ifMatch item 1 leaked an update: %v", data)
	}
	if data := getEntityData(t, id2, ""); data["name"] != "B2" {
		t.Errorf("no-ifMatch item 2 should have updated: %v", data)
	}
	if data := getEntityData(t, id3, ""); data["name"] != "C2" {
		t.Errorf("fresh-ifMatch item 3 should have updated: %v", data)
	}
}

// --- Test 4: every item has stale ifMatch ---
//
// All items fail their precondition. Chunk's transactionId is still emitted
// (the tx commits as a read-only/zero-write tx — the read set was validated).
// `entityIds` is an empty array; `failed` carries every item with itemIndex
// matching the request order.
func TestUpdateCollection_IfMatch_AllStale(t *testing.T) {
	const model = "e2e-upd-ifmatch-all-stale"
	setupModelWithWorkflow(t, model, trivialModelWF())

	id1, id2 := seedTwo(t, model)
	const stale = "00000000-0000-0000-0000-000000000000"
	tx1Actual := getEntityTxID(t, id1)
	tx2Actual := getEntityTxID(t, id2)

	items := []string{
		updateCollectionItem(id1, `{"name":"A_STALE","amount":99,"status":"upd"}`, stale),
		updateCollectionItem(id2, `{"name":"B_STALE","amount":99,"status":"upd"}`, stale),
	}
	status, body := putUpdateCollection(t, items, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}

	var arr []map[string]any
	json.Unmarshal([]byte(body), &arr)
	if len(arr) != 1 {
		t.Fatalf("expected 1 chunk, got %d; body: %s", len(arr), body)
	}
	if tx, _ := arr[0]["transactionId"].(string); tx == "" {
		t.Errorf("expected transactionId on all-stale chunk (zero-write commit); body: %s", body)
	}
	// Issue #228 I2: entityIds MUST be present (key exists) and an empty
	// JSON array on an all-stale zero-write chunk. Doc/wire parity:
	// `json:"entityIds"` (no omitempty) plus non-nil construction means
	// zero-success chunks emit `entityIds: []`, not omit it.
	rawIDs, idsPresent := arr[0]["entityIds"]
	if !idsPresent {
		t.Errorf("entityIds key missing from all-stale chunk; doc says it must be present as []; body: %s", body)
	}
	ids, idsTyped := rawIDs.([]any)
	if !idsTyped {
		t.Errorf("entityIds is not a JSON array on all-stale chunk; got %T (%v); body: %s", rawIDs, rawIDs, body)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty entityIds on all-failed chunk, got %d; body: %s", len(ids), body)
	}
	failed, _ := arr[0]["failed"].([]any)
	if len(failed) != 2 {
		t.Fatalf("expected 2 failed entries, got %d; body: %s", len(failed), body)
	}
	for i, want := range []string{id1, id2} {
		f := failed[i].(map[string]any)
		if got, _ := f["entityId"].(string); got != want {
			t.Errorf("failed[%d].entityId: got %q, want %q", i, got, want)
		}
		errObj, _ := f["error"].(map[string]any)
		if code, _ := errObj["code"].(string); code != "ENTITY_MODIFIED" {
			t.Errorf("failed[%d].error.code: got %q, want ENTITY_MODIFIED", i, code)
		}
		if idx, _ := errObj["itemIndex"].(float64); int(idx) != i {
			t.Errorf("failed[%d].error.itemIndex: got %v, want %d", i, errObj["itemIndex"], i)
		}
	}

	// Neither item touched on disk.
	if data := getEntityData(t, id1, ""); data["name"] != "A" {
		t.Errorf("stale-ifMatch chunk leaked an update on item 1: %v", data)
	}
	if data := getEntityData(t, id2, ""); data["name"] != "B" {
		t.Errorf("stale-ifMatch chunk leaked an update on item 2: %v", data)
	}

	// Audit-trail self-consistency: every failed item must have a paired
	// STATE_MACHINE_START + TRANSITION_ABORTED sequence (issue #228 S1).
	assertTransitionAbortedPaired(t, id1, stale, tx1Actual)
	assertTransitionAbortedPaired(t, id2, stale, tx2Actual)
}

// --- Test 5: CBD interaction with stale ifMatch ---
//
// When an item's transition has a COMMIT_BEFORE_DISPATCH processor, the engine
// consumes ifMatch at the first segment-flush (spec §4.1). A stale ifMatch
// must isolate the item without firing the processor's external dispatch and
// without letting the item's update land. The processor is the same dispatch
// counter used by TestWorkflowProc_UpdateWithCBD_StaleIfMatchAbortsBeforeDispatch.
func TestUpdateCollection_IfMatch_CBDStaleAbortsBeforeDispatch(t *testing.T) {
	const model = "e2e-upd-ifmatch-cbd"

	var dispatchCount atomic.Int32
	procSvc.RegisterProcessor("upd-coll-cbd-counted", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		dispatchCount.Add(1)
		return entity, nil
	})
	defer procSvc.Reset()

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "upd-coll-cbd-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":     {"transitions": [{"name": "init", "next": "PENDING", "manual": false}]},
				"PENDING":  {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true,
					"processors": [{"type": "calculator", "name": "upd-coll-cbd-counted",
						"executionMode": "COMMIT_BEFORE_DISPATCH",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"APPROVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	id1 := createEntityE2E(t, model, 1, `{"name":"X","amount":1,"status":"new"}`)
	if state := getEntityState(t, id1); state != "PENDING" {
		t.Fatalf("post-create state = %q; want PENDING", state)
	}
	const stale = "00000000-0000-0000-0000-000000000000"
	tx1Actual := getEntityTxID(t, id1)

	body := fmt.Sprintf(
		`[{"id":"%s","payload":"{\"name\":\"X_STALE\",\"amount\":99,\"status\":\"upd\"}","transition":"approve","ifMatch":"%s"}]`,
		id1, stale)
	resp := doAuth(t, http.MethodPut, "/api/entity/JSON", body)
	rbody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (per-item failure isolated); body: %s", resp.StatusCode, rbody)
	}

	var arr []map[string]any
	if err := json.Unmarshal([]byte(rbody), &arr); err != nil {
		t.Fatalf("parse: %v; body: %s", err, rbody)
	}
	if len(arr) != 1 {
		t.Fatalf("expected 1 chunk, got %d; body: %s", len(arr), rbody)
	}
	failed, _ := arr[0]["failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("expected 1 failed entry, got %d; body: %s", len(failed), rbody)
	}
	f := failed[0].(map[string]any)
	errObj, _ := f["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "ENTITY_MODIFIED" {
		t.Errorf("failed[0].error.code: got %q, want ENTITY_MODIFIED; body: %s", code, rbody)
	}

	// CBD dispatch must NOT have fired (spec §4.1 strictly-earlier-enforcement).
	if c := dispatchCount.Load(); c != 0 {
		t.Errorf("CBD dispatch fired %d time(s) despite stale ifMatch on collection item — spec §4.1 violated", c)
	}
	// Entity must still be PENDING with original payload.
	if state := getEntityState(t, id1); state != "PENDING" {
		t.Errorf("post-failure state = %q; want PENDING (cascade should have aborted)", state)
	}
	if data := getEntityData(t, id1, ""); data["name"] == "X_STALE" {
		t.Errorf("stale-ifMatch item leaked update: %v", data)
	}

	// Audit-trail self-consistency: the engine-side abort happens at the CBD
	// segment-flush BEFORE the external dispatch. The TRANSITION_ABORTED event
	// must precede any dispatch attempt and pair with the entry-side
	// STATE_MACHINE_START (issue #228 S1).
	assertTransitionAbortedPaired(t, id1, stale, tx1Actual)
}

// --- Test 6: non-conflict per-item failure still rolls the chunk back ---
//
// A missing-entity item among ifMatch-bearing siblings is NOT an
// ENTITY_MODIFIED conflict — it's an ENTITY_NOT_FOUND. Per-item isolation is
// reserved for ENTITY_MODIFIED. ENTITY_NOT_FOUND continues to roll back the
// entire chunk and surfaces as a 4xx (chunk 0) or chunk-wide error element.
func TestUpdateCollection_IfMatch_NonConflictRollsBackChunk(t *testing.T) {
	const model = "e2e-upd-ifmatch-nonconflict"
	setupModelWithWorkflow(t, model, trivialModelWF())

	id1 := createEntityE2E(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)
	tx1 := getEntityTxID(t, id1)
	const bogus = "00000000-0000-0000-0000-000000000099"

	items := []string{
		updateCollectionItem(id1, `{"name":"A_NEVER","amount":99,"status":"upd"}`, tx1),
		updateCollectionItem(bogus, `{"name":"never","amount":0,"status":"upd"}`, ""),
	}
	status, body := putUpdateCollection(t, items, "")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (missing-entity rolls chunk back); body: %s", status, body)
	}
	// Item 1 must be unchanged — the chunk rolled back even though the
	// fresh ifMatch on item 1 would have otherwise let it commit.
	if data := getEntityData(t, id1, ""); data["name"] == "A_NEVER" {
		t.Errorf("missing-entity sibling did not roll back item 1: %v", data)
	}
}

// --- Test 7: per-item ifMatch failures across multi-chunk batch ---
//
// With transactionWindow=2, four items split into two chunks. Item 0 (chunk 0)
// has stale ifMatch — it surfaces in chunk 0's `failed` while item 1 commits.
// Chunk 1 has fresh ifMatches on both items, both commit. Two response
// elements, neither carrying the chunk-wide `error` shape, both carrying
// transactionIds.
func TestUpdateCollection_IfMatch_MultiChunkPerItemFailures(t *testing.T) {
	const model = "e2e-upd-ifmatch-multichunk"
	setupModelWithWorkflow(t, model, trivialModelWF())

	const total = 4
	ids := make([]string, total)
	for i := 0; i < total; i++ {
		ids[i] = createEntityE2E(t, model, 1, fmt.Sprintf(`{"name":"orig-%d","amount":%d,"status":"new"}`, i, i))
	}
	const stale = "00000000-0000-0000-0000-000000000000"
	tx1 := getEntityTxID(t, ids[1])
	tx2 := getEntityTxID(t, ids[2])
	tx3 := getEntityTxID(t, ids[3])

	items := []string{
		updateCollectionItem(ids[0], `{"name":"upd-0","amount":10,"status":"upd"}`, stale),
		updateCollectionItem(ids[1], `{"name":"upd-1","amount":11,"status":"upd"}`, tx1),
		updateCollectionItem(ids[2], `{"name":"upd-2","amount":12,"status":"upd"}`, tx2),
		updateCollectionItem(ids[3], `{"name":"upd-3","amount":13,"status":"upd"}`, tx3),
	}
	status, body := putUpdateCollection(t, items, "2")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}

	var arr []map[string]any
	json.Unmarshal([]byte(body), &arr)
	if len(arr) != 2 {
		t.Fatalf("expected 2 chunks, got %d; body: %s", len(arr), body)
	}

	// Chunk 0: 1 success + 1 failure. No chunk-wide error.
	if _, has := arr[0]["error"]; has {
		t.Errorf("chunk 0: did not expect chunk-wide error; body: %s", body)
	}
	c0failed, _ := arr[0]["failed"].([]any)
	if len(c0failed) != 1 {
		t.Errorf("chunk 0: expected 1 failed entry, got %d; body: %s", len(c0failed), body)
	} else {
		idx, _ := c0failed[0].(map[string]any)["error"].(map[string]any)["itemIndex"].(float64)
		// itemIndex is per-chunk-relative (0 within chunk 0).
		if int(idx) != 0 {
			t.Errorf("chunk 0 failed[0].itemIndex: got %v, want 0 (per-chunk relative)", idx)
		}
	}
	c0ids, _ := arr[0]["entityIds"].([]any)
	if len(c0ids) != 1 || c0ids[0].(string) != ids[1] {
		t.Errorf("chunk 0 entityIds: expected [%s], got %v", ids[1], c0ids)
	}

	// Chunk 1: both items commit; no `failed`.
	if _, has := arr[1]["failed"]; has {
		t.Errorf("chunk 1: did not expect `failed` field; body: %s", body)
	}
	c1ids, _ := arr[1]["entityIds"].([]any)
	if len(c1ids) != 2 {
		t.Errorf("chunk 1 entityIds: expected 2, got %d; body: %s", len(c1ids), body)
	}

	// Item 0 unchanged; items 1..3 updated.
	if data := getEntityData(t, ids[0], ""); data["name"] != "orig-0" {
		t.Errorf("item 0 leaked an update: %v", data)
	}
	for i := 1; i < total; i++ {
		want := fmt.Sprintf("upd-%d", i)
		if data := getEntityData(t, ids[i], ""); data["name"] != want {
			t.Errorf("item %d: expected name=%s, got %v", i, want, data["name"])
		}
	}
}

// --- Test 8: ifMatch absent — regression — non-IfMatch items still work ---
//
// All items omit ifMatch. The behavior must match the pre-#228 contract: a
// single chunk with transactionId + entityIds, no `failed` field, all updates
// land. This pins the no-regression invariant.
func TestUpdateCollection_IfMatch_AbsentRegression(t *testing.T) {
	const model = "e2e-upd-ifmatch-absent"
	setupModelWithWorkflow(t, model, trivialModelWF())

	id1, id2 := seedTwo(t, model)
	items := []string{
		updateCollectionItem(id1, `{"name":"A2","amount":11,"status":"upd"}`, ""),
		updateCollectionItem(id2, `{"name":"B2","amount":22,"status":"upd"}`, ""),
	}
	status, body := putUpdateCollection(t, items, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}
	var arr []map[string]any
	json.Unmarshal([]byte(body), &arr)
	if len(arr) != 1 {
		t.Fatalf("expected 1 chunk, got %d; body: %s", len(arr), body)
	}
	if _, has := arr[0]["failed"]; has {
		t.Errorf("no-ifMatch chunk should not carry `failed`; body: %s", body)
	}
	ids, _ := arr[0]["entityIds"].([]any)
	if len(ids) != 2 {
		t.Errorf("expected 2 entityIds, got %d; body: %s", len(ids), body)
	}
	if data := getEntityData(t, id1, ""); data["name"] != "A2" {
		t.Errorf("item 1 update did not land: %v", data)
	}
	if data := getEntityData(t, id2, ""); data["name"] != "B2" {
		t.Errorf("item 2 update did not land: %v", data)
	}
}
