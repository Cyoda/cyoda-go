package common

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrRollbackOnly is the cause every TRANSACTION_ROLLBACK_ONLY error carries.
var ErrRollbackOnly = errors.New("transaction is rollback-only")

// TransactionRollbackOnly is the owner's refusal: a request that joined its
// transaction failed, so the transaction can only be rolled back. 409, not
// retryable. failedCode is the code the failed callback got.
func TransactionRollbackOnly(failedCode string) *AppError {
	return Operational(http.StatusConflict, ErrCodeTransactionRollbackOnly,
		fmt.Sprintf("a request that joined the transaction failed with %s, so the transaction can only be rolled back", failedCode)).
		WithCause(ErrRollbackOnly)
}

// TransactionRollbackOnlyJoined refuses a joined request under a marked scope.
// 409, not retryable.
func TransactionRollbackOnlyJoined() *AppError {
	return Operational(http.StatusConflict, ErrCodeTransactionRollbackOnly,
		"the transaction, or the part of it this request would run in, can only be rolled back").
		WithCause(ErrRollbackOnly)
}

// TransactionBusy refuses a sibling of a paused callback. 503, retryable.
func TransactionBusy() *AppError {
	return Operational(http.StatusServiceUnavailable, ErrCodeTransactionBusy,
		"another callback of this transaction is waiting for a callout of its own — send this callback again once that one has been answered").
		AsRetryable()
}

// TooManyJoinedRequests is what a compute member is told when its transaction
// already has as many callbacks queued as it may have. Retryable, the capacity
// idiom of this codebase: the queue drains as the callbacks ahead of it finish.
// The figure is not in the message — which bound was met is operator
// information, and the help topic names the setting.
func TooManyJoinedRequests() *AppError {
	return Operational(http.StatusServiceUnavailable, ErrCodeTooManyJoinedRequests,
		"too many requests of this transaction are already waiting — retry later").AsRetryable()
}
