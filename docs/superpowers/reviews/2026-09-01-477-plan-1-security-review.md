# Security review — #477 plugin internals (`134bcaa..HEAD`)

Scope reviewed: `git diff 134bcaa..HEAD -- plugins/ cmd/cyoda/help/content/crud.md CHANGELOG.md internal/`.
Gates applied: `antigravity-bundle-security-developer:cc-skill-security-review`,
`.claude/rules/security.md`, CLAUDE.md Gate 3, `.claude/rules/correctness-over-availability.md`.

Read-only review. No files in the worktree other than this report were touched;
no tests were run.

## Severity counts

| Severity | Count |
|---|---|
| Critical | 0 |
| High | 0 |
| Medium | 4 |
| Low | 5 |
| Informational | 6 |

---

## Medium

### M1 — `Begin` waits on an uncancellable mutex; a slow write stalls every new transaction, process-wide

**Location:** `plugins/sqlite/txmanager.go:251-259` (new `m.commitMu.Lock()` inside `Begin`),
with `plugins/sqlite/entity_store.go:302-303` (`saveDirectly`) and
`plugins/sqlite/entity_store.go:699-700` (non-tx `Delete`) as the new holders.

**Risk.** `Begin(ctx)` now blocks on `sync.Mutex.Lock()`, which ignores `ctx`
entirely. The gate is held for the whole of a transaction `Commit`'s flush and
for the whole of every direct write's SQL round-trip. Both of those in turn
contend for `db`, which is a *one-connection* pool
(`store_factory.go:101 SetMaxOpenConns(1)`) shared with unbounded reads
(`searchCommitted`, `getAllDirect`, `getPageAsAt`, `GetAllAsAt` all run on
`s.db`). So a single long scan on the writer connection can park a direct write
inside `commitMu`, and every subsequent `Begin` — from every tenant — blocks
behind it with no way for the caller's deadline to release it. Requests time out
at the transport while their goroutines stay pinned, so goroutines and their
retained request state accumulate under sustained load: a cross-tenant
availability and memory-exhaustion surface that one tenant's traffic can drive.
Before this change `Begin` never waited on a write.

Note the design intent is sound and matches `correctness-over-availability.md`
(the snapshot floor must imply visibility). The defect is the *uncancellable*
acquisition, not the ordering.

**Fix.** Make the wait context-aware: replace `commitMu sync.Mutex` with a
`chan struct{}`-of-capacity-1 semaphore and acquire it in `Begin` with
`select { case gate <- struct{}{}: ... case <-ctx.Done(): return "", ctx, ctx.Err() }`,
so a client deadline aborts the wait and returns the ctx error. (`Commit` and
the direct writes can keep an unconditional acquire.) Separately, consider
moving long committed-only reads (`searchCommitted`, `getAllDirect`,
`getPageAsAt`, `GetAllAsAt`) off `s.db` onto `readDB` so they cannot park a
direct write inside the gate at all.

### M2 — An in-transaction iterator pins one of 4–8 `readDB` connections for its whole lifetime

**Location:** `plugins/sqlite/tx_overlay.go:125` (`s.readDB.QueryContext`, cursor
kept open in `txOverlay.rows`), `plugins/sqlite/tx_overlay.go:235-267`
(`iterateTx` returns the live cursor to the caller);
pool sized 4–8 at `plugins/sqlite/store_factory.go:194-227`, applied at
`store_factory.go:164`.

**Risk.** This change routes six more operations onto `readDB` — in-tx
`Iterate`, `GetPage`, `Count`, `CountByState`, `GetAll` and `DeleteAll`'s id
scan — and the `Iterate` case holds its connection open until the consumer calls
`Close()`. Two consequences:

1. **Pool exhaustion.** Enough concurrent long-lived in-tx iterators (the pool
   floor is 4, ceiling 8, and 8 is also the default async-search worker count)
   leaves no reader connection for anything else. Other tenants' reads queue.
2. **Self-deadlock on nesting.** Any consumer that issues a second `readDB` read
   *inside* an iteration loop waits on a connection only the outer loop can
   release. The comment added at `store_factory.go:203-206` states this
   requirement, but nothing enforces or tests it, and the in-tx `Iterate`,
   `Count`, `CountByState`, `GetPage` and `GetAll` combination that would trip it
   is exactly what this change made reachable inside one transaction. If the
   inner `ctx` carries no deadline the hang is permanent.

An iterator that is never `Close()`d (an early `return` on a scan error that
skips the close, a panic path without a `defer`) leaks the connection for the
process lifetime. Current in-tree consumers do close correctly
(`internal/domain/entity/service.go:1643-1662`,
`internal/domain/search/service.go:1003-1011` and `:1381-1408`), so this is
latent rather than live.

**Fix.** Turn the "no nested `readDB` read inside an iteration" rule from a
comment into an enforced invariant — e.g. a per-transaction reader-slot counter
that returns an error (rather than blocking) when an in-tx read is issued while
that transaction already holds an open overlay cursor. At minimum add a
regression test that opens `defaultReaderPoolSize()+1` concurrent in-tx
iterators and asserts the extra one fails with the ctx error rather than hanging,
and require every in-tx read path to run under a deadline-bearing ctx.

### M3 — The `tx.Closed` guard was added to the read paths but not to the write paths; a write to a committed transaction returns success and is discarded

**Location:** `plugins/sqlite/entity_store.go:243-250` (`Save`), `:386-392`
(`CompareAndSave`); `plugins/memory/entity_store.go:168-180` (`Save`), `:212-220`
(`CompareAndSave`). Compare with the guards this change *did* add:
`plugins/sqlite/entity_store.go:575-577, 769-771, 900-902, 990-992, 1052-1054`
and `plugins/memory/entity_store.go:478-480, 691-693, 857-859, 909-911, 1052-1054`.

**Risk.** After this change every in-transaction *read* entry point refuses a
committed transaction, while `Save`, `CompareAndSave`, `Delete` and `Exists` still
check only `tx.RolledBack`. A write issued against an already-committed
transaction writes into `tx.Buffer` and returns `(0, nil)` — reported to the
caller as success — and the value is then silently dropped, because the flush
already ran. That is a fail-open on an integrity path: the caller believes the
write landed, and there is no error, log line or metric to contradict it. It is
also now internally inconsistent, since the same transaction state produces a
clean error from `Count` and a silent success from `Save`.

**Fix.** Add the same guard to every in-transaction write and existence entry
point in both plugins:

```go
if tx.Closed {
    return 0, fmt.Errorf("Save: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
}
```

Cover `Save`, `CompareAndSave`, `Delete` and `Exists` (and `Get`), and add a
parity scenario asserting `ErrTxAlreadyCommitted` from a write on a committed tx
on every backend.

### M4 — Per-yield read-set writes and the full-buffer walk widen an already-fatal concurrent-map window on a joinable transaction

**Location:** `plugins/sqlite/tx_overlay.go:316-333` (`checkAndRecord`, writes
`it.tx.ReadSet[id]` under `OpMu.RLock` on **every** yield),
`plugins/sqlite/tx_overlay.go:90-120` (walks the whole `tx.Buffer` map under
`RLock` at overlay open), `plugins/memory/grouped_stats.go` `memoryIter.checkAndRecord`
(same shape). Contract: `cyoda-go-spi@…/txcontext.go` "Within-class
serialisation — application's responsibility, NOT enforced by OpMu … will trigger
the runtime's *concurrent map writes* fatal".

**Risk.** `OpMu.RLock` does not exclude `RLock` holders from each other, so two
concurrent operations on one transaction produce a Go runtime *fatal* — not a
panic, so `recover()` cannot catch it and the whole process dies, taking every
tenant with it. Concurrent operations on one transaction are remotely reachable:
`internal/domain/txjoin/txjoin.go:36-66` joins any request carrying a valid
transaction routing token, and neither `txjoin` nor
`internal/domain/entity/txscope.go` serialises operations on a joined
transaction — so two in-flight callbacks bearing the same token race.

This is a pre-existing gap at the application layer (the SPI documents the
contract and the plugins comply). What this change does is widen the window
substantially: read-set recording moved from one short critical section at open
(memory's old `buildSnapshot`) to a write on every yield across an arbitrarily
long iteration, and `openTxOverlay` now walks the entire `tx.Buffer` map while a
joined `Save` may be writing it.

**Fix.** Serialise per-transaction operations at the application layer — a
per-`txID` mutex acquired in `txjoin.JoinFromToken` / the tx scope, released when
the request's use of the transaction ends — so a second concurrent request on the
same tx either waits or is rejected with a 409 rather than crashing the process.
Add an isolated (non-parity, per `.claude/rules/tdd.md` and the concurrency-test
rule) e2e that fires two concurrent joined requests on one token and asserts a
clean outcome.

---

## Low

### L1 — `projectIDState` evaluates a residual post-filter against an entity with no payload

**Location:** `plugins/sqlite/tx_overlay.go:141-151` — the residual check
`plan.preparedPostFilter != nil && !evaluateFilter(*plan.preparedPostFilter, e)`
runs for both projections, but `scanIDState` (`tx_overlay.go:224-231`) returns an
entity with `Data == nil`.

**Risk.** With a non-zero filter on `projectIDState` the residual would silently
evaluate against empty data, producing a wrong count rather than an error — a
silent-wrong-answer failure mode of the kind
`correctness-over-availability.md` forbids. Not currently reachable: all three
`projectIDState` callers (`countTx` → `Count`, `CountByState`) pass
`spi.Filter{}`. The precondition is enforced only by the doc comment at
`tx_overlay.go:58-60`.

**Fix.** Make it structural, not documentary — in `openTxOverlay`, immediately
after `planFor`:

```go
if proj == projectIDState && (plan.where != "" || plan.preparedPostFilter != nil) {
    return nil, fmt.Errorf("tx overlay: id/state projection cannot evaluate a filter")
}
```

### L2 — `snapshotIDStateBase`'s outer row is not constrained to the requested model

**Location:** `plugins/sqlite/tx_overlay.go:208-220`. The `latest` subquery is
scoped by `tenant_id`, `model_name`, `model_version` and `submit_time`; the outer
`WHERE` re-applies only `ev.tenant_id` and `change_type != 'DELETED'`, joining on
`entity_id` + `version` alone.

**Risk.** **No cross-tenant exposure** — both sides bind the same
`s.tenantID`. But a same-tenant row of a *different* model with a colliding
`(entity_id, version)` would be counted here, and `scanIDState` labels the result
with the caller's requested `modelRef` rather than the row's own, so the
mislabelling is invisible. This mirrors the pre-existing `searchSnapshotBase`
(`plugins/sqlite/searcher.go:186-199`), so the new query inherits rather than
introduces the shape.

**Fix.** Add `AND ev.model_name = ? AND ev.model_version = ?` to the outer
`WHERE` in both `snapshotIDStateBase` and `searchSnapshotBase`, binding the same
two args.

### L3 — Non-transactional `CompareAndSave` reads outside the commit gate and writes inside it

**Location:** `plugins/sqlite/entity_store.go:431-445` — the `transaction_id`
comparison runs on `s.db` before `saveDirectly` (`:293-303`) takes `commitMu`.

**Risk.** The check-then-act window is unchanged by this diff, but the new gate
makes the write look atomic with the check when it is not: two concurrent
`CompareAndSave` calls on the same entity can both read the same `transaction_id`
and both proceed, so a lost update is still possible on the non-tx door. Same
window exists in `plugins/memory/entity_store.go`'s non-tx branch.

**Fix.** Move the CAS read inside the `commitMu`-held region (and, on sqlite,
inside the same `sqlTx` as the write) so read and write are one atomic unit.

### L4 — `countTx`'s buffer loop has no cancellation point (memory)

**Location:** `plugins/memory/entity_store.go:840-845`.

**Risk.** The committed walk at `:822-827` carries the standard amortized
`i&1023 == 0 → ctx.Err()` check, but the `tx.Buffer` loop that follows has none.
A transaction with a very large buffer is walked to completion after the client's
deadline has fired — inconsistent with the spec D5 posture every sibling loop in
both plugins follows (`tx_overlay.go:96-104`, `searcher.go:145-153`).

**Fix.** Add the same amortized check to the buffer loop.

### L5 — `unstageDelete` is correct but the invariant it restores is not asserted

**Location:** `plugins/sqlite/entity_store.go:135-142` and
`plugins/memory/entity_store.go:50-55`.

**Risk.** The helper fixes a real integrity bug (a stale
`DeleteAttribution` entry surviving a `Save`-after-`Delete`, so a resurrected
entity could carry another principal's delete attribution into an audit record).
The `Deletes` ⊇ `DeleteAttribution` key-set invariant it restores is stated only
in a comment; `DeleteAll` and `Delete` are the other two writers and nothing
checks them.

**Fix.** Add a cheap invariant assertion at commit time (both key sets equal, or
`DeleteAttribution` ⊆ `Deletes`) that fails the commit rather than writing a
mis-attributed audit row.

---

## Informational

### I1 — Tenant isolation: verified clean on every new query and every new walk

Every SQL statement added by this diff binds `tenant_id` on **both** the inner
`latest` subquery and the outer row:

- `plugins/sqlite/tx_overlay.go:214` and `:217` (`snapshotIDStateBase`) —
  args at `:218` bind `string(s.tenantID)` twice.
- `plugins/sqlite/entity_store.go:781` and `:784` (`DeleteAll`'s new id-only
  scan) — args at `:785-786`.
- The overlay's full projection reuses `searchSnapshotBase`
  (`plugins/sqlite/searcher.go:193`, `:196`), unchanged and tenant-bound.

Every memory walk this diff touches is rooted at the tenant-partitioned map:
`getAllSnapshotPointersUnlocked` (`plugins/memory/grouped_stats.go:233`),
`currentStatePointersUnlocked` (`plugins/memory/entity_store.go:961`),
`getAllSnapshotUnlocked` (`:134`) all iterate
`s.factory.entityData[s.tenant]`. `countTx` and `DeleteAll` consume those
helpers rather than the map directly. No new cross-tenant path.

### I2 — SQL injection: no user-controlled text reaches SQL unparameterised

- `plan.where` and `plan.args` come from `plugins/sqlite/query_planner.go`, which
  this diff does not modify. All operands bind via `?`
  (`query_planner.go:563-672`).
- The only string interpolated into SQL is `Filter.Path` inside `fieldExpr`
  (`query_planner.go:507`, `:509`). Its grammar guard is
  `validateFilterPaths` → `validateJSONPath` → `spi.ValidateFilterPath`
  (`plugins/sqlite/path_validation.go:39-74`), which rejects `'`, `"`, `\`, `;`,
  `/`, whitespace and control bytes.
- The new call path is gated: `Iterate` runs `validateFilterPaths(filter)` at
  `plugins/sqlite/grouped_stats.go:81-83` **before** dispatching to `iterateTx`
  at `:97`. The other three `openTxOverlay` callers — `GetAll`
  (`entity_store.go:578`), `getPageTx` (`:1194`), `countTx`
  (`tx_overlay.go:178`) — pass a zero-value `spi.Filter{}`, so `plan.where` is
  empty.
- `ORDER BY ev.entity_id` (`tx_overlay.go:83`) is a compile-time literal; no
  `OrderBy` reaches the overlay (in-tx + non-empty `OrderBy` is rejected at
  `grouped_stats.go:88-90`).

### I3 — Secrets and logging: nothing added

The diff adds no `slog`/`log`/`fmt.Print` call in either plugin and touches no
credential, token, key or connection string. `internal/`'s only change is
`internal/domain/workflow/engine_test.go` (test-only).

### I4 — Error text: no internals leaked

The new `tx overlay: ` prefixes (`tx_overlay.go:102, 127, 135, 147, 159, 161`)
wrap only driver errors, scan errors and `ctx.Err()` — never SQL text, file
paths, or connection details. `(txID=%s)` exposes a transaction UUID the caller
already holds (it is what `Begin` returns and what the routing token carries).
Whether any of this reaches an HTTP client is decided upstream by
`common.Internal` (generic message + ticket UUID) and, for the classified
sentinels, `search.ClassifyStoreQueryError` — neither changed here. The
`CompareAndSave %s: %w` wraps echo a caller-supplied entity ID back to that same
caller: no disclosure.

### I5 — Neither plugin cross-checks `tx.TenantID` against the store's tenant

`openTxOverlay` reads `tx.Buffer` filtered only by `ModelRef`
(`tx_overlay.go:96-120`); memory's `countTx` and `buildSnapshot` do the same.
Isolation therefore rests entirely on two upstream invariants: the store handle
is resolved per tenant, and `transactionManager.Join`
(`plugins/sqlite/txmanager.go:283-288`) rejects a tenant mismatch. This is
pre-existing and holds today, but the new overlay inherits the reliance.
Defense in depth: assert `tx.TenantID == s.tenantID` at each tx-path entry point
and fail closed on mismatch.

### I6 — memory `Search`'s pointer-snapshot change does not leak store pointers

The switch from copying helpers (`getAllSnapshotUnlocked`,
`currentStateMatchesUnlocked`) to pointer helpers
(`plugins/memory/searcher.go:74, 100, 118`) was checked on every return path:

- non-tx and in-tx-PIT branches return through `matchSortBounded`, which copies
  on match (`searcher.go:246`).
- the RYW branch copies buffered adds at `:159` and the committed survivors after
  the merge at `:199-205`.

No raw `entityVersion.entity` pointer escapes `entityMu` on the `Search` path.
`Iterate` and `GetPage` do yield store pointers, but that is unchanged by this
diff and rests on the documented entityVersion-immutability invariant
(`plugins/memory/grouped_stats.go:221-232`).
