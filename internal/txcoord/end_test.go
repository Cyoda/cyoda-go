package txcoord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
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

// mustSealRollbackOnly asserts that Seal refuses with TRANSACTION_ROLLBACK_ONLY
// naming the code the failed callback got.
func mustSealRollbackOnly(t *testing.T, e *env, failedCode string) {
	t.Helper()
	err := e.seal()
	mustCode(t, err, http.StatusConflict, common.ErrCodeTransactionRollbackOnly, false)
	if !errors.Is(err, common.ErrRollbackOnly) {
		t.Errorf("Seal error does not carry ErrRollbackOnly: %v", err)
	}
	if !strings.Contains(err.Error(), failedCode) {
		t.Errorf("Seal error %q does not name the failed code %s", err.Error(), failedCode)
	}
}

// markedByChild leaves scope a (on actx) marked: a child callback of a nested
// callout fails a write. It returns a's resume error.
func markedByChild(t *testing.T, e *env, actx context.Context) error {
	t.Helper()
	_, endX1 := e.callout(actx, "x1")
	resume := Pause(actx)
	d, _ := e.mustEnter(Write, pair("x1", 1), pair("c1", 1))
	mustOwnAnswer(t, d.End(failed(http.StatusUnprocessableEntity, common.ErrCodeValidationFailed)))
	endX1()
	return resume()
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

func TestEnd_Row1_SupersededWithMarkedDescendantNoClimb(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Write, pair("c1", 1))
	if err := markedByChild(t, e, actx); !errors.Is(err, common.ErrRollbackOnly) {
		t.Fatalf("resume of the marked scope = %v; want rollback-only", err)
	}

	advanced := async(func() { e.c.Advance("c1", 2) })
	mustSuperseded(t, actx)
	mustStillBlock(t, "Advance", advanced)
	mustAnswer(t, a.End(failed(http.StatusConflict, common.ErrCodeTransactionRollbackOnly)),
		http.StatusGone, common.ErrCodeCalloutSuperseded, false)
	mustFinish(t, "Advance", advanced)

	if e.rootMarked() {
		t.Fatal("a superseded scope passed its mark up to the root")
	}
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
	if e.rootMarked() {
		t.Fatal("a lost race marked the root")
	}
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

func TestEnd_Row4_MarkedScope409AndClimbsForWrite(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Write, pair("c1", 1))
	_ = markedByChild(t, e, actx)

	// The handler's own success does not outrank the mark.
	mustAnswer(t, a.End(&common.Outcome{}), http.StatusConflict, common.ErrCodeTransactionRollbackOnly, false)
	mustCalls(t, e.tm, "savepoint", "savepoint", "rollback", "rollback")
	if !e.rootMarked() {
		t.Fatal("a marked write scope did not mark its parent")
	}
	endC1()
	mustSealRollbackOnly(t, e, common.ErrCodeValidationFailed)
}

func TestEnd_Row4_ReadDoesNotClimb(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Read, pair("c1", 1))
	_ = markedByChild(t, e, actx)

	mustAnswer(t, a.End(&common.Outcome{}), http.StatusConflict, common.ErrCodeTransactionRollbackOnly, false)
	mustCalls(t, e.tm, "savepoint", "savepoint", "rollback", "rollback")
	if e.rootMarked() {
		t.Fatal("a marked read scope marked its parent")
	}
	endC1()
	mustSealOK(t, e)
}

func TestEnd_Row5_SuccessReleases(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))

	mustOwnAnswer(t, a.End(&common.Outcome{}))
	mustCalls(t, e.tm, "savepoint", "release")
	endC1()
	mustSealOK(t, e)
}

func TestEnd_Row5_ReleaseFails500AndMarks(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))
	relErr := errors.New("current transaction is aborted (25P02)")
	e.tm.failRelease.Store(&relErr)

	mustAnswer(t, a.End(&common.Outcome{}), http.StatusInternalServerError, common.ErrCodeServerError, false)
	mustCalls(t, e.tm, "savepoint", "release", "rollback")
	if !e.rootMarked() {
		t.Fatal("a write whose completion failed did not mark its parent")
	}
	e.tm.failRelease.Store(nil)
	endC1()
	mustSealRollbackOnly(t, e, common.ErrCodeServerError)
}

func TestEnd_Row6_RetryableDiscardsNoMark(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))

	o := failed(http.StatusConflict, common.ErrCodeConflict)
	o.Retryable = true
	mustOwnAnswer(t, a.End(o))
	mustCalls(t, e.tm, "savepoint", "rollback")
	if e.rootMarked() {
		t.Fatal("a retryable failure marked the root")
	}
	endC1()
	mustSealOK(t, e)
}

func TestEnd_Row7_NonRetryableWriteMarksParent(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))

	mustOwnAnswer(t, a.End(failed(http.StatusUnprocessableEntity, common.ErrCodeValidationFailed)))
	mustCalls(t, e.tm, "savepoint", "rollback")
	endC1()
	mustSealRollbackOnly(t, e, common.ErrCodeValidationFailed)
}

func TestEnd_Row8_Read4xxKeeps(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Read, pair("c1", 1))

	mustOwnAnswer(t, a.End(failed(http.StatusNotFound, common.ErrCodeEntityNotFound)))
	mustCalls(t, e.tm, "savepoint", "release")
	endC1()
	mustSealOK(t, e)
}

func TestEnd_Row9_Read5xxDiscards(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Read, pair("c1", 1))

	mustOwnAnswer(t, a.End(failed(http.StatusInternalServerError, common.ErrCodeServerError)))
	mustCalls(t, e.tm, "savepoint", "rollback")
	if e.rootMarked() {
		t.Fatal("a failed read marked the root")
	}
	endC1()
	mustSealOK(t, e)
}

func TestEnd_Outside_NeverRewrittenNeverMarks(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Outside, pair("c1", 1))

	advanced := async(func() { e.c.Advance("c1", 2) })
	mustSuperseded(t, actx)
	mustStillBlock(t, "Advance", advanced)
	mustOwnAnswer(t, a.End(failed(http.StatusBadRequest, common.ErrCodeBadRequest)))
	mustFinish(t, "Advance", advanced)

	mustCalls(t, e.tm)
	if e.rootMarked() {
		t.Fatal("a request outside the transaction marked the root")
	}
	endC1()
	mustSealOK(t, e)
}

func TestEnd_PanicOutcome_DiscardsAndMarks(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))

	mustOwnAnswer(t, a.End(&common.Outcome{Panicked: true}))
	mustCalls(t, e.tm, "savepoint", "rollback")
	endC1()
	mustSealRollbackOnly(t, e, common.ErrCodeServerError)
}

// A backstop answer (a buffered HTTP status, a raw gRPC error) may carry no
// code. A non-retryable write failure marks the parent all the same, and the
// owner's refusal names the status.
func TestEnd_Row7_EmptyCodeStillMarks(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))

	mustOwnAnswer(t, a.End(&common.Outcome{Status: http.StatusRequestEntityTooLarge}))
	mustCalls(t, e.tm, "savepoint", "rollback")
	if !e.rootMarked() {
		t.Fatal("a non-retryable write failure without a code did not mark the parent")
	}
	endC1()
	mustSealRollbackOnly(t, e, "413")
}

// A completion that fails is judged again from row 1: a lost race answers by
// row 2, not with the 500.
func TestEnd_Row5_ReleaseFailsLostRaceAnswersRow2(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Write, pair("c1", 1))
	relErr := errors.New("release refused")
	e.tm.failRelease.Store(&relErr)
	e.tm.lost.Store(true)

	mustAnswer(t, a.End(&common.Outcome{}), http.StatusConflict, common.ErrCodeConflict, false)
	mustCalls(t, e.tm, "savepoint", "release", "rollback")
	if e.rootMarked() {
		t.Fatal("a lost race marked the root")
	}
	e.tm.failRelease.Store(nil)
	endC1()
}

// A read whose completion fails is a 5xx read: discarded, no mark (row 9).
func TestEnd_Row5_ReadReleaseFailsAnswersRow9(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, _ := e.mustEnter(Read, pair("c1", 1))
	relErr := errors.New("release refused")
	e.tm.failRelease.Store(&relErr)

	mustAnswer(t, a.End(&common.Outcome{}), http.StatusInternalServerError, common.ErrCodeServerError, false)
	mustCalls(t, e.tm, "savepoint", "release", "rollback")
	if e.rootMarked() {
		t.Fatal("a read whose completion failed marked the root")
	}
	e.tm.failRelease.Store(nil)
	endC1()
	mustSealOK(t, e)
}

// A lost race outranks a mark: the failed request of a marked scope in a
// transaction that lost answers by row 2, and passes no mark up.
func TestEnd_Row2_OutranksRow4(t *testing.T) {
	e := newEnv(t)
	_, endC1 := e.callout(e.ownerCtx, "c1")
	a, actx := e.mustEnter(Write, pair("c1", 1))
	_ = markedByChild(t, e, actx)
	e.tm.lost.Store(true)

	mustAnswer(t, a.End(failed(http.StatusConflict, common.ErrCodeTransactionRollbackOnly)),
		http.StatusConflict, common.ErrCodeConflict, false)
	if e.rootMarked() {
		t.Fatal("a scope answered by row 2 passed its mark up")
	}
	endC1()
}
