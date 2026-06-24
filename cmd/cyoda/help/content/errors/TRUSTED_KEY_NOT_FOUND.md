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

TRUSTED_KEY_NOT_FOUND — an admin operation referenced a trusted-key KID that is not present in the registry.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

Returned by trusted-key admin endpoints when the supplied KID does not match any registered key:

- `DELETE /oauth/keys/trusted/{keyId}` — the deletion target does not exist.
- `POST /oauth/keys/trusted/{keyId}/invalidate` — the lifecycle target does not exist.
- `POST /oauth/keys/trusted/{keyId}/reactivate` — the lifecycle target does not exist.

The detail field carries a generic `key not found` message; internal store phrasing (e.g. backend-specific KID echoes) is never leaked into the response body. Operators can correlate the request via the slog event emitted server-side at INFO level with `kid` and the underlying error.

Not retryable. Verify the KID via `GET /oauth/keys/trusted` before retrying the operation.

Returned uniformly for kids that don't exist AND kids owned by another tenant; the response does not distinguish — by design, to prevent cross-tenant existence enumeration.

## SEE ALSO

- errors
- errors.UNAUTHORIZED
- errors.FORBIDDEN
