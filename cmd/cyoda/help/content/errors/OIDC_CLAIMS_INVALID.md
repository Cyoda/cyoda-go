---
topic: errors.OIDC_CLAIMS_INVALID
title: "OIDC_CLAIMS_INVALID — token claims validation failed"
stability: stable
see_also:
  - errors
  - errors.UNAUTHORIZED
  - errors.OIDC_AUDIENCE_MISMATCH
  - errors.OIDC_TOKEN_PRE_TRANSITION
---

# errors.OIDC_CLAIMS_INVALID

## NAME

OIDC_CLAIMS_INVALID — one or more JWT claims failed validation after signature
verification.

## SYNOPSIS

HTTP: `401` `Unauthorized`. Retryable: `no`.

## CURRENT PRODUCTION SURFACE

The current bearer-auth middleware translates all OIDC token-validation failures
— including claims validation failures — to a generic `UNAUTHORIZED` response
(HTTP 401, code `UNAUTHORIZED`). The specific `OIDC_CLAIMS_INVALID` code is the
**intended** wire surface and will be emitted when the bearer-auth translation
is implemented.

## DESCRIPTION

After signature verification, cyoda validates the token's standard and
provider-specific claims. This error covers failures in that validation step.
Common subcodes (carried in the `subcode` field of the response body):

- `missing_sub` — the token has no `sub` claim. A subject identifier is
  required to establish user identity.
- `invalid_sub` — the `sub` claim is present but fails structural validation:
  exceeds maximum length or contains control characters.
- `unsupported_alg` — the token header's `alg` field specifies an algorithm
  not accepted by this provider (e.g. `none`, symmetric algorithms).
- `expired` — the token's `exp` claim is in the past (beyond clock-skew
  tolerance).
- `not_yet_valid` — the token's `nbf` claim is in the future.

`OIDC_AUDIENCE_MISMATCH` and `OIDC_TOKEN_PRE_TRANSITION` are distinct codes
covering the audience and pre-transition checks respectively, even though they
are technically claims-layer validations.

## RESOLUTION

- **Client:** obtain a fresh token from the identity provider. Ensure the token
  includes a valid `sub` claim, uses a supported signing algorithm (RS256,
  RS384, RS512, ES256, ES384, ES512), and has not expired.
- **Admin:** if `unsupported_alg` is reported, verify the identity provider is
  configured to sign tokens with an asymmetric algorithm supported by cyoda.

## SEE ALSO

- errors
- errors.UNAUTHORIZED
- errors.OIDC_AUDIENCE_MISMATCH
- errors.OIDC_TOKEN_PRE_TRANSITION
