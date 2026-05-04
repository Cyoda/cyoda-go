# Design Review — Issue #21 OpenAPI Server-Spec Conformance (Revision 3)

**Date:** 2026-04-29
**Reviewing:** `docs/superpowers/specs/2026-04-29-issue-21-openapi-conformance-design.md` (revised twice)
**Reviewer:** Independent agent acting as senior systems architect + Go expert; context-free
**Branch:** `issue-21-openapi-conformance`

## 1. Overall assessment

Revision 3 is materially closer to executable. The Go test mechanics issues from review 02 (S-1 file-vs-function ordering, S-2 `-run` filter dropping the gate) are addressed correctly: the conformance test moves to `zzz_openapi_conformance_test.go` (filename ordering, not function name), and a per-request `t.Errorf` mitigation handles the `-run`-filtered case. The tee-writer optional-interface set is enumerated with rationale (`Flusher`/`Hijacker`/`ReaderFrom` delegated; `Pusher`/`CloseNotifier` justified as omitted). The env-var control surface is replaced with a package-level constant flipped in commit 11. The pattern fixtures (polymorphic body + discriminator union) are front-loaded into commit 1, removing the per-domain ordering risk that drove much of review 02's debate.

One real defect remains, in Section 5: the rejection of the `rawJSONOrString` helper rests on a factually wrong claim about `json.Unmarshal` into `json.RawMessage`. The handler today explicitly falls back to storing non-JSON bytes verbatim, so post-fix `json.RawMessage(payloadBytes)` will produce `json.Marshal` errors (→ 500) on the exact data the current code is designed to tolerate. This needs a design decision before implementation, not discovery during execution.

Otherwise the spec has converged. Other findings below are at-most moderate.

## 2. Showstoppers

### S-1. Section 5's "no fallback helper" reasoning is factually wrong; will silently break `GetMessage` on legacy/binary payloads.

The spec says (Section 5):
> The `NewMessage` handler … decodes the request envelope via `json.Unmarshal` into a `json.RawMessage` field, which guarantees stored `payloadBytes` is valid JSON by construction.

That claim is false on two counts:

1. **`json.Unmarshal` into a `json.RawMessage` field does not validate the bytes.** `RawMessage.UnmarshalJSON` is `*m = append((*m)[0:0], data...)` — a copy, no validation. `json.Unmarshal` only validates that the surrounding envelope is JSON; the `RawMessage` field receives whatever bytes the parser saw at that position. (For object/array values this is necessarily JSON; for a string-shaped position it could be a stringified base64 blob — still JSON-valid as a string token.)
2. **The handler explicitly falls back to non-JSON storage.** `internal/domain/messaging/handler.go:59-66`:
   ```go
   if err := json.Compact(&compacted, envelope.Payload); err != nil {
       // Not valid JSON — store as-is (payload is opaque).
       compacted.Reset()
       compacted.Write(envelope.Payload)
   }
   ```
   The current code path **deliberately stores non-JSON bytes** when compaction fails. The comment ("payload is opaque") and the documented #193 base64-stringify workaround both depend on this tolerance.

Consequence of the proposed fix as written: `json.RawMessage(payloadBytes)` inside the response map will pass through to `json.Marshal`, which calls `RawMessage.MarshalJSON`, which calls `checkValid`. On non-JSON bytes it returns an error, and `common.WriteJSON` emits a 500. Any legacy message stored before the fix — or any message stored via the documented base64-stringify workaround that arrived at the handler with the base64 NOT wrapped in quotes (i.e. the workaround misused) — will start 500-ing on `GetMessage`.

This is the opposite of the design's intent, and the validator will not catch it (a 500 with `ProblemDetail` is still a declared status under the shared 5xx fragment).

Fix options:
- Remove the fallback in `NewMessage` (reject non-JSON at write with 400). Hard break, but consistent with the "do it right or don't bother" charter and the project has no production consumers. Document that #193's base64 path requires a JSON-string wrapper.
- Keep some form of `rawJSONOrString` for `GetMessage` but discriminate it (e.g. `{"_raw": "...", "_encoding": "non-json-legacy"}`) so the wire is unambiguous.
- Validate at write time AND fail fast at read time on legacy bad data with a ticket UUID.

Pick one. The current spec text rejects the helper for a reason that doesn't survive contact with the current code. Either change the reasoning (and accept the storage-corruption risk surfaces as 500) or change the design (reject at write).

This is the only showstopper.

## 3. Strong concerns

Empty.

## 4. Moderate concerns

### M-1. The mode constant gate is good, but commit-1-through-10 atomicity isn't enforced by the constant.

Section 8 acceptance says "PR does not merge unless commit 11 lands and the constant is `ModeEnforce`." That's a reviewer checklist, not a mechanism. If commit 11 is forgotten or reverted, nothing in the build fails — the suite just runs in record mode, and main accumulates silent drift.

A small belt-and-braces guard: a unit test in `internal/e2e/openapivalidator/` that asserts `Mode == ModeEnforce`. The test is `// +build !openapi_record` (or just unconditional, since there's no escape hatch). Then "commit 11 not landed" surfaces as a failing test rather than a checklist miss. Cheap, removes the human-checklist dependency. Worth adding to commit 1's fixture suite (initially failing; flipped green by commit 11's constant change) — that also makes commit 11 a real green-the-test commit, not a free-floating constant edit.

### M-2. `go test -shuffle=on` will break the filename-ordering invariant.

The spec correctly identifies that filename order (not function name) controls test execution sequence. It does not address `-shuffle=on`, which the prior review flagged. With shuffle, `TestOpenAPIConformanceReport` can run before any other test, see an empty collector, and pass falsely. CI doesn't shuffle by default, but a developer running `go test -shuffle=on ./internal/e2e/...` (a reasonable thing to do when chasing a test-order dependency) gets a green that means nothing.

Fix: at the top of `TestOpenAPIConformanceReport`, check `flag.Lookup("test.shuffle").Value.String()` and `t.Skip` (or `t.Fatal` with a clear message) if shuffle is on. One line. Documents the invariant in code where it can't drift away from the design.

### M-3. The middleware's `t.Errorf`-from-request-goroutine path needs a small caveat about test lifecycle.

`t.Errorf` is safe to call from any goroutine while the test is running — it acquires a mutex on `T.mu`. **It is not safe after the test returns**, because the `*testing.T` has been released and `Errorf` will panic on a nil log writer. In an HTTP middleware that fires after `httptest.Server` finishes a request, the test goroutine that issued the request is still blocked on the response, so the test is alive — that's the common case and the spec is fine there.

Edge case: if the test issues a request via a goroutine it doesn't `Wait` on (e.g. fire-and-forget background HTTP call in a test that completes before the response returns), the validator's `t.Errorf` could fire after the test returned, panicking. Unlikely in this codebase but worth a one-line guard: `defer func() { _ = recover() }()` around the `t.Errorf` call, or skip per-request enforcement if the test-name context value is missing.

The probability is low; the cost of guarding is one line. Recommend the guard.

### M-4. `kin-openapi`'s router and 81-op spec — confirm the once-per-suite build is safe.

Section 2 says the router is "built once in `TestMain` from the embedded spec." `kin-openapi`'s `gorillamux.NewRouter(doc)` is the standard path; verify `genapi.GetSwagger()` returns a `*openapi3.T` that the router accepts as-is (i.e. no `LoadFromData` round-trip needed). Both are likely fine, but if `GetSwagger()` returns the kin-openapi typed object directly, the router builds; if it returns YAML bytes, there's a parse step. One-line concern that the spec should pin in the implementation note. Not architecturally significant.

## 5. Nitpicks

- Section 2's mode table is helpful. Add one row for "the validator panics on a router-mismatch internal bug" — the captured `*testing.T` should not propagate validator panics into test failures unrelated to the user's response shape. Wrap the validator call in a `defer recover()` and log the panic to the collector as a `Mismatch` with `Reason: "validator panic: ..."`. Otherwise a single bad spec entry takes down the whole suite.
- Section 8 commit 11's "verify ADR 0001 is unchanged" is good discipline; if it surfaces drift mid-merge, the fix is presumably an ADR-0002 superseding. State that explicitly so the implementing agent doesn't try to silently amend ADR 0001.
- Section 2 ("Streaming responses") still says "future work could add a custom ndjson validator" — fine to defer, but with `IncludeResponseStatus: true` the streaming endpoints' status codes still validate, which means the spec must declare every status they emit. Worth a sentence in the audit-table notes: "ndjson endpoints — declare 200, 400, 401, 403, 500 even though body shape isn't checked."
- Section 9's "Validator runtime cost ... tens of microseconds" is unchanged from review 01. With `MultiError: true` and 50-field schemas it's closer to single-digit milliseconds. Not impactful (E2E suite is dominated by container startup) but the number is wrong.

## 6. What's done well

- File-vs-function ordering correctly diagnosed. `zzz_openapi_conformance_test.go` does sort after `workflow_test.go` (the alphabetically-last existing test file in `internal/e2e/`). Verified directory listing.
- Per-request `t.Errorf` for `-run`-filtered enforce mode is the right architecture. It also makes the test-name context plumbing load-bearing rather than nice-to-have, which the spec acknowledges and bundles into commit 1.
- Tee-writer optional-interface enumeration is now complete and justified. `Pusher`/`CloseNotifier` are correctly identified as safe to drop.
- Mode constant in code (vs env var) is the right control surface. The reasoning (no silent CI/local divergence; no flaky-validator silencing on main) is sharp.
- Pattern fixtures in commit 1 (polymorphic body, discriminator union, plus the four named #21 defects) remove the per-domain-ordering debate from review 01/02 entirely. Order is now a stylistic choice, as it should be.
- Tie-breaker rule in Section 3 (router-mismatch → spec is the bug → fix path templates) is the right escalation order and explicitly addresses review 02's C-4.
- `record` → `enforce` mode flip lands the validator before defects are fixed without spamming CI. Good answer to review 01's C-1.
- ADR 0001 anchoring throughout. No decision drift.

## 7. Bottom line — approve or revise?

**Revise — one showstopper to resolve.** Section 5's `rawJSONOrString` rejection rests on a wrong claim about `json.Unmarshal`/`RawMessage` validation, and the current handler at `internal/domain/messaging/handler.go:59-66` explicitly stores non-JSON bytes as-is. The proposed fix will turn legacy non-JSON storage into 500s on `GetMessage`. Pick a deliberate path: reject at write (clean break, no consumers to break) or keep a discriminating fallback envelope. State the choice and update the rationale.

Everything else is at most moderate (atomicity-via-test, shuffle guard, panic-safe `t.Errorf`). The architecture is sound; revision 3 has otherwise converged.
