# Design Review — Issue #21 OpenAPI Server-Spec Conformance (Revision 2)

**Date:** 2026-04-29
**Reviewing:** `docs/superpowers/specs/2026-04-29-issue-21-openapi-conformance-design.md` (revised)
**Reviewer:** Independent agent acting as senior systems architect + Go expert; context-free
**Branch:** `issue-21-openapi-conformance`

## 1. Overall assessment

The revision picks up the prior review's substantive concerns cleanly. `Options.IncludeResponseStatus: true` and `Options.MultiError: true` are now explicit (Section 2). The ndjson hole is resolved by skipping body validation for that content-type (Section 2 / Section 8 commit 6). The `os.Exit(1)`-from-`TestMain` problem is replaced with a `TestZZZ_OpenAPIConformance` end-of-suite test that preserves `t.Cleanup`. A `record` → `enforce` mode flip lets the validator land before the per-domain fix-ups without spamming CI. The audit-skeleton tooling has been dropped in favour of LLM-paced direct reading, paired with a record-mode cross-check.

These are the right moves. The spec is now executable. But several mechanical claims still don't hold up against the underlying primitives — most importantly the "ZZZ sorts last alphabetically" assumption, the tee-writer's optional-interface coverage, and the fragility of `Content-Type`-based ndjson detection. None are architecturally fatal; all need to be specified, not discovered during execution.

I verified `Options.IncludeResponseStatus` and `Options.MultiError` exist with the claimed semantics in `~/go/pkg/mod/github.com/getkin/kin-openapi@v0.137.0/openapi3filter/options.go:24,26` and that `validate_response.go:54` is exactly the silent-pass branch the spec is guarding against. That part is solid.

## 2. Showstoppers

### S-1. "ZZZ sorts last alphabetically" is wrong about how Go orders tests.

Section 2 ("Collector + report") and Section 2 ("Test-name capture") rest on the assumption that naming the test `TestZZZ_OpenAPIConformance` makes it run last. **That is not how `go test` orders tests.** Per the `testing` package and `cmd/go/internal/test` source, tests are discovered and executed in **source declaration order within each file, with files processed in alphabetical filename order** (per `go/build`'s `Package.GoFiles`). Function name has no effect on ordering.

The practical consequence:
- If `TestZZZ_OpenAPIConformance` lives in `e2e_test.go` (which already has `TestHealth` declared at line 135), it runs at whatever source position it occupies in that file — almost certainly *not* last across the package.
- If it lives in its own file, the file's name (not the test name) determines ordering. Naming the file `zzz_conformance_test.go` works; naming the function `TestZZZ_*` does not.

Fix: drop the `ZZZ` from the function name and instead require the conformance test to live in a file whose filename sorts last among `_test.go` files in `internal/e2e/` — e.g. `zzz_openapi_conformance_test.go`. Document the filename-ordering invariant explicitly so a future contributor doesn't move the function back into a sibling file. Better still, have the conformance test register a `t.Cleanup` on every other test through a helper, but that requires touching every test file — the filename hack is the bounded solution. Either way the design needs to stop claiming the function name does the work.

(Note: even with correct filename ordering, `go test -shuffle=on` will reorder. If anyone in the project uses shuffle, the conformance test breaks. Worth a one-line guard: `if testing.Short() { return }` plus a check that `flag.Lookup("test.shuffle").Value.String() == "off"`, or accept the limitation.)

### S-2. `-run` filtering does not include `TestZZZ_OpenAPIConformance` automatically.

Section 2 says: "`go test -run TestEntity_Create` … is preserved" — implying the conformance test still runs. It does not. `-run` is a regex match against test names; `TestEntity_Create` matches only that one test. `TestZZZ_OpenAPIConformance` is filtered out entirely, so the validator collects mismatches but the report-fail step never executes, and the developer sees green. That is **worse than the `os.Exit(1)`-from-`TestMain` pattern** the revision replaced — it silently drops the guard during the most common iteration workflow.

Fix options:
1. Document explicitly that single-test runs do not enforce the validator; the developer is expected to run the full suite before pushing. Make the README and `CONTRIBUTING.md` say so.
2. Have the validator middleware itself call `t.Errorf` (via the captured test context) at request time when a mismatch is detected, so the failing test is the one that issued the bad response. Mismatches still show up under `-run`, attributed to the test that triggered them. The end-of-suite report then aggregates; it doesn't need to be the sole signal.

Option 2 is the right architecture. It also fixes the "test-name capture" plumbing — instead of stashing names in a context for later attribution, you have the actual `*testing.T` and can fail it directly. The end-of-suite test then only handles "uncovered ops" which is genuinely a full-suite-only concern.

## 3. Strong concerns

### C-1. Tee-writer optional-interface coverage is incomplete.

Section 2 promises `http.Flusher` and `http.Hijacker` delegation. For Go 1.26 / `net/http`, the optional interfaces a `ResponseWriter` may implement are: `http.Flusher`, `http.Hijacker`, `http.Pusher` (HTTP/2 server push), `http.CloseNotifier` (deprecated but still honoured by some libraries), and — most importantly here — the `io.ReaderFrom` shortcut used by `http.ServeContent` and `io.Copy` to avoid double-buffering. Missing `ReaderFrom` means any handler that streams a large file via `io.Copy(w, file)` will fall back to the slow path through your tee buffer; for ndjson and any future binary-export endpoint that's a real perf cliff at test time.

Fix: enumerate every optional interface and either delegate or document why it's safe to drop. `CloseNotifier` is fine to drop (deprecated). `Pusher` is fine to drop (we don't push). `ReaderFrom` should be delegated. `Flusher` and `Hijacker` should be delegated as the spec already says. Add a small unit test that constructs the tee with a fake `ResponseWriter` implementing each interface and asserts the tee implements them too — `kin-openapi` is not the right place to discover this gap; it'll surface as a flaky streaming test.

### C-2. `Content-Type`-based ndjson detection is fragile.

Section 2's skip rule keys off the response `Content-Type` header. Two failure modes:
1. A handler that writes to the body before calling `w.Header().Set("Content-Type", ...)` will have the type auto-sniffed by `net/http` (per `http.DetectContentType`), which never returns `application/x-ndjson`. The validator sees `text/plain` or `application/octet-stream` and tries to validate the body against the spec's ndjson schema → spurious mismatch.
2. A future SSE handler will set `text/event-stream`. The skip rule won't fire; the validator will try to parse the event stream as JSON → spurious mismatch.

Fix: skip on the **operation's declared content-type** (read from the matched route's response schema), not the response header. The router gave you the operation; the operation knows it produces `application/x-ndjson`. That's authoritative and immune to header-set ordering. As a belt-and-braces, also skip on `text/event-stream` from either source.

### C-3. Mode flip via `CYODA_OPENAPI_VALIDATOR_MODE` env var has two foot-guns.

Section 2 ("Enforcement gate") and Section 8 commit 11 say "the flip is the gate" but don't pin down *what mechanism* changes the default.

- **Foot-gun A: developer / CI divergence.** If the default is a hard-coded constant in the validator package and the env var is the override, then commit 11 changes the constant. Fine. But if instead the env var has no default and CI sets `CYODA_OPENAPI_VALIDATOR_MODE=enforce` while local dev doesn't, a developer's local pass != CI's. The spec needs to say which.
- **Foot-gun B: silent silencing.** Anyone can `export CYODA_OPENAPI_VALIDATOR_MODE=record` to silence a flaky validator on `main`. There's no audit trail. For a load-bearing guard, that's too easy.

Fix: hard-code the mode constant in the validator package after commit 11, and remove the env var. If you need a single-test-run escape hatch, add it as a build tag (`//go:build openapi_record`) so it's visible in `git grep` and CI can refuse to build it. Env-var configuration for a CI gate is the wrong control surface.

### C-4. Audit table cross-check rule is underspecified for disagreement.

Section 3 ("Process" step 5) says: "any disagreement flags a human-error in the audit, surfacing it before per-domain commits start. … If it surfaces noise, fix the audit table row."

That assumes the audit table is wrong. But the validator can also be wrong — if the `kin-openapi` router matches the wrong operation (e.g. a path-template ambiguity, or a method the spec doesn't declare), the mismatch is in the validator's side of the comparison. The spec needs a tie-breaker rule:

1. Read the actual handler code.
2. Read the spec block.
3. If they agree, the validator's report is wrong → file a `kin-openapi` repro and skip the operation in the validator.
4. If they disagree, the audit table is wrong → fix it.

Without rule 3, the implementing agent will silently mis-edit the audit table to match a mis-routed validator finding.

### C-5. Commit ordering — messaging-first risks blocking the PR on a load-bearing pattern flaw.

Section 8's rationale is "hardest patterns first". I sympathise with the philosophy but the practical risk asymmetry isn't acknowledged: if commit 3 (messaging) surfaces a structural defect — e.g. `EdgeMessagePayload` polymorphism doesn't validate cleanly with `MultiError`, or the discriminator approach for `AuditEvent` requires a spec restructure — then commits 4-11 are stalled while we redesign. Easy-first inverts that: stalls happen on small isolated domains, not on the load-bearing pattern.

Counterpoint: the prior review *also* recommended messaging-first, on the grounds that easy-first locks in a pattern that may not generalize. Both arguments are real. The right resolution is to **front-load the pattern probes as unit-test fixtures, not E2E commits**: before commit 3 lands, the validator package's unit tests exercise polymorphic-payload validation and discriminator-union validation against synthetic schemas. That proves the patterns work in isolation. Then commits 3-10 can be in any order — easy-first or hard-first is a stylistic choice, not a load-bearing one.

Suggest: add a step to commit 1 ("validator + collector") — fixture tests for (a) `additionalProperties: true` polymorphic field, (b) `oneOf` + `discriminator` union, (c) `application/x-ndjson` skip, (d) undeclared status code surfacing. Then re-evaluate ordering.

### C-6. Audit (commit 4) is harder than messaging (commit 3); the rationale has the order backwards.

Section 8 puts audit second on the assumption it's the second-hardest pattern. I disagree. `additionalProperties: true` (messaging) is the most permissive validation — the validator accepts anything, so messaging is in some sense the *easiest* to make pass. `oneOf + discriminator` (audit) requires every variant's schema to round-trip cleanly and the `propertyName` to match exactly; `kin-openapi`'s discriminator support has historically had rough edges (see `kin-openapi/openapi3filter/validation_discriminator_test.go` for the test surface). Audit should come first if the goal is "discover the hardest pattern early".

But — see C-5 — the right answer is to remove this question from the commit ordering by validating both patterns as unit-test fixtures in commit 1.

### C-7. Sequencing risk if commit 11 stalls.

Asked in the prompt: what if commit 10 lands but commit 11 fails review? Main has all the per-domain spec/handler fixes but the validator is in `record` mode. Drift can creep in silently — a new handler change wouldn't fail the suite.

Two mitigations the spec should pick between:
1. **Don't merge commits 1-10 standalone.** The PR is atomic; either all 11 commits squash-merge or none do. (Project convention is squash-merge per `feedback_release_branch_for_milestones.md`, so this is the natural posture anyway. Make it explicit in the design.)
2. **Move the mode flip earlier.** If commits 3-10 end with the validator showing zero mismatches for that domain, flip enforce *for that domain only* (a per-tag allowlist). Commit 11 becomes "remove the allowlist, all domains enforced". This requires a tag-aware mode setting but de-risks the final commit.

Option 1 is cheaper and aligned with how the project already merges. Option 2 is more incremental but adds machinery. Either way, the spec should commit explicitly — "if commit 11 doesn't merge, commits 1-10 don't either" is a reasonable rule, but it must be written down.

## 4. Moderate concerns

### M-1. `rawJSONOrString` ambiguity.

Section 5's helper falls back to `string(b)` when `json.Valid(b)` is false. The polymorphic `EdgeMessagePayload` schema accepts both, so the wire validates. But the wire is now ambiguous: a client receiving `"content": "<xml>...</xml>"` cannot tell whether the producer stored XML or stored a JSON string that happened to begin with `<`. If #193 ever lands proper content-type handling, this ambiguity becomes a migration problem ("how do we distinguish legacy stringified-binary from legacy stringified-JSON?").

Fix options:
1. Wrap the fallback in a discriminating envelope: `{"_raw": "<base64>", "_encoding": "binary-legacy"}`. Ugly, but unambiguous and documented as a #193-bridge shape.
2. Reject non-JSON at write time *now* and let GetMessage panic-recover on legacy data with a 5xx + ticket. Hard break, but #193 is on the roadmap and the project has no production consumers.

The current proposal (silent stringify) is the *least* good option — it preserves exactly the JSON-in-string defect this PR is supposed to eliminate, just for a smaller class of payloads. Pick option 2 unless you have stored data you can't migrate.

`json.Valid(b)` cost on large payloads is a non-issue (it's a single-pass scan, ~GB/s); ignore that aspect.

### M-2. Test-name capture optional-migration plan creates a coverage cliff.

Section 2 ("Test-name capture") says existing tests can opt in to `e2e.NewRequest(t, ...)` and the validator works without it (test name shows as "unknown"). At ~50+ existing E2E test files, "unknown" will dominate the report for months. The end-of-suite mismatch list becomes hard to triage. Either:
- Migrate all callers in commit 1 (it's a mechanical sed-able change), or
- Use the recommendation from S-2 (have the middleware fail the test directly via the captured `*testing.T`) — then attribution is automatic.

Either resolves the cliff. The "organic migration" plan does not.

### M-3. Audit table population without tooling — feasible but boring.

81 ops × ~4 columns of human-read-and-paraphrase. At LLM pace this is ~30-60 minutes of focused work. That's not the risk; the risk is **transcription accuracy**. The cross-check against record-mode validator output is the right backstop, but only catches "server response" column errors — not "spec response" column errors. A misread spec block silently disagrees with reality.

Mitigation is cheap: have the audit table generate by reading both sides programmatically (parse `api/openapi.yaml` for the spec column; parse handler functions for `WriteJSON(w, X, status)` patterns for the server column) — even a `gopls`-driven extraction would be more reliable than read-and-paraphrase. The revision dropped this, citing "minutes not hours at LLM pace". I'd push back: human/LLM reading is *fast* but *imprecise*. For an artefact that "lives past the PR" and seeds a future Cloud-reconciliation milestone, the cost of even a small parser is amortised across both efforts.

Not a showstopper. But "drop the tooling and read manually" is a regression on quality for a marginal time win.

### M-4. Streaming endpoints' status-code coverage isn't actually free.

Section 2 ("Streaming responses") says ndjson endpoints get "status only" coverage. That's true, but with `IncludeResponseStatus: true`, the spec must declare every status these endpoints can emit (200, 4xx, 5xx) or the validator will fire on missing-status. For the streaming variant of `getAllEntities`, a 401/403/500 mid-stream is hard to test cleanly (the headers may already be flushed before the error occurs — the streaming handler has to emit an error record in the stream rather than a status code). The spec needs to either declare what those endpoints actually do on error (which is a separate design question) or document the constraint.

## 5. Nitpicks

- Section 2 ("Collector + report") shows `var collector struct{...}` as a package-level singleton. With `t.Parallel()` E2E tests this is fine because the mutex guards it, but the `TestName` field captured at request time is unreliable when the same test issues multiple parallel requests via goroutines. Not common in this codebase, but worth a note.
- Section 8 commit 1's "small unit test feeding a known-mismatching response" should be plural — at minimum the four #21 fixtures from ADR 0001 step 2, not "a small unit test".
- Section 9's risk row "Validator runtime cost ... tens of microseconds per response" still understates. With `MultiError: true` and a 50-field response object, single-digit milliseconds is more honest. Doesn't change the conclusion (negligible vs E2E wall-clock), but the prior review already flagged this and the number didn't change.
- Section 8 commit 11 includes "verify ADR 0001 is unchanged (no decision drift during execution)" — good discipline, but if ADR 0001 *did* drift, where does the corrective action live? The commit subject implies a no-op verification; if it surfaces drift the PR is mid-merge. Document the rollback path.
- `TestZZZ_OpenAPIConformance` writes a markdown report to `internal/e2e/_openapi-conformance-report.md` (gitignored). Fine, but the path is buried in package-internal test data — nobody runs CI then opens that file. CI should print the report (or a link to its uploaded artefact) to stdout on failure.

## 6. What's done well

- The `record` → `enforce` mode design is the right answer to "validator landing before defects are fixed". Cleanly resolves the noise-floor problem from review 01 C-1.
- Explicit commitment to `Options.IncludeResponseStatus: true` and `Options.MultiError: true`, with a fixture pinning test that asserts the constructor sets them. Verified against `kin-openapi@v0.137.0/openapi3filter/options.go:24,26`.
- Tee-writer pattern (vs `httptest.ResponseRecorder`) — correctly diagnosed the problem with the original proxy, even if optional-interface enumeration is incomplete (see C-1).
- Dropping `os.Exit(1)` from `TestMain` in favour of a normal test that preserves `t.Cleanup`. Right direction even if the implementation has bugs (see S-1, S-2).
- ADR 0001 reference is anchored throughout. No decision drift between ADR and design.
- Risk register continues to name cross-repo concerns (Cassandra plugin, cyoda-docs) and adopts the four-fixture pinning gate seriously.
- Documenting the `EdgeMessagePayload` polymorphism limit and naming #193 explicitly is honest about what this PR does and doesn't fix.

## 7. Bottom line

The architecture is right; the revision addressed every load-bearing concern from review 01. Two showstoppers remain — both about Go test mechanics rather than OpenAPI: `TestZZZ_*` doesn't sort last by function name (file order, not function name, controls Go test ordering) and `-run` filtering silently disables the conformance gate. Both are cheap fixes (rename a file; have the middleware call `t.Errorf` directly). Strong concerns C-1 (tee-writer optional interfaces), C-2 (ndjson detection via response header is fragile), and C-3 (env-var control of an enforcement gate is the wrong surface) are all design-level and resolvable in the spec without re-architecting.

Approve direction; revise spec in three places before plan-writing:
1. Section 2: filename-based ordering for the conformance test, plus middleware-driven `t.Errorf` so `-run` doesn't drop the gate.
2. Section 2: enumerate optional `ResponseWriter` interfaces (delegate `Flusher`, `Hijacker`, `ReaderFrom`; document drop of `Pusher`/`CloseNotifier`); switch ndjson skip to the matched operation's declared content-type.
3. Section 5: pick a deterministic resolution for non-JSON legacy payloads — either the `_raw`/`_encoding` envelope or a hard reject with #193-bridge documentation.

Pre-merge atomicity (commits 1-10 don't merge without 11) should be stated explicitly; otherwise the transitional record-mode-on-main state in C-7 is a real risk.
