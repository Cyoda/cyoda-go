---
topic: auth.trusted-keys
title: "auth.trusted-keys — register public keys for offline JWT signing"
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

auth.trusted-keys — register a public key with cyoda so JWTs you sign offline with the matching private key are accepted, without an IdP or `/oauth/token` round-trip.

## GOAL

You have a workload that can sign JWTs (a CI job, a controller, an air-gapped service) and you don't want to depend on cyoda being reachable for token minting or an external IdP for discovery. Register the public key once; thereafter your workload signs JWTs locally and cyoda validates them by `kid` lookup against the registry.

Use this path when token minting must work even when cyoda's `/oauth/token` is unreachable, or when you want zero IdP infrastructure.

**Feature flag.** The 5 trusted-key endpoints under `/oauth/keys/trusted/*` are **off by default**. The operator must set `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true` to enable them; otherwise every endpoint returns `404 FEATURE_DISABLED`. This is intentional — trusted keys move the trust boundary, and that posture should be explicit.

## PREREQUISITES

**Admin (cyoda operator) sets up:**

- `CYODA_IAM_MODE=jwt`.
- `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true` (gate; see callout above).
- `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` (default `10`) — per-tenant cap on currently-valid trusted keys. Counts active + within-validity entries only.
- `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` (default `365`) — default validity for trusted keys when not specified at registration.

**Client (you) needs:**

- A keypair you generated yourself. cyoda-go v0.8.0 supports `kty: "RSA"` only. Cloud also supports `kty: "EC"` and `kty: "OKP"`; cyoda-go parity is tracked for a future release.
- A `ROLE_ADMIN` cyoda token to register / delete / lifecycle the entry.

## REQUEST FLOW

### Register a public key

```bash
# Generate a keypair locally
openssl genrsa -out signing.pem 2048
openssl rsa -in signing.pem -pubout -out signing.pub
# Convert the public key to JWK shape — your tooling of choice;
# the API expects the JWK members at the top level.

curl -X POST https://cyoda.example.com/api/oauth/keys/trusted \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
        "keyId": "my-signing-key-2026-06",
        "kty":   "RSA",
        "n":     "<base64url-modulus>",
        "e":     "AQAB"
      }'
```

Response (`200 OK`) echoes the registered key shape plus lifecycle metadata.

Pick a stable, descriptive `keyId`. It becomes the JWT `kid` header you must set when signing.

### List trusted keys

```bash
curl -X GET https://cyoda.example.com/api/oauth/keys/trusted \
  -H "Authorization: Bearer ${TOKEN}"
```

Returns the tenant's keys with status (active / invalidated) and validity window.

### Invalidate / reactivate

```bash
# Stop accepting tokens signed with this key, without removing the entry.
curl -X POST https://cyoda.example.com/api/oauth/keys/trusted/${KEY_ID}/invalidate \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"

# Re-enable.
curl -X POST https://cyoda.example.com/api/oauth/keys/trusted/${KEY_ID}/reactivate \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

### Delete

```bash
curl -X DELETE https://cyoda.example.com/api/oauth/keys/trusted/${KEY_ID} \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

### Sign and present a token

Your workload signs a JWT with the matching private key, setting `kid` to the registered `keyId`:

```text
Header:  { "alg": "RS256", "typ": "JWT", "kid": "my-signing-key-2026-06" }
Payload: { universal cyoda claim contract — see auth.tokens }
```

Present it like any other cyoda token:

```bash
curl -X GET https://cyoda.example.com/api/clients \
  -H "Authorization: Bearer ${SIGNED_JWT}"
```

cyoda looks up `kid` in the trusted-key registry, validates the RS256 signature against the registered public key, then enforces the universal claim checks (`iss`, `aud`, `exp`, …).

## TOKEN

JWTs you sign with a trusted-key private key must conform to the universal cyoda claim contract documented in `auth.tokens`. In particular:

- `iss` must match `CYODA_JWT_ISSUER` (typically the value you set on the cyoda deployment) — trusted-key tokens are *not* issuer-bound to your IdP because there is no IdP.
- `aud` is checked against `CYODA_JWT_AUDIENCE` if set.
- `caas_org_id` must match the tenant that registered the key.

Cyoda does not mint trusted-key JWTs — you sign them. This page covers the registration + lifecycle of the public key; the claim contract is in `auth.tokens`.

## ERRORS

- `errors.FEATURE_DISABLED` (`404`) — trusted-key endpoints called with `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=false`.
- `errors.TRUSTED_KEY_NOT_FOUND` (`404`) — referenced `keyId` not in the registry (also returned for cross-tenant access — the existence of another tenant's key is never confirmed).
- `errors.TRUSTED_KEY_CAP_REACHED` (`400`) — per-tenant cap reached; delete or invalidate an old key first.
- `errors.KEY_OWNED_BY_DIFFERENT_TENANT` (`409`) — registration request specifies a `keyId` that already belongs to another tenant. Pick a fresh `keyId`.
- `errors.UNSUPPORTED_KEY_TYPE` (`400`) — `kty` is not `"RSA"` (the only type cyoda-go v0.8.0 accepts).
- `errors.UNAUTHORIZED` (`401`) — caller lacks a valid bearer for the management call; or, at validation time, the signing key is unknown / invalidated / expired.

## SEE ALSO

- `auth.tokens` — universal JWT claim contract
- `config.auth` — `CYODA_IAM_TRUSTED_KEY_*` env vars
- `openapi` — `cyoda help openapi tags` and look for the `IAM` tag's `/oauth/keys/trusted/*` operations
