package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// One tenant's due backlog does not slow another tenant's claims: a claim
// reads, per tenant, no more tasks than it can take. The backlog is 200,000
// due tasks of one tenant, each on its own entity. A claim that reads every
// due task into Go takes about a second over it; the bound is far above what
// a bounded claim takes.
func TestTasks_ClaimCostDoesNotFollowOneTenantsBacklog(t *testing.T) {
	if testing.Short() {
		t.Skip("inserts a 200,000-row backlog")
	}
	const (
		backlog = 200_000
		bound   = 150 * time.Millisecond
		rounds  = 5
	)
	fx := newTaskFixture(t)
	db := sqlite.DBForTest(fx.f)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `WITH RECURSIVE g(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM g WHERE n < ?)
		INSERT INTO scheduled_tasks (id, tenant_id, type, scheduled_time, entity_id, model_name, model_version,
			transition, source_state, armed_at, arm_token, status, next_attempt_time)
		SELECT 'big-' || n, 'tenant-big', ?, n, 'e-' || n, 'M', 1, 'T', 'S', n,
		       lower(hex(randomblob(16))), 'WAITING', n
		  FROM g`, backlog, string(spi.ScheduledTaskFireTransition)); err != nil {
		t.Fatalf("insert backlog: %v", err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	small := spi.TenantID("tenant-small")
	if _, err := fx.sts.ReconcileForEntity(ctx, spi.ReconcileRequest{
		TenantID: small, EntityID: "e-small", CurrentState: "S",
		Arm: []spi.ScheduledTask{{ID: "small", TenantID: small, Type: spi.ScheduledTaskFireTransition,
			ScheduledTime: backlog + 1, EntityID: "e-small", ModelName: "M", ModelVersion: 1,
			Transition: "T", SourceState: "S"}},
	}); err != nil {
		t.Fatalf("arm: %v", err)
	}

	req := spi.ClaimRequest{NowMs: backlog + 1, StaleAfter: time.Minute, Limit: 50, PerTenantLimit: 50, AllowLostOwner: true}
	// The owner is live, so a later round does not take back an earlier
	// round's claims; the lost-owner branch still reads its RUNNING tasks.
	req.Owner = uuid.New()
	if err := fx.sts.Heartbeat(ctx, req.Owner); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	for i := range rounds {
		began := time.Now()
		got, err := fx.sts.ClaimDue(ctx, req)
		took := time.Since(began)
		if err != nil {
			t.Fatalf("round %d: ClaimDue: %v", i, err)
		}
		t.Logf("round %d: %d claims in %v", i, len(got), took)
		if len(got) != req.Limit {
			t.Fatalf("round %d: %d claims, want %d", i, len(got), req.Limit)
		}
		if i == 0 && !hasTenant(got, small) {
			t.Fatalf("round 0: the small tenant's task was not claimed beside the backlog")
		}
		if took > bound {
			t.Errorf("round %d: the claim took %v over a %d-task backlog of another tenant, want under %v", i, took, backlog, bound)
		}
	}
}

func hasTenant(tasks []spi.ScheduledTask, tenant spi.TenantID) bool {
	for _, x := range tasks {
		if x.TenantID == tenant {
			return true
		}
	}
	return false
}
