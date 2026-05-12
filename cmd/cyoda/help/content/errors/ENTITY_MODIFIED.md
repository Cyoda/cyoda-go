---
topic: errors.ENTITY_MODIFIED
title: "ENTITY_MODIFIED — If-Match precondition failed; entity changed since last read"
stability: stable
see_also:
  - errors
  - errors.CONFLICT
  - errors.IDEMPOTENCY_CONFLICT
  - errors.EPOCH_MISMATCH
---

# errors.ENTITY_MODIFIED

## NAME

ENTITY_MODIFIED — an `If-Match`-guarded entity update was rejected because the supplied transaction-ID no longer matches the entity's current version.

## SYNOPSIS

HTTP: `412` `Precondition Failed`. Retryable: `no`.

## DESCRIPTION

When an entity update request carries an `If-Match` header, the server requires the supplied transaction ID to equal the entity's current `meta.transactionId`. A mismatch means another writer has updated the entity since the caller's last read. The optimistic-concurrency guard rejects the update rather than silently overwrite.

The `entityId` property in the problem-detail body identifies the conflicting entity.

Not retryable in the protocol sense — replaying the same payload with the same `If-Match` value will fail again.

## RECOVERY

1. **Re-read the entity:** `GET /api/entity/{entityId}`. The response envelope's `meta.transactionId` is the entity's current version.
2. **Reconcile your change against the current state.** Whatever the concurrent writer changed is now the baseline; merge or override it intentionally rather than blindly replaying your previous payload.
3. **Re-submit the update with the fresh `If-Match`:**

   ```
   curl -X PUT \
     -H "Authorization: Bearer $TOKEN" \
     -H "Content-Type: application/json" \
     -H "If-Match: <meta.transactionId from step 1>" \
     -d '<reconciled payload>' \
     "http://localhost:8080/api/entity/JSON/{entityId}[/{transition}]"
   ```

A second `412 ENTITY_MODIFIED` on retry means another writer raced you again. Either accept the loss (drop your change), back off and retry the read-reconcile-write loop with jitter, or escalate to a coarser locking strategy (lock the model, or coordinate writers out of band) — naive looping will livelock under contention.

## RELATIONSHIP TO TRANSACTION-LEVEL CONFLICT DETECTION

`If-Match` is **not** the only conflict guard on entity updates; it is an additional, narrower one. Every PUT runs under Snapshot Isolation with first-committer-wins (SI+FCW) validation at commit time. The handler reads the entity inside the transaction, the read is recorded in the read-set, and any concurrent committer who changes the same entity between this PUT's transaction start and its commit is detected — the commit fails with `409 CONFLICT` and `retryable: true` (the `RetryableConflict` path). On the postgres backend, PostgreSQL's own SQLSTATE 40001 detection (under `REPEATABLE READ`) covers write-write races equivalently. So a PUT *cannot* silently overwrite a concurrent writer that committed during its own transaction window, regardless of whether `If-Match` was supplied.

What `If-Match` adds is a precondition tied to **the caller's earlier read in a different HTTP request**, before this PUT's transaction even began. Two concrete scenarios:

**Cross-request race (caller GET-then-PUT, another writer commits in between).** Caller GETs at `t0` and observes `transactionId` `T0`. Another writer commits a change at `t1`. Caller submits the PUT at `t2`, with `t0 < t1 < t2`. With `If-Match: T0`, the PUT fails `412 ENTITY_MODIFIED` because the entity's current `transactionId` is no longer `T0`. Without `If-Match`, the PUT's own intra-transaction GET at `t2` already sees the writer's change as the current baseline; the PUT proceeds and applies the caller's payload on top of the writer's change — the caller never sees that they overwrote a state they hadn't read.

**Overlapping-transaction race (two PUTs starting from the same baseline).** Two PUTs both begin a transaction and read the same entity version. With `If-Match`, the loser fails fast at write-time as `412 ENTITY_MODIFIED` via `CompareAndSave`. Without `If-Match`, the loser fails at commit-time as `409 CONFLICT` with `retryable: true` via SI+FCW read-set validation.

So omitting `If-Match` does not turn off all concurrency control — but it does silence the cross-request precondition. Use it when reconciling against the live state at PUT-time is what you actually want; supply it when you specifically need to detect that the entity has changed since *your* last read and refuse to clobber.

## SEE ALSO

- errors
- errors.CONFLICT
- errors.IDEMPOTENCY_CONFLICT
- errors.EPOCH_MISMATCH
