// Package txcoord coordinates the work done in one transaction: the owner's
// callouts, the callbacks that join the transaction under a callout's pass, and
// the ASYNC_NEW_TX processors that run inside it.
//
// Every joined request runs in a scope, and a scope is all-or-nothing: either
// all its writes become part of the scope above it, or none do. The scopes form
// a tree. The root is the owner's own work and ends with the transaction's
// commit or rollback. A callback scope is one joined request; its parent is the
// scope that began the callout its pass names. A processor scope is one
// ASYNC_NEW_TX processor, a child of the scope on the chain that runs it.
//
// A scope that cannot complete is marked (rollback-only). A failed write marks
// the parent of its scope; a processor scope stops the mark from climbing
// further. The transaction is rollback-only when its root is marked. Seal is
// the owner's cut-off before commit or rollback: it waits for the callback
// scopes in progress, refuses every later one, and reports a lost write race, a
// broken transaction or a marked root.
//
// The in-process implementation serves the backends whose transaction lives in
// one pnode's memory or connection (memory, SQLite, PostgreSQL). It gives each
// scope a savepoint, serialises the scopes of a transaction through a gate with
// selective admission, and keeps a scope's writes, with its descendants',
// contiguous (the stack rule): while a scope waits in a callout of its own, only
// its descendants are admitted.
//
// The durable implementation (NewDurable) serves a backend that coordinates
// transactions itself (spi.TransactionCoordinationCapable): the backend keeps
// the scopes, marks and fencing numbers, so any pnode serves any
// transaction's callbacks, and they may run concurrently. It has no gate and
// no stack rule, and nothing waits: a scope still in progress when its callout
// moves on is refused completion by the backend. Select picks one of the two
// at start-up.
package txcoord

import (
	"context"
	"errors"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
)

// RequestClass is what a joined request does to the transaction, decided by the
// door before the handler runs.
type RequestClass uint8

const (
	// Read reads in the transaction.
	Read RequestClass = iota + 1
	// Write writes in the transaction.
	Write
	// Outside writes outside the transaction (messages, async search jobs, the
	// key-value-backed administration routes). Its scope takes no savepoint,
	// its answer is never rewritten, and it never marks.
	Outside
)

// Coordination is the coordination of every transaction of one pnode. The
// waits, the gate and the queue below are the in-process implementation's
// (New). The durable one (NewDurable) has none of them: nothing waits, nothing
// queues, and a scope still in progress when its callout moves on or the
// transaction is sealed is refused completion instead.
type Coordination interface {
	// BeginCallout registers callout calloutID on txID under the scope on ctx
	// (the root of txID when ctx carries none, or one of another transaction)
	// and the pairs that scope was admitted under. The returned ctx is
	// cancelled with cause ErrSuperseded when an outer pair stops being
	// current, and with cause common.ErrRollbackOnly when the scope it was
	// begun from is marked (at once when it already is); Check on it then
	// reports the refusal of the scope it was begun from. end unregisters the
	// callout, cancels the callouts begun under it, and waits until no open
	// scope admitted under it remains; it is idempotent. An empty txID is a
	// callout with no transaction: it is registered with the fence only.
	BeginCallout(ctx context.Context, txID, calloutID string) (context.Context, func())
	// Advance raises the callout's major, cancels the callouts begun under its
	// earlier passes, then waits until no open scope admitted under it at a
	// lower major remains. A major not higher than the current one, or a
	// callout that has ended, changes nothing and waits for nothing it has not
	// already waited for.
	Advance(calloutID string, major uint32)
	// Seal waits until no callback scope of txID is open, then cuts the
	// transaction off: Enter refuses it from then on. err is, in order:
	// spi.ErrTxAborted (wrapped) when the root is marked or the transaction is
	// broken and LostRace says the transaction lost; the broken cause; or
	// common.TransactionRollbackOnly(code) when the root is marked. finish
	// forgets the transaction; the caller defers it after its commit or
	// rollback, also when err != nil. Seal of a transaction never seen returns
	// (finish, nil). The wait does not end with ctx: a callback scope still in
	// progress uses the transaction, so the owner may not commit or roll back
	// under it. ctx is the owner's, for LostRace.
	Seal(ctx context.Context, txID string) (finish func(), err error)
	// Enter opens a callback scope for a joined request: it admits pairs (own
	// pair first, then every enclosing one), takes the transaction's gate,
	// refuses the request when the transaction is broken (404), sealed (410),
	// or under a marked scope (409), and takes a savepoint (Outside: none).
	// Refusals are *common.AppError and leave nothing held. ctx is the joined
	// context; the returned one is detached from its cancellation and carries
	// the scope. The caller ends the scope with End.
	Enter(ctx context.Context, txID string, pairs []fence.Pair, class RequestClass, maxWaiters int) (Scope, context.Context, error)
	// OpenChild opens an ASYNC_NEW_TX processor scope in ctx's transaction,
	// under the scope on ctx (the root when ctx carries none, or one of
	// another transaction), with a savepoint. It refuses as the parent
	// scope's check would. A broken transaction, and a savepoint that cannot
	// be taken, are ErrScopeInfra (with the broken cause or the savepoint's
	// error); a session that has ended leaves the transaction broken. The
	// caller ends the scope with Close.
	OpenChild(ctx context.Context) (Scope, context.Context, error)
	// AtCapacity is the reading Joiner.CheckRoom takes before a door reads its
	// body: whether maxWaiters requests already queue for txID's gate.
	AtCapacity(txID string, maxWaiters int) bool
}

// Scope is one open scope.
type Scope interface {
	// End ends a callback scope by the table of how a scope ends: it returns
	// the rewritten answer, or nil to send the handler's own. It releases the
	// scope's savepoint or rolls back to it, then the gate. A scope that
	// cannot be discarded is answered 500 SERVER_ERROR, not retryable,
	// except in row 3, which answers 404. The in-process coordination then
	// records the transaction as broken. The durable one records nothing:
	// the scope is never completed, so the commit does not keep it, and the
	// backend must fail Seal when the discard carried a mark it could not
	// record. o is what the request was answered with; nil, or a status
	// below 400 without a panic, is a success. Call it exactly once, after
	// the handler returned.
	End(o *common.Outcome) *common.AppError
	// Close ends a processor scope. procErr nil and the scope current and
	// unmarked: kept, returns nil. Otherwise discarded; returns procErr, or
	// the scope's own refusal (410, or after LostRace the lost race or 409
	// TRANSACTION_ROLLBACK_ONLY), or an error carrying ErrScopeInfra when the
	// savepoint could not be released or undone. A mark on the scope goes
	// with it: Close never marks the parent.
	Close(procErr error) error
}

// ErrScopeInfra marks a scope whose savepoint could not be taken, released or
// undone, or, on a backend that coordinates transactions itself, a scope or
// callout the backend could not be asked about: the transaction is unusable,
// which is not the processor's fault.
var ErrScopeInfra = errors.New("transaction scope could not be ended")

// scopeKey is the context key of the scope a chain runs in: an in-process
// *scope, or a durableNode.
type scopeKey struct{}

// scopeFrom returns the in-process scope ctx carries, nil when it carries
// none.
func scopeFrom(ctx context.Context) *scope {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	return s
}

// Pause marks the scope on ctx paused, for the length of a callout of its own,
// and releases the gate its chain holds, so that the callout's callbacks (its
// descendants, and only those) can be admitted. resume retakes the gate
// (uncapped, never refused) and returns Check(ctx); it is idempotent. Callers
// defer resume and call it explicitly before they touch the transaction again.
//
// Pause is a no-op with no scope on ctx and on a chain that holds no gate (the
// owner's); its resume then only returns Check(ctx).
//
// A chain has at most one paused scope at a time. The engine makes its
// callouts one after another on the goroutine that serves the request (a
// transition's processors run in order, and criteria and schedule functions
// are evaluated inline), and that goroutine is blocked in the callout for as
// long as its scope is paused. A Pause on a chain whose gate a Pause has
// already released, before that one's resume, is therefore a caller's
// mistake; it is a silent no-op that releases nothing.
//
// On a backend that coordinates transactions itself there is no gate: Pause
// lets nothing go, and resume is Check(ctx).
func Pause(ctx context.Context) (resume func() error) {
	switch s := ctx.Value(scopeKey{}).(type) {
	case *scope:
		return s.c.pause(ctx, s)
	case durableNode:
		return func() error { return s.check() }
	}
	return func() error { return nil }
}

// Check reports, for the scope on ctx and its ancestors: 410 CALLOUT_SUPERSEDED
// when a pair is not current; else, when a scope on the chain is marked, the
// lost race (spi.ErrTxAborted) if LostRace says so, otherwise
// common.TransactionRollbackOnly(code). On the context BeginCallout returned,
// it reports on the scope the callout was begun from, the root included. nil
// for a ctx with no scope.
//
// On a backend that coordinates transactions itself, it asks the backend
// (CheckScope); a refusal the backend does not name carries ErrScopeInfra.
func Check(ctx context.Context) error {
	switch n := ctx.Value(scopeKey{}).(type) {
	case *scope:
		return n.check(ctx)
	case durableNode:
		return n.check()
	}
	return nil
}

// Pair names one callout at one fencing number, as a pass carries it.
type Pair = fence.Pair

// ErrSuperseded is the cause of every 410 CALLOUT_SUPERSEDED refusal, and the
// cause with which a callout's context is cancelled when an enclosing pass
// stops being current.
var ErrSuperseded = fence.ErrSuperseded

// NewSupersededError returns the 410 CALLOUT_SUPERSEDED refusal, not
// retryable, with ErrSuperseded as its cause.
func NewSupersededError() *common.AppError { return fence.NewSupersededError() }

// Pairs returns the pairs the callback scope on ctx (or the nearest one above a
// processor scope on ctx) was admitted under: the outer of a callout begun from
// it. Nil otherwise.
func Pairs(ctx context.Context) []fence.Pair {
	if n, ok := ctx.Value(scopeKey{}).(durableNode); ok {
		return n.pairs()
	}
	for s := scopeFrom(ctx); s != nil; s = s.parent {
		if s.kind == callbackKind {
			return append([]fence.Pair(nil), s.pairs...)
		}
	}
	return nil
}
