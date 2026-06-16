# Design — #262: `RetryPolicy` import-time validation + surface inbound `retryable` flag

**Date:** 2026-06-15
**Issue:** [#262](https://github.com/Cyoda-platform/cyoda-go/issues/262)
**Milestone:** v0.8.0
**Parent (deferred):** #254 (full member-failover retry loop)
**Audit reference:** `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` §M1
**Worktree branch:** `feat/262-retrypolicy-validation` (based on `release/v0.8.0`)

## Context

The wire-level retry contract between cyoda-go and calculation members is two
fields: the workflow-author selects a `retryPolicy` on each processor, and the
member can veto further retries by setting `retryable=false` on its error
response. The full retry loop (member-failover, exclusion-set `FindByTags`,
server-side config keys, async-mode suppression, aggregated-failure error
shape) is deferred to #254 for a later release.

This issue lands the **import-side guard and the wire-level flag plumbing**
only — so:

1. Operators get a clear import-time error today for typo'd policies (instead
   of silent acceptance of whatever string they wrote).
2. The inbound `retryable` data is captured into `ProcessingResponse` so #254
   can light up the retry consumer without re-touching the streaming layer.

The dispatcher remains single-shot. No retry, no behaviour change.

## Out of scope (stays in #254)

- Retry loop in `internal/grpc/dispatch.go`.
- Exclusion-set variant of `FindByTags`.
- Server-side config keys (`CYODA_RETRY_FIXED_NUM_RETRIES`,
  `CYODA_RETRY_FIXED_DELAY_MS`).
- Async-suppression coupling with #223.
- Aggregated-failure error shape.

The `ProcessingResponse.Retryable` field this issue adds will have **zero
consumers** when the PR merges. That is intentional — the test asserts
plumbing, not policy.

## Current shape (verified 2026-06-15)

### RetryPolicy

- **SPI:** `cyoda-go-spi/types.go:162` —
  `RetryPolicy string `json:"retryPolicy,omitempty"`` on
  `ProcessorConfig`. Bare string, no enum constants exported. (Same shape
  as `ExecutionMode` on the SPI side — the SPI is permissive; the import
  layer is strict.)
- **OpenAPI:** `api/openapi.yaml:8120-8122` (on `ExternalizedFunctionConfigDto`)
  and `8785-8787` (on `ExternalizedProcessorConfigDto`) — declared as a
  bare `type: string` with no `enum:`, so the generated DTO at
  `api/generated.go:2114-2115, 2167-2168` is also a bare string. No client-side hint that only specific
  values are valid.
- **In-tree consumers:** none. The dispatcher does not read it.
- **Audit doc §M1 verdict:** allowed values per Cloud semantics are `NONE`
  (single-shot), `FIXED` (default when unset; up to N additional attempts
  with fixed delay, where N and delay are server-configured).

### ProcessingResponse + Retryable plumbing

- **Struct:** `internal/grpc/members.go:24-30` —
  ```go
  type ProcessingResponse struct {
      Payload  json.RawMessage
      Success  bool
      Error    string
      Matches  *bool    // for criteria responses (nil for processor responses)
      Warnings []string // warnings from processor/criteria, propagated to client
  }
  ```
- **Construction sites:**
  - `internal/grpc/streaming.go:267-272` — processor response handler.
  - `internal/grpc/streaming.go:302-307` — criteria response handler.
  - `internal/grpc/members.go:89-92` — fail-all-pending sweep (no inbound data).
- **Wire field already there:** every CloudEvent error variant declares
  `Retryable *bool` (e.g. `api/grpc/events/types.go:36, 111, 189, 261, 2536,
  …`). The cyoda-go handlers currently unmarshal a slim
  `Error *struct { Message string }` and drop everything else.

### Existing validator patterns to mirror

- `internal/domain/workflow/validate.go:40-49` — `validExecutionModes` map.
- `internal/domain/workflow/validate.go:207-210` — per-processor enum check
  inside `validateWorkflowStructure`, with the error format:
  ```
  workflow %q state %q transition %q processor %q: unknown executionMode %q (allowed: SYNC, ASYNC_SAME_TX, ASYNC_NEW_TX, COMMIT_BEFORE_DISPATCH, or empty)
  ```
- `internal/domain/workflow/validate_import_test.go:186-432` — companion tests:
  `TestValidateImportRequest_RejectsUnknownExecutionMode`,
  `TestValidateImportRequest_AcceptsEmptyExecutionMode`,
  `TestValidateImportRequest_AcceptsAllKnownExecutionModes`.

This issue clones all three patterns one-for-one.

## Design

### Slice 1 — RetryPolicy import validation

**Location of constants and map** — `internal/domain/workflow/validate.go`,
adjacent to the existing `ExecutionMode*` constants and `validExecutionModes`
map. Identical convention. The SPI stays permissive (bare string); only the
import layer is strict, matching the ExecutionMode precedent.

```go
const (
    RetryPolicyNone  = "NONE"
    RetryPolicyFixed = "FIXED"
)

var validRetryPolicies = map[string]struct{}{
    "":               {}, // empty → server defaults to FIXED
    RetryPolicyNone:  {},
    RetryPolicyFixed: {},
}
```

**Validation site** — inside the processor loop in
`validateWorkflowStructure`, immediately after the ExecutionMode check.
One additional map lookup, identical error format:

```go
if _, ok := validRetryPolicies[p.Config.RetryPolicy]; !ok {
    return fmt.Errorf("workflow %q state %q transition %q processor %q: unknown retryPolicy %q (allowed: NONE, FIXED, or empty)",
        wf.Name, stateName, tr.Name, p.Name, p.Config.RetryPolicy)
}
```

(Field path `p.Config.RetryPolicy` per `cyoda-go-spi/types.go:154,162`.)

### Slice 2 — `ProcessingResponse.Retryable` field + plumbing

**Struct change** — add `Retryable *bool` to `ProcessingResponse` in
`internal/grpc/members.go`, after `Warnings`. Pointer because we need to
distinguish "wire said retryable=false" from "wire didn't say". Consistent
with the existing `Matches *bool` field. No JSON tag — the struct is an
internal handoff type, never marshalled outbound.

```go
type ProcessingResponse struct {
    Payload   json.RawMessage
    Success   bool
    Error     string
    Matches   *bool    // for criteria responses (nil for processor responses)
    Warnings  []string // warnings from processor/criteria, propagated to client
    Retryable *bool    // member-supplied retryable flag (nil when wire didn't set it); no consumer in this issue (see #254)
}
```

**Plumbing — both handlers** — widen the anonymous `Error` struct in
`handleProcessorResponse` (`internal/grpc/streaming.go:244-252`) and
`handleCriteriaResponse` (`internal/grpc/streaming.go:278-286`) to capture
`Retryable *bool` alongside `Message string`. Then pass the pointer through
when constructing `ProcessingResponse`. When `resp.Error == nil`, the field
stays nil — matching wire semantics (retryable is an error-level field).

```go
Error *struct {
    Message   string `json:"message"`
    Retryable *bool  `json:"retryable,omitempty"`
} `json:"error"`
```

```go
var retryable *bool
if resp.Error != nil {
    retryable = resp.Error.Retryable
}
member.CompleteRequest(resp.RequestID, &ProcessingResponse{
    Payload:   resp.Payload,
    Success:   resp.Success,
    Error:     errMsg,
    Warnings:  resp.Warnings,
    Retryable: retryable,
})
```

Identical change in the criteria handler. Both updated for symmetry —
the future #254 retry loop covers processor and criteria dispatch paths
(`internal/grpc/dispatch.go:46-135` and `:181-260` per audit doc §M1).

### Slice 3 — OpenAPI schema tightening (non-breaking)

`api/openapi.yaml:8120-8122` and `:8785-8787` currently:

```yaml
retryPolicy:
  type: string
  description: Retry policy for the function
```

Becomes:

```yaml
retryPolicy:
  type: string
  enum: [NONE, FIXED]
  description: |
    Retry policy selector. NONE → single attempt, no retry. FIXED → up to N
    additional attempts with fixed delay between tries (N and delay are
    server-configured). When omitted, defaults to FIXED at engine fire.
```

The schema appears on both `ExternalizedFunctionConfigDto` and
`ExternalizedProcessorConfigDto` — both occurrences updated. This is a spec
tightening, not a breaking change: any previously-imported workflow with a
value outside the enum was already going to be rejected by the new
validator, so accepted-yesterday inputs remain accepted-tomorrow.

`cyoda help openapi {json,yaml}` re-emits the YAML as a machine-consumable
artefact (per memory: help topic actions are the publication channel,
superseding release-asset publishing), so this tightening flows to clients
that consume the spec via help.

### Slice 4 — Help topic update

`cmd/cyoda/help/content/workflows.md`:

1. **Line 174** — replace the current `retryPolicy` bullet, which incorrectly
   says *"empty means no retry"*, with an accurate description in the style
   of the surrounding `executionMode` / `asyncResult` bullets:

   > `retryPolicy` — string — selects the server-resolved retry strategy.
   > Valid values: `NONE` (single attempt, no retry), `FIXED` (default when
   > omitted; up to N additional attempts with fixed delay between tries,
   > where N and delay are server-configured). Import-time validation
   > rejects any other value. The empty string is accepted and defaults to
   > `FIXED` at engine fire. **cyoda-go status:** captured but not consumed
   > — the dispatcher is single-shot regardless of policy; the retry loop
   > ships in a later release. Cloud honours both policies.

2. **Around line 318** — add to the import-rejection bullet list:

   > - Unknown `retryPolicy` value on any processor (allowed: `NONE`,
   >   `FIXED`, or empty).

3. Example JSON at line 70 (`"retryPolicy": ""`) is left untouched — empty
   is valid.

### Slice 5 — Audit doc honesty pass

`docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` §M1 currently lists `RetryPolicy` as
dead-by-design and `ProcessingResponse.Retryable` as not-surfaced. After
this PR, both halves change status:

- `RetryPolicy` import validation: present, enum-restricted.
- `ProcessingResponse.Retryable`: surfaced (not consumed).

Flip the status lines in §M1 to reflect reality, keeping the remaining
gap-list (retry loop, exclusion-set lookup, config keys, async suppression,
aggregated-failure shape) pointing at #254. The audit doc is a living
record of cloud-parity gaps; it must remain accurate.

## Tests (RED-first)

### Validator tests (`internal/domain/workflow/validate_import_test.go`)

Cloned one-for-one from the ExecutionMode tests at lines 186-432:

| Test name | Asserts |
|---|---|
| `TestValidateImportRequest_RejectsUnknownRetryPolicy` | `retryPolicy: "FOO"` → error containing the substring `unknown retryPolicy "FOO"` |
| `TestValidateImportRequest_AcceptsEmptyRetryPolicy` | `retryPolicy: ""` → nil error |
| `TestValidateImportRequest_AcceptsAllKnownRetryPolicies` | subtests for `NONE`, `FIXED` → nil error each |

Error-message substring assertion follows the ExecutionMode test's
`strings.Contains` style.

### Streaming-plumbing tests (`internal/grpc/streaming_test.go`)

Co-located with the existing `TestStreaming_ProcessorResponse` (:377) and
`TestStreaming_CriteriaResponse` (:435), reusing their `mockBidiStream`
harness and `TestStreaming_*` naming convention. Each new test feeds a
crafted CloudEvent through the existing streaming path and asserts on the
`ProcessingResponse` captured by `member.CompleteRequest`.

| Test name | Asserts |
|---|---|
| `TestStreaming_ProcessorResponse_PropagatesRetryableFalse` | payload `{"error":{"message":"boom","retryable":false}}` → captured `ProcessingResponse.Retryable != nil && *r == false` |
| `TestStreaming_ProcessorResponse_PropagatesRetryableTrue` | payload with `retryable:true` → captured pointer to true |
| `TestStreaming_ProcessorResponse_RetryableNilWhenAbsent` | payload with error but no `retryable` key → `Retryable == nil` |
| `TestStreaming_ProcessorResponse_RetryableNilOnSuccess` | success payload, no `error` block → `Retryable == nil` |
| `TestStreaming_CriteriaResponse_PropagatesRetryableFlag` | criteria handler symmetry: at least one positive + one nil-absent case |

The acceptance criterion calls out the false-case test explicitly; the rest
ensure the wire-tri-state (true / false / absent) round-trips correctly into
the nilable pointer.

## File footprint

```
internal/domain/workflow/validate.go              (+ ~12 lines: constants, map, check)
internal/domain/workflow/validate_import_test.go  (+ ~80 lines: 3 tests cloned from ExecutionMode pattern)
internal/grpc/members.go                          (+ 1 line: Retryable field)
internal/grpc/streaming.go                        (+ ~12 lines: 2 handlers widened)
internal/grpc/streaming_test.go                   (+ ~80 lines: 5 tests)
api/openapi.yaml                                  (+ 3 lines × 2 occurrences: enum + description)
cmd/cyoda/help/content/workflows.md               (rewrite retryPolicy bullet + 1-line addition)
docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md              (flip §M1 status lines for RetryPolicy + Retryable)
```

No new files. No SPI changes (SPI already has the field as bare string;
strictness lives in the cyoda-go import layer per ExecutionMode precedent).
No `go.mod` changes. No env-var changes (none of the server-side retry
config keys land in this issue — they're in #254).

## Acceptance criteria mapping

From the issue body:

- [x] **Validator rejects unknown `RetryPolicy` values; unit tests cover
      `NONE`, `FIXED`, `""`, and an unknown value.** → Slice 1 +
      validator tests above.
- [x] **`ProcessingResponse.Retryable` populated from the inbound CloudEvent;
      test asserts a member-returned `retryable=false` reaches the field.** →
      Slice 2 + `TestStreaming_ProcessorResponse_PropagatesRetryableFalse`.
- [x] **No behaviour change to the dispatch path itself (single-shot
      retained).** → Zero edits to `internal/grpc/dispatch.go`; verified by
      the existing dispatch tests staying green without modification.

## Risk and reversibility

- The validator addition is an import-time reject. Any workflow with a
  previously-silent typo'd `retryPolicy` value will start failing import
  with a 400. This is the intended behaviour — operators have been getting
  silent acceptance of values that would behave like FIXED-default at
  engine fire. The migration path is "fix your config to one of `NONE`,
  `FIXED`, or empty".
- Reversibility: each slice is independently revertable. The struct field
  addition is additive (no removals). The OpenAPI enum tightening is the
  one place a hostile rollback would matter, but it's covered by the
  validator anyway, so undoing one without the other does not corrupt
  state.

## Workflow next steps

After this spec is approved:

1. `superpowers:writing-plans` — produce an implementation plan with
   RED→GREEN→REFACTOR cycles per slice.
2. Implementation via `superpowers:subagent-driven-development` or in
   session.
3. `superpowers:verification-before-completion` —
   `go test ./... -v`, `go vet ./...`, `make test-all`,
   `go test -race ./...` as the one-shot end-of-deliverable sanity check.
4. `superpowers:requesting-code-review` and
   `antigravity-bundle-security-developer:cc-skill-security-review`.
5. PR against `release/v0.8.0` with `Closes #262` in the body
   (release-branch issue closure rule).
