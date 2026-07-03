# 0003. OpenAPI Contract Conformance & Evolution

**Status:** Proposed
**Date:** 2026-07-03
**Supersedes:** 0001 (OpenAPI Server-Spec Conformance Approach)

## Context

ADR 0001 adopted runtime response-shape validation of `api/openapi.yaml` at the E2E
boundary (kin-openapi, enforce mode) and deferred compile-time strict typing. It listed
reconsideration triggers requiring a superseding ADR. Two have now fired:

- **Consumers that generate clients from the published spec now exist** — codegen clients
  and LLMs that read `api/openapi.yaml` and reach materially wrong conclusions when shapes
  are under-specified (e.g. concluding from `GET /entity/{entityId}` that there is no way
  to learn the entity model, because `meta` is an opaque bag).
- **cyoda-go now leads the contract and Cyoda Cloud conforms to it** (Gate 7); contract
  parity is first-class, no longer a "spec-as-shared-document" posture.

A full audit (`docs/analysis/openapi/README.md`) found ~55 spec-vs-server divergences,
including ~61 objects modelled as unspecified "loose bags." A dedicated research pass
(`docs/analysis/openapi/schema-strictness-research.md`, 24/25 claims adversarially
verified against Zalando, Google AIP, Stripe, RFC 9457, protobuf, JSON Schema 2020-12)
established how to model open vs closed schemas. This ADR records the resulting policy.

The core tension: strict validation with `additionalProperties: false` conflicts with
additive forward-compatibility (sealing turns "add a field / add an event type" into a
breaking change), while leaving schemas as loose bags hides shape from consumers.

## Decision

**1. Retain runtime response validation; continue to defer compile-time strict typing.**
ADR 0001's mechanism stands: kin-openapi enforce-mode validation at the E2E boundary,
loose `WriteJSON`/`WriteError` handlers, no generated strict-server migration. Typed-but-open
(below) achieves shape precision without that migration.

**2. Model response schemas typed-but-OPEN.** Enumerate every known property, but do **not**
seal — `additionalProperties` stays absent/`true`, never `false`. This gives codegen/LLMs
the real shape while additive fields remain non-breaking and still validate under enforce
mode (empirically confirmed for kin-openapi v0.140.0). If a composed (`allOf`) schema ever
must be closed, use `unevaluatedProperties: false` at the composition point — never
`additionalProperties: false` on the base (it cannot see into `allOf` branches).

**3. Choose strictness by direction.** Requests the service owns may reject unknown *input*
fields (HTTP 400) on the service-controlled envelope; the user-supplied document region of
a request stays open. Responses stay open for evolution; the must-ignore-unknown
(tolerant-reader) obligation sits on the **consumer** (Cloud), documented as a contract
expectation, not enforced by sealing cyoda-go's schema.

**4. Evolve value sets and unions additively.** Frequently-changing enumerations (event
types, states) are modelled as documented open value sets (open string with documented
examples / "[Extensible enum]"), not closed response enums. Discriminated unions carry an
explicit unknown/default variant so new variants are additive and consumers degrade
gracefully.

**5. RFC 9457 problem-details are open by standard.** Extension members are permitted and
consumers must ignore unrecognised ones; problem-detail schemas must not be sealed.

**6. Enforce additive-only evolution with a CI gate.** A breaking-change detector
(oasdiff or equivalent) runs on `api/openapi.yaml` as a merge gate — catching sealing,
field removal, type narrowing, and required-field additions — so precision is maintained
by tooling rather than by a sealed schema.

**7. Reconciliation direction.** The authored `api/openapi.yaml` is the canonical contract
and source of intent. When spec and server disagree, the defect is on whichever side left
the contract, decided per finding (spec-stale / spec-incomplete / server-gap /
needs-decision). This refines the #21 seed's "server is source of truth" framing, which was
correct only for the specific #21 defects.

## Consequences

**Positive.** Consumers (codegen, LLMs, Cloud) get the true shape of every service-owned
object while cyoda-go retains full freedom to add fields and event types without a version
bump or a breaking change. The loose-bag anti-pattern is eliminated for known-shape objects
without reintroducing the brittleness of sealed schemas. The CI gate makes breaking edits
visible before they reach the conforming Cloud implementation.

**Negative / cost.** Commits the project to maintaining a breaking-change gate and to
enumerating properties that were previously dumped into bags. Response validation remains a
runtime (test-time) guarantee, inherited from ADR 0001. Tolerant-reader correctness on the
Cloud side is a contract expectation this repo cannot enforce directly — it is recorded in
`docs/cloud-parity/` and handed off.

**Neutral.** No change to the served artefact shape, the help subsystem, or codegen mode.
Only schema *content* and the addition of a CI gate change.

**Supersession.** ADR 0001 is marked *Superseded by 0003*. Its runtime-validation decision
is carried forward here verbatim; only the surrounding context and the schema-authoring /
evolution policy are new.

## Alternatives considered

- **Seal schemas with `additionalProperties: false` for precision.** Rejected: the research
  is unanimous (Zalando Rule #111 mandates *not* sealing; JSON Schema is open-by-default)
  that sealing converts every additive change into a breaking one. It would trade the
  loose-bag problem for a brittleness problem.
- **Migrate to compile-time strict typing** (ADR 0001's deferred option A/B). Still
  deferred: typed-but-open + the CI gate delivers the shape precision the strict-typing
  migration was wanted for, without its multi-week cost or its inability to validate
  genuinely polymorphic payloads.
- **Keep loose bags for extensibility.** Rejected: it was the original motive, but it
  destroys shape information for consumers; open-enum/additive-union techniques satisfy the
  same extensibility goal while preserving shape.
- **Complement ADR 0001 without superseding.** Rejected: ADR 0001 explicitly states that
  revisiting (which its fired triggers now require) is done via a superseding ADR, and the
  registry status vocabulary supports supersession, not "extended by."
