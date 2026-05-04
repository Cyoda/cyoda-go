package workflow

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/match"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
)

var tracer = otel.Tracer("github.com/cyoda-platform/cyoda-go/workflow")

//go:embed default_workflow.json
var defaultWorkflowJSON []byte

// ErrTransitionNotFound is returned by ManualTransition (and surfaces from
// Execute) when the requested transition name is absent from the entity's
// current state — either because no such transition exists or because it is
// disabled. Callers can discriminate this case from other engine failures via
// errors.Is(err, ErrTransitionNotFound).
var ErrTransitionNotFound = errors.New("transition not found")

// maxCascadeDepth is an absolute safety net for total cascade steps.
const maxCascadeDepth = 100

// defaultMaxStateVisits is the default per-state visit limit.
const defaultMaxStateVisits = 10

// Engine orchestrates workflow execution for entities.
type Engine struct {
	factory          spi.StoreFactory
	uuids            spi.UUIDGenerator
	txMgr            spi.TransactionManager
	extProc          contract.ExternalProcessingService
	maxStateVisits   int
	defaultWorkflows []spi.WorkflowDefinition
}

// NewEngine creates a new workflow engine.
func NewEngine(factory spi.StoreFactory, uuids spi.UUIDGenerator, txMgr spi.TransactionManager, opts ...EngineOption) *Engine {
	e := &Engine{factory: factory, uuids: uuids, txMgr: txMgr, maxStateVisits: defaultMaxStateVisits}
	for _, opt := range opts {
		opt(e)
	}

	var defaultWF spi.WorkflowDefinition
	if err := json.Unmarshal(defaultWorkflowJSON, &defaultWF); err != nil {
		panic(fmt.Sprintf("failed to parse default workflow: %v", err))
	}
	e.defaultWorkflows = []spi.WorkflowDefinition{defaultWF}

	return e
}

// EngineOption configures the workflow engine.
type EngineOption func(*Engine)

// WithExternalProcessing configures the engine with an external processing
// service for dispatching processors and function criteria to calculation nodes.
func WithExternalProcessing(extProc contract.ExternalProcessingService) EngineOption {
	return func(e *Engine) {
		e.extProc = extProc
	}
}

// WithMaxStateVisits sets the per-state visit limit for cascade loop protection.
func WithMaxStateVisits(n int) EngineOption {
	return func(e *Engine) {
		if n > 0 {
			e.maxStateVisits = n
		}
	}
}

// Execute runs the workflow engine for entity creation. It selects the matching
// workflow, sets the initial state, optionally fires a named transition, and
// cascades automated transitions.
//
// State-machine audit events are recorded under entity.Meta.TransactionID so
// that the transaction ID returned by POST /entity can be used to look up
// workflow results via /audit/entity/{id}/workflow/{txId}/finished (issue #20).
func (e *Engine) Execute(ctx context.Context, entity *spi.Entity, transitionName string) (*spi.ExecutionResult, error) {
	ctx, span := tracer.Start(ctx, "workflow.execute", trace.WithAttributes(
		observability.AttrEntityID.String(entity.Meta.ID),
		observability.AttrEntityModel.String(entity.Meta.ModelRef.String()),
		observability.AttrTransitionName.String(transitionName),
	))
	defer span.End()

	wfStore, err := e.factory.WorkflowStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get workflow store: %w", err)
	}
	auditStore, err := e.factory.StateMachineAuditStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get audit store: %w", err)
	}

	txID := e.resolveAuditTxID(entity)

	// Record STARTED.
	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventStarted, "State machine started", nil)

	// Load workflows for model. A "not found" error is treated as empty.
	workflows, err := wfStore.Get(ctx, entity.Meta.ModelRef)
	if err != nil && errors.Is(err, spi.ErrNotFound) {
		workflows = nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to load workflows: %w", err)
	}

	// No workflows defined → use default workflow.
	if len(workflows) == 0 {
		common.AddWarning(ctx, "no imported workflow matched — using default workflow")
		workflows = e.defaultWorkflows
	}

	// Select matching workflow.
	selectedWF, err := e.selectWorkflow(ctx, workflows, entity, auditStore, txID)
	if err != nil {
		return nil, err
	}

	// Set initial state.
	entity.Meta.State = selectedWF.InitialState

	// Named transition (on creation with explicit transition).
	if transitionName != "" {
		if err := e.attemptTransition(ctx, entity, selectedWF, transitionName, auditStore, txID); err != nil {
			return nil, err
		}
	}

	// Cascade automated transitions.
	if err := e.cascadeAutomated(ctx, entity, selectedWF, auditStore, txID); err != nil {
		return nil, err
	}

	// Record FINISHED.
	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventFinished, "State machine finished", map[string]any{"success": true})

	return &spi.ExecutionResult{
		State:   entity.Meta.State,
		Success: true,
	}, nil
}

// ManualTransition fires a named transition on an existing entity and cascades
// any automated transitions from the resulting state.
func (e *Engine) ManualTransition(ctx context.Context, entity *spi.Entity, transitionName string) (*spi.ExecutionResult, error) {
	ctx, span := tracer.Start(ctx, "workflow.manual_transition", trace.WithAttributes(
		observability.AttrEntityID.String(entity.Meta.ID),
		observability.AttrEntityModel.String(entity.Meta.ModelRef.String()),
		observability.AttrTransitionName.String(transitionName),
		observability.AttrEntityState.String(entity.Meta.State),
	))
	defer span.End()

	wfStore, err := e.factory.WorkflowStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get workflow store: %w", err)
	}
	auditStore, err := e.factory.StateMachineAuditStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get audit store: %w", err)
	}

	txID := e.resolveAuditTxID(entity)

	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventStarted, "Manual transition started", nil)

	// Load workflows, find the one whose states contain the entity's current state.
	workflows, err := wfStore.Get(ctx, entity.Meta.ModelRef)
	if err != nil && errors.Is(err, spi.ErrNotFound) {
		workflows = nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to load workflows: %w", err)
	}

	// No workflows defined → use default workflow.
	if len(workflows) == 0 {
		common.AddWarning(ctx, "no imported workflow matched — using default workflow")
		workflows = e.defaultWorkflows
	}

	wf := e.findWorkflowForState(workflows, entity.Meta.State)
	if wf == nil {
		return nil, fmt.Errorf("no workflow contains state %q for model %s", entity.Meta.State, entity.Meta.ModelRef)
	}

	if err := e.attemptTransition(ctx, entity, wf, transitionName, auditStore, txID); err != nil {
		return nil, err
	}

	if err := e.cascadeAutomated(ctx, entity, wf, auditStore, txID); err != nil {
		return nil, err
	}

	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventFinished, "Manual transition finished", map[string]any{"success": true})

	return &spi.ExecutionResult{
		State:   entity.Meta.State,
		Success: true,
	}, nil
}

// Loopback re-evaluates automated transitions from the entity's current state
// without firing a specific named transition. This is used when entity data is
// updated and the workflow should re-check conditions from the current state.
func (e *Engine) Loopback(ctx context.Context, entity *spi.Entity) (*spi.ExecutionResult, error) {
	ctx, span := tracer.Start(ctx, "workflow.loopback", trace.WithAttributes(
		observability.AttrEntityID.String(entity.Meta.ID),
		observability.AttrEntityModel.String(entity.Meta.ModelRef.String()),
		observability.AttrEntityState.String(entity.Meta.State),
	))
	defer span.End()

	wfStore, err := e.factory.WorkflowStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get workflow store: %w", err)
	}
	auditStore, err := e.factory.StateMachineAuditStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get audit store: %w", err)
	}

	txID := e.resolveAuditTxID(entity)

	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventStarted, "Loopback started", nil)

	// Load workflows, find the one whose states contain the entity's current state.
	workflows, err := wfStore.Get(ctx, entity.Meta.ModelRef)
	if err != nil && errors.Is(err, spi.ErrNotFound) {
		workflows = nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to load workflows: %w", err)
	}

	// No workflows defined → use default workflow.
	if len(workflows) == 0 {
		common.AddWarning(ctx, "no imported workflow matched — using default workflow")
		workflows = e.defaultWorkflows
	}

	wf := e.findWorkflowForState(workflows, entity.Meta.State)
	if wf == nil {
		// Current state not in any workflow — stable, nothing to do.
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventForcedSuccess, "No workflow contains current state for loopback", nil)
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventFinished, "Loopback finished (state not in workflow)", map[string]any{"success": true})
		return &spi.ExecutionResult{
			State:      entity.Meta.State,
			Success:    true,
			StopReason: "STATE_NOT_IN_WORKFLOW",
		}, nil
	}

	if err := e.cascadeAutomated(ctx, entity, wf, auditStore, txID); err != nil {
		return nil, err
	}

	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventFinished, "Loopback finished", map[string]any{"success": true})

	return &spi.ExecutionResult{
		State:   entity.Meta.State,
		Success: true,
	}, nil
}

// selectWorkflow iterates active workflows and returns the first whose criterion
// matches the entity. Workflows without a criterion match unconditionally.
func (e *Engine) selectWorkflow(ctx context.Context, workflows []spi.WorkflowDefinition, entity *spi.Entity, auditStore spi.StateMachineAuditStore, txID string) (*spi.WorkflowDefinition, error) {
	for i := range workflows {
		wf := &workflows[i]
		if !wf.Active {
			continue
		}

		if len(wf.Criterion) == 0 || string(wf.Criterion) == "null" {
			// No criterion — matches unconditionally.
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
				spi.SMEventWorkflowFound, fmt.Sprintf("Workflow %q selected (no criterion)", wf.Name), nil)
			return wf, nil
		}

		matched, err := e.evaluateCriterion(wf.Criterion, entity, &criterionContext{
			ctx: ctx, txID: txID, workflowName: wf.Name, target: "WORKFLOW",
		})
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate workflow criterion for %q: %w", wf.Name, err)
		}
		if matched {
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
				spi.SMEventWorkflowFound, fmt.Sprintf("Workflow %q matched criterion", wf.Name), nil)
			return wf, nil
		}

		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventWorkflowSkipped, fmt.Sprintf("Workflow %q criterion not matched", wf.Name), nil)
	}

	// No imported workflow matched — fall back to the default workflow.
	if len(e.defaultWorkflows) > 0 {
		common.AddWarning(ctx, "no imported workflow matched — using default workflow")
		defaultWF := &e.defaultWorkflows[0]
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventWorkflowFound, fmt.Sprintf("No imported workflow matched; using default workflow %q", defaultWF.Name), nil)
		return defaultWF, nil
	}

	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventWorkflowNotFound, "No workflow matched entity", nil)
	return nil, fmt.Errorf("no matching workflow for model %s", entity.Meta.ModelRef)
}

// findWorkflowForState returns the first active workflow whose state map contains
// the given state name.
func (e *Engine) findWorkflowForState(workflows []spi.WorkflowDefinition, state string) *spi.WorkflowDefinition {
	for i := range workflows {
		wf := &workflows[i]
		if !wf.Active {
			continue
		}
		if _, ok := wf.States[state]; ok {
			return wf
		}
	}
	return nil
}

// attemptTransition finds and fires a named transition from the entity's current state.
func (e *Engine) attemptTransition(ctx context.Context, entity *spi.Entity, wf *spi.WorkflowDefinition, transitionName string, auditStore spi.StateMachineAuditStore, txID string) error {
	stateDef, ok := wf.States[entity.Meta.State]
	if !ok {
		return fmt.Errorf("state %q not found in workflow %q", entity.Meta.State, wf.Name)
	}

	var transition *spi.TransitionDefinition
	for i := range stateDef.Transitions {
		if stateDef.Transitions[i].Name == transitionName {
			transition = &stateDef.Transitions[i]
			break
		}
	}

	if transition == nil {
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventTransitionNotFound, fmt.Sprintf("Transition %q not found in state %q", transitionName, entity.Meta.State), nil)
		return fmt.Errorf("transition %q not found in state %q: %w", transitionName, entity.Meta.State, ErrTransitionNotFound)
	}

	if transition.Disabled {
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventTransitionNotFound, fmt.Sprintf("Transition %q is disabled", transitionName), nil)
		return fmt.Errorf("transition %q is disabled in state %q: %w", transitionName, entity.Meta.State, ErrTransitionNotFound)
	}

	// Evaluate transition criterion.
	if len(transition.Criterion) > 0 && string(transition.Criterion) != "null" {
		matched, err := e.evaluateCriterion(transition.Criterion, entity, &criterionContext{
			ctx: ctx, txID: txID, workflowName: wf.Name, transitionName: transitionName, target: "TRANSITION",
		})
		if err != nil {
			return fmt.Errorf("failed to evaluate transition criterion: %w", err)
		}
		if !matched {
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
				spi.SMEventTransitionCriterionNoMatch,
				fmt.Sprintf("Transition %q criterion not matched", transitionName), nil)
			return fmt.Errorf("transition %q criterion not matched", transitionName)
		}
	}

	// Execute processors.
	if err := e.executeProcessors(ctx, transition.Processors, entity, auditStore, wf.Name, transitionName, txID); err != nil {
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventStateProcessResult, fmt.Sprintf("Processor failed for transition %q: %v", transitionName, err),
			map[string]any{"success": false})
		return err
	}

	// Record transition and move state.
	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventTransitionMade,
		fmt.Sprintf("Transition %q: %s → %s", transitionName, entity.Meta.State, transition.Next), nil)
	entity.Meta.State = transition.Next

	return nil
}

// cascadeAutomated loops through automated transitions until a stable state is reached.
// It enforces both a per-state visit limit and a total cascade depth safety net.
func (e *Engine) cascadeAutomated(ctx context.Context, entity *spi.Entity, wf *spi.WorkflowDefinition, auditStore spi.StateMachineAuditStore, txID string) error {
	ctx, cascadeSpan := tracer.Start(ctx, "workflow.cascade", trace.WithAttributes(
		observability.AttrWorkflowName.String(wf.Name),
		observability.AttrEntityID.String(entity.Meta.ID),
	))
	defer cascadeSpan.End()

	stateVisits := make(map[string]int)

	for depth := 0; depth < maxCascadeDepth; depth++ {
		state := entity.Meta.State
		stateVisits[state]++
		if stateVisits[state] > e.maxStateVisits {
			reason := fmt.Sprintf("state %q visited %d times (limit: %d)", state, stateVisits[state], e.maxStateVisits)
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, state,
				spi.SMEventCancelled, "State machine aborted: "+reason, nil)
			return fmt.Errorf("state machine aborted: %s", reason)
		}

		stateDef, ok := wf.States[state]
		if !ok {
			return nil // state not in workflow — stable
		}

		fired := false
		for i := range stateDef.Transitions {
			tr := &stateDef.Transitions[i]
			if tr.Disabled || tr.Manual {
				continue
			}

			// Evaluate criterion.
			if len(tr.Criterion) > 0 && string(tr.Criterion) != "null" {
				matched, err := e.evaluateCriterion(tr.Criterion, entity, &criterionContext{
					ctx: ctx, txID: txID, workflowName: wf.Name, transitionName: tr.Name, target: "TRANSITION",
				})
				if err != nil {
					return fmt.Errorf("failed to evaluate transition criterion: %w", err)
				}
				if !matched {
					e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
						spi.SMEventTransitionCriterionNoMatch,
						fmt.Sprintf("Automated transition %q criterion not matched", tr.Name), nil)
					continue
				}
			}

			// Execute processors.
			_, trSpan := tracer.Start(ctx, "workflow.transition", trace.WithAttributes(
				observability.AttrTransitionName.String(tr.Name),
				observability.AttrStateFrom.String(entity.Meta.State),
				observability.AttrStateTo.String(tr.Next),
				observability.AttrCascadeDepth.Int(depth),
			))
			if err := e.executeProcessors(ctx, tr.Processors, entity, auditStore, wf.Name, tr.Name, txID); err != nil {
				trSpan.RecordError(err)
				trSpan.End()
				e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
					spi.SMEventStateProcessResult,
					fmt.Sprintf("Processor failed for transition %q: %v", tr.Name, err),
					map[string]any{"success": false})
				return err
			}

			// Record transition and move state.
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
				spi.SMEventTransitionMade,
				fmt.Sprintf("Transition %q: %s → %s", tr.Name, entity.Meta.State, tr.Next), nil)
			entity.Meta.State = tr.Next
			trSpan.End()
			fired = true
			break // restart from new state
		}

		if !fired {
			cascadeSpan.SetAttributes(observability.AttrCascadeDepth.Int(depth))
			return nil // stable state
		}
	}

	reason := fmt.Sprintf("cascade depth exceeded (%d) at state %q", maxCascadeDepth, entity.Meta.State)
	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventCancelled, "State machine aborted: "+reason, nil)
	return fmt.Errorf("state machine aborted: %s", reason)
}

// criterionContext carries contextual information needed for FUNCTION criteria
// dispatch. Fields are set according to where the criterion is being evaluated.
type criterionContext struct {
	ctx            context.Context
	txID           string
	workflowName   string
	transitionName string
	target         string // "WORKFLOW", "TRANSITION", "PROCESSOR"
}

// evaluateCriterion parses and matches a JSON criterion against the entity.
// If the criterion is a FUNCTION type, it delegates to the external processing
// service using the provided criterionContext.
func (e *Engine) evaluateCriterion(criterion []byte, entity *spi.Entity, cc *criterionContext) (bool, error) {
	cond, err := predicate.ParseCondition(criterion)
	if err != nil {
		return false, fmt.Errorf("failed to parse criterion: %w", err)
	}

	if _, ok := cond.(*predicate.FunctionCondition); ok {
		if e.extProc == nil {
			return false, fmt.Errorf("no external processing service configured for FUNCTION criteria")
		}
		return e.extProc.DispatchCriteria(cc.ctx, entity, criterion, cc.target, cc.workflowName, cc.transitionName, "", cc.txID)
	}

	return match.Match(cond, entity.Data, entity.Meta)
}

// executeProcessors runs each processor in the transition's processor pipeline
// sequentially. Processors are dispatched according to their ExecutionMode:
// ASYNC_NEW_TX runs within a savepoint (failures are non-fatal), while SYNC
// and ASYNC_SAME_TX run inline in the caller's transaction context.
func (e *Engine) executeProcessors(ctx context.Context, processors []spi.ProcessorDefinition, entity *spi.Entity, auditStore spi.StateMachineAuditStore, workflow string, transition string, txID string) error {
	if len(processors) == 0 {
		return nil
	}

	// Record processing pause.
	names := make([]string, len(processors))
	for i, p := range processors {
		names[i] = p.Name
	}
	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventProcessingPaused,
		fmt.Sprintf("Paused for processors: %v", names), nil)

	for _, proc := range processors {
		var success bool
		var procErr error

		switch proc.ExecutionMode {
		case ExecutionModeAsyncNewTx:
			procErr = e.executeAsyncNewTx(ctx, entity, proc, workflow, transition, txID)
			success = procErr == nil

			// ASYNC_NEW_TX failures are non-fatal: log warning, continue pipeline.
			if procErr != nil {
				slog.Warn("ASYNC_NEW_TX processor failed, continuing pipeline",
					"pkg", "workflow", "processor", proc.Name, "error", procErr)
			}

		default: // SYNC, ASYNC_SAME_TX — both inline in caller's transaction.
			procErr = e.executeSyncProcessor(ctx, entity, proc, workflow, transition, txID)
			success = procErr == nil
		}

		auditData := map[string]any{
			"success": success,
			"mode":    proc.ExecutionMode,
		}
		if procErr != nil {
			auditData["error"] = procErr.Error()
		}
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventStateProcessResult,
			fmt.Sprintf("Processor %q completed", proc.Name), auditData)

		// For SYNC/ASYNC_SAME_TX, failure kills the pipeline.
		if procErr != nil && proc.ExecutionMode != ExecutionModeAsyncNewTx {
			return fmt.Errorf("processor %s failed: %w", proc.Name, procErr)
		}
	}
	return nil
}

// executeSyncProcessor runs a SYNC or ASYNC_SAME_TX processor inline in the
// caller's transaction. On success the entity's Data is updated with the
// processor's returned modifications.
func (e *Engine) executeSyncProcessor(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, workflow, transition, txID string) error {
	if e.extProc == nil {
		return nil
	}
	modifiedEntity, err := e.extProc.DispatchProcessor(ctx, entity, proc, workflow, transition, txID)
	if err != nil {
		return err
	}
	if modifiedEntity != nil && modifiedEntity.Data != nil {
		entity.Data = modifiedEntity.Data
	}
	return nil
}

// executeAsyncNewTx runs an ASYNC_NEW_TX processor within a savepoint. The
// processor's returned entity modifications are intentionally discarded —
// ASYNC_NEW_TX processors perform side-effects only. On dispatch failure the
// savepoint is rolled back and the error is returned; on success the savepoint
// is released.
func (e *Engine) executeAsyncNewTx(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, workflow, transition, txID string) error {
	if e.extProc == nil {
		return nil
	}

	// Without a transaction manager, fall back to plain dispatch (no savepoint).
	if e.txMgr == nil {
		_, err := e.extProc.DispatchProcessor(ctx, entity, proc, workflow, transition, txID)
		return err
	}

	spID, err := e.txMgr.Savepoint(ctx, txID)
	if err != nil {
		return fmt.Errorf("savepoint creation failed: %w", err)
	}

	_, dispatchErr := e.extProc.DispatchProcessor(ctx, entity, proc, workflow, transition, txID)
	if dispatchErr != nil {
		if rbErr := e.txMgr.RollbackToSavepoint(ctx, txID, spID); rbErr != nil {
			slog.Warn("failed to rollback savepoint after processor error",
				"pkg", "workflow", "processor", proc.Name,
				"savepointID", spID, "rollbackError", rbErr)
		}
		return dispatchErr
	}

	if err := e.txMgr.ReleaseSavepoint(ctx, txID, spID); err != nil {
		return fmt.Errorf("savepoint release failed: %w", err)
	}
	return nil
}

// commitAndReopenSegment is the COMMIT_BEFORE_DISPATCH segment-boundary
// primitive. It flushes the in-memory entity to txID's buffer, commits
// txID (TX_pre), and begins a fresh TX (TX_post). The caller continues
// the cascade in (newCtx, newTxID).
//
// On any failure the original transaction may already have been committed
// (durable) — the caller cannot rollback prior work. Errors flow back as
// they do for any other engine failure: the cascade aborts and surfaces
// the error to its caller.
func (e *Engine) commitAndReopenSegment(ctx context.Context, entity *spi.Entity, txID string) (newTxID string, newCtx context.Context, err error) {
	es, err := e.factory.EntityStore(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("commit-before-dispatch: get entity store: %w", err)
	}
	if _, err := es.Save(ctx, entity); err != nil {
		return "", nil, fmt.Errorf("commit-before-dispatch: flush pre-callout state: %w", err)
	}
	if err := e.txMgr.Commit(ctx, txID); err != nil {
		return "", nil, fmt.Errorf("commit-before-dispatch: commit TX_pre: %w", err)
	}
	newTxID, newCtx, err = e.txMgr.Begin(context.WithoutCancel(ctx))
	if err != nil {
		return "", nil, fmt.Errorf("commit-before-dispatch: begin TX_post: %w", err)
	}
	return newTxID, newCtx, nil
}

// resolveAuditTxID returns the transaction ID to use for state-machine audit
// events. It uses the entity's transaction ID (set by the caller, e.g.
// CreateEntity or UpdateEntity) so that audit events are keyed on the same
// txID returned in the HTTP response. Falls back to generating a fresh ID
// when the entity has no transaction ID set (e.g. unit tests that don't
// simulate a full transaction lifecycle).
func (e *Engine) resolveAuditTxID(entity *spi.Entity) string {
	if entity.Meta.TransactionID != "" {
		return entity.Meta.TransactionID
	}
	return uuid.UUID(e.uuids.NewTimeUUID()).String()
}

// recordEvent records a single audit event.
func (e *Engine) recordEvent(auditStore spi.StateMachineAuditStore, ctx context.Context, entityID, txID, state string, eventType spi.StateMachineEventType, details string, data map[string]any) {
	event := spi.StateMachineEvent{
		EventType:     eventType,
		EntityID:      entityID,
		TimeUUID:      uuid.UUID(e.uuids.NewTimeUUID()).String(),
		State:         state,
		TransactionID: txID,
		Details:       details,
		Data:          data,
		Timestamp:     time.Now(),
	}
	// Best-effort recording; audit failures should not break workflow execution.
	_ = auditStore.Record(ctx, entityID, event)
}
