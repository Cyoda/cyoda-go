package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	spi "github.com/cyoda-platform/cyoda-go-spi"
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
// fail the transition that already committed.
func (e *Engine) reconcileScheduledTasks(ctx context.Context, entity *spi.Entity, wf *spi.WorkflowDefinition, txID string) error {
	if !workflowHasSchedule(wf) {
		return nil
	}

	nowMs := e.now().UnixMilli()
	state := entity.Meta.State

	var arm []spi.ScheduledTask
	if stateDef, ok := wf.States[state]; ok {
		for i := range stateDef.Transitions {
			tr := &stateDef.Transitions[i]
			if tr.Schedule == nil || tr.Manual || tr.Disabled {
				continue
			}
			var timeoutMs *int64
			if tr.Schedule.TimeoutMs != nil {
				v := *tr.Schedule.TimeoutMs
				timeoutMs = &v
			}
			// ModelRef.ModelVersion is the wire/string form ("1.0"); the
			// ScheduledTask payload stores the numeric form, matching the
			// existing ref.ModelVersion -> int conversion used elsewhere
			// (e.g. internal/domain/model/service.go ListModels).
			modelVersion, _ := strconv.ParseInt(entity.Meta.ModelRef.ModelVersion, 10, 32)
			arm = append(arm, spi.ScheduledTask{
				ID:            taskID(entity.Meta.TenantID, entity.Meta.ID, state, tr.Name),
				TenantID:      entity.Meta.TenantID,
				Type:          spi.ScheduledTaskFireTransition,
				ScheduledTime: nowMs + tr.Schedule.DelayMs,
				TimeoutMs:     timeoutMs,
				EntityID:      entity.Meta.ID,
				ModelName:     entity.Meta.ModelRef.EntityName,
				ModelVersion:  int(modelVersion),
				Transition:    tr.Name,
				SourceState:   state,
				ArmedAt:       nowMs,
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
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile scheduled tasks: %w", err)
	}

	auditStore, aerr := e.factory.StateMachineAuditStore(ctx)
	if aerr == nil {
		for _, a := range arm {
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, state,
				spi.SMEventScheduledTransitionArmed,
				fmt.Sprintf("Scheduled transition %q armed for %dms from now", a.Transition, a.ScheduledTime-nowMs),
				map[string]any{"transition": a.Transition, "sourceState": state, "scheduledTime": a.ScheduledTime})
		}
		for _, c := range cancelled {
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, c.SourceState,
				spi.SMEventScheduledTransitionCancelled,
				fmt.Sprintf("Scheduled transition %q cancelled (left state %q)", c.Transition, c.SourceState),
				map[string]any{"transition": c.Transition, "sourceState": c.SourceState})
		}
	}

	return nil
}
