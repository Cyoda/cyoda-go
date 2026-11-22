# An async search that failed says why (`failureReason`)

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Behaviour

The status of an async search carries an optional `failureReason` (string).

- **Present** only when the status is `FAILED`. **Absent** for `RUNNING`,
  `SUCCESSFUL`, `CANCELLED` and `NOT_FOUND`. It is never `null` and never an
  empty string.
- **HTTP:** `GET /search/async/{jobId}/status` (`getAsyncSearchStatus`),
  member `failureReason` of `AsyncSearchStatusDto`.
- **gRPC:** the `EntitySnapshotSearchResponse` answering a
  `SnapshotGetStatusRequest` (and a refused `SnapshotCancelRequest`), member
  `status.failureReason` of `SearchSnapshotStatus`
  (`docs/cyoda/schema/search/SearchSnapshotStatus.json`).
- **Text.** The same rule as an error response:
  - A failure the request itself caused carries the client error a direct
    search would answer for it, error code first — for example
    `INVALID_FIELD_PATH: condition or sort references an invalid field path`.
  - A server-side failure carries `SERVER_ERROR: internal error [ticket:
    <uuid>]`, as an error response does, with its detail logged under the
    ticket.
  - A storage outage, a lost write race or a lost shard carries its own code,
    as an error response does — for example `STORAGE_UNAVAILABLE: storage is
    temporarily unavailable — retry` — with its cause logged.
  - A known cause carries a fixed text — `search abandoned: executor lost
    repeatedly` (a job whose executor was lost too many times) or the ceiling
    text. The detail goes to the server log. No driver message, query text,
    stack trace or connection detail appears in it.
  - A storage backend that runs async searches itself records its own text,
    which cyoda-go has not sanitised; for such a backend every failed job
    reports `search failed unexpectedly`.
  - The leading error code, when there is one, is stable. The rest of the
    text is for people, not for parsing.

The status read does no new work: it serves the reason recorded when the job
was failed.

## What changed

The status had no failure field, so a caller saw `FAILED` and no reason.
cyoda-go already recorded a sanitised reason on every failed job; it is now
served.

## Cloud today

Cloud's `SnapshotStatusDto`
(`tree-node/tree-node-backend/src/main/kotlin/com/cyoda/tdb/dto/search/SnapshotStatusDto.kt`)
has no failure field (its members are `snapshotStatus`, `expirationDate`,
`entitiesCount`, `calculationTimeMillis`, `createTime`, `finishTime`), and
Cloud's copy of `SearchSnapshotStatus.json`
(`client/src/main/resources/schema/search/`) has no `failureReason`. To align,
Cloud:

1. Adds `failureReason: String?` to `SnapshotStatusDto`, omitted from the
   JSON when null, and sets it only for `FAILED`.
2. Adds `failureReason` to its `SearchSnapshotStatus.json` and sets it on the
   gRPC status answer on the same rule.
3. Fills it with text that follows the rule above: a client error's own
   message, code first, or a fixed non-revealing text — never the raw
   exception message of a failed shard or a storage call.

## Tests

- Unit: `internal/domain/search/failure_reason_test.go`
  (`TestAsyncStatus_FailureReason`: the service status and the transport
  summary carry the reason for `FAILED` only) and
  `internal/domain/search/status_response_internal_test.go`
  (`TestBuildStatusResponse_FailureReason`: the HTTP body).
- gRPC: `internal/grpc/snapshot_failure_reason_test.go`
  (`TestRPC_SnapshotGetStatus_FailureReason`).
- E2E (PostgreSQL): `internal/e2e/storage_ceilings_e2e_test.go`
  (`TestE2E_AsyncSearch_CeilingExceeded_ServesSanitizedFailureReason`),
  `internal/e2e/async_stream_test.go` (`TestE2E_AsyncSearch_AttemptCap_Fails`)
  and `internal/e2e/async_search_failure_reason_test.go`
  (`TestE2E_AsyncSearch_Successful_HasNoFailureReason`).
- Parity: `e2e/parity/async_failure_reason.go`
  (`AsyncStatusNoFailureReasonUnlessFailed`). It covers the `SUCCESSFUL`
  side only: no public request makes an async search fail on every backend,
  because submission checks the condition, sort keys and limits first.
