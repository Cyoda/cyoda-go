# Workflow Schema Version Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `WorkflowConfigurationDto.version` strict, discoverable semver `MAJOR.MINOR` semantics — validated on import, stamped on export, with a Go-side single source of truth (`CurrentSchemaVersion` + `SupportedSchemaRanges`) consumed by validator, exporter, OpenAPI consistency test, and a `versions` help-topic action mirrored to HTTP.

**Architecture:** New `internal/domain/workflow/schemaversion.go` owns the constants, parser, and `Supports()` check. Import handler validates each workflow's `Version` before mutating anything; export handler overwrites `Version` with `CurrentSchemaVersion` on every emitted workflow. A new `workflows.schema-version` help topic carries a `versions` action emitting `{"current":"1.0","supported":[{"major":1,"minMinor":0,"maxMinor":0}]}`. `internal/api/help.go`'s `RegisterHelpRoutes` is extended to dispatch any registered action over HTTP with a declared per-action `Content-Type`, closing a gap so this and every existing action (`grpc proto`, `openapi json`, etc.) become reachable via `GET /help/.../<action>`.

**Tech Stack:** Go 1.26, `log/slog`, `encoding/json`, `gopkg.in/yaml.v3` (consistency test), `net/http/httptest` (HTTP test scaffolding), existing `internal/e2e/` harness (PostgreSQL via testcontainers-go + in-process server). No new dependencies.

---

## File Map

**Create:**

- `internal/domain/workflow/schemaversion.go` — constants, `SchemaRange`, `ParseSchemaVersion`, `Supports`, sentinel errors
- `internal/domain/workflow/schemaversion_test.go` — unit tests for parser + `Supports`
- `internal/domain/workflow/schemaversion_validate_test.go` — table-driven tests for the per-workflow validation that wires `Supports` into import
- `internal/domain/workflow/openapi_consistency_test.go` — YAML-vs-Go drift check
- `cmd/cyoda/help/content/workflows/schema-version.md` — subtopic body
- `cmd/cyoda/help/workflow_schema.go` — `emitWorkflowSchemaVersions` action
- `cmd/cyoda/help/workflow_schema_test.go` — action emitter tests
- `internal/api/help_action_test.go` — HTTP action dispatch tests
- `internal/e2e/workflow_schema_version_test.go` — full-stack tests
- `docs/workflow-schema-versioning.md` — bump-rule reference + version changelog

**Modify:**

- `internal/common/error_codes.go` — add `ErrCodeWorkflowSchemaVersionUnsupported`
- `internal/domain/workflow/validate.go` — add `validateSchemaVersions` (new top-level function, separate from `validateWorkflows` so it can use a distinct error code)
- `internal/domain/workflow/handler.go` — call schema-version validation before any other mutation in `ImportEntityModelWorkflow`; stamp `CurrentSchemaVersion` in `ExportEntityModelWorkflow`
- `internal/domain/workflow/default_workflow.json` — `"version": "1"` → `"1.0"`
- `internal/e2e/workflow_test.go` — fixtures `"version": "1"` / `"2"` → `"1.0"`
- `api/openapi.yaml` — `WorkflowConfigurationDto.version` description + add `pattern`
- `cmd/cyoda/help/actions.go` — refactor `actionRegistry` value type to `actionEntry{Handler, ContentType}`; register `workflows.schema-version` action; declare content type for every existing action
- `cmd/cyoda/help/command.go` — adjust action lookup to use the new struct
- `cmd/cyoda/help/content/workflows.md` — add `workflows.schema-version` to `see_also` (or rely on auto-listed SUBTOPICS — verify rendering)
- `internal/api/help.go` — extend the `prefix+"/"` handler to detect action paths and dispatch
- `cmd/cyoda/help/actions_test.go` — update existing assertions for the registry shape change
- `cmd/cyoda/help/help_test.go` — update assertions if topic tree count is asserted

**Reference (no edits required, just for context):**

- `cmd/cyoda/help/help.go` — `Tree.Find`, `Topic.Descriptor` (already returns `Actions` slice)
- `cmd/cyoda/help/renderer/json.go` — `TopicDescriptor` shape
- `cmd/cyoda/help/content/workflows.md` — existing parent topic
- `e2e/parity/externalapi/workflow_import_export.go` — parity fixtures already use `"1.0"` (no edits needed; they happen to be compliant)

---

## Task 1: Add error code constant

**Files:**
- Modify: `internal/common/error_codes.go`

- [ ] **Step 1: Add the constant**

  Insert into the first `const ( ... )` block in `internal/common/error_codes.go`, keeping alphabetical adjacency with other workflow codes. Place the line after `ErrCodeWorkflowFailed = "WORKFLOW_FAILED"`:

  ```go
  ErrCodeWorkflowSchemaVersionUnsupported = "WORKFLOW_SCHEMA_VERSION_UNSUPPORTED"
  ```

- [ ] **Step 2: Confirm compile-clean**

  Run: `go build ./internal/common/...`
  Expected: no output (success).

- [ ] **Step 3: Commit**

  ```bash
  git add internal/common/error_codes.go
  git -c commit.gpgsign=false commit -m "feat(workflow): add WORKFLOW_SCHEMA_VERSION_UNSUPPORTED error code

  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
  ```

---

## Task 2: Schema-version primitives + unit tests (RED → GREEN)

**Files:**
- Create: `internal/domain/workflow/schemaversion_test.go`
- Create: `internal/domain/workflow/schemaversion.go`

- [ ] **Step 1: Write the failing test file**

  Create `internal/domain/workflow/schemaversion_test.go`:

  ```go
  package workflow

  import (
  	"errors"
  	"testing"
  )

  func TestCurrentSchemaVersionIsSupported(t *testing.T) {
  	t.Parallel()
  	maj, min, err := ParseSchemaVersion(CurrentSchemaVersion)
  	if err != nil {
  		t.Fatalf("CurrentSchemaVersion %q does not parse: %v", CurrentSchemaVersion, err)
  	}
  	if err := Supports(maj, min); err != nil {
  		t.Fatalf("CurrentSchemaVersion %q not inside SupportedSchemaRanges: %v", CurrentSchemaVersion, err)
  	}
  }

  func TestParseSchemaVersion(t *testing.T) {
  	t.Parallel()
  	cases := []struct {
  		in        string
  		wantMajor int
  		wantMinor int
  		wantErr   bool
  	}{
  		{"1.0", 1, 0, false},
  		{"0.0", 0, 0, false},
  		{"12.345", 12, 345, false},
  		{"2.0", 2, 0, false},
  		// rejected
  		{"", 0, 0, true},
  		{"1", 0, 0, true},
  		{"1.0.0", 0, 0, true},
  		{" 1.0 ", 0, 0, true},
  		{"v1.0", 0, 0, true},
  		{"1.x", 0, 0, true},
  		{"01.0", 0, 0, true},
  		{"1.00", 0, 0, true},
  		{"-1.0", 0, 0, true},
  		{"1.-0", 0, 0, true},
  		{"1.0\n", 0, 0, true},
  		{"１.0", 0, 0, true}, // fullwidth digit
  	}
  	for _, tc := range cases {
  		tc := tc
  		t.Run(tc.in, func(t *testing.T) {
  			t.Parallel()
  			maj, min, err := ParseSchemaVersion(tc.in)
  			if tc.wantErr {
  				if err == nil {
  					t.Fatalf("ParseSchemaVersion(%q) = (%d, %d, nil); want error", tc.in, maj, min)
  				}
  				return
  			}
  			if err != nil {
  				t.Fatalf("ParseSchemaVersion(%q) = error %v; want (%d, %d, nil)", tc.in, err, tc.wantMajor, tc.wantMinor)
  			}
  			if maj != tc.wantMajor || min != tc.wantMinor {
  				t.Fatalf("ParseSchemaVersion(%q) = (%d, %d); want (%d, %d)", tc.in, maj, min, tc.wantMajor, tc.wantMinor)
  			}
  		})
  	}
  }

  func TestSupports(t *testing.T) {
  	t.Parallel()
  	// Stash & swap ranges so the test is independent of the global ranges
  	// the production code uses.
  	orig := SupportedSchemaRanges
  	SupportedSchemaRanges = []SchemaRange{
  		{Major: 1, MinMinor: 1, MaxMinor: 3},
  	}
  	t.Cleanup(func() { SupportedSchemaRanges = orig })

  	cases := []struct {
  		name      string
  		major     int
  		minor     int
  		wantErr   error
  	}{
  		{"in range low", 1, 1, nil},
  		{"in range high", 1, 3, nil},
  		{"minor too old", 1, 0, ErrSchemaMinorTooOld},
  		{"minor too new", 1, 4, ErrSchemaMinorTooNew},
  		{"major absent", 2, 0, ErrSchemaMajorUnsupported},
  	}
  	for _, tc := range cases {
  		tc := tc
  		t.Run(tc.name, func(t *testing.T) {
  			t.Parallel()
  			err := Supports(tc.major, tc.minor)
  			if tc.wantErr == nil {
  				if err != nil {
  					t.Fatalf("Supports(%d, %d) = %v; want nil", tc.major, tc.minor, err)
  				}
  				return
  			}
  			if !errors.Is(err, tc.wantErr) {
  				t.Fatalf("Supports(%d, %d) = %v; want errors.Is(_, %v)", tc.major, tc.minor, err, tc.wantErr)
  			}
  		})
  	}
  }

  func TestSupportsMessageNamesSupportedMajors(t *testing.T) {
  	t.Parallel()
  	orig := SupportedSchemaRanges
  	SupportedSchemaRanges = []SchemaRange{
  		{Major: 1, MinMinor: 0, MaxMinor: 0},
  		{Major: 3, MinMinor: 0, MaxMinor: 0},
  	}
  	t.Cleanup(func() { SupportedSchemaRanges = orig })

  	err := Supports(2, 0)
  	if err == nil {
  		t.Fatalf("Supports(2,0) = nil; want major-unsupported error")
  	}
  	msg := err.Error()
  	for _, want := range []string{"2", "1", "3"} {
  		if !contains(msg, want) {
  			t.Fatalf("Supports(2,0) message %q missing %q", msg, want)
  		}
  	}
  }

  // contains is a local helper so this test file doesn't import strings
  // for one call.
  func contains(haystack, needle string) bool {
  	for i := 0; i+len(needle) <= len(haystack); i++ {
  		if haystack[i:i+len(needle)] == needle {
  			return true
  		}
  	}
  	return false
  }
  ```

- [ ] **Step 2: Run the test to confirm it fails**

  Run: `go test ./internal/domain/workflow/ -run 'TestCurrentSchemaVersionIsSupported|TestParseSchemaVersion|TestSupports' -v`
  Expected: build failure — symbols `CurrentSchemaVersion`, `ParseSchemaVersion`, `SchemaRange`, `SupportedSchemaRanges`, `Supports`, `ErrSchema*` are undefined.

- [ ] **Step 3: Create the production file**

  Create `internal/domain/workflow/schemaversion.go`:

  ```go
  // Package workflow — schema-version contract for the workflow-import
  // DTO shape. Independent of the cyoda-go binary version and the
  // OpenAPI document version. See docs/workflow-schema-versioning.md
  // for the bump rules and the per-version changelog.
  package workflow

  import (
  	"errors"
  	"fmt"
  	"strconv"
  	"strings"
  )

  // CurrentSchemaVersion is stamped on every exported workflow. Bump
  // only per docs/workflow-schema-versioning.md. MUST be inside one of
  // SupportedSchemaRanges; schemaversion_test.go asserts this.
  const CurrentSchemaVersion = "1.0"

  // SchemaRange is a closed integer interval [MinMinor..MaxMinor] on
  // the MINOR axis of a given MAJOR. A range models a single contiguous
  // supported window — when a MINOR ages out, raise MinMinor; when an
  // older MAJOR is retired, drop its range entirely.
  type SchemaRange struct {
  	Major    int
  	MinMinor int
  	MaxMinor int
  }

  // SupportedSchemaRanges is the closed set of (MAJOR, MINOR) pairs the
  // server accepts on import. To add a new MINOR within a MAJOR, raise
  // MaxMinor. To retire old MINORs, raise MinMinor. To add a new MAJOR,
  // append a new SchemaRange.
  //
  // Tests may overwrite this variable (with t.Cleanup restoration) to
  // exercise alternative range configurations without changing
  // production defaults.
  var SupportedSchemaRanges = []SchemaRange{
  	{Major: 1, MinMinor: 0, MaxMinor: 0},
  }

  // Sentinel errors returned by Supports. Callers use errors.Is to
  // branch on sub-case and produce a precise client-facing message.
  var (
  	ErrSchemaMajorUnsupported = errors.New("workflow schema major version unsupported")
  	ErrSchemaMinorTooNew      = errors.New("workflow schema minor version too new")
  	ErrSchemaMinorTooOld      = errors.New("workflow schema minor version no longer accepted")
  )

  // ParseSchemaVersion parses a MAJOR.MINOR string into integers. The
  // accepted shape is the regex ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ —
  // no leading zeros (except a single "0"), no whitespace, no PATCH
  // suffix, no sign. Errors mention the offending input so the import
  // handler can surface it verbatim to the client.
  func ParseSchemaVersion(s string) (major, minor int, err error) {
  	if s == "" {
  		return 0, 0, fmt.Errorf("workflow schema version is empty; required MAJOR.MINOR form")
  	}
  	parts := strings.Split(s, ".")
  	if len(parts) != 2 {
  		return 0, 0, fmt.Errorf("workflow schema version %q is not in MAJOR.MINOR form", s)
  	}
  	parseSegment := func(seg string) (int, error) {
  		if seg == "" {
  			return 0, fmt.Errorf("empty segment")
  		}
  		// Reject leading zeros for non-zero values: "01" is invalid but
  		// "0" is fine.
  		if len(seg) > 1 && seg[0] == '0' {
  			return 0, fmt.Errorf("leading zero")
  		}
  		for _, c := range seg {
  			if c < '0' || c > '9' {
  				return 0, fmt.Errorf("non-digit character %q", c)
  			}
  		}
  		n, convErr := strconv.Atoi(seg)
  		if convErr != nil {
  			return 0, convErr
  		}
  		return n, nil
  	}
  	maj, e1 := parseSegment(parts[0])
  	if e1 != nil {
  		return 0, 0, fmt.Errorf("workflow schema version %q is not in MAJOR.MINOR form", s)
  	}
  	min, e2 := parseSegment(parts[1])
  	if e2 != nil {
  		return 0, 0, fmt.Errorf("workflow schema version %q is not in MAJOR.MINOR form", s)
  	}
  	return maj, min, nil
  }

  // Supports reports whether (major, minor) is inside any supported
  // range. On failure, the returned error wraps one of
  // ErrSchemaMajorUnsupported, ErrSchemaMinorTooNew, or
  // ErrSchemaMinorTooOld, and its message is suitable for client-facing
  // surfacing — it names the offending pair and the supported window.
  func Supports(major, minor int) error {
  	var matchedMajor bool
  	for _, r := range SupportedSchemaRanges {
  		if r.Major != major {
  			continue
  		}
  		matchedMajor = true
  		if minor < r.MinMinor {
  			return fmt.Errorf("%w: workflow schema %d.%d is no longer accepted on this server; minimum supported in major %d: %d.%d",
  				ErrSchemaMinorTooOld, major, minor, r.Major, r.Major, r.MinMinor)
  		}
  		if minor > r.MaxMinor {
  			return fmt.Errorf("%w: this server supports workflow schema up to %d.%d; payload declares %d.%d. Upgrade cyoda-go or regenerate the file against an older schema",
  				ErrSchemaMinorTooNew, r.Major, r.MaxMinor, major, minor)
  		}
  		return nil
  	}
  	if !matchedMajor {
  		majors := make([]int, 0, len(SupportedSchemaRanges))
  		for _, r := range SupportedSchemaRanges {
  			majors = append(majors, r.Major)
  		}
  		return fmt.Errorf("%w: workflow schema major version %d unsupported on this server; supported majors: %v",
  			ErrSchemaMajorUnsupported, major, majors)
  	}
  	return nil
  }
  ```

- [ ] **Step 4: Run tests, confirm green**

  Run: `go test ./internal/domain/workflow/ -run 'TestCurrentSchemaVersionIsSupported|TestParseSchemaVersion|TestSupports' -v`
  Expected: all subtests PASS.

- [ ] **Step 5: Run full workflow-package tests to confirm no regression**

  Run: `go test ./internal/domain/workflow/ -short`
  Expected: `ok`.

- [ ] **Step 6: Commit**

  ```bash
  git add internal/domain/workflow/schemaversion.go internal/domain/workflow/schemaversion_test.go
  git -c commit.gpgsign=false commit -m "feat(workflow): schema-version primitives (semver MAJOR.MINOR)

  - CurrentSchemaVersion = \"1.0\"
  - SupportedSchemaRanges with MinMinor/MaxMinor windows per MAJOR
  - ParseSchemaVersion rejects leading zeros, whitespace, PATCH suffix
  - Supports returns sentinel errors for major-absent, minor-too-new, minor-too-old

  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
  ```

---

## Task 3: Wire schema-version validation into import — RED

**Files:**
- Create: `internal/domain/workflow/schemaversion_validate_test.go`

This task only writes the failing test; the implementation lands in Task 4. Split intentionally for clear RED → GREEN boundaries.

- [ ] **Step 1: Write the failing test file**

  Create `internal/domain/workflow/schemaversion_validate_test.go`:

  ```go
  package workflow

  import (
  	"errors"
  	"strings"
  	"testing"

  	spi "github.com/cyoda-platform/cyoda-go-spi"
  )

  // validateSchemaVersions is the per-workflow check exercised here.
  // It is added in Task 4 (see plan).

  func TestValidateSchemaVersions_AcceptsCurrent(t *testing.T) {
  	t.Parallel()
  	wfs := []spi.WorkflowDefinition{
  		{Name: "wf", Version: CurrentSchemaVersion, InitialState: "S",
  			States: map[string]spi.StateDefinition{"S": {}}},
  	}
  	if err := validateSchemaVersions(wfs); err != nil {
  		t.Fatalf("validateSchemaVersions(current) = %v; want nil", err)
  	}
  }

  func TestValidateSchemaVersions_RejectsMalformed(t *testing.T) {
  	t.Parallel()
  	wfs := []spi.WorkflowDefinition{
  		{Name: "wf-bad", Version: "1.0.0", InitialState: "S",
  			States: map[string]spi.StateDefinition{"S": {}}},
  	}
  	err := validateSchemaVersions(wfs)
  	if err == nil {
  		t.Fatalf("validateSchemaVersions(\"1.0.0\") = nil; want error")
  	}
  	if !strings.Contains(err.Error(), "wf-bad") {
  		t.Fatalf("error message %q does not name workflow wf-bad", err.Error())
  	}
  	if !strings.Contains(err.Error(), "MAJOR.MINOR") {
  		t.Fatalf("error message %q does not mention MAJOR.MINOR form", err.Error())
  	}
  }

  func TestValidateSchemaVersions_RejectsMajorUnsupported(t *testing.T) {
  	t.Parallel()
  	wfs := []spi.WorkflowDefinition{
  		{Name: "wf", Version: "2.0", InitialState: "S",
  			States: map[string]spi.StateDefinition{"S": {}}},
  	}
  	err := validateSchemaVersions(wfs)
  	if err == nil {
  		t.Fatalf("validateSchemaVersions(\"2.0\") = nil; want error")
  	}
  	if !errors.Is(err, ErrSchemaMajorUnsupported) {
  		t.Fatalf("error %v is not ErrSchemaMajorUnsupported", err)
  	}
  }

  func TestValidateSchemaVersions_RejectsMinorTooNew(t *testing.T) {
  	t.Parallel()
  	wfs := []spi.WorkflowDefinition{
  		{Name: "wf", Version: "1.99", InitialState: "S",
  			States: map[string]spi.StateDefinition{"S": {}}},
  	}
  	err := validateSchemaVersions(wfs)
  	if err == nil {
  		t.Fatalf("validateSchemaVersions(\"1.99\") = nil; want error")
  	}
  	if !errors.Is(err, ErrSchemaMinorTooNew) {
  		t.Fatalf("error %v is not ErrSchemaMinorTooNew", err)
  	}
  }

  func TestValidateSchemaVersions_NamesOffendingWorkflowInMixedList(t *testing.T) {
  	t.Parallel()
  	wfs := []spi.WorkflowDefinition{
  		{Name: "good-wf", Version: "1.0", InitialState: "S",
  			States: map[string]spi.StateDefinition{"S": {}}},
  		{Name: "bad-wf", Version: "2.0", InitialState: "S",
  			States: map[string]spi.StateDefinition{"S": {}}},
  	}
  	err := validateSchemaVersions(wfs)
  	if err == nil {
  		t.Fatalf("validateSchemaVersions(mixed) = nil; want error")
  	}
  	if !strings.Contains(err.Error(), "bad-wf") {
  		t.Fatalf("error message %q does not name offending workflow bad-wf", err.Error())
  	}
  	if strings.Contains(err.Error(), "good-wf") {
  		t.Fatalf("error message %q wrongly names compliant workflow good-wf", err.Error())
  	}
  }
  ```

- [ ] **Step 2: Run, confirm fail**

  Run: `go test ./internal/domain/workflow/ -run 'TestValidateSchemaVersions' -v`
  Expected: build failure — `validateSchemaVersions` is undefined.

- [ ] **Step 3: Commit the RED test**

  ```bash
  git add internal/domain/workflow/schemaversion_validate_test.go
  git -c commit.gpgsign=false commit -m "test(workflow): RED — validateSchemaVersions contract

  Failing test for the per-workflow schema-version check that will be
  added to validate.go in the next commit.

  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
  ```

---

## Task 4: Wire schema-version validation into import — GREEN

**Files:**
- Modify: `internal/domain/workflow/validate.go`
- Modify: `internal/domain/workflow/handler.go`
- Modify: `internal/e2e/workflow_test.go` (co-migrate fixtures in the same commit)

The e2e fixtures `workflowV1`/`workflowV2` are migrated in this same task so the commit leaves the tree green. The two other migration sites (default_workflow.json, defaultwf_fallback_test.go) bypass the import handler — they're handled separately in Task 5.

- [ ] **Step 1: Add `validateSchemaVersions` to validate.go**

  Append to `internal/domain/workflow/validate.go` (after `validateProcessorFlags`):

  ```go
  // validateSchemaVersions checks each workflow's Version field against
  // SupportedSchemaRanges. Returns an error wrapping one of the
  // sentinel schema errors so callers can branch with errors.Is. The
  // error message names the offending workflow by name so a multi-
  // workflow import surfaces a clear diagnosis without iterating again.
  //
  // This check is intentionally separate from validateWorkflows: it
  // maps to ErrCodeWorkflowSchemaVersionUnsupported in the handler,
  // not ErrCodeValidationFailed, and it must run BEFORE
  // applyImportMode mutates anything.
  func validateSchemaVersions(workflows []spi.WorkflowDefinition) error {
  	for _, wf := range workflows {
  		maj, min, err := ParseSchemaVersion(wf.Version)
  		if err != nil {
  			return fmt.Errorf("workflow %q: %w", wf.Name, err)
  		}
  		if err := Supports(maj, min); err != nil {
  			return fmt.Errorf("workflow %q: %w", wf.Name, err)
  		}
  	}
  	return nil
  }
  ```

- [ ] **Step 2: Run validate-only test, confirm green**

  Run: `go test ./internal/domain/workflow/ -run 'TestValidateSchemaVersions' -v`
  Expected: all subtests PASS.

- [ ] **Step 3: Call validateSchemaVersions in the import handler**

  In `internal/domain/workflow/handler.go`, insert the schema-version check between the mode validation block and the `ref := spi.ModelRef{...}` line (around line 55–57, immediately AFTER the `if mode != "MERGE" && ...` block):

  ```go
  // Schema-version gate — runs before any mutation or store access.
  // Surfaces with WORKFLOW_SCHEMA_VERSION_UNSUPPORTED so clients can
  // distinguish "wrong contract version" from generic validation
  // failures.
  if err := validateSchemaVersions(req.Workflows); err != nil {
  	common.WriteError(w, r, common.Operational(http.StatusBadRequest,
  		common.ErrCodeWorkflowSchemaVersionUnsupported, err.Error()))
  	return
  }
  ```

- [ ] **Step 4: Co-migrate the e2e fixtures used through the import handler**

  In `internal/e2e/workflow_test.go`:
  - The `workflowV1` constant declaration contains `"version": "1"`. Change to `"version": "1.0"`.
  - The `workflowV2` constant declaration contains `"version": "2"`. Change to `"version": "1.0"`. The `name` field (`order-workflow-v1` vs `order-workflow-v2`) carries the actual uniqueness identity; the schema-version field is contract, not identity.

  These constants are the only e2e fixtures that flow through the import handler at v0.7.1. The audit at plan-write time confirmed no other strict-version-sensitive e2e fixtures exist.

- [ ] **Step 5: Confirm workflow-package tests still green**

  Run: `go test ./internal/domain/workflow/ -short -v`
  Expected: all PASS. The pre-existing tests in this package either use `"1.0"` already or bypass the import handler via `wfStore.Save` directly (e.g. `defaultwf_fallback_test.go`); strict validation does not affect them.

- [ ] **Step 6: Confirm e2e tests still green**

  Run: `go test ./internal/e2e/ -short -v`
  Expected: all PASS. The fixture co-migration in Step 4 keeps the e2e workflow-import tests aligned with the new strict validation.

- [ ] **Step 7: Commit**

  ```bash
  git add internal/domain/workflow/validate.go internal/domain/workflow/handler.go internal/e2e/workflow_test.go
  git -c commit.gpgsign=false commit -m "feat(workflow): validate schema version on import

  Per-workflow validateSchemaVersions runs before any store access or
  mutation. Failures map to WORKFLOW_SCHEMA_VERSION_UNSUPPORTED (400)
  with the offending workflow named in the message body. E2E fixtures
  workflowV1/workflowV2 co-migrated to \"1.0\" so the tree stays green
  in one commit.

  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
  ```

---

## Task 5: Migrate remaining fixtures from "1" to "1.0"

**Files:**
- Modify: `internal/domain/workflow/default_workflow.json`
- Modify: `internal/domain/workflow/defaultwf_fallback_test.go`

The e2e fixtures (`workflowV1`/`workflowV2`) were already co-migrated in Task 4. This task covers the two remaining bare-integer literals, both of which bypass the import handler (so they could not break Task 4's commit but should still be normalised for consistency). Do NOT search-and-replace blindly — `ModelRef.ModelVersion` literals like `"1"` are an unrelated concern and must stay.

- [ ] **Step 1: Replace `"version": "1"` in the embedded default workflow**

  In `internal/domain/workflow/default_workflow.json`, line 2 currently reads `"version": "1",`. Change to `"version": "1.0",`. Leave all other fields untouched. The default workflow is loaded by the engine fallback path (not through `ImportEntityModelWorkflow`), so this change is purely cosmetic at runtime — but matters for re-export consistency.

- [ ] **Step 2: Replace the test fixture in defaultwf_fallback_test.go**

  In `internal/domain/workflow/defaultwf_fallback_test.go`, the `WorkflowDefinition` literal around line 22 has `Version: "1",`. Change to `Version: "1.0",`. The surrounding `ModelRef{ModelVersion: "1"}` on line 12 is the entity-model version — leave it alone.

- [ ] **Step 3: Run workflow-package tests**

  Run: `go test ./internal/domain/workflow/ -short -v`
  Expected: all PASS.

- [ ] **Step 4: Confirm no other in-tree fixture uses the bare-integer form**

  Run:
  ```bash
  grep -rn '"version": *"[0-9]\+"' --include='*.go' --include='*.json' .
  grep -rn 'Version: *"[0-9]\+",' --include='*_test.go' . | grep -v ModelVersion
  ```
  Expected:
  - First command returns no hits.
  - Second command returns no hits.
  - `e2e/parity/externalapi/workflow_import_export.go` already uses `"1.0"` (compliant; no edits needed).

  Any remaining hits must be addressed before commit.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/domain/workflow/default_workflow.json \
          internal/domain/workflow/defaultwf_fallback_test.go
  git -c commit.gpgsign=false commit -m "chore(workflow): normalise remaining fixtures to schema version 1.0

  Default workflow (embedded) and defaultwf_fallback_test bypass the
  import handler but should still match the canonical wire form.

  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
  ```

---

## Task 6: Stamp CurrentSchemaVersion on export — RED → GREEN

**Files:**
- Modify: `internal/domain/workflow/handler.go`
- Create test: extend an existing test file or add `internal/domain/workflow/export_schema_version_test.go`

- [ ] **Step 1: Write a failing unit test**

  Create `internal/domain/workflow/export_schema_version_test.go`:

  ```go
  package workflow

  import (
  	"context"
  	"encoding/json"
  	"net/http"
  	"net/http/httptest"
  	"strings"
  	"testing"

  	spi "github.com/cyoda-platform/cyoda-go-spi"
  )

  // exportStubFactory provides a WorkflowStore that returns a fixed
  // set of workflows whose Version differs from CurrentSchemaVersion.
  // The exporter must overwrite that on the wire.
  type exportStubFactory struct {
  	spi.StoreFactory
  	wfs []spi.WorkflowDefinition
  }

  func (f *exportStubFactory) WorkflowStore(_ context.Context) (spi.WorkflowStore, error) {
  	return &exportStubWFStore{wfs: f.wfs}, nil
  }

  type exportStubWFStore struct {
  	spi.WorkflowStore
  	wfs []spi.WorkflowDefinition
  }

  func (s *exportStubWFStore) Get(_ context.Context, _ spi.ModelRef) ([]spi.WorkflowDefinition, error) {
  	return s.wfs, nil
  }

  func TestExportStampsCurrentSchemaVersion(t *testing.T) {
  	t.Parallel()
  	h := &Handler{factory: &exportStubFactory{wfs: []spi.WorkflowDefinition{
  		{Name: "wf-stale", Version: "0.0", InitialState: "S",
  			States: map[string]spi.StateDefinition{"S": {}}},
  	}}}
  	req := httptest.NewRequest(http.MethodGet, "/api/model/E/1/workflow/export", nil)
  	rec := httptest.NewRecorder()
  	h.ExportEntityModelWorkflow(rec, req, "E", 1)
  	if rec.Code != http.StatusOK {
  		t.Fatalf("export status = %d; want 200; body: %s", rec.Code, rec.Body.String())
  	}
  	var body struct {
  		Workflows []struct {
  			Name    string `json:"name"`
  			Version string `json:"version"`
  		} `json:"workflows"`
  	}
  	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
  		t.Fatalf("decode export body: %v", err)
  	}
  	if len(body.Workflows) != 1 {
  		t.Fatalf("got %d workflows; want 1", len(body.Workflows))
  	}
  	if body.Workflows[0].Version != CurrentSchemaVersion {
  		t.Fatalf("export Version = %q; want %q", body.Workflows[0].Version, CurrentSchemaVersion)
  	}
  	if strings.Contains(rec.Body.String(), "\"0.0\"") {
  		t.Fatalf("stored stale Version 0.0 leaked into export body")
  	}
  }
  ```

  Note: this test uses minimal embedded interfaces. If the existing `Handler{factory, engine}` construction or the SPI shape requires anything extra at v0.7.1, add the minimum field initialisers needed to compile. The stub's `engine` field can remain nil — `ExportEntityModelWorkflow` does not touch `h.engine`.

- [ ] **Step 2: Run, confirm fail**

  Run: `go test ./internal/domain/workflow/ -run TestExportStampsCurrentSchemaVersion -v`
  Expected: FAIL with `export Version = "0.0"; want "1.0"`.

- [ ] **Step 3: Update the export handler to stamp the version**

  In `internal/domain/workflow/handler.go`, modify `ExportEntityModelWorkflow`. The current code (around line 145–148) builds the response as:

  ```go
  resp := map[string]any{
  	"entityName":   entityName,
  	"modelVersion": modelVersion,
  	"workflows":    workflows,
  }
  ```

  Replace with a version-stamping pass before composing the response:

  ```go
  // Stamp the current schema version on every workflow on the wire.
  // The stored Version is the workflow content's record; the exported
  // Version is the serialiser's contract. Callers re-importing an
  // export always see the current contract.
  stamped := make([]spi.WorkflowDefinition, len(workflows))
  for i, wf := range workflows {
  	wf.Version = CurrentSchemaVersion
  	stamped[i] = wf
  }

  resp := map[string]any{
  	"entityName":   entityName,
  	"modelVersion": modelVersion,
  	"workflows":    stamped,
  }
  ```

  The `wf.Version = ...` assignment copies the loop variable (which is itself a value copy of `workflows[i]`), so the stored slice is not mutated — important per the ownership-mutability rule about not mutating inputs after handing off.

- [ ] **Step 4: Run, confirm green**

  Run: `go test ./internal/domain/workflow/ -run TestExportStampsCurrentSchemaVersion -v`
  Expected: PASS.

  Run: `go test ./internal/domain/workflow/ -short`
  Expected: `ok`.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/domain/workflow/handler.go internal/domain/workflow/export_schema_version_test.go
  git -c commit.gpgsign=false commit -m "feat(workflow): stamp CurrentSchemaVersion on export

  Stored Version is the workflow content; exported Version is the
  serialiser's contract. Re-importing an export always declares the
  current contract.

  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
  ```

---

## Task 7: OpenAPI pattern + description + consistency test — RED → GREEN

**Files:**
- Create: `internal/domain/workflow/openapi_consistency_test.go`
- Modify: `api/openapi.yaml`

- [ ] **Step 1: Write the failing test**

  Create `internal/domain/workflow/openapi_consistency_test.go`:

  ```go
  package workflow

  import (
  	"os"
  	"strings"
  	"testing"

  	"gopkg.in/yaml.v3"
  )

  // TestOpenAPIWorkflowVersionContract asserts that
  // WorkflowConfigurationDto.version in api/openapi.yaml carries the
  // exact pattern enforced by the Go ParseSchemaVersion parser and
  // names the help-topic discovery endpoint in its description. Drift
  // between YAML and Go becomes a test failure here, not a runtime
  // surprise.
  func TestOpenAPIWorkflowVersionContract(t *testing.T) {
  	t.Parallel()
  	raw, err := os.ReadFile("../../../api/openapi.yaml")
  	if err != nil {
  		t.Fatalf("read openapi.yaml: %v", err)
  	}
  	var spec map[string]any
  	if err := yaml.Unmarshal(raw, &spec); err != nil {
  		t.Fatalf("parse openapi.yaml: %v", err)
  	}
  	comps, ok := spec["components"].(map[string]any)
  	if !ok {
  		t.Fatalf("components missing or wrong shape")
  	}
  	schemas, ok := comps["schemas"].(map[string]any)
  	if !ok {
  		t.Fatalf("components.schemas missing or wrong shape")
  	}
  	wcd, ok := schemas["WorkflowConfigurationDto"].(map[string]any)
  	if !ok {
  		t.Fatalf("WorkflowConfigurationDto missing")
  	}
  	props, ok := wcd["properties"].(map[string]any)
  	if !ok {
  		t.Fatalf("WorkflowConfigurationDto.properties missing")
  	}
  	ver, ok := props["version"].(map[string]any)
  	if !ok {
  		t.Fatalf("WorkflowConfigurationDto.version missing")
  	}
  	pattern, _ := ver["pattern"].(string)
  	const wantPattern = `^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`
  	if pattern != wantPattern {
  		t.Fatalf("version.pattern = %q; want %q", pattern, wantPattern)
  	}
  	desc, _ := ver["description"].(string)
  	for _, want := range []string{"MAJOR.MINOR", "/help/workflows/schema-version/versions"} {
  		if !strings.Contains(desc, want) {
  			t.Fatalf("version.description does not contain %q. Got:\n%s", want, desc)
  		}
  	}
  }
  ```

  Path note: the test file is at `internal/domain/workflow/openapi_consistency_test.go`; the OpenAPI YAML is at `api/openapi.yaml`. From the test's directory, that's three `..` hops to repo root, then `api/openapi.yaml` → `../../../api/openapi.yaml`. If running tests with a non-default working directory, this would break — Go's test runner always uses the package dir as cwd, so this is safe.

- [ ] **Step 2: Run, confirm fail**

  Run: `go test ./internal/domain/workflow/ -run TestOpenAPIWorkflowVersionContract -v`
  Expected: FAIL — the YAML currently lacks `pattern` and the description does not mention the help endpoint.

- [ ] **Step 3: Update `api/openapi.yaml` — WorkflowConfigurationDto.version**

  Find the `WorkflowConfigurationDto` block in `api/openapi.yaml`. Locate the `version` property (at approximately line 7619; verify by searching for `WorkflowConfigurationDto:` and reading forward). The current definition is:

  ```yaml
          version:
            type: string
            description: Version of the workflow configuration schema
  ```

  Replace with:

  ```yaml
          version:
            type: string
            pattern: '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
            description: |
              Workflow-import contract version in semver MAJOR.MINOR form
              (e.g. `1.0`). Identifies the wire-format contract this
              workflow was authored against; independent of the cyoda-go
              binary version and the OpenAPI document version. The server
              validates this strictly and rejects unsupported values with
              `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` (400). The
              authoritative list of supported values is published at
              `GET /help/workflows/schema-version/versions` (and the
              equivalent CLI `cyoda help workflows schema-version
              versions`).
  ```

  Keep the existing `required: [..., version, ...]` clause as-is.

- [ ] **Step 4: Run consistency test, confirm green**

  Run: `go test ./internal/domain/workflow/ -run TestOpenAPIWorkflowVersionContract -v`
  Expected: PASS.

- [ ] **Step 5: Run the broader workflow-package and any OpenAPI-related tests**

  Run: `go test ./internal/domain/workflow/ ./internal/e2e/openapivalidator/ -short`
  Expected: PASS. The `openapivalidator` package validates request bodies against the YAML — fixtures already use `"1.0"` which matches the new pattern.

- [ ] **Step 6: Commit**

  ```bash
  git add internal/domain/workflow/openapi_consistency_test.go api/openapi.yaml
  git -c commit.gpgsign=false commit -m "feat(api): OpenAPI pattern + description for workflow schema version

  Adds pattern constraint matching ParseSchemaVersion, points
  description at the discovery endpoint. Consistency test asserts
  YAML and Go cannot drift.

  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
  ```

---

## Task 8: workflows.schema-version topic content

**Files:**
- Create: `cmd/cyoda/help/content/workflows/schema-version.md`

- [ ] **Step 1: Create the topic file**

  Create `cmd/cyoda/help/content/workflows/schema-version.md`:

  ```markdown
  ---
  topic: workflows.schema-version
  title: "workflows schema-version — wire-format contract for workflow import"
  stability: stable
  see_also:
    - workflows
    - errors.WORKFLOW_SCHEMA_VERSION_UNSUPPORTED
    - openapi
  ---

  # workflows schema-version

  ## NAME

  workflows schema-version — semver `MAJOR.MINOR` contract identifying the workflow-import DTO shape that a workflow definition was authored against.

  ## SYNOPSIS

  Every `WorkflowConfigurationDto` carries a `version` field. The server validates it strictly on import and stamps the current contract version on every workflow it exports.

  ```json
  {
    "version": "1.0",
    "name": "my-workflow",
    "initialState": "ready",
    "states": { "ready": {} }
  }
  ```

  ## SEMANTICS

  - **MAJOR** bumps when a payload valid under the previous MAJOR is no longer valid (or vice-versa) — removing a field, renaming, changing semantics, making an optional field required.
  - **MINOR** bumps for additive, backward-compatible changes — a new optional field, a new enum value in an existing string-enum, a new condition operator. **This is the common case.**

  Multiple MAJORs may be accepted concurrently during a deprecation window. Within a MAJOR, the server accepts any MINOR in its declared `[minMinor, maxMinor]` range.

  ## DISCOVERY

  Authoritative discovery is via the `versions` action:

  ```
  cyoda help workflows schema-version versions
  ```

  HTTP mirror:

  ```
  GET /help/workflows/schema-version/versions
  ```

  Both emit the same structured JSON:

  ```json
  {
    "current": "1.0",
    "supported": [
      { "major": 1, "minMinor": 0, "maxMinor": 0 }
    ]
  }
  ```

  ## VALIDATION ERRORS

  On import, an unsupported or malformed `version` returns HTTP 400 with `errorCode: "WORKFLOW_SCHEMA_VERSION_UNSUPPORTED"`. The message body distinguishes:

  - **Malformed** (`"x"`, `"1"`, `"1.0.0"`, leading zeros) — not in `MAJOR.MINOR` form.
  - **Major unsupported** — the major version is not in any supported range.
  - **Minor too new** — the major matches but the minor exceeds this server's `maxMinor`. Upgrade cyoda-go, or regenerate the file against an older schema.
  - **Minor too old** — the major matches but the minor is below the server's `minMinor` (deprecation window). Re-author the file against a supported MINOR.

  ## EXAMPLE: PINNING

  Pin your authoring tools and CI to the schema version they were tested against:

  ```bash
  # in a CI step
  current=$(curl -s $CYODA_HOST/api/help/workflows/schema-version/versions | jq -r .current)
  test "$current" = "1.0" || { echo "schema drift"; exit 1; }
  ```
  ```

- [ ] **Step 2: Confirm tree-load picks up the new topic**

  Run: `go test ./cmd/cyoda/help/ -short -v -run TestLoad`
  Expected: PASS. The tree-load test (if present under that exact name — see `cmd/cyoda/help/help_test.go`) iterates the embedded `content/` tree. Adding a new file in a new subdirectory should not fail the load.

  If `help_test.go` asserts on a specific topic count, adjust the expected count in the same step.

- [ ] **Step 3: Confirm CLI rendering smoke-test**

  Run: `go run ./cmd/cyoda help workflows schema-version --format=markdown 2>&1 | head -20`
  Expected: renders the topic markdown. Title line "workflows schema-version — wire-format contract for workflow import" present.

- [ ] **Step 4: Commit**

  ```bash
  git add cmd/cyoda/help/content/workflows/schema-version.md
  git -c commit.gpgsign=false commit -m "docs(help): add workflows.schema-version topic

  Explains semver MAJOR.MINOR contract, discovery via the versions
  action, validation errors, and a CI-pinning example.

  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
  ```

---

## Task 9: Refactor action registry to carry Content-Type — RED → GREEN

The HTTP action mirror (Task 10) needs to know each action's content type. Refactor first so the new structure is in place.

**Files:**
- Modify: `cmd/cyoda/help/actions.go`
- Modify: `cmd/cyoda/help/command.go`
- Modify: `cmd/cyoda/help/actions_test.go` (if it asserts on the registry shape)

- [ ] **Step 1: Update the registry value type**

  In `cmd/cyoda/help/actions.go`, replace the `actionRegistry` map declaration and `lookupAction` with:

  ```go
  // ActionEntry pairs an action handler with the HTTP Content-Type
  // header to set when serving the action's output over HTTP. The CLI
  // ignores ContentType; the HTTP action-mirror handler in
  // internal/api/help.go uses it.
  type ActionEntry struct {
  	Handler     ActionFunc
  	ContentType string
  }

  // actionRegistry maps topic dotted-path to a map of action-name to
  // ActionEntry. Actions are invoked via "cyoda help <topic> <action>"
  // or "GET /help/<topic>/<action>". Action names must not collide
  // with subtopic names on the same topic.
  var actionRegistry = map[string]map[string]ActionEntry{
  	"openapi": {
  		"json": {Handler: emitOpenAPIJSON, ContentType: "application/json"},
  		"yaml": {Handler: emitOpenAPIYAML, ContentType: "application/yaml"},
  		"tags": {Handler: emitOpenAPITags, ContentType: "text/plain; charset=utf-8"},
  	},
  	"grpc": {
  		"proto": {Handler: emitGRPCProto, ContentType: "text/plain; charset=utf-8"},
  		"json":  {Handler: emitGRPCDescriptorJSON, ContentType: "application/json"},
  	},
  	"cloudevents": {
  		"json": {Handler: emitCloudEventsJSON, ContentType: "application/json"},
  	},
  }

  // lookupAction returns the entry for a topic action, if registered.
  func lookupAction(topic, action string) (ActionEntry, bool) {
  	if m, ok := actionRegistry[topic]; ok {
  		if e, ok := m[action]; ok {
  			return e, true
  		}
  	}
  	return ActionEntry{}, false
  }
  ```

- [ ] **Step 2: Update `command.go` to use the new shape**

  In `cmd/cyoda/help/command.go`, search for callers of `lookupAction`. There are two sites:
  - Around line 65: `if handler, ok := lookupAction(parent.DottedPath(), actionName); ok { return handler(out) }`
  - Around line 75: `if handler, ok := lookupOpenAPITagAction(actionName, format); ok { return handler(out) }`

  Update the first site to:

  ```go
  if entry, ok := lookupAction(parent.DottedPath(), actionName); ok {
  	return entry.Handler(out)
  }
  ```

  Leave the second site (`lookupOpenAPITagAction`) untouched — it returns a different function type, not an `ActionEntry`.

- [ ] **Step 3: Update any tests that introspect the registry**

  Run: `grep -n 'actionRegistry\|lookupAction' cmd/cyoda/help/*_test.go`
  Expected: a few sites. For each, adjust the assertion shape from `ActionFunc` to `ActionEntry`/`.Handler`. Concretely, in `actions_test.go`, any line like `fn, ok := lookupAction(...)` becomes `entry, ok := lookupAction(...)` and the test runs `entry.Handler(out)` instead of `fn(out)`. Add an assertion that `entry.ContentType != ""` for at least one existing action while you're there (a free invariant test).

- [ ] **Step 4: Run help-package tests**

  Run: `go test ./cmd/cyoda/help/ -short -v`
  Expected: all PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add cmd/cyoda/help/actions.go cmd/cyoda/help/command.go cmd/cyoda/help/actions_test.go
  git -c commit.gpgsign=false commit -m "refactor(help): action registry carries Content-Type

  Action entries are now {Handler, ContentType} structs. The CLI
  ignores ContentType; the HTTP action mirror added in the next
  commit uses it.

  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
  ```

---

## Task 10: HTTP action dispatch — RED → GREEN

**Files:**
- Create: `internal/api/help_action_test.go`
- Modify: `internal/api/help.go`

- [ ] **Step 1: Write the failing test**

  Create `internal/api/help_action_test.go`:

  ```go
  package api

  import (
  	"net/http"
  	"net/http/httptest"
  	"strings"
  	"testing"

  	"github.com/cyoda-platform/cyoda-go/cmd/cyoda/help"
  )

  // TestHelpActionMirror — every registered topic action is reachable
  // via HTTP at GET /help/<topic-with-dots-or-slashes>/<action> with
  // its declared Content-Type.
  func TestHelpActionMirror(t *testing.T) {
  	t.Parallel()
  	mux := http.NewServeMux()
  	RegisterHelpRoutes(mux, help.DefaultTree, "", "test-version")
  	srv := httptest.NewServer(mux)
  	t.Cleanup(srv.Close)

  	cases := []struct {
  		path        string
  		wantStatus  int
  		wantPrefix  string // first few bytes of body, format-specific
  		contentType string
  	}{
  		// Existing actions — confirms the mirror is generic.
  		{"/help/grpc/proto", http.StatusOK, "//", "text/plain; charset=utf-8"},
  		{"/help/grpc/json", http.StatusOK, "{", "application/json"},
  		{"/help/openapi/json", http.StatusOK, "{", "application/json"},
  		{"/help/cloudevents/json", http.StatusOK, "{", "application/json"},
  		// Equivalent with dotted separators.
  		{"/help/grpc.json", http.StatusOK, "{", "application/json"},
  		// Unknown action — falls through to topic-not-found.
  		{"/help/grpc/nonsense", http.StatusNotFound, "", ""},
  	}
  	for _, tc := range cases {
  		tc := tc
  		t.Run(tc.path, func(t *testing.T) {
  			t.Parallel()
  			resp, err := http.Get(srv.URL + tc.path)
  			if err != nil {
  				t.Fatalf("GET %s: %v", tc.path, err)
  			}
  			defer resp.Body.Close()
  			if resp.StatusCode != tc.wantStatus {
  				t.Fatalf("GET %s status = %d; want %d", tc.path, resp.StatusCode, tc.wantStatus)
  			}
  			if tc.wantStatus != http.StatusOK {
  				return
  			}
  			ct := resp.Header.Get("Content-Type")
  			if ct != tc.contentType {
  				t.Fatalf("GET %s Content-Type = %q; want %q", tc.path, ct, tc.contentType)
  			}
  			buf := make([]byte, 64)
  			n, _ := resp.Body.Read(buf)
  			if !strings.HasPrefix(string(buf[:n]), tc.wantPrefix) {
  				t.Fatalf("GET %s body prefix = %q; want %q", tc.path, string(buf[:n]), tc.wantPrefix)
  			}
  		})
  	}
  }
  ```

- [ ] **Step 2: Run, confirm fail**

  Run: `go test ./internal/api/ -run TestHelpActionMirror -v`
  Expected: FAIL — every action path returns 404 (current handler only does topic lookup).

- [ ] **Step 3: Extend `RegisterHelpRoutes` to dispatch actions**

  In `internal/api/help.go`, modify the `prefix+"/"` handler. After the line `node := tree.Find(segs)` and BEFORE the `if node == nil { ... 404 ... }` block, insert an action-fallback path:

  ```go
  node := tree.Find(segs)
  if node == nil && len(segs) >= 2 {
  	// Try treating the last segment as an action on the parent topic.
  	parentSegs := segs[:len(segs)-1]
  	actionName := segs[len(segs)-1]
  	parent := tree.Find(parentSegs)
  	if parent != nil {
  		if entry, ok := help.LookupAction(parent.DottedPath(), actionName); ok {
  			w.Header().Set("Content-Type", entry.ContentType)
  			if rc := entry.Handler(w); rc != 0 {
  				slog.Error("help: action handler reported failure",
  					"pkg", "api",
  					"topic", parent.DottedPath(),
  					"action", actionName,
  					"exitCode", rc)
  			}
  			return
  		}
  	}
  }
  if node == nil {
  	common.WriteError(w, r, common.Operational(
  		http.StatusNotFound,
  		common.ErrCodeHelpTopicNotFound,
  		"no such help topic: "+topic,
  	))
  	return
  }
  ```

  Note: the existing `lookupAction` is package-private to `cmd/cyoda/help`. To expose it, in `cmd/cyoda/help/actions.go` add an exported wrapper:

  ```go
  // LookupAction returns the action entry for a topic, if registered.
  // Exported for HTTP-action-mirror consumers in internal/api.
  func LookupAction(topic, action string) (ActionEntry, bool) {
  	return lookupAction(topic, action)
  }
  ```

  And ensure `ActionEntry` is exported (it already is — see Task 9).

- [ ] **Step 4: Run, confirm green**

  Run: `go test ./internal/api/ -run TestHelpActionMirror -v`
  Expected: PASS.

  Run: `go test ./internal/api/ ./cmd/cyoda/help/ -short`
  Expected: `ok`.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/api/help.go internal/api/help_action_test.go cmd/cyoda/help/actions.go
  git -c commit.gpgsign=false commit -m "feat(api): mirror help topic actions to HTTP

  RegisterHelpRoutes now dispatches registered actions when a topic
  lookup misses. Existing actions (grpc proto/json, openapi
  json/yaml/tags, cloudevents json) become reachable via HTTP with
  their declared Content-Type, in addition to the CLI.

  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
  ```

---

## Task 11: workflows.schema-version `versions` action — RED → GREEN

**Files:**
- Create: `cmd/cyoda/help/workflow_schema_test.go`
- Create: `cmd/cyoda/help/workflow_schema.go`
- Modify: `cmd/cyoda/help/actions.go` (register the new action)

- [ ] **Step 1: Write the failing test**

  Create `cmd/cyoda/help/workflow_schema_test.go`:

  ```go
  package help

  import (
  	"bytes"
  	"encoding/json"
  	"testing"

  	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
  )

  func TestEmitWorkflowSchemaVersions_StructuredJSON(t *testing.T) {
  	t.Parallel()
  	var buf bytes.Buffer
  	rc := emitWorkflowSchemaVersions(&buf)
  	if rc != 0 {
  		t.Fatalf("emitWorkflowSchemaVersions = %d; want 0", rc)
  	}
  	var got struct {
  		Current   string `json:"current"`
  		Supported []struct {
  			Major    int `json:"major"`
  			MinMinor int `json:"minMinor"`
  			MaxMinor int `json:"maxMinor"`
  		} `json:"supported"`
  	}
  	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
  		t.Fatalf("unmarshal action output: %v; raw: %s", err, buf.String())
  	}
  	if got.Current != workflow.CurrentSchemaVersion {
  		t.Fatalf("current = %q; want %q", got.Current, workflow.CurrentSchemaVersion)
  	}
  	if len(got.Supported) != len(workflow.SupportedSchemaRanges) {
  		t.Fatalf("supported len = %d; want %d", len(got.Supported), len(workflow.SupportedSchemaRanges))
  	}
  	for i, want := range workflow.SupportedSchemaRanges {
  		g := got.Supported[i]
  		if g.Major != want.Major || g.MinMinor != want.MinMinor || g.MaxMinor != want.MaxMinor {
  			t.Fatalf("supported[%d] = %+v; want %+v", i, g, want)
  		}
  	}
  }

  func TestWorkflowSchemaVersionsRegistered(t *testing.T) {
  	t.Parallel()
  	entry, ok := lookupAction("workflows.schema-version", "versions")
  	if !ok {
  		t.Fatalf("workflows.schema-version/versions not registered")
  	}
  	if entry.ContentType != "application/json" {
  		t.Fatalf("ContentType = %q; want application/json", entry.ContentType)
  	}
  	if entry.Handler == nil {
  		t.Fatalf("Handler is nil")
  	}
  }
  ```

- [ ] **Step 2: Run, confirm fail**

  Run: `go test ./cmd/cyoda/help/ -run 'TestEmitWorkflowSchemaVersions|TestWorkflowSchemaVersionsRegistered' -v`
  Expected: build failure — `emitWorkflowSchemaVersions` is undefined.

- [ ] **Step 3: Create the action emitter**

  Create `cmd/cyoda/help/workflow_schema.go`:

  ```go
  package help

  import (
  	"encoding/json"
  	"fmt"
  	"io"

  	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
  )

  // workflowSchemaVersionsPayload mirrors what the versions action
  // emits. Defined here (not in the workflow package) because it's a
  // wire-format concern of the help subsystem; the workflow package
  // owns the constants.
  type workflowSchemaVersionsPayload struct {
  	Current   string                  `json:"current"`
  	Supported []workflow.SchemaRange  `json:"supported"`
  }

  // emitWorkflowSchemaVersions writes the supported workflow-schema
  // version manifest as JSON. The shape is the contract; consumers
  // pin tooling against it. See cmd/cyoda/help/content/workflows/
  // schema-version.md for the human-readable explanation.
  func emitWorkflowSchemaVersions(w io.Writer) int {
  	// Defensive copy so the registered action never aliases the
  	// production slice (ownership rule 6: consumed once serialized).
  	supported := make([]workflow.SchemaRange, len(workflow.SupportedSchemaRanges))
  	copy(supported, workflow.SupportedSchemaRanges)
  	payload := workflowSchemaVersionsPayload{
  		Current:   workflow.CurrentSchemaVersion,
  		Supported: supported,
  	}
  	enc := json.NewEncoder(w)
  	enc.SetIndent("", "  ")
  	if err := enc.Encode(payload); err != nil {
  		fmt.Fprintf(w, "cyoda help workflows schema-version versions: encode: %v\n", err)
  		return 1
  	}
  	return 0
  }
  ```

  The `workflow.SchemaRange` struct's JSON tags need to match the payload contract. Looking at the production struct from Task 2, the field names are `Major`, `MinMinor`, `MaxMinor` with no explicit tags — Go's default marshalling lowercases to `Major`, `MinMinor`, `MaxMinor` (the field name verbatim). The test expects `major`/`minMinor`/`maxMinor`. So `SchemaRange` MUST carry JSON tags. Go back to `internal/domain/workflow/schemaversion.go` and update the struct definition:

  ```go
  type SchemaRange struct {
  	Major    int `json:"major"`
  	MinMinor int `json:"minMinor"`
  	MaxMinor int `json:"maxMinor"`
  }
  ```

  Per `.claude/rules/ownership-mutability.md` rule 2 (unexported fields by default, exported permitted for DTOs at the API boundary): `SchemaRange` is now an API-boundary DTO via this action — exported fields with JSON tags are correct.

- [ ] **Step 4: Register the action**

  In `cmd/cyoda/help/actions.go`, add a new entry to `actionRegistry`:

  ```go
  "workflows.schema-version": {
  	"versions": {Handler: emitWorkflowSchemaVersions, ContentType: "application/json"},
  },
  ```

- [ ] **Step 5: Run, confirm green**

  Run: `go test ./cmd/cyoda/help/ ./internal/domain/workflow/ -short -v`
  Expected: PASS. The earlier `TestExportStampsCurrentSchemaVersion` and others still pass; adding JSON tags to `SchemaRange` is purely additive at the Go API boundary.

- [ ] **Step 6: Commit**

  ```bash
  git add cmd/cyoda/help/workflow_schema.go cmd/cyoda/help/workflow_schema_test.go cmd/cyoda/help/actions.go internal/domain/workflow/schemaversion.go
  git -c commit.gpgsign=false commit -m "feat(help): workflows schema-version versions action

  CLI: cyoda help workflows schema-version versions
  HTTP: GET /help/workflows/schema-version/versions
  Both emit {current, supported[{major,minMinor,maxMinor}]} JSON.
  SchemaRange gains JSON tags for the wire contract.

  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
  ```

---

## Task 12: docs/workflow-schema-versioning.md changelog

**Files:**
- Create: `docs/workflow-schema-versioning.md`
- Modify: `cmd/cyoda/help/content/workflows.md` (see_also link to subtopic)

- [ ] **Step 1: Write the doc**

  Create `docs/workflow-schema-versioning.md`:

  ```markdown
  # Workflow Schema Versioning

  This document tracks the wire-format contract for `WorkflowConfigurationDto.version` — the semver `MAJOR.MINOR` string each workflow declares on import and the server stamps on export.

  See the in-product help topic for the user-facing reference:

  - CLI: `cyoda help workflows schema-version`
  - HTTP: `GET /help/workflows/schema-version`
  - JSON discovery: `GET /help/workflows/schema-version/versions`

  ## Bump rules

  - **MAJOR**: a payload valid under the previous MAJOR is no longer valid, or vice-versa. Examples: removing a field, renaming a field, making an optional field required, changing semantics of an existing field.
  - **MINOR**: additive, backward-compatible changes. Examples: new optional field, new enum value in an existing string-enum, new optional sub-object, new condition operator. **This is the common case.**

  Both bumps must:

  1. Update `CurrentSchemaVersion` in `internal/domain/workflow/schemaversion.go`.
  2. Extend or amend the appropriate `SchemaRange` in the same file (raise `MaxMinor` for a MINOR; append a new range for a MAJOR).
  3. Add an entry below describing the change and citing the PR.
  4. Update `cmd/cyoda/help/content/workflows/schema-version.md` if the description of the contract changes.

  ## Changelog

  ### 1.0 — initial contract

  First version. Wire shape matches the `WorkflowConfigurationDto` schema in `api/openapi.yaml` at this commit. Prior to this version, `WorkflowConfigurationDto.version` was an unvalidated free-form string; values such as `"1"` and `"1.0"` were both accepted but conveyed no contract. Pre-1.0 binary; no migration window.
  ```

- [ ] **Step 2: Cross-link from the workflows topic**

  In `cmd/cyoda/help/content/workflows.md`, append `workflows.schema-version` to the `see_also` YAML list in the front-matter. Example: after `- errors.COMPUTE_MEMBER_DISCONNECTED`, add `  - workflows.schema-version`.

- [ ] **Step 3: Verify load**

  Run: `go test ./cmd/cyoda/help/ -short`
  Expected: PASS.

- [ ] **Step 4: Commit**

  ```bash
  git add docs/workflow-schema-versioning.md cmd/cyoda/help/content/workflows.md
  git -c commit.gpgsign=false commit -m "docs(workflow): schema-versioning bump rules + changelog

  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
  ```

---

## Task 13: E2E tests

**Files:**
- Create: `internal/e2e/workflow_schema_version_test.go`

- [ ] **Step 1: Confirm the existing E2E harness helpers**

  These were audited against the v0.7.1 tree at plan-write time:
  - Package: `e2e_test` (NOT `e2e`).
  - `importModelE2E(t, entityName, modelVersion int)` creates a sample entity model.
  - `importWorkflowE2E(t, entityName, modelVersion int, payload string) (statusCode int, body string)` performs the workflow import.
  - `exportWorkflowE2E(t, entityName, modelVersion int) (statusCode int, body map[string]any)` performs the workflow export.
  - `doAuth(t, method, path, body string) *http.Response` is the authed HTTP primitive.
  - `readBody(t, *http.Response) string` reads + closes.

  The new tests use these helpers; do NOT add a new `TestMain`. If the harness has rotated between plan-write and execution, re-audit `helpers_test.go` and `workflow_test.go` lines 1–110 and update helper calls.

- [ ] **Step 2: Write the E2E tests**

  Create `internal/e2e/workflow_schema_version_test.go`:

  ```go
  package e2e_test

  import (
  	"encoding/json"
  	"net/http"
  	"strings"
  	"testing"
  )

  // TestWorkflowSchemaVersion_ImportAccepts10 — happy path: a "1.0"
  // workflow imports successfully.
  func TestWorkflowSchemaVersion_ImportAccepts10(t *testing.T) {
  	const entity = "wf-schema-accept"
  	importModelE2E(t, entity, 1)
  	body := `{
  		"importMode": "REPLACE",
  		"workflows": [{
  			"version": "1.0",
  			"name": "wf-1",
  			"initialState": "S1",
  			"active": true,
  			"states": {"S1": {}}
  		}]
  	}`
  	status, respBody := importWorkflowE2E(t, entity, 1, body)
  	if status != http.StatusOK {
  		t.Fatalf("import status = %d; want 200; body: %s", status, respBody)
  	}
  }

  // TestWorkflowSchemaVersion_ImportRejectsMajorUnsupported — a "2.0"
  // workflow is rejected with WORKFLOW_SCHEMA_VERSION_UNSUPPORTED.
  func TestWorkflowSchemaVersion_ImportRejectsMajorUnsupported(t *testing.T) {
  	const entity = "wf-schema-reject"
  	importModelE2E(t, entity, 1)
  	body := `{
  		"importMode": "REPLACE",
  		"workflows": [{
  			"version": "2.0",
  			"name": "wf-bad",
  			"initialState": "S1",
  			"active": true,
  			"states": {"S1": {}}
  		}]
  	}`
  	status, respBody := importWorkflowE2E(t, entity, 1, body)
  	if status != http.StatusBadRequest {
  		t.Fatalf("import status = %d; want 400; body: %s", status, respBody)
  	}
  	var errBody struct {
  		ErrorCode string `json:"errorCode"`
  		Message   string `json:"message"`
  	}
  	if err := json.Unmarshal([]byte(respBody), &errBody); err != nil {
  		t.Fatalf("decode error body: %v; raw: %s", err, respBody)
  	}
  	if errBody.ErrorCode != "WORKFLOW_SCHEMA_VERSION_UNSUPPORTED" {
  		t.Fatalf("errorCode = %q; want WORKFLOW_SCHEMA_VERSION_UNSUPPORTED; body: %s", errBody.ErrorCode, respBody)
  	}
  	if !strings.Contains(errBody.Message, "wf-bad") {
  		t.Fatalf("message %q does not name offending workflow", errBody.Message)
  	}
  }

  // TestWorkflowSchemaVersion_ImportRejectsMalformed — a "1.0.0"
  // workflow is rejected with a message pointing at MAJOR.MINOR.
  func TestWorkflowSchemaVersion_ImportRejectsMalformed(t *testing.T) {
  	const entity = "wf-schema-malformed"
  	importModelE2E(t, entity, 1)
  	body := `{
  		"importMode": "REPLACE",
  		"workflows": [{
  			"version": "1.0.0",
  			"name": "wf-malformed",
  			"initialState": "S1",
  			"active": true,
  			"states": {"S1": {}}
  		}]
  	}`
  	status, respBody := importWorkflowE2E(t, entity, 1, body)
  	if status != http.StatusBadRequest {
  		t.Fatalf("import status = %d; want 400; body: %s", status, respBody)
  	}
  	if !strings.Contains(respBody, "MAJOR.MINOR") {
  		t.Fatalf("body does not mention MAJOR.MINOR form: %s", respBody)
  	}
  	if !strings.Contains(respBody, "WORKFLOW_SCHEMA_VERSION_UNSUPPORTED") {
  		t.Fatalf("body does not contain WORKFLOW_SCHEMA_VERSION_UNSUPPORTED: %s", respBody)
  	}
  }

  // TestWorkflowSchemaVersion_ExportStampsCurrent — after import, the
  // export response carries the current schema version on every workflow.
  func TestWorkflowSchemaVersion_ExportStampsCurrent(t *testing.T) {
  	const entity = "wf-schema-export"
  	importModelE2E(t, entity, 1)
  	body := `{
  		"importMode": "REPLACE",
  		"workflows": [{
  			"version": "1.0",
  			"name": "wf-export",
  			"initialState": "S1",
  			"active": true,
  			"states": {"S1": {}}
  		}]
  	}`
  	if status, b := importWorkflowE2E(t, entity, 1, body); status != http.StatusOK {
  		t.Fatalf("import status = %d; body: %s", status, b)
  	}
  	status, exportBody := exportWorkflowE2E(t, entity, 1)
  	if status != http.StatusOK {
  		t.Fatalf("export status = %d", status)
  	}
  	wfs, ok := exportBody["workflows"].([]any)
  	if !ok {
  		t.Fatalf("export body missing workflows: %+v", exportBody)
  	}
  	for i, raw := range wfs {
  		m, ok := raw.(map[string]any)
  		if !ok {
  			t.Fatalf("workflow[%d] not a map: %T", i, raw)
  		}
  		if m["version"] != "1.0" {
  			t.Fatalf("workflow[%d] version = %v; want \"1.0\"", i, m["version"])
  		}
  	}
  }

  // TestWorkflowSchemaVersion_HelpVersionsAction — discovery endpoint.
  func TestWorkflowSchemaVersion_HelpVersionsAction(t *testing.T) {
  	resp := doAuth(t, http.MethodGet, "/api/help/workflows/schema-version/versions", "")
  	respBody := readBody(t, resp)
  	if resp.StatusCode != http.StatusOK {
  		t.Fatalf("status = %d; body: %s", resp.StatusCode, respBody)
  	}
  	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
  		t.Fatalf("Content-Type = %q; want application/json", ct)
  	}
  	var got struct {
  		Current   string             `json:"current"`
  		Supported []map[string]int   `json:"supported"`
  	}
  	if err := json.Unmarshal([]byte(respBody), &got); err != nil {
  		t.Fatalf("decode: %v; raw: %s", err, respBody)
  	}
  	if got.Current != "1.0" {
  		t.Fatalf("current = %q; want 1.0", got.Current)
  	}
  	if len(got.Supported) != 1 || got.Supported[0]["major"] != 1 {
  		t.Fatalf("supported = %+v; want [{major:1, minMinor:0, maxMinor:0}]", got.Supported)
  	}
  }

  // TestWorkflowSchemaVersion_HelpGRPCProtoStillWorks — regression on a
  // pre-existing action, proves the HTTP action mirror is generic.
  func TestWorkflowSchemaVersion_HelpGRPCProtoStillWorks(t *testing.T) {
  	resp := doAuth(t, http.MethodGet, "/api/help/grpc/proto", "")
  	respBody := readBody(t, resp)
  	if resp.StatusCode != http.StatusOK {
  		t.Fatalf("status = %d; body: %s", resp.StatusCode, respBody)
  	}
  	if !strings.Contains(respBody, "syntax") && !strings.Contains(respBody, "proto") {
  		t.Fatalf("body does not contain proto source: %.200s", respBody)
  	}
  }
  ```

  Note: the help endpoints are served under the configured `CYODA_CONTEXT_PATH` (defaults to `/api`). The e2e harness mounts with that default — `/api/help/...` is the expected path. Confirm against the harness setup in `helpers_test.go` if `doAuth` already prepends `/api`; in the example above, `doAuth(t, GET, "/api/help/...", "")` passes the full path explicitly since other helpers in the file do the same (see `importModelE2E` building `/api/model/...`).

- [ ] **Step 3: Run E2E tests**

  Run: `go test ./internal/e2e/ -run TestWorkflowSchemaVersion -v`
  Expected: all PASS. Docker must be running (testcontainers).

- [ ] **Step 4: Confirm broader E2E suite still green**

  Run: `go test ./internal/e2e/ -v 2>&1 | tail -20`
  Expected: no FAIL lines.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/e2e/workflow_schema_version_test.go
  git -c commit.gpgsign=false commit -m "test(e2e): workflow schema version contract

  Covers: import accept/reject, export stamping, help versions
  endpoint, generic action-mirror regression on /help/grpc/proto.

  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
  ```

---

## Task 14: Final verification

- [ ] **Step 1: Run full short test sweep**

  Run: `go test -short ./...`
  Expected: `ok` for every package; no FAIL lines.

- [ ] **Step 2: Run full E2E suite**

  Run: `go test ./internal/e2e/... -v 2>&1 | tail -20`
  Expected: no FAIL lines. Docker required.

- [ ] **Step 3: Vet**

  Run: `go vet ./...`
  Expected: no output.

- [ ] **Step 4: Race detector (one-shot, end-of-deliverable)**

  Run: `go test -race ./...`
  Expected: no race reports, all tests pass.

- [ ] **Step 5: Build**

  Run: `go build -o /tmp/cyoda-bin ./cmd/cyoda && /tmp/cyoda-bin help workflows schema-version versions | head -10`
  Expected: emits the JSON payload with `"current": "1.0"`.

- [ ] **Step 6: Plugin submodules**

  Per the plugin-submodule rule:
  ```bash
  for d in plugins/memory plugins/sqlite plugins/postgres; do
    (cd $d && go test -short ./...) || exit 1
  done
  ```
  Expected: each module reports `ok` for its tests.

- [ ] **Step 7: Tidy**

  Run: `go mod tidy && git diff --exit-code go.mod go.sum`
  Expected: exit 0 (no changes).

---

## Self-Review Notes (kept for the executing agent)

- The spec calls for `WorkflowConfigurationDto.version` semver semantics; every task either implements or tests a slice of that. Tasks 1–7 deliver the import/export/OpenAPI core; 8–12 deliver discovery (help topic, content, action emitter, HTTP action mirror); 13–14 are end-to-end + verification.
- The HTTP action mirror (Task 10) is a small generic improvement that closes a real gap between CLI and HTTP help surfaces. Scope is bounded to one handler + one wrapper + one test file. Per Gate 6, the right call is to fix it now alongside the new action that needs it.
- The fixture migration (Task 5) is intentionally a separate commit so the diff that introduces strict validation is reviewable against a tree where the fixtures are still pre-migration. This makes the bisect story clean if a regression surfaces later.
- The action registry refactor (Task 9) is its own commit before the HTTP action mirror (Task 10) so each commit's responsibility is clear: refactor → behaviour change → consumer.
- `SchemaRange` becomes a wire-format DTO via Task 11's emitter. Adding JSON tags is the minimal change to make that contract explicit; no need for a separate DTO type.
- All code follows the in-tree conventions: `log/slog` for the one log line, `common.WriteError` for error responses, `fmt.Errorf("...: %w", err)` for wrap, no `panic`, error codes from `internal/common/error_codes.go`.

---

## Forward-port (after this branch ships)

Once `feature/workflow-schema-version` is merged on `release/v0.7.2` and v0.7.2 is tagged, forward-port to `release/v0.8.0`:

```bash
git fetch origin
git switch release/v0.8.0
git cherry-pick <merge-base..tip-of-v0.7.2-branch>
# Resolve any conflicts in handler.go (v0.8.0 has workflowImportDef
# + DisallowUnknownFields + AllowCycles; the schema-version check
# slots into the same position, just before validateImportRequest).
go test -short ./...
go test ./internal/e2e/...
# Open PR targeting release/v0.8.0.
```

Most touch surfaces (schemaversion.go, schemaversion_test.go, help topic, OpenAPI spec, fixture sweep) are disjoint from in-flight v0.8.0 work. The handler.go conflict is mechanical: v0.8.0's handler is reorganised but the insertion point (post-mode-validation, pre-mutation) is unambiguous.
