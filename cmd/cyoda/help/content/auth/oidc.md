---
topic: auth.oidc
title: "auth.oidc — federated OIDC providers"
stability: evolving
version_added: 0.8.0
see_also:
  - auth
  - auth.tokens
  - config.auth
  - errors.OIDC_INVALID_TENANT
  - errors.OIDC_PROVIDER_DUPLICATE
  - errors.OIDC_PROVIDER_NOT_FOUND
  - errors.OIDC_PROVIDER_INACTIVE
  - errors.OIDC_SSRF_BLOCKED
  - errors.UNAUTHORIZED
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

Registration succeeds even when the IdP's discovery / JWKS endpoints are unreachable — cyoda warms them asynchronously after the response. Tokens issued by an un-warmed provider fail with `401 UNAUTHORIZED` until the next warmup cycle (or an explicit `/reload`). See **DIAGNOSTICS** below for the operator path.

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
- The configured `rolesClaim` (per-provider override or `CYODA_OIDC_ROLES_CLAIM`) is looked up as a literal **top-level** JWT claim name. The value at that key may be any of:
  1. **JSON array of strings** — `["admin","warehouse"]` → used as-is.
  2. **JSON object** — `{ "admin": {…}, "warehouse": {…} }` → roles are the **top-level keys** (`["admin","warehouse"]`); inner values are ignored. This is the shape Zitadel emits for `urn:zitadel:iam:org:project:roles` with `projectRoleAssertion=true`.
  3. **String** — `"admin warehouse"` → split on whitespace per RFC 6749 §3.3 / RFC 8693 §4.2; a lone token `"admin"` yields one role.

  Empty / absent / non-collection scalar (number, bool) → no roles, no error (the user is authenticated but unprivileged). Role names containing a comma are dropped silently (cyoda comma-joins roles for downstream serialisation; no major IdP emits commas in role names by convention).

  Common per-IdP values for `rolesClaim`:

```text
  IdP                              rolesClaim value
  -------------------------------  -----------------------------------------------
  Default (no override)            roles
  Amazon Cognito                   cognito:groups
  Zitadel (project-wide)           urn:zitadel:iam:org:project:roles
  Zitadel (per-project)            urn:zitadel:iam:org:project:<projectId>:roles
  Auth0 (namespaced custom claim)  https://your-app.example.com/roles
```

  Note: there is no path/dot-walking syntax — whatever string you configure is used as a single literal claim-key lookup against the JWT, so colons, slashes, and dots inside the value are treated as part of the key name (which is exactly what Zitadel, Cognito, and Auth0 need).

  Keycloak's default `realm_access.roles` lives one level deep inside the `realm_access` object, which the no-dot-walking rule means cyoda cannot reach. Configure a Keycloak token mapper (hardcoded-claim or role-list, at realm or client scope) to flatten the role list to a top-level `roles` claim, then point `rolesClaim` at that top-level key.

Cyoda does not re-mint federated tokens — they are validated and trusted directly.

## DIAGNOSTICS

**Every token-validation failure surfaces as `401 UNAUTHORIZED` with a uniform problem-detail body. No precise error code distinguishes audience mismatch, expired token, unknown `kid`, JWKS-unreachable, claims-invalid, or ambiguous-tenant routing.** A precise wire code would enumerate IdP / tenant / kid / claim-shape recognition to an unauthenticated caller, so the wire stays uniform by design.

The diagnostic path for every failure mode is **server-side logs**, not the HTTP response. Look at `oidc.registry` (KID resolution), `oidc.validator` (claims + signature), and `oidc.discovery` / `oidc.jwks` (transport) events. The bearer-auth middleware emits one structured WARN per failed request with the failure reason slug.

Symptom → cause map for the common cases:

- **"I get 401 but my token looks valid":** check `oidc.registry` resolve logs. The most common cause is the provider's JWKS hasn't warmed yet (registration succeeds before discovery completes); force-warm via `/oauth/oidc/providers/reload`.
- **"After re-registering, valid tokens are rejected":** the second-most-common case — two tenants registered the same IdP with overlapping or empty `expectedAudiences`. Internally `ErrAmbiguousProvider` (`internal/auth/oidc/registry.go`) wraps to `ErrUnknownKID`. To resolve, pick disjoint `expectedAudiences` per tenant.
- **"Tokens reject for the right tenant but the IdP is up":** verify the JWT `iss` claim matches either an explicit `issuers` entry or the discovery document's `issuer` byte-for-byte. Trailing slashes count.

Registration-time failures (these DO carry precise codes — see ERRORS) are SSRF, duplicate URI per tenant, malformed tenant UUID, and provider-not-found / invalid-state for lifecycle ops. Registration succeeds even when the IdP itself is unreachable — discovery failures don't fail the response; they delay the warmup.

## ERRORS

Registration / lifecycle (precise codes — admin-facing surface):

- `errors.OIDC_INVALID_TENANT` (`400`) — caller's tenant ID is not UUID-shaped (commonly: bootstrap `default-tenant` literal).
- `errors.OIDC_SSRF_BLOCKED` (`400`) — `wellKnownConfigUri` resolves to a blocked address range (set `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true` for dev).
- `errors.OIDC_PROVIDER_DUPLICATE` (`409`) — same `wellKnownConfigUri` already registered for this tenant.
- `errors.OIDC_PROVIDER_NOT_FOUND` (`404`) — referenced provider ID absent in this tenant.
- `errors.OIDC_PROVIDER_INACTIVE` (`409`) — update or operation attempted on an invalidated provider; reactivate first.

Token validation (always opaque — see DIAGNOSTICS):

- `errors.UNAUTHORIZED` (`401`) — every token-validation failure path. The wire body never distinguishes audience mismatch, expired token, unknown `kid`, JWKS-unreachable, claims-invalid, or ambiguous-tenant routing.

## SEE ALSO

- `auth.tokens` — universal claim contract
- `config.auth` — `CYODA_OIDC_*` env vars
- `openapi` — `cyoda help openapi tags` and look for `OAuth, OIDC Providers`
