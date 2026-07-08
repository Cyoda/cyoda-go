# Renderer Annotations on Processors & Criteria — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend the engine-ignored `annotations` mechanism to workflow processors (embedded field) and criteria (`criterionAnnotations` sibling), with documented well-known keys `displayName`/`description` uniform across all five workflow element types.

**Architecture:** Additive fields on three SPI structs. Processors get an embedded `annotations` bag; criteria get a `criterionAnnotations` sibling next to the (verbatim, opaque) `criterion` on the workflow and transition structs. All new fields are canonicalised (object-only, ≤64 KB) by the existing import validation walk. Additive MINOR workflow-schema bump 1.1 → 1.2 (dual-shape, 1.1 retained). Non-breaking; annotations never reach external compute members.

**Tech Stack:** Go 1.26, `log/slog`, oapi-codegen v2.7.0 (embedded-spec:false), testcontainers-go (Postgres e2e), cross-repo SPI (`cyoda-go-spi`) composed locally via `go.work`.

**Spec:** `docs/superpowers/specs/2026-07-07-processor-criteria-annotations-design.md` — GitHub issue #384, milestone v0.8.2.

## Global Constraints

- **Go 1.26+.** Use `log/slog` only; never log annotation contents.
- **SPI change is additive, recompile-only.** The v0.8.2 SPI tag is deferred to milestone-end — do **not** cut an SPI release tag in this plan. Compose the SPI change locally via `go.work` (skip-worktree), and at the end pseudo-version-pin all four `go.mod` files to the pushed SPI commit. Never use committed `replace` directives.
- **SPI pin must be identical in all four manifests** (`go.mod`, `plugins/{memory,postgres,sqlite}/go.mod`); `make check-spi-pin-sync` enforces this.
- **Schema version: dual-shape.** `CurrentSchemaVersion = "1.2"`, `SupportedSchemaRanges = {Major:1, MinMinor:1, MaxMinor:2}`. **Import fixtures across the tree STAY `"1.1"`** — they remain valid and prove dual-shape retention. Only current/exported/discovered-version *assertions* move to 1.2 / `{1,1,2}`.
- **Well-known-key types are advisory** — the engine validates only object-shape + ≤64 KB, never value types.
- **No issue IDs (`#384`) in shipped artefacts** (code, comments, error messages, OpenAPI, help topics). Issue IDs are allowed only in commits, PR bodies, spec/plan docs.
- **Annotations validation error:** 400 `VALIDATION_FAILED` (existing code — no new error code, no new `errors/<CODE>.md`).
- **Canonicalisation helper** (reuse verbatim): `canonicalizeAnnotations(raw json.RawMessage, location string) (json.RawMessage, error)` in `internal/domain/workflow/validate.go` — object-only, ≤64 KB compacted, `null`/blank/absent → nil.
- **TDD throughout.** Each task: failing test → run-fails → implement → run-passes → commit. `make race` once before PR only, never per-step.

---

## Task 1: SPI field additions + local `go.work` composition

Adds the three additive fields to `cyoda-go-spi` and wires local composition so the rest of the plan builds against them. The SPI checkout is at `/Users/paul/go-projects/cyoda-light/cyoda-go-spi` (branch `main`).

**Files:**
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types.go` (`ProcessorDefinition`, `WorkflowDefinition`, `TransitionDefinition`)
- Create/Modify (SPI test): `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types_annotations_test.go`
- Modify (local only, skip-worktree): `go.work` (this repo)

**Interfaces:**
- Produces: `spi.ProcessorDefinition.Annotations json.RawMessage`, `spi.WorkflowDefinition.CriterionAnnotations json.RawMessage`, `spi.TransitionDefinition.CriterionAnnotations json.RawMessage` — all `json:"...,omitempty"`.

- [ ] **Step 1: Create an SPI feature branch**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git checkout -b feat/processor-criteria-annotations
```

- [ ] **Step 2: Write the failing SPI marshal round-trip test**

Create `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types_annotations_test.go`:

```go
package spi

import (
	"encoding/json"
	"testing"
)

func TestProcessorAndCriterionAnnotations_RoundTrip(t *testing.T) {
	in := WorkflowDefinition{
		Name:                 "wf",
		Version:              "1.2",
		InitialState:         "S",
		CriterionAnnotations: json.RawMessage(`{"displayName":"wf-guard"}`),
		States: map[string]StateDefinition{
			"S": {Transitions: []TransitionDefinition{{
				Name:                 "t",
				Next:                 "S",
				CriterionAnnotations: json.RawMessage(`{"displayName":"t-guard"}`),
				Processors: []ProcessorDefinition{{
					Name:        "p",
					Type:        "externalized",
					Annotations: json.RawMessage(`{"displayName":"proc"}`),
				}},
			}}},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out WorkflowDefinition
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(out.CriterionAnnotations) != `{"displayName":"wf-guard"}` {
		t.Errorf("wf criterionAnnotations lost: %s", out.CriterionAnnotations)
	}
	tr := out.States["S"].Transitions[0]
	if string(tr.CriterionAnnotations) != `{"displayName":"t-guard"}` {
		t.Errorf("transition criterionAnnotations lost: %s", tr.CriterionAnnotations)
	}
	if string(tr.Processors[0].Annotations) != `{"displayName":"proc"}` {
		t.Errorf("processor annotations lost: %s", tr.Processors[0].Annotations)
	}
}

func TestAnnotations_OmittedWhenAbsent(t *testing.T) {
	b, _ := json.Marshal(ProcessorDefinition{Name: "p", Type: "externalized"})
	if got := string(b); contains(got, "annotations") {
		t.Errorf("absent processor annotations should be omitted, got %s", got)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 3: Run the test to verify it fails to compile**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./... -run TestProcessorAndCriterionAnnotations -v`
Expected: FAIL — `in.CriterionAnnotations undefined`, `Processors[0].Annotations undefined`.

- [ ] **Step 4: Add the three fields to `types.go`**

In `ProcessorDefinition` (after the `Config` field):

```go
	Config        ProcessorConfig `json:"config,omitempty"`
	// Annotations is arbitrary client-owned metadata, stored and
	// round-tripped verbatim and never interpreted by the engine.
	// Well-known renderer keys (displayName, description) are a documented
	// convention only — the engine validates object-shape and size, not
	// the value types.
	Annotations json.RawMessage `json:"annotations,omitempty"`
```

In `WorkflowDefinition` (after the `Annotations` field):

```go
	Annotations json.RawMessage `json:"annotations,omitempty"`
	// CriterionAnnotations is client-owned metadata describing the
	// workflow-selection Criterion as a whole (a sibling to Criterion,
	// because the criterion value is opaque and round-trips verbatim).
	// Same bag shape as Annotations; engine-ignored.
	CriterionAnnotations json.RawMessage `json:"criterionAnnotations,omitempty"`
```

In `TransitionDefinition` (after the `Annotations` field):

```go
	Annotations json.RawMessage `json:"annotations,omitempty"`
	// CriterionAnnotations is client-owned metadata describing this
	// transition's guard Criterion as a whole. Sibling to Criterion;
	// engine-ignored. See WorkflowDefinition.CriterionAnnotations.
	CriterionAnnotations json.RawMessage `json:"criterionAnnotations,omitempty"`
```

- [ ] **Step 5: Run SPI tests to verify pass**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./... -v`
Expected: PASS (all, including existing SPI tests).

- [ ] **Step 6: Commit the SPI change (local branch)**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add types.go types_annotations_test.go
git commit -m "feat(types): annotations on ProcessorDefinition; criterionAnnotations on Workflow/Transition

Additive fields for renderer metadata (displayName/description well-known
keys). Engine-ignored; consumers validate object-shape + size only."
```

- [ ] **Step 7: Compose locally via `go.work` (skip-worktree, not committed)**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat+processor-criteria-annotations
go work edit -use /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git update-index --skip-worktree go.work go.work.sum
```

- [ ] **Step 8: Verify cyoda-go composes against the local SPI**

Run: `go build ./... && go vet ./internal/domain/workflow/...`
Expected: builds clean; the new SPI fields are visible to this module.

---

## Task 2: Extend the annotations validation walk

Canonicalise processor `Annotations`, workflow `CriterionAnnotations`, and transition `CriterionAnnotations` in the existing import walk. Unit-tested directly against `[]spi.WorkflowDefinition`.

**Files:**
- Modify: `internal/domain/workflow/validate.go` (`validateAndNormalizeAnnotations`)
- Test: `internal/domain/workflow/validate_test.go`

**Interfaces:**
- Consumes: `canonicalizeAnnotations` (unchanged), the three SPI fields from Task 1.
- Produces: extended `validateAndNormalizeAnnotations` behaviour (processors + criterionAnnotations canonicalised, non-object/oversize rejected).

- [ ] **Step 1: Write the failing unit tests**

Append to `internal/domain/workflow/validate_test.go`:

```go
func wfWithNewAnnotations(procA, wfCritA, trCritA json.RawMessage) []spi.WorkflowDefinition {
	return []spi.WorkflowDefinition{{
		Name:                 "wf",
		Version:              "1.2",
		InitialState:         "S",
		Active:               true,
		CriterionAnnotations: wfCritA,
		States: map[string]spi.StateDefinition{
			"S": {
				Transitions: []spi.TransitionDefinition{{
					Name:                 "t",
					Next:                 "S",
					CriterionAnnotations: trCritA,
					Processors: []spi.ProcessorDefinition{
						{Name: "p", Type: "externalized", Annotations: procA},
					},
				}},
			},
		},
	}}
}

func TestValidateAndNormalizeAnnotations_ProcessorAndCriterionCompacted(t *testing.T) {
	wfs := wfWithNewAnnotations(
		json.RawMessage(`{ "displayName" : "Proc" }`),
		json.RawMessage("{\n  \"displayName\": \"WF guard\"\n}"),
		json.RawMessage(`{"description":"t guard"}`),
	)
	if err := validateAndNormalizeAnnotations(wfs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(wfs[0].CriterionAnnotations); got != `{"displayName":"WF guard"}` {
		t.Errorf("wf criterionAnnotations not compacted: %s", got)
	}
	tr := wfs[0].States["S"].Transitions[0]
	if got := string(tr.CriterionAnnotations); got != `{"description":"t guard"}` {
		t.Errorf("transition criterionAnnotations not compacted: %s", got)
	}
	if got := string(tr.Processors[0].Annotations); got != `{"displayName":"Proc"}` {
		t.Errorf("processor annotations not compacted: %s", got)
	}
}

func TestValidateAndNormalizeAnnotations_ProcessorNonObjectRejected(t *testing.T) {
	err := validateAndNormalizeAnnotations(wfWithNewAnnotations(json.RawMessage(`[1,2]`), nil, nil))
	if err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
		t.Fatalf("expected object error, got %v", err)
	}
	if !strings.Contains(err.Error(), "processor") {
		t.Errorf("error should locate the processor: %v", err)
	}
}

func TestValidateAndNormalizeAnnotations_TransitionCriterionNonObjectRejected(t *testing.T) {
	err := validateAndNormalizeAnnotations(wfWithNewAnnotations(nil, nil, json.RawMessage(`"x"`)))
	if err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
		t.Fatalf("expected object error, got %v", err)
	}
	if !strings.Contains(err.Error(), "criterionAnnotations") {
		t.Errorf("error should name criterionAnnotations: %v", err)
	}
}

func TestValidateAndNormalizeAnnotations_ProcessorOversizeRejected(t *testing.T) {
	big := json.RawMessage(`{"displayName":"` + strings.Repeat("a", 64*1024) + `"}`)
	err := validateAndNormalizeAnnotations(wfWithNewAnnotations(big, nil, nil))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size error, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/domain/workflow/ -run TestValidateAndNormalizeAnnotations_Processor -v`
Expected: FAIL — new fields not yet canonicalised (compaction assertions fail; rejection tests get nil error).

- [ ] **Step 3: Extend `validateAndNormalizeAnnotations`**

Replace the body of `validateAndNormalizeAnnotations` in `internal/domain/workflow/validate.go` with:

```go
func validateAndNormalizeAnnotations(workflows []spi.WorkflowDefinition) error {
	for i := range workflows {
		wf := &workflows[i]
		canon, err := canonicalizeAnnotations(wf.Annotations, fmt.Sprintf("workflow %q", wf.Name))
		if err != nil {
			return err
		}
		wf.Annotations = canon
		// Criterion annotations sit beside the (verbatim, opaque) criterion.
		// The criterion blob itself is never parsed here.
		wfCrit, err := canonicalizeAnnotations(wf.CriterionAnnotations,
			fmt.Sprintf("workflow %q criterionAnnotations", wf.Name))
		if err != nil {
			return err
		}
		wf.CriterionAnnotations = wfCrit
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
				trCrit, err := canonicalizeAnnotations(tr.CriterionAnnotations,
					fmt.Sprintf("workflow %q state %q transition %q criterionAnnotations", wf.Name, stateName, tr.Name))
				if err != nil {
					return err
				}
				tr.CriterionAnnotations = trCrit
				// Processors must be mutated by index — a range-value copy
				// would discard the canonicalised blob.
				for k := range tr.Processors {
					p := &tr.Processors[k]
					pCanon, err := canonicalizeAnnotations(p.Annotations,
						fmt.Sprintf("workflow %q state %q transition %q processor %q", wf.Name, stateName, tr.Name, p.Name))
					if err != nil {
						return err
					}
					p.Annotations = pCanon
				}
			}
			// Map values are not addressable — write the state back so its
			// canonicalised Annotations persist. Transitions/processors mutate
			// through the shared slice backing array and need no write-back.
			wf.States[stateName] = stateDef
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./internal/domain/workflow/ -run TestValidateAndNormalizeAnnotations -v`
Expected: PASS (new tests + all pre-existing annotations tests).

- [ ] **Step 5: Commit**

```bash
git add internal/domain/workflow/validate.go internal/domain/workflow/validate_test.go
git commit -m "feat(workflow): canonicalise processor + criterion annotations at import"
```

---

## Task 3: Wire workflow-level `criterionAnnotations` through the import request mirror

Transition-level `criterionAnnotations` and processor `annotations` decode automatically through the embedded `spi.StateDefinition`. Only the workflow-level field needs adding to the hand-written `workflowImportDef` mirror + its conversion.

**Files:**
- Modify: `internal/domain/workflow/handler.go` (`workflowImportDef` struct + conversion loop)
- Test: `internal/domain/workflow/handler_annotations_test.go` (new) — or fold into e2e Task 5. This task uses a focused unit test on the decode+convert path if one is reachable; otherwise its behaviour is proven by the e2e round-trip in Task 5. **Prefer the e2e proof** (the mirror is only exercised through the HTTP handler), so this task is a minimal code change verified by Task 5's `criterionAnnotations` round-trip.

**Interfaces:**
- Consumes: `spi.WorkflowDefinition.CriterionAnnotations` (Task 1).
- Produces: workflow-level `criterionAnnotations` accepted by the strict import decoder and carried into the SPI slice.

- [ ] **Step 1: Add the field to `workflowImportDef`**

In `internal/domain/workflow/handler.go`, in `workflowImportDef` (after the `Annotations` field):

```go
	Annotations  json.RawMessage                `json:"annotations,omitempty"`
	CriterionAnnotations json.RawMessage         `json:"criterionAnnotations,omitempty"`
```

- [ ] **Step 2: Copy it in the request→SPI conversion**

In the `incoming[i] = spi.WorkflowDefinition{...}` literal (after `Annotations: w.Annotations,`):

```go
			Annotations:          w.Annotations,
			CriterionAnnotations: w.CriterionAnnotations,
```

- [ ] **Step 3: Verify it builds**

Run: `go build ./... && go vet ./internal/domain/workflow/...`
Expected: clean. (Behavioural verification lands in Task 5's e2e round-trip.)

- [ ] **Step 4: Commit**

```bash
git add internal/domain/workflow/handler.go
git commit -m "feat(workflow): accept workflow-level criterionAnnotations on import"
```

---

## Task 4: Schema-version bump 1.1 → 1.2 (dual-shape)

Bump the constant + range, bump the embedded default fixture, and correct the current/exported/discovered-version assertions. **Do not touch import fixtures** — they stay `"1.1"`.

**Files:**
- Modify: `internal/domain/workflow/schemaversion.go`
- Modify: `internal/domain/workflow/default_workflow.json`
- Modify: `internal/e2e/workflow_schema_version_test.go` (export-stamp + discovery assertions; add a 1.2-accepted test)

**Interfaces:**
- Produces: `CurrentSchemaVersion == "1.2"`; supported window `{1,1,2}`.

- [ ] **Step 1: Update the constant and range**

In `internal/domain/workflow/schemaversion.go`:

```go
const CurrentSchemaVersion = "1.2"
```

and in `SupportedSchemaRanges`:

```go
	{Major: 1, MinMinor: 1, MaxMinor: 2},
```

- [ ] **Step 2: Run the symbolic + drift-guard tests to see the failures**

Run: `go test ./internal/domain/workflow/ -run 'SchemaVersion|DefaultWorkflow' -v`
Expected: FAIL — `TestDefaultWorkflowFixtureSchemaVersion` (`default_workflow.json` still `"1.1"`).

- [ ] **Step 3: Bump the embedded default fixture**

In `internal/domain/workflow/default_workflow.json`, line 2:

```json
  "version": "1.2",
```

- [ ] **Step 4: Re-run domain tests to confirm green**

Run: `go test ./internal/domain/workflow/ -run 'SchemaVersion|DefaultWorkflow' -v`
Expected: PASS.

- [ ] **Step 5: Fix the e2e export-stamp + discovery assertions**

In `internal/e2e/workflow_schema_version_test.go`:

- Export-stamp assertion (~line 169) — the exported version is now the current one:

```go
		if m["version"] != "1.2" {
			t.Fatalf("workflow[%d] version = %v; want \"1.2\"", i, m["version"])
		}
```

- Discovery assertion (~lines 192, 199-200):

```go
	if got.Current != "1.2" {
		t.Fatalf("current = %q; want 1.2", got.Current)
	}
	if len(got.Supported) != 1 {
		t.Fatalf("supported length = %d; want 1; got %+v", len(got.Supported), got.Supported)
	}
	s := got.Supported[0]
	if s["major"] != 1 || s["minMinor"] != 1 || s["maxMinor"] != 2 {
		t.Fatalf("supported[0] = %+v; want {major:1, minMinor:1, maxMinor:2}", s)
	}
```

- Refresh the stale header comment near the top of the file (`v0.8.0 ships 1.1` → note 1.2 is current, 1.1 retained).

- [ ] **Step 6: Add an explicit "1.2 accepted on import" test**

Append to `internal/e2e/workflow_schema_version_test.go`:

```go
// TestWorkflowSchemaVersion_ImportAccepts12 proves the new current MINOR is
// accepted. The pre-existing 1.1-import test stays as the dual-shape proof.
func TestWorkflowSchemaVersion_ImportAccepts12(t *testing.T) {
	const entity, version = "schemaver-12", 1
	importModelE2E(t, entity, version)
	payload := `{
	  "importMode": "REPLACE",
	  "workflows": [{
	    "version": "1.2", "name": "v12-wf", "initialState": "S", "active": true,
	    "states": { "S": { "transitions": [ { "name": "t", "next": "S", "manual": true } ] } }
	  }]
	}`
	if status, body := importWorkflowE2E(t, entity, version, payload); status != http.StatusOK {
		t.Fatalf("import 1.2: expected 200, got %d: %s", status, body)
	}
}
```

- [ ] **Step 7: Run the e2e schema-version suite (Docker required)**

Run: `go test ./internal/e2e/ -run TestWorkflowSchemaVersion -v`
Expected: PASS (export-stamp, discovery, 1.1-still-accepted, 1.2-accepted).

- [ ] **Step 8: Commit**

```bash
git add internal/domain/workflow/schemaversion.go internal/domain/workflow/default_workflow.json internal/e2e/workflow_schema_version_test.go
git commit -m "feat(workflow): bump schema version 1.1 -> 1.2 (dual-shape, retain 1.1)"
```

---

## Task 5: E2E coverage — processor `annotations` + `criterionAnnotations`

Extend the existing annotations e2e suite to cover the new fields through the full HTTP stack (real Postgres).

**Files:**
- Modify: `internal/e2e/workflow_annotations_test.go`

**Interfaces:**
- Consumes: Tasks 1-4. Helpers `importModelE2E`, `importWorkflowE2E`, `exportWorkflowE2E`, `firstWorkflow` (existing in the e2e package).

- [ ] **Step 1: Write the failing e2e tests**

Append to `internal/e2e/workflow_annotations_test.go`:

```go
const procCritWorkflowPayload = `{
  "importMode": "REPLACE",
  "workflows": [{
    "version": "1.2",
    "name": "pc-wf",
    "initialState": "S",
    "active": true,
    "criterionAnnotations": { "displayName": "WF guard" },
    "states": {
      "S": {
        "transitions": [
          {
            "name": "t", "next": "S", "manual": true,
            "criterionAnnotations": { "displayName": "T guard", "description": "checks status" },
            "processors": [
              { "name": "p1", "type": "externalized", "annotations": { "displayName": "Proc One" } }
            ]
          }
        ]
      }
    }
  }]
}`

func TestWorkflowAnnotations_ProcessorAndCriterionRoundTrip(t *testing.T) {
	const entity, version = "annot-pc-rt", 1
	importModelE2E(t, entity, version)
	if status, body := importWorkflowE2E(t, entity, version, procCritWorkflowPayload); status != http.StatusOK {
		t.Fatalf("import: expected 200, got %d: %s", status, body)
	}
	_, body := exportWorkflowE2E(t, entity, version)
	wf := firstWorkflow(t, body)
	if got, want := wf["criterionAnnotations"], map[string]any{"displayName": "WF guard"}; !reflect.DeepEqual(got, want) {
		t.Errorf("wf criterionAnnotations: got %#v, want %#v", got, want)
	}
	state := wf["states"].(map[string]any)["S"].(map[string]any)
	tr := state["transitions"].([]any)[0].(map[string]any)
	if got, want := tr["criterionAnnotations"], map[string]any{"displayName": "T guard", "description": "checks status"}; !reflect.DeepEqual(got, want) {
		t.Errorf("transition criterionAnnotations: got %#v, want %#v", got, want)
	}
	proc := tr["processors"].([]any)[0].(map[string]any)
	if got, want := proc["annotations"], map[string]any{"displayName": "Proc One"}; !reflect.DeepEqual(got, want) {
		t.Errorf("processor annotations: got %#v, want %#v", got, want)
	}
}

func TestWorkflowAnnotations_ProcessorNonObjectRejected(t *testing.T) {
	const entity, version = "annot-pc-bad", 1
	importModelE2E(t, entity, version)
	payload := strings.Replace(procCritWorkflowPayload,
		`"annotations": { "displayName": "Proc One" }`, `"annotations": 5`, 1)
	status, body := importWorkflowE2E(t, entity, version, payload)
	if status != http.StatusBadRequest || !strings.Contains(body, "VALIDATION_FAILED") {
		t.Fatalf("expected 400 VALIDATION_FAILED, got %d: %s", status, body)
	}
}

func TestWorkflowAnnotations_CriterionAnnotationsTypoRejected(t *testing.T) {
	const entity, version = "annot-pc-typo", 1
	importModelE2E(t, entity, version)
	payload := strings.Replace(procCritWorkflowPayload, `"criterionAnnotations": { "displayName": "WF guard" }`,
		`"criterionAnnotationss": { "displayName": "WF guard" }`, 1)
	status, body := importWorkflowE2E(t, entity, version, payload)
	if status != http.StatusBadRequest || !strings.Contains(body, "BAD_REQUEST") {
		t.Fatalf("expected 400 BAD_REQUEST for unknown field, got %d: %s", status, body)
	}
}
```

- [ ] **Step 2: Run to verify they fail (before Tasks 1-4 fully wired) or pass (after)**

Run: `go test ./internal/e2e/ -run 'TestWorkflowAnnotations_(ProcessorAndCriterionRoundTrip|ProcessorNonObjectRejected|CriterionAnnotationsTypoRejected)' -v`
Expected after Tasks 1-4: PASS. If the round-trip fails on a missing key, confirm Task 3's mirror field and Task 2's walk landed.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/workflow_annotations_test.go
git commit -m "test(e2e): processor + criterion annotations round-trip, reject, typo"
```

---

## Task 6: Cross-backend parity scenario

Add a parity scenario asserting processor `annotations` + `criterionAnnotations` round-trip on every backend (memory/sqlite/postgres + commercial).

**Files:**
- Modify: `e2e/parity/workflow.go` (new payload + `RunWorkflowProcCriterionAnnotationsRoundTrip`)
- Modify: `e2e/parity/registry.go` (register + bump count comment)

**Interfaces:**
- Consumes: `BackendFixture`, `client.NewClient`, `c.ImportModel/LockModel/ImportWorkflow/ExportWorkflow` (existing).
- Produces: `RunWorkflowProcCriterionAnnotationsRoundTrip(t *testing.T, fixture BackendFixture)`.

- [ ] **Step 1: Add the parity scenario**

Append to `e2e/parity/workflow.go`:

```go
const workflowProcCriterionAnnotationsPayload = `{
  "importMode": "REPLACE",
  "workflows": [{
    "version": "1.2", "name": "pc-annot-wf", "initialState": "NONE", "active": true,
    "criterionAnnotations": { "displayName": "WF guard" },
    "states": { "NONE": { "transitions": [
      { "name": "t", "next": "NONE", "manual": true,
        "criterionAnnotations": { "displayName": "T guard" },
        "processors": [ { "name": "p1", "type": "externalized", "annotations": { "displayName": "Proc One" } } ]
      }
    ] } }
  }]
}`

// RunWorkflowProcCriterionAnnotationsRoundTrip verifies processor annotations
// and criterionAnnotations survive import → export on every backend.
func RunWorkflowProcCriterionAnnotationsRoundTrip(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "wf-pc-annotations-test"
	const modelVersion = 1

	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"Test Order","status":"draft"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, workflowProcCriterionAnnotationsPayload); err != nil {
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
	if got, want := wf["criterionAnnotations"], map[string]any{"displayName": "WF guard"}; !reflect.DeepEqual(got, want) {
		t.Errorf("wf criterionAnnotations: got %#v, want %#v", got, want)
	}
	tr := wf["states"].(map[string]any)["NONE"].(map[string]any)["transitions"].([]any)[0].(map[string]any)
	if got, want := tr["criterionAnnotations"], map[string]any{"displayName": "T guard"}; !reflect.DeepEqual(got, want) {
		t.Errorf("transition criterionAnnotations: got %#v, want %#v", got, want)
	}
	proc := tr["processors"].([]any)[0].(map[string]any)
	if got, want := proc["annotations"], map[string]any{"displayName": "Proc One"}; !reflect.DeepEqual(got, want) {
		t.Errorf("processor annotations: got %#v, want %#v", got, want)
	}
}
```

- [ ] **Step 2: Register the scenario**

In `e2e/parity/registry.go`, next to the existing `{"WorkflowAnnotationsRoundTrip", RunWorkflowAnnotationsRoundTrip},`:

```go
	{"WorkflowProcCriterionAnnotationsRoundTrip", RunWorkflowProcCriterionAnnotationsRoundTrip},
```

Bump the `// Total parity scenarios: 168` header comment to `169`.

- [ ] **Step 3: Run the parity scenario on the memory + postgres backends**

Run: `go test ./e2e/parity/... -run 'Parity/WorkflowProcCriterionAnnotationsRoundTrip' -v`
Expected: PASS on each registered backend fixture.

- [ ] **Step 4: Commit**

```bash
git add e2e/parity/workflow.go e2e/parity/registry.go
git commit -m "test(parity): processor + criterion annotations round-trip across backends"
```

---

## Task 7: gRPC dispatch — annotations never reach compute members

Assert the external `EntityProcessorCalculationRequestJson` sent to a compute member carries no annotation content, and an annotated processor still dispatches.

**Files:**
- Modify: `internal/grpc/dispatch_test.go`

**Interfaces:**
- Consumes: `setupTestDispatcher`, `testContext`, `testEntity`, `ParseCloudEvent` (existing in `internal/grpc`).

- [ ] **Step 1: Write the failing (then passing) test**

Append to `internal/grpc/dispatch_test.go`:

```go
func TestDispatchProcessor_AnnotationsNotSentToMember(t *testing.T) {
	dispatcher, _, _, sentCh := setupTestDispatcher(t)
	ctx := testContext()
	entity := testEntity()

	processor := spi.ProcessorDefinition{
		Name:        "my-proc",
		Type:        "externalized",
		Annotations: json.RawMessage(`{"displayName":"SECRET-LABEL"}`),
		Config: spi.ProcessorConfig{
			AttachEntity:         true,
			CalculationNodesTags: "python",
			ResponseTimeoutMs:    5000,
		},
	}

	go func() {
		ce := <-sentCh
		_, payload, err := ParseCloudEvent(ce)
		if err != nil {
			t.Errorf("ParseCloudEvent: %v", err)
			return
		}
		if strings.Contains(string(payload), "SECRET-LABEL") || strings.Contains(string(payload), "annotations") {
			t.Errorf("processor annotations leaked to compute member: %s", payload)
		}
	}()

	// Dispatch should succeed (no error path) despite the processor carrying annotations.
	_, err := dispatcher.DispatchProcessor(ctx, entity, processor, "wf", "t", "tx-1")
	if err != nil {
		t.Fatalf("DispatchProcessor: %v", err)
	}
}
```

Add `"strings"` to the test file's imports if not already present.

- [ ] **Step 2: Run to verify pass**

Run: `go test ./internal/grpc/ -run TestDispatchProcessor_AnnotationsNotSentToMember -v`
Expected: PASS — the external request struct has no annotations field, so the payload cannot contain them.

- [ ] **Step 3: Commit**

```bash
git add internal/grpc/dispatch_test.go
git commit -m "test(grpc): assert processor annotations never reach compute members"
```

---

## Task 8: OpenAPI — shared annotations schema across all five elements

Introduce one typed-but-open `WorkflowElementAnnotations` schema and reference it from all five occurrences; add `annotations` to `ProcessorDefinitionDto` and `criterionAnnotations` to `WorkflowConfigurationDto` + `TransitionDefinitionDto`. Regenerate.

**Files:**
- Modify: `api/openapi.yaml`
- Regenerate: `api/generated.go` (via `go generate`)

**Interfaces:**
- Produces: OpenAPI documents the new fields; generated DTOs gain them.

- [ ] **Step 1: Add the shared schema**

In `api/openapi.yaml` under `components: schemas:`, add:

```yaml
    WorkflowElementAnnotations:
      type: object
      additionalProperties: true
      description: >
        Client-owned metadata attached to a workflow element, stored and
        round-tripped verbatim and never interpreted by the engine. The engine
        validates only that the value is a JSON object of at most 64 KB
        (compacted); it does not enforce the types of the well-known keys below,
        which are a documented convention for renderers. Additional client keys
        are permitted.
      properties:
        displayName:
          type: string
          description: Advisory human-readable label for renderers (not enforced).
        description:
          type: string
          description: Advisory human-readable description for renderers (not enforced).
```

- [ ] **Step 2: Point the three existing annotations fields at the shared schema**

Replace each existing inline `annotations` schema on `StateDefinitionDto`, `TransitionDefinitionDto`, and `WorkflowConfigurationDto` (currently `type: object, additionalProperties: true`) with:

```yaml
        annotations:
          $ref: "#/components/schemas/WorkflowElementAnnotations"
```

- [ ] **Step 3: Add `annotations` to `ProcessorDefinitionDto`**

In `ProcessorDefinitionDto.properties` (alongside `name`/`type`):

```yaml
        annotations:
          $ref: "#/components/schemas/WorkflowElementAnnotations"
```

(`ExternalizedProcessorDefinitionDto` inherits it via its existing `allOf: [$ref ProcessorDefinitionDto]`.)

- [ ] **Step 4: Add `criterionAnnotations` to `WorkflowConfigurationDto` and `TransitionDefinitionDto`**

In each schema's `properties` (next to `criterion`):

```yaml
        criterionAnnotations:
          $ref: "#/components/schemas/WorkflowElementAnnotations"
```

- [ ] **Step 5: Bump the `WorkflowConfigurationDto.version` example if present**

If `WorkflowConfigurationDto.properties.version` names an `example:` or default of `"1.1"`, update it to `"1.2"`.

- [ ] **Step 6: Regenerate and verify sync**

Run:
```bash
cd api && go generate ./... && cd ..
make check-codegen
```
Expected: `api/generated.go` regenerates; `check-codegen` reports in sync. New generated types: `WorkflowElementAnnotations`; the three existing annotations fields change from `*map[string]interface{}` to `*WorkflowElementAnnotations`; `ProcessorDefinitionDto.Annotations` and the two `CriterionAnnotations` fields appear.

- [ ] **Step 7: Build + vet (catch any consumer of the changed generated fields)**

Run: `go build ./... && go vet ./...`
Expected: clean. If a compile error surfaces a runtime consumer of the old `*map[string]interface{}` annotations field, stop and reconcile (the spec expects none — handlers parse via `workflowImportDef`/spi types).

- [ ] **Step 8: Commit**

```bash
git add api/openapi.yaml api/generated.go
git commit -m "feat(openapi): shared WorkflowElementAnnotations; annotations on processor + criterion"
```

---

## Task 9: Documentation (Gate 4 + Gate 7)

**Files:**
- Modify: `docs/workflow-schema-versioning.md`
- Modify: `CHANGELOG.md`
- Modify: `cmd/cyoda/help/content/workflows/schema-version.md`
- Modify: `cmd/cyoda/help/content/workflows.md`
- Modify: `COMPATIBILITY.md`
- Create: `docs/cloud-parity/processor-criteria-annotations.md`

- [ ] **Step 1: Schema-versioning changelog entry**

In `docs/workflow-schema-versioning.md`, add a `1.1 → 1.2` entry to the per-version changelog: additive MINOR; new optional fields `annotations` (processor) and `criterionAnnotations` (workflow + transition); **dual-shape retention** of 1.1 (rationale: purely additive, every 1.1 payload stays valid — no retirement).

- [ ] **Step 2: CHANGELOG**

Under `[Unreleased]` / the v0.8.2 section, add:

```markdown
### Added
- Workflow **processors** now accept an engine-ignored `annotations` object, and **criteria** an engine-ignored `criterionAnnotations` sibling (workflow-selection and transition guards). Well-known renderer keys `displayName`/`description` are documented across all five workflow element types (object-only, ≤64 KB, types advisory). Workflow schema version bumps **1.1 → 1.2** (additive; 1.1 still accepted).
```

- [ ] **Step 3: Help topics**

- `cmd/cyoda/help/content/workflows/schema-version.md`: state current `1.2`, supported `{1.1, 1.2}`.
- `cmd/cyoda/help/content/workflows.md`: document that all five element types carry an open `annotations` bag (criteria via `criterionAnnotations`) with well-known optional keys `displayName`/`description`; engine-ignored. Keep it compact.

- [ ] **Step 4: COMPATIBILITY.md**

Add/adjust the SPI-pin note for the in-progress v0.8.2 line to reflect the new SPI pseudo-version (filled in by Task 10) and a one-line summary: "SPI: +ProcessorDefinition.Annotations, +Workflow/Transition.CriterionAnnotations."

- [ ] **Step 5: Cloud-parity doc**

Create `docs/cloud-parity/processor-criteria-annotations.md`: cyoda-go defines the import/export shape (`annotations` on processors, `criterionAnnotations` on workflow/transition) and the 1.2 schema version; Cloud mirrors the wire shape and dual-shape acceptance. Note the fields are engine-ignored metadata.

- [ ] **Step 6: Commit**

```bash
git add docs/workflow-schema-versioning.md CHANGELOG.md cmd/cyoda/help/content/workflows/schema-version.md cmd/cyoda/help/content/workflows.md COMPATIBILITY.md docs/cloud-parity/processor-criteria-annotations.md
git commit -m "docs: schema 1.2, annotations well-known keys, cloud-parity, compatibility"
```

---

## Task 10: SPI pin bump + final verification

Push the SPI change, pseudo-version-pin all four manifests to it, restore `go.work`, and run the full verification battery.

**Files:**
- Modify: `go.mod`, `plugins/memory/go.mod`, `plugins/postgres/go.mod`, `plugins/sqlite/go.mod`
- Restore: `go.work`, `go.work.sum`

- [ ] **Step 1: Push the SPI branch**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git push -u origin feat/processor-criteria-annotations
SPI_SHA=$(git rev-parse HEAD)
echo "SPI commit: $SPI_SHA"
```

(The v0.8.2 SPI **tag** is deferred to milestone-end — do not tag here.)

- [ ] **Step 2: Pin all four manifests to the SPI commit (pseudo-version)**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat+processor-criteria-annotations
for m in . plugins/memory plugins/postgres plugins/sqlite; do
  (cd "$m" && go get github.com/cyoda-platform/cyoda-go-spi@feat/processor-criteria-annotations)
done
```

GOPRIVATE resolves the private repo directly; this writes an identical pseudo-version (`v0.8.2-0.<ts>-<sha12>`) into each `go.mod`.

- [ ] **Step 3: Remove the local go.work SPI composition**

```bash
go work edit -dropuse /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git update-index --no-skip-worktree go.work go.work.sum
git checkout go.work go.work.sum   # restore the committed root+plugins composition
```

- [ ] **Step 4: Tidy + pin-sync + full test battery**

```bash
for m in . plugins/memory plugins/postgres plugins/sqlite; do (cd "$m" && go mod tidy); done
make check-spi-pin-sync
make test-all
go vet ./...
```
Expected: pin-sync green (all four agree); `make test-all` green (root + all plugins, Postgres via testcontainers); vet clean.

- [ ] **Step 5: Update COMPATIBILITY.md with the resolved pin**

Fill the SPI pseudo-version placeholder from Step 2 into the COMPATIBILITY.md note added in Task 9.

- [ ] **Step 6: Race sanity check (once, end-of-deliverable)**

Run: `make race`
Expected: green. (E2E excluded from `make race` per repo policy.)

- [ ] **Step 7: Commit the pin bump**

```bash
git add go.mod go.sum plugins/*/go.mod plugins/*/go.sum COMPATIBILITY.md
git commit -m "chore: pin cyoda-go-spi to processor/criteria-annotations commit (v0.8.2 WIP)"
```

---

## Self-Review

**Spec coverage** (each spec section → task):
- §1 uniform bag / two placements → Task 1 (SPI fields) + Task 8 (OpenAPI shared schema).
- §2 validation walk (addressability) → Task 2.
- §3 import/export wire (mirror field, omitempty) → Task 3 + Task 5 round-trip.
- §4 dispatch (external vs peer-forward) → Task 7 (external invariant asserted; peer-forward acceptance documented, not asserted).
- §5 schema bump + default_workflow.json → Task 4.
- §6 SPI change → Task 1 (change) + Task 10 (pin).
- Error/status table → Task 5 (non-object 400 VALIDATION_FAILED, typo 400 BAD_REQUEST) + Task 4 (version codes).
- Coverage matrix → unit (Task 2), e2e (Tasks 4-5), parity (Task 6), gRPC (Task 7).
- Docs (Gate 4 + 7) → Task 9.

**Placeholder scan:** none — every code/YAML/command step carries concrete content.

**Type consistency:** field names identical across tasks — `Annotations` (processor), `CriterionAnnotations` (workflow + transition), `criterionAnnotations` (JSON/YAML), `WorkflowElementAnnotations` (OpenAPI schema). `canonicalizeAnnotations` signature reused unchanged. Schema literals: constant `"1.2"`, range `{1,1,2}`, import fixtures stay `"1.1"`.

**Coverage-matrix gap check (per `.claude/rules/test-coverage.md`):** every import error/status cell has a running-backend e2e (Tasks 4-5); backend-agnostic round-trip has a parity scenario (Task 6); gRPC entry point covered (Task 7); import/export are HTTP-only (no gRPC import). No new error code → no `errors/<CODE>.md` needed. No concurrency scenario required (stateless validation, no new shared-state path).
