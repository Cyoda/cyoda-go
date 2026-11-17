package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// A scheduled fire's unique-key claims are computed at its commit, from its
// final entity and the keys of the stored model, like any other write: the
// fire attaches no keys of its own.

// codeKey is the one unique key of the key-fire model: $.code.
var codeKey = spi.UniqueKey{ID: "code-key", Fields: []string{"$.code"}}

// keyFireEnv is a memory engine whose model holds codeKey, with a workflow
// OPEN -[AutoClose, scheduled]-> CLOSED whose idempotent SYNC processor sets
// $.code to newCode, and a task armed for entity "kf-e1" (code "A") that is
// due now.
type keyFireEnv struct {
	engine  *Engine
	factory spi.StoreFactory
	ctx     context.Context
	ref     spi.ModelRef
	taskID  string
}

func newKeyFireEnv(t *testing.T, newCode string, others map[string]string) keyFireEnv {
	t.Helper()
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	mock := &mockExternalProcessing{
		dispatchFunc: func(_ context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			var data map[string]any
			if err := json.Unmarshal(e.Data, &data); err != nil {
				return nil, err
			}
			data["code"] = newCode
			raw, err := json.Marshal(data)
			if err != nil {
				return nil, err
			}
			return &spi.Entity{Meta: e.Meta, Data: raw}, nil
		},
	}
	engine, factory, _, advance := setupEngineForCBDFire(t, armMs, mock, 0)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "key-fire", ModelVersion: "1.0"}
	// The processor's output passes the model checks a client write does, so
	// the model declares $.code; its keys are on the model only.
	node := schema.NewObjectNode()
	node.SetChild("code", schema.NewLeafNode(schema.String))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	saveLockedModel(t, factory, ctx, &spi.ModelDescriptor{Ref: ref, Schema: raw, UniqueKeys: []spi.UniqueKey{codeKey}})

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "KeyFireWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "CLOSED", Schedule: &spi.TransitionSchedule{DelayMs: delayMs},
					Processors: []spi.ProcessorDefinition{{
						Type: ProcessorTypeExternalized, Name: "set-code", ExecutionMode: ExecutionModeSync,
						// Idempotent: no unsafe work, so a failed run is a
						// counted attempt, not FAILED at once.
						Config: spi.ProcessorConfig{Idempotent: true},
					}}},
			}},
			"CLOSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, ref, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "kf-e1", ref, "OPEN", true, map[string]any{"code": "A"})
	for id, code := range others {
		seedFireEntity(t, factory, ctx, id, ref, "CLOSED", true, map[string]any{"code": code})
	}

	id := taskID(testTenant, "kf-e1", "OPEN", "AutoClose")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: id, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "kf-e1", ModelName: ref.EntityName,
		Transition: "AutoClose", SourceState: "OPEN", ArmedAt: armMs,
	})
	advance(delayMs)
	return keyFireEnv{engine: engine, factory: factory, ctx: ctx, ref: ref, taskID: id}
}

// saveCode saves a new CLOSED entity id holding code, outside any
// transaction: its own commit checks codeKey.
func (k keyFireEnv) saveCode(t *testing.T, id, code string) error {
	t.Helper()
	es, err := k.factory.EntityStore(k.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	e := makeEntity(id, k.ref, map[string]any{"code": code})
	e.Meta.State = "CLOSED"
	e.Meta.TenantID = testTenant
	_, err = es.Save(k.ctx, e)
	return err
}

// entityCode reads the stored $.code of id.
func (k keyFireEnv) entityCode(t *testing.T, id string) string {
	t.Helper()
	es, err := k.factory.EntityStore(k.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	e, err := es.Get(k.ctx, id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	var data map[string]any
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("decode %s: %v", id, err)
	}
	code, _ := data["code"].(string)
	return code
}

// TestFireScheduled_KeyChangeMovesClaim: the fire changes the key field from
// A to B. After it commits, A is free and B is held by the fired entity.
func TestFireScheduled_KeyChangeMovesClaim(t *testing.T) {
	k := newKeyFireEnv(t, "B", nil)

	r := fireDue(t, k.engine, k.ctx, k.taskID)
	if r.Err != nil || r.Outcome != OutcomeFired {
		t.Fatalf("fire: outcome %v, err %v; want Fired", r.Outcome, r.Err)
	}
	if got := k.entityCode(t, "kf-e1"); got != "B" {
		t.Fatalf("fired entity code = %q; want B", got)
	}
	if got := getEntityState(t, k.factory, k.ctx, "kf-e1"); got != "CLOSED" {
		t.Fatalf("fired entity state = %q; want CLOSED", got)
	}

	if err := k.saveCode(t, "kf-probe-new", "B"); !errors.Is(err, spi.ErrUniqueViolation) {
		t.Errorf("save of a second entity with B: err %v; want ErrUniqueViolation (the fired entity holds B)", err)
	}
	if err := k.saveCode(t, "kf-probe-old", "A"); err != nil {
		t.Errorf("save of a second entity with A: %v; want success (the fire freed A)", err)
	}
}

// TestFireScheduled_BreaksKey_FailedRun: the fire changes the key field to a
// value another entity holds. The fire's commit fails with the violation:
// nothing is committed, the task stays, and the report is a failed run with
// no unsafe work reached, no partial commit and no reason of the engine's
// own — the row of the scheduler's bookkeeping that records the attempt and
// retries it (FAILED once its timeoutMs has passed).
func TestFireScheduled_BreaksKey_FailedRun(t *testing.T) {
	k := newKeyFireEnv(t, "B", map[string]string{"kf-holder": "B"})

	r := fireDue(t, k.engine, k.ctx, k.taskID)
	if r.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %v (err %v); want Failed", r.Outcome, r.Err)
	}
	if !errors.Is(r.Err, spi.ErrUniqueViolation) {
		t.Errorf("err = %v; want ErrUniqueViolation", r.Err)
	}
	if r.UnsafeReached || r.PartialCommit || r.FailReason != "" || r.MarkHeld || r.MarkErrored {
		t.Errorf("report = %+v; want a failed run with no unsafe work, no partial commit, no mark and no engine reason", r)
	}

	if got := getEntityState(t, k.factory, k.ctx, "kf-e1"); got != "OPEN" {
		t.Errorf("entity state = %q; want OPEN (the fire committed nothing)", got)
	}
	if got := k.entityCode(t, "kf-e1"); got != "A" {
		t.Errorf("entity code = %q; want A (the fire committed nothing)", got)
	}
	if _, found := getTask(t, k.factory, k.ctx, k.taskID); !found {
		t.Error("task removed; want it kept for the scheduler to record the attempt")
	}
	if n := countAuditEvents(t, k.factory, k.ctx, "kf-e1", spi.SMEventScheduledTransitionFired); n != 0 {
		t.Errorf("SCHEDULED_TRANSITION_FIRE events = %d; want 0", n)
	}
	// The claims are as before the fire: A is still the entity's, B the holder's.
	if err := k.saveCode(t, "kf-probe-a", "A"); !errors.Is(err, spi.ErrUniqueViolation) {
		t.Errorf("save of a second entity with A: err %v; want ErrUniqueViolation", err)
	}
	if err := k.saveCode(t, "kf-probe-b", "B"); !errors.Is(err, spi.ErrUniqueViolation) {
		t.Errorf("save of a second entity with B: err %v; want ErrUniqueViolation", err)
	}
}
