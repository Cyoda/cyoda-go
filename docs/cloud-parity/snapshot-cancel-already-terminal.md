# Cancelling a snapshot that has already ended is refused on both doors

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Behaviour

A cancel of an async search (snapshot) that is no longer `RUNNING` is refused
with `SEARCH_JOB_ALREADY_TERMINAL`, not retryable:

- **HTTP:** `PUT /search/async/{jobId}/cancel` answers `400` with
  `properties.currentStatus` (`SUCCESSFUL`, `FAILED` or `CANCELLED`) and
  `properties.snapshotId`. Unchanged.
- **gRPC:** the `EntitySnapshotSearchResponse` answering a
  `SnapshotCancelRequest` has `success` false, `error.code` `CLIENT_ERROR`,
  `error.message` starting `SEARCH_JOB_ALREADY_TERMINAL: `, and `status` the
  snapshot's own status: `status.snapshotId` the requested id, `status.status`
  the status it ended with.

A job that finishes while the cancel is landing is answered the same way. The
store's cancel is idempotent on a terminal job, so cyoda-go reads the job's
status after the cancel and answers from it: a cancel is reported only when
the job's status is `CANCELLED`.

## What changed

- gRPC answered `success` true for a snapshot that had already ended.
- On both doors, a job that finished between the status check and the cancel
  was answered as cancelled (`CANCELLED`) while its status stayed
  `SUCCESSFUL` or `FAILED`.

## Cloud today

Cloud's `SnapshotCancelRequestHandler`
(`backend/src/main/kotlin/net/cyoda/saas/messaging/handler/search/SnapshotCancelRequestHandler.kt`)
sets `success` to `cancelResult.isCancelled`, sets `status.status` to the
current status, and adds the warning `Snapshot not cancelled, current status is
'<status>'`. It sets no `error`. To align, Cloud:

1. Sets `error` (`code` `CLIENT_ERROR`, `message`
   `SEARCH_JOB_ALREADY_TERMINAL: snapshot by id=<id> is not running. current
   status=<status>`) when the snapshot was not cancelled. The warning may stay.
2. Decides "cancelled" from the snapshot's status after its cancel, not from
   the status read before it, on both doors.

## Tests

- Unit: `internal/domain/search/cancel_outcome_test.go`
  (`TestCancelAsync_JobThatFinishedFirstIsNotReportedCancelled`).
- gRPC: `internal/grpc/search_test.go`
  (`TestEntitySearch_SnapshotCancel_AlreadyTerminal`, and
  `TestEntitySearch_SnapshotCancel_Envelope` for a running snapshot).
- E2E (HTTP): `internal/e2e/search_test.go`
  (`TestAsyncSearch_Cancel_AlreadyCompleted`).
