# #477 — the whole-model search fallback: what is actually there

Factual research for #516 row 13, measured against `release/v0.8.4` at
`134bcaa` (pin `cyoda-go-spi@f6863ae`). Every claim below has a file:line.
No design decisions here; those go in the spec.

## 1. The issue's premises, re-read

#477 was written before rows 5–11 landed. Its acceptance criteria, checked
one by one against the code today:

| # | Acceptance bullet | Status at `134bcaa` |
|---|---|---|
| 1 | Translation failure against a `Searcher` store returns 400, not a result set | **Open.** `service.go:668-711` logs `translateErr` at DEBUG and falls through to `GetAll` |
| 2 | No search request against any in-house backend reaches `GetAll` + in-process evaluation | **Open in code, closed in practice.** The branch exists (`service.go:734-739`); nothing a client can send reaches it (§3) |
| 3 | A non-`Searcher` store still executes searches in-process, with a test pinning it | **Exists, wrong shape.** `newFallbackFixture` (`service_test.go:2286`) pins it, but the path is `GetAll`, not paged |
| 4 | The surviving in-process path evaluates from a paged/streamed read | **Open.** The only `GetAll`/`GetAllAsAt` call in the engine is this path |
| 5 | Async submit and conditional delete stop reaching the fallback; the interim fallback retained from #472 is deleted | **Half done.** Conditional delete never materialises (§5). The async executor still routes untranslatable conditions into `Search`'s `GetAll` branch (`service.go:1313-1330`) |

The preconditions listed on the issue: #476 shipped (row 4). spi#32 and #478
are **closed, not done** — the quantifier was withdrawn; the existential
semantics it wanted are what a wildcard leaf already has (row 11 reference on
#516). #472 Part 3's paged read shipped as `GetPage` (`persistence.go:126`).

## 2. What the fallback is

One function, `SearchService.Search` (`internal/domain/search/service.go:576`).
Routing at `:659-661`:

```go
searcher, storeIsSearcher := store.(spi.Searcher)
iterableStore, storeIsIterable := store.(spi.Iterable)
if (storeIsSearcher && opts.Limit > 0) || (storeIsIterable && opts.Limit <= 0) {
    filter, translateErr := spi.ConditionToFilter(cond, validatedFields)
    if translateErr == nil { /* Searcher.Search or drainIterate */ }
    // Fall through to in-memory filtering if translation fails.
}
// Fallback: GetAll/GetAllAsAt + in-memory filtering.
```

The fallback body (`:734-836`):

1. `GetAll` or `GetAllAsAt` — the entire model into a slice (`:734-739`).
2. `cond == nil` → every entity matches (`:756-758`).
3. Otherwise `loadFieldsMap` → `match.Prepare` → per-row `prepared.Match`
   with a cancellation check every 1024 rows (`:770-822`).
4. `sortEntities` (`:825`), then bounded-or-fail: `opts.Limit > 0 &&
   len(matches) > opts.Limit` → 400 `SEARCH_RESULT_LIMIT` (`:830-834`).
   `Limit <= 0` is unbounded and never raises.

Its own doc comment (`:713-733`) records the two costs: the whole model is in
memory before the bound is evaluated, and in-transaction `GetAll` has already
recorded every entity into the read-set before a 400 can be raised.

This is the **only** `EntityStore.GetAll`/`GetAllAsAt` call site in the
engine. Every other `GetAll` hit in `internal/` is `ModelStore.GetAll`
(returns `[]ModelRef`).

## 3. How the fallback is reached

Three triggers, in the routing above:

**(a) Translation failure.** Measured unreachable from client input.
`TestValidateCondition_ClearingImpliesTranslates`
(`validate_translate_agreement_test.go:29`) runs every operator name on a
data path and a meta field, plus group/array nesting, a positional index and
a mid-path wildcard, and asserts each shape that clears `ValidateCondition`
also clears `spi.ConditionToFilter`. #516 row 13 records the 200,000-condition
randomised sweep behind it. Both grammars are the same function call
(`ValidateCondition` delegates to `spi.ParseFilterPath`; `canonicalOperators`
is built from `spi.OperatorNames()`).

**(b) `cond == nil`.** Not reachable from either transport:

- HTTP: `handler.go:108` calls `predicate.ParseCondition(body)`, which never
  returns `(nil, nil)` — an empty body is a JSON error, `{}` is
  `unknown condition type: ""` (`predicate/parse.go:29-45`). 400 either way.
- gRPC: `grpc/search.go:137-143` marshals `req.Condition` and parses it the
  same way; a missing condition marshals to `null` and fails the same
  `unknown condition type` check.
- Internal callers: `DirectSearch` (`service.go:1637`) is the only wrapper;
  its callers are `grpc/search.go:391` and `handler.go:185`, both post-parse.
  No production call passes `nil`.

The `cond == nil` guard at `:756` is defended by its own comment as "not
reachable today". #516 row 13 calls it "the sole remaining escape". It is a
test-only escape: `TestSearch_FallbackBranchIsBounded_TranslateFailureRoute`
(`service_test.go:2468`) passes `nil` to reach it.

**(c) A store lacking the capability for the request shape.** A store
without `Searcher` for `Limit > 0`, or without `Iterable` for `Limit <= 0`.
All four in-house backends implement both:

| Backend | `Searcher` | `Iterable` |
|---|---|---|
| memory | `plugins/memory/searcher.go:12` | `plugins/memory/grouped_stats.go:59` |
| sqlite | `plugins/sqlite/searcher.go:13` | `plugins/sqlite/grouped_stats.go:75` |
| postgres | `plugins/postgres/searcher.go:14` | `plugins/postgres/grouped_stats.go:87` |
| commercial (local checkout, pinned at SPI `v0.8.3`) | `internal/store/entity_store.go:1346` | `internal/store/grouped_stats.go:93` |

So (c) is reachable only by a store outside the four. Unit tests reach it
with `nonSearcherEntityStore` (`service_test.go:2300`).

## 4. The `Limit <= 0` mode of `Search`

`Search` accepts `Limit <= 0` as "unbounded" and routes it to `drainIterate`
(`service.go:993-1021`), which drains the iterator fully into a slice —
streamed at the store, O(matches) in the engine. The routing comment
(`:635-646`) says this serves "an internal caller that genuinely wants every
match".

Measured: **no production caller passes `Limit <= 0` to `Search`.**

- `handler.go` resolves an omitted limit to `DefaultDirectSearchLimit`
  before calling; `grpc/search.go:365` does the same.
- `grep -rn 'Limit:\s*(-1|0)\b'` over non-test `internal/` returns nothing.
- The one production call with a caller-supplied `opts` is the async
  executor's fallback branch (`service.go:1320`), which is the branch this
  issue deletes.

`TestSearch_FallbackBranchUnboundedReturnsAll` (`service_test.go:2387`)
justifies `-1` by "the sentinel a scoped conditional delete relies on at
`internal/domain/entity/service.go:947`". That line is now `mintDeleteTicket`;
conditional delete selects via `spi.Iterable` (§5) and never calls `Search`.
The justification is stale.

The SPI already retired this mode at its own boundary: `Searcher.Search`
requires `Limit >= 1` and treats `<= 0` as a contract violation
(`searcher.go:17-22`, #472 spec §3), and `GetPage` requires `limit >= 1`
(`persistence.go:115-117`).

## 5. The two consumers #472 rerouted

**Conditional delete** — `internal/domain/entity/service.go`. Already does
not materialise. `planDeleteSelection` (`:1070-1095`) translates; on failure
it selects everything at the store via `Iterate` with a zero-value filter and
applies `match.Prepared` per streamed entity as a residual. Its doc
(`:956-966`) says so explicitly, mirroring `GroupedStatsService.tallyStreaming`.
Every capability check in that file fails closed (`:835-842`, `:1228-1234`,
`:1538-1545`). Nothing here reaches `Search` or `GetAll`. #477 bullet 5's
"interim materialising fallback they retained from #472" does not exist on
this path.

**Async search** — `runAsyncJob` (`service.go:1245`). Its happy path streams
(`Iterate` at `:1357` feeding `SaveResults` at `:1412-1426`) and fails closed
on a non-`Iterable` store (`:1337-1346`). Its translate-failure branch
(`:1313-1330`) calls `s.Search(jobCtx, modelRef, cond, opts)` with the job's
`opts` — `Limit` 0 — and by the routing in §2 lands in the `GetAll` fallback
(the same translation fails again in `Search`). The comment names it
"unchanged interim fallback". This is the one remaining consumer of the
fallback besides direct search, and it exists to serve a condition shape
that §3(a) measured cannot be submitted.

`SubmitAsync` (`:1029-1203`) validates the condition but does not translate
it; the job record persists the raw condition (`SearchJob.Condition`,
`search_store.go:17-21`). spi#31 is closed as not-planned: the raw condition
stays on the job permanently (spi#45 documents it). So translation happens
once, in the executor, with the schema as it stands at execution.

## 6. The streamed and paged primitives available

| Primitive | Contract | Filtered? | Engine consumers |
|---|---|---|---|
| `Searcher.Search` | optional; bounded-or-fail, `Limit >= 1` | yes | `service.go:671` |
| `Iterable.Iterate` | optional; zero-value filter = whole model; residual inside `Next` | yes | `service.go:994` (drainIterate), `:1357` (async), `entity/service.go:1167`, `:1632`, `grouped_stats_service.go:332` |
| `EntityStore.GetPage` | **required**; `limit >= 1 && offset >= 0`; canonical per-engine ID order; `asAt` for PIT; in-tx overlay with unconditional read-set recording of the page | no | `entity/service.go:1866` (`ListEntities`) only |
| `EntityStore.GetAll`/`GetAllAsAt` | required | no | `service.go:734-739` only |

`GetPage` in a transaction records every returned page into the read-set
unconditionally (`persistence.go:119-125`) — the same model-wide recording
today's in-tx `GetAll` performs, arrived at page by page. With `asAt` set it
is committed-only and records nothing, matching `GetAllAsAt`.

The SPI's `Iterable` doc comment (`iterable.go:42-46`) states the current
fallback as the contract: "a store implementing neither Searcher nor Iterable
runs the engine's documented in-process fallback (GetAll plus in-memory
filtering)". `Searcher`'s doc (`searcher.go:8-11`) says plugins that don't
implement it "fall back to in-memory filtering". Both are SPI text that
describes this engine path.

## 7. `internal/match` after the fallback goes

Non-test importers (six files, one package each):

| Importer | Use | Survives? |
|---|---|---|
| `search/service.go:785` | `match.Prepare` for the fallback scan | this is the path under change |
| `search/service.go:964,975` | `match.ErrUnevaluableLeaf`/`ErrUnsupportedOperator` in `ClassifyStoreQueryError` | yes — classification of plugin/kernel errors |
| `search/condition_type_validate.go:320` | `match.IsTemporalOperator` | yes — validation |
| `entity/service.go:1084` | delete residual | yes |
| `entity/grouped_stats_service.go:317` | grouped-stats residual | yes |
| `workflow/engine.go:1191` | single-entity criterion evaluation | yes |

The package does not shrink materially: `Prepare`/`Prepared` stay for three
residual consumers and the criterion path. #477's "survives in reduced form"
and its "re-check whether those two can share the same traversal as the
kernel path" belong to #464, not here.

## 8. Tests that pin the fallback today

Unit (`internal/domain/search/`):

- `service_test.go:2359` `TestSearch_FallbackBranchIsBounded` — non-Searcher store, bound → 400.
- `service_test.go:2387` `TestSearch_FallbackBranchUnboundedReturnsAll` — `Limit` 0/-1 unbounded; stale justification (§4).
- `service_test.go:2468` `TestSearch_FallbackBranchIsBounded_TranslateFailureRoute` — `cond == nil` against a Searcher store proves `getAllCalls == 1`.
- `fallback_typed_test.go:70,117` — type-directed matching and fail-closed schema load on the fallback.
- `handler_timeout_test.go:162` `TestSearch_FallbackLoop_PreExpiredCtx_ReturnsDeadlineExceeded`.
- `classify_store_query_error_test.go:345` `TestSearch_BareLeafField_MatchFallback_MapsTo400InvalidCondition`.

E2E:

- `internal/e2e/search_test.go:777` `TestSearchSort_PushdownFallbackAgree`
  claims `$.tags[*] NOT_NULL` "fails ConditionToFilter (stripDollarDot
  rejects '[')". That was true before rows 9–11; a wildcard leaf now
  translates and both planners route it to the residual. The test's
  "fallback" leg runs the pushdown today, so it compares pushdown with
  itself. Its premise is stale.

## 9. Documentation that describes the fallback

- `cmd/cyoda/help/content/search.md:49` — "Only when translation fails …
  does the service fall back to in-memory filtering after a full `GetAll`
  scan."
- `cmd/cyoda/help/content/search.md:373` — `SEARCH_RESULT_LIMIT` "enforced
  on every direct-search code path (Searcher pushdown and in-memory fallback
  alike)".
- `docs/cloud-parity/tx-aware-search.md:14-16` — "the engine only falls back
  when the search condition itself cannot be translated".
- `docs/cloud-parity/direct-search-bounded-or-fail.md:40-42` — invariant 1
  lists "fallback in-memory filtering" as a code path; invariant 2 says
  `limit <= 0` "is a deliberate request for the complete matched set".
- SPI: `searcher.go:8-11`, `iterable.go:42-46` (§6).
- `docs/ARCHITECTURE.md:1045` refers to "the old whole-model `GetAll`
  behaviour it replaced" for `ListEntities` — historical phrasing in a
  reference doc.
- `service.go` comments at `:559-566`, `:635-657`, `:713-733`, `:744-754`,
  `:790-797`, `:826-834`.

## 10. What is not in scope (and why)

- `drainIterate` draining into a slice for `Limit <= 0`: no production
  caller (§4). Whether the mode is retired is a design question for the spec.
- `internal/match` traversal sharing with the kernel: #464.
- The commercial backend's obligations from rows 9–11: cassandra#95.
- Memory/sqlite in-tx `Iterate` tracking every scanned row (`service.go:650-657`
  calls it follow-up work): unrelated to whether the model is materialised.
