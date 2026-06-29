# PostgreSQL predicate pushdown — `spi.Searcher` (#37)

## Problem

The search service fetches **all** entities of a model via `GetAll` / `GetAllAsAt`
and evaluates predicates in-memory. PostgreSQL stores entity data as JSONB but
never uses JSONB operators in `WHERE`, so every search is O(n) over the whole
model regardless of selectivity.

`internal/domain/search/service.go` already delegates to `spi.Searcher` when the
store implements it and there is no active transaction (`tx == nil`), falling
back to `GetAll` + in-memory filtering otherwise. The SQLite plugin implements
`Searcher`; PostgreSQL does not. This change makes PostgreSQL implement it.

## Scope

**In scope:** Implement `spi.Searcher` on the PostgreSQL `entityStore` —
synchronous predicate pushdown for current-state and point-in-time searches,
reusing the existing query substrate.

**Prerequisite: #349 (point-in-time semantics canonicalization).** Review
surfaced that PIT time-bounding is inconsistent across engines and read paths
(memory/`GetAsAt`/`GetAllAsAt` round the timestamp up to the next ms; Search /
Iterate / grouped-stats bind the raw value; sqlite uses strict `<` while
postgres/memory use `<=`). The decided canonical rule is **full-precision,
inclusive `<=`, no rounding, applied uniformly across every engine and path**.
#349 lands that first. #37 then consumes the canonical bound — which, post-#349,
is exactly the raw `valid_time <= $N` the grouped-stats PIT query already emits.
**#37 introduces no rounding and no PIT-bound logic of its own.**

**Out of scope (named to keep the boundary sharp):**
- Client-facing sort parameter on the HTTP API — that is #347. This change only
  ensures PostgreSQL *honours* `SearchOptions.OrderBy` symmetrically with SQLite;
  no caller sends a non-empty `OrderBy` until #347 wires it.
- **Cross-backend `ORDER BY` *value/ordering* fidelity** — deferred to #347, which
  designs client-facing sort. This change ships structural ordering only and does
  **not** attempt to make sort *results* byte-identical across backends. Known
  divergences left for #347 (none are load-bearing pre-#347, since no caller sends
  a non-empty `OrderBy` and the default key `entity_id` is unique and never null):
  - *Type:* SQLite `json_extract` preserves type → numeric sort (`9 < 10`);
    PostgreSQL `->>` yields text → lexicographic (`'10' < '9'`).
  - *NULL placement:* SQLite sorts NULLs **first** in ascending order; PostgreSQL
    defaults to **NULLS LAST**. A sort key some entities lack orders differently.
  - *Tie-break stability:* neither backend appends a deterministic tiebreaker after
    a non-unique sort key, so equal-key rows may interleave differently.
- Async search store / job pipeline — unchanged. `SubmitAsync` calls the same
  `SearchService.Search` *only when the store is not a `SelfExecutingSearchStore`*
  (`service.go:241-243`); in that in-process case it inherits pushdown. A
  self-executing async store generates IDs via its own executor and would not
  pick up the sync pushdown — out of #37's scope. (The async ID-select-then-
  re-fetch self-inconsistency at PIT boundaries is a symptom of the broader PIT
  bug fixed uniformly by #349, not something #37 patches.)
- PIT timestamp semantics — owned by #349 (prerequisite above). #37 neither
  rounds nor changes the bound; it reuses the canonical query.
- GIN / expression indexes on JSON paths. Pushdown delivers the correctness and
  the WHERE-clause shape that *lets* an operator add indexes later; choosing and
  shipping default indexes is a separate operational decision.

## Existing substrate (already present, built for grouped-stats)

No new query-translation logic is needed. The PostgreSQL plugin already has:

- `query_planner.go` — `planQuery` splits a `spi.Filter` into a pushable SQL
  `WHERE` fragment (`$N`-placeholder form) and a residual `*spi.Filter`. Greedy
  AND, conservative OR, numeric ordering via `cyoda_try_float8`. The pushable-op
  set is parity-locked to SQLite's.
- `path_validation.go` — `validateFilterPaths` + injection-safe `validateJSONPath`.
- `grouped_stats.go` — `evalPostFilter(filter, entity, doc)` residual evaluator,
  the `postgresIter` streaming wrapper, the current-state and point-in-time base
  queries (`SELECT doc FROM entities …` / bi-temporal `DISTINCT ON` over
  `entity_versions`), and `shiftPlaceholders` (renumbers `planQuery`'s `$1…`
  fragment after the base args).

> **PIT base-query defect to fix (caught in review).** The current PIT query
> (`grouped_stats.go:89-97`) wraps the `DISTINCT ON (entity_id)` in a derived
> table that projects **only `doc`** — `entity_id` and the other direct columns
> are not selected. The Searcher *always* appends an `ORDER BY` (default
> `entity_id`), which would fail at runtime with `column "entity_id" does not
> exist` on every PIT search. (This is a latent gap in `Iterate` too: a PIT
> filter on a `SourceMeta` direct column would already fail today, untested.)
> The fix lands in the shared base-query helper below: the inner `DISTINCT ON`
> must also project the direct columns the outer `ORDER BY`/`WHERE` references.
> **Project only columns that physically exist on `entity_versions`**
> (`entity_id, model_name, model_version, version` — `entity_id` alone covers the
> shipping default order). `directMetaColumns` *also* lists `deleted`, but
> `entity_versions` has **no `deleted` column** — it lives only in
> `doc->'_meta'->>'deleted'` (which is how the existing outer query already
> computes it, `grouped_stats.go:97`). Projecting a raw `deleted` column would
> raise `column "deleted" does not exist` and re-break every PIT search.
> Projecting the *real* columns does not change `Iterate`'s output, so the fix is
> safe to share. Note the `entity_id` ⇄ `_meta.id` key mismatch (`entity_doc.go`
> stores the id under `_meta.id`, not `entity_id`): direct-column ordering must
> use the real `entity_versions` columns, not a `doc->'_meta'` extraction.
>
> **PIT timestamp bound — owned by #349, not #37.** An earlier draft of this
> spec prescribed making the Searcher *adopt* the `Truncate+Add(ms)` round-up to
> match `GetAsAt`. That was backwards: the round-up is a cargo-culted in-memory
> artifact whose premise (ms-precision clients) the system contradicts by emitting
> nanosecond timestamps. #349 establishes the canonical rule — **full-precision,
> inclusive `<=`, no rounding, uniform across all engines/paths** — and lands
> before this work. Post-#349, the grouped-stats PIT query's raw `valid_time <=
> $N` (`grouped_stats.go:98`) *is* the canonical form, so #37 reuses it unchanged.
> #37 adds **only** the projection fix above (direct columns in the inner
> `DISTINCT ON`); it does not touch the timestamp bound.
>
> **Known residual gap (deferred to #347):** a `SourceMeta` order/filter on
> `deleted` under PIT cannot resolve against a real column — `deleted` is
> **doc-only** on `entity_versions` (it lives in `doc->'_meta'->>'deleted'`,
> `grouped_stats.go:97`), so the PIT inner projection cannot include it.
> `fieldExpr`/`orderByFieldExpr` would emit the bare token `deleted`, unresolved
> in the PIT derived table. (`tenant_id` is **not** in this gap — it *is* a real
> `entity_versions` column, `migrations/000001:29`, so it would project fine; it
> is simply never client-reachable as an order/filter key.) `directMetaColumns`
> lists `deleted`, but the inner PIT projection must omit it. Not load-bearing
> pre-#347 (no caller orders/filters on meta `deleted` under PIT); #347 resolves
> it via a derived `(doc->'_meta'->>'deleted')::boolean AS deleted` expression if
> needed.

## Design decisions

1. **No scan budget.** SQLite's `Searcher` caps residual post-filter scans at a
   configurable `SearchScanLimit` (default 100k) and returns
   `ErrScanBudgetExhausted`. That is a single-node-embedded-engine guardrail; the
   production engine should not inherit it. PostgreSQL streams rows in SQL order
   and **early-stops** once `offset + limit` matches are collected — bounded
   memory *when a limit is set*, no new env var, no new help topic. (An unbounded
   `Limit <= 0` request is O(n) memory either way, same as the fallback — see the
   guard.) **Guard:** early-stop applies only when `Limit > 0`. When `Limit <= 0` (unbounded — the value the service
   forwards when no client limit is set) `offset + limit == offset`, so a naive
   early-stop would return an empty page past the offset; unbounded requests must
   drain all matches and then apply the offset. **Accepted risk:** an unbounded
   (`Limit <= 0`) request with a residual drains the entire matching set into a Go
   slice with no guardrail. This is the same O(n) memory profile as the in-memory
   fallback (`service.go` `GetAll` already loads everything), so #37 introduces no
   *new* exposure — but a production PostgreSQL model can be far larger than a
   single-node SQLite file, so the absence of any scan budget is a deliberate
   inherited risk, not a free lunch. Revisit if/when an operator hits it.
2. **`OrderBy` structural parity now.** Mirror SQLite's `orderByClause` /
   `validateOrderSpecs`: default `ORDER BY entity_id` when `OrderBy` is empty;
   otherwise emit one clause per `OrderSpec` mapped through the existing
   `directMetaColumns` / `doc->>'path'` / `doc->'_meta'->>'path'` field rules.
   `OrderSpec.Path` is validated through `validateJSONPath` before interpolation
   (same injection invariant as filter paths).

## Components — new file `plugins/postgres/searcher.go`

```go
var _ spi.Searcher = (*entityStore)(nil)
```

### `Search(ctx, filter, opts) ([]*spi.Entity, error)`

1. `validateFilterPaths(filter)`; `validateOrderSpecs(opts.OrderBy)` — reject
   bad input at the boundary before any interpolation.
2. `plan := planQuery(filter)` (skip when `filter.Op == ""`, mirroring `Iterate`).
3. Build the base query: point-in-time → the `DISTINCT ON (entity_id)` bi-temporal
   form; otherwise → `SELECT doc FROM entities WHERE tenant/model/version AND NOT
   deleted`. Extract these into a shared helper reused by `Iterate` (Gate 6 dedup;
   the literals exist twice today). Two invariants the helper must carry:
   - **PIT timestamp bound is #349's canonical raw `<=` — do not round.** Post-#349
     the bound is full-precision `valid_time <= $N` (the form grouped-stats already
     emits, `grouped_stats.go:98`). #37 must not reintroduce `Truncate+Add(ms)`.
   - **Outer projection stays `SELECT doc` only (S-1).** The B1 fix adds direct
     columns to the **inner** `DISTINCT ON`, never the outer select — `postgresIter`
     and the row scanner read exactly one column (`it.rows.Scan(&doc)`,
     `grouped_stats.go:191`). Adding columns to the outer select breaks `Scan` for
     both `Iterate` and `Search` with a column/destination mismatch.
4. Append `shiftPlaceholders(plan.where, len(baseArgs))` when pushable.
5. Append `orderByClause(opts)`.
6. **No residual** (`plan.postFilter == nil`): push `LIMIT`/`OFFSET` into SQL
   (omit `LIMIT` when `Limit <= 0`, matching SQLite — unbounded).
   **Residual present:** omit SQL `LIMIT`/`OFFSET`; stream rows, run
   `evalPostFilter` per row, collect matches, apply `offset`/`limit` in Go.
   Early-stop once `offset + limit` results are gathered **only when
   `Limit > 0`**; when `Limit <= 0` drain all matches before applying `offset`
   (see decision 1 guard). Note: with a residual, `ORDER BY` still sorts the
   entire pushdown-matching set in SQL (no SQL `LIMIT` to enable a top-N);
   early-stop saves only Go-side decode + post-filter work, not the sort. Page
   determinism relies on a total sort key — the default `entity_id` is unique, so
   it holds; #347's non-unique keys would need a tiebreaker (see ORDER divergences).
7. Decode each `doc` into a `*spi.Entity` via `unmarshalEntityDoc` — the same
   path `Iterate`/`GetAll` already use. The residual stream/decode/post-filter
   loop is exactly `postgresIter.Next`; reuse `postgresIter` to drain rows rather
   than re-implementing scan + `unmarshalEntityDoc` + `evalPostFilter` (Gate 6).

### Helpers

- `validateOrderSpecs([]spi.OrderSpec) error` — `validateJSONPath` on each
  non-direct path; returns `ErrInvalidFilterPath` on the first bad one.
- `orderByClause(opts) string` / `orderByFieldExpr(spec) string` — ordering-side
  analogues of `fieldExpr`, reusing `directMetaColumns`. Default `entity_id`.

## Data flow

```
SearchService.Search (tx == nil)
  └─> entityStore.Search(filter, opts)
        ├─ validate paths + order specs
        ├─ planQuery → (pushable WHERE, residual)
        ├─ base query (current-state | point-in-time) + WHERE + ORDER BY
        ├─ no residual  → SQL LIMIT/OFFSET → decode rows
        └─ residual     → stream → evalPostFilter → Go offset/limit (early-stop)
```

## Error handling

- Invalid filter / order path → `ErrInvalidFilterPath` (maps to 4xx upstream,
  unchanged).
- Query / scan / decode failures → wrapped with `%w` context, surfaced as 5xx by
  the service layer (no internals leaked — Gate 3).
- No new sentinel errors; **no** `ErrScanBudgetExhausted` analogue (decision 1).

## Testing (TDD)

**Coverage-matrix waiver (gate-brainstorming / test-coverage rule).** #37
introduces **no new HTTP endpoint, gRPC method, status code, or error code** —
the search HTTP/gRPC surface, its pagination params, and its only domain error
(`ErrInvalidFilterPath` → 4xx; everything else → 5xx-wrapped) are unchanged. It
swaps the *storage implementation* behind an already-covered endpoint. The
per-endpoint error/status table and scenario×layer matrix the rule normally
requires are therefore **waived for the API surface** (no cell changes); the
work is covered by (a) new postgres-plugin unit tests, and (b) a new
cross-backend parity scenario for the one genuinely backend-agnostic new
behavior — PIT search through the `Searcher` (see below).

The current-state executable contract already exists and currently passes on
PostgreSQL **via the fallback**: `e2e/parity/search.go` (`RunSearchSimpleCondition`,
`…LifecycleCondition`, `…GroupCondition` with numeric `>`, `…NoMatches`,
`…AfterUpdate`) and `search_consistency.go`. Once PostgreSQL implements
`Searcher`, these exercise the pushdown path unchanged — the parity harness
guarantees pushdown ≡ in-memory **for the result SET** of these scenarios.
Caveat (review S3): result *equivalence* is not unconditional — the service
applies its default `Limit` of 1000 only on the fallback path, not the Searcher
path (pre-existing; SQLite already behaves this way), so an unbounded search can
return more rows via a SQL Searcher than via the memory backend. This is
orthogonal to #37 and left as-is; the spec does not claim equivalence past the
default limit.

Caveat (S4): most numeric filter translations (`EQ`/`BETWEEN` via
`cyoda_try_float8`) have so far only been string-asserted in
`query_planner_test.go`, never executed against a live PostgreSQL. (Once this
lands, `RunSearchGroupCondition`'s `amount > 75` does give numeric `GT` live
coverage — but `EQ`/`BETWEEN` remain unexercised.) The new unit tests below MUST
use numeric values for `EQ`/`GT`/`BETWEEN` so every numeric translation is
exercised end-to-end before we lean on the parity scenarios as a guarantee.

New PostgreSQL-plugin unit tests (mirroring SQLite's `searcher_test.go` and
`search_injection_test.go`), written RED first:
- Each pushable operator returns the same rows as the in-memory matcher;
  EQ/GT/BETWEEN tested with **numeric** values (S4).
- OR / mixed pushable+residual → residual correctly post-filtered.
- Pagination: `limit`/`offset` with and without a residual; the unbounded
  (`Limit == 0`) + residual + non-zero `offset` case must return the correct
  page, not empty (S1 guard).
- Point-in-time search with the **default order** returns the snapshot-correct
  version set — this is the test that catches the B1 projection defect; no
  SQLite/PG PIT-search or OrderBy unit test exists today, so this is new coverage.
  (PIT *boundary* correctness and cross-engine agreement are #349's parity tests,
  not #37's — #37 reuses the canonical bound and asserts only the projection fix.)
- `OrderBy`: default `entity_id`; explicit data-path and meta-path specs; `Desc`.
- Injection: malicious `Filter.Path` / `OrderSpec.Path` rejected, never reaches SQL.
- Compile-time `var _ spi.Searcher` assertion.

**New cross-backend parity scenario — `RunSearchPointInTime`** (required, fills
the one real coverage gap). All five existing parity scenarios are current-state,
no-offset, no-OrderBy; none drives a PIT search through the `Searcher`. Since #37
is the first code to run a predicate search at a snapshot through SQL — directly
on top of the freshly canonicalized (#350) PIT bound — add a parity scenario that:
saves an entity, advances it through ≥2 versions at distinct times, then searches
with a predicate + `PointInTime` between versions and asserts the snapshot-correct
result set. Registered in `e2e/parity/registry.go` so it runs on memory / sqlite /
postgres (and the commercial backend) — asserting pushdown ≡ in-memory at a PIT.
This is the cell that proves the B1 projection fix is correct cross-engine, not
just in a postgres-only unit test. (PIT *boundary* exact-T agreement remains
#349/#350's `pit_boundary` parity; this scenario asserts predicate+PIT *search*
equivalence, which those do not cover.)

Gate 5: `go test ./...` (root, incl. e2e — Docker), `plugins/postgres` suite,
`go vet`. `make race` once before PR.

## Gates / docs

- **Gate 4:** No env-var or help-topic change (decision 1). No SPI change → no
  `go.mod` pin bump, `COMPATIBILITY.md` unaffected. `CHANGELOG.md` gets an
  additive entry under the current unreleased section (performance: PostgreSQL
  search predicate pushdown). **Keep the CHANGELOG wording honest:** without a
  GIN/expression index (deferred), pushdown narrows by `idx_entities_model` then
  evaluates the JSONB predicate as a filter over that model's rows. The win is
  **constant-factor** — no wire transfer of every doc, no Go-side decode of every
  doc, DB-side filtering, and real `LIMIT` pushdown (early stop) for non-residual
  queries — **not** selectivity-proportional / index-grade. Pushdown is the
  prerequisite that makes JSON-path indexes useful later (separate operational
  decision); the entry must not imply index-grade speedups.
- **Gate 6:** Fold the duplicated current-state / point-in-time base-query
  literals (`Iterate` + `Search`) into one shared helper as part of this change,
  carrying the B1 PIT-projection fix (project the *real* `entity_versions` direct
  columns — `entity_id`/`version`/… — on the **inner** `DISTINCT ON`, outer stays
  `SELECT doc`; do **not** project `deleted`, which is doc-only on
  `entity_versions`), which closes the latent `Iterate` PIT direct-column gap.
  The shared helper is parameterized by entity name + model version, since its
  two callers pass different shapes (`Iterate` takes a `spi.ModelRef`; `Search`
  takes `opts.ModelName`/`opts.ModelVersion` off `SearchOptions`) — accept
  name+version (or construct a `ModelRef`), not one caller's struct. The PIT
  *timestamp* bound is canonicalized by #349 (full-precision `<=`, no rounding),
  not here. The meta-`deleted`-under-PIT ordering gap remains deferred to #347.
  Note: #349 (now landed as #350) did **not** extract a shared PIT base-query
  helper and left `grouped_stats.go`'s PIT query unchanged, so #37 performs this
  extraction.
- Cross-plugin: no SPI or shared-contract change, so the Cassandra plugin and
  the parity registry are unaffected beyond already running these scenarios.
