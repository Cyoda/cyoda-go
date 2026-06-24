---
topic: errors.OIDC_PROVIDER_DUPLICATE
title: "OIDC_PROVIDER_DUPLICATE — provider already registered"
stability: stable
see_also:
  - errors
  - errors.OIDC_PROVIDER_NOT_FOUND
---

# errors.OIDC_PROVIDER_DUPLICATE

## NAME

OIDC_PROVIDER_DUPLICATE — a provider with the same `wellKnownConfigUri` is already registered for this tenant.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Each tenant may register a given `wellKnownConfigUri` only once. Submitting
`POST /oauth/oidc/providers` with a URI that is already registered for the
caller's tenant returns this error.

To update the existing provider's configuration (issuers, expected audiences,
roles claim), use `PATCH /oauth/oidc/providers/{id}` instead.

## SEE ALSO

- errors
- errors.OIDC_PROVIDER_NOT_FOUND
