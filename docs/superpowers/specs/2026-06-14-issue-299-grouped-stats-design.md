# Grouped entity statistics query — design

| Field | Value |
|---|---|
| Issue | [#299](https://github.com/Cyoda-platform/cyoda-go/issues/299) |
| Target milestone | v0.8.0 (cyoda-go), v0.8.0 (cyoda-go-spi) |
| Spec date | 2026-06-14 |
| Review iterations | 1 (independent fresh-context review folded in) |
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

Captured during brainstorming and the design-review iteration. Each entry is the option chosen and the rationale in one line.

| # | Decision | Rationale |
|---|---|---|
| D1 | Response shape: top-level `count` always emitted; `aggregations` map sibling, omitted when none requested. | Forward-compatible with sum/avg/etc.; doesn't force nesting on count-only callers. |
| D2 | No server-side cap on entities scanned; cap is on **result group cardinality** (`CYODA_STATS_GROUP_MAX`, default 10000). Exceeded → 422 `GROUP_CARDINALITY_EXCEEDED`. | Implementation streams/pushes-down; intrinsic cost dimension is buckets, not entities. 422 (semantic, unprocessable) is more correct than 400 — the request itself is well-formed. |
| D3 | Optional `limit` request field. When set, server returns top-N by `(count desc, groupKey lex)`. Default = unlimited. | Display convenience; orthogonal to safety bound. |
| D4 | Missing JSONPath value → JSON `null` bucket key. Conflates with values that are literally JSON `null` (matches SQL `IS NULL` / `IS NOT DISTINCT FROM NULL` semantics). Deliberate v1 simplification. | A sentinel string (`"__missing__"`) would distinguish absent from literal-null but adds magic. v1 picks the SQL precedent. If real consumer pain emerges, swap to sentinel with a documented flag. |
| D5 | `groupBy` accepts scalar JSONPaths only. Anything containing `[` → 400 `INVALID_GROUP_BY_PATH`. Bracket-quoted scalar (`$.['variantId']`) is accepted iff it normalizes to a scalar dotted form during validation; otherwise rejected. Array projection (`[*]`, `[N]`, `[?(...)]`) is always rejected. | Array fan-out is a follow-up; the driving cases (inventory, ABC/Pareto) are scalar. Bracket-quoted property access is normalized to dotted form by the existing JSONPath conversion. |
| D6 | Add two new SPI capabilities: `Iterable` (streaming) and `GroupedAggregator` (native GROUP BY pushdown). Both optional via type assertion. Backend implementing neither → 501 `NOT_IMPLEMENTED_BY_BACKEND`. | Streaming `Iterator` is the honest primitive for the streaming-tally path; cassandra needs it because CQL can't `GROUP BY` arbitrary JSONPaths. The "could later migrate /search off slurp-all" framing is removed from the justification — that migration, if it ever happens, is its own design and shouldn't constrain this SPI shape. |
| D7 | Path: `POST /api/entity/stats/{entityName}/{modelVersion}/query`. | More RESTful than the `/entity/stats/query/{name}/{ver}` shape the issue suggested — model is the resource, `query` is the action. |
| D8 | Aggregations Tier 1+2 in v1: `sum`, `avg`, `min`, `max`, `stdev`. Sample stdev (n−1 denominator); v1 op name is `stdev`. `stdev_pop` reserved for the future-extension PR. Mode and median deferred to a separate follow-up issue with its own design pass. | Tier 1+2 marginal cost is ~30–40% on top of count-only; mode/median introduce unbounded intermediate state and asymmetric backend support (postgres-only natively) that warrant their own contract. |
| D9 | Stdev numerical contract: use **Welford's online algorithm** on the streaming-tally path. The catastrophic-cancellation single-pass SQL formula (`√((sum_sq − sum²/n)/(n−1))`) is **not** used. Postgres pushes `STDDEV_SAMP` natively (numerically stable). Sqlite does NOT push stdev down (no native function and the single-pass formula is unsafe for low-variance/high-mean data such as monetary valuations); sqlite stdev requests fall through to the streaming-tally path via `Iterable`. | Valuation rollups are the driving use case; the cancellation regime (variance ≪ mean²) is exactly the case the catastrophic formula breaks down on. Welford costs one extra Go pass and ~30 LOC of textbook code. |
| D10 | Stdev pushdown asymmetry across backends is handled by `ErrAggregationNotPushdownable`: a `GroupedAggregator` implementation may decline a specific request, signalling the service to fall through to the streaming-tally path. | Keeps the SPI capability check coarse-grained while allowing fine-grained safety opt-outs per request shape. |
| D11 | In-tx grouped-stats is **supported** via the streaming-tally path, respecting `tx.SnapshotTime` / `VisibilityFilter`. The pushdown path is skipped when `spi.GetTransaction(ctx) != nil`. No new error code. | Mirrors the actual `/search/*` precedent (`internal/domain/search/service.go:118-144`): in-tx silently falls back to the in-process path. Stats endpoints inherit the same contract. |
| D12 | `groupKey` in responses is an **ordered array of `{path, value}` pairs**, not a map keyed on JSONPath strings. | OpenAPI-typeable cleanly, friendly for typed clients, preserves request `groupBy` order, no magic in object keys. Slight wire overhead is acceptable for a small-payload endpoint. |
| D13 | Target v0.8.0, not v0.8.1. cyoda-go-spi v0.8.0 tag has not been published yet; we can fold the additive SPI changes alongside the tx-state sentinels (#16) under one tag. | v0.8.0 is not feature-frozen (12 issues open). Adding additive SPI changes pre-tag does not breach immutability. |

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

- **`groupBy`** (required, 1..N): each entry is the reserved token `"state"` or a scalar JSONPath (`$.variantId`, `$.address.country`). Multiple dimensions return cartesian buckets that actually occur. Order in the request determines order in the response's `groupKey` array.
- **`condition`** (optional): the existing search `Condition` DSL (`simple` / `lifecycle` / `group` / `function`). Reused verbatim — same parser, same depth cap, same operator validator. Omitted ⇒ match-all.
- **`aggregations`** (optional, 0..N): per entry: `op` ∈ {`sum`, `avg`, `min`, `max`, `stdev`}, `field` scalar JSONPath, optional `as` alias. When `as` is omitted, server synthesizes `<op>_<field>` (e.g. `sum_$.costPrice`). Server **dedupes identical `(op, field)` pairs** silently (the second of two identical entries is dropped). Aliases that collide on **distinct** `(op, field)` pairs → 400 `DUPLICATE_AGGREGATION_ALIAS`.
- **`pointInTime`** (optional RFC 3339): historical snapshot; default = now. Same semantics as existing stats and `GetAsAt`.
- **`limit`** (optional positive int): top-N by `(count desc, groupKey lex)`. Default = unlimited (within `CYODA_STATS_GROUP_MAX`).

### Response 200 application/json

```json
[
  {
    "groupKey":     [
      { "path": "$.variantId", "value": "1111" },
      { "path": "state",       "value": "available" }
    ],
    "count":        812,
    "aggregations": { "totalCost": 41200.00, "avg_$.costPrice": 50.74, "stdev_$.costPrice": 18.42 }
  },
  {
    "groupKey":     [
      { "path": "$.variantId", "value": "1111" },
      { "path": "state",       "value": "allocated" }
    ],
    "count":        37,
    "aggregations": { "totalCost": 1880.00, "avg_$.costPrice": 50.81, "stdev_$.costPrice": 17.99 }
  },
  {
    "groupKey":     [
      { "path": "$.variantId", "value": null },
      { "path": "state",       "value": "available" }
    ],
    "count":        3,
    "aggregations": { "totalCost": null, "avg_$.costPrice": null, "stdev_$.costPrice": null }
  }
]
```

- `groupKey`: ordered array of `{path, value}` entries. Order matches the request's `groupBy`. `value` is JSON-typed (string for scalar paths and `"state"`, JSON `null` for missing or literal-null).
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
| 400 | `INVALID_GROUP_BY_PATH` | groupBy entry empty, or contains array projection (`[*]`, `[N]`, `[?(...)]`) |
| 400 | `DUPLICATE_GROUP_BY` | duplicate entries after normalization |
| 400 | `INVALID_AGGREGATION_OP` | unknown `op` |
| 400 | `INVALID_AGGREGATION_FIELD` | aggregation `field` empty or contains array projection |
| 400 | `DUPLICATE_AGGREGATION_ALIAS` | two aliases collide on **distinct** `(op, field)` pairs (identical pairs are silently deduped) |
| 400 | `INVALID_OPERATOR` / `INVALID_CONDITION` / depth-exceeded | propagated from `predicate.ParseCondition` + `search.ValidateCondition` |
| 400 | `INVALID_POINT_IN_TIME` | unparseable RFC 3339 |
| 400 | `INVALID_LIMIT` | non-positive int |
| 401 | (standard) | missing/invalid Bearer |
| 413 | (standard) | request body > 10 MiB |
| 422 | `GROUP_CARDINALITY_EXCEEDED` | result buckets exceed `CYODA_STATS_GROUP_MAX`; body includes the ceiling. 422 because the request was well-formed but the result is unprocessable under the configured safety bound. |
| 501 | `NOT_IMPLEMENTED_BY_BACKEND` | backend implements neither `Iterable` nor `GroupedAggregator` |
| 500 | (with ticket UUID) | internal/driver errors; generic message per project policy |

In-transaction calls are **not** an error — they route through the streaming-tally path via `Iterable`, respecting `tx.SnapshotTime` / `VisibilityFilter`. This matches the existing `/search/*` precedent.

## 4. Service-layer dispatch

Single decision point at the top of `entity.Service.QueryGroupedStats`:

```go
// 1. Native pushdown when available, condition pushdownable, and not in-tx.
//    (In-tx skips pushdown — matches /search/* precedent; buffered tx writes
//    are visible only through the iterator's VisibilityFilter.)
if ga, ok := store.(spi.GroupedAggregator); ok && spi.GetTransaction(ctx) == nil {
    flt, err := filter_translate.ConditionToFilter(req.Condition)
    if err == nil {
        out, err := ga.GroupedAggregate(ctx, modelRef, req.GroupBy, flt, opts)
        if err == nil {
            return s.postProcess(out, req), nil
        }
        if !errors.Is(err, spi.ErrAggregationNotPushdownable) {
            return nil, err
        }
        // plugin opted out of pushdown for this request shape; fall through
    }
}

// 2. Streaming fallback. Used for non-pushdownable conditions, in-tx calls,
//    backends without GroupedAggregator, and any aggregation the plugin
//    declined to push.
if it, ok := store.(spi.Iterable); ok {
    return s.tallyStreaming(ctx, it, req)
}

// 3. 501
return nil, ErrBackendNotSupported
```

`tallyStreaming` uses Welford's online algorithm for stdev (numerically stable across the variance regimes the SIOMS valuation use case lives in):

```go
iter, err := it.Iterate(ctx, modelRef, spi.IterateOptions{PointInTime: req.PointInTime})
if err != nil { return nil, err }
defer iter.Close()

acc := newAccumulators(req)  // per-bucket: count, sum, min, max, Welford {mean, m2}
for iter.Next() {
    e := iter.Entity()
    if req.Condition != nil && !match.Match(req.Condition, e.Data, e.Meta) {
        continue
    }
    k, err := buildGroupKey(req.GroupBy, e)
    if err != nil { return nil, err }
    if !acc.has(k) && acc.len() > maxGroupCardinality {
        return nil, ErrGroupCardinalityExceeded
    }
    acc.observe(k, e)  // skips non-numeric samples per aggregation
}
if err := iter.Err(); err != nil { return nil, err }
return s.postProcess(acc.materialize(), req), nil
```

Welford recurrence per numeric sample x in a bucket:

```
n      += 1
delta   = x - mean
mean   += delta / n
delta2  = x - mean                 // post-update
m2     += delta * delta2
// stdev_samp = sqrt(m2 / (n - 1))  (when n >= 2; else null)
```

Cardinality check on `acc.len() > maxGroupCardinality` (strict `>`) matches the SQL backends' `LIMIT MaxBuckets + 1` overflow detection.

Post-processing (sort by `(count desc, groupKey lex)`, apply `Limit`) is shared by both paths so observable ordering is backend-independent and doesn't leak SQL-planner choices.

## 5. SPI delta (cyoda-go-spi v0.8.0)

Additive. No breaking changes. Lands on `cyoda-go-spi` main on top of `c301c0e` (tx-state sentinels, #16) and is included in the v0.8.0 SPI tag.

```go
// Streaming iteration over a model's entities.
// Implementations MUST NOT hold a global write-blocking lock for the lifetime
// of the iterator. Snapshot-then-iterate (memory) or cursor-based (sql.Rows,
// pgx.Rows, gocql.Iter) are the accepted shapes.
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
// A plugin may decline a specific request shape by returning
// ErrAggregationNotPushdownable; the service will then fall through to the
// streaming-tally path via Iterable.
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
    GroupExprDataPath                  // scalar JSONPath in Path
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

type GroupKeyEntry struct {
    Path  string
    Value any // string for scalar/state, nil for missing/literal-null
}

type GroupedAggregateBucket struct {
    GroupKey     []GroupKeyEntry     // ordered, matches request groupBy order
    Count        int64
    Aggregations map[string]any      // alias → float64 or nil
}

// Sentinels.
var ErrGroupCardinalityExceeded   = errors.New("group cardinality exceeded ceiling")
var ErrAggregationNotPushdownable = errors.New("aggregation not pushdownable on this backend")
```

Forward-compat: `having`, mode/median, and `stdev_pop` land as additive fields and enum values without a major bump.

## 6. Per-plugin implementations

### Memory (`plugins/memory`)

Implements **both** `Iterable` and `GroupedAggregator`.

- `Iterate`: **snapshot-then-iterate**. Under `entityMu.RLock`, capture a slice of version-pointer snapshots scoped to the tenant + model + (optional) `pointInTime`. Release the RLock. Iterator walks the snapshot lock-free, applying deletion-skip and visibility filter per entry. This avoids the writer-starvation risk of holding RLock across slow consumer work. Memory cost of the snapshot is one pointer per matching entity — proportional to model size, not entity payload size.
- `GroupedAggregate`: takes the same snapshot, applies the filter via `match.Match`, extracts group keys via `gjson.GetBytes(entity.Data, path)` (already imported in `match.go`), accumulates per-bucket via Welford. Enforces `MaxBuckets` and returns `ErrGroupCardinalityExceeded` on overflow.

### SQLite (`plugins/sqlite`)

Implements **both**.

- `Iterate`: wraps `sql.Rows` with `rows.Next()`. PointInTime path uses the existing `entity_versions` snapshot query (the inner join on `MAX(version) WHERE submit_time <= ?`). `Close` calls `rows.Close()`.
- `GroupedAggregate`: pushes count + sum + avg + min + max natively. **Returns `ErrAggregationNotPushdownable` whenever the request includes any `stdev`** — sqlite has no native `STDDEV` function and the single-pass `√((sum_sq − sum²/n)/(n−1))` formula loses precision catastrophically on low-variance/high-mean data (the valuation use case). Stdev requests on sqlite fall through to streaming-tally with Welford via the service layer's dispatch.

  SQL template (no stdev):

  ```sql
  SELECT
      json_extract(data, '$.variantId')           AS gk_0,
      json_extract(meta, '$.state')               AS gk_1,
      COUNT(*)                                    AS cnt,
      SUM(CAST(json_extract(data, '$.costPrice') AS REAL)) AS agg_totalCost,
      AVG(CAST(json_extract(data, '$.costPrice') AS REAL)) AS agg_avg_x
  FROM entities  -- or entity_versions inner-join for pointInTime
  WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND NOT deleted
    AND <pushed-down filter>
  GROUP BY json_extract(data, '$.variantId'), json_extract(meta, '$.state')
  LIMIT <MaxBuckets + 1>
  ```

  `GROUP BY` references the full extractor expressions (not aliases) for portability across SQLite query-plan shapes (CTEs, subqueries).

  Reuses the existing `planQuery`/`dissect` planner from `query_planner.go` for the filter. If `planQuery` produces a residual filter (non-pushdownable condition), the service layer falls through to `Iterate` + Go-tally — no grouped-residual-eval path inside the plugin.

  `MaxBuckets` enforced via `LIMIT MaxBuckets+1`; rowcount of `MaxBuckets+1` → `ErrGroupCardinalityExceeded`.

  State group-key normalization: `json_extract(meta, '$.state')` returns SQL `NULL` for missing — surfaces as JSON `null` in the response, consistent with D4. We do not collapse to empty string.

### Postgres (`plugins/postgres`)

Implements **both**. Pushes the full Tier 1+2 set natively.

- `Iterate`: wraps `pgx.Rows`. PointInTime uses the bi-temporal `entity_versions` table with `valid_time <= $N AND transaction_time <= CURRENT_TIMESTAMP` (existing pattern from `GetAsAt`).

- `GroupedAggregate` SQL uses JSONB operators and `STDDEV_SAMP`:

  ```sql
  SELECT
      doc->>'variantId'                             AS gk_0,
      doc->'_meta'->>'state'                        AS gk_1,
      COUNT(*)                                      AS cnt,
      SUM(cyoda_try_float8(doc->>'costPrice'))      AS agg_totalCost,
      AVG(cyoda_try_float8(doc->>'costPrice'))      AS agg_avg_x,
      STDDEV_SAMP(cyoda_try_float8(doc->>'costPrice')) AS agg_stdev_x
  FROM entities  -- or entity_versions for pointInTime
  WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted
    AND <pushed-down filter>
  GROUP BY doc->>'variantId', doc->'_meta'->>'state'
  LIMIT <MaxBuckets + 1>
  ```

- **`cyoda_try_float8(text) RETURNS float8` immutable helper** (created by migration) is the central numeric-coercion contract. Behavior:

  | Input | Output |
  |---|---|
  | SQL `NULL` (key missing from JSONB) | `NULL` |
  | `''` (empty string) | `NULL` |
  | `'abc'` (non-numeric) | `NULL` |
  | `'NaN'`, `'+NaN'`, `'-NaN'` | `NULL` (not Postgres's NaN float — avoids corrupting aggregates) |
  | `'Infinity'`, `'-Infinity'`, `'+Infinity'`, `'inf'` | `NULL` (same reasoning) |
  | `'1e308'`, `'-1.5'`, `'42'`, etc. | parsed `float8` |
  | JSON `true` / `false` / array / object (any non-string JSON value coerced through `->>`) | exits as text representation; `cyoda_try_float8` returns `NULL` (no boolean→1.0 surprise) |

  Implementation is a `regexp_match`-gated `::float8` cast inside a `BEGIN ... EXCEPTION WHEN invalid_text_representation` block, marked `IMMUTABLE PARALLEL SAFE`. Parity tests cover every row of the table above.

- `MaxBuckets` enforced via `LIMIT MaxBuckets+1`; overflow → `ErrGroupCardinalityExceeded`.

- Index guidance documented in the help topic: for hot grouping dimensions, callers add `CREATE INDEX entities_variantid_idx ON entities ((doc->>'variantId'))`. The base migration is **not** extended in this PR; expression-index management per data field is out of scope.

### Cassandra (`cyoda-go-cassandra`, commercial backend)

Implements **`Iterable` only** in v1. Does **not** implement `GroupedAggregator`.

- Rationale: CQL's `GROUP BY` works only over partition/clustering keys. For arbitrary JSONPath group-by, the plugin would itself stream and tally in Go — the same code the service-layer fallback already runs.
- `Iterate` is shaped after the existing `GetAll` fan-out (`entity_store.go:907-959`), not `CountByState`. `CountByState` reads the `$._meta.state` lifecycle index and isn't useful for grouping on arbitrary entity data. The honest description: `Iterate` fans out across shards on `entity_by_model` to collect IDs, point-reads each entity through the visibility filter, and yields one entity at a time without materializing the result slice. This is streaming GetAll — better than slurp-all (one row in flight at a time) but not faster than slurp-all in absolute terms; the win is bounded memory, not bounded latency.
- Each entity ID lives in exactly one shard; there is no cross-shard winner-map. Per-shard streams concatenate, with per-entity version resolution happening inside each shard's row stream via the visibility filter.
- Out of scope follow-up: a state-only fast-path can later route `groupBy: ["state"]` directly to the lifecycle-index reader that `CountByState` already uses. Not v1.
- The cassandra PR is an independent timeline. Adding `GroupedAggregator` to the SPI doesn't break any plugin — it's a type-assertion contract with default-to-501 behavior, so cassandra can pin v0.8.0 SPI without implementing it. Until the cassandra `Iterable` PR lands, cassandra-backed deployments running cyoda-go v0.8.0 return 501 on the grouped-stats endpoint. Honest graceful degradation.

## 7. Conformance, OpenAPI, help, e2e

### Cross-backend conformance (`internal/e2e/parity/registry.go`)

Per the cross-plugin policy, parity tests added here are picked up by every backend including cassandra. New parity cases:

| Case | Cover |
|---|---|
| single data-field group-by | basic happy path |
| `state` group-by | parity with existing `/entity/stats/states` |
| multi-dim `$.variantId × state` | cartesian buckets, response ordering |
| with vs without `condition` | reuse of search DSL |
| with `pointInTime` | historical snapshot |
| in-tx call routes through streaming-tally | matches `/search/*` precedent; no error |
| sum / avg / min / max | Tier 1 aggregations |
| stdev — wide dynamic range (8+ orders of magnitude) | nominal numerical sanity |
| **stdev — low-variance / high-mean (monetary valuation regime)** | the actual stress case; Welford produces correct stdev within float64 ulp tolerance |
| non-numeric values mixed in | silently skipped (per `cyoda_try_float8` table for postgres; per Welford skip rule for streaming) |
| all-null aggregation field | result is JSON `null` |
| count-only request (no `aggregations` field) | response omits `aggregations` |
| explicit `aggregations: []` | equivalent to count-only; response omits `aggregations` |
| OpenAPI schema vs DTO check | DTO field `omitempty` and OpenAPI schema agree on `aggregations` absence |
| groupBy contains `[*]` / `[N]` / `[?(...)]` | 400 `INVALID_GROUP_BY_PATH` |
| aggregation `field` contains `[*]` / `[N]` / `[?(...)]` | 400 `INVALID_AGGREGATION_FIELD` |
| duplicate groupBy | 400 `DUPLICATE_GROUP_BY` |
| **identical `(op, field)` aggregation pair** (no `as`) | silently deduped, single entry in response |
| **distinct `(op, field)` pair colliding on explicit `as` alias** | 400 `DUPLICATE_AGGREGATION_ALIAS` |
| non-pushdownable condition | service falls through to streaming-tally; identical bucket set |
| sqlite stdev request | plugin returns `ErrAggregationNotPushdownable`; service falls through to streaming-tally |
| cardinality ceiling exceeded | 422 `GROUP_CARDINALITY_EXCEEDED` |
| backend with neither capability | 501 `NOT_IMPLEMENTED_BY_BACKEND` (only exercised by a mock test-backend) |
| **tenant isolation** | tenant A's request on a model also-owned-by tenant B sees only tenant A buckets |
| **SQL-injection surface — groupBy path** | path containing `'`, `;`, `--`, `\\`, `\n`, NUL → 400 `INVALID_GROUP_BY_PATH` (rides on existing path validator); explicit case in the matrix |
| **SQL-injection surface — aggregation field** | same surface, same validator, same 400 |
| **concurrent writes + grouped-stats reads (memory plugin)** | snapshot-then-iterate contract verified: stats result is internally consistent against the snapshot taken at request entry; writers do not block on the iterator |

### E2E (`internal/e2e/grouped_stats_test.go`)

One e2e test hitting the full HTTP stack with the postgres testcontainer. Doesn't enumerate every parity case — that's the parity suite's job. Proves wiring: handler → service → SPI → postgres GROUP BY → JSON response.

### OpenAPI (`api/openapi.yaml`)

New path `POST /api/entity/stats/{entityName}/{modelVersion}/query`. Per `//go:embed`-based embed (project policy), the spec stays as-is in source. New schemas:

- `GroupedStatsRequest`
- `GroupedStatsBucket`
- `GroupKeyEntry` (`{path: string, value: any}`)
- `GroupExpr` (enum-or-oneOf: `"state"` token vs scalar JSONPath)
- `AggregationExpr`
- `AggregateOp` enum

The `aggregations` field on `GroupedStatsBucket` uses `nullable: false` and is marked `omitempty` on the Go DTO so the field is omitted when no aggregations were requested. Parity test verifies the absent-vs-empty-object distinction matches the schema.

No issue IDs in user-facing schema descriptions per project policy.

### `cyoda help` (`cmd/cyoda/help/content/`)

Updates to the search/stats help topic:

- Request and response examples (count-only, with aggregations, with pointInTime, multi-dim).
- Cardinality ceiling and its env var.
- JSONPath restriction (scalar only) and rationale.
- Aggregation operators, the sample-stdev semantics, the `stdev_pop` reservation.
- Numeric coercion contract for the postgres backend (the `cyoda_try_float8` table).
- Postgres expression-index guidance for hot grouping dimensions.
- In-tx behavior (routes through streaming-tally; tx-visible writes counted via VisibilityFilter).

No `#299` references in help content per memory `feedback_no_issue_ids_in_code`.

### Config (Gate 4)

`CYODA_STATS_GROUP_MAX` env var (default 10000) added to:

- `cmd/cyoda/help/content/config/*.md` — the relevant topic.
- `README.md` configuration reference.
- `DefaultConfig()` in code.

All three updated together.

A sensible-default sanity check: the SIOMS driving case has K variants × S states ≈ a few hundred to a few thousand buckets for the typical inventory screen. 10000 is comfortably above that. Tenants approaching the ceiling should narrow their condition or reduce groupBy dimensions; the 422 surfaces this explicitly.

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
| `mode` / `median` | New `AggregateOp` enum values. Each backend declares support via `ErrAggregationNotPushdownable`; unsupported ops on a backend that *also* can't fall back to streaming-tally (e.g. unbounded distinct-value cardinality per group) return 422 `AGGREGATION_NOT_SUPPORTED_BY_BACKEND`. New `CYODA_STATS_AGG_INTERMEDIATE_MAX` knob caps per-bucket distinct-value cardinality. Worth its own design pass (memory contract, postgres-only pushdown, sketch alternatives). |
| `stdev_pop` / population stdev | New `AggregateOp` value (`stdev_pop`). v1 keeps `stdev` as sample (n−1). |
| Array-projection in `groupBy` (`$.tags[*]`) | New validation accepts `[*]`; `GroupExpr.Kind` gains `DataPathArray`. Per-backend fan-out semantics and same-entity-same-value dedupe policy specified in that spec. |
| Numeric paging | Optional `offset` paired with `limit` once the sort key is stable. |
| Distinguishing absent from literal-null in `groupKey` | Optional `nullPolicy: "merge" | "distinguish"` request field; default stays `merge` for backward compatibility. |

## 9. Release sequencing

Coordinated change across three repos. Sequencing per project policy.

1. **cyoda-go-spi PR** — additive: `Iterable`, `Iterator`, `IterateOptions`, `GroupedAggregator`, `GroupExpr`, `AggregateExpr`, `AggregateOp`, `GroupedAggregationsOptions`, `GroupKeyEntry`, `GroupedAggregateBucket`, `ErrGroupCardinalityExceeded`, `ErrAggregationNotPushdownable`. Lands on cyoda-go-spi `main` on top of `c301c0e`. No tag yet — folds into the eventual v0.8.0 SPI tag.
2. **cyoda-go-spi v0.8.0 tag** — published once the v0.8.0 SPI scope (tx-state sentinels + this change) is settled. Adding `GroupedAggregator` to the SPI doesn't break any plugin — it's a type-assertion contract with default-to-501 behavior. Cassandra (and any out-of-tree plugin) can pin v0.8.0 SPI without implementing either new interface.
3. **cyoda-go PR series on `release/v0.8.0`** — each in its own commit/PR:
   - SPI pin bump (one isolated commit).
   - Handler + DTO + validation.
   - Service-layer dispatch + accumulators (Welford) + post-processing.
   - Plugin implementations (memory, sqlite, postgres), `cyoda_try_float8` migration, conformance and parity tests.
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
- **Streaming `/search/*` itself** — `Iterable` is for grouped-stats; any future streaming-search migration is a separate design with its own SPI shape decisions.
- **Distinguishing absent from literal-null in `groupKey`** — out of scope for v1 per D4; `nullPolicy` is the forward-compat hook.
- **Sqlite-native stdev pushdown** — out of scope per D9; sqlite stdev requests route through streaming-tally with Welford.

## 11. Acceptance

Mirrors the issue's acceptance criteria, expanded for the v1 scope:

- `POST /api/entity/stats/{entityName}/{modelVersion}/query` returns grouped counts and Tier 1+2 aggregations for:
  - single data-field group-by;
  - `state` group-by (parity with `/entity/stats/states`);
  - multi-dimension `variantId × state`;
  - with and without a `condition`;
  - with `pointInTime`;
  - in-tx (routes through streaming-tally, no error).
- No entity envelopes ever materialized server-side as a full slice: postgres + sqlite push GROUP BY into SQL; memory snapshots version pointers and walks them; cassandra streams per-shard with one entity in flight at a time. No GetAll-then-tally code path inside the service or any plugin.
- Auth, tenant scoping, Condition validation, and error model identical to `/search/*` where shared. Tenant isolation verified by parity test.
- `CYODA_STATS_GROUP_MAX` ceiling enforced consistently across all backends (`LIMIT MaxBuckets+1` in SQL, `> ceiling` check in streaming); exceeded returns 422 with the ceiling value.
- Numerical contracts pinned: postgres `cyoda_try_float8` table verified row-by-row in parity; streaming-tally stdev passes the low-variance/high-mean stress case within float64 ulp tolerance; sqlite stdev pushdown declined deterministically, falls through cleanly.
- Backend without either capability returns 501.
- `cyoda help` search/stats topic updated; OpenAPI spec updated; CONFIG env var documented; COMPATIBILITY.md updated alongside the SPI pin bump.
- Conformance parity tests cover the case matrix in §7 — including tenant isolation, SQL-injection surface on groupBy and aggregation `field`, the dedupe-vs-collision policy, the in-tx routing, and the snapshot-vs-writer test for memory. Race detector clean before PR per memory `feedback_race_testing_discipline`.
