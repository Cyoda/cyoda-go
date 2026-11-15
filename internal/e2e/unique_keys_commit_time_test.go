package e2e_test

// unique_keys_commit_time_test.go — unique keys and the model's state are
// checked when the transaction commits, on its final entities, with the
// stored model. Through the full HTTP and gRPC stack on postgres:
//
//   - a joined callback's write whose entity duplicates a key, or whose
//     processors' returned data leaves a partial key, is answered 2xx; the
//     owner's commit is refused (409 UNIQUE_VIOLATION / 422
//     INVALID_UNIQUE_KEY);
//   - a write whose model is unlocked during the callout (an unjoined unlock;
//     the uncommitted entity does not count) is refused at commit. The
//     transaction's snapshot predates the unlock, so postgres answers a
//     retryable 409 CONFLICT, and the retry 409 MODEL_NOT_LOCKED.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// ukctKeyedModel imports model from workflowSampleModel, declares keysJSON on
// it, locks it and imports workflow wf.
func ukctKeyedModel(t *testing.T, h *callbackHarness, model, keysJSON, wf string) {
	t.Helper()
	resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model), workflowSampleModel, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import model %s: %d %s", model, resp.StatusCode, body)
	}
	resp = h.DoAuth(t, http.MethodPut, fmt.Sprintf("/api/model/%s/1/unique-keys", model), keysJSON, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("unique keys of %s: %d %s", model, resp.StatusCode, body)
	}
	resp = h.DoAuth(t, http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", model), "", "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock model %s: %d %s", model, resp.StatusCode, body)
	}
	resp = h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), wf, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import workflow %s: %d %s", model, resp.StatusCode, body)
	}
}

// ukctNameKey is a unique key on $.name.
const ukctNameKey = `{"uniqueKeys":[{"id":"name-key","fields":["$.name"]}]}`

// ukctDup is the key value the duplicate tests collide on.
const ukctDup = `{"name":"dup","amount":1,"status":"draft"}`

// requireModelState fails unless model's state is want.
func (h *callbackHarness) requireModelState(t *testing.T, model, want string) {
	t.Helper()
	var list []struct {
		ModelName    string `json:"modelName"`
		CurrentState string `json:"currentState"`
	}
	resp := h.DoAuth(t, http.MethodGet, "/api/model/", "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/model/: %d %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode model list: %v (body: %s)", err, body)
	}
	for _, m := range list {
		if m.ModelName == model {
			if m.CurrentState != want {
				t.Errorf("model %s is %s; want %s", model, m.CurrentState, want)
			}
			return
		}
	}
	t.Fatalf("model %s not listed: %s", model, body)
}

// joinedDuplicate runs the joined-callback duplicate scenario: entity X of
// target K holds name dup; an owner create's SYNC processor writes, joined
// through write, a second K entity named dup and records what write answered.
// It returns the owner's answer and the write's record.
func joinedDuplicate[R any](t *testing.T, prefix string, write func(rc *reqCtx, model string) (R, error)) (h *callbackHarness, target, owner string, status int, body string, res R) {
	t.Helper()
	h = newCallbackHarness(t)
	proc := prefix + "-proc"
	target, owner = uniq(prefix+"-target"), uniq(prefix+"-owner")
	type record struct {
		res R
		err error
	}
	done := make(chan record, 1)
	h.RegisterProc(proc, func(rc *reqCtx) (map[string]any, error) {
		var r record
		r.res, r.err = write(rc, target)
		done <- r
		return nil, nil
	})
	ukctKeyedModel(t, h, target, ukctNameKey, chainWorkflowJSON(prefix+"-target-wf"))
	h.SetupModelWithWorkflow(t, owner, syncProcWorkflow(proc))

	if _, st, b := h.CreateEntity(t, target, 1, ukctDup); st != http.StatusOK {
		t.Fatalf("create X: %d %s", st, b)
	}
	_, status, body = h.CreateEntity(t, owner, 1, workflowSampleModel)
	r := awaitOutcome(t, done)
	if r.err != nil {
		t.Fatalf("joined write: transport error: %v", r.err)
	}
	return h, target, owner, status, body, r.res
}

// TestUniqueKeys_JoinedCallbackDuplicate_OwnerCommit409: a joined HTTP create
// duplicates a committed key value. The callback is answered 200; the owner's
// commit is refused 409 UNIQUE_VIOLATION, and nothing it wrote is stored.
func TestUniqueKeys_JoinedCallbackDuplicate_OwnerCommit409(t *testing.T) {
	h, target, owner, status, body, cb := joinedDuplicate(t, "ukct-dup", func(rc *reqCtx, model string) (callbackResult, error) {
		return rc.CreateEntity(model, 1, ukctDup)
	})
	if cb.StatusCode != http.StatusOK {
		t.Errorf("joined create of a duplicate: %d %s; want 200 (keys are checked at the owner's commit)", cb.StatusCode, cb.Body)
	}
	assertProblem(t, status, body, http.StatusConflict, "UNIQUE_VIOLATION", false)
	if n := h.countEntities(t, target); n != 1 {
		t.Errorf("target has %d entities; want 1 (only X)", n)
	}
	if n := h.countEntities(t, owner); n != 0 {
		t.Errorf("owner model has %d entities; want 0", n)
	}
}

// TestUniqueKeys_JoinedCallbackDuplicate_GRPCTxToken_OwnerCommit409 is the
// same scenario with the joined create sent over gRPC with the tx-token: the
// envelope reports success; the owner's commit is refused 409
// UNIQUE_VIOLATION.
func TestUniqueKeys_JoinedCallbackDuplicate_GRPCTxToken_OwnerCommit409(t *testing.T) {
	h, target, owner, status, body, env := joinedDuplicate(t, "ukct-dup-grpc", func(rc *reqCtx, model string) (txEnvelope, error) {
		return rc.h.createEntityGRPCJoined(model, 1, ukctDup, rc.token)
	})
	if !env.Success {
		t.Errorf("joined gRPC create of a duplicate: %s; want success (keys are checked at the owner's commit)", grpcJoinedOutcome(env, nil).detail)
	}
	assertProblem(t, status, body, http.StatusConflict, "UNIQUE_VIOLATION", false)
	if n := h.countEntities(t, target); n != 1 {
		t.Errorf("target has %d entities; want 1 (only X)", n)
	}
	if n := h.countEntities(t, owner); n != 0 {
		t.Errorf("owner model has %d entities; want 0", n)
	}
}

// TestUniqueKeys_CallbackProcessorPartialKey_OwnerCommit422: a joined create
// whose body states the whole composite key runs a processor that returns
// data without one of the key's fields. The callback is answered 200; the
// owner's commit is refused 422 INVALID_UNIQUE_KEY, and nothing is stored.
func TestUniqueKeys_CallbackProcessorPartialKey_OwnerCommit422(t *testing.T) {
	h := newCallbackHarness(t)
	const driver, partial = "ukct-partial-driver", "ukct-partial-proc"
	target, owner := uniq("ukct-partial-target"), uniq("ukct-partial-owner")

	h.RegisterProc(partial, func(rc *reqCtx) (map[string]any, error) {
		return map[string]any{"name": "p", "status": "draft"}, nil
	})
	type record struct {
		res callbackResult
		err error
	}
	done := make(chan record, 1)
	h.RegisterProc(driver, func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.CreateEntity(target, 1, `{"name":"p","amount":1,"status":"draft"}`)
		done <- record{res, err}
		return nil, nil
	})
	ukctKeyedModel(t, h, target, `{"uniqueKeys":[{"id":"name-amount","fields":["$.name","$.amount"]}]}`, syncProcWorkflow(partial))
	h.SetupModelWithWorkflow(t, owner, syncProcWorkflow(driver))

	_, status, body := h.CreateEntity(t, owner, 1, workflowSampleModel)
	r := awaitOutcome(t, done)
	if r.err != nil || r.res.StatusCode != http.StatusOK {
		t.Errorf("joined create: err=%v %d %s; want 200 (keys are checked at the owner's commit)", r.err, r.res.StatusCode, r.res.Body)
	}
	assertProblem(t, status, body, http.StatusUnprocessableEntity, "INVALID_UNIQUE_KEY", false)
	if n := h.countEntities(t, target); n != 0 {
		t.Errorf("target has %d entities; want 0", n)
	}
	if n := h.countEntities(t, owner); n != 0 {
		t.Errorf("owner model has %d entities; want 0", n)
	}
}

// unlockingModel sets up model with a SYNC processor that unlocks it with an
// unjoined call during the callout and returns no data. The first unlock's
// answer is sent on the returned channel.
func unlockingModel(t *testing.T, h *callbackHarness, prefix string) (model string, unlocked <-chan unlockRecord) {
	t.Helper()
	proc := prefix + "-proc"
	model = uniq(prefix)
	ch := make(chan unlockRecord, 1)
	h.RegisterProc(proc, func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.h.unlockUnjoined(model)
		select {
		case ch <- unlockRecord{res, err}:
		default:
		}
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, model, syncProcWorkflow(proc))
	return model, ch
}

// requireNothingStored fails unless model is UNLOCKED with no entity.
func requireNothingStored(t *testing.T, h *callbackHarness, model string) {
	t.Helper()
	if n := h.countEntities(t, model); n != 0 {
		t.Errorf("model %s has %d entities; want 0", model, n)
	}
	h.requireModelState(t, model, "UNLOCKED")
}

// TestModelNotLockedAtCommit_Create: the create's processor unlocks the
// model. The commit is refused 409 CONFLICT (retryable); the retry 409
// MODEL_NOT_LOCKED.
func TestModelNotLockedAtCommit_Create(t *testing.T) {
	h := newCallbackHarness(t)
	model, unlocked := unlockingModel(t, h, "ukct-nl-create")

	_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	u := awaitOutcome(t, unlocked)
	requireUnlocked(t, u.res, u.err)
	assertProblem(t, status, body, http.StatusConflict, "CONFLICT", true)

	_, status, body = h.CreateEntity(t, model, 1, workflowSampleModel)
	assertProblem(t, status, body, http.StatusConflict, notLockedCode, false)
	requireNothingStored(t, h, model)
}

// TestModelNotLockedAtCommit_CollectionCreate is the create scenario on the
// collection create door (POST /api/entity/{format}).
func TestModelNotLockedAtCommit_CollectionCreate(t *testing.T) {
	h := newCallbackHarness(t)
	model, unlocked := unlockingModel(t, h, "ukct-nl-ccreate")
	h.token(t)

	res, err := h.callback(http.MethodPost, "/api/entity/JSON", collectionCreateItems(model, workflowSampleModel), "")
	if err != nil {
		t.Fatalf("collection create: %v", err)
	}
	u := awaitOutcome(t, unlocked)
	requireUnlocked(t, u.res, u.err)
	assertProblem(t, res.StatusCode, res.Body, http.StatusConflict, "CONFLICT", true)

	if res, err = h.callback(http.MethodPost, "/api/entity/JSON", collectionCreateItems(model, workflowSampleModel), ""); err != nil {
		t.Fatalf("collection create retry: %v", err)
	}
	assertProblem(t, res.StatusCode, res.Body, http.StatusConflict, notLockedCode, false)
	requireNothingStored(t, h, model)
}

// TestModelNotLockedAtCommit_GRPCCreate is the create scenario on the gRPC
// door: the envelope reports a retryable CONFLICT, the retry's
// MODEL_NOT_LOCKED.
func TestModelNotLockedAtCommit_GRPCCreate(t *testing.T) {
	h := newCallbackHarness(t)
	model, unlocked := unlockingModel(t, h, "ukct-nl-grpc")
	h.token(t)

	env, err := h.createEntityGRPC(model, 1, workflowSampleModel)
	u := awaitOutcome(t, unlocked)
	requireUnlocked(t, u.res, u.err)
	assertEnvelope(t, "gRPC create", env, err, "CONFLICT", true)

	env, err = h.createEntityGRPC(model, 1, workflowSampleModel)
	requireNotLockedEnvelope(t, "gRPC create retry", env, err)
	requireNothingStored(t, h, model)
}

// TestModelNotLockedAtCommit_GRPCCollectionCreate is the create scenario on
// the gRPC collection create door (EntityManageCollection).
func TestModelNotLockedAtCommit_GRPCCollectionCreate(t *testing.T) {
	h := newCallbackHarness(t)
	model, unlocked := unlockingModel(t, h, "ukct-nl-grpc-ccreate")
	h.token(t)

	create := func() (txEnvelope, error) {
		return h.joinedManageCollectionGRPC(internalgrpc.EntityCreateCollectionRequest, map[string]any{
			"id": "ukct-nl-grpc-ccreate", "dataFormat": "JSON",
			"payloads": []any{map[string]any{
				"model": map[string]any{"name": model, "version": 1},
				"data":  mustJSONMap(workflowSampleModel),
			}},
		}, "")
	}
	env, err := create()
	u := awaitOutcome(t, unlocked)
	requireUnlocked(t, u.res, u.err)
	assertEnvelope(t, "gRPC collection create", env, err, "CONFLICT", true)

	env, err = create()
	requireNotLockedEnvelope(t, "gRPC collection create retry", env, err)
	requireNothingStored(t, h, model)
}

// TestModelNotLockedAtCommit_UpdateViaCallbackCreate: an update of a
// committed entity E runs a SYNC processor that creates, through a joined
// callback, an entity of a second model M2, and then unlocks M2 (unjoined;
// the joined entity is not committed, so M2 has none). The commit finds M2
// not LOCKED: 409 CONFLICT (retryable; the transaction's snapshot predates
// the unlock), and nothing is committed — E keeps its data and state,
// and M2 is UNLOCKED with no entity.
func TestModelNotLockedAtCommit_UpdateViaCallbackCreate(t *testing.T) {
	h := newCallbackHarness(t)
	const proc = "ukct-nl-update-proc"
	owner, m2 := uniq("ukct-nl-update-owner"), uniq("ukct-nl-update-m2")

	type record struct {
		created callbackResult
		cErr    error
		unlock  unlockRecord
	}
	done := make(chan record, 1)
	h.RegisterProc(proc, func(rc *reqCtx) (map[string]any, error) {
		var r record
		r.created, r.cErr = rc.CreateEntity(m2, 1, workflowSampleModel)
		if r.cErr == nil && r.created.StatusCode == http.StatusOK {
			r.unlock.res, r.unlock.err = rc.h.unlockUnjoined(m2)
		}
		done <- r
		return nil, nil
	})
	// E rests in OPEN until an update sets status go, which takes the
	// automated transition that runs the processor.
	h.SetupModelWithWorkflow(t, owner, workflowDocJSON(proc+"-wf", map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{"name": "open", "next": "OPEN", "manual": false}}},
		"OPEN": map[string]any{"transitions": []any{map[string]any{
			"name": "go", "next": "DONE", "manual": false,
			"criterion":  map[string]any{"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "go"},
			"processors": []any{map[string]any{"type": "externalized", "name": proc, "executionMode": "SYNC", "config": map[string]any{"attachEntity": true, "calculationNodesTags": ""}}},
		}}},
		"DONE": map[string]any{},
	}))
	h.SetupModelWithWorkflow(t, m2, chainWorkflowJSON(m2+"-wf"))

	e, status, body := h.CreateEntity(t, owner, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("create E: %d %s", status, body)
	}

	res, err := h.callback(http.MethodPut, "/api/entity/JSON/"+e, `{"name":"Test Order","amount":100,"status":"go"}`, "")
	if err != nil {
		t.Fatalf("update E: %v", err)
	}
	r := awaitOutcome(t, done)
	if r.cErr != nil || r.created.StatusCode != http.StatusOK {
		t.Fatalf("joined create in M2: err=%v %d %s", r.cErr, r.created.StatusCode, r.created.Body)
	}
	requireUnlocked(t, r.unlock.res, r.unlock.err)
	// The transaction's snapshot predates the unlock: a retryable conflict.
	assertProblem(t, res.StatusCode, res.Body, http.StatusConflict, "CONFLICT", true)

	if state, _ := h.GetEntityState(t, e); state != "OPEN" {
		t.Errorf("E is %s; want OPEN (the update is not committed)", state)
	}
	if got := h.GetEntityData(t, e)["status"]; got != "draft" {
		t.Errorf("E has status %v; want draft (the update is not committed)", got)
	}
	requireNothingStored(t, h, m2)
}
