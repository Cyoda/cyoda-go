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
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
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
// (criterion-evaluation error, processor failure — a failure the scheduler
// records and retries). errors.Is(err, ErrCriterionNotMatched) reports true
// for the error fireTransition returns from either of its
// criterion-not-matched branches.
//
// fireTransition cannot wrap with a plain %w here: the criterion-not-matched
// message is `transition %q criterion not matched`, with `: <reason>`
// appended when the reason is not the default, and %w appends the
// sentinel's own "criterion not matched" text, which would change that
// message. criterionNotMatchedError instead carries the exact text in
// Error() while still satisfying errors.Is via a custom Is method.
var ErrCriterionNotMatched = errors.New("criterion not matched")

// criterionNotMatchedError carries fireTransition's criterion-not-matched
// text while satisfying errors.Is(err, ErrCriterionNotMatched).
type criterionNotMatchedError struct {
	msg string
}

func (e *criterionNotMatchedError) Error() string { return e.msg }

// Is reports whether target is ErrCriterionNotMatched, so callers can use
// errors.Is instead of a type assertion.
func (e *criterionNotMatchedError) Is(target error) bool {
	return target == ErrCriterionNotMatched
}

// ErrCriterionTypingInfra marks a server-side failure while loading the
// model schema a criterion needs to compare data leaves by their declared
// types. The model store being unavailable is not attributable to the
// caller's input, so callers map this to a sanitized 5xx instead of letting
// raw store text reach a 4xx body — the same treatment
// ErrProcessorOutputInfra and ErrCommitBeforeDispatchInfra already get.
//
// This deliberately covers "model not found" as well as a store outage: an
// entity whose model has been deleted out from under it is a server-side
// inconsistency, not something the caller's request got wrong. Detail is
// still available to operators — the cause stays wrapped and is logged with
// the ticket UUID — it just no longer ships in a 4xx body. Same call
// ErrProcessorOutputInfra already makes for its missing-descriptor case.
var ErrCriterionTypingInfra = errors.New("criterion typing failed")

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
// audit is durable storage, so it is capped defensively — but the bound
// expected to bite is the member-side one (internal/grpc's
// MaxCriterionReasonRunes, 2048 runes, marked with "…"), applied upstream of
// this. 8195 is that bound's worst case in bytes — 2048 runes at up to 4
// bytes each plus the 3-byte ellipsis — so this cap cannot silently cut a
// second time, unmarked, for text made of multi-byte runes.
const maxCriterionReasonLen = 8195

// defaultCriterionReason is recorded when a criterion returns false without an
// explanatory reason: a criterion without a function condition, or a
// whole-criterion function whose member gave no reason. Keeps the audit
// data.reason shape stable.
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
	// and for FireScheduledTransition's deadline decisions. Defaults to
	// time.Now; overridden via WithScheduledClock for deterministic tests.
	clock func() time.Time
	// commitBudget bounds flushAndCommitSegment's shielded CBD-segment
	// commit (common.ShieldedCommitWithBudget). Defaults to
	// common.CommitBudget (the same 30s production budget
	// txScope.commitOwned uses on the entity-handler side); overridden via
	// WithCommitBudget so a test can observe the common.ErrCommitInterrupted
	// wrap firing for real without waiting out the production budget.
	commitBudget time.Duration
	// runTxs maps each transaction a scheduled run began to the run's guard.
	runTxs runTxGuards
}

// NewEngine creates a new workflow engine. txMgr is required and must not be
// nil: the engine takes savepoints and rolls back segments through it.
func NewEngine(factory spi.StoreFactory, uuids spi.UUIDGenerator, txMgr spi.TransactionManager, opts ...EngineOption) *Engine {
	e := &Engine{factory: factory, uuids: uuids, txMgr: txMgr, maxStateVisits: defaultMaxStateVisits, clock: time.Now, commitBudget: common.CommitBudget}
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
// reconcileScheduledTasks' scheduledTime/armedAt computation and
// FireScheduledTransition's deadline decisions. Defaults to time.Now; tests
// inject a deterministic clock instead.
func WithScheduledClock(clock func() time.Time) EngineOption {
	return func(e *Engine) {
		if clock != nil {
			e.clock = clock
		}
	}
}

// WithCommitBudget overrides the budget flushAndCommitSegment's shielded CBD
// commit is bound by. Defaults to common.CommitBudget (30s); tests inject a
// short budget to observe common.ErrCommitInterrupted firing for real
// instead of waiting out the production value.
func WithCommitBudget(budget time.Duration) EngineOption {
	return func(e *Engine) {
		if budget > 0 {
			e.commitBudget = budget
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

// rollbackSegment releases a transaction segment the engine opened and never
// handed back to its caller. It is a no-op for the caller's own entry
// transaction — that one is the caller's to commit or roll back.
//
// Two guard shapes call this. The entry points (Execute, ManualTransition,
// Loopback) keep dedicated openCtx/openTxID locals and a handedOff flag, so they
// release the segment on error returns as well as panics. The intermediate frames
// (fireTransition, executeProcessors, cascadeAutomated) hand their segment back on
// every normal return, so their guards fire only when a nil named ctx return says
// the stack is unwinding through a panic. None of them recover: surviving a panic
// is the request door's decision, not the engine's.
func (e *Engine) rollbackSegment(ctx context.Context, openTxID, entryTxID string) {
	if openTxID == "" || openTxID == entryTxID {
		return
	}
	rbCtx, cancel := common.RollbackContext(ctx)
	defer cancel()
	if err := e.txMgr.Rollback(rbCtx, openTxID); err != nil && !errors.Is(err, spi.ErrTxNotFound) {
		slog.Warn("failed to roll back engine-opened segment",
			"pkg", "workflow", "txID", openTxID, "err", err)
	}
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

	auditStore, err := e.factory.StateMachineAuditStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get audit store: %w", err)
	}

	txID := e.resolveAuditTxID(entity)

	// The engine owns every segment it opens after entryTxID until it hands one
	// back on the success return. openCtx/openTxID are dedicated locals, NOT the
	// named returns: every failure path in executeCommitBeforeDispatch is
	// `return nil, "", err`, so a guard reading a named newTxID return would see
	// "" on exactly the paths that need it and skip the rollback.
	//
	// entryTxID is resolveAuditTxID's value, which equals the handler's
	// transaction because every handler stamps Meta.TransactionID = txID before
	// calling in. flushAndCommitSegment's Commit(ctx, txID) already relies on the
	// same invariant.
	//
	// Invariant: attemptTransition and fireTransition return their INPUT ctx/txID on
	// every early exit, so openTxID != entryTxID cannot be true unless a processor
	// actually segmented. Processors run after criteria; reordering them would break
	// this guard silently.
	entryTxID := txID
	openCtx, openTxID := ctx, txID
	handedOff := false
	defer func() {
		if !handedOff {
			e.rollbackSegment(openCtx, openTxID, entryTxID)
		}
	}()

	// Record STARTED.
	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventStarted, "State machine started", nil)

	selectedWF, modelScheduled, err := e.resolveWorkflow(ctx, entity, auditStore, txID)
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
		openCtx, openTxID = currentCtx, currentTxID
		if err != nil {
			return nil, err
		}
	}

	// Cascade automated transitions.
	nCtx, nTxID, err := e.cascadeAutomated(currentCtx, entity, selectedWF, auditStore, currentTxID)
	currentCtx = nCtx
	currentTxID = nTxID
	openCtx, openTxID = currentCtx, currentTxID
	if err != nil {
		return nil, err
	}

	// Arm/cancel the settled state's scheduled tasks. Runs after the cascade
	// settles, using the FINAL ctx/txID — the write joins whatever
	// transaction currentCtx carries, atomic with the entity write it just
	// cascaded into.
	if err := e.reconcileScheduledTasks(currentCtx, entity, selectedWF, modelScheduled, currentTxID, auditStore); err != nil {
		// Already self-describing, and marked ErrScheduledTaskInfra when the
		// store is what failed — re-wrapping only doubles the phrase.
		return nil, err
	}

	// Record FINISHED. Recorded via currentCtx so it lands in whichever segment
	// is currently open; cascade-entry txID for client-correlation continuity
	// (spec §8).
	e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventFinished, "State machine finished", map[string]any{"success": true})

	handedOff = true
	return &EngineResult{
		ExecutionResult: &spi.ExecutionResult{
			State:   entity.Meta.State,
			Success: true,
		},
		FinalCtx:  currentCtx,
		FinalTxID: currentTxID,
	}, nil
}

// ManualTransitionWithIfMatch is ManualTransition under a caller's If-Match.
// The engine records the transition's start and then checks the
// precondition, before it selects a workflow or runs any criterion or
// processor. A mismatch records TRANSITION_ABORTED and fails with
// spi.ErrConflict (see IfMatch and checkIfMatch).
func (e *Engine) ManualTransitionWithIfMatch(ctx context.Context, entity *spi.Entity, transitionName string, ifMatch IfMatch) (*EngineResult, error) {
	return e.manualTransition(ctx, entity, transitionName, ifMatch)
}

// ManualTransition fires a named transition on an existing entity and cascades
// any automated transitions from the resulting state.
func (e *Engine) ManualTransition(ctx context.Context, entity *spi.Entity, transitionName string) (*EngineResult, error) {
	return e.manualTransition(ctx, entity, transitionName, IfMatch{})
}

func (e *Engine) manualTransition(ctx context.Context, entity *spi.Entity, transitionName string, ifMatch IfMatch) (*EngineResult, error) {
	ctx, span := tracer.Start(ctx, "workflow.manual_transition", trace.WithAttributes(
		observability.AttrEntityID.String(entity.Meta.ID),
		observability.AttrEntityModel.String(entity.Meta.ModelRef.String()),
		observability.AttrTransitionName.String(transitionName),
		observability.AttrEntityState.String(entity.Meta.State),
	))
	defer span.End()

	auditStore, err := e.factory.StateMachineAuditStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get audit store: %w", err)
	}

	txID := e.resolveAuditTxID(entity)

	// Same ownership guard as Execute — see the comment there for why these are
	// dedicated locals and why openTxID != entryTxID is a sound segmentation test.
	entryTxID := txID
	openCtx, openTxID := ctx, txID
	handedOff := false
	defer func() {
		if !handedOff {
			e.rollbackSegment(openCtx, openTxID, entryTxID)
		}
	}()

	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventStarted, "Manual transition started", nil)
	if err := e.checkIfMatch(ctx, auditStore, entity, txID, transitionName, ifMatch); err != nil {
		return nil, err
	}

	// Select the workflow the entity's criterion binds it to. If the
	// entity's current state is absent from that definition, attemptTransition
	// below rejects the call — the engine never falls through to another
	// definition that happens to declare the state.
	wf, modelScheduled, err := e.resolveWorkflow(ctx, entity, auditStore, txID)
	if err != nil {
		return nil, err
	}

	currentCtx, currentTxID, err := e.attemptTransition(ctx, entity, wf, transitionName, auditStore, txID)
	openCtx, openTxID = currentCtx, currentTxID
	if err != nil {
		return nil, err
	}

	currentCtx, currentTxID, err = e.cascadeAutomated(currentCtx, entity, wf, auditStore, currentTxID)
	openCtx, openTxID = currentCtx, currentTxID
	if err != nil {
		return nil, err
	}

	// Arm/cancel the settled state's scheduled tasks — same FINAL ctx/txID
	// treatment as Execute, atomic with the entity write.
	if err := e.reconcileScheduledTasks(currentCtx, entity, wf, modelScheduled, currentTxID, auditStore); err != nil {
		// Already self-describing, and marked ErrScheduledTaskInfra when the
		// store is what failed — re-wrapping only doubles the phrase.
		return nil, err
	}

	e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventFinished, "Manual transition finished", map[string]any{"success": true})

	handedOff = true
	return &EngineResult{
		ExecutionResult: &spi.ExecutionResult{
			State:   entity.Meta.State,
			Success: true,
		},
		FinalCtx:  currentCtx,
		FinalTxID: currentTxID,
	}, nil
}

// LoopbackWithIfMatch is Loopback under a caller's If-Match, checked as
// ManualTransitionWithIfMatch checks it.
func (e *Engine) LoopbackWithIfMatch(ctx context.Context, entity *spi.Entity, ifMatch IfMatch) (*EngineResult, error) {
	return e.loopback(ctx, entity, ifMatch)
}

// Loopback re-evaluates automated transitions from the entity's current state
// without firing a specific named transition. This is used when entity data is
// updated and the workflow should re-check conditions from the current state.
func (e *Engine) Loopback(ctx context.Context, entity *spi.Entity) (*EngineResult, error) {
	return e.loopback(ctx, entity, IfMatch{})
}

func (e *Engine) loopback(ctx context.Context, entity *spi.Entity, ifMatch IfMatch) (*EngineResult, error) {
	ctx, span := tracer.Start(ctx, "workflow.loopback", trace.WithAttributes(
		observability.AttrEntityID.String(entity.Meta.ID),
		observability.AttrEntityModel.String(entity.Meta.ModelRef.String()),
		observability.AttrEntityState.String(entity.Meta.State),
	))
	defer span.End()

	auditStore, err := e.factory.StateMachineAuditStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get audit store: %w", err)
	}

	txID := e.resolveAuditTxID(entity)

	// Same ownership guard as Execute — see the comment there.
	entryTxID := txID
	openCtx, openTxID := ctx, txID
	handedOff := false
	defer func() {
		if !handedOff {
			e.rollbackSegment(openCtx, openTxID, entryTxID)
		}
	}()

	e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventStarted, "Loopback started", nil)
	if err := e.checkIfMatch(ctx, auditStore, entity, txID, "loopback", ifMatch); err != nil {
		return nil, err
	}

	wf, modelScheduled, err := e.resolveWorkflow(ctx, entity, auditStore, txID)
	if err != nil {
		return nil, err
	}

	if _, ok := wf.States[entity.Meta.State]; !ok {
		// Current state not in the SELECTED workflow — stable, nothing to
		// cascade. Another definition declaring the state is not a reason to
		// cascade it: the entity is bound to the workflow its criterion
		// selected.
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventForcedSuccess,
			fmt.Sprintf("Current state is not declared in the selected workflow %q — nothing to loop back", wf.Name), nil)
		// Its tasks are still reconciled: nothing can be armed for a state the
		// selected workflow does not declare, so every task of the entity is
		// removed.
		if err := e.reconcileScheduledTasks(ctx, entity, wf, modelScheduled, txID, auditStore); err != nil {
			return nil, err
		}
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventFinished, "Loopback finished (state not in workflow)", map[string]any{"success": true})
		handedOff = true
		return &EngineResult{
			ExecutionResult: &spi.ExecutionResult{
				State:      entity.Meta.State,
				Success:    true,
				StopReason: "STATE_NOT_IN_WORKFLOW",
			},
			FinalCtx:  ctx,
			FinalTxID: txID,
		}, nil
	}

	currentCtx, currentTxID, err := e.cascadeAutomated(ctx, entity, wf, auditStore, txID)
	openCtx, openTxID = currentCtx, currentTxID
	if err != nil {
		return nil, err
	}

	// Arm/cancel the settled state's scheduled tasks — same FINAL ctx/txID
	// treatment as Execute/ManualTransition, atomic with the entity write.
	if err := e.reconcileScheduledTasks(currentCtx, entity, wf, modelScheduled, currentTxID, auditStore); err != nil {
		// Already self-describing, and marked ErrScheduledTaskInfra when the
		// store is what failed — re-wrapping only doubles the phrase.
		return nil, err
	}

	e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventFinished, "Loopback finished", map[string]any{"success": true})

	handedOff = true
	return &EngineResult{
		ExecutionResult: &spi.ExecutionResult{
			State:   entity.Meta.State,
			Success: true,
		},
		FinalCtx:  currentCtx,
		FinalTxID: currentTxID,
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

		out, err := e.evaluateCriterion(wf.Criterion, entity, &criterionContext{
			ctx: ctx, txID: txID, workflowName: wf.Name, target: "WORKFLOW",
		})
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate workflow criterion for %q: %w", wf.Name, err)
		}
		if out.matched {
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
				spi.SMEventWorkflowFound, fmt.Sprintf("Workflow %q matched criterion", wf.Name), out.functionsData())
			return wf, nil
		}

		data := map[string]any{"workflowName": wf.Name, "reason": out.notMatchedReason()}
		out.addFunctions(data)
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventWorkflowSkipped, fmt.Sprintf("Workflow %q criterion not matched", wf.Name), data)
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

// resolveWorkflow loads the model's workflow definitions and returns the one
// that applies to entity, per the documented workflow-level selection rules
// (`cyoda help workflows`): stored declaration order, active definitions
// only, the first matching `criterion` wins, and the embedded default
// workflow as the fallback when nothing matches.
//
// This is the ONLY workflow-resolution path in the engine. Every door goes
// through it — creation (Execute), manual firing (ManualTransition),
// re-evaluation (Loopback), the scheduler's fire door
// (FireScheduledTransition) and the transitions query
// (GetAvailableTransitionsForEntity) — so an entity always runs the
// definition its own criterion selects, and the WORKFLOW_SKIP /
// WORKFLOW_FOUND audit trail is recorded identically on all of them.
//
// Resolving instead by "the first active workflow that happens to declare
// the entity's current state" cannot distinguish definitions that share
// state names — the normal shape for a per-kind machine — and silently
// binds every entity to the first declared workflow, running the wrong
// guards, processors and target states.
//
// Selection is per call and never cached: the criterion is evaluated against
// the entity as it is right now, which is what makes a data change able to
// re-bind an entity to a different definition.
//
// It also reports modelScheduled: whether any workflow of the model, active
// or not, has a transition the arm rule arms (modelHasSchedule). It counts
// every workflow the entity can be bound to: the stored ones and the default
// workflow, which selection falls back to when none is stored or none
// matches.
func (e *Engine) resolveWorkflow(ctx context.Context, entity *spi.Entity, auditStore spi.StateMachineAuditStore, txID string) (*spi.WorkflowDefinition, bool, error) {
	return e.resolveWorkflowWith(ctx, entity, auditStore, txID)
}

// resolveWorkflowForQuery is resolveWorkflow for read-only callers: it
// discards the selection audit events, which belong to the transaction
// executing the entity and have nothing to key to on a read.
//
// It deliberately does NOT suppress the operator-facing
// default-substitution WARN. Emitting one per read is more log volume than
// an execution path produces, and that was reason enough to consider
// silencing it — but common.AddWarning cannot compensate here: the
// diagnostics bag is installed only on the gRPC entry points
// (internal/grpc/*), the transitions endpoints are HTTP-only, and
// common.WriteJSON emits a bare array with no warnings field. Silencing the
// log would leave a read that answered from the default workflow with no
// signal on any channel at all.
func (e *Engine) resolveWorkflowForQuery(ctx context.Context, entity *spi.Entity) (*spi.WorkflowDefinition, error) {
	wf, _, err := e.resolveWorkflowWith(ctx, entity, discardedAuditStore{}, "")
	return wf, err
}

func (e *Engine) resolveWorkflowWith(ctx context.Context, entity *spi.Entity, auditStore spi.StateMachineAuditStore, txID string) (*spi.WorkflowDefinition, bool, error) {
	// Store failures are server-side conditions, never attributable to the
	// caller's input, so they are minted as sanitized 5xx AppErrors here
	// rather than left as bare errors: callers classify a bare engine error
	// as 400 WORKFLOW_FAILED with err.Error() in the body, which would put
	// raw store text (pgx messages, connection detail) into a 4xx response.
	// Same treatment the engine already gives its other infra failures
	// (ErrCommitBeforeDispatchInfra, ErrProcessorOutputInfra).
	wfStore, err := e.factory.WorkflowStore(ctx)
	if err != nil {
		return nil, false, common.Internal("failed to access workflow store", err)
	}

	// Load workflows for model. A "not found" error is treated as empty.
	workflows, err := wfStore.Get(ctx, entity.Meta.ModelRef)
	if err != nil && errors.Is(err, spi.ErrNotFound) {
		workflows = nil
	} else if err != nil {
		return nil, false, common.Internal("failed to load workflows", err)
	}

	// No workflows defined → use embedded default. Body warning surfaces to
	// the client; slog.Warn surfaces to operators.
	if len(workflows) == 0 {
		common.AddWarning(ctx, "no workflows imported for model — using default workflow")
		e.logDefaultFallback(ctx, entity, "no_workflows_imported")
		workflows = e.defaultWorkflows
	}
	// Counts every workflow the entity can be bound to: the stored ones and
	// the default workflow, which selection falls back to when none is stored
	// or none matches.
	modelScheduled := modelHasSchedule(workflows) || modelHasSchedule(e.defaultWorkflows)

	wf, err := e.selectWorkflow(ctx, workflows, entity, auditStore, txID)
	if err != nil {
		return nil, false, err
	}
	return wf, modelScheduled, nil
}

// discardedAuditStore is the audit sink used by read-only paths. Workflow
// selection records WORKFLOW_SKIP / WORKFLOW_FOUND against the transaction
// that is executing the entity; a query has no such transaction, so the
// events would land keyed to an empty transaction ID and pollute the
// entity's trail. Recording is discarded rather than skipped by a nil check
// so resolveWorkflow keeps one unconditional code path.
type discardedAuditStore struct{}

func (discardedAuditStore) Record(context.Context, string, spi.StateMachineEvent) error { return nil }

func (discardedAuditStore) GetEvents(context.Context, string) ([]spi.StateMachineEvent, error) {
	return nil, nil
}

func (discardedAuditStore) GetEventsByTransaction(context.Context, string, string) ([]spi.StateMachineEvent, error) {
	return nil, nil
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

	newCtx, newTxID, err := e.fireTransition(ctx, entity, wf, transition, auditStore, txID)
	return newCtx, newTxID, err
}

// fireTransition runs the transition *mechanism* for an already-resolved
// transition: criterion evaluation, processor execution, and the audited
// state advance. It applies no policy — callers are responsible for
// rejecting disabled or scheduled transitions before invoking it, so
// FireScheduledTransition reuses the mechanism without going through
// attemptTransition's manual/scheduled reject policy. On any error
// entity.Meta.State is left unchanged.
func (e *Engine) fireTransition(ctx context.Context, entity *spi.Entity, wf *spi.WorkflowDefinition, transition *spi.TransitionDefinition, auditStore spi.StateMachineAuditStore, txID string) (retCtx context.Context, retTxID string, retErr error) {
	transitionName := transition.Name

	// Panic-only guard (see rollbackSegment). segCtx/segTxID track the segment
	// this frame holds once processors have run.
	segCtx, segTxID := ctx, txID
	defer func() {
		if retCtx == nil {
			e.rollbackSegment(segCtx, segTxID, txID)
		}
	}()

	// Evaluate transition criterion.
	var criterionData map[string]any // TRANSITION_MAKE data
	if len(transition.Criterion) > 0 && string(transition.Criterion) != "null" {
		// ctx/txID here are already correct: they are this function's own
		// inputs, and nothing can have segmented yet — processors (the only
		// thing that can open a new segment) run after the criterion below.
		out, err := e.evaluateCriterion(transition.Criterion, entity, &criterionContext{
			ctx: ctx, txID: txID, workflowName: wf.Name, transitionName: transitionName, target: "TRANSITION",
		})
		if err != nil {
			return ctx, txID, fmt.Errorf("failed to evaluate transition criterion: %w", err)
		}
		if !out.matched {
			reason := out.notMatchedReason()
			data := map[string]any{
				"workflowName": wf.Name,
				"transition":   transitionName,
				"criterion":    criterionName(transition.Criterion),
				"reason":       reason,
			}
			out.addFunctions(data)
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
				spi.SMEventTransitionCriterionNoMatch,
				fmt.Sprintf("Transition %q criterion not matched", transitionName), data)
			// The manual path rolls back, so this message — not the audit
			// event — is where the caller learns why.
			if reason != defaultCriterionReason {
				return ctx, txID, &criterionNotMatchedError{msg: fmt.Sprintf("transition %q criterion not matched: %s", transitionName, reason)}
			}
			return ctx, txID, &criterionNotMatchedError{msg: fmt.Sprintf("transition %q criterion not matched", transitionName)}
		}
		criterionData = out.functionsData()
	}

	// Execute processors. May shift (ctx, txID) for COMMIT_BEFORE_DISPATCH.
	newCtx, newTxID, err := e.executeProcessors(ctx, transition.Processors, entity, auditStore, wf.Name, transitionName, txID)
	// Advance before checking err so the deferred rollback always targets the
	// segment actually open.
	segCtx, segTxID = newCtx, newTxID
	if err != nil {
		e.recordEvent(auditStore, newCtx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventStateProcessResult, fmt.Sprintf("Processor failed for transition %q: %v", transitionName, err),
			map[string]any{"success": false})
		return newCtx, newTxID, err
	}

	// Record transition and move state. The audit event uses the cascade-entry
	// txID for correlation; it is recorded via newCtx so it lands in whichever
	// segment is currently open.
	e.recordEvent(auditStore, newCtx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventTransitionMade,
		fmt.Sprintf("Transition %q: %s → %s", transitionName, entity.Meta.State, transition.Next), criterionData)
	entity.Meta.State = transition.Next

	return newCtx, newTxID, nil
}

// cascadeAutomated loops through automated transitions until a stable state
// is reached. It enforces both a per-state visit limit and a total cascade
// depth safety net.
//
// Returns the (possibly updated) ctx and txID — the cascade segment boundary
// may shift these when a COMMIT_BEFORE_DISPATCH processor runs (spec §3, §4).
// The cascade-entry txID is preserved for audit-event correlation (spec §8).
func (e *Engine) cascadeAutomated(ctx context.Context, entity *spi.Entity, wf *spi.WorkflowDefinition, auditStore spi.StateMachineAuditStore, txID string) (retCtx context.Context, retTxID string, retErr error) {
	ctx, cascadeSpan := tracer.Start(ctx, "workflow.cascade", trace.WithAttributes(
		observability.AttrWorkflowName.String(wf.Name),
		observability.AttrEntityID.String(entity.Meta.ID),
	))
	defer cascadeSpan.End()

	currentCtx := ctx
	currentTxID := txID

	// Panic-only guard (see rollbackSegment). The loop can segment on any
	// iteration and then fail — or blow up — several iterations later, all
	// before the caller has seen the new txID.
	defer func() {
		if retCtx == nil {
			e.rollbackSegment(currentCtx, currentTxID, txID)
		}
	}()

	stateVisits := make(map[string]int)

	for depth := 0; depth < maxCascadeDepth; depth++ {
		// Spec D9: observe cancellation/expiry between transitions, the same
		// way an attemptTransition failure is routed, so the deferred
		// rollback guard above still runs. Naturally inert once the cascade
		// is running on a post-CBD ctx — commitAndBeginNextSegment derives
		// that one via context.WithoutCancel, which carries no deadline of
		// its own, so an already-elapsed ORIGINAL client deadline cannot
		// abort a cascade continuing past a segment commit (spec D3).
		if err := currentCtx.Err(); err != nil {
			return currentCtx, currentTxID, fmt.Errorf("cascade aborted: %w", err)
		}
		// Checkpoint before each cascade step (spec §5.3). ctx.Err above is
		// inert after a segment commit; the guard is not.
		if err := runCheckpoint(currentCtx, "cascade aborted"); err != nil {
			return currentCtx, currentTxID, err
		}

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
			var criterionData map[string]any // TRANSITION_MAKE data
			if len(tr.Criterion) > 0 && string(tr.Criterion) != "null" {
				// currentTxID, not txID: after a COMMIT_BEFORE_DISPATCH segment
				// the cascade-entry txID names a committed transaction, and
				// this value is the compute node's join token — a callback
				// joining on it would get ErrTxNotFound. Audit correlation
				// keeps using the entry txID; that is a separate concern from
				// transaction identity.
				out, err := e.evaluateCriterion(tr.Criterion, entity, &criterionContext{
					ctx: currentCtx, txID: currentTxID, workflowName: wf.Name, transitionName: tr.Name, target: "TRANSITION",
				})
				if err != nil {
					return currentCtx, currentTxID, fmt.Errorf("failed to evaluate transition criterion: %w", err)
				}
				if !out.matched {
					data := map[string]any{
						"workflowName": wf.Name,
						"transition":   tr.Name,
						"criterion":    criterionName(tr.Criterion),
						"reason":       out.notMatchedReason(),
					}
					out.addFunctions(data)
					e.recordEvent(auditStore, currentCtx, entity.Meta.ID, txID, entity.Meta.State,
						spi.SMEventTransitionCriterionNoMatch,
						fmt.Sprintf("Automated transition %q criterion not matched", tr.Name), data)
					continue
				}
				criterionData = out.functionsData()
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
				fmt.Sprintf("Transition %q: %s → %s", tr.Name, entity.Meta.State, tr.Next), criterionData)
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

// evaluateCriterion evaluates a criterion against the entity in two phases.
// The compile phase splits it at its function conditions
// (buildCriterionTree) and checks and prepares every local unit — each
// function-free subtree and each pre-check — against the model; no callout
// is made until all of it has passed. The run phase evaluates the tree:
// groups in declaration order with the stop rule, and each function
// condition reached by its pre-check and, when that matches, one callout
// with its own JSON. A failed callout fails the evaluation.
func (e *Engine) evaluateCriterion(criterion []byte, entity *spi.Entity, cc *criterionContext) (criterionOutcome, error) {
	cond, err := predicate.ParseCondition(criterion)
	if err != nil {
		return criterionOutcome{}, fmt.Errorf("failed to parse criterion: %w", err)
	}
	root, err := buildCriterionTree(cond, "")
	if err != nil {
		return criterionOutcome{}, err
	}
	units, fns := collectCriterion(root)
	if len(fns) > 0 && e.extProc == nil {
		return criterionOutcome{}, fmt.Errorf("no external processing service configured for FUNCTION criteria")
	}
	if err := e.prepareLocalUnits(cc.ctx, entity, units); err != nil {
		return criterionOutcome{}, err
	}
	var out criterionOutcome
	if len(fns) > 0 {
		out.functions = []functionRecord{}
		_, out.whole = root.(*critFunction)
	}
	if out.matched, err = root.eval(e, cc, entity, &out.functions); err != nil {
		return criterionOutcome{}, err
	}
	if out.whole && out.functions[0].called {
		out.reason = out.functions[0].reason
	}
	return out, nil
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
// substitutes the embedded default workflow on a door that actually runs the
// entity. Two call sites, one per cause group, both in the shared resolution
// path:
//   - "no_workflows_imported": the model has no stored workflows
//     (resolveWorkflowWith).
//   - "no_criterion_matched":  workflows exist but none matched the entity
//     (selectWorkflow tail).
//
// Read paths log too (see resolveWorkflowForQuery): on the HTTP-only
// transitions endpoints this line is the ONLY channel the substitution
// surfaces on, since the diagnostics bag common.AddWarning writes to is
// installed on the gRPC entry points only.
//
// The body-level warning via common.AddWarning is raised on every path for
// client-facing surfacing wherever a diagnostics bag exists; this log line
// is purely additive for operational observability.
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
	// A chain the fence refuses performs no store operation of any kind, an
	// audit row included: on PostgreSQL it would be a statement on the
	// operation's connection, after the owner has stopped waiting for this
	// chain. The guard is here, and not at the error paths that record, so that
	// no further site can appear later.
	if fence.Check(ctx) != nil {
		return
	}
	event := spi.StateMachineEvent{
		EventType:     eventType,
		EntityID:      entityID,
		State:         state,
		TransactionID: txID,
		Details:       details,
		Data:          data,
		// The engine's clock, not time.Now: scheduled-transition timings are
		// computed from e.now(), so stamping audit events from a different
		// source makes the two incomparable whenever a clock is injected.
		Timestamp: e.now(),
	}
	event.Attributed, event.Executor = spi.AttributionFor(ctx)
	// Best-effort recording; audit failures do not break workflow execution.
	// Logged rather than dropped: audit events are bound to the transaction
	// on every backend, so a Record failure caused by the transaction's own
	// state (rolled back, already committed) mirrors what the entity write
	// in the same transaction would see. A failure local to the audit call
	// itself (no id generator configured, an event that cannot be written)
	// is not otherwise correlated with the entity write, so a silently
	// missing audit trail is still the kind of thing that is only ever
	// noticed long after it mattered.
	if err := auditStore.Record(ctx, entityID, event); err != nil {
		slog.Warn("state-machine audit event not recorded",
			"pkg", "workflow", "entityId", entityID, "eventType", eventType, "err", err)
	}
}
