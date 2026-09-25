package workflow

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestManualTransitionWithIfMatch_CBDCascadeStaleEmitsTransitionAborted is the
// Compensating-audit contract: when the engine's CBD first-segment
// flush detects a stale IfMatch and aborts via ErrConflict, it MUST emit a
// compensating TRANSITION_ABORTED audit event so a reader inside the same
// transaction sees a paired entry+abort sequence — STATE_MACHINE_START +
// WORKFLOW_FOUND + TRANSITION_ABORTED — and can correlate the failure
// cleanly, with reason=ENTITY_MODIFIED, the supplied (stale) txID as
// expectedTxId, and the entity's actual txID as actualTxId.
//
// Audit events are bound to the transaction on every backend: once the
// transaction rolls back, every event it recorded — entry and compensating
// abort alike — is gone. So this test reads the events THROUGH cCtx (the
// aborted transaction) before rolling it back, asserts the paired shape and
// payload there, then rolls back and asserts nothing of it survives.
func TestManualTransitionWithIfMatch_CBDCascadeStaleEmitsTransitionAborted(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	mock := &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			return entity, nil
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "ifmatch-aborted", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "AbortedWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{
				{Name: "go", Next: "S_post", Manual: true,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "cbd-proc", ExecutionMode: ExecutionModeCommitBeforeDispatch},
					}},
			}},
			"S_post": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	// Seed the entity in S_pre via TX1.
	seedTxID, seedCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("seed Begin: %v", err)
	}
	es, _ := factory.EntityStore(seedCtx)
	if _, err := es.Save(seedCtx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "ifmatch-aborted-1", TenantID: testTenant,
			ModelRef: modelRef, State: "S_pre", TransactionID: seedTxID,
		},
		Data: []byte(`{"x":1}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if err := txMgr.Commit(seedCtx, seedTxID); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}

	cTxID, cCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("cascade Begin: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "ifmatch-aborted-1", TenantID: testTenant,
			ModelRef: modelRef, State: "S_pre", TransactionID: cTxID,
		},
		Data: []byte(`{"x":1}`),
	}

	const stale = "tx-that-never-existed"
	_, err = engine.ManualTransitionWithIfMatch(cCtx, entity, "go", stale)
	if err == nil {
		t.Fatalf("expected error on stale IfMatch, got nil")
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("expected errors.Is(err, spi.ErrConflict); got %v", err)
	}

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}

	// Read THROUGH the aborted transaction, before it rolls back: the store
	// stages events recorded on cCtx and makes them visible to a read on the
	// same transaction, so this is where the paired entry+abort shape and
	// the abort payload are observable.
	events, err := auditStore.GetEvents(cCtx, "ifmatch-aborted-1")
	if err != nil {
		t.Fatalf("GetEvents (in tx): %v", err)
	}

	var sawStart, sawAbort bool
	var abortEv spi.StateMachineEvent
	for _, ev := range events {
		switch ev.EventType {
		case spi.SMEventStarted:
			sawStart = true
		case SMEventTransitionAborted:
			sawAbort = true
			abortEv = ev
		}
	}
	if !sawStart {
		t.Errorf("expected STATE_MACHINE_START before abort; got events: %v", events)
	}
	if !sawAbort {
		t.Fatalf("expected TRANSITION_ABORTED event after stale-ifMatch abort; got events: %v", events)
	}

	// The abort event's data MUST carry reason=ENTITY_MODIFIED, the supplied
	// (stale) txID as expectedTxId, and the entity's actual txID as actualTxId
	// (here the seedTxID is the row's current transactionId).
	if reason, _ := abortEv.Data["reason"].(string); reason != "ENTITY_MODIFIED" {
		t.Errorf("abort event reason = %q; want ENTITY_MODIFIED", reason)
	}
	if got, _ := abortEv.Data["expectedTxId"].(string); got != stale {
		t.Errorf("abort event expectedTxId = %q; want %q", got, stale)
	}
	if got, _ := abortEv.Data["actualTxId"].(string); got != seedTxID {
		t.Errorf("abort event actualTxId = %q; want %q (entity row's current txID)", got, seedTxID)
	}
	if abortEv.Data["transitionName"] != "go" {
		t.Errorf("abort event transitionName = %v; want \"go\"", abortEv.Data["transitionName"])
	}

	// Audit events are bound to the transaction on every backend: once it
	// rolls back, both the entry events and the compensating abort event are
	// gone, on every backend equally.
	_ = txMgr.Rollback(cCtx, cTxID)

	postEvents, err := auditStore.GetEvents(ctx, "ifmatch-aborted-1")
	if err != nil {
		t.Fatalf("GetEvents (post-rollback): %v", err)
	}
	if len(postEvents) != 0 {
		t.Errorf("rolled-back cascade left %d audit event(s); want 0: %+v", len(postEvents), postEvents)
	}
}
