# 0002. Federated Identity Provider Architecture

**Status:** Accepted
**Date:** 2026-06-16

## Context

cyoda-go must accept JWTs issued by external OpenID Connect providers
alongside its own first-party tokens. The OpenAPI surface for OIDC
provider management has existed at `/oauth/oidc/providers/*` since v0.6,
but every handler returns HTTP 501 — there is no implementation. Issue
#284 lands the subsystem as the v0.8.0 milestone's headline IAM
deliverable.

Three load-bearing decisions shape the subsystem and bind future
federated-identity work (e.g., SAML, federated dev tokens, social
login). They are decided together because they are coupled: the
tenancy model determines the validator-side iteration strategy;
the validator-composition shape determines how cleanly future
non-OIDC validators slot in; the persistence shape determines per-
plugin work and the surface area of failure modes.

The cyoda-cloud reference resolves the same axes differently — system-
wide registry, in-place validator extension, a typed entity in a
dedicated column family (`CYODA_LE`). cyoda-cloud's choices are
defensible inside its single-tenant-SaaS-with-cluster-perimeter
deployment model; cyoda-go-light runs in heterogeneous contexts
(single-node dev, on-prem multi-tenant, embedded) where the trade-
offs differ. The decision is what shape best fits cyoda-go's
deployment envelope without painting future work into a corner.

## Decision

Decision labels match the spec (`2026-06-16-284-oidc-providers-design.md` §2) so cross-references read naturally.

### D1. Per-tenant provider registry

OIDC providers are owned by a legal entity. Management API (`POST /oauth/oidc/providers` etc.) scopes by `tenantFromCtx(r)`; KV storage is namespaced per tenant. Token validation iterates providers across all tenants (tokens carry no tenant claim); the **resolving provider's `OwnerLegalEntityID` determines the user context's tenant**, not any claim in the JWT.

### D2. KeyValueStore-backed persistence

The provider registry uses the generic `spi.KeyValueStore` SPI (namespace `oidc:providers:<tenantID>`; secondary uniqueness index `oidc:uri-idx:<tenantID>` keyed by SHA-256 of the well-known URI). The pattern is the existing `KVTrustedKeyStore`. A small SPI extension — `KeyValueStore.ListNamespaces(prefix) ([]string, error)` — supports startup-time tenant enumeration.

### D3. Chained validator composition

A new `auth.Validator` interface (`Validate(token) (*spi.UserContext, error)`) replaces the implicit single-validator contract. `auth.ChainedValidator` wraps a slice of validators and falls through on a typed `auth.ErrUnknownIssuer` sentinel. The existing `JWKSValidator` stays single-issuer and joins the chain unchanged (one error-rename only). The new `oidc.OIDCValidator` is a second chain entry that consults the per-tenant registry.

## Consequences

### Easier

- **Adding future federated validators (SAML, dev tokens, social).** The chain pattern accepts any `auth.Validator` implementation; OIDC is not a special case.
- **Per-tenant operational autonomy.** A legal entity can register, invalidate, and reactivate its own providers without involving a system administrator. Closes the door on tenant-A discovering that tenant-B's provider misconfiguration broke shared validation.
- **Plugin parity.** OIDC ships across memory, sqlite, postgres, and cassandra simultaneously because the persistence path is the generic KV SPI. No per-plugin work beyond the `ListNamespaces` implementation each plugin already wants for other reasons.
- **Reasoning about token validation.** Each validator owns its concerns: `JWKSValidator` is first-party only, `OIDCValidator` is federated only. Lifecycle bookkeeping (active/invalidated) lives where it logically belongs.

### Harder

- **Token validation is `O(active providers)` worst case.** Mitigated by a `kid`-indexed fast path inside the registry making the steady state `O(1)` average. The cold path remains a linear scan when an unseen `kid` first appears; this is a correctness requirement (a freshly rotated key has no index entry yet).
- **The (index, blob) write pair on `Register` is best-effort transactional.** A crash between the index `Put` and the blob `Put` leaves an orphan index entry. Detected and cleaned up on the next collision check or `LoadAll` warmup; the race window is microseconds wide. A future `KeyValueStore.PutBatch` SPI extension would close this window for all KV-backed subsystems but is out of scope for v0.8.0.
- **Broadcast failure semantics diverge from cyoda-cloud.** The SPI's `ClusterBroadcaster.Broadcast` is fire-and-forget; cyoda-cloud's `notifyAllNodes(awaitResult=true)` ack semantics are not faithfully portable. A `Broadcast` error from the SPI surfaces as HTTP 500 + ticket UUID on the management call; the cluster-intercom-failure parity test is rewritten against this contract.

### New risks

- **Tenant binding via provider ownership creates a token-routing invariant.** A bug in `OIDCValidator` that pulls the tenant from a JWT claim instead of the resolving provider's `OwnerLegalEntityID` is a cross-tenant escalation. Mitigated by a cyoda-go-specific parity test asserting that a `tid` claim in the token cannot override the provider's `OwnerLegalEntityID`. The test is mandatory before any change to the validator hot path.
- **SSRF surface.** Per-tenant management means a tenant admin can point a `wellKnownConfigUri` at internal addresses (AWS metadata, link-local, RFC1918). Mitigated by a default-on register-time DNS resolution + CIDR block list (`CYODA_OIDC_ALLOW_PRIVATE_NETWORKS=true` overrides for test/dev). cyoda-cloud relies on its deployment perimeter for the same defence; cyoda-go-light cannot assume one.
- **The chain's order is semantically meaningful.** `JWKSValidator` runs before `OIDCValidator`. A misconfiguration that put the OIDC validator first would cause first-party tokens with a known `kid` to be checked against the OIDC registry first, returning `ErrUnknownIssuer` (cheap), then the JWKS validator (cheap). No correctness bug, but unnecessary work. Order is fixed at construction in `app/app.go` and exercised by an E2E smoke test.

### Neutral

- **The `Validator` interface is `cyoda-go`-internal.** It is not part of the SPI; out-of-tree plugins cannot supply alternative validators. This is intentional for v0.8.0 — the chain composition is a deployment concern, not a plugin concern.
- **`OidcProvider.ProviderID`** is always `"USER_EXTERNAL"` in v0.8.0. The field is in the persisted struct for forward-compat with system-defined provider types and costs nothing today.

## Alternatives considered

### System-wide registry (cyoda-cloud parity)

One global provider list managed by ROLE_ADMIN, no `OwnerLegalEntityID`, tokens validate against any registered provider. Faithful to cyoda-cloud. **Rejected** because cyoda-go-light's per-tenant management API is one of its differentiators; a global registry creates a hard-to-fix coupling between unrelated tenants' identity infrastructure ("tenant B's identity team broke my login"). The system-wide model is recoverable from the per-tenant model if we ever want it — the reverse is not.

### In-place extension of `JWKSValidator`

Add an optional `oidcRegistry` field to `JWKSValidator`; consult it when the bound `issuer` doesn't match the token's `iss`. Fewer types. **Rejected** because it mixes first-party and federated validation in one struct, makes adding a third source (SAML) require another optional field, and puts lifecycle bookkeeping in a class that previously had none. The smell is the implicit nil-check on the optional registry.

### `IssuerResolver`-based refactor

Replace `issuer string` with an `IssuerResolver` interface; the static single-issuer case becomes one resolver impl, OIDC is another. Most extensible. **Rejected** as over-engineering for the two-validator world: every existing `JWKSValidator` construction site changes, and the `KeySource + lifecycle` coupling inside a single resolver method conflates concerns the chain pattern keeps separate.

### EntityStore-backed persistence

Model `OidcProvider` as a managed entity (ModelRef, audit trail via `StateMachineAuditStore`, version history via `GetVersionHistory`). Closer to cyoda-cloud's column-family-scoped JPA entity. **Rejected** for v0.8.0 because uniqueness on `wellKnownConfigUri` still needs an out-of-band index (the EntityStore has no unique-constraint primitive), every storage plugin gets a new entity kind to register, and the `KVTrustedKeyStore` precedent already covers the operational shape we need. Audit-trail gain is real but not load-bearing for OIDC management at v0.8.0 scale.

### Per-message broadcast topics

Three topics (`oidc.provider.reload`, `oidc.provider.invalidate`, `oidc.provider.reload_all`) instead of one envelope-carrying topic. Topic name encodes intent. **Rejected** because handlers all live in the same registry anyway, and gossip topic-cardinality is not free. cyoda-cloud's `JWKOIDCCacheNotification` already packs the three operations into one envelope; the cyoda-go choice matches that and our own `modelcache` precedent.

### Synchronous broadcast ack (`BroadcastSync`)

Extend `ClusterBroadcaster` with an acked-broadcast method to faithfully port cyoda-cloud's "broadcaster failure → 500" semantic. **Rejected** per the project-memory principle that the engine doesn't claim consistency rights existing primitives don't already give. The SPI's fire-and-forget contract is the contract; the parity test is rewritten to match.
