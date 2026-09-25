package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// scheduledTransitions returns the keep function for DeleteForModel: it
// keeps a task whose (source state, transition) some workflow of the model
// arms by the one arm rule, armsOnSchedule (arm.go), which modelHasSchedule
// uses too. Every workflow counts, active or not — a task a workflow could
// still fire is never removed here; the fire itself cancels one that no
// selected workflow schedules.
func scheduledTransitions(wfs []spi.WorkflowDefinition) func(sourceState, transition string) bool {
	type pair struct{ state, transition string }
	armed := make(map[pair]struct{})
	for _, wf := range wfs {
		for state, sd := range wf.States {
			for i := range sd.Transitions {
				if armsOnSchedule(&sd.Transitions[i]) {
					armed[pair{state, sd.Transitions[i].Name}] = struct{}{}
				}
			}
		}
	}
	return func(sourceState, transition string) bool {
		_, ok := armed[pair{sourceState, transition}]
		return ok
	}
}

// removeUnscheduledTasks removes the model's tasks whose transition no
// workflow the model's entities can be bound to schedules any more: none of
// wfs, the model's saved workflows, and not the engine's default workflow,
// which selection falls back to when no saved workflow matches. It runs after
// the workflows are saved, so a failed removal never loses a timer that is
// still scheduled.
//
// When this request owns its transaction, the removal runs in a transaction
// of its own. One that loses a task-row race with the scheduler runs again,
// at most common.TaskConflictRetries more times; one that still conflicts is
// a retryable 409, and a retried import saves the same workflows and removes
// again. The tasks left behind until then are cancelled by the fire door if
// they fall due first.
//
// When the request joined a transaction (a routed callback), the removal runs
// in that transaction, once, and does not commit: the owner's commit decides,
// together with a workflow save that joined the same transaction. Removing
// them in a transaction of its own could commit the removal while the owner
// rolls the import back, and lose a timer the model still schedules.
func (h *Handler) removeUnscheduledTasks(ctx context.Context, name string, version int, wfs []spi.WorkflowDefinition) *common.AppError {
	all := make([]spi.WorkflowDefinition, 0, len(wfs)+len(h.engine.defaultWorkflows))
	all = append(append(all, wfs...), h.engine.defaultWorkflows...)
	keep := scheduledTransitions(all)

	owned := spi.GetTransaction(ctx) == nil
	err := common.RetryOnTaskConflict(ctx, owned, func() error {
		if !owned {
			return h.deleteUnscheduled(ctx, name, version, keep)
		}
		return h.removeUnscheduledTasksOwned(ctx, name, version, keep)
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, spi.ErrConflict) {
		return common.Operational(http.StatusConflict, common.ErrCodeConflict,
			"workflows saved, but removing the tasks of transitions no longer scheduled conflicted with the scheduler — retry the import").
			AsRetryable().WithCause(err)
	}
	return common.Internal("failed to remove scheduled tasks", err)
}

// removeUnscheduledTasksOwned is one attempt, in a transaction of its own.
func (h *Handler) removeUnscheduledTasksOwned(ctx context.Context, name string, version int, keep func(string, string) bool) error {
	txMgr := h.engine.txMgr
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	done := false
	defer func() {
		if done {
			return
		}
		rbCtx, cancel := common.RollbackContext(txCtx)
		defer cancel()
		if rbErr := txMgr.Rollback(rbCtx, txID); rbErr != nil && !errors.Is(rbErr, spi.ErrTxNotFound) {
			slog.Warn("failed to roll back transaction", "pkg", "workflow", "txID", txID, "err", rbErr)
		}
	}()

	if err := h.deleteUnscheduled(txCtx, name, version, keep); err != nil {
		return err
	}
	// After a commit attempt the transaction is finished, whatever the
	// outcome.
	done = true
	if err := common.ShieldedCommit(txCtx, func(commitCtx context.Context) error {
		return txMgr.Commit(commitCtx, txID)
	}); err != nil {
		return fmt.Errorf("failed to commit task removal: %w", err)
	}
	return nil
}

// deleteUnscheduled removes the tasks keep drops, in the transaction on ctx
// and in that transaction's tenant.
func (h *Handler) deleteUnscheduled(ctx context.Context, name string, version int, keep func(string, string) bool) error {
	tx := spi.GetTransaction(ctx)
	if tx == nil {
		return errors.New("no transaction on the context")
	}
	sts, err := h.factory.ScheduledTaskStore(ctx)
	if err != nil {
		return fmt.Errorf("failed to access scheduled task store: %w", err)
	}
	if err := sts.DeleteForModel(ctx, tx.TenantID, name, version, keep); err != nil {
		return fmt.Errorf("failed to remove scheduled tasks: %w", err)
	}
	return nil
}
