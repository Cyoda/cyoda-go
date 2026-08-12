# Transaction-control params: `transactionTimeoutMillis`, `transactionSize`, search `timeoutMillis` — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's transaction-control parameters on entity/message writes and
deletes, and on direct search. cyoda-go is the authoritative implementation.

## Behaviour

### Fictional defaults removed

`transactionTimeoutMillis` and `transactionSize` were declared in the HTTP
OpenAPI spec (and `transactionSize` in the `EntityDeleteAllRequest` gRPC
CloudEvent schema) with `default:` values that the server never enforced —
fictional contract surface. The defaults are removed, not implemented:
absence of a param now means no server-side timeout / no batching, matching
what has always actually happened.

`EntityDeleteAllRequest.transactionSize` changes from a non-nullable `int`
with a baked default to a nullable field; the generated Go type
(`api/grpc/events/types.go`) changes from `int` to `*int`. This is a
compile-breaking change for any Go code importing `api/grpc/events` and
constructing `EntityDeleteAllRequest` literals that assign a bare `int` to
`TransactionSize` — accepted pre-1.0 (no backward-compatibility constraint on
`api`/`grpc`/`events` importers at this stage).

### Both params are now honored, opt-in

Absent parameter → behavior unchanged (single transaction / no timeout).
Present and valid → the described behavior below applies. This is a PATCH
release; no default changes existing behavior.

**`transactionTimeoutMillis`** (HTTP query, all entity write ops +
`newMessage`) / **`transactionTimeoutMs`** (gRPC
`EntityCreate/Update/Patch/CreateCollection/UpdateCollectionRequest`):
maximum time the server may spend before the first commit. Expiry before the
first commit rolls back and fails with `408 TRANSACTION_TIMEOUT` (HTTP) /
`CLIENT_ERROR` envelope prefixed `TRANSACTION_TIMEOUT: …`, `Retryable: true`
(gRPC) — nothing is committed. The guarantee is structural: commit calls
(including every commit-before-dispatch segment commit and `newMessage`'s
`Save`) run on a deadline-shielded context, so a timeout can never fire mid
commit and leave an in-doubt "408 but durable" outcome. Once a commit
succeeds, the response is success regardless of when the deadline later
fires.

For operations that commit more than once — chunked collection writes
(`create`, `createCollection`, `updateCollection` under `transactionWindow`)
and commit-before-dispatch cascades — the timeout only bounds time-to-first-
commit. After the first chunk/segment is durable, further expiry surfaces
through the existing per-chunk `200` contract (error elements carrying
`TRANSACTION_TIMEOUT`) instead of a 408.

**`transactionSize`** (HTTP query, `deleteEntities` / `deleteMessages`) /
gRPC `EntityDeleteAllRequest.transactionSize`: batches a bulk delete instead
of running it as one transaction / one store call.

- `deleteEntities`: matching ids and their current versions are resolved
  once, in one committed transaction (as-at `pointInTime` when supplied).
  Ids are then deleted in successive independent transactions of at most N
  ids each. Each batch re-reads every id's current version and deletes only
  if it still equals the resolution-time baseline; a version mismatch (the
  entity changed after resolution) or a per-id delete failure is recorded in
  `deleteResult.idToError`, not retried. A batch whose commit itself fails
  maps every id it attempted into `idToError` rather than counting them as
  removed. Batches already committed before a later failure stay committed —
  a deliberate, opt-in partial-commit contract. `deleteAllErrorsByID` on the
  gRPC delete-all response now carries these per-id failures for the first
  time (`transactionSize` sent).
- `deleteMessages`: the id list is chunked into successive `DeleteBatch`
  calls, one response element (`{entityIds, success}`) per batch; a failed
  batch does not stop later batches. Messages are immutable, so no version
  guard applies. Message delete is not part of an entity transaction and was
  already documented as non-transactional; batching does not change that.
  The memory backend's `DeleteBatch` aborts on the first per-id error, so a
  batch reporting `success: false` may have already removed some of its ids
  (partial application within the batch) — postgres deletes a batch as one
  `ANY(...)` statement (atomic); sqlite splits batches over 500 ids into
  internal chunked `DELETE ... IN (...)` statements, so the same partial-
  application caveat applies there once a batch exceeds that internal
  chunk size.

### 408s and two new error codes

`errors.TRANSACTION_TIMEOUT` (408, retryable) covers entity writes and
`newMessage`. `errors.SEARCH_TIMEOUT` (408, retryable) covers direct search.
Both are `application/problem+json` on HTTP and `CLIENT_ERROR` envelopes
(`TRANSACTION_TIMEOUT: …` / `SEARCH_TIMEOUT: …` prefix, `Retryable: true`)
on gRPC. Classification is deliberately narrow: a 408 requires the executing
node's own deadline marker on the error, the marked context itself currently
expired, and the error not carrying the shielded-commit's in-doubt sentinel.
A raw client disconnect (`context.Canceled`) is never a 408. Pre-existing
timeout sources keep their own codes (postgres `statement_timeout` → 500,
dispatch timeout → 503).

### Search `timeoutMillis` re-added

`searchEntities` regains the optional `timeoutMillis` (int64, no default)
query parameter and `408 SEARCH_TIMEOUT` response, completing the intent
recorded when the previously-fictional param was removed in v0.8.2. Enforced
uniformly across memory, sqlite, and postgres backends (postgres was already
cancellation-aware).

### Joined-request rejection

Every transaction-control parameter (`transactionTimeoutMillis`,
`transactionSize`, search `timeoutMillis`) is rejected with `400 BAD_REQUEST`
(HTTP) / `CLIENT_ERROR` `BAD_REQUEST: …` (gRPC) when present on a request
that joins an existing transaction (a tx-token'd request — how a routed
compute-node callback or a cross-node forwarded request presents at
param-resolution time). Honoring any of these on a joiner is impossible
without corrupting the transaction owner's guarantees: a joiner's deadline
would poison a connection it does not own, and a joiner cannot commit per
batch (`commitOwned` no-ops when the caller does not own the transaction).
This is a uniform rule with no per-operation carve-outs, on both HTTP and
gRPC.

## Invariant Cloud must mirror

1. `TRANSACTION_TIMEOUT` / `408` guarantees nothing was committed; `Cloud`
   must not produce this code for any outcome where a commit already
   succeeded.
2. Timeout enforcement bounds only time-to-first-commit on multi-commit
   operations; expiry after the first commit must surface through the
   existing partial-success/per-chunk contract, never as a top-level 408.
3. `transactionSize`-batched deletes are partial-commit: batches committed
   before a failure remain committed and are reported as removed; failed or
   version-conflicted ids are reported per-id, never silently dropped or
   retried.
4. Any transaction-control parameter on a joined/forwarded request is a hard
   `400`, never silently honored or silently ignored.
5. No parameter changes default (absent) behavior — this is a PATCH-safe,
   strictly opt-in contract.

## Backend support

All three in-tree backends (memory, sqlite, postgres) support timeout
enforcement and batched delete identically; cross-backend parity tests cover
the batched-delete final-state scenarios and the joined-request rejection.
The memory/sqlite partial-application caveat on `deleteMessages` batches is a
backend implementation detail of an already-non-transactional operation, not
a parity gap — postgres's atomic per-batch statement is the strictly
stronger case, not the baseline other backends must match.
