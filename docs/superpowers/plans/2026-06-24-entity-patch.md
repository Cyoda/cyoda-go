# Entity Partial-Update (PATCH / RFC 7386) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a single-entity PATCH (RFC 7386 JSON Merge Patch) capability over both the HTTP API and the gRPC layer, as an additive non-breaking feature for v0.8.2.

**Architecture:** PATCH is implemented as a *merge hook* on the existing `UpdateEntity` flow — `UpdateEntity` is refactored into a shared `updateEntityCore` that accepts a merge transform (applied to the stored payload inside the transaction) and a strict-validation flag. A pure RFC 7386 merge function produces the new payload; everything downstream (validation, transition engine, If-Match CAS, commit) is reused unchanged. HTTP selects the dialect via `Content-Type`; gRPC mirrors it with a generated `patchFormat` enum on a new `EntityPatchRequest` CloudEvent type. RFC 6902 is scaffolded (returns 501 / NOT_IMPLEMENTED).

**Tech Stack:** Go 1.26+, `log/slog`, oapi-codegen (HTTP from `api/openapi.yaml`), go-jsonschema (gRPC event types from `docs/cyoda/schema/`), testcontainers-go (E2E).

**Spec:** `docs/superpowers/specs/2026-06-24-entity-patch-design.md` (twice-reviewed). Issue #341; gRPC error follow-up #342.

## Global Constraints

- Go 1.26+; use `log/slog` only (never `log.Printf`/`fmt.Printf`).
- Wrap errors: `fmt.Errorf("...: %w", err)`. Use `uuid.UUID` not `string` for UUIDs in new code (note: existing `UpdateEntityInput.EntityID` is `string` — match it at the service boundary).
- TDD mandatory (Gate 1): failing test before implementation, every task.
- E2E coverage for user-facing behaviour (Gate 2): `internal/e2e/`.
- Security (Gate 3): never log tokens/secrets; every data path tenant-isolated; validate at boundaries; no stack traces/internals in responses.
- **No issue IDs** in shipped artefacts (error text, response bodies, code comments, OpenAPI/help content). Issue IDs only in commits/PR/spec docs.
- Precondition token is the entity **`transactionId`** (never call it "version" in any user-facing text).
- Merge must preserve numbers (operate on `json.Number`-decoded values; never re-coerce through `float64`).
- 4xx: full domain detail + error code. 5xx: generic message + ticket.
- Frequent commits — one per task minimum; the fidelity-reframe is its **own** commit (Task 9).

---

### Task 1: RFC 7386 merge function (pure, number-preserving)

**Files:**
- Create: `internal/domain/entity/mergepatch.go`
- Test: `internal/domain/entity/mergepatch_test.go`

**Interfaces:**
- Produces: `func mergeMergePatch(existing json.RawMessage, patch any) (any, error)` — decodes `existing` (number-preserving) and applies RFC 7386 merge of the already-parsed `patch` onto it, returning the merged value. `func applyMergePatch(target, patch any) any` — the recursive RFC 7386 §2 algorithm.
- Consumes: `decodeJSONPreservingNumbers` (existing, `service.go:29`).

- [ ] **Step 1: Write the failing test** (RFC 7386 Appendix A cases + null-delete, nested, array-replace, non-object, empty, number fidelity)

```go
package entity

import (
	"encoding/json"
	"reflect"
	"testing"
)

func mustParse(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := decodeJSONPreservingNumbers([]byte(s), &v); err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

func TestApplyMergePatch_RFC7386_AppendixA(t *testing.T) {
	cases := []struct{ name, target, patch, want string }{
		{"replace-field", `{"a":"b"}`, `{"a":"c"}`, `{"a":"c"}`},
		{"add-field", `{"a":"b"}`, `{"b":"c"}`, `{"a":"b","b":"c"}`},
		{"delete-field", `{"a":"b"}`, `{"a":null}`, `{}`},
		{"delete-one-of-two", `{"a":"b","b":"c"}`, `{"a":null}`, `{"b":"c"}`},
		{"array-replaces", `{"a":["b"]}`, `{"a":"c"}`, `{"a":"c"}`},
		{"scalar-replaces-array", `{"a":"c"}`, `{"a":["b"]}`, `{"a":["b"]}`},
		{"nested-merge", `{"a":{"b":"c"}}`, `{"a":{"b":"d","c":null}}`, `{"a":{"b":"d"}}`},
		{"array-wholesale", `{"a":[{"b":"c"}]}`, `{"a":[1]}`, `{"a":[1]}`},
		{"non-object-patch-replaces", `{"a":"b"}`, `["c"]`, `["c"]`},
		{"empty-patch-noop", `{"a":"b"}`, `{}`, `{"a":"b"}`},
		{"null-creates-nothing", `{"a":"foo"}`, `null`, `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergeMergePatch(json.RawMessage(tc.target), mustParse(t, tc.patch))
			if err != nil {
				t.Fatalf("merge: %v", err)
			}
			gotBytes, _ := json.Marshal(got)
			var gotN, wantN any
			_ = decodeJSONPreservingNumbers(gotBytes, &gotN)
			_ = decodeJSONPreservingNumbers([]byte(tc.want), &wantN)
			if !reflect.DeepEqual(gotN, wantN) {
				t.Errorf("got %s, want %s", gotBytes, tc.want)
			}
		})
	}
}

func TestApplyMergePatch_NumberFidelity(t *testing.T) {
	// int64 above 2^53 must survive without float64 coercion.
	target := json.RawMessage(`{"big":1}`)
	patch := mustParse(t, `{"big":9007199254740993}`)
	got, err := mergeMergePatch(target, patch)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	b, _ := json.Marshal(got)
	if string(b) != `{"big":9007199254740993}` {
		t.Errorf("number fidelity lost: got %s", b)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/domain/entity/ -run TestApplyMergePatch -v`
Expected: FAIL — `undefined: mergeMergePatch`.

- [ ] **Step 3: Implement the merge**

```go
package entity

import (
	"encoding/json"
	"fmt"
)

// mergeMergePatch applies an RFC 7386 JSON Merge Patch (the already-parsed
// patch) onto the stored entity data, returning the merged value. Both sides
// are number-preserving (json.Number) so large integers survive the merge.
func mergeMergePatch(existing json.RawMessage, patch any) (any, error) {
	var target any
	if len(existing) > 0 {
		if err := decodeJSONPreservingNumbers(existing, &target); err != nil {
			return nil, fmt.Errorf("failed to decode stored entity data: %w", err)
		}
	}
	return applyMergePatch(target, patch), nil
}

// applyMergePatch is the RFC 7386 section 2 algorithm. When patch is not a
// JSON object it replaces the target wholesale; otherwise each key is merged
// recursively, and an explicit null deletes the key.
func applyMergePatch(target, patch any) any {
	patchObj, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	targetObj, ok := target.(map[string]any)
	if !ok {
		targetObj = map[string]any{}
	}
	for k, v := range patchObj {
		if v == nil {
			delete(targetObj, k)
		} else {
			targetObj[k] = applyMergePatch(targetObj[k], v)
		}
	}
	return targetObj
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/domain/entity/ -run TestApplyMergePatch -v`
Expected: PASS (both tests, all sub-cases).

- [ ] **Step 5: Commit**

```bash
git add internal/domain/entity/mergepatch.go internal/domain/entity/mergepatch_test.go
git commit -m "feat(entity): RFC 7386 JSON merge patch function

Pure, number-preserving merge for partial entity updates (#341)."
```

---

### Task 2: New error codes (428, 415)

**Files:**
- Modify: `internal/common/error_codes.go` (the `const (...)` block)
- Test: `internal/common/error_codes_test.go` (create if absent)

**Interfaces:**
- Produces: `common.ErrCodePreconditionRequired = "PRECONDITION_REQUIRED"`, `common.ErrCodeUnsupportedMediaType = "UNSUPPORTED_MEDIA_TYPE"`. (`ErrCodeNotImplemented` and `ErrCodeConflict` already exist.)

- [ ] **Step 1: Write the failing test**

```go
package common

import "testing"

func TestPatchErrorCodes(t *testing.T) {
	if ErrCodePreconditionRequired != "PRECONDITION_REQUIRED" {
		t.Errorf("got %q", ErrCodePreconditionRequired)
	}
	if ErrCodeUnsupportedMediaType != "UNSUPPORTED_MEDIA_TYPE" {
		t.Errorf("got %q", ErrCodeUnsupportedMediaType)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/common/ -run TestPatchErrorCodes -v`
Expected: FAIL — `undefined: ErrCodePreconditionRequired`.

- [ ] **Step 3: Add the constants** to the `const` block in `internal/common/error_codes.go` (after `ErrCodeNotFound`):

```go
	ErrCodePreconditionRequired      = "PRECONDITION_REQUIRED"
	ErrCodeUnsupportedMediaType      = "UNSUPPORTED_MEDIA_TYPE"
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/common/ -run TestPatchErrorCodes -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/common/error_codes.go internal/common/error_codes_test.go
git commit -m "feat(common): PRECONDITION_REQUIRED and UNSUPPORTED_MEDIA_TYPE error codes (#341)"
```

---

### Task 3: Service layer — `updateEntityCore` seam + `PatchEntity`

**Files:**
- Modify: `internal/domain/entity/service.go` (refactor `UpdateEntity` ~960; add `PatchEntity`, `PatchEntityInput`, `updateOptions`)
- Modify: `internal/domain/entity/handler.go` (add `validateStrict`)
- Test: `internal/domain/entity/patch_test.go` (create)

**Interfaces:**
- Consumes: `mergeMergePatch` (Task 1); `common.ErrCodePreconditionRequired`/`ErrCodeUnsupportedMediaType`/`ErrCodeNotImplemented` (Task 2); existing `validateOrExtend`, `classifyValidateOrExtendErr`, `enrichWithModelRef`, `validationErrorsToError`, `schema.Unmarshal`, `schema.Validate`, `errInternalSchema`.
- Produces:
  - `type PatchEntityInput struct { EntityID string; Patch json.RawMessage; PatchFormat string; Transition string; IfMatch string }`
  - `func (h *Handler) PatchEntity(ctx context.Context, input PatchEntityInput) (*EntityTransactionResult, error)`
  - `type updateOptions struct { merge func(existing json.RawMessage, incoming any) (any, error); strictValidate bool }`
  - `func (h *Handler) validateStrict(desc *spi.ModelDescriptor, parsedData any) error`

- [ ] **Step 1: Refactor `UpdateEntity` into `updateEntityCore` (pure refactor — existing tests must stay green).**

In `service.go`, rename the existing `func (h *Handler) UpdateEntity(...)` body to `updateEntityCore` with an added `opts updateOptions` parameter, and add a thin wrapper. Add the `updateOptions` type near `UpdateEntityInput`:

```go
// updateOptions tunes the shared update flow. Zero value = plain replace (PUT).
type updateOptions struct {
	// merge, when non-nil, transforms the parsed request body against the
	// existing entity's stored data inside the transaction, before validation
	// (RFC 7386 for PATCH).
	merge func(existing json.RawMessage, incoming any) (any, error)
	// strictValidate forces validate-only — never extend the model schema (PATCH).
	strictValidate bool
}

func (h *Handler) UpdateEntity(ctx context.Context, input UpdateEntityInput) (*EntityTransactionResult, error) {
	return h.updateEntityCore(ctx, input, updateOptions{})
}

func (h *Handler) updateEntityCore(ctx context.Context, input UpdateEntityInput, opts updateOptions) (*EntityTransactionResult, error) {
	// ... existing UpdateEntity body, with the two edits in Step 2 ...
}
```

- [ ] **Step 2: Insert the merge hook and strict-validation branch.**

Locate the model-descriptor load (the line `desc, err := modelStore.Get(txCtx, existing.Meta.ModelRef)` — the descriptor used by `validateOrExtend`), which sits between the `existing, err := entityStore.Get(...)` 404 check and the `h.validateOrExtend(txCtx, modelStore, desc, parsedData)` call (~`service.go:1015`). **Immediately before** the `validateOrExtend` call, insert the merge block, and replace the single `validateOrExtend` call with the strict/extend branch:

```go
	// PATCH: merge the sparse body onto the stored data before validation,
	// inside this transaction so the merge base is the version being overwritten.
	if opts.merge != nil {
		merged, mErr := opts.merge(existing.Data, parsedData)
		if mErr != nil {
			h.txMgr.Rollback(txCtx, txID)
			return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid patch: "+mErr.Error())
		}
		parsedData = merged
		bodyBytes, err = json.Marshal(parsedData)
		if err != nil {
			h.txMgr.Rollback(txCtx, txID)
			return nil, common.Internal("failed to serialize merged entity", err)
		}
	}

	// Validate the (possibly merged) result. PATCH validates strictly and never
	// extends the model; PUT may extend per the model's ChangeLevel.
	if opts.strictValidate {
		if vErr := h.validateStrict(desc, parsedData); vErr != nil {
			h.txMgr.Rollback(txCtx, txID)
			return nil, classifyValidateOrExtendErr(vErr)
		}
	} else {
		if vErr := h.validateOrExtend(txCtx, modelStore, desc, parsedData); vErr != nil {
			h.txMgr.Rollback(txCtx, txID)
			return nil, classifyValidateOrExtendErr(vErr)
		}
	}
```

(Remove the original standalone `if err := h.validateOrExtend(...)` block this replaces.)

- [ ] **Step 3: Add `validateStrict` to `handler.go`** (mirrors `validateOrExtend`'s `ChangeLevel == ""` branch, never extends):

```go
// validateStrict validates parsedData against the model schema WITHOUT
// extending it. PATCH uses this: a sparse delta must never widen the tenant's
// model (a stray/typo'd key is rejected, not absorbed). Mirrors the
// ChangeLevel=="" branch of validateOrExtend.
func (h *Handler) validateStrict(desc *spi.ModelDescriptor, parsedData any) error {
	modelNode, err := schema.Unmarshal(desc.Schema)
	if err != nil {
		return fmt.Errorf("%w: failed to unmarshal model schema: %w", errInternalSchema, err)
	}
	errs := schema.Validate(modelNode, parsedData)
	if len(errs) > 0 {
		return enrichWithModelRef(validationErrorsToError(errs), desc.Ref)
	}
	return nil
}
```

- [ ] **Step 4: Run the existing entity tests to confirm the refactor is green.**

Run: `go test ./internal/domain/entity/ -v`
Expected: PASS (all pre-existing tests; the refactor changed no behaviour for PUT).

- [ ] **Step 5: Write the failing PatchEntity test** (`patch_test.go`).

Use the package's existing entity-test setup helpers (mirror those used in the existing `service`/`handler` tests in this package — same factory/txMgr/engine wiring used by `UpdateEntity` tests). The test imports a small model, creates an entity, then patches it.

```go
package entity

import (
	"net/http"
	"testing"
)

func TestPatchEntity_MergeAndStrictValidate(t *testing.T) {
	h, ctx := newPatchTestHandler(t) // helper: see Step 6
	id := createPersonEntity(t, h, ctx, map[string]any{"name": "Alice", "age": 30})
	txid := getTxID(t, h, ctx, id)

	// Patch only "age"; "name" must be preserved.
	res, err := h.PatchEntity(ctx, PatchEntityInput{
		EntityID:    id,
		Patch:       []byte(`{"age":31}`),
		PatchFormat: "MERGE_PATCH",
		IfMatch:     txid,
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if len(res.EntityIDs) != 1 {
		t.Fatalf("expected 1 entity id")
	}
	got := getEntityData(t, h, ctx, id)
	if got["name"] != "Alice" || got["age"].(json.Number).String() != "31" {
		t.Errorf("merge wrong: %#v", got)
	}
}

func TestPatchEntity_JSONPatchNotImplemented(t *testing.T) {
	h, ctx := newPatchTestHandler(t)
	id := createPersonEntity(t, h, ctx, map[string]any{"name": "A"})
	_, err := h.PatchEntity(ctx, PatchEntityInput{
		EntityID: id, Patch: []byte(`[]`), PatchFormat: "JSON_PATCH", IfMatch: "*",
	})
	assertStatus(t, err, http.StatusNotImplemented)
}

func TestPatchEntity_StrictRejectsNewField(t *testing.T) {
	h, ctx := newPatchTestHandler(t) // model is locked & strict (ChangeLevel "")
	id := createPersonEntity(t, h, ctx, map[string]any{"name": "A"})
	_, err := h.PatchEntity(ctx, PatchEntityInput{
		EntityID: id, Patch: []byte(`{"newfield":"x"}`), PatchFormat: "MERGE_PATCH", IfMatch: "*",
	})
	assertStatus(t, err, http.StatusBadRequest)
}

func TestPatchEntity_StaleTokenIs412(t *testing.T) {
	h, ctx := newPatchTestHandler(t)
	id := createPersonEntity(t, h, ctx, map[string]any{"name": "A", "age": 1})
	stale := getTxID(t, h, ctx, id)
	// Advance the entity so the token goes stale.
	if _, err := h.PatchEntity(ctx, PatchEntityInput{EntityID: id, Patch: []byte(`{"age":2}`), PatchFormat: "MERGE_PATCH", IfMatch: stale}); err != nil {
		t.Fatalf("first patch: %v", err)
	}
	_, err := h.PatchEntity(ctx, PatchEntityInput{EntityID: id, Patch: []byte(`{"age":3}`), PatchFormat: "MERGE_PATCH", IfMatch: stale})
	assertStatus(t, err, http.StatusPreconditionFailed)
}

func TestPatchEntity_StarIsUnconditional(t *testing.T) {
	h, ctx := newPatchTestHandler(t)
	id := createPersonEntity(t, h, ctx, map[string]any{"name": "A", "age": 1})
	if _, err := h.PatchEntity(ctx, PatchEntityInput{EntityID: id, Patch: []byte(`{"age":2}`), PatchFormat: "MERGE_PATCH", IfMatch: "*"}); err != nil {
		t.Fatalf("star patch should succeed unconditionally: %v", err)
	}
}
```

Add the small helpers (`newPatchTestHandler`, `createPersonEntity`, `getTxID`, `getEntityData`, `assertStatus`) at the bottom of `patch_test.go`, modelled on the existing entity-package test setup. `assertStatus`:

```go
func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %v", err)
	}
	if appErr.Status != want {
		t.Fatalf("status: got %d want %d (%s)", appErr.Status, want, appErr.Message)
	}
}
```

- [ ] **Step 6: Run to verify the new tests fail**

Run: `go test ./internal/domain/entity/ -run TestPatchEntity -v`
Expected: FAIL — `undefined: PatchEntity` / `PatchEntityInput`.

- [ ] **Step 7: Implement `PatchEntity` + `PatchEntityInput`** in `service.go`:

```go
// PatchEntityInput holds parameters for a partial (RFC 7386) entity update.
// IfMatch is required in some form (a transactionId, or "*" for unconditional);
// its absence is rejected as 428 at the HTTP/gRPC edge, not here.
type PatchEntityInput struct {
	EntityID    string
	Patch       json.RawMessage
	PatchFormat string // "MERGE_PATCH" | "JSON_PATCH"
	Transition  string
	IfMatch     string
}

// PatchEntity applies a partial update. RFC 7386 merge patch is implemented;
// RFC 6902 (JSON_PATCH) is scaffolded and returns 501.
func (h *Handler) PatchEntity(ctx context.Context, input PatchEntityInput) (*EntityTransactionResult, error) {
	switch input.PatchFormat {
	case "MERGE_PATCH":
		// handled below
	case "JSON_PATCH":
		return nil, common.Operational(http.StatusNotImplemented, common.ErrCodeNotImplemented,
			"RFC 6902 JSON Patch is not implemented; use application/merge-patch+json")
	default:
		return nil, common.Operational(http.StatusUnsupportedMediaType, common.ErrCodeUnsupportedMediaType,
			"unsupported patch format")
	}

	// "*" = unconditional: drop the CAS token. Existence is still guaranteed by
	// the in-transaction Get inside updateEntityCore (404 if the entity is gone).
	ifMatch := input.IfMatch
	if ifMatch == "*" {
		ifMatch = ""
	}

	return h.updateEntityCore(ctx, UpdateEntityInput{
		EntityID:   input.EntityID,
		Format:     "JSON",
		Data:       input.Patch,
		Transition: input.Transition,
		IfMatch:    ifMatch,
	}, updateOptions{
		merge:          mergeMergePatch,
		strictValidate: true,
	})
}
```

- [ ] **Step 8: Run to verify the new tests pass**

Run: `go test ./internal/domain/entity/ -run TestPatchEntity -v`
Expected: PASS.

- [ ] **Step 9: Run the whole package (no regressions)**

Run: `go test ./internal/domain/entity/ -v`
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/domain/entity/service.go internal/domain/entity/handler.go internal/domain/entity/patch_test.go
git commit -m "feat(entity): PatchEntity via shared updateEntityCore merge hook

RFC 7386 merge applied inside the update transaction; strict validate-only
(no schema extension); If-Match: * -> unconditional; JSON_PATCH -> 501 (#341)."
```

---

### Task 4: HTTP PATCH endpoints (OpenAPI + handler)

**Files:**
- Modify: `api/openapi.yaml` (add two `patch:` operations)
- Regenerate: `api/generated.go` (via oapi-codegen)
- Modify: `internal/domain/entity/handler.go` (add `PatchSingleWithLoopback`, `PatchSingle`, shared `patch`, `patchFormatFromContentType`)
- Test: `internal/domain/entity/handler_patch_test.go` (create)

**Interfaces:**
- Consumes: `h.PatchEntity` (Task 3); generated `genapi.PatchSingleParams{IfMatch *string}`, `genapi.PatchSingleParamsFormat`, etc. (after regen).
- Produces: HTTP routes `PATCH /entity/{format}/{entityId}` and `PATCH /entity/{format}/{entityId}/{transition}`.

- [ ] **Step 1: Add the two PATCH operations to `api/openapi.yaml`.**

Under the existing `/entity/{format}/{entityId}:` path object (which currently has only `put:`), add a sibling `patch:` key. Mirror the PUT `updateSingleWithLoopback` parameters (format, entityId, If-Match header, the two query params) but with `operationId: patchSingleWithLoopback`, this requestBody, and these responses:

```yaml
    patch:
      tags:
        - Entity Management
      summary: Patch Single with a loopback transition
      description: |
        Partially updates a single entity (RFC 7386 JSON Merge Patch) with a
        loopback transition. Send only the fields that change; an explicit null
        deletes a key. The merged result is validated against the model schema
        and cannot introduce a new field (unlike PUT). The dialect is selected
        by Content-Type: application/merge-patch+json (RFC 7386, implemented) or
        application/json-patch+json (RFC 6902, not implemented -> 501).

        If-Match is required in some form: the transactionId from your last GET
        of this entity (conditional; 412 if it has moved), or "*" to accept
        last-writer-wins. Its absence returns 428.
      operationId: patchSingleWithLoopback
      parameters:
        - name: format
          in: path
          description: Must be JSON (merge patch is JSON only)
          required: true
          schema:
            type: string
            enum:
              - JSON
              - XML
        - name: entityId
          in: path
          description: The UUID of the entity to patch
          required: true
          schema:
            type: string
            format: uuid
        - name: If-Match
          in: header
          description: transactionId from the last read, or "*" for unconditional. Absent returns 428.
          required: false
          schema:
            type: string
      requestBody:
        required: true
        content:
          application/merge-patch+json:
            schema:
              type: object
              format: json
            examples:
              MergePatch:
                summary: Change one field
                value:
                  year: "2025"
          application/json-patch+json:
            schema:
              type: array
              items:
                type: object
      responses:
        "200":
          description: Entity patched successfully
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/EntityTransactionResponse"
        "400":
          description: Bad request — invalid patch or validation failure
          content:
            application/problem+json:
              schema:
                $ref: "#/components/schemas/ProblemDetail"
        "404":
          description: Entity not found
          content:
            application/problem+json:
              schema:
                $ref: "#/components/schemas/ProblemDetail"
        "409":
          description: Conflict — concurrent writer committed during the patch (retryable)
          content:
            application/problem+json:
              schema:
                $ref: "#/components/schemas/ProblemDetail"
        "412":
          description: Precondition Failed — If-Match transactionId no longer matches
          content:
            application/problem+json:
              schema:
                $ref: "#/components/schemas/ProblemDetail"
        "415":
          description: Unsupported Media Type — non-JSON format or unrecognised Content-Type
          content:
            application/problem+json:
              schema:
                $ref: "#/components/schemas/ProblemDetail"
        "428":
          description: Precondition Required — If-Match absent; supply a transactionId or "*"
          content:
            application/problem+json:
              schema:
                $ref: "#/components/schemas/ProblemDetail"
        "501":
          $ref: '#/components/responses/NotImplemented'
        "401":
          $ref: '#/components/responses/Unauthorized'
        "403":
          $ref: '#/components/responses/Forbidden'
        default:
          $ref: '#/components/responses/InternalServerError'
```

Add the analogous `patch:` under `/entity/{format}/{entityId}/{transition}:` with `operationId: patchSingle`, the extra `transition` path parameter (copy it verbatim from the PUT `updateSingle` operation), and the identical requestBody/responses.

- [ ] **Step 2: Regenerate the HTTP layer**

Run: `cd api && go tool oapi-codegen --config=config.yaml openapi.yaml && cd ..`
Expected: `api/generated.go` now declares `PatchSingleWithLoopback`/`PatchSingle` in `ServerInterface`, the `PatchSingleParams`/`PatchSingleWithLoopbackParams` structs (with `IfMatch *string`), the `PatchSingleParamsFormat` enum, and route registrations for `http.MethodPatch`.

- [ ] **Step 3: Confirm it fails to compile** (entity.Handler does not yet implement the new interface methods)

Run: `go build ./... 2>&1 | head`
Expected: compile error — `*entity.Handler` does not implement `genapi.ServerInterface` (missing `PatchSingle`, `PatchSingleWithLoopback`).

- [ ] **Step 4: Write the failing handler test** (`handler_patch_test.go`) — drive the handler via `httptest` for the edge codes (415/428/501) that don't need a full store:

```go
package entity

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openapi_types "github.com/oapi-codegen/runtime/types"
)

func TestPatch_XMLFormatIs415(t *testing.T) {
	h, _ := newPatchTestHandler(t)
	r := httptest.NewRequest(http.MethodPatch, "/entity/XML/"+sampleUUID, strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/merge-patch+json")
	r.Header.Set("If-Match", "*")
	w := httptest.NewRecorder()
	h.PatchSingleWithLoopback(w, r, "XML", openapi_types.UUID(mustUUID(sampleUUID)), genapiPatchLoopbackParams("*"))
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d", w.Code)
	}
}

func TestPatch_BadContentTypeIs415(t *testing.T) {
	h, _ := newPatchTestHandler(t)
	r := httptest.NewRequest(http.MethodPatch, "/entity/JSON/"+sampleUUID, strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.PatchSingleWithLoopback(w, r, "JSON", openapi_types.UUID(mustUUID(sampleUUID)), genapiPatchLoopbackParams("*"))
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d", w.Code)
	}
}

func TestPatch_MissingIfMatchIs428(t *testing.T) {
	h, _ := newPatchTestHandler(t)
	r := httptest.NewRequest(http.MethodPatch, "/entity/JSON/"+sampleUUID, strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/merge-patch+json")
	w := httptest.NewRecorder()
	h.PatchSingleWithLoopback(w, r, "JSON", openapi_types.UUID(mustUUID(sampleUUID)), genapiPatchLoopbackParams(""))
	if w.Code != http.StatusPreconditionRequired {
		t.Fatalf("got %d", w.Code)
	}
}
```

Add helpers `genapiPatchLoopbackParams(ifMatch string) genapi.PatchSingleWithLoopbackParams` (returns the struct with `IfMatch` nil when `ifMatch==""`, else `&ifMatch`), `mustUUID`, and a `sampleUUID` const, at the bottom of the file.

- [ ] **Step 5: Run to verify it fails**

Run: `go test ./internal/domain/entity/ -run TestPatch_ -v`
Expected: FAIL — undefined `PatchSingleWithLoopback`.

- [ ] **Step 6: Implement the handlers** in `handler.go` (add `mime` to imports):

```go
// PatchSingleWithLoopback handles PATCH /entity/{format}/{entityId} (loopback).
func (h *Handler) PatchSingleWithLoopback(w http.ResponseWriter, r *http.Request, format genapi.PatchSingleWithLoopbackParamsFormat, entityId openapi_types.UUID, params genapi.PatchSingleWithLoopbackParams) {
	h.patch(w, r, string(format), entityId, "", params.IfMatch)
}

// PatchSingle handles PATCH /entity/{format}/{entityId}/{transition}.
func (h *Handler) PatchSingle(w http.ResponseWriter, r *http.Request, format genapi.PatchSingleParamsFormat, entityId openapi_types.UUID, transition string, params genapi.PatchSingleParams) {
	h.patch(w, r, string(format), entityId, transition, params.IfMatch)
}

// patch is the shared PATCH implementation. Error precedence: media-type/format
// (415) -> If-Match presence (428) -> service (404/412/409/501/4xx).
func (h *Handler) patch(w http.ResponseWriter, r *http.Request, format string, entityId openapi_types.UUID, transition string, ifMatchHeader *string) {
	if format != "JSON" {
		common.WriteError(w, r, common.Operational(http.StatusUnsupportedMediaType, common.ErrCodeUnsupportedMediaType, "patch supports the JSON format only"))
		return
	}
	patchFormat, ok := patchFormatFromContentType(r.Header.Get("Content-Type"))
	if !ok {
		common.WriteError(w, r, common.Operational(http.StatusUnsupportedMediaType, common.ErrCodeUnsupportedMediaType,
			"unsupported Content-Type; use application/merge-patch+json or application/json-patch+json"))
		return
	}
	if ifMatchHeader == nil {
		common.WriteError(w, r, common.Operational(http.StatusPreconditionRequired, common.ErrCodePreconditionRequired,
			"missing If-Match: send If-Match: <transactionId> from your last GET of this entity to patch safely, or If-Match: * to explicitly accept last-writer-wins"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxEntityBodySize)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read body"))
		return
	}
	result, err := h.PatchEntity(r.Context(), PatchEntityInput{
		EntityID:    entityId.String(),
		Patch:       bodyBytes,
		PatchFormat: patchFormat,
		Transition:  transition,
		IfMatch:     *ifMatchHeader,
	})
	if err != nil {
		common.WriteError(w, r, classifyError(err))
		return
	}
	common.WriteJSON(w, http.StatusOK, map[string]any{
		"transactionId": result.TransactionID,
		"entityIds":     result.EntityIDs,
	})
}

// patchFormatFromContentType maps the request Content-Type to a patch dialect.
func patchFormatFromContentType(ct string) (string, bool) {
	if ct == "" {
		return "", false
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return "", false
	}
	switch mediaType {
	case "application/merge-patch+json":
		return "MERGE_PATCH", true
	case "application/json-patch+json":
		return "JSON_PATCH", true
	default:
		return "", false
	}
}
```

- [ ] **Step 7: Run handler tests + build**

Run: `go test ./internal/domain/entity/ -run TestPatch_ -v && go build ./...`
Expected: PASS and clean build.

- [ ] **Step 8: Commit**

```bash
git add api/openapi.yaml api/generated.go internal/domain/entity/handler.go internal/domain/entity/handler_patch_test.go
git commit -m "feat(api): HTTP PATCH entity endpoints (RFC 7386 merge patch)

Dialect via Content-Type; JSON only (XML->415); If-Match three-state
(token/412, *, absent->428); json-patch+json->501 (#341)."
```

---

### Task 5: gRPC EntityPatchRequest

**Files:**
- Create: `docs/cyoda/schema/common/PatchFormat.json`, `docs/cyoda/schema/entity/EntityPatchPayload.json`, `docs/cyoda/schema/entity/EntityPatchRequest.json`
- Modify: `scripts/generate-events.sh` (add PatchFormat.json to the explicit common list)
- Regenerate: `api/grpc/events/types.go`
- Modify: `internal/grpc/cloudevent_types.go` (add `EntityPatchRequest` const)
- Modify: `internal/grpc/entity.go` (add `case EntityPatchRequest`; add `net/http` import)
- Test: `internal/grpc/rpc_test.go` (add `TestRPC_EntityPatch*`)

**Interfaces:**
- Consumes: `s.entityHandler.PatchEntity` (Task 3); generated `events.EntityPatchRequestJson{ Payload EntityPatchPayloadJson; PatchFormat PatchFormatJson }`, `events.EntityPatchPayloadJson{ EntityID string; Patch interface{}; Transition *string; IfMatch string }`.

- [ ] **Step 1: Author the schema files.**

`docs/cyoda/schema/common/PatchFormat.json`:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://cyoda.com/cloud/event/common/PatchFormat.json",
  "title": "PatchFormat",
  "type": "string",
  "enum": [
    "MERGE_PATCH",
    "JSON_PATCH"
  ],
  "description": "Patch dialect: MERGE_PATCH (RFC 7386, implemented) or JSON_PATCH (RFC 6902, not implemented)."
}
```

`docs/cyoda/schema/entity/EntityPatchPayload.json`:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://cyoda.com/cloud/event/entity/EntityPatchPayload.json",
  "title": "EntityPatchPayload",
  "type": "object",
  "properties": {
    "entityId": {
      "type": "string",
      "description": "ID of the entity to patch.",
      "format": "uuid"
    },
    "patch": {
      "type": "any",
      "existingJavaType": "com.fasterxml.jackson.databind.JsonNode",
      "description": "The patch document (RFC 7386 merge patch)."
    },
    "transition": {
      "type": "string",
      "description": "Transition to apply, or omit for loopback."
    },
    "ifMatch": {
      "type": "string",
      "description": "transactionId from the last read, or \"*\" for unconditional. Empty is rejected as precondition-required."
    }
  },
  "required": [
    "entityId",
    "patch"
  ]
}
```

`docs/cyoda/schema/entity/EntityPatchRequest.json`:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://cyoda.com/cloud/event/entity/EntityPatchRequest.json",
  "title": "EntityPatchRequest",
  "type": "object",
  "extends": {
    "$ref": "../common/BaseEvent.json"
  },
  "properties": {
    "payload": {
      "$ref": "./EntityPatchPayload.json",
      "description": "Data payload containing the entity id and patch."
    },
    "patchFormat": {
      "$ref": "../common/PatchFormat.json"
    },
    "transactionTimeoutMs": {
      "type": "integer",
      "existingJavaType": "java.lang.Long",
      "description": "Indicates the timeout of transaction for transactional save."
    }
  },
  "required": [
    "patchFormat",
    "payload"
  ]
}
```

- [ ] **Step 2: Add PatchFormat.json to the generator's explicit common list.**

In `scripts/generate-events.sh`, in the `"$TOOL" ...` invocation, add this line alongside the other `common/*.json` entries (e.g. right after the `DataFormat.json` line):

```bash
  "$CLEAN_DIR"/common/PatchFormat.json \
```

(The `entity/*.json` glob already picks up the two new entity schemas.)

- [ ] **Step 3: Regenerate event types.**

Run: `./scripts/generate-events.sh`
(If the tool is missing: `go install github.com/atombender/go-jsonschema@latest` first.)
Expected: `api/grpc/events/types.go` now contains `EntityPatchRequestJson`, `EntityPatchPayloadJson`, and `PatchFormatJson` (with `MERGE_PATCH`/`JSON_PATCH` constants and an `UnmarshalJSON` validator).

- [ ] **Step 4: Add the CloudEvent type constant** to `internal/grpc/cloudevent_types.go` (in the entity-management `const` block, after `EntityUpdateRequest`):

```go
	EntityPatchRequest            = "EntityPatchRequest"
```

- [ ] **Step 5: Write the failing gRPC test** (`rpc_test.go`), mirroring `TestRPC_EntityCreate`/`TestRPC_EntityTransition`:

```go
func TestRPC_EntityPatch(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice", "age": 30})

	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id": "c1", "dataFormat": "JSON",
		"payload": map[string]any{"model": map[string]any{"name": "person", "version": 1}, "data": map[string]any{"name": "Alice", "age": 30}},
	})
	createResp, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	entityID := parseResponsePayload(t, createResp)["transactionInfo"].(map[string]any)["entityIds"].([]any)[0].(string)

	patchCE := makeCE(EntityPatchRequest, map[string]any{
		"id": "p1", "patchFormat": "MERGE_PATCH",
		"payload": map[string]any{"entityId": entityID, "patch": map[string]any{"age": 31}, "ifMatch": "*"},
	})
	resp, err := svc.EntityManage(ctx, patchCE)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("expected success; got %#v", typed.Error)
	}
}

func TestRPC_EntityPatch_MissingIfMatch428(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "A"})
	createResp, _ := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "c1", "dataFormat": "JSON",
		"payload": map[string]any{"model": map[string]any{"name": "person", "version": 1}, "data": map[string]any{"name": "A"}},
	}))
	entityID := parseResponsePayload(t, createResp)["transactionInfo"].(map[string]any)["entityIds"].([]any)[0].(string)

	patchCE := makeCE(EntityPatchRequest, map[string]any{
		"id": "p1", "patchFormat": "MERGE_PATCH",
		"payload": map[string]any{"entityId": entityID, "patch": map[string]any{"name": "B"}, "ifMatch": ""},
	})
	resp, err := svc.EntityManage(ctx, patchCE)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success || typed.Error == nil {
		t.Fatalf("expected failure envelope")
	}
	if !strings.Contains(typed.Error.Message, "PRECONDITION_REQUIRED") {
		t.Errorf("expected PRECONDITION_REQUIRED in message, got %q", typed.Error.Message)
	}
}
```

- [ ] **Step 6: Run to verify it fails**

Run: `go test ./internal/grpc/ -run TestRPC_EntityPatch -v`
Expected: FAIL — no `case EntityPatchRequest` (dispatch returns an "unknown type" error).

- [ ] **Step 7: Add the dispatch case** in `internal/grpc/entity.go` `EntityManage` (after the `EntityUpdateRequest` case; add `"net/http"` to imports):

```go
	case EntityPatchRequest:
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		var req events.EntityPatchRequestJson
		if err := dec.Decode(&req); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid payload: %v", err)
		}
		if req.Payload.IfMatch == "" {
			return entityTransactionError(ctx, ce.Id, common.Operational(
				http.StatusPreconditionRequired, common.ErrCodePreconditionRequired,
				"missing ifMatch: provide the transactionId from your last read, or \"*\" to accept last-writer-wins"))
		}
		patchBytes, err := json.Marshal(req.Payload.Patch)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "failed to marshal patch: %v", err)
		}
		transition := ""
		if req.Payload.Transition != nil {
			transition = *req.Payload.Transition
		}
		result, err := s.entityHandler.PatchEntity(ctx, entity.PatchEntityInput{
			EntityID:    req.Payload.EntityID,
			Patch:       patchBytes,
			PatchFormat: string(req.PatchFormat),
			Transition:  transition,
			IfMatch:     req.Payload.IfMatch,
		})
		if err != nil {
			slog.Error("operation failed", "pkg", "grpc", "rpc", "entityManage", "type", eventType, "ceId", ce.Id, "error", err.Error())
			return entityTransactionError(ctx, ce.Id, err)
		}
		diag := common.GetDiagnostics(ctx)
		resp := events.EntityTransactionResponseJson{
			ID:        ce.Id,
			Success:   true,
			Warnings:  diag.GetWarnings(),
			RequestID: ce.Id,
			TransactionInfo: events.EntityTransactionInfoJson{EntityIds: result.EntityIDs},
		}
		if result.TransactionID != "" {
			resp.TransactionInfo.TransactionID = &result.TransactionID
		}
		return NewCloudEvent(EntityTransactionResponse, resp)
```

- [ ] **Step 8: Run to verify it passes**

Run: `go test ./internal/grpc/ -run TestRPC_EntityPatch -v && go build ./...`
Expected: PASS and clean build.

- [ ] **Step 9: Commit**

```bash
git add docs/cyoda/schema/common/PatchFormat.json docs/cyoda/schema/entity/EntityPatchPayload.json docs/cyoda/schema/entity/EntityPatchRequest.json scripts/generate-events.sh api/grpc/events/types.go internal/grpc/cloudevent_types.go internal/grpc/entity.go internal/grpc/rpc_test.go
git commit -m "feat(grpc): EntityPatchRequest CloudEvent (RFC 7386 merge patch)

New event type + generated patchFormat enum; dispatches to PatchEntity;
empty ifMatch -> PRECONDITION_REQUIRED envelope (#341)."
```

---

### Task 6: E2E coverage (HTTP through the full stack)

**Files:**
- Create: `internal/e2e/entity_patch_test.go`

**Interfaces:**
- Consumes: the e2e harness (`TestMain` PostgreSQL + httptest server, JWT auth) and existing helpers used by entity E2E tests in `internal/e2e/` (model import/lock, entity create, authed HTTP client). Mirror an existing `*_test.go` in that package for the exact helper names.

- [ ] **Step 1: Write the failing E2E test** covering the contract surface. Use the package's existing authed-request helper (match its signature to a sibling test) — sketch:

```go
//go:build !short

package e2e

import (
	"net/http"
	"testing"
)

func TestE2E_EntityPatch_MergeAndPreconditions(t *testing.T) {
	c := newE2EClient(t) // existing harness client
	importLockModel(t, c, "person", "1", map[string]any{"name": "Alice", "age": 30})
	id, txid := createEntity(t, c, "person", "1", map[string]any{"name": "Alice", "age": 30})

	// Happy path: merge-patch one field with a valid token.
	resp := c.patch(t, "/api/entity/JSON/"+id, "application/merge-patch+json", txid, `{"age":31}`)
	if resp.status != http.StatusOK {
		t.Fatalf("merge patch: got %d", resp.status)
	}
	got := getEntity(t, c, id)
	if got["name"] != "Alice" || got["age"].(string) != "31" {
		t.Errorf("merge wrong: %#v", got)
	}

	// XML format -> 415.
	if r := c.patch(t, "/api/entity/XML/"+id, "application/merge-patch+json", "*", `{}`); r.status != http.StatusUnsupportedMediaType {
		t.Errorf("XML: got %d", r.status)
	}
	// Wrong Content-Type -> 415.
	if r := c.patch(t, "/api/entity/JSON/"+id, "application/json", "*", `{}`); r.status != http.StatusUnsupportedMediaType {
		t.Errorf("bad CT: got %d", r.status)
	}
	// Missing If-Match -> 428.
	if r := c.patchNoIfMatch(t, "/api/entity/JSON/"+id, "application/merge-patch+json", `{}`); r.status != http.StatusPreconditionRequired {
		t.Errorf("no If-Match: got %d", r.status)
	}
	// json-patch+json -> 501.
	if r := c.patch(t, "/api/entity/JSON/"+id, "application/json-patch+json", "*", `[]`); r.status != http.StatusNotImplemented {
		t.Errorf("json-patch: got %d", r.status)
	}
	// Stale token -> 412.
	if r := c.patch(t, "/api/entity/JSON/"+id, "application/merge-patch+json", txid, `{"age":99}`); r.status != http.StatusPreconditionFailed {
		t.Errorf("stale token: got %d", r.status)
	}
}

func TestE2E_EntityPatch_TransitionRunsAfterMerge(t *testing.T) {
	// Patch sets a field AND names a transition whose processor overwrites it;
	// assert the processor's value wins (merge-then-processor ordering).
	// Use a model/workflow fixture with a mutating processor on the transition.
	// ... mirror an existing workflow E2E fixture ...
}
```

Provide the `c.patch` / `c.patchNoIfMatch` helpers in the test file (set method PATCH, `Content-Type`, optional `If-Match`, body), modelled on the harness's existing request helper. If the package lacks a fixture with a mutating processor, build the smallest one mirroring an existing workflow E2E test; if that proves out of proportion, assert ordering at the service level in Task 3 instead and `log`/comment the substitution here (do not silently drop it).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/e2e/ -run TestE2E_EntityPatch -v`
Expected: FAIL (routes/behaviour exercised end-to-end; fails until Tasks 4 are wired into the running server — they are, so failures here are assertion-level until helpers compile).

- [ ] **Step 3: Make it pass** — adjust helper names to the harness, fix any assertion mismatches. No production code should be needed beyond Tasks 1–5; if a gap surfaces, fix it under TDD in the owning task.

- [ ] **Step 4: Run the full E2E + unit suite**

Run: `go test ./... 2>&1 | tail -30`
Expected: PASS (requires Docker for E2E).

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/entity_patch_test.go
git commit -m "test(e2e): entity PATCH through the full HTTP stack

merge happy-path, 415/428/501/412, merge-then-transition ordering (#341)."
```

---

### Task 7: Cloud-parity contract docs

**Files:**
- Create: `docs/cloud-parity/README.md`, `docs/cloud-parity/entity-patch.md`

**Interfaces:** none (documentation).

- [ ] **Step 1: Write `docs/cloud-parity/README.md`** — state the convention: cyoda-go leads the contract; this folder holds Cloud-facing implementation specs; no shared ticketing yet; cross-link `../cyoda/cloud-divergences.md` as the inverse vector (fields cyoda-go declares but does not yet implement). ~15 lines.

- [ ] **Step 2: Write `docs/cloud-parity/entity-patch.md`** — the PATCH contract for Cloud to implement, derived from the spec §4–§8: RFC 7386 merge semantics; the three-state `If-Match` precondition (transactionId / `*` / absent→428) with the educational 428 body; HTTP surface (verbs, media types, JSON-only, error table incl. 409 vs 412 and precedence); gRPC surface (`EntityPatchRequest`, `patchFormat` enum, the `CLIENT_ERROR` envelope reality and the #342 caveat); strict validate-only (PATCH can never introduce a new field); merge-then-processor ordering. Frame explicitly as "the spec Cloud implements." No issue IDs in the prose body (the #342 reference belongs only in an internal note, not user-facing text — refer to it as "a tracked future gRPC-error improvement").

- [ ] **Step 3: Commit**

```bash
git add docs/cloud-parity/
git commit -m "docs(cloud-parity): entity PATCH contract for Cloud twin-alignment (#341)"
```

---

### Task 8: Help topic (`crud.md`)

**Files:**
- Modify: `cmd/cyoda/help/content/crud.md`

**Interfaces:** none (documentation).

- [ ] **Step 1: Update the SYNOPSIS** — add to the endpoint list:

```
PATCH  /api/entity/{format}/{entityId}
PATCH  /api/entity/{format}/{entityId}/{transition}
```

- [ ] **Step 2: Add a "Partial update (PATCH)" section** after the PUT update sections, covering: RFC 7386 merge semantics (send only changed fields; null deletes a key; arrays replace wholesale); JSON only; `Content-Type: application/merge-patch+json` (implemented) vs `application/json-patch+json` (501); the three-state `If-Match` (transactionId / `*` / absent→428) with the educational framing; the error table (404/409/412/415/428/501); and the two surprise behaviours called out in the spec — **strict validation** (a PATCH cannot introduce a new field, even in extend mode) and **processors run after the merge** (a named transition's processor may overwrite patched fields). Match the file's existing topic prose style. No issue IDs.

- [ ] **Step 3: Verify the help topic renders** (front-matter intact, builds):

Run: `go build ./cmd/cyoda && ./cyoda help crud | head -40` (or the package's help test if present: `go test ./cmd/cyoda/help/...`)
Expected: topic includes the PATCH lines; no build/parse error.

- [ ] **Step 4: Commit**

```bash
git add cmd/cyoda/help/content/crud.md
git commit -m "docs(help): document entity PATCH in the crud topic (#341)"
```

---

### Task 9: Fidelity-direction reframe (own commit)

**Files:**
- Modify: `CLAUDE.md` (lines 3-4), `docs/cyoda/cloud-divergences.md` (intro), `docs/cyoda/README.md` (intro)

**Interfaces:** none (documentation). This is a **separate commit** by design (both reviews flagged it as a governance change riding on a feature).

- [ ] **Step 1: Reword the three "fidelity-to-Cloud" framings** to "cyoda-go defines the contract; Cloud aligns." Keep edits surgical:
  - `CLAUDE.md:3-4`: reframe "digital twin … fidelity with Cyoda Cloud" to express that cyoda-go now leads the API/integration contract and Cloud follows, while still describing the project as a multi-node Go implementation of the Cyoda platform.
  - `docs/cyoda/cloud-divergences.md:1-7`: reframe "cyoda-go is the digital twin of Cyoda Cloud" to note cyoda-go leads; this page still tracks deliberate divergences (declared-but-unimplemented), now framed as Cloud-alignment items. Cross-link `../cloud-parity/`.
  - `docs/cyoda/README.md:2-6`: clarify the directory is a snapshot of Cloud's prior spec used as reference; with the direction reversed, Cloud aligns to cyoda-go's authoritative `api/openapi.yaml`.
- [ ] **Step 2: Do NOT** blanket-reword the per-feature `"accepted for Cyoda Cloud parity"` notes (crud.md/workflows.md/errors) — those describe specific declared-but-unimplemented fields, a different sense of "parity". Leave them unless a specific note's who-implements-what is now wrong; reword only those case-by-case.
- [ ] **Step 3: Verify nothing else references the old framing** in a way that now contradicts:

Run: `grep -rn "digital twin\|fidelity with Cyoda" CLAUDE.md docs/ README.md`
Expected: only intended, reworded occurrences remain.

- [ ] **Step 4: Commit (separate from the feature)**

```bash
git add CLAUDE.md docs/cyoda/cloud-divergences.md docs/cyoda/README.md
git commit -m "docs: reframe contract direction — cyoda-go leads, Cloud aligns (#341)"
```

---

### Task 10: Full verification

- [ ] **Step 1: Vet**

Run: `go vet ./...`
Expected: no findings.

- [ ] **Step 2: Full test suite (unit + E2E, Docker required)**

Run: `go test ./... -v 2>&1 | tail -40`
Expected: all green.

- [ ] **Step 3: Race detector (CI-parity scope), once, end-of-deliverable**

Run: `make race`
Expected: green. (Do not run per-step.)

- [ ] **Step 4: Regenerated-artefact check** — confirm `api/generated.go` and `api/grpc/events/types.go` are committed and reflect the schema/spec edits (no uncommitted regen diff):

Run: `git status --short && git diff --stat`
Expected: clean working tree.

---

## Notes for the executor

- **COMPATIBILITY.md** does not need a feature entry (it tracks SPI pins / chart / release). The release entry (CHANGELOG.md + `docs/release-notes/v0.8.2.md`) is added at release time, not in this plan.
- **Tooling:** Task 5 needs `go-jsonschema` (`go install github.com/atombender/go-jsonschema@latest`). Task 4 uses `go tool oapi-codegen` (already a module tool).
- **Plugin submodules:** this change is in the root module only; no `plugins/*` edits. Still run `go vet ./...` at the root.
- **Atomicity/409:** the commit-time read-set-conflict → 409 path is covered by existing tx-manager tests across backends; Task 3's `TestPatchEntity_StaleTokenIs412` covers the conditional precondition deterministically. A deterministic concurrent-interleave 409 test is hard to write at the service boundary; if one is added, it must use two genuinely concurrent commits (not a sequential call) — otherwise omit it and rely on the tx-manager coverage rather than writing a no-op.
