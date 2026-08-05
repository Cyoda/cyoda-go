# Transaction lifecycle safety — design

Issue: #471 (supersedes #466, #354, #362). Milestone v0.8.4.

A transaction that cannot complete correctly must be rolled back and its
connection returned on **every** exit path, including a panic, under a ceiling
the application cannot dodge. Today none of that holds and there is no backstop
underneath it. Four independent pieces of work, one slice because two of them
would otherwise be written twice.

---

## 1. Deferred rollback (all backends)

### 1.1 The defect

`internal/domain/entity/service.go` has 40 `rollbackOwned` call sites, all plain
calls on error branches, zero deferred. A panic between `Begin` and `Commit`
unwinds straight past them: the transaction is neither committed nor rolled
back, and its pooled connection never returns.

The workflow engine has the same defect on the segments it opens itself.
`executeCommitBeforeDispatch` opens TX_post (`engine_processors.go:333`, `:409`)
and rolls it back only through plain calls (`:292`, `:341`, `:349`, `:353`);
`rollbackOpenSegmentOnFailure` (`:150`) covers the error path out of
`executeProcessors` and nothing covers a panic. The handler cannot cover this:
once the engine has segmented, `flushAndCommitSegment` has already committed the
entry transaction, so a handler-side guard naming the entry txID is a guaranteed
`ErrTxNotFound` no-op while TX_post leaks.

`engine_processors.go:146` already asserts that "the caller-side handler tracks
the original entryTxID and will roll that back via its own deferred-rollback
path". No such path exists. This spec creates it and makes the comment true.

`internal/domain/workflow/fire_scheduled.go:112-135` is the one place that got it
right — a `committed` flag, a deferred rollback, and `curCtx/curTxID` advanced on
segmentation. That is the discipline being extracted.

### 1.2 `txScope`

A value type in `internal/domain/entity`:

```go
scope, err := h.beginScope(ctx)   // beginOrJoin only — does NOT touch the gate
defer scope.Release()
...
scope.Advance(result.FinalCtx, result.FinalTxID)   // FIRST statement after every engine call
...
err := scope.Commit()
```

All 40 explicit `rollbackOwned` calls are **deleted**, not duplicated.

Required properties:

- **`beginScope` does not touch the joined gate.** The seven existing
  `defer releaseGate()` calls (`service.go:269`, `:652`, `:792`, `:916`, `:1179`,
  `:1719`) stay exactly as they are. Folding the gate into the scope while
  `Release` is a rollback no-op for joined calls would leave the gate permanently
  held — it is a non-reentrant mutex (`internal/txgate/txgate.go:43`), so every
  later joined callback on that txID would block forever.
- **Joined callbacks never roll back their owner's transaction.** `Release` is a
  no-op when `owned == false`, matching `rollbackOwned` (`handler.go:107`).
- **Except a segment the engine opened, which is nobody else's.** When a joined
  call detects that the engine unexpectedly segmented (`service.go:330`, `:1262`,
  `:1510`, `:1853`) it returns `common.Internal` today with no rollback, so TX_post
  leaks. That segment is not the owner's transaction — it was opened by the engine
  during this call — so `Release` rolls it back regardless of `owned` whenever the
  scope has advanced past its entry txID. It is a can't-happen branch; fail-closed
  says handle it anyway.
- **`Release` targets the segment actually open.** `Advance` must be the first
  statement after every engine call — `service.go:325`, `:1256`, `:1506`, `:1849`
  currently interleave `StopReason`/`Segmented` handling first. Nothing between
  the call and those lines can return an error, so the exposure is a panic in that
  window; the ordering costs nothing and removes the case. While moving it, delete
  the dead `result != nil` guards at `:321`, `:1248`, `:1256` — the engine never
  returns `(nil, nil)`, and `:325` already dereferences unguarded (Gate 6).
- **`Release` acquires the per-tx gate before rolling back.** Nine of the 40
  rollbacks run inside `h.gate.Acquire(...)` today (`service.go:356`, `:680`,
  `:833`, `:1283`, `:1548`, `:1556`, `:1567`, `:1872`, `:1911`); the other 31 are
  already ungated, so for those `Release` is a strengthening. For the nine, the
  safety property that must survive is mutual exclusion on the `pgx.Tx`, and
  re-acquiring preserves it. What does *not* survive is failed-Save→rollback as one
  atomic gated section: a joined callback can win the gate in the window between
  the IIFE releasing it and `Release` re-acquiring, Save successfully, return 200
  to its caller, and then have its write discarded by the rollback. That is
  strictly better than today's alternative — a leaked transaction — and the joined
  caller's write was doomed either way once the owner failed, but it is a real
  change and is stated rather than glossed. No self-deadlock: every
  `defer h.gate.Acquire(...)()` site is inside an IIFE, so the gate is free by
  outer-defer time.
- **`Commit()` marks the scope done regardless of outcome.** This preserves
  today's behaviour exactly — no path rolls back after a failed commit
  (`service.go:980` documents why) — and avoids aborting a commit another
  goroutine is running on memory's `ErrTxCommitInProgress` path
  (`plugins/memory/txmanager.go:222`).
- **Rollback runs on a fresh, bounded context**:
  `context.WithTimeout(context.WithoutCancel(ctx), 5s)`. `WithoutCancel` keeps
  the `UserContext` that `verifyTenant` reads
  (`plugins/postgres/transaction_manager.go:486`) while dropping cancellation, so a
  timed-out request still returns its connection; the timeout stops a wedged
  `Rollback` from blocking the unwinding goroutine forever. Applied
  unconditionally — `FinalCtx` is `WithoutCancel`-wrapped only in the
  `startNewTxOnDispatch=false` case (`engine_result.go:25`).

  The bound is real only on postgres. memory and sqlite take `tx.OpMu.Lock()` in
  `Rollback` (`plugins/memory/txmanager.go:513`, `plugins/sqlite/txmanager.go:567`)
  and `txgate.Registry.Acquire` (`txgate.go:30`) is a plain mutex — neither takes a
  context, so on those backends the unwinding goroutine can still block on an
  in-flight operation. Accepted: both waits are on operations that themselves
  terminate, and giving `OpMu`/the gate context-aware acquisition is a wider change
  to two plugins' concurrency model than this issue should carry. Stated here so it
  is a known bound, not an assumed one.

Flows converted: `CreateEntity`, `DeleteEntity`, `DeleteAllEntities`,
`DeleteEntitiesConditional`, `CreateEntityCollection`, `updateEntityCore` (and
`UpdateEntity` / `PatchEntity` through it), `UpdateEntityCollection`.

`internal/domain/model` opens no transaction of its own, and the message paths do
not either — `beginOrJoin` is the entity service's alone. That answers the
question the issue left open: they do not share the shape and need no change.

### 1.3 Engine-side guard

The handler-side scope cannot cover the engine's segments, and not only because of
panics: **`Execute`, `ManualTransition` and `Loopback` return a nil `EngineResult`
on every error** (`engine.go:273`, `:282`, `:290`, `:354`, `:359`, `:364`, `:370`,
`:424`, `:451`, `:457`). There is nothing for the handler to `Advance` from, so on
any engine error the scope still names the cascade-entry transaction — already
committed once a COMMIT_BEFORE_DISPATCH segment flushed — while TX_post is dropped.

`rollbackOpenSegmentOnFailure` (`engine_processors.go:150`) only sees failures
raised *inside* `executeProcessors`. These all occur outside it, after
`currentTxID` has advanced:

- `engine.go:754` — `maxStateVisits` abort in `cascadeAutomated`
- `engine.go:775` — criterion evaluation error, which includes a FUNCTION-criterion
  compute-node dispatch failure: an ordinary, expected occurrence
- `engine.go:834` — `maxCascadeDepth` exceeded
- `engine.go:272` — `attemptTransition` error (`currentTxID` is assigned at `:271`,
  then discarded by `return nil, err`)
- `engine.go:290`, `:370`, `:457` — `reconcileScheduledTasks` failure
- `engine.go:685`, `:714` — `fireTransition`'s own error returns

So the leak this issue is about is reachable **without any panic at all**, by a
compute node failing a criterion callout mid-cascade. On memory and sqlite there is
no DB-side ceiling underneath, so it is permanent rather than 5-minute-bounded.

`fire_scheduled.go:405-412` already documents exactly this and fixes it for the
scheduler door — "may fail entirely outside executeProcessors … Advance
curCtx/curTxID before checking err so the deferred rollback always targets the
segment actually open". The three public entry points never got the same
treatment.

**Fix:** one panic-safe guard at each of `Execute`, `ManualTransition` and
`Loopback`, over the segment that entry point owns:

```go
entryTxID := txID
openCtx, openTxID := ctx, txID   // dedicated locals, NOT the named returns
handedOff := false
defer func() {
    if !handedOff && openTxID != entryTxID {
        _ = e.txMgr.Rollback(rollbackCtx(openCtx), openTxID)
    }
}()
```

`openCtx/openTxID` are advanced wherever the engine segments, and `handedOff` is
set only where the segment is returned to the caller. The guard therefore covers
the error paths above **and** panics, in one mechanism.

The locals are load-bearing: every failure path in `executeCommitBeforeDispatch`
is `return nil, "", err` (`engine_processors.go:286`, `:293`, `:313`, `:326`,
`:335`, `:342`, `:350`, `:354`), so a guard reading the named `newTxID` return
would see `""` on exactly the paths that need it and skip the rollback — turning
four working rollbacks into four leaks.

This **subsumes** `rollbackOpenSegmentOnFailure` and the four plain rollbacks at
`engine_processors.go:292`, `:341`, `:349`, `:353`, which are all removed. Net
result is fewer moving parts than today, not more, and it makes
`engine_processors.go:146`'s existing claim about a caller-side deferred-rollback
path true for the first time.

`executeCommitBeforeDispatch` keeps its own guard as well, since it opens TX_post
(`:333`, `:409`) and can panic before returning it to `executeProcessors`.

### 1.4 Panic recovery on the doors that lack it

Two entry points run transactional work with no recovery at all, so a panic kills
the process rather than the request.

**gRPC.** `internal/grpc/server.go:69` installs auth and tx-route interceptors
only, and grpc-go does not recover handler panics. Several operations are
genuinely gRPC-only — `internal/grpc/model.go:85`, `:119`, `:156`, `:186`, `:202`,
`:240` — so this is not merely a second door onto HTTP-reachable code. Add a
recovery interceptor, unary and stream.

**Peer scheduler RPC.** When `cfg.Cluster.Enabled`,
`cluster.NewSchedulerRPCHandler(...).Register(...)` is registered directly on the
outer mux (`app/app.go:747`, `:760`), outside `middleware.Recovery`, which wraps
only the `/` catch-all (`:729`). A more specific ServeMux pattern wins, so those
routes bypass recovery entirely — and the handler opens a transaction and runs a
full fire plus cascade including compute-node callouts
(`internal/cluster/scheduler_rpc.go:273` → `fire_scheduled.go:112`). A panic in a
peer-dispatched scheduled fire therefore takes down a node, on the primary
operating target, through a route any peer can reach.
`clusterdispatch.NewDispatchHandler` (`:746`, `:759`) sits in the same position
without a transaction. Wrap both in `middleware.Recovery`.

Both recoveries mirror `internal/api/middleware/recovery.go`: log with stack, mark
the health flag, return a generic internal error with a ticket UUID. Marking the
health flag means the first recovered panic on any door takes the node to
`503 DOWN` permanently, which under a liveness probe is a restart. That is the
existing HTTP contract and is deliberately extended, not softened: a node that has
panicked has unknown state, and restarting it is the correct response. §6 row 2's
"still serving" assertion is about request handling, which continues.

Recovery must not land without §1.2 and §1.3. On its own it would convert a
process crash — which PostgreSQL cleans up by killing every session — into a
silent connection leak.

### 1.5 Corrections to the issue's framing

Verified against the tree; the spec's tests depend on getting these right.

- **The node does not "look healthy".** `recovery.go:23` calls
  `healthFlag.Store(false)` and nothing resets it, so the health endpoint reports
  `503 DOWN` after the first recovered panic. The acceptance test asserts that
  *requests still succeed*, not that health is green.
- **The scheduled path is not a leak path for the entity service.** It enters via
  `FireScheduledTransition`, which already has the deferred guard.
- **sqlite holds no connection.** `plugins/sqlite/txmanager.go:149` opens no
  `*sql.Tx`; writes buffer in `spi.TransactionState.Buffer` and the real
  `db.BeginTx` happens inside `Commit` (`:386`). `SetMaxOpenConns(1)` is therefore
  irrelevant to an orphaned transaction. The real harm on memory and sqlite is the
  leaked buffer plus a pinned `committedLog` prune floor
  (`plugins/memory/txmanager.go:469`, `plugins/sqlite/txmanager.go:348`), which
  makes every later commit's conflict scan slower without bound.

---

## 2. DB-side ceilings (postgres)

| Var | Default | Limits | Mechanism |
|---|---|---|---|
| `CYODA_POSTGRES_STATEMENT_TIMEOUT` | `5m` | how long one SQL statement may run | server GUC |
| `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` | `5m` | how long a connection may sit inside an open transaction doing nothing | server GUC |
| `CYODA_POSTGRES_ACQUIRE_TIMEOUT` | `10s` | how long a request waits for a free pooled connection | Go-side deadline (§2.1) |

`0` disables any of them, matching PostgreSQL's own convention.

The two GUCs are set on the app pool via `ConnConfig.RuntimeParams` at connect
time (no `AfterConnect` round-trip). `pgxpool.Config` has no `AcquireTimeout`
field, which is why the third is a deadline rather than a fourth setting.

**Encoding.** Values go into the startup packet, so a malformed one fails
`pool.Ping` at boot for every deployment. PostgreSQL's time units are
`us`/`ms`/`s`/`min`/`h`/`d` — **`m` is not among them**, and Go's
`(5*time.Minute).String()` is `"5m0s"`, which is also invalid. The config values
are parsed as Go durations and emitted as bare integer milliseconds
(`strconv.FormatInt(d.Milliseconds(), 10)`), which is the default unit for all
three GUCs. A test asserts the rendered form.

**Precedence.** `pgxpool.ParseConfig` folds unrecognised DSN keys into
`RuntimeParams` (`plugins/postgres/config.go:131`), so an operator who already
sets one of these in `CYODA_POSTGRES_URL` would have it silently overwritten. The
plugin logs at WARN and the env var wins, so there is one authority and it is
visible.

The idle limit is the one that plugs the leak: an abandoned transaction is idle
by definition. It must clear the longest legitimate idle gap, which is a
compute-node callout — `responseTimeoutMs` defaults to 30s
(`internal/grpc/dispatch.go:32`), so 5m clears it tenfold. All cluster timeouts
sit well under it (`CYODA_TX_TOKEN_TTL` 90s, proxy and dispatch-forward 30s).

The margin is per-gap, not cumulative — a deep cascade (`maxCascadeDepth` is 100,
`engine.go:97`) can spend far more than 5m in total across many callouts. It stays
safe because the postgres audit store issues a real `INSERT` inside the transaction
between every processor (`engine_processors.go:129` →
`plugins/postgres/sm_audit_store.go:39`), which resets the idle timer. That is
load-bearing and was previously undesigned, so it is recorded here and asserted by
a test: a multi-processor cascade whose total callout time exceeds the ceiling
must still commit.

When PostgreSQL aborts a session, the handler's next operation on that `pgx.Tx`
fails cleanly and runs its existing error path — nothing is yanked out from under
a live goroutine.

**Async search** is the one workload whose purpose is to run long. It is
pool-direct (`plugins/postgres/search_store.go`) and bounded today only by a
row-count scan budget (`internal/domain/search/service.go:227`), never by time. A
5m `statement_timeout` newly caps it, and that is intended: an unbounded scan
holding a pooled connection is the same hazard in a different costume. Operators
who need longer raise the ceiling.

### 2.1 Acquire-timeout scope

Applied **only** at `TransactionManager.Begin`'s `pool.BeginTx`
(`transaction_manager.go:79`) and `model_store.go:359`'s `pool.Begin`. Both return
immediately, so the deadline bounds the acquire and does not leak into the
returned handle.

Both are also safe from nesting: `model_store.go:359` self-wraps only when there is
no ambient transaction (`:355`) and already has `defer tx.Rollback` (`:364`). Its
only reachable callers are `ValidateOrExtend`'s five entity/workflow call sites —
model import does **not** reach it — so it adds no endpoint to §5's table.

It is **not** applied to `pool.Query` / `Exec` / `QueryRow` / `CopyFrom`. For
`Query`, `pgxpool` holds the connection for the returned `pgx.Rows` under the same
context, so a deadline there caps statement execution and row iteration too — it
would break `search_store.go:113`'s `CopyFrom` of a whole async-search result set
and every non-transactional read routed through `StoreFactory.resolveRaw`.
(`Exec`/`QueryRow`/`CopyFrom` release before returning, so the objection is
narrower for them, but splitting the rule by method would be a trap for the next
reader.)
Bounding those properly means an explicit `Acquire`/`Release` restructure that
reimplements what `pgxpool` already does internally, with a connection-leak
failure mode of its own — disproportionate here, and unnecessary: those
statements are bounded server-side by `statement_timeout`, so the connection
returns within that ceiling regardless.

Net: every connection is released within `max(statement_timeout,
idle_in_tx_timeout)`, and interactive writes fail fast instead of queueing.

### 2.2 Error classification

Pool exhaustion surfaces as **503 `STORAGE_UNAVAILABLE`, retryable**.

The plugin must not classify on `context.DeadlineExceeded` alone: `pool.BeginTx`
returns it both when the acquire wait expired and when the *caller's* request
context expired, and mislabelling a client timeout as a retryable server 503 is
wrong. The plugin owns the acquire context, so it returns an error satisfying

```go
interface{ StorageUnavailable() bool }
```

which the domain layer type-asserts. No `cyoda-go-spi` change, so no coordinated
cross-repo release; the commercial backend can opt in later by returning the same
shape.

Two server-side aborts also need classifying, or they surface as opaque 500s:

- **`idle_in_transaction_session_timeout`** → SQLSTATE `25P03`. It *terminates the
  session*, it does not merely abort the transaction, so the next operation may
  return either a `*pgconn.PgError` carrying `25P03` or a transport error
  (unexpected EOF, broken pipe) depending on whether pgx reads the buffered
  `ErrorResponse` before noticing the closed socket. Classify **both** shapes.
  Which one actually arrives is settled by test, not by reading — same standard
  §3.2 applies to its own load-bearing claim.
- **`statement_timeout`** → SQLSTATE `57014` (`query_canceled`), on any statement,
  on any endpoint including reads. Nothing classifies it today, so it falls
  through `classifyError` (`plugins/postgres/transaction_manager.go:507`) as an
  unexplained error.

They are classified differently, because they differ in whether retrying helps:

- `25P03` and pool exhaustion → **503 `STORAGE_UNAVAILABLE`, retryable**. Both are
  transient contention; the same request may well succeed on a second attempt.
  `25P03` only arises inside a transaction, so it is reachable only on the write
  operations §5 already covers.
- `57014` → **500 with a ticket UUID**, per the project's 5xx convention (generic
  message to the client, full detail logged server-side). Re-running a statement
  that just exceeded the ceiling will exceed it again, so advertising it as
  retryable would be a lie. Every operation already declares
  `default: InternalServerError`, so this adds no wire-contract change and keeps
  §5's table at nine rows. What changes is that the log line names the ceiling
  instead of reporting an unexplained failure.

---

## 3. Migration startup safety (postgres)

### 3.1 Migration-connection settings

`openDB` builds the migration connection from `pool.Config().ConnConfig`
(`migrate.go:23`), so it would inherit the app pool's `statement_timeout` and kill
a legitimate long index build. It needs the opposite settings:

| Setting | Value |
|---|---|
| `lock_timeout` | `CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT`, default `5m` |
| `statement_timeout` | `0` |
| `idle_in_transaction_session_timeout` | `0` |

Doing work for a long time is fine; waiting is what must be bounded. `5m` rather
than something tighter because a migration's own DDL lock waits are legitimate
during a rolling upgrade — an old node's in-flight write transaction is itself
bounded by the ceilings in §2, so a bounded wait succeeds where a 30s one would
abort a healthy upgrade.

Applying it at `openDB` (`migrate.go:23`) covers every caller: `runMigrations` via
`plugin.go:50`, `checkSchemaCompat`'s own handle (`plugin.go:42`), and the
test-only `migrateDown` (`migrate.go:232`). `RunMigrateWithDSN` — the `cyoda
migrate` subcommand — builds an independent pool (`migrate.go:88`) that inherits
nothing from the app pool, though it *does* inherit any `RuntimeParams` embedded in
the DSN, so it sets the same three explicitly.

### 3.2 Single migrator

Serialisation across nodes is not new: golang-migrate's `Lock()` already blocks on
`SELECT pg_advisory_lock($1)` (`golang-migrate/v4@v4.19.1
database/pgx/v5/pgx.go:229`) and followers get `ErrNoChange` once the winner
finishes. What is missing is a *bound* — that call uses `context.Background()`, so
the wait is indefinite at the Go level.

The bound comes from the session `lock_timeout` in §3.1: advisory locks go through
PostgreSQL's regular lock manager, so it aborts the wait. A single-node install is
unaffected because its lock is uncontended.

A node whose lock wait times out re-runs `checkSchemaCompat`: if the schema is now
current, it logs and proceeds; otherwise it exits with an actionable message for
the supervisor to retry.

The `lock_timeout`-aborts-an-advisory-lock-wait claim is load-bearing and will be
proven by test, not taken from documentation.

### 3.3 Concurrent-boot dirty-flag false alarm

golang-migrate sets `dirty=true` **before** each migration step and clears it
after (`migrate.go:738`, `:750`), each in its own committed transaction. So while
one node migrates, every other booting node reads `dirty=true` and
`checkSchemaCompat` (`plugins/postgres/migrate.go:195`) exits with *"migration
state is dirty — manual intervention required"* — a fatal false alarm on a
completely normal concurrent boot, which invites an operator to hand-edit
`schema_migrations` while a live migration is running.

The bug exists only because `checkSchemaCompat` (`plugin.go:42`) runs *before*
`runMigrations` (`:50`) and reads `dirty` unserialised. So serialise it: take one
**cyoda-owned session advisory lock**, on its own connection with `lock_timeout`
set, around the whole `[checkSchemaCompat; runMigrations]` sequence. Under that
lock, `dirty == true` unambiguously means a migration genuinely died, and stays
fatal exactly as today.

This is strictly nested outside golang-migrate's own lock, so there is no deadlock,
and it avoids the alternative — recomputing golang-migrate's lock id via
`database.GenerateAdvisoryLockId` and probing `pg_locks` — which would couple us to
that function's hash, its argument order, and the driver's internal derivation of
database/schema/table at `WithInstance` (`pgx.go:73`). One lock we own is less
machinery and fewer things that can silently drift.

### 3.4 Index migrations

Migrations `000001`–`000006` are **not** modified.

The one migration whose index could block live writers is `000002`, which adds
`entities_state_idx` to `entities` — a table that already holds data by then, and
whose writers a non-concurrent `CREATE INDEX` locks out for the duration of the
build (SHARE conflicts with the ROW EXCLUSIVE every INSERT/UPDATE/DELETE holds).
But `000002` only runs on a database below schema version 2, which means an
instance last on v0.7.x; v0.8.1 is version 2, v0.8.2 is 3, v0.8.3 is 6. There are
no such instances, so the case is not worth engineering for. Nothing else in
`000003`–`000006` blocks a hot table: `000003` and `000004` create their own new
tables, `000005` is functions only, and `000006` adds two defaulted columns to
`scheduled_tasks`, needing only a brief AccessExclusive lock that
`lock_timeout` now bounds.

The exposure that remains is the *next* index migration. Deliverables:

- **Pattern**: `CREATE INDEX CONCURRENTLY` alone in its own migration file. The
  driver sends the whole file through one `Exec` with `MultiStatementEnabled`
  false (`pgx.go:270`), and PostgreSQL wraps a multi-statement simple query in an
  implicit transaction, in which `CREATE INDEX CONCURRENTLY` cannot run.
  `000002_grouped_stats.up.sql` is the proof: a function plus an index in one
  file.
- **Guard test rule**, two clauses: (a) an index added to a table created in an
  *earlier* migration must be `CONCURRENTLY` — an index created in the same
  migration as its own table need not be, because that table is empty and
  unreachable by writers; (b) a file containing `CREATE INDEX CONCURRENTLY` must
  contain no other statement, or the implicit transaction above makes it fail at
  runtime. Clause (a) passes `000001`'s indexes on their merits rather than by
  exemption; `000002_grouped_stats.up.sql:44` is the sole grandfathered entry.
- **Recovery**: a failed `CREATE INDEX CONCURRENTLY` leaves an INVALID index and a
  dirty version. Document the `DROP INDEX` + re-run procedure in the migration
  help topic and reference it from the dirty-state error message.

---

## 4. Remove `internal/cluster/lifecycle`

### 4.1 Evidence

- The package arrived whole in `d1f6875`, "Initial import from cyoda-light-go"
  (2026-04-14). No commit in cyoda-go history ever added a production call to
  `Register`, `RecordOutcome`, `IsAlive`, `GetOutcome`, `ListByNode` or `Remove`.
  It was imported inert; `active` is permanently empty, so `ReapExpired` reaps
  nothing. `SetTransactionManager` is the sole method with a production caller
  (`app/app.go:444`), and it exists only to serve the reaper.
- The reaper goroutine only starts when `cfg.Cluster.Enabled` (`app/app.go:445`),
  so single-node never had one at all.
- Even if a transaction were registered, `ReapExpired` calls
  `tm.Rollback(context.Background(), …)`, which `verifyTenant` rejects on all three
  in-tree plugins (`plugins/postgres/transaction_manager.go:486`,
  `plugins/memory/txmanager.go:513`, `plugins/sqlite/txmanager.go:567`) — a
  background context carries no `UserContext`.
- Transaction affinity does not use it. Routing is driven by a signed token
  carrying node ID and expiry (`internal/cluster/proxy/http.go`,
  `internal/cluster/token`), with its own live knob `CYODA_TX_TOKEN_TTL`.
- When a node dies, its PostgreSQL sessions die with it and the server rolls those
  transactions back. No cluster bookkeeping is required for that.
- `CYODA_TX_TTL` has zero consumers, not even the reaper — `Register`'s `ttl`
  argument is never supplied. `CYODA_TX_REAP_INTERVAL` and `CYODA_TX_OUTCOME_TTL`
  feed only the dead Manager.
- The commercial cassandra backend cannot reference it — Go forbids importing
  another module's `internal/` — and does not: it has its own unrelated
  `internal/tx.TxLifecycle` enum and its own `CYODA_CASSANDRA_TX_*` timers, and
  from cyoda-go's root module it imports only `app.Config`, `app.DefaultConfig`,
  `app.LoadEnvFiles`, `app.New`, `app.ProfileBanner`. Neither it nor cyoda-go-paas
  references `TxTTL`, `OutcomeTTL`, `TxReapInterval` or the three env vars.

Nothing is lost. The one capability forgone is a test's ability to assert *which*
node holds a transaction (`e2e/parity/multinode/cbd_tx_pinning.go:54`); that test
already asserts the observable signature through version history.

The alternative — wiring the reaper — is rejected on the grounds the issue
records: `tm.Rollback` → `cleanupTx` → `registry.Remove` means a transaction reaped
out from under a still-running handler makes the handler's remaining writes fall
through to the pool (`resolveRaw` returns `f.pool` on a registry miss) and
auto-commit as standalone statements. Partial, non-atomic application is worse
than the leak, and making it safe needs a cancel handshake the DB-side ceiling
does not.

### 4.2 Removal list

Code: `internal/cluster/lifecycle/` (both files), `App.TxLifecycle()`
(`app/app.go:822`), the `txLifecycle` field and its construction/wiring
(`app/app.go:70`, `:440-444`), the reaper goroutine (`:459-477`) and `stopReaper`
(`:72`, `:887-888`), the three `cluster.Config` fields
(`internal/cluster/config.go:16,17,19`) and their `app/config.go` bindings
(`:298`, `:299`, `:301`).

Tests: `internal/cluster/lifecycle/manager_test.go`, and
`TestEndToEnd_LifecycleTracking` in `internal/cluster/integration_test.go:103-128`
plus its import at `:11` — it constructs `lifecycle.NewManager` and asserts
`OutcomeRolledBack`, so it will not compile otherwise.

Config surface: `cmd/cyoda/help/config_registry.go:59-61`,
`app/config_registry_binding_test.go:101-103`,
`cmd/cyoda/help/content/config.md:91-93`, and
`scripts/multi-node-docker/start-cluster.sh:418`, which emits
`CYODA_TX_TTL: "60s"` into the generated compose file for every cluster node.

### 4.3 Claims in the tree that must be corrected

These ship today and describe a capability that has never existed:

- `docs/PRD.md:346` — "A background reaper goroutine periodically scans for
  expired transactions and rolls them back."
- `docs/PRD.md:319` — the `ROLLBACK ◄──── timeout (TTL reaper)` state diagram.
- `docs/ARCHITECTURE.md:365-380` — an entire section, "3.4 Transaction Lifecycle
  Manager", with a struct listing.
- `docs/ARCHITECTURE.md:123` — `lifecycle/ Transaction lifecycle manager (TTL,
  reaper, outcomes)` in the package tree.
- `docs/ARCHITECTURE.md:1425-1426`, `:1428` — the three env vars as live knobs.
  `:1427` is `CYODA_PROXY_TIMEOUT` and stays.
- `docs/ARCHITECTURE.md:1569` — the DD-2 design-decision rationale.
- `docs/ARCHITECTURE.md:1650-1651` — "Workflow chains that exceed TTL are reaped.
  Long-running processors must complete within this window", and the companion row
  advising that `idle_in_transaction_session_timeout` should *exceed* the TTL.
  Both are rewritten around §2's ceilings, which invert that advice: the DB-side
  limit is now the authority. `:1652-1655` are unrelated rows and stay.
- `docs/CONCURRENCY.md:63` and `:105` — the latter cites a path that stops
  existing.
- `docs/analysis/failure-modes/…-playbook.md:59` — the actionable companion to the
  analysis document dispositioned below. A playbook that instructs an operator to
  rely on a reaper that does not run is worse than no playbook.
- `e2e/parity/multinode/cbd_tx_pinning.go:54` — "not yet wired into the runtime".
  Comment-only, no compile dependency.

`docs/analysis/failure-modes/2026-06-29-operational-failure-mode-analysis.md:288,311`
names `lifecycle.Manager` as remediation R1 for this exact issue. That directory is
a historical record like `docs/plans/`, so it is not rewritten; the spec records
the disposition so a future reader does not re-propose the wired reaper.

---

## 5. Error and status codes

New code: `STORAGE_UNAVAILABLE` — 503, retryable. Emitted when the pool cannot
supply a connection within `CYODA_POSTGRES_ACQUIRE_TIMEOUT`, or when an operation
finds its transaction aborted by the idle-in-transaction ceiling (SQLSTATE
`25P03`). Needs `cmd/cyoda/help/content/errors/STORAGE_UNAVAILABLE.md`
(`TestErrCode_Parity` is a strict bijection).

No status code changes to any existing endpoint. `503` becomes newly *reachable*
on the entity write operations, which must therefore declare it in
`api/openapi.yaml` — the e2e validator runs `ValidateResponse` with
`IncludeResponseStatus=true` (`internal/e2e/openapivalidator/validator.go:168`), so
an undeclared 503 fails conformance.

| Operation | Method + path | Declared today | Add |
|---|---|---|---|
| `create` | POST `/entity/{format}/{entityName}/{modelVersion}` | 200, 400, 401, 403, 404, 409, 422 | 503 |
| `createCollection` | POST `/entity/{format}` | 200, 400, 401, 403, 404, 409, 422 | 503 |
| `updateSingle` | PUT `/entity/{format}/{entityId}/{transition}` | 200, 400, 401, 403, 404, 409, 412, 422 | 503 |
| `updateSingleWithLoopback` | PUT `/entity/{format}/{entityId}` | 200, 400, 401, 403, 404, 409, 412, 422 | 503 |
| `updateCollection` | PUT `/entity/{format}` | 200, 400, 401, 403, 404, 409, 422 | 503 |
| `patchSingle` | PATCH `/entity/{format}/{entityId}/{transition}` | 200, 400, 401, 403, 404, 409, 412, 415, 422, 428, 501 | 503 |
| `patchSingleWithLoopback` | PATCH `/entity/{format}/{entityId}` | 200, 400, 401, 403, 404, 409, 412, 415, 422, 428, 501 | 503 |
| `deleteSingleEntity` | DELETE `/entity/{entityId}` | 200, 400, 401, 403, 404 | 503 |
| `deleteEntities` | DELETE `/entity/{entityName}/{modelVersion}` | 200, 400, 401, 403, 404 | 503 |

Every one of these also carries `default: InternalServerError` (500), which is
unchanged. The "declared today" column is extracted from `api/openapi.yaml` at
HEAD; no existing entry is modified, and the added 503 is the only change to any
operation's response set.

gRPC: the same failure surfaces in the envelope as `Success=false` with
`Error.Code = STORAGE_UNAVAILABLE`.

---

## 6. Coverage matrix

| # | Scenario | Unit | Running-backend e2e | Cross-backend parity | gRPC |
|---|---|---|---|---|---|
| 1 | Panic in an owned write path rolls back; pool returns to baseline | — | postgres | — | — |
| 2 | Repeated panics beyond pool size leave the node serving requests | — | postgres | — | — |
| 3 | Panic in a joined callback does **not** roll back the owner's tx | ✔ | postgres | ✔ | — |
| 4 | Panic after engine segmentation rolls back TX_post, not the entry tx | ✔ | postgres | — | — |
| 4a | **Non-panic** engine error after segmentation (criterion callout fails mid-cascade) rolls back TX_post | ✔ | postgres | ✔ | — |
| 4b | Every `executeCommitBeforeDispatch` `return nil, "", err` path rolls its segment back | ✔ | — | — | — |
| 5 | Committed-transaction behaviour unchanged (existing write suites) | ✔ | ✔ | ✔ | ✔ |
| 5a | **Ordinary error paths still roll back** — one case per converted flow, asserting the tx is gone, not just the status code | ✔ | postgres | ✔ | — |
| 6 | Panicking write on memory/sqlite releases its tx state — no leaked buffer, `committedLog` prune floor advances | ✔ | — | — | — |
| 7 | gRPC handler panic is recovered; process survives; tx rolled back | — | — | — | ✔ |
| 7a | Peer scheduler-RPC panic is recovered; process survives; fire tx rolled back | — | postgres | — | — |
| 8 | `Release` holds the per-tx gate while rolling back | ✔ | — | — | — |
| 9 | Idle-in-tx beyond the ceiling: session aborted, next op returns 503 `STORAGE_UNAVAILABLE` | — | postgres | — | ✔ |
| 10 | Saturated pool: a **write** returns 503 within the acquire timeout rather than queueing | — | postgres | — | ✔ |
| 11 | Caller-cancelled request is **not** mislabelled `STORAGE_UNAVAILABLE` | ✔ | postgres | — | — |
| 11a | GUC values render in a form PostgreSQL accepts (bare ms integers, never `5m`) | ✔ | postgres | — | — |
| 11b | Deep cascade whose total callout time exceeds the idle ceiling still commits (per-gap, not cumulative) | — | postgres | — | — |
| 12 | `statement_timeout` fires → SQLSTATE `57014` → 500 with a ticket, cause named in the log | ✔ | postgres | — | — |
| 13 | `lock_timeout` aborts a `pg_advisory_lock` wait | ✔ | postgres | — | — |
| 14 | Migration connection inherits neither `statement_timeout` nor `idle_in_transaction_session_timeout` from the app pool | ✔ | — | — | — |
| 15 | Concurrent boot while another node migrates: waits instead of declaring dirty fatal, then proceeds once the schema is current | — | postgres | — | — |
| 16 | Genuinely dirty schema with no lock holder still fails fast | ✔ | postgres | — | — |
| 17 | Single-node install migrates itself | — | postgres | — | — |
| 18 | Guard test rejects a new non-concurrent index on a hot table | ✔ | — | — | — |
| 19 | `STORAGE_UNAVAILABLE` declared in OpenAPI on every write op | — | conformance | — | — |

Row 5a is the highest-value row in the table. Row 5 leans on the existing write
suites, which assert response codes and never observe transaction state — so a
single defect in `Release` would leave a transaction open on *every* error path in
the entity service with no test noticing. 5a asserts the transaction is actually
gone.

Scenarios 1, 2 and 10 are concurrency/fault tests: isolated single-backend e2e,
never the shared parity suite, and they assert consistency (pool returns to
baseline, one winner, no torn write) rather than a precise interleave.

**Fixture.** The shared e2e suite builds one `app.App` and one pool in `TestMain`
with `CYODA_POSTGRES_MAX_CONNS=5` (`internal/e2e/e2e_test.go:106`, `:156`).
Scenarios 1, 2 and 10 need their own app with a deliberately tiny pool — run
against the shared one they cannot isolate, and row 10 would stall the rest of the
suite for a full acquire timeout. They construct a dedicated `app.App` and read
`pool.Stat()` through `App.StoreFactory()` type-asserted to the plugin's `Pool()`
accessor.

**Panic injection** needs no production surface and no new hook: the harness
already injects behaviour through `cfg.ExternalProcessing`
(`internal/e2e/e2e_test.go:140`, `internal/testing/localproc`). A registered
processor that panics puts the panic inside `engine.Execute`, which is exactly
where it matters, with nothing compiled into the binary.

---

## 7. Documentation (Gate 4)

- New vars in `plugins/postgres/plugin.go` `ConfigVars()`, `parseConfig`, and
  `cmd/cyoda/help/content/config/database.md`:
  `CYODA_POSTGRES_STATEMENT_TIMEOUT`, `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT`,
  `CYODA_POSTGRES_ACQUIRE_TIMEOUT`, `CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT`.
- Removed vars: `cmd/cyoda/help/config_registry.go`,
  `cmd/cyoda/help/content/config.md`, `docs/ARCHITECTURE.md`.
- `cmd/cyoda/help/content/errors/STORAGE_UNAVAILABLE.md`.
- Migration guidance (CONCURRENT pattern, INVALID-index recovery) in
  `cmd/cyoda/help/content/cli/migrate.md`.
- Note: nothing enforces `ConfigVars()` ↔ `parseConfig` parity for plugin vars, so
  a var added to one and not the other passes CI silently. The four new vars are
  added to both, and a parity test is added alongside them — the same class of
  gap `TestErrCode_Parity` already closes for error codes (Gate 6).
- `CHANGELOG.md` needs a `### Breaking` section: three env vars are removed, and
  three ceilings that did not exist now apply by default. Per `COMPATIBILITY.md`
  that section, not the version digit, is what consumers are told to read.
  v0.8.4 remains a PATCH — the HTTP/wire API is unchanged.

## 8. Out of scope

- Capping `responseTimeoutMs` against the idle ceiling. It is per-processor
  workflow config and currently uncapped, so a workflow may configure a callout
  longer than 5m and have its transaction aborted. This is reconciled here by
  classifying SQLSTATE `25P03` into a clear, actionable error (§2.2) and
  documenting the relationship, rather than by adding cross-layer validation
  between workflow import and storage-plugin config. A cap belongs with the gRPC
  hardening work.
- Read-only routes registered outside `middleware.Recovery` (`app/app.go:671`,
  `:672`, `:718`). They open no transaction, so a panic there leaks nothing —
  unlike the peer scheduler-RPC route, which does and is therefore pulled into
  scope in §1.4. The remaining inconsistency is left to the HTTP-hardening issue.
- Request-level context deadlines (#32), which would bound pool acquisition on
  read paths as well.
- Context-aware acquisition for `tx.OpMu` and the txgate (see §1.2). Both would
  make the rollback bound real on memory and sqlite, and both are changes to two
  plugins' concurrency model rather than to transaction lifecycle.
- `fetchEntityTransitions` lacks the 503 that its documented alias
  `getEntityTransitions` declares (`api/openapi.yaml:1584`,
  `transitions_handler.go:117`). Pre-existing drift on the same surface, unrelated
  to this change's mechanism; flagged so it is not mistaken for something this
  change introduced.
