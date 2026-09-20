package contract

import (
	"errors"
	"fmt"
)

// ErrCalloutDeadline is the cause an owner gives the context it derives for a
// callout's hard limit on time (context.WithDeadlineCause). The local procedure
// reads it with context.Cause to tell "the callout ran out of time" — the try
// in progress is classified and no further try starts — from "the caller went
// away", which is returned as ctx.Err(), unchanged.
var ErrCalloutDeadline = errors.New("callout deadline passed")

// CalloutFailureKind says what happened to a try that produced no result. The
// one question it answers is whether another cnode may be given the work.
type CalloutFailureKind int

const (
	// NoHandOff: the work provably never left this pnode.
	NoHandOff CalloutFailureKind = iota
	// NoAnswer: the work was handed to a cnode, then silence or a dropped
	// connection. The work may have run.
	NoAnswer
	// MemberFailed: the cnode answered success=false.
	MemberFailed
	// Terminal: the try would fail identically on any cnode.
	Terminal
)

// String is the word the hand-over answer uses for the kind.
func (k CalloutFailureKind) String() string {
	switch k {
	case NoHandOff:
		return "no_handoff"
	case NoAnswer:
		return "no_answer"
	case MemberFailed:
		return "member_failed"
	case Terminal:
		return "terminal"
	default:
		return fmt.Sprintf("CalloutFailureKind(%d)", int(k))
	}
}

// MayTryAnother reports whether another cnode may be tried after a failure of
// this kind. repeatSafe is true for every criterion, every function, and a
// processor declared idempotent.
func (k CalloutFailureKind) MayTryAnother(repeatSafe bool) bool {
	switch k {
	case NoHandOff:
		return true
	case NoAnswer:
		return repeatSafe
	default:
		return false
	}
}

// CalloutAttempt records one failed try, for the message a client sees when
// every try is used up.
type CalloutAttempt struct {
	// MemberID is the cnode tried, or "-" for a hand-over whose answer was lost.
	MemberID string
	Kind     CalloutFailureKind
	// Cause is client-safe text.
	Cause string
}

// CalloutFailure reports why a callout produced no result.
//
// It is an error, and it carries the classified error the try produced in Err,
// so that errors.As still finds an *AppError, and errors.Is still finds
// ErrNoMatchingMember or ErrAuthContextUnavailable, behind any wrapping the
// workflow engine adds. A MemberFailed failure carries none: what the client
// sees for it is decided where workflow errors are classified.
type CalloutFailure struct {
	Kind CalloutFailureKind
	// Code is the error code for the client; empty for MemberFailed and for a
	// Terminal failure that has no code of its own.
	Code string
	// Message is client-safe text; for MemberFailed, the cnode's own.
	Message string
	// Retryable is the cnode's verdict (MemberFailed only); nil if it gave none.
	Retryable *bool
	// Attempts is every failed try of the callout. The local procedure leaves
	// it nil and reports its tries beside the failure; the owner fills it on
	// the failure it finally returns.
	Attempts []CalloutAttempt
	// Err is the classified error of the try; nil for MemberFailed.
	Err error
}

func (f *CalloutFailure) Error() string {
	if f.Err != nil {
		return f.Err.Error()
	}
	return f.Message
}

func (f *CalloutFailure) Unwrap() error { return f.Err }
