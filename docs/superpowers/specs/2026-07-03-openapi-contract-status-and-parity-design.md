# OpenAPI Contract Status & Cloud Parity (prerequisite to the entity slice)

**Date:** 2026-07-03
**Status:** design / awaiting review
**Governed by:** ADR 0003 (`docs/adr/0003-openapi-contract-conformance-and-evolution.md`).
**Reference evidence:** `docs/analysis/openapi/README.md` (drift audit).
**Runs before:** `2026-07-02-openapi-contract-reconciliation-design.md` (entity slice) — this
spec makes the published surface honest and enforceable so the entity slice lands on a clean base.

---

## 1. Problem

`api/openapi.yaml` publishes ~22 operations (the `Stream Data` and `SQL-Schema` tags) that are
excluded from codegen (`api/config.yaml` `exclude-tags`), unrouted, and therefore return 404 at
runtime — with **no in-spec signal** that they are not live. Worse, the coverage gate skips
excluded-tag operations entirely (`internal/e2e/e2e_test.go` drops them before building
`allOperationIds`), so they are invisible to conformance accounting: in the spec, unrouted, and
unmeasured. A separate list, `knownUncoveredOps` (`internal/e2e/zzz_openapi_conformance_test.go`),
hides a second class of not-live operations (IAM/OIDC stubs, a routing wart) from the gate. An
app-builder or LLM reading the published contract cannot distinguish a live operation from one
that 404s — the same "silent" failure mode that motivated the whole reconciliation.

The published contract must state each operation's status honestly and enforce it, so that "in
the spec" means either **live** (implemented, routed, and tested) or **explicitly marked**
not-live — never silently 404.

## 2. Goals / Non-goals

**Goals**
- A machine-readable operation-status marker (`x-cyoda-status`) distinguishing live from
  planned/unimplemented, visible to consumers, cyoda-docs, and Cloud.
- A coverage gate that enforces **exactly one of {exercised-by-e2e, marked}** per published
  operation — retiring both `knownUncoveredOps` and the excluded-tag skip.
- An **additive-only breaking-change gate** on `api/openapi.yaml` (ADR 0003, Decision 6).
- Delete only genuinely-dead schemas.
- A `docs/cloud-parity/` record handing off the Cloud-side conformance mechanism.

**Non-goals**
- Implementing SQL-Schema or Stream Data (marked, not built here).
- The entity-group shape reconciliation (the entity slice; runs after).
- Integrating the `StateMachine*Dto` audit-event union (audit-group follow-on).
- Any machinery that reads or asserts against `docs/cyoda/` — that is a read-only vendored
  reference; Cloud conforms to cyoda-go, not the reverse.

## 3. The `x-cyoda-status` marker

An OpenAPI specification extension on the operation object. Two values:

| Value | Meaning | Lifecycle |
|---|---|---|
| `planned` | In the contract and **committed** to be implemented in cyoda-go (roadmap / tracked). | → becomes live (marker removed) when implemented. |
| `unimplemented` | In the contract, **not committed**; disposition under review. | → either becomes `planned`/live, or is removed from the spec once a "won't implement" decision is final. |

- Every operation that is **not** live carries exactly one `x-cyoda-status`.
- Each marked operation's `description` also opens with a human-readable caveat (e.g.
  "NOT YET IMPLEMENTED in cyoda-go — planned for Trino integration"), so a human reading the
  rendered spec (Scalar UI, cyoda-docs) sees it without inspecting extensions.
- Live operations MUST NOT carry the marker.
- `x-cyoda-status` is **orthogonal to codegen exclusion.** Only the `Stream Data` and
  `SQL-Schema` tags are excluded from codegen (`api/config.yaml`); the IAM/OIDC/account stubs
  are generated and routed yet still `planned`. So a marked operation may or may not have a
  generated client method — the marker records *implementation status*, not codegen state.

## 4. The coverage gate (live-or-marked)

Replace the two current escape hatches with one spec-derived rule.

- **Stop skipping excluded-tag operations** when building `allOperationIds`
  (`internal/e2e/e2e_test.go`): the gate now sees every published operation.
- **Retire `knownUncoveredOps`** (`internal/e2e/zzz_openapi_conformance_test.go`): its entries
  become `x-cyoda-status` markers in the spec (§6).
- **Marker set:** built in `TestMain` (`internal/e2e/e2e_test.go`, which already iterates
  `swagger.Paths.Map()`) by reading each operation's `x-cyoda-status` from
  `openapi3.Operation.Extensions`, and exported as a package var (`markedOps`) alongside
  `allOperationIds`. The gate in `zzz_openapi_conformance_test.go` reads it — no second source
  of truth.
- **Rule 1 (coverage):** each published operation must be *exercised* (hit by ≥1 e2e — proxy for
  routed-and-live) **or** *marked*. Unexercised and unmarked → CI fail (a silent 404, or a live
  op with no coverage).
- **Rule 2 (stale-marker guard):** a *marked* operation exercised **with a 2xx success** → CI
  fail (the marker sits on an operation that is actually live). Exercising a marked op with only
  `501`/`4xx` does **not** trip this — so a stub's documented `501` can be covered per
  `.claude/rules/test-coverage.md` while the marker stands. This needs the collector to record
  whether each operation ever returned a 2xx; add that minimal per-op flag (the entity slice's
  richer `(op, status, errorCode)` matrix builds on it).

This subsumes the entity slice's earlier plan to "reuse `knownUncoveredOps`": that list no
longer exists after this spec; the entity slice's error-code matrix builds on the marker-aware gate.

## 5. The additive-only evolution gate (ADR 0003, Decision 6)

A breaking-change detector (`oasdiff` or equivalent) runs as a CI merge gate, diffing the PR's
`api/openapi.yaml` against the base branch's, and **fails on breaking edits**: sealing a
previously-open object (`additionalProperties: false`), removing a field/operation, narrowing a
type, adding a required request field, or removing an enum value. This is the discipline that
lets schemas stay typed-but-open (per ADR 0003) without brittleness — precision is guarded by
the gate, not by sealing. Adding `x-cyoda-status` to an existing operation, and removing it when
an operation goes live, are non-breaking and pass.

- Wire as a dedicated CI job (mirrors the existing `per-module-hygiene` pattern).
- Ship one fixture pair proving the gate catches a representative breaking edit and passes a
  representative additive one, so the gate itself is trusted.
- **Interaction with §7 dead-schema deletion:** this same PR removes unreferenced component
  schemas, and "removing a field/operation" is a breaking signal. Confirm whether the detector
  flags removal of a genuinely *unreferenced* component (oasdiff generally does not, since no
  operation resolves it) — if it does, allow-list the specific dead schemas being deleted, or
  land the deletion in a separate commit ahead of the gate turning on. Resolve this before the
  single-commit constraint (§12) so the gate does not flag its own PR.

## 6. Operation dispositions

| Operation(s) | Disposition | `x-cyoda-status` |
|---|---|---|
| `Stream Data` tag (13 ops) | most likely not implemented; undecided | `unimplemented` |
| `SQL-Schema` tag (9 ops) | committed — Trino / `trino-cyoda` connector (decision recorded in this effort) | `planned` |
| `accountSubscriptionsGet` | genuine stub — always `501` (`internal/domain/account/handler.go:87`) | `planned` |
| OIDC provider ops (`registerOidcProvider`, `deleteOidcProvider`, `invalidateOidcProvider`, `reactivateOidcProvider`, `listOidcProviders`, `reloadOidcProviders`, `updateOidcProvider`) | **status must be verified per op** — dispatched via `s.Account.<Op>` when wired, else `501` (`internal/api/server.go:531+`); the adapters carry real logic, so some may be live | verify then assign — see below |
| `fetchEntityTransitions` | **live** — routed (mounted outside the generated `ServerInterface`), not a stub | none — add e2e coverage so it is *exercised*, not marked |

**Per-op status verification (required before marking).** The `knownUncoveredOps` comment calls
the OIDC group "stub-implemented," but the adapters carry real behaviour (e.g. duplicate →
`400`, inactive → `409`); whether they return `501` or a real `2xx` depends on the e2e server
wiring (`s.Account != nil`). Before assigning a marker, verify each op's **actual runtime status
under the e2e config**: an op that returns `2xx` is **live** (cover it, do not mark — else Rule 2
fails it); an op that returns `501` is `planned`. Do not trust the stale "stub" comment.
`fetchEntityTransitions` moving to real coverage removes the last "routing wart" exemption; the
marker is reserved strictly for not-live operations.

## 7. Dead-schema deletion (minimal, verified)

Delete a component schema only when it is **unreferenced transitively within
`api/openapi.yaml`** (no operation and no other kept schema reaches it) **and** is not a
generated Go model consumed by `api/generated.go` or handlers. Verify per schema before deleting.

- **Exclude the `StateMachine*Dto` cluster** — the audit endpoints emit that event shape; it is
  under-integrated, not dead, and its integrate-vs-delete decision is an audit-group follow-on.
- Expected set is small; the leading candidate is a bare `ErrorResponse` duplicate (the audit
  finished-event endpoint uses `ErrorResponseDto`). Each deletion is confirmed by the transitive
  reachability check, not by the earlier HTTP-path-only "orphan" scan (which false-flagged the
  `StateMachine*Dto`).

## 8. Spec embedding (no regeneration step)

`GetSwagger()` embeds `api/openapi.yaml` **verbatim** via `//go:embed` (`api/spec.go`;
`api/config.yaml` sets `embedded-spec: false`, so oapi-codegen does not emit its own re-encoded
copy). Editing `api/openapi.yaml` is therefore the only step — the embed picks up the new bytes at
build time, and no embedded-vs-source drift is possible. There is nothing to regenerate and no
drift test is needed. (An earlier audit note describing a gzip+base64 snapshot referred to a
since-replaced `embedded-spec: true` configuration.)

## 9. Cloud-parity hand-off (design-only in this repo)

cyoda-go's responsibility ends at publishing an honest contract. The parity mechanism lives in
cyoda-cloud:

- cyoda-go **publishes** `api/openapi.yaml` (already served at `/openapi.json` and via the help
  subsystem). The **live common ground** = operations *without* `x-cyoda-status`.
- Cloud **consumes** that published spec and runs response-conformance validation against its own
  live responses, asserting Cloud conforms to the live common ground and may only *extend* it
  (add operations/fields), never diverge on it. `planned`/`unimplemented` operations are
  shared-intent Cloud may or may not have.
- This repo produces **one `docs/cloud-parity/` record** stating the contract expectation and the
  tolerant-reader obligation (ADR 0003, Decision 3), plus a hand-off issue to the Cloud team. No
  cyoda-go test or gate is built for the Cloud side.
- **Reconcile with `docs/cyoda/cloud-divergences.md`:** that file catalogs *field-level* and
  *semantic* deliberate divergences (e.g. `ProcessorDefinitionDto.asyncResult`); `x-cyoda-status`
  covers *operation-level* not-live status. They stay distinct and cross-reference; no op-level
  entries are duplicated between them.

## 10. Coverage matrix (scenario × layer)

Per `.claude/rules/test-coverage.md`. This spec changes contract-governance infrastructure and
adds no HTTP/gRPC endpoint or error code, so coverage is at the gate/infra layer.

| Scenario | Unit | Running-backend e2e | Cross-backend parity | gRPC |
|---|---|---|---|---|
| Coverage gate: unexercised+unmarked → fail | ✓ (gate logic) | ✓ (suite enforces) | — | — |
| Coverage gate: exercised+marked → fail (contradiction) | ✓ | — | — | — |
| Every published unrouted op carries a valid `x-cyoda-status` | ✓ (spec-lint test) | — | — | — |
| oasdiff gate: breaking edit fails, additive edit passes | ✓ (one fixture pair) | — | — | — |
| `fetchEntityTransitions` now exercised | — | ✓ | — | — |

No new error codes or endpoints → no per-endpoint error table and no gRPC error-envelope rows.
Concurrency: n/a.

## 11. Documentation & gate obligations

- **Marker convention** documented in `CONTRIBUTING.md` (and a short note where the help subsystem
  describes the spec), so future operations are marked or covered by rule, not habit.
- **Gate 4:** no new error codes or env vars. If the oasdiff gate is a new `make` target / CI job,
  update `CONTRIBUTING.md` and the CI docs.
- **Gate 7:** the `docs/cloud-parity/` record (§9).
- **ADR:** governed by ADR 0003; no new ADR.

## 12. Sequencing & risks

- **Sequencing:** this spec lands first. It converts the two silent escape hatches into an
  enforced live-or-marked contract, so when the entity slice adds response-shape tightening and
  the error-code matrix, the surface is already honest and the coverage gate already marker-aware.
- **Risk — turning on full coverage surfaces gaps.** Making the gate see every operation
  (~22 excluded-tag ops added to `allOperationIds`, ~9 `knownUncoveredOps` resolved → ~30 markers
  + 1 new e2e) may fail CI for a live-but-uncovered op we hadn't noticed. Mitigation: that is the
  point; each such op gets e2e coverage (if live) or a marker (if not) in this PR — no silent
  suppression. The marker additions, the coverage-gate flip, and the `knownUncoveredOps`
  retirement must land in the **same commit** so CI never observes an intermediate red state.
- **Task — extension round-trip.** Confirm `scalar_test.go` / help-subsystem tests assert output
  *prefixes* (not full-document byte equality) so the added `x-cyoda-status` extensions don't
  break them; `/openapi.json` (`scalar.go`) round-trips extensions by design, which is intended
  (Cloud/consumers see the status).
- **Risk — marker misuse.** A marker on a live op, or a missing marker on an unrouted op, both
  fail the gate by construction (§4), so the marker cannot drift silently.
- **Risk — Stream Data disposition still open.** `unimplemented` is honest for "undecided"; when
  the decision is final the ops are either implemented (marker removed) or removed from the spec.
  No blocker for this spec.
