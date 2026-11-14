# Schema extensions commit on their own and never conflict

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Behaviour

A model with a `changeLevel` grows as entity writes bring new structure the
level permits: the write computes an additive delta and extends the model
before its own entity changes commit. This applies to every write that can
extend — entity create and update, collection create and update, and data a
workflow processor returns — over HTTP and gRPC.

- **Commits on its own.** The extension is committed in a transaction of its
  own, never in the write's transaction (owned or joined). When the extension
  call returns, it is durable, whatever then happens to the write.
- **Visible on return.** When the write returns `2xx`, the node that ran it
  serves the new schema (the node that executes the write, wherever the
  transaction's coordination routes it). Another node serves it once the model cache
  invalidation broadcast arrives, or when the cache lease
  (`CYODA_MODEL_CACHE_LEASE`) ends if the broadcast is lost. Others may see
  the extension before the write returns.
- **Idempotent.** An extension the schema already holds writes nothing; a
  retry of a request whose outcome is unknown is safe.
- **Never a conflict.** Two writes extending the same model never fail
  because of each other: no `409 CONFLICT`, no retry-budget error.
  Contention is a wait that lasts until the request ends.
- **Loss-free.** Every extension whose call returned is in the model's
  schema.
- **Locked models only.** An extension of a model that is not `LOCKED` is
  refused, answered `409 MODEL_NOT_LOCKED` (an existing code). A write
  reaches it only when the model is unlocked while the write, or the
  transaction it joined, is in progress, e.g. during a compute-node callout
  on an uncommitted entity; an update, collection update or transition does
  not check the model's state before it extends.
- **Storage failure is retryable.** A storage outage during an extension,
  including one for data a workflow processor returns, answers the retryable
  `503 STORAGE_UNAVAILABLE`.
- **Model reads ignore the transaction.** A model read returns the latest
  committed model whatever transaction the request holds, so a criterion
  later in the same request sees a field the request just added.

## Side effects clients see

1. A new field can be visible to other clients (model export, search path
   validation, criteria) before the write that brought it commits; a query
   on it finds nothing from that write until it commits.
2. A write that fails after its extension — a workflow or processor error,
   a unique-key violation, a conflict, a client cancel, a crash — leaves the
   field in the model. Removing it takes unlock (zero entities), delete and
   a fresh import. A write whose own data fills a composite unique key only
   in part, or puts a non-scalar value in a key's field, is refused with
   `422 INVALID_UNIQUE_KEY` before its schema check on every door, so it
   extends nothing; a partial key in data a processor returns is found only
   when the entity is saved, after the extension.
3. An extension made by a joined compute-node callback stays if the owner's
   transaction rolls back, or if the callback's scope is discarded (a failed
   `ASYNC_NEW_TX` processor discards its callbacks' entity writes, not their
   extensions).
4. Writers that bring different types for one field leave the model
   admitting both.
5. A failed request may still have extended the model; retrying is safe.
6. On another node, until the broadcast arrives, the older schema is in
   use: a search or criterion on an unknown path reads the model once more
   and normally finds the field; a query that does not refuse can miss the
   new field or a widened type; a write, PATCH or validation there can be
   refused as if the field did not exist, or extend again (a no-op).
7. A `changeLevel` change on a locked model takes effect on each node as its
   model cache picks it up; set the level before locking.
8. A write that extends waits for an extension slot (PostgreSQL: half the
   model pool); a client-set `transactionTimeoutMillis` keeps running while
   it waits, and on the writes that hold an entity transaction (update,
   collection update, processor output) so does the idle-in-transaction
   ceiling.

## No negative path cache

A search, conditional delete or criterion path check that names a path the
node's cached schema does not hold reads the model from storage once, then
refuses with `400 INVALID_FIELD_PATH` if the path is still unknown. No
refusal is remembered between requests.

## Alignment consequence for Cloud

Cloud must never answer a conflict or retry-exhaustion error because of a
concurrent extension of the same model, must keep an extension when the
write that made it fails, and must answer `409 MODEL_NOT_LOCKED` for an
extension of a model that is not locked.

## Test coverage (cyoda-go)

- SPI conformance (`cyoda-go-spi/spitest/model_extend.go`): visible on
  return, kept when the caller's transaction rolls back, `Get` ignoring the
  caller's transaction, a delta the schema holds writing nothing, concurrent
  extenders, `ErrModelNotLocked`, a missing model, an empty delta.
- E2E, PostgreSQL (`internal/e2e`):
  - `model_schema_extensions_test.go` — `TestSchemaExtension_ConcurrentEveryDoor_No409`
    (concurrent creates, updates, collection creates and updates and
    processor output on one model, every write `2xx`, every field in the
    schema).
  - `schema_extension_doors_test.go` — `TestSchemaExtension_ConcurrentNestedUnderNewParent`,
    `TestSchemaExtension_CriterionOnFieldAddedInSameRequest`,
    `TestGRPC_ConcurrentExtension_SuccessEnvelope`.
  - `schema_extension_not_locked_test.go` — `TestSchemaExtension_NotLocked_CreateProcessorOutput`,
    `_CreateCollection`, `_JoinedUpdate`, `_JoinedCollectionUpdate`,
    `_JoinedManualTransition` (`409 MODEL_NOT_LOCKED`), and on gRPC
    `TestGRPC_ModelNotLocked_Envelope`, `_CreateCollection`, `_JoinedUpdate`,
    `_JoinedCollectionUpdate`, `_JoinedTransition` (`CLIENT_ERROR`, message
    prefix `MODEL_NOT_LOCKED:`, not retryable).
  - `unique_keys_write_variants_test.go` — `TestUniqueKeys_PartialKeyDoesNotExtendModel`
    (a partial key with a new field: `422 INVALID_UNIQUE_KEY` and no
    extension, on every client write door).
  - `workflow_proc_output_test.go` — `TestWorkflowProcOutput_ExtensionKeptWhenTransitionFails`.
- Cross-backend parity (memory, sqlite, postgres): `e2e/parity/schema_extension_contract.go`
  — `SchemaExtension_KeptAfterFailedWrite`, `SchemaExtension_CriterionSameRequest`,
  `SchemaExtension_NotLocked`.
- Multi-node: `e2e/parity/multinode/schema_extension.go` —
  `SchemaExtension_VisibleOnOtherNode` (a node that cached the model serves
  the new field after another node's write).
