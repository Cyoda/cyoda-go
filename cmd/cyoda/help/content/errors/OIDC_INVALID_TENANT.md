---
topic: errors.OIDC_INVALID_TENANT
title: "OIDC_INVALID_TENANT — OIDC provider registration requires UUID-shaped tenant identifier"
stability: stable
see_also:
  - errors
  - errors.OIDC_PROVIDER_DUPLICATE
  - config.auth
---

# errors.OIDC_INVALID_TENANT

## NAME

OIDC_INVALID_TENANT — OIDC provider registration requires a UUID-shaped tenant identifier.

## SYNOPSIS

HTTP: `400` `Bad Request` with code `OIDC_INVALID_TENANT` on `POST /oauth/oidc/providers`
when the calling tenant's ID is not a valid UUID.

## DESCRIPTION

cyoda treats legal entity identifiers as UUIDs. OIDC provider ownership is
recorded as a `uuid.UUID` `OwnerLegalEntityID` field, which keys both the
per-tenant KV blob storage and the validated user-context tenant binding at
token validation time.

Non-UUID tenant IDs (e.g. the dev-convenience `default-tenant` string accepted
by `CYODA_BOOTSTRAP_TENANT_ID`) cannot be used to register OIDC providers for
two reasons:

1. **KV collision** — every non-UUID tenant would map to the same
   `00000000-0000-0000-0000-000000000000` storage key, allowing cross-tenant
   data leakage between all bootstrap deployments.
2. **Synthetic identity** — OIDC-validated tokens issued against such a provider
   would carry a fabricated "nil tenant" downstream, breaking tenant-scoped
   access control.

Production deployments use UUID-shaped legal entity identifiers and are not
affected by this restriction.

## RESOLUTION

Provision a real tenant with a UUID identifier before registering OIDC providers:

- For bootstrap deployments: set `CYODA_BOOTSTRAP_TENANT_ID` to a valid UUID
  (e.g. `CYODA_BOOTSTRAP_TENANT_ID=$(uuidgen)`) and restart the server.
- For non-default tenants in production: ensure the tenant was created with a
  UUID identifier and that your M2M credential carries that UUID as `caas_org_id`.

Then retry the `POST /oauth/oidc/providers` registration.

## SEE ALSO

- errors
- errors.OIDC_PROVIDER_DUPLICATE
- config.auth
