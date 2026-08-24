package scheduler

import (
	"context"
	"log/slog"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// Engine is the minimal seam LocalExecutor needs to fire a ScheduledTask.
// The outcome is widened to a plain string (rather than
// workflow.ScheduledOutcome) so this package stays decoupled from
// internal/domain/workflow, matching the decoupling Task D1/D2 already
// established via contract.NodeRegistry and spi.ScheduledTask. The real
// *workflow.Engine satisfies this through a thin adapter in
// internal/cluster (which already depends on the workflow package).
type Engine interface {
	FireScheduledTransition(ctx context.Context, task spi.ScheduledTask) (string, error)
}

// LocalExecutor fires a ScheduledTask on THIS node by calling Engine under
// a freshly synthesised system identity. It satisfies Executor directly —
// useful as-is for a single-node/cluster-disabled deployment — and is also
// the building block ClusterExecutor (internal/cluster) wraps for its
// target-is-self branch, so "build the system context, call the engine,
// log the outcome without leaking the task payload" exists in exactly one
// place regardless of how the fire was dispatched.
type LocalExecutor struct {
	engine Engine
}

// NewLocalExecutor constructs a LocalExecutor backed by engine.
func NewLocalExecutor(engine Engine) *LocalExecutor {
	return &LocalExecutor{engine: engine}
}

// Execute fires task locally. ctx and target are accepted to satisfy the
// Executor interface: target is not consulted (routing by target is
// ClusterExecutor's job — LocalExecutor always fires), and ctx is not used
// to build the engine call's context — see common.SystemUserContext's doc comment
// on why the system identity always derives from context.Background()
// rather than any caller-supplied ctx.
func (l *LocalExecutor) Execute(_ context.Context, task spi.ScheduledTask, _ string) {
	sysCtx := common.SystemUserContext(task.TenantID)
	outcome, err := l.engine.FireScheduledTransition(sysCtx, task)
	if err != nil {
		// ERROR, not WARN: a fire failure here most commonly means the
		// cascade re-armed into a downstream state whose schedule.function
		// compute node is unavailable, so the fire transaction rolled back.
		// The task is left in place — Execute has no store handle to delete
		// it with — and the scan loop's existing redispatch backoff
		// (internal/scheduler/service.go) still throttles the retry. Left
		// at WARN, a broken downstream function silently blocks an
		// unrelated scheduled transition and retries every scan with no
		// operator-visible signal; ERROR makes that observable.
		slog.Error("scheduled task local fire failed",
			"pkg", "scheduler",
			"taskId", task.ID,
			"entityId", task.EntityID,
			"transition", task.Transition,
			"sourceState", task.SourceState,
			"err", err)
		return
	}
	slog.Debug("scheduled task local fire resolved",
		"pkg", "scheduler", "taskId", task.ID, "outcome", outcome)
}
