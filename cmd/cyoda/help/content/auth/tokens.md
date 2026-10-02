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

auth.tokens — exchange M2M client credentials for a JWT at `POST /api/oauth/token`. Covers both grants and the canonical JWT claim contract of every cyoda token.

## GOAL

You hold an M2M client (`client_id`/`secret`) and want a cyoda-issued JWT to present on subsequent API calls: a token of the client itself (`client_credentials`), or — for an on-behalf-of client — a token of a user of your application, acting through the client (token exchange).

This is the single home for the JWT claim contract. `auth.trusted-keys` links here for claim shape.

## PREREQUISITES

**Admin (cyoda operator) sets up:**

- `CYODA_IAM_MODE=jwt`
- `CYODA_JWT_SIGNING_KEY` (PEM RSA private key; tokens cyoda issues are signed with this)
- `CYODA_JWT_ISSUER` (default `cyoda`; populates the `iss` claim; must not be empty; a user assertion's `aud` must contain it)
- `CYODA_JWT_AUDIENCE` (default empty = no `aud` check on inbound tokens, and no `aud` on issued tokens)
- `CYODA_JWT_EXPIRY_SECONDS` (default `3600`)

See `config.auth` for the full env-var reference.

**Client (you) needs** an M2M client (see `auth.clients`). Each kind of client uses one grant:

- A plain client (`ROLE_M2M`) or an admin client (`ROLE_M2M`, `ROLE_ADMIN`) uses `client_credentials`.
- An on-behalf-of client (`ROLE_M2M`, created with `?onBehalfOf=true`) uses the token exchange.

For the token exchange, the tenant also needs a trusted key whose private half your application signs user assertions with (`auth.trusted-keys`).

## REQUEST FLOW

### client_credentials

Mint a token of the client itself:

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

`expires_in` is the token's remaining life in seconds (`exp` minus now). Use the `access_token` as `Authorization: Bearer …` on every subsequent API call. Mint again when it nears `exp`; cyoda does not issue refresh tokens.

An on-behalf-of client is refused this grant: `400 unauthorized_client`.

### token exchange (on behalf of a user, RFC 8693)

Your application has authenticated a user and calls cyoda for them. It signs a short user assertion with its trusted key and exchanges it, as its on-behalf-of client, for a token of that user acting through the client.

The assertion is a JWT:

- header: `alg` `RS256`, `kid` = the trusted key's `keyId`;
- `sub`: the user id (1 to 255 characters; no control character, noncharacter or U+FFFD; not `system` in any letter case — see `config.auth`);
- `caas_org_id`: the client's tenant;
- `aud`: contains `CYODA_JWT_ISSUER` (a string or an array);
- `iat` and `exp`: both required, with `exp − iat` at most 300 seconds;
- `nbf`: optional;
- `iss`: required when the trusted key lists `issuers`, and then one of them.

Every time claim is checked with 30 seconds of clock skew. Roles in the assertion are ignored.

```bash
# The client secret and the assertion go on stdin (-K-), not the command
# line.
curl -X POST https://cyoda.example.com/api/oauth/token \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=urn:ietf:params:oauth:grant-type:token-exchange" \
  -d "subject_token_type=urn:ietf:params:oauth:token-type:jwt" \
  -K- <<EOF
user = "${CLIENT_ID}:${CLIENT_SECRET}"
data = "subject_token=${USER_ASSERTION}"
EOF
```

Response shape matches `client_credentials` plus an `issued_token_type` field:

```json
{
  "access_token":      "eyJhbGciOiJSUzI1NiIs…",
  "token_type":        "Bearer",
  "expires_in":        120,
  "issued_token_type": "urn:ietf:params:oauth:token-type:jwt"
}
```

The token expires at the earlier of the assertion's `exp` and now + `CYODA_JWT_EXPIRY_SECONDS`; `expires_in` is its remaining life. Cache one token per user and exchange a new assertion when it nears `exp`.

The request carries none of the other RFC 8693 parameters: `actor_token`, `actor_token_type`, `resource`, `audience`, `scope` and `requested_token_type` are refused, even when empty. A client that is not an on-behalf-of client is refused the exchange before its assertion is read.

## TOKEN

Every cyoda token (issued at `/oauth/token`, or signed offline by `cyoda token`) carries the following claim shape:

- `sub` (string) — Principal. The client id on a `client_credentials` token; the user id on a token-exchange or `cyoda token` token.
- `iss` (string) — `CYODA_JWT_ISSUER`.
- `aud` (string) — `CYODA_JWT_AUDIENCE` when it is set, and no `aud` otherwise. Checked against `CYODA_JWT_AUDIENCE` if set.
- `exp` (int unix) — Expiry.
- `iat` (int unix) — Issued-at.
- `jti` (string UUID) — Unique token ID.
- `caas_org_id` (string) — Tenant scope: the client's tenant on tokens from `/oauth/token`. A tenant id matching the tenant grammar in `config.auth` (`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`), or the token is rejected with `401`. Every API call is constrained to this tenant.
- `caas_user_id` (string) — User identifier: the client id on a `client_credentials` token, the user id on a token-exchange token. When it is absent, `sub` is the user identifier instead; when it is present, it must be a non-empty string and `sub` is not consulted. Either way the value must pass the user-identifier rule, or the token is rejected with `401`; see `config.auth`.
- `scopes` (string array) — **Both grants.** The M2M client's roles (e.g. `ROLE_M2M`, `ROLE_ADMIN`), never the assertion's. A token that carries `scopes` and no `act` is the client itself, a service principal; with `act` it is a user acting through that client.
- `cgen` (integer) — **`client_credentials` only.** The client's secret generation: 1 at creation, one more after each secret reset.
- `act` (object) — **Token exchange only.** `{"sub": "<client_id>"}`, one level: the on-behalf-of client acting for the user. A token whose `act` is not an object, whose `act.sub` is not a client id, or that carries `act` without `scopes` is rejected with `401`.
- `user_roles` (string array) — Roles of a `cyoda token` person token. Its presence marks a user principal. A token that carries both `scopes` and `user_roles`, or `act` and `user_roles`, is rejected with `401`. Tokens from `/oauth/token` never carry it.
- `caas_tier` (string) — Tier label, on tokens from `/oauth/token`. cyoda-go: always `"unlimited"`; Cloud distinguishes paid tiers. `cyoda token` tokens carry none.

Cyoda issues tokens signed by the selected signing key (RS256): the bootstrap key from `CYODA_JWT_SIGNING_KEY`, or an issued key pair if one is active and wins selection. The `kid` header points at that signing key pair, shared by every node of the cluster (`/oauth/keys/*`). A trusted key only verifies the assertion of a token exchange; it is never checked on an API call.

## ERRORS

On API calls:

- `errors.UNAUTHORIZED` (`401`) — `Authorization` header missing, token expired, signature invalid, issuer untrusted, or `kid` not a usable key of this node.
- `errors.FORBIDDEN` (`403`) — token valid but caller lacks the required role for the operation.

The `/oauth/token` endpoint returns OAuth-shaped errors (`{"error": "...", "error_description": "..."}`, RFC 6749 §5.2) rather than the generic cyoda error envelope. The descriptions are fixed and never repeat the user id, the tenant or the key id.

- `405 method_not_allowed` — any method but `POST`. Carries `Allow: POST`.
- `401 invalid_client` — no Basic credentials, a client id that does not match `^[A-Za-z0-9]{1,100}$` (refused before the client store is read), an unknown client id, or a wrong secret. Carries `WWW-Authenticate: Basic realm="cyoda"`. Each of the last three makes the same store reads and one bcrypt comparison, so the server's own work does not depend on whether the client id exists. A storage backend can take longer to read a present key than a missing one (on Cassandra a hit is two queries and a miss one), which can let a caller who already holds a client id confirm that it exists. Client ids are not secret — a token's `sub` carries one — and generated ids are 80-bit random, so this does not allow enumeration.
- `400 unsupported_grant_type` — `grant_type` missing or not one of the two grants.
- `400 unauthorized_client` — `client_credentials` by an on-behalf-of client, or a token exchange by any other client.
- `400 invalid_request` — a `Content-Type` other than `application/x-www-form-urlencoded` (a `charset` parameter is accepted), refused before the client authenticates and without reading the body; a body over 1 MiB or one that does not parse as a form; on the token exchange: a refused RFC 8693 parameter; a `subject_token_type` other than `urn:ietf:params:oauth:token-type:jwt`; an assertion that does not parse, is not RS256 or has no `kid`; a `kid` that is not an active trusted key of the client's tenant, or a key outside its validity window; a signature that does not verify, or an `iss` the key does not list; `aud`, `exp`, `iat` or `nbf` missing or out of bounds; a `sub` that breaks the user-identifier rule; or a token that would expire at once.
- `403 access_denied` — the assertion's `caas_org_id` is not the client's tenant.
- `503 temporarily_unavailable` (with `Retry-After: 1`) — the client store or the trusted-key store could not be read. The request is refused; it is never served from a copy that might hold a deleted client or an invalidated key.
- `500 server_error` — any other store failure, a damaged client record or index entry (see `auth.clients`), or a signing failure. `error_description` carries a `ticket` for log correlation and no internal detail. A store failure is never answered `401`.

`GET /.well-known/jwks.json` answers `503` with `Retry-After` while the node's key copy is stale.

## SEE ALSO

- `auth.clients` — provision the plain, admin and on-behalf-of clients
- `auth.trusted-keys` — register the public key your application signs user assertions with
- `cli.token` — sign an admin token offline with the signing key
- `config.auth` — `CYODA_JWT_*` and the user-identifier rule
- `openapi` — `cyoda help openapi tags` and look for the `IAM` tag
