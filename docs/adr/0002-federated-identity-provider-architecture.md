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
guards**: tokens with `iat` predating the provider's `CreatedAt` are
rejected, and `iss` validation is mandatory at resolution time
(`provider.Issuers` if set, else `DiscoveryDoc.Issuer` exact match).
The token's own `tid`/`tenant`/`org` claims are ignored.

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
(transient JWKS-fetch failure → 503). The existing `JWKSValidator`
joins the chain unchanged in behaviour: today's untyped
"untrusted token issuer" becomes `ErrIssuerMismatch`, today's
untyped "failed to resolve key for kid" becomes `ErrUnknownKID`. The
new `oidc.OIDCValidator` returns the same sentinels.

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
- **Broadcast failure semantics diverge from cyoda-cloud.** The SPI's
  `ClusterBroadcaster.Broadcast` is fire-and-forget; cyoda-cloud's
  `notifyAllNodes(awaitResult=true)` ack semantics are not portable.
  A `Broadcast` error from the SPI surfaces as HTTP 500 + ticket
  UUID on the management call. Receiver-side broadcast handlers are
  bounded per-op single-flight with `defer recover()` so a panic in
  the OIDC handler cannot kill delivery of other subsystems'
  invalidations on the same memberlist node.

### New risks

- **Provider-ownership transitions are security-relevant events.**
  When tenant A deletes a provider and tenant B registers the same
  `wellKnownConfigUri`, the `iat`-binding guard rejects A's
  pre-deletion tokens — but only if their `iat` claim is honest. An
  attacker who can forge `iat` defeats the guard. Mitigation
  follow-up: log the ownership transition at INFO with provider
  UUID + tenant transition; operators monitor for unexpected
  transitions.
- **The chain's order is semantically meaningful.** `JWKSValidator`
  runs before `OIDCValidator`. A misconfiguration that put the OIDC
  validator first would cause first-party tokens with a known `kid`
  to be checked against the OIDC registry first, returning
  `ErrUnknownKID` (cheap), then the JWKS validator (cheap). No
  correctness bug under the four-sentinel rule, but unnecessary work.
  An admin introspection endpoint (`GET /api/admin/validators`)
  surfaces the chain order at runtime for debugging.
- **`kid`-namespace collisions across tenants.** Two tenants registering
  IdPs that share signing infrastructure (e.g., both AWS Cognito in
  `us-east-1`) can publish overlapping `kid` values. The `iss`-
  mandatory routing guard (D1) closes the correctness gap; the self-
  healing kidIndex closes the cache-poisoning gap. Both are mandatory,
  not optional.

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
The SPI's fire-and-forget contract is the contract; the parity test
is rewritten to match.

### Single error sentinel (`ErrUnknownIssuer`)

rev. 1 of the spec defined one sentinel that the chain interpreted
as "fall through". **Rejected during fresh-context review.** Today
`JWKSValidator` verifies signature **before** the iss-check, so the
"untrusted token issuer" error covers two materially different cases:
(a) kid was resolvable and signature was first-party but iss is
wrong, (b) kid was unresolvable. Conflating these makes
(a) — which is unambiguously iss-mismatch and must hard-fail — a
chain-fall-through case, creating a brittle escalation path. Four
sentinels make the cases mechanically distinct.

### `KeyValueStore.PutBatch` SPI extension for atomic (index, blob) writes

Would close the rev. 2 D11 register-race window at the SPI level.
**Rejected** by the project lead during review: no SPI changes in
this PR. The read-after-write index validation in D11 is sufficient
for correctness; the write-race remains observable but harmless.
