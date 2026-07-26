# Bounded-or-fail direct search across all backends

Issue: [#437](https://github.com/Cyoda/cyoda-go/issues/437) — milestone v0.8.3
Date: 2026-07-26

## 1. Problem

The commercial Cassandra backend's direct (synchronous) `spi.Searcher` is
bounded-or-fail: it aborts with `spi.ErrSearchResultLimitExceeded` — which the
engine maps to `400 SEARCH_RESULT_LIMIT` — the instant the matched count
exceeds the effective limit. The three OSS plugins instead truncate to top-N
(`ORDER BY … LIMIT` in SQL, or Go slicing after a residual post-filter).

Same request, same data, two different answers depending on the backend. The
divergence is currently quarantined in the Cassandra repo's parity run for
scenario `SearchOmittedLimitDefaults1000`.

The engine side already exists: `internal/domain/search/service.go:224` maps
the sentinel to `400 SEARCH_RESULT_LIMIT`, the error code is registered, the
help topic exists, and OpenAPI documents the response. Only the OSS producers
are missing.

## 2. The rule

For a direct search, **`Limit > 0` is a bounded-or-fail cap, not a page size**.
If the matched set exceeds it, the search fails; it never returns a truncated
prefix. Exactly-at-limit succeeds.

`Limit <= 0` means **unbounded**, and a plugin must not substitute a default of
its own — the engine resolves the direct-search default at both transports
before any plugin is called. Unbounded is what conditional delete
(`internal/domain/entity/service.go:947`, `Limit: -1`) and async submit
(store-all, paginated at retrieval) rely on.

### 2.1 Why truncation is wrong here

An unordered top-N truncation returns a wrong-but-available answer: the client
asked "which entities match?" and got an arbitrary subset with no indication
that it was a subset. That is exactly the failure mode
`.claude/rules/correctness-over-availability.md` forbids.

Ordered top-N (`?sort=…&limit=10`) is a correct answer, and this change does
remove it from direct search. That is accepted: Cloud and the commercial
backend do not offer it either, and async search covers the use case
(`submit → poll → paginate`). Keeping it would re-open the divergence this
change exists to close. It is a breaking change and is documented as one.

### 2.2 Offset is dropped

`spi.SearchOptions.Offset` and `search.SearchOptions.Offset` are removed
entirely. Verified: no HTTP or gRPC direct-search parameter exposes an offset
(`api/generated.go:3764` — the params are `pointInTime`, `limit`, `sort`,
`trackingRead`), no production caller sets it (`internal/grpc/search.go:148,332`,
`internal/domain/search/handler.go:96,189`, `internal/domain/entity/service.go:947`),
and the commercial backend already documents `directRequest.offset` as never
consulted.

Async *result* pagination is unaffected — it uses a separate
`search.ResultOptions.Offset` over the persisted result-ID list
(`handler.go:285` → `GetResultIDs`), which never reaches a `Searcher`.

The `"offset"` key is dropped from the persisted async job-opts JSON
(`internal/domain/search/service.go:386`). Safe: the only
`spi.SelfExecutingSearchStore` implementation does not parse that blob
(cyoda-go-cassandra `search/executor.go:221` — "reserved for future SearchOpts
parsing").

## 3. SPI changes

The SPI is pseudo-pinned pending the v0.8.3 tag, so the window for a
coordinated change is open. Per `MAINTAINING.md`, the SPI tag lands first and
the `go.mod` pin bump follows in one commit.

| Change | Detail |
| --- | --- |
| `SearchOptions.Offset` | Removed (§2.2). |
| `SearchOptions.Limit` doc | States the contract: `Limit > 0` is a bounded-or-fail cap — a `Searcher` MUST return `ErrSearchResultLimitExceeded` rather than truncate; `Limit <= 0` is unbounded and a plugin MUST NOT substitute a default. |
| `Searcher` interface doc | Same contract stated at the interface, not only on the error var. |
| `MergePage` → `MergeBounded` | Signature drops `offset`: `MergeBounded(next, adds, deleted, specs, limit)`. Returns `ErrSearchResultLimitExceeded` once total survivors exceed `limit` (internal `need` becomes `limit+1`). `limit <= 0` keeps the unbounded drain — stated explicitly in the doc so the name is not read as "always bounded". The `adds`-alone-exceeds-limit case raises, because the bound gates on total survivors, not on the committed stream. |
| `spitest` Searcher conformance | New, the first `Searcher` suite in `spitest`. Seeds a small fixed N (N = 5, not 1000 — every plugin including the commercial backend runs this). Asserts `Limit = N-1` → sentinel, `Limit = N` → N rows, `Limit = 0` → all rows. Auto-skips via a `store.(spi.Searcher)` type assertion when the harness `EntityStore` does not implement the optional interface — not via the `Harness.Skip` map, whose keys must all match or `StoreFactoryConformance` fails. `Skip` stays available for a backend that implements `Searcher` but has a tracked gap. |

### 3.1 The `Limit == 0` ambiguity (must be fixed here)

`spi.SearchOptions.Limit == 0` currently means two different things:

- OSS: unbounded. Every plugin gates on `Limit > 0`
  (`plugins/postgres/searcher.go:105`, `plugins/sqlite/searcher.go:71`,
  `plugins/memory/searcher.go:183`).
- Commercial: capped at 1000 then aborted. `entity_store.go:1367` does
  `if effectiveLimit <= 0 { effectiveLimit = search.DefaultDirectSearchLimit }`.

The consequence is live and specific: conditional delete passes `Limit: -1`
precisely so "a scoped delete is never silently capped regardless of match-set
size" (`internal/domain/entity/service.go:944`), and
`internal/e2e/entity_delete_conditional_test.go` proves it with 1050 matches.
On the commercial backend that same call aborts above 1000.

The engine resolves the direct-search default at both transports
(`handler.go:128`, `internal/grpc/search.go:339`), so a plugin cannot
distinguish "client omitted a limit" from "internal caller wants everything" —
re-defaulting is the bug. The SPI doc rules `Limit <= 0` = unbounded, and the
commercial backend drops its substitution (tracked in the cross-repo issue,
§8).

## 4. Enforcement sites

Both the plugins and the service enforce the bound. Plugins so each backend
aborts as early as its storage allows and conforms to the SPI contract when
driven directly; the service so the branch that bypasses `Searcher` cannot
quietly truncate.

| Path | Change |
| --- | --- |
| sqlite / postgres, no residual | SQL `LIMIT n+1`; `n+1` rows back → sentinel. |
| sqlite / postgres, residual post-filter | Stop and raise once matches exceed `n` (replaces the truncation at `plugins/postgres/searcher.go:152`, `plugins/sqlite/searcher.go:133`). |
| memory, non-tx and in-tx PIT | `matchSortPage` raises when matches exceed `n`, short-circuiting before the sort. |
| memory + sqlite, tx overlay | Inherited from `MergeBounded`. |
| postgres, in-tx | No change beyond the committed path — a real `pgx.Tx` under REPEATABLE READ provides RYW natively, so postgres never calls `MergeBounded` (`plugins/postgres/searcher.go:29`). |
| service in-memory fallback | `internal/domain/search/service.go:300` applies the same bound instead of truncating. |

### 4.1 Interaction with sqlite's scan budget

sqlite raises `SCAN_BUDGET_EXHAUSTED` when a residual scan exceeds
`CYODA_SQLITE_SEARCH_SCAN_LIMIT` (default 100 000). The two bounds are
independent and whichever trips first during the scan wins:
`SEARCH_RESULT_LIMIT` when matches are dense, `SCAN_BUDGET_EXHAUSTED` when the
scan is long and matches sparse. Both are `400`, both are actionable, and the
ordering is deterministic for a given dataset.

postgres has no scan budget by design (`plugins/postgres/searcher.go:24`), so
the same request can yield a different `400` code per backend. That is
pre-existing, not introduced here, and out of scope — filed as a separate
cyoda-go issue rather than accepted silently (§8).

### 4.2 The in-memory fallback is not a rare path

`internal/domain/search/service.go:249` calls `GetAll`/`GetAllAsAt` — the whole
model into memory — before any bound applies. Function conditions are never
translatable to a `spi.Filter` (`internal/domain/search/filter_translate.go:36`)
and `FunctionConditionDto` is a first-class public request shape, so this
branch is reachable in normal use.

Bounding it is a correctness fix, not a resource protection: the memory has
already been spent by the time the bound is evaluated. In-transaction, this
branch's `GetAll` also records the entire model into the read-set before the
bound can raise, so a request that `400`s still leaves the transaction with a
model-wide read-set. Both facts are documented at the call site rather than
redesigned here.

## 5. Transport validation gaps closed

Two lower-bound holes are live today and become cap bypasses under
bounded-or-fail. Both are fixed at the transports, which own "what the client
asked for" — the service cannot reject `Limit <= 0` centrally, because
conditional delete and async submit legitimately pass it.

- **HTTP** rejects only `lim < 0` (`internal/domain/search/handler.go:115`), so
  `?limit=0` yields an unbounded synchronous NDJSON search. Now rejected with
  `400 BAD_REQUEST`, consistent with the handler's existing "reject, don't
  silently clamp" stance for `limit > 10000`.
- **gRPC** validates neither bound (`internal/grpc/search.go:337`), so
  `limit: -1` yields the same unbounded search. Now rejects `limit < 1`. The
  upper bound stays covered by the service check at `service.go:157`.

`api/openapi.yaml` types `limit` as a string with an inert `maximum: 10000` and
no lower bound; the schema gets a pattern/description that states the accepted
range so the spec matches the server.

## 6. Error and status codes

### `POST /api/search/direct/{entityName}/{modelVersion}`

| Condition | Status | Code | Change |
| --- | --- | --- | --- |
| matched ≤ effective limit | `200` | — | unchanged (NDJSON stream) |
| matched > effective limit | `400` | `SEARCH_RESULT_LIMIT` | **new** (was `200`, truncated) |
| `limit` not an integer, or `< 1` | `400` | `BAD_REQUEST` | **`limit=0` newly rejected** |
| `limit > 10000` | `400` | `BAD_REQUEST` | unchanged |
| residual scan over budget (sqlite) | `400` | `SCAN_BUDGET_EXHAUSTED` | unchanged |
| malformed condition / invalid path / bad sort | `400` | existing codes | unchanged |
| model not registered | `404` | `MODEL_NOT_FOUND` | unchanged |

### `POST /api/entity/delete` (conditional delete)

| Condition | Status | Code | Change |
| --- | --- | --- | --- |
| selection search returns a classified 4xx | `400` | forwarded from the search error | **fixed** (was `500 SERVER_ERROR` + ticket) |
| matched set unbounded, any size | `200` | — | unchanged (`Limit: -1`) |

`common.Internal` unwraps only `ErrUniqueViolation` / `ErrPartialUniqueKey` /
`ErrConflict` (`internal/common/errors.go:107`), so the `*common.AppError`
returned by `Search` is currently re-wrapped as a `500`. This already buries
sqlite's `SCAN_BUDGET_EXHAUSTED`, unknown-field-path and invalid-condition
`400`s on this endpoint. Fixed with the `errors.As` + forward pattern the
search handler already uses (`internal/domain/search/handler.go:141`).

### gRPC `EntitySearchRequest`

| Condition | Envelope | Change |
| --- | --- | --- |
| matched > effective limit | `Success=false`, `Error.Code=CLIENT_ERROR`, message prefixed `SEARCH_RESULT_LIMIT:` | producer is new; mapping unchanged |
| `limit < 1` | `Success=false`, `Error.Code=CLIENT_ERROR` | **new** |

Async submit, status, cancel and result retrieval are unchanged on both
transports.

## 7. Test coverage

| Scenario | unit | running-backend e2e | cross-backend parity | gRPC |
| --- | --- | --- | --- | --- |
| over limit → sentinel / `400` | all 3 plugins, pushdown + residual/overlay branches; `MergeBounded`; spitest conformance | ✓ (real postgres) | ✓ | ✓ (existing stub test covers the envelope; add a real-backend variant) |
| exactly at limit → success | all 3 plugins + spitest | ✓ | ✓ | — |
| `Limit <= 0` unbounded | all 3 plugins + spitest | existing `TestDeleteEntities_Conditional_OverThousandMatches` (1050 matches) stays green | — | — |
| in-transaction over limit | sqlite + memory overlay, postgres tx | ✓ | ✓ | — |
| sqlite residual branch over limit | ✓ | ✓ (existing `handler_scan_budget_sqlite_test.go` harness) | — | — |
| service in-memory fallback bounded | ✓ | — | — | — |
| `limit=0` → `400 BAD_REQUEST` | handler unit | ✓ | — | — |
| gRPC `limit < 1` → `CLIENT_ERROR` | — | — | — | ✓ |
| conditional delete forwards search 4xx | service unit | ✓ | — | — |
| scan budget still wins when it trips first | sqlite | existing | — | — |

### 7.1 Parity scenario

`SearchOmittedLimitDefaults1000` (`e2e/parity/search.go:262`) is rewritten in
place as `SearchDirectBoundedOrFail`, keeping the registry at 218 entries so
`registry_count_test.go` needs no bump. One seed of 1001 matching entities,
four assertions:

1. omitted limit → `400 SEARCH_RESULT_LIMIT` (proves both the 1000 default and
   the bound)
2. `limit=1000` → `400 SEARCH_RESULT_LIMIT`
3. `limit=1001` → 1001 rows
4. in-transaction, `limit=1000` → `400 SEARCH_RESULT_LIMIT` (exercises the
   memory/sqlite overlay and the postgres tx path)

`e2e/parity/client` needs a limit-bearing raw variant — `SyncSearchRaw`
(`client/http.go:1162`) returns status and body but takes no limit.

### 7.2 Tests the offset removal breaks

These assert offset behaviour and must be removed or rewritten, not merely
compiled away:

- `plugins/postgres/searcher_test.go:179-232` (incl. `TestPGSearcher_UnboundedOffsetWithResidual`)
- `plugins/sqlite/searcher_test.go:302-361`, `plugins/sqlite/searcher_tx_test.go:237,286-330`
- `plugins/memory/searcher_test.go:105`
- `internal/domain/search/service_test.go:274-284`
- SPI `merge_page_test.go:37,54,75,126`

`plugins/*/search_store_test.go` "offset beyond end" cases exercise
`GetResultIDs` async-result pagination and are unaffected.

### 7.3 Read-set semantics change

`TestSearchTx_TrackingRead_PagedWindowOnly`
(`plugins/sqlite/searcher_tx_test.go:286`) asserts that a `TrackingRead` in-tx
search records only the *returned page* — committed rows paged out by `Offset`
or `Limit` must not enter `tx.ReadSet`. Under bounded-or-fail the returned page
is always the complete match set, so the invariant becomes "records exactly the
match set". That widens first-committer-wins conflict scope relative to today
and is more correct — no partially-observed predicate — but it is a
transaction-semantics change on the primary multi-node path, so it is stated
here rather than discovered later. The test is rewritten to the new invariant
(it cannot simply drop `Offset`: `Limit=2` over 5 matches now errors), and the
read-set doc comments at `plugins/memory/searcher.go:132`,
`plugins/sqlite/searcher.go:290` and `plugins/postgres/searcher.go:55` are
updated.

## 8. Documentation and cross-repo

Gate 4:

- `cmd/cyoda/help/content/errors/SEARCH_RESULT_LIMIT.md` — the current remedy
  ("a smaller `pageSize` … reduce the result set below the cap", line 26) is
  now backwards: a smaller limit makes failure *more* likely. Becomes "narrow
  the condition, raise `limit` up to 10000, or use async search".
- `cmd/cyoda/help/content/search.md` — lines 45 and 314 describe `limit` as a
  truncating bound; rewritten to state bounded-or-fail and point at async for
  large or ordered-top-N result sets.
- `api/openapi.yaml` — `searchEntities` description and the `limit` schema
  (§5).
- `CHANGELOG.md` — breaking behaviour change: direct search over the limit now
  `400`s; `limit=0` rejected; ordered top-N moves to async.
- `COMPATIBILITY.md` — SPI pin bump.
- `docs/cloud-parity/direct-search-bounded-or-fail.md` — Gate 7. The SPI
  `Searcher` contract changes (bounded-or-fail wording, `Limit <= 0` =
  unbounded, `Offset` removal, `MergeBounded`) are integration-contract changes
  Cloud mirrors.

No new error codes, so no new `errors/<CODE>.md` topic and no
`TestErrCode_Parity` impact.

Cross-repo issues to file:

- **cyoda-go-cassandra** — remove the `Offset` mapping at
  `internal/store/entity_store.go:1390` and the `directRequest.offset` field
  (won't compile after the SPI field is removed); drop the
  `effectiveLimit <= 0 → 1000` substitution at `entity_store.go:1367` per §3.1;
  remove the `SearchOmittedLimitDefaults1000` quarantine entry in
  `e2e/cassandra_test.go` and adopt the renamed scenario. The rename makes that
  quarantine key inert on their next dependency bump — their skip map does not
  fail on unmatched keys, so the renamed scenario will simply run.
- **cyoda-go** — postgres has no residual scan budget while sqlite does, so the
  same request can yield a different `400` code per backend (§4.1).

## 9. Out of scope

- Giving postgres a scan budget (own issue, §8).
- Any change to async submit, status, cancel or result pagination.
- Reinstating ordered top-N on direct search in any form (§2.1).
