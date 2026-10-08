# Consistency time and the point-in-time fence — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's point-in-time reads. cyoda-go is the authoritative implementation.

A point-in-time read never gives an answer that can change later, and a read
whose instant the server chooses includes every save already confirmed. The
contract adds one endpoint, one gRPC request pair and two error codes, and makes
every point-in-time read refuse an instant that is not yet final.

## The consistency time

The **consistency time** `C` is an instant in the store's stamp domain, returned
by the store for a tenant:

1. **Complete.** Every save, of any tenant, whose success was returned on any
   node before the request for `C` started, has a stamp `<= C`.
2. **Final.** A read for the requesting tenant at `T <= C` that starts after `C`
   was returned sees every save of that tenant stamped `<= T`, and always will.
   A save not yet stamped when `C` is returned is stamped `> C`.
3. **Monotonic.** Every `C` returned, for any tenant on any node, is `>=` every
   `C` returned before its request started, across a restart too.
4. **Read resolution.** `C` covers the store's read unit: a store that widens
   `T` to a coarser unit when it reads returns a `C` that closes that whole
   unit. `C` is never rounded up when rendered.

Completeness and monotonicity hold backend-wide (the stamp floor is shared by
every tenant); finality is per tenant, because reads are. The mechanism on every
backend is **reserve, then wait**: raise the stamp floor to
`C = max(store clock, highest stamp issued)`, so every later save stamps above
`C`, then wait until every save of the tenant that already holds a stamp `<= C`
has finished committing or aborted. A store that cannot do that within its wait
budget fails the request; it never returns a guessed instant.

## The fence

A read with `pointInTime = T` compares `T` with `C` before it runs. `T <= C`:
the read runs at `T`, and its answer is final. `T > C`: the read is refused with
`400 POINT_IN_TIME_AFTER_CONSISTENCY_TIME`. There is no waiting.

| Operation | gRPC | Absent `pointInTime` | `pointInTime = T` |
|---|---|---|---|
| `GET /entity/{entityId}` | `EntityGetRequest` | current revision | fenced |
| `GET /entity/{entityName}/{modelVersion}` | `EntityGetAllRequest` | current page | fenced |
| `POST /search/direct/{entityName}/{modelVersion}` | `EntitySearchRequest` | current | fenced |
| `POST /search/async/{entityName}/{modelVersion}` | `EntitySnapshotSearchRequest` | a fresh `C`, recorded on the job | fenced at submit; `T` recorded |
| `GET /search/async/{jobId}...` | `SnapshotGetStatusRequest`, `SnapshotGetRequest` | read at the job's instant, not fenced (final since submit) | — |
| `DELETE /entity/{entityName}/{modelVersion}` | `EntityDeleteAllRequest` | selects what exists now | fenced |
| `GET /entity/stats`, `/entity/stats/{entityName}/{modelVersion}` | `EntityStatsGetRequest` | current counts | fenced |
| `GET /entity/stats/states`, `/entity/stats/states/{entityName}/{modelVersion}` | `EntityStatsByStateGetRequest` | current counts | fenced |
| `POST /entity/stats/{entityName}/{modelVersion}/query` | — | current | fenced |
| `GET /entity/{entityId}/changes` | `EntityChangesMetadataGetRequest` | full history | fenced |
| `GET /entity/{entityId}/transitions` | — | from the current revision | fenced; a `transactionId` is fenced at that transaction's commit stamp |

Reads with no `pointInTime` read the current state: what is committed when the
store runs the query. The consistency time is not involved, except at async
submit. **Pages of a list read without `pointInTime` are read at different
moments.** A client that needs consistent pages takes `C` once and passes it as
`pointInTime` on every page.

`GET /entity/{entityId}?transactionId=` is not fenced (a committed revision is
immutable). The audit trail is not fenced either: it is a time window over a
growing log.

**Where the fence runs.** After every check that does not itself read the
instant, and whether or not a store read follows (a page size of 0, a tenant
with no models and an empty state list are fenced too). Existing request errors
— a malformed instant, `pointInTime` with `transactionId`, an unknown or
unregistered model, an invalid grouped-stats path, the async per-tenant cap
pre-check — keep precedence over the refusal. A `404 ENTITY_NOT_FOUND` that the
read itself produces (get by id, change history, transitions) comes after it: a
future `T` on a missing entity answers `400`.

**Inside a transaction.** Without `pointInTime`, a read sees the transaction's
snapshot plus its own writes: the committed state as it was when the transaction
began. A commit that completes later is not visible to it, also when it reads
again. The timing per backend is in
`docs/submit-times-snapshots-consistency-time.html`. With `pointInTime`, it sees
committed data at `T` only, and is fenced the same way.

## The endpoint and the gRPC pair

**`GET /entity/consistency-time`** (`getConsistencyTime`, `ROLE_M2M`, tenant
from the token, no parameters). `200`:

```json
{ "consistencyTime": "2026-10-05T14:03:07.123456Z" }
```

`consistencyTime` is `format: date-time`, RFC 3339 with fractional seconds at
the store's full precision, never rounded. A read at that instant, on any node,
is never refused, and includes every save confirmed before the call.

**gRPC.** `EntityConsistencyTimeGetRequest` (base event fields only) returns
`EntityConsistencyTimeResponse` with `consistencyTime` set when `success` is
true; a failure carries `error` instead, as the other responses do. It is served
on the unary `EntitySearch` RPC. Authentication and role failures stay transport
errors.

## Error codes

| Code | HTTP | Retryable | When | Detail |
|---|---|---|---|---|
| `POINT_IN_TIME_AFTER_CONSISTENCY_TIME` | 400 | no | `T > C` on a fenced read | `properties.consistencyTime` is the current `C`; the message states it too |
| `CONSISTENCY_TIME_UNAVAILABLE` | 503 | yes | the store could not certify `C` within its wait budget (a save of the tenant held in its commit phase), or a store call ran past its own deadline | — |

A store failure that carries the storage-unavailable marker keeps the existing
`503 STORAGE_UNAVAILABLE` (retryable). Any other failure is `500` with a ticket.

**gRPC envelope.** Both codes follow the operational-error convention:
`Success: false`, `Error.Code: CLIENT_ERROR`, the domain code as the message
prefix (`POINT_IN_TIME_AFTER_CONSISTENCY_TIME:`, `CONSISTENCY_TIME_UNAVAILABLE:`)
and `Retryable` set for the `503`. The refusal message carries `C`, because the
envelope has no properties. A ticketed failure is `SERVER_ERROR`.

Async submit with no `pointInTime` can answer the two `503` codes (it gets a
fresh `C`); it cannot answer the `400`.

## Conditional delete

`DELETE /entity/{entityName}/{modelVersion}` with `pointInTime` selects the
entities that existed at `T` and deletes their current rows. It has **no
match-count limit**. Cloud's `entitySearchLimit` on delete-by-condition at a
point in time is not part of this contract. A client that wants to know how many
entities a delete will touch counts at the instant (the stats read with the same
`pointInTime`), then deletes at the same instant.

## Cloud differences to close

- Cloud's consistency time is final but **not complete**: it can miss a save
  already confirmed before the request.
- Cloud serves reads later than its consistency time **unfenced**; the contract
  refuses them with `400 POINT_IN_TIME_AFTER_CONSISTENCY_TIME`.
- Cloud has **no consistency-time endpoint** and no gRPC pair; both are added.

## Backend support

The contract is on `spi.TransactionManager.ConsistencyTime`, with
`Count`/`CountByState` taking an `asAt` instant and `GetVersionMetadata` reading
committed data only. memory, sqlite and postgres implement it and are covered by
the shared `spitest` group `ConsistencyTime` and the cross-backend parity
scenarios; the engine's fence (`internal/domain/consistency`) sits above the
SPI, so the refusal is identical on every backend.
