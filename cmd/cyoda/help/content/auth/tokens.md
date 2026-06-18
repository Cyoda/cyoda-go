---
topic: auth.tokens
title: "auth.tokens — /oauth/token grants and JWT claim contract"
stability: evolving
version_added: 0.8.0
see_also:
  - auth
  - auth.clients
  - auth.oidc
  - auth.trusted-keys
  - config.auth
  - errors.UNAUTHORIZED
  - errors.FORBIDDEN
---

# auth.tokens

## NAME

auth.tokens — exchange credentials for a JWT at `POST /api/oauth/token`. Covers every supported grant and the canonical JWT claim contract that all cyoda tokens (M2M, OBO, federated OIDC, trusted-key) conform to.

## GOAL

You have a way to prove identity (an M2M `client_id`/`secret`, or a JWT minted by a federated IdP, or a JWT you signed offline) and you want a cyoda-issued (or cyoda-validated) JWT to present on subsequent API calls.

This is the single home for the JWT claim contract. `auth.oidc` and `auth.trusted-keys` link here for claim shape.

## PREREQUISITES

**Admin (cyoda operator) sets up:**

- `CYODA_IAM_MODE=jwt`
- `CYODA_JWT_SIGNING_KEY` (PEM RSA private key; tokens cyoda issues are signed with this)
- `CYODA_JWT_ISSUER` (default `cyoda`; populates the `iss` claim)
- `CYODA_JWT_AUDIENCE` (default empty = no `aud` check on inbound tokens)
- `CYODA_JWT_EXPIRY_SECONDS` (default `3600`)
- `CYODA_JWT_BOOTSTRAP_AUDIENCE` (default `client`; controls which key signs M2M tokens)

See `config.auth` for the full env-var reference.

**Client (you) needs:**

- For `client_credentials`: a registered M2M `client_id`/`secret` (see `auth.clients`).
- For token-exchange (OBO): an already-valid subject JWT plus an M2M `client_id`/`secret` to act as the actor.

## REQUEST FLOW

### client_credentials — most common

Mint an M2M JWT with your client credentials:

```bash
curl -X POST https://cyoda.example.com/api/oauth/token \
  -u "${CLIENT_ID}:${CLIENT_SECRET}" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials"
```

Response (`200 OK`):

```json
{
  "access_token": "eyJhbGciOiJSUzI1NiIs…",
  "token_type":   "Bearer",
  "expires_in":   3600
}
```

Use the `access_token` as `Authorization: Bearer …` on every subsequent API call. Mint again when it nears `exp`; cyoda does not issue refresh tokens.

### token-exchange (OBO)

You are an M2M actor (e.g. a backend service) and you want to call cyoda **on behalf of a user** whose token you already hold. The OBO grant re-signs the subject token so cyoda sees the user as the principal and your service as the actor (RFC 8693).

```bash
curl -X POST https://cyoda.example.com/api/oauth/token \
  -u "${CLIENT_ID}:${CLIENT_SECRET}" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=urn:ietf:params:oauth:grant-type:token-exchange" \
  -d "subject_token=${USER_TOKEN}" \
  -d "subject_token_type=urn:ietf:params:oauth:token-type:jwt"
```

Response shape matches `client_credentials` plus a `issued_token_type` field:

```json
{
  "access_token":      "eyJhbGciOiJSUzI1NiIs…",
  "token_type":        "Bearer",
  "expires_in":        3600,
  "issued_token_type": "urn:ietf:params:oauth:token-type:jwt"
}
```

Key constraints:

- The subject token's `caas_org_id` must match the M2M client's tenant. Tenant mismatch → `403 access_denied`.
- The issued OBO token carries `sub` = the subject's `sub`, `user_roles` from the subject token, and an `act` claim `{"sub": "<m2m client_id>"}` identifying the actor.
- Subject token must already be valid (signature, not expired).

## TOKEN

Every JWT cyoda accepts — whether minted via `client_credentials`, OBO, federated OIDC (`auth.oidc`), or trusted-key offline signing (`auth.trusted-keys`) — conforms to this claim shape:

- `sub` (string) — Principal. `client_id` for M2M, user ID for OBO and federated tokens.
- `iss` (string) — Issuer. Cyoda-minted tokens use `CYODA_JWT_ISSUER`. Federated tokens use the upstream IdP's issuer.
- `aud` (string or string array) — Audience. Checked against `CYODA_JWT_AUDIENCE` if set; against `expectedAudiences` for federated providers.
- `exp` (int unix) — Expiry.
- `iat` (int unix) — Issued-at.
- `jti` (string UUID) — Unique token ID.
- `caas_org_id` (string UUID) — Tenant scope. Every API call is constrained to this tenant.
- `caas_user_id` (string) — User identifier. For M2M tokens this duplicates `sub` (= `client_id`).
- `user_roles` (string array) — Roles granted (e.g. `ROLE_ADMIN`, `ROLE_M2M`). Federated OIDC tokens carry roles from the provider's configured `rolesClaim` (default `roles`; per-provider override available — see `auth.oidc`).
- `caas_tier` (string) — Tier label. cyoda-go: always `"unlimited"`; Cloud distinguishes paid tiers.
- `act` (object) — **OBO only.** `{"sub": "<m2m client_id>"}` identifying the M2M actor that exchanged the user token. Absent on `client_credentials` tokens.

Cyoda issues tokens signed with `CYODA_JWT_SIGNING_KEY` (RS256). The `kid` header points at the active keypair in the keystore (`/oauth/keys/*`). Federated OIDC tokens are validated against the registered provider's JWKS — never signed by cyoda. Trusted-key tokens are signed by you (offline) with the matching private key for a registered public key.

## ERRORS

- `errors.UNAUTHORIZED` (`401`) — `Authorization` header missing, token expired, signature invalid, issuer untrusted, or `kid` not in any registered keystore / OIDC JWKS / trusted-key registry.
- `errors.FORBIDDEN` (`403`) — token valid but caller lacks the required role for the operation.
- `errors.BAD_REQUEST` (`400`) — malformed `grant_type`, missing form fields, invalid `subject_token` shape.
- The `/oauth/token` endpoint returns OAuth-shaped errors (`{"error": "...", "error_description": "..."}`) per RFC 6749 rather than the generic cyoda error envelope — `invalid_client`, `invalid_grant`, `access_denied`, `server_error`.

## SEE ALSO

- `auth.clients` — provision the M2M client used by `client_credentials` and OBO
- `auth.oidc` — federate an external IdP whose JWTs cyoda will accept directly
- `auth.trusted-keys` — register a public key so JWTs you sign offline are accepted
- `config.auth` — `CYODA_JWT_*`, `CYODA_BOOTSTRAP_*`
- `openapi` — `cyoda help openapi tags` and look for the `IAM` tag
