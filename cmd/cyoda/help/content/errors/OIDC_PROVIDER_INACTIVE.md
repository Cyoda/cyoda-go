---
topic: errors.OIDC_PROVIDER_INACTIVE
title: "OIDC_PROVIDER_INACTIVE — provider is invalidated"
stability: stable
see_also:
  - errors
  - errors.OIDC_PROVIDER_NOT_FOUND
---

# errors.OIDC_PROVIDER_INACTIVE

## NAME

OIDC_PROVIDER_INACTIVE — the OIDC provider is currently invalidated and cannot be updated.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`.

## DESCRIPTION

`PATCH /oauth/oidc/providers/{id}` requires the provider to be in the active
state. If the provider has been invalidated via
`POST /oauth/oidc/providers/{id}/invalidate`, this error is returned.

To update an invalidated provider, first reactivate it via
`POST /oauth/oidc/providers/{id}/reactivate`, then apply the update.

## SEE ALSO

- errors
- errors.OIDC_PROVIDER_NOT_FOUND
