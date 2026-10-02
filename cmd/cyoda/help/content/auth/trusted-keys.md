---
topic: auth.trusted-keys
title: "auth.trusted-keys — register public keys for token-exchange subject tokens"
stability: evolving
version_added: 0.8.0
see_also:
  - auth
  - auth.tokens
  - config.auth
  - errors.TRUSTED_KEY_NOT_FOUND
  - errors.TRUSTED_KEY_CAP_REACHED
  - errors.STORAGE_UNAVAILABLE
  - errors.UNSUPPORTED_KEY_TYPE
  - errors.FEATURE_DISABLED
---

# auth.trusted-keys

## NAME

auth.trusted-keys — register a public key with cyoda so that JWTs you sign with the matching private key can be exchanged, by an M2M client of the same tenant, for a cyoda token on behalf of a user.

## GOAL

You have a system that knows who its users are and can sign JWTs (your own identity service, a gateway, a back-office tool). You want an M2M client to act in cyoda on behalf of those users, so that each call is attributed to the user rather than to the client.

Register the public key once. Your system signs a JWT for the user — the *subject token* — and your M2M client exchanges it at `POST /api/oauth/token` with the token-exchange grant. cyoda returns a cyoda token for that user. See `auth.tokens` for the grant.

A trusted-key JWT is used **only** as a subject token in that grant. cyoda does not accept it as a bearer token on API calls.

**Feature flag.** The 5 trusted-key endpoints under `/oauth/keys/trusted/*` are **off by default**. The operator must set `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true` to enable them; otherwise every endpoint returns `404 FEATURE_DISABLED`. This is intentional — trusted keys move the trust boundary, and that posture should be explicit.

## PREREQUISITES

**Admin (cyoda operator) sets up:**

- `CYODA_IAM_MODE=jwt`.
- `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true` (gate; see callout above).
- `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` (default `10`) — per-tenant cap on trusted keys that can verify. It counts every active key whose `validTo` has not passed, including one whose `validFrom` is still ahead. Trusted keys have no grace period: an invalidated key frees its slot at once. A registration with `invalidatePrevious` ends every other key of the tenant, so it is never refused by the cap. Reactivating a key is held to the same cap. To register or reactivate at the cap, delete or invalidate an old key first.
- `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` (default `365`) — default validity for trusted keys when not specified at registration.

**Client (you) needs:**

- A keypair you generated yourself. cyoda-go accepts `kty: "RSA"` only. Cloud also supports `kty: "EC"` and `kty: "OKP"`; cyoda-go parity is tracked for a future release.
- A `ROLE_ADMIN` cyoda token to register, list, delete and lifecycle the entry.
- An M2M client in the same tenant (`auth.clients`) to perform the exchange.

## REQUEST FLOW

### Register a public key

```bash
# Generate a keypair locally
openssl genrsa -out signing.pem 2048
openssl rsa -in signing.pem -pubout -out signing.pub
# Convert the public key to a JWK with your tooling of choice.

# -H @- reads the header from stdin: a command line is visible to other
# local users, stdin is not.
curl -X POST https://cyoda.example.com/api/oauth/keys/trusted \
  -H @- \
  -H "Content-Type: application/json" \
  -d '{
        "keyId": "my-signing-key-2026-06",
        "jwk":   { "kty": "RSA", "n": "<base64url-modulus>", "e": "AQAB" }
      }' \
  <<<"Authorization: Bearer ${ADMIN_TOKEN}"
```

Optional fields: `issuers` (when set, the subject token's `iss` must be one of them), `validFrom`, `validTo`, and `invalidatePrevious` (invalidates every other key of the tenant at once). Response (`200 OK`) echoes the registered key shape plus lifecycle metadata.

The key belongs to the tenant of the admin who registers it. Pick a stable, descriptive `keyId`: it becomes the `kid` header you set when signing. Key ids are unique within a tenant only — another tenant may register the same `keyId` for its own, independent key. Registering a `keyId` your tenant already has replaces that key (an upsert), so a retried registration succeeds.

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
           "user_roles": ["ROLE_USER"], "iat": <now>, "exp": <later> }
```

Your M2M client exchanges it:

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

cyoda looks up `kid` among the trusted keys **of the M2M client's tenant**, checks that the key is active and within its validity window, verifies the RS256 signature, and checks the claims below. The response carries a cyoda token for the user; use that token on API calls.

## TOKEN

A subject token you sign with a trusted-key private key must carry:

- `sub` — the user id. It becomes the issued token's user id, so it must pass the user-identifier rule in `config.auth`.
- `caas_org_id` — must equal the M2M client's tenant, which is also the tenant that registered the key.
- `exp` and `iat` — required; `nbf` is honoured if present.
- `iss` — checked only when the key was registered with `issuers`; it must then be one of them.
- Roles (`user_roles`, `roles`) are ignored: the issued token carries the M2M client's roles.

Cyoda does not mint subject tokens — you sign them. The claim shape of the token cyoda issues is in `auth.tokens`.

## ERRORS

Management endpoints:

- `errors.FEATURE_DISABLED` (`404`) — trusted-key endpoints called with `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=false`.
- `errors.TRUSTED_KEY_NOT_FOUND` (`404`) — the caller's tenant has no key with this `keyId`. A `keyId` is looked up in the caller's tenant only; another tenant's key with the same `keyId` is a different key.
- `errors.TRUSTED_KEY_CAP_REACHED` (`400`) — registering or reactivating a key would exceed the per-tenant cap; delete or invalidate an old key first.
- `errors.STORAGE_UNAVAILABLE` (`503`, retryable) — the store could not be read or written, on any of the endpoints, the list included. Any other store failure is `500` with a ticket.
- `errors.UNSUPPORTED_KEY_TYPE` (`400`) — `kty` is not `"RSA"`.
- `errors.UNAUTHORIZED` (`401`) — caller lacks a valid bearer for the management call.

Token exchange (OAuth error shape, see `auth.tokens`):

- `400 unauthorized_client` — the exchanging client is not an on-behalf-of client.
- `400 invalid_request` — the `subject_token_type` is not `urn:ietf:params:oauth:token-type:jwt`; the assertion does not parse, is not RS256, or has no `kid`; the `kid` is not an active trusted key of the client's tenant, or the key is outside its validity window; the signature does not verify; `iss` is not among the key's `issuers`; `aud` does not contain the cyoda issuer; `exp` or `iat` is missing, `exp − iat` exceeds 300 seconds, or a time claim fails; or `sub` is missing or breaks the user-identifier rule.
- `503 temporarily_unavailable` (with `Retry-After: 1`) — the trusted-key store could not be read. The exchange is refused; it is never served from a copy that might hold an invalidated key. Any other store failure is `500 server_error` with a ticket.
- `403 access_denied` — `caas_org_id` is not the client's tenant.

## SEE ALSO

- `auth.tokens` — the token-exchange grant and the claim shape of issued tokens
- `auth.clients` — the M2M client that performs the exchange
- `config.auth` — `CYODA_IAM_TRUSTED_KEY_*` env vars and the user-identifier rule
- `openapi` — `cyoda help openapi tags` and look for the `IAM` tag's `/oauth/keys/trusted/*` operations
