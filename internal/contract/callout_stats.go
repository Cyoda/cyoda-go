package contract

import (
	"context"
	"time"
)

// Outcomes of a try or a hand-over that are not a CalloutFailureKind.
const (
	// CalloutOutcomeOK: a cnode answered.
	CalloutOutcomeOK = "ok"
	// CalloutOutcomeAbandoned: the caller went away, or the callout was
	// released by the fence, while the try or hand-over was in progress.
	CalloutOutcomeAbandoned = "abandoned"
	// CalloutOutcomeUnreachable: a hand-over that gave no cnode the work — the
	// pnode could not be connected to, or answered that it has no cnode. It
	// uses no try.
	CalloutOutcomeUnreachable = "unreachable"
)

// CalloutStats is what the owner's loop reports about one callout to whoever
// asked for it by putting a CalloutStats on the context — the tracing decorator
// around the ExternalProcessingService. The loop fills it before it returns,
// from the goroutine that called it; the decorator reads it afterwards.
type CalloutStats struct {
	// Tries has one outcome per try counted against the callout's number of
	// tries: CalloutOutcomeOK, CalloutOutcomeAbandoned, or a
	// CalloutFailureKind's String().
	Tries []string
	// HandOvers has one outcome per hand-over to another pnode:
	// CalloutOutcomeOK, CalloutOutcomeUnreachable, CalloutOutcomeAbandoned, or
	// a CalloutFailureKind's String().
	HandOvers []string
	// Waited is how long the callout waited for a cnode to exist.
	Waited time.Duration
}

type calloutStatsKey struct{}

// WithCalloutStats returns a context that asks the owner's loop to report on
// the callout made under it, and the value the report lands in.
func WithCalloutStats(ctx context.Context) (context.Context, *CalloutStats) {
	stats := &CalloutStats{}
	return context.WithValue(ctx, calloutStatsKey{}, stats), stats
}

// CalloutStatsFrom returns the CalloutStats asked for on ctx, or nil.
func CalloutStatsFrom(ctx context.Context) *CalloutStats {
	stats, _ := ctx.Value(calloutStatsKey{}).(*CalloutStats)
	return stats
}
