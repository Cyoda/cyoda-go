# Consistency time for point-in-time reads

Issues: #649 (this design), #581 (closed by it). Facts, with file:line
evidence: `docs/superpowers/research/2026-10-04-649-consistency-time-research.md`.
Agreed design brief: `docs/superpowers/research/2026-10-04-649-design-brief.md`.
Per-endpoint matrix (shared page): https://claude.ai/artifact/E8BCsvDpJKJWhnpZm2CAcL

## 1. Goal

A point-in-time read never gives an answer that can change later. A read whose
instant cyoda-go chooses includes every save already confirmed. Both hold on
every backend and on every node of a cluster.

## 2. The consistency time

The **consistency time** `C` is an instant in the store's stamp domain,
returned by the store for the caller's tenant:

1. **Complete.** Every save of that tenant whose success was returned, on any
   node, before the request for `C` started, has a stamp `≤ C`.
2. **Final.** A read at `T ≤ C` that starts after `C` was returned sees every
   save of that tenant stamped `≤ T`, and always will. A save not yet stamped
   when `C` is returned is stamped `> C`.
3. **Read resolution.** `C` covers the store's read unit. A store that widens
   `T` to a coarser unit when it reads (cassandra: the whole millisecond)
   returns a `C` that closes that whole unit.

Mechanism, for every backend: **reserve, then wait.** Raise the stamp floor to
`C = max(store clock, highest stamp issued)`, so every later save stamps above
`C`. Then wait until every save of the tenant that already holds a stamp `≤ C`
has finished committing or has aborted.

## 3. Read semantics (the contract)

### 3.1 Rules

1. **No `pointInTime`: current state.** The store answers from what is
   committed when it runs the query. `C` is not involved. Exception: **async
   search submit** takes a fresh `C` and records it on the job.
2. **`pointInTime = T`: fenced.** Before the read runs, `T` is compared with
   `C`. `T ≤ C`: the read runs at `T`, and its answer is final. `T > C`: the
   read is refused with `400 POINT_IN_TIME_AFTER_CONSISTENCY_TIME`, whose
   detail carries the current `C`. There is no waiting.
3. **Inside a transaction.** With no `pointInTime`, a read sees the current
   committed state plus the transaction's own writes, as today. With a
   `pointInTime`, it sees committed data at `T` only, and is fenced as in
   rule 2.
4. **`GET /entity/consistency-time`** returns a fresh `C` for the caller's
   tenant. A read at that instant is never refused, and it includes every
   save confirmed before the call.

### 3.2 Per operation

| Operation | gRPC | No `pointInTime` | `pointInTime = T` | Other instant |
|---|---|---|---|---|
| `GET /entity/{entityId}` | `EntityGetRequest` | current revision | fenced; revision at `T` | `transactionId`: the revision that transaction wrote; not fenced (a committed revision is immutable) |
| `GET /entity/{entityName}/{modelVersion}` | `EntityGetAllRequest` | current page | fenced; page at `T` | — |
| `POST /search/direct/{entityName}/{modelVersion}` | `EntitySearchRequest` | current | fenced; at `T` | — |
| `POST /search/async/{entityName}/{modelVersion}` | `EntitySnapshotSearchRequest` | **fresh `C`, recorded on the job** | fenced at submit; `T` recorded on the job | — |
| `GET /search/async/{jobId}…` (status, results) | `SnapshotGetStatusRequest`, `SnapshotGetRequest` | reads at the job's instant; not fenced (final since submit) | — | — |
| `DELETE /entity/{entityName}/{modelVersion}` | `EntityDeleteAllRequest` | selects what exists now | fenced; selects what existed at `T` | — |
| `GET /entity/stats`, `/entity/stats/{entityName}/{modelVersion}` | `EntityStatsGetRequest` | current counts | fenced; counts at `T` | — |
| `GET /entity/stats/states`, `/entity/stats/states/{entityName}/{modelVersion}` | `EntityStatsByStateGetRequest` | current counts | fenced; counts at `T` | — |
| `POST /entity/stats/{entityName}/{modelVersion}/query` | — | current | fenced; at `T` | — |
| `GET /entity/{entityId}/changes` | `EntityChangesMetadataGetRequest` | full history | fenced; history up to `T` | — |
| `GET /entity/{entityId}/transitions` | — | from the current revision | fenced; from the revision at `T` | `transactionId`: at that transaction's commit stamp; fenced |
| audit trail | — | unchanged: a time window over a growing log, not fenced | — | — |
| `GET /entity/consistency-time` (new) | `EntityConsistencyTimeGetRequest` (new) | fresh `C` | — | — |

Today, the four HTTP stats reads, the two gRPC stats reads and gRPC get-all
accept `pointInTime` and ignore it. They honour it after this change.

**List paging.** Pages of a list taken with no `pointInTime` are read at
different moments. A client that needs consistent pages takes `C` once and
passes it as `pointInTime` on every page. `cyoda help crud` says so.

### 3.3 Check order

The fence runs after the request's existing checks (parse, mutual exclusion of
`pointInTime` and `transactionId`, model lookup and registration, grouped-stats
path validation) and immediately before the store read. An existing `4xx`
keeps precedence over the refusal.

## 4. API additions

### 4.1 HTTP

`GET {context}/entity/consistency-time`, operationId `getConsistencyTime`, tag
of the entity group. Authenticated, `ROLE_M2M`, tenant from the token (no path
tenant). No parameters, no `X-Tx-Token`.

`200` body `ConsistencyTimeDto`:

```json
{ "consistencyTime": "2026-10-05T14:03:07.123456Z" }
```

`consistencyTime`: `string`, `format: date-time`, required, RFC 3339 with the
store's full precision (microseconds on memory, sqlite and postgres).

Errors: `401`, `403` (shared components), `503 CONSISTENCY_TIME_UNAVAILABLE`,
`503 STORAGE_UNAVAILABLE`, `500`.

### 4.2 gRPC

`EntityConsistencyTimeGetRequest` (no fields beyond the base event) →
`EntityConsistencyTimeResponse` (`consistencyTime`, date-time, required), on
the unary `EntitySearch` RPC. Schemas in `docs/cyoda/schema/search/`, both
names in `CloudEventType.json`, constants in `internal/grpc/cloudevent_types.go`,
types regenerated with `scripts/generate-events.sh`.

### 4.3 Error codes

| Code | HTTP | Retryable | When | Detail |
|---|---|---|---|---|
| `POINT_IN_TIME_AFTER_CONSISTENCY_TIME` | 400 | no | `T > C` on a fenced read | `properties.consistencyTime` = current `C`; the message also states it |
| `CONSISTENCY_TIME_UNAVAILABLE` | 503 | yes | the store could not certify `C` within its wait budget (a save held in its commit phase) | — |

A store failure that carries the storage-unavailable marker keeps the existing
`503 STORAGE_UNAVAILABLE`; any other failure is `500` with a ticket. On gRPC
both codes follow the existing convention (`Error.Code = CLIENT_ERROR` or
`SERVER_ERROR`, the domain code as the message prefix, `Retryable` set); the
refusal message carries `C` because the envelope has no properties.

Each code gets `internal/common/error_codes.go` + `knownErrorCodes`, a
`cmd/cyoda/help/content/errors/<CODE>.md` topic and an `errors.md` index line.

## 5. Storage interface (cyoda-go-spi)

1. **`TransactionManager.ConsistencyTime(ctx context.Context) (time.Time, error)`**,
   required. Tenant from `ctx`. The doc states §2 in full, that it never
   returns a guessed instant, and that it is safe to call from a context that
   holds a transaction (it never uses the transaction).
2. **`ErrConsistencyTimeUnavailable`**: returned (wrapped) when the store
   cannot certify `C` within its wait budget.
3. **`Count(ctx, modelRef, asAt *time.Time)`** and
   **`CountByState(ctx, modelRef, states, asAt *time.Time)`**: `asAt == nil`
   keeps today's meaning; `asAt != nil` counts the entities whose latest
   revision at or before `asAt` is not deleted, committed only, ignoring the
   ambient transaction and recording nothing in the read set — the same
   point-in-time rule as `GetPage(asAt)` and `IterateOptions.PointInTime`.
   `Count` gets the doc comment it lacks.
4. **Conformance (`spitest`)**, new group `ConsistencyTime`:
   - `AcknowledgedCommitIncluded`: after a commit, `GetSubmitTime(tx) ≤ C`.
   - `LaterCommitStampsAbove`: a transaction that commits after `C` returned
     has `GetSubmitTime > C`.
   - `Monotonic`: successive calls never decrease.
   - `FinalAtC`: a `GetPage`/`Search`/`Count` at `C`, repeated after further
     commits, returns the same answer.
   - `NonTransactionalSaveIncluded`: a save outside a transaction, then `C`;
     a `GetAsAt(C)` finds it.
   - `CalledInsideTransaction`: calling with a transaction in `ctx` works and
     does not join it.
   - `Count/AsAt` and `CountByState/AsAt`: counts at an instant between two
     commits; deleted entities excluded; committed-only inside a
     transaction.
   - Harness: memory and sqlite run with a clock floored ahead
     (`NewTestClockAt`) as an additional harness instance.
5. SPI PR into `cyoda-go-spi` main with a `### Breaking` changelog entry;
   cyoda-go pseudo-pins main; no tag before the release cut (MAINTAINING.md).

## 6. Backends

### 6.1 memory (`plugins/memory`)

- `ConsistencyTime`: under `m.mu`, `C = max(clock.Now(), lastSubmitTime)`,
  `lastSubmitTime = C`, return `C`. No wait: commit holds `entityMu` from
  stamp to publish (`txmanager.go:852-1204`) and every reader takes
  `entityMu.RLock`.
- Wall clock: `wallClock.Now()` strips the monotonic reading (`Round(0)`), so
  the floor, stored stamps and `C` compare wall time and a wall-clock step back
  is floored. **Verify first:** the comment at `txmanager.go:725-726` relies
  on monotonic time; the change must keep what it protects.
- `Count`/`CountByState` with `asAt`: over the existing snapshot pointers
  (`grouped_stats.go:248-258`).

### 6.2 sqlite (`plugins/sqlite`)

- `ConsistencyTime`: take the commit gate with the caller's context (waits for
  a commit in flight to pass `sqlTx.Commit()`), then under `m.mu` reserve
  exactly as `Begin` does (`txmanager.go:738-789`), release the gate.
- Floor on open (`txmanager.go:595-602`): the highest of `MAX(submit_time)` on
  `entity_versions` and `MAX(point_in_time)` on `search_jobs` (an instant
  already handed out). A query error fails factory construction.
- `Count`/`CountByState` with `asAt`: SQL over the existing PIT base
  (`submit_time <= ?`, latest version per entity, not deleted).
- `GroupedAggregate` with `PointInTime`: pushed down over the same PIT base
  instead of declining (`grouped_stats.go:31`, `:227`).

### 6.3 postgres (`plugins/postgres`)

**Migration** `0000NN_consistency_time` (next free number):

- Sequence `cyoda_stamp_floor` (bigint microseconds since the epoch,
  `MINVALUE 0`), seeded with the highest of: `max(transaction_time)` on
  `entity_versions`, `max(submit_time)` on `submit_times`,
  `max(point_in_time)` on `search_jobs`.
- Function `cyoda_stamp(tenant text) RETURNS timestamptz`:
  1. `pg_advisory_xact_lock(tenant_key, xact_key)`: the in-flight marker,
     released when the transaction ends, after its rows are visible.
     `tenant_key = hashtext(tenant)`; `xact_key = (pg_current_xact_id() %
     2147483647) + 1` (never 0, unique among live transactions).
  2. `set_config('idle_in_transaction_session_timeout', '5s', true)`: a
     session left by a dead node ends within seconds and releases its marker.
  3. Under the session mutex `pg_advisory_lock(0, 0)`, inside a block whose
     handler also catches `query_canceled` and releases the mutex:
     `s = greatest(clock_us(), last_value + 1)`, `setval(floor, s)`.
  4. Return `s` as `timestamptz` (exact microsecond conversion via interval
     arithmetic, not floating point).
- Function `cyoda_consistency_time(tenant text, wait_budget interval) RETURNS timestamptz`:
  1. `set_config('lock_timeout', wait_budget, true)`: the wait budget. The
     plugin passes a fixed 10 s; a white-box test calls the function directly
     with a short budget (no test hook in Go code).
  2. Under the mutex (same guard): `c = greatest(clock_us(), last_value)`,
     `setval(floor, c)`.
  3. After releasing the mutex: list granted `ExclusiveLock` advisory locks
     in `pg_locks` for this database with `classid = tenant_key` and
     `objid <> 0`; take each with `pg_advisory_xact_lock_shared` (waits for
     that transaction to end; held only until this statement ends; no later
     transaction reuses the key).
  4. Return `c`.
- Objects schema-qualified; the advisory key layout recorded once as named
  constants in the plugin and in the migration comment. The two-int key space
  is used by nothing else (the scheduler and golang-migrate use the one-bigint
  form). Key `(0, 0)` is the mutex; no marker has `objid = 0`. A tenant-hash
  collision only adds waiting.

**Go side:**

- `stampCommitInstant` (`transaction_manager.go:363-453`) and
  `stampOwnCommitInstant` (`entity_store.go:395-415`) call
  `SELECT cyoda_stamp($tenant)` instead of `SELECT clock_timestamp()`. These
  are the only two stamp sites.
- `ConsistencyTime` runs `SELECT cyoda_consistency_time($tenant, '10s')` on its own
  pool connection, in autocommit, never on a transaction's connection. SQLSTATE
  `55P03` (lock timeout) maps to `spi.ErrConsistencyTimeUnavailable`.
- **Design rule: nothing after the stamp waits on a lock.** The stamp-phase
  statements touch only the transaction's own rows, and on the
  non-transactional path the stamp is the last statement before `COMMIT`. A
  fenced read made while the caller holds a transaction therefore cannot
  deadlock with the commits it waits for. A code comment at both stamp sites
  states the rule.
- PIT SQL: remove `ev.transaction_time <= CURRENT_TIMESTAMP`
  (`search_base.go:65`, `entity_store.go:604-611`, and every other
  occurrence — verify by grep). With the fence it protects nothing; when the
  floor runs ahead of the DB clock it hides rows `≤ C`.
- `Count`/`CountByState` with `asAt`: `count(*)` over the PIT lateral base
  (`search_base.go:54-71`), deleted flag from the version document; one index
  probe per entity of the model, no document leaves the database.
- `GroupedAggregate` with `PointInTime`: pushed down over the same base instead
  of declining (`grouped_stats.go:384-388`).
- `dropSchema` (`migrate.go:375`, called only by tests) moves to a test file.
- Exposure stated in `docs/plugins/POSTGRES.md`: with asynchronous replicas, a
  failover to a host whose clock is behind can stamp below a `C` already
  returned, the same exposure as losing commits on asynchronous failover.

### 6.4 cassandra (commercial plugin)

Cannot meet §2 on today's code (research §8). A cassandra issue asks for:
`ConsistencyTime` as a cluster-wide reserve-then-wait over shard owners that
closes `C`'s whole millisecond; `Count`/`CountByState` with `asAt`;
prerequisites cassandra#110, #97 items 1-2, #64 and a takeover floor; "snapshot
at `Begin` = `C`" (would close #97 item 3) as an option to assess. Its v0.9.0
bump needs this. No "not supported" answer exists in the SPI.

## 7. Engine (`cyoda-go` root module)

### 7.1 Package `internal/domain/consistency`

```go
type Service struct { /* txMgr spi.TransactionManager; per-tenant highest C */ }

func New(txMgr spi.TransactionManager) *Service
// Fresh returns a C from a store call made by this call.
func (s *Service) Fresh(ctx context.Context) (time.Time, error)
// Fence returns nil when t ≤ C, using the highest C seen for the tenant and
// asking the store only when t is above it.
func (s *Service) Fence(ctx context.Context, t time.Time) error
```

- The per-tenant highest `C` is in process memory; it only grows. A returned
  `C` stays final forever, so `t ≤` it needs no store call.
- Errors: refusal → `common.Operational(400, POINT_IN_TIME_AFTER_CONSISTENCY_TIME, …)`
  with `Props["consistencyTime"]`; `spi.ErrConsistencyTimeUnavailable` →
  `Operational(503, CONSISTENCY_TIME_UNAVAILABLE).AsRetryable()`; anything else
  → `common.Internal` (which already maps the storage-unavailable marker).
- No coalescing of concurrent store calls (not needed for correctness).
- Built once in `app/app.go` from the transaction manager after the tracing
  wrapper, which forwards `ConsistencyTime`.

### 7.2 Call sites

The fence goes into the service function that HTTP and gRPC share, so each
read is fenced once:

| Service function | Fence on | Fresh |
|---|---|---|
| `entity.GetEntity` (`service.go:408`) | `PointInTime` branch | — |
| `entity.ListEntities` (`:1888`) | non-nil `pointInTime` | — |
| `search.Search` (`search/service.go:662`) | non-nil `PointInTime` | — |
| `search.SubmitAsync` (`:911`) | non-nil `PointInTime` | nil → replaces `time.Now()` at `:980-983` |
| `entity.DeleteEntitiesConditional` (`:1278`) | non-nil `pointInTime` | — |
| `entity.GetStatistics*` (`:466`, `:520`, `:564`, `:610`) | non-nil `pointInTime` (new parameter) | — |
| `entity.QueryGroupedStats` (`grouped_stats_service.go:50`) | non-nil `PointInTime` | — |
| `entity.GetChangesMetadata` (`:783`) | non-nil `pointInTime` | — |
| transitions (`transitions_handler.go:16`) | when `usePointInTime` (either source) | — |
| new consistency-time handlers (HTTP, gRPC) | — | always |

`SearchService` gains the consistency service as a constructor dependency.

### 7.3 Changes on the way

- HTTP stats handlers (`entity/handler.go:396,421,452,477`) and gRPC stats
  handlers (`internal/grpc/search.go:441,512`) pass `pointInTime` to the
  service; the service passes it to `Count`/`CountByState`.
- gRPC get-all passes `req.PointInTime` (`internal/grpc/search.go:284-287`).
- Async result paging: the "`GetAsAt` → not found → skip" branch
  (`search/service.go:1495-1516`) is deleted as unreachable (no hard-delete
  path exists); the error takes the generic internal path. Its comment at
  `search/handler.go:346-347` goes too.
- Model-service guards (`model/service.go:377`, `:443`) call `Count` with
  `asAt = nil`.
- `cmd/cyoda/help/content/grpc.md:93` (unary vs streaming RPC lists) is
  corrected while the catalogue is edited.

## 8. Documentation

- `api/openapi.yaml`: new path and `ConsistencyTimeDto`; on every fenced
  operation, the new `400` response and its description; the async submit
  `pointInTime` text ("absent: the consistency time at submission"); the async
  results description's phantom "point-in-time" sentence (`:7212`).
- `cmd/cyoda/help/content/crud.md`: point-in-time semantics rewritten
  (`:579-614`: the fence, the consistency time, the stability rule replaced);
  transitions (`:395`); list-paging caveat; the new endpoint.
- `cmd/cyoda/help/content/search.md:236` (async default) and grouped stats
  text; `grpc.md` (new message types, RPC lists); two error topics; `errors.md`.
- `docs/CONSISTENCY.md` §1a: stability rule, the sqlite claim, "not
  scheduled" replaced by the consistency time.
- `docs/ARCHITECTURE.md:1475-1479` and DD-11; message catalogue (`:1827`).
- `docs/plugins/POSTGRES.md`: stamping, the floor sequence and functions, the
  "no session-level state" statement (`:458-460`), the replica exposure.
- gRPC schemas that say "current consistency time"
  (`docs/cyoda/schema/search/EntityGetRequest.json:20`,
  `EntityStatsGetRequest.json:15`, `EntityStatsByStateGetRequest.json:15`,
  `EntityChangesMetadataGetRequest.json:20`): "absent: the current state".
- Comments: `e2e/parity/pit_time.go:12-38`, `plugins/postgres/pit_time_test.go:11-38`.
- `COMPATIBILITY.md` (SPI pin), `docs/cloud-parity/consistency-time.md` +
  README row (§9).

## 9. Cloud parity (Gate 7)

`docs/cloud-parity/consistency-time.md` records: the definition (§2); the
fence and its `400`; the async default; the new endpoint and gRPC pair; the
two error codes; stats and get-all honouring `pointInTime`. Cloud differences
it must close: Cloud's `C` is final but not complete; Cloud serves reads later
than `C` unfenced; Cloud has no consistency-time endpoint. CaaS ticket filed
with it.

## 10. Error and status table

Rows marked *new* are added by this change; existing rows are unchanged and not
repeated.

| Endpoint (HTTP / gRPC) | Status / envelope | Code | Trigger |
|---|---|---|---|
| every fenced operation (§3.2) | 400 / `CLIENT_ERROR` | `POINT_IN_TIME_AFTER_CONSISTENCY_TIME` *new* | `T > C` |
| every fenced operation | 503 / `CLIENT_ERROR` (the existing convention for an operational code), retryable | `CONSISTENCY_TIME_UNAVAILABLE` *new* | store wait budget exhausted |
| every fenced operation | 503 / retryable | `STORAGE_UNAVAILABLE` | store unreachable while getting `C` |
| async submit with no `pointInTime` | 503 / retryable | `CONSISTENCY_TIME_UNAVAILABLE` *new* | as above |
| `GET /entity/consistency-time` / `EntityConsistencyTimeGetRequest` *new* | 200 / `Success=true` | — | — |
| same | 401, 403 | `UNAUTHORIZED`, `FORBIDDEN` | auth |
| same | 503 | `CONSISTENCY_TIME_UNAVAILABLE`, `STORAGE_UNAVAILABLE` | as above |
| same | 500 / `SERVER_ERROR` | `SERVER_ERROR` + ticket | other store failure |

## 11. Test coverage

| Scenario | Unit | Plugin white-box | spitest | e2e (postgres) | gRPC | Parity | Isolated multi-node / concurrency |
|---|---|---|---|---|---|---|---|
| `C` complete and final | | | ✓ (§5.4) | | | | |
| commit held between stamp and visibility makes `C` wait | | ✓ sqlite (gate held by test), memory (`gatedClock`), postgres (test-driven pgx tx calls `cyoda_stamp`, holds before COMMIT) | | | | | |
| another tenant's held commit does not delay `C` | | ✓ postgres | | | | | |
| wait budget → `ErrConsistencyTimeUnavailable` | | ✓ postgres (held marker; the SQL function called with a short budget; the Go mapping of `55P03` tested on the same held marker) | | | | | |
| cancelled `cyoda_consistency_time` leaves no lock | | ✓ postgres | | | | | |
| floor survives a clock step back | | ✓ memory, sqlite (`Clock`); postgres (sequence set ahead, own DB) | ✓ floored-ahead harness | | | | |
| sqlite reopen with stamps / job instants ahead | | ✓ | | | | | |
| `Count`/`CountByState` with `asAt` | | | ✓ | | | | |
| grouped stats PIT pushdown (sqlite, postgres) | | ✓ | | | | ✓ (existing grouped-stats PIT scenario) | |
| `Fence`: cached pass, store call, refusal, error mapping | ✓ (fake TM) | | | | | | |
| async default from the store, DB clock ahead finds confirmed saves | ✓ | | | ✓ (own DB, floor set ahead) | | | |
| every fenced endpoint × `T > C` → 400 + `consistencyTime` | | | | ✓ one per endpoint | ✓ one per gRPC request | ✓ one scenario per door | |
| every fenced endpoint × `T ≤ C` (a `C` from the endpoint) → 200 | | | | ✓ | ✓ | ✓ | |
| fenced endpoint × 503 `CONSISTENCY_TIME_UNAVAILABLE` | ✓ (fake TM) | | | ✓ one representative (own DB, held marker) | ✓ one representative | | |
| `GET /entity/consistency-time` 200 / 401 / 403 | | | | ✓ | ✓ | ✓ 200 | |
| stats ×4 and gRPC stats ×2 honour `pointInTime` | | | | ✓ | ✓ | ✓ | |
| gRPC get-all honours `pointInTime` | | | | | ✓ | | |
| list paging at one `C` is consistent while writes run | | | | | | | ✓ concurrency e2e |
| save confirmed on node A, read at node B's fresh `C` includes it | | | | | | | ✓ multi-node (subprocess fixture) |
| writers + async submits: no confirmed save missing; repeated reads at `C` identical | | | | | | | ✓ concurrency e2e |

Waivers: none planned. A cell that turns out unreachable is waived in the plan
with a one-line reason.

## 12. Existing tests that change

Tests that pass a future instant and expect `200` now expect the refusal, or
take their instant from the new endpoint. From the research (each verified
again when changed): `internal/domain/entity/handler_test.go:1499-1530`;
`internal/e2e/grouped_stats_invalid_path_test.go:63,111-118,174-182`;
`internal/e2e/entity_delete_unconditional_test.go:97-108,145-150`;
`internal/e2e/zzz_errorcode_matrix_test.go:250-257` (and the matrix gains the
new codes on every keyed operation); `internal/grpc/entity_deleteall_fields_test.go:212-235`.
spitest cases at `h.Now()+1h` stay valid: the SPI itself does not refuse a
future instant; the engine does. The parity cross-tenant cases
(`tenant_isolation.go:307-317,384-394`) stay valid because the floor is shared
across tenants on every in-tree backend.

## 13. Not in scope

- `AuditEventDto.consistencyTime` stays unset (Cloud fills it with the `C` at
  transaction creation; computing a `C` per `Begin` is a cost with no consumer).
- Reads without `pointInTime` stay current-state reads.
- The async job status does not report its instant.
- #611 later moves the clock source into the stamp function and the
  memory/sqlite `Clock`.

## 14. Delivery

1. SPI PR into `cyoda-go-spi` main (§5); cyoda-go pseudo-pins it in all four
   `go.mod` files in one commit; `COMPATIBILITY.md` updated.
2. cyoda-go PR into `release/v0.9.0`: backends, engine, API, docs, tests.
3. Filed with the PRs: the cassandra issue (§6.4) and the CaaS ticket (§9).
4. On merge: close #649 and #581 by hand; update #651.
