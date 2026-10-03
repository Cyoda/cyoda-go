package help

import (
	"io/fs"
	"strings"
	"testing"
)

// TestHelpContent_MigrateTopicCarriesTheDirtyRecovery — the postgres and sqlite
// plugins both refuse to start on a dirty schema and both tell the operator to
// run `cyoda help cli migrate` for the recovery procedure. That pointer only
// earns its place if the topic carries one: an operator holding a half-migrated
// database is the least good audience for a dead end.
func TestHelpContent_MigrateTopicCarriesTheDirtyRecovery(t *testing.T) {
	data, err := fs.ReadFile(embeddedContent, "content/cli/migrate.md")
	if err != nil {
		t.Fatalf("read the migrate topic: %v", err)
	}
	body := string(data)

	for _, want := range []string{
		// The refusal an operator arrives with, so the topic is searchable by it.
		"database migration state is dirty",
		// Clearing the flag, which is the whole of the generic recovery.
		"schema_migrations",
		// The PostgreSQL-specific cause and its cleanup.
		"CREATE INDEX CONCURRENTLY",
		"indisvalid",
		"DROP INDEX CONCURRENTLY",
		// The convention the guard in the postgres plugin enforces.
		"## ADDING AN INDEX MIGRATION",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the migrate topic is missing %q, so the refusal's pointer resolves to nothing useful", want)
		}
	}
}
