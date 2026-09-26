package postgres

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// lostSubject is one task of TestSchedulerMN_LostOwners, in a tenant of its own.
type lostSubject struct {
	tenant parity.Tenant
	c      *client.Client
	tag    string
	id     uuid.UUID
	stall  parity.ComputeClient
	claim  mnTask
}

// TestSchedulerMN_LostOwners: four tasks run on live owners with their
// processors stalled on the host pnode:
//
//	A  idempotent processor, no mark
//	B  unsafe processor: a mark is written before it is sent
//	C  idempotent, timeoutMs 5000
//	D  a cascade step commits the entity in Mid (COMMIT_BEFORE_DISPATCH),
//	   then its idempotent second processor stalls
//
// First, for longer than STALE_AFTER — while every other pnode may make
// lost-owner claims — nobody claims them: the owners heartbeat, and liveness
// is not progress (§6.2). Then every owner is killed. After STALE_AFTER a
// survivor claims each with lostOwners 1 and decides (§5.1): B is FAILED
// UNSAFE_WORK_NOT_COMPLETED and its processor was sent exactly once; C is
// FAILED EXPIRED_AFTER_FAILED_ATTEMPTS; D is FAILED STOPPED_AFTER_PARTIAL_COMMIT;
// A runs again and fires.
func TestSchedulerMN_LostOwners(t *testing.T) {
	t.Parallel()
	const host = 5
	s := newSchedMN(t, 6, fixtureutil.LaunchOpts{NodeEnv: hostOnly(host)}, mnLongAnswerEnv)

	subject := func() *lostSubject {
		tenant := s.pg.NewTenant(t)
		sub := &lostSubject{tenant: tenant, c: s.client(host, tenant), tag: "lo-" + mnShort()}
		sub.stall = s.startClient(t, host, tenant, sub.tag, parity.ComputeBehaviourStall)
		return sub
	}
	a, b, cc, d := subject(), subject(), subject(), subject()
	a.id = mnSetup(t, a.c, "mn-lo-a", mnWorkflow("mn-lo-a-wf", mnFire(300, 0, mnProc("noop", "SYNC", a.tag, true, mnLongAnswer))))
	b.id = mnSetup(t, b.c, "mn-lo-b", mnWorkflow("mn-lo-b-wf", mnFire(300, 0, mnProc("noop", "SYNC", b.tag, false, mnLongAnswer))))
	cc.id = mnSetup(t, cc.c, "mn-lo-c", mnWorkflow("mn-lo-c-wf", mnFire(300, 5000, mnProc("noop", "SYNC", cc.tag, true, mnLongAnswer))))
	dStep := "lod-" + mnShort()
	dCommit := s.startClient(t, host, d.tenant, dStep, parity.ComputeBehaviourCatalog)
	d.id = mnSetup(t, d.c, "mn-lo-d", mnWorkflow("mn-lo-d-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Fire", "next": "Mid", "manual": false, "schedule": map[string]any{"delayMs": 300}}}},
		"Mid": map[string]any{"transitions": []any{map[string]any{
			"name": "Step", "next": "Done", "manual": false, "processors": []any{
				mnProc("noop", "COMMIT_BEFORE_DISPATCH", dStep, true, 10000),
				mnProc("noop", "SYNC", d.tag, true, mnLongAnswer)}}}},
		"Done": map[string]any{},
	}))

	// All four running, each with its processor at its stalled client.
	a.claim = s.awaitTask(t, a.id, "Fire", 30*time.Second, "A running", func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" })
	b.claim = s.awaitTask(t, b.id, "Fire", 30*time.Second, "B running with its mark", func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" && r.Marked })
	cc.claim = s.awaitTask(t, cc.id, "Fire", 30*time.Second, "C running", func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" })
	d.claim = s.awaitTask(t, d.id, "Fire", 30*time.Second, "D running after its partial commit", func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" && r.PartialCommit })
	all := []*lostSubject{a, b, cc, d}
	for _, sub := range all {
		mnAwait(t, 10*time.Second, "the stalled processor", func() bool { return mnReceived(t, sub.stall, sub.id) == 1 })
	}
	runningAt := time.Now()

	// Not claimed elsewhere: the owners heartbeat through a whole stale
	// period in which every other pnode is allowed lost-owner claims.
	until := s.bootAt.Add(fixtureutil.TunedStaleAfter + 5*time.Second)
	if min := runningAt.Add(fixtureutil.TunedStaleAfter); until.Before(min) {
		until = min
	}
	for time.Now().Before(until) {
		for _, sub := range all {
			r, ok := s.task(t, sub.id, "Fire")
			if !ok || r.ClaimToken != sub.claim.ClaimToken || r.LostOwners != 0 {
				t.Fatalf("task %s was claimed while its owner heartbeated: %+v (was %+v)", sub.id, r, sub.claim)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	for _, sub := range all {
		if n := mnReceived(t, sub.stall, sub.id); n != 1 {
			t.Fatalf("task %s's processor was sent %d times during the run; want 1", sub.id, n)
		}
	}

	// Kill every owner.
	owners := map[int]bool{}
	for _, sub := range all {
		n := s.ownerNode(t, sub.claim.ClaimOwner)
		if n < 0 || n == host {
			t.Fatalf("task %s's owner %q is not a claiming pnode", sub.id, sub.claim.ClaimOwner)
		}
		owners[n] = true
	}
	for n := range owners {
		s.pg.KillNode(n)
	}
	survivors := map[string]bool{}
	for i := 0; i < host; i++ {
		if !owners[i] {
			survivors[s.pg.Incarnation(t, i).String()] = true
		}
	}

	// A must fire when reclaimed: its stalled client goes, a hold client takes
	// its tag. B, C and D keep their stalled clients (their counts must stay
	// 1) and gain healthy clients that must never be sent anything.
	a.stall.Stop()
	aHold := s.startClient(t, host, a.tenant, a.tag, parity.ComputeBehaviourHold)
	bFresh := s.startClient(t, host, b.tenant, b.tag, parity.ComputeBehaviourCatalog)
	cFresh := s.startClient(t, host, cc.tenant, cc.tag, parity.ComputeBehaviourCatalog)
	dFresh := s.startClient(t, host, d.tenant, d.tag, parity.ComputeBehaviourCatalog)

	within := fixtureutil.TunedStaleAfter + 30*time.Second
	ra := s.awaitTask(t, a.id, "Fire", within, "A claimed as a lost owner",
		func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" && r.LostOwners == 1 })
	if !survivors[ra.ClaimOwner] {
		t.Errorf("A was claimed by %q; want a surviving pnode", ra.ClaimOwner)
	}
	mnAwait(t, 10*time.Second, "A's processor at the hold client", func() bool { return mnReceived(t, aHold, a.id) == 1 })
	aHold.Release(t)
	mnAwait(t, 20*time.Second, "A fired", func() bool {
		got, err := a.c.GetEntity(t, a.id)
		return err == nil && got.Meta.State == "Done"
	})

	for _, tc := range []struct {
		sub    *lostSubject
		reason string
	}{
		{b, "UNSAFE_WORK_NOT_COMPLETED"},
		{cc, "EXPIRED_AFTER_FAILED_ATTEMPTS"},
		{d, "STOPPED_AFTER_PARTIAL_COMMIT"},
	} {
		r := s.awaitTask(t, tc.sub.id, "Fire", within, "FAILED", func(r mnTask, ok bool) bool { return ok && r.Status == "FAILED" })
		if r.FailureReason != tc.reason || r.LostOwners != 1 {
			t.Errorf("task %s = %+v; want FAILED %s with lostOwners 1", tc.sub.id, r, tc.reason)
		}
		if n := mnCountEvents(t, tc.sub.c, tc.sub.id, "SCHEDULED_TRANSITION_FAIL"); n != 1 {
			t.Errorf("task %s: %d SCHEDULED_TRANSITION_FAIL events; want 1", tc.sub.id, n)
		}
	}
	if got, err := d.c.GetEntity(t, d.id); err != nil || got.Meta.State != "Mid" {
		t.Errorf("D's entity = %+v (err %v); want Mid, the committed step", got.Meta, err)
	}

	// Sent once: three retry delays later nothing more has been sent.
	time.Sleep(3 * fixtureutil.TunedRetryDelay)
	for _, x := range []struct {
		name  string
		cc    parity.ComputeClient
		id    uuid.UUID
		wantN int
	}{
		{"B stalled", b.stall, b.id, 1}, {"B fresh", bFresh, b.id, 0},
		{"C stalled", cc.stall, cc.id, 1}, {"C fresh", cFresh, cc.id, 0},
		{"D stalled", d.stall, d.id, 1}, {"D fresh", dFresh, d.id, 0},
		{"D step commit", dCommit, d.id, 1},
	} {
		if n := mnReceived(t, x.cc, x.id); n != x.wantN {
			t.Errorf("%s: %d requests; want %d", x.name, n, x.wantN)
		}
	}
}
