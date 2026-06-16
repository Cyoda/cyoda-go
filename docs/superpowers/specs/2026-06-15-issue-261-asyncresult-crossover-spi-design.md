# Issue #261 — asyncResult + crossoverToAsyncMs SPI shape, with import rejection

**Date:** 2026-06-15
**Milestone:** v0.8.0
**Issue:** #261 (closed-by this PR)
**Parent (deferred runtime):** #223
**Audit reference:** `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` §M6

> **Revision history**
>
> - **Revision 1 (2026-06-15):** initial spec, post-brainstorm pivot from
>   the issue text's "WARN-on-import" stance to **reject-at-import**. Per
>   project lead: Cloud→cyoda-go downgrade is not a real flow; silent
>   semantic drift is worse than a loud 400 at the import boundary. The
>   issue's reference to #223's "don't break Cloud-to-cyoda-go migration
>   of definitions whose authors set the field defensively" is
>   intentionally overridden as a management decision and recorded here
>   for traceability.

---

## 1. Problem

The OpenAPI `ExternalizedProcessorConfigDto` declares two fields,
`asyncResult` (boolean) and `crossoverToAsyncMs` (int64), at
`api/openapi.yaml:8798–8811`. The SPI `ProcessorConfig`
(`cyoda-go-spi/types.go:158`) does not carry either field. The
OpenAPI-generated DTO already carries the fields as `*bool` and
`*int64` (`api/generated.go:2131,2135,2152-2155`), so they decode
cleanly into the DTO — but they vanish when the handler maps the
DTO to `spi.WorkflowDefinition` because the SPI type lacks the
matching slots. The audit records two compounding consequences (§M6):

- `json.Unmarshal` in the import handler (`internal/domain/workflow/handler.go`,
  around line 78) silently drops the fields on import after they
  fail to bind to the SPI struct. The client receives 200 and the
  configuration is lost.
- The fields advertise a runtime semantic (suspend the cascade
  transaction; cross over to async-result delivery after a timer
  elapses) that cyoda-go's storage engines cannot implement. The
  full runtime is parent issue #223 and is gated on Cassandra-plugin
  work-recovery primitives — out of scope for cyoda-go.

This carve-out (#261) lands the SPI **shape** so the fields are
typed at the API/SPI boundary, and converts the silent drop into a
loud **400 VALIDATION_FAILED at import** when either field requests
semantics this backend cannot honour.

The acceptance criteria recorded on #261 require:

- SPI types tagged for cyoda-go-spi v0.8.0 (the milestone tag, not a
  per-PR tag — see §10).
- Import validation accepts both fields with type checks.
- ~~WARN emitted on `asyncResult=true` import.~~ **Superseded by reject —
  see §3.**
- Export round-trip preserves both fields.
- OpenAPI description flags the gap.
- Release notes mention the partial support.

This spec covers all six (with the WARN→reject pivot explicit in §3).

## 2. Goals

- Add `AsyncResult *bool` and `CrossoverToAsyncMs *int64` to the SPI
  `ProcessorConfig`. Both pointer with `omitempty` for byte-equivalent
  round-trip of the nil case. Precedent: `StartNewTxOnDispatch *bool`
  on the same struct.
- Regenerate `api/generated.go` against the updated SPI and the
  OpenAPI description amendments.
- Land two import-validator rules inside `validateWorkflowStructure`'s
  per-processor loop:
  - `AsyncResult != nil && *AsyncResult` → reject with
    `VALIDATION_FAILED 400`.
  - `CrossoverToAsyncMs != nil` (any value, including `&0`) → reject
    with `VALIDATION_FAILED 400`.
- Round-trip the accepted shape (nil-default; explicit
  `AsyncResult=&false`) byte-equivalent through import → store → export.
- Amend OpenAPI descriptions on both fields to record the reject-at-
  import behaviour and the parity gap.
- Update `docs/cyoda/cloud-divergences.md` to reflect "rejected at
  import" rather than the current "silently ignored".
- Update `cmd/cyoda/help/content/workflows.md` PROCESSORS section to
  document the fields' status under `ProcessorConfig fields` (one
  bullet each) plus a closing note on the import rejection rule.

## 3. Non-goals

- Implement the async-result / crossover runtime. (#223 owns the
  runtime — durable suspend state, work-stealing recovery,
  distributed timer coordination. Gated on Cassandra-plugin
  primitives, out of scope for cyoda-go.)
- Add an SPI capability-detection / advertisement mechanism for
  backend-side feature flags. (#223 scope.)
- Modify the closed-source Cassandra plugin. Additive SPI fields
  are non-breaking; courtesy refresh via the end-of-milestone tag,
  not from this PR (memory `feedback_courtesy_pr_scope`).
- Bundle the carve from #254 (RetryPolicy import validation) into
  this PR. Separate PR — its enum-exposure and error-wording
  decisions are distinct.
- Modify the vendored upstream OpenAPI copies under `docs/cyoda/`.
  They are point-in-time snapshots maintained upstream.
- Add a v0.8.0 SPI tag in isolation. Bundled into v0.8.0's
  end-of-milestone tag per `cyoda-go-spi/MAINTAINING.md`.

## 4. Decision summary (post-brainstorm)

| Question | Decision |
|---|---|
| SPI shape | `AsyncResult *bool` + `CrossoverToAsyncMs *int64` on `ProcessorConfig`, both `omitempty`. Pointer for nil-vs-explicit-zero distinction and byte-equivalent round-trip of the absent case. Precedent: `StartNewTxOnDispatch *bool`. |
| Import action on `asyncResult=true` | **Reject 400 VALIDATION_FAILED.** Project-lead override of the issue's "WARN" stance. Cloud→cyoda-go downgrade is not a flow we care about; loud failure at the import boundary beats silent semantic drift at runtime. |
| Import action on `asyncResult=false` (explicit *bool to false) | **Accept.** Operator is affirming the no-async default, not requesting an unsupported semantic. Round-trips byte-equivalent. |
| Import action on `asyncResult` absent | **Accept.** Identical wire shape to `&false` after omitempty marshalling. |
| Import action on `crossoverToAsyncMs != nil` (any value, paired or orphan) | **Reject 400 VALIDATION_FAILED.** The field is meaningful only as a tuner for the async-result semantic; if that semantic is unsupported, the tuner has no home. Includes the orphan (no `asyncResult=true`) and the paired cases. |
| Rule-fire ordering when both fields are set | The `asyncResult=true` check fires first. A processor with both `asyncResult=true` and `crossoverToAsyncMs=&5000` surfaces "asyncResult=true is not supported" — the more semantically anchored message. |
| Where in validator | New per-processor rule inside `validateWorkflowStructure`'s `for _, p := range tr.Processors` loop, immediately before the existing `ExecutionMode` enum check. Mirrors the pattern #259 introduced for Schedule rules. |
| Export round-trip | Automatic via SPI JSON tags. Only the accepted-by-validator shapes (nil and `&false`) can land in the store via import; both round-trip byte-equivalent under `*bool` + omitempty. |
| OpenAPI descriptions | Both fields' descriptions updated: "This backend does not implement async/crossover semantics; imports that set `asyncResult=true` or any `crossoverToAsyncMs` value are rejected with HTTP 400." Keep the existing "storage-engine-plugin dependent" framing as the upstream-Cloud context. |
| `docs/cyoda/cloud-divergences.md` | Rows 16–17 updated: status flips from "silently ignored" to "rejected at import (400 VALIDATION_FAILED)". Citation #223 retained as the parent. |
| Help-topic | `cmd/cyoda/help/content/workflows.md` PROCESSORS section: extend the `ProcessorConfig fields` bullet list with `asyncResult` and `crossoverToAsyncMs` lines (each mentioning the reject), and add a one-paragraph note that the runtime semantics are not implemented. |
| SPI CHANGELOG | New `[Unreleased]` `### Added` bullet for the two `ProcessorConfig` fields. |
| SPI tagging | None forced. Bundled into v0.8.0's end-of-milestone tag per `cyoda-go-spi/MAINTAINING.md`. cyoda-go pseudo-version-pins to SPI `main` HEAD across all four `go.mod` files per memory `project_v0_8_0_milestone_state`. |
| Cassandra plugin | **Untouched in this PR.** Additive fields are non-breaking; the plugin does not need to inspect them today. Courtesy refresh via end-of-milestone tag (memory `feedback_courtesy_pr_scope`). |
| Error wording | Self-contained, no issue ID (memory `feedback_no_issue_ids_in_code`). Each error names workflow / state / transition / processor and the specific rule violated. |
| Acceptance bullet 3 ("WARN emitted") | **Superseded by reject.** The pivot is recorded in §1 revision history and in this row; the issue text is not edited (project policy: issues are immutable conversational history). |
| #254 (RetryPolicy carve) | **Out of scope.** Separate PR. |

## 5. Detailed design

### 5.1 SPI changes (`cyoda-go-spi/types.go`)

Extend `ProcessorConfig` with two new fields, placed after
`StartNewTxOnDispatch` (the existing pointer field, which establishes
the precedent):

```go
type ProcessorConfig struct {
    AttachEntity         bool   `json:"attachEntity,omitempty"`
    CalculationNodesTags string `json:"calculationNodesTags,omitempty"`
    ResponseTimeoutMs    int64  `json:"responseTimeoutMs,omitempty"`
    RetryPolicy          string `json:"retryPolicy,omitempty"`
    Context              string `json:"context,omitempty"`
    StartNewTxOnDispatch *bool  `json:"startNewTxOnDispatch,omitempty"`

    // AsyncResult, when true, requests that the cascade engine suspend
    // the transaction at processor dispatch and resume only when the
    // processor's result eventually arrives via the async-result
    // delivery slot. The runtime that implements this — durable
    // suspend state, work-stealing recovery, distributed timer
    // coordination — is gated on storage-engine primitives not
    // available in every backend. Consuming engines that do not
    // implement async-result semantics MUST reject this field at
    // import (or the equivalent configuration-boundary) rather than
    // silently degrade to synchronous dispatch.
    //
    // Pointer so that nil (absent) and &false (explicit no-async) are
    // distinguishable on the wire and round-trip byte-equivalent.
    AsyncResult *bool `json:"asyncResult,omitempty"`

    // CrossoverToAsyncMs is the timer, in milliseconds, after which
    // the engine crosses over from sync-wait to async-result delivery
    // for an AsyncResult=true processor. Effective only when
    // AsyncResult is true. Consuming engines that do not implement
    // async-result semantics MUST reject any non-nil value at import.
    CrossoverToAsyncMs *int64 `json:"crossoverToAsyncMs,omitempty"`
}
```

Round-trip behaviour under `encoding/json` + `omitempty`:
- nil pointer → field omitted on marshal.
- `&false` / `&0` → field present with the zero value on marshal.
- Non-nil non-zero → field present with the value on marshal.

`types_test.go` extension: add subtests mirroring the existing
`StartNewTxOnDispatch` round-trip at line 36, covering:
- `AsyncResult: nil, CrossoverToAsyncMs: nil` → JSON omits both.
- `AsyncResult: &false` → JSON contains `"asyncResult": false`.
- `AsyncResult: &true` → JSON contains `"asyncResult": true`.
- `CrossoverToAsyncMs: &0` → JSON contains `"crossoverToAsyncMs": 0`.
- `CrossoverToAsyncMs: &5000` → JSON contains `"crossoverToAsyncMs": 5000`.
- Bidirectional: unmarshal each shape back and assert pointer
  equality semantics (`*bool` equal where present; nil where absent).

### 5.2 SPI CHANGELOG (`cyoda-go-spi/CHANGELOG.md`)

Append to the existing `[Unreleased]` `### Added` section (after the
`TransitionSchedule` entry):

```markdown
- `ProcessorConfig.AsyncResult *bool` and
  `ProcessorConfig.CrossoverToAsyncMs *int64` for the async-result /
  crossover-timer configuration shape carve-out (cyoda-go #261). The
  fields are pointer-typed (omitempty) so the absent case round-trips
  byte-equivalent. Runtime not yet wired — see cyoda-go #223 for full
  feature tracking. Consuming engines that do not implement
  async-result semantics MUST reject non-default values at the
  configuration-import boundary rather than silently degrade.
```

### 5.3 OpenAPI changes (`api/openapi.yaml`)

Amend the existing `asyncResult` and `crossoverToAsyncMs` descriptions
under `ExternalizedProcessorConfigDto.properties` (current location
lines 8798–8811). The schema types stay `boolean` and `integer/int64`
respectively (no shape change — the SPI is what changed, the wire
contract was already correct).

The validator errors flow through the existing error path that maps
to `VALIDATION_FAILED`; the parity-runner assertion at
`e2e/parity/externalapi/workflow_import_export.go:491,515` confirms
the precedent for similar import-time rules. No new error-
classification code is required.

```yaml
        asyncResult:
          type: boolean
          description: |
            Whether to await the result asynchronously, outside of the
            transaction. Behavior is storage-engine-plugin dependent —
            not every plugin implements crossover semantics; consult
            the runtime plugin's documentation for the supported
            behavior. This backend does not implement async/crossover
            semantics; imports that set asyncResult=true are rejected
            with HTTP 400 VALIDATION_FAILED. The field is round-tripped
            for nil (absent) and the explicit-false case.
        crossoverToAsyncMs:
          type: integer
          format: int64
          description: |
            Crossover delay to switch to asynchronous processing (ms),
            effective only when asyncResult is true. Behavior is
            storage-engine-plugin dependent — see asyncResult. This
            backend does not implement async/crossover semantics;
            imports that set any value for crossoverToAsyncMs are
            rejected with HTTP 400 VALIDATION_FAILED.
```

Regenerate `api/generated.go` against the SPI bump. Expected change:
descriptions on the two fields updated; no structural change to the
generated DTO (the fields were already there).

### 5.4 Validator rules (`internal/domain/workflow/validate.go`)

Add two checks inside the existing per-processor loop in
`validateWorkflowStructure`. Place them immediately before the
existing `ExecutionMode` enum check (currently around line 196).

```go
for _, p := range tr.Processors {
    // L-1 — Processor Name non-empty.  (existing)
    if p.Name == "" { ... }
    // L-2 — Processor Name length cap. (existing)
    if len(p.Name) > maxIdentifierLen { ... }

    // M6 (audit) — asyncResult=true requests a runtime semantic this
    // backend does not implement; reject rather than silently degrade
    // to sync dispatch.
    if p.Config.AsyncResult != nil && *p.Config.AsyncResult {
        return fmt.Errorf(
            "workflow %q state %q transition %q processor %q: asyncResult=true is not supported on this backend (async/crossover semantics are not implemented)",
            wf.Name, stateName, tr.Name, p.Name)
    }

    // M6 (audit) — crossoverToAsyncMs is a tuner for the asyncResult
    // semantic. With that semantic unsupported, any non-nil value has
    // no honourable home. Includes the orphan (no asyncResult=true)
    // and the (otherwise-rejected) paired cases — defence in depth.
    if p.Config.CrossoverToAsyncMs != nil {
        return fmt.Errorf(
            "workflow %q state %q transition %q processor %q: crossoverToAsyncMs is not supported on this backend (async/crossover semantics are not implemented)",
            wf.Name, stateName, tr.Name, p.Name)
    }

    // H4 — ExecutionMode enum. (existing)
    if _, ok := validExecutionModes[p.ExecutionMode]; !ok { ... }
}
```

Scope: runs in `validateImportRequest` only (per-incoming-workflow
scope). Legacy stored workflows are not retroactively rejected — same
scoping decision as the v0.8.0 structural rules in #298. The engine
itself never inspects these fields at runtime (verified by
`grep -rn "AsyncResult\|CrossoverToAsyncMs" internal/ plugins/` →
zero matches outside the unrelated `search.AsyncResultsPage` type),
so a legacy stored workflow that somehow contains a non-nil
`AsyncResult`/`CrossoverToAsyncMs` is functionally equivalent to one
without — silent inert state at runtime, loud reject at import for
new data. Export round-trips such legacy state byte-equivalent; §6.3
adds a test that exercises the store-direct write path to back the
assertion.

### 5.5 Export round-trip

No code change in the export handler. The SPI struct's JSON tags
carry the new fields automatically through the existing
`json.Marshal` path. Two observable post-conditions:

- A workflow imported without either field exports without either
  field (both nil → omitted).
- A workflow imported with `asyncResult=false` (explicit) exports
  with `"asyncResult": false` (pointer to false survives omitempty).

`asyncResult=true` and any `crossoverToAsyncMs` cannot land in the
store via import (validator rejects), so the export is observationally
moot for those shapes.

### 5.6 `docs/cyoda/cloud-divergences.md`

Update the existing rows 16 and 17. Current text records "OSS storage
engine plugins silently ignore" the fields and "OSS no-op" as the
status. Replace with "OSS rejects at import". The replacement also
fixes an incidental Gate-6 issue — the current row 16 names "closed-
source Cassandra plugin", which violates memory
`feedback_cassandra_repo_private` ("commercial backend" wording, never
name/link the Cassandra repo). The new text uses "commercial backend":

```markdown
| `ProcessorDefinitionDto.asyncResult` | Field declared in OpenAPI; OSS backend rejects asyncResult=true at workflow import (400 VALIDATION_FAILED). Crossover semantics need durable suspend state + cluster-wide work-stealing recovery + a distributed timer — implementable only in the commercial backend. | Reject-at-import on OSS; enterprise-tier in the commercial backend (not yet implemented there either). | [#223](https://github.com/Cyoda-platform/cyoda-go/issues/223) |
| `ProcessorDefinitionDto.crossoverToAsyncMs` | Field declared in OpenAPI; OSS backend rejects any non-nil crossoverToAsyncMs at workflow import (400 VALIDATION_FAILED). See `asyncResult` — same parity gap. | Reject-at-import on OSS; enterprise-tier in the commercial backend. | [#223](https://github.com/Cyoda-platform/cyoda-go/issues/223) |
```

(Per memory `feedback_cassandra_repo_private`: refer to the commercial
Cassandra plugin as "the commercial backend"; do not link the repo.)

### 5.7 Help-topic (`cmd/cyoda/help/content/workflows.md`)

Under `## PROCESSORS` → `**ProcessorConfig fields:**` bullet list,
append two new bullets after the existing `context` bullet:

```markdown
- `asyncResult` — boolean (pointer; nil-default) — declared in the
  OpenAPI for Cloud parity; the runtime does **not** implement
  async-result semantics on this backend. Imports that set
  `asyncResult=true` are rejected with `400 VALIDATION_FAILED`. The
  explicit `asyncResult=false` and absent cases are accepted and
  round-tripped.
- `crossoverToAsyncMs` — int64 (pointer; nil-default) — crossover
  delay (ms) for the async-result semantic; declared in the OpenAPI
  for Cloud parity; the runtime does **not** implement it. Imports
  that set any non-nil value are rejected with `400 VALIDATION_FAILED`.
```

No change to the section heading structure.

### 5.8 Release notes

One bullet for v0.8.0 release notes (added to whatever staging
artifact the release process uses; the spec records the wording):

> Workflow import now rejects `asyncResult=true` and any non-nil
> `crossoverToAsyncMs` on processor configurations with `400
> VALIDATION_FAILED`. The fields are declared in the OpenAPI for Cloud
> parity; this backend does not implement async-result / crossover
> semantics. See `docs/cyoda/cloud-divergences.md`. (Closes #261.)

## 6. Test plan

### 6.1 SPI repo (`cyoda-go-spi`)

Extend `types_test.go` with one new test function, named
`TestProcessorConfig_AsyncResultAndCrossover_RoundTrips` to mirror
the existing `TestProcessorConfig_StartNewTxOnDispatch_RoundTrips` at
line 34. Cover:

| Subtest | Input | Marshal expectation | Unmarshal expectation |
|---|---|---|---|
| `AsyncNil_CrossNil` | `ProcessorConfig{}` | both fields omitted | both nil |
| `AsyncTrue_CrossNil` | `AsyncResult: &true` | `"asyncResult": true` present, no crossover | `*AsyncResult == true`, crossover nil |
| `AsyncFalse_CrossNil` | `AsyncResult: &false` | `"asyncResult": false` present | `*AsyncResult == false`, crossover nil |
| `AsyncNil_CrossZero` | `CrossoverToAsyncMs: &0` | `"crossoverToAsyncMs": 0` present | `*CrossoverToAsyncMs == 0` |
| `AsyncNil_CrossN` | `CrossoverToAsyncMs: &5000` | `"crossoverToAsyncMs": 5000` present | `*CrossoverToAsyncMs == 5000` |
| `AsyncTrue_CrossN` | both non-nil | both present | both non-nil with values |

### 6.2 cyoda-go validator (`validate_import_test.go`)

Extend the existing test file. New test functions follow the existing
naming convention (`TestValidator_*`, e.g.
`TestValidator_ManualAndSchedule_Rejected` at line 409). No
audit-issue suffix in test names.

| Test function | Input shape | Expected |
|---|---|---|
| `TestValidator_AsyncResultTrue_Rejected` | one processor, `AsyncResult: &true` | error names workflow/state/transition/processor; message matches the asyncResult rule |
| `TestValidator_AsyncResultFalse_Accepted` | one processor, `AsyncResult: &false` | nil error |
| `TestValidator_AsyncResultAbsent_Accepted` | one processor, `AsyncResult: nil` | nil error |
| `TestValidator_CrossoverOrphan_Rejected` | one processor, `CrossoverToAsyncMs: &5000`, AsyncResult absent | error names processor; message matches the crossover rule |
| `TestValidator_CrossoverZero_Rejected` | one processor, `CrossoverToAsyncMs: &0` | error names processor; message matches the crossover rule |
| `TestValidator_AsyncTrueAndCrossover_AsyncRuleFiresFirst` | `AsyncResult: &true`, `CrossoverToAsyncMs: &5000` | error message is the asyncResult one (rule-fire ordering documented in §4) |
| `TestValidator_AsyncFalseAndCrossover_CrossoverRuleFires` | `AsyncResult: &false`, `CrossoverToAsyncMs: &5000` | error message is the crossover one |
| `TestValidator_MultiProcessor_OnlyOneBad_Rejected` | transition with three processors, the second has `AsyncResult: &true` | error names the **second** processor specifically; verifies the loop visits every element and does not break early or skip |

### 6.3 cyoda-go round-trip (`async_result_roundtrip_test.go`)

New file in `internal/domain/workflow/`, mirroring
`schedule_roundtrip_test.go` from #259. Cover:

- Import workflow with no async fields → export omits both (byte-
  equivalent to import).
- Import workflow with `asyncResult=&false` only → export contains
  `"asyncResult": false`, no `crossoverToAsyncMs`. JSON normalized
  comparison (`json_equals_normalized`).
- Import workflow with `asyncResult=&true` → expected reject at
  import (mirrors the validator test but at the handler boundary).
- Import workflow with `crossoverToAsyncMs=&5000` → expected reject
  at import.
- **Legacy-data round-trip** — bypass the validator by calling the
  workflow store's `Save` directly with a `WorkflowDefinition` whose
  processor carries `AsyncResult: &true` and `CrossoverToAsyncMs:
  &5000`. Then call the export handler and assert the response body
  contains both fields with the original values. Backs §5.4's
  "legacy stored data round-trips byte-equivalent" assertion.

### 6.4 cyoda-go E2E (`internal/e2e/`)

New file `async_result_import_reject_test.go`, modelled on
`scheduled_transition_test.go` from #259. Two tests:

- `TestAsyncResultTrueRejectedAtImport` — POST import body with
  `asyncResult=true`, assert response is `400 VALIDATION_FAILED`,
  body contains the processor location identifiers.
- `TestCrossoverToAsyncMsRejectedAtImport` — POST import body with
  `crossoverToAsyncMs=5000` (no asyncResult), assert `400
  VALIDATION_FAILED`, body identifies the processor.

### 6.5 Parity scenarios (`e2e/parity/externalapi/`)

Verified next-free slot is `wf-import/10` (slots 07–09 are populated
by #259's scheduled-transition + allowCycles scenarios).

Two new scenarios in `e2e/externalapi/scenarios/08-workflow-import-export.yaml`:

- `wf-import/10-asyncresult-true-rejects` — POST a minimal workflow
  with one transition carrying one processor with `asyncResult: true`.
  Expected: `400` with `VALIDATION_FAILED` code and a processor-named
  error.
- `wf-import/11-crossover-without-async-rejects` — Same shape but
  the processor sets `crossoverToAsyncMs: 5000` and `asyncResult`
  absent. Expected: `400` with `VALIDATION_FAILED`.

Corresponding Go runners added to
`e2e/parity/externalapi/workflow_import_export.go`, mirroring the
existing 07–09 runner pattern. Both runners must pass on all three
backends (memory, sqlite, postgres) via `make test-all`.

The Cassandra plugin sibling parity job (`e2e/parity/registry.go`)
will pick the new scenarios up on its next dependency update — same
positive externality as #259.

## 7. SPI / cyoda-go bundling sequence

Per memory `feedback_spi_coordinated_release_procedure` and
`project_v0_8_0_milestone_state`:

1. **In `cyoda-go-spi` `main`:** commit the two new `ProcessorConfig`
   fields with their docstrings + `CHANGELOG.md [Unreleased]` entry +
   `types_test.go` extension. Push to `main`. **No tag cut** — all
   v0.8.0 SPI work bundles into the milestone-end tag (CHANGELOG
   v0.8.0 retraction note explains the cadence).
2. **In `cyoda-go` (this worktree):** bump `cyoda-go-spi` pseudo-
   version pin in all four `go.mod` files (root + `plugins/memory` +
   `plugins/sqlite` + `plugins/postgres`) to the new SPI `main` HEAD
   SHA. Run `go mod tidy` per module.
3. **Regenerate `api/generated.go`** against the OpenAPI description
   amendments. Expected diff: description text only; no structural
   change to the generated DTO (fields were already present).
4. **Implement validator rules + tests** as §5.4 and §6.
5. **Update docs:** OpenAPI descriptions (§5.3),
   `cloud-divergences.md` (§5.6), `workflows.md` help topic (§5.7).
6. **Verification:** `go vet ./...`, `go test -short ./...`,
   `go test ./internal/e2e/...`, `make test-all`. Then **once,
   end-of-deliverable** (not at every iteration), `go test -race
   ./...` as the one-shot pre-PR sanity check per
   `.claude/rules/race-testing.md`. Subagents dispatched on step 6
   work must not run `-race` per iteration.

The per-module-hygiene CI job is expected to FAIL until the SPI tag
is cut at end-of-milestone — same status as the v0.8.0 milestone
state memory documents.

## 8. Risks

- **API regression** — today's import body with `asyncResult=true`
  returns 200 and silently ignores the field; tomorrow's returns
  400. For Cloud→cyoda-go migration this is a breaking change,
  deliberately taken. Release-notes bullet (§5.8) calls it out.
- **#145 (DisallowUnknownFields) interaction** — independently
  planned. When #145 lands, the rejection becomes belt-and-braces:
  typed reject at the validator AND raw reject at the decoder. No
  conflict — the validator rule has a more informative message
  (processor location) than the decoder would, so it should run
  first; the decoder catches anything that bypasses the validator
  path. No code change required from this spec to coexist with #145.
- **#223 forward compatibility** — when/if a backend gains the
  async-result runtime, the validator rule has to flip from
  unconditional reject to capability-gated accept. The SPI shape
  this spec lands is the same shape #223 will need (no rework). The
  capability-flag mechanism is explicitly #223's scope.
- **Cassandra plugin** — additive SPI fields are non-breaking. The
  plugin does not inspect these fields at runtime today (no
  consumer). Pin bump propagates via the end-of-milestone tag, not
  from this PR. Memory `feedback_courtesy_pr_scope`.
- **Issue text override** — §1 records the WARN→reject pivot as a
  project-lead management decision. Reviewers and future readers
  should see the spec's §3 + §4 + this risk bullet rather than the
  issue text for the authoritative behaviour.

## 9. Acceptance mapping

| Issue acceptance | Disposition |
|---|---|
| SPI types tagged for cyoda-go-spi v0.8.0 | Met via §7 — fields on `main`, bundled into milestone-end tag, not per-PR |
| Import validation accepts both fields with type checks | Met via §5.4 — accepts nil, accepts `&false` for AsyncResult, rejects with location-naming error otherwise |
| WARN emitted on `asyncResult=true` import | **Superseded by reject** — §3, §4. Project-lead decision recorded in §1 revision history |
| Export round-trip preserves both fields | Met via §5.5 — automatic via SPI JSON tags |
| OpenAPI description flags the gap | Met via §5.3 — descriptions amended to record reject behaviour |
| Release notes mention the partial support | Met via §5.8 — bullet wording included |

## 10. Open questions

None — all design decisions resolved in §4 with project-lead input
during the brainstorm session.
