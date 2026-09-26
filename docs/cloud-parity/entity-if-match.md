# Entity `If-Match` — the version an update starts from

cyoda-go defines this contract; Cyoda Cloud mirrors it.

## Doors

Every entity update that takes a precondition:

| Door | Precondition |
|---|---|
| `PUT /entity/{format}/{entityId}` and `PUT /entity/{format}/{entityId}/{transition}` | `If-Match` header, optional |
| `PATCH /entity/{format}/{entityId}` and `PATCH /entity/{format}/{entityId}/{transition}` | `If-Match` header, required (`*` = none; see `entity-patch.md`) |
| `PUT /entity/{format}` (collection) | per-item `ifMatch`, optional |
| gRPC `EntityPatchRequest` | `payload.ifMatch`, required (`*` = none) |

## Rule

`If-Match` names the version the update starts from: the entity's
`meta.transactionId` from the caller's last read.

1. The server checks it once, when the update's transition starts, against the
   entity as the update's own transaction reads it — before the workflow is
   selected and before any criterion or processor runs.
2. A mismatch answers `412 ENTITY_MODIFIED`. The state-machine audit records
   `STATE_MACHINE_START` and then `TRANSITION_ABORTED`
   (`{reason: "ENTITY_MODIFIED", transitionName, expectedTxId, actualTxId}`),
   and nothing else of the transition. On the collection door the item is
   listed in `failed[]`, its siblings commit, and so do those two events.
3. A write to the entity later in the same transaction is the update's own and
   does not break the precondition: a processor's callback that joined the
   transaction (`SYNC`, `ASYNC_SAME_TX`), or a `COMMIT_BEFORE_DISPATCH`
   segment's own commit. The update succeeds; a callback's write is kept when
   the processor answers with no payload.
4. A change that another transaction commits after the update's read is a lost
   race, not a failed precondition: the update answers the retryable
   `409 CONFLICT`, and the transaction that lost the race does not commit. On
   the collection door the whole request answers `409`; the item is not
   isolated.
5. The same on every storage backend.

One case answers `412 ENTITY_MODIFIED` outside this rule, with or without
`If-Match`: a `COMMIT_BEFORE_DISPATCH` processor committed the update's first
segment before its callout, and another transaction changed the entity before
the processor's result was applied. A single update answers `412`; a collection
update fails whole with `400 WORKFLOW_FAILED`. The committed segment stays.

## What changed

- Before, the precondition was compared at the update's final save (or at the
  first `COMMIT_BEFORE_DISPATCH` segment's flush), against the entity as the
  transaction then saw it. A processor's callback that wrote the entity in the
  same transaction made every such update answer `412` although nobody else
  had written the entity.
- A change committed by another transaction after the update's read could
  answer `412` (and be isolated as a collection item). It is now `409` on every
  backend.
- A stale `If-Match` was found only after the transition had run, or at its
  first segment flush, so the audit recorded the transition's own events
  (`WORKFLOW_FOUND`, `TRANSITION_MAKE`, …) before `TRANSITION_ABORTED`. Now
  only `STATE_MACHINE_START` precedes it.

## Tests

- Parity (memory, SQLite, PostgreSQL): `e2e/parity/ifmatch_callback_self_write.go`
  (`IfMatch_OwnEntityCallbackWrite`) — the loopback `PUT` door: a callback's
  write kept under `SYNC`, `ASYNC_SAME_TX` and before a `COMMIT_BEFORE_DISPATCH`
  segment; a stale `If-Match` is `412`; a callback write that loses a race is
  `409`.
- E2E (PostgreSQL): `internal/e2e/ifmatch_callback_self_write_test.go`, on every
  door above (`PUT` loopback, `PUT` with a transition, `PATCH`, collection `PUT`,
  gRPC `EntityPatchRequest`):
  - `TestIfMatch_OwnEntityCallbackWriteIsKept` — rule 3 under `SYNC`,
    `ASYNC_SAME_TX` and before a `COMMIT_BEFORE_DISPATCH` segment;
  - `TestIfMatch_ChangeAfterReadIs409` — rule 4, the rival committing before
    the final save or before the first segment's flush and commit;
  - `TestIfMatch_StaleIs412` — rule 2, with no processor dispatched.
  - `TestIfMatch_StaleLoopbackPut412` — rule 2 on the loopback `PUT`, on the
    server the OpenAPI conformance validator records.
- Unit: `internal/domain/workflow/engine_ifmatch_test.go` and
  `engine_transition_aborted_test.go` — the check runs before any dispatch,
  its conflict carries no transaction-conflict marker, and the audit holds
  exactly `STATE_MACHINE_START` then `TRANSITION_ABORTED`.
