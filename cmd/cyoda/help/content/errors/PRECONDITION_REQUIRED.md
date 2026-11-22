---
topic: errors.PRECONDITION_REQUIRED
title: "PRECONDITION_REQUIRED — PATCH request with no If-Match value"
stability: stable
see_also:
  - errors
  - errors.ENTITY_MODIFIED
  - errors.BAD_REQUEST
---

# errors.PRECONDITION_REQUIRED

## NAME

PRECONDITION_REQUIRED — a PATCH request was submitted without an `If-Match` header, or with an empty one; a value is mandatory for patch operations.

## SYNOPSIS

HTTP: `428` `Precondition Required`. Retryable: `no`.

## DESCRIPTION

PATCH applies a merge patch to the entity's stored state rather than replacing it wholesale. Because the patch is applied relative to the current stored data, cyoda-go requires the caller to state a precondition explicitly via `If-Match`. Omitting the header, or sending it empty, is rejected: an empty value names no version, and only `*` accepts last-writer-wins.

This behaviour is specific to PATCH. A PUT without `If-Match` is treated as an unconditional replace and is accepted.

Not retryable as-is — the same request without an `If-Match` value produces the same error.

## RECOVERY

Add an `If-Match` header to the PATCH request. Two modes are supported:

- **Conditional patch** — supply the `transactionId` obtained from your most recent GET of the entity as the `If-Match` value. The patch is applied only if the entity has not changed since that read; a `412 ENTITY_MODIFIED` is returned if another writer has committed in the meantime.
- **Unconditional patch (last-writer-wins)** — supply `If-Match: *` to explicitly accept that the patch will be applied to whatever the current stored state is, without a version check.

Choose the conditional form when you need to detect concurrent modifications; choose `*` when you intentionally want last-writer-wins semantics and have already decided the patch is safe to apply regardless of concurrent changes.

## SEE ALSO

- errors
- errors.ENTITY_MODIFIED
- errors.BAD_REQUEST
