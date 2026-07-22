# Direct-search limit correctness (#432 + #433) + plugin bounded-fail mapping + grouped-stats consistency

**Status:** design (independently reviewed)
**Date:** 2026-07-22
**Target branch:** `release/v0.8.3`
**Issues:** #432 (bug), #433 (enhancement) — siblings; prerequisites for `cyoda-go-cassandra` #24

## Problem

Defects on the **direct** (synchronous) search path, plus the transport
asymmetry and plugin-sentinel gaps they expose across search and grouped-stats.

### #432 — omitted `limit` returns unbounded on the pushdown path

`SearchOptions.Limit == 0` is overloaded: "client omitted → apply default
1000" vs. "no explicit limit → unbounded". The `1000` default is applied
**only** on the `GetAll`+match fallback branch (`internal/domain/search/service.go:257-260`),
never on the `spi.Searcher` pushdown branch (`service.go:198-210`) that **all
three OSS backends take** (`plugins/memory/searcher.go:29`,
`plugins/postgres/searcher.go:41`, `plugins/sqlite/searcher.go:34`; each treats
`Limit <= 0` as unbounded). Result: `POST /api/search/direct/{entity}/{version}`
with no `limit` returns the **entire matched set unbounded**, contradicting the
documented default of 1000 (`cmd/cyoda/help/content/search.md:41,181`;
`api/openapi.yaml:90,295`). The bound that protects the cluster is silently
absent, and behaviour diverges by branch (fallback caps at 1000, pushdown does
not).

### #433 — a `Searcher` cannot surface `SEARCH_RESULT_LIMIT` (400)

The engine documents `SEARCH_RESULT_LIMIT`
(`internal/common/error_codes.go:73` `ErrCodeSearchResultLimit`) but a plugin
`Searcher` has no SPI sentinel to raise when it detects an over-limit result
set. Any non-`*common.AppError` error from `searcher.Search` surfaces as
**500 + ticket** on both transports, not the documented **400**. OSS
sqlite/postgres SQL-truncate (top-N) and never raise it; the forthcoming
`cyoda-go-cassandra` index-driven `Searcher` (#24) implements bounded-or-fail
and must surface the documented 400.

### Adjacent bounded-fail defect — `ErrScanBudgetExhausted` 500s (sqlite)

`plugins/sqlite/searcher.go:19` defines `ErrScanBudgetExhausted` ("a search
with a residual filter examined more than `SearchScanLimit` rows"), raised at
runtime from **two sites, both inside `Searcher.Search`**: the committed
pushdown path (`searchCommitted`, ~line 99) and the in-tx read-your-own-writes
overlay path (`searchTxOverlay`, ~line 241). (Line 241 is *not* the
grouped-stats path, despite the proximity of the two concerns — the
grouped-stats streaming aggregation, `Iterable.Iterate` in
`plugins/sqlite/grouped_stats.go`, enforces no scan budget at all today.)
Nothing translates the sentinel, so it surfaces as **500 on both transports**
— the same failure shape as #433, and genuinely runtime-reachable on the
search path (it is a data-volume guard, not upstream-guarded validation).
It is semantically **distinct** from `SEARCH_RESULT_LIMIT` (rows *examined*, not
matched-count exceeding a cap), so it warrants its own error code.

### Latent inconsistency — transport-asymmetric sentinel mapping

The two transports promote a raw SPI sentinel to a client-facing status
**differently**:

- **HTTP**: unknown error → `common.Internal(...)`, itself a promotion net —
  it recognises `spi.ErrConflict` / `ErrUniqueViolation` / `ErrPartialUniqueKey`
  and promotes each to an operational 4xx instead of 500
  (`internal/common/errors.go:96-105`).
- **gRPC**: `buildErrorFields` (`internal/grpc/errors.go:24-49`) understands
  **only** `*common.AppError`; it never routes through `common.Internal`, so a
  raw SPI sentinel → `SERVER_ERROR` ticket (500). No promotion net.

Consequently, mapping a sentinel **in a handler switch** (as
`internal/domain/entity/grouped_stats_handler.go:149-195` does) only produces
the correct status **on the transport that owns that handler**. Grouped-stats
is HTTP-only today, so it happens to be correct — but the pattern is a
transport-specific trap: any such feature that gains a second transport silently
500s on the sentinel there. This is precisely the #433 bug.

The **correct, transport-symmetric pattern** — already used in ~25 places in
`internal/domain/entity/service.go` — is for the **service** to translate SPI
sentinels into `*common.AppError` before returning. Both transports then
forward the `AppError` (HTTP → 4xx; gRPC `buildErrorFields` → `CLIENT_ERROR`
with the code:message), as does any future transport. `common.Internal`'s
promotion block is left untouched (a working HTTP-only safety net).

## Design

### Part A — #432: resolve the default at the entry points; `0` = unbounded in the service

1. **New constant** `search.DefaultDirectSearchLimit = 1000` in the `search`
   package (replaces the scattered magic `1000`s on the direct path).
   `pagination.MaxPageSize = 10000` (`pagination/pagination.go:22`), so the
   default is well within bounds.

2. **HTTP entry point** (`internal/domain/search/handler.go`, direct handler):
   when `params.Limit == nil`, set `opts.Limit = search.DefaultDirectSearchLimit`.
   Present-limit parse + `MaxPageSize` reject logic unchanged.

3. **gRPC entry point** (`internal/grpc/search.go` `handleDirectSearchRequest`,
   ~336): when `req.Limit == nil`, set
   `opts.Limit = search.DefaultDirectSearchLimit`. Present-limit path unchanged.

4. **Service `Search`** (`service.go`): remove the fallback branch's implicit
   `if limit == 0 { limit = 1000 }` (lines 257-260). After removal, `0` and
   negative both mean **unbounded** on *both* branches — the fallback's
   `if limit > 0 && limit < len(matches)` already no-ops for `0`, matching the
   pushdown branch's existing `Limit < 0 → 0` mapping and the SPI's
   `0 = unbounded` convention.

5. **Both internal `Search` callers keep working.** There are two callers that
   reach the service with a non-client limit: (a) `entity/service.go:935` passes
   `Limit: -1` (explicit unbounded) — unaffected; (b) the async goroutine
   `service.go:387` calls `s.Search(bgCtx, …, opts)` with `opts.Limit == 0`
   (neither async submit entry point sets a limit). Async-on-pushdown is
   **already** unbounded today; removing the fallback default only changes
   async-on-*fallback* (translate-failure conditions) from silently-capped-1000
   to unbounded — the intended "async stores all" semantic and the latent bug
   this corrects. No caller that legitimately expects a 1000 cap is affected,
   provided the entry-point defaults land in the same change.

**Limit semantics after Part A (uniform across both service branches):**

| `opts.Limit` | Meaning | Reached by |
|---|---|---|
| `> 0` | cap at N (≤ `MaxPageSize`) | client supplied `limit=N` |
| `0` | unbounded / store-all | async submit; internal callers wanting all |
| `< 0` | unbounded | internal callers (`entity/service.go:935` uses `-1`) |

Omitted `limit` on the direct path never reaches the service as `0` — the entry
points resolve it to `DefaultDirectSearchLimit` first.

### Part B — plugin bounded-fail conditions: SPI sentinels + service-level translation

#433 and the `ErrScanBudgetExhausted` defect are the **same shape**: a plugin
detects a bounded-fail condition and must surface a documented 4xx on every
transport. Because the sqlite sentinel lives in a separate module the engine
cannot import, both must be **SPI sentinels** translated at the service.

1. **SPI** (`cyoda-go-spi`, `feat/schedule-function` branch — the live v0.8.3
   SPI window): add two sentinels, siblings of `ErrGroupCardinalityExceeded`:
   ```go
   // ErrSearchResultLimitExceeded is returned by a Searcher whose search
   // matched more entities than the configured result-limit cap
   // (bounded-or-fail). The engine maps it to 400 SEARCH_RESULT_LIMIT.
   var ErrSearchResultLimitExceeded = errors.New("search result limit exceeded")

   // ErrScanBudgetExhausted is returned by a Searcher/aggregator whose residual
   // (non-pushdown) scan examined more rows than the configured scan budget.
   // The engine maps it to 400 SCAN_BUDGET_EXHAUSTED.
   var ErrScanBudgetExhausted = errors.New("scan budget exhausted")
   ```
   Coordinated-release procedure: SPI-first — push to `feat/schedule-function`,
   take the new pseudo-version, repin cyoda-go's four `go.mod` files
   (root + `plugins/memory|postgres|sqlite`) in one commit. (No SPI tag yet;
   v0.8.3 tag deferred to milestone-end. Local `go.work use ../cyoda-go-spi`
   stays uncommitted; never `git add -A`.)

2. **sqlite plugin** (`plugins/sqlite/searcher.go`): return
   `spi.ErrScanBudgetExhausted` (wrapping, preserving the `examined N rows`
   detail: `fmt.Errorf("%w: examined %d rows", spi.ErrScanBudgetExhausted, …)`)
   from both raise sites (lines 99, 241), replacing the plugin-local sentinel.
   Update the plugin's own tests to assert on the SPI sentinel.

3. **New engine error code** `common.ErrCodeScanBudgetExhausted =
   "SCAN_BUDGET_EXHAUSTED"` (`internal/common/error_codes.go`) + help topic
   `cmd/cyoda/help/content/errors/SCAN_BUDGET_EXHAUSTED.md` (required by
   `TestErrCode_Parity`'s strict errcode↔help-topic bijection). `SEARCH_RESULT_LIMIT`
   already has its code and help topic — no new topic there.

4. **Service `Search`** (`service.go`): after the `searcher.Search` call,
   translate both sentinels to operational `AppError`s (which both transports
   forward; OSS truncation means `ErrSearchResultLimitExceeded` is exercised via
   a stub `Searcher`, while `ErrScanBudgetExhausted` is reachable on sqlite):
   ```go
   res, err := searcher.Search(ctx, filter, spiOpts)
   switch {
   case errors.Is(err, spi.ErrSearchResultLimitExceeded):
       return nil, common.Operational(http.StatusBadRequest,
           common.ErrCodeSearchResultLimit,
           "matched result count exceeds the configured limit").WithCause(err)
   case errors.Is(err, spi.ErrScanBudgetExhausted):
       return nil, common.Operational(http.StatusBadRequest,
           common.ErrCodeScanBudgetExhausted,
           "search scan budget exhausted; narrow the query or add an indexable predicate").WithCause(err)
   }
   return res, err
   ```

5. **`common.AppError` cause support**: `AppError` has `Err error`
   (`errors.go:41`) and `Unwrap()` (`errors.go:48`), but `Operational` does not
   set a cause. Add a chainable `func (e *AppError) WithCause(err error) *AppError
   { e.Err = err; return e }` (mirroring the existing `.AsRetryable()` builder)
   so `errors.Is(returned, sentinel)` still holds through the AppError.

### Part C — grouped-stats consistency (transport-symmetric translation)

Move sentinel→status translation from the HTTP handler switch
(`grouped_stats_handler.go:149-195`) into `QueryGroupedStats`
(`grouped_stats_service.go`), returning `AppError`s (each `.WithCause(sentinel)`
so `errors.Is` still holds), so the handler collapses to the standard
`errors.As(&appErr)`-forward + `common.Internal` default. **All six** sentinels
the handler currently translates must be covered (the first five are today's
behaviour; the sixth is the new scan-budget mapping, added for forward
compatibility with a bounded-scan `Iterable`/`GroupedAggregator` — sqlite's
`Iterate` (`plugins/sqlite/grouped_stats.go`) enforces no scan budget today,
so this sentinel is **not** reachable on any OSS backend via the grouped-stats
path; it is exercised only via the service unit test's fake iterable):

| Sentinel | Status + code (unchanged behaviour) |
|---|---|
| `ErrBackendNotSupported` | 501 `NOT_IMPLEMENTED_BY_BACKEND` |
| `spi.ErrGroupCardinalityExceeded` | 422 `GROUP_CARDINALITY_EXCEEDED` |
| `ErrInvalidCondition` | 400 `INVALID_CONDITION` (msg passthrough) |
| `search.ErrInvalidFieldPath` | 400 `INVALID_FIELD_PATH` (msg passthrough) |
| `search.ErrConditionTypeMismatch` | 400 `CONDITION_TYPE_MISMATCH` (msg passthrough) |
| `spi.ErrScanBudgetExhausted` (new) | 400 `SCAN_BUDGET_EXHAUSTED` (msg passthrough) |

**RISK — do not blanket-wrap arbitrary errors.** `QueryGroupedStats` must
translate **only** these known sentinels and return every other storage/driver
error unwrapped, so the handler's `default → common.Internal` (500) still fires.
`TestQueryGroupedStats_PushdownArbitraryErrorPropagates`
(`grouped_stats_service_test.go:161`) asserts a raw `errors.New("boom")`
propagates to a 500; a naive "wrap everything" refactor would break it and turn
genuine 500s into malformed 4xx. The five existing `errors.Is` service tests
(`:102,:215,:406,:437,:470,:496,:522`) continue to pass because each returned
`AppError` wraps its sentinel via `WithCause`.

`common.Internal`'s HTTP-only promotion net is **not** modified — out of scope.

## Error / status-code tables

### `POST /api/search/direct/{entity}/{version}` (HTTP) and gRPC `EntitySearchCollection`

| Scenario | HTTP status + code | gRPC (`buildErrorFields`) | Origin after change |
|---|---|---|---|
| omitted `limit` | 200, ≤1000 results | 200 | entry point → `DefaultDirectSearchLimit` |
| `limit=N` (0<N≤10000) | 200, ≤N results | 200 | unchanged |
| `limit>10000` | 400 `BAD_REQUEST` | `CLIENT_ERROR` | unchanged (entry-point reject) |
| `limit<0` (HTTP string) | 400 `BAD_REQUEST` | — | unchanged (HTTP rejects `<0`) |
| `Searcher` → `ErrSearchResultLimitExceeded` | **400 `SEARCH_RESULT_LIMIT`** | **`CLIENT_ERROR` `SEARCH_RESULT_LIMIT: …`** | service translation (Part B) |
| `Searcher` → `ErrScanBudgetExhausted` (sqlite) | **400 `SCAN_BUDGET_EXHAUSTED`** | **`CLIENT_ERROR` `SCAN_BUDGET_EXHAUSTED: …`** | service translation (Part B) |
| unknown field path | 400 `INVALID_FIELD_PATH` | `CLIENT_ERROR` | unchanged |
| unregistered model | 404 `MODEL_NOT_FOUND` | `CLIENT_ERROR` | unchanged |
| other `searcher.Search` error | 500 ticket | `SERVER_ERROR` ticket | unchanged |

### `POST /api/entity/{entity}/{version}/stats/grouped` (HTTP only)

| Scenario | Status + code | After Part C |
|---|---|---|
| neither Iterable nor GroupedAggregator | 501 `NOT_IMPLEMENTED_BY_BACKEND` | via service `AppError`; handler forwards |
| group cardinality exceeds ceiling | 422 `GROUP_CARDINALITY_EXCEEDED` | via service `AppError`; handler forwards |
| malformed condition | 400 `INVALID_CONDITION` | via service `AppError`; handler forwards |
| unknown meta filter field | 400 `INVALID_FIELD_PATH` | via service `AppError`; handler forwards |
| type-unsound condition | 400 `CONDITION_TYPE_MISMATCH` | via service `AppError`; handler forwards |
| scan budget exhausted (forward-compat; not reachable on OSS today) | **400 `SCAN_BUDGET_EXHAUSTED`** | **new mapping** — no running OSS backend raises it |
| arbitrary storage error | 500 ticket | unchanged (`default → Internal`) |
| success | 200 | unchanged |

(The scan-budget row adds a translation for forward compatibility — no OSS
`Iterable`/`GroupedAggregator` currently raises `spi.ErrScanBudgetExhausted`
on the grouped-stats path, so this row is unreachable via a running backend
until a future implementation enforces a streaming scan budget; the other
five rows are identical statuses/codes with the translation site moved for
transport symmetry.)

## Test coverage matrix (scenario × layer)

Layers: **U**=service unit · **E**=running-backend e2e (sqlite + postgres) ·
**P**=cross-backend parity (memory/sqlite/postgres) · **G**=gRPC.

| Scenario | U | E | P | G |
|---|---|---|---|---|
| direct omitted-limit caps at 1000 (Searcher pushdown) | ✓ | ✓ | ✓ | ✓ |
| direct `limit=N` caps at N | ✓ | — | ✓ | ✓ |
| service treats `Limit=0` as unbounded (both branches) | ✓ | — | — | — |
| async submit still store-all (`Limit=0` unbounded, incl. fallback) | ✓ | ✓ | — | — |
| stub `Searcher` → `ErrSearchResultLimitExceeded` → 400 `SEARCH_RESULT_LIMIT` | ✓ | — | — | ✓ |
| sqlite scan budget → `ErrScanBudgetExhausted` → 400 `SCAN_BUDGET_EXHAUSTED` (direct search) | ✓ | ✓ (sqlite) | — | ✓ |
| scan budget exhausted → 400 `SCAN_BUDGET_EXHAUSTED` (grouped-stats) | ✓ (fake iterable, forward-compat) | — (not reachable on OSS) | — | n/a |
| grouped-stats 501/422/400×3 unchanged after refactor | ✓ | — | ✓ (existing) | n/a |
| `QueryGroupedStats` returns `AppError` wrapping sentinel (`errors.Is` holds) | ✓ | — | — | — |
| arbitrary storage error still 500 (`PushdownArbitraryErrorPropagates`) | ✓ (existing) | — | — | — |
| errcode↔help-topic parity incl. new `SCAN_BUDGET_EXHAUSTED` | ✓ (`TestErrCode_Parity`) | — | — | — |

RED-first per issue TDDs: (#432) omitted-limit direct search against a
`Searcher` backend currently returns > 1000 → must cap at 1000, asserted on
HTTP **and** gRPC, async submission asserted unchanged; (#433) a stub `Searcher`
returning `spi.ErrSearchResultLimitExceeded` yields HTTP **and** gRPC **400
`SEARCH_RESULT_LIMIT`** (currently 500); scan-budget: an sqlite search that
exceeds `SearchScanLimit` (test hook `store_factory.go:324`) yields **400
`SCAN_BUDGET_EXHAUSTED`** (currently 500) on search (HTTP+gRPC); the
grouped-stats scan-budget mapping is exercised only at the service-unit layer
(fake iterable), since no OSS `Iterable` currently raises it.

## Out of scope (investigated)

- **Plugin validation-backstop sentinels** — `ErrInvalidFilterPath`
  (`plugins/{postgres,sqlite}/path_validation.go`), `ErrInvalidGroupExpr` /
  `ErrInvalidAggregate` (`plugins/{postgres,sqlite}/grouped_stats.go`). These
  sit **behind** engine-side validation (`SearchService.validateConditionPaths`
  / `ConditionToFilter`; the grouped-stats `validated` request), so a request
  that reaches the plugin has already passed path/expression validation; they
  are defense-in-depth backstops, not runtime-reachable client-facing 500s.
  Left unmapped deliberately.
- `common.Internal`'s HTTP-only sentinel-promotion net (working safety net).
- Any OSS backend *raising* `ErrSearchResultLimitExceeded` (they SQL-truncate).
- `plugins/sqlite/entity_store.go` `ErrStateFilterTooLarge` — a different
  (stats-by-state) path, outside the search/grouped-stats result-limit contract.
- Adding a gRPC endpoint for grouped-stats; async `limit`/pagination redesign.

## Rollout / coordination

1. SPI: add both sentinels on `feat/schedule-function`; new pseudo-version.
2. cyoda-go: repin 4 `go.mod` to the new SPI pseudo-version in one commit;
   sqlite plugin returns `spi.ErrScanBudgetExhausted`.
3. Add `SCAN_BUDGET_EXHAUSTED` error code + help topic.
4. Implement Parts A–C behind RED tests.
5. Race detector once, end-of-deliverable, before PR.
6. PR targets `release/v0.8.3`; body carries `Closes #432`, `Closes #433`; both
   milestoned v0.8.3 at merge.
