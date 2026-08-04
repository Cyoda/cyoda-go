---
topic: errors.WORKFLOW_FAILED
title: "WORKFLOW_FAILED — workflow processor returned an error"
stability: stable
see_also:
  - errors
  - errors.WORKFLOW_NOT_FOUND
  - errors.TRANSITION_NOT_FOUND
  - errors.NO_COMPUTE_MEMBER_FOR_TAG
---

# errors.WORKFLOW_FAILED

## NAME

WORKFLOW_FAILED — a workflow processor or guard condition returned a failure during entity state transition, or returned data the model rejects.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

During an entity create or transition operation the associated workflow processors (pre-processors, post-processors) or guard conditions ran but one of them signalled failure. The failure message from the processor is included in the error detail.

Two further causes are not processor failures at all, and reach the read endpoints `GET /entity/{entityId}/transitions` and `GET /platform-api/entity/fetch/transitions` as well as the write paths:

- The workflow selected for the entity does not declare its current state. Selection is by criterion and is re-evaluated on every call, so a data change can bind an entity to a definition that does not model the state it is parked in. The engine rejects rather than falling through to another definition that happens to declare it.
- A workflow selection criterion could not be evaluated. Where the cause is an unavailable compute member the error is `NO_COMPUTE_MEMBER_FOR_TAG` (`503`, retryable) instead.

Not retryable unless the underlying condition has changed. The failure originates from application logic in the processor, or from the workflow configuration; the data, the processor implementation, or the workflow configuration determines the outcome.

## SEE ALSO

- errors
- errors.WORKFLOW_NOT_FOUND
- errors.TRANSITION_NOT_FOUND
