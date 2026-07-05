# OpenAPI entity-model & workflow reconciliation — design

Reconciles `api/openapi.yaml` with the actual server for the entity-model and
workflow domain (`internal/domain/{model,workflow}/*`, `internal/grpc/model.go`).
Follow-on group 2 of the OpenAPI reconciliation effort (issue #369). Governed by
[ADR 0003](../../adr/0003-openapi-contract-conformance-and-evolution.md) and the
typed-but-open schema policy (`docs/analysis/openapi/schema-strictness-research.md`).
Source findings: `docs/analysis/openapi/README.md` §6C.

Prior merged slices this reuses the machinery of:
`2026-07-02-openapi-contract-reconciliation-design.md` (entity),
`2026-07-04-openapi-stats-audit-search-design.md` (stats/audit/search).

## 1. Scope

All of §6C in one PR to `release/v0.8.2`: the 11 operations under the
`Entity model` / `Entity Model` / `Entity Model, Workflow` tags.

This slice is **almost pure spec-documentation reconciliation** — every error
code is already emitted, correctly named, and already has a
`cmd/cyoda/help/content/errors/<CODE>.md` topic — with **one deliberate runtime
behaviour change** (Design area 1). No new error code is introduced, so no new
help topic and no `TestErrCode_Parity` delta.

**Explicitly out of scope (stated so reviewers don't re-litigate):**
`getAvailableEntityModels` (200 + 5xx only, no 4xx surface) and
`validateEntityModel` (400 parse + 404 already documented; validation *failure*
is a `200 {success:false}`, correctly modelled — not an error code). Both are
already faithful.

## 2. Design area 1 — `deleteEntityModel` enforces UNLOCKED state (server-gap)

### 2.1 The gap
`api/openapi.yaml` describes `deleteEntityModel` as requiring the model be "in
UNLOCKED state", but `service.DeleteModel` never inspects `desc.State`. A LOCKED
model with zero entities deletes successfully — the description is false and a
documented precondition is unenforced.

### 2.2 The fix
`DeleteModel` gains a lock-state check that returns `409` with the existing
`MODEL_ALREADY_LOCKED` code (reused, not minted — `setEntityModelUniqueKeys`
already uses `MODEL_ALREADY_LOCKED` to mean "operation refused because the model
is locked", `service.go:571`). The delete path carries a **delete-specific
message** (`cannot delete entityModel{...}: expectedState=UNLOCKED,
actualState=LOCKED`), not a literal "already locked" string.

Both entry points inherit this via the shared service method:
- HTTP `deleteEntityModel` (`handler.go` → `DeleteModel`)
- gRPC `EntityModelDeleteRequest` (`grpc/model.go:handleModelDelete` → `DeleteModel`;
  `modelDeleteError` → `buildErrorFields` passes the AppError through generically).

### 2.3 Check ordering
`404 (not-found)` → `409 MODEL_ALREADY_LOCKED (lock)` → `409 MODEL_HAS_ENTITIES (count)`.

Lock-before-count is required for coherence: for a locked-model-with-entities,
count-first would emit `MODEL_HAS_ENTITIES` on a *locked* model — contradicting
the "must be UNLOCKED" contract and sending the caller on a two-round-trip dead
end (delete entities → retry → now `MODEL_ALREADY_LOCKED`). Lock-first names the
first gate; the caller unlocks (which itself forces clearing entities) then deletes.

### 2.4 `MODEL_HAS_ENTITIES`-on-delete is retained (not dead code)
After 2.2, is the count-check reachable? Entity creation hard-requires a LOCKED
model (`service.go:220-222` single, `:1094-1096` bulk → `MODEL_NOT_LOCKED`), and
unlock refuses when entities exist (`service.go:341-352`). So through any
single-node API sequence the invariant **unlocked ⟹ 0 entities** holds, and a
locked-model-with-entities now hits the lock-check first — making the count
branch unreachable *single-node*.

It is retained because it **is** constructible, and guarding it matters:
- **Multi-node TOCTOU** (this repo is primarily multi-node — see
  `.claude/rules/multi-node-primary.md`): a concurrent create (reads
  `state=LOCKED`) and unlock (reads `count=0`, create uncommitted) can both
  commit, yielding an unlocked model holding an entity. The delete count-check is
  the backstop for that window.
- **Service/store layer** directly (a unit test can `Save` an unlocked
  descriptor and insert an entity).

Per Gate 6 this is a live invariant, not dead code — keep it. Cover it with a
**service-layer unit test** (construct unlocked+entities directly) and **waive
the running-backend cell** in the coverage matrix.

### 2.5 Description reconciliation
The "Model must be in UNLOCKED state" prose becomes **true** after 2.2. Keep it;
add the `409 MODEL_ALREADY_LOCKED` response documenting the enforced precondition.

## 3. Design area 2 — converter `400` documentation

Both `converter` params advertise an enum and reject unsupported values with
`400 BAD_REQUEST`, but they are **asymmetric** and only one is e2e-producible.

- **`importEntityModel`** — enum `[SAMPLE_DATA, JSON_SCHEMA, SIMPLE_VIEW]`
  (`openapi.yaml:3972`); server accepts **only** `SAMPLE_DATA` (`service.go:94`).
  So `JSON_SCHEMA`/`SIMPLE_VIEW` are **in-enum but server-rejected** → the `400
  BAD_REQUEST "unsupported import converter"` is producible on a valid path (route
  matches, server 400s). Keep the enum (Paul's decision: retains the
  planned-converter signal); **add the `400`**; cover with a running-backend e2e
  test sending `converter=JSON_SCHEMA`.
- **`exportMetadata`** — enum `[JSON_SCHEMA, SIMPLE_VIEW]` (`openapi.yaml:3829`);
  server accepts **both** and rejects anything else (`service.go:209-216`). No
  in-enum value is rejected, so the `400 BAD_REQUEST "unsupported export
  converter"` is reachable **only via an out-of-enum path segment**, which the
  e2e conformance route-matcher (`internal/e2e/openapivalidator`, ModeEnforce)
  rejects as `<unmatched>`. Keep the enum accurate (do not widen it just to make a
  negative testable — that would advertise a rejected value); **keep the `400`
  documented**, replace its fabricated example (§5), cover with a **service/handler
  unit test**, and **waive** the running-backend cell (rationale: enum-guarded;
  only reachable via out-of-spec input the conformance matcher rejects — same
  class as `INVALID_CHANGE_LEVEL`, §6).

## 4. Design area 3 — documenting emitted-but-undocumented error codes

Add the following already-emitted responses to `api/openapi.yaml`. All codes
exist in `internal/common/error_codes.go` with help topics; the gap is purely
documentation.

| Operation | Add | Status | Producible on running backend? |
|---|---|---|---|
| `deleteEntityModel` | `MODEL_HAS_ENTITIES` | 409 | No — §2.4 waiver (multi-node/unit) |
| `unlockEntityModel` | `MODEL_ALREADY_UNLOCKED` | 409 | Yes (unlock an unlocked model) |
| `unlockEntityModel` | `MODEL_HAS_ENTITIES` | 409 | Yes (lock+create, then unlock) |
| `lockEntityModel` | `MODEL_ALREADY_LOCKED` | 409 | Yes (lock a locked model) |
| `setEntityModelUniqueKeys` | `MODEL_NOT_FOUND` | 404 | Yes (unknown model) |
| `setEntityModelUniqueKeys` | `BAD_REQUEST` | 400 | Yes (malformed body) |
| `setEntityModelUniqueKeys` | `COMPOSITE_KEY_UNSUPPORTED` | 422 | No — waiver (all in-repo backends support composite keys; `capableStoreFactory` unit stub) |
| `importEntityModel` | `MODEL_ALREADY_LOCKED` | 409 | Yes (re-import a locked model) |
| `importEntityModel` | `INVALID_UNIQUE_KEY_DEFINITION` | 422 | No — waiver (defensive; additive merge can't invalidate carried keys, `service.go:160-162`; unit stub) |

`setEntityModelUniqueKeys` already documents `409 MODEL_ALREADY_LOCKED` and
`422 INVALID_UNIQUE_KEY_DEFINITION` — those two stay.

## 5. Design area 4 — `exportMetadata` typed-200 + fabricated-example fix

- **200 body:** currently `type: object, additionalProperties: true` — an opaque
  bag. `ExportModel` (`service.go:190-252`) returns `{currentState, model}` and,
  when `desc.UniqueKeys` is non-empty, injects a top-level **`uniqueKeys`** array
  (`[{id, fields[]}]`). Type the response **typed-but-open** (ADR 0003 Decision 3):
  enumerate `currentState`, `model`, and the `uniqueKeys` array shape; do **not**
  set `additionalProperties: false`. This closes the "opaque bag hides real
  fields" defect (class P3, same as entity-slice E1).
- **400 example:** the documented example (`Invalid value 'WRONG' for parameter
  'converter'`, with `properties {parameter, invalidValue}`) is fabricated — no
  server path emits it. Replace with the real `ProblemDetail` shape: `code:
  BAD_REQUEST`, `detail: unsupported export converter`.

## 6. Design area 5 — stale-prose fixes

- **`setEntityModelChangeLevel`** — the "or null to disallow changes" / "Set to
  null to disallow all changes" prose (`openapi.yaml:4191,4231-4239`) describes
  unimplemented behaviour: `changeLevel` is a **required** path enum
  `[ARRAY_LENGTH, ARRAY_ELEMENTS, TYPE, STRUCTURAL]` that cannot be null. Remove
  the null prose. Add `400 INVALID_CHANGE_LEVEL` (emitted `service.go:524`). Keep
  the enum accurate; the `400` is reachable only via out-of-enum path input
  (conformance matcher rejects) → **service-layer unit test + waived
  running-backend cell** (same class as export converter, §3). HTTP-only (no gRPC
  entry point for setChangeLevel).

## 7. Design area 6 — workflow-op error enumeration

- **`importEntityModelWorkflow`** — the generic untyped `400` folds three codes,
  all HTTP 400: enumerate `BAD_REQUEST` (`handler.go:98,118,122,131`),
  `VALIDATION_FAILED` (`:222,233,242,253`), `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED`
  (`:167`) in the `400` ProblemDetail description. (`WORKFLOW_SCHEMA_VERSION_UNSUPPORTED`
  documentation aligns with `docs/workflow-schema-versioning.md`.) `404
  MODEL_NOT_FOUND` (`:187`) already documented — keep.
- **`exportEntityModelWorkflow`** — the single `404` conflates two codes:
  `MODEL_NOT_FOUND` (`handler.go:339`) and `WORKFLOW_NOT_FOUND` (`:364`). Document
  both in the `404` description so callers can disambiguate via the ProblemDetail
  `code`.

Neither workflow op has a gRPC entry point (HTTP-only).

## 8. Per-endpoint error / status-code table

Boilerplate `401 Unauthorized` / `403 Forbidden` / `default InternalServerError`
omitted (every op carries them). ✎ = added/changed by this slice.

| Operation | Method + Path | 200 | 400 | 404 | 409 | 422 |
|---|---|---|---|---|---|---|
| getAvailableEntityModels | GET `/model/` | list | — | — | — | — |
| exportMetadata | GET `/model/export/{converter}/{entityName}/{modelVersion}` | ✎ typed `uniqueKeys` | ✎ `BAD_REQUEST` (real example) | `MODEL_NOT_FOUND` | — | — |
| importEntityModel | POST `/model/import/{dataFormat}/{converter}/{entityName}/{modelVersion}` | ok | `BAD_REQUEST` | — | ✎ `MODEL_ALREADY_LOCKED` | ✎ `INVALID_UNIQUE_KEY_DEFINITION` |
| validateEntityModel | POST `/model/validate/{entityName}/{modelVersion}` | `{success}` | `BAD_REQUEST` | `MODEL_NOT_FOUND` | — | — |
| deleteEntityModel | DELETE `/model/{entityName}/{modelVersion}` | ok | — | `MODEL_NOT_FOUND` | ✎ `MODEL_ALREADY_LOCKED`, ✎ `MODEL_HAS_ENTITIES` | — |
| setEntityModelChangeLevel | POST `/model/{entityName}/{modelVersion}/changeLevel/{changeLevel}` | ok | ✎ `INVALID_CHANGE_LEVEL` | `MODEL_NOT_FOUND` | — | — |
| lockEntityModel | PUT `/model/{entityName}/{modelVersion}/lock` | ok | — | `MODEL_NOT_FOUND` | ✎ `MODEL_ALREADY_LOCKED` | — |
| unlockEntityModel | PUT `/model/{entityName}/{modelVersion}/unlock` | ok | — | `MODEL_NOT_FOUND` | ✎ `MODEL_ALREADY_UNLOCKED`, ✎ `MODEL_HAS_ENTITIES` | — |
| setEntityModelUniqueKeys | PUT `/model/{entityName}/{modelVersion}/unique-keys` | ok | ✎ `BAD_REQUEST` | ✎ `MODEL_NOT_FOUND` | `MODEL_ALREADY_LOCKED` | `INVALID_UNIQUE_KEY_DEFINITION`, ✎ `COMPOSITE_KEY_UNSUPPORTED` |
| exportEntityModelWorkflow | GET `/model/{entityName}/{modelVersion}/workflow/export` | dto | — | ✎ `MODEL_NOT_FOUND` + `WORKFLOW_NOT_FOUND` | — | — |
| importEntityModelWorkflow | POST `/model/{entityName}/{modelVersion}/workflow/import` | `{success}` | ✎ `BAD_REQUEST` + `VALIDATION_FAILED` + `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` | `MODEL_NOT_FOUND` | — | — |

## 9. Coverage matrix (scenario × layer)

Layers: **U** = domain/service unit test · **E** = running-backend HTTP e2e
(`internal/e2e`, real Postgres) · **G** = gRPC envelope test (`internal/grpc`,
CloudEvents) · **P** = cross-backend parity (`e2e/parity`). ✓ = required cell,
— = N/A, **W** = waived with rationale.

gRPC assertion shape (established, `rpc_test.go:531` etc.): operational AppErrors
map to `Success=false`, `Error.Code == "CLIENT_ERROR"`, `Error.Message` contains
the domain code. **Not** `Error.Code == <DOMAIN_CODE>`.

| Scenario (op × code) | U | E | G | P | Notes |
|---|---|---|---|---|---|
| deleteEntityModel happy | — | ✓ | ✓ | — | HTTP + gRPC delete |
| deleteEntityModel `MODEL_NOT_FOUND` 404 | — | ✓ | ✓ | — | |
| deleteEntityModel `MODEL_ALREADY_LOCKED` 409 (NEW behaviour) | ✓ | ✓ | ✓ | — | producible: lock, then delete |
| deleteEntityModel `MODEL_HAS_ENTITIES` 409 | ✓ | **W** | **W** | — | unlocked+entities not API-constructible single-node; multi-node TOCTOU guard; unit constructs directly |
| lockEntityModel `MODEL_ALREADY_LOCKED` 409 | — | ✓ | ✓ | — | gRPC via `EntityModelTransitionRequest` LOCK |
| unlockEntityModel `MODEL_ALREADY_UNLOCKED` 409 | — | ✓ | ✓ | — | gRPC via TRANSITION UNLOCK |
| unlockEntityModel `MODEL_HAS_ENTITIES` 409 | — | ✓ | ✓ | — | lock+create then unlock |
| importEntityModel `MODEL_ALREADY_LOCKED` 409 | — | ✓ | ✓ | — | gRPC via `EntityModelImportRequest` |
| importEntityModel `BAD_REQUEST` 400 (converter) | — | ✓ | ✓ | — | send `converter=JSON_SCHEMA` (in-enum, rejected) |
| importEntityModel `INVALID_UNIQUE_KEY_DEFINITION` 422 | ✓ | **W** | **W** | — | defensive; additive merge can't trigger via API |
| exportMetadata typed-200 `uniqueKeys` | — | ✓ | ✓ | — | set keys, export, assert array present |
| exportMetadata `BAD_REQUEST` 400 (converter) | ✓ | **W** | **W** | — | enum==accept-set; out-of-enum → matcher rejects |
| exportMetadata `MODEL_NOT_FOUND` 404 | — | ✓ | ✓ | — | |
| setEntityModelChangeLevel `INVALID_CHANGE_LEVEL` 400 | ✓ | **W** | — | — | HTTP-only; enum-guarded → out-of-enum matcher-rejected |
| setEntityModelChangeLevel `MODEL_NOT_FOUND` 404 | — | ✓ | — | — | HTTP-only |
| setEntityModelUniqueKeys `MODEL_NOT_FOUND` 404 | — | ✓ | ✓ | — | |
| setEntityModelUniqueKeys `BAD_REQUEST` 400 | — | ✓ | ✓ | — | malformed body |
| setEntityModelUniqueKeys `COMPOSITE_KEY_UNSUPPORTED` 422 | ✓ | **W** | ✓ | — | e2e-W (all backends support composite); gRPC test already exists (`rpc_test.go:617`) |
| setEntityModelUniqueKeys `INVALID_UNIQUE_KEY_DEFINITION` 422 | ✓ | ✓ | ✓ | — | already covered; keep |
| importEntityModelWorkflow `VALIDATION_FAILED` 400 | — | ✓ | — | — | HTTP-only; empty/invalid workflow |
| importEntityModelWorkflow `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` 400 | — | ✓ | — | — | HTTP-only; stale version |
| importEntityModelWorkflow `BAD_REQUEST` 400 | — | ✓ | — | — | HTTP-only; unknown importMode |
| exportEntityModelWorkflow `MODEL_NOT_FOUND` 404 | — | ✓ | — | — | HTTP-only |
| exportEntityModelWorkflow `WORKFLOW_NOT_FOUND` 404 | — | ✓ | — | — | HTTP-only; model exists, no workflow |

Backend-agnostic behaviour here is model-registry state (lock/unlock/has-entities)
shared across all backends. Existing model parity coverage exercises the common
path; no new parity scenario is required unless implementation surfaces a
backend divergence (per CLAUDE.md, a divergence would be a bug to fix). No
concurrency/race test is added (the multi-node TOCTOU window in §2.4 is a
documented defensive guard, not a behaviour this slice asserts an interleave for;
per `.claude/rules/test-coverage.md` such tests stay isolated, never in parity).

**Waiver summary (5 cells):** each is a code that is emitted but not reachable on
a running in-repo backend through spec-conformant input — covered by
service/handler unit tests instead. This is the established `capableStoreFactory`
pattern (`service_test.go:354`).

## 10. gRPC coverage note

Model ops with a CloudEvents entry point (`grpc/model.go:47-58`): import, export,
transition (lock/unlock), delete, getAll, setUniqueKeys. `setChangeLevel`,
`validateEntityModel`, and both workflow ops are **HTTP-only**.

The CloudEvents `events` error envelope types define `Error.Code` as a
**required free-form string with no enum** — so **no gRPC schema artifact needs
updating** for any documented code; only `api/openapi.yaml` (HTTP) and the
already-present help topics change. The new delete-lock 409 and the documented
transition/import/setUniqueKeys codes flow to gRPC automatically via
`buildErrorFields`; the slice adds gRPC envelope tests for the new/uncovered
classes (matrix column G).

## 11. oasdiff dispositions

All changes are **additive** (new documented responses, a typed-but-open 200
body, prose edits) — expected to pass the additive-only gate with no
`.github/oasdiff-err-ignore.txt` entries. Watch-item: the `exportMetadata` 200
schema change from `additionalProperties:true` to an enumerated typed-but-open
object must **not** add `additionalProperties:false` (would be caught by
`TestSpecHasNoSealedSchemas`) and must not narrow a type (oasdiff). Enumerating
properties on an open object is non-breaking. If any correction unexpectedly
trips the gate, add a surgical documented ignore entry (the E4/stats-slice
pattern) rather than weakening the fix.

## 12. Cloud-parity notes (`docs/cloud-parity/openapi-conformance.md`)

Add a "Model/workflow reconciliations (2026-07)" section:
- **M1 — `deleteEntityModel` enforces UNLOCKED (runtime change).** Delete now
  returns `409 MODEL_ALREADY_LOCKED` for a locked model. Direction: server-gap
  (closed). Cloud MUST mirror: refuse deletion of a locked model.
- **M2 — documented error-code clarifications.** The previously-undocumented
  codes now in the contract (unlock `MODEL_HAS_ENTITIES`/`MODEL_ALREADY_UNLOCKED`,
  lock/import `MODEL_ALREADY_LOCKED`, setUniqueKeys `MODEL_NOT_FOUND`/`BAD_REQUEST`/
  `COMPOSITE_KEY_UNSUPPORTED`, changeLevel `INVALID_CHANGE_LEVEL`, workflow-import
  `VALIDATION_FAILED`/`WORKFLOW_SCHEMA_VERSION_UNSUPPORTED`, workflow-export
  `MODEL_NOT_FOUND` vs `WORKFLOW_NOT_FOUND` disambiguation) are contract
  clarifications Cloud must match. Direction: spec-incomplete (closed).
- **M3 — `exportMetadata.uniqueKeys` typed.** The 200 body now enumerates the
  `uniqueKeys` array (typed-but-open). Cloud MUST emit it when keys exist.

## 13. Dead-code / zombie sweep checklist

- `MODEL_HAS_ENTITIES`-on-delete: **retained** as a multi-node guard (§2.4) — not
  removed.
- Confirm no handler still references the removed changeLevel-null prose.
- Confirm `MODEL_NOT_LOCKED` (defined, used by entity-create gate, not by
  model-management ops) is untouched — out of scope, no change.
- Verify `api/generated.go` regenerates clean after the spec edits (the
  `codegen-sync` gate); regen if the router surface shifts.

## 14. Verification gates

- TDD: RED for the delete-lock behaviour change first (§2.2); RED producing tests
  for each documented code before its doc is added.
- Full `go test ./... -v` (incl. `internal/e2e`), `go vet ./...`, plugin
  submodules, `make race` once pre-PR.
- `make check-codegen` (generated.go in sync), `make check-gofmt`, oasdiff gate,
  `TestSpecHasNoSealedSchemas`, `TestErrCode_Parity` (no new code → no delta),
  the OpenAPI conformance test (every documented status exercised or waived).
- Gate 4 docs: `docs/cloud-parity/openapi-conformance.md` (§12), CHANGELOG. No
  env-var/help-topic/README/COMPATIBILITY change (no new code, no interface
  change). `docs/workflow-schema-versioning.md` already references
  `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` — no change needed there.
