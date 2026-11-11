package txcoord

import (
	"errors"
	"fmt"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
)

// failure is a request that did not succeed, as the table of how a scope ends
// reads it.
type failure struct {
	status    int
	code      string
	retryable bool
	err       error
	// completion is set when the handler succeeded and the completion step
	// itself (ReleaseSavepoint) failed: the request is then answered 500
	// SERVER_ERROR with this as the cause.
	completion error
}

// failureOf reads the request's outcome: nil for a success.
func failureOf(o *common.Outcome) *failure {
	switch {
	case o == nil:
		return nil
	case o.Panicked:
		return &failure{status: http.StatusInternalServerError, code: common.ErrCodeServerError, err: o.Err}
	case o.Status >= http.StatusBadRequest:
		return &failure{status: o.Status, code: o.Code, retryable: o.Retryable, err: o.Err}
	default:
		return nil
	}
}

// markCode is the code the parent's mark names: the failure's own, or, for an
// answer that carries none (a backstop: a buffered HTTP status, a raw gRPC
// error), SERVER_ERROR for a 5xx and the status otherwise.
func (f *failure) markCode() string {
	switch {
	case f.code != "":
		return f.code
	case f.status >= http.StatusInternalServerError:
		return common.ErrCodeServerError
	default:
		return fmt.Sprintf("HTTP %d", f.status)
	}
}

// parentMark is a mark to set on the parent of an ended scope, with the code
// the owner's refusal names. A nil *parentMark is no mark.
type parentMark struct{ code string }

// answer is what the request is told for a failure: the handler's own answer
// (nil), except for a failed completion, which the handler did not answer.
func (f *failure) answer() *common.AppError {
	if f.completion == nil {
		return nil
	}
	return common.InternalWithCode(common.ErrCodeServerError,
		"the request's work could not be completed in the transaction", f.completion)
}

// ending is one implementation's part in ending a callback scope by the table
// of how a scope ends; decide holds the table itself.
type ending interface {
	// standing reads rows 1 and 4: superseded is the cause of the 410 when a
	// pass of the scope is no longer current (nil otherwise); marked reports a
	// mark on the scope or an ancestor, with its code when it is known.
	standing() (superseded error, code string, marked bool)
	// lostRace asks the transaction manager whether the transaction lost a
	// write race.
	lostRace() bool
	// complete is row 5's completion step. A completion that fails is judged
	// again from row 1, where the implementation's refusal does not decide
	// the answer itself.
	complete(class RequestClass) (*common.AppError, *parentMark, error)
	// discard ends the scope discarded, passing mark to the parent when it is
	// not nil (an implementation may leave that to its caller). It returns
	// the cause that leaves the transaction broken when the discard fails.
	discard(mark *parentMark) error
}

// decide decides a callback scope's end by the first matching row, and
// completes or discards the scope. It returns the rewritten answer (nil: the
// handler's own), the mark to set on the parent (nil for none), and the cause
// that leaves the transaction broken (nil when it is not).
func decide(e ending, class RequestClass, f *failure) (*common.AppError, *parentMark, error) {
	superseded, code, marked := e.standing()
	// Row 1: its pass is no longer current. No mark is passed up: the owner
	// has moved on.
	if superseded != nil {
		return fence.NewSupersededError().WithCause(superseded), nil, e.discard(nil)
	}
	// Row 2: the transaction lost a write race and the request failed.
	if f != nil && e.lostRace() {
		return lostRaceAnswer(), nil, e.discard(nil)
	}
	// Row 3: the transaction's session has ended. The scope is discarded if
	// that is still possible; the transaction is broken either way.
	if f != nil && errors.Is(f.err, spi.ErrTxNotFound) {
		_ = e.discard(nil)
		return transactionNotFound(f.err), nil, sessionEnded(f.err)
	}
	// Row 4: the scope is marked; a write passes the mark up.
	if marked {
		var mark *parentMark
		if class == Write {
			mark = &parentMark{code: code}
		}
		return common.TransactionRollbackOnlyJoined(), mark, e.discard(mark)
	}
	switch {
	case f == nil:
		// Row 5: success, if the completion succeeds; otherwise judged again
		// as a failed request.
		return e.complete(class)
	case f.retryable:
		// Row 6.
		return f.answer(), nil, e.discard(nil)
	case class == Write:
		// Row 7.
		mark := &parentMark{code: f.markCode()}
		return f.answer(), mark, e.discard(mark)
	case f.status < http.StatusInternalServerError:
		// Row 8: a read that answered keeps its read set, so the commit still
		// validates what the compute node was told.
		return e.complete(class)
	default:
		// Row 9.
		return f.answer(), nil, e.discard(nil)
	}
}

// completionFailure is a request whose handler succeeded and whose completion
// step failed with err: it is answered 500 SERVER_ERROR.
func completionFailure(err error) *failure {
	return &failure{
		status:     http.StatusInternalServerError,
		code:       common.ErrCodeServerError,
		err:        err,
		completion: err,
	}
}

// End implements Scope for a callback scope: it applies the table of how a
// scope ends, then takes the scope out of the tree and releases the gate. The
// savepoint is released or rolled back while the gate is still held.
func (s *scope) End(o *common.Outcome) *common.AppError {
	var answer *common.AppError
	var mark *parentMark
	var broken error
	if s.class != Outside {
		answer, mark, broken = decide(s, s.class, failureOf(o))
	}
	s.c.leave(s, mark, broken)
	s.releaseGate()
	return answer
}

// standing implements ending. The gate serialises the scope's descendants, so
// nothing marks the scope while it ends.
func (s *scope) standing() (error, string, bool) {
	if err := fence.Check(s.ctx); err != nil {
		return err, "", false
	}
	code, marked := s.markedCode()
	return nil, code, marked
}

// lostRace implements ending.
func (s *scope) lostRace() bool { return s.c.lostRace(s.ctx, s.tx.id) }

// complete implements ending: it releases the savepoint. A release that fails
// is a failed request, judged again from row 1.
func (s *scope) complete(class RequestClass) (*common.AppError, *parentMark, error) {
	err := s.c.txMgr.ReleaseSavepoint(s.ctx, s.tx.id, s.savepoint)
	if err == nil {
		return nil, nil, nil
	}
	return decide(s, class, completionFailure(err))
}

// discard implements ending: it rolls back to the scope's savepoint. The mark
// is set by leave, under the coordination's lock.
func (s *scope) discard(*parentMark) error {
	if err := s.c.txMgr.RollbackToSavepoint(s.ctx, s.tx.id, s.savepoint); err != nil {
		return fmt.Errorf("failed to roll back a scope of the transaction: %w", err)
	}
	return nil
}

// Close implements Scope for a processor scope.
func (s *scope) Close(procErr error) error {
	superseded := fence.Check(s.ctx)
	code, marked := s.markedCode()
	var result, broken error
	if procErr == nil && superseded == nil && !marked {
		if err := s.c.txMgr.ReleaseSavepoint(s.ctx, s.tx.id, s.savepoint); err != nil {
			result = fmt.Errorf("failed to release a processor scope: %w", errors.Join(ErrScopeInfra, err))
			broken = s.discard(nil)
		}
	} else {
		broken = s.discard(nil)
		switch {
		case broken != nil:
			result = errors.Join(ErrScopeInfra, broken)
		case procErr != nil:
			result = procErr
		case superseded != nil:
			result = superseded
		default:
			result = s.c.rollbackOnly(s.ctx, s.tx.id, code)
		}
	}
	s.c.leave(s, nil, broken)
	return result
}
