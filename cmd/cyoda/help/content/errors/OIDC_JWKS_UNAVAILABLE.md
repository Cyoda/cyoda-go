---
topic: errors.OIDC_JWKS_UNAVAILABLE
title: "OIDC_JWKS_UNAVAILABLE — transient JWKS endpoint failure"
stability: stable
see_also:
  - errors
  - errors.UNAUTHORIZED
  - errors.OIDC_DISCOVERY_FAILED
  - config.auth
---

# errors.OIDC_JWKS_UNAVAILABLE

## NAME

OIDC_JWKS_UNAVAILABLE — the JWKS endpoint for a registered OIDC provider
returned an error or was unreachable during key resolution.

## SYNOPSIS

HTTP: `503` `Service Unavailable` with `Retry-After` header. Retryable: `yes`.

## CURRENT PRODUCTION SURFACE

The current bearer-auth middleware translates this failure to a generic
`UNAUTHORIZED` response (HTTP 401, code `UNAUTHORIZED`). The specific
`OIDC_JWKS_UNAVAILABLE` code and the `503` + `Retry-After` surface are the
**intended** wire surface and will be emitted when the bearer-auth translation
is implemented.

## DESCRIPTION

After a successful discovery-document fetch, cyoda contacts the provider's
`jwks_uri` to retrieve the public keys needed to verify token signatures. If
this request fails transiently (network timeout, upstream 5xx), all
authentication attempts for that provider return this error until the JWKS
endpoint recovers.

This is a transient infrastructure failure — the client's token may be valid.
Clients should retry the original request after the interval indicated in the
`Retry-After` response header.

Distinct from `OIDC_DISCOVERY_FAILED`, which covers failure to fetch the
`.well-known/openid-configuration` discovery document itself.

## RESOLUTION

- **Client:** retry the request after the `Retry-After` interval. The server
  will re-attempt JWKS resolution on the next request.
- **Admin:** verify the upstream identity provider's JWKS endpoint is healthy.
  Check for rate limiting, firewall rules, or certificate issues between cyoda
  and the identity provider.

## SEE ALSO

- errors
- errors.UNAUTHORIZED
- errors.OIDC_DISCOVERY_FAILED
- config.auth
