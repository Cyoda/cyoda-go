# Schema Strictness in a Spec-First OpenAPI Contract — Research & Rubric

**Status:** research reference (input to ADR 0003).
**Date:** 2026-07-03
**Method:** deep-research fan-out (5 angles, 19 sources, 79 claims extracted, 25
adversarially verified 3-vote, 24 confirmed / 1 refuted).

## Question

When should schemas in `api/openapi.yaml` be strictly typed/closed
(`properties` + `additionalProperties: false`, discriminated `oneOf`) versus
intentionally open (`additionalProperties: true`, bare `type: object`)? Constraint:
response shapes are validated at runtime by kin-openapi in enforce mode (ADR 0001)
— fixed. Consumers include codegen clients and LLMs. The team's original motive for
loose bags was extensibility (add an event type / field without a schema-version bump).

## Resolution

Accepted practice resolves the strict-validation-vs-additive-compatibility tension by
**separating responsibilities by direction**, not by picking a global posture:

- **Enumerate every known property, but keep the schema OPEN** (`additionalProperties`
  absent/`true`, never `false`). Enumeration gives codegen/LLMs the real shape (fixes
  the loose-bag problem); leaving it open keeps additive changes non-breaking.
- **The tolerant-reader / must-ignore-unknown obligation sits on the CONSUMER**, not on
  the producer's schema. Additive producer changes (new field, new event type) are
  backward-compatible by contract; consumers must ignore what they don't recognise.
- **Strictness is per-direction:** requests the service owns may reject unknown input
  (HTTP 400); responses the service emits stay open for evolution.
- **Extensibility is better served by typed-but-open + extensible-enums than by loose
  bags** — same freedom to avoid version bumps, but the shape is preserved. Loose bags
  were solving a real problem the wrong way.
- **Enforce additive-only discipline with a CI gate** (oasdiff / Specmatic) instead of a
  sealed schema — the gate catches genuinely breaking edits (sealing, field removal,
  type change) while letting additive ones through.

## The decision rubric

| Category | cyoda-go example | Recommendation |
|---|---|---|
| (a) user/polymorphic request body | `create`/`patch` payload | **Strict envelope** (reject unknown *input* fields → 400) + **open document** region |
| (b) service-owned response envelope / metadata | `Envelope.meta`, `exportMetadata` | **Typed-but-open** — enumerate all fields, do **not** seal |
| (c) evolving discriminated union | state-machine audit events | **Discriminated `oneOf` + explicit unknown/default variant + open-enum discriminator** |
| (d) RFC 9457 problem-details | `ProblemDetail` | **Open by standard** — extension members allowed; sealing is non-conformant |
| (e) genuinely "any JSON" | entity `data`, `JsonNode` | **Open bag is correct** — no fixed shape to convey |

Discriminating question for any object: *does the service know this object's shape?*
If yes → typed-but-open (b/c/d). If genuinely no → open bag (e). "Unspecified" is only
honest for (e); using it for (b)/(c)/(d) is the anti-pattern that hides shape from
consumers.

## Key verified findings (with sources)

1. **`properties` alone does not seal a schema**; `additionalProperties: false` is an
   explicit opt-in, and it is what converts additive changes into breaking ones. Zalando
   Rule #111 *mandates* not declaring `additionalProperties: false` "as this prevents
   objects being extended in the future." (learnjsonschema.com; Zalando compatibility.adoc)
2. **`allOf` composition gotcha:** `additionalProperties` only restricts *sibling*
   `properties` and cannot see into `allOf` branches — so a sealed base extended via
   `allOf` rejects the child's new fields. If a composed schema must be closed, use
   **`unevaluatedProperties: false`** at the composition point, never
   `additionalProperties: false` on the base. (learnjsonschema.com)
3. **Tolerant-reader belongs on the consumer** (Fowler: "only take the elements you need,
   ignore anything you don't"; Zalando Rule #108; RFC 9457 "clients... MUST ignore any
   such extensions that they don't recognize").
4. **Strictness is per-direction:** Zalando Rule #109 rejects unknown *input* fields with
   HTTP 400, while output evolution stays safe via consumer tolerance (#108) and open
   definitions (#111).
5. **Additive evolution within a major version is sanctioned:** Google AIP-180 permits
   adding fields/messages/enums/enum-values; Zalando Rule #107 gives the additive-only
   matrix (inputs add optional-only; outputs may add fields).
6. **Frequently-changing value sets → open string + documented values, not a closed
   enum** (Google AIP-126). Zalando #112 documents known values via `examples` with an
   "[Extensible enum]" prefix; `x-extensible-enum` is its now-deprecated predecessor
   (changelog 2025-11-27).
7. **Stripe (production scale)** classifies "adding new properties to responses" and
   "adding new event types" as backward-compatible, and instructs consumers to "gracefully
   handle unfamiliar event types" — the discriminated-union-with-unknown-fallback pattern.
8. **RFC 9457 problem-details are open by standard** (§3.2): extension members are allowed
   and consumers MUST ignore unknown ones. Sealing a `ProblemDetail` schema is
   non-conformant.
9. **Breaking-change gates (oasdiff, Specmatic)** treat added optional response fields as
   safe, flag added response enum values as a warning (mitigation: extensible-enum), and
   are the mechanism to keep responses open-but-documented while catching real breaks in CI.

**Refuted (did not survive verification):** the claim that AIP-126 *requires* an
`_UNSPECIFIED`/`UNKNOWN` zero-value enum default (0-3). An explicit unknown/default variant
is a recommended *pattern* (Stripe, protobuf) but not an AIP-126 mandate.

## Caveats / must-verify

- **kin-openapi enforce behaviour (load-bearing):** no source covered kin-openapi
  specifically. The rubric assumes standard JSON-Schema open-by-default semantics — that
  enforce mode **passes** an additive/unknown response field against a typed-but-open
  schema and **fails** it against `additionalProperties: false`. **Confirmed 2026-07-03**
  via a probe against kin-openapi v0.140.0 (`Schema.VisitJSON`): additive field passes the
  open schema, `additionalProperties: false` rejects it. A permanent fixture test lands with
  the implementation (the probe was temporary). ADR 0003 records this as confirmed.
- `x-extensible-enum` is deprecated by Zalando in favour of `examples` + "[Extensible
  enum]"; both express the same intent. Pick whichever cyoda-go's toolchain/consumers read
  best; note `x-extensible-enum` is a proprietary extension some validators reject.
- Protobuf unknown-field *preservation* is binary-wire-only (JSON drops unknowns) — it is
  an analogy for tolerant-reading, not a directly transferable JSON mechanism.

## Consequences for the cyoda-go reconciliation

1. The earlier "tighten with `additionalProperties: false`" prescription (in the
   entity-slice spec and the drift audit) is **wrong** → replace with **typed-but-open**.
2. `ProblemDetail.properties`, entity `data`, and `JsonNode` come **off** the tightenable
   list — they are correct as open.
3. The state-machine audit-event union should be integrated as category (c): discriminated
   `oneOf` + unknown/default variant + open-enum discriminator.
4. Add an **additive-only breaking-change CI gate** on `api/openapi.yaml` as the evolution
   discipline (replacing "seal the schema").
5. Anchor all of the above in **ADR 0003** (supersedes ADR 0001), gated on the kin-openapi
   empirical check.

## Sources

Primary: RFC 9457; Google AIP-180/126/185; Zalando RESTful API Guidelines
(compatibility.adoc, json-guidelines.adoc); Stripe API upgrades; protobuf.dev
(UnknownFieldSet). Secondary/community: learnjsonschema.com (2020-12
additionalProperties, unevaluatedProperties); oasdiff; Specmatic; Martin Fowler
(TolerantReader).
