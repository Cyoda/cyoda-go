# Schema extensions: never a conflict, never a slowdown (#710)

## 1. Intent

A model with a `changeLevel` grows as entity writes bring new fields: the write
computes an additive delta and calls `ModelStore.ExtendSchema`. High-volume
ingestion into such a model is a common case. Extensions are additive and
idempotent, so two writers extending the same model never have a real
conflict, and a client must never get one.

Today postgres gives a `409 CONFLICT` to the second of two transactions that
extend the same model, holds a lock on the model for the whole of an extending
entity transaction (workflow callouts included), and the backends disagree on
when an extension becomes visible.

### Requirements (the project lead's)

1. A concurrent extension never surfaces as an error to a client.
2. An extension is the initiator's own business until its call returns. It
   **must** be visible when the call returns to the initiator; it **may** be
   visible to others earlier. A simpler design that relies on this is
   preferred, with its side effects documented. Across nodes, the
   invalidation broadcast is sufficient: a node that does not receive it is
   partitioned or down, which cluster management handles.
3. The mechanism must not materially slow down transactional throughput.
4. No returned extension is ever lost, and the backends behave the same on
   everything the contract guarantees.

### Goals

- Every backend commits an extension on its own, before the entity write, and
  never fails a write because another write extends the same model.
- The steady state (data that fits the schema) does no extra work.
- A write that extends pays one short transaction, never a lock held for the
  length of another request.
- The model cache never serves a schema that storage does not hold. On the
  extending node it never serves one older than a returned extension; on
  other nodes, not after the invalidation broadcast has arrived (or, if a
  broadcast is lost, after the cache lease).
- All documentation and `cyoda help` describe the mechanism the same way, and
  correctly (§12).

### Non-goals

- Commit-bound extensions (visible only when the entity write commits, gone
  if it rolls back). Requirement 2 does not ask for it, and it costs work on
  every commit.
- The cassandra plugin (out of tree). An issue describes the contract change;
  it already extends in a transaction of its own.
- Any change to what an extension may add (`changeLevel` rules, `schema.Diff`,
  `schema.Apply`).

### Anti-goals

- A `409`, or any error, caused by another write's extension.
- A schema in a cache that storage does not hold, or a returned extension
  missing from a later read after the invalidation broadcast.
- Extra database work on writes whose data fits the schema.

## 2. Today

Verified against `release/v0.9.0` 9058cb92.

**Callers** (`ingest.ValidateOrExtend`, `internal/domain/model/ingest/validate.go`
~:77, calls `ExtendSchema` at ~:163; a nil delta returns before it, ~:157-160):

| Caller | Runs |
|---|---|
| `CreateEntity`, `internal/domain/entity/service.go` ~:257 | before `Begin` (~:275); the model is named in the path |
| `CreateEntityCollection`, ~:1971 | before `Begin` (~:2006) |
| `updateEntityCore` (PUT), ~:2238 | in the transaction: the request names only the entity id (`PUT /entity/{format}/{entityId}`), so the model is known once the entity is read (~:2194, ~:2207) |
| `UpdateEntityCollection`, ~:2534 | in the transaction (`Begin` ~:2466) |
| `applyProcessorData`, `internal/domain/workflow/processor_output.go` ~:68 | in the engine's transaction, mid-workflow |

**Backends.**

- **memory** (`plugins/memory/model_store.go` ~:112-133) applies the delta in
  place at once under `modelMu`, ignoring the transaction.
- **sqlite** (`plugins/sqlite/model_store.go` ~:444-571) appends to its
  extension log in a write transaction of its own (the ambient transaction id
  is only recorded, ~:436-439), with a retry loop on `SQLITE_BUSY`
  (`CYODA_SCHEMA_EXTEND_MAX_RETRIES`, default 8, then `spi.ErrRetryExhausted`).
- **postgres** (`plugins/postgres/model_store.go` ~:400-568) appends in the
  ambient transaction when there is one, otherwise in a self-wrapped `READ
  COMMITTED` one (`inWriteTx` ~:419-467). Its first statement,
  `UPDATE models SET doc = doc` (~:498-501), claims the `models` row: inside a
  `REPEATABLE READ` entity transaction the row lock is held until that
  transaction ends, and a second extender fails `40001`, answered `409
  CONFLICT` (`common.Internal`, `internal/common/errors.go` ~:283-284; from
  processor output `500`, because `processor_output.go` ~:70 drops the cause
  with `%s`). The claim exists because the extending writer also folds a
  savepoint every `CYODA_SCHEMA_SAVEPOINT_INTERVAL` appends, and a savepoint
  folded in a stale snapshot leaves out a concurrent lower-`seq` delta forever
  (59f51a30). `seq` is one `BIGSERIAL` for all models, so the interval does
  not count this model's deltas (~:550). Every delta is inserted, even one the
  schema already holds (~:519-540; sqlite the same, ~:502-523). A model read
  takes three statements (~:90-112, `model_extensions.go` ~:47-100), through
  the ambient transaction when there is one (`classifying_querier.go`
  ~:33-37). `Lock` is not in a transaction and decides "was unlocked" from a
  read before its write (~:185-232).
- **cassandra** (out of tree) extends in a transaction of its own and retries
  conflicts (`ExtendMaxRetries`, default 16).
- The SPI doc (`persistence.go` ~:558-579) says success means "durably
  committed and visible to subsequent reads on this node"; `docs/CONSISTENCY.md`
  §3a invariant 2 says extensions are commit-bound. They contradict each other.

**Model cache** (`internal/cluster/modelcache`). Caches LOCKED descriptors per
node, lease `CYODA_MODEL_CACHE_LEASE` (default 5 min), invalidated by
`ExtendSchema` on return (`factory.go` ~:232-238) and across the cluster by the
`model.invalidate` broadcast. A miss is not coalesced (`Get`, `factory.go`
~:130-146; only `RefreshAndGet` uses single-flight, ~:166-183). A miss inside a
transaction reads through it and stores the result in the shared cache, so an
uncommitted extension can be served to others and kept after a rollback
(~:144). A fill that read before an invalidation can store after it and keep
a stale entry for the lease. The negative path cache
(`search.PathValidationCache`, `app/app.go` ~:452-453) answers `400` before
the model is read (`search/service.go` ~:1903-1905); it has no lease and no
generation (`path_validation_cache.go` ~:235-237), and the bounded refresh
covers only unknown paths (`path_validate.go` ~:314-357).

## 3. The contract (SPI)

`ModelStore.ExtendSchema(ctx, ref, level, delta) error`, where `level` is the
`changeLevel` the caller computed the delta under (a new parameter):

1. **Commits on its own.** It never joins a transaction on `ctx`. When it
   returns nil the delta is durably committed, whatever then happens to the
   caller's transaction.
2. **Visible on return.** After a nil return, every later `Get` on any node of
   the store returns a schema that includes the delta. It may be visible to
   others before the return.
3. **Idempotent.** A delta the schema already holds is a successful no-op: no
   log growth, no change.
4. **Never a conflict.** Contention with other extenders or with entity
   transactions never produces an error: the backend waits (or retries) until
   `ctx` ends. It never returns `spi.ErrConflict` or `spi.ErrRetryExhausted`
   for contention. (A store saturated by load still answers its existing
   retryable unavailable error, as any store operation does.)
5. **Loss-free.** The folded schema includes every delta whose call returned
   nil.
6. **Errors:** `spi.ErrNotFound` (no model); a new `spi.ErrModelNotLocked`
   when the model is not `LOCKED` under the backend's lock (`409
   MODEL_NOT_LOCKED`, an existing code; a model-lifecycle conflict, not an
   extension conflict); `spi.ErrConflict` when the model's `changeLevel` under
   the lock is not `level` (an operator changed it after the caller read the
   model; retryable `409 CONFLICT`, and the retry validates under the new
   level); a delta `applyFunc` refuses; storage unavailable; a `ctx` error. A non-nil error means the outcome is unknown; a retry is safe
   (§3.3).

`ModelStore.Get` reads the latest committed model and ignores any transaction
on `ctx`, on every backend.

The SPI doc of `ExtendSchema` and `Get` is rewritten to this; spitest gains
conformance cases (§10). `spi.ErrRetryExhausted` stays (cassandra uses it for
model `Save` retries); its doc stops naming schema extension.

## 4. Design

1. **Every caller extends through a transaction of the extension's own,
   before its entity write commits.** The call sites stay where they are
   (§2). Inside an entity transaction, `ExtendSchema` uses neither the
   transaction's connection nor its snapshot.
2. **postgres: one short `READ COMMITTED` transaction per extension.**
   `BEGIN`, `set_config` (tenant), `SELECT … FROM models … FOR NO KEY UPDATE`
   (the per-model lock: it writes no row version, conflicts with the
   `UPDATE models` of `Lock`, `Unlock`, `SetChangeLevel` and `Save` and with
   `Delete`, and serialises extenders of one model), the `LOCKED` and
   `changeLevel` checks (§3.6), the fold (one statement, §4.8), the no-op
   check and the insert of the reduced delta (§4.3), the savepoint when due,
   `COMMIT`. A transaction that holds a `models` row lock never acquires a
   second connection: every statement of an extension, and of `Lock`,
   `Unlock`, `SetChangeLevel`, `Save` and `Delete` (all on the model pool,
   §4.4), runs on its own transaction's connection, `Lock`'s fold included.
   An extension transaction that meets a classified `spi.ErrConflict`
   (`40001`, `40P01`) retries itself rather than returning it (§3.4). The lock is held only for this transaction,
   never for an entity transaction, so extenders of one model wait a few round
   trips at most and never fail. Every statement after the lock sees every
   earlier extension's commit, so `seq` order is commit order and a savepoint
   folded under the lock holds every lower-`seq` delta: the fold stays
   loss-free without serialising entity transactions. The ambient-transaction
   path and the `UPDATE models SET doc = doc` claim are deleted.
3. **A delta the schema already holds writes nothing.** Under the lock, the
   backend applies the delta to the current fold and asks `schema.Diff`
   (`internal/domain/model/schema/diff.go` ~:17) for the difference, which is
   nil for a semantic no-op. Nil: no row, success. Otherwise the
   row holds that difference (the reduced delta), not the caller's delta. N
   writers adding the same field write one row.
4. **postgres: a dedicated pool for the model store.** Model reads and
   extensions use a small pool of their own, as the scheduler does
   (`plugins/postgres/scheduler_pool.go` ~:89-93,
   `CYODA_POSTGRES_SCHEDULER_CONNS`). A PUT, collection update or
   processor-output request already holds a main-pool connection for its
   transaction; taking the extension's connection from the same pool would let
   requests that each hold one and wait for another block one another until
   the acquire timeout. A model-pool holder never waits on the main pool or on
   entity rows, so there is no cycle. New variable
   `CYODA_POSTGRES_MODEL_CONNS` (default `8`, at least `2`; a smaller or
   malformed value fails startup). Cache misses (lease expiry, the read after
   an invalidation) now run on this pool rather than on the request's
   transaction or the main pool; they are coalesced per node (§4.7), so the
   pool carries roughly one read per model per invalidation. Model administration (`Lock`, `Unlock`,
   `SetChangeLevel`, `Save`, `Delete`) uses it too.
   - **Extenders queue before they take a connection.** On each node,
     extenders of one (tenant, model) take turns in-process; only the one whose
     turn it is takes a connection, so waiting for the row lock never holds
     more than one connection per model per node. At most half the pool
     (`CONNS / 2`, at least `1`) is held by extensions at once, so model reads
     always have the rest. Both waits (for the model's turn, and for a
     connection once it holds an extension slot) last until `ctx` ends
     (§3.4), never until the pool's acquire timeout: reads and model
     administration hold connections only briefly, so a connection frees.
   - Reads beyond the pool's capacity under load wait for the acquire timeout
     and answer the pool's existing retryable `503`, as every store read does.
5. **Identical deltas on one node share one call.** The caching layer
   coalesces concurrent `ExtendSchema` calls with the same tenant, model,
   `level` and delta into one call (single-flight); every caller gets its
   result.
   Different deltas are not merged: each is its own short transaction. The
   shared call runs on a context detached from any one caller's cancellation
   (`context.WithoutCancel`); each caller waits for it on its own context
   (`DoChan`), so one caller giving up never fails the others. The shared call
   is cancelled when its last waiter has left, so a call no one waits for
   does not hold the model's turn (§4.4) forever.
6. **`ModelStore.Get` ignores the entity transaction on every backend.** It
   reads committed state (postgres: on the model pool). So a workflow
   criterion later in the same request sees the field the request just added,
   and a cache fill can never carry uncommitted data. A transaction that reads
   a schema newer than its snapshot is safe: a `LOCKED` model's schema only
   grows.
7. **The model cache.**
   - **Fill guard.** Each key has an invalidation generation, bumped by every
     local invalidation and every one received by broadcast
     (`cache.go` ~:325-331), and kept when the entry is evicted (`evict`
     deletes the entry, ~:307-311). A fill records it before reading and stores
     only if it is unchanged.
   - **Misses are coalesced, per generation.** `Get`, `SchemaNode` and
     `RefreshAndGet` misses go through single-flight keyed by (key,
     generation): a read that starts after an invalidation never joins one
     that started before it (today the key is the key alone,
     `cache.go` ~:369-371). An invalidation of a hot model costs one read per
     node. The shared read runs detached from any one caller's cancellation, as
     in §4.5.
   - **After an extension call, whatever its outcome** (success, no-op or
     error): the caller's delta was non-nil, so its node had a cache entry
     without it, and so may other nodes (the broadcast of whoever committed
     the delta may be lost, or that node may have crashed before sending it).
     The cache evicts the entry locally, bumps its generation and broadcasts
     `model.invalidate`, every time. That costs one broadcast per node per
     burst of writes carrying a new field, since a non-nil delta comes only
     from a stale cache. This runs **inside the shared call** (§4.5), after
     the inner call ends, whether or not any caller still waits, so a call
     whose callers all left cannot commit without a broadcast. So the
     extending node never keeps serving the old schema, a retry after `409
     CONFLICT` or `409 MODEL_NOT_LOCKED` reads the model fresh, and a lost
     broadcast is repaired by the next node that meets the stale schema.
   - **A refresh bumps the generation before it reads.** `RefreshAndGet`
     evicts and bumps (today it only evicts, `factory.go` ~:171), so a
     refresh never joins a flight that started before it.
   - `CachingModelStore` stops being a `ModelStore`: it is built in
     production only with a nil inner (`factory.go` ~:38) and its store
     methods serve tests. Its `Get` / `RefreshAndGet` / `SchemaNode` /
     `ExtendSchema` (`cache.go` ~:141-229, `SchemaNode` ~:292-305), the pass-throughs (~:183-235), the
     `inner` field, `New`'s `inner` parameter and the `var _ spi.ModelStore`
     assertion (~:374) go, leaving the cache state.
8. **A model is read in one snapshot.** postgres: one statement (base
   `models.doc`, the latest savepoint, the later deltas); today three. sqlite:
   one read transaction (today `Get` reads the doc and then the log in
   separate statements, `sqlite/model_store.go` ~:99-121,
   `model_extensions.go` ~:23-24). A read that races an `Unlock` (which
   rewrites `doc` and clears the log) could otherwise combine the old `doc`
   with the cleared log.
9. **The negative path cache is deleted.** It exists to absorb floods of
   invalid requests; cyoda-go adds no such bounds. With no lease and no
   generation it can answer `400 INVALID_FIELD_PATH` for a field that exists,
   on a node that has not yet received the invalidation, which breaks
   requirement 2 for the initiator.
9a. **Every read that answers a client from a cached schema either refreshes
   once or reads storage.** A node that has not yet received an invalidation
   holds an older schema. So:
   - **Refusals refresh once.** A refusal on schema grounds re-reads the model
     once (`RefreshAndGet`) and decides again: search and criterion path
     checks (`INVALID_FIELD_PATH`), type checks on a known path
     (`CONDITION_TYPE_MISMATCH`, `condition_type_validate.go` ~:367-370; a
     field whose type another write widened), PATCH's strict validation
     (`service.go` ~:2233-2236, `400 VALIDATION_FAILED`) and
     `ValidateModel` (`model/service.go` ~:513). The three copies of the
     refresh today (`ValidateKnownPaths`, `validateConditionPaths`
     `service.go` ~:1883-1993, the sort-key path ~:2097-2180) become one
     helper; `ValidateWithRefresh` (`handler.go` ~:162-202, today without a
     production caller, and a `Handler` method while PATCH validates in the
     service; it moves) is that helper for validation, and it refreshes on
     **any** schema refusal, a type mismatch included: today it refreshes only
     on unknown elements (`handler.go` ~:184), and
     `TestValidateWithRefresh_TypeMismatch_NoRefresh`
     (`handler_validate_refresh_test.go` ~:158) pins that; the test is
     rewritten. The other copies go
     into the same helper: `EnsureModelRegistered`
     (`internal/common/model_registration.go` ~:30-52; its comment ~:16-17
     claims a negative cache that does not exist), `getModelFresh`
     (`model/service.go` ~:122-131) and `refreshFieldsMap`
     (`path_validate.go` ~:234-266).
   - A refusal refresh evicts the shared entry, so a client that keeps sending
     a bad path costs one coalesced model read per refusal. That replaces the
     negative cache's protection; cyoda-go adds no flood bounds.
   - **Export reads storage.** `ExportModel` (`model/service.go` ~:243) gives
     no refusal to refresh on, so it reads the model from storage, not the
     cache. `getModelFresh`'s comment (~:115-119), which says export and
     validation keep the cache, is rewritten.
10. **`ValidateOrExtend` returns the extended descriptor.** The processor
    pipeline's memo (`processor_output.go` ~:80-119) adopts it, so a later
    processor returning the same field does not extend again; the memo's
    comment about a cache "flap" (~:86-91) is rewritten (the flap cannot
    happen any more).
11. **processor output keeps the cause of an infrastructure failure**: the
    `ErrProcessorOutputInfra` branches use `%w` (~:60, ~:70, ~:112), so a
    storage outage is `503`, not `500`. The `ErrProcessorOutputRejected`
    branch (~:72) keeps `%s`: with `%w` the `422` from `validate.go` would pass
    through `classifyWorkflowError`'s `errors.As` (`service.go` ~:2836)
    instead of becoming `400 WORKFLOW_FAILED`.
12. **The savepoint interval counts this model's deltas** (postgres: deltas
    since its latest savepoint; sqlite numbers per model already).
13. **memory and sqlite.** memory keeps applying in place under `modelMu` and
    adds the `LOCKED`, `changeLevel` and no-op checks; its `Lock`, `Unlock`,
    `SetChangeLevel` and `IsLocked` return `spi.ErrNotFound` for a missing
    model, as the other backends do (today a bare "not found",
    `plugins/memory/model_store.go` ~:71, ~:82, ~:93, ~:103). sqlite keeps its own write
    transaction, adds the same checks, and retries `SQLITE_BUSY` until `ctx` ends
    instead of within a budget: `SchemaExtendMaxRetries` and
    `CYODA_SCHEMA_EXTEND_MAX_RETRIES` are deleted. Both already read the model
    ignoring the entity transaction.
14. **postgres `Lock`** moves into one `READ COMMITTED` transaction and
    decides "was unlocked" from its own conditional update, so its savepoint is
    written only on the real `UNLOCKED → LOCKED` edge.
15. **Error mapping.** `ValidateOrExtend` tags store errors with
    `ErrInternalSchema` (`validate.go` ~:164) and the doors send that to
    `common.Internal` (`handler.go` ~:271; processor output through
    `classifyWorkflowError`, `service.go` ~:2859). `common.Internal`
    (`internal/common/errors.go` ~:266-300) maps `spi.ErrModelNotLocked` to
    `409 MODEL_NOT_LOCKED` before its generic case, so every door answers it
    without a branch of its own; `spi.ErrConflict` already maps to a retryable
    `409 CONFLICT`.
16. **Model administration overlapping an extension.** `SetUniqueKeys` and
    import read the model and then `Save` it whole (`model/service.go`
    ~:631-669, `postgres/model_store.go` ~:67-79); a `Lock` and an extension
    landing between the read and the `Save` would be overwritten. That is two
    administration calls overlapping on one model, which the help already puts
    outside the contract (model administration runs from one place,
    `cmd/cyoda/help/content/models.md` ~:341). #617 makes `Save` refuse a
    `LOCKED` model at write time.

## 5. Side effects (documented, §12)

1. A new field can be visible to other clients (model export, search path
   validation, criteria) before the write that brought it commits. A query on
   it finds nothing from that write until it commits.
2. If the write fails after the extension (a workflow or processor error, a
   unique-key violation, a conflict, the client cancelling, a crash), the model
   keeps the field. It can be removed only by unlocking (which needs zero
   entities) and re-importing the model.
3. An extension made inside a joined compute-node callback stays even if the
   owner's transaction rolls back.
4. Writers that bring different types for one field leave the model admitting
   both.
5. A failed request may still have extended the model; retrying it is safe.
6. Once a write that extends has returned `2xx`, the node that served it has
   the new schema. Another node has it when the invalidation broadcast
   arrives (normally milliseconds; if a broadcast is lost, when the cache
   lease, `CYODA_MODEL_CACHE_LEASE`, ends). Until then, on that node:
   anything that would refuse on schema grounds refreshes once first, so the
   new field is not refused, and export reads storage (§4.9a); but a query
   that does not refuse is planned against the older schema and can silently
   miss the new field or a widened type (e.g. `code = 42` after `code` was
   widened from string to string-or-integer finds no integer match there).

## 6. Performance

Round trip = one request-response with the database.

| Case | Today (postgres) | After |
|---|---|---|
| Data fits the schema (steady state) | cached read, CPU diff, nil delta; no database work | the same; nothing new on the write or commit path |
| A write that extends | create: own transaction (~8 round trips); PUT / collection update / processor output: lock on the model row held for the whole entity transaction, callouts included; other extenders wait or get `409` | one short transaction on the model pool (~3 round trips with batching, one disk flush), the per-model lock held only for it; the entity transaction is longer by that, only for this write |
| Burst of N writes adding the same field | in-transaction: 1 winner, N−1 `409`s; self-wrapped: N queued behind a whole-transaction lock; N log rows | one call per node (§4.5); across M nodes, M short transactions in turn, 1 row, 1 invalidation, then one read per node (§4.7) |
| Many writers extending many models | serialised per model for whole transactions | parallel, up to half of `CYODA_POSTGRES_MODEL_CONNS` per node; no lock across models |
| A cache miss (lease expiry, after an invalidation) | read through the request's transaction or the main pool, not coalesced | one coalesced read per model per node, on the model pool |
| Every savepoint interval | savepoint folded inside an entity transaction | savepoint folded in the short transaction under the lock |

Where it can still slow down: a model whose field names are always new (an
object keyed by ids) extends on every write with a different delta; writes to
that model serialise on short transactions and its log grows. That is the
model's nature; this design makes each step short. And a steady stream of
requests on paths a model does not have makes every refusal refresh that
model (§4.9a), so valid writes on it then miss the cache and read storage;
cyoda-go adds no flood bounds.

Measurement: the builder adds a benchmark of concurrent PUTs on one model on
postgres, (a) all fitting the schema, (b) each adding a different field, (c)
all adding the same field, run before and after; the PR reports the numbers.
(a) must not get slower.

## 7. Behaviour visible to clients

| Area | Before | After |
|---|---|---|
| Two writes extend the same model (PUT, collection update, processor output), postgres | second `409 CONFLICT` (processor output `500`) | both succeed |
| An extending write fails or rolls back | memory, sqlite, postgres creates: field kept; postgres in-transaction paths: discarded | field kept on every backend and path (§5.2) |
| Another client reads the model while an extending write is in progress | depends on backend and path | may see the new field (§5.1) |
| The initiator searches on, PATCHes, validates or exports the new field right after its write returned, on another node, before the broadcast arrives | search: `400 INVALID_FIELD_PATH` possible (negative cache, no lease); PATCH, validate: `400 VALIDATION_FAILED`; export: field missing | not refused (one refresh), export from storage; a query that does not refuse may still miss the new field or a widened type until the broadcast arrives (§5.6) |
| An operator changes `changeLevel` while a write extends under the old level | postgres: conflicts with the write's lock | the extension is refused with a retryable `409 CONFLICT`; the retry validates under the new level |
| A criterion in the same request on the field just added | works through the shared cache on most paths | works on every path (§4.6) |
| A storage failure during processor-output extension | `500` | `503` (retryable) |
| Extension after the model was unlocked mid-request | postgres: appended to the unlocked model's log | `409 MODEL_NOT_LOCKED` on every backend |

## 8. Error / status table

| Endpoint | Status / code | Scenario |
|---|---|---|
| Entity create, update (PUT), collection create/update, transitions (processor output); HTTP and gRPC | `2xx` | a concurrent write extends the same model (was `409` on postgres for PUT, collection update, processor output) |
| | `409 MODEL_NOT_LOCKED` | the model was unlocked after the request read it, before its extension |
| | `400 VALIDATION_FAILED` / change-level refusals | unchanged |
| | `503` (retryable) | storage unavailable during an extension (processor output was `500`) |
| | `409 CONFLICT` (retryable) | the model's `changeLevel` changed between the request's read and its extension |
| Search, criteria (`INVALID_FIELD_PATH`, `CONDITION_TYPE_MISMATCH`), PATCH and model validation (`VALIDATION_FAILED`) | `2xx` instead of `400` | a field another node just added or widened, before the broadcast arrives |
| `GET /model/...` export | `200` with the field | export reads storage |

## 9. What the design deletes

The postgres write claim and its comment; the postgres ambient-transaction
extension path; `CachingModelStore`'s read/extend methods; the negative path
cache (`search.PathValidationCache`, its wiring at `app/app.go` ~:452-453,
`WithPathValidationCache`, `markPathsAbsent`, `markPathsPresent`,
`cachedAbsentPaths`, `pathSurface` / `surfaceSort`, `search/service.go`
~:497-504, ~:2020-2080, and their tests, `search/export_test.go` and
`modelcache/local_subscriber_test.go` among them), and with it the cache's local
subscribers it was the only user of (`SubscribeLocal`,
`notifyLocalSubscribers`, `localSubs`, `cache.go` ~:46-57, ~:334-360,
`factory.go` ~:108-115); the two copies of the refresh that §4.9a merges; the sqlite retry budget
(`SchemaExtendMaxRetries`, `CYODA_SCHEMA_EXTEND_MAX_RETRIES` in sqlite
`config.go`, `plugin.go`, help, `docs/ARCHITECTURE.md`, `DefaultConfig` and
`README.md` where listed); the postgres-only conflict tests
(`model_extensions_concurrency_internal_test.go` ~:34 expecting `ErrConflict`,
`handler_extend_conflict_test.go`); and `plugins/postgres/classified_querier_internal_test.go`
~:45 uses `UPDATE models SET doc = doc` as a sample statement, with an
`ExtendSchema` doc comment: it takes another statement. The tests that assert an extension rolls
back with its transaction (`plugins/postgres/model_extensions_test.go`
~:298-339, `internal/e2e/workflow_proc_output_test.go` ~:135
`TestWorkflowProcOutput_ExtensionRollsBackWithTheTransition`) are rewritten
to the new rule (the field is kept). Exit check:
`grep -rn "SchemaExtendMaxRetries\|SCHEMA_EXTEND_MAX_RETRIES\|SET doc = doc\|PathValidationCache\|markPathsAbsent\|markPathsPresent\|SubscribeLocal\|notifyLocalSubscribers"`
over cyoda-go, the SPI and the three in-tree plugins (`--include='*.go'`, plus `docs/`
outside `docs/superpowers/`, `cmd/cyoda/help/`, `README.md`) is empty.

## 10. Test coverage matrix

| Scenario | Unit / plugin | spitest | e2e (postgres) | Parity | gRPC |
|---|---|---|---|---|---|
| Extension visible to `Get` on return; kept after the caller's transaction rolls back | per-plugin | new | rewritten processor-output test (field kept) | new | — |
| A delta the schema already holds: no row, no invalidation | per-plugin | new | — | — | — |
| Two extenders of one model concurrently: both succeed, both deltas in the schema, never `ErrConflict` | per-plugin | new | — | — | — |
| Extension of a model that is not `LOCKED` → `ErrModelNotLocked` | per-plugin | new | new: a create whose processor returns a new field, with the test compute node unlocking the model during the callout (the entity is uncommitted, so the unlock succeeds); `409 MODEL_NOT_LOCKED` | — | new, same setup over gRPC (envelope `Error.Code`) |
| `changeLevel` changed between read and extension → `ErrConflict` | per-plugin | new | coverage waiver: the e2e (a test compute node changing `changeLevel` during the callout) would pass on today's postgres too (its write claim answers `409` already), so it cannot prove the change; the spitest and plugin cases do | — | coverage waiver, same reason |
| `Get` ignores the transaction on `ctx` | per-plugin | new | — | — | — |
| Concurrent PUTs, collection updates, collection creates and processor-output writes each adding fields → all `2xx`, every field in the schema | — | — | extend `model_schema_extensions_test.go` ~:36-75 (today creates only) to every door (isolated, not parity) | — | new (concurrent gRPC writes, envelope `Success=true`) |
| A write adds a field nested under a parent another write added after this transaction began → `2xx` | postgres plugin | — | new | — | — |
| Criterion in the same request on the field just added | engine unit | — | new | new | — |
| On a node whose cache is older than the model (broadcast not yet received): search, PATCH and model validation on the new field, or on a widened type, are not refused; export has the field | search, entity and model service units (a cache entry seeded older than storage; no production hook) | — | — | — | — |
| Multi-node: an extension on node A is visible on node B after the broadcast | — | — | — | new, multi-node harness (`e2e/parity/postgres/multinode_test.go`) | — |
| Type mismatch on a known path (`CONDITION_TYPE_MISMATCH`) refreshes once | search unit | — | — | — | — |
| Refresh-on-refusal through the gRPC search door | — | — | — | — | new |
| A refresh never joins a flight started before it; an extension error still evicts and broadcasts | modelcache unit | — | — | — | — |
| Fill guard drops a fill that straddles an invalidation (local and broadcast, also after eviction); a read starting after an invalidation never joins a flight started before it; one caller's cancellation does not fail the others | modelcache unit | — | — | — | — |
| Identical concurrent `ExtendSchema` calls on one node → one storage call; a non-nil delta that turns out a no-op still evicts locally, without a broadcast | modelcache unit | — | — | — | — |
| postgres: a savepoint under the lock keeps every lower-`seq` delta; the interval counts the model's deltas; a one-statement read during `Unlock` misses nothing | postgres plugin | — | — | — | — |
| postgres: extenders of one model queue in-process (one connection per model per node); at most `CONNS / 2` extension connections; model reads proceed during an extension burst; `Lock`'s fold takes no second connection; no deadlock with the main pool | postgres plugin | — | — | — | — |
| sqlite: a read racing `Unlock` misses nothing (one read transaction) | sqlite plugin | — | — | — | — |
| memory: `Lock`/`Unlock`/`SetChangeLevel`/`IsLocked` on a missing model → `spi.ErrNotFound` | memory plugin | existing model cases extended | — | — | — |
| sqlite: `SQLITE_BUSY` retried until `ctx` ends | sqlite plugin | — | — | — | — |
| postgres `Lock` decides on its own update | postgres plugin | — | — | — | — |
| Processor-output storage failure → `503` | workflow unit (failing store double) | — | waived: a storage outage cannot be produced in e2e without a test hook in production code | — | waived, same reason |
| Throughput benchmark (§6) | benchmark | — | — | — | — |

Concurrency tests are isolated single-backend tests, never in the shared
parity suite. Each new test is shown failing on the unfixed code first; one
that cannot go RED is dropped.

## 11. SPI and cross-repo

- SPI: `persistence.go` (`ExtendSchema`, `Get`, §3), `errors.go`
  (`ErrModelNotLocked` new; `ErrRetryExhausted` doc), spitest cases (§10), SPI
  CHANGELOG. SPI PR into SPI `main`; cyoda-go and the three plugins pseudo-pin
  it (`make repin-plugins`).
- `CHANGELOG.md` `### Breaking` (contract, removed variable, removed negative
  path cache, new variable), `COMPATIBILITY.md`.
- `docs/cloud-parity/710-schema-extensions.md`: the contract and the side
  effects.
- Issue in `cyoda-go-cassandra`: §3 (the `level` parameter; no
  `ErrRetryExhausted` for contention, retry until `ctx` ends, so its
  `CYODA_SCHEMA_EXTEND_MAX_RETRIES` budget, `internal/config/config.go`
  ~:242, goes; the no-op check; `ErrModelNotLocked`; `Get` ignores the
  transaction).

## 12. Documentation: one description, everywhere

The builder audits **every** place that describes schema extension, the model
cache, field-path validation or the variables this change touches, reads each
document in full (not only the matching lines), and makes them say the same
thing: §3's contract, §5's side effects, and §4's mechanics at the level each
document works at. Present tense, no history. Sites found by
`grep -rniE "extendschema|schema extension|extension log|savepoint|SCHEMA_EXTEND|changeLevel|change level|extends? the (model|schema)|model extension|commit-bound|path validation|path-validation|MODEL_CACHE_LEASE|RefreshAndGet|POSTGRES_.*CONNS|connection pool|pools?\b|model cache|invalidat"`
over `README.md`, `CONTRIBUTING.md`, `COMPATIBILITY.md`, `docs/` (outside
`docs/superpowers/`, `docs/cloud-parity/` included), `cmd/cyoda/help/content/`,
`api/openapi.yaml` and the `plugins/*/doc.go` and `plugin.go` files:

- `cmd/cyoda/help/content/models.md` (~:77, ~:98, ~:334, ~:341 the
  model-administration paragraph, ~:343-345, the ERRORS list ~:348-361 which
  gains `MODEL_NOT_LOCKED`): when an extension happens, that it never
  conflicts, that it is visible when the write returns and may be earlier,
  the side effects, the `409 MODEL_NOT_LOCKED` and `changeLevel` cases.
- `cmd/cyoda/help/content/config/schema.md`: what the savepoint interval bounds
  (the deltas a read folds, per model; the current "reduce recovery time after
  a crash" is wrong), the retry variable removed, the examples.
- `cmd/cyoda/help/content/config/database.md`: `CYODA_POSTGRES_MODEL_CONNS`.
- `cmd/cyoda/help/content/config.md`, `cmd/cyoda/help/content/cli/serve.md`:
  variable lists.
- `cmd/cyoda/help/content/workflows.md` (~:149): processor output extends like
  a client write, with the same side effects.
- `cmd/cyoda/help/content/crud.md`, `grpc.md`, `openapi.md`, `errors.md`: any
  extension or field-path wording.
- `cmd/cyoda/help/content/errors/`: `INVALID_FIELD_PATH.md` (~:51: the refresh,
  now also for type mismatches; no negative cache), `MODEL_NOT_LOCKED.md` (the
  extension case), `CONFLICT.md` (never from extensions),
  `INVALID_CHANGE_LEVEL.md`, `INCOMPATIBLE_TYPE.md`, `VALIDATION_FAILED.md`,
  `MODEL_ADMIN_IN_JOINED_TRANSACTION.md`, `CONDITION_TYPE_MISMATCH.md` (the
  refresh), `STORAGE_UNAVAILABLE.md` (processor output's `503`).
- `docs/CONSISTENCY.md` §3a: the five invariants rewritten to §3 (invariant 1:
  no conflict; invariant 2: visible on return, may be earlier, kept on
  failure) and the Unlock operator contract's postgres paragraph.
- `docs/ARCHITECTURE.md`: the extension log and savepoints (~:331-339), the
  model cache and invalidation (~:944-949), the negative path cache, the
  postgres pools, the config tables (~:2515, ~:2567, ~:2582-2583).
- `docs/FEATURES.md`, `docs/PRD.md`, `docs/plugins/*.md`: extension wording;
  `docs/plugins/POSTGRES.md` (~:220, ~:503) the pools and variables.
- `cmd/cyoda/help/content/cluster.md` (~:185, model cache invalidation) and
  `cmd/cyoda/help/content/quickstart.md` (~:69-70, postgres connection
  defaults).
- `docs/CONCURRENCY.md` (~:61, ~:108): the lock inventory (the path-validation
  cache goes; the model pool and the per-model extender queue come).
- `internal/common/model_registration.go` ~:16-17 and `model/service.go`
  ~:115-119 comments (§4.9a).
- `docs/cloud-parity/model-administration-not-joined.md` (~:36-39: says an
  extension is "bound to the entity's commit", contradicting §3) and
  `docs/cloud-parity/nested-join-tx-serialisation.md` (~:20).
- `plugins/postgres/plugin.go` (variable list), `plugins/postgres/doc.go`
  (~:17, ~:70), `docs/ARCHITECTURE.md` ~:2972 (connection count: main +
  scheduler + heartbeat, now + model pool).
- `api/openapi.yaml`: the `changeLevel` operation (~:5344-5470) and every
  response description that mentions extension, `409` or `INVALID_FIELD_PATH`;
  then `go generate ./api`.
- `README.md`: today it mentions none of the extension terms (its one pool
  match, ~:229, is the async-search worker pool); checked again after the
  change, and the configuration reference gains the new variable if it lists
  the postgres pools.
- SPI docs (§11) and the `docs/cloud-parity/` entry.
- Code comments that describe the old mechanics: `validate.go` ~:161-162
  ("participates in the ambient entity transaction so visibility is
  commit-bound"), `processor_output.go` ~:86-91, the postgres `Lock`, `Unlock`
  and `extendSchemaBody` comments, `handler.go`'s `validateOrExtend` comment,
  the modelcache package comment.

Exit check: the grep, re-run after the change, finds no sentence that
contradicts §3 or §5 (the PR lists each site and what changed), and a fresh
reviewer reads `cyoda help models`, `cyoda help config schema` and
`docs/CONSISTENCY.md` §3a end to end against §3 and §5.

## 13. Left to the builder

- The SQL of the one-statement read and of the savepoint trigger.
- How the no-op check is computed (apply then `schema.Diff`, or an equivalent
  the property tests cover).
- The coalescing key's exact form (§4.5).
- Whether `ValidateOrExtend` validates against the cache's parsed node
  instead of re-parsing the schema on every write (`validate.go` ~:85), once
  it has confirmed that admission never mutates the node: a free saving on the
  steady state.
