package common

import "net/http"

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
