---
topic: errors.OIDC_AUDIENCE_MISMATCH
title: "OIDC_AUDIENCE_MISMATCH — JWT aud claim does not match provider configuration"
stability: stable
see_also:
  - errors
  - errors.UNAUTHORIZED
  - errors.OIDC_CLAIMS_INVALID
  - errors.OIDC_PROVIDER_NOT_FOUND
---

# errors.OIDC_AUDIENCE_MISMATCH

## NAME

OIDC_AUDIENCE_MISMATCH — the JWT `aud` claim does not match any value in the
provider's configured `expectedAudiences` list (D20).

## SYNOPSIS

HTTP: `401` `Unauthorized`. Retryable: `no`.

## CURRENT PRODUCTION SURFACE

The current bearer-auth middleware translates all OIDC token-validation failures
— including audience mismatches — to a generic `UNAUTHORIZED` response (HTTP
401, code `UNAUTHORIZED`). The specific `OIDC_AUDIENCE_MISMATCH` code is the
**intended** wire surface and will be emitted when the bearer-auth translation
is implemented.

## DESCRIPTION

When a provider is registered with a non-empty `expectedAudiences` list, every
incoming JWT must carry an `aud` claim whose value (or one element of the array)
matches at least one entry in that list. If no match is found, authentication
fails with this code.

An empty `expectedAudiences` list disables audience checking entirely — any
`aud` value (or a token without `aud`) is accepted.

## RESOLUTION

- **Client:** obtain a token issued with an `aud` claim that matches the
  provider's `expectedAudiences`. The audience is typically configured when
  creating the OAuth 2.0 client application in the upstream identity provider.
- **Admin:** verify the provider's `expectedAudiences` via
  `GET /oauth/oidc/providers/{id}`. Update via
  `PATCH /oauth/oidc/providers/{id}` if the configured audiences are stale.

## SEE ALSO

- errors
- errors.UNAUTHORIZED
- errors.OIDC_CLAIMS_INVALID
- errors.OIDC_PROVIDER_NOT_FOUND
