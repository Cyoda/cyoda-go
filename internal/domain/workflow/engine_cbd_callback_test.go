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
// S_post whose one processor is COMMIT_BEFORE_DISPATCH with
// startNewTxOnDispatch=true, so the processor's callback joins TX_post.
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
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	mock := &mockExternalProcessing{dispatchFunc: func(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
		return dispatch(ctx, entity, txID)
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
	startNewTx := true
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "CbdCallbackWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{{Name: "CALLOUT", Next: "S_post",
				Processors: []spi.ProcessorDefinition{{
					Type: ProcessorTypeExternalized, Name: "cbd-proc", ExecutionMode: ExecutionModeCommitBeforeDispatch,
					Config: spi.ProcessorConfig{StartNewTxOnDispatch: &startNewTx},
				}}}}},
			"S_post": {},
		},
	}})
	return &cbdCallbackEnv{factory: factory, txMgr: txMgr, engine: engine, ctx: ctx, model: modelRef}
}

// execute creates entity id through the workflow. It returns TX_pre's id, the
// engine's entity, the engine's result and error.
func (env *cbdCallbackEnv) execute(t *testing.T, id string) (string, *spi.Entity, *EngineResult, error) {
	t.Helper()
	txID, txCtx, err := env.txMgr.Begin(env.ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	entity := &spi.Entity{
		Meta: spi.EntityMeta{ID: id, TenantID: testTenant, ModelRef: env.model, TransactionID: txID},
		Data: []byte(`{"x":0}`),
	}
	result, err := env.engine.Execute(txCtx, entity, "")
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

// A callback that joins TX_post and writes the cascade-anchor entity: the
// transition succeeds, and the engine's result is the last write, whether or
// not the processor also returns mutations.
func TestCBDCallback_WritesAnchor_EngineResultIsLastWrite(t *testing.T) {
	cases := []struct {
		name     string
		returned *spi.Entity
		wantData map[string]any
	}{
		{name: "no mutations returned", returned: nil, wantData: map[string]any{"x": float64(0)}},
		{name: "mutations returned", returned: &spi.Entity{Data: []byte(`{"x":42,"by":"engine"}`)}, wantData: map[string]any{"x": float64(42), "by": "engine"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var env *cbdCallbackEnv
			env = newCBDCallbackEnv(t, func(ctx context.Context, entity *spi.Entity, _ string) (*spi.Entity, error) {
				if err := writeAnchor(ctx, env.factory, entity); err != nil {
					return nil, err
				}
				return tc.returned, nil
			})
			_, entity, result, err := env.execute(t, "cbdw-1")
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			es, err := env.factory.EntityStore(result.FinalCtx)
			if err != nil {
				t.Fatalf("EntityStore: %v", err)
			}
			// What the boundary left in TX_post: the engine's result, over
			// the callback's write.
			inTx, err := es.Get(result.FinalCtx, "cbdw-1")
			if err != nil {
				t.Fatalf("Get in TX_post: %v", err)
			}
			assertData(t, inTx.Data, tc.wantData)
			if err := finish(env, entity, result); err != nil {
				t.Fatalf("finish: %v", err)
			}
			got, found := env.committed(t, "cbdw-1")
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
	txPre, _, _, err := env.execute(t, "cbdd-1")
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
			_, entity, result, err := env.execute(t, "cbdo-1")
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
		t.Errorf("data = %v, want %v: the engine's result is the last write", data, want)
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

			_, entity, result, err := env.execute(t, "cbdb-1")
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
