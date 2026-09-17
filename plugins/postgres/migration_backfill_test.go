package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestMigration000012_BackfillsExistingRows proves the migration's backfill
// UPDATEs actually run against real data. Every other test in this package
// migrates an empty database, so both UPDATEs match zero rows on every run —
// the one genuinely risky part of this migration (a NULL landing in a NOT
// NULL column, aborting the migration and leaving schema_migrations dirty)
// is otherwise never exercised. This seeds three pre-existing rows at the
// version-11 schema — one with a complete _meta, one missing
// last_modified_date, one with no _meta at all — plus a fourth covering the
// present-but-empty-string case NULLIF exists to catch, then migrates to 12
// and asserts each column landed correctly rather than aborting or going
// NULL.
func TestMigration000012_BackfillsExistingRows(t *testing.T) {
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.MigrateToVersionForTest(pool, 11); err != nil {
		t.Fatalf("migrate to version 11: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	const tenant = "t-backfill"
	ctx := context.Background()

	seed := func(id, doc string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO entities (tenant_id, entity_id, model_name, model_version, version, deleted, doc)
			 VALUES ($1, $2, 'm', '1', 1, false, $3::jsonb)`, tenant, id, doc); err != nil {
			t.Fatalf("seed entities %s: %v", id, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO entity_versions (tenant_id, entity_id, model_name, model_version, version, valid_time, doc)
			 VALUES ($1, $2, 'm', '1', 1, CURRENT_TIMESTAMP, $3::jsonb)`, tenant, id, doc); err != nil {
			t.Fatalf("seed entity_versions %s: %v", id, err)
		}
	}

	// Complete: every field backfill can use is present.
	seed("bf-complete", `{"_meta":{"creation_date":"2020-01-01T00:00:00Z","last_modified_date":"2020-06-01T00:00:00Z","transaction_id":"tx-1"}}`)
	// Missing last_modified_date entirely — the shape that aborted the
	// migration before the COALESCE fix, since the brief's UPDATE assigned
	// this row's last_modified from a key that isn't there.
	seed("bf-missing-lm", `{"_meta":{"creation_date":"2020-02-02T00:00:00Z"}}`)
	// Present but empty — NULLIF turns "" into NULL; without the COALESCE
	// fallback that NULL lands directly in a NOT NULL column.
	seed("bf-empty-cd", `{"_meta":{"creation_date":""}}`)
	// No _meta at all — nothing to backfill from any key.
	seed("bf-no-meta", `{}`)

	if err := postgres.MigrateToVersionForTest(pool, 12); err != nil {
		t.Fatalf("migrate to version 12 (the migration under test): %v", err)
	}

	type entitiesRow struct {
		creationDate time.Time
		lastModified time.Time
	}
	readEntities := func(id string) entitiesRow {
		t.Helper()
		var r entitiesRow
		if err := pool.QueryRow(ctx,
			`SELECT creation_date, last_modified FROM entities WHERE tenant_id = $1 AND entity_id = $2`,
			tenant, id).Scan(&r.creationDate, &r.lastModified); err != nil {
			t.Fatalf("read entities %s: %v", id, err)
		}
		return r
	}
	readVersion := func(id string) (transactionID string, creationDate time.Time) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT transaction_id, creation_date FROM entity_versions WHERE tenant_id = $1 AND entity_id = $2`,
			tenant, id).Scan(&transactionID, &creationDate); err != nil {
			t.Fatalf("read entity_versions %s: %v", id, err)
		}
		return transactionID, creationDate
	}
	mustParse := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		return tm
	}

	// bf-no-meta had nothing to backfill anywhere, so every one of its date
	// columns stays at the migration's ALTER TABLE default — CURRENT_TIMESTAMP
	// is transaction-start time and this whole migration is one transaction,
	// so entities.creation_date, entities.last_modified, and
	// entity_versions.creation_date all share that one instant here. Reading
	// it back from this row, rather than asserting a specific wall-clock
	// value this test doesn't control, is what "not backfilled" looks like.
	baseline := readEntities("bf-no-meta")
	noMetaTxID, noMetaVersionCD := readVersion("bf-no-meta")
	if noMetaTxID != "" {
		t.Errorf("bf-no-meta entity_versions.transaction_id = %q, want empty (nothing to backfill from)", noMetaTxID)
	}
	if !noMetaVersionCD.Equal(baseline.creationDate) {
		t.Errorf("bf-no-meta entity_versions.creation_date = %v, entities.creation_date = %v, want equal — "+
			"both are the same untouched ALTER TABLE default", noMetaVersionCD, baseline.creationDate)
	}

	complete := readEntities("bf-complete")
	if !complete.creationDate.Equal(mustParse("2020-01-01T00:00:00Z")) {
		t.Errorf("bf-complete entities.creation_date = %v, want 2020-01-01", complete.creationDate)
	}
	if !complete.lastModified.Equal(mustParse("2020-06-01T00:00:00Z")) {
		t.Errorf("bf-complete entities.last_modified = %v, want 2020-06-01", complete.lastModified)
	}
	completeTxID, completeVersionCD := readVersion("bf-complete")
	if completeTxID != "tx-1" {
		t.Errorf("bf-complete entity_versions.transaction_id = %q, want %q", completeTxID, "tx-1")
	}
	if !completeVersionCD.Equal(mustParse("2020-01-01T00:00:00Z")) {
		t.Errorf("bf-complete entity_versions.creation_date = %v, want 2020-01-01", completeVersionCD)
	}

	missingLM := readEntities("bf-missing-lm")
	if !missingLM.creationDate.Equal(mustParse("2020-02-02T00:00:00Z")) {
		t.Errorf("bf-missing-lm entities.creation_date = %v, want 2020-02-02 (must still backfill the field that IS present)",
			missingLM.creationDate)
	}
	if !missingLM.lastModified.Equal(baseline.lastModified) {
		t.Errorf("bf-missing-lm entities.last_modified = %v, want the ALTER TABLE default %v — "+
			"there is no last_modified_date key to backfill from, so it must fall back, not go NULL or wrong",
			missingLM.lastModified, baseline.lastModified)
	}

	emptyCD := readEntities("bf-empty-cd")
	if !emptyCD.creationDate.Equal(baseline.creationDate) {
		t.Errorf("bf-empty-cd entities.creation_date = %v, want the ALTER TABLE default %v — "+
			"an empty string must fall back through COALESCE, not NULLIF its way into NULL",
			emptyCD.creationDate, baseline.creationDate)
	}
	_, emptyVersionCD := readVersion("bf-empty-cd")
	if !emptyVersionCD.Equal(baseline.creationDate) {
		t.Errorf("bf-empty-cd entity_versions.creation_date = %v, want the ALTER TABLE default %v",
			emptyVersionCD, baseline.creationDate)
	}
}
