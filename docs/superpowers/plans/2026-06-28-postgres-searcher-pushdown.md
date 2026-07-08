# PostgreSQL Search Predicate Pushdown (`spi.Searcher`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the PostgreSQL storage plugin implement `spi.Searcher` so entity searches push supported predicates into SQL (JSONB `->>` / numeric / range / string ops) instead of fetching every entity and filtering in memory.

**Architecture:** Add `Search(ctx, filter, opts)` to the postgres `*entityStore`, mirroring the existing SQLite `Searcher`. It reuses the already-present query substrate — `planQuery` (pushable/residual split, `query_planner.go`), `postgresIter` + `evalPostFilter` (streaming residual eval, `grouped_stats.go`), `shiftPlaceholders`, and `unmarshalEntityDoc`. A shared base-query helper is extracted from `Iterate` (Gate-6 dedup) and carries a fix for a latent point-in-time projection defect (B1). No SPI change, no new env vars, no Cassandra impact. The service layer (`internal/domain/search/service.go`) already delegates to `spi.Searcher` when `tx == nil`, so no service-layer change is needed.

**Tech Stack:** Go 1.26+, `github.com/jackc/pgx/v5`, PostgreSQL JSONB, testcontainers-go (postgres plugin tests require Docker), `cyoda-go-spi` (pinned, unchanged).

## Global Constraints

- Go 1.26+; use `log/slog` only (no code here logs, but never `fmt.Printf`). Wrap errors with `fmt.Errorf("...: %w", err)`.
- **No SPI change.** `spi.Searcher`, `spi.Filter`, `spi.SearchOptions`, `spi.OrderSpec` are already in the pinned SPI. Do **not** bump `go.mod`; `COMPATIBILITY.md` is unaffected.
- **Parity invariant:** the postgres pushable-op set (`isPushable` in `query_planner.go`) MUST stay identical to SQLite's. This plan does **not** change `isPushable` — do not touch it.
- **Outer-projection invariant (S-1):** the outer search/iterate query MUST stay `SELECT doc` (one column) — `postgresIter` scans exactly one column (`it.rows.Scan(&doc)`, `grouped_stats.go:191`). The B1 fix adds columns to the **inner** `DISTINCT ON` only, never the outer select.
- **PIT bound is canonical (#349/#350):** point-in-time uses raw inclusive `valid_time <= $N`, no rounding. Do not introduce `Truncate`/`Add(...Millisecond)`.
- **Security (Gate 3):** every interpolated JSON path MUST pass `validateJSONPath` before reaching SQL. Tenant isolation is enforced by `tenant_id = $1` in every base query — never remove it.
- **Postgres placeholders are `$N`** (1-based, positional), unlike SQLite's `?`. The pushdown WHERE fragment from `planQuery` starts at `$1` and MUST be renumbered via `shiftPlaceholders(frag, len(baseArgs))` before appending.
- Plugin submodules have their own `go.mod`; run postgres tests with `cd plugins/postgres && go test ./...` (Docker required) — root `./...` does not recurse into them.

Reference spec: `docs/superpowers/specs/2026-06-27-postgres-searcher-pushdown-design.md`.

---

## File Structure

- **Create** `plugins/postgres/search_base.go` — `searchBaseQuery(entityName, modelVersion string, pit *time.Time) (string, []any)`, the shared current-state / point-in-time base SELECT used by both `Iterate` and `Search`.
- **Create** `plugins/postgres/searcher.go` — `Search` (the `spi.Searcher` impl), `orderByClause`, `orderByFieldExpr`, and the `var _ spi.Searcher` assertion.
- **Create** `plugins/postgres/searcher_test.go` — postgres-plugin unit tests (operators, residual, pagination, order, injection, point-in-time).
- **Modify** `plugins/postgres/grouped_stats.go` — `Iterate` calls `searchBaseQuery` instead of building the base query inline (Gate-6 dedup).
- **Modify** `plugins/postgres/path_validation.go` — add `validateOrderSpecs`.
- **Modify** `e2e/parity/client/http.go` — add `SyncSearchAt` (point-in-time sync search helper).
- **Create** `e2e/parity/search_pit.go` — `RunSearchPointInTime` cross-backend parity scenario.
- **Modify** `e2e/parity/registry.go` — register `{"SearchPointInTime", RunSearchPointInTime}`.
- **Modify** `CHANGELOG.md` — additive performance entry.

---

## Task 1: Extract shared base-query helper from `Iterate` (Gate-6 dedup, pure refactor)

Carve the current-state / point-in-time base SELECT out of `Iterate` into a reusable helper so `Search` (Task 2) builds on the same literals. **Behavior-preserving** — this task copies the *existing* (still-unfixed) PIT query shape verbatim; the B1 fix lands in Task 3.

**Files:**
- Create: `plugins/postgres/search_base.go`
- Modify: `plugins/postgres/grouped_stats.go:77-104` (the `Iterate` base-query construction)
- Test: `plugins/postgres/grouped_stats_test.go` (existing `TestPostgresIterate_*` are the regression guard)

**Interfaces:**
- Produces: `func (s *entityStore) searchBaseQuery(entityName, modelVersion string, pit *time.Time) (string, []any)` — returns the base SQL (outer `SELECT doc`) and its positional args (`$1`=tenant, `$2`=name, `$3`=version, and for PIT `$4`=pit). Consumed by `Iterate` (this task) and `Search` (Task 2).

- [ ] **Step 1: Create the helper, copying `Iterate`'s current query verbatim**

Create `plugins/postgres/search_base.go`:

```go
package postgres

import "time"

// searchBaseQuery builds the base SELECT over a model for current-state
// (pit == nil) or point-in-time (pit != nil) reads. The outer projection is
// always `SELECT doc` (one column) — the S-1 invariant the row scanner
// (postgresIter, grouped_stats.go) depends on.
//
// Positional args: $1 tenant, $2 entityName, $3 modelVersion, and for PIT
// $4 the snapshot time. Callers append a pushdown WHERE fragment with
// shiftPlaceholders(frag, len(args)) and (for Search) ORDER BY / LIMIT / OFFSET.
//
// PIT uses the canonical inclusive bound valid_time <= $4 (no rounding; #349).
// Shared by Iterate and Search so both stay in lock-step.
func (s *entityStore) searchBaseQuery(entityName, modelVersion string, pit *time.Time) (string, []any) {
	tid := string(s.tenantID)
	if pit != nil {
		// Bi-temporal snapshot: inner DISTINCT ON picks the latest version per
		// entity visible at the snapshot; outer drops deletion-marker versions
		// AFTER the DISTINCT ON (so a delete shadows an older live version).
		baseQuery := `SELECT doc FROM (
		                SELECT DISTINCT ON (entity_id) doc
		                FROM entity_versions
		                WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3
		                  AND valid_time <= $4
		                  AND transaction_time <= CURRENT_TIMESTAMP
		                ORDER BY entity_id, valid_time DESC, transaction_time DESC
		             ) latest
		             WHERE (doc->'_meta'->>'deleted')::boolean IS NOT TRUE`
		return baseQuery, []any{tid, entityName, modelVersion, *pit}
	}
	baseQuery := `SELECT doc
	             FROM entities
	             WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted`
	return baseQuery, []any{tid, entityName, modelVersion}
}
```

- [ ] **Step 2: Point `Iterate` at the helper**

In `plugins/postgres/grouped_stats.go`, replace the inline base-query block (currently `grouped_stats.go:77-104`, the `tid := ...` through the `if opts.PointInTime != nil { ... } else { ... }` that sets `baseQuery`/`baseArgs`) with:

```go
	baseQuery, baseArgs := s.searchBaseQuery(model.EntityName, model.ModelVersion, opts.PointInTime)
```

Leave the rest of `Iterate` unchanged (the `if plan.where != "" { shifted := shiftPlaceholders(...) ... }` block and the `s.q.Query(...)` call still apply). Remove the now-unused `tid` local if the compiler flags it (it moved into the helper).

- [ ] **Step 3: Run the Iterate regression tests — expect PASS (no behavior change)**

Run: `cd plugins/postgres && go test ./... -run 'TestPostgresIterate' -v`
Expected: PASS (Docker must be running). The base SQL is byte-identical to before, so every Iterate test — current-state and point-in-time — still passes.

- [ ] **Step 4: Vet and commit**

Run: `cd plugins/postgres && go vet ./...`
Expected: clean.

```bash
git add plugins/postgres/search_base.go plugins/postgres/grouped_stats.go
git commit -m "refactor(postgres): extract shared searchBaseQuery from Iterate

Gate-6 dedup ahead of spi.Searcher: Iterate's current-state / point-in-time
base SELECT moves into searchBaseQuery so Search reuses the same literals.
Behavior-preserving; the PIT projection fix lands next.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: Implement current-state `Search` (`spi.Searcher`)

Add the `Search` method covering current-state queries: pushdown WHERE, residual post-filter, default + explicit `ORDER BY`, SQL/Go pagination, and injection-safe order-spec validation. Point-in-time is deliberately left for Task 3 (its B1 defect needs its own RED test).

**Files:**
- Create: `plugins/postgres/searcher.go`
- Modify: `plugins/postgres/path_validation.go` (add `validateOrderSpecs`)
- Test: `plugins/postgres/searcher_test.go`

**Interfaces:**
- Consumes: `searchBaseQuery` (Task 1); `planQuery`, `shiftPlaceholders`, `fieldExpr`, `directMetaColumns`, `jsonbExtractText`, `validateFilterPaths`, `validateJSONPath`, `postgresIter`, `unmarshalEntityDoc` (all already present).
- Produces: `func (s *entityStore) Search(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error)` (satisfies `spi.Searcher`); `validateOrderSpecs([]spi.OrderSpec) error`; `orderByClause(spi.SearchOptions) string`; `orderByFieldExpr(spi.OrderSpec) string`.

- [ ] **Step 1: Write the failing test (current-state Eq + numeric Gt/Between, AND, OR, residual, pagination, order, injection)**

Create `plugins/postgres/searcher_test.go`. It mirrors `plugins/sqlite/searcher_test.go` and reuses the existing postgres harness (`setupEntityTest`, `ctxWithTenant` from the `postgres_test` package). All values that exercise numeric translation use real numbers (spec caveat S4).

```go
package postgres_test

import (
	"context"
	"errors"
	"testing"

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

// Compile-time guard mirrored as a runtime assertion for clarity.
func TestPGSearcher_ImplementsSearcher(t *testing.T) {
	store, ctx := setupSearcher(t)
	if _, ok := store.(spi.Searcher); !ok {
		t.Fatal("postgres entityStore must implement spi.Searcher")
	}
	_ = ctx
}
```

- [ ] **Step 2: Run the tests to verify they FAIL to compile (no `Search` method yet)**

Run: `cd plugins/postgres && go test ./... -run 'TestPGSearcher' -v`
Expected: build failure — `store.(spi.Searcher)` is never satisfied / `Search` undefined. (If Docker is off, the harness skips; ensure Docker is running so the failure is the real one.)

- [ ] **Step 3: Add `validateOrderSpecs` to `path_validation.go`**

Append to `plugins/postgres/path_validation.go`:

```go
// validateOrderSpecs checks every OrderSpec.Path against the same dotted-
// identifier grammar as filter paths. Empty paths (direct-column / default
// ordering) are skipped. MUST be called at the Search() boundary before any
// OrderSpec.Path is interpolated into SQL (injection invariant).
func validateOrderSpecs(specs []spi.OrderSpec) error {
	for _, sp := range specs {
		if sp.Path == "" {
			continue
		}
		if err := validateJSONPath(sp.Path); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Create `searcher.go` with `Search`, `orderByClause`, `orderByFieldExpr`**

Create `plugins/postgres/searcher.go`:

```go
package postgres

import (
	"context"
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Compile-time check that *entityStore implements spi.Searcher.
var _ spi.Searcher = (*entityStore)(nil)

// Search implements spi.Searcher for the PostgreSQL entity store. Pushable
// predicates go into the SQL WHERE via planQuery; the residual (regex /
// case-insensitive ops) is evaluated in Go by postgresIter/evalPostFilter.
//
// Pagination: when there is no residual, LIMIT/OFFSET are pushed into SQL.
// With a residual, rows are streamed, post-filtered, and offset/limit applied
// in Go — early-stopping once offset+limit matches are gathered, but ONLY when
// Limit > 0 (an unbounded Limit<=0 request must drain all matches before
// applying the offset, else a naive offset+limit==offset stop returns empty).
//
// No scan budget (unlike sqlite): the production engine streams in SQL order
// and bounds memory via the early-stop when a limit is set. An unbounded
// request with a residual is O(n) memory — the same profile as the in-memory
// fallback it replaces.
func (s *entityStore) Search(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	if err := validateFilterPaths(filter); err != nil {
		return nil, err
	}
	if err := validateOrderSpecs(opts.OrderBy); err != nil {
		return nil, err
	}

	// Zero-value Filter means "match all" — skip planQuery (it would treat the
	// empty Op as non-pushable and install the zero filter as a residual).
	var plan sqlPlan
	if filter.Op != "" {
		plan = planQuery(filter)
	}

	baseQuery, baseArgs := s.searchBaseQuery(opts.ModelName, opts.ModelVersion, opts.PointInTime)

	if plan.where != "" {
		shifted := shiftPlaceholders(plan.where, len(baseArgs))
		baseQuery += " AND (" + shifted + ")"
		baseArgs = append(baseArgs, plan.args...)
	}

	baseQuery += orderByClause(opts)

	// No residual → LIMIT/OFFSET in SQL.
	if plan.postFilter == nil {
		if opts.Limit > 0 {
			baseQuery += fmt.Sprintf(" LIMIT $%d", len(baseArgs)+1)
			baseArgs = append(baseArgs, opts.Limit)
		}
		if opts.Offset > 0 {
			baseQuery += fmt.Sprintf(" OFFSET $%d", len(baseArgs)+1)
			baseArgs = append(baseArgs, opts.Offset)
		}
	}

	rows, err := s.q.Query(ctx, baseQuery, baseArgs...)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	it := &postgresIter{ctx: ctx, rows: rows, postFilter: plan.postFilter}
	defer it.Close()

	var results []*spi.Entity

	// No residual: SQL already applied LIMIT/OFFSET; collect everything.
	if plan.postFilter == nil {
		for it.Next() {
			results = append(results, it.Entity())
		}
		if err := it.Err(); err != nil {
			return nil, err
		}
		return results, nil
	}

	// Residual present: postgresIter yields only post-filter matches. Apply
	// offset/limit in Go. Early-stop only when a limit is set (S1 guard).
	for it.Next() {
		results = append(results, it.Entity())
		if opts.Limit > 0 && len(results) >= opts.Offset+opts.Limit {
			break
		}
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	if opts.Offset > 0 {
		if opts.Offset >= len(results) {
			return nil, nil
		}
		results = results[opts.Offset:]
	}
	if opts.Limit > 0 && len(results) > opts.Limit {
		results = results[:opts.Limit]
	}
	return results, nil
}

// orderByClause builds the SQL ORDER BY from opts.OrderBy. Empty → default
// "ORDER BY entity_id" (a unique, never-null key, so pages are deterministic).
// Bare column names resolve against the entities table (current-state) or the
// `latest` derived table (point-in-time), both of which expose entity_id.
func orderByClause(opts spi.SearchOptions) string {
	if len(opts.OrderBy) == 0 {
		return " ORDER BY entity_id"
	}
	clauses := make([]string, 0, len(opts.OrderBy))
	for _, spec := range opts.OrderBy {
		expr := orderByFieldExpr(spec)
		if spec.Desc {
			expr += " DESC"
		}
		clauses = append(clauses, expr)
	}
	return " ORDER BY " + strings.Join(clauses, ", ")
}

// orderByFieldExpr returns the SQL ordering expression for an OrderSpec,
// reusing the filter field rules (directMetaColumns / doc->'_meta'->>'p' /
// doc->>'p').
//
// Safety invariant: spec.Path is interpolated into a JSON-key literal and
// MUST have been validated by validateOrderSpecs at the Search() boundary.
func orderByFieldExpr(spec spi.OrderSpec) string {
	if spec.Source == spi.SourceMeta {
		if directMetaColumns[spec.Path] {
			return spec.Path
		}
		return jsonbExtractText("doc->'_meta'", spec.Path)
	}
	return jsonbExtractText("doc", spec.Path)
}
```

- [ ] **Step 5: Run the current-state tests to verify they PASS**

Run: `cd plugins/postgres && go test ./... -run 'TestPGSearcher' -v`
Expected: PASS for every `TestPGSearcher_*` (Docker required).

- [ ] **Step 6: Confirm the service layer now drives pushdown (no change, just verify) and vet**

Run: `cd plugins/postgres && go vet ./...`
Expected: clean. (No service-layer edit — `internal/domain/search/service.go` already type-asserts `spi.Searcher` and uses it when `tx == nil`.)

- [ ] **Step 7: Commit**

```bash
git add plugins/postgres/searcher.go plugins/postgres/path_validation.go plugins/postgres/searcher_test.go
git commit -m "feat(postgres): implement spi.Searcher for current-state search

Pushes supported predicates into SQL (JSONB ->>, numeric via cyoda_try_float8,
range, string ops), post-filters the residual via postgresIter, and applies
LIMIT/OFFSET in SQL (no residual) or Go (residual, with the Limit<=0 drain-then-
offset guard). Structural ORDER BY parity with sqlite; order paths are
injection-validated. Point-in-time covered next.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: Point-in-time `Search` + B1 projection fix

A point-in-time search appends `ORDER BY entity_id`, but the PIT base query's inner `DISTINCT ON` projects only `doc`, so `entity_id` is unresolved in the outer query — every PIT search fails at runtime. This task drives that defect out RED-first and fixes the inner projection in the shared helper (closing the latent `Iterate` gap too).

**Files:**
- Modify: `plugins/postgres/search_base.go` (inner `DISTINCT ON` projection)
- Test: `plugins/postgres/searcher_test.go` (add the PIT test)

**Interfaces:**
- Consumes: `searchBaseQuery` (Task 1), the `pitSetup`-style version-seeding pattern from `plugins/postgres/pit_boundary_test.go`.
- Produces: a working point-in-time `Search`; no new exported symbols.

- [ ] **Step 1: Write the failing point-in-time search test**

Add to `plugins/postgres/searcher_test.go`. It seeds two versions of one entity at distinct, deterministic `valid_time`s (the `pit_boundary_test.go` pattern: save twice, then force valid_times via direct SQL), then searches at the base timestamp and asserts the v1 snapshot is returned. The `factory.Pool()` accessor used here already exists (used by `pit_boundary_test.go`).

```go
import "time" // add to the existing import block

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
```

- [ ] **Step 2: Run it to verify it FAILS with the projection error**

Run: `cd plugins/postgres && go test ./... -run 'TestPGSearcher_PointInTimeDefaultOrder' -v`
Expected: FAIL — the query errors with something like `ERROR: column "entity_id" does not exist (SQLSTATE 42703)`, surfaced as `search query: ...`. This is the B1 defect manifesting.

- [ ] **Step 3: Fix the inner projection in `searchBaseQuery`**

In `plugins/postgres/search_base.go`, change the PIT branch's inner `DISTINCT ON` to also project the real `entity_versions` direct columns (so the outer `ORDER BY entity_id` and any direct-column pushdown resolve). The outer projection stays `SELECT doc`. Do **not** project `deleted` — it is doc-only on `entity_versions`.

```go
		baseQuery := `SELECT doc FROM (
		                SELECT DISTINCT ON (entity_id)
		                       entity_id, model_name, model_version, version, doc
		                FROM entity_versions
		                WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3
		                  AND valid_time <= $4
		                  AND transaction_time <= CURRENT_TIMESTAMP
		                ORDER BY entity_id, valid_time DESC, transaction_time DESC
		             ) latest
		             WHERE (doc->'_meta'->>'deleted')::boolean IS NOT TRUE`
```

(Only the inner `SELECT DISTINCT ON (entity_id) doc` line changes — it becomes the multi-column projection above. Everything else in the helper is untouched.)

- [ ] **Step 4: Run the PIT test to verify it PASSES**

Run: `cd plugins/postgres && go test ./... -run 'TestPGSearcher_PointInTimeDefaultOrder' -v`
Expected: PASS.

- [ ] **Step 5: Run the whole postgres suite to confirm no regression (Iterate now also benefits from the fix)**

Run: `cd plugins/postgres && go test ./... -v 2>&1 | tail -30`
Expected: PASS, including all `TestPostgresIterate_*` and `TestPGSearcher_*`.

- [ ] **Step 6: Commit**

```bash
git add plugins/postgres/search_base.go plugins/postgres/searcher_test.go
git commit -m "fix(postgres): project direct columns in PIT inner DISTINCT ON (B1)

A point-in-time search appends ORDER BY entity_id, but the PIT base query's
inner DISTINCT ON projected only doc, so entity_id was unresolved and every
PIT search errored. Project entity_id/model_name/model_version/version on the
inner select (outer stays SELECT doc; deleted is doc-only, not projected).
Also closes the latent same-shape gap in Iterate, which shares the helper.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: Cross-backend parity scenario `RunSearchPointInTime`

Add the one genuinely backend-agnostic new behavior to the parity suite: a predicate search at a snapshot, asserting pushdown ≡ in-memory across memory / sqlite / postgres (and the commercial backend). Needs a point-in-time sync-search client helper.

**Files:**
- Modify: `e2e/parity/client/http.go` (add `SyncSearchAt`)
- Create: `e2e/parity/search_pit.go`
- Modify: `e2e/parity/registry.go` (register the scenario)
- Test: the scenario itself runs under `TestParity` across backends.

**Interfaces:**
- Consumes: `client.Client` (`CreateEntity`, `UpdateEntityData`, `GetEntityChanges`, the new `SyncSearchAt`); `BackendFixture`; the `searchWorkflowJSON`/`setupSearchModel` helpers in `e2e/parity/search.go`; the `pit_boundary.go` timing pattern.
- Produces: `func RunSearchPointInTime(t *testing.T, fixture BackendFixture)`; `func (c *Client) SyncSearchAt(t *testing.T, modelName string, modelVersion int, condition string, at time.Time) ([]EntityResult, error)`.

- [ ] **Step 1: Add the `SyncSearchAt` client helper**

In `e2e/parity/client/http.go`, immediately after `SyncSearch` (ends ~line 1218), add a point-in-time variant. It targets the same direct-search endpoint with a `pointInTime` query param (the handler reads `params.PointInTime`, `internal/domain/search/handler.go:126`) and parses the same NDJSON response.

```go
// SyncSearchAt issues POST /api/search/direct/{name}/{version}?pointInTime=<t>
// with the given condition JSON and returns the entity results at the snapshot.
// Mirrors SyncSearch (NDJSON response); the direct-search handler honours the
// pointInTime query param.
func (c *Client) SyncSearchAt(t *testing.T, modelName string, modelVersion int, condition string, at time.Time) ([]EntityResult, error) {
	t.Helper()
	path := fmt.Sprintf("/api/search/direct/%s/%d?pointInTime=%s",
		modelName, modelVersion, at.UTC().Format(time.RFC3339Nano))
	raw, err := c.doRaw(t, http.MethodPost, path, condition)
	if err != nil {
		return nil, err
	}
	var results []EntityResult
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		var r EntityResult
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			return nil, fmt.Errorf("decode NDJSON line: %w", err)
		}
		results = append(results, r)
	}
	return results, nil
}
```

(`time`, `net/http`, `strings`, `encoding/json`, `fmt` are already imported in `http.go`.)

- [ ] **Step 2: Write the parity scenario**

Create `e2e/parity/search_pit.go`. The timing mirrors `pit_boundary.go`: space writes ≥1ms apart, capture each version's change time via `GetEntityChanges`, then search at the captured snapshot.

```go
package parity

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunSearchPointInTime asserts that a predicate search at a snapshot returns
// the snapshot-correct result set on every backend — exercising the Searcher
// (or in-memory fallback) through a point-in-time read. It is the cross-engine
// guard for predicate-pushdown ≡ in-memory at a PIT, and the parity counterpart
// to the postgres B1 projection unit test.
func RunSearchPointInTime(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-pit"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	// v1: status=active.
	id, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	t1 := latestChangeTime(t, c, id)

	// Space ≥1ms so v2 lands in a distinct millisecond (commercial backend
	// stores ms precision — see pit_boundary.go rationale).
	time.Sleep(2 * time.Millisecond)
	if err := c.UpdateEntityData(t, id, `{"name":"Alice","status":"inactive"}`); err != nil {
		t.Fatalf("UpdateEntityData: %v", err)
	}
	t2 := latestChangeTime(t, c, id)
	if !t1.Before(t2) {
		t.Fatalf("version timestamps not increasing: t1=%s t2=%s",
			t1.Format(time.RFC3339Nano), t2.Format(time.RFC3339Nano))
	}

	activeCond := `{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"active"}`

	// At t1 the snapshot shows status=active → 1 hit.
	at1, err := c.SyncSearchAt(t, modelName, modelVersion, activeCond, t1)
	if err != nil {
		t.Fatalf("SyncSearchAt(t1): %v", err)
	}
	if len(at1) != 1 {
		t.Errorf("status=active @t1: want 1, got %d", len(at1))
	}

	// At t2 the snapshot shows status=inactive → the active search misses.
	at2, err := c.SyncSearchAt(t, modelName, modelVersion, activeCond, t2)
	if err != nil {
		t.Fatalf("SyncSearchAt(t2): %v", err)
	}
	if len(at2) != 0 {
		t.Errorf("status=active @t2: want 0 (now inactive), got %d", len(at2))
	}
}

// latestChangeTime returns the most recent change timestamp for the entity.
func latestChangeTime(t *testing.T, c *client.Client, id interface{ String() string }) time.Time {
	t.Helper()
	// id is a uuid.UUID; reuse the pit_boundary helper signature.
	return pitbLatestChangeTime(t, c, mustUUID(id))
}
```

Note on the helper: `pit_boundary.go` already defines `pitbLatestChangeTime(t, c, uuid.UUID)`. To avoid a second copy, **do not** add `latestChangeTime`/`mustUUID` above — instead call `pitbLatestChangeTime` directly with the `uuid.UUID` that `CreateEntity` returns. Replace the two `latestChangeTime(t, c, id)` calls with `pitbLatestChangeTime(t, c, id)` and delete the `latestChangeTime`/`mustUUID` shim. (`CreateEntity` returns `uuid.UUID`, matching `pitbLatestChangeTime`'s signature.) Final imports: `testing`, `time`, and `client`.

So the committed file uses `id` (a `uuid.UUID`) with `pitbLatestChangeTime(t, c, id)` and contains no shim helper.

- [ ] **Step 3: Register the scenario**

In `e2e/parity/registry.go`, add to the search block (after line 98, `{"SearchAfterUpdate", RunSearchAfterUpdate},`):

```go
	{"SearchPointInTime", RunSearchPointInTime},
```

- [ ] **Step 4: Build the parity package and run the scenario on the fast backends**

Run: `go test ./e2e/parity/... -run 'TestParity' -v 2>&1 | grep -iE 'SearchPointInTime|FAIL|ok' | head`
Expected: `SearchPointInTime` passes on the memory and sqlite fixtures (and postgres if that fixture runs in your environment; Docker required for postgres). If only memory/sqlite run locally, that is sufficient here — postgres parity also runs in CI.

- [ ] **Step 5: Commit**

```bash
git add e2e/parity/client/http.go e2e/parity/search_pit.go e2e/parity/registry.go
git commit -m "test(parity): add SearchPointInTime cross-backend scenario

Predicate search at a snapshot must return the snapshot-correct set on every
backend — the parity guard for pushdown ≡ in-memory at a PIT. Adds a
point-in-time SyncSearchAt client helper.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: CHANGELOG + full verification

**Files:**
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Add the CHANGELOG entry**

Open `CHANGELOG.md`, find the current unreleased section, and add under its performance/changed list (match the file's existing bullet style). Keep the wording bounded — constant-factor, not index-grade (no GIN index is added):

```markdown
- **Performance:** PostgreSQL search now pushes supported predicates into SQL
  (JSONB operators, numeric/range/string comparisons) instead of loading every
  entity of a model and filtering in memory. Non-pushable operators (regex,
  case-insensitive) are post-filtered while streaming. This is a constant-factor
  win (no full-result transfer/decode, DB-side filtering, LIMIT pushdown) — not
  a JSON-path-index speedup; adding indexes remains a separate operational step.
```

- [ ] **Step 2: Commit the CHANGELOG**

```bash
git add CHANGELOG.md
git commit -m "docs(changelog): note PostgreSQL search predicate pushdown

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

- [ ] **Step 3: Full verification — root module + postgres plugin**

Run: `go build ./... && go vet ./...`
Expected: clean.

Run: `cd plugins/postgres && go vet ./... && go test ./... 2>&1 | tail -20`
Expected: PASS (Docker required).

Run (root, includes e2e + parity; Docker required): `go test ./... 2>&1 | tail -30`
Expected: PASS. Pay attention to `e2e/parity` (the new scenario) and `internal/e2e` search tests.

- [ ] **Step 4: Race sanity check (once, before PR)**

Run: `make race`
Expected: PASS. (Per `.claude/rules/race-testing.md`, this is the single end-of-deliverable race run; the target excludes `internal/e2e` by design.)

- [ ] **Step 5: Verify the spec/plan are committed and the tree is clean**

Run: `git status --short`
Expected: clean (spec + plan already committed; all task commits landed).

---

## Self-Review

**Spec coverage:**
- "Implement `spi.Searcher` on postgres `entityStore`" → Tasks 2 (current-state) + 3 (point-in-time). ✓
- "Reuse existing substrate (`planQuery`/`evalPostFilter`/`postgresIter`/`shiftPlaceholders`/`unmarshalEntityDoc`)" → Task 2 `Search` reuses all. ✓
- "B1 PIT projection fix (inner direct columns, outer stays `SELECT doc`, no `deleted`)" → Task 3. ✓
- "Gate-6 dedup: shared base-query helper, parameterized by name+version, carrying the B1 fix; #350 left `grouped_stats.go` unchanged so #37 extracts it" → Task 1 (extract) + Task 3 (fix lands in the shared helper, benefiting Iterate). ✓
- "No scan budget; early-stop only when Limit>0; Limit<=0 drain-then-offset" → Task 2 `Search` + `TestPGSearcher_UnboundedOffsetWithResidual`. ✓
- "`OrderBy` structural parity; default `entity_id`; injection-validated paths" → Task 2 `orderByClause`/`orderByFieldExpr`/`validateOrderSpecs` + order tests + injection tests. ✓
- "PIT bound = canonical raw `<=`, no rounding" → Task 1 helper uses `valid_time <= $4` verbatim; no `Truncate`. ✓
- "Numeric EQ/GT/BETWEEN exercised live (S4)" → Task 2 uses `float64` values. ✓
- "Coverage-matrix waiver (no new API/error/status)" → no endpoint/error-code task needed; the only domain error `ErrInvalidFilterPath` already has its help topic and pre-existing handling. ✓
- "Required `RunSearchPointInTime` parity scenario" → Task 4. ✓
- "CHANGELOG additive, wording bounded; no SPI bump / COMPATIBILITY change / new env var / help topic" → Task 5; no go.mod or docs/config edits. ✓

**Placeholder scan:** No TBD/TODO; every code step has complete code. ✓

**Type consistency:** `searchBaseQuery(entityName, modelVersion string, pit *time.Time) (string, []any)` is defined in Task 1 and called identically in Iterate (Task 1) and Search (Task 2). `Search`/`orderByClause`/`orderByFieldExpr`/`validateOrderSpecs` signatures match between definition (Task 2) and use. `pitbLatestChangeTime(t, c, uuid.UUID)` is the existing helper reused in Task 4. `ErrInvalidFilterPath` is the existing exported sentinel asserted in injection tests. ✓

**Note for the implementer:** Tasks are sequential (each builds on the prior). Postgres-plugin tests and the root `./...`/`e2e` tests require Docker (testcontainers). Run `cd plugins/postgres && go test ./...` for the plugin — root `./...` does not recurse into the submodule.
