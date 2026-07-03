# 0003. OpenAPI Contract Conformance & Evolution

**Status:** Accepted
**Date:** 2026-07-03
**Supersedes:** 0001 (OpenAPI Server-Spec Conformance Approach)

## Context

cyoda-go publishes a single, hand-authored OpenAPI 3.1 contract
(`api/openapi.yaml`) — embedded in the binary and served at `/openapi.json` — as
the authoritative definition of its HTTP API. Every HTTP response is validated
against that contract at the end-to-end test boundary, so a response whose shape
or status code diverges from the spec fails the test suite. This conformance
posture was established by ADR 0001.

Two developments have since changed the constraints under which that posture was
chosen:

- **Consumers now generate clients directly from the published contract** — both
  code generators and LLMs read `api/openapi.yaml` and produce clients from it.
  When a shape is under-specified they reach wrong conclusions (for example,
  reading `GET /entity/{entityId}` and concluding there is no way to learn an
  entity's model, because the `meta` object is declared as an opaque bag).
- **cyoda-go leads the contract and Cyoda Cloud conforms to it.** Contract
  fidelity between the two implementations is now a first-class concern rather
  than an informal shared-document convention.

An audit of the contract (`docs/analysis/openapi/README.md`) found roughly 55
spec-vs-server divergences, including about 61 objects modelled as unspecified
"loose bags" (`type: object` with no properties, or `additionalProperties: true`
with no enumerated fields). A dedicated research pass
(`docs/analysis/openapi/schema-strictness-research.md`, whose claims were
adversarially verified against the Zalando RESTful API Guidelines, Google API
Improvement Proposals, Stripe, RFC 9457, protobuf, and JSON Schema 2020-12)
established the open-vs-closed schema policy recorded below.

The central tension this ADR resolves: validating responses strictly with
`additionalProperties: false` conflicts with additive forward-compatibility —
sealing a schema makes "add a field" or "add an event-type variant" a breaking
change — while modelling shapes as loose bags hides the structure that
consumers need. The team's original motive for loose bags was extensibility:
not wanting to revise a schema every time a new aspect (such as a new
state-machine event type) is added.

## Decision

**1. Validate responses at runtime; do not adopt compile-time strict typing.**
The service validates each HTTP response against its operation's schema in
`api/openapi.yaml` at the end-to-end test boundary (using the `kin-openapi`
response validator in enforcing mode); a mismatch fails the test naming the
operation and the offending JSON path. Handlers remain free-form JSON writers
and are not generated from the spec, and the service does not produce
strict-typed server stubs. This keeps validation of genuinely polymorphic
payloads possible (a value strict code generation cannot express) and keeps
handler code decoupled from generated wire types.

**2. Model response schemas typed-but-OPEN.** Enumerate every property the
service knows it emits, but do not seal the object — leave `additionalProperties`
absent (or `true`), never `false`. Enumerating properties gives code generators
and LLMs the true shape; leaving the object open keeps a later field addition
additive and non-breaking, and such additions still pass runtime validation.
(Empirically confirmed against the pinned validator: an object with an
un-declared extra member validates against an open schema and fails against
`additionalProperties: false`.) If a schema composed with `allOf` must ever be
closed, close it with `unevaluatedProperties: false` at the composition point,
never with `additionalProperties: false` on the base — the latter cannot see
into `allOf` branches and would reject a child's own declared fields.

**3. Choose strictness by data direction.** For request bodies, the service may
reject unknown fields on the portion of the envelope it owns (responding
`400`), giving callers early feedback; the user-supplied document region of a
request (an arbitrary entity payload) stays open. For responses, schemas stay
open for evolution, and the obligation to ignore unrecognised fields rests on
the consumer. That consumer obligation is recorded as a contract expectation for
the conforming implementation, not enforced by sealing this service's schemas.

**4. Evolve value sets and unions additively.** Model a frequently-changing set
of values (event types, workflow states) as a documented open value set — an
open string annotated with its currently-known values — rather than a closed
response `enum`, so a new value does not break consumers pinned to the old set.
Model a discriminated union so that it carries an explicit unknown/default
variant, so a new variant is an additive change and consumers degrade gracefully
instead of failing to match.

**5. Keep error objects open.** Problem-details error responses (RFC 9457) are
an open content model by that standard: responses may carry problem-specific
extension members and consumers must ignore members they do not recognise.
Error schemas therefore enumerate their known members but are not sealed.

**6. Enforce additive-only evolution with a CI gate.** A breaking-change
detector (for example, `oasdiff`) runs against `api/openapi.yaml` as a merge
gate and flags breaking edits — sealing a previously-open object, removing a
field, narrowing a type, or adding a required field. Precision is thus maintained
by tooling that catches regressions, rather than by sealing schemas.

**7. Reconcile drift by direction, per finding.** The authored `api/openapi.yaml`
is the canonical contract and the source of intent. When the spec and the server
disagree, the divergence is a defect on whichever side departed from the
contract, classified per finding as: spec-stale (server is right, fix the spec),
spec-incomplete (server is right, enrich the spec), server-gap (the spec states
intended behaviour the server never implemented, fix the server), or
needs-decision (record the decision, then it becomes one of the former). Some
existing code comments in the entity domain assert that "the server is the source
of truth"; those originate from an earlier defect-reconciliation effort (issue
#21) in which the server's behaviour happened to be the intended one, and they
are not a general rule — this ADR replaces that framing with the per-finding
classification above.

## Consequences

**Positive.** Consumers — code generators, LLMs, and the conforming Cloud
implementation — receive the true shape of every object the service controls,
while the service keeps full freedom to add fields and event variants without a
version bump or a breaking change. The loose-bag anti-pattern is eliminated for
objects whose shape is known, without reintroducing the brittleness of sealed
schemas. The CI gate surfaces breaking edits before they reach the conforming
implementation.

**Negative / cost.** The project commits to maintaining a breaking-change gate
and to enumerating properties previously dumped into bags. Response-shape
conformance is a test-time guarantee: a wrong shape is caught at the next
end-to-end run, not at compile time — acceptable given a broad end-to-end suite
across all storage backends, and weaker only on paths that suite does not cover.
Correct handling of unrecognised fields on the consumer side is a contract
expectation this repository cannot enforce directly; it is recorded for the
conforming implementation and handed off.

**Neutral.** The served artefact, the help subsystem, and handler code shape are
unchanged; only schema *content* and the addition of a CI gate change.

## Supersession of ADR 0001

ADR 0001 decided to validate response shapes at runtime with `kin-openapi` at
the end-to-end boundary and to defer compile-time strict typing, on the reasoning
that there were then no consumers generating clients from the contract and that
the contract was a shared document rather than a led specification. ADR 0001
listed the conditions under which that reasoning should be revisited — consumers
generating clients from the spec, and contract parity becoming a first-class
argument — and stated that revisiting requires a superseding ADR.

Both conditions have now occurred (see Context). This ADR therefore supersedes
ADR 0001. It carries ADR 0001's runtime-validation mechanism forward unchanged
(Decision 1, stated in full above so this document stands alone) and adds the
schema-authoring and evolution policy (Decisions 2–7). ADR 0001's status is set
to "Superseded by 0003."

Supersession — rather than leaving ADR 0001 in force and adding a separate,
parallel ADR — is used because ADR 0001 itself requires a superseding ADR on
revisit, and because the registry's status vocabulary expresses this
relationship directly ("Superseded by NNNN") whereas it has no "amended-by" or
"extended-by" status.

## Alternatives considered

- **Seal schemas with `additionalProperties: false` for precision.** Rejected:
  sealing converts every additive change into a breaking one, and the surveyed
  guidance is unanimous that object schemas should be left open for extension by
  default. It would trade the loose-bag problem for a brittleness problem and
  make routine field additions breaking.
- **Generate strict-typed server stubs from the spec (compile-time
  conformance).** Not adopted: the typed-but-open policy plus the CI gate
  delivers the shape precision this option is wanted for, without a large
  generator migration, and without giving up runtime validation of genuinely
  polymorphic payloads (which strict code generation must model as opaque).
- **Keep loose bags for extensibility.** Rejected: this was the original motive,
  but loose bags destroy the shape information consumers need; documented
  open-value-sets and additive unions satisfy the same extensibility goal while
  preserving shape.
