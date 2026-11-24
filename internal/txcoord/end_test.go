package txcoord

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go/internal/common"
)

// One test per row of the table that decides how a callback scope ends. The
// first matching row decides; precedence runs from row 1 down.

func mustAnswer(t *testing.T, ans *common.AppError, status int, code string, retryable bool) {
	t.Helper()
	if ans == nil {
		t.Fatalf("answer = nil (the handler's own); want %d %s", status, code)
	}
	mustCode(t, ans, status, code, retryable)
}

func mustOwnAnswer(t *testing.T, ans *common.AppError) {
	t.Helper()
	if ans != nil {
		t.Fatalf("answer = %d %s; want nil (the handler's own)", ans.Status, ans.Code)
	}
}

func mustSealOK(t *testing.T, e *env) {
	t.Helper()
	if err := e.seal(); err != nil {
		t.Fatalf("Seal = %v; want nil", err)
	}
}

func TestEnd_Row1_SupersededDiscardsNoMark(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Write, pair("c1", 1))

	advanced := async(func() { e.c.Advance("c1", 2) })
	mustSuperseded(t, actx)
	mustStillBlock(t, "Advance", advanced)
	mustAnswer(t, a.End(&common.Outcome{}), http.StatusGone, common.ErrCodeCalloutSuperseded, false)
	mustFinish(t, "Advance", advanced)

	mustCalls(t, e.tm, "savepoint", "rollback")
	endC1()
	mustSealOK(t, e)
}

func TestEnd_Row2_LostRaceNotRetryable(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))
	e.tm.lost.Store(true)

	ans := a.End(failed(http.StatusBadRequest, common.ErrCodeBadRequest))
	mustAnswer(t, ans, http.StatusConflict, common.ErrCodeConflict, false)
	if !errors.Is(ans, spi.ErrTxAborted) {
		t.Errorf("lost-race answer does not carry spi.ErrTxAborted: %v", ans)
	}
	mustCalls(t, e.tm, "savepoint", "rollback")
	endC1()
	mustSealOK(t, e)
}

func TestEnd_Row3_DeadSession404(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))

	dead := common.StorageUnavailable(fmt.Errorf("failed to save: %w", deadSession{}))
	ans := a.End(&common.Outcome{Status: dead.Status, Code: dead.Code, Retryable: true, Err: dead})
	mustAnswer(t, ans, http.StatusNotFound, common.ErrCodeTransactionNotFound, false)
	mustCalls(t, e.tm, "savepoint", "rollback")

	// A broken transaction refuses further joined requests with 404.
	_, _, err := e.enter(Read, pair("c1", 1))
	mustCode(t, err, http.StatusNotFound, common.ErrCodeTransactionNotFound, false)

	endC1()
	err = e.seal()
	if !errors.Is(err, spi.ErrTxNotFound) {
		t.Fatalf("Seal = %v; want the broken cause", err)
	}
	mustCode(t, common.Internal("failed to commit", err), http.StatusServiceUnavailable, common.ErrCodeStorageUnavailable, true)
}

// Row 3 answers 404 even when the scope's own rollback fails too: the session
// has ended, so the discard fails like the request did, and the answer stays
// the row's rather than the 500 of a failed discard.
func TestEnd_Row3_OwnDiscardFailureStill404(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))

	var rbErr error = fmt.Errorf("failed to roll back: %w", deadSession{})
	e.tm.failRollback.Store(&rbErr)
	dead := common.StorageUnavailable(fmt.Errorf("failed to save: %w", deadSession{}))
	ans := a.End(&common.Outcome{Status: dead.Status, Code: dead.Code, Retryable: true, Err: dead})
	mustAnswer(t, ans, http.StatusNotFound, common.ErrCodeTransactionNotFound, false)
	mustCalls(t, e.tm, "savepoint", "rollback")

	_, _, err := e.enter(Read, pair("c1", 1))
	mustCode(t, err, http.StatusNotFound, common.ErrCodeTransactionNotFound, false)

	endC1()
	if err := e.seal(); !errors.Is(err, spi.ErrTxNotFound) {
		t.Fatalf("Seal = %v; want the broken cause", err)
	}
}

// Row 3 answers 404 even when the request's error carries another scope's
// failed discard: a processor scope under the callback scope could not be
// rolled back because the session had ended, and its Close error reached the
// handler's answer. Only the ending scope's own discard decides the 500.
func TestEnd_Row3_NestedDiscardFailureStill404(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Write, pair("c1", 1))

	p, _, err := e.c.OpenChild(actx)
	if err != nil {
		t.Fatalf("OpenChild = %v", err)
	}
	var rbErr error = fmt.Errorf("failed to roll back: %w", deadSession{})
	e.tm.failRollback.Store(&rbErr)
	closeErr := p.Close(errors.New("processor failed"))
	if !errors.Is(closeErr, ErrScopeInfra) {
		t.Fatalf("Close = %v; want a scope-infrastructure failure", closeErr)
	}
	e.tm.failRollback.Store(nil)

	failure := common.Internal("failed to run the processor", closeErr)
	ans := a.End(&common.Outcome{Status: failure.Status, Code: failure.Code, Retryable: failure.Retryable, Err: failure})
	mustAnswer(t, ans, http.StatusNotFound, common.ErrCodeTransactionNotFound, false)

	endC1()
	if err := e.seal(); !errors.Is(err, spi.ErrTxNotFound) {
		t.Fatalf("Seal = %v; want the broken cause", err)
	}
}

func TestEnd_Row4_SuccessReleases(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))

	mustOwnAnswer(t, a.End(&common.Outcome{}))
	mustCalls(t, e.tm, "savepoint", "release")
	endC1()
	mustSealOK(t, e)
}

// A write whose completion fails is a failed write (row 6): discarded and
// answered 500, and the transaction goes on.
func TestEnd_Row4_ReleaseFails500NoMark(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))
	relErr := errors.New("current transaction is aborted (25P02)")
	e.tm.failRelease.Store(&relErr)

	mustAnswer(t, a.End(&common.Outcome{}), http.StatusInternalServerError, common.ErrCodeServerError, false)
	mustCalls(t, e.tm, "savepoint", "release", "rollback")
	e.tm.failRelease.Store(nil)
	b, _ := e.mustEnter(Write, pair("c1", 1))
	mustOwnAnswer(t, b.End(&common.Outcome{}))
	endC1()
	mustSealOK(t, e)
}

func TestEnd_Row5_RetryableDiscardsNoMark(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))

	o := failed(http.StatusConflict, common.ErrCodeConflict)
	o.Retryable = true
	mustOwnAnswer(t, a.End(o))
	mustCalls(t, e.tm, "savepoint", "rollback")
	endC1()
	mustSealOK(t, e)
}

// Row 6: a write answered with a failure that is not retryable is discarded
// and answered as the handler answered it. The transaction goes on: a later
// request under the same pass completes, and Seal succeeds. A backstop answer
// (a buffered HTTP status, a raw gRPC error) that carries no code ends the
// same way.
func TestEnd_Row6_NonRetryableWriteDiscardsNoMark(t *testing.T) {
	for name, o := range map[string]*common.Outcome{
		"with a code":    failed(http.StatusUnprocessableEntity, common.ErrCodeValidationFailed),
		"without a code": {Status: http.StatusRequestEntityTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			_, endC1 := e.callout(e.ownerCtx, "c1")
			a, _ := e.mustEnter(Write, pair("c1", 1))

			mustOwnAnswer(t, a.End(o))
			b, _ := e.mustEnter(Write, pair("c1", 1))
			mustOwnAnswer(t, b.End(&common.Outcome{}))
			mustCalls(t, e.tm, "savepoint", "rollback", "savepoint", "release")
			endC1()
			mustSealOK(t, e)
		})
	}
}

func TestEnd_Row7_Read4xxKeeps(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Read, pair("c1", 1))

	mustOwnAnswer(t, a.End(failed(http.StatusNotFound, common.ErrCodeEntityNotFound)))
	mustCalls(t, e.tm, "savepoint", "release")
	endC1()
	mustSealOK(t, e)
}

func TestEnd_Row8_Read5xxDiscards(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Read, pair("c1", 1))

	mustOwnAnswer(t, a.End(failed(http.StatusInternalServerError, common.ErrCodeServerError)))
	mustCalls(t, e.tm, "savepoint", "rollback")
	endC1()
	mustSealOK(t, e)
}

func TestEnd_Outside_NeverRewritten(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Outside, pair("c1", 1))

	advanced := async(func() { e.c.Advance("c1", 2) })
	mustSuperseded(t, actx)
	mustStillBlock(t, "Advance", advanced)
	mustOwnAnswer(t, a.End(failed(http.StatusBadRequest, common.ErrCodeBadRequest)))
	mustFinish(t, "Advance", advanced)

	mustCalls(t, e.tm)
	endC1()
	mustSealOK(t, e)
}

// A write whose handler panicked is a failed write (row 6): discarded, and the
// transaction goes on.
func TestEnd_PanicOutcome_DiscardsNoMark(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))

	mustOwnAnswer(t, a.End(&common.Outcome{Panicked: true}))
	b, _ := e.mustEnter(Write, pair("c1", 1))
	mustOwnAnswer(t, b.End(&common.Outcome{}))
	mustCalls(t, e.tm, "savepoint", "rollback", "savepoint", "release")
	endC1()
	mustSealOK(t, e)
}

// A completion that fails is judged again from row 1: a lost race answers by
// row 2, not with the 500.
func TestEnd_Row4_ReleaseFailsLostRaceAnswersRow2(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))
	relErr := errors.New("release refused")
	e.tm.failRelease.Store(&relErr)
	e.tm.lost.Store(true)

	mustAnswer(t, a.End(&common.Outcome{}), http.StatusConflict, common.ErrCodeConflict, false)
	mustCalls(t, e.tm, "savepoint", "release", "rollback")
	e.tm.failRelease.Store(nil)
	endC1()
}

// A read whose completion fails is a 5xx read: discarded (row 8).
func TestEnd_Row4_ReadReleaseFailsAnswersRow8(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Read, pair("c1", 1))
	relErr := errors.New("release refused")
	e.tm.failRelease.Store(&relErr)

	mustAnswer(t, a.End(&common.Outcome{}), http.StatusInternalServerError, common.ErrCodeServerError, false)
	mustCalls(t, e.tm, "savepoint", "release", "rollback")
	e.tm.failRelease.Store(nil)
	endC1()
	mustSealOK(t, e)
}

// A lost race comes before row 6: a write that failed in a transaction that
// lost a write race answers the conflict, not retryable, and is discarded.
func TestEnd_Row2_LostRaceBeatsNonRetryableWrite(t *testing.T) {
	for name, o := range map[string]*common.Outcome{
		"with a code":    failed(http.StatusUnprocessableEntity, common.ErrCodeValidationFailed),
		"without a code": {Status: http.StatusRequestEntityTooLarge},
		"panicked":       {Panicked: true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			_, endC1 := e.callout(e.ownerCtx, "c1")
			a, _ := e.mustEnter(Write, pair("c1", 1))
			e.tm.lost.Store(true)

			ans := a.End(o)
			mustAnswer(t, ans, http.StatusConflict, common.ErrCodeConflict, false)
			if !errors.Is(ans, spi.ErrTxAborted) {
				t.Errorf("lost-race answer does not carry spi.ErrTxAborted: %v", ans)
			}
			mustCalls(t, e.tm, "savepoint", "rollback")
			endC1()
		})
	}
}

// A scope whose discard fails did not end as its row answers: the request
// gets a 500, never an answer that invites a retry, and the transaction is
// broken, so later requests are refused 404 and Seal fails with the cause.
func TestEnd_DiscardFails500AndBreaks(t *testing.T) {
	retryable := failed(http.StatusServiceUnavailable, common.ErrCodeStorageUnavailable)
	retryable.Retryable = true
	for _, tc := range []struct {
		name      string
		class     RequestClass
		outcome   *common.Outcome
		supersede bool
		lost      bool
	}{
		{"row 1, superseded", Write, &common.Outcome{}, true, false},
		{"row 2, lost race", Write, failed(http.StatusBadRequest, common.ErrCodeBadRequest), false, true},
		{"row 5, retryable", Write, retryable, false, false},
		{"row 6, non-retryable write", Write, failed(http.StatusUnprocessableEntity, common.ErrCodeValidationFailed), false, false},
		{"row 8, read 5xx", Read, failed(http.StatusInternalServerError, common.ErrCodeServerError), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			_, endC1 := e.callout(e.ownerCtx, "c1")
			a, actx := e.mustEnter(tc.class, pair("c1", 1))
			if tc.lost {
				e.tm.lost.Store(true)
			}
			rbErr := errors.New("the connection is gone")
			e.tm.failRollback.Store(&rbErr)

			var advanced <-chan struct{}
			if tc.supersede {
				advanced = async(func() { e.c.Advance("c1", 2) })
				mustSuperseded(t, actx)
			}
			ans := a.End(tc.outcome)
			mustAnswer(t, ans, http.StatusInternalServerError, common.ErrCodeServerError, false)
			if !errors.Is(ans, rbErr) {
				t.Errorf("the 500 does not carry the discard's cause: %v", ans)
			}
			if advanced != nil {
				mustFinish(t, "Advance", advanced)
			}

			_, _, err := e.enter(Read, pair("c1", 1))
			if tc.supersede {
				_, _, err = e.enter(Read, pair("c1", 2))
			}
			mustCode(t, err, http.StatusNotFound, common.ErrCodeTransactionNotFound, false)

			endC1()
			err = e.seal()
			if tc.lost {
				// A lost race is reported before the broken cause (see Check),
				// as the retryable conflict the owner is retried on.
				if !errors.Is(err, spi.ErrTxAborted) {
					t.Fatalf("Seal = %v; want the lost race (spi.ErrTxAborted)", err)
				}
			} else if !errors.Is(err, rbErr) {
				t.Fatalf("Seal = %v; want the discard's cause", err)
			}
		})
	}
}
