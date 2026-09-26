package postgres

import (
	"syscall"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// TestSchedulerMN_ShutdownDrain: pnodes 0-2 claim, pnode 3 hosts the compute
// clients. T1's idempotent processor stalls; T2's unsafe processor is held and
// followed by a safe one. Every pnode that owns one of them gets SIGTERM (the
// signal path of run.go, spec §6.4), with a 2s drain.
//
//   - T1: cut after the drain with nothing unsafe handed off: WAITING,
//     uncounted, and claimed at once by a surviving pnode — within seconds,
//     not after STALE_AFTER — which sends its processor again.
//   - T2: its unsafe callout is in flight, so its run is not cut. Released
//     after the drain, it continues with the safe processor, commits, and its
//     owner then exits. The unsafe processor was sent once.
func TestSchedulerMN_ShutdownDrain(t *testing.T) {
	t.Parallel()
	const host = 3
	s := newSchedMN(t, 4, fixtureutil.LaunchOpts{NodeEnv: hostOnly(host)},
		mnLongAnswerEnv, "CYODA_SCHEDULER_SHUTDOWN_DRAIN=2s")
	tenant := s.pg.NewTenant(t)
	c := s.client(host, tenant)
	tag1, tag2a, tag2b := "sd1-"+mnShort(), "sd2a-"+mnShort(), "sd2b-"+mnShort()
	stall := s.startClient(t, host, tenant, tag1, parity.ComputeBehaviourStall)
	hold := s.startClient(t, host, tenant, tag2a, parity.ComputeBehaviourHold)
	safe := s.startClient(t, host, tenant, tag2b, parity.ComputeBehaviourCatalog)

	t1 := mnSetup(t, c, "mn-sd-1", mnWorkflow("mn-sd-1-wf", mnFire(300, 0, mnProc("noop", "SYNC", tag1, true, mnLongAnswer))))
	t2 := mnSetup(t, c, "mn-sd-2", mnWorkflow("mn-sd-2-wf", mnFire(300, 0,
		mnProc("noop", "SYNC", tag2a, false, mnLongAnswer), mnProc("noop", "SYNC", tag2b, true, 10000))))
	r1 := s.awaitTask(t, t1, "Fire", 30*time.Second, "T1 running", func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" })
	r2 := s.awaitTask(t, t2, "Fire", 30*time.Second, "T2 running", func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" && r.Marked })
	mnAwait(t, 10*time.Second, "both processors at their clients", func() bool {
		return mnReceived(t, stall, t1) == 1 && mnReceived(t, hold, t2) == 1
	})

	owners := map[int]bool{s.ownerNode(t, r1.ClaimOwner): true, s.ownerNode(t, r2.ClaimOwner): true}
	for n := range owners {
		if n < 0 || n == host {
			t.Fatalf("an owner is not a claiming pnode: %v", owners)
		}
		if err := s.pg.SignalNode(n, syscall.SIGTERM); err != nil {
			t.Fatalf("SIGTERM node %d: %v", n, err)
		}
	}
	signalAt := time.Now()
	t.Logf("owners signalled: %v", owners)

	// T1: cut, recorded uncounted, claimed at once elsewhere.
	again := s.awaitTask(t, t1, "Fire", 20*time.Second, "T1 claimed by a survivor",
		func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" && r.ClaimOwner != r1.ClaimOwner })
	if since := time.Since(signalAt); since > 15*time.Second {
		t.Errorf("T1 was claimed again %s after SIGTERM; want at once, not after STALE_AFTER", since)
	}
	if n := s.ownerNode(t, again.ClaimOwner); owners[n] || n == host || n < 0 {
		t.Errorf("T1 was claimed by node %d; want a pnode that was not signalled", n)
	}
	if again.Attempts != 0 || again.LostOwners != 0 || again.LastError != "CANCELLED: the run was stopped by the scheduler" {
		t.Errorf("T1 after the cut = %+v; want attempts 0, lostOwners 0 and the CANCELLED text", again)
	}
	mnAwait(t, 10*time.Second, "T1's processor sent again", func() bool { return mnReceived(t, stall, t1) == 2 })

	// T2: not cut. Its owner must still be up 4s after the signal — past the
	// 2s drain and step 3 — with the task not FAILED; then the unsafe callout
	// is released.
	owner2 := s.ownerNode(t, r2.ClaimOwner)
	if err := s.pg.AwaitNodeExit(owner2, time.Until(signalAt.Add(4*time.Second))); err == nil {
		t.Fatalf("T2's owner (node %d) exited with an unsafe callout in flight", owner2)
	}
	if r, ok := s.task(t, t2, "Fire"); !ok || r.Status == "FAILED" {
		t.Fatalf("T2 was cut or is missing (found=%v): %+v", ok, r)
	}
	hold.Release(t)
	mnAwait(t, 30*time.Second, "T2 fired", func() bool {
		got, err := c.GetEntity(t, t2)
		return err == nil && got.Meta.State == "Done"
	})
	if n := mnReceived(t, hold, t2); n != 1 {
		t.Errorf("T2's unsafe processor was sent %d times; want 1", n)
	}
	if n := mnReceived(t, safe, t2); n != 1 {
		t.Errorf("T2's safe processor was sent %d times; want 1", n)
	}
	if n := mnCountEvents(t, c, t2, "SCHEDULED_TRANSITION_FIRE"); n != 1 {
		t.Errorf("T2 fired %d times; want 1", n)
	}
	if err := s.pg.AwaitNodeExit(owner2, 60*time.Second); err != nil {
		t.Errorf("T2's owner did not exit after its run committed: %v", err)
	}
}
