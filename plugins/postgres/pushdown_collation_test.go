package postgres_test

// pushdown_collation_test.go — Task 13: the WHERE-clause side of a text
// ordering comparison (Gt/Lt/Gte/Lte, and BETWEEN/BETWEEN_INCLUSIVE) must
// compare the way the kernel does (byte order), not the way the database's
// own default collation happens to.
//
// query_planner.go's orderingOp and the text branch of leafToSQL's
// FilterBetween/FilterBetweenInclusive case build those WHERE fragments;
// searcher.go's orderByFieldExpr has said COLLATE "C" on the equivalent
// ORDER BY expression from the start, precisely because the kernel
// (spi.Prepare/PreparedFilter.Match) compares strings with Go's
// strings.Compare — byte order — and a database whose default collation is
// NOT "C" can disagree with that. Before this task, neither WHERE-clause
// site carried such collation, so under a non-C database collation a
// narrowing WHERE range could disagree with the kernel and drop a row the
// kernel would have matched. That under-select is unrecoverable: a row the
// WHERE excludes is never fetched from the database, so the Go-side
// postFilter re-check (see soundness_property_test.go's doc comment) never
// gets a chance to save it.
//
// Equality (FilterEq/FilterNe) is NOT collation-sensitive and carries no
// COLLATE "C" — see the comment at that branch in query_planner.go for why.
//
// This matters more after the write-path change admitting date-shaped
// strings into [STRING] leaves (spec §7 "Pushdown"): text comparisons now
// routinely run over punctuation-heavy ISO-8601-shaped values, exactly the
// kind of string where ICU collation and byte order are most likely to
// diverge.

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go/plugins/postgres"
)

// freshICUDatabase creates a scratch database on the test server whose
// DEFAULT collation is ICU's root ("und") collation rather than the
// server's own default. Under the postgres:17-alpine image this suite and
// CI both run against, the server's own default is unavoidably "C" in
// practice: the image's musl libc carries no real locale data, so every
// libc-provided collation (LC_COLLATE/LOCALE) behaves as "C" regardless of
// what name is requested. ICU is a bundled library, not an OS locale, so
// its root collation is available even on musl — giving this file a
// genuinely non-C collation to test the fix against.
//
// TEMPLATE template0 is required: template1 (and the implicit default
// template) carry the cluster's own locale provider, and CREATE DATABASE
// refuses to switch locale provider against a template that doesn't share
// it.
//
// This deliberately duplicates the shape of migrate_concurrency_test.go's
// freshDatabase rather than reusing it: that helper lives in the internal
// `postgres` test package (unexported, not reachable from this `postgres_test`
// package) and does not parameterise the locale provider — this one only
// ever needs ICU.
func freshICUDatabase(t *testing.T) string {
	t.Helper()
	base := testDBURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	t.Cleanup(admin.Close)

	name := "cyoda_icu_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	quoted := pgx.Identifier{name}.Sanitize()
	ddl := "CREATE DATABASE " + quoted + " TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER 'icu' ICU_LOCALE 'und'"
	if _, err := admin.Exec(ctx, ddl); err != nil {
		t.Fatalf("create ICU-collated database %s: %v", name, err)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})

	u, err := url.Parse(base)
	if err != nil {
		// *url.Error renders the raw URL (Op, URL, and Err); a malformed
		// CYODA_TEST_DB_URL could carry an embedded credential, and that
		// string would otherwise land in CI logs verbatim. Unwrap to just
		// the underlying parse error — it names what's wrong without
		// repeating the URL (security review L4).
		t.Fatalf("parse CYODA_TEST_DB_URL: %v", errors.Unwrap(err))
	}
	u.Path = "/" + name
	return u.String()
}

// fStrBetween builds a SourceData BETWEEN/BETWEEN_INCLUSIVE leaf over a
// string-declared field. Mirrors soundness_property_test.go's fNumBetween,
// for text bounds instead of numeric ones (no such helper existed there —
// that file's BETWEEN coverage is numeric/temporal only).
func fStrBetween(op spi.FilterOp, path string, lo, hi any) spi.Filter {
	return spi.Filter{Op: op, Source: spi.SourceData, Path: path, Values: []any{lo, hi}, Declared: []spi.DataType{spi.String}}
}

// TestPostgresPushdownSoundness_TextRangeUnderNonCCollation is the Task 13
// regression test. It covers both collation-sensitive text ordering shapes
// leafToSQL pushes: a Gte comparison (orderingOp) and an inclusive BETWEEN
// (leafToSQL's FilterBetweenInclusive case), as subtests sharing one
// ICU-collated fixture.
//
// The two stored values below are date-shaped strings — the shape this
// change (admitting date-shaped strings into [STRING] leaves) newly fills
// text leaves with — differing only in the case of the trailing UTC
// marker ('Z' vs 'z'). Byte order (ASCII 'Z'=0x5A=90 < 'z'=0x7A=122) puts
// the lowercase variant strictly AFTER the uppercase one. The default
// Unicode Collation Algorithm ICU implements ties same-base-letter case
// pairs at the tertiary level with lowercase sorting BEFORE uppercase —
// the opposite order — which is exactly the ICU/byte-order divergence
// this test needs. The self-check below pins that assumption against the
// real database before relying on it, so a change in ICU's default
// tie-break (or an image without ICU support) fails loudly and
// specifically here rather than as a confusing property-test result.
func TestPostgresPushdownSoundness_TextRangeUnderNonCCollation(t *testing.T) {
	dsn := freshICUDatabase(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to ICU-collated database: %v", err)
	}
	defer pool.Close()

	const upper = "2024-01-15T10:00:00Z"
	const lower = "2024-01-15T10:00:00z"

	// Self-check 1: under the database's own default (ICU root) collation,
	// lower must sort BEFORE upper.
	var icuLowerFirst bool
	if err := pool.QueryRow(ctx, "SELECT $1::text < $2::text", lower, upper).Scan(&icuLowerFirst); err != nil {
		t.Fatalf("collation self-check query: %v", err)
	}
	if !icuLowerFirst {
		t.Fatalf("test fixture invalid: expected the database's default (ICU root) collation to sort %q before %q (lowercase-before-uppercase tie-break), got the opposite — this database is not usefully non-C-collated and the assertions below would be vacuous", lower, upper)
	}

	// Self-check 2: under COLLATE "C" (byte order), lower must sort AFTER
	// upper — 'z' (0x7A) > 'Z' (0x5A) — the opposite of self-check 1. This
	// pins the actual divergence the fix (and this test) cares about.
	var cLowerFirst bool
	if err := pool.QueryRow(ctx, `SELECT ($1::text COLLATE "C") < ($2::text COLLATE "C")`, lower, upper).Scan(&cLowerFirst); err != nil {
		t.Fatalf("byte-order self-check query: %v", err)
	}
	if cLowerFirst {
		t.Fatalf("test fixture invalid: expected byte order (COLLATE \"C\") to sort %q AFTER %q ('z'=0x7A > 'Z'=0x5A), got the opposite", lower, upper)
	}

	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migrate ICU-collated database: %v", err)
	}

	factory := postgres.NewStoreFactory(pool)
	const tenant spi.TenantID = "icu-collation-tenant"
	fctx := ctxWithTenant(tenant)
	lockModels(t, factory, fctx, gsModel)
	store, err := factory.EntityStore(fctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	gsSave(t, fctx, store, "e-upper", "available", map[string]any{"value": upper})
	gsSave(t, fctx, store, "e-lower", "available", map[string]any{"value": lower})

	// A minimal in-process corpus mirroring what buildSoundnessCorpus does —
	// the entities are trivial enough (a single string field) that a
	// hand-built Data payload is exact, no read-back needed.
	corpus := []*spi.Entity{
		{Meta: spi.EntityMeta{ID: "e-upper"}, Data: []byte(`{"value":"` + upper + `"}`)},
		{Meta: spi.EntityMeta{ID: "e-lower"}, Data: []byte(`{"value":"` + lower + `"}`)},
	}

	// value >= upper: byte order (the kernel) says BOTH match — e-upper
	// exactly, e-lower because 'z' > 'Z' makes its value byte-greater.
	filter := fStr(spi.FilterGte, "value", upper)
	oracle := oracleIDs(t, corpus, filter)
	if !oracle["e-upper"] || !oracle["e-lower"] {
		t.Fatalf("test setup invalid: kernel must match both e-upper (equal) and e-lower (byte order 'z' > 'Z') against a >= %q filter, got oracle=%v", upper, sortedKeys(oracle))
	}

	// value BETWEEN upper AND lower (inclusive): in BYTE order upper <=
	// lower ('Z' < 'z'), so this is a well-formed, non-empty range whose
	// bounds are exactly the two stored values — the kernel matches both
	// (each equals one bound). Under the database's ICU default collation
	// the SAME two bind values are in the OPPOSITE order (lower collates
	// before upper), so an un-collated SQL BETWEEN would test "col BETWEEN
	// (ICU-greater) AND (ICU-lesser)" — a range no value can ever satisfy,
	// under-selecting BOTH rows to zero rather than just one.
	betweenFilter := fStrBetween(spi.FilterBetweenInclusive, "value", upper, lower)
	betweenOracle := oracleIDs(t, corpus, betweenFilter)
	if !betweenOracle["e-upper"] || !betweenOracle["e-lower"] {
		t.Fatalf("test setup invalid: kernel must match both e-upper and e-lower (each equals one bound in byte order) against a BETWEEN %q AND %q filter, got oracle=%v", upper, lower, sortedKeys(betweenOracle))
	}

	for _, tc := range []struct {
		name   string
		filter spi.Filter
		oracle map[string]bool
	}{
		{"gte", filter, oracle},
		{"between_inclusive", betweenFilter, betweenOracle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Assertion 1: the raw SQL candidate set (the WHERE fragment
			// orderingOp/leafToSQL's BETWEEN branch builds, before any
			// Go-side postFilter re-check) must be a superset of the
			// kernel's true matches. Before COLLATE "C", the database's ICU
			// default collation disagrees with byte order here, so the
			// pushed comparison excludes a row the kernel matches — an
			// under-select the re-check stage can never recover, because
			// the row is never fetched.
			candidateIDs, err := postgres.SearchCandidateIDsForTest(pool, fctx, tenant, gsModel.EntityName, gsModel.ModelVersion, tc.filter)
			if err != nil {
				t.Fatalf("SearchCandidateIDsForTest: %v", err)
			}
			candidates := idSetFromStrings(candidateIDs)
			for id := range tc.oracle {
				if !candidates[id] {
					t.Errorf("UNDER-SELECT under non-C collation: kernel matches %q but the raw SQL candidate set does not contain it (WHERE evaluated under the database's default ICU collation instead of byte order)\n  candidates=%v", id, sortedKeys(candidates))
				}
			}

			// Assertion 2: the full Search() pipeline (WHERE + postFilter
			// re-check) must equal the kernel's true matches exactly.
			results, err := store.Search(fctx, tc.filter, spi.SearchOptions{ModelName: gsModel.EntityName, ModelVersion: gsModel.ModelVersion, Limit: 10})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			actual := idSetFromEntities(results)
			for id := range tc.oracle {
				if !actual[id] {
					t.Errorf("UNDER-SELECT (survived to Search()) under non-C collation: kernel matches %q, backend Search() did not return it", id)
				}
			}
			for id := range actual {
				if !tc.oracle[id] {
					t.Errorf("OVER-SELECT: backend Search() returned %q, kernel does not match it", id)
				}
			}
		})
	}
}
