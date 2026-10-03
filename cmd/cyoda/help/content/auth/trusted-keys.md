---
topic: auth.trusted-keys
title: "auth.trusted-keys — register public keys for token-exchange subject tokens"
stability: evolving
version_added: 0.8.0
see_also:
  - auth
  - auth.integration
  - auth.tokens
  - config.auth
  - errors.TRUSTED_KEY_NOT_FOUND
  - errors.TRUSTED_KEY_CAP_REACHED
  - errors.STORAGE_UNAVAILABLE
  - errors.UNSUPPORTED_KEY_TYPE
  - errors.FEATURE_DISABLED
  - errors.FORBIDDEN
---

# auth.trusted-keys

## NAME

auth.trusted-keys — register a public key with cyoda so that user assertions you sign with the matching private key can be exchanged, by an on-behalf-of client of the same tenant, for a cyoda token on behalf of a user.

## GOAL

Your application signs its users in and decides what each may do. You want it to call cyoda for those users, so that each change is recorded for the user, with the application's client as its executor.

Register the public key once. For each user, your application signs a short JWT — the *user assertion*, sent as the `subject_token` — and its on-behalf-of client exchanges it at `POST /api/oauth/token` with the token-exchange grant. cyoda returns a cyoda token for that user, carrying the client's roles. cyoda records the user the assertion names; it does not verify the user. See `auth.tokens` for the grant.

A trusted-key JWT is used **only** as the subject token of that grant. cyoda does not accept it as a bearer token on API calls.

**Feature flag.** The 5 trusted-key endpoints under `/oauth/keys/trusted/*` are **off by default**. The operator must set `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true` to enable them; otherwise every endpoint returns `404 FEATURE_DISABLED`. This is intentional — trusted keys move the trust boundary, and that posture should be explicit. Each node reads the flag at startup and the node that takes a call decides by its own value, so in a cluster set it on every node. The flag gates the management endpoints only: the token exchange works with every key registered while it was on, on every node, whatever that node's flag.

## PREREQUISITES

**Admin (cyoda operator) sets up:**

- `CYODA_IAM_MODE=jwt`.
- `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true` (gate; see callout above).
- `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` (default `10`) — per-tenant cap on trusted keys that can verify. It counts every active key whose `validTo` has not passed, including one whose `validFrom` is still ahead. Trusted keys have no grace period: an invalidated key frees its slot at once. A registration with `invalidatePrevious` ends every other key of the tenant, so it is never refused by the cap. Reactivating a key is held to the same cap. To register or reactivate at the cap, delete or invalidate an old key first.
- `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` (default `365`) — the validity, in days from `validFrom`, of a key registered without `validTo`. Despite its name it is not a maximum: a `validTo` you send is not limited by it.

**Client (you) needs:**

- A keypair you generated yourself. cyoda-go accepts `kty: "RSA"` only. Cloud also supports `kty: "EC"` and `kty: "OKP"`; cyoda-go parity is tracked for a future release.
- A `ROLE_ADMIN` cyoda token to register, list, delete and lifecycle the entry.
- An on-behalf-of client in the same tenant (`POST /clients?onBehalfOf=true`, see `auth.clients`) to perform the exchange. Any other client is refused the exchange.

## REQUEST FLOW

### Register a public key

```bash
# Generate a keypair locally
openssl genrsa -out signing.pem 2048
# The JWK modulus n: the public modulus, base64url without padding.
N=$(openssl rsa -in signing.pem -noout -modulus | cut -d= -f2 \
    | xxd -r -p | openssl base64 -A | tr '+/' '-_' | tr -d '=')
# openssl genrsa uses the public exponent 65537, whose JWK form is "AQAB".

# -H @- reads the header from stdin: a command line is visible to other
# local users, stdin is not.
curl -X POST https://cyoda.example.com/api/oauth/keys/trusted \
  -H @- \
  -H "Content-Type: application/json" \
  -d '{
        "keyId": "my-signing-key-2026-06",
        "jwk":   { "kty": "RSA", "n": "<the value of N>", "e": "AQAB" }
      }' \
  <<<"Authorization: Bearer ${ADMIN_TOKEN}"
```

Optional fields: `issuers` (when set, the subject token's `iss` must be one of them), `validFrom`, `validTo`, and `invalidatePrevious` (invalidates every other key of the tenant at once). Response (`200 OK`) echoes the registered key shape plus lifecycle metadata.

The JWK is an RSA public key with a modulus of 2048 to 4096 bits (at most 512 bytes) and a positive, odd public exponent that fits a machine integer. A JWK that carries a private member (`d`, `p`, `q`, `dp`, `dq`, `qi` or `oth`) is refused with `400 BAD_REQUEST`, and the detail names the member. cyoda stores and returns only the public members `kty`, `kid` (set to the `keyId`), `n` and `e`, plus `alg` and `use` when you send them (as strings); every other member you send is dropped.

The key belongs to the tenant of the admin who registers it. Pick a stable, descriptive `keyId`: it becomes the `kid` header you set when signing. A `keyId` is 1 to 128 characters from `A`–`Z`, `a`–`z`, `0`–`9`, `.`, `_` and `-` (`^[A-Za-z0-9._-]{1,128}$`); the path `{keyId}` of the other endpoints follows the same rule. If the JWK carries a `kid`, it must equal the `keyId`. A trusted key belongs to the tenant, not to a client: any on-behalf-of client of the tenant can exchange assertions signed with any active key of the tenant. Key ids are unique within a tenant only — another tenant may register the same `keyId` for its own, independent key. Registering a `keyId` your tenant already has replaces that key (an upsert), so a retried registration succeeds.

### List trusted keys

```bash
curl -X GET https://cyoda.example.com/api/oauth/keys/trusted \
  -H @- <<<"Authorization: Bearer ${ADMIN_TOKEN}"
```

Returns the tenant's keys with status (active / invalidated) and validity window.

### Invalidate / reactivate

```bash
# Stop accepting subject tokens signed with this key at once, without
# removing the entry. The request has no body: trusted keys have no grace
# period.
curl -X POST https://cyoda.example.com/api/oauth/keys/trusted/${KEY_ID}/invalidate \
  -H @- <<<"Authorization: Bearer ${ADMIN_TOKEN}"

# Re-enable. validTo is required; validFrom defaults to now.
curl -X POST https://cyoda.example.com/api/oauth/keys/trusted/${KEY_ID}/reactivate \
  -H @- \
  -H "Content-Type: application/json" \
  -d '{ "validTo": "2027-06-01T00:00:00Z" }' \
  <<<"Authorization: Bearer ${ADMIN_TOKEN}"
```

Every change is written to the shared store, and every token exchange reads
the key from the store, so a registration, invalidation, reactivation or
delete is in force on every node when the call returns. A reactivation's
`validTo` comes from the request, but its default `validFrom` is the clock of
the node that takes the call, and each node checks the window against its own
clock.

### Delete

```bash
curl -X DELETE https://cyoda.example.com/api/oauth/keys/trusted/${KEY_ID} \
  -H @- <<<"Authorization: Bearer ${ADMIN_TOKEN}"
```

### Sign a subject token and exchange it

Your system signs a JWT for the user with the matching private key, setting `kid` to the registered `keyId`:

```text
Header:  { "alg": "RS256", "typ": "JWT", "kid": "my-signing-key-2026-06" }
Payload: { "sub": "<user id>", "caas_org_id": "<your tenant>",
           "aud": "<CYODA_JWT_ISSUER>",
           "iat": <now>, "exp": <now + at most 300> }
```

Your on-behalf-of client exchanges it:

```bash
# The client secret and the subject token go on stdin (-K-), not the
# command line.
curl -X POST https://cyoda.example.com/api/oauth/token \
  -d grant_type=urn:ietf:params:oauth:grant-type:token-exchange \
  -d subject_token_type=urn:ietf:params:oauth:token-type:jwt \
  -K- <<EOF
user = "${CLIENT_ID}:${CLIENT_SECRET}"
data = "subject_token=${SIGNED_JWT}"
EOF
```

cyoda looks up `kid` among the trusted keys **of the client's tenant**, checks that the key is active and within its validity window, verifies the RS256 signature, and checks the claims below. The response carries a cyoda token for the user, with the client's roles; use that token on API calls for that user.

## TOKEN

A subject token you sign with a trusted-key private key must carry:

- `sub` — the user id. It becomes the issued token's user id, so it must pass the user-identifier rule in `config.auth`.
- `caas_org_id` — must equal the client's tenant, which is also the tenant that registered the key.
- `aud` — must contain `CYODA_JWT_ISSUER` (a string or an array).
- `exp` and `iat` — required, with `exp − iat` at most 300 seconds; `nbf` is honoured if present. Every time claim is checked with 30 seconds of clock skew.
- `iss` — checked only when the key was registered with `issuers`; it must then be one of them.
- Roles (`user_roles`, `roles`) are ignored: the issued token carries the client's roles. Every other claim is ignored too, and so is the header's `typ`.
- `jti` is not checked, and cyoda keeps no record of used assertions: an assertion can be exchanged again until it expires. Sign a fresh one for each exchange, with a short life, and keep it on your server.

Cyoda does not mint subject tokens — you sign them. The claim shape of the token cyoda issues is in `auth.tokens`.

## ERRORS

Management endpoints:

- `errors.FEATURE_DISABLED` (`404`) — trusted-key endpoints called with `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=false`.
- `errors.TRUSTED_KEY_NOT_FOUND` (`404`) — the caller's tenant has no key with this `keyId`. A `keyId` is looked up in the caller's tenant only; another tenant's key with the same `keyId` is a different key.
- `errors.TRUSTED_KEY_CAP_REACHED` (`400`) — registering or reactivating a key would exceed the per-tenant cap; delete or invalidate an old key first.
- `errors.STORAGE_UNAVAILABLE` (`503`, retryable) — the store could not be read or written, on any of the endpoints, the list included.
- `errors.SERVER_ERROR` (`500`) — any other store failure, on any of the endpoints; the body carries a generic message and a `ticket`.
- `errors.NOT_IMPLEMENTED` (`501`) — `CYODA_IAM_MODE` is `mock`, which has no trusted-key store (while the flag is on; with it off, `404 FEATURE_DISABLED` answers first).
- `errors.UNSUPPORTED_KEY_TYPE` (`400`) — `kty` is not `"RSA"`.
- `errors.BAD_REQUEST` (`400`) — on register: the body is malformed; the `keyId` does not match `^[A-Za-z0-9._-]{1,128}$` ("invalid keyId format", also on the other endpoints' path); the JWK carries a private member, has more than `CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES` members, has no `kty`, has a `kid` other than the `keyId`, has a modulus under 2048 bits, or has a non-string `alg` or `use`; or the validity window is out of range or `validTo` is not after `validFrom`.
- `errors.UNAUTHORIZED` (`401`) — caller lacks a valid bearer for the management call.
- `errors.FORBIDDEN` (`403`) — the caller's token lacks `ROLE_ADMIN`, or is an on-behalf-of token.

Token exchange (OAuth error shape, see `auth.tokens`):

- `400 unauthorized_client` — the exchanging client is not an on-behalf-of client.
- `400 invalid_request` — an RFC 8693 parameter cyoda refuses (`actor_token`, `actor_token_type`, `resource`, `audience`, `scope`, `requested_token_type`) is present; the `subject_token_type` is not `urn:ietf:params:oauth:token-type:jwt`; the assertion does not parse, is not RS256, or has no `kid`; the `kid` is not an active trusted key of the client's tenant, or the key is outside its validity window; the signature does not verify; `iss` is not among the key's `issuers`; `aud` does not contain the cyoda issuer; `exp` or `iat` is missing, `exp − iat` exceeds 300 seconds, or a time claim fails; `sub` is missing or breaks the user-identifier rule; or the assertion's `exp` is not after now, so the token would expire at once. Each cause has its own fixed `error_description`, listed in `auth.tokens`.
- `503 temporarily_unavailable` (with `Retry-After: 1`) — the trusted-key store could not be read. The exchange is refused; it is never served from a copy that might hold an invalidated key. Any other store failure is `500 server_error` with a ticket.
- `403 access_denied` — `caas_org_id` is not the client's tenant.
- `429 slow_down` — the client has used its `CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE` on this node (see `auth.tokens`).

## SEE ALSO

- `auth.tokens` — the token-exchange grant and the claim shape of issued tokens
- `auth.clients` — the on-behalf-of client that performs the exchange
- `config.auth` — `CYODA_IAM_TRUSTED_KEY_*` env vars and the user-identifier rule
- `openapi` — `cyoda help openapi tags`: the `/oauth/keys/trusted/*` operations are under the `OAuth, Keys` tag (`oauth-keys`)
