# Grouped entity statistics query — design

| Field | Value |
|---|---|
| Issue | [#299](https://github.com/Cyoda-platform/cyoda-go/issues/299) |
| Target milestone | v0.8.0 (cyoda-go), v0.8.0 (cyoda-go-spi) |
| Spec date | 2026-06-14 |
| Review iterations | 3 (independent reviews on architecture, then iterator filter-awareness, then performance / memory / correctness / concurrency) |
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

Each entry is the option chosen and the rationale in one line.

| # | Decision | Rationale |
|---|---|---|
| D1 | Response shape: top-level `count` always emitted; `aggregations` map sibling, omitted when none requested. | Forward-compatible with sum/avg/etc.; doesn't force nesting on count-only callers. |
| D2 | No server-side cap on entities scanned; cap is on **result group cardinality** (`CYODA_STATS_GROUP_MAX`, default 10000). Exceeded → 422 `GROUP_CARDINALITY_EXCEEDED`. | Implementation streams/pushes-down; intrinsic cost dimension is buckets, not entities. 422 (semantic, unprocessable) is more correct than 400. |
| D3 | Optional `limit` request field. When set, server returns top-N by `(count desc, groupKey lex)`. Default = unlimited (within `CYODA_STATS_GROUP_MAX`). `limit > CYODA_STATS_GROUP_MAX` → 400 `INVALID_LIMIT`. | Display convenience; orthogonal to safety bound. A `limit` larger than the safety ceiling is nonsensical, so we reject up front rather than silently capping. |
| D4 | Missing JSONPath value → JSON `null` bucket key. Conflates with literal-`null` values (SQL `IS NULL` precedent). Non-scalar extracted values (object/array at runtime even when the path is scalar-shaped) also coerce to `null` for cross-backend consistency. | Deliberate v1 simplification. |
| D5 | `groupBy` accepts scalar JSONPaths only. Anything containing `[` after bracket-quoted-property normalization → 400 `INVALID_GROUP_BY_PATH`. | Array fan-out is a follow-up. Bracket-quoted property access (`$.['variantId']`) is normalized to dotted form during validation. |
| D6 | Add two new SPI capabilities: `Iterable` (filter-aware streaming) and `GroupedAggregator` (native GROUP BY pushdown). Both optional via type assertion. Backend implementing neither → 501 `NOT_IMPLEMENTED_BY_BACKEND`. | A streaming primitive is the honest cassandra implementation (no native CQL `GROUP BY` on JSONPath). Filter-aware so the fallback path doesn't full-scan. |
| D7 | Path: `POST /api/entity/stats/{entityName}/{modelVersion}/query`. | More RESTful — model is the resource, `query` is the action. |
| D8 | Aggregations Tier 1+2 in v1: `sum`, `avg`, `min`, `max`, `stdev`. Sample stdev (n−1 denominator); v1 op name is `stdev`. `stdev_pop` reserved. Mode/median deferred. | Tier 1+2 marginal cost is low; mode/median introduce unbounded intermediate state. |
| D9 | Stdev numerical contract: **Welford's online algorithm** on streaming-tally. Postgres pushes `STDDEV_SAMP`. Sqlite returns `ErrAggregationNotPushdownable` for stdev and falls through to streaming. **Parity tolerance: relative error ≤ 1e-9 between backends.** `n < 2` → `null` on both paths. | Welford and `STDDEV_SAMP` are both numerically stable but not bit-identical; ulp tolerance is over-tight. |
| D10 | Stdev pushdown asymmetry handled by `ErrAggregationNotPushdownable`: a `GroupedAggregator` may decline a specific request shape, signalling the service to fall through to streaming-tally. | Coarse capability check; fine per-request opt-out. |
| D11 | In-tx grouped-stats is **supported** via the streaming-tally path. **Memory backend in-tx path overlays `tx.Buffer` and excludes `tx.Deletes`** (RYW-correct, matching the actual `/search/*` precedent at `internal/domain/search/service.go:118-136` which falls back to `GetAll` — and memory's `GetAll` is RYW-correct via the same buffer overlay). Pushdown skipped when `spi.GetTransaction(ctx) != nil`. No new error code. | RYW is what the precedent actually does; spec must match. |
| D12 | `groupKey` in responses is an **ordered array of `{path, value}` pairs**, not a map keyed on JSONPath strings. | OpenAPI-typeable, friendly for typed clients, preserves request order. |
| D13 | Target v0.8.0. cyoda-go-spi v0.8.0 tag not yet published; additive SPI changes fold alongside #16. | v0.8.0 is not feature-frozen. |
| D14 | `Iterable.Iterate(model, filter, opts)` takes a Filter parameter. Iterator yields entities matching the filter; plugins push what they can and apply residual inside `Next()`. | Without it, the streaming-tally path full-scans; cassandra (which always falls through) suffers 10,000×+ cost ratio on narrow predicates. |
| D15 | When `search.ConditionToFilter` errors (function conditions, future operators not yet translated), the service passes a zero-value `Filter` to `Iterate` (match-all) and re-applies `match.Match` per yielded entity. Otherwise the iterator's yielded set is authoritative and the service does not re-check. | One source of truth per pluggable surface. |
| D16 | Postgres gets a new Filter→SQL translation layer as part of this change. Greedy AND / conservative OR, JSONB ops. Supports the same operator set sqlite pushes. **Reusable substrate for a future postgres `Searcher`**, but that work is out of scope here. | Postgres has no `Searcher` today; without this layer, postgres `Iterate` would be filter-blind. |
| D17 | **Cardinality detection via `LIMIT MaxBuckets+1` without `ORDER BY` is correct.** SQL `LIMIT N` is strict: returns `min(actual, N)` rows. Postgres parallel/hash-aggregate plans materialize all groups before emit; streaming-aggregate plans emit at most N+1 groups before stopping. In all cases we observe `rowcount == MaxBuckets+1` iff actual cardinality > `MaxBuckets`. Non-determinism in *which* buckets we'd return doesn't matter because we discard them and 422. **Adding `ORDER BY` would force a full sort over the group set, killing the early-exit optimization for no contract benefit.** | Verified against postgres planner semantics; same logic applies to sqlite. |
| D18 | `buildGroupKey` encoding: **length-prefixed concatenation** — `len(v_0)|v_0|len(v_1)|v_1|…` where lengths are 4-byte big-endian uints and nil is encoded as a sentinel length of `0xFFFFFFFF`. Used as a Go `string` map key (Go strings can hold arbitrary bytes). | Prevents collision under naive `strings.Join` (e.g. `["a|b","c"]` vs `["a","b|c"]`); avoids JSON serialization alloc; zero-copy `[]byte`-to-`string` conversion. |
| D19 | Ship one canonical postgres expression index in this PR: partial `(tenant_id, model_name, model_version, (doc->'_meta'->>'state'))` `WHERE NOT deleted`. Data-field expression indexes are documented as caller responsibility in the help topic. | State-based grouping/filtering is the SIOMS driving case; this is the one index that materially changes the perf story. Wider data-field indexing is application-specific. |
| D20 | Memory snapshot is `[]*entityVersion` (pointer slice) — 8 bytes per matching entity, not entity-payload bytes per matching entity. This relies on the invariant that `entityVersion` fields are immutable post-publish. The invariant is already true in current code (`saveUnlocked` appends; never mutates published slots); this PR adds explicit godoc on `entityVersion` documenting it and a `// invariant: immutable post-publish` comment in `saveUnlocked`. | 1 GB vs 80 MB per-request memory at 10M entities × 100-byte payloads. The invariant is already-honored fact; making it explicit is the cost. |
| D21 | Postgres `cyoda_try_float8(text) RETURNS float8` is implemented as `LANGUAGE sql IMMUTABLE PARALLEL SAFE` — `CASE WHEN val ~ '<numeric-regex>' THEN val::float8 ELSE NULL END` wrapped in `NULLIF(..., 'Infinity'::float8)`. No PL/pgSQL EXCEPTION block — that approach is one subtransaction per row (~30M subtransactions on a 10M-row scan × 3 numeric aggregations), unacceptable cost. | Reviewer flagged the PL/pgSQL EXCEPTION approach as a real per-row perf cost. SQL-language form has regex cost only. Regex is conservative enough that valid-but-overflowing inputs (`'1e500'`) yield Infinity; the `NULLIF` strips that. |
| D22 | Cassandra `Iterate` walks an **in-memory entity-ID list per shard** (the existing `ListEntityIDs` materializes `[]gocql.UUID` per shard), then yields entities one-materialization-at-a-time via point-reads through the visibility filter. **Shards are walked sequentially**, not fanned out in goroutines. | Honest: the streaming property exists at per-entity materialization, not at per-shard scan. Sequential shard walk: simpler, no extra goroutine memory, and per-entity point-read is the dominant cost — parallel scan would barely move the needle. |

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

- **`groupBy`** (required, 1..N): each entry is the reserved token `"state"` or a scalar JSONPath. Order in the request determines order in the response's `groupKey` array.
- **`condition`** (optional): the existing search `Condition` DSL. Omitted ⇒ match-all.
- **`aggregations`** (optional, 0..N): per entry: `op` ∈ {`sum`, `avg`, `min`, `max`, `stdev`}, `field` scalar JSONPath, optional `as` alias. When `as` is omitted, server synthesizes `<op>_<field>`. Server **dedupes identical `(op, field)` pairs**. Aliases that collide on **distinct** `(op, field)` pairs → 400 `DUPLICATE_AGGREGATION_ALIAS`.
- **`pointInTime`** (optional RFC 3339): historical snapshot; default = now.
- **`limit`** (optional positive int, `≤ CYODA_STATS_GROUP_MAX`): top-N by `(count desc, groupKey lex)`. `> CYODA_STATS_GROUP_MAX` → 400 `INVALID_LIMIT`. Default = unlimited.

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

**Total order for response sorting** (D12, D18): primary key is `count` descending; tiebreaker is groupKey lex defined as element-wise comparison of the `groupKey` array. Per entry: `null` < any string (`null` sorts first); strings compared via bytes-wise (UTF-8 lex). Two `null`s compare equal. Both backends and the streaming-tally path emit identical ordering for the same bucket set.

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
| 400 | `DUPLICATE_AGGREGATION_ALIAS` | two aliases collide on **distinct** `(op, field)` pairs |
| 400 | `INVALID_OPERATOR` / `INVALID_CONDITION` / depth-exceeded | propagated from `predicate.ParseCondition` + `search.ValidateCondition` |
| 400 | `INVALID_POINT_IN_TIME` | unparseable RFC 3339 |
| 400 | `INVALID_LIMIT` | non-positive int, or `> CYODA_STATS_GROUP_MAX` |
| 401 | (standard) | missing/invalid Bearer |
| 413 | (standard) | request body > 10 MiB |
| 422 | `GROUP_CARDINALITY_EXCEEDED` | result buckets exceed `CYODA_STATS_GROUP_MAX`; body includes the ceiling |
| 501 | `NOT_IMPLEMENTED_BY_BACKEND` | backend implements neither `Iterable` nor `GroupedAggregator` |
| 500 | (with ticket UUID) | internal/driver errors; generic message per project policy |

In-transaction calls are **not** an error — they route through the streaming-tally path via `Iterable`, with RYW semantics (D11).

## 4. Service-layer dispatch

```go
import "github.com/cyoda-platform/cyoda-go/internal/domain/search"

// 1. Native pushdown when available, condition pushdownable, and not in-tx.
//    (In-tx skips pushdown — matches /search/* precedent.)
if ga, ok := store.(spi.GroupedAggregator); ok && spi.GetTransaction(ctx) == nil {
    flt, terr := search.ConditionToFilter(req.Condition)  // (Filter, error)
    if terr == nil {
        out, err := ga.GroupedAggregate(ctx, modelRef, req.GroupBy, flt, aggOpts)
        if err == nil {
            return s.postProcess(out, req), nil
        }
        if !errors.Is(err, spi.ErrAggregationNotPushdownable) {
            return nil, err
        }
        // plugin opted out of pushdown for this shape; fall through to streaming
    }
}

// 2. Streaming fallback. Filter-aware: iterator yields entities matching
//    the filter (when ConditionToFilter succeeded); otherwise pass match-all
//    and re-apply match.Match per yielded entity (D15).
if it, ok := store.(spi.Iterable); ok {
    flt, terr := search.ConditionToFilter(req.Condition)
    filterOK := req.Condition == nil || terr == nil
    if !filterOK {
        flt = spi.Filter{}  // zero-value; iterator treats as match-all
    }
    return s.tallyStreaming(ctx, it, req, flt, filterOK)
}

// 3. 501
return nil, ErrBackendNotSupported
```

`tallyStreaming`:

```go
iter, err := it.Iterate(ctx, modelRef, flt, spi.IterateOptions{PointInTime: req.PointInTime})
if err != nil { return nil, err }
defer iter.Close()  // idempotent (D5 contract)

acc := newAccumulators(req)  // per-bucket: count, Welford-per-aggregation
for iter.Next() {
    e := iter.Entity()
    // Iterator owns filter semantics when filterOK; service rechecks only when
    // ConditionToFilter could not represent the full condition (D15).
    if !filterOK && req.Condition != nil &&
       !match.Match(req.Condition, e.Data, e.Meta) {
        continue
    }
    k := buildGroupKey(req.GroupBy, e)  // length-prefixed, see D18
    // Cardinality check: when k is new and we already hold MaxBuckets distinct
    // keys, adding k would make MaxBuckets+1 — overflow. (We trip on the
    // (MaxBuckets+1)th distinct key, matching SQL's LIMIT MaxBuckets+1
    // overflow detection.)
    if !acc.has(k) && acc.len() >= maxGroupCardinality {
        return nil, ErrGroupCardinalityExceeded
    }
    acc.observe(k, e)  // skips non-numeric samples per aggregation
}
if err := iter.Err(); err != nil { return nil, err }
return s.postProcess(acc.materialize(), req), nil
```

`acc.observe` updates each aggregation's accumulator via Welford for stdev:

```
n      += 1
delta   = x - mean
mean   += delta / n
delta2  = x - mean                 // post-update
m2     += delta * delta2
// stdev_samp = sqrt(m2 / (n - 1))  (when n >= 2; n < 2 → null per D9)
```

Post-processing: when `Limit > 0 && Limit < len(buckets)/2`, extract top-N via `container/heap` (O(N log Limit)); otherwise full sort by the D12 total order then slice. Backend-independent observable ordering.

## 5. SPI delta (cyoda-go-spi v0.8.0)

Additive. No breaking changes. Lands on `cyoda-go-spi` main on top of `c301c0e`.

```go
// Streaming iteration over a model's entities matching a Filter.
//
// Semantics:
//   - Plugins push pushable parts of the filter into storage (SQL WHERE, CQL
//     index lookup); residual is applied inside Next() before yielding.
//   - A zero-value Filter means "yield all entities for the model" (subject to opts).
//   - Implementations MUST NOT hold a global write-blocking lock for the
//     lifetime of the iterator. Snapshot-then-iterate or cursor-based.
//   - The iterator MUST observe ctx cancellation. When the caller cancels ctx,
//     the underlying driver surfaces an error; the iterator reports it via Err()
//     and Next() returns false.
//   - No retry on transient driver errors. First error from the underlying
//     driver becomes the sticky Err(); Next() returns false thereafter.
//   - Close() is idempotent — safe to call multiple times (defer-friendly).
type Iterable interface {
    Iterate(
        ctx context.Context,
        model ModelRef,
        filter Filter,                  // zero-value = match-all
        opts IterateOptions,
    ) (Iterator, error)
}

type Iterator interface {
    Next() bool         // advance; false on end or sticky error
    Entity() *Entity    // current row (valid only after Next() == true)
    Err() error         // sticky; first error encountered
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
        filter Filter,
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
    Value any // string for scalar/state, nil for missing/literal-null/non-scalar (D4)
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

`internal/match` gains a new sibling of `Match`:

```go
// MatchFilter evaluates an SPI Filter against an entity. Filter is the
// pushdown-friendly subset of predicate.Condition used by GroupedAggregator,
// Iterable, and (existing) Searcher.
//
// Used by memory plugin's Iterate to apply filters inside Next().
func MatchFilter(f spi.Filter, data []byte, meta spi.EntityMeta) bool { ... }
```

Forward-compat: `having`, `mode` / `median`, `stdev_pop`, array-projection in `groupBy`, paging — all land as additive enum values or optional fields without a major bump. See §8.

## 6. Per-plugin implementations

### Memory (`plugins/memory`)

Implements **both** `Iterable` and `GroupedAggregator`. New godoc on `entityVersion` documenting the immutability invariant (D20) that the snapshot pattern relies on.

- **Non-tx `Iterate`** (snapshot-then-iterate, D20): under `entityMu.RLock`, walk `entityData[tenant]` once and append `*entityVersion` pointers to a snapshot slice where the version matches model + (optional) `pointInTime` + not-deleted. Release the RLock. Iterator walks the snapshot lock-free; inside `Next()` applies `match.MatchFilter(filter, e.Data, e.Meta)` before yielding. Memory cost: 8 bytes × matching entities.

- **In-tx `Iterate`** (RYW-correct, D11): non-tx snapshot + overlay `tx.Buffer` (in-flight writes) + filter out entities present in `tx.Deletes`. Mirrors what `memory/entity_store.go:339-381` does for `GetAll` inside a tx. Iterator yields the overlaid set lock-free.

- `GroupedAggregate`: same snapshot pattern, applies `match.MatchFilter`, extracts group keys via `gjson.GetBytes`, accumulates per-bucket via Welford. Enforces `MaxBuckets` and returns `ErrGroupCardinalityExceeded` on overflow.

The memory snapshot walks `entityData[tenant]` and filters by `ModelRef` per entry — the underlying map shape is `tenantID → entityID → versions`, not partitioned by model. Cost is `O(tenant entities)`, not `O(model entities)`. Documented in the help topic for operators.

### SQLite (`plugins/sqlite`)

Implements **both**.

- `Iterate(model, filter, opts)`: reuses the existing `planQuery`/`dissect` translator (`plugins/sqlite/query_planner.go`). Pushable filter → `WHERE`; residual captured for in-iterator post-filtering. Wraps `sql.Rows` with `rows.Next()`. Inside `Next()`, if the plan has a `postFilter`, evaluate it against the scanned row before yielding. PointInTime path uses the existing `entity_versions` snapshot query.

- `GroupedAggregate`: pushes count + sum + avg + min + max natively. **Returns `ErrAggregationNotPushdownable`** for any request containing `stdev` (no native `STDDEV`; single-pass formula unsafe for valuation data — D9). Also returns it when the filter has a residual (the plugin doesn't post-filter inside aggregation pipelines).

  SQL template (no stdev path):

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

  `GROUP BY` uses full extractor expressions (not aliases) for portability. `LIMIT MaxBuckets+1` overflow detection per D17.

### Postgres (`plugins/postgres`)

Implements **both**. **Adds:**
- a new Filter→SQL translation layer (D16);
- the `cyoda_try_float8(text)` immutable SQL helper (D21);
- a canonical state expression index (D19) via migration.

#### Filter→SQL translator (`plugins/postgres/query_planner.go`)

Analogue of sqlite's `planQuery`/`dissect`. Same input (SPI `Filter` tree) and same output shape (`(whereClause, args, postFilter *Filter)`), with three differences:

1. **JSONB operators** for path extraction: `doc->'_meta'->>'state'` for state; `doc->>'variantId'` (and chained `->`/`->>` for nested paths) for data fields. Path translation rides on the existing `path_validation` package (no array projection, no SQL meta-characters).
2. **Operator mapping:** `=`, `!=`, `<`, `<=`, `>`, `>=`, `IN`, `LIKE`, `ILIKE`, `IS NULL`, `IS NOT NULL`, `BETWEEN`. Same set sqlite already pushes. Numeric comparisons via `cyoda_try_float8`; string comparisons compare `text` directly.
3. **Pushdown discipline:** greedy AND / conservative OR. Same shape as sqlite's `dissect`; ~300 LOC plus tests.

Used by both `Iterate` and `GroupedAggregate`. Parity-tested against sqlite's translator on the shared operator set (§7).

#### `cyoda_try_float8(text) RETURNS float8` (D21)

```sql
CREATE FUNCTION cyoda_try_float8(t text) RETURNS float8 AS $$
  SELECT NULLIF(
    CASE WHEN t ~ '^-?[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]+)?$'
         THEN t::float8
         ELSE NULL END,
    'Infinity'::float8
  );
$$ LANGUAGE sql IMMUTABLE PARALLEL SAFE;
```

No subtransactions. Behavior:

| Input | Output |
|---|---|
| SQL `NULL` (key missing) | `NULL` |
| `''` | `NULL` (regex rejects) |
| `'abc'`, `'NaN'`, `'Infinity'`, `'inf'`, `'+Inf'` | `NULL` (regex rejects) |
| `'-1.5'`, `'42'`, `'1e308'` | parsed `float8` |
| `'1e500'` (valid-but-overflows-float8) | `NULL` (regex accepts; `::float8` yields `Infinity`; `NULLIF` strips) |
| JSON `true` / `false` / array / object (via `->>` text rendering, yields `'true'`/`'[…]'` etc.) | `NULL` (regex rejects) |
| `'  42  '` (whitespace) | `NULL` (regex rejects — strict; callers can sanitize at ingest) |

Parity tests cover every row.

#### Canonical state expression index (D19)

New migration `00000N_grouped_stats_state_idx.up.sql`:

```sql
CREATE INDEX IF NOT EXISTS entities_state_idx
ON entities (tenant_id, model_name, model_version, (doc->'_meta'->>'state'))
WHERE NOT deleted;
```

Backs the SIOMS driving case (state-based filter + groupBy). Data-field expression indexes (e.g. `((doc->>'variantId'))`) remain caller responsibility, documented in the help topic with `CREATE INDEX` recipes.

#### `Iterate(model, filter, opts)`

Wraps `pgx.Rows`. Translator produces `whereClause + postFilter`. SQL:

```sql
SELECT doc
FROM entities  -- or entity_versions for pointInTime
WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted
  AND <whereClause from translator>
```

PointInTime variant uses the bi-temporal `entity_versions` table with `valid_time <= $N AND transaction_time <= CURRENT_TIMESTAMP` **and `(doc->'_meta'->>'deleted')::boolean IS NOT TRUE`** to exclude deletion-marker versions (mirrors `GetAsAt` at `entity_store.go:206-213`).

Inside `Next()`, if `postFilter != nil`, evaluate against scanned row before yielding.

#### `GroupedAggregate`

Same translator. SQL:

```sql
SELECT
    doc->>'variantId'                             AS gk_0,
    doc->'_meta'->>'state'                        AS gk_1,
    COUNT(*)                                      AS cnt,
    SUM(cyoda_try_float8(doc->>'costPrice'))      AS agg_totalCost,
    AVG(cyoda_try_float8(doc->>'costPrice'))      AS agg_avg_x,
    STDDEV_SAMP(cyoda_try_float8(doc->>'costPrice')) AS agg_stdev_x
FROM entities  -- or entity_versions for pointInTime (with the deletion-marker filter)
WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted
  AND <whereClause from translator>
GROUP BY doc->>'variantId', doc->'_meta'->>'state'
LIMIT <MaxBuckets + 1>
```

When the translator produces a non-nil `postFilter`, `GroupedAggregate` returns `ErrAggregationNotPushdownable` and falls through to streaming-tally.

### Cassandra (`cyoda-go-cassandra`, commercial backend)

Implements **`Iterable` only** in v1. Does **not** implement `GroupedAggregator`.

- Rationale: CQL `GROUP BY` works only on partition/clustering keys.

- `Iterate(model, filter, opts)` (D22): wires the existing `search/shard_executor.go` planner output into an iterator. The planner produces:
  - **Indexed predicates** (state, declared lifecycle index fields) → `index_string_data` lookup yielding entity IDs.
  - **Non-indexed predicates** → full-shard scan, post-filter in Go.

  Per shard, `ListEntityIDs` materializes an `[]gocql.UUID` in memory (this is the existing implementation; not new code). The iterator then walks that ID list and point-reads each entity through the visibility filter; entities are yielded one materialization at a time. **Shards are walked sequentially**, not fanned out in goroutines.

  `Next()` applies any planner residual filter before yielding.

- PointInTime honored via the existing `tx.VisibilityFilter` HLC contract.

- Cost profile:
  - Indexed-predicate condition (state or declared lifecycle field): work is **`O(matching entities)`** — SIOMS inventory dashboard regime.
  - Non-indexed condition: full-shard scan + post-filter. Documented in the help topic with index-declaration guidance.
  - In-memory ID-list per shard is `O(matching entities in shard)` × 16 bytes; for very large per-shard ID sets, the spec acknowledges that as a known limitation of the current `ListEntityIDs` shape. Refactoring to a per-shard channel/iterator is a follow-up outside #299's scope.

- Out of scope follow-up: state-only fast-path routing `groupBy: ["state"]` directly to the lifecycle-index reader. Not v1.

- The cassandra PR is an independent timeline. Adding the new SPI interfaces is type-assertion-only; cassandra can pin SPI v0.8.0 without implementing either. Until the cassandra `Iterable` PR lands, cassandra-backed deployments running cyoda-go v0.8.0 return 501.

## 7. Conformance, OpenAPI, help, e2e

### Cross-backend conformance (`internal/e2e/parity/registry.go`)

New parity cases. Picked up by every backend including cassandra.

| Case | Cover |
|---|---|
| single data-field group-by | basic happy path |
| `state` group-by | parity with existing `/entity/stats/states` |
| multi-dim `$.variantId × state` | cartesian buckets, response ordering |
| with vs without `condition` | reuse of search DSL |
| with `pointInTime` (including a deleted entity at the requested instant) | historical snapshot; **deletion-marker versions are excluded** |
| in-tx call routes through streaming-tally | matches `/search/*` precedent; no error |
| **in-tx grouped-stats sees own uncommitted writes (RYW)** | memory backend overlay of `tx.Buffer` / `tx.Deletes` |
| sum / avg / min / max | Tier 1 aggregations |
| stdev — wide dynamic range (8+ orders of magnitude) | nominal numerical sanity |
| stdev — low-variance / high-mean (monetary valuation regime) | Welford vs `STDDEV_SAMP` **within 1e-9 relative error** (D9) |
| **stdev with `n=1` bucket** | both backends → `null` |
| **stdev with empty-numeric bucket** (all values non-numeric) | both backends → `null` |
| non-numeric values mixed in | silently skipped |
| **runtime non-scalar extracted value** (path is `$.variantId`, entity has `{"variantId": {"x": 1}}`) | bucketed under `null`, consistent across backends (D4) |
| count-only request (no `aggregations` field) | response omits `aggregations` |
| explicit `aggregations: []` | equivalent to count-only |
| OpenAPI schema vs DTO check | DTO `omitempty` and OpenAPI schema agree |
| groupBy contains `[*]` / `[N]` / `[?(...)]` | 400 `INVALID_GROUP_BY_PATH` |
| **groupBy bracket-quoted scalar** (`$.['variantId']`) | accepted; normalized to dotted form |
| aggregation `field` contains `[*]` / `[N]` / `[?(...)]` | 400 `INVALID_AGGREGATION_FIELD` |
| duplicate groupBy | 400 `DUPLICATE_GROUP_BY` |
| identical `(op, field)` aggregation pair (no `as`) | silently deduped |
| distinct `(op, field)` pair colliding on explicit `as` | 400 `DUPLICATE_AGGREGATION_ALIAS` |
| **`limit > CYODA_STATS_GROUP_MAX`** | 400 `INVALID_LIMIT` |
| filter pushdown observable — sqlite | narrow predicate → row count yielded by iterator ≪ total via counting wrapper |
| filter pushdown observable — postgres | narrow predicate → uses `entities_state_idx` (D19); `EXPLAIN` asserts index scan |
| filter pushdown observable — cassandra | indexed predicate → planner reads lifecycle index, not full scan |
| filter pushdown observable — memory | narrow predicate → `MatchFilter` skips non-matching inside `Next()` |
| non-pushdownable condition (function condition) — all backends | iterator gets match-all filter; service applies `match.Match` per yielded entity; result identical to pushdown path |
| partial pushdown (residual filter) | sqlite + postgres iterators yield rows matching pushable part; iterator's `Next()` applies residual; service sees only matching entities |
| sqlite stdev request | plugin returns `ErrAggregationNotPushdownable`; service falls through |
| postgres in-tx grouped-stats | pushdown skipped; iterator uses Filter→SQL on `entity_versions` snapshot with deletion-marker filter |
| **cardinality detection determinism** | overflowing request consistently returns 422 across parallel-query runs (D17) |
| cardinality ceiling exceeded | 422 `GROUP_CARDINALITY_EXCEEDED` |
| backend with neither capability | 501 `NOT_IMPLEMENTED_BY_BACKEND` (mock backend) |
| tenant isolation | tenant A's request on a model also-owned-by tenant B sees only tenant A buckets |
| SQL-injection surface — groupBy path | `'`, `;`, `--`, `\\`, `\n`, NUL → 400 `INVALID_GROUP_BY_PATH` |
| SQL-injection surface — aggregation field | same surface, same validator, same 400 |
| postgres translator parity vs sqlite translator | identical pushed-vs-residual dissection on the same Filter tree |
| postgres `cyoda_try_float8` row-by-row table | folded into postgres plugin unit tests (not cross-backend) |
| concurrent writes + grouped-stats reads (memory plugin) | snapshot consistency: stats result reflects the snapshot taken at request entry; writers do not block on the iterator |
| **iterator contract — Err() sticky** | inject a transient driver error mid-stream; `Err()` returns it; `Next()` stays `false`; no retry |
| **iterator contract — Close() idempotent** | double `Close()` returns the same (nil or error) twice without panic |
| **iterator contract — ctx cancellation observed** | cancel ctx mid-stream; `Next()` returns `false`; `Err()` is `ctx.Err()` (or driver-wrapped) |
| **buildGroupKey collision-free** | constructed values designed to collide under naive `strings.Join` produce distinct map keys (D18) |
| **postgres `entities_state_idx` is used** | `EXPLAIN` on a state-equality query shows `Index Scan` using `entities_state_idx` |
| **postgres `cyoda_try_float8` performance smoke** | bench-style test: 100k rows × 3 numeric aggregates runs within p99 budget defined per CI environment (catches regression on the SQL→PL/pgSQL flip) |

### E2E (`internal/e2e/grouped_stats_test.go`)

One e2e test hitting the full HTTP stack with the postgres testcontainer. Proves wiring; parity matrix carries the breadth.

### OpenAPI (`api/openapi.yaml`)

New path. New schemas: `GroupedStatsRequest`, `GroupedStatsBucket`, `GroupKeyEntry` (`{path: string, value: string | null}` — `nullable: true` on `value`, schema-typed not `any`), `GroupExpr`, `AggregationExpr`, `AggregateOp`. `aggregations` on `GroupedStatsBucket` is `omitempty` Go-side; parity test verifies absent-vs-empty-object distinction matches the schema.

No issue IDs in user-facing descriptions per project policy.

### `cyoda help` (`cmd/cyoda/help/content/`)

Updates to the search/stats help topic:

- Request and response examples (count-only, with aggregations, with pointInTime, multi-dim).
- Cardinality ceiling, `limit` upper bound, and the env var.
- JSONPath scalar-only restriction and rationale.
- Aggregation operators, sample-stdev semantics, `n<2` → null boundary, `stdev_pop` reservation.
- Numeric coercion contract for postgres (`cyoda_try_float8` table).
- **Postgres**: state grouping/filtering is index-backed out of the box (`entities_state_idx`); for hot data-field dimensions, callers add `CREATE INDEX ... ON entities ((doc->>'variantId'))`.
- **Cassandra**: index-declaration guidance for hot filter dimensions (model authors can declare lifecycle-style indexes to keep the iterator from full-shard-scanning on common predicates).
- **Memory**: snapshot cost is `O(tenant entities)`, not `O(model entities)` — relevant for operators sizing memory backends with many models per tenant.
- In-tx behavior (routes through streaming-tally; RYW-correct on memory; pushdown skipped on sqlite/postgres).
- Non-scalar runtime values bucket under `null`.

### Config (Gate 4)

`CYODA_STATS_GROUP_MAX` env var (default 10000) added to:

- `cmd/cyoda/help/content/config/*.md` — the relevant topic.
- `README.md` configuration reference.
- `DefaultConfig()` in code.

### COMPATIBILITY.md (Gate 4)

Updated when:

- the cyoda-go-spi v0.8.0 pin lands (one-line bump),
- the chart `appVersion:` bumps (auto-PR; human-reviewed),
- the cassandra plugin pin guidance changes once its v0.8.0 plugin tag exists.

## 8. Forward-compatibility hooks

Strict count + Tier 1+2 in v1. Surfaces left open: `having`, `mode`/`median` (each needs its own design pass — unbounded intermediate state, asymmetric backend support), `stdev_pop`, array-projection in `groupBy`, paging via `offset`, `nullPolicy` to distinguish absent from literal-null. Postgres `Searcher` is reusable substrate (D16) but its own separate PR.

## 9. Release sequencing

1. **cyoda-go-spi PR** — additive: all the new types from §5 including `MatchFilter` callout (the latter is a cyoda-go internal addition, not part of the SPI, but pinned to land in the same series). `Iterate` carries the filter parameter from the start. Folds into the v0.8.0 SPI tag alongside #16 (tx-state sentinels).

2. **cyoda-go-spi v0.8.0 tag** — once SPI scope settles. Adding both new interfaces is type-assertion-only; cassandra (and any out-of-tree plugin) can pin v0.8.0 SPI without implementing either.

3. **cyoda-go PR series on `release/v0.8.0`**:
   - SPI pin bump (one isolated commit).
   - Handler + DTO + validation.
   - `internal/match.MatchFilter` (new) + service-layer dispatch + accumulators (Welford) + post-processing (heap top-N when applicable).
   - **Postgres Filter→SQL translator** (`plugins/postgres/query_planner.go`, path validation, tests). Discrete PR — independently reviewable substrate.
   - **Postgres `cyoda_try_float8` + `entities_state_idx` migrations.** Discrete PR — schema changes get their own review.
   - Plugin implementations (memory + entityVersion immutability godoc, sqlite, postgres).
   - Conformance and parity tests (§7).
   - E2E test.
   - OpenAPI + help + config + COMPATIBILITY.

4. **cyoda-go-cassandra PR** — independent timeline. SPI pin bump to v0.8.0 (combined bump for tx-state + new interfaces) + `Iterable` implementation wiring the existing shard executor into an iterator (D22). Strictly in scope per memory `feedback_courtesy_pr_scope`.

## 10. Non-goals

- **Numeric aggregations beyond Tier 1+2** — mode and median deferred.
- **Returning entity bodies** — `/search/*`.
- **Cross-model joins** — out of scope.
- **`having` post-aggregation predicates** — forward-compat noted, not built.
- **Array fan-out in `groupBy`** — out of scope; surface left open.
- **Auto-created data-field expression indexes per dimension on postgres** — out of scope; documented as caller responsibility. The one canonical state index (D19) ships in this PR.
- **Postgres `Searcher` implementation** — translator is reusable substrate but `Searcher` impl is its own PR.
- **Streaming `/search/*`** — `Iterable` is for grouped-stats; future streaming-search is a separate design.
- **Distinguishing absent from literal-null in `groupKey`** — out of scope per D4.
- **Sqlite-native stdev pushdown** — out of scope per D9.
- **Cassandra `GroupedAggregator`** — not feasible against CQL semantics for arbitrary JSONPaths.
- **Cassandra per-shard streaming of `ListEntityIDs`** — current implementation materializes per-shard ID lists; refactoring to a channel/iterator is a follow-up outside #299's scope (D22).
- **Per-shard parallel fan-out on cassandra `Iterate`** — sequential shard walk in v1 (D22).
- **PL/pgSQL `cyoda_try_float8`** — explicitly chose SQL-language form per D21.

## 11. Acceptance

- `POST /api/entity/stats/{entityName}/{modelVersion}/query` returns grouped counts and Tier 1+2 aggregations for: single data-field group-by; `state` group-by; multi-dim `variantId × state`; with/without `condition`; with `pointInTime` (including correct exclusion of deletion-marker versions); in-tx (routes through streaming-tally with RYW on memory).
- No entity envelopes ever materialized server-side as a full slice: postgres + sqlite push GROUP BY into SQL; memory snapshots `*entityVersion` pointers (D20) and walks them; cassandra streams per-entity materialization within the existing per-shard ID list (D22).
- Filter pushdown observable per backend via parity test: narrow predicate yields `≈ matching` entities. Postgres state grouping/filtering hits the new `entities_state_idx` (D19) via `EXPLAIN` assertion.
- Auth, tenant scoping, Condition validation, and error model identical to `/search/*` where shared. Tenant isolation verified.
- `CYODA_STATS_GROUP_MAX` ceiling enforced consistently; exceeded returns 422 with the ceiling. Detection is deterministic via `LIMIT MaxBuckets+1` rowcount (D17).
- Numerical contracts: postgres `cyoda_try_float8` row-by-row table verified (D21); Welford vs `STDDEV_SAMP` parity within **1e-9 relative error** on the low-variance/high-mean stress case (D9); `n<2` → `null` on both paths; sqlite stdev pushdown declined deterministically.
- Postgres Filter→SQL translator (D16) is parity-tested against sqlite for the shared operator set.
- Iterator contract verified per backend: `Err()` sticky, `Close()` idempotent, ctx cancellation observed.
- `buildGroupKey` (D18) collision-free under adversarial inputs.
- Backend without either capability returns 501.
- `cyoda help` updated; OpenAPI updated; config env vars documented; COMPATIBILITY.md updated alongside the SPI pin bump.
- Conformance parity tests cover the §7 matrix. Race detector clean before PR per memory `feedback_race_testing_discipline`.
