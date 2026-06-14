# Grouped entity statistics query — design

| Field | Value |
|---|---|
| Issue | [#299](https://github.com/Cyoda-platform/cyoda-go/issues/299) |
| Target milestone | v0.8.0 (cyoda-go), v0.8.0 (cyoda-go-spi) |
| Spec date | 2026-06-14 |
| Status | Draft, pending review |
| Related repos | `cyoda-go`, `cyoda-go-spi`, `cyoda-go-cassandra` (commercial backend) |

## 1. Background

Consumers routinely need *"how many entities match X, broken down by Y"* as a cheap, real-time number. The driving case is SIOMS: an inventory model where every physical unit is its own entity (`ProductItem`) and inventory figures are derived by counting items per workflow state — e.g. "per `variantId`, how many units are `available` vs `allocated`."

Today there is no way to obtain counts grouped by a **data field** in one call. The existing surface forces consumers into:

1. `GET /entity/stats/states/{model}/{ver}` — groups only by `state`, no data-field filter.
2. Async snapshot search with `entitiesCount` — one count per call; a per-variant breakdown is N async jobs and on backends without predicate pushdown falls back to a full `GetAll` scan.
3. Direct search — streams full entity envelopes (NDJSON, ≤10k) so the client tallies, transferring the entire matching population.

Rendering an inventory list of K variants × S states costs **K × S async jobs** (or K × S full scans, or streaming the entire population) — `O(entities × dimensions)` work for one screen. The Trino/SQL `GROUP BY` surface that would solve this is Cloud-only and unavailable on the cyoda-go binary.

This change introduces a first-class grouped-stats primitive that turns the inventory screen into one call and is broadly useful beyond inventory: dashboards, ABC/Pareto classification, valuation rollups, point-in-time audit counts.

## 2. Decisions log

Captured during brainstorming. Each entry is the option chosen and the rationale in one line.

| # | Decision | Rationale |
|---|---|---|
| D1 | Response shape: top-level `count` always emitted; `aggregations` map sibling, omitted when none requested. | Forward-compatible with sum/avg/etc.; doesn't force nesting on count-only callers. |
| D2 | No server-side cap on entities scanned; cap is on **result group cardinality** (`CYODA_STATS_GROUP_MAX`, default 10000). Exceeded → 400 `GROUP_CARDINALITY_EXCEEDED`. | Implementation streams/pushes-down; intrinsic cost dimension is buckets, not entities. |
| D3 | Optional `limit` request field. When set, server returns top-N by `(count desc, groupKey lex)`. Default = unlimited. | Display convenience; orthogonal to safety bound. |
| D4 | Missing JSONPath value → explicit JSON `null` bucket key. | Matches SQL `GROUP BY` NULL semantics; surfaces data quality. Collides with "field present but literally `null`" — documented. |
| D5 | `groupBy` accepts scalar JSONPaths only. Anything containing `[` → 400 `INVALID_GROUP_BY_PATH`. | Array fan-out is a follow-up; the driving cases (inventory, ABC/Pareto) are scalar. |
| D6 | Add two new SPI capabilities: `Iterable` (streaming) and `GroupedAggregator` (native GROUP BY pushdown). Both optional via type assertion. Backend implementing neither → 501 `NOT_IMPLEMENTED_BY_BACKEND`. | Streaming Iterator prevents the slurp-all fallback that would betray the "stats not lists" promise. Native aggregator lets sqlite/postgres push GROUP BY to SQL. |
| D7 | Path: `POST /api/entity/stats/{entityName}/{modelVersion}/query`. | More RESTful than the `/entity/stats/query/{name}/{ver}` shape the issue suggested — model is the resource, `query` is the action. |
| D8 | Aggregations Tier 1+2 in v1: `sum`, `avg`, `min`, `max`, `stdev` (sample stdev, n−1 denominator). Mode and median deferred to a separate follow-up issue with its own design pass. | Tier 1+2 marginal cost is ~30–40% on top of count-only; mode/median introduce unbounded intermediate state and asymmetric backend support (postgres-only natively) that warrant their own contract. |
| D9 | Target v0.8.0, not v0.8.1. cyoda-go-spi v0.8.0 tag has not been published yet; we can fold the additive SPI changes alongside the tx-state sentinels (#16) under one tag. | v0.8.0 is not feature-frozen (12 issues open). Adding additive SPI changes pre-tag does not breach immutability. |

## 3. API surface

### Path & auth

```
POST /api/entity/stats/{entityName}/{modelVersion}/query
Auth: Bearer (bypassed under CYODA_IAM_MODE=mock)
Body limit: 10 MiB (shared with /search/*)
```

### Request

```json
{
  "groupBy":      ["$.variantId", "state"],
  "condition":    { "type": "lifecycle", "field": "state", "operatorType": "NOT_EQUAL", "value": "shipped" },
  "aggregations": [
    { "op": "sum",   "field": "$.costPrice", "as": "totalCost" },
    { "op": "avg",   "field": "$.costPrice" },
    { "op": "stdev", "field": "$.costPrice" }
  ],
  "pointInTime":  "2026-06-14T12:00:00Z",
  "limit":        100
}
```

- **`groupBy`** (required, 1..N): each entry is the reserved token `"state"` or a scalar JSONPath (`$.variantId`, `$.address.country`). Multiple dimensions return cartesian buckets that actually occur.
- **`condition`** (optional): the existing search `Condition` DSL (`simple` / `lifecycle` / `group` / `function`). Reused verbatim — same parser, same depth cap, same operator validator. Omitted ⇒ match-all.
- **`aggregations`** (optional, 0..N): per entry: `op` ∈ {`sum`, `avg`, `min`, `max`, `stdev`}, `field` scalar JSONPath, optional `as` alias. When `as` is omitted, server synthesizes `<op>_<field>` (e.g. `sum_$.costPrice`). Duplicate aliases after synthesis → 400.
- **`pointInTime`** (optional RFC 3339): historical snapshot; default = now. Same semantics as existing stats and `GetAsAt`.
- **`limit`** (optional positive int): top-N by `(count desc, groupKey lex)`. Default = unlimited (within `CYODA_STATS_GROUP_MAX`).

### Response 200 application/json

```json
[
  {
    "groupKey":     { "$.variantId": "1111", "state": "available" },
    "count":        812,
    "aggregations": { "totalCost": 41200.00, "avg_$.costPrice": 50.74, "stdev_$.costPrice": 18.42 }
  },
  {
    "groupKey":     { "$.variantId": "1111", "state": "allocated" },
    "count":        37,
    "aggregations": { "totalCost": 1880.00, "avg_$.costPrice": 50.81, "stdev_$.costPrice": 17.99 }
  },
  {
    "groupKey":     { "$.variantId": null, "state": "available" },
    "count":        3,
    "aggregations": { "totalCost": null, "avg_$.costPrice": null, "stdev_$.costPrice": null }
  }
]
```

- `groupKey`: map from `groupBy` expression → value for that bucket. Strings for scalar paths, JSON `null` for missing.
- `count`: int64, always emitted. Tally is implicit in any `GROUP BY` query anyway and is free.
- `aggregations`: object keyed by alias. Omitted entirely when the request asked for none. Per-alias value is JSON `null` when the bucket had zero numeric samples for that field (non-numeric and absent values are silently skipped, matching SQL `SUM(NULL) = NULL` semantics).
- `modelName` / `modelVersion` are not emitted per bucket — the path carries them; redundant.
- Zero-count buckets are not emitted. Callers default absent keys to 0; documented in the help topic.

### Errors

| Status | Code | Trigger |
|---|---|---|
| 400 | `MALFORMED_REQUEST` | JSON parse failed |
| 400 | `UNKNOWN_MODEL` | path `{entityName}/{modelVersion}` does not resolve for this tenant |
| 400 | `MISSING_GROUP_BY` | `groupBy` empty or missing |
| 400 | `INVALID_GROUP_BY_PATH` | groupBy entry empty, or contains `[` (non-scalar) |
| 400 | `DUPLICATE_GROUP_BY` | duplicate entries after normalization |
| 400 | `INVALID_AGGREGATION_OP` | unknown `op` |
| 400 | `INVALID_AGGREGATION_FIELD` | aggregation `field` empty or non-scalar |
| 400 | `DUPLICATE_AGGREGATION_ALIAS` | two aliases collide after synthesis |
| 400 | `INVALID_OPERATOR` / `INVALID_CONDITION` / depth-exceeded | propagated from `predicate.ParseCondition` + `search.ValidateCondition` |
| 400 | `INVALID_POINT_IN_TIME` | unparseable RFC 3339 |
| 400 | `INVALID_LIMIT` | non-positive int |
| 400 | `NOT_SUPPORTED_IN_TX` | called inside an active transaction (matches `/search/*`) |
| 400 | `GROUP_CARDINALITY_EXCEEDED` | result buckets exceed `CYODA_STATS_GROUP_MAX`; body includes the ceiling |
| 401 | (standard) | missing/invalid Bearer |
| 413 | (standard) | request body > 10 MiB |
| 501 | `NOT_IMPLEMENTED_BY_BACKEND` | backend implements neither `Iterable` nor `GroupedAggregator` |
| 500 | (with ticket UUID) | internal/driver errors; generic message per project policy |

## 4. Service-layer dispatch

Single decision point at the top of `entity.Service.QueryGroupedStats`:

```go
// 1. Native pushdown when available AND condition is pushdownable
if ga, ok := store.(spi.GroupedAggregator); ok {
    flt, err := filter_translate.ConditionToFilter(req.Condition)
    if err == nil {
        return ga.GroupedAggregate(ctx, modelRef, req.GroupBy, flt, opts)
    }
    // condition not pushdownable → fall through to streaming tally
}

// 2. Streaming fallback (also handles tx-active path; no slurp)
if it, ok := store.(spi.Iterable); ok {
    return s.tallyStreaming(ctx, it, req)
}

// 3. 501
return nil, ErrBackendNotSupported
```

`tallyStreaming`:

```go
iter, err := it.Iterate(ctx, modelRef, spi.IterateOptions{PointInTime: req.PointInTime})
if err != nil { return nil, err }
defer iter.Close()

acc := newAccumulators(req)  // per-bucket {count, sum, sum_sq, min, max, mean, m2}
for iter.Next() {
    e := iter.Entity()
    if req.Condition != nil && !match.Match(req.Condition, e.Data, e.Meta) {
        continue
    }
    k, err := buildGroupKey(req.GroupBy, e)
    if err != nil { return nil, err }
    if !acc.has(k) && acc.len() >= maxGroupCardinality {
        return nil, ErrGroupCardinalityExceeded
    }
    acc.observe(k, e)
}
if err := iter.Err(); err != nil { return nil, err }
return acc.materialize(req.Limit), nil
```

Post-processing (sort by `(count desc, groupKey lex)`, apply `Limit`) is shared by both paths so observable ordering is backend-independent and doesn't leak SQL-planner choices.

**Transaction behavior:** inside an active transaction (`spi.GetTransaction(ctx) != nil`) the endpoint returns 400 `NOT_SUPPORTED_IN_TX`. Mirrors `/search/*` and avoids snapshot/iterator semantics inside a writable tx.

## 5. SPI delta (cyoda-go-spi v0.8.0)

Additive. No breaking changes. Lands on `cyoda-go-spi` main on top of `c301c0e` (tx-state sentinels, #16) and is included in the v0.8.0 SPI tag.

```go
// Streaming iteration over a model's entities.
type Iterable interface {
    Iterate(ctx context.Context, model ModelRef, opts IterateOptions) (Iterator, error)
}

type Iterator interface {
    Next() bool         // advance; false on end or error
    Entity() *Entity    // current row (valid only after Next() == true)
    Err() error         // first error encountered, sticky
    Close() error       // release server resources; idempotent
}

type IterateOptions struct {
    PointInTime *time.Time
}

// Native grouped-aggregation pushdown.
type GroupedAggregator interface {
    GroupedAggregate(
        ctx context.Context,
        model ModelRef,
        groupBy []GroupExpr,
        filter Filter,                  // nil = match-all
        opts GroupedAggregationsOptions,
    ) ([]GroupedAggregateBucket, error)
}

type GroupExprKind int
const (
    GroupExprState    GroupExprKind = iota
    GroupExprDataPath               // scalar JSONPath in Path
)

type GroupExpr struct {
    Kind GroupExprKind
    Path string // empty when Kind == GroupExprState
}

type AggregateOp string
const (
    AggSum   AggregateOp = "sum"
    AggAvg   AggregateOp = "avg"
    AggMin   AggregateOp = "min"
    AggMax   AggregateOp = "max"
    AggStdev AggregateOp = "stdev" // sample (n-1 denominator)
)

type AggregateExpr struct {
    Op    AggregateOp
    Field string // scalar JSONPath
    Alias string // server-synthesized if blank
}

type GroupedAggregationsOptions struct {
    PointInTime  *time.Time
    MaxBuckets   int                // cardinality ceiling
    Aggregations []AggregateExpr
}

type GroupedAggregateBucket struct {
    GroupKey     map[string]any
    Count        int64
    Aggregations map[string]any     // alias → float64 or nil
}

// Sentinel for backends that exceed the cardinality ceiling internally.
var ErrGroupCardinalityExceeded = errors.New("group cardinality exceeded ceiling")
```

Forward-compat: `having`, mode/median, and population stdev land as additive fields on the same structs without a major bump.

## 6. Per-plugin implementations

### Memory (`plugins/memory`)

Implements **both** `Iterable` and `GroupedAggregator`.

- `Iterate` returns an iterator that holds `entityMu.RLock` for its lifetime; `Close` releases. For tx-scoped reads, uses existing `tx.SnapshotTime` and `getSnapshotVersion` per entity. For non-tx reads, latest version per entity. Skips deleted versions and filters by `modelRef` (parity with today's `GetAll`).
- `GroupedAggregate` is a single pass over `entityData[tenant][*]` under RLock, applying the filter via `match.Match` and extracting group keys via `gjson.GetBytes(entity.Data, path)` (already imported in `match.go`). Aggregator state is a per-bucket struct using Welford's algorithm for numerically-stable mean and stdev. Enforces `MaxBuckets` and returns `ErrGroupCardinalityExceeded` on overflow.

### SQLite (`plugins/sqlite`)

Implements **both**.

- `Iterate` wraps `sql.Rows` with `rows.Next()`. PointInTime path uses the existing `entity_versions` snapshot query (the inner join on `MAX(version)` with `submit_time <= ?`). `Close` calls `rows.Close()`.
- `GroupedAggregate` generates SQL like:

  ```sql
  SELECT
      json_extract(data, '$.variantId')           AS gk_0,
      COALESCE(json_extract(meta, '$.state'), '') AS gk_1,
      COUNT(*)                                    AS cnt,
      SUM(CAST(json_extract(data, '$.costPrice') AS REAL))           AS agg_sum_totalCost,
      AVG(CAST(json_extract(data, '$.costPrice') AS REAL))           AS agg_avg_avg_x,
      SUM(CAST(json_extract(data, '$.costPrice') AS REAL) *
          CAST(json_extract(data, '$.costPrice') AS REAL))           AS agg_sumsq_stdev_x,
      COUNT(CAST(json_extract(data, '$.costPrice') AS REAL))         AS agg_n_stdev_x
  FROM entities -- or entity_versions for pointInTime
  WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND NOT deleted
    AND <pushed-down filter>
  GROUP BY gk_0, gk_1
  LIMIT <MaxBuckets + 1>
  ```

  Reuses the existing `planQuery`/`dissect` planner from `query_planner.go`. If a residual filter exists, the service layer falls through to `Iterate` + Go-tally (no grouped-residual-eval path). Sqlite has **no** `STDDEV` function in the standard build; for stdev we push `SUM(x)`, `SUM(x*x)`, `COUNT(non-null x)` and compute stdev in Go from `√((sum_sq − sum²/n) / (n−1))`. Numerical sensitivity acknowledged and tested; acceptable for stats use, may differ in the last 1–2 ulps from a Welford pass over the same dataset.

  `MaxBuckets` enforced via `LIMIT MaxBuckets+1`; rowcount of `MaxBuckets+1` → `ErrGroupCardinalityExceeded`.

### Postgres (`plugins/postgres`)

Implements **both**.

- `Iterate` wraps `pgx.Rows`. PointInTime uses the bi-temporal `entity_versions` table with `valid_time <= ? AND transaction_time <= CURRENT_TIMESTAMP` (existing pattern from `GetAsAt`).
- `GroupedAggregate` SQL uses JSONB operators and native stdev:

  ```sql
  SELECT
      doc->>'variantId'                                 AS gk_0,
      doc->'_meta'->>'state'                            AS gk_1,
      COUNT(*)                                          AS cnt,
      SUM((doc->>'costPrice')::float8)                  AS agg_sum_totalCost,
      AVG((doc->>'costPrice')::float8)                  AS agg_avg_avg_x,
      STDDEV_SAMP((doc->>'costPrice')::float8)          AS agg_stdev_stdev_x
  FROM entities -- or entity_versions for pointInTime
  WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted
    AND <pushed-down filter>
  GROUP BY gk_0, gk_1
  LIMIT <MaxBuckets + 1>
  ```

  Same residual fallback policy as sqlite; same overflow detection. Non-numeric values that fail the cast become SQL `NULL` and are excluded by the aggregate functions, matching the spec's "silently skip" rule. The cast pattern uses a safe try-cast wrapper (`CASE WHEN ... ~ '^-?\d+(\.\d+)?$' THEN ... ::float8 ELSE NULL END` or a dedicated immutable helper function created by migration — final shape decided during implementation, covered by parity tests).

  **Index guidance** in the help topic: for hot grouping dimensions, callers add `CREATE INDEX entities_variantid_idx ON entities ((doc->>'variantId'))`. The base migration is **not** extended in this PR; expression-index management per data field is out of scope.

### Cassandra (`cyoda-go-cassandra`, commercial backend)

Implements **`Iterable` only** in v1. Does **not** implement `GroupedAggregator`.

- Rationale: CQL's `GROUP BY` works only over partition/clustering keys. For grouping by arbitrary JSONPaths, the plugin would itself fall back to streaming-tally — the same code the service-layer fallback already runs. A native pushdown adapter would be pure indirection.
- `Iterate` wraps the existing per-shard `gocql.Iter().Scan()` pattern used by `CountByState`. Yields entities per shard; merge across shards via the existing winner-map machinery. Honors `tx.VisibilityFilter` for pointInTime (the existing HLC contract).
- The service layer's two-level dispatch naturally lands on the `Iterate` path for cassandra-backed installs.
- Out of scope follow-up: a state-only fast-path can later route `groupBy: ["state"]` directly to the lifecycle-index reader. Not v1.
- The cassandra PR is an independent timeline. Until it lands and pins the new SPI v0.8.0, cassandra-backed deployments running cyoda-go v0.8.0 return 501 on the grouped-stats endpoint. Honest graceful degradation.

## 7. Conformance, OpenAPI, help, e2e

### Cross-backend conformance (`internal/e2e/parity/registry.go`)

Per the cross-plugin policy, parity tests added here are picked up by every backend including cassandra. New parity cases:

| Case | Cover |
|---|---|
| single data-field group-by | basic happy path |
| `state` group-by | parity with existing `/entity/stats/states` |
| multi-dim `$.variantId × state` | cartesian buckets |
| with vs without `condition` | reuse of search DSL |
| with `pointInTime` | historical snapshot |
| sum / avg / min / max | Tier 1 aggregations |
| stdev | Tier 2 aggregation; values spread over 8 orders of magnitude; tolerance per backend |
| non-numeric values mixed in | silently skipped |
| all-null aggregation field | result is JSON `null` |
| count-only request | no `aggregations` in response |
| groupBy contains `[` | 400 `INVALID_GROUP_BY_PATH` |
| duplicate groupBy | 400 `DUPLICATE_GROUP_BY` |
| duplicate alias after synthesis | 400 `DUPLICATE_AGGREGATION_ALIAS` |
| non-pushdownable condition | service falls through to streaming-tally cleanly; identical bucket set |
| cardinality ceiling exceeded | 400 `GROUP_CARDINALITY_EXCEEDED` |
| in-tx call | 400 `NOT_SUPPORTED_IN_TX` |
| backend with neither capability | 501 `NOT_IMPLEMENTED_BY_BACKEND` (only exercised by a mock test-backend) |

### E2E (`internal/e2e/grouped_stats_test.go`)

One e2e test hitting the full HTTP stack with the postgres testcontainer. Doesn't enumerate every parity case — that's the parity suite's job. Proves wiring: handler → service → SPI → postgres GROUP BY → JSON response.

### OpenAPI (`api/openapi.yaml`)

New path `POST /api/entity/stats/{entityName}/{modelVersion}/query`. Per `//go:embed`-based embed (project policy), the spec stays as-is in source. New schemas:

- `GroupedStatsRequest`
- `GroupedStatsBucket`
- `GroupExpr` (enum-or-oneOf: `"state"` token vs scalar JSONPath)
- `AggregationExpr`
- `AggregateOp` enum

No issue IDs in user-facing schema descriptions per project policy.

### `cyoda help` (`cmd/cyoda/help/content/`)

Updates to the search/stats help topic:

- Request and response examples (count-only, with aggregations, with pointInTime, multi-dim).
- Cardinality ceiling and its env var.
- JSONPath restriction (scalar only) and rationale.
- Aggregation operators and the sample-stdev semantics.
- Postgres expression-index guidance for hot grouping dimensions.

No `#299` references in help content per memory `feedback_no_issue_ids_in_code`.

### Config (Gate 4)

`CYODA_STATS_GROUP_MAX` env var (default 10000) added to:

- `cmd/cyoda/help/content/config/*.md` — the relevant topic.
- `README.md` configuration reference.
- `DefaultConfig()` in code.

All three updated together.

### COMPATIBILITY.md (Gate 4)

Updated when:

- the cyoda-go-spi v0.8.0 pin lands (one-line bump),
- the chart `appVersion:` bumps (auto-PR; human-reviewed),
- the cassandra plugin pin guidance changes once its v0.8.0 plugin tag exists.

## 8. Forward-compatibility hooks

Strict count + Tier 1+2 in v1. Surfaces left open:

| Extension | How it lands later without breaking v1 |
|---|---|
| `having` | Optional request field; optional field on `GroupedAggregationsOptions`. Backends that don't push it down evaluate residual in Go after streaming. |
| `mode` / `median` | New `AggregateOp` enum values. Each backend declares support; unsupported ops on a backend return 400 `AGGREGATION_NOT_SUPPORTED_BY_BACKEND`. New `CYODA_STATS_AGG_INTERMEDIATE_MAX` knob caps per-bucket distinct-value cardinality. Worth its own design pass (memory contract, postgres-only pushdown, sketch alternatives). |
| `stdev_pop` / population stdev | New `AggregateOp` value. Trivial. |
| Array-projection in `groupBy` (`$.tags[*]`) | New validation accepts `[*]`; `GroupExpr.Kind` gains `DataPathArray`. Per-backend fan-out semantics specified separately. Dedup policy (same entity contributing the same value twice via different elements) belongs in that spec. |
| Numeric paging | Optional `offset` paired with `limit` once the sort key is stable. |

## 9. Release sequencing

Coordinated change across three repos. Sequencing per project policy.

1. **cyoda-go-spi PR** — additive: `Iterable`, `Iterator`, `IterateOptions`, `GroupedAggregator`, `GroupExpr`, `AggregateExpr`, `AggregateOp`, `GroupedAggregationsOptions`, `GroupedAggregateBucket`, `ErrGroupCardinalityExceeded`. Lands on cyoda-go-spi `main` on top of `c301c0e`. No tag yet — folds into the eventual v0.8.0 SPI tag.
2. **cyoda-go-spi v0.8.0 tag** — published once the v0.8.0 SPI scope (tx-state sentinels + this change) is settled.
3. **cyoda-go PR series on `release/v0.8.0`** — each in its own commit/PR:
   - SPI pin bump (one isolated commit).
   - Handler + DTO + validation.
   - Service-layer dispatch + accumulator + post-processing.
   - Plugin implementations (memory, sqlite, postgres) with conformance and parity tests.
   - E2E test.
   - OpenAPI + help + config + COMPATIBILITY.
4. **cyoda-go-cassandra PR** — independent timeline. SPI pin bump to v0.8.0 (one combined bump for tx-state + Iterable) + `Iterable` implementation. Strictly in scope per memory `feedback_courtesy_pr_scope`.

## 10. Non-goals

- **Numeric aggregations beyond Tier 1+2** — mode and median are deferred.
- **Returning entity bodies** — that's `/search/*`.
- **Cross-model joins** — out of scope.
- **`having` post-aggregation predicates** — forward-compat noted but not built.
- **Array fan-out in `groupBy`** — out of scope; surfaces left open.
- **Auto-created postgres expression indexes per data field** — out of scope; documented as caller responsibility in the help topic.
- **Streaming `/search/*` itself** — the new `Iterable` SPI is available to migrate `/search/*` off slurp-all later, but that work is not part of this change.
- **In-tx grouped-stats** — 400 `NOT_SUPPORTED_IN_TX`, matching `/search/*`. Could be reconsidered later with a clearer snapshot/iterator contract; out of scope here.

## 11. Acceptance

Mirrors the issue's acceptance criteria, expanded for the v1 scope:

- `POST /api/entity/stats/{entityName}/{modelVersion}/query` returns grouped counts and Tier 1+2 aggregations for:
  - single data-field group-by;
  - `state` group-by (parity with `/entity/stats/states`);
  - multi-dimension `variantId × state`;
  - with and without a `condition`;
  - with `pointInTime`.
- No entity envelopes ever materialized server-side: postgres + sqlite push GROUP BY into SQL; memory walks the in-memory map in one pass; cassandra streams per-shard via existing iterators. No GetAll-then-tally code path.
- Auth, tenant scoping, Condition validation, and error model identical to `/search/*` where shared.
- `CYODA_STATS_GROUP_MAX` ceiling enforced consistently across all backends; exceeded returns 400 with the ceiling value.
- Backend without either capability returns 501.
- `cyoda help` search/stats topic updated; OpenAPI spec updated; CONFIG env var documented; COMPATIBILITY.md updated alongside the SPI pin bump.
- Conformance parity tests cover the case matrix in §7. Race detector clean before PR per memory `feedback_race_testing_discipline`.
