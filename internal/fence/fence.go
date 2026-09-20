// Package fence decides, on the pnode that holds a transaction, whether a
// callback still comes from the compute node that currently holds a callout's
// work. Each callout has a fencing number (major, minor) that only rises; a
// pass carries the number it was minted under; a callback is admitted only
// under the current number, checked again whenever its chain takes the
// transaction's write lock, and whoever takes the work over waits until a write
// in progress has finished. The fence never cancels a callback's context and
// never interrupts a database statement: it works by checks and by the wait.
//
// The package is a leaf: it imports only internal/txgate and internal/common,
// so every place that enforces the fence (the join, the entity service, the
// workflow engine) and every place that drives it (the owner's loop) can
// import it.
package fence

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// ErrSuperseded is the cause carried by every refusal, and the cause with which
// the context returned by Begin is cancelled when an enclosing pair stops being
// current. errors.Is(err, ErrSuperseded) identifies a refusal through any
// number of %w wraps; context.Cause(ctx) tells the fence's cancellation from a
// caller that went away.
var ErrSuperseded = errors.New("callout superseded")

const supersededMessage = "this compute node was replaced, or its callout has ended"

// NewSupersededError returns the client-facing refusal: 410 CALLOUT_SUPERSEDED,
// not retryable, with ErrSuperseded as its cause. A fresh value each time —
// *common.AppError is mutable and must not be shared.
func NewSupersededError() *common.AppError {
	return common.Operational(http.StatusGone, common.ErrCodeCalloutSuperseded, supersededMessage).WithCause(ErrSuperseded)
}

// Pair names one callout at one fencing number. The JSON names are the wire
// form inside a pass and inside a hand-over request.
type Pair struct {
	Callout string `json:"c"`
	Major   uint32 `json:"j"`
	Minor   uint32 `json:"i,omitempty"`
}

// Fence is the arbiter of one pnode. One per process, shared by the owner's
// loop (Begin, Advance) and by both callback doors (Admit).
type Fence struct {
	gate *txgate.Registry

	mu       sync.Mutex
	callouts map[string]*callout
}

// callout is the registration of one callout in progress.
type callout struct {
	txID  string
	major uint32 // 0 until the first Advance: no pass is current yet
	minor uint32 // highest minor seen under major
	// inner holds the callouts begun from inside a callback of this one, each
	// with the pair of this callout it was begun under.
	inner map[*inner]Pair
}

// inner is the handle by which an enclosing callout releases a Coordinator
// that was begun under one of its pairs.
type inner struct {
	cancel context.CancelCauseFunc
}

// New returns a Fence whose wait takes the write locks of gate.
func New(gate *txgate.Registry) *Fence {
	return &Fence{gate: gate, callouts: make(map[string]*callout)}
}

// Begin registers a callout on transaction txID, under the enclosing callouts
// named by outer, and returns the context the callout runs under and the func
// that ends it. The context is cancelled, with ErrSuperseded as its cause, when
// one of the outer pairs stops being current — at once if one already is not.
// The caller defers end, so a callout is ended on every exit path, a panic
// included. end is idempotent.
//
// No pass is current until the first Advance. An empty txID is a callout with
// no transaction: it is registered so that it is released with an enclosing
// callback, and its wait is a no-op.
func (f *Fence) Begin(ctx context.Context, calloutID, txID string, outer []Pair) (context.Context, func()) {
	cctx, cancel := context.WithCancelCause(ctx)
	in := &inner{cancel: cancel}
	c := &callout{txID: txID, inner: make(map[*inner]Pair)}
	outer = append([]Pair(nil), outer...)

	stale := func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.callouts[calloutID] = c
		for _, p := range outer {
			if !f.currentLocked(p) {
				return true
			}
		}
		for _, p := range outer {
			f.callouts[p.Callout].inner[in] = p
		}
		return false
	}()
	if stale {
		cancel(ErrSuperseded)
	}

	var once sync.Once
	return cctx, func() {
		once.Do(func() { f.end(calloutID, c, in, outer) })
	}
}

// end unregisters the callout — all its passes are refused from then on —
// releases the Coordinators begun under it, and waits for a joined write in
// progress on its transaction.
func (f *Fence) end(calloutID string, c *callout, in *inner, outer []Pair) {
	released := func() []*inner {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.callouts[calloutID] == c {
			delete(f.callouts, calloutID)
		}
		for _, p := range outer {
			if oc := f.callouts[p.Callout]; oc != nil {
				delete(oc.inner, in)
			}
		}
		return takeInner(c, func(Pair) bool { return true })
	}()
	for _, r := range released {
		r.cancel(ErrSuperseded)
	}
	in.cancel(context.Canceled)
	f.wait(c.txID)
}

// Advance raises the callout's number to (major, 0), which shuts out every
// pass issued under a lower one, releases the Coordinators begun under those
// passes, and then waits until no joined write is in progress on the
// transaction. It is called before each local try and before each hand-over.
// A major that is not higher than the current one, or a callout that has
// ended, changes nothing.
func (f *Fence) Advance(calloutID string, major uint32) {
	txID, released, raised := func() (string, []*inner, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		c := f.callouts[calloutID]
		if c == nil || major <= c.major {
			return "", nil, false
		}
		c.major, c.minor = major, 0
		return c.txID, takeInner(c, func(Pair) bool { return true }), true
	}()
	if !raised {
		return
	}
	for _, r := range released {
		r.cancel(ErrSuperseded)
	}
	f.wait(txID)
}

// wait is the hand-over of the write lock: a joined write that made its check
// before the number rose still holds the lock, and this blocks until it has
// finished; one that takes the lock afterwards is refused by its check. It runs
// outside f.mu — the fence never waits under its own mutex.
func (f *Fence) wait(txID string) {
	release := f.gate.Acquire(txID)
	release()
}

// Admit is the check on entry. pairs names the pass's own callout first and
// then every enclosing one. Under one lock it verifies that every pair is
// current, and only then absorbs a higher minor — so a pass refused for an
// enclosing callout changes nothing. It returns a context that carries the
// pairs. It cancels no callback's context; absorbing a higher minor does
// release the Coordinators of callouts begun under the lower one.
func (f *Fence) Admit(ctx context.Context, pairs []Pair) (context.Context, error) {
	pairs = append([]Pair(nil), pairs...)
	released, ok := func() ([]*inner, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(pairs) == 0 {
			return nil, false
		}
		for _, p := range pairs {
			if !f.currentLocked(p) {
				return nil, false
			}
		}
		var released []*inner
		for _, p := range pairs {
			c := f.callouts[p.Callout]
			if p.Minor > c.minor {
				c.minor = p.Minor
				released = append(released, takeInner(c, func(under Pair) bool { return under.Minor < p.Minor })...)
			}
		}
		return released, true
	}()
	if !ok {
		slog.Debug("callback refused on entry: its pass is no longer current", "pkg", "fence")
		return ctx, NewSupersededError()
	}
	for _, r := range released {
		r.cancel(ErrSuperseded)
	}
	return context.WithValue(ctx, admittedKey{}, &admitted{f: f, pairs: pairs}), nil
}

type admittedKey struct{}

// admitted is what Admit leaves on the context: the pairs, and the fence that
// judges them. It travels as a context value, so it survives
// context.WithoutCancel.
type admitted struct {
	f     *Fence
	pairs []Pair
}

// Check reports CALLOUT_SUPERSEDED if ctx carries pairs (it was admitted) and
// one of them is no longer current. A context that was never admitted — the
// owner's own chain, an ordinary request — always passes. Check absorbs
// nothing. Callers run it once they hold the transaction's write lock.
func Check(ctx context.Context) error {
	a, _ := ctx.Value(admittedKey{}).(*admitted)
	if a == nil {
		return nil
	}
	current := func() bool {
		a.f.mu.Lock()
		defer a.f.mu.Unlock()
		for _, p := range a.pairs {
			if !a.f.currentLocked(p) {
				return false
			}
		}
		return true
	}()
	if !current {
		slog.Debug("joined chain refused: its pass is no longer current", "pkg", "fence")
		return NewSupersededError()
	}
	return nil
}

// Pairs returns the pairs ctx was admitted under: the outer of a callout begun
// from inside this callback. Nil for a context that was never admitted.
func Pairs(ctx context.Context) []Pair {
	a, _ := ctx.Value(admittedKey{}).(*admitted)
	if a == nil {
		return nil
	}
	return append([]Pair(nil), a.pairs...)
}

// currentLocked reports whether p is current: its callout is registered, its
// major equals the registered one, and its minor is not lower than the highest
// seen under that major. f.mu must be held.
func (f *Fence) currentLocked(p Pair) bool {
	c := f.callouts[p.Callout]
	return c != nil && p.Major != 0 && p.Major == c.major && p.Minor >= c.minor
}

// takeInner removes from c, and returns, every inner handle whose pair
// satisfies stale. The fence's mutex must be held.
func takeInner(c *callout, stale func(under Pair) bool) []*inner {
	var taken []*inner
	for in, under := range c.inner {
		if stale(under) {
			taken = append(taken, in)
			delete(c.inner, in)
		}
	}
	return taken
}
