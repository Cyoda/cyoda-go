---
topic: errors.DELETE_NOT_CONVERGED
title: "DELETE_NOT_CONVERGED — a batched delete never ran out of matching entities"
stability: stable
see_also:
  - errors
  - errors.CONFLICT
---

# errors.DELETE_NOT_CONVERGED

## NAME

DELETE_NOT_CONVERGED — a batched delete kept finding new matching entities and was stopped before it finished.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `yes`.

## DESCRIPTION

`DELETE /entity/{entityName}/{modelVersion}` with `transactionSize` set (and no `pointInTime`) deletes in batches, re-selecting the matching entities before each batch. It finishes when a selection pass finds nothing left. If entities matching the condition are created at least as fast as they are removed, that pass never comes up empty, so the delete is capped at a fixed number of batches and fails with this code instead of running indefinitely.

Batches that committed before the failure are durable — the matching entities are partially deleted. The request fails rather than returning counts, because those counts would describe only the part of the work that fit inside the cap, not the delete that was asked for.

Do one of:

- stop the writers that keep creating matching entities, then retry;
- narrow the condition so the matching set is one the delete can drain;
- retry as-is if the concurrent writes were a transient burst.

An unbatched delete (`transactionSize` absent) and a `pointInTime`-pinned delete both resolve their target set once and cannot raise this.

Not to be confused with `CONFLICT`, which is a single entity losing an optimistic-concurrency race; the remedy there is to re-read that entity and replay the write.

## SEE ALSO

- errors
- errors.CONFLICT
