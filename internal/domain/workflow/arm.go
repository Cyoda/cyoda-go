package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// taskID deterministically derives a ScheduledTask's ID from the tenant,
// entity, source state, and transition it fires. The tenant is included
// (in addition to the plan's original entityID|sourceState|transition
// shape) as defense-in-depth: the stores key rows by this raw string id,
// so folding the tenant in makes a cross-tenant id collision impossible
// even though entity IDs are already expected to be tenant-scoped upstream.
//
// This is the ONLY place a fire-transition task ID is computed. Once armed,
// FireScheduledTransition consumes the ID stored on the ScheduledTask row —
// it never recomputes — so this function staying the single source of truth
// keeps arm and fire consistent.
func taskID(tenantID spi.TenantID, entityID, sourceState, transition string) string {
	h := sha256.Sum256([]byte("fire-transition|" + string(tenantID) + "|" + entityID + "|" + sourceState + "|" + transition))
	return hex.EncodeToString(h[:16])
}

// workflowHasSchedule reports whether any transition in wf carries a
// Schedule. reconcileScheduledTasks uses this to skip all ScheduledTaskStore
// I/O for the (overwhelmingly common) case of a workflow with no scheduled
// transitions at all.
func workflowHasSchedule(wf *spi.WorkflowDefinition) bool {
	for _, st := range wf.States {
		for _, tr := range st.Transitions {
			if tr.Schedule != nil {
				return true
			}
		}
	}
	return false
}

// reconcileScheduledTasks brings the entity's pending ScheduledTask rows in
// line with its current state: it arms every non-manual, non-disabled
// scheduled transition out of the CURRENT state, and cancels (deletes) any
// pending task left over from a state the entity is no longer in. The write
// is atomic with the entity write because it runs against the same ctx/txID
// the caller is already inside — the context-resolving, tx-scoped
// ScheduledTaskStore joins whatever transaction ctx carries.
//
// No-op (zero store I/O) when the workflow has no scheduled transitions
// anywhere, so entities on schedule-free workflows pay no cost.
//
// Arm/cancel audit events are recorded best-effort via recordEvent, matching
// every other audit call site in the engine — a failure to audit must never
// fail the transition that already committed. auditStore is the
// already-resolved store from the caller (Execute/ManualTransition/Loopback
// all hard-fail before reaching here if they can't resolve one), so arm/cancel
// events share the same guaranteed-non-nil store as every other audit event
// in the call — reconcile no longer re-derives its own and silently skips
// the audit block on a transient resolution failure.
//
// suppressCancelAuditFor, when non-empty, names a ScheduledTask ID whose
// SourceState-mismatch is NOT a genuine "left behind" cancel: it is the very
// task FireScheduledTransition just fired, so its SourceState no longer
// matching the post-cascade CurrentState is expected. ReconcileForEntity
// still reports (and its own Delete still removes) that task's row like any
// other cancelled entry — this only suppresses the misleading
// SCHEDULED_TRANSITION_CANCEL audit event that would otherwise sit alongside
// its SCHEDULED_TRANSITION_FIRE event. Every non-fire caller
// (Execute/ManualTransition/Loopback) passes "" — no exclusion.
func (e *Engine) reconcileScheduledTasks(ctx context.Context, entity *spi.Entity, wf *spi.WorkflowDefinition, txID string, auditStore spi.StateMachineAuditStore, suppressCancelAuditFor string) error {
	if !workflowHasSchedule(wf) {
		return nil
	}

	// armMs is the single "now" reference for every schedule computed in
	// this reconcile pass — both the static DelayMs arithmetic and the
	// base a Function's relative fireAfterMs/expireAfterMs resolve against
	// (see resolveSchedule). Computed once so a slow dispatch to one
	// Function callout doesn't skew the "now" seen by a sibling
	// transition's static or relative computation.
	armMs := e.now().UnixMilli()
	state := entity.Meta.State

	var arm []spi.ScheduledTask
	// cancelIDs carries born-expired task IDs into ReconcileRequest.Cancel
	// — deleted in the same transaction as the entity write (Task 6.1's
	// store-side Cancel branch) without ever having been armed.
	var cancelIDs []string
	var bornExpired []expiredSchedule

	if stateDef, ok := wf.States[state]; ok {
		for i := range stateDef.Transitions {
			tr := &stateDef.Transitions[i]
			if tr.Schedule == nil || tr.Manual || tr.Disabled {
				continue
			}
			// ModelRef.ModelVersion is the wire/string form ("1.0"); the
			// ScheduledTask payload stores the numeric form, matching the
			// existing ref.ModelVersion -> int conversion used elsewhere
			// (e.g. internal/domain/model/service.go ListModels).
			modelVersion, _ := strconv.ParseInt(entity.Meta.ModelRef.ModelVersion, 10, 32)
			id := taskID(entity.Meta.TenantID, entity.Meta.ID, state, tr.Name)

			if tr.Schedule.Function != nil {
				task, expired, err := e.armViaFunction(ctx, entity, wf, tr, state, id, armMs, int(modelVersion), txID)
				if err != nil {
					return err
				}
				if expired != nil {
					cancelIDs = append(cancelIDs, id)
					bornExpired = append(bornExpired, *expired)
					continue
				}
				arm = append(arm, *task)
				continue
			}

			var timeoutMs *int64
			if tr.Schedule.TimeoutMs != nil {
				v := *tr.Schedule.TimeoutMs
				timeoutMs = &v
			}
			arm = append(arm, spi.ScheduledTask{
				ID:            id,
				TenantID:      entity.Meta.TenantID,
				Type:          spi.ScheduledTaskFireTransition,
				ScheduledTime: armMs + tr.Schedule.DelayMs,
				TimeoutMs:     timeoutMs,
				EntityID:      entity.Meta.ID,
				ModelName:     entity.Meta.ModelRef.EntityName,
				ModelVersion:  int(modelVersion),
				Transition:    tr.Name,
				SourceState:   state,
				ArmedAt:       armMs,
				ArmedBy:       spi.ResolveOrigin(ctx),
			})
		}
	}

	sts, err := e.factory.ScheduledTaskStore(ctx)
	if err != nil {
		return fmt.Errorf("failed to get scheduled task store: %w", err)
	}

	cancelled, err := sts.ReconcileForEntity(ctx, spi.ReconcileRequest{
		TenantID:     entity.Meta.TenantID,
		EntityID:     entity.Meta.ID,
		CurrentState: state,
		Arm:          arm,
		Cancel:       cancelIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile scheduled tasks: %w", err)
	}

	for _, a := range arm {
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, state,
			spi.SMEventScheduledTransitionArmed,
			fmt.Sprintf("Scheduled transition %q armed for %dms from now", a.Transition, a.ScheduledTime-armMs),
			map[string]any{"transition": a.Transition, "sourceState": state, "scheduledTime": a.ScheduledTime})
	}
	for _, c := range cancelled {
		if suppressCancelAuditFor != "" && c.ID == suppressCancelAuditFor {
			continue
		}
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, c.SourceState,
			spi.SMEventScheduledTransitionCancelled,
			fmt.Sprintf("Scheduled transition %q cancelled (left state %q)", c.Transition, c.SourceState),
			map[string]any{"transition": c.Transition, "sourceState": c.SourceState})
	}
	// Born-expired Function results are cancelled via req.Cancel above but
	// deliberately excluded from ReconcileForEntity's returned cancelled
	// slice (Task 6.1) so they are audited distinctly here as EXPIRE, not
	// CANCEL — a "left the state" cancel and "never fired, expired on
	// arrival" are different operational stories.
	for _, be := range bornExpired {
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, state,
			spi.SMEventScheduledTransitionExpired,
			fmt.Sprintf("Scheduled transition %q born expired (Function result already past its resolved expiry)", be.transition),
			map[string]any{"transition": be.transition, "sourceState": state})
	}

	return nil
}

// expiredSchedule records a born-expired scheduled transition for the
// post-reconcile EXPIRE audit pass. Cancellation of the born-expired task's
// row is driven separately by cancelIDs (built alongside bornExpired in
// reconcileScheduledTasks) — this struct only carries what the EXPIRE audit
// event itself reports.
type expiredSchedule struct {
	transition string
}

// armViaFunction dispatches tr's Schedule.Function callout and resolves its
// result into either a ScheduledTask ready to arm, or a born-expired
// signal. Exactly one of the two return values is non-nil on a nil error.
//
// The per-tx gate is released (txgate.Suspend) across the blocking dispatch
// and re-acquired immediately on return — BEFORE this function's caller
// does any further tx-buffer work — because the callout can re-enter with a
// descendant callback joined on the same txID (same H3 rationale as
// evaluateCriterion's FUNCTION-criterion dispatch and
// executeSyncProcessor). Unlike evaluateCriterion, resume is NOT deferred:
// reconcileScheduledTasks writes the tx buffer (ReconcileForEntity) after
// every transition in the loop has been resolved, so the gate must already
// be held again by the time this call returns, not merely by the time the
// whole loop unwinds.
func (e *Engine) armViaFunction(ctx context.Context, entity *spi.Entity, wf *spi.WorkflowDefinition, tr *spi.TransitionDefinition, state, id string, armMs int64, modelVersion int, txID string) (task *spi.ScheduledTask, expired *expiredSchedule, err error) {
	if e.extProc == nil {
		return nil, nil, fmt.Errorf("no external processing service configured for scheduled-transition Function %q", tr.Schedule.Function.Name)
	}

	resume := txgate.Suspend(ctx)
	res, derr := e.extProc.DispatchFunction(ctx, entity, *tr.Schedule.Function, wf.Name, tr.Name, txID)
	resume() // BEFORE any tx-buffer write — see doc comment above.
	if derr != nil {
		return nil, nil, derr // already a classified AppError (503) — fails the write, fail-closed
	}
	if res.Kind != "Schedule" {
		return nil, nil, invalidScheduleResult("function %q returned resultKind %q, want %q", tr.Schedule.Function.Name, res.Kind, "Schedule")
	}

	sched, timeoutMs, born, rerr := resolveSchedule(res.Value, armMs)
	if rerr != nil {
		return nil, nil, rerr
	}
	if born {
		return nil, &expiredSchedule{transition: tr.Name}, nil
	}

	return &spi.ScheduledTask{
		ID:            id,
		TenantID:      entity.Meta.TenantID,
		Type:          spi.ScheduledTaskFireTransition,
		ScheduledTime: sched,
		TimeoutMs:     timeoutMs,
		EntityID:      entity.Meta.ID,
		ModelName:     entity.Meta.ModelRef.EntityName,
		ModelVersion:  modelVersion,
		Transition:    tr.Name,
		SourceState:   state,
		ArmedAt:       armMs,
		// ArmedBy is resolved from ctx (the chain origin), NOT from the
		// Function's dispatch result — res carries only timing (fireAt /
		// fireAfterMs / expireAfterMs), never a principal. The callout
		// affects WHEN this task fires, never WHO it is attributed to.
		ArmedBy: spi.ResolveOrigin(ctx),
	}, nil, nil
}
