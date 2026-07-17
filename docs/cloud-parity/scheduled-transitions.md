# Scheduled-transition runtime — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's scheduled-transition runtime. cyoda-go is the authoritative
implementation; the behaviour described here is derived directly from its
design spec ([#251](https://github.com/Cyoda-platform/cyoda-go/issues/251))
and implemented code.

## 1. Timing contract

A transition carrying `schedule: {delayMs, timeoutMs}` fires automatically at
`scheduledTime = stateEntryTime + delayMs` — no manual or automated-cascade
call is involved. `stateEntryTime` is captured at **arm time** (the write
that lands the entity in the source state), not read back from entity
metadata at fire time.

At pickup, `lateness = now − scheduledTime` (the worker's own clock):

- `lateness ≤ timeoutMs` → attempt (evaluate the criterion once).
- `timeoutMs < lateness ≤ timeoutMs + grace` → drop and wait; a later scan
  resolves it. `grace` is a small fixed band (cyoda-go default 100 ms,
  configurable) sized above expected inter-node clock skew.
- `lateness > timeoutMs + grace` → **Expired** — dropped, never attempted.
- `timeoutMs` absent → never expires; fires whenever picked up.

The grace band exists so two cluster members observing the same task at the
same instant, differing only by clock skew, cannot land on opposite sides of
the fire/expire boundary and produce a contradictory outcome.

## 2. Arm-on-entry / cancel-on-exit — atomic with the entity write

Every entity write that changes or re-confirms current state must, within
the **same transaction**:

1. Upsert a pending task for each scheduled transition on the *new* current
   state (`scheduledTime = now + delayMs`).
2. Delete any pending task whose `sourceState` is no longer the entity's
   current state, emitting `SCHEDULED_TRANSITION_CANCEL` for each.

A loopback (write that leaves the entity in the same state) re-arms via step
1 and cancels nothing — no spurious `CANCEL` event. This dual-write must be
atomic: an entity commit that lands in a scheduled state with no
corresponding armed task is a silent lost fire, and is the failure mode this
contract exists to prevent. A committed fire removes its own task in the
same transaction — there is no window where a fired task is still live.

## 3. One-shot criterion

The transition's criterion is evaluated **exactly once**, at fire time, not
polled or retried:

- `true` (or no criterion) → fire normally: processors run, state advances,
  `TRANSITION_MAKE` is recorded, `SCHEDULED_TRANSITION_FIRE` marks the
  scheduler origin.
- `false` → **Declined**: `TRANSITION_NOT_MATCH_CRITERION` is recorded, the
  entity stays in its source state, and the task is **not** retried or
  re-armed. This is deliberate — a conditional scheduled transition is a
  one-shot deadline gate, not a poll. Poll-until-condition is expressed at
  the workflow level (an unconditional scheduled tick into a state whose
  ordinary transitions carry the condition), never as timer-level retry.

## 4. Explicit fire — stays rejected

`POST/PUT …/entity/.../transition/{scheduledName}` (HTTP) and the gRPC
`ManualTransition` equivalent both return `400 TRANSITION_NOT_FOUND` for a
scheduled transition — same code as an unreachable/disabled transition, same
semantic ("exists but not dispatchable from the caller's POV"). Message:

> `transition "X" in state "Y" is scheduled and fires automatically; it is
> not manually fireable`

Early firing is not supported through the scheduled transition itself; model
it as an ordinary manual transition alongside the scheduled one if the
workflow needs an escape hatch.

## 5. Settled-interval semantic (any write resets the timer)

The timer is re-armed by **every** write that leaves the entity in the
source state — not just transitions. A routine payload update or a
self-loop pushes `scheduledTime` forward by `delayMs` from that write. An
entity written more often than its `delayMs` interval never reaches the
scheduled fire. This is intentional (arming is a plain consequence of "which
scheduled transitions apply to the current state," re-evaluated on every
write) and must not regress to an entry-time-only timer.

## 6. Audit event set

| Outcome | Event | Notes |
|---|---|---|
| Armed | `SCHEDULED_TRANSITION_ARM` | Emitted on every arm/re-arm, including loopback re-arms. |
| Fired | `SCHEDULED_TRANSITION_FIRE` + `TRANSITION_MAKE` | Scheduler-origin marker alongside the ordinary transition-made event. |
| Declined | `TRANSITION_NOT_MATCH_CRITERION` | Existing event, reused — no dedicated "declined" event. |
| Expired | `SCHEDULED_TRANSITION_EXPIRE` | Delete-gated: only the worker whose `Delete` actually removed the row emits it. |
| Cancelled | `SCHEDULED_TRANSITION_CANCEL` | Only when the entity genuinely left `sourceState`; a same-state loopback never cancels. |

A guard-fail drop during a fire attempt (task already gone, entity already
moved on, task re-armed to the future) is **silent** — no audit event — by
design; it is not a distinct outcome from Cloud's point of view.

## 7. Multi-node correctness contract

- **Coordinator/distribution are pluggable strategies**, not a fixed
  protocol; cyoda-go's defaults are lowest-live-node-ID coordination and
  round-robin distribution, but Cloud may implement equivalent or different
  strategies as long as the properties below hold.
- **Exactly-once fire, state-correct always.** The fire's entity write is
  read-then-compare-and-swap; a second delivery (dual-coordinator, retry)
  conflicts and no-ops. State is exactly-once correct under any
  interleaving.
- **No fire/expire contradiction.** The grace band (§1) makes the two
  decisions mutually exclusive for any clock skew below the grace window.
- **At-least-once dispatch, not at-most-once.** A best-effort redispatch
  throttle avoids storming a task every scan tick, but is not a lease —
  under a transient dual-coordinator both may dispatch. This is safe
  because the fire is idempotent (CAS) and expiry is skew-safe (grace
  band); double-dispatch of a normal fire is the same at-least-once
  processor-idempotency contract the engine already documents for ordinary
  transitions.
- **Failover delays fires, never loses them.** Pending tasks are durable;
  if the coordinator dies, scanning pauses until failover completes, then
  resumes from the durable store. No election protocol is required to
  match this contract — any mechanism that converges on one coordinator
  and durably persists pending tasks satisfies it.

## 8. Accepted edge: duplicate `Fired` audit under transient dual-coordinator (memory/sqlite only)

On cyoda-go's memory and sqlite backends, the audit store is **not**
transaction-scoped, so a *losing* coordinator's `SCHEDULED_TRANSITION_FIRE`
audit write can land durably even though its entity/task write loses the
CAS race and is correctly discarded — producing two `FIRE` audit lines for
one real state transition. **Entity state remains exactly-once correct**
(the CAS guarantees it); only the audit trail can cosmetically duplicate,
and only on these two backends, and only under a rare transient
dual-coordinator window. cyoda-go's postgres backend does not exhibit this
(its audit store is transaction-scoped) and treats it as accepted rather
than closed — the fix would require making the shared audit subsystem
transactional on memory/sqlite, which is out of scope for this feature.
Cloud's audit store, if transactional, need not reproduce this dup; if
Cloud's audit path is similarly non-transactional under a comparable
concurrency window, the same accepted-edge reasoning applies: state
correctness is the invariant to preserve, not audit-line count.
