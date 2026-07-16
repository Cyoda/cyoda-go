# Scheduled State Transition Runtime — Design

**Issue:** #251 (parent). Depends on #259 (config shape + SPI, DONE, cyoda-go-spi v0.8.0) and #250 (schema cleanup, DONE).
**Date:** 2026-07-16
**Status:** Design — reconciled with one independent fresh-context review (findings folded in; see §15). Awaiting user review.

---

## 1. Goal

Make a workflow's declared scheduled transitions fire. A transition carrying
`Schedule{DelayMs, TimeoutMs}` (settled in #259, SPI v0.8.2) fires automatically
`DelayMs` after the entity enters the transition's source state, subject to a
lateness tolerance (`TimeoutMs`) and the transition's own criterion. Durable
across restarts, correct across cluster members (cyoda-go is primarily
multi-node).

Today the engine **skips** scheduled transitions during cascade and **rejects**
explicit fires. This design supplies the timer runtime.

### Non-goals (v1)

- Generic non-entity scheduled tasks (the abstraction is built to extend, but
  the only shipped task type is fire-transition).
- Distribution/coordinator strategies beyond the two defaults (seams are
  pluggable; better strategies later).
- An admin/query API for pending timers (the audit trail is the surface).
- Time-window / retry-until-condition criterion semantics (one-shot only).

---

## 2. Settled semantics (#259 / SPI v0.8.2)

- `scheduledTime = stateEntryTime + DelayMs`.
- At pickup `lateness = now − scheduledTime`. `TimeoutMs` nil → always attempt;
  non-nil and `lateness > TimeoutMs` → **drop, do not attempt** (Expired); else
  attempt.
- Mutually exclusive with `manual: true`. `DelayMs > 0` enforced at import.
- One instance of a generic `ScheduledTask` abstraction; `TimeoutMs` is the
  uniform lateness concept across future task types.

### Decisions taken (brainstorming + review reconciliation)

1. **Durability: separate durable `ScheduledTask` store (Approach B).** Not
   derived from entity state — chosen for scan isolation from the hot entity
   path, clean per-event expiry, and to realise the generic ScheduledTask
   abstraction. The task⟷entity dual-write is made **atomic** (§5.1).
2. **Arming captures at write time.** No new `stateEntryTime` on `EntityMeta`;
   the arming write records `scheduledTime = now + DelayMs`. A **loopback is a
   state re-entry that resets the timer** — every entity save leaving the entity
   in a scheduled state re-arms.
3. **Guard = re-read the task in the fire transaction** (no arm-time token).
   Fire iff the task row still exists, `entity.state == task.sourceState`, and
   `task.scheduledTime ≤ now`; the entity write is ordinary read-then-
   `CompareAndSave` at fire time. This encodes loopback-reset (a re-arm pushes
   `scheduledTime` forward), supersede, and still-in-state — and is
   **backend-agnostic**, avoiding the finalTxID-vs-entryTxID discrepancy that an
   arm-time token hits on postgres (§5.3, §15 F1).
4. **One-shot criterion.** At the lateness-valid pickup, evaluate the criterion
   once: `true` → fire; `false` → **Declined**, terminal, no retry. The
   criterion is the intelligent gate; retrying overrides its judgment (§5.4).
5. **Coordinator scans, distribution delegates, idempotency + an expiry grace
   band guarantee correctness — no dispatch lease.** Pluggable coordinator
   (default lowest-live-`NodeID`) and distribution (default round-robin, plus
   `SELF`). Pure idempotency (guard CAS) covers the *fire*. The
   mutually-exclusive *expire vs. fire* decision is made safe by an **expiry
   grace band** (§5.5): a worker expires only when `lateness > timeoutMs + grace`
   with `grace` sized above max clock skew, so no two members can ever decide
   fire-and-expire for the same task (§15 C2). At-least-once double-dispatch is
   already the engine's documented processor contract (idempotency is the
   author's responsibility, `workflows.md:170`) — so no dispatch lease is
   needed; `redispatchAfter` is a plain best-effort throttle only.
6. **Explicit fire-by-name stays rejected**, reworded (§7).

---

## 3. Architecture

Two layers:

- **Generic — `ScheduledTask`:** durable store + scanner service +
  coordinator/distribution + lifecycle + audit. "Do something at
  `scheduledTime`, with `timeoutMs` lateness tolerance."
- **First task type — `fire-transition`:** payload `{entityId, tenantId,
  modelRef, transition, sourceState}`; per-type guard = fire-time task re-read
  (§5.3); per-type action = fire via the engine.

```
 entity write (create / transition / loopback), one transaction
        │  (SQL: task store shares the pgx.Tx → atomic; memory: staged in tx buffer)
        ▼
  engine: cancel entity's pending tasks + arm current state's scheduled transitions
        │
        ▼   ScheduledTaskStore (SPI, per-backend): Upsert / ScanDue / MarkRedispatch / Get / Delete / ReconcileForEntity
                                    ▲                          │
 coordinator (lowest NodeID) ───────┘ scan due                │ plain redispatch throttle
        │  DistributionStrategy picks target member            │
        │  MarkRedispatch(id) (best-effort, no lease)          │
        ▼  fire-and-forget peer RPC (PeerAuth/AEAD)  ExecuteScheduledTask(task)
 worker: system UserContext(task.tenantId) → engine.FireScheduledTransition(task)
        │  re-read guard → grace-band gate → fire via read-then-CompareAndSave → one-shot criterion
        ▼  Fired / Declined / Expired  → delete-gated audit + Delete(task)   (guard-fail → silent Delete)
```

---

## 4. Data model — `ScheduledTask`

| Field | Type | Notes |
|---|---|---|
| `id` | UUID | Deterministic for fire-transition: `hash(type, entityId, sourceState, transition)` → re-arm upserts in place; **includes `sourceState`** so the same transition name in two states cannot collide (§15 L1). |
| `tenantId` | TenantID | A **row column, not a partition boundary** — one `ScanDue` returns due tasks across all tenants (§15 L2). |
| `type` | string | `"fire-transition"` (only value in v1). |
| `scheduledTime` | int64 ms | `armTime + DelayMs`. Due when `≤ now`. |
| `timeoutMs` | *int64 | From `Schedule.TimeoutMs`. |
| `redispatchAfter` | int64 ms, nullable | Best-effort throttle. Set (plain write) to `now + backoff` at dispatch; scan excludes rows inside the window so a long-running task isn't re-dispatched every tick. **Not** a lease and **not** conditional — correctness rests on the guard CAS + grace band, not on this. |
| `payload` | json | fire-transition: `entityId`, `modelRef` (name+version), `transition`, `sourceState`. No stored guard token — the guard is a fire-time re-read (§5.3). |
| `armedAt` | int64 ms | Observability. |
| `attemptCount` | int | Bumped on each claim; observability. |

**Scan index:** on `scheduledTime`, filtered `scheduledTime ≤ now AND
(redispatchAfter IS NULL OR redispatchAfter ≤ now)`, `ORDER BY scheduledTime`,
`LIMIT batch`, across tenants. Small table (pending only), isolated from the
entity hot path.

**No stored terminal status.** On resolution the row is **deleted**; the audit
event is the durable record. Re-processing a not-yet-deleted row is safe (guard
fails on the second attempt). **Terminal audit is delete-gated:** a worker emits
the terminal event (`Expired`/`Declined`/`Cancelled`) **only if its `Delete`
actually removed the row** — the delete is atomic per backend, so under a rare
dual-dispatch exactly one worker "wins" the audit (dedups duplicate Expired
lines without a claim protocol). `Fired` rides the `TRANSITION_MAKE`, already
single via the entity CAS.

---

## 5. Engine integration

### 5.1 Arm-on-entry / cancel-on-exit — atomic with the entity write

The cascade currently **skips** scheduled transitions (`engine.go:618`). That
skip becomes **arm**. Within the transaction that writes the entity (create /
manual transition / self-loop / loopback), **only if the workflow contains any
scheduled transition** (in-memory check from the workflow def → zero store I/O
otherwise), after the entity is written, **reconcile** the entity's pending
tasks to its *current* state:

1. For each scheduled transition `T` on the new current state: `Upsert` a task
   (deterministic id) with `scheduledTime = now + T.Schedule.DelayMs`,
   `timeoutMs`, `sourceState`.
2. Delete any pending task for this entity whose `sourceState ≠ the new current
   state` — emit `…Cancelled` for those (the entity genuinely left that state).

A loopback (same state) re-arms via the step-1 upsert and deletes nothing → **no
spurious `Cancelled`** (§15 F5). A transition `S→S'` deletes the `S` tasks
(`Cancelled`) and arms `S'`. A scheduled fire uses this same reconcile, so the
fired task is removed **atomically within the fire transaction** — no window for
a committed fire to leave a live task (§15 F3).

**Atomicity** (this is the load-bearing premise — §15 C1/F2):

- **Postgres only:** the store factory hands every store a context-resolving
  querier that picks up the transaction's `pgx.Tx` per call
  (`plugins/postgres/store_factory.go:106-127`; the audit store already works
  this way, `:168`). A `ScheduledTaskStore` built the same way executes its
  Upsert/Delete **inside the entity write's DB transaction** → atomic for free.
- **Memory *and* sqlite:** both buffer entity writes into `tx.Buffer` and open
  the only DB tx at commit-flush (memory `txmanager.go:312-355`; sqlite
  `entity_store.go:206` + `flushToSQLite` `txmanager.go:331`). So **both** must
  **stage task ops in the tx state and flush them in the same commit** alongside
  entities. This is the real tx-model addition — two backends, not one (§15 F2)
  — but still scoped to the plugins, not a core rewrite.

The dangerous non-atomic direction (entity commits into a scheduled state, task
write lost → silent lost fire) is thereby closed on every backend. The benign
direction (orphan task, entity rolled back) self-heals via the guard.

The `ScheduledTaskStore` is obtained from `StoreFactory` and is tx-scoped like
every other store.

### 5.2 `FireScheduledTransition` — the scheduler's door

Refactor `attemptTransition` (`engine.go:496`) to split policy from mechanism:

- **`fireTransition(...)`** — extracted mechanism: criterion (`evaluateCriterion`,
  `:706`) → processors → `TRANSITION_MAKE` + advance (`recordEvent`, `:765`) →
  cascade. No manual/scheduled opinion.
- **Manual/API door** (`attemptTransition` ← `ManualTransition`): find → reject
  if `disabled` → **reject if `Schedule != nil`** → `fireTransition`.
- **Scheduler door** (`FireScheduledTransition(ctx, task)`, new, internal):
  in one tx, apply the **re-read guard** (§5.3) → **grace-band lateness gate**
  (§5.5: fire-eligible if `lateness ≤ timeoutMs`; expire if
  `lateness > timeoutMs + grace`; between → drop-and-wait) → `fireTransition`
  via ordinary read-then-`CompareAndSave` at first flush. Criterion `false` →
  Declined; success → Fired.

Two doors, one mechanism. `Schedule != nil` at the manual door means "not
manually fireable"; the scheduler door is the authorised trigger. No special
consistency rights.

### 5.3 Guard = fire-time task re-read (backend-agnostic)

No token is captured at arm time — that approach breaks on postgres, which
persists the *entry* txID (not the final) after a segmenting cascade because
`Save` stamps `TransactionID` only when empty (`entity_store.go:46-48`,
`entity_doc.go:49`), whereas memory/sqlite persist the final txID; there is no
single arm-time value correct across backends (§15 F1).

Instead, the whole fire runs in **one transaction** (both `ScheduledTaskStore`
and `EntityStore` are tx-scoped), and the worker applies a **guard by re-read**
before acting:

1. Re-read the task by id. Absent → drop (cancelled elsewhere).
2. Read the entity. `entity.state ≠ task.sourceState` → the entity already moved
   on (transitioned out, or already fired) → **silent drop** (no audit), delete
   any stale row.
3. `task.scheduledTime > now` → re-armed to the future by a loopback since
   dispatch → **drop** (leave the row for its new time).
4. Else apply the grace-band decision (§5.5): fire / expire / decline.

The fire's entity write is **ordinary read-then-`CompareAndSave`** — expected =
the txID read in *this* fire transaction, applied at the **first flush** (reusing
the existing `IfMatch` first-segment machinery, `service.go:1502` vs
`1525-1536`, so a segmenting fire is handled). A concurrent loopback/transition
during the fire writes the entity → this CAS conflicts → the fire aborts and the
task is retried.

This encodes loopback-reset (step 3 + `scheduledTime` pushed forward), supersede
and still-in-state (step 2), and needs **no** knowledge of how any backend stamps
txIDs — so it is sound on core *and* commercial backends without the HLC caveat.
Phase 1 still confirms the commercial backend honours the same `scheduledTime`
re-read + first-flush CAS semantics.

### 5.4 One-shot criterion vs polling patterns (§15 H3)

One-shot is deliberate and composes with the cyclic/polling shapes the importer
accepts under `allowCycles`:

- **Unconditional scheduled cycle** (e.g. `S1 --schedule--> S2 --schedule-->
  S1`, no criteria — the accepted `allowCycles` "polling" case,
  `validate_import_test.go:606`): an intended **infinite heartbeat**, firing
  every `DelayMs`; each hop re-arms. Runs correctly. *Documented cost:* it fires
  forever per entity — batch/scan and cascade churn scale with the number of
  such entities; this is the operator's opt-in, surfaced in the help topic.
- **Conditional scheduled transition** (a criterion *on* the scheduled
  transition): a **one-shot deadline gate** — "at the deadline, fire iff the
  condition holds, else abandon (Declined)." Deliberate; not a poll.
- **Poll-until-condition** is expressed as an *unconditional* scheduled tick
  into a state whose **ordinary cascade transitions** carry the condition and
  exit when it holds. The condition lives in normal workflow structure, not in
  the timer.

The help topic documents these three so a conditional scheduled transition's
"Declined and stranded" outcome is expected, not surprising.

### 5.5 Expiry grace band — makes expire vs. fire skew-safe (§15 C2)

A worker resolves a due task in three zones of `lateness = now − scheduledTime`
(worker's own clock), where `grace = CYODA_SCHEDULER_EXPIRY_GRACE` (default
100 ms, sized ≥ 10× typical inter-node skew):

This decision is reached only *after* the guard (§5.3) passes — i.e. the entity
is still in `sourceState` and hasn't fired. It applies to a task that genuinely
hasn't resolved:

- `lateness ≤ timeoutMs` → **attempt** (evaluate criterion once → fire/decline).
- `timeoutMs < lateness ≤ timeoutMs + grace` → **drop and wait** (no fire, no
  expire, row left in place). A later scan resolves it (within `redispatchBackoff`,
  since the dispatch throttle hides it that long, §15 F4 — immaterial, expiry
  timing doesn't matter).
- `lateness > timeoutMs + grace` → **Expired** (delete + delete-gated audit).

`timeoutMs` nil → never expires (always attempt).

**What the grace band buys, precisely (§15 F3):** the fire zone (`≤ timeoutMs`)
and expire zone (`> timeoutMs + grace`) are separated by a gap `grace`. Two
members evaluating the *same task at the same instant* differ in `lateness` only
by the clock skew `δ`; if `grace > δ` they cannot land in opposite zones — so a
**simultaneous, skew-induced** fire-and-expire contradiction is impossible. It
does **not** by itself cover the *temporal* case (a member fires, then the row is
re-evaluated much later and looks expired); that is closed separately by the
in-transaction reconcile — a committed fire deletes its own task atomically
(§5.1), and the guard (§5.3 step 2) drops any task whose entity already left
`sourceState` before the expire branch is ever reached. Together: no
contradictory `Expired`. Residual is a member whose clock is >`grace` out of sync
(NTP/operator concern; `grace` is configurable).

---

## 6. Scheduler service (core, generic layer)

Constructed in `app.New`, stop-channel closed in `App.Shutdown` — mirrors the
reaper pattern (`app/app.go:412,455`). Wired with the `ScheduledTaskStore`,
`NodeRegistry`, the engine, `TransactionManager`, a peer client (PeerAuth), and
an **injectable clock** used at **both** arm-time and scan-time (§11, §15 L3).

### 6.1 Per tick (scan interval)

1. `CoordinatorStrategy.IsCoordinator(registry.List(), selfNodeID)` — default
   `LowestLiveNodeID`. Non-coordinators idle.
2. Coordinator: `ScanDue(now, batchSize)` (cross-tenant). For each task:
   a. `MarkRedispatch(id, redispatchAfter = now + backoff)` — **plain write**, a
      best-effort throttle so a long-running task isn't re-dispatched every tick
      (§4). Not conditional, not a lease. Under transient dual-coordinator both
      may still dispatch; that's safe — the fire is idempotent (guard CAS) and
      the expire is skew-safe (grace band, §5.5); double-dispatch is the same
      at-least-once replay the engine already documents (`workflows.md:170`).
   b. `target = DistributionStrategy.Pick(registry.List(), task)` — default
      round-robin (cursor in coordinator memory); `SELF` = self.
   c. Fire-and-forget peer RPC `ExecuteScheduledTask(task)` to `target`
      (local call when `target == self` or cluster disabled).

### 6.2 Worker — `ExecuteScheduledTask` handler (every node)

- **Auth:** the RPC rides the existing peer-auth wrapper (`PeerAuth`/AEAD,
  `internal/cluster/dispatch/forwarder.go:20-32`) — same channel as
  processor/criteria dispatch. Prevents cross-tenant task injection (§15 H2).
- **Identity:** the worker synthesises a **system `UserContext` scoped to
  `task.tenantId`** (a service principal, `changeUser = "scheduler"`), required
  because `TransactionManager.Begin` rejects a missing tenant
  (`plugins/memory/txmanager.go:98-104`) and the background loop has none
  (§15 H1).
- `engine.FireScheduledTransition(task)` opens one tx, applies the §5.3 re-read
  guard, then Fired / Declined / Expired → emit audit → resolve the row (Fired
  removes it via the reconcile; Expired/Declined delete it). A guard drop
  (§5.3 steps 1–3) is **silent** — no audit. The entity load/write routes through
  the normal cluster tx path.

Long tasks don't block the coordinator (fire-and-forget) — the reason for
delegation.

### 6.3 Multi-node correctness

- **Exactly-once fire** — guard-token CAS: any second delivery / residual
  dual-worker re-fires against a changed token → `ErrConflict` → no-op.
- **No expire/fire contradiction** — the grace band (§5.5) makes the two
  decisions mutually exclusive under any skew `< grace`, without a dispatch
  lease. Double-dispatch of a normal fire is the engine's already-documented
  at-least-once replay (processor idempotency is the author's contract,
  `workflows.md:170`) — no new hazard.
- **At-least-once** — coordinator re-scans; the `redispatchAfter` throttle
  suppresses storms and lapses on worker death → re-dispatch.
- **Coordinator uniqueness** — best-effort lowest-`NodeID` (unique memberlist
  name, `gossip.go:69`); converged view → one coordinator. Transient
  dual-coordinator (flux/partition) is safe (claim + idempotency) and collapses
  on convergence. **Failover latency** ≈ memberlist detection window: when the
  lowest-ID node dies, scanning pauses until it is reaped, so fires are
  **delayed, not lost** (§15 M4). No election protocol, no takeover.
- **Restart durability** — pending rows survive; the scan resumes.
- **Clock skew** — the grace band (§5.5) absorbs skew `< grace` so it cannot
  produce a contradictory expire/fire; it only shifts effective fire time within
  `TimeoutMs` tolerance. Core backends rely on NTP-grade sync (`grace` is
  configurable above observed skew); the commercial backend has HLC.

**Pluggable seams:** `CoordinatorStrategy` (default `LowestLiveNodeID`),
`DistributionStrategy` (default `round-robin`, plus `SELF`). Future strategies
drop in without touching the runtime.

---

## 7. Explicit fire-by-name — stays rejected, reworded

`POST …/transition/{scheduledName}` keeps **400 `TRANSITION_NOT_FOUND`**
(exists but not dispatchable from the caller's POV — same as `disabled`).
Reword `scheduledNotYetImplementedReason` (`engine.go:45`) to:

> `transition "X" in state "Y" is scheduled and fires automatically; it is not manually fireable`

Same over gRPC `ManualTransition`. Early firing is expressible by giving the
state an ordinary manual transition alongside the scheduled one.

---

## 8. Audit events (SPI additions)

New `StateMachineEventType` constants (`types.go:268` block):

| Outcome | Event |
|---|---|
| Armed (observability) | `SMEventScheduledTransitionArmed` = `SCHEDULED_TRANSITION_ARM` |
| Fired | `SMEventTransitionMade` (existing — the fire *is* a transition-make) + `SMEventScheduledTransitionFired` = `SCHEDULED_TRANSITION_FIRE` (scheduler-origin marker) |
| Declined (criterion false) | `SMEventTransitionCriterionNoMatch` (existing) |
| Expired (lateness) | `SMEventScheduledTransitionExpired` = `SCHEDULED_TRANSITION_EXPIRE` |
| Cancelled (explicit exit only) | `SMEventScheduledTransitionCancelled` = `SCHEDULED_TRANSITION_CANCEL` |

`Cancelled` is emitted **only** from the cancel-on-exit reconcile for tasks whose
entity left their `sourceState` (§5.1) — a loopback staying in state emits none
(§15 F5); a guard drop during a fire is silent (§5.3). Emitted via `recordEvent`
(`engine.go:765`); the read model surfaces them with no new endpoint. No new HTTP
error codes → no `errors/<CODE>.md` additions.

---

## 9. Configuration (Gate 4)

New `CYODA_`-prefixed vars (add to `DefaultConfig()`, config help topic,
`README.md` together):

| Var | Default | Meaning |
|---|---|---|
| `CYODA_SCHEDULER_ENABLED` | `true` | Kill switch for the scan loop. |
| `CYODA_SCHEDULER_SCAN_INTERVAL` | `1s` | Coordinator scan cadence. |
| `CYODA_SCHEDULER_BATCH_SIZE` | `100` | Max due tasks per scan. |
| `CYODA_SCHEDULER_DISTRIBUTION` | `round-robin` | `round-robin` \| `self`. |
| `CYODA_SCHEDULER_COORDINATOR` | `lowest-node-id` | Coordinator strategy. |
| `CYODA_SCHEDULER_REDISPATCH_BACKOFF` | `30s` | Best-effort re-dispatch throttle window (§4). |
| `CYODA_SCHEDULER_EXPIRY_GRACE` | `100ms` | Grace band above `timeoutMs` before expiring; size ≥ max inter-node clock skew (§5.5). |

---

## 10. HTTP / gRPC surface — error/status table

No new endpoints.

| Endpoint | Scenario | Status / code | Change |
|---|---|---|---|
| `POST /api/entity/{model}/{version}` | entity lands in a scheduled state | `201` + task armed (in-tx) | none |
| `PUT/PATCH …/{id}` (update/loopback) | re-arm (clock reset) | `200` + task re-armed (in-tx) | none |
| `POST …/transition/{name}` | `name` is scheduled | **400 `TRANSITION_NOT_FOUND`** | **message reworded** (§7) |
| `POST …/transition/{name}` | `name` is `disabled` | 400 `TRANSITION_NOT_FOUND` | unchanged |
| `POST /api/workflow-import` | `manual:true` + `schedule` | 400 `VALIDATION_FAILED` | unchanged (#259) |
| `POST /api/workflow-import` | `schedule.delayMs <= 0` | 400 `VALIDATION_FAILED` | unchanged (#259) |
| gRPC `ManualTransition` | scheduled name | `Success=false`, `Error.Code=TRANSITION_NOT_FOUND` | message reworded |
| any fire path | internal store/dispatch failure | `500` generic + ticket UUID | 5xx hygiene |

---

## 11. Test coverage matrix (scenario × layer)

**U** = unit (`internal/domain/...`, fake clock), **E** = running-backend e2e
(`internal/e2e`, real Postgres), **P** = cross-backend parity (`e2e/parity`,
memory/sqlite/postgres + commercial), **G** = gRPC (`internal/grpc`).

Time control: the injectable clock is threaded through **both** arm-time
(engine/entity) and scan-time, so exact `lateness`/`TimeoutMs` boundary tests
are **deterministic unit tests** — never wall-clock e2e (§15 L3). E2E covers
coarse happy-path firing within a generous window (small `DelayMs`), not exact
thresholds, to avoid CI flakes.

| Scenario | U | E | P | G |
|---|---|---|---|---|
| Arm on entry, atomic with entity (all 3 backends) | ✓ | ✓ | ✓ | — |
| Arm rolled back with entity (no orphan on abort) | ✓ | — | ✓ | — |
| Fire on time, no criterion | ✓ | ✓ | ✓ | — |
| Fire, criterion true (inline + FUNCTION) | ✓ | ✓ | ✓ | — |
| Decline, criterion false (one-shot, no retry) | ✓ | ✓ | ✓ | — |
| Expire (`lateness > timeoutMs + grace`) — boundary | ✓ | — | — | — |
| No expire when `timeoutMs` nil (fires late) | ✓ | ✓ | ✓ | — |
| Cancel on state exit → `…Cancelled` audit | ✓ | ✓ | ✓ | — |
| Loopback in scheduled state → re-arm, **no** `Cancelled` | ✓ | ✓ | ✓ | — |
| Re-arm / clock reset on loopback | ✓ | ✓ | ✓ | — |
| Guard re-read: superseded/moved-on → silent drop | ✓ | ✓ | — | — |
| Fire under segmenting (CBD) cascade — CAS at 1st flush | ✓ | ✓ | — | — |
| Cascade from `Next` after fire | ✓ | ✓ | ✓ | — |
| Unconditional scheduled cycle = heartbeat | ✓ | ✓ | — | — |
| Explicit fire-by-name → 400 (reworded) | ✓ | ✓ | ✓ | ✓ |
| Restart durability (pending survives) | — | ✓ | — | — |
| Grace band: no fire+expire under skew (fake clock) | ✓ | — | — | — |
| Drop-and-wait in grace band, resolves next round | ✓ | — | — | — |
| Delete-gated audit: one Expired under dual-dispatch | ✓ | ✓ (isolated) | — | — |
| Idempotent double-delivery → one fire | ✓ | ✓ (isolated) | — | — |
| Failover: dead worker re-dispatch | ✓ | ✓ (isolated) | — | — |
| Peer RPC rejects unauthenticated / wrong-tenant | ✓ | — | — | ✓ |

Concurrency/multi-node scenarios are **isolated single-backend e2e** asserting
consistency (one fire, one winner), never the shared parity suite. Parity
scenarios register in `e2e/parity/registry.go`.

---

## 12. Cross-repo & release logistics

- **SPI additions** (cyoda-go-spi): `ScheduledTask` type, `ScheduledTaskStore`
  interface (`Upsert`, `ScanDue`, `MarkRedispatch`, `Get`/re-read for the guard,
  `Delete` returning rows-affected for the delete-gated audit,
  `ReconcileForEntity` = upsert-current-state + delete-other-state tasks),
  `StoreFactory.ScheduledTaskStore()`, new `StateMachineEventType` constants.
  `ScheduledTaskStore` is a **tenant-pattern exception** like `AsyncSearchStore`
  — its `ScanDue` is cross-tenant (obtained with `context.Background()`, tenant
  per row), so its impls must not follow the per-tenant-at-construction pattern,
  and the postgres impl must run the scan under a role/path that RLS does not
  filter to a single tenant (§15 F7). Coordinated release per MAINTAINING.md —
  **SPI tag first, then the cyoda-go pin bump in one commit**; rides the in-flight
  v0.8.x SPI work; local composition via `go.work`, never a committed `replace`.
- **In-tree plugins:** **memory and sqlite** both need **tx-buffer staging for
  atomic co-commit** (§5.1/§15 F2 — both are buffer-and-flush); postgres is
  atomic via the context-resolving querier. Each implements a
  `scheduledTime`-indexed table; plain `MarkRedispatch`; `Delete` returns
  rows-affected. Per-plugin tests + `make test-all`.
- **Commercial backend:** implements `ScheduledTaskStore` (due-time-bucketed
  table). Leader-scan works over it — **no shard-ownership pull-up needed**. Must
  confirm two backend-specific invariants: the §5.3 guard-token (token changes on
  every save incl. loopback) and the §5.1 arm/cancel atomicity with the entity
  write. Tracked as a separate implementation issue in the commercial backend
  repo; substantial, scheduled independently of v1. Keep any courtesy PR strictly
  in-scope.
- **Gate 7 cloud-parity:** changes workflow runtime semantics Cloud mirrors →
  one `docs/cloud-parity/` file.
- **Gate 4 docs:** rewrite the "runtime not yet implemented" section of
  `cmd/cyoda/help/content/workflows.md` (add the §5.4 patterns), config help
  topic, `README.md`, `COMPATIBILITY.md` (SPI pin), `CHANGELOG`.

---

## 13. Suggested implementation phasing (for writing-plans)

1. SPI: `ScheduledTask`, `ScheduledTaskStore`, factory accessor, SMEvent
   constants.
2. Store impls — memory + sqlite (both incl. tx-buffer co-commit), then
   postgres; plain `MarkRedispatch`; `Delete` returns rows-affected; cross-tenant
   `ScanDue` (tenant-pattern exception).
3. Engine: extract `fireTransition`; `FireScheduledTransition` (re-read guard +
   grace-band gate + read-then-CAS at first flush + one-shot); replace
   cascade-skip with the arm/cancel **reconcile** — all in-tx.
4. Scheduler service: coordinator + distribution, scan loop, `redispatchAfter`
   throttle, grace band, system `UserContext`, PeerAuth peer RPC
   `ExecuteScheduledTask`; wire `app.New`/`Shutdown`; injectable clock at arm +
   scan.
5. Explicit-fire reject rewording (+ gRPC).
6. Audit events (Cancelled from explicit-exit only).
7. Tests across the matrix.
8. Docs (Gate 4) + cloud-parity (Gate 7).
9. Commercial-backend store impl (separate repo, scheduled separately).

---

## 14. Open risks

- **Memory + sqlite tx-buffer co-commit** (§5.1/§15 F2) — the genuine tx-model
  addition; two buffer-and-flush plugins, not one. Postgres atomic for free.
- **Timer starvation from frequent writes** (§15 F6) — because every save
  re-arms, an entity written more often than `DelayMs` never fires its scheduled
  transition. A direct consequence of "a data write resets the clock" — document
  as a known semantic in the help topic. *(Pending user confirmation this is
  intended vs. resetting only on state entry.)*
- **Infinite-heartbeat load** (§5.4) — unconditional scheduled cycles fire
  forever; documented operator opt-in, bounded by batch/scan config.
- **Failover / RPC latency** (§6.3, §15 F8) — fires delayed by the memberlist
  detection window on coordinator death, and by up to `redispatchBackoff` on a
  silently-failed delegation RPC; not lost.
- **Commercial backend** (§12) — must honour the §5.3 re-read + first-flush CAS
  guard and §5.1 atomic reconcile; confirmed in its own implementation issue.

---

## 15. Independent-review reconciliation

### 15.1 First review (pre-grace-band draft)

A fresh-context reviewer audited the first draft. Dispositions:

- **C1 (arm-in-tx atomicity)** — draft's "like every other store" was wrong for
  memory. Corrected here to "atomic on SQL backends," then **further corrected by
  F2**: only *postgres* is free; sqlite buffers like memory, so memory + sqlite
  both need the tx-buffer co-commit (§5.1).
- **C2 (unguarded expire vs fire under dual-coordinator + skew)** — real, but
  cosmetic (entity fires once via CAS; damage is a contradictory Expired audit
  line). **Fixed** by an **expiry grace band** (§5.5) that makes fire and expire
  provably mutually exclusive for skew `< grace` — no dispatch lease. The atomic
  claim I first proposed was dropped: its other purpose (preventing double-
  *dispatch* → double-*processor* side effects) is unnecessary because the engine
  **already** documents processors as at-least-once with author-owned
  idempotency (`workflows.md:170`), so a double-dispatch is no new hazard.
  Duplicate (non-contradictory) Expired audit under rare dual-dispatch is deduped
  by the delete-gated audit (§4). Net: simpler than the draft.
- **H1 (background identity)** — **fixed:** system `UserContext(task.tenantId)`
  (§6.2).
- **H2 (peer RPC auth)** — **fixed:** rides existing `PeerAuth`/AEAD (§6.2).
- **H3 (one-shot vs accepted polling cycles)** — **clarified:** unconditional
  cycle = heartbeat; conditional scheduled = one-shot deadline; poll = tick +
  conditional cascade exits (§5.4). Documentation, not redesign.
- **M1 (CBD segmentation / which txID)** — the `finalTxID`-at-arm fix here was
  **superseded by F1**: no arm-time token at all; guard is a fire-time re-read
  (§5.3).
- **M2 (round-robin hops)** — the draft's "proxy hop to the entity owner" was
  **wrong**: proxy forwarding only fires for an *open-transaction* token
  (`internal/cluster/proxy/http.go:18-27`); a scheduled fire opens its own tx and
  writes the entity directly on the worker. So it's **one** hop (coordinator →
  worker) with round-robin, **zero** with `SELF` — no owner hop. **Decision:
  round-robin default** (kept) to exercise the delegation path and spread
  orchestration load off the coordinator under high scheduling volume; `SELF`
  available. **Back-pressure is tracked as #416** — today the only bounds are
  batch size × scan interval × the redispatch throttle; real load-feedback /
  bounded in-flight dispatch is that follow-on, not v1.
- **M3 (spurious Cancelled after crash-then-refire)** — **fixed:** guard-fail
  deletes silently; `Cancelled` only from explicit exit (§5.3, §8).
- **M4 (failover window)** — **documented** (§6.3).
- **M5 (guard invariant)** — reviewer confirmed `TransactionID` changes on every
  save; but **F1 showed that was the wrong invariant** (postgres persists the
  *entry* txID after segmentation), so the arm-time-token guard was dropped
  entirely for the re-read guard (§5.3). The invariant is now moot.
- **L1 (id collision)** — **fixed:** `id` includes `sourceState` (§4).
- **L2 (cross-tenant scan)** — **fixed:** tenant is a row column; single
  cross-tenant `ScanDue` (§4).
- **L3 (clock threading / e2e flake)** — **fixed:** inject the clock at arm +
  scan; exact-threshold tests are unit, not e2e (§11).

### 15.2 Second review (post-grace-band revision)

A second fresh-context reviewer audited the revised design. Dispositions:

- **F1 (arm-time `finalTxID` guard is *wrong* on postgres → silent lost fire)** —
  confirmed real (`entity_store.go:46-48`, `entity_doc.go:49`: postgres persists
  the entry txID after a segmenting cascade; memory/sqlite persist the final).
  **Fixed by redesign:** no arm-time token — the guard is a fire-time task
  re-read (existence + `sourceState` + `scheduledTime ≤ now`) plus ordinary
  read-then-CAS for the entity write (§5.3). Backend-agnostic; removes the HLC
  caveat too.
- **F2 (sqlite is *not* atomic-for-free)** — confirmed (`sqlite/entity_store.go:206`
  buffers like memory). **Corrected:** memory *and* sqlite need the tx-buffer
  co-commit; only postgres is free (§5.1, §12, §13).
- **F3 (grace "proof" oversold)** — correct: the band only covers *simultaneous*
  skew, not fire-then-later-expire. **Fixed:** the fired task is removed
  atomically by the in-tx reconcile (§5.1) and the guard drops a moved-on entity
  before the expire branch (§5.3 step 2); wording softened (§5.5).
- **F4 (redispatch throttle vs "next scan" timing)** — **corrected:** an in-band
  drop resolves within `redispatchBackoff`, not one scan (§5.5). Immaterial.
- **F5 (spurious `Cancelled` on loopback)** — **fixed:** arm is now a
  **reconcile** — delete only tasks whose `sourceState ≠ current state`; a
  loopback emits no `Cancelled` (§5.1, §8).
- **F6 (timer starvation from frequent writes)** — a consequence of "every save
  re-arms." **Pending user confirmation** (intended + documented, vs. reset only
  on state entry); tracked in §14.
- **F7 (cross-tenant scan / RLS)** — **addressed:** `ScheduledTaskStore` is a
  tenant-pattern exception like `AsyncSearchStore`; postgres scan must not be
  RLS-filtered to one tenant (§12).
- **F8 (RPC-failure latency ≈ `redispatchBackoff`)** — **documented** (§14).
- **F9 (round-robin default is YAGNI; prefer `SELF`)** — reviewer's call noted;
  round-robin kept as a deliberate user decision (load-spread exposure), with
  its delegation-surface concerns (F3/F8) now resolved.
- **Confirmed sound:** postgres atomic-arm-in-tx, `CompareAndSave` compares
  `transaction_id`, the cascade-skip→arm site, and bounded per-fire cascade
  (`maxCascadeDepth`/`maxStateVisits`) so an unconditional scheduled cycle can't
  runaway (§5.4).
```
