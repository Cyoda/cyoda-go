package workflow

import (
	"context"
	"errors"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// RunGuard travels on the context of one scheduled run (spec §5.3). The
// scheduler builds it from the task it claimed and attaches it with
// WithRunGuard before it calls FireScheduledTransition. The engine reads it
// wherever a run differs from a client request: the re-read at the start of
// each segment and the task-row writes.
//
// Ref names this claim of this life. Store is the scheduled-task store the
// guarded calls use; the context each call is given decides whether it joins
// the open transaction. Done is the run's cancellation. It is closed on
// self-cancel, on the panic latch and at shutdown step 3.
type RunGuard struct {
	Ref   spi.TaskRef
	Store spi.ScheduledTaskStore
	Done  <-chan struct{}
}

type runGuardKey struct{}

// WithRunGuard returns ctx carrying g. Every context the engine derives from
// it carries g too, including the context.WithoutCancel segments after a
// COMMIT_BEFORE_DISPATCH commit: WithoutCancel keeps values.
func WithRunGuard(ctx context.Context, g *RunGuard) context.Context {
	return context.WithValue(ctx, runGuardKey{}, g)
}

// RunGuardFrom returns the run guard ctx carries, or nil outside a scheduled
// run.
func RunGuardFrom(ctx context.Context) *RunGuard {
	g, _ := ctx.Value(runGuardKey{}).(*RunGuard)
	return g
}

// errRunSuperseded ends a run whose task was re-armed, reclaimed or removed.
// FireScheduledTransition reports it as OutcomeSuperseded; it never reaches
// RunReport.Err.
var errRunSuperseded = errors.New("scheduled run superseded")

// holds reports whether cur is still this run's life and claim.
func (g *RunGuard) holds(cur *spi.ScheduledTask) bool {
	return cur.ArmToken == g.Ref.ArmToken && cur.Claim != nil && cur.Claim.Token == g.Ref.ClaimToken
}

// rereadTask is the first read of every segment of a run (spec §5.2). On
// PostgreSQL it also fixes the segment's snapshot before any other statement,
// so a later change to the task row by another transaction makes this
// segment's own task-row write conflict (C1).
func rereadTask(ctx context.Context, g *RunGuard) (*spi.ScheduledTask, error) {
	cur, found, err := g.Store.Get(ctx, g.Ref.TenantID, g.Ref.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to re-read scheduled task: %w", err)
	}
	if !found || !g.holds(cur) {
		return nil, errRunSuperseded
	}
	return cur, nil
}

// rereadSegment is rereadTask for a segment opened after a
// COMMIT_BEFORE_DISPATCH commit. It does nothing outside a scheduled run.
func rereadSegment(ctx context.Context) error {
	g := RunGuardFrom(ctx)
	if g == nil {
		return nil
	}
	_, err := rereadTask(ctx, g)
	return err
}

// removeOwnLife removes this run's task in the transaction on ctx. It does
// nothing if the same transaction already replaced or removed the task.
func removeOwnLife(ctx context.Context, g *RunGuard) error {
	if err := g.Store.RemoveLife(ctx, g.Ref.TenantID, g.Ref.ID, g.Ref.ArmToken); err != nil {
		return fmt.Errorf("failed to remove the scheduled task: %w", err)
	}
	return nil
}

// errRunCancelled marks a run stopped by the scheduler (spec §5.3).
var errRunCancelled = errors.New("scheduled run cancelled")

// runCancelled returns the error of a run stopped at a checkpoint. It
// satisfies errors.Is(err, context.Canceled), which is what the scheduler's
// recorded-error allow-list keys on (spec §5.8).
func runCancelled(where string) error {
	return fmt.Errorf("%s: %w", where, errors.Join(errRunCancelled, context.Canceled))
}

func (g *RunGuard) cancelled() bool {
	if g.Done == nil {
		return false
	}
	select {
	case <-g.Done:
		return true
	default:
		return false
	}
}

// runCheckpoint refuses to go on once the run is cancelled. It reads the
// guard, not ctx: after a COMMIT_BEFORE_DISPATCH commit the run continues on
// context.WithoutCancel segments, which never report a cancellation.
func runCheckpoint(ctx context.Context, where string) error {
	if g := RunGuardFrom(ctx); g != nil && g.cancelled() {
		return runCancelled(where)
	}
	return nil
}

// runCallCtx binds a callout to the run's cancellation, including on the
// WithoutCancel segments after a COMMIT_BEFORE_DISPATCH commit. Outside a run
// it returns ctx unchanged. The caller must call the returned cancel; it may
// call it more than once.
func runCallCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	g := RunGuardFrom(ctx)
	if g == nil || g.Done == nil {
		return ctx, func() {}
	}
	callCtx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-g.Done:
			cancel()
		case <-callCtx.Done():
		}
	}()
	return callCtx, cancel
}
