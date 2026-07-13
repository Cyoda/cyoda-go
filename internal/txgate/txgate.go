// Package txgate provides per-transaction exclusive gates. A joined callback
// and the transaction owner's commit both Acquire the same txID's gate so their
// access to the shared tx buffer / pgx.Tx is serialised. This is the
// application-side concurrency contract the SPI delegates
// (cyoda-go-spi transaction.go: "the application must serialise its own
// concurrent in-flight ops on the same tx").
package txgate

import (
	"context"
	"sync"
)

type gate struct {
	mu   sync.Mutex
	refs int
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
func (r *Registry) Acquire(txID string) func() {
	if txID == "" {
		return func() {}
	}
	r.mu.Lock()
	g := r.gates[txID]
	if g == nil {
		g = &gate{}
		r.gates[txID] = g
	}
	g.refs++
	r.mu.Unlock()

	g.mu.Lock()

	return func() {
		g.mu.Unlock()
		r.mu.Lock()
		g.refs--
		if g.refs == 0 {
			delete(r.gates, txID)
		}
		r.mu.Unlock()
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
// engine releases it across a blocking callout (SYNC processor / FUNCTION
// criterion dispatch) via Suspend and re-acquires it afterward — the one window
// that touches no local buffer yet can re-enter with a descendant callback on
// the same txID. This generalises the owner's H3 invariant ("never hold the
// gate across engine.Execute") to the joined-callback path.
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
