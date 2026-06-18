---
topic: auth.oidc
title: "auth.oidc — federated OIDC providers"
stability: evolving
version_added: 0.8.0
see_also:
  - auth
  - auth.tokens
  - config.auth
  - errors.OIDC_DISCOVERY_FAILED
  - errors.OIDC_JWKS_UNAVAILABLE
  - errors.OIDC_PROVIDER_DUPLICATE
  - errors.OIDC_PROVIDER_NOT_FOUND
  - errors.OIDC_PROVIDER_INACTIVE
  - errors.OIDC_SSRF_BLOCKED
  - errors.OIDC_INVALID_TENANT
  - errors.OIDC_AUDIENCE_MISMATCH
  - errors.OIDC_CLAIMS_INVALID
  - errors.OIDC_TOKEN_PRE_TRANSITION
---

# auth.oidc

## NAME

auth.oidc — register an external OIDC identity provider so JWTs it issues are accepted by cyoda directly, without re-minting at `/oauth/token`.

## GOAL

You already have an OIDC IdP — Cognito, Keycloak, Auth0, Okta, your own — and you want clients to present that IdP's JWTs to cyoda. Register the provider once; cyoda fetches its JWKS, validates inbound tokens against the keys, maps roles from the configured claim, and binds the resulting identity to a tenant.

Use this path when the IdP is the source of truth for user accounts. For pure M2M, `auth.clients` + `auth.tokens` is simpler.

## PREREQUISITES

**Admin (cyoda operator) sets up:**

- `CYODA_IAM_MODE=jwt` (federated OIDC is unavailable in mock mode).
- `CYODA_OIDC_REQUIRE_HTTPS=true` for production. Set to `false` only for dev IdPs over plain HTTP.
- `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=false` for production. Setting to `true` disables the SSRF blocklist that prevents `wellKnownConfigUri` resolving to RFC 1918 / loopback / link-local addresses.
- `CYODA_OIDC_ROLES_CLAIM` (default `roles`) — global default JWT claim from which roles are read. Per-provider override available at registration.
- `CYODA_OIDC_CONNECT_TIMEOUT_MS`, `CYODA_OIDC_SOCKET_TIMEOUT_MS`, `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS` (each default `5000`) — HTTP timeouts for discovery + JWKS fetches.

**Client (you) needs:**

- A working `.well-known/openid-configuration` URL on the IdP.
- A `ROLE_ADMIN` cyoda token to register the provider.
- A **UUID-shaped tenant ID** — the bootstrap convenience literal `default-tenant` is rejected by registration (returns `OIDC_INVALID_TENANT`) because non-UUID tenant identifiers collide in storage.

## REQUEST FLOW

### Register a provider

```bash
curl -X POST https://cyoda.example.com/api/oauth/oidc/providers \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
        "wellKnownConfigUri": "https://idp.example.com/.well-known/openid-configuration",
        "issuers":            ["https://idp.example.com/"],
        "expectedAudiences":  ["cyoda-prod"],
        "rolesClaim":         "cognito:groups"
      }'
```

Response (`200 OK`) carries the full provider DTO:

```json
{
  "id":                 "f47ac10b-58cc-…",
  "wellKnownConfigUri": "https://idp.example.com/.well-known/openid-configuration",
  "active":             true,
  "createdAt":          "2026-06-17T12:00:00Z",
  "issuers":            ["https://idp.example.com/"],
  "expectedAudiences":  ["cyoda-prod"],
  "rolesClaim":         "cognito:groups"
}
```

Behaviour on `expectedAudiences`:

- Omitted, `null`, or empty array → no `aud` enforcement (issuer-binding is the trust anchor).
- Non-empty → inbound JWT `aud` claim must match at least one entry byte-wise.

Behaviour on `issuers`:

- Omitted, `null`, or empty → cyoda enforces the `iss` claim matches the discovery document's `issuer` field byte-wise (OIDC Core 1.0 §2).
- Non-empty → inbound JWT `iss` must match one of the listed values byte-wise.

### List providers

```bash
curl -X GET "https://cyoda.example.com/api/oauth/oidc/providers?activeOnly=true" \
  -H "Authorization: Bearer ${TOKEN}"
```

`activeOnly=true` filters out invalidated providers. Available to any authenticated tenant member, not just admin.

### Update a provider (tri-state PATCH)

```bash
curl -X PATCH https://cyoda.example.com/api/oauth/oidc/providers/${PROVIDER_ID} \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
        "expectedAudiences": ["cyoda-prod", "cyoda-staging"]
      }'
```

Per-field tri-state semantics: absent = unchanged, `null` or `[]` = clear, value = set. Same for `issuers` and `rolesClaim`.

### Invalidate / reactivate / delete

```bash
# Invalidate — token validation stops accepting this provider's JWTs immediately.
curl -X POST https://cyoda.example.com/api/oauth/oidc/providers/${PROVIDER_ID}/invalidate \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"

# Reactivate — by default refreshes JWKS from upstream.
curl -X POST https://cyoda.example.com/api/oauth/oidc/providers/${PROVIDER_ID}/reactivate \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"reactivateKeys": true}'

# Delete permanently.
curl -X DELETE https://cyoda.example.com/api/oauth/oidc/providers/${PROVIDER_ID} \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

### Reload JWKS for all providers

```bash
curl -X POST https://cyoda.example.com/api/oauth/oidc/providers/reload \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

Forces an immediate JWKS refresh for every active provider on the receiving node. In a multi-node cluster the reload is broadcast.

### Present an IdP-issued JWT

Once registered, clients present the IdP's JWT directly:

```bash
curl -X GET https://cyoda.example.com/api/clients \
  -H "Authorization: Bearer ${IDP_ISSUED_JWT}"
```

The token is validated against the provider's JWKS; roles are read from the configured `rolesClaim`; identity is bound to the tenant that registered the provider.

## TOKEN

JWTs issued by the federated IdP must conform to the universal cyoda claim contract documented in `auth.tokens`. In particular:

- `iss` must match per the `issuers` / discovery-document rule above.
- `aud` must match `expectedAudiences` if set.
- The configured `rolesClaim` (per-provider override or `CYODA_OIDC_ROLES_CLAIM`) must yield a string array of role values.

Cyoda does not re-mint federated tokens — they are validated and trusted directly.

## DIAGNOSTICS

### "I get 401 but my token looks valid"

When two tenants register the **same IdP** with overlapping or empty `expectedAudiences`, cyoda cannot deterministically route an inbound token to one tenant rather than the other. Internally this is `ErrAmbiguousProvider` (in `internal/auth/oidc/registry.go`), but it is wrapped in `ErrUnknownKID` and surfaces as a generic `401 UNAUTHORIZED` — *not* a dedicated error code.

This is deliberate: a precise "two tenants claim your IdP" error would leak provider-routing topology across the tenant boundary. The diagnostic path is server-side logs (look for `oidc.registry` resolve events identifying the conflict), not the HTTP response.

To resolve: pick **disjoint** `expectedAudiences` for the same IdP across tenants, or accept that one tenant must register and the other federate through it.

### "JWKS warmup window"

After registering a provider, cyoda warms JWKS asynchronously. During the cold-start window (typically <1s), tokens whose `kid` is not yet cached fall through to `ErrUnknownKID` → 401. Retry; or force-warm with the `/reload` endpoint above.

## ERRORS

- `errors.OIDC_DISCOVERY_FAILED` (`502`) — `wellKnownConfigUri` unreachable or returned malformed JSON.
- `errors.OIDC_JWKS_UNAVAILABLE` (`502`) — discovery succeeded but the JWKS endpoint did not.
- `errors.OIDC_SSRF_BLOCKED` (`400`) — `wellKnownConfigUri` resolves to a blocked address range (set `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true` for dev).
- `errors.OIDC_PROVIDER_DUPLICATE` (`400`) — same `wellKnownConfigUri` already registered for this tenant.
- `errors.OIDC_PROVIDER_NOT_FOUND` (`404`) — referenced provider ID absent in this tenant.
- `errors.OIDC_PROVIDER_INACTIVE` (`409`) — update or operation attempted on an invalidated provider; reactivate first.
- `errors.OIDC_INVALID_TENANT` (`400`) — caller's tenant ID is not UUID-shaped (commonly: bootstrap `default-tenant` literal).
- `errors.OIDC_AUDIENCE_MISMATCH` (`401`) — token `aud` does not match any `expectedAudiences`.
- `errors.OIDC_CLAIMS_INVALID` (`401`) — required claim missing or malformed.
- `errors.OIDC_TOKEN_PRE_TRANSITION` (`401`) — token `iat` precedes the most recent provider reactivation; mint a fresh one.
- `errors.UNAUTHORIZED` (`401`) — generic fallback (includes the ambiguous-routing case described in DIAGNOSTICS).

## SEE ALSO

- `auth.tokens` — universal claim contract
- `config.auth` — `CYODA_OIDC_*` env vars
- `openapi` — `cyoda help openapi tags` and look for `OAuth, OIDC Providers`
