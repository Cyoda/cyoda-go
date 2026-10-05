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
returned by the store on a request made for a tenant:

1. **Complete.** Every save, of any tenant, whose success was returned on any
   node before the request for `C` started, has a stamp `≤ C`.
2. **Final.** A read for the requesting tenant at `T ≤ C` that starts after
   `C` was returned sees every save of that tenant stamped `≤ T`, and always
   will. A save not yet stamped when `C` is returned is stamped `> C`.
3. **Monotonic.** Every `C` returned, for any tenant on any node, is `≥` every
   `C` returned before its request started.
4. **Read resolution.** `C` covers the store's read unit. A store that widens
   `T` to a coarser unit when it reads (cassandra: the whole millisecond)
   returns a `C` that closes that whole unit. `C` is never rounded up when it
   is rendered.

Completeness and monotonicity are backend-wide because the stamp floor is
shared by every tenant. Finality is per tenant because reads are: the store
waits only for the requesting tenant's saves in flight.

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
   rule 2. The change history (`GetVersionMetadata`) reads committed data only
   on every backend (postgres today reads the transaction's own uncommitted
   versions, `plugins/postgres/entity_store.go:1136,1161`; fixed, §6.3).
4. **`GET /entity/consistency-time`** returns a fresh `C` for the caller's
   tenant. A read at that instant is never refused (monotonicity), and it
   includes every save confirmed before the call.

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

### 3.3 Where the fence runs

The fence runs immediately before the store read that uses the instant, after
every check that does not itself read the instant. Existing request errors
(malformed instant, `pointInTime` with `transactionId`, unknown or
unregistered model, invalid grouped-stats path, async submit cap) therefore
keep precedence over the refusal. A `404 ENTITY_NOT_FOUND` that the read
itself produces (get by id, change history, transitions) comes after the
fence: a future `T` on a missing entity answers `400`. Exact placement per
call site: §7.2.

## 4. API additions

### 4.1 HTTP

`GET {context}/entity/consistency-time`, operationId `getConsistencyTime`, tag
of the entity group. Authenticated, `ROLE_M2M` (the default route guard),
tenant from the token. No parameters. `X-Tx-Token` is not declared; a token
sent anyway is handled by the join middleware as on any route and does not
change the answer (`C` never uses a transaction).

`200` body `ConsistencyTimeDto` (`required: [consistencyTime]`):

```json
{ "consistencyTime": "2026-10-05T14:03:07.123456Z" }
```

`consistencyTime`: `string`, `format: date-time`, rendered with `RFC3339Nano`
at the store's full precision (microseconds on sqlite and postgres,
nanoseconds on memory). Never rounded.

Errors: `401`, `403` (shared components), `503 CONSISTENCY_TIME_UNAVAILABLE`,
`503 STORAGE_UNAVAILABLE`, `500`.

### 4.2 gRPC

`EntityConsistencyTimeGetRequest` (base event fields only) →
`EntityConsistencyTimeResponse` (`consistencyTime`, date-time, optional: set
when `success` is true; the failure envelope carries `error` instead, as the
other responses do), on the unary `EntitySearch` RPC. Schemas in
`docs/cyoda/schema/search/`, both names in `CloudEventType.json`, constants in
`internal/grpc/cloudevent_types.go`, types regenerated with
`scripts/generate-events.sh`, dispatch in the unary switch
(`internal/grpc/search.go:29-56`). A request carrying a transaction token is
routed to the owning node by the existing interceptor; the answer is the same.

### 4.3 Error codes

| Code | HTTP | Retryable | When | Detail |
|---|---|---|---|---|
| `POINT_IN_TIME_AFTER_CONSISTENCY_TIME` | 400 | no | `T > C` on a fenced read | `properties.consistencyTime` = current `C`; the message also states it |
| `CONSISTENCY_TIME_UNAVAILABLE` | 503 | yes | the store could not certify `C` within its wait budget (a save of the tenant held in its commit phase) | — |

A store failure that carries the storage-unavailable marker keeps the existing
`503 STORAGE_UNAVAILABLE`; any other failure is `500` with a ticket. On gRPC
both codes follow the existing operational-error convention
(`internal/grpc/errors.go:42-66`: `Error.Code = CLIENT_ERROR`, the domain code
as the message prefix, `Retryable` set); the refusal message carries `C`
because the envelope has no properties.

Each code gets `internal/common/error_codes.go` + `knownErrorCodes`, a
`cmd/cyoda/help/content/errors/<CODE>.md` topic and an `errors.md` index line.
Every fenced operation already declares `400` and `503` in OpenAPI; their
descriptions gain the new codes (no new response, so oasdiff sees an additive
change plus the new path).

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
4. **`GetVersionMetadata`** doc: reads committed data only, inside a
   transaction too (states what memory and sqlite already do).
5. **Conformance (`spitest`)**, new group `ConsistencyTime`:
   - `AcknowledgedCommitIncluded`: after a commit, `GetSubmitTime(tx) ≤ C`.
   - `CrossTenantCommitIncluded`: a commit for tenant A, then `C` for tenant
     B: `GetSubmitTime ≤ C`.
   - `LaterCommitStampsAbove`: a transaction that commits after `C` returned
     has `GetSubmitTime > C`.
   - `Monotonic`: successive calls, across two tenants, never decrease.
   - `FinalUnderConcurrentWrites`: writers commit in goroutines while a
     checker takes `C`, counts at `C`, waits, counts again; the counts match
     and every commit acknowledged before the request is counted (the
     research probe's shape). Deterministic proof of the wait is in the
     white-box tests (§11).
   - `NonTransactionalSaveIncluded`: a save outside a transaction, then `C`;
     `GetAsAt(C)` finds it.
   - `CalledInsideTransaction`: calling with a transaction in `ctx` works and
     does not join it.
   - `Count/AsAt` and `CountByState/AsAt`: counts at an instant between two
     commits; deleted entities excluded; committed-only inside a
     transaction.
   - `GetVersionMetadata/CommittedOnlyInTx`: a transaction's own uncommitted
     version is not listed.
6. SPI PR into `cyoda-go-spi` main with a `### Breaking` changelog entry;
   consumers notified per `KNOWN_CONSUMERS.md`; cyoda-go pseudo-pins main; no
   tag before the release cut (MAINTAINING.md).

## 6. Backends

### 6.1 memory (`plugins/memory`)

- `ConsistencyTime`: under `m.mu`, `C = max(clock.Now(), lastSubmitTime)`,
  `lastSubmitTime = C`, return `C`. No wait: every writer holds `entityMu`
  from stamp to publish (`txmanager.go:852-1204`) and every reader takes
  `entityMu.RLock`.
- Wall-clock step back: `nextSubmitTime`, `Begin` and `ConsistencyTime` strip
  the monotonic reading (`Round(0)`) before comparing with the floor, so the
  floor compares wall time. Other users of the clock (scheduled-task claims,
  search store, `txmanager.go:1191`) keep the monotonic reading. The comment
  at `txmanager.go:725-726` is rewritten: the floor alone gives what it
  describes.
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
  instead of declining (`grouped_stats.go:31`, `:227`). Reason: today a
  grouped-stats read at a point in time streams every entity document out of
  the store to count in Go; the pushdown counts in the store, as `Count` now
  does.

### 6.3 postgres (`plugins/postgres`)

**Migration `000016_consistency_time`:**

- Sequence `cyoda_stamp_floor` (bigint microseconds since the epoch,
  `MINVALUE 0 START 0`), set with `setval(..., coalesce(greatest(a, b, c), 0),
  true)` where `a`, `b`, `c` are the microsecond values of
  `max(transaction_time)` on `entity_versions`, `max(submit_time)` on
  `submit_times` and `max(point_in_time)` on `search_jobs`.
- Both functions are `LANGUAGE plpgsql`, `SET search_path FROM CURRENT` (the
  plugin has no schema setting; objects resolve in the migration's schema).
  The floor is read as `SELECT last_value FROM cyoda_stamp_floor` (always
  defined after the seed's `setval(..., true)`).
- Microsecond conversions are exact: `(extract(epoch FROM ts) * 1000000)::bigint`
  (numeric) and `'epoch'::timestamptz + n * interval '1 microsecond'`.
- **`cyoda_stamp(tenant text) RETURNS timestamptz`:**
  1. `set_config('lock_timeout', '2000ms', true)`: no step after the stamp
     waits long on a lock (the mutex is held for microseconds).
  2. `set_config('idle_in_transaction_session_timeout', <least(current
     setting, 5 s), 0 treated as unset>, true)`: a session left by a dead
     node ends within seconds and releases its marker. Documented: a pause of
     more than 5 s between the stamp and `COMMIT` aborts the commit.
  3. `pg_advisory_xact_lock(tenant_key, xact_key)`: the in-flight marker,
     released when the transaction ends, after its rows are visible.
     `tenant_key = hashtext(tenant)`;
     `xact_key = ((pg_current_xact_id()::text::bigint % 2147483647) + 1)::int4`
     (never 0; unique among live transactions, which stay within
     `xidStopLimit` < 2^31 − 1).
  4. In a block whose handler catches `OTHERS` and `query_canceled`, releases
     the mutex if held, and re-raises: `pg_advisory_lock(0, 0)` (the mutex),
     `s = greatest(clock_us(), last_value + 1)`, `setval(floor, s, true)`,
     `pg_advisory_unlock(0, 0)`.
  5. Return `s`.
- **`cyoda_consistency_time(tenant text, wait_budget_ms bigint) RETURNS timestamptz`:**
  1. `deadline = clock_timestamp() + wait_budget_ms ms`.
  2. In the same guarded block as above: take the mutex, `c =
     greatest(clock_us(), last_value)`, `setval(floor, c, true)`, release.
  3. After the mutex: list `objid` from `pg_locks` where `locktype =
     'advisory'`, `database` = this database's oid, `classid = tenant_key`,
     `objsubid = 2`, `objid <> 0`, `mode = 'ExclusiveLock'`, `granted`.
  4. For each: if `deadline` has passed, raise SQLSTATE `55P03`; else set
     `lock_timeout` to the remaining milliseconds (transaction-local) and take
     `pg_advisory_xact_lock_shared(tenant_key, objid)` (waits for that
     transaction to end; held until this statement ends; no later transaction
     reuses the key).
  5. Return `c`.
- Key layout: `(0, 0)` is the mutex; markers are `(hashtext(tenant), 1…2^31−1)`.
  The two-int form (`objsubid = 2`) is used by nothing else in the plugin (the
  scheduler and golang-migrate use the one-bigint form, `objsubid = 1`). A
  tenant-hash collision only adds waiting. Named constants in the plugin; the
  layout stated in the migration comment.
- The plugin assumes it connects as the owner of its objects
  (`docs/plugins/POSTGRES.md:331-344`); a non-owner role needs `USAGE, UPDATE`
  on the sequence and `EXECUTE` on both functions. Stated in POSTGRES.md.

**Go side:**

- `stampCommitInstant` (`transaction_manager.go:363-453`) and
  `stampOwnCommitInstant` (`entity_store.go:395-415`) call
  `SELECT cyoda_stamp($tenant)` instead of `SELECT clock_timestamp()`. These
  are the only two stamp sites.
- `ConsistencyTime` runs `SELECT cyoda_consistency_time($tenant, $budget_ms)`
  on its own pool connection, in autocommit, never on a transaction's
  connection. `budget_ms = min(10 000, statement timeout)` with the
  statement timeout from the plugin config (`config.go:56`). SQLSTATE `55P03`
  and `57014` map to `spi.ErrConsistencyTimeUnavailable`.
- When either function returns an error, the connection is closed instead of
  being returned to the pool, so no session-level lock can outlive the error.
- **Design rule: nothing after the stamp waits on a lock.** Read-set
  validation (`FOR SHARE`) runs before the stamp
  (`transaction_manager.go:236-267`); the statements after the stamp touch only
  rows the transaction wrote, on both paths. A fenced read made while the
  caller holds a transaction therefore cannot deadlock with the commits it
  waits for. A code comment at both stamp sites states the rule; step 1 of
  `cyoda_stamp` makes a violation fail fast.
- PIT SQL: remove `ev.transaction_time <= CURRENT_TIMESTAMP` at its two
  occurrences (`search_base.go:65`, `entity_store.go:604-611`). With the fence
  it protects nothing; when the floor runs ahead of the DB clock it hides rows
  `≤ C`.
- `GetVersionMetadata` (`entity_store.go:1136,1161`) runs on
  `committedQuerier`, as every other committed read does
  (`search_base.go:147-149`).
- `Count`/`CountByState` with `asAt`: `count(*)` over the PIT lateral base
  (`search_base.go:54-71`), deleted flag from the version document; one index
  probe per entity of the model, no document leaves the database.
- `GroupedAggregate` with `PointInTime`: pushed down over the same base
  instead of declining (`grouped_stats.go:384-388`), for the reason in §6.2.
- `dropSchema` (`migrate.go:375`, called only by tests) moves to a test file.
- Exposure stated in `docs/plugins/POSTGRES.md`: with asynchronous replicas, a
  failover to a host whose clock is behind can stamp below a `C` already
  returned, the same exposure as losing commits on asynchronous failover.

### 6.4 cassandra (commercial plugin)

Cannot meet §2 on today's code (research §8). A cassandra issue asks for:
`ConsistencyTime` meeting all four properties of §2 (backend-wide
completeness and monotonicity included) as a cluster-wide reserve-then-wait
over shard owners that closes `C`'s whole millisecond; `Count`/`CountByState`
with `asAt`; `GetVersionMetadata` committed-only in a transaction;
prerequisites cassandra#110, #97 items 1-2, #64 and a takeover floor; "snapshot
at `Begin` = `C`" (would close #97 item 3) as an option to assess. Its v0.9.0
bump needs this. No "not supported" answer exists in the SPI.

## 7. Engine (`cyoda-go` root module)

### 7.1 Package `internal/domain/consistency`

```go
type Service struct { /* txMgr; per-tenant state */ }

func New(txMgr spi.TransactionManager) *Service
// Fresh returns a C from a store call that started after Fresh was called.
func (s *Service) Fresh(ctx context.Context) (time.Time, error)
// Fence returns nil when t ≤ C. It compares with the highest C seen for the
// tenant and asks the store only when t is above it.
func (s *Service) Fence(ctx context.Context, t time.Time) error
```

- **Per-tenant state**, keyed by the exact `spi.TenantID` with no
  normalisation: the highest `C` seen (only grows; a returned `C` stays final
  forever, so `t ≤` it needs no store call) and at most one store call in
  flight.
- **Sharing a store call.** A `Fence` caller joins the call in flight, if any,
  and makes a new one only if the joined result is still below `t`. A `Fresh`
  caller joins only a call that started after it did; otherwise it waits for
  the next one. This bounds concurrent store calls per tenant to one, so a
  held commit cannot drain the connection pool.
- **Errors:** refusal →
  `common.Operational(400, POINT_IN_TIME_AFTER_CONSISTENCY_TIME, …)` with
  `Props["consistencyTime"]` and `C` in the message;
  `spi.ErrConsistencyTimeUnavailable` →
  `Operational(503, CONSISTENCY_TIME_UNAVAILABLE).AsRetryable()`; anything else
  → `common.Internal` (which already maps the storage-unavailable marker).
- **One instance**, built in `app/app.go` from the transaction manager after
  the tracing wrapper (which forwards `ConsistencyTime`), injected into
  `entity.New` (`app/app.go:537`), `NewGroupedStatsService`
  (`grouped_stats_service.go:40`) and `SearchService` (a `With…` option, like
  `WithHealthFlag`, `search/service.go:324`, so its test call sites keep
  compiling), and into the gRPC service for the new request.

### 7.2 Call sites

| Service function | Fence | Exact placement |
|---|---|---|
| `entity.GetEntity` (`service.go:408`) | `PointInTime` branch | before `GetAsAt` (`:418-419`) |
| `entity.ListEntities` (`:1888`) | non-nil `pointInTime` | before `GetPage` (`:1923`), after the model checks |
| `search.Search` (`search/service.go:662`) | non-nil `PointInTime` | before `store.Search` (`:730-737`), after query validation |
| `search.SubmitAsync` (`:911`) | non-nil → `Fence`; nil → `Fresh` replaces `time.Now()` (`:980-983`) | after the tenant cap check (`:985-997`, which stays storeless and wins with `503 SEARCH_QUEUE_FULL`), before `CreateJob` |
| `entity.DeleteEntitiesConditional` (`:1278`) | non-nil `pointInTime` | after the selection plan, in both `deleteConditionalSingleTx` and `deleteBatched`, before the first `Iterate`; `404 MODEL_NOT_FOUND` from the plan keeps precedence |
| `entity.GetStatistics*` (`:466`, `:520`, `:564`, `:610`) | non-nil `pointInTime` (new parameter) | after `EnsureModelRegistered` (single-model variants) / model enumeration, before the first `Count`/`CountByState` |
| `entity.QueryGroupedStats` (`grouped_stats_service.go:50`) | non-nil `PointInTime` | after request and path validation, before pushdown/`Iterate` |
| `entity.GetChangesMetadata` (`:783`) | non-nil `pointInTime` | before `GetVersionMetadata` |
| transitions (`transitions_handler.go:16`) | when `usePointInTime` (either source) | after the instant is resolved (`:42-83`), before `GetAsAt` |
| consistency-time handlers (HTTP, gRPC) | — | `Fresh` |

Async reclaim and re-execution read at the job's stored instant without a
fence (final since submit).

### 7.3 Changes on the way

- HTTP stats handlers (`entity/handler.go:396,421,452,477`) and gRPC stats
  handlers (`internal/grpc/search.go:441,512`) pass `pointInTime` to the
  service; the service passes it to `Count`/`CountByState`.
- gRPC get-all passes `req.PointInTime` (`internal/grpc/search.go:284-287`).
- Async result paging: the "`GetAsAt` → not found → skip" branch
  (`search/service.go:1495-1516`) is deleted as unreachable (no hard-delete
  path exists); the error takes the generic internal path. Its comment at
  `search/handler.go:346-347` goes too. Covered by a unit test with a fake
  store (the e2e cell is unreachable by construction).
- Model-service guards (`model/service.go:377`, `:443`) call `Count` with
  `asAt = nil`.
- `cmd/cyoda/help/content/grpc.md:93` (it lists streaming types under the
  unary RPC) is corrected while the catalogue is edited.
- `cmd/compute-test-client/callback.go:650` reads at `time.Now().Add(time.Hour)`;
  it reads at a `C` from the new endpoint instead (it backs parity
  `CallbackTxJoin_PITCommittedOnly`, `e2e/parity/pit_committed_only.go:66-71`,
  whose uncommitted secondary still answers 404).

## 8. Documentation

- `api/openapi.yaml`: new path and `ConsistencyTimeDto`; on every fenced
  operation, the `400` and `503` descriptions gain the new codes and the
  `pointInTime` parameter text states the fence; the async submit
  `pointInTime` text ("absent: the consistency time at submission"); the
  async results description's phantom "point-in-time" sentence (`:7212`).
- `cmd/cyoda/help/content/crud.md`: point-in-time semantics rewritten
  (`:579-614`: the fence, the consistency time, the stability rule replaced);
  transitions (`:395`); list-paging caveat; the new endpoint.
- `cmd/cyoda/help/content/search.md:236` (async default) and grouped stats
  text; `grpc.md` (new message types, RPC lists); two error topics; `errors.md`.
- `docs/CONSISTENCY.md` §1a: stability rule, the sqlite claim, "not
  scheduled" replaced by the consistency time.
- `docs/ARCHITECTURE.md:1475-1479` and DD-11; message catalogue (`:1827`).
- `docs/plugins/POSTGRES.md`: stamping, the floor sequence and functions, the
  "no session-level state" statement (`:458-460`), the 5 s commit-phase
  limit, the role grants, the replica exposure.
- gRPC schemas: "current consistency time" becomes "absent: the current
  state" in `EntityGetRequest.json:20`, `EntityStatsGetRequest.json:15`,
  `EntityStatsByStateGetRequest.json:15`,
  `EntityChangesMetadataGetRequest.json:20`; `EntitySnapshotSearchRequest.json`
  states the async default.
- Comments: `e2e/parity/pit_time.go:12-38`, `plugins/postgres/pit_time_test.go:11-38`.
- `COMPATIBILITY.md` (SPI pin), `docs/cloud-parity/consistency-time.md` +
  README row (§9).

## 9. Cloud parity (Gate 7)

`docs/cloud-parity/consistency-time.md` records: the definition (§2); the
fence and its `400`; the async default; the new endpoint and gRPC pair; the
two error codes; stats and get-all honouring `pointInTime`; change history
committed-only in a transaction. Cloud differences it must close: Cloud's `C`
is final but not complete; Cloud serves reads later than `C` unfenced; Cloud
has no consistency-time endpoint. CaaS ticket filed with it.

## 10. Error and status table

Rows marked *new* are added by this change; existing rows are unchanged and not
repeated. HTTP and gRPC alike; gRPC envelopes per §4.3.

| Endpoint | Status | Code | Trigger |
|---|---|---|---|
| every fenced operation (§3.2) | 400 | `POINT_IN_TIME_AFTER_CONSISTENCY_TIME` *new* | `T > C` |
| every fenced operation, and async submit with no `pointInTime` | 503, retryable | `CONSISTENCY_TIME_UNAVAILABLE` *new* | the tenant has a save held in its commit phase beyond the wait budget |
| every fenced operation, and async submit with no `pointInTime` | 503, retryable | `STORAGE_UNAVAILABLE` | store unreachable while getting `C` (existing classification) |
| `GET /entity/consistency-time` / `EntityConsistencyTimeGetRequest` *new* | 200 | — | — |
| same | 401, 403 | `UNAUTHORIZED`, `FORBIDDEN` | auth |
| same | 503 | `CONSISTENCY_TIME_UNAVAILABLE`, `STORAGE_UNAVAILABLE` | as above |
| same | 500 | `SERVER_ERROR` + ticket | other store failure |

## 11. Test coverage

The 503 cells run on a dedicated e2e stack: its own database
(`newSchedDB`-style), `CYODA_POSTGRES_STATEMENT_TIMEOUT=1s` (so the wait budget
is 1 s), and a test-held marker whose holder keeps a statement running
(`cyoda_stamp(...)` then `pg_sleep`), so the idle timeout does not end it. Each
cell costs about 1 s.

| Scenario | Unit | Plugin white-box | spitest | e2e (postgres) | gRPC | Parity (HTTP) | Isolated multi-node / concurrency |
|---|---|---|---|---|---|---|---|
| `C` complete, final, monotonic, cross-tenant | | | ✓ (§5.5) | | | | |
| commit held between stamp and visibility makes `C` wait | | ✓ sqlite (gate held by the test), memory (`gatedClock`), postgres (test-driven pgx transaction calls `cyoda_stamp`, holds before COMMIT) | | | | | |
| `C` reads the store clock, not `time.Now()` | | ✓ memory, sqlite (`NewTestClockAt` ahead) | | | | | |
| another tenant's held commit does not delay `C` | | ✓ postgres | | | | | |
| wait budget → `ErrConsistencyTimeUnavailable` (`55P03`, `57014`) | | ✓ postgres | | | | | |
| cancelled call or stamp leaves no lock; erroring connection is closed | | ✓ postgres | | | | | |
| floor survives a clock step back | | ✓ memory, sqlite (`Clock`); postgres (sequence set ahead, own DB) | | | | | |
| sqlite reopen with stamps / job instants ahead | | ✓ | | | | | |
| `Count`/`CountByState` with `asAt`; change history committed-only in a tx | | | ✓ | | | | |
| grouped stats PIT pushdown (sqlite, postgres) | | ✓ | | | | ✓ (existing grouped-stats PIT scenario) | |
| `Fence`/`Fresh`: cached pass, store call, sharing rules, refusal, error mapping | ✓ (fake TM) | | | | | | |
| each §7.2 call site propagates the fence's errors | ✓ (fake TM per service) | | | | | | |
| async default from the store; DB floor ahead finds confirmed saves | ✓ | | | ✓ (own DB, floor set ahead) | | | |
| every fenced HTTP operation × 400 refusal (detail carries `C`) | | | | ✓ one per operation | | ✓ one per operation | |
| every fenced gRPC request × refusal envelope | | | | | ✓ one per request | | |
| every fenced operation × 200 at a `C` from the endpoint | | | | ✓ | ✓ | ✓ | |
| every fenced operation × 503 `CONSISTENCY_TIME_UNAVAILABLE` | | | | ✓ one per operation (dedicated stack) | ✓ one per request (dedicated stack) | | |
| async submit with no `pointInTime` × 503 | | | | ✓ (dedicated stack) | ✓ | | |
| `GET /entity/consistency-time` × 200 / 401 / 403 / 503 `CONSISTENCY_TIME_UNAVAILABLE` | | | | ✓ | ✓ | ✓ 200 | |
| stats ×4 and gRPC stats ×2 honour `pointInTime` | | | | ✓ | ✓ | ✓ | |
| gRPC get-all honours `pointInTime` | | | | | ✓ | | |
| list paging at one `C` is consistent while writes run | | | | | | | ✓ concurrency e2e (`internal/e2e`) |
| save confirmed on node A, node B's fresh `C` includes it | | | | | | | ✓ `e2e/parity/multinode` with the postgres fixture (`e2e/parity/postgres/multinode_fixture.go`) |
| writers + async submits: no confirmed save missing; repeated reads at `C` identical | | | | | | | ✓ concurrency e2e (`internal/e2e`) |

Waivers (one line each, as the coverage rule requires):

- `503 STORAGE_UNAVAILABLE` on the fence and on the new endpoint: produced by
  the existing `common.Internal` classification, unchanged by this design and
  covered by its existing tests; no new e2e cell.
- `500` on the new endpoint: the generic internal-error path, covered by the
  unit test of the error mapping.
- The async "not found → skip" removal: unreachable by construction; unit test
  with a fake store.

The e2e error-code matrix (`internal/e2e/zzz_errorcode_matrix_test.go`)
declares the new codes only on the keyed operations whose cells the e2e run
above produces.

## 12. Existing tests

**Change (they pass a future instant and expect `200`):**
`internal/domain/entity/handler_test.go:1512`;
`internal/e2e/grouped_stats_invalid_path_test.go:111-118`;
`internal/e2e/entity_delete_unconditional_test.go:97-108`. They take their
instant from the new endpoint, and the "future instant" case becomes a
refusal assertion.

**Keep, now pinning check order (§3.3):**
`internal/e2e/grouped_stats_invalid_path_test.go:63,174-182` (path `400`
before the fence); `internal/e2e/entity_delete_unconditional_test.go:145-150`
(`404 MODEL_NOT_FOUND` before the fence);
`internal/e2e/zzz_errorcode_matrix_test.go:250-257` (`pointInTime` +
`transactionId` `400` before the fence);
`internal/grpc/entity_deleteall_fields_test.go:212-235`.

**Fixtures that build a state that can no longer occur** (Gate 6): job rows
seeded with `point_in_time = now+1m` (`internal/e2e/async_stream_test.go:907`,
`internal/e2e/scheduled_run_fencing_test.go:382`) and stamps pushed `+1h`
(`internal/e2e/transitions_clockskew_test.go`) use an instant at or below a
`C`, or raise the floor (own database) instead.

**Stale skips** to verify and remove if stale: `e2e/parity/externalapi/entity_delete.go:88`,
`e2e/parity/externalapi/negative_validation.go:195`.

**Fakes** with explicit methods that need the new signatures:
`Count`/`CountByState` in `internal/domain/entity/mock_store_test.go:81,84`
and `service_unique_keys_test.go:504-508`; `ConsistencyTime` in
`internal/observability/tx_tracing_test.go:14`,
`internal/domain/workflow/engine_test.go:2261`,
`internal/domain/workflow/fire_scheduled_test.go:770`.

**Stay valid:** spitest cases at `h.Now()+1h` (the SPI does not refuse a
future instant; the engine does); the parity cross-tenant cases
(`e2e/parity/tenant_isolation.go:307-317,384-394`), by backend-wide
completeness (§2).

## 13. Not in scope

- `AuditEventDto.consistencyTime` stays unset (Cloud fills it with the `C` at
  transaction creation; computing a `C` per `Begin` is a cost with no
  consumer).
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
