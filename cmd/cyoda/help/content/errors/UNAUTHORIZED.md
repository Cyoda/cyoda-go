---
topic: errors.UNAUTHORIZED
title: "UNAUTHORIZED — authentication required or token invalid"
stability: stable
see_also:
  - errors
  - errors.FORBIDDEN
---

# errors.UNAUTHORIZED

## NAME

UNAUTHORIZED — the request does not include valid authentication credentials or the provided token failed verification.

## SYNOPSIS

HTTP: `401` `Unauthorized`. Retryable: `no`.

## DESCRIPTION

Returned when the `Authorization` header is missing, the bearer token is expired, the token is not signed by one of cyoda's own key pairs (an identity provider's token, a user assertion), its `iss` is not `CYODA_JWT_ISSUER`, its `aud` lacks `CYODA_JWT_AUDIENCE` when that is set, or its claims break the token contract (see `cyoda help auth tokens`). Also returned when a request reaches a protected route with no identity context established by the auth middleware.

Authentication runs before any handler and before any transaction is joined, on HTTP and gRPC: the request did nothing. Not retryable with the same token. Get a fresh token and send the request again, once; a second `401` with a fresh token is a configuration error (see `cyoda help auth integration`, *ERRORS AND RETRIES*).

## SEE ALSO

- errors
- errors.FORBIDDEN
