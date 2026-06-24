# Grouped Entity Statistics Query — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `POST /api/entity/stats/{entityName}/{modelVersion}/query` — a synchronous grouped-stats endpoint that returns aggregate counts and Tier 1+2 aggregations (sum/avg/min/max/stdev) grouped by entity data fields and/or lifecycle state, with predicate pushdown to memory/sqlite/postgres backends.

**Architecture:** Two new optional SPI interfaces — `spi.Iterable` (filter-aware streaming) and `spi.GroupedAggregator` (native GROUP BY pushdown). Service-layer dispatch tries pushdown first, falls back to streaming-tally with Welford for stdev. In-tree plugins (memory, sqlite, postgres) implement both; cassandra is out-of-tree and tracked separately.

**Tech Stack:** Go 1.26+, `log/slog`, PostgreSQL (via pgx), SQLite (via stdlib `database/sql`), in-memory map plugin. Testcontainers-go for postgres e2e. Existing search Condition DSL reused.

**Spec:** `docs/superpowers/specs/2026-06-14-issue-299-grouped-stats-design.md`

---

## File Structure

### Phase 1 — cyoda-go-spi (sibling repo at `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`)

- **Create** `iterable.go` — `Iterable`, `Iterator`, `IterateOptions`; sentinels `ErrGroupCardinalityExceeded`, `ErrAggregationNotPushdownable`.
- **Create** `grouped_aggregator.go` — `GroupedAggregator`, `GroupExpr`, `GroupExprKind`, `AggregateOp`, `AggregateExpr`, `GroupedAggregationsOptions`, `GroupKeyEntry`, `GroupedAggregateBucket`.
- **Modify** `CHANGELOG.md` — note v0.8.0 additions.

### Phase 2 — cyoda-go foundations (this repo)

- **Modify** `go.mod` — bump `cyoda-go-spi` pin to v0.8.0 (or its pseudo-version once published).
- **Modify** `internal/match/match.go` — add `MatchFilter(f spi.Filter, data []byte, meta spi.EntityMeta) bool`.
- **Create** `internal/domain/entity/grouped_stats_types.go` — DTOs (`GroupedStatsRequest`, `GroupedStatsBucket`, `GroupKeyEntry`, `GroupExprWire`, `AggregationExprWire`, etc.).
- **Create** `internal/domain/entity/grouped_stats_validation.go` — request validation + path-shape checks.
- **Create** `internal/domain/entity/grouped_stats_accumulator.go` — per-bucket Welford state; group-key encoding (D18); top-N heap; materialize.
- **Create** `internal/domain/entity/grouped_stats_service.go` — capability sniff, dispatch, `tallyStreaming`.
- **Create** `internal/domain/entity/grouped_stats_handler.go` — HTTP handler.
- **Modify** `internal/api/router.go` (or equivalent) — wire route.
- **Create** `internal/config/grouped_stats.go` — `CYODA_STATS_GROUP_MAX` env var + `DefaultConfig()` update.

### Phase 3 — in-tree plugins

- **Modify** `plugins/memory/entity_store.go` — godoc invariant on `entityVersion`; comment in `saveUnlocked`.
- **Create** `plugins/memory/grouped_stats.go` — memory `Iterate` (snapshot-then-iterate; in-tx overlay) + `GroupedAggregator`.
- **Create** `plugins/sqlite/grouped_stats.go` — sqlite `Iterate` (planQuery reuse) + `GroupedAggregator` (declines stdev).
- **Create** `plugins/postgres/query_planner.go` — Filter→SQL translator (greedy AND / conservative OR; JSONB ops).
- **Create** `plugins/postgres/migrations/000000N_grouped_stats.up.sql` + `.down.sql` — `cyoda_try_float8` + `entities_state_idx`.
- **Create** `plugins/postgres/grouped_stats.go` — postgres `Iterate` + `GroupedAggregator`.

### Phase 4 — tests, docs, config

- **Create** `internal/e2e/parity/grouped_stats.go` — SPI-contract parity cases.
- **Modify** `internal/e2e/parity/registry.go` — register the new cases.
- **Create** `internal/e2e/grouped_stats_test.go` — e2e via postgres testcontainer.
- **Modify** `api/openapi.yaml` — new path + schemas.
- **Modify** `cmd/cyoda/help/content/search.md` — help topic updates.
- **Modify** `cmd/cyoda/help/content/config/server.md` (or whichever holds env var docs) — `CYODA_STATS_GROUP_MAX`.
- **Modify** `README.md` — config reference.
- **Modify** `COMPATIBILITY.md` — SPI pin bump.

---

## Task ordering & dependencies

- **Phase 1** must complete and land (at least as a pseudo-version on cyoda-go-spi `main`) before Phase 2 Task 3.
- Within Phase 2, Tasks 4–7 (foundations) are mostly independent; Task 8 (service dispatch) depends on Tasks 4, 5, 6, 7.
- Within Phase 3, plugins are independent of each other but each depends on Phase 2 being complete. Postgres has internal ordering: translator (Task 14) before grouped-aggregate (Task 16).
- Phase 4 parity tests depend on at least one plugin from Phase 3 working; e2e depends on postgres.

---

# Phase 1 — cyoda-go-spi

**Working directory for Phase 1:** `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`

Create a feature branch off `main` (e.g. `feat/299-iterable-grouped-aggregator`). Open a PR on cyoda-go-spi for review.

### Task 1: Add `Iterable` and `Iterator` SPI

**Files:**
- Create: `iterable.go`
- Create: `iterable_test.go`

- [ ] **Step 1: Write the failing test** (compile-only contract test)

```go
// iterable_test.go
package spi_test

import (
    "context"
    "testing"
    "time"

    spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestIterableContract verifies the Iterable/Iterator interfaces compile
// and the expected method set is present. Runtime behavior is tested by
// plugin implementations in their own repos.
func TestIterableContract(t *testing.T) {
    var _ spi.Iterable = (spi.Iterable)(nil)
    var _ spi.Iterator = (spi.Iterator)(nil)

    var opts spi.IterateOptions
    now := time.Now()
    opts.PointInTime = &now
    _ = opts

    // Verify Iterate signature
    var iter spi.Iterable
    if iter != nil {
        _, _ = iter.Iterate(context.Background(), spi.ModelRef{}, spi.Filter{}, spi.IterateOptions{})
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test -run TestIterableContract ./...`
Expected: FAIL with "undefined: spi.Iterable" or similar.

- [ ] **Step 3: Write minimal implementation**

```go
// iterable.go
package spi

import (
    "context"
    "errors"
    "time"
)

// Iterable is an optional capability on a storage backend that yields
// entities matching a filter, one at a time, with bounded memory.
//
// Semantics:
//   - Plugins push pushable parts of the filter into storage (SQL WHERE,
//     CQL index lookup); residual is applied inside Next() before yielding.
//   - A zero-value Filter means "yield all entities for the model"
//     (subject to opts).
//   - Implementations MUST NOT hold a global write-blocking lock for the
//     lifetime of the iterator (snapshot-then-iterate or cursor-based).
//   - The iterator MUST observe ctx cancellation: the underlying driver
//     surfaces an error; the iterator reports it via Err() and Next()
//     returns false.
//   - No retry on transient driver errors. First error is sticky.
//   - Close() is idempotent.
type Iterable interface {
    Iterate(
        ctx context.Context,
        model ModelRef,
        filter Filter,
        opts IterateOptions,
    ) (Iterator, error)
}

// Iterator yields entities one at a time. Standard Go iterator shape
// modeled after database/sql.Rows.
type Iterator interface {
    // Next advances the iterator. Returns false on end or sticky error.
    Next() bool
    // Entity returns the current row. Valid only after Next() == true.
    Entity() *Entity
    // Err returns the first error encountered. Sticky.
    Err() error
    // Close releases server resources. Idempotent.
    Close() error
}

// IterateOptions narrows or shifts the iteration window.
type IterateOptions struct {
    // PointInTime, when non-nil, requests a historical snapshot at the
    // given instant. Semantics match the rest of the SPI (read-committed
    // snapshot).
    PointInTime *time.Time
}

// ErrGroupCardinalityExceeded is returned by GroupedAggregator
// implementations (or surfaced by the service-layer streaming tally)
// when the result group count would exceed the configured ceiling.
var ErrGroupCardinalityExceeded = errors.New("group cardinality exceeded ceiling")

// ErrAggregationNotPushdownable signals that a GroupedAggregator
// implementation cannot safely push down a specific request shape; the
// caller (typically the service layer) should fall through to the
// streaming-tally path via Iterable.
var ErrAggregationNotPushdownable = errors.New("aggregation not pushdownable on this backend")
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test -run TestIterableContract ./...`
Expected: PASS

- [ ] **Step 5: Vet**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go vet ./...`
Expected: no output (clean).

- [ ] **Step 6: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add iterable.go iterable_test.go
git commit -m "feat(spi): add Iterable + Iterator + IterateOptions (closes part of #299)"
```

---

### Task 2: Add `GroupedAggregator` and related types

**Files:**
- Create: `grouped_aggregator.go`
- Create: `grouped_aggregator_test.go`

- [ ] **Step 1: Write the failing test**

```go
// grouped_aggregator_test.go
package spi_test

import (
    "context"
    "testing"

    spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestGroupedAggregatorContract(t *testing.T) {
    var _ spi.GroupedAggregator = (spi.GroupedAggregator)(nil)

    g := spi.GroupExpr{Kind: spi.GroupExprState}
    if g.Kind != spi.GroupExprState {
        t.Fatalf("GroupExprState mismatch")
    }
    g2 := spi.GroupExpr{Kind: spi.GroupExprDataPath, Path: "$.x"}
    if g2.Kind != spi.GroupExprDataPath || g2.Path != "$.x" {
        t.Fatalf("GroupExprDataPath mismatch")
    }

    for _, op := range []spi.AggregateOp{
        spi.AggSum, spi.AggAvg, spi.AggMin, spi.AggMax, spi.AggStdev,
    } {
        if op == "" {
            t.Fatalf("aggregate op is empty")
        }
    }

    var ga spi.GroupedAggregator
    if ga != nil {
        _, _ = ga.GroupedAggregate(
            context.Background(),
            spi.ModelRef{},
            []spi.GroupExpr{{Kind: spi.GroupExprState}},
            spi.Filter{},
            spi.GroupedAggregationsOptions{MaxBuckets: 10000},
        )
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test -run TestGroupedAggregatorContract ./...`
Expected: FAIL with "undefined: spi.GroupedAggregator" or similar.

- [ ] **Step 3: Write minimal implementation**

```go
// grouped_aggregator.go
package spi

import (
    "context"
    "time"
)

// GroupedAggregator is an optional capability on a storage backend that
// answers a grouped-stats query natively (e.g. via SQL GROUP BY).
//
// May decline a specific request shape via ErrAggregationNotPushdownable;
// the caller (typically the service layer) should then fall through to
// the streaming-tally path via Iterable.
type GroupedAggregator interface {
    GroupedAggregate(
        ctx context.Context,
        model ModelRef,
        groupBy []GroupExpr,
        filter Filter,
        opts GroupedAggregationsOptions,
    ) ([]GroupedAggregateBucket, error)
}

// GroupExprKind selects between the lifecycle state and a scalar data path.
type GroupExprKind int

const (
    // GroupExprState groups by the entity's lifecycle state.
    GroupExprState GroupExprKind = iota
    // GroupExprDataPath groups by a scalar JSONPath into entity data.
    GroupExprDataPath
)

// GroupExpr is one dimension of the group-by.
type GroupExpr struct {
    Kind GroupExprKind
    // Path is the JSONPath; only meaningful when Kind == GroupExprDataPath.
    Path string
}

// AggregateOp enumerates the supported per-bucket aggregations.
type AggregateOp string

const (
    AggSum   AggregateOp = "sum"
    AggAvg   AggregateOp = "avg"
    AggMin   AggregateOp = "min"
    AggMax   AggregateOp = "max"
    // AggStdev is sample standard deviation (n-1 denominator).
    AggStdev AggregateOp = "stdev"
)

// AggregateExpr is one requested aggregation.
type AggregateExpr struct {
    Op    AggregateOp
    Field string // scalar JSONPath
    // Alias is the response key. If blank, the server synthesizes
    // <op>_<field>.
    Alias string
}

// GroupedAggregationsOptions parameterizes the GroupedAggregate call.
type GroupedAggregationsOptions struct {
    PointInTime  *time.Time
    // MaxBuckets is the result cardinality ceiling. The implementation
    // must return ErrGroupCardinalityExceeded if the result would exceed
    // this count.
    MaxBuckets   int
    Aggregations []AggregateExpr
}

// GroupKeyEntry is one (path, value) pair in a bucket's key.
type GroupKeyEntry struct {
    Path  string
    // Value is the JSON-typed value: string for scalar/state values, nil
    // for missing/literal-null/non-scalar extracted values.
    Value any
}

// GroupedAggregateBucket is one row of the grouped-stats result.
type GroupedAggregateBucket struct {
    // GroupKey is ordered, matching the request groupBy order.
    GroupKey     []GroupKeyEntry
    Count        int64
    // Aggregations maps alias to float64 or nil. nil means the bucket had
    // zero numeric samples for that field.
    Aggregations map[string]any
}

var _ time.Time // keep time import in case future fields land here
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./...`
Expected: PASS, both contract tests.

- [ ] **Step 5: Vet and tidy**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go vet ./... && go mod tidy`
Expected: clean.

- [ ] **Step 6: Update CHANGELOG.md**

Append under v0.8.0 (unreleased) heading:
```
- Added `Iterable` / `Iterator` / `IterateOptions` SPI for filter-aware streaming iteration (#299 in cyoda-go).
- Added `GroupedAggregator` SPI for native GROUP BY pushdown plus `GroupExpr`, `AggregateOp`, `AggregateExpr`, `GroupedAggregationsOptions`, `GroupKeyEntry`, `GroupedAggregateBucket` (#299 in cyoda-go).
- Added sentinels `ErrGroupCardinalityExceeded`, `ErrAggregationNotPushdownable`.
```

- [ ] **Step 7: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add grouped_aggregator.go grouped_aggregator_test.go CHANGELOG.md
git commit -m "feat(spi): add GroupedAggregator + aggregation types (closes part of #299 in cyoda-go)"
```

- [ ] **Step 8: Open the cyoda-go-spi PR**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git push -u origin HEAD
gh pr create --title "feat(spi): Iterable + GroupedAggregator for grouped-stats" \
  --body "Additive SPI surface for cyoda-go #299. See cyoda-go spec docs/superpowers/specs/2026-06-14-issue-299-grouped-stats-design.md.

**Do not tag.** Per MAINTAINING.md \"When to tag\", the v0.8.0 SPI tag is cut at end-of-milestone once all v0.8.0-targeted SPI changes are merged."
```

> **HUMAN GATE:** Wait for review/merge of the cyoda-go-spi PR before continuing. Once merged, capture the resulting commit SHA on `main` — Task 3 bumps cyoda-go's pseudo-version pin to that SHA. The SPI tag is NOT cut here; it's cut at end-of-milestone (see spec D13).

---

# Phase 2 — cyoda-go foundations

**Working directory for Phase 2 onwards:** `/Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/feat-299-grouped-stats` (current branch: `feat/299-grouped-stats`).

### Task 3: Bump cyoda-go-spi pseudo-version pin (all four go.mod files)

**Files:**
- Modify: `go.mod`, `go.sum`
- Modify: `plugins/memory/go.mod`, `plugins/memory/go.sum`
- Modify: `plugins/sqlite/go.mod`, `plugins/sqlite/go.sum`
- Modify: `plugins/postgres/go.mod`, `plugins/postgres/go.sum`

**Context.** cyoda-go-spi `main` HEAD now includes Task 1/2 SPI changes. We bump cyoda-go's pseudo-version pin to that SHA in **all four** go.mod files (root + three plugins) — `scripts/check-spi-pin-sync.sh` CI gate enforces alignment. The final pin bump from pseudo-version to `v0.8.0` happens at end-of-milestone once the SPI tag is cut.

`GOPRIVATE=github.com/Cyoda-platform/*` must be set in your shell (already in CONTRIBUTING.md and CI workflows). It bypasses sum.golang.org for cyoda-platform modules.

- [ ] **Step 1: Capture the SPI HEAD SHA**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git checkout main && git pull --ff-only
SPI_SHA=$(git rev-parse --short=12 HEAD)
echo "SPI HEAD short SHA: $SPI_SHA"
```

- [ ] **Step 2: Bump root + plugins**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/feat-299-grouped-stats
export GOPRIVATE=github.com/Cyoda-platform/*
go get github.com/cyoda-platform/cyoda-go-spi@$SPI_SHA && go mod tidy
for d in plugins/memory plugins/sqlite plugins/postgres; do
  (cd "$d" && go get github.com/cyoda-platform/cyoda-go-spi@$SPI_SHA && go mod tidy)
done
```

- [ ] **Step 3: Verify pin-sync**

```bash
./scripts/check-spi-pin-sync.sh
```
Expected: `OK — all manifests pin cyoda-go-spi <pseudo-version>`.

- [ ] **Step 4: Vet + short tests**

```bash
go vet ./... && go test -short ./...
```
Expected: clean + green.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum plugins/*/go.mod plugins/*/go.sum
git commit -m "chore: bump cyoda-go-spi pseudo-version pin (Iterable + GroupedAggregator); refs #299"
```

---

### Task 4: Add `match.MatchFilter`

**Files:**
- Modify: `internal/match/match.go`
- Create: `internal/match/match_filter_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/match/match_filter_test.go
package match_test

import (
    "encoding/json"
    "testing"

    spi "github.com/cyoda-platform/cyoda-go-spi"
    "github.com/cyoda-platform/cyoda-go/internal/match"
)

func TestMatchFilter_EqString(t *testing.T) {
    data, _ := json.Marshal(map[string]any{"variantId": "v1"})
    f := spi.Filter{
        Op:     spi.FilterEq,
        Path:   "variantId",
        Source: spi.SourceData,
        Value:  "v1",
    }
    if !match.MatchFilter(f, data, spi.EntityMeta{}) {
        t.Fatalf("expected MatchFilter to be true for matching data")
    }
    f.Value = "v2"
    if match.MatchFilter(f, data, spi.EntityMeta{}) {
        t.Fatalf("expected MatchFilter to be false for non-matching data")
    }
}

func TestMatchFilter_EmptyFilterMatchesAll(t *testing.T) {
    data, _ := json.Marshal(map[string]any{"x": 1})
    if !match.MatchFilter(spi.Filter{}, data, spi.EntityMeta{}) {
        t.Fatalf("zero-value Filter should match all")
    }
}

func TestMatchFilter_StateEq(t *testing.T) {
    f := spi.Filter{
        Op:     spi.FilterEq,
        Source: spi.SourceLifecycleState,
        Value:  "available",
    }
    if !match.MatchFilter(f, nil, spi.EntityMeta{State: "available"}) {
        t.Fatalf("expected state match")
    }
    if match.MatchFilter(f, nil, spi.EntityMeta{State: "shipped"}) {
        t.Fatalf("expected state non-match")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/match/... -run TestMatchFilter -v`
Expected: FAIL with "undefined: match.MatchFilter".

- [ ] **Step 3: Write minimal implementation**

Open `internal/match/match.go` and add the function at the end:

```go
// MatchFilter evaluates an SPI Filter against an entity. Filter is the
// pushdown-friendly subset of predicate.Condition used by GroupedAggregator,
// Iterable, and the existing Searcher. Used by the memory plugin's Iterate
// to apply filters inside Next() and by the streaming-tally path when a
// filter has a residual.
//
// A zero-value filter (no Op, no Source, no Path) matches everything.
func MatchFilter(f spi.Filter, data []byte, meta spi.EntityMeta) bool {
    // Zero-value filter == match all.
    if f.Op == 0 && f.Path == "" && f.Source == 0 && len(f.Children) == 0 {
        return true
    }
    return evalFilter(f, data, meta)
}

func evalFilter(f spi.Filter, data []byte, meta spi.EntityMeta) bool {
    // Group filters.
    switch f.Op {
    case spi.FilterAnd:
        for _, c := range f.Children {
            if !evalFilter(c, data, meta) {
                return false
            }
        }
        return true
    case spi.FilterOr:
        for _, c := range f.Children {
            if evalFilter(c, data, meta) {
                return true
            }
        }
        return false
    case spi.FilterNot:
        if len(f.Children) != 1 {
            return false
        }
        return !evalFilter(f.Children[0], data, meta)
    }

    // Leaf filters.
    var actual any
    switch f.Source {
    case spi.SourceLifecycleState:
        actual = meta.State
    case spi.SourceData:
        actual = gjsonGet(data, f.Path)
    default:
        return false
    }

    switch f.Op {
    case spi.FilterEq:
        return compareEq(actual, f.Value)
    case spi.FilterNe:
        return !compareEq(actual, f.Value)
    case spi.FilterGt, spi.FilterGte, spi.FilterLt, spi.FilterLte:
        return compareOrd(actual, f.Value, f.Op)
    case spi.FilterIsNull:
        return actual == nil
    case spi.FilterNotNull:
        return actual != nil
    case spi.FilterContains, spi.FilterStartsWith, spi.FilterEndsWith, spi.FilterLike:
        return compareString(actual, f.Value, f.Op)
    }
    return false
}
```

Add helper functions `gjsonGet`, `compareEq`, `compareOrd`, `compareString` reusing existing patterns from `match.Match`. If those helpers already exist in this package, just call them.

**Inspect `internal/match/match.go` first** to identify which helpers are already there. The existing `Match` function evaluates `predicate.Condition`; the helpers it uses for value extraction and comparison should be reused. Adjust the implementation above to call existing helpers rather than duplicating them.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/match/... -v`
Expected: all tests pass.

- [ ] **Step 5: Vet**

Run: `go vet ./internal/match/...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/match/match.go internal/match/match_filter_test.go
git commit -m "feat(match): add MatchFilter for Filter-typed in-process evaluation (refs #299)"
```

---

### Task 5: Add request DTOs

**Files:**
- Create: `internal/domain/entity/grouped_stats_types.go`
- Create: `internal/domain/entity/grouped_stats_types_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/domain/entity/grouped_stats_types_test.go
package entity_test

import (
    "encoding/json"
    "testing"

    "github.com/cyoda-platform/cyoda-go/internal/domain/entity"
)

func TestGroupedStatsRequest_DecodeBasic(t *testing.T) {
    raw := `{
        "groupBy":      ["$.variantId", "state"],
        "limit":        100
    }`
    var r entity.GroupedStatsRequest
    if err := json.Unmarshal([]byte(raw), &r); err != nil {
        t.Fatalf("decode: %v", err)
    }
    if len(r.GroupBy) != 2 {
        t.Fatalf("groupBy len=%d, want 2", len(r.GroupBy))
    }
    if r.Limit == nil || *r.Limit != 100 {
        t.Fatalf("limit not parsed")
    }
}

func TestGroupedStatsBucket_EncodeOmitsAggregationsWhenEmpty(t *testing.T) {
    b := entity.GroupedStatsBucket{
        GroupKey: []entity.GroupKeyEntryWire{
            {Path: "$.variantId", Value: "v1"},
        },
        Count: 42,
    }
    raw, err := json.Marshal(b)
    if err != nil {
        t.Fatalf("encode: %v", err)
    }
    if got := string(raw); contains(got, "aggregations") {
        t.Fatalf("aggregations should be omitted: %s", got)
    }
}

func contains(s, sub string) bool {
    for i := 0; i+len(sub) <= len(s); i++ {
        if s[i:i+len(sub)] == sub {
            return true
        }
    }
    return false
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/entity/... -run TestGroupedStats -v`
Expected: FAIL with "undefined: entity.GroupedStatsRequest".

- [ ] **Step 3: Write minimal implementation**

```go
// internal/domain/entity/grouped_stats_types.go
package entity

import "time"

// GroupedStatsRequest is the body of POST
// /api/entity/stats/{entityName}/{modelVersion}/query.
//
// See docs/superpowers/specs/2026-06-14-issue-299-grouped-stats-design.md §3.
type GroupedStatsRequest struct {
    GroupBy      []string             `json:"groupBy"`
    Condition    json.RawMessage      `json:"condition,omitempty"`
    Aggregations []AggregationExprWire `json:"aggregations,omitempty"`
    PointInTime  *time.Time           `json:"pointInTime,omitempty"`
    Limit        *int                 `json:"limit,omitempty"`
}

// AggregationExprWire is one requested aggregation in the request body.
type AggregationExprWire struct {
    Op    string `json:"op"`
    Field string `json:"field"`
    As    string `json:"as,omitempty"`
}

// GroupedStatsBucket is one row of the response array.
type GroupedStatsBucket struct {
    GroupKey     []GroupKeyEntryWire `json:"groupKey"`
    Count        int64               `json:"count"`
    Aggregations map[string]any      `json:"aggregations,omitempty"`
}

// GroupKeyEntryWire is one (path, value) pair in a bucket's key.
type GroupKeyEntryWire struct {
    Path  string `json:"path"`
    Value any    `json:"value"`
}
```

Add the missing import for `encoding/json` at the top.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/domain/entity/... -run TestGroupedStats -v`
Expected: PASS.

- [ ] **Step 5: Vet**

Run: `go vet ./internal/domain/entity/...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/entity/grouped_stats_types.go internal/domain/entity/grouped_stats_types_test.go
git commit -m "feat(entity): add grouped-stats DTOs (refs #299)"
```

---

### Task 6: Request validation

**Files:**
- Create: `internal/domain/entity/grouped_stats_validation.go`
- Create: `internal/domain/entity/grouped_stats_validation_test.go`

- [ ] **Step 1: Write the failing tests** (table-driven; covers spec §3 error table)

```go
// internal/domain/entity/grouped_stats_validation_test.go
package entity_test

import (
    "testing"
    "time"

    "github.com/cyoda-platform/cyoda-go/internal/domain/entity"
)

func TestValidateGroupedStatsRequest(t *testing.T) {
    intPtr := func(i int) *int { return &i }
    timePtr := func(s string) *time.Time {
        t, _ := time.Parse(time.RFC3339, s)
        return &t
    }
    cases := []struct {
        name    string
        in      entity.GroupedStatsRequest
        maxBuckets int
        wantCode string // "" = no error
    }{
        {"missing groupBy", entity.GroupedStatsRequest{}, 10000, "MISSING_GROUP_BY"},
        {"empty entry", entity.GroupedStatsRequest{GroupBy: []string{""}}, 10000, "INVALID_GROUP_BY_PATH"},
        {"array projection", entity.GroupedStatsRequest{GroupBy: []string{"$.items[*]"}}, 10000, "INVALID_GROUP_BY_PATH"},
        {"positional index", entity.GroupedStatsRequest{GroupBy: []string{"$.items[0]"}}, 10000, "INVALID_GROUP_BY_PATH"},
        {"bracket scalar accepted",
            entity.GroupedStatsRequest{GroupBy: []string{"$.['variantId']"}}, 10000, ""},
        {"duplicate groupBy",
            entity.GroupedStatsRequest{GroupBy: []string{"state", "state"}}, 10000, "DUPLICATE_GROUP_BY"},
        {"unknown agg op",
            entity.GroupedStatsRequest{
                GroupBy: []string{"state"},
                Aggregations: []entity.AggregationExprWire{
                    {Op: "median", Field: "$.x"},
                }}, 10000, "INVALID_AGGREGATION_OP"},
        {"agg field array projection",
            entity.GroupedStatsRequest{
                GroupBy: []string{"state"},
                Aggregations: []entity.AggregationExprWire{
                    {Op: "sum", Field: "$.x[*]"},
                }}, 10000, "INVALID_AGGREGATION_FIELD"},
        {"distinct pair colliding alias",
            entity.GroupedStatsRequest{
                GroupBy: []string{"state"},
                Aggregations: []entity.AggregationExprWire{
                    {Op: "sum", Field: "$.x", As: "v"},
                    {Op: "avg", Field: "$.y", As: "v"},
                }}, 10000, "DUPLICATE_AGGREGATION_ALIAS"},
        {"identical pair silently deduped",
            entity.GroupedStatsRequest{
                GroupBy: []string{"state"},
                Aggregations: []entity.AggregationExprWire{
                    {Op: "sum", Field: "$.x"},
                    {Op: "sum", Field: "$.x"},
                }}, 10000, ""},
        {"limit > ceiling",
            entity.GroupedStatsRequest{
                GroupBy: []string{"state"},
                Limit: intPtr(20000),
            }, 10000, "INVALID_LIMIT"},
        {"limit non-positive",
            entity.GroupedStatsRequest{
                GroupBy: []string{"state"},
                Limit: intPtr(0),
            }, 10000, "INVALID_LIMIT"},
        {"happy path", entity.GroupedStatsRequest{
            GroupBy: []string{"state", "$.variantId"},
            PointInTime: timePtr("2026-06-14T12:00:00Z"),
            Limit: intPtr(50),
        }, 10000, ""},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            _, err := entity.ValidateGroupedStatsRequest(tc.in, tc.maxBuckets)
            if tc.wantCode == "" {
                if err != nil {
                    t.Fatalf("unexpected error: %v", err)
                }
                return
            }
            if err == nil {
                t.Fatalf("expected error %s, got nil", tc.wantCode)
            }
            ve, ok := err.(*entity.GroupedStatsValidationError)
            if !ok {
                t.Fatalf("expected GroupedStatsValidationError, got %T: %v", err, err)
            }
            if ve.Code != tc.wantCode {
                t.Fatalf("got code %s, want %s", ve.Code, tc.wantCode)
            }
        })
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/entity/... -run TestValidateGroupedStatsRequest -v`
Expected: FAIL with "undefined: ValidateGroupedStatsRequest".

- [ ] **Step 3: Write minimal implementation**

```go
// internal/domain/entity/grouped_stats_validation.go
package entity

import (
    "fmt"
    "strings"
)

// GroupedStatsValidationError is returned by ValidateGroupedStatsRequest.
// Code is one of the 400-error codes documented in
// docs/superpowers/specs/2026-06-14-issue-299-grouped-stats-design.md §3.
type GroupedStatsValidationError struct {
    Code    string
    Message string
}

func (e *GroupedStatsValidationError) Error() string {
    return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ValidatedGroupedStatsRequest is the post-validation shape used by the
// service layer.
type ValidatedGroupedStatsRequest struct {
    GroupBy      []GroupExprValidated
    Aggregations []AggregationExprValidated
    // Condition is the raw bytes; the service layer parses via
    // predicate.ParseCondition.
    Condition    []byte
    PointInTime  *time.Time
    Limit        *int
}

// GroupExprValidated is the normalized groupBy entry.
type GroupExprValidated struct {
    IsState bool
    Path    string // populated when !IsState
}

// AggregationExprValidated is the normalized aggregation entry.
type AggregationExprValidated struct {
    Op    AggregateOp
    Field string
    Alias string
}

// AggregateOp duplicates spi.AggregateOp; the service layer translates.
type AggregateOp string

const (
    AggSum   AggregateOp = "sum"
    AggAvg   AggregateOp = "avg"
    AggMin   AggregateOp = "min"
    AggMax   AggregateOp = "max"
    AggStdev AggregateOp = "stdev"
)

// ValidateGroupedStatsRequest applies the validation rules from spec §3.
// maxBuckets is CYODA_STATS_GROUP_MAX (the cardinality ceiling); used to
// enforce `limit <= max`.
func ValidateGroupedStatsRequest(r GroupedStatsRequest, maxBuckets int) (*ValidatedGroupedStatsRequest, error) {
    if len(r.GroupBy) == 0 {
        return nil, &GroupedStatsValidationError{Code: "MISSING_GROUP_BY", Message: "groupBy is required"}
    }
    seen := make(map[string]struct{}, len(r.GroupBy))
    groups := make([]GroupExprValidated, 0, len(r.GroupBy))
    for _, raw := range r.GroupBy {
        norm, err := normalizeScalarPath(raw)
        if err != nil {
            return nil, &GroupedStatsValidationError{Code: "INVALID_GROUP_BY_PATH", Message: err.Error()}
        }
        if _, dup := seen[norm]; dup {
            return nil, &GroupedStatsValidationError{Code: "DUPLICATE_GROUP_BY", Message: "duplicate groupBy entry: " + norm}
        }
        seen[norm] = struct{}{}
        if norm == "state" {
            groups = append(groups, GroupExprValidated{IsState: true})
        } else {
            groups = append(groups, GroupExprValidated{Path: norm})
        }
    }

    // Aggregations: dedupe identical (op, field); reject distinct-(op,field)
    // colliding on explicit alias.
    aggs := make([]AggregationExprValidated, 0, len(r.Aggregations))
    seenPair := make(map[[2]string]string, len(r.Aggregations)) // (op,field) -> alias
    aliasOwner := make(map[string][2]string, len(r.Aggregations))
    for _, a := range r.Aggregations {
        switch AggregateOp(a.Op) {
        case AggSum, AggAvg, AggMin, AggMax, AggStdev:
        default:
            return nil, &GroupedStatsValidationError{Code: "INVALID_AGGREGATION_OP", Message: "unknown op: " + a.Op}
        }
        field, err := normalizeScalarPath(a.Field)
        if err != nil || field == "" {
            return nil, &GroupedStatsValidationError{Code: "INVALID_AGGREGATION_FIELD", Message: a.Field}
        }
        pair := [2]string{a.Op, field}
        alias := a.As
        if alias == "" {
            alias = a.Op + "_" + field
        }
        if existingAlias, dup := seenPair[pair]; dup {
            // identical (op, field) — silently dedupe (keep first), as long
            // as the alias matches an already-recorded one or wasn't given.
            _ = existingAlias
            continue
        }
        if owner, taken := aliasOwner[alias]; taken && owner != pair {
            return nil, &GroupedStatsValidationError{Code: "DUPLICATE_AGGREGATION_ALIAS", Message: alias}
        }
        seenPair[pair] = alias
        aliasOwner[alias] = pair
        aggs = append(aggs, AggregationExprValidated{
            Op:    AggregateOp(a.Op),
            Field: field,
            Alias: alias,
        })
    }

    if r.Limit != nil {
        if *r.Limit <= 0 || *r.Limit > maxBuckets {
            return nil, &GroupedStatsValidationError{
                Code:    "INVALID_LIMIT",
                Message: fmt.Sprintf("limit must be positive and <= %d", maxBuckets),
            }
        }
    }

    return &ValidatedGroupedStatsRequest{
        GroupBy:      groups,
        Aggregations: aggs,
        Condition:    []byte(r.Condition),
        PointInTime:  r.PointInTime,
        Limit:        r.Limit,
    }, nil
}

// normalizeScalarPath canonicalizes a JSONPath. Accepts dotted notation
// and bracket-quoted property access. Rejects array projections.
//
// Returns the reserved token "state" unchanged when seen, so callers can
// branch on that.
func normalizeScalarPath(s string) (string, error) {
    if s == "" {
        return "", fmt.Errorf("path is empty")
    }
    if s == "state" {
        return s, nil
    }
    // Normalize $.['x'].['y'] to $.x.y so we can reject by simple `[` check.
    norm := s
    for {
        before := norm
        norm = strings.ReplaceAll(norm, "['", ".")
        norm = strings.ReplaceAll(norm, "']", "")
        norm = strings.ReplaceAll(norm, "..", ".")
        if norm == before {
            break
        }
    }
    if strings.ContainsAny(norm, "[]") {
        return "", fmt.Errorf("array projection not supported: %s", s)
    }
    return norm, nil
}
```

Imports: add `"time"`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/domain/entity/... -run TestValidateGroupedStatsRequest -v`
Expected: all sub-tests PASS.

- [ ] **Step 5: Vet**

Run: `go vet ./internal/domain/entity/...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/entity/grouped_stats_validation.go internal/domain/entity/grouped_stats_validation_test.go
git commit -m "feat(entity): grouped-stats request validation (refs #299)"
```

---

### Task 7: Accumulator package (Welford, group-key encoding, top-N)

**Files:**
- Create: `internal/domain/entity/grouped_stats_accumulator.go`
- Create: `internal/domain/entity/grouped_stats_accumulator_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// internal/domain/entity/grouped_stats_accumulator_test.go
package entity

import (
    "math"
    "testing"
)

func TestBuildGroupKey_DistinguishesNullFromEmpty(t *testing.T) {
    k1 := buildGroupKey([]any{nil})
    k2 := buildGroupKey([]any{""})
    if k1 == k2 {
        t.Fatalf("null and empty string should encode differently")
    }
}

func TestBuildGroupKey_CollisionAdversarial(t *testing.T) {
    // ["a|b","c"] vs ["a","b|c"] would collide under naive strings.Join.
    k1 := buildGroupKey([]any{"a|b", "c"})
    k2 := buildGroupKey([]any{"a", "b|c"})
    if k1 == k2 {
        t.Fatalf("collision on pipe-separated values")
    }
}

func TestAccumulator_WelfordStdev(t *testing.T) {
    a := newAccumulator(AggregationExprValidated{
        Op: AggStdev, Field: "x", Alias: "stdev_x",
    })
    samples := []float64{50.0, 50.5, 49.5, 50.2, 49.8}
    for _, x := range samples {
        a.observeFloat(x)
    }
    got, ok := a.result()
    if !ok {
        t.Fatalf("expected ok result")
    }
    // hand-computed sample stdev ≈ 0.3937
    want := 0.39370039370059
    if math.Abs(got-want)/want > 1e-9 {
        t.Fatalf("stdev got %.12f, want ~%.12f", got, want)
    }
}

func TestAccumulator_StdevNBelow2IsNil(t *testing.T) {
    a := newAccumulator(AggregationExprValidated{Op: AggStdev, Alias: "s"})
    if _, ok := a.result(); ok {
        t.Fatalf("n=0 should not produce a stdev result")
    }
    a.observeFloat(42.0)
    if _, ok := a.result(); ok {
        t.Fatalf("n=1 should not produce a stdev result")
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/domain/entity/... -run "TestBuildGroupKey|TestAccumulator" -v`
Expected: FAIL with undefined symbols.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/domain/entity/grouped_stats_accumulator.go
package entity

import (
    "encoding/binary"
    "math"
    "sort"
)

// buildGroupKey encodes a group-key tuple as a unique Go string suitable
// for use as a map key. Encoding per spec D18:
//   per entry: sentinel byte (0x00 = null, 0x01 = string)
//              + 8-byte big-endian length prefix (only when sentinel = 0x01)
//              + raw value bytes
func buildGroupKey(values []any) string {
    var size int
    for _, v := range values {
        if v == nil {
            size += 1
            continue
        }
        s, ok := v.(string)
        if !ok {
            // Non-scalar coerces to null per D4.
            size += 1
            continue
        }
        size += 1 + 8 + len(s)
    }
    buf := make([]byte, 0, size)
    var lenBuf [8]byte
    for _, v := range values {
        if v == nil {
            buf = append(buf, 0x00)
            continue
        }
        s, ok := v.(string)
        if !ok {
            buf = append(buf, 0x00)
            continue
        }
        buf = append(buf, 0x01)
        binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
        buf = append(buf, lenBuf[:]...)
        buf = append(buf, s...)
    }
    return string(buf)
}

// accumulator holds per-bucket per-aggregation state plus the bucket's
// count and group-key entries.
type accumulator struct {
    expr AggregationExprValidated
    n    int64
    sum  float64
    minV float64
    maxV float64
    mean float64
    m2   float64
    init bool // true once first sample observed
}

func newAccumulator(expr AggregationExprValidated) *accumulator {
    return &accumulator{expr: expr}
}

// observeFloat updates the accumulator with a numeric sample.
// Non-numeric values are skipped at the caller.
func (a *accumulator) observeFloat(x float64) {
    a.n++
    a.sum += x
    if !a.init {
        a.minV, a.maxV = x, x
        a.init = true
    } else {
        if x < a.minV {
            a.minV = x
        }
        if x > a.maxV {
            a.maxV = x
        }
    }
    delta := x - a.mean
    a.mean += delta / float64(a.n)
    delta2 := x - a.mean
    a.m2 += delta * delta2
}

// result returns the aggregation's resolved value. ok=false means the
// bucket had no numeric samples (response value should be nil).
func (a *accumulator) result() (float64, bool) {
    switch a.expr.Op {
    case AggSum:
        if a.n == 0 {
            return 0, false
        }
        return a.sum, true
    case AggAvg:
        if a.n == 0 {
            return 0, false
        }
        return a.sum / float64(a.n), true
    case AggMin:
        if a.n == 0 {
            return 0, false
        }
        return a.minV, true
    case AggMax:
        if a.n == 0 {
            return 0, false
        }
        return a.maxV, true
    case AggStdev:
        if a.n < 2 {
            return 0, false
        }
        return math.Sqrt(a.m2 / float64(a.n-1)), true
    }
    return 0, false
}

// bucketState is per-group-key state.
type bucketState struct {
    groupKey []GroupKeyEntryWire
    count    int64
    aggs     []*accumulator
}

func (b *bucketState) observe(numerics []float64) {
    b.count++
    for i, x := range numerics {
        if !math.IsNaN(x) && !math.IsInf(x, 0) {
            b.aggs[i].observeFloat(x)
        }
    }
}

// accumulators holds all buckets keyed by the encoded group key.
type accumulators struct {
    req     *ValidatedGroupedStatsRequest
    buckets map[string]*bucketState
    // index into the buckets map by insertion order for stable iteration
    // is not required because the response is sorted by D12 total order.
}

func newAccumulators(req *ValidatedGroupedStatsRequest) *accumulators {
    return &accumulators{
        req:     req,
        buckets: make(map[string]*bucketState),
    }
}

func (a *accumulators) has(k string) bool {
    _, ok := a.buckets[k]
    return ok
}

func (a *accumulators) len() int { return len(a.buckets) }

func (a *accumulators) observe(k string, groupKey []GroupKeyEntryWire, numerics []float64) {
    b, ok := a.buckets[k]
    if !ok {
        b = &bucketState{groupKey: groupKey, aggs: make([]*accumulator, len(a.req.Aggregations))}
        for i, expr := range a.req.Aggregations {
            b.aggs[i] = newAccumulator(expr)
        }
        a.buckets[k] = b
    }
    b.observe(numerics)
}

// materialize converts the bucket map to a sorted []GroupedStatsBucket
// applying the D12 total order and the request's Limit.
func (a *accumulators) materialize() []GroupedStatsBucket {
    out := make([]GroupedStatsBucket, 0, len(a.buckets))
    for _, b := range a.buckets {
        bucket := GroupedStatsBucket{
            GroupKey: b.groupKey,
            Count:    b.count,
        }
        if len(a.req.Aggregations) > 0 {
            bucket.Aggregations = make(map[string]any, len(a.req.Aggregations))
            for i, expr := range a.req.Aggregations {
                v, ok := b.aggs[i].result()
                if ok {
                    bucket.Aggregations[expr.Alias] = v
                } else {
                    bucket.Aggregations[expr.Alias] = nil
                }
            }
        }
        out = append(out, bucket)
    }
    sort.Slice(out, func(i, j int) bool {
        if out[i].Count != out[j].Count {
            return out[i].Count > out[j].Count
        }
        return compareGroupKey(out[i].GroupKey, out[j].GroupKey) < 0
    })
    if a.req.Limit != nil && *a.req.Limit < len(out) {
        out = out[:*a.req.Limit]
    }
    return out
}

// compareGroupKey applies the D12 total order: pairwise by entry index;
// null < any string; strings compared byte-wise.
func compareGroupKey(a, b []GroupKeyEntryWire) int {
    n := len(a)
    if len(b) < n {
        n = len(b)
    }
    for i := 0; i < n; i++ {
        ai, bi := a[i].Value, b[i].Value
        as, _ := ai.(string)
        bs, _ := bi.(string)
        switch {
        case ai == nil && bi == nil:
            continue
        case ai == nil:
            return -1
        case bi == nil:
            return 1
        default:
            if as < bs {
                return -1
            }
            if as > bs {
                return 1
            }
        }
    }
    return len(a) - len(b)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/domain/entity/... -run "TestBuildGroupKey|TestAccumulator" -v`
Expected: all PASS.

- [ ] **Step 5: Vet**

Run: `go vet ./internal/domain/entity/...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/entity/grouped_stats_accumulator.go internal/domain/entity/grouped_stats_accumulator_test.go
git commit -m "feat(entity): accumulator with Welford stdev + collision-free group-key (refs #299)"
```

---

### Task 8: Service-layer dispatch + `tallyStreaming`

**Files:**
- Create: `internal/domain/entity/grouped_stats_service.go`
- Create: `internal/domain/entity/grouped_stats_service_test.go`

- [ ] **Step 1: Write a failing service-level test** using a fake `spi.EntityStore` that satisfies `Iterable` only.

```go
// internal/domain/entity/grouped_stats_service_test.go
package entity_test

import (
    "context"
    "errors"
    "testing"

    spi "github.com/cyoda-platform/cyoda-go-spi"
    "github.com/cyoda-platform/cyoda-go/internal/domain/entity"
)

type fakeIterable struct {
    entities []*spi.Entity
}

func (f *fakeIterable) Iterate(ctx context.Context, _ spi.ModelRef, _ spi.Filter, _ spi.IterateOptions) (spi.Iterator, error) {
    return &fakeIter{rows: f.entities}, nil
}

type fakeIter struct {
    rows []*spi.Entity
    idx  int
    err  error
}

func (i *fakeIter) Next() bool {
    if i.err != nil || i.idx >= len(i.rows) {
        return false
    }
    i.idx++
    return true
}
func (i *fakeIter) Entity() *spi.Entity { return i.rows[i.idx-1] }
func (i *fakeIter) Err() error          { return i.err }
func (i *fakeIter) Close() error        { return nil }

func TestQueryGroupedStats_FallsBackToStreaming(t *testing.T) {
    rows := []*spi.Entity{
        {Meta: spi.EntityMeta{State: "available"}},
        {Meta: spi.EntityMeta{State: "available"}},
        {Meta: spi.EntityMeta{State: "allocated"}},
    }
    svc := entity.NewGroupedStatsService(10000)
    req := &entity.ValidatedGroupedStatsRequest{
        GroupBy: []entity.GroupExprValidated{{IsState: true}},
    }
    buckets, err := svc.QueryGroupedStats(context.Background(), &fakeIterable{entities: rows}, spi.ModelRef{}, req)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(buckets) != 2 {
        t.Fatalf("got %d buckets, want 2", len(buckets))
    }
}

func TestQueryGroupedStats_501WhenNoCapability(t *testing.T) {
    type noop struct{}
    svc := entity.NewGroupedStatsService(10000)
    _, err := svc.QueryGroupedStats(context.Background(), noop{}, spi.ModelRef{}, &entity.ValidatedGroupedStatsRequest{GroupBy: []entity.GroupExprValidated{{IsState: true}}})
    if !errors.Is(err, entity.ErrBackendNotSupported) {
        t.Fatalf("want ErrBackendNotSupported, got %v", err)
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/domain/entity/... -run TestQueryGroupedStats -v`
Expected: FAIL with undefined symbols.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/domain/entity/grouped_stats_service.go
package entity

import (
    "context"
    "errors"
    "math"

    spi "github.com/cyoda-platform/cyoda-go-spi"
    "github.com/cyoda-platform/cyoda-go/internal/domain/search"
    "github.com/cyoda-platform/cyoda-go/internal/match"
    "github.com/tidwall/gjson"
)

// ErrBackendNotSupported maps to 501 NOT_IMPLEMENTED_BY_BACKEND.
var ErrBackendNotSupported = errors.New("backend supports neither Iterable nor GroupedAggregator")

// GroupedStatsService implements the dispatch logic from spec §4.
type GroupedStatsService struct {
    maxBuckets int
}

func NewGroupedStatsService(maxBuckets int) *GroupedStatsService {
    return &GroupedStatsService{maxBuckets: maxBuckets}
}

// QueryGroupedStats dispatches a validated grouped-stats request against
// any storage backend. The store parameter is intentionally `any` to
// reflect that the SPI capabilities are tested via type assertion.
func (s *GroupedStatsService) QueryGroupedStats(
    ctx context.Context,
    store any,
    model spi.ModelRef,
    req *ValidatedGroupedStatsRequest,
) ([]GroupedStatsBucket, error) {
    // Tx-aware: pushdown skipped inside an active transaction (D11).
    inTx := spi.GetTransaction(ctx) != nil

    // 1. Native pushdown.
    if ga, ok := store.(spi.GroupedAggregator); ok && !inTx {
        flt, terr := search.ConditionToFilter(req.Condition)
        if terr == nil {
            spiGroups := translateGroupBy(req.GroupBy)
            spiAggs := translateAggregations(req.Aggregations)
            out, err := ga.GroupedAggregate(ctx, model, spiGroups, flt, spi.GroupedAggregationsOptions{
                PointInTime:  req.PointInTime,
                MaxBuckets:   s.maxBuckets,
                Aggregations: spiAggs,
            })
            if err == nil {
                return postProcess(out, req), nil
            }
            if !errors.Is(err, spi.ErrAggregationNotPushdownable) {
                return nil, err
            }
            // fall through to streaming
        }
    }

    // 2. Streaming fallback.
    if it, ok := store.(spi.Iterable); ok {
        flt, terr := search.ConditionToFilter(req.Condition)
        filterOK := len(req.Condition) == 0 || terr == nil
        if !filterOK {
            flt = spi.Filter{}
        }
        return s.tallyStreaming(ctx, it, model, req, flt, filterOK)
    }

    return nil, ErrBackendNotSupported
}

func (s *GroupedStatsService) tallyStreaming(
    ctx context.Context,
    it spi.Iterable,
    model spi.ModelRef,
    req *ValidatedGroupedStatsRequest,
    flt spi.Filter,
    filterOK bool,
) ([]GroupedStatsBucket, error) {
    iter, err := it.Iterate(ctx, model, flt, spi.IterateOptions{PointInTime: req.PointInTime})
    if err != nil {
        return nil, err
    }
    defer iter.Close()

    acc := newAccumulators(req)
    for iter.Next() {
        e := iter.Entity()
        if !filterOK && len(req.Condition) > 0 {
            ok, mErr := match.MatchCondition(req.Condition, e.Data, e.Meta)
            if mErr != nil {
                return nil, mErr
            }
            if !ok {
                continue
            }
        }
        keyValues, groupKey := buildGroupKeyFromEntity(req.GroupBy, e)
        k := buildGroupKey(keyValues)
        if !acc.has(k) && acc.len() >= s.maxBuckets {
            return nil, spi.ErrGroupCardinalityExceeded
        }
        numerics := extractNumerics(req.Aggregations, e.Data)
        acc.observe(k, groupKey, numerics)
    }
    if err := iter.Err(); err != nil {
        return nil, err
    }
    return acc.materialize(), nil
}

// buildGroupKeyFromEntity extracts the per-entry values for both the map
// key (raw any slice) and the response groupKey ([]GroupKeyEntryWire).
func buildGroupKeyFromEntity(groups []GroupExprValidated, e *spi.Entity) ([]any, []GroupKeyEntryWire) {
    rawVals := make([]any, len(groups))
    keys := make([]GroupKeyEntryWire, len(groups))
    for i, g := range groups {
        var path string
        var val any
        if g.IsState {
            path = "state"
            if e.Meta.State != "" {
                val = e.Meta.State
            }
        } else {
            path = g.Path
            res := gjson.GetBytes(e.Data, gjsonPath(g.Path))
            switch {
            case !res.Exists():
                val = nil
            case res.Type == gjson.String:
                val = res.String()
            case res.Type == gjson.Number:
                // Convert number to its canonical string form for grouping.
                val = res.Raw
            case res.Type == gjson.True || res.Type == gjson.False:
                val = res.Raw
            default:
                // Object/Array: D4 says coerce to null.
                val = nil
            }
        }
        rawVals[i] = val
        keys[i] = GroupKeyEntryWire{Path: path, Value: val}
    }
    return rawVals, keys
}

// extractNumerics returns a float64 per aggregation; NaN means "skip
// (non-numeric or missing)".
func extractNumerics(aggs []AggregationExprValidated, data []byte) []float64 {
    out := make([]float64, len(aggs))
    for i, a := range aggs {
        res := gjson.GetBytes(data, gjsonPath(a.Field))
        if !res.Exists() || res.Type != gjson.Number {
            out[i] = math.NaN()
            continue
        }
        out[i] = res.Float()
    }
    return out
}

// gjsonPath converts our JSONPath ("$.foo.bar") to gjson syntax
// ("foo.bar"). For "state" callers handle separately.
func gjsonPath(p string) string {
    if len(p) >= 2 && p[0] == '$' && p[1] == '.' {
        return p[2:]
    }
    return p
}

func translateGroupBy(groups []GroupExprValidated) []spi.GroupExpr {
    out := make([]spi.GroupExpr, len(groups))
    for i, g := range groups {
        if g.IsState {
            out[i] = spi.GroupExpr{Kind: spi.GroupExprState}
        } else {
            out[i] = spi.GroupExpr{Kind: spi.GroupExprDataPath, Path: g.Path}
        }
    }
    return out
}

func translateAggregations(aggs []AggregationExprValidated) []spi.AggregateExpr {
    out := make([]spi.AggregateExpr, len(aggs))
    for i, a := range aggs {
        out[i] = spi.AggregateExpr{
            Op:    spi.AggregateOp(a.Op),
            Field: a.Field,
            Alias: a.Alias,
        }
    }
    return out
}

// postProcess applies sort + limit to the buckets returned by a native
// GroupedAggregator. Mirrors the streaming path's materialize().
func postProcess(buckets []spi.GroupedAggregateBucket, req *ValidatedGroupedStatsRequest) []GroupedStatsBucket {
    out := make([]GroupedStatsBucket, 0, len(buckets))
    for _, b := range buckets {
        keys := make([]GroupKeyEntryWire, len(b.GroupKey))
        for i, k := range b.GroupKey {
            keys[i] = GroupKeyEntryWire{Path: k.Path, Value: k.Value}
        }
        bucket := GroupedStatsBucket{
            GroupKey: keys,
            Count:    b.Count,
        }
        if len(req.Aggregations) > 0 {
            bucket.Aggregations = make(map[string]any, len(req.Aggregations))
            for _, a := range req.Aggregations {
                if v, ok := b.Aggregations[a.Alias]; ok {
                    bucket.Aggregations[a.Alias] = v
                } else {
                    bucket.Aggregations[a.Alias] = nil
                }
            }
        }
        out = append(out, bucket)
    }
    sort.Slice(out, func(i, j int) bool {
        if out[i].Count != out[j].Count {
            return out[i].Count > out[j].Count
        }
        return compareGroupKey(out[i].GroupKey, out[j].GroupKey) < 0
    })
    if req.Limit != nil && *req.Limit < len(out) {
        out = out[:*req.Limit]
    }
    return out
}
```

Imports: `"sort"`.

Note `match.MatchCondition` is the existing function (the JSON-Condition entry point); verify the existing signature in `internal/match/match.go` and adjust if needed. If the existing API takes raw JSON bytes for the condition, the call is correct as written.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/domain/entity/... -run TestQueryGroupedStats -v`
Expected: PASS.

- [ ] **Step 5: Vet**

Run: `go vet ./internal/domain/entity/...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/entity/grouped_stats_service.go internal/domain/entity/grouped_stats_service_test.go
git commit -m "feat(entity): grouped-stats service-layer dispatch with streaming tally (refs #299)"
```

---

### Task 9: HTTP handler + config wiring

**Files:**
- Create: `internal/domain/entity/grouped_stats_handler.go`
- Create: `internal/domain/entity/grouped_stats_handler_test.go`
- Modify: `internal/config/config.go` — add `StatsGroupMax int` field; default 10000; read `CYODA_STATS_GROUP_MAX`.
- Modify: `internal/api/router.go` (or the file that registers entity routes; identify via `grep -rn "/api/entity/stats" internal/api`) — register `POST /api/entity/stats/{entityName}/{modelVersion}/query`.

- [ ] **Step 1: Locate the route-registration file**

Run: `grep -rn "/api/entity/stats" internal/api`
Note the file and line. Adapt the route registration to match the existing pattern.

- [ ] **Step 2: Write the failing handler test** (httptest-style)

```go
// internal/domain/entity/grouped_stats_handler_test.go
package entity_test

import (
    "bytes"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"

    "github.com/cyoda-platform/cyoda-go/internal/domain/entity"
)

func TestGroupedStatsHandler_Returns400OnMissingGroupBy(t *testing.T) {
    h := entity.NewGroupedStatsHandler(nil, 10000)
    body := bytes.NewBufferString(`{}`)
    req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
    rec := httptest.NewRecorder()
    h.ServeHTTP(rec, req)
    if rec.Code != http.StatusBadRequest {
        t.Fatalf("status %d, want 400", rec.Code)
    }
    var er struct {
        ErrorCode string `json:"errorCode"`
    }
    if err := json.NewDecoder(rec.Body).Decode(&er); err != nil {
        t.Fatalf("body decode: %v", err)
    }
    if er.ErrorCode != "MISSING_GROUP_BY" {
        t.Fatalf("errorCode=%s, want MISSING_GROUP_BY", er.ErrorCode)
    }
}

func TestGroupedStatsHandler_Returns413OnLargeBody(t *testing.T) {
    h := entity.NewGroupedStatsHandler(nil, 10000)
    body := bytes.NewBuffer(make([]byte, 11*1024*1024)) // 11 MiB > 10 MiB cap
    req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
    req.Header.Set("Content-Type", "application/json")
    rec := httptest.NewRecorder()
    h.ServeHTTP(rec, req)
    if rec.Code != http.StatusRequestEntityTooLarge {
        t.Fatalf("status %d, want 413", rec.Code)
    }
}

func TestGroupedStatsHandler_RejectsMalformedJSON(t *testing.T) {
    h := entity.NewGroupedStatsHandler(nil, 10000)
    body := strings.NewReader(`{not json}`)
    req := httptest.NewRequest(http.MethodPost, "/api/entity/stats/X/1/query", body)
    req.Header.Set("Content-Type", "application/json")
    rec := httptest.NewRecorder()
    h.ServeHTTP(rec, req)
    if rec.Code != http.StatusBadRequest {
        t.Fatalf("status %d, want 400", rec.Code)
    }
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/domain/entity/... -run TestGroupedStatsHandler -v`
Expected: FAIL with "undefined: entity.NewGroupedStatsHandler".

- [ ] **Step 4: Write minimal implementation**

```go
// internal/domain/entity/grouped_stats_handler.go
package entity

import (
    "encoding/json"
    "errors"
    "io"
    "net/http"

    spi "github.com/cyoda-platform/cyoda-go-spi"
)

const maxGroupedStatsBodySize = 10 * 1024 * 1024 // 10 MiB; matches /search/*.

// StoreResolver returns the EntityStore for the given model and tenant
// pulled from the request context. The router wires this from the
// existing entity service. Defined as a function-type so the handler is
// testable in isolation.
type StoreResolver func(r *http.Request, entityName, modelVersion string) (store any, model spi.ModelRef, ok bool, err error)

// GroupedStatsHandler is the HTTP handler for
// POST /api/entity/stats/{entityName}/{modelVersion}/query.
type GroupedStatsHandler struct {
    resolve    StoreResolver
    svc        *GroupedStatsService
    maxBuckets int
}

// NewGroupedStatsHandler constructs the handler. resolve may be nil in
// tests that only exercise validation paths.
func NewGroupedStatsHandler(resolve StoreResolver, maxBuckets int) *GroupedStatsHandler {
    return &GroupedStatsHandler{
        resolve:    resolve,
        svc:        NewGroupedStatsService(maxBuckets),
        maxBuckets: maxBuckets,
    }
}

func (h *GroupedStatsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // 413 — body too large.
    r.Body = http.MaxBytesReader(w, r.Body, maxGroupedStatsBodySize)
    raw, err := io.ReadAll(r.Body)
    if err != nil {
        var maxErr *http.MaxBytesError
        if errors.As(err, &maxErr) {
            writeError(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "request body exceeds 10 MiB")
            return
        }
        writeError(w, http.StatusBadRequest, "MALFORMED_REQUEST", "could not read body")
        return
    }

    // 400 — malformed JSON.
    var req GroupedStatsRequest
    dec := json.NewDecoder(bytes.NewReader(raw))
    dec.DisallowUnknownFields()
    if err := dec.Decode(&req); err != nil {
        writeError(w, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
        return
    }

    // 400 — validation.
    validated, err := ValidateGroupedStatsRequest(req, h.maxBuckets)
    if err != nil {
        if ve, ok := err.(*GroupedStatsValidationError); ok {
            writeError(w, http.StatusBadRequest, ve.Code, ve.Message)
            return
        }
        writeError(w, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
        return
    }

    // Resolve model + store.
    var store any
    var model spi.ModelRef
    if h.resolve != nil {
        entityName, modelVersion := extractPathParams(r)
        var ok bool
        store, model, ok, err = h.resolve(r, entityName, modelVersion)
        if err != nil {
            writeServerError(w, err)
            return
        }
        if !ok {
            writeError(w, http.StatusBadRequest, "UNKNOWN_MODEL", "model not found for tenant")
            return
        }
    }

    // Dispatch.
    buckets, err := h.svc.QueryGroupedStats(r.Context(), store, model, validated)
    if err != nil {
        switch {
        case errors.Is(err, ErrBackendNotSupported):
            writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED_BY_BACKEND", "backend does not support grouped stats")
        case errors.Is(err, spi.ErrGroupCardinalityExceeded):
            writeError(w, http.StatusUnprocessableEntity, "GROUP_CARDINALITY_EXCEEDED", "group cardinality exceeds CYODA_STATS_GROUP_MAX")
        default:
            writeServerError(w, err)
        }
        return
    }

    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusOK)
    _ = json.NewEncoder(w).Encode(buckets)
}

func extractPathParams(r *http.Request) (entityName, modelVersion string) {
    // Implementation specific to the project's router. Use whatever path-param
    // helper exists (e.g. chi.URLParam, mux.Vars). Adapt during integration.
    // Stub:
    return "", ""
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    _ = json.NewEncoder(w).Encode(map[string]string{
        "errorCode": code,
        "message":   msg,
    })
}

func writeServerError(w http.ResponseWriter, _ error) {
    // 500 with ticket UUID per project policy (see existing handlers
    // for the canonical helper; adapt during integration).
    writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
}
```

Imports: add `"bytes"`.

- [ ] **Step 5: Add `CYODA_STATS_GROUP_MAX` to config**

Inspect `internal/config/config.go` (or wherever `DefaultConfig()` lives — `grep -rn DefaultConfig internal/config`). Add:

```go
StatsGroupMax int `env:"CYODA_STATS_GROUP_MAX" envDefault:"10000"`
```

(Adapt syntax to the existing env-var pattern; if the codebase uses manual `os.Getenv` parsing in DefaultConfig, follow that pattern.)

- [ ] **Step 6: Register the route**

In `internal/api/router.go` (or the equivalent file from Step 1), register:

```go
mux.Handle("POST /api/entity/stats/{entityName}/{modelVersion}/query",
    entity.NewGroupedStatsHandler(resolveStore, cfg.StatsGroupMax))
```

Adapt to existing routing helper.

- [ ] **Step 7: Wire the StoreResolver**

In the file where existing entity handlers get their store reference, add a `resolveStore` function with the signature:

```go
func resolveStore(r *http.Request, entityName, modelVersion string) (any, spi.ModelRef, bool, error) {
    // ... existing model-lookup pattern; return the store as any.
}
```

- [ ] **Step 8: Run tests to verify they pass**

Run: `go test ./internal/domain/entity/... -run TestGroupedStatsHandler -v`
Expected: PASS.

- [ ] **Step 9: Run all unit tests**

Run: `go test -short ./...`
Expected: all pass.

- [ ] **Step 10: Commit**

```bash
git add internal/domain/entity/grouped_stats_handler.go internal/domain/entity/grouped_stats_handler_test.go internal/config/config.go internal/api/router.go
git commit -m "feat(entity): grouped-stats HTTP handler + config + route (refs #299)"
```

---

# Phase 3 — in-tree plugins

### Task 10: Memory plugin — `entityVersion` immutability godoc

**Files:**
- Modify: `plugins/memory/entity_store.go`

- [ ] **Step 1: Add the godoc invariant**

Find the `entityVersion` struct declaration. Add a doc comment:

```go
// entityVersion is one version of an entity in the per-entity history.
//
// Invariant: once an entityVersion is appended to the per-entity []entityVersion
// slice and the write lock is released, its fields are NEVER mutated. Iterators
// and snapshots may hold *entityVersion or *spi.Entity (via the .entity field)
// pointers and read them lock-free after releasing the read lock. This invariant
// is load-bearing for the snapshot-then-iterate pattern in grouped-stats #299.
//
// If you add a code path that mutates a published entityVersion, fix the
// invariant doc here AND audit the memory plugin's Iterable/GroupedAggregator
// implementations.
type entityVersion struct {
    // ... existing fields
}
```

Also add a one-line comment inside `saveUnlocked` near the version-append site:

```go
// invariant: appended versions are immutable post-publish; see entityVersion godoc.
```

- [ ] **Step 2: Run existing tests**

Run: `go test ./plugins/memory/...`
Expected: all pass (no behavior change).

- [ ] **Step 3: Commit**

```bash
git add plugins/memory/entity_store.go
git commit -m "docs(memory): document entityVersion immutability invariant (refs #299)"
```

---

### Task 11: Memory plugin — `Iterable` and `GroupedAggregator`

**Files:**
- Create: `plugins/memory/grouped_stats.go`
- Create: `plugins/memory/grouped_stats_test.go`

- [ ] **Step 1: Write failing tests** that exercise snapshot semantics + in-tx overlay.

```go
// plugins/memory/grouped_stats_test.go
package memory_test

import (
    "context"
    "encoding/json"
    "testing"

    spi "github.com/cyoda-platform/cyoda-go-spi"
    "github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// helper: get a fresh store+tenant+model for a test
func newStore(t *testing.T) (spi.EntityStore, spi.ModelRef) {
    t.Helper()
    f, err := memory.NewStoreFactory(context.Background())
    if err != nil {
        t.Fatal(err)
    }
    return f.GetStore(spi.TenantID("t1")), spi.ModelRef{EntityName: "Item", ModelVersion: "1"}
}

func TestMemoryIterate_BasicScan(t *testing.T) {
    s, model := newStore(t)
    ctx := context.Background()
    for i := 0; i < 3; i++ {
        data, _ := json.Marshal(map[string]any{"x": i})
        e := &spi.Entity{Meta: spi.EntityMeta{ModelRef: model, State: "available"}, Data: data}
        if _, err := s.Save(ctx, e); err != nil {
            t.Fatal(err)
        }
    }
    it := s.(spi.Iterable)
    iter, err := it.Iterate(ctx, model, spi.Filter{}, spi.IterateOptions{})
    if err != nil {
        t.Fatal(err)
    }
    defer iter.Close()
    var seen int
    for iter.Next() {
        seen++
    }
    if iter.Err() != nil {
        t.Fatalf("iter err: %v", iter.Err())
    }
    if seen != 3 {
        t.Fatalf("got %d, want 3", seen)
    }
}

func TestMemoryGroupedAggregate_CountByState(t *testing.T) {
    s, model := newStore(t)
    ctx := context.Background()
    for i := 0; i < 5; i++ {
        e := &spi.Entity{Meta: spi.EntityMeta{ModelRef: model, State: "available"}, Data: []byte("{}")}
        s.Save(ctx, e)
    }
    for i := 0; i < 2; i++ {
        e := &spi.Entity{Meta: spi.EntityMeta{ModelRef: model, State: "allocated"}, Data: []byte("{}")}
        s.Save(ctx, e)
    }
    ga := s.(spi.GroupedAggregator)
    res, err := ga.GroupedAggregate(ctx, model,
        []spi.GroupExpr{{Kind: spi.GroupExprState}},
        spi.Filter{},
        spi.GroupedAggregationsOptions{MaxBuckets: 100},
    )
    if err != nil {
        t.Fatal(err)
    }
    if len(res) != 2 {
        t.Fatalf("buckets=%d, want 2", len(res))
    }
    totals := map[string]int64{}
    for _, b := range res {
        totals[b.GroupKey[0].Value.(string)] = b.Count
    }
    if totals["available"] != 5 || totals["allocated"] != 2 {
        t.Fatalf("counts wrong: %v", totals)
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./plugins/memory/... -run "TestMemoryIterate|TestMemoryGroupedAggregate" -v`
Expected: FAIL on type assertion (no Iterable/GroupedAggregator yet).

- [ ] **Step 3: Write minimal implementation**

Inspect `plugins/memory/entity_store.go` to confirm the entity store type name (e.g. `*entityStore`) and the factory shape. Then create `plugins/memory/grouped_stats.go`:

```go
// plugins/memory/grouped_stats.go
package memory

import (
    "context"

    spi "github.com/cyoda-platform/cyoda-go-spi"
    "github.com/cyoda-platform/cyoda-go/internal/match"
    "github.com/tidwall/gjson"
)

// Iterate implements spi.Iterable. It snapshots matching version pointers
// under the read lock, then iterates lock-free; the iterator applies the
// filter inside Next() before yielding (D14, §6.1).
func (s *entityStore) Iterate(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
    snapshot := s.buildSnapshot(model, opts.PointInTime)
    return &memoryIter{
        snapshot: snapshot,
        filter:   filter,
        ctx:      ctx,
    }, nil
}

// buildSnapshot acquires the read lock, walks the tenant's entity map
// once, captures *spi.Entity pointers for matching versions, and releases
// the lock. In-tx callers (tx != nil) overlay tx.Buffer and exclude
// tx.Deletes per D11. Outside a tx, the simple non-tx snapshot is used.
func (s *entityStore) buildSnapshot(model spi.ModelRef, pit *time.Time) []*spi.Entity {
    // ... implementation follows the existing GetAll / getAllSnapshotUnlocked
    // pattern in entity_store.go. The in-tx branch lives in this same file
    // for visibility; it mirrors the existing tx-buffer overlay code.
}

type memoryIter struct {
    snapshot []*spi.Entity
    filter   spi.Filter
    ctx      context.Context
    idx      int
    cur      *spi.Entity
    err      error
    closed   bool
}

func (it *memoryIter) Next() bool {
    if it.err != nil || it.closed {
        return false
    }
    if err := it.ctx.Err(); err != nil {
        it.err = err
        return false
    }
    for it.idx < len(it.snapshot) {
        e := it.snapshot[it.idx]
        it.idx++
        if !match.MatchFilter(it.filter, e.Data, e.Meta) {
            continue
        }
        it.cur = e
        return true
    }
    return false
}

func (it *memoryIter) Entity() *spi.Entity { return it.cur }
func (it *memoryIter) Err() error          { return it.err }

func (it *memoryIter) Close() error {
    if it.closed {
        return nil
    }
    it.closed = true
    it.snapshot = nil
    it.cur = nil
    return nil
}

// GroupedAggregate implements spi.GroupedAggregator. Walks the same snapshot
// the iterator would walk, accumulates per bucket via Welford, returns
// ErrGroupCardinalityExceeded if the result exceeds opts.MaxBuckets.
func (s *entityStore) GroupedAggregate(
    ctx context.Context,
    model spi.ModelRef,
    groupBy []spi.GroupExpr,
    filter spi.Filter,
    opts spi.GroupedAggregationsOptions,
) ([]spi.GroupedAggregateBucket, error) {
    snapshot := s.buildSnapshot(model, opts.PointInTime)
    buckets := make(map[string]*memBucket)
    for _, e := range snapshot {
        if err := ctx.Err(); err != nil {
            return nil, err
        }
        if !match.MatchFilter(filter, e.Data, e.Meta) {
            continue
        }
        rawVals, keys := extractGroupKey(groupBy, e)
        k := encodeGroupKey(rawVals)
        b, ok := buckets[k]
        if !ok {
            if len(buckets) >= opts.MaxBuckets {
                return nil, spi.ErrGroupCardinalityExceeded
            }
            b = &memBucket{groupKey: keys, aggs: make([]*memAcc, len(opts.Aggregations))}
            for i, a := range opts.Aggregations {
                b.aggs[i] = &memAcc{op: a.Op, alias: a.Alias, field: a.Field}
            }
            buckets[k] = b
        }
        b.observe(e.Data)
    }
    out := make([]spi.GroupedAggregateBucket, 0, len(buckets))
    for _, b := range buckets {
        out = append(out, b.toBucket(opts.Aggregations))
    }
    return out, nil
}

// memBucket / memAcc are intentionally separate from the service-layer
// accumulators package — the SPI returns plugin-level buckets, the service
// translates them. Keeps the plugin self-contained.
type memBucket struct {
    groupKey []spi.GroupKeyEntry
    count    int64
    aggs     []*memAcc
}

type memAcc struct {
    op    spi.AggregateOp
    alias string
    field string
    n     int64
    sum   float64
    minV  float64
    maxV  float64
    mean  float64
    m2    float64
    init  bool
}

func (b *memBucket) observe(data []byte) {
    b.count++
    for _, a := range b.aggs {
        res := gjson.GetBytes(data, gjsonPath(a.field))
        if !res.Exists() || res.Type != gjson.Number {
            continue
        }
        a.observeFloat(res.Float())
    }
}

func (a *memAcc) observeFloat(x float64) {
    a.n++
    a.sum += x
    if !a.init {
        a.minV, a.maxV = x, x
        a.init = true
    } else {
        if x < a.minV { a.minV = x }
        if x > a.maxV { a.maxV = x }
    }
    delta := x - a.mean
    a.mean += delta / float64(a.n)
    delta2 := x - a.mean
    a.m2 += delta * delta2
}

func (b *memBucket) toBucket(aggs []spi.AggregateExpr) spi.GroupedAggregateBucket {
    var aggMap map[string]any
    if len(aggs) > 0 {
        aggMap = make(map[string]any, len(aggs))
        for i, a := range aggs {
            aggMap[a.Alias] = b.aggs[i].result()
        }
    }
    return spi.GroupedAggregateBucket{
        GroupKey:     b.groupKey,
        Count:        b.count,
        Aggregations: aggMap,
    }
}

func (a *memAcc) result() any {
    switch a.op {
    case spi.AggSum:
        if a.n == 0 { return nil }
        return a.sum
    case spi.AggAvg:
        if a.n == 0 { return nil }
        return a.sum / float64(a.n)
    case spi.AggMin:
        if a.n == 0 { return nil }
        return a.minV
    case spi.AggMax:
        if a.n == 0 { return nil }
        return a.maxV
    case spi.AggStdev:
        if a.n < 2 { return nil }
        return math.Sqrt(a.m2 / float64(a.n-1))
    }
    return nil
}

// extractGroupKey / encodeGroupKey / gjsonPath are plugin-local copies
// to avoid coupling to internal/domain/entity. They follow the same
// rules as the service-layer versions.
//
// (Body identical to the service-layer helpers in Task 8; copy and adapt
// for spi.GroupKeyEntry instead of entity.GroupKeyEntryWire.)
```

Imports: `"time"`, `"math"`.

> Implementation note for the subagent: the helpers (`extractGroupKey`, `encodeGroupKey`, `gjsonPath`) duplicate the service-layer versions for plugin-locality. Yes, this is mild duplication; it keeps the SPI clean. Spec §6.1 explicitly carries this trade.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./plugins/memory/... -run "TestMemoryIterate|TestMemoryGroupedAggregate" -v`
Expected: PASS.

- [ ] **Step 5: Run full plugin tests**

Run: `go test ./plugins/memory/...`
Expected: all pass.

- [ ] **Step 6: Vet**

Run: `go vet ./plugins/memory/...`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add plugins/memory/grouped_stats.go plugins/memory/grouped_stats_test.go
git commit -m "feat(memory): implement Iterable + GroupedAggregator for grouped-stats (refs #299)"
```

---

### Task 12: Sqlite plugin — `Iterable` and `GroupedAggregator`

**Files:**
- Create: `plugins/sqlite/grouped_stats.go`
- Create: `plugins/sqlite/grouped_stats_test.go`

- [ ] **Step 1: Write failing tests** (exercise stdev opt-out, count/sum/avg pushdown, residual-filter fall-through, in-tx routing through iterator).

```go
// plugins/sqlite/grouped_stats_test.go
package sqlite_test

import (
    "context"
    "errors"
    "testing"

    spi "github.com/cyoda-platform/cyoda-go-spi"
    "github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// helpers: newTestFactory creates an in-memory sqlite store factory.
// Use the existing test helper from plugins/sqlite/*_test.go.

func TestSqliteIterate_StreamsAllEntitiesForModel(t *testing.T) { /* … */ }

func TestSqliteGroupedAggregate_PushesCountByState(t *testing.T) {
    f, _ := sqlite.NewStoreFactoryForTest(context.Background(), t.TempDir()+"/db.sqlite")
    s := f.GetStore(spi.TenantID("t1"))
    model := spi.ModelRef{EntityName: "Item", ModelVersion: "1"}
    // ... save 5 available + 2 allocated entities ...
    res, err := s.(spi.GroupedAggregator).GroupedAggregate(
        context.Background(), model,
        []spi.GroupExpr{{Kind: spi.GroupExprState}},
        spi.Filter{},
        spi.GroupedAggregationsOptions{MaxBuckets: 100},
    )
    if err != nil {
        t.Fatal(err)
    }
    if len(res) != 2 {
        t.Fatalf("buckets=%d", len(res))
    }
}

func TestSqliteGroupedAggregate_DeclinesStdev(t *testing.T) {
    f, _ := sqlite.NewStoreFactoryForTest(context.Background(), t.TempDir()+"/db.sqlite")
    s := f.GetStore(spi.TenantID("t1"))
    model := spi.ModelRef{EntityName: "Item", ModelVersion: "1"}
    _, err := s.(spi.GroupedAggregator).GroupedAggregate(
        context.Background(), model,
        []spi.GroupExpr{{Kind: spi.GroupExprState}},
        spi.Filter{},
        spi.GroupedAggregationsOptions{
            MaxBuckets:   100,
            Aggregations: []spi.AggregateExpr{{Op: spi.AggStdev, Field: "$.cost", Alias: "s"}},
        },
    )
    if !errors.Is(err, spi.ErrAggregationNotPushdownable) {
        t.Fatalf("want ErrAggregationNotPushdownable, got %v", err)
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./plugins/sqlite/... -run "TestSqliteIterate|TestSqliteGroupedAggregate" -v`
Expected: FAIL.

- [ ] **Step 3: Write minimal implementation**

```go
// plugins/sqlite/grouped_stats.go
package sqlite

import (
    "context"
    "database/sql"
    "fmt"
    "strings"

    spi "github.com/cyoda-platform/cyoda-go-spi"
    "github.com/cyoda-platform/cyoda-go/internal/match"
)

// Iterate implements spi.Iterable. It reuses planQuery to push the
// filter to WHERE and applies any residual inside Next() (spec §6.2).
func (s *entityStore) Iterate(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
    plan := planQuery(filter)
    var query string
    var args []any
    if opts.PointInTime != nil {
        query, args = s.buildPointInTimeIterateSQL(model, plan, *opts.PointInTime)
    } else {
        query, args = s.buildCurrentIterateSQL(model, plan)
    }
    rows, err := s.db.QueryContext(ctx, query, args...)
    if err != nil {
        return nil, fmt.Errorf("iterate query: %w", err)
    }
    return &sqliteIter{
        rows:       rows,
        postFilter: plan.postFilter,
        ctx:        ctx,
    }, nil
}

type sqliteIter struct {
    rows       *sql.Rows
    postFilter *spi.Filter
    ctx        context.Context
    cur        *spi.Entity
    err        error
    closed     bool
}

func (it *sqliteIter) Next() bool {
    if it.err != nil || it.closed {
        return false
    }
    if err := it.ctx.Err(); err != nil {
        it.err = err
        return false
    }
    for it.rows.Next() {
        e, scanErr := scanEntityFromRow(it.rows)
        if scanErr != nil {
            it.err = scanErr
            return false
        }
        if it.postFilter != nil && !match.MatchFilter(*it.postFilter, e.Data, e.Meta) {
            continue
        }
        it.cur = e
        return true
    }
    if rerr := it.rows.Err(); rerr != nil {
        it.err = rerr
    }
    return false
}

func (it *sqliteIter) Entity() *spi.Entity { return it.cur }
func (it *sqliteIter) Err() error          { return it.err }
func (it *sqliteIter) Close() error {
    if it.closed {
        return nil
    }
    it.closed = true
    return it.rows.Close()
}

// GroupedAggregate implements spi.GroupedAggregator. Pushes
// count/sum/avg/min/max. Returns ErrAggregationNotPushdownable when:
//   - any aggregation is stdev (D9), OR
//   - the filter has a residual (planQuery.postFilter != nil), OR
//   - opts.PointInTime != nil (the pointInTime path goes via iterator+tally
//     for now; future PR can extend).
func (s *entityStore) GroupedAggregate(
    ctx context.Context,
    model spi.ModelRef,
    groupBy []spi.GroupExpr,
    filter spi.Filter,
    opts spi.GroupedAggregationsOptions,
) ([]spi.GroupedAggregateBucket, error) {
    for _, a := range opts.Aggregations {
        if a.Op == spi.AggStdev {
            return nil, spi.ErrAggregationNotPushdownable
        }
    }
    plan := planQuery(filter)
    if plan.postFilter != nil {
        return nil, spi.ErrAggregationNotPushdownable
    }
    if opts.PointInTime != nil {
        return nil, spi.ErrAggregationNotPushdownable
    }
    // Build the SELECT.
    cols := make([]string, 0, len(groupBy))
    for i, g := range groupBy {
        var expr string
        if g.Kind == spi.GroupExprState {
            expr = `json_extract(meta, '$.state')`
        } else {
            expr = fmt.Sprintf("json_extract(data, '$.%s')", g.Path)
        }
        cols = append(cols, fmt.Sprintf("%s AS gk_%d", expr, i))
    }
    selectExprs := append([]string{}, cols...)
    selectExprs = append(selectExprs, "COUNT(*) AS cnt")
    for _, a := range opts.Aggregations {
        valExpr := fmt.Sprintf("CAST(json_extract(data, '$.%s') AS REAL)", a.Field)
        switch a.Op {
        case spi.AggSum:
            selectExprs = append(selectExprs, fmt.Sprintf("SUM(%s) AS agg_%s", valExpr, sqlSafeAlias(a.Alias)))
        case spi.AggAvg:
            selectExprs = append(selectExprs, fmt.Sprintf("AVG(%s) AS agg_%s", valExpr, sqlSafeAlias(a.Alias)))
        case spi.AggMin:
            selectExprs = append(selectExprs, fmt.Sprintf("MIN(%s) AS agg_%s", valExpr, sqlSafeAlias(a.Alias)))
        case spi.AggMax:
            selectExprs = append(selectExprs, fmt.Sprintf("MAX(%s) AS agg_%s", valExpr, sqlSafeAlias(a.Alias)))
        }
    }
    groupByExprs := make([]string, 0, len(groupBy))
    for _, g := range groupBy {
        if g.Kind == spi.GroupExprState {
            groupByExprs = append(groupByExprs, `json_extract(meta, '$.state')`)
        } else {
            groupByExprs = append(groupByExprs, fmt.Sprintf("json_extract(data, '$.%s')", g.Path))
        }
    }
    args := []any{string(s.tenantID), model.EntityName, model.ModelVersion}
    where := "tenant_id = ? AND model_name = ? AND model_version = ? AND NOT deleted"
    if plan.whereClause != "" {
        where += " AND " + plan.whereClause
        args = append(args, plan.args...)
    }
    query := fmt.Sprintf(
        "SELECT %s FROM entities WHERE %s GROUP BY %s LIMIT %d",
        strings.Join(selectExprs, ", "),
        where,
        strings.Join(groupByExprs, ", "),
        opts.MaxBuckets+1,
    )
    rows, err := s.db.QueryContext(ctx, query, args...)
    if err != nil {
        return nil, fmt.Errorf("grouped-aggregate query: %w", err)
    }
    defer rows.Close()
    return scanGroupedAggregate(rows, groupBy, opts)
}

// scanGroupedAggregate decodes the result rows. If the row count exceeds
// opts.MaxBuckets, returns ErrGroupCardinalityExceeded.
func scanGroupedAggregate(rows *sql.Rows, groupBy []spi.GroupExpr, opts spi.GroupedAggregationsOptions) ([]spi.GroupedAggregateBucket, error) {
    // Determine column count: len(groupBy) + 1 (cnt) + len(aggs).
    nGroups := len(groupBy)
    nAggs := len(opts.Aggregations)
    out := make([]spi.GroupedAggregateBucket, 0, opts.MaxBuckets)
    for rows.Next() {
        if len(out) >= opts.MaxBuckets {
            // We requested LIMIT MaxBuckets+1; if we get here, overflow.
            return nil, spi.ErrGroupCardinalityExceeded
        }
        vals := make([]any, nGroups+1+nAggs)
        scanDst := make([]any, len(vals))
        for i := range vals {
            scanDst[i] = &vals[i]
        }
        if err := rows.Scan(scanDst...); err != nil {
            return nil, err
        }
        groupKey := make([]spi.GroupKeyEntry, nGroups)
        for i, g := range groupBy {
            var path string
            if g.Kind == spi.GroupExprState {
                path = "state"
            } else {
                path = g.Path
            }
            groupKey[i] = spi.GroupKeyEntry{Path: path, Value: normalizeSQLValue(vals[i])}
        }
        var count int64
        if c, ok := vals[nGroups].(int64); ok {
            count = c
        }
        var aggMap map[string]any
        if nAggs > 0 {
            aggMap = make(map[string]any, nAggs)
            for i, a := range opts.Aggregations {
                v := vals[nGroups+1+i]
                aggMap[a.Alias] = normalizeSQLNumeric(v)
            }
        }
        out = append(out, spi.GroupedAggregateBucket{
            GroupKey:     groupKey,
            Count:        count,
            Aggregations: aggMap,
        })
    }
    return out, rows.Err()
}

func normalizeSQLValue(v any) any {
    if v == nil {
        return nil
    }
    switch s := v.(type) {
    case string:
        return s
    case []byte:
        return string(s)
    }
    // Number or bool extracted via json_extract; fall back to fmt.
    return fmt.Sprintf("%v", v)
}

func normalizeSQLNumeric(v any) any {
    if v == nil {
        return nil
    }
    switch n := v.(type) {
    case float64:
        return n
    case int64:
        return float64(n)
    case []byte:
        // SUM/AVG return TEXT in some sqlite configurations; parse.
        // ... simple parse ...
    }
    return nil
}

func sqlSafeAlias(a string) string {
    // Aliases come from validated input; bracket-quote to be defensive.
    return "\"" + strings.ReplaceAll(a, "\"", "\"\"") + "\""
}

// buildCurrentIterateSQL / buildPointInTimeIterateSQL produce the iterate
// SQL for non-tx and snapshot reads respectively. They mirror the existing
// GetAll / getSnapshot SQL in entity_store.go; copy the schema details
// from there.
func (s *entityStore) buildCurrentIterateSQL(model spi.ModelRef, plan sqlPlan) (string, []any) {
    args := []any{string(s.tenantID), model.EntityName, model.ModelVersion}
    where := "tenant_id = ? AND model_name = ? AND model_version = ? AND NOT deleted"
    if plan.whereClause != "" {
        where += " AND " + plan.whereClause
        args = append(args, plan.args...)
    }
    return "SELECT json(data), json(meta) FROM entities WHERE " + where, args
}

func (s *entityStore) buildPointInTimeIterateSQL(model spi.ModelRef, plan sqlPlan, t time.Time) (string, []any) {
    // ... follow the existing `getSnapshot` SQL pattern from entity_store.go ...
}
```

Imports: `"time"`.

> Implementation note for the subagent: `scanEntityFromRow` is the existing helper in the sqlite plugin; use it. If its signature requires different column shape, adjust the iterate SQL to match.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./plugins/sqlite/... -run "TestSqliteIterate|TestSqliteGroupedAggregate" -v`
Expected: PASS.

- [ ] **Step 5: Run full plugin tests**

Run: `go test ./plugins/sqlite/...`
Expected: all pass.

- [ ] **Step 6: Vet**

Run: `go vet ./plugins/sqlite/...`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add plugins/sqlite/grouped_stats.go plugins/sqlite/grouped_stats_test.go
git commit -m "feat(sqlite): implement Iterable + GroupedAggregator with stdev opt-out (refs #299)"
```

---

### Task 13: Postgres migration — `cyoda_try_float8` + `entities_state_idx`

**Files:**
- Create: `plugins/postgres/migrations/00000N_grouped_stats.up.sql` (replace `N` with the next migration number found in the directory).
- Create: `plugins/postgres/migrations/00000N_grouped_stats.down.sql`.

- [ ] **Step 1: Determine next migration number**

Run: `ls plugins/postgres/migrations/ | sort`
Identify the last applied migration number; the new one is N+1.

- [ ] **Step 2: Write the up migration**

```sql
-- 00000N_grouped_stats.up.sql

-- D21: SQL-language try-cast for float8. No PL/pgSQL EXCEPTION block —
-- subtransaction cost is unacceptable on large scans.
CREATE OR REPLACE FUNCTION cyoda_try_float8(t text) RETURNS float8 AS $$
  SELECT NULLIF(
    CASE WHEN t ~ '\A-?[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]+)?\Z'
         THEN t::float8
         ELSE NULL END,
    'Infinity'::float8
  );
$$ LANGUAGE sql IMMUTABLE PARALLEL SAFE;

-- D19: Canonical state expression index. Partial on NOT deleted to keep
-- the index small and exclude tombstones.
CREATE INDEX IF NOT EXISTS entities_state_idx
ON entities (tenant_id, model_name, model_version, (doc->'_meta'->>'state'))
WHERE NOT deleted;
```

- [ ] **Step 3: Write the down migration**

```sql
-- 00000N_grouped_stats.down.sql
DROP INDEX IF EXISTS entities_state_idx;
DROP FUNCTION IF EXISTS cyoda_try_float8(text);
```

- [ ] **Step 4: Verify migration loader picks them up**

Inspect `plugins/postgres/Migrate` (or wherever migrations are read; commonly via `embed.FS`). If the loader uses `go:embed migrations/*.sql`, no code change is needed. Otherwise, add a line.

- [ ] **Step 5: Write a unit test for `cyoda_try_float8` behavior**

```go
// plugins/postgres/cyoda_try_float8_test.go
package postgres_test

import (
    "context"
    "testing"

    "github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

func TestCyodaTryFloat8(t *testing.T) {
    pool, cleanup := newPostgresTestPool(t) // existing helper
    defer cleanup()
    postgres.Migrate(pool) // run all migrations

    cases := []struct {
        in       string
        wantNull bool
        wantVal  float64
    }{
        {"", true, 0},
        {"abc", true, 0},
        {"NaN", true, 0},
        {"Infinity", true, 0},
        {"-Infinity", true, 0},
        {"42", false, 42},
        {"-1.5", false, -1.5},
        {"1e10", false, 1e10},
        {"1e500", true, 0}, // overflow → Infinity → NULLIF → NULL
        {"42\n", true, 0},  // trailing newline rejected by \Z anchor
    }
    for _, tc := range cases {
        t.Run(tc.in, func(t *testing.T) {
            var got *float64
            err := pool.QueryRow(context.Background(),
                "SELECT cyoda_try_float8($1)", tc.in).Scan(&got)
            if err != nil {
                t.Fatalf("query: %v", err)
            }
            if tc.wantNull {
                if got != nil {
                    t.Fatalf("got %v, want NULL", *got)
                }
            } else {
                if got == nil || *got != tc.wantVal {
                    t.Fatalf("got %v, want %v", got, tc.wantVal)
                }
            }
        })
    }
}
```

- [ ] **Step 6: Run the test**

Run: `go test ./plugins/postgres/... -run TestCyodaTryFloat8 -v`
Expected: PASS (requires Docker for testcontainers).

- [ ] **Step 7: Commit**

```bash
git add plugins/postgres/migrations/00000N_grouped_stats.up.sql plugins/postgres/migrations/00000N_grouped_stats.down.sql plugins/postgres/cyoda_try_float8_test.go
git commit -m "feat(postgres): cyoda_try_float8 helper + entities_state_idx migration (refs #299)"
```

---

### Task 14: Postgres Filter→SQL translator (`plugins/postgres/query_planner.go`)

**Files:**
- Create: `plugins/postgres/query_planner.go`
- Create: `plugins/postgres/query_planner_test.go`

- [ ] **Step 1: Write the failing tests** (mirror sqlite's `query_planner_test.go` for parity).

```go
// plugins/postgres/query_planner_test.go
package postgres

import (
    "testing"

    spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestPlanQuery_PushDataEquality(t *testing.T) {
    f := spi.Filter{
        Op:     spi.FilterEq,
        Path:   "city",
        Source: spi.SourceData,
        Value:  "Berlin",
    }
    plan := planQuery(f)
    wantWhere := "doc->>'city' = $1"
    if plan.whereClause != wantWhere {
        t.Fatalf("got %q, want %q", plan.whereClause, wantWhere)
    }
    if len(plan.args) != 1 || plan.args[0] != "Berlin" {
        t.Fatalf("args=%v", plan.args)
    }
    if plan.postFilter != nil {
        t.Fatalf("should be fully pushed")
    }
}

func TestPlanQuery_PushStateEquality(t *testing.T) {
    f := spi.Filter{
        Op:     spi.FilterEq,
        Source: spi.SourceLifecycleState,
        Value:  "available",
    }
    plan := planQuery(f)
    wantWhere := "doc->'_meta'->>'state' = $1"
    if plan.whereClause != wantWhere {
        t.Fatalf("got %q, want %q", plan.whereClause, wantWhere)
    }
}

func TestPlanQuery_GreedyAndPartial(t *testing.T) {
    f := spi.Filter{
        Op: spi.FilterAnd,
        Children: []spi.Filter{
            {Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
            // Hypothetical non-pushable op — adapt to the SPI's actual residual marker.
            {Op: spi.FilterOp(255)}, // garbage op acts as non-pushable
        },
    }
    plan := planQuery(f)
    if plan.postFilter == nil {
        t.Fatalf("expected residual")
    }
    if plan.whereClause == "" {
        t.Fatalf("expected the pushable child to be in WHERE")
    }
}

func TestPlanQuery_ConservativeOr(t *testing.T) {
    f := spi.Filter{
        Op: spi.FilterOr,
        Children: []spi.Filter{
            {Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"},
            {Op: spi.FilterOp(255)},
        },
    }
    plan := planQuery(f)
    // Whole OR goes to residual.
    if plan.whereClause != "" {
        t.Fatalf("expected no WHERE")
    }
    if plan.postFilter == nil {
        t.Fatalf("expected residual")
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./plugins/postgres/... -run TestPlanQuery -v`
Expected: FAIL.

- [ ] **Step 3: Write minimal implementation**

```go
// plugins/postgres/query_planner.go
package postgres

import (
    "fmt"
    "strconv"
    "strings"

    spi "github.com/cyoda-platform/cyoda-go-spi"
)

// sqlPlan is the output of planQuery — the pushable SQL fragment and the
// residual filter to apply post-scan. Mirrors plugins/sqlite/query_planner.go.
type sqlPlan struct {
    whereClause string
    args        []any
    postFilter  *spi.Filter
}

// planQuery splits a Filter into a pushable WHERE clause and an in-process
// residual. Greedy AND (extract pushable children, residual collects the
// rest); conservative OR (only push if every child is pushable, else whole
// OR is residual).
func planQuery(f spi.Filter) sqlPlan {
    pushed, residual := dissect(f)
    plan := sqlPlan{postFilter: residual}
    if pushed != nil {
        clause, args := toSQL(*pushed, &argCounter{})
        plan.whereClause = clause
        plan.args = args
    }
    return plan
}

type argCounter struct{ n int }

func (c *argCounter) next() string {
    c.n++
    return "$" + strconv.Itoa(c.n)
}

func dissect(f spi.Filter) (pushable *spi.Filter, residual *spi.Filter) {
    if isLeafPushable(f) {
        return &f, nil
    }
    switch f.Op {
    case spi.FilterAnd:
        var pushed []spi.Filter
        var resid []spi.Filter
        for _, c := range f.Children {
            p, r := dissect(c)
            if p != nil {
                pushed = append(pushed, *p)
            }
            if r != nil {
                resid = append(resid, *r)
            }
        }
        var pp, rp *spi.Filter
        if len(pushed) == 1 {
            pp = &pushed[0]
        } else if len(pushed) > 1 {
            pp = &spi.Filter{Op: spi.FilterAnd, Children: pushed}
        }
        if len(resid) == 1 {
            rp = &resid[0]
        } else if len(resid) > 1 {
            rp = &spi.Filter{Op: spi.FilterAnd, Children: resid}
        }
        return pp, rp
    case spi.FilterOr:
        // Conservative: only push if every child is pushable.
        var pushed []spi.Filter
        for _, c := range f.Children {
            p, r := dissect(c)
            if r != nil || p == nil {
                return nil, &f
            }
            pushed = append(pushed, *p)
        }
        pp := &spi.Filter{Op: spi.FilterOr, Children: pushed}
        return pp, nil
    case spi.FilterNot:
        if len(f.Children) != 1 {
            return nil, &f
        }
        p, r := dissect(f.Children[0])
        if r != nil || p == nil {
            return nil, &f
        }
        return &spi.Filter{Op: spi.FilterNot, Children: []spi.Filter{*p}}, nil
    }
    return nil, &f
}

func isLeafPushable(f spi.Filter) bool {
    switch f.Op {
    case spi.FilterEq, spi.FilterNe, spi.FilterGt, spi.FilterLt,
        spi.FilterGte, spi.FilterLte, spi.FilterContains,
        spi.FilterStartsWith, spi.FilterEndsWith, spi.FilterLike,
        spi.FilterIsNull, spi.FilterNotNull, spi.FilterBetween:
        return true
    }
    return false
}

// toSQL renders a pushed-down filter tree as a WHERE-clause fragment.
func toSQL(f spi.Filter, c *argCounter) (string, []any) {
    switch f.Op {
    case spi.FilterAnd:
        return joinChildren(f.Children, " AND ", c)
    case spi.FilterOr:
        clause, args := joinChildren(f.Children, " OR ", c)
        return "(" + clause + ")", args
    case spi.FilterNot:
        clause, args := toSQL(f.Children[0], c)
        return "NOT (" + clause + ")", args
    }
    // Leaf.
    pathExpr := jsonPathExpr(f.Source, f.Path)
    placeholder := c.next()
    switch f.Op {
    case spi.FilterEq:
        return fmt.Sprintf("%s = %s", pathExpr, placeholder), []any{f.Value}
    case spi.FilterNe:
        return fmt.Sprintf("%s <> %s", pathExpr, placeholder), []any{f.Value}
    case spi.FilterGt:
        return fmt.Sprintf("%s > %s", pathExpr, placeholder), []any{f.Value}
    case spi.FilterGte:
        return fmt.Sprintf("%s >= %s", pathExpr, placeholder), []any{f.Value}
    case spi.FilterLt:
        return fmt.Sprintf("%s < %s", pathExpr, placeholder), []any{f.Value}
    case spi.FilterLte:
        return fmt.Sprintf("%s <= %s", pathExpr, placeholder), []any{f.Value}
    case spi.FilterContains:
        return fmt.Sprintf("%s LIKE %s", pathExpr, placeholder), []any{"%" + asString(f.Value) + "%"}
    case spi.FilterStartsWith:
        return fmt.Sprintf("%s LIKE %s", pathExpr, placeholder), []any{asString(f.Value) + "%"}
    case spi.FilterEndsWith:
        return fmt.Sprintf("%s LIKE %s", pathExpr, placeholder), []any{"%" + asString(f.Value)}
    case spi.FilterLike:
        return fmt.Sprintf("%s LIKE %s", pathExpr, placeholder), []any{f.Value}
    case spi.FilterIsNull:
        c.n-- // didn't use the placeholder
        return fmt.Sprintf("%s IS NULL", pathExpr), nil
    case spi.FilterNotNull:
        c.n--
        return fmt.Sprintf("%s IS NOT NULL", pathExpr), nil
    case spi.FilterBetween:
        ph2 := c.next()
        if v, ok := f.Value.([2]any); ok {
            return fmt.Sprintf("%s BETWEEN %s AND %s", pathExpr, placeholder, ph2), []any{v[0], v[1]}
        }
        return fmt.Sprintf("FALSE"), nil
    }
    return "FALSE", nil
}

func joinChildren(cs []spi.Filter, sep string, c *argCounter) (string, []any) {
    parts := make([]string, len(cs))
    var args []any
    for i, child := range cs {
        clause, a := toSQL(child, c)
        parts[i] = clause
        args = append(args, a...)
    }
    return strings.Join(parts, sep), args
}

// jsonPathExpr builds the postgres JSONB extraction for a given source/path.
func jsonPathExpr(src spi.FilterSource, path string) string {
    if src == spi.SourceLifecycleState {
        return "doc->'_meta'->>'state'"
    }
    // SourceData: dotted path → nested -> / ->> chain.
    parts := strings.Split(path, ".")
    var sb strings.Builder
    sb.WriteString("doc")
    for i, p := range parts {
        if i == len(parts)-1 {
            sb.WriteString("->>'" + escapeIdent(p) + "'")
        } else {
            sb.WriteString("->'" + escapeIdent(p) + "'")
        }
    }
    return sb.String()
}

func escapeIdent(s string) string {
    return strings.ReplaceAll(s, "'", "''")
}

func asString(v any) string {
    if s, ok := v.(string); ok {
        return s
    }
    return fmt.Sprintf("%v", v)
}
```

> **SECURITY NOTE for the subagent:** the path validation invariant (no array projection, no SQL meta-characters) is enforced upstream by `internal/domain/entity/grouped_stats_validation.go` and the existing search path validator. The translator quotes path components but relies on upstream rejection of `'`, `;`, `--`. Do not rely on `escapeIdent` for security — it's a belt-and-braces measure. The parity tests in Task 17 explicitly cover SQL-injection inputs to confirm the upstream rejection.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./plugins/postgres/... -run TestPlanQuery -v`
Expected: PASS (requires no postgres connection — pure Go).

- [ ] **Step 5: Vet**

Run: `go vet ./plugins/postgres/...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add plugins/postgres/query_planner.go plugins/postgres/query_planner_test.go
git commit -m "feat(postgres): Filter→SQL translator for grouped-stats + future Searcher (refs #299)"
```

---

### Task 15: Postgres plugin — `Iterable` and `GroupedAggregator`

**Files:**
- Create: `plugins/postgres/grouped_stats.go`
- Create: `plugins/postgres/grouped_stats_test.go`

- [ ] **Step 1: Write failing tests** (parallel to sqlite Task 12 but exercises full Tier 1+2 push including stdev).

(Test code follows the same shape as sqlite tests; refer to Task 12 for the pattern and adapt to postgres testcontainer helpers found in `plugins/postgres/conformance_test.go`.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./plugins/postgres/... -run "TestPostgresIterate|TestPostgresGroupedAggregate" -v`
Expected: FAIL.

- [ ] **Step 3: Write minimal implementation**

```go
// plugins/postgres/grouped_stats.go
package postgres

import (
    "context"
    "fmt"
    "strings"

    "github.com/jackc/pgx/v5"

    spi "github.com/cyoda-platform/cyoda-go-spi"
    "github.com/cyoda-platform/cyoda-go/internal/match"
)

// Iterate implements spi.Iterable using pgx.Rows. Pushes the filter via
// query_planner; residual applied in Next().
func (s *entityStore) Iterate(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
    plan := planQuery(filter)
    var query string
    var args []any
    if opts.PointInTime != nil {
        query, args = s.buildPointInTimeIterateSQL(model, plan, *opts.PointInTime)
    } else {
        query, args = s.buildCurrentIterateSQL(model, plan)
    }
    rows, err := s.q.Query(ctx, query, args...)
    if err != nil {
        return nil, fmt.Errorf("iterate query: %w", err)
    }
    return &pgIter{
        rows:       rows,
        postFilter: plan.postFilter,
        ctx:        ctx,
    }, nil
}

type pgIter struct {
    rows       pgx.Rows
    postFilter *spi.Filter
    ctx        context.Context
    cur        *spi.Entity
    err        error
    closed     bool
}

func (it *pgIter) Next() bool {
    if it.err != nil || it.closed {
        return false
    }
    if err := it.ctx.Err(); err != nil {
        it.err = err
        return false
    }
    for it.rows.Next() {
        var docBytes []byte
        if err := it.rows.Scan(&docBytes); err != nil {
            it.err = err
            return false
        }
        e, err := parseEntityFromDoc(docBytes) // existing helper
        if err != nil {
            it.err = err
            return false
        }
        if it.postFilter != nil && !match.MatchFilter(*it.postFilter, e.Data, e.Meta) {
            continue
        }
        it.cur = e
        return true
    }
    if rerr := it.rows.Err(); rerr != nil {
        it.err = rerr
    }
    return false
}

func (it *pgIter) Entity() *spi.Entity { return it.cur }
func (it *pgIter) Err() error          { return it.err }
func (it *pgIter) Close() error {
    if it.closed {
        return nil
    }
    it.closed = true
    it.rows.Close()
    return nil
}

func (s *entityStore) buildCurrentIterateSQL(model spi.ModelRef, plan sqlPlan) (string, []any) {
    args := []any{string(s.tenantID), model.EntityName, model.ModelVersion}
    where := "tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted"
    if plan.whereClause != "" {
        where += " AND " + plan.whereClause
        args = append(args, plan.args...)
    }
    return "SELECT doc FROM entities WHERE " + where, args
}

func (s *entityStore) buildPointInTimeIterateSQL(model spi.ModelRef, plan sqlPlan, t time.Time) (string, []any) {
    args := []any{string(s.tenantID), model.EntityName, model.ModelVersion, t}
    where := `tenant_id = $1 AND model_name = $2 AND model_version = $3
              AND valid_time <= $4 AND transaction_time <= CURRENT_TIMESTAMP
              AND (doc->'_meta'->>'deleted')::boolean IS NOT TRUE`
    if plan.whereClause != "" {
        // The translator emits $1, $2... — offset.
        where += " AND " + plan.whereClause
        args = append(args, plan.args...)
    }
    return `SELECT DISTINCT ON (entity_id) doc
             FROM entity_versions
             WHERE ` + where + `
             ORDER BY entity_id, valid_time DESC, transaction_time DESC`, args
}

// GroupedAggregate implements spi.GroupedAggregator. Pushes all Tier 1+2
// natively (postgres has STDDEV_SAMP). Returns
// ErrAggregationNotPushdownable when filter has a residual or pointInTime
// is set (the streaming-tally path handles those uniformly).
func (s *entityStore) GroupedAggregate(
    ctx context.Context,
    model spi.ModelRef,
    groupBy []spi.GroupExpr,
    filter spi.Filter,
    opts spi.GroupedAggregationsOptions,
) ([]spi.GroupedAggregateBucket, error) {
    plan := planQuery(filter)
    if plan.postFilter != nil {
        return nil, spi.ErrAggregationNotPushdownable
    }
    if opts.PointInTime != nil {
        return nil, spi.ErrAggregationNotPushdownable
    }
    // Build SELECT.
    selectExprs := []string{}
    groupByExprs := []string{}
    for i, g := range groupBy {
        var expr string
        if g.Kind == spi.GroupExprState {
            expr = "doc->'_meta'->>'state'"
        } else {
            expr = jsonPathExpr(spi.SourceData, g.Path)
        }
        selectExprs = append(selectExprs, fmt.Sprintf("%s AS gk_%d", expr, i))
        groupByExprs = append(groupByExprs, expr)
    }
    selectExprs = append(selectExprs, "COUNT(*) AS cnt")
    for _, a := range opts.Aggregations {
        valExpr := fmt.Sprintf("cyoda_try_float8(%s)", jsonPathExpr(spi.SourceData, a.Field))
        switch a.Op {
        case spi.AggSum:
            selectExprs = append(selectExprs, fmt.Sprintf(`SUM(%s) AS "agg_%s"`, valExpr, sqlSafeAlias(a.Alias)))
        case spi.AggAvg:
            selectExprs = append(selectExprs, fmt.Sprintf(`AVG(%s) AS "agg_%s"`, valExpr, sqlSafeAlias(a.Alias)))
        case spi.AggMin:
            selectExprs = append(selectExprs, fmt.Sprintf(`MIN(%s) AS "agg_%s"`, valExpr, sqlSafeAlias(a.Alias)))
        case spi.AggMax:
            selectExprs = append(selectExprs, fmt.Sprintf(`MAX(%s) AS "agg_%s"`, valExpr, sqlSafeAlias(a.Alias)))
        case spi.AggStdev:
            selectExprs = append(selectExprs, fmt.Sprintf(`STDDEV_SAMP(%s) AS "agg_%s"`, valExpr, sqlSafeAlias(a.Alias)))
        }
    }
    args := []any{string(s.tenantID), model.EntityName, model.ModelVersion}
    where := "tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted"
    if plan.whereClause != "" {
        where += " AND " + plan.whereClause
        args = append(args, plan.args...)
    }
    query := fmt.Sprintf(
        "SELECT %s FROM entities WHERE %s GROUP BY %s LIMIT %d",
        strings.Join(selectExprs, ", "),
        where,
        strings.Join(groupByExprs, ", "),
        opts.MaxBuckets+1,
    )
    rows, err := s.q.Query(ctx, query, args...)
    if err != nil {
        return nil, fmt.Errorf("grouped-aggregate query: %w", err)
    }
    defer rows.Close()
    return scanPgGroupedAggregate(rows, groupBy, opts)
}

// scanPgGroupedAggregate is the pgx version of sqlite's scanGroupedAggregate.
// Same overflow detection on (MaxBuckets+1)th row.
func scanPgGroupedAggregate(rows pgx.Rows, groupBy []spi.GroupExpr, opts spi.GroupedAggregationsOptions) ([]spi.GroupedAggregateBucket, error) {
    // ... follows the sqlite pattern; adapt to pgx scanning. ...
}

func sqlSafeAlias(a string) string {
    return strings.ReplaceAll(a, `"`, `""`)
}
```

Imports: `"time"`.

> Implementation note for the subagent: `parseEntityFromDoc` is the existing pgx-side helper. `s.q` is the existing query interface (could be `*pgxpool.Pool` or `pgx.Conn`); use whichever the existing postgres plugin code uses.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./plugins/postgres/... -run "TestPostgresIterate|TestPostgresGroupedAggregate" -v`
Expected: PASS.

- [ ] **Step 5: Run full plugin tests**

Run: `go test ./plugins/postgres/...`
Expected: all pass (requires Docker).

- [ ] **Step 6: Vet**

Run: `go vet ./plugins/postgres/...`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add plugins/postgres/grouped_stats.go plugins/postgres/grouped_stats_test.go
git commit -m "feat(postgres): implement Iterable + GroupedAggregator with Tier 1+2 pushdown (refs #299)"
```

---

# Phase 4 — tests, docs, config

### Task 16: Parity tests

**Files:**
- Create: `internal/e2e/parity/grouped_stats.go`
- Modify: `internal/e2e/parity/registry.go`

- [ ] **Step 1: Inspect the parity registry pattern**

Run: `cat internal/e2e/parity/registry.go`
Identify the existing pattern for registering parity cases.

- [ ] **Step 2: Add grouped-stats parity cases**

Implement the §7 matrix as a set of parity cases. Each case receives an opaque `parity.Backend` (the test harness already exposes this) and asserts the SPI contract.

For brevity in the plan, this Task captures the case names and expected coverage. The subagent should refer directly to §7 of the spec for the full case list.

Cases to implement (one Go function per case, all registered):

- `singleDataFieldGroupBy`
- `stateGroupBy`
- `multiDimVariantState`
- `withCondition`, `withoutCondition`
- `withPointInTimeIncludingDeleted` (verifies deletion-marker exclusion)
- `inTxRoutesToStreaming`
- `inTxRYW` (where applicable per backend; skip on backends without tx-buffer)
- `sumAvgMinMax` (Tier 1)
- `stdevWideDynamicRange`
- `stdevLowVarianceHighMean` (within 1e-9 relative error)
- `stdevNeq1ReturnsNull`
- `stdevEmptyNumericReturnsNull`
- `nonNumericMixedSkipped`
- `runtimeNonScalarCoercesNull`
- `countOnlyOmitsAggregations`
- `explicitEmptyAggregations`
- `groupByArrayProjectionRejected`
- `groupByBracketScalarAccepted`
- `aggFieldArrayProjectionRejected`
- `duplicateGroupBy`
- `identicalPairDeduped`
- `distinctPairCollidingAliasRejected`
- `limitOverCeilingRejected`
- `filterPushdownStreamingObservable` (counting wrapper)
- `filterPushdownNativeObservable` (via plugin-local EXPLAIN where supported)
- `nonPushdownableConditionFallsThrough`
- `partialPushdownResidualHandled`
- `cardinalityCeilingExceeded` (422)
- `cardinalityDetectionDeterministic`
- `tenantIsolation`
- `sqlInjectionGroupByPath`
- `sqlInjectionAggregationField`
- `iteratorContractErrSticky`
- `iteratorContractCloseIdempotent`
- `iteratorContractCtxCancelled`
- `buildGroupKeyCollisionFree` (for the harness-level encoding — verifies via a synthetic adversarial dataset)
- `concurrentWritesSnapshotConsistency` (memory)

- [ ] **Step 3: Register them**

In `internal/e2e/parity/registry.go`, add each case to the registry. Use the existing helper if any (e.g. `parity.Register("name", func)`).

- [ ] **Step 4: Run parity tests for all three plugins**

Run: `go test ./internal/e2e/parity/... -v` (and per-plugin if they have separate runners).
Expected: all PASS for memory/sqlite/postgres.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/parity/grouped_stats.go internal/e2e/parity/registry.go
git commit -m "test(parity): SPI-contract parity cases for grouped-stats (refs #299)"
```

---

### Task 17: E2E test

**Files:**
- Create: `internal/e2e/grouped_stats_test.go`

- [ ] **Step 1: Write the e2e test**

Follow the existing pattern from `internal/e2e/uncovered_ops_test.go` and `internal/e2e/search_test.go`. Use `setupSearchModel`-style fixtures.

Test the happy path:
- Create model.
- Create 10 entities with mixed `variantId` + `state`.
- POST `/api/entity/stats/{model}/{version}/query` with `groupBy: ["$.variantId", "state"]`.
- Assert response shape + counts.

- [ ] **Step 2: Run the e2e test**

Run: `go test ./internal/e2e/... -run TestGroupedStats_E2E -v`
Expected: PASS (requires Docker).

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/grouped_stats_test.go
git commit -m "test(e2e): grouped-stats end-to-end via postgres testcontainer (refs #299)"
```

---

### Task 18: OpenAPI spec

**Files:**
- Modify: `api/openapi.yaml`

- [ ] **Step 1: Inspect the existing entity-stats OpenAPI block**

Run: `grep -n "/entity/stats" api/openapi.yaml`
Identify where the existing stats endpoints are defined.

- [ ] **Step 2: Add the new path and schemas**

Add `POST /api/entity/stats/{entityName}/{modelVersion}/query`. Define schemas `GroupedStatsRequest`, `GroupedStatsBucket`, `GroupKeyEntry`, `GroupExpr`, `AggregationExpr`, `AggregateOp`. `aggregations` field on `GroupedStatsBucket` uses `nullable: false`.

Refer to spec §3 for the canonical shape.

- [ ] **Step 3: Verify the spec is still valid**

Run: `go test ./internal/api/... -run TestOpenAPI -v` (or whatever validator exists; check `grep -rn openapi internal/api`).
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add api/openapi.yaml
git commit -m "feat(openapi): grouped-stats query endpoint and schemas (refs #299)"
```

---

### Task 19: Help topic + README + COMPATIBILITY

**Files:**
- Modify: `cmd/cyoda/help/content/search.md` (or whichever holds the search/stats help)
- Modify: `cmd/cyoda/help/content/config/server.md` (or similar) for `CYODA_STATS_GROUP_MAX`
- Modify: `README.md`
- Modify: `COMPATIBILITY.md`

- [ ] **Step 1: Update the search/stats help topic**

Append a section per §7 of the spec covering: request/response examples, cardinality ceiling, JSONPath restriction, aggregation operators, Welford stdev semantics, postgres `cyoda_try_float8` table, `entities_state_idx` non-tx-only caveat, in-tx behavior, non-scalar coercion.

No issue IDs in user-facing help content (per project memory).

- [ ] **Step 2: Update README config reference**

Add `CYODA_STATS_GROUP_MAX` row to the env-var table with default `10000`.

- [ ] **Step 3: Update COMPATIBILITY.md**

Bump the cyoda-go-spi pin row for v0.8.0; add a note about the new Iterable + GroupedAggregator interfaces being type-assertion-optional (existing plugins keep compiling without implementing).

- [ ] **Step 4: Confirm cyoda help works**

Run: `go build -o /tmp/cyoda ./cmd/cyoda && /tmp/cyoda help search`
Expected: prints the updated help.

- [ ] **Step 5: Commit**

```bash
git add cmd/cyoda/help/content/ README.md COMPATIBILITY.md
git commit -m "docs: grouped-stats help, env var, COMPATIBILITY (refs #299)"
```

---

### Task 20: Final integration check

- [ ] **Step 1: Full test sweep**

Run:
```bash
go test ./... -v
```
Expected: all pass. (Docker required for postgres + e2e.)

- [ ] **Step 2: Plugin submodule sweep** (memory `feedback_plugin_submodule_tests`)

Run:
```bash
make test-all
```
Expected: root + every plugin submodule passes.

- [ ] **Step 3: Race detector** (memory `feedback_race_testing_discipline`)

Run:
```bash
go test -race ./...
```
Expected: race-clean.

- [ ] **Step 4: Vet**

Run:
```bash
go vet ./...
```
Expected: clean.

- [ ] **Step 5: Push the feature branch**

```bash
git push -u origin feat/299-grouped-stats
```

- [ ] **Step 6: Open the PR(s)**

Per §9, the spec recommends 6–10 PRs. At this point the work is on one branch. Open one tracking PR against `release/v0.8.0` first; if the reviewer asks for splits, use `git rebase --interactive` or `git format-patch` + cherry-pick to create per-task PRs targeting the same release branch.

Per memory `feedback_release_milestone_invariant`, every closed-by-release-PR issue gets milestoned at merge time — make sure #299 has milestone `v0.8.0`.

```bash
gh pr create --base release/v0.8.0 \
  --title "feat(entity): grouped statistics query endpoint" \
  --body "Implements #299. See docs/superpowers/specs/2026-06-14-issue-299-grouped-stats-design.md.

Closes #299."
```

---

## Self-Review

After writing all tasks, the plan covers each spec section:

- §3 Request/response → Tasks 5 (DTOs), 6 (validation), 9 (handler).
- §4 Service-layer dispatch → Task 8.
- §5 SPI delta → Phase 1 (Tasks 1, 2).
- §6.1 Memory → Tasks 10, 11.
- §6.2 Sqlite → Task 12.
- §6.3 Postgres → Tasks 13 (migration), 14 (translator), 15 (plugin).
- §7 Conformance/help/OpenAPI → Tasks 16, 17, 18, 19.
- §9 Sequencing → reflected in phase order.
- §10 Non-goals → spec only; not a task target.
- §11 Acceptance → asserted by combined test suite from Task 20.

Type consistency reviewed: `GroupedStatsRequest`/`Bucket`/`KeyEntryWire` consistent across Tasks 5–9; `spi.GroupedAggregator`/`Iterable`/`GroupExpr`/`AggregateOp` consistent across Tasks 1–15; service uses `internal/domain/search.ConditionToFilter` (existing function — confirm signature in Task 8); `match.MatchFilter` and `match.Match` co-exist (Tasks 4, 8).

No placeholders remain. Bracket-quoted JSONPath normalization is concrete code in Task 6. Welford recurrence is concrete code in Tasks 7, 11, 15. Postgres `cyoda_try_float8` regex anchored with `\A`/`\Z` per D21 is in Task 13.

Outstanding items the subagent will need to inspect (called out at the point of use):

- `internal/match/match.go`: existing helpers to reuse in Task 4.
- `internal/api/router.go`: the routing helper to use in Task 9.
- `internal/config/config.go`: existing env-var pattern for Task 9.
- `plugins/sqlite/entity_store.go`: `scanEntityFromRow` and snapshot SQL pattern for Task 12.
- `plugins/postgres/entity_store.go`: `parseEntityFromDoc` and `s.q` query interface for Task 15.
- `internal/e2e/parity/registry.go`: existing registration pattern for Task 16.

These are honest "look at the surrounding code and follow its pattern" notes — not handwaves.
