# 0001. OpenAPI Server-Spec Conformance Approach

**Status:** Accepted
**Date:** 2026-04-29

## Context

`cyoda-go` exposes ~81 operations declared in a 9 202-line, hand-edited
OpenAPI **3.1.0** specification (`api/openapi.yaml`). The spec is the
authoritative shared contract across cyoda-go, the `cyoda-go-cassandra`
plugin, the `cyoda-docs` site, and the upstream Cyoda Cloud team. Code is
derived from spec — not the other way around.

`oapi-codegen` v2.6.0 with `std-http-server: true` generates the server
interface from the spec, enforcing operation routing and request-parameter
typing. It does **not** enforce response shapes: handlers call
`common.WriteJSON(w, anyValue, status)` with whatever Go value they
choose, and nothing checks that the bytes match the spec or that the
status code is one the spec declares. This is the root of issue #21:
spec-vs-server drift exists today (POST create returns array but spec
says single object; GET entity returns envelope but spec says raw entity;
`messaging.GetMessage.content` is JSON-encoded as a string instead of
embedded JSON), and there is no automated guard preventing future drift.

There are no production consumers yet, so backwards-incompatible wire
changes are acceptable now and increasingly painful later. The project
charter is "do it right or don't bother."

The decision is what conformance mechanism to adopt: how strictly the
server is held to the spec, and at what layer enforcement happens.

## Decision

**Adopt runtime response-shape validation at the E2E test boundary.** Use
`kin-openapi` (already a direct dep at v0.137.0) inside an E2E HTTP test
middleware that validates every response against its operation's spec
schema. Drift surfaces as an E2E test failure naming the operation and
the offending JSON path. Handler signatures, the codegen mode, and the
loose `WriteJSON` pattern stay as they are.

**Defer compile-time strict typing of response shapes.** The
`oapi-codegen` strict-server migration (and the `ogen` alternative) were
both seriously evaluated and rejected at this stage. Reconsider when at
least one of these signals appears:

- Production consumers exist that consume `api/openapi.yaml` to generate
  clients and would be broken by silent server drift.
- The spec changes frequently enough that runtime drift becomes a
  recurring incident class rather than a one-time audit.
- Cyoda Cloud adopts strict typing, and contract parity is a stronger
  argument than today's "spec-as-shared-document" posture.

The reconsideration trigger lives in the open-items list at the end of
this ADR; revisiting requires a new ADR superseding this one.

## Consequences

### Positive

- **Drift detection without migration churn.** Every E2E response is
  automatically checked against the spec; new tests inherit the guard.
  No handler signature changes, no per-op error wrappers, no generator
  scripts, no long-running release branch.
- **Polymorphic fields are validated at runtime** in a way that
  compile-time strict typing fundamentally cannot do (the strict path
  necessarily declares `interface{}` / `json.RawMessage` for
  user-supplied payloads, giving up validation on those fields).
- **Bounded one-time work.** Spec audit + per-defect handler fix +
  validator middleware. Estimated 2-3 days end-to-end vs the strict
  path's multi-week migration.
- **Flexibility preserved.** Handler code stays terse; `WriteJSON` /
  `WriteError` helpers continue to work; service-layer types are not
  coupled to generated wire types.
- **Spec-first stays intact.** The spec remains canonical; this ADR
  does not change how code is derived from it. Only the conformance
  mechanism is added.

### Negative

- **Runtime, not compile-time guarantee.** A handler can return the
  wrong shape and we find out at the next E2E run, not at build. This
  is acceptable while we have a robust E2E suite (we do — `make
  test-all` covers all backends including postgres testcontainers); it
  becomes weaker if a wire-shape regression slips into a less-covered
  path. **Mitigation:** treat E2E coverage as load-bearing for this
  guarantee; gaps surfaced during the spec audit get tests added
  in-PR per Gate 6.
- **No defense against bugs in the validator itself.** `kin-openapi`'s
  response validator could miss a class of mismatch we care about. We
  pin the dep version, watch upstream issues, and verify the validator
  catches each known defect during the audit (the four named in #21
  serve as fixtures: POST array shape, GET envelope, JSON-in-string
  content, basicAuth — the validator must catch all four before being
  trusted as a guard).
- **Spec edits must remain disciplined.** The runtime validator only
  catches drift between spec and server; it does not catch errors
  *within* the spec itself (e.g. a typo in a field name that propagates
  to both the spec and the handler). The hand-editing nature of
  `api/openapi.yaml` already imposes this discipline; the runtime
  validator does not change it.
- **Strict-server migration is still a future cost** if/when consumers
  require it. The validator does not move us closer to compile-time
  guarantees; it sidesteps them. Re-evaluation will require a new ADR.

### Neutral

- **Build, codegen, and the help subsystem (`cmd/cyoda/help/openapi*`)
  are unchanged.** The shipped artefact (`/openapi.json`,
  `cyoda help openapi yaml|json|tags`) stays as-is in shape; only the
  contents of the spec change as defects are fixed.
- **`e2e/parity/client/types.go`** updates only where corrected wire
  shapes change the types it mirrors. Same import surface for the
  Cassandra plugin.

## Alternatives considered

### A. `oapi-codegen` v2.6.0 with `strict-server: true`

Generates per-operation typed response sum types; handler signatures
become `(ctx, req) (Response, error)`; build fails on shape or status
drift. Compile-time guarantee is the right idea.

Rejected because:

- **Mythical generic helper.** `oapi-codegen`'s response types are
  nominal sealed interfaces with per-operation marker methods; a
  generic `ToTypedResponse[R](err) R` is not implementable in Go.
  The honest pattern is per-operation error wrapper functions —
  ~250-400 trivial functions across 81 ops. Either ~5150 generated
  lines (with a custom reflection-walking script and silent-failure
  risk) or ~1500 hand-written lines duplicated across handlers. Either
  is bounded but real.
- **Per-op default-response types.** Each operation gets its own
  `<OpName>defaultJSONResponse`; the security argument for a single
  5xx chokepoint moves down to a shared body-builder, with per-op
  wrappers around it.
- **Dual-interface coexistence is hard.** `NewStrictHandler` wraps the
  full `ServerInterface`. Per-domain incremental migration requires
  an `embed-and-override` `notImplementedHandler` scaffolding that
  lives only during the migration.
- **OpenAPI 3.1 support is "awaiting upstream parser improvements"**
  per the project README. Works for our current spec by accident; one
  idiomatic 3.1 spec edit (`type: ["string", "null"]`) away from being
  blocked.
- **No consumer pressure justifies the cost today.** Compile-time
  guarantees protect downstream consumers against silent server drift.
  We have none. The migration is "do it right for an imagined future
  state with strict-typed consumers we don't yet have."

If consumer pressure or 3.1 friction emerges, this is the most likely
revisit candidate.

### B. `ogen` (`github.com/ogen-go/ogen`)

Spec-first, strict-by-default, first-class OpenAPI 3.1, per-operation
sum-type responses returned directly (no marker-method gymnastics),
single global `NewError(ctx, err)` hook, real Go sum types for
discriminator unions, `go-faster/jx` zero-reflection JSON.

Rejected after running ogen v1.20.3 against the actual `api/openapi.yaml`:

- **6 core operations failed to generate** due to documented
  unimplemented features (`searchEntities`,
  `searchEntityAuditEvents`, `getAllEntities`,
  `exportEntityModelWorkflow`, `importEntityModelWorkflow`,
  `submitAsyncSearchJob`). Reasons: "sum types with same names not
  implemented", "type-based discrimination with same jxType not
  implemented", "unsupported content types: application/x-ndjson".
- **Polymorphic bodies emit `struct{}`** by default. The
  `additionalProperties: true` workaround emits `map[string]jx.Raw`
  for object-shaped bodies but cannot express "object OR array"
  payloads (entity create/update endpoints) without spec restructuring
  that ogen's `oneOf` gaps may not accept.
- **`application/x-ndjson` is unsupported** entirely; the streaming
  variant of `getAllEntities` would be dead.

ogen would force open-ended spec restructuring exercises around 6 core
operations — unbounded work in exchange for the architectural wins.
Reconsider if upstream gaps close.

### C. `goa` (`github.com/goadesign/goa`)

**Eliminated on the spec-first constraint.** Goa is design-first by its
core concept: you write the API in Goa's Go DSL, `goa gen` produces both
the OpenAPI document and the server skeleton. Spec-first usage requires
manually re-translating the existing 9 202-line YAML into Goa DSL — an
explicit "translate-then-regenerate" workflow with no round-trip safety
back to YAML.

For cyoda-go specifically, this would invert the contract: the Go DSL
becomes canonical, the YAML is downstream-generated, and every external
consumer (cyoda-docs, Cyoda Cloud team) reads a generator output rather
than the source of truth. That contradicts the load-bearing project
constraint.

### D. Runtime validation via `kin-openapi` (this decision)

Detailed above. **Accepted.**

### E. Other spec-first Go OpenAPI server frameworks

`swagger-codegen` and `openapi-generator` produce Go server stubs but
their generated code quality is widely cited as the worst of the field;
neither merits a serious slot. Industry comparisons (Speakeasy,
oapi-codegen project discussions) name `ogen` and `oapi-codegen` as the
only two production-grade Go server choices; both are evaluated above.

## Open items for the implementing PR (#21)

1. **Validator placement.** Where in the E2E test stack does the
   `kin-openapi` middleware live? Likely a `RoundTripper` wrapping
   `httptest.NewServer`'s client. Verify the middleware can extract the
   matched operation from the request method + path + spec, then
   validate the response body + status against that operation's
   declared schemas.
2. **Defect-fixture pass.** Before the validator is trusted as a guard,
   run it against the four known defects (POST create array, GET
   envelope, `messaging.GetMessage.content` JSON-in-string,
   `basicAuth` reference). It must catch each one. If it misses any,
   investigate the validator before continuing.
3. **Audit table.** Produce a per-operation spec-vs-server inventory
   either in the PR description or as a checked-in markdown artefact.
   This is the deliverable that closes the "audit" half of #21.
4. **Performance.** The validator runs only in E2E (test-time), not in
   production. No production-path performance question.
5. **Reconsideration triggers** above (consumer pressure, 3.1 friction,
   Cloud parity) become a `// TODO(adr-0001): revisit` only if a real
   signal surfaces; otherwise this ADR stands.
