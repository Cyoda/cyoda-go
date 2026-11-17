package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// cbdCallbackEnv runs an ordinary (not scheduled) transition S_pre --CALLOUT-->
// S_post with the given processors. newCBDCallbackEnv gives it one
// COMMIT_BEFORE_DISPATCH processor with startNewTxOnDispatch=true, so the
// processor's callback joins TX_post.
type cbdCallbackEnv struct {
	factory *memory.StoreFactory
	txMgr   spi.TransactionManager
	engine  *Engine
	ctx     context.Context
	model   spi.ModelRef
}

// wrapTx, when given, wraps the engine's transaction manager only; the test's
// own transactions use the unwrapped one.
func newCBDCallbackEnv(t *testing.T, dispatch func(ctx context.Context, entity *spi.Entity, txID string) (*spi.Entity, error), wrapTx ...func(spi.TransactionManager) spi.TransactionManager) *cbdCallbackEnv {
	t.Helper()
	return newCallbackEnv(t, []spi.ProcessorDefinition{cbdNewTxProc("cbd-proc")},
		func(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, txID string) (*spi.Entity, error) {
			return dispatch(ctx, entity, txID)
		}, wrapTx...)
}

func cbdNewTxProc(name string) spi.ProcessorDefinition {
	startNewTx := true
	return spi.ProcessorDefinition{
		Type: ProcessorTypeExternalized, Name: name, ExecutionMode: ExecutionModeCommitBeforeDispatch,
		StartNewTxOnDispatch: &startNewTx,
	}
}

func newCallbackEnv(t *testing.T, procs []spi.ProcessorDefinition, dispatch func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, txID string) (*spi.Entity, error), wrapTx ...func(spi.TransactionManager) spi.TransactionManager) *cbdCallbackEnv {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	mock := &mockExternalProcessing{dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
		return dispatch(ctx, entity, proc, txID)
	}}
	var engineTx spi.TransactionManager = txMgr
	for _, w := range wrapTx {
		engineTx = w(engineTx)
	}
	engine := NewEngine(cachedFactory(factory), uuids, engineTx, platformclock.Real(), WithExternalProcessing(mock), WithCoordination(txcoord.New(engineTx)))
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cbd-callback", ModelVersion: "1.0"}
	registerModelFields(t, ctx, factory, modelRef, map[string]schema.DataType{
		"x": schema.Integer, "by": schema.String,
	})
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "CbdCallbackWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre":  {Transitions: []spi.TransitionDefinition{{Name: "CALLOUT", Next: "S_post", Manual: true, Processors: procs}}},
			"S_post": {},
		},
	}})
	lockTestModel(t, factory, ctx, modelRef)
	return &cbdCallbackEnv{factory: factory, txMgr: txMgr, engine: engine, ctx: ctx, model: modelRef}
}

// transition seeds entity id in S_pre with {"x":0}, then runs the manual
// transition CALLOUT on it in a new transaction, as the entity service does.
// It returns the transaction's id (TX_pre when the processor segments), the
// engine's entity, the engine's result and error.
func (env *cbdCallbackEnv) transition(t *testing.T, id string) (string, *spi.Entity, *EngineResult, error) {
	t.Helper()
	txID, txCtx, entity := env.prepare(t, id)
	result, err := env.engine.ManualTransition(txCtx, entity, "CALLOUT", IfMatch{})
	return txID, entity, result, err
}

// prepare seeds entity id in S_pre with {"x":0} and reads it in a new
// transaction, ready for the transition.
func (env *cbdCallbackEnv) prepare(t *testing.T, id string) (string, context.Context, *spi.Entity) {
	t.Helper()
	seedFireEntity(t, env.factory, env.ctx, id, env.model, "S_pre", true, map[string]any{"x": 0})
	txID, txCtx, err := env.txMgr.Begin(env.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
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

func (env *cbdCallbackEnv) committed(t *testing.T, id string) (*spi.Entity, bool) {
	t.Helper()
	es, err := env.factory.EntityStore(env.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	e, err := es.Get(env.ctx, id)
	if errors.Is(err, spi.ErrNotFound) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return e, true
}

var engineApplied = map[string]any{"x": float64(42), "by": "engine"}

// A processor that returns no mutations leaves the engine's payload as it is:
// the engine keeps the mutations an earlier processor returned.
func TestProcessor_NoMutations_KeepsEnginePayload(t *testing.T) {
	env := newCallbackEnv(t, []spi.ProcessorDefinition{
		{Type: ProcessorTypeExternalized, Name: "p1", ExecutionMode: ExecutionModeSync},
		{Type: ProcessorTypeExternalized, Name: "p2", ExecutionMode: ExecutionModeSync},
	}, func(_ context.Context, _ *spi.Entity, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		if proc.Name == "p1" {
			return &spi.Entity{Data: []byte(`{"x":42,"by":"engine"}`)}, nil
		}
		return nil, nil
	})
	_, entity, _, err := env.transition(t, "cbc-1")
	if err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	assertData(t, entity.Data, engineApplied)
}

// A write that another transaction commits during the callout makes the
// apply-result CompareAndSave conflict. The processor is held while the test
// commits the rival write in a transaction of its own.
func TestCBDCallback_OtherTransactionWrites_Conflicts(t *testing.T) {
	held := newHeldStub(t)
	env := newCBDCallbackEnv(t, func(context.Context, *spi.Entity, string) (*spi.Entity, error) {
		held.hold()
		return nil, nil
	})
	_, txCtx, entity := env.prepare(t, "cbdo-1")
	type outcome struct {
		result *EngineResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := env.engine.ManualTransition(txCtx, entity, "CALLOUT", IfMatch{})
		done <- outcome{result, err}
	}()
	held.awaitRunning(t)
	rivalSave(t, env.factory, env.txMgr, env.ctx, "cbdo-1", []byte(`{"x":9,"by":"other"}`))
	held.releaseNow()
	var out outcome
	select {
	case out = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the transition hung")
	}
	err := out.err
	if err == nil {
		err = finish(env, entity, out.result)
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("err = %v, want a conflict", err)
	}
	got, _ := env.committed(t, "cbdo-1")
	if got == nil || got.Meta.State == "S_post" {
		t.Fatalf("committed entity = %+v, want the other transaction's write to stand", got)
	}
	assertData(t, got.Data, map[string]any{"x": float64(9), "by": "other"})
}

// finish saves the engine's entity in the open transaction and commits it, as
// the entity service does.
func finish(env *cbdCallbackEnv, entity *spi.Entity, result *EngineResult) error {
	es, err := env.factory.EntityStore(result.FinalCtx)
	if err != nil {
		return err
	}
	if _, err := es.Save(result.FinalCtx, entity); err != nil {
		return err
	}
	return env.txMgr.Commit(result.FinalCtx, result.FinalTxID)
}

func assertData(t *testing.T, raw []byte, want map[string]any) {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(data) != len(want) || data["x"] != want["x"] || data["by"] != want["by"] {
		t.Errorf("data = %v, want %v", data, want)
	}
}

// Another transaction writes the anchor and commits after TX_pre committed and
// before TX_post began. TX_post's read before the dispatch finds that write:
// the transition conflicts, the processor is not dispatched, and the other
// write stands.
func TestCBDCallback_OtherTransactionWritesBeforeTXPostBegins_Conflicts(t *testing.T) {
	var env *cbdCallbackEnv
	var wrap *beforeNthBeginTxMgr
	dispatched := 0
	env = newCBDCallbackEnv(t, func(context.Context, *spi.Entity, string) (*spi.Entity, error) {
		dispatched++
		return nil, nil
	}, func(tm spi.TransactionManager) spi.TransactionManager {
		// The engine's first Begin is TX_post's; TX_pre is the test's.
		wrap = &beforeNthBeginTxMgr{TransactionManager: tm, n: 1}
		return wrap
	})
	wrap.hook = func() error {
		otherID, otherCtx, err := env.txMgr.Begin(env.ctx)
		if err != nil {
			return err
		}
		es, err := env.factory.EntityStore(otherCtx)
		if err != nil {
			return err
		}
		cur, err := es.Get(otherCtx, "cbdb-1")
		if err != nil {
			return err
		}
		cur.Data = []byte(`{"x":9,"by":"other"}`)
		if _, err := es.Save(otherCtx, cur); err != nil {
			return err
		}
		return env.txMgr.Commit(env.ctx, otherID)
	}

	_, entity, result, err := env.transition(t, "cbdb-1")
	if wrap.err != nil {
		t.Fatalf("other transaction: %v", wrap.err)
	}
	if err == nil {
		err = finish(env, entity, result)
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("err = %v, want a conflict", err)
	}
	if dispatched != 0 {
		t.Errorf("the processor was dispatched %d times, want 0", dispatched)
	}
	got, _ := env.committed(t, "cbdb-1")
	if got == nil {
		t.Fatal("entity gone")
	}
	assertData(t, got.Data, map[string]any{"x": float64(9), "by": "other"})
}

// Another transaction deletes the anchor after TX_pre committed and before
// TX_post began. The transition conflicts, the processor is not dispatched,
// and the entity stays deleted.
func TestCBDCallback_OtherTransactionDeletesBeforeTXPostBegins_Conflicts(t *testing.T) {
	var env *cbdCallbackEnv
	var wrap *beforeNthBeginTxMgr
	dispatched := 0
	env = newCBDCallbackEnv(t, func(context.Context, *spi.Entity, string) (*spi.Entity, error) {
		dispatched++
		return nil, nil
	}, func(tm spi.TransactionManager) spi.TransactionManager {
		// The engine's first Begin is TX_post's; TX_pre is the test's.
		wrap = &beforeNthBeginTxMgr{TransactionManager: tm, n: 1}
		return wrap
	})
	wrap.hook = func() error {
		otherID, otherCtx, err := env.txMgr.Begin(env.ctx)
		if err != nil {
			return err
		}
		es, err := env.factory.EntityStore(otherCtx)
		if err != nil {
			return err
		}
		if err := es.Delete(otherCtx, "cbdx-1"); err != nil {
			return err
		}
		return env.txMgr.Commit(env.ctx, otherID)
	}

	_, entity, result, err := env.transition(t, "cbdx-1")
	if wrap.err != nil {
		t.Fatalf("other transaction: %v", wrap.err)
	}
	if err == nil {
		err = finish(env, entity, result)
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("err = %v, want a conflict", err)
	}
	if dispatched != 0 {
		t.Errorf("the processor was dispatched %d times, want 0", dispatched)
	}
	if got, found := env.committed(t, "cbdx-1"); found {
		t.Errorf("committed entity = %+v, want none: the other transaction's delete stands", got)
	}
}
