# A change during a `COMMIT_BEFORE_DISPATCH` callout is `412 ENTITY_MODIFIED`

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Behaviour

A `COMMIT_BEFORE_DISPATCH` processor commits the write's first segment before
its callout. The engine then applies the processor's result in a second
transaction by compare-and-save, against the version the first segment
committed (or the second transaction's own write, when a callback wrote the
entity there). With `startNewTxOnDispatch: true` the engine also checks, before
the dispatch, that the entity is still that version.

If another transaction changed the entity in between, the write fails:

- **Answer:** `412`, error code `ENTITY_MODIFIED`, not retryable.
  `properties.entityId` names the entity. The detail is the fixed
  `ENTITY_MODIFIED: entity has been modified since last read`; the engine's
  own text (which names internal transaction ids) is not in the response.
- **Doors:** every entity write that runs the workflow engine. On HTTP: create
  (single and array body), create collection, update (loopback or transition,
  PUT or PATCH), update collection. On gRPC: `EntityManage` (create, update,
  patch, transition) and `EntityManageCollection` (create, update), as the
  `CLIENT_ERROR` envelope whose message is that detail.
- **Collections:** the request fails whole. No later item runs, and the item is
  never isolated as a per-item `failed[]` entry, with or without an `ifMatch`.
  In a later chunk of a chunked HTTP collection request the failure is that
  chunk's error element (`code: ENTITY_MODIFIED`), as every chunk-wide failure
  is.
- **What stays:** the first segment's commit, and the other transaction's
  change. No engine-side retry and no compensation. Not retryable as-is: the
  callout already fired.
- **Precedence:** a transaction that an earlier conflict aborted still answers
  the retryable `409 CONFLICT`, never `412`.

## What changed

Create and create collection (HTTP and gRPC) and the HTTP update collection
answered `400 WORKFLOW_FAILED` for this conflict, with the engine's text as the
detail. The single-entity doors and the gRPC update collection, which updates
item by item, answered `412`. All write doors now answer as the single update
does.

## Cloud today

Cloud has no `COMMIT_BEFORE_DISPATCH` execution mode (no occurrence of
`COMMIT_BEFORE_DISPATCH` or `startNewTxOnDispatch` in the Cloud tree), so the
conflict cannot arise there yet. When Cloud adopts the mode it adopts this
answer with it, on every door.

## Tests

- E2E (PostgreSQL): `internal/e2e/cbd_post_segment_conflict_test.go`
  (`TestCreate_CBDPostSegmentConflict_Is412`,
  `TestCreateCollection_CBDPostSegmentConflict_Is412`) and
  `internal/e2e/collection_update_ifmatch_test.go`
  (`TestUpdateCollection_IfMatch_CBDPostSegmentConflictAbortsChunk`).
- gRPC: `internal/grpc/post_segment_conflict_rpc_test.go`
  (`TestRPC_PostSegmentConflict_IsEntityModified`) — create, create
  collection, update and update collection.
- Unit: `internal/domain/entity/service_rollback_test.go` — the `412` and
  `entityId` on create, create collection, update and update collection.
