# Schema extensions: commit-bound on every backend, never a conflict (#710)

## 1. Intent

A model with a `changeLevel` grows as entity writes bring new fields: the write
computes an additive delta and calls `ModelStore.ExtendSchema`. High-volume
ingestion into such a model is a common case. Extensions are additive and
idempotent, so two writers extending the same model never have a real
conflict, and a client must never get one.

Today postgres gives a `409 CONFLICT` to the second of two transactions that
extend the same model, and the backends disagree on when an extension becomes
visible. This change makes one contract hold on every backend:

- **No conflict.** Extending a model never fails a write, and never makes
  another write fail, because another transaction extends the same model.
- **Commit-bound.** An extension made in a transaction is visible to reads in
  that transaction at once, to everyone else when the transaction commits,
  and to no one if it rolls back (or its savepoint is rolled back).

### Goals

- The contract above on memory, sqlite and postgres, stated in the SPI and
  checked by new spitest conformance cases.
- Every extension happens inside the entity transaction, on every write path.
- The model cache never serves an uncommitted extension to another
  transaction, and is invalidated, locally and across the cluster, when the
  extending transaction commits.
- The postgres extension log never loses a committed delta, without
  serialising extending writers.

### Non-goals

- The cassandra plugin (out of tree). An issue in `cyoda-go-cassandra`
  describes the contract change; today it folds each extension into the model
  entity in a transaction of its own and retries conflicts.
- Any change to what an extension may add (`changeLevel` rules, `schema.Diff`,
  `schema.Apply`).

### Anti-goals

- A `409` (or any error) caused by another transaction's extension.
- An extension visible to another transaction before its own commits, or
  after it rolled back, through storage or through the model cache.
- A committed delta missing from the model's schema.

## 2. Today

Verified against `release/v0.9.0` 9058cb92.

**Callers** (`ingest.ValidateOrExtend`, `internal/domain/model/ingest/validate.go`
~:77, calls `ExtendSchema` at ~:163):

| Caller | Runs |
|---|---|
| `CreateEntity`, `internal/domain/entity/service.go` ~:257 | before `Begin` (~:275) |
| `CreateEntityCollection`, ~:1971 | before `Begin` (~:2006); each item reads the model with no transaction (~:1946) |
| `updateEntityCore` (PUT), ~:2238 | in the transaction (`Begin` ~:2181) |
| `UpdateEntityCollection`, ~:2534 | in the transaction (`Begin` ~:2466) |
| `applyProcessorData`, `internal/domain/workflow/processor_output.go` ~:68 | in the engine's transaction (TX_post after a `COMMIT_BEFORE_DISPATCH` segment) |

**Backends.**

- **memory** (`plugins/memory/model_store.go` ~:112-133) applies the delta to
  the stored schema at once under `modelMu`, ignoring the transaction. Visible
  to all, kept on rollback.
- **sqlite** (`plugins/sqlite/model_store.go` ~:444-571) appends the delta in a
  transaction of its own on the writer connection (the ambient transaction id
  is recorded "purely for diagnostic traceability", ~:436-439), with a retry
  loop on `SQLITE_BUSY` (`CYODA_SCHEMA_EXTEND_MAX_RETRIES`, default 8,
  returning `spi.ErrRetryExhausted`). Visible to all, kept on rollback.
- **postgres** (`plugins/postgres/model_store.go` ~:400-568) appends in the
  ambient transaction when there is one (otherwise a self-wrapped `READ
  COMMITTED` one, `inWriteTx` ~:419-467). Its first statement is a write claim,
  `UPDATE models SET doc = doc` (~:498-501), which serialises extending writers.
  Under the entity transaction's `REPEATABLE READ` the second writer fails
  `40001`, answered `409 CONFLICT` (retryable) by `common.Internal`; from
  processor output the cause is dropped (`%s`, `processor_output.go` ~:70) and
  the answer is `500`. Commit-bound only on the in-transaction paths.
- The SPI doc (`ExtendSchema`, `persistence.go` ~:558-579) says success means
  "durably committed and visible to subsequent reads on this node";
  `docs/CONSISTENCY.md`
  §3a invariant 2 says extensions are commit-bound. They contradict each other
  and each backend matches a different part.

**Why postgres serialises.** postgres and sqlite keep an extension log
(`model_schema_extensions`: `delta` and `savepoint` rows by `seq`) and fold it
on read: the latest savepoint, then the deltas with a higher `seq`
(`plugins/postgres/model_extensions.go` ~:47-100). Every
`CYODA_SCHEMA_SAVEPOINT_INTERVAL` (default 64) appends, the extending writer
also writes a savepoint, in the extender's transaction. Appends commute; the
savepoint does not: a savepoint written in a snapshot that cannot see a
concurrent delta with a lower `seq` leaves that delta out of every later fold.
59f51a30 fixed that loss by serialising every extender with the write claim.

On postgres the interval trigger compares the new `seq` with the last
savepoint's, but `seq` is one `BIGSERIAL` shared by all tenants and models, so
it does not count this model's deltas (~:550). sqlite numbers per model
(`MAX(seq)+1` per ref, `plugins/sqlite/model_store.go` ~:519).

**Model cache** (`internal/cluster/modelcache`). Caches LOCKED descriptors,
shared by all requests of the node, lease `CYODA_MODEL_CACHE_LEASE` (default
5 min). `ExtendSchema` evicts the entry, notifies local subscribers and
broadcasts `model.invalidate` the moment it returns, before the transaction
commits (`factory.go` ~:232-238, `cache.go` ~:313-323). A cache miss inside a
transaction reads through that transaction and stores the result in the
shared cache, so an uncommitted extension can be served to others and kept
after a rollback (`Get`, `factory.go` ~:130-144). `RefreshAndGet`
(`factory.go` ~:169-190, `cache.go` ~:160-178) evicts and reloads inside a
single-flight call whose result goes to every caller collapsed into it, and
criterion typing calls it with the transaction's context
(`criterion_eval.go` ~:223 → `path_validate.go` ~:241). A negative path cache
(`search.PathValidationCache`, `app/app.go` ~:452-453) is checked before the
model is read (`search/service.go` ~:1903-1905). A fill that read the model
before an invalidation can store after it, and the stale entry then lives for
the lease. `processor_output.go` ~:86-92
works around this by reading the descriptor once per pipeline.

**Reads that need the transaction's own extension.** Criterion typing
(`criterion_eval.go` ~:223-262, through `search.LoadFieldsMap` and
`ValidateKnownPaths` with one refresh) and a joined callback's search on a
field the request just added. Later items of a collection write re-derive the
delta if they do not see it (a duplicate, harmless).

**Tests.** No spitest case covers `ExtendSchema`. Postgres-only tests pin the
conflict (`plugins/postgres/model_extensions_concurrency_internal_test.go`
~:34, `internal/domain/entity/handler_extend_conflict_test.go` ~:22) and the
rollback (`plugins/postgres/model_extensions_test.go` ~:298-339,
`internal/e2e/workflow_proc_output_test.go` ~:135). Parity covers concurrent
creates (`e2e/parity/schema_concurrent_convergence.go`), which never
conflicted because create extends before `Begin`.

## 3. The contract (SPI)

`ModelStore.ExtendSchema(ctx, ref, delta)`:

1. **Requires a transaction in `ctx`.** Without one it returns an error and
   changes nothing. (After §4 no production caller extends outside a
   transaction; this removes every backend's self-wrapped extension path.)
2. **Belongs to the transaction.** `ModelStore.Get` with that transaction's
   `ctx` returns the schema with the delta applied. Other transactions, and
   reads without a transaction, see it only after the transaction commits.
   Rollback, and `RollbackToSavepoint` to a savepoint taken before the call,
   discard it.
3. **Never conflicts.** Neither `ExtendSchema` nor the transaction's commit
   fails because another transaction extends the same model, committed or
   not. (Other failures are unchanged: a model that does not exist, a delta
   `applyFunc` refuses, storage errors.)
4. **Never loses a committed delta.** After a commit, every later read of the
   model includes the delta.

The SPI doc of `ExtendSchema` is rewritten to this. `spi.ErrRetryExhausted`
stays (the cassandra plugin also uses it for model `Save` retries,
`cyoda-go-cassandra/internal/store/model_store.go` ~:148); its doc stops
naming schema extension.

5. **Names what it extended.** The plugin's transaction manager is the
   authority on which models a transaction extended: a new SPI method,
   `TransactionManager.ExtendedModels(ctx, txID) ([]ModelRef, error)`, returns
   them (empty for a transaction that extended none, or that has ended). Every
   backend tracks them anyway (staging, compaction); the model cache (§5) asks
   it, so the answer is right whichever node made the extension. A backend
   whose transactions are location-independent (durable coordination) answers
   for the transaction on any node.

**A model changed while an extending transaction is open.** `Unlock`, `Save`
and `Delete` of a model with writers in flight stay outside the operator
contract (`docs/CONSISTENCY.md` "Operator Contract for Unlock": drain first),
as today; the window is now the transaction rather than one call. No backend
installs a delta on a model that is gone: memory and sqlite fail the commit
(§6.1, §6.2). The commit of an entity into a model that is no longer `LOCKED`
is refused on every backend by #617, which follows this change.

## 4. Callers: every extension inside the entity transaction

- `CreateEntity` validates-or-extends after `Begin`, with the transaction's
  context.
- `CreateEntityCollection` keeps validating **every** item before any item
  runs: after `Begin`, a first loop validates-and-extends every item on the
  opening transaction's context (each item reads the model with that context,
  so it sees the earlier items' extensions, §3.2); then the existing execute
  loop runs. Extending item by item inside the execute loop would let item N
  fail after item 1's `COMMIT_BEFORE_DISPATCH` segment committed.
- `processor_output.go`: the `ErrProcessorOutputInfra` branches keep their
  cause with `%w` (~:60, ~:70, ~:112), so a model-store outage keeps its class
  (`503`) instead of becoming `500`. The `ErrProcessorOutputRejected` branch
  (~:72) keeps `%s`: with `%w` the `422` from `validate.go` would pass through
  `classifyWorkflowError`'s `errors.As` (`service.go` ~:2836) instead of
  becoming `400 WORKFLOW_FAILED`.
- `modelDescMemo` (`processor_output.go` ~:80-119) stays: one descriptor per
  pipeline is still correct, and its comment is rewritten to the reason that
  remains (a stale-narrow descriptor is harmless; re-reading is not needed).
  Whether to delete it is the builder's call, if every read then sees the
  transaction's own extension (§5).

## 5. Model cache

The caching layer (`CachingStoreFactory` / `requestScopedStore`,
`internal/cluster/modelcache`) asks the transaction manager which models a
transaction extended (§3.5). It keeps no per-transaction state of its own.

**Inside a transaction that extended `ref`** (ctx carries a transaction and
`ExtendedModels` includes `ref`):

- `Get`, `SchemaNode` and `RefreshAndGet` read the inner store directly with
  the transaction's context. They never read the shared entry, never lead or
  join the shared single-flight, and never fill the cache.
- The search path check skips the negative path cache, in both directions (it
  neither trusts an "absent" entry nor records one).

**Everywhere else** (no transaction, or a transaction that did not extend
`ref`) the cache serves and fills as today, from committed state:

- A fill from inside a transaction reads the inner store **without** the
  transaction (committed state), so a fill never carries uncommitted data.
- **Fill guard.** Each key has an invalidation generation. A fill records the
  generation before it reads and stores only if it is unchanged; an
  invalidation in between makes the fill return its result without storing it.
  The negative path cache follows the same rule. (This race exists today; the
  commit-bound promise makes it matter.)

**Invalidation at commit.** The caching factory's transaction manager wraps
the plugin's. Before the inner `Commit` it asks `ExtendedModels`; after the
inner `Commit` returns, **whatever its result** (a commit that reports a
network error may have committed; invalidating is always harmless), it evicts
each ref locally, notifies local subscribers and broadcasts `model.invalidate`.
`ExtendSchema` no longer invalidates. Rollback invalidates nothing.

Today the app takes its transaction manager from the plugin factory, not the
caching one (`app/app.go` ~:268-277); it takes it from the caching factory
instead, under the tracing wrapper. That is the only place a transaction
manager is obtained, so the entity service, engine segment commits, scheduled
fires (`scheduler/service.go` ~:967) and joined transactions' owners all go
through it.

## 6. Backends

### 6.1 memory

- `ExtendSchema` with a transaction stages the delta in a new per-transaction
  map on `TransactionManager` (`txSchemaDeltas`, txID → ref → ordered deltas,
  guarded by `m.mu`, removed in `forgetLocked`), under `tx.OpMu.RLock` with the
  closed / rolled-back checks the other staged operations use
  (`txmanager.go` ~:504), after checking that the model exists and `applyFunc`
  accepts the delta against the schema the transaction sees.
  `ExtendedModels` reads the map's refs.
- `Get` with a transaction returns the committed schema with the transaction's
  staged deltas applied, in order.
- `Savepoint` records the staged length per ref in `savepointSnapshot`;
  `RollbackToSavepoint` truncates to it.
- `Commit` captures the staged deltas in its step-3 `m.mu` section, as it
  captures the audit operations (`txmanager.go` ~:912); `m.mu` and `modelMu`
  are never nested. Before its last abort path it computes the new schemas
  under `modelMu` (taken inside `entityMu`, a new order; no path takes
  `modelMu` and then `entityMu`); a model that is gone, or a delta `applyFunc`
  refuses, aborts the commit (fail closed, nothing applied). After the last
  abort path it installs them. Additive deltas apply onto any schema that
  contains their base (§6.5), so a refusal means the model was replaced under
  the transaction.

### 6.2 sqlite

- `ExtendSchema` with a transaction stages the delta per transaction as in
  memory (same map, savepoint length, `forgetLocked`), after the same checks.
  The retry loop, `SchemaExtendMaxRetries` and
  `CYODA_SCHEMA_EXTEND_MAX_RETRIES` are deleted.
- `Get` with a transaction folds the committed log (`foldLockedOn` on the read
  connection) and applies the staged deltas.
- `flushToSQLite` checks, in its `sqlTx`, that each extended model still
  exists and that its fold accepts the staged deltas (a refusal fails the
  commit, nothing applied), then inserts the staged deltas into
  `model_schema_extensions`. sqlite commits one at a time (commit gate, one writer
  connection), so no other transaction's delta is in flight in the table and
  `seq` order is commit order: the flush may also write a savepoint, in the
  same `sqlTx`, when this model's deltas since its last savepoint reach the
  interval. sqlite's fold (latest savepoint, then deltas with a higher `seq`)
  and its savepoint on `Lock` stay as they are: with `seq` in commit order
  they lose nothing.

### 6.3 postgres

- `ExtendSchema` appends the delta row in the ambient transaction and takes no
  lock: the `UPDATE models SET doc = doc` claim is deleted, and no savepoint is
  written by an extending writer. The `applyFunc` pre-check against the
  transaction's fold stays. The self-wrapped path (`inWriteTx` for extension)
  is deleted with §3.1. The transaction's state records the refs it extended
  (`ExtendedModels`, and the compaction trigger below); a savepoint rollback
  may leave a ref recorded, which costs only an invalidation and a count.
- **Fold on read:** the model's one savepoint, if any, then **every** delta
  row of the model, in `seq` order, read in **one statement** (or one
  snapshot). Today the savepoint and the deltas are two statements
  (`model_extensions.go` ~:50-73); outside a `REPEATABLE READ` transaction a
  compaction committing between them would leave the old savepoint with the
  folded deltas already deleted. A delta already folded into the savepoint
  is applied again, which changes nothing (§6.5); a delta committed after the savepoint was written, whatever its
  `seq`, is applied.
- **Compaction** replaces the savepoint-in-the-writer: in a `READ COMMITTED`
  transaction of its own, take `SELECT … FOR UPDATE` on the model's `models`
  row (serialising compactions with each other and with `Unlock`, `Save` and
  `Delete`, which write that row; a lock without an update makes no other
  transaction fail, and no foreign key references `models`). Only after the
  lock: read the model's state, the savepoint and the deltas; skip if the row
  is gone or the model is `UNLOCKED`, and re-check the delta count (two nodes
  may both trigger). Then fold, write the new savepoint, and delete the old
  savepoint and exactly the delta rows it folded (by `seq`). A delta still
  uncommitted is not seen, not deleted, and folded by readers once it commits.
- **When compaction runs:** after a commit that extended the model, when the
  model has at least `CYODA_SCHEMA_SAVEPOINT_INTERVAL` delta rows. The
  transaction manager records the refs a transaction extended (per
  transaction, in its state; a savepoint rollback may leave a ref recorded,
  which only costs a count) and, after a successful commit, counts and
  compacts. It runs after the commit has returned its result internally and
  uses a pool connection of its own (the committed transaction's connection is
  released first). A compaction failure is logged at `WARN` and changes
  nothing: the commit stands, and reads stay correct (a longer fold).
- `Lock`'s savepoint write is deleted: the fold reads every delta whatever
  savepoint exists, so a savepoint written at `Lock` only duplicates what
  compaction writes.
- `Unlock` folds the log into the schema and deletes **exactly the rows it
  folded** (by `seq`), the same rule as compaction, instead of every row of the
  ref (`model_store.go` ~:269-286). Without the write claim, a delta committing
  between `Unlock`'s fold and its delete would otherwise be deleted unfolded; a
  late delta now stays in the log and readers fold it.
- `Save` and `Delete` keep clearing the log (they replace or remove the
  model); their write of the `models` row serialises them with compaction.

### 6.4 Savepoint interval

`CYODA_SCHEMA_SAVEPOINT_INTERVAL` counts **this model's** delta rows
(postgres: all its delta rows, since compaction deletes the folded ones;
sqlite: unchanged, since its last savepoint, already per model). Its help text ("reduce recovery time after a crash",
`cmd/cyoda/help/content/config/schema.md` ~:25-27) is rewritten: it bounds how
many deltas a read folds.

### 6.5 Re-applying and reordering deltas

The postgres fold re-applies deltas a savepoint already contains, and applies
a late delta after a savepoint that contains deltas diffed from later bases.
The property suites do not state this case: I4 (idempotence,
`idempotence_property_test.go` ~:13) re-applies onto the delta's own result;
I2 and I5 cover deltas diffed from one shared base
(`commutativity_property_test.go` ~:14, `fold_order_test.go` ~:9-19). The
argument that it holds: `Merge` is a set union (`merge.go` ~:9-14);
`AddScalarTypes` and `SetNullable` give the same result in either order; and
`resolvePath` (`apply.go` ~:189-222) fails only on a missing segment or kind,
which a schema containing the delta's base always has. A new property test
pins it: chained deltas d1…dn, each diffed from the fold of the ones before;
for every compaction point k, the fold of (fold of d1…dk) and then every delta
d1…dn re-applied in `seq` order equals the plain fold of d1…dn, and so does
any order of the deltas after k. If the test finds a counterexample, the
builder stops and brings it back.

## 7. Behaviour visible to clients

| Area | Before | After |
|---|---|---|
| Two transactions extend the same model (PUT, collection update, processor output), postgres | second gets `409 CONFLICT` (processor output: `500`) | both succeed |
| A write that extends a model rolls back (failed workflow, failed transition) | memory, sqlite, and postgres creates: extension kept; postgres in-transaction paths: discarded | discarded on every backend and path |
| Another client reads the model (`GET /model/...` export, search on a new field) while an extending write is in progress | memory, sqlite, postgres creates: sees the new field; postgres in-transaction paths: does not, except through the shared cache | does not; after the write commits, sees it on this node at once and on others when the invalidation broadcast arrives (export reads through the cache and is eventually consistent by design, `model/service.go` ~:119-121) |
| A criterion or joined-callback search in the extending request on the new field | works through the shared cache on most paths | works: the transaction's own reads see its extension |
| A storage failure during processor-output extension | `500` | `503` (retryable) |

No new error codes. No status code is added to any endpoint; `409 CONFLICT`
caused by a concurrent extension disappears.

## 8. Error / status table

| Endpoint | Status / code | Scenario |
|---|---|---|
| `POST /entity/...`, `PUT /entity/...`, collection create/update, transitions (processor output), HTTP and gRPC | `2xx` | a concurrent transaction extends the same model (was `409` on postgres for PUT, collection update, processor output) |
| | `400 VALIDATION_FAILED` / change-level refusals | unchanged |
| | `503` (retryable) | storage unavailable during extension, processor output included (was `500` there) |
| `GET /model/...` (export) and search | `200` | a rolled-back extension never appears; a committed one appears after commit |

## 9. Test coverage matrix

| Scenario | Unit / plugin | spitest | e2e (postgres) | Parity | gRPC |
|---|---|---|---|---|---|
| Extension visible in its transaction, not to another before commit, to all after | per-plugin | new | — | new | — |
| Rollback discards; savepoint rollback discards a later extension, keeps an earlier one | per-plugin | new | existing processor-output rollback (`workflow_proc_output_test.go` ~:135) | new (rollback through a failing workflow) | — |
| Two transactions extend the same model concurrently, both commit, both deltas in the schema | per-plugin | new | — | — | — |
| `ExtendSchema` without a transaction refused, nothing changed | per-plugin | new | — | — | — |
| Concurrent PUTs each adding a field → all `2xx`, schema has every field | — | — | new (isolated, not parity) | — | — |
| Concurrent processor-output extensions → all `2xx` | — | — | new (isolated) | — | — |
| Concurrent collection updates and collection creates each adding fields → all `2xx` | — | — | new (isolated) | — | — |
| Collection create: an invalid item N fails the batch before any item runs (validation first, §4) | entity service unit | — | new | — | — |
| Concurrent creates (existing) | — | — | existing (`model_schema_extensions_test.go` ~:24) | existing | — |
| Collection create/update: a later item sees an earlier item's extension, no duplicate delta | entity service unit | — | — | new | — |
| Criterion on a field the same request added | engine unit | — | new | new | — |
| Cache: an uncommitted extension is never stored in or served from the shared cache, through `Get`, `SchemaNode` or `RefreshAndGet` (including a single-flight led or joined by another request); invalidation (local + broadcast) after commit whatever its result, none at rollback; the fill guard drops a fill that straddles an invalidation | modelcache unit | — | — | — | — |
| A search in the extending transaction on its new field is not refused by a negative path-cache entry another request recorded | search unit | — | new (joined callback search) | — | — |
| `ExtendedModels`: names the extended refs, empty after commit/rollback | per-plugin | new | — | — | — |
| postgres `Unlock`: a delta committing between its fold and its delete survives | postgres plugin | — | — | — | — |
| Fold re-apply/reorder property (§6.5) | schema property test | — | — | — | — |
| memory/sqlite: a model replaced under an extending transaction fails its commit, nothing applied | per-plugin | — | — | — | — |
| postgres compaction: a delta committed after a compaction with a lower `seq` is in the fold; concurrent compactions lose nothing; compaction failure leaves reads correct | postgres plugin | — | — | — | — |
| Interval counts the model's own deltas | postgres + sqlite plugin | — | — | — | — |
| Processor-output storage failure → `503` | workflow unit (failing store double) | — | waived: a storage outage cannot be produced in e2e without a test hook in production code | — | waived, same reason |
| gRPC: concurrent extension through the gRPC entity door | — | — | — | — | new (envelope `Success=true`) |

Concurrency tests are isolated single-backend tests, never in the shared
parity suite. Each new test is shown failing on the unfixed code first; one
that cannot go RED is dropped.

## 10. Tests and code that change or go

- `plugins/postgres/model_extensions_concurrency_internal_test.go` ~:34
  (expects `ErrConflict`), `handler_extend_conflict_test.go` (the conflict
  path it pins no longer exists; delete or repoint at a genuine conflict),
  the `TestExtendSchema_SavepointEvery64` / interval / save-on-lock /
  `UnlockDoesNotWriteSavepoint` tests (rewritten to compaction), the sqlite
  retry tests (`TransparentRetry_*`, `RetryExhaustion_*`), and every
  `ExtendSchema` test that calls it without a transaction.
- Deleted: the postgres write claim and its comment, `inWriteTx` for
  extension, `Lock`'s savepoint write, the sqlite retry loop and config field,
  `CYODA_SCHEMA_EXTEND_MAX_RETRIES` (sqlite
  `config.go`, `plugin.go`, help `config/schema.md`, `docs/ARCHITECTURE.md`
  config table, `DefaultConfig` if listed, `README.md` if listed).
- Exit check: `grep -rn "SchemaExtendMaxRetries\|SCHEMA_EXTEND_MAX_RETRIES\|SET doc = doc"`
  over cyoda-go, the SPI and the plugins (`--include='*.go'` plus the help and
  docs trees) is empty.

## 11. Documentation

- `docs/CONSISTENCY.md` §3a: invariant 1 (no conflict for any extending or
  non-extending write), invariant 2 (commit-bound, every backend), the Unlock
  operator contract's postgres paragraph (no write claim; compaction and
  `Unlock` serialise on the `models` row).
- `docs/ARCHITECTURE.md` ~:331-339 (log, compaction), ~:944-949 (invalidation
  at commit), config tables (~:2567, ~:2582-2583).
- `cmd/cyoda/help/content/models.md` ~:343-345 (extensions are committed with
  the write, on every path, and never conflict).
- `cmd/cyoda/help/content/config/schema.md` (§6.4; remove the retry variable).
- SPI `persistence.go` `ExtendSchema` doc (§3), `errors.go`; spitest cases;
  SPI CHANGELOG. SPI PR into SPI `main`; cyoda-go and the three plugins
  pseudo-pin it.
- `CHANGELOG.md` `### Breaking` (SPI contract, removed variable),
  `COMPATIBILITY.md`.
- `docs/cloud-parity/710-schema-extensions-commit-bound.md`.
- Issue in `cyoda-go-cassandra`: the contract (§3, including
  `ExtendedModels` answered on any node under durable coordination), the
  spitest cases, and that its own-transaction fold with retries does not meet
  §3.2-§3.3. Until it follows, `ExtendSchema` no longer invalidates the cache
  for it at once.

## 12. Left to the builder

- Whether `modelDescMemo` stays (§4).
- The SQL of compaction and of counting a model's deltas.
- Whether postgres compaction runs inline after the commit or on a
  background goroutine, as long as it never runs on the committed
  transaction's connection and never fails the commit.
