package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
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
		Config: spi.ProcessorConfig{StartNewTxOnDispatch: &startNewTx},
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
	engine := NewEngine(factory, uuids, engineTx, WithExternalProcessing(mock))
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
	return &cbdCallbackEnv{factory: factory, txMgr: txMgr, engine: engine, ctx: ctx, model: modelRef}
}

// transition seeds entity id in S_pre with {"x":0}, then runs the manual
// transition CALLOUT on it in a new transaction, as the entity service does.
// It returns the transaction's id (TX_pre when the processor segments), the
// engine's entity, the engine's result and error.
func (env *cbdCallbackEnv) transition(t *testing.T, id string) (string, *spi.Entity, *EngineResult, error) {
	t.Helper()
	seedFireEntity(t, env.factory, env.ctx, id, env.model, "S_pre", "seed-tx", map[string]any{"x": 0})
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
	result, err := env.engine.ManualTransition(txCtx, entity, "CALLOUT")
	return txID, entity, result, err
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

// writeAnchor saves the cascade-anchor entity through the transaction on ctx,
// with data marking the processor as its writer.
func writeAnchor(ctx context.Context, factory spi.StoreFactory, entity *spi.Entity) error {
	es, err := factory.EntityStore(ctx)
	if err != nil {
		return err
	}
	cur, err := es.Get(ctx, entity.Meta.ID)
	if err != nil {
		return err
	}
	cur.Data = []byte(`{"x":7,"by":"processor"}`)
	_, err = es.Save(ctx, cur)
	return err
}

// callbackModes are the processor dispatch sites whose callback joins the
// transaction the engine continues in.
func callbackModes() []struct {
	name string
	proc func(name string) spi.ProcessorDefinition
} {
	mode := func(m string) func(string) spi.ProcessorDefinition {
		return func(name string) spi.ProcessorDefinition {
			return spi.ProcessorDefinition{Type: ProcessorTypeExternalized, Name: name, ExecutionMode: m}
		}
	}
	return []struct {
		name string
		proc func(name string) spi.ProcessorDefinition
	}{
		{"sync", mode(ExecutionModeSync)},
		{"async_same_tx", mode(ExecutionModeAsyncSameTx)},
		{"cbd_start_new_tx", cbdNewTxProc},
	}
}

var (
	processorWrote = map[string]any{"x": float64(7), "by": "processor"}
	engineApplied  = map[string]any{"x": float64(42), "by": "engine"}
)

// A processor whose callback writes the anchor through the transaction it
// joined. With no mutations returned, the callback's write is kept and the
// transition still takes effect. With mutations returned, the engine's result
// is applied last and overwrites it.
func TestProcessorCallback_WritesAnchor(t *testing.T) {
	cases := []struct {
		name     string
		returned *spi.Entity
		wantData map[string]any
	}{
		{name: "no mutations returned keeps the callback's write", returned: nil, wantData: processorWrote},
		{name: "mutations returned overwrite it", returned: &spi.Entity{Data: []byte(`{"x":42,"by":"engine"}`)}, wantData: engineApplied},
	}
	for _, mode := range callbackModes() {
		for _, tc := range cases {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				var env *cbdCallbackEnv
				env = newCallbackEnv(t, []spi.ProcessorDefinition{mode.proc("p1")},
					func(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
						if err := writeAnchor(ctx, env.factory, entity); err != nil {
							return nil, err
						}
						return tc.returned, nil
					})
				_, entity, result, err := env.transition(t, "cbw-1")
				if err != nil {
					t.Fatalf("ManualTransition: %v", err)
				}
				if err := finish(env, entity, result); err != nil {
					t.Fatalf("finish: %v", err)
				}
				got, found := env.committed(t, "cbw-1")
				if !found {
					t.Fatal("entity not found after commit")
				}
				if got.Meta.State != "S_post" {
					t.Errorf("state = %q, want S_post", got.Meta.State)
				}
				assertData(t, got.Data, tc.wantData)
			})
		}
	}
}

// A later processor in the same chain is dispatched with the payload the
// callback wrote.
func TestProcessorCallback_WritesAnchor_LaterProcessorSeesIt(t *testing.T) {
	for _, mode := range callbackModes() {
		t.Run(mode.name, func(t *testing.T) {
			var env *cbdCallbackEnv
			var seen []byte
			env = newCallbackEnv(t, []spi.ProcessorDefinition{
				mode.proc("p1"),
				{Type: ProcessorTypeExternalized, Name: "p2", ExecutionMode: ExecutionModeSync},
			}, func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
				if proc.Name == "p2" {
					seen = append([]byte(nil), entity.Data...)
					return nil, nil
				}
				return nil, writeAnchor(ctx, env.factory, entity)
			})
			_, entity, result, err := env.transition(t, "cbl-1")
			if err != nil {
				t.Fatalf("ManualTransition: %v", err)
			}
			assertData(t, seen, processorWrote)
			if err := finish(env, entity, result); err != nil {
				t.Fatalf("finish: %v", err)
			}
			got, _ := env.committed(t, "cbl-1")
			if got == nil || got.Meta.State != "S_post" {
				t.Fatalf("committed = %+v, want S_post", got)
			}
			assertData(t, got.Data, processorWrote)
		})
	}
}

// A processor whose callback writes nothing leaves the engine's payload
// alone, even when the anchor was written earlier in the same transaction by
// an earlier processor's callback and a later processor then returned
// mutations the transaction has not stored yet.
func TestProcessorCallback_NoWrite_KeepsEnginePayload(t *testing.T) {
	var env *cbdCallbackEnv
	env = newCallbackEnv(t, []spi.ProcessorDefinition{
		{Type: ProcessorTypeExternalized, Name: "p1", ExecutionMode: ExecutionModeSync},
		{Type: ProcessorTypeExternalized, Name: "p2", ExecutionMode: ExecutionModeSync},
		{Type: ProcessorTypeExternalized, Name: "p3", ExecutionMode: ExecutionModeSync},
	}, func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		switch proc.Name {
		case "p1":
			return nil, writeAnchor(ctx, env.factory, entity)
		case "p2":
			return &spi.Entity{Data: []byte(`{"x":42,"by":"engine"}`)}, nil
		default:
			return nil, nil
		}
	})
	_, entity, result, err := env.transition(t, "cbn-1")
	if err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	assertData(t, entity.Data, engineApplied)
	if err := finish(env, entity, result); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

// The same without any callback write: the anchor is still the committed
// version, and the engine keeps the mutations an earlier processor returned.
func TestProcessorCallback_NoWrite_CommittedAnchor_KeepsEnginePayload(t *testing.T) {
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

// A callback that deletes the cascade-anchor entity mid-chain still fails the
// transition; nothing past TX_pre's commit is kept.
func TestCBDCallback_DeletesAnchor_Fails(t *testing.T) {
	var env *cbdCallbackEnv
	env = newCBDCallbackEnv(t, func(ctx context.Context, entity *spi.Entity, _ string) (*spi.Entity, error) {
		es, err := env.factory.EntityStore(ctx)
		if err != nil {
			return nil, err
		}
		return nil, es.Delete(ctx, entity.Meta.ID)
	})
	txPre, _, _, err := env.transition(t, "cbdd-1")
	if !errors.Is(err, spi.ErrConflict) || !errors.Is(err, ErrPostSegmentConflict) {
		t.Fatalf("Execute: err = %v, want a post-segment conflict", err)
	}
	got, found := env.committed(t, "cbdd-1")
	if !found {
		t.Fatal("entity gone: the callback's delete in TX_post must not commit")
	}
	if got.Meta.TransactionID != txPre {
		t.Errorf("committed transaction id = %q, want TX_pre's %q", got.Meta.TransactionID, txPre)
	}
}

// A write committed by another transaction still conflicts, with or without
// the callback's own write in TX_post.
func TestCBDCallback_OtherTransactionWrites_Conflicts(t *testing.T) {
	for _, ownWrite := range []bool{false, true} {
		name := "other write only"
		if ownWrite {
			name = "own write and other write"
		}
		t.Run(name, func(t *testing.T) {
			var env *cbdCallbackEnv
			env = newCBDCallbackEnv(t, func(ctx context.Context, entity *spi.Entity, _ string) (*spi.Entity, error) {
				if ownWrite {
					if err := writeAnchor(ctx, env.factory, entity); err != nil {
						return nil, err
					}
				}
				otherID, otherCtx, err := env.txMgr.Begin(env.ctx)
				if err != nil {
					return nil, err
				}
				es, err := env.factory.EntityStore(otherCtx)
				if err != nil {
					return nil, err
				}
				cur, err := es.Get(otherCtx, entity.Meta.ID)
				if err != nil {
					return nil, err
				}
				cur.Data = []byte(`{"x":9,"by":"other"}`)
				if _, err := es.Save(otherCtx, cur); err != nil {
					return nil, err
				}
				return nil, env.txMgr.Commit(env.ctx, otherID)
			})
			_, entity, result, err := env.transition(t, "cbdo-1")
			if err == nil {
				err = finish(env, entity, result)
			}
			if !errors.Is(err, spi.ErrConflict) {
				t.Fatalf("err = %v, want a conflict", err)
			}
			got, _ := env.committed(t, "cbdo-1")
			if got == nil || got.Meta.State == "S_post" {
				t.Errorf("committed entity = %+v, want the other transaction's write to stand", got)
			}
		})
	}
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
// before TX_post began. The callback reads that write in TX_post and saves
// over it; the transition must conflict rather than lose the other write.
func TestCBDCallback_OtherTransactionWritesBeforeTXPostBegins_Conflicts(t *testing.T) {
	for _, ownWrite := range []bool{false, true} {
		name := "no callback write"
		if ownWrite {
			name = "callback write"
		}
		t.Run(name, func(t *testing.T) {
			var env *cbdCallbackEnv
			var wrap *beforeNthBeginTxMgr
			env = newCBDCallbackEnv(t, func(ctx context.Context, entity *spi.Entity, _ string) (*spi.Entity, error) {
				if ownWrite {
					return nil, writeAnchor(ctx, env.factory, entity)
				}
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
			got, _ := env.committed(t, "cbdb-1")
			if got == nil {
				t.Fatal("entity gone")
			}
			assertData(t, got.Data, map[string]any{"x": float64(9), "by": "other"})
		})
	}
}

// Another transaction deletes the anchor after TX_pre committed and before
// TX_post began, and the callback saves the entity blind in TX_post. The
// transition conflicts and the entity stays deleted.
func TestCBDCallback_OtherTransactionDeletesBeforeTXPostBegins_Conflicts(t *testing.T) {
	var env *cbdCallbackEnv
	var wrap *beforeNthBeginTxMgr
	env = newCBDCallbackEnv(t, func(ctx context.Context, entity *spi.Entity, _ string) (*spi.Entity, error) {
		es, err := env.factory.EntityStore(ctx)
		if err != nil {
			return nil, err
		}
		blind := &spi.Entity{Meta: entity.Meta, Data: []byte(`{"x":7,"by":"processor"}`)}
		_, err = es.Save(ctx, blind)
		return nil, err
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
	if got, found := env.committed(t, "cbdx-1"); found {
		t.Errorf("committed entity = %+v, want none: the other transaction's delete stands", got)
	}
}

// A callback write is adopted even when it stores the committed payload
// unchanged: the transaction had not written the anchor before the dispatch,
// so the write is this dispatch's, and it replaces the payload an earlier
// processor returned.
func TestProcessorCallback_FirstWriteOfCommittedBytes_IsAdopted(t *testing.T) {
	var env *cbdCallbackEnv
	env = newCallbackEnv(t, []spi.ProcessorDefinition{
		{Type: ProcessorTypeExternalized, Name: "p1", ExecutionMode: ExecutionModeSync},
		{Type: ProcessorTypeExternalized, Name: "p2", ExecutionMode: ExecutionModeSync},
	}, func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _ string) (*spi.Entity, error) {
		if proc.Name == "p1" {
			return &spi.Entity{Data: []byte(`{"x":42,"by":"engine"}`)}, nil
		}
		es, err := env.factory.EntityStore(ctx)
		if err != nil {
			return nil, err
		}
		stored, err := es.Get(ctx, entity.Meta.ID)
		if err != nil {
			return nil, err
		}
		_, err = es.Save(ctx, stored) // the committed bytes, unchanged
		return nil, err
	})
	_, entity, _, err := env.transition(t, "cbf-1")
	if err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	assertData(t, entity.Data, map[string]any{"x": float64(0)})
}
