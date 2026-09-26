package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// TestSchedulerMN_DatabaseOutageLongerThanStaleAfter: pnodes 0 and 1 claim,
// pnode 2 hosts the compute client. Task T1 runs on its owner O, which is
// killed; task T2 then runs on the survivor S. The database is paused for
// longer than STALE_AFTER.
//
//   - During the outage S's heartbeats fail; its watchdog cancels T2's run,
//     and T2's bookkeeping is retried until the database is back: T2 records
//     a counted attempt with the fixed CANCELLED text (§5.6, §5.8).
//   - S's liveness record, removed as a sweep would remove it, is recreated by
//     the next heartbeat (§6.2).
//   - O is stale when the database returns, but S makes no lost-owner claim
//     until its own heartbeats have run clean for a whole STALE_AFTER (§6.1):
//     T1 keeps O's claim until then, and is claimed with lostOwners 1 after.
func TestSchedulerMN_DatabaseOutageLongerThanStaleAfter(t *testing.T) {
	t.Parallel()
	const host = 2
	s := newSchedMN(t, 3, fixtureutil.LaunchOpts{NodeEnv: hostOnly(host)}, mnLongAnswerEnv)
	tenant := s.pg.NewTenant(t)
	c := s.client(host, tenant)
	tag := "mo-" + mnShort()
	stall := s.startClient(t, host, tenant, tag, parity.ComputeBehaviourStall)
	wf := mnWorkflow("mn-outage-wf", mnFire(300, 0, mnProc("noop", "SYNC", tag, true, mnLongAnswer)))

	t1 := mnSetup(t, c, "mn-outage-1", wf)
	r1 := s.awaitTask(t, t1, "Fire", 30*time.Second, "T1 running",
		func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" })
	owner := s.ownerNode(t, r1.ClaimOwner)
	if owner != 0 && owner != 1 {
		t.Fatalf("T1's owner %q is not a claiming pnode (got node %d)", r1.ClaimOwner, owner)
	}
	survivor := 1 - owner
	survivorInc := s.pg.Incarnation(t, survivor).String()
	// RUNNING is written at the claim, before the dispatch: kill O only once
	// its run has sent the processor.
	mnAwait(t, 10*time.Second, "T1's processor sent by its owner", func() bool { return mnReceived(t, stall, t1) == 1 })
	s.pg.KillNode(owner)

	// T2 can only be claimed by the survivor now.
	t2 := mnSetup(t, c, "mn-outage-2", wf)
	r2 := s.awaitTask(t, t2, "Fire", 30*time.Second, "T2 running on the survivor",
		func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" })
	if r2.ClaimOwner != survivorInc {
		t.Fatalf("T2 is held by %q; want the survivor %q", r2.ClaimOwner, survivorInc)
	}
	mnAwait(t, 10*time.Second, "T2's processor sent by the survivor", func() bool { return mnReceived(t, stall, t2) == 1 })

	s.pg.PauseDatabase(t)
	time.Sleep(fixtureutil.TunedStaleAfter + 5*time.Second)
	s.pg.UnpauseDatabase(t)
	resumeAt := time.Now()

	// T2: the self-cancelled run's attempt, recorded after recovery.
	r2 = s.awaitTask(t, t2, "Fire", 30*time.Second, "T2's recorded attempt",
		func(r mnTask, ok bool) bool { return ok && r.Attempts >= 1 })
	if r2.LastError != "CANCELLED: the run was stopped by the scheduler" || r2.FailureReason != "" {
		t.Errorf("T2 after the outage = %+v; want a counted attempt with the CANCELLED text", r2)
	}

	// The survivor's liveness record: removed, then recreated by a heartbeat.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var removedAt time.Time
	if err := s.db.QueryRow(ctx,
		`WITH d AS (DELETE FROM scheduler_owners WHERE owner::text = $1 RETURNING 1) SELECT now() FROM d`,
		survivorInc).Scan(&removedAt); err != nil {
		t.Fatalf("remove the survivor's liveness record: %v", err)
	}
	mnAwait(t, 5*fixtureutil.TunedHeartbeatInterval, "the recreated liveness record", func() bool {
		var n int
		if err := s.db.QueryRow(ctx, `SELECT count(*) FROM scheduler_owners WHERE owner::text = $1 AND heartbeat_at > $2`,
			survivorInc, removedAt).Scan(&n); err != nil {
			t.Fatalf("read liveness: %v", err)
		}
		return n == 1
	})

	// T1: no lost-owner claim before a whole stale period of clean heartbeats.
	gate := resumeAt.Add(fixtureutil.TunedStaleAfter - 3*time.Second)
	for time.Now().Before(gate) {
		r, ok := s.task(t, t1, "Fire")
		if !ok || r.ClaimOwner != r1.ClaimOwner || r.LostOwners != 0 {
			t.Fatalf("T1 was claimed %s after the database returned (%+v); the survivor had not yet heartbeated clean for %s",
				time.Since(resumeAt), r, fixtureutil.TunedStaleAfter)
		}
		time.Sleep(500 * time.Millisecond)
	}
	r1 = s.awaitTask(t, t1, "Fire", 30*time.Second, "T1 claimed as a lost owner",
		func(r mnTask, ok bool) bool { return ok && r.ClaimOwner == survivorInc && r.LostOwners == 1 })
	if r1.Status != "RUNNING" {
		t.Errorf("T1 = %+v; want RUNNING under the survivor", r1)
	}
	mnAwait(t, 10*time.Second, "T1's processor sent again", func() bool { return mnReceived(t, stall, t1) >= 2 })
	if n := mnReceived(t, stall, t1); n != 2 {
		t.Errorf("T1's processor was sent %d times; want once by each owner", n)
	}
}
