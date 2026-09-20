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
}

// New returns a Fence whose wait takes the write locks of gate.
func New(gate *txgate.Registry) *Fence {
	return &Fence{gate: gate, callouts: make(map[string]*callout)}
}

// Begin registers a callout on transaction txID and returns the context the
// callout runs under and the func that ends it. The caller defers end, so a
// callout is ended on every exit path, a panic included. end is idempotent.
//
// No pass is current until the first Advance.
func (f *Fence) Begin(ctx context.Context, calloutID, txID string, outer []Pair) (context.Context, func()) {
	cctx, cancel := context.WithCancelCause(ctx)
	c := &callout{txID: txID}

	func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.callouts[calloutID] = c
	}()

	var once sync.Once
	return cctx, func() {
		once.Do(func() {
			f.end(calloutID, c)
			cancel(context.Canceled)
		})
	}
}

// end unregisters the callout: all its passes are refused from then on.
func (f *Fence) end(calloutID string, c *callout) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.callouts[calloutID] == c {
		delete(f.callouts, calloutID)
	}
}

// Advance raises the callout's number to (major, 0), which shuts out every
// pass issued under a lower one. It is called before each local try and before
// each hand-over. A major that is not higher than the current one, or a
// callout that has ended, changes nothing.
func (f *Fence) Advance(calloutID string, major uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.callouts[calloutID]
	if c == nil || major <= c.major {
		return
	}
	c.major, c.minor = major, 0
}

// Admit is the check on entry. pairs names the pass's own callout first and
// then every enclosing one. Under one lock it verifies that every pair is
// current, and only then absorbs a higher minor — so a pass refused for an
// enclosing callout changes nothing. It returns a context that carries the
// pairs. It cancels no callback's context.
func (f *Fence) Admit(ctx context.Context, pairs []Pair) (context.Context, error) {
	pairs = append([]Pair(nil), pairs...)
	ok := func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(pairs) == 0 {
			return false
		}
		for _, p := range pairs {
			if !f.currentLocked(p) {
				return false
			}
		}
		for _, p := range pairs {
			if c := f.callouts[p.Callout]; p.Minor > c.minor {
				c.minor = p.Minor
			}
		}
		return true
	}()
	if !ok {
		slog.Debug("callback refused on entry: its pass is no longer current", "pkg", "fence")
		return ctx, NewSupersededError()
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
