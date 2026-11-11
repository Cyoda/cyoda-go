package txcoord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// New returns the in-process coordination over txMgr. It builds its own fence
// and gate; nothing else constructs them.
//
// Lock order: the gate's lock, then inProcess.mu, then the fence's lock. The
// gate's admission predicate takes inProcess.mu, so inProcess never calls the
// gate while it holds mu; it never cancels a context under mu either.
func New(txMgr spi.TransactionManager) Coordination {
	return &inProcess{
		txMgr:    txMgr,
		fence:    fence.New(),
		gate:     txgate.New(),
		txs:      make(map[string]*txState),
		callouts: make(map[string]*calloutReg),
	}
}

type inProcess struct {
	txMgr spi.TransactionManager
	fence *fence.Fence
	gate  *txgate.Registry

	mu       sync.Mutex
	txs      map[string]*txState
	callouts map[string]*calloutReg // every callout registered with a transaction
}

// txState is the coordination of one transaction. Its fields are guarded by
// inProcess.mu.
type txState struct {
	id       string
	root     *scope
	open     map[*scope]struct{} // callback and processor scopes not yet ended
	callouts map[string]*calloutReg
	paused   []*scope // the stack of paused scopes, top last
	sealed   bool
	broken   error // why the transaction is unusable; nil while it is usable
	// changed is closed, and replaced, each time a scope leaves open: the
	// waits watch it.
	changed chan struct{}
}

// calloutReg is a callout registered with the scope it was begun from.
type calloutReg struct {
	id     string
	tx     *txState
	parent *scope
	cancel context.CancelCauseFunc
}

type scopeKind uint8

const (
	rootKind scopeKind = iota + 1
	callbackKind
	processorKind
)

// scope is one node of a transaction's scope tree. kind, class, pairs, ctx, tx
// and parent are set before the scope is published and never change.
type scope struct {
	c      *inProcess
	kind   scopeKind
	class  RequestClass // callback scopes only
	pairs  []fence.Pair // callback scopes only: own pair first
	ctx    context.Context
	tx     *txState
	parent *scope
	// savepoint is written once, by the goroutine that opened the scope, and
	// read by the one that ends it.
	savepoint string

	// Guarded by inProcess.mu.
	release    func() // the gate slot a callback scope holds; nil while paused
	marked     bool
	failedCode string // the code the failure that marked the scope got
	callouts   map[string]*calloutReg
}

func (c *inProcess) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.txs) + len(c.callouts)
}

// txLocked returns txID's state, creating it when it is not known.
func (c *inProcess) txLocked(txID string) *txState {
	tx := c.txs[txID]
	if tx == nil {
		tx = &txState{
			id:       txID,
			open:     make(map[*scope]struct{}),
			callouts: make(map[string]*calloutReg),
			changed:  make(chan struct{}),
		}
		tx.root = &scope{c: c, kind: rootKind, tx: tx}
		c.txs[txID] = tx
	}
	return tx
}

// forgetIfIdleLocked drops a transaction's state when it holds nothing a fresh
// state would not: no scope, no callout, no mark, not broken, not sealed. A
// sealed state is dropped by Seal's finish.
func (c *inProcess) forgetIfIdleLocked(tx *txState) {
	if len(tx.open) == 0 && len(tx.callouts) == 0 && !tx.sealed && !tx.root.marked && tx.broken == nil && c.txs[tx.id] == tx {
		delete(c.txs, tx.id)
	}
}

// await blocks until done, called under mu, reports true. done may change the
// state in the same critical section in which it reports true.
func (c *inProcess) await(tx *txState, done func() bool) {
	for {
		changed := func() chan struct{} {
			c.mu.Lock()
			defer c.mu.Unlock()
			if done() {
				return nil
			}
			return tx.changed
		}()
		if changed == nil {
			return
		}
		<-changed
	}
}

// openUnder reports whether an open scope of tx was admitted under calloutID at
// a major below below. mu must be held.
func openUnder(tx *txState, calloutID string, below uint32) bool {
	for s := range tx.open {
		for _, p := range s.pairs {
			if p.Callout == calloutID && p.Major < below {
				return true
			}
		}
	}
	return false
}

// BeginCallout implements Coordination.
func (c *inProcess) BeginCallout(ctx context.Context, txID, calloutID string) (context.Context, func()) {
	// The scope on ctx is the parent only when it belongs to txID; otherwise
	// the callout is the root's, and no pair of another transaction encloses
	// it.
	from := scopeFrom(ctx)
	if from != nil && from.tx.id != txID {
		from = nil
	}
	var outer []fence.Pair
	if from != nil {
		outer = Pairs(ctx)
	}
	fctx, fenceEnd := c.fence.Begin(ctx, calloutID, outer)
	cctx, cancel := context.WithCancelCause(fctx)
	var once sync.Once
	if txID == "" {
		return cctx, func() {
			once.Do(func() {
				fenceEnd()
				cancel(context.Canceled)
			})
		}
	}
	reg, marked := func() (*calloutReg, bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		tx := c.txLocked(txID)
		parent := from
		if parent == nil || parent.tx != tx {
			parent = tx.root
		}
		reg := &calloutReg{id: calloutID, tx: tx, parent: parent, cancel: cancel}
		tx.callouts[calloutID] = reg
		c.callouts[calloutID] = reg
		if parent.callouts == nil {
			parent.callouts = make(map[string]*calloutReg)
		}
		parent.callouts[calloutID] = reg
		_, marked := parent.markedLocked()
		return reg, marked
	}()
	if marked {
		cancel(common.ErrRollbackOnly)
	}
	if reg.parent.kind == rootKind {
		// So that Check on the callout's context reports the root's mark: the
		// owner's chain carries no scope of its own.
		cctx = context.WithValue(cctx, scopeKey{}, reg.parent)
	}
	return cctx, func() {
		once.Do(func() { c.endCallout(reg, fenceEnd) })
	}
}

// endCallout unregisters the callout from the fence (its passes are refused and
// the callouts begun under it cancelled), waits until no open scope admitted
// under it remains, then forgets it.
func (c *inProcess) endCallout(reg *calloutReg, fenceEnd func()) {
	fenceEnd()
	c.await(reg.tx, func() bool { return !openUnder(reg.tx, reg.id, ^uint32(0)) })
	func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if reg.tx.callouts[reg.id] == reg {
			delete(reg.tx.callouts, reg.id)
		}
		if c.callouts[reg.id] == reg {
			delete(c.callouts, reg.id)
		}
		if reg.parent.callouts[reg.id] == reg {
			delete(reg.parent.callouts, reg.id)
		}
		c.forgetIfIdleLocked(reg.tx)
	}()
	reg.cancel(context.Canceled)
}

// Advance implements Coordination.
func (c *inProcess) Advance(calloutID string, major uint32) {
	c.fence.Advance(calloutID, major)
	reg := func() *calloutReg {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.callouts[calloutID]
	}()
	if reg == nil {
		return
	}
	c.await(reg.tx, func() bool { return !openUnder(reg.tx, calloutID, major) })
}

// Seal implements Coordination.
func (c *inProcess) Seal(ctx context.Context, txID string) (func(), error) {
	tx := func() *txState {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.txLocked(txID)
	}()
	var broken error
	var marked bool
	var code string
	c.await(tx, func() bool {
		for s := range tx.open {
			if s.kind == callbackKind {
				return false
			}
		}
		tx.sealed = true
		broken, marked, code = tx.broken, tx.root.marked, tx.root.failedCode
		return true
	})
	finish := func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.txs[txID] == tx {
			delete(c.txs, txID)
		}
	}
	return finish, sealRefusal(ctx, c.txMgr, txID, broken, marked, code)
}

// sealRefusal is Seal's answer for a transaction that is broken (broken, the
// cause) or whose root is marked (code, the failed request's code when it is
// known): the lost race first, then the broken cause, then the mark. nil when
// it is neither.
func sealRefusal(ctx context.Context, txMgr spi.TransactionManager, txID string, broken error, marked bool, code string) error {
	if broken == nil && !marked {
		return nil
	}
	if lostRace(ctx, txMgr, txID) {
		if broken != nil {
			return fmt.Errorf("the transaction lost a write race and is unusable (%v): %w", broken, spi.ErrTxAborted)
		}
		return lostRaceError(code)
	}
	if broken != nil {
		return fmt.Errorf("the transaction is unusable: %w", broken)
	}
	return common.TransactionRollbackOnly(code)
}

// lostRace asks the transaction manager; an error reads as not lost, so that
// the caller reports what it would have reported without asking.
func lostRace(ctx context.Context, txMgr spi.TransactionManager, txID string) bool {
	lost, err := txMgr.LostRace(context.WithoutCancel(ctx), txID)
	return err == nil && lost
}

func (c *inProcess) lostRace(ctx context.Context, txID string) bool {
	return lostRace(ctx, c.txMgr, txID)
}

// lostRaceError is the owner's report of a marked scope in a transaction that
// lost a write race: the conflict, with the mark as context. failedCode is
// empty when the storage backend coordinates the transaction itself.
func lostRaceError(failedCode string) error {
	if failedCode == "" {
		return fmt.Errorf("the transaction lost a write race; a request that joined it failed: %w", spi.ErrTxAborted)
	}
	return fmt.Errorf("the transaction lost a write race; a request that joined it failed with %s: %w", failedCode, spi.ErrTxAborted)
}

// rollbackOnly is the refusal for a marked chain, the lost race first.
func rollbackOnly(ctx context.Context, txMgr spi.TransactionManager, txID, failedCode string) error {
	if lostRace(ctx, txMgr, txID) {
		return lostRaceError(failedCode)
	}
	return common.TransactionRollbackOnly(failedCode)
}

func (c *inProcess) rollbackOnly(ctx context.Context, txID, failedCode string) error {
	return rollbackOnly(ctx, c.txMgr, txID, failedCode)
}

// lostRaceAnswer is a joined request's answer in a transaction that lost a
// write race: not retryable, because the request cannot succeed in it. The
// owner retries the operation.
func lostRaceAnswer() *common.AppError {
	return common.Operational(http.StatusConflict, common.ErrCodeConflict,
		"the transaction lost a write race and cannot commit; the operation that owns it is retried, not this request").
		WithCause(spi.ErrTxAborted)
}

// transactionNotFound refuses a joined request of a transaction that is no
// longer usable.
func transactionNotFound(cause error) *common.AppError {
	return common.Operational(http.StatusNotFound, common.ErrCodeTransactionNotFound,
		"transaction not found or no longer active").WithCause(cause)
}

// Enter implements Coordination.
func (c *inProcess) Enter(ctx context.Context, txID string, pairs []fence.Pair, class RequestClass, maxWaiters int) (Scope, context.Context, error) {
	admitted, err := c.fence.Admit(ctx, pairs)
	if err != nil {
		return nil, ctx, err
	}
	calloutID := pairs[0].Callout // Admit refuses an empty pass

	// The wait for the gate is the one part of a joined request that its
	// client may still call off: nothing of the transaction has been touched.
	release, err := c.gate.AcquireJoined(admitted, txID, maxWaiters, func() bool { return c.admits(txID, calloutID) })
	if err != nil {
		switch {
		case errors.Is(err, txgate.ErrNotAdmitted):
			return nil, ctx, common.TransactionBusy()
		case errors.Is(err, txgate.ErrTooManyWaiters):
			return nil, ctx, common.TooManyJoinedRequests()
		default:
			// The request's own context ended while it queued: the client
			// went away.
			return nil, ctx, common.ClientGone(err)
		}
	}

	// From here the request is detached from its client's cancellation: it
	// ends by finishing or by being refused at a check.
	s := &scope{c: c, kind: callbackKind, class: class, pairs: append([]fence.Pair(nil), pairs...), release: release}
	s.ctx = context.WithValue(context.WithoutCancel(admitted), scopeKey{}, s)
	if err := c.open(s, txID, calloutID); err != nil {
		release()
		return nil, ctx, err
	}
	if class == Outside {
		return s, s.ctx, nil
	}
	sp, err := c.txMgr.Savepoint(s.ctx, txID)
	if err != nil {
		refusal, broken := c.savepointRefusal(s.ctx, txID, err)
		c.leave(s, nil, broken)
		s.releaseGate()
		return nil, ctx, refusal
	}
	s.savepoint = sp
	return s, s.ctx, nil
}

// admits is the gate's admission predicate for a request whose pass names
// calloutID: true while no scope of the transaction is paused, or when the top
// paused scope is on the chain the new scope would open under. A callout no
// longer registered admits: the check after the gate refuses it.
func (c *inProcess) admits(txID, calloutID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	tx := c.txs[txID]
	if tx == nil || len(tx.paused) == 0 {
		return true
	}
	reg := tx.callouts[calloutID]
	if reg == nil {
		return true
	}
	top := tx.paused[len(tx.paused)-1]
	for s := reg.parent; s != nil; s = s.parent {
		if s == top {
			return true
		}
	}
	return false
}

// open places a callback scope in its transaction's tree, under the gate. The
// checks and the placement are one step, so a wait that starts after it sees
// the scope, and Seal either sees it or refuses it.
func (c *inProcess) open(s *scope, txID, calloutID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	tx := c.txs[txID]
	var reg *calloutReg
	if tx != nil {
		reg = tx.callouts[calloutID]
	}
	if reg == nil {
		return fence.NewSupersededError().
			WithCause(fmt.Errorf("callback refused: its callout has ended: %w", fence.ErrSuperseded))
	}
	if err := fence.Check(s.ctx); err != nil {
		return err
	}
	if tx.broken != nil {
		return transactionNotFound(tx.broken)
	}
	if tx.sealed {
		return fence.NewSupersededError().
			WithCause(fmt.Errorf("callback refused: the transaction is being committed or rolled back: %w", fence.ErrSuperseded))
	}
	if _, marked := reg.parent.markedLocked(); marked {
		return common.TransactionRollbackOnlyJoined()
	}
	s.tx, s.parent = tx, reg.parent
	tx.open[s] = struct{}{}
	return nil
}

// savepointRefusal answers a request whose savepoint could not be taken: the
// handler did not run, so nothing is marked. A lost race answers as the lost
// race. A session that has ended answers 404 and leaves the transaction broken
// (the returned cause). Anything else is a 500, not retryable: the request
// cannot succeed in this transaction, so no answer may invite a retry.
func (c *inProcess) savepointRefusal(ctx context.Context, txID string, err error) (*common.AppError, error) {
	if c.lostRace(ctx, txID) {
		return lostRaceAnswer(), nil
	}
	if errors.Is(err, spi.ErrTxNotFound) {
		return transactionNotFound(err), sessionEnded(err)
	}
	return common.InternalWithCode(common.ErrCodeServerError,
		"the request could not open its scope in the transaction", err), nil
}

// sessionEnded is the broken cause of a transaction whose session has ended.
func sessionEnded(err error) error {
	return fmt.Errorf("the transaction's session has ended: %w", err)
}

// leave takes an ended scope out of its transaction's tree: it records broken,
// sets mark on the parent when it is not nil, and wakes the waits.
func (c *inProcess) leave(s *scope, mark *parentMark, broken error) {
	cancels := func() []context.CancelCauseFunc {
		c.mu.Lock()
		defer c.mu.Unlock()
		tx := s.tx
		if broken != nil && tx.broken == nil {
			tx.broken = broken
		}
		var cancels []context.CancelCauseFunc
		if mark != nil {
			cancels = s.parent.markLocked(mark.code)
		}
		delete(tx.open, s)
		close(tx.changed)
		tx.changed = make(chan struct{})
		c.forgetIfIdleLocked(tx)
		return cancels
	}()
	for _, cancel := range cancels {
		cancel(common.ErrRollbackOnly)
	}
}

// markLocked marks s rollback-only, keeping the first failure's code, and
// returns the cancel funcs of the callouts begun from it. mu must be held.
func (s *scope) markLocked(code string) []context.CancelCauseFunc {
	if !s.marked {
		s.marked, s.failedCode = true, code
	}
	cancels := make([]context.CancelCauseFunc, 0, len(s.callouts))
	for _, reg := range s.callouts {
		cancels = append(cancels, reg.cancel)
	}
	return cancels
}

// markedLocked reports whether s or an ancestor is marked, with the code of the
// nearest mark. mu must be held.
func (s *scope) markedLocked() (string, bool) {
	for n := s; n != nil; n = n.parent {
		if n.marked {
			return n.failedCode, true
		}
	}
	return "", false
}

func (s *scope) markedCode() (string, bool) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	return s.markedLocked()
}

// check implements Check for s.
func (s *scope) check() error {
	if err := fence.Check(s.ctx); err != nil {
		return err
	}
	if code, marked := s.markedCode(); marked {
		return s.c.rollbackOnly(s.ctx, s.tx.id, code)
	}
	return nil
}

// releaseGate gives up the gate slot a callback scope holds.
func (s *scope) releaseGate() {
	release := func() func() {
		s.c.mu.Lock()
		defer s.c.mu.Unlock()
		r := s.release
		s.release = nil
		return r
	}()
	if release != nil {
		release()
	}
}

// pause implements Pause for s. The gate is held by the nearest callback scope
// on s's chain; s itself goes on the paused stack, because the callout it makes
// is registered with s and its callbacks open under s.
func (c *inProcess) pause(s *scope) func() error {
	holder, release := func() (*scope, func()) {
		c.mu.Lock()
		defer c.mu.Unlock()
		h := s
		for h != nil && h.kind != callbackKind {
			h = h.parent
		}
		if h == nil || h.release == nil {
			return nil, nil
		}
		r := h.release
		h.release = nil
		s.tx.paused = append(s.tx.paused, s)
		return h, r
	}()
	var once sync.Once
	var err error
	if holder == nil {
		return func() error {
			once.Do(func() { err = s.check() })
			return err
		}
	}
	// The stack is pushed before the slot is let go: the hand-over re-reads
	// admission, and Reevaluate refuses the siblings still queued.
	release()
	c.gate.Reevaluate(s.tx.id)
	return func() error {
		once.Do(func() {
			r := c.gate.AcquireResume(s.tx.id)
			func() {
				c.mu.Lock()
				defer c.mu.Unlock()
				holder.release = r
				i := slices.Index(s.tx.paused, s)
				s.tx.paused = slices.Delete(s.tx.paused, i, i+1)
			}()
			err = s.check()
		})
		return err
	}
}

// OpenChild implements Coordination.
func (c *inProcess) OpenChild(ctx context.Context) (Scope, context.Context, error) {
	spiTx := spi.GetTransaction(ctx)
	if spiTx == nil {
		return nil, ctx, errors.New("failed to open a processor scope: the context carries no transaction")
	}
	txID := spiTx.ID
	// The scope on ctx is the parent only when it belongs to ctx's
	// transaction; otherwise the processor scope is the root's.
	parent := scopeFrom(ctx)
	if parent != nil && parent.tx.id != txID {
		parent = nil
	}
	if parent != nil {
		if err := parent.check(); err != nil {
			return nil, ctx, err
		}
	}
	s := &scope{c: c, kind: processorKind}
	s.ctx = context.WithValue(ctx, scopeKey{}, s)
	broken := func() error {
		c.mu.Lock()
		defer c.mu.Unlock()
		tx := c.txLocked(txID)
		if parent == nil || parent.tx != tx {
			parent = tx.root
		}
		if tx.broken != nil {
			return tx.broken
		}
		s.tx, s.parent = tx, parent
		tx.open[s] = struct{}{}
		return nil
	}()
	if broken != nil {
		return nil, ctx, fmt.Errorf("failed to open a processor scope: the transaction is unusable: %w", errors.Join(ErrScopeInfra, broken))
	}
	sp, err := c.txMgr.Savepoint(s.ctx, txID)
	if err != nil {
		var broken error
		if errors.Is(err, spi.ErrTxNotFound) {
			broken = sessionEnded(err)
		}
		c.leave(s, nil, broken)
		return nil, ctx, fmt.Errorf("failed to open a processor scope: %w", errors.Join(ErrScopeInfra, err))
	}
	s.savepoint = sp
	return s, s.ctx, nil
}

// AtCapacity implements Coordination.
func (c *inProcess) AtCapacity(txID string, maxWaiters int) bool {
	return c.gate.AtCapacity(txID, maxWaiters)
}
