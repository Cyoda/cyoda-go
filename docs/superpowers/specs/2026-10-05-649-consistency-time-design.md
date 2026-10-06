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
   `C` returned before its request started. This holds across a restart of
   the store or of cyoda-go.
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
   rule 2. The change history (`GetVersionMetadata`), which also backs the
   audit trail (`internal/domain/audit/handler.go:98`), reads committed data
   only on every backend (postgres today reads the transaction's own
   uncommitted versions, `plugins/postgres/entity_store.go:1136,1161`; fixed in
   §6.3).
4. **`GET /entity/consistency-time`** returns a fresh `C` for the caller's
   tenant. A read at that instant, on any node, is never refused
   (monotonicity), and it includes every save confirmed before the call.

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

The fence runs before the store read that uses the instant, after every check
that does not itself read the instant, and whether or not a store read follows
(a page size of 0, a tenant with no models, an empty state list are fenced
too). So existing request errors (malformed instant, `pointInTime` with
`transactionId`, unknown or unregistered model, invalid grouped-stats path,
the async per-tenant cap pre-check) keep precedence over the refusal. A
`404 ENTITY_NOT_FOUND` that the read itself produces (get by id, change
history, transitions) comes after the fence: a future `T` on a missing entity
answers `400`. Exact placement per call site: §7.2.

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
Authentication and role failures stay transport errors (`Unauthenticated`,
`PermissionDenied`, `internal/grpc/role_interceptor.go:17-25`), as for every
RPC.

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
6. SPI call sites that change with the signatures: `default_save_all_test.go:39-40`,
   `spitest/transaction.go:580,583`, the `Count`/`CountByState` calls in
   `spitest/entity.go`.
7. SPI PR into `cyoda-go-spi` main with a `### Breaking` changelog entry;
   consumers notified per `KNOWN_CONSUMERS.md`; cyoda-go pseudo-pins main; no
   tag before the release cut (MAINTAINING.md).

## 6. Backends

### 6.1 memory (`plugins/memory`)

- `ConsistencyTime`: under `m.mu`, `C = max(clock.Now(), lastSubmitTime)`,
  `lastSubmitTime = C`, return `C`. No wait: every writer holds `entityMu`
  from stamp to publish (`txmanager.go:852-1204`) and every reader takes
  `entityMu.RLock`. A restart loses every stamp and job with it, so
  monotonicity across a restart is vacuous.
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
  exactly as `Begin` does (`txmanager.go:738-789`), then the high-water step
  below, then release the gate.
- **High-water mark** (monotonicity across a restart whose wall clock stepped
  back): a one-row table `consistency_floor(micros)` (new migration). When the
  reserved `C` exceeds the stored value, `ConsistencyTime` stores `C + 1 s`
  (one write per second of use at most). Stamps and later `C` values are
  floored by it after a restart.
- Floor on open (`txmanager.go:595-602`): the highest of `MAX(submit_time)` on
  `entity_versions`, `MAX(submit_time)` on `submit_times` (written by every
  commit, `txmanager.go:1200-1202`) and `consistency_floor.micros`. A query
  error fails factory construction. `search_jobs.point_in_time` is not a
  source: it held the caller's `pointInTime` as sent, with no check against
  the future, so one old submit dated far ahead would move every later stamp
  there for good. Every `C` handed out is already covered by
  `consistency_floor`.
- `Count`/`CountByState` with `asAt`: SQL over the existing PIT base
  (`submit_time <= ?`, latest version per entity, not deleted).
- `GroupedAggregate` with `PointInTime`: pushed down over the same PIT base
  instead of declining (`grouped_stats.go:31`, `:227`). Reason: today a
  grouped-stats read at a point in time streams every entity document out of
  the store to count in Go; the pushdown counts in the store, as `Count` now
  does.

### 6.3 postgres (`plugins/postgres`)

**Migration `000016_consistency_time`** (up and down; down drops both
functions and the sequence). SQL as verified by the second spec review on
postgres 17 with a non-superuser owner role (24 clients, 30 s, 63,410
commits, 15,815 calls: 0 finality, completeness or monotonicity violations;
a checker without the wait found 6,607):

```sql
CREATE SEQUENCE cyoda_stamp_floor AS bigint MINVALUE 0 START 0;
SELECT setval('cyoda_stamp_floor', coalesce(greatest(
  (SELECT (extract(epoch FROM max(transaction_time))*1000000)::bigint FROM entity_versions),
  (SELECT (extract(epoch FROM max(submit_time))*1000000)::bigint FROM submit_times)),0), true);

CREATE FUNCTION cyoda_stamp(tenant text) RETURNS timestamptz LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE cur_idle bigint; tkey int4 := hashtext(tenant);
  xkey int4 := ((pg_current_xact_id()::text::bigint % 2147483647) + 1)::int4; s bigint; held boolean := false;
BEGIN
  PERFORM set_config('lock_timeout','2000ms',true);
  SELECT setting::bigint INTO cur_idle FROM pg_settings WHERE name='idle_in_transaction_session_timeout';
  PERFORM set_config('idle_in_transaction_session_timeout',
    (CASE WHEN cur_idle=0 THEN 5000 ELSE least(cur_idle,5000) END)::text||'ms', true);
  PERFORM pg_advisory_xact_lock(tkey, xkey);
  BEGIN
    held := true; PERFORM pg_advisory_lock(0,0);
    SELECT greatest((extract(epoch FROM clock_timestamp())*1000000)::bigint, last_value+1) INTO s FROM cyoda_stamp_floor;
    PERFORM setval('cyoda_stamp_floor', s, true);
    PERFORM pg_advisory_unlock(0,0); held := false;
  EXCEPTION WHEN query_canceled OR OTHERS THEN
    IF held THEN PERFORM pg_advisory_unlock(0,0); END IF; RAISE;
  END;
  RETURN 'epoch'::timestamptz + s * interval '1 microsecond';
END $$;

CREATE FUNCTION cyoda_consistency_time(tenant text, wait_budget_ms bigint) RETURNS timestamptz LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE deadline timestamptz := clock_timestamp() + wait_budget_ms * interval '1 millisecond';
  tkey int4 := hashtext(tenant); c bigint; held boolean := false; k oid; rem bigint;
BEGIN
  PERFORM set_config('lock_timeout', greatest(wait_budget_ms,1)::text||'ms', true);
  BEGIN
    held := true; PERFORM pg_advisory_lock(0,0);
    SELECT greatest((extract(epoch FROM clock_timestamp())*1000000)::bigint, last_value) INTO c FROM cyoda_stamp_floor;
    PERFORM setval('cyoda_stamp_floor', c, true);
    PERFORM pg_advisory_unlock(0,0); held := false;
  EXCEPTION WHEN query_canceled OR OTHERS THEN
    IF held THEN PERFORM pg_advisory_unlock(0,0); END IF; RAISE;
  END;
  FOR k IN SELECT objid FROM pg_locks WHERE locktype='advisory'
      AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
      AND classid=tkey AND objsubid=2 AND objid<>0 AND mode='ExclusiveLock' AND granted LOOP
    IF clock_timestamp() >= deadline THEN
      RAISE EXCEPTION 'consistency time wait budget exhausted' USING ERRCODE='55P03'; END IF;
    rem := ceil(extract(epoch FROM deadline - clock_timestamp())*1000)::bigint;
    PERFORM set_config('lock_timeout', greatest(rem,1)::text||'ms', true);
    PERFORM pg_advisory_xact_lock_shared(tkey, k::bigint::int4);
  END LOOP;
  RETURN 'epoch'::timestamptz + c * interval '1 microsecond';
END $$;
```

Notes on the SQL:

- The floor is seeded from the stamps already stored:
  `entity_versions.transaction_time` and `submit_times.submit_time`.
  `search_jobs.point_in_time` is not a source, for the reason in §6.2.
- The in-flight marker `(hashtext(tenant), xact_key)` is taken outside the
  guarded block (a block is a subtransaction; rolling it back would drop a
  lock taken inside it). It is held until the transaction ends, after its rows
  are visible. `xact_key` is never 0 and unique among live transactions
  (`xidStopLimit` < 2^31 − 1). `cyoda_stamp` is called at the top level of the
  commit, never inside a savepoint that may roll back.
- `(0, 0)` is the mutex. The `held` flag is set before the lock call and
  survives the block's rollback, so a cancel serviced between the grant and
  the next statement cannot leak the session-level mutex; unlocking a mutex
  that was never granted only raises a WARNING. The two-int key form
  (`objsubid = 2`) is used by nothing else in the plugin (the scheduler and
  golang-migrate use the one-bigint form). `classid = tkey` matches negative
  hashes (int4 → oid wraps). A tenant-hash collision only adds waiting.
- `lock_timeout` of 2 s in `cyoda_stamp` bounds the mutex wait (held for
  microseconds) and makes a violation of the design rule below fail fast. In
  `cyoda_consistency_time` the first statement sets `lock_timeout` to the
  budget, so the mutex wait is inside the budget too. The
  idle-in-transaction limit is the lower of the operator's setting and 5 s
  (0 means unset); a pause of more than 5 s between the stamp and `COMMIT`
  aborts the commit.
- The checker's shared locks are held to the end of its statement; their
  number is the tenant's commits in their commit phase at that moment, bounded
  by the pool sizes of the nodes.
- `set_config(..., true)` effects end with the caller's transaction or
  autocommit statement; `SET search_path FROM CURRENT` does not scope them.
- Role: the plugin connects as the owner of its objects
  (`docs/plugins/POSTGRES.md:331-344`). A non-owner role needs `SELECT, UPDATE`
  on the sequence, `USAGE` on the schema and `EXECUTE` on both functions
  (granted to `PUBLIC` by default). Stated in POSTGRES.md.

**Go side:**

- `stampCommitInstant` (`transaction_manager.go:363-453`) and
  `stampOwnCommitInstant` (`entity_store.go:395-415`) call
  `SELECT cyoda_stamp($tenant)` instead of `SELECT clock_timestamp()`. These
  are the only two stamp sites. A `55P03` from `cyoda_stamp` (lock contention
  in the commit phase; the transaction rolls back) is classified as retryable
  `503 STORAGE_UNAVAILABLE`, like the existing idle-in-transaction abort
  (`isIdleInTxAbort`).
- `ConsistencyTime` runs `SELECT cyoda_consistency_time($tenant, $budget_ms)`
  on its own pool connection, in autocommit, never on a transaction's
  connection. `budget_ms` is 10 000, or the configured statement timeout when
  that is above 0 and lower (`config.go:56`). Error mapping, in order, before
  the generic classifier (`ceilings.go:194-197`): `ctx.Err() != nil` → that
  error (a client cancel also arrives as `57014`); SQLSTATE `55P03` or `57014`
  → `spi.ErrConsistencyTimeUnavailable`; the storage-unavailable
  classification as today; anything else wrapped.
- When either function returns an error, the connection is closed instead of
  being returned to the pool, so no session-level lock can outlive the error.
- **Design rule: nothing after the stamp waits on a lock.** Read-set
  validation (`FOR SHARE`) runs before the stamp
  (`transaction_manager.go:236-267`). The statements after the stamp touch
  only rows the transaction wrote, on both paths, with one exception: the
  `sm_audit_events` UPDATE matches by transaction label
  (`transaction_manager.go:436-441`), which only this transaction's audit rows
  carry, so it cannot wait on another transaction. A fenced read made while the
  caller holds a transaction therefore cannot deadlock with the commits it
  waits for. A code comment at both stamp sites states the rule.
- PIT SQL: remove `ev.transaction_time <= CURRENT_TIMESTAMP` at its two
  occurrences (`search_base.go:65`, `entity_store.go:604-611`). With the fence
  it protects nothing; when the floor runs ahead of the DB clock it hides rows
  `≤ C`.
- `GetVersionMetadata` (`entity_store.go:1136,1161`) runs on
  `committedQuerier` (`search_base.go:147-149`), as every other committed read
  does.
- `Count`/`CountByState` with `asAt`: `count(*)` over the PIT lateral base
  (`search_base.go:54-71`), deleted flag from the version document; one index
  probe per entity of the model, no document leaves the database.
- `GroupedAggregate` with `PointInTime`: pushed down over the same base
  instead of declining (`grouped_stats.go:384-388`), for the reason in §6.2.
- `dropSchema` (`migrate.go:365`, called only by tests) moves to a test file.
- Exposure stated in `docs/plugins/POSTGRES.md`: with asynchronous replicas, a
  failover to a host whose clock is behind can stamp below a `C` already
  returned, the same exposure as losing commits on asynchronous failover.
  (Each `cyoda_consistency_time` call commits its `setval` durably.)

### 6.4 cassandra (commercial plugin)

Cannot meet §2 on today's code (research §8). A cassandra issue asks for:
`ConsistencyTime` meeting all four properties of §2 (backend-wide
completeness and monotonicity included) as a cluster-wide reserve-then-wait
over shard owners that closes `C`'s whole millisecond; `Count`/`CountByState`
with `asAt` (`internal/store/entity_store.go:1805`,
`entity_store_count_by_state.go:39`); `GetVersionMetadata` committed-only in a
transaction; prerequisites cassandra#110, #97 items 1-2, #64 and a takeover
floor; "snapshot at `Begin` = `C`" (would close #97 item 3) as an option to
assess. Its v0.9.0 bump needs this. No "not supported" answer exists in the
SPI.

## 7. Engine (`cyoda-go` root module)

### 7.1 Package `internal/domain/consistency`

```go
type Service struct { /* txMgr; per-tenant state */ }

func New(txMgr spi.TransactionManager) *Service
// Fresh returns a C from a store call that started after Fresh was called.
func (s *Service) Fresh(ctx context.Context) (time.Time, error)
// Fence returns nil when t ≤ C, else the refusal or the store's error.
func (s *Service) Fence(ctx context.Context, t time.Time) error
```

**Per-tenant state**, keyed by the exact `spi.TenantID` (no normalisation):
`hi`, the highest `C` seen (only grows; a returned `C` stays final forever),
and the store calls in flight with their start times. An entry idle for a
while may be dropped; that costs one extra store call.

**Rules:**

- **`Fence(t)`:** pass if `t ≤ hi`. Else join the call in flight, if any;
  pass if its result is `≥ t`. Else `Fresh` and compare; refuse if still
  `t > C`. (A refusal is always based on a call that started after the
  `Fence` did, which monotonicity makes the right answer.)
- **`Fresh`:** join a call in flight only if it started after `Fresh` was
  called; otherwise start a new one at once, which every later caller then
  shares. At most two calls are in flight per tenant. When both are older
  than a `Fresh`, it waits for the oldest one's remaining time and then for
  one call that started after it entered. A `Fence` first waits for the
  in-flight call it joined, so its bound is the joined call's remaining time
  plus one call.
- **The store call** runs on `context.WithoutCancel(ctx)` (keeps the tenant)
  with a deadline of the store's budget plus a margin of 1 s (11 s), so one
  caller's cancel does not fail the others; sqlite's gate wait obeys the same
  deadline. Each caller waits on its own `ctx.Done()` as well. Every caller
  that shares a call gets its result or its error.
- **Errors:** refusal → `common.Operational(400,
  POINT_IN_TIME_AFTER_CONSISTENCY_TIME, …)` with `Props["consistencyTime"]`
  and `C` in the message; `spi.ErrConsistencyTimeUnavailable` →
  `Operational(503, CONSISTENCY_TIME_UNAVAILABLE).AsRetryable()`; the caller's
  own context error → as today for a cancelled request; a store call past its
  own deadline (`context.DeadlineExceeded`) → the same retryable
  `503 CONSISTENCY_TIME_UNAVAILABLE`; anything else → `common.Internal` (which
  maps the storage-unavailable marker).

**Wiring:** one instance, built in `app/app.go` from the transaction manager
after the tracing wrapper (which forwards `ConsistencyTime`). It is a
**required constructor argument** of `entity.New` (`app/app.go:537`),
`NewSearchService` (`search/service.go:301`) and `NewGroupedStatsHandler`
(`grouped_stats_handler.go:64`, which builds the grouped-stats service), and
is passed to the gRPC service for the new request. A nil argument panics at
construction. Test call sites pass a service built on their store's
transaction manager (a helper per test package); there is no fallback to the
process clock.

### 7.2 Call sites

| Service function | Fence | Exact placement |
|---|---|---|
| `entity.GetEntity` (`service.go:408`) | `PointInTime` branch | before `GetAsAt` (`:418-419`) |
| `entity.ListEntities` (`:1888`) | non-nil `pointInTime` | after the model checks, before the `pageSize > 0` branch (`:1922`) |
| `search.Search` (`search/service.go:662`) | non-nil `PointInTime` | after query validation, before `store.Search` (`:730-737`) |
| `search.SubmitAsync` (`:911`) | non-nil → `Fence`; nil → `Fresh` | delete the `time.Now()` default (`:980-983`); run the fence/fresh between the cap pre-check (`:993-997`) and the job construction (`:999`). The pre-check wins with `503 SEARCH_QUEUE_FULL`; the authoritative cap (`registerJob`, `:1066-1083`) and the pool rejection (`:1085-1098`) run after the fence. |
| `entity.DeleteEntitiesConditional` (`:1278`) | non-nil `pointInTime` | at function entry, after the condition parse and before branching to `deleteBatched` / `deleteConditionalSingleTx`: a model existence check on `ctx` (outside any transaction; `404 MODEL_NOT_FOUND` keeps precedence), then the fence. Both paths keep their own model check inside their scope. No pooled connection is held during the fence. |
| `entity.GetStatistics*` (`:466`, `:520`, `:564`, `:610`) | non-nil `pointInTime` (new parameter) | after `EnsureModelRegistered` (single-model variants) and model enumeration (all-models variants), unconditionally, before the first `Count`/`CountByState` |
| `QueryGroupedStats` (`grouped_stats_service.go:50`) | non-nil `PointInTime` | after request and path validation, before pushdown/`Iterate` |
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
  (`search/service.go:1494-1516`) is deleted as unreachable (no hard-delete
  path exists); the error takes the generic internal path. Its comment at
  `search/handler.go:346-347` and its test
  (`internal/domain/search/job_lookup_outage_test.go:364`) go too; a unit
  test with a fake store covers the error path.
- Model-service guards (`model/service.go:377`, `:443`) call `Count` with
  `asAt = nil`.
- `cmd/cyoda/help/content/grpc.md`: `:93` lists streaming types under the
  unary RPC; `:89` lists `EntityDeleteAllRequest` under `entityManage`
  (served on `entityManageCollection`, `internal/grpc/entity.go:463`) and
  leaves out `EntityPatchRequest` (`:141`). Both corrected while the
  catalogue is edited.
- `cmd/compute-test-client/callback.go:650` reads at `time.Now().Add(time.Hour)`;
  it reads at a `C` from the new endpoint instead (it backs parity
  `CallbackTxJoin_PITCommittedOnly`, `e2e/parity/pit_committed_only.go:67-72`,
  whose uncommitted secondary still answers 404).
- Stale comments: `plugins/postgres/pit_committed_only_test.go:12-19` (cites
  the removed guard); `plugins/sqlite/grouped_stats_test.go:308-322` (calls
  committed-only PIT inside a transaction a "limitation"; it is the contract).

## 8. Documentation

- `api/openapi.yaml`: new path and `ConsistencyTimeDto`; on every fenced
  operation, the `400` and `503` descriptions gain the new codes and the
  `pointInTime` parameter text states the fence; the async submit
  `pointInTime` text ("absent: the consistency time at submission"); the
  async results description's phantom "point-in-time" sentence (`:7212`).
- `cmd/cyoda/help/content/crud.md`: point-in-time semantics rewritten
  (`:579-614`: the fence, the consistency time, the stability rule replaced);
  transitions (`:395`); list-paging caveat; the new endpoint; change history
  and audit trail committed-only in a transaction.
- `cmd/cyoda/help/content/search.md:236` (async default) and grouped stats
  text; `grpc.md` (new message types, RPC lists, §7.3); two error topics;
  `errors.md`; `errors/STORAGE_UNAVAILABLE.md:31` (a statement timeout while
  getting `C` is `CONSISTENCY_TIME_UNAVAILABLE`, not a `500`).
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
two error codes; stats and get-all honouring `pointInTime`; change history and
audit trail committed-only in a transaction. Cloud differences it must close:
Cloud's `C` is final but not complete; Cloud serves reads later than `C`
unfenced; Cloud has no consistency-time endpoint. CaaS ticket filed with it.

## 10. Error and status table

Rows marked *new* are added by this change; existing rows are unchanged and not
repeated. gRPC envelopes per §4.3; gRPC auth failures are transport errors
(§4.2).

| Endpoint | Status | Code | Trigger |
|---|---|---|---|
| every fenced operation (§3.2) | 400 | `POINT_IN_TIME_AFTER_CONSISTENCY_TIME` *new* | `T > C` |
| every fenced operation, and async submit with no `pointInTime` | 503, retryable | `CONSISTENCY_TIME_UNAVAILABLE` *new* | the tenant has a save held in its commit phase beyond the wait budget |
| every fenced operation, and async submit with no `pointInTime` | 503, retryable | `STORAGE_UNAVAILABLE` | store unreachable while getting `C` |
| every write (commit) | 503, retryable | `STORAGE_UNAVAILABLE` | `cyoda_stamp` lock wait over 2 s (postgres) |
| `GET /entity/consistency-time` *new* | 200 | — | — |
| same | 401, 403 | `UNAUTHORIZED`, `FORBIDDEN` | auth (HTTP) |
| same | 503 | `CONSISTENCY_TIME_UNAVAILABLE`, `STORAGE_UNAVAILABLE` | as above |
| same | 500 | `SERVER_ERROR` + ticket | other store failure |
| `EntityConsistencyTimeGetRequest` *new* | `Success=true` | — | — |
| same | `Success=false`, `CLIENT_ERROR`, retryable | `CONSISTENCY_TIME_UNAVAILABLE` / `STORAGE_UNAVAILABLE` prefix | as above |
| same | `Success=false`, `SERVER_ERROR` | ticket | other store failure |

## 11. Test coverage

**The 503 e2e stack:** its own database (`newSchedDB`-style),
`CYODA_POSTGRES_STATEMENT_TIMEOUT=1s` (budget 1 s; the call ends with `57014`,
mapped to `CONSISTENCY_TIME_UNAVAILABLE`), wrapped with
`openapivalidator.NewMiddleware` as `entity_delete_nonconvergence_test.go:94`
does, so its responses feed the conformance report and the error-code matrix.
A test-held marker for the tenant: the holder calls `cyoda_stamp(tenant)` and
then `pg_sleep` in the same transaction, so the idle limit does not end it.
Every 503 cell uses a far-future `T`, so the fence cannot pass on the node's
cached `hi`. Each cell costs about 1 s.

**gRPC 503 cells** run in `internal/grpc` with a transaction-manager wrapper
whose `ConsistencyTime` returns `spi.ErrConsistencyTimeUnavailable` (the
`onCommitTxMgr` pattern, `entity_timeout_test.go:280-298`), or a
storage-unavailable error.

| Scenario | Unit | Plugin white-box | spitest | e2e (postgres) | gRPC | Parity (HTTP) | Isolated multi-node / concurrency |
|---|---|---|---|---|---|---|---|
| `C` complete, final, monotonic, cross-tenant | | | ✓ (§5.5) | | | | |
| commit held between stamp and visibility makes `C` wait | | ✓ sqlite (gate held by the test), memory (`gatedClock`), postgres (test-driven pgx transaction calls `cyoda_stamp`, holds before COMMIT) | | | | | |
| `C` reads the store clock, not `time.Now()` | | ✓ memory, sqlite (`NewTestClockAt` ahead) | | | | | |
| another tenant's held commit does not delay `C` | | ✓ postgres | | | | | |
| wait budget → `ErrConsistencyTimeUnavailable` (`55P03` with a short budget; `57014` under a low statement timeout); client cancel stays a cancel | | ✓ postgres | | | | | |
| cancelled call or stamp leaves no lock; erroring connection closed; `cyoda_stamp` `55P03` → 503 | | ✓ postgres | | | | | |
| acquire timeout while getting `C` → storage-unavailable classification | | ✓ postgres | | | | | |
| floor survives a clock step back | | ✓ memory, sqlite (`Clock`); postgres (sequence set ahead, own DB) | | | | | |
| reopen / migration seed: stamps, `submit_times`, high-water mark ahead of the clock raise the floor; a far-future job instant does not | | ✓ sqlite, postgres | | | | | |
| `Count`/`CountByState` with `asAt`; change history committed-only in a tx | | | ✓ | | | | |
| grouped stats PIT pushdown (sqlite, postgres) | | ✓ | | | | ✓ (existing grouped-stats PIT scenario) | |
| `Fence`/`Fresh`: cached pass, join rules, at most two calls, caller cancel vs shared call, refusal, error mapping (unavailable, storage-unavailable marker, other) | ✓ (fake TM) | | | | | | |
| each §7.2 call site propagates the fence's errors; nil service panics at construction | ✓ (fake TM per service) | | | | | | |
| async default from the store; DB floor ahead finds confirmed saves | ✓ | | | ✓ (own DB, floor set ahead) | | | |
| every fenced HTTP operation × 400 refusal (detail carries `C`) | | | | ✓ one per operation | | ✓ one per operation | |
| every fenced gRPC request × refusal envelope | | | | | ✓ one per request | | |
| every fenced operation × 200 at a `C` from the endpoint | | | | ✓ | ✓ | ✓ | |
| future `T` on a missing entity → 400 (check order) | | | | ✓ get by id | | | |
| fenced read inside a transaction (rule 3) | | | | ✓ get by id in a transaction | | | |
| every fenced operation × 503 `CONSISTENCY_TIME_UNAVAILABLE` | | | | ✓ one per operation (503 stack) | ✓ one per request | | |
| async submit with no `pointInTime` × 503 | | | | ✓ (503 stack) | ✓ | | |
| fenced operation × 503 `STORAGE_UNAVAILABLE` | ✓ (fake TM, marker error) | | | | ✓ one representative | | |
| `GET /entity/consistency-time` × 200 (main validated stack) / 401 / 403 / 503 (503 stack) | | | | ✓ | ✓ 200, 503 envelope | ✓ 200 | |
| stats ×4 and gRPC stats ×2 honour `pointInTime` | | | | ✓ | ✓ | ✓ | |
| gRPC get-all honours `pointInTime` | | | | | ✓ | | |
| list paging at one `C` is consistent while writes run | | | | | | | ✓ concurrency e2e (`internal/e2e`) |
| save confirmed on node A, node B's fresh `C` includes it; a `C` from node A passes the fence on node B | | | | | | | ✓ `e2e/parity/multinode` with the postgres fixture (`e2e/parity/postgres/multinode_fixture.go`) |
| writers + async submits: no confirmed save missing; repeated reads at `C` identical | | | | | | | ✓ concurrency e2e (`internal/e2e`) |

**Error-code matrix** (`internal/e2e/zzz_errorcode_matrix_test.go`, checked
both ways): `POINT_IN_TIME_AFTER_CONSISTENCY_TIME` is declared on every keyed
fenced operation (`getOneEntity`, `getAllEntities`,
`getEntityStatisticsForModel`, `getEntityStatisticsByStateForModel`,
`deleteEntities`) and produced on the main stack; `CONSISTENCY_TIME_UNAVAILABLE`
is declared on the same keys and produced on the validated 503 stack. If
`getConsistencyTime` becomes a key, `FORBIDDEN` is declared and produced
too.

**Waivers** (one line each): the `500` on the new endpoint is the generic
internal path, covered by the unit test of the mapping; the deleted async
"not found → skip" branch is unreachable by construction, covered by a unit
test with a fake store; e2e `503 STORAGE_UNAVAILABLE` on the fence needs an
unreachable database mid-request and is covered by the unit, plugin and gRPC
cells above.

## 12. Existing tests

**Change (they pass a future instant and expect success):**
`internal/domain/entity/handler_test.go:1512`;
`internal/e2e/entity_delete_unconditional_test.go:97-108`;
`e2e/parity/pit_committed_only.go:67-72` (through `callback.go:650`, §7.3).

**Change (they assert behaviour this design replaces):**
`plugins/postgres/grouped_stats_test.go:552` and
`plugins/sqlite/grouped_stats_test.go:724` (pushdown declines at a point in
time); sqlite `grouped_stats_test.go:583` (`PointInTimeBeatsMalformedPath`);
`internal/domain/search/job_lookup_outage_test.go:364` (the deleted skip).

**Rewrite their premise:** `internal/e2e/grouped_stats_invalid_path_test.go:48-52,111-118,174-182`
use a point in time to force the streaming path, which the pushdown removes;
they force it with a joined transaction instead
(`grouped_stats_service.go:124-125`), and their instants come from the new
endpoint.

**Keep, now pinning check order (§3.3):**
`internal/e2e/entity_delete_unconditional_test.go:145-150`
(`404 MODEL_NOT_FOUND` before the fence);
`internal/e2e/zzz_errorcode_matrix_test.go:250-257` (`pointInTime` +
`transactionId` `400` before the fence). `internal/grpc/entity_deleteall_fields_test.go:212-235`
uses `time.Now()`; it switches to a far-future instant so it pins the order.

**Fixtures that build a state that can no longer occur** (Gate 6): job rows
seeded with `point_in_time = now+1m` (`internal/e2e/async_stream_test.go:907`,
`internal/e2e/scheduled_run_fencing_test.go:382`) use an instant at or below
a `C` instead.

**Stale skip:** `e2e/parity/externalapi/entity_delete.go:88` is removed.

**Compile changes:** `GetStatistics*` callers (`entity/service_test.go:232,249,262,309,326,347`,
`entity/handler.go:397,427,458,478`, `internal/grpc/search.go:449,478,526,537`);
`Count`/`CountByState` fakes (`internal/domain/entity/mock_store_test.go:81,84`,
`service_unique_keys_test.go:504-508`) and the plugin tests' callers;
`ConsistencyTime` fakes (`internal/observability/tx_tracing_test.go:14`,
`internal/domain/workflow/engine_test.go:2261`,
`internal/domain/workflow/fire_scheduled_test.go:770`); every
`NewSearchService`, `entity.New` and `NewGroupedStatsHandler` call site (the
new required argument).

**Stay valid:** spitest cases at `h.Now()+1h` (the SPI does not refuse a
future instant; the engine does); the parity cross-tenant cases
(`e2e/parity/tenant_isolation.go:307-317,384-394`), by backend-wide
completeness (§2); `internal/e2e/transitions_clockskew_test.go` (shifts
`valid_time` only, independent of the removed guard).

## 13. Not in scope

- `AuditEventDto.consistencyTime` stays unset (Cloud fills it with the `C` at
  transaction creation; computing a `C` per `Begin` is a cost with no
  consumer).
- Reads without `pointInTime` stay current-state reads.
- The async job status does not report its instant.
- `e2e/parity/externalapi/negative_validation.go:195` (12_07) stays skipped:
  it expects an `entitySearchLimit` parameter cyoda-go does not have, which is
  unrelated to this design.
- #611 later moves the clock source into the stamp function and the
  memory/sqlite `Clock`.

## 14. Delivery

1. SPI PR into `cyoda-go-spi` main (§5); cyoda-go pseudo-pins it in all four
   `go.mod` files in one commit; `COMPATIBILITY.md` updated.
2. cyoda-go PR into `release/v0.9.0`: backends, engine, API, docs, tests.
3. Filed with the PRs: the cassandra issue (§6.4) and the CaaS ticket (§9).
4. On merge: close #649 and #581 by hand; update #651.
