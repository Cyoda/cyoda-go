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
3. **Guard = the entity's `CompareAndSave` token.** Fire iff the entity is
   unchanged since arming. One check encodes loopback-reset, supersede, and
   still-in-state — and it *is* the existing CAS, not new machinery (§5.3).
   **Verified sound** for core backends: `TransactionID` changes on every save
   incl. loopback (`plugins/*/…` — see §15 M5).
4. **One-shot criterion.** At the lateness-valid pickup, evaluate the criterion
   once: `true` → fire; `false` → **Declined**, terminal, no retry. The
   criterion is the intelligent gate; retrying overrides its judgment (§5.4).
5. **Coordinator scans, distribution delegates, idempotency + a lightweight
   atomic dispatch-claim guarantee correctness.** Pluggable coordinator (default
   lowest-live-`NodeID`) and distribution (default round-robin, plus `SELF`).
   Pure idempotency covers the *fire*; an **atomic visibility-claim on dispatch**
   (§6.1) additionally serialises the mutually-exclusive expire/cancel decision
   that idempotency alone cannot (§15 C2).
6. **Explicit fire-by-name stays rejected**, reworded (§7).

---

## 3. Architecture

Two layers:

- **Generic — `ScheduledTask`:** durable store + scanner service +
  coordinator/distribution + lifecycle + audit. "Do something at
  `scheduledTime`, with `timeoutMs` lateness tolerance."
- **First task type — `fire-transition`:** payload `{entityId, tenantId,
  modelRef, transition, sourceState, guardToken}`; per-type guard = token check;
  per-type action = fire via the engine.

```
 entity write (create / transition / loopback), one transaction
        │  (SQL: task store shares the pgx.Tx → atomic; memory: staged in tx buffer)
        ▼
  engine: cancel entity's pending tasks + arm current state's scheduled transitions
        │
        ▼   ScheduledTaskStore (SPI, per-backend): Upsert / ScanDue / ClaimForDispatch / Delete / CancelForEntity
                                    ▲                          │
 coordinator (lowest NodeID) ───────┘ scan due                │ atomic claim (CAS on visibility)
        │  DistributionStrategy picks target member            │
        │  ClaimForDispatch(id) → won?  (serialises dispatch)  │
        ▼  fire-and-forget peer RPC (PeerAuth/AEAD)  ExecuteScheduledTask(task)
 worker: system UserContext(task.tenantId) → engine.FireScheduledTransition(task)
        │  lateness gate → fire via CompareAndSave(guardToken) → one-shot criterion
        ▼  Fired / Declined / Expired  → audit + Delete(task)   (guard-fail → silent Delete)
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
| `visibleAfter` | int64 ms, nullable | Dispatch-claim deadline. `ClaimForDispatch` atomically sets `now + backoff` iff currently `≤ now`; scan excludes rows inside the window. Serialises dispatch under dual-coordinator (§6.1). |
| `payload` | json | fire-transition: `entityId`, `modelRef` (name+version), `transition`, `sourceState`, `guardToken` (§5.3). |
| `armedAt` | int64 ms | Observability. |
| `attemptCount` | int | Bumped on each claim; observability. |

**Scan index:** on `scheduledTime`, filtered `scheduledTime ≤ now AND
(visibleAfter IS NULL OR visibleAfter ≤ now)`, `ORDER BY scheduledTime`,
`LIMIT batch`, across tenants. Small table (pending only), isolated from the
entity hot path.

**No stored terminal status.** On resolution the row is **deleted** after the
audit event; the audit event is the durable record. Re-processing a
not-yet-deleted row is safe (guard fails on the second attempt), so delete need
not be atomic with the fire.

---

## 5. Engine integration

### 5.1 Arm-on-entry / cancel-on-exit — atomic with the entity write

The cascade currently **skips** scheduled transitions (`engine.go:618`). That
skip becomes **arm**. Within the transaction that writes the entity (create /
manual transition / self-loop / loopback), **only if the workflow contains any
scheduled transition** (in-memory check from the workflow def → zero store I/O
otherwise), after the entity is written:

1. `CancelForEntity(tenantId, entityId)` — delete the entity's pending tasks
   (belonged to the state just left); emit `…Cancelled` for any deleted.
2. For each scheduled transition `T` on the new current state: `Upsert` a task
   with `scheduledTime = now + T.Schedule.DelayMs`, `timeoutMs`, and
   `guardToken` = the arming write's **final committed txID** (§5.3, §15 M1).

**Atomicity** (this is the load-bearing premise — §15 C1):

- **SQL backends (sqlite/postgres):** the store factory hands every store a
  context-resolving querier that picks up the transaction's DB handle
  (`plugins/postgres/store_factory.go:117-127`; the audit store already works
  this way, `:168`). A `ScheduledTaskStore` built the same way executes its
  Upsert/Delete **inside the entity write's DB transaction** → atomic for free.
- **Memory backend:** the tx buffer is entity-only and `Commit` hard-codes the
  entity flush (`plugins/memory/txmanager.go:312-355`); the audit store writes
  immediately (`sm_audit_store.go:21`). So the memory `ScheduledTaskStore` must
  **stage task ops in the tx state and flush them in `Commit`** alongside
  entities. This is the one real tx-model addition — scoped to the memory
  plugin, not a cross-backend rewrite.

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
- **Scheduler door** (`FireScheduledTransition(ctx, task)`, new, internal): load
  entity → **lateness gate** (→ Expired) → `fireTransition` via
  `CompareAndSave(expectedTxID = guardToken)`. Criterion `false` → Declined;
  success → Fired.

Two doors, one mechanism. `Schedule != nil` at the manual door means "not
manually fireable"; the scheduler door is the authorised trigger. No special
consistency rights.

### 5.3 Guard token = `CompareAndSave` (verified sound)

`guardToken` = the entity's `TransactionID` captured at arming — specifically
the **final committed txID** of the arming write. (For a segmenting
COMMIT_BEFORE_DISPATCH arming cascade `finalTxID ≠ entry txID`, `service.go:319,
1472`; use `finalTxID` — §15 M1.) The fire is
`CompareAndSave(entity, expectedTxID = guardToken)`, applied at the **first
flush** of the firing transition, before its own cascade can segment — mirroring
how `IfMatch` is applied at first-segment flush (`service.go:1502` vs
`1525-1536`):

- Nothing wrote since arming → `TransactionID == guardToken` → CAS succeeds →
  fire. (Still-in-state implied: no write ⇒ no state change.)
- Loopback / other transition wrote → token differs → CAS `ErrConflict` → fire
  aborts → task obsolete → **silently deleted** (no Cancelled audit — §15 M3).

**Verified:** every commit stamps a fresh per-`Begin` txID
(`plugins/memory/txmanager.go:106`, applied `:329`) and CAS compares the latest
version's `TransactionID` against `expectedTxID`
(`plugins/postgres/entity_store.go:160,191`); loopback commits through the same
path (`service.go:1436`). So `TransactionID` changes on every save incl.
loopback — the guard is sound on core backends. **Unverified for the commercial
HLC backend** — phase 1 must confirm (§14).

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
   a. `ClaimForDispatch(id, visibleAfter = now + backoff)` — **atomic
      conditional update** (`SET visibleAfter WHERE visibleAfter ≤ now`),
      returns whether this node won. Only the winner proceeds. This serialises
      dispatch even under **transient dual-coordinator**, so a given task is
      dispatched once per window → one worker → no expire-vs-fire divergence
      (§15 C2). Per-backend: postgres/sqlite `UPDATE … RETURNING`, memory mutex,
      commercial LWT. It is a **visibility window, not a renewed lease** — no
      heartbeat, no ownership record.
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
- `engine.FireScheduledTransition(task)` → Fired / Declined / Expired → emit
  audit → `Delete(id)`. Guard-CAS-fail → **silent** `Delete`, no audit (§15 M3).
  The entity load/write routes through the normal cluster tx/proxy path.

Long tasks don't block the coordinator (fire-and-forget) — the reason for
delegation.

### 6.3 Multi-node correctness

- **Exactly-once fire** — guard-token CAS: any second delivery / residual
  dual-worker re-fires against a changed token → `ErrConflict` → no-op.
- **Single dispatch decision** — atomic `ClaimForDispatch` (§6.1) ensures one
  outcome-decider per task per window, closing the unguarded expire/cancel
  divergence that CAS alone can't (§15 C2).
- **At-least-once** — coordinator re-scans; the claim window suppresses storms
  and lapses on worker death → re-dispatch.
- **Coordinator uniqueness** — best-effort lowest-`NodeID` (unique memberlist
  name, `gossip.go:69`); converged view → one coordinator. Transient
  dual-coordinator (flux/partition) is safe (claim + idempotency) and collapses
  on convergence. **Failover latency** ≈ memberlist detection window: when the
  lowest-ID node dies, scanning pauses until it is reaped, so fires are
  **delayed, not lost** (§15 M4). No election protocol, no takeover.
- **Restart durability** — pending rows survive; the scan resumes.
- **Clock skew** — with dispatch serialised by the claim, skew no longer
  produces contradictory expire/fire; it only shifts effective fire time within
  `TimeoutMs` tolerance. Core backends rely on NTP-grade sync (acceptable for a
  timing primitive); the commercial backend has HLC.

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

`Cancelled` is emitted **only** from the definitive cancel-on-exit path (§5.1);
a guard-CAS-fail during a fire deletes silently (§15 M3). Emitted via
`recordEvent` (`engine.go:765`); the read model surfaces them with no new
endpoint. No new HTTP error codes → no `errors/<CODE>.md` additions.

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
| `CYODA_SCHEDULER_REDISPATCH_BACKOFF` | `30s` | Dispatch-claim visibility window. |

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
| Arm on entry (task row created, in-tx atomic) | ✓ | ✓ | ✓ | — |
| Arm rolled back with entity (no orphan on abort) | ✓ | — | — | — |
| Fire on time, no criterion | ✓ | ✓ | ✓ | — |
| Fire, criterion true (inline + FUNCTION) | ✓ | ✓ | ✓ | — |
| Decline, criterion false (one-shot, no retry) | ✓ | ✓ | ✓ | — |
| Expire (`lateness > timeoutMs`) — exact boundary | ✓ | — | — | — |
| No expire when `timeoutMs` nil (fires late) | ✓ | ✓ | ✓ | — |
| Cancel on state exit → `…Cancelled` audit | ✓ | ✓ | ✓ | — |
| Re-arm / clock reset on loopback | ✓ | ✓ | ✓ | — |
| Guard mismatch → silent delete, **no** audit | ✓ | ✓ | — | — |
| Cascade from `Next` after fire | ✓ | ✓ | ✓ | — |
| Unconditional scheduled cycle = heartbeat | ✓ | ✓ | — | — |
| Explicit fire-by-name → 400 (reworded) | ✓ | ✓ | ✓ | ✓ |
| Restart durability (pending survives) | — | ✓ | — | — |
| `ClaimForDispatch` atomic under dual-coordinator | ✓ | ✓ (isolated) | — | — |
| Idempotent double-delivery → one fire | ✓ | ✓ (isolated) | — | — |
| Failover: dead worker re-dispatch | ✓ | ✓ (isolated) | — | — |
| Peer RPC rejects unauthenticated / wrong-tenant | ✓ | — | — | ✓ |

Concurrency/multi-node scenarios are **isolated single-backend e2e** asserting
consistency (one fire, one winner), never the shared parity suite. Parity
scenarios register in `e2e/parity/registry.go`.

---

## 12. Cross-repo & release logistics

- **SPI additions** (cyoda-go-spi): `ScheduledTask` type, `ScheduledTaskStore`
  interface (incl. `ClaimForDispatch`, `ScanDue`, `CancelForEntity`),
  `StoreFactory.ScheduledTaskStore()`, new `StateMachineEventType` constants.
  Coordinated release per MAINTAINING.md — **SPI tag first, then the cyoda-go
  pin bump in one commit**; rides the in-flight v0.8.x SPI work; local
  composition via `go.work`, never a committed `replace`.
- **In-tree plugins:** memory (incl. **tx-buffer staging for atomic co-commit**,
  §5.1), sqlite, postgres each implement `ScheduledTaskStore` (a
  `scheduledTime`-indexed table + atomic conditional `ClaimForDispatch`).
  Per-plugin tests + `make test-all`.
- **Commercial backend (Cassandra):** implements `ScheduledTaskStore`
  (due-time-bucketed table; `ClaimForDispatch` via LWT). Leader-scan works over
  it — **no shard-ownership pull-up needed**. Also confirm the §5.3 guard-token
  invariant on HLC. Substantial commercial-side task — **flag for scheduling**;
  keep any courtesy PR strictly in-scope.
- **Gate 7 cloud-parity:** changes workflow runtime semantics Cloud mirrors →
  one `docs/cloud-parity/` file.
- **Gate 4 docs:** rewrite the "runtime not yet implemented" section of
  `cmd/cyoda/help/content/workflows.md` (add the §5.4 patterns), config help
  topic, `README.md`, `COMPATIBILITY.md` (SPI pin), `CHANGELOG`.

---

## 13. Suggested implementation phasing (for writing-plans)

1. SPI: `ScheduledTask`, `ScheduledTaskStore` (with `ClaimForDispatch`), factory
   accessor, SMEvent constants. Confirm guard-token invariant incl. HLC.
2. Store impls — memory first (incl. tx-buffer co-commit), then sqlite,
   postgres; each with the atomic conditional claim.
3. Engine: extract `fireTransition`; `FireScheduledTransition` (lateness + CAS
   guard at first flush + one-shot); replace cascade-skip with arm-on-entry;
   cancel-on-exit — all in-tx; `finalTxID` guard capture.
4. Scheduler service: coordinator + distribution, scan loop, atomic
   `ClaimForDispatch`, system `UserContext`, PeerAuth peer RPC
   `ExecuteScheduledTask`; wire `app.New`/`Shutdown`; injectable clock at arm +
   scan.
5. Explicit-fire reject rewording (+ gRPC).
6. Audit events (Cancelled from explicit-exit only).
7. Tests across the matrix.
8. Docs (Gate 4) + cloud-parity (Gate 7).
9. Commercial-backend store impl (separate repo, scheduled separately).

---

## 14. Open risks

- **Guard-token invariant on the commercial HLC backend** (§5.3) — verified for
  core; confirm for Cassandra in phase 1 with a test.
- **Memory tx-buffer co-commit** (§5.1) — the one genuine tx-model addition;
  scoped to the memory plugin. SQL backends atomic for free.
- **Infinite-heartbeat load** (§5.4) — unconditional scheduled cycles fire
  forever; documented operator opt-in, bounded by batch/scan config.
- **Failover latency** (§6.3) — fires delayed by the memberlist detection window
  on coordinator death; not lost.

---

## 15. Independent-review reconciliation

A fresh-context reviewer audited the first draft. Dispositions:

- **C1 (arm-in-tx atomicity)** — draft's "like every other store" was wrong for
  memory. **Corrected:** atomic for free on SQL backends (context-resolving
  querier shares the DB tx, `store_factory.go:117`); memory needs a tx-buffer
  co-commit (§5.1). Scoped, not a cross-backend rewrite.
- **C2 (unguarded expire/cancel vs idempotency)** — real. **Fixed** by the
  atomic `ClaimForDispatch` visibility-claim (§6.1) serialising the dispatch
  decision under dual-coordinator; idempotency remains the fire backstop.
- **H1 (background identity)** — **fixed:** system `UserContext(task.tenantId)`
  (§6.2).
- **H2 (peer RPC auth)** — **fixed:** rides existing `PeerAuth`/AEAD (§6.2).
- **H3 (one-shot vs accepted polling cycles)** — **clarified:** unconditional
  cycle = heartbeat; conditional scheduled = one-shot deadline; poll = tick +
  conditional cascade exits (§5.4). Documentation, not redesign.
- **M1 (CBD segmentation / which txID)** — **fixed:** guard = `finalTxID` at
  arm; CAS applied at first flush (§5.3).
- **M2 (round-robin YAGNI / extra hop)** — trimmed speculative fields; kept the
  pluggable seam + `SELF`. **Round-robin-vs-`SELF` default is an open call for
  the user** (round-robin spreads orchestration load — the reason for delegation
  — but adds a proxy hop to the entity owner, since no entity-ownership routing
  exists to target the owner directly).
- **M3 (spurious Cancelled after crash-then-refire)** — **fixed:** guard-fail
  deletes silently; `Cancelled` only from explicit exit (§5.3, §8).
- **M4 (failover window)** — **documented** (§6.3).
- **M5 (guard invariant)** — reviewer **confirmed sound** for core backends;
  folded into §5.3 as verified.
- **L1 (id collision)** — **fixed:** `id` includes `sourceState` (§4).
- **L2 (cross-tenant scan)** — **fixed:** tenant is a row column; single
  cross-tenant `ScanDue` (§4).
- **L3 (clock threading / e2e flake)** — **fixed:** inject the clock at arm +
  scan; exact-threshold tests are unit, not e2e (§11).
```
