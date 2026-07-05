# Entity-model & workflow OpenAPI reconciliation + #369 doc catch-up — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reconcile `api/openapi.yaml` (+ the shipped help subsystem) with the actual server for the entity-model & workflow domain, enforce the documented `deleteEntityModel` unlocked-precondition, and sync the help/core docs the prior #371/#373 slices left stale.

**Architecture:** One PR to `release/v0.8.2`, three logical commits — (1) the single runtime behaviour change (`deleteEntityModel` lock enforcement) + its tests + its docs; (2) Part A documentation reconciliation (openapi.yaml + Part-A help topics) with deliberate producing tests; (3) Part B documentation catch-up for #371/#373 (help topics, one openapi description-line fix, new `audit.md`). Spec: `docs/superpowers/specs/2026-07-05-openapi-model-workflow-design.md`.

**Tech Stack:** Go 1.26; `internal/domain/model` + `internal/domain/workflow` (services/handlers); `internal/e2e` (Postgres testcontainer HTTP e2e, package `e2e_test`); `internal/grpc` (CloudEvents envelope tests, package `grpc`); `internal/domain/model` unit tests (package `model_test`); `api/openapi.yaml` (`//go:embed`); `cmd/cyoda/help/content/` (embedded help tree).

## Global Constraints

- **Typed-but-OPEN** (ADR 0003): enumerate response properties, **NEVER** `additionalProperties: false`. Enforced by `TestSpecHasNoSealedSchemas`.
- **No new error code** is introduced by this slice → no new `errors/<CODE>.md` topic, no `TestErrCode_Parity` delta. `deleteEntityModel`-when-locked **reuses** existing `MODEL_ALREADY_LOCKED` (consistent with `setEntityModelUniqueKeys`).
- **Coverage rule** (`.claude/rules/test-coverage.md`): every documented status/error code proven on a running backend; HTTP and gRPC are separate entry points; concurrency tests isolated, never in parity. **Model/workflow ops are NOT keys in `EntityErrorCodeMatrix`** (`internal/e2e/zzz_errorcode_matrix_test.go`), so the matrix will not auto-fail on undocumented cells here — producing tests are written **deliberately** per this plan. Do **not** add model/workflow ops to `EntityErrorCodeMatrix` (avoids the per-op-completeness footgun; matches the #373 precedent).
- **Conformance harness** (`internal/e2e/openapivalidator`, ModeEnforce): validates every exercised response **shape** against the spec; and (`zzz_openapi_conformance_test.go`) requires each op be exercised at least once OR carry `x-cyoda-status`. So every documented response schema must be correct, and every touched op must have ≥1 e2e call.
- **gRPC envelope**: operational `AppError`s surface as `Success=false`, `Error.Code == "CLIENT_ERROR"`, domain code inside `Error.Message` (assert with `strings.Contains`). **Never** assert `Error.Code == "<DOMAIN_CODE>"`.
- **Workflow schema version** in any workflow-import fixture MUST be `"1.1"` (current supported minor; `"1.0"`/`"2.0"` are rejected).
- **oasdiff additive-only gate**: all edits here are additive (new responses, typed-but-open 200, prose/description). No `.github/oasdiff-err-ignore.txt` entry is expected; if one unexpectedly trips, add a surgical documented entry rather than weakening the fix.
- **codegen-sync gate**: after editing `api/openapi.yaml`, run `make check-codegen`; regenerate (`go generate ./api/...` or the documented regen) if `api/generated.go` drifts, and commit the regen in the same task.
- **Error decode idioms**: HTTP ProblemDetail carries the code in `properties.errorCode` and the message in top-level `detail`. e2e helper `assertErrorCode(t, body, "CODE")` reads `properties.errorCode`; some tests use `strings.Contains(detail, "CODE")`.

---

## File Structure

**Code (behaviour change — Task 1 only):**
- Modify: `internal/domain/model/service.go` (`DeleteModel`, ~365-408) — add lock check.

**Tests:**
- Modify: `internal/domain/model/handler_test.go` — new locked-delete unit test; fix `TestDeleteSucceedsAfterEntitiesDeleted`.
- Modify: `internal/e2e/model_*_test.go` / new `internal/e2e/model_errors_test.go` — HTTP producing tests.
- Modify: `internal/grpc/rpc_test.go` — gRPC envelope producing tests.
- Modify: `internal/domain/model/service_test.go` — service unit tests for waived codes.

**Contract + docs:**
- Modify: `api/openapi.yaml` (Part A response docs + §14.0 modelKey description).
- Modify: `api/generated.go` (regen if needed).
- Modify: `cmd/cyoda/help/content/models.md`, `crud.md`, `search.md`, `workflows.md`, `errors/MODEL_ALREADY_LOCKED.md`, `errors/MODEL_NOT_FOUND.md`; Create: `cmd/cyoda/help/content/audit.md`.
- Modify: `docs/FEATURES.md`, `docs/cloud-parity/openapi-conformance.md`, `CHANGELOG.md`.

---

# COMMIT 1 — `deleteEntityModel` enforces UNLOCKED (spec §2)

### Task 1: Add lock-state check to `DeleteModel`

**Files:**
- Modify: `internal/domain/model/service.go` (`DeleteModel`, ~365-408)
- Test: `internal/domain/model/handler_test.go` (package `model_test`)

**Interfaces:**
- Consumes: `DeleteModel(ctx, entityName, modelVersion string) error`; `spi.ModelLocked`/`spi.ModelUnlocked`; `common.Operational(status int, code, msgFmt string, args...)`; `common.ErrCodeModelAlreadyLocked`.
- Produces: `DeleteModel` now returns `409 MODEL_ALREADY_LOCKED` for a LOCKED model, checked **before** the entity-count check.

- [ ] **Step 1: Write the failing unit test (new) — delete of a LOCKED, entity-free model → 409 MODEL_ALREADY_LOCKED.** Add to `handler_test.go` (mirrors `TestDeleteSucceedsAfterEntitiesDeleted` setup at :664, but without purging and expecting a conflict):

```go
func TestDeleteBlockedByLock(t *testing.T) {
	_, srv := newTestApp(t)

	// Import + lock a model with no entities.
	resp := doImport(t, srv.URL, "DeleteLockGuard", 1, sampleJSON)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = doLock(t, srv.URL, "DeleteLockGuard", 1)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Delete a LOCKED model → 409 MODEL_ALREADY_LOCKED (must unlock first).
	resp = doDelete(t, srv.URL, "DeleteLockGuard", 1)
	expectStatus(t, resp, http.StatusConflict)
	commontest.ExpectErrorCode(t, resp, "MODEL_ALREADY_LOCKED")
	resp.Body.Close()
}
```

- [ ] **Step 2: Run it — expect FAIL** (today delete ignores lock → 200):

Run: `go test ./internal/domain/model/ -run TestDeleteBlockedByLock -v`
Expected: FAIL — got status 200, want 409.

- [ ] **Step 3: Implement the lock check in `DeleteModel`.** After the descriptor fetch + not-found (404) check and **before** the entity-count (409 `MODEL_HAS_ENTITIES`) check, insert:

```go
	if desc.State == spi.ModelLocked {
		return common.Operational(http.StatusConflict, common.ErrCodeModelAlreadyLocked,
			"cannot delete entityModel{name=%s, version=%s}: expectedState=UNLOCKED, actualState=LOCKED",
			entityName, modelVersion)
	}
```

(Order = 404 → lock → count, per spec §2.3. Match the exact `desc`/param variable names already in `DeleteModel`; if the descriptor var is `descriptor`, use that.)

- [ ] **Step 4: Run it — expect PASS.**

Run: `go test ./internal/domain/model/ -run TestDeleteBlockedByLock -v`
Expected: PASS.

- [ ] **Step 5: Reconcile `TestDeleteSucceedsAfterEntitiesDeleted` (handler_test.go:664).** It imports **and locks**, then deletes after purging entities — now that delete requires UNLOCKED, insert an unlock step before the final delete (unlock is legal once entities are purged). Add, immediately before the final `doDelete`:

```go
	// Model must be UNLOCKED before it can be deleted.
	resp = doUnlock(t, srv.URL, "<sameEntityName>", 1)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
```

(Use that test's actual entity name/version. Verify `TestDeleteBlockedByEntities` at :630 is UNTOUCHED and still green — it imports an *unlocked* model with an entity, so the new lock-check passes and it still returns `MODEL_HAS_ENTITIES`. This is the §2.4 unlocked-with-entities coverage; do not add a duplicate.)

- [ ] **Step 6: Run the whole model package — expect PASS.**

Run: `go test ./internal/domain/model/ -v`
Expected: PASS (incl. `TestDeleteBlockedByLock`, `TestDeleteBlockedByEntities`, `TestDeleteSucceedsAfterEntitiesDeleted`).

- [ ] **Step 7: Commit.**

```bash
git add internal/domain/model/service.go internal/domain/model/handler_test.go
git commit -m "feat(model): deleteEntityModel enforces UNLOCKED state (409 MODEL_ALREADY_LOCKED)"
```

### Task 2: Delete-lock producing tests — e2e + gRPC

**Files:**
- Test: `internal/e2e/model_errors_test.go` (create; package `e2e_test`)
- Test: `internal/grpc/rpc_test.go` (append; package `grpc`)

**Interfaces:**
- Consumes (e2e): `importModelE2E(t, name, version int)`, `lockModelE2E(t, name, version int)`, `doAuth(t, method, path, body)`, `readBody(t, resp)`, `assertErrorCode(t, body, code)`.
- Consumes (gRPC): `newTestEnv(t)`, `importAndLockModel(t, svc, ctx, name, version, sample)`, `makeCE(type, fields)`, `svc.EntityModelManage(ctx, ce)`, `validateResponse(t, resp, &typed)`, `events.EntityModelDeleteResponseJson`, `EntityModelDeleteRequest`.

- [ ] **Step 1: Write the e2e producing test.** Create `internal/e2e/model_errors_test.go`:

```go
package e2e_test

import (
	"net/http"
	"testing"
)

func TestModelDelete_Locked_409(t *testing.T) {
	const model = "e2e-del-locked"
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)

	resp := doAuth(t, http.MethodDelete, "/api/model/"+model+"/1", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete locked model: expected 409, got %d: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "MODEL_ALREADY_LOCKED")
}
```

- [ ] **Step 2: Write the gRPC producing test.** Append to `rpc_test.go` (delete uses the delete envelope; `importAndLockModel` leaves the model LOCKED):

```go
func TestRPC_ModelDelete_409_Locked(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "del-locked", "1", map[string]any{"name": "Alice"})

	ce := makeCE(EntityModelDeleteRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "del-locked", "version": 1},
	})
	resp, err := svc.EntityModelManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var typed events.EntityModelDeleteResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Error("expected success=false for locked-model delete")
	}
	if typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" {
		t.Fatalf("expected CLIENT_ERROR envelope, got %+v", typed.Error)
	}
	if !strings.Contains(typed.Error.Message, "MODEL_ALREADY_LOCKED") {
		t.Errorf("expected message to contain MODEL_ALREADY_LOCKED, got %s", typed.Error.Message)
	}
}
```

- [ ] **Step 3: Run both — expect PASS** (behaviour already implemented in Task 1):

Run: `go test ./internal/grpc/ -run TestRPC_ModelDelete_409_Locked -v && go test ./internal/e2e/ -run TestModelDelete_Locked_409 -v`
Expected: PASS (e2e requires Docker).

- [ ] **Step 4: Commit.**

```bash
git add internal/e2e/model_errors_test.go internal/grpc/rpc_test.go
git commit -m "test(model): prove delete-locked 409 MODEL_ALREADY_LOCKED on HTTP + gRPC"
```

### Task 3: Delete-lock documentation (openapi + help + cloud-parity + CHANGELOG)

**Files:**
- Modify: `api/openapi.yaml` (`deleteEntityModel`, ~4117-4173), `api/generated.go` (regen if needed)
- Modify: `cmd/cyoda/help/content/models.md`, `cmd/cyoda/help/content/errors/MODEL_ALREADY_LOCKED.md`
- Modify: `docs/cloud-parity/openapi-conformance.md`, `CHANGELOG.md`

- [ ] **Step 1: Document the two 409s on `deleteEntityModel` in `api/openapi.yaml`.** Under `deleteEntityModel.responses`, add a `409` keyed to the shared `ProblemDetail` component, with two documented codes in the description (`MODEL_ALREADY_LOCKED` when locked; `MODEL_HAS_ENTITIES` when entities exist). Mirror the existing `409` block used by `setEntityModelUniqueKeys` (openapi.yaml ~4533) for exact shape/`$ref`. Confirm the operation description's "Model must be in UNLOCKED state" line remains (now enforced/true).

- [ ] **Step 2: Regen check.**

Run: `make check-codegen`
Expected: clean, OR regenerate `api/generated.go` and stage it.

- [ ] **Step 3: Update `models.md`.** (a) DELETE endpoint response line (~100) — add `409 MODEL_ALREADY_LOCKED` for the locked case alongside the existing entities-409. (b) ERRORS section (~306) — extend `MODEL_ALREADY_LOCKED` line to include "delete of a locked model".

- [ ] **Step 4: Update `errors/MODEL_ALREADY_LOCKED.md`.** In DESCRIPTION's emitting-operations list add: `DELETE /model/{name}/{version}` (delete requires UNLOCKED) and `PUT /model/{name}/{version}/unique-keys`; note the delete branch also sets `expectedState`/`actualState`.

- [ ] **Step 5: Add the cloud-parity section.** In `docs/cloud-parity/openapi-conformance.md`, add after the stats/audit/search section:

```markdown
## Model/workflow reconciliations (2026-07)

Per-finding contract decisions from the entity-model & workflow reconciliation slice.

- **M1 — `deleteEntityModel` enforces UNLOCKED (runtime change).** Deleting a
  LOCKED model now returns `409 MODEL_ALREADY_LOCKED` (reuses the locked-refusal
  code, delete-specific message). The `409 MODEL_HAS_ENTITIES` guard is retained
  (multi-node create/unlock TOCTOU backstop). Direction: server-gap (closed).
  Cloud MUST refuse deletion of a locked model.
- **M2 — documented error-code clarifications.** Previously-undocumented codes now
  in the contract are clarifications Cloud must match: unlock `MODEL_HAS_ENTITIES`
  / `MODEL_ALREADY_UNLOCKED`; lock & import `MODEL_ALREADY_LOCKED`; import
  `INVALID_UNIQUE_KEY_DEFINITION`; setUniqueKeys `MODEL_NOT_FOUND` / `BAD_REQUEST`
  / `COMPOSITE_KEY_UNSUPPORTED`; changeLevel `INVALID_CHANGE_LEVEL`; import/export
  unsupported-converter `BAD_REQUEST`; workflow-import `VALIDATION_FAILED` /
  `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED`; workflow-export `MODEL_NOT_FOUND`-vs-
  `WORKFLOW_NOT_FOUND` disambiguation. Direction: spec-incomplete (closed).
```

(M3 is appended in Task 6, which produces the exportMetadata typed body.)

- [ ] **Step 6: CHANGELOG.** Under `## [Unreleased]` → `### Changed`, add:

```markdown
- **`DELETE /model/{entityName}/{modelVersion}` now enforces the documented
  UNLOCKED precondition** — deleting a `LOCKED` model returns `409
  MODEL_ALREADY_LOCKED` (previously the lock state was ignored). Unlock the model
  first. The `409 MODEL_HAS_ENTITIES` guard is unchanged.
```

- [ ] **Step 7: Verify docs build + conformance shape.**

Run: `go build ./cmd/cyoda && go test ./internal/e2e/ -run 'TestOpenAPIConformanceReport|TestModelDelete_Locked_409' -v`
Expected: PASS (documented 409 shape validates against the produced body).

- [ ] **Step 8: Commit.**

```bash
git add api/openapi.yaml api/generated.go cmd/cyoda/help/content/models.md cmd/cyoda/help/content/errors/MODEL_ALREADY_LOCKED.md docs/cloud-parity/openapi-conformance.md CHANGELOG.md
git commit -m "docs(model): document deleteEntityModel 409s + cloud-parity M1 + CHANGELOG"
```

---

# COMMIT 2 — Part A documentation reconciliation (spec §3–§7)

Each task adds documented responses to `api/openapi.yaml` + deliberate producing tests. After any `openapi.yaml` edit, run `make check-codegen` and regen `api/generated.go` if needed (fold into the task's commit). Before writing a new producing test, grep for an existing one (`git grep -n "lock" internal/e2e`) and reference/extend it rather than duplicating.

### Task 4: lock/unlock 409 documentation + producing tests

**Files:** `api/openapi.yaml` (`lockEntityModel` ~4310, `unlockEntityModel` ~4395); `internal/e2e/model_errors_test.go`; `internal/grpc/rpc_test.go`.

**Interfaces:** gRPC lock/unlock go through `EntityModelTransitionRequest` with `"transition": "LOCK"|"UNLOCK"`, response type `events.EntityModelTransitionResponseJson`.

- [ ] **Step 1: openapi.yaml.** Add `409` (ProblemDetail) to `lockEntityModel` documenting `MODEL_ALREADY_LOCKED`; add `409` to `unlockEntityModel` documenting `MODEL_ALREADY_UNLOCKED` and `MODEL_HAS_ENTITIES`. Reuse the existing 409 block shape.
- [ ] **Step 2: e2e producing tests** (append to `model_errors_test.go`):

```go
func TestModelLock_AlreadyLocked_409(t *testing.T) {
	const m = "e2e-lock-twice"
	importModelE2E(t, m, 1)
	lockModelE2E(t, m, 1)
	resp := doAuth(t, http.MethodPut, "/api/model/"+m+"/1/lock", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("relock: expected 409, got %d: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "MODEL_ALREADY_LOCKED")
}

func TestModelUnlock_AlreadyUnlocked_409(t *testing.T) {
	const m = "e2e-unlock-unlocked"
	importModelE2E(t, m, 1) // stays UNLOCKED
	resp := doAuth(t, http.MethodPut, "/api/model/"+m+"/1/unlock", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("unlock unlocked: expected 409, got %d: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "MODEL_ALREADY_UNLOCKED")
}

func TestModelUnlock_HasEntities_409(t *testing.T) {
	const m = "e2e-unlock-entities"
	importModelE2E(t, m, 1)
	lockModelE2E(t, m, 1)
	createEntityE2E(t, m, 1, `{"name":"x"}`)
	resp := doAuth(t, http.MethodPut, "/api/model/"+m+"/1/unlock", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("unlock with entities: expected 409, got %d: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "MODEL_HAS_ENTITIES")
}
```

- [ ] **Step 3: gRPC producing tests** (append to `rpc_test.go`). Use `EntityModelTransitionRequest` with `"transition":"LOCK"` after `importAndLockModel` (already locked) → `MODEL_ALREADY_LOCKED`; and `"transition":"UNLOCK"` on an imported-only (unlocked) model → `MODEL_ALREADY_UNLOCKED`. Assert `CLIENT_ERROR` + `strings.Contains(msg, "<CODE>")` against `events.EntityModelTransitionResponseJson`. (Grep first — a lock/unlock RPC test may already exist; extend rather than duplicate.)
- [ ] **Step 4: Run** `go test ./internal/grpc/ -run 'TestRPC_ModelLock|TestRPC_ModelUnlock' -v` and `go test ./internal/e2e/ -run 'TestModelLock|TestModelUnlock' -v` → PASS.
- [ ] **Step 5: Commit** `docs(model): document lock/unlock 409s + producing tests`.

### Task 5: importEntityModel 409 + 422 + converter-400 + producing tests

**Files:** `api/openapi.yaml` (`importEntityModel` ~3947); `internal/e2e/model_errors_test.go`; `internal/grpc/rpc_test.go`; `internal/domain/model/service_test.go`.

- [ ] **Step 1: openapi.yaml.** On `importEntityModel` add: `409` (`MODEL_ALREADY_LOCKED`), `422` (`INVALID_UNIQUE_KEY_DEFINITION`), and enrich the existing `400` description to note unsupported converters (`JSON_SCHEMA`/`SIMPLE_VIEW` → `BAD_REQUEST "unsupported import converter"`) and malformed sample data. Keep the converter enum `[SAMPLE_DATA, JSON_SCHEMA, SIMPLE_VIEW]` (spec §3 decision).
- [ ] **Step 2: e2e producing tests** — 409 (re-import a locked model) and 400 (converter=JSON_SCHEMA):

```go
func TestModelImport_LockedModel_409(t *testing.T) {
	const m = "e2e-import-locked"
	importModelE2E(t, m, 1)
	lockModelE2E(t, m, 1)
	resp := doAuth(t, http.MethodPost,
		"/api/model/import/JSON/SAMPLE_DATA/"+m+"/1", `{"name":"x"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("re-import locked: expected 409, got %d: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "MODEL_ALREADY_LOCKED")
}

func TestModelImport_UnsupportedConverter_400(t *testing.T) {
	const m = "e2e-import-conv"
	resp := doAuth(t, http.MethodPost,
		"/api/model/import/JSON/JSON_SCHEMA/"+m+"/1", `{"name":"x"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("import JSON_SCHEMA converter: expected 400, got %d: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "BAD_REQUEST")
}
```

- [ ] **Step 3: gRPC producing test** — import re-lock 409 (`EntityModelImportRequest` after `importAndLockModel`) and converter-400 (`"converter":"JSON_SCHEMA"` in the CE fields), asserting `CLIENT_ERROR` + message contains `MODEL_ALREADY_LOCKED` / `BAD_REQUEST` against `events.EntityModelImportResponseJson` (grep for the import response type name; the error envelope is `modelImportError`).
- [ ] **Step 4: service unit test (waived e2e) — 422 `INVALID_UNIQUE_KEY_DEFINITION`.** This is a defensive re-import path. Add a `service_test.go` unit test using `refreshingModelStore` seeded with a LOCKED descriptor carrying `UniqueKeys` that the merged schema invalidates, asserting `appErr.Status==422` and `appErr.Code==common.ErrCodeInvalidUniqueKeyDefinition`. (If constructing the invalidation is impractical via the fake, cover it by documenting the waiver in the spec matrix — it is already marked **W**; a service test is preferred but a documented waiver is acceptable per §9.)
- [ ] **Step 5: Run** the new import tests → PASS. **Commit** `docs(model): document importEntityModel 409/422/converter-400 + tests`.

### Task 6: exportMetadata typed-200 + fabricated-400 fix + converter-400 (§5, §3)

**Files:** `api/openapi.yaml` (`exportMetadata` ~3804-3945); `internal/e2e/model_errors_test.go`; `internal/grpc/rpc_test.go`; `internal/domain/model/service_test.go`; `cmd/cyoda/help/content/models.md`.

- [ ] **Step 1: openapi.yaml — type the 200 body.** Replace the `200` `type: object, additionalProperties: true` bag with an enumerated **typed-but-open** object: properties `currentState` (string), `model` (object), and `uniqueKeys` (array of `{id: string, fields: [string]}`). Do **NOT** add `additionalProperties: false`. Keep it open (omit `additionalProperties`, or `true`).
- [ ] **Step 2: openapi.yaml — fix the fabricated 400 example.** Replace the `Invalid value 'WRONG' for parameter 'converter'` example (with `{parameter, invalidValue}`) with the real ProblemDetail: `properties.errorCode: BAD_REQUEST`, `detail: "unsupported export converter"`. Keep the export converter enum `[JSON_SCHEMA, SIMPLE_VIEW]` (accurate; spec §3).
- [ ] **Step 3: e2e producing test — typed-200 uniqueKeys present.** Import a model, declare unique keys (`setUniqueKeysE2E`), export via `GET /api/model/export/JSON_SCHEMA/{m}/1`, assert 200 and that the body has a top-level `uniqueKeys` array. Also add a MODEL_NOT_FOUND 404 export test (`GET /api/model/export/JSON_SCHEMA/nope/1` → 404 `MODEL_NOT_FOUND`) if not already covered.
- [ ] **Step 4: service unit test (waived e2e) — export converter 400.** Add a `service_test.go` test calling `ExportModel`/the export path with an unsupported converter and asserting `appErr.Status==400`, `appErr.Code==common.ErrCodeBadRequest`. (Waived at the running-backend layer because the enum == accept-set → out-of-enum input is rejected by the conformance route-matcher; §3.)
- [ ] **Step 5: models.md.** Add the top-level `uniqueKeys` array to the SIMPLE_VIEW/JSON_SCHEMA export examples so the topic matches the typed-200.
- [ ] **Step 6: Add cloud-parity M3** (exportMetadata typed uniqueKeys) to the Model/workflow section:

```markdown
- **M3 — `exportMetadata.uniqueKeys` typed.** The export 200 body now enumerates
  the top-level `uniqueKeys` array (typed-but-open) alongside `currentState`/`model`.
  Direction: spec-incomplete (closed). Cloud MUST emit `uniqueKeys` when keys exist.
```

- [ ] **Step 7: Run** export tests + `TestOpenAPIConformanceReport` → PASS. **Commit** `docs(model): type exportMetadata 200 (uniqueKeys) + fix 400 example`.

### Task 7: setEntityModelUniqueKeys 404/400/422 documentation + producing tests

**Files:** `api/openapi.yaml` (`setEntityModelUniqueKeys` ~4485); `internal/e2e/model_errors_test.go`; `internal/grpc/rpc_test.go`.

- [ ] **Step 1: openapi.yaml.** Add `404` (`MODEL_NOT_FOUND`) and `400` (`BAD_REQUEST`, malformed body); add `COMPOSITE_KEY_UNSUPPORTED` to the existing `422` description (alongside the already-documented `INVALID_UNIQUE_KEY_DEFINITION`).
- [ ] **Step 2: e2e producing tests** — 404 (set keys on unknown model) and 400 (malformed body):

```go
func TestModelSetUniqueKeys_UnknownModel_404(t *testing.T) {
	status, body := setUniqueKeysE2E(t, "e2e-uk-nope", 1, `{"uniqueKeys":[{"id":"k","fields":["$.name"]}]}`)
	if status != http.StatusNotFound {
		t.Fatalf("set keys unknown model: expected 404, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "MODEL_NOT_FOUND")
}

func TestModelSetUniqueKeys_MalformedBody_400(t *testing.T) {
	const m = "e2e-uk-malformed"
	importModelWithSample(t, m, 1, ukSampleData)
	status, body := setUniqueKeysE2E(t, m, 1, `{not-json`)
	if status != http.StatusBadRequest {
		t.Fatalf("malformed keys body: expected 400, got %d: %s", status, body)
	}
	assertErrorCode(t, body, "BAD_REQUEST")
}
```

- [ ] **Step 3: gRPC.** `COMPOSITE_KEY_UNSUPPORTED` (422) already has a gRPC test (`rpc_test.go:614`), and `MODEL_ALREADY_LOCKED` (409) already has both e2e (`TestUniqueKeys_DeclareOnUnlocked`) and gRPC coverage — reference them in the spec matrix, do not duplicate. Add a gRPC unknown-model `MODEL_NOT_FOUND` test if absent.
- [ ] **Step 4: Run** the new tests → PASS. **Commit** `docs(model): document setEntityModelUniqueKeys 404/400/422 + tests`.

### Task 8: setEntityModelChangeLevel 400 + prose fix (§6)

**Files:** `api/openapi.yaml` (`setEntityModelChangeLevel` ~4210); `internal/domain/model/service_test.go`.

- [ ] **Step 1: openapi.yaml.** Add `400` (`INVALID_CHANGE_LEVEL`) to the responses. Remove the false "or null to disallow changes" / "Set to null to disallow all changes" prose (the param is a required path enum). Keep the `changeLevel` enum `[ARRAY_LENGTH, ARRAY_ELEMENTS, TYPE, STRUCTURAL]`.
- [ ] **Step 2: service unit test (waived e2e) — INVALID_CHANGE_LEVEL 400.** Add a `service_test.go` test calling `SetChangeLevel(ctx, name, ver, "BOGUS")` and asserting `appErr.Status==400`, `appErr.Code==common.ErrCodeInvalidChangeLevel`. (HTTP-only op; e2e-waived because the path enum rejects out-of-enum input at the conformance matcher; §6.)
- [ ] **Step 3: Run** `go test ./internal/domain/model/ -run SetChangeLevel -v` → PASS. **Commit** `docs(model): document setEntityModelChangeLevel 400 + fix null prose`.

### Task 9: workflow-op error enumeration (§7)

**Files:** `api/openapi.yaml` (`importEntityModelWorkflow` ~4632, `exportEntityModelWorkflow` ~4577); `internal/e2e/` (workflow tests — mostly existing); `cmd/cyoda/help/content/workflows.md`.

- [ ] **Step 1: openapi.yaml — importEntityModelWorkflow.** In the `400` ProblemDetail description enumerate all three codes: `BAD_REQUEST`, `VALIDATION_FAILED`, `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED`. Keep the existing `404 MODEL_NOT_FOUND`.
- [ ] **Step 2: openapi.yaml — exportEntityModelWorkflow.** In the `404` description document both `MODEL_NOT_FOUND` (model absent) and `WORKFLOW_NOT_FOUND` (model present, no workflows).
- [ ] **Step 3: Producing tests — reference existing, add the gaps.** Confirm/keep: `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` (`workflow_schema_version_test.go`), `BAD_REQUEST` (`TestWorkflow_Import_UnknownField_Returns400`), import `MODEL_NOT_FOUND` (`TestWorkflow_ImportUnknownModel`), export `WORKFLOW_NOT_FOUND` (`TestWorkflow_ExportEmpty`). **Add** a `VALIDATION_FAILED` producing e2e (import a REPLACE workflow that fails structural validation — e.g. an empty `workflows` array with `importMode: REPLACE`, or a state referencing a non-existent transition target — with `version:"1.1"`), asserting 400 + `properties.errorCode == "VALIDATION_FAILED"`. **Add** an export `MODEL_NOT_FOUND` e2e (`exportWorkflowE2E` on a never-imported model → 404, `strings.Contains(detail, "MODEL_NOT_FOUND")`).

```go
func TestWorkflow_ExportUnknownModel_404(t *testing.T) {
	status, body := exportWorkflowE2E(t, "e2e-wf-nomodel", 1)
	if status != http.StatusNotFound {
		t.Fatalf("export unknown model: expected 404, got %d", status)
	}
	detail, _ := body["detail"].(string)
	if !strings.Contains(detail, "MODEL_NOT_FOUND") {
		t.Errorf("expected MODEL_NOT_FOUND in detail, got %s", detail)
	}
}
```

(For `VALIDATION_FAILED`, verify the exact minimal structurally-invalid payload against `internal/domain/workflow/validate.go` before asserting; use `version:"1.1"` so the schema-version gate passes and validation is what fails.)

- [ ] **Step 4: workflows.md.** Completeness pass — ensure the import-error surface names `VALIDATION_FAILED` + `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED`; add `errors.WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` to `see_also` if absent.
- [ ] **Step 5: Run** `go test ./internal/e2e/ -run 'TestWorkflow' -v` → PASS. **Commit** `docs(workflow): enumerate import/export error codes + VALIDATION_FAILED/export-404 tests`.

---

# COMMIT 3 — Part B: #371/#373 documentation catch-up (spec §14)

Documentation-only sync to the already-shipped contract. Each edit is checked against the cited `api/openapi.yaml` line. No behaviour change.

### Task 10: openapi.yaml `modelKey` description fix (§14.0)

**Files:** `api/openapi.yaml` (`:10257`).

- [ ] **Step 1:** Change the `modelKey` property description from `Model of the entity. Present on single-entity reads.` to `Model of the entity. Present on all entity reads (single-get, list, search).`
- [ ] **Step 2:** `make check-codegen` (description-only → no generated.go change expected).
- [ ] **Step 3:** `go test ./internal/e2e/ -run TestOpenAPIConformanceReport -v` → PASS (additive). **Commit** `docs(openapi): modelKey present on all reads (completes #371 E1b)`.

### Task 11: `crud.md` reconciliation (§14.1)

**Files:** `cmd/cyoda/help/content/crud.md`.

- [ ] **Step 1:** Rewrite the `deleteEntities` section (~318-334) **and** the curl example (~678) from "delete all entities for a model" to the conditional contract: optional `AbstractConditionDto` body scopes the delete (empty ⇒ all), `verbose=true` returns deleted `ids`, `transactionSize`/`pointInTime` query params, `numberOfEntitites` (matched) vs `numberOfEntititesRemoved` (removed), `400 INVALID_CONDITION` on malformed condition. (Source of truth: openapi.yaml:1803-1928.)
- [ ] **Step 2:** `changeType` values `CREATED/UPDATED/DELETED` → `CREATE/UPDATE/DELETE` at :355, :361, :369.
- [ ] **Step 3:** Envelope (~591) — "`modelKey` … omitted from list/search" → present on all reads (single-get, list, search).
- [ ] **Step 4:** List endpoint (~336-343) — add `pointInTime` query param + as-at note. Envelope (~576-596) — add `meta.pointInTime`.
- [ ] **Step 5:** ERRORS (~609-621) + see_also (~5-18) — add `UNIQUE_VIOLATION` (409) and `INVALID_UNIQUE_KEY` (422).
- [ ] **Step 6:** `/changes` (~345-370) — add "reverse-chronological (newest first)" note; reorder the example newest-first. Single-GET meta example (~146-153) — add `modelKey`.
- [ ] **Step 7:** `go build ./cmd/cyoda && ./cyoda help crud` renders; **Commit** `docs(help): reconcile crud.md with #371 entity-slice contract`.

### Task 12: `search.md` + `errors/MODEL_NOT_FOUND.md` + `docs/FEATURES.md` (§14.2/3/5)

**Files:** `cmd/cyoda/help/content/search.md`, `cmd/cyoda/help/content/errors/MODEL_NOT_FOUND.md`, `docs/FEATURES.md`.

- [ ] **Step 1:** `search.md:229` — add `NOT_FOUND` to the `searchJobStatus` enum list. Envelope examples (~190-191, 252) — add `meta.modelKey`.
- [ ] **Step 2:** `errors/MODEL_NOT_FOUND.md` (~23-25) — add the read-path (list/stats/search/grouped-stats) usage to the DESCRIPTION; add `search`/`crud` to see_also (~5-8).
- [ ] **Step 3:** `docs/FEATURES.md:54` — `CREATED, UPDATED, DELETED` → `CREATE, UPDATE, DELETE`.
- [ ] **Step 4:** `go build ./cmd/cyoda && ./cyoda help search && ./cyoda help errors MODEL_NOT_FOUND` render; **Commit** `docs(help): reconcile search.md/MODEL_NOT_FOUND/FEATURES with #373 + #371`.

### Task 13: new `audit.md` topic (§14.4)

**Files:** Create `cmd/cyoda/help/content/audit.md`.

**Interfaces:** Follow the exact front-matter + section structure of `models.md`/`search.md` (topic/title/stability/see_also front-matter; NAME/SYNOPSIS/DESCRIPTION/ENDPOINTS/ERRORS/SEE ALSO). Register in the help tree if registration is explicit (check how `models.md` is discovered — the tree is embedded via `//go:embed`; a new file under `content/` is auto-included, but verify any index/test).

- [ ] **Step 1:** Write `audit.md` documenting:
  - `GET /api/audit/entity/{entityId}` (searchEntityAuditEvents) → `EntityChangeAuditEventDto` with `changeType` = `CREATE/UPDATE/DELETE`; `eventType` query enum `StateMachine`/`EntityChange`/`System` — **System excluded from the default set, "use with caution"** (openapi.yaml:361-397).
  - The `changes` before/after diff is a **deferred gap** — document it as not-yet-emitted; do NOT promise it as working.
  - `GET /api/audit/entity/{entityId}/workflow/{transactionId}/finished` (getStateMachineFinishedEvent; returns `ProblemDetail` on error, reconciled by #373).
  - see_also: crud, search, models, errors.MODEL_NOT_FOUND.
- [ ] **Step 2:** `go build ./cmd/cyoda && ./cyoda help audit` renders; if a help-tree completeness test exists (`git grep -n "help/content" --  '*_test.go'`), run it → PASS.
- [ ] **Step 3: Commit** `docs(help): add audit.md topic for the audit endpoints (#371 catch-up)`.

---

# FINAL — verification

### Task 14: Full verification + PR

**Files:** none (verification only), then PR.

- [ ] **Step 1: Regen guard.** `make check-codegen` → clean (regen + commit if drifted).
- [ ] **Step 2: Formatting.** `gofmt -l internal/ api/ cmd/ | tee /dev/stderr | (! read)` and `make check-gofmt` → clean.
- [ ] **Step 3: Vet + build.** `go vet ./...` and `go build ./...` → clean. (Reminder: `go build` does NOT compile test files — `go vet` catches test-signature breaks.)
- [ ] **Step 4: Full test suite** (incl. e2e; Docker required). `go test ./... -v` → PASS. Then plugin submodules: `make test-all` (or per-plugin `go test ./...` in `plugins/{memory,postgres,sqlite}`) → PASS.
- [ ] **Step 5: Spec conformance + strictness.** Confirm `TestOpenAPIConformanceReport`, `TestSpecHasNoSealedSchemas`, `TestErrCode_Parity`, and the `zzz_` conformance/matrix tests are green (in the Step-4 run).
- [ ] **Step 6: oasdiff gate (local).** Run the repo's oasdiff check (mirrors `.github/workflows/openapi-breaking-change.yml`) against `release/v0.8.2` → additive/no ERR. If an unexpected ERR appears, add a surgical documented `.github/oasdiff-err-ignore.txt` entry with rationale (do not weaken the fix).
- [ ] **Step 7: Race (one-shot).** `make race` → PASS.
- [ ] **Step 8: Help render spot-check.** `./cyoda help models`, `crud`, `search`, `audit`, `errors MODEL_ALREADY_LOCKED`, `errors MODEL_NOT_FOUND` all render without error.
- [ ] **Step 9: Open the PR** to `release/v0.8.2` (three logical commits already in place). PR body: link the spec, summarize Part A (delete-lock behaviour change + doc reconciliation) and Part B (#371/#373 catch-up), note the coverage-matrix waivers (5 codes) and their unit coverage, and that Part B is doc-only sync (no contract change).

---

## Coverage matrix carry-forward (spec §9 → tasks)

| Documented code (op) | Layer(s) | Task |
|---|---|---|
| deleteEntityModel MODEL_ALREADY_LOCKED 409 | unit + e2e + gRPC | 1, 2 |
| deleteEntityModel MODEL_HAS_ENTITIES 409 | unit (existing `TestDeleteBlockedByEntities`); e2e/gRPC **waived** | 1 (reconcile) |
| lockEntityModel MODEL_ALREADY_LOCKED 409 | e2e + gRPC | 4 |
| unlockEntityModel MODEL_ALREADY_UNLOCKED / MODEL_HAS_ENTITIES 409 | e2e + gRPC | 4 |
| importEntityModel MODEL_ALREADY_LOCKED 409 / BAD_REQUEST 400 | e2e + gRPC | 5 |
| importEntityModel INVALID_UNIQUE_KEY_DEFINITION 422 | unit; e2e/gRPC **waived** | 5 |
| exportMetadata typed-200 uniqueKeys / MODEL_NOT_FOUND 404 | e2e (+gRPC where present) | 6 |
| exportMetadata BAD_REQUEST 400 (converter) | unit; e2e **waived** | 6 |
| setEntityModelUniqueKeys MODEL_NOT_FOUND 404 / BAD_REQUEST 400 | e2e (+gRPC) | 7 |
| setEntityModelUniqueKeys COMPOSITE_KEY_UNSUPPORTED 422 | unit + gRPC (existing); e2e **waived** | 7 (reference) |
| setEntityModelUniqueKeys MODEL_ALREADY_LOCKED 409 / INVALID_UNIQUE_KEY_DEFINITION 422 | existing e2e/gRPC/unit | 7 (reference) |
| setEntityModelChangeLevel INVALID_CHANGE_LEVEL 400 | unit; e2e **waived** (HTTP-only) | 8 |
| importEntityModelWorkflow VALIDATION_FAILED / WORKFLOW_SCHEMA_VERSION_UNSUPPORTED / BAD_REQUEST / MODEL_NOT_FOUND | e2e (HTTP-only) | 9 |
| exportEntityModelWorkflow MODEL_NOT_FOUND / WORKFLOW_NOT_FOUND 404 | e2e (HTTP-only) | 9 |

Shared already-documented 404/409s (lock/unlock/importWorkflow `MODEL_NOT_FOUND`, setUniqueKeys `MODEL_ALREADY_LOCKED`) keep their existing coverage — no new test, verified present during Task 14.
