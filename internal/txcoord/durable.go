package txcoord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
)

// NewDurable returns the coordination over co, the coordinator of a storage
// backend that offers spi.TransactionCoordinationCapable. txMgr is that
// backend's transaction manager; it is asked only whether a transaction lost a
// write race.
//
// The backend holds the scopes, the marks and the fencing numbers, for every
// pnode at once. So this coordination keeps no gate and no stack rule, and it
// never waits: a callout that advances or ends does not wait for the scopes in
// progress under it, because the backend refuses their completion. Pause
// releases nothing, and its resume is Check. The only state kept here is the
// callouts this pnode runs the loop of, so that Advance can name their
// transaction and check the scope each was begun from.
func NewDurable(co spi.TransactionCoordinator, txMgr spi.TransactionManager) Coordination {
	return &durable{co: co, txMgr: txMgr, callouts: make(map[string]*durableCallout)}
}

type durable struct {
	co    spi.TransactionCoordinator
	txMgr spi.TransactionManager

	mu       sync.Mutex
	callouts map[string]*durableCallout // the callouts this pnode runs
}

// durableNode is what a chain's context carries under scopeKey on the durable
// implementation: a scope, or a callout begun from one.
type durableNode interface {
	// check is Check for the node.
	check() error
	// pairs is Pairs for the node.
	pairs() []fence.Pair
	// scope is the scope the node stands for: itself, or the scope a callout
	// was begun from.
	scope() *durableScope
}

// durableScope is one scope as this pnode sees it. Its state is the
// backend's; ctx carries the backend's scope.
type durableScope struct {
	c      *durable
	kind   scopeKind
	class  RequestClass // callback scopes only
	txID   string
	own    []fence.Pair  // callback scopes only: own pair first
	parent *durableScope // the scope on the context it was opened from, nil for a callback scope or the root
	ctx    context.Context
}

func (s *durableScope) scope() *durableScope { return s }

// pairs returns the pairs of the nearest callback scope on the chain.
func (s *durableScope) pairs() []fence.Pair {
	for n := s; n != nil; n = n.parent {
		if n.kind == callbackKind {
			return append([]fence.Pair(nil), n.own...)
		}
	}
	return nil
}

// check asks the backend for the scope's standing, as Check reports it.
func (s *durableScope) check() error {
	err := s.c.co.CheckScope(s.ctx)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, spi.ErrCalloutSuperseded):
		return supersededBy(err)
	case errors.Is(err, spi.ErrTxRollbackOnly):
		return rollbackOnly(s.ctx, s.c.txMgr, s.txID, failedCodeOf(err))
	}
	return fmt.Errorf("failed to check a scope of the transaction: %w", errors.Join(ErrScopeInfra, err))
}

// failedCodeOf is the code a coordinator's refusal for a mark names: the
// FailedCode of its *spi.RollbackOnlyError, "" when it carries none.
func failedCodeOf(err error) string {
	var refused *spi.RollbackOnlyError
	if errors.As(err, &refused) {
		return refused.FailedCode
	}
	return ""
}

// supersededBy is the 410 for a refusal the backend made with
// spi.ErrCalloutSuperseded; it carries ErrSuperseded.
func supersededBy(err error) *common.AppError {
	return fence.NewSupersededError().WithCause(fmt.Errorf("%w: %w", fence.ErrSuperseded, err))
}

// scopeConflict is a joined request's answer when its scope wrote an entity
// that a scope completed before it also wrote from the same version. The
// request kept nothing and may be sent again.
func scopeConflict(err error) *common.AppError {
	return common.Operational(http.StatusConflict, common.ErrCodeConflict,
		"another request of the transaction changed an entity this request changed, from the same version — retry").
		AsRetryable().WithCause(err)
}

// durableCallout is a callout this pnode runs the loop of.
type durableCallout struct {
	id, txID string
	from     *durableScope // the scope it was begun from, the root included
	cancel   context.CancelCauseFunc

	mu     sync.Mutex
	endErr error // why EndCallout failed; nil while it has not
}

func (r *durableCallout) scope() *durableScope { return r.from }
func (r *durableCallout) pairs() []fence.Pair  { return r.from.pairs() }

// check reports a failed end first: the callout's callbacks may then still
// complete, so the callout cannot be counted as answered.
func (r *durableCallout) check() error {
	if err := func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.endErr
	}(); err != nil {
		return err
	}
	return r.from.check()
}

// stopIfRefused cancels the callout when the scope it was begun from can no
// longer complete, with the cause the owner's loop maps (ended in the callout
// package): ErrSuperseded, common.ErrRollbackOnly, or ErrScopeInfra.
func (r *durableCallout) stopIfRefused() {
	err := r.from.c.co.CheckScope(r.from.ctx)
	switch {
	case err == nil:
	case errors.Is(err, spi.ErrCalloutSuperseded):
		r.cancel(fmt.Errorf("%w: %w", ErrSuperseded, err))
	case errors.Is(err, spi.ErrTxRollbackOnly):
		r.cancel(fmt.Errorf("%w: %w", common.ErrRollbackOnly, err))
	default:
		r.cancel(fmt.Errorf("failed to check a scope of the transaction: %w", errors.Join(ErrScopeInfra, err)))
	}
}

// nodeFrom returns the durable scope ctx carries when it belongs to txID, nil
// otherwise.
func nodeFrom(ctx context.Context, txID string) *durableScope {
	n, _ := ctx.Value(scopeKey{}).(durableNode)
	if n == nil || n.scope().txID != txID {
		return nil
	}
	return n.scope()
}

// BeginCallout implements Coordination. The callout is registered with the
// backend under the scope on ctx (the root when ctx carries none of txID).
// Its context is cancelled when Advance finds that scope refused, and when
// end runs; end does not wait for anything.
func (c *durable) BeginCallout(ctx context.Context, txID, calloutID string) (context.Context, func()) {
	cctx, cancel := context.WithCancelCause(ctx)
	if txID == "" {
		return cctx, func() { cancel(context.Canceled) }
	}
	from := nodeFrom(ctx, txID)
	var outer []spi.CalloutPair
	if from != nil {
		outer = toSPIPairs(from.pairs())
	} else {
		// The owner's own work: the root, which the backend reads from a
		// context that carries the transaction and no scope of it.
		from = &durableScope{c: c, kind: rootKind, txID: txID, ctx: context.WithoutCancel(ctx)}
	}
	reg := &durableCallout{id: calloutID, txID: txID, from: from, cancel: cancel}
	cctx = context.WithValue(cctx, scopeKey{}, reg)
	if err := c.co.BeginCallout(context.WithoutCancel(ctx), txID, calloutID, outer); err != nil {
		if errors.Is(err, spi.ErrCalloutSuperseded) {
			cancel(fmt.Errorf("%w: %w", ErrSuperseded, err))
		} else {
			cancel(fmt.Errorf("failed to begin a callout in the transaction: %w", errors.Join(ErrScopeInfra, err)))
		}
		return cctx, func() {}
	}
	func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.callouts[calloutID] = reg
	}()
	reg.stopIfRefused()
	var once sync.Once
	return cctx, func() { once.Do(func() { c.endCallout(reg) }) }
}

// endCallout ends the callout with the backend and forgets it. A failed end
// is kept for Check on the callout's context.
func (c *durable) endCallout(reg *durableCallout) {
	if err := c.co.EndCallout(context.WithoutCancel(reg.from.ctx), reg.txID, reg.id); err != nil {
		func() {
			reg.mu.Lock()
			defer reg.mu.Unlock()
			reg.endErr = fmt.Errorf("failed to end a callout in the transaction: %w", errors.Join(ErrScopeInfra, err))
		}()
	}
	func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.callouts[reg.id] == reg {
			delete(c.callouts, reg.id)
		}
	}()
	reg.cancel(context.Canceled)
}

// Advance implements Coordination. The owner's loop draws a number before
// each try and each hand-over, so this is where it learns that the scope the
// callout was begun from can no longer complete: nothing cancels the callout
// when that happens on another pnode.
func (c *durable) Advance(calloutID string, major uint32) {
	reg := func() *durableCallout {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.callouts[calloutID]
	}()
	if reg == nil {
		return
	}
	if err := c.co.AdvanceCallout(context.WithoutCancel(reg.from.ctx), reg.txID, calloutID, major); err != nil {
		reg.cancel(fmt.Errorf("failed to advance a callout in the transaction: %w", errors.Join(ErrScopeInfra, err)))
		return
	}
	reg.stopIfRefused()
}

// Seal implements Coordination. finish has nothing to forget. The backend is
// asked on a context the owner's cancellation does not reach: the owner also
// seals on its rollback path, where its context may already be cancelled.
func (c *durable) Seal(ctx context.Context, txID string) (func(), error) {
	finish := func() {}
	err := c.co.Seal(context.WithoutCancel(ctx), txID)
	switch {
	case err == nil:
		return finish, nil
	case errors.Is(err, spi.ErrTxRollbackOnly):
		return finish, sealRefusal(ctx, c.txMgr, txID, nil, true, failedCodeOf(err))
	}
	return finish, sealRefusal(ctx, c.txMgr, txID, fmt.Errorf("failed to seal the transaction: %w", err), false, "")
}

// Enter implements Coordination. maxWaiters does not apply: nothing queues.
func (c *durable) Enter(ctx context.Context, txID string, pairs []fence.Pair, class RequestClass, _ int) (Scope, context.Context, error) {
	// From here the request is detached from its client's cancellation: it
	// ends by finishing or by being refused.
	detached := context.WithoutCancel(ctx)
	sctx, err := c.co.OpenScope(detached, txID, toSPIPairs(pairs))
	if err != nil {
		// The context a refusing coordinator returns may be nil: the refusal
		// is judged on the joined context.
		return nil, ctx, c.openRefusal(detached, txID, err)
	}
	s := &durableScope{c: c, kind: callbackKind, class: class, txID: txID, own: append([]fence.Pair(nil), pairs...)}
	s.ctx = context.WithValue(sctx, scopeKey{}, s)
	return s, s.ctx, nil
}

// openRefusal answers a request whose scope the backend did not open: the
// handler did not run, so nothing is marked.
func (c *durable) openRefusal(ctx context.Context, txID string, err error) *common.AppError {
	switch {
	case errors.Is(err, spi.ErrCalloutSuperseded):
		return supersededBy(err)
	case errors.Is(err, spi.ErrTxRollbackOnly):
		return common.TransactionRollbackOnlyJoined()
	case lostRace(ctx, c.txMgr, txID):
		return lostRaceAnswer()
	case errors.Is(err, spi.ErrTxNotFound):
		return transactionNotFound(err)
	}
	return common.InternalWithCode(common.ErrCodeServerError,
		"the request could not open its scope in the transaction", err)
}

// OpenChild implements Coordination.
func (c *durable) OpenChild(ctx context.Context) (Scope, context.Context, error) {
	spiTx := spi.GetTransaction(ctx)
	if spiTx == nil {
		return nil, ctx, errors.New("failed to open a processor scope: the context carries no transaction")
	}
	parent := nodeFrom(ctx, spiTx.ID)
	if parent != nil && parent.kind != rootKind {
		if err := parent.check(); err != nil {
			return nil, ctx, err
		}
	}
	pctx, err := c.co.OpenChildScope(ctx)
	if err != nil {
		return nil, ctx, fmt.Errorf("failed to open a processor scope: %w", errors.Join(ErrScopeInfra, err))
	}
	if parent != nil && parent.kind == rootKind {
		parent = nil
	}
	s := &durableScope{c: c, kind: processorKind, txID: spiTx.ID, parent: parent}
	s.ctx = context.WithValue(pctx, scopeKey{}, s)
	return s, s.ctx, nil
}

// AtCapacity implements Coordination: nothing queues for a transaction.
func (c *durable) AtCapacity(string, int) bool { return false }

// End implements Scope for a callback scope, by the table of how a scope ends.
// A request that writes outside the transaction completes its scope, which
// holds nothing of the transaction, and is never rewritten.
func (s *durableScope) End(o *common.Outcome) *common.AppError {
	if s.class == Outside {
		if err := s.c.co.CompleteScope(s.ctx); err != nil {
			slog.Warn("the scope of a request that writes outside the transaction could not be completed",
				"pkg", "txcoord", "txId", s.txID, "err", err)
		}
		return nil
	}
	answer, _, failed := decide(s, s.class, failureOf(o))
	return endAnswer(answer, failed)
}

// standing implements ending. Row 1 comes before row 4 in the backend's own
// order (CheckScope). An error the backend does not name is a failed
// completion: the request cannot be judged to have succeeded.
func (s *durableScope) standing() (error, string, bool, error) {
	err := s.c.co.CheckScope(s.ctx)
	switch {
	case err == nil:
		return nil, "", false, nil
	case errors.Is(err, spi.ErrCalloutSuperseded):
		return fmt.Errorf("%w: %w", fence.ErrSuperseded, err), "", false, nil
	case errors.Is(err, spi.ErrTxRollbackOnly):
		return nil, failedCodeOf(err), true, nil
	}
	return nil, "", false, fmt.Errorf("failed to check a scope of the transaction: %w", err)
}

// lostRace implements ending.
func (s *durableScope) lostRace() bool { return lostRace(s.ctx, s.c.txMgr, s.txID) }

// complete implements ending: CompleteScope, whose refusal maps by its error.
// A refused completion has discarded the scope already.
func (s *durableScope) complete(class RequestClass) (*common.AppError, *parentMark, error) {
	err := s.c.co.CompleteScope(s.ctx)
	switch {
	case err == nil:
		return nil, nil, nil
	case errors.Is(err, spi.ErrCalloutSuperseded):
		// Row 1.
		return supersededBy(err), nil, nil
	case errors.Is(err, spi.ErrTxRollbackOnly):
		// Row 4: a write passes the mark up, which only DiscardScope does,
		// with the code the refusal names.
		var mark *parentMark
		var failed error
		if class == Write {
			mark = &parentMark{code: failedCodeOf(err)}
			failed = s.discard(mark)
		}
		return common.TransactionRollbackOnlyJoined(), mark, failed
	case errors.Is(err, spi.ErrScopeConflict):
		return scopeConflict(err), nil, nil
	}
	return decide(s, class, completionFailure(err))
}

// discard implements ending: DiscardScope, which marks the parent itself with
// the mark's code. A mark always passes a code: the backend marks nothing on
// an empty one.
func (s *durableScope) discard(mark *parentMark) error {
	code := ""
	if mark != nil {
		code = mark.code
		if code == "" {
			code = common.ErrCodeTransactionRollbackOnly
		}
	}
	if err := s.c.co.DiscardScope(s.ctx, code); err != nil {
		return &discardFailure{err: err}
	}
	return nil
}

// Close implements Scope for a processor scope. A mark on the scope goes with
// it: the scope is discarded without passing one up.
func (s *durableScope) Close(procErr error) error {
	if procErr != nil {
		if err := s.c.co.DiscardScope(s.ctx, ""); err != nil {
			return fmt.Errorf("failed to discard a processor scope: %w", errors.Join(ErrScopeInfra, err))
		}
		return procErr
	}
	err := s.c.co.CompleteScope(s.ctx)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, spi.ErrCalloutSuperseded):
		return supersededBy(err)
	case errors.Is(err, spi.ErrTxRollbackOnly):
		return rollbackOnly(s.ctx, s.c.txMgr, s.txID, failedCodeOf(err))
	case errors.Is(err, spi.ErrScopeConflict):
		return scopeConflict(err)
	}
	return fmt.Errorf("failed to complete a processor scope: %w", errors.Join(ErrScopeInfra, err))
}

// toSPIPairs converts a pass's pairs to the backend's.
func toSPIPairs(pairs []fence.Pair) []spi.CalloutPair {
	out := make([]spi.CalloutPair, len(pairs))
	for i, p := range pairs {
		out[i] = spi.CalloutPair{Callout: p.Callout, Major: p.Major, Minor: p.Minor}
	}
	return out
}
