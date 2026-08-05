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
scope, err := h.beginScope(ctx)   // beginOrJoin + joined-gate acquire
defer scope.Release()
...
scope.Advance(result.FinalCtx, result.FinalTxID)   // FIRST statement after every engine call
...
err := scope.Commit()
```

All 40 explicit `rollbackOwned` calls are **deleted**, not duplicated.

Required properties:

- **Joined callbacks never roll back their owner's transaction.** `Release` is a
  no-op when `owned == false`, matching `rollbackOwned` (`handler.go:107`).
- **`Release` targets the segment actually open.** `Advance` must be the first
  statement after every engine call — `service.go:325`, `:1256`, `:1506`, `:1849`
  currently interleave `StopReason`/`Segmented` handling first, leaving a window
  where the scope names a committed transaction.
- **`Release` acquires the per-tx gate before rolling back.** Today the finalize
  rollbacks (`service.go:356`, `:680`, `:833`, `:1283`, `:1548`, `:1556`, `:1567`,
  `:1872`, `:1911`) run inside `h.gate.Acquire(...)`, so they cannot race a joined
  callback's access to the same `pgx.Tx`. An outer defer runs after the gate is
  released, so `Release` must re-acquire or the guard silently weakens what it
  replaces. No self-deadlock: the owner never holds the gate at outer-defer time
  (the IIFE's own deferred release runs first during unwinding), and joined
  callbacks release it across dispatch via `txgate.Suspend`.
- **Gate lifetime and rollback lifetime are mutually exclusive by `owned`.** The
  existing `defer releaseGate()` exists only on the joined path; `Release` only
  rolls back on the owned path. There is no ordering interaction between them.
- **`Commit()` marks the scope done regardless of outcome.** This preserves
  today's behaviour exactly — no path rolls back after a failed commit
  (`service.go:980` documents why) — and avoids aborting a commit another
  goroutine is running on memory's `ErrTxCommitInProgress` path
  (`plugins/memory/txmanager.go:222`).
- **Rollback runs on a fresh, bounded context**:
  `context.WithTimeout(context.WithoutCancel(ctx), 5s)`. `WithoutCancel` keeps
  the `UserContext` that `verifyTenant` reads (`transaction_manager.go:486`) while
  dropping cancellation, so a timed-out request still returns its connection; the
  timeout stops a wedged `Rollback` from blocking the unwinding goroutine forever.
  memory and sqlite additionally take `tx.OpMu` in `Rollback`
  (`plugins/memory/txmanager.go:518`), which waits on in-flight operations.
  Applied unconditionally — `FinalCtx` is `WithoutCancel`-wrapped only in the
  `startNewTxOnDispatch=false` case (`engine_result.go:25`).

Flows converted: `CreateEntity`, `DeleteEntity`, `DeleteAllEntities`,
`DeleteEntitiesConditional`, `CreateEntityCollection`, `updateEntityCore` (and
`UpdateEntity` / `PatchEntity` through it), `UpdateEntityCollection`.

`internal/domain/model` opens no transaction of its own, and the message paths do
not either — `beginOrJoin` is the entity service's alone. That answers the
question the issue left open: they do not share the shape and need no change.

### 1.3 Engine-side guard

`executeCommitBeforeDispatch` and the segment-owning loop in `executeProcessors`
each get a named-return guard over the segment they own:

```go
handedOff := false
defer func() {
    if !handedOff && newTxID != "" {
        _ = e.txMgr.Rollback(rollbackCtx(newCtx), newTxID)
    }
}()
...
handedOff = true
return newCtx, newTxID, nil
```

`handedOff` is set only where the segment is handed to the caller, so a panic
anywhere in between rolls it back. The four plain rollback calls are removed.

### 1.4 gRPC panic recovery

`internal/grpc/server.go:69` installs auth and tx-route interceptors only, and
grpc-go does not recover handler panics — a panic in a gRPC entity write takes
down the process. `DeleteAllEntities` is reachable **only** over gRPC
(`internal/grpc/entity.go:399`), so this is not a hypothetical path.

Add a recovery interceptor (unary + stream) mirroring
`internal/api/middleware/recovery.go`: log with stack, mark the health flag,
return a generic internal error with a ticket UUID.

These two changes must land together. Recovery without the deferred rollback
would convert a crash — which PostgreSQL cleans up by killing the session — into
a silent connection leak.

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

Three limits, set on the app pool via `ConnConfig.RuntimeParams` at connect time
(no `AfterConnect` round-trip). `pgxpool.Config` has no `AcquireTimeout` field.

| Var | Default | Limits |
|---|---|---|
| `CYODA_POSTGRES_STATEMENT_TIMEOUT` | `5m` | how long one SQL statement may run |
| `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` | `5m` | how long a connection may sit inside an open transaction doing nothing |
| `CYODA_POSTGRES_ACQUIRE_TIMEOUT` | `10s` | how long a request waits for a free pooled connection |

`0` disables any of them, matching PostgreSQL's own convention.

The idle limit is the one that plugs the leak: an abandoned transaction is idle
by definition. It must clear the longest legitimate idle gap, which is a
compute-node callout — `responseTimeoutMs` defaults to 30s
(`internal/grpc/dispatch.go:32`), so 5m clears it tenfold. All cluster timeouts
sit well under it (`CYODA_TX_TOKEN_TTL` 90s, proxy and dispatch-forward 30s).

When PostgreSQL aborts a session, the handler's next operation on that `pgx.Tx`
fails cleanly and runs its existing error path — nothing is yanked out from under
a live goroutine.

### 2.1 Acquire-timeout scope

Applied **only** at `TransactionManager.Begin`'s `pool.BeginTx`
(`transaction_manager.go:79`) and `model_store.go:359`'s `pool.Begin`. Both return
immediately, so the deadline bounds the acquire and does not leak into the
returned handle.

It is **not** applied to `pool.Query` / `Exec` / `QueryRow` / `CopyFrom`.
`pgxpool` holds the connection for the returned `pgx.Rows` under the same
context, so a deadline there caps statement execution and row iteration too — it
would break `search_store.go:113`'s `CopyFrom` of a whole async-search result set
and every non-transactional read routed through `StoreFactory.resolveRaw`.
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

A transaction aborted by `idle_in_transaction_session_timeout` surfaces on the
next operation as SQLSTATE `25P03`. Classify it explicitly rather than letting an
opaque connection error through: 503 `STORAGE_UNAVAILABLE` with a message naming
the ceiling, so an operator can act on it.

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

There are **three** paths that open a migration connection; all three need this:

1. `plugin.NewFactory` → `runMigrations` → `openDB(pool)` (`plugin.go:49`)
2. `checkSchemaCompat`'s own `openDB(pool)` (`plugin.go:42`)
3. `RunMigrateWithDSN`, the `cyoda migrate` subcommand, which builds an
   independent pool and inherits nothing (`migrate.go:88`)

### 3.2 Single migrator

golang-migrate's `Lock()` runs `SELECT pg_advisory_lock($1)` on
`context.Background()` (`golang-migrate/v4@v4.19.1 database/pgx/v5/pgx.go:229`) —
indefinite at the Go level. Advisory locks go through PostgreSQL's regular lock
manager, so a session `lock_timeout` aborts the wait. Whoever acquires it
migrates; the others back off. No leader election is introduced, and a single-node
install still migrates itself because its lock is uncontended.

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

This is a pre-existing bug in the same family, fixed here: when the state is
dirty, check whether the migration advisory lock is currently held (its id is
derived by `database.GenerateAdvisoryLockId` from database, schema and table
names, so it can be recomputed and looked up in `pg_locks`).

- Lock held → another node is mid-migration. Wait for it, bounded by
  `CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT`, then re-check.
- Lock not held → a migration genuinely died. Fatal, as today.

### 3.4 Index migrations

Migrations `000001`–`000006` are **not** modified. golang-migrate stores only
`(version, dirty)` and never checksums applied migrations
(`pgx.go:338`), so editing an applied file changes nothing on any database that
has already run it, and a fresh install has no concurrent writers for a
non-concurrent index to block.

The exposure is the *next* index migration on a hot table. Deliverables:

- **Pattern**: `CREATE INDEX CONCURRENTLY` alone in its own migration file. The
  driver sends the whole file through one `Exec` with `MultiStatementEnabled`
  false (`pgx.go:270`), and PostgreSQL wraps a multi-statement simple query in an
  implicit transaction, in which `CREATE INDEX CONCURRENTLY` cannot run.
  `000002_grouped_stats.up.sql` is the proof: a function plus an index in one
  file.
- **Guard test**: fails when a *new* migration creates a non-concurrent index on
  `entities` or `entity_versions`. Existing files are grandfathered by an explicit
  allow-list — `000001_initial_schema.up.sql:23`, `:41`, `:44` and
  `000002_grouped_stats.up.sql:44` are all non-concurrent today and the test would
  otherwise fail on day one.
- **Recovery**: a failed `CREATE INDEX CONCURRENTLY` leaves an INVALID index and a
  dirty version. Document the `DROP INDEX` + re-run procedure in the migration
  help topic and reference it from the dirty-state error message.

---

## 4. Remove `internal/cluster/lifecycle`

### 4.1 Evidence

- The package arrived whole in `d1f6875`, "Initial import from cyoda-light-go"
  (2026-04-14). No commit in cyoda-go history ever added a production call to
  `Register`, `RecordOutcome`, `IsAlive`, `GetOutcome` or `ListByNode`. It was
  imported inert; `active` is permanently empty, so `ReapExpired` reaps nothing.
- The reaper goroutine only starts when `cfg.Cluster.Enabled` (`app/app.go:461`),
  so single-node never had one at all.
- Even if a transaction were registered, `ReapExpired` calls
  `tm.Rollback(context.Background(), …)`, which `verifyTenant` rejects
  (`transaction_manager.go:486`) — a background context carries no `UserContext`.
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
(`app/app.go:70`, `:440-443`), the reaper goroutine (`:459-477`) and `stopReaper`,
the three `cluster.Config` fields (`internal/cluster/config.go:16,17,19`) and
their `app/config.go` bindings (`:298`, `:299`, `:301`).

Tests: `internal/cluster/lifecycle/manager_test.go`,
and `internal/cluster/integration_test.go:11,104,125`, which constructs
`lifecycle.NewManager` and asserts `OutcomeRolledBack` — it will not compile
otherwise.

Config surface: `cmd/cyoda/help/config_registry.go:59-61`,
`app/config_registry_binding_test.go:101-103`,
`cmd/cyoda/help/content/config.md:91-93`.

### 4.3 Claims in the tree that must be corrected

These ship today and describe a capability that has never existed:

- `docs/PRD.md:346` — "A background reaper goroutine periodically scans for
  expired transactions and rolls them back."
- `docs/PRD.md:319` — the `ROLLBACK ◄──── timeout (TTL reaper)` state diagram.
- `docs/ARCHITECTURE.md:123` — `lifecycle/ Transaction lifecycle manager (TTL,
  reaper, outcomes)` in the package tree.
- `docs/ARCHITECTURE.md:1425-1426`, `:1428` — the three env vars as live knobs.
  `:1427` is `CYODA_PROXY_TIMEOUT` and stays.
- `docs/ARCHITECTURE.md:1650-1656` — "Workflow chains that exceed TTL are reaped.
  Long-running processors must complete within this window", and a companion row
  advising that `idle_in_transaction_session_timeout` should *exceed* the TTL.
  Both are rewritten around §2's ceilings, which invert that advice: the DB-side
  limit is now the authority.
- `e2e/parity/multinode/cbd_tx_pinning.go:54` — "not yet wired into the runtime".

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
| 5 | Committed-transaction behaviour unchanged (existing write suites) | ✔ | ✔ | ✔ | ✔ |
| 6 | Panicking write on memory/sqlite releases its tx state — no leaked buffer, `committedLog` prune floor advances | ✔ | — | — | — |
| 7 | gRPC handler panic is recovered; process survives; tx rolled back | — | — | — | ✔ |
| 8 | `Release` holds the per-tx gate while rolling back | ✔ | — | — | — |
| 9 | Idle-in-tx beyond the ceiling: session aborted, next op returns 503 `STORAGE_UNAVAILABLE` | — | postgres | — | ✔ |
| 10 | Saturated pool: a **write** returns 503 within the acquire timeout rather than queueing | — | postgres | — | ✔ |
| 11 | Caller-cancelled request is **not** mislabelled `STORAGE_UNAVAILABLE` | ✔ | postgres | — | — |
| 12 | `statement_timeout` aborts a runaway statement cleanly | ✔ | postgres | — | — |
| 13 | `lock_timeout` aborts a `pg_advisory_lock` wait | ✔ | postgres | — | — |
| 14 | Migration connection does not inherit the app pool's `statement_timeout` | ✔ | — | — | — |
| 15 | Concurrent boot while another node migrates: waits instead of declaring dirty fatal, then proceeds once the schema is current | — | postgres | — | — |
| 16 | Genuinely dirty schema with no lock holder still fails fast | ✔ | postgres | — | — |
| 17 | Single-node install migrates itself | — | postgres | — | — |
| 18 | Guard test rejects a new non-concurrent index on a hot table | ✔ | — | — | — |
| 19 | `STORAGE_UNAVAILABLE` declared in OpenAPI on every write op | — | conformance | — | — |

Scenarios 1, 2 and 10 are concurrency/fault tests: isolated single-backend e2e,
never the shared parity suite, and they assert consistency (pool returns to
baseline, one winner, no torn write) rather than a precise interleave.

Panic injection uses a test-only fault hook compiled into the e2e harness — not a
production flag — so no injection surface ships in the binary.

---

## 7. Documentation (Gate 4)

- New vars in `plugins/postgres/plugin.go` `ConfigVars()`, `parseConfig`, and
  `cmd/cyoda/help/content/config/database.md`:
  `CYODA_POSTGRES_STATEMENT_TIMEOUT`, `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT`,
  `CYODA_POSTGRES_ACQUIRE_TIMEOUT`, `CYODA_POSTGRES_MIGRATE_LOCK_TIMEOUT`.
- Removed vars: `cmd/cyoda/help/config_registry.go`,
  `cmd/cyoda/help/content/config.md`, `docs/ARCHITECTURE.md`.
- `cmd/cyoda/help/content/errors/STORAGE_UNAVAILABLE.md`.
- Migration guidance (CONCURRENT pattern, INVALID-index recovery) in the migration
  help topic.
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
  `:672`, `:718`). They open no transaction, so they leak nothing; the
  inconsistency is noted here and left to the HTTP-hardening issue.
- Request-level context deadlines (#32), which would bound pool acquisition on
  read paths as well.
