---
topic: auth.clients
title: "auth.clients — M2M client lifecycle"
stability: evolving
version_added: 0.8.0
see_also:
  - auth
  - auth.tokens
  - config.auth
  - errors.M2M_CLIENT_NOT_FOUND
  - errors.FEATURE_DISABLED
---

# auth.clients

## NAME

auth.clients — provision and manage machine-to-machine (M2M) clients that authenticate against cyoda via the `client_credentials` grant.

## GOAL

You want a backend service or CI job to call cyoda APIs. Register an M2M client to obtain a `client_id` + `client_secret`. Your service then mints JWTs via `POST /api/oauth/token` (documented in `auth.tokens`) and presents them as `Authorization: Bearer …` on every request.

Use this path when you control both the service and its cyoda registration. For user-facing flows, federate via `auth.oidc` instead.

## PREREQUISITES

**Admin (cyoda operator) sets up:**

- `CYODA_IAM_MODE=jwt` (mock mode bypasses auth entirely — fine for dev, never for prod)
- `CYODA_JWT_SIGNING_KEY` (PEM RSA key; `_FILE` suffix supported)
- Optionally: `CYODA_BOOTSTRAP_CLIENT_ID` + `CYODA_BOOTSTRAP_CLIENT_SECRET` provisions a single admin M2M at startup, useful for CI. See `config.auth`.
- For admin-scoped M2M creation (`withAdminRole=true`): `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=true`. Off by default; when off, the `withAdminRole=true` request shape returns `404 FEATURE_DISABLED`.

**Client (you) needs:**

- An `Authorization: Bearer …` token with `ROLE_ADMIN`. Every `/clients` endpoint (list, create, delete) requires admin role today.
- A target tenant ID — the client is scoped to the caller's tenant.

## REQUEST FLOW

### Provision a client

```bash
curl -X POST https://cyoda.example.com/api/clients \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
        "name": "billing-svc",
        "description": "billing service backend"
      }'
```

Response (`200 OK`):

```json
{
  "clientId": "c1a3b9e0-…",
  "clientSecret": "1f4e…",
  "name": "billing-svc",
  "tenantId": "00000000-…",
  "roles": ["ROLE_M2M"]
}
```

**`clientSecret` is shown only at creation time.** Capture it now; the server cannot return it again.

### List clients in the caller's tenant

```bash
curl -X GET https://cyoda.example.com/api/clients \
  -H "Authorization: Bearer ${TOKEN}"
```

Returns `[]` of clients (no secrets — clients carry `clientId`, `name`, `tenantId`, `roles`).

### Delete a client

```bash
curl -X DELETE https://cyoda.example.com/api/clients/${CLIENT_ID} \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

Response: `204 No Content`. The deleted client's tokens remain valid until their natural `exp`; deletion stops new token issuance.

### Reset a client secret

Rotates `clientSecret` for an existing client. The new secret is shown only in the response — capture it before the connection closes.

```bash
curl -X POST https://cyoda.example.com/api/clients/${CLIENT_ID}/secret \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

Response (`200 OK`) carries the rotated `clientSecret`. Existing JWTs minted with the previous secret remain valid until their natural `exp`; only new `/oauth/token` requests will require the new secret.

## TOKEN

Clients are not tokens. After provisioning, the client uses `auth.tokens` (the `/oauth/token` endpoint) to mint JWTs. The JWT carries the client's `tenantId` in `caas_org_id` and its roles in `user_roles`. Full claim shape is in `auth.tokens`.

## ERRORS

- `errors.FORBIDDEN` (`403`) — caller lacks `ROLE_ADMIN` (required for list, create, and delete).
- `errors.M2M_CLIENT_NOT_FOUND` (`404`) — referenced `clientId` does not exist or belongs to a different tenant.
- `errors.FEATURE_DISABLED` (`404`) — `withAdminRole=true` requested with `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=false`.
- `errors.BAD_REQUEST` (`400`) — request body shape invalid.
- `errors.UNAUTHORIZED` (`401`) — bearer token missing, expired, or untrusted.

## SEE ALSO

- `auth.tokens` — the `/oauth/token` endpoint and JWT claim contract
- `config.auth` — `CYODA_BOOTSTRAP_*`, `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED`
- `openapi` — `cyoda help openapi tags` and look for the `User, Machine` tag
