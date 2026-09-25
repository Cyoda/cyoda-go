package scheduler

import (
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

const testNowMs = int64(10_000_000)

func bookkeepingTask(attempts int, timeoutMs *int64) spi.ScheduledTask {
	return spi.ScheduledTask{ID: "task-1", TenantID: "t1", ScheduledTime: testNowMs - 60_000, TimeoutMs: timeoutMs, Attempts: attempts}
}

func millis(v int64) *int64 { return &v }

func failedReport(mod func(*workflow.RunReport)) workflow.RunReport {
	r := workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: errors.New("boom")}
	if mod != nil {
		mod(&r)
	}
	return r
}

func TestDecideBookkeeping(t *testing.T) {
	cfg := Config{RetryDelay: 30 * time.Second, RetryDelayMax: 15 * time.Minute}
	const text = "CODE: detail"
	fail := func(reason spi.ScheduledTaskFailureReason) Bookkeeping {
		return Bookkeeping{Kind: FailKind, Failure: spi.Failure{Reason: reason, Error: text, AtMs: testNowMs}}
	}
	attempt := func(next int64, clearMark bool) Bookkeeping {
		return Bookkeeping{Kind: RecordAttemptKind, Attempt: spi.Attempt{Error: text, AtMs: testNowMs, NextAttemptTime: next, ClearOwnMark: clearMark}}
	}
	uncounted := Bookkeeping{Kind: RecordAttemptKind, Attempt: spi.Attempt{Error: text, AtMs: testNowMs, NextAttemptTime: testNowMs, NotCounted: true, ClearOwnMark: true}}
	markHeld := func(r *workflow.RunReport) { r.MarkHeld = true }
	handedOff := func(r *workflow.RunReport) { r.MarkHeld, r.UnsafeReached = true, true }

	tests := []struct {
		name     string
		report   workflow.RunReport
		task     spi.ScheduledTask
		cut      bool
		panicked bool
		want     Bookkeeping
	}{
		{name: "a panicked run", task: bookkeepingTask(0, nil), panicked: true, want: fail(spi.FailureRunPanicked)},
		{name: "fired", report: workflow.RunReport{Outcome: workflow.OutcomeFired}, task: bookkeepingTask(0, nil)},
		{name: "declined", report: workflow.RunReport{Outcome: workflow.OutcomeDeclined}, task: bookkeepingTask(0, nil)},
		{name: "expired", report: workflow.RunReport{Outcome: workflow.OutcomeExpired}, task: bookkeepingTask(0, nil)},
		{name: "cancelled", report: workflow.RunReport{Outcome: workflow.OutcomeCancelled}, task: bookkeepingTask(0, nil)},
		{name: "superseded", report: workflow.RunReport{Outcome: workflow.OutcomeSuperseded}, task: bookkeepingTask(0, nil)},
		{name: "owner lost too often, decided by the engine",
			report: failedReport(func(r *workflow.RunReport) { r.FailReason = spi.FailureOwnerLostRepeatedly }),
			task:   bookkeepingTask(0, nil), want: fail(spi.FailureOwnerLostRepeatedly)},
		{name: "marked by another claim, decided by the engine",
			report: failedReport(func(r *workflow.RunReport) { r.FailReason = spi.FailureUnsafeWorkNotCompleted }),
			task:   bookkeepingTask(0, nil), want: fail(spi.FailureUnsafeWorkNotCompleted)},
		{name: "a partial commit comes before the mark",
			report: failedReport(func(r *workflow.RunReport) { handedOff(r); r.PartialCommit = true }),
			task:   bookkeepingTask(0, nil), want: fail(spi.FailureStoppedAfterPartialCommit)},
		{name: "unsafe work reached a compute node", report: failedReport(handedOff),
			task: bookkeepingTask(0, nil), want: fail(spi.FailureUnsafeWorkNotCompleted)},
		{name: "cut by shutdown after unsafe work was handed off", report: failedReport(handedOff),
			task: bookkeepingTask(0, nil), cut: true, want: fail(spi.FailureUnsafeWorkNotCompleted)},
		{name: "cut by shutdown, nothing handed off", report: failedReport(markHeld),
			task: bookkeepingTask(0, nil), cut: true, want: uncounted},
		{name: "cut by shutdown after the deadline is uncounted, not FAILED", report: failedReport(nil),
			task: bookkeepingTask(0, millis(1_000)), cut: true, want: uncounted},
		{name: "mark held, nothing handed off", report: failedReport(markHeld),
			task: bookkeepingTask(0, nil), want: attempt(testNowMs+30_000, true)},
		{name: "MarkUnsafe failed with a non-refusal error",
			report: failedReport(func(r *workflow.RunReport) { r.MarkErrored = true }),
			task:   bookkeepingTask(0, nil), want: attempt(testNowMs+30_000, true)},
		{name: "a plain safe failure", report: failedReport(nil),
			task: bookkeepingTask(0, nil), want: attempt(testNowMs+30_000, false)},
		{name: "the second failure doubles the delay", report: failedReport(nil),
			task: bookkeepingTask(1, nil), want: attempt(testNowMs+60_000, false)},
		{name: "the third failure doubles it again", report: failedReport(nil),
			task: bookkeepingTask(2, nil), want: attempt(testNowMs+120_000, false)},
		{name: "the delay saturates at the maximum", report: failedReport(nil),
			task: bookkeepingTask(9, nil), want: attempt(testNowMs+900_000, false)},
		{name: "the delay never overflows", report: failedReport(nil),
			task: bookkeepingTask(10_000, nil), want: attempt(testNowMs+900_000, false)},
		{name: "the next attempt is clamped to the deadline", report: failedReport(nil),
			task: bookkeepingTask(0, millis(70_000)), want: attempt(testNowMs+10_000, false)},
		{name: "at the deadline the attempt is still recorded", report: failedReport(nil),
			task: bookkeepingTask(0, millis(60_000)), want: attempt(testNowMs, false)},
		{name: "a counted attempt past the deadline fails the task", report: failedReport(nil),
			task: bookkeepingTask(0, millis(59_999)), want: fail(spi.FailureExpiredAfterFailedAttempts)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideBookkeeping(tt.report, tt.task, tt.cut, tt.panicked, testNowMs, cfg, text)
			if got != tt.want {
				t.Errorf("decideBookkeeping\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}
