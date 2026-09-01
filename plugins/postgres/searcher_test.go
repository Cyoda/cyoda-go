package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

var searchModel = spi.ModelRef{EntityName: "person", ModelVersion: "1"}

// setupSearcher seeds five people and returns a store + ctx. Numeric `age`
// drives EQ/GT/BETWEEN through cyoda_try_float8 (S4: numeric paths must run
// against a live backend, not just string-asserted).
func setupSearcher(t *testing.T) (spi.EntityStore, context.Context) {
	t.Helper()
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("search-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	seed := []struct {
		id, data string
	}{
		{"e1", `{"name":"Alice","age":30,"city":"Berlin"}`},
		{"e2", `{"name":"Bob","age":25,"city":"Munich"}`},
		{"e3", `{"name":"Charlie","age":35,"city":"Berlin"}`},
		{"e4", `{"name":"Diana","age":28,"city":"Hamburg"}`},
		{"e5", `{"name":"Eve","age":40,"city":"Munich"}`},
	}
	for _, s := range seed {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: s.id, ModelRef: searchModel, State: "NEW"},
			Data: []byte(s.data),
		}); err != nil {
			t.Fatalf("Save %s: %v", s.id, err)
		}
	}
	return store, ctx
}

func searcherOf(t *testing.T, store spi.EntityStore) spi.Searcher {
	t.Helper()
	sr, ok := store.(spi.Searcher)
	if !ok {
		t.Fatal("postgres entityStore does not implement spi.Searcher")
	}
	return sr
}

// searcherBaseLimit is generously above every baseOpts() caller's expected
// match count (setupSearcher seeds 5, the largest seed among them) — these
// tests exercise filtering/ordering, not the bounded-or-fail cap itself
// (that is boundedOpts's job below), so the Limit here only needs to never
// be the thing that trips.
const searcherBaseLimit = 100

func baseOpts() spi.SearchOptions {
	return spi.SearchOptions{ModelName: "person", ModelVersion: "1", Limit: searcherBaseLimit}
}

func TestPGSearcher_Eq(t *testing.T) {
	store, ctx := setupSearcher(t)
	got, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin", Declared: []spi.DataType{spi.String}},
		baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("city=Berlin: want 2, got %d", len(got))
	}
}

// TestPGSearcher_RejectsUnevaluableFilter pins the propagation of
// spi.Prepare's error through Search: a leaf spi.Prepare genuinely cannot
// evaluate (a LIKE pattern with a trailing unpaired escape, which will not
// compile) must fail the search outright, not silently degrade to an empty
// page. See .claude/rules/correctness-over-availability.md. Both malformed
// operands `spitest`'s `Pattern/MalformedLike` conformance case requires an
// error for (a trailing escape after a literal, and a bare trailing escape)
// are exercised here too, pinning the same requirement at this plugin's own
// Search boundary.
func TestPGSearcher_RejectsUnevaluableFilter(t *testing.T) {
	for _, operand := range []string{`a\`, `\`} {
		t.Run(operand, func(t *testing.T) {
			store, ctx := setupSearcher(t)
			_, err := searcherOf(t, store).Search(ctx,
				spi.Filter{
					Op: spi.FilterLike, Source: spi.SourceData, Path: "name",
					Value: operand, Declared: []spi.DataType{spi.String},
				}, baseOpts())
			if err == nil {
				t.Fatal("Search must fail on an unevaluable filter, not return an empty page")
			}
			if !errors.Is(err, spi.ErrUnevaluableLeaf) {
				t.Errorf("err = %v, want errors.Is(err, spi.ErrUnevaluableLeaf)", err)
			}
		})
	}
}

func TestPGSearcher_GtNumeric(t *testing.T) {
	store, ctx := setupSearcher(t)
	got, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterGt, Path: "age", Source: spi.SourceData, Value: float64(30), Declared: []spi.DataType{spi.Integer}},
		baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 { // Charlie 35, Eve 40
		t.Fatalf("age>30: want 2, got %d", len(got))
	}
}

func TestPGSearcher_BetweenNumeric(t *testing.T) {
	store, ctx := setupSearcher(t)
	got, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterBetween, Path: "age", Source: spi.SourceData,
			Values: []any{float64(28), float64(35)}, Declared: []spi.DataType{spi.Integer}},
		baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Kernel-authoritative: FilterBetween is EXCLUSIVE (28 < age < 35). The
	// inclusive SQL BETWEEN pushed as a sound superset is re-checked by the
	// kernel, which drops the boundary rows (28, 35). Only age 30 (Alice)
	// remains. (An inclusive range would use FilterBetweenInclusive.)
	if len(got) != 1 {
		t.Fatalf("age in (28,35) exclusive: want 1, got %d", len(got))
	}
}

func TestPGSearcher_And(t *testing.T) {
	store, ctx := setupSearcher(t)
	got, err := searcherOf(t, store).Search(ctx, spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin", Declared: []spi.DataType{spi.String}},
			{Op: spi.FilterGt, Path: "age", Source: spi.SourceData, Value: float64(31), Declared: []spi.DataType{spi.Integer}},
		},
	}, baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 { // Charlie (Berlin, 35)
		t.Fatalf("Berlin AND age>31: want 1, got %d", len(got))
	}
}

func TestPGSearcher_Or(t *testing.T) {
	store, ctx := setupSearcher(t)
	got, err := searcherOf(t, store).Search(ctx, spi.Filter{
		Op: spi.FilterOr,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Hamburg", Declared: []spi.DataType{spi.String}},
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Munich", Declared: []spi.DataType{spi.String}},
		},
	}, baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 3 { // Bob, Eve (Munich), Diana (Hamburg)
		t.Fatalf("Hamburg OR Munich: want 3, got %d", len(got))
	}
}

// FilterMatchesRegex is non-pushable → becomes a residual evaluated by
// postgresIter/evalPostFilter. Proves the residual path runs under Search.
func TestPGSearcher_ResidualRegex(t *testing.T) {
	store, ctx := setupSearcher(t)
	// MATCHES_PATTERN is anchored to a whole-string match by the kernel (Cloud
	// Pattern.matcher(x).matches() semantics), so "A.*" (not the prefix "^A")
	// expresses "name starts with A".
	got, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: "A.*"},
		baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 { // Alice
		t.Fatalf("name ~ A.*: want 1, got %d", len(got))
	}
}

// Mixed: pushable city=Berlin AND residual regex on name.
func TestPGSearcher_MixedPushAndResidual(t *testing.T) {
	store, ctx := setupSearcher(t)
	got, err := searcherOf(t, store).Search(ctx, spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin", Declared: []spi.DataType{spi.String}},
			// Anchored whole-string match: "C.*" expresses "name starts with C".
			{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: "C.*"},
		},
	}, baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 { // Charlie
		t.Fatalf("Berlin AND name~C.*: want 1, got %d", len(got))
	}
}

func TestPGSearcher_OrderByDataPathDesc(t *testing.T) {
	store, ctx := setupSearcher(t)
	opts := baseOpts()
	opts.OrderBy = []spi.OrderSpec{{Path: "name", Source: spi.SourceData, Desc: true, Kind: spi.OrderText}}
	got, err := searcherOf(t, store).Search(ctx, spi.Filter{}, opts)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("want 5, got %d", len(got))
	}
	// Lexicographic desc: Eve, Diana, Charlie, Bob, Alice.
	if got[0].Meta.ID != "e5" {
		t.Errorf("desc by name: want first=Eve(e5), got id=%s", got[0].Meta.ID)
	}
}

func TestPGSearcher_OrderByDataPathAsc(t *testing.T) {
	store, ctx := setupSearcher(t)
	opts := baseOpts()
	opts.OrderBy = []spi.OrderSpec{{Path: "name", Source: spi.SourceData, Kind: spi.OrderText}} // Desc omitted → ascending
	got, err := searcherOf(t, store).Search(ctx, spi.Filter{}, opts)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("asc by name: want 5, got %d", len(got))
	}
	// Lexicographic asc: Alice, Bob, Charlie, Diana, Eve → e1 first, e5 last.
	if got[0].Meta.ID != "e1" {
		t.Errorf("asc by name: want first=Alice(e1), got id=%s", got[0].Meta.ID)
	}
	if got[4].Meta.ID != "e5" {
		t.Errorf("asc by name: want last=Eve(e5), got id=%s", got[4].Meta.ID)
	}
}

func TestPGSearcher_OrderByMetaDirectColumn(t *testing.T) {
	// "id" is the canonical meta sort name for the entity identifier; the
	// implementation maps it to the entity_id column directly (not a JSONB
	// extraction). Exercises the special-case branch in orderByFieldExpr.
	store, ctx := setupSearcher(t)
	opts := baseOpts()
	opts.OrderBy = []spi.OrderSpec{{Path: "id", Source: spi.SourceMeta, Kind: spi.OrderText}}
	got, err := searcherOf(t, store).Search(ctx, spi.Filter{}, opts)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("asc by id: want 5, got %d", len(got))
	}
	// Lexicographic asc over "e1".."e5" → e1 first, e5 last.
	if got[0].Meta.ID != "e1" {
		t.Errorf("asc by id: want first=e1, got %s", got[0].Meta.ID)
	}
	if got[4].Meta.ID != "e5" {
		t.Errorf("asc by id: want last=e5, got %s", got[4].Meta.ID)
	}
}

func TestPGSearcher_OrderByMetaJSONPath(t *testing.T) {
	// "state" maps to _meta.state key; ordering uses COLLATE "C" (OrderText).
	// All five seed rows have state="NEW", so ordering is uniform; this test
	// confirms the meta-JSONPath ORDER BY expression is valid SQL without error.
	store, ctx := setupSearcher(t)
	opts := baseOpts()
	opts.OrderBy = []spi.OrderSpec{{Path: "state", Source: spi.SourceMeta, Kind: spi.OrderText}}
	got, err := searcherOf(t, store).Search(ctx, spi.Filter{}, opts)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("order by meta state: want 5, got %d", len(got))
	}
}

// assertIDOrder checks that got matches want by entity ID position.
func assertIDOrder(t *testing.T, got []*spi.Entity, want []string) {
	t.Helper()
	gotIDs := make([]string, len(got))
	for i, e := range got {
		gotIDs[i] = e.Meta.ID
	}
	if len(gotIDs) != len(want) {
		t.Fatalf("length mismatch: got %v (len=%d), want %v (len=%d)", gotIDs, len(gotIDs), want, len(want))
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Errorf("wrong order: got %v, want %v", gotIDs, want)
			return
		}
	}
}

// TestPGSearcher_OrderByNumericData verifies that OrderNumeric applies
// cyoda_try_float8 so that string-encoded numbers sort by magnitude, not
// byte order. Without the cast: "10" < "100" < "9" (lexical). With the
// cast: 9 < 10 < 100 (numeric) → order a, b, c.
func TestPGSearcher_OrderByNumericData(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("orderby-numeric-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	for _, e := range []struct{ id, data string }{
		{"a", `{"n":"9"}`},
		{"b", `{"n":"10"}`},
		{"c", `{"n":"100"}`},
	} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: e.id, ModelRef: ref, State: "NEW"},
			Data: []byte(e.data),
		}); err != nil {
			t.Fatalf("Save %s: %v", e.id, err)
		}
	}
	sr := store.(spi.Searcher)
	results, err := sr.Search(ctx,
		spi.Filter{Op: spi.FilterNotNull, Path: "n", Source: spi.SourceData},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			Limit:        searcherBaseLimit,
			OrderBy:      []spi.OrderSpec{{Path: "n", Source: spi.SourceData, Kind: spi.OrderNumeric}},
		})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Numeric: 9 < 10 < 100 → a, b, c.
	assertIDOrder(t, results, []string{"a", "b", "c"})
}

// TestPGSearcher_OrderByCreationDateMeta verifies that the canonical meta path
// "creationDate" is mapped to the blob key "creation_date", enabling temporal
// ordering by entity creation time.
//
// Entity IDs are assigned so that chronological (creationDate) order DIFFERS
// from entity_id lexicographic order: "z" is oldest, "m" is middle, "a" is
// newest. Expected ascending result is [z, m, a]. If the meta key mapping is
// wrong (returns NULL for all rows), the entity_id tiebreaker takes over and
// produces [a, m, z] — the test would FAIL, proving genuine RED capability.
func TestPGSearcher_OrderByCreationDateMeta(t *testing.T) {
	factory := setupEntityTest(t)
	const tenant = "orderby-creation-tenant"
	ctx := ctxWithTenant(spi.TenantID(tenant))
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	for _, id := range []string{"z", "m", "a"} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: ref, State: "NEW"},
			Data: []byte(`{"v":1}`),
		}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}
	// Patch creation_date directly so each entity has a distinct, ordered
	// timestamp (instants ≥ 1 ms apart). "z" is oldest, "a" is newest —
	// the inverse of entity_id lexicographic order.
	pool := factory.Pool()
	for _, pair := range []struct{ id, ts string }{
		{"z", "2020-01-01T00:00:00.000Z"},
		{"m", "2020-06-15T12:00:00.000Z"},
		{"a", "2021-01-01T00:00:00.000Z"},
	} {
		if _, err := pool.Exec(ctx,
			`UPDATE entities SET doc = jsonb_set(doc, '{_meta,creation_date}', to_jsonb($1::text))
			 WHERE tenant_id = $2 AND entity_id = $3`,
			pair.ts, tenant, pair.id); err != nil {
			t.Fatalf("patch creation_date %s: %v", pair.id, err)
		}
	}
	sr := store.(spi.Searcher)
	results, err := sr.Search(ctx,
		spi.Filter{Op: spi.FilterNotNull, Path: "v", Source: spi.SourceData},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			Limit:        searcherBaseLimit,
			OrderBy:      []spi.OrderSpec{{Path: "creationDate", Source: spi.SourceMeta, Kind: spi.OrderTemporal}},
		})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Chronological ascending: z (2020-01-01), m (2020-06-15), a (2021-01-01).
	// If mapping were wrong (all-NULL → entity_id tiebreaker): a, m, z — FAIL.
	assertIDOrder(t, results, []string{"z", "m", "a"})
}

// TestPGSearcher_OrderByStateMeta verifies that meta state field sorts
// correctly using COLLATE "C" (OrderText).
func TestPGSearcher_OrderByStateMeta(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("orderby-state-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	for _, e := range []struct {
		id    string
		state string
	}{
		{"pending-ent", "PENDING"},
		{"active-ent", "ACTIVE"},
		{"new-ent", "NEW"},
	} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: e.id, ModelRef: ref, State: e.state},
			Data: []byte(`{"tag":"x"}`),
		}); err != nil {
			t.Fatalf("Save %s: %v", e.id, err)
		}
	}
	sr := store.(spi.Searcher)
	results, err := sr.Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "tag", Source: spi.SourceData, Value: "x", Declared: []spi.DataType{spi.String}},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			Limit:        searcherBaseLimit,
			OrderBy:      []spi.OrderSpec{{Path: "state", Source: spi.SourceMeta, Kind: spi.OrderText}},
		})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// "C" locale alphabetical: "ACTIVE" < "NEW" < "PENDING".
	assertIDOrder(t, results, []string{"active-ent", "new-ent", "pending-ent"})
}

// TestPGSearcher_OrderByMetaEmptyTransitionLast verifies that a meta text field
// whose stored value is "" (empty string) sorts LAST, matching the sqlite and
// in-memory comparator behaviour (both treat empty/absent as MISSING → LAST).
//
// The divergence: postgres serialises state/transition/transaction_id without
// omitempty, so a zero-value field lands as `"transition":""` in the _meta
// JSONB blob — a PRESENT, non-null empty string.  Without NULLIF the "C"
// collation places "" before any non-empty string, so the empty entity would
// appear FIRST ascending — wrong.  NULLIF("", ”) → NULL → NULLS LAST restores
// cross-backend parity.
//
// Why this test would FAIL against the pre-fix code:
// Before the NULLIF wrap, ORDER BY on transition sorts "" < "nonblank" under
// COLLATE "C", placing e-empty first.  The assertion expects e-empty last, so
// the test FAILs, proving genuine RED capability.
func TestPGSearcher_OrderByMetaEmptyTransitionLast(t *testing.T) {
	const tenant = "orderby-empty-trans-tenant"
	factory := setupEntityTest(t)
	ctx := ctxWithTenant(spi.TenantID(tenant))
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	// Both entities start with transition="" (zero value, stored without omitempty).
	// We then patch e-has to a non-empty value so the two entities differ.
	for _, id := range []string{"e-has", "e-empty"} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: ref, State: "NEW"},
			Data: []byte(`{"tag":"y"}`),
		}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}
	// Patch e-has to have a non-empty transition so it differs from e-empty.
	pool := factory.Pool()
	if _, err := pool.Exec(ctx,
		`UPDATE entities SET doc = jsonb_set(doc, '{_meta,transition}', to_jsonb($1::text))
		 WHERE tenant_id = $2 AND entity_id = $3`,
		"nonblank", tenant, "e-has"); err != nil {
		t.Fatalf("patch transition e-has: %v", err)
	}

	sr := store.(spi.Searcher)
	results, err := sr.Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "tag", Source: spi.SourceData, Value: "y", Declared: []spi.DataType{spi.String}},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			Limit:        searcherBaseLimit,
			OrderBy:      []spi.OrderSpec{{Path: "transitionForLatestSave", Source: spi.SourceMeta, Kind: spi.OrderText}},
		})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Non-empty "nonblank" sorts before empty (NULL-treated) "".
	// Empty entity must be LAST; without NULLIF it would be FIRST (wrong).
	assertIDOrder(t, results, []string{"e-has", "e-empty"})
}

// TestPGSearcher_OrderByNullsLast verifies that missing fields sort after all
// non-null values (NULLS LAST). Without NULLS LAST, PostgreSQL places NULLs
// first for ascending sorts, so "no-score" would appear before scored entities.
func TestPGSearcher_OrderByNullsLast(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("orderby-nullslast-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	for _, e := range []struct{ id, data string }{
		{"has-score", `{"score":"50","present":true}`},
		{"high-score", `{"score":"90","present":true}`},
		{"no-score", `{"present":true}`},
	} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: e.id, ModelRef: ref, State: "NEW"},
			Data: []byte(e.data),
		}); err != nil {
			t.Fatalf("Save %s: %v", e.id, err)
		}
	}
	sr := store.(spi.Searcher)
	// FilterNotNull on "present" matches all three entities (all have the field).
	// Sorting by "score" ASC with NULLS LAST puts the null score last.
	results, err := sr.Search(ctx,
		spi.Filter{Op: spi.FilterNotNull, Path: "present", Source: spi.SourceData},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			Limit:        searcherBaseLimit,
			OrderBy:      []spi.OrderSpec{{Path: "score", Source: spi.SourceData, Kind: spi.OrderText}},
		})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// "50" < "90" lexically; no-score (NULL score) must be LAST.
	assertIDOrder(t, results, []string{"has-score", "high-score", "no-score"})
}

// TestPGSearcher_OrderByTiebreaker verifies that when all sort-key values are
// equal, a secondary entity_id tiebreaker ensures deterministic output.
// Entities are inserted in z-ent, a-ent, m-ent order; without tiebreaker
// PostgreSQL may return in any order. Expected: a-ent, m-ent, z-ent.
func TestPGSearcher_OrderByTiebreaker(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("orderby-tiebreaker-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	for _, id := range []string{"z-ent", "a-ent", "m-ent"} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: ref, State: "NEW"},
			Data: []byte(`{"city":"Berlin"}`),
		}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}
	sr := store.(spi.Searcher)
	results, err := sr.Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin", Declared: []spi.DataType{spi.String}},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			Limit:        searcherBaseLimit,
			OrderBy:      []spi.OrderSpec{{Path: "city", Source: spi.SourceData, Kind: spi.OrderText}},
		})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// All have same city → tiebreaker by entity_id → alphabetical: a-ent, m-ent, z-ent.
	assertIDOrder(t, results, []string{"a-ent", "m-ent", "z-ent"})
}

// TestPGSearcher_OrderByPointInTime verifies that ORDER BY works correctly
// for point-in-time queries (the doc column comes from entity_versions).
func TestPGSearcher_OrderByPointInTime(t *testing.T) {
	const tenant = "pit-orderby-tenant"
	const baseTS = "2026-04-01T00:00:00Z"

	factory := setupEntityTest(t)
	ctx := ctxWithTenant(spi.TenantID(tenant))
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	for _, id := range []string{"pit-1", "pit-2"} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: ref, State: "NEW"},
			Data: []byte(`{"v":1}`),
		}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}
	// Patch entity_versions: set valid_time before the PIT snapshot and set
	// distinct creation_date values so temporal ordering is deterministic.
	pool := factory.Pool()
	for _, pair := range []struct{ id, ts, createdAt string }{
		{"pit-1", baseTS, "2020-01-01T00:00:00Z"},
		{"pit-2", baseTS, "2020-06-01T00:00:00Z"},
	} {
		if _, err := pool.Exec(ctx,
			`UPDATE entity_versions
			 SET valid_time = $1,
			     doc = jsonb_set(doc, '{_meta,creation_date}', to_jsonb($2::text))
			 WHERE tenant_id = $3 AND entity_id = $4`,
			pair.ts, pair.createdAt, tenant, pair.id); err != nil {
			t.Fatalf("patch entity_versions %s: %v", pair.id, err)
		}
	}
	pit, _ := time.Parse(time.RFC3339, "2026-05-01T00:00:00Z")
	sr := store.(spi.Searcher)
	results, err := sr.Search(ctx,
		spi.Filter{Op: spi.FilterNotNull, Path: "v", Source: spi.SourceData},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			Limit:        searcherBaseLimit,
			PointInTime:  &pit,
			OrderBy:      []spi.OrderSpec{{Path: "creationDate", Source: spi.SourceMeta, Kind: spi.OrderTemporal}},
		})
	if err != nil {
		t.Fatalf("Search PIT: %v", err)
	}
	// pit-1 has earlier creationDate → should appear first.
	assertIDOrder(t, results, []string{"pit-1", "pit-2"})
}

// TestPGSearcher_ValidateOrderSpecsRejectsUnknownMetaPath verifies that an
// unrecognised SourceMeta path is rejected by validateOrderSpecs before any
// SQL is constructed.
func TestPGSearcher_ValidateOrderSpecsRejectsUnknownMetaPath(t *testing.T) {
	store, ctx := setupSearcher(t)
	sr := searcherOf(t, store)
	_, err := sr.Search(ctx,
		spi.Filter{Op: spi.FilterNotNull, Path: "name", Source: spi.SourceData},
		spi.SearchOptions{
			ModelName:    "person",
			ModelVersion: "1",
			Limit:        searcherBaseLimit,
			OrderBy:      []spi.OrderSpec{{Path: "unknownMetaField", Source: spi.SourceMeta}},
		})
	if err == nil {
		t.Fatal("expected error for unknown meta sort path, got nil")
	}
	if !errors.Is(err, postgres.ErrInvalidFilterPath) {
		t.Fatalf("expected ErrInvalidFilterPath, got: %v", err)
	}
}

// TestPGSearcher_OrderByMetaIDNoTiebreaker verifies that sorting by the "id"
// meta path (which maps to entity_id column) does not add a duplicate
// entity_id tiebreaker, and results are in correct order.
func TestPGSearcher_OrderByMetaIDNoTiebreaker(t *testing.T) {
	store, ctx := setupSearcher(t)
	sr := searcherOf(t, store)
	results, err := sr.Search(ctx,
		spi.Filter{Op: spi.FilterNotNull, Path: "name", Source: spi.SourceData},
		spi.SearchOptions{
			ModelName:    "person",
			ModelVersion: "1",
			Limit:        searcherBaseLimit,
			OrderBy:      []spi.OrderSpec{{Path: "id", Source: spi.SourceMeta, Kind: spi.OrderText}},
		})
	if err != nil {
		t.Fatalf("Search by meta id: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("expected 5 results, got %d", len(results))
	}
	// Results should be in ascending entity_id order: e1, e2, e3, e4, e5.
	assertIDOrder(t, results, []string{"e1", "e2", "e3", "e4", "e5"})
}

func TestPGSearcher_InjectionFilterPath(t *testing.T) {
	store, ctx := setupSearcher(t)
	_, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "city'); DROP TABLE entities;--", Source: spi.SourceData, Value: "x", Declared: []spi.DataType{spi.String}},
		baseOpts())
	if !errors.Is(err, postgres.ErrInvalidFilterPath) {
		t.Fatalf("want ErrInvalidFilterPath, got %v", err)
	}
}

func TestPGSearcher_InjectionOrderPath(t *testing.T) {
	store, ctx := setupSearcher(t)
	opts := baseOpts()
	opts.OrderBy = []spi.OrderSpec{{Path: "name'); DROP TABLE entities;--", Source: spi.SourceData}}
	_, err := searcherOf(t, store).Search(ctx, spi.Filter{}, opts)
	if !errors.Is(err, postgres.ErrInvalidFilterPath) {
		t.Fatalf("want ErrInvalidFilterPath for order path, got %v", err)
	}
}

func TestPGSearcher_TenantIsolation(t *testing.T) {
	// One factory (one database), two tenants. Tenant A is seeded with a
	// Berlin entity; tenant B searches the same model and must see none of A's
	// rows — every base query filters tenant_id = $1.
	factory := setupEntityTest(t)
	ctxA := ctxWithTenant("tenant-a")
	ctxB := ctxWithTenant("tenant-b")

	storeA, err := factory.EntityStore(ctxA)
	if err != nil {
		t.Fatalf("EntityStore A: %v", err)
	}
	if _, err := storeA.Save(ctxA, &spi.Entity{
		Meta: spi.EntityMeta{ID: "a1", ModelRef: searchModel, State: "NEW"},
		Data: []byte(`{"name":"Alice","city":"Berlin"}`),
	}); err != nil {
		t.Fatalf("Save A: %v", err)
	}

	storeB, err := factory.EntityStore(ctxB)
	if err != nil {
		t.Fatalf("EntityStore B: %v", err)
	}
	got, err := searcherOf(t, storeB).Search(ctxB,
		spi.Filter{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin", Declared: []spi.DataType{spi.String}},
		baseOpts())
	if err != nil {
		t.Fatalf("Search B: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("cross-tenant leak: tenant B saw %d of tenant A's rows, want 0", len(got))
	}
}

func TestPGSearcher_EqNumeric(t *testing.T) {
	store, ctx := setupSearcher(t)
	got, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "age", Source: spi.SourceData, Value: float64(30), Declared: []spi.DataType{spi.Integer}},
		baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 { // Alice age=30
		t.Fatalf("age=30: want 1, got %d", len(got))
	}
}

func TestPGSearcher_NeNumeric(t *testing.T) {
	store, ctx := setupSearcher(t)
	got, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterNe, Path: "age", Source: spi.SourceData, Value: float64(30), Declared: []spi.DataType{spi.Integer}},
		baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 4 { // Bob 25, Charlie 35, Diana 28, Eve 40
		t.Fatalf("age!=30: want 4, got %d", len(got))
	}
}

func TestPGSearcher_ContainsNumericValue(t *testing.T) {
	store, ctx := setupSearcher(t)
	// Kernel-authoritative: a string op (Contains) acts only on a TEXTUAL stored
	// value; against a numeric stored slot (age) it is a non-match and never
	// stringifies. The SQL doc->>'age' extraction stringifies (over-selects), so
	// it is a sound superset re-checked by the kernel, which drops all numeric
	// rows. Result: 0 matches (not the old SQL-stringify behaviour).
	got, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterContains, Path: "age", Source: spi.SourceData, Value: float64(3)},
		baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("age contains '3' (string op on numeric = non-match): want 0, got %d", len(got))
	}
}

// TestPGSearcher_NeNumeric_MissingField verifies the kernel's null-uniformity
// divergence: an absent stored value is a NON-match for every binary op,
// INCLUDING the negatives (NE). A row with no "age" field is therefore EXCLUDED
// from age!=30 results (not included as classic 3VL would). Ne is residual-only
// (non-pushable), so the kernel is authoritative here.
func TestPGSearcher_NeNumeric_MissingField(t *testing.T) {
	store, ctx := setupSearcher(t) // seeds e1..e5
	// Save an extra entity with no "age" field.
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e6", ModelRef: searchModel, State: "NEW"},
		Data: []byte(`{"name":"Frank","city":"Bremen"}`),
	}); err != nil {
		t.Fatalf("Save e6: %v", err)
	}
	got, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterNe, Path: "age", Source: spi.SourceData, Value: float64(30), Declared: []spi.DataType{spi.Integer}},
		baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Alice (age=30) excluded by NE; Frank (no age) also excluded by null-
	// uniformity; Bob/Charlie/Diana/Eve (age≠30) = 4.
	if len(got) != 4 {
		t.Fatalf("age!=30 (missing field excluded by null-uniformity): want 4, got %d", len(got))
	}
	for _, e := range got {
		if e.Meta.ID == "e6" {
			t.Error("age!=30: e6 (no age field) must be EXCLUDED under kernel null-uniformity")
		}
	}
}

// Compile-time guard mirrored as a runtime assertion for clarity.
func TestPGSearcher_ImplementsSearcher(t *testing.T) {
	store, ctx := setupSearcher(t)
	if _, ok := store.(spi.Searcher); !ok {
		t.Fatal("postgres entityStore must implement spi.Searcher")
	}
	_ = ctx
}

// pitSearchSetup saves v1 (active) then v2 (inactive) of one entity, then
// pins v1's valid_time to baseTS and v2's to 300µs later (same millisecond),
// mirroring pit_boundary_test.go. Returns the store, ctx, and the base time.
func pitSearchSetup(t *testing.T) (spi.EntityStore, context.Context, time.Time) {
	t.Helper()
	const (
		tenant = "pit-search-tenant"
		baseTS = "2026-02-01 00:00:00+00"
		nextTS = "2026-02-01 00:00:00.000300+00"
	)
	factory := setupEntityTest(t)
	ctx := ctxWithTenant(tenant)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ent := &spi.Entity{
		Meta: spi.EntityMeta{ID: "pp1", ModelRef: searchModel, State: "NEW"},
		Data: []byte(`{"name":"Alice","status":"active"}`),
	}
	if _, err := store.Save(ctx, ent); err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	ent.Data = []byte(`{"name":"Alice","status":"inactive"}`)
	if _, err := store.Save(ctx, ent); err != nil {
		t.Fatalf("Save v2: %v", err)
	}
	pool := factory.Pool()
	if _, err := pool.Exec(ctx,
		`UPDATE entity_versions SET valid_time=$1, transaction_time=$1
		 WHERE tenant_id=$2 AND entity_id=$3 AND version=1`, baseTS, tenant, "pp1"); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE entity_versions SET valid_time=$1, transaction_time=$1
		 WHERE tenant_id=$2 AND entity_id=$3 AND version=2`, nextTS, tenant, "pp1"); err != nil {
		t.Fatalf("pin v2: %v", err)
	}
	base, err := time.Parse(time.RFC3339Nano, "2026-02-01T00:00:00Z")
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	return store, ctx, base
}

// At the base timestamp only v1 (status=active) is visible, so a search for
// status=active returns the entity; a search for status=inactive returns none.
// This is the test that catches the B1 inner-projection defect — without the
// fix the query errors with `column "entity_id" does not exist` (default
// ORDER BY entity_id over a derived table that only projected doc).
func TestPGSearcher_PointInTimeDefaultOrder(t *testing.T) {
	store, ctx, base := pitSearchSetup(t)
	opts := spi.SearchOptions{ModelName: "person", ModelVersion: "1", PointInTime: &base, Limit: searcherBaseLimit}

	active, err := store.(spi.Searcher).Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "status", Source: spi.SourceData, Value: "active", Declared: []spi.DataType{spi.String}}, opts)
	if err != nil {
		t.Fatalf("Search active@base: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("status=active @base: want 1 (v1 snapshot), got %d", len(active))
	}

	inactive, err := store.(spi.Searcher).Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "status", Source: spi.SourceData, Value: "inactive", Declared: []spi.DataType{spi.String}}, opts)
	if err != nil {
		t.Fatalf("Search inactive@base: %v", err)
	}
	if len(inactive) != 0 {
		t.Fatalf("status=inactive @base: want 0 (v2 not yet visible), got %d", len(inactive))
	}
}

// TestPGSearcher_OrderByNullsLastDesc verifies NULLS LAST for DESC ordering.
// In PostgreSQL the default for DESC is NULLS FIRST, so a missing-field row
// would appear FIRST without an explicit "NULLS LAST" clause. This test FAILS
// against code that omits "NULLS LAST" for DESC, proving genuine RED capability.
func TestPGSearcher_OrderByNullsLastDesc(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("orderby-nullslast-desc-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	for _, e := range []struct{ id, data string }{
		{"has-score", `{"score":"50","present":true}`},
		{"high-score", `{"score":"90","present":true}`},
		{"no-score", `{"present":true}`},
	} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: e.id, ModelRef: ref, State: "NEW"},
			Data: []byte(e.data),
		}); err != nil {
			t.Fatalf("Save %s: %v", e.id, err)
		}
	}
	sr := store.(spi.Searcher)
	// DESC on "score": "90" > "50" lexically, so order is high-score, has-score.
	// no-score (NULL) must still be LAST. Without "NULLS LAST", DESC puts it
	// FIRST — the assertion on the last element proves "NULLS LAST" is present.
	results, err := sr.Search(ctx,
		spi.Filter{Op: spi.FilterNotNull, Path: "present", Source: spi.SourceData},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			Limit:        searcherBaseLimit,
			OrderBy:      []spi.OrderSpec{{Path: "score", Source: spi.SourceData, Desc: true, Kind: spi.OrderText}},
		})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Desc: "90" > "50"; NULL must still be last.
	assertIDOrder(t, results, []string{"high-score", "has-score", "no-score"})
}

// TestPGSearcher_OrderByBool verifies that OrderBool sorts false < true (asc)
// and true first (desc). Without Kind=OrderBool the implementation may treat
// the stored JSON boolean as text; the DESC case is the sensitive discriminator.
func TestPGSearcher_OrderByBool(t *testing.T) {
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("orderby-bool-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	for _, e := range []struct{ id, data string }{
		{"t", `{"active":true,"tag":"y"}`},
		{"f", `{"active":false,"tag":"y"}`},
	} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: e.id, ModelRef: ref, State: "NEW"},
			Data: []byte(e.data),
		}); err != nil {
			t.Fatalf("Save %s: %v", e.id, err)
		}
	}
	sr := store.(spi.Searcher)

	// ASC: false < true → f, t.
	asc, err := sr.Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "tag", Source: spi.SourceData, Value: "y", Declared: []spi.DataType{spi.String}},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			Limit:        searcherBaseLimit,
			OrderBy:      []spi.OrderSpec{{Path: "active", Source: spi.SourceData, Kind: spi.OrderBool}},
		})
	if err != nil {
		t.Fatalf("Search asc: %v", err)
	}
	assertIDOrder(t, asc, []string{"f", "t"})

	// DESC: true > false → t, f.
	desc, err := sr.Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "tag", Source: spi.SourceData, Value: "y", Declared: []spi.DataType{spi.String}},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			Limit:        searcherBaseLimit,
			OrderBy:      []spi.OrderSpec{{Path: "active", Source: spi.SourceData, Desc: true, Kind: spi.OrderBool}},
		})
	if err != nil {
		t.Fatalf("Search desc: %v", err)
	}
	assertIDOrder(t, desc, []string{"t", "f"})
}

// ---------------------------------------------------------------------------
// Bounded-or-fail direct search.
//
// boundedSearchModel is a dedicated model (distinct from searchModel/"item")
// so seedBoundedMatching's count is exact and unaffected by other subtests'
// seed data.
var boundedSearchModel = spi.ModelRef{EntityName: "bounded-item", ModelVersion: "1"}

// newBoundedSearchStore returns a fresh store + ctx on its own tenant.
func newBoundedSearchStore(t *testing.T) (spi.EntityStore, context.Context) {
	t.Helper()
	factory := setupEntityTest(t)
	ctx := ctxWithTenant("bounded-search-tenant")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	return store, ctx
}

// seedBoundedMatching saves n entities of boundedSearchModel, each carrying a
// non-null "tag" field so both boundedPushdownFilter (IS NOT NULL, no
// residual) and boundedResidualFilter (regex, always residual) match every
// seeded row.
func seedBoundedMatching(t *testing.T, store spi.EntityStore, ctx context.Context, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("bnd-%d", i)
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: boundedSearchModel, State: "NEW"},
			Data: []byte(`{"tag":"present"}`),
		}); err != nil {
			t.Fatalf("seedBoundedMatching Save %s: %v", id, err)
		}
	}
}

func boundedOpts(limit int) spi.SearchOptions {
	return spi.SearchOptions{
		ModelName:    boundedSearchModel.EntityName,
		ModelVersion: boundedSearchModel.ModelVersion,
		Limit:        limit,
	}
}

// boundedPushdownFilter is IS_NOT_NULL on "tag" — a presence check, which is
// the ONLY leaf shape query_planner.go's planQuery treats as provably EXACT
// (see leafExact): no residual, and the pushed leaf itself needs no kernel
// re-check, so postFilter stays nil and Search takes the SQL "LIMIT
// limit+1" probe branch.
//
// A FilterEq leaf does NOT reach that branch despite being in isPushable's
// set: leafExact(Eq) is false (its SQL is only a SOUND SUPERSET, e.g. the
// float8/text coercion in leafToSQL), so planQuery's soundness gate installs
// the full original filter as postFilter and Search takes the residual path
// instead. Confirmed by tracing planQuery → allPushedExact → leafExact
// directly (see query_planner.go), and independently by the shared spitest
// conformance suite (spitest/searcher.go), which drives its bounded-or-fail
// assertions through a FilterEq predicate — if that predicate reached
// postFilter == nil here, this plugin-local pushdown test would be
// redundant; instead it is the ONLY coverage of the SQL "LIMIT limit+1"
// probe path in this package. Do not swap this for an Eq filter without
// re-verifying postFilter == nil.
var boundedPushdownFilter = spi.Filter{Op: spi.FilterNotNull, Path: "tag", Source: spi.SourceData}

// boundedResidualFilter is a regex — never pushable (query_planner.go's
// isPushable has no FilterMatchesRegex case) — so it always drives the
// residual/postgresIter early-raise branch. ".+" matches every seeded row's
// non-empty "tag" value.
var boundedResidualFilter = spi.Filter{Op: spi.FilterMatchesRegex, Path: "tag", Source: spi.SourceData, Value: ".+"}

// TestPGSearcher_PushdownOverLimitFails exercises the no-residual branch: SQL
// is asked for limit+1 rows, and the extra row proves the matched set (3)
// does not fit in Limit (2).
func TestPGSearcher_PushdownOverLimitFails(t *testing.T) {
	store, ctx := newBoundedSearchStore(t)
	seedBoundedMatching(t, store, ctx, 3)
	_, err := searcherOf(t, store).Search(ctx, boundedPushdownFilter, boundedOpts(2))
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Fatalf("pushdown branch, 3 matches over limit 2: got err %v, want ErrSearchResultLimitExceeded", err)
	}
}

// TestPGSearcher_ResidualOverLimitFails exercises the residual branch:
// postgresIter streams post-filter matches and Search raises the moment the
// count exceeds Limit, without draining the rest of the result set.
func TestPGSearcher_ResidualOverLimitFails(t *testing.T) {
	store, ctx := newBoundedSearchStore(t)
	seedBoundedMatching(t, store, ctx, 3)
	_, err := searcherOf(t, store).Search(ctx, boundedResidualFilter, boundedOpts(2))
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Fatalf("residual branch, 3 matches over limit 2: got err %v, want ErrSearchResultLimitExceeded", err)
	}
}

// TestPGSearcher_AtLimitSucceeds: exactly-at-limit is not an overflow.
func TestPGSearcher_AtLimitSucceeds(t *testing.T) {
	store, ctx := newBoundedSearchStore(t)
	seedBoundedMatching(t, store, ctx, 2)
	got, err := searcherOf(t, store).Search(ctx, boundedPushdownFilter, boundedOpts(2))
	if err != nil {
		t.Fatalf("2 matches at limit 2: unexpected err %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

// TestPGSearcher_ZeroLimitRejected and TestPGSearcher_NegativeLimitRejected:
// Limit <= 0 is a contract violation, not "unbounded" — Search must reject it
// with an error rather than substituting a default cap of its own. Exercised
// here through the SQL-pushdown branch (boundedPushdownFilter has no
// residual) specifically, on top of the generic coverage the shared spitest
// conformance suite (spitest/searcher.go's BoundedOrFail/ZeroLimitRejected
// and /NegativeLimitRejected) already provides, so the rejection is proven to
// happen before runSearch builds any SQL at all — not merely somewhere along
// the residual path.
func TestPGSearcher_ZeroLimitRejected(t *testing.T) {
	store, ctx := newBoundedSearchStore(t)
	seedBoundedMatching(t, store, ctx, 3)
	got, err := searcherOf(t, store).Search(ctx, boundedPushdownFilter, boundedOpts(0))
	if err == nil {
		t.Fatal("limit 0 is a contract violation, not \"unbounded\": expected an error, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("a rejected Limit must not also return a result: got %d entities", len(got))
	}
}

func TestPGSearcher_NegativeLimitRejected(t *testing.T) {
	store, ctx := newBoundedSearchStore(t)
	seedBoundedMatching(t, store, ctx, 3)
	got, err := searcherOf(t, store).Search(ctx, boundedPushdownFilter, boundedOpts(-1))
	if err == nil {
		t.Fatal("negative limit is a contract violation: expected an error, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("a rejected Limit must not also return a result: got %d entities", len(got))
	}
}

// TestPGSearcher_InTxOverLimitFails proves the bound applies through a live
// pgx.Tx: postgres has no buffer overlay and never calls spi.MergeBounded —
// Search's committed pushdown IS the in-tx result (the Querier resolves the
// active pgx.Tx from ctx, so REPEATABLE READ gives RYW natively), so the same
// searchCommitted code path and the same single bound check apply whether or
// not a transaction is active. Here the 3 matches are staged but never
// committed, so this exercises the bound over uncommitted rows visible only
// inside the searching transaction itself.
func TestPGSearcher_InTxOverLimitFails(t *testing.T) {
	factory, tm := setupFCWTest(t)
	ctx := ctxWithTenant("bounded-search-tx-tenant")
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	store, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore (tx): %v", err)
	}
	seedBoundedMatching(t, store, txCtx, 3)
	_, err = searcherOf(t, store).Search(txCtx, boundedPushdownFilter, boundedOpts(2))
	if !errors.Is(err, spi.ErrSearchResultLimitExceeded) {
		t.Fatalf("in-tx, 3 uncommitted matches over limit 2: got err %v, want ErrSearchResultLimitExceeded", err)
	}
}
