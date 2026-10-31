# #611 platform clock — research notes

Facts gathered before the design. Each line cites the code it rests on. The
design is in `docs/superpowers/specs/2026-10-26-611-platform-clock-design.md`.

Base: `release/v0.9.0` at `c031ec7a`. SPI pin `v0.8.5-0.20261018180211-97ff6210e982`
(`go.mod:7`, `plugins/*/go.mod:6`); the local SPI checkout at `97ff6210` matches it.

## 1. Where business time comes from today

### Storage plugins (the "stamp domain")

Every plugin stamps a commit with `max(physical clock, floor + 1 µs)` and keeps a
floor that never goes back. The floor moves on a submit time, a SnapshotTime
(memory, sqlite) and a consistency time.

- **memory:** submit time `plugins/memory/txmanager.go:662-674`; SnapshotTime
  `:744-755`; consistency time `:1293-1302`; audit events rewritten to the
  commit instant `:1005-1006`. Physical clock: `factory.clock`
  (`plugins/memory/clock.go:10-17`), default `wallClock{}`
  (`store_factory.go:165`).
- **sqlite:** submit time `plugins/sqlite/txmanager.go:666-674`; SnapshotTime
  `:797-807`; consistency time `:1379-1404`, which also persists a high-water
  mark of C + 1 s (`:1395-1399`); floor seed on open `:605-620`.
- **postgres:** `cyoda_stamp` and `cyoda_consistency_time`
  (`plugins/postgres/migrations/000016_consistency_time.up.sql:39-84`) take
  `clock_timestamp()` under advisory lock (0,0) and update the sequence
  `cyoda_stamp_floor`, seeded from stored stamps (`:25-28`). Postgres has no
  SnapshotTime; visibility uses the REPEATABLE READ snapshot.
  `stampCommitInstant` (`transaction_manager.go:403-503`) writes the stamp into
  `entity_versions`, `entities`, `sm_audit_events` and `submit_times`.
- The guard `transaction_time <= CURRENT_TIMESTAMP` that #611 names was removed
  by #649 (`6b2e3dbd`); `search_base.go:124-129` explains why.

### Root module

Business-time reads (non-test), all `time.Now()` unless noted:

| Site | Becomes |
|---|---|
| `internal/scheduler/clock.go:20` (via `app/app.go:495-521`) | scheduler `ClaimRequest.NowMs` (`service.go:607`), attempt/retry/failure times and audit timestamp (`service.go:690`, `:939`) |
| `internal/domain/workflow/engine.go:173,236` (replaced by `schedClock.Now`, `app.go:499`) | arm time `armMs` (`arm.go:115`) → `ScheduledTime`, `ArmedAt`; fire decision (`fire_scheduled.go:91,186`); every state-machine audit event (`engine.go:1110`) |
| `internal/domain/entity/service.go:282,2015,2271,2528` | pre-commit `CreationDate`/`LastModifiedDate`; reaches compute nodes (`internal/grpc/dispatch.go:121-122`) and meta-date criteria (`internal/match/prepared.go:484-486`) before the store restamps |
| `internal/domain/model/service.go:195` | `ModelDescriptor.UpdateDate` |
| `internal/domain/search/service.go:967,1183,1189,1208,1216,1228,1315,1496`; `reaper.go:258,292` | search job `CreateTime`, `finishTime` |

Real-time reads that are not business time: auth (JWT, token issuance, key
validity, M2M client dates, key `validFrom` defaults), callout deadlines,
gossip, scheduler heartbeats/watchdog, model-cache lease, metrics.

`internal/common/uuid.go:22` makes v1 UUIDs (wall-clock time inside). The audit
listing reads that time only to order events whose other keys are equal
(`internal/domain/audit/events.go:144`).

`NewCloudEvent` sets no `time` attribute (`internal/grpc/cloudevent.go:23-29`).
Cloud sets it from its JVM clock
(`~/dev/cyoda/backend/src/main/kotlin/net/cyoda/saas/grpc/ProtobufMessageConverter.kt:153`).

### Places that compare a business time with a physical clock

These break once the business clock leaves real time, in either direction.

- memory/sqlite `GetSubmitTime` eviction: `clock.Now() - 1h` against submit
  times (memory `txmanager.go:1193`; sqlite `txmanager.go:986`, `998-1001`).
- postgres submit-time pruning: Go `time.Now() - 1h` against stamps
  (`transaction_manager.go:354`, `543-560`).
- postgres search `ClaimStale`: host `created_at` against DB `now()`
  (`search_store.go:630`); `ReapExpired`: host `finished_at` against Go
  `time.Now()` (`search_store.go:587`).
- memory/sqlite use one `factory.clock` for business stamps and for liveness
  (scheduler owners `scheduled_task_claims.go`, search heartbeats
  `search_store.go`). A test that advances it moves both.

## 2. Scheduler

- Polls every `ScanInterval` (default 1 s, `app/config.go:457`); claims WAITING
  tasks with `NextAttemptTime <= NowMs` (SPI `persistence.go:201-209`), per
  tenant in `(NextAttemptTime, ID)` order, one per entity, up to `MaxRuns` (8)
  and `MaxRunsPerTenant` (4).
- A task with `TimeoutMs` whose `ScheduledTime + TimeoutMs < now` is recorded
  as expired, not fired (`internal/domain/workflow/fire_scheduled.go:62-67`).
- `ScheduledTaskStore` has no cross-tenant "earliest due" read; `Query` is per
  tenant (SPI `persistence.go`, `Query(ctx, tenant, q)`).
- A schedule function's request carries no platform time (`arm.go:275`).
- e2e: `internal/e2e/scheduler_harness_test.go` gives a test its own stack on
  its own postgres database, because `ClaimDue` is cross-tenant.

## 3. SPI and plugin construction

- `Plugin.NewFactory(ctx, getenv, opts ...FactoryOption)`; the only option is
  `WithClusterBroadcaster` (SPI `plugin.go:51-72`). No clock hook.
- `TransactionManager.ConsistencyTime` is "in the store's own stamp domain",
  `max(store clock, highest stamp issued)` (SPI `transaction.go:71-92`).
- spitest: `Harness.AdvanceClock` required, `Harness.Now` optional
  (`spitest.go:24-33`). memory/sqlite pass `TestClock`; postgres sleeps.

## 4. Cluster

- Gossip (`internal/cluster/registry/gossip_broadcast.go`) is best-effort, no
  ordering, no persistence (SPI `cluster.go:3-19`).
- Admin `/api/admin/*` changes are process-local and not propagated
  (`cmd/cyoda/help/content/admin.md`).
- Platform operator: `auth.OperatorGuard.Require` — `ROLE_ADMIN` in tenant
  `PLATFORM`, not on-behalf-of (`internal/auth/operator_guard.go:19-53`).

## 5. cassandra (`../cyoda-go-cassandra` at `1f53fb5`)

- Business time comes from its own hybrid logical clock (`clock/hlc.go`), whose
  physical part is `time.Now()`. No host hook (`plugin.go:83` calls
  `factory.New` with no options).
- Transaction ids and audit `event_time` are real-time timeuuids
  (`internal/tx/tx_manager.go:125`, `internal/store/sm_audit_store.go:38`).
  Recovery turns the floor into a timeuuid bound
  (`internal/tx/tx_recovery.go:330-337`); with the business clock away
  from real time it skips committed rows.
- `period_bucket` uses real time (`internal/tx/tx_coordinator.go:1244`); TTLs use the
  server clock. Scheduled tasks are not implemented (`factory.go:753-761`).

## 6. Cyoda Cloud (`~/dev/cyoda`, platform library 4.0.0-SNAPSHOT)

- No controllable clock and no time endpoint. `ClockCtrl` is a per-JVM
  singleton whose clock only test code can replace
  (`~/dev/cyoda-platform/core-libs/core/.../clocks/ClockCtrl.java:33`).
- The scheduler arms JDK timers with a real-time delay
  (`ScheduledTasksHolderCtrl.java:70-90`).

## 7. Prior art

| Product | Start | Moves | Between moves | Scheduled work in the skipped time |
|---|---|---|---|---|
| Stripe test clocks | past or future, then forward only | to a target | frozen | stepped through, each at its own time, next event recomputed after each; move is async (`advancing` → `ready`) |
| Temporal test server | real time | automatic skip | runs | skips to the next timer when all workflows wait |
| Camunda 8 (Zeebe) | pin to any time | pin again / reset | frozen while pinned | not documented; `UPDATE` on `SYSTEM` |
| XTDB 2 | per-transaction `:system-time` | must not be earlier than any previous | — | — |
| Datomic | per-transaction `:db/txInstant` | must not be earlier than the last | — | — |
| libfaketime | any absolute start | — | runs at a rate (`x10`) | — |

Sources: docs.stripe.com/billing/testing/test-clocks/api-advanced-usage;
stripe.dev/blog/test-clocks-how-we-made-it-easier-to-test-stripe-billing-integrations;
docs.temporal.io/develop/typescript/testing-suite;
docs.camunda.io/docs/apis-tools/orchestration-cluster-api-rest/specifications/pin-clock/;
docs.xtdb.com/drivers/clojure/codox/xtdb.api.html;
forum.datomic.com/t/ingest-historical-data-from-some-log-files-into-datomic-maybe-local-and-preserving-the-transaction-time/2483;
man.archlinux.org/man/faketime.1.

## 8. Why postgres stamps from the database clock

No document states "database clock instead of process clock" as a decision.
The reasons on record:

- cyoda-light-go `docs/plans/2026-03-27-postgresql-storage-design.md` §5:
  `transaction_time` from `CURRENT_TIMESTAMP` — "same for all writes in the
  transaction"; `wall_clock_time` from `clock_timestamp()` — "Actual insertion
  moment … for debugging and analysis".
- cyoda-light-go `docs/ARCHITECTURE.md:366`: the submit time "is the database's
  view of commit time, not the application's wall clock".
- cyoda-light-go #132 (`docs/superpowers/specs/2026-04-01-postgres-temporal-consistency-design.md`):
  Delete moved from the Go clock to the database clock so that all stamps "come
  from the same source (DB clock)".
- #649 (Paul, issuecomment-5954874596): the consistency time "must come from
  the store's clock domain … In a cluster, cyoda-go nodes ask the store; they
  never use their own clocks for this."
- Liveness checks stamp and compare on the database clock
  (`plugins/postgres/search_store.go:217-220`, `scheduled_task_store.go:453-458`).
- Incidents from comparing process time with database stamps: #460 (transitions
  404 when the database clock ran ahead; Docker VM 10–13 ms behind the host),
  #649 (async search dropped confirmed saves; database ahead by up to 21.8 ms).
- Contrast: cassandra stamps from node clocks and needed a hybrid logical clock
  (cyoda-light-go `docs/hlc-clock.md`; `CASSANDRA_BACKEND_DESIGN.md` §2.2:
  "Physical wall clocks can skew — NTP is not a guarantee").

Conclusion used by the design: production keeps one database clock for every
stamp; only a test clock replaces it.
