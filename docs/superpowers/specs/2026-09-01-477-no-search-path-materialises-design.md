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
  `spi.ErrInvalidFilterPath`, `spi.ErrUnknownOperator` and the pattern
  compiler's error; the client-visible code must not depend on which layer
  rejected the input), then defaults to `400 INVALID_CONDITION` carrying the
  translator's message. No new error code: the branch is unreachable from
  validated input (research §3a) and could never earn e2e coverage.

### 2.2 Routing

```
store is spi.Searcher  →  Searcher.Search(filter, opts)          (unchanged)
otherwise              →  Iterate(filter) drained to Limit+1     (bounded drain)
```

The second rung: `Iterate` with the translated filter (pushdown at the
store, residual inside `Next()` by the same kernel), `PointInTime` and
`TrackingRead` forwarded, `OrderBy` left out of `IterateOptions` (in-tx
ordered iteration is an SPI error) and the bounded match set sorted by
`sortEntities` (its empty-spec fallback is the canonical ID tiebreaker, so
both rungs return canonical order). The `Limit + 1`-th yield raises
`400 SEARCH_RESULT_LIMIT` with `spi.ErrSearchResultLimitExceeded` as cause
and closes the iterator. Peak memory `Limit + 1` entities. On this rung a
400 may leave up to `Limit + 1` entities recorded in the read-set — a prefix.

There is no `GetPage` rung: offset paging outside a transaction is not a
snapshot (each page is its own statement; a concurrent insert or delete that
sorts before the cursor shifts the window, so a match can be skipped or
returned twice). A skipped match is a wrong-but-available answer.

There is no third rung: `Iterate` becomes a required `EntityStore` method
(§3.2), so the "store supports neither" refusal branches — five of them
across search, delete and grouped stats — are dead and deleted.

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
   `Searcher` stays optional as the bounded-or-fail optimisation. The only
   `Searcher`-without-`Iterate` shape the SPI contemplates (self-executing
   async, `search_store.go:76-88`) already cannot serve delete-all,
   conditional delete or grouped stats.
3. spitest: `GetAll` empty/populated, `GetAllAsAt`,
   `GetAllAsAt/CommittedOnlyInTx`, `TenantIsolation/GetAll` and
   `testEntityEmptyTenant`'s `GetAll` read are deleted or moved to
   `GetPage`; **`TenantIsolation/GetPage` is added** (absent today). The
   `Iterable` suite stops gating on a type assertion (about 25 assertion
   sites in `spitest/iterable.go` and `filter_not.go:196`). Test doubles
   `default_save_all_test.go:15` and `persistence_extendschema_test.go`
   drop `GetAll` and gain `Iterate`.
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
involved. Test: pause a commit mid-flush with a test hook, begin a second
transaction, iterate on it, assert it sees the committed rows, then assert
its conflicting commit is refused.

### 4.2 `unstageDelete` seals the buffer invariant

`Save` clears only `tx.Deletes` (memory `:187`, sqlite `:255`) though
`txcontext.go:19-23,108` says `Deletes` and `DeleteAttribution` always cover
the same key set; `CompareAndSave` clears neither (memory `:224-237`, sqlite
`:392-393`), so delete-then-CAS leaves an id in both `Buffer` and `Deletes`
and commits a tombstone while in-tx reads disagree. One `unstageDelete(tx,
id)` helper per plugin, deleting from both maps, called from `Save` and
`CompareAndSave`. Test asserts the committed outcome after `Commit`.

### 4.3 In-tx `Count` / `CountByState` in O(buffer) (memory, sqlite)

`count = committedTotal(snapshot) − committed(id IN buffered ∪ deleted) +
|buffered entities of this model not in tx.Deletes|`. `CountByState` is the
same with `GROUP BY state`, the buffered entity supplying its own state.
sqlite passes the id set as one JSON-array parameter through `json_each`
(one query, no variable-number cap; the plugin already depends on JSON
functions). `tx.Deletes` carries no model; a deleted id not committed in
this model contributes nothing to the `IN` term by construction. Memory
walks its committed map without copying. Postgres counts on the
transaction's connection and needs nothing.

### 4.4 sqlite in-tx `Iterate` and `GetPage` stream through `MergeOrdered`

Replace `getAllTx` with `spi.MergeOrdered` over a committed cursor on
`readDB` at `tx.SnapshotTime` and the sorted buffered adds — the unbounded
twin of `searchTxOverlay`'s `MergeBounded`. `adds` and `tx.Deletes` are
copied into locals at open under `tx.OpMu.RLock` (the overlay is a snapshot
at call time). With `TrackingRead`, each yield records under a short
`OpMu.RLock` with a `RolledBack` check. `getPageTx` uses the same stream,
discarding the first `offset` merged rows as they are pulled and recording
only the returned page; memory bounded to `limit + len(adds)`. `getAllTx`
then has no caller and is deleted. Test: same-tx `Delete` while an iterator
is open does not deadlock; yield sequence equals the previous materialised
sequence; a page equals the corresponding slice of the full sequence.

### 4.5 memory `Iterate` records on yield; `Search` copies survivors only

`buildSnapshot` records the whole merged model into the read-set at open
when `TrackingRead` (`grouped_stats.go:150-165`); it records per yield
instead, the pattern 4.4 introduces. `Search` takes a pointer snapshot under
`entityMu.RLock` (the shape `currentStatePointersUnlocked` and
`getAllSnapshotPointersUnlocked` already have), releases it, filters
lock-free — the stored `*spi.Entity` is immutable: `saveUnlocked` and the
commit flush build a fresh entity via `copyEntity` — and copies only the
survivors `matchSortBounded` keeps (at most `Limit + 1`). `tx.OpMu.RLock`
stays across the in-tx branch as today.

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

## 5. Error/status-code table (verification — the wire contract is unchanged)

| Endpoint | Codes that must still hold |
|---|---|
| `POST /search/direct/{entityName}/{modelVersion}` | 200; 400 `INVALID_CONDITION`, `INVALID_FIELD_PATH`, `SEARCH_RESULT_LIMIT`, `BAD_REQUEST` (limit ≤ 0, body); 401; 404 `MODEL_NOT_FOUND`; 408 |
| `POST /search/async/{entityName}/{modelVersion}` | 200 submit; 400 `INVALID_CONDITION`/`INVALID_FIELD_PATH`; 401; 404; 503 `SEARCH_QUEUE_FULL` |
| `GET /search/async/{jobId}` `/status` `/cancel` | unchanged |
| `DELETE /entity/{entityName}/{modelVersion}` (conditional, delete-all) | 200; 400; 401; 404; 409 `DELETE_NOT_CONVERGED` |
| `GET /entity/{entityName}/{modelVersion}` (list) | 200; 400; 401; 404 |
| `POST /entity/stats/…/query` (grouped) | 200; 400; 404 — **501 for "neither capability" is gone** (dead: `Iterate` is required) |
| gRPC search / delete / list | existing envelope codes per class |

New 400s (§2.1 nil condition, translation failure) are unreachable from a
client and covered at unit level.

## 6. Coverage matrix

| Scenario | unit | e2e (postgres) | parity | gRPC |
|---|---|---|---|---|
| `Search` with `Limit <= 0` errors | ✓ | — (unreachable) | — | — |
| nil condition → 400 `INVALID_CONDITION` (`Search`, `SubmitAsync`) | ✓ | — (unreachable) | — | — |
| translation failure → classified 400 (both codes) | ✓ | — (unreachable) | — | — |
| non-`Searcher` store → `Iterate` rung: correct result, sorted, bound raises after `Limit+1` yields, iterator closed | ✓ | — (all backends are `Searcher`) | — | — |
| `Iterate` rung agrees with `Searcher` rung on a numeric sort key (carried from the deleted e2e test) | ✓ | — | — | — |
| async submit dry-run 400; execution-time failure → job FAILED | ✓ | — | — | — |
| every existing search/delete/list/stats status code still holds | — | ✓ (existing suites) | ✓ (existing) | ✓ (existing) |
| sqlite `Begin` gate: tx begun mid-flush sees the flush and its conflicting commit is refused | ✓ (plugin) | — | — | — |
| `unstageDelete`: delete-then-Save / delete-then-CAS committed outcome | ✓ (memory, sqlite) | — | — | — |
| in-tx `Count`/`CountByState`: create, update, delete committed, create-then-delete, delete-then-CAS, state change, after `DeleteAll` | ✓ (memory, sqlite) | — | ✓ (existing in-tx count parity keeps passing) | — |
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
- `TestSearchSort_PushdownFallbackAgree` (`internal/e2e/search_test.go:777`)
  is deleted: its premise is stale since row 11 (a wildcard leaf translates)
  and it compares pushdown with itself. Its sort-agreement assertion lives
  in the §6 unit contract test.
- Docs: `cmd/cyoda/help/content/search.md` (:49, :373),
  `docs/cloud-parity/tx-aware-search.md` (:14-16),
  `docs/cloud-parity/direct-search-bounded-or-fail.md` (invariants 1–2),
  `docs/ARCHITECTURE.md`, `docs/plugins/*.md`, `CHANGELOG.md`,
  `COMPATIBILITY.md` (narrate the wave with the pin move).

## 9. Exit checks (greppable)

- `grep -rn 'GetAll\b\|GetAllAsAt\|getAllTx\|Iterable' --include='*.go'`
  over the three repos' non-test code: empty (the compiler enforces the
  first two; the grep also catches prose).
- `grep -rn GetAll plugins/ internal/domain/search`: no comment describes
  behaviour by reference to it (about forty do today).
- `grep -n 'cond == nil\|drainIterate\|ErrBackendNotSupported' internal/domain/search/service.go internal/domain/entity/*.go`:
  only the bounded drain remains; the `drainIterate` mention at
  `entity/grouped_stats_service.go:351` is updated.
- `make test-full` green; `make race` once before each PR.

## 10. Out of scope

- `internal/match` traversal sharing with the kernel (#464).
- The memory plugin's O(tenant) pointer walk per model read (§4.7 note).
- Async job wire-syntax translation (spi#31, closed not-planned).
