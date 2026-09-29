---
topic: errors.M2M_CLIENT_NOT_FOUND
title: "M2M_CLIENT_NOT_FOUND — referenced technical user does not exist"
stability: stable
see_also:
  - errors
  - errors.UNAUTHORIZED
  - errors.FORBIDDEN
  - auth.clients
---

# errors.M2M_CLIENT_NOT_FOUND

## NAME

M2M_CLIENT_NOT_FOUND — an admin operation referenced a `clientId` that is not present in this tenant's M2M registry.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

Returned by `/clients` admin endpoints when the supplied `clientId` does not match any registered M2M client in the caller's tenant:

- `DELETE /clients/{clientId}` — the deletion target does not exist.
- `PUT /clients/{clientId}/secret` — the rotation target does not exist.

The detail field carries a generic `M2M client not found` message; internal store phrasing is never leaked into the response body.

Not retryable. Verify the `clientId` via `GET /clients` before retrying the operation.

A reset can answer `404` for a client that `GET /clients` still lists: a reset that raced a delete of the same client wrote the client back without its index entry. That client cannot get a token; `DELETE /clients/{clientId}` removes it. See `auth.clients` (STORAGE AND CONSISTENCY).

Returned uniformly for `clientId`s that do not exist AND `clientId`s owned by another tenant; the response does not distinguish — by design, to prevent cross-tenant existence enumeration.

## SEE ALSO

- errors
- errors.UNAUTHORIZED
- errors.FORBIDDEN
- auth.clients
