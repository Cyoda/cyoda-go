package grpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/internal/testing/localproc"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// presaveStoreText is what the failing store says. No envelope carries it.
const presaveStoreText = "pq: relation entities is on fire"

// saveFailingFactory hands out entity stores whose Save fails with the armed
// error. Only the engine is given it, so the failure is the engine's save
// before a callout; the entity handler's own saves stay healthy.
type saveFailingFactory struct {
	spi.StoreFactory
	saveErr atomic.Pointer[error]
}

func (f *saveFailingFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	es, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return &saveFailingStore{EntityStore: es, f: f}, nil
}

type saveFailingStore struct {
	spi.EntityStore
	f *saveFailingFactory
}

func (s *saveFailingStore) Save(ctx context.Context, e *spi.Entity) (int64, error) {
	if p := s.f.saveErr.Load(); p != nil {
		return 0, *p
	}
	return s.EntityStore.Save(ctx, e)
}

// TestRPC_PreCalloutSaveFailure_Envelope: the save the engine makes before a
// SYNC processor's callout fails (before "reader", after "changer" changed the
// data). Every gRPC write door answers as the HTTP
// doors do: a conflict is a retryable CONFLICT, a storage outage a retryable
// STORAGE_UNAVAILABLE, any other failure a SERVER_ERROR with a ticket — never
// WORKFLOW_FAILED or ENTITY_MODIFIED, never the store's text, and nothing is
// dispatched.
func TestRPC_PreCalloutSaveFailure_Envelope(t *testing.T) {
	failures := []struct {
		name  string
		err   error
		check func(t *testing.T, env rpcEnvelope)
	}{
		{"conflict", fmt.Errorf("%s: %w", presaveStoreText, spi.ErrConflict), func(t *testing.T, env rpcEnvelope) {
			t.Helper()
			if env.class != "CLIENT_ERROR" || !strings.HasPrefix(env.message, common.ErrCodeConflict+":") || !env.retryable {
				t.Errorf("envelope = %+v; want a retryable CLIENT_ERROR carrying %s", env, common.ErrCodeConflict)
			}
		}},
		{"storage outage", &storageOutageError{detail: presaveStoreText}, func(t *testing.T, env rpcEnvelope) {
			t.Helper()
			if env.class != "CLIENT_ERROR" || !strings.HasPrefix(env.message, common.ErrCodeStorageUnavailable+":") || !env.retryable {
				t.Errorf("envelope = %+v; want a retryable CLIENT_ERROR carrying %s", env, common.ErrCodeStorageUnavailable)
			}
		}},
		{"other failure", errors.New(presaveStoreText), func(t *testing.T, env rpcEnvelope) {
			t.Helper()
			if env.class != "SERVER_ERROR" || !strings.Contains(env.message, "ticket") || env.retryable {
				t.Errorf("envelope = %+v; want a SERVER_ERROR with a ticket", env)
			}
		}},
	}
	doors := []struct {
		name string
		// auto puts the processor on the automated transition a create
		// reaches; otherwise it is on the manual transition "go".
		auto  bool
		drive func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model, entityID string) rpcEnvelope
	}{
		{"create", true, func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model, _ string) rpcEnvelope {
			return manageEnvelope(t, svc, ctx, makeCE(EntityCreateRequest, map[string]any{
				"id": "c1", "dataFormat": "JSON",
				"payload": map[string]any{"model": map[string]any{"name": model, "version": 1}, "data": map[string]any{"name": "A", "amount": 1}},
			}))
		}},
		{"create-collection", true, func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model, _ string) rpcEnvelope {
			item := map[string]any{"model": map[string]any{"name": model, "version": 1}, "data": map[string]any{"name": "A", "amount": 1}}
			return collectionEnvelope(t, svc, ctx, makeCE(EntityCreateCollectionRequest, map[string]any{
				"id": "cc1", "dataFormat": "JSON", "payloads": []any{item, item},
			}))
		}},
		{"update", false, func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, _, entityID string) rpcEnvelope {
			return manageEnvelope(t, svc, ctx, makeCE(EntityUpdateRequest, map[string]any{
				"id": "u1", "dataFormat": "JSON",
				"payload": map[string]any{"entityId": entityID, "transition": "go", "data": map[string]any{"name": "B", "amount": 2}},
			}))
		}},
		{"update-collection", false, func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, _, entityID string) rpcEnvelope {
			return collectionEnvelope(t, svc, ctx, makeCE(EntityUpdateCollectionRequest, map[string]any{
				"id": "uc1", "dataFormat": "JSON",
				"payloads": []any{map[string]any{"entityId": entityID, "transition": "go", "data": map[string]any{"name": "B", "amount": 2}}},
			}))
		}},
		{"transition", false, func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, _, entityID string) rpcEnvelope {
			return transitionEnvelope(t, svc, ctx, makeCE(EntityTransitionRequest, map[string]any{
				"id": "t1", "entityId": entityID, "transition": "go",
			}))
		}},
	}
	for _, door := range doors {
		for _, f := range failures {
			t.Run(door.name+"/"+f.name, func(t *testing.T) {
				svc, ctx := newTestEnv(t)
				inner := memory.NewStoreFactory(memory.WithApplyFunc(schema.ApplyMany))
				inner.NewTransactionManager(common.NewDefaultUUIDGenerator())
				txMgr := inner.GetTransactionManager()
				failing := &saveFailingFactory{StoreFactory: inner}

				lp := localproc.New()
				lp.RegisterProcessor("changer", func(_ context.Context, e *spi.Entity, _ spi.ProcessorDefinition) (*spi.Entity, error) {
					changed := *e
					changed.Data = []byte(`{"name":"changed","amount":3}`)
					return &changed, nil
				})
				lp.RegisterProcessor("reader", func(_ context.Context, e *spi.Entity, _ spi.ProcessorDefinition) (*spi.Entity, error) {
					return e, nil
				})
				engine := workflow.NewEngine(cachedFactory(failing), common.NewDefaultUUIDGenerator(), txMgr, platformclock.Real(), workflow.WithExternalProcessing(lp), workflow.WithCoordination(txcoord.New(txMgr)))
				svc.txMgr = txMgr
				svc.entityHandler = entity.New(cachedFactory(inner), txMgr, common.NewDefaultUUIDGenerator(), engine, txcoord.New(txMgr), newTestConsistency(t, inner), platformclock.Real())
				svc.modelHandler = model.New(cachedFactory(inner), platformclock.Real())

				const modelName = "presave"
				importAndLockModel(t, svc, ctx, modelName, "1", map[string]any{"name": "A", "amount": 1})
				// A transition sends no data, so the anchor is unchanged before
				// "changer" and nothing is saved there (spec §2.2); the save
				// before "reader" carries changer's data on every door.
				reader := []spi.ProcessorDefinition{
					{Type: workflow.ProcessorTypeExternalized, Name: "changer", ExecutionMode: workflow.ExecutionModeSync},
					{Type: workflow.ProcessorTypeExternalized, Name: "reader", ExecutionMode: workflow.ExecutionModeSync},
				}
				wf := spi.WorkflowDefinition{
					Version: "1.1", Name: "presave-wf", InitialState: "NONE", Active: true,
					States: map[string]spi.StateDefinition{
						"NONE":  {Transitions: []spi.TransitionDefinition{{Name: "init", Next: "READY"}}},
						"READY": {Transitions: []spi.TransitionDefinition{{Name: "go", Next: "DONE", Manual: true, Processors: reader}}},
						"DONE":  {},
					},
				}
				if door.auto {
					wf.States["NONE"] = spi.StateDefinition{Transitions: []spi.TransitionDefinition{{Name: "init", Next: "READY", Processors: reader}}}
				}
				ws, err := inner.WorkflowStore(ctx)
				if err != nil {
					t.Fatalf("WorkflowStore: %v", err)
				}
				if err := ws.Save(ctx, spi.ModelRef{EntityName: modelName, ModelVersion: "1"}, []spi.WorkflowDefinition{wf}); err != nil {
					t.Fatalf("WorkflowStore.Save: %v", err)
				}

				entityID := ""
				if !door.auto {
					seeded := manageReply(t, svc, ctx, makeCE(EntityCreateRequest, map[string]any{
						"id": "seed", "dataFormat": "JSON",
						"payload": map[string]any{"model": map[string]any{"name": modelName, "version": 1}, "data": map[string]any{"name": "A", "amount": 1}},
					}))
					txInfo, ok := parseResponsePayload(t, seeded)["transactionInfo"].(map[string]any)
					if !ok {
						t.Fatalf("seeding the entity failed: %v", parseResponsePayload(t, seeded))
					}
					entityID = txInfo["entityIds"].([]any)[0].(string)
				}
				calls := lp.ProcessorCallCount("reader")

				armed := f.err
				failing.saveErr.Store(&armed)
				env := door.drive(t, svc, ctx, modelName, entityID)

				if n := lp.ProcessorCallCount("reader") - calls; n != 0 {
					t.Fatalf("processor dispatched %d time(s) after the save before it failed", n)
				}
				if env.success {
					t.Fatal("expected a failure envelope")
				}
				if strings.Contains(env.message, presaveStoreText) {
					t.Errorf("the envelope carries the store's text: %q", env.message)
				}
				for _, code := range []string{common.ErrCodeWorkflowFailed, common.ErrCodeEntityModified} {
					if strings.HasPrefix(env.message, code) {
						t.Errorf("envelope message %q: a failed save before a callout is not %s", env.message, code)
					}
				}
				f.check(t, env)
			})
		}
	}
}
