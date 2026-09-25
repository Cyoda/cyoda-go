package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// RunGuard travels on the context of one scheduled run (spec §5.3). The
// scheduler builds it from the task it claimed and attaches it with
// WithRunGuard before it calls FireScheduledTransition. The engine reads it
// wherever a run differs from a client request: the re-read at the start of
// each segment and the task-row writes. Every commit of a run, each segment
// commit and the final commit (commitRun), finds it by the id of the
// transaction it commits, through Engine.runTxs, not from the context; so does
// the first read of each segment after the first.
//
// Ref names this claim of this life. Store is the scheduled-task store the
// guarded calls use; the context each call is given decides whether it joins
// the open transaction. Done is the run's cancellation. It is closed on
// self-cancel, on the panic latch and at shutdown step 3.
type RunGuard struct {
	Ref   spi.TaskRef
	Store spi.ScheduledTaskStore
	Done  <-chan struct{}
	// NoNewUnsafe is closed at shutdown step 1: from then on the run starts
	// no new unsafe dispatch, and reaching one counts as cut (spec §6.4).
	// nil means never.
	NoNewUnsafe <-chan struct{}
	// Unsafe records each unsafe dispatch in flight; the scheduler reads it
	// at shutdown steps 3 and 4. nil is allowed and records nothing.
	Unsafe *UnsafeFlight

	// Run state, written only by the run's own goroutine, except the two
	// segment-commit fields below.
	markHeld      bool                           // a MarkUnsafe was accepted
	markErrored   bool                           // a MarkUnsafe failed with a non-refusal error
	unsafeReached bool                           // unsafe work reached a compute node (spec §5.5)
	failReason    spi.ScheduledTaskFailureReason // the run decided FAILED itself
	// firedTransitionDone is set once the fired transition has changed the
	// state. Every segment committed after it sets PartialCommit (spec §5.4),
	// also a cascade that loops back into the source state.
	//
	// It and partialCommitted are read and written by every segment commit
	// the registry maps to this run, on whichever goroutine reaches it: they
	// are atomic, so the stamp does not depend on the COMMIT_BEFORE_DISPATCH
	// refusal of a joined chain (spec §5.2).
	firedTransitionDone atomic.Bool
	partialCommitted    atomic.Bool // a segment stamped with partial committed
	txIDs               []string    // registered in Engine.runTxs; guarded by runTxGuards.mu
}

// UnsafeFlight counts a run's unsafe dispatches in flight and remembers when
// the oldest one started. It is safe for concurrent use: the run writes it,
// the scheduler reads it. All methods are safe on a nil receiver.
type UnsafeFlight struct {
	mu    sync.Mutex
	n     int
	since time.Time
}

// Begin counts one more unsafe dispatch in flight.
func (f *UnsafeFlight) Begin() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == 0 {
		f.since = time.Now()
	}
	f.n++
}

// End counts one unsafe dispatch less. It never takes the count below zero.
func (f *UnsafeFlight) End() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n > 0 {
		f.n--
	}
}

// Since returns the start of the oldest unsafe dispatch in flight, and false
// when none is.
func (f *UnsafeFlight) Since() (time.Time, bool) {
	if f == nil {
		return time.Time{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.since, f.n > 0
}

// UnsafeInFlight reports whether an unsafe processor dispatch of this run is
// in progress. The scheduler exempts such a run at shutdown step 3 (spec
// §6.4). A dispatch counts from before its mark until its step's error is
// known. It is counted before the NoNewUnsafe check: a scheduler that closes
// NoNewUnsafe and then reads the count cannot miss a dispatch that goes
// ahead. A dispatch refused by the signal raises the count only until the
// refusal returns.
func (g *RunGuard) UnsafeInFlight() bool {
	_, ok := g.Unsafe.Since()
	return ok
}

// noNewUnsafe reports whether NoNewUnsafe is closed. A nil channel is never
// ready, so a nil NoNewUnsafe never stops a dispatch.
func (g *RunGuard) noNewUnsafe() bool {
	select {
	case <-g.NoNewUnsafe:
		return true
	default:
		return false
	}
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

// rereadTask is the first read of every segment of a run (spec §5.2). Every
// backend fixes the segment's snapshot at Begin, so a later change to the
// task row by another transaction makes this segment's own task-row write
// conflict (C1).
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

// cancelled reports whether Done is closed. A nil channel is never ready, so
// a nil Done never cancels.
func (g *RunGuard) cancelled() bool {
	select {
	case <-g.Done:
		return true
	default:
		return false
	}
}

// runCheckpoint refuses to go on once the run is cancelled, at a checkpoint
// inside a segment; a commit uses commitCheckpoint. It reads the guard's
// Done, not ctx: after a COMMIT_BEFORE_DISPATCH commit the run continues on
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
	if g.cancelled() {
		// Already cancelled: the callout must see it from its first
		// instruction, not once a watcher goroutine has been scheduled.
		cancel()
		return callCtx, cancel
	}
	go func() {
		select {
		case <-g.Done:
			cancel()
		case <-callCtx.Done():
		}
	}()
	return callCtx, cancel
}

// errUnsafeNotDispatched marks every refusal of beforeDispatch. It is fatal
// in every execution mode, ASYNC_NEW_TX included.
var errUnsafeNotDispatched = errors.New("unsafe processor not dispatched")

func notDispatched(proc spi.ProcessorDefinition, cause error) error {
	return fmt.Errorf("processor %s: %w", proc.Name, errors.Join(errUnsafeNotDispatched, cause))
}

// beforeDispatch applies spec §5.5 before a processor dispatch. Outside a
// run, and for a processor declared idempotent, it does nothing. Otherwise it
// counts the dispatch in flight, refuses it once the run is stopped or
// NoNewUnsafe is closed, and then writes the unsafe mark, even when this run
// already holds one: for the same claim the call is idempotent, and it is the
// check that stops a superseded run from sending more unsafe work.
//
// The caller dispatches only on a nil error. It then defers dispatched,
// right after the refusal check, with the error its whole step returns: a
// failure after the dispatch returned (applying its result, a savepoint, a
// later segment's begin or re-read) is passed too, and a deferred call still
// runs when the dispatch panics. dispatched ends the in-flight count and
// applies the reset rule of "unsafe work reached a compute node".
//
// Every refusal ends the run, so no later call sees the state a refusal
// leaves on g.
func beforeDispatch(ctx context.Context, proc spi.ProcessorDefinition) (dispatched func(stepErr error), err error) {
	g := RunGuardFrom(ctx)
	if g == nil || proc.Config.Idempotent {
		return func(error) {}, nil
	}
	// Counted before the NoNewUnsafe check: a scheduler that closes
	// NoNewUnsafe and then reads the count cannot miss a dispatch that goes
	// ahead. A dispatch refused by the signal raises the count only until the
	// refusal returns. Every exit before the hand-over to dispatched, a
	// refusal or a panic in MarkUnsafe, ends it again.
	g.Unsafe.Begin()
	handedOver := false
	defer func() {
		if !handedOver {
			g.Unsafe.End()
		}
	}()
	// A run cut here has handed nothing off: no mark, so the next claim may
	// run it again.
	if g.noNewUnsafe() {
		return nil, notDispatched(proc, runCancelled("unsafe processor not started after the shutdown signal"))
	}
	if g.cancelled() {
		return nil, notDispatched(proc, runCancelled("unsafe processor not marked"))
	}
	if err := g.Store.MarkUnsafe(ctx, g.Ref); err != nil {
		switch {
		case errors.Is(err, spi.ErrStaleClaim):
			// MarkUnsafe never joins: the refusal is the committed state.
			return nil, notDispatched(proc, errors.Join(errRunSuperseded, err))
		case errors.Is(err, spi.ErrMarkedByAnotherClaim):
			// An earlier claim of this life may have handed the work off.
			g.failReason = spi.FailureUnsafeWorkNotCompleted
			return nil, notDispatched(proc, err)
		case errors.Is(err, spi.ErrTaskBusy):
			// No write was made: a safe failure.
			return nil, notDispatched(proc, err)
		default:
			// The mark may have landed with its reply lost: the scheduler
			// clears this claim's mark when it records the attempt.
			g.markErrored = true
			return nil, notDispatched(proc, fmt.Errorf("failed to mark the scheduled task: %w", err))
		}
	}
	g.markHeld = true
	// "Unsafe work reached a compute node" (spec §5.5): set before every
	// unsafe dispatch, reset only by the NotHandedOff proof on the error the
	// step ends with, and only if it was false before this dispatch. A
	// success, a panic (which reaches dispatched as a nil error) and every
	// error without the proof leave it set.
	reachedBefore := g.unsafeReached
	g.unsafeReached = true
	handedOver = true
	return func(stepErr error) {
		g.Unsafe.End()
		if !reachedBefore && contract.ProvesNoHandOff(stepErr) {
			g.unsafeReached = false
		}
	}, nil
}

// runTxGuards maps a transaction id to the guard of the scheduled run that
// began it. Every commit of a run, each segment commit and the final commit
// (commitRun), reads it by the id of the transaction it commits, so every
// commit of a run's transaction checks the run's cancellation, and every
// segment commit is stamped, whichever call chain reaches it (spec §5.2). The zero
// value is ready for use.
type runTxGuards struct {
	mu sync.Mutex
	m  map[string]*RunGuard
}

// register records g as the guard of txID.
func (r *runTxGuards) register(txID string, g *RunGuard) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = make(map[string]*RunGuard)
	}
	r.m[txID] = g
	g.txIDs = append(g.txIDs, txID)
}

// forTx returns the guard registered for txID, or nil outside a scheduled
// run.
func (r *runTxGuards) forTx(txID string) *RunGuard {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[txID]
}

// release drops every transaction id registered for g. The run calls it on
// every ending, a panic included: a leaked entry would hand a stale guard to
// a later transaction that reuses the id.
func (r *runTxGuards) release(g *RunGuard) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range g.txIDs {
		delete(r.m, id)
	}
}

// commitCheckpoint refuses a commit of a cancelled run (spec §5.3). g is the
// guard the registry maps the committed transaction to: every commit of a run
// finds its guard by transaction, never from the context. A nil g is a commit
// outside a scheduled run.
func commitCheckpoint(g *RunGuard, where string) error {
	if g != nil && g.cancelled() {
		return runCancelled(where)
	}
	return nil
}
