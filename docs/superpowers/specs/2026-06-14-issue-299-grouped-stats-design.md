# Grouped entity statistics query — design

| Field | Value |
|---|---|
| Issue | [#299](https://github.com/Cyoda-platform/cyoda-go/issues/299) |
| Target milestone | v0.8.0 (cyoda-go), v0.8.0 (cyoda-go-spi) |
| Spec date | 2026-06-14 |
| Review iterations | 4 (architecture · iterator filter-awareness · perf/memory/correctness/concurrency · cassandra-prescription removal + OSS plugin clarity) |
| Status | Draft, pending review |
| Related repos | `cyoda-go`, `cyoda-go-spi`. Implementation in `cyoda-go-cassandra` (commercial backend) is tracked in that repo's own follow-up issue and is intentionally not prescribed here. |

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
| D3 | Optional `limit` request field (top-N by `(count desc, groupKey lex)`). `> CYODA_STATS_GROUP_MAX` → 400. | Display convenience; orthogonal to safety bound. |
| D4 | Missing JSONPath value → JSON `null` bucket key. Conflates with literal-`null` values. Non-scalar extracted values also coerce to `null` for cross-backend consistency. | Deliberate v1 simplification. |
| D5 | `groupBy` accepts scalar JSONPaths only. Array projection → 400. Bracket-quoted property access normalized to dotted form. | Array fan-out is a follow-up. |
| D6 | Add two new SPI capabilities: `Iterable` (filter-aware streaming) and `GroupedAggregator` (native GROUP BY pushdown). Both optional via type assertion. Backend implementing neither → 501 `NOT_IMPLEMENTED_BY_BACKEND`. | The streaming primitive exists because the cyoda-go service layer needs a fallback path for three cases: in-tx grouped-stats (where pushdown would miss buffered writes — D11); non-pushdownable conditions (where `ConditionToFilter` can't represent the user's `Condition` — D15); and sqlite stdev (where the SQL formula is numerically unsafe — D9). Filter-aware so the fallback path doesn't full-scan when the condition is narrowing (D14). `GroupedAggregator` exists because SQL backends can answer the whole query in one roundtrip. |
| D7 | Path: `POST /api/entity/stats/{entityName}/{modelVersion}/query`. | More RESTful — model is the resource, `query` is the action. |
| D8 | Aggregations Tier 1+2 in v1: `sum`, `avg`, `min`, `max`, `stdev`. Sample stdev (n−1 denominator); v1 op name is `stdev`. Mode/median deferred. | Tier 1+2 marginal cost is low; mode/median introduce unbounded intermediate state. |
| D9 | Stdev numerical contract: Welford's online algorithm on streaming-tally. Postgres pushes `STDDEV_SAMP`. Sqlite returns `ErrAggregationNotPushdownable` for stdev. Parity tolerance: relative error ≤ 1e-9 between backends. `n < 2` → `null` on both paths. | Welford and `STDDEV_SAMP` are both numerically stable but not bit-identical. |
| D10 | Stdev pushdown asymmetry handled by `ErrAggregationNotPushdownable`: a `GroupedAggregator` may decline a specific request shape, signalling fall-through to streaming-tally. | Coarse capability check; fine per-request opt-out. |
| D11 | In-tx grouped-stats is **supported** via the streaming-tally path. Memory backend in-tx path overlays `tx.Buffer` and excludes `tx.Deletes` (RYW-correct, matching the actual `/search/*` precedent at `internal/domain/search/service.go:118-136`). Pushdown skipped when `spi.GetTransaction(ctx) != nil`. | RYW is what the precedent actually does. |
| D12 | `groupKey` in responses is an ordered array of `{path, value}` pairs. | OpenAPI-typeable; friendly for typed clients; preserves request order. |
| D13 | Target v0.8.0. cyoda-go-spi v0.8.0 tag not yet published; additive SPI changes fold alongside #16. | v0.8.0 is not feature-frozen. |
| D14 | `Iterable.Iterate(model, filter, opts)` takes a Filter parameter. Iterator yields entities matching the filter; plugins push what they can and apply residual inside `Next()`. | Without it, the streaming-tally path full-scans regardless of how narrow the condition is. |
| D15 | When `search.ConditionToFilter` errors (function conditions, future operators not yet translated), the service passes a zero-value `Filter` to `Iterate` and re-applies `match.Match` per yielded entity. Otherwise the iterator's yielded set is authoritative. | One source of truth per pluggable surface. |
| D16 | Postgres gets a new Filter→SQL translation layer as part of this change. Greedy AND / conservative OR, JSONB ops. Reusable substrate for a future postgres `Searcher`; that work is out of scope here. | Postgres has no `Searcher` today; without this layer, postgres `Iterate` would be filter-blind. |
| D17 | Cardinality detection via `LIMIT MaxBuckets+1` without `ORDER BY` is correct. SQL `LIMIT N` returns `min(actual, N)` rows; rowcount == `MaxBuckets+1` ⇔ actual cardinality > `MaxBuckets`. Adding `ORDER BY` would force a full sort over the group set for no contract benefit. | Verified against postgres planner semantics. |
| D18 | `buildGroupKey` encoding: length-prefixed byte concatenation — `len(v_0)|v_0|len(v_1)|v_1|…`. Used as a Go `string` map key. | Prevents collision under naive `strings.Join`. |
| D19 | Ship one canonical postgres expression index in this PR: partial `(tenant_id, model_name, model_version, (doc->'_meta'->>'state'))` `WHERE NOT deleted`. Data-field indexes are caller responsibility. | State-based grouping/filtering is the SIOMS driving case. |
| D20 | Memory snapshot is `[]*entityVersion` pointers (8 bytes per matching entity). Relies on the invariant that `entityVersion` fields are immutable post-publish; this PR documents the invariant in `entityVersion`'s godoc. | 1 GB vs 80 MB per-request memory at 10M entities × 100-byte payloads. |
| D21 | Postgres `cyoda_try_float8(text)` ships as `LANGUAGE sql IMMUTABLE PARALLEL SAFE` with regex CASE WHEN + `NULLIF` on Infinity. No PL/pgSQL EXCEPTION block. | PL/pgSQL EXCEPTION opens a subtransaction per row — ~30M subtransactions on a 10M-row × 3-aggregation scan. |
| D22 | **Cassandra implementation is not prescribed by this spec.** The SPI surface is designed for cyoda-go-internal reasons (D6); cassandra implements it on its own schedule, with its own design choices, tracked in a follow-up issue in `cyoda-go-cassandra`. cyoda-go ships with the standard capability check: a backend implementing neither `Iterable` nor `GroupedAggregator` returns 501. | Cross-repo separation of concerns: cyoda-go defines the contract; each plugin team owns its implementation. |

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
- **`aggregations`** (optional, 0..N): per entry: `op` ∈ {`sum`, `avg`, `min`, `max`, `stdev`}, `field` scalar JSONPath, optional `as` alias. When `as` is omitted, server synthesizes `<op>_<field>`. Server dedupes identical `(op, field)` pairs. Aliases colliding on distinct `(op, field)` pairs → 400.
- **`pointInTime`** (optional RFC 3339): historical snapshot; default = now.
- **`limit`** (optional positive int, `≤ CYODA_STATS_GROUP_MAX`): top-N. `> CYODA_STATS_GROUP_MAX` → 400. Default = unlimited.

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

**Total order for response sorting** (D12, D18): primary key is `count` descending; tiebreaker is groupKey lex defined as element-wise comparison of the `groupKey` array. Per entry: `null` < any string; strings compared bytes-wise. Backend-independent.

### Errors

| Status | Code | Trigger |
|---|---|---|
| 400 | `MALFORMED_REQUEST` | JSON parse failed |
| 400 | `UNKNOWN_MODEL` | path does not resolve for this tenant |
| 400 | `MISSING_GROUP_BY` | `groupBy` empty or missing |
| 400 | `INVALID_GROUP_BY_PATH` | empty entry, or array projection |
| 400 | `DUPLICATE_GROUP_BY` | duplicate entries after normalization |
| 400 | `INVALID_AGGREGATION_OP` | unknown `op` |
| 400 | `INVALID_AGGREGATION_FIELD` | aggregation `field` empty or contains array projection |
| 400 | `DUPLICATE_AGGREGATION_ALIAS` | two aliases collide on distinct `(op, field)` pairs |
| 400 | `INVALID_OPERATOR` / `INVALID_CONDITION` / depth-exceeded | propagated from existing search validators |
| 400 | `INVALID_POINT_IN_TIME` | unparseable RFC 3339 |
| 400 | `INVALID_LIMIT` | non-positive int, or `> CYODA_STATS_GROUP_MAX` |
| 401 | (standard) | missing/invalid Bearer |
| 413 | (standard) | request body > 10 MiB |
| 422 | `GROUP_CARDINALITY_EXCEEDED` | result buckets exceed `CYODA_STATS_GROUP_MAX` |
| 501 | `NOT_IMPLEMENTED_BY_BACKEND` | backend implements neither `Iterable` nor `GroupedAggregator` |
| 500 | (with ticket UUID) | internal/driver errors |

In-transaction calls are not an error — they route through the streaming-tally path via `Iterable`, with RYW semantics (D11).

## 4. Service-layer dispatch

```go
import "github.com/cyoda-platform/cyoda-go/internal/domain/search"

// 1. Native pushdown when available, condition pushdownable, and not in-tx.
if ga, ok := store.(spi.GroupedAggregator); ok && spi.GetTransaction(ctx) == nil {
    flt, terr := search.ConditionToFilter(req.Condition)
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

// 2. Streaming fallback. Filter-aware: iterator yields entities matching the
//    filter when ConditionToFilter succeeded; otherwise pass match-all and
//    re-apply match.Match per yielded entity (D15).
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
defer iter.Close()  // idempotent (§5)

acc := newAccumulators(req)
for iter.Next() {
    e := iter.Entity()
    if !filterOK && req.Condition != nil &&
       !match.Match(req.Condition, e.Data, e.Meta) {
        continue
    }
    k := buildGroupKey(req.GroupBy, e)
    // Trip on the (MaxBuckets+1)th distinct key. SQL pushes LIMIT MaxBuckets+1
    // for the same overflow detection.
    if !acc.has(k) && acc.len() >= maxGroupCardinality {
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
// stdev_samp = sqrt(m2 / (n - 1))  (when n >= 2; n < 2 → null)
```

Post-processing: heap top-N when `Limit > 0 && Limit < len(buckets)/2`; otherwise full sort. Backend-independent observable ordering.

## 5. SPI delta (cyoda-go-spi v0.8.0)

Additive. No breaking changes. Lands on `cyoda-go-spi` main on top of `c301c0e`.

```go
// Streaming iteration over a model's entities matching a Filter.
//
// Semantics:
//   - Plugins push pushable parts of the filter into storage; residual is
//     applied inside Next() before yielding.
//   - A zero-value Filter means "yield all entities for the model".
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

type Iterator interface {
    Next() bool
    Entity() *Entity
    Err() error
    Close() error
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
    GroupExprDataPath
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
    GroupKey     []GroupKeyEntry
    Count        int64
    Aggregations map[string]any
}

var ErrGroupCardinalityExceeded   = errors.New("group cardinality exceeded ceiling")
var ErrAggregationNotPushdownable = errors.New("aggregation not pushdownable on this backend")
```

`internal/match` gains a new sibling of `Match`:

```go
// MatchFilter evaluates an SPI Filter against an entity. Filter is the
// pushdown-friendly subset of predicate.Condition used by GroupedAggregator,
// Iterable, and (existing) Searcher. Used by memory plugin's Iterate to apply
// filters inside Next().
func MatchFilter(f spi.Filter, data []byte, meta spi.EntityMeta) bool { ... }
```

Forward-compat: see §8.

## 6. OSS plugin implementations

The three plugins shipped in this repository — memory, sqlite, and postgres — implement both `Iterable` and `GroupedAggregator`. Below, each plugin's design is explained in plain prose: when the SQL layer does the heavy lifting and when we fall back to Go-side work; how memory stays bounded; how an active transaction is handled.

### 6.1 Memory plugin

**The setup.** The memory backend keeps every entity in an in-memory map, protected by a single read-write mutex. Reads acquire the read lock; writes take the exclusive lock. For grouped-stats we want two things at once: bounded memory regardless of model size, and minimal blocking of writers while a read is in progress.

**The naive approach is wrong.** Holding the read lock for the duration of the iteration would block every writer until the grouped-stats request completes. That's tolerable for a thousand-entity model and unacceptable for ten million. So we do *snapshot-then-iterate*:

1. Acquire the read lock.
2. Walk the tenant's entity map once, appending a pointer (`*entityVersion`) to a slice for every version that matches the requested model, isn't deleted, and is visible at the requested point-in-time.
3. Release the read lock.
4. Hand the slice to the iterator. It walks lock-free.

This works because **version records in the memory plugin are immutable once published.** Saving a new version appends to the per-entity version slice; the old version's bytes are never overwritten. So a slice of `*entityVersion` pointers stays valid for as long as we hold it: the bytes those pointers address are no longer reachable from the live map after some operations, but no one mutates them either. This PR adds explicit godoc on `entityVersion` documenting the invariant and a one-line comment in `saveUnlocked` reminding future maintainers (D20).

**Memory cost.** The snapshot is one pointer per matching entity — 8 bytes each. For a 10-million-entity model, that's 80 MB of pointer slice during the request. The alternative (deep-copying entity payloads on capture) would be 10–100× larger.

**Pushdown means something different here.** There's no SQL layer; everything is Go. But filter-aware iteration still pays off. The iterator's `Next()` evaluates the filter against the current entity before yielding it. Entities that don't pass the filter are skipped without ever being seen by the service-layer tally accumulator. For a request like "count items where `state = 'available'` grouped by `variantId`" where most entities are not in that state, the iterator still has to look at every snapshot entry (no index exists), but it only yields and tallies the matching ones — saving one `match.MatchFilter` call and one map operation per non-matching entity. When the filter is the user's whole condition (the common case), the tally only sees the rows that count.

For the native `GroupedAggregator` path — which memory also implements — we skip the iterator entirely and walk the snapshot inline, tallying into a `map[groupKey]bucketState` as we go. The accumulator map is bounded by the cardinality ceiling: if it grows beyond `CYODA_STATS_GROUP_MAX` distinct buckets we abort with a 422 before continuing the walk.

**In a transaction.** Suppose a caller is mid-transaction, has called `Save` on a few new entities (their changes sit in `tx.Buffer` uncommitted), called `Delete` on others (their IDs sit in `tx.Deletes`), and then calls grouped-stats. They expect to see their own writes. This is the read-your-writes property.

The memory plugin's existing `GetAll` already handles this in the `/search/*` fall-through path: it builds the base set, overlays `tx.Buffer`, and removes anything in `tx.Deletes`. Our `Iterate` in-tx implementation does the same — capture the base snapshot under the read lock, then apply the tx buffer overlay and delete-mask before constructing the lock-free iterator. Outside a transaction (the common case), none of the overlay work runs.

The native `GroupedAggregator` path is **skipped inside a transaction** — service-layer dispatch checks `spi.GetTransaction(ctx) != nil` and routes straight to `Iterate`. This matches the actual `/search/*` precedent (`internal/domain/search/service.go:118-136`) and avoids the awkward problem of pretending an aggregation can be answered without observing the buffered writes.

**Limits to be honest about.** The snapshot walk is `O(tenant entities)`, not `O(model entities)`. The underlying map is `tenantID → entityID → versions`, not partitioned by model. So a tenant with many models pays a constant per-request walk cost proportional to all their entities, even when one specific model is queried. The help topic documents this for operators sizing memory backends.

### 6.2 SQLite plugin

**The story in one line.** The sqlite plugin can usually do the entire grouped-stats query in one SQL statement; when it can't, the same plugin streams rows through an iterator with the same filter-pushdown machinery, and the service layer tallies in Go.

**The setup.** SQLite stores entities as rows in an `entities` table; each row's `data` column holds the entity payload as JSON bytes, and `meta` holds the lifecycle metadata. A grouped-stats request like "count items per `(variantId, state)` where `state != 'shipped'`" maps cleanly to:

```sql
SELECT json_extract(data, '$.variantId') AS gk_0,
       json_extract(meta, '$.state')     AS gk_1,
       COUNT(*) AS cnt
FROM entities
WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND NOT deleted
  AND json_extract(meta, '$.state') != ?
GROUP BY json_extract(data, '$.variantId'), json_extract(meta, '$.state')
LIMIT MaxBuckets + 1;
```

The database engine does all the work in C: it walks the table (using whatever indexes exist), builds the group-by hash table in process, and returns only the aggregated rows. The application sees one row per bucket. No entity payloads cross the boundary.

**Pushdown is variable, by design.** The user's `Condition` DSL is rich (group, lifecycle, simple, function). Some shapes translate cleanly to SQL `WHERE` clauses; others don't. The existing `planQuery`/`dissect` planner (`plugins/sqlite/query_planner.go`) already handles this exact split for `/search/*`. It walks the `Filter` tree and returns:

- `whereClause` — the part of the filter the database can evaluate, in SQL syntax.
- `args` — the bound parameter values for that clause.
- `postFilter` — the part that has to be evaluated in Go after the rows come back.

For `AND` groups it's *greedy*: every child that can be pushed is pushed; non-pushable children go to the residual. For `OR` groups it's *conservative*: if any child can't be pushed, the whole group goes to the residual. The conservative `OR` is the only safe policy — pushing only some alternatives of a disjunction silently changes the query's meaning.

**Our `GroupedAggregate` follows this dissection.** If the planner produces a fully-pushable filter (`postFilter == nil`), we emit the single `SELECT … FROM entities WHERE … GROUP BY … LIMIT MaxBuckets+1` query and we're done. If it produces a residual, we can't safely aggregate in the database — the `GROUP BY` would be over rows surviving the partial `WHERE`, not the rows actually matching the user's condition. So we return `ErrAggregationNotPushdownable` and the service layer falls through to streaming-tally via `Iterate`.

**`Iterate` uses the same planner, with one extra step.** It pushes what it can to `WHERE`, then captures the residual to evaluate per row inside the iterator's `Next()` before yielding. Concretely:

```go
plan := planQuery(filter)
rows, err := s.db.QueryContext(ctx, "SELECT data, meta FROM entities WHERE … "+plan.whereClause, args…)
// inside iterator.Next():
for rows.Next() {
    var e spi.Entity
    rows.Scan(&e.Data, &e.Meta)
    if plan.postFilter != nil && !match.MatchFilter(*plan.postFilter, e.Data, e.Meta) {
        continue  // residual filter rejected this row
    }
    return &e, nil  // yield
}
```

The Go-side residual eval is the same shape `/search/*` uses today.

**Stdev is the odd op out.** SQLite has no native `STDDEV` function in the standard build, and the textbook one-pass SQL formula `√(sum_sq/n − mean²)` suffers catastrophic cancellation when the variance is small relative to the mean. That's exactly the regime for monetary valuations (the SIOMS valuation rollup case): cost prices clustered around $50 with a standard deviation of a few dollars lose 3+ decimal digits of precision per order of `mean²/variance`. We refuse to ship that. So sqlite's `GroupedAggregate` deliberately returns `ErrAggregationNotPushdownable` whenever the request includes `stdev`, and the service layer falls through to streaming-tally — which runs Welford's online algorithm in Go (numerically stable for any data regime). The other Tier 1 ops (sum, avg, min, max) push down natively.

**Memory efficiency.** SQL pushdown returns at most `MaxBuckets + 1` rows — a tiny result set. `Iterate` uses a `sql.Rows` cursor; the driver fetches in batches; the application reads one row at a time, applies any residual filter, builds the group key, updates the accumulator. The accumulator map is bounded by `MaxBuckets` (the service trips on `len(acc) > MaxBuckets`). At no point does the application hold more than one batch of rows plus the accumulator.

The full-table-scan question. SQLite uses indexes when they exist and the planner picks them. The shipped schema already has indexes on `(tenant_id, model_name, model_version)` covering the WHERE-clause prefix. For hot grouping dimensions (`variantId`, etc.), callers add `CREATE INDEX entities_variantid_idx ON entities (json_extract(data, '$.variantId'))`; documented in the help topic with recipes.

**In a transaction.** Inside a tx, the service layer skips the `GroupedAggregator` path and routes the call to `Iterate`. The sqlite plugin's `Iterate` selects from the `entity_versions` snapshot table with `submit_time <= tx.SnapshotTime` — the same bi-temporal pattern that the existing sqlite read paths use for transactional reads. This makes pointInTime queries work for free (the same SQL handles both): pointInTime stats select from `entity_versions WHERE submit_time <= ?`, and in-tx stats use the same query with the tx's snapshot time.

### 6.3 Postgres plugin

**The story in one line.** Postgres has the most powerful native aggregation surface of our backends; the work in this PR is making sure the user's `Condition` DSL gets translated to a postgres `WHERE` clause so the database can narrow rows before grouping, and pinning down the numeric coercion so non-numeric values in JSONB don't crash the whole query.

**Why a new translator.** Postgres currently has **no `Searcher`** — `/search/*` on postgres falls back to GetAll-then-filter-in-Go, exactly the slurp-then-tally shape grouped-stats can't afford to inherit. Without a Filter→SQL translator, the grouped-stats `Iterate` would also fall back to GetAll. So this PR adds the missing piece (D16): a translator that mirrors sqlite's `planQuery`/`dissect` but emits postgres JSONB operators (`->` for nested objects, `->>` for text extraction).

The translator follows the same pushdown policy as sqlite — greedy `AND`, conservative `OR`. Each leaf `Filter` node maps to a `WHERE` predicate:

- `state = 'available'` becomes `doc->'_meta'->>'state' = $1`.
- `costPrice > 50` becomes `cyoda_try_float8(doc->>'costPrice') > $1`.
- Strings compare as text; numerics go through `cyoda_try_float8`.

The translator is reusable substrate for a future postgres `Searcher` — but that follow-on work is explicitly out of scope here.

**The numeric coercion problem.** Postgres's raw `::float8` cast **raises an exception** on a bad input. If a single row in a 10-million-row scan has `"costPrice": "n/a"`, the whole query aborts. We need a try-cast that returns NULL on parse failure so the bad value gets silently skipped by the aggregate (matching standard SQL "NULL doesn't count" semantics).

The obvious implementation — PL/pgSQL with `BEGIN ... EXCEPTION WHEN invalid_text_representation THEN RETURN NULL; END` — is **prohibitively expensive**. Each EXCEPTION block opens a subtransaction; on a 10M-row scan with three numeric aggregations, that's 30 million subtransactions. Performance hits the floor.

So we ship `cyoda_try_float8` as a SQL function (D21):

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

A regex pre-filter rejects anything that obviously isn't a number; only strict-numeric strings reach the `::float8` cast (which then succeeds, except for the case of `'1e500'` which would yield `Infinity` — the outer `NULLIF` strips that too). Zero subtransactions; cost is one regex match per row. `IMMUTABLE PARALLEL SAFE` lets the postgres planner inline and parallelize.

**`GroupedAggregate` pushes the full Tier 1+2 set, including stdev.** Unlike sqlite, postgres has `STDDEV_SAMP` natively and it's numerically stable. The query:

```sql
SELECT doc->>'variantId'                                AS gk_0,
       doc->'_meta'->>'state'                           AS gk_1,
       COUNT(*)                                         AS cnt,
       SUM(cyoda_try_float8(doc->>'costPrice'))         AS sum_x,
       AVG(cyoda_try_float8(doc->>'costPrice'))         AS avg_x,
       STDDEV_SAMP(cyoda_try_float8(doc->>'costPrice')) AS stdev_x
FROM entities
WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted
  AND <pushed-down filter>
GROUP BY doc->>'variantId', doc->'_meta'->>'state'
LIMIT <MaxBuckets + 1>;
```

Residual-filter rule same as sqlite: when the translator produces a non-empty residual, `GroupedAggregate` returns `ErrAggregationNotPushdownable` and the service layer falls through to `Iterate`-streaming with the pushable part of the filter; the iterator applies the residual per row.

**Avoiding full table scans.** The shipped postgres schema has only `(tenant_id, model_name, model_version)` indexed. Without a deeper index, a `state = 'available'` clause still partial-scans the entire model. State-based grouping and filtering is the SIOMS driving case (and the case most consumers will hit first), so this PR ships one canonical expression index (D19):

```sql
CREATE INDEX IF NOT EXISTS entities_state_idx
ON entities (tenant_id, model_name, model_version, (doc->'_meta'->>'state'))
WHERE NOT deleted;
```

State queries now hit the index. Data-field expression indexes (`(doc->>'variantId')`, etc.) remain caller responsibility, with `CREATE INDEX` recipes in the help topic — the application-specific space is too varied to ship one-size-fits-all in our migration.

**Memory efficiency.** SQL pushdown returns at most `MaxBuckets + 1` rows. `Iterate` uses pgx's `Rows` cursor with row-at-a-time fetch inside `Next()`. The accumulator map is bounded by `MaxBuckets`. As with sqlite, the application never holds more than one batch of rows plus the accumulator.

**In a transaction.** Inside a tx, the service layer skips the `GroupedAggregator` path and routes to `Iterate`. The postgres plugin's `Iterate` reads from the bi-temporal `entity_versions` table with `valid_time <= tx.SnapshotTime AND transaction_time <= CURRENT_TIMESTAMP` — the same pattern `GetAsAt` already uses. One critical addition: postgres marks deletions by inserting a new version row with `_meta.deleted = true` (rather than physically removing the row). So the pointInTime path adds `AND (doc->'_meta'->>'deleted')::boolean IS NOT TRUE` to exclude deletion-marker versions. Without that filter, point-in-time stats would count entities that were already deleted at the requested instant. (This was an explicit bug caught and fixed during review iteration 3.)

The pointInTime SQL is the same shape for non-tx callers passing `pointInTime` and in-tx callers using the tx's snapshot time — different `tx.SnapshotTime` source, identical SQL pattern.

### 6.4 Other backends

Other backends — including `cyoda-go-cassandra` (the commercial backend) and any out-of-tree plugins — implement the SPI on their own timelines, with their own design choices, in their own repositories. Because both new SPI interfaces are optional via type assertion, an unmodified plugin keeps compiling against the new SPI tag; cyoda-go's capability check returns 501 `NOT_IMPLEMENTED_BY_BACKEND` for any backend that implements neither interface. This is honest graceful degradation: existing deployments that haven't adopted the new SPI continue to work for every other endpoint, and the new endpoint surfaces a clean error until the backend catches up.

The cassandra plugin's grouped-stats implementation will be tracked as a follow-up issue in `cyoda-go-cassandra`, referencing this spec for the contract. Cyoda-go does not prescribe its implementation strategy.

## 7. Conformance, OpenAPI, help, e2e

### Cross-backend conformance (`internal/e2e/parity/registry.go`)

Parity tests describe SPI-contract behavior — what any backend implementing `Iterable` or `GroupedAggregator` must do — not plugin-specific optimizations. They are picked up automatically by every backend, including cassandra when its plugin lands.

| Case | Cover |
|---|---|
| single data-field group-by | basic happy path |
| `state` group-by | parity with existing `/entity/stats/states` |
| multi-dim `$.variantId × state` | cartesian buckets, response ordering |
| with vs without `condition` | reuse of search DSL |
| with `pointInTime` (including a deleted entity at the requested instant) | historical snapshot; deletion-marker versions excluded |
| in-tx call routes through streaming-tally | matches `/search/*` precedent; no error |
| in-tx grouped-stats sees own uncommitted writes (RYW) | applicable to backends with tx-buffer overlay |
| sum / avg / min / max | Tier 1 aggregations |
| stdev — wide dynamic range (8+ orders of magnitude) | nominal numerical sanity |
| stdev — low-variance / high-mean (monetary valuation regime) | within 1e-9 relative error between any two backends |
| stdev with `n=1` bucket | both → `null` |
| stdev with empty-numeric bucket | both → `null` |
| non-numeric values mixed in | silently skipped |
| runtime non-scalar extracted value (object/array at path) | bucketed under `null`, consistent across backends (D4) |
| count-only request (no `aggregations` field) | response omits `aggregations` |
| explicit `aggregations: []` | equivalent to count-only |
| OpenAPI schema vs DTO check | DTO `omitempty` and OpenAPI schema agree |
| groupBy contains `[*]` / `[N]` / `[?(...)]` | 400 `INVALID_GROUP_BY_PATH` |
| groupBy bracket-quoted scalar (`$.['variantId']`) | accepted; normalized to dotted form |
| aggregation `field` contains `[*]` / `[N]` / `[?(...)]` | 400 `INVALID_AGGREGATION_FIELD` |
| duplicate groupBy | 400 `DUPLICATE_GROUP_BY` |
| identical `(op, field)` aggregation pair (no `as`) | silently deduped |
| distinct `(op, field)` pair colliding on explicit `as` | 400 `DUPLICATE_AGGREGATION_ALIAS` |
| `limit > CYODA_STATS_GROUP_MAX` | 400 `INVALID_LIMIT` |
| **filter pushdown observable — every backend** | narrow predicate → iterator yields ≈ matching, not model size (via counting wrapper in parity harness) |
| non-pushdownable condition (function condition) | iterator gets match-all; service applies `match.Match`; result identical to pushdown path |
| partial pushdown (residual filter) | iterators yield rows matching pushable part; iterator's `Next()` applies residual before yielding |
| cardinality detection determinism | overflowing request consistently returns 422 |
| cardinality ceiling exceeded | 422 `GROUP_CARDINALITY_EXCEEDED` |
| backend with neither capability | 501 `NOT_IMPLEMENTED_BY_BACKEND` (mock backend) |
| tenant isolation | tenant A's request on a model also-owned-by tenant B sees only tenant A buckets |
| SQL-injection surface — groupBy path | `'`, `;`, `--`, `\\`, `\n`, NUL → 400 |
| SQL-injection surface — aggregation field | same surface, same validator, same 400 |
| iterator contract — `Err()` sticky | inject transient driver error mid-stream; `Err()` returns it; `Next()` stays false; no retry |
| iterator contract — `Close()` idempotent | double `Close()` returns safely |
| iterator contract — ctx cancellation observed | cancel ctx mid-stream; `Next()` returns false; `Err()` is ctx error or driver-wrapped |
| `buildGroupKey` collision-free | adversarial inputs produce distinct keys (D18) |
| concurrent writes + grouped-stats reads | snapshot consistency: stats result reflects the snapshot taken at request entry; writers do not block on the iterator |

### Plugin-local tests (not parity)

Tests that probe a specific backend's chosen mechanism live in that plugin's test suite, not the cross-backend parity registry:

- **Sqlite**: stdev request returns `ErrAggregationNotPushdownable` (mechanism-specific).
- **Postgres**: translator parity vs sqlite translator on the shared operator set; `cyoda_try_float8` row-by-row coercion table; `entities_state_idx` is used (`EXPLAIN` assertion); in-tx grouped-stats uses `entity_versions` snapshot with deletion-marker filter.
- **Memory**: snapshot-then-iterate writer-non-blocking under concurrent load.

### E2E (`internal/e2e/grouped_stats_test.go`)

One e2e test hitting the full HTTP stack with the postgres testcontainer. Doesn't enumerate every parity case — proves wiring end-to-end.

### OpenAPI (`api/openapi.yaml`)

New path `POST /api/entity/stats/{entityName}/{modelVersion}/query`. New schemas: `GroupedStatsRequest`, `GroupedStatsBucket`, `GroupKeyEntry` (`{path: string, value: string | null}` — schema-typed, not `any`), `GroupExpr`, `AggregationExpr`, `AggregateOp`. `aggregations` on `GroupedStatsBucket` is `omitempty` Go-side; parity test verifies absent-vs-empty-object distinction matches the schema.

### `cyoda help` (`cmd/cyoda/help/content/`)

Updates to the search/stats help topic:

- Request and response examples (count-only, with aggregations, with pointInTime, multi-dim).
- Cardinality ceiling, `limit` upper bound, env var.
- JSONPath scalar-only restriction and rationale.
- Aggregation operators, sample-stdev semantics, `n<2` → null boundary.
- Postgres: state grouping/filtering is index-backed out of the box (`entities_state_idx`); for hot data-field dimensions, callers add `CREATE INDEX ... ON entities ((doc->>'variantId'))`.
- Sqlite: `CREATE INDEX` recipes for hot dimensions on the JSON path.
- Memory: snapshot cost is `O(tenant entities)`, not `O(model entities)`.
- In-tx behavior (routes through streaming-tally; RYW-correct on memory; pushdown skipped on sqlite/postgres).
- Non-scalar runtime values bucket under `null`.

### Config (Gate 4)

`CYODA_STATS_GROUP_MAX` env var (default 10000) added to `cmd/cyoda/help/content/config/*.md`, `README.md`, and `DefaultConfig()`. All three updated together.

### COMPATIBILITY.md (Gate 4)

Updated when the cyoda-go-spi v0.8.0 pin lands and when the chart `appVersion:` bumps (auto-PR; human-reviewed).

## 8. Forward-compatibility hooks

Strict count + Tier 1+2 in v1. Surfaces left open: `having`, `mode`/`median` (each needs its own design pass — unbounded intermediate state), `stdev_pop`, array-projection in `groupBy`, paging via `offset`, `nullPolicy` to distinguish absent from literal-null. Postgres `Searcher` is reusable substrate (D16) but its own separate PR.

## 9. Release sequencing

1. **cyoda-go-spi PR** — additive: all the new types from §5. `Iterate` carries the filter parameter from the start. Folds into the v0.8.0 SPI tag alongside #16 (tx-state sentinels).

2. **cyoda-go-spi v0.8.0 tag** — once SPI scope settles. Adding both new interfaces is type-assertion-only; any plugin can pin v0.8.0 SPI without implementing either.

3. **cyoda-go PR series on `release/v0.8.0`**:
   - SPI pin bump (one isolated commit).
   - Handler + DTO + validation.
   - `internal/match.MatchFilter` + service-layer dispatch + accumulators (Welford) + post-processing.
   - Postgres Filter→SQL translator (`plugins/postgres/query_planner.go`). Discrete PR — independently reviewable substrate.
   - Postgres `cyoda_try_float8` + `entities_state_idx` migrations. Discrete PR — schema changes get their own review.
   - Plugin implementations (memory + `entityVersion` immutability godoc, sqlite, postgres).
   - Conformance and parity tests (§7).
   - E2E test.
   - OpenAPI + help + config + COMPATIBILITY.

4. **Other plugins (cassandra, out-of-tree)** — tracked in their own repos on their own timelines. Not part of this milestone.

## 10. Non-goals

- Numeric aggregations beyond Tier 1+2 — mode and median deferred.
- Returning entity bodies — `/search/*`.
- Cross-model joins.
- `having` post-aggregation predicates — forward-compat noted, not built.
- Array fan-out in `groupBy` — surface left open.
- Auto-created data-field expression indexes per dimension on postgres — caller responsibility. The one canonical state index (D19) ships.
- Postgres `Searcher` implementation — translator is reusable substrate but `Searcher` impl is its own PR.
- Streaming `/search/*` — separate design with its own SPI shape decisions.
- Distinguishing absent from literal-null in `groupKey` — out of scope per D4.
- Sqlite-native stdev pushdown — out of scope per D9.
- **Prescription of any out-of-tree plugin's implementation strategy** (D22) — including cassandra. cyoda-go ships with the standard capability check; each plugin team owns its own design.
- PL/pgSQL `cyoda_try_float8` — explicitly chose SQL-language form per D21.

## 11. Acceptance

- `POST /api/entity/stats/{entityName}/{modelVersion}/query` returns grouped counts and Tier 1+2 aggregations for: single data-field group-by; `state` group-by; multi-dim; with/without `condition`; with `pointInTime` (deletion-marker versions correctly excluded); in-tx (routes through streaming-tally with RYW on memory).
- No entity envelopes ever materialized server-side as a full slice in any in-tree plugin: postgres + sqlite push GROUP BY into SQL; memory snapshots `*entityVersion` pointers (D20) and walks them.
- Filter pushdown observable per in-tree backend via parity test: narrow predicate yields `≈ matching` entities. Postgres state grouping/filtering hits the new `entities_state_idx` via `EXPLAIN` assertion.
- Auth, tenant scoping, Condition validation, and error model identical to `/search/*` where shared. Tenant isolation verified.
- `CYODA_STATS_GROUP_MAX` ceiling enforced consistently; exceeded returns 422 with the ceiling. Detection is deterministic (D17).
- Numerical contracts: postgres `cyoda_try_float8` row-by-row table verified (D21); Welford vs `STDDEV_SAMP` parity within 1e-9 relative error (D9); `n<2` → `null` on both paths; sqlite stdev pushdown declined deterministically.
- Postgres Filter→SQL translator (D16) is parity-tested against sqlite for the shared operator set.
- Iterator contract verified per in-tree backend: `Err()` sticky, `Close()` idempotent, ctx cancellation observed.
- `buildGroupKey` (D18) collision-free under adversarial inputs.
- Backend without either capability returns 501.
- `cyoda help` updated; OpenAPI updated; config env vars documented; COMPATIBILITY.md updated alongside the SPI pin bump.
- Conformance parity tests cover the §7 matrix. Race detector clean before PR per memory `feedback_race_testing_discipline`.
