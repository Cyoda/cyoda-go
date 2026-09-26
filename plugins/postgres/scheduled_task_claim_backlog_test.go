package postgres_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// The claim timing tests below own their schema and their database load, so
// they stay out of the shared parity suite. Each asserts on the median of its
// rounds, with a bound far below what a claim that reads a backlog takes and
// far above what a bounded claim takes, so a loaded machine does not make them
// flaky. Measured on PostgreSQL 17 before the claim was bounded: 1.5 s per
// claim over one tenant's 1,000,000 due tasks.

const claimRounds = 7

// newBacklogStore is a migrated schema, a factory and its task store.
func newBacklogStore(t *testing.T) (*pgxpool.Pool, *postgres.StoreFactory, spi.ScheduledTaskStore) {
	t.Helper()
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
	sts, err := f.ScheduledTaskStore(context.Background())
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	return pool, f, sts
}

// insertTasks inserts n WAITING tasks of tenant||g (tenant alone when
// perTenant is false) with ids prefix||g, entity prefix||g, model model and
// next_attempt_time at+g, for g in 1..n.
func insertTasks(t *testing.T, pool *pgxpool.Pool, tenant string, perTenant bool, prefix, model string, at, n int64) {
	t.Helper()
	tenantExpr := `$1::text`
	if perTenant {
		tenantExpr = `$1::text || g`
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO scheduled_tasks
		(id, tenant_id, type, scheduled_time, entity_id, model_name, model_version,
		 transition, source_state, armed_at, arm_token, status, next_attempt_time)
		SELECT $2::text || g, `+tenantExpr+`, $3, $5 + g, $2::text || g, $4, 1, 'T', 'S', 0,
		       gen_random_uuid(), 'WAITING', $5 + g
		  FROM generate_series(1, $6::bigint) AS g`,
		tenant, prefix, string(spi.ScheduledTaskFireTransition), model, at, n); err != nil {
		t.Fatalf("insert tasks: %v", err)
	}
}

func analyze(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `ANALYZE scheduled_tasks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// timeClaims runs rounds claims of req as a live owner, checks each result,
// and returns the median time a claim took.
func timeClaims(t *testing.T, sts spi.ScheduledTaskStore, req spi.ClaimRequest, check func(round int, got []spi.ScheduledTask)) time.Duration {
	t.Helper()
	req.Owner = uuid.New()
	// The owner is live, so a later round does not take back an earlier
	// round's claims; the lost-owner branch still reads its RUNNING tasks.
	if err := sts.Heartbeat(context.Background(), req.Owner); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	var took []time.Duration
	for i := range claimRounds {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		began := time.Now()
		got, err := sts.ClaimDue(ctx, req)
		d := time.Since(began)
		cancel()
		if err != nil {
			t.Fatalf("round %d: ClaimDue after %v: %v", i, d, err)
		}
		t.Logf("round %d: %d claims in %v", i, len(got), d)
		check(i, got)
		took = append(took, d)
	}
	slices.Sort(took)
	return took[len(took)/2]
}

// explain logs the ranking's plan for req with the excluded rows given, in
// a transaction set up as a claim's is.
func explain(t *testing.T, pool *pgxpool.Pool, req spi.ClaimRequest, exclRows []string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('jit', 'off', true),
		set_config('plan_cache_mode', 'force_custom_plan', true)`); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var tenants []string
	rows, err := tx.Query(ctx, postgres.ClaimTenantsSQLForTest, req.NowMs, req.AllowLostOwner)
	if err != nil {
		t.Fatalf("tenants: %v", err)
	}
	for rows.Next() {
		var tn string
		if err := rows.Scan(&tn); err != nil {
			t.Fatalf("tenants: %v", err)
		}
		tenants = append(tenants, tn)
	}
	rows.Close()
	t.Logf("tenant list plan:\n%s", explainInTx(t, tx, postgres.ClaimTenantsSQLForTest, req.NowMs, req.AllowLostOwner))
	t.Logf("ranking plan:\n%s", explainInTx(t, tx, postgres.RankClaimsSQLForTest,
		req.NowMs, req.AllowLostOwner, req.StaleAfter.Microseconds(), req.PerTenantLimit,
		[]string{}, []int{}, req.Limit, exclRows, []string{}, tenants))
}

func explainInTx(t *testing.T, tx pgx.Tx, sql string, args ...any) string {
	t.Helper()
	rows, err := tx.Query(context.Background(), `EXPLAIN (ANALYZE, BUFFERS) `+sql, args...)
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
	return plan.String()
}

func containsTenant(tasks []spi.ScheduledTask, tenant spi.TenantID) bool {
	for _, x := range tasks {
		if x.TenantID == tenant {
			return true
		}
	}
	return false
}

// One tenant's due backlog does not slow another tenant's claims, whether it
// is the only other tenant or there are many tenants whose tasks are not due.
func TestPostgres_ClaimDue_CostDoesNotFollowOneTenantsBacklog(t *testing.T) {
	if testing.Short() {
		t.Skip("inserts large backlogs")
	}
	const bound = 300 * time.Millisecond
	for _, shape := range []struct {
		name          string
		backlog       int64
		futureTenants int64
	}{
		{name: "OneLargeTenant", backlog: 1_000_000},
		{name: "LargeTenantAndManyFutureTenants", backlog: 200_000, futureTenants: 25_000},
	} {
		t.Run(shape.name, func(t *testing.T) {
			pool, _, sts := newBacklogStore(t)
			start := time.Now()
			insertTasks(t, pool, "tenant-big", false, "big-", "M", 0, shape.backlog)
			// Each future-only tenant has one task, never due in this test.
			insertTasks(t, pool, "tenant-future-", true, "future-", "M", 1_000_000_000, shape.futureTenants)
			analyze(t, pool)
			t.Logf("%d due tasks and %d future-only tenants inserted in %v", shape.backlog, shape.futureTenants, time.Since(start))
			small := spi.TenantID("tenant-small")
			arm(t, sts, small, "e-small", "S", taskSpec(small, "e-small", "S", "T", shape.backlog+1))

			req := spi.ClaimRequest{NowMs: shape.backlog + 1, StaleAfter: time.Minute,
				Limit: 50, PerTenantLimit: 50, AllowLostOwner: true}
			median := timeClaims(t, sts, req, func(i int, got []spi.ScheduledTask) {
				if len(got) != req.Limit {
					t.Fatalf("round %d: %d claims, want %d", i, len(got), req.Limit)
				}
				if i == 0 && !containsTenant(got, small) {
					t.Fatalf("round 0: the small tenant's task was not claimed beside the backlog")
				}
			})
			if median > bound {
				t.Errorf("median claim took %v, want under %v", median, bound)
			}
			explain(t, pool, req, []string{})
		})
	}
}

// Rows an open transaction holds are passed over, and a claim over many of
// them still ends in bounded time: each ranking round reads, per tenant, the
// rows up to its quota plus the rows already found busy, never the tenant's
// backlog.
func TestPostgres_ClaimDue_BusyRowsCostBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("inserts a large backlog")
	}
	const (
		busy  = 1_000
		bound = 300 * time.Millisecond
	)
	pool, f, sts := newBacklogStore(t)
	big := spi.TenantID("tenant-big")
	// The busy rows are the tenant's earliest; the backlog follows them.
	insertTasks(t, pool, string(big), false, "busy-", "MB", 0, busy)
	insertTasks(t, pool, string(big), false, "big-", "M", busy, 200_000)
	insertTasks(t, pool, "tenant-future-", true, "future-", "M", 1_000_000_000, 5_000)
	analyze(t, pool)

	txCtx, rollback := beginEntityTx(t, f, big)
	defer rollback()
	if err := sts.DeleteForModel(txCtx, big, "MB", 1, nil); err != nil {
		t.Fatalf("DeleteForModel: %v", err)
	}

	req := spi.ClaimRequest{NowMs: busy + 200_001, StaleAfter: time.Minute,
		Limit: 50, PerTenantLimit: 50, AllowLostOwner: true}
	median := timeClaims(t, sts, req, func(i int, got []spi.ScheduledTask) {
		if len(got) != req.Limit {
			t.Fatalf("round %d: %d claims, want %d", i, len(got), req.Limit)
		}
		for _, c := range got {
			if strings.HasPrefix(c.ID, "busy-") {
				t.Fatalf("round %d: claimed %s, which an open transaction holds", i, c.ID)
			}
		}
	})
	if median > bound {
		t.Errorf("median claim took %v with %d busy rows, want under %v", median, busy, bound)
	}

	// The last ranking round of a claim runs with every busy row excluded.
	excl := make([]string, busy)
	for i := range busy {
		excl[i] = postgres.PairKeyForTest(string(big), "busy-"+strconv.Itoa(i+1))
	}
	explain(t, pool, req, excl)
}
