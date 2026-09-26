# Model and workflow administration never runs inside a transaction

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Behaviour

A compute node's callback — a request carrying a transaction token (`X-Tx-Token`
on HTTP, `tx-token` metadata on gRPC) — runs in the transaction of the operation
that called the compute node out. Entity operations join it. Model and workflow
administration does not: a model's schema, its lock state, its change level, its
unique keys, its existence and its workflows are configuration, changed by an
operation of their own and never as a step of a running transition.

- **Refused operations.** On HTTP: `importEntityModel`, `deleteEntityModel`,
  `setEntityModelChangeLevel`, `lockEntityModel`, `unlockEntityModel`,
  `setEntityModelUniqueKeys`, `importEntityModelWorkflow`. On gRPC:
  `entityModelManage` with `EntityModelImportRequest`,
  `EntityModelTransitionRequest`, `EntityModelDeleteRequest` or
  `EntityModelSetUniqueKeysRequest`.
- **When:** first. On HTTP the join layer answers before the token is verified,
  so no transaction lock is taken, the body is not read and nothing is written;
  a forged or expired token on one of these routes gets this answer, not `401`
  or `410`. On gRPC the handler answers before the payload is read. The
  transaction the token names is untouched either way.
- **Answer:** `400`, error code `MODEL_ADMIN_IN_JOINED_TRANSACTION`, not
  retryable, with a message saying that model and workflow administration
  cannot run inside a transaction and that the request is to be made without
  the token. Over gRPC the code is the message prefix of the `CLIENT_ERROR`
  envelope of that request's own response type (`Success: false`).
- **Not refused:** the read-only model and workflow operations — list, export,
  validate, workflow export; `EntityModelExportRequest` and
  `EntityModelGetAllRequest` — and every entity operation, which joins as
  before.
- **Consequence for workflow import:** it always owns its transaction. Its
  removal of the tasks no workflow schedules any more runs in a transaction of
  its own and is retried on a scheduler race; the joined form of that `409
  CONFLICT` no longer exists. The published API document no longer describes
  it.
- **Remedy the error points to:** the compute node makes the administration
  request as an independent request, without the transaction token.

## Cloud today

Cloud does not describe a transaction-token door on its model or workflow
administration endpoints. Aligning means answering a request that carries one
with this status and code, before any part of the change is applied, on every
administration operation Cloud exposes, and leaving the read-only operations
and the entity operations as they are.
