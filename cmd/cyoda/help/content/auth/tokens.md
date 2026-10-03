---
topic: auth.tokens
title: "auth.tokens — /oauth/token grants and JWT claim contract"
stability: evolving
version_added: 0.8.0
see_also:
  - auth
  - auth.integration
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
- `CYODA_JWT_SIGNING_KEY` (RSA private key, PEM or base64-encoded PEM; tokens cyoda issues are signed with this)
- `CYODA_JWT_ISSUER` (default `cyoda`; cyoda's own name, not an identity provider's; populates the `iss` claim; must not be empty; a user assertion's `aud` must contain it)
- `CYODA_JWT_AUDIENCE` (default empty = no `aud` check on inbound tokens, and no `aud` on issued tokens; it plays no part in a user assertion)
- `CYODA_JWT_EXPIRY_SECONDS` (default `300`, maximum `3600`)

See `config.auth` for the full env-var reference.

**Client (you) needs** an M2M client (see `auth.clients`). Each kind of client uses one grant:

- A plain client (`ROLE_M2M`) or an admin client (`ROLE_M2M`, `ROLE_ADMIN`) uses `client_credentials`.
- An on-behalf-of client (`ROLE_M2M`, created with `?onBehalfOf=true`) uses the token exchange.

For the token exchange, the tenant also needs a trusted key whose private half your application signs user assertions with (`auth.trusted-keys`).

## REQUEST FLOW

The endpoint is `POST /api/oauth/token` (under `CYODA_CONTEXT_PATH`, `/api` by default). For the whole integration, step by step, see `auth.integration`.

**Client authentication** is HTTP Basic only (`Authorization: Basic base64(client_id:client_secret)`). A `client_id` and `client_secret` sent as form fields (`client_secret_post`) are not read: such a request has no Basic header and answers `401 invalid_client`. As RFC 6749 §2.3.1 allows, the id and the secret may be form-urlencoded before base64; cyoda decodes both. A generated client id is 16 characters from `0`–`9` and `A`–`V`, and a generated secret 64 lower-case hex characters, so encoding leaves them unchanged.

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
  "expires_in":   300
}
```

`expires_in` is the token's remaining life in seconds (`exp` minus now). Use the `access_token` as `Authorization: Bearer …` on every subsequent API call. cyoda issues no refresh token and no `scope`: mint again when less than about 60 seconds remain, and once more if a call answers `401`.

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

Every time claim is checked with 30 seconds of clock skew; an `iat` more than 30 seconds in the future is refused. A missing or different `caas_org_id` answers `403 access_denied`. Every other claim is ignored, roles included, and so is the header's `typ`. cyoda does not check `jti` and keeps no record of used assertions, so an assertion can be exchanged again until it expires: sign a fresh one for each exchange and keep it on your server. An assertion cannot set a display name: cyoda records the user id alone.

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

The token expires at the earlier of the assertion's `exp` and now + `CYODA_JWT_EXPIRY_SECONDS`; `expires_in` is its remaining life. An assertion lives at most 300 seconds, so an on-behalf-of token never lives more than 300 seconds past its assertion's `iat`, whatever `CYODA_JWT_EXPIRY_SECONDS` says. Cache one token per user and exchange a new assertion when less than about 60 seconds remain.

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

- `errors.UNAUTHORIZED` (`401`) — `Authorization` header missing, token expired, signature invalid, `iss` other than `CYODA_JWT_ISSUER`, `aud` missing `CYODA_JWT_AUDIENCE` when that is set, or `kid` not a usable key of this node. A token cyoda did not sign — an identity provider's token, a user assertion — is refused here. Authentication runs before any handler and before any transaction is joined, on HTTP and gRPC alike: a `401` request did nothing, so it can be sent again, once, with a new token.
- `errors.FORBIDDEN` (`403`) — token valid but caller lacks the required role for the operation (`ROLE_M2M` for every data operation), or a token-exchange token on a client, trusted-key, key-pair or `/admin/*` operation, which it never reaches. Each is decided before the operation reads or writes anything.

The `/oauth/token` endpoint returns OAuth-shaped errors (`{"error": "...", "error_description": "..."}`, RFC 6749 §5.2) rather than the generic cyoda error envelope. Every `error_description` is one of the fixed strings below, so it tells the causes apart; none repeats the user id, the tenant or the key id. Every error response carries `Cache-Control: no-store`. The endpoint changes nothing, so a retry is always safe; the list says when one can succeed. In mock IAM mode the endpoint issues no token and answers `501 NOT_IMPLEMENTED` in the generic cyoda error envelope, not an OAuth error. In jwt mode, in the order cyoda checks:

- `405 method_not_allowed`, `"method_not_allowed"` — any method but `POST`. Carries `Allow: POST`.
- `400 invalid_request`, `"the request body must be application/x-www-form-urlencoded"` — any other `Content-Type` (a `charset` parameter is accepted). Refused before the client authenticates, without reading the body.
- `401 invalid_client`, `"client authentication failed"` — no Basic credentials, a client id that does not match `^[A-Za-z0-9]{1,100}$` (refused before the client store is read), an unknown client id, or a wrong secret. Carries `WWW-Authenticate: Basic realm="cyoda"`. Each of the last three makes the same store reads and one bcrypt comparison, so the server's own work does not depend on whether the client id exists. A storage backend can take longer to read a present key than a missing one (on Cassandra a hit is two queries and a miss one), which can let a caller who already holds a client id confirm that it exists. Client ids are not secret — a token's `sub` carries one — and generated ids are 80-bit random, so this does not allow enumeration. Re-read the credentials once; do not retry in a loop.
- `503 temporarily_unavailable`, `"temporarily_unavailable"` (with `Retry-After: 1`) — the client store could not be read, or no client-secret check slot freed up within 1 second (`CYODA_IAM_TOKEN_MAX_CONCURRENT_SECRET_CHECKS`). Retry after `Retry-After`, with backoff.
- `500 server_error`, `"server_error [ticket: <uuid>]"` — any other client-store failure, or a damaged client record or index entry (see `auth.clients`). The ticket names the server's log line; no internal detail is sent. A store failure is never answered `401`. Retry a few times with backoff; if it persists, give the ticket to the operator.
- `400 invalid_request`, `"malformed request body"` — a body over 1 MiB, or one that does not parse as a form.
- `400 unsupported_grant_type`, `"unsupported_grant_type"` — `grant_type` missing or not one of the two grants.
- `400 unauthorized_client`, `"this client may only exchange user assertions"` — `client_credentials` by an on-behalf-of client.
- `400 unauthorized_client`, `"this client may not exchange user assertions"` — a token exchange by any other client, refused before its assertion is read.
- `429 slow_down`, `"slow_down"` (with `Retry-After`, whole seconds until the next request is allowed) — the client has used its `CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE` on this node. Counted after the client authenticates and is accepted for the grant, across both grants; other clients are not affected.

On the token exchange only, after the checks above:

- `400 invalid_request`, `"unsupported parameter"` — `actor_token`, `actor_token_type`, `resource`, `audience`, `scope` or `requested_token_type` is present, in the body or the query string, even empty.
- `400 invalid_request`, `"unsupported subject_token_type"` — `subject_token_type` is not `urn:ietf:params:oauth:token-type:jwt`.
- `400 invalid_request`, `"invalid subject token"` — the assertion does not parse as a JWT, its `alg` is not `RS256`, or it has no `kid`.
- `400 invalid_request`, `"unknown or inactive trusted key"` — the `kid` is not a trusted key of the client's tenant, or the key is invalidated or outside its validity window.
- `503 temporarily_unavailable` (with `Retry-After: 1`) or `500 server_error` — the trusted-key store could not be read, as for the client store above. The exchange is never served from a copy that might hold an invalidated key.
- `400 invalid_request`, `"subject token signature or issuer rejected"` — the signature does not verify with that key, or the key lists `issuers` and `iss` is not one of them.
- `400 invalid_request`, `"subject token claims rejected"` — `aud` does not contain `CYODA_JWT_ISSUER`; `exp` or `iat` is missing or not a number; `exp − iat` is over 300 seconds; `iat` is more than 30 seconds in the future; `exp` is 30 seconds or more in the past; or `nbf` is not a number or more than 30 seconds in the future.
- `403 access_denied`, `"tenant mismatch"` — `caas_org_id` is missing or is not the client's tenant.
- `400 invalid_request`, `"subject token sub rejected"` — `sub` is missing or breaks the user-identifier rule (`config.auth`), the reserved id `system` included.
- `400 invalid_request`, `"subject token has expired"` — the token would expire at once: the assertion's `exp` is not after now.

Signing the token can fail too: `500 server_error` with a ticket.

`GET /api/.well-known/jwks.json` (the JWKS document lives under `CYODA_CONTEXT_PATH`, like `/api/oauth/token`) answers `503` with `Retry-After` while the node's key copy is stale.

**Rate-limit the token endpoint at ingress.** `/oauth/token` authenticates callers that are not yet authenticated, and each request with an unknown client id or a wrong secret costs one bcrypt comparison. cyoda bounds that work per node with `CYODA_IAM_TOKEN_MAX_CONCURRENT_SECRET_CHECKS`; past the bound it answers `503 temporarily_unavailable` and does no more work (it fails closed). The bound protects the node's CPU, not the endpoint's availability: a flood of bad credentials can keep legitimate clients getting `503`. The per-client `CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE` limit applies only after a client authenticates, so it does not stop such a flood. Deployments must put a per-source rate limit in front of `/api/oauth/token` at the ingress, gateway or load balancer.

## SEE ALSO

- `auth.clients` — provision the plain, admin and on-behalf-of clients
- `auth.trusted-keys` — register the public key your application signs user assertions with
- `cli.token` — sign an admin token offline with the signing key
- `config.auth` — `CYODA_JWT_*` and the user-identifier rule
- `openapi` — `cyoda help openapi tags` and look for the `IAM` tag
