---
topic: errors.OIDC_TOKEN_PRE_TRANSITION
title: "OIDC_TOKEN_PRE_TRANSITION — token predates provider registration"
stability: stable
see_also:
  - errors
  - errors.UNAUTHORIZED
  - errors.OIDC_CLAIMS_INVALID
  - errors.OIDC_PROVIDER_NOT_FOUND
---

# errors.OIDC_TOKEN_PRE_TRANSITION

## NAME

OIDC_TOKEN_PRE_TRANSITION — the token's `iat` (issued-at) claim predates the
OIDC provider's registration time by more than the 30-second clock-skew
allowance (D17).

## SYNOPSIS

HTTP: `401` `Unauthorized`. Retryable: `no`.

## CURRENT PRODUCTION SURFACE

The current bearer-auth middleware translates all OIDC token-validation failures
— including pre-transition tokens — to a generic `UNAUTHORIZED` response (HTTP
401, code `UNAUTHORIZED`). The specific `OIDC_TOKEN_PRE_TRANSITION` code is the
**intended** wire surface and will be emitted when the bearer-auth translation
is implemented.

## DESCRIPTION

When an OIDC provider URI ownership changes — for example, when an existing
provider is invalidated and a new provider is registered for the same issuer —
tokens that were issued before the new provider's `createdAt` timestamp must be
rejected. This defence prevents accidental spillover of tokens minted against a
prior ownership of the URI.

The allowed skew window is 30 seconds. Tokens whose `iat` falls within that
window of the provider's `createdAt` are accepted; tokens older than that are
rejected with this code.

## RESOLUTION

- **Client:** discard the current token and obtain a fresh one from the identity
  provider. The new token will have an `iat` that postdates the provider
  registration and will be accepted.
- **Admin:** if the rejection is unexpected, verify the provider's `createdAt`
  via `GET /oauth/oidc/providers/{id}` and compare it against the token's `iat`.

## SEE ALSO

- errors
- errors.UNAUTHORIZED
- errors.OIDC_CLAIMS_INVALID
- errors.OIDC_PROVIDER_NOT_FOUND
