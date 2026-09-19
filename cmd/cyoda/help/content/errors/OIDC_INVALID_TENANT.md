---
topic: errors.OIDC_INVALID_TENANT
title: "OIDC_INVALID_TENANT — OIDC provider operations require a canonically-spelled UUID tenant identifier"
stability: stable
see_also:
  - errors
  - errors.OIDC_PROVIDER_DUPLICATE
  - config.auth
---

# errors.OIDC_INVALID_TENANT

## NAME

OIDC_INVALID_TENANT — OIDC provider operations require a canonically-spelled UUID tenant identifier.

## SYNOPSIS

HTTP: `400` `Bad Request` with code `OIDC_INVALID_TENANT` on every OIDC provider
operation — `POST`, `GET`, `PATCH`, `DELETE` under `/oauth/oidc/providers` — when the
calling tenant's ID is not a UUID in its canonical lowercase form
(`1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d`). (`POST /oauth/oidc/providers/reload` is
tenant-independent and is not affected.)

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

The same answer is given on read and lifecycle operations, not only on
registration. A non-UUID tenant owns no provider and can never come to own one,
so listing its providers is not an empty success — it is the same rejection
registration gives, delivered at the point the caller asks.

The canonical lowercase spelling is **required**, not normalised to. Storage
keys a provider by that form, and everywhere else in cyoda — entities, KV
namespaces, audit records, messages — a tenant is compared as raw text. So
`1A2B3C4D-…` and `1a2b3c4d-…` are two different tenants that own two different
sets of data; folding them together on this one surface would let either list,
modify and delete the other's providers, and register a provider owned by the
other, which is an authentication trust anchor for a tenant it is not. A tenant
spelled any other way — upper case, or the 32-character hyphenless form — is
answered `400 OIDC_INVALID_TENANT` here rather than silently addressing another
tenant's providers.

## RESOLUTION

Provision a real tenant with a UUID identifier before registering OIDC providers:

- For bootstrap deployments: set `CYODA_BOOTSTRAP_TENANT_ID` to a valid UUID
  (e.g. `CYODA_BOOTSTRAP_TENANT_ID=$(uuidgen)`) and restart the server.
- For non-default tenants in production: ensure the tenant was created with a
  UUID identifier and that your M2M credential carries that UUID as
  `caas_org_id` in its canonical lowercase form.

Then retry the operation.

## SEE ALSO

- errors
- errors.OIDC_PROVIDER_DUPLICATE
- config.auth
