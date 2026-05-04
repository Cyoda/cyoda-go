# OpenAPI server-spec conformance — design

**Issue:** [#21](https://github.com/Cyoda-platform/cyoda-go/issues/21) (originally migrated from `cyoda-light-go#202`; was superseded by #192, then #192 was closed won't-do and #21 reopened with the runtime-validation scope captured here)
**Branch:** `issue-21-openapi-conformance` → `main`
**ADR:** [0001 OpenAPI server-spec conformance approach](../../adr/0001-openapi-server-spec-conformance.md) (Accepted 2026-04-29 — runtime validation via `kin-openapi` at the E2E test boundary; defer compile-time strict typing)
**Related:** [#193](https://github.com/Cyoda-platform/cyoda-go/issues/193) — feature work for arbitrary EdgeMessage payload content types (out of scope for this PR)
**Status:** Design approved 2026-04-29

## 1. Scope, framing, non-goals

### Scope (single PR landing on `main`)

- **Audit table** at `docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md` — every operationId paired with handler function, current spec response shape, actual server response shape, disposition.
- **Spec fixes** in `api/openapi.yaml`:
  - Named schemas for every response.
  - All 6 sites with `type: array` + sibling `$ref` corrected to well-formed `type: array, items: { $ref: ... }`.
  - Loose `type: object` blocks replaced with `$ref` to a named schema or `additionalProperties: true` + `description: polymorphic by intent`.
  - `basicAuth` declared in `components.securitySchemes` (referenced at `api/openapi.yaml:4587` but never declared — uncovered during the ogen spike that informed ADR 0001).
  - Per-operation error blocks declared, including the shared 401/403/500 from middleware via `components.responses` `$ref`s.
  - `messaging.GetMessage.content` declared as polymorphic JSON (`EdgeMessagePayload`), not `type: string`.
- **Handler defect fixes**:
  - `internal/domain/messaging/handler.go:183` — `string(payloadBytes)` → `json.RawMessage(payloadBytes)`.
  - Other shape defects discovered during audit — fixed inline per Gate 6 unless they require design input.
- **Runtime validator** at the E2E test layer — collect-and-report mode, single end-of-suite failure with full mismatch list.
- **E2E coverage closure** — minimal happy-path test for every operationId currently uncovered.
- **Derived-artefact updates** — `e2e/parity/client/types.go`, parity scenarios, `cmd/cyoda/help/content/openapi.md` narrative.

### Non-goals

- **Compile-time strict typing of response shapes** (deferred per ADR 0001).
- **External reconciliation with `docs/cyoda/openapi.yml`** (separate future milestone). The audit table lives past this PR and becomes the starting point for that work.
- **Cassandra plugin SPI changes** (out of scope; surfaces via parity registry's next dep update).
- **5xx envelope standardization beyond what CLAUDE.md already mandates** (ticket UUID + generic message). The validator enforces the declared shape; we don't redesign.
- **Arbitrary EdgeMessage payload content types** (filed as #193). This PR documents the current JSON-only limitation in the spec (`contentType` description: "informational; does not affect storage or retrieval format — payload is always treated as a JSON value; clients needing non-JSON content stringify it (e.g. base64 for binary). See #193 for proper content-type support.").

## 2. Validator architecture

### Library

`github.com/getkin/kin-openapi/openapi3filter` (the validation subpackage of `kin-openapi v0.137.0`, already a direct dep). Exposes `ValidateResponse(ctx, ValidateResponseInput{...})` which checks a `*http.Response` against the matched route's response schema for the actual status code.

**Required `Options` flags** (verified against `kin-openapi/openapi3filter/validate_response.go:48-58` — defaults are unsafe):

- `IncludeResponseStatus: true` — without this, undeclared status codes pass silently. This flag is **load-bearing** for the design's claim that the validator catches undeclared statuses; verified by the fixture pinning test below.
- `MultiError: true` — accumulate all errors per response rather than failing on the first. Lets the per-response collection surface the full mismatch picture.

These flags are set in the validator package's constructor; tests verify they are set (a small unit test that constructs the validator and asserts `opts.IncludeResponseStatus == true`).

### Hook point

Wrap the `http.Handler` returned by `app.New(...)` before it's passed to `httptest.NewServer` in `internal/e2e/e2e_test.go`'s `TestMain`. The wrapper is a small `http.Handler` middleware that:

1. Constructs a tee-writer (a `http.ResponseWriter` wrapper that forwards bytes to the real writer AND buffers them) around the real `http.ResponseWriter`.
2. The tee-writer delegates the optional interfaces `http.Flusher`, `http.Hijacker`, and `io.ReaderFrom` via runtime type-checks against the underlying writer. `Flusher` is required for streaming endpoints to flush mid-stream. `Hijacker` is required for any websocket-like upgrade paths that may exist now or in future. `ReaderFrom` is required so `io.Copy` and `http.ServeContent` retain their fast path. `http.Pusher` and `http.CloseNotifier` are intentionally NOT delegated — Pusher is HTTP/2-server-push and unused; CloseNotifier is deprecated. Documented in the validator package's package doc.
3. Calls the wrapped handler with the tee-writer.
4. Routes the captured request through `kin-openapi`'s router (built once in `TestMain` from the embedded spec via `genapi.GetSwagger()`) to find the matched operation.
5. Calls `openapi3filter.ValidateResponse` with the buffered response — **except** when the matched operation's declared response content-type for the actual status code is `application/x-ndjson`, in which case body validation is skipped (see Streaming responses below). Note: the skip uses the *spec's* declared content-type for the matched operation, not the response's `Content-Type` header. Header-based detection would be fragile against handlers that auto-sniff types or future SSE/`text/event-stream` additions; spec-based detection skips exactly the operations we know are streaming.
6. If validation fails, appends the diff (operation, path, status, JSON path, expected, actual) to a process-level collector via a mutex-guarded `append`.

Single insertion point, zero changes to test code. The tee-writer pattern (vs `httptest.ResponseRecorder` which doesn't implement `http.Flusher` and would silently break streaming) is required because the spec contains streaming endpoints.

### Streaming responses

The spec declares `application/x-ndjson` for two response variants (the streaming variant of `getAllEntities` and `searchEntities`'s direct synchronous mode). `kin-openapi/openapi3filter.ValidateResponse` parses the body as a single JSON document and cannot validate ndjson, which is a stream of newline-delimited JSON values.

**Decision:** the validator skips response-body validation for `application/x-ndjson` responses but still validates the matched operation's status code and headers. The audit table flags these endpoints as "validator coverage: status only; body shape verified by hand." Future work could add a custom ndjson validator (line-by-line parse + per-line schema check) but is out of scope for this PR.

Streaming endpoints are documented in the audit table's notes column.

### Collector + report

The collector lives in `internal/e2e/openapivalidator/`:

```go
type Mismatch struct {
    Operation string
    Method    string
    Path      string
    Status    int
    JSONPath  string
    Reason    string
    TestName  string  // from t.Name() at request time, captured via context
}

var collector struct {
    mu  sync.Mutex
    out []Mismatch
}
```

A dedicated test `TestOpenAPIConformanceReport` lives in a file named **`zzz_openapi_conformance_test.go`**. Function name does NOT drive Go's test ordering — Go runs tests in source-declaration order within a file, processing files in alphabetical filename order. The `zzz_` filename prefix is what ensures this test runs after every other E2E test in the package.

The test reads the collector and:

- Writes the full mismatch list to a markdown file (`internal/e2e/_openapi-conformance-report.md`, gitignored).
- In `enforce` mode (Section 2 enforcement gate): if `len(collector.out) > 0`, calls `t.Fatalf` with a summary listing the first 20 mismatches and pointing to the full report file.
- In `record` mode: writes the file but does not fail. Used during audit and per-domain commits before commit 11.

This pattern (vs `os.Exit(1)` from `TestMain`) preserves Go's normal test cleanup machinery — `t.Cleanup` registrations run, postgres testcontainers are torn down, the test runner reports the failure with normal CI integration.

### `-run` single-test workflow

`go test -run TestEntity_Create` matches against the test function name; `TestOpenAPIConformanceReport` does not match `TestEntity_Create`, so the conformance test is excluded from a `-run`-filtered run. Without further mitigation, single-test workflows would silently skip drift detection.

**Mitigation:** the middleware itself enforces drift per-request when invoked from a `-run`-filtered run.

- Detect `-run` filter via `flag.Lookup("test.run").Value.String() != ""` once at suite start.
- If filtered AND mode is `enforce`: middleware calls `t.Errorf` directly on the captured `*testing.T` (extracted from request context — see Test-name capture below) when validation fails. Single-test workflow gets immediate per-request failures on the test that triggered them.
- If full suite (no `-run` filter): middleware only appends to the collector; `TestOpenAPIConformanceReport` fails once with the aggregate. This preserves the user-requested "single failure with list of deviations" pattern for the common full-suite case.

**Panic-safe `t.Errorf` from middleware.** Calling `t.Errorf` is safe from any goroutine while the test is running. Edge case: if a test issues a request and exits before the response (fire-and-forget pattern), the captured `*testing.T` is no longer valid by the time the middleware runs. Wrap the `t.Errorf` call in a `defer recover()` to swallow the panic and append to the collector instead — the conformance report still surfaces the mismatch, just without the per-test failure attribution.

The mode + filter combinations:

| Run kind | Mode | Per-request behavior | Aggregate test |
|---|---|---|---|
| Full suite | record | append-only | runs, reports, never fails |
| Full suite | enforce | append-only | runs, fails once with full list |
| `-run`-filtered | record | append-only | not invoked |
| `-run`-filtered | enforce | append-only AND `t.Errorf` on captured test | not invoked |

### Test-name capture

Each `httptest`-issued request gets the current test name attached via a context key set by a thin helper in `helpers_test.go`. Two paths to attach the test name:

- **Helper migration.** Replace `http.NewRequest(method, url, body)` with `e2e.NewRequest(t, method, url, body)` in every E2E test file. The helper sets the context value before returning the request.
- **Existing helper functions** (`postEntityE2E`, `getEntityData`, etc. in `helpers_test.go`) take `*testing.T` and use `e2e.NewRequest` internally. Most call sites already pass `t` — these conversions are mechanical.

The migration is **part of commit 1**, not deferred. Without test names attached at request time, the per-request `t.Errorf` mitigation for `-run`-filtered enforce mode (Section 2) cannot work — there's no `*testing.T` to call `Errorf` on. "Test name = unknown" is not a tolerable cliff; it disables the showstopper-S2 mitigation.

Estimated scope: ~14 E2E test files; mechanical find-and-replace plus passing `t` through any helpers that didn't already take it. Bounded.

### Coverage gap reporting

`TestOpenAPIConformanceReport` (in `zzz_openapi_conformance_test.go`) reports any operationId that was *never* exercised during the run — surfaces dead spots in E2E coverage. This list informs the per-domain commits (Section 8) — every uncovered op needs a happy-path test before merge. The uncovered-ops check only runs when there's no `-run` filter (single-test runs naturally exclude most operations).

### Enforcement gate

The validator runs in two modes controlled by a **package-level constant** in the validator package:

```go
// internal/e2e/openapivalidator/mode.go
const Mode = ModeRecord  // flipped to ModeEnforce in commit 11
```

- **`ModeRecord`** — runs validation, writes the report file, but does NOT fail the suite. Used for commits 1-10.
- **`ModeEnforce`** — same as record, but additionally fails the conformance test (full-suite run) or the requesting test via `t.Errorf` (`-run`-filtered run). Used from commit 11 onward.

Why a constant rather than env var: an env var lets developers diverge from CI silently (forget to set it locally, get green; CI sets it, fails) and lets someone toggle to record on `main` to silence a flaky validator. A constant change is reviewable code; toggling it requires a PR. If a future "I really need to run E2E without the validator just this once" escape hatch is ever needed, add a build tag — but until proven necessary, no escape hatch.

The flip in commit 11 is the gate signaling "every mismatch fixed; drift is now a hard failure." Commit 11 is **non-optional** — the PR does not merge with the constant in `ModeRecord` (see Section 8 acceptance).

**Mechanical enforcement of the flip.** Commit 11 also adds a unit test in the validator package:

```go
// internal/e2e/openapivalidator/mode_test.go
func TestModeIsEnforce(t *testing.T) {
    if Mode != ModeEnforce {
        t.Fatalf("Mode = %v; expected ModeEnforce on main. Re-flipping to ModeRecord requires explicit PR review.", Mode)
    }
}
```

The test pins the constant. Any future PR that flips back to `ModeRecord` must remove or change the test — visible in diff review, not silent. Lands in commit 11 alongside the constant flip.

### `-shuffle=on` incompatibility

Go's test runner supports `-shuffle on` (or `-shuffle <seed>`) which randomizes test order. This defeats the file-name-based ordering that ensures `TestOpenAPIConformanceReport` runs last. Under shuffle, the conformance test could run before other tests, find an empty collector, and falsely pass.

**Mitigation:** the conformance test detects shuffle at runtime and fails with a clear message:

```go
if v := flag.Lookup("test.shuffle"); v != nil && v.Value.String() != "off" {
    t.Fatalf("openapi conformance suite is not compatible with -shuffle; rerun without it")
}
```

Documented in the validator package's package doc. CI does not use `-shuffle`; developers running locally see the explanatory failure if they try.

## 3. Audit table — format and process

### Location

`docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md` (new directory). Checked in. Lives past the PR; the future external-reconciliation milestone consumes it.

### Format

One row per operationId. Columns:

| operationId | method | path | handler | spec response (today) | server response (today) | disposition | resolved-by-commit |
|---|---|---|---|---|---|---|---|

Where:
- **handler** = `internal/domain/<domain>/handler.go:<line>` linking to the function.
- **spec response (today)** = brief summary of what `api/openapi.yaml` claims for the 200/primary success status.
- **server response (today)** = what the handler actually writes.
- **disposition** = one of `match` / `fix-spec` / `fix-server` / `fix-both`. Default policy: server is source of truth, so most `fix-spec`; `fix-server` only when the server is genuinely emitting wrong wire data (e.g. JSON-in-string, missing JSON tags producing PascalCase).
- **resolved-by-commit** = filled in as commits land — short SHA or commit subject.

### Process

1. Initial pass during the audit-foundation commit fills in `operationId`, `method`, `path`, `handler`, `spec response (today)`, `server response (today)` for all 81 ops. Disposition starts empty.
2. Per-domain commits (Section 8) fill in disposition + resolved-by-commit as defects are fixed.
3. By the final commit, every row's disposition is non-empty.
4. PR description links the audit table; reviewers spot-check rows.

### Generation

The implementing agent populates the table directly — reading the spec and the ~11 handler files is fast (minutes, not hours, at LLM pace). No tooling required.

Process:
1. Parse `api/openapi.yaml` to enumerate the 81 ops with method + path + tag.
2. For each op, find the `ServerInterface` method in `api/generated.go` and the implementing function in `internal/domain/<domain>/handler.go`. Record handler:line.
3. Read the handler's `WriteJSON` / `WriteError` calls to characterize the actual response shape. Record in the "server response" column.
4. Read the spec block for the same op to characterize the declared response. Record in the "spec response" column.
5. The `record`-mode validator output (Section 2) cross-checks the human-recorded "server response" against the actual wire — any disagreement flags a discrepancy to investigate before per-domain commits start.

**Tie-breaker rule** when validator output and audit-table row disagree:

- **Step 1: verify the validator matched the right operation.** Inspect the captured request method + path; confirm `kin-openapi`'s router resolved it to the operationId the audit row claims. If the router mis-matched (e.g. a path with overlapping templates), the *spec* is the bug — fix the path templates so resolution is unambiguous, then re-run.
- **Step 2: if the router match is correct, the audit row is wrong.** Re-read the handler at the recorded line and update the row to match what the wire actually shows.
- **Step 3: if neither (1) nor (2) resolves it, stop and surface.** This is a Gate 6 trigger — something subtle is happening (middleware rewriting the response? content-encoding interfering? response sent before headers written?) that needs design input before continuing.

The cross-check exists precisely because hand-recording 81 rows is mechanical and error-prone. The tie-breaker rule prevents silent acceptance of either side when they disagree.

### Future use

When the external-reconciliation milestone opens, the table is the starting point — extended with two more columns (`Cloud spec response` / `Cloud disposition`) and walked again with Cloud's spec as the second axis.

## 4. Spec changes (`api/openapi.yaml`)

### Schema additions

New named components in `components/schemas/`:

- `Envelope` = `{type: string, data: object (polymorphic), meta: object}` — for `getOneEntity`.
- `EnvelopeList` = `array<Envelope>` — for `getAllEntities`.
- `EntityChangeMetadata`, `EntityStatistics` (+ 3 by-state/by-model variants), `WorkflowTransition`, `TransitionsList`, `AuditEvent` (3-variant `oneOf` with `discriminator.propertyName: type`), `SearchSnapshot`, `SearchJobStatus`, `SearchJobResults`, `ModelExportResponse`, `ModelImportResponse`, `AccountInfo`, `SubscriptionList`, `TechnicalUser`, `OidcProvider`, `JwtKeyPair`, `EdgeMessage`, `EdgeMessagePayload`.

The exact list emerges during the audit pass; the design commits to the set, the audit row for each op confirms which named schema applies.

### Schema fixes

- All 6 sites with `type: array` + sibling `$ref` (the original #21 anti-pattern) become well-formed `type: array, items: { $ref: ... }`.
- Loose `type: object` blocks replaced with `$ref` to a named schema (or `additionalProperties: true` for polymorphic-by-intent fields with a `description: polymorphic by intent; user-supplied content` marker).
- `basicAuth` declared in `components.securitySchemes`.
- `messaging.GetMessage.content` declared as `EdgeMessagePayload` (polymorphic), not `type: string`.

### Per-operation error declarations

Every operation gets:

- A success block per actually-emitted status (`200`/`201`/`204`).
- Per-operation 4xx blocks for each error code the *handler* emits, sourced by reading `common.WriteError` call sites in `internal/domain/**/handler.go`.
- A **shared 5xx fragment** referenced by every operation: `default: $ref: '#/components/responses/InternalServerError'` where the response component is `ProblemDetail` with the ticket-UUID shape per CLAUDE.md.
- A **shared 401 fragment** referenced by every operation under `bearerAuth`: `401: $ref: '#/components/responses/Unauthorized'`.
- A **shared 403 fragment** for tenant-isolation enforcement: `403: $ref: '#/components/responses/Forbidden'`.

Shared fragments live in `components.responses` — declared once, `$ref`'d from each operation. Avoids 81 copies of the same 5xx block.

### Polymorphic markers

Fields that intentionally accept any JSON (entity `data`, edge message payload) get `description: polymorphic by intent; user-supplied content` and remain `additionalProperties: true`.

### Tag exclusions

`Stream Data`, `CQL Execution Statistics`, `SQL-Schema` stay excluded in `api/config.yaml` — out of scope for cyoda-go regardless of this PR.

### Validation against the audit

The validator (Section 2) catches drift between these declarations and what the server actually emits — that's the runtime guard. The spec-tightening commits write the schemas; the per-domain commits fix any mismatches the validator surfaces.

## 5. Handler defect fixes

### Confirmed defect — `messaging.GetMessage.content` JSON-in-string

`internal/domain/messaging/handler.go:183` returns `content: string(payloadBytes)`. Wire today is `{"content": "{\"actual\":\"json\"}", ...}`. Wire after fix is `{"content": {actual: "json"}, ...}`.

Change:
```go
// before
"content": string(payloadBytes),
// after
"content": json.RawMessage(payloadBytes),
```

**No fallback helper, but remove the existing dead-code path that motivates one.** The current `NewMessage` handler (`internal/domain/messaging/handler.go:59-66`) has this code:

```go
var compacted bytes.Buffer
if err := json.Compact(&compacted, envelope.Payload); err != nil {
    // Not valid JSON — store as-is (payload is opaque).
    compacted.Reset()
    compacted.Write(envelope.Payload)
}
```

The fallback branch is **dead code**: `envelope.Payload` is a `json.RawMessage` field that has just been successfully populated by `json.Unmarshal` (which validates JSON during parsing — invalid input would have been rejected at line 49 before reaching here). The "Not valid JSON" comment is misleading and the branch is unreachable.

Per Gate 6, **replace the dead branch with a defensive guard** as part of the messaging commit:

```go
var compacted bytes.Buffer
if err := json.Compact(&compacted, envelope.Payload); err != nil {
    // Unreachable in practice: json.Unmarshal above validated envelope.Payload
    // as a JSON value. This guard documents the invariant and surfaces the
    // bug as a 500 if a future code path violates it (e.g. constructing
    // envelope.Payload by hand instead of via json.Unmarshal).
    common.WriteError(w, r, common.Internal("payload validation invariant broken", err))
    return
}
```

The branch is effectively unreachable today — if `json.Unmarshal` validated the envelope (line 49), `envelope.Payload` is valid JSON and `json.Compact` cannot fail on it. The guard is intentionally retained as **executable documentation** of the invariant: anyone who modifies the handler in the future and breaks the invariant gets an honest 500 instead of silent corruption.

After this cleanup, the **invariant holds**: every byte sequence stored as a payload via the handler is valid JSON. `json.RawMessage(payloadBytes)` in `GetMessage` is then always safe to marshal.

**External-write risk.** Storage could in principle be populated via paths that bypass the handler (migrations from a legacy system, direct DB edits, future bulk-import endpoints). If invalid JSON arrives in storage that way, `json.RawMessage(payloadBytes).MarshalJSON()` will surface a marshal error → handler returns 500 with a ticket UUID per CLAUDE.md's 5xx convention. This is correct: the contract is "stored bytes are valid JSON"; violating the contract via an out-of-band write produces an honest 500, not a silent shape-shift.

**Considered and rejected: a `rawJSONOrString` helper that falls back to `string(b)` for non-JSON bytes.** Two reasons to reject:
- It would mask storage corruption silently. If invalid JSON ever appears in storage, that's a bug to surface, not paper over.
- The wire shape `"content": "<xml>..."` is ambiguous — a client can't tell whether that's a stringified payload (the #193 workaround) or stored XML returned via fallback. Forcing one path through `json.RawMessage` keeps the wire unambiguous.

If/when a future ingestion path is added that doesn't go through `NewMessage` (e.g. a bulk-import endpoint), it MUST validate input JSON at the boundary — same invariant. Adding such a path without enforcing the invariant would be a Gate 6 stop-and-ask in that PR, not a problem to solve here.

Spec change in lockstep — `EdgeMessagePayload` becomes the field's schema (polymorphic) instead of `type: string`. The polymorphic schema admits any JSON value, including JSON strings used per the #193 workaround. The constraint named in #21 stays: when `contentType` is genuinely binary (`application/octet-stream`), base64 string with `format: byte` remains correct; the rule applies only when the bytes are JSON. **Today's reality** (per the spec's `contentType` description and the workaround documented in #193) is that `contentType` is informational — clients stringify non-JSON content into JSON strings. The polymorphic `EdgeMessagePayload` accommodates that workaround without ambiguity.

Test pinning: a new E2E test posts a message with a JSON payload, calls `GetMessage`, asserts the `content` field is parseable as JSON without a second `json.Unmarshal` (i.e. it's already JSON, not a string). The validator's `EdgeMessagePayload` schema then prevents future regression on the wire shape.

### Audit-discovered defects

Other shape defects surface during the audit pass. Per Gate 6, each gets fixed inline via TDD (red test → green fix) in the same domain commit, unless the fix:

- requires structural change beyond the wire shape (stop-and-ask),
- requires a design decision (stop-and-ask), or
- would balloon a single domain commit beyond reviewability (split into a focused commit, but still inside this PR — no follow-up issue).

### Likely candidates (to be confirmed during audit, not committed to as scope yet)

- Any `WriteJSON(w, x, status)` site where the Go value's field tags don't match the spec (the `EntityEnvelope`-via-`map[string]any` pattern is the one we know about; others may exist).
- Any handler emitting an undeclared status code (will surface as a validator mismatch — "status N not declared for operation X").
- Any handler emitting a `ProblemDetail.code` value not declared in the operation's 4xx blocks.

### Fix-vs-defer decision rule (Gate 6 surface)

- Mechanical (one handler, one wire-shape change, test pins it) → fix in this PR.
- Structural (e.g. service-layer type needs reshaping to fix the wire) → stop and surface.

## 6. E2E coverage closure

Every operationId gets at least a happy-path E2E test before merge. Process:

1. **First validator run (audit foundation commit) prints the uncovered list.** This is the work backlog for the per-domain commits.
2. **Per-domain commits add minimal happy-path tests** for each uncovered op. "Minimal" means one positive-path call, no edge-case coverage; just enough to exercise the wire shape so the validator's automatic guard inherits coverage.
3. **The final cutover commit's validator run shows zero uncovered ops.** That's the Section 2 acceptance signal.

### Test-writing scope per uncovered op

- For ops with simple prerequisites (e.g. `getEntityStatistics` — no setup needed), a single test function: setup minimal state, call the endpoint, assert 200, validator pins the shape.
- For ops with complex prerequisites (e.g. async search jobs requiring a full search-and-poll cycle), reuse existing test helpers if they exist; otherwise add a focused helper in `helpers_test.go`. If the helper itself becomes a significant lift (>~50 lines), surface as a Gate-6 stop-and-ask — it might mean the op needs proper feature work, not a shape probe.
- For ops we genuinely can't test (e.g. multi-node cluster state we don't model in E2E), document the gap in the audit table's notes column and stop-and-ask. Don't fake coverage.

### Estimated work

Unknown until first validator run produces the uncovered list. Audit foundation commit will tell us the size; the design commits to closing whatever it surfaces but acknowledges scope visibility is delayed until the audit runs.

## 7. Derived-artefact updates

Three artefacts update in lockstep with the per-domain commits, not in a separate batch.

### `e2e/parity/client/types.go`

Hand-rolled mirror types for the wire format (sometimes drift from server reality, per the M3 design doc's "approved deviation"). After the spec fixes land, these types update to reflect the corrected shapes. Likely shrinks via re-export from `genapi.*` where the generated type is a clean fit; otherwise keeps hand-rolled types with corrected fields.

### `e2e/parity/registry.go` and parity scenarios in `e2e/parity/*.go`

Pinpoint each scenario whose assertion implicitly relied on a now-fixed shape (e.g. JSON-in-string `content`, missing envelope wrapper, malformed `EntityTransactionResponse` array). Update assertions to consume the corrected wire format. The Cassandra plugin (out of scope) consumes this registry via Go module dep and surfaces any breakage on its next dep update — file an issue in `cyoda-go-cassandra` only if a backwards-incompatible interface shift surfaces.

### `cmd/cyoda/help/content/openapi.md`

Narrative content that may reference corrected fields. Audited and updated where the narrative would be misleading after the fix. The `cyoda help openapi {json,yaml,tags}` action outputs auto-emit from the embedded spec via `genapi.GetSwagger()` — no code change needed there.

### Lockstep rule

Every commit that changes a response schema in `api/openapi.yaml` also updates the corresponding handler (if any handler-side fix is needed), parity test (if affected), and narrative (if affected) in the same commit. No "schema first, fix tests next commit" — that violates Gate 5 (would leave intermediate commits with failing tests).

## 8. Commit topology

Foundation-then-domains. ~10-11 commits.

### Foundation

1. **Validator + collector + report + pattern fixtures.** Adds `internal/e2e/openapivalidator/` package with collector, `Mismatch` type, the wrapping tee-writer middleware, the `TestOpenAPIConformanceReport` test in `zzz_openapi_conformance_test.go`, and the pinning fixture suite. Wires into `internal/e2e/e2e_test.go`'s `TestMain`. Mode constant starts at `ModeRecord`. No spec or handler changes yet.

   The pinning fixtures are unit tests in the validator package that feed known wire shapes against fragment specs and assert the collector records the expected mismatch. **Front-loaded coverage of both load-bearing schema patterns** so per-domain commits inherit a verified template:
   - **Each of the four #21 confirmed defects** (POST array shape mismatch, GET envelope mismatch, JSON-in-string content, undeclared status caught by `IncludeResponseStatus: true`).
   - **Polymorphic body pattern** — fragment with `additionalProperties: true` + `description: polymorphic by intent`; fixture asserts it accepts arbitrary JSON values without raising mismatch.
   - **Discriminator union pattern** — fragment with `oneOf` + `discriminator.propertyName: type`; fixture asserts each variant's positive case validates and that an undeclared variant raises mismatch.

   With both patterns verified in commit 1, per-domain ordering (Section 8) stops being a "discover the load-bearing flaw" exercise — the patterns are already known to work; per-domain commits just apply them.

   Build green; existing E2E tests pass; record-mode validator runs against the current (drifted) spec and produces the first mismatch report at end-of-suite. Mismatch list informs the audit table.
2. **Audit table.** Adds `docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md` with all 81 ops listed: `operationId`/`method`/`path`/`handler` enumerated by reading the spec and `api/generated.go`; `spec response` and `server response` columns populated by reading each handler (Section 3 process). Disposition column empty; resolved-by-commit column empty. The `record`-mode validator output from commit 1 cross-checks the human-recorded "server response" column against the actual wire and surfaces audit-table errors before per-domain commits start.

### Per-domain commits

(One per domain, each: spec changes + handler fixes + new E2E coverage + parity updates + audit table rows updated.)

With pattern fixtures front-loaded into commit 1, per-domain order is no longer "discover the load-bearing flaw" — it's "apply known-good templates." Order by tag-alphabetic to remove personal preference from the sequence:

3. **account / IAM** (10 ops; mostly simple GETs)
4. **audit** (4 ops; uses the discriminator-union pattern from commit 1's `AuditEvent` fixture)
5. **dispatch / health** (4 ops; trivial)
6. **entity** (14 ops; includes the original #21 confirmed defects — POST array, GET envelope)
7. **messaging** (5 ops; uses the polymorphic-body pattern from commit 1's fixture; includes the JSON-in-string fix and #193's documentation marker)
8. **model** (12 ops; export/import; XML content-type handled per spec audit)
9. **search** (6 ops; includes streaming `application/x-ndjson` skip per Section 2)
10. **workflow** (8 ops)

### Final cleanup

11. **Mode flip + derived artefacts + final consistency check.** Flip the validator package's `Mode` constant from `ModeRecord` to `ModeEnforce` (Section 2). `cmd/cyoda/help/content/openapi.md` narrative pass; final `e2e/parity` consistency check; verify ADR 0001 is unchanged (no decision drift during execution); close out any audit-table rows still empty (everything `match` if no fix was needed); confirm `TestOpenAPIConformanceReport` reports zero mismatches and zero uncovered ops.

### Verification cadence

After each commit: `go build ./... && go test -short ./...`. Before merge: `make test-all && go test -race ./...` (CLAUDE.md gates).

### Acceptance gate — non-optional commit 11

The PR **does not merge** unless commit 11 lands and the validator's mode constant is `ModeEnforce`. Commits 1-10 in isolation leave the codebase in a state where drift is collected but not enforced — useful as a transitional state on the feature branch but unsafe on `main` (drift could creep back in silently in any subsequent PR while no one is looking).

PR description checklist explicitly includes "validator constant is `ModeEnforce`" and "`TestOpenAPIConformanceReport` reports zero mismatches and zero uncovered ops." Reviewer verifies both before approving merge.

### Order rationale

**Patterns front-loaded into commit 1's fixture suite**, then per-domain commits in alphabetical order. This removes the false dichotomy of "easy first vs. hard first" — the load-bearing patterns (polymorphic body, discriminator union) are validated as fixtures before any domain commit lands, so per-domain ordering doesn't carry pattern-discovery risk. Alphabetical removes preference from the sequence and is a natural reading order for reviewers who want to walk the PR linearly.

## 9. Risk register

Risks ordered by likelihood × impact, each with a mitigation:

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| `kin-openapi/openapi3filter` doesn't catch a class of mismatch we care about (e.g. silently passes a missing required field, doesn't validate `oneOf` discriminator dispatch correctly) | Medium | High | Validator must catch all four named #21 defects as fixtures (POST array, GET envelope, JSON-in-string, basicAuth) BEFORE being trusted as a guard. A small unit test under `internal/e2e/openapivalidator/` feeds each defect's wire shape against its operation's spec and asserts the collector records the expected mismatch. If any fixture fails to surface, investigate the validator before continuing the audit |
| Operations not exercisable in E2E (e.g. require multi-node cluster state we don't model) leave coverage holes | Medium | Medium | Stop-and-ask at the per-domain commit. Document the gap in the audit table's notes column. Don't fake coverage; either expand E2E infrastructure or accept the documented gap |
| Audit pass surfaces a handler defect that requires service-layer changes to fix (Gate 6 stop-and-ask) | Medium | Medium | Each per-domain commit can stop on its own surface. If the fix balloons, surface the choice: either land a bounded fix in this PR or split off as a separate issue with explicit scope boundary |
| Cyoda Cloud reference spec (`docs/cyoda/openapi.yml`) shows the same defect as cyoda-go, so "fix the spec" doesn't have a clear right answer (server-vs-Cloud-vs-cyoda-go-spec triangle) | Low | Medium | Default: server is source of truth (per #21 body). Note the Cloud divergence in the audit table's notes column. The future external-reconciliation milestone resolves it; this PR doesn't |
| Validator runtime cost in E2E suite is non-trivial | Low | Low | E2E suite already takes ~30s; validator adds tens of microseconds per response. Negligible. If wrong, wrap behind a build tag or env var |
| Per-op error-status declarations explode the spec size (81 ops × 4-6 error blocks each) | Medium | Low | Use shared `responses` components — `default`, `Unauthorized`, `Forbidden`, `BadRequest` defined once in `components.responses`, `$ref`'d from each operation. Adds ~3 lines per operation, not 30 |
| `e2e/parity` registry changes break the Cassandra plugin on its next dep update | Low | Medium | Out of scope per #21. If parity test fails for Cassandra, file an issue in `cyoda-go-cassandra`. Wire-shape changes should not affect the SPI surface; parity test assertion changes might |
| Per-domain commits drift in style — each one writes the audit table entries differently, error declarations differently | Medium | Low | Foundation commit 2 establishes the audit-table convention; the first per-domain commit (account/IAM) sets the implementation pattern. Subsequent commits follow it |
| `cmd/cyoda/help/openapi*.go` artefacts consumed by cyoda-docs change shape, breaking docs build | Medium | Low | cyoda-docs takes a snapshot of the help output; the docs repo will need a sympathetic update. Cross-repo coordination noted in PR description |
| 5xx envelope misuse — handler returns an unsanitized error message in the body, leaking internals | Low | High (security) | Existing `common.Internal(...)` already structures the 5xx envelope correctly. Validator catches shape drift but not message content. Reviewer scans new handler code for `common.Internal(msg, err)` calls and verifies `msg` is generic. Per CLAUDE.md no-leak rule |

### Stop-and-ask triggers (Gate 6 surface points)

- Validator misses a fixture (showstopper — investigate before continuing)
- Audit surfaces a handler defect requiring service-layer rework
- An operation has no clean E2E coverage path
- A spec defect is shared between cyoda-go and Cloud, so "match the server" doesn't resolve which form is canonical

In each case: stop, surface the choice, do not silently pick.
