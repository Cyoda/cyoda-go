# Issue #259 — Scheduled-transition configuration shape + SPI — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the scheduled-transition configuration shape (SPI type, OpenAPI surface, validator rules, engine guards) plus a request-level `allowCycles` escape hatch — all per the rev 3 spec at `docs/superpowers/specs/2026-06-15-issue-259-scheduled-transition-shape-design.md`. Timer runtime stays deferred to #251.

**Architecture:** Two coordinated PRs.
1. **cyoda-go-spi PR** (sibling repo at `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`) — additive SPI types: new `TransitionSchedule` struct, new `Schedule *TransitionSchedule` field on `TransitionDefinition`, plus a docstring update on `ProcessorDefinition.Type` deferred from #250.
2. **cyoda-go PR** (this worktree) — pseudo-version pin bump to SPI main HEAD, OpenAPI surface update, validator rules, engine cascade-skip + explicit-fire-reject guards, `allowCycles` request envelope, help-topic, parity scenarios, E2E.

**Tech Stack:** Go 1.26, `log/slog`, `encoding/json`, `errors.Is`/wrap, `oapi-codegen` (for `api/generated.go`), testcontainers-go (for E2E), `httptest` (for in-process E2E HTTP server).

---

## Spec cross-reference

This plan executes the spec sections in this order:

| Plan phase | Spec section | What |
|---|---|---|
| Phase 1 (Tasks 1–4) | §5.3 | cyoda-go-spi PR |
| Phase 2 (Task 5) | §10 step 2a | SPI pin bump |
| Phase 3 (Tasks 6–7) | §5.1, §5.2 | OpenAPI + regen |
| Phase 4 (Tasks 8–10) | §5.4 (Schedule rules) | Validator |
| Phase 5 (Tasks 11–12) | §5.4 (`allowCycles`) | Import envelope |
| Phase 6 (Task 13) | §5.5(a) | Cascade-skip |
| Phase 7 (Task 14) | §5.5(b) | Explicit-fire reject |
| Phase 8 (Task 15) | §6.1 test 8 | Round-trip |
| Phase 9 (Task 16) | §6.1 test 9 | E2E |
| Phase 10 (Task 17) | §5.7 | Parity scenarios |
| Phase 11 (Task 18) | §5.6 | Help-topic |
| Phase 12 (Task 19) | Appendix B | Verification + open PR |

---

## File map

### cyoda-go-spi (sibling repo)

- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types.go` — add `TransitionSchedule` type (after `ProcessorConfig`), add `Schedule` field on `TransitionDefinition` (line 130-137), add docstring on `ProcessorDefinition.Type` (line 140).
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types_test.go` — round-trip tests for `TransitionSchedule` and `TransitionDefinition.Schedule`.
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/CHANGELOG.md` — `[Unreleased]` Added + Changed entries.

### cyoda-go (this worktree at `/Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/259-scheduled-transition-config-shape/`)

- Modify: `go.mod` + `plugins/memory/go.mod` + `plugins/sqlite/go.mod` + `plugins/postgres/go.mod` — bump cyoda-go-spi pseudo-version.
- Modify: `api/openapi.yaml` — add `TransitionScheduleDto` schema, add `schedule` property to `TransitionDefinitionDto`, add `allowCycles` to the workflow-import request body schema.
- Modify: `api/generated.go` — regenerated.
- Modify: `internal/domain/workflow/handler.go` — `AllowCycles bool` on `importRequest` (lines 60-64); thread to `validateWorkflows`; `slog.Warn` when true.
- Modify: `internal/domain/workflow/validate.go` — two Schedule-coherence checks in `validateWorkflowStructure` per-transition loop (between Next-state check at lines 193-196 and processor loop at 198); `validateWorkflows` signature gets `allowCycles bool` parameter and short-circuits `validateWorkflowLoops` when true.
- Modify: `internal/domain/workflow/scenarios_test.go` — update 8 `validateWorkflows` call sites for the new signature.
- Modify: `internal/domain/workflow/engine.go` — cascade-skip filter at line 528 (one-line addition); explicit-fire reject branch in `attemptTransition` immediately after the `Disabled` check at lines 449-453.
- Create or modify: `internal/domain/workflow/validate_test.go` or sibling — Schedule-rule unit tests, `allowCycles` tests.
- Create or modify: `internal/domain/workflow/engine_test.go` or sibling — cascade-skip + explicit-fire-reject + round-trip tests.
- Modify: `internal/e2e/` (pick the existing workflow-import E2E file or add a new one named after the issue) — E2E test for explicit-fire of scheduled transition.
- Modify: `e2e/externalapi/scenarios/08-workflow-import-export.yaml` — append `wf-import/07-scheduled-transition-roundtrip`, `wf-import/08-scheduled-transition-rejects`, `wf-import/09-allowcycles-bypass`.
- Modify: `cmd/cyoda/help/content/workflows.md` — add `## SCHEDULED TRANSITIONS` section (level 2) after the `## PROCESSORS` section, before `## CRITERIA`.

---

# Phase 1 — cyoda-go-spi PR

> **Working directory for this phase:** `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`.
> Create a feature branch off `origin/main`; the SPI PR will target SPI `main`.

## Task 1: Create SPI feature branch and add `TransitionSchedule` type

**Files:**
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types.go`
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types_test.go`

- [ ] **Step 1: Create the feature branch**

Run from `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git fetch origin
git checkout -b feat/scheduled-transition-shape origin/main
```

Expected: `Switched to a new branch 'feat/scheduled-transition-shape'`.

- [ ] **Step 2: Write failing round-trip test for `TransitionSchedule`**

Append to `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types_test.go`:

```go
func TestTransitionSchedule_RoundTrips(t *testing.T) {
	// Non-nil positive TimeoutMs round-trips byte-equivalent.
	tm := int64(5000)
	sched := TransitionSchedule{DelayMs: 1000, TimeoutMs: &tm}
	bs, err := json.Marshal(sched)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bs), `"delayMs":1000`) {
		t.Errorf("missing delayMs: %s", bs)
	}
	if !strings.Contains(string(bs), `"timeoutMs":5000`) {
		t.Errorf("missing timeoutMs: %s", bs)
	}

	var back TransitionSchedule
	if err := json.Unmarshal(bs, &back); err != nil {
		t.Fatal(err)
	}
	if back.DelayMs != 1000 {
		t.Errorf("DelayMs round-trip lost value: got %d", back.DelayMs)
	}
	if back.TimeoutMs == nil || *back.TimeoutMs != 5000 {
		t.Errorf("TimeoutMs round-trip lost value: got %v", back.TimeoutMs)
	}

	// Non-nil zero TimeoutMs (strictest semantic) survives omitempty.
	zero := int64(0)
	schedZero := TransitionSchedule{DelayMs: 1000, TimeoutMs: &zero}
	bs2, _ := json.Marshal(schedZero)
	if !strings.Contains(string(bs2), `"timeoutMs":0`) {
		t.Errorf("non-nil zero TimeoutMs should marshal as timeoutMs:0, got %s", bs2)
	}
	var back2 TransitionSchedule
	_ = json.Unmarshal(bs2, &back2)
	if back2.TimeoutMs == nil || *back2.TimeoutMs != 0 {
		t.Errorf("non-nil zero TimeoutMs round-trip dropped distinction: %v", back2.TimeoutMs)
	}

	// Nil TimeoutMs (no-timeout semantic) does not marshal.
	schedNil := TransitionSchedule{DelayMs: 1000}
	bs3, _ := json.Marshal(schedNil)
	if strings.Contains(string(bs3), "timeoutMs") {
		t.Errorf("nil TimeoutMs should be omitted, got %s", bs3)
	}
	var back3 TransitionSchedule
	_ = json.Unmarshal(bs3, &back3)
	if back3.TimeoutMs != nil {
		t.Errorf("nil TimeoutMs round-trip surfaced a non-nil pointer: %v", back3.TimeoutMs)
	}
}
```

- [ ] **Step 3: Run the test to confirm it fails**

Run:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
go test -run TestTransitionSchedule_RoundTrips -v .
```

Expected: FAIL with `undefined: TransitionSchedule`.

- [ ] **Step 4: Add the `TransitionSchedule` type**

Edit `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types.go` — insert immediately after the `ProcessorConfig` struct (after line 161, before any `SMEvent*` constants):

```go
// TransitionSchedule configures automatic firing of a future state
// transition. Presence of this struct on a TransitionDefinition marks
// the transition as scheduled.
//
// Semantics. The scheduled execution time of the transition is
// scheduledTime = stateEntryTime + DelayMs. When the scheduler picks
// the task up at executionTime, it computes
// lateness = executionTime - scheduledTime.
//   - If TimeoutMs is nil, the task is always attempted (no timeout).
//   - If TimeoutMs is non-nil and lateness > *TimeoutMs, the task is
//     dropped and the transition is NOT attempted.
//   - If TimeoutMs is non-nil and lateness <= *TimeoutMs (including
//     *TimeoutMs == 0 when lateness is 0), the transition fires.
//
// TimeoutMs gives operators control over how the system handles
// backlog and intermittent-offline conditions. Short positive values
// prefer freshness — stale tasks are discarded rather than fired
// against a possibly-changed entity. Nil prefers eventual execution.
//
// Scheduled transitions are mutually exclusive with Manual=true.
//
// Scheduled transitions are a special case of a generic ScheduledTask
// abstraction. The lateness-tolerance concept (TimeoutMs) applies
// uniformly across all ScheduledTask variants. The generic
// abstraction and the runtime that implements it ship in a later
// release; until then, the cyoda-go engine silently skips scheduled
// transitions during automated cascade selection, and rejects explicit
// fires by name with HTTP 400 TRANSITION_NOT_FOUND.
type TransitionSchedule struct {
	// DelayMs is the delay between source-state entry and the
	// scheduled execution time, in milliseconds. Must be > 0.
	DelayMs int64 `json:"delayMs"`

	// TimeoutMs is the late-tolerance window past the scheduled
	// execution time, in milliseconds. Nil means no timeout — the
	// task fires whenever the scheduler eventually picks it up.
	// Non-nil zero is the strictest setting — drop on any lateness.
	// Non-nil positive N drops the task if it picks up more than N
	// milliseconds after scheduledTime. Independent of DelayMs; the
	// two measure different quantities.
	TimeoutMs *int64 `json:"timeoutMs,omitempty"`
}
```

- [ ] **Step 5: Run the test to confirm it passes**

Run:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
go test -run TestTransitionSchedule_RoundTrips -v .
```

Expected: PASS.

- [ ] **Step 6: Run the full SPI test suite to confirm no regressions**

Run:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
go test ./... -v
```

Expected: all green.

- [ ] **Step 7: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add types.go types_test.go
git commit -m "feat(types): add TransitionSchedule struct for scheduled-transition shape

New SPI type for the scheduled-transition configuration shape carve-out
(cyoda-go #259). DelayMs (required, >0) is the delay from state-entry
to scheduled execution time. TimeoutMs (*int64, optional) is the
late-tolerance window past scheduled time — nil means no timeout, &0
is the strictest setting, &N drops if late > N ms.

Runtime not yet wired — see cyoda-go #251 for full feature tracking.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Add `Schedule` field to `TransitionDefinition`

**Files:**
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types.go` — `TransitionDefinition` struct at lines 130-137.
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types_test.go` — round-trip test for the new field.

- [ ] **Step 1: Write failing round-trip test for `TransitionDefinition.Schedule`**

Append to `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types_test.go`:

```go
func TestTransitionDefinition_Schedule_RoundTrips(t *testing.T) {
	tm := int64(5000)
	tr := TransitionDefinition{
		Name:     "AutoClose",
		Next:     "Closed",
		Manual:   false,
		Schedule: &TransitionSchedule{DelayMs: 86400000, TimeoutMs: &tm},
	}
	bs, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bs), `"schedule":{"delayMs":86400000,"timeoutMs":5000}`) {
		t.Errorf("schedule field missing or wrong shape: %s", bs)
	}

	var back TransitionDefinition
	if err := json.Unmarshal(bs, &back); err != nil {
		t.Fatal(err)
	}
	if back.Schedule == nil {
		t.Fatalf("Schedule round-trip dropped the field: %+v", back)
	}
	if back.Schedule.DelayMs != 86400000 {
		t.Errorf("Schedule.DelayMs lost value: got %d", back.Schedule.DelayMs)
	}
	if back.Schedule.TimeoutMs == nil || *back.Schedule.TimeoutMs != 5000 {
		t.Errorf("Schedule.TimeoutMs lost value: got %v", back.Schedule.TimeoutMs)
	}

	// Schedule omitted is preserved as nil through round-trip.
	trNoSched := TransitionDefinition{Name: "Foo", Next: "Bar"}
	bs2, _ := json.Marshal(trNoSched)
	if strings.Contains(string(bs2), "schedule") {
		t.Errorf("nil Schedule should be omitted, got %s", bs2)
	}
	var back2 TransitionDefinition
	_ = json.Unmarshal(bs2, &back2)
	if back2.Schedule != nil {
		t.Errorf("Schedule round-trip surfaced a non-nil pointer for absent field: %v", back2.Schedule)
	}
}
```

- [ ] **Step 2: Run the test to confirm it fails**

Run:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
go test -run TestTransitionDefinition_Schedule_RoundTrips -v .
```

Expected: FAIL with `unknown field Schedule in struct literal of type TransitionDefinition`.

- [ ] **Step 3: Add the `Schedule` field**

Edit `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types.go` — change `TransitionDefinition` to add `Schedule` as the last field:

```go
// TransitionDefinition represents a single transition from a state.
type TransitionDefinition struct {
	Name       string                `json:"name"`
	Next       string                `json:"next"`
	Manual     bool                  `json:"manual"`
	Disabled   bool                  `json:"disabled,omitempty"`
	Criterion  json.RawMessage       `json:"criterion,omitempty"`
	Processors []ProcessorDefinition `json:"processors,omitempty"`
	Schedule   *TransitionSchedule   `json:"schedule,omitempty"`
}
```

- [ ] **Step 4: Run the test to confirm it passes**

Run:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
go test -run TestTransitionDefinition_Schedule_RoundTrips -v .
```

Expected: PASS.

- [ ] **Step 5: Run the full SPI test suite**

Run:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
go test ./... -v
```

Expected: all green.

- [ ] **Step 6: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add types.go types_test.go
git commit -m "feat(types): add TransitionDefinition.Schedule field

Optional *TransitionSchedule pointer on TransitionDefinition. Presence
marks the transition as scheduled. Pointer (not value) so absent
vs. zero is distinguishable on the wire — supports the round-trip
fidelity contract per cyoda-go #259 spec §5.3.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Add deferred docstring on `ProcessorDefinition.Type`

**Files:**
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types.go` — `ProcessorDefinition` struct at lines 140-145.

This change was deferred from cyoda-go #250 spec §5.3, intentionally held back for the first substantive SPI carve-out — that's #259. **First check whether `origin/main` already carries the docstring** (per spec §8 O1 risk-bullet): if #260's SPI PR landed first, this docstring is already on `main` and this task is a no-op.

- [ ] **Step 1: Check whether `origin/main` already has the docstring**

Run:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git fetch origin
git show origin/main:types.go | grep -A 3 "type ProcessorDefinition struct"
```

If the output shows a docstring above the `Type` field starting with `// Type is the execution-location axis`, the docstring is already on main — **skip the rest of this task entirely**, jump to Task 4. Otherwise proceed.

- [ ] **Step 2: Add the docstring**

Edit `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/types.go` — replace the `ProcessorDefinition` struct (lines 139-145) with:

```go
// ProcessorDefinition represents a processor attached to a transition.
type ProcessorDefinition struct {
	// Type is the execution-location axis. Recognised values are defined
	// by the cyoda-go engine package; canonical values are "externalized"
	// (dispatched via gRPC to a calculation node selected by
	// Config.CalculationNodesTags) and "internalized" (reserved for an
	// in-process execution location, currently rejected at engine
	// dispatch as not yet implemented). Empty is treated as "externalized".
	// Any value other than "internalized" falls through to the
	// ExecutionMode dispatch path; import-time validation does not
	// constrain this field.
	Type          string          `json:"type"`
	Name          string          `json:"name"`
	ExecutionMode string          `json:"executionMode,omitempty"`
	Config        ProcessorConfig `json:"config,omitempty"`
}
```

- [ ] **Step 3: Verify the SPI test suite still passes**

Run:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
go test ./... -v
```

Expected: all green (docstring-only change should not affect any test).

- [ ] **Step 4: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add types.go
git commit -m "docs(types): document ProcessorDefinition.Type as execution-location axis

Deferred from cyoda-go #250 spec §5.3 — intentionally held back for the
first substantive SPI carve-out. Documents Type as the
execution-location axis with values 'externalized' / 'internalized',
matches the engine's tolerance behaviour exactly (only 'internalized'
is rejected; unknown values fall through to ExecutionMode dispatch).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: CHANGELOG entries, push, open SPI PR

**Files:**
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/CHANGELOG.md` — `[Unreleased]` section.

- [ ] **Step 1: Update CHANGELOG**

Edit `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/CHANGELOG.md` — under the `[Unreleased]` section's existing `### Added` block, append:

```markdown
- `TransitionSchedule` type + `TransitionDefinition.Schedule` field for
  the scheduled-transition shape carve-out (cyoda-go #259). The new
  type carries `DelayMs` (required, >0) and `TimeoutMs *int64`
  (optional; nil = no timeout, &0 = strictest, &N = drop if late > N
  ms). Runtime not yet wired — see cyoda-go #251 for full feature
  tracking.
```

If Task 3 was NOT skipped (i.e. you added the docstring), also add a new `### Changed` subsection under `[Unreleased]` (after the Added block, before any existing Notes block):

```markdown
### Changed

- Document `ProcessorDefinition.Type` field as the execution-location
  axis (deferred from cyoda-go #250 per its spec §5.3, intentionally
  bundled with the first substantive SPI carve-out — that is cyoda-go
  #259).
```

If Task 3 was skipped (docstring already on main), do NOT add the Changed block — it would create a duplicate entry.

- [ ] **Step 2: Commit the CHANGELOG**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add CHANGELOG.md
git commit -m "chore(changelog): record TransitionSchedule additions for cyoda-go #259

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

- [ ] **Step 3: Push the branch**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git push -u origin feat/scheduled-transition-shape
```

- [ ] **Step 4: Open the SPI PR**

Run:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
gh pr create --base main --head feat/scheduled-transition-shape \
  --title "feat(types): scheduled-transition shape" \
  --body "$(cat <<'EOF'
Additive SPI change for the cyoda-go v0.8.0 scheduled-transition shape
carve-out.

## What this adds

- New \`TransitionSchedule\` type carrying \`DelayMs int64\` (required, >0)
  and \`TimeoutMs *int64\` (optional pointer). Semantics:
    - \`TimeoutMs == nil\` → no timeout, task fires whenever the scheduler
      eventually picks it up.
    - \`TimeoutMs == &0\` → strictest, drop on any lateness past scheduled
      time.
    - \`TimeoutMs == &N\` (N > 0) → drop if \`lateness > N ms\` where
      \`lateness = executionTime - (stateEntryTime + DelayMs)\`.
- New \`TransitionDefinition.Schedule *TransitionSchedule\` field
  (optional). Presence marks the transition as scheduled.
- Docstring on \`ProcessorDefinition.Type\` (deferred from cyoda-go #250
  spec §5.3). Skip if it landed via a sibling carve-out first.

## What this does NOT do

- No runtime — the timer that arms and fires scheduled transitions is
  cyoda-go #251 (deferred beyond v0.8.0).
- No new \`SMEventScheduledTransition*\` events — those ship with #251.
- No tag — bundled into v0.8.0's end-of-milestone tag per MAINTAINING.md
  coordinated-release procedure.

## Cross-repo

- Consumer: cyoda-go #259 (this carve-out).
- Parent feature (deferred): cyoda-go #251.

## Reference

cyoda-go spec: \`docs/superpowers/specs/2026-06-15-issue-259-scheduled-transition-shape-design.md\` rev 3.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Expected: PR URL printed.

- [ ] **Step 5: Pause for human review of the SPI PR**

> **CHECKPOINT.** Do NOT continue to Phase 2 until the SPI PR is merged to `cyoda-go-spi` `main`. The cyoda-go pseudo-version pin (Phase 2) requires the merge commit SHA. Manual operator step.

---

# Phase 2 — cyoda-go pseudo-version pin refresh

> **Working directory from here on:** the cyoda-go worktree at
> `/Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/259-scheduled-transition-config-shape/`.

## Task 5: Bump cyoda-go-spi pin across all four `go.mod` files

**Files:**
- Modify: `go.mod`
- Modify: `plugins/memory/go.mod`
- Modify: `plugins/sqlite/go.mod`
- Modify: `plugins/postgres/go.mod`

- [ ] **Step 1: Confirm the SPI PR was merged and capture the new HEAD SHA**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git fetch origin
git rev-parse --short origin/main
```

Expected: short SHA of the new SPI `main` HEAD (the SPI PR's merge commit). Record it — call it `$SPISHA`.

- [ ] **Step 2: Update root `go.mod` with `go get`**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/259-scheduled-transition-config-shape
GOPRIVATE=github.com/Cyoda-platform/* go get github.com/cyoda-platform/cyoda-go-spi@main
go mod tidy
```

Expected: `go.mod` and `go.sum` updated with a fresh pseudo-version (the SHA prefix should match `$SPISHA`).

- [ ] **Step 3: Update each plugin submodule**

For each of `plugins/memory`, `plugins/sqlite`, `plugins/postgres`:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/259-scheduled-transition-config-shape/plugins/memory
GOPRIVATE=github.com/Cyoda-platform/* go get github.com/cyoda-platform/cyoda-go-spi@main
go mod tidy

cd ../sqlite
GOPRIVATE=github.com/Cyoda-platform/* go get github.com/cyoda-platform/cyoda-go-spi@main
go mod tidy

cd ../postgres
GOPRIVATE=github.com/Cyoda-platform/* go get github.com/cyoda-platform/cyoda-go-spi@main
go mod tidy
```

- [ ] **Step 4: Verify all four `go.mod` files pin the same pseudo-version**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/259-scheduled-transition-config-shape
grep "cyoda-go-spi v" go.mod plugins/*/go.mod
```

Expected: identical pseudo-version string across all four files, containing the `$SPISHA` SHA fragment.

- [ ] **Step 5: Build everything to confirm the new SPI compiles**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/259-scheduled-transition-config-shape
go build ./...
```

Expected: clean build.

- [ ] **Step 6: Run short tests to confirm no regression from the pin bump**

```bash
go test -short ./...
```

Expected: all green.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum plugins/*/go.mod plugins/*/go.sum
git commit -m "chore(deps): bump cyoda-go-spi pin for #259 SPI types

Picks up the bundled TransitionSchedule type + TransitionDefinition.Schedule
field for the scheduled-transition shape carve-out (closes #259 in this
repo), plus the deferred #250 ProcessorDefinition.Type docstring.

Per cyoda-go-spi MAINTAINING.md coordinated-release procedure: no SPI
tag was cut — bundled into v0.8.0's end-of-milestone tag. All four
go.mod files (root + plugins/memory|sqlite|postgres) bump to the same
pseudo-version pinned to SPI main HEAD.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 3 — OpenAPI surface

## Task 6: Add `TransitionScheduleDto` schema + `schedule` on `TransitionDefinitionDto` + `allowCycles` on import body

**Files:**
- Modify: `api/openapi.yaml`

- [ ] **Step 1: Add `TransitionScheduleDto` schema**

In `api/openapi.yaml`, add a new schema definition near the existing `TransitionDefinitionDto` (insert in alphabetical order under `components.schemas`). The exact insertion point is just before `TransitionDefinitionDto`:

```yaml
    TransitionScheduleDto:
      type: object
      description: |
        Scheduling configuration for an automatic future state transition.
        Presence of this object marks the parent transition as scheduled.
        The transition is scheduled to fire at `stateEntryTime + delayMs`
        (the "scheduledTime"). When the scheduler picks the task up for
        execution at `executionTime`, it computes `lateness = executionTime
        - scheduledTime`; if `lateness > timeoutMs` the task is dropped
        and the transition is NOT attempted. `timeoutMs` gives operators
        control over how the system should handle backlog or intermittent-
        offline conditions: short timeoutMs values prefer freshness over
        eventual execution; absent timeoutMs always eventually fires.

        Mutually exclusive with parent transition's manual=true.

        The runtime that arms and fires the timer is not yet implemented.
        Until it ships, the engine silently skips scheduled transitions
        during automated cascade selection, and explicit fires by name are
        rejected with HTTP 400 TRANSITION_NOT_FOUND.
      properties:
        delayMs:
          type: integer
          format: int64
          minimum: 1
          description: |
            Delay between source-state entry and the scheduled execution
            time, in milliseconds. Must be a positive integer (zero or
            negative would make this a regular automated transition).
        timeoutMs:
          type: integer
          format: int64
          minimum: 0
          description: |
            Late-tolerance window past the scheduled execution time, in
            milliseconds. When the scheduler picks the task up, if
            `(executionTime - scheduledTime) > timeoutMs` the task is
            dropped. Absent (omitted) means no timeout — the task fires
            whenever the scheduler eventually picks it up. Explicit zero
            is the strictest setting — drop on any lateness. Independent
            of `delayMs`; the two measure different quantities.
      required:
        - delayMs
```

- [ ] **Step 2: Add `schedule` to `TransitionDefinitionDto.properties`**

Locate `TransitionDefinitionDto` in `api/openapi.yaml`. Add to its `properties` block, after the existing `processors` property:

```yaml
        schedule:
          $ref: "#/components/schemas/TransitionScheduleDto"
          description: |
            Optional scheduling configuration. Presence marks the transition as
            scheduled; mutually exclusive with manual=true. Runtime not yet
            implemented — see TransitionScheduleDto for engine behaviour.
```

Do NOT add `schedule` to `required`.

- [ ] **Step 3: Add `allowCycles` to the workflow-import request body schema**

Find the request body schema for `POST /model/{entityName}/{modelVersion}/workflow/import`. Confirm the schema name (likely `WorkflowImportRequestDto` or similar — search for it):

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/259-scheduled-transition-config-shape
grep -n -B 1 -A 3 "workflow/import" api/openapi.yaml | head -30
```

Then add to the request body schema's `properties` (after the existing `importMode` / `workflows` properties):

```yaml
        allowCycles:
          type: boolean
          default: false
          description: |
            When true, bypasses the import-time check that rejects workflows
            containing unguarded automated cycles (transitions with manual=false
            and no criterion that form a closed loop). The runtime cascade
            safety net — per-state visit cap and total cascade depth limit —
            remains in effect and catches actual runaway at fire time. Use
            when the cyclicity is intentional, e.g. polling patterns built on
            scheduled transitions. Default false preserves the pre-#259
            rejection behaviour exactly.
```

Do NOT add to `required`.

- [ ] **Step 4: Commit the OpenAPI changes**

```bash
git add api/openapi.yaml
git commit -m "feat(openapi): add TransitionScheduleDto + schedule + allowCycles

Schema work for #259:
- New TransitionScheduleDto with delayMs (required, >0) and timeoutMs
  (optional, >=0). Description captures the late-tolerance-past-
  scheduled-time semantic agreed with the project lead.
- Optional schedule property on TransitionDefinitionDto, presence
  marks the transition as scheduled (mutually exclusive with
  manual=true).
- Optional allowCycles boolean on the workflow-import request body,
  default false, request-level escape hatch for unguarded automated
  cycles.

Runtime in-place engine guards land in subsequent commits; the
generated DTOs come from the next make generate run.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: Regenerate `api/generated.go`

**Files:**
- Modify: `api/generated.go`

- [ ] **Step 1: Run the codegen target**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/259-scheduled-transition-config-shape
make generate
```

Expected: succeeds with `api/generated.go` updated. If the target name differs, run:

```bash
grep -n "generate:" Makefile
```

…and use whatever target invokes `oapi-codegen`.

- [ ] **Step 2: Verify the new DTOs are present**

```bash
grep -n "TransitionScheduleDto\|AllowCycles\|Schedule " api/generated.go | head -20
```

Expected output should contain:
- `type TransitionScheduleDto struct {` (or similar).
- `Schedule *TransitionScheduleDto` on `TransitionDefinitionDto`.
- `AllowCycles` field on the import request body type.

- [ ] **Step 3: Verify the build is clean**

```bash
go build ./...
```

Expected: clean build.

- [ ] **Step 4: Verify short tests still pass**

```bash
go test -short ./...
```

Expected: all green (the codegen alone changes no behaviour).

- [ ] **Step 5: Commit**

```bash
git add api/generated.go
git commit -m "chore(api): regenerate DTOs for #259 schema additions

Regenerated from the updated api/openapi.yaml. Adds:
- TransitionScheduleDto struct with DelayMs int64 + TimeoutMs *int64.
- Schedule *TransitionScheduleDto field on TransitionDefinitionDto.
- AllowCycles bool on the workflow-import request body type.

No behavioural change — just generator output.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 4 — Validator (TDD)

## Task 8: Validator rule — `Manual && Schedule != nil` rejection

**Files:**
- Modify: `internal/domain/workflow/validate.go` — `validateWorkflowStructure` per-transition loop between lines 193 and 198.
- Create or modify: `internal/domain/workflow/validate_test.go` (or whichever existing file holds validator tests — check with `grep -ln 'validateImportRequest\|validateWorkflowStructure' internal/domain/workflow/*_test.go`).

- [ ] **Step 1: Write failing test for manual+schedule rejection**

Add to the validator test file:

```go
func TestValidator_ManualAndSchedule_Rejected(t *testing.T) {
	tm := int64(1000)
	wf := spi.WorkflowDefinition{
		Version:      "1",
		Name:         "test",
		InitialState: "S1",
		Active:       true,
		States: map[string]spi.StateDefinition{
			"S1": {
				Transitions: []spi.TransitionDefinition{
					{
						Name:     "T1",
						Next:     "S1",
						Manual:   true,
						Schedule: &spi.TransitionSchedule{DelayMs: 1000, TimeoutMs: &tm},
					},
				},
			},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil {
		t.Fatal("expected validation error for manual=true + schedule, got nil")
	}
	if !strings.Contains(err.Error(), "manual and scheduled are mutually exclusive") {
		t.Errorf("error message missing expected substring; got: %v", err)
	}
}
```

If the test file does not exist, create `internal/domain/workflow/validate_test.go` with:

```go
package workflow

import (
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi"
)
```

…and the test function above.

- [ ] **Step 2: Run the test to confirm it fails**

```bash
go test -run TestValidator_ManualAndSchedule_Rejected -v ./internal/domain/workflow/
```

Expected: FAIL with `expected validation error for manual=true + schedule, got nil` (the validator currently accepts the shape).

- [ ] **Step 3: Add the rule in `validateWorkflowStructure`**

Edit `internal/domain/workflow/validate.go`. Find the per-transition loop (around lines 178-212). Between the Next-state existence check (lines 193-196) and the processor loop (line 198), insert:

```go
			if tr.Manual && tr.Schedule != nil {
				return fmt.Errorf(
					"workflow %q state %q transition %q: manual and scheduled are mutually exclusive",
					wf.Name, stateName, tr.Name)
			}
```

- [ ] **Step 4: Run the test to confirm it passes**

```bash
go test -run TestValidator_ManualAndSchedule_Rejected -v ./internal/domain/workflow/
```

Expected: PASS.

- [ ] **Step 5: Run the full package tests to confirm no regression**

```bash
go test -short ./internal/domain/workflow/...
```

Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/workflow/validate.go internal/domain/workflow/validate_test.go
git commit -m "feat(workflow): reject manual+schedule transitions at import (#259)

A scheduled transition fires automatically on a timer; declaring it
manual at the same time is a config contradiction. validateWorkflowStructure
rejects the combination with VALIDATION_FAILED 400.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 9: Validator rule — `Schedule != nil && DelayMs <= 0` rejection

**Files:**
- Modify: `internal/domain/workflow/validate.go`
- Modify: validator test file from Task 8.

- [ ] **Step 1: Write failing tests for `DelayMs <= 0`**

Append to the validator test file:

```go
func TestValidator_DelayMsZero_Rejected(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version:      "1",
		Name:         "test",
		InitialState: "S1",
		Active:       true,
		States: map[string]spi.StateDefinition{
			"S1": {
				Transitions: []spi.TransitionDefinition{
					{
						Name:     "T1",
						Next:     "S1",
						Manual:   false,
						Schedule: &spi.TransitionSchedule{DelayMs: 0},
					},
				},
			},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil || !strings.Contains(err.Error(), "delayMs must be > 0") {
		t.Errorf("expected delayMs error, got: %v", err)
	}
}

func TestValidator_DelayMsNegative_Rejected(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version:      "1",
		Name:         "test",
		InitialState: "S1",
		Active:       true,
		States: map[string]spi.StateDefinition{
			"S1": {
				Transitions: []spi.TransitionDefinition{
					{
						Name:     "T1",
						Next:     "S1",
						Manual:   false,
						Schedule: &spi.TransitionSchedule{DelayMs: -100},
					},
				},
			},
		},
	}
	err := validateImportRequest([]spi.WorkflowDefinition{wf})
	if err == nil || !strings.Contains(err.Error(), "delayMs must be > 0") {
		t.Errorf("expected delayMs error, got: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

```bash
go test -run 'TestValidator_DelayMs(Zero|Negative)_Rejected' -v ./internal/domain/workflow/
```

Expected: FAIL with `expected delayMs error, got: <nil>` for both.

- [ ] **Step 3: Add the rule**

Edit `internal/domain/workflow/validate.go`. Below the manual+schedule check added in Task 8, add:

```go
			if tr.Schedule != nil && tr.Schedule.DelayMs <= 0 {
				return fmt.Errorf(
					"workflow %q state %q transition %q: schedule.delayMs must be > 0 (got %d)",
					wf.Name, stateName, tr.Name, tr.Schedule.DelayMs)
			}
```

- [ ] **Step 4: Run the tests to confirm they pass**

```bash
go test -run 'TestValidator_DelayMs(Zero|Negative)_Rejected' -v ./internal/domain/workflow/
```

Expected: PASS.

- [ ] **Step 5: Run package tests**

```bash
go test -short ./internal/domain/workflow/...
```

Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/workflow/validate.go internal/domain/workflow/validate_test.go
git commit -m "feat(workflow): reject schedule.delayMs <= 0 at import (#259)

A scheduled transition with delayMs <= 0 is effectively a regular
automated transition (firing in zero or negative time has no
schedule semantic). Reject at import with VALIDATION_FAILED 400.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 10: Validator happy-path regression tests

**Files:**
- Modify: validator test file from Task 8.

- [ ] **Step 1: Add happy-path tests**

Append to the validator test file:

```go
func TestValidator_ScheduleHappyPaths(t *testing.T) {
	tmZero := int64(0)
	tmPositive := int64(5000)

	cases := []struct {
		name string
		sch  *spi.TransitionSchedule
	}{
		{"timeoutMs_absent_nil", &spi.TransitionSchedule{DelayMs: 1000}},
		{"timeoutMs_explicit_zero", &spi.TransitionSchedule{DelayMs: 1000, TimeoutMs: &tmZero}},
		{"timeoutMs_less_than_delayMs", &spi.TransitionSchedule{DelayMs: 1000, TimeoutMs: &([]int64{500}[0])}},
		{"timeoutMs_greater_than_delayMs", &spi.TransitionSchedule{DelayMs: 1000, TimeoutMs: &tmPositive}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := spi.WorkflowDefinition{
				Version:      "1",
				Name:         "test",
				InitialState: "S1",
				Active:       true,
				States: map[string]spi.StateDefinition{
					"S1": {
						Transitions: []spi.TransitionDefinition{
							{Name: "T1", Next: "S1", Manual: false, Schedule: tc.sch},
						},
					},
				},
			}
			if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
				t.Errorf("expected acceptance for shape %q; got: %v", tc.name, err)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests**

```bash
go test -run TestValidator_ScheduleHappyPaths -v ./internal/domain/workflow/
```

Expected: all four sub-tests PASS. The third case (`timeoutMs_less_than_delayMs`) is the critical assertion that proves the rev 1 / rev 2 bogus rule was removed — under the late-tolerance semantic, this is a valid shape.

- [ ] **Step 3: Commit**

```bash
git add internal/domain/workflow/validate_test.go
git commit -m "test(workflow): pin Schedule-shape happy paths (#259)

Four shapes that MUST validate:
- timeoutMs absent (nil) - no-timeout semantic
- timeoutMs explicit zero (&0) - strictest semantic
- timeoutMs < delayMs (e.g. delay 1000ms, timeout 500ms) - valid
  under late-tolerance semantic (NOT max-age-of-armed-timer)
- timeoutMs > delayMs - valid

The third case is the regression guard for rev 1 / rev 2's bogus
TimeoutMs >= DelayMs rule.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 5 — `allowCycles` request envelope

## Task 11: Add `AllowCycles` to `importRequest` + signature change on `validateWorkflows` + slog.Warn

**Files:**
- Modify: `internal/domain/workflow/handler.go` (lines 60-64 for `importRequest`; line 183 for the `validateWorkflows` call).
- Modify: `internal/domain/workflow/validate.go` — `validateWorkflows` signature.
- Modify: `internal/domain/workflow/scenarios_test.go` — 8 call sites at lines 267, 287, 313, 334, 587, 619, 641, 652 (check current line numbers — they may have shifted from Phase 4 additions).

- [ ] **Step 1: Change `validateWorkflows` signature**

Edit `internal/domain/workflow/validate.go`. Find `validateWorkflows`. Change its signature from:

```go
func validateWorkflows(workflows []spi.WorkflowDefinition) error {
```

…to:

```go
func validateWorkflows(workflows []spi.WorkflowDefinition, allowCycles bool) error {
```

…and update the body to short-circuit `validateWorkflowLoops` when `allowCycles` is true:

```go
func validateWorkflows(workflows []spi.WorkflowDefinition, allowCycles bool) error {
	for _, wf := range workflows {
		if !allowCycles {
			if err := validateWorkflowLoops(wf); err != nil {
				return err
			}
		}
		if err := validateProcessorFlags(wf); err != nil {
			return err
		}
	}
	return nil
}
```

(If the existing body iterates differently, adapt the short-circuit logic accordingly. The principle: skip only `validateWorkflowLoops`; keep `validateProcessorFlags` unconditional.)

- [ ] **Step 2: Update the handler call site**

Edit `internal/domain/workflow/handler.go` lines 60-64. Replace the `importRequest` struct:

```go
// importRequest is the JSON body shape for workflow import.
type importRequest struct {
	ImportMode  string              `json:"importMode"`
	AllowCycles bool                `json:"allowCycles,omitempty"`
	Workflows   []workflowImportDef `json:"workflows"`
}
```

Then locate the `validateWorkflows(result)` call at line 183. Replace with:

```go
	if err := validateWorkflows(result, req.AllowCycles); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeValidationFailed, err.Error()))
		return
	}

	if req.AllowCycles {
		workflowNames := make([]string, len(result))
		for i, wf := range result {
			workflowNames[i] = wf.Name
		}
		slog.WarnContext(r.Context(), "workflow import: cycle validation bypassed",
			"pkg", "workflow",
			"entityName", entityName,
			"modelVersion", modelVersion,
			"importMode", mode,
			"workflows", workflowNames)
	}
```

If `slog` is not already imported in `handler.go`, add `"log/slog"` to the import block.

- [ ] **Step 3: Update all `validateWorkflows` call sites in `scenarios_test.go`**

```bash
grep -n "validateWorkflows" internal/domain/workflow/scenarios_test.go
```

Each call site has the form `validateWorkflows([]spi.WorkflowDefinition{wf})` or similar. Add `false` as the second argument at every site:

```bash
sed -i.bak 's|validateWorkflows(\([^)]*\))|validateWorkflows(\1, false)|g' internal/domain/workflow/scenarios_test.go
rm internal/domain/workflow/scenarios_test.go.bak
```

Verify with grep:

```bash
grep -n "validateWorkflows" internal/domain/workflow/scenarios_test.go
```

All entries should now have `, false)` at the end.

- [ ] **Step 4: Run the package tests to confirm compile + green**

```bash
go test -short ./internal/domain/workflow/...
```

Expected: all green (the existing tests still pass with `allowCycles=false`; new behaviour is gated behind the flag).

- [ ] **Step 5: Run the broader short-test suite**

```bash
go test -short ./...
```

Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/workflow/handler.go internal/domain/workflow/validate.go internal/domain/workflow/scenarios_test.go
git commit -m "feat(workflow): add request-level allowCycles escape hatch (#259)

Adds an optional 'allowCycles' boolean to the workflow-import request
body. Default false preserves byte-identical pre-#259 behaviour.

When true:
- validateWorkflows short-circuits validateWorkflowLoops (the cycle
  detector) for the duration of the import call.
- All other validators (Schedule rules, processor flags, structural
  checks) remain unconditional.
- The handler emits a single slog.Warn naming the affected model + the
  workflow names that were imported under the bypass.

Use cases: polling-pattern workflows built on scheduled transitions
(S1 →scheduled→ S2 →scheduled→ S1), and other workflows whose
cyclicity is intentional. The runtime cascade safety net
(maxStateVisits, maxCascadeDepth) is unchanged; catches actual
runaway at fire time.

Touches 8 validateWorkflows call sites in scenarios_test.go — all
mechanical additions of ', false' to preserve the existing
default-rejection behaviour in those tests.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 12: Tests for `allowCycles` bypass behaviour

**Files:**
- Modify: validator test file from Task 8.

- [ ] **Step 1: Write tests for the bypass matrix**

Append to the validator test file:

```go
// cyclicWorkflow builds S1 -automated, no criterion-> S2 -automated, no
// criterion-> S1. validateWorkflowLoops rejects this by default.
func cyclicWorkflow(t *testing.T) spi.WorkflowDefinition {
	t.Helper()
	return spi.WorkflowDefinition{
		Version:      "1",
		Name:         "cyclic",
		InitialState: "S1",
		Active:       true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{{Name: "to-S2", Next: "S2", Manual: false}}},
			"S2": {Transitions: []spi.TransitionDefinition{{Name: "to-S1", Next: "S1", Manual: false}}},
		},
	}
}

func TestValidator_DefaultFalse_RejectsCycle(t *testing.T) {
	wf := cyclicWorkflow(t)
	if err := validateWorkflows([]spi.WorkflowDefinition{wf}, false); err == nil {
		t.Fatal("expected cycle rejection at default allowCycles=false")
	}
}

func TestValidator_AllowCyclesTrue_BypassesCycleCheck(t *testing.T) {
	wf := cyclicWorkflow(t)
	if err := validateWorkflows([]spi.WorkflowDefinition{wf}, true); err != nil {
		t.Errorf("expected acceptance with allowCycles=true; got: %v", err)
	}
}

func TestValidator_AllowCyclesTrue_DoesNotBypassScheduleRules(t *testing.T) {
	tm := int64(1000)
	wf := spi.WorkflowDefinition{
		Version:      "1",
		Name:         "cyclic_and_incoherent",
		InitialState: "S1",
		Active:       true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "to-S2", Next: "S2", Manual: true, Schedule: &spi.TransitionSchedule{DelayMs: 1000, TimeoutMs: &tm}},
			}},
			"S2": {Transitions: []spi.TransitionDefinition{
				{Name: "to-S1", Next: "S1", Manual: false},
			}},
		},
	}
	// validateImportRequest catches the structural (manual+schedule) rule,
	// regardless of allowCycles. Note this calls validateImportRequest, not
	// validateWorkflows — the Schedule rules live in validateWorkflowStructure.
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err == nil {
		t.Fatal("expected manual+schedule rejection even when cyclic")
	} else if !strings.Contains(err.Error(), "manual and scheduled are mutually exclusive") {
		t.Errorf("expected Schedule-rule error, got: %v", err)
	}
}

func TestValidator_AllowCyclesTrue_PollingScheduledWorkflow_Accepted(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version:      "1",
		Name:         "polling",
		InitialState: "S1",
		Active:       true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "to-S2", Next: "S2", Manual: false, Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
			}},
			"S2": {Transitions: []spi.TransitionDefinition{
				{Name: "to-S1", Next: "S1", Manual: false, Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
			}},
		},
	}
	// Default (allowCycles=false) rejects (the scheduled-polling cycle).
	if err := validateWorkflows([]spi.WorkflowDefinition{wf}, false); err == nil {
		t.Fatal("expected cycle rejection on polling workflow at allowCycles=false")
	}
	// allowCycles=true accepts.
	if err := validateWorkflows([]spi.WorkflowDefinition{wf}, true); err != nil {
		t.Errorf("expected polling workflow to validate with allowCycles=true; got: %v", err)
	}
	// Structural validation (Schedule rules) also passes.
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
		t.Errorf("expected structural validation to pass on polling workflow; got: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests**

```bash
go test -run 'TestValidator_(DefaultFalse|AllowCyclesTrue)' -v ./internal/domain/workflow/
```

Expected: all four PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/domain/workflow/validate_test.go
git commit -m "test(workflow): pin allowCycles bypass behaviour (#259)

Four assertions:
- Default allowCycles=false rejects unguarded automated cycles
  (regression guard for existing cycle-detector behaviour).
- allowCycles=true accepts the same cyclic workflow.
- allowCycles=true does NOT bypass Schedule-coherence rules
  (manual+schedule still rejected via validateImportRequest).
- Polling-pattern scheduled workflow imports with allowCycles=true.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 6 — Engine cascade-skip

## Task 13: Cascade silent-skip for scheduled transitions

**Files:**
- Modify: `internal/domain/workflow/engine.go` — line 528 cascade-skip filter.
- Modify: engine test file (find with `grep -l 'TestCascade\|cascadeAutomated' internal/domain/workflow/*_test.go`).

- [ ] **Step 1: Write failing test — only-scheduled state rests**

Find the engine test file. Append:

```go
func TestEngine_CascadeSkipsScheduled_RestsInState(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version:      "1",
		Name:         "test",
		InitialState: "S1",
		Active:       true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "scheduledOnly", Next: "S2", Manual: false, Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
			}},
			"S2": {},
		},
	}
	e, _, _ := newTestEngine(t) // <- if a builder exists; else inline-build per engine_test.go's pattern
	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "e1", State: "S1"}}

	ctx := context.Background()
	auditStore := newMemoryAuditStore() // <- inline-build per engine_test.go's pattern
	_, _, err := e.cascadeAutomated(ctx, entity, &wf, auditStore, "tx-1")
	if err != nil {
		t.Fatalf("cascade returned error: %v", err)
	}
	if entity.Meta.State != "S1" {
		t.Errorf("expected entity to rest in S1; got %q", entity.Meta.State)
	}
}
```

(If `newTestEngine`/`newMemoryAuditStore` builders don't already exist in the test file, replace with whatever the existing engine tests use. Use `grep -A 20 'func TestEngine_' internal/domain/workflow/engine_test.go` to find the pattern.)

- [ ] **Step 2: Run the test to confirm it fails**

```bash
go test -run TestEngine_CascadeSkipsScheduled_RestsInState -v ./internal/domain/workflow/
```

Expected outcome: depends on current cascade behaviour. Most likely the cascade will fire the scheduled transition (since cascade doesn't yet skip on `Schedule != nil`) → entity ends in `S2` → test fails with `expected entity to rest in S1; got "S2"`.

- [ ] **Step 3: Add the cascade-skip filter**

Edit `internal/domain/workflow/engine.go` at line 528 (the skip filter inside `cascadeAutomated`'s per-transition loop). Change:

```go
			if tr.Disabled || tr.Manual {
				continue
			}
```

…to:

```go
			if tr.Disabled || tr.Manual || tr.Schedule != nil {
				continue
			}
```

- [ ] **Step 4: Run the test to confirm it passes**

```bash
go test -run TestEngine_CascadeSkipsScheduled_RestsInState -v ./internal/domain/workflow/
```

Expected: PASS.

- [ ] **Step 5: Add a second test — scheduled coexists with regular automated**

Append to the engine test file:

```go
func TestEngine_CascadeSkipsScheduled_FiresRegularSibling(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version:      "1",
		Name:         "test",
		InitialState: "S1",
		Active:       true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "scheduled", Next: "S2", Manual: false, Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
				{Name: "regular", Next: "Sfinal", Manual: false},
			}},
			"S2":     {},
			"Sfinal": {},
		},
	}
	e, _, _ := newTestEngine(t)
	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "e1", State: "S1"}}

	ctx := context.Background()
	auditStore := newMemoryAuditStore()
	_, _, err := e.cascadeAutomated(ctx, entity, &wf, auditStore, "tx-1")
	if err != nil {
		t.Fatalf("cascade returned error: %v", err)
	}
	if entity.Meta.State != "Sfinal" {
		t.Errorf("expected entity to land in Sfinal via the regular sibling; got %q", entity.Meta.State)
	}
}
```

Run:

```bash
go test -run 'TestEngine_CascadeSkipsScheduled_' -v ./internal/domain/workflow/
```

Expected: both PASS.

- [ ] **Step 6: Run the broader test suite**

```bash
go test -short ./internal/domain/workflow/...
```

Expected: all green.

- [ ] **Step 7: Commit**

```bash
git add internal/domain/workflow/engine.go internal/domain/workflow/engine_test.go
git commit -m "feat(workflow): cascade silently skips scheduled transitions (#259)

One-line extension to the existing Disabled/Manual skip filter in
cascadeAutomated's per-transition loop. Schedule != nil transitions
are not eligible for automated cascade selection — they wait for
their timer.

Until the timer runtime ships (#251), scheduled transitions are
invisible to cascade. An entity whose only exit is scheduled rests
in its source state. Other automated/manual exits out of the same
state fire normally.

No audit event for the skip — matches existing silent-skip semantics
for Disabled and Manual transitions.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 7 — Engine explicit-fire reject

## Task 14: Explicit-fire reject for scheduled transitions

**Files:**
- Modify: `internal/domain/workflow/engine.go` — `attemptTransition` after the `Disabled` check at lines 449-453.
- Modify: engine test file from Task 13.

- [ ] **Step 1: Write failing test — explicit fire of scheduled returns ErrTransitionNotFound**

Append to the engine test file:

```go
func TestEngine_AttemptTransition_Scheduled_ReturnsTransitionNotFound(t *testing.T) {
	wf := spi.WorkflowDefinition{
		Version:      "1",
		Name:         "test",
		InitialState: "S1",
		Active:       true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "AutoClose", Next: "Closed", Manual: false, Schedule: &spi.TransitionSchedule{DelayMs: 1000}},
			}},
			"Closed": {},
		},
	}
	e, _, _ := newTestEngine(t)
	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "e1", State: "S1"}}
	auditStore := newMemoryAuditStore()

	_, _, err := e.attemptTransition(context.Background(), entity, &wf, "AutoClose", auditStore, "tx-1")
	if err == nil {
		t.Fatal("expected error firing a scheduled transition by name")
	}
	if !errors.Is(err, ErrTransitionNotFound) {
		t.Errorf("expected error to wrap ErrTransitionNotFound (mirrors Disabled precedent); got: %v", err)
	}
	if !strings.Contains(err.Error(), "scheduled transitions are not yet implemented") {
		t.Errorf("expected error message to name the unavailability; got: %v", err)
	}
	if entity.Meta.State != "S1" {
		t.Errorf("expected entity to stay in source state S1; got %q", entity.Meta.State)
	}

	// Audit trail: exactly one SMEventTransitionNotFound row, no
	// SMEventTransitionMade, no SMEventStateProcessResult.
	events := auditStore.Events()
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 audit event; got %d: %+v", len(events), events)
	}
	if events[0].EventType != spi.SMEventTransitionNotFound {
		t.Errorf("expected SMEventTransitionNotFound; got %q", events[0].EventType)
	}
	if !strings.Contains(events[0].Details, "scheduled transitions are not yet implemented") {
		t.Errorf("expected Details to carry rejection cause; got %q", events[0].Details)
	}
}
```

(Adapt `auditStore.Events()` to whichever accessor the test's audit store exposes — e.g. `auditStore.records` directly, or a helper. Grep `internal/domain/workflow/engine_test.go` for an existing audit-trail-assertion test for the shape.)

- [ ] **Step 2: Run the test to confirm it fails**

```bash
go test -run TestEngine_AttemptTransition_Scheduled_ReturnsTransitionNotFound -v ./internal/domain/workflow/
```

Expected: FAIL. The current `attemptTransition` will progress past the missing schedule check, evaluate criterion (none, accepts), call `executeProcessors` (no processors, succeeds), and FIRE the transition → entity moves to `Closed`. So the test fails on `entity.Meta.State` assertion.

- [ ] **Step 3: Add the explicit-fire reject branch**

Edit `internal/domain/workflow/engine.go`. Find `attemptTransition`'s `Disabled` check (lines 449-453). Immediately after the `if transition.Disabled { ... }` block, insert a new branch BEFORE the criterion evaluation:

```go
	if transition.Schedule != nil {
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventTransitionNotFound,
			fmt.Sprintf(
				"Transition %q is scheduled; scheduled transitions are not yet implemented",
				transitionName), nil)
		return ctx, txID, fmt.Errorf(
			"transition %q in state %q is scheduled; scheduled transitions are not yet implemented: %w",
			transitionName, entity.Meta.State, ErrTransitionNotFound)
	}
```

- [ ] **Step 4: Run the test to confirm it passes**

```bash
go test -run TestEngine_AttemptTransition_Scheduled_ReturnsTransitionNotFound -v ./internal/domain/workflow/
```

Expected: PASS.

- [ ] **Step 5: Run the package suite**

```bash
go test -short ./internal/domain/workflow/...
```

Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/workflow/engine.go internal/domain/workflow/engine_test.go
git commit -m "feat(workflow): reject explicit fire of scheduled transitions (#259)

attemptTransition gains a new branch after the existing Disabled check.
Mirrors the Disabled precedent byte-for-byte:
- Emit SMEventTransitionNotFound with Details naming the rejection
  cause.
- Return error wrapping ErrTransitionNotFound sentinel.
- classifyWorkflowError maps to 400 TRANSITION_NOT_FOUND.

Per project lead: TRANSITION_NOT_FOUND is the right code for
'transition exists but is not currently dispatchable from the
caller's POV' — exactly the scheduled-but-runtime-not-yet-implemented
case. Same code Disabled returns. Audit consumers searching for
'transition not currently dispatchable' events see a uniform shape
across missing-by-name, disabled, and scheduled cases; the Details
substring distinguishes them.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 8 — Round-trip preservation

## Task 15: Round-trip test across the three `TimeoutMs` pointer states

**Files:**
- Modify: workflow test file (e.g. `internal/domain/workflow/handler_test.go` or whichever file hosts existing import/export round-trip tests; check with `grep -l 'TestImport\|RoundTrip' internal/domain/workflow/*_test.go`).

- [ ] **Step 1: Write round-trip tests for the three pointer states**

The test asserts JSON byte-equivalence for each of the three states via Go-level marshal/unmarshal cycles:

```go
func TestSchedule_RoundTrip_TimeoutMsPointerStates(t *testing.T) {
	tm := int64(90000000)
	tmZero := int64(0)

	cases := []struct {
		name        string
		schedule    *spi.TransitionSchedule
		wantInJSON  string  // substring that MUST appear in marshalled JSON
		wantNotInJSON string  // substring that MUST NOT appear
	}{
		{
			name:       "timeoutMs_non_nil_positive",
			schedule:   &spi.TransitionSchedule{DelayMs: 86400000, TimeoutMs: &tm},
			wantInJSON: `"timeoutMs":90000000`,
		},
		{
			name:       "timeoutMs_non_nil_zero",
			schedule:   &spi.TransitionSchedule{DelayMs: 86400000, TimeoutMs: &tmZero},
			wantInJSON: `"timeoutMs":0`,
		},
		{
			name:          "timeoutMs_nil",
			schedule:      &spi.TransitionSchedule{DelayMs: 86400000},
			wantNotInJSON: "timeoutMs",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := spi.TransitionDefinition{
				Name:     "AutoClose",
				Next:     "Closed",
				Manual:   false,
				Schedule: tc.schedule,
			}
			bs, err := json.Marshal(tr)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantInJSON != "" && !strings.Contains(string(bs), tc.wantInJSON) {
				t.Errorf("expected JSON to contain %q; got %s", tc.wantInJSON, bs)
			}
			if tc.wantNotInJSON != "" && strings.Contains(string(bs), tc.wantNotInJSON) {
				t.Errorf("expected JSON NOT to contain %q; got %s", tc.wantNotInJSON, bs)
			}

			var back spi.TransitionDefinition
			if err := json.Unmarshal(bs, &back); err != nil {
				t.Fatal(err)
			}
			if back.Schedule == nil {
				t.Fatal("Schedule lost on round-trip")
			}
			if back.Schedule.DelayMs != tc.schedule.DelayMs {
				t.Errorf("DelayMs lost: got %d", back.Schedule.DelayMs)
			}
			// Pointer-state-preserving equality check.
			gotPtr := back.Schedule.TimeoutMs
			wantPtr := tc.schedule.TimeoutMs
			if (gotPtr == nil) != (wantPtr == nil) {
				t.Errorf("TimeoutMs pointer-presence mismatched: got %v, want %v", gotPtr, wantPtr)
			}
			if gotPtr != nil && wantPtr != nil && *gotPtr != *wantPtr {
				t.Errorf("TimeoutMs value mismatched: got %d, want %d", *gotPtr, *wantPtr)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test**

```bash
go test -run TestSchedule_RoundTrip_TimeoutMsPointerStates -v ./internal/domain/workflow/
```

Expected: all three sub-tests PASS — this is a regression guard, the round-trip already works given the SPI-level pointer field added in Task 2.

- [ ] **Step 3: Commit**

```bash
git add internal/domain/workflow/*_test.go
git commit -m "test(workflow): pin Schedule round-trip across TimeoutMs pointer states (#259)

Regression guard for the *int64 pointer pattern's round-trip fidelity:
- non-nil positive: marshals as 'timeoutMs:N', round-trips byte-equal.
- non-nil zero: marshals as 'timeoutMs:0' (NOT omitted — the pointer
  semantics are what omitempty preserves; the value is zero).
- nil: marshals without the 'timeoutMs' key.

Pins the entire *int64+omitempty contract end-to-end.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 9 — E2E test

## Task 16: E2E test — manual fire of scheduled returns 400 TRANSITION_NOT_FOUND

**Files:**
- Modify or create: a test file in `internal/e2e/` (check `ls internal/e2e/*_test.go` for the existing workflow-import E2E test file to extend; if none, create `internal/e2e/scheduled_transition_test.go`).

- [ ] **Step 1: Locate the existing E2E test pattern**

```bash
ls internal/e2e/*_test.go
grep -l "POST /model" internal/e2e/*_test.go
```

Read the existing patterns to match the auth, model-creation, and HTTP-client helper conventions. The new test should reuse the same shared `TestMain` setup (PostgreSQL via testcontainers, in-process `httptest.Server`, JWT auth).

- [ ] **Step 2: Write the E2E test**

Following the existing pattern, add a test that:

1. Creates a model `EntityX`, version 1.
2. POSTs a workflow with one transition: `{name: "AutoClose", next: "Closed", manual: false, schedule: {delayMs: 1000}}`. Asserts HTTP 200.
3. POSTs to create an entity instance. Asserts the entity lands in the initial state (cascade skips the scheduled transition).
4. POSTs `transition/AutoClose` to fire the scheduled transition by name. Asserts HTTP 400 with `error.code == "TRANSITION_NOT_FOUND"` and the message contains `"scheduled transitions are not yet implemented"`.
5. GETs the entity and asserts the state is still the initial state.

Concrete skeleton (adapt to the existing E2E harness's helpers — the test below uses placeholder helper names that you should replace with the actual ones from the existing tests):

```go
func TestE2E_ExplicitFireOfScheduledTransition_ReturnsTransitionNotFound(t *testing.T) {
	srv := setupE2EServer(t) // reuse the existing helper
	defer srv.Close()
	client := srv.Client()
	token := mintAuthToken(t)

	entityName := "EntityX"
	modelVersion := int32(1)

	// (1) Create model.
	createModel(t, client, srv.URL, token, entityName, modelVersion, defaultModelSchema())

	// (2) Import workflow with a scheduled transition.
	importBody := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1",
			"name": "test",
			"initialState": "Open",
			"active": true,
			"states": {
				"Open": {"transitions": [{"name":"AutoClose","next":"Closed","manual":false,"schedule":{"delayMs":1000}}]},
				"Closed": {}
			}
		}]
	}`
	resp := postJSON(t, client, srv.URL,
		fmt.Sprintf("/api/model/%s/%d/workflow/import", entityName, modelVersion),
		token, importBody)
	if resp.StatusCode != 200 {
		t.Fatalf("import returned %d; body: %s", resp.StatusCode, readBody(t, resp))
	}

	// (3) Create entity.
	createBody := `{"data":{"foo":"bar"}}`
	resp = postJSON(t, client, srv.URL, fmt.Sprintf("/api/entity/%s", entityName), token, createBody)
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		t.Fatalf("create entity returned %d; body: %s", resp.StatusCode, readBody(t, resp))
	}
	var created struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	mustDecodeJSON(t, resp.Body, &created)
	if created.State != "Open" {
		t.Errorf("expected entity to land in initial state 'Open' (cascade skips scheduled); got %q", created.State)
	}

	// (4) Fire the scheduled transition by name.
	resp = postJSON(t, client, srv.URL,
		fmt.Sprintf("/api/entity/%s/%s/transition/AutoClose", entityName, created.ID),
		token, `{}`)
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 TRANSITION_NOT_FOUND; got %d, body: %s", resp.StatusCode, readBody(t, resp))
	}
	var errBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	mustDecodeJSON(t, resp.Body, &errBody)
	if errBody.Error.Code != "TRANSITION_NOT_FOUND" {
		t.Errorf("expected error.code TRANSITION_NOT_FOUND; got %q", errBody.Error.Code)
	}
	if !strings.Contains(errBody.Error.Message, "scheduled transitions are not yet implemented") {
		t.Errorf("expected error.message to name the rejection cause; got %q", errBody.Error.Message)
	}

	// (5) GET entity asserts the state is still Open.
	resp = getJSON(t, client, srv.URL, fmt.Sprintf("/api/entity/%s/%s", entityName, created.ID), token)
	var fetched struct {
		State string `json:"state"`
	}
	mustDecodeJSON(t, resp.Body, &fetched)
	if fetched.State != "Open" {
		t.Errorf("expected entity to remain in initial state after rejection; got %q", fetched.State)
	}
}
```

> **Note for the implementer.** The helper functions above (`setupE2EServer`, `mintAuthToken`, `createModel`, `postJSON`, `getJSON`, `readBody`, `mustDecodeJSON`, `defaultModelSchema`) are placeholders. Use whatever the existing E2E tests use. If no equivalent exists for some, factor what you need from an existing test rather than inventing a new style — these helpers exist as a pattern, not a contract.

- [ ] **Step 3: Run the E2E test (requires Docker)**

```bash
go test -run TestE2E_ExplicitFireOfScheduledTransition_ReturnsTransitionNotFound -v ./internal/e2e/
```

Expected: PASS (after the helper adaptations).

- [ ] **Step 4: Run the full E2E suite to confirm no regressions**

```bash
go test ./internal/e2e/ -v
```

Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/
git commit -m "test(e2e): explicit-fire of scheduled transition returns 400 TRANSITION_NOT_FOUND (#259)

End-to-end coverage through the full HTTP stack:
1. Create model.
2. Import workflow with one scheduled transition. Assert 200.
3. Create entity instance. Assert it rests in initial state
   (cascade skips scheduled).
4. POST transition/AutoClose. Assert 400 TRANSITION_NOT_FOUND with
   the 'scheduled transitions are not yet implemented' message
   substring.
5. GET entity. Assert state unchanged.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 10 — Parity scenarios

## Task 17: Add `wf-import/07`, `wf-import/08`, `wf-import/09` to scenario 08

**Files:**
- Modify: `e2e/externalapi/scenarios/08-workflow-import-export.yaml`.

- [ ] **Step 1: Confirm the next free slot**

```bash
grep -n "wf-import/" e2e/externalapi/scenarios/08-workflow-import-export.yaml | sort -u
```

Expected: slots `01..06` populated. Next free is `07`.

- [ ] **Step 2: Append the three scenarios**

At the end of the YAML file, after the last existing wf-import entry, append:

```yaml
  - id: wf-import/07-scheduled-transition-roundtrip
    description: |
      Round-trip preservation across the three TimeoutMs pointer states:
      non-nil positive, non-nil zero, and absent (nil).
    auth: admin
    requests:
      - method: POST
        path: /api/model/RoundTripModel/1
        body_json: {}
      - method: POST
        path: /api/model/RoundTripModel/1/workflow/import
        body_json:
          importMode: REPLACE
          workflows:
            - version: "1"
              name: roundtrip
              initialState: Open
              active: true
              states:
                Open:
                  transitions:
                    - name: schedNonNilPositive
                      next: Closed
                      manual: false
                      schedule:
                        delayMs: 86400000
                        timeoutMs: 90000000
                    - name: schedNonNilZero
                      next: Closed
                      manual: false
                      schedule:
                        delayMs: 86400000
                        timeoutMs: 0
                    - name: schedNil
                      next: Closed
                      manual: false
                      schedule:
                        delayMs: 86400000
                Closed: {}
          allowCycles: false
        assert_status: 200
      - method: GET
        path: /api/model/RoundTripModel/1/workflow
        assert_status: 200
        assert_json_equals_normalized:
          workflows:
            - version: "1"
              name: roundtrip
              initialState: Open
              active: true
              states:
                Open:
                  transitions:
                    - name: schedNonNilPositive
                      next: Closed
                      manual: false
                      schedule:
                        delayMs: 86400000
                        timeoutMs: 90000000
                    - name: schedNonNilZero
                      next: Closed
                      manual: false
                      schedule:
                        delayMs: 86400000
                        timeoutMs: 0
                    - name: schedNil
                      next: Closed
                      manual: false
                      schedule:
                        delayMs: 86400000
                Closed: {}

  - id: wf-import/08-scheduled-transition-rejects
    description: |
      Validator rejection matrix — manual+schedule mutually exclusive
      and delayMs <= 0. (Rev 1's third rule TimeoutMs >= DelayMs was
      removed in rev 3 as bogus.)
    auth: admin
    requests:
      - method: POST
        path: /api/model/RejectModel/1
        body_json: {}
      - method: POST
        path: /api/model/RejectModel/1/workflow/import
        body_json:
          importMode: REPLACE
          workflows:
            - version: "1"
              name: bad
              initialState: S1
              active: true
              states:
                S1:
                  transitions:
                    - name: T1
                      next: S1
                      manual: true
                      schedule:
                        delayMs: 1000
        assert_status: 400
        assert_json_contains:
          error:
            code: VALIDATION_FAILED
            message: "manual and scheduled are mutually exclusive"
      - method: POST
        path: /api/model/RejectModel/1/workflow/import
        body_json:
          importMode: REPLACE
          workflows:
            - version: "1"
              name: bad2
              initialState: S1
              active: true
              states:
                S1:
                  transitions:
                    - name: T1
                      next: S1
                      manual: false
                      schedule:
                        delayMs: 0
        assert_status: 400
        assert_json_contains:
          error:
            code: VALIDATION_FAILED
            message: "delayMs must be > 0"

  - id: wf-import/09-allowcycles-bypass
    description: |
      Polling-pattern scheduled workflow imports only with
      allowCycles=true. Default false preserves the cycle rejection.
    auth: admin
    requests:
      - method: POST
        path: /api/model/PollingModel/1
        body_json: {}
      - method: POST
        path: /api/model/PollingModel/1/workflow/import
        body_json:
          importMode: REPLACE
          workflows:
            - version: "1"
              name: polling
              initialState: S1
              active: true
              states:
                S1:
                  transitions:
                    - name: toS2
                      next: S2
                      manual: false
                      schedule:
                        delayMs: 1000
                S2:
                  transitions:
                    - name: toS1
                      next: S1
                      manual: false
                      schedule:
                        delayMs: 1000
        assert_status: 400
        assert_json_contains:
          error:
            code: VALIDATION_FAILED
      - method: POST
        path: /api/model/PollingModel/1/workflow/import
        body_json:
          importMode: REPLACE
          allowCycles: true
          workflows:
            - version: "1"
              name: polling
              initialState: S1
              active: true
              states:
                S1:
                  transitions:
                    - name: toS2
                      next: S2
                      manual: false
                      schedule:
                        delayMs: 1000
                S2:
                  transitions:
                    - name: toS1
                      next: S1
                      manual: false
                      schedule:
                        delayMs: 1000
        assert_status: 200
```

> **Note.** If the parity test harness uses slightly different keys (e.g. `assert_body_contains` instead of `assert_json_contains`, or `expect_status` instead of `assert_status`), adjust to match the existing scenarios at `wf-import/01..06`. The structural shape (three scenarios, the round-trip / reject / bypass split) stays unchanged.

- [ ] **Step 3: Run the parity suite**

Find the parity-suite run command:

```bash
grep -rn "externalapi\|parity" Makefile
```

…and run that target. Expected: all scenarios green.

- [ ] **Step 4: Commit**

```bash
git add e2e/externalapi/scenarios/08-workflow-import-export.yaml
git commit -m "test(parity): add wf-import/07..09 scheduled-transition scenarios (#259)

Three new scenarios at slots 07-09:
- 07: round-trip preservation across the three TimeoutMs pointer
  states (non-nil positive, non-nil zero, nil).
- 08: validator rejection matrix (manual+schedule, delayMs <= 0).
- 09: allowCycles=true bypasses the cycle rejection for a polling-
  pattern scheduled workflow.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 11 — Documentation

## Task 18: Add `## SCHEDULED TRANSITIONS` section to the workflows help topic

**Files:**
- Modify: `cmd/cyoda/help/content/workflows.md`.

- [ ] **Step 1: Locate the insertion point**

```bash
grep -n "^## " cmd/cyoda/help/content/workflows.md
```

Expected: a list of `## TRANSITIONS`, `## PROCESSORS`, `## CRITERIA`, etc. Insert the new section AFTER the last line of `## PROCESSORS`, BEFORE `## CRITERIA`.

- [ ] **Step 2: Add the new section**

Insert at that location (preserve heading level 2 to match siblings):

````markdown
## SCHEDULED TRANSITIONS

A transition may carry an optional `schedule` object marking it as
scheduled. Presence of `schedule` declares that the transition fires
automatically at `scheduledTime = stateEntryTime + delayMs`. The
`schedule` shape is:

```json
{
  "name": "AutoClose",
  "next": "Closed",
  "manual": false,
  "schedule": {
    "delayMs": 86400000,
    "timeoutMs": 600000
  }
}
```

**Fields:**

- `delayMs` (integer, required) — delay between source-state entry
  and the scheduled execution time, in milliseconds. Must be `> 0`.
- `timeoutMs` (integer, optional) — late-tolerance window past the
  scheduled execution time, in milliseconds. When the scheduler
  picks the task up, if `(executionTime - scheduledTime) > timeoutMs`
  the task is dropped and the transition is NOT attempted. Absent
  (omitted) means no timeout — fire whenever the scheduler
  eventually picks it up. Explicit `0` is the strictest setting —
  drop on any lateness. Independent of `delayMs`; the two measure
  different quantities.

**Import-time validation rules:**

- `manual: true` and `schedule` present are mutually exclusive
  (`VALIDATION_FAILED`).
- `schedule.delayMs <= 0` is rejected (`VALIDATION_FAILED`).
- No further rule on `timeoutMs` beyond `>= 0` (enforced at the DTO
  boundary).

**Engine behaviour (runtime not yet implemented).** The scheduler
that arms and fires scheduled transitions is not yet implemented.
Two guards govern behaviour until it ships:

- During automated cascade evaluation, scheduled transitions are
  silently skipped — they are never selected for immediate firing.
  Other automated/manual transitions out of the same state fire
  normally. An entity whose only exit is scheduled rests in its
  current state.
- Explicitly firing a scheduled transition by name returns HTTP 400
  `TRANSITION_NOT_FOUND` with the message `transition "X" in state
  "Y" is scheduled; scheduled transitions are not yet implemented`.
  Same code returned when a transition is `disabled: true` — same
  semantic: "the transition exists but is not currently dispatchable
  from the caller's POV." The entity remains in the source state.

**Importing cyclic scheduled workflows.** A canonical scheduled-
transition use case is a polling pattern such as `S1 →scheduled→ S2
→scheduled→ S1`. The import-time cycle detector rejects unguarded
automated cycles by default — including this one, because a delayed
cycle is still a cycle. To import such a workflow, set the request-
level field `allowCycles: true` on the import body:

```json
{
  "importMode": "REPLACE",
  "allowCycles": true,
  "workflows": [ /* ... */ ]
}
```

`allowCycles: true` bypasses only the cycle-detection check. Schedule
shape rules (manual+schedule, delayMs) and all other validators
remain unconditional. The runtime cascade-depth and per-state visit
caps still catch actual runaway at fire time. Use only for workflows
whose cyclicity is intentional.
````

- [ ] **Step 3: Verify the help-topic builds**

If there's a help-topic test or build step (check the Makefile):

```bash
grep -n "help\|content" Makefile | head -10
```

Run whatever target exists. If none, render the topic by running:

```bash
go run ./cmd/cyoda help workflows | head -100
```

Expected: the new section appears in the rendered output.

- [ ] **Step 4: Commit**

```bash
git add cmd/cyoda/help/content/workflows.md
git commit -m "docs(help): document scheduled transitions in workflows topic (#259)

Adds a new ## SCHEDULED TRANSITIONS section between ## PROCESSORS and
## CRITERIA. Documents:
- The schedule object shape (delayMs required, timeoutMs optional).
- The late-tolerance semantic for timeoutMs (scheduledTime, lateness,
  drop-if-late).
- The two import-time validation rules.
- The v0.8.0 engine behaviour: cascade silently skips, explicit-fire
  returns 400 TRANSITION_NOT_FOUND.
- The allowCycles request-level escape hatch for polling-pattern
  workflows.

No issue IDs in shipped content per project policy.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

# Phase 12 — Verification + PR

## Task 19: Run full verification + open the cyoda-go PR

- [ ] **Step 1: Full test sweep (root module)**

```bash
go vet ./...
go test -short ./...
```

Expected: both clean.

- [ ] **Step 2: E2E suite (Docker required)**

```bash
go test ./internal/e2e/... -v
```

Expected: all green.

- [ ] **Step 3: Plugin submodule sweep**

```bash
make test-all
```

(Or per-plugin manually: `cd plugins/memory && go test ./... && cd ../sqlite && go test ./... && cd ../postgres && go test ./...`. Postgres requires Docker.)

Expected: all green.

- [ ] **Step 4: Race detector**

```bash
go test -race ./...
```

Expected: clean. This is the per-`.claude/rules/race-testing.md` one-shot pre-PR sanity check.

- [ ] **Step 5: Confirm no TODOs were added**

```bash
make todos 2>&1 || git grep -n 'TODO(plan-' -- '*.go'
```

Expected: no new TODOs from this work.

- [ ] **Step 6: Re-verify the SPI pin is on SPI main HEAD**

```bash
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi fetch origin
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi rev-parse --short origin/main
grep "cyoda-go-spi v" go.mod
```

Expected: the SPI HEAD short-SHA matches the SHA fragment in the pseudo-version pin. If they don't match (e.g. someone else merged to SPI main between Phase 2 and now), repeat Task 5's `go get … @main` + `go mod tidy` across all four go.mods and commit a final bump.

- [ ] **Step 7: Push the branch**

```bash
git push -u origin $(git branch --show-current)
```

(The worktree's current branch is `worktree-259-scheduled-transition-config-shape`. The push will create the same name on `origin`. If you want a different name on push, rename first: `git branch -m feat/259-scheduled-transition-shape && git push -u origin feat/259-scheduled-transition-shape`.)

- [ ] **Step 8: Open the cyoda-go PR**

```bash
gh pr create --base release/v0.8.0 \
  --title "feat(workflow): scheduled-transition configuration shape + SPI" \
  --milestone "v0.8.0" \
  --body "$(cat <<'EOF'
Closes #259.

## What this adds

Implements the scheduled-transition configuration shape and SPI per
the spec at \`docs/superpowers/specs/2026-06-15-issue-259-scheduled-transition-shape-design.md\` rev 3.

- New \`TransitionScheduleDto\` schema (api/openapi.yaml) + regenerated
  DTO.
- Optional \`schedule\` property on \`TransitionDefinitionDto\`.
- Two validator rules: \`manual+schedule\` mutually exclusive,
  \`delayMs <= 0\` rejected.
- Cascade silently skips \`Schedule != nil\` transitions (one-line
  extension to existing Disabled/Manual filter).
- Explicit-fire of a scheduled transition returns
  \`400 TRANSITION_NOT_FOUND\` with message
  \"scheduled transitions are not yet implemented\" (mirrors the
  Disabled precedent byte-for-byte).
- Request-level \`allowCycles\` escape hatch on the import body for
  polling-pattern scheduled workflows. Bypasses only
  \`validateWorkflowLoops\`; emits one \`slog.Warn\` per import call.
- New help-topic section \`## SCHEDULED TRANSITIONS\` in
  \`cmd/cyoda/help/content/workflows.md\`.
- Three new parity scenarios \`wf-import/07..09\`.
- E2E test for the explicit-fire reject.

## What this does NOT do

- No timer runtime — that's #251 (deferred beyond v0.8.0).
- No new \`SMEventScheduledTransition*\` events — also #251.
- No per-workflow \`AllowCycles\` field — request-level only by
  design.

## SPI coordination

- Bundled SPI changes landed in cyoda-go-spi (PR <fill in once
  merged>): new \`TransitionSchedule\` type + \`Schedule\` field +
  deferred \`ProcessorDefinition.Type\` docstring from #250.
- This PR bumps the cyoda-go-spi pseudo-version pin across all
  four go.mod files (root + plugins/memory|sqlite|postgres).
- No SPI tag cut — bundled into v0.8.0's end-of-milestone tag per
  \`cyoda-go-spi/MAINTAINING.md\`.

## TimeoutMs semantic

Per project lead's clarification:
- \`scheduledTime = stateEntryTime + delayMs\`
- \`lateness = executionTime - scheduledTime\`
- if \`lateness > timeoutMs\` → drop the task, transition NOT
  attempted

\`TimeoutMs\` is \`*int64\`. \`nil\` = no timeout (always eventually
fire), \`&0\` = strictest (drop on any lateness), \`&N\` = drop if
late > N ms. Pointer pattern matches precedent
\`ProcessorConfig.StartNewTxOnDispatch *bool\`. All three states
round-trip byte-equivalent.

## Verification

- \`go vet ./...\` clean.
- \`go test -short ./...\` green.
- \`go test ./internal/e2e/...\` green (Docker).
- \`make test-all\` green (all plugins).
- \`go test -race ./...\` clean (one-shot pre-PR sanity check).

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Expected: PR URL printed.

- [ ] **Step 9: Hand off**

> **DONE.** Notify the human reviewer. The cyoda-go PR awaits review on the `release/v0.8.0` milestone branch. The PR description references the SPI PR which must be merged first; if not yet merged at PR-open time, leave the PR in draft until the SPI side merges and the pseudo-version pin is final.

---

## Self-review

- **Spec coverage:** every section of the rev 3 spec maps to one or more tasks per the cross-reference table at the top of this plan. The `[Removed in rev 3]` test 4 in the spec corresponds to no task in this plan — deliberately, because the rule it tested no longer exists.
- **Placeholder scan:** no TBDs, no TODOs, no "implement later"; every step has either concrete code or a concrete command.
- **Type consistency:** `TransitionSchedule`, `TransitionDefinition.Schedule`, `importRequest.AllowCycles`, `validateWorkflows(workflows, allowCycles bool)`, `ErrTransitionNotFound`, `SMEventTransitionNotFound` — names match across all tasks.
- **Cross-repo handoff:** Task 4 step 5 explicitly halts for human review of the SPI PR before Task 5 (the pin bump) can begin.
