---
topic: auth.tokens
title: "auth.tokens — /oauth/token grants and JWT claim contract"
stability: evolving
version_added: 0.8.0
see_also:
  - auth
  - auth.clients
  - auth.trusted-keys
  - cli.token
  - config.auth
  - errors.UNAUTHORIZED
  - errors.FORBIDDEN
---

# auth.tokens

## NAME

auth.tokens — exchange credentials for a JWT at `POST /api/oauth/token`. Covers every supported grant and the canonical JWT claim contract that all cyoda tokens (M2M, OBO, federated OIDC) conform to.

## GOAL

You have a way to prove identity (an M2M `client_id`/`secret`, a JWT minted by a federated IdP, or — for token exchange — a subject token signed with a trusted key) and you want a cyoda-issued (or cyoda-validated) JWT to present on subsequent API calls.

This is the single home for the JWT claim contract. `auth.oidc` and `auth.trusted-keys` link here for claim shape.

## PREREQUISITES

**Admin (cyoda operator) sets up:**

- `CYODA_IAM_MODE=jwt`
- `CYODA_JWT_SIGNING_KEY` (PEM RSA private key; tokens cyoda issues are signed with this)
- `CYODA_JWT_ISSUER` (default `cyoda`; populates the `iss` claim; must not be empty)
- `CYODA_JWT_AUDIENCE` (default empty = no `aud` check on inbound tokens, and no `aud` on issued tokens)
- `CYODA_JWT_EXPIRY_SECONDS` (default `3600`)

See `config.auth` for the full env-var reference.

**Client (you) needs:**

- For `client_credentials`: a registered M2M `client_id`/`secret` (see `auth.clients`).
- For token-exchange (OBO): an already-valid subject JWT plus an M2M `client_id`/`secret` to act as the actor.

## REQUEST FLOW

### client_credentials — most common

Mint an M2M JWT with your client credentials:

```bash
# -K- reads curl options from stdin: a command line is visible to other
# local users, stdin is not.
curl -X POST https://cyoda.example.com/api/oauth/token \
  -K- <<<"user = \"${CLIENT_ID}:${CLIENT_SECRET}\"" \
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
# The client secret and the subject token go on stdin (-K-), not the
# command line.
curl -X POST https://cyoda.example.com/api/oauth/token \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=urn:ietf:params:oauth:grant-type:token-exchange" \
  -d "subject_token_type=urn:ietf:params:oauth:token-type:jwt" \
  -K- <<EOF
user = "${CLIENT_ID}:${CLIENT_SECRET}"
data = "subject_token=${USER_TOKEN}"
EOF
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

- The subject token must be signed by a trusted key registered in the M2M client's own tenant (`auth.trusted-keys`). A key registered by another tenant is not found → `400 invalid_grant`.
- The subject token's `caas_org_id` must match the M2M client's tenant. Tenant mismatch → `403 access_denied`.
- The subject token's `sub` becomes the issued token's user identifier, so it must pass the user-identifier rule in `config.auth` (1 to 255 characters; no control character, noncharacter or U+FFFD; not beginning with the reserved word `oidc:`). Otherwise → `400 invalid_grant`.
- The issued OBO token carries `sub` = the subject's `sub`, `user_roles` from the subject token, and an `act` claim `{"sub": "<m2m client_id>"}` identifying the actor.
- Subject token must already be valid (signature, not expired).

## TOKEN

**Cyoda-minted tokens** (issued via `client_credentials` or token-exchange/OBO at `/oauth/token`, or signed offline by `cyoda token`) carry the following claim shape. **Federated OIDC tokens** (`auth.oidc`) are *not* re-minted; they carry the upstream IdP's claim shape, and tenant + user identity are bound server-side from the registered provider's `OwnerLegalEntityID` — claims like `caas_org_id`, `caas_user_id`, `tid` on a federated token are explicitly ignored to prevent attacker-controlled tenant routing.

Claim shape for cyoda-minted tokens:

- `sub` (string) — Principal. `client_id` for M2M, user ID for OBO, `cyoda token` and federated tokens.
- `iss` (string) — Issuer. Cyoda-minted tokens use `CYODA_JWT_ISSUER`. Federated tokens use the upstream IdP's issuer.
- `aud` (string or string array) — Audience. Cyoda-minted tokens carry `CYODA_JWT_AUDIENCE` when it is set, and no `aud` otherwise. Checked against `CYODA_JWT_AUDIENCE` if set; against `expectedAudiences` for federated providers.
- `exp` (int unix) — Expiry.
- `iat` (int unix) — Issued-at.
- `jti` (string UUID) — Unique token ID.
- `caas_org_id` (string) — Tenant scope: a tenant id matching the tenant grammar in `config.auth` (`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`), or the token is rejected with `401`. Every API call is constrained to this tenant.
- `caas_user_id` (string) — User identifier. For M2M tokens this duplicates `sub` (= `client_id`). When it is absent, `sub` is the user identifier instead; when it is present, it must be a non-empty string and `sub` is not consulted. Either way the value must pass the user-identifier rule (1 to 255 characters; no control character, noncharacter or U+FFFD; not beginning with the reserved word `oidc:`), or the token is rejected with `401`; see `config.auth`.
- `scopes` (string array) — **`client_credentials` only.** The M2M client's roles (e.g. `ROLE_M2M`, `ROLE_ADMIN`); a token that carries `scopes` and no `user_roles` is a service principal.
- `user_roles` (string array) — Roles of a person token: OBO and `cyoda token`. Its presence marks a user principal. Federated OIDC tokens carry roles from the provider's configured `rolesClaim` (default `roles`; per-provider override available — see `auth.oidc`).
- `caas_tier` (string) — Tier label, on tokens from `/oauth/token`. cyoda-go: always `"unlimited"`; Cloud distinguishes paid tiers. `cyoda token` tokens carry none.
- `act` (object) — **OBO only.** `{"sub": "<m2m client_id>"}` identifying the M2M actor that exchanged the user token. Absent on `client_credentials` tokens.

Cyoda issues tokens signed by the selected signing key (RS256): the bootstrap key from `CYODA_JWT_SIGNING_KEY`, or an issued key pair if one is active and wins selection. The `kid` header points at that signing key pair, shared by every node of the cluster (`/oauth/keys/*`). Federated OIDC tokens are validated against the registered provider's JWKS — never signed by cyoda. A trusted key only verifies the subject token of a token exchange; it is never checked on an API call.

## ERRORS

- `errors.UNAUTHORIZED` (`401`) — `Authorization` header missing, token expired, signature invalid, issuer untrusted, or `kid` not a usable key of this node or a registered OIDC provider's JWKS. A `kid` that names a key pair this node holds is never resolved through a provider.
- `errors.FORBIDDEN` (`403`) — token valid but caller lacks the required role for the operation.
- `errors.BAD_REQUEST` (`400`) — malformed `grant_type`, missing form fields, invalid `subject_token` shape.
- The `/oauth/token` endpoint returns OAuth-shaped errors (`{"error": "...", "error_description": "..."}`) per RFC 6749 rather than the generic cyoda error envelope — `invalid_client`, `invalid_grant`, `access_denied`, `server_error`.
- `401 invalid_client` — no Basic credentials, a client id that does not match `^[A-Za-z0-9]{1,100}$` (refused before the client store is read), an unknown client id, or a wrong secret. Each of the last three makes the same store reads and one bcrypt comparison, so the server's own work does not depend on whether the client id exists. A storage backend can take longer to read a present key than a missing one (on Cassandra a hit is two queries and a miss one), which can let a caller who already holds a client id confirm that it exists. Client ids are not secret — a token's `sub` carries one — and generated ids are 80-bit random, so this does not allow enumeration.
- `500 server_error` — the client store failed, or holds a damaged record or index entry for the client id (see `auth.clients`); also a signing failure. `error_description` carries a `ticket` for log correlation and no internal detail. A store failure is never answered `401`.
- `GET /.well-known/jwks.json` answers `503` with `Retry-After` while the node's key copy is stale.

## SEE ALSO

- `auth.clients` — provision the M2M client used by `client_credentials` and OBO
- `auth.oidc` — federate an external IdP whose JWTs cyoda will accept directly
- `auth.trusted-keys` — register a public key whose JWTs can be exchanged for cyoda tokens
- `cli.token` — sign an admin token offline with the signing key
- `config.auth` — `CYODA_JWT_*`
- `openapi` — `cyoda help openapi tags` and look for the `IAM` tag
