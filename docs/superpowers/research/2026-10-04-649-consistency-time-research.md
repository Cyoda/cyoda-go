# #649 consistency time — research

The facts that the design for #649 depends on. Paths are relative to the
cyoda-go repository unless prefixed. Five read-only research passes (root
module, postgres, memory/sqlite, cassandra, Cloud) and one postgres probe,
2026-10-04, on `release/v0.9.0` at `5d6f52ed`. Statements marked
**unverified** were not checked against code or a run.

## 1. The defect and the guarantee

An async search with no `pointInTime` uses the cyoda-go process clock
(`internal/domain/search/service.go:980-983`). Stores stamp with their own
clock. When the store clock is ahead, confirmed saves fall after the search
instant and the job finishes `SUCCESSFUL` without them (#649 evidence).

A **consistency time** `C` is an instant in the store's stamp domain with:

1. **Complete** — every save whose success was returned before `C` was
   requested has a stamp `≤ C`.
2. **Final** — once `C` is returned, no save can still become visible with
   a stamp `≤ C`.

### 1.1 The issue's formula is not complete

The design notes on #649 propose
`C = max(floor, min(storeClockNow, earliest stamp among stamped-not-committed saves) − ε)`.
That is final but **not complete**: save P stamps 100 and is still
committing; save A stamps 105, commits, and is acknowledged. The formula
gives `C = 99`, so A is missing from a read at `C`. This is the same gap as
Cloud's (section 6), which Cloud papers over with an opt-in client wait.

Both properties together need a **wait**: pick `C` at "now" (raised to the
highest stamp issued), then wait until every save that already holds a stamp
`≤ C` has finished committing. Saves that stamp later must stamp `> C`. This
is the "reserve, then wait" pattern (industry names: safe time, resolved
timestamp, closed timestamp).

## 2. Where cyoda-go chooses or consumes an instant

**Only one read has its instant chosen by cyoda-go:** the async search
default (`service.go:980-983`). Every other read with no `pointInTime` is a
current-state read (complete by the #501 contract, no instant needed). The
transitions handler had the same defect and was fixed to read the current
version instead (`f671d285`, `internal/domain/entity/transitions_handler.go:32-96`).

**Reads that take an explicit instant** (HTTP and gRPC):

| Read | Path to the store | Notes |
|---|---|---|
| entity get `pointInTime` | `GetAsAt` (`entity/service.go:415-422`) | |
| entity list `pointInTime` (HTTP) | `GetPage(asAt)` (`service.go:1923`) | |
| entity get-all (gRPC) | — | **ignores `req.PointInTime`**, passes `nil` (`internal/grpc/search.go:284-287`) |
| direct search `pointInTime` | `Search` (`search/service.go:730-737`) | |
| async search with `pointInTime` | `Iterate` + per-page `GetAsAt` (`service.go:1274-1277`, `:1507`) | |
| conditional delete `pointInTime` | `Iterate` (`entity/service.go:1190-1191`, `:1594`) | |
| grouped stats `pointInTime` | pushdown declined for PIT → `Iterate` (`grouped_stats_service.go:265-299`) | |
| stats ×4 HTTP, stats ×2 gRPC | — | **declared, never read** (`entity/handler.go:396,421,452,477`; `internal/grpc/search.go:441,512`) — answer the current state |
| changes metadata `pointInTime` | `GetVersionMetadata{Until}` (`service.go:793-795`) | postgres has no `transaction_time` guard here |
| transitions `pointInTime` / `transactionId` | `GetAsAt` at T, or at `GetSubmitTime(tx)` (`transitions_handler.go:43-82`) | |

**No path rejects a future instant.** The SPI's own conformance cases read
at `h.Now()+1h` (`cyoda-go-spi/spitest/entity.go:952,991`).

**Async job instant.** Stored twice: in the `SearchOpts` JSON and in
`SearchJob.PointInTime` (`service.go:1014-1035`). Execution and reclaim on
another node read the JSON copy (`reaper.go:398-418`); paging reads the
column (`service.go:1460-1519`). A page whose `GetAsAt` returns
`ErrNotFound` is **skipped with a warning while `total` still counts it**
(`service.go:1495-1516`). The instant is not returned to the client
(`handler.go:421-433`).

## 3. Backends: how stamps are taken and become visible

### 3.1 memory

- Commit holds the factory-global `entityMu` write lock from before the
  stamp (`plugins/memory/txmanager.go:852`, stamp `:994`) to after the rows
  are published (`:1089-1204`). Every entity reader takes `entityMu.RLock`
  (`entity_store.go:535,574,1048`; `searcher.go:69-115`;
  `grouped_stats.go:135`). **No reader can see the stamp-to-publish gap.**
- Floor: `nextSubmitTime` = `max(clock.Now(), lastSubmitTime + 1µs)`
  (`txmanager.go:662-671`), global across tenants, starts at zero, not
  persisted. `Begin` reserves its snapshot as the floor (`:742-753`).
- **Defect:** the floor compares Go `time.Time` values that carry a
  monotonic reading (`clock.go:16`), so `After`/`Before` compare monotonic
  time, not wall time. A wall-clock step back produces stamps whose wall
  value is below earlier stamps. PIT filters compare wall time.

### 3.2 sqlite

- Commit holds a one-slot commit gate (`plugins/sqlite/txmanager.go:873`)
  through stamp (`:933`) and `sqlTx.Commit()` (`:1259`). **No entity reader
  takes the gate** (`grep gate` over `entity_store.go`, `searcher.go`,
  `grouped_stats.go`: no hits). A reader on `readDB` with `asAt ≥ stamp`
  misses the rows from `:933` to `:1259`. The same shape exists on direct
  saves (`entity_store.go:379` stamp → `:439` `BeginTx`).
- **`docs/CONSISTENCY.md` §1a is wrong** when it says memory *and sqlite*
  avoid the commit window structurally. Only memory does.
- Floor: integer microseconds (`txmanager.go:649-658`), so a wall step back
  is caught. Recovered on open from `SELECT MAX(submit_time) FROM
  entity_versions` (`txmanager.go:595-602`); **a query error is ignored and
  leaves the floor at 0** (fail-open).
- `Begin` waits for the gate and reserves its snapshot as the floor
  (`txmanager.go:738-789`). A file is locked to one process
  (`store_factory.go:86-93`).

### 3.3 postgres

- Transactional commit (`plugins/postgres/transaction_manager.go:201-328`):
  read-set validation `FOR SHARE` → `stampCommitInstant` (`:363-453`):
  `SELECT clock_timestamp()`, `UPDATE entity_versions … WHERE
  transaction_id`, `UPDATE entities`, `UPDATE sm_audit_events`, `INSERT
  submit_times` → `COMMIT` (`:294`). Isolation REPEATABLE READ (`:129`).
- Non-transactional Save/Delete/CompareAndSave: own READ COMMITTED
  transaction, `stampOwnCommitInstant` (`entity_store.go:395-415`), no
  `submit_times` row.
- **No floor, no monotonicity; no lock serialises commits.** A DB clock step
  back produces stamps below earlier ones.
- PIT SQL: `ev.valid_time <= $T AND ev.transaction_time <= CURRENT_TIMESTAMP`
  (`search_base.go:54-71`; `GetAsAt` `entity_store.go:604-611`). Under
  commit-instant stamping the `CURRENT_TIMESTAMP` guard no longer protects
  anything; after a DB clock step back it hides committed rows.
- Cluster: every node has its own pool and TM against one database; no
  cross-node coordination of stamps (`docs/plugins/POSTGRES.md:94-113`).
- No `application_name`, sequence or `pg_locks` use today. `pg_stat_activity`
  appears once, in `dropSchema` (`plugins/postgres/migrate.go:375`), a
  function only tests call, kept in a production file.

### 3.4 cassandra (commercial, `../cyoda-go-cassandra` at `1f53fb5`)

- HLC per node, 48-bit ms + 16-bit counter (`clock/hlc.go:8-82`); converted
  to `time.Time` at ms precision, dropping the counter (`hlc.go:140-143`).
  Cross-node propagation only via Redpanda headers.
- Commit HLC is taken before the COMMITTED write
  (`internal/tx/tx_coordinator.go:793` → `:814`); index and listing rows are
  written after it (`materialize`, `:844`); a materialize failure still
  returns success (`:841-864`, cassandra#97 item 2).
- Its "consistency clock" (`min_active_time`) is neither complete nor final;
  its read fast path was removed as unsound
  (`docs/CASSANDRA_BACKEND_DESIGN.md:713-720,2742-2747`). No committed
  watermark exists (`:2745-2746`).
- Async search is self-executing and receives cyoda-go's instant
  (`search/store.go:103-118`). A new transaction's snapshot is the local HLC
  at `Begin` (`internal/tx/tx_manager.go:127`) — cassandra#97 item 3, open.
- Pinned at SPI v0.8.3; cyoda-go pins a v0.8.5 pseudo-version (58 commits,
  14 breaking).
- Feasibility of the reserve-then-wait pattern: separate assessment, section 8.

## 4. Postgres probe (throwaway, postgres 17, non-superuser role)

Verified facts:

1. A sequence value set with `setval` is visible at once to another
   session, including inside that session's already-open REPEATABLE READ
   transaction, and survives the setter's rollback
   (`pg_sequence_last_value`).
2. A non-superuser role sees other sessions' advisory locks in `pg_locks`,
   from the same role and from another role.
3. A session that waits on another transaction's transaction-level advisory
   lock sees that transaction's rows in its next statement after it gets the
   lock (locks are released after the commit becomes visible).

Stress test: 16 clients each insert a row, take a stamp, sleep up to 4 ms
(stand-in for the stamp-to-commit window), write the stamp, commit; 2
checkers request an instant, count rows `≤` it, wait 30 ms, count again, and
also check that the newest committed stamp seen just before the request is
`≤` the instant.

| Instant used by the checker | Commits/s | Checks | Not final | Not complete |
|---|---|---|---|---|
| DB clock (`clock_timestamp()`), no wait | ~3,710 | 820 | **816** | 0 |
| Reserve-then-wait (below) | ~3,850 | 461 | **0** | **0** |
| No checker, plain stamp | ~3,630 | — | — | — |
| No checker, stamp with floor and marker | ~3,780 | — | — | — |

Mechanism probed: a stamp function takes a transaction-level advisory lock
keyed by its backend pid (the "in-flight marker", released when the
transaction ends), then under a short session-level advisory mutex sets
`s = max(clock, floor + 1µs)` and `setval(floor, s)`. The consistency-time
function, under the same mutex, sets `C = max(clock, floor)`, raises the
floor to `C`, lists granted markers from `pg_locks`, releases the mutex,
then takes and releases each listed marker in shared mode (waits for that
transaction to end). Commit throughput was not measurably affected. A first
variant that held the shared locks to the end of the reader's statement
halved commit throughput (it blocked the next commit on the same pooled
connection); releasing each lock at once removed the cost.

Design review probe (fresh-context reviewer, same container): markers keyed
by the transaction's xid instead of the backend pid, waited on with
transaction-level shared locks (no session lock to leak on a cancel, and no
later transaction reuses the key), `pg_locks` read after the mutex is
released: **0 violations in 2,942 checks**, 16 writers + 8 checkers, ~4,077
commits/s. A transaction-local `idle_in_transaction_session_timeout` set in
the middle of a transaction fires (bounds how long a session left behind by
a dead node holds its marker). `setval` assigns a transaction id, so a floor
raise is flushed at commit.

Not probed: a real DB clock step (the container shares the VM kernel clock);
cost with many listed markers.

## 5. Docs and contract text that state the current behaviour

- `api/openapi.yaml:7089-7090` (async submit): "Absent means the current
  committed state." — the code uses process `time.Now()`.
- `cmd/cyoda/help/content/search.md:236`: "if not provided, the current time
  is captured at submission".
- `cmd/cyoda/help/content/crud.md:395` (transitions): "the current time is
  used" — stale since `f671d285`.
- `cmd/cyoda/help/content/crud.md:579-614` and `docs/CONSISTENCY.md:68-71`:
  "A point-in-time read at instant T is stable once every transaction that
  started before T has finished" — wrong under commit stamping (a
  transaction that started before T and commits after T stamps after T).
- `docs/CONSISTENCY.md:82-97`: the sqlite claim (3.2) and "deliberately not
  scheduled".
- `docs/ARCHITECTURE.md:1475-1479` and DD-11 `:2470-2476`: "defaulting to
  `time.Now()`".
- gRPC schemas (`docs/cyoda/schema/search/EntityGetRequest.json:20`,
  `EntityStatsGetRequest.json:15`, `EntityStatsByStateGetRequest.json:15`,
  `EntityChangesMetadataGetRequest.json:20`) say "current consistency time".
- `e2e/parity/pit_time.go:12-38`, `plugins/postgres/pit_time_test.go:11-38`:
  comments still say postgres stamps with `CURRENT_TIMESTAMP`.
- `AuditEventDto.consistencyTime` (`api/openapi.yaml:11343-11346`) is
  declared and never populated (`internal/domain/audit/events.go:41-42`).

## 6. Cloud (Cyoda Platform 4.0.0-SNAPSHOT, read from sources jars)

- `C` = 6 ms below the earliest unfinished transaction's submit time, or
  `now − 6 ms` when idle; computed by each node from shared Cassandra tables
  and cached up to 1 s (`ConsistencyTimeCqlDao.java:227-304`,
  `ConsistencyTimeServiceImpl.java:80-121`). Stamps are taken at submit from
  the submitting node's clock; no HLC; the 6 ms look-back is the only skew
  allowance.
- **Final but not complete**: an earlier in-flight transaction holds `C`
  below already-acknowledged commits. Hence `waitForConsistencyAfter`, whose
  timeout result is discarded (`CoroutineTransactionService.kt:97-107`).
- Used as the default instant for async search, direct search, stats,
  delete-all, list-all (`ReportWrapperService.kt:92` and others). A plain
  PIT read does not consult `C`; a read with no PIT reads at node
  wall-clock now, contradicting its own docs.
- **A read later than `C`, or in the future, is served unfenced.**
- Not exposed in the API except `AuditEventDto.consistencyTime` (the `C`
  captured when the transaction was created).
- A stuck `C` is cleared by a controller that rolls back transactions older
  than 120 s.

## 7. Adjacent defects found

| Defect | Where |
|---|---|
| Stats (4 HTTP, 2 gRPC) accept `pointInTime` and ignore it | `entity/handler.go:396,421,452,477`; `internal/grpc/search.go:441,512` |
| gRPC get-all ignores `pointInTime` | `internal/grpc/search.go:284-287` |
| Async result page skips `ErrNotFound` silently, `total` disagrees | `search/service.go:1495-1516` |
| memory floor compares monotonic, not wall, time | `plugins/memory/txmanager.go:662-671,746` |
| sqlite floor recovery ignores a query error (floor 0) | `plugins/sqlite/txmanager.go:595-602` |
| postgres: no stamp floor; `transaction_time <= CURRENT_TIMESTAMP` hides rows after a DB clock step back | `transaction_manager.go:365`; `search_base.go:65`; `entity_store.go:604-611` |
| Stale docs listed in section 5 | |

## 8. Cassandra feasibility of reserve-then-wait

Code reading at `1f53fb5`; nothing run. **The pattern can be built from
mechanisms the plugin has; it cannot be made sound on today's code.**

- **Raising the floor cluster-wide is possible.** Shard owners already serve
  requests from other nodes over the plugin's Redpanda request/response
  (version-check pattern, `internal/tx/tx_coordinator.go:539-622`,
  `entity_shard_owner.go:380-531`), and every published message carries the
  sender's HLC, which the receiver applies before its handler runs
  (`internal/queue/queue_adaptor.go:124-131,172-178`). A flow: probe every
  shard owner for its clock, `C = max` of the answers, persist `C` as a
  durable floor, send `Fence(C)`; each owner raises its HLC above `C`, waits
  for its own stamped-not-visible commits, acknowledges. One or two message
  rounds over all shards per call, about the cost of a commit's version
  checks. `spi.ClusterBroadcaster` cannot do it (fire-and-forget).
- **Blockers (prerequisites in the plugin):**
  - **Every node appears to run every commit** — the tx-commands consumer
    has no consumer group and the handler no ownership check
    (`queue_adaptor.go:147-151`, `tx_coordinator.go:281-291,458-490`;
    verified by reading, not reproduced). One transaction can then carry
    several commit stamps. Filed as **cassandra#110**.
  - "Fully visible" cannot be known: a materialise failure still reports
    success (#97 item 2); an ambiguous COMMITTED write leaks the tracker
    entry (#64); recovery can skip a partly materialised transaction
    (`committed_write_log.go:189-197`).
  - Shard takeover does not carry forward the highest stamp issued on the
    transaction shard, or any `C` (`shard_takeover.go:267-282`); zombie
    owners (#29); HLC poisoning (#28) would spread through the fence.
- **#97 item 3** (a new transaction's snapshot below an acknowledged commit)
  is fixed by the same mechanism: snapshot at `Begin` = consistency time.
- **Stamps are ms-granular** (`clock/hlc.go:140-143`; reads widen `T` to the
  whole ms, `entity_store.go:1072`), so the plugin's reservation must push
  later stamps into a later millisecond than `C`'s.
- **Closest workable today:** single-node local reserve-then-wait. The
  cluster version needs cassandra#110, #97 items 1-2, #64 and a takeover
  floor first.
