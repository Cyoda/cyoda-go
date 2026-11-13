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
   preferred, with its side effects documented. "The node that served the
   write" is the node that executed it: for a write carrying a transaction
   token, forwarded to the transaction's owner (`internal/cluster/proxy`),
   that is the owner, not the node the client called. Across nodes, the
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
- The model cache never serves an uncommitted extension. On the extending
  node it never serves a schema older than a returned extension; on other
  nodes, not after the invalidation broadcast has arrived (or, if a broadcast
  is lost, after the cache lease).
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
  ~:33-37). `Lock` runs its statements without a transaction: it reads the
  state, folds, sets `LOCKED`, then writes a savepoint of the fold
  (~:185-232); an extension committing between the fold (~:203) and the
  savepoint insert (~:224) gets a lower `seq` than the savepoint and is
  skipped by every later read.
- **cassandra** (out of tree) extends in a transaction of its own and retries
  conflicts (`SchemaExtendMaxRetries`, `CYODA_SCHEMA_EXTEND_MAX_RETRIES`,
  default 16).
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

`ModelStore.ExtendSchema(ctx, ref, delta)` (signature unchanged):

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
   extension conflict); a delta `applyFunc` refuses; storage unavailable; a `ctx` error. A non-nil error means the outcome is unknown; a retry is safe
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
   `Delete`, and serialises extenders of one model), the `LOCKED` check
   (§3.6), the fold, the no-op check (§4.3), the insert, the savepoint when
   due, `COMMIT`. The fold is a statement of its own after the lock statement:
   under `READ COMMITTED` a statement that waited for a lock re-checks only
   the locked row, so a fold merged into it would use the snapshot from before
   the wait. The transaction takes one row lock and runs at `READ COMMITTED`,
   so it cannot fail `40001` or `40P01`; the model pool sets `lock_timeout = 0`
   (the wait for another extender's lock lasts a few round trips) and the main
   pool's `statement_timeout`, whose abort keeps its documented answer
   (`docs/ARCHITECTURE.md` ~:451): a five-minute lock wait is a stuck system,
   not contention. The lock is held only for this transaction, never for an
   entity transaction. Every statement after the lock sees every earlier
   extension's commit, so `seq` order is commit order and a savepoint folded
   under the lock holds every lower-`seq` delta: the fold stays loss-free
   without serialising entity transactions. The ambient-transaction path and
   the `UPDATE models SET doc = doc` claim are deleted.
3. **A delta the schema already holds writes nothing** (postgres and sqlite,
   which keep a log; memory applies in place and needs no check). Under the
   lock, the backend compares `applyFunc(current, delta)` with `applyFunc(current, nil)`
   byte for byte (an empty delta gives a clean re-encoding of the same schema,
   `internal/domain/model/schema/apply.go` ~:29-31). Equal: no row, success.
   Otherwise it stores the caller's delta. The plugins need nothing beyond the
   `applyFunc` they already receive (`SetApplyFunc`, `app/app.go` ~:231);
   postgres refuses to extend without one, as sqlite and memory already do
   (today postgres tolerates a nil one, `model_store.go` ~:516-521). The
   comparison is canonical: the wire form sorts children (a JSON map) and keeps
   kinds and type sets in a fixed order (`model_schema.go` ~:218-225,
   ~:387-391; `merge.go` ~:28-35), and both sides go through the same
   unmarshal, clone and marshal; at worst it writes a needless row. N writers
   adding the same field write one row.
4. **postgres: a dedicated pool for the model store.** Model reads,
   extensions and model administration (`Lock`, `Unlock`, `SetChangeLevel`,
   `Save`, `Delete`) use a small pool of their own, as the scheduler has one
   (`plugins/postgres/scheduler_pool.go` ~:89-93,
   `CYODA_POSTGRES_SCHEDULER_CONNS`). A PUT, collection update or
   processor-output request already holds a main-pool connection for its
   transaction; taking the extension's connection from the same pool would let
   requests that each hold one and wait for another block one another until
   the acquire timeout. A model-pool holder never waits on the main pool or on
   entity rows, so there is no cycle. New variable
   `CYODA_POSTGRES_MODEL_CONNS` (default `8`, at least `2`; a smaller or
   malformed value fails startup). Cache misses (lease expiry, the read after
   an invalidation) run on this pool, coalesced per node (§4.7).
   - **An extension slot before a connection.** At most half the pool
     (`CONNS / 2`, at least `1`) is held by extensions at once, so model reads
     and administration always have the rest. The row-lock holder is always
     one of these short transactions, so the slots always make progress. The
     wait for a slot lasts until `ctx` ends: it is contention between
     extensions (§3.4). Taking the connection itself is bounded by
     `CYODA_POSTGRES_ACQUIRE_TIMEOUT` and answers the pool's existing
     retryable `503`, as the documented rule for a second connection inside a
     transaction says (`docs/ARCHITECTURE.md` ~:442, ~:450, ~:2564): with the
     slot cap that means a pool saturated by load, not contention.
   - An extending write holds its main-pool connection while it waits for a
     slot. Each extension is a few round trips, so slots free fast; a burst of
     extensions across many models holds that many main-pool connections for
     the length of the wait (§6).
5. **Identical deltas on one node share one call.** The caching layer
   coalesces concurrent `ExtendSchema` calls with the same tenant, model and
   delta into one call (single-flight, on the first caller's context);
   every caller gets its result. Each caller waits with `DoChan` and a
   `select` on its own context, so it can leave when its own context ends
   (it never stays blocked on another caller's call while holding its
   transaction's connection and gate). When the call fails and the first
   caller's context has ended while this caller's is live, this caller starts
   a new call, whatever form the error takes (a backend may surface a
   cancellation as a driver or classified error). Deltas are compared by their
   bytes, so `schema.Diff` emits its ops in a fixed order: `diffObject` ranges
   over a map today (`diff.go` ~:141, `model_schema.go` ~:89) and sorts its
   names instead; two writes adding the same fields then produce the same
   delta. The
   first caller's context may carry its entity transaction; that is harmless
   because `ExtendSchema` ignores it (§3.1). Different deltas are not merged:
   each is its own short transaction.
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
   - **Misses are coalesced, per generation.** `Get` and `SchemaNode` misses
     go through single-flight keyed by (key, generation): a read that starts
     after an invalidation never joins one that started before it (today the
     key is the key alone, `cache.go` ~:369-371). A caller that gets another
     caller's context error with its own context live reads again (as §4.5).
   - **A refresh reads storage without evicting.** `RefreshAndGet` (the
     one-shot refresh on an unknown path, §4.9) reads storage through the same
     (key, generation) flight as a miss, skipping the cached entry, and stores
     the result under the fill guard. It neither evicts nor bumps (today it
     evicts, `factory.go` ~:171), so a stream of requests on paths a model
     does not have does not empty the cache for valid traffic.
   - **After an extension call, whatever its outcome** (success, no-op or
     error), the cache evicts the entry locally, bumps its generation and
     broadcasts `model.invalidate`, every time: the call may have committed
     (an error leaves the outcome unknown, §3.6), and another node's
     broadcast for the same delta may have been lost. This runs after the
     inner call ends, in the call itself (§4.5), not in the callers, so it
     happens even if every caller has given up. So the extending node never
     keeps serving the old schema, and a retry after `409 MODEL_NOT_LOCKED`
     reads the model fresh.
   - `CachingModelStore` stops being a `ModelStore`: it is built in
     production only with a nil inner (`factory.go` ~:38) and its store
     methods serve tests. Its `Get` / `RefreshAndGet` / `SchemaNode` /
     `ExtendSchema` (`cache.go` ~:141-229, `SchemaNode` ~:292-305), the
     pass-throughs (~:183-235), the `inner` field, `New`'s `inner` parameter
     and the `var _ spi.ModelStore` assertion (~:374) go, leaving the cache
     state.
8. **A model is read in one snapshot.** postgres: one statement (base
   `models.doc`, the latest savepoint, the later deltas); today three. sqlite:
   one deferred read transaction (its handle is `_txlock=immediate`,
   `store_factory.go` ~:104, so a plain `BeginTx` would take the write lock;
   today `Get` reads the doc and then the log in
   separate statements, `sqlite/model_store.go` ~:99-121,
   `model_extensions.go` ~:23-24). A read that races an `Unlock` (which
   rewrites `doc` and clears the log) could otherwise combine the old `doc`
   with the cleared log.
9. **The negative path cache is deleted.** It has no lease and no
   generation, and it can refuse the initiator's own field on the node that
   served the write, indefinitely: a search's refresh reads storage before the
   extension commits (`search/service.go` ~:1939), the extension's
   invalidation drops the bucket (`app/app.go` ~:453 → `cache.go` ~:316), and
   the search then records the path absent (`markPathsAbsent`, ~:1992). It
   exists to absorb floods of invalid requests, and cyoda-go adds no such
   bounds. Deleting it also removes a mutex taken on every valid search
   (`path_validation_cache.go` ~:163-177, called at `service.go` ~:1932).
   The existing one-shot refresh on an unknown path (search and criterion
   path checks) stays and is consolidated: its copies (`ValidateKnownPaths`,
   `validateConditionPaths` `service.go` ~:1883-1993, the sort-key path
   ~:2097-2180, `refreshFieldsMap` `path_validate.go` ~:234-266,
   and `getModelFresh` `model/service.go` ~:122-131) become one helper.
   `EnsureModelRegistered`'s refresh (`internal/common/model_registration.go`
   ~:30-52, whose comment ~:16-19 claims a negative cache that does not exist)
   is deleted: it refreshes on not-found, and a not-found `Get` always reads
   storage (not-found is never cached, `factory.go` ~:135-141).
   `ValidateWithRefresh` (`handler.go` ~:162-202) has no production caller
   and goes with its tests. No other read gains a refresh: on the serving node
   the eviction and the fill guard (§4.7) already give the new schema, and
   other nodes follow the broadcast (requirement 2, §5.6).
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
    since its latest savepoint; sqlite numbers per model already). Today it
    measures distance in a sequence shared by all models, so on a busy system
    nearly every extension also folds and writes a savepoint: extra work on
    every extension.
13. **memory and sqlite.** memory keeps applying in place under `modelMu` and
    adds the `LOCKED` and no-op checks; its `Lock`, `Unlock`,
    `SetChangeLevel` and `IsLocked` return `spi.ErrNotFound` for a missing
    model, as the other backends do (today a bare "not found",
    `plugins/memory/model_store.go` ~:71, ~:82, ~:93, ~:103). sqlite keeps its own write
    transaction, adds the same checks, and retries `SQLITE_BUSY` until `ctx` ends
    instead of within a budget: `SchemaExtendMaxRetries` and
    `CYODA_SCHEMA_EXTEND_MAX_RETRIES` are deleted. Both already read the model
    ignoring the entity transaction.
14. **`Lock` writes no savepoint** (postgres `model_store.go` ~:216-231,
    sqlite ~:244-290). Under §3.6 no extension reaches an `UNLOCKED` model,
    and `Unlock`, `Save` and a new model leave the log empty, so at `Lock` the
    savepoint would equal the base schema. Writing it is what loses an
    extension that commits between `Lock`'s state change and its savepoint
    (§2). `Lock` is then one statement (the state update), on the model pool.
    Correctness does not depend on the log being empty: a savepoint only
    shortens the fold.
15. **Error mapping.** `ValidateOrExtend` tags store errors with
    `ErrInternalSchema` (`validate.go` ~:164) and the doors send that to
    `common.Internal` (`handler.go` ~:271; processor output through
    `classifyWorkflowError`, `service.go` ~:2859). `common.Internal`
    (`internal/common/errors.go` ~:266-300) maps `spi.ErrModelNotLocked` to
    `409 MODEL_NOT_LOCKED` before its generic case, so every door answers it
    without a branch of its own.
16. **Model administration overlapping an extension.** `SetUniqueKeys` and
    import read the model and then `Save` it whole (`model/service.go`
    ~:631-669, `postgres/model_store.go` ~:67-79); a `Lock` and an extension
    landing between the read and the `Save` would be overwritten. That is two
    administration calls overlapping on one model, which the help already puts
    outside the contract (model administration runs from one place,
    `cmd/cyoda/help/content/models.md` ~:341); such an overlap can also leave
    a delta row behind, since `Save`'s log delete runs in its statement's
    snapshot. #617 makes `Save` refuse a `LOCKED` model at write time.

## 5. Side effects (documented, §12)

1. A new field can be visible to other clients (model export, search path
   validation, criteria) before the write that brought it commits. A query on
   it finds nothing from that write until it commits.
2. If the write fails after the extension (a workflow or processor error, a
   unique-key violation, a conflict, the client cancelling, a crash), the model
   keeps the field. Removing it takes unlocking (which needs zero entities),
   deleting and importing the model again: an import into an existing model
   merges (`model/service.go` ~:177-182).
3. An extension made inside a joined compute-node callback stays even if the
   owner's transaction rolls back, or if the callback's scope is undone (an
   `ASYNC_NEW_TX` processor that fails discards its callbacks' entity writes,
   not their extensions).
4. Writers that bring different types for one field leave the model admitting
   both.
5. A failed request may still have extended the model; retrying it is safe.
6. Once a write that extends has returned `2xx`, the node that served it has
   the new schema. Another node has it when the invalidation broadcast
   arrives (normally milliseconds; if a broadcast is lost, when the cache
   lease, `CYODA_MODEL_CACHE_LEASE`, ends). Until then that node uses the
   older schema: a search or criterion on an unknown path refreshes once and
   normally finds the field (a refresh that joins a read started before the
   other node's commit still sees the older schema), but a query that does not refuse can silently miss the new
   field or a widened type (e.g. `code = 42` after `code` was widened from
   string to string-or-integer finds no integer match there), and a write,
   PATCH or validation there can be refused as if the field did not exist, or
   extend again (a no-op).
7. A `changeLevel` change takes effect on each node as its model cache picks
   it up (the invalidation broadcast; the cache lease if it is lost). Until
   then, a node keeps admitting or refusing extensions by the old level, and an
   extension computed under the old level can commit just after the change.
   The change level is set when a model is defined and rarely changes after
   it is locked; the help says so (§12).
8. A write that extends waits for an extension slot (§4.4); a client-set
   `transactionTimeoutMillis` keeps running while it waits, and on the doors
   that hold an entity transaction (PUT, collection update, processor output)
   so does `CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT`, since that transaction's
   connection is idle meanwhile.

## 6. Performance

Round trip = one request-response with the database.

| Case | Today (postgres) | After |
|---|---|---|
| Data fits the schema (steady state) | cached read, CPU diff, nil delta; no database work | the same; nothing new on the write or commit path |
| A write that extends | create: own transaction (~8 round trips); PUT / collection update / processor output: lock on the model row held for the whole entity transaction, callouts included; other extenders wait or get `409` | one short transaction on the model pool (~3 round trips with batching, one disk flush), the per-model lock held only for it; the entity transaction is longer by that, only for this write |
| Burst of N writes adding the same field | in-transaction: 1 winner, N−1 `409`s; self-wrapped: N queued behind a whole-transaction lock; N log rows | one call per node (§4.5), plus a no-op call for each write whose delta was computed before the first call ended; 1 row; one broadcast per call, and each node reads the model once per broadcast it receives (§4.7) |
| Many writers extending many models | serialised per model for whole transactions | parallel, up to half of `CYODA_POSTGRES_MODEL_CONNS` per node; no lock across models. A burst of different deltas on one hot model can fill every slot with writers waiting for that model's row, delaying other models' extensions on the node until it drains |
| A cache miss (lease expiry, after an invalidation) | read through the request's transaction or the main pool, not coalesced | one coalesced read per model per node, on the model pool |
| Every savepoint interval | savepoint folded inside an entity transaction | savepoint folded in the short transaction under the lock |

Where it can still slow down: a model whose field names are always new (an
object keyed by ids) extends on every write with a different delta; writes to
that model serialise on short transactions and its log grows. That is the
model's nature; this design makes each step short, and each extension costs
one storage read per node (the invalidation). A steady stream of requests on
paths a model does not have costs one storage read per refusal (the refresh,
§4.7), without emptying the cache for valid requests; cyoda-go adds no flood
bounds. A burst of extensions across many models holds one main-pool
connection per write waiting for an extension slot (§4.4).

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
| The initiator searches on its new field right after its write returned, on the node that served it | `400 INVALID_FIELD_PATH` possible indefinitely, if a search raced the extension (negative cache, no lease) | found |
| The same, on another node before the broadcast arrives | search: `400 INVALID_FIELD_PATH` possible (negative cache); PATCH, validate: `400 VALIDATION_FAILED`; export: field missing | search and criteria on the new path: one refresh, normally found; otherwise the older schema until the broadcast arrives (§5.6) |
| An operator changes `changeLevel` on a locked model | takes effect as each node's cache picks it up (postgres: an in-transaction extender conflicts with it) | takes effect as each node's cache picks it up (§5.7) |
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
| Search, criteria (`INVALID_FIELD_PATH`) | `2xx` instead of `400` | the initiator's own new field on the node that served its write (the negative path cache is gone) |

## 9. What the design deletes

The postgres write claim and its comment; the postgres ambient-transaction
extension path; `CachingModelStore`'s read/extend methods; the negative path
cache (`search.PathValidationCache`, its wiring at `app/app.go` ~:452-453,
`WithPathValidationCache`, `markPathsAbsent`, `markPathsPresent`,
`cachedAbsentPaths`, `pathSurface` / `surfaceSort`, `search/service.go`
~:497-504, ~:2020-2080, and their tests, `search/export_test.go` and
`modelcache/local_subscriber_test.go` among them, and the `pathCache` field,
~:339-342), and with it the cache's local
subscribers it was the only user of (`SubscribeLocal`,
`notifyLocalSubscribers`, `localSubs`, `cache.go` ~:46-57, ~:334-360,
`factory.go` ~:108-115); the copies of the refresh that §4.9 merges and `ValidateWithRefresh` with its tests; the
`tx_id` column of the extension logs (postgres `model_store.go` ~:226,
~:538, ~:561; sqlite ~:286, ~:532, ~:559), written and never read, and
meaningless once an extension never joins a transaction (it is `NOT NULL`,
`000001_initial_schema.up.sql` ~:180, so postgres migration `000018` and the
next sqlite migration drop it); the sqlite retry budget
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
`grep -rn "SchemaExtendMaxRetries\|SCHEMA_EXTEND_MAX_RETRIES\|models SET doc = doc\|PathValidationCache\|markPathsAbsent\|markPathsPresent\|SubscribeLocal\|notifyLocalSubscribers"`
over cyoda-go, the SPI and the three in-tree plugins (`--include='*.go'`, plus `docs/`
outside `docs/superpowers/`, `cmd/cyoda/help/`, `README.md`) is empty.

## 10. Test coverage matrix

| Scenario | Unit / plugin | spitest | e2e (postgres) | Parity | gRPC |
|---|---|---|---|---|---|
| Extension visible to `Get` on return; kept after the caller's transaction rolls back | per-plugin | new | rewritten processor-output test (field kept) | new | — |
| A delta the schema already holds: no row | per-plugin | new | — | — | — |
| Two extenders of one model concurrently: both succeed, both deltas in the schema, never `ErrConflict` | per-plugin | new | — | — | — |
| Extension of a model that is not `LOCKED` → `ErrModelNotLocked` | per-plugin | new | new, through a test compute node that unlocks the model during a callout (the owner's entity is uncommitted, so the unlock succeeds; `UnlockModel` counts committed entities, `model/service.go` ~:381): a create whose processor returns a new field; a PUT and a collection update made by a joined callback on the owner's uncommitted entity with a new field (`updateEntityCore` never checks the state before extending, `entity/service.go` ~:2207-2238); each `409 MODEL_NOT_LOCKED`. Also a manual transition whose processor returns a new field (the transition door). The test compute node's unlock is an unjoined call (a joined one is refused `MODEL_ADMIN_IN_JOINED_TRANSACTION`). A direct create and a collection create (no callout) reach it only by a race no test can order without a production hook: waived | new: the same scenario on every backend | new, same setup over gRPC (envelope `Error.Code`) |
| `Get` ignores the transaction on `ctx` | per-plugin | new | — | — | — |
| Concurrent PUTs, collection updates, collection creates and processor-output writes each adding fields → all `2xx`, every field in the schema | — | — | extend `model_schema_extensions_test.go` ~:36-75 (today creates only) to every door (isolated, not parity) | — | new (concurrent gRPC writes, envelope `Success=true`) |
| A write adds a field nested under a parent another write added after this transaction began → `2xx` | postgres plugin | — | new | — | — |
| Criterion in the same request on the field just added | engine unit | — | new | new | — |
| Multi-node: an extension on node A is visible on node B after the broadcast | — | — | — | new, multi-node harness (`e2e/parity/postgres/multinode_test.go`) | — |
| A refresh does not evict and stores under the fill guard; an extension error still evicts, bumps and broadcasts | modelcache unit | — | — | — | — |
| Fill guard drops a fill that straddles an invalidation (local and broadcast, also after eviction); a read starting after an invalidation never joins a flight started before it; one caller's cancellation does not fail the others | modelcache unit | — | — | — | — |
| Identical concurrent `ExtendSchema` calls on one node → one storage call; every extension call, a no-op included, evicts, bumps and broadcasts; a caller arriving after a shared call was cancelled starts a fresh one | modelcache unit | — | — | — | — |
| `CYODA_POSTGRES_MODEL_CONNS` below 2 or malformed fails startup | postgres config unit | — | — | — | — |
| The initiator's search on its new field on the serving node after a search raced the extension → `2xx` (negative cache gone) | search unit | — | new | — | — |
| postgres: a savepoint under the lock keeps every lower-`seq` delta; the interval counts the model's deltas; a one-statement read during `Unlock` misses nothing | postgres plugin | — | — | — | — |
| postgres: at most `CONNS / 2` extension connections; model reads proceed during an extension burst; no deadlock with the main pool | postgres plugin | — | — | — | — |
| sqlite: a read racing `Unlock` misses nothing (one read transaction) | sqlite plugin | — | — | — | — |
| memory: `Lock`/`Unlock`/`SetChangeLevel`/`IsLocked` on a missing model → `spi.ErrNotFound` | memory plugin | existing model cases extended | — | — | — |
| sqlite: `SQLITE_BUSY` retried until `ctx` ends | sqlite plugin | — | — | — | — |
| `Lock` writes no extension-log row | postgres + sqlite plugin | — | — | — | — |
| Two writes adding the same fields produce the same delta bytes (`schema.Diff` sorted) | schema unit | — | — | — | — |
| A joiner of a shared extension call leaves when its own context ends; retries when only the leader's ended | modelcache unit | — | — | — | — |
| Processor-output storage failure → `503` | workflow unit (failing store double) | — | waived: a storage outage cannot be produced in e2e without a test hook in production code | — | waived, same reason |
| Throughput benchmark (§6) | benchmark | — | — | — | — |

The plugin tests that call `ExtendSchema` on an `UNLOCKED` model (57 call
sites, `grep -c "ExtendSchema(" plugins/*/*_test.go`; the postgres fixture
saves `State: spi.ModelUnlocked`, `model_extensions_internal_test.go` ~:234)
lock the model first: `ErrModelNotLocked` refuses them. The save-on-lock
tests (`TestExtendSchema_SaveOnLock`, `model_extensions_internal_test.go`
~:430, its sqlite twin) and the parity scenario
`e2e/parity/schema_save_on_lock.go` (registered at `e2e/parity/registry.go`
~:293) are deleted with the savepoint (§4.14).

Concurrency tests are isolated single-backend tests, never in the shared
parity suite. Each new test is shown failing on the unfixed code first; one
that cannot go RED on at least one backend is dropped.

## 11. SPI and cross-repo

- SPI: `persistence.go` (`ExtendSchema`, `Get`, §3), `errors.go`
  (`ErrModelNotLocked` new; `ErrRetryExhausted` doc), spitest cases (§10), SPI
  CHANGELOG. SPI PR into SPI `main`; cyoda-go and the three plugins pseudo-pin
  it (`make repin-plugins`).
- `CHANGELOG.md` `### Breaking` (contract, removed variable, removed negative
  path cache, new variable), `COMPATIBILITY.md`.
- `docs/cloud-parity/710-schema-extensions.md`: the contract and the side
  effects.
- Issue in `cyoda-go-cassandra`: §3 (no
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
`grep -rniE "extendschema|schema extension|extension log|savepoint|SCHEMA_EXTEND|changeLevel|change level|extends? the (model|schema)|model extension|commit-bound|path validation|path-validation|MODEL_CACHE_LEASE|RefreshAndGet|POSTGRES_.*CONNS|connection pool|pool(s|ed)?\b|second connection|model cache|invalidat"`
over `README.md`, `CONTRIBUTING.md`, `COMPATIBILITY.md`, `docs/` (outside
`docs/superpowers/`, `docs/cloud-parity/` included), `cmd/cyoda/help/content/`,
`api/openapi.yaml` and the `plugins/*/doc.go` and `plugin.go` files:

- `cmd/cyoda/help/content/models.md` (~:77, ~:98, ~:334, ~:341 the
  model-administration paragraph, ~:343-345, the ERRORS list ~:348-361 which
  gains `MODEL_NOT_LOCKED`): when an extension happens, that it never
  conflicts, that it is visible when the write returns and may be earlier,
  the side effects, the `409 MODEL_NOT_LOCKED` case, and on the
  `changeLevel` operation (~:129-131) that a change on a locked model takes
  effect as each node's model cache picks it up, and that the level is
  normally set before the model is locked (§5.7).
- `cmd/cyoda/help/content/config/schema.md`: what the savepoint interval bounds
  (the deltas a read folds, per model; the current "reduce recovery time after
  a crash" is wrong), the retry variable removed, the examples.
- `cmd/cyoda/help/content/config/database.md`: `CYODA_POSTGRES_MODEL_CONNS`.
- `cmd/cyoda/help/content/config/database.md` ~:83 and
  `errors/STORAGE_UNAVAILABLE.md` ~:25, `docs/ARCHITECTURE.md` ~:442, ~:450,
  ~:2023, ~:2559 (where the new variable's row goes), ~:2564,
  `plugins/postgres/plugin.go` ~:26, ~:30 and `plugins/postgres/doc.go` ~:44:
  the second-connection rule (the model pool, §4.4) and the savepoint interval
  now counting per-model deltas.
- `docs/audits/2026-06-e2e-test-catalog.md` ~:937-947 and ~:964: the tests §9
  and §10 rewrite or delete.
- `docs/PROCESSOR_EXECUTION_MODES.md` ~:42, ~:173-227,
  `cmd/cyoda/help/content/workflows.md` ~:174 ("with everything its callbacks
  wrote") and `docs/ARCHITECTURE.md` ~:659, ~:832-853: a discarded callback
  scope keeps the extensions its callbacks made (§5.3).
- `cmd/cyoda/help/content/workflows.md` (~:149): processor output extends like
  a client write, with the same side effects.
- `cmd/cyoda/help/content/crud.md`, `grpc.md`, `openapi.md`, `errors.md`: any
  extension or field-path wording.
- `cmd/cyoda/help/content/errors/`: `INVALID_FIELD_PATH.md` (~:51: the refresh; no
  negative cache), `MODEL_NOT_LOCKED.md` (the
  extension case), `CONFLICT.md` (never from extensions),
  `INVALID_CHANGE_LEVEL.md`, `INCOMPATIBLE_TYPE.md`, `VALIDATION_FAILED.md`,
  `MODEL_ADMIN_IN_JOINED_TRANSACTION.md`, `STORAGE_UNAVAILABLE.md`
  (processor output's `503`).
- `docs/CONSISTENCY.md` §3a: the five invariants rewritten to §3 (invariant 1:
  no conflict; invariant 2: visible on return, may be earlier, kept on
  failure) and the Unlock operator contract's postgres paragraph.
- `docs/ARCHITECTURE.md`: the extension log and savepoints (~:331-339), the
  model cache and invalidation (~:944-949), the postgres pools, the config tables (~:2515, ~:2567, ~:2582-2583).
- `docs/FEATURES.md`, `docs/PRD.md`, `docs/plugins/*.md`: extension wording;
  `docs/plugins/POSTGRES.md` (~:220, ~:503) the pools and variables.
- `cmd/cyoda/help/content/cluster.md` (~:185, model cache invalidation) and
  `cmd/cyoda/help/content/quickstart.md` (~:69-70, postgres connection
  defaults).
- `docs/CONCURRENCY.md` (~:61, ~:108): the lock inventory (the path-validation
  cache goes; the model pool and its extension slots come).
- `internal/common/model_registration.go` ~:16-19 and `model/service.go`
  ~:115-119 comments (§4.9).
- `cmd/cyoda/help/content/config.md` ~:88: `CYODA_MODEL_CACHE_LEASE` is also
  the bound when an invalidation broadcast is lost.
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
- The coalescing key's exact form (§4.5).
- Whether `ValidateOrExtend` validates against the cache's parsed node
  instead of re-parsing the schema on every write (`validate.go` ~:85), once
  it has confirmed that admission never mutates the node: a free saving on the
  steady state.
