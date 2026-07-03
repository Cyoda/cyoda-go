# OpenAPI Contract Status & Parity — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `api/openapi.yaml` honest and enforceable — every published operation is either *live* (routed + e2e-exercised) or *explicitly marked* not-live, with an additive-only CI gate guarding schema evolution.

**Architecture:** Add an `x-cyoda-status` operation-status extension to the spec; rework the existing e2e conformance gate (`internal/e2e`) from "skip excluded-tag ops + `knownUncoveredOps` allowlist" to a spec-derived "exactly-one-of {exercised, marked}" rule; add an `oasdiff` breaking-change CI gate; delete genuinely-dead schemas; record a Cloud-parity hand-off.

**Tech Stack:** Go 1.26; `github.com/getkin/kin-openapi v0.140.0`; existing `internal/e2e/openapivalidator` (kin-openapi response validation, `ModeEnforce`); testcontainers Postgres for e2e; GitHub Actions CI.

**Spec:** `docs/superpowers/specs/2026-07-03-openapi-contract-status-and-parity-design.md`. **ADR:** `docs/adr/0003-openapi-contract-conformance-and-evolution.md`. **Issue:** #369.

## Global Constraints

- Go 1.26+; `log/slog` only; wrap errors `fmt.Errorf("...: %w", err)`.
- `api/openapi.yaml` is embedded verbatim via `//go:embed` in `api/spec.go` (`embedded-spec: false`). Editing the file is sufficient — **there is no snapshot to regenerate.**
- **`docs/cyoda/` is read-only vendored Cloud reference — no code, test, or gate may read or assert against it.** cyoda-go leads the contract; Cloud conforms.
- Typed-but-open policy (ADR 0003): never `additionalProperties: false` on evolvable schemas.
- **Ordering invariant:** markers + `fetchEntityTransitions` coverage (Task 4/5) land *before* the gate rework (Task 6). Each task must leave `go test ./internal/e2e/... -run TestOpenAPIConformanceReport` green.
- Run e2e with Docker running: `go test ./internal/e2e/... -run <Name> -v`.

---

### Task 1: Collector records per-operation 2xx-success

**Files:**
- Modify: `internal/e2e/openapivalidator/collector.go`
- Modify: `internal/e2e/openapivalidator/validator.go` (add the `recordStatus` call at the existing `recordExercised` site)
- Test: `internal/e2e/openapivalidator/collector_test.go`

**Interfaces:**
- Produces: `func Success2xxSet() map[string]bool` (package-level; reads `defaultCollector`). `func (c *collector) recordStatus(operationId string, status int)`.

- [ ] **Step 1: Write the failing test** in `collector_test.go`:

```go
func TestCollector_recordStatus_tracks2xx(t *testing.T) {
	c := newCollector()
	c.recordStatus("opA", 200)
	c.recordStatus("opB", 501)
	c.recordStatus("opC", 404)
	got := c.success2xxSet()
	if !got["opA"] {
		t.Errorf("opA (200) should be in the 2xx set")
	}
	if got["opB"] || got["opC"] {
		t.Errorf("opB (501) / opC (404) must not be in the 2xx set: %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/e2e/openapivalidator/ -run TestCollector_recordStatus_tracks2xx -v`
Expected: FAIL — `recordStatus`/`success2xxSet` undefined.

- [ ] **Step 3: Implement in `collector.go`.** Add `saw2xx map[string]struct{}` to the `collector` struct; initialise it in `newCollector` (`saw2xx: make(map[string]struct{})`). Add:

```go
func (c *collector) recordStatus(operationId string, status int) {
	if status < 200 || status >= 300 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saw2xx[operationId] = struct{}{}
}

func (c *collector) success2xxSet() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]bool, len(c.saw2xx))
	for k := range c.saw2xx {
		out[k] = true
	}
	return out
}

// Success2xxSet returns the set of operationIds that returned a 2xx during the run.
func Success2xxSet() map[string]bool { return defaultCollector.success2xxSet() }
```

- [ ] **Step 4: Wire the recording.** In `validator.go`, at every site that calls `c.recordExercised(op)` (the successful route-match path), add immediately after it: `c.recordStatus(op, resp.StatusCode)`. (`resp *http.Response` is in scope there.)

- [ ] **Step 5: Run tests to verify pass**

Run: `go test ./internal/e2e/openapivalidator/ -v`
Expected: PASS (all, including existing).

- [ ] **Step 6: Commit**

```bash
git add internal/e2e/openapivalidator/collector.go internal/e2e/openapivalidator/validator.go internal/e2e/openapivalidator/collector_test.go
git commit -m "feat(e2e): collector tracks per-op 2xx success for the marker gate"
```

---

### Task 2: Permanent kin-openapi typed-but-open fixture (ADR 0003 evidence)

**Files:**
- Test: `internal/e2e/openapivalidator/typed_but_open_test.go` (create)

**Interfaces:** none exported; a standalone behavioral guard.

- [ ] **Step 1: Write the test** (this is the permanent version of the 2026-07-03 probe ADR 0003 references):

```go
package openapivalidator

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// Guards the typed-but-open policy (ADR 0003): an additive/unknown member
// validates against an open schema and is rejected by additionalProperties:false.
func TestTypedButOpen_AdditionalPropertiesSemantics(t *testing.T) {
	body := map[string]any{"id": "x", "extra": "y"} // "extra" = additive/unknown

	open := openapi3.NewObjectSchema().WithProperty("id", openapi3.NewStringSchema())
	if err := open.VisitJSON(body); err != nil {
		t.Fatalf("open schema must ACCEPT an additive field: %v", err)
	}

	sealed := openapi3.NewObjectSchema().WithProperty("id", openapi3.NewStringSchema())
	sealed.AdditionalProperties = openapi3.AdditionalProperties{Has: openapi3.BoolPtr(false)}
	if err := sealed.VisitJSON(body); err == nil {
		t.Fatal("additionalProperties:false must REJECT an additive field")
	}
}
```

- [ ] **Step 2: Run — expected PASS immediately** (documents current validator behaviour):

Run: `go test ./internal/e2e/openapivalidator/ -run TestTypedButOpen -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/openapivalidator/typed_but_open_test.go
git commit -m "test(e2e): pin kin-openapi typed-but-open semantics (ADR 0003)"
```

---

### Task 3: Marker-set plumbing (read `x-cyoda-status` in TestMain)

**Files:**
- Modify: `internal/e2e/e2e_test.go` (the `TestMain` loop that builds `allOperationIds` by iterating `swagger.Paths.Map()`)
- Test: `internal/e2e/marker_plumbing_test.go` (create)

**Interfaces:**
- Produces: package var `markedOps map[string]string` (operationId → status value, e.g. `"planned"`/`"unimplemented"`), populated in `TestMain` alongside `allOperationIds`.

- [ ] **Step 1: Write the failing test.** A pure helper is easier to test than `TestMain`; extract the extension read into a helper and test that:

```go
package e2e

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestReadCyodaStatus(t *testing.T) {
	op := &openapi3.Operation{
		OperationID: "foo",
		Extensions:  map[string]any{"x-cyoda-status": "planned"},
	}
	if got := readCyodaStatus(op); got != "planned" {
		t.Errorf("got %q, want planned", got)
	}
	bare := &openapi3.Operation{OperationID: "bar"}
	if got := readCyodaStatus(bare); got != "" {
		t.Errorf("unmarked op should return empty, got %q", got)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`readCyodaStatus` undefined):

Run: `go test ./internal/e2e/ -run TestReadCyodaStatus -v`

- [ ] **Step 3: Implement `readCyodaStatus`** in `e2e_test.go`:

```go
// readCyodaStatus returns the x-cyoda-status marker on an operation, or "".
func readCyodaStatus(op *openapi3.Operation) string {
	if op == nil || op.Extensions == nil {
		return ""
	}
	if v, ok := op.Extensions["x-cyoda-status"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
```

Add a package var `var markedOps = map[string]string{}`. In `TestMain`, inside the existing `for _, op := range item.Operations()` loop (where `allOperationIds` is appended), add — **before** the exclude-tag skip is removed in Task 6, place it after the `op.OperationID == ""` guard so it sees every op:

```go
if s := readCyodaStatus(op); s != "" {
	markedOps[op.OperationID] = s
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./internal/e2e/ -run TestReadCyodaStatus -v`

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/e2e_test.go internal/e2e/marker_plumbing_test.go
git commit -m "feat(e2e): read x-cyoda-status markers into markedOps"
```

---

### Task 4: Add `x-cyoda-status` markers to the spec (per-op status verified)

**Files:**
- Modify: `api/openapi.yaml` (add `x-cyoda-status` + a description caveat to each not-live operation)

**Pre-work — verify actual runtime status (do NOT trust the stale "stub" comment).** For each candidate op, determine whether it returns `501` or a real `2xx` under the e2e server config:
- `Stream Data` (13 ops) + `SQL-Schema` (9 ops): tag-excluded from codegen, unrouted → 404/501 → **`unimplemented`** / **`planned`** respectively (`planned` for SQL-Schema per the Trino decision, issue #83).
- `accountSubscriptionsGet`: genuine stub, always `501` (`internal/domain/account/handler.go:87`) → **`planned`**.
- OIDC provider ops (`registerOidcProvider`, `deleteOidcProvider`, `invalidateOidcProvider`, `reactivateOidcProvider`, `listOidcProviders`, `reloadOidcProviders`, `updateOidcProvider`): dispatched via `s.Account.<Op>` when wired else `501` (`internal/api/server.go:531+`). **Check `s.Account` in the e2e server construction (`app/app.go` / the e2e server setup).** If `s.Account == nil` in e2e → they return `501` → mark `planned`. If wired and returning `2xx` → they are **live**: do NOT mark; instead add e2e coverage (fold into Task 5). Record the finding in the commit message.

- [ ] **Step 1: Add markers.** For each not-live op, add the extension and a description caveat, e.g.:

```yaml
  /sql/schema/:
    get:
      operationId: getSchemaByName
      x-cyoda-status: planned
      description: |
        NOT YET IMPLEMENTED in cyoda-go. Planned for the Trino SQL surface.
        <existing description...>
```

`unimplemented` caveat wording: `NOT IMPLEMENTED in cyoda-go (disposition under review).`

- [ ] **Step 2: Verify the spec still parses** (the served spec + validator load it):

Run: `go test ./internal/e2e/ -run TestHealth -v`
Expected: PASS (server boots, `//go:embed` spec loads).

- [ ] **Step 3: Verify markers are read:** temporarily log `len(markedOps)` or add an assertion test that `markedOps` contains `getSchemaByName` → `planned`. Expected count ≈ 22 excluded-tag + verified stubs.

- [ ] **Step 4: Commit** (markers are inert under the *current* gate — CI stays green):

```bash
git add api/openapi.yaml
git commit -m "feat(openapi): mark not-live ops with x-cyoda-status (planned|unimplemented)

Per-op runtime status verified; OIDC group: <live vs 501 finding>. Refs #369."
```

---

### Task 5: e2e coverage for the now-known-live ops (`fetchEntityTransitions` + 7 OIDC provider ops)

**Files:**
- Test: `internal/e2e/entity_transitions_test.go` (create or extend an existing entity e2e test)
- Test: `internal/e2e/oidc_providers_test.go` (create)

**Scope note (from Task 4's finding):** Task 4's probe confirmed the 7 OIDC provider ops are **live** (`GET /oauth/oidc/providers` → 200 in JWT-mode e2e; adapter wired via `WithOIDCAdapter`), so they were NOT marked. The new gate (Task 6) requires every live op be *exercised*. This task therefore covers **both** `fetchEntityTransitions` **and** the 7 OIDC ops (`registerOidcProvider`, `listOidcProviders`, `reloadOidcProviders`, `updateOidcProvider`, `invalidateOidcProvider`, `reactivateOidcProvider`, `deleteOidcProvider`). Coverage here is **exercise-level** (hit each op, assert a sane status) — exhaustive OIDC error-code coverage is deferred to the auth/OIDC follow-on group.

`fetchEntityTransitions` (`GET /platform-api/entity/fetch/transitions`) is **live** — routed at `app/app.go:607` outside the generated `ServerInterface`. It is exempted today only via `knownUncoveredOps`; the new gate requires it be *exercised*.

**OIDC lifecycle test (`oidc_providers_test.go`):** authenticate with an admin token (reuse the existing e2e auth helper — see `helpers_test.go` / `clients_test.go` / `oauth_keys_test.go` for the admin-token pattern), then exercise all 7 ops in one flow: `registerOidcProvider` (create a provider) → `listOidcProviders` → `reloadOidcProviders` → `updateOidcProvider` → `invalidateOidcProvider` → `reactivateOidcProvider` → `deleteOidcProvider`, asserting each returns a success status (2xx). Model request bodies on the OIDC DTOs in `api/openapi.yaml` (`RegisterOidcProviderRequestDto`, etc.). Each op must be hit at least once so the conformance gate marks it exercised.

- [ ] **Step 1: Write an e2e test** that creates an entity in a known state and calls the endpoint, asserting a `200` and a JSON array (`TransitionNameList`). Model it on the existing `getEntityTransitions` e2e test (search `internal/e2e` for `transitions`). Include the required query params the handler reads (`HandleFetchTransitions`, `internal/domain/entity/transitions_handler.go`).

- [ ] **Step 2: Run — expect PASS** and confirm the op is now exercised:

Run: `go test ./internal/e2e/ -run TestFetchEntityTransitions -v`

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/entity_transitions_test.go
git commit -m "test(e2e): cover fetchEntityTransitions (removes knownUncoveredOps exemption)"
```

---

### Task 6: Rework the coverage gate (live-or-marked) — the flip

**Files:**
- Modify: `internal/e2e/e2e_test.go` (remove the exclude-tag skip so `allOperationIds` = every published op)
- Modify: `internal/e2e/zzz_openapi_conformance_test.go` (delete `knownUncoveredOps`; new rules)
- Test: `internal/e2e/gate_logic_test.go` (create — unit-test the pure rule function)

**Interfaces:**
- Produces: `func uncoveredOps(all []string, exercised, success2xx map[string]bool, marked map[string]string) (unmarkedUncovered, staleMarkers []string)`.

- [ ] **Step 1: Write the failing unit test** for the pure rule:

```go
func TestUncoveredOps_rules(t *testing.T) {
	all := []string{"live", "liveUncovered", "planned", "staleMarked"}
	exercised := map[string]bool{"live": true, "staleMarked": true}
	success2xx := map[string]bool{"live": true, "staleMarked": true}
	marked := map[string]string{"planned": "planned", "staleMarked": "planned"}

	unmarked, stale := uncoveredOps(all, exercised, success2xx, marked)

	// liveUncovered: neither exercised nor marked -> unmarked-uncovered
	if len(unmarked) != 1 || unmarked[0] != "liveUncovered" {
		t.Errorf("unmarked-uncovered = %v, want [liveUncovered]", unmarked)
	}
	// staleMarked: marked but returned 2xx -> stale marker
	if len(stale) != 1 || stale[0] != "staleMarked" {
		t.Errorf("stale = %v, want [staleMarked]", stale)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`uncoveredOps` undefined):

Run: `go test ./internal/e2e/ -run TestUncoveredOps_rules -v`

- [ ] **Step 3: Implement `uncoveredOps`** in `zzz_openapi_conformance_test.go`:

```go
func uncoveredOps(all []string, exercised, success2xx map[string]bool, marked map[string]string) (unmarkedUncovered, staleMarkers []string) {
	for _, op := range all {
		_, isMarked := marked[op]
		switch {
		case !exercised[op] && !isMarked:
			unmarkedUncovered = append(unmarkedUncovered, op) // silent 404 or untested live op
		case isMarked && success2xx[op]:
			staleMarkers = append(staleMarkers, op) // marker on an op that is actually live
		}
	}
	return
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./internal/e2e/ -run TestUncoveredOps_rules -v`

- [ ] **Step 5: Wire it into the enforce check and delete the old escape hatches.**
  - In `e2e_test.go` `TestMain`: **remove** the `for _, tag := range op.Tags { if excludeTags[tag] { skip } }` block so `allOperationIds` includes every operation. (Leave `api/config.yaml` `exclude-tags` untouched — that governs codegen, not coverage.)
  - In `zzz_openapi_conformance_test.go`: **delete** the `knownUncoveredOps` map and its usage. Replace the coverage block (currently `if !exercised[op] && !knownUncoveredOps[op]`) with:

```go
if !RunFilterActive() { // keep the existing single-test-run guard
	unmarked, stale := uncoveredOps(allOperationIds, exercised, openapivalidator.Success2xxSet(), markedOps)
	if len(unmarked) > 0 {
		t.Fatalf("openapi conformance: %d published ops are neither exercised nor x-cyoda-status-marked (silent 404 or untested live op): %v", len(unmarked), unmarked)
	}
	if len(stale) > 0 {
		t.Fatalf("openapi conformance: %d x-cyoda-status-marked ops returned 2xx (marker is stale — op is live): %v", len(stale), stale)
	}
}
```

  (`exercised` is the existing exercised set; `RunFilterActive()` is the existing guard from `openapivalidator`.)

- [ ] **Step 6: Run the full e2e conformance suite — expect GREEN** (markers from Task 4 + coverage from Task 5 make every op live-or-marked):

Run: `go test ./internal/e2e/ -run TestOpenAPIConformanceReport -v`
Expected: PASS. If it fails listing an op, that op is a real gap → cover it (if live) or mark it (if not) — do not re-add an allowlist.

- [ ] **Step 7: Commit**

```bash
git add internal/e2e/e2e_test.go internal/e2e/zzz_openapi_conformance_test.go internal/e2e/gate_logic_test.go
git commit -m "feat(e2e): live-or-marked coverage gate; retire knownUncoveredOps + exclude-tag skip"
```

---

### Task 7: Delete genuinely-dead schemas

**Files:**
- Modify: `api/openapi.yaml` (remove verified-dead component schemas)

- [ ] **Step 1: Verify `ErrorResponse` (bare) is dead.** It is defined once and never `$ref`d (every reference is `ErrorResponseDto`):

Run: `grep -n "ErrorResponse\b" api/openapi.yaml` and confirm the only hit is its `components.schemas.ErrorResponse:` definition (no `$ref: '#/components/schemas/ErrorResponse'`). Also confirm no generated Go type: `grep -n "type ErrorResponse struct" api/generated.go` (expect only `ErrorResponseDto`).

- [ ] **Step 2: Delete the `ErrorResponse` schema block** from `api/openapi.yaml`. Do **not** touch `StateMachine*Dto` (under-integrated, not dead — audit-group follow-on).

- [ ] **Step 3: Verify the spec still loads and no dangling ref**

Run: `go test ./internal/e2e/ -run TestHealth -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add api/openapi.yaml
git commit -m "chore(openapi): remove dead ErrorResponse schema (duplicate of ErrorResponseDto)"
```

---

### Task 8: oasdiff additive-only breaking-change CI gate

**Files:**
- Create: `.github/workflows/openapi-breaking-change.yml`
- Create: `internal/e2e/openapi_breaking_change_test.go` (the trust fixture — one breaking + one additive case)

**Interfaces:** none exported; a CI gate + a self-test of the gate's premise.

- [ ] **Step 1: Fixture test** proving oasdiff's premise holds (run oasdiff via `go run` or a vendored binary in CI; the Go test asserts the *classification* on two tiny in-repo fixture specs):

Write two fixture specs under `internal/e2e/testdata/oasdiff/` — `base.yaml`, `additive.yaml` (adds an optional response field), `breaking.yaml` (sets `additionalProperties: false`). The test shells out to `oasdiff breaking base additive` (expect exit 0 / no breaking) and `oasdiff breaking base breaking` (expect non-zero / breaking reported). Skip the test if the `oasdiff` binary is absent (`t.Skip`) so local runs without it still pass; CI installs it.

- [ ] **Step 2: CI workflow.** `.github/workflows/openapi-breaking-change.yml`:

```yaml
name: openapi-breaking-change
on:
  pull_request:
    paths: ["api/openapi.yaml"]
jobs:
  oasdiff:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - name: Install oasdiff
        run: go install github.com/oasdiff/oasdiff@latest
      - name: Breaking-change check (base..PR)
        run: |
          git show origin/${{ github.base_ref }}:api/openapi.yaml > /tmp/base-openapi.yaml
          oasdiff breaking /tmp/base-openapi.yaml api/openapi.yaml --fail-on ERR
```

- [ ] **Step 3: Resolve the deletion interaction (§5/§7 of the spec).** Confirm removing the unreferenced `ErrorResponse` (Task 7) is **not** flagged breaking (oasdiff keys on operations/referenced schemas; an unreferenced component removal is typically not breaking). If it *is* flagged, add `--exclude-elements` for that component or land Task 7 in a commit the gate's base already contains. Document the outcome in the workflow comments.

- [ ] **Step 4: Run the fixture test locally** (with oasdiff installed) — expect PASS; without it — expect SKIP:

Run: `go test ./internal/e2e/ -run TestOpenapiBreakingChange -v`

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/openapi-breaking-change.yml internal/e2e/openapi_breaking_change_test.go internal/e2e/testdata/oasdiff/
git commit -m "ci(openapi): additive-only breaking-change gate (oasdiff) per ADR 0003"
```

---

### Task 9: Docs — marker convention + Cloud-parity hand-off

**Files:**
- Modify: `CONTRIBUTING.md` (document the `x-cyoda-status` convention + the gate)
- Create: `docs/cloud-parity/openapi-conformance.md` (the Cloud-side hand-off record)

- [ ] **Step 1: `CONTRIBUTING.md`** — add a short "OpenAPI operation status" section: every published operation must be either e2e-exercised (live) or carry `x-cyoda-status: planned|unimplemented` (not-live); the e2e conformance gate enforces this; an `oasdiff` CI gate rejects breaking spec edits (sealing an open object, removing a field/op, narrowing a type). Note `additionalProperties: false` is disallowed on evolvable schemas (ADR 0003).

- [ ] **Step 2: `docs/cloud-parity/openapi-conformance.md`** — record the contract expectation for Cyoda Cloud: Cloud consumes the published `api/openapi.yaml` (served at `/openapi.json`, help subsystem); the **live common ground** = operations *without* `x-cyoda-status`; Cloud MUST conform to it and MAY only extend it; the tolerant-reader (must-ignore-unknown) obligation sits on Cloud (ADR 0003 Decision 3). List the **deferred open questions** for the entity slice: E6 `fieldsChangedCount` (does Cloud emit it?), D2 gRPC conditional-delete parity (does Cloud expect it?). Mark this as a hand-off to the Cloud team (issue #369).

- [ ] **Step 3: Commit**

```bash
git add CONTRIBUTING.md docs/cloud-parity/openapi-conformance.md
git commit -m "docs: x-cyoda-status convention + Cloud-parity conformance hand-off (#369)"
```

---

## Coverage matrix (carried from the spec)

| Scenario | Unit | Running-backend e2e | Parity | gRPC |
|---|---|---|---|---|
| Collector 2xx flag | ✓ (Task 1) | — | — | — |
| kin-openapi typed-but-open | ✓ (Task 2) | — | — | — |
| Marker read | ✓ (Task 3) | — | — | — |
| Gate rules (unmarked-uncovered / stale-marker) | ✓ (Task 6) | ✓ (suite enforces, Task 6.6) | — | — |
| `fetchEntityTransitions` exercised | — | ✓ (Task 5) | — | — |
| oasdiff breaking vs additive | ✓ fixture (Task 8) | — | — | — |

No new error codes (no `errors/<CODE>.md` needed); no new endpoints (no per-endpoint error table). Concurrency: n/a. gRPC: unaffected by this plan (entity slice handles gRPC).

## Gate obligations (Gate 4 / 7)

- Gate 4 docs: `CONTRIBUTING.md` (Task 9); no README/COMPATIBILITY/CHANGELOG change (no env var, no version bump, no public API surface change). If the oasdiff job is later added to a `make` target, update `CONTRIBUTING.md`.
- Gate 7: `docs/cloud-parity/openapi-conformance.md` (Task 9).

## Notes for the executor / a fresh session

- Full effort context: memory note `project_openapi_reconciliation_effort_state` + issue #369.
- This is **Spec 1** (prerequisite). After it merges, write + execute the **entity slice** plan from `docs/superpowers/specs/2026-07-02-openapi-contract-reconciliation-design.md`.
- Do not re-verify: kin-openapi typed-but-open semantics (Task 2 pins them); the `//go:embed` mechanism (no snapshot).
