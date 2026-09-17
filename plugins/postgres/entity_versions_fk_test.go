package postgres_test

import (
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestEntityVersionsRequireAnEntityRow proves the foreign key holds. A version
// row without its entity row is invisible to every point-in-time read — the
// read enumerates entities and probes each one's revision — so it must be
// impossible to create, not merely absent by convention. A fixture in this
// repository violated the convention before the constraint existed, and
// nothing detected it until a query change made the rows disappear.
func TestEntityVersionsRequireAnEntityRow(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("t-fk")
	pool := postgres.PoolForTest(factory)

	_, err := pool.Exec(ctx,
		`INSERT INTO entity_versions
		   (tenant_id, entity_id, model_name, model_version, version, valid_time, creation_date, doc)
		 VALUES ('t-fk', 'orphan-id', 'm', '1', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, '{"_meta":{}}'::jsonb)`)
	if err == nil {
		t.Fatal("an entity_versions row with no entities row must be rejected by the foreign key")
	}
	if !strings.Contains(err.Error(), "entity_versions_entity_fk") {
		t.Errorf("expected the foreign key to reject it, got: %v", err)
	}
	t.Logf("orphan insert rejected: %v", err)
}
