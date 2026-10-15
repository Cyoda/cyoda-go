# Criterion stoppage reason — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's criterion stoppage reason feature. cyoda-go is the authoritative
implementation; the behaviour described here is derived directly from its
design spec and implemented code.

## 1. Wire shape

`EntityCriteriaCalculationResponse` (the compute-node → server criteria
response, unchanged wire shape) may carry an optional `reason` string
alongside `matches: false`:

```jsonc
{
  "id": "<a new event id>",
  "requestId": "<requestId>",
  "entityId": "<entityUUID>",
  "success": true,
  "matches": false,
  "reason": "credit score 540 below threshold 600"
}
```

`matches` itself is required on a successful response; an answer that omits it
is unreadable rather than `false` (`docs/cloud-parity/callout-failover.md` §1).

A compute node's own `reason` is kept to its first **2048 characters**, with
`…` appended when it was cut. Unlike every other piece of a compute node's
free text (see `docs/cloud-parity/callout-failover.md`), the reason keeps a
wider allowance of its own: it is the criterion's business explanation for
refusing a transition, not a diagnostic. Whatever reaches the audit trail or an
error message is capped well above that on top of it — generously enough that
this second cap is never what bites — which is what also bounds a reason the
platform itself composes.
The reason recorded for a criterion that evaluates false is:

- for a criterion with no function condition: the inline default
  `"criterion did not match"`;
- for a function condition that is the whole criterion and was called: the
  compute node's `reason`, else the inline default;
- for any other criterion that holds a function condition: one clause per
  function condition the evaluation reached, in evaluation order, joined with
  `; ` — `<name> at <path>: pre-check did not match, not called`, or
  `<name> at <path>: answered <true|false>` followed by `: <reason>` when the
  compute node gave one. ` at <path>` is left out for a function condition that
  is the whole criterion. With no clause, the inline default applies.
  `function-conditions-in-criteria.md` §5 defines this.

## 2. Delivery surfaces

Two independent surfaces carry the reason — they are not interchangeable:

1. **Manual-transition 400 response (primary, guaranteed).** When an explicit
   `PUT .../entity/{format}/{entityId}/{transition}` is rejected by its
   criterion, the `400 WORKFLOW_FAILED` `ProblemDetail.detail` is
   `transition "<name>" criterion not matched: <reason>`, with the reason of
   §1. The inline default is *not* appended — when the reason is
   `"criterion did not match"`, the detail is the bare
   `transition "<name>" criterion not matched`. This is the only delivery
   guaranteed for a manual rejection (see §3).

2. **State-machine audit `data.reason` (durable, automated/skip paths only).**
   The `TRANSITION_NOT_MATCH_CRITERION` event's `data` carries
   `{workflowName, transition, criterion, reason}`; the `WORKFLOW_SKIP`
   event's `data` carries `{workflowName, reason}`. `reason` is always present
   on both, as defined in §1. `criterion` is the function's name when a
   function condition is the whole criterion, and the condition's type
   (e.g. `"simple"`, `"group"`) otherwise. When the criterion holds a function
   condition, both events also carry `functions`, the list of function
   conditions the evaluation reached (`function-conditions-in-criteria.md` §5).

## 3. Transactional-audit invariant (no tx-semantics change)

A manual transition rejected by its criterion performs zero mutation and its
transaction is rolled back like any other no-op manual attempt — this feature
does **not** force that rollback to a commit. Audit events are bound to the
transaction on every backend (memory, sqlite, postgres), so the
`TRANSITION_NOT_MATCH_CRITERION` audit event recorded for a *manual*
rejection is rolled back with the rest of the transaction and is not
retrievable via `GET /audit/entity/{id}` afterward. This is by design: the
manual reason's guaranteed delivery is the 400 response (§2.1), not the
audit trail.

The audit `data.reason` **is** durable for the automated cascade and
workflow-selection (`WORKFLOW_SKIP`) paths, because those evaluations happen
inside a transaction that commits regardless of the no-match outcome — the
no-match simply stops the cascade rather than aborting the request.

## 4. Backend support

Delivery surface 1 (400 response) is backend-agnostic — it is produced by the
engine before any storage write. Delivery surface 2 (durable audit
`data.reason`) is exercised across memory, sqlite, and postgres by the
cross-backend parity suite (inline-default scenario, backend-agnostic by
construction since it requires no compute node); the commercial backend must
match the same `data` shape and the same transactional-audit invariant (§3).
