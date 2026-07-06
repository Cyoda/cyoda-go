# OpenAPI Dead-Surface Disposition — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Dispose the OpenAPI dead surface (final #369 slice): fix the SQL-Schema authored-contract drift, remove the vestigial CQL exclude-tags entry, lock the unrouted contract with a 404 test, and record the disposition — leaving Stream Data minimally touched.

**Architecture:** Almost all edits are to `api/openapi.yaml` (an authored, `//go:embed`'d, **not-generated** contract) and `api/config.yaml`. The touched SQL-Schema/Stream-Data ops are **excluded from codegen and unrouted** — there is no server behavior to drive, so the "tests" for the contract edits are: (1) the spec loads in kin-openapi (`go test ./api`), (2) the oasdiff additive-only gate stays green via surgical fail-closed err-ignore entries, and (3) the e2e conformance-marker gate stays green. One new e2e characterization test asserts the unrouted paths 404.

**Tech Stack:** Go 1.26+, `oapi-codegen` (`api/generated.go`), kin-openapi v0.140.0, oasdiff v1.21.0, testcontainers-go (e2e Postgres), `internal/e2e` conformance harness.

## Global Constraints

- **Typed-but-open, never `additionalProperties: false`** (ADR 0003; [[project_openapi_typed_but_open_policy]]). Enumerate props, keep objects open.
- **Additive-only oasdiff gate.** Breaking-but-clientless corrections are allowed ONLY via surgical `.github/oasdiff-err-ignore.txt` entries whose text matches op+path+message exactly (fail-closed). **Capture the EXACT pinned-oasdiff (v1.21.0) singleline text at implementation — never pre-write it.** Verify a change is actually `ERR` (not `WARN`/additive) before adding an entry.
- **No issue IDs in shipped artefacts.** #381/#382/#369 appear only in commits/PR body/this plan — never in `openapi.yaml`, `config.yaml`, or the cloud-parity doc content.
- **ProblemDetail is a correct typed-but-open error bag** — reuse `#/components/schemas/ProblemDetail`; do not add a new error schema or error code.
- **Worktree paths for all edits/commits.** Working dir is `.claude/worktrees/feat-openapi-dead-surface`; absolute main-repo paths (`/Users/paul/go-projects/cyoda-light/cyoda-go/...` without `.claude/worktrees/...`) hit the MAIN repo, NOT this worktree.
- **Docker split:** subagents run compile/`go vet`/`gofmt`/`go test ./api`/oasdiff/`make check-codegen` (no Docker). The **controller** runs the Docker-backed `internal/e2e` suite (conformance gate + the new 404 test) and `make race` at consolidation points.
- Stream Data ops (13, `x-cyoda-status: unimplemented`) are **not** edited by any task.

---

### Task 1: Lock the unrouted contract with a 404 characterization test

Establishes the safety net first: a representative op from each excluded tag must 404. This passes immediately (ops are unrouted) and must stay green through every later edit — proving no edit accidentally routes anything.

**Files:**
- Create: `internal/e2e/dead_surface_test.go`

**Interfaces:**
- Consumes: the e2e harness (`TestMain` starts Postgres + an in-process `httptest.Server` with JWT auth). Use the same authenticated-request helper the sibling tests use (inspect a neighbor, e.g. `internal/e2e/message_test.go` or `entity_test.go`, for the request/client/token helper names — do NOT invent them).
- Produces: nothing consumed downstream.

- [ ] **Step 1: Write the characterization test**

Assert one representative path per excluded tag returns 404. Use the existing authenticated-GET helper (name discovered from a neighbor test):

```go
package e2e

import (
	"net/http"
	"testing"
)

// Dead surface: SQL-Schema (planned) and Stream Data (unimplemented) ops are
// published in the contract but excluded from codegen and unrouted. They MUST
// 404. This locks that contract: if a future change routes one without removing
// its x-cyoda-status marker, this test (with the conformance stale-marker gate)
// catches it.
func TestDeadSurfaceUnrouted(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"sql-schema listAll", http.MethodGet, "/api/sql/schema/listAll"},
		{"stream-data config list", http.MethodGet, "/api/platform-api/stream-data/config/list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doAuthedRequest(t, tc.method, tc.path, nil) // ← use the real helper name
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s: got %d, want 404 (op must be unrouted)", tc.method, tc.path, resp.StatusCode)
			}
		})
	}
}
```

Confirm the exact base path prefix (`/api`) and helper signature from a neighbor test before finalizing — adjust the request construction to match the harness.

- [ ] **Step 2: Compile-check (subagent)**

Run: `go vet ./internal/e2e/`
Expected: no errors (helper name resolves).

- [ ] **Step 3: Run under Docker (controller consolidation)**

Run: `go test ./internal/e2e/ -run TestDeadSurfaceUnrouted -v`
Expected: PASS (both paths 404). If a path returns 401/405 instead, the request/auth helper is wrong — fix the construction, not the assertion.

- [ ] **Step 4: Commit**

```bash
git add internal/e2e/dead_surface_test.go
git commit -m "test(openapi): lock dead-surface ops as unrouted (404)"
```

---

### Task 2: D1 — correct malformed `array` + sibling `$ref` response schemas

`genTables` and `getSchemas` document `200` as `type: array` with a **sibling `$ref`** and no `items` — kin-openapi decodes this as an untyped array. Give them proper `items`.

**Files:**
- Modify: `api/openapi.yaml` (`genTables` 200 ≈ line 7510-7516 → `TableConfigDto`; `getSchemas` 200 ≈ line 7647-7653 → `SchemaConfigDto`)
- Modify: `.github/oasdiff-err-ignore.txt` (append only if the change is `ERR`)

**Interfaces:**
- Consumes: existing `#/components/schemas/TableConfigDto`, `#/components/schemas/SchemaConfigDto`.
- Produces: well-formed array responses; no downstream code consumes these (unrouted).

- [ ] **Step 1: Fix `genTables` 200 schema**

Before:
```yaml
              schema:
                type: array
                $ref: "#/components/schemas/TableConfigDto"
```
After:
```yaml
              schema:
                type: array
                items:
                  $ref: "#/components/schemas/TableConfigDto"
```

- [ ] **Step 2: Fix `getSchemas` 200 schema** (same transform, `$ref` → `SchemaConfigDto`)

Before:
```yaml
              schema:
                type: array
                $ref: "#/components/schemas/SchemaConfigDto"
```
After:
```yaml
              schema:
                type: array
                items:
                  $ref: "#/components/schemas/SchemaConfigDto"
```

- [ ] **Step 3: Verify the spec still loads (subagent)**

Run: `go test ./api -v`
Expected: PASS (`GetSwagger()`/`TestEnvelopeMetaTypedButOpen` load the corrected spec).

- [ ] **Step 4: Run oasdiff and capture exact ERR text (subagent)**

```bash
go install github.com/oasdiff/oasdiff@v1.21.0
git show origin/release/v0.8.2:api/openapi.yaml > /tmp/base.yaml
oasdiff breaking /tmp/base.yaml api/openapi.yaml --fail-on ERR --err-ignore .github/oasdiff-err-ignore.txt > /tmp/oasdiff.out 2>&1; echo "exit=$?"
cat /tmp/oasdiff.out
```
If exit≠0, the printed `error at api/openapi.yaml, in API GET /sql/schema/... [<category>]` lines are the changes to allow. If exit=0, the array-items change was non-breaking (skip Step 5).

- [ ] **Step 5: Append surgical err-ignore entries (only for real ERR lines)**

Add under a new comment block, pasting the EXACT lines from Step 4 verbatim:
```
# SQL-Schema slice (group 5, ADR 0003 Decision 7) — genTables/getSchemas 200 array
# schema corrected from malformed `type:array`+sibling-$ref to array<items>. The ops
# are unrouted (404); no working client depends on the malformed shape.
<paste exact oasdiff ERR line(s) here>
```
Re-run the Step 4 command; expected `exit=0`.

- [ ] **Step 6: gofmt + commit**

```bash
gofmt -l api/ ; git add api/openapi.yaml .github/oasdiff-err-ignore.txt
git commit -m "fix(openapi): SQL-Schema genTables/getSchemas 200 array items (D1)"
```

---

### Task 3: D2 — correct `updateTables` request body (string → array) + its 200 response array-items

`updateTables` declares its request body as `type: string`, but its own example is an array of `TableConfigDto`. Correct the body to the real shape. **Also (found in Task 2 review):** `updateTables`' own `200` response carries the same malformed `type: array` + sibling `$ref` (no `items`) as the D1 ops — fix it here since we're already in this op (the D1 audit list of genTables/getSchemas was incomplete; updateTables 200 is the third instance).

**Files:**
- Modify: `api/openapi.yaml` (`updateTables` requestBody ≈ line 7953-7956; `updateTables` 200 response schema ≈ line 8088-8090)
- Modify: `.github/oasdiff-err-ignore.txt`

**Interfaces:**
- Consumes: `#/components/schemas/TableConfigDto`.
- Produces: array request body + well-formed array response; no codegen impact (unrouted).

- [ ] **Step 1a: Fix the `updateTables` 200 response array-items (same transform as D1)**

Before:
```yaml
              schema:
                type: array
                $ref: "#/components/schemas/TableConfigDto"
```
After:
```yaml
              schema:
                type: array
                items:
                  $ref: "#/components/schemas/TableConfigDto"
```
(This will add `response-required-property-removed` err-ignore lines for `POST /sql/schema/updateTables/{entityModelId}` exactly like Task 2's genTables — expect ~5 lines for TableConfigDto's required props. Capture verbatim from oasdiff in Step 3.)

- [ ] **Step 1: Fix the request body schema**

Before:
```yaml
      requestBody:
        content:
          application/json:
            schema:
              type: string
            examples:
```
After:
```yaml
      requestBody:
        content:
          application/json:
            schema:
              type: array
              items:
                $ref: "#/components/schemas/TableConfigDto"
            examples:
```
(Leave the existing `examples:` block unchanged — it is already an array of tables.)

- [ ] **Step 2: Verify the spec loads (subagent)**

Run: `go test ./api -v`
Expected: PASS.

- [ ] **Step 3: Run oasdiff, capture exact text (subagent)**

Run the Step-4 command from Task 2. Expect a `request-body-type-changed` ERR for `POST /sql/schema/updateTables/{entityModelId}` (mirrors the createCollection precedent at the top of the err-ignore file).

- [ ] **Step 4: Append the err-ignore entry (verbatim from Step 3)**

```
# SQL-Schema slice (group 5) — updateTables request body corrected string→array<TableConfigDto>;
# the prior string schema was prototype drift (the op is unrouted). ADR 0003 Decision 7.
<paste exact oasdiff ERR line here>
```
Re-run oasdiff; expected `exit=0`.

- [ ] **Step 5: Commit**

```bash
git add api/openapi.yaml .github/oasdiff-err-ignore.txt
git commit -m "fix(openapi): SQL-Schema updateTables request body array (D2)"
```

---

### Task 4: D3 — align the four `404` responses to `ProblemDetail`

`getSchemaByName`, `deleteSchemaByName`, `getSchema`, `deleteSchema` return `404` as `application/json` with no error schema; the platform standard (and these ops' own sibling `400`s on `saveSchema`/`putSchema`) is `application/problem+json ProblemDetail`.

**Files:**
- Modify: `api/openapi.yaml` (404 blocks: `getSchemaByName` ≈ 7283, `deleteSchemaByName` ≈ 7479, `getSchema` ≈ 8354, `deleteSchema` ≈ 8391)
- Modify: `.github/oasdiff-err-ignore.txt`

**Interfaces:**
- Consumes: `#/components/schemas/ProblemDetail`.
- Produces: consistent problem+json 404s; unrouted (no runtime impact).

- [ ] **Step 1: Read one existing correct 400 for the exact shape**

Read `saveSchema`'s `400` block (≈ line 7443-7448) — copy its `application/problem+json` + `schema: {$ref: ProblemDetail}` structure as the template (keep each 404's own `description`/`example` if present, but ensure a `schema` is set).

- [ ] **Step 2: Convert each of the four 404 blocks**

For each, transform:
```yaml
        "404":
          description: <keep existing>
          content:
            application/json:
              # (no schema, or an ad-hoc inline shape)
```
to:
```yaml
        "404":
          description: <keep existing>
          content:
            application/problem+json:
              schema:
                $ref: "#/components/schemas/ProblemDetail"
```
Preserve any existing `example:` under the new `application/problem+json` (adjust it to a valid ProblemDetail if it isn't).

- [ ] **Step 3: Verify the spec loads (subagent)**

Run: `go test ./api -v`
Expected: PASS.

- [ ] **Step 4: Run oasdiff, capture exact text (subagent)**

Run the Task-2 Step-4 command. Expect four `response-media-type-removed` ERR lines (the removed `application/json` on the 404 of each op); the added `application/problem+json` is additive (non-breaking). Mirrors the OIDC/message envelope precedents.

- [ ] **Step 5: Append err-ignore entries (verbatim)**

```
# SQL-Schema slice (group 5) — getSchemaByName/deleteSchemaByName/getSchema/deleteSchema
# 404 aligned application/json→application/problem+json ProblemDetail (platform standard,
# matching the ops' own 400s). Unrouted; prototype drift. ADR 0003 Decision 7.
<paste the four exact oasdiff ERR lines here>
```
Re-run oasdiff; expected `exit=0`.

- [ ] **Step 6: Commit**

```bash
git add api/openapi.yaml .github/oasdiff-err-ignore.txt
git commit -m "fix(openapi): SQL-Schema 404s to problem+json ProblemDetail (D3)"
```

---

### Task 5: D4 — reconcile `FieldConfigDto` schema with its examples

`FieldConfigDto` marks `hidden` and `isArray` **required**, but the `genTables`/`updateTables` examples omit them on every field and use an **unmodeled `arrayFields`**. Reconcile the schema to the examples (the intended shape): make `hidden`/`isArray` optional and model `arrayFields` as a recursive typed-but-open array.

**Files:**
- Modify: `api/openapi.yaml` (`FieldConfigDto` schema ≈ line 8441-8464)
- Modify: `.github/oasdiff-err-ignore.txt`

**Interfaces:**
- Consumes/Produces: `#/components/schemas/FieldConfigDto` (self-referential via `arrayFields`).

- [ ] **Step 1: Edit the `FieldConfigDto` schema**

Before:
```yaml
    FieldConfigDto:
      type: object
      properties:
        fieldName:
          type: string
        fieldKey:
          type: string
        fieldCategory:
          type: string
        dataType:
          type: string
        isArray:
          type: boolean
        hidden:
          type: boolean
        flatten:
          type: boolean
      required:
        - dataType
        - fieldCategory
        - fieldKey
        - fieldName
        - hidden
        - isArray
```
After:
```yaml
    FieldConfigDto:
      type: object
      properties:
        fieldName:
          type: string
        fieldKey:
          type: string
        fieldCategory:
          type: string
        dataType:
          type: string
        isArray:
          type: boolean
        hidden:
          type: boolean
        flatten:
          type: boolean
        arrayFields:
          type: array
          items:
            $ref: "#/components/schemas/FieldConfigDto"
      required:
        - dataType
        - fieldCategory
        - fieldKey
        - fieldName
```
(Dropped `hidden`/`isArray` from `required`; added recursive `arrayFields`. Object stays open — no `additionalProperties: false`.)

- [ ] **Step 2: Verify the spec loads with the recursive schema (subagent)**

Run: `go test ./api -v`
Expected: PASS (kin-openapi resolves the self-`$ref`).

- [ ] **Step 3: Eyeball example consistency**

Confirm the `genTables` example fields (e.g. `entity_id` with only `fieldName/fieldKey/fieldCategory/dataType`, and the nested `index` field with `arrayFields`) now satisfy the relaxed schema. No example edits should be needed; if any example still contradicts the schema, fix the example, not the schema.

- [ ] **Step 4: Run oasdiff, capture exact text (subagent)**

Run the Task-2 Step-4 command. Expect `response-required-property-removed` ERR lines for `hidden`/`isArray` on the responses that embed `FieldConfigDto` (`genTables` 200; `getSchemas` 200 via `SchemaConfigDto`→`TableConfigDto`→`FieldConfigDto`). The added `arrayFields` is additive.

- [ ] **Step 5: Append err-ignore entries (verbatim)**

```
# SQL-Schema slice (group 5) — FieldConfigDto reconciled to its examples: hidden/isArray
# made optional (examples always omitted them), arrayFields modelled (recursive). Unrouted;
# prototype drift. ADR 0003 Decision 7.
<paste exact oasdiff ERR lines here>
```
Re-run oasdiff; expected `exit=0`.

- [ ] **Step 6: Commit**

```bash
git add api/openapi.yaml .github/oasdiff-err-ignore.txt
git commit -m "fix(openapi): reconcile FieldConfigDto schema with examples (D4)"
```

---

### Task 6: Remove the vestigial `CQL Execution Statistics` exclude-tags entry

Zero ops carry this tag, so codegen output is byte-identical — a pure Gate-6 cleanup.

**Files:**
- Modify: `api/config.yaml` (remove line 10)
- (Regenerate) `api/generated.go` — must be unchanged.

**Interfaces:** none.

- [ ] **Step 1: Remove the entry**

Before:
```yaml
  exclude-tags:
    - "Stream Data"
    - "CQL Execution Statistics"
    - "SQL-Schema"
```
After:
```yaml
  exclude-tags:
    - "Stream Data"
    - "SQL-Schema"
```

- [ ] **Step 2: Regenerate and prove no diff (subagent)**

```bash
go generate ./api
make check-codegen
```
Expected: `check-codegen` green (generated.go byte-identical — no op carries the removed tag). If `generated.go` changed, STOP — an op unexpectedly carries the tag; investigate before proceeding.

- [ ] **Step 3: Commit**

```bash
git add api/config.yaml
git commit -m "chore(openapi): drop vestigial CQL exclude-tags entry (0 ops)"
```

---

### Task 7: Record the disposition + invariant in cloud-parity

Gate-4 doc task: mirror the group-5 disposition and the marker-backing invariant so Cloud follows the contract decision.

**Files:**
- Modify: `docs/cloud-parity/openapi-conformance.md` (append a section)

**Interfaces:** none.

- [ ] **Step 1: Read the current doc**

Read `docs/cloud-parity/openapi-conformance.md` to match its heading style and existing deferred-items sections.

- [ ] **Step 2: Append the disposition section**

Add (no issue numbers in the doc body — describe by surface/title):
```markdown
## Group 5 — dead-surface disposition (final reconciliation slice)

The excluded-tag / always-501 dead surface is disposed as follows:

- **SQL-Schema** (`/sql/schema/*`, 9 ops) — retained as `x-cyoda-status: planned`; the
  authored contract was corrected (well-formed array responses, array request body,
  `problem+json` 404s, `FieldConfigDto` reconciled to its examples). Implementation is
  tracked (Trino SQL management API). Cloud mirrors the corrected contract.
- **Stream Data** (`/platform-api/stream-data/*`, 13 ops) — retained as
  `x-cyoda-status: unimplemented`; left minimally touched pending a disposition decision
  (implement / redesign / remove). Tracked.
- **CQL Execution Statistics** — vestigial exclude-tags entry removed (no ops).
- **accountSubscriptionsGet** — unchanged (`planned`, routed, returns 501; tracked).

**Invariant:** every non-live `x-cyoda-status` marker is backed by a tracking issue, so a
marked surface can never become unowned relabeled fiction. The e2e conformance gate enforces
exactly-one-of {exercised, marked} and fails any marker that goes stale (op returns 2xx).
```

- [ ] **Step 3: Commit**

```bash
git add docs/cloud-parity/openapi-conformance.md
git commit -m "docs(cloud-parity): record group-5 dead-surface disposition + marker invariant"
```

---

### Task 8: Final consolidation & full verification (Gate 5)

Controller-run. Proves the whole slice is green before code review.

**Files:** none (verification only).

- [ ] **Step 1: Full e2e incl. conformance gate + 404 test (controller, Docker)**

Run: `go test ./internal/e2e/... -v`
Expected: PASS — `TestOpenAPIConformanceReport` green (all 23 `x-cyoda-status` markers still valid, none stale), `TestDeadSurfaceUnrouted` green.

- [ ] **Step 2: oasdiff gate final (subagent or controller)**

```bash
git show origin/release/v0.8.2:api/openapi.yaml > /tmp/base.yaml
oasdiff breaking /tmp/base.yaml api/openapi.yaml --fail-on ERR --err-ignore .github/oasdiff-err-ignore.txt > /tmp/oasdiff.out 2>&1; echo "exit=$?"
```
Expected: `exit=0`.

- [ ] **Step 3: codegen + static + format gates**

```bash
make check-codegen
go vet ./...
gofmt -l api/ internal/ docs/ 2>/dev/null   # expect no files listed under api//internal/
```
Expected: all clean.

- [ ] **Step 4: Race (controller, CI-parity scope)**

Run: `make race`
Expected: exit 0.

- [ ] **Step 5: Plugins short (controller)**

Run: `make test-short-all`
Expected: PASS.

- [ ] **Step 6: No final commit needed** — verification only. Proceed to `superpowers:requesting-code-review`, then `antigravity-bundle-security-developer:cc-skill-security-review`, then open the PR to `release/v0.8.2` (milestone v0.8.2) with body `Closes #369`, the fix-SQL-but-not-Stream asymmetry rationale, and the two tracking-issue references.

---

## Self-Review

**Spec coverage:** §3 decisions → Tasks 2-6 (SQL fixes D1-D4, CQL removal) + Task 7 (Stream Data no-op is inherent — no task edits it, as required) + accountSubscriptions untouched (no task). §4 D1-D4 → Tasks 2-5. §5 CQL + cloud-parity → Tasks 6-7. §5 issue-id rule → Global Constraints + Task 7 Step 2. §6 status table → Tasks 2-5. §7 coverage matrix → Task 1 (404 e2e), Task 8 (conformance gate), per-task oasdiff + `go test ./api`. §8 oasdiff → each SQL task's capture-verbatim steps. §9 verification → Task 8. §11 deliverables 1-7 → Tasks 2-8 + issues already created. All covered.

**Placeholder scan:** No TBD/TODO. The `<paste exact oasdiff ERR line>` markers are deliberate — the constraint is capture-at-impl (pinned-wording, fail-closed); the surrounding command shows exactly how to obtain the text. The `doAuthedRequest` helper name is explicitly flagged as "discover the real name from a neighbor test."

**Type consistency:** `TableConfigDto`, `SchemaConfigDto`, `FieldConfigDto`, `ProblemDetail` used consistently and all exist in `api/openapi.yaml` components. `arrayFields` self-references `FieldConfigDto`. Line numbers marked "≈" (the spec notes they drift; each task re-locates by content).
