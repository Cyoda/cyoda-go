---
topic: errors.OIDC_PROVIDER_NOT_FOUND
title: "OIDC_PROVIDER_NOT_FOUND — OIDC provider does not exist"
stability: stable
see_also:
  - errors
  - errors.OIDC_PROVIDER_DUPLICATE
---

# errors.OIDC_PROVIDER_NOT_FOUND

## NAME

OIDC_PROVIDER_NOT_FOUND — the OIDC provider with the given ID does not exist for this tenant.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

The provider UUID supplied in the path parameter does not correspond to a
registered OIDC provider for the caller's tenant. Either the provider was
never registered, was deleted, or the UUID belongs to a different tenant
(cross-tenant existence is not disclosed).

## SEE ALSO

- errors
- errors.OIDC_PROVIDER_DUPLICATE
- errors.OIDC_PROVIDER_INACTIVE
