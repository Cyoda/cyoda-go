// Package txgate provides per-transaction exclusive gates. Three users take the
// same txID's gate: every joined request, for the whole of its handler; the
// transaction owner's final save, commit and rollback; and the fence's wait,
// which takes the gate once after it has shut a compute member out, so that
// nothing that member had in progress is still running when the work moves on.
// Their access to the shared tx buffer / pgx.Tx is thereby serialised, which is
// the application-side concurrency contract the SPI delegates
// (cyoda-go-spi transaction.go: "the application must serialise its own
// concurrent in-flight ops on the same tx").
package txgate

import (
	"context"
	"errors"
	"sync"
)

// ErrTooManyWaiters is what AcquireCtx returns when the transaction's gate
// already has maxWaiters callers queued behind whoever holds it. The refused
// caller holds nothing and has touched nothing of the transaction.
var ErrTooManyWaiters = errors.New("too many callers are waiting for the transaction's gate")

type gate struct {
	// held is the gate itself: one token, and holding the gate is holding the
	// one slot. A channel rather than a sync.Mutex, so that a caller that has
	// not taken the gate yet can give the wait up when its own context ends —
	// it has touched nothing of the transaction, so there is nothing to undo.
	held chan struct{}
	refs int
	// waiting counts the callers queued for the slot — not the one holding it,
	// which drops out of the count the moment it wins the slot. It is what the
	// cap is applied to, so a cap of n admits the holder plus n queued. An
	// uncapped wait (Acquire) is counted like any other: it cannot be refused
	// itself, but it is one of the callers a capped one would queue behind, so
	// a cap read while the owner waits is a shade conservative rather than
	// wrong.
	waiting int
}

// Registry hands out exclusive per-txID gates, cleaning up entries once no
// goroutine holds or waits on them.
type Registry struct {
	mu    sync.Mutex
	gates map[string]*gate
}

func New() *Registry { return &Registry{gates: make(map[string]*gate)} }

// Acquire blocks until the caller holds the exclusive gate for txID, then
// returns a release func. Empty txID returns a no-op release (never gated).
// The wait has no end and is never refused for capacity: the transaction
// owner's own chain, the fence's wait and a suspended callback's resume all
// take the gate this way, and each holds state that neither giving up nor a
// refusal could undo.
func (r *Registry) Acquire(txID string) func() {
	release, _ := r.acquire(context.Background(), txID, 0) // a background, uncapped wait cannot fail
	return release
}

// AcquireCtx is Acquire for a caller that may go away before it has the gate:
// when ctx ends first it returns ctx.Err() and no release func, and the caller
// holds nothing. Once a release func has been returned the gate is held, and
// ctx no longer bears on it — a holder gives the gate up by releasing it, never
// by being cancelled.
//
// maxWaiters bounds the queue: when that many callers are already waiting the
// call returns ErrTooManyWaiters at once rather than joining them, having
// touched nothing. A caller that sits on one transaction can otherwise park
// callbacks on it without bound, each holding its request for the life of the
// callout. Zero or less is no cap.
func (r *Registry) AcquireCtx(ctx context.Context, txID string, maxWaiters int) (func(), error) {
	return r.acquire(ctx, txID, maxWaiters)
}

// AtCapacity reports whether txID's gate already has maxWaiters callers queued.
// It is a reading, not a reservation: a door that asks before it reads its
// request — so that a refused request costs it no buffer — may still be refused
// by AcquireCtx, and may be admitted by it having read full here. AcquireCtx's
// answer is the binding one.
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
	return g != nil && g.waiting >= maxWaiters
}

func (r *Registry) acquire(ctx context.Context, txID string, maxWaiters int) (func(), error) {
	if txID == "" {
		return func() {}, nil
	}
	// A caller that has already gone takes nothing, free gate or not: the
	// select below would otherwise pick either.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g, err := func() (*gate, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		g := r.gates[txID]
		if g == nil {
			g = &gate{held: make(chan struct{}, 1)}
			r.gates[txID] = g
		}
		if maxWaiters > 0 && g.waiting >= maxWaiters {
			// Nothing was added: a gate created a line above has no waiters,
			// so this branch only ever sees one the caller found.
			return nil, ErrTooManyWaiters
		}
		g.refs++
		g.waiting++
		return g, nil
	}()
	if err != nil {
		return nil, err
	}

	// When the gate frees and ctx ends at the same instant, select takes either:
	// a caller that wins the send holds the gate with a context that has just
	// ended, and its request runs. That is deliberate, and not re-checked
	// afterwards — it is the same case as a client that went away one
	// instruction after acquiring, which runs to completion by contract, and no
	// caller can tell the two apart.
	select {
	case g.held <- struct{}{}: // held until the returned release runs
		r.stopWaiting(g)
	case <-ctx.Done():
		r.stopWaiting(g)
		r.unref(txID, g)
		return nil, ctx.Err()
	}

	// The release is taken once: the suspend handle below releases through a
	// pointer the caller also defers, and one of the two runs second.
	var once sync.Once
	return func() {
		once.Do(func() {
			<-g.held
			r.unref(txID, g)
		})
	}, nil
}

// stopWaiting takes the caller out of the queue count, whether it won the slot
// or gave the wait up.
func (r *Registry) stopWaiting(g *gate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g.waiting--
}

// unref drops one reference to txID's gate and forgets the gate once no
// goroutine holds or waits on it.
func (r *Registry) unref(txID string, g *gate) {
	r.mu.Lock()
	defer r.mu.Unlock()
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

// heldKeyT is the (unexported, collision-free) context key under which a joined
// caller records the gate it currently holds so the engine can Suspend it across
// a blocking external dispatch.
type heldKeyT struct{}

var heldKey = heldKeyT{}

// held is the ctx-scoped handle to a gate the current call chain holds. The
// engine releases it across every blocking callout — a SYNC or ASYNC_SAME_TX
// processor, an ASYNC_NEW_TX processor, a FUNCTION criterion, and the
// scheduled-transition arming function — via Suspend, and re-acquires it
// afterward: the one window that touches no local buffer yet can re-enter with
// a descendant callback on the same txID, and the fence's wait takes the same
// gate. The transaction owner's own chain never holds the gate across
// engine.Execute; this extends that rule to the joined-callback path.
//
// The handle is single-goroutine by construction: Suspend/resume and the
// caller's deferred release all run on the synchronous handler→engine→dispatch
// call chain, and the deferred release reads *release with no lock, so the
// handle is not shareable across goroutines regardless of the mutex. The mutex
// only orders the explicit + deferred resume against a redundant Suspend so a
// double call cannot double-release the gate.
type held struct {
	reg     *Registry
	txID    string
	release *func() // points at the caller's live release variable
	mu      sync.Mutex
	active  bool // true while the gate is currently held via *release
}

// WithHeld records, on the returned ctx, that the caller holds reg's gate for
// txID via the release func pointed to by release. release MUST point at the
// caller's own release variable: a re-acquire (Suspend's resume) mints a fresh
// release func and stores it through the pointer, so the caller's deferred
// release frees the re-acquired gate rather than double-freeing the old one.
func WithHeld(ctx context.Context, reg *Registry, txID string, release *func()) (context.Context, *held) {
	h := &held{reg: reg, txID: txID, release: release, active: true}
	return context.WithValue(ctx, heldKey, h), h
}

// Suspend releases the gate the ctx's call chain currently holds (installed via
// WithHeld) and returns a resume func that re-acquires it. If ctx carries no
// held gate — the transaction owner (which never installs one), or a plain
// non-joined call — Suspend and its resume are both no-ops. Callers MUST invoke
// resume before touching the shared tx buffer again; a deferred resume also
// makes the re-acquire panic-safe.
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
		// Already suspended (no nested Suspend is expected on the engine's
		// sequential dispatch path; this guard keeps a double call harmless
		// rather than double-releasing the gate).
		return func() {}
	}
	(*h.release)()
	h.active = false

	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			*h.release = h.reg.Acquire(h.txID) // blocks until a descendant frees the gate
			h.active = true
		})
	}
}
