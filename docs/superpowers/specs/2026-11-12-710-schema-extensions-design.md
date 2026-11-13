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
   preferred, with its side effects documented.
3. The mechanism must not materially slow down transactional throughput.
4. No returned extension is ever lost, and the backends behave the same on
   everything the contract guarantees.

### Goals

- Every backend commits an extension on its own, before the entity write, and
  never fails a write because another write extends the same model.
- The steady state (data that fits the schema) does no extra work.
- A write that extends pays one short transaction, never a lock held for the
  length of another request.
- The model cache never keeps a schema older than a returned extension longer
  than the invalidation broadcast takes, and never serves a schema that
  storage does not hold.
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

`ModelStore.ExtendSchema(ctx, ref, delta)`:

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
   for contention.
5. **Loss-free.** The folded schema includes every delta whose call returned
   nil.
6. **Errors:** `spi.ErrNotFound` (no model); a new `spi.ErrModelNotLocked`
   when the model is not `LOCKED` under the backend's lock (`409
   MODEL_NOT_LOCKED`, an existing code; a model-lifecycle conflict, not an
   extension conflict); a delta `applyFunc` refuses; storage unavailable; a
   `ctx` error. A non-nil error means the outcome is unknown; a retry is safe
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
   `Delete`, and serialises extenders of one model), the `LOCKED` check, the
   fold (one statement, §4.8), the no-op check (§4.3), the insert, the
   savepoint when due, `COMMIT`. The lock is held only for this transaction,
   never for an entity transaction, so extenders of one model wait a few round
   trips at most and never fail. Every statement after the lock sees every
   earlier extension's commit, so `seq` order is commit order and a savepoint
   folded under the lock holds every lower-`seq` delta: the fold stays
   loss-free without serialising entity transactions. The ambient-transaction
   path and the `UPDATE models SET doc = doc` claim are deleted.
3. **A delta the schema already holds writes nothing.** Under the lock, the
   backend applies the delta to the current fold and asks `schema.Diff`
   (`internal/domain/model/schema/diff.go` ~:17) for the difference, which is
   nil for a semantic no-op. Nil: no row, no invalidation, success. N writers
   adding the same field write one row.
4. **postgres: a dedicated pool for the model store.** Model reads and
   extensions use a small pool of their own, as the scheduler does
   (`plugins/postgres/scheduler_pool.go` ~:89-93,
   `CYODA_POSTGRES_SCHEDULER_CONNS`). A PUT, collection update or
   processor-output request already holds a main-pool connection for its
   transaction; taking the extension's connection from the same pool would let
   requests that each hold one and wait for another block one another until
   the acquire timeout. A model-pool holder never waits on the main pool or on
   entity rows, so there is no cycle. New variable
   `CYODA_POSTGRES_MODEL_CONNS` (default `4`, at least `1`; a smaller or
   malformed value fails startup). Exhaustion makes an extending write wait,
   then fail closed with the pool's existing retryable `503`.
5. **Identical deltas on one node share one call.** The caching layer
   coalesces concurrent `ExtendSchema` calls with the same tenant, model and
   delta into one call (single-flight); every caller gets its result.
   Different deltas are not merged: each is its own short transaction.
6. **`ModelStore.Get` ignores the entity transaction on every backend.** It
   reads committed state (postgres: on the model pool). So a workflow
   criterion later in the same request sees the field the request just added,
   and a cache fill can never carry uncommitted data. A transaction that reads
   a schema newer than its snapshot is safe: a `LOCKED` model's schema only
   grows.
7. **The model cache.**
   - **Fill guard.** Each key has an invalidation generation, bumped by every
     local invalidation and every one received by broadcast
     (`cache.go` ~:325-331). A fill records it before reading and stores only
     if it is unchanged.
   - **Misses are coalesced.** `Get` and `SchemaNode` misses go through
     single-flight, as `RefreshAndGet` does, so an invalidation of a hot model
     costs one read per node.
   - **Invalidate only on change.** `ExtendSchema` invalidates (locally and by
     broadcast) after the extension committed, and only when it changed the
     schema.
   - `CachingModelStore`'s own `Get` / `RefreshAndGet` / `SchemaNode` /
     `ExtendSchema` (`cache.go` ~:141-229) are built in production only with a
     nil inner (`factory.go` ~:38) and serve tests; they go, leaving the cache
     state.
8. **postgres reads a model in one statement**: base (`models.doc`), the
   latest savepoint and the later deltas. Today it is three statements; a read
   that races an `Unlock` (which rewrites `doc` and clears the log) could
   combine the old `doc` with the cleared log.
9. **The negative path cache is deleted.** It exists to absorb floods of
   invalid requests; cyoda-go adds no such bounds. With no lease and no
   generation it can answer `400 INVALID_FIELD_PATH` for a field that exists,
   on a node that has not yet received the invalidation, which breaks
   requirement 2 for the initiator. The bounded refresh (`path_validate.go`
   ~:314-357) becomes the only mechanism, and also covers a type mismatch on a
   known path (a field whose type another write widened), not only an unknown
   path.
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
    adds the `LOCKED` and no-op checks. sqlite keeps its own write
    transaction, adds both checks, and retries `SQLITE_BUSY` until `ctx` ends
    instead of within a budget: `SchemaExtendMaxRetries` and
    `CYODA_SCHEMA_EXTEND_MAX_RETRIES` are deleted. Both already read the model
    ignoring the entity transaction.
14. **postgres `Lock`** moves into one `READ COMMITTED` transaction and
    decides "was unlocked" from its own conditional update, so its savepoint is
    written only on the real `UNLOCKED → LOCKED` edge.

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
6. Guarantee: once a write that extends has returned `2xx`, the initiator sees
   the field on every node: a node that has not yet received the invalidation
   refreshes once on the unknown path or the type mismatch.

## 6. Performance

Round trip = one request-response with the database.

| Case | Today (postgres) | After |
|---|---|---|
| Data fits the schema (steady state) | cached read, CPU diff, nil delta; no database work | the same; nothing new on the write or commit path |
| A write that extends | create: own transaction (~8 round trips); PUT / collection update / processor output: lock on the model row held for the whole entity transaction, callouts included; other extenders wait or get `409` | one short transaction on the model pool (~3 round trips with batching, one disk flush), the per-model lock held only for it; the entity transaction is longer by that, only for this write |
| Burst of N writes adding the same field | in-transaction: 1 winner, N−1 `409`s; self-wrapped: N queued behind a whole-transaction lock; N log rows | one call per node (§4.5); across M nodes, M short transactions in turn, 1 row, 1 invalidation, then one read per node (§4.7) |
| Many writers extending many models | serialised per model for whole transactions | parallel, up to `CYODA_POSTGRES_MODEL_CONNS` per node; no lock across models |
| Every savepoint interval | savepoint folded inside an entity transaction | savepoint folded in the short transaction under the lock |

Where it can still slow down: a model whose field names are always new (an
object keyed by ids) extends on every write with a different delta; writes to
that model serialise on short transactions and its log grows. That is the
model's nature; this design makes each step short. An exhausted model pool
makes extending writes wait, then fail closed with a retryable `503`.

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
| The initiator searches on the new field right after its write returned, on another node | `400 INVALID_FIELD_PATH` possible until the broadcast arrives (negative cache) | `200` (one refresh) |
| A criterion in the same request on the field just added | works through the shared cache on most paths | works on every path (§4.6) |
| A storage failure during processor-output extension | `500` | `503` (retryable) |
| Extension after the model was unlocked mid-request | postgres: appended to the unlocked model's log | `409 MODEL_NOT_LOCKED` on every backend |

## 8. Error / status table

| Endpoint | Status / code | Scenario |
|---|---|---|
| Entity create, update (PUT), collection create/update, transitions (processor output); HTTP and gRPC | `2xx` | a concurrent write extends the same model (was `409` on postgres for PUT, collection update, processor output) |
| | `409 MODEL_NOT_LOCKED` | the model was unlocked after the request read it, before its extension |
| | `400 VALIDATION_FAILED` / change-level refusals | unchanged |
| | `503` (retryable) | storage unavailable, or the model pool exhausted, during an extension (processor output was `500`) |
| Search, criteria (`INVALID_FIELD_PATH`) | `200` instead of `400` | a field another node just added, before the broadcast arrives |

## 9. What the design deletes

The postgres write claim and its comment; the postgres ambient-transaction
extension path; `CachingModelStore`'s read/extend methods; the negative path
cache (`search.PathValidationCache`, its wiring at `app/app.go` ~:452-453,
`markPathsAbsent` / `cachedAbsentPaths`, their tests); the sqlite retry budget
(`SchemaExtendMaxRetries`, `CYODA_SCHEMA_EXTEND_MAX_RETRIES` in sqlite
`config.go`, `plugin.go`, help, `docs/ARCHITECTURE.md`, `DefaultConfig` and
`README.md` where listed); the postgres-only conflict tests
(`model_extensions_concurrency_internal_test.go` ~:34 expecting `ErrConflict`,
`handler_extend_conflict_test.go`). The tests that assert an extension rolls
back with its transaction (`plugins/postgres/model_extensions_test.go`
~:298-339, `internal/e2e/workflow_proc_output_test.go` ~:135
`TestWorkflowProcOutput_ExtensionRollsBackWithTheTransition`) are rewritten
to the new rule (the field is kept). Exit check:
`grep -rn "SchemaExtendMaxRetries\|SCHEMA_EXTEND_MAX_RETRIES\|SET doc = doc\|PathValidationCache\|markPathsAbsent"`
over cyoda-go, the SPI and the plugins (`--include='*.go'`, plus `docs/`
outside `docs/superpowers/`, `cmd/cyoda/help/`, `README.md`) is empty.

## 10. Test coverage matrix

| Scenario | Unit / plugin | spitest | e2e (postgres) | Parity | gRPC |
|---|---|---|---|---|---|
| Extension visible to `Get` on return; kept after the caller's transaction rolls back | per-plugin | new | rewritten processor-output test (field kept) | new | — |
| A delta the schema already holds: no row, no invalidation | per-plugin | new | — | — | — |
| Two extenders of one model concurrently: both succeed, both deltas in the schema, never `ErrConflict` | per-plugin | new | — | — | — |
| Extension of a model that is not `LOCKED` → `ErrModelNotLocked` | per-plugin | new | new (`409 MODEL_NOT_LOCKED`) | — | new (envelope) |
| `Get` ignores the transaction on `ctx` | per-plugin | new | — | — | — |
| Concurrent PUTs, collection updates, collection creates and processor-output writes each adding fields → all `2xx`, every field in the schema | — | — | new (isolated, not parity) | — | new (concurrent gRPC writes, envelope `Success=true`) |
| A write adds a field nested under a parent another write added after this transaction began → `2xx` | postgres plugin | — | new | — | — |
| Criterion in the same request on the field just added | engine unit | — | new | new | — |
| Initiator's search on its new field on another node before the broadcast → `200` | search unit | — | — | new, multi-node harness (`e2e/parity/postgres/multinode_test.go`) | — |
| Type mismatch on a known path refreshes once | search unit | — | — | — | — |
| Fill guard drops a fill that straddles an invalidation (local and broadcast); misses coalesced | modelcache unit | — | — | — | — |
| Identical concurrent `ExtendSchema` calls on one node → one storage call | modelcache unit | — | — | — | — |
| postgres: a savepoint under the lock keeps every lower-`seq` delta; the interval counts the model's deltas; a one-statement read during `Unlock` misses nothing | postgres plugin | — | — | — | — |
| postgres: model pool exhausted → extending write `503`, no deadlock with the main pool | postgres plugin | — | — | — | — |
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
- Issue in `cyoda-go-cassandra`: §3 (no `ErrRetryExhausted` for contention,
  retry until `ctx` ends; the no-op check; `ErrModelNotLocked`; `Get` ignores
  the transaction).

## 12. Documentation: one description, everywhere

The builder audits **every** place that describes schema extension, the model
cache, field-path validation or the variables this change touches, reads each
document in full (not only the matching lines), and makes them say the same
thing: §3's contract, §5's side effects, and §4's mechanics at the level each
document works at. Present tense, no history. Sites found by
`grep -rniE "extendschema|schema extension|extension log|savepoint|SCHEMA_EXTEND|changeLevel|change level|extends? the (model|schema)|model extension|commit-bound|path validation|MODEL_CACHE_LEASE|RefreshAndGet"`
over `README.md`, `CONTRIBUTING.md`, `COMPATIBILITY.md`, `docs/` (outside
`docs/superpowers/`), `cmd/cyoda/help/content/` and `api/openapi.yaml`:

- `cmd/cyoda/help/content/models.md` (~:77, ~:98, ~:334, ~:341 the
  model-administration paragraph, ~:343-345): when an extension happens, that
  it never conflicts, that it is visible when the write returns and may be
  earlier, the side effects, the `409 MODEL_NOT_LOCKED` case.
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
  `MODEL_ADMIN_IN_JOINED_TRANSACTION.md`.
- `docs/CONSISTENCY.md` §3a: the five invariants rewritten to §3 (invariant 1:
  no conflict; invariant 2: visible on return, may be earlier, kept on
  failure) and the Unlock operator contract's postgres paragraph.
- `docs/ARCHITECTURE.md`: the extension log and savepoints (~:331-339), the
  model cache and invalidation (~:944-949), the negative path cache, the
  postgres pools, the config tables (~:2515, ~:2567, ~:2582-2583).
- `docs/FEATURES.md`, `docs/PRD.md`, `docs/plugins/*.md`: extension wording.
- `api/openapi.yaml`: the `changeLevel` operation (~:5344-5470) and every
  response description that mentions extension, `409` or `INVALID_FIELD_PATH`;
  then `go generate ./api`.
- `README.md`: configuration reference (new variable, removed variable).
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
