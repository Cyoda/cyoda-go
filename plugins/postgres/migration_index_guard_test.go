package postgres

// migration_index_guard_test.go — a static rule over the embedded up-migrations,
// not a database test. It guards the convention an index migration must follow;
// see checkIndexRules for the two clauses and the tests below for what each one
// is worth.

import (
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestMigrations_IndexesOnExistingTablesAreConcurrent enforces two clauses over
// the embedded up-migrations.
//
// (a) An index added to a table created in an EARLIER migration must be
// CONCURRENTLY. A plain CREATE INDEX takes SHARE, which conflicts with the ROW
// EXCLUSIVE every INSERT/UPDATE/DELETE holds — it locks writers out for the
// whole build. An index created in the same migration as its own table need not
// be: that table is empty and unreachable by writers, which is why 000001's
// indexes pass on their merits rather than by exemption.
//
// (b) A file containing CREATE INDEX CONCURRENTLY must contain no other
// statement. The driver sends the whole file through one Exec with
// MultiStatementEnabled false, and PostgreSQL wraps a multi-statement simple
// query in an implicit transaction, in which CREATE INDEX CONCURRENTLY cannot
// run. 000002_grouped_stats.up.sql is the proof — a function plus an index in
// one file — and is the sole grandfathered entry.
func TestMigrations_IndexesOnExistingTablesAreConcurrent(t *testing.T) {
	// The only pre-existing violation. Adding to this list is a decision, not a
	// convenience: it means shipping a migration that locks writers out of a
	// populated table for the duration of an index build.
	grandfathered := map[string]bool{
		"000002_grouped_stats.up.sql": true,
	}

	for _, v := range checkIndexRules(upMigrations(t), grandfathered) {
		t.Error(v)
	}
}

// TestMigrations_GuardRejectsANewNonConcurrentIndex proves the guard has teeth
// by running the same rule over a synthetic file set.
func TestMigrations_GuardRejectsANewNonConcurrentIndex(t *testing.T) {
	violations := checkIndexRules([]migrationFile{
		{name: "000001_init.up.sql", body: "CREATE TABLE entities (id uuid);"},
		{name: "000007_new_index.up.sql", body: "CREATE INDEX entities_foo_idx ON entities (foo);"},
	}, nil)
	if len(violations) != 1 {
		t.Fatalf("guard did not reject a non-concurrent index on a hot table: %v", violations)
	}
}

// TestMigrations_GuardRejectsConcurrentIndexSharingAFile covers clause (b).
func TestMigrations_GuardRejectsConcurrentIndexSharingAFile(t *testing.T) {
	violations := checkIndexRules([]migrationFile{
		{name: "000007_two.up.sql", body: "CREATE FUNCTION f() RETURNS int AS $$ SELECT 1 $$ LANGUAGE sql;\nCREATE INDEX CONCURRENTLY x ON entities (foo);"},
	}, nil)
	if len(violations) != 1 {
		t.Fatalf("guard accepted CONCURRENTLY sharing a file: %v", violations)
	}
}

// --- the rule -----------------------------------------------------------------

// migrationFile is one up-migration: its base name and its SQL body.
type migrationFile struct {
	name string
	body string
}

// indexStatement is one CREATE INDEX the rule found — the table it targets, and
// whether it was declared CONCURRENTLY.
type indexStatement struct {
	table      string
	concurrent bool
}

// The patterns are deliberately simple. They guard a convention over the files
// in migrations/, not arbitrary SQL: every one of them is a plain CREATE with at
// most the optional modifiers already in use. Case-insensitive throughout, since
// nothing stops a future migration shouting its keywords.
var (
	createTableRe = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
	createIndexRe = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+(CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?[a-z_][a-z0-9_]*\s+ON\s+([a-z_][a-z0-9_]*)`)

	blockCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineCommentRe  = regexp.MustCompile(`--[^\n]*`)
)

// checkIndexRules applies both clauses to files, which must be in version order,
// and returns one string per violation. An empty result is a clean tree.
//
// A table the file set never creates is treated as pre-existing — the fail-closed
// reading, and the right one: the guard cannot know that a table it never saw
// created is empty.
func checkIndexRules(files []migrationFile, grandfathered map[string]bool) []string {
	var violations []string
	tableOrigin := map[string]string{} // table name -> file that created it

	for _, f := range files {
		sql := stripComments(f.body)
		for _, tbl := range createdTables(sql) {
			if _, seen := tableOrigin[tbl]; !seen {
				tableOrigin[tbl] = f.name
			}
		}

		for _, idx := range createIndexStatements(sql) {
			if idx.concurrent {
				// Clause (b).
				if n := statementCount(sql); n > 1 {
					violations = append(violations, fmt.Sprintf(
						"%s: CREATE INDEX CONCURRENTLY shares a file with %d other statement(s); "+
							"the driver sends the file as one simple query, whose implicit transaction "+
							"forbids CONCURRENTLY at runtime", f.name, n-1))
				}
				continue
			}
			// Clause (a).
			if origin, ok := tableOrigin[idx.table]; ok && origin == f.name {
				continue // same migration as its own table: empty, unreachable
			}
			if grandfathered[f.name] {
				continue
			}
			violations = append(violations, fmt.Sprintf(
				"%s: CREATE INDEX on %q, which was created in an earlier migration — "+
					"use CREATE INDEX CONCURRENTLY, alone in its own migration file",
				f.name, idx.table))
		}
	}
	return violations
}

// upMigrations reads the embedded up-migrations in version order. golang-migrate
// zero-pads the version prefix, so lexical order is version order.
func upMigrations(t *testing.T) []migrationFile {
	t.Helper()
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	var files []migrationFile
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		body, err := fs.ReadFile(migrationFS, "migrations/"+e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		files = append(files, migrationFile{name: e.Name(), body: string(body)})
	}
	if len(files) == 0 {
		t.Fatal("no up-migrations found; the guard would pass vacuously")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files
}

// createdTables names every table the SQL creates, lowercased.
func createdTables(sql string) []string {
	var out []string
	for _, m := range createTableRe.FindAllStringSubmatch(sql, -1) {
		out = append(out, strings.ToLower(m[1]))
	}
	return out
}

// createIndexStatements returns every CREATE INDEX in the SQL.
func createIndexStatements(sql string) []indexStatement {
	var out []indexStatement
	for _, m := range createIndexRe.FindAllStringSubmatch(sql, -1) {
		out = append(out, indexStatement{
			table:      strings.ToLower(m[2]),
			concurrent: strings.TrimSpace(m[1]) != "",
		})
	}
	return out
}

// statementCount counts semicolon-terminated statements. A file that pairs
// CONCURRENTLY with anything else overcounts rather than undercounts — a
// dollar-quoted body's own semicolons inflate the total — and overcounting only
// ever makes the guard stricter about a file that is already a violation.
func statementCount(sql string) int {
	n := 0
	for _, stmt := range strings.Split(sql, ";") {
		if strings.TrimSpace(stmt) != "" {
			n++
		}
	}
	return n
}

// stripComments removes block and line comments so neither the patterns nor the
// statement count sees SQL that PostgreSQL never will.
func stripComments(sql string) string {
	return lineCommentRe.ReplaceAllString(blockCommentRe.ReplaceAllString(sql, ""), "")
}
