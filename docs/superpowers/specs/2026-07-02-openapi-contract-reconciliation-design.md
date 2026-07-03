# OpenAPI Contract Reconciliation — Phase 0 Gate + Entity Slice

**Date:** 2026-07-02
**Status:** design / awaiting review
**Reference evidence:** `docs/analysis/openapi/README.md` (full 87-op audit, six-pattern
taxonomy, per-finding reconciliation direction).

---

## 1. Problem

`api/openapi.yaml` is a deliberate, authored, `//go:embed`'d contract — the definitive
common ground between `cyoda-go` and `cyoda-cloud`. It is intentionally **not** generated
from Go. The cost of that choice is that nothing binds either side to the contract, and
the audit (`docs/analysis/openapi/README.md`) found ~55 divergences across 87 operations,
including a data-loss defect and a 25%-dead published surface. An app-builder generating a
client from the spec reaches materially wrong conclusions — the trigger case: concluding
from `GET /entity/{entityId}` that there is *no way to learn the entity model*, because
`meta` is an opaque bag whose prose names a field (`previousTransition`) that does not
exist.

The mess is not the 55 findings. It is that **any fix rots**, because there is no
enforced binding. This effort installs the binding first, then reconciles under it.

## 2. Goals / Non-goals

**Goals**
- A durable **conformance gate** that makes server↔spec drift fail CI, with a portable
  core decoupled from `cyoda-go` internals (liftable to a shared cyoda-go/Cloud kit later).
- A repeatable **reconciliation loop** (classify → fix losing side → lock → record) that
  every follow-on group reuses.
- Apply both to the **entity group** end-to-end as the proving vertical, including
  implementing the documented conditional `deleteEntities`.

**Non-goals (this spec)**
- Reconciling the other five groups (stats/audit/search, model, auth/oidc, message) — each
  is its own spec→plan cycle fed through the loop established here.
- The dead-surface disposition (22 excluded-tag ops + always-501 stubs) — a contract
  decision deferred to its own spec.
- Lifting the gate into a shared cross-repo kit — the core is *structured* to allow it;
  extraction is future work.
- Changing the authoring model: the spec stays hand-authored and owned. The gate binds the
  **server** to the authored spec; it does not generate the spec.

## 3. Architecture — two pillars

```
                 api/openapi.yaml  (authored contract, source of intent)
                          │
          ┌───────────────┴────────────────┐
          │                                 │
   PILLAR A: conformance gate        PILLAR B: reconciliation loop
   (portable core + e2e harness)     (per-finding process + recording)
          │                                 │
          │  validates every e2e response   │  classify direction
          │  against the op schema;         │  → fix losing side (TDD)
          │  asserts error-code matrix      │  → tighten schema / matrix (locks it)
          │  (bidirectional)                │  → record contract decision
          └───────────────┬────────────────┘
                          │
              tightening a loose schema is simultaneously
              the FIX (B) and what makes the GATE (A) enforceable
```

The two pillars are coupled by design: the gate without tightening validates nothing
meaningful (an `additionalProperties:true` bag passes anything); tightening without the
gate rots. They advance together, per finding.

## 4. Pillar A — the conformance gate

### 4.1 Portable core (`internal/e2e/conformance/`, no cyoda-go internals)

A self-contained package that depends only on the spec file, an HTTP response, and a
static error-code registry — deliberately free of cyoda-go domain types so it can later be
lifted into a shared kit.

- **`SpecSet`** — loads and indexes `api/openapi.yaml` (via the existing
  `kin-openapi/openapi3` dependency already used by the e2e `openapivalidator`). Resolves
  an `(method, path-template)` → operation, request schema, per-status response schema,
  and declared content-types.
- **`ValidateResponse(op, status, contentType, body) []Violation`** — asserts the response
  body validates against the operation's schema for that status, the status is a declared
  response, and the `Content-Type` matches a declared media type. Returns structured
  violations (never panics; never mutates).
- **`ErrorCodeMatrix`** — a static registry: `operationId → {documented status/error
  codes}`, authored from the spec's per-endpoint error tables (§7). Exposes two checks used
  by the harness aggregation:
  - **producible**: every documented code was observed by ≥1 e2e (catches *fictional*
    codes — P5).
  - **declared**: no e2e produced a status/error-code absent from the registry (catches
    *missing* codes — P5).

The core is unit-tested in isolation (§8.1): a fixture spec + hand-built good/mutated
responses prove both validation directions and both matrix directions fail exactly when
they should. This is what makes the gate trustworthy enough to gate everything else.

### 4.2 e2e harness integration (`internal/e2e/`, cyoda-go-aware)

- A thin `recordAndAssert` helper wraps existing e2e request execution: after each call it
  feeds `(op, status, contentType, body)` to the core validator and accumulates the
  observed `(op, status/errorCode)` pairs into a run-scoped collector.
- Builds on the existing e2e-only `openapivalidator` and the `entity_conformance_test.go`
  seed rather than replacing them; the seed's ad-hoc assertions are migrated to the core.
- A `TestConformance_ErrorCodeMatrix` aggregates the run-scoped collector at suite end and
  runs the producible+declared checks **for the operations in scope** (entity group in this
  spec). Out-of-scope operations are explicitly listed as `pending` so the matrix never
  silently claims coverage it doesn't have (no-silent-caps).

### 4.3 What the gate does *not* do

- It does not validate the polymorphic entity `data` payload — that stays
  `additionalProperties:true` by contract (user-defined shape). The gate validates the
  **envelope** (`type`/`data`/`meta`), not the user's document.
- It is only as strong as the spec is precise. For the entity slice we tighten the relevant
  schemas (§6); still-loose schemas in other groups will pass permissively until their own
  plans tighten them — the gate must not false-fail on them.

## 5. Pillar B — the reconciliation loop

For each finding, in order:

1. **Classify direction** — `spec-stale` (server right, fix spec) · `spec-incomplete`
   (server right, enrich spec) · `server-gap` (spec is intended contract, fix server) ·
   `needs-decision` (record the decision, then it becomes one of the above).
2. **Fix the losing side** via red/green TDD (Gate 1). Spec-side fixes are edits to
   `api/openapi.yaml` (+ `api/generated.go` regen where models change); server-side fixes
   follow the bugfix TDD protocol.
3. **Lock it** — tighten the schema (add `properties`, set `additionalProperties:false`,
   correct types) and/or add the `ErrorCodeMatrix` entry, so the gate now fails if the
   fix regresses.
4. **Record the contract decision** in `docs/cloud-parity/` (Gate 7) — one file per
   behaviour, stating the direction chosen and why, so Cloud mirrors the same contract.

**Tightening policy:** tightened response objects use declared `properties` +
`additionalProperties:false`. Consequence: adding a new field later fails CI until the spec
is updated. In a spec-first world this is the intended binding, not a regression.

## 6. The entity slice — findings, directions, fixes

| # | Finding | Direction | Fix | Lock |
|---|---|---|---|---|
| E1 | `Envelope.meta` opaque bag + `previousTransition` fossil (`getOneEntity`, `getAllEntities`) | spec-incomplete + spec-stale | Named `EntityMeta` schema: `id`, `modelKey{name,version}`, `state`, `creationDate`, `lastUpdateTime`, `transactionId`, optional `transitionForLatestSave`; `additionalProperties:false`; delete `previousTransition`; fix examples | response validation asserts meta shape on both ops |
| E2 | `deleteEntities` ignores condition body + `pointInTime` + `verbose`; wipes whole model | server-gap (data loss) | **Implement** the documented contract (§6.1) | e2e (subset survives) + parity + matrix |
| E3 | `getAllEntities` ignores `pointInTime` | server-gap | Plumb `pointInTime` into `ListEntities` reusing the existing PIT path (`getOneEntity` has it) | as-at e2e + parity |
| E4 | `createCollection` / `create` request body `type:object` erases real shape | spec-incomplete | Tighten request schemas to the array-of-`{model{name,version},payload}` shape; document the batch/array `create` form | request validation |
| E5 | create/update/patch/collection composite-unique-key `409`/`422` codes undocumented | spec-incomplete | Add codes to the endpoints' error tables | error-code matrix (producible+declared) |
| E6 | `EntityChangeMeta.fieldsChangedCount` advertised, never emitted | spec-stale | Remove `fieldsChangedCount` from schema + examples | response validation |
| E7 | `getEntityChangesMetadata` ordering prose says chronological; server is newest-first | spec-stale | Fix prose to reverse-chronological (newest-first is the correct audit default) | (doc-only; no gate assertion) |
| E8 | `changeType` prose `CREATE/UPDATE/DELETE` vs enum/emit `CREATED/UPDATED/DELETED` | spec-stale | Fix prose to match enum | response validation (enum) |
| E9 | `getOneEntity` 200 example shows only `{id,state}` | spec-incomplete | Enrich example to the full meta shape | (example; validated structurally by E1) |

### 6.1 `deleteEntities` — implementing the documented contract

The contract: delete only entities matching an `AbstractConditionDto` body, honouring
`pointInTime` selection and `verbose` (return deleted ids). Implementation principle
(no special engine rights — reuse existing primitives):

> **select-then-delete.** Evaluate the condition using the *same condition-evaluation
> primitive `searchEntities` already uses* to obtain the matching entity ids (at
> `pointInTime` when supplied), then delete those ids. When `verbose`, return the deleted
> ids in `StreamDeleteResult.ids`.

- No new engine capability, no new cross-node invariant. Condition evaluation already works
  consistently across backends, so cross-backend parity follows from reusing it.
- **Absent/empty body** preserves today's "delete all of the model" behaviour (backward
  compatible). A **present** condition scopes the delete. `verbose` toggles id return.
- Data-loss is eliminated structurally: a condition that is present is *honoured*, never
  silently ignored.
- Records a `docs/cloud-parity/` entry: "server now conforms to conditional delete."

## 7. Per-endpoint error/status tables (in-scope endpoints)

Authored into the `ErrorCodeMatrix`. `✎` = added/changed by this spec.

**`GET /entity/{entityId}` (getOneEntity)**

| Status | Code | Producing scenario |
|---|---|---|
| 200 | — | entity found |
| 400 | (conflicting `pointInTime`+`transactionId`) | both query params supplied |
| 404 | ENTITY_NOT_FOUND | unknown id |
| 401/403 | — | auth |

**`GET /entity/{entityName}/{modelVersion}` (getAllEntities)**

| Status | Code | Producing scenario |
|---|---|---|
| 200 | — | list (now honouring `pointInTime` ✎) |
| 400 | — | invalid page params |
| 404 | MODEL_NOT_FOUND | unknown model |
| 401/403 | — | auth |

**`DELETE /entity/{entityName}/{modelVersion}` (deleteEntities)** ✎

| Status | Code | Producing scenario |
|---|---|---|
| 200 | — | delete (all, or condition-scoped ✎); `ids` when `verbose` ✎ |
| 400 | MALFORMED_REQUEST / INVALID_CONDITION ✎ | unparseable body / invalid condition |
| 404 | MODEL_NOT_FOUND | unknown model |
| 401/403 | — | auth |

**`POST /entity/{format}/{entityName}/{modelVersion}` (create)** and
**`POST|PUT /entity/{format}` (createCollection / updateCollection)**

| Status | Code | Producing scenario |
|---|---|---|
| 200 | — | success |
| 400 | MALFORMED_REQUEST / INCOMPATIBLE_TYPE / POLYMORPHIC_SLOT | bad body shape ✎ / type mismatch |
| 409 | UNIQUE_VIOLATION ✎ | composite-unique-key conflict |
| 422 | INVALID_UNIQUE_KEY / INVALID_UNIQUE_KEY_DEFINITION ✎ | null/invalid unique-key field or definition |
| 404 | MODEL_NOT_FOUND | unknown model (create) |
| 401/403 | — | auth |

**`PUT|PATCH /entity/{format}/{entityId}[/{transition}]` (updateSingle[WithLoopback], patchSingle[WithLoopback])**

| Status | Code | Producing scenario |
|---|---|---|
| 200 | — | success |
| 400 / 404 / 409 / 412 | (existing) | as documented |
| 415 / 428 / 501 | (existing, PATCH) | as documented |
| 422 | INVALID_UNIQUE_KEY[_DEFINITION] ✎ | unique-key violation on the save path |

**`GET /entity/{entityId}/changes` (getEntityChangesMetadata)** — response-shape fixes
(E6/E7/E8); error table unchanged (200/404/401/403).

## 8. Coverage matrix (scenario × layer)

Per `.claude/rules/test-coverage.md`. `✓` required · `—` n/a · `waive` = waived with reason.

| Scenario | Unit | Running-backend e2e | Cross-backend parity | gRPC |
|---|---|---|---|---|
| Conformance core: response validation both directions | ✓ | — | — | — |
| Conformance core: error-code matrix both directions | ✓ | — | — | — |
| E1 meta shape (fields present, no `previousTransition`, `additionalProperties:false`) | — | ✓ | ✓ (backend-agnostic) | verify gRPC GetEntity meta if it returns one; else `waive`: gRPC entity read has no meta envelope |
| E2 conditional delete (matching subset only) | condition→ids selection ✓ | ✓ | ✓ | verify gRPC delete if present; else `waive` |
| E2 `pointInTime` + `verbose` (ids returned) | — | ✓ | ✓ | per above |
| E3 `getAllEntities` as-at `pointInTime` | — | ✓ | ✓ | — |
| E4 request-body shape (array-of-`{model,payload}`) | — | ✓ | ✓ | — |
| E5 unique-key `409`/`422` codes | — | ✓ (per code) | ✓ | ✓ (gRPC error envelope: `Success`,`Error.Code`) |
| E6/E7/E8 changes shape/enum/ordering | — | ✓ (gate-validated) | — | — |

**Concurrency:** none of the entity-slice changes need the isolated concurrency-test
treatment. `deleteEntities` is select-then-delete over a snapshot; parity asserts the
correct subset survives, not an interleave (per `.claude/rules/test-coverage.md`, concurrency
tests stay out of the shared parity suite).

**gRPC applicability** is confirmed per row during implementation; any `waive` records a
one-line reason in the plan (no silent gaps).

### 8.1 Test infrastructure notes
- Core unit tests live beside the core (`internal/e2e/conformance/`), runnable with
  `-short` (no Docker).
- e2e conformance assertions run in the existing `internal/e2e` suite (Postgres testcontainer).
- Backend-agnostic scenarios register in `e2e/parity/registry.go` (picked up by all
  backends incl. the commercial one).

## 9. Documentation & gate obligations

- **Gate 4:** any new error code → `cmd/cyoda/help/content/errors/<CODE>.md`
  (`TestErrCode_Parity` enforces the bijection). E2/E5 introduce/surface
  `INVALID_CONDITION`, `UNIQUE_VIOLATION`, `INVALID_UNIQUE_KEY[_DEFINITION]` at these
  endpoints — ensure topics exist.
- **Gate 7:** `docs/cloud-parity/` entry per reconciled behaviour (E1 meta shape; E2
  conditional-delete conformance; E3 as-at list). Each states the reconciliation direction
  and rationale.
- `api/generated.go` is regenerated (or hand-updated, per the existing oapi-codegen/Go 1.26
  caveat) whenever request/response models change (E1, E4).

## 10. Decomposition — follow-on plans (out of scope here)

Each reuses the Pillar-B loop and the Pillar-A gate:
1. stats / audit / search (fictional 404/408, page-size default, ndjson, `pointInTime`).
2. entity-model & workflow (missing 409/422/404 on lock/unlock/delete/import).
3. auth / OIDC (error-envelope family decision: problem+json vs OAuth `ErrorResponseDto`).
4. message (mis-typed bodies, v1-UUID validation, `ValueMaps`).
5. **dead-surface decision** — implement vs remove the 22 excluded-tag ops + always-501
   stubs from the published contract. Highest single "scale" signal; it's a contract
   decision, so it gets its own brainstorm.

## 11. Risks

- **Spec precision debt:** the gate only guards what the spec declares precisely. Mitigation:
  in-scope schemas are tightened; the matrix explicitly lists out-of-scope ops as `pending`
  so coverage is never overstated.
- **`deleteEntities` blast radius:** implementing conditional delete touches a
  destructive path. Mitigation: reuse the proven `searchEntities` condition primitive,
  backward-compatible empty-body behaviour, parity asserts the surviving subset, and the
  data-loss failure mode is closed by construction (present conditions are honoured).
- **`api/generated.go` regen friction** (known oapi-codegen v2.6.0 / Go 1.26 incompatibility):
  model changes may need the same manual-edit approach used in commit `9c721b4`.
```
