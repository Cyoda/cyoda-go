# Platform operator — Cloud twin-alignment spec

cyoda-go **defines** the contract; Cyoda Cloud aligns to it.

## What cyoda-go does

- **Six OpenAPI operations need a platform operator, not a tenant admin.**
  The five key-pair operations (`issueJwtKeyPair`, `getCurrentJwtKeyPair`,
  `invalidateJwtKeyPair`, `reactivateJwtKeyPair`, `deleteJwtKeyPair`) and OIDC
  reload (`reloadOidcProviders`) require `ROLE_ADMIN` in the tenant (legal
  entity) `PLATFORM`. An admin of any other tenant gets `403 FORBIDDEN`, with
  the detail "platform operator required". No token gets `401`.
- **`/admin/log-level` and `/admin/trace-sampler` are cyoda-go only.** They
  are runtime controls for a single cyoda-go node's process (log level,
  trace sampler); Cloud has no such endpoints and needs none. They are not
  part of the OpenAPI spec.
- **The rule is a tenant check, not a role check.** `ROLE_ADMIN` in
  `PLATFORM` passes; `ROLE_ADMIN` in any other tenant, including a tenant
  spelled `platform` in another case, is refused.

## Prerequisites Cloud does not meet today

The rule depends on tenant binding: every token source other than cyoda-go's
own signing key fixes the token's tenant to the tenant that set the source
up, so a tenant admin can never mint a principal in `PLATFORM`. Cloud does
not bind tenants this way today — it takes the legal entity of any JWT,
including one from an external OIDC provider or a trusted key, from its
`caas_org_id` claim
(`backend/.../iam/integration/AbstractCaasOidcComponents.kt:47-54,158-168`).
These two changes are **prerequisites** for adopting the rule, not follow-ups:

1. The legal entity of an externally issued token must come from the
   provider's or key's owner, not from a claim in the token.
2. `PLATFORM` and `SYSTEM` must not be claimable by an external token. In
   Cloud, `SYSTEM` also skips entitlement enforcement
   (`entitlements-common/.../CyodaEntitlements.kt:72-73`).

## Found in Cloud during this work

Found while tracing the equivalent Cloud code paths for this rule, filed
under the same ticket:

- The key-pair invalidate / reactivate / delete endpoints look a key up by
  `keyId` only
  (`platform-service-iam/.../jwk/StoredJWKService.kt:587-596`), so they also
  reach other tenants' trusted keys and OIDC keys.
- A denied role answers `401`, not `403`
  (`tree-node/tree-node-api/.../TdbRestControllerAdvice.kt:41-58`), while
  Cloud's own OpenAPI documents `403`.
- `SUPER_USER` gives no platform-level right in the contract, but Cloud's
  Auth0 login action adds it to every user
  (`scripts/auth0/action-setup-cyoda-token-claims.js:22`).
- OIDC providers are not scoped to a tenant
  (`platform-service-iam/.../oidc/JWKOIDCService.kt:188-200`).

## Cloud action

Tracked in CP-3981.

1. Meet the two prerequisites above before adopting the rule: bind an
   externally issued token's legal entity to its provider's or key's owner,
   and make `PLATFORM` and `SYSTEM` unclaimable by an external token.
2. Require `ROLE_ADMIN` in the tenant `PLATFORM` on the six operator
   operations, and answer `403` for a tenant admin — not the `401` that
   `TdbRestControllerAdvice` answers for a role denial today, which
   disagrees with Cloud's own OpenAPI (this fix is general, not limited to
   the six operator operations).
3. Fix the key-pair lookups to be tenant-scoped, so they cannot reach another
   tenant's trusted keys or OIDC keys by `keyId` alone.
4. Stop granting `SUPER_USER` to every Auth0 user; it carries no
   platform-level right in this contract.
5. Scope OIDC providers to a tenant.
