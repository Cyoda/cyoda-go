package postgres_test

import (
	"context"
	"errors"
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

func baseOpts() spi.SearchOptions {
	return spi.SearchOptions{ModelName: "person", ModelVersion: "1"}
}

func TestPGSearcher_Eq(t *testing.T) {
	store, ctx := setupSearcher(t)
	got, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
		baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("city=Berlin: want 2, got %d", len(got))
	}
}

func TestPGSearcher_GtNumeric(t *testing.T) {
	store, ctx := setupSearcher(t)
	got, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterGt, Path: "age", Source: spi.SourceData, Value: float64(30)},
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
			Values: []any{float64(28), float64(35)}},
		baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 3 { // 30, 35, 28
		t.Fatalf("age in [28,35]: want 3, got %d", len(got))
	}
}

func TestPGSearcher_And(t *testing.T) {
	store, ctx := setupSearcher(t)
	got, err := searcherOf(t, store).Search(ctx, spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
			{Op: spi.FilterGt, Path: "age", Source: spi.SourceData, Value: float64(31)},
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
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Hamburg"},
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Munich"},
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
	got, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: "^A"},
		baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 { // Alice
		t.Fatalf("name ~ ^A: want 1, got %d", len(got))
	}
}

// Mixed: pushable city=Berlin AND residual regex on name.
func TestPGSearcher_MixedPushAndResidual(t *testing.T) {
	store, ctx := setupSearcher(t)
	got, err := searcherOf(t, store).Search(ctx, spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
			{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: "^C"},
		},
	}, baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 { // Charlie
		t.Fatalf("Berlin AND name~^C: want 1, got %d", len(got))
	}
}

// Pagination with NO residual → LIMIT/OFFSET pushed into SQL.
func TestPGSearcher_PaginationNoResidual(t *testing.T) {
	store, ctx := setupSearcher(t)
	opts := baseOpts()
	opts.Limit = 2
	opts.Offset = 1
	got, err := searcherOf(t, store).Search(ctx, spi.Filter{}, opts) // match-all, default order entity_id
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit 2 offset 1: want 2, got %d", len(got))
	}
}

// Pagination WITH residual → LIMIT/OFFSET applied in Go after post-filter.
func TestPGSearcher_PaginationWithResidual(t *testing.T) {
	store, ctx := setupSearcher(t)
	opts := baseOpts()
	opts.Limit = 1
	// Residual regex matching all five names (contains any letter), so the
	// residual path is active and pagination is applied in Go.
	got, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: "."},
		opts)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("residual + limit 1: want 1, got %d", len(got))
	}
}

// S1 guard: unbounded (Limit==0) + residual + non-zero offset must return the
// correct page, not empty. A naive early-stop at offset+limit==offset breaks this.
func TestPGSearcher_UnboundedOffsetWithResidual(t *testing.T) {
	store, ctx := setupSearcher(t)
	opts := baseOpts()
	opts.Limit = 0 // unbounded
	opts.Offset = 2
	got, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: "."},
		opts)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 3 { // 5 total, drop first 2
		t.Fatalf("unbounded offset 2 + residual: want 3, got %d", len(got))
	}
}

func TestPGSearcher_OrderByDataPathDesc(t *testing.T) {
	store, ctx := setupSearcher(t)
	opts := baseOpts()
	opts.OrderBy = []spi.OrderSpec{{Path: "name", Source: spi.SourceData, Desc: true}}
	got, err := searcherOf(t, store).Search(ctx, spi.Filter{}, opts)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("want 5, got %d", len(got))
	}
	// Lexicographic desc: Eve, Diana, Charlie, Bob, Alice.
	if string(got[0].Data) == "" || got[0].Meta.ID != "e5" {
		t.Errorf("desc by name: want first=Eve(e5), got id=%s", got[0].Meta.ID)
	}
}

func TestPGSearcher_InjectionFilterPath(t *testing.T) {
	store, ctx := setupSearcher(t)
	_, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "city'); DROP TABLE entities;--", Source: spi.SourceData, Value: "x"},
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
		spi.Filter{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
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
		spi.Filter{Op: spi.FilterEq, Path: "age", Source: spi.SourceData, Value: float64(30)},
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
		spi.Filter{Op: spi.FilterNe, Path: "age", Source: spi.SourceData, Value: float64(30)},
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
	// value is float64(3); string-op treats it as "3"; ages 30 and 35 contain "3"
	got, err := searcherOf(t, store).Search(ctx,
		spi.Filter{Op: spi.FilterContains, Path: "age", Source: spi.SourceData, Value: float64(3)},
		baseOpts())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 { // Alice 30, Charlie 35
		t.Fatalf("age contains '3': want 2, got %d", len(got))
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
	opts := spi.SearchOptions{ModelName: "person", ModelVersion: "1", PointInTime: &base}

	active, err := store.(spi.Searcher).Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "status", Source: spi.SourceData, Value: "active"}, opts)
	if err != nil {
		t.Fatalf("Search active@base: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("status=active @base: want 1 (v1 snapshot), got %d", len(active))
	}

	inactive, err := store.(spi.Searcher).Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "status", Source: spi.SourceData, Value: "inactive"}, opts)
	if err != nil {
		t.Fatalf("Search inactive@base: %v", err)
	}
	if len(inactive) != 0 {
		t.Fatalf("status=inactive @base: want 0 (v2 not yet visible), got %d", len(inactive))
	}
}
