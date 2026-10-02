# Platform operator — Cloud twin-alignment spec

cyoda-go **defines** the contract; Cyoda Cloud aligns to it.

## What cyoda-go does

- **Five OpenAPI operations need a platform operator, not a tenant admin.**
  The five key-pair operations (`issueJwtKeyPair`, `getCurrentJwtKeyPair`,
  `invalidateJwtKeyPair`, `reactivateJwtKeyPair`, `deleteJwtKeyPair`) require
  `ROLE_ADMIN` in the tenant (legal entity) `PLATFORM`. An admin of any other
  tenant gets `403 FORBIDDEN`, with the detail "platform operator required". A
  request without a token gets `401`.
- **An on-behalf-of token is never a platform operator.** The operator rule,
  and the tenant-admin rule on clients and trusted keys, refuse a token from
  the token exchange with `403 FORBIDDEN` ("on-behalf-of tokens cannot
  administer") whatever its roles. No on-behalf-of client can be created in
  `PLATFORM`, and the token exchange carries the client's roles, never the
  assertion's (see `obo-only-user-identity.md`).
- **`/admin/log-level` and `/admin/trace-sampler` are cyoda-go only.** They
  are runtime controls for a single cyoda-go node's process (log level,
  trace sampler); Cloud has no such endpoints and needs none. They are not
  part of the OpenAPI spec.
- **The rule is a tenant check, not a role check.** `ROLE_ADMIN` in
  `PLATFORM` passes; `ROLE_ADMIN` in any other tenant, including a tenant
  spelled `platform` in another case, is refused.

## Prerequisites Cloud does not meet today

The rule depends on tenant binding. In cyoda-go the only token source other
than its own signing key is `POST /oauth/token`, and both grants take the
tenant from the stored client, so a tenant admin can never mint a principal
in `PLATFORM`. Cloud does not bind tenants this way today — it takes the
legal entity of any JWT, including one from an external OIDC provider or a
trusted key, from its `caas_org_id` claim
(`backend/.../iam/integration/AbstractCaasOidcComponents.kt:47-54,158-168`).
These two changes are **prerequisites** for adopting the rule, not follow-ups:

1. The legal entity of every token must come from the stored client (or,
   for a user assertion, from the trusted key's owner, which must equal the
   client's), never from a claim the token's issuer wrote. Retiring the
   external providers and bearer trusted-key tokens
   (`obo-only-user-identity.md`) removes the paths that take it from a claim.
2. `PLATFORM` and `SYSTEM` must not be claimable by any token but the
   platform's own. In Cloud, `SYSTEM` also skips entitlement enforcement
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

1. Meet the two prerequisites above before adopting the rule: bind every
   token's legal entity to its stored client, and make `PLATFORM` and
   `SYSTEM` unclaimable by any token but the platform's own.
2. Require `ROLE_ADMIN` in the tenant `PLATFORM` on the five operator
   operations, and answer `403` for a tenant admin — not the `401` that
   `TdbRestControllerAdvice` answers for a role denial today, which
   disagrees with Cloud's own OpenAPI (this fix is general, not limited to
   the five operator operations).
3. Refuse an on-behalf-of token on every operator and tenant-admin
   operation.
4. Fix the key-pair lookups to be tenant-scoped, so they cannot reach another
   tenant's trusted keys or OIDC keys by `keyId` alone.
5. Stop granting `SUPER_USER` to every Auth0 user; it carries no
   platform-level right in this contract.
6. Retire the OIDC provider endpoints (`obo-only-user-identity.md`), which
   also ends the unscoped-provider finding above.
