package txcoord

import (
	"errors"
	"fmt"
	"net/http"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go/internal/common"
	"github.com/cyoda/cyoda-go/internal/fence"
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
	// standing reads row 1: superseded is the cause of the 410 when a pass of
	// the scope is no longer current (nil otherwise). failed is set when the
	// standing could not be read: the request then ends as a failed
	// completion.
	standing() (superseded error, failed error)
	// lostRace asks the transaction manager whether the transaction lost a
	// write race.
	lostRace() bool
	// complete is row 4's completion step. A completion that fails is judged
	// again from row 1, where the implementation's refusal does not decide
	// the answer itself.
	complete(class RequestClass) (*common.AppError, error)
	// discard ends the scope discarded. It returns a *discardFailure when the
	// discard fails, which endAnswer answers as a 500; the in-process
	// coordination also records it as the cause that leaves the transaction
	// broken.
	discard() error
}

// decide decides a callback scope's end by the first matching row, and
// completes or discards the scope. It returns the rewritten answer (nil: the
// handler's own) and the infrastructure failure the scope ended with (nil for
// none): a failed discard, or row 3's ended session. endAnswer reads it for
// the request's answer; only the in-process coordination records it, as the
// cause that leaves the transaction broken.
//
// No row touches the scope's parent: a scope is all-or-nothing, so a failed
// request keeps nothing in the transaction, and the transaction goes on. The
// compute node gets the answer and decides what its own work does with it.
func decide(e ending, class RequestClass, f *failure) (*common.AppError, error) {
	superseded, failed := e.standing()
	if failed != nil {
		f = completionFailure(failed)
	}
	// Row 1: its pass is no longer current.
	if superseded != nil {
		return fence.NewSupersededError().WithCause(superseded), e.discard()
	}
	// Row 2: the transaction lost a write race and the request failed.
	if f != nil && e.lostRace() {
		return lostRaceAnswer(), e.discard()
	}
	// Row 3: the transaction's session has ended. The scope is discarded if
	// that is still possible, and the request answers 404 whatever the
	// discard did. The cause that breaks the transaction is the dead session,
	// never the request's own answer: the owner's commit is answered by that
	// cause, not by what another request was told.
	if f != nil && errors.Is(f.err, spi.ErrTxNotFound) {
		_ = e.discard()
		return transactionNotFound(f.err), sessionEnded(underAnswers(f.err))
	}
	switch {
	case f == nil:
		// Row 4: success, if the completion succeeds; otherwise judged again
		// as a failed request.
		return e.complete(class)
	case f.retryable:
		// Row 5.
		return f.answer(), e.discard()
	case class == Write:
		// Row 6: a non-retryable failure of a write.
		return f.answer(), e.discard()
	case f.status < http.StatusInternalServerError:
		// Row 7: a read that answered keeps its read set, so the commit still
		// validates what the compute node was told.
		return e.complete(class)
	default:
		// Row 8.
		return f.answer(), e.discard()
	}
}

// endAnswer is the answer of a scope that decide ended with answer and failed.
// A discard that failed overrides the row's answer with a 500 SERVER_ERROR,
// never retryable: the scope's writes may still be in the transaction, so the
// request did not end as its row answers, and a retry in the same transaction
// cannot succeed.
//
// Only the scope's own discard counts: every row that discards returns the
// discard's result as failed, unwrapped. Row 3's failed wraps the cause of
// the request's error, which may carry the failed discard of another scope (a
// processor scope the handler ran), and row 3 answers 404 whatever its own
// discard did.
func endAnswer(answer *common.AppError, failed error) *common.AppError {
	if _, ok := failed.(*discardFailure); ok {
		return common.InternalWithCode(common.ErrCodeServerError,
			"the request's scope could not be ended in the transaction", failed)
	}
	return answer
}

// discardFailure is a discard of a scope that failed: a rollback to its
// savepoint, or a DiscardScope the backend refused.
type discardFailure struct{ err error }

func (d *discardFailure) Error() string {
	return "failed to discard a scope of the transaction: " + d.err.Error()
}
func (d *discardFailure) Unwrap() error { return d.err }

// underAnswers is the cause of a dead-session error under the answers a
// request's handlers gave it: every *common.AppError on the way to
// spi.ErrTxNotFound is taken off, so what remains matches spi.ErrTxNotFound
// and carries no answer.
func underAnswers(err error) error {
	var appErr *common.AppError
	for errors.As(err, &appErr) && errors.Is(appErr.Err, spi.ErrTxNotFound) {
		err = appErr.Err
	}
	return err
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
	var broken error
	if s.class != Outside {
		answer, broken = decide(s, s.class, failureOf(o))
	}
	s.c.leave(s, broken)
	s.releaseGate()
	return endAnswer(answer, broken)
}

// standing implements ending.
func (s *scope) standing() (error, error) {
	return fence.Check(s.ctx), nil
}

// lostRace implements ending.
func (s *scope) lostRace() bool { return s.c.lostRace(s.ctx, s.tx.id) }

// complete implements ending: it releases the savepoint. A release that fails
// is a failed request, judged again from row 1.
func (s *scope) complete(class RequestClass) (*common.AppError, error) {
	err := s.c.txMgr.ReleaseSavepoint(s.ctx, s.tx.id, s.savepoint)
	if err == nil {
		return nil, nil
	}
	return decide(s, class, completionFailure(err))
}

// discard implements ending: it rolls back to the scope's savepoint.
func (s *scope) discard() error {
	if err := s.c.txMgr.RollbackToSavepoint(s.ctx, s.tx.id, s.savepoint); err != nil {
		return &discardFailure{err: err}
	}
	return nil
}

// Close implements Scope for a processor scope.
func (s *scope) Close(procErr error) error {
	superseded := fence.Check(s.ctx)
	var result, broken error
	if procErr == nil && superseded == nil {
		if err := s.c.txMgr.ReleaseSavepoint(s.ctx, s.tx.id, s.savepoint); err != nil {
			result = fmt.Errorf("failed to release a processor scope: %w", errors.Join(ErrScopeInfra, err))
			broken = s.discard()
		}
	} else {
		broken = s.discard()
		switch {
		case broken != nil:
			result = errors.Join(ErrScopeInfra, broken)
		case procErr != nil:
			result = procErr
		default:
			result = superseded
		}
	}
	s.c.leave(s, broken)
	return result
}
