package e2e_test

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// scheduled_run_shutdown_test.go — the scheduler's drain on one stack
// (spec §6.4), driven through App.Shutdown while the cnode streams stay open:
// the harness, not the app, serves the gRPC listener, as the signal path
// keeps the servers up until the drain has ended (§6.4 step 6).

// shutdownAsync runs h.app.Shutdown on a goroutine and reports how long it took.
func shutdownAsync(h *callbackHarness) <-chan time.Duration {
	out := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		h.app.Shutdown()
		out <- time.Since(start)
	}()
	return out
}

// awaitWork waits for a held script to take its callout.
func awaitWork(t *testing.T, gotWork <-chan struct{}) {
	t.Helper()
	select {
	case <-gotWork:
	case <-time.After(scheduledFireTimeout):
		t.Fatalf("the scheduled run's callout did not reach the cnode within %s", scheduledFireTimeout)
	}
}

// requireStillRunning fails the test if Shutdown returns within d.
func requireStillRunning(t *testing.T, took <-chan time.Duration, d time.Duration, why string) {
	t.Helper()
	select {
	case got := <-took:
		t.Fatalf("Shutdown returned after %s; %s", got, why)
	case <-time.After(d):
	}
}

// TestSchedShutdown_RunFinishesWithinDrainStreamsOpen: a run in flight when
// the drain starts finishes inside it — its cnode's answer still arrives —
// and commits; Shutdown returns without waiting out the drain (§6.4 step 2).
// A second Shutdown does nothing.
func TestSchedShutdown_RunFinishesWithinDrainStreamsOpen(t *testing.T) {
	h, s := newSchedulerHarness(t, func(cfg *app.Config) { cfg.Scheduler.ShutdownDrain = 10 * time.Second })
	model, tag := uniq("sd-drain"), uniq("sd-drain-tag")
	gotWork, release := make(chan struct{}, 1), make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: holdScript(gotWork, release)})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sd-drain-wf", 100, 0, sProc("p", "SYNC", tag, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	awaitWork(t, gotWork)
	took := shutdownAsync(h)
	requireStillRunning(t, took, 500*time.Millisecond, "a run was in flight and the drain is 10s")
	rel() // answered over the still-open stream
	select {
	case d := <-took:
		if d >= 10*time.Second {
			t.Errorf("Shutdown took %s; the run ended long before the 10s drain", d)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	requireState(t, h, id, "Done")
	if _, ok := s.task(t, id, "Fire"); ok {
		t.Error("the drained run's task is still stored")
	}

	select {
	case <-shutdownAsync(h):
	case <-time.After(5 * time.Second):
		t.Fatal("a second Shutdown did not return at once")
	}
}

// TestSchedShutdown_UnsafeInFlightNotCut: the run's unsafe processor has been
// handed off and is in flight when the drain (1s) ends. Step 3 does not cut
// that run; its callout finishes, the run continues with a safe processor and
// commits; Shutdown returns after it (§6.4 step 3).
func TestSchedShutdown_UnsafeInFlightNotCut(t *testing.T) {
	h, s := newSchedulerHarness(t, nil) // ShutdownDrain 1s
	model, tagA, tagB := uniq("sd-unsafe"), uniq("sd-unsafe-a"), uniq("sd-unsafe-b")
	gotWork, release := make(chan struct{}, 1), make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: holdScript(gotWork, release)})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sd-unsafe-wf", 100, 0,
		sProc("p1", "SYNC", tagA, false), sProc("p2", "SYNC", tagB, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	awaitWork(t, gotWork)
	took := shutdownAsync(h)
	requireStillRunning(t, took, 3*time.Second, "an unsafe callout was in flight past the 1s drain and step 3")
	if r := mustTask(t, s, id); r.Status == "FAILED" {
		t.Fatalf("the run was cut: %+v", r)
	}
	rel()
	select {
	case <-took:
	case <-time.After(90 * time.Second):
		t.Fatal("Shutdown did not return after the exempt run ended")
	}
	requireState(t, h, id, "Done")
	if n := len(b.Received()); n != 1 {
		t.Errorf("the safe processor after the unsafe one was sent %d times; want 1", n)
	}
	if hasSMEventType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FAIL", "") {
		t.Error("a deploy turned the run into a FAILED task")
	}
}

// TestSchedShutdown_CutAfterEarlierUnsafeHandOffFails: the unsafe first
// processor completed; the idempotent second is in flight when the drain ends.
// No unsafe callout is in flight, so step 3 cuts the run; unsafe work reached a
// cnode and the run did not commit: FAILED UNSAFE_WORK_NOT_COMPLETED (§5.6).
func TestSchedShutdown_CutAfterEarlierUnsafeHandOffFails(t *testing.T) {
	h, s := newSchedulerHarness(t, nil) // ShutdownDrain 1s
	model, tagA, tagB := uniq("sd-cut"), uniq("sd-cut-a"), uniq("sd-cut-b")
	gotWork, release := make(chan struct{}, 1), make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}})
	h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: holdScript(gotWork, release)})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sd-cut-wf", 100, 0,
		sProc("p1", "SYNC", tagA, false), sProc("p2", "SYNC", tagB, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	awaitWork(t, gotWork)
	select {
	case <-shutdownAsync(h):
	case <-time.After(90 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	r := mustTask(t, s, id)
	if r.Status != "FAILED" || r.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Errorf("after the cut: %+v; want FAILED UNSAFE_WORK_NOT_COMPLETED", r)
	}
	if data := failEvent(t, h, id); data["reason"] != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Errorf("SCHEDULED_TRANSITION_FAIL data = %v", data)
	}
	requireState(t, h, id, "Open")
}
