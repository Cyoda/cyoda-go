# Transaction-control params: `transactionTimeoutMillis`, `transactionSize`, search `timeoutMillis`

**Issue:** #379 (supersedes #372) · **Milestone:** v0.8.4 (PATCH — settled, see issue) · **Date:** 2026-08-11

## Problem

Write/delete endpoints document transaction-control query parameters that the server binds but
never reads. `transactionTimeoutMillis` is declared on **8** entity/message write ops (the issue
says 7; `updateSingleWithLoopback` also declares it), `transactionSize` on the 2 bulk-delete ops.
The gRPC CloudEvent request schemas declare the same knobs (`transactionTimeoutMs` on the 5 entity
write events, `transactionSize` on `EntityDeleteAllRequest`, `timeoutMillis` on
`EntitySearchRequest`) — equally unread. `searchEntities` had its fictional `timeoutMillis` + 408
removed in v0.8.2 with the recorded intent to re-add them with a real implementation (#372, folded
in here). No 408 path and no timeout error code exist anywhere in the tree.

## Settled constraints (do not re-litigate)

- **v0.8.4 is a PATCH.** Honoring the params must be opt-in: absent param ⇒ behavior unchanged.
  Two declared no-param-visible alignments ship with it (both backend-divergence/correctness
  fixes, called out in the changelog): the D2 commit shield also covers client disconnects
  mid-commit, and D9 aligns disconnect-abort behavior across backends.
- **No defaults.** The issue's version policy requires that neither param gets a default. The spec
  *currently* declares `default: 10000` / `default: 1000` — never enforced, so the defaults are
  fictional contract surface. They are **removed** (same reconciliation class as v0.8.2's removal
  of search `timeoutMillis`), not implemented. Same for the baked `default: 1000` in the
  `EntityDeleteAllRequest` JSON schema (field becomes nullable, regenerated via
  `scripts/generate-events.sh`). cyoda-go leads the contract (Gate 7); logged in
  `docs/cloud-parity/`.
- Backends must behave identically on the same contract; enforcement must work on memory/sqlite,
  not only where the database happens to cancel queries.

## Design decisions

**D1 — Naming stays split by concept.** Writes bound a *transaction*: `transactionTimeoutMillis`
(HTTP) / `transactionTimeoutMs` (gRPC events). Search bounds a *query*: `timeoutMillis` on both
doors. This matches the upstream Cloud contract on both surfaces; no rename.

**D2 — Timeout semantics: 408 ⇔ nothing committed, made structural by shielding commits.** When
present and valid, the executing node wraps the operation in `context.WithTimeout` before
beginning the transaction. Deadline expiry before commit fails the operation, rolls back, and
returns `408 TRANSACTION_TIMEOUT` (retryable, `application/problem+json`). The guarantee is made
structural, not probabilistic:

- **Pre-commit check:** immediately before commit, if the deadline has expired → rollback → 408.
- **Shielded commit:** the commit call itself (final commit and every CBD segment commit) runs on
  a `context.WithoutCancel`-derived context with its own budget (the `common.RollbackContext`
  pattern; a sibling `CommitContext`). A deadline can therefore never cancel a commit in flight
  and produce an in-doubt "408 but durable" outcome. This also closes the pre-existing in-doubt
  hazard for client disconnects mid-commit (declared in the changelog; before-state differs by
  backend — postgres/sqlite abort the flush today, memory completes — after: all complete). The
  postgres acquire-deadline precedent (`plugins/postgres/ceilings.go:88-99`) already keeps
  *unintended* deadlines off the transaction handle; the shield extends that discipline to the
  commit.
- **`newMessage` gets the same shape:** no transaction exists on that path, so the store `Save`
  (an autocommit statement — its completion *is* the commit boundary) is shielded identically:
  pre-Save deadline check → 408; the `Save` itself runs on the shielded context; save-wins. The
  deadline therefore bounds the pre-Save phase (validation, store/pool acquisition) and can never
  produce "408 but saved". The shield budget bounds the Save itself.
- **Commit wins:** once commit succeeds, the response is success regardless of when the deadline
  fired. Verified: in all write flows `scope.Commit()` is the last error-returning operation, and
  all post-first-segment engine work already rides an uncancellable context — so with the shield
  there is no path to a false 408 after a durable commit.
- **Classifier rule (pinned):** a 408 is produced only when the operation error's chain contains
  the deadline's own `context.DeadlineExceeded` (`errors.Is`, unwrapping `AppError` causes —
  load-bearing, since some paths pre-wrap the ctx error in a classified `AppError`) **and** the
  attached marker identifies the deadline as ours. Never from ctx state alone: a shielded-commit
  failure, a commit-time `ErrConflict` (409), or any unrelated error keeps its existing
  classification even if the deadline has since expired.
- Rollback runs on `common.RollbackContext` (existing precedent), so the expired deadline never
  poisons the rollback.

**D3 — Operations that commit more than once: the timeout applies until the first commit.** Two
shapes exist:
- *Commit-before-dispatch workflows:* the engine re-derives segment contexts via
  `context.WithoutCancel` after a CBD segment commits (committed-segment cleanup must complete).
  The first segment commit itself is shielded per D2; from that point on the operation is
  durable-in-part, runs to completion, and the timeout is no longer enforced.
- *Chunked collection writes* (`create`/`createCollection`/`updateCollection` under
  `transactionWindow`): one budget for the whole request. A 408 is only possible while **no**
  chunk has committed. Once chunk 1 is durable, deadline expiry surfaces through the existing
  per-chunk contract — HTTP 200 with error elements (carrying code `TRANSACTION_TIMEOUT`) for
  the chunks not processed — never a 408.

Both are the same principle forced by D2: a 408 is a guarantee that nothing was committed.

**D4 — `transactionSize` batching: ids resolved once, committed per batch, version-guarded,
failures don't stop later batches.** When present and valid on `deleteEntities`/`deleteMessages`,
the id set is resolved once up front, then deleted in successive independent transactions of at
most N ids. A failed batch records its failure and later batches still run — mirroring the
existing per-id continue-on-error semantics of conditional delete. Batches committed before a
failure stay committed (deliberate, opt-in partial-commit contract). Absent param ⇒ existing
single-transaction (entities) / single `DeleteBatch` call (messages) behavior unchanged.
- `deleteEntities`: the condition is evaluated once in the first (resolution) transaction,
  **as-at `pointInTime`** when supplied; the guard baseline is each matched entity's **current
  version read in that same resolution transaction** (`EntityMeta.Version`; no SPI change).
  An id matched as-at PIT but already gone at resolution time is recorded in `idToError` (the
  unbatched path records the same per-id delete failure). Each batch transaction re-reads the
  entity and deletes only if the version equals the baseline; an id modified **after resolution**
  is skipped into `idToError`, not deleted — batching must not silently tombstone data written
  after resolution. That per-id guard is the batched analog of the single-tx path's commit-time
  conflict detection; the only semantic delta vs. unbatched is that unbatched fails the whole
  request on conflict (409) while batched skips the conflicted id and reports it. Response stays
  `StreamDeleteResult` (matched count, removed count, `idToError`; a failed batch commit maps all
  its ids into `idToError`). The empty-condition delete-all fast path batches the same way when
  the param is present (enumerate ids + current versions in the resolution tx, then batch — the
  existing fast path already materializes the full model for its count, so no new memory hazard);
  without it, the single-tx `DeleteAll` bulk call is kept.
- `deleteMessages`: one response element per batch in the existing array-of-batches shape
  (`[{entityIds, success}]`), which was designed for exactly this. Store failure on a batch ⇒
  that element `success: false`, later batches still attempted. Messages are immutable
  (Save/Get/Delete only), so no version guard applies. Without the param the current
  single-call/500-on-failure behavior is unchanged. Note: memory's `DeleteBatch` is a
  first-error-abort loop, so a `success:false` element may be partially applied there —
  documented with the (already documented) non-transactional nature of message delete.
- The two knobs never meet: the delete ops declare no timeout param, the timeout ops no batch
  param. No timeout×batching interaction exists.

**D5 — Search timeout is re-added and enforced uniformly.** `searchEntities` regains optional
`timeoutMillis` (int64, no default) + documented 408 (`SEARCH_TIMEOUT`, retryable), completing
#372's recorded intent. On tx-token'd (joined) requests the param is rejected per D7 — an in-tx
search runs on the owner's transaction, so a joiner's deadline would poison it exactly as for
writes. Enforcement = `WithTimeout` around the search + cancellation-awareness in the scan paths:
- domain in-memory fallback match loop (`internal/domain/search/service.go:405`) — near-dead on
  OSS backends (all three implement `spi.Searcher`), guarded anyway for uniformity,
- memory plugin: searcher match/merge loops, threading ctx through the currently ctx-less helpers
  (`matchSortBounded`, snapshot/`GetAllAsAt` scans) and checking before the O(n log n) sort,
- sqlite plugin: committed/streamed/tx-overlay row loops (row fetch is already cancellable via
  `database/sql`; explicit checks make interruption deterministic and cover the post-filter and
  sort stages),
- postgres: already cancellation-aware (pgx + iterator checks) — no change.
Checks are amortized (every N iterations) to keep scan cost flat. `spi.MergeBounded` stays
untouched — the plugin-local `next` closures gain the check (a local edit; they don't reference
ctx today). **No SPI change anywhere in this design.**

**D6 — gRPC honors what its contract already declares.** Same seam, same semantics, same codes:
- `EntityCreate/Update/Patch/CreateCollection/UpdateCollectionRequest.transactionTimeoutMs` →
  deadline around the same domain-service calls; expiry → error envelope `CLIENT_ERROR`,
  message prefixed `TRANSACTION_TIMEOUT: …`, `Retryable: true`.
- `EntitySearchRequest.timeoutMillis` → deadline around `DirectSearch`; expiry → search error
  envelope, `SEARCH_TIMEOUT: …` prefix. (Timeout expiry aborts before/mid collection; error
  envelope replaces further stream elements.)
- `EntityDeleteAllRequest.transactionSize` → honored **only when explicitly sent** (schema field
  becomes nullable, baked default removed — see settled constraints): enumerate ids + versions,
  batch per D4 with the same version guard. Absent ⇒ current single-tx `DeleteAllEntities`.
- Tx-token'd (joined) gRPC requests: any transaction-control param present → `CLIENT_ERROR`,
  `BAD_REQUEST: …` (D7's uniform rule, both search and write/delete events).
- `EntitySnapshotSearchRequest` declares no timeout field — explicitly out of scope (async search
  has its own ceiling machinery; postgres scan budget is #472).
- Messages have no gRPC surface; `EntityTransitionRequest` declares no timeout field — out of
  scope.

**D7 — Multi-node: enforcement is always node-local; forwarded ⇔ joined ⇔ 400.** On this
architecture, cross-node forwarding happens **only** for tx-token'd requests (no token ⇒ served
locally, both doors). A forwarded request therefore always joins an existing transaction on the
owner node — and **every transaction-control param is rejected with `400 BAD_REQUEST` on a
joined request** (uniform rule; on gRPC, `CLIENT_ERROR` / `BAD_REQUEST: …`):
- *Timeout params* (`transactionTimeoutMillis`, search `timeoutMillis`): attaching a joiner's
  deadline to a transaction it does not own would let one caller's timeout cancel SQL on the
  owner's shared connection and poison the whole transaction; a 408 there could be falsified when
  the owner later commits. Honoring is impossible → fail closed with a clear error rather than
  silently ignore (silent ignoring is the contract-bug class this issue exists to kill).
- *`transactionSize`*: a joined request cannot commit per batch (`commitOwned` no-ops when not
  owned; segmenting inside a joined tx is invariant-forbidden), so batching is unsatisfiable →
  same rejection. Applies to `deleteEntities`, `deleteMessages` (uniformity — one rule, no
  per-op carve-outs), and tx-token'd gRPC delete-all.
- Consequently the deadline is only ever attached on the node that directly receives a
  non-token'd request, which is also the node that executes it. No deadline ever needs to cross
  a hop; query-string/payload preservation across the proxy matters so the peer can *reject*
  correctly (the multinode e2e asserts the 400 crosses the hop).
- Internal engine/callback paths send no such params; only external clients writing inside a
  client-managed transaction are affected.
- In-flight compute callouts already select on `ctx.Done()`, so an expired deadline aborts them
  and the transaction rolls back.
- The timeout is a processing bound on the executing node, not an end-to-end SLA.

**D8 — Two new error codes.** `TRANSACTION_TIMEOUT` (408, retryable — entity writes +
`newMessage`) and `SEARCH_TIMEOUT` (408, retryable — search). Both get `errors/<CODE>.md` help
topics (`TestErrCode_Parity`) and `errors.md` index rows. Classification is ours-only per D2's
pinned rule; pre-existing deadline sources (postgres `statement_timeout` → 500 by design,
dispatch `time.After` → 503) keep their codes. Client-disconnect cancellation
(`context.Canceled`) is never a 408. The classifier runs at the handler seam, ahead of the
generic classifiers that would otherwise map a raw context error to 400 `WORKFLOW_FAILED` or a
ticketed 500.

**D9 — Cancellation checks are generic, and the resulting alignment is declared.** The new
`ctx.Err()` checks in domain loops (chunk loop, collection per-item loops, conditional-delete
per-id/per-batch loops, workflow cascade loop) and plugin scan loops fire on any context error,
not only the feature deadline. Today the backends diverge on client disconnect: postgres aborts
mid-operation (pgx), sqlite aborts at the commit flush, memory runs everything to completion.
Generic checks + the D2 commit shield align all backends: in-flight work stops at the next check
(fail closed, rollback), while a commit/flush already in progress completes (memory's behavior
becomes the contract; sqlite's disconnect-at-flush flips from abort to durable commit). Declared
in the changelog rather than shipped silently. Committed chunks stay durable. Post-CBD contexts
are uncancellable, so these checks are naturally inert there — consistent with D3.

**D10 — Validation.** Mirrors the `transactionWindow` precedent (parse → 400 → proceed):
- `transactionTimeoutMillis` / `timeoutMillis`: must be ≥ 1; values whose millisecond duration
  overflows `time.Duration` (> ~9.2e12 ms) rejected. Violations → `400 BAD_REQUEST`.
- `transactionSize`: must be ≥ 1 → else `400 BAD_REQUEST`. No upper bound: a value larger than
  the id set degenerates to the current single-batch behavior.
- Any transaction-control param on a joined (tx-token'd) request → `400 BAD_REQUEST` (D7).
- Non-numeric values → 400 via the existing generated-binding `BindingErrorHandler`.
- gRPC: same checks; violations → `CLIENT_ERROR` envelope with `BAD_REQUEST: …` prefix.

**D11 — Sibling fixes folded in (Gate 6, bounded).**
- `internal/grpc/dispatch.go` abandons the pending-request map entry on **three** paths — the
  `ctx.Done()` arm, the `time.After` arm, and the `Send`-failure path (removal today happens only
  via `CompleteRequest`/`FailAllPending`). All three are fixed with one deferred cleanup so a late
  compute-node reply finds no dangling entry. It is a bounded map-entry leak, not a goroutine
  leak; a reachable write deadline makes the `ctx.Done()` arm hot.
- sqlite `MessageStore.DeleteBatch` builds one `IN (?,…)` for the whole id list and breaks on
  SQLite's bound-variable limit (32766 in the wasm build the `ncruces/go-sqlite3` driver ships)
  for large lists. Fixed in-plugin by chunking the IN list at a size well under the limit
  (message delete is documented non-transactional, so statement chunking changes nothing
  observable).

## Mechanism (seams)

- **Param resolution:** per-handler, following `resolveTransactionWindow` — parse + validate the
  param, 400 on violation, before any I/O. Joined-request rejection happens here too
  (`spi.GetTransaction(ctx) != nil` ⇒ tx-token'd request; fed by the txjoin middleware / gRPC
  tx-route interceptor on both doors, never set on engine-internal or loopback paths at
  param-resolution time).
- **Deadline attachment + classification:** a small `internal/common` helper set —
  - attach: `WithTimeout` + a marker recording that *this feature* set the deadline,
  - classify: per D2's pinned rule (error-chain `errors.Is` + marker, with `AppError` cause
    unwrapping; never ctx-state alone),
  - `CommitContext`: `WithoutCancel` + budget, the D2 commit shield (sibling of
    `RollbackContext`), applied in `commitOwned`, `flushAndCommitSegment`, and around
    `newMessage`'s `Save`.
  Applied at: HTTP entity handlers (around the whole service call incl. all chunks), `newMessage`
  (pre-Save check + shielded Save), the search handler (around `searchSvc.Search`), and the gRPC
  handlers (budget from the event payload).
- **Batched delete:** restructure `DeleteEntitiesConditional` to (1) resolve ids + current
  versions (resolution tx, condition as-at `pointInTime`), (2) per batch of N: re-read versions,
  delete unchanged ids, commit (shielded), aggregate one `DeleteResult`; `deleteMessages` chunks
  the id list into per-batch `DeleteBatch` calls, one response element each. gRPC delete-all with
  explicit size: enumerate ids + versions, batch the same way.
- **Plan-level mechanics** (recorded so the plan carries them): new 408/400 responses declared in
  `api/openapi.yaml` **and** `internal/e2e/zzz_errorcode_matrix_test.go` `declaredGaps` updated;
  the callback-harness server stack is outside the conformance middleware, so the write-408 e2e
  asserts the problem+json shape explicitly; parity scenarios need the deliberately-absent
  `transactionSize` client helper added to `e2e/parity/client/http.go` and the
  `wantParityScenarioCount` bump; the blocking gRPC processor in the 408 e2e must release its
  gate before teardown to avoid the 5s drain.

## Error/status tables (per endpoint, delta only)

New/changed rows only; every listed 4xx is `application/problem+json` with `errorCode` in
`properties`. All other documented codes are unchanged.

| Op (HTTP) | New response | Trigger |
|---|---|---|
| `create`, `createCollection`, `updateSingle`, `updateSingleWithLoopback`, `updateCollection`, `patchSingle`, `patchSingleWithLoopback` | `408 TRANSACTION_TIMEOUT` (retryable) | supplied `transactionTimeoutMillis` expired before the first commit; nothing committed (D2/D3). After a first chunk/segment commit, expiry surfaces via the existing 200/per-chunk contract (error elements carry `TRANSACTION_TIMEOUT`), never 408 |
| same 7 ops | `400 BAD_REQUEST` | `transactionTimeoutMillis` ≤ 0, overflow, non-numeric, or sent on a joined (tx-token) request |
| `newMessage` | `408 TRANSACTION_TIMEOUT` (retryable) | deadline expired before the message save began (save itself is shielded; save-wins) |
| `newMessage` | `400 BAD_REQUEST` | invalid `transactionTimeoutMillis`, or sent with a tx token |
| `deleteEntities` | `400 BAD_REQUEST` | `transactionSize` ≤ 0, non-numeric, or sent on a joined (tx-token) request |
| `deleteEntities` | 200 semantics extended | with `transactionSize`: per-batch commits; failed batch's ids in `idToError`; version-changed/vanished ids skipped into `idToError`; committed batches stay committed |
| `deleteMessages` | `400 BAD_REQUEST` | `transactionSize` ≤ 0, non-numeric, or sent with a tx token |
| `deleteMessages` | 200 semantics extended | with `transactionSize`: one array element per batch; failed batch ⇒ `success:false`, later batches still attempted |
| `searchEntities` | param `timeoutMillis` re-added; `408 SEARCH_TIMEOUT` (retryable) | supplied `timeoutMillis` expired before the result set was collected |
| `searchEntities` | `400 BAD_REQUEST` | invalid `timeoutMillis`, or sent on a joined (tx-token) request |

| Op (gRPC event) | Error class | Trigger |
|---|---|---|
| `EntityCreate/Update/Patch/CreateCollection/UpdateCollectionRequest` | `CLIENT_ERROR`, msg `TRANSACTION_TIMEOUT: …`, `Retryable: true` | `transactionTimeoutMs` expired pre-commit |
| same events | `CLIENT_ERROR`, msg `BAD_REQUEST: …` | `transactionTimeoutMs` ≤ 0 / overflow / sent on a tx-token'd request |
| `EntitySearchRequest` | search error envelope, msg `SEARCH_TIMEOUT: …`, `Retryable: true` | `timeoutMillis` expired |
| `EntitySearchRequest` | `CLIENT_ERROR`, msg `BAD_REQUEST: …` | invalid `timeoutMillis`, or tx-token'd request |
| `EntityDeleteAllRequest` | `CLIENT_ERROR`, msg `BAD_REQUEST: …` | explicit `transactionSize` ≤ 0, or tx-token'd request |
| `EntityDeleteAllRequest` | 200-envelope semantics extended | explicit `transactionSize` ⇒ batched per-tx version-guarded deletes, committed batches durable |

## Coverage matrix

Layers: **U** = unit/service-level (fake or real plugin store in-process, deterministic),
**E** = running-backend e2e (`internal/e2e`, real Postgres through full HTTP stack),
**P** = cross-backend parity (`e2e/parity`, memory/sqlite/postgres + commercial),
**G** = gRPC (`internal/grpc` tests, envelope asserted).

| Scenario | U | E | P | G |
|---|---|---|---|---|
| Write 408: deadline expires mid-op, rollback, nothing committed — **all 8 entity ops** | ✔ blocking fake store | ✔ per op, withheld compute-callout reply (callback harness; verified feasible, no harness change) | — (timing) | ✔ |
| Write 408 honored on memory backend (regression: no cancellable syscall) | ✔ pre-expired ctx + memory plugin | — | — | — |
| Commit shield: expiry at the commit boundary ⇒ pre-commit check → 408 + rollback; commit itself uncancellable | ✔ | — | — | — |
| Commit-wins: deadline after commit ⇒ success reported | ✔ | — | — | ✔ |
| CBD workflow: timeout inert after first segment commit | ✔ engine-level | — | — | — |
| Chunked create: expiry after chunk 1 committed ⇒ 200 + per-chunk `TRANSACTION_TIMEOUT` elements, never 408 | ✔ | ✔ | — | — |
| Joined (tx-token) request with any transaction-control param ⇒ 400 / `CLIENT_ERROR BAD_REQUEST` — write, search, delete | — | ✔ (incl. forwarded-hop multinode scenario: 400 crosses the hop) | — | ✔ |
| 400 invalid `transactionTimeoutMillis` (≤0, non-numeric, overflow) — every declaring op | — | ✔ per op | — | ✔ |
| `newMessage` 408 + 400; save-wins | ✔ | ✔ 400; 408 via U (waiver: no blocking seam through HTTP) | — | n/a |
| Batched `deleteEntities`: partial failure — earlier batches committed, failed batch in `idToError`, later batches run | ✔ fault-injected store | ✔ happy-path batching (counts, >1 batch) | ✔ final-state consistency | — |
| Batched `deleteEntities`: version-guard — id modified after resolution skipped into `idToError`, not deleted; PIT condition with current-version baseline | ✔ | waiver (below) | — | — |
| Batched `deleteMessages`: per-batch elements, `success:false` on failed batch, later batches attempted | ✔ | ✔ happy path | ✔ | n/a |
| `deleteEntities`/`deleteMessages` 400 on invalid `transactionSize` | — | ✔ | — | ✔ (delete-all) |
| Absent params ⇒ behavior unchanged (single tx / single call) | ✔ | ✔ | existing suites | ✔ |
| Search 408: deadline honored on memory + sqlite scan loops | ✔ pre-expired ctx per plugin + domain fallback loop | 408 via U (waiver: no deterministic blocking seam through HTTP search); ✔ 400 + happy-path-with-param | — (timing) | ✔ blocking store |
| gRPC delete-all: explicit `transactionSize` batches (version-guarded); absent ⇒ single tx | — | — | — | ✔ |
| Proxy preserves the query string across the forwarded hop | ✔ proxy unit test | — | — | — |
| Dispatch pending-entry cleared on all three abandonment paths | ✔ | — | — | ✔ |
| sqlite `DeleteBatch` IN-list chunking (large id list) | ✔ plugin test | — | — | — |

Waivers (one-line, per `.claude/rules/test-coverage.md`): e2e 408 for `newMessage` and
`searchEntities` — no deterministic blocking seam exists through the full HTTP stack on those
paths; deterministic coverage lives at U with real plugin stores. Concurrency scenarios: none
added — batching is sequential by design; existing conflict semantics (409 on commit conflict)
unchanged. Version-guard e2e: the guard window (a mutation landing between
resolution and a batch) is not reachable deterministically through the full HTTP
stack — covered at U with a controlled interleave hook.

## Documentation & parity obligations (Gate 4 / Gate 7)

- `api/openapi.yaml`: drop `default:` on all 10 param declarations; re-add `timeoutMillis` to
  `searchEntities`; add 408 responses + validation and joined-request wording; align the three
  drifted `transactionTimeoutMillis` description variants to one wording naming 408; fix the
  `deleteMessages` summary that already claims batching exists.
- `docs/cyoda/schema/entity/EntityDeleteAllRequest.json`: `transactionSize` nullable, no default;
  regenerate `api/grpc/events/types.go`.
- Help topics: `crud.md`, `messages.md`, `search.md` — remove "no behavioural effect" wording,
  document semantics incl. partial-commit batching, version guard, D3, joined-request rejection,
  and memory's partially-applied `success:false` caveat; new `errors/TRANSACTION_TIMEOUT.md`,
  `errors/SEARCH_TIMEOUT.md`; `errors.md` index rows.
- `docs/cloud-parity/`: one file recording the contract deltas (defaults removed, 408s added,
  search param re-added, joined-request rejection, `EntityDeleteAllRequest` schema change — noting
  `TransactionSize` becomes `*int`, a compile-breaking change for Go importers of
  `api/grpc/events`, acceptable pre-1.0).
- `CHANGELOG.md`: Fixed/Added entries; declares the two no-param-visible alignments (D2 commit
  shield: disconnect-at-flush now completes on all backends — was abort on postgres/sqlite,
  complete on memory; D9: disconnect now aborts in-flight loop work on memory/sqlite as it
  already did on postgres); **no `### Breaking`** — every param-driven behavior change is opt-in.
- `COMPATIBILITY.md`: untouched (no SPI change, no release/chart/pin change in this PR).

## Out of scope

- `waitForConsistencyAfter` (third inert param family) — same class, different feature
  (consistency wait, not timeout); needs its own design. Filed as a follow-up issue at PR time —
  not silently dropped.
- gRPC `EntityDeleteAllRequest`'s other inert fields (`pageSize`, `pointInTime`, `verbose`,
  hardcoded empty `entityIds` in the response) — folded into the same follow-up issue.
- Async search (`submitAsyncSearchJob`) and `EntitySnapshotSearchRequest` ceilings — #472.
- Grouped-stats / count endpoints (no params declared there).
- `PerShardTimeout` / `AllowUnbounded` dead fields in domain `SearchOptions` — reserved for the
  commercial backend; not wired here (feeding an HTTP param into them is a commercial-backend
  design question).
