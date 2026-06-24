package workflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// lifecycleCriterion returns a criterion that checks a lifecycle field (e.g. "state").
func lifecycleCriterion(field, op string, value any) json.RawMessage {
	c, _ := json.Marshal(map[string]any{
		"type": "lifecycle", "field": field, "operatorType": op, "value": value,
	})
	return c
}

// --- Test: Auto cascade chain ---

func TestScenarioAutoCascadeChain(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "cascade-chain", ModelVersion: "1.0"}

	// INITIAL ->(auto)-> STEP1 ->(auto)-> STEP2 ->(auto)-> FINAL
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "CascadeChainWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "TO_STEP1", Next: "STEP1", Manual: false},
			}},
			"STEP1": {Transitions: []spi.TransitionDefinition{
				{Name: "TO_STEP2", Next: "STEP2", Manual: false},
			}},
			"STEP2": {Transitions: []spi.TransitionDefinition{
				{Name: "TO_FINAL", Next: "FINAL", Manual: false},
			}},
			"FINAL": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("chain1", modelRef, map[string]any{"ok": true})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	if entity.Meta.State != "FINAL" {
		t.Fatalf("expected state FINAL, got %s", entity.Meta.State)
	}

	// Verify 3 transitions were made via audit events.
	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("failed to get audit store: %v", err)
	}
	events, err := auditStore.GetEvents(ctx, "chain1")
	if err != nil {
		t.Fatalf("failed to get events: %v", err)
	}
	transitionCount := 0
	for _, ev := range events {
		if ev.EventType == spi.SMEventTransitionMade {
			transitionCount++
		}
	}
	if transitionCount != 3 {
		t.Errorf("expected 3 transitions, got %d", transitionCount)
	}
}

// --- Test: Loopback with auto exit ---

func TestScenarioLoopbackWithAutoExit(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "loopback-exit", ModelVersion: "1.0"}

	// PROCESSING:
	//   - RETRY -> PROCESSING (manual, loopback)
	//   - COMPLETE ->(auto, criterion: $.status == "done")-> COMPLETED
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "LoopbackExitWF", InitialState: "PROCESSING", Active: true,
		States: map[string]spi.StateDefinition{
			"PROCESSING": {Transitions: []spi.TransitionDefinition{
				{Name: "COMPLETE", Next: "COMPLETED", Manual: false,
					Criterion: simpleCriterion("$.status", "EQUALS", "done")},
				{Name: "RETRY", Next: "PROCESSING", Manual: true},
			}},
			"COMPLETED": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	// Entity with status=done → auto-completes to COMPLETED.
	entity1 := makeEntity("lb1", modelRef, map[string]any{"status": "done"})
	result, err := engine.Execute(ctx, entity1, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	if entity1.Meta.State != "COMPLETED" {
		t.Fatalf("expected COMPLETED, got %s", entity1.Meta.State)
	}

	// Entity with status=pending → stays at PROCESSING (criterion false).
	entity2 := makeEntity("lb2", modelRef, map[string]any{"status": "pending"})
	result, err = engine.Execute(ctx, entity2, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success (stable state)")
	}
	if entity2.Meta.State != "PROCESSING" {
		t.Fatalf("expected PROCESSING, got %s", entity2.Meta.State)
	}

	// Manual RETRY transition → stays at PROCESSING, auto re-evaluates, still pending.
	entity2.Meta.State = "PROCESSING"
	result, err = engine.ManualTransition(ctx, entity2, "RETRY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	if entity2.Meta.State != "PROCESSING" {
		t.Fatalf("expected PROCESSING after retry (still pending), got %s", entity2.Meta.State)
	}
}

// --- Test: Stuck state (only manual transitions) ---

func TestScenarioStuckState(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "stuck", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "StuckWF", InitialState: "STUCK", Active: true,
		States: map[string]spi.StateDefinition{
			"STUCK": {Transitions: []spi.TransitionDefinition{
				{Name: "UNSTICK", Next: "FREE", Manual: true},
			}},
			"FREE": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("stuck1", modelRef, map[string]any{"x": 1})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success (stable at STUCK)")
	}
	if entity.Meta.State != "STUCK" {
		t.Fatalf("expected STUCK, got %s", entity.Meta.State)
	}
}

// --- Test: Successive auto transitions with criteria ---

func TestScenarioSuccessiveAutoWithCriteria(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "priority-route", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "PriorityWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "FAST", Next: "FAST_TRACK", Manual: false,
					Criterion: simpleCriterion("$.priority", "EQUALS", "high")},
				{Name: "SLOW", Next: "SLOW_TRACK", Manual: false,
					Criterion: simpleCriterion("$.priority", "EQUALS", "low")},
			}},
			"FAST_TRACK": {Transitions: []spi.TransitionDefinition{
				{Name: "FAST_DONE", Next: "DONE", Manual: false},
			}},
			"SLOW_TRACK": {Transitions: []spi.TransitionDefinition{
				{Name: "TO_REVIEW", Next: "REVIEW", Manual: false},
			}},
			"REVIEW": {Transitions: []spi.TransitionDefinition{
				{Name: "REVIEW_DONE", Next: "DONE", Manual: false},
			}},
			"DONE": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	// High priority: INITIAL -> FAST_TRACK -> DONE
	entityHigh := makeEntity("prio-h", modelRef, map[string]any{"priority": "high"})
	result, err := engine.Execute(ctx, entityHigh, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	if entityHigh.Meta.State != "DONE" {
		t.Fatalf("expected DONE for high priority, got %s", entityHigh.Meta.State)
	}

	// Low priority: INITIAL -> SLOW_TRACK -> REVIEW -> DONE
	entityLow := makeEntity("prio-l", modelRef, map[string]any{"priority": "low"})
	result, err = engine.Execute(ctx, entityLow, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	if entityLow.Meta.State != "DONE" {
		t.Fatalf("expected DONE for low priority, got %s", entityLow.Meta.State)
	}

	// Verify different transition counts via audit.
	auditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("failed to get audit store: %v", err)
	}

	highEvents, _ := auditStore.GetEvents(ctx, "prio-h")
	lowEvents, _ := auditStore.GetEvents(ctx, "prio-l")

	highTransitions := countEventType(highEvents, spi.SMEventTransitionMade)
	lowTransitions := countEventType(lowEvents, spi.SMEventTransitionMade)

	if highTransitions != 2 {
		t.Errorf("expected 2 transitions for high priority, got %d", highTransitions)
	}
	if lowTransitions != 3 {
		t.Errorf("expected 3 transitions for low priority, got %d", lowTransitions)
	}
}

// --- Test: Static loop detection on import ---

func TestScenarioStaticLoopDetection(t *testing.T) {
	// A -> (auto, no criterion) -> B -> (auto, no criterion) -> A
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "LoopWF", InitialState: "A", Active: true,
		States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{
				{Name: "TO_B", Next: "B", Manual: false},
			}},
			"B": {Transitions: []spi.TransitionDefinition{
				{Name: "TO_A", Next: "A", Manual: false},
			}},
		},
	}

	err := validateWorkflows([]spi.WorkflowDefinition{wf}, false)
	if err == nil {
		t.Fatal("expected error for infinite loop")
	}
	if !strings.Contains(err.Error(), "infinite loop detected") {
		t.Fatalf("expected 'infinite loop detected' in error, got: %v", err)
	}
}

func TestScenarioStaticLoopDetectionSelfLoop(t *testing.T) {
	// A -> (auto, no criterion) -> A (self-loop)
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "SelfLoopWF", InitialState: "A", Active: true,
		States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{
				{Name: "TO_A", Next: "A", Manual: false},
			}},
		},
	}

	err := validateWorkflows([]spi.WorkflowDefinition{wf}, false)
	if err == nil {
		t.Fatal("expected error for self-loop")
	}
	if !strings.Contains(err.Error(), "infinite loop detected") {
		t.Fatalf("expected 'infinite loop detected' in error, got: %v", err)
	}
}

func TestScenarioStaticValidationPassesGuardedCycle(t *testing.T) {
	// A -> (auto, WITH criterion) -> B -> (auto, WITH criterion) -> A
	// This should pass static validation because the criteria may break the cycle.
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "GuardedCycleWF", InitialState: "A", Active: true,
		States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{
				{Name: "TO_B", Next: "B", Manual: false,
					Criterion: simpleCriterion("$.x", "EQUALS", 1)},
			}},
			"B": {Transitions: []spi.TransitionDefinition{
				{Name: "TO_A", Next: "A", Manual: false,
					Criterion: simpleCriterion("$.x", "EQUALS", 2)},
			}},
		},
	}

	err := validateWorkflows([]spi.WorkflowDefinition{wf}, false)
	if err != nil {
		t.Fatalf("expected no error for guarded cycle, got: %v", err)
	}
}

func TestScenarioStaticValidationPassesManualCycle(t *testing.T) {
	// A -> (manual) -> B -> (manual) -> A
	// Manual transitions never form infinite automated loops.
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "ManualCycleWF", InitialState: "A", Active: true,
		States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{
				{Name: "TO_B", Next: "B", Manual: true},
			}},
			"B": {Transitions: []spi.TransitionDefinition{
				{Name: "TO_A", Next: "A", Manual: true},
			}},
		},
	}

	err := validateWorkflows([]spi.WorkflowDefinition{wf}, false)
	if err != nil {
		t.Fatalf("expected no error for manual cycle, got: %v", err)
	}
}

// --- Test: Dynamic loop limit ---

func TestScenarioDynamicLoopLimit(t *testing.T) {
	// A ->(auto, criterion: lifecycle state NOT_NULL)-> B ->(auto, criterion: lifecycle state NOT_NULL)-> A
	// Criteria always match, so static validation passes, but dynamic execution loops.
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	// Use a small maxStateVisits for faster test.
	txMgr := factory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, txMgr, WithMaxStateVisits(3))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "dynamic-loop", ModelVersion: "1.0"}

	alwaysTrue := lifecycleCriterion("state", "NOT_NULL", "")

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "DynamicLoopWF", InitialState: "A", Active: true,
		States: map[string]spi.StateDefinition{
			"A": {Transitions: []spi.TransitionDefinition{
				{Name: "TO_B", Next: "B", Manual: false, Criterion: alwaysTrue},
			}},
			"B": {Transitions: []spi.TransitionDefinition{
				{Name: "TO_A", Next: "A", Manual: false, Criterion: alwaysTrue},
			}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entity := makeEntity("dyn1", modelRef, map[string]any{"x": 1})
	_, err := engine.Execute(ctx, entity, "")
	if err == nil {
		t.Fatal("expected error from dynamic loop limit")
	}
	if !strings.Contains(err.Error(), "state machine aborted") {
		t.Fatalf("expected 'state machine aborted' in error, got: %v", err)
	}

	// Verify CANCEL event was recorded.
	auditStore, err2 := factory.StateMachineAuditStore(ctx)
	if err2 != nil {
		t.Fatalf("failed to get audit store: %v", err2)
	}
	events, err2 := auditStore.GetEvents(ctx, "dyn1")
	if err2 != nil {
		t.Fatalf("failed to get events: %v", err2)
	}
	hasCancelEvent := false
	for _, ev := range events {
		if ev.EventType == spi.SMEventCancelled {
			hasCancelEvent = true
			break
		}
	}
	if !hasCancelEvent {
		t.Error("expected CANCEL audit event")
	}
}

// --- Test: Manual transition triggers auto cascade ---

func TestScenarioManualTriggersAutoCascade(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "manual-cascade", ModelVersion: "1.0"}

	// INITIAL: no auto transitions
	// INITIAL -> PROCESS (manual)
	// PROCESS ->(auto)-> DONE
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "ManualCascadeWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{
				{Name: "PROCESS", Next: "PROCESS", Manual: true},
			}},
			"PROCESS": {Transitions: []spi.TransitionDefinition{
				{Name: "AUTO_DONE", Next: "DONE", Manual: false},
			}},
			"DONE": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	// Create entity → stays at INITIAL (no auto transitions).
	entity := makeEntity("mc1", modelRef, map[string]any{"x": 1})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	if entity.Meta.State != "INITIAL" {
		t.Fatalf("expected INITIAL, got %s", entity.Meta.State)
	}

	// Manual transition PROCESS → cascades to DONE.
	result, err = engine.ManualTransition(ctx, entity, "PROCESS")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	if entity.Meta.State != "DONE" {
		t.Fatalf("expected DONE after manual transition + cascade, got %s", entity.Meta.State)
	}
}

// --- Test: Loopback re-evaluates after data change ---

func TestScenarioLoopbackReEvaluates(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "loopback-reeval", ModelVersion: "1.0"}

	// WAITING: auto transition with criterion $.ready == true -> DONE
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "LoopbackReEvalWF", InitialState: "WAITING", Active: true,
		States: map[string]spi.StateDefinition{
			"WAITING": {Transitions: []spi.TransitionDefinition{
				{Name: "AUTO_DONE", Next: "DONE", Manual: false,
					Criterion: simpleCriterion("$.ready", "EQUALS", true)},
			}},
			"DONE": {Transitions: []spi.TransitionDefinition{}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	// Create with ready=false → stays at WAITING.
	entity := makeEntity("lbr1", modelRef, map[string]any{"ready": false})
	result, err := engine.Execute(ctx, entity, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entity.Meta.State != "WAITING" {
		t.Fatalf("expected WAITING, got %s", entity.Meta.State)
	}

	// Update data to ready=true and loopback → should cascade to DONE.
	entity.Data, _ = json.Marshal(map[string]any{"ready": true})
	result, err = engine.Loopback(ctx, entity)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	if entity.Meta.State != "DONE" {
		t.Fatalf("expected DONE after loopback, got %s", entity.Meta.State)
	}
}

// --- Test: Static validation via import handler (integration) ---

func TestScenarioStaticLoopDetectionViaImport(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr2 := factory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, txMgr2)
	handler := New(factory, engine)

	ctx := ctxWithTenant(testTenant)

	// Register the target model so the import handler reaches static
	// validation (since #131, the handler returns 404 if the model is missing).
	mstore, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := mstore.Save(ctx, &spi.ModelDescriptor{
		Ref:   spi.ModelRef{EntityName: "looptest", ModelVersion: "1"},
		State: spi.ModelLocked,
	}); err != nil {
		t.Fatalf("ModelStore.Save: %v", err)
	}

	// Prepare the HTTP request with a looping workflow.
	body := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1",
			"name": "loop-wf",
			"initialState": "A",
			"active": true,
			"states": {
				"A": {"transitions": [{"name": "TO_B", "next": "B", "manual": false}]},
				"B": {"transitions": [{"name": "TO_A", "next": "A", "manual": false}]}
			}
		}]
	}`

	req := newTestHTTPRequest(t, "POST", "/model/looptest/1/workflow/import", body, ctx)
	rr := newTestHTTPResponse()

	handler.ImportEntityModelWorkflow(rr, req, "looptest", 1)

	if rr.Code != 400 {
		t.Fatalf("expected status 400 for loop detection, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "infinite loop detected") {
		t.Fatalf("expected 'infinite loop detected' in response, got: %s", rr.Body.String())
	}
}

// --- helpers ---

func countEventType(events []spi.StateMachineEvent, eventType spi.StateMachineEventType) int {
	count := 0
	for _, ev := range events {
		if ev.EventType == eventType {
			count++
		}
	}
	return count
}

func newTestHTTPRequest(t *testing.T, method, target, body string, ctx context.Context) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(ctx)
}

func newTestHTTPResponse() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}

// --- Test: validateProcessorFlags — startNewTxOnDispatch is COMMIT_BEFORE_DISPATCH-only ---

func TestValidateWorkflows_RejectsStartNewTxOnDispatchOnNonCommitBeforeDispatch(t *testing.T) {
	// startNewTxOnDispatch:true on a SYNC processor must be rejected at registration.
	tt := true
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "test-wf", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "t", Next: "S2", Manual: true, Processors: []spi.ProcessorDefinition{
					{Type: ProcessorTypeExternalized, Name: "p", ExecutionMode: "SYNC",
						Config: spi.ProcessorConfig{StartNewTxOnDispatch: &tt}},
				}},
			}},
			"S2": {},
		},
	}
	err := validateWorkflows([]spi.WorkflowDefinition{wf}, false)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "startNewTxOnDispatch") {
		t.Fatalf("error message must mention startNewTxOnDispatch, got: %v", err)
	}
	if !strings.Contains(err.Error(), "COMMIT_BEFORE_DISPATCH") {
		t.Fatalf("error message must mention COMMIT_BEFORE_DISPATCH, got: %v", err)
	}
	if !strings.Contains(err.Error(), "SYNC") {
		t.Fatalf("error message must mention the offending mode (SYNC), got: %v", err)
	}
	if !strings.Contains(err.Error(), "test-wf") || !strings.Contains(err.Error(), `"t"`) || !strings.Contains(err.Error(), `"p"`) {
		t.Fatalf("error message must mention workflow, transition, and processor names, got: %v", err)
	}
}

func TestValidateWorkflows_AcceptsStartNewTxOnDispatchOnCommitBeforeDispatch(t *testing.T) {
	tt := true
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "test-wf", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "t", Next: "S2", Manual: true, Processors: []spi.ProcessorDefinition{
					{Type: ProcessorTypeExternalized, Name: "p", ExecutionMode: "COMMIT_BEFORE_DISPATCH",
						Config: spi.ProcessorConfig{StartNewTxOnDispatch: &tt}},
				}},
			}},
			"S2": {},
		},
	}
	if err := validateWorkflows([]spi.WorkflowDefinition{wf}, false); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateWorkflows_AcceptsStartNewTxOnDispatchNilOrFalse(t *testing.T) {
	// Default (nil) must not be rejected. Explicit false must not be rejected.
	ff := false
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "test-wf", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "t", Next: "S2", Manual: true, Processors: []spi.ProcessorDefinition{
					{Type: ProcessorTypeExternalized, Name: "p1", ExecutionMode: "SYNC",
						Config: spi.ProcessorConfig{}},
					{Type: ProcessorTypeExternalized, Name: "p2", ExecutionMode: "SYNC",
						Config: spi.ProcessorConfig{StartNewTxOnDispatch: &ff}},
				}},
			}},
			"S2": {},
		},
	}
	if err := validateWorkflows([]spi.WorkflowDefinition{wf}, false); err != nil {
		t.Fatalf("expected no error for nil/false flag, got: %v", err)
	}
}

// TestScenarioStartNewTxOnDispatchRejectionViaImport asserts that the
// validator's startNewTxOnDispatch=true-on-non-COMMIT_BEFORE_DISPATCH
// rejection surfaces through the public workflow-import HTTP path with a
// 400 Bad Request and a meaningful error message — i.e. that registration
// callers (not just the package-internal helper) see the rejection. Mirrors
// TestScenarioStaticLoopDetectionViaImport for parity with the other
// validateWorkflows guard.
func TestScenarioStartNewTxOnDispatchRejectionViaImport(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr2 := factory.NewTransactionManager(uuids)
	engine := NewEngine(factory, uuids, txMgr2)
	handler := New(factory, engine)

	ctx := ctxWithTenant(testTenant)

	// Register the target model so the import handler reaches static
	// validation (otherwise it returns 404 MODEL_NOT_FOUND first, per #131).
	mstore, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := mstore.Save(ctx, &spi.ModelDescriptor{
		Ref:   spi.ModelRef{EntityName: "snttd", ModelVersion: "1"},
		State: spi.ModelLocked,
	}); err != nil {
		t.Fatalf("ModelStore.Save: %v", err)
	}

	// Workflow with a SYNC processor declaring startNewTxOnDispatch=true —
	// invalid combination per spec; validateProcessorFlags must reject.
	body := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1",
			"name": "snttd-wf",
			"initialState": "S1",
			"active": true,
			"states": {
				"S1": {"transitions": [{
					"name": "t",
					"next": "S2",
					"manual": true,
					"processors": [{
						"type": "EXTERNAL",
						"name": "p",
						"executionMode": "SYNC",
						"config": {"startNewTxOnDispatch": true}
					}]
				}]},
				"S2": {"transitions": []}
			}
		}]
	}`

	req := newTestHTTPRequest(t, "POST", "/model/snttd/1/workflow/import", body, ctx)
	rr := newTestHTTPResponse()

	handler.ImportEntityModelWorkflow(rr, req, "snttd", 1)

	if rr.Code != 400 {
		t.Fatalf("expected status 400 for startNewTxOnDispatch misuse, got %d; body: %s", rr.Code, rr.Body.String())
	}
	respBody := rr.Body.String()
	// Pin the full error format reaches the HTTP caller intact: error code,
	// flag name, plus all four context fields the validator includes
	// (workflow / transition / processor / offending mode). A future handler
	// change that collapses this message would surface as a regression here,
	// not just in the unit-level validator test. The validator uses %q on
	// each name; the JSON response body escapes those inner quotes as \".
	for _, want := range []string{
		"VALIDATION_FAILED",
		"startNewTxOnDispatch",
		`workflow \"snttd-wf\"`,
		`transition \"t\"`,
		`processor \"p\"`,
		`got \"SYNC\"`,
	} {
		if !strings.Contains(respBody, want) {
			t.Errorf("expected response body to contain %q; got: %s", want, respBody)
		}
	}
}
