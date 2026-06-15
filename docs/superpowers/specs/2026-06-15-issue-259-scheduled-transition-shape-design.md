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
> - **Revision 2 (2026-06-15):** incorporated independent design review.
>   Six load-bearing / medium defects fixed: (L1) parity-scenario slot
>   collision — `wf-import/04` was already populated, switched to
>   `wf-import/07`; (L2) explicit acknowledgement that acceptance bullet 5's
>   "link to #251" wording is satisfied by self-contained "not yet
>   implemented" phrasing per project policy on no issue IDs in shipped
>   artefacts; (L3) §8 risk bullet 1 rewritten to separate wire-decoded
>   from in-process-construction paths; (L4) cycle-detector behaviour kept
>   unchanged (rev 1's reasoning was inverted but the rejection IS correct
>   — once #251 lands, a scheduled-only cycle IS an infinite loop), and a
>   new request-level `allowCycles` escape hatch added so polling-pattern
>   workflows have a documented bypass; (M1) help-text heading lifted to
>   `## SCHEDULED TRANSITIONS` (level 2) to match `## PROCESSORS` /
>   `## CRITERIA`; (M2) version label dropped from the heading; (M6)
>   round-trip test 8 switched from byte-exact substring match to
>   `json_equals_normalized`; (M7) one-line clarification that the
>   Schedule rules sit in `validateWorkflowStructure` (incoming-only) by
>   design; (O1) conditional note on the bundled #250 docstring (drop if
>   #260 lands first); (O2, O3) editorial trim. M3 (audit-event choice
>   for explicit-fire reject) and M4 (TimeoutMs scope) preserved at rev 1
>   pending owner review.

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
- Add a request-level `allowCycles` boolean to the workflow import body
  as an escape hatch for unguarded automated cycles (the canonical
  scheduled-transition use case is a polling pattern `S1 →scheduled→ S2
  →scheduled→ S1` which the existing `validateWorkflowLoops` rejects).
  The flag bypasses only `validateWorkflowLoops`; all other validators
  (the three new Schedule rules, processor flags, structural checks)
  remain unconditional. Request-level (not workflow-level) — each
  import re-asserts intent; the durable schema does not carry a
  "skip safety check" knob. Runtime safety net (`maxStateVisits`,
  `maxCascadeDepth`) is unchanged and catches actual runaway at fire
  time.

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
| `allowCycles` escape hatch (added rev 2) | **Request-level `bool` on the import body envelope**, default `false`. Bypasses only `validateWorkflowLoops`. Default-false behaviour is byte-identical to today; the flag is purely opt-in. Composes with `importMode`. Not persisted on `WorkflowDefinition`. |
| `allowCycles` operator signal | **One `slog.Warn`** per import call when `allowCycles=true` is observed, naming the workflow(s) whose cycles were bypassed. No audit event (audit is for entity lifecycle, not import-time config). |
| Acceptance bullet 5's "link to #251" | **Deliberately satisfied by self-contained wording.** The shipped error / help text / OpenAPI description carry the phrase `"scheduled transitions are not yet implemented"` (no issue ID). Issue IDs are referenced only in the spec doc, PR body, and commit message per memory `feedback_no_issue_ids_in_code`. |

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

**Workflow-import request body** — add an optional `allowCycles` field
to the existing import-request schema (the exact name depends on the
current OpenAPI generator output; the schema is the request body of the
`POST /model/{name}/{version}/workflow/import` operation):

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
    scheduled transitions. Default false preserves the pre-v0.8.0
    rejection behaviour exactly.
```

The field is not added to `required`; absence is equivalent to `false`.

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

**Placement rationale (M7).** The three Schedule rules sit in
`validateWorkflowStructure`, which is invoked by `validateImportRequest`
on the incoming workflow set only — not on the merged-after-importMode
result. This matches `validateImportRequest`'s scope: structural shape
is a property of an individual workflow definition, asserted on what the
client sends. Cross-workflow concerns (cycle detection, name collisions
post-merge) sit in `validateWorkflows`, which runs on the merged result.
Schedule coherence is per-transition, so the incoming-only placement is
correct.

`validateProcessorFlags` is **unchanged.** It still enforces only
`StartNewTxOnDispatch` / `ExecutionMode` coherence.

**`validateWorkflowLoops` behaviour for scheduled transitions.** The
DFS today treats a `Manual=false, Disabled=false, Criterion=null`
transition as an unguarded automated edge — that includes any scheduled
transition with no criterion. A workflow `S1 →scheduled→ S2 →scheduled
→ S1` is rejected today and continues to be rejected by default after
this PR.

This is the correct default. Once #251 lands and the timer runtime
arms scheduled transitions, a delayed cycle becomes an actual infinite
loop — identical to today's unguarded automated cascade hazard, just
slowed by `DelayMs`. The cycle detector's job is to catch that. The
v0.8.0 cascade silent-skip (§5.5(a)) is a stopgap that hides the cycle
at runtime; the validator preserves the design-time invariant.

**`allowCycles` escape hatch.** Polling-pattern workflows
(`S1 →scheduled→ S2 →scheduled→ S1` with `S2`'s scheduled transition
gated on entity state, or unconditionally cyclic by author intent) are
a legitimate use case the cycle detector cannot distinguish from
accidental runaway. The import request grows an optional boolean
`AllowCycles` field that, when true, bypasses `validateWorkflowLoops`
for the duration of that import call only.

```go
// handler.go:32-46 (import request envelope)
type importRequest struct {
    ImportMode  string                   `json:"importMode"`
    AllowCycles bool                     `json:"allowCycles,omitempty"`
    Workflows   []spi.WorkflowDefinition `json:"workflows"`
}

// validate.go (signature change to validateWorkflows)
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

The handler's import path threads `req.AllowCycles` into the
`validateWorkflows` call (the existing call site in `handler.go`
around the post-merge validation step).

Other validators — `validateImportRequest` /
`validateWorkflowStructure` (including the three Schedule rules
above), `validateProcessorFlags` — remain **unconditional**. The flag
is specifically a cycle-detection bypass, not a general validation
override. A workflow with both an unguarded cycle and a
`Manual=true, Schedule!=nil` transition still gets rejected on the
Schedule rule with `AllowCycles=true`.

**Operator signal.** When `req.AllowCycles == true`, the handler emits
a single `slog.Warn` line per import call:

```go
slog.WarnContext(ctx, "workflow import: cycle validation bypassed",
    "pkg", "workflow",
    "entityName", entityName,
    "modelVersion", modelVersion,
    "importMode", req.ImportMode,
    "workflows", workflowNames)  // []string of req.Workflows[i].Name
```

One log line per import call — not per workflow — keeps audit volume
bounded. No SPI audit event (audit covers entity-lifecycle, not
import-time config). No log emission when `AllowCycles=false` or omitted.

**Runtime safety net unchanged.** `cascadeAutomated`'s `maxStateVisits`
(engine.go:513) and `maxCascadeDepth` (line 510) limits remain in
effect. A cyclic workflow that was force-imported and whose cycle
actually fires at runtime still aborts at the visit cap with the
existing `SMEventCancelled "state machine aborted: state X visited N
times"` event. The `allowCycles` flag only opens the import gate; it
does not weaken runtime runaway protection.

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

Add a new top-level subsection after the existing `## PROCESSORS`
discussion (before `## CRITERIA`). The heading level matches the
sibling sections `## PROCESSORS` and `## CRITERIA` — level 2, not
level 3, so the content is not visually orphaned under `## PROCESSORS`.

Content:

````markdown
## SCHEDULED TRANSITIONS

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

**Engine behaviour (runtime not yet implemented).** The timer runtime
that arms and fires scheduled transitions is not yet implemented. Two
guards govern behaviour until it ships:

- During automated cascade evaluation, scheduled transitions are
  silently skipped — they are never selected for immediate firing.
  Other automated/manual transitions out of the same state fire
  normally. An entity whose only exit is scheduled rests in its
  current state.
- Explicitly firing a scheduled transition by name returns HTTP 400
  `WORKFLOW_FAILED` with the message `transition "X" in state "Y" is
  scheduled; scheduled transitions are not yet implemented`. The
  entity remains in the source state.

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
shape rules (manual+schedule, delayMs, timeoutMs) and all other
validators remain unconditional. The runtime cascade-depth and per-
state visit caps still catch actual runaway at fire time. Use only
for workflows whose cyclicity is intentional.
````

### 5.7 Parity scenario (`e2e/externalapi/scenarios/08-workflow-import-export.yaml`)

Slots `wf-import/01..06` are already populated. Add new entries at the
next free slots. Three new scenarios:

- **`wf-import/07-scheduled-transition-roundtrip`** — round-trip
  preservation. A workflow with one regular and one scheduled
  transition (`Schedule: {DelayMs: 86400000, TimeoutMs: 90000000}` and
  another with `Schedule: {DelayMs: 1000}` to exercise the
  `TimeoutMs:0` omitempty path). Assert `json_equals_normalized`
  round-trip equality on import + export.
- **`wf-import/08-scheduled-transition-rejects`** — validator rejection
  matrix. Three sub-cases per the validator rules (`manual: true +
  schedule`, `delayMs: 0`, `timeoutMs < delayMs`); each asserts
  HTTP 400 `VALIDATION_FAILED` with the rule-specific error substring.
- **`wf-import/09-allowcycles-bypass`** — request-level cycle bypass.
  Workflow `S1 →scheduled→ S2 →scheduled→ S1`. Without
  `allowCycles: true` import returns `400 VALIDATION_FAILED` with the
  cycle error. With `allowCycles: true` import returns `200 OK` and
  round-trip preserves the workflow. The `allowCycles` field itself
  is request-only and does not appear in the exported workflow set.

The existing `wf-import/03` test (the post-#250 group-criterion +
function-criterion + externalized-processor case) and `wf-import/01..06`
are **unchanged.** No follow-up edits to the existing entries. (Free
slot enumeration confirmed at spec-write time; re-verify at
implementation time and pick the next-free integer prefix if the file
has gained entries since.)

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
- **Test-fixture sweep (`internal/domain/workflow/*_test.go`).** A grep
  for `schedule|delayMs|timeoutMs|Schedule` returns only #250's
  intentional fall-through coverage at `engine_processors_type_test.go`
  (lines 147, 152, 308-313, 313 — `"scheduled"` as a legacy-value
  unknown-Type round-trip fixture). Leave as-is; new tests added by
  §6.1 cover the `TransitionDefinition.Schedule` field.

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
   export, assert `json_equals_normalized` round-trip equality (parse
   both JSONs, compare structurally — robust against future field-
   ordering refactors of `TransitionDefinition`). Variant: `Schedule:
   {DelayMs: 1000}` (no TimeoutMs) — assert exported JSON's `schedule`
   object has `delayMs: 1000` and no `timeoutMs` key (the `omitempty`
   contract on the zero `TimeoutMs`).

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

10. **Parity — scenarios `wf-import/07..09` pass.** Per §5.7.

11. **`allowCycles` default-false rejects unguarded automated cycle.**
    Import payload `{importMode: "REPLACE", workflows: [<cyclic
    workflow>]}` — `allowCycles` omitted (default false). Workflow is
    `S1 →automated, no criterion→ S2 →automated, no criterion→ S1`
    (regular non-scheduled transitions). Assert `400 VALIDATION_FAILED`
    with cycle-detection error from `validateWorkflowLoops`.

12. **`allowCycles: true` bypasses the cycle rejection.** Same workflow
    as test 11, with `{importMode: "REPLACE", allowCycles: true,
    workflows: [...]}`. Assert `200 OK`; the workflow persists; round-
    trip preserves shape (the exported workflow set does not carry the
    `allowCycles` flag — it's request-only). Assert a `slog.Warn` line
    matching `"workflow import: cycle validation bypassed"` is emitted
    exactly once via a test log handler.

13. **`allowCycles: true` on polling scheduled-transition workflow
    succeeds.** Workflow `S1 →scheduled→ S2 →scheduled→ S1` (both
    transitions have `Manual: false, Schedule: {DelayMs: 1000}`,
    no criterion). Assert without `allowCycles`: `400 VALIDATION_FAILED`
    cycle error. With `allowCycles: true`: `200 OK`. Create an entity
    via `POST /entity/X`; assert entity lands in initial state and
    rests there (cascade silently skips both scheduled transitions, no
    cycle fires).

14. **`allowCycles: true` does NOT bypass Schedule-coherence rules.**
    Workflow with a cyclic shape AND a `Manual: true, Schedule:
    {DelayMs: 1000}` transition. Import with `allowCycles: true`.
    Assert `400 VALIDATION_FAILED` with the manual+schedule error —
    not the cycle error. Demonstrates that the bypass is scoped to
    `validateWorkflowLoops` only.

15. **Runtime safety net unchanged.** Import with `allowCycles: true`
    a cyclic regular (non-scheduled) workflow whose cycle actually
    fires at runtime (`S1 →automated→ S2 →automated→ S1`, no criteria,
    no schedule). Create an entity. Assert the cascade aborts at the
    per-state visit cap with `SMEventCancelled "state machine aborted:
    state ... visited ... times"`. (May piggyback an existing cascade-
    limit test rather than add a new one — the assertion is that the
    runtime guard still fires when the import-time guard is bypassed.)

### 6.2 Coverage gaps explicitly NOT closed in #259

- Timer runtime behaviour: arming, firing, cancellation, durability
  across restarts, cluster ownership, work-stealing recovery. (#251
  scope.)
- Audit-event semantics for armed/fired/cancelled. (#251 scope.)
- Interaction tie-breaking when manual non-scheduled and scheduled
  transitions coexist in the same state with overlapping firing
  windows. (#251 scope — currently moot because v0.8.0 silently skips
  scheduled transitions.)
- Per-workflow `AllowCycles` field on `WorkflowDefinition` (persistent
  escape hatch). Request-level was chosen deliberately so the durable
  schema does not carry a "skip safety check" knob. Revisit if
  operators need set-and-forget cyclicity declarations.
- Initial-state-with-only-scheduled-exits special-case. Covered
  incidentally by tests 6 and 9 (the entity simply rests in the
  initial state after create); no dedicated test.
- Scheduled transition with a non-null `criterion`. Spec accepts the
  combination (criterion is uninterpreted by v0.8.0 cascade because
  Schedule!=nil already takes precedence in the skip filter); a
  dedicated test is omitted for brevity, but reviewers may add one
  if the semantics warrant pinning.

## 7. Backwards compatibility

| Inbound payload shape | Behaviour before #259 | Behaviour after #259 |
|---|---|---|
| `TransitionDefinitionDto` with no `schedule` | Accepted, fired normally | Same |
| `TransitionDefinitionDto.schedule: {delayMs: N>0}` | Field unknown to OpenAPI; silently dropped by `json.Unmarshal` into `spi.TransitionDefinition`; transition behaves as a regular one | **Accepted, stored, round-tripped.** Validator enforces the three coherence rules. Cascade silently skips; explicit-fire returns 400. |
| `TransitionDefinitionDto.schedule: {delayMs: 0}` | Silently dropped | **Rejected at import** with `VALIDATION_FAILED`. |
| `TransitionDefinitionDto` with `manual: true, schedule: {...}` | Silently dropped (schedule lost) | **Rejected at import** with `VALIDATION_FAILED`. |
| Legacy `processors[]` with `{type: "scheduled", config: {delayMs, transition, timeoutMs}}` (raw-JSON path; pre-#250 shape) | After #250: SPI's free-string `Type` accepts; scheduled config fields silently dropped by `ProcessorConfig` unmarshal; dispatched as externalized | **Same as post-#250.** The new `TransitionDefinition.Schedule` field does not interact with `Processors[].Type` or `ProcessorConfig`. Carried-over technical debt from §H1's silent-drop. |
| Import request body with `allowCycles` field | Field unknown to importRequest struct; silently dropped by `json.Unmarshal` (Go default behaviour) | **Read as a bool;** when true, bypasses `validateWorkflowLoops` for that request. When false / omitted: byte-identical pre-#259 behaviour. |
| Import request body with cyclic workflow, no `allowCycles` flag | Rejected at import with `VALIDATION_FAILED` (cycle detection) | **Same** — default `allowCycles: false` preserves the rejection. |

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

- **`*TransitionSchedule` zero-value pointer handling — wire vs.
  in-process paths.** Two distinct concerns are sometimes conflated.
  (a) **Wire path.** A client POSTing `"schedule":{}` (empty object)
  unmarshals into `&TransitionSchedule{DelayMs: 0, TimeoutMs: 0}` —
  the validator's `DelayMs <= 0` rule catches this with
  `VALIDATION_FAILED`. Same for `"schedule":{"delayMs":0}`. No silent
  acceptance from any wire payload. (b) **In-process construction.**
  A direct Go-code construction of `&TransitionSchedule{}` in a test
  fixture or a future SPI consumer is a fixture-author concern, not a
  wire risk — fixture code is reviewed and tested directly. The
  validator catches it only if `validateImportRequest` runs on that
  path, which is the standard import handler path but not necessarily
  every internal code path. Author-discipline matter; not a system
  vulnerability.
- **OpenAPI generator behaviour for the new `schedule` field.** Some
  `oapi-codegen` versions emit `*TransitionScheduleDto` (pointer) for
  optional non-required object fields, matching the SPI pointer choice.
  Verify in the regeneration spike (§5.2). If a value-type is emitted,
  there is no behavioural divergence (zero-value still goes through
  validator), but the generated code may need a manual nudge.
- **Cycle-detector blind spot (resolved via `allowCycles`).** A
  workflow `S1 --scheduled--> S2 --scheduled--> S1` is rejected by
  `validateWorkflowLoops` today; once #251 ships and the timer arms
  scheduled transitions, that's an actual infinite loop, so the
  rejection is correct (delayed cycle = cycle). The request-level
  `allowCycles` escape hatch (§5.4) gives operators a documented path
  to import intentionally-cyclic workflows. No code change in
  `validateWorkflowLoops` itself.
- **`allowCycles: true` footgun.** Operators who set the flag bypass
  the import-time cycle check; a buggy workflow with an unintended
  unguarded cycle can be imported. Mitigations: (a) the runtime
  cascade safety net (`maxStateVisits`, `maxCascadeDepth`) catches
  actual runaway at fire time and emits `SMEventCancelled`; (b) one
  `slog.Warn` per import call surfaces the bypass to operators
  reviewing logs; (c) the flag is request-level only — each import
  re-asserts intent; the durable schema does not carry the bypass.
- **`*int64` vs `int64` for `TimeoutMs` round-trip.** Choosing `int64`
  with `omitempty` (per §5.3) means the JSON omits `timeoutMs` only
  when the Go value is exactly 0. A client that explicitly sets
  `timeoutMs: 0` in import JSON, exports, and gets back JSON without
  `timeoutMs` is technically a lossy round-trip — but the SPI and
  OpenAPI both document `0 == no timeout`, so this is semantically
  equivalent. Round-trip preservation test 8 (§6.1) uses
  `json_equals_normalized` rather than byte-exact substring matching,
  so this case is asserted to be semantically equivalent both
  directions.
- **SPI tag coupling.** #259 bundles the deferred #250 docstring; if
  this PR is reverted, #250's docstring is also lost from SPI main.
  Acceptable because the bundling is intentional per #250 spec §5.3,
  and a revert would similarly carry the new shape work.
- **#260 racing #259 on the docstring (O1).** If #260's SPI PR lands
  before #259's, #260 picks up #250's docstring under its own banner
  and #259's §5.3(c) becomes a no-op. The implementer of #259 should
  check SPI `main` immediately before opening the SPI PR; if the
  docstring is already present, drop §5.3(c) from #259's SPI commit
  set and update the CHANGELOG `[Unreleased]` entry to remove the
  "Changed" subsection. The substantive shape work (5.3(a), 5.3(b))
  is unaffected.

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
- Per-workflow `AllowCycles` field on `WorkflowDefinition` (persistent
  / round-tripped escape hatch). #259 lands request-level only.
- Per-transition cycle-allowance markers. Same reason.
- A general-purpose validation-bypass mechanism (e.g. an `unsafe: true`
  flag that disables all validators). `allowCycles` is deliberately
  cycle-specific; structural rules (Schedule coherence, processor
  flags, name uniqueness) remain unconditional.

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
| `internal/domain/workflow/validate.go` | Add three Schedule-coherence checks in `validateWorkflowStructure` per-transition loop (between lines 193 and 198); add `allowCycles bool` parameter to `validateWorkflows` and short-circuit `validateWorkflowLoops` when true. |
| `internal/domain/workflow/handler.go` | Add `AllowCycles bool` field to `importRequest` struct (lines 32-46); thread `req.AllowCycles` into the `validateWorkflows` call; emit `slog.Warn "workflow import: cycle validation bypassed"` when `req.AllowCycles == true`. |
| `internal/domain/workflow/engine.go` | Cascade-skip filter at line 528 (one-line addition); explicit-fire reject branch in `attemptTransition` after the `Disabled` check (between lines 453 and 455). |
| `internal/domain/workflow/validate_test.go` (or sibling) | TDD tests per §6.1 items 1–4, 11–15. |
| `internal/domain/workflow/engine_test.go` (or sibling) | TDD tests per §6.1 items 5–8. |
| `internal/e2e/` | E2E test per §6.1 item 9; additional E2E for `allowCycles` if not covered by the unit tests at items 11–15. |
| `cmd/cyoda/help/content/workflows.md` | New `## SCHEDULED TRANSITIONS` section per §5.6 (level 2, mirroring `## PROCESSORS` / `## CRITERIA`). |
| `e2e/externalapi/scenarios/08-workflow-import-export.yaml` | Add `wf-import/07..09` (round-trip, validator rejects, `allowCycles` bypass) per §5.7. |
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
