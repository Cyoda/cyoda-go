# No search path materialises a model — #477 design

**Issue:** Cyoda/cyoda-go#477 (#516 row 13). **Target:** `release/v0.8.4`.
**Research:** `docs/superpowers/research/2026-09-01-477-search-fallback-research.md`
(facts, file:line) and `2026-09-01-477-design-brief.md` (design through two
independent reviews). This spec supersedes both.

## 1. Goal

The search bounding contract (#475, `docs/cloud-parity/direct-search-bounded-
or-fail.md`) requires that no search path materialises a model. Today that is
a property most paths happen to have. What still loads a whole model into
memory, measured at `134bcaa`:

| Where | What | Reached by |
|---|---|---|
| `search/service.go:734-739` | `GetAll`/`GetAllAsAt` + in-process match | translation failure (unreachable from a client), `cond == nil` (test-only), a store without `Searcher` (none shipped) |
| `search/service.go:1313-1330` | async executor routes an untranslatable condition into the above | unreachable from a client |
| memory `searcher.go:72,101,118,238` | every `Search` deep-copies every entity's `Data` bytes before filtering | **every search on the memory backend** |
| sqlite `grouped_stats.go:101` | in-tx `Iterate` materialises the merged view (`getAllTx`) | any in-tx iterate: conditional delete, grouped stats, async is detached |
| sqlite `entity_store.go:1181-1197` | in-tx `GetPage` drains `offset+limit+len(Deletes)` rows | in-tx `ListEntities` deep pages |
| memory + sqlite in-tx `Count`/`CountByState` | build the merged view via `GetAll` and count it | in-tx counts |
| memory `entity_store.go:677`, sqlite `:764-777` | in-tx `DeleteAll` copies every payload to stage ids | in-tx delete-all |
| postgres | audited: no materialising path (`GetPage`/`Iterate` on the tx connection; in-tx `DeleteAll` selects `entity_id` only, `entity_store.go:401`) | — |

After this change the SPI has no whole-model read, every engine path that
reads more than one entity does so through one cursor, and the memory and
sqlite plugins copy no payload bytes beyond what they return.

## 2. Engine — `SearchService` (`internal/domain/search/service.go`)

### 2.1 Contract of `Search`

- **`Limit >= 1` is required.** `Limit <= 0` returns a plain error (a
  programmer contract violation; HTTP resolves `DefaultDirectSearchLimit`
  and gRPC rejects `< 1`, so no transport can produce it). `drainIterate`'s
  unbounded mode and the `Limit <= 0` routing go.
- **A nil condition is rejected** in `Search` and `SubmitAsync` with
  `400 INVALID_CONDITION` before `ValidateCondition` runs. Not inside
  `ValidateCondition`: `ValidateCondition(nil)` is vacuously valid and the
  delete path relies on nil meaning "select everything". The `cond == nil`
  special case in the fallback is deleted with the fallback.
- **Translation failure is a 400.** `translateErr` is passed to
  `ClassifyStoreQueryError` first (`ConditionToFilter` wraps
  `spi.ErrInvalidFilterPath`; the client-visible code must not depend on
  which layer rejected the input, `path-grammar.md` §11), then defaults to
  `400 INVALID_CONDITION` carrying the translator's message. Reachability:
  the `INVALID_CONDITION` leg is reachable through `Search` with a
  caller-built condition type (validation's switch defaults to accept,
  `operators.go`; translation rejects, `condition_filter.go:181`). The
  `INVALID_FIELD_PATH` leg is not reachable through `Search` — validation
  and translation share `spi.ParseFilterPath` — and is pinned by a direct
  `ClassifyStoreQueryError` test on the wrapped sentinel. `ErrUnknownOperator`
  cannot reach translation either (the canonical operator set is
  `spi.OperatorNames()`), so it is not listed. No new error code: the branch
  is unreachable from validated input (research §3a) and could never earn
  e2e coverage.

### 2.2 Routing

```
store is spi.Searcher  →  Searcher.Search(filter, opts)          (unchanged)
otherwise              →  Iterate(filter) drained to Limit+1     (bounded drain)
```

The second rung: `Iterate` with the translated filter (pushdown at the
store, residual inside `Next()` by the same kernel), `PointInTime` and
`TrackingRead` forwarded, `OrderBy` left out of `IterateOptions` (in-tx
ordered iteration is an SPI error) and the bounded match set sorted by
`sortEntities` (`ordersort.go:23`, delegating to `spi.LessByOrder`, whose
terminal tiebreaker is byte-wise `Meta.ID`, so both rungs return canonical
order). The `Limit + 1`-th yield raises
`400 SEARCH_RESULT_LIMIT` with `spi.ErrSearchResultLimitExceeded` as cause
and closes the iterator. Peak memory `Limit + 1` entities. On this rung a
400 may leave up to `Limit + 1` entities recorded in the read-set — a prefix.

There is no `GetPage` rung: offset paging outside a transaction is not a
snapshot (each page is its own statement; a concurrent insert or delete that
sorts before the cursor shifts the window, so a match can be skipped or
returned twice). A skipped match is a wrong-but-available answer.

There is no third rung: `Iterate` becomes a required `EntityStore` method
(§3.2), so the five "store supports neither" refusal branches are dead and
deleted: `entity/service.go:835`, `:1228`, `:1538`, `search/service.go:1337`,
`entity/grouped_stats_service.go:280-285`.

### 2.3 Async

- `runAsyncJob`: the translate-failure branch that called `Search` is
  deleted. A translation failure at execution fails the job closed via
  `writeAsyncFailure`, like the branch beside it.
- `SubmitAsync` dry-runs `spi.ConditionToFilter` against the FieldsMap
  `validateConditionPaths` already returns (currently discarded) and answers
  with §2.1's classification on failure. A persisted job therefore carries a
  condition that translated once; an execution-time failure can only mean
  the schema changed after submission. Cost: one translation per submit.

## 3. SPI — `cyoda-go-spi` (one PR, merged before the row-14 tag)

1. **`EntityStore.GetAll` and `GetAllAsAt` are removed.** Replacements:
   `GetPage` (paged, random access) and `Iterate` with a zero-value filter
   (one cursor, streamed).
2. **`Iterate` moves onto `EntityStore`; the `Iterable` interface is
   deleted.** Every engine path that reads more than one entity requires it
   and fails closed without it; all four in-house backends implement it.
   `Iterator` and `IterateOptions` stay in `iterable.go` beside the method's
   doc. `Searcher` stays optional as the bounded-or-fail optimisation. The only
   `Searcher`-without-`Iterate` shape the SPI contemplates (self-executing
   async, `search_store.go:76-88`) already cannot serve delete-all,
   conditional delete or grouped stats.
3. spitest: `GetAll` empty/populated, `GetAllAsAt`,
   `GetAllAsAt/CommittedOnlyInTx`, `TenantIsolation/GetAll` and
   `testEntityEmptyTenant`'s `GetAll` read are deleted or moved to
   `GetPage`; **`TenantIsolation/GetPage` is added** (absent today). The
   `Iterable` suite stops gating on a type assertion (18 assertion
   sites: 17 in `spitest/iterable.go`, 1 in `filter_not.go:196`). The test double
   `default_save_all_test.go:15` drops `GetAll` and gains `Iterate`
   (`persistence_extendschema_test.go` is a `ModelStore` double and is
   unaffected).
4. Doc comments: `searcher.go:8-11` ("fall back to in-memory filtering") and
   `:26-28` ("identical to what GetAll + in-memory match would produce"),
   and the former `iterable.go:42-46` paragraph, state the new contract.
5. `CHANGELOG.md` `### Breaking` with migration notes. `MAINTAINING.md`'s
   pre-1.0 break conditions: the cassandra#95 addendum (§7) is the
   KNOWN_CONSUMERS notification, linked from the PR; the entry states why
   there is no deprecation window — a deprecated `Iterable` alias would keep
   `store.(spi.Iterable)` compiling and hide exactly the dead branches the
   change removes.

## 4. Plugins — memory, sqlite, postgres

Sibling defects of the same family, fixed in the same row. Items 4.1–4.6
need no SPI change and ship first (§8).

### 4.1 sqlite `Begin` gates on in-flight commits (prerequisite)

`Commit` bumps `lastSubmitTime` at step 4 (`txmanager.go:353-366`) before its
`sqlTx` commits; `Begin` floors `SnapshotTime` to `lastSubmitTime` under
`m.mu` only (`:236-246`). A transaction begun in that window has
`SnapshotTime >= submitTime` of a commit whose rows are not yet visible on
`readDB`, and the first-committer-wins check at `:333` excludes that commit
from conflict detection: a lost update. Today every in-tx snapshot read runs
on the single writer connection (`store_factory.go:101`) and queues behind
the flush, which hides the window. Any in-tx read on `readDB` needs the
window closed first.

Fix: `Begin` acquires the same gate the flush holds (`commitMu`, held from
step 2 through `sqlTx.Commit`) around its floor-and-capture. Lock order
`commitMu → m.mu`, the order `Commit` already uses; `tx.OpMu` is not
involved. The seam is the transaction manager only (`txmanager.go`);
entity-store code never reads `lastSubmitTime`.

Cheaper alternative, rejected: capture a sequence baseline atomically with
`SnapshotTime`, as the memory plugin does (`plugins/memory/txmanager.go:249-276`).
That closes the conflict-detection half of the window but not the
visibility half — a `readDB` cursor could miss commit X's rows while a later
`s.db` read in the same transaction queues behind the flush and sees them:
two snapshots in one transaction.

Test (in-package, no production hook): hold `m.commitMu`, bump
`m.lastSubmitTime` under `m.mu` to stand in for step 4, start `Begin` on a
goroutine and assert it blocks until release; then assert its
`SnapshotTime`, that it sees the committed rows, and that its conflicting
commit is refused.

### 4.2 `unstageDelete` seals the buffer invariant

`Save` clears only `tx.Deletes` (memory `:187`, sqlite `:255`) though the SPI's
`txcontext.go:19-23,108` says `Deletes` and `DeleteAttribution` always cover
the same key set; `CompareAndSave` clears neither (memory `:224-237`, sqlite
`:392-393`), so delete-then-CAS leaves an id in both `Buffer` and `Deletes`
and commits a tombstone while in-tx reads disagree. One `unstageDelete(tx,
id)` helper per plugin, deleting from both maps, called from `Save` and
`CompareAndSave`. The committed outcome is a contract every backend shares
and is reachable over the wire (`DELETE /entity/{id}` then `PUT` with
`If-Match` under one `X-Tx-Token`, then commit), so it is pinned at the
cross-backend seams: spitest `Transaction/DeleteThenSave` and
`Transaction/DeleteThenCompareAndSave` (PR 2), and an HTTP parity scenario
registered in `e2e/parity/registry.go` (PR 3). Plugin unit tests drive the
red/green in PR 1 and stay.

### 4.3 In-tx `Count` / `CountByState` count over the merge cursor (memory, sqlite)

One in-transaction read path, not two: the in-tx counts drain the §4.4
merge cursor with an `(entity_id, state)` projection — committed rows
selecting id and state only, merged with the buffer, deletes excluded — and
tally. O(1) memory, no payload bytes, no new SQL, and by construction the
same view `Iterate` and `GetPage` return. Time is O(model ids), which is
what the non-tx `Count` costs the database anyway; the property under
repair is memory, not latency. Memory walks its committed pointers under
`entityMu.RLock` and tallies without copying. Postgres counts on the
transaction's connection and needs nothing.

Rejected: set arithmetic (`committedTotal − committed(id IN buffered ∪
deleted) + buffered`) with the id set passed through `json_each`. O(buffer)
time, but a second in-tx read mechanism with three formulas that must agree
with the cursor's view and new SQL surface to pin.

### 4.4 sqlite in-tx `Iterate` and `GetPage` stream through `MergeOrdered`

Replace `getAllTx` with `spi.MergeOrdered` over a committed cursor on
`readDB` at `tx.SnapshotTime` and the sorted buffered adds — the unbounded
twin of `searchTxOverlay`'s `MergeBounded`, and built the same way:

- The committed cursor is `searchSnapshotBase(tx.SnapshotTime)` plus
  `planFor(filter).where` pushed into SQL, with `plan.preparedPostFilter`
  applied in Go to each committed row, exactly as `searchTxOverlay`
  (`plugins/sqlite/searcher.go:235+`). The buffered adds are filtered by the
  same `spi.Prepare(filter)` value. Order specs for both inputs are
  `[]spi.OrderSpec{{Source: spi.SourceMeta, Path: "id"}}` (canonical ID
  order; `Iterate` rejects a caller `OrderBy` in-tx, `GetPage` has none).
- `adds` and `tx.Deletes` are copied into locals at open under
  `tx.OpMu.RLock` (the overlay is a snapshot at call time).
- With `TrackingRead`, recording happens **only when `Next()` returns
  true** — post-residual, matches only — under a short `OpMu.RLock` that
  checks `RolledBack` and `Closed`; a closed transaction ends iteration with
  `spi.ErrTxAlreadyCommitted` (a `Commit` takes `OpMu.Lock` between two
  yields and would otherwise let an open iterator record into a committed
  transaction).
- Pool assumption: the cursor's consumer never needs a second `readDB`
  connection to progress — in-tx `Get`/`getSnapshot` and `searchTxOverlay`
  stay on `s.db`. `searchTxOverlay` is not moved: it is bounded and
  single-statement, and moving it would widen this PR for no property gain.

`getPageTx` uses the same stream, discarding the first `offset` merged rows
as they are pulled and recording only the returned page; memory bounded to
`limit + len(adds)`. `getAllTx` then has no caller and is deleted. Tests:
same-tx `Delete` while an iterator is open does not deadlock; a `Commit`
while an iterator is open ends it with `ErrTxAlreadyCommitted`; the yielded
set equals the previous materialised set (the old order was map order) and
the sequence is `ORDER BY entity_id`; a page equals the corresponding slice
of the full sequence; with a filter that excludes some committed rows, the
excluded ids are absent from `tx.ReadSet` (spitest's `TrackingRead/Gating`
checks a single yielded id and would not catch pre-residual recording).

### 4.5 memory `Iterate` records on yield; `Search` copies survivors only

`buildSnapshot` records the whole merged model into the read-set at open
when `TrackingRead` (`grouped_stats.go:150-165`); it records per yield
instead, the pattern 4.4 introduces. `GroupedAggregate` has no yield and
the backends disagree today — memory records the whole model
(`grouped_stats.go:357`), sqlite (`:285`) and postgres (`:377`) record
nothing. The rule for all backends: **in-transaction grouped stats records
nothing** (the engine passes no `TrackingRead` to `Iterate` for stats
either, `grouped_stats_service.go:332`). Memory drops the recording; a
spitest case pins it on every backend. `Search` takes a pointer snapshot under
`entityMu.RLock` (the shape `currentStatePointersUnlocked` and
`getAllSnapshotPointersUnlocked` already have), releases it, filters
lock-free — the stored `*spi.Entity` is immutable: `saveUnlocked` and the
commit flush build a fresh entity via `copyEntity` — and copies only the
survivors `matchSortBounded` keeps (at most `Limit + 1`) — survivors from
both streams, committed and the transaction's own buffer, so no caller
aliases live buffer state. `tx.OpMu.RLock` stays across the in-tx branch
as today.

### 4.6 In-tx `DeleteAll` stages ids without payloads

memory `:677` copies every entity via `getAllSnapshotUnlocked` to stage ids;
sqlite `:764-777` selects `json(data), json(meta)` to read `entity_id`. Both
walk ids only.

### 4.7 After the SPI change

Delete `GetAll`/`GetAllAsAt`; the `Iterable` compile-time assertions become
`EntityStore` ones. `make repin-plugins` after every plugin change.

Documented, not changed: the memory store keeps no per-model index, so every
model read is an O(tenant) pointer walk and in-tx `GetPage` sorts a pointer
slice per page. The property is "no payload bytes copied beyond the result",
not "no O(n) pointer slice".

## 5. Error/status-code table (one code withdrawn; everything else re-verified)

| Endpoint | Codes that must still hold |
|---|---|
| `POST /search/direct/{entityName}/{modelVersion}` | 200; 400 `INVALID_CONDITION`, `INVALID_FIELD_PATH`, `SEARCH_RESULT_LIMIT`, `BAD_REQUEST` (limit ≤ 0, body); 401; 404 `MODEL_NOT_FOUND`; 408 |
| `POST /search/async/{entityName}/{modelVersion}` | 200 submit; 400 `INVALID_CONDITION`/`INVALID_FIELD_PATH`; 401; 404; 503 `SEARCH_QUEUE_FULL` |
| `GET /search/async/{jobId}` `/status` `/cancel` | unchanged |
| `DELETE /entity/{entityName}/{modelVersion}` (conditional, delete-all) | 200; 400; 401; 404; 409 `DELETE_NOT_CONVERGED` |
| `GET /entity/{entityName}/{modelVersion}` (list) | 200; 400; 401; 404 |
| `POST /entity/stats/…/query` (grouped) | 200; 400; 404 — **`501 NOT_IMPLEMENTED_BY_BACKEND` is withdrawn**: its only trigger (a store with neither `Iterable` nor `GroupedAggregator`) cannot exist once `Iterate` is required. The code is retired everywhere (§8), with a `### Breaking` line and a `docs/cloud-parity/` note |
| gRPC search / delete / list | existing envelope codes per class |

New 400s (§2.1 nil condition, translation failure) are unreachable from a
client and covered at unit level. The withdrawn 501 is a Gate-7 contract
change: a new `docs/cloud-parity/grouped-stats-iterate-required.md` (no
grouped-stats contract file exists today; add the row to that folder's
`README.md` index) states in one paragraph: `Iterate` is required of every backend, so the 501 has no
trigger and is withdrawn; Cloud must not answer it.

## 6. Coverage matrix

| Scenario | unit | e2e (postgres) | parity | gRPC |
|---|---|---|---|---|
| `Search` with `Limit <= 0` errors | ✓ | — (unreachable) | — | — |
| nil condition → 400 `INVALID_CONDITION` (`Search`, `SubmitAsync`) | ✓ | — (unreachable) | — | — |
| translation failure → `400 INVALID_CONDITION` through `Search` (caller-built condition type) | ✓ | — (unreachable) | — | — |
| `ClassifyStoreQueryError` maps a wrapped `spi.ErrInvalidFilterPath` to `INVALID_FIELD_PATH` (direct test; the leg is unreachable through `Search`) | ✓ | — | — | — |
| non-`Searcher` store → `Iterate` rung: correct result, sorted, bound raises after `Limit+1` yields, iterator closed | ✓ | — (all backends are `Searcher`) | — | — |
| `Iterate` rung agrees with `Searcher` rung on a numeric sort key (carried from the deleted e2e test) | ✓ | — | — | — |
| `Iterate` rung: pre-expired ctx and mid-drain cancellation → `context.DeadlineExceeded` in the error chain (408 at the handler; carried from `handler_timeout_test.go:162`) | ✓ | — | — | — |
| `Iterate` rung: type-directed comparison through the plugin kernel with a registered model; schema-load failure fails closed (carried from `fallback_typed_test.go:70,117`) | ✓ | — | — | — |
| `Iterate` rung: a bare leaf field the kernel cannot evaluate → `400 INVALID_CONDITION` (carried from `classify_store_query_error_test.go:345`) | ✓ | — | — | — |
| async submit dry-run 400; execution-time failure → job FAILED | ✓ | — | — | — |
| every existing search/delete/list/stats status code still holds | — | ✓ (existing suites) | ✓ (existing) | ✓ (existing) |
| sqlite `Begin` gate: tx begun mid-flush sees the flush and its conflicting commit is refused | ✓ (plugin) | — | — | — |
| `unstageDelete`: delete-then-Save / delete-then-CAS committed outcome | ✓ (memory, sqlite; PR 1) | — | ✓ spitest `Transaction/DeleteThenSave`, `Transaction/DeleteThenCompareAndSave` (PR 2); HTTP parity scenario delete-then-CAS under one `X-Tx-Token` (PR 3) | — |
| in-tx `Count`/`CountByState`: create, update, delete committed, create-then-delete, delete-then-CAS, state change, after `DeleteAll` | ✓ (memory, sqlite; PR 1) | — | ✓ spitest in-tx count case (`spitest/entity.go:312` today covers one shape) extended to the seven shapes (PR 2) | — |
| in-tx `GroupedAggregate` records nothing into the read-set, on every backend | ✓ (memory) | — | ✓ spitest (PR 2) | — |
| sqlite in-tx `Iterate`/`GetPage` via `MergeOrdered`: sequence equality, page slice equality, no deadlock with same-tx `Delete`, `TrackingRead` gating | ✓ (plugin) | — | ✓ (spitest, existing) | — |
| memory `Iterate` per-yield recording; `Search` survivor-only copies | ✓ (plugin) | — | ✓ (spitest `TrackingRead` gating) | — |
| in-tx `DeleteAll` stages ids only | ✓ (plugin) | — | — | — |
| spitest `TenantIsolation/GetPage` | — | — | ✓ (spitest, new) | — |

Waived cells carry their reason inline: the scenario is unreachable over
the wire on every shipped backend.

## 7. Commercial backend obligations (cassandra#95 addendum)

`GetAll`/`GetAllAsAt` removed (`entity_store.go:1275`, `:1442`); `Iterate`
becomes a required `EntityStore` method (already implemented,
`grouped_stats.go:93`); `model_store.go:252` reads meta-model entities via
`es.GetAll` and moves to `Iterate`; `internal/integration/iterate_test.go:114-118`
and `internal/store/grouped_stats.go:90` drop their `Iterable` assertions.

## 8. Delivery — three PRs, all to `release/v0.8.4`

1. **cyoda-go, plugin internals** (§4.1–4.6): no SPI change, each item
   red/green with its own failing test, `make repin-plugins`.
2. **cyoda-go-spi** (§3): removals, `Iterate` required, spitest, docs,
   CHANGELOG. Merged and pin moved to its commit before PR 3.
3. **cyoda-go, engine + pin bump** (§2, §4.7, tests, docs): the
   compiler-driven removal plus the routing change.

PR 3 also carries:

- Test migration: 70 `GetAll` lines in 17 test files move to a shared
  `Iterate` drain helper. Two are not mechanical: the parity RYW oracle
  (`e2e/parity/txsearchryw/tx_search_ryw_test.go:198-231`, "Search = GetAll +
  Prepare.Match") becomes an `Iterate` drain plus `Prepare.Match`; the
  read-set control leg (`internal/e2e/list_intx_readset_test.go:154-201`)
  becomes a full `GetPage` drain, which records identically.
  `service_list_test.go:23`'s `noWholeModelEntityStore` is deleted: the
  compiler now proves what it pinned. `mock_store_test.go:47`
  (`failingEntityStore`) gains `Iterate`.
- The six unit tests that pin the old fallback, and their fate:
  `TestSearch_FallbackBranchIsBounded` (`service_test.go:2359`) → rewritten as
  the `Iterate`-rung bound test; `TestSearch_FallbackBranchUnboundedReturnsAll`
  (`:2387`) → deleted, replaced by the `Limit <= 0` error test;
  `TestSearch_FallbackBranchIsBounded_TranslateFailureRoute` (`:2468`) →
  deleted, replaced by the nil-condition 400 test and the caller-built-type
  translation-failure test; `fallback_typed_test.go:70,117` → rewritten on the
  `Iterate` rung (§6 rows); `handler_timeout_test.go:162` → rewritten on the
  `Iterate` rung; `classify_store_query_error_test.go:345` → rewritten on the
  `Iterate` rung. All through the existing `nonSearcherEntityStore` fixture.
- `TestSearchSort_PushdownFallbackAgree` (`internal/e2e/search_test.go:777`)
  is deleted: its premise is stale since row 11 (a wildcard leaf translates)
  and it compares pushdown with itself. Its sort-agreement assertion lives
  in the §6 unit contract test.
- Docs: `cmd/cyoda/help/content/search.md` (:49, :373);
  `docs/cloud-parity/tx-aware-search.md` (the `:14-16` fallback sentence and
  every `GetAllAsAt` mention — the PIT carve-out list and invariant 1);
  `docs/cloud-parity/direct-search-bounded-or-fail.md` (invariants 1–2, the
  Behaviour bullet "`limit <= 0` means unbounded", the SPI-level "`Limit <= 0`
  MUST be treated as unbounded" bullet — already contradicted by
  `searcher.go:17-22` — and the "Known commercial-backend gap" paragraph,
  all of which invert once `Limit <= 0` is an engine error);
  `docs/cloud-parity/path-grammar.md` §11 ("the bounded search call and the
  unbounded streaming drain alike" → "the `Searcher` call and the `Iterate`
  rung alike"); `docs/CONSISTENCY.md` — §3c (`:200`) prescribes `GetAll` as
  the always-tracking fence primitive: the replacement is `GetPage`, which
  records every returned page unconditionally, or `Iterate` with
  `TrackingRead`, which records yields only — the two record differently
  and §3c must say which serves a fence (a `GetPage` drain does; a
  predicate `Iterate` has the same subset gap §3c already describes for
  `Search`); also `:508` and the Appendix A/B walkthroughs
  (`:649, :709-764, :855-893, :1002-1167`); `cmd/cyoda/help/content/crud.md:533-536`
  (the in-tx iterator "materializes via getAllTx" / `buildSnapshot`
  description) and `workflows.md:174`; `docs/ARCHITECTURE.md`;
  `docs/plugins/*.md`; `CHANGELOG.md`; `COMPATIBILITY.md` (narrate the wave
  with the pin move).
- `NOT_IMPLEMENTED_BY_BACKEND` retired in full — `TestErrCode_Parity`
  (`cmd/cyoda/help/help_test.go:549`) enforces the code↔topic bijection, so
  constant and topic go together: `internal/common/error_codes.go:157-162`,
  `cmd/cyoda/help/content/errors/NOT_IMPLEMENTED_BY_BACKEND.md`,
  `errors.md:97`, `crud.md:541,579,656`, `api/openapi.yaml:1151,1161,1277-1280`,
  `internal/e2e/zzz_errorcode_matrix_test.go:61`,
  `grouped_stats_handler_test.go`, `grouped_stats_service_test.go`,
  `COMPATIBILITY.md`, `CHANGELOG.md` (`### Breaking`).
- Rollback: PR 1 reverts independently; PR 3 reverts by re-pinning
  `f6863ae`; an SPI tag is immutable — a bad tag is superseded by a fresh
  version, never moved.

## 9. Exit checks (greppable)

- `grep -rn 'GetAll(ctx, .*ModelRef\|GetAllAsAt\|getAllTx\|spi\.Iterable' --include='*.go'`
  over the three repos' non-test code: empty (the compiler enforces the
  entity-store methods; the grep also catches prose). `ModelStore.GetAll`
  (`persistence.go:171`, returns `[]ModelRef`) is out of scope and keeps its
  five production callers.
- `grep -rn 'GetAll\b' docs cmd/cyoda/help/content README.md`: no prose
  describes entity reads by reference to it.
- `grep -rn GetAll plugins/ internal/domain/search`: no comment describes
  behaviour by reference to it (about forty do today).
- `grep -n 'cond == nil\|drainIterate\|ErrBackendNotSupported' internal/domain/search/service.go internal/domain/entity/*.go`:
  only the bounded drain remains; the `drainIterate` mention at
  `entity/grouped_stats_service.go:351` is updated.
- `make test-full` green; `make race` once before each PR.

## 10. Accepted trade-offs

- **No new error code for a translation failure.** It is unreachable from
  validated input; `INVALID_CONDITION` / `INVALID_FIELD_PATH` via
  `ClassifyStoreQueryError` already name the fault.
- **`Limit <= 0` in `Search` is a plain error, not a 400.** No transport can
  produce it.
- **The `Iterate` rung may leave up to `Limit + 1` entities in the read-set
  on a 400.** A prefix, never the model; only a store without `Searcher`
  reaches the rung.
- **No deprecation window for `Iterable` or `GetAll`.** A deprecated alias
  would keep the dead refusal branches compiling. Pre-1.0, `### Breaking`
  with migration notes and the KNOWN_CONSUMERS notification (cassandra#95).
- **sqlite `Begin` waits for an in-flight commit's flush** (§4.1). Begin
  latency under a concurrent commit is the price of a correct snapshot:
  sqlite's row visibility on `readDB` is not atomic with its
  `lastSubmitTime` bump, and only serialising `Begin` behind the flush makes
  "`SnapshotTime >= submitTime`" imply "rows visible". (The memory plugin
  has no such window — its visibility is atomic under `entityMu` and its
  conflict check is sequence-based — so it pays nothing.)
- **The memory plugin walks an O(tenant) pointer slice per model read and
  sorts pointers per page.** No payload bytes are copied; a per-model
  sorted index is not kept.
- **Three PRs, not one.** Reviewability over a single atomic landing; the
  plugin PR is independently correct.

## 11. Out of scope

- `internal/match` traversal sharing with the kernel (#464).
- The memory plugin's O(tenant) pointer walk per model read (§4.7 note).
- Async job wire-syntax translation (spi#31, closed not-planned).
