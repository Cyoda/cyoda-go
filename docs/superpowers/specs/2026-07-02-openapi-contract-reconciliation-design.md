# OpenAPI Contract Reconciliation — Phase 0 Gate + Entity Slice

**Date:** 2026-07-02
**Status:** design / awaiting review
**Runs after:** Spec 1 (`2026-07-03-openapi-contract-status-and-parity-design.md`), which
establishes the marker-aware coverage gate (`markedOps`, replacing `knownUncoveredOps`) that
this slice's error-code matrix builds on.
**Reference evidence:** `docs/analysis/openapi/README.md` (full 87-op audit, six-pattern
taxonomy, per-finding reconciliation direction).
**Builds on:** ADR 0003 (`docs/adr/0003-openapi-contract-conformance-and-evolution.md`,
which supersedes 0001 and codifies the reconciliation-direction and typed-but-open policies
applied here), the `internal/e2e/openapivalidator` package, the schema-strictness research
(`docs/analysis/openapi/schema-strictness-research.md`), and the earlier defect-reconciliation
seed (issue #21).

---

## 1. Problem

`api/openapi.yaml` is a deliberate, hand-authored, `//go:embed`'d OpenAPI 3.1 contract —
the canonical common ground across `cyoda-go`, the commercial (Cassandra) plugin,
`cyoda-docs`, and Cyoda Cloud. Per ADR 0001, **code is derived from the spec**, not the
reverse. The audit (`docs/analysis/openapi/README.md`) found ~55 divergences across 87
operations, including a data-loss defect and a 25%-dead published surface. An app-builder
generating a client from the spec reaches materially wrong conclusions — the trigger case:
concluding from `GET /entity/{entityId}` that there is *no way to learn the entity model*,
because `meta` is an opaque bag whose prose names a field (`previousTransition`) that does
not exist.

ADR 0001 already installed the *mechanism* to catch shape drift: runtime `kin-openapi`
response validation at the E2E boundary (`internal/e2e/openapivalidator`, enforce mode).
But two gaps remain: (a) the validator is only as strong as the spec is precise, and the
loose `additionalProperties:true` bags let real drift through; (b) it validates response
*shapes* but not *error-code coverage* (documented-but-fictional codes, emitted-but-
undocumented codes). This effort closes both, and reconciles the entity group under them.

## 2. Reconciliation policy (per ADR 0003)

ADR 0003 (Decision 7) governs: the authored `api/openapi.yaml` is the canonical contract and
source of intent; when spec and server disagree the defect is on whichever side left the
contract, classified per finding — `spec-stale` (fix spec), `spec-incomplete` (enrich spec),
`server-gap` (fix server), `needs-decision` (record, then reclassify). This spec applies that
policy to the entity group.

A mechanical "sync spec→server" is wrong here: ~⅓ of audit findings are
`server-gap`/`needs-decision` (e.g. syncing would turn the `deleteEntities` data-loss bug into
a documented feature). Some entity-domain code comments assert "the server is the source of
truth" (`entity_conformance_test.go:6`, `internal/domain/entity/handler.go:601`) — inherited
from the issue #21 defect-reconciliation, where the server happened to be the intended one;
this slice corrects those comments to the per-finding policy above (Gate 6).

## 3. Goals / Non-goals

**Goals**
- Extend the existing conformance validator with **error-code-coverage** enforcement and
  **tighten** the entity-group schemas so drift there fails CI.
- Establish a repeatable **reconciliation loop** (classify → fix losing side → lock → record)
  every follow-on group reuses.
- Apply both to the **entity group** end-to-end as the proving vertical, including
  implementing the documented conditional `deleteEntities`.

**Non-goals (this spec)**
- Reconciling the other five groups (stats/audit/search, model, auth/oidc, message).
- The dead-surface disposition (22 excluded-tag ops + always-501 stubs).
- A strict-typing migration or extracting a portable cross-repo conformance kit — both are
  explicitly out (ADR 0001 defers the former; the latter is speculative generality).
- Changing the authoring model: the spec stays hand-authored and canonical.

## 4. Pillar A — extend the existing conformance validator

**Do not build a parallel system.** `internal/e2e/openapivalidator` already loads
`api/openapi.yaml` via kin-openapi, validates every E2E response body + status against the
operation schema, runs in `ModeEnforce`, accumulates a `collector`, emits a `report`, and
tracks operation/status coverage (failing on uncovered *in-scope* ops via
`zzz_openapi_conformance_test.go`'s `knownUncoveredOps`). ADR 0001 governs it.

The **only genuinely new capability** is error-code coverage at **errorCode-string
granularity** (the existing coverage is operation/status granularity). Scope Pillar A to
exactly that delta plus schema tightening:

### 4.1 Error-code matrix (extension of the collector)
- Extend the `openapivalidator` collector to record, per response, the
  `(operationId, status, errorCode)` triple. The `errorCode` is read from the response
  body's `properties.errorCode` (the `ProblemDetail` shape produced by
  `common.WriteError`); absent for success responses.
- Add a static **`ErrorCodeMatrix`**: `operationId → {documented (status, errorCode)}`,
  authored from the spec's per-endpoint error tables (§7), scoped to the **entity group**
  in this spec (out-of-scope ops are exempted via the **marker-aware coverage gate Spec 1
  establishes** — `x-cyoda-status` / the `markedOps` set — which replaces the retired
  `knownUncoveredOps`; this slice runs *after* Spec 1).
- Two aggregate checks at suite end (bidirectional drift detection):
  - **producible** — every documented `(status, errorCode)` in scope was observed by ≥1
    E2E (catches *fictional* codes, audit pattern P5).
  - **declared** — no in-scope E2E produced a `(status, errorCode)` absent from the matrix
    (catches *missing* codes, P5).

### 4.2 Schema tightening — typed-but-open (per ADR 0003)
The validator passes anything against an `additionalProperties:true` bag. For each
entity-group finding we make the schema precise **without sealing it**: enumerate every
property the server emits (correct types, required vs optional), but leave
`additionalProperties` open (absent/`true`), never `false`. The enforce-mode validator then
fails when a documented field is missing or mistyped, while a future additive field still
validates — the binding is on the known shape, not on rejecting extras. A composed (`allOf`)
schema that must ever close uses `unevaluatedProperties:false`, never `additionalProperties:false`
on the base. No new validation engine — we feed the existing one a precise-but-open schema.

### 4.3 Seed cleanup (Gate 6)
Once `meta`/envelope are tightened under enforce mode, most hand-parsed key assertions in
`entity_conformance_test.go` become redundant (the ambient validator enforces them). Thin
that seed rather than migrating it into a new package; correct its "server is source of
truth" comment per §2.

### 4.4 What the validator does *not* do
- It does not validate the polymorphic entity `data` payload — that stays
  `additionalProperties:true` by contract. Confirmed separable in code: `EntityEnvelope`
  has `Data any` cleanly split from `Meta` (`internal/domain/entity/service.go:64-69`). We
  tighten the **envelope/meta**, never `data`.
- It remains only as strong as the spec is precise; still-loose out-of-scope schemas pass
  permissively until their own plans tighten them (must not false-fail).

## 5. Pillar B — the reconciliation loop

For each finding: **classify direction** (§2) → **fix the losing side** via red/green TDD
(Gate 1) → **lock it** (tighten schema and/or add the matrix entry) → **record the contract
decision** in `docs/cloud-parity/` (Gate 7).

**Tightening policy (typed-but-open, per ADR 0003):** enumerate every known property
(correct types, required vs optional) but do **not** seal — `additionalProperties` stays open,
never `false`. The validator binds the known shape (missing/mistyped documented fields fail)
while additive fields stay non-breaking. Genuinely open values (entity `data`, error extension
bags) stay open by design. Discipline against breaking edits (sealing, field removal, type
narrowing) is an **additive-only CI gate** on `api/openapi.yaml` (ADR 0003, Decision 6), not
schema sealing.

**Canonical-schema reconciliation:** several entity wire shapes have a *canonical* JSON
schema under `docs/cyoda/schema/common/` (embedded, consumed by cyoda-docs/Cloud) **and** a
mirror in `e2e/parity/client/types.go`. Fixes to these shapes must reconcile **all**
definitions (canonical JSON schema ↔ OpenAPI ↔ parity client), not author a fourth. The
`e2e/parity/client/types.go` mirror updates whenever a corrected wire shape changes it (it is
the commercial plugin's import surface).

## 6. The entity slice — findings, directions, fixes

| # | Finding | Direction | Fix | Lock |
|---|---|---|---|---|
| E1 | `Envelope.meta` opaque bag + `previousTransition` fossil (`getOneEntity`, `getAllEntities`) | spec-incomplete + spec-stale | Add an `EntityMeta` OpenAPI schema **mirroring the canonical `docs/cyoda/schema/common/EntityMetadata.json`**: required `{id, state, creationDate, lastUpdateTime}`; **optional** `{modelKey, transactionId, transitionForLatestSave, pointInTime}`; **typed-but-open** (`additionalProperties` absent, never `false`); delete `previousTransition`; fix examples. Reconcile OpenAPI ↔ canonical JSON ↔ `e2e/parity/client/types.go` (S5). | enforce-mode response validation on both ops |
| E2 | `deleteEntities` ignores condition body + `pointInTime` + `verbose`; wipes whole model | server-gap (data loss) | **Implement** the documented contract (§6.1) | e2e (subset survives) + parity (inside tx) + matrix |
| E3 | `getAllEntities` ignores `pointInTime` | server-gap | Plumb `pointInTime` into the list read via **`GetAllAsAt`** (the model-scoped list-PIT primitive in all 3 backends, already used by search at `internal/domain/search/service.go:161`) — **not** `getOneEntity`'s single-entity `GetAsAt`. Also populate `meta.pointInTime` (E1). | as-at e2e + parity |
| E4 | `createCollection` / `create` request body `type:object` erases real shape | spec-incomplete | Tighten request schemas to the array-of-`{model{name,version},payload}` shape; document the batch/array `create` form. Reconcile `e2e/parity/client/types.go` (S5). | request validation |
| E5 | create/update/patch/collection composite-unique-key `409`/`422` codes undocumented | spec-incomplete | Add codes to the endpoints' error tables | error-code matrix (producible+declared) |
| E6 | `EntityChangeMeta.fieldsChangedCount` advertised (in **canonical** `EntityChangeMeta.json`), never emitted | **needs-decision (deferred)** | **Out of this slice** — Cloud-fact-blocked; §6.2 | — |
| E7 | `getEntityChangesMetadata` ordering prose says chronological; server is newest-first | spec-stale | Fix prose to reverse-chronological | (doc-only) |
| E8 | `changeType` spelling diverges across surfaces: canonical `EntityChangeMeta.json` `CREATE/UPDATE/DELETE`, OpenAPI+HTTP `CREATED/UPDATED/DELETED`, **gRPC `CREATE/UPDATE/DELETE`** (`mapChangeType`, `grpc/search.go:591`) | **needs-decision** | Decide canonical spelling, then align canonical JSON ↔ OpenAPI ↔ HTTP ↔ gRPC (§6.3) | response validation (enum) + gRPC |
| E9 | `getOneEntity` 200 example shows only `{id,state}` | spec-incomplete | Enrich example to full meta shape | (validated structurally by E1) |

### 6.1 `deleteEntities` — implementing conditional delete

Principle (no special engine rights — reuse existing primitives): **select-then-delete.**
Evaluate the `AbstractConditionDto` to obtain matching entity ids (at `pointInTime` when
supplied), then delete those ids; return them when `verbose`.

Feasibility confirmed against code, with three implementation realities the plan must name:
- **Reuse primitives:** `predicate.ParseCondition` parses the body; `match.Match(...)` is a
  free function (`internal/match/match.go:17`); `SearchService.Search` returns `[]*spi.Entity`
  with ids off `e.Meta.ID` and honours `pointInTime`. Selection reuses these.
- **Handler wiring task (name it):** `entity.Handler` currently holds `StoreFactory` but not
  search/match — wiring them in is mechanical but is a real task, not free.
- **Transaction visibility (parity implication):** `SearchService.Search` bypasses backend
  pushdown when a tx is on the context (`internal/domain/search/service.go:140-141`), and
  delete runs *inside* a tx. Buffered-write visibility therefore differs by backend, so the
  cross-backend parity test must exercise conditional delete **inside the tx** — parity is
  *not* automatic. (Removes the earlier "parity is free" over-claim.)
- **No batch delete:** `EntityStore` has no `DeleteByIDs` (SPI `persistence.go`); a per-id
  `Delete` loop is required.
- **Backward compatible:** absent/empty body ⇒ delete-all (today's behaviour, preserves
  `TestDeleteEntities_ResponseShape`); a present condition scopes the delete; `verbose`
  toggles id return. Data loss is closed by construction — present conditions are honoured,
  never ignored.
- **gRPC:** see §8 / decision D2.

### 6.2 E6 `fieldsChangedCount` — deferred (Cloud-fact-blocked)
The field is declared in the **canonical, Cloud-consumed** `EntityChangeMeta.json` but the
cyoda-go server never emits it (`internal/domain/entity/handler.go:576-587`). Direction is
genuinely ambiguous — (a) `server-gap`: implement emission (compute the changed-field count);
(b) `spec-stale`: remove it from the canonical schema (risky if Cloud emits it). Resolving it
needs a Cloud-parity fact we do not yet have: **does Cloud emit `fieldsChangedCount`?**
**This slice does not implement E6** — a `needs-decision` must not drive net-new speculative
server code. E6 is recorded as an open question in `docs/cloud-parity/` and decided once the
Cloud fact is gathered; the leaning is (a) implement (removing a Cloud-consumed canonical field
is the higher-risk direction).

### 6.3 HTTP ↔ gRPC divergences to reconcile
Two entity shapes already differ between the HTTP and gRPC entry points; typed-but-open schemas
would *mask* these, so — per the project's "an entry-point divergence is a bug" stance — the
slice reconciles them explicitly rather than letting the open schema hide them:
- **`changeType` spelling (E8):** HTTP emits `CREATED/UPDATED/DELETED`; gRPC emits
  `CREATE/UPDATE/DELETE` (`mapChangeType`, `internal/grpc/search.go:591`); the canonical
  `EntityChangeMeta.json` sides with gRPC. Decide one spelling and align all four surfaces.
- **`meta` fields:** HTTP `meta` includes `modelKey` (and, after E3, `pointInTime`); gRPC
  `buildEntityMeta` (`internal/grpc/search.go:577-589`) omits both. Add `modelKey` and
  `pointInTime` to the gRPC builder so both entry points emit the same metadata (E1 fixes the
  HTTP side).

## 7. Per-endpoint error/status tables (in-scope endpoints)

Authored into the `ErrorCodeMatrix`. `✎` = added/changed by this spec.

**`GET /entity/{entityId}` (getOneEntity)** — 200; 400 (both `pointInTime`+`transactionId`);
404 ENTITY_NOT_FOUND; 401/403.

**`GET /entity/{entityName}/{modelVersion}` (getAllEntities)** — 200 (now honouring
`pointInTime` ✎); 400 invalid page; 404 MODEL_NOT_FOUND; 401/403.

**`DELETE /entity/{entityName}/{modelVersion}` (deleteEntities)** ✎ — 200 (all, or
condition-scoped ✎; `ids` when `verbose` ✎); 400 MALFORMED_REQUEST / INVALID_CONDITION ✎;
404 MODEL_NOT_FOUND; 401/403.

**`POST /entity/{format}/{entityName}/{modelVersion}` (create)**,
**`POST|PUT /entity/{format}` (createCollection / updateCollection)** — 200; 400
MALFORMED_REQUEST / INCOMPATIBLE_TYPE / POLYMORPHIC_SLOT (bad body shape ✎); 409
UNIQUE_VIOLATION ✎; 422 INVALID_UNIQUE_KEY / INVALID_UNIQUE_KEY_DEFINITION ✎; 404
MODEL_NOT_FOUND (create); 401/403.

**`PUT|PATCH /entity/{format}/{entityId}[/{transition}]`** — 200; 400/404/409/412 existing;
415/428/501 existing (PATCH); 422 INVALID_UNIQUE_KEY[_DEFINITION] ✎.

**`GET /entity/{entityId}/changes` (getEntityChangesMetadata)** — response-shape fixes
(E7/E8; E6 deferred); error table unchanged (200/404/401/403).

## 8. Coverage matrix (scenario × layer)

Per `.claude/rules/test-coverage.md`. HTTP and gRPC are **separate entry points — both
covered** (no false waivers; the gRPC meta and delete surfaces both exist).

| Scenario | Unit | Running-backend e2e | Cross-backend parity | gRPC |
|---|---|---|---|---|
| Error-code matrix (producible + declared), entity scope | ✓ (collector logic) | ✓ (aggregate) | — | — |
| E1 meta shape (mirror canonical; typed-but-open — no `additionalProperties:false`; no `previousTransition`) | — | ✓ | ✓ (backend-agnostic) | ✓ — reconcile gRPC `buildEntityMeta` (`internal/grpc/search.go:577`) to add `modelKey`+`pointInTime` (§6.3), then assert it matches the tightened shape |
| E2 conditional delete (matching subset only) | condition→ids selection ✓ | ✓ | ✓ (inside tx — §6.1) | gRPC stays unconditional (D2 deferred, §8) |
| E2 `pointInTime` + `verbose` (ids returned) | — | ✓ | ✓ | — |
| E3 `getAllEntities` as-at (`GetAllAsAt`) | — | ✓ | ✓ | — |
| E4 request-body shape (array-of-`{model,payload}`) | — | ✓ | ✓ | — |
| E5 unique-key `409`/`422` codes | — | ✓ (per code) | ✓ | ✓ — assert envelope; note N1: gRPC `Error.Code` is a coarse CLIENT/SERVER bucket, the unique-key code is in `Error.Message` (`internal/grpc/errors.go`), matching `TestRPC_EntityCreate_UniqueViolation` (`rpc_test.go:654`) |
| E8 `changeType` spelling aligned across HTTP + gRPC (§6.3) | — | ✓ (gate-validated enum) | — | ✓ (gRPC enum) |
| E7 changes ordering | — | ✓ (gate-validated) | — | — |

**Decision D2 — gRPC conditional delete (deferred, Cloud-fact-blocked).** gRPC exposes
`EntityDeleteAllRequest` (unconditional; `internal/grpc/entity.go:391`) and per-entity
`EntityDeleteRequest`, but no *conditional* delete. The OpenAPI conditional-delete is an
**HTTP-contract** feature; the gRPC/proto contract does not promise it. **This slice implements
E2 on HTTP only**; gRPC `DeleteAll` stays unconditional and gets test coverage for its existing
behaviour (so the gRPC entry point is covered, not waived). Whether Cloud expects gRPC
conditional-delete parity is an **open question recorded in `docs/cloud-parity/`**, decided when
the Cloud fact is gathered (Gate 7 / multi-node).

**Concurrency:** entity-slice changes need no isolated concurrency test; `deleteEntities` is
select-then-delete over a snapshot; parity asserts the surviving subset, not an interleave.

## 9. Documentation & gate obligations

- **Gate 4:** new error codes → `cmd/cyoda/help/content/errors/<CODE>.md`
  (`TestErrCode_Parity` bijection). E2/E5 surface `INVALID_CONDITION`, `UNIQUE_VIOLATION`,
  `INVALID_UNIQUE_KEY[_DEFINITION]` at these endpoints.
- **Gate 7:** `docs/cloud-parity/` entry per reconciled behaviour (E1 meta; E2 conditional
  delete; E3 as-at; E8 `changeType` spelling) — each states direction + rationale — **plus two
  open questions** (E6 `fieldsChangedCount` direction; D2 gRPC conditional-delete parity),
  decided when the Cloud facts are gathered.
- **Canonical schemas / parity mirror:** reconcile `docs/cyoda/schema/common/EntityMetadata.json`
  (E1) and `EntityChangeMeta.json` (E8 `changeType`; E6 deferred) with the OpenAPI, and update
  `e2e/parity/client/types.go` (S5) for E1/E4 wire-shape changes.
- **Codegen:** `api/generated.go` regenerated (or hand-updated) on model changes (E1, E4).
  Note: tree is on **oapi-codegen v2.7.0** with a `//go:generate` directive
  (`api/generate.go`); re-verify whether the Go 1.26 regen caveat still applies before relying
  on `go generate`.
- **ADR:** governed by **ADR 0003** (typed-but-open, additive-only gate, reconciliation
  direction); ADR 0003 supersedes 0001.

## 10. Decomposition — follow-on plans (out of scope here)

Each reuses the Pillar-B loop and the extended validator:
1. stats / audit / search · 2. entity-model & workflow · 3. auth / OIDC (error-envelope
family decision) · 4. message · 5. **dead-surface disposition** — Spec 1 already assigns each
not-live op a machine-readable `x-cyoda-status` (SQL-Schema `planned`/Trino, Stream Data
`unimplemented`, IAM/OIDC stubs `planned`); the eventual *implement-vs-remove* call for the
`unimplemented` set remains a follow-on decision.

## 11. Risks

- **Spec precision debt:** the validator only guards precisely-declared schemas. Mitigation:
  in-scope schemas tightened; out-of-scope coverage is exempted via Spec 1's marker-aware gate
  (`markedOps`), so it is never overstated.
- **`deleteEntities` blast radius:** destructive path. Mitigation: reuse the proven search
  condition primitive, backward-compatible empty-body behaviour, parity asserts the surviving
  subset **inside the tx** (§6.1), per-id delete loop, data-loss closed by construction.
- **Canonical-schema coupling:** E1/E6 touch embedded, Cloud-consumed schemas; changes must
  reconcile all mirrors (§5) and be recorded in cloud-parity — mishandling risks silent
  cross-repo drift.
- **Two deferred decisions (E6 `fieldsChangedCount`, D2 gRPC conditional-delete parity):**
  both Cloud-fact-blocked and out of this slice (§6.2, §8); recorded as `docs/cloud-parity/`
  open questions, decided when the Cloud facts are gathered.
