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

// TestEntitiesModelEntityIDIndex_HasNoPartialPredicate is the schema-property
// counterpart to TestPITBaseQuery_ProbesPerEntity above: that test guards the
// INNER lateral shape and, by design, passes regardless of whether
// idx_entities_model_entity_id is partial — a partial-vs-full index doesn't
// change which entity_versions index the lateral picks. So nothing in this
// file failed the reason Task 3 exists: a point-in-time read enumerates
// entities from the OUTER `entities` table, and it must see entities deleted
// SINCE the requested instant — rows whose CURRENT state carries
// deleted = true. A `WHERE NOT deleted` predicate on this index would silently
// exclude exactly those rows from ever being considered by the outer scan,
// which is a correctness gap, not a cost one.
//
// Asserting the planner's chosen access method would be the wrong way to
// catch that regression — on a small, freshly-ANALYZEd table a sequential
// scan is the planner's correct choice regardless of which indexes exist,
// and seeding enough rows to force an index scan every CI run would be
// disproportionate. The schema property is deterministic and cheap instead:
// query the index's own definition and assert it carries no WHERE clause at
// all, independent of table size or planner statistics.
func TestEntitiesModelEntityIDIndex_HasNoPartialPredicate(t *testing.T) {
	factory := setupEntityTest(t)
	pool := postgres.PoolForTest(factory)
	ctx := context.Background()

	var indexdef string
	err := pool.QueryRow(ctx,
		"SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND indexname = $1",
		"idx_entities_model_entity_id").Scan(&indexdef)
	if err != nil {
		t.Fatalf("look up idx_entities_model_entity_id: %v", err)
	}

	if strings.Contains(strings.ToUpper(indexdef), "WHERE") {
		t.Errorf("idx_entities_model_entity_id must have no WHERE clause: a partial index here "+
			"cannot serve a point-in-time read, because such a read must enumerate entities that "+
			"have since been deleted; got definition:\n%s", indexdef)
	}
}
