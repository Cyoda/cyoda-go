package scheduler

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// The outcome attribute of cyoda.scheduler.runs for a run that did not commit.
// A committed run uses its workflow.ScheduledOutcome word.
const (
	outcomeAttemptFailed     = "attempt_failed"
	outcomeFailed            = "failed"
	outcomeSelfCancelled     = "self_cancelled"
	outcomeShutdownCancelled = "shutdown_cancelled"
	outcomePanicked          = "panicked"
	outcomeSuperseded        = "superseded"
)

// The reason attribute of cyoda.scheduler.claims.
const (
	claimDue       = "due"
	claimOwnerLost = "owner_lost"
)

// metrics are the scheduler's instruments. None carries a tenant.
type metrics struct {
	runs               metric.Int64Counter
	runDuration        metric.Float64Histogram
	inProgress         metric.Int64UpDownCounter
	claims             metric.Int64Counter
	heartbeatFailures  metric.Int64Counter
	bookkeepingRetries metric.Int64Counter
}

func newMetrics(meter metric.Meter) (*metrics, error) {
	if meter == nil {
		meter = noop.NewMeterProvider().Meter("")
	}
	m := &metrics{}
	var err error
	if m.runs, err = meter.Int64Counter("cyoda.scheduler.runs",
		metric.WithDescription("Scheduled runs that ended, by outcome")); err != nil {
		return nil, fmt.Errorf("instrument cyoda.scheduler.runs: %w", err)
	}
	if m.runDuration, err = meter.Float64Histogram("cyoda.scheduler.run.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of a scheduled run, from its claim to its recorded outcome")); err != nil {
		return nil, fmt.Errorf("instrument cyoda.scheduler.run.duration: %w", err)
	}
	if m.inProgress, err = meter.Int64UpDownCounter("cyoda.scheduler.runs.in_progress",
		metric.WithDescription("Scheduled runs this node holds now")); err != nil {
		return nil, fmt.Errorf("instrument cyoda.scheduler.runs.in_progress: %w", err)
	}
	if m.claims, err = meter.Int64Counter("cyoda.scheduler.claims",
		metric.WithDescription("Scheduled tasks this node claimed, by reason")); err != nil {
		return nil, fmt.Errorf("instrument cyoda.scheduler.claims: %w", err)
	}
	if m.heartbeatFailures, err = meter.Int64Counter("cyoda.scheduler.heartbeat.failures",
		metric.WithDescription("Scheduler heartbeats that failed or came too late")); err != nil {
		return nil, fmt.Errorf("instrument cyoda.scheduler.heartbeat.failures: %w", err)
	}
	if m.bookkeepingRetries, err = meter.Int64Counter("cyoda.scheduler.bookkeeping.retries",
		metric.WithDescription("Writes of a run's outcome that failed and were retried")); err != nil {
		return nil, fmt.Errorf("instrument cyoda.scheduler.bookkeeping.retries: %w", err)
	}
	return m, nil
}

func (m *metrics) runStarted() { m.inProgress.Add(context.Background(), 1) }

func (m *metrics) runEnded(outcome string, d time.Duration) {
	ctx := context.Background()
	attrs := metric.WithAttributes(attribute.String("outcome", outcome))
	m.inProgress.Add(ctx, -1)
	m.runs.Add(ctx, 1, attrs)
	m.runDuration.Record(ctx, d.Seconds(), attrs)
}

func (m *metrics) claimed(reason string) {
	m.claims.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", reason)))
}

func (m *metrics) heartbeatFailed() { m.heartbeatFailures.Add(context.Background(), 1) }

func (m *metrics) bookkeepingRetried() { m.bookkeepingRetries.Add(context.Background(), 1) }
