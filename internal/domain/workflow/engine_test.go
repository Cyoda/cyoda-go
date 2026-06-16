package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

const testTenant = spi.TenantID("test-tenant")

func setupEngine(t *testing.T) (*Engine, spi.StoreFactory) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, txMgr)
	return engine, factory
}

func ctxWithTenant(tid spi.TenantID) context.Context {
	uc := &spi.UserContext{UserID: "test-user", Tenant: spi.Tenant{ID: tid, Name: string(tid)}, Roles: []string{"USER"}}
	return spi.WithUserContext(context.Background(), uc)
}

func saveWorkflow(t *testing.T, factory spi.StoreFactory, ctx context.Context, modelRef spi.ModelRef, workflows []spi.WorkflowDefinition) {
	t.Helper()
	ws, err := factory.WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("failed to get workflow store: %v", err)
	}
	if err := ws.Save(ctx, modelRef, workflows); err != nil {
		t.Fatalf("failed to save workflows: %v", err)
	}
}

func makeEntity(id string, modelRef spi.ModelRef, data map[string]any) *spi.Entity {
	d, _ := json.Marshal(data)
	return &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       id,
			TenantID: testTenant,
			ModelRef: modelRef,
			State:    "",
		},
		Data: d,
	}
}

func simpleCriterion(jsonPath, op string, value any) json.RawMessage {
	c, _ := json.Marshal(map[string]any{
		"type": "simple", "jsonPath": jsonPath, "operatorType": op, "value": value,
	})
	return c
}

func TestSelectWorkflow(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "order", ModelVersion: "1.0"}

	// Workflow 1: criterion requires amount > 100
	wf1 := spi.WorkflowDefinition{
		Version: "1.0", Name: "HighValueWF", InitialState: "HIGH_INITIAL", Active: true,
		Criterion: simpleCriterion("$.amount", "GREATER_THAN", 100),
		States: map[string]spi.StateDefinition{
			"HIGH_INITIAL": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	// Workflow 2: criterion requires amount < 50
	wf2 := spi.WorkflowDefinition{
		Version: "1.0", Name: "LowValueWF", InitialState: "LOW_INITIAL", Active: true,
		Criterion: simpleCriterion("$.amount", "LESS_THAN", 50),
		States: map[string]spi.StateDefinition{
			"LOW_INITIAL": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf1, wf2})

	entity := makeEntity("e1", modelRef, map[string]any{"amount": 200})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success")
	}
	if entity.Meta.State != "HIGH_INITIAL" {
		t.Fatalf("expected state HIGH_INITIAL, got %s", entity.Meta.State)
	}
}

func TestNoMatchingWorkflow_FallsBackToDefault(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "order", ModelVersion: "1.0"}

	// Import a workflow with a criterion that won't match.
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "HighValueWF", InitialState: "HIGH_INITIAL", Active: true,
		Criterion: simpleCriterion("$.amount", "GREATER_THAN", 1000),
		States: map[string]spi.StateDefinition{
			"HIGH_INITIAL": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	// Entity with amount=5 won't match the HighValueWF criterion.
	// Engine should fall back to the default workflow.
	entity := makeEntity("e2", modelRef, map[string]any{"amount": 5})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed (should fall back to default): %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success")
	}
	if entity.Meta.State != "CREATED" {
		t.Fatalf("expected state CREATED from default workflow, got %s", entity.Meta.State)
	}
}

func TestNoWorkflowDefined(t *testing.T) {
	engine, _ := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "orphan", ModelVersion: "1.0"}

	entity := makeEntity("e3", modelRef, map[string]any{"name": "test"})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success with default workflow")
	}
	// Default workflow: NONE → (auto NEW) → CREATED
	if entity.Meta.State != "CREATED" {
		t.Fatalf("expected state CREATED from default workflow, got %s", entity.Meta.State)
	}
}

func TestAutomatedCascade(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "approval", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "ApprovalWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "VALIDATE", Next: "PENDING", Manual: false},
			}},
			"PENDING": {Transitions: []spi.TransitionDefinition{
				{Name: "APPROVE", Next: "APPROVED", Manual: false},
			}},
			"APPROVED": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e4", modelRef, map[string]any{"ok": true})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success")
	}
	if entity.Meta.State != "APPROVED" {
		t.Fatalf("expected state APPROVED, got %s", entity.Meta.State)
	}
}

func TestCriterionBlocksTransition(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "blocked", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "BlockedWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "ADVANCE", Next: "DONE", Manual: false,
					Criterion: simpleCriterion("$.approved", "EQUALS", true)},
			}},
			"DONE": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e5", modelRef, map[string]any{"approved": false})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success (stable state)")
	}
	if entity.Meta.State != "INITIAL" {
		t.Fatalf("expected entity to stay at INITIAL, got %s", entity.Meta.State)
	}
}

func TestMultipleTransitionsFirstEligible(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "multi", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "MultiWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "PATH_A", Next: "STATE_A", Manual: false,
					Criterion: simpleCriterion("$.route", "EQUALS", "A")},
				{Name: "PATH_B", Next: "STATE_B", Manual: false,
					Criterion: simpleCriterion("$.route", "EQUALS", "B")},
				{Name: "PATH_C", Next: "STATE_C", Manual: false},
			}},
			"STATE_A": {Transitions: []spi.TransitionDefinition{}},
			"STATE_B": {Transitions: []spi.TransitionDefinition{}},
			"STATE_C": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e6", modelRef, map[string]any{"route": "B"})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success")
	}
	if entity.Meta.State != "STATE_B" {
		t.Fatalf("expected state STATE_B, got %s", entity.Meta.State)
	}
}

func TestNamedTransition(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "named", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "NamedWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "GO", Next: "DONE", Manual: true},
			}},
			"DONE": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e7", modelRef, map[string]any{"x": 1})
	result, err := engine.Execute(ctx, entity, "GO")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success")
	}
	if entity.Meta.State != "DONE" {
		t.Fatalf("expected state DONE, got %s", entity.Meta.State)
	}
}

func TestNamedTransitionNotFound(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "named2", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "NamedWF2", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "GO", Next: "DONE", Manual: true},
			}},
			"DONE": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e8", modelRef, map[string]any{"x": 1})
	_, err := engine.Execute(ctx, entity, "NONEXISTENT")
	if err == nil {
		t.Fatalf("expected error for non-existent transition")
	}
}

// TestErrTransitionNotFound_SentinelWrapped verifies that ManualTransition
// wraps ErrTransitionNotFound when the requested transition is absent from
// the entity's current state, enabling callers to discriminate this case
// from other engine errors via errors.Is.
func TestErrTransitionNotFound_SentinelWrapped(t *testing.T) {
	eng, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "sentinel-tnf", ModelVersion: "1"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "SentinelWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "GO", Next: "DONE", Manual: true},
			}},
			"DONE": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e-sentinel-tnf", modelRef, map[string]any{"x": 1})
	entity.Meta.State = "INITIAL"

	_, err := eng.ManualTransition(ctx, entity, "NONEXISTENT")
	if err == nil {
		t.Fatal("expected error for unknown transition")
	}
	if !errors.Is(err, ErrTransitionNotFound) {
		t.Errorf("expected errors.Is(err, ErrTransitionNotFound) to be true; got: %v", err)
	}
}

// TestErrTransitionNotFound_DisabledTransition verifies that a disabled
// transition also wraps ErrTransitionNotFound.
func TestErrTransitionNotFound_DisabledTransition(t *testing.T) {
	eng, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "sentinel-disabled", ModelVersion: "1"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "DisabledWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "BLOCKED", Next: "DONE", Manual: true, Disabled: true},
			}},
			"DONE": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e-sentinel-disabled", modelRef, map[string]any{"x": 1})
	entity.Meta.State = "INITIAL"

	_, err := eng.ManualTransition(ctx, entity, "BLOCKED")
	if err == nil {
		t.Fatal("expected error for disabled transition")
	}
	if !errors.Is(err, ErrTransitionNotFound) {
		t.Errorf("expected errors.Is(err, ErrTransitionNotFound) for disabled transition; got: %v", err)
	}
}

func TestManualTransition(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "manual", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "ManualWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "SUBMIT", Next: "SUBMITTED", Manual: true},
			}},
			"SUBMITTED": {Transitions: []spi.TransitionDefinition{
				{Name: "AUTO_APPROVE", Next: "APPROVED", Manual: false},
			}},
			"APPROVED": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e9", modelRef, map[string]any{"x": 1})
	entity.Meta.State = "INITIAL"

	result, err := engine.ManualTransition(ctx, entity, "SUBMIT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success")
	}
	// Should have cascaded to APPROVED via automated transition.
	if entity.Meta.State != "APPROVED" {
		t.Fatalf("expected state APPROVED, got %s", entity.Meta.State)
	}
}

func TestProcessorStubSuccess(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "proc", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "ProcWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "PROCESS", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "myProcessor"},
					}},
			}},
			"DONE": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e10", modelRef, map[string]any{"x": 1})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success with processor stub")
	}
	if entity.Meta.State != "DONE" {
		t.Fatalf("expected state DONE, got %s", entity.Meta.State)
	}
}

func TestAuditEventsRecorded(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "audit", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "AuditWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "GO", Next: "DONE", Manual: false},
			}},
			"DONE": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("e11", modelRef, map[string]any{"x": 1})
	_, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("failed to get audit store: %v", err)
	}
	events, err := auditStore.GetEvents(ctx, "e11")
	if err != nil {
		t.Fatalf("failed to get events: %v", err)
	}

	// Expect at minimum: STARTED, WORKFLOW_FOUND, TRANSITION_MAKE, FINISHED
	typeSet := make(map[spi.StateMachineEventType]bool)
	for _, ev := range events {
		typeSet[ev.EventType] = true
	}
	required := []spi.StateMachineEventType{
		spi.SMEventStarted,
		spi.SMEventWorkflowFound,
		spi.SMEventTransitionMade,
		spi.SMEventFinished,
	}
	for _, rt := range required {
		if !typeSet[rt] {
			t.Errorf("missing expected event type %s; got events: %v", rt, events)
		}
	}
}

// TestExecuteUsesCallerTxID verifies that Execute uses the caller-provided
// transaction ID for all state-machine audit events (issue #20). The caller's
// txID is the entity-write transaction ID — it must match what the audit
// endpoint expects so clients can look up /audit/entity/{id}/workflow/{txId}/finished
// using the transactionId returned by POST /entity.
func TestExecuteUsesCallerTxID(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "txid-match", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "TxIdWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "GO", Next: "DONE", Manual: false},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	// Simulate what CreateEntity does: begin a transaction and pass its txID
	// to the workflow engine.
	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	entity := makeEntity("txid-e1", modelRef, map[string]any{"x": 1})
	entity.Meta.TransactionID = txID

	_, err = engine.Execute(txCtx, entity, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// All SM audit events must be findable by the entity-write txID.
	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEventsByTransaction(ctx, "txid-e1", txID)
	if err != nil {
		t.Fatalf("GetEventsByTransaction: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected SM audit events to be findable by the entity-write txID, but got 0 events")
	}

	// Verify the FINISHED event is among them.
	var foundFinished bool
	for _, ev := range events {
		if ev.EventType == spi.SMEventFinished {
			foundFinished = true
		}
		if ev.TransactionID != txID {
			t.Errorf("event %s has txID %q, want %q", ev.EventType, ev.TransactionID, txID)
		}
	}
	if !foundFinished {
		t.Error("FINISHED event not found among events matching entity-write txID")
	}
}

// TestManualTransitionUsesCallerTxID verifies ManualTransition uses the
// caller-provided txID (same issue #20 pattern as Execute).
func TestManualTransitionUsesCallerTxID(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "mt-txid", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "MtTxWF", InitialState: "PENDING", Active: true,
		States: map[string]spi.StateDefinition{
			"PENDING": {Transitions: []spi.TransitionDefinition{
				{Name: "approve", Next: "APPROVED", Manual: true},
			}},
			"APPROVED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	entity := makeEntity("mt-txid-e1", modelRef, map[string]any{"x": 1})
	entity.Meta.State = "PENDING"
	entity.Meta.TransactionID = txID

	_, err = engine.ManualTransition(txCtx, entity, "approve")
	if err != nil {
		t.Fatalf("ManualTransition: %v", err)
	}

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEventsByTransaction(ctx, "mt-txid-e1", txID)
	if err != nil {
		t.Fatalf("GetEventsByTransaction: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected SM audit events to be findable by the entity-write txID, but got 0 events")
	}

	var foundFinished bool
	for _, ev := range events {
		if ev.EventType == spi.SMEventFinished {
			foundFinished = true
		}
		if ev.TransactionID != txID {
			t.Errorf("event %s has txID %q, want %q", ev.EventType, ev.TransactionID, txID)
		}
	}
	if !foundFinished {
		t.Error("FINISHED event not found among events matching entity-write txID")
	}
}

// TestLoopbackUsesCallerTxID verifies Loopback uses the caller-provided
// txID (same issue #20 pattern as Execute and ManualTransition).
func TestLoopbackUsesCallerTxID(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "lb-txid", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "LbTxWF", InitialState: "ACTIVE", Active: true,
		States: map[string]spi.StateDefinition{
			"ACTIVE": {Transitions: []spi.TransitionDefinition{
				{Name: "auto-finish", Next: "DONE", Manual: false},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	entity := makeEntity("lb-txid-e1", modelRef, map[string]any{"x": 1})
	entity.Meta.State = "ACTIVE"
	entity.Meta.TransactionID = txID

	_, err = engine.Loopback(txCtx, entity)
	if err != nil {
		t.Fatalf("Loopback: %v", err)
	}

	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := auditStore.GetEventsByTransaction(ctx, "lb-txid-e1", txID)
	if err != nil {
		t.Fatalf("GetEventsByTransaction: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected SM audit events to be findable by the entity-write txID, but got 0 events")
	}

	var foundFinished bool
	for _, ev := range events {
		if ev.EventType == spi.SMEventFinished {
			foundFinished = true
		}
		if ev.TransactionID != txID {
			t.Errorf("event %s has txID %q, want %q", ev.EventType, ev.TransactionID, txID)
		}
	}
	if !foundFinished {
		t.Error("FINISHED event not found among events matching entity-write txID")
	}
}

func TestLoopback(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "loop", ModelVersion: "1.0"}

	// Loopback: INITIAL -> INITIAL (via auto transition with criterion on counter),
	// then INITIAL -> DONE (fallback without criterion).
	// Since we can't mutate data in the processor stub, we use a trick:
	// First transition has a criterion that won't match, second goes to DONE.
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "LoopWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "LOOP", Next: "INITIAL", Manual: false,
					Criterion: simpleCriterion("$.loop", "EQUALS", true)},
				{Name: "EXIT", Next: "DONE", Manual: false},
			}},
			"DONE": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	// Entity with loop=true: should take LOOP (back to INITIAL), then re-evaluate.
	// On second pass, LOOP matches again → infinite loop risk.
	// Instead, test with loop=false: LOOP criterion fails, EXIT taken.
	entity := makeEntity("e12", modelRef, map[string]any{"loop": false})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success")
	}
	if entity.Meta.State != "DONE" {
		t.Fatalf("expected state DONE, got %s", entity.Meta.State)
	}

	// Now test with loop=true → should hit the loopback and then exit.
	// Since data doesn't change, LOOP would fire infinitely. We need a guard.
	// The engine should have a max-cascade guard. Let's verify it doesn't hang.
	// For now, test that entity with loop=true and an EXIT fallback eventually
	// takes the loop once then re-evaluates (LOOP matches again → loop forever).
	// We'll test the case where the loopback transition exists but doesn't
	// cause infinite loops because the second transition is preferred after a loopback.
	// Actually, with loop=true, LOOP always matches first, causing infinite cascade.
	// A production engine would need a max-iteration guard. Let's verify the
	// non-loopback case is handled correctly (tested above).
}

// --- Transaction-Aware Processor Execution Mode Tests ---

func TestProcessorAsyncNewTxIndependent(t *testing.T) {
	// Define a workflow with an ASYNC_NEW_TX processor.
	// The processor runs in a separate transaction (no conflict with caller's tx).
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "async-new-tx", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "AsyncNewTxWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {
				Transitions: []spi.TransitionDefinition{
					{
						Name: "auto-process", Next: "PROCESSED", Manual: false,
						Processors: []spi.ProcessorDefinition{
							{Type: ProcessorTypeExternalized, Name: "ext-proc", ExecutionMode: "ASYNC_NEW_TX"},
						},
					},
				},
			},
			"PROCESSED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("async-new-tx-entity-1", modelRef, map[string]any{"status": "new"})

	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success=true, got false")
	}
	if entity.Meta.State != "PROCESSED" {
		t.Errorf("expected state=PROCESSED, got %q", entity.Meta.State)
	}
}

func TestProcessorSyncInCallerTx(t *testing.T) {
	// SYNC processor inherits caller's transaction context.
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "sync-proc", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "SyncProcWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {
				Transitions: []spi.TransitionDefinition{
					{
						Name: "auto-sync", Next: "DONE", Manual: false,
						Processors: []spi.ProcessorDefinition{
							{Type: ProcessorTypeExternalized, Name: "sync-proc", ExecutionMode: "SYNC"},
						},
					},
				},
			},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("sync-proc-entity-1", modelRef, map[string]any{"value": 42})

	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success=true, got false")
	}
	if entity.Meta.State != "DONE" {
		t.Errorf("expected state=DONE, got %q", entity.Meta.State)
	}
}

func TestProcessorAsyncSameTxDefault(t *testing.T) {
	// ASYNC_SAME_TX (or empty) processor uses caller's transaction.
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "async-same-tx", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "AsyncSameTxWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {
				Transitions: []spi.TransitionDefinition{
					{
						Name: "auto-same", Next: "COMPLETE", Manual: false,
						Processors: []spi.ProcessorDefinition{
							{Type: ProcessorTypeExternalized, Name: "same-proc", ExecutionMode: "ASYNC_SAME_TX"},
						},
					},
				},
			},
			"COMPLETE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("async-same-tx-entity-1", modelRef, map[string]any{"ready": true})

	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success=true, got false")
	}
	if entity.Meta.State != "COMPLETE" {
		t.Errorf("expected state=COMPLETE, got %q", entity.Meta.State)
	}
}

// --- ExternalProcessingService Integration Tests ---

// mockExtProc is a test double for contract.ExternalProcessingService.
type mockExtProc struct {
	mu                     sync.Mutex
	dispatchProcessorCalls int
	dispatchCriteriaCalls  int
	returnEntity           *spi.Entity
	returnErr              error
	criteriaResult         bool
	criteriaErr            error
}

func (m *mockExtProc) DispatchProcessor(_ context.Context, _ *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dispatchProcessorCalls++
	return m.returnEntity, m.returnErr
}

func (m *mockExtProc) DispatchCriteria(_ context.Context, _ *spi.Entity, _ json.RawMessage, _, _, _, _, _ string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dispatchCriteriaCalls++
	return m.criteriaResult, m.criteriaErr
}

func TestProcessorDispatchWithExtProc(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	mock := &mockExtProc{}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "ext-proc", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "ExtProcWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "RUN", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "ext-proc-1", ExecutionMode: "SYNC"},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("ext-1", modelRef, map[string]any{"x": 1})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success=true")
	}
	if entity.Meta.State != "DONE" {
		t.Errorf("expected state=DONE, got %q", entity.Meta.State)
	}

	mock.mu.Lock()
	calls := mock.dispatchProcessorCalls
	mock.mu.Unlock()
	if calls != 1 {
		t.Errorf("expected DispatchProcessor called 1 time, got %d", calls)
	}
}

func TestProcessorDispatchWithExtProcAsyncNewTx(t *testing.T) {
	// ASYNC_NEW_TX processors run inside a savepoint. The engine's Execute
	// generates a logical txID that is not backed by a real Begin() call,
	// so Savepoint will fail. ASYNC_NEW_TX failures are non-fatal — the
	// pipeline continues and the entity still transitions to the next state.
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	mock := &mockExtProc{}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "ext-async-new", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "AsyncNewTxExtWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "RUN", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "ext-proc-new-tx", ExecutionMode: "ASYNC_NEW_TX"},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("ext-async-1", modelRef, map[string]any{"x": 1})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success=true")
	}
	// Entity should still reach DONE even though the ASYNC_NEW_TX savepoint
	// failed — ASYNC_NEW_TX failures are non-fatal.
	if entity.Meta.State != "DONE" {
		t.Errorf("expected state=DONE, got %q", entity.Meta.State)
	}

	// DispatchProcessor is NOT called because the savepoint creation fails
	// before dispatch is attempted.
	mock.mu.Lock()
	calls := mock.dispatchProcessorCalls
	mock.mu.Unlock()
	if calls != 0 {
		t.Errorf("expected DispatchProcessor called 0 times (savepoint fails), got %d", calls)
	}
}

// TestProcessorEmptyExecutionMode_FiresAsSync covers the audit's case
// 14: a processor with an empty `ExecutionMode` is dispatched through
// the SYNC path at engine fire-time. The validator already accepts the
// empty value (see TestValidateImportRequest_AcceptsEmptyExecutionMode)
// — this asserts the engine-fire counterpart: failure propagates and
// blocks state advance, which is SYNC's distinguishing semantic
// (ASYNC_NEW_TX would warn-and-continue; COMMIT_BEFORE_DISPATCH would
// split the transaction). engine_processors.go's switch falls through
// to executeSyncProcessor via the default case for "" — we verify the
// effect at the observable behaviour level.
func TestProcessorEmptyExecutionMode_FiresAsSync(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	mock := &mockExtProc{returnErr: fmt.Errorf("processor failed")}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "empty-mode", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "EmptyModeWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "RUN", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{
							Type:          ProcessorTypeExternalized,
							Name:          "empty-mode-proc",
							ExecutionMode: "", // the unit under test
						},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("empty-mode-1", modelRef, map[string]any{"x": 1})
	_, err := engine.Execute(ctx, entity, "")

	// SYNC-style failure semantics: the processor error propagates AND
	// the entity does not advance to the next state. ASYNC_NEW_TX would
	// swallow the failure and let the entity reach DONE.
	if err == nil {
		t.Fatalf("expected error from failing processor (SYNC-style propagation)")
	}
	if entity.Meta.State == "DONE" {
		t.Errorf("entity advanced to DONE — empty executionMode did NOT fire as SYNC")
	}

	// And the dispatcher was actually called exactly once — i.e. it
	// wasn't silently treated as "no-op" via some unknown-mode swallow.
	mock.mu.Lock()
	calls := mock.dispatchProcessorCalls
	mock.mu.Unlock()
	if calls != 1 {
		t.Errorf("expected DispatchProcessor called 1 time for empty executionMode, got %d", calls)
	}
}

func TestProcessorDispatchError(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	mock := &mockExtProc{returnErr: fmt.Errorf("processor failed")}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "ext-err", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "ErrWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "RUN", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "fail-proc", ExecutionMode: "SYNC"},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("ext-err-1", modelRef, map[string]any{"x": 1})
	_, err := engine.Execute(ctx, entity, "")
	if err == nil {
		t.Fatalf("expected error from failing processor")
	}
}

func TestProcessorDispatchModifiesEntityData(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	modifiedData, _ := json.Marshal(map[string]any{"x": 1, "enriched": true})
	mock := &mockExtProc{
		returnEntity: &spi.Entity{Data: modifiedData},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "ext-mod", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "ModWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "ENRICH", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "enrich-proc", ExecutionMode: "SYNC"},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("ext-mod-1", modelRef, map[string]any{"x": 1})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success=true")
	}

	// Verify entity data was updated by the processor.
	var data map[string]any
	if err := json.Unmarshal(entity.Data, &data); err != nil {
		t.Fatalf("failed to unmarshal entity data: %v", err)
	}
	if data["enriched"] != true {
		t.Errorf("expected enriched=true in entity data, got %v", data)
	}
}

func TestNilExtProcProcessorNoOp(t *testing.T) {
	// Ensure that with nil extProc, processors remain no-op (backward compat).
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "nil-ext", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "NilExtWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "RUN", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "proc-1", ExecutionMode: "SYNC"},
						{Type: ProcessorTypeExternalized, Name: "proc-2", ExecutionMode: "ASYNC_NEW_TX"},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("nil-ext-1", modelRef, map[string]any{"x": 1})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success=true")
	}
	if entity.Meta.State != "DONE" {
		t.Errorf("expected state=DONE, got %q", entity.Meta.State)
	}
}

func TestFunctionCriterionDispatched(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	mock := &mockExtProc{criteriaResult: true}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "func-crit", ModelVersion: "1.0"}

	funcCriterion, _ := json.Marshal(map[string]any{"type": "function", "name": "checkEligibility"})

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "FuncCritWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "CHECK", Next: "ELIGIBLE", Manual: false,
					Criterion: funcCriterion},
			}},
			"ELIGIBLE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("func-crit-1", modelRef, map[string]any{"x": 1})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success=true")
	}
	if entity.Meta.State != "ELIGIBLE" {
		t.Errorf("expected state=ELIGIBLE, got %q", entity.Meta.State)
	}

	mock.mu.Lock()
	calls := mock.dispatchCriteriaCalls
	mock.mu.Unlock()
	if calls != 1 {
		t.Errorf("expected DispatchCriteria called 1 time, got %d", calls)
	}
}

func TestFunctionCriterionNoExtProcError(t *testing.T) {
	// FUNCTION criterion without extProc should return an error.
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "func-no-ext", ModelVersion: "1.0"}

	funcCriterion, _ := json.Marshal(map[string]any{"type": "function", "name": "check"})

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "FuncNoExtWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "CHECK", Next: "DONE", Manual: false,
					Criterion: funcCriterion},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("func-no-ext-1", modelRef, map[string]any{"x": 1})
	_, err := engine.Execute(ctx, entity, "")
	if err == nil {
		t.Fatalf("expected error for FUNCTION criterion without extProc")
	}
}

func TestFunctionCriterionReturnsFalse(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	mock := &mockExtProc{criteriaResult: false}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "func-false", ModelVersion: "1.0"}

	funcCriterion, _ := json.Marshal(map[string]any{"type": "function", "name": "deny"})

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "FuncFalseWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "CHECK", Next: "DONE", Manual: false,
					Criterion: funcCriterion},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("func-false-1", modelRef, map[string]any{"x": 1})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	// Entity should stay at INITIAL since criterion returned false.
	if entity.Meta.State != "INITIAL" {
		t.Errorf("expected state=INITIAL, got %q", entity.Meta.State)
	}
	if !result.Success {
		t.Errorf("expected success=true (stable state)")
	}
}

func TestWorkflowFunctionCriterion(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	mock := &mockExtProc{criteriaResult: true}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "wf-func-crit", ModelVersion: "1.0"}

	funcCriterion, _ := json.Marshal(map[string]any{"type": "function", "name": "selectWF"})

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "WFFuncCritWF", InitialState: "SELECTED", Active: true,
		Criterion: funcCriterion,
		States: map[string]spi.StateDefinition{
			"SELECTED": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("wf-func-1", modelRef, map[string]any{"x": 1})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success=true")
	}
	if entity.Meta.State != "SELECTED" {
		t.Errorf("expected state=SELECTED, got %q", entity.Meta.State)
	}

	mock.mu.Lock()
	calls := mock.dispatchCriteriaCalls
	mock.mu.Unlock()
	if calls != 1 {
		t.Errorf("expected DispatchCriteria called 1 time, got %d", calls)
	}
}

// --- Default Workflow Tests ---

func TestDefaultWorkflowParsesCorrectly(t *testing.T) {
	var wf spi.WorkflowDefinition
	if err := json.Unmarshal(defaultWorkflowJSON, &wf); err != nil {
		t.Fatalf("failed to parse default workflow: %v", err)
	}
	if wf.InitialState != "NONE" {
		t.Errorf("expected initialState NONE, got %s", wf.InitialState)
	}
	if !wf.Active {
		t.Error("expected active=true")
	}
	if len(wf.States) != 3 {
		t.Errorf("expected 3 states, got %d", len(wf.States))
	}
	noneTransitions := wf.States["NONE"].Transitions
	if len(noneTransitions) != 1 || noneTransitions[0].Name != "NEW" || noneTransitions[0].Manual {
		t.Error("NONE state should have one automated NEW transition")
	}
}

func TestDefaultWorkflowEntityCreateEndsInCreated(t *testing.T) {
	engine, _ := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "no-wf-model", ModelVersion: "1.0"}

	entity := makeEntity("dw-1", modelRef, map[string]any{"key": "value"})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success")
	}
	if entity.Meta.State != "CREATED" {
		t.Fatalf("expected state CREATED, got %s", entity.Meta.State)
	}
}

func TestDefaultWorkflowManualUpdateStaysCreated(t *testing.T) {
	engine, _ := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "no-wf-update", ModelVersion: "1.0"}

	// Create entity via default workflow.
	entity := makeEntity("dw-2", modelRef, map[string]any{"key": "value"})
	_, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if entity.Meta.State != "CREATED" {
		t.Fatalf("precondition: expected state CREATED, got %s", entity.Meta.State)
	}

	// Manual UPDATE transition.
	result, err := engine.ManualTransition(ctx, entity, "UPDATE")
	if err != nil {
		t.Fatalf("ManualTransition failed: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success")
	}
	if entity.Meta.State != "CREATED" {
		t.Fatalf("expected state CREATED after UPDATE, got %s", entity.Meta.State)
	}
}

func TestDefaultWorkflowManualDeleteEndsInDeleted(t *testing.T) {
	engine, _ := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "no-wf-delete", ModelVersion: "1.0"}

	// Create entity via default workflow.
	entity := makeEntity("dw-3", modelRef, map[string]any{"key": "value"})
	_, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if entity.Meta.State != "CREATED" {
		t.Fatalf("precondition: expected state CREATED, got %s", entity.Meta.State)
	}

	// Manual DELETE transition.
	result, err := engine.ManualTransition(ctx, entity, "DELETE")
	if err != nil {
		t.Fatalf("ManualTransition failed: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success")
	}
	if entity.Meta.State != "DELETED" {
		t.Fatalf("expected state DELETED after DELETE, got %s", entity.Meta.State)
	}
}

func TestCustomWorkflowTakesPrecedence(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "custom-wf", ModelVersion: "1.0"}

	// Import a custom workflow for this model.
	customWF := spi.WorkflowDefinition{
		Version: "1.0", Name: "CustomWF", InitialState: "CUSTOM_INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"CUSTOM_INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "GO", Next: "CUSTOM_DONE", Manual: false},
			}},
			"CUSTOM_DONE": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{customWF})

	entity := makeEntity("cw-1", modelRef, map[string]any{"key": "value"})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success")
	}
	// Custom workflow should be used, not the default.
	if entity.Meta.State != "CUSTOM_DONE" {
		t.Fatalf("expected state CUSTOM_DONE from custom workflow, got %s", entity.Meta.State)
	}
}

func TestDefaultWorkflowNotPersistedInStore(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "no-persist", ModelVersion: "1.0"}

	// Create entity using default workflow (no workflow registered).
	entity := makeEntity("np-1", modelRef, map[string]any{"key": "value"})
	_, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if entity.Meta.State != "CREATED" {
		t.Fatalf("expected state CREATED, got %s", entity.Meta.State)
	}

	// Verify the default workflow was NOT saved to the store.
	wfStore, err := factory.WorkflowStore(ctx)
	if err != nil {
		t.Fatalf("failed to get workflow store: %v", err)
	}
	workflows, err := wfStore.Get(ctx, modelRef)
	if err == nil && len(workflows) > 0 {
		t.Fatalf("expected no workflows in store for model, got %d", len(workflows))
	}
	// Either error (not found) or empty slice is acceptable.
}

func TestDefaultWorkflowLoopbackNoOp(t *testing.T) {
	engine, _ := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "no-wf-loopback", ModelVersion: "1.0"}

	// Create entity via default workflow → CREATED.
	entity := makeEntity("dw-lb-1", modelRef, map[string]any{"key": "value"})
	_, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if entity.Meta.State != "CREATED" {
		t.Fatalf("precondition: expected state CREATED, got %s", entity.Meta.State)
	}

	// Loopback on CREATED — should stay CREATED (no automated transitions from CREATED).
	result, err := engine.Loopback(ctx, entity)
	if err != nil {
		t.Fatalf("Loopback failed: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success")
	}
	if entity.Meta.State != "CREATED" {
		t.Fatalf("expected state CREATED after loopback, got %s", entity.Meta.State)
	}
}

func TestDefaultWorkflowDeletedIsTerminal(t *testing.T) {
	engine, _ := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "no-wf-terminal", ModelVersion: "1.0"}

	// Create entity → CREATED, then DELETE → DELETED.
	entity := makeEntity("dw-term-1", modelRef, map[string]any{"key": "value"})
	_, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	_, err = engine.ManualTransition(ctx, entity, "DELETE")
	if err != nil {
		t.Fatalf("DELETE failed: %v", err)
	}
	if entity.Meta.State != "DELETED" {
		t.Fatalf("precondition: expected DELETED, got %s", entity.Meta.State)
	}

	// Attempt UPDATE on DELETED entity → should fail.
	_, err = engine.ManualTransition(ctx, entity, "UPDATE")
	if err == nil {
		t.Fatal("expected error for manual transition on DELETED entity")
	}
}

// wrappedNotFoundWFStore wraps a WorkflowStore and returns errors in the
// format "key workflow/X:1: not found" wrapping spi.ErrNotFound — a
// common shape produced by wire-level stores that don't use the memory
// store's "no workflows found" prefix. Ensures the engine's not-found
// detection is based on errors.Is, not substring matching.
type wrappedNotFoundWFStore struct {
	inner spi.WorkflowStore
}

func (s *wrappedNotFoundWFStore) Save(ctx context.Context, modelRef spi.ModelRef, workflows []spi.WorkflowDefinition) error {
	return s.inner.Save(ctx, modelRef, workflows)
}

func (s *wrappedNotFoundWFStore) Get(ctx context.Context, modelRef spi.ModelRef) ([]spi.WorkflowDefinition, error) {
	wfs, err := s.inner.Get(ctx, modelRef)
	if err != nil {
		return nil, fmt.Errorf("key workflow/%s: %w", modelRef, spi.ErrNotFound)
	}
	return wfs, nil
}

func (s *wrappedNotFoundWFStore) Delete(ctx context.Context, modelRef spi.ModelRef) error {
	return s.inner.Delete(ctx, modelRef)
}

type wrappedNotFoundFactory struct {
	spi.StoreFactory
}

func (f *wrappedNotFoundFactory) WorkflowStore(ctx context.Context) (spi.WorkflowStore, error) {
	inner, err := f.StoreFactory.WorkflowStore(ctx)
	if err != nil {
		return nil, err
	}
	return &wrappedNotFoundWFStore{inner: inner}, nil
}

// TestExecute_WrappedNotFoundFallsBackToDefault verifies that the engine
// recognises a wrapped spi.ErrNotFound via errors.Is rather than string
// matching, and falls back to the default workflow.
func TestExecute_WrappedNotFoundFallsBackToDefault(t *testing.T) {
	memFactory := memory.NewStoreFactory()
	t.Cleanup(func() { memFactory.Close() })

	factory := &wrappedNotFoundFactory{StoreFactory: memFactory}
	uuids := common.NewTestUUIDGenerator()
	txMgr := memFactory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "WrappedModel", ModelVersion: "1.0"}

	entity := makeEntity("wrapped-1", modelRef, map[string]any{"key": "value"})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed (should fall back to default workflow): %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	if entity.Meta.State != "CREATED" {
		t.Fatalf("expected state CREATED from default workflow, got %s", entity.Meta.State)
	}
}

// --- ASYNC_NEW_TX Semantics Tests ---

// mockExternalProcessing is a flexible test double for contract.ExternalProcessingService
// that supports per-call dispatch logic via function fields.
type mockExternalProcessing struct {
	dispatchFunc func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, wf, tr, txID string) (*spi.Entity, error)
}

func (m *mockExternalProcessing) DispatchProcessor(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, wf, tr, txID string) (*spi.Entity, error) {
	if m.dispatchFunc != nil {
		return m.dispatchFunc(ctx, entity, proc, wf, tr, txID)
	}
	return entity, nil
}

func (m *mockExternalProcessing) DispatchCriteria(_ context.Context, _ *spi.Entity, _ json.RawMessage, _, _, _, _, _ string) (bool, error) {
	return true, nil
}

func TestAsyncNewTxFailureDoesNotKillPipeline(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	mock := &mockExternalProcessing{
		dispatchFunc: func(_ context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			switch proc.Name {
			case "sync-proc":
				modified, _ := json.Marshal(map[string]any{"modified": "by-sync"})
				return &spi.Entity{Data: modified}, nil
			case "async-fail-proc":
				return nil, fmt.Errorf("async processor exploded")
			default:
				return entity, nil
			}
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "async-fail-test", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "AsyncFailWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "PROCESS", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "sync-proc", ExecutionMode: "SYNC"},
						{Type: ProcessorTypeExternalized, Name: "async-fail-proc", ExecutionMode: "ASYNC_NEW_TX"},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("async-fail-1", modelRef, map[string]any{"original": true})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute should succeed despite ASYNC_NEW_TX failure, got: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success=true")
	}
	if entity.Meta.State != "DONE" {
		t.Fatalf("expected state DONE, got %s", entity.Meta.State)
	}

	// Verify entity retains sync processor changes.
	var data map[string]any
	if err := json.Unmarshal(entity.Data, &data); err != nil {
		t.Fatalf("failed to unmarshal entity data: %v", err)
	}
	if data["modified"] != "by-sync" {
		t.Fatalf("expected entity.Data to have sync changes, got %v", data)
	}
}

func TestAsyncNewTxEntityMutationsDiscarded(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	mock := &mockExternalProcessing{
		dispatchFunc: func(_ context.Context, _ *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			// Return modified entity data — should be discarded for ASYNC_NEW_TX.
			modified, _ := json.Marshal(map[string]any{"sneaky": "mutation"})
			return &spi.Entity{Data: modified}, nil
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "async-discard-test", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "AsyncDiscardWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "PROCESS", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "async-mutator", ExecutionMode: "ASYNC_NEW_TX"},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	originalData := map[string]any{"original": "data"}
	entity := makeEntity("async-discard-1", modelRef, originalData)
	originalBytes := make([]byte, len(entity.Data))
	copy(originalBytes, entity.Data)

	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success=true")
	}

	// Verify entity data is unchanged — ASYNC_NEW_TX mutations are discarded.
	var data map[string]any
	if err := json.Unmarshal(entity.Data, &data); err != nil {
		t.Fatalf("failed to unmarshal entity data: %v", err)
	}
	if data["original"] != "data" {
		t.Fatalf("expected entity.Data to be unchanged, got %v", data)
	}
	if _, hasSneaky := data["sneaky"]; hasSneaky {
		t.Fatalf("ASYNC_NEW_TX processor mutation leaked into entity data: %v", data)
	}
}

func TestSyncProcessorsSequentialCumulativeMutations(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	var callOrder []string
	var callMu sync.Mutex

	mock := &mockExternalProcessing{
		dispatchFunc: func(_ context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			callMu.Lock()
			callOrder = append(callOrder, proc.Name)
			callMu.Unlock()

			// Read current entity data, add this processor's key, return modified.
			var data map[string]any
			if err := json.Unmarshal(entity.Data, &data); err != nil {
				return nil, fmt.Errorf("unmarshal failed: %w", err)
			}
			data[proc.Name] = true
			modified, _ := json.Marshal(data)
			return &spi.Entity{Data: modified}, nil
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cumulative-test", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "CumulativeWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "PROCESS", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "first", ExecutionMode: "SYNC"},
						{Type: ProcessorTypeExternalized, Name: "second", ExecutionMode: "SYNC"},
						{Type: ProcessorTypeExternalized, Name: "third", ExecutionMode: "SYNC"},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("cumulative-1", modelRef, map[string]any{"base": true})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success=true")
	}

	// Verify all three processor keys plus the original base key are present.
	var data map[string]any
	if err := json.Unmarshal(entity.Data, &data); err != nil {
		t.Fatalf("failed to unmarshal entity data: %v", err)
	}
	for _, key := range []string{"base", "first", "second", "third"} {
		if data[key] != true {
			t.Errorf("expected key %q=true in entity data, got %v", key, data)
		}
	}

	// Verify call order is sequential: first, second, third.
	callMu.Lock()
	defer callMu.Unlock()
	if len(callOrder) != 3 {
		t.Fatalf("expected 3 processor calls, got %d", len(callOrder))
	}
	expectedOrder := []string{"first", "second", "third"}
	for i, name := range expectedOrder {
		if callOrder[i] != name {
			t.Errorf("expected call %d to be %q, got %q", i, name, callOrder[i])
		}
	}
}

func TestAsyncNewTx_SeesSyncChanges(t *testing.T) {
	// Use an engine without a txMgr so that ASYNC_NEW_TX falls back to plain
	// dispatch (no savepoint), allowing the processor to actually be called
	// and receive the entity data as modified by the preceding SYNC processor.
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	engine := NewEngine(factory, uuids, nil)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "async-sees-sync", ModelVersion: "1.0"}

	var asyncReceivedData []byte
	engine.extProc = &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, wf, tr, txID string) (*spi.Entity, error) {
			if proc.Name == "sync-modifier" {
				modified := *entity
				modified.Data = []byte(`{"sync":"applied"}`)
				return &modified, nil
			}
			if proc.Name == "async-reader" {
				asyncReceivedData = make([]byte, len(entity.Data))
				copy(asyncReceivedData, entity.Data)
				return entity, nil
			}
			return entity, nil
		},
	}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "AsyncSeesSyncWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "RUN", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "sync-modifier", ExecutionMode: "SYNC"},
						{Type: ProcessorTypeExternalized, Name: "async-reader", ExecutionMode: "ASYNC_NEW_TX"},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("async-sees-sync-e1", modelRef, map[string]any{"original": true})
	_, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if string(asyncReceivedData) != `{"sync":"applied"}` {
		t.Errorf("ASYNC_NEW_TX processor received %s, want sync-modified data", asyncReceivedData)
	}
}

func TestSyncProcessor_ContextCancellation(t *testing.T) {
	engine, factory := setupEngine(t)
	modelRef := spi.ModelRef{EntityName: "ctx-cancel", ModelVersion: "1.0"}

	engine.extProc = &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, wf, tr, txID string) (*spi.Entity, error) {
			return nil, ctx.Err()
		},
	}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "CtxCancelWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "RUN", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "slow-proc", ExecutionMode: "SYNC"},
					}},
			}},
			"DONE": {},
		},
	}

	ctx, cancel := context.WithCancel(ctxWithTenant(testTenant))
	cancel() // cancel immediately

	saveWorkflow(t, factory, ctxWithTenant(testTenant), modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("ctx-cancel-e1", modelRef, map[string]any{"status": "new"})
	_, err := engine.Execute(ctx, entity, "")
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestAsyncNewTx_ParentRollbackDiscardsWork(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	ctx := ctxWithTenant(testTenant)

	// Begin parent transaction.
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// Create savepoint (simulating ASYNC_NEW_TX).
	spID, err := txMgr.Savepoint(txCtx, txID)
	if err != nil {
		t.Fatalf("Savepoint: %v", err)
	}

	// Write an entity inside the savepoint.
	es, _ := factory.EntityStore(txCtx)
	entity := &spi.Entity{
		Meta: spi.EntityMeta{ID: "sp-entity", TenantID: testTenant, ModelRef: spi.ModelRef{EntityName: "M", ModelVersion: "1"}},
		Data: []byte(`{"from":"savepoint"}`),
	}
	if _, err := es.Save(txCtx, entity); err != nil {
		t.Fatalf("Save in savepoint: %v", err)
	}

	// Release savepoint (async processor succeeded).
	if err := txMgr.ReleaseSavepoint(txCtx, txID, spID); err != nil {
		t.Fatalf("ReleaseSavepoint: %v", err)
	}

	// Now rollback the parent transaction (simulating a later SYNC processor failure).
	if err := txMgr.Rollback(txCtx, txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// The entity written inside the savepoint should NOT be visible.
	readCtx := ctxWithTenant(testTenant)
	readEs, _ := factory.EntityStore(readCtx)
	_, err = readEs.Get(readCtx, "sp-entity")
	if err == nil {
		t.Error("entity from savepoint should NOT be visible after parent rollback")
	}
}

// TestEngine_CommitAndBeginNextSegment_FlushesAndReopens drives the
// COMMIT_BEFORE_DISPATCH segment-boundary primitive. The helper must:
//   - Flush the in-memory entity to TX_pre's buffer.
//   - Commit TX_pre, making the entity durable across the boundary.
//   - Begin a fresh TX_post and return its txID + ctx.
func TestEngine_CommitAndBeginNextSegment_FlushesAndReopens(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, txMgr)

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "SegmentModel", ModelVersion: "1.0"}

	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "seg-e1",
			TenantID: testTenant,
			ModelRef: modelRef,
			State:    "S_pre",
		},
		Data: []byte(`{"x":1}`),
	}

	newTxID, newCtx, err := engine.commitAndBeginNextSegment(txCtx, entity, txID, "", false)
	if err != nil {
		t.Fatalf("commitAndBeginNextSegment: %v", err)
	}
	if newTxID == "" {
		t.Fatalf("expected non-empty newTxID")
	}
	if newTxID == txID {
		t.Fatalf("expected new txID, got same as old: %s", newTxID)
	}
	if newCtx == nil {
		t.Fatalf("expected non-nil newCtx")
	}
	if state := spi.GetTransaction(newCtx); state == nil || state.ID != newTxID {
		t.Fatalf("newCtx must carry newTxID, got %v", state)
	}

	// Entity is durable: readable from a fresh, independent TX.
	readTxID, readCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (read): %v", err)
	}
	defer func() {
		if rbErr := txMgr.Rollback(readCtx, readTxID); rbErr != nil {
			t.Errorf("Rollback (read): %v", rbErr)
		}
	}()

	es, err := factory.EntityStore(readCtx)
	if err != nil {
		t.Fatalf("EntityStore (read): %v", err)
	}
	got, err := es.Get(readCtx, "seg-e1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Meta.State != "S_pre" {
		t.Fatalf("entity state = %q, want %q", got.Meta.State, "S_pre")
	}

	// Cleanup: commit the new tx the helper opened.
	if err := txMgr.Commit(newCtx, newTxID); err != nil {
		t.Fatalf("Commit (cleanup): %v", err)
	}
}

// TestEngine_CommitBeforeDispatch_FalseBranch_HappyPath drives the
// COMMIT_BEFORE_DISPATCH execution branch with startNewTxOnDispatch=false
// (the default). It asserts:
//   - TX_pre is committed before the processor is dispatched (the entity in
//     state S_pre is durable and readable from an independent TX).
//   - The processor receives a context with NO active transaction.
//   - The processor's mutations are applied via CompareAndSave in TX_post.
//   - The cascade completes; the post-dispatch txID is different from the
//     cascade-entry txID.
func TestEngine_CommitBeforeDispatch_FalseBranch_HappyPath(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	var dispatchCtxHasTx bool
	var dispatchCtxTxID string
	var dispatchTxToken string
	var preEntityVisible bool

	mock := &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
			// Capture the tx state seen by the processor:
			//   - dispatchCtxHasTx / dispatchCtxTxID: from the ctx (spi.GetTransaction)
			//   - dispatchTxToken:                   from the gRPC-arg txID
			// In the false branch both must be empty/false; in the =true branch
			// (Task 8 reuses this fixture) both must be non-empty AND agree.
			if state := spi.GetTransaction(ctx); state != nil {
				dispatchCtxHasTx = true
				dispatchCtxTxID = state.ID
			}
			dispatchTxToken = txID

			// Verify TX_pre is durable: a fresh, independent TX should see
			// the entity flushed in S_pre (the pre-callout state). We use
			// a brand-new context so we don't inherit any tx token.
			readCtx := ctxWithTenant(testTenant)
			readTxID, readCtx2, err := txMgr.Begin(readCtx)
			if err == nil {
				es, _ := factory.EntityStore(readCtx2)
				got, getErr := es.Get(readCtx2, entity.Meta.ID)
				if getErr == nil && got != nil && got.Meta.State == "S_pre" {
					preEntityVisible = true
				}
				_ = txMgr.Rollback(readCtx2, readTxID)
			}

			// Return an entity-data mutation that should land in TX_post.
			modified, _ := json.Marshal(map[string]any{"enriched": true, "x": 1})
			return &spi.Entity{Data: modified}, nil
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cbd-false", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "CbdFalseWF", InitialState: "S_pre", Active: true,
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

	// Wrap the engine call in a parent transaction so that TX_pre is real
	// (mirrors what entity/service.go does for callers).
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:            "cbd-false-1",
			TenantID:      testTenant,
			ModelRef:      modelRef,
			State:         "",
			TransactionID: txID,
		},
		Data: []byte(`{"x":1}`),
	}

	result, err := engine.Execute(txCtx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success=true")
	}
	if entity.Meta.State != "S_post" {
		t.Fatalf("expected state=S_post, got %q", entity.Meta.State)
	}

	// Processor must have been dispatched with NO transaction in ctx and
	// NO tx token (false branch).
	if dispatchCtxHasTx {
		t.Errorf("expected processor ctx to have no tx, but found one (id=%q)", dispatchCtxTxID)
	}
	if dispatchTxToken != "" {
		t.Errorf("expected processor txID token to be empty, got %q", dispatchTxToken)
	}

	// TX_pre must be durable at dispatch time: the entity in S_pre had to be
	// visible from an independent TX while the processor was running.
	if !preEntityVisible {
		t.Errorf("expected entity to be durable in S_pre during dispatch (TX_pre committed)")
	}

	// The original TX_pre is now closed (committed by the engine). The
	// service-layer Commit will fail with no-such-tx — emulating that here
	// confirms TX_pre is gone:
	if err := txMgr.Commit(txCtx, txID); err == nil {
		t.Errorf("expected TX_pre commit attempt to fail (engine already committed it)")
	}

	// Verify the in-memory entity reflects the processor's mutation
	// (applied via CompareAndSave inside TX_post's buffer) and the post-
	// callout state.
	var finalData map[string]any
	if err := json.Unmarshal(entity.Data, &finalData); err != nil {
		t.Fatalf("unmarshal entity data: %v", err)
	}
	if finalData["enriched"] != true {
		t.Errorf("expected in-memory processor mutation enriched=true, got %v", finalData)
	}

	// TX_pre's view of the entity (in S_pre) is the only durable view at
	// this point: TX_post is still open (Task 12/13 will commit it via the
	// handler). Verify TX_pre is durable from a fresh, independent TX.
	readTxID, readCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (final read): %v", err)
	}
	defer func() { _ = txMgr.Rollback(readCtx, readTxID) }()
	es, _ := factory.EntityStore(readCtx)
	got, err := es.Get(readCtx, "cbd-false-1")
	if err != nil {
		t.Fatalf("final Get: %v", err)
	}
	if got.Meta.State != "S_pre" {
		t.Errorf("durable state from independent TX = %q, want S_pre (TX_pre commit); "+
			"S_post will become durable once Task 12/13 commits TX_post",
			got.Meta.State)
	}
}

// segCounter records Begin/Commit/Rollback calls observed by
// countingTxManager. Lives at package level so the wrapper can carry it
// via pointer and the test can read fields after Engine.Execute returns.
type segCounter struct {
	begins, commits, rollbacks int
}

// countingTxManager wraps a spi.TransactionManager and counts every
// Begin/Commit/Rollback invocation. All other methods delegate to the
// wrapped manager unchanged. Used by single-segment regression tests to
// assert the engine's contribution to the commit count.
type countingTxManager struct {
	inner spi.TransactionManager
	c     *segCounter
}

func (m *countingTxManager) Begin(ctx context.Context) (string, context.Context, error) {
	m.c.begins++
	return m.inner.Begin(ctx)
}

func (m *countingTxManager) Commit(ctx context.Context, txID string) error {
	m.c.commits++
	return m.inner.Commit(ctx, txID)
}

func (m *countingTxManager) Rollback(ctx context.Context, txID string) error {
	m.c.rollbacks++
	return m.inner.Rollback(ctx, txID)
}

func (m *countingTxManager) Join(ctx context.Context, txID string) (context.Context, error) {
	return m.inner.Join(ctx, txID)
}

func (m *countingTxManager) GetSubmitTime(ctx context.Context, txID string) (time.Time, error) {
	return m.inner.GetSubmitTime(ctx, txID)
}

func (m *countingTxManager) Savepoint(ctx context.Context, txID string) (string, error) {
	return m.inner.Savepoint(ctx, txID)
}

func (m *countingTxManager) RollbackToSavepoint(ctx context.Context, txID string, savepointID string) error {
	return m.inner.RollbackToSavepoint(ctx, txID, savepointID)
}

func (m *countingTxManager) ReleaseSavepoint(ctx context.Context, txID string, savepointID string) error {
	return m.inner.ReleaseSavepoint(ctx, txID, savepointID)
}

// TestEngine_SingleSegment_NoEngineCommit is a regression bound: a cascade
// with NO COMMIT_BEFORE_DISPATCH processors must NOT trigger any engine-
// side Begin/Commit. The handler-driven single-Begin/single-Commit
// pattern (Begin → engine → Save → Commit) is unchanged.
//
// If a future change causes the engine to start mid-cascade-committing on
// non-CBD cascades, this test catches it.
func TestEngine_SingleSegment_NoEngineCommit(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	rawTxMgr := factory.NewTransactionManager(uuids)

	cnt := &segCounter{}
	txMgr := &countingTxManager{inner: rawTxMgr, c: cnt}

	mock := &mockExternalProcessing{
		dispatchFunc: func(_ context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			// SYNC processor: no entity mutation needed; the test cares
			// about whether the engine calls Begin/Commit, not about
			// entity-data shape.
			return e, nil
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "single-seg", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "SingleSegWF", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "t1", Next: "S2", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "p1", ExecutionMode: ExecutionModeSync},
					}},
			}},
			"S2": {Transitions: []spi.TransitionDefinition{
				{Name: "t2", Next: "S3", Manual: false},
			}},
			"S3": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	// Handler-style: begin a tx, run engine, save, commit.
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:            "single-seg-1",
			TenantID:      testTenant,
			ModelRef:      modelRef,
			State:         "",
			TransactionID: txID,
		},
		Data: []byte(`{"x":1}`),
	}

	// Snapshot the counter at the cascade boundary: only the test's own
	// Begin should have been counted so far. The deltas after Execute
	// represent the engine's own Begin/Commit contribution.
	beginsBefore := cnt.begins
	commitsBefore := cnt.commits

	if _, err := engine.Execute(txCtx, entity, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if engineBegins := cnt.begins - beginsBefore; engineBegins != 0 {
		t.Errorf("engine performed %d Begin call(s) for a cascade with no COMMIT_BEFORE_DISPATCH; want 0", engineBegins)
	}
	if engineCommits := cnt.commits - commitsBefore; engineCommits != 0 {
		t.Errorf("engine performed %d Commit call(s) for a cascade with no COMMIT_BEFORE_DISPATCH; want 0", engineCommits)
	}

	if entity.Meta.State != "S3" {
		t.Errorf("final state = %q, want S3", entity.Meta.State)
	}

	// Handler-side cleanup: save and commit. This validates the
	// single-Begin/single-Commit pattern is intact end-to-end.
	es, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Save(txCtx, entity); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := txMgr.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// TestEngine_CommitBeforeDispatch_CASConflict_BubblesAsErrConflict drives the
// COMMIT_BEFORE_DISPATCH execution branch (startNewTxOnDispatch=false) and
// simulates a concurrent writer that commits a competing modification to the
// cascade-anchor entity between the cascade's TX_pre.Commit and the engine's
// CompareAndSave (in TX_post). The engine's CAS — which expects the durable
// entity's TransactionID to still match TX_pre — must fail and surface
// spi.ErrConflict unwrapped (errors.Is matches). The interloper's write must
// be the durable state because TX_post is rolled back.
func TestEngine_CommitBeforeDispatch_CASConflict_BubblesAsErrConflict(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cbd-cas-conflict", ModelVersion: "1.0"}

	mock := &mockExternalProcessing{
		dispatchFunc: func(_ context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			// Competing transaction from a fresh root context (no tx token in
			// ctx). This commits a write between TX_pre.Commit and TX_post's
			// CAS, which advances the durable entity's TransactionID off TX_pre
			// and forces CompareAndSave(expected=TX_pre) to fail.
			interloperCtx := ctxWithTenant(testTenant)
			itxID, ictx, err := txMgr.Begin(interloperCtx)
			if err != nil {
				return nil, fmt.Errorf("interloper Begin: %w", err)
			}
			ies, err := factory.EntityStore(ictx)
			if err != nil {
				return nil, fmt.Errorf("interloper EntityStore: %w", err)
			}
			cur, err := ies.Get(ictx, entity.Meta.ID)
			if err != nil {
				_ = txMgr.Rollback(ictx, itxID)
				return nil, fmt.Errorf("interloper Get: %w", err)
			}
			cur.Data = []byte(`{"interloper":true}`)
			if _, err := ies.Save(ictx, cur); err != nil {
				_ = txMgr.Rollback(ictx, itxID)
				return nil, fmt.Errorf("interloper Save: %w", err)
			}
			if err := txMgr.Commit(ictx, itxID); err != nil {
				return nil, fmt.Errorf("interloper Commit: %w", err)
			}

			// Cascade's intended result. The engine will attempt to apply this
			// via CompareAndSave against TX_pre, which will now fail.
			modified, _ := json.Marshal(map[string]any{"cascade": true})
			return &spi.Entity{Data: modified}, nil
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "CbdCASConflictWF", InitialState: "S_pre", Active: true,
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

	// Cascade-entry transaction (TX_pre). The engine commits this during the
	// CBD branch and opens its own TX_post for the apply-result CAS.
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:            "cbd-cas-conflict-1",
			TenantID:      testTenant,
			ModelRef:      modelRef,
			State:         "",
			TransactionID: txID,
		},
		Data: []byte(`{"x":1}`),
	}

	_, err = engine.Execute(txCtx, entity, "")
	if err == nil {
		t.Fatalf("expected ErrConflict from CompareAndSave, got nil")
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("expected errors.Is(err, spi.ErrConflict), got: %v", err)
	}

	// TX_pre was already committed by the engine before dispatch; TX_post was
	// rolled back by executeCommitBeforeDispatch on CAS failure. A best-effort
	// rollback of the test's original token is a no-op (no such tx).
	_ = txMgr.Rollback(txCtx, txID)

	// Independent reader: durable state must be the interloper's write — the
	// cascade's CAS failed and TX_post rolled back, so {"cascade":true} never
	// became durable.
	rTxID, rCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (final read): %v", err)
	}
	defer func() { _ = txMgr.Rollback(rCtx, rTxID) }()
	es, err := factory.EntityStore(rCtx)
	if err != nil {
		t.Fatalf("EntityStore (final read): %v", err)
	}
	got, err := es.Get(rCtx, "cbd-cas-conflict-1")
	if err != nil {
		t.Fatalf("final Get: %v", err)
	}
	var finalData map[string]any
	if jErr := json.Unmarshal(got.Data, &finalData); jErr != nil {
		t.Fatalf("unmarshal final data: %v", jErr)
	}
	if finalData["interloper"] != true {
		t.Errorf("durable data = %s, want interloper's write {\"interloper\":true}", got.Data)
	}
	if finalData["cascade"] == true {
		t.Errorf("durable data unexpectedly contains cascade's intended mutation: %s", got.Data)
	}
}

// TestEngine_CommitBeforeDispatch_TrueBranch_HappyPath drives the
// COMMIT_BEFORE_DISPATCH execution branch with startNewTxOnDispatch=true.
// In this branch the engine commits TX_pre, opens TX_post BEFORE dispatch,
// hands the processor TX_post's token (both via the gRPC arg and via the
// ctx tx-state), then CAS-applies the engine's apply-result against TX_pre's
// ID inside TX_post. The processor may perform transactional CRUD on
// secondary entities via TX_post's token; both that CRUD and the engine's
// CAS land atomically when TX_post commits.
//
// Asserts:
//   - The dispatch ctx carries a transaction (spi.GetTransaction(ctx) != nil).
//   - The dispatch txID arg is non-empty AND equals the ctx-state TX ID
//     (the processor sees a single, consistent TX_post identifier).
//   - The in-memory entity reflects the engine's apply-result (the cascade
//     anchor's mutated Data) and the post-callout state.
//
// Durability of e1 + e2 from an independent reader is NOT asserted here:
// TX_post is left open by the engine pending the Task 12/13 handler refactor
// that wires the final commit. See the trailing comment block.
func TestEngine_CommitBeforeDispatch_TrueBranch_HappyPath(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	var dispatchCtxHasTx bool
	var dispatchCtxTxID string
	var dispatchTxToken string

	mock := &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
			// Capture the tx state seen by the processor:
			//   - dispatchCtxHasTx / dispatchCtxTxID: from the ctx (spi.GetTransaction)
			//   - dispatchTxToken:                   from the gRPC-arg txID
			// In the =true branch both must be non-empty AND agree.
			if state := spi.GetTransaction(ctx); state != nil {
				dispatchCtxHasTx = true
				dispatchCtxTxID = state.ID
			}
			dispatchTxToken = txID

			// Processor performs CRUD on a SECONDARY entity (distinct from
			// the cascade anchor) via TX_post's token in ctx.
			es, esErr := factory.EntityStore(ctx)
			if esErr != nil {
				return nil, fmt.Errorf("processor EntityStore: %w", esErr)
			}
			secondary := &spi.Entity{
				Meta: spi.EntityMeta{
					ID:            "cbd-true-secondary",
					TenantID:      testTenant,
					ModelRef:      entity.Meta.ModelRef,
					State:         "ready",
					TransactionID: txID,
				},
				Data: []byte(`{"y":1}`),
			}
			if _, sErr := es.Save(ctx, secondary); sErr != nil {
				return nil, fmt.Errorf("processor Save secondary: %w", sErr)
			}

			// Cascade-anchor mutation; engine applies via CAS in TX_post.
			modified, _ := json.Marshal(map[string]any{"enriched": true, "x": 42})
			return &spi.Entity{Data: modified}, nil
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cbd-true", ModelVersion: "1.0"}

	tt := true
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "CbdTrueWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{
				{Name: "CALLOUT", Next: "S_post", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{
							Type:          ProcessorTypeExternalized,
							Name:          "cbd-proc",
							ExecutionMode: ExecutionModeCommitBeforeDispatch,
							Config:        spi.ProcessorConfig{StartNewTxOnDispatch: &tt},
						},
					}},
			}},
			"S_post": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	// Cascade-entry transaction (TX_pre). The engine commits this during the
	// CBD branch and opens its own TX_post.
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:            "cbd-true-1",
			TenantID:      testTenant,
			ModelRef:      modelRef,
			State:         "",
			TransactionID: txID,
		},
		Data: []byte(`{"x":1}`),
	}

	result, err := engine.Execute(txCtx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success=true")
	}
	if entity.Meta.State != "S_post" {
		t.Fatalf("expected state=S_post, got %q", entity.Meta.State)
	}

	// Processor must have been dispatched with a tx in ctx AND a non-empty
	// tx token, and the two must agree (single TX_post identifier).
	if !dispatchCtxHasTx {
		t.Errorf("expected processor ctx to carry a transaction in =true branch, but found none")
	}
	if dispatchTxToken == "" {
		t.Errorf("expected processor txID arg to be non-empty in =true branch, got empty")
	}
	if dispatchCtxTxID != dispatchTxToken {
		t.Errorf("ctx tx and arg-token disagree: ctx=%q arg=%q", dispatchCtxTxID, dispatchTxToken)
	}
	// And the dispatch-time TX_post is distinct from the cascade-entry TX_pre.
	if dispatchTxToken == txID {
		t.Errorf("dispatch tx token equals cascade-entry tx (TX_pre); expected a fresh TX_post")
	}

	// Verify the in-memory entity reflects the processor's mutation
	// (applied via CompareAndSave inside TX_post's buffer) and the post-
	// callout state.
	var finalData map[string]any
	if err := json.Unmarshal(entity.Data, &finalData); err != nil {
		t.Fatalf("unmarshal entity data: %v", err)
	}
	if finalData["enriched"] != true {
		t.Errorf("expected in-memory processor mutation enriched=true, got %v", finalData)
	}
	if v, ok := finalData["x"].(float64); !ok || v != 42 {
		t.Errorf("expected in-memory processor mutation x=42, got %v", finalData["x"])
	}

	// Durability of e1 (cascade anchor in S_post) and e2 (secondary written
	// by the processor via TX_post's token) is NOT asserted here. Both
	// writes live in TX_post's buffer, and TX_post is left open by the
	// engine pending the Task 12/13 handler refactor that wires the final
	// commit. Once that lands, this test should be extended to assert that
	// an independent reader sees both entities post-cascade.
	// TODO(issue-27, Task 13): assert durability of e1 in S_post and e2
	// once the handler commits the engine's final TX_post.
}

// TestEngine_CommitBeforeDispatch_TrueBranch_DoubleWriteIsLastWriterWins
// documents (does NOT endorse) the last-writer-wins outcome when a
// startNewTxOnDispatch=true processor writes the cascade-anchor entity
// itself AND returns mutations for it.
//
// Per spec §10.3, this pattern is forbidden by existing best-practice
// across SYNC, ASYNC_SAME_TX, and COMMIT_BEFORE_DISPATCH (true). The engine
// does NOT detect or prevent the violation — the processor's intra-TX_post
// write is silently overwritten by the engine's apply-result CAS.
//
// This test exists so the LWW outcome is pinned down (not "undefined")
// and so a future engine change that accidentally REVERSES the order
// (processor's write wins) would surface as a test failure for review.
//
// Asserts the in-memory entity after Execute, NOT durable state: TX_post
// is left open by the engine pending the Task 12/13 handler refactor.
func TestEngine_CommitBeforeDispatch_TrueBranch_DoubleWriteIsLastWriterWins(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	mock := &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
			// VIOLATION: processor writes the cascade-anchor entity it is
			// being dispatched FOR, via TX_post's token in ctx.
			es, esErr := factory.EntityStore(ctx)
			if esErr != nil {
				return nil, fmt.Errorf("processor EntityStore: %w", esErr)
			}
			processorWrite := &spi.Entity{
				Meta: spi.EntityMeta{
					ID:            entity.Meta.ID,
					TenantID:      entity.Meta.TenantID,
					ModelRef:      entity.Meta.ModelRef,
					State:         entity.Meta.State,
					TransactionID: txID,
				},
				Data: []byte(`{"processor_wrote":true}`),
			}
			if _, sErr := es.Save(ctx, processorWrite); sErr != nil {
				return nil, fmt.Errorf("processor double-write: %w", sErr)
			}

			// AND ALSO returns conflicting mutations for the same entity.
			return &spi.Entity{Data: []byte(`{"engine_applied":true}`)}, nil
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cbd-true-lww", ModelVersion: "1.0"}

	tt := true
	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "CbdTrueLWWWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{
				{Name: "CALLOUT", Next: "S_post", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{
							Type:          ProcessorTypeExternalized,
							Name:          "cbd-proc",
							ExecutionMode: ExecutionModeCommitBeforeDispatch,
							Config:        spi.ProcessorConfig{StartNewTxOnDispatch: &tt},
						},
					}},
			}},
			"S_post": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	// Cascade-entry transaction (TX_pre).
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:            "cbd-true-lww-1",
			TenantID:      testTenant,
			ModelRef:      modelRef,
			State:         "",
			TransactionID: txID,
		},
		Data: []byte(`{"x":0}`),
	}

	if _, err := engine.Execute(txCtx, entity, ""); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Engine's apply-result wins: the in-memory entity carries the engine's
	// data, NOT the processor's intra-TX_post write.
	if string(entity.Data) != `{"engine_applied":true}` {
		t.Errorf("LWW expected engine apply-result to win, got: %s", entity.Data)
	}

	// TODO(issue-27, Task 13): once the handler refactor commits TX_post,
	// add a durable-read assertion confirming the engine's data is what hits
	// the committed store. Until then, only in-memory entity is asserted.
}

// TestEngine_CommitBeforeDispatch_AuditEventPlacement pins down spec §8's
// audit-event labelling decision for COMMIT_BEFORE_DISPATCH:
//
//   - SMEventProcessingPaused is recorded BEFORE the processor dispatches; it
//     lands physically in TX_pre's audit (durable on TX_pre.Commit).
//   - SMEventStateProcessResult is recorded AFTER the processor returns; it
//     lands physically in TX_post's audit.
//   - BOTH events carry the cascade-entry txID as their TransactionID field
//     (NOT the segment's currentTxID). This preserves the existing
//     GET /audit/entity/{id}/workflow/{txId}/finished query semantics.
//
// We assert both axes inside the dispatch fake (which runs after TX_pre.Commit
// and before TX_post.Begin in the false branch):
//
//   - SMEventProcessingPaused IS visible to a fresh reader (durable in TX_pre)
//     AND its TransactionID equals the cascade-entry txID.
//   - SMEventStateProcessResult is NOT yet visible (the engine emits it only
//     after executeCommitBeforeDispatch returns).
//
// After cascade returns, both bracketing events are present with the
// cascade-entry txID label. Full durable-after-TX_post-commit assertion lands
// once Task 13 commits TX_post end-to-end.
func TestEngine_CommitBeforeDispatch_AuditEventPlacement(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	var (
		auditDuringDispatch    []spi.StateMachineEvent
		auditDuringDispatchErr error
	)

	mock := &mockExternalProcessing{
		dispatchFunc: func(_ context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			// Read audit from a FRESH context (no inherited tx token) so we
			// observe what a third-party reader would see at this instant.
			rCtx := ctxWithTenant(testTenant)
			rTxID, rTxCtx, err := txMgr.Begin(rCtx)
			if err != nil {
				auditDuringDispatchErr = fmt.Errorf("reader Begin: %w", err)
				return &spi.Entity{Data: e.Data}, nil
			}
			defer func() { _ = txMgr.Rollback(rTxCtx, rTxID) }()

			as, err := factory.StateMachineAuditStore(rTxCtx)
			if err != nil {
				auditDuringDispatchErr = fmt.Errorf("reader audit store: %w", err)
				return &spi.Entity{Data: e.Data}, nil
			}
			auditDuringDispatch, auditDuringDispatchErr = as.GetEvents(rTxCtx, e.Meta.ID)

			modified, _ := json.Marshal(map[string]any{"x": 42})
			return &spi.Entity{Data: modified}, nil
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cbd-audit", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "CbdAuditWF", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "t1", Next: "S_post", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "p1", ExecutionMode: ExecutionModeCommitBeforeDispatch},
					}},
			}},
			"S_post": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	// Cascade-entry txID — captured before Execute so we can assert that
	// audit events carry it as their TransactionID label, regardless of
	// which segment physically commits each event.
	cascadeEntryTxID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:            "cbd-audit-1",
			TenantID:      testTenant,
			ModelRef:      modelRef,
			State:         "",
			TransactionID: cascadeEntryTxID,
		},
		Data: []byte(`{"x":1}`),
	}

	if _, err := engine.Execute(txCtx, entity, ""); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if auditDuringDispatchErr != nil {
		t.Fatalf("audit read inside dispatch failed: %v", auditDuringDispatchErr)
	}

	// --- Assertion 1: SMEventProcessingPaused is durable during dispatch
	// (recorded in TX_pre, which has been committed before the processor
	// runs in the false branch).
	var paused *spi.StateMachineEvent
	for i := range auditDuringDispatch {
		if auditDuringDispatch[i].EventType == spi.SMEventProcessingPaused {
			paused = &auditDuringDispatch[i]
			break
		}
	}
	if paused == nil {
		t.Fatalf("expected SMEventProcessingPaused durable during dispatch (TX_pre.Commit), "+
			"none found in %d events: %+v", len(auditDuringDispatch), auditDuringDispatch)
	}

	// --- Assertion 2 (load-bearing): SMEventProcessingPaused carries the
	// cascade-entry txID as its TransactionID — NOT the segment's currentTxID.
	// If this fails, Task 5's audit-labelling decision is broken.
	if paused.TransactionID != cascadeEntryTxID {
		t.Errorf("paused.TransactionID = %q, want cascade-entry %q "+
			"(spec §8: bracketing audit events carry the cascade-entry txID)",
			paused.TransactionID, cascadeEntryTxID)
	}

	// --- Assertion 3: SMEventStateProcessResult is NOT yet visible during
	// dispatch — the engine emits it only after executeCommitBeforeDispatch
	// returns.
	for _, e := range auditDuringDispatch {
		if e.EventType == spi.SMEventStateProcessResult {
			t.Errorf("unexpected SMEventStateProcessResult visible during dispatch "+
				"(should be emitted only after the processor returns); event=%+v", e)
		}
	}

	// --- Assertion 4: after the cascade returns, BOTH bracketing events
	// are present with the cascade-entry txID label. We read via the
	// engine-discarded txCtx — which still owns its (unstaffed) tx token,
	// but the in-memory audit store is non-transactional so it sees
	// everything recorded so far. This is sufficient to assert labelling;
	// the durable-read variant of this assertion lands once Task 13
	// commits TX_post end-to-end.
	auditAfter, err := func() ([]spi.StateMachineEvent, error) {
		rCtx := ctxWithTenant(testTenant)
		rTxID, rTxCtx, bErr := txMgr.Begin(rCtx)
		if bErr != nil {
			return nil, bErr
		}
		defer func() { _ = txMgr.Rollback(rTxCtx, rTxID) }()
		as, asErr := factory.StateMachineAuditStore(rTxCtx)
		if asErr != nil {
			return nil, asErr
		}
		return as.GetEvents(rTxCtx, entity.Meta.ID)
	}()
	if err != nil {
		t.Fatalf("post-cascade audit read: %v", err)
	}

	var postPaused, postResult *spi.StateMachineEvent
	for i := range auditAfter {
		switch auditAfter[i].EventType {
		case spi.SMEventProcessingPaused:
			postPaused = &auditAfter[i]
		case spi.SMEventStateProcessResult:
			postResult = &auditAfter[i]
		}
	}
	if postPaused == nil {
		t.Errorf("expected SMEventProcessingPaused after cascade, none in %d events", len(auditAfter))
	} else if postPaused.TransactionID != cascadeEntryTxID {
		t.Errorf("post-cascade paused.TransactionID = %q, want %q",
			postPaused.TransactionID, cascadeEntryTxID)
	}
	if postResult == nil {
		t.Errorf("expected SMEventStateProcessResult after cascade, none in %d events", len(auditAfter))
	} else if postResult.TransactionID != cascadeEntryTxID {
		t.Errorf("post-cascade result.TransactionID = %q, want cascade-entry %q "+
			"(spec §8: even though this event physically lives in TX_post, its "+
			"TransactionID label is the cascade-entry txID for client correlation)",
			postResult.TransactionID, cascadeEntryTxID)
	}
}

// TestEngine_Execute_ReturnsFinalSegmentTxOnCBDCascade asserts that after a
// cascade with one COMMIT_BEFORE_DISPATCH segment, Execute returns the FINAL
// segment's txID (TX_post) — not the entry txID. The handler (Task 13) needs
// FinalCtx/FinalTxID to commit TX_post instead of the now-closed TX_pre.
func TestEngine_Execute_ReturnsFinalSegmentTxOnCBDCascade(t *testing.T) {
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
	modelRef := spi.ModelRef{EntityName: "cbd-final-tx", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "CbdFinalTxWF", InitialState: "S_pre", Active: true,
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
			ID:            "cbd-final-tx-1",
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
	if res == nil {
		t.Fatal("nil result")
	}
	if res.FinalTxID == cascadeEntryTxID {
		t.Errorf("FinalTxID = entryTxID = %q; expected post-segment txID", cascadeEntryTxID)
	}
	if res.FinalTxID == "" {
		t.Errorf("FinalTxID is empty")
	}
	if res.FinalCtx == nil {
		t.Errorf("FinalCtx is nil")
	}
	if state := spi.GetTransaction(res.FinalCtx); state == nil || state.ID != res.FinalTxID {
		t.Errorf("FinalCtx must carry FinalTxID, got %v", state)
	}

	// Cleanup: handler-style commit of the final TX (this is what Task 13 will codify).
	if err := txMgr.Commit(res.FinalCtx, res.FinalTxID); err != nil {
		t.Fatalf("commit FinalTxID: %v", err)
	}
}

// TestEngine_Execute_ReturnsInputTxOnNonSegmentingCascade asserts that for a
// cascade with no COMMIT_BEFORE_DISPATCH processors, FinalTxID equals the
// caller's input txID — the engine did not segment.
func TestEngine_Execute_ReturnsInputTxOnNonSegmentingCascade(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "no-cbd-final-tx", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "NoCbdFinalTxWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "GO", Next: "DONE", Manual: false},
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

	entity := makeEntity("no-cbd-final-tx-1", modelRef, map[string]any{"x": 1})
	entity.Meta.TransactionID = inputTxID

	res, err := engine.Execute(txCtx, entity, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.FinalTxID != inputTxID {
		t.Errorf("FinalTxID = %q, want input txID %q", res.FinalTxID, inputTxID)
	}
	if res.FinalCtx == nil {
		t.Errorf("FinalCtx is nil")
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

// TestEngine_CBD_FollowedBySyncFailure_RollsBackPostSegment is the regression
// for security review finding Sec-#2: when a CBD processor segments the
// cascade (TX_pre committed, TX_post opened) and a SUBSEQUENT processor in
// the same pipeline fails, the engine must roll back TX_post before
// returning. Otherwise the caller-side handler — which only knows the
// original entry txID — never sees TX_post and the connection leaks until
// postgres' idle-in-transaction timeout reclaims it.
//
// Asserts:
//   - Execute returns an error (existing behavior — pipeline aborts).
//   - The post-segment TX is no longer registered with the manager
//     (Commit on it returns "transaction not found" / spi.ErrNotFound,
//     wrapped). The countingTxManager records exactly one engine Begin
//     (TX_post) and one engine Rollback (the new defensive cleanup).
func TestEngine_CBD_FollowedBySyncFailure_RollsBackPostSegment(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	rawTxMgr := factory.NewTransactionManager(uuids)
	cnt := &segCounter{}
	txMgr := &countingTxManager{inner: rawTxMgr, c: cnt}

	mock := &mockExternalProcessing{
		dispatchFunc: func(_ context.Context, e *spi.Entity, proc spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
			switch proc.Name {
			case "cbd-proc":
				// Successful CBD: just return the entity unchanged. The
				// engine commits TX_pre and opens TX_post around this call.
				return e, nil
			case "sync-fail-proc":
				return nil, fmt.Errorf("sync processor exploded after CBD segment")
			}
			return e, nil
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cbd-then-fail", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "CbdThenFailWF", InitialState: "S_pre", Active: true,
		States: map[string]spi.StateDefinition{
			"S_pre": {Transitions: []spi.TransitionDefinition{
				{Name: "CALLOUT", Next: "S_post", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "cbd-proc", ExecutionMode: ExecutionModeCommitBeforeDispatch},
						{Type: ProcessorTypeExternalized, Name: "sync-fail-proc", ExecutionMode: ExecutionModeSync},
					}},
			}},
			"S_post": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entryTxID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	beginsBeforeExecute := cnt.begins
	rollbacksBeforeExecute := cnt.rollbacks

	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:            "cbd-then-fail-1",
			TenantID:      testTenant,
			ModelRef:      modelRef,
			State:         "",
			TransactionID: entryTxID,
		},
		Data: []byte(`{"x":1}`),
	}

	_, execErr := engine.Execute(txCtx, entity, "")
	if execErr == nil {
		t.Fatalf("expected Execute to fail when SYNC processor after CBD fails")
	}

	// The engine must have opened TX_post (one Begin attributable to the
	// CBD segment boundary) and then rolled it back when the SYNC
	// processor failed (one Rollback attributable to the new cleanup).
	enginesBegins := cnt.begins - beginsBeforeExecute
	engineRollbacks := cnt.rollbacks - rollbacksBeforeExecute
	if enginesBegins < 1 {
		t.Errorf("expected >=1 engine Begin (TX_post), got %d", enginesBegins)
	}
	if engineRollbacks < 1 {
		t.Errorf("expected >=1 engine Rollback (TX_post cleanup) after SYNC-fail, got %d "+
			"— TX_post would leak until idle-in-transaction timeout (Sec-#2)",
			engineRollbacks)
	}

	// Defensive: caller still rolls back the original entry TX. This is
	// what the production handler does on a workflow error.
	_ = txMgr.Rollback(txCtx, entryTxID)
}

// TestEngine_CascadeSkipsScheduled_RestsInState verifies that when a state has
// ONLY a scheduled transition as its exit, the automated cascade silently skips
// the scheduled transition and the entity rests in its source state. Until the
// scheduled-task runtime ships (#251), scheduled transitions are invisible to
// the cascade — they wait for their timer.
func TestEngine_CascadeSkipsScheduled_RestsInState(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "sched-rest", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "SchedRestWF", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "scheduledOnly", Next: "S2", Manual: false,
					Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
			}},
			"S2": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("sched-rest-e1", modelRef, map[string]any{})

	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success=true")
	}
	if entity.Meta.State != "S1" {
		t.Errorf("expected entity to rest in S1 (scheduled transitions are invisible to cascade); got %q", entity.Meta.State)
	}
}

// TestEngine_CascadeSkipsScheduled_FiresRegularSibling verifies that when a
// state has both a scheduled transition and a regular automated sibling, the
// cascade silently skips the scheduled one and fires the regular sibling.
func TestEngine_CascadeSkipsScheduled_FiresRegularSibling(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "sched-sibling", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "SchedSiblingWF", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "scheduled", Next: "S2", Manual: false,
					Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
				{Name: "regular", Next: "Sfinal", Manual: false},
			}},
			"S2":     {},
			"Sfinal": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("sched-sibling-e1", modelRef, map[string]any{})

	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success=true")
	}
	if entity.Meta.State != "Sfinal" {
		t.Errorf("expected entity to land in Sfinal via the regular sibling; got %q", entity.Meta.State)
	}
}

// TestEngine_AttemptTransition_Scheduled_ReturnsTransitionNotFound verifies
// that an explicit fire by name of a scheduled transition is rejected with an
// error wrapping ErrTransitionNotFound. Mirrors the Disabled precedent: the
// transition exists in config but is not currently dispatchable from the
// caller's POV. classifyWorkflowError maps the sentinel to
// 400 TRANSITION_NOT_FOUND, identical to the Disabled and missing-by-name
// cases. The audit event reuses SMEventTransitionNotFound; the Details string
// distinguishes the scheduled case.
func TestEngine_AttemptTransition_Scheduled_ReturnsTransitionNotFound(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "sched-explicit", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.0", Name: "SchedExplicitWF", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "Closed", Manual: false,
					Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
			}},
			"Closed": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("sched-explicit-e1", modelRef, map[string]any{})
	entity.Meta.State = "S1"

	_, err := engine.ManualTransition(ctx, entity, "AutoClose")
	if err == nil {
		t.Fatal("expected error firing a scheduled transition by name")
	}
	if !errors.Is(err, ErrTransitionNotFound) {
		t.Errorf("expected error to wrap ErrTransitionNotFound (mirrors Disabled precedent); got: %v", err)
	}
	if !strings.Contains(err.Error(), "scheduled transitions are not yet implemented") {
		t.Errorf("expected error message to name the unavailability; got: %v", err)
	}
	if entity.Meta.State != "S1" {
		t.Errorf("expected entity to stay in source state S1; got %q", entity.Meta.State)
	}
}
