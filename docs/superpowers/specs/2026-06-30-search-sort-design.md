# Search result sorting by single-value field paths (#347)

Status: design (for review)
Date: 2026-06-30
Branch: `feat-search-sort` (off `release/v0.8.2`)

## 1. Summary

Expose client-controllable sorting of entity-search results by single-value
(scalar leaf) field paths — over entity **data** fields and a closed set of
engine **meta** fields. The SPI already models ordering (`spi.SearchOptions.OrderBy`,
`spi.OrderSpec`) and both SQL plugins already append an `ORDER BY`, but:

- it is **not plumbed** from the HTTP/gRPC surface through the domain service
  into the SPI call;
- the meta canonical→physical name mapping **does not exist** (plugins
  interpolate `OrderSpec.Path` verbatim);
- **cross-backend ordering agreement is not engineered** — postgres extracts
  JSON as `TEXT` (lexical), sqlite extracts typed (numeric/temporal), NULL
  placement and string collation differ by default, and the memory backend has
  no `Searcher` at all (it sorts in Go);
- the OpenAPI/help docs claim results sort *descending* by entity id while the
  implementation defaults to *ascending*.

The core of this work is therefore a **single canonical ordering semantic** that
the memory, sqlite, and postgres backends all conform to — bit-for-bit identical
result order — plus the plumbing and the doc fix.

## 2. Scope

In scope: sorting by scalar leaf paths over entity data (`Source=data`) and a
closed allowlist of scalar meta fields (`Source=meta`); HTTP and gRPC surfaces;
sync (`/search/direct`, `EntitySearchRequest`) and async
(`/search/async`, `EntitySnapshotSearchRequest`) entry points; deterministic
cross-backend order including NULL placement and a stable tiebreaker.

Out of scope (rejected at the boundary, documented): ordering on arrays/objects,
collation/locale options (canonical collation is byte order — see §4), computed
expressions, `ByteArray` fields, union-typed scalar fields whose member types
disagree on ordering class (§4.4).

Every entity model is registered and carries a schema (there is no
schemaless/unregistered model), so a data field's ordering class is **always**
resolvable from `FieldsMap`. A sort path absent from the schema is simply an
invalid path ⇒ `400 INVALID_FIELD_PATH` (§9), the same outcome the condition-path
validator produces.

## 3. Wire formats

Two entry points, two idioms, both lowering to `[]spi.OrderSpec`.

### 3.1 HTTP — repeatable `sort` query param

A repeatable `sort` query parameter on both search endpoints. Repetition order
is sort precedence (first = primary key). Token grammar:

```
token   := [ "@" ] path [ ":" dir ]
path     := segment ( "." segment )*          ; data path (bare) or, after "@", a flat meta field name
dir      := "asc" | "desc"                     ; optional, default "asc"
```

- **Bare path ⇒ data field** (`Source=data`). A leading `@` ⇒ **meta field**
  (`Source=meta`); the remainder is a single flat meta field name from the §5
  allowlist. `@` is not a legal path character, so it can never collide with a
  data path — even a data field literally named `meta` sorts cleanly as
  `sort=meta.label.position.x`.
- A leading `$.` on a data path is tolerated and stripped (parity with the
  condition-body JSONPath vocabulary), so `sort=$.surname:desc` ==
  `sort=surname:desc`.
- Direction is split on the token's **last** `:`; `:` is not a legal path
  character, so the split is unambiguous.

Examples:
```
?sort=surname:desc&sort=@creationDate:asc
?sort=address.home-address.country:desc&sort=@creationDate
?sort=meta.label.position.x:desc                 # 'meta' here is a DATA field
```

Generated in `api/openapi.yaml` as `name: sort, in: query, schema: {type: array,
items: {type: string}}, style: form, explode: true` ⇒ `Sort *[]string` in
`api/generated.go`; parsed in `internal/domain/search/handler.go` the way `limit`
is today.

### 3.2 gRPC — structured `orderBy` array

The CloudEvent search request payloads gain an optional `orderBy` array of
objects. The Go structs (`events.EntitySearchRequestJson`,
`events.EntitySnapshotSearchRequestJson` in `api/grpc/events/types.go`) are
**generated** (`DO NOT EDIT`) from JSON Schema sources
`docs/cyoda/schema/search/{EntitySearchRequest,EntitySnapshotSearchRequest}.json`
— so `orderBy` is added to **both schema sources and regenerated**, never
hand-edited, and the CloudEvents schema surfaced by `cyoda help cloudevents json`
updates with it:

```json
"orderBy": [
  { "path": "surname", "source": "data", "desc": true },
  { "path": "creationDate", "source": "meta", "desc": false }
]
```

`source` defaults to `"data"`, `desc` to `false`. No string grammar on the gRPC
side — a JSON body has no query-encoding constraints, so the structured form is
the natural idiom. Both forms validate against the same rules (§4–§6) and produce
identical `[]spi.OrderSpec`.

## 4. Canonical ordering semantic (the contract)

Every backend MUST produce the identical total order. Order is defined key by
key; for each key a value resolves to one **ordering class**, and the class
fixes the comparison on every backend.

`OrderSpec` gains a `Kind` field (§8) carrying the class, because the class is
derived from the model schema / meta vocabulary — knowledge only the domain has,
which the plugin needs in order to emit the right SQL. The domain assigns `Kind`;
the plugin and the in-memory comparator render it.

### 4.1 Ordering classes and cross-backend rendering

| Kind | Applies to | sqlite `ORDER BY` expr | postgres `ORDER BY` expr | Go comparator |
|------|-----------|------------------------|--------------------------|---------------|
| **Numeric** | `IsNumeric` data types | `CAST(json_extract(<col>,'$.<p>') AS REAL)` | `cyoda_try_float8(<extract>)` | compare as `float64` |
| **Text** | `String`, `Character`, `UUIDType`, `TimeUUIDType`, **temporal data** types, meta `state`/`transitionForLatestSave`/`transactionId`/`id` | `json_extract(...) COLLATE BINARY` | `(<extract>) COLLATE "C"` | `bytes.Compare` |
| **Bool** | `Boolean` | `json_extract(...)` (0/1) | `(<extract>)::boolean` | `false < true` |
| **Temporal** | meta `creationDate`, `lastUpdateTime` (engine-normalized instants) | `creation_date µs / 1000` (int ms) | `floor(extract(epoch from (<extract>)::timestamptz)*1000)` (int ms) | `t.UnixMilli()` (int64) |

Notes:
- **Numeric canonical precision is IEEE-754 double.** Integers beyond 2^53 and
  high-precision decimals order by their double approximation. Documented limit;
  it is the only representation all three backends render identically. Postgres
  uses the existing `cyoda_try_float8` helper (not a raw `::double precision`
  cast) so a non-numeric value yields NULL (→ sorts last) instead of failing the
  whole query — matching sqlite's lenient `CAST` and the Go comparator.
- **Text canonical collation is byte order** (`BINARY` / `COLLATE "C"` /
  `bytes.Compare`). This is why locale collation is out of scope: byte order is
  the cross-backend canonical, not a missing feature.
- **Temporal canonical resolution is MILLISECONDS.** All backends floor the
  instant to whole milliseconds before comparing (int64 epoch-ms), because that
  is the coarsest resolution common to every parity backend (the commercial
  Cassandra/HLC backend; cf. the point-in-time canonicalization's ms cross-engine
  floor). Without the floor, the in-memory comparator would distinguish
  sub-millisecond instants that the SQL backends tie — a divergent total order.
  `float64`-of-nanoseconds is explicitly NOT used (it cannot represent `UnixNano`
  past 2^53). Two instants within the same millisecond tie and fall through to
  the `entity_id` tiebreaker on every backend. The parity test seeds
  sub-millisecond-apart instants to exercise the floor.
- **Temporal is only ever an engine-controlled meta field.** Data temporal types
  (`LocalDate`, `LocalDateTime`, `LocalTime`, `ZonedDateTime`) are class **Text**
  (lexical on the stored ISO string): deterministic across backends, and
  chronological **only when** values are normalized ISO 8601. Note `ZonedDateTime`
  is lexical on the *offset-bearing* string, so equal instants with different
  offsets do not order chronologically — a documented limitation, not a bug
  (sqlite cannot reliably cast arbitrary ISO strings to instants the way postgres
  can, so casting would itself diverge).

### 4.2 NULL / missing placement

A path that is absent or JSON `null` sorts **last**, for both `asc` and `desc`
(explicit, not the SQL engine default which differs). Rendered as `NULLS LAST`
(SQLite ≥3.30 and postgres both support the syntax; emulate with
`CASE WHEN <expr> IS NULL THEN 1 ELSE 0 END ASC, <expr>` if a target lacks it).
The Go comparator places `nil`/absent last identically.

### 4.3 Default order and tiebreaker

- No `sort`/`orderBy` ⇒ **`entity_id` ascending** (today's real behavior).
- With sort keys, append **`entity_id asc`** as the final, total-order
  tiebreaker so paging is deterministic on non-unique keys.
- If the terminal user-supplied key already resolves to the entity id
  (`@id`), the appended tiebreaker is **skipped** (no redundant/contradictory
  `… , entity_id ASC`).

### 4.4 Type resolution and rejected fields

The domain resolves a data path's `Kind` from `FieldDescriptor.Types`
(`internal/domain/model/schema/field.go`):

- single non-`Null` type ⇒ its class (nullable fields are fine — `Null` is
  ignored);
- a union whose members map to the **same** class ⇒ that class (e.g.
  `[Integer, Long]` ⇒ Numeric);
- a union whose members map to **different** classes (e.g. `[Integer, String]`)
  ⇒ **reject** (`INVALID_FIELD_PATH`), because no single canonical order exists;
- `ByteArray`, or a non-scalar (object / array / `[*]`) path ⇒ **reject**.

Meta fields have fixed classes (§5), so meta `Kind` needs no schema lookup.

## 5. Meta vocabulary and canonical→physical mapping

Sortable meta fields = the scalar fields the result envelope already exposes to
clients (`entityEnvelope`, `internal/domain/search/handler.go`). `OrderSpec.Path`
carries the **canonical client name**; each backend maps it to physical storage.
This mapping is **new code** in each plugin (today they interpolate verbatim and
would extract a non-existent key).

| Canonical (`@name`) | Kind | `EntityMeta` field | sqlite physical | postgres `_meta` key |
|---------------------|------|--------------------|-----------------|----------------------|
| `state` | Text | `State` | blob `$.state` | `state` |
| `creationDate` | Temporal | `CreationDate` | blob `$.creation_date` (µs int) | `creation_date` (RFC3339) → `::timestamptz` |
| `lastUpdateTime` | Temporal | `LastModifiedDate` | blob `$.last_modified_date` | `last_modified_date` → `::timestamptz` |
| `transitionForLatestSave` | Text | `TransitionForLatestSave` | blob `$.transition_for_latest_save` | `transition` |
| `transactionId` | Text | `TransactionID` | blob `$.transaction_id` | `transaction_id` |
| `id` | Text | `ID` | `entity_id` column | `entity_id` column |

The physical name **differs between backends** (`transition` vs
`transition_for_latest_save`), so the mapping cannot be shared — each plugin owns
its table. The mapping must be consistent for **both** current-state and
point-in-time queries (the PIT query projects different columns — §9 verification
item: do not map `creationDate` to a `created_at` row column that the PIT
`entity_versions` projection lacks; use the meta value).

The meta vocabulary at the plugin boundary is the **canonical client names**
(this table's first column) — *not* the old physical names. The plugins'
`validateOrderSpecs` is extended to **reject any `Source=meta` path outside this
canonical set** (defense in depth: no unmapped-name panic, no broken empty SQL),
and the existing meta-order tests migrate from physical names (e.g. `entity_id`,
`state` blob key) to canonical (`id`, `state`). This is a deliberate, documented
vocabulary change from the pre-existing verbatim behavior.

## 6. Domain plumbing

```
HTTP sort=[]string ─┐
                    ├─► []search.OrderKey ─► validate+classify ─► []spi.OrderSpec ─► spi.SearchOptions.OrderBy
gRPC orderBy=[]obj ─┘
```

- New field `OrderBy []OrderKey` on `search.SearchOptions`
  (`internal/domain/search/service.go`), where `OrderKey{Path, Source, Desc}` is
  the domain-level parsed form (pre-classification).
- `SearchService.Search` populates `spi.SearchOptions.OrderBy` on the **pushdown**
  path (currently omitted, `service.go:118-136`) **and** the GetAll+filter
  **fallback** path gains an in-memory sort (§7).
- Validation (`§4.4`, `§5`) runs at the boundary, reusing the schema load /
  negative-cache machinery in `path_validate.go` but with a **new scalar-leaf
  predicate** (the existing `isPathKnown` accepts object prefixes — correct for
  filters, wrong for sort).
- **Sort keys resolve to typed `[]spi.OrderSpec` synchronously** — in `Search`
  for the sync path and in `SubmitAsync` for the async path (mirroring the
  synchronous `validateConditionPaths` at `service.go:192`). The async path
  persists the **resolved, `Kind`-bearing** specs in the job opts (not the raw
  pre-classification keys), so a `SelfExecutingSearchStore` (commercial backend)
  executes with correct ordering classes and a bad sort field returns
  `400 INVALID_FIELD_PATH` at submit — not a silently FAILED background job.

## 7. In-memory comparator (memory backend + fallback parity)

The memory backend has no `Searcher`; sqlite/postgres also fall back to
`GetAll`+filter when a transaction is active or the condition is untranslatable
(`service.go:118-158`). All of these sort in Go. The comparator MUST implement
the §4 semantic exactly — same class comparisons, same NULLS-LAST, same
tiebreaker — so that (a) memory matches the SQL backends and (b) **each SQL
backend matches itself** across its pushdown and fallback paths.

Comparator: for each `OrderSpec` in order, extract the leaf value
(gjson over `Entity.Data` for data; `EntityMeta` field for meta), compare per
`Kind`, respect `Desc`, nulls last; final tiebreaker on `entity_id`.

## 8. SPI change

`spi.OrderSpec` gains an additive field:

```go
type OrderSpec struct {
    Path   string
    Source FieldSource
    Desc   bool
    Kind   OrderKind   // NEW: Text(zero) | Numeric | Bool | Temporal
}
```

`OrderKind` zero value is `Text` (current verbatim-string behavior), so the
change is backward compatible. This is an SPI contract change: it lands on
`cyoda-go-spi` main within the open v0.8.2 SPI window and cyoda-go pseudo-version
bumps to pin it (per MAINTAINING.md coordinated-release; SPI tag stays deferred to
milestone end). The Cassandra plugin consumes the same `Kind` and the §4 table is
its conformance contract.

## 9. Validation, error codes, edge cases

Bad sort input is a `400` with code **`INVALID_FIELD_PATH`** (existing,
help-topic-backed at `cmd/cyoda/help/content/errors/INVALID_FIELD_PATH.md`,
guarded by `TestErrCode_Parity`) — matching the existing condition-path
validator, not generic `BAD_REQUEST`. No new error code.

| Input | Result |
|-------|--------|
| `sort=surname` / `sort=surname:desc` / `sort=@creationDate` | OK |
| direction omitted | `asc` |
| `sort=` (empty), `sort=:desc` (no path), `sort=@` (sigil only), `sort=name:` (empty dir) | 400 `INVALID_FIELD_PATH` |
| `sort=name:up` (bad direction) | 400 `INVALID_FIELD_PATH` |
| data path not a registered scalar leaf / object / array / `[*]` / `ByteArray` / disagreeing union | 400 `INVALID_FIELD_PATH` |
| `@<name>` not in §5 allowlist, or `@a.b.c` (nested meta) | 400 `INVALID_FIELD_PATH` |
| same path appears twice (any directions) | 400 `INVALID_FIELD_PATH` (duplicate sort key) |
| more than `CYODA_SEARCH_MAX_SORT_KEYS` keys (default 16) | 400 `INVALID_FIELD_PATH` (bounded; configurable cap) |

PIT mapping verification (§5): confirm `creationDate`/`lastUpdateTime` resolve to
the meta value in both current-state and `entity_versions` PIT queries on both
SQL backends. Hyphenated key verification: confirm sqlite `json_extract('$.a.b-c')`
and postgres `doc->'a'->>'b-c'` extract the same value before shipping the
hyphenated example (pre-existing for filters; surfaced here).

## 10. Documentation

- `api/openapi.yaml` (`searchEntities` ~6304, async ~6492) and the mirror copies
  in `docs/cyoda/`: replace "sorted in descending order by entity id" with the
  ascending default + the `sort` param + grammar.
- `cmd/cyoda/help/content/search.md`: add a **Sorting** section (grammar, `@`
  meta sigil, meta allowlist, canonical order semantics, NULLS LAST, tiebreaker).
- gRPC: document `orderBy` in the CloudEvents request schema surfaced by
  `cyoda help cloudevents json`.
- Gate-7 cloud-parity: add `docs/cloud-parity/search-sort.md` (new contract
  surface Cloud mirrors).
- Gate-4: `COMPATIBILITY.md` / CHANGELOG entries for the SPI `OrderKind` bump.
- Gate-4 config: new env var `CYODA_SEARCH_MAX_SORT_KEYS` (default 16, §9) wired
  into `DefaultConfig()`, the `cmd/cyoda/help/content/config/*.md` topic, and
  `README.md` together.

## 11. Error/status-code table per endpoint

| Endpoint | 200 | 400 `INVALID_FIELD_PATH` | 400 `BAD_REQUEST` | 5xx |
|----------|-----|--------------------------|-------------------|-----|
| `POST /search/direct/{e}/{v}` (sync) | results stream | bad sort token/path (§9) | existing (bad limit/condition) | existing |
| `POST /search/async/{e}/{v}` (submit) | snapshot id | bad sort token/path (§9) | existing | existing |
| gRPC `EntitySearchRequest` | stream | `InvalidArgument` env, code `INVALID_FIELD_PATH` | existing | existing |
| gRPC `EntitySnapshotSearchRequest` | snapshot id | `InvalidArgument` env, code `INVALID_FIELD_PATH` | existing | existing |

## 12. Coverage matrix (scenario × layer)

Layers: U=plugin/domain unit · E=running-backend e2e (`internal/e2e`, postgres) ·
P=cross-backend parity (`e2e/parity`, memory+sqlite+postgres+commercial) ·
G=gRPC (`internal/grpc`).

| Scenario | U | E | P | G |
|----------|---|---|---|---|
| sort data field asc/desc (Text) | ✓ sqlite+pg `orderByFieldExpr` | ✓ | ✓ | ✓ |
| sort numeric data field (lexical-vs-numeric regression) | ✓ | ✓ | ✓ | — |
| sort by `@creationDate` / `@lastUpdateTime` (Temporal, cross-backend chronological) | ✓ | ✓ | ✓ | ✓ |
| sort by `@state` (Text meta) | ✓ | ✓ | ✓ | — |
| multi-key precedence + `entity_id` tiebreaker determinism | ✓ | ✓ | ✓ | — |
| NULL/missing placement (NULLS LAST) | ✓ | ✓ | ✓ | — |
| pushdown vs in-memory-fallback agree (same backend, txn active) | ✓ | ✓ | — | — |
| sort under point-in-time query | ✓ | ✓ | ✓ | — |
| each 400 edge case (§9), sync AND async submit | ✓ | ✓ (HTTP sync+async) | — | ✓ (gRPC direct+snapshot) |
| async submit returns 400 synchronously for bad sort (no FAILED job) | ✓ | ✓ | — | ✓ |
| async submit persists typed OrderSpecs (self-executing store path) | ✓ | ✓ | ✓ | ✓ |
| `data field named 'meta'` sorts as data (no `@` collision) | ✓ | ✓ | — | — |

Concurrency: not applicable (read-only ordering); no parity-suite race test.
The numeric/temporal/NULL parity rows are the regression guard for the review's
BLOCKER findings — they must be RED before the canonicalization lands.

## 13. Async result-order integrity (§review finding 6)

`SubmitAsync` serializes opts to JSON (`service.go:209-217`) — **add `OrderBy`**
so `SelfExecutingSearchStore` backends (which execute from the persisted opts,
`service.go:241-243`) don't silently drop the sort. Verify the async result store
preserves the ordered id list (`SaveResults`/`GetResultIDs`) and `GetAsyncResults`
does not re-order on refetch.

## 14. Cross-plugin / Cassandra

The §4 canonical table and §5 meta mapping are the conformance contract for the
out-of-tree Cassandra plugin; the §12 parity rows propagate to it via
`e2e/parity/registry.go`. A separate Cassandra issue (created alongside this work)
tracks implementing `Kind`-aware ordering + the canonical meta mapping there.

## 15. Resolved decisions

- **D1** — there is no schemaless/unregistered model; a data field's ordering
  class is always resolvable from the schema, and an unknown path is just a
  `400 INVALID_FIELD_PATH`. No special-casing. (§2, §7)
- **D2** — numeric canonical ordering is IEEE-754 double (documented precision
  limit; exact decimal is not identically renderable on sqlite). (§4.1)
- **D3** — sort-key count is capped, configurable via
  `CYODA_SEARCH_MAX_SORT_KEYS` (default 16). (§9, §10)
- **D4** — a duplicate sort key ⇒ `400 INVALID_FIELD_PATH`. (§9)
