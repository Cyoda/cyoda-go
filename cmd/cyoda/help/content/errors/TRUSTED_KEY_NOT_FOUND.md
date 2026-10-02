---
topic: errors.TRUSTED_KEY_NOT_FOUND
title: "TRUSTED_KEY_NOT_FOUND — referenced trusted key does not exist"
stability: stable
see_also:
  - errors
  - errors.UNAUTHORIZED
  - errors.FORBIDDEN
---

# errors.TRUSTED_KEY_NOT_FOUND

## NAME

TRUSTED_KEY_NOT_FOUND — an admin operation referenced a trusted-key KID that the caller's tenant has not registered.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

Returned by trusted-key admin endpoints when the supplied KID does not match any key of the caller's tenant:

- `DELETE /oauth/keys/trusted/{keyId}` — the deletion target does not exist.
- `POST /oauth/keys/trusted/{keyId}/invalidate` — the lifecycle target does not exist.
- `POST /oauth/keys/trusted/{keyId}/reactivate` — the lifecycle target does not exist.

The detail field carries a generic `key not found` message; internal store phrasing (e.g. backend-specific KID echoes) is never leaked into the response body. Operators can correlate the request via the slog event emitted server-side at INFO level with `kid` and the underlying error.

Not retryable. Verify the KID via `GET /oauth/keys/trusted` before retrying the operation.

A kid is looked up in the caller's tenant only. Key ids are unique within a tenant, not across tenants: another tenant's key with the same kid is a different key, which this tenant can neither see nor change.

## SEE ALSO

- errors
- errors.UNAUTHORIZED
- errors.FORBIDDEN
