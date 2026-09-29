package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func requireConflictProblem(t *testing.T, res httpResult) {
	t.Helper()
	if res.err != nil {
		t.Fatalf("request: %v", res.err)
	}
	if res.status != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", res.status, res.body)
	}
	pd := decodeProblem(t, res.body)
	if pd.errorCode() != common.ErrCodeConflict || !pd.retryable() {
		t.Fatalf("errorCode=%q retryable=%v, want CONFLICT retryable; body: %s", pd.errorCode(), pd.retryable(), res.body)
	}
}

func TestScheduledTaskWrites_DeleteEntity_RemovesItsTasks(t *testing.T) {
	const model = "e2e-stw-delete-one"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	other := createEntityE2E(t, model, 1, schedWritesPayload)
	if n := taskRows(t, "entity_id = $1", id); n != 1 {
		t.Fatalf("task rows before the delete = %d, want 1", n)
	}

	res := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodDelete, "/api/entity/"+id, ""))
	if res.status != http.StatusOK {
		t.Fatalf("delete: %d %s", res.status, res.body)
	}
	if n := taskRows(t, "entity_id = $1", id); n != 0 {
		t.Errorf("task rows of the deleted entity = %d, want 0", n)
	}
	if n := taskRows(t, "entity_id = $1", other); n != 1 {
		t.Errorf("task rows of the other entity = %d, want 1", n)
	}
}

func TestScheduledTaskWrites_DeleteEntity_RacingOneClaim_SucceedsAfterRetry(t *testing.T) {
	const model = "e2e-stw-delete-race"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	hold := holdTaskRows(t, "entity_id = $1", id)

	ctx := e2eCtx(t)
	done := make(chan httpResult, 1)
	go func() { done <- resultOf(doAuthOnceRaw(ctx, http.MethodDelete, "/api/entity/"+id, "")) }()
	hold.awaitBlocked(t)
	hold.claimAndCommit(t)

	res := <-done
	if res.status != http.StatusOK {
		t.Fatalf("delete racing a claim: %d %s, want 200 after the server's retry", res.status, res.body)
	}
	if n := taskRows(t, "entity_id = $1", id); n != 0 {
		t.Errorf("task rows = %d, want 0", n)
	}
	if st := resultOf(doAuthOnceRaw(ctx, http.MethodGet, "/api/entity/"+id, "")).status; st != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", st)
	}
}

func TestScheduledTaskWrites_DeleteEntity_PersistentConflict_409(t *testing.T) {
	const model = "e2e-stw-delete-persist"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	trig := installConflictTrigger(t, "entity_id", id)

	requireConflictProblem(t, resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodDelete, "/api/entity/"+id, "")))
	if got, want := trig.refusals(t), int64(1+common.TaskConflictRetries); got != want {
		t.Errorf("server attempts = %d, want %d", got, want)
	}
	if n := taskRows(t, "entity_id = $1", id); n != 1 {
		t.Errorf("task rows = %d, want 1", n)
	}
	if st := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodGet, "/api/entity/"+id, "")).status; st != http.StatusOK {
		t.Errorf("GET after the refused delete = %d, want 200", st)
	}
}

// An update has no server retry. Racing a claim, it answers 409 CONFLICT,
// and the retried update re-arms the task as a new life.
func TestScheduledTaskWrites_UpdateRacingOneClaim_409ThenRetrySucceeds(t *testing.T) {
	const model = "e2e-stw-update-race"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	hold := holdTaskRows(t, "entity_id = $1", id)

	ctx := e2eCtx(t)
	path := "/api/entity/JSON/" + id
	done := make(chan httpResult, 1)
	go func() {
		done <- resultOf(doAuthOnceRaw(ctx, http.MethodPut, path, `{"name":"Order","amount":101,"status":"draft"}`))
	}()
	hold.awaitBlocked(t)
	hold.claimAndCommit(t)
	requireConflictProblem(t, <-done)

	retry := resultOf(doAuthOnceRaw(ctx, http.MethodPut, path, `{"name":"Order","amount":101,"status":"draft"}`))
	if retry.status != http.StatusOK {
		t.Fatalf("retried update: %d %s", retry.status, retry.body)
	}
	if n := taskRows(t, "entity_id = $1 AND status = 'WAITING' AND claim_token IS NULL", id); n != 1 {
		t.Errorf("WAITING unclaimed task rows = %d, want 1 (the retry re-armed the task)", n)
	}
}

// A delete that joined another request's transaction is not retried on the
// server. Its conflict reaches the joined caller, and the owner's
// transaction does not commit.
func TestScheduledTaskWrites_DeleteEntity_Joined_NotRetried(t *testing.T) {
	h := newCallbackHarness(t)
	const target = "e2e-stw-joined-target"
	const primary = "e2e-stw-joined-primary"
	h.SetupModelWithWorkflow(t, target, schedWritesWorkflow)
	targetID, status, body := h.CreateEntity(t, target, 1, schedWritesPayload)
	if status != http.StatusOK {
		t.Fatalf("create target: %d %s", status, body)
	}

	var (
		mu       sync.Mutex
		callback callbackResult
	)
	h.RegisterProc("stw-delete-target", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.h.callback(http.MethodDelete, "/api/entity/"+targetID, "", rc.token)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		callback = res
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("joined delete: %d", res.StatusCode)
		}
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "stw-joined-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "DONE", "manual": false,
					"processors": [{"type": "calculator", "name": "stw-delete-target", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]}]},
				"DONE": {}
			}
		}]
	}`)

	hold := holdTaskRows(t, "entity_id = $1", targetID)
	h.token(t)
	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(primary, 1, schedWritesPayload) }()
	hold.awaitBlocked(t)
	hold.claimAndCommit(t)

	var outer createEntityResult
	select {
	case outer = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("primary create did not finish")
	}
	mu.Lock()
	got := callback
	mu.Unlock()
	if got.StatusCode != http.StatusConflict {
		t.Fatalf("joined delete answered %d %s, want 409", got.StatusCode, got.Body)
	}
	if pd := decodeProblem(t, got.Body); pd.errorCode() != common.ErrCodeConflict || !pd.retryable() {
		t.Errorf("joined delete errorCode=%q retryable=%v, want CONFLICT retryable", pd.errorCode(), pd.retryable())
	}
	if outer.status == http.StatusOK {
		t.Errorf("primary create = 200, want a failure: its transaction met the conflict")
	}
	if _, code := h.GetEntityState(t, targetID); code != http.StatusOK {
		t.Errorf("target GET = %d, want 200: nothing the joined delete did may commit", code)
	}
	if n := taskRows(t, "entity_id = $1 AND status = 'RUNNING'", targetID); n != 1 {
		t.Errorf("target task rows = %d, want 1 (the test's claim, untouched)", n)
	}
}

func TestScheduledTaskWrites_DeleteAll_RemovesModelTasks_OtherTenantKept(t *testing.T) {
	const model = "e2e-stw-delete-all"
	setupScheduledModel(t, model)
	createEntityE2E(t, model, 1, schedWritesPayload)
	createEntityE2E(t, model, 1, schedWritesPayload)

	// Tenant B: same model name, its own entity and task.
	bID, bSecret := createM2MClient(t, "tenant-b-stw", "user-b", true)
	asB := func(method, path, body string) {
		t.Helper()
		var raw []byte
		if body != "" {
			raw = []byte(body)
		}
		resp := adminRequestAs(t, bID, bSecret, method, path, raw)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("tenant B %s %s: %d %s", method, path, resp.StatusCode, b)
		}
	}
	asB(http.MethodPost, fmt.Sprintf("/model/import/JSON/SAMPLE_DATA/%s/1", model), workflowSampleModel)
	asB(http.MethodPut, fmt.Sprintf("/model/%s/1/lock", model), "")
	asB(http.MethodPost, fmt.Sprintf("/model/%s/1/workflow/import", model), schedWritesWorkflow)
	asB(http.MethodPost, fmt.Sprintf("/entity/JSON/%s/1", model), schedWritesPayload)

	if n := taskRows(t, "tenant_id = $1 AND model_name = $2", "test-tenant", model); n != 2 {
		t.Fatalf("tenant A task rows before = %d, want 2", n)
	}
	res := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), ""))
	if res.status != http.StatusOK {
		t.Fatalf("delete-all: %d %s", res.status, res.body)
	}
	if n := taskRows(t, "tenant_id = $1 AND model_name = $2", "test-tenant", model); n != 0 {
		t.Errorf("tenant A task rows = %d, want 0", n)
	}
	if n := taskRows(t, "tenant_id = $1 AND model_name = $2", "tenant-b-stw", model); n != 1 {
		t.Errorf("tenant B task rows = %d, want 1: a delete never reaches another tenant", n)
	}
}

func TestScheduledTaskWrites_DeleteAll_RacingOneClaim_SucceedsAfterRetry(t *testing.T) {
	const model = "e2e-stw-delete-all-race"
	setupScheduledModel(t, model)
	createEntityE2E(t, model, 1, schedWritesPayload)
	hold := holdTaskRows(t, "tenant_id = 'test-tenant' AND model_name = $1", model)

	ctx := e2eCtx(t)
	done := make(chan httpResult, 1)
	go func() {
		done <- resultOf(doAuthOnceRaw(ctx, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), ""))
	}()
	hold.awaitBlocked(t)
	hold.claimAndCommit(t)

	if res := <-done; res.status != http.StatusOK {
		t.Fatalf("delete-all racing a claim: %d %s, want 200", res.status, res.body)
	}
	if n := taskRows(t, "tenant_id = 'test-tenant' AND model_name = $1", model); n != 0 {
		t.Errorf("task rows = %d, want 0", n)
	}
}

func TestScheduledTaskWrites_DeleteAll_PersistentConflict_409(t *testing.T) {
	const model = "e2e-stw-delete-all-persist"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	trig := installConflictTrigger(t, "model_name", model)

	requireConflictProblem(t, resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), "")))
	if got, want := trig.refusals(t), int64(1+common.TaskConflictRetries); got != want {
		t.Errorf("server attempts = %d, want %d", got, want)
	}
	if st := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodGet, "/api/entity/"+id, "")).status; st != http.StatusOK {
		t.Errorf("GET after the refused delete-all = %d, want 200", st)
	}
}

const amountOver50 = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":50}`

func TestScheduledTaskWrites_ConditionalDelete_RemovesTheDeletedEntitiesTasks(t *testing.T) {
	const model = "e2e-stw-cond"
	setupScheduledModel(t, model)
	kept := createEntityE2E(t, model, 1, `{"name":"Order","amount":10,"status":"draft"}`)
	gone := createEntityE2E(t, model, 1, schedWritesPayload)

	res := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), amountOver50))
	if res.status != http.StatusOK {
		t.Fatalf("conditional delete: %d %s", res.status, res.body)
	}
	if n := taskRows(t, "entity_id = $1", gone); n != 0 {
		t.Errorf("task rows of the deleted entity = %d, want 0", n)
	}
	if n := taskRows(t, "entity_id = $1", kept); n != 1 {
		t.Errorf("task rows of the kept entity = %d, want 1", n)
	}
}

func TestScheduledTaskWrites_ConditionalDelete_RacingOneClaim_SucceedsAfterRetry(t *testing.T) {
	const model = "e2e-stw-cond-race"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	hold := holdTaskRows(t, "entity_id = $1", id)

	ctx := e2eCtx(t)
	done := make(chan httpResult, 1)
	go func() {
		done <- resultOf(doAuthOnceRaw(ctx, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), amountOver50))
	}()
	hold.awaitBlocked(t)
	hold.claimAndCommit(t)

	if res := <-done; res.status != http.StatusOK {
		t.Fatalf("conditional delete racing a claim: %d %s, want 200", res.status, res.body)
	}
	if n := taskRows(t, "entity_id = $1", id); n != 0 {
		t.Errorf("task rows = %d, want 0", n)
	}
}

func TestScheduledTaskWrites_ConditionalDelete_PersistentConflict_409(t *testing.T) {
	const model = "e2e-stw-cond-persist"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	trig := installConflictTrigger(t, "entity_id", id)

	requireConflictProblem(t, resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), amountOver50)))
	if got, want := trig.refusals(t), int64(1+common.TaskConflictRetries); got != want {
		t.Errorf("server attempts = %d, want %d", got, want)
	}
}

type batchedDeleteBody struct {
	DeleteResult struct {
		IDToError map[string]string `json:"idToError"`
		Removed   int               `json:"numberOfEntititesRemoved"`
	} `json:"deleteResult"`
}

func decodeBatched(t *testing.T, body string) batchedDeleteBody {
	t.Helper()
	var b batchedDeleteBody
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatalf("decode delete result: %v; body: %s", err, body)
	}
	return b
}

func TestScheduledTaskWrites_BatchedDelete_RacingOneClaim_BatchSucceedsAfterRetry(t *testing.T) {
	const model = "e2e-stw-batched-race"
	setupScheduledModel(t, model)
	id := createEntityE2E(t, model, 1, schedWritesPayload)
	hold := holdTaskRows(t, "entity_id = $1", id)

	ctx := e2eCtx(t)
	done := make(chan httpResult, 1)
	go func() {
		done <- resultOf(doAuthOnceRaw(ctx, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1?transactionSize=1", model), amountOver50))
	}()
	hold.awaitBlocked(t)
	hold.claimAndCommit(t)

	res := <-done
	if res.status != http.StatusOK {
		t.Fatalf("batched delete racing a claim: %d %s", res.status, res.body)
	}
	if b := decodeBatched(t, res.body); b.DeleteResult.Removed != 1 || len(b.DeleteResult.IDToError) != 0 {
		t.Errorf("removed=%d idToError=%v, want 1 and none", b.DeleteResult.Removed, b.DeleteResult.IDToError)
	}
	if n := taskRows(t, "entity_id = $1", id); n != 0 {
		t.Errorf("task rows = %d, want 0", n)
	}
}

func TestScheduledTaskWrites_BatchedDelete_PersistentConflict_PerIDNot409(t *testing.T) {
	const model = "e2e-stw-batched-persist"
	setupScheduledModel(t, model)
	stuck := createEntityE2E(t, model, 1, schedWritesPayload)
	free := createEntityE2E(t, model, 1, schedWritesPayload)
	trig := installConflictTrigger(t, "entity_id", stuck)

	res := resultOf(doAuthOnceRaw(e2eCtx(t), http.MethodDelete, fmt.Sprintf("/api/entity/%s/1?transactionSize=1", model), amountOver50))
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200: a batched delete never answers 409 for a batch; body: %s", res.status, res.body)
	}
	b := decodeBatched(t, res.body)
	if msg := b.DeleteResult.IDToError[stuck]; !strings.HasPrefix(msg, common.ErrCodeConflict+":") {
		t.Errorf("idToError[stuck] = %q, want a CONFLICT entry", msg)
	}
	if b.DeleteResult.Removed != 1 {
		t.Errorf("removed = %d, want 1 (the other batch ran)", b.DeleteResult.Removed)
	}
	if got, want := trig.refusals(t), int64(1+common.TaskConflictRetries); got != want {
		t.Errorf("attempts of the stuck batch = %d, want %d", got, want)
	}
	if n := taskRows(t, "entity_id = $1", free); n != 0 {
		t.Errorf("task rows of the deleted entity = %d, want 0", n)
	}
	if n := taskRows(t, "entity_id = $1", stuck); n != 1 {
		t.Errorf("task rows of the stuck entity = %d, want 1", n)
	}
}
