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
  - errors.UNAUTHORIZED
  - errors.FORBIDDEN
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

- An `Authorization: Bearer …` token with `ROLE_ADMIN`. Every `/clients` endpoint (list, create, delete, reset-secret) requires admin role today.
- The created client is scoped to the caller's tenant — there is no per-request tenant parameter on these endpoints.

## REQUEST FLOW

The 4 `/clients` operations: provision, list, delete, reset-secret. Field names follow RFC 7591 (snake_case) for the credentials DTOs; list-item DTOs use cyoda's customary camelCase.

### Provision a client

The request takes no body. `withAdminRole` is a query parameter; the only role the new client receives unconditionally is `ROLE_M2M`, and `ROLE_ADMIN` is added when `withAdminRole=true` AND the IAM feature is enabled.

```bash
curl -X POST "https://cyoda.example.com/api/clients?withAdminRole=false" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

Response (`200 OK`) — schema `TechnicalUserCredentialsDto`:

```json
{
  "client_id":                "abc523BCD",
  "client_secret":            "mySecretKey123",
  "grant_type":               "client_credentials",
  "client_secret_expires_at": 0,
  "roles":                    ["ROLE_M2M"]
}
```

**`client_secret` is shown only at creation time.** Capture it now; the server cannot return it again. `client_secret_expires_at = 0` means the secret does not expire (per RFC 7591 §3.2.1).

### List clients in the caller's tenant

```bash
curl -X GET https://cyoda.example.com/api/clients \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

Response (`200 OK`) — array of `TechnicalUserDto` (no secrets):

```json
[
  {
    "clientId":       "abc523BCD",
    "creationDate":   "2026-06-17T10:02:27.88Z",
    "lastUpdateDate": "2026-06-17T10:02:27.88Z",
    "roles":          ["ROLE_M2M"]
  }
]
```

### Delete a client

```bash
curl -X DELETE https://cyoda.example.com/api/clients/${CLIENT_ID} \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

Response (`200 OK`):

```json
{
  "message":  "M2M client deleted successfully",
  "clientId": "abc523BCD"
}
```

The deleted client's tokens remain valid until their natural `exp`; deletion stops new token issuance.

### Reset a client secret

Rotates `client_secret` for an existing client. The verb is `PUT`, not `POST`.

```bash
curl -X PUT https://cyoda.example.com/api/clients/${CLIENT_ID}/secret \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

Response (`200 OK`) is `TechnicalUserCredentialsDto` — same shape as creation, carrying the new `client_secret`. Capture it before the connection closes. Existing JWTs minted with the previous secret remain valid until their natural `exp`; only new `/oauth/token` requests need the new secret.

## TOKEN

Clients are not tokens. After provisioning, the client uses `auth.tokens` (the `/oauth/token` endpoint) to mint JWTs from the `client_id` + `client_secret`. The JWT carries the client's tenant in `caas_org_id` and its roles in `user_roles`. Full claim shape is in `auth.tokens`.

## ERRORS

- `errors.UNAUTHORIZED` (`401`) — bearer token missing, expired, signature invalid, or issuer untrusted.
- `errors.FORBIDDEN` (`403`) — caller lacks `ROLE_ADMIN` (required for every `/clients` endpoint today).
- `errors.M2M_CLIENT_NOT_FOUND` (`404`) — referenced `clientId` does not exist or belongs to a different tenant.
- `errors.FEATURE_DISABLED` (`404`) — `withAdminRole=true` requested with `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=false`.
- `errors.BAD_REQUEST` (`400`) — query-string parameter invalid (e.g. malformed `withAdminRole` value).

## SEE ALSO

- `auth.tokens` — the `/oauth/token` endpoint and JWT claim contract
- `config.auth` — `CYODA_BOOTSTRAP_*`, `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED`
- `openapi` — `cyoda help openapi tags` and look for the `User, Machine` tag
