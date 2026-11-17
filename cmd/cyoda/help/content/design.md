---
topic: design
title: "design — practices for entity models and workflows"
stability: stable
see_also:
  - models
  - workflows
  - crud
  - search
  - scheduled-tasks
---

# design

## NAME

design — practices for designing entity models and workflows on cyoda.

## DESCRIPTION

This topic gives advice, not rules. Each practice names the reference topic that states the rules it relies on.

## ENTITIES

- **Keep data tree-shaped.** Nested objects and arrays are first-class: search conditions and workflow criteria address array elements (`$.items[*].sku`, `$.items[0]`; see `cyoda help search`). Do not flatten a document into relational tables. Grouped statistics are the exception: a `groupBy` path or an aggregation `field` cannot address an array element, so keep a value you group or aggregate on at a scalar path (`cyoda help crud`, grouped statistics).
- **Compute aggregates; do not maintain them.** Avoid counter entities and running totals. Writes that update the same counter conflict, and each loser gets `409 CONFLICT` (`cyoda help errors CONFLICT`). Read the figures instead: counts by state, grouped statistics with sums, averages, minimum, maximum and standard deviation, at the current time or at a `pointInTime` (`cyoda help crud`). Keep an aggregate in an entity of its own only when the volume makes these reads too slow.
- **Link entities by entity id.** Store the id of the entity you refer to in a field, for a one-to-one, one-to-many or many-to-many relation. It is a convention: cyoda has no referential integrity, a search reads one model and does not join, and deleting an entity another one refers to is not refused.

## WORKFLOWS

- **Treat workflows as first-class.** A workflow is the shared language of domain experts and developers. Name states and transitions so that the workflow reads as the business process.
- **Drive processing with automated transitions.** After every write, the engine fires the first automated transition of the current state — `manual: false`, not `disabled`, no `schedule` — whose criterion matches, in declaration order, and repeats from the state it reaches (`cyoda help workflows`, ENGINE EXECUTION). An automated transition with no criterion always matches, so put it last: the ones after it are never reached, and import does not warn about them.
- **Use manual transitions for steps the outside world triggers.** A user acting through your application, a batch job, another system: each fires a transition by naming it on a save (`PUT` or `PATCH /entity/{format}/{entityId}/{transition}`). `manual: true` means the engine never fires the transition on its own. It does not stop a client from naming a `manual: false` transition, and cyoda has no per-user permissions (`cyoda help auth`): your application decides which user may fire which transition.
- **Cascade across entities from processors.** A processor's callback that creates or updates another entity, or fires a transition on it, starts that entity's workflow. Whether the callback runs in the transaction of the processor's operation depends on the processor's `executionMode`, and a failed callback write can fail that operation (`cyoda help workflows`, PROCESSORS). A callout never writes the entity it runs for: a processor changes its own entity only by returning data.
- **Balance the number of states.** Too few states hide the process inside processors; too many make it hard to follow. In one cascade, the engine visits a state at most `CYODA_MAX_STATE_VISITS` times (default `10`) and takes at most 100 steps.
- **Re-evaluate with a loopback, not by naming an automated transition.** A loopback is a save that names no transition: the engine runs the automated transitions from the current state again, so a transition whose criterion did not match before gets another chance. `PATCH` with the empty patch `{}` does this without changing the data, and still commits a new version (`cyoda help crud`). A loopback also re-arms the state's scheduled transitions with a freshly computed time (`now + delayMs` for a fixed delay), so repeated loopbacks can postpone a scheduled escalation (`cyoda help workflows`, SCHEDULED TRANSITIONS). A loopback is for clients: a processor never loops back its own entity.
- **Build explicit validation cycles.** When a change makes an entity invalid, route it back to a state before the validation step. Import refuses a cycle made only of automated or scheduled transitions without criteria, unless the import sets `allowCycles: true`; one criterion on the cycle, or one manual transition, is enough.
- **Model error states.** Do not model only the happy path: put an explicit error state, such as `Errored`, on the straight-through path. An automated transition out of it needs a criterion, or the entity leaves it at once. A manual transition out of it keeps the entity there until a client names it, and a scheduled transition can escalate after a delay. Find the entities that need attention with the counts by state (`cyoda help crud`) or a search on the state (`cyoda help search`). For an action on entering the state, put a processor on the transition into it; `executionMode: ASYNC_NEW_TX` keeps that processor's failure from failing the operation.
- **Add transitions back up the flow.** Paths to earlier states handle retries, corrections and exceptions.
- **Explain a refusal.** A function condition in a criterion — the whole criterion or a member of a group — can return a `reason` with `matches: false`; cyoda keeps its first 2048 characters. For a transition a client named, the reason is in the `400 WORKFLOW_FAILED` detail. For an automated transition, it is in the `TRANSITION_NOT_MATCH_CRITERION` audit event, which is kept when the operation commits. A criterion with no function condition records only `criterion did not match`; state its intent in `criterionAnnotations`, which the engine stores but never shows in an error (`cyoda help workflows`, CRITERIA).
- **Prefer a ledger to deletion.** A delete keeps the entity's history: its change history records the delete, and reads at a point in time before it still return the entity. But a delete runs no workflow — no transition, criterion or processor — and removes the entity's scheduled tasks without an audit event. End a lifecycle in a final state instead.
- **Name the final states precisely.** The engine has no "final" flag: a state with no automated and no scheduled transition keeps the entity until a client names a transition. Use several such states to say how a lifecycle ended, for example `Completed`, `Cancelled`, `Aborted`, `Replaced`.

## SEE ALSO

- models
- workflows
- crud
- search
- scheduled-tasks
