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
  "requestId": "<requestId>",
  "success": true,
  "matches": false,
  "reason": "credit score 540 below threshold 600"
}
```

`reason` is capped at **2 KiB**; longer values are truncated once, server-side.
When a criterion evaluates false without a compute-node-supplied reason
(inline predicate criteria, or a `FUNCTION` criterion that returns none), the
engine substitutes the inline default `"criterion did not match"`.

## 2. Delivery surfaces

Two independent surfaces carry the reason — they are not interchangeable:

1. **Manual-transition 400 response (primary, guaranteed).** When an explicit
   `PUT .../entity/{format}/{entityId}/{transition}` is rejected by its
   criterion, the `400 WORKFLOW_FAILED` `ProblemDetail.detail` is
   `transition "<name>" criterion not matched: <reason>`. The inline default
   is *not* appended — a criterion rejected with no external reason yields the
   bare `transition "<name>" criterion not matched`. This is the only
   delivery guaranteed for a manual rejection (see §3).

2. **State-machine audit `data.reason` (durable, automated/skip paths only).**
   The `TRANSITION_NOT_MATCH_CRITERION` event's `data` carries
   `{workflowName, transition, criterion, reason}`; the `WORKFLOW_SKIP`
   event's `data` carries `{workflowName, reason}`. `reason` is always present
   on both (external reason, else the inline default). `criterion` is the
   `FUNCTION` name, or the inline condition's type (e.g. `"simple"`) for a
   predicate criterion.

## 3. Transactional-audit invariant (no tx-semantics change)

A manual transition rejected by its criterion performs zero mutation and its
transaction is rolled back like any other no-op manual attempt — this feature
does **not** force that rollback to a commit. On a TX-bound audit store
(Postgres), the `TRANSITION_NOT_MATCH_CRITERION` audit event recorded for a
*manual* rejection is therefore rolled back with the rest of the transaction
and is not retrievable via `GET /audit/entity/{id}` afterward. This is by
design: the manual reason's guaranteed delivery is the 400 response (§2.1),
not the audit trail.

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
