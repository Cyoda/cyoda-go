# Grouped entity statistics query — design

| Field | Value |
|---|---|
| Issue | [#299](https://github.com/Cyoda-platform/cyoda-go/issues/299) |
| Target milestone | v0.8.0 (cyoda-go), v0.8.0 (cyoda-go-spi) |
| Spec date | 2026-06-14 |
| Review iterations | 2 (independent fresh-context review + user pushback on iterator filter-awareness) |
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

Captured during brainstorming and two review iterations. Each entry is the option chosen and the rationale in one line.

| # | Decision | Rationale |
|---|---|---|
| D1 | Response shape: top-level `count` always emitted; `aggregations` map sibling, omitted when none requested. | Forward-compatible with sum/avg/etc.; doesn't force nesting on count-only callers. |
| D2 | No server-side cap on entities scanned; cap is on **result group cardinality** (`CYODA_STATS_GROUP_MAX`, default 10000). Exceeded → 422 `GROUP_CARDINALITY_EXCEEDED`. | Implementation streams/pushes-down; intrinsic cost dimension is buckets, not entities. 422 (semantic, unprocessable) is more correct than 400 — the request itself is well-formed. |
| D3 | Optional `limit` request field. When set, server returns top-N by `(count desc, groupKey lex)`. Default = unlimited. | Display convenience; orthogonal to safety bound. |
| D4 | Missing JSONPath value → JSON `null` bucket key. Conflates with values that are literally JSON `null` (matches SQL `IS NULL` / `IS NOT DISTINCT FROM NULL` semantics). Deliberate v1 simplification. | A sentinel string (`"__missing__"`) would distinguish absent from literal-null but adds magic. v1 picks the SQL precedent. If real consumer pain emerges, swap to sentinel with a documented flag. |
| D5 | `groupBy` accepts scalar JSONPaths only. Anything containing `[` (after bracket-quoted-property normalization) → 400 `INVALID_GROUP_BY_PATH`. | Array fan-out is a follow-up; the driving cases (inventory, ABC/Pareto) are scalar. Bracket-quoted property access (`$.['variantId']`) is normalized to dotted form during validation. |
| D6 | Add two new SPI capabilities: `Iterable` (filter-aware streaming) and `GroupedAggregator` (native GROUP BY pushdown). Both optional via type assertion. Backend implementing neither → 501 `NOT_IMPLEMENTED_BY_BACKEND`. | A streaming primitive is the honest cassandra implementation (no native CQL `GROUP BY` on JSONPath). It's filter-aware so the fallback path doesn't full-scan when the condition is narrowing. |
| D7 | Path: `POST /api/entity/stats/{entityName}/{modelVersion}/query`. | More RESTful than the `/entity/stats/query/{name}/{ver}` shape the issue suggested — model is the resource, `query` is the action. |
| D8 | Aggregations Tier 1+2 in v1: `sum`, `avg`, `min`, `max`, `stdev`. Sample stdev (n−1 denominator); v1 op name is `stdev`. `stdev_pop` reserved for the future-extension PR. Mode and median deferred to a separate follow-up issue. | Tier 1+2 marginal cost is ~30–40% on top of count-only; mode/median introduce unbounded intermediate state and asymmetric backend support that warrant their own contract. |
| D9 | Stdev numerical contract: **Welford's online algorithm** on the streaming-tally path. The single-pass `√((sum_sq − sum²/n)/(n−1))` formula is not used — it suffers catastrophic cancellation for low-variance/high-mean data (the valuation regime). Postgres pushes `STDDEV_SAMP` natively. Sqlite does NOT push stdev down; sqlite stdev requests fall through to the streaming-tally path via `Iterable`. | Valuation rollups are the driving use case. Welford costs one extra Go pass and ~30 LOC of textbook code. |
| D10 | Stdev pushdown asymmetry across backends is handled by `ErrAggregationNotPushdownable`: a `GroupedAggregator` implementation may decline a specific request shape, signalling the service to fall through to the streaming-tally path. | Keeps the SPI capability check coarse-grained while allowing fine-grained safety opt-outs per request shape. |
| D11 | In-tx grouped-stats is **supported** via the streaming-tally path, respecting `tx.SnapshotTime` / `VisibilityFilter`. The pushdown path is skipped when `spi.GetTransaction(ctx) != nil`. No new error code. | Mirrors the actual `/search/*` precedent (`internal/domain/search/service.go:118-144`): in-tx silently falls back to the in-process path. Stats endpoints inherit the same contract. |
| D12 | `groupKey` in responses is an **ordered array of `{path, value}` pairs**, not a map keyed on JSONPath strings. | OpenAPI-typeable cleanly, friendly for typed clients, preserves request `groupBy` order. |
| D13 | Target v0.8.0, not v0.8.1. cyoda-go-spi v0.8.0 tag has not been published yet; the additive SPI changes fold alongside tx-state sentinels (#16) under one tag. | v0.8.0 is not feature-frozen (12 issues open). Adding additive SPI changes pre-tag does not breach immutability. |
| D14 | **`Iterable.Iterate(model, filter, opts)` takes a Filter parameter.** The iterator yields entities matching the filter. Plugins push what they can to storage and apply any residual inside `Next()` before yielding. The Filter type is shared with `GroupedAggregator` and (existing) `Searcher.Search`. | Without a filter parameter, the streaming-tally path scans the whole model regardless of how narrow the condition is. Cassandra (which always falls through to streaming) and in-tx postgres would full-scan; the cost ratio against a narrow condition is 10,000×+. Filter-aware Iterate makes the fallback path scale with `matching`, not `total`. |
| D15 | When `ConditionToFilter` cannot represent the condition (function conditions, future operators not yet translated), the service passes a `nil` filter to `Iterate` and re-applies `match.Match` per yielded entity. Otherwise the iterator's yielded set is authoritative and the service does not re-check. | One source of truth per pluggable surface: when the plugin can be told the filter, it owns the filter. When the SPI Filter type can't carry the semantics, the service falls back to the existing in-process matcher over an unfiltered stream. |
| D16 | Postgres gets a new Filter→SQL translation layer as part of this change. It uses JSONB operators (`->>`, `->`) for path extraction and follows sqlite's greedy-AND / conservative-OR `planQuery` shape. Scope: support the same operator set sqlite already pushes. | Postgres has no `Searcher` today; without this layer, postgres `Iterate` would be filter-blind and the streaming-tally path on postgres would also full-scan (e.g. in-tx grouped-stats). The translator is reusable substrate for a future postgres `Searcher` PR; that work is not in scope here. |

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

- **`groupBy`** (required, 1..N): each entry is the reserved token `"state"` or a scalar JSONPath. Multiple dimensions return cartesian buckets that actually occur. Order in the request determines order in the response's `groupKey` array.
- **`condition`** (optional): the existing search `Condition` DSL (`simple` / `lifecycle` / `group` / `function`). Reused verbatim. Omitted ⇒ match-all.
- **`aggregations`** (optional, 0..N): per entry: `op` ∈ {`sum`, `avg`, `min`, `max`, `stdev`}, `field` scalar JSONPath, optional `as` alias. When `as` is omitted, server synthesizes `<op>_<field>`. Server **dedupes identical `(op, field)` pairs** silently. Aliases that collide on **distinct** `(op, field)` pairs → 400 `DUPLICATE_AGGREGATION_ALIAS`.
- **`pointInTime`** (optional RFC 3339): historical snapshot; default = now.
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
      { "path": "$.variantId", "value": null },
      { "path": "state",       "value": "available" }
    ],
    "count":        3,
    "aggregations": { "totalCost": null, "avg_$.costPrice": null, "stdev_$.costPrice": null }
  }
]
```

- `groupKey`: ordered array of `{path, value}` entries. Order matches request `groupBy`. `value` is JSON-typed (string for scalar paths and `"state"`, JSON `null` for missing or literal-null).
- `count`: int64, always emitted.
- `aggregations`: object keyed by alias. Omitted entirely when none requested. Per-alias value is JSON `null` when the bucket had zero numeric samples (non-numeric and absent values are silently skipped).
- Zero-count buckets are not emitted.

### Errors

| Status | Code | Trigger |
|---|---|---|
| 400 | `MALFORMED_REQUEST` | JSON parse failed |
| 400 | `UNKNOWN_MODEL` | path `{entityName}/{modelVersion}` does not resolve for this tenant |
| 400 | `MISSING_GROUP_BY` | `groupBy` empty or missing |
| 400 | `INVALID_GROUP_BY_PATH` | empty entry, or array projection (`[*]`, `[N]`, `[?(...)]`) |
| 400 | `DUPLICATE_GROUP_BY` | duplicate entries after normalization |
| 400 | `INVALID_AGGREGATION_OP` | unknown `op` |
| 400 | `INVALID_AGGREGATION_FIELD` | aggregation `field` empty or contains array projection |
| 400 | `DUPLICATE_AGGREGATION_ALIAS` | two aliases collide on **distinct** `(op, field)` pairs (identical pairs silently deduped) |
| 400 | `INVALID_OPERATOR` / `INVALID_CONDITION` / depth-exceeded | propagated from `predicate.ParseCondition` + `search.ValidateCondition` |
| 400 | `INVALID_POINT_IN_TIME` | unparseable RFC 3339 |
| 400 | `INVALID_LIMIT` | non-positive int |
| 401 | (standard) | missing/invalid Bearer |
| 413 | (standard) | request body > 10 MiB |
| 422 | `GROUP_CARDINALITY_EXCEEDED` | result buckets exceed `CYODA_STATS_GROUP_MAX`; body includes the ceiling |
| 501 | `NOT_IMPLEMENTED_BY_BACKEND` | backend implements neither `Iterable` nor `GroupedAggregator` |
| 500 | (with ticket UUID) | internal/driver errors; generic message per project policy |

In-transaction calls are **not** an error — they route through the streaming-tally path via `Iterable`, respecting `tx.SnapshotTime` / `VisibilityFilter`. This matches the existing `/search/*` precedent.

## 4. Service-layer dispatch

```go
// 1. Native pushdown when available, condition pushdownable, and not in-tx.
//    (In-tx skips pushdown — matches /search/* precedent.)
if ga, ok := store.(spi.GroupedAggregator); ok && spi.GetTransaction(ctx) == nil {
    if flt, err := filter_translate.ConditionToFilter(req.Condition); err == nil {
        out, err := ga.GroupedAggregate(ctx, modelRef, req.GroupBy, flt, aggOpts)
        if err == nil {
            return s.postProcess(out, req), nil
        }
        if !errors.Is(err, spi.ErrAggregationNotPushdownable) {
            return nil, err
        }
        // plugin opted out of pushdown for this shape; fall through
    }
}

// 2. Streaming fallback. Filter-aware: iterator yields entities matching
//    the (possibly-partial) filter. When ConditionToFilter fails, pass nil
//    and re-apply match.Match per yielded entity (D15).
if it, ok := store.(spi.Iterable); ok {
    flt, filterOK := filter_translate.ConditionToFilter(req.Condition), true
    if req.Condition != nil && flt == nil { filterOK = false }
    return s.tallyStreaming(ctx, it, req, flt, filterOK)
}

// 3. 501
return nil, ErrBackendNotSupported
```

`tallyStreaming`:

```go
iter, err := it.Iterate(ctx, modelRef, flt, spi.IterateOptions{PointInTime: req.PointInTime})
if err != nil { return nil, err }
defer iter.Close()

acc := newAccumulators(req)  // per-bucket: count, sum, min, max, Welford {mean, m2}
for iter.Next() {
    e := iter.Entity()
    // Iterator owns filter semantics when filterOK; service rechecks only when
    // ConditionToFilter could not represent the full condition (D15).
    if !filterOK && req.Condition != nil &&
       !match.Match(req.Condition, e.Data, e.Meta) {
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

Post-processing (sort by `(count desc, groupKey lex)`, apply `Limit`) is shared by both paths so observable ordering is backend-independent.

## 5. SPI delta (cyoda-go-spi v0.8.0)

Additive. No breaking changes. Lands on `cyoda-go-spi` main on top of `c301c0e` (tx-state sentinels, #16) and is included in the v0.8.0 SPI tag.

```go
// Streaming iteration over a model's entities matching a Filter.
// Implementations:
//   - Push pushable parts of the filter into storage (SQL WHERE, CQL index lookup).
//   - Apply any residual filter inside Next() before yielding.
// A nil filter means "yield all entities for the model" (subject to opts).
// Implementations MUST NOT hold a global write-blocking lock for the lifetime
// of the iterator. Snapshot-then-iterate (memory) or cursor-based (sql.Rows,
// pgx.Rows, gocql.Iter) are the accepted shapes.
type Iterable interface {
    Iterate(
        ctx context.Context,
        model ModelRef,
        filter Filter,                  // nil = match-all
        opts IterateOptions,
    ) (Iterator, error)
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
// May decline a specific request shape via ErrAggregationNotPushdownable.
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
    MaxBuckets   int
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

- `Iterate(model, filter, opts)`: **snapshot-then-iterate**. Under `entityMu.RLock`, capture a slice of `*entityVersion` pointers scoped to tenant + model + (optional) `pointInTime`. Release the RLock. Iterator walks the snapshot lock-free; inside `Next()` applies `match.MatchFilter(filter, e.Data, e.Meta)` (a Filter-typed version of the existing `match.Match`) before yielding. Avoids writer starvation; entity-version records are append-only and safe to read lock-free.
- `GroupedAggregate`: takes the same snapshot, applies filter via `match.MatchFilter`, extracts group keys via `gjson.GetBytes`, accumulates per-bucket via Welford. Enforces `MaxBuckets` and returns `ErrGroupCardinalityExceeded` on overflow.

For the memory backend there is no meaningful "pushdown" — filtering happens in-process either way. Filter-aware `Iterate` is structurally the same as filter-blind followed by `match.Match` in the service, but moves the work behind the SPI boundary so the iterator's contract holds across backends.

### SQLite (`plugins/sqlite`)

Implements **both**.

- `Iterate(model, filter, opts)`: reuses the existing `planQuery`/`dissect` translator (`plugins/sqlite/query_planner.go`). Pushable part of the filter → `WHERE`; residual part captured for in-iterator post-filtering. Wraps `sql.Rows` with `rows.Next()`. Inside `Next()`, if the plan has a `postFilter`, evaluate it against the scanned row before yielding; otherwise yield directly. PointInTime path uses the existing `entity_versions` snapshot query (the inner join on `MAX(version) WHERE submit_time <= ?`). `Close` calls `rows.Close()`.

- `GroupedAggregate`: pushes count + sum + avg + min + max natively. **Returns `ErrAggregationNotPushdownable` whenever the request includes any `stdev`** — sqlite has no native `STDDEV` and the single-pass formula is unsafe for valuation data. Stdev requests fall through to streaming-tally with Welford.

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

  `GROUP BY` references full extractor expressions (not aliases) for portability across query plan shapes. Residual filter (when `planQuery` produces one) is applied as post-aggregation `HAVING`? No — `HAVING` operates on group rows, not entity rows. When the filter has a residual the plugin **also** returns `ErrAggregationNotPushdownable` and falls through to streaming-tally, which can apply the residual per row.

  `MaxBuckets` enforced via `LIMIT MaxBuckets+1`; overflow → `ErrGroupCardinalityExceeded`.

  State group-key normalization: `json_extract(meta, '$.state')` returns SQL `NULL` for missing — surfaces as JSON `null` in the response, consistent with D4. We do not collapse to empty string.

### Postgres (`plugins/postgres`)

Implements **both**. **Adds a new Filter→SQL translation layer** as part of this change (D16); postgres has no `Searcher` today and therefore no existing translator.

#### Postgres Filter→SQL translator (`plugins/postgres/query_planner.go`)

Analogue of sqlite's `planQuery`/`dissect`. Same input (SPI `Filter` tree) and same output shape (`(whereClause, args, postFilter *Filter)`), with three differences:

1. **JSONB operators** for path extraction: `doc->'_meta'->>'state'` for state, `doc->>'variantId'` (and chained `->`/`->>` for nested paths) for data fields. Path translation mirrors sqlite's existing path-validation invariant (no array projection, no SQL meta-characters; rides on the existing `path_validation` package).
2. **Operator mapping:** `=`, `!=`, `<`, `<=`, `>`, `>=`, `IN`, `LIKE`, `ILIKE`, `IS NULL`, `IS NOT NULL`, `BETWEEN`. Same set sqlite already pushes. Comparison operands cast via `cyoda_try_float8` for numeric comparisons (see below); string comparisons compare `text` directly.
3. **Pushdown discipline:** greedy AND (extract pushable children; collect non-pushable as residual), conservative OR (only push if all children pushable, else whole OR is residual). Same shape as sqlite's `dissect`; ~300 LOC plus tests.

Used by both `Iterate` and `GroupedAggregate`.

#### `Iterate(model, filter, opts)`

Wraps `pgx.Rows`. Translator produces `whereClause + postFilter`. SQL:

```sql
SELECT doc
FROM entities  -- or entity_versions inner-join for pointInTime
WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted
  AND <whereClause from translator>
```

Inside `Next()`, if `postFilter != nil`, evaluate against scanned row before yielding. PointInTime uses the bi-temporal `entity_versions` table with `valid_time <= $N AND transaction_time <= CURRENT_TIMESTAMP` (existing pattern from `GetAsAt`).

#### `GroupedAggregate`

Uses the same translator. Pushes the full Tier 1+2 set natively (including `STDDEV_SAMP`, which is numerically stable on postgres):

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
  AND <whereClause from translator>
GROUP BY doc->>'variantId', doc->'_meta'->>'state'
LIMIT <MaxBuckets + 1>
```

When the translator produces a non-nil `postFilter`, `GroupedAggregate` returns `ErrAggregationNotPushdownable` (same as sqlite). The streaming-tally fallback then iterates with the same pushable-part filter and applies the residual in-iterator.

#### `cyoda_try_float8(text) RETURNS float8` immutable helper

Central numeric-coercion contract, created by migration. Behavior:

| Input | Output |
|---|---|
| SQL `NULL` (key missing from JSONB) | `NULL` |
| `''` (empty string) | `NULL` |
| `'abc'` (non-numeric) | `NULL` |
| `'NaN'`, `'+NaN'`, `'-NaN'` | `NULL` (not Postgres's NaN float — avoids corrupting aggregates) |
| `'Infinity'`, `'-Infinity'`, `'+Infinity'`, `'inf'` | `NULL` |
| `'1e308'`, `'-1.5'`, `'42'`, etc. | parsed `float8` |
| JSON `true` / `false` / array / object (via `->>` text rendering) | `NULL` |

Implementation is a `regexp_match`-gated `::float8` cast inside `BEGIN ... EXCEPTION WHEN invalid_text_representation`, marked `IMMUTABLE PARALLEL SAFE`. Parity tests cover every row of the table.

#### Index guidance

Documented in the help topic: for hot grouping dimensions, callers add `CREATE INDEX entities_variantid_idx ON entities ((doc->>'variantId'))`. The base migration is **not** extended in this PR; expression-index management per data field is out of scope.

### Cassandra (`cyoda-go-cassandra`, commercial backend)

Implements **`Iterable` only** in v1. Does **not** implement `GroupedAggregator`.

- Rationale: CQL `GROUP BY` works only on partition/clustering keys. For arbitrary JSONPath group-by, the plugin would itself stream and tally in Go — the same code the service-layer fallback already runs.

- `Iterate(model, filter, opts)`: wires the existing `search/shard_executor.go` planner output into an iterator. The planner already produces:
  - **Indexed predicates** (state, declared lifecycle index fields) → `index_string_data` scan for that field/value, yielding entity IDs that then get point-read from `entity_versions` through the visibility filter.
  - **Non-indexed predicates** → planner falls back to full-shard scan + post-filter in Go.
  
  Per-shard `gocql.Iter()` cursors yield entities one at a time. Each entity ID lives in exactly one shard; per-shard streams concatenate. The iterator's `Next()` applies any post-filter (the planner's residual) before yielding.

- `PointInTime` honored via the existing `tx.VisibilityFilter` HLC contract used by `CountByState` and the search planner.

- Cost profile of filter-aware iterate on cassandra:
  - Condition with at least one indexed predicate (state, declared lifecycle field) → reads the lifecycle index, scope of work is **`O(matching entities)`**, not `O(model size)`. This is the case for the SIOMS inventory dashboard (state-based scoping).
  - Condition with no indexed predicates → full-shard fan-out scan + post-filter. Documented in the help topic; index-declaration guidance applies (a model author adds `$._meta` lifecycle index entries for high-traffic data fields the same way `$._meta.state` is indexed today).

- Out of scope follow-up: a state-only fast-path can later route `groupBy: ["state"]` directly to the lifecycle-index reader that `CountByState` already uses. Not v1.

- The cassandra PR is an independent timeline. Adding `Iterable` to the SPI doesn't break any plugin — it's a type-assertion contract with default-to-501 behavior. Until the cassandra `Iterable` PR lands, cassandra-backed deployments running cyoda-go v0.8.0 return 501 on the grouped-stats endpoint. Honest graceful degradation.

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
| stdev — low-variance / high-mean (monetary valuation regime) | Welford produces correct stdev within float64 ulp tolerance |
| non-numeric values mixed in | silently skipped |
| all-null aggregation field | result is JSON `null` |
| count-only request (no `aggregations` field) | response omits `aggregations` |
| explicit `aggregations: []` | equivalent to count-only |
| OpenAPI schema vs DTO check | DTO `omitempty` and OpenAPI schema agree on `aggregations` absence |
| groupBy contains `[*]` / `[N]` / `[?(...)]` | 400 `INVALID_GROUP_BY_PATH` |
| aggregation `field` contains `[*]` / `[N]` / `[?(...)]` | 400 `INVALID_AGGREGATION_FIELD` |
| duplicate groupBy | 400 `DUPLICATE_GROUP_BY` |
| identical `(op, field)` aggregation pair (no `as`) | silently deduped |
| distinct `(op, field)` pair colliding on explicit `as` | 400 `DUPLICATE_AGGREGATION_ALIAS` |
| **filter pushdown observable on `Iterate` path — sqlite** | narrow predicate (`state = 'X'`) → row count yielded by iterator ≪ total entities in model (assertion uses a counting wrapper in the parity harness) |
| **filter pushdown observable on `Iterate` path — postgres** | same case; new postgres Filter→SQL translator emits a `WHERE` clause that yields a small row set |
| **filter pushdown observable on `Iterate` path — cassandra** | narrow predicate on `state` → planner reads lifecycle index; iterator yields only matching entities |
| **filter pushdown observable on `Iterate` path — memory** | narrow predicate → `match.MatchFilter` inside `Next()` skips non-matching; iterator yields only matching entities |
| **non-pushdownable condition (e.g. function condition) — all backends** | iterator receives nil filter, yields all entities; service applies `match.Match` per entity; result identical to pushdown path |
| **partial pushdown (residual filter)** | sqlite + postgres iterators yield rows matching pushable part; iterator's `Next()` applies residual before yielding; service sees only matching entities |
| sqlite stdev request | plugin returns `ErrAggregationNotPushdownable`; service falls through to streaming-tally |
| postgres in-tx grouped-stats | pushdown skipped; iterator uses Filter→SQL translator on entity_versions snapshot; correct buckets returned |
| cardinality ceiling exceeded | 422 `GROUP_CARDINALITY_EXCEEDED` |
| backend with neither capability | 501 `NOT_IMPLEMENTED_BY_BACKEND` (only exercised by a mock test-backend) |
| tenant isolation | tenant A's request on a model also-owned-by tenant B sees only tenant A buckets |
| SQL-injection surface — groupBy path | `'`, `;`, `--`, `\\`, `\n`, NUL → 400 `INVALID_GROUP_BY_PATH` (rides on existing path validator) |
| SQL-injection surface — aggregation field | same surface, same validator, same 400 |
| **Postgres translator parity vs sqlite translator** | for each operator the sqlite translator pushes, the postgres translator pushes the same — identical residual-vs-pushed dissection on the same Filter tree |
| **Postgres `cyoda_try_float8` row-by-row table** | every row of the §6 table verified |
| concurrent writes + grouped-stats reads (memory plugin) | snapshot-then-iterate contract: stats result is internally consistent against the snapshot taken at request entry; writers do not block on the iterator |

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

The `aggregations` field on `GroupedStatsBucket` is marked `omitempty` on the Go DTO so the field is omitted when no aggregations were requested. Parity test verifies the absent-vs-empty-object distinction matches the schema.

No issue IDs in user-facing schema descriptions per project policy.

### `cyoda help` (`cmd/cyoda/help/content/`)

Updates to the search/stats help topic:

- Request and response examples (count-only, with aggregations, with pointInTime, multi-dim).
- Cardinality ceiling and its env var.
- JSONPath restriction (scalar only) and rationale.
- Aggregation operators, sample-stdev semantics, `stdev_pop` reservation.
- Numeric coercion contract for the postgres backend (the `cyoda_try_float8` table).
- Postgres expression-index guidance for hot grouping dimensions.
- Cassandra index-declaration guidance for hot filter dimensions (model authors can declare `$._meta`-equivalent indexes to keep the iterator from full-shard-scanning on common predicates).
- In-tx behavior (routes through streaming-tally; tx-visible writes counted via VisibilityFilter).

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
| `mode` / `median` | New `AggregateOp` enum values. Per-backend support advertised via `ErrAggregationNotPushdownable`. Backends that can't fall back (unbounded distinct-value cardinality per group) return 422 `AGGREGATION_NOT_SUPPORTED_BY_BACKEND`. New `CYODA_STATS_AGG_INTERMEDIATE_MAX` knob caps per-bucket distinct-value cardinality. Worth its own design pass. |
| `stdev_pop` / population stdev | New `AggregateOp` value. v1 keeps `stdev` as sample (n−1). |
| Array-projection in `groupBy` (`$.tags[*]`) | New validation accepts `[*]`; `GroupExpr.Kind` gains `DataPathArray`. Per-backend fan-out semantics specified in that spec. |
| Numeric paging | Optional `offset` paired with `limit` once the sort key is stable. |
| Distinguishing absent from literal-null in `groupKey` | Optional `nullPolicy: "merge" | "distinguish"` request field; default stays `merge`. |
| **Postgres `Searcher`** | The Filter→SQL translator added in this change is reusable substrate. A future PR can implement `spi.Searcher` on postgres on top of it, closing the existing GetAll-fallback gap for `/search/*`. Not in scope here. |
| **Sqlite stdev pushdown** | Future numerical-stability work (e.g. Welford emulation in SQLite via aggregate extension) can let sqlite stop returning `ErrAggregationNotPushdownable` for stdev. v1 ships the streaming fallback. |

## 9. Release sequencing

Coordinated change across three repos. Sequencing per project policy.

1. **cyoda-go-spi PR** — additive: `Iterable`, `Iterator`, `IterateOptions`, `GroupedAggregator`, `GroupExpr`, `AggregateExpr`, `AggregateOp`, `GroupedAggregationsOptions`, `GroupKeyEntry`, `GroupedAggregateBucket`, `ErrGroupCardinalityExceeded`, `ErrAggregationNotPushdownable`. `Iterate` carries the filter parameter from the start. Lands on cyoda-go-spi `main` on top of `c301c0e`. No tag yet — folds into the eventual v0.8.0 SPI tag.

2. **cyoda-go-spi v0.8.0 tag** — published once the v0.8.0 SPI scope (tx-state sentinels + this change) is settled. Adding both new interfaces is type-assertion-only; cassandra (and any out-of-tree plugin) can pin v0.8.0 SPI without implementing either.

3. **cyoda-go PR series on `release/v0.8.0`**:
   - SPI pin bump (one isolated commit).
   - Handler + DTO + validation.
   - Service-layer dispatch + accumulators (Welford) + post-processing.
   - **Postgres Filter→SQL translator** (`plugins/postgres/query_planner.go` + path validation + tests). Discrete PR — independently reviewable substrate that the plugin's `Iterate` and `GroupedAggregate` both depend on.
   - Plugin implementations:
     - memory `Iterable` + `GroupedAggregator`,
     - sqlite `Iterable` (filter via existing `planQuery`) + `GroupedAggregator`,
     - postgres `Iterable` + `GroupedAggregator` (uses new translator) + `cyoda_try_float8` migration.
   - Conformance and parity tests (the §7 matrix).
   - E2E test.
   - OpenAPI + help + config + COMPATIBILITY.

4. **cyoda-go-cassandra PR** — independent timeline. SPI pin bump to v0.8.0 (one combined bump for tx-state + new interfaces) + `Iterable` implementation wiring the existing shard executor into an iterator. Strictly in scope per memory `feedback_courtesy_pr_scope`.

## 10. Non-goals

- **Numeric aggregations beyond Tier 1+2** — mode and median are deferred.
- **Returning entity bodies** — that's `/search/*`.
- **Cross-model joins** — out of scope.
- **`having` post-aggregation predicates** — forward-compat noted but not built.
- **Array fan-out in `groupBy`** — out of scope; surfaces left open.
- **Auto-created postgres expression indexes per data field** — out of scope; documented as caller responsibility in the help topic.
- **Postgres `Searcher` implementation** — the new Filter→SQL translator is reusable substrate, but wiring `Searcher` on postgres (and migrating `/search/*` off its GetAll fallback) is a separate PR with its own design considerations. Not in scope here.
- **Streaming `/search/*` itself** — `Iterable` is for grouped-stats; any future streaming-search migration is a separate design with its own SPI shape decisions.
- **Distinguishing absent from literal-null in `groupKey`** — out of scope for v1 per D4.
- **Sqlite-native stdev pushdown** — out of scope per D9; sqlite stdev requests route through streaming-tally with Welford.
- **Cassandra GroupedAggregator implementation** — not feasible against CQL `GROUP BY` semantics for arbitrary JSONPaths. Cassandra grouped-stats goes through `Iterable` + streaming-tally.

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
- **Filter pushdown is observable on the streaming-tally path**: for each backend, a parity test with a narrow indexed-or-pushable predicate observes the iterator yielding `≈ matching` entities, not `model size`. Cassandra against a `state` predicate uses the lifecycle index; sqlite/postgres against a scalar-equality predicate compile to a `WHERE`; memory applies the matcher inside `Next()`. Non-pushdownable conditions degrade gracefully to full iteration + in-process `match.Match`, with identical result.
- Auth, tenant scoping, Condition validation, and error model identical to `/search/*` where shared. Tenant isolation verified by parity test.
- `CYODA_STATS_GROUP_MAX` ceiling enforced consistently across all backends (`LIMIT MaxBuckets+1` in SQL, `> ceiling` check in streaming); exceeded returns 422 with the ceiling value.
- Numerical contracts pinned: postgres `cyoda_try_float8` table verified row-by-row in parity; streaming-tally stdev passes the low-variance/high-mean stress case within float64 ulp tolerance; sqlite stdev pushdown declined deterministically, falls through cleanly.
- Postgres Filter→SQL translator is parity-tested against sqlite's existing planner: for every operator the sqlite translator pushes, postgres pushes the same; residual-vs-pushed dissection is identical for the same Filter tree.
- Backend without either capability returns 501.
- `cyoda help` search/stats topic updated; OpenAPI spec updated; CONFIG env var documented; COMPATIBILITY.md updated alongside the SPI pin bump.
- Conformance parity tests cover the §7 matrix including the new filter-pushdown observability cases. Race detector clean before PR per memory `feedback_race_testing_discipline`.
