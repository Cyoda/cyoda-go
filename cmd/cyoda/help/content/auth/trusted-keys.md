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
  - errors.KEY_OWNED_BY_DIFFERENT_TENANT
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
- `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` (default `10`) — per-tenant cap on trusted keys that can verify. It counts every key that can still verify: an active key, and one in its grace period after invalidation, until its `validTo`. A key invalidated with a grace period keeps its slot until the period ends — including one invalidated by `invalidatePrevious`, so a rotation frees no slot for the key it registers. Reactivating a key is held to the same cap. To register or reactivate at the cap, delete an old key or invalidate it with no grace period first.
- `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` (default `365`) — default validity for trusted keys when not specified at registration.

**Client (you) needs:**

- A keypair you generated yourself. cyoda-go accepts `kty: "RSA"` only. Cloud also supports `kty: "EC"` and `kty: "OKP"`; cyoda-go parity is tracked for a future release.
- A `ROLE_ADMIN` cyoda token to register / delete / lifecycle the entry.
- An M2M client in the same tenant (`auth.clients`) to perform the exchange.

## REQUEST FLOW

### Register a public key

```bash
# Generate a keypair locally
openssl genrsa -out signing.pem 2048
openssl rsa -in signing.pem -pubout -out signing.pub
# Convert the public key to a JWK with your tooling of choice.

curl -X POST https://cyoda.example.com/api/oauth/keys/trusted \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
        "keyId":    "my-signing-key-2026-06",
        "audience": "human",
        "jwk":      { "kty": "RSA", "n": "<base64url-modulus>", "e": "AQAB" }
      }'
```

`audience` is required (`human` or `client`). Optional fields: `issuers` (when set, the subject token's `iss` must be one of them), `validFrom`, `validTo`, `invalidatePrevious` and `invalidateGracePeriodSec`. Response (`200 OK`) echoes the registered key shape plus lifecycle metadata.

The key belongs to the tenant of the admin who registers it. Pick a stable, descriptive `keyId`: it becomes the `kid` header you set when signing, and it must be unique across all tenants.

### List trusted keys

```bash
curl -X GET https://cyoda.example.com/api/oauth/keys/trusted \
  -H "Authorization: Bearer ${TOKEN}"
```

Returns the tenant's keys with status (active / invalidated) and validity window.

### Invalidate / reactivate

```bash
# Stop accepting subject tokens signed with this key, without removing the entry.
# Optional body: {"gracePeriodSec": 3600} keeps it verifying for up to that
# many seconds more, never past its validTo; without it, it stops at once.
curl -X POST https://cyoda.example.com/api/oauth/keys/trusted/${KEY_ID}/invalidate \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"

# Re-enable. validTo is required; validFrom defaults to now.
curl -X POST https://cyoda.example.com/api/oauth/keys/trusted/${KEY_ID}/reactivate \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{ "validTo": "2027-06-01T00:00:00Z" }'
```

### Delete

```bash
curl -X DELETE https://cyoda.example.com/api/oauth/keys/trusted/${KEY_ID} \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
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
curl -X POST https://cyoda.example.com/api/oauth/token \
  -u "${CLIENT_ID}:${CLIENT_SECRET}" \
  -d grant_type=urn:ietf:params:oauth:grant-type:token-exchange \
  -d subject_token_type=urn:ietf:params:oauth:token-type:jwt \
  -d subject_token="${SIGNED_JWT}"
```

cyoda looks up `kid` among the trusted keys **of the M2M client's tenant**, checks that the key is within its validity window (an invalidated key stays valid until its grace period ends), verifies the RS256 signature, and checks the claims below. The response carries a cyoda token for the user; use that token on API calls.

## TOKEN

A subject token you sign with a trusted-key private key must carry:

- `sub` — the user id. It becomes the issued token's user id, so it must pass the user-identifier rule in `config.auth`.
- `caas_org_id` — must equal the M2M client's tenant, which is also the tenant that registered the key.
- `exp` and `iat` — required; `nbf` is honoured if present.
- `iss` — checked only when the key was registered with `issuers`; it must then be one of them.
- `user_roles` (or `roles`) — the roles the issued token carries.

Cyoda does not mint subject tokens — you sign them. The claim shape of the token cyoda issues is in `auth.tokens`.

## ERRORS

Management endpoints:

- `errors.FEATURE_DISABLED` (`404`) — trusted-key endpoints called with `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=false`.
- `errors.TRUSTED_KEY_NOT_FOUND` (`404`) — referenced `keyId` not in the registry (also returned for cross-tenant access — the existence of another tenant's key is never confirmed).
- `errors.TRUSTED_KEY_CAP_REACHED` (`400`) — registering or reactivating a key would exceed the per-tenant cap; delete an old key or invalidate it with no grace period first.
- `errors.KEY_OWNED_BY_DIFFERENT_TENANT` (`409`) — registration request specifies a `keyId` that already belongs to another tenant. Pick a fresh `keyId`.
- `errors.UNSUPPORTED_KEY_TYPE` (`400`) — `kty` is not `"RSA"`.
- `errors.UNAUTHORIZED` (`401`) — caller lacks a valid bearer for the management call.

Token exchange (OAuth error shape, see `auth.tokens`):

- `400 invalid_grant` — the `subject_token_type` is not `urn:ietf:params:oauth:token-type:jwt`; the subject token does not parse, is not RS256, or has no `kid`; the `kid` is not a trusted key of the client's tenant, or the key is outside its validity window; the signature does not verify; `iss` is not among the key's `issuers`; a time claim fails; or `sub` is missing or breaks the user-identifier rule. The same `unknown trusted key` answer is given while cyoda cannot confirm its trusted-key cache is current, so no key is used that might have been revoked.
- `403 access_denied` — `caas_org_id` is not the client's tenant.

## SEE ALSO

- `auth.tokens` — the token-exchange grant and the claim shape of issued tokens
- `auth.clients` — the M2M client that performs the exchange
- `config.auth` — `CYODA_IAM_TRUSTED_KEY_*` env vars and the user-identifier rule
- `openapi` — `cyoda help openapi tags` and look for the `IAM` tag's `/oauth/keys/trusted/*` operations
