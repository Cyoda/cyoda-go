package txcoord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go/internal/common"
	"github.com/cyoda/cyoda-go/internal/fence"
)

func mustCancelledWith(t *testing.T, ctx context.Context, cause error) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(watchdog):
		t.Fatalf("context not cancelled within %v", watchdog)
	}
	if got := context.Cause(ctx); !errors.Is(got, cause) {
		t.Fatalf("cancellation cause = %v; want %v", got, cause)
	}
}

func outcomeOf(err error) *common.Outcome {
	var appErr *common.AppError
	if errors.As(err, &appErr) {
		return &common.Outcome{Status: appErr.Status, Code: appErr.Code, Retryable: appErr.Retryable, Err: appErr}
	}
	return &common.Outcome{Status: http.StatusInternalServerError, Code: common.ErrCodeServerError, Err: err}
}

func TestEnter_NotCurrentPass410(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	defer endC1()

	_, _, err := e.enter(Write, pair("c1", 2))
	mustCode(t, err, http.StatusGone, common.ErrCodeCalloutSuperseded, false)
	_, _, err = e.enter(Write, pair("nobody", 1))
	mustCode(t, err, http.StatusGone, common.ErrCodeCalloutSuperseded, false)
	mustCalls(t, e.tm)
}

func TestEnter_SiblingOfPausedScopeIsBusy(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Write, pair("c1", 1))
	_, endX1 := e.callout(actx, "x1")
	resume := Pause(actx)

	_, _, err := e.enter(Write, pair("c1", 1))
	mustCode(t, err, http.StatusServiceUnavailable, common.ErrCodeTransactionBusy, true)

	d, _ := e.mustEnter(Write, pair("x1", 1), pair("c1", 1))
	mustOwnAnswer(t, d.End(&common.Outcome{}))
	endX1()
	if err := resume(); err != nil {
		t.Fatalf("resume = %v", err)
	}
	mustOwnAnswer(t, a.End(&common.Outcome{}))

	// Once nothing is paused, a sibling is admitted again.
	b, _ := e.mustEnter(Write, pair("c1", 1))
	mustOwnAnswer(t, b.End(&common.Outcome{}))
	endC1()
	mustSealOK(t, e)
}

func TestEnter_QueuedSiblingRefusedWhenScopePauses(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Write, pair("c1", 1))

	var bErr error
	bDone := async(func() { _, _, bErr = e.enter(Write, pair("c1", 1)) })
	mustQueue(t, e, 1)
	mustStillBlock(t, "the queued sibling", bDone)

	_, endX1 := e.callout(actx, "x1")
	resume := Pause(actx)
	mustFinish(t, "the queued sibling", bDone)
	mustCode(t, bErr, http.StatusServiceUnavailable, common.ErrCodeTransactionBusy, true)

	endX1()
	if err := resume(); err != nil {
		t.Fatalf("resume = %v", err)
	}
	mustOwnAnswer(t, a.End(&common.Outcome{}))
	endC1()
	mustSealOK(t, e)
}

// A descendant's callback can arrive, and queue, before its parent pauses. When
// the parent pauses the descendant takes the slot, and a sibling queued behind
// it is refused at that moment, not when the descendant ends.
func TestEnter_QueuedSiblingBehindDescendantRefusedAtPause(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Write, pair("c1", 1))
	_, endX1 := e.callout(actx, "x1")

	var d Scope
	var dErr error
	dDone := async(func() { d, _, dErr = e.enter(Write, pair("x1", 1), pair("c1", 1)) })
	mustQueue(t, e, 1)
	var bErr error
	bDone := async(func() { _, _, bErr = e.enter(Write, pair("c1", 1)) })
	mustQueue(t, e, 2)

	resume := Pause(actx)
	mustFinish(t, "the queued descendant", dDone)
	if dErr != nil {
		t.Fatalf("descendant refused: %v", dErr)
	}
	mustFinish(t, "the queued sibling", bDone)
	mustCode(t, bErr, http.StatusServiceUnavailable, common.ErrCodeTransactionBusy, true)

	mustOwnAnswer(t, d.End(&common.Outcome{}))
	endX1()
	if err := resume(); err != nil {
		t.Fatalf("resume = %v", err)
	}
	mustOwnAnswer(t, a.End(&common.Outcome{}))
	endC1()
	mustSealOK(t, e)
}

// mustQueue polls until n requests queue for the transaction's gate.
func mustQueue(t *testing.T, e *env, n int) {
	t.Helper()
	deadline := time.Now().Add(watchdog)
	for !e.c.AtCapacity(e.txID, n) {
		if time.Now().After(deadline) {
			t.Fatalf("%d requests never queued", n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEnter_SealedTransactionRefused410(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	finish, err := e.c.Seal(e.userCtx, e.txID)
	if err != nil {
		t.Fatalf("Seal = %v", err)
	}

	_, _, err = e.enter(Write, pair("c1", 1))
	mustCode(t, err, http.StatusGone, common.ErrCodeCalloutSuperseded, false)
	mustCalls(t, e.tm)
	endC1()
	finish()
	if n := e.c.len(); n != 0 {
		t.Fatalf("registry holds %d entries after finish; want 0", n)
	}
}

func TestAdvance_WaitsForLowerMajorScopes(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Write, pair("c1", 1))

	advanced := async(func() { e.c.Advance("c1", 2) })
	mustSuperseded(t, actx)
	mustStillBlock(t, "Advance", advanced)
	mustAnswer(t, a.End(&common.Outcome{}), http.StatusGone, common.ErrCodeCalloutSuperseded, false)
	mustFinish(t, "Advance", advanced)

	// A scope at the current major is not waited for.
	b, _ := e.mustEnter(Write, pair("c1", 2))
	mustFinish(t, "Advance to the same major", async(func() { e.c.Advance("c1", 2) }))
	mustOwnAnswer(t, b.End(&common.Outcome{}))

	endC1()
	mustSealOK(t, e)
}

func TestCalloutEnd_WaitsForScopesAndCancelsNested(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Write, pair("c1", 1))
	x1ctx, endX1 := e.callout(actx, "x1")
	resume := Pause(actx)

	ended := async(endC1)
	mustCancelledWith(t, x1ctx, fence.ErrSuperseded)
	mustStillBlock(t, "the callout's end", ended)

	endX1() // the nested callout's loop returns on its cancelled context
	err := resume()
	mustCode(t, err, http.StatusGone, common.ErrCodeCalloutSuperseded, false)
	mustAnswer(t, a.End(outcomeOf(err)), http.StatusGone, common.ErrCodeCalloutSuperseded, false)
	mustFinish(t, "the callout's end", ended)
	mustCalls(t, e.tm, "savepoint", "rollback")
	mustSealOK(t, e)
}

func TestCalloutEnd_AnsweredBeforeCallbackReturned(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Write, pair("c1", 1))

	ended := async(endC1)
	mustSuperseded(t, actx)
	mustStillBlock(t, "the callout's end", ended)
	mustAnswer(t, a.End(&common.Outcome{}), http.StatusGone, common.ErrCodeCalloutSuperseded, false)
	mustFinish(t, "the callout's end", ended)
	mustCalls(t, e.tm, "savepoint", "rollback")
	mustSealOK(t, e)
}

// A joined write that failed keeps nothing and leaves the transaction usable:
// a later write under the same pass is served, Seal succeeds, and the commit
// holds the owner's write and the later one, and none of the failed one's.
func TestSeal_AfterFailedJoinedWriteSucceeds(t *testing.T) {
	e := newEnv(t)
	own := e.save(e.ownerCtx)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Write, pair("c1", 1))
	lost := e.save(actx)
	mustOwnAnswer(t, a.End(failed(http.StatusConflict, common.ErrCodeUniqueViolation)))

	b, bctx := e.mustEnter(Write, pair("c1", 1))
	kept := e.save(bctx)
	mustOwnAnswer(t, b.End(&common.Outcome{}))
	endC1()

	e.commit()
	if !e.committed(own) || !e.committed(kept) || e.committed(lost) {
		t.Fatalf("committed: owner's %v, later write %v, failed write %v; want true, true, false",
			e.committed(own), e.committed(kept), e.committed(lost))
	}
}

func TestSeal_WaitsForOpenCallbackScopes(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))

	var finish func()
	var sealErr error
	sealed := async(func() { finish, sealErr = e.c.Seal(e.userCtx, e.txID) })
	mustStillBlock(t, "Seal", sealed)
	mustOwnAnswer(t, a.End(&common.Outcome{}))
	mustFinish(t, "Seal", sealed)
	if sealErr != nil {
		t.Fatalf("Seal = %v", sealErr)
	}
	endC1()
	finish()
}

func TestSeal_FinishForgetsTransaction(t *testing.T) {
	t.Run("committed", func(t *testing.T) {
		e := newEnv(t)
		_, endC1 := e.callout(e.ownerCtx, "c1")
		a, _ := e.mustEnter(Write, pair("c1", 1))
		mustOwnAnswer(t, a.End(&common.Outcome{}))
		endC1()
		mustSealOK(t, e)
		mustForgotten(t, e)
	})
	t.Run("refused", func(t *testing.T) {
		e := newEnv(t)
		_, endC1 := e.callout(e.ownerCtx, "c1")
		a, _ := e.mustEnter(Write, pair("c1", 1))
		rbErr := errors.New("the connection is gone")
		e.tm.failRollback.Store(&rbErr)
		mustAnswer(t, a.End(failed(http.StatusUnprocessableEntity, common.ErrCodeValidationFailed)),
			http.StatusInternalServerError, common.ErrCodeServerError, false)
		endC1()
		if err := e.seal(); err == nil {
			t.Fatal("Seal of a broken transaction = nil")
		}
		mustForgotten(t, e)
	})
	t.Run("never seen", func(t *testing.T) {
		e := newEnv(t)
		finish, err := e.c.Seal(e.userCtx, e.txID)
		if err != nil {
			t.Fatalf("Seal = %v", err)
		}
		finish()
		mustForgotten(t, e)
	})
}

func mustForgotten(t *testing.T, e *env) {
	t.Helper()
	if n := e.c.len(); n != 0 {
		t.Fatalf("registry holds %d entries after finish; want 0", n)
	}
}

func TestOpenChild_CloseKeeps(t *testing.T) {
	e := newEnv(t)
	p, _, err := e.c.OpenChild(e.ownerCtx)
	if err != nil {
		t.Fatalf("OpenChild = %v", err)
	}
	if err := p.Close(nil); err != nil {
		t.Fatalf("Close = %v", err)
	}
	mustCalls(t, e.tm, "savepoint", "release")
	mustSealOK(t, e)
}

func TestOpenChild_CloseFailedProcessorDiscards(t *testing.T) {
	e := newEnv(t)
	p, _, err := e.c.OpenChild(e.ownerCtx)
	if err != nil {
		t.Fatalf("OpenChild = %v", err)
	}
	procErr := errors.New("processor failed")
	if err := p.Close(procErr); !errors.Is(err, procErr) {
		t.Fatalf("Close = %v; want the processor's error", err)
	}
	mustCalls(t, e.tm, "savepoint", "rollback")
	mustSealOK(t, e)
}

// A callback of an ASYNC_NEW_TX processor's callout that fails a write keeps
// nothing, and does not stop the processor's scope: Close(nil) keeps it, with
// the writes of the callbacks that succeeded.
func TestOpenChild_CloseAfterFailedCallbackWriteKeeps(t *testing.T) {
	e := newEnv(t)
	p, pctx, err := e.c.OpenChild(e.ownerCtx)
	if err != nil {
		t.Fatalf("OpenChild = %v", err)
	}
	_, endP1 := e.callout(pctx, "p1")
	d, dctx := e.mustEnter(Write, pair("p1", 1))
	lost := e.save(dctx)
	mustOwnAnswer(t, d.End(failed(http.StatusUnprocessableEntity, common.ErrCodeValidationFailed)))
	d2, d2ctx := e.mustEnter(Write, pair("p1", 1))
	kept := e.save(d2ctx)
	mustOwnAnswer(t, d2.End(&common.Outcome{}))
	endP1()

	if err := p.Close(nil); err != nil {
		t.Fatalf("Close = %v; want nil", err)
	}
	mustCalls(t, e.tm, "savepoint", "savepoint", "rollback", "savepoint", "release", "release")
	e.commit()
	if !e.committed(kept) || e.committed(lost) {
		t.Fatalf("committed: later write %v, failed write %v; want true, false", e.committed(kept), e.committed(lost))
	}
}

func TestOpenChild_UnderCallbackPausesItsHolder(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Write, pair("c1", 1))
	p, pctx, err := e.c.OpenChild(actx)
	if err != nil {
		t.Fatalf("OpenChild = %v", err)
	}
	if got := Pairs(pctx); len(got) != 1 || got[0] != pair("c1", 1) {
		t.Fatalf("Pairs under the processor scope = %v; want the callback's", got)
	}
	_, endP1 := e.callout(pctx, "p1")
	resume := Pause(pctx)

	_, _, err = e.enter(Write, pair("c1", 1))
	mustCode(t, err, http.StatusServiceUnavailable, common.ErrCodeTransactionBusy, true)
	d, _ := e.mustEnter(Write, pair("p1", 1), pair("c1", 1))
	mustOwnAnswer(t, d.End(&common.Outcome{}))
	endP1()
	if err := resume(); err != nil {
		t.Fatalf("resume = %v", err)
	}
	if err := p.Close(nil); err != nil {
		t.Fatalf("Close = %v", err)
	}
	mustOwnAnswer(t, a.End(&common.Outcome{}))
	endC1()
	mustSealOK(t, e)
}

func TestPause_RootIsNoOp(t *testing.T) {
	e := newEnv(t)
	if err := Pause(e.ownerCtx)(); err != nil {
		t.Fatalf("resume on the owner's chain = %v", err)
	}
	if err := Pause(context.Background())(); err != nil {
		t.Fatalf("resume with no transaction = %v", err)
	}
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))
	mustOwnAnswer(t, a.End(&common.Outcome{}))
	endC1()
	mustSealOK(t, e)
}

func TestPairs(t *testing.T) {
	e := newEnv(t)
	if got := Pairs(e.ownerCtx); got != nil {
		t.Fatalf("Pairs on the owner's chain = %v; want nil", got)
	}
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Read, pair("c1", 1))
	if got := Pairs(actx); len(got) != 1 || got[0] != pair("c1", 1) {
		t.Fatalf("Pairs = %v; want [c1/1]", got)
	}
	mustOwnAnswer(t, a.End(&common.Outcome{}))
	endC1()
	mustSealOK(t, e)
}

func TestEnter_SavepointRefused(t *testing.T) {
	t.Run("lost race", func(t *testing.T) {
		e := newEnv(t)
		_, endC1 := e.callout(e.ownerCtx, "c1")
		spErr := errors.New("savepoint refused")
		e.tm.failSavepoint.Store(&spErr)
		e.tm.lost.Store(true)

		_, _, err := e.enter(Write, pair("c1", 1))
		mustCode(t, err, http.StatusConflict, common.ErrCodeConflict, false)
		mustNothingHeld(t, e)
		endC1()
	})
	t.Run("other", func(t *testing.T) {
		e := newEnv(t)
		_, endC1 := e.callout(e.ownerCtx, "c1")
		// A storage outage classifies as a retryable 503; the refusal of a
		// savepoint is not retryable, so it must not read as one.
		spErr := error(storageDown{})
		e.tm.failSavepoint.Store(&spErr)

		_, _, err := e.enter(Write, pair("c1", 1))
		mustCode(t, err, http.StatusInternalServerError, common.ErrCodeServerError, false)
		if strings.Contains(strings.ToLower(err.Error()), "retry") {
			t.Errorf("a non-retryable refusal says retry: %q", err.Error())
		}
		mustNothingHeld(t, e)
		endC1()
		mustSealOK(t, e)
	})
	t.Run("dead session", func(t *testing.T) {
		e := newEnv(t)
		_, endC1 := e.callout(e.ownerCtx, "c1")
		spErr := error(fmt.Errorf("failed to take a savepoint: %w", deadSession{}))
		e.tm.failSavepoint.Store(&spErr)

		_, _, err := e.enter(Write, pair("c1", 1))
		mustCode(t, err, http.StatusNotFound, common.ErrCodeTransactionNotFound, false)
		e.tm.failSavepoint.Store(nil)
		_, _, err = e.enter(Read, pair("c1", 1))
		mustCode(t, err, http.StatusNotFound, common.ErrCodeTransactionNotFound, false)
		endC1()
		if err := e.seal(); !errors.Is(err, spi.ErrTxNotFound) {
			t.Fatalf("Seal = %v; want the broken cause", err)
		}
	})
}

// mustNothingHeld asserts that a refused Enter left no scope open and the gate
// free: the next request is admitted at once.
func mustNothingHeld(t *testing.T, e *env) {
	t.Helper()
	e.tm.failSavepoint.Store(nil)
	e.tm.lost.Store(false)
	var s Scope
	var err error
	mustFinish(t, "the next Enter", async(func() { s, _, err = e.enter(Read, pair("c1", 1)) }))
	if err != nil {
		t.Fatalf("the next Enter = %v", err)
	}
	mustOwnAnswer(t, s.End(&common.Outcome{}))
}

type storageDown struct{}

func (storageDown) Error() string            { return "storage is down" }
func (storageDown) StorageUnavailable() bool { return true }

func TestOpenChild_SavepointOnDeadSessionBreaks(t *testing.T) {
	e := newEnv(t)
	spErr := error(fmt.Errorf("failed to take a savepoint: %w", deadSession{}))
	e.tm.failSavepoint.Store(&spErr)

	_, _, err := e.c.OpenChild(e.ownerCtx)
	if !errors.Is(err, ErrScopeInfra) {
		t.Fatalf("OpenChild = %v; want ErrScopeInfra", err)
	}
	// The transaction is now broken: a later processor scope is refused as
	// an unusable transaction, which is never the processor's failure.
	e.tm.failSavepoint.Store(nil)
	if _, _, err := e.c.OpenChild(e.ownerCtx); !errors.Is(err, ErrScopeInfra) || !errors.Is(err, spi.ErrTxNotFound) {
		t.Fatalf("OpenChild on a broken transaction = %v; want ErrScopeInfra with the broken cause", err)
	}
	if err := e.seal(); !errors.Is(err, spi.ErrTxNotFound) {
		t.Fatalf("Seal = %v; want the broken cause", err)
	}
}

// A ctx that carries a scope of another transaction registers nothing with
// that scope: the callout and the processor scope go under txID's own root.
func TestBeginCalloutAndOpenChild_ScopeOfAnotherTransaction(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Write, pair("c1", 1))

	tx2, tx2Ctx, err := e.tm.Begin(e.userCtx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = e.tm.Rollback(e.userCtx, tx2) })
	_, endY := e.c.BeginCallout(actx, tx2, "y1")
	reg := func() *calloutReg {
		e.c.mu.Lock()
		defer e.c.mu.Unlock()
		return e.c.callouts["y1"]
	}()
	if reg.tx.id != tx2 || reg.parent != reg.tx.root {
		t.Fatalf("callout registered on %s under %v; want %s's root", reg.tx.id, reg.parent.kind, tx2)
	}
	endY()

	mixed := spi.WithTransaction(actx, spi.GetTransaction(tx2Ctx))
	p, _, err := e.c.OpenChild(mixed)
	if err != nil {
		t.Fatalf("OpenChild = %v", err)
	}
	ps := p.(*scope)
	if ps.tx.id != tx2 || ps.parent != ps.tx.root {
		t.Fatalf("processor scope opened on %s; want under %s's root", ps.tx.id, tx2)
	}
	if err := p.Close(nil); err != nil {
		t.Fatalf("Close = %v", err)
	}
	_ = a.End(&common.Outcome{})
	endC1()
}

// A second Pause before the first one's resume is a no-op: it releases
// nothing, and its resume only checks.
func TestPause_TwiceIsNoOp(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Write, pair("c1", 1))
	resume1 := Pause(actx)
	resume2 := Pause(actx)
	if err := resume2(); err != nil {
		t.Fatalf("second resume = %v", err)
	}
	if err := resume1(); err != nil {
		t.Fatalf("first resume = %v", err)
	}
	mustOwnAnswer(t, a.End(&common.Outcome{}))
	// The gate is free: the next request is admitted at once.
	mustNothingHeld(t, e)
	endC1()
	mustSealOK(t, e)
}

// The end of a callout forgets only its own registration: a later callout
// registered under the same id stays registered.
func TestCalloutEnd_ForgetsOnlyItsOwnRegistration(t *testing.T) {
	e := newEnv(t)
	_, endFirst := e.c.BeginCallout(e.ownerCtx, e.txID, "dup")
	_, endSecond := e.c.BeginCallout(e.ownerCtx, e.txID, "dup")
	endFirst()
	registered := func() bool {
		e.c.mu.Lock()
		defer e.c.mu.Unlock()
		tx := e.c.txs[e.txID]
		return e.c.callouts["dup"] != nil && tx != nil && tx.callouts["dup"] != nil
	}()
	if !registered {
		t.Fatal("the first callout's end forgot the second's registration")
	}
	endSecond()
	mustForgotten(t, e)
}

// Writers enter and end scopes of one callout in a loop while the owner keeps
// raising its number. Once Advance(m) has returned, no scope admitted under a
// lower major is still open, and none of them completes afterwards: each ends
// superseded. Run with -race.
func TestAdvance_UnderConcurrentWriters_NoEarlierMajorOutlivesIt(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	defer endC1()
	var current, advanced, entered atomic.Uint32
	current.Store(1)
	advanced.Store(1)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var violations atomic.Int32
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				major := current.Load()
				s, _, err := e.enter(Write, pair("c1", major))
				if err != nil {
					continue // superseded on entry
				}
				entered.Store(major)
				time.Sleep(100 * time.Microsecond) // the scope's work, while the owner advances
				seen := advanced.Load()
				if answer := s.End(nil); answer == nil && seen > major {
					violations.Add(1) // completed after an Advance past its major returned
				}
			}
		}()
	}
	for m := uint32(2); m <= 60; m++ {
		// A writer is in a scope of the current major before it is raised.
		deadline := time.Now().Add(watchdog)
		for entered.Load() != m-1 {
			if time.Now().After(deadline) {
				t.Fatalf("no writer entered at major %d", m-1)
			}
			time.Sleep(10 * time.Microsecond)
		}
		current.Store(m)
		e.c.Advance("c1", m)
		advanced.Store(m)
		open := func() bool {
			e.c.mu.Lock()
			defer e.c.mu.Unlock()
			tx := e.c.txs[e.txID]
			return tx != nil && openUnder(tx, "c1", m)
		}()
		if open {
			t.Fatalf("a scope admitted below major %d is still open after Advance returned", m)
		}
	}
	close(stop)
	wg.Wait()
	if n := violations.Load(); n != 0 {
		t.Fatalf("%d scopes of an earlier major completed after the Advance past them returned", n)
	}
}

// The context a callout of the owner's runs under carries the root. Pause's
// resume and Check on it report nothing, also after a callback of the callout
// failed a write, and a processor scope opened on it is the root's.
func TestRootOnCalloutContext_PauseAndOpenChildUnderTheRoot(t *testing.T) {
	e := newEnv(t)
	cctx, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))
	mustOwnAnswer(t, a.End(failed(http.StatusUnprocessableEntity, common.ErrCodeValidationFailed)))

	if err := Pause(cctx)(); err != nil {
		t.Fatalf("resume on the root = %v; want nil", err)
	}
	if err := Check(cctx); err != nil {
		t.Fatalf("Check on the callout's context = %v; want nil", err)
	}
	p, _, err := e.c.OpenChild(cctx)
	if err != nil {
		t.Fatalf("OpenChild under the root = %v; want nil", err)
	}
	if ps := p.(*scope); ps.parent != ps.tx.root {
		t.Fatal("the processor scope opened on the callout's context is not the root's")
	}
	if err := p.Close(nil); err != nil {
		t.Fatalf("Close = %v", err)
	}

	endC1()
	mustSealOK(t, e)
}
