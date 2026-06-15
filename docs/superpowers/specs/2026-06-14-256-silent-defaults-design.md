# #256 — Silent-default semantics review (H2 / H5 / M3)

**Issue:** Cyoda-platform/cyoda-go#256
**Branch:** `feat/silent-defaults-256` (off `release/v0.8.0`)
**Milestone:** v0.8.0
**Audit reference:** `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` §H2, §H5, §M3
**Decisions locked:** issue comment 2026-05-19

## Goal

Replace three "succeed silently with a substitute" patterns in the workflow
import / engine paths with explicit, observable, and minimally-invasive
behaviour. The three changes are independent; they ship as three commits on
the same branch under one PR targeting `release/v0.8.0`.

## Non-goals

- **No** workflow-DELETE endpoint. The embedded default workflow
  (`internal/domain/workflow/default_workflow.json`) is the bootstrap for
  never-imported models. After the first successful import, a model carries
  ≥1 workflow for life. "Wipe back to fresh-install state" is not a
  supported operator action; this is intentional per the maintainer
  direction in the May 19 comment.
- **No** `CYODA_WORKFLOW_REQUIRE_MATCH` config flag (explicitly rejected in
  the May 19 comment).
- **No** retroactive validation of existing stored workflows — these are
  request-shape rules, applied at the import boundary.
- **No** SPI changes. `WorkflowDefinition.Active` stays plain `bool`; the
  presence-vs-zero distinction is a handler-layer concern only.

---

## H2 — Honour explicit `active: false`

### Current behaviour

`internal/domain/workflow/handler.go:101-103` unconditionally overwrites
every incoming workflow's `Active` field to `true`. An operator who sends
`"active": false` in the import body silently has it flipped. This breaks
export → re-import idempotency: ACTIVATE mode legitimately produces stored
workflows with `Active=false`, export emits them faithfully, REPLACE
re-import silently re-activates them.

### Target behaviour

- `active` absent from the JSON → default to `true` (preserves out-of-the-box
  contract for clients that don't send the field).
- `active: true` → pass through unchanged.
- `active: false` → pass through unchanged.

### Implementation

Define a handler-local DTO mirroring `spi.WorkflowDefinition` but with
`Active *bool`. Convert the slice to `[]spi.WorkflowDefinition` after JSON
unmarshal, defaulting `nil → true`. The rest of the handler
(`validateImportRequest`, `applyImportMode`, `wfStore.Save`) consumes the
converted slice unchanged.

```go
// in handler.go
type workflowImportDef struct {
    Version      string                         `json:"version"`
    Name         string                         `json:"name"`
    Description  string                         `json:"desc,omitempty"`
    InitialState string                         `json:"initialState"`
    Active       *bool                          `json:"active"`
    Criterion    json.RawMessage                `json:"criterion,omitempty"`
    States       map[string]spi.StateDefinition `json:"states"`
}

type importRequest struct {
    ImportMode string               `json:"importMode"`
    Workflows  []workflowImportDef  `json:"workflows"`
}

// After unmarshal, before validateImportRequest:
incoming := make([]spi.WorkflowDefinition, len(req.Workflows))
for i, w := range req.Workflows {
    active := true
    if w.Active != nil {
        active = *w.Active
    }
    incoming[i] = spi.WorkflowDefinition{
        Version:      w.Version,
        Name:         w.Name,
        Description:  w.Description,
        InitialState: w.InitialState,
        Active:       active,
        Criterion:    w.Criterion,
        States:       w.States,
    }
}
```

**Why a handler-local DTO and not custom UnmarshalJSON / side-channel decoding:**
The SPI `WorkflowDefinition` is shared across all stores (memory, sqlite,
postgres, out-of-tree cassandra). Changing it to `*bool` cascades into four
plugin schemas for what is purely a request-shape concern. The mirror
struct contains the change to `handler.go`.

### Failing tests (RED)

In `internal/domain/workflow/handler_test.go`:

1. `TestImport_ExplicitActiveFalse_Preserved` — POST `{"importMode":"REPLACE","workflows":[{"name":"x", ..., "active":false}]}` against a fresh model; assert `wfStore.Get` returns the workflow with `Active == false`.
2. `TestImport_ActiveAbsent_DefaultsToTrue` — POST the same body without the `active` key; assert `Active == true`.
3. `TestImport_ExplicitActiveTrue_Preserved` — POST with `"active":true`; assert `Active == true`. (Degenerate but pins the contract against future regressions.)
4. `TestExportReimportRoundtrip_PreservesActiveFalse` — seed the store with a workflow at `Active=false`; GET `/export`; POST the exported body to `/import?mode=REPLACE`; assert store row still `Active=false`. This is the regression gate the audit calls out — "export is not a usable backup format" today.

### Cleanup

`internal/domain/workflow/active_default_test.go::TestWorkflowWithoutActiveField_DefaultsToActive`
fakes the handler defaulting inline (`workflows[i].Active = true` in the test
itself). Once the real handler does the defaulting, that test asserts a
behaviour that no longer exists in the handler — it's testing the engine,
not the import path. Delete it; case #2 above subsumes it through the real
HTTP stack.

---

## H5 — Elevate default-workflow fallback to `slog.Warn`

### Current behaviour

Four sites in `engine.go` substitute the embedded default workflow with
only a body-level warning via `common.AddWarning`. Operators see HTTP 200
and entities "still run" — on the default — with no log signal.

| Site | Lines | Cause |
|---|---|---|
| `Execute` cold path | `:127-133` | `wfStore.Get` returned 0 rows (no workflows ever imported for this model) |
| `ManualTransition` cold path | `:229-232` | same |
| `Loopback` cold path | `:311-314` | same |
| `selectWorkflow` no-criterion-match | `:386-391` | rows existed but no criterion matched the entity |

The cold paths share an inaccurate message ("no imported workflow matched
— using default workflow") when in fact no workflows were imported at all.

### Target behaviour

- Keep the existing `common.AddWarning` body warning at every site (it's
  the user-facing channel surfaced in the HTTP response and gRPC
  `warnings` array). Correct the message text per site (see below).
- Additionally emit one `slog.Warn` log line per site with structured
  context. Both fallback causes (cold-path and no-criterion-match) log at
  WARN — operators learn to ignore split levels and the signal value drops.

**Log line shape, uniform across the four sites:**

```go
slog.WarnContext(ctx, "default workflow substituted",
    slog.String("pkg", "workflow"),
    slog.String("tenant", tenantFromContext(ctx)),
    slog.String("entityName", entity.Meta.ModelRef.EntityName),
    slog.String("modelVersion", entity.Meta.ModelRef.ModelVersion),
    slog.String("entityId", entity.Meta.ID),
    slog.String("reason", reason)) // "no_workflows_imported" | "no_criterion_matched"
```

`tenantFromContext` mirrors the helper already in
`internal/domain/search/service.go:592` — `spi.GetUserContext(ctx).Tenant.ID`
or empty when no UserContext is bound. Add a package-local copy in
`engine.go` (or extract to a shared internal helper if no consensus emerges
during implementation; default is local copy, this isn't a refactor task).

### Implementation

Add a small private helper on `Engine`:

```go
func (e *Engine) logDefaultFallback(ctx context.Context, entity *spi.Entity, reason string) {
    slog.WarnContext(ctx, "default workflow substituted",
        slog.String("pkg", "workflow"),
        slog.String("tenant", tenantFromContext(ctx)),
        slog.String("entityName", entity.Meta.ModelRef.EntityName),
        slog.String("modelVersion", entity.Meta.ModelRef.ModelVersion),
        slog.String("entityId", entity.Meta.ID),
        slog.String("reason", reason))
}
```

Update each call site:

- The three cold paths (`:127-133`, `:229-232`, `:311-314`):
  - body warning text → `"no workflows imported for model — using default workflow"`
  - `e.logDefaultFallback(ctx, entity, "no_workflows_imported")`
- `selectWorkflow` (`:386-391`):
  - body warning text → `"no imported workflow matched entity — using default workflow"`
  - `e.logDefaultFallback(ctx, entity, "no_criterion_matched")`

### Failing tests (RED)

In a new `internal/domain/workflow/defaultwf_logging_test.go`:

1. `TestDefaultFallback_NoWorkflowsImported_EmitsSlogWarn` — empty workflow
   store, call `engine.Execute`. Capture slog via a test handler attached
   for the duration of the test. Assert exactly one WARN line with all
   six expected attributes and `reason="no_workflows_imported"`.
2. `TestDefaultFallback_NoCriterionMatched_EmitsSlogWarn` — same setup as
   the existing `defaultwf_fallback_test.go`; assert WARN with
   `reason="no_criterion_matched"`.
3. `TestDefaultFallback_PreservesBodyWarning` — assert that the
   `common.AddWarning` channel is still populated (regression gate against
   inadvertent removal of the user-facing warning when adding the slog
   line).

Test slog handler: grep for an existing pattern first (`slog.Handler`
implementations in `internal/`). If none exists, a thin
`bytes.Buffer`-backed `slog.NewJSONHandler` captured via a
`slog.SetDefault` swap in `t.Cleanup` is sufficient.

### Body warning message change is user-visible

The corrected text is more accurate but changes the wire response. Note in
the release-note entry: "engine fallback warnings now distinguish 'no
workflows imported' from 'no imported workflow matched entity'".

---

## M3 — Reject `workflows: []` in REPLACE / ACTIVATE

### Current behaviour

`import.go:11` (REPLACE) returns `incoming` unchanged. `import.go:13-29`
(ACTIVATE) flips every existing workflow to `Active=false` when no
matching incoming workflow is supplied. Both accept `workflows: []`
without warning, silently wiping or deactivating the operator's imported
workflows. The engine then falls back to the embedded default — the
operator sees HTTP 200 and "the model still works".

### Target behaviour

- REPLACE with `len(workflows) == 0` → HTTP 400, `ErrCodeValidationFailed`.
- ACTIVATE with `len(workflows) == 0` → HTTP 400, `ErrCodeValidationFailed`.
- MERGE with `len(workflows) == 0` → HTTP 200, no-op. (Pinned by test.)
- Default mode (`importMode` absent → MERGE) with empty workflows → no-op.
- The same error code/message applies whether `workflows: []` was sent
  explicitly or the `workflows` key was absent entirely (both yield
  `len() == 0`).

Error detail text:

```
empty workflows array not allowed in REPLACE/ACTIVATE mode — use MERGE if you intended a no-op
```

The May 19 comment's "DELETE the model to wipe" advice is dropped from
the message: `DELETE /model/{e}/{v}` wipes the entire model (descriptor,
schema, entities) — it is not equivalent to "wipe workflows", and
recommending it would mislead operators.

### Implementation

The guard lives in `handler.go`, immediately before the H2 conversion
loop (i.e. after mode parsing, model-existence check, and workflow-store
acquisition; before `validateImportRequest`).

```go
// M3 — empty workflows array is silently destructive in REPLACE/ACTIVATE;
// reject it explicitly. MERGE-empty is a legitimate no-op and stays allowed.
if len(req.Workflows) == 0 && (mode == "REPLACE" || mode == "ACTIVATE") {
    common.WriteError(w, r, common.Operational(
        http.StatusBadRequest,
        common.ErrCodeValidationFailed,
        "empty workflows array not allowed in REPLACE/ACTIVATE mode — use MERGE if you intended a no-op"))
    return
}
```

**Why in `handler.go` and not `applyImportMode`:** `applyImportMode` is a
pure pre-existing slice transform with no HTTP concerns; embedding
`common.Operational` errors there couples merge logic to the wire
protocol. The handler is already the HTTP gate.

### Failing tests (RED)

In `internal/domain/workflow/handler_test.go`:

1. `TestImport_EmptyArrayReplace_Rejected` — POST `{"importMode":"REPLACE","workflows":[]}`; assert 400 + `ErrCodeValidationFailed` + expected detail.
2. `TestImport_EmptyArrayActivate_Rejected` — same for `"ACTIVATE"`.
3. `TestImport_MissingWorkflowsKeyReplace_Rejected` — POST `{"importMode":"REPLACE"}` (no workflows key); assert 400 (same error). Pins that "absent key" and `[]` are equivalent.
4. `TestImport_EmptyArrayMerge_NoOp` — pre-seed store with two workflows; POST `{"importMode":"MERGE","workflows":[]}`; assert 200; assert `wfStore.Get` returns the same two workflows unchanged. Pins the MERGE carve-out.
5. `TestImport_EmptyArrayDefaultMode_NoOp` — POST `{"workflows":[]}` (mode defaults to MERGE); assert 200, store unchanged.

---

## Sequencing & commits

Three commits on `feat/silent-defaults-256`:

1. `feat(workflow): honour explicit active=false at import (H2)` — DTO swap + tests #H2.1–4 + delete `active_default_test.go`'s obsoleted test.
2. `feat(workflow): emit slog.Warn on default-workflow substitution (H5)` — helper + 4 call sites + tests #H5.1–3 + message-text corrections.
3. `feat(workflow): reject empty workflows array in REPLACE/ACTIVATE (M3)` — guard + tests #M3.1–5.

Order is independent; this order minimises churn (H2 only touches the
handler; H5 only touches the engine; M3 only touches the handler).

Each commit is RED → GREEN per `.claude/rules/tdd.md`: write the failing
tests, verify they fail, implement, verify they pass, run the full
workflow package, move on.

## Verification

Per Gate 5 (`CLAUDE.md`):
- `go test ./internal/domain/workflow/... -v` green after each commit.
- `go test -short ./... -v` green before PR.
- `go test ./internal/e2e/... -v` green before PR (E2E exercises import via
  HTTP, so H2 and M3 surface there if anything regresses).
- `go vet ./...` clean.
- `go test -race ./...` one-shot before PR.

## Release-note entries

Three bullets under v0.8.0:

- **workflow/import: explicit `"active": false` is now honoured.** Previously,
  the import handler force-overrode every incoming workflow's `active` field
  to `true`. Operators can now stage inactive workflows via REPLACE without
  the system silently re-activating them. Export → re-import via REPLACE is
  now an idempotent round-trip.
- **workflow/engine: default-workflow substitution is logged at WARN.** When
  the engine falls back to the embedded default workflow (either because
  no workflow was imported for the model, or because no imported
  workflow's criterion matched the entity), a structured `slog.Warn` line
  is now emitted in addition to the existing body warning. Body warning
  text now distinguishes the two causes.
- **workflow/import: empty `workflows: []` in REPLACE/ACTIVATE is now rejected.**
  Previously these modes silently wiped or deactivated all stored
  workflows for the model, falling back to the embedded default at runtime
  with no error. They now return HTTP 400 with `VALIDATION_FAILED`. MERGE
  with empty stays a no-op.

## Documentation hygiene

Per Gate 4 — files to check while implementing:
- `README.md` — no changes expected (workflow import not documented here).
- `cmd/cyoda/help/content/` — check whether any help topic mentions
  workflow import / active-field semantics. If so, update.
- `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` — leave as-is; it's the historical
  audit document, not a living spec.
- `CHANGELOG.md` / release notes file — add the three entries above.

## Out of scope

- A first-class workflow-wipe channel (DELETE endpoint or SPI method). Not
  needed: the embedded default is the cold-start path; once imported,
  always ≥1 workflow.
- Static state-graph validation hardening beyond #255 (already merged).
- `DisallowUnknownFields` enforcement (#145 will absorb the workflow
  handler scope).
- Cyoda-cloud parity validation. Per the maintainer's direction in the
  May 19 comment: sensible defaults land in cyoda-go; Cloud follows.
