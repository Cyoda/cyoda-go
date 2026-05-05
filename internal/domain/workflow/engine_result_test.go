package workflow

import (
	"context"
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestEngineResult_Segmented_FalseOnNonSegmentingCascade asserts that for a
// Manual transition through a workflow with no COMMIT_BEFORE_DISPATCH
// processors, EngineResult.Segmented is false. The engine never opened a
// fresh TX_post; FinalTxID equals the caller's input txID.
//
// This is the "Segmented" flag that handlers read to decide whether they
// own the IfMatch precondition (non-segmenting → handler-side CompareAndSave)
// versus the engine having consumed it at first-segment flush (segmented
// → handler does plain Save). Replaces the old `FinalTxID != entryTxID`
// comparison-vs-entry-txID convention in single UpdateEntity and the
// UpdateEntityCollection per-item loop (issue #228 N1).
func TestEngineResult_Segmented_FalseOnNonSegmentingCascade(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "no-cbd-segmented", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "NoCbdSegmentedWF", InitialState: "INITIAL", Active: true,
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
	if res.Segmented {
		t.Errorf("Segmented = true; want false (no CBD processor in pipeline)")
	}
	if res.FinalTxID != inputTxID {
		t.Errorf("FinalTxID = %q, want input txID %q (sanity-check: not segmented)", res.FinalTxID, inputTxID)
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

// TestEngineResult_Segmented_TrueOnCBDCascade asserts that after a cascade
// containing a COMMIT_BEFORE_DISPATCH processor, EngineResult.Segmented is
// true. The engine has committed TX_pre and opened TX_post; the handler
// must NOT re-apply the caller's IfMatch (already consumed at first-segment
// flush) and must commit TX_post via FinalCtx/FinalTxID.
func TestEngineResult_Segmented_TrueOnCBDCascade(t *testing.T) {
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

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "CbdSegmentedWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{
				{Name: "CALLOUT", Next: "S_post", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: "EXTERNAL", Name: "cbd-proc", ExecutionMode: ExecutionModeCommitBeforeDispatch},
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
	if !res.Segmented {
		t.Errorf("Segmented = false; want true (CBD processor segmented the cascade)")
	}
	if res.FinalTxID == cascadeEntryTxID {
		t.Errorf("FinalTxID = entryTxID = %q; sanity-check: should differ on segmented cascade", cascadeEntryTxID)
	}

	// Cleanup: handler-style commit of the final TX.
	if err := txMgr.Commit(res.FinalCtx, res.FinalTxID); err != nil {
		t.Fatalf("commit FinalTxID: %v", err)
	}
}

// TestEngineResult_Segmented_FalseOnLoopbackNoWorkflow asserts that the
// Loopback fast-path (current state not in any workflow → STATE_NOT_IN_WORKFLOW)
// reports Segmented=false. The engine performs no work and cannot have segmented.
func TestEngineResult_Segmented_FalseOnLoopbackNoWorkflow(t *testing.T) {
	engine, _ := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "loopback-no-wf", ModelVersion: "1.0"}

	// No workflow saved for this modelRef, but Loopback uses default WF for
	// missing models. To force the no-workflow path, use a state that the
	// default WF doesn't contain.
	entity := makeEntity("loopback-no-wf-1", modelRef, map[string]any{"x": 1})
	entity.Meta.State = "ORPHAN_STATE_NOT_IN_DEFAULT_WF"

	res, err := engine.Loopback(ctx, entity)
	if err != nil {
		t.Fatalf("Loopback: %v", err)
	}
	if res.Segmented {
		t.Errorf("Segmented = true; want false (loopback fast-path, no engine work)")
	}
	if res.StopReason != "STATE_NOT_IN_WORKFLOW" {
		t.Fatalf("StopReason = %q, want STATE_NOT_IN_WORKFLOW (sanity check)", res.StopReason)
	}
}
