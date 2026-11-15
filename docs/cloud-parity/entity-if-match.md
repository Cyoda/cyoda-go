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
2. A mismatch answers `412 ENTITY_MODIFIED`. Inside the update's
   transaction the engine records `STATE_MACHINE_START` and then
   `TRANSITION_ABORTED`
   (`{eventType, reason: "ENTITY_MODIFIED", transition, expectedTxId, actualTxId}`;
   `transition` is absent on a loopback, which has no transition),
   and nothing else of the transition. Audit events are bound to the
   transaction, so what the audit log keeps depends on the door:
   - `PUT` and `PATCH` of one entity, and gRPC `EntityPatchRequest`: the
     transaction rolls back, and neither event is kept.
   - Collection `PUT`: the item is listed in `failed[]`, its siblings commit,
     and so do those two events.
3. A write to the entity later in the same transaction is the update's own and
   does not break the precondition: the engine's save of the entity before a
   callout that carries the transaction token, or a `COMMIT_BEFORE_DISPATCH`
   segment's own commit. A processor's callback never writes the entity the
   workflow runs for; a processor changes it only through the data it
   returns.
4. A change that another transaction commits after the update's read is a lost
   race, not a failed precondition: the update answers the retryable
   `409 CONFLICT`, and the transaction that lost the race does not commit. On
   the collection door the whole request answers `409`; the item is not
   isolated.
5. The same on every storage backend.

One case answers `412 ENTITY_MODIFIED` outside this rule, with or without
`If-Match`: a `COMMIT_BEFORE_DISPATCH` processor committed the update's first
segment before its callout, and another transaction changed the entity before
the processor's result was applied. Every write door answers `412`, and a
collection request fails whole; see `post-segment-conflict.md`. The committed
segment stays.

## What changed

- Before, the precondition was compared at the update's final save (or at the
  first `COMMIT_BEFORE_DISPATCH` segment's flush), against the entity as the
  transaction then saw it. A write the update's own transaction had made to
  the entity made such an update answer `412` although nobody else had
  written the entity.
- A change committed by another transaction after the update's read could
  answer `412` (and be isolated as a collection item). It is now `409` on every
  backend.
- A stale `If-Match` was found only after the transition had run, or at its
  first segment flush, so on the collection door the audit kept the
  transition's own events (`WORKFLOW_FOUND`, `TRANSITION_MAKE`, …) before
  `TRANSITION_ABORTED`. Now only `STATE_MACHINE_START` precedes it. The
  single-update doors kept no event before and keep none now.

## Tests

- Parity (memory, SQLite, PostgreSQL): `e2e/parity/ifmatch_lost_race.go`
  (`IfMatch_ChangeAfterReadIs409`) — rule 4 on the loopback `PUT`: another
  client's write committed during the callout, before the final save or
  before the first segment's flush and commit, is a retryable `409`, and the
  entity keeps that write.
- E2E (PostgreSQL): `internal/e2e/ifmatch_doors_test.go`, on every door above
  (`PUT` loopback, `PUT` with a transition, `PATCH`, collection `PUT`, gRPC
  `EntityPatchRequest`):
  - `TestIfMatch_ChangeAfterReadIs409` — rule 4, the rival committing before
    the final save or before the first segment's flush and commit;
  - `TestIfMatch_StaleIs412` — rule 2, with no processor dispatched.
  - `TestIfMatch_StaleLoopbackPut412` — rule 2 on the loopback `PUT`, on the
    server the OpenAPI conformance validator records.
- Parity (memory, SQLite, PostgreSQL):
  `e2e/parity/externalapi/transition_aborted_audit.go`
  (`ExternalAPI_05_TransitionAbortedAuditEventPaired`) — rule 2 on a single
  `PUT`: `412`, and the audit log keeps neither event.
- E2E (PostgreSQL): `internal/e2e/collection_update_ifmatch_test.go`
  (`TestUpdateCollection_IfMatch_PerItemStaleIsolated`) — rule 2 on the
  collection door: the item is in `failed[]` and the audit log keeps
  `STATE_MACHINE_START` and `TRANSITION_ABORTED`.
- Unit: `internal/domain/workflow/engine_ifmatch_test.go` and
  `engine_transition_aborted_test.go` — the check runs before any dispatch,
  its conflict carries no transaction-conflict marker, and the transaction,
  read before it rolls back, holds exactly `STATE_MACHINE_START` then
  `TRANSITION_ABORTED`.
