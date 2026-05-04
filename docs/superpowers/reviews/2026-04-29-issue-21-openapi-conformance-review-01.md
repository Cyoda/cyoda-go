# Design Review — Issue #21 OpenAPI Server-Spec Conformance

**Date:** 2026-04-29
**Reviewing:** `docs/superpowers/specs/2026-04-29-issue-21-openapi-conformance-design.md`
**Reviewer:** Independent agent acting as senior systems architect + Go expert; context-free
**Branch:** `issue-21-openapi-conformance`

## 1. Overall assessment

The strategy is sound. Choosing `kin-openapi/openapi3filter.ValidateResponse` as a runtime guard at the E2E boundary — instead of an `oapi-codegen` strict-server / `ogen` migration — is the right call for the project's current state (no consumers, hand-edited spec, 3.1 syntax, polymorphic payloads). ADR 0001 properly anchors the decision, and the spec follows it without drift.

The spec is also well-organized along the right axes: validator first, audit second, then per-domain commits, with derived artefacts (parity, help) updated in lockstep. The risk register is unusually honest — naming the discriminator-union worry, the parity-cascade worry, and the cross-repo cyoda-docs sympathy update.

Two non-trivial gaps need to be closed before the plan is written: (a) the validator design as written will silently miss undeclared status codes and undeclared `Content-Type`s, which is the second-most-likely defect class after shape drift; (b) the `tools/audit-skeleton/` story under-counts the actual mapping work and will produce a noisy initial pass that's hard to triage. Both are fixable inside the same PR — they don't change the architecture — but they need to be specified, not discovered during execution.

## 2. Showstoppers (must resolve before plan-writing)

### S-1. `Options.IncludeResponseStatus` must be set, and the design must say so.

`openapi3filter.ValidateResponse` (verified at `~/go/pkg/mod/github.com/getkin/kin-openapi@v0.137.0/openapi3filter/validate_response.go:48-58`) will silently **pass** any response whose status code isn't declared in the operation, unless `Options.IncludeResponseStatus = true` is explicitly set:

```go
responseRef := responses.Status(status)
if responseRef == nil { responseRef = responses.Default() }
if responseRef == nil {
    if !options.IncludeResponseStatus { return nil }   // <-- silent pass
    return &ResponseError{Input: input, Reason: "status is not supported"}
}
```

Section 5's "Likely candidates" claims "Any handler emitting an undeclared status code (will surface as a validator mismatch)". That is **false by default** with this library. The validator must construct `Options{IncludeResponseStatus: true, MultiError: true}` and the design must commit to that explicitly. Add it to Section 2 ("Library") and to the four-fixture pinning test in Section 9 (add a fifth fixture: handler returns a status the spec doesn't declare → mismatch recorded).

This is a one-line fix but it changes the validator's actual coverage profile. Without it, a major class of drift goes undetected, including the original #21 "POST array vs single object" defect's cousins (handlers returning 200 where the spec says 201, or vice versa).

### S-2. `application/x-ndjson` streaming responses cannot be validated by `kin-openapi`, and the design pretends otherwise.

`internal/domain/search/handler.go:166-175` writes `application/x-ndjson` with mid-stream JSON encoding and (likely) `http.Flusher` calls. `openapi3filter.ValidateResponse` at line 105-112 looks up `content.Get(inputMIME)` and validates against the schema for that content type — but for ndjson the schema describes one record per line, not a JSON document, so the line-by-line decode-and-validate isn't what the library does. It will either:
- See `application/x-ndjson` declared in the spec, decode the entire concatenated stream as one JSON value (which fails), and report a spurious mismatch; or
- See no schema declared for `x-ndjson` (line 114-117), pass silently, and we have zero coverage on streaming.

Additionally the `httptest.ResponseRecorder` proxy described in Section 2 step 1 buffers the entire response before flushing — that defeats streaming for the test client and breaks any test that relies on incremental delivery (e.g. async-cancel mid-stream).

Resolutions to choose between, in order of preference:
1. Skip validation for `application/x-ndjson` content type explicitly (validator wrapper inspects `Content-Type` header before invoking `ValidateResponse`); document the gap in the audit table notes column.
2. Add a small streaming-aware adapter that splits on `\n`, decodes each line as JSON, and runs `Schema.VisitJSON` on each record. Bounded work but real (~50 lines).
3. Use a tee-reader instead of `ResponseRecorder` so streaming continues to work, then validate from the buffered copy after the handler returns.

The design needs to pick one. As written it implies streaming "just works", which it won't.

## 3. Strong concerns (resolve at design level)

### C-1. Validator-before-spec-fix ordering produces a noise floor that swamps signal.

Section 8's commit 1 lands the validator against the **drifted** spec. The first run will produce ~100+ mismatches — every `type: object` site, every missing 401/403/500 declaration, every `type: array + sibling $ref` malformation. That's not a useful signal for the audit pass; it's a haystack.

Suggested re-ordering:
- Commit 1: validator wired in **disabled-by-default** (env-var gate, e.g. `CYODA_OPENAPI_VALIDATE=1`), with the four-fixture pinning unit tests proving it works.
- Commit 2: audit table skeleton.
- Commits 3..N: per-domain fixes. The validator is enabled only **after** the audit pass and the spec-tightening commits are in — so its first signal is real drift, not known drift.
- Final commit: validator enabled by default in `TestMain`; `os.Exit(1)` on mismatch.

Alternative: keep the order, but have the validator's first run write its noisy output to the audit-table generator's input, so commit 2 consumes commit 1's report rather than re-deriving spec shapes by hand. Either way: don't conflate "validator is wired" with "validator gates CI" until the spec is plausibly clean.

### C-2. `tools/audit-skeleton/` understates the work.

Section 3's claim that `operationId → handler line` mapping is "partly mechanical" via "oapi-codegen's `ServerInterface` method names + grep" is half-true. `oapi-codegen` produces method names in PascalCase derived from operationId; matching them to the actual `func (h *Handler) Foo(...)` definition is a grep — but the **response shape** column is the work, and that's per-handler reading of `WriteJSON(w, x, status)` call sites (often constructing `map[string]any` or returning service-layer types whose JSON tags need to be read transitively).

For 81 ops at ~10-30 minutes per row of "what does this handler actually emit on the wire?" that's 14-40 hours. The design should either:
- Acknowledge that explicit cost (so the per-domain commit estimates are realistic), or
- Cut the column by deriving it from a recorded validator run rather than human reading. Run every E2E test once with a "record actual response shape" sink, then auto-fill the "server response (today)" column from observed shapes. Reduces the human pass to disposition + spec-side reading.

### C-3. `os.Exit(1)` from `TestMain` skips `t.Cleanup()` — this matters more than the design admits.

`testcontainers-go` registers cleanup via `t.Cleanup` and `TestMain` flow; calling `os.Exit(1)` after `m.Run()` returns will skip those cleanup hooks. Postgres containers will leak and need manual `docker rm` between runs. CI is usually fine (containers die with the runner), but local dev becomes annoying fast.

Better pattern:
```go
exit := m.Run()
if validator.HasMismatches() {
    validator.WriteReport(...)
    if exit == 0 { exit = 1 }
}
// allow deferred cleanup (testcontainers Reaper, etc.)
os.Exit(exit)
```

Or push the assertion into a single `TestOpenAPIConformance(t *testing.T)` test that runs after `m.Run()` returns — keeps Go's test reporting machinery, surfaces in CI as a normal failed test, and doesn't bypass cleanup. This is also better for "developer ergonomics: running a single test should still work" — currently as designed, `go test -run TestSomeOneTest` would still trigger an `os.Exit(1)` if that one test causes a mismatch, which is correct, but it would also trigger if the validator's "operation never exercised" tracker fires for the other 80 ops. The design needs to handle the single-test-run case explicitly.

### C-4. "Operation never exercised" tracking is broken under `-run` and parallel test execution.

The collector reports any operationId not exercised. In a `-run` filtered run that's noise. With `t.Parallel()` it's not racy (the collector tracks union of seen ops across goroutines, mutex-guarded), but the report-time decision "should we fail because op X was never seen?" must distinguish "full suite ran" from "filtered run". Suggested rule:

- If `flag.Lookup("test.run").Value.String()` is non-empty (i.e. a `-run` filter is in effect), only report shape mismatches for tests that did run; suppress the "uncovered ops" report.
- If running the full suite, both reports fire.

Without this, the validator becomes hostile to fast-iteration single-test runs.

## 4. Moderate concerns

### M-1. Discriminator union (`AuditEvent` `oneOf` + `discriminator.propertyName: type`).

Verify against `kin-openapi`'s `validation_discriminator_test.go` (present in v0.137.0) that the discriminator dispatch actually works the way the design assumes. The library does support discriminators, but in OpenAPI 3.1 the `discriminator.mapping` object is required for non-trivial dispatch (otherwise it falls back to matching `type` value against `$ref`'d schema names). The design says only `propertyName: type` — confirm the schema names match the `type` values, or add `mapping`.

Also: `oapi-codegen` v2.6.0 with `std-http-server: true` does generate Go types from `oneOf` with discriminator, but as `interface{}`-shaped sum types via embedded marshalling helpers. The validator catches the wire shape; the codegen's Go type is irrelevant since handlers return `map[string]any` (per ADR 0001's "loose `WriteJSON` pattern stays"). Design's "code that `oapi-codegen` actually handles" is a non-question for this approach — note that explicitly so future readers don't worry.

### M-2. `additionalProperties: true` validation behavior.

`kin-openapi` does honor `additionalProperties: true` correctly (verified by reading `openapi3.Schema.VisitJSON` semantics — extra fields are permitted). But the design uses `additionalProperties: true` as a "polymorphic by intent" marker, which means the validator effectively can't distinguish "field is intentionally polymorphic" from "field shape is unspecified". For `EdgeMessagePayload` this is the intended outcome. For other sites it might mask real defects. Recommend tagging each polymorphic site with `x-cyoda-polymorphic: true` (custom extension) so future tightening can grep for them, rather than treating `additionalProperties: true + description: "polymorphic by intent"` as the marker.

### M-3. `string(payloadBytes)` → `json.RawMessage(payloadBytes)` fix is correct *but* requires `payloadBytes` to be valid JSON.

`encoding/json` will marshal `json.RawMessage` as inline bytes (not as a string). Confirmed correct. **However**: if `payloadBytes` happens to be invalid JSON (e.g. a binary payload from the documented #193 stringify-as-base64 workaround), `json.Marshal` on the enclosing map will return an error and the response becomes a 500. Today's behavior wraps it in a string and survives.

The design needs a safety net here: either
- Validate `payloadBytes` is JSON before assigning (`json.Valid(payloadBytes)`), and if not, fall back to base64-encoded string with a warning header, or
- Document explicitly that `messaging.PostMessage` now rejects non-JSON `content` at write time (and add the validation there), and that GetMessage on a pre-existing non-JSON payload returns a 500 with a ticket UUID until #193 lands.

The design's spec change (`EdgeMessagePayload` polymorphic) is fine; the handler-side defensive code is what's missing.

### M-4. 50-line helper threshold (Section 6) is arbitrary.

Either drop the number and say "stop-and-ask if writing the helper feels disproportionate to the shape probe" (honest, judgment-call), or replace with a real heuristic ("stop-and-ask if the helper requires invoking >2 other endpoints to set up state"). The 50-line number will get gamed.

### M-5. Per-domain ordering (account/IAM first, entity last) is fine, but a "spike commit" is missing.

Account/IAM is too easy to surface a load-bearing pattern flaw. The first per-domain commit should hit a domain that exercises the hardest patterns (polymorphic body, status code variants, error envelope) — that's `messaging` (JSON-in-string + polymorphic content) or `entity` (envelope + array-vs-single). Going easy-first means the pattern is set on simple cases and may not survive contact with the hard ones, forcing rework of the early commits.

Suggested order: validator + fixtures → audit skeleton → **messaging** (proves the hard pattern) → entity (largest, original #21 defects) → audit/search (discriminator/ndjson — high risk) → model → workflow → account/IAM → dispatch/health → derived artefacts.

## 5. Nitpicks

- Section 2's "Constructs a `httptest.ResponseRecorder` proxy around the real `http.ResponseWriter`" — `ResponseRecorder` doesn't proxy; it captures. The design likely means a custom `http.ResponseWriter` that tees to both the recorder and the real writer. Worth being precise: `httptest.ResponseRecorder` doesn't implement `http.Flusher` / `http.Hijacker`, which trips `application/x-ndjson` (S-2) and any future SSE/WebSocket handler. Use a custom `responseTee` type that implements the optional interfaces.
- Section 3's audit table column "resolved-by-commit" assumes squash-merge produces one SHA per row, which won't hold inside the PR's commit history (each domain commit will resolve many rows). Either fill in the column at PR-merge time with the squash SHA, or rename to "resolved-in-domain" and use the domain name.
- Section 4's "Tag exclusions" mentions `Stream Data`, `CQL Execution Statistics`, `SQL-Schema` — verify these are still in `api/config.yaml` and not stale references. If excluded, the validator should also skip operations with those tags so the "operation never exercised" report doesn't list them.
- ADR 0001 line 215: validator hook described as "`RoundTripper` wrapping `httptest.NewServer`'s client". The design rightly chose handler middleware instead (better — covers all clients, not just `httptest`'s default one). The ADR's "Open items" should be updated post-merge to reflect the chosen placement.
- Section 9 risk "Validator runtime cost" cites "tens of microseconds" — closer to single-digit milliseconds for non-trivial schemas with `MultiError` enabled. Still negligible against E2E suite wall-clock. Worth measuring, not estimating.

## 6. What's done well

- ADR 0001 is exemplary: alternatives evaluated with concrete failure modes (ogen v1.20.3 actually run, six failing operations enumerated), not strawmanned.
- The four-fixture validator-pinning gate (Section 9 first risk row) is the right discipline — "trust the validator only after it catches the bugs we already know about."
- Lockstep rule (Section 7) — schema + handler + parity + narrative in one commit — prevents the most common drift class during this kind of work.
- Risk register names cross-repo concerns (cyoda-go-cassandra parity registry, cyoda-docs help artefact consumers) instead of pretending the blast radius stops at this repo.
- "Don't fake coverage" in Section 6 for genuinely-untestable ops, with explicit Gate-6 stop-and-ask. Better than the usual "TODO: add coverage later" punt.

## 7. Bottom line

Approve direction; revise design before plan-writing. Two showstoppers (S-1 `IncludeResponseStatus`, S-2 ndjson streaming) are concrete library/protocol issues that the validator architecture has to handle explicitly — not discover during execution. Four strong concerns (commit ordering vs noise floor, audit-skeleton cost realism, `os.Exit` cleanup, single-test-run UX) are design-level and cheap to fix in the spec. The core architectural choice (runtime validation via `kin-openapi` middleware) is right; the spec's risk register and lockstep rules are the right kind of discipline. Reorder messaging earlier in the per-domain commits to surface the hard pattern before the easy ones lock in a pattern that doesn't generalize.
