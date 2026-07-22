package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// testCtx returns a context with a UserContext for the given tenant.
func testCtx(tenantID string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "test-user",
		UserName: "Test User",
		Tenant: spi.Tenant{
			ID:   spi.TenantID(tenantID),
			Name: "Test Tenant",
		},
		Roles: []string{"ROLE_USER"},
	})
}

// setupSearcherTest creates a StoreFactory and saves test entities.
func setupSearcherTest(t *testing.T) (*sqlite.StoreFactory, context.Context) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "search_test.db")

	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	entities := []struct {
		id   string
		data string
	}{
		{"e1", `{"name":"Alice","age":30,"city":"Berlin"}`},
		{"e2", `{"name":"Bob","age":25,"city":"Munich"}`},
		{"e3", `{"name":"Charlie","age":35,"city":"Berlin"}`},
		{"e4", `{"name":"Diana","age":28,"city":"Hamburg"}`},
		{"e5", `{"name":"Eve","age":40,"city":"Munich"}`},
	}

	for _, e := range entities {
		_, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{
				ID:       e.id,
				ModelRef: ref,
				State:    "NEW",
			},
			Data: []byte(e.data),
		})
		if err != nil {
			t.Fatalf("Save %s: %v", e.id, err)
		}
	}

	return factory, ctx
}

func TestSearcher_EqFilter(t *testing.T) {
	factory, ctx := setupSearcherTest(t)

	store, _ := factory.EntityStore(ctx)
	searcher, ok := store.(spi.Searcher)
	if !ok {
		t.Fatal("entityStore does not implement spi.Searcher")
	}

	results, err := searcher.Search(ctx, spi.Filter{
		Op:     spi.FilterEq,
		Path:   "city",
		Source: spi.SourceData,
		Value:  "Berlin",
	}, spi.SearchOptions{
		ModelName:    "person",
		ModelVersion: "1",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for city=Berlin, got %d", len(results))
	}
}

func TestSearcher_GtFilter(t *testing.T) {
	factory, ctx := setupSearcherTest(t)

	store, _ := factory.EntityStore(ctx)
	searcher := store.(spi.Searcher)

	results, err := searcher.Search(ctx, spi.Filter{
		Op:     spi.FilterGt,
		Path:   "age",
		Source: spi.SourceData,
		Value:  float64(30),
	}, spi.SearchOptions{
		ModelName:    "person",
		ModelVersion: "1",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Charlie(35) and Eve(40) have age > 30.
	if len(results) != 2 {
		t.Fatalf("expected 2 results for age>30, got %d", len(results))
	}
}

func TestSearcher_ContainsFilter(t *testing.T) {
	factory, ctx := setupSearcherTest(t)

	store, _ := factory.EntityStore(ctx)
	searcher := store.(spi.Searcher)

	results, err := searcher.Search(ctx, spi.Filter{
		Op:     spi.FilterContains,
		Path:   "name",
		Source: spi.SourceData,
		Value:  "li",
	}, spi.SearchOptions{
		ModelName:    "person",
		ModelVersion: "1",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Alice and Charlie both contain "li".
	if len(results) != 2 {
		t.Fatalf("expected 2 results containing 'li', got %d", len(results))
	}
}

func TestSearcher_ANDFilter(t *testing.T) {
	factory, ctx := setupSearcherTest(t)

	store, _ := factory.EntityStore(ctx)
	searcher := store.(spi.Searcher)

	results, err := searcher.Search(ctx, spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
			{Op: spi.FilterGt, Path: "age", Source: spi.SourceData, Value: float64(31)},
		},
	}, spi.SearchOptions{
		ModelName:    "person",
		ModelVersion: "1",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Only Charlie: Berlin, age 35.
	if len(results) != 1 {
		t.Fatalf("expected 1 result for Berlin AND age>31, got %d", len(results))
	}
	if results[0].Meta.ID != "e3" {
		t.Errorf("expected e3, got %s", results[0].Meta.ID)
	}
}

func TestSearcher_ORFilter(t *testing.T) {
	factory, ctx := setupSearcherTest(t)

	store, _ := factory.EntityStore(ctx)
	searcher := store.(spi.Searcher)

	results, err := searcher.Search(ctx, spi.Filter{
		Op: spi.FilterOr,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Hamburg"},
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Munich"},
		},
	}, spi.SearchOptions{
		ModelName:    "person",
		ModelVersion: "1",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Bob(Munich), Diana(Hamburg), Eve(Munich).
	if len(results) != 3 {
		t.Fatalf("expected 3 results for Hamburg OR Munich, got %d", len(results))
	}
}

func TestSearcher_PostFilterRegex(t *testing.T) {
	factory, ctx := setupSearcherTest(t)

	store, _ := factory.EntityStore(ctx)
	searcher := store.(spi.Searcher)

	// Regex is not pushable, should post-filter.
	results, err := searcher.Search(ctx, spi.Filter{
		Op:     spi.FilterMatchesRegex,
		Path:   "name",
		Source: spi.SourceData,
		Value:  "^[A-C]",
	}, spi.SearchOptions{
		ModelName:    "person",
		ModelVersion: "1",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Alice, Bob, Charlie start with A/B/C.
	if len(results) != 3 {
		t.Fatalf("expected 3 results for regex ^[A-C], got %d", len(results))
	}
}

func TestSearcher_MixedPushAndPostFilter(t *testing.T) {
	factory, ctx := setupSearcherTest(t)

	store, _ := factory.EntityStore(ctx)
	searcher := store.(spi.Searcher)

	// AND with pushable eq(city) and non-pushable regex(name).
	results, err := searcher.Search(ctx, spi.Filter{
		Op: spi.FilterAnd,
		Children: []spi.Filter{
			{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
			{Op: spi.FilterMatchesRegex, Path: "name", Source: spi.SourceData, Value: "^A"},
		},
	}, spi.SearchOptions{
		ModelName:    "person",
		ModelVersion: "1",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Alice: Berlin, starts with A. Charlie: Berlin, starts with C (no match).
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Meta.ID != "e1" {
		t.Errorf("expected e1 (Alice), got %s", results[0].Meta.ID)
	}
}

func TestSearcher_Pagination_NoPushdown(t *testing.T) {
	factory, ctx := setupSearcherTest(t)

	store, _ := factory.EntityStore(ctx)
	searcher := store.(spi.Searcher)

	// Use a pushable filter that matches all.
	filter := spi.Filter{
		Op:     spi.FilterNotNull,
		Path:   "name",
		Source: spi.SourceData,
	}

	// Get all (5 entities).
	all, err := searcher.Search(ctx, filter, spi.SearchOptions{
		ModelName:    "person",
		ModelVersion: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("expected 5 results, got %d", len(all))
	}

	// Limit 2.
	page, err := searcher.Search(ctx, filter, spi.SearchOptions{
		ModelName:    "person",
		ModelVersion: "1",
		Limit:        2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("expected 2 results with limit=2, got %d", len(page))
	}

	// Offset 3, Limit 10.
	tail, err := searcher.Search(ctx, filter, spi.SearchOptions{
		ModelName:    "person",
		ModelVersion: "1",
		Limit:        10,
		Offset:       3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 2 {
		t.Fatalf("expected 2 results with offset=3, got %d", len(tail))
	}
}

func TestSearcher_Pagination_WithPostFilter(t *testing.T) {
	factory, ctx := setupSearcherTest(t)

	store, _ := factory.EntityStore(ctx)
	searcher := store.(spi.Searcher)

	// Non-pushable filter matching all.
	filter := spi.Filter{
		Op:     spi.FilterMatchesRegex,
		Path:   "name",
		Source: spi.SourceData,
		Value:  ".*",
	}

	// Get all.
	all, err := searcher.Search(ctx, filter, spi.SearchOptions{
		ModelName:    "person",
		ModelVersion: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("expected 5 results, got %d", len(all))
	}

	// Limit 2.
	page, err := searcher.Search(ctx, filter, spi.SearchOptions{
		ModelName:    "person",
		ModelVersion: "1",
		Limit:        2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("expected 2 results with limit=2, got %d", len(page))
	}

	// Offset 3.
	tail, err := searcher.Search(ctx, filter, spi.SearchOptions{
		ModelName:    "person",
		ModelVersion: "1",
		Limit:        10,
		Offset:       3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 2 {
		t.Fatalf("expected 2 results with offset=3, got %d", len(tail))
	}
}

func TestSearcher_ScanBudgetExhausted(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "budget_test.db")

	// Create factory with a very low scan limit.
	factory, err := sqlite.NewStoreFactoryForTestWithScanLimit(context.Background(), dbPath, 3)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	defer factory.Close()

	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	store, _ := factory.EntityStore(ctx)

	// Save 10 entities.
	for i := 0; i < 10; i++ {
		_, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{
				ID:       fmt.Sprintf("e%d", i),
				ModelRef: ref,
				State:    "NEW",
			},
			Data: []byte(fmt.Sprintf(`{"val":%d}`, i)),
		})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	searcher := store.(spi.Searcher)

	// Use a non-pushable filter to force post-filtering (triggering scan budget).
	_, err = searcher.Search(ctx, spi.Filter{
		Op:     spi.FilterMatchesRegex,
		Path:   "val",
		Source: spi.SourceData,
		Value:  ".*",
	}, spi.SearchOptions{
		ModelName:    "item",
		ModelVersion: "1",
	})

	if err == nil {
		t.Fatal("expected spi.ErrScanBudgetExhausted, got nil")
	}
	if !errors.Is(err, spi.ErrScanBudgetExhausted) {
		t.Fatalf("expected spi.ErrScanBudgetExhausted, got: %v", err)
	}
}

func TestSearcher_TenantIsolation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tenant_test.db")

	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	defer factory.Close()

	ctxA := testCtx("tenant-A")
	ctxB := testCtx("tenant-B")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	storeA, _ := factory.EntityStore(ctxA)
	storeB, _ := factory.EntityStore(ctxB)

	_, _ = storeA.Save(ctxA, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e1", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"name":"Alice"}`),
	})

	_, _ = storeB.Save(ctxB, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e2", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"name":"Bob"}`),
	})

	searcherA := storeA.(spi.Searcher)
	searcherB := storeB.(spi.Searcher)

	filter := spi.Filter{Op: spi.FilterNotNull, Path: "name", Source: spi.SourceData}
	opts := spi.SearchOptions{ModelName: "person", ModelVersion: "1"}

	resultsA, err := searcherA.Search(ctxA, filter, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(resultsA) != 1 || resultsA[0].Meta.ID != "e1" {
		t.Errorf("tenant A should see e1 only, got %d results", len(resultsA))
	}

	resultsB, err := searcherB.Search(ctxB, filter, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(resultsB) != 1 || resultsB[0].Meta.ID != "e2" {
		t.Errorf("tenant B should see e2 only, got %d results", len(resultsB))
	}
}

// ---- ORDER BY tests (Task 10: Kind-aware ORDER BY) -------------------------

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

// TestSearcher_OrderByNumericData verifies that OrderNumeric applies CAST AS
// REAL so that string-encoded numbers sort by magnitude, not byte order.
// Without CAST: "10" < "100" < "9" (lexical) → order b, c, a.
// With CAST:    9 < 10 < 100 (numeric) → order a, b, c.
func TestSearcher_OrderByNumericData(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "numeric_test.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	defer factory.Close()

	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	store, _ := factory.EntityStore(ctx)

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

	searcher := store.(spi.Searcher)
	results, err := searcher.Search(ctx,
		spi.Filter{Op: spi.FilterNotNull, Path: "n", Source: spi.SourceData},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			OrderBy:      []spi.OrderSpec{{Path: "n", Source: spi.SourceData, Kind: spi.OrderNumeric}},
		})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Numeric: 9 < 10 < 100 → a, b, c.
	assertIDOrder(t, results, []string{"a", "b", "c"})
}

// TestSearcher_OrderByCreationDateMeta verifies that the canonical meta path
// "creationDate" is mapped to the blob key "creation_date", enabling temporal
// ordering by entity creation time.
//
// Entity IDs are assigned so that chronological (creationDate) order DIFFERS
// from entity_id lexicographic order: "z" is saved first (oldest clock), "m"
// second, "a" last (newest). Expected ascending result is [z, m, a]. If the
// meta key mapping is wrong (returns NULL for all rows), the entity_id
// tiebreaker takes over and produces [a, m, z] — the test would FAIL, proving
// genuine RED capability.
func TestSearcher_OrderByCreationDateMeta(t *testing.T) {
	clock := sqlite.NewTestClock()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "creationdate_test.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath, sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	defer factory.Close()

	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	store, _ := factory.EntityStore(ctx)

	// "z" saved at t0 (oldest), "m" at t1, "a" at t2 (newest) —
	// the inverse of entity_id lexicographic order.
	for _, id := range []string{"z", "m", "a"} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: ref, State: "NEW"},
			Data: []byte(`{"v":1}`),
		}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
		clock.Advance(10 * time.Millisecond)
	}

	searcher := store.(spi.Searcher)
	results, err := searcher.Search(ctx,
		spi.Filter{Op: spi.FilterNotNull, Path: "v", Source: spi.SourceData},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			OrderBy:      []spi.OrderSpec{{Path: "creationDate", Source: spi.SourceMeta, Kind: spi.OrderTemporal}},
		})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Chronological ascending: z (t0), m (t1), a (t2).
	// If mapping were wrong (all-NULL → entity_id tiebreaker): a, m, z — FAIL.
	assertIDOrder(t, results, []string{"z", "m", "a"})
}

// TestSearcher_OrderByStateMeta verifies that meta state field sorts correctly
// using COLLATE BINARY (OrderText).
func TestSearcher_OrderByStateMeta(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state_test.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	defer factory.Close()

	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	store, _ := factory.EntityStore(ctx)

	// Insert in reverse alphabetical order to confirm correct sorting.
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

	searcher := store.(spi.Searcher)
	results, err := searcher.Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "tag", Source: spi.SourceData, Value: "x"},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			OrderBy:      []spi.OrderSpec{{Path: "state", Source: spi.SourceMeta, Kind: spi.OrderText}},
		})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// BINARY alphabetical: "ACTIVE" < "NEW" < "PENDING".
	assertIDOrder(t, results, []string{"active-ent", "new-ent", "pending-ent"})
}

// TestSearcher_OrderByNullsLast verifies that missing fields sort after all
// non-null values (NULLS LAST). Without NULLS LAST, SQLite places NULLs first
// for ascending sorts, making "no-score" appear before scored entities.
func TestSearcher_OrderByNullsLast(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "nullslast_test.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	defer factory.Close()

	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	store, _ := factory.EntityStore(ctx)

	// All entities share "present":true so they all pass the filter.
	// "no-score" has no "score" field → NULL for the sort key.
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

	searcher := store.(spi.Searcher)
	// Filter by "present" so all 3 entities are returned; sort by "score" ASC.
	results, err := searcher.Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "present", Source: spi.SourceData, Value: true},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			OrderBy:      []spi.OrderSpec{{Path: "score", Source: spi.SourceData, Kind: spi.OrderText}},
		})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// "50" < "90" lexically; no-score (NULL score) must be LAST.
	assertIDOrder(t, results, []string{"has-score", "high-score", "no-score"})
}

// TestSearcher_OrderByTiebreaker verifies that when all sort-key values are
// equal, a secondary entity_id tiebreaker ensures deterministic output.
// Without the tiebreaker SQLite returns rows in scan (insertion) order.
// We insert z-ent, a-ent, m-ent and expect alphabetical a-ent, m-ent, z-ent.
func TestSearcher_OrderByTiebreaker(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tiebreaker_test.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	defer factory.Close()

	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	store, _ := factory.EntityStore(ctx)

	// Deliberate insertion order: z first, then a, then m.
	// Without a tiebreaker, SQLite returns in scan order: z, a, m.
	for _, id := range []string{"z-ent", "a-ent", "m-ent"} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: ref, State: "NEW"},
			Data: []byte(`{"city":"Berlin"}`),
		}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}

	searcher := store.(spi.Searcher)
	results, err := searcher.Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			OrderBy:      []spi.OrderSpec{{Path: "city", Source: spi.SourceData, Kind: spi.OrderText}},
		})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// All have same city → tiebreaker by entity_id → alphabetical: a-ent, m-ent, z-ent.
	assertIDOrder(t, results, []string{"a-ent", "m-ent", "z-ent"})
}

// TestSearcher_OrderByPointInTime verifies that OrderBy works correctly for
// point-in-time queries (table alias "ev").
func TestSearcher_OrderByPointInTime(t *testing.T) {
	clock := sqlite.NewTestClock()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pit_order_test.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath, sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	defer factory.Close()

	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	store, _ := factory.EntityStore(ctx)

	// Save pit-1 at t0.
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "pit-1", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"v":1}`),
	}); err != nil {
		t.Fatalf("Save pit-1: %v", err)
	}
	clock.Advance(10 * time.Millisecond) // → t1

	// Save pit-2 at t1.
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "pit-2", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"v":2}`),
	}); err != nil {
		t.Fatalf("Save pit-2: %v", err)
	}
	clock.Advance(10 * time.Millisecond) // → t2
	t2 := clock.Now()

	searcher := store.(spi.Searcher)
	results, err := searcher.Search(ctx,
		spi.Filter{Op: spi.FilterNotNull, Path: "v", Source: spi.SourceData},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			PointInTime:  &t2,
			OrderBy:      []spi.OrderSpec{{Path: "creationDate", Source: spi.SourceMeta, Kind: spi.OrderTemporal}},
		})
	if err != nil {
		t.Fatalf("Search PIT: %v", err)
	}
	// pit-1 created at t0 (earlier) → appears first.
	assertIDOrder(t, results, []string{"pit-1", "pit-2"})
}

// TestSearcher_ValidateOrderSpecsRejectsUnknownMetaPath verifies that an
// unrecognised SourceMeta path is rejected by validateOrderSpecs before any
// SQL is constructed.
func TestSearcher_ValidateOrderSpecsRejectsUnknownMetaPath(t *testing.T) {
	factory, ctx := setupSearcherTest(t)
	store, _ := factory.EntityStore(ctx)
	searcher := store.(spi.Searcher)

	_, err := searcher.Search(ctx,
		spi.Filter{Op: spi.FilterNotNull, Path: "name", Source: spi.SourceData},
		spi.SearchOptions{
			ModelName:    "person",
			ModelVersion: "1",
			OrderBy:      []spi.OrderSpec{{Path: "unknownMetaField", Source: spi.SourceMeta}},
		})
	if err == nil {
		t.Fatal("expected error for unknown meta sort path, got nil")
	}
	if !errors.Is(err, sqlite.ErrInvalidFilterPath) {
		t.Fatalf("expected ErrInvalidFilterPath, got: %v", err)
	}
}

// TestSearcher_OrderByMetaIDNoTiebreaker verifies that sorting by the "id"
// meta path (which maps to the entity_id column) does not add a duplicate
// entity_id tiebreaker, and that results are returned in correct order.
func TestSearcher_OrderByMetaIDNoTiebreaker(t *testing.T) {
	factory, ctx := setupSearcherTest(t)
	store, _ := factory.EntityStore(ctx)
	searcher := store.(spi.Searcher)

	results, err := searcher.Search(ctx,
		spi.Filter{Op: spi.FilterNotNull, Path: "name", Source: spi.SourceData},
		spi.SearchOptions{
			ModelName:    "person",
			ModelVersion: "1",
			OrderBy:      []spi.OrderSpec{{Path: "id", Source: spi.SourceMeta}},
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

// TestSearcher_OrderByDesc verifies that Desc:true reverses the sort order.
// Without Desc:true the names are Alice, Bob, Charlie, Diana, Eve (ascending).
// With Desc:true we expect the reverse: Eve, Diana, Charlie, Bob, Alice.
// If Desc is silently ignored the test fails, proving genuine RED capability.
func TestSearcher_OrderByDesc(t *testing.T) {
	factory, ctx := setupSearcherTest(t)
	store, _ := factory.EntityStore(ctx)
	searcher := store.(spi.Searcher)

	results, err := searcher.Search(ctx,
		spi.Filter{Op: spi.FilterNotNull, Path: "name", Source: spi.SourceData},
		spi.SearchOptions{
			ModelName:    "person",
			ModelVersion: "1",
			OrderBy:      []spi.OrderSpec{{Path: "name", Source: spi.SourceData, Desc: true, Kind: spi.OrderText}},
		})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("want 5, got %d", len(results))
	}
	// setupSearcherTest seeds e1=Alice, e2=Bob, e3=Charlie, e4=Diana, e5=Eve.
	// Desc alphabetical: Eve > Diana > Charlie > Bob > Alice.
	assertIDOrder(t, results, []string{"e5", "e4", "e3", "e2", "e1"})
}

// TestSearcher_OrderByBool verifies that OrderBool sorts false < true (asc)
// and true < false (desc — i.e. true appears first).
// Without Kind=OrderBool the implementation may treat booleans as text
// ("false" < "true" lexically happens to agree for asc, but the DESC case
// would also expose wrong behaviour if Desc is broken); this test pins both.
func TestSearcher_OrderByBool(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "bool_test.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	defer factory.Close()

	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	store, _ := factory.EntityStore(ctx)

	for _, e := range []struct{ id, data string }{
		{"t", `{"active":true,"tag":"x"}`},
		{"f", `{"active":false,"tag":"x"}`},
	} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: e.id, ModelRef: ref, State: "NEW"},
			Data: []byte(e.data),
		}); err != nil {
			t.Fatalf("Save %s: %v", e.id, err)
		}
	}

	searcher := store.(spi.Searcher)

	// ASC: false < true → f, t.
	asc, err := searcher.Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "tag", Source: spi.SourceData, Value: "x"},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			OrderBy:      []spi.OrderSpec{{Path: "active", Source: spi.SourceData, Kind: spi.OrderBool}},
		})
	if err != nil {
		t.Fatalf("Search asc: %v", err)
	}
	assertIDOrder(t, asc, []string{"f", "t"})

	// DESC: true > false → t, f.
	desc, err := searcher.Search(ctx,
		spi.Filter{Op: spi.FilterEq, Path: "tag", Source: spi.SourceData, Value: "x"},
		spi.SearchOptions{
			ModelName:    "item",
			ModelVersion: "1",
			OrderBy:      []spi.OrderSpec{{Path: "active", Source: spi.SourceData, Desc: true, Kind: spi.OrderBool}},
		})
	if err != nil {
		t.Fatalf("Search desc: %v", err)
	}
	assertIDOrder(t, desc, []string{"t", "f"})
}
