package scheduler

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

// A panic recovered outside the scheduler (the HTTP Recovery middleware, the
// gRPC server, the search reaper) latches Deps.HealthFlag false without ever
// touching Service.latched. Spec §6.5: a latched pnode stops claiming, but
// unless the heartbeat itself panicked, it keeps heartbeating.
func TestService_ExternalHealthFlagStopsClaimingButKeepsHeartbeating(t *testing.T) {
	var fires atomic.Int32
	h := newHarness(t, testConfig(), firerFunc(func(context.Context, spi.ScheduledTask, int, time.Duration) workflow.RunReport {
		fires.Add(1)
		return fired
	}))
	h.start(t)
	eventually(t, "healthy", func() bool { return h.svc.isHealthy() })

	// Simulate a panic recovered elsewhere in the process: only the shared
	// flag flips, never the scheduler's own s.latched.
	h.flag.Store(false)
	baseline := h.fs.claims()
	beats := h.fs.heartbeatCount()
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })

	eventually(t, "heartbeats go on after an external latch", func() bool { return h.fs.heartbeatCount() > beats+2 })
	if n := h.fs.claims(); n != baseline {
		t.Errorf("%d claims after an external latch, want none", n-baseline)
	}
	if n := fires.Load(); n != 0 {
		t.Errorf("%d runs fired after an external latch", n)
	}
}

// A run already in progress when the flag is latched from outside is not
// cancelled: only a panic recovered inside the scheduler (latchAndCancel)
// cancels runs in progress (§6.5).
func TestService_ARunInProgressWhenTheFlagIsSetFromOutsideIsNotCancelled(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		started <- struct{}{}
		select {
		case <-release:
			return workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: errors.New("boom")}
		case <-ctx.Done():
			return failedOnCancel(ctx)
		}
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	receive(t, started)

	h.flag.Store(false) // an external latch while the run is in flight

	time.Sleep(100 * time.Millisecond)
	if n := liveRuns(h.svc); n != 1 {
		t.Fatalf("%d live runs after an external latch, want the run still in progress", n)
	}
	close(release)
	eventually(t, "the outcome recorded", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	if a := h.fs.attemptsRecorded()[0]; a.Error == cancelledText {
		t.Errorf("attempt = %+v, want the run's own outcome, not a cancellation", a)
	}
}

// A claim that returns after an external latch is not started: it is refused
// and given back uncounted, the same as for the scheduler's own latch
// (service.go:618-631).
func TestService_AClaimInFlightWhenTheFlagIsSetFromOutsideIsGivenBackWithoutRunning(t *testing.T) {
	var fires atomic.Int32
	h := newHarness(t, testConfig(), firerFunc(func(context.Context, spi.ScheduledTask, int, time.Duration) workflow.RunReport {
		fires.Add(1)
		return fired
	}))
	block := make(chan struct{})
	h.fs.with(func() {
		h.fs.claimBlock = block
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	eventually(t, "a claim in flight", func() bool { return len(h.fs.claimed()) == 1 })
	h.flag.Store(false) // an external latch while the claim is in flight
	from := len(h.fs.giveBackCalls())
	close(block)

	token := h.fs.claimed()[0]
	eventually(t, "a give-back after the claim returned", func() bool { return len(h.fs.giveBackCalls()) > from })
	if keep := h.fs.giveBackCalls()[from]; slices.Contains(keep, token) {
		t.Errorf("the give-back kept %v; a claim that never ran is given back", keep)
	}
	if n := fires.Load(); n != 0 {
		t.Errorf("%d runs fired for a claim that returned after an external latch", n)
	}
	if a, f := len(h.fs.attemptsRecorded()), len(h.fs.failsRecorded()); a+f != 0 {
		t.Errorf("a claim that never ran wrote %d attempts and %d failures", a, f)
	}
	if n := liveRuns(h.svc); n != 0 {
		t.Errorf("%d live runs, want none", n)
	}
}
