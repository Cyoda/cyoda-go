# Edge Message OpenAPI Reconciliation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reconcile the four Edge message operations in `api/openapi.yaml` with the server's real behavior, and fix one Gate-6 error-handling inconsistency, so the published contract is provably accurate.

**Architecture:** Almost entirely `api/openapi.yaml` edits + regenerated `api/generated.go`, guarded by e2e characterization tests (the enforce-mode conformance validator checks *responses only*), the oasdiff breaking-change gate (two deliberate `err-ignore` entries), and the `codegen-sync` gate. Exactly one runtime change: `deleteMessages` returns `413` (not `500`) on oversized bodies, mirroring `newMessage`.

**Tech Stack:** Go 1.26, `oapi-codegen v2.7.0` (embedded spec via `//go:embed`), `oasdiff v1.21.0`, testcontainers-go (Postgres e2e), `kin-openapi` conformance validator.

**Spec:** `docs/superpowers/specs/2026-07-06-openapi-message-design.md`

## Global Constraints

- **Worktree paths only.** Work in `/Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-openapi-message-slice`. The session shell resets cwd here, but absolute main-repo paths (`/Users/paul/go-projects/cyoda-light/cyoda-go/...` without `.claude/worktrees/...`) write to the MAIN repo — always target the worktree.
- **Regen after every `openapi.yaml` edit:** `go generate ./api` (directive: `api/generate.go` → `oapi-codegen --config=config.yaml openapi.yaml`). Commit `api/generated.go` in the same commit. `make check-codegen` must pass.
- **Never hand-edit `api/generated.go`** (it is `DO NOT EDIT`). Never add `additionalProperties: false` (typed-but-open policy).
- **oasdiff gate (run locally to verify):**
  ```bash
  go install github.com/oasdiff/oasdiff@v1.21.0   # once
  git show origin/release/v0.8.2:api/openapi.yaml > /tmp/base-openapi.yaml
  oasdiff breaking /tmp/base-openapi.yaml api/openapi.yaml --fail-on ERR --err-ignore .github/oasdiff-err-ignore.txt; echo "exit=$?"
  ```
  Capture the exit code via `echo`/redirect, never pipe-to-tail. `--fail-on ERR` must exit 0.
- **No new error codes:** ops emit only `BAD_REQUEST` (`common.ErrCodeBadRequest`) and `ENTITY_NOT_FOUND` (`common.ErrCodeEntityNotFound`); both already have `cmd/cyoda/help/content/errors/*.md` topics. Do not add error-code topics.
- **e2e run:** `go test ./internal/e2e/... -run <Name> -v` (needs Docker/Postgres). Full conformance: `go test ./internal/e2e/... -run TestOpenAPIConformanceReport -v`.
- **Do NOT edit `EntityTransactionResponse`** (shared by 8 ops; `newMessage` reuses it).
- Commit messages end with: `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

## File Structure

| File | Responsibility | Tasks |
|---|---|---|
| `internal/domain/messaging/handler.go` | `DeleteMessages` 413-on-oversize fix | 1 |
| `api/openapi.yaml` | all message op/schema/example reconciliations | 1–6 |
| `api/generated.go` | regenerated after each YAML edit | 2,3,5 |
| `.github/oasdiff-err-ignore.txt` | 2 deliberate breaking-change allowances | 2,3 |
| `internal/e2e/message_test.go` | characterization + F5-proof + 413 e2e tests | 1–5 |
| `e2e/parity/message.go` (new) + `e2e/parity/registry.go` | cross-backend round-trip parity | 7 |
| `docs/cloud-parity/openapi-conformance.md` | record message reconciliation decisions | 8 |

---

### Task 1: `deleteMessages` returns 413 on oversized body (Gate-6 fix + spec)

**Files:**
- Modify: `internal/domain/messaging/handler.go:227-233` (DeleteMessages body-read)
- Modify: `api/openapi.yaml` (deleteMessages: add `413` response)
- Test: `internal/e2e/message_test.go`

**Interfaces:**
- Consumes: `common.Operational`, `common.ErrCodeBadRequest`, `http.StatusRequestEntityTooLarge` (already imported/used in `NewMessage`).
- Produces: `deleteMessages` now emits `413` on >10 MB body.

- [ ] **Step 1: Write the failing test** — append to `internal/e2e/message_test.go`:

```go
// TestMessage_DeleteMessages_413 asserts an oversized batch-delete body is
// rejected with 413 (parity with newMessage), not 500.
func TestMessage_DeleteMessages_413(t *testing.T) {
	// A JSON array string just over the 10MB MaxBytesReader limit.
	big := make([]byte, 10*1024*1024+1)
	for i := range big {
		big[i] = 'a'
	}
	body := `["` + string(big) + `"]`
	resp := doAuth(t, http.MethodDelete, "/api/message", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("deleteMessages oversized body: status=%d, want 413", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/e2e/... -run TestMessage_DeleteMessages_413 -v`
Expected: FAIL — `status=500, want 413`.

- [ ] **Step 3: Implement the fix** — in `internal/domain/messaging/handler.go`, change the `DeleteMessages` body-read block (currently maps every read error to `Internal`) to mirror `NewMessage:36-43`:

```go
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		if err.Error() == "http: request body too large" {
			common.WriteError(w, r, common.Operational(http.StatusRequestEntityTooLarge, common.ErrCodeBadRequest, "request payload exceeds maximum allowed limit of 10MB"))
			return
		}
		common.WriteError(w, r, common.Internal("failed to read request body", err))
		return
	}
```

- [ ] **Step 4: Add the `413` response to the spec** — in `api/openapi.yaml`, under `deleteMessages` (`operationId: deleteMessages`), add a `"413"` response before `"401"` (copy the newMessage 413 block, corrected instance):

```yaml
        "413":
          description: Payload too large
          content:
            application/problem+json:
              schema:
                $ref: "#/components/schemas/ProblemDetail"
              examples:
                Payload Too Large:
                  description: Payload Too Large
                  value:
                    type: about:blank
                    title: Request Entity Too Large
                    status: 413
                    detail: request payload exceeds maximum allowed limit of 10MB
                    instance: /api/message
                    properties:
                      errorCode: BAD_REQUEST
```

- [ ] **Step 5: Regenerate + verify green**

Run:
```bash
go generate ./api && make check-codegen
go test ./internal/e2e/... -run TestMessage_DeleteMessages_413 -v
go vet ./...
```
Expected: check-codegen clean; test PASS; vet clean. (413 is an additive response — oasdiff-neutral.)

- [ ] **Step 6: Commit**

```bash
git add internal/domain/messaging/handler.go api/openapi.yaml api/generated.go internal/e2e/message_test.go
git commit -m "fix(messaging): deleteMessages returns 413 on oversized body (parity with newMessage)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: `newMessage` request body → object envelope (F2/F3)

**Files:**
- Modify: `api/openapi.yaml` (newMessage requestBody + description; add `NewMessageRequest` schema)
- Modify: `api/generated.go` (regen)
- Modify: `.github/oasdiff-err-ignore.txt` (entry #1)
- Test: `internal/e2e/message_test.go`

**Interfaces:**
- Consumes: existing `EdgeMessagePayload` schema (unrestricted — any JSON value).
- Produces: `NewMessageRequest` schema; `newMessage` requestBody references it.

- [ ] **Step 1: Write characterization tests** — append to `internal/e2e/message_test.go`:

```go
// TestMessage_NewMessage_ObjectEnvelope characterizes the real body contract:
// an object {payload, meta-data}; missing payload -> 400; a top-level array -> 400.
func TestMessage_NewMessage_ObjectEnvelope(t *testing.T) {
	ok := doAuth(t, http.MethodPost, "/api/message/new/env-ok", `{"payload":{"a":1},"meta-data":{"k":"v"}}`)
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("object envelope: status=%d, want 200", ok.StatusCode)
	}

	// payload may be any JSON value, incl. a base64-ish string (binary workaround).
	strp := doAuth(t, http.MethodPost, "/api/message/new/env-str", `{"payload":"SGVsbG8="}`)
	defer strp.Body.Close()
	if strp.StatusCode != http.StatusOK {
		t.Fatalf("string payload: status=%d, want 200", strp.StatusCode)
	}

	missing := doAuth(t, http.MethodPost, "/api/message/new/env-missing", `{"meta-data":{}}`)
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing payload: status=%d, want 400", missing.StatusCode)
	}

	arr := doAuth(t, http.MethodPost, "/api/message/new/env-arr", `[{"payload":{"a":1}}]`)
	defer arr.Body.Close()
	if arr.StatusCode != http.StatusBadRequest {
		t.Fatalf("array body: status=%d, want 400 (single object only)", arr.StatusCode)
	}
}
```

- [ ] **Step 2: Run to verify PASS (characterization — behavior already exists)**

Run: `go test ./internal/e2e/... -run TestMessage_NewMessage_ObjectEnvelope -v`
Expected: PASS. (Locks the real contract before the doc is corrected.)

- [ ] **Step 3: Add the `NewMessageRequest` schema** — in `api/openapi.yaml` `components.schemas`, next to the other message schemas (near `EdgeMessageDto`):

```yaml
    NewMessageRequest:
      type: object
      description: >
        A single JSON object carrying the message payload and optional flat metadata.
      properties:
        payload:
          $ref: "#/components/schemas/EdgeMessagePayload"
          description: >
            The message payload — any JSON value (object, array, string, number, …).
            Non-JSON content (e.g. binary) must be stringified (e.g. base64) and sent
            as a JSON string; see the EdgeMessagePayload note.
        meta-data:
          type: object
          additionalProperties: true
          description: Optional flat key→value map; indexed to enable search by metadata.
      required:
        - payload
```

- [ ] **Step 4: Point the requestBody at it + fix the description** — replace the `newMessage` `requestBody` `description` (the "single JSON object or an array of homomorphic JSON objects" prose) and its `schema: {type: string}`:

```yaml
      requestBody:
        description: >
          A single JSON object. It must contain a `payload` field (any valid JSON
          value) and may include an optional flat `meta-data` map of key-value pairs.
          The meta-data is indexed to enable fast search by meta-data.
        content:
          application/json:
            schema:
              $ref: "#/components/schemas/NewMessageRequest"
            examples:
              # keep the two existing examples (they are already {payload, meta-data} objects)
        required: true
```

Preserve the two existing `examples` blocks verbatim (they are already valid objects).

- [ ] **Step 5: Regenerate + add the oasdiff err-ignore entry**

Run `go generate ./api`, then run the oasdiff command (Global Constraints) and copy the EXACT single-line message it prints for the newMessage request-body change into `.github/oasdiff-err-ignore.txt`, under a new comment block modelled on the createCollection precedent (line ~11):

```
# message slice — newMessage (POST /message/new/{subject}) request body corrected
# from string to the real {payload, meta-data} object envelope; the prior string
# schema was prototype-era drift the server never honoured (a bare string is
# rejected — the handler unmarshals an object). ADR 0003 Decision 7.
<PASTE EXACT oasdiff line, e.g.:>
error at api/openapi.yaml, in API POST /message/new/{subject} the request's body `type/format` changed from `string/` to `object/json` [request-body-type-changed].
```

- [ ] **Step 6: Verify green**

Run:
```bash
make check-codegen
oasdiff breaking /tmp/base-openapi.yaml api/openapi.yaml --fail-on ERR --err-ignore .github/oasdiff-err-ignore.txt; echo "exit=$?"
go test ./internal/e2e/... -run 'TestMessage_NewMessage_ObjectEnvelope|TestOpenAPIConformanceReport' -v
go vet ./...
```
Expected: check-codegen clean; oasdiff `exit=0`; tests PASS; vet clean.

- [ ] **Step 7: Commit**

```bash
git add api/openapi.yaml api/generated.go .github/oasdiff-err-ignore.txt internal/e2e/message_test.go
git commit -m "docs(openapi): newMessage request body string -> {payload, meta-data} object (F2/F3)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: `deleteMessages` request body → array of uuid strings (F1)

**Files:**
- Modify: `api/openapi.yaml` (deleteMessages requestBody + drop v1 prose)
- Modify: `api/generated.go` (regen)
- Modify: `.github/oasdiff-err-ignore.txt` (entry #2)
- Test: `internal/e2e/message_test.go`

- [ ] **Step 1: Write characterization test** — append:

```go
// TestMessage_DeleteMessages_ArrayBody characterizes the real body: a JSON array
// of uuid strings (200); a non-array JSON (e.g. object) -> 400.
func TestMessage_DeleteMessages_ArrayBody(t *testing.T) {
	id := createMessageE2E(t, "del-arr", `{"x":1}`)
	ok := doAuth(t, http.MethodDelete, "/api/message", fmt.Sprintf(`["%s"]`, id))
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("array body: status=%d, want 200", ok.StatusCode)
	}
	bad := doAuth(t, http.MethodDelete, "/api/message", `{"not":"an array"}`)
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("object body: status=%d, want 400", bad.StatusCode)
	}
}
```

- [ ] **Step 2: Run to verify PASS** — `go test ./internal/e2e/... -run TestMessage_DeleteMessages_ArrayBody -v` → PASS.

- [ ] **Step 3: Correct the spec** — replace the `deleteMessages` `requestBody` block:

```yaml
      requestBody:
        description: List of message IDs to delete.
        content:
          application/json:
            schema:
              type: array
              items:
                type: string
                format: uuid
            examples:
              Message IDs:
                description: Message IDs
                value:
                  - 8824c480-c166-11ee-9cc7-ae468cd3ed16
                  - 31134900-d9cb-11ee-9cc7-ae468cd3ed16
        required: true
```

(Drops the fictional "All IDs must be version 1 (time-based) UUIDs" prose; the array examples are unchanged.)

- [ ] **Step 4: Regenerate + add oasdiff err-ignore entry #2**

`go generate ./api`, run oasdiff, paste the exact line under a new comment block:

```
# message slice — deleteMessages (DELETE /message) request body corrected from a
# single string/uuid to the real array<string> of ids; prior schema was prototype
# drift (the handler unmarshals []string). ADR 0003 Decision 7.
<PASTE EXACT oasdiff line, e.g.:>
error at api/openapi.yaml, in API DELETE /message the request's body `type/format` changed from `string/uuid` to `array<string>` [request-body-type-changed].
```

- [ ] **Step 5: Verify green** — `make check-codegen`; oasdiff `exit=0`; `TestMessage_DeleteMessages_ArrayBody` PASS; `go vet ./...`.

- [ ] **Step 6: Commit**

```bash
git add api/openapi.yaml api/generated.go .github/oasdiff-err-ignore.txt internal/e2e/message_test.go
git commit -m "docs(openapi): deleteMessages request body string -> array<uuid> (F1)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Remove fictional v1-UUID `400` + retype path params (F5)

**Files:**
- Modify: `api/openapi.yaml` (getMessage, deleteMessage: remove `400`, `uuid-v1`→`uuid`, drop v1 prose; deleteMessages: re-describe the real `400`)
- Test: `internal/e2e/message_test.go`

**Interfaces:**
- Produces: getMessage/deleteMessage no longer document `400`; both path params typed `format: uuid`.

- [ ] **Step 1: Write the F5-proof tests** — a valid **v4** UUID (route-matches `format: uuid`, but is NOT v1) must resolve to 404, proving no v1 validation exists:

```go
// TestMessage_NoV1Validation proves the fictional "not a time-based UUID (v1)" 400
// does not exist: a valid v4 uuid (non-v1) that is absent -> 404, never 400.
func TestMessage_NoV1Validation(t *testing.T) {
	v4 := "123e4567-e89b-42d3-a456-426614174000" // version nibble = 4
	get := doAuth(t, http.MethodGet, "/api/message/"+v4, "")
	defer get.Body.Close()
	if get.StatusCode != http.StatusNotFound {
		t.Fatalf("getMessage v4 uuid: status=%d, want 404 (no v1 check)", get.StatusCode)
	}
	del := doAuth(t, http.MethodDelete, "/api/message/"+v4, "")
	defer del.Body.Close()
	if del.StatusCode != http.StatusNotFound {
		t.Fatalf("deleteMessage v4 uuid: status=%d, want 404 (no v1 check)", del.StatusCode)
	}
}
```

- [ ] **Step 2: Run to verify PASS** — `go test ./internal/e2e/... -run TestMessage_NoV1Validation -v` → PASS.

- [ ] **Step 3: Edit the spec.**
  - `getMessage` and `deleteMessage` path param `messageId`: change `format: uuid-v1` → `format: uuid` and delete the `description: Must be a version 1 (time-based) UUID` line.
  - Delete the entire `"400"` response block from BOTH `getMessage` and `deleteMessage` (the "Invalid UUID format or not a time-based UUID" blocks).
  - `deleteMessages` `"400"`: keep the response, but replace its `description` and example with the real invalid-JSON error and correct instance:

```yaml
        "400":
          description: Invalid request body (not a JSON array of UUID strings)
          content:
            application/problem+json:
              schema:
                $ref: "#/components/schemas/ProblemDetail"
              examples:
                Invalid Body:
                  description: Invalid Body
                  value:
                    type: about:blank
                    title: Bad Request
                    status: 400
                    detail: "invalid JSON: expected array of UUID strings: json: cannot unmarshal object into Go value of type []string"
                    instance: /api/message
                    properties:
                      errorCode: BAD_REQUEST
```

- [ ] **Step 4: Verify oasdiff classification** — run the oasdiff command. Removing a non-success `400` is expected to be WARN (`response-non-success-status-removed`), so `--fail-on ERR` stays `exit=0` with NO new err-ignore. If oasdiff instead prints an ERR-level line for either 400 removal, add a surgical err-ignore entry (comment: "getMessage/deleteMessage 400 removed — server performs no v1/uuid validation; the 400 was fictional prototype drift"). Record which happened.

- [ ] **Step 5: Verify green**

```bash
oasdiff breaking /tmp/base-openapi.yaml api/openapi.yaml --fail-on ERR --err-ignore .github/oasdiff-err-ignore.txt; echo "exit=$?"
go test ./internal/e2e/... -run 'TestMessage_NoV1Validation|TestOpenAPIConformanceReport' -v
```
Expected: `exit=0`; tests PASS.

- [ ] **Step 6: Commit**

```bash
git add api/openapi.yaml internal/e2e/message_test.go .github/oasdiff-err-ignore.txt
git commit -m "docs(openapi): remove fictional v1-UUID 400 on message reads; uuid-v1 -> uuid (F5)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: `getMessage.metaData` flat-map retype + dead-schema deletion (F6)

**Files:**
- Modify: `api/openapi.yaml` (`EdgeMessageMetaData` retype; delete `ValueMaps`, `LocalTime`)
- Modify: `api/generated.go` (regen)
- Test: `internal/e2e/message_test.go`

- [ ] **Step 1: Write the characterization test** — assert the flat map + injected `typeReferences`:

```go
// TestMessage_MetaDataFlatShape characterizes getMessage metaData: values and
// indexedValues are flat maps, each always containing an injected typeReferences key.
func TestMessage_MetaDataFlatShape(t *testing.T) {
	id := createMessageE2E(t, "meta-shape", `{"x":1}`) // createMessageE2E sends meta-data {source:e2e}
	resp := doAuth(t, http.MethodGet, "/api/message/"+id, "")
	defer resp.Body.Close()
	var body struct {
		MetaData struct {
			Values        map[string]any `json:"values"`
			IndexedValues map[string]any `json:"indexedValues"`
		} `json:"metaData"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body.MetaData.Values["typeReferences"]; !ok {
		t.Errorf("values missing injected typeReferences key: %v", body.MetaData.Values)
	}
	if _, ok := body.MetaData.IndexedValues["typeReferences"]; !ok {
		t.Errorf("indexedValues missing injected typeReferences key: %v", body.MetaData.IndexedValues)
	}
	if _, ok := body.MetaData.IndexedValues["source"]; !ok {
		t.Errorf("indexedValues missing flat user key 'source': %v", body.MetaData.IndexedValues)
	}
}
```

- [ ] **Step 2: Run to verify PASS** — `go test ./internal/e2e/... -run TestMessage_MetaDataFlatShape -v` → PASS.

- [ ] **Step 3: Retype `EdgeMessageMetaData`** — replace its `values`/`indexedValues` (currently `$ref: ValueMaps`):

```yaml
    EdgeMessageMetaData:
      type: object
      properties:
        values:
          type: object
          additionalProperties: true
          description: >
            Flat map of metadata values. Always contains an injected `typeReferences`
            key (an empty object in this version).
        indexedValues:
          type: object
          additionalProperties: true
          description: >
            Flat map of indexed (searchable) metadata values. Always contains an
            injected `typeReferences` key (an empty object in this version).
      required:
        - indexedValues
        - values
```

- [ ] **Step 4: Delete the dead schemas** — remove the entire `ValueMaps:` schema block and the entire `LocalTime:` schema block from `components.schemas`. (Verify no other `$ref` remains: `grep -n 'ValueMaps\|LocalTime' api/openapi.yaml` should return nothing after deletion.)

- [ ] **Step 5: Regenerate + verify compile-safe**

```bash
go generate ./api && make check-codegen
grep -n 'ValueMaps\|LocalTime' api/openapi.yaml   # expect: no matches
go build ./... && go vet ./...
```
Expected: check-codegen clean; grep empty; build+vet clean (generated `ValueMaps`/`LocalTime`/satellite types removed, no hand-written references).

- [ ] **Step 6: Verify response conformance stays green**

Run: `go test ./internal/e2e/... -run 'TestMessage_MetaDataFlatShape|TestOpenAPIConformanceReport' -v`
Expected: PASS (the real flat response validates against the new open schema).
Run oasdiff — the retype removes only optional nested props + adds `additionalProperties`; expected non-breaking (`exit=0`, no new err-ignore). If ERR, add a surgical entry with rationale.

- [ ] **Step 7: Commit**

```bash
git add api/openapi.yaml api/generated.go internal/e2e/message_test.go
git commit -m "docs(openapi): getMessage metaData ValueMaps -> typed-but-open flat map; delete dead schemas (F6)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: Example fidelity — `typeReferences`, `errorCode`, drop fictional props (F7/F8)

**Files:**
- Modify: `api/openapi.yaml` (examples only)

No code change. The conformance validator does not validate examples, so this task is verified by re-reading against server truth + keeping oasdiff/codegen green.

- [ ] **Step 1: Fix the getMessage 200 example (F7)** — in the `getMessage` `"200"` example, change:

```yaml
                    metaData:
                      values:
                        typeReferences: {}
                      indexedValues:
                        typeReferences: {}
```

- [ ] **Step 2: Fix error examples (F8)** — for every message-op ProblemDetail example, ensure `properties` contains `errorCode` and drop fictional props:
  - `newMessage` `400` example: replace `properties: {error: ...}` with `properties: {errorCode: BAD_REQUEST}`.
  - `newMessage` `413` example (from Task 1): already `properties: {errorCode: BAD_REQUEST}`, title `Request Entity Too Large` — confirm the *pre-existing* 413 example on newMessage also has the correct title/detail; fix if it still reads `title: Payload Too Large` / old detail:
    ```yaml
                    title: Request Entity Too Large
                    detail: request payload exceeds maximum allowed limit of 10MB
                    properties:
                      errorCode: BAD_REQUEST
    ```
  - `getMessage`/`deleteMessage` `404` examples: keep `messageId`, add `errorCode`:
    ```yaml
                    properties:
                      messageId: 8824c480-c166-11ee-bf9f-ae468cd3ed16
                      errorCode: ENTITY_NOT_FOUND
    ```
  - `deleteMessages` `400` example (from Task 4): already `properties: {errorCode: BAD_REQUEST}` — confirm no `uuid` prop remains.

- [ ] **Step 3: Verify green**

```bash
make check-codegen   # examples don't affect codegen, but confirm no accidental structural edit
oasdiff breaking /tmp/base-openapi.yaml api/openapi.yaml --fail-on ERR --err-ignore .github/oasdiff-err-ignore.txt; echo "exit=$?"
go test ./internal/e2e/... -run TestOpenAPIConformanceReport -v
```
Expected: check-codegen clean; oasdiff `exit=0`; conformance PASS.

- [ ] **Step 4: Commit**

```bash
git add api/openapi.yaml
git commit -m "docs(openapi): message example fidelity - typeReferences, errorCode, drop fictional props (F7/F8)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: Cross-backend parity round-trip scenario

**Files:**
- Create: `e2e/parity/message.go`
- Modify: `e2e/parity/registry.go` (register the scenario)
- Reuse: `e2e/parity/client` methods `CreateMessage`, `GetMessage`, `DeleteMessage` (already exist)

**Interfaces:**
- Consumes: `BackendFixture` (`NewTenant`, `BaseURL`), `client.NewClient`, `client.CreateMessage/GetMessage/DeleteMessage`.
- Produces: `RunMessageRoundTrip(t, fixture)` registered as a `NamedTest`.

- [ ] **Step 1: Write the parity scenario** — `e2e/parity/message.go`:

```go
package parity

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunMessageRoundTrip exercises the edge-message new->get->delete cycle and
// asserts the backend-agnostic response shape: metaData.values/indexedValues are
// flat maps each carrying an injected typeReferences key. Guards against a
// storage backend diverging on the message contract.
func RunMessageRoundTrip(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	id, err := c.CreateMessage(t, "parity-roundtrip", `{"hello":"world"}`)
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	msg, err := c.GetMessage(t, id)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	meta, ok := msg["metaData"].(map[string]any)
	if !ok {
		t.Fatalf("metaData missing/!object: %v", msg["metaData"])
	}
	for _, k := range []string{"values", "indexedValues"} {
		m, ok := meta[k].(map[string]any)
		if !ok {
			t.Fatalf("metaData.%s missing/!object: %v", k, meta[k])
		}
		if _, ok := m["typeReferences"]; !ok {
			t.Errorf("metaData.%s missing injected typeReferences: %v", k, m)
		}
	}

	if err := c.DeleteMessage(t, id); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
}
```

- [ ] **Step 2: Register it** — in `e2e/parity/registry.go`, add to the canonical `allTests` list (follow the existing `NamedTest{Name: ..., Run: ...}` entries):

```go
	{Name: "MessageRoundTrip", Run: RunMessageRoundTrip},
```

- [ ] **Step 3: Run parity on the in-process backend**

Run: `go test ./internal/e2e/... -run 'Parity.*MessageRoundTrip' -v` (or the parity suite entry point that iterates backends; confirm the scenario name appears and passes on memory + postgres).
Expected: PASS on each registered backend.

- [ ] **Step 4: Commit**

```bash
git add e2e/parity/message.go e2e/parity/registry.go
git commit -m "test(parity): edge-message new->get->delete round-trip across backends

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 8: Cloud-parity doc + full verification

**Files:**
- Modify: `docs/cloud-parity/openapi-conformance.md`

- [ ] **Step 1: Record the reconciliation decisions** — append a "Message ops" section to `docs/cloud-parity/openapi-conformance.md` capturing: request-body shapes corrected (newMessage object envelope, deleteMessages array); fictional v1-UUID `400` removed (no v1 validation; `uuid-v1`→`uuid`); metaData retyped to typed-but-open flat map (injected `typeReferences`); the `deleteMessages` `413`-parity runtime fix; and the deferrals — `transactionTimeoutMillis`/`transactionSize` honoring → **#379**, native non-JSON content types → **#193**.

- [ ] **Step 2: Full verification suite**

```bash
go test ./internal/e2e/... -v            # incl. TestOpenAPIConformanceReport (0 mismatches)
make check-codegen
make check-gofmt
oasdiff breaking /tmp/base-openapi.yaml api/openapi.yaml --fail-on ERR --err-ignore .github/oasdiff-err-ignore.txt; echo "exit=$?"
go vet ./...
make race                                # race-sensitive scope incl. e2e/parity (MessageRoundTrip)
```
Expected: all green; conformance 0 mismatches; oasdiff `exit=0`; race clean.

- [ ] **Step 3: Plugin submodule check** — the message reconciliation touches no plugin code, but confirm no drift:

```bash
make test-short-all
```
Expected: green.

- [ ] **Step 4: Commit**

```bash
git add docs/cloud-parity/openapi-conformance.md
git commit -m "docs(cloud-parity): record edge-message reconciliation decisions + #379/#193 deferrals

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage:** F1 (Task 3), F2 (Task 2), F3 (Task 2), F5 (Task 4), F6 (Task 5), F7 (Task 6), F8 (Task 6), §3 413-parity (Task 1), oasdiff entries (Tasks 2/3, verify 4/5/6), codegen regen (Tasks 1/2/3/5), dead-schema deletion (Task 5), parity (Task 7), cloud-parity doc + full verify (Task 8). Per-endpoint error table & coverage matrix rows all map to tests in Tasks 1–7. No gap.

**Placeholder scan:** The two `<PASTE EXACT oasdiff line>` markers are deliberate — the exact singleline text is emitted by the pinned oasdiff at run time (capturing invented text would be wrong/fragile); an illustrative example line is given for each. No other placeholders.

**Type consistency:** `NewMessageRequest` (Task 2) referenced consistently; `EdgeMessagePayload`/`ProblemDetail`/`EntityTransactionResponse` reused unchanged; `RunMessageRoundTrip` name consistent between `message.go` and `registry.go`; test helper names (`doAuth`, `createMessageE2E`) match the existing `message_test.go`.
