package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

// withRunMetrics gives the service a meter and returns a reader of
// cyoda.scheduler.runs by outcome. Call it before start.
func withRunMetrics(t *testing.T, h *harness) func() map[string]int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	h.svc.deps.Meter = mp.Meter("test")
	return func() map[string]int64 {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		out := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, md := range sm.Metrics {
				sum, ok := md.Data.(metricdata.Sum[int64])
				if md.Name != "cyoda.scheduler.runs" || !ok {
					continue
				}
				for _, dp := range sum.DataPoints {
					v, _ := dp.Attributes.Value("outcome")
					out[v.AsString()] += dp.Value
				}
			}
		}
		return out
	}
}

// claimedTask is a task as ClaimDue returns it to h's service.
func claimedTask(h *harness, tenant spi.TenantID, id string) spi.ScheduledTask {
	task := dueTask(tenant, id)
	task.Status = spi.ScheduledTaskRunning
	task.Claim = &spi.TaskClaim{Token: uuid.New(), Owner: h.svc.incarnation}
	return task
}

// cancelOrFire returns a cancelled run's report when its context is
// cancelled, and fired otherwise after a short while.
func cancelOrFire(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
	select {
	case <-ctx.Done():
		return failedOnCancel(ctx)
	case <-time.After(100 * time.Millisecond):
		return fired
	}
}

// A heartbeat that fails while a claim is in flight does not cancel the runs
// that claim returns: one failed heartbeat never self-cancels (§6.3).
func TestService_OneFailedHeartbeatDuringAClaimDoesNotCancelItsRuns(t *testing.T) {
	h := newHarness(t, testConfig(), firerFunc(cancelOrFire))
	h.start(t)
	eventually(t, "healthy", h.svc.isHealthy)
	h.fs.with(func() { h.fs.hbErr = errors.New("heartbeat: connection refused") })
	eventually(t, "a failed heartbeat", func() bool { return h.fs.heartbeatFailures() >= 1 })

	h.svc.heartbeatDone(time.Now(), errors.New("heartbeat: connection refused"))
	runs, _ := h.svc.register([]spi.ScheduledTask{claimedTask(h, "t1", "task-1")}, 1)
	for _, r := range runs {
		go h.svc.run(r)
	}
	eventually(t, "the run released", func() bool { return liveRuns(h.svc) == 0 })
	if a := h.fs.attemptsRecorded(); len(a) != 0 {
		t.Errorf("attempts = %+v: one failed heartbeat cancelled a just-claimed run", a)
	}
}

// A claim whose reply arrives after the watchdog fired: its runs are
// cancelled at once, as self-cancelled.
func TestService_ClaimReplyAfterTheWatchdogFiredIsSelfCancelled(t *testing.T) {
	h := newHarness(t, testConfig(), firerFunc(cancelOrFire))
	outcomes := withRunMetrics(t, h)
	h.svc.window = 50 * time.Millisecond
	h.start(t)
	eventually(t, "healthy", h.svc.isHealthy)
	h.fs.with(func() { h.fs.hbErr = errors.New("heartbeat: connection refused") })
	eventually(t, "a failed heartbeat", func() bool { return h.fs.heartbeatFailures() >= 1 })
	time.Sleep(2 * h.svc.window) // past the last in-time heartbeat's deadline

	runs, _ := h.svc.register([]spi.ScheduledTask{claimedTask(h, "t1", "task-1")}, 1)
	for _, r := range runs {
		go h.svc.run(r)
	}
	eventually(t, "the run's attempt", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	if a := h.fs.attemptsRecorded()[0]; a.NotCounted || a.Error != cancelledText {
		t.Errorf("attempt = %+v, want a counted attempt with %q", a, cancelledText)
	}
	eventually(t, "the run counted as self_cancelled", func() bool { return outcomes()[outcomeSelfCancelled] == 1 })
}

// A hung heartbeat never reports failure; the watchdog alone stops claims.
func TestService_HungHeartbeatStopsClaimsOnceTheWatchdogFires(t *testing.T) {
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		return failedOnCancel(ctx)
	}))
	h.svc.window = 100 * time.Millisecond
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	h.fs.with(func() { h.fs.hbBlock = hang })
	eventually(t, "the run cancelled by the watchdog", func() bool { return len(h.fs.attemptsRecorded()) == 1 })

	claims := h.fs.claims()
	time.Sleep(50 * time.Millisecond)
	if n := h.fs.claims(); n != claims {
		t.Errorf("%d claims after the watchdog fired with the heartbeat hung", n-claims)
	}
}

// The watchdog's deadline counts from the heartbeat's recorded start, not
// from when its reply arrived.
func TestService_WatchdogCountsFromTheHeartbeatsStart(t *testing.T) {
	cancelled := make(chan time.Time, 1)
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		r := failedOnCancel(ctx)
		cancelled <- time.Now()
		return r
	}))
	const window, delta = 2 * time.Second, 100 * time.Millisecond
	h.svc.window = window
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })
	h.fs.with(func() { h.fs.hbErr = errors.New("heartbeat: connection refused") })
	eventually(t, "a failed heartbeat", func() bool { return h.fs.heartbeatFailures() >= 1 })

	at := time.Now()
	h.svc.heartbeatDone(at.Add(-(window - delta)), nil)
	if d := receive(t, cancelled).Sub(at); d > window/2 {
		t.Errorf("the watchdog fired %s after a heartbeat that started W-%s ago; want about %s", d, delta, delta)
	}
}

// A slot that frees while heartbeats fail claims nothing.
func TestService_FreedSlotClaimsNothingWhileHeartbeatsFail(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRuns = 1
	cfg.ScanInterval = 50 * time.Millisecond
	release := make(chan struct{})
	h := newHarness(t, cfg, firerFunc(func(context.Context, spi.ScheduledTask, int, time.Duration) workflow.RunReport {
		<-release
		return fired
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1"), dueTask("t2", "task-2")} })
	h.start(t)
	eventually(t, "the only slot taken", func() bool { return liveRuns(h.svc) == 1 })
	h.fs.with(func() { h.fs.hbErr = errors.New("heartbeat: connection refused") })
	eventually(t, "a failed heartbeat", func() bool { return h.fs.heartbeatFailures() >= 1 })

	claims := h.fs.claims()
	close(release)
	eventually(t, "the slot freed", func() bool { return liveRuns(h.svc) == 0 })
	time.Sleep(100 * time.Millisecond)
	if n := h.fs.claims(); n != claims {
		t.Errorf("%d claims after a slot freed while heartbeats were failing", n-claims)
	}
}
