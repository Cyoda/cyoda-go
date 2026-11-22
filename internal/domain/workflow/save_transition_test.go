package workflow

import (
	"context"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"

	"github.com/cyoda/cyoda-go/internal/common"
	"github.com/cyoda/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda/cyoda-go/internal/platformclock"
	"github.com/cyoda/cyoda-go/internal/txcoord"
	"github.com/cyoda/cyoda-go/plugins/memory"
)

// Every save the engine makes for a request writes the request's transition
// name; what the run evaluates (workflow selection, criteria) sees the name of
// the version the request started from.

// newNamedEnv is a memory-backed engine with external processing that runs
// workflows on one model with an integer field x.
func newNamedEnv(t *testing.T, workflows []spi.WorkflowDefinition, dispatch func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error)) *cbdCallbackEnv {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	mock := &mockExternalProcessing{dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
		return dispatch(ctx, entity, proc)
	}}
	engine := NewEngine(cachedFactory(factory), uuids, txMgr, platformclock.Real(), WithExternalProcessing(mock), WithCoordination(txcoord.New(txMgr)))
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "save-transition", ModelVersion: "1.0"}
	registerModelFields(t, ctx, factory, modelRef, map[string]schema.DataType{"x": schema.Integer})
	saveWorkflow(t, factory, ctx, modelRef, workflows)
	lockTestModel(t, factory, ctx, modelRef)
	return &cbdCallbackEnv{factory: factory, txMgr: txMgr, engine: engine, ctx: ctx, model: modelRef}
}

// seedSavedBy commits entity id in state, as the version a request naming
// savedBy made, and returns it read in a new transaction, stamped with that
// transaction's id as the entity service hands it to the engine.
func seedSavedBy(t *testing.T, env *cbdCallbackEnv, id, state, savedBy string) (string, context.Context, *spi.Entity) {
	t.Helper()
	seed := makeEntity(id, env.model, map[string]any{"x": 0})
	seed.Meta.State = state
	seed.Meta.TransitionForLatestSave = savedBy
	saveCommittedEntity(t, env.factory, env.ctx, seed)
	txID, txCtx, err := env.txMgr.Begin(env.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = env.txMgr.Rollback(env.ctx, txID) })
	es, err := env.factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	entity, err := es.Get(txCtx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	entity.Meta.TransactionID = txID
	return txID, txCtx, entity
}

// committedSaveTransition returns the transition name on the version of id
// that transaction txID committed.
func committedSaveTransition(t *testing.T, factory spi.StoreFactory, ctx context.Context, id, txID string) string {
	t.Helper()
	es, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	v, err := es.GetVersionByTransaction(ctx, id, txID)
	if err != nil {
		t.Fatalf("GetVersionByTransaction(%s): %v", txID, err)
	}
	return v.Entity.Meta.TransitionForLatestSave
}

// The save before a callout writes the transition the request named; the
// entity the run evaluates keeps the start version's name.
func TestSaveTransition_AnchorSaveWritesRequestName(t *testing.T) {
	var env *cbdCallbackEnv
	var stored *spi.Entity
	var evaluated string
	env = newNamedEnv(t, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "AnchorWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre":  {Transitions: []spi.TransitionDefinition{{Name: "approve", Next: "S_post", Manual: true, Processors: []spi.ProcessorDefinition{syncProc("p1")}}}},
			"S_post": {},
		},
	}}, func(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition) (*spi.Entity, error) {
		evaluated = entity.Meta.TransitionForLatestSave
		var err error
		stored, err = readUnderToken(ctx, env.factory, entity.Meta.ID)
		return nil, err
	})
	_, txCtx, entity := seedSavedBy(t, env, "st-anchor-1", "S_pre", "submit")
	entity.Data = []byte(`{"x":5}`) // a data change: the anchor is saved before the callout

	if _, err := env.engine.ManualTransition(txCtx, entity, "approve", IfMatch{}); err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	if stored == nil {
		t.Fatal("the callout read nothing")
	}
	if got := stored.Meta.TransitionForLatestSave; got != "approve" {
		t.Errorf("version saved before the callout carries %q, want the request's %q", got, "approve")
	}
	if evaluated != "submit" {
		t.Errorf("in-memory entity at the callout carries %q, want the start version's %q", evaluated, "submit")
	}
	if got := entity.Meta.TransitionForLatestSave; got != "submit" {
		t.Errorf("engine's entity after the run carries %q, want the start version's %q", got, "submit")
	}
}

// On a create, the version a COMMIT_BEFORE_DISPATCH boundary commits and the
// version the CompareAndSave opening TX_post writes both carry "loopback".
func TestSaveTransition_BoundaryFlushAndPostCASWriteRequestName(t *testing.T) {
	env := newCreateEnv(t, []spi.ProcessorDefinition{cbdNewTxProc("cbd")},
		func(context.Context, *spi.Entity, spi.ProcessorDefinition, string) (*spi.Entity, error) {
			return nil, nil
		})
	const id = "st-boundary-1"
	txPre, entity, result, err := env.create(t, id)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	t.Cleanup(func() { _ = env.txMgr.Rollback(env.ctx, result.FinalTxID) })
	if result.FinalTxID == txPre {
		t.Fatal("the processor did not segment the run; this test needs a boundary")
	}
	if got := committedSaveTransition(t, env.factory, env.ctx, id, txPre); got != "loopback" {
		t.Errorf("boundary version (TX_pre) carries %q, want %q", got, "loopback")
	}
	post, err := readUnderToken(result.FinalCtx, env.factory, id)
	if err != nil {
		t.Fatalf("read in TX_post: %v", err)
	}
	if post.Meta.TransactionID != result.FinalTxID {
		t.Fatalf("TX_post read transactionId = %q, want TX_post's %q", post.Meta.TransactionID, result.FinalTxID)
	}
	if got := post.Meta.TransitionForLatestSave; got != "loopback" {
		t.Errorf("TX_post pending version carries %q, want %q", got, "loopback")
	}
	if got := entity.Meta.TransitionForLatestSave; got != "" {
		t.Errorf("engine's entity after a create carries %q, want none", got)
	}
}

// A lifecycle criterion on transitionForLatestSave sees the start version's
// name, before a COMMIT_BEFORE_DISPATCH boundary and after it.
func TestSaveTransition_CriterionSeesStartValue(t *testing.T) {
	savedBySubmit := lifecycleCriterion("transitionForLatestSave", "EQUALS", "submit")
	env := newNamedEnv(t, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "CriterionWF", InitialState: "S0", Active: true,
		States: map[string]spi.StateDefinition{
			"S0": {Transitions: []spi.TransitionDefinition{{
				Name: "first", Next: "S1", Criterion: savedBySubmit,
				Processors: []spi.ProcessorDefinition{cbdNewTxProc("cbd")},
			}}},
			"S1": {Transitions: []spi.TransitionDefinition{{Name: "second", Next: "S2", Criterion: savedBySubmit}}},
			"S2": {},
		},
	}}, func(context.Context, *spi.Entity, spi.ProcessorDefinition) (*spi.Entity, error) {
		return nil, nil
	})
	txID, txCtx, entity := seedSavedBy(t, env, "st-criterion-1", "S0", "submit")

	result, err := env.engine.Loopback(txCtx, entity, IfMatch{})
	if err != nil {
		t.Fatalf("Loopback: %v", err)
	}
	t.Cleanup(func() { _ = env.txMgr.Rollback(env.ctx, result.FinalTxID) })
	if result.FinalTxID == txID {
		t.Fatal("the processor did not segment the run; this test needs a boundary")
	}
	if entity.Meta.State != "S2" {
		t.Errorf("state = %q, want S2: the criterion before and after the boundary sees %q", entity.Meta.State, "submit")
	}
}

// Workflow selection sees the start version's name, not the transition the
// request names: a request does not choose its workflow by the name it sends.
func TestSaveTransition_SelectionSeesStartValue(t *testing.T) {
	mk := func(name string, criterion []byte, next string) spi.WorkflowDefinition {
		return spi.WorkflowDefinition{
			Version: "1.1", Name: name, InitialState: "S", Active: true, Criterion: criterion,
			States: map[string]spi.StateDefinition{
				"S":  {Transitions: []spi.TransitionDefinition{{Name: "y", Next: next, Manual: true}}},
				next: {},
			},
		}
	}
	env := newNamedEnv(t, []spi.WorkflowDefinition{
		mk("SavedByXWF", lifecycleCriterion("transitionForLatestSave", "EQUALS", "x"), "X_DONE"),
		mk("OtherWF", nil, "OTHER_DONE"),
	}, func(context.Context, *spi.Entity, spi.ProcessorDefinition) (*spi.Entity, error) {
		return nil, nil
	})
	_, txCtx, entity := seedSavedBy(t, env, "st-select-1", "S", "x")

	if _, err := env.engine.ManualTransition(txCtx, entity, "y", IfMatch{}); err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	if entity.Meta.State != "X_DONE" {
		t.Errorf("state = %q, want X_DONE: selection sees the start version's %q", entity.Meta.State, "x")
	}
}
