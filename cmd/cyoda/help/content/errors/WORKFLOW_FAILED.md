---
topic: errors.WORKFLOW_FAILED
title: "WORKFLOW_FAILED — workflow processor returned an error"
stability: stable
see_also:
  - errors
  - errors.WORKFLOW_NOT_FOUND
  - errors.TRANSITION_NOT_FOUND
  - errors.NO_COMPUTE_MEMBER_FOR_TAG
  - errors.CALLOUT_FAILED
---

# errors.WORKFLOW_FAILED

## NAME

WORKFLOW_FAILED — a workflow processor or guard condition returned a failure during entity state transition, or returned data the model rejects.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: only when the compute member that failed said so.

## DESCRIPTION

During an entity create or transition operation the associated workflow processors (pre-processors, post-processors) or guard conditions ran but one of them signalled failure. The failure message from the processor is included in the error detail.

When the failure is a compute member's own answer — a processor, a function criterion or a schedule function that answered `success: false` — the detail is the member's own message behind the step that failed (`processor <name> failed: <message>`, `failed to evaluate transition criterion: <message>`, `schedule function <name> failed: <message>`), and the response is marked `retryable: true` exactly when the member's error carried `retryable: true`. A member that answered is never replaced by another member: its answer is the callout's outcome, whatever `retryPolicy` says. This holds when the member is attached to another cluster node, too.

Three further causes are not processor failures at all, and reach the read endpoints `GET /entity/{entityId}/transitions` and `GET /platform-api/entity/fetch/transitions` as well as the write paths:

- The workflow selected for the entity does not declare its current state. Selection is by criterion and is re-evaluated on every call, so a data change can bind an entity to a definition that does not model the state it is parked in. The engine rejects rather than falling through to another definition that happens to declare it.
- A workflow selection criterion could not be evaluated. Where the cause is an unavailable compute member the error is `NO_COMPUTE_MEMBER_FOR_TAG` (`503`, retryable) instead.
- **A transition or workflow criterion cannot be evaluated.** Import validates a criterion's path grammar, operator names, and pattern operands, but not against the model — a model may legitimately be declared after the workflow that references it. If, when the criterion is evaluated, it names a field the model still does not declare, or names an operator nobody recognises, or carries an operand a declared type cannot satisfy, the save that triggered the evaluation is aborted and rolled back — no entity write, no state transition, no partial effect. This applies to every operator, including a `NOT` group's child leaf. See `docs/cloud-parity/unevaluable-criterion-fails-save.md`.

Without the member's `retryable: true` the failure is not retryable unless the underlying condition has changed: it originates from application logic in the processor, or from the workflow configuration; the data, the processor implementation, or the workflow configuration determines the outcome.

`retryable: true` speaks for Cyoda's state only. With a `SYNC` or `ASYNC_SAME_TX` processor, a criterion, or a schedule function, the operation failed and its transaction was rolled back, so running it again starts clean — unless an earlier `COMMIT_BEFORE_DISPATCH` processor of the same request had already committed. With a `COMMIT_BEFORE_DISPATCH` processor the part of the operation before the callout stays committed. (An `ASYNC_NEW_TX` processor's failure never reaches the client: the operation continues.) A compute member that was handed the work may have carried it out; what it did outside Cyoda is the application's to reconcile.

## SEE ALSO

- errors
- errors.WORKFLOW_NOT_FOUND
- errors.TRANSITION_NOT_FOUND
- errors.CALLOUT_FAILED
