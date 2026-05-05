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
// reviewer S1 fix (issue #228 follow-up): when the engine's CBD first-segment
// flush detects a stale IfMatch and aborts via ErrConflict, it MUST emit a
// compensating TRANSITION_ABORTED audit event so downstream consumers see a
// paired entry+abort sequence and can correlate the failure cleanly.
//
// Without this event the audit log on memory/sqlite (audit emission is not
// TX-bound) leaves orphan STATE_MACHINE_START / WORKFLOW_FOUND events with no
// matching FINISH and no accompanying entity-version change. The post-fix
// shape is: STATE_MACHINE_START + WORKFLOW_FOUND + TRANSITION_ABORTED with
// reason=ENTITY_MODIFIED, and the abort event references the supplied
// (stale) txID as expectedTxId and the entity's actual txID as actualTxId.
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
		Version: "1.0", Name: "AbortedWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{
				{Name: "go", Next: "S_post", Manual: true,
					Processors: []spi.ProcessorDefinition{
						{Type: "EXTERNAL", Name: "cbd-proc", ExecutionMode: ExecutionModeCommitBeforeDispatch},
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

	_ = txMgr.Rollback(cCtx, cTxID)

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEvents(ctx, "ifmatch-aborted-1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
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
}
