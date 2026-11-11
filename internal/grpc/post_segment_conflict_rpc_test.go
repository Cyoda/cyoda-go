package grpc

import (
	"context"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/internal/testing/localproc"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestRPC_PostSegmentConflict_IsEntityModified: another transaction commits a
// write to the entity while its COMMIT_BEFORE_DISPATCH callout runs, so the
// engine's apply-result compare after the dispatch conflicts. Every gRPC write
// door answers as the HTTP doors do: ENTITY_MODIFIED, not retryable, with the
// fixed message and no engine text.
func TestRPC_PostSegmentConflict_IsEntityModified(t *testing.T) {
	doors := []struct {
		name string
		// auto puts the segmenting processor on the automated transition out of
		// the initial state, which a create reaches; otherwise it is on the
		// manual transition "go", which an update names.
		auto  bool
		drive func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model, entityID string) rpcEnvelope
	}{
		{
			name: "create",
			auto: true,
			drive: func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model, _ string) rpcEnvelope {
				return manageEnvelope(t, svc, ctx, makeCE(EntityCreateRequest, map[string]any{
					"id": "c1", "dataFormat": "JSON",
					"payload": map[string]any{"model": map[string]any{"name": model, "version": 1}, "data": map[string]any{"name": "A", "amount": 1}},
				}))
			},
		},
		{
			name: "create-collection",
			auto: true,
			drive: func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model, _ string) rpcEnvelope {
				item := map[string]any{"model": map[string]any{"name": model, "version": 1}, "data": map[string]any{"name": "A", "amount": 1}}
				return collectionEnvelope(t, svc, ctx, makeCE(EntityCreateCollectionRequest, map[string]any{
					"id": "cc1", "dataFormat": "JSON", "payloads": []any{item, item},
				}))
			},
		},
		{
			name: "update",
			drive: func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, _, entityID string) rpcEnvelope {
				return manageEnvelope(t, svc, ctx, makeCE(EntityUpdateRequest, map[string]any{
					"id": "u1", "dataFormat": "JSON",
					"payload": map[string]any{"entityId": entityID, "transition": "go", "data": map[string]any{"name": "B", "amount": 2}},
				}))
			},
		},
		{
			name: "update-collection",
			drive: func(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, _, entityID string) rpcEnvelope {
				return collectionEnvelope(t, svc, ctx, makeCE(EntityUpdateCollectionRequest, map[string]any{
					"id": "uc1", "dataFormat": "JSON",
					"payloads": []any{map[string]any{"entityId": entityID, "transition": "go", "data": map[string]any{"name": "B", "amount": 2}}},
				}))
			},
		},
	}
	for _, door := range doors {
		t.Run(door.name, func(t *testing.T) {
			svc, ctx := newTestEnv(t)
			inner := memory.NewStoreFactory(memory.WithApplyFunc(testSchemaApply))
			inner.NewTransactionManager(common.NewDefaultUUIDGenerator())
			txMgr := inner.GetTransactionManager()

			// Armed only for the call under test, so seeding the entity an
			// update acts on runs the workflow undisturbed.
			var armed atomic.Bool
			lp := localproc.New()
			lp.RegisterProcessor("rival-writer", func(_ context.Context, e *spi.Entity, _ spi.ProcessorDefinition) (*spi.Entity, error) {
				if !armed.Load() {
					return e, nil
				}
				// Dispatched after TX_pre committed and outside any
				// transaction: a rival commits its own write to the entity,
				// which the apply-result compare then meets.
				rivalID, rivalCtx, err := txMgr.Begin(ctx)
				if err != nil {
					return nil, err
				}
				es, err := inner.EntityStore(rivalCtx)
				if err != nil {
					return nil, err
				}
				current, err := es.Get(rivalCtx, e.Meta.ID)
				if err != nil {
					return nil, err
				}
				current.Data = []byte(`{"name":"rival","amount":9}`)
				if _, err := es.Save(rivalCtx, current); err != nil {
					return nil, err
				}
				if err := txMgr.Commit(rivalCtx, rivalID); err != nil {
					return nil, err
				}
				return e, nil
			})
			engine := workflow.NewEngine(inner, common.NewDefaultUUIDGenerator(), txMgr, platformclock.Real(), workflow.WithExternalProcessing(lp), workflow.WithCoordination(txcoord.New(txMgr)))
			svc.txMgr = txMgr
			svc.entityHandler = entity.New(inner, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), newTestConsistency(t, inner), platformclock.Real())
			svc.modelHandler = model.New(inner, platformclock.Real())

			const modelName = "postseg"
			importAndLockModel(t, svc, ctx, modelName, "1", map[string]any{"name": "A", "amount": 1})
			segmenting := []spi.ProcessorDefinition{{
				Type: workflow.ProcessorTypeExternalized, Name: "rival-writer",
				ExecutionMode: workflow.ExecutionModeCommitBeforeDispatch,
			}}
			wf := spi.WorkflowDefinition{
				Version: "1.1", Name: "postseg-wf", InitialState: "NONE", Active: true,
				States: map[string]spi.StateDefinition{
					"NONE":  {Transitions: []spi.TransitionDefinition{{Name: "init", Next: "READY"}}},
					"READY": {Transitions: []spi.TransitionDefinition{{Name: "go", Next: "DONE", Manual: true, Processors: segmenting}}},
					"DONE":  {},
				},
			}
			if door.auto {
				wf.States["NONE"] = spi.StateDefinition{Transitions: []spi.TransitionDefinition{{Name: "init", Next: "READY", Processors: segmenting}}}
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
			calls := lp.ProcessorCallCount("rival-writer")

			armed.Store(true)
			env := door.drive(t, svc, ctx, modelName, entityID)

			if n := lp.ProcessorCallCount("rival-writer") - calls; n != 1 {
				t.Fatalf("callout fired %d time(s), want 1; the request must stop at the entity that conflicted", n)
			}
			if env.success {
				t.Fatal("expected a failure envelope")
			}
			if env.class != "CLIENT_ERROR" {
				t.Errorf("Error.Code = %q; want CLIENT_ERROR", env.class)
			}
			if want := common.ErrCodeEntityModified + ": entity has been modified since last read"; env.message != want {
				t.Errorf("Error.Message = %q; want %q", env.message, want)
			}
			if env.retryable {
				t.Error("a conflict past a committed segment is not retryable: a retry re-fires the callout")
			}
		})
	}
}
