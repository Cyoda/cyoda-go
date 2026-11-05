# An async search's `expirationDate` is when it stops being readable

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Behaviour

- **The date.** The status of an async search carries `expirationDate`: the
  instant from which the job is no longer readable.
  - **HTTP:** `GET /search/async/{jobId}/status` (`getAsyncSearchStatus`),
    member `expirationDate` of `AsyncSearchStatusDto` (required).
  - **gRPC:** the `EntitySnapshotSearchResponse` answering a
    `SnapshotGetStatusRequest` or a `SnapshotCancelRequest`, member
    `status.expirationDate` of `SearchSnapshotStatus`
    (`docs/cyoda/schema/search/SearchSnapshotStatus.json`).
- **Exact once it ends.** From `expirationDate` on, the job's status, results
  and cancel answer `404 SEARCH_JOB_NOT_FOUND` (gRPC: `CLIENT_ERROR`,
  `SEARCH_JOB_NOT_FOUND:` message), whether or not the store has deleted the
  job yet. Before it, they answer as usual. A client never finds a job gone
  before the date it was given.
- **While running, a lower bound.** A job that has not ended may report a
  date that moves later from one read to the next; it never moves earlier.
  The date a terminal job reports does not change.
- **One signal for "gone".** A job that does not exist, belongs to another
  tenant, or has expired answers `404 SEARCH_JOB_NOT_FOUND`. The status
  enum's `NOT_FOUND` stays in the contract (it grows by addition only), and a
  client treats it as that 404.

How the date is set is the server's policy. cyoda-go: `finishTime` +
`CYODA_SEARCH_SNAPSHOT_TTL` (default 1h), and now + that TTL while the job
runs. The background reap uses the same TTL.

## What changed

cyoda-go answered `createTime + 24h`, while it deleted a job
`CYODA_SEARCH_SNAPSHOT_TTL` after it finished, so a client that trusted the
date got 404 for up to 23 hours of the job's advertised life. A job past its
TTL also stayed readable until the next reap, and the gRPC status never set
`expirationDate`.

## Cloud today

Cloud already conforms. `SearchUsageEntityService.saveNewReport` stamps
`expirationTimestamp` = submit time + `maxAllowedTimeAliveForUnusedSnapshots`;
`getDistributedReportId` refuses a snapshot from that instant on, which
`EntitySearchFacade.getSnapshotStatus` answers as
`TreeNodeSnapshotNotFoundException` (404, gRPC `NOT_FOUND`); and the status
reports the same timestamp as `expirationDate` on both doors
(`CloudEventCommons.kt`). Its date is fixed at submit, which satisfies the
lower-bound rule. Its `NOT_FOUND` status (`ReportWrapperService`, a report
missing behind a live snapshot) is allowed by the rule above. No Cloud change
is needed.

## Tests

- Unit: `internal/domain/search/snapshot_expiry_test.go` — finish + TTL for a
  finished job, now + TTL for a running one, not found from the expiration on
  (status, results, cancel; HTTP 404), the reap uses the service's TTL.
- gRPC: `internal/grpc/search_test.go`
  (`TestEntitySearch_SnapshotGetStatus_ExpirationDate`).
- Parity: `e2e/parity/platform_clock.go`
  (`RunPlatformClockAsyncSearchExpiry`, memory/sqlite/postgres): the date is
  `finishTime` + TTL, and after the platform clock moves past it, status and
  results answer 404 with the reaper idle.
- Config: `app/config_validate_test.go` (a non-positive
  `CYODA_SEARCH_SNAPSHOT_TTL` or `CYODA_SEARCH_REAP_INTERVAL` is a startup
  error).
