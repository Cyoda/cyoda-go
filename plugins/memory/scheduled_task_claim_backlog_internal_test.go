package memory

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// One tenant's due backlog of 200,000 tasks, each on its own entity, does not
// slow another tenant's claims: a claim reads, per tenant, no more tasks than
// it can take (see claimCandidatesLocked). A claim that reads and sorts every
// task row takes about 200 ms over this backlog; the bound, on the median of
// the rounds, is far below that and far above what a bounded claim takes.
func TestTasks_ClaimCostDoesNotFollowOneTenantsBacklog(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 200,000-task backlog")
	}
	const (
		backlog = 200_000
		bound   = 50 * time.Millisecond
		rounds  = 7
	)
	f := NewStoreFactory(WithClock(NewTestClockAt(time.UnixMilli(1_000_000))))
	t.Cleanup(func() { _ = f.Close() })
	sts := &scheduledTaskStore{f: f}
	ops := make([]scheduledTaskOp, backlog)
	for i := range backlog {
		k := taskKey{tenant: "tenant-big", id: fmt.Sprintf("big-%07d", i)}
		ops[i] = scheduledTaskOp{key: k, after: &spi.ScheduledTask{
			ID: k.id, TenantID: k.tenant, Type: spi.ScheduledTaskFireTransition,
			ScheduledTime: int64(i), EntityID: fmt.Sprintf("e-%07d", i), ModelName: "M", ModelVersion: 1,
			Transition: "T", SourceState: "S", ArmToken: uuid.New(),
			Status: spi.ScheduledTaskWaiting, NextAttemptTime: int64(i),
		}}
	}
	func() {
		f.entityMu.Lock()
		defer f.entityMu.Unlock()
		f.txManager.commitTaskWrites(ops)
	}()
	small := spi.TenantID("tenant-small")
	if _, err := sts.ReconcileForEntity(context.Background(), spi.ReconcileRequest{
		TenantID: small, EntityID: "e-small", CurrentState: "S",
		Arm: []spi.ScheduledTask{{ID: "small", TenantID: small, Type: spi.ScheduledTaskFireTransition,
			ScheduledTime: backlog, EntityID: "e-small", ModelName: "M", ModelVersion: 1,
			Transition: "T", SourceState: "S"}},
	}); err != nil {
		t.Fatalf("arm: %v", err)
	}

	req := spi.ClaimRequest{Owner: uuid.New(), NowMs: backlog, StaleAfter: time.Minute,
		Limit: 50, PerTenantLimit: 50, AllowLostOwner: true}
	if err := sts.Heartbeat(context.Background(), req.Owner); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	var durations []time.Duration
	for i := range rounds {
		began := time.Now()
		got, err := sts.ClaimDue(context.Background(), req)
		took := time.Since(began)
		if err != nil {
			t.Fatalf("round %d: ClaimDue: %v", i, err)
		}
		t.Logf("round %d: %d claims in %v", i, len(got), took)
		if len(got) != req.Limit {
			t.Fatalf("round %d: %d claims, want %d", i, len(got), req.Limit)
		}
		if i == 0 {
			found := false
			for _, c := range got {
				found = found || c.TenantID == small
			}
			if !found {
				t.Fatalf("round 0: the small tenant's task was not claimed beside the backlog")
			}
		}
		durations = append(durations, took)
	}
	slices.Sort(durations)
	if median := durations[len(durations)/2]; median > bound {
		t.Errorf("median claim took %v over a %d-task backlog of another tenant, want under %v", median, backlog, bound)
	}
}
