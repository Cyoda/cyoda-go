# SPI release-day hygiene — design

**Status:** approved
**Date:** 2026-05-05
**Scope:** `cyoda-go-spi` and `cyoda-go`. Sibling: `cyoda-go-cassandra` (notification only).
**Outcome:** symmetric `v0.7.1` release in both repos; closes the SPI repo's CI/governance gap and the cyoda-go pin-drift gap in one workstream.

---

## 1. Background

Two findings, treated as one workstream because they share a release-day cause and a single coherent fix:

1. **`cyoda-go-spi` has no governance infrastructure.** No `.github/` directory (no CI, no Dependabot, no CodeQL), no `CHANGELOG.md`, no `MAINTAINING.md`, no deprecation policy, no consumer registry. Tags `v0.1.0` through `v0.7.0` are lightweight (commit-pointer), not annotated, not signed. `git verify-tag v0.7.0` fails.
2. **SPI version drift across modules in cyoda-go.** Root `go.mod` pins `cyoda-go-spi v0.7.0`; `plugins/{memory,postgres,sqlite}/go.mod` pin `v0.6.1`. The discrepancy is masked at link time by Go's MVS resolution (`max(v0.6.1, v0.7.0) = v0.7.0`), so the binary is correct but the per-plugin manifest is dishonest. `cyoda-go-cassandra` (sibling repo) pins `v0.6.0`.

These findings are connected. Both stem from the absence of release-day discipline: no CI to fail when manifests drift, no procedural hook that bumps consumers when SPI tags. Solving one without the other leaves the other to drift again.

A separate documentation gap surfaced during design: the `cyoda-go` plugin submodule tags (`plugins/memory/v0.1.0`, `plugins/postgres/v0.1.0`) lag the umbrella by five minor versions; `plugins/sqlite` was never tagged. The existing `MAINTAINING.md` rule says plugin tags track the umbrella version — that rule is correct for this codebase shape (plugins are coupled, ship together, have no independent consumers) but has not been enforced. This workstream closes that gap as part of the same release.

## 2. Decisions

| Decision | Choice | Rationale |
|---|---|---|
| SPI repo CI scope | Self-contained: `go test`, `go vet`, `go build`, `golangci-lint`. No cross-repo conformance in SPI's own CI. | Cross-repo conformance pushes integration cost back onto consumers, where it belongs. SPI's CI proves the SPI module is internally sound. |
| Sequencing | Symmetric `v0.7.1` in both repos (revised from initial "infrastructure-only, no SPI tag"). | The fix-forward statement is dramatically stronger when anchored to the first annotated+signed tag. Symmetric `v0.7.1` makes the regime change literal at a specific commit. |
| Drift-gate scope (cyoda-go) + third-party affordance | Internal drift gate in cyoda-go (single repo), plus reusable conformance workflow template in SPI for third parties to drop into their own CI, plus opt-in `KNOWN_CONSUMERS.md` registry. | The drift gate is internal hygiene. Third-party authors need a way to test their own backend, a CHANGELOG to read, and a place to be heard — not a centralised dashboard tracking them. |
| Doc layout | `MAINTAINING.md` (release process + deprecation policy + fix-forward), `CHANGELOG.md` separate, README pointer. Use `MAINTAINING.md` not `RELEASING.md` — matches existing cyoda-go convention, keeps sibling repos in lockstep on file naming. | Standard OSS layout under cyoda-platform's existing convention. |
| Deprecation policy | Pre-1.0: minors additive-only by default; breaking minors permitted iff (a) called out in CHANGELOG under "Breaking" with migration notes, (b) deprecated symbols carry `// Deprecated:` for at least one minor when feasible, (c) consumers in `KNOWN_CONSUMERS.md` are notified before merge. Post-1.0: standard semver, N-1 minor deprecation window. | Honest about pre-1.0 reality (we already broke v0.6→v0.7 informally) without surrendering all consumer protection. |
| Plugin-version policy | Lockstep with umbrella (preserve existing `MAINTAINING.md` rule, fix enforcement). Plugins evolve, ship, and tag with the umbrella; no independent cadence. | Plugins are tightly coupled to umbrella, ship together, have no independent consumers (unlike e.g. prometheus/common). Lockstep matches the codebase shape; idiomatic Go for this coupling profile (k8s, etcd, opentelemetry-go follow the same pattern). |
| Tag history | Immutable. v0.1.0–v0.7.0 stay as lightweight tags; v0.7.1+ annotated+signed. Plugin tags `plugins/{memory,postgres}/v0.1.0` stay as legacy artifacts. | sum.golang.org caches per-tag SHA; force-moving breaks consumer checksums. Forward-only is the only safe option. |

## 3. Deliverables

### 3.1 `cyoda-go-spi`

**CI & supply-chain (`.github/`)**

- `.github/workflows/ci.yml`: `go vet ./...`, `go test -race ./...`, `go build ./...`, `golangci-lint run`. Triggers on `push` to `main` and on `pull_request`. Go matrix: latest 1.26.x patch.
- `.github/workflows/codeql.yml`: weekly schedule + on PR. Standard GitHub-managed CodeQL Go config.
- `.github/dependabot.yml`: `gomod` daily, `github-actions` daily.
- `.github/PULL_REQUEST_TEMPLATE.md`: short checklist; key item — *"if this PR changes a public symbol, confirm CHANGELOG.md is updated and each entry in KNOWN_CONSUMERS.md has been notified."*

**Release governance (root)**

- `MAINTAINING.md`: three sections.
  1. *Release process.* No release branch (small repo, direct to main). Tag procedure: `git tag -s -a vX.Y.Z` (annotated and signed); push tag. CHANGELOG is the source of truth — no separate GitHub release ceremony required.
  2. *Deprecation policy.* B3 verbatim (above).
  3. *Fixing forward.* Single paragraph: *"Tags v0.1.0 through v0.7.0 are lightweight (commit-pointer) tags by design — they are immutable per Go module checksum stability and we do not retroactively modify them. Beginning with v0.7.1, all tags are annotated and signed. The new regime is forward-only: existing history stays as-is, new releases follow the new rules."*
- `CHANGELOG.md`: Keep-a-Changelog format. Initial state has `[Unreleased]` collecting the hygiene additions; on `v0.7.1` tag, those entries become the v0.7.1 row. Header preamble links to `MAINTAINING.md`'s deprecation policy and fix-forward note.
- `KNOWN_CONSUMERS.md`: opt-in registry. Initial entries: `cyoda-platform/cyoda-go` (in-tree plugins), `cyoda-platform/cyoda-go-cassandra`. PR template for adding new entries: one line — *"add my-org/my-plugin, claims compliance with vX.Y.Z, contact: @handle."*

**Third-party on-ramp**

- `spitest/README.md` (extend if exists, create otherwise): "Writing a Cyoda storage plugin" section. Covers importing `spitest`, running conformance against a backend, plus a copy-pasteable GitHub Actions snippet (~30 lines of YAML) doing nightly drift-detection against latest SPI HEAD via `replace`. The SPI repo has no in-tree backend to validate the snippet against (spitest is a conformance harness, not a reference implementation). Validation is out-of-tree: copy the snippet once into a scratch checkout of `cyoda-go` with a local `replace` directive pointing at the worktree's SPI, run `cyoda-go/plugins/memory` tests through it, confirm green. SPI's own CI does **not** run the snippet — it stays documentation only, consistent with the Q1 decision to keep SPI CI self-contained.

**README pointer**

- `README.md`: add a "Versioning & Compatibility" section (3 lines) linking to `CHANGELOG.md`, `MAINTAINING.md`, `KNOWN_CONSUMERS.md`.

### 3.2 `cyoda-go`

**Pin sync**

- `plugins/{memory,postgres,sqlite}/go.mod`: bump `cyoda-go-spi v0.6.1` → `cyoda-go-spi v0.7.1` (consumes the SPI v0.7.1 tag from Phase 2 — clean signal we're under the new regime). `go mod tidy` per submodule.
- `go.mod` (root): bump `cyoda-go-spi v0.7.0` → `v0.7.1`. `go mod tidy`.

**Drift gate**

- `scripts/check-spi-pin-sync.sh` (or inline `Makefile` target `check-spi-pin-sync`): greps `cyoda-go-spi v` across `go.mod` and `plugins/*/go.mod`; fails non-zero with a readable message if more than one distinct version appears.
- Wired into CI (`.github/workflows/`) as a job triggered on `push`/`pull_request`. ~15 lines of YAML.
- One-line note in `CONTRIBUTING.md` under dependency-update guidance: *"When bumping `cyoda-go-spi` in the root `go.mod`, bump it identically in every `plugins/*/go.mod` in the same PR. The `check-spi-pin-sync` CI gate enforces this."*

**Doc updates**

- `MAINTAINING.md`: add SPI lockstep-bump rule (must-bump-plugins-in-same-PR). Add reference to drift gate. Re-affirm plugin-version lockstep with umbrella (preserve existing rule; do not amend).
- `COMPATIBILITY.md`: update with the v0.7.1 row (per existing convention; cross-repo matrix update is mandatory on every cyoda-go binary release).

### 3.3 `cyoda-go-cassandra` (sibling)

- Recommended-not-blocked PR: bump SPI pin from `v0.6.0` → `v0.7.1`. Run their tests.
- Notification per the new `KNOWN_CONSUMERS.md` etiquette (issue, comment, or DM with link to drafted PR).
- Not part of v0.7.1 release blocking. Their cadence is theirs.

## 4. Sequencing

Five phases, with hard ordering between them.

| Phase | Repo | Deliverable | Depends on |
|---|---|---|---|
| 1a | cyoda-go-spi | PR-S1: CI + supply-chain (workflows, dependabot, PR template) | — |
| 1b | cyoda-go-spi | PR-S2: governance docs (`MAINTAINING.md`, `CHANGELOG.md`, `KNOWN_CONSUMERS.md`, README pointer) | PR-S1 merged (CI must be green) |
| 1c | cyoda-go-spi | PR-S3: third-party on-ramp (`spitest/README.md` + conformance snippet) | PR-S2 merged |
| 2 | cyoda-go-spi | Tag `v0.7.1` (annotated, signed). First annotated+signed tag in repo history. **Release-prep commit immediately before tagging:** rename `[Unreleased]` → `[0.7.1] - YYYY-MM-DD` in `CHANGELOG.md`, push, then `git tag -s -a v0.7.1 -m "Release v0.7.1"`. The release-prep commit is a tiny diff that PR-S2 reviewers should expect; can be done as the final commit on PR-S3 or as a stand-alone tag-prep PR — author's choice. | Phase 1 complete |
| 3a | cyoda-go | PR-C1: SPI pin bump to v0.7.1 in root + plugins; `go mod tidy`; `make test-all` green | Phase 2 (tag must exist on proxy) |
| 3b | cyoda-go | PR-C2: drift gate, MAINTAINING.md amendments, COMPATIBILITY.md update | PR-C1 merged |
| 4 | cyoda-go | Cyoda-go v0.7.1 release per existing MAINTAINING.md procedure: plugin tags first (`plugins/{memory,postgres,sqlite}/v0.7.1` annotated+signed), drop replaces, pin plugin modules, COMPATIBILITY.md cross-repo rows, push `v0.7.1` tag, merge auto-opened chart-bump PR. | Phase 3 complete |
| 5 | cyoda-go-cassandra | Notify maintainers via KNOWN_CONSUMERS.md etiquette; draft bump PR (their team merges on their cadence) | Phase 4 complete (so their PR can pin the live v0.7.1) |

## 5. Success criteria

### Hard gates (objectively verifiable)

1. `gh api repos/Cyoda-platform/cyoda-go-spi/contents/.github` returns 200.
2. `git verify-tag v0.7.1` in `cyoda-go-spi` succeeds (annotated + signed). v0.1.0–v0.7.0 remain lightweight per the immutability rule.
3. `cyoda-go-spi/CHANGELOG.md` exists with a v0.7.1 row; preamble links MAINTAINING.md.
4. `cyoda-go-spi/MAINTAINING.md` exists with three sections: release process, deprecation policy, fix-forward.
5. `cyoda-go-spi/KNOWN_CONSUMERS.md` lists `cyoda-platform/cyoda-go` and `cyoda-platform/cyoda-go-cassandra`.
6. `cyoda-go-spi/spitest/README.md` (or equivalent) contains the conformance CI snippet for third parties.
7. `grep "cyoda-go-spi v" cyoda-go/go.mod cyoda-go/plugins/*/go.mod` returns exactly one distinct version: `v0.7.1`.
8. `make check-spi-pin-sync` exists and passes; CI job wired to PR/push.
9. `git tag -l "plugins/*/v0.7.1"` in `cyoda-go` lists three tags (memory, postgres, sqlite); all annotated + signed.
10. `cyoda-go v0.7.1` tag exists, annotated + signed; `release.yml` ran green; chart-bump PR merged.
11. `cyoda-go/MAINTAINING.md` contains the SPI lockstep-bump rule + drift gate reference; existing plugin-tag-lockstep rule preserved (not amended).
12. `cyoda-go/COMPATIBILITY.md` reflects the v0.7.1 row.

### Soft gates (judgement)

13. The fix-forward paragraph reads as current-state-and-intent (not a hedge); explicitly says v0.1.0–v0.7.0 are immutable lightweight by design.
14. The conformance CI snippet works when copy-pasted into a third-party repo (every import path resolves, no non-public references).
15. PR-C2 includes a deliberate "make this fail" exercise of the drift gate — confirmed to fire when manifests are out of sync, not just to pass on a pristine tree.
16. `cyoda-go-cassandra` team notified per `KNOWN_CONSUMERS.md` etiquette with link to drafted bump PR.

## 6. Non-goals (explicit)

- **Do not retag** v0.1.0–v0.7.0 in either repo. Lightweight history is immutable.
- **Do not retag** `plugins/{memory,postgres}/v0.1.0`. Stays as legacy artifact.
- **Do not add** cross-repo CI verification (option B/C from Q1) in this workstream. Revisit only if the new regime surfaces real drift incidents.
- **Do not automate** CHANGELOG generation or release-please. Manual edits stay manual.
- **Do not require** cassandra to bump in this workstream's release window. Their cadence is theirs.
- **Do not amend** the plugin-version-lockstep rule in MAINTAINING.md. It's correct for the codebase shape; the gap is enforcement.

## 7. Risks & mitigations

| Risk | Likelihood | Mitigation |
|---|---|---|
| Phase 4's first end-to-end run of MAINTAINING.md procedure exposes a documentation bug or release-workflow gap. | Medium — procedure has likely never been exercised end-to-end since the lockstep rule was written. | Treat the v0.7.1 release as a procedure validation event. Pause if anything fails; fix the procedure or the release tooling, then resume. The release content (docs + pin sync) is low-risk; the value is in proving the procedure works. |
| Tagging v0.7.1 in SPI before consumers verify it works against their plugins. | Low — SPI v0.7.1 is identical to v0.7.0 in API surface; only metadata changes (annotated+signed). | The cyoda-go side bumps to v0.7.1 in PR-C1 and runs `make test-all` before any cyoda-go release. If it breaks, we know before tagging anything else. |
| Cassandra team is mid-refactor and the SPI v0.7.1 bump is disruptive. | Low. | Notification is recommended-not-blocked; they merge on their cadence. We don't gate our v0.7.1 on theirs. |
| Plugin tags `plugins/{memory,postgres,sqlite}/v0.7.1` jump from v0.1.0 (memory, postgres) or no-tag (sqlite); a HEAD consumer of those submodule paths gets a new SPI version. | Low — no known consumers of plugin submodule paths independent of the umbrella. | If one surfaces post-release, they can pin to v0.1.0 explicitly. The CHANGELOG row documents the jump. |
| `make check-spi-pin-sync` is satisfied at PR time but drift sneaks in via a partial PR. | Low — gate runs on every PR. | Gate is on the diff post-merge state; partial drift fails CI before merge. |

## 8. Out of scope (acknowledged)

- Forced retagging of historical SPI tags (per never-force-move rule).
- Cross-repo CI verification (B/C from Q1).
- CHANGELOG auto-generation tooling.
- Independent plugin-version cadence (rejected; lockstep preserved).
- Tagging SPI minors purely for promotion/marketing.
