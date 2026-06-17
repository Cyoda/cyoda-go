# 0002. Federated Identity Provider Architecture

**Status:** Accepted
**Date:** 2026-06-16

## Context

cyoda-go must accept JWTs issued by external OpenID Connect providers
alongside its own first-party tokens. The OpenAPI surface for OIDC
provider management has existed at `/oauth/oidc/providers/*` since
v0.6, but every handler returns HTTP 501. Issue #284 lands the
subsystem as the v0.8.0 milestone's headline IAM deliverable.

Three load-bearing decisions shape the subsystem and bind future
federated-identity work (SAML, federated dev tokens, social login).
They are decided together because they are coupled: tenancy model
determines validator-side iteration strategy; validator-composition
shape determines how cleanly non-OIDC validators slot in; persistence
shape determines per-plugin work and the surface area of failure
modes.

cyoda-cloud resolves the same axes differently — system-wide
registry, in-place validator extension, a typed entity in a
dedicated column family. Those choices are defensible inside a
single-tenant-SaaS-with-cluster-perimeter deployment model;
cyoda-go-light runs in heterogeneous contexts (single-node dev,
on-prem multi-tenant, embedded) where the trade-offs differ.

## Decision

Decision labels match the implementation spec so cross-references
read naturally.

### D1. Per-tenant provider registry

OIDC providers are owned by a legal entity. Management API scopes by
`tenantFromCtx(r)`; KV records are keyed by `(tenantID, ...)`. Token
validation iterates providers across all tenants — tokens carry no
tenant claim — but the resolving provider's `OwnerLegalEntityID`
determines the user context's tenant, **subject to two routing
guards** and one **honest scope limit**: tokens with `iat` predating
the provider's `CreatedAt` (with 30s clock skew) are rejected, and
`iss` validation is mandatory at resolution time (`provider.Issuers`
if set, else `DiscoveryDoc.Issuer` exact match — or fall through if
the discovery doc has not been fetched yet). These guards close the
**accidental** cross-tenant spillover case (a long-lived legitimate
JWT surviving an ownership handover). They do not close the
**adversarial** case (the IdP operator mints a fresh token with
`iat = now` after observing the handover). The mitigation for the
adversarial case is **D25 ownership-transition audit logging** —
operational observability, not a security property of the validator.
The token's own `tid`/`tenant`/`org` claims are ignored regardless.

### D2. KeyValueStore-backed persistence, single namespace, composite keys

The provider registry uses the generic `spi.KeyValueStore` SPI with
**one namespace `oidc-providers`** and composite keys
(`<tenantID>:<provider-uuid>` for blobs, `<tenantID>:uri:<sha256(uri)>`
for the uniqueness index). The store runs under the system tenant
context, mirroring `KVTrustedKeyStore`. **No SPI changes.** The
read-after-write race window is left observable; correctness is
preserved by re-reading the index after `Put` (loser deletes its
own blob and returns 409) and by stale-index defence on every read
(`Get` verifies the blob exists and its `OwnerLegalEntityID` matches
the index-key tenant prefix).

### D3. Chained validator composition with four distinct error sentinels

A new `auth.Validator` interface (`Validate(token) (*spi.UserContext,
error)`) replaces the implicit single-validator contract.
`auth.ChainedValidator` wraps a slice of validators and falls through
**only** on `auth.ErrUnknownKID`. The other three sentinels —
`ErrIssuerMismatch`, `ErrSignatureFailure`, `ErrClaimsFailure` —
hard-fail without consulting subsequent validators. Plus
`ErrTokenPreTransition` (D1 iat-binding) and `ErrJWKSUnavailable`
(transient JWKS-fetch failure → 503).

The existing `JWKSValidator` already produces the correct hard-fail
behaviour today (`validator.go:82-84` returns "untrusted token
issuer" after signature verification succeeds at line 73). This is
**not behaviour we are adding** — it is behaviour we are
**preserving** under chaining. A single-sentinel design would have
*promoted* the existing hard-fail iss-mismatch case to chain-fall-
through, creating a new escalation path that the current
single-validator code does not have. Four sentinels make the cases
mechanically distinct and preserve the current correctness under
the chain.

## Consequences

### Easier

- **Adding future federated validators (SAML, dev tokens, social).**
  The chain accepts any `auth.Validator`; OIDC is not a special case.
- **Per-tenant operational autonomy.** A legal entity registers,
  invalidates, and reactivates its own providers without involving a
  system administrator. Tenant A's IdP misconfiguration cannot break
  Tenant B's shared validation surface.
- **Plugin parity.** OIDC ships across memory / sqlite / postgres /
  cassandra simultaneously because the persistence path is the
  generic KV SPI with no new ops. The trusted-key precedent is
  battle-tested.
- **Reasoning about token validation.** Four sentinels make the
  cases mechanically distinct: "I don't know this kid" (chain
  fall-through) versus "I know this kid but iss is wrong" (hard
  fail) versus "signature didn't verify" versus "claims are bad".

### Harder

- **Token validation iterates `O(active providers)` in the worst
  case.** Mitigated by a self-healing `kid`-indexed fast path. The
  cold path (kid never seen before, or after rotation) remains a
  linear scan. At ≥10K active providers across all tenants, persisting
  the kid index across restarts becomes worthwhile; v0.8.0 does not.
- **The (index, blob) write pair on `Register` races under load.**
  The window is bounded by the slower node's write latency between
  read and second write — potentially seconds, not microseconds.
  Closed at correctness by re-reading the index after `Put`: the
  loser deletes its own blob and surfaces 409. The window is
  observable but harmless.
- **Broadcast failure cannot be surfaced.** The SPI's
  `ClusterBroadcaster.Broadcast` returns nothing — fire-and-forget by
  contract. The management API ignores delivery state; if broadcast
  enqueueing fails silently, peers re-converge via subsequent
  `reload_all` and via D11 read-time validation. Misconfiguration
  (`broadcaster == nil`) is a startup invariant; `os.Exit(1)` if
  missing. Receiver-side broadcast handlers are single-flight per
  `(tenantID, uri)` with `defer recover()` so a panic in the OIDC
  handler cannot kill delivery of other subsystems' invalidations on
  the same memberlist node.
- **Cold-start window for OIDC tokens.** Phase-2 (async JWKS fetch)
  runs after the listener binds; tokens arriving for a not-yet-warmed
  provider get `ErrUnknownKID` → 401. This is a documented
  operational characteristic — the alternative (block listener on
  JWKS fetch for all tenants) is a startup outage waiting to happen
  at scale.

### New risks

- **Provider-ownership transitions are security-relevant events.**
  D1's `iat`-binding rejects pre-transition tokens by `iat` —
  but `iat` is set by the IdP. An attacker controlling the IdP after
  the ownership transition can mint tokens with current `iat` that
  the validator accepts. The mitigation is D25 INFO-level audit
  logging on every ownership transition (Register-after-Delete-from-
  different-tenant) with `{from_tenant, to_tenant,
  wellknown_uri_hash, new_provider_uuid}` fields. This is operational
  observability, not a security property of the validator. Operators
  must monitor; the spec calls this out honestly rather than claiming
  defence-in-validator.
- **The chain's order is semantically meaningful.** `JWKSValidator`
  runs before `OIDCValidator`. A misconfiguration that put the OIDC
  validator first would cause first-party tokens with a known `kid`
  to be checked against the OIDC registry first, returning
  `ErrUnknownKID` (cheap), then the JWKS validator (cheap). No
  correctness bug under the four-sentinel rule, but unnecessary work.
- **`kid`-namespace collisions across tenants.** Two tenants registering
  IdPs that share signing infrastructure (e.g., both AWS Cognito in
  `us-east-1`) can publish overlapping `kid` values. The `iss`-
  mandatory routing guard (D1) closes the correctness gap; the self-
  healing kidIndex closes the cache-poisoning gap. Both are mandatory,
  not optional.
- **Cross-IdP `sub`-collision.** External IdPs' `sub` values are
  opaque per-IdP and not globally unique. The spec namespaces
  `UserContext.UserID` as `oidc:<providerUUID>:<sub>` to prevent
  collision into a shared UserID space. Per-IdP role-claim names
  (Auth0 / Cognito / Keycloak vary) are honoured via a per-provider
  `RolesClaim` field falling back to a global default.

### Neutral

- **The `Validator` interface is `cyoda-go`-internal.** Not part of
  the SPI; out-of-tree plugins cannot supply alternative validators.
  Intentional for v0.8.0 — chain composition is a deployment concern,
  not a plugin concern.

## Alternatives considered

### System-wide registry (cyoda-cloud parity)

One global provider list managed by ROLE_ADMIN, no `OwnerLegalEntityID`,
tokens validate against any registered provider. Faithful to
cyoda-cloud. **Rejected** because cyoda-go-light's per-tenant
management API is a differentiator; a global registry creates a
hard-to-fix coupling between unrelated tenants' identity
infrastructure. The system-wide model is recoverable from the per-
tenant model if we ever want it — the reverse is not.

### In-place extension of `JWKSValidator`

Add an optional `oidcRegistry` field to `JWKSValidator`; consult it
when the bound `issuer` doesn't match the token's `iss`. Fewer types.
**Rejected** because it mixes first-party and federated validation
in one struct and makes adding a third source require another
optional field. The implicit nil-check on the optional registry is
the smell.

### `IssuerResolver`-based refactor

Replace `issuer string` with an `IssuerResolver` interface; the
static single-issuer case becomes one resolver impl, OIDC is another.
**Rejected** as over-engineering for the two-validator world: every
existing `JWKSValidator` construction site changes, and the
`KeySource + lifecycle` coupling inside a single resolver method
conflates concerns the chain pattern keeps separate.

### EntityStore-backed persistence

Model `OidcProvider` as a managed entity (`ModelRef`, audit trail
via `StateMachineAuditStore`, version history via
`GetVersionHistory`). Closer to cyoda-cloud's column-family-scoped
JPA entity. **Rejected** for v0.8.0 because uniqueness on
`wellKnownConfigUri` still needs an out-of-band index (the EntityStore
has no unique-constraint primitive), every storage plugin gets a new
entity kind to register, and the `KVTrustedKeyStore` precedent already
covers the operational shape we need.

### Per-tenant KV namespaces

Use the namespace dimension of `KeyValueStore` for tenant scoping
(`oidc:providers:<tenantID>` etc.) and add a `ListNamespaces(prefix)`
SPI op for startup enumeration. **Rejected** because `KeyValueStore`
is already tenant-scoped at acquisition (every plugin filters by
`tenant_id` in SQL or partition key); the proposed SPI extension
either returns an empty list when called from a single-tenant store
(honest implementation) or deliberately bypasses isolation. The
single-namespace + composite-key pattern (used by `KVTrustedKeyStore`)
needs no SPI changes and respects the existing isolation contract.

### Three separate broadcast topics

`oidc.provider.reload`, `oidc.provider.invalidate`,
`oidc.provider.reload_all` instead of one envelope-carrying topic.
**Rejected** because handlers all live in the same registry and
gossip topic-cardinality is not free. cyoda-cloud's notification
type packs the same three operations into one envelope; matching
that and `modelcache`'s precedent is consistent.

### Synchronous broadcast ack (`BroadcastSync`)

Extend `ClusterBroadcaster` with an acked-broadcast method to
faithfully port cyoda-cloud's "broadcaster failure → 500" semantic.
**Rejected** per the project-memory principle that the engine does
not claim consistency rights existing primitives do not already give.
The SPI's fire-and-forget contract is the contract; rev. 2 of the
spec briefly tried to handle broadcast failure as a 500 + KV
rollback — that path is mechanically impossible (no failure surface
exists) and the rollback would have been wrong (peers may have
received the broadcast). rev. 3 deletes the path entirely; the
cyoda-cloud "intercom failure" test simply does not port.

### Single error sentinel (`ErrUnknownIssuer`)

rev. 1 of the spec defined one sentinel that the chain interpreted
as "fall through". **Rejected during the first fresh-context review.**
Today `JWKSValidator` verifies signature **before** the iss-check
(`validator.go:73` vs `:82`), so the "untrusted token issuer" error
covers a token whose signature was already first-party-verified —
that is unambiguously iss-mismatch and must hard-fail. A single
sentinel would have promoted this existing hard-fail to chain-fall-
through, creating an escalation path the current single-validator
code does not have. Four sentinels are not new behaviour — they
**preserve** existing behaviour under chaining.

### Cross-op single-flight by `(op, tenantID, uri)`

rev. 2 of the spec keyed the broadcast-handler single-flight by
`(op, tenantID, uri)`. **Rejected during the second fresh-context
review.** A reload and an invalidate for the same provider arrived
under different keys, ran in parallel, and the local end-state
depended on which finished last. Memberlist's unordered gossip then
made the inter-node states diverge. rev. 3 keys by `(tenantID, uri)`
only — all operations for the same provider serialize through one
worker locally. Inter-node convergence is provided by periodic
`reload_all` and by D11 read-time validation.

### Per-tenant Prometheus gauge label

rev. 2 specified `oidc_registry_providers{tenant=...}` for per-tenant
provider counts. **Rejected during the second fresh-context review.**
At 1K+ tenants this is a Prometheus cardinality footgun. rev. 3
aggregates: `oidc_registry_providers` with no tenant label. Per-
tenant counts go via admin API if/when needed, not via Prometheus
labels.

### Admin chain-introspection endpoint (`GET /api/admin/validators`)

rev. 2 added a read-only endpoint dumping the validator chain order
for debugging. **Rejected during the second fresh-context review for
v0.8.0.** The chain is exactly two validators (JWKS + OIDC) in
v0.8.0; composition is invariant per-deployment and known at startup
from logs. No current consumer; deferring is reversible (add when
the third validator lands and there is a debugging need). Per
`feedback_gate6_no_followups`, this is the *correct* deferral — the
decision is "add when there is a use case," not "we'll get to it."

### `KeyValueStore.PutBatch` SPI extension for atomic (index, blob) writes

Would close the D11 register-race window at the SPI level.
**Rejected** by the project lead during the first review: no SPI
changes in this PR. The read-after-write index validation in D11 is
sufficient for correctness; the write-race remains observable but
harmless.
