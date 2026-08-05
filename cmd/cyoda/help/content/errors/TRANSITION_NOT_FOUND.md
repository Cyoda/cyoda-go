---
topic: errors.TRANSITION_NOT_FOUND
title: "TRANSITION_NOT_FOUND — workflow transition does not exist"
stability: stable
see_also:
  - errors
  - errors.WORKFLOW_NOT_FOUND
  - errors.WORKFLOW_FAILED
---

# errors.TRANSITION_NOT_FOUND

## NAME

TRANSITION_NOT_FOUND — the requested workflow transition is not defined for the entity's current state.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Entity workflow state machines define explicit transitions between states. This error fires when a transition is triggered that does not exist in the model's workflow definition for the entity's current state. Also occurs when the transition name is misspelled, when the transition is disabled, when it is a scheduled transition (those fire on their timer and are never manually fireable), or when the entity is in a terminal state that allows no further transitions.

The workflow consulted is the one the entity's selection criterion binds it to, not merely one that declares the state — see `cyoda help workflows`, *Workflow-level selection*. On a model with several workflows, a transition can therefore be valid for one entity and absent for another in the same state.

Not retryable. The workflow definition and the entity's current state determine which transition names are valid.

## SEE ALSO

- errors
- errors.WORKFLOW_NOT_FOUND
- errors.WORKFLOW_FAILED
