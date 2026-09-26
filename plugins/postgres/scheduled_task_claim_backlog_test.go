package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// One tenant's due backlog does not slow another tenant's claims. A claim
// looks, per tenant, at no more tasks than it can take, so its cost follows
// the number of tenants with due work and the per-tenant limit, not the
// number of due tasks.
//
// The backlog is 1,000,000 due tasks of one tenant, each on its own entity.
// A claim that ranks every due task takes seconds over it (measured on
// PostgreSQL 17: 1.7-3.6 s); the bound below is an order of magnitude under
// that and far above what a bounded claim takes, so a loaded machine does not
// make the test flaky. Kept out of the shared parity suite: it owns its
// schema and its database load.
func TestPostgres_ClaimDue_CostDoesNotFollowOneTenantsBacklog(t *testing.T) {
	if testing.Short() {
		t.Skip("inserts a 1,000,000-row backlog")
	}
	const (
		backlog = 1_000_000
		bound   = 300 * time.Millisecond
		rounds  = 12
	)
	ctx := context.Background()
	pool := newTestPoolSized(t, 8)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	f := postgres.NewStoreFactory(pool)
	f.InitTransactionManager(newTestUUIDGenerator())
	t.Cleanup(func() { postgres.CloseSchedulerPoolsForTest(f) })
	sts, err := f.ScheduledTaskStore(ctx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}

	start := time.Now()
	if _, err := pool.Exec(ctx, `INSERT INTO scheduled_tasks
		(id, tenant_id, type, scheduled_time, entity_id, model_name, model_version,
		 transition, source_state, armed_at, arm_token, status, next_attempt_time)
		SELECT 'big-' || g, 'tenant-big', $1, g, 'e-' || g, 'M', 1, 'T', 'S', g,
		       gen_random_uuid(), 'WAITING', g
		  FROM generate_series(1, $2::bigint) AS g`,
		string(spi.ScheduledTaskFireTransition), backlog); err != nil {
		t.Fatalf("insert backlog: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE scheduled_tasks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	t.Logf("backlog of %d due tasks inserted in %v", backlog, time.Since(start))
	small := spi.TenantID("tenant-small")
	arm(t, sts, small, "e-small", "S", taskSpec(small, "e-small", "S", "T", backlog+1))

	req := spi.ClaimRequest{NowMs: backlog + 1, StaleAfter: time.Minute, Limit: 50, PerTenantLimit: 50, AllowLostOwner: true}
	// The owner is live, so a later round does not take back an earlier
	// round's claims; the lost-owner branch still reads its RUNNING tasks.
	req.Owner = uuid.New()
	if err := sts.Heartbeat(ctx, req.Owner); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	for i := range rounds {
		began := time.Now()
		got, err := sts.ClaimDue(ctx, req)
		took := time.Since(began)
		if err != nil {
			t.Fatalf("round %d: ClaimDue: %v", i, err)
		}
		t.Logf("round %d: %d claims in %v", i, len(got), took)
		if len(got) != req.Limit {
			t.Fatalf("round %d: %d claims, want %d", i, len(got), req.Limit)
		}
		if i == 0 && !containsTenant(got, small) {
			t.Fatalf("round 0: the small tenant's task was not claimed beside the backlog")
		}
		if took > bound {
			t.Errorf("round %d: the claim took %v over a %d-task backlog of another tenant, want under %v", i, took, backlog, bound)
		}
	}

	// The plan, for the record (go test -v).
	rows, err := pool.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS) `+postgres.RankClaimsSQLForTest,
		req.NowMs, req.AllowLostOwner, req.StaleAfter.Microseconds(), req.PerTenantLimit,
		[]string{}, []int{}, req.Limit, []string{}, []string{}, []string{}, []string{})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("explain: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain: %v", err)
	}
	t.Logf("ranking plan:\n%s", plan.String())
}

func containsTenant(tasks []spi.ScheduledTask, tenant spi.TenantID) bool {
	for _, x := range tasks {
		if x.TenantID == tenant {
			return true
		}
	}
	return false
}
