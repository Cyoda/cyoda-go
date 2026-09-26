package sqlite_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// insertBacklog inserts n WAITING tasks, ids and entities prefix||g and
// next_attempt_time at+g for g in 1..n, of tenant, or of tenant||g when
// perTenant.
func insertBacklog(t *testing.T, db *sql.DB, tenant string, perTenant bool, prefix string, at, n int64) {
	t.Helper()
	tenantExpr := `?1`
	if perTenant {
		tenantExpr = `?1 || n`
	}
	if _, err := db.ExecContext(context.Background(), `WITH RECURSIVE g(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM g WHERE n < ?5)
		INSERT INTO scheduled_tasks (id, tenant_id, type, scheduled_time, entity_id, model_name, model_version,
			transition, source_state, armed_at, arm_token, status, next_attempt_time)
		SELECT ?2 || n, `+tenantExpr+`, ?3, ?4 + n, ?2 || n, 'M', 1, 'T', 'S', 0,
		       lower(hex(randomblob(16))), 'WAITING', ?4 + n
		  FROM g`, tenant, prefix, string(spi.ScheduledTaskFireTransition), at, n); err != nil {
		t.Fatalf("insert backlog: %v", err)
	}
}

// One tenant's due backlog does not slow another tenant's claims, whether it
// is the only other tenant or there are many tenants whose tasks are not due:
// a claim reads, per tenant with a due task, no more tasks than it can take.
// The bound is on the median of the rounds, far below what a claim that reads
// the backlog or queries every tenant takes (about a second each) and far
// above what a bounded claim takes.
func TestTasks_ClaimCostDoesNotFollowOneTenantsBacklog(t *testing.T) {
	if testing.Short() {
		t.Skip("inserts large backlogs")
	}
	const (
		bound  = 150 * time.Millisecond
		rounds = 7
	)
	for _, shape := range []struct {
		name          string
		backlog       int64
		futureTenants int64
	}{
		{name: "OneLargeTenant", backlog: 200_000},
		{name: "LargeTenantAndManyFutureTenants", backlog: 200_000, futureTenants: 5_000},
	} {
		t.Run(shape.name, func(t *testing.T) {
			fx := newTaskFixture(t)
			db := sqlite.DBForTest(fx.f)
			ctx := context.Background()
			insertBacklog(t, db, "tenant-big", false, "big-", 0, shape.backlog)
			insertBacklog(t, db, "tenant-future-", true, "future-", 1_000_000_000, shape.futureTenants)
			if _, err := db.ExecContext(ctx, `ANALYZE`); err != nil {
				t.Fatalf("analyze: %v", err)
			}
			small := spi.TenantID("tenant-small")
			if _, err := fx.sts.ReconcileForEntity(ctx, spi.ReconcileRequest{
				TenantID: small, EntityID: "e-small", CurrentState: "S",
				Arm: []spi.ScheduledTask{{ID: "small", TenantID: small, Type: spi.ScheduledTaskFireTransition,
					ScheduledTime: shape.backlog + 1, EntityID: "e-small", ModelName: "M", ModelVersion: 1,
					Transition: "T", SourceState: "S"}},
			}); err != nil {
				t.Fatalf("arm: %v", err)
			}

			req := spi.ClaimRequest{NowMs: shape.backlog + 1, StaleAfter: time.Minute,
				Limit: 50, PerTenantLimit: 50, AllowLostOwner: true}
			// The owner is live, so a later round does not take back an
			// earlier round's claims; the lost-owner query still reads its
			// RUNNING tasks.
			req.Owner = uuid.New()
			if err := fx.sts.Heartbeat(ctx, req.Owner); err != nil {
				t.Fatalf("heartbeat: %v", err)
			}
			var took []time.Duration
			for i := range rounds {
				began := time.Now()
				got, err := fx.sts.ClaimDue(ctx, req)
				d := time.Since(began)
				if err != nil {
					t.Fatalf("round %d: ClaimDue: %v", i, err)
				}
				t.Logf("round %d: %d claims in %v", i, len(got), d)
				if len(got) != req.Limit {
					t.Fatalf("round %d: %d claims, want %d", i, len(got), req.Limit)
				}
				if i == 0 && !hasTenant(got, small) {
					t.Fatalf("round 0: the small tenant's task was not claimed beside the backlog")
				}
				took = append(took, d)
			}
			slices.Sort(took)
			if median := took[len(took)/2]; median > bound {
				t.Errorf("median claim took %v, want under %v", median, bound)
			}
		})
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
