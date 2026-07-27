# Scheduled-transition runtime — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's scheduled-transition runtime. cyoda-go is the authoritative
implementation; the behaviour described here is derived directly from its
design spec ([#251](https://github.com/Cyoda-platform/cyoda-go/issues/251))
and implemented code.

## 1. Timing contract

A transition carries a `schedule` object with exactly one of two mutually
exclusive timing sources:

- **`delayMs`** — a static delay. `scheduledTime = stateEntryTime + delayMs`.
- **`function`** — an arm-time compute-node callout that computes
  `scheduledTime` (and optionally an expiry) per entity. See §9.

Either way, no manual or automated-cascade call is involved once armed.
`stateEntryTime` (for `delayMs`) or the callout dispatch instant (for
`function`) is captured at **arm time** (the write that lands the entity in
the source state), not read back from entity metadata at fire time.

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

A `function`-timed transition's step 1 is not a plain upsert: the callout
result may resolve to **born expired** (§9), in which case the transition is
never armed at all, and any prior row for that transition is cancelled via
the same atomic write instead — see §9 for the resolution rules and the
distinct `EXPIRE` (not `CANCEL`) audit outcome this produces.

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

## 9. Arm-time Function computation (alternative to static `delayMs`)

A `schedule` object carries exactly one of `delayMs` (§1) or `function`
(this section) — never both, never neither. `function` names a compute-node
callout that determines the firing time (and optionally an expiry) **per
entity**, evaluated once at arm time — the same moment a static `delayMs`
would be added to `stateEntryTime`.

**Dispatch.** The callout is a generic Function calculation request — same
routing (`calculationNodesTags`), attach-entity, and context conventions as
an externalized processor or criterion, targeting a compute node reachable
by tag. It is dispatched synchronously, inside the entity write's
transaction, before the write commits — arming is part of the write, not a
follow-up step.

**Result contract.** The callout must return a result of kind `Schedule`
shaped:

```json
{ "fireAt": 1700000000000 }
```

or the relative form:

```json
{ "fireAfterMs": 60000, "expireAfterMs": 30000 }
```

- Exactly one of `fireAt` (absolute unix-millis) or `fireAfterMs` (relative
  to the arm-time dispatch instant) is required.
- At most one of `expireAt` (absolute) or `expireAfterMs` (relative to the
  *resolved* fire time, not to arm time) may additionally be present.
- A `fireAt`/`fireAfterMs` that resolves into the past is **not** an error —
  the transition arms to fire immediately, same as a due `delayMs` task
  would on the next scan.

**Resolution to the stored task.** The resolved values map onto the same
`ScheduledTask` fields a static `delayMs` schedule uses — there is no
separate wire shape at the storage layer:

- resolved fire time → `scheduledTime`
- resolved expiry minus resolved fire time (when an expiry was given) →
  `timeoutMs`
- no expiry given → `timeoutMs` is absent (never expires), identical to an
  absent static `timeoutMs`.

From the fire/expire/audit machinery's point of view (§1, §3, §6) a
`function`-resolved task is indistinguishable from a `delayMs`-resolved one
— the distinction exists only at arm time.

**Born expired.** If the resolved expiry is at or before the resolved fire
time, the transition is **born expired**: it is deliberately never armed at
all (no row is ever written with an already-passed timeout). Instead, in the
same atomic write as the rest of arming (§2):

- any pending task for this transition/source-state is cancelled, and
- a `SCHEDULED_TRANSITION_EXPIRE` audit event is recorded directly — not
  `SCHEDULED_TRANSITION_CANCEL` — because this is "never had a chance to
  fire," not "entity left the state that would have fired it."

The entity write itself still succeeds; a born-expired result is a valid,
handled outcome, not a failure.

**Failure surface — fail-closed.** A malformed or wrong-kind result (missing
or duplicate fire/expiry fields, an unknown field, a non-`Schedule`
`resultKind`) is a compute-node implementation defect: the engine rejects
the *entity write* rather than guessing or silently skipping the schedule,
so no state change ever commits with an unschedulable transition sitting
behind it. This is a `500` internal error (the failure is in the callout's
response shape, not the caller's request) — cyoda-go's code is
`SCHEDULE_FUNCTION_INVALID_RESULT`, not retryable as-is (the caller must fix
the Function's implementation first). See §10 for the distinct, retryable
failure mode of the compute node being *unreachable*.

## 10. Uniform infra-failure surface (503) across processor / criterion / function callouts

A compute node being unreachable, disconnected mid-request, or timing out is
an **infrastructure** failure, not a request-validation failure — it applies
identically regardless of which of the three callout kinds (externalized
processor, externalized/FUNCTION criterion, or a scheduled transition's
`function`) triggered the dispatch. cyoda-go surfaces exactly one retryable
`503` outcome for this failure class, uniformly across all three:

| Condition | Code |
|---|---|
| No connected compute member matches `calculationNodesTags` | `NO_COMPUTE_MEMBER_FOR_TAG` |
| The dispatch round-trip exceeds its deadline | `DISPATCH_TIMEOUT` |
| A cluster peer forward of the callout fails | `DISPATCH_FORWARD_FAILED` |
| The target compute member disconnects mid-request | `COMPUTE_MEMBER_DISCONNECTED` |

All four are `503`, all four are `retryable: true`. The entity write that
triggered the callout is rejected (nothing commits); the client's own retry
is expected to succeed once the compute-node infrastructure recovers — no
engine-side retry loop is implied. This is a request-time surface: a
`function` callout dispatched synchronously inside an entity write hits it
exactly like a processor or criterion callout would.

**Background re-arm on the fire path.** A scheduled transition's own fire
can itself trigger a *downstream* arm (the cascade lands the entity in a new
state that has its own `function`-timed schedule — §2). If that downstream
arm's callout hits one of the four conditions above, the entire fire
transaction rolls back (same CAS/atomicity guarantee as any other failed
write) and the task is left in place, retried on a later scan under the
existing best-effort redispatch throttle (§7) — there is no separate error
surface here because there is no synchronous caller to return a `503` to;
this path runs on the scan loop's own background goroutine. Operators are
expected to observe this via server-side error logging (structured with the
entity/transition/task identity), not via a client-facing status code.
