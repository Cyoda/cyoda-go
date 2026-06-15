# Issue #259 — Scheduled-transition configuration shape + SPI

**Date:** 2026-06-15
**Milestone:** v0.8.0
**Issue:** #259 (closed-by this PR)
**Parent (deferred runtime):** #251
**Prerequisite (landed):** #250 — processor execution-location split
**Audit reference:** `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` §H1

> **Revision history**
>
> - **Revision 1 (2026-06-15):** initial spec.

---

## 1. Problem

The workflow import/export audit (§H1) records that the pre-#250 OpenAPI
surfaced a `ScheduledTransitionProcessorDefinitionDto` inside `processors[]`
carrying `{delayMs, transition, timeoutMs}` — modelled as one of two
`oneOf` variants on `ProcessorDefinitionDto`. Three compounding problems
made it inert at the runtime: the SPI flattened both variants into a single
`ProcessorDefinition`; the engine never consulted `proc.Type`; and the
scheduling config fields were silently dropped by `json.Unmarshal`. The
audit graded this HIGH severity for contract↔implementation divergence.

#250 (merged in #265) executed the schema cleanup: `processors[].type` is
now the execution-location axis only (`externalized` today, `internalized`
reserved). `ScheduledTransitionProcessorDefinitionDto` and
`ScheduledTransitionConfigDto` were removed entirely from the OpenAPI
surface. The scheduled-transition primitive has no home today.

#259 re-introduces the primitive at its proper home — a sibling field on
`TransitionDefinition` — while explicitly **not** implementing the timer
runtime (timer durability across restarts, cluster ownership / failover,
work-stealing recovery, cancellation on state change, audit events for
arming / firing / cancellation). That runtime is parent issue #251, deferred
beyond v0.8.0.

The acceptance criteria recorded on #259 require:

- SPI types tagged for cyoda-go-spi v0.8.0 (the milestone tag, not a
  per-PR tag — see §11).
- OpenAPI + generated DTOs updated.
- Import validation accepts the shape.
- Export round-trip preserves the shape.
- Engine rejects use at runtime.
- Help-topic + release notes updated.

This spec covers all six. Per memory `feedback_no_issue_ids_in_code`,
no shipped artefact references issue IDs; the rejection error names the
unavailability of the feature in self-contained terms.

## 2. Goals

- Define `TransitionSchedule` as a new SPI type and add an optional
  `Schedule *TransitionSchedule` field to `TransitionDefinition`.
- Match the SPI change in `api/openapi.yaml` via a new
  `TransitionScheduleDto` schema and an optional `schedule` field on
  `TransitionDefinitionDto`.
- Land import-time validator rules for the shape's internal coherence:
  `Manual=true && Schedule!=nil` is rejected (structurally incoherent);
  `Schedule!=nil && DelayMs<=0` is rejected; `Schedule!=nil &&
  TimeoutMs>0 && TimeoutMs<DelayMs` is rejected.
- Land two-pronged engine guards:
  - The cascade-automatic loop silently treats `Schedule!=nil` as ineligible
    (mirroring how it already treats `Disabled` and `Manual`). No audit
    signal; entity progress through other automated/manual transitions out
    of the same state is unaffected.
  - The explicit-fire-by-name path (`attemptTransition`) rejects any
    transition with `Schedule!=nil` with `WORKFLOW_FAILED 400` and a
    self-contained error message. Mirrors #250's pattern for `Type:
    internalized` rejection.
- Bundle #250's deferred `ProcessorDefinition.Type` docstring update into
  the same SPI PR — per #250 spec §5.3 the docstring was intentionally
  held back for the first carve-out that needs a substantive SPI change.
- Update the help topic `cmd/cyoda/help/content/workflows.md` to document
  the shape, the validator rules, and the v0.8.0 behavioural status. No
  issue IDs.

## 3. Non-goals

- Implement scheduled-transition firing. (#251 owns the runtime — timer
  persistence, cluster ownership, work-stealing recovery, cancellation on
  state change, audit surface for armed / fired / cancelled events.)
- Add new `SMEventScheduledTransition*` SPI event types. (#251 scope.)
- Tighten import-time validation for unknown processor `Type` values.
  (#255's scope — closed in v0.8.0 already.)
- Modify the vendored upstream OpenAPI copies under `docs/cyoda/`. They
  are point-in-time snapshots maintained upstream.
- Add a v0.8.0 SPI tag in isolation. The bundled v0.8.0 milestone tag at
  the end of the release cycle covers this change together with the
  deferred #250 docstring, #260's internalized SPI shape, and any other
  v0.8.0 SPI work that lands in the same window.

## 4. Decision summary (post-brainstorm)

| Question | Decision |
|---|---|
| Where does scheduling data live? | **Optional `Schedule *TransitionSchedule` field on `TransitionDefinition`.** Presence marks the transition as scheduled. One declaration encodes both "what fires" and "when". Engine state-entry hook (in #251) reads `tr.Schedule` to arm a timer. |
| Cascade semantics for `Schedule!=nil` (`cascadeAutomated`, `engine.go:528`) | **Silent skip.** Add `\|\| tr.Schedule != nil` to the existing `Disabled \|\| Manual` skip filter. No audit event. An entity whose only exit is scheduled rests in the source state. |
| Explicit-fire semantics for `Schedule!=nil` (`attemptTransition`, `engine.go:443-453`) | **Reject with `WORKFLOW_FAILED 400`.** Free-string error → `classifyWorkflowError` default branch returns `Operational(400, WORKFLOW_FAILED, msg)`. Entity stays in source state. Single `SMEventStateProcessResult{success:false}` audit row. |
| Validator coherence (`validate.go:178-211`) | **Three new rules** inside the existing per-transition loop, placed between the Next-state check (line 193) and the processor loop (line 198): (a) `Manual && Schedule != nil` → reject; (b) `Schedule != nil && DelayMs <= 0` → reject; (c) `Schedule != nil && TimeoutMs > 0 && TimeoutMs < DelayMs` → reject. All `VALIDATION_FAILED 400`. |
| `TimeoutMs == 0` semantics | **"No timeout."** Documented in SPI docstring and OpenAPI description. Allowed by the validator (the `TimeoutMs > 0` guard). |
| OpenAPI surface | New `TransitionScheduleDto` schema with `delayMs` (required, `minimum: 1`) and `timeoutMs` (optional, `minimum: 0`). New optional `schedule: $ref` field on `TransitionDefinitionDto`. Regenerate `api/generated.go`. |
| Audit-event shape for explicit-fire reject | **Single `SMEventStateProcessResult{success:false}`** with a Details string naming the transition and the rejection cause. Reuses the existing event type (no new SPI events). |
| Error wording | **Self-contained**, no issue ID. Validator messages name the rule violated; engine error names the transition, source state, and rejection cause. |
| SPI PR bundling | **One SPI PR** combining (a) deferred `ProcessorDefinition.Type` docstring from #250, (b) new `TransitionSchedule` type, (c) new `Schedule` field on `TransitionDefinition`. CHANGELOG `[Unreleased]` entry covers all three. |
| SPI tagging | **None forced.** Bundled into v0.8.0's end-of-milestone tag per `MAINTAINING.md` "Coordinated release" cadence. cyoda-go pseudo-version-pins to SPI `main` HEAD across all four `go.mod` files per memory `project_v0_8_0_milestone_state`. |
| Cassandra plugin | **Untouched in this PR.** Additive SPI fields are non-breaking; courtesy refresh via the end-of-milestone tag, not from this PR (memory `feedback_courtesy_pr_scope`). |
| Source of truth for field names | **OpenAPI and SPI agreed on lowercase camelCase wire form** (`schedule`, `delayMs`, `timeoutMs`). Go-side: `Schedule`, `DelayMs`, `TimeoutMs`. |

## 5. Detailed design

### 5.1 OpenAPI changes (`api/openapi.yaml`)

New schema (insert near `TransitionDefinitionDto`, alphabetical placement
in `components.schemas`):

```yaml
TransitionScheduleDto:
  type: object
  description: |
    Scheduling configuration for an automatic future state transition.
    Presence of this object marks the parent transition as scheduled —
    it fires automatically delayMs milliseconds after the entity enters
    the source state. Mutually exclusive with parent transition's
    manual=true.

    The runtime that arms and fires the timer is not yet implemented.
    Until it ships, the engine silently skips scheduled transitions
    during automated cascade selection, and explicit fires by name are
    rejected with HTTP 400 WORKFLOW_FAILED.
  properties:
    delayMs:
      type: integer
      format: int64
      minimum: 1
      description: |
        Delay before firing, in milliseconds. Must be a positive integer.
    timeoutMs:
      type: integer
      format: int64
      minimum: 0
      description: |
        Maximum age of the armed timer before abandonment, in
        milliseconds. 0 (default) means no timeout. When non-zero, must
        be greater than or equal to delayMs.
  required:
    - delayMs
```

Add an optional property to `TransitionDefinitionDto`:

```yaml
schedule:
  $ref: "#/components/schemas/TransitionScheduleDto"
  description: |
    Optional scheduling configuration. Presence marks the transition as
    scheduled; mutually exclusive with manual=true. Runtime not yet
    implemented — see TransitionScheduleDto for v0.8.0 engine behaviour.
```

The `schedule` property is **not added to `required`**. Its absence
denotes a regular transition.

### 5.2 Generated DTOs (`api/generated.go`)

Regenerated from the updated `openapi.yaml`. New entries:

- `type TransitionScheduleDto struct { ... }` with fields `DelayMs int64`
  and `TimeoutMs *int64` (pointer because non-required).
- `TransitionDefinitionDto.Schedule *TransitionScheduleDto`.

Validation spike (per `make generate`):

1. `make generate` runs to completion without error.
2. `go build ./...` is clean post-regeneration.
3. `api/generated.go` has `TransitionScheduleDto` and the new field on
   `TransitionDefinitionDto`.
4. No legacy `ScheduledTransition*Dto` symbols reappear (sanity check).

### 5.3 SPI changes (`cyoda-go-spi/types.go`)

Repo: `https://github.com/Cyoda-platform/cyoda-go-spi`. Co-located at
`../cyoda-go-spi`. Single bundled PR with three changes:

**(a) New `TransitionSchedule` type.** Inserted alongside the other
workflow types (after `TransitionDefinition`, before the
`SMEvent*` constants block):

```go
// TransitionSchedule configures automatic firing of a future state
// transition. Presence of this struct on a TransitionDefinition marks
// the transition as scheduled: the engine arms a timer when the entity
// enters the source state, and the transition fires automatically after
// DelayMs milliseconds.
//
// Scheduled transitions are mutually exclusive with Manual=true.
//
// The runtime that arms and fires the timer is not yet implemented.
// Engine behaviour in the absence of that runtime is defined by the
// cyoda-go engine package: scheduled transitions are silently skipped
// during automated cascade selection, and explicit fires by name are
// rejected with HTTP 400.
type TransitionSchedule struct {
    // DelayMs is the delay before firing, in milliseconds. Must be > 0.
    DelayMs int64 `json:"delayMs"`

    // TimeoutMs is the maximum age of the armed timer before abandonment,
    // in milliseconds. 0 means no timeout. When non-zero, must be >=
    // DelayMs.
    TimeoutMs int64 `json:"timeoutMs,omitempty"`
}
```

**(b) New `Schedule` field on `TransitionDefinition`.** Appended after
`Processors`:

```go
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

Pointer (not value) because `omitempty` on a non-pointer struct doesn't
omit the JSON field for zero values — that would emit
`"schedule":{"delayMs":0,"timeoutMs":0}` for every transition. The
pointer makes "absent vs zero-default" round-trippable.

**(c) Deferred `ProcessorDefinition.Type` docstring** from #250 spec
§5.3, sitting on branch `spi-docstring-processor-type-250` at commit
`cc715ef`. Apply verbatim. Quoted here for completeness (no semantic
divergence):

```go
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

**CHANGELOG entry.** `cyoda-go-spi/CHANGELOG.md` `[Unreleased]` section
gains:

```markdown
### Added
- `TransitionSchedule` type + `TransitionDefinition.Schedule` field for
  the v0.8.0 scheduled-transition shape carve-out (cyoda-go #259).
  Runtime not yet wired — see cyoda-go #251 for full feature tracking.

### Changed
- Document `ProcessorDefinition.Type` field (deferred from cyoda-go #250
  per its spec §5.3, intentionally bundled with the first substantive
  SPI carve-out).
```

**Cross-repo coordination.** This SPI PR contains additive surface
(one new exported type, one new field on an existing exported type,
one docstring update). Per `MAINTAINING.md` pre-1.0 policy, additive
is non-breaking. No SPI tag is cut for this PR alone; the v0.8.0
milestone tag at end-of-cycle captures it together with #260 and any
other v0.8.0 SPI changes (per memory `project_v0_8_0_milestone_state`
the cyoda-go side pseudo-version-pins to SPI `main` HEAD during the
milestone flight).

### 5.4 Validator (`internal/domain/workflow/validate.go`)

Three new checks inside the existing per-transition loop in
`validateWorkflowStructure` (lines 178–211 today). Insertion point is
between the Next-state check (line 193–196) and the processor loop
(line 198).

```go
// Schedule shape coherence.
if tr.Manual && tr.Schedule != nil {
    return fmt.Errorf(
        "workflow %q state %q transition %q: manual and scheduled are mutually exclusive",
        wf.Name, stateName, tr.Name)
}
if tr.Schedule != nil {
    if tr.Schedule.DelayMs <= 0 {
        return fmt.Errorf(
            "workflow %q state %q transition %q: schedule.delayMs must be > 0 (got %d)",
            wf.Name, stateName, tr.Name, tr.Schedule.DelayMs)
    }
    if tr.Schedule.TimeoutMs > 0 && tr.Schedule.TimeoutMs < tr.Schedule.DelayMs {
        return fmt.Errorf(
            "workflow %q state %q transition %q: schedule.timeoutMs (%d) must be >= schedule.delayMs (%d)",
            wf.Name, stateName, tr.Name, tr.Schedule.TimeoutMs, tr.Schedule.DelayMs)
    }
}
```

All three errors propagate through the existing import handler to a
`400 VALIDATION_FAILED` response (`handler.go:65-90` import path; the
specific error code is set by the boundary classification).

`validateProcessorFlags` is **unchanged.** It still enforces only
`StartNewTxOnDispatch` / `ExecutionMode` coherence.

`validateWorkflowLoops` (`validate.go:242-295`) is **unchanged.** DFS
cycle detection skips `Manual` and `Disabled` transitions today; per
§5.5 cascade semantics, scheduled transitions are similarly never fired
automatically in v0.8.0. The cycle detector's role is to prevent
infinite cascade — adding scheduled to its skip filter is unnecessary
because the engine cascade skip already removes them from consideration,
and adding the skip in the validator would falsely allow workflows that
become cycle-unsafe once #251 lands. Leaving the cycle detector intact
preserves a forward-compatible safety property.

### 5.5 Engine guards (`internal/domain/workflow/engine.go`)

**(a) Cascade-skip** at `engine.go:528`. One-line addition to the
existing skip filter:

```go
// before
if tr.Disabled || tr.Manual {
    continue
}

// after
if tr.Disabled || tr.Manual || tr.Schedule != nil {
    continue
}
```

No audit event. This matches the existing silent-skip semantics for
`Disabled` and `Manual` transitions. The cascade loop iterates state
transitions for automated firing; scheduled transitions are by
definition not eligible for immediate automated firing — they wait for
their timer. Until the timer runtime ships, they remain ineligible
forever, which is the correct degenerate behaviour for v0.8.0.

**(b) Explicit-fire reject** at `engine.go:443-453` precedent. New
branch immediately after the `Disabled` check (currently the rejection
for `transition.Disabled` at lines 449-453), before criterion evaluation:

```go
if transition.Schedule != nil {
    e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
        spi.SMEventStateProcessResult,
        fmt.Sprintf(
            "Transition %q in state %q is scheduled; scheduled transitions are not yet implemented",
            transitionName, entity.Meta.State),
        map[string]any{"success": false})
    return ctx, txID, fmt.Errorf(
        "transition %q in state %q is scheduled; scheduled transitions are not yet implemented",
        transitionName, entity.Meta.State)
}
```

**Failure surface.** The wrapped error wraps no specific sentinel.
`classifyWorkflowError` (`internal/domain/entity/service.go:1449-1457`
per #250 spec §5.4) falls through to its default branch and returns
`common.Operational(http.StatusBadRequest, common.ErrCodeWorkflowFailed,
err.Error())`. HTTP response: `400 WORKFLOW_FAILED`. Entity stays in
source state because the function returns before line 486
(`entity.Meta.State = transition.Next`).

**Audit-event shape.** The explicit-fire reject emits one
`SMEventStateProcessResult` with `Data: {success: false}` and a Details
string naming the transition, source state, and rejection cause.
Reusing the existing event type (no new SPI event constants) is the
deliberate choice — the rejection is fundamentally a "transition could
not complete" failure mode, structurally identical to the post-#250
internalized-processor failure path that also emits
`SMEventStateProcessResult{success:false}`. An audit consumer searching
for failure events does not need a new event-type vocabulary; the
Details substring `"scheduled transitions are not yet implemented"` is
the unique fingerprint. The calling handler's failure path
(`engine.go:474-477` for manual, `:562-566` for cascade) emits its own
follow-up `SMEventStateProcessResult` per the existing two-event
shape — but it is **never reached** for this rejection because the new
branch sits earlier in `attemptTransition` (before
`executeProcessors`), and the cascade path silently skips and never
calls `attemptTransition` on a `Schedule!=nil` entry. Net audit trail
for the explicit-fire reject: **one** `SMEventStateProcessResult` row.

**Operator-side slog.** No `slog.Warn`. Consistent with #250 spec §5.4:
fatal failure paths write the audit event and the API error; they do
not duplicate to slog. The `WORKFLOW_FAILED 400` response is sufficient
operator-facing signal.

**No issue IDs in the error message.** The message names the transition
and the absence of an implementation; it does not link to a tracker.

### 5.6 Help text (`cmd/cyoda/help/content/workflows.md`)

Add a new subsection after the existing per-transition field listing
(after the closing of the `processors[]` discussion around line 175).
Suggested heading: `### Scheduled transitions (v0.8.0: shape only)`.

Content:

```markdown
### Scheduled transitions (shape only — runtime not yet implemented)

A transition may carry an optional `schedule` object marking it as
scheduled. Presence of `schedule` declares that the transition fires
automatically `delayMs` milliseconds after the entity enters the
source state. The `schedule` shape is:

```json
{
  "name": "AutoClose",
  "next": "Closed",
  "manual": false,
  "schedule": {
    "delayMs": 86400000,
    "timeoutMs": 90000000
  }
}
```

**Fields:**

- `delayMs` (integer, required) — delay before firing, in milliseconds.
  Must be `> 0`.
- `timeoutMs` (integer, optional) — maximum age of the armed timer
  before abandonment, in milliseconds. `0` (default) means no timeout.
  When non-zero, must be `>= delayMs`.

**Import-time validation rules:**

- `manual: true` and `schedule` present are mutually exclusive
  (`VALIDATION_FAILED`).
- `schedule.delayMs <= 0` is rejected (`VALIDATION_FAILED`).
- `schedule.timeoutMs > 0 && schedule.timeoutMs < schedule.delayMs` is
  rejected (`VALIDATION_FAILED`).

**v0.8.0 engine behaviour.** The timer runtime that arms and fires
scheduled transitions is not yet implemented. Two guards govern
behaviour until it ships:

- During automated cascade evaluation, scheduled transitions are
  silently skipped — they are never selected for immediate firing.
  Other automated/manual transitions out of the same state fire
  normally. An entity whose only exit is scheduled rests in its
  current state.
- Explicitly firing a scheduled transition by name returns HTTP 400
  `WORKFLOW_FAILED` with the message `transition "X" in state "Y" is
  scheduled; scheduled transitions are not yet implemented`. The
  entity remains in the source state.
```

### 5.7 Parity scenario (`e2e/externalapi/scenarios/08-workflow-import-export.yaml`)

Add a new test entry (`wf-import/04` or next free slot) exercising
round-trip of a transition with `schedule`. Two import payloads:

1. **Round-trip preservation.** A workflow with one regular and one
   scheduled transition. Assert `json_equals_normalized` round-trip
   equality on import + export.
2. **Validator rejection.** Three sub-cases (`manual: true + schedule`,
   `delayMs: 0`, `timeoutMs < delayMs`) — each asserts HTTP 400 with
   `VALIDATION_FAILED` and the rule-specific error substring.

The existing `wf-import/03` test (the post-#250 group-criterion +
function-criterion + externalized-processor case) is **unchanged.** No
follow-up edit there.

### 5.8 Docs hygiene & test-fixture sweep (Gate 4 + Gate 6)

- **`README.md`** — grep confirmed no scheduled-transition or
  `delayMs` references. No change required; re-verify as hygiene.
- **`CLAUDE.md`** — grep confirmed clean. No change.
- **`docs/PROCESSOR_EXECUTION_MODES.md`** — out of scope; this doc
  covers processor `type` × `executionMode`, not scheduled transitions.
- **`COMPATIBILITY.md`** — untouched. SPI tag bumps at end-of-milestone;
  this PR doesn't force a tag.
- **`docs/cyoda/openapi.yml`** and vendored copies — explicitly out of
  scope (point-in-time Cloud snapshots maintained upstream).
- **`docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md`** — §H1 already references
  #259 as the carve-out. After this PR merges, the audit's tracking is
  accurate; no edit required.
- **Test-fixture sweep (`internal/domain/workflow/*_test.go`).** Grep
  for `schedule`, `delayMs`, `timeoutMs`, `Schedule`:
  ```bash
  grep -rn 'schedule\|delayMs\|timeoutMs' --include="*_test.go" \
       internal/domain/workflow/
  ```
  Expected matches (intentional, leave as-is):
  - `engine_processors_type_test.go:147` — comment annotating
    `"scheduled"` as a legacy value removed by #250.
  - `engine_processors_type_test.go:152` — `"scheduled"` string in
    `TestType_UnknownValues_FallThrough`.
  - `engine_processors_type_test.go:308-313` — comment + test fixture
    for `"scheduled"` as an unknown-Type round-trip value.

  These are #250's intentional fall-through coverage — they prove that
  the SPI's free-string `Type` accepts a legacy `"scheduled"` value
  without rejection. New tests added by §6.1 cover the new
  `Schedule` field and do not regress these.

## 6. Test plan (TDD)

The TDD protocol (`.claude/rules/tdd.md`) drives this work: tests are
authored first, run to confirm RED, then driven GREEN.

### 6.1 Failing-test-first sequence

1. **Validator — manual + schedule coherence.** Import payload with
   `Manual: true, Schedule: {DelayMs: 1000}`. Assert
   `400 VALIDATION_FAILED`, error substring
   `"manual and scheduled are mutually exclusive"`.

2. **Validator — DelayMs <= 0.** Import payload with `Schedule:
   {DelayMs: 0}` and another with `DelayMs: -100`. Both assert
   `400 VALIDATION_FAILED`, error substring `"delayMs must be > 0"`.

3. **Validator — TimeoutMs < DelayMs (non-zero).** Import payload with
   `Schedule: {DelayMs: 1000, TimeoutMs: 500}`. Assert
   `400 VALIDATION_FAILED`, error substring
   `"timeoutMs (500) must be >= delayMs (1000)"`.

4. **Validator — happy paths.** Three sub-cases that must pass import:
   - `Schedule: {DelayMs: 1000}` (TimeoutMs omitted)
   - `Schedule: {DelayMs: 1000, TimeoutMs: 0}` (TimeoutMs explicit zero)
   - `Schedule: {DelayMs: 1000, TimeoutMs: 5000}` (TimeoutMs > DelayMs)

5. **Cascade — silent skip.** Workflow with state `S` containing two
   automated transitions: `Tsched` (scheduled, declared first) and
   `Treg` (regular, declared second, leading to `T`). Cascade enters
   `S`, must select `Treg`. Assert: entity ends in `T`; no audit event
   for `Tsched` (no `SMEventTransitionCriterionNoMatch`, no
   `SMEventStateProcessResult`, no skip-reason event); the
   `Tsched` transition is invisible to selection.

6. **Cascade — only-scheduled rests.** Workflow with state `S`
   containing one automated transition that is scheduled, and no other
   exits. Cascade enters `S`, must rest there. Assert: entity in `S`;
   `cascadeAutomated` returns no error; no `SMEventCancelled`.

7. **Explicit-fire reject — manual=false + Schedule.** Workflow as test
   5 but the scheduled transition is fired by name. Assert:
   - HTTP `400 WORKFLOW_FAILED`.
   - Error body contains `"scheduled transitions are not yet implemented"`.
   - Entity in source state (`Get` returns unchanged state).
   - Audit trail contains exactly one `SMEventStateProcessResult` with
     `Data.success == false` and Details matching the rejection
     substring. No `SMEventTransitionMade`, no
     `SMEventStateMachineFinish`.

8. **Round-trip preservation.** Import a workflow with a transition
   carrying `Schedule: {DelayMs: 86400000, TimeoutMs: 90000000}`,
   export, assert the exported JSON contains
   `"schedule":{"delayMs":86400000,"timeoutMs":90000000}` byte-for-byte
   on the round-trip. Variant: `Schedule: {DelayMs: 1000}` (no
   TimeoutMs) — assert exported JSON contains
   `"schedule":{"delayMs":1000}` (omitempty drops the zero
   `timeoutMs`).

9. **E2E — explicit fire of scheduled transition returns 400.**
   Integration test in `internal/e2e/` mirroring #250's test 8 shape:
   1. Create an entity model.
   2. `POST /model/X/1/workflow/import` with a workflow whose initial
      state has exactly one transition: `{name: "AutoClose", next:
      "Closed", manual: false, schedule: {delayMs: 1000}}`. Assert
      HTTP 200 (the validator accepts shape-coherent scheduled
      transitions).
   3. `POST /entity/X` to create an instance — entity lands in the
      initial state because the cascade silently skips the scheduled
      transition and finds no other automated exit.
   4. `POST /entity/X/<id>/transition/AutoClose` — assert HTTP 400
      with error code `WORKFLOW_FAILED` and message substring
      `scheduled transitions are not yet implemented`.
   5. `GET /entity/X/<id>` returns the entity in the initial state
      (cascade rested there; explicit fire rejected).

10. **Parity — scenario 08/wf-import/04 round-trip passes.** Per §5.7.

### 6.2 Coverage gaps explicitly NOT closed in #259

- Timer runtime behaviour: arming, firing, cancellation, durability
  across restarts, cluster ownership, work-stealing recovery. (#251
  scope.)
- Audit-event semantics for armed/fired/cancelled. (#251 scope.)
- Interaction tie-breaking when manual non-scheduled and scheduled
  transitions coexist in the same state with overlapping firing
  windows. (#251 scope — currently moot because v0.8.0 silently skips
  scheduled transitions.)
- Cycle detection awareness of scheduled transitions. The DFS in
  `validateWorkflowLoops` does not skip scheduled transitions; the
  conservative choice is to preserve cycle detection's existing
  reachability assumption so workflows that would become cycle-unsafe
  once #251 lands are caught at v0.8.0 import time. (Documented in
  §5.4.)

## 7. Backwards compatibility

| Inbound payload shape | Behaviour before #259 | Behaviour after #259 |
|---|---|---|
| `TransitionDefinitionDto` with no `schedule` | Accepted, fired normally | Same |
| `TransitionDefinitionDto.schedule: {delayMs: N>0}` | Field unknown to OpenAPI; silently dropped by `json.Unmarshal` into `spi.TransitionDefinition`; transition behaves as a regular one | **Accepted, stored, round-tripped.** Validator enforces the three coherence rules. Cascade silently skips; explicit-fire returns 400. |
| `TransitionDefinitionDto.schedule: {delayMs: 0}` | Silently dropped | **Rejected at import** with `VALIDATION_FAILED`. |
| `TransitionDefinitionDto` with `manual: true, schedule: {...}` | Silently dropped (schedule lost) | **Rejected at import** with `VALIDATION_FAILED`. |
| Legacy `processors[]` with `{type: "scheduled", config: {delayMs, transition, timeoutMs}}` (raw-JSON path; pre-#250 shape) | After #250: SPI's free-string `Type` accepts; scheduled config fields silently dropped by `ProcessorConfig` unmarshal; dispatched as externalized | **Same as post-#250.** The new `TransitionDefinition.Schedule` field does not interact with `Processors[].Type` or `ProcessorConfig`. Carried-over technical debt from §H1's silent-drop. |

The wire surface is **strictly wider** in exactly one place
(`TransitionDefinitionDto.schedule` is a new optional field on a
previously-fixed schema). All existing payloads continue to parse and
behave identically. No upgrade migration required: workflows persisted
under v0.7.x or pre-#250 v0.8.0 carry no `Schedule` field; new imports
under v0.8.0 may add it; both round-trip cleanly.

**Cyoda Cloud parity.** Cloud regenerates its OpenAPI client at v0.8.0
and picks up the new `TransitionScheduleDto` and the `schedule` field
together with #260's `internalized` subtype and #250's docstring update.
Cloud-side runtime for scheduled transitions remains a Cloud-only
feature until cyoda-go's #251 lands; in the meantime, Cloud-emitted
workflows with `schedule` round-trip cleanly through cyoda-go (the
shape is now well-defined) and become inert at runtime per §5.5.

## 8. Risks

- **`*TransitionSchedule` pointer JSON edge case.** A pointer with all
  fields zero-valued (`&TransitionSchedule{}`) marshals as
  `"schedule":{"delayMs":0,"timeoutMs":0}` (omitempty drops the
  `timeoutMs:0` but not the empty struct itself). If callers construct
  such a value, the validator catches it via the `DelayMs <= 0` rule.
  No silent acceptance.
- **OpenAPI generator behaviour for the new `schedule` field.** Some
  `oapi-codegen` versions emit `*TransitionScheduleDto` (pointer) for
  optional non-required object fields, matching the SPI pointer choice.
  Verify in the regeneration spike (§5.2). If a value-type is emitted,
  there is no behavioural divergence (zero-value still goes through
  validator), but the generated code may need a manual nudge.
- **Cycle-detector blind spot.** `validateWorkflowLoops` does not
  account for scheduled transitions. Once #251 ships, a workflow with
  `S1 --scheduled--> S2 --scheduled--> S1` becomes a delayed-cycle.
  The cycle detector today treats both edges as eligible and rejects.
  This is the conservative choice (preserve detection); revisit when
  #251 lands.
- **`*int64` vs `int64` for `TimeoutMs` round-trip.** Choosing `int64`
  with `omitempty` (per §5.3) means the JSON omits `timeoutMs` only
  when the Go value is exactly 0. A client that explicitly sets
  `timeoutMs: 0` in import JSON, exports, and gets back JSON without
  `timeoutMs` is technically a lossy round-trip — but the SPI and
  OpenAPI both document `0 == no timeout`, so this is semantically
  equivalent. Round-trip preservation test 8 (§6.1) pins the
  byte-exact shape for non-zero values; the zero-value case is
  semantically equivalent both directions.
- **SPI tag coupling.** #259 bundles the deferred #250 docstring; if
  this PR is reverted, #250's docstring is also lost from SPI main.
  Acceptable because the bundling is intentional per #250 spec §5.3,
  and a revert would similarly carry the new shape work.

## 9. Out of scope (sanity check)

- Scheduled-transition runtime (timer arming, firing, durability,
  cluster ownership, work-stealing recovery, cancellation on state
  change) — #251.
- `SMEventScheduledTransition*` SPI event types — #251.
- Internalized processor execution-location shape + SPI — #260.
- Import-time validation tightening for other shape fields — #255
  (already closed in v0.8.0).
- `ProcessorConfig.Context` / `RetryPolicy` wiring — #253, #254, #262.
- `WorkflowStore.CompareAndSave` SPI addition — #35.
- Vendored upstream OpenAPI copies under `docs/cyoda/`.
- Cassandra plugin coordination — additive SPI; refresh via end-of-milestone
  tag bump, not from this PR.

## 10. Cross-repo coordination

Per memory `feedback_spi_coordinated_release_procedure` and
`cyoda-go-spi/MAINTAINING.md` "Coordinated release across sibling
repos":

1. **cyoda-go-spi PR first.** Bundled change: deferred #250 docstring +
   `TransitionSchedule` type + `Schedule` field on `TransitionDefinition`.
   `CHANGELOG.md` `[Unreleased]` entry per §5.3. Merged to `main`. No
   tag.

2. **cyoda-go PR second.** Steps:
   - Refresh pseudo-version pin to SPI `main` HEAD across all four
     `go.mod` files (root + `plugins/memory|sqlite|postgres`).
   - Implement validator rules + engine guards + tests + help text +
     parity scenario.
   - Regenerate `api/generated.go` from the updated `openapi.yaml`.
   - Single PR targeting `release/v0.8.0` per memory
     `feedback_release_branch_for_milestones`.

3. **Cassandra plugin (commercial backend, separate repo).** Additive
   SPI is non-breaking; no per-PR refresh required. End-of-milestone
   v0.8.0 SPI tag triggers a courtesy bump in the cassandra repo per
   memory `feedback_courtesy_pr_scope` — strictly in-scope, no
   drive-by fixes.

4. **PR body conventions.**
   - cyoda-go-spi PR: title `feat(types): scheduled-transition shape +
     ProcessorDefinition.Type docstring`. Body references cyoda-go
     #259 (parent) and #250 (deferred docstring origin). No issue IDs
     in the docstring text or CHANGELOG entry — only in the PR body
     and commit message.
   - cyoda-go PR: title `feat(workflow): scheduled-transition
     configuration shape + SPI`. Body has `Closes #259`. Milestone:
     `v0.8.0`. Target branch: `release/v0.8.0`. Per memory
     `feedback_release_milestone_invariant`, ensure the milestone is
     applied at PR-merge time.

---

## Appendix A: File-level impact summary

| File | Change |
|---|---|
| `cyoda-go-spi/types.go` (sibling repo) | Add `TransitionSchedule` struct + `TransitionDefinition.Schedule` field; add docstring on `ProcessorDefinition.Type` from `spi-docstring-processor-type-250` branch. |
| `cyoda-go-spi/CHANGELOG.md` (sibling repo) | Add `[Unreleased]` Added + Changed entries per §5.3. |
| `api/openapi.yaml` | Add `TransitionScheduleDto` schema; add optional `schedule` property to `TransitionDefinitionDto`. |
| `api/generated.go` | Regenerated from above. |
| `internal/domain/workflow/validate.go` | Add three Schedule-coherence checks in `validateWorkflowStructure` per-transition loop (between lines 193 and 198). |
| `internal/domain/workflow/engine.go` | Cascade-skip filter at line 528 (one-line addition); explicit-fire reject branch in `attemptTransition` after the `Disabled` check (between lines 453 and 455). |
| `internal/domain/workflow/validate_test.go` (or sibling) | TDD tests per §6.1 items 1–4. |
| `internal/domain/workflow/engine_test.go` (or sibling) | TDD tests per §6.1 items 5–8. |
| `internal/e2e/` | E2E test per §6.1 item 9. |
| `cmd/cyoda/help/content/workflows.md` | New `### Scheduled transitions` subsection per §5.6. |
| `e2e/externalapi/scenarios/08-workflow-import-export.yaml` | Add `wf-import/04` round-trip + reject scenario per §5.7. |
| `README.md`, `CLAUDE.md`, `COMPATIBILITY.md`, `docs/PROCESSOR_EXECUTION_MODES.md` | Untouched. Re-grep as Gate 4 hygiene. |
| `docs/cyoda/openapi.yml` and vendored copies | Out of scope. |
| `go.mod` (root + `plugins/memory|sqlite|postgres`) | Bump pseudo-version pin to cyoda-go-spi `main` HEAD post-SPI-PR-merge. |

## Appendix B: Verification before completion

Per `superpowers:verification-before-completion`:

- `go build ./...` — clean.
- `go vet ./...` — clean (root and per-plugin per
  `per-module-hygiene` CI job).
- `go test -short ./...` — green.
- `go test ./internal/e2e/...` — green (Docker required for the
  testcontainers Postgres backend).
- Parity scenario suite — green.
- `go test -race ./...` — one-shot before PR creation per
  `.claude/rules/race-testing.md`.
- `make test-all` — covers root + `plugins/memory|sqlite|postgres` per
  CLAUDE.md.
- `make todos` — no new TODOs introduced.

## Appendix C: Out-of-tree plugin impact

The cyoda-go-cassandra plugin (commercial backend) consumes the SPI.
The added `TransitionSchedule` type and `Schedule` field are additive
to the workflow types — Cassandra's `WorkflowStore` stores
`[]WorkflowDefinition` opaquely (per the SPI conformance test
`spitest/workflow.go`), so no behavioural change is required. Per
memory `feedback_cross_plugin_design_verification` the design check is:
does the Cassandra plugin read fields of `TransitionDefinition`
beyond what's needed for storage? Answer: no — the SPI conformance
test in `cyoda-go-spi/spitest/workflow.go` exercises `Save`/`Get`/`Delete`
only; the plugin treats `WorkflowDefinition` as a value type without
field-level inspection. The new field round-trips cleanly through any
conforming implementation.
