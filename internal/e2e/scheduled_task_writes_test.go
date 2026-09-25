package e2e_test

import (
	"fmt"
	"net/http"
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
