---
topic: errors.OIDC_DISCOVERY_FAILED
title: "OIDC_DISCOVERY_FAILED — OIDC discovery document fetch failed"
stability: stable
see_also:
  - errors
  - errors.SERVER_ERROR
  - errors.OIDC_SSRF_BLOCKED
  - errors.OIDC_JWKS_UNAVAILABLE
  - config.auth
---

# errors.OIDC_DISCOVERY_FAILED

## NAME

OIDC_DISCOVERY_FAILED — the server failed to fetch or parse the
`.well-known/openid-configuration` document for a registered OIDC provider.

## SYNOPSIS

HTTP: `500` `Internal Server Error` (with ticket UUID). Retryable: `no`.

## CURRENT PRODUCTION SURFACE

The current bearer-auth middleware translates this failure to a generic
`SERVER_ERROR` response (HTTP 500, code `SERVER_ERROR`, with a ticket UUID). The
specific `OIDC_DISCOVERY_FAILED` code is the **intended** wire surface and will
be emitted when the bearer-auth translation is implemented.

## DESCRIPTION

When a registered OIDC provider's `wellKnownConfigUri` cannot be fetched (network
error, non-2xx HTTP response, or malformed JSON), the server cannot initialise
the provider's JWKS endpoint and rejects all authentication attempts for that
provider with this error.

This is a server-side configuration or connectivity issue — the client's token
may be valid but cannot be verified.

Distinct errors:

- `OIDC_SSRF_BLOCKED` — the URI was blocked by SSRF policy before any network
  request was made (happens at provider registration or reactivation time, not
  at token-validation time).
- `OIDC_JWKS_UNAVAILABLE` — the discovery document was fetched successfully but
  the subsequent JWKS-endpoint request failed.

The response body includes a `ticket` UUID for correlation with server logs.

## RESOLUTION

- **Admin:** verify that the provider's `wellKnownConfigUri` is reachable from
  the cyoda server. Check for DNS failures, firewall rules, or certificate
  errors. If HTTPS is required, confirm `CYODA_OIDC_REQUIRE_HTTPS` matches your
  environment.
- Confirm the upstream identity provider is healthy and serving a valid
  `.well-known/openid-configuration` document.
- After resolving the connectivity issue, the next authentication attempt will
  retry discovery automatically.

## SEE ALSO

- errors
- errors.SERVER_ERROR
- errors.OIDC_SSRF_BLOCKED
- errors.OIDC_JWKS_UNAVAILABLE
- config.auth
