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

	ticket string // the ticket the recorded failure was logged under, if any
	// held is the recorded failure's log line, kept until the request's scope
	// has ended (LogAnswer); nil once written or folded, or when none is held.
	held *heldLine
	mu   sync.Mutex
}

// heldLine is a log line LogAnswer keeps for the join layer to write.
type heldLine struct {
	appErr *AppError
	ticket string
	attrs  []any
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

// LogAnswer writes the one log line of a failure a door answers, as LogError
// does, and returns the ticket it is logged under — except the failure a
// joined request recorded (ctx carries the holder WithOutcome installed, and
// appErr is its recorded failure). That line is held, under the ticket
// returned, until the request's scope has ended: the join layer then writes it
// (WriteHeldLine), or, when the scope's end answers the request with another
// error, folds it into that error's line (FoldHeldLine). A joined request's
// failure is logged once, as what its client is told, and a ticket that is
// logged is a ticket the client is given.
func LogAnswer(ctx context.Context, appErr *AppError, attrs ...any) string {
	if o, ok := ctx.Value(outcomeKey{}).(*Outcome); ok {
		if ticket, held := o.hold(appErr, attrs); held {
			return ticket
		}
	}
	return LogError(ctx, appErr, attrs...)
}

// hold keeps appErr's line when appErr is the recorded failure and no line is
// held yet.
func (o *Outcome) hold(appErr *AppError, attrs []any) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.Err != error(appErr) || o.held != nil {
		return "", false
	}
	ticket := mintTicket(appErr)
	o.ticket = ticket
	o.held = &heldLine{appErr: appErr, ticket: ticket, attrs: attrs}
	return ticket, true
}

// takeHeld returns the line held on ctx's holder and lets it go; nil when none
// is held.
func takeHeld(ctx context.Context) *heldLine {
	o, ok := ctx.Value(outcomeKey{}).(*Outcome)
	if !ok {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	line := o.held
	o.held = nil
	return line
}

// WriteHeldLine writes the line LogAnswer held on ctx's holder, under the
// ticket the answer carries: the request is answered as its handler answered
// it. No-op when no line is held.
func WriteHeldLine(ctx context.Context) {
	if line := takeHeld(ctx); line != nil {
		logLine(ctx, line.appErr, line.ticket, line.attrs...)
	}
}

// FoldHeldLine folds the line LogAnswer held on ctx's holder into answer, the
// error the request is answered with instead of its handler's: answer's Detail,
// which is logged and never answered, names what the handler answered, and
// answer's line is logged at least at the level the handler's would have been.
// The handler's line is not written: its ticket would name a line no client is
// given. answer must be freshly constructed; it is changed in place. No-op
// when no line is held.
func FoldHeldLine(ctx context.Context, answer *AppError) {
	line := takeHeld(ctx)
	if line == nil {
		return
	}
	handler := "the handler answered " + line.appErr.Message
	if line.appErr.Detail != "" {
		handler += " (" + line.appErr.Detail + ")"
	}
	if answer.Detail == "" {
		answer.Detail = handler
	} else {
		answer.Detail += "; " + handler
	}
	if answer.Level == LevelOperational {
		if level := lineLevel(line.appErr); level > answer.OperationalLogLevel() {
			answer.WithLogLevel(level)
		}
	}
}

// recordedAnswer returns the failure recorded on ctx's holder and the ticket it
// was logged under; nil when none was recorded.
func recordedAnswer(ctx context.Context) (*AppError, string) {
	o, ok := ctx.Value(outcomeKey{}).(*Outcome)
	if !ok {
		return nil, ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	appErr, _ := o.Err.(*AppError)
	if o.Status == 0 || appErr == nil {
		return nil, ""
	}
	return appErr, o.ticket
}

// RecordedFailure returns the failure recorded on ctx's holder so far, nil
// when none was (or ctx carries no holder). Unlike the fields, it may be read
// while the handler runs: a door that holds a streamed answer asks it frame by
// frame, to tell the frame that carries the failure from the frames before it,
// and a door that buffers an answer asks it to send the failure the outcome
// recorded rather than a success status the handler wrote first.
func RecordedFailure(ctx context.Context) *AppError {
	o, ok := ctx.Value(outcomeKey{}).(*Outcome)
	if !ok {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.Status == 0 {
		return nil
	}
	appErr, _ := o.Err.(*AppError)
	return appErr
}
