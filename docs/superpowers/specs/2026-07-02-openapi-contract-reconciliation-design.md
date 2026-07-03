# OpenAPI Contract Reconciliation — Phase 0 Gate + Entity Slice

**Date:** 2026-07-02
**Status:** design / awaiting review
**Reference evidence:** `docs/analysis/openapi/README.md` (full 87-op audit, six-pattern
taxonomy, per-finding reconciliation direction).
**Builds on:** ADR 0001 (`docs/adr/0001-openapi-server-spec-conformance.md`), the
`internal/e2e/openapivalidator` package, and the #21 reconciliation seed.

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

## 2. Reconciliation policy (refines the #21 "server is source of truth" framing)

The #21 seed carries "server is source of truth" language (`entity_conformance_test.go:6`,
`internal/domain/entity/handler.go:601` "Server is source of truth per design §3"). That
was correct for the four #21 defects, where the server's behaviour was the intended one and
the spec was stale. But it is **not** a universal reconciliation rule, and stating it as one
contradicts ADR 0001's "spec is canonical."

**Refined policy (this effort):** the *authored spec is the canonical contract and the
source of intent*; when spec and server disagree it is a **defect on whichever side left
the contract**, decided per finding:

- `spec-stale` — server evolved correctly; fix the spec.
- `spec-incomplete` — server is right and intended; enrich the spec.
- `server-gap` — the spec expresses intended contract the server never implemented; fix the
  **server**.
- `needs-decision` — genuine ambiguity; record the decision, then it becomes one of the
  above.

~⅓ of audit findings are `server-gap`/`needs-decision`, so a mechanical "sync spec→server"
would *destroy* contract intent (it would turn the `deleteEntities` data-loss bug into a
documented feature). This policy refinement is itself recorded (Gate 6): the stale
"server is source of truth" comments in the seed are corrected to point here as part of the
entity slice.

**Relationship to ADR 0001:** this effort stays within ADR 0001 (runtime validation, no
strict-typing migration). It does not supersede it. It *does* move us toward one of ADR
0001's stated reconsideration triggers (Cloud parity as a first-class argument); if that
trigger fires later it gets its own superseding ADR — out of scope here.

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
  in this spec (other ops listed as pending, reusing the existing `knownUncoveredOps`
  mechanism — do not introduce a second pending list).
- Two aggregate checks at suite end (bidirectional drift detection):
  - **producible** — every documented `(status, errorCode)` in scope was observed by ≥1
    E2E (catches *fictional* codes, audit pattern P5).
  - **declared** — no in-scope E2E produced a `(status, errorCode)` absent from the matrix
    (catches *missing* codes, P5).

### 4.2 Schema tightening (what makes the existing validator bite)
The validator passes anything against `additionalProperties:true`. For each entity-group
finding we tighten the relevant schema (add `properties`, set `additionalProperties:false`,
correct types) so the *already-present* enforce-mode validator now fails on regression. No
new validation engine — we feed the existing one a precise schema.

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

**Tightening policy:** tightened response objects use declared `properties` +
`additionalProperties:false`. Consequence: adding a new field later fails CI until the spec
is updated. In a spec-first world that is the intended binding.

**Canonical-schema reconciliation:** several entity wire shapes have a *canonical* JSON
schema under `docs/cyoda/schema/common/` (embedded, consumed by cyoda-docs/Cloud) **and** a
mirror in `e2e/parity/client/types.go`. Fixes to these shapes must reconcile **all**
definitions (canonical JSON schema ↔ OpenAPI ↔ parity client), not author a fourth. ADR
0001 Consequences explicitly flags the `e2e/parity/client/types.go` update (it is the
commercial plugin's import surface).

## 6. The entity slice — findings, directions, fixes

| # | Finding | Direction | Fix | Lock |
|---|---|---|---|---|
| E1 | `Envelope.meta` opaque bag + `previousTransition` fossil (`getOneEntity`, `getAllEntities`) | spec-incomplete + spec-stale | Add an `EntityMeta` OpenAPI schema **mirroring the canonical `docs/cyoda/schema/common/EntityMetadata.json`**: required `{id, state, creationDate, lastUpdateTime}`; **optional** `{modelKey, transactionId, transitionForLatestSave, pointInTime}`; `additionalProperties:false`; delete `previousTransition`; fix examples. Reconcile OpenAPI ↔ canonical JSON ↔ `e2e/parity/client/types.go` (S5). | enforce-mode response validation on both ops |
| E2 | `deleteEntities` ignores condition body + `pointInTime` + `verbose`; wipes whole model | server-gap (data loss) | **Implement** the documented contract (§6.1) | e2e (subset survives) + parity (inside tx) + matrix |
| E3 | `getAllEntities` ignores `pointInTime` | server-gap | Plumb `pointInTime` into the list read via **`GetAllAsAt`** (the model-scoped list-PIT primitive in all 3 backends, already used by search at `internal/domain/search/service.go:161`) — **not** `getOneEntity`'s single-entity `GetAsAt`. Also populate `meta.pointInTime` (E1). | as-at e2e + parity |
| E4 | `createCollection` / `create` request body `type:object` erases real shape | spec-incomplete | Tighten request schemas to the array-of-`{model{name,version},payload}` shape; document the batch/array `create` form. Reconcile `e2e/parity/client/types.go` (S5). | request validation |
| E5 | create/update/patch/collection composite-unique-key `409`/`422` codes undocumented | spec-incomplete | Add codes to the endpoints' error tables | error-code matrix (producible+declared) |
| E6 | `EntityChangeMeta.fieldsChangedCount` advertised (in **canonical** `docs/cyoda/schema/common/EntityChangeMeta.json`), never emitted by server | **needs-decision** | See §6.2 | per decision |
| E7 | `getEntityChangesMetadata` ordering prose says chronological; server is newest-first | spec-stale | Fix prose to reverse-chronological | (doc-only) |
| E8 | `changeType` prose `CREATE/UPDATE/DELETE` vs enum/emit `CREATED/UPDATED/DELETED` | spec-stale | Fix prose to match enum | response validation (enum) |
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

### 6.2 E6 `fieldsChangedCount` — the decision
The field is declared in the **canonical, Cloud-consumed** `EntityChangeMeta.json`, but the
cyoda-go server never emits it (`internal/domain/entity/handler.go:576-587`). Direction is
genuinely ambiguous:
- **(a) server-gap** — the contract intends it; **implement** emission (compute the
  changed-field count on the change path). Keeps the canonical schema; small server addition.
- **(b) spec-stale** — remove it from the canonical schema. **But** this is a Cloud-parity
  contract change to an embedded, downstream-consumed CloudEvents schema; risky if Cloud
  already emits it.

**Recommendation:** (a) implement server-side — removing a field from a Cloud-consumed
canonical schema is the higher-risk direction, and the field carries real audit-UI value. To
be confirmed with the Cloud-parity check (does Cloud emit it?) before finalizing. Recorded in
`docs/cloud-parity/`.

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
(E6/E7/E8); error table unchanged (200/404/401/403).

## 8. Coverage matrix (scenario × layer)

Per `.claude/rules/test-coverage.md`. HTTP and gRPC are **separate entry points — both
covered** (no false waivers; the gRPC meta and delete surfaces both exist).

| Scenario | Unit | Running-backend e2e | Cross-backend parity | gRPC |
|---|---|---|---|---|
| Error-code matrix (producible + declared), entity scope | ✓ (collector logic) | ✓ (aggregate) | — | — |
| E1 meta shape (mirror canonical; `additionalProperties:false`; no `previousTransition`) | — | ✓ | ✓ (backend-agnostic) | ✓ — assert `buildEntityMeta` output (`internal/grpc/search.go:377,577`) matches the tightened shape |
| E2 conditional delete (matching subset only) | condition→ids selection ✓ | ✓ | ✓ (inside tx — §6.1) | per decision D2 |
| E2 `pointInTime` + `verbose` (ids returned) | — | ✓ | ✓ | per D2 |
| E3 `getAllEntities` as-at (`GetAllAsAt`) | — | ✓ | ✓ | — |
| E4 request-body shape (array-of-`{model,payload}`) | — | ✓ | ✓ | — |
| E5 unique-key `409`/`422` codes | — | ✓ (per code) | ✓ | ✓ — assert envelope; note N1: gRPC `Error.Code` is a coarse CLIENT/SERVER bucket, the unique-key code is in `Error.Message` (`internal/grpc/errors.go`), matching `TestRPC_EntityCreate_UniqueViolation` (`rpc_test.go:654`) |
| E6 `fieldsChangedCount` (per §6.2 decision) | — | ✓ | ✓ | ✓ if emitted on gRPC change path |
| E7/E8 changes ordering/enum | — | ✓ (gate-validated) | — | — |

**Decision D2 — gRPC conditional delete.** gRPC exposes `EntityDeleteAllRequest`
(unconditional; `internal/grpc/entity.go:391`) and per-entity `EntityDeleteRequest`, but no
*conditional* delete. The OpenAPI conditional-delete is an **HTTP-contract** feature; the
gRPC/proto contract does not promise conditional delete. **Recommendation:** implement E2 on
HTTP only; keep gRPC `DeleteAll` as-is (it conforms to its own proto contract), and add/verify
gRPC test coverage for the existing `DeleteAll` behaviour so the gRPC entry point is covered,
not waived. Record the HTTP-only scope in `docs/cloud-parity/`. (Confirm with Cloud that gRPC
parity is not expected before finalizing — Gate 7 / multi-node.)

**Concurrency:** entity-slice changes need no isolated concurrency test; `deleteEntities` is
select-then-delete over a snapshot; parity asserts the surviving subset, not an interleave.

## 9. Documentation & gate obligations

- **Gate 4:** new error codes → `cmd/cyoda/help/content/errors/<CODE>.md`
  (`TestErrCode_Parity` bijection). E2/E5 surface `INVALID_CONDITION`, `UNIQUE_VIOLATION`,
  `INVALID_UNIQUE_KEY[_DEFINITION]` at these endpoints.
- **Gate 7:** `docs/cloud-parity/` entry per reconciled behaviour (E1 meta; E2 conditional-
  delete + D2 gRPC scope; E3 as-at; E6 decision). Each states direction + rationale.
- **Canonical schemas / parity mirror:** reconcile `docs/cyoda/schema/common/EntityMetadata.json`
  (E1) and `EntityChangeMeta.json` (E6) with the OpenAPI, and update `e2e/parity/client/types.go`
  (S5) for E1/E4 wire-shape changes.
- **Codegen:** `api/generated.go` regenerated (or hand-updated) on model changes (E1, E4).
  Note: tree is on **oapi-codegen v2.7.0** with a `//go:generate` directive
  (`api/generate.go`); re-verify whether the Go 1.26 regen caveat still applies before relying
  on `go generate`.
- **ADR:** reference ADR 0001; no superseding ADR needed (we stay within it).

## 10. Decomposition — follow-on plans (out of scope here)

Each reuses the Pillar-B loop and the extended validator:
1. stats / audit / search · 2. entity-model & workflow · 3. auth / OIDC (error-envelope
family decision) · 4. message · 5. **dead-surface decision** (implement vs remove 22
excluded-tag ops + always-501 stubs — its own brainstorm; highest single scale signal).

## 11. Risks

- **Spec precision debt:** the validator only guards precisely-declared schemas. Mitigation:
  in-scope schemas tightened; matrix reuses `knownUncoveredOps` so out-of-scope coverage is
  never overstated.
- **`deleteEntities` blast radius:** destructive path. Mitigation: reuse the proven search
  condition primitive, backward-compatible empty-body behaviour, parity asserts the surviving
  subset **inside the tx** (§6.1), per-id delete loop, data-loss closed by construction.
- **Canonical-schema coupling:** E1/E6 touch embedded, Cloud-consumed schemas; changes must
  reconcile all mirrors (§5) and be recorded in cloud-parity — mishandling risks silent
  cross-repo drift.
- **Two open decisions (E6 direction, D2 gRPC scope):** recommendations given; confirm the
  Cloud-parity facts before finalizing.
