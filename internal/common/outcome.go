package common

import (
	"context"
	"sync"
)

// Outcome is what a request was answered with, kept for the layer that has to
// know before it lets go of a transaction. The shared error funnels fill it
// (WriteError for the HTTP door, the envelope builder for the gRPC door).
//
// RecordOutcome writes under an internal lock, so concurrent recorders are
// safe and the first failure wins. The fields are plain: read them only after
// the handler that was handed the context has returned and every goroutine it
// started has finished — the join layer reads after the handler returns.
type Outcome struct {
	Status    int // 0: nothing recorded
	Code      string
	Retryable bool
	Err       error // the answered error, cause chain intact
	Panicked  bool

	mu sync.Mutex
}

type outcomeKey struct{}

// WithOutcome installs a holder on ctx.
func WithOutcome(ctx context.Context) (context.Context, *Outcome) {
	o := &Outcome{}
	return context.WithValue(ctx, outcomeKey{}, o), o
}

// RecordOutcome records appErr on ctx's holder. It is a no-op without one, and
// the first failure recorded wins: a later answer on the same request does not
// replace the failure that decided it.
func RecordOutcome(ctx context.Context, appErr *AppError) {
	o, ok := ctx.Value(outcomeKey{}).(*Outcome)
	if !ok || appErr == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.Status != 0 {
		return
	}
	o.Status = appErr.Status
	o.Code = appErr.Code
	o.Retryable = appErr.Retryable
	o.Err = appErr
}

// FailureRecorded reports whether a failure has been recorded on ctx's holder
// so far. Unlike the fields, it may be read while the handler runs: a door
// that holds a streamed answer asks it frame by frame, to tell the frame that
// carries the failure from the frames before it. False without a holder.
func FailureRecorded(ctx context.Context) bool {
	o, ok := ctx.Value(outcomeKey{}).(*Outcome)
	if !ok {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.Status != 0
}
