package workflow

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// ifMatchFixture is one memory backend with an entity seeded in S_pre, whose
// manual transition "go" runs procs. seedTxID is the version an If-Match names.
type ifMatchFixture struct {
	factory  spi.StoreFactory
	txMgr    spi.TransactionManager
	engine   *Engine
	ctx      context.Context
	modelRef spi.ModelRef
	id       string
	seedTxID string
}

func newIfMatchFixture(t *testing.T, name string, dispatch func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error), procs ...spi.ProcessorDefinition) *ifMatchFixture {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	mock := &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			return dispatch(ctx, entity, proc)
		},
	}
	f := &ifMatchFixture{
		factory: factory, txMgr: txMgr, ctx: ctxWithTenant(testTenant),
		engine:   NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock)),
		modelRef: spi.ModelRef{EntityName: name, ModelVersion: "1.0"},
		id:       name + "-1",
	}
	registerModelFields(t, f.ctx, factory, f.modelRef, map[string]schema.DataType{"x": schema.Integer})
	saveWorkflow(t, factory, f.ctx, f.modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: name + "-wf", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre":  {Transitions: []spi.TransitionDefinition{{Name: "go", Next: "S_post", Manual: true, Processors: procs}}},
			"S_post": {},
		},
	}})
	seedTxID, seedCtx, err := txMgr.Begin(f.ctx)
	if err != nil {
		t.Fatalf("seed Begin: %v", err)
	}
	es, _ := factory.EntityStore(seedCtx)
	if _, err := es.Save(seedCtx, f.entity(seedTxID)); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if err := txMgr.Commit(seedCtx, seedTxID); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}
	f.seedTxID = seedTxID
	return f
}

// entity is the engine's entity for a request in the transaction txID.
func (f *ifMatchFixture) entity(txID string) *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{ID: f.id, TenantID: testTenant, ModelRef: f.modelRef, State: "S_pre", TransactionID: txID},
		Data: []byte(`{"x":1}`),
	}
}

func (f *ifMatchFixture) stored(t *testing.T) *spi.Entity {
	t.Helper()
	es, _ := f.factory.EntityStore(f.ctx)
	e, err := es.Get(f.ctx, f.id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return e
}

var cbdProc = spi.ProcessorDefinition{Type: ProcessorTypeExternalized, Name: "cbd", ExecutionMode: ExecutionModeCommitBeforeDispatch}

// TestIfMatch_OwnWriteBeforeSegmentKeepsThePrecondition — If-Match states the
// version the request starts from. A SYNC processor writes the entity in the
// request's own transaction (as a joined callback does) before a
// COMMIT_BEFORE_DISPATCH segment commits: the write is the request's own, so
// the precondition still holds and the cascade completes with that write.
func TestIfMatch_OwnWriteBeforeSegmentKeepsThePrecondition(t *testing.T) {
	var f *ifMatchFixture
	f = newIfMatchFixture(t, "ifmatch-own-write", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		if proc.Name != "writer" {
			return nil, nil
		}
		es, err := f.factory.EntityStore(ctx)
		if err != nil {
			return nil, err
		}
		own := *entity
		own.Data = []byte(`{"x":2}`)
		if _, err := es.Save(ctx, &own); err != nil {
			return nil, err
		}
		return nil, nil
	}, spi.ProcessorDefinition{Type: ProcessorTypeExternalized, Name: "writer", ExecutionMode: ExecutionModeSync}, cbdProc)

	txID, txCtx, err := f.txMgr.Begin(f.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	entity := f.entity(txID)
	res, err := f.engine.ManualTransitionWithIfMatch(txCtx, entity, "go", IfMatch{Expected: f.seedTxID, Current: f.seedTxID})
	if err != nil {
		t.Fatalf("ManualTransitionWithIfMatch: %v", err)
	}
	if res.FinalTxID == txID || entity.Meta.State != "S_post" {
		t.Fatalf("FinalTxID=%s state=%q; want a segmented cascade ending in S_post", res.FinalTxID, entity.Meta.State)
	}
	es, _ := f.factory.EntityStore(res.FinalCtx)
	if _, err := es.Save(res.FinalCtx, entity); err != nil {
		t.Fatalf("final Save: %v", err)
	}
	if err := f.txMgr.Commit(res.FinalCtx, res.FinalTxID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := f.stored(t); got.Meta.State != "S_post" || string(got.Data) != `{"x":2}` {
		t.Fatalf("stored %s %s; want S_post with the processor's write", got.Meta.State, got.Data)
	}
}

// TestIfMatch_StaleAbortsBeforeAnything — a stale If-Match fails the request
// before the engine selects a workflow or dispatches anything, with a conflict
// that carries none of the markers a transaction conflict carries: it is the
// request's own precondition, which a batching caller isolates to its item.
func TestIfMatch_StaleAbortsBeforeAnything(t *testing.T) {
	for _, door := range []string{"ManualTransition", "Loopback"} {
		t.Run(door, func(t *testing.T) {
			dispatched := false
			f := newIfMatchFixture(t, "ifmatch-stale-"+door, func(context.Context, *spi.Entity, spi.ProcessorDefinition) (*spi.Entity, error) {
				dispatched = true
				return nil, nil
			}, cbdProc)
			txID, txCtx, err := f.txMgr.Begin(f.ctx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer func() { _ = f.txMgr.Rollback(f.ctx, txID) }()

			stale := IfMatch{Expected: "tx-that-never-existed", Current: f.seedTxID}
			if door == "Loopback" {
				_, err = f.engine.LoopbackWithIfMatch(txCtx, f.entity(txID), stale)
			} else {
				_, err = f.engine.ManualTransitionWithIfMatch(txCtx, f.entity(txID), "go", stale)
			}
			if !errors.Is(err, spi.ErrConflict) {
				t.Fatalf("err = %v; want spi.ErrConflict", err)
			}
			for _, marker := range []error{spi.ErrTxAborted, ErrPostSegmentConflict, ErrCommitBeforeDispatchInfra} {
				if errors.Is(err, marker) {
					t.Errorf("the precondition failure carries %v; a batching caller could not isolate it", marker)
				}
			}
			if dispatched {
				t.Error("a processor was dispatched despite the stale If-Match")
			}
			if got := f.stored(t); got.Meta.TransactionID != f.seedTxID {
				t.Errorf("the entity is at %s; want it untouched at %s", got.Meta.TransactionID, f.seedTxID)
			}
		})
	}
}

// TestIfMatch_Matching_RunsTheTransition — an If-Match naming the version the
// request starts from lets the transition run as it would without one.
func TestIfMatch_Matching_RunsTheTransition(t *testing.T) {
	f := newIfMatchFixture(t, "ifmatch-match", func(context.Context, *spi.Entity, spi.ProcessorDefinition) (*spi.Entity, error) {
		return nil, nil
	})
	txID, txCtx, err := f.txMgr.Begin(f.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = f.txMgr.Rollback(f.ctx, txID) }()
	entity := f.entity(txID)
	res, err := f.engine.ManualTransitionWithIfMatch(txCtx, entity, "go", IfMatch{Expected: f.seedTxID, Current: f.seedTxID})
	if err != nil {
		t.Fatalf("ManualTransitionWithIfMatch: %v", err)
	}
	if res.FinalTxID != txID || entity.Meta.State != "S_post" {
		t.Fatalf("FinalTxID=%s state=%q; want %s and S_post", res.FinalTxID, entity.Meta.State, txID)
	}
}
