---
topic: errors.KEY_OWNED_BY_DIFFERENT_TENANT
title: "KEY_OWNED_BY_DIFFERENT_TENANT — trusted-key collision with another tenant"
stability: stable
see_also:
  - errors
  - errors.TRUSTED_KEY_NOT_FOUND
---

# errors.KEY_OWNED_BY_DIFFERENT_TENANT

## NAME

KEY_OWNED_BY_DIFFERENT_TENANT — the requested `keyId` is already registered by another tenant.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`.

## DESCRIPTION

Trusted keys are tenant-scoped. When `POST /oauth/keys/trusted` is called with a `keyId` that already belongs to a different tenant, the request is rejected with `409`. Pick a fresh `keyId` (the caller cannot see or affect the other tenant's keys).

## SEE ALSO

- errors
- errors.TRUSTED_KEY_NOT_FOUND
