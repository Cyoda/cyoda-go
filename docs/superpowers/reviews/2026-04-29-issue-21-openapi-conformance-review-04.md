# Design Review — Issue #21 OpenAPI Server-Spec Conformance (Revision 4)

**Date:** 2026-04-29
**Reviewing:** revised spec (`docs/superpowers/specs/2026-04-29-issue-21-openapi-conformance-design.md`)
**Reviewer:** Independent agent acting as senior systems architect + Go expert; context-free

## 1. Overall assessment

Revision 4 has converged. The four checkpoints requested for verification all hold up against the primitives:

- `json.Unmarshal` into a `json.RawMessage` field DOES validate: feeding `{"payload": <xml/>}` to `json.Unmarshal(&envelope)` returns `invalid character '<'`. So the dead-branch claim is correct — once `Unmarshal` succeeded at line 49 of `internal/domain/messaging/handler.go`, `envelope.Payload` is guaranteed to be a syntactically valid JSON value, and `json.Compact` cannot subsequently fail on it (verified empirically across string/object/array/large-number cases).
- `flag.Lookup("test.shuffle")` exists in Go 1.26.2's `testing` package (declared at `src/testing/testing.go:480`) and `Value.String()` returns `"off"` by default. The shuffle guard is well-formed.
- `t.Errorf` after the test has returned panics via `c.log` at `src/testing/testing.go:1039` (`"Log in goroutine after <name> has completed: ..."`). `defer recover()` is the correct safety net for the fire-and-forget edge case the spec calls out.
- The mode-pinning unit test plus filename ordering plus `-shuffle` guard plus per-request `t.Errorf` collectively form a defensible drift-detection envelope.

The architecture is sound, the risks are honestly named, and prior reviews' substantive concerns have been resolved without over-correction. Recommend approval.

## 2. Showstoppers

Empty — design has converged.

## 3. Strong concerns

Empty — design has converged.

## 4. Moderate concerns

### M-1. The "invariant broken" 500 in Section 5 is unreachable theatre — but acceptable theatre.

I verified empirically: any `[]byte` that survives `json.Unmarshal` into a `json.RawMessage` field is syntactically valid JSON, and `json.Compact` never fails on syntactically valid JSON. The proposed replacement —

```go
if err := json.Compact(&compacted, envelope.Payload); err != nil {
    common.WriteError(w, r, common.Internal("payload validation invariant broken", err))
    return
}
```

— is therefore as dead as the branch it replaces. The `if err != nil` arm cannot fire in any execution that reaches it.

Two ways to handle this honestly:

- **Keep the invariant assertion** (current spec) — value: documents the invariant in code, catches regressions if a future refactor extracts the parsing into a path that doesn't validate. The `common.Internal` route is correct; it logs server-side and returns a sanitised 5xx per CLAUDE.md. Cost: one unreachable branch readers will puzzle over.
- **Drop the check entirely** — the bytes are trusted, write `compacted.String()` directly. Smaller diff, less code to read, but loses the documentation effect.

Either is defensible. I'd lean toward the assertion (current spec) precisely because the hand-rolled comment explaining *why* it's unreachable is more informative than the absence of code. Add one sentence to Section 5: "this branch is intentionally unreachable in normal operation; it documents the invariant and surfaces a server bug if upstream parsing is ever refactored to skip validation." That converts theatre into intentional documentation.

This is a phrasing nit on the spec, not a blocker.

## 5. Nitpicks

- Section 2's "fire-and-forget" rationale for the `defer recover()` around `t.Errorf` is correct but could name the panic message verbatim (`"Log in goroutine after <name> has completed"`) — locks in what we're guarding against and helps future readers tracing a recovered panic.
- Section 2 mode table omits one combination worth a row: `-shuffle=on` + `enforce`. The shuffle guard at the top of `TestOpenAPIConformanceReport` causes a `t.Fatalf`, but only fires when the conformance test actually runs (which it may not under `-run` filtering). Under `-shuffle=on -run TestEntity_Create`, no conformance test runs, no shuffle guard fires, and the per-request `t.Errorf` path is the only signal — which is fine, but worth one explicit table row so the matrix is truly complete.
- Section 8 commit 11 says `TestModeIsEnforce` lands in commit 11. Consider landing the test in commit 1 with a `t.Skip("flips to fail in commit 11")` so the test file already exists and reviewers see the gate's intent up front. Optional.
- Section 5's "External-write risk" paragraph is the right discipline (honest 500 with ticket UUID rather than silent shape-shift). No change requested.
- The `TestModeIsEnforce` body has a minor formatting bug — `t.Fatalf("...")` with `%v` placeholder but no varargs:
  ```go
  t.Fatalf("Mode = %v; expected ModeEnforce on main. Re-flipping to ModeRecord requires explicit PR review.")
  ```
  Will compile but the `%v` will render as `%!v(MISSING)`. Pass `Mode` as the arg (or use `t.Fatal` with a static string). Trivial; flag for the implementing agent.

## 6. What's done well

- All four review-03 verification questions hold up against the primitives. Empirical test of `json.Unmarshal` + `json.RawMessage` validates the dead-branch claim that prior reviews disputed.
- `flag.Lookup("test.shuffle")` is verified present in Go 1.26.2 with the right default. The one-line guard is exactly right.
- `defer recover()` around `t.Errorf` correctly addresses the `c.log` panic at `src/testing/testing.go:1039`.
- `TestModeIsEnforce` makes commit-11 enforcement mechanical instead of checklist-dependent. Resolves review-03 M-1 directly.
- Tie-breaker rule (Section 3) handles the router-mismatch case explicitly with a Gate-6 escalation. Resolves review-02 C-4.
- Pattern fixtures (polymorphic body, discriminator union, ndjson skip, undeclared status) front-loaded into commit 1 — pattern risk is decoupled from per-domain ordering. Resolves review-01 C-1 / review-02 C-5/C-6.
- Spec-declared content-type (not response header) drives ndjson skip — immune to `http.DetectContentType` ordering quirks. Resolves review-02 C-2.
- The `messaging.GetMessage` fix's "external-write risk" paragraph is the right honesty: contract is "stored bytes are valid JSON," violation produces an honest 500, no silent shape-shift. Resolves review-03 S-1.

## 7. Bottom line — approve or revise?

Approve, no further changes required. The remaining items are phrasing nits and a one-character `Fatalf` arg the implementing agent will catch. The design has reached convergence — proceed to plan-writing.
