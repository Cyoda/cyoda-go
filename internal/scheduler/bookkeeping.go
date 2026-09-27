package scheduler

import (
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

// BookkeepingKind is the fenced write that records a run's outcome.
type BookkeepingKind int

const (
	// NoneKind: the run committed its own outcome, or was superseded.
	NoneKind BookkeepingKind = iota
	// RecordAttemptKind: the task goes back to WAITING.
	RecordAttemptKind
	// FailKind: the task becomes FAILED, with the audit event (§5.7).
	FailKind
)

// Bookkeeping is the write decideBookkeeping chose.
type Bookkeeping struct {
	Kind    BookkeepingKind
	Attempt spi.Attempt
	Failure spi.Failure
}

// decideBookkeeping chooses how a run's outcome is recorded. The first
// matching row applies: a panic; a committed or superseded run; the engine's
// own FAILED decision; a partial commit; unsafe work that reached a compute
// node; a cut by the shutdown drain; then a counted attempt, which fails the
// task instead when the deadline has passed. errText is already sanitised.
func decideBookkeeping(r workflow.RunReport, task spi.ScheduledTask, cutByShutdown, panicked bool,
	nowMs int64, cfg Config, errText string) Bookkeeping {
	fail := func(reason spi.ScheduledTaskFailureReason) Bookkeeping {
		return Bookkeeping{Kind: FailKind, Failure: spi.Failure{Reason: reason, Error: errText, AtMs: nowMs}}
	}
	switch {
	case panicked:
		return fail(spi.FailureRunPanicked)
	case r.Outcome != workflow.OutcomeFailed:
		return Bookkeeping{Kind: NoneKind}
	case r.FailReason != "":
		return fail(r.FailReason)
	case r.PartialCommit:
		return fail(spi.FailureStoppedAfterPartialCommit)
	case r.UnsafeReached:
		return fail(spi.FailureUnsafeWorkNotCompleted)
	case cutByShutdown:
		return Bookkeeping{Kind: RecordAttemptKind, Attempt: spi.Attempt{
			Error: errText, AtMs: nowMs, NextAttemptTime: nowMs, NotCounted: true, ClearOwnMark: true}}
	}
	next := nowMs + retryDelay(task.Attempts+1, cfg.RetryDelay, cfg.RetryDelayMax).Milliseconds()
	if task.TimeoutMs != nil {
		deadline := task.ScheduledTime + *task.TimeoutMs
		if nowMs > deadline {
			return fail(spi.FailureExpiredAfterFailedAttempts)
		}
		next = min(next, deadline)
	}
	return Bookkeeping{Kind: RecordAttemptKind, Attempt: spi.Attempt{
		Error: errText, AtMs: nowMs, NextAttemptTime: next, ClearOwnMark: r.MarkHeld || r.MarkErrored}}
}

// retryDelay is base × 2^(attempts−1), saturating at max without overflow.
func retryDelay(attempts int, base, max time.Duration) time.Duration {
	d := base
	for i := 1; i < attempts; i++ {
		if d >= max/2 {
			return max
		}
		d *= 2
	}
	return min(d, max)
}
