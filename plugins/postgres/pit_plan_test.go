package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestPITBaseQuery_ProbesPerEntity asserts the property that bounds the cost:
// the point-in-time read reaches entity_versions through idx_ev_bitemporal,
// one probe per entity, rather than deduplicating every revision.
//
// It deliberately does NOT assert anything about the outer scan of `entities`
// (seq scan vs index scan): on a small, freshly-ANALYZEd table every row
// qualifies, so a seq scan there is the planner's CORRECT choice, and pinning
// the outer access method would make this test flaky/meaningless rather than
// a real regression guard. What must never regress is the inner lateral
// shape — Task 2's fix from a DISTINCT-ON-over-every-revision scan to one
// idx_ev_bitemporal probe per entity.
//
// It plans pitBaseQueryTemplate itself — the constant the production path
// runs — so a query edit moves the assertion with it.
func TestPITBaseQuery_ProbesPerEntity(t *testing.T) {
	factory := setupEntityTest(t)
	tenant := spi.TenantID("pit-plan-tenant")
	mref := spi.ModelRef{EntityName: "pit-plan", ModelVersion: "1"}

	// 200 entities is enough that a per-entity O(n) probe plan and an O(n^2)
	// scan-every-revision plan would visibly diverge in EXPLAIN's cost
	// estimate, and enough for ANALYZE to produce real (non-degenerate)
	// statistics on entity_versions instead of the near-empty-table numbers
	// that steered a related fixture onto the wrong index earlier in this
	// branch.
	seedPagePlanEntities(t, factory, tenant, mref, 200)

	pool := postgres.PoolForTest(factory)
	ctx := context.Background()
	// Stale statistics are the known trap here: a bulk INSERT with no
	// ANALYZE leaves the planner on pre-seed statistics, which steers it onto
	// idx_ev_model instead of idx_ev_bitemporal and turns the per-entity
	// probe into an O(n^2) scan. ANALYZE both tables the query touches so the
	// plan asserted below is the plan a real (analyzed) workload gets.
	if _, err := pool.Exec(ctx, "ANALYZE entities"); err != nil {
		t.Fatalf("ANALYZE entities: %v", err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE entity_versions"); err != nil {
		t.Fatalf("ANALYZE entity_versions: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	plan := explainPlan(t, ctx, conn, postgres.PITBaseQueryForTest,
		string(tenant), mref.EntityName, mref.ModelVersion, time.Now())

	if !strings.Contains(plan, "idx_ev_bitemporal") {
		t.Errorf("point-in-time read must probe idx_ev_bitemporal per entity; plan was:\n%s", plan)
	}
	if strings.Contains(plan, "Unique") {
		t.Errorf("plan still deduplicates revisions (DISTINCT ON shape); plan was:\n%s", plan)
	}
}
