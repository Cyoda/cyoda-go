package postgres

import (
	"syscall"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// TestSchedulerMN_FrozenOwnerSendsNoUnsafe: pnodes 0-2 claim, pnode 3 hosts
// the clients. The run's first processor (idempotent) is held; its owner is
// frozen (SIGSTOP). Its heartbeat stops, and after STALE_AFTER a survivor
// claims the task (lostOwners 1), runs the first processor again and marks
// and sends the unsafe second one, which is held too. Then the frozen owner
// resumes (SIGCONT) with its first processor answered: its run goes on where
// it stopped and reaches the unsafe processor under a claim that is no longer
// its own.
//
// The watchdog does not protect a frozen owner (spec §6.3): the mark and the
// claim token do, as the check before every unsafe send and as the fence on
// every write. So while the survivor's run is still in progress, the resumed
// owner must send nothing, and none of its late writes may land: the task row
// stays exactly the survivor's claim. Then the survivor fires: the unsafe
// processor was sent once, the entity fired once.
//
// A SIGSTOP does not stop the monotonic clock, so on the SIGCONT the owner's
// overdue watchdog may fire and race the run; it too stops the send, and its
// cancelled run then records an attempt, which the fence must refuse. The send
// is seen only when both the MarkUnsafe refusal and the watchdog are removed;
// the late write is seen when the claim-token fence is removed.
func TestSchedulerMN_FrozenOwnerSendsNoUnsafe(t *testing.T) {
	t.Parallel()
	const host = 3
	s := newSchedMN(t, 4, fixtureutil.LaunchOpts{NodeEnv: hostOnly(host)}, mnLongAnswerEnv)
	tenant := s.pg.NewTenant(t)
	c := s.client(host, tenant)
	tagHold, tagUnsafe := "fh-"+mnShort(), "fu-"+mnShort()
	hold := s.startClient(t, host, tenant, tagHold, parity.ComputeBehaviourHold)
	unsafe := s.startClient(t, host, tenant, tagUnsafe, parity.ComputeBehaviourHold)
	id := mnSetup(t, c, "mn-frozen", mnWorkflow("mn-frozen-wf", mnFire(300, 0,
		mnProc("noop", "SYNC", tagHold, true, mnLongAnswer),
		mnProc("noop", "SYNC", tagUnsafe, false, mnLongAnswer))))

	// A task is RUNNING from its claim, before its processor is sent: wait
	// for the processor itself before freezing its owner.
	first := s.awaitTask(t, id, "Fire", 30*time.Second, "running", func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" })
	mnAwait(t, 10*time.Second, "the held processor", func() bool { return mnReceived(t, hold, id) == 1 })
	owner := s.ownerNode(t, first.ClaimOwner)
	if owner < 0 || owner == host {
		t.Fatalf("owner %q is not a claiming pnode", first.ClaimOwner)
	}
	if err := s.pg.SignalNode(owner, syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP node %d: %v", owner, err)
	}
	t.Cleanup(func() { _ = s.pg.SignalNode(owner, syscall.SIGCONT) })

	re := s.awaitTask(t, id, "Fire", fixtureutil.TunedStaleAfter+40*time.Second, "the reclaim",
		func(r mnTask, ok bool) bool {
			return ok && r.Status == "RUNNING" && r.ClaimOwner != first.ClaimOwner && r.LostOwners == 1
		})
	if n := s.ownerNode(t, re.ClaimOwner); n == owner || n < 0 || n == host {
		t.Fatalf("reclaimed by node %d; want another claiming pnode", n)
	}
	mnAwait(t, 10*time.Second, "the reclaimed run's held processor", func() bool { return mnReceived(t, hold, id) == 2 })
	hold.Release(t) // answers both: the survivor's now, the frozen owner's when it resumes

	// The survivor marks and sends the unsafe processor, which is held: its
	// claim stays in progress while the frozen owner resumes.
	mnAwait(t, 10*time.Second, "the survivor's unsafe processor", func() bool { return mnReceived(t, unsafe, id) == 1 })
	claimed := s.awaitTask(t, id, "Fire", 10*time.Second, "the survivor's mark",
		func(r mnTask, ok bool) bool { return ok && r.Marked })
	if claimed.ClaimToken != re.ClaimToken || claimed.Status != "RUNNING" {
		t.Fatalf("task = %+v; want the survivor's claim %s RUNNING", claimed, re.ClaimToken)
	}

	// Resume the owner. Its heartbeat landing again proves it runs.
	beat := s.heartbeatAt(t, first.ClaimOwner)
	if err := s.pg.SignalNode(owner, syscall.SIGCONT); err != nil {
		t.Fatalf("SIGCONT node %d: %v", owner, err)
	}
	mnAwait(t, 20*time.Second, "the resumed owner's heartbeat", func() bool { return s.heartbeatAt(t, first.ClaimOwner).After(beat) })

	// With both guards removed, the resumed run's unsafe processor arrives
	// before the first poll. Watch well past that and past a bookkeeping
	// retry: at no point may the owner send, or change the row.
	until := time.Now().Add(15 * time.Second)
	for time.Now().Before(until) {
		if n := mnReceived(t, unsafe, id); n != 1 {
			t.Fatalf("the unsafe processor was sent %d times after the frozen owner resumed; want 1", n)
		}
		if r, ok := s.task(t, id, "Fire"); !ok || r != claimed {
			t.Fatalf("a late write landed after the frozen owner resumed: task present=%t %+v; want %+v", ok, r, claimed)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The survivor completes: one send, one fire, no failure.
	unsafe.Release(t)
	mnAwait(t, 30*time.Second, "the fire", func() bool {
		got, err := c.GetEntity(t, id)
		return err == nil && got.Meta.State == "Done"
	})
	if n := mnReceived(t, unsafe, id); n != 1 {
		t.Errorf("the unsafe processor was sent %d times; want 1", n)
	}
	if n := mnCountEvents(t, c, id, "SCHEDULED_TRANSITION_FIRE"); n != 1 {
		t.Errorf("%d fires; want 1", n)
	}
	if n := mnCountEvents(t, c, id, "SCHEDULED_TRANSITION_FAIL"); n != 0 {
		t.Errorf("%d SCHEDULED_TRANSITION_FAIL events; want 0", n)
	}
}
