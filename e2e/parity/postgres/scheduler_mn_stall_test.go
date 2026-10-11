package postgres

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// TestSchedulerMN_LostOwnerAfterSurvivorStall: pnodes 0 and 1 claim, pnode 2
// hosts the compute client. Task T runs on its owner O, which is killed.
// Before O goes stale, the database stalls for longer than one tuned
// heartbeat's budget, so a heartbeat of the survivor S fails.
//
//   - S makes no lost-owner claim until its heartbeats have run clean for a
//     whole STALE_AFTER after that failure (§6.1), although O went stale
//     earlier.
//   - S then claims T with lostOwners 1, promptly: a short stall postpones
//     the claim and does not prevent it.
//
// This is the stall a loaded test host causes at random; here it is placed so
// that a lost-owner wait counting from O going stale alone always fails.
func TestSchedulerMN_LostOwnerAfterSurvivorStall(t *testing.T) {
	t.Parallel()
	const host = 2
	s := newSchedMN(t, 3, fixtureutil.LaunchOpts{NodeEnv: hostOnly(host)}, mnLongAnswerEnv)
	tenant := s.pg.NewTenant(t)
	c := s.client(host, tenant)
	tag := "ms-" + mnShort()
	stall := s.startClient(t, host, tenant, tag, parity.ComputeBehaviourStall)
	id := mnSetup(t, c, "mn-stall", mnWorkflow("mn-stall-wf", mnFire(300, 0, mnProc("noop", "SYNC", tag, true, mnLongAnswer))))

	first := s.awaitTask(t, id, "Fire", 30*time.Second, "T running",
		func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" })
	owner := s.ownerNode(t, first.ClaimOwner)
	if owner != 0 && owner != 1 {
		t.Fatalf("T's owner %q is not a claiming pnode (got node %d)", first.ClaimOwner, owner)
	}
	survivor := 1 - owner
	survivorInc := s.pg.Incarnation(t, survivor).String()
	// RUNNING is written at the claim, before the dispatch: kill O only once
	// its run has sent the processor.
	mnAwait(t, 10*time.Second, "T's processor sent by its owner", func() bool { return mnReceived(t, stall, id) == 1 })
	s.pg.KillNode(owner)
	ownerStaleAt := time.Now().Add(fixtureutil.TunedStaleAfter)

	// The stall: 40s after the kill, 13s before O goes stale. 2.5s is more
	// than the 1s a tuned heartbeat may take.
	time.Sleep(40 * time.Second)
	pauseAt := time.Now()
	s.pg.PauseDatabase(t)
	time.Sleep(2500 * time.Millisecond)
	s.pg.UnpauseDatabase(t)

	// S's heartbeat failed in the stall. The failures that follow it (the
	// heartbeat's connection is re-dialled) land within a few heartbeats.
	mnAwait(t, 10*time.Second, "the survivor's failed heartbeat", func() bool {
		return !fixtureutil.LastHeartbeatFailure(s.pg.NodeLogs(survivor)).Before(pauseAt)
	})
	time.Sleep(3 * fixtureutil.TunedHeartbeatInterval)
	failedAt := fixtureutil.LastHeartbeatFailure(s.pg.NodeLogs(survivor))

	// No lost-owner claim before a whole stale period of clean heartbeats
	// after the failure. A later failure only moves the claim later still.
	gate := failedAt.Add(fixtureutil.TunedStaleAfter - time.Second)
	if !gate.After(ownerStaleAt) {
		t.Fatalf("the survivor's last failed heartbeat (%s) is not late enough to postpone the claim past the owner going stale (%s)", failedAt, ownerStaleAt)
	}
	for time.Now().Before(gate) {
		r, ok := s.task(t, id, "Fire")
		if !ok || r.ClaimToken != first.ClaimToken || r.LostOwners != 0 {
			t.Fatalf("T was claimed %s after the survivor's heartbeat failed (%+v); the survivor had not yet heartbeated clean for %s",
				time.Since(failedAt), r, fixtureutil.TunedStaleAfter)
		}
		time.Sleep(500 * time.Millisecond)
	}
	r := s.awaitLostOwnerClaim(t, id, "Fire", ownerStaleAt, 30*time.Second, []int{survivor}, "T claimed as a lost owner",
		func(r mnTask, ok bool) bool { return ok && r.ClaimOwner == survivorInc && r.LostOwners == 1 })
	if r.Status != "RUNNING" {
		t.Errorf("T = %+v; want RUNNING under the survivor", r)
	}
	mnAwait(t, 10*time.Second, "T's processor sent again", func() bool { return mnReceived(t, stall, id) == 2 })
}
