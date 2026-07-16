package workflow

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/match"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

var tracer = otel.Tracer("github.com/cyoda-platform/cyoda-go/workflow")

//go:embed default_workflow.json
var defaultWorkflowJSON []byte

// ErrTransitionNotFound is returned by ManualTransition (and surfaces from
// Execute) when the requested transition name is absent from the entity's
// current state — either because no such transition exists, it is disabled,
// or it is scheduled and therefore not manually fireable (it fires
// automatically via the timer runtime). Callers can discriminate this case
// from other engine failures via errors.Is(err, ErrTransitionNotFound).
var ErrTransitionNotFound = errors.New("transition not found")

// ErrCriterionNotMatched is the sentinel FireScheduledTransition uses to
// distinguish "the transition's criterion evaluated to false" (Declined —
// terminal, one-shot, no retry) from every other fireTransition failure
// (criterion-evaluation error, processor failure — both retried on the next
// scan). errors.Is(err, ErrCriterionNotMatched) reports true for the error
// fireTransition returns from either of its criterion-not-matched branches.
//
// fireTransition cannot wrap with a plain %w here: attemptTransition's tests
// assert the criterion-not-matched message byte-for-byte
// (`transition %q criterion not matched: %s` / `transition %q criterion not
// matched`), and %w appends the sentinel's own "criterion not matched" text,
// which would change that message. criterionNotMatchedError instead carries
// the exact pre-existing text in Error() while still satisfying errors.Is
// via a custom Is method.
var ErrCriterionNotMatched = errors.New("criterion not matched")

// criterionNotMatchedError preserves fireTransition's existing
// criterion-not-matched error text exactly while satisfying
// errors.Is(err, ErrCriterionNotMatched).
type criterionNotMatchedError struct {
	msg string
}

func (e *criterionNotMatchedError) Error() string { return e.msg }

// Is reports whether target is ErrCriterionNotMatched, so callers can use
// errors.Is instead of a type assertion.
func (e *criterionNotMatchedError) Is(target error) bool {
	return target == ErrCriterionNotMatched
}

// scheduledReason is the human-readable cause emitted by both the audit
// event Details and the wrapped error message when an explicit fire of a
// scheduled transition is rejected: a scheduled transition fires
// automatically via the timer runtime and is never manually fireable by
// name. Extracted as a const so the two call sites (audit Details, wrapped
// error) stay in sync.
const scheduledReason = "scheduled and fires automatically; it is not manually fireable"

// maxCascadeDepth is an absolute safety net for total cascade steps.
const maxCascadeDepth = 100

// defaultMaxStateVisits is the default per-state visit limit.
const defaultMaxStateVisits = 10

// maxCriterionReasonLen bounds the criterion reason stored in the audit and
// reflected into the 400 body. The reason is compute-node-supplied and the
// audit is durable storage, so it is capped defensively.
const maxCriterionReasonLen = 2048

// defaultCriterionReason is recorded when a criterion returns false without an
// explanatory reason (inline predicates, or a FUNCTION criterion that supplies
// none). Keeps the audit data.reason shape stable.
const defaultCriterionReason = "criterion did not match"

// capReason truncates a compute-node-supplied criterion reason to
// maxCriterionReasonLen before it is persisted to the audit trail or
// reflected into an error message.
func capReason(s string) string {
	if len(s) <= maxCriterionReasonLen {
		return s
	}
	// Back off to a UTF-8 rune boundary so truncation never splits a
	// multibyte rune.
	cut := maxCriterionReasonLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// criterionName derives the audit "criterion" field: the FUNCTION name for a
// function criterion, else the parsed condition type (e.g. "simple", "group").
// The canonical FUNCTION wire shape nests the name under "function" (see
// internal/grpc/dispatch.go); a top-level "name" is kept as a fallback for
// robustness.
func criterionName(criterion []byte) string {
	var envelope struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(criterion, &envelope) == nil && envelope.Type == "function" {
		if envelope.Function.Name != "" {
			return envelope.Function.Name
		}
		if envelope.Name != "" {
			return envelope.Name
		}
	}
	if cond, err := predicate.ParseCondition(criterion); err == nil {
		return cond.Type()
	}
	return ""
}

// Engine orchestrates workflow execution for entities.
type Engine struct {
	factory          spi.StoreFactory
	uuids            spi.UUIDGenerator
	txMgr            spi.TransactionManager
	extProc          contract.ExternalProcessingService
	maxStateVisits   int
	defaultWorkflows []spi.WorkflowDefinition
	// clock supplies "now" for scheduled-transition arming (reconcileScheduledTasks)
	// and for FireScheduledTransition's lateness/grace-band math and the
	// scheduler's scan loop. Defaults to time.Now; overridden via
	// WithScheduledClock for deterministic tests.
	clock func() time.Time
	// expiryGraceMs is the margin (ms) above a scheduled transition's
	// TimeoutMs that FireScheduledTransition tolerates before expiring a
	// late task instead of leaving it for the next scan (design §5.5).
	// Defaults to defaultExpiryGraceMs; overridden via WithExpiryGrace.
	expiryGraceMs int64
}

// NewEngine creates a new workflow engine.
func NewEngine(factory spi.StoreFactory, uuids spi.UUIDGenerator, txMgr spi.TransactionManager, opts ...EngineOption) *Engine {
	e := &Engine{factory: factory, uuids: uuids, txMgr: txMgr, maxStateVisits: defaultMaxStateVisits, clock: time.Now, expiryGraceMs: defaultExpiryGraceMs}
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

// WithScheduledClock overrides the engine's clock, used for
// reconcileScheduledTasks' scheduledTime/armedAt computation and, later, by
// FireScheduledTransition and the scan-loop scheduler. Defaults to
// time.Now; tests inject a deterministic clock instead.
func WithScheduledClock(clock func() time.Time) EngineOption {
	return func(e *Engine) {
		if clock != nil {
			e.clock = clock
		}
	}
}

// now returns the engine's current time per its configured clock.
func (e *Engine) now() time.Time {
	if e.clock != nil {
		return e.clock()
	}
	return time.Now()
}

// Execute runs the workflow engine for entity creation. It selects the matching
// workflow, sets the initial state, optionally fires a named transition, and
// cascades automated transitions.
//
// State-machine audit events are recorded under entity.Meta.TransactionID so
// that the transaction ID returned by POST /entity can be used to look up
// workflow results via /audit/entity/{id}/workflow/{txId}/finished.
func (e *Engine) Execute(ctx context.Context, entity *spi.Entity, transitionName string) (*EngineResult, error) {
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

	// No workflows defined → use embedded default. Body warning surfaces to
	// the client; slog.Warn surfaces to operators.
	if len(workflows) == 0 {
		common.AddWarning(ctx, "no workflows imported for model — using default workflow")
		e.logDefaultFallback(ctx, entity, "no_workflows_imported")
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
	currentCtx := ctx
	currentTxID := txID
	if transitionName != "" {
		nCtx, nTxID, err := e.attemptTransition(currentCtx, entity, selectedWF, transitionName, auditStore, currentTxID)
		currentCtx = nCtx
		currentTxID = nTxID
		if err != nil {
			return nil, err
		}
	}

	// Cascade automated transitions.
	nCtx, nTxID, err := e.cascadeAutomated(currentCtx, entity, selectedWF, auditStore, currentTxID)
	currentCtx = nCtx
	currentTxID = nTxID
	if err != nil {
		return nil, err
	}

	// Arm/cancel the settled state's scheduled tasks. Runs after the cascade
	// settles, using the FINAL ctx/txID — the write joins whatever
	// transaction currentCtx carries, atomic with the entity write it just
	// cascaded into.
	if err := e.reconcileScheduledTasks(currentCtx, entity, selectedWF, currentTxID, auditStore, ""); err != nil {
		return nil, fmt.Errorf("failed to reconcile scheduled tasks: %w", err)
	}

	// Record FINISHED. Recorded via currentCtx so it lands in whichever segment
	// is currently open; cascade-entry txID for client-correlation continuity
	// (spec §8).
	e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventFinished, "State machine finished", map[string]any{"success": true})

	return &EngineResult{
		ExecutionResult: &spi.ExecutionResult{
			State:   entity.Meta.State,
			Success: true,
		},
		FinalCtx:  currentCtx,
		FinalTxID: currentTxID,
		Segmented: currentTxID != txID,
	}, nil
}

// ManualTransitionWithIfMatch is the variant of ManualTransition used by
// callers that supply an If-Match expected-txID (cross-request optimistic
// concurrency). Per spec §4.1, the expected-txID is applied at the FIRST
// segment-flush of the cascade — the engine's first EntityStore write inside
// the cascade — so a stale If-Match aborts before any segment commits or any
// external dispatch fires.
//
// For non-segmenting cascades (no COMMIT_BEFORE_DISPATCH processors) the
// engine never performs a first-segment flush; ifMatch is left untouched on
// the context for the handler to apply post-engine via its own CompareAndSave
// path. The handler distinguishes the two cases via EngineResult.Segmented.
//
// If ifMatch is empty this method is identical to ManualTransition.
func (e *Engine) ManualTransitionWithIfMatch(ctx context.Context, entity *spi.Entity, transitionName, ifMatch string) (*EngineResult, error) {
	return e.ManualTransition(withIfMatch(ctx, ifMatch), entity, transitionName)
}

// ManualTransition fires a named transition on an existing entity and cascades
// any automated transitions from the resulting state.
func (e *Engine) ManualTransition(ctx context.Context, entity *spi.Entity, transitionName string) (*EngineResult, error) {
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

	// No workflows defined → use embedded default. Body warning surfaces to
	// the client; slog.Warn surfaces to operators.
	if len(workflows) == 0 {
		common.AddWarning(ctx, "no workflows imported for model — using default workflow")
		e.logDefaultFallback(ctx, entity, "no_workflows_imported")
		workflows = e.defaultWorkflows
	}

	wf := e.findWorkflowForState(workflows, entity.Meta.State)
	if wf == nil {
		return nil, fmt.Errorf("no workflow contains state %q for model %s", entity.Meta.State, entity.Meta.ModelRef)
	}

	currentCtx, currentTxID, err := e.attemptTransition(ctx, entity, wf, transitionName, auditStore, txID)
	if err != nil {
		return nil, err
	}

	currentCtx, currentTxID, err = e.cascadeAutomated(currentCtx, entity, wf, auditStore, currentTxID)
	if err != nil {
		return nil, err
	}

	// Arm/cancel the settled state's scheduled tasks — same FINAL ctx/txID
	// treatment as Execute, atomic with the entity write.
	if err := e.reconcileScheduledTasks(currentCtx, entity, wf, currentTxID, auditStore, ""); err != nil {
		return nil, fmt.Errorf("failed to reconcile scheduled tasks: %w", err)
	}

	e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventFinished, "Manual transition finished", map[string]any{"success": true})

	return &EngineResult{
		ExecutionResult: &spi.ExecutionResult{
			State:   entity.Meta.State,
			Success: true,
		},
		FinalCtx:  currentCtx,
		FinalTxID: currentTxID,
		Segmented: currentTxID != txID,
	}, nil
}

// LoopbackWithIfMatch is the variant of Loopback used by callers that supply
// an If-Match expected-txID. The engine consumes ifMatch on the FIRST
// segment-flush of any COMMIT_BEFORE_DISPATCH cascade encountered during the
// loopback (spec §4.1), so a stale If-Match aborts before any external
// dispatch fires. For loopback runs that produce no engine-side flush
// (the common case — no CBD processors), ifMatch is left untouched on the
// context for the handler to apply post-engine. Callers distinguish via
// EngineResult.Segmented.
//
// If ifMatch is empty this method is identical to Loopback.
func (e *Engine) LoopbackWithIfMatch(ctx context.Context, entity *spi.Entity, ifMatch string) (*EngineResult, error) {
	return e.Loopback(withIfMatch(ctx, ifMatch), entity)
}

// Loopback re-evaluates automated transitions from the entity's current state
// without firing a specific named transition. This is used when entity data is
// updated and the workflow should re-check conditions from the current state.
func (e *Engine) Loopback(ctx context.Context, entity *spi.Entity) (*EngineResult, error) {
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

	// No workflows defined → use embedded default. Body warning surfaces to
	// the client; slog.Warn surfaces to operators.
	if len(workflows) == 0 {
		common.AddWarning(ctx, "no workflows imported for model — using default workflow")
		e.logDefaultFallback(ctx, entity, "no_workflows_imported")
		workflows = e.defaultWorkflows
	}

	wf := e.findWorkflowForState(workflows, entity.Meta.State)
	if wf == nil {
		// Current state not in any workflow — stable, nothing to do.
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventForcedSuccess, "No workflow contains current state for loopback", nil)
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventFinished, "Loopback finished (state not in workflow)", map[string]any{"success": true})
		return &EngineResult{
			ExecutionResult: &spi.ExecutionResult{
				State:      entity.Meta.State,
				Success:    true,
				StopReason: "STATE_NOT_IN_WORKFLOW",
			},
			FinalCtx:  ctx,
			FinalTxID: txID,
			Segmented: false,
		}, nil
	}

	currentCtx, currentTxID, err := e.cascadeAutomated(ctx, entity, wf, auditStore, txID)
	if err != nil {
		return nil, err
	}

	// Arm/cancel the settled state's scheduled tasks — same FINAL ctx/txID
	// treatment as Execute/ManualTransition, atomic with the entity write.
	if err := e.reconcileScheduledTasks(currentCtx, entity, wf, currentTxID, auditStore, ""); err != nil {
		return nil, fmt.Errorf("failed to reconcile scheduled tasks: %w", err)
	}

	e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventFinished, "Loopback finished", map[string]any{"success": true})

	return &EngineResult{
		ExecutionResult: &spi.ExecutionResult{
			State:   entity.Meta.State,
			Success: true,
		},
		FinalCtx:  currentCtx,
		FinalTxID: currentTxID,
		Segmented: currentTxID != txID,
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

		matched, reason, err := e.evaluateCriterion(wf.Criterion, entity, &criterionContext{
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

		if reason == "" {
			reason = defaultCriterionReason
		}
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventWorkflowSkipped, fmt.Sprintf("Workflow %q criterion not matched", wf.Name),
			map[string]any{"workflowName": wf.Name, "reason": reason})
	}

	// No imported workflow matched the entity — fall back to the embedded
	// default. Both channels (body warning + slog.Warn) fire.
	if len(e.defaultWorkflows) > 0 {
		common.AddWarning(ctx, "no imported workflow matched entity — using default workflow")
		e.logDefaultFallback(ctx, entity, "no_criterion_matched")
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

// attemptTransition finds and fires a named transition from the entity's
// current state. Returns the (possibly updated) ctx and txID — the cascade
// segment boundary may shift these when a COMMIT_BEFORE_DISPATCH processor
// runs. Audit events continue to use the cascade-entry txID for
// client-correlation continuity (spec §8).
func (e *Engine) attemptTransition(ctx context.Context, entity *spi.Entity, wf *spi.WorkflowDefinition, transitionName string, auditStore spi.StateMachineAuditStore, txID string) (context.Context, string, error) {
	stateDef, ok := wf.States[entity.Meta.State]
	if !ok {
		return ctx, txID, fmt.Errorf("state %q not found in workflow %q", entity.Meta.State, wf.Name)
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
		return ctx, txID, fmt.Errorf("transition %q not found in state %q: %w", transitionName, entity.Meta.State, ErrTransitionNotFound)
	}

	if transition.Disabled {
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventTransitionNotFound, fmt.Sprintf("Transition %q is disabled", transitionName), nil)
		return ctx, txID, fmt.Errorf("transition %q is disabled in state %q: %w", transitionName, entity.Meta.State, ErrTransitionNotFound)
	}

	if transition.Schedule != nil {
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventTransitionNotFound,
			fmt.Sprintf("Transition %q is %s", transitionName, scheduledReason), nil)
		return ctx, txID, fmt.Errorf(
			"transition %q in state %q is %s: %w",
			transitionName, entity.Meta.State, scheduledReason, ErrTransitionNotFound)
	}

	newCtx, newTxID, _, err := e.fireTransition(ctx, entity, wf, transition, auditStore, txID)
	return newCtx, newTxID, err
}

// fireTransition runs the transition *mechanism* for an already-resolved
// transition: criterion evaluation, processor execution, and the audited
// state advance. It applies no policy — callers are responsible for
// rejecting disabled or scheduled transitions before invoking it, so a
// later scheduled-transition firing path can reuse the mechanism without
// going through attemptTransition's manual/scheduled reject policy.
//
// matched reports whether the transition actually fired (criterion matched
// and the state advanced). It is false whenever the criterion evaluated to
// false or processor execution failed; in both cases entity.Meta.State is
// left unchanged and err carries the same error attemptTransition has
// always returned in that case.
func (e *Engine) fireTransition(ctx context.Context, entity *spi.Entity, wf *spi.WorkflowDefinition, transition *spi.TransitionDefinition, auditStore spi.StateMachineAuditStore, txID string) (context.Context, string, bool, error) {
	transitionName := transition.Name

	// Evaluate transition criterion.
	if len(transition.Criterion) > 0 && string(transition.Criterion) != "null" {
		matched, reason, err := e.evaluateCriterion(transition.Criterion, entity, &criterionContext{
			ctx: ctx, txID: txID, workflowName: wf.Name, transitionName: transitionName, target: "TRANSITION",
		})
		if err != nil {
			return ctx, txID, false, fmt.Errorf("failed to evaluate transition criterion: %w", err)
		}
		if !matched {
			external := reason != ""
			if reason == "" {
				reason = defaultCriterionReason
			}
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
				spi.SMEventTransitionCriterionNoMatch,
				fmt.Sprintf("Transition %q criterion not matched", transitionName),
				map[string]any{
					"workflowName": wf.Name,
					"transition":   transitionName,
					"criterion":    criterionName(transition.Criterion),
					"reason":       reason,
				})
			if external {
				return ctx, txID, false, &criterionNotMatchedError{msg: fmt.Sprintf("transition %q criterion not matched: %s", transitionName, reason)}
			}
			return ctx, txID, false, &criterionNotMatchedError{msg: fmt.Sprintf("transition %q criterion not matched", transitionName)}
		}
	}

	// Execute processors. May shift (ctx, txID) for COMMIT_BEFORE_DISPATCH.
	newCtx, newTxID, err := e.executeProcessors(ctx, transition.Processors, entity, auditStore, wf.Name, transitionName, txID)
	if err != nil {
		e.recordEvent(auditStore, newCtx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventStateProcessResult, fmt.Sprintf("Processor failed for transition %q: %v", transitionName, err),
			map[string]any{"success": false})
		return newCtx, newTxID, false, err
	}

	// Record transition and move state. The audit event uses the cascade-entry
	// txID for correlation; it is recorded via newCtx so it lands in whichever
	// segment is currently open.
	e.recordEvent(auditStore, newCtx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventTransitionMade,
		fmt.Sprintf("Transition %q: %s → %s", transitionName, entity.Meta.State, transition.Next), nil)
	entity.Meta.State = transition.Next

	return newCtx, newTxID, true, nil
}

// cascadeAutomated loops through automated transitions until a stable state
// is reached. It enforces both a per-state visit limit and a total cascade
// depth safety net.
//
// Returns the (possibly updated) ctx and txID — the cascade segment boundary
// may shift these when a COMMIT_BEFORE_DISPATCH processor runs (spec §3, §4).
// The cascade-entry txID is preserved for audit-event correlation (spec §8).
func (e *Engine) cascadeAutomated(ctx context.Context, entity *spi.Entity, wf *spi.WorkflowDefinition, auditStore spi.StateMachineAuditStore, txID string) (context.Context, string, error) {
	ctx, cascadeSpan := tracer.Start(ctx, "workflow.cascade", trace.WithAttributes(
		observability.AttrWorkflowName.String(wf.Name),
		observability.AttrEntityID.String(entity.Meta.ID),
	))
	defer cascadeSpan.End()

	currentCtx := ctx
	currentTxID := txID

	stateVisits := make(map[string]int)

	for depth := 0; depth < maxCascadeDepth; depth++ {
		state := entity.Meta.State
		stateVisits[state]++
		if stateVisits[state] > e.maxStateVisits {
			reason := fmt.Sprintf("state %q visited %d times (limit: %d)", state, stateVisits[state], e.maxStateVisits)
			e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, state,
				spi.SMEventCancelled, "State machine aborted: "+reason, nil)
			return currentCtx, currentTxID, fmt.Errorf("state machine aborted: %s", reason)
		}

		stateDef, ok := wf.States[state]
		if !ok {
			return currentCtx, currentTxID, nil // state not in workflow — stable
		}

		fired := false
		for i := range stateDef.Transitions {
			tr := &stateDef.Transitions[i]
			if tr.Disabled || tr.Manual || tr.Schedule != nil {
				continue
			}

			// Evaluate criterion.
			if len(tr.Criterion) > 0 && string(tr.Criterion) != "null" {
				matched, reason, err := e.evaluateCriterion(tr.Criterion, entity, &criterionContext{
					ctx: currentCtx, txID: txID, workflowName: wf.Name, transitionName: tr.Name, target: "TRANSITION",
				})
				if err != nil {
					return currentCtx, currentTxID, fmt.Errorf("failed to evaluate transition criterion: %w", err)
				}
				if !matched {
					if reason == "" {
						reason = defaultCriterionReason
					}
					e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
						spi.SMEventTransitionCriterionNoMatch,
						fmt.Sprintf("Automated transition %q criterion not matched", tr.Name),
						map[string]any{
							"workflowName": wf.Name,
							"transition":   tr.Name,
							"criterion":    criterionName(tr.Criterion),
							"reason":       reason,
						})
					continue
				}
			}

			// Execute processors. May shift (currentCtx, currentTxID) when
			// a COMMIT_BEFORE_DISPATCH processor commits the segment.
			_, trSpan := tracer.Start(currentCtx, "workflow.transition", trace.WithAttributes(
				observability.AttrTransitionName.String(tr.Name),
				observability.AttrStateFrom.String(entity.Meta.State),
				observability.AttrStateTo.String(tr.Next),
				observability.AttrCascadeDepth.Int(depth),
			))
			newCtx, newTxID, err := e.executeProcessors(currentCtx, tr.Processors, entity, auditStore, wf.Name, tr.Name, currentTxID)
			currentCtx = newCtx
			currentTxID = newTxID
			if err != nil {
				trSpan.RecordError(err)
				trSpan.End()
				e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
					spi.SMEventStateProcessResult,
					fmt.Sprintf("Processor failed for transition %q: %v", tr.Name, err),
					map[string]any{"success": false})
				return currentCtx, currentTxID, err
			}

			// Record transition and move state.
			e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
				spi.SMEventTransitionMade,
				fmt.Sprintf("Transition %q: %s → %s", tr.Name, entity.Meta.State, tr.Next), nil)
			entity.Meta.State = tr.Next
			trSpan.End()
			fired = true
			break // restart from new state
		}

		if !fired {
			cascadeSpan.SetAttributes(observability.AttrCascadeDepth.Int(depth))
			return currentCtx, currentTxID, nil // stable state
		}
	}

	reason := fmt.Sprintf("cascade depth exceeded (%d) at state %q", maxCascadeDepth, entity.Meta.State)
	e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventCancelled, "State machine aborted: "+reason, nil)
	return currentCtx, currentTxID, fmt.Errorf("state machine aborted: %s", reason)
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
// service using the provided criterionContext and returns the capped
// compute-node-supplied reason (passed through regardless of match result;
// only consumed on a matched=false rejection). Inline predicates have no such
// explanation and always return an empty reason.
func (e *Engine) evaluateCriterion(criterion []byte, entity *spi.Entity, cc *criterionContext) (bool, string, error) {
	cond, err := predicate.ParseCondition(criterion)
	if err != nil {
		return false, "", fmt.Errorf("failed to parse criterion: %w", err)
	}

	if _, ok := cond.(*predicate.FunctionCondition); ok {
		if e.extProc == nil {
			return false, "", fmt.Errorf("no external processing service configured for FUNCTION criteria")
		}
		// Release any per-tx gate this call chain holds across the blocking
		// FUNCTION-criterion dispatch — same H3 rationale as executeSyncProcessor:
		// the callout can re-enter with a descendant joined callback on the same
		// txID. No-op for the owner / non-joined calls.
		resume := txgate.Suspend(cc.ctx)
		defer resume()
		matches, reason, err := e.extProc.DispatchCriteria(cc.ctx, entity, criterion, cc.target, cc.workflowName, cc.transitionName, "", cc.txID)
		return matches, capReason(reason), err
	}

	matched, err := match.Match(cond, entity.Data, entity.Meta)
	return matched, "", err
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

// logDefaultFallback emits a single slog.Warn line whenever the engine
// substitutes the embedded default workflow. The four call sites map to
// two cause groups via the reason argument:
//   - "no_workflows_imported": cold-path (Execute/ManualTransition/Loopback)
//     with no stored workflows for the model — three call sites.
//   - "no_criterion_matched":  workflows exist but no criterion matched the
//     entity (selectWorkflow tail) — one call site.
//
// The body-level warning via common.AddWarning is retained at each call
// site for client-facing surfacing; this log line is purely additive for
// operational observability.
func (e *Engine) logDefaultFallback(ctx context.Context, entity *spi.Entity, reason string) {
	slog.WarnContext(ctx, "default workflow substituted",
		slog.String("pkg", "workflow"),
		slog.String("tenant", common.TenantFromContext(ctx)),
		slog.String("entityName", entity.Meta.ModelRef.EntityName),
		slog.String("modelVersion", entity.Meta.ModelRef.ModelVersion),
		slog.String("entityId", entity.Meta.ID),
		slog.String("reason", reason))
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
