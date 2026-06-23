# Workflow `annotations` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an optional, opaque, client-owned `annotations` JSON-object field to workflows, states, and transitions — stored and round-tripped (compacted) by the engine, never interpreted.

**Architecture:** The workflow types live in the sibling `cyoda-go-spi` repo; add `Annotations json.RawMessage` to three structs there first, compose locally via `go.work`, then implement validation/normalisation and wiring in cyoda-go, and finish by bumping the SPI pseudo-version pin. Validation at the import boundary canonicalises each value (object-only, compacted, 64 KB cap); persistence and export need no changes because workflows are stored as whole JSON blobs and export marshals the SPI structs directly.

**Tech Stack:** Go 1.26, `encoding/json` (`json.RawMessage`, `json.Compact`), `log/slog`, testcontainers-go (E2E PostgreSQL), the cyoda-go parity registry.

**Spec:** `docs/superpowers/specs/2026-06-22-workflow-annotations-design.md`

## Global Constraints

- **Go 1.26+.** Use `log/slog` exclusively; never `log.Printf`/`fmt.Printf` for operational logging.
- **No issue IDs in shipped artefacts** — no `#NNN` in error messages, response bodies, code comments, OpenAPI, or help content. Issue IDs belong only in commits/PR bodies.
- **4xx errors carry full domain detail with an error code**; the annotations validator returns `400 VALIDATION_FAILED` via `common.Operational(http.StatusBadRequest, common.ErrCodeValidationFailed, msg)`.
- **Cross-repo pin sync.** `github.com/cyoda-platform/cyoda-go-spi` must be pinned to the **same** version string in all four manifests: root `go.mod`, `plugins/memory/go.mod`, `plugins/postgres/go.mod`, `plugins/sqlite/go.mod`. CI gate: `make check-spi-pin-sync`.
- **Mid-milestone SPI window.** The SPI is pseudo-version-pinned to `cyoda-go-spi` `main` HEAD (no tag). Land the SPI change on `main`, then bump the pseudo-version pin — do **not** cut an SPI tag.
- **No schema-version bump.** `annotations` folds into the unreleased `1.1` contract. `CurrentSchemaVersion` and `SupportedSchemaRanges` are unchanged.
- **`annotations` is optional everywhere** — `omitempty`; never added to any OpenAPI `required` list; absent/`null` normalise to no-annotations and are omitted on export.
- **Field name is exactly `annotations`** (JSON tag) / `Annotations` (Go). Object-only; per-field cap `maxAnnotationsBytes = 64 * 1024`.

## File Map

| File | Change |
|---|---|
| `cyoda-go-spi/types.go` | Add `Annotations json.RawMessage` to `WorkflowDefinition`, `StateDefinition`, `TransitionDefinition` |
| `cyoda-go-spi/types_test.go` | Add round-trip test |
| `go.work` (worktree, **local-only, never committed**) | Add local SPI checkout for development |
| `internal/domain/workflow/validate.go` | Add `maxAnnotationsBytes`, `canonicalizeAnnotations`, `validateAndNormalizeAnnotations` |
| `internal/domain/workflow/validate_test.go` | Unit tests for the above |
| `internal/domain/workflow/handler.go` | Add `Annotations` to `workflowImportDef` + the `incoming` literal; call the normaliser |
| `internal/e2e/workflow_annotations_test.go` | New E2E round-trip / omission / rejection tests |
| `e2e/parity/workflow.go` + `e2e/parity/registry.go` | New parity round-trip test + registration |
| `api/openapi.yaml` | Add `annotations` property to the three DTOs |
| `cmd/cyoda/help/content/workflows.md` | Document `annotations` on three field lists + export-omission note + example |
| `docs/workflow-schema-versioning.md` | Note `annotations` in the `1.1` changelog entry |
| `CHANGELOG.md` | Feature entry under `[Unreleased]` |
| root + 3 plugin `go.mod` | Bump SPI pseudo-version pin (final task) |

---

### Task 1: SPI — add `Annotations` to the three workflow types

**Repo:** `cyoda-go-spi` (`/Users/paul/go-projects/cyoda-light/cyoda-go-spi`). Work on a branch, e.g. `feat/workflow-annotations`.

**Files:**
- Modify: `types.go` (`WorkflowDefinition` ~L114, `StateDefinition` ~L125, `TransitionDefinition` ~L130)
- Test: `types_test.go`

**Interfaces:**
- Produces: `spi.WorkflowDefinition.Annotations`, `spi.StateDefinition.Annotations`, `spi.TransitionDefinition.Annotations`, all `json.RawMessage` with tag `json:"annotations,omitempty"`.

- [ ] **Step 1: Write the failing test**

Append to `types_test.go` (package `spi`, `encoding/json` and `strings` already imported):

```go
func TestWorkflowAnnotations_RoundTrip(t *testing.T) {
	wf := WorkflowDefinition{
		Name:         "wf",
		Version:      "1.1",
		InitialState: "S",
		Active:       true,
		Annotations:  json.RawMessage(`{"roles":["admin"]}`),
		States: map[string]StateDefinition{
			"S": {
				Annotations: json.RawMessage(`{"label":"Start"}`),
				Transitions: []TransitionDefinition{
					{Name: "t", Next: "S", Annotations: json.RawMessage(`{"icon":"x"}`)},
				},
			},
		},
	}
	bs, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"annotations":{"roles":["admin"]}`, `"annotations":{"label":"Start"}`, `"annotations":{"icon":"x"}`} {
		if !strings.Contains(string(bs), want) {
			t.Errorf("marshalled JSON missing %s: %s", want, bs)
		}
	}

	var back WorkflowDefinition
	if err := json.Unmarshal(bs, &back); err != nil {
		t.Fatal(err)
	}
	if string(back.Annotations) != `{"roles":["admin"]}` {
		t.Errorf("workflow annotations round-trip: got %s", back.Annotations)
	}
	if string(back.States["S"].Annotations) != `{"label":"Start"}` {
		t.Errorf("state annotations round-trip: got %s", back.States["S"].Annotations)
	}
	if string(back.States["S"].Transitions[0].Annotations) != `{"icon":"x"}` {
		t.Errorf("transition annotations round-trip: got %s", back.States["S"].Transitions[0].Annotations)
	}

	// Absent annotations are omitted (omitempty).
	plain, _ := json.Marshal(WorkflowDefinition{Name: "p", Version: "1.1", InitialState: "S", States: map[string]StateDefinition{"S": {}}})
	if strings.Contains(string(plain), "annotations") {
		t.Errorf("nil annotations should be omitted, got %s", plain)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./... -run TestWorkflowAnnotations_RoundTrip`
Expected: FAIL — compile error `unknown field 'Annotations' in struct literal`.

- [ ] **Step 3: Add the fields**

In `types.go`, `WorkflowDefinition` — add the `Annotations` line after `States`:

```go
	States       map[string]StateDefinition `json:"states"`
	Annotations  json.RawMessage            `json:"annotations,omitempty"`
}
```

`StateDefinition`:

```go
type StateDefinition struct {
	Transitions []TransitionDefinition `json:"transitions,omitempty"`
	Annotations json.RawMessage        `json:"annotations,omitempty"`
}
```

`TransitionDefinition` — add after `Schedule`:

```go
	Schedule    *TransitionSchedule   `json:"schedule,omitempty"`
	Annotations json.RawMessage       `json:"annotations,omitempty"`
}
```

Add a one-line doc comment above each new field, e.g.:

```go
	// Annotations is arbitrary client-owned metadata, stored and
	// round-tripped verbatim and never interpreted by the engine.
	Annotations json.RawMessage `json:"annotations,omitempty"`
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit (in the SPI repo, on the feature branch)**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git checkout -b feat/workflow-annotations
git add types.go types_test.go
git commit -m "feat(types): add Annotations to workflow/state/transition definitions"
```

(The SPI PR is opened and merged to `main` as part of Task 9. Do not tag.)

---

### Task 2: Local SPI composition via `go.work`

Make the cyoda-go worktree build against the local SPI checkout (which now has `Annotations`) so the rest of the tasks can be developed and tested before the SPI change is merged/pinned.

**Files:**
- Modify (LOCAL ONLY, never `git add`): `go.work`

- [ ] **Step 1: Point `go.work` at the local SPI checkout**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat+workflow-meta-field
go work use /Users/paul/go-projects/cyoda-light/cyoda-go-spi
```

This appends `use /Users/paul/go-projects/cyoda-light/cyoda-go-spi` (as a relative path) to `go.work`. **Do not commit `go.work`** — the committed integration is the pseudo-version pin (Task 9). The agent must never run `git add go.work`.

- [ ] **Step 2: Verify the workspace sees the new field**

Run: `go build ./... && go vet ./internal/domain/workflow/...`
Expected: builds clean; the SPI `Annotations` field is now resolvable from the worktree.

- [ ] **Step 3: Confirm baseline is green**

Run: `go test -short ./internal/domain/workflow/...`
Expected: PASS (no behaviour change yet).

(No commit — this task produces only local, uncommitted `go.work` state.)

---

### Task 3: Annotations validation + normalisation helper

**Files:**
- Modify: `internal/domain/workflow/validate.go`
- Test: `internal/domain/workflow/validate_test.go`

**Interfaces:**
- Produces:
  - `const maxAnnotationsBytes = 64 * 1024`
  - `func canonicalizeAnnotations(raw json.RawMessage, location string) (json.RawMessage, error)` — returns compacted object bytes, or `nil` for absent/`null`/empty; error for non-object or over-cap.
  - `func validateAndNormalizeAnnotations(workflows []spi.WorkflowDefinition) error` — mutates each workflow/state/transition `Annotations` in place to its canonical form; returns the first validation error.

- [ ] **Step 1: Write the failing tests**

Append to `internal/domain/workflow/validate_test.go`:

```go
func wfWithAnnotations(wfA, stateA, trA json.RawMessage) []spi.WorkflowDefinition {
	return []spi.WorkflowDefinition{{
		Name:         "wf",
		Version:      "1.1",
		InitialState: "S",
		Active:       true,
		Annotations:  wfA,
		States: map[string]spi.StateDefinition{
			"S": {
				Annotations: stateA,
				Transitions: []spi.TransitionDefinition{
					{Name: "t", Next: "S", Annotations: trA},
				},
			},
		},
	}}
}

func TestValidateAndNormalizeAnnotations_ObjectCompacted(t *testing.T) {
	wfs := wfWithAnnotations(
		json.RawMessage(`{ "a" : 1 }`),
		json.RawMessage("{\n  \"b\": 2\n}"),
		json.RawMessage(`{"c":3}`),
	)
	if err := validateAndNormalizeAnnotations(wfs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(wfs[0].Annotations); got != `{"a":1}` {
		t.Errorf("workflow annotations not compacted: %s", got)
	}
	if got := string(wfs[0].States["S"].Annotations); got != `{"b":2}` {
		t.Errorf("state annotations not compacted: %s", got)
	}
	if got := string(wfs[0].States["S"].Transitions[0].Annotations); got != `{"c":3}` {
		t.Errorf("transition annotations not compacted: %s", got)
	}
}

func TestValidateAndNormalizeAnnotations_NullAndAbsentNormaliseToNil(t *testing.T) {
	wfs := wfWithAnnotations(json.RawMessage("null"), nil, json.RawMessage("  "))
	if err := validateAndNormalizeAnnotations(wfs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wfs[0].Annotations != nil {
		t.Errorf("workflow null annotations should normalise to nil, got %s", wfs[0].Annotations)
	}
	if wfs[0].States["S"].Annotations != nil {
		t.Errorf("state absent annotations should be nil, got %s", wfs[0].States["S"].Annotations)
	}
	if wfs[0].States["S"].Transitions[0].Annotations != nil {
		t.Errorf("transition blank annotations should be nil, got %s", wfs[0].States["S"].Transitions[0].Annotations)
	}
}

func TestValidateAndNormalizeAnnotations_NonObjectRejected(t *testing.T) {
	cases := map[string]json.RawMessage{
		"array":  json.RawMessage(`[1,2,3]`),
		"string": json.RawMessage(`"hello"`),
		"number": json.RawMessage(`5`),
		"bool":   json.RawMessage(`true`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateAndNormalizeAnnotations(wfWithAnnotations(raw, nil, nil))
			if err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
				t.Fatalf("expected object-only error, got %v", err)
			}
		})
	}
}

func TestValidateAndNormalizeAnnotations_LocationInError(t *testing.T) {
	// State-level offender names the state.
	err := validateAndNormalizeAnnotations(wfWithAnnotations(nil, json.RawMessage(`[1]`), nil))
	if err == nil || !strings.Contains(err.Error(), `state "S"`) {
		t.Fatalf("expected state location in error, got %v", err)
	}
	// Transition-level offender names the transition.
	err = validateAndNormalizeAnnotations(wfWithAnnotations(nil, nil, json.RawMessage(`"x"`)))
	if err == nil || !strings.Contains(err.Error(), `transition "t"`) {
		t.Fatalf("expected transition location in error, got %v", err)
	}
}

func TestValidateAndNormalizeAnnotations_SizeCap(t *testing.T) {
	// At cap: a compacted object of exactly maxAnnotationsBytes is accepted.
	filler := strings.Repeat("a", maxAnnotationsBytes-len(`{"k":""}`))
	atCap := json.RawMessage(`{"k":"` + filler + `"}`)
	if l := len(atCap); l != maxAnnotationsBytes {
		t.Fatalf("test setup: atCap len = %d, want %d", l, maxAnnotationsBytes)
	}
	if err := validateAndNormalizeAnnotations(wfWithAnnotations(atCap, nil, nil)); err != nil {
		t.Fatalf("at-cap annotations should be accepted: %v", err)
	}
	// Over cap by one byte: rejected.
	over := json.RawMessage(`{"k":"` + filler + `b"}`)
	err := validateAndNormalizeAnnotations(wfWithAnnotations(over, nil, nil))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size-cap error, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/domain/workflow/ -run TestValidateAndNormalizeAnnotations`
Expected: FAIL — compile error `undefined: validateAndNormalizeAnnotations` / `undefined: maxAnnotationsBytes`.

- [ ] **Step 3: Implement the helper**

In `internal/domain/workflow/validate.go`, extend the import block to add `bytes` and `encoding/json`:

```go
import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)
```

Add (e.g. just below the `maxIdentifierLen` const block):

```go
// maxAnnotationsBytes caps each individual annotations object at 64 KB,
// measured on its compacted form. Aggregate annotations across a workflow
// are already bounded by the 10 MB import-body cap; this per-field guard
// stops a single bloated blob. Sits above the 256-char identifier cap by
// three orders of magnitude — annotations carry structured client data
// (role lists, labels, UI hints), not identifiers.
const maxAnnotationsBytes = 64 * 1024

// canonicalizeAnnotations validates one annotations value and returns its
// canonical (compacted) form, or nil when the value is absent, blank, or
// the JSON literal null. The engine never interprets the contents — this
// only enforces that what is stored is a bounded JSON object. The location
// string (e.g. `workflow "x" state "y"`) is used solely to build errors.
func canonicalizeAnnotations(raw json.RawMessage, location string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	// The value is already syntactically valid JSON (the strict decoder
	// validated it when populating the RawMessage), so a leading '{' is a
	// sufficient and necessary marker of a JSON object.
	if trimmed[0] != '{' {
		return nil, fmt.Errorf("%s: annotations must be a JSON object", location)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		// Unreachable in practice (decoder already validated); defensive.
		return nil, fmt.Errorf("%s: annotations is not valid JSON: %v", location, err)
	}
	if buf.Len() > maxAnnotationsBytes {
		return nil, fmt.Errorf("%s: annotations size %d bytes exceeds the %d-byte limit",
			location, buf.Len(), maxAnnotationsBytes)
	}
	out := make(json.RawMessage, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}

// validateAndNormalizeAnnotations canonicalises the annotations on every
// workflow, state, and transition in the incoming slice, mutating each in
// place. Returns the first validation error (object-only, size cap). Run on
// the incoming import request only — consistent with the other structural
// validators, which are not retroactive against already-stored workflows.
func validateAndNormalizeAnnotations(workflows []spi.WorkflowDefinition) error {
	for i := range workflows {
		wf := &workflows[i]
		canon, err := canonicalizeAnnotations(wf.Annotations, fmt.Sprintf("workflow %q", wf.Name))
		if err != nil {
			return err
		}
		wf.Annotations = canon
		for stateName, stateDef := range wf.States {
			sCanon, err := canonicalizeAnnotations(stateDef.Annotations,
				fmt.Sprintf("workflow %q state %q", wf.Name, stateName))
			if err != nil {
				return err
			}
			stateDef.Annotations = sCanon
			for j := range stateDef.Transitions {
				tr := &stateDef.Transitions[j]
				tCanon, err := canonicalizeAnnotations(tr.Annotations,
					fmt.Sprintf("workflow %q state %q transition %q", wf.Name, stateName, tr.Name))
				if err != nil {
					return err
				}
				tr.Annotations = tCanon
			}
			// Map values are not addressable — write the state (with its
			// normalised annotations) back. The transitions slice is shared
			// by reference, but the state-level Annotations assignment above
			// only touched the local copy.
			wf.States[stateName] = stateDef
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/domain/workflow/ -run TestValidateAndNormalizeAnnotations -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/domain/workflow/validate.go internal/domain/workflow/validate_test.go
git commit -m "feat(workflow): validate + canonicalize annotations (object-only, 64KB)"
```

---

### Task 4: Wire annotations through the import handler + E2E coverage

**Files:**
- Modify: `internal/domain/workflow/handler.go` (`workflowImportDef` L74-82; `incoming` literal L149-157; insert call after L233)
- Test: `internal/e2e/workflow_annotations_test.go` (new)

**Interfaces:**
- Consumes: `validateAndNormalizeAnnotations` (Task 3); E2E helpers `importModelE2E`, `importWorkflowE2E`, `exportWorkflowE2E` (existing in `internal/e2e/workflow_test.go`).

- [ ] **Step 1: Write the failing E2E test**

Create `internal/e2e/workflow_annotations_test.go`:

```go
package e2e

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const annotatedWorkflowPayload = `{
  "importMode": "REPLACE",
  "workflows": [{
    "version": "1.1",
    "name": "annot-wf",
    "initialState": "S",
    "active": true,
    "annotations": { "roles": ["admin"], "label": "WF" },
    "states": {
      "S": {
        "annotations": { "ui": "start" },
        "transitions": [
          { "name": "t", "next": "S", "manual": true, "annotations": { "icon": "x" } }
        ]
      }
    }
  }]
}`

const plainWorkflowPayload = `{
  "importMode": "REPLACE",
  "workflows": [{
    "version": "1.1", "name": "plain-wf", "initialState": "S", "active": true,
    "states": { "S": { "transitions": [ { "name": "t", "next": "S", "manual": true } ] } }
  }]
}`

func firstWorkflow(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	wfs, ok := body["workflows"].([]any)
	if !ok || len(wfs) == 0 {
		t.Fatalf("export: expected workflows array, got %T", body["workflows"])
	}
	wf, ok := wfs[0].(map[string]any)
	if !ok {
		t.Fatalf("export: expected workflow object, got %T", wfs[0])
	}
	return wf
}

func TestWorkflowAnnotations_RoundTrip(t *testing.T) {
	const entity, version = "annot-rt", 1
	importModelE2E(t, entity, version)
	if status, body := importWorkflowE2E(t, entity, version, annotatedWorkflowPayload); status != http.StatusOK {
		t.Fatalf("import: expected 200, got %d: %s", status, body)
	}
	status, body := exportWorkflowE2E(t, entity, version)
	if status != http.StatusOK {
		t.Fatalf("export: expected 200, got %d", status)
	}
	wf := firstWorkflow(t, body)
	if got, want := wf["annotations"], map[string]any{"roles": []any{"admin"}, "label": "WF"}; !reflect.DeepEqual(got, want) {
		t.Errorf("workflow annotations: got %#v, want %#v", got, want)
	}
	state := wf["states"].(map[string]any)["S"].(map[string]any)
	if got, want := state["annotations"], map[string]any{"ui": "start"}; !reflect.DeepEqual(got, want) {
		t.Errorf("state annotations: got %#v, want %#v", got, want)
	}
	tr := state["transitions"].([]any)[0].(map[string]any)
	if got, want := tr["annotations"], map[string]any{"icon": "x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("transition annotations: got %#v, want %#v", got, want)
	}
}

func TestWorkflowAnnotations_AbsentOmittedOnExport(t *testing.T) {
	const entity, version = "annot-absent", 1
	importModelE2E(t, entity, version)
	if status, body := importWorkflowE2E(t, entity, version, plainWorkflowPayload); status != http.StatusOK {
		t.Fatalf("import: expected 200, got %d: %s", status, body)
	}
	_, body := exportWorkflowE2E(t, entity, version)
	wf := firstWorkflow(t, body)
	if _, present := wf["annotations"]; present {
		t.Errorf("absent annotations should be omitted on export, got %#v", wf["annotations"])
	}
}

func TestWorkflowAnnotations_NonObjectRejected(t *testing.T) {
	const entity, version = "annot-bad", 1
	importModelE2E(t, entity, version)
	payload := strings.Replace(annotatedWorkflowPayload, `"annotations": { "roles": ["admin"], "label": "WF" }`, `"annotations": [1,2,3]`, 1)
	status, body := importWorkflowE2E(t, entity, version, payload)
	if status != http.StatusBadRequest || !strings.Contains(body, "VALIDATION_FAILED") {
		t.Fatalf("expected 400 VALIDATION_FAILED, got %d: %s", status, body)
	}
}

var _ = json.Marshal // keep encoding/json imported if unused above
```

(If `json` ends up unused, drop the import and the trailing `var _` line.)

- [ ] **Step 2: Run the E2E test to verify it fails**

Run: `go test ./internal/e2e/ -run TestWorkflowAnnotations -v` (requires Docker)
Expected: FAIL — `TestWorkflowAnnotations_RoundTrip` reports the workflow-level `annotations` missing (the strict decoder rejects the unknown workflow-level field, returning 400, so import fails) **or** the round-trip assertion fails.

- [ ] **Step 3: Wire the handler**

In `handler.go`, add `Annotations` to `workflowImportDef` (after `States`):

```go
	States       map[string]spi.StateDefinition `json:"states"`
	Annotations  json.RawMessage                `json:"annotations,omitempty"`
}
```

Add the field to the `incoming[i]` literal (after `States: w.States,`):

```go
		incoming[i] = spi.WorkflowDefinition{
			Version:      w.Version,
			Name:         w.Name,
			Description:  w.Description,
			InitialState: w.InitialState,
			Active:       active,
			Criterion:    w.Criterion,
			States:       w.States,
			Annotations:  w.Annotations,
		}
```

Insert the normalisation call immediately after the existing `validateImportRequest` block (the block ending at the `return` after line 233), before `result := applyImportMode(...)`:

```go
	// Canonicalise and validate client-owned annotations on the incoming
	// workflows (object-only, compacted, 64 KB per field). Mutates incoming
	// in place so the stored/exported form is canonical. Incoming-only,
	// matching the non-retroactive structural-validation policy above.
	if err := validateAndNormalizeAnnotations(incoming); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeValidationFailed, err.Error()))
		return
	}
```

- [ ] **Step 4: Run the E2E test to verify it passes**

Run: `go test ./internal/e2e/ -run TestWorkflowAnnotations -v`
Expected: PASS (round-trip, omission, rejection).

- [ ] **Step 5: Commit**

```bash
git add internal/domain/workflow/handler.go internal/e2e/workflow_annotations_test.go
git commit -m "feat(workflow): accept + round-trip annotations through import/export"
```

---

### Task 5: Parity test — annotations round-trip across backends

So every storage backend (including the commercial Cassandra backend, which consumes the parity registry) inherits annotations round-trip coverage.

**Files:**
- Modify: `e2e/parity/workflow.go` (add payload + test func), `e2e/parity/registry.go` (register)

**Interfaces:**
- Consumes: `BackendFixture`, `client.NewClient`, `c.ImportModel`, `c.LockModel`, `c.ImportWorkflow`, `c.ExportWorkflow` (mirror `RunWorkflowImportExport` in `e2e/parity/workflow.go`).
- Produces: `RunWorkflowAnnotationsRoundTrip(t *testing.T, fixture BackendFixture)` registered as `{"WorkflowAnnotationsRoundTrip", RunWorkflowAnnotationsRoundTrip}`.

- [ ] **Step 1: Write the failing parity test**

In `e2e/parity/workflow.go`, add a payload constant near `workflowRoundTripPayload` and the test function (mirroring `RunWorkflowImportExport`'s setup — import model, lock, import workflow, export):

```go
const workflowAnnotationsPayload = `{
  "importMode": "REPLACE",
  "workflows": [{
    "version": "1.1", "name": "annot-wf", "initialState": "NONE", "active": true,
    "annotations": { "roles": ["admin"] },
    "states": { "NONE": { "annotations": { "ui": "start" }, "transitions": [] } }
  }]
}`

// RunWorkflowAnnotationsRoundTrip verifies client-owned annotations survive
// an import → export cycle on every backend.
func RunWorkflowAnnotationsRoundTrip(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "wf-annotations-test"
	const modelVersion = 1

	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"Test Order","amount":100,"status":"draft"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, workflowAnnotationsPayload); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	raw, err := c.ExportWorkflow(t, modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportWorkflow: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("ExportWorkflow: parse: %v", err)
	}
	wf := body["workflows"].([]any)[0].(map[string]any)
	if got, want := wf["annotations"], map[string]any{"roles": []any{"admin"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("workflow annotations: got %#v, want %#v", got, want)
	}
	state := wf["states"].(map[string]any)["NONE"].(map[string]any)
	if got, want := state["annotations"], map[string]any{"ui": "start"}; !reflect.DeepEqual(got, want) {
		t.Errorf("state annotations: got %#v, want %#v", got, want)
	}
}
```

Ensure `reflect` is imported in `e2e/parity/workflow.go` (add to the import block if absent).

In `e2e/parity/registry.go`, add to the `allTests` slice (next to the `WorkflowImportExport` entry ~L44):

```go
	{"WorkflowAnnotationsRoundTrip", RunWorkflowAnnotationsRoundTrip},
```

- [ ] **Step 2: Run to verify it fails (if run before Task 4) or passes (after)**

Run: `go test ./internal/e2e/ -run 'Parity.*WorkflowAnnotationsRoundTrip' -v` (the parity registry is executed by the e2e backend driver; use the driver's test name — confirm via `grep -rn "AllTests()" internal/e2e e2e`).
Expected: PASS now that Task 4 wired the handler. (If you authored this before Task 4, it FAILs on the missing annotations.)

- [ ] **Step 3: Commit**

```bash
git add e2e/parity/workflow.go e2e/parity/registry.go
git commit -m "test(parity): annotations round-trip across all backends"
```

---

### Task 6: OpenAPI — declare `annotations` on the three DTOs

Contract accuracy: the export response (validated in `ModeEnforce`) carries `annotations`; declare it. It is **never** added to a `required` list.

**Files:**
- Modify: `api/openapi.yaml` (`StateDefinitionDto` L8925, `TransitionDefinitionDto` L8933, `WorkflowConfigurationDto` L9022)
- Test: `internal/domain/workflow/openapi_consistency_test.go` (must stay green)

- [ ] **Step 1: Add the property to `StateDefinitionDto`**

Under `StateDefinitionDto.properties`, after the `transitions` property:

```yaml
    annotations:
      type: object
      additionalProperties: true
      description: >
        Arbitrary client-owned metadata, stored and round-tripped (compacted)
        and never interpreted by the engine. Must be a JSON object; capped at
        64 KB.
```

- [ ] **Step 2: Add the property to `TransitionDefinitionDto`**

Under `TransitionDefinitionDto.properties`, after `criterion` (and before the `required:` block — do **not** add it to `required`):

```yaml
    annotations:
      type: object
      additionalProperties: true
      description: >
        Arbitrary client-owned metadata, stored and round-tripped (compacted)
        and never interpreted by the engine. Must be a JSON object; capped at
        64 KB.
```

- [ ] **Step 3: Add the property to `WorkflowConfigurationDto`**

Under `WorkflowConfigurationDto.properties`, after `criterion` (and before `required:` — do **not** add it to `required`):

```yaml
    annotations:
      type: object
      additionalProperties: true
      description: >
        Arbitrary client-owned metadata for the whole workflow, stored and
        round-tripped (compacted) and never interpreted by the engine. Must be
        a JSON object; capped at 64 KB. Use for client concerns such as
        permitted roles, display labels, or UI hints.
```

- [ ] **Step 4: Verify the OpenAPI contract test and E2E conformance stay green**

Run: `go test ./internal/domain/workflow/ -run TestOpenAPIWorkflowVersionContract && go test ./internal/e2e/ -run TestWorkflowAnnotations`
Expected: PASS. (The annotated export now validates against a DTO that declares the property.)

- [ ] **Step 5: Commit**

```bash
git add api/openapi.yaml
git commit -m "docs(openapi): declare optional annotations on workflow DTOs"
```

---

### Task 7: Help content, schema-version note, CHANGELOG

**Files:**
- Modify: `cmd/cyoda/help/content/workflows.md` (field lists L102-125, export-omission note L366, an example block ~L45-99)
- Modify: `docs/workflow-schema-versioning.md` (`### 1.1` entry L60-74)
- Modify: `CHANGELOG.md` (`[Unreleased]` L5)

- [ ] **Step 1: Document the field on the three help lists**

In `cmd/cyoda/help/content/workflows.md`, add to **WorkflowDefinition fields** (after the `states` bullet):

```markdown
- `annotations` — object or absent — optional client-owned metadata, stored and round-tripped (compacted) but never interpreted by the engine. Must be a JSON object; capped at 64 KB per field. Use for client concerns such as permitted roles, display labels, or UI hints
```

Add to **StateDefinition** (after the `transitions` bullet):

```markdown
- `annotations` — object or absent — optional client-owned metadata (see WorkflowDefinition `annotations`); object-only, 64 KB cap, engine-opaque
```

Add to **TransitionDefinition fields** (after the `processors` bullet):

```markdown
- `annotations` — object or absent — optional client-owned metadata (see WorkflowDefinition `annotations`); object-only, 64 KB cap, engine-opaque
```

- [ ] **Step 2: Extend the export-omission note (L366)**

Append to the existing **Export field omission** paragraph:

```markdown
 `annotations` (on the workflow, any state, or any transition) is omitted when absent, and is re-serialised in compacted form when present.
```

- [ ] **Step 3: Add `annotations` to the example block**

In the workflow JSON EXAMPLE (~L45-99), add an `annotations` entry to the top-level workflow object and to the `APPROVE` transition, e.g. after `"active": true,`:

```json
  "annotations": { "roles": ["reviewer"], "label": "Prize lifecycle" },
```

and inside the `APPROVE` transition object after `"manual": true,`:

```json
          "annotations": { "ui": { "color": "green" } },
```

- [ ] **Step 4: Note annotations in the schema-version `1.1` changelog entry**

In `docs/workflow-schema-versioning.md`, append a bullet to the `### 1.1 — v0.8.0 contract (current)` list:

```markdown
- **Client annotations**. Optional `annotations` JSON-object field on workflows, states, and transitions — opaque client-owned metadata, stored and round-tripped (compacted), object-only, 64 KB per field. Additive within 1.1 (the field ships in the same unreleased contract); no version bump.
```

- [ ] **Step 5: Add the CHANGELOG entry**

In `CHANGELOG.md`, under `## [Unreleased]`, add:

```markdown
### Added

- Optional `annotations` JSON-object field on workflows, states, and transitions — arbitrary client-owned metadata, stored and round-tripped (compacted) but never interpreted by the engine. Object-only, capped at 64 KB per field.
```

- [ ] **Step 6: Verify help content tests / build**

Run: `go test ./cmd/cyoda/... -short && go build ./cmd/cyoda`
Expected: PASS (help content is embedded; build proves it compiles).

- [ ] **Step 7: Commit**

```bash
git add cmd/cyoda/help/content/workflows.md docs/workflow-schema-versioning.md CHANGELOG.md
git commit -m "docs(workflow): document annotations field (help, schema-version, changelog)"
```

---

### Task 8: Full local verification (against local SPI)

**Files:** none (verification only).

- [ ] **Step 1: Vet + full unit/E2E suite**

Run:
```bash
go vet ./...
go test ./... -v        # root incl. internal/e2e (Docker required)
```
Expected: PASS. (E2E spins up its own PostgreSQL.)

- [ ] **Step 2: Plugin submodules**

Run: `make test-short-all` (root + `plugins/memory|sqlite|postgres`; postgres needs Docker for full `make test-all`).
Expected: PASS. (Plugins compile against the local SPI via `go.work`; no plugin code changed.)

- [ ] **Step 3: Race sanity check (once, before PR)**

Run: `make race`
Expected: PASS. (Per `.claude/rules/race-testing.md`, this is the single end-of-deliverable race run.)

(No commit.)

---

### Task 9: Cross-repo finalisation — merge SPI, bump the pin, drop local `go.work`

This is the coordination step. The cyoda-go PR's CI resolves the SPI from the pinned pseudo-version (it has no `go.work`), so the SPI change MUST be on `cyoda-go-spi` `main` and the pin MUST point at it before cyoda-go CI can pass.

**Files:**
- Modify: root `go.mod`, `plugins/memory/go.mod`, `plugins/postgres/go.mod`, `plugins/sqlite/go.mod` (SPI pin)
- Revert (local-only): `go.work`

- [ ] **Step 1: Open and merge the SPI PR**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git push -u origin feat/workflow-annotations
gh pr create --fill --base main
# After review/CI:
gh pr merge --squash
```
Record the merged commit SHA on `main` (e.g. `git rev-parse origin/main` after fetch).

- [ ] **Step 2: Drop the local SPI `use` from `go.work`**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat+workflow-meta-field
go work edit -dropuse /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git diff --stat go.work   # should show NO change vs committed (entry was never committed)
```
Expected: `go.work` matches the committed version (the SPI `use` line is gone). The repo no longer composes against local SPI; it resolves the pin.

- [ ] **Step 3: Bump the SPI pin in all four manifests to the merged `main`**

```bash
export GOPRIVATE=github.com/cyoda-platform
go get github.com/cyoda-platform/cyoda-go-spi@<merged-sha>
(cd plugins/memory   && go get github.com/cyoda-platform/cyoda-go-spi@<merged-sha> && go mod tidy)
(cd plugins/postgres && go get github.com/cyoda-platform/cyoda-go-spi@<merged-sha> && go mod tidy)
(cd plugins/sqlite   && go get github.com/cyoda-platform/cyoda-go-spi@<merged-sha> && go mod tidy)
go mod tidy
```
(`@<merged-sha>` resolves to a `v0.7.2-0.<timestamp>-<sha>` pseudo-version; `@main` works too.)

- [ ] **Step 4: Verify pin sync**

Run: `make check-spi-pin-sync`
Expected: PASS — identical SPI version across root + 3 plugins.

- [ ] **Step 5: Re-run the suite against the pinned SPI**

Run: `go vet ./... && go test ./... && make test-short-all`
Expected: PASS (now using the pinned SPI, not `go.work`-local).

- [ ] **Step 6: Commit the pin bump**

```bash
git add go.mod go.sum plugins/memory/go.mod plugins/memory/go.sum plugins/postgres/go.mod plugins/postgres/go.sum plugins/sqlite/go.mod plugins/sqlite/go.sum
git commit -m "build: bump cyoda-go-spi pin for workflow annotations field"
```

- [ ] **Step 7: Push and open the cyoda-go PR (targeting `release/v0.8.0`)**

```bash
git push -u origin feat/workflow-meta-field
gh pr create --base release/v0.8.0 --fill
```
The PR body cites the SPI PR and the cyoda-go issue (issue IDs in the PR body only, never in code/artefacts). Milestone the PR per the release-milestone invariant.

---

## Self-Review

**Spec coverage:**
- Data model (SPI 3 fields) → Task 1. ✓
- Cross-repo coordination (SPI-first, pin bump, no tag) → Tasks 2, 9. ✓
- Validation (object-only, null→nil, compact, 64 KB, location in error) → Task 3. ✓
- Handler wiring (mirror struct, incoming literal, call placement, incoming-only) → Task 4. ✓
- Merge modes (annotations ride along; no special handling) → Task 4 (no extra code) + covered by REPLACE in E2E. ✓
- API surface (3 DTOs, never `required`, ModeEnforce response conformance) → Task 6. ✓
- Help content (3 lists + export-omission + example) → Task 7. ✓
- Schema versioning (no bump; doc + CHANGELOG) → Task 7. ✓
- Out-of-scope confirmations (audit digest unchanged, no env knob, storage unchanged, tenant isolation, deep-nesting accepted) → no code; honoured by not touching those paths. ✓
- Testing: SPI round-trip (Task 1), unit (Task 3), E2E round-trip+omission+rejection (Task 4), parity (Task 5), existing gates green (Tasks 6, 8). ✓

**Placeholder scan:** No `TBD`/`TODO`/"handle edge cases"; every code step shows real code. Two intentional conditionals are explicit: the `json` import in the E2E file (drop if unused) and the parity driver test-name `grep`. ✓

**Type consistency:** `canonicalizeAnnotations(json.RawMessage, string) (json.RawMessage, error)` and `validateAndNormalizeAnnotations([]spi.WorkflowDefinition) error` are used identically in Tasks 3–4. JSON tag `annotations` and Go field `Annotations` consistent across SPI, mirror struct, OpenAPI, help, tests. `maxAnnotationsBytes` defined once (Task 3) and referenced in the Task 3 size test. ✓

## Execution Notes

- Tasks 1–8 run entirely against the local SPI via `go.work` and can be completed before any push. Task 9 is the only step needing remote coordination (SPI merge → pin bump).
- If the `go.work` local `use` is ever accidentally staged, unstage it — it must never be committed.
