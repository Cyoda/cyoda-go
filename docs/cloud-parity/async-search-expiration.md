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
  job yet. Before it, they answer as usual. Up to the clock skew between
  nodes, a client never finds a job gone before the date it was given. The
  date a terminal job reports does not change.
- **While running, a lower bound.** A job that has not ended reports a date
  no later than the one it will end with, and is not refused for expiry
  while it runs.
- **One signal for "gone".** A job that does not exist, belongs to another
  tenant, or has expired answers `404 SEARCH_JOB_NOT_FOUND`. The status
  enum's `NOT_FOUND` stays in the contract (it grows by addition only), and a
  client treats it as that 404.

How the date is set is the server's policy. cyoda-go: `finishTime` +
`CYODA_SEARCH_SNAPSHOT_TTL` (default 1h), and `createTime` + that TTL while
the job runs (now + TTL is not a lower bound: the executor stamps the finish
time before it writes the terminal status, so a read in that gap sees RUNNING
with the finish already behind it). The background reap uses the same TTL.

## What changed

cyoda-go answered `createTime + 24h`, while it deleted a job
`CYODA_SEARCH_SNAPSHOT_TTL` after it finished, so a client that trusted the
date got 404 for up to 23 hours of the job's advertised life. A job past its
TTL also stayed readable until the next reap, and the gRPC status never set
`expirationDate`.

## Cloud today

Cloud's HTTP door conforms on the date and the refusal, except for the
running-job refusal below.
`SearchUsageEntityService.saveNewReport` stamps `expirationTimestamp` = submit
time + `maxAllowedTimeAliveForUnusedSnapshots` (that field holds, in
milliseconds, the duration property
`com.cyoda.tdb.search.maxAllowedTimeAliveForSnapshots`, default 1 day);
`getDistributedReportId` refuses a snapshot from that instant on, on
status, cancel and results (`EntitySearchFacade`), with
`TreeNodeSnapshotNotFoundException`; and the status reports the same
timestamp as `expirationDate` on both doors (`CloudEventCommons.kt`). Its
date is fixed at submit, which satisfies the date rules above. Its `NOT_FOUND`
status (`ReportWrapperService`, a report missing behind a live snapshot) is
allowed by the rule above. On HTTP the exception answers 404, but its problem
detail carries only `properties.snapshotId` (`TdbRestControllerAdvice`), with
no `properties.errorCode`. To align, Cloud sets `properties.errorCode`
`SEARCH_JOB_NOT_FOUND` on that 404.

gRPC does not. `CloudEventsApiGrpcService` maps
`TreeNodeSnapshotNotFoundException` to `Status.NOT_FOUND` and ends the call
with `responseObserver.onError`, a transport-level gRPC error, for an unknown
snapshot as for an expired one. cyoda-go answers both with an
`EntitySnapshotSearchResponse` envelope. To align, Cloud answers a
`SnapshotGetStatusRequest`, `SnapshotGetRequest` or `SnapshotCancelRequest`
for an unknown or expired snapshot with that envelope: `success` false,
`error.code` `CLIENT_ERROR`, `error.message` starting
`SEARCH_JOB_NOT_FOUND: `, not retryable.

Cloud also refuses a snapshot that is still running once its expiration has
passed: `getDistributedReportId` checks the timestamp whatever the report's
status (only the cleaner skips running reports). That breaks "not refused for
expiry while it runs" for a job running longer than
`maxAllowedTimeAliveForSnapshots`. To align, Cloud refuses for expiry only a
snapshot whose report has ended.

All three items: CP-3994.

## Storage backends

The in-tree backends remove a job only on `DeleteJob` or `ReapExpired`, and
stamp a finish time on every terminal write. The SPI states both
(Cyoda/cyoda-go-spi#76, pinned). The commercial backend does not yet: a job cancelled
before any shard finished has no finish time, a failed job's finish time can
move, and its rows expire on a timer of their own (Cyoda/cyoda-go-cassandra#120,
before its v0.9.0 bump).

## Tests

- Unit: `internal/domain/search/snapshot_expiry_test.go` — finish + TTL for a
  finished job, create + TTL for a running one (never refused while it runs),
  not found from the expiration on (status, results, cancel; HTTP 404), a
  terminal job without a finish time fails, the reap uses the service's TTL.
- gRPC: `internal/grpc/search_test.go`
  (`TestEntitySearch_SnapshotGetStatus_ExpirationDate`).
- Parity: `e2e/parity/platform_clock.go`
  (`RunPlatformClockAsyncSearchExpiry`, memory/sqlite/postgres): the date is
  `finishTime` + TTL, and after the platform clock moves past it, status,
  results and cancel answer 404 with the reaper idle.
- No separate `internal/e2e` test: the parity scenario drives the full HTTP
  stack on PostgreSQL, and the expiry needs the platform clock it sets up.
- Config: `app/config_validate_test.go` (a non-positive
  `CYODA_SEARCH_SNAPSHOT_TTL` or `CYODA_SEARCH_REAP_INTERVAL` is a startup
  error).
