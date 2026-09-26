package workflow

import (
	"context"
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestEngineResult_FinalTxID_EntryOnNonSegmentingCascade asserts that for a
// manual transition through a workflow with no COMMIT_BEFORE_DISPATCH
// processors the engine opens no fresh TX_post: FinalTxID equals the caller's
// input txID, and the caller commits it.
func TestEngineResult_FinalTxID_EntryOnNonSegmentingCascade(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "no-cbd-segmented", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "NoCbdSegmentedWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "GO", Next: "DONE", Manual: true},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	inputTxID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	entity := makeEntity("non-segmented-1", modelRef, map[string]any{"x": 1})
	entity.Meta.State = "INITIAL"
	entity.Meta.TransactionID = inputTxID

	res, err := engine.ManualTransition(txCtx, entity, "GO")
	if err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}
	if res.FinalTxID != inputTxID {
		t.Errorf("FinalTxID = %q, want input txID %q (no CBD processor in pipeline)", res.FinalTxID, inputTxID)
	}

	// Cleanup: caller commits the (un-segmented) input TX.
	es, _ := factory.EntityStore(txCtx)
	if _, err := es.Save(txCtx, entity); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := txMgr.Commit(txCtx, inputTxID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// TestEngineResult_FinalTxID_PostSegmentOnCBDCascade asserts that after a
// cascade containing a COMMIT_BEFORE_DISPATCH processor the engine has
// committed TX_pre and opened TX_post, which it returns as FinalCtx/FinalTxID
// for the caller to commit.
func TestEngineResult_FinalTxID_PostSegmentOnCBDCascade(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	mock := &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
			modified, _ := json.Marshal(map[string]any{"enriched": true})
			return &spi.Entity{Data: modified}, nil
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cbd-segmented", ModelVersion: "1.0"}

	// cbd-proc's returned data passes the same model checks a client write
	// does: declare the entity's own `x` and the `enriched` field the
	// processor writes. Strict model — no ChangeLevel.
	registerModelFields(t, ctx, factory, modelRef, map[string]schema.DataType{
		"x":        schema.Integer,
		"enriched": schema.Boolean,
	})

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "CbdSegmentedWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{
				{Name: "CALLOUT", Next: "S_post", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "cbd-proc", ExecutionMode: ExecutionModeCommitBeforeDispatch},
					}},
			}},
			"S_post": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	cascadeEntryTxID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:            "cbd-segmented-1",
			TenantID:      testTenant,
			ModelRef:      modelRef,
			State:         "",
			TransactionID: cascadeEntryTxID,
		},
		Data: []byte(`{"x":1}`),
	}

	res, err := engine.Execute(txCtx, entity, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.FinalTxID == cascadeEntryTxID {
		t.Errorf("FinalTxID = entryTxID = %q; want TX_post (CBD processor segmented the cascade)", cascadeEntryTxID)
	}

	// Cleanup: handler-style commit of the final TX.
	if err := txMgr.Commit(res.FinalCtx, res.FinalTxID); err != nil {
		t.Fatalf("commit FinalTxID: %v", err)
	}
}
