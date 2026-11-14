package e2e_test

// schema_extension_not_locked_test.go — a schema extension reaches only a
// LOCKED model. An extension that finds its model not LOCKED — unlocked while
// a write to it, or the transaction the write joined, is in progress — is
// refused: 409 MODEL_NOT_LOCKED, and the model keeps its schema.
//
// The compute node orders that: its processor unlocks the model during the
// callout, with an unjoined request (a joined one is refused as
// administration inside a transaction). The unlock succeeds because the
// model's only entity is not committed yet, and an unlock counts committed
// entities. Then the processor, or a joined write it makes, brings a field
// the model does not have.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

const notLockedCode = "MODEL_NOT_LOCKED"

// notLockedModel sets up model on h, LOCKED and STRUCTURAL, with workflow wf.
// The name is chosen before the processors that use it are registered, so the
// member reads it after the registration's lock.
func notLockedModel(t *testing.T, h *callbackHarness, model, wf string) {
	t.Helper()
	h.SetupModelWithWorkflow(t, model, wf)
	resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/changeLevel/STRUCTURAL", model), "", "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("set changeLevel of %s: %d %s", model, resp.StatusCode, body)
	}
}

// unlockUnjoined unlocks model without a transaction token, as an ordinary
// administration request made while the callout is in progress.
func (h *callbackHarness) unlockUnjoined(model string) (callbackResult, error) {
	return h.callback(http.MethodPut, fmt.Sprintf("/api/model/%s/1/unlock", model), "", "")
}

// syncProcWorkflow is a NONE -> DONE workflow whose automated transition runs
// the member's processor proc, SYNC.
func syncProcWorkflow(proc string) string {
	return chainWorkflowJSON(proc+"-wf", procSpec{name: proc, mode: "SYNC", config: map[string]any{"calculationNodesTags": ""}})
}

// awaitOutcome waits for the processor's record of what it did.
func awaitOutcome[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(30 * time.Second):
		t.Fatal("timeout: the processor did not run")
	}
	var zero T
	return zero
}

// requireUnlocked fails the test unless the processor's unlock answered 200:
// otherwise the scenario under test never happened.
func requireUnlocked(t *testing.T, res callbackResult, err error) {
	t.Helper()
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("unjoined unlock during the callout: err=%v status=%d body=%s; want 200", err, res.StatusCode, res.Body)
	}
}

// requireNotLocked409 fails the test unless an HTTP answer is 409
// MODEL_NOT_LOCKED.
func requireNotLocked409(t *testing.T, what string, res callbackResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: transport error: %v", what, err)
	}
	out := httpJoinedOutcome(res, nil)
	if out.status != http.StatusConflict || out.errorCode != notLockedCode {
		t.Fatalf("%s: status=%d code=%q; want 409 %s: %s", what, out.status, out.errorCode, notLockedCode, res.Body)
	}
}

// requireSchemaLacks fails the test if model's export names field: the
// refused extension must not have reached the model.
func requireSchemaLacks(t *testing.T, h *callbackHarness, model, field string) {
	t.Helper()
	resp := h.DoAuth(t, http.MethodGet, fmt.Sprintf("/api/model/export/SIMPLE_VIEW/%s/1", model), "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("model export of %s: %d %s", model, resp.StatusCode, body)
	}
	if strings.Contains(body, field) {
		t.Errorf("model %s gained %q from a refused extension: %s", model, field, body)
	}
}

type unlockRecord struct {
	res callbackResult
	err error
}

// TestSchemaExtension_NotLocked_CreateProcessorOutput — the create door: the
// processor unlocks the model, then returns data with a new field.
func TestSchemaExtension_NotLocked_CreateProcessorOutput(t *testing.T) {
	h := newCallbackHarness(t)
	const proc = "se-nl-create"
	model := uniq("se-nl-create")
	const field = "nlCreateField"
	unlocked := make(chan unlockRecord, 1)
	h.RegisterProc(proc, func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.h.unlockUnjoined(model)
		unlocked <- unlockRecord{res, err}
		return map[string]any{"name": "Test Order", "amount": 100, "status": "draft", field: "v"}, nil
	})
	notLockedModel(t, h, model, syncProcWorkflow(proc))

	_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	u := awaitOutcome(t, unlocked)
	requireUnlocked(t, u.res, u.err)
	requireNotLocked409(t, "create", callbackResult{StatusCode: status, Body: body}, nil)
	requireSchemaLacks(t, h, model, field)
}

// TestSchemaExtension_NotLocked_CreateCollection — the collection create door
// (POST /api/entity/{format}): the processor unlocks the model, then returns
// data with a new field.
func TestSchemaExtension_NotLocked_CreateCollection(t *testing.T) {
	h := newCallbackHarness(t)
	const proc = "se-nl-ccreate"
	model := uniq("se-nl-ccreate")
	const field = "nlCollectionCreateField"
	unlocked := make(chan unlockRecord, 1)
	h.RegisterProc(proc, func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.h.unlockUnjoined(model)
		unlocked <- unlockRecord{res, err}
		return map[string]any{"name": "Test Order", "amount": 100, "status": "draft", field: "v"}, nil
	})
	notLockedModel(t, h, model, syncProcWorkflow(proc))

	// An empty token sends the request unjoined.
	res, err := h.callback(http.MethodPost, "/api/entity/JSON", collectionCreateItems(model, workflowSampleModel), "")
	u := awaitOutcome(t, unlocked)
	requireUnlocked(t, u.res, u.err)
	requireNotLocked409(t, "collection create", res, err)
	requireSchemaLacks(t, h, model, field)
}

// joinedNotLocked runs one joined write against an uncommitted entity of a
// model unlocked during the callout, and returns what the write answered: an
// HTTP answer or a gRPC envelope, whichever door write uses.
//
// An entity being created is not stored until its first transition ends, so
// a callback cannot write the owner's own entity during a create's callout;
// and an existing entity is committed, which stops the unlock. So the owner
// is an entity of a second model: its processor creates entity S of the
// target model (joined, so S is uncommitted), unlocks the target model
// (unjoined), and makes the joined write on S. The target's workflow takes S
// to OPEN; S's manual transition "go" runs the processor adder, which returns
// data with field.
func joinedNotLocked[R any](t *testing.T, prefix, field string, write func(rc *reqCtx, sID string) (R, error)) (h *callbackHarness, target string, res R, err error) {
	t.Helper()
	h = newCallbackHarness(t)
	driver, adder := prefix+"-driver", prefix+"-adder"
	target, owner := uniq(prefix+"-target"), uniq(prefix+"-owner")

	h.RegisterProc(adder, func(rc *reqCtx) (map[string]any, error) {
		return map[string]any{"name": "Test Order", "amount": 100, "status": "draft", field: "v"}, nil
	})
	type record struct {
		created callbackResult
		cErr    error
		unlock  unlockRecord
		res     R
		err     error
	}
	done := make(chan record, 1)
	h.RegisterProc(driver, func(rc *reqCtx) (map[string]any, error) {
		var r record
		r.created, r.cErr = rc.CreateEntity(target, 1, workflowSampleModel)
		if r.cErr == nil && r.created.StatusCode == http.StatusOK {
			r.unlock.res, r.unlock.err = rc.h.unlockUnjoined(target)
			r.res, r.err = write(rc, r.created.EntityID)
		}
		done <- r
		return nil, fmt.Errorf("the joined write was answered")
	})
	notLockedModel(t, h, target, openThenGoWorkflowJSON(prefix+"-target-wf", adder))
	notLockedModel(t, h, owner, syncProcWorkflow(driver))

	h.CreateEntity(t, owner, 1, workflowSampleModel)
	r := awaitOutcome(t, done)
	if r.cErr != nil || r.created.StatusCode != http.StatusOK || r.created.EntityID == "" {
		t.Fatalf("joined create of S: err=%v status=%d body=%s", r.cErr, r.created.StatusCode, r.created.Body)
	}
	requireUnlocked(t, r.unlock.res, r.unlock.err)
	return h, target, r.res, r.err
}

// withField is the sample entity with one more field.
func withField(field string) string {
	return fmt.Sprintf(`{"name":"Test Order","amount":100,"status":"draft",%q:"v"}`, field)
}

// TestSchemaExtension_NotLocked_JoinedUpdate — the update door: after the
// unlock, a joined PUT on the uncommitted entity brings a new field.
func TestSchemaExtension_NotLocked_JoinedUpdate(t *testing.T) {
	const field = "nlUpdateField"
	h, target, res, err := joinedNotLocked(t, "se-nl-update", field, func(rc *reqCtx, sID string) (callbackResult, error) {
		return rc.UpdateEntity(sID, withField(field))
	})
	requireNotLocked409(t, "joined PUT", res, err)
	requireSchemaLacks(t, h, target, field)
}

// TestSchemaExtension_NotLocked_JoinedCollectionUpdate — the collection
// update door: after the unlock, a joined collection update of the
// uncommitted entity brings a new field.
func TestSchemaExtension_NotLocked_JoinedCollectionUpdate(t *testing.T) {
	const field = "nlCollectionUpdateField"
	h, target, res, err := joinedNotLocked(t, "se-nl-cupdate", field, func(rc *reqCtx, sID string) (callbackResult, error) {
		return rc.h.callback(http.MethodPut, "/api/entity/JSON", collectionUpdateBody(sID, "", withField(field)), rc.token)
	})
	requireNotLocked409(t, "joined collection update", res, err)
	requireSchemaLacks(t, h, target, field)
}

// TestSchemaExtension_NotLocked_JoinedManualTransition — the transition door:
// after the unlock, a joined manual transition of the uncommitted entity runs
// a processor that returns data with a new field.
func TestSchemaExtension_NotLocked_JoinedManualTransition(t *testing.T) {
	const field = "nlTransitionField"
	h, target, res, err := joinedNotLocked(t, "se-nl-tr", field, func(rc *reqCtx, sID string) (callbackResult, error) {
		return rc.h.callback(http.MethodPut, "/api/entity/JSON/"+sID+"/go", workflowSampleModel, rc.token)
	})
	requireNotLocked409(t, "joined manual transition", res, err)
	requireSchemaLacks(t, h, target, field)
}

// TestGRPC_ModelNotLocked_Envelope is the create scenario on the gRPC door:
// the envelope reports a client error carrying MODEL_NOT_LOCKED.
func TestGRPC_ModelNotLocked_Envelope(t *testing.T) {
	h := newCallbackHarness(t)
	const proc = "se-nl-grpc"
	model := uniq("se-nl-grpc")
	const field = "nlGrpcField"
	unlocked := make(chan unlockRecord, 1)
	h.RegisterProc(proc, func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.h.unlockUnjoined(model)
		unlocked <- unlockRecord{res, err}
		return map[string]any{"name": "Test Order", "amount": 100, "status": "draft", field: "v"}, nil
	})
	notLockedModel(t, h, model, syncProcWorkflow(proc))

	env, err := h.createEntityGRPC(model, 1, workflowSampleModel)
	if err != nil {
		t.Fatalf("gRPC create: transport error: %v", err)
	}
	u := awaitOutcome(t, unlocked)
	requireUnlocked(t, u.res, u.err)
	requireNotLockedEnvelope(t, "gRPC create", env, nil)
	requireSchemaLacks(t, h, model, field)
}

// requireNotLockedEnvelope fails the test unless a gRPC envelope reports a
// client error carrying MODEL_NOT_LOCKED, not retryable.
func requireNotLockedEnvelope(t *testing.T, what string, env txEnvelope, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: transport error: %v", what, err)
	}
	if env.Success || env.Error == nil {
		raw, _ := json.Marshal(env)
		t.Fatalf("%s: envelope = %s; want success=false with an error", what, raw)
	}
	if env.Error.Code != "CLIENT_ERROR" || !strings.HasPrefix(env.Error.Message, notLockedCode+":") {
		t.Fatalf("%s: error = code %q message %q; want CLIENT_ERROR with a message prefixed %s:", what, env.Error.Code, env.Error.Message, notLockedCode)
	}
	if env.Error.Retryable != nil && *env.Error.Retryable {
		t.Errorf("%s: error is retryable; MODEL_NOT_LOCKED is not", what)
	}
}

// TestGRPC_ModelNotLocked_CreateCollection — the collection create door
// (EntityManageCollection): the processor unlocks the model, then returns
// data with a new field.
func TestGRPC_ModelNotLocked_CreateCollection(t *testing.T) {
	h := newCallbackHarness(t)
	const proc = "se-nl-grpc-ccreate"
	model := uniq("se-nl-grpc-ccreate")
	const field = "nlGrpcCollectionCreateField"
	unlocked := make(chan unlockRecord, 1)
	h.RegisterProc(proc, func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.h.unlockUnjoined(model)
		unlocked <- unlockRecord{res, err}
		return map[string]any{"name": "Test Order", "amount": 100, "status": "draft", field: "v"}, nil
	})
	notLockedModel(t, h, model, syncProcWorkflow(proc))

	// An empty pass sends the request unjoined.
	env, err := h.joinedManageCollectionGRPC(internalgrpc.EntityCreateCollectionRequest, map[string]any{
		"id": "se-nl-grpc-ccreate", "dataFormat": "JSON",
		"payloads": []any{map[string]any{
			"model": map[string]any{"name": model, "version": 1},
			"data":  mustJSONMap(workflowSampleModel),
		}},
	}, "")
	u := awaitOutcome(t, unlocked)
	requireUnlocked(t, u.res, u.err)
	requireNotLockedEnvelope(t, "gRPC collection create", env, err)
	requireSchemaLacks(t, h, model, field)
}

// TestGRPC_ModelNotLocked_JoinedUpdate — the update door (EntityUpdateRequest
// without a transition): after the unlock, a joined update of the uncommitted
// entity brings a new field.
func TestGRPC_ModelNotLocked_JoinedUpdate(t *testing.T) {
	const field = "nlGrpcUpdateField"
	h, target, env, err := joinedNotLocked(t, "se-nl-grpc-update", field, func(rc *reqCtx, sID string) (txEnvelope, error) {
		return rc.h.joinedManageGRPC(internalgrpc.EntityUpdateRequest, map[string]any{
			"id": "se-nl-grpc-update", "dataFormat": "JSON",
			"payload": map[string]any{"entityId": sID, "data": mustJSONMap(withField(field))},
		}, rc.token)
	})
	requireNotLockedEnvelope(t, "gRPC joined update", env, err)
	requireSchemaLacks(t, h, target, field)
}

// TestGRPC_ModelNotLocked_JoinedCollectionUpdate — the collection update door
// (EntityManageCollection): after the unlock, a joined collection update of
// the uncommitted entity brings a new field.
func TestGRPC_ModelNotLocked_JoinedCollectionUpdate(t *testing.T) {
	const field = "nlGrpcCollectionUpdateField"
	h, target, env, err := joinedNotLocked(t, "se-nl-grpc-cupdate", field, func(rc *reqCtx, sID string) (txEnvelope, error) {
		return rc.h.joinedManageCollectionGRPC(internalgrpc.EntityUpdateCollectionRequest, map[string]any{
			"id": "se-nl-grpc-cupdate", "dataFormat": "JSON",
			"payloads": []any{map[string]any{"entityId": sID, "data": mustJSONMap(withField(field))}},
		}, rc.token)
	})
	requireNotLockedEnvelope(t, "gRPC joined collection update", env, err)
	requireSchemaLacks(t, h, target, field)
}

// TestGRPC_ModelNotLocked_JoinedTransition — the transition door
// (EntityTransitionRequest): after the unlock, a joined manual transition of
// the uncommitted entity runs a processor that returns data with a new field.
func TestGRPC_ModelNotLocked_JoinedTransition(t *testing.T) {
	const field = "nlGrpcTransitionField"
	h, target, env, err := joinedNotLocked(t, "se-nl-grpc-tr", field, func(rc *reqCtx, sID string) (txEnvelope, error) {
		return rc.h.joinedManageGRPC(internalgrpc.EntityTransitionRequest, map[string]any{
			"id": "se-nl-grpc-tr", "entityId": sID, "transition": "go",
		}, rc.token)
	})
	requireNotLockedEnvelope(t, "gRPC joined transition", env, err)
	requireSchemaLacks(t, h, target, field)
}
