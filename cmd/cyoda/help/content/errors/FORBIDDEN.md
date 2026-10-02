---
topic: errors.FORBIDDEN
title: "FORBIDDEN — caller lacks required role or permission"
stability: stable
see_also:
  - errors
  - errors.UNAUTHORIZED
  - auth
---

# errors.FORBIDDEN

## NAME

FORBIDDEN — the authenticated caller does not have the role or permission required to perform the operation.

## SYNOPSIS

HTTP: `403` `Forbidden`. Retryable: `no`.

## DESCRIPTION

The request was authenticated successfully but the caller does not have the role or tenant required by the endpoint. Joining a transaction owned by another tenant also produces this error.

One cause: the token lacks `ROLE_M2M`, which every operation requires except `GET /account` and the client, trusted-key, key-pair and `/admin/*` operations. The detail is "this operation requires ROLE_M2M"; over gRPC the call fails with `PermissionDenied` and the same text. See `auth`.

Another cause: the endpoint needs a platform operator: `ROLE_ADMIN` in the tenant `PLATFORM` (signing key pairs, `/admin/*`). An admin of any other tenant gets this error with the detail "platform operator required".

Not retryable with the same token. Access depends on the token's role claims and, for some endpoints, its tenant.

## SEE ALSO

- errors
- errors.UNAUTHORIZED
