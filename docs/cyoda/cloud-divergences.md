# Cloud divergences

cyoda-go is the digital twin of Cyoda Cloud. Most of the API surface
matches one-for-one. This page tracks the **deliberate, known
divergences** — fields cyoda-go declares for spec parity but does not
yet implement, behavior that intentionally differs, or
enterprise-tier features that live only in the closed-source
Cassandra plugin.

This is the canonical place for "I see this in the OpenAPI spec but
cyoda-go silently ignores it" entries. Add new rows here whenever a
divergence is identified.

| Surface | Divergence | Status | Tracking |
|---|---|---|---|
| `ProcessorDefinitionDto.asyncResult` | Field declared in OpenAPI; OSS backend rejects `asyncResult=true` at workflow import (400 VALIDATION_FAILED). The explicit `asyncResult=false` and absent cases are accepted and round-tripped. Crossover semantics need durable suspend state + cluster-wide work-stealing recovery + a distributed timer — implementable only in the commercial backend. | Reject-at-import on OSS; enterprise-tier in the commercial backend (not yet implemented there either). | [#223](https://github.com/Cyoda-platform/cyoda-go/issues/223) |
| `ProcessorDefinitionDto.crossoverToAsyncMs` | Field declared in OpenAPI; OSS backend rejects any non-nil `crossoverToAsyncMs` at workflow import (400 VALIDATION_FAILED), including the orphan case where `asyncResult` is absent or false. See `asyncResult` — same parity gap. | Reject-at-import on OSS; enterprise-tier in the commercial backend. | [#223](https://github.com/Cyoda-platform/cyoda-go/issues/223) |
| 22 IAM/OAuth/OIDC/account stub endpoints | Declared in OpenAPI as `501 Not Implemented`; handlers return 501 by design (per ADR 0001's A+C policy on the conformance reconciliation). | Deferred. | [#194](https://github.com/Cyoda-platform/cyoda-go/issues/194) |
| `EdgeMessage.payload` content types beyond JSON | OpenAPI's `contentType` field suggests support for non-JSON; cyoda-go currently stores/returns JSON-encoded values only. Cloud has the same restriction today. | Future feature, would lead Cloud. | [#193](https://github.com/Cyoda-platform/cyoda-go/issues/193) |

## Adding a row

When you discover a divergence:

1. File a tracking issue (or reference an existing one).
2. Add a row above with: surface, what diverges, current status (silent-ignore / partial-impl / deferred / enterprise-only), tracking issue.
3. If the divergence is silently ignored, add a `⚠️` note to the OpenAPI field's `description` so SDK consumers see the gap at the spec layer too.

## Why we keep declaring fields we don't implement

Per ADR 0001 (`docs/adr/0001-openapi-server-spec-conformance.md`), our
spec mirrors Cloud's so client SDKs generated against either spec are
shape-compatible. Removing fields from the spec to match server
behavior would break that compatibility for clients moving between
deployments. Keeping the field declared with a `⚠️` divergence note is
the lesser evil.

## Anti-pattern

Never silently flip server behavior to *match* a Cloud field whose
shape we declare but whose runtime semantics we don't implement.
Either:

- Implement the field properly (preferred), OR
- Document the divergence here AND in the OpenAPI description (current
  policy for the rows above).

The "silently honor a fraction of the field" middle ground is what
this document exists to prevent.
