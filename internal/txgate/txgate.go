// Package txgate provides per-transaction exclusive gates with selective
// admission. Each transaction's gate has one slot and a queue: whoever holds
// the slot is the only caller touching the transaction's shared buffer /
// pgx.Tx, which is the application-side concurrency contract the SPI delegates
// (cyoda-go-spi transaction.go: "the application must serialise its own
// concurrent in-flight ops on the same tx").
//
// A joined request takes the slot through AcquireJoined, which is capped and
// subject to an admission predicate the caller supplies: the predicate is read
// when the caller arrives and again each time Reevaluate runs while it queues,
// and a caller it no longer admits is turned away holding nothing. A paused
// scope resuming takes the slot through AcquireResume, which is neither capped
// nor refused nor cancelled. The slot passes to the queued callers in the order
// they queued.
package txgate

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// ErrTooManyWaiters is what AcquireJoined returns when the transaction's gate
// already has maxWaiters callers queued behind whoever holds it. The refused
// caller holds nothing and has touched nothing of the transaction.
var ErrTooManyWaiters = errors.New("too many callers are waiting for the transaction's gate")

// ErrNotAdmitted is what AcquireJoined returns when its admission predicate
// reads false, on arrival or at a Reevaluate while the caller queues. The
// refused caller holds nothing and has touched nothing of the transaction.
var ErrNotAdmitted = errors.New("the transaction's gate does not admit this caller now")

// waiter is one caller queued for a gate's slot. Its fate is decided under the
// registry's lock by whoever settles it — a release granting it the slot, a
// Reevaluate refusing it, or the caller itself giving the wait up — and done is
// closed once it is settled, so the caller reads err only after done.
type waiter struct {
	// admitted is nil for a caller that is never refused (a resume).
	admitted func() bool
	done     chan struct{}
	granted  bool  // set with done closed: the caller holds the slot
	err      error // set with done closed when the caller was refused
}

type gate struct {
	// held is the slot. While it is set, the holder alone touches the
	// transaction; while it is clear, queue is empty — a release hands the slot
	// straight to the head of the queue rather than freeing it.
	held bool
	// queue is the callers waiting for the slot, oldest first. The holder is not
	// in it: a cap of n admits the holder plus n queued.
	queue []*waiter
	// refs counts the callers holding or queued for the slot; the gate is
	// forgotten when it reaches zero.
	refs int
}

// Registry hands out exclusive per-txID gates, cleaning up entries once no
// goroutine holds or waits on them.
type Registry struct {
	mu    sync.Mutex
	gates map[string]*gate
}

func New() *Registry { return &Registry{gates: make(map[string]*gate)} }

// AcquireJoined takes the slot for a joined request and returns the func that
// releases it. Empty txID returns a no-op release (never gated).
//
// admitted is evaluated under the registry's lock on entry and again each time
// Reevaluate is called while the caller queues; when it reads false the caller
// gets ErrNotAdmitted at once, holding nothing. It must not call back into the
// registry. A nil admitted admits always.
//
// maxWaiters bounds the queue: when that many callers are already queued the
// call returns ErrTooManyWaiters rather than joining them, having touched
// nothing. A caller that sits on one transaction can otherwise park callbacks
// on it without bound, each holding its request for the life of the callout.
// Zero or less is no cap. Admission is read before the cap.
//
// When ctx ends before the slot is granted the call returns ctx.Err(), holding
// nothing. When the grant and the end of ctx coincide either may win; a grant
// the caller does not take up is passed on to the next caller, never lost. Once a release func has been returned the slot is held
// and ctx no longer bears on it — a holder gives the slot up by releasing it,
// never by being cancelled. The release func may be called more than once; the
// calls after the first do nothing.
func (r *Registry) AcquireJoined(ctx context.Context, txID string, maxWaiters int, admitted func() bool) (release func(), err error) {
	if txID == "" {
		return func() {}, nil
	}
	// A caller that has already gone takes nothing, free slot or not.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g, w, err := r.enter(txID, maxWaiters, admitted)
	if err != nil {
		return nil, err
	}
	if w != nil {
		select {
		case <-w.done:
		case <-ctx.Done():
			if err := r.giveUp(txID, g, w, ctx.Err()); err != nil {
				return nil, err
			}
		}
		if w.err != nil {
			return nil, w.err
		}
	}
	return r.releaseFunc(txID, g), nil
}

// AcquireResume takes the slot for a paused scope resuming: uncapped, never
// refused, never cancelled. It waits its turn behind the callers already
// queued. Empty txID returns a no-op release.
func (r *Registry) AcquireResume(txID string) (release func()) {
	if txID == "" {
		return func() {}
	}
	g, w, _ := r.enter(txID, 0, nil) // uncapped and always admitted: cannot fail
	if w != nil {
		<-w.done
	}
	return r.releaseFunc(txID, g)
}

// Reevaluate re-runs admitted for every caller queued on txID; those it now
// refuses return ErrNotAdmitted, holding nothing. The others keep their place.
func (r *Registry) Reevaluate(txID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.gates[txID]
	if g == nil {
		return
	}
	kept := g.queue[:0]
	for _, w := range g.queue {
		if w.admitted == nil || w.admitted() {
			kept = append(kept, w)
			continue
		}
		w.err = ErrNotAdmitted
		close(w.done)
		r.unrefLocked(txID, g)
	}
	clear(g.queue[len(kept):])
	g.queue = kept
}

// AtCapacity reports whether txID's gate already has maxWaiters callers queued.
// It is a reading, not a reservation: a door that asks before it reads its
// request — so that a refused request costs it no buffer — may still be refused
// by AcquireJoined, and may be admitted by it having read full here.
// AcquireJoined's answer is the binding one.
//
// A transaction with no gate reads the same as one nobody is queued for, so the
// reading never says whether a transaction exists.
func (r *Registry) AtCapacity(txID string, maxWaiters int) bool {
	if txID == "" || maxWaiters <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.gates[txID]
	return g != nil && len(g.queue) >= maxWaiters
}

// enter takes the slot when it is free, or queues the caller for it. A nil
// waiter means the slot is already the caller's.
func (r *Registry) enter(txID string, maxWaiters int, admitted func() bool) (*gate, *waiter, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.gates[txID]
	if admitted != nil && !admitted() {
		return nil, nil, ErrNotAdmitted
	}
	if g != nil && maxWaiters > 0 && len(g.queue) >= maxWaiters {
		return nil, nil, ErrTooManyWaiters
	}
	if g == nil {
		g = &gate{}
		r.gates[txID] = g
	}
	g.refs++
	if !g.held {
		g.held = true
		return g, nil, nil
	}
	w := &waiter{admitted: admitted, done: make(chan struct{})}
	g.queue = append(g.queue, w)
	return g, w, nil
}

// giveUp settles a waiter whose context ended. When it was settled first — the
// slot granted or the caller refused — that outcome stands, except that a
// granted slot is passed on rather than kept: the caller has gone. It returns
// the error the caller reports, or nil when w.err already holds it.
func (r *Registry) giveUp(txID string, g *gate, w *waiter, cause error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-w.done:
		if !w.granted {
			return nil // refused before the context ended: w.err stands
		}
		r.passOnLocked(txID, g)
		return cause
	default:
	}
	i := slices.Index(g.queue, w)
	g.queue = slices.Delete(g.queue, i, i+1)
	r.unrefLocked(txID, g)
	return cause
}

// releaseFunc is the holder's release: it runs once, however often it is called
// — the suspend handle and the caller's own defer may both call it.
func (r *Registry) releaseFunc(txID string, g *gate) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.passOnLocked(txID, g)
		})
	}
}

// passOnLocked gives the held slot up: to the head of the queue when anyone
// waits, else it frees it. It drops the giver's reference.
func (r *Registry) passOnLocked(txID string, g *gate) {
	if len(g.queue) > 0 {
		next := g.queue[0]
		g.queue[0] = nil
		g.queue = g.queue[1:]
		next.granted = true
		close(next.done)
	} else {
		g.held = false
	}
	r.unrefLocked(txID, g)
}

// unrefLocked drops one reference to txID's gate and forgets the gate once no
// goroutine holds or waits on it.
func (r *Registry) unrefLocked(txID string, g *gate) {
	g.refs--
	if g.refs == 0 {
		delete(r.gates, txID)
	}
}

func (r *Registry) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.gates)
}

func admitAll() bool { return true }

// Acquire is the pre-coordination entry point kept for its current callers:
// it takes the slot uncapped, never refused and never cancelled.
func (r *Registry) Acquire(txID string) func() {
	return r.AcquireResume(txID)
}

// AcquireCtx is the pre-coordination entry point kept for its current callers:
// AcquireJoined admitting every caller.
func (r *Registry) AcquireCtx(ctx context.Context, txID string, maxWaiters int) (func(), error) {
	return r.AcquireJoined(ctx, txID, maxWaiters, admitAll)
}

// heldKeyT is the (unexported, collision-free) context key under which a joined
// caller records the gate it currently holds so the engine can Suspend it across
// a blocking external dispatch.
type heldKeyT struct{}

var heldKey = heldKeyT{}

// held is the pre-coordination ctx-scoped handle to a gate the current call
// chain holds, kept for its current callers. The engine releases it across
// every blocking callout via Suspend and re-acquires it afterward.
//
// The handle is single-goroutine by construction: Suspend/resume and the
// caller's deferred release all run on the synchronous handler→engine→dispatch
// call chain, and the deferred release reads *release with no lock. The mutex
// only orders the explicit + deferred resume against a redundant Suspend so a
// double call cannot double-release the gate.
type held struct {
	reg     *Registry
	txID    string
	release *func() // points at the caller's live release variable
	mu      sync.Mutex
	active  bool // true while the gate is currently held via *release
}

// WithHeld is the pre-coordination entry point kept for its current callers:
// it records, on the returned ctx, that the caller holds reg's gate for txID
// via the release func pointed to by release. release MUST point at the
// caller's own release variable: Suspend's resume mints a fresh release func
// and stores it through the pointer, so the caller's deferred release frees the
// re-acquired gate.
func WithHeld(ctx context.Context, reg *Registry, txID string, release *func()) (context.Context, *held) {
	h := &held{reg: reg, txID: txID, release: release, active: true}
	return context.WithValue(ctx, heldKey, h), h
}

// Suspend is the pre-coordination entry point kept for its current callers: it
// releases the gate the ctx's call chain holds (installed via WithHeld) and
// returns a resume func that re-acquires it. If ctx carries no held gate,
// Suspend and its resume are both no-ops. Callers MUST invoke resume before
// touching the shared tx buffer again; a deferred resume also makes the
// re-acquire panic-safe.
func Suspend(ctx context.Context) (resume func()) {
	h, _ := ctx.Value(heldKey).(*held)
	if h == nil {
		return func() {}
	}
	return h.suspend()
}

func (h *held) suspend() func() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.active {
		// Already suspended: a double call stays harmless rather than
		// double-releasing the gate.
		return func() {}
	}
	(*h.release)()
	h.active = false

	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			*h.release = h.reg.AcquireResume(h.txID) // blocks until a descendant frees the gate
			h.active = true
		})
	}
}
