package scheduler

import (
	"context"
	"log/slog"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// systemSchedulerUserID identifies the scheduler's service principal in
// audit trails and as the UserContext.UserID TransactionManager.Begin
// requires. Never a real end-user identity.
const systemSchedulerUserID = "scheduler"

// SystemUserContext derives, from context.Background(), a context.Context
// carrying a synthesised system UserContext scoped to tenant. It exists
// because TransactionManager.Begin rejects any context whose UserContext
// has no tenant (plugins/memory/txmanager.go Begin), and the scheduler's
// background fire path — the scan loop's own goroutine, and the peer RPC
// handler on the receiving node — has no caller-derived UserContext at
// all: there is no inbound HTTP/gRPC request to inherit one from.
//
// Always builds from context.Background(), not a caller-supplied parent,
// so no request-scoped values (deadlines, trace IDs) a caller didn't
// intend to share leak into the background fire. Exported because both
// LocalExecutor (this package) and the peer RPC client/handler
// (internal/cluster) need the identical identity — the system principal
// firing a task must look the same regardless of which node does it.
func SystemUserContext(tenant spi.TenantID) context.Context {
	uc := &spi.UserContext{
		UserID:   systemSchedulerUserID,
		UserName: systemSchedulerUserID,
		Tenant:   spi.Tenant{ID: tenant},
	}
	return spi.WithUserContext(context.Background(), uc)
}

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
// to build the engine call's context — see SystemUserContext's doc comment
// on why the system identity always derives from context.Background()
// rather than any caller-supplied ctx.
func (l *LocalExecutor) Execute(_ context.Context, task spi.ScheduledTask, _ string) {
	sysCtx := SystemUserContext(task.TenantID)
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
