package e2e_test

// transaction_control_test.go is the running-backend e2e coverage for the
// transaction-control-params feature (issue #379): ?transactionTimeoutMillis
// on the 7 entity write ops + newMessage, ?transactionSize on
// deleteEntities/deleteMessages, and ?timeoutMillis on searchEntities. The
// feature surface itself (validation, deadline attachment, 408
// classification, batching) shipped and was unit-tested in earlier tasks of
// this plan (T1-T18); this file proves it end to end through the real
// HTTP(+gRPC) stack.
//
// Coverage cells (spec matrix, task-19-brief.md):
//   1. 408 per entity write op (7 ops) — TestTransactionControl_WriteOps_TransactionTimeout408
//   2. 400 invalid param per declaring op (11 ops) — TestTransactionControl_InvalidParams400
//   3. Joined-request 400 (write + delete + search) — TestTransactionControl_JoinedRequests400
//   4. Chunked create, post-first-chunk 408 — TestTransactionControl_ChunkedCreate_PostFirstChunk408
//   5. Batched deleteEntities happy path — TestTransactionControl_DeleteEntities_BatchedHappyPath
//   6. Batched deleteMessages — TestTransactionControl_DeleteMessages_Batched
//   7. Absent params unchanged (regression pin) — TestTransactionControl_AbsentParams_UnchangedShapes
//   8. Search timeoutMillis (400 + happy path) — TestTransactionControl_Search_TimeoutParam
//   9. newMessage transactionTimeoutMillis (400 + happy path) — TestTransactionControl_NewMessage_TimeoutParam
//
// Cell 1's set is the SAME 7 write ops the unit-level table
// (internal/domain/entity/handler_reqtimeout_test.go's reqTimeoutOps) drives
// with a fake blocking store: Create, CreateCollection, UpdateCollection,
// UpdateSingleWithLoopback, UpdateSingle, PatchSingleWithLoopback,
// PatchSingle. newMessage carries the same param but is not included in cell
// 1 — its pre-Save 408 path has no compute-node dispatch to park on and is
// already covered at the unit level; this file's newMessage coverage (cell
// 9) is limited to 400/happy-path per the brief.
//
// Cells 1/3/4 need a real gRPC compute member to dispatch a SYNC processor
// that withholds its reply — the deterministic mechanism that lets the
// deadline win (internal/grpc/dispatch.go's dispatchCalloutToMember selects
// on ctx.Done() when the reply never arrives) — so they use the callback
// harness (callback_harness_test.go). Every blocking processor's release
// channel is closed via the SUBTEST's own t.Cleanup, which fires before the
// harness-level t.Cleanup(h.member.stop) registered by newCallbackHarness
// (LIFO order across nesting), avoiding the 5s handler-drain timeout
// documented at callback_harness_test.go:645-668.
//
// Cells 2/5/6/7/8/9 need no compute dispatch, so they run against the shared
// package-level testApp via doAuth/readBody like every other e2e test in
// this package.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// --- shared literals ---

// txctlSimpleCond is a syntactically-valid AbstractConditionDto simple
// condition, reused wherever a handler's param-validation order requires the
// request body to parse successfully before the transactionTimeoutMillis /
// transactionSize / timeoutMillis check is reached (deleteEntities parses its
// body after the param check, so it doesn't need this; searchEntities parses
// its body — the condition — BEFORE the param check, so it does).
const txctlSimpleCond = `{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"draft"}`

// --- callback-harness helpers (cells 1, 3, 4) ---

// txctlRegisterBlockingProc registers a SYNC processor on h under name that
// blocks forever until released. The release channel is closed via t's own
// Cleanup, which — for a subtest's *testing.T — runs before the parent
// harness's own t.Cleanup(h.member.stop) (LIFO order), so the compute
// member's handler goroutine is unblocked before the harness starts its
// shutdown drain.
func txctlRegisterBlockingProc(t *testing.T, h *callbackHarness, name string) {
	t.Helper()
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	h.RegisterProc(name, func(rc *reqCtx) (map[string]any, error) {
		<-block
		return nil, nil
	})
}

// txctlSetupCreateStyleModel registers a model whose sole (automated) NONE ->
// ACTIVE transition carries a blocking SYNC processor, so any create against
// it hangs until the processor is released. Used for the Create /
// CreateCollection cell-1 cases, where the deadline must fire before the
// entity is ever committed.
func txctlSetupCreateStyleModel(t *testing.T, h *callbackHarness, model, procName string) {
	t.Helper()
	txctlRegisterBlockingProc(t, h, procName)
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": %q, "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`, model+"-wf", procName)
	h.SetupModelWithWorkflow(t, model, wf)
}

// txctlSetupNamedTransitionModel registers a model with an instant (no
// processor) NONE -> ACTIVE auto transition, followed by a MANUAL ACTIVE ->
// BLOCKED "promote" transition carrying a blocking SYNC processor. Creates
// one entity (which lands in ACTIVE, since "init" has no processor) and
// returns its id. Used for the UpdateSingle / PatchSingle / UpdateCollection
// (explicit transition) cell-1 cases — invoking "promote" by name dispatches
// the blocking processor without needing a criterion.
func txctlSetupNamedTransitionModel(t *testing.T, h *callbackHarness, model, procName string) string {
	t.Helper()
	txctlRegisterBlockingProc(t, h, procName)
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false}]},
				"ACTIVE": {"transitions": [{"name": "promote", "next": "BLOCKED", "manual": true,
					"processors": [{"type": "calculator", "name": %q, "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"BLOCKED": {}
			}
		}]
	}`, model+"-wf", procName)
	h.SetupModelWithWorkflow(t, model, wf)
	entityID, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("setup create for %s: expected 200, got %d: %s", model, status, body)
	}
	return entityID
}

// txctlSetupLoopbackModel is txctlSetupNamedTransitionModel's loopback
// counterpart: the ACTIVE -> BLOCKED "promote" transition is AUTOMATED but
// gated by a simple criterion (data.status == "trigger"), so it only fires
// when a loopback update actually sets that field — not on create (which
// uses workflowSampleModel's status:"draft") and not merely by reaching
// ACTIVE. Used for UpdateSingleWithLoopback / PatchSingleWithLoopback.
func txctlSetupLoopbackModel(t *testing.T, h *callbackHarness, model, procName string) string {
	t.Helper()
	txctlRegisterBlockingProc(t, h, procName)
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false}]},
				"ACTIVE": {"transitions": [{"name": "promote", "next": "BLOCKED", "manual": false,
					"criterion": {"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"trigger"},
					"processors": [{"type": "calculator", "name": %q, "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"BLOCKED": {}
			}
		}]
	}`, model+"-wf", procName)
	h.SetupModelWithWorkflow(t, model, wf)
	entityID, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("setup create for %s: expected 200, got %d: %s", model, status, body)
	}
	return entityID
}

// txctlHarnessPatch issues an authenticated PATCH against h with an explicit
// Content-Type and If-Match header. h.DoAuth always sends
// Content-Type: application/json and has no If-Match parameter, neither of
// which the shared patch() implementation's 415/428 precedence accepts.
func txctlHarnessPatch(t *testing.T, h *callbackHarness, path, contentType, ifMatch, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, h.baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new PATCH request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token(t))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("If-Match", ifMatch)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", path, err)
	}
	return resp
}

// txctlCollectionItem builds an UpdateCollection item literal naming an
// explicit transition (payload is JSON-escaped to embed as a wire string,
// matching the endpoint's documented "payload is a JSON-encoded string" contract).
func txctlCollectionItem(id, payload, transition string) string {
	escaped := strings.ReplaceAll(payload, `"`, `\"`)
	return fmt.Sprintf(`[{"id":"%s","payload":"%s","transition":"%s"}]`, id, escaped, transition)
}

// txctlAssert408 asserts the RFC 9457 problem+json 408 TRANSACTION_TIMEOUT
// shape manually — the callback-harness stack bypasses the openapivalidator
// middleware (see this file's header comment), so nothing else checks it.
func txctlAssert408(t *testing.T, h *callbackHarness, resp *http.Response) {
	t.Helper()
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408; body: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("decode problem detail: %v; body: %s", err, body)
	}
	if pd.Properties["errorCode"] != "TRANSACTION_TIMEOUT" {
		t.Errorf("errorCode = %v, want TRANSACTION_TIMEOUT; body: %s", pd.Properties["errorCode"], body)
	}
	if pd.Properties["retryable"] != true {
		t.Errorf("retryable = %v, want true; body: %s", pd.Properties["retryable"], body)
	}
}

// --- Cell 1: 408 per entity write op ---

func TestTransactionControl_WriteOps_TransactionTimeout408(t *testing.T) {
	h := newCallbackHarness(t)

	t.Run("Create", func(t *testing.T) {
		const model = "e2e-txctl408-create"
		txctlSetupCreateStyleModel(t, h, model, "txctl408-create-proc")
		resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1?transactionTimeoutMillis=1000", model), workflowSampleModel, "")
		txctlAssert408(t, h, resp)

		list := h.DoAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/1", model), "", "")
		listBody := h.readBody(t, list)
		if list.StatusCode != http.StatusOK {
			t.Fatalf("list entities: expected 200, got %d: %s", list.StatusCode, listBody)
		}
		var entities []map[string]any
		if err := json.Unmarshal([]byte(listBody), &entities); err != nil {
			t.Fatalf("decode entity list: %v; body: %s", err, listBody)
		}
		if len(entities) != 0 {
			t.Errorf("expected 0 entities after 408 (nothing committed), got %d: %s", len(entities), listBody)
		}
	})

	t.Run("CreateCollection", func(t *testing.T) {
		const model = "e2e-txctl408-createcoll"
		txctlSetupCreateStyleModel(t, h, model, "txctl408-createcoll-proc")
		escaped := strings.ReplaceAll(workflowSampleModel, `"`, `\"`)
		body := fmt.Sprintf(`[{"model":{"name":%q,"version":1},"payload":"%s"}]`, model, escaped)
		resp := h.DoAuth(t, http.MethodPost, "/api/entity/JSON?transactionTimeoutMillis=1000", body, "")
		txctlAssert408(t, h, resp)

		list := h.DoAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/1", model), "", "")
		listBody := h.readBody(t, list)
		if list.StatusCode != http.StatusOK {
			t.Fatalf("list entities: expected 200, got %d: %s", list.StatusCode, listBody)
		}
		var entities []map[string]any
		if err := json.Unmarshal([]byte(listBody), &entities); err != nil {
			t.Fatalf("decode entity list: %v; body: %s", err, listBody)
		}
		if len(entities) != 0 {
			t.Errorf("expected 0 entities after 408 (nothing committed), got %d: %s", len(entities), listBody)
		}
	})

	t.Run("UpdateSingle", func(t *testing.T) {
		const model = "e2e-txctl408-update"
		entityID := txctlSetupNamedTransitionModel(t, h, model, "txctl408-update-proc")
		resp := h.DoAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s/promote?transactionTimeoutMillis=1000", entityID), workflowSampleModel, "")
		txctlAssert408(t, h, resp)
		if st, _ := h.GetEntityState(t, entityID); st != "ACTIVE" {
			t.Errorf("state after 408 = %q, want ACTIVE (nothing committed)", st)
		}
	})

	t.Run("PatchSingle", func(t *testing.T) {
		const model = "e2e-txctl408-patch"
		entityID := txctlSetupNamedTransitionModel(t, h, model, "txctl408-patch-proc")
		resp := txctlHarnessPatch(t, h, fmt.Sprintf("/api/entity/JSON/%s/promote?transactionTimeoutMillis=1000", entityID), "application/merge-patch+json", "*", `{}`)
		txctlAssert408(t, h, resp)
		if st, _ := h.GetEntityState(t, entityID); st != "ACTIVE" {
			t.Errorf("state after 408 = %q, want ACTIVE (nothing committed)", st)
		}
	})

	t.Run("UpdateCollection", func(t *testing.T) {
		const model = "e2e-txctl408-updatecoll"
		entityID := txctlSetupNamedTransitionModel(t, h, model, "txctl408-updatecoll-proc")
		body := txctlCollectionItem(entityID, workflowSampleModel, "promote")
		resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON?transactionTimeoutMillis=1000", body, "")
		txctlAssert408(t, h, resp)
		if st, _ := h.GetEntityState(t, entityID); st != "ACTIVE" {
			t.Errorf("state after 408 = %q, want ACTIVE (nothing committed)", st)
		}
	})

	t.Run("UpdateSingleWithLoopback", func(t *testing.T) {
		const model = "e2e-txctl408-updateloop"
		entityID := txctlSetupLoopbackModel(t, h, model, "txctl408-updateloop-proc")
		resp := h.DoAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s?transactionTimeoutMillis=1000", entityID),
			`{"name":"Test Order","amount":100,"status":"trigger"}`, "")
		txctlAssert408(t, h, resp)
		if st, _ := h.GetEntityState(t, entityID); st != "ACTIVE" {
			t.Errorf("state after 408 = %q, want ACTIVE (nothing committed)", st)
		}
		if data := h.GetEntityData(t, entityID); data["status"] != "draft" {
			t.Errorf("status after 408 = %v, want unchanged \"draft\" (nothing committed)", data["status"])
		}
	})

	t.Run("PatchSingleWithLoopback", func(t *testing.T) {
		const model = "e2e-txctl408-patchloop"
		entityID := txctlSetupLoopbackModel(t, h, model, "txctl408-patchloop-proc")
		resp := txctlHarnessPatch(t, h, fmt.Sprintf("/api/entity/JSON/%s?transactionTimeoutMillis=1000", entityID),
			"application/merge-patch+json", "*", `{"status":"trigger"}`)
		txctlAssert408(t, h, resp)
		if st, _ := h.GetEntityState(t, entityID); st != "ACTIVE" {
			t.Errorf("state after 408 = %q, want ACTIVE (nothing committed)", st)
		}
		if data := h.GetEntityData(t, entityID); data["status"] != "draft" {
			t.Errorf("status after 408 = %v, want unchanged \"draft\" (nothing committed)", data["status"])
		}
	})
}

// --- Cell 3 (+ extra): joined-request 400 across write, delete, AND search ---
//
// The extra cell (beyond the brief) was surfaced by review of task 11: the
// spec's joined-rejection matrix row covers write, search, and delete, but
// the brief's item 3 only listed write+delete. This test drives all three
// from inside the SAME callback-harness processor, echoing the tx token on
// each — the same technique callback_txjoin_test.go uses for the write leg.

func TestTransactionControl_JoinedRequests400(t *testing.T) {
	h := newCallbackHarness(t)

	const secondary = "e2e-txctl-joined-secondary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	type joinedResult struct {
		createStatus, deleteStatus, searchStatus int
		createBody, deleteBody, searchBody       string
	}
	resultsCh := make(chan joinedResult, 1)

	h.RegisterProc("txctl-joined400", func(rc *reqCtx) (map[string]any, error) {
		createRes, cErr := rc.h.callback(http.MethodPost,
			fmt.Sprintf("/api/entity/JSON/%s/1?transactionTimeoutMillis=5000", secondary),
			`{"name":"x","amount":1,"status":"draft"}`, rc.token)
		deleteRes, dErr := rc.h.callback(http.MethodDelete,
			fmt.Sprintf("/api/entity/%s/1?transactionSize=2", secondary),
			txctlSimpleCond, rc.token)
		searchRes, sErr := rc.h.callback(http.MethodPost,
			fmt.Sprintf("/api/search/direct/%s/1?timeoutMillis=5000", secondary),
			txctlSimpleCond, rc.token)
		if cErr != nil || dErr != nil || sErr != nil {
			return nil, fmt.Errorf("callback transport error: create=%v delete=%v search=%v", cErr, dErr, sErr)
		}
		resultsCh <- joinedResult{
			createStatus: createRes.StatusCode, createBody: createRes.Body,
			deleteStatus: deleteRes.StatusCode, deleteBody: deleteRes.Body,
			searchStatus: searchRes.StatusCode, searchBody: searchRes.Body,
		}
		return nil, nil
	})

	const primary = "e2e-txctl-joined-primary"
	primaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "txctl-joined-primary-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "txctl-joined400", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, primary, primaryWF)

	_, status, body := h.CreateEntity(t, primary, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("primary create: expected 200, got %d: %s", status, body)
	}

	var res joinedResult
	select {
	case res = <-resultsCh:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for the joined-callback results")
	}

	if res.createStatus != http.StatusBadRequest {
		t.Errorf("joined create with transactionTimeoutMillis: status = %d, want 400; body: %s", res.createStatus, res.createBody)
	} else if !strings.Contains(res.createBody, "transactionTimeoutMillis") {
		t.Errorf("joined create 400 body does not mention the param: %s", res.createBody)
	}
	if res.deleteStatus != http.StatusBadRequest {
		t.Errorf("joined deleteEntities with transactionSize: status = %d, want 400; body: %s", res.deleteStatus, res.deleteBody)
	} else if !strings.Contains(res.deleteBody, "transactionSize") {
		t.Errorf("joined delete 400 body does not mention the param: %s", res.deleteBody)
	}
	if res.searchStatus != http.StatusBadRequest {
		t.Errorf("joined search with timeoutMillis: status = %d, want 400; body: %s", res.searchStatus, res.searchBody)
	} else if !strings.Contains(res.searchBody, "timeoutMillis") {
		t.Errorf("joined search 400 body does not mention the param: %s", res.searchBody)
	}
}

// --- Cell 4: chunked create, post-first-chunk 408 ---

func TestTransactionControl_ChunkedCreate_PostFirstChunk408(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "e2e-txctl408-chunked"

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	h.RegisterProc("txctl-chunk-block", func(rc *reqCtx) (map[string]any, error) {
		if seq, _ := rc.entityData["seq"].(float64); seq == 2 {
			<-block
		}
		return nil, nil
	})

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "txctl-chunked-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "txctl-chunk-block", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`
	h.setupModelSampleWithWorkflow(t, model, workflowSampleWith(`"seq": 0`), wf)

	body := `[
		{"name":"Test Order","amount":100,"status":"draft","seq":1},
		{"name":"Test Order","amount":100,"status":"draft","seq":2},
		{"name":"Test Order","amount":100,"status":"draft","seq":3}
	]`
	resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1?transactionWindow=1&transactionTimeoutMillis=1000", model), body, "")
	respBody := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chunked create: expected 200, got %d: %s", resp.StatusCode, respBody)
	}

	type txctlChunkErr struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		ChunkIndex int    `json:"chunkIndex"`
	}
	type txctlChunkResult struct {
		TransactionID string         `json:"transactionId,omitempty"`
		EntityIDs     []string       `json:"entityIds"`
		Error         *txctlChunkErr `json:"error,omitempty"`
	}
	var results []txctlChunkResult
	if err := json.Unmarshal([]byte(respBody), &results); err != nil {
		t.Fatalf("decode chunked create response: %v; body: %s", err, respBody)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d entries, want 2 (chunk 0 success + chunk 1 error, chunk 2 absent); got %+v", len(results), results)
	}
	if results[0].Error != nil {
		t.Errorf("chunk 0 unexpectedly failed: %+v", results[0].Error)
	}
	if len(results[0].EntityIDs) != 1 {
		t.Errorf("chunk 0 EntityIDs = %v, want exactly 1", results[0].EntityIDs)
	}
	if results[1].Error == nil {
		t.Fatal("chunk 1 expected a TRANSACTION_TIMEOUT error element, got none")
	}
	if results[1].Error.Code != "TRANSACTION_TIMEOUT" {
		t.Errorf("chunk 1 error code = %q, want TRANSACTION_TIMEOUT", results[1].Error.Code)
	}
	if results[1].Error.ChunkIndex != 1 {
		t.Errorf("chunk 1 ChunkIndex = %d, want 1", results[1].Error.ChunkIndex)
	}
}

// --- Cell 2: 400 invalid param per declaring op (11 ops) ---

// txctlInvalidParamOp names one HTTP call whose sole varying ingredient is
// the raw query-string value handed to its transactionTimeoutMillis /
// transactionSize / timeoutMillis parameter.
type txctlInvalidParamOp struct {
	name  string
	issue func(t *testing.T, val string) *http.Response
}

func txctlInvalidParamOps() []txctlInvalidParamOp {
	uid := uuid.NewString()
	return []txctlInvalidParamOp{
		{"Create", func(t *testing.T, val string) *http.Response {
			return doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/e2e-txctl-badparam/1?transactionTimeoutMillis=%s", val), "")
		}},
		{"CreateCollection", func(t *testing.T, val string) *http.Response {
			return doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON?transactionTimeoutMillis=%s", val), "")
		}},
		{"UpdateCollection", func(t *testing.T, val string) *http.Response {
			return doAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON?transactionTimeoutMillis=%s", val), "")
		}},
		{"UpdateSingleWithLoopback", func(t *testing.T, val string) *http.Response {
			return doAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s?transactionTimeoutMillis=%s", uid, val), "")
		}},
		{"UpdateSingle", func(t *testing.T, val string) *http.Response {
			return doAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s/someTransition?transactionTimeoutMillis=%s", uid, val), "")
		}},
		{"PatchSingleWithLoopback", func(t *testing.T, val string) *http.Response {
			return patchEntity(t, fmt.Sprintf("/api/entity/JSON/%s?transactionTimeoutMillis=%s", uid, val), "application/merge-patch+json", "*", "")
		}},
		{"PatchSingle", func(t *testing.T, val string) *http.Response {
			return patchEntity(t, fmt.Sprintf("/api/entity/JSON/%s/someTransition?transactionTimeoutMillis=%s", uid, val), "application/merge-patch+json", "*", "")
		}},
		{"DeleteEntities", func(t *testing.T, val string) *http.Response {
			return doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/e2e-txctl-badparam-del/1?transactionSize=%s", val), "")
		}},
		{"DeleteMessages", func(t *testing.T, val string) *http.Response {
			return doAuth(t, http.MethodDelete, fmt.Sprintf("/api/message?transactionSize=%s", val), `[]`)
		}},
		{"NewMessage", func(t *testing.T, val string) *http.Response {
			return doAuth(t, http.MethodPost, fmt.Sprintf("/api/message/new/e2e-txctl-badparam?transactionTimeoutMillis=%s", val), `{"payload":{"x":1}}`)
		}},
		{"SearchEntities", func(t *testing.T, val string) *http.Response {
			return doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/e2e-txctl-badparam-search/1?timeoutMillis=%s", val), txctlSimpleCond)
		}},
	}
}

func TestTransactionControl_InvalidParams400(t *testing.T) {
	for _, op := range txctlInvalidParamOps() {
		for _, val := range []string{"0", "-1", "abc"} {
			t.Run(fmt.Sprintf("%s/%s", op.name, val), func(t *testing.T) {
				resp := op.issue(t, val)
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, readBody(t, resp))
				}
				commontest.ExpectErrorCode(t, resp, "BAD_REQUEST")
			})
		}
	}
}

// --- Cell 5: batched deleteEntities happy path ---

func TestTransactionControl_DeleteEntities_BatchedHappyPath(t *testing.T) {
	const model = "e2e-txctl-delbatch"
	importModelWithSample(t, model, 1, `{"n":0}`)
	lockModelE2E(t, model, 1)

	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		ids = append(ids, createEntityE2E(t, model, 1, fmt.Sprintf(`{"n":%d}`, i)))
	}

	cond := `{"type":"simple","jsonPath":"$.n","operatorType":"GREATER_OR_EQUAL","value":0}`
	resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1?transactionSize=2&verbose=true", model), cond)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batched delete: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	dr, _ := obj["deleteResult"].(map[string]any)
	if got := dr["numberOfEntititesRemoved"]; got != float64(5) {
		t.Errorf("numberOfEntititesRemoved = %v, want 5", got)
	}
	respIDs, _ := obj["ids"].([]any)
	if len(respIDs) != 5 {
		t.Fatalf("ids = %v, want 5 entries", respIDs)
	}
	seen := map[string]bool{}
	for _, id := range respIDs {
		s, _ := id.(string)
		seen[s] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("expected deleted id %s to be echoed in ids, got %v", id, respIDs)
		}
	}
	for _, id := range ids {
		if r := doAuth(t, http.MethodGet, "/api/entity/"+id, ""); r.StatusCode != http.StatusNotFound {
			t.Errorf("entity %s should be gone after batched delete, got %d", id, r.StatusCode)
		}
	}
}

// --- Cell 6: batched deleteMessages ---

func TestTransactionControl_DeleteMessages_Batched(t *testing.T) {
	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		ids = append(ids, createMessageE2E(t, "e2e-txctl-msgbatch", fmt.Sprintf(`{"n":%d}`, i)))
	}
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		t.Fatalf("marshal ids: %v", err)
	}
	resp := doAuth(t, http.MethodDelete, "/api/message?transactionSize=2", string(idsJSON))
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batched deleteMessages: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var results []map[string]any
	if err := json.Unmarshal([]byte(body), &results); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d entries, want 3 (chunks of 2,2,1)", len(results))
	}
	seen := map[string]bool{}
	for _, r := range results {
		if r["success"] != true {
			t.Errorf("chunk success = %v, want true: %+v", r["success"], r)
		}
		chunkIDs, _ := r["entityIds"].([]any)
		for _, id := range chunkIDs {
			s, _ := id.(string)
			seen[s] = true
		}
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("expected id %s to appear across the batched response, got %+v", id, results)
		}
	}
	for _, id := range ids {
		if r := doAuth(t, http.MethodGet, "/api/message/"+id, ""); r.StatusCode != http.StatusNotFound {
			t.Errorf("message %s should be gone after batched delete, got %d", id, r.StatusCode)
		}
	}
}

// --- Cell 7: absent params unchanged (regression pin) ---

func TestTransactionControl_AbsentParams_UnchangedShapes(t *testing.T) {
	const model = "e2e-txctl-noparam"
	importModelWithSample(t, model, 1, `{"n":0}`)
	lockModelE2E(t, model, 1)

	// Create without transactionTimeoutMillis: unchanged single-object-array shape.
	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), `{"n":1}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create without params: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var createResults []map[string]any
	if err := json.Unmarshal([]byte(body), &createResults); err != nil {
		t.Fatalf("decode create response: %v; body: %s", err, body)
	}
	if len(createResults) != 1 {
		t.Fatalf("create response = %d elements, want 1", len(createResults))
	}
	if _, ok := createResults[0]["transactionId"]; !ok {
		t.Errorf("create response missing transactionId: %+v", createResults[0])
	}

	createEntityE2E(t, model, 1, `{"n":2}`)
	createEntityE2E(t, model, 1, `{"n":3}`)

	// deleteEntities without transactionSize: single-transaction shape.
	cond := `{"type":"simple","jsonPath":"$.n","operatorType":"GREATER_OR_EQUAL","value":2}`
	delResp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1?verbose=true", model), cond)
	delBody := readBody(t, delResp)
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete without params: expected 200, got %d: %s", delResp.StatusCode, delBody)
	}
	var delObj map[string]any
	if err := json.Unmarshal([]byte(delBody), &delObj); err != nil {
		t.Fatalf("decode delete response: %v; body: %s", err, delBody)
	}
	dr, _ := delObj["deleteResult"].(map[string]any)
	if got := dr["numberOfEntititesRemoved"]; got != float64(2) {
		t.Errorf("numberOfEntititesRemoved = %v, want 2", got)
	}

	// deleteMessages without transactionSize: single-element response shape.
	msgID := createMessageE2E(t, "e2e-txctl-noparam-msg", `{"x":1}`)
	idsJSON, err := json.Marshal([]string{msgID})
	if err != nil {
		t.Fatalf("marshal ids: %v", err)
	}
	msgResp := doAuth(t, http.MethodDelete, "/api/message", string(idsJSON))
	msgBody := readBody(t, msgResp)
	if msgResp.StatusCode != http.StatusOK {
		t.Fatalf("deleteMessages without params: expected 200, got %d: %s", msgResp.StatusCode, msgBody)
	}
	var msgResults []map[string]any
	if err := json.Unmarshal([]byte(msgBody), &msgResults); err != nil {
		t.Fatalf("decode deleteMessages response: %v; body: %s", err, msgBody)
	}
	if len(msgResults) != 1 {
		t.Fatalf("deleteMessages response = %d elements, want 1 (no-batching shape)", len(msgResults))
	}
	if msgResults[0]["success"] != true {
		t.Errorf("deleteMessages success = %v, want true", msgResults[0]["success"])
	}
}

// --- Cell 8: search timeoutMillis (400 + happy path) ---

func TestTransactionControl_Search_TimeoutParam(t *testing.T) {
	const model = "e2e-txctl-searchtimeout"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)

	cond := `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_OR_EQUAL","value":0}`

	// 400: timeoutMillis=0
	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/%s/1?timeoutMillis=0", model), cond)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("timeoutMillis=0: expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "BAD_REQUEST")

	// Happy path: timeoutMillis=60000 -> 200 ndjson.
	resp2 := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/%s/1?timeoutMillis=60000", model), cond)
	body2 := readBody(t, resp2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("timeoutMillis=60000: expected 200, got %d: %s", resp2.StatusCode, body2)
	}
	if ct := resp2.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
	}
	lines := 0
	for _, line := range strings.Split(strings.TrimRight(body2, "\n"), "\n") {
		if line == "" {
			continue
		}
		lines++
	}
	if lines == 0 {
		t.Errorf("expected at least one ndjson result line, got body: %s", body2)
	}
}

// --- Cell 9: newMessage transactionTimeoutMillis (400 + happy path) ---

func TestTransactionControl_NewMessage_TimeoutParam(t *testing.T) {
	// 400: transactionTimeoutMillis=0
	resp := doAuth(t, http.MethodPost, "/api/message/new/e2e-txctl-newmsg-timeout?transactionTimeoutMillis=0", `{"payload":{"x":1}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("transactionTimeoutMillis=0: expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "BAD_REQUEST")

	// Happy path: transactionTimeoutMillis=5000 -> 200.
	resp2 := doAuth(t, http.MethodPost, "/api/message/new/e2e-txctl-newmsg-timeout?transactionTimeoutMillis=5000", `{"payload":{"x":1}}`)
	body2 := readBody(t, resp2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("transactionTimeoutMillis=5000: expected 200, got %d: %s", resp2.StatusCode, body2)
	}
	var results []map[string]any
	if err := json.Unmarshal([]byte(body2), &results); err != nil {
		t.Fatalf("decode newMessage response: %v; body: %s", err, body2)
	}
	if len(results) != 1 {
		t.Fatalf("newMessage response = %d elements, want 1", len(results))
	}
	ids, _ := results[0]["entityIds"].([]any)
	if len(ids) != 1 {
		t.Fatalf("newMessage entityIds = %v, want 1 entry", ids)
	}
}
