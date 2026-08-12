package common

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"
)

// reqTimeoutKey marks a context whose deadline was attached from a
// client-supplied transactionTimeoutMillis / timeoutMillis param. The marker is
// what lets ClassifyRequestTimeout distinguish OUR deadline from every other
// deadline source (postgres statement_timeout stays 500, dispatch time.After
// stays 503) — spec D8.
type reqTimeoutKey struct{}

const maxRequestTimeoutMillis = math.MaxInt64 / int64(time.Millisecond)

// ValidateRequestTimeoutMillis returns a 400 AppError unless 1 <= millis and
// time.Duration(millis)*time.Millisecond does not overflow.
func ValidateRequestTimeoutMillis(millis int64) *AppError {
	if millis < 1 || millis > maxRequestTimeoutMillis {
		return Operational(http.StatusBadRequest, ErrCodeBadRequest,
			fmt.Sprintf("timeout must be a positive number of milliseconds not exceeding %d", maxRequestTimeoutMillis))
	}
	return nil
}

// WithRequestTimeout attaches a feature-owned deadline. Caller must have validated millis.
func WithRequestTimeout(ctx context.Context, millis int64) (context.Context, context.CancelFunc) {
	ctx = context.WithValue(ctx, reqTimeoutKey{}, true)
	return context.WithTimeout(ctx, time.Duration(millis)*time.Millisecond)
}

// HasRequestTimeout reports whether ctx carries a feature-attached deadline
// marker. The stored value must be exactly true — CommitContext clears the
// marker by re-stamping the key with nil (context.WithValue provides no way
// to delete a key), so a present-but-nil value must read as absent.
func HasRequestTimeout(ctx context.Context) bool {
	v, _ := ctx.Value(reqTimeoutKey{}).(bool)
	return v
}

// ClassifyRequestTimeout maps err to Operational(408, code).AsRetryable() only
// when the feature's own deadline is in err's chain (spec D2 pinned rule):
// the marker must be on ctx AND the chain — unwrapping *AppError causes —
// must contain context.DeadlineExceeded. context.Canceled never matches.
// Never inspects ctx.Err() state alone.
func ClassifyRequestTimeout(ctx context.Context, err error, code string) *AppError {
	if err == nil || !HasRequestTimeout(ctx) {
		return nil
	}
	if !chainHasDeadlineExceeded(err) {
		return nil
	}
	return Operational(http.StatusRequestTimeout, code,
		"operation exceeded the client-requested timeout before completing; nothing was committed").
		AsRetryable()
}

func chainHasDeadlineExceeded(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var appErr *AppError
	if errors.As(err, &appErr) && appErr.Err != nil {
		return errors.Is(appErr.Err, context.DeadlineExceeded)
	}
	return false
}
