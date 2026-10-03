package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// ScheduledOutcome reports how FireScheduledTransition ended one claimed run.
type ScheduledOutcome string

const (
	// OutcomeFired: the transition fired and the run committed.
	OutcomeFired ScheduledOutcome = "fired"
	// OutcomeDeclined: the criterion evaluated false; the task was removed.
	OutcomeDeclined ScheduledOutcome = "declined"
	// OutcomeExpired: late on its first attempt; the task was removed with
	// SCHEDULED_TRANSITION_EXPIRE.
	OutcomeExpired ScheduledOutcome = "expired"
	// OutcomeCancelled: the task was removed without firing — the entity is
	// gone or moved on, the transition is no longer scheduled in the selected
	// workflow, or the entity has no transaction id to guard the fire.
	OutcomeCancelled ScheduledOutcome = "cancelled"
	// OutcomeSuperseded: the task's life or claim changed. The run's open
	// transaction did not commit; an earlier COMMIT_BEFORE_DISPATCH segment
	// of the run may have committed. Nothing is recorded.
	OutcomeSuperseded ScheduledOutcome = "superseded"
	// OutcomeFailed: the run did not commit. The scheduler records the
	// outcome (spec §5.6).
	OutcomeFailed ScheduledOutcome = "failed"
)

// RunReport is what the scheduler needs to record a run's outcome (spec
// §5.6). Err is nil when the run committed or was superseded.
type RunReport struct {
	Outcome       ScheduledOutcome
	Err           error
	MarkHeld      bool                           // a MarkUnsafe was accepted in this run
	UnsafeReached bool                           // the in-memory fact of spec §5.5
	MarkErrored   bool                           // the last MarkUnsafe failed with a non-refusal error
	PartialCommit bool                           // a stamp with partial=true committed in this run
	FailReason    spi.ScheduledTaskFailureReason // the engine decided FAILED itself
}

// preRunDecision applies the checks the owner makes on the claimed record
// before it runs (spec §5.1), in order. It returns a failure reason, or
// expire=true for a first attempt past its deadline, or neither.
func preRunDecision(task spi.ScheduledTask, nowMs int64, maxLostOwners int, retryDelay time.Duration) (reason spi.ScheduledTaskFailureReason, expire bool) {
	switch {
	case task.UnsafeMarked:
		return spi.FailureUnsafeWorkNotCompleted, false
	case task.PartialCommit:
		return spi.FailureStoppedAfterPartialCommit, false
	case task.LostOwners >= maxLostOwners:
		return spi.FailureOwnerLostRepeatedly, false
	}
	if task.TimeoutMs == nil {
		return "", false
	}
	deadline := task.ScheduledTime + *task.TimeoutMs
	if task.Attempts == 0 && task.LostOwners == 0 {
		return "", nowMs > deadline
	}
	if nowMs > deadline+retryDelay.Milliseconds() {
		return spi.FailureExpiredAfterFailedAttempts, false
	}
	return "", false
}

// FireScheduledTransition runs one claimed scheduled task. It is the
// scheduler's only door into the engine; no HTTP or gRPC handler calls it.
//
// task is the record ClaimDue returned. ctx carries the system identity of
// task.TenantID and a RunGuard whose Ref is this claim; the scheduler builds
// both. The engine makes the §5.1 decisions on the claimed record, then runs
// the fire in transactions that each start by re-reading the task (§5.2) and
// each write the task row before they commit. It commits the endings that
// remove or re-arm the task (fired, declined, expired, cancelled). It never
// writes RecordAttempt or Fail: the scheduler records every other outcome
// from the returned report (§5.6, §5.7).
func (e *Engine) FireScheduledTransition(ctx context.Context, task spi.ScheduledTask, maxLostOwners int, retryDelay time.Duration) RunReport {
	g := RunGuardFrom(ctx)
	// Releases every transaction the run registered, on every ending,
	// a panic included.
	defer e.runTxs.release(g)
	reason, expire := preRunDecision(task, e.now().UnixMilli(), maxLostOwners, retryDelay)
	if reason != "" {
		return RunReport{Outcome: OutcomeFailed, FailReason: reason}
	}
	outcome, err := e.fireScheduled(ctx, g, task, expire)
	return e.runReport(ctx, g, outcome, err)
}

// runReport turns the run's result into a report. It runs after the run's
// open segment was rolled back (fireScheduled's deferred rollback).
func (e *Engine) runReport(ctx context.Context, g *RunGuard, outcome ScheduledOutcome, err error) RunReport {
	r := RunReport{MarkHeld: g.markHeld, MarkErrored: g.markErrored, UnsafeReached: g.unsafeReached, PartialCommit: g.partialCommitted.Load()}
	switch {
	case err == nil:
		r.Outcome = outcome
	case supersededBy(ctx, g, err):
		r.Outcome = OutcomeSuperseded
	default:
		r.Outcome = OutcomeFailed
		r.Err = err
		r.FailReason = g.failReason
	}
	return r
}

// supersededBy decides whether a failed run was superseded (spec §5.2). A
// refusal or a conflict is classified by re-reading the task with a read that
// does not join any transaction: only the committed state says whether the
// life or the claim changed.
func supersededBy(ctx context.Context, g *RunGuard, err error) bool {
	if errors.Is(err, errRunSuperseded) {
		return true
	}
	if !errors.Is(err, spi.ErrStaleClaim) && !errors.Is(err, spi.ErrConflict) {
		return false
	}
	readCtx := spi.WithTransaction(context.WithoutCancel(ctx), nil)
	cur, found, rerr := g.Store.Get(readCtx, g.Ref.TenantID, g.Ref.ID)
	if rerr != nil {
		// Undecided. Reported as a failure: the scheduler's bookkeeping write
		// is fenced, so it is refused if the run was in fact superseded.
		slog.WarnContext(ctx, "scheduled run: re-read after a refused write failed",
			"pkg", "workflow", "taskId", g.Ref.ID, "err", rerr)
		return false
	}
	return !found || !g.holds(cur)
}

func (e *Engine) fireScheduled(ctx context.Context, g *RunGuard, task spi.ScheduledTask, expire bool) (ScheduledOutcome, error) {
	// The claimed record carries ArmedBy. Seeded before Begin, so that
	// spi.ResolveOrigin inside Begin sees it as the fire's root origin and
	// every write of the cascade inherits it. If the life changed since the
	// claim, the re-read below ends the run before anything is written.
	ctx = spi.WithAmbientOrigin(ctx, task.ArmedBy)

	txID, txCtx, err := e.txMgr.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to begin scheduled-fire transaction: %w", err)
	}
	// The run's guard belongs to its transactions (spec §5.2): a segment
	// commit finds it by the transaction id.
	e.runTxs.register(txID, g)
	// curCtx/curTxID track the open segment. A COMMIT_BEFORE_DISPATCH
	// processor commits the entry segment and opens a new one; every exit
	// that does not commit rolls back the segment open now.
	curCtx, curTxID := txCtx, txID
	committed := false
	defer func() {
		if !committed {
			rbCtx, cancel := common.RollbackContext(curCtx)
			defer cancel()
			_ = e.txMgr.Rollback(rbCtx, curTxID)
		}
	}()
	commit := func(id string) error {
		if err := e.commitRun(ctx, id); err != nil {
			return err
		}
		committed = true
		return nil
	}

	cur, err := rereadTask(txCtx, g)
	if err != nil {
		return "", err
	}
	auditStore, err := e.factory.StateMachineAuditStore(txCtx)
	if err != nil {
		return "", fmt.Errorf("failed to get audit store: %w", err)
	}

	if expire {
		if err := removeOwnLife(txCtx, g); err != nil {
			return "", err
		}
		lateness := e.now().UnixMilli() - cur.ScheduledTime
		e.recordEvent(auditStore, txCtx, cur.EntityID, txID, cur.SourceState,
			spi.SMEventScheduledTransitionExpired,
			fmt.Sprintf("Scheduled transition %q expired (lateness %dms > timeout %dms)",
				cur.Transition, lateness, *task.TimeoutMs), nil)
		slog.InfoContext(txCtx, "scheduled transition expired",
			"pkg", "workflow", "taskId", cur.ID, "entityId", cur.EntityID, "transition", cur.Transition)
		return OutcomeExpired, commit(txID)
	}

	es, err := e.factory.EntityStore(txCtx)
	if err != nil {
		return "", fmt.Errorf("failed to get entity store: %w", err)
	}
	entity, err := es.Get(txCtx, cur.EntityID)
	if err != nil && !errors.Is(err, spi.ErrNotFound) {
		return "", fmt.Errorf("failed to read entity: %w", err)
	}
	if err != nil || entity.Meta.State != cur.SourceState {
		// Gone, or moved on: removed without an audit event (spec §4).
		slog.DebugContext(txCtx, "scheduled task's entity is gone or has left the source state; removing",
			"pkg", "workflow", "taskId", cur.ID, "entityId", cur.EntityID)
		if err := removeOwnLife(txCtx, g); err != nil {
			return "", err
		}
		return OutcomeCancelled, commit(txID)
	}

	// Selection is criterion-based, as on the client doors: a fire runs the
	// definition the entity is bound to now. A resolution failure is a safe
	// failure; it never falls through to another definition.
	wf, modelScheduled, err := e.resolveWorkflow(txCtx, entity, auditStore, txID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve workflow for scheduled fire: %w", err)
	}
	transition := findFireableTransitionInState(wf, entity.Meta.State, cur.Transition)
	if transition == nil {
		// Audited: a vanished timer must be attributable.
		slog.DebugContext(txCtx, "scheduled task references a transition the selected workflow does not declare as scheduled; removing",
			"pkg", "workflow", "entityId", cur.EntityID, "workflowName", wf.Name,
			"sourceState", cur.SourceState, "transition", cur.Transition)
		if err := removeOwnLife(txCtx, g); err != nil {
			return "", err
		}
		e.recordEvent(auditStore, txCtx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventScheduledTransitionCancelled,
			fmt.Sprintf("Scheduled transition %q cancelled (not a scheduled transition of state %q in the selected workflow %q)",
				cur.Transition, cur.SourceState, wf.Name),
			map[string]any{"transition": cur.Transition, "sourceState": cur.SourceState, "workflowName": wf.Name})
		return OutcomeCancelled, commit(txID)
	}

	// Anchor stamp, before any processor can flush the entity: attributed to
	// the arming principal, executed by the system. Both arm sites set
	// ArmedBy from spi.AttributionFor on the arming write's context.
	armed := cur.ArmedBy
	entity.Meta.ChangeUser = armed.ID
	entity.Meta.ChangeUserKind = armed.Kind
	entity.Meta.ChangeExecutor = common.SystemPrincipal()

	// expectedTxID is the entity's last committed transaction id as read in
	// this transaction: the precondition of the final persist.
	expectedTxID := entity.Meta.TransactionID
	if expectedTxID == "" {
		// No precondition to fire under. Refused before any processor runs,
		// because a COMMIT_BEFORE_DISPATCH segment would already have
		// committed. Nothing rewrites a stored transaction id, so the task is
		// removed and audited; a later write re-arms it.
		slog.Error("scheduled fire refused: stored entity carries no transaction ID to guard against",
			"pkg", "workflow", "taskID", cur.ID, "entityID", cur.EntityID)
		if err := removeOwnLife(txCtx, g); err != nil {
			return "", err
		}
		e.recordEvent(auditStore, txCtx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventScheduledTransitionCancelled,
			fmt.Sprintf("Scheduled transition %q cancelled (entity carries no committed transaction ID, so the fire cannot be guarded)",
				cur.Transition),
			map[string]any{"transition": cur.Transition, "sourceState": cur.SourceState})
		return OutcomeCancelled, commit(txID)
	}
	newCtx, newTxID, fireErr := e.fireTransition(txCtx, entity, wf, transition, auditStore, txID)
	curCtx, curTxID = newCtx, newTxID
	if fireErr != nil {
		if errors.Is(fireErr, ErrCriterionNotMatched) {
			// Declined; fireTransition already recorded the criterion event.
			if err := removeOwnLife(newCtx, g); err != nil {
				return "", err
			}
			return OutcomeDeclined, commit(newTxID)
		}
		return "", fireErr
	}
	g.firedTransitionDone.Store(true)

	finalCtx, finalTxID, err := e.cascadeAutomated(newCtx, entity, wf, auditStore, newTxID)
	curCtx, curTxID = finalCtx, finalTxID
	if err != nil {
		return "", err
	}

	finalEntityStore, err := e.factory.EntityStore(finalCtx)
	if err != nil {
		return "", fmt.Errorf("failed to get entity store for persist: %w", err)
	}
	// A joined callback may have written or deleted the fired entity earlier
	// in this transaction; the re-read sees both. A write or delete committed
	// by another transaction is invisible to this transaction's snapshot and
	// fails at commit instead.
	anchor, err := readAnchor(finalCtx, finalEntityStore, entity.Meta.ID)
	if err != nil {
		return "", fmt.Errorf("failed to re-read fired entity: %w", err)
	}
	if !anchor.found {
		// Deleted in this transaction: the transition did not take effect.
		// The run removes its life and commits the delete, re-creates
		// nothing, re-arms nothing and records no FIRE event.
		if err := removeOwnLife(finalCtx, g); err != nil {
			return "", err
		}
		return OutcomeCancelled, commit(finalTxID)
	}
	writtenHere := anchor.writtenBy(finalTxID)

	// Order at the end (spec §5.2): the run removes its own life first, then
	// the re-arm step runs for the final state. A self-loop therefore re-arms
	// the same id as a new life. A joining read sees the operations staged
	// earlier in the same transaction on every backend (C2): reconcile does
	// not see the life removed here, and RemoveLife does nothing if a joined
	// callback already replaced the task in this transaction. PostgreSQL
	// writes task rows into the open transaction at once; memory and SQLite
	// overlay the staged operations on the committed state.
	if err := removeOwnLife(finalCtx, g); err != nil {
		return "", err
	}
	if err := e.reconcileScheduledTasks(finalCtx, entity, wf, modelScheduled, finalTxID, auditStore); err != nil {
		return "", fmt.Errorf("failed to reconcile scheduled tasks after fire: %w", err)
	}

	// The precondition of the final persist. A version written earlier in this
	// transaction is the one the persist supersedes: the engine's result is
	// the last writer inside the transaction, as for an ordinary transition.
	// Every segmented run takes this branch, because the segment boundary's
	// CompareAndSave writes the entity into the segment it opens. Otherwise
	// the entity is still the committed version read at the start.
	persistAgainst := expectedTxID
	if writtenHere {
		persistAgainst = finalTxID
	}
	if _, err := finalEntityStore.CompareAndSave(finalCtx, entity, persistAgainst); err != nil {
		return "", err
	}

	e.recordEvent(auditStore, finalCtx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventScheduledTransitionFired,
		fmt.Sprintf("Scheduled transition %q fired", cur.Transition), nil)
	return OutcomeFired, commit(finalTxID)
}

// commitRun commits one entity transaction of a scheduled run, shielded: a
// cancellation that arrives while the commit is in flight does not cut it
// (spec §5.3).
func (e *Engine) commitRun(ctx context.Context, txID string) error {
	// Checkpoint before the run's final commit (spec §5.3), on the guard of
	// the transaction, as at every segment commit.
	if err := commitCheckpoint(e.runTxs.forTx(txID), "scheduled run not committed"); err != nil {
		return err
	}
	return common.ShieldedCommitWithBudget(ctx, e.commitBudget, func(commitCtx context.Context) error {
		if err := e.txMgr.Commit(commitCtx, txID); err != nil {
			return fmt.Errorf("failed to commit scheduled run: %w", err)
		}
		return nil
	})
}

// findFireableTransitionInState returns the named transition from wf's given
// state, but only if the scheduler is allowed to fire it: it must carry a
// Schedule and be neither manual nor disabled. Returns nil if wf, the state,
// or the transition is absent, or if a transition of that name exists but is
// not scheduler-fireable.
//
// The eligibility test is armsOnSchedule, the rule reconcileScheduledTasks
// arms by. Arm and fire MUST agree: a name match alone would let the
// scheduler fire a MANUAL transition — running its processors and moving the
// entity with no client asking for it — whenever the definition holding the
// name changed under a live task. Reconcile removes such a task at the
// entity's next write, but a task can fall due before any write reaches it:
// a workflow import that changes which definition the entity's criterion
// selects keeps every task whose transition some definition of the model
// still arms.
func findFireableTransitionInState(wf *spi.WorkflowDefinition, state, transitionName string) *spi.TransitionDefinition {
	if wf == nil {
		return nil
	}
	stateDef, ok := wf.States[state]
	if !ok {
		return nil
	}
	for i := range stateDef.Transitions {
		tr := &stateDef.Transitions[i]
		if tr.Name != transitionName {
			continue
		}
		if !armsOnSchedule(tr) {
			return nil
		}
		return tr
	}
	return nil
}
