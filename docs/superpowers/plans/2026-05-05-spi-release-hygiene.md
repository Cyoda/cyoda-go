# SPI Release-Day Hygiene Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship symmetric `v0.7.1` releases of `cyoda-go-spi` (first annotated+signed tag, full governance regime) and `cyoda-go` (SPI pin sync across plugin go.mods, drift gate, plugin tags closing the five-version-gap), under the new release-day hygiene regime documented in MAINTAINING.md across both repos.

**Architecture:** Two repos, sequenced. SPI repo gets `.github/` (CI/Dependabot/CodeQL/PR template), `MAINTAINING.md`, `CHANGELOG.md`, `KNOWN_CONSUMERS.md`, README pointer, third-party conformance snippet — landed across PR-S1, PR-S2, PR-S3 then tagged v0.7.1. Cyoda-go side then bumps SPI pin to v0.7.1 across root + 3 plugin submodules (PR-C1), adds drift-gate CI + MAINTAINING.md amendments (PR-C2), and cuts v0.7.1 per existing MAINTAINING.md procedure including first-time-ever lockstep plugin-submodule tags.

**Tech Stack:** Go 1.26+, GitHub Actions, golangci-lint, CodeQL, Dependabot, GoReleaser (existing), git annotated+signed tags.

**Spec:** `docs/superpowers/specs/2026-05-05-spi-release-hygiene-design.md`

---

## Working directories

Two repos. Use absolute paths in commands to avoid confusion.

- `cyoda-go-spi` checkout: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/`. If sandbox blocks reads of this path, the executing session needs `additionalDirectories` or worktree permissions for it. Phases 1, 2 happen here.
- `cyoda-go` worktree: `/Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/spi-release-hygiene/` (current session). Phases 3, 4 happen here.
- `cyoda-go-cassandra` checkout: `/Users/paul/go-projects/cyoda-light/cyoda-go-cassandra/`. Phase 5 happens here (notification only).

When a task says "in SPI repo," `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi/` first. When it says "in cyoda-go," `cd` back to the worktree.

## Pre-phase: land spec and plan as a docs PR

The spec (`docs/superpowers/specs/2026-05-05-spi-release-hygiene-design.md`) and this plan are already committed on the worktree branch `worktree-spi-release-hygiene`. They land first as their own docs-only PR so subsequent feature branches branch fresh from `origin/main`.

- [ ] **Step 1: Push the worktree branch and open a docs PR**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/spi-release-hygiene
git push -u origin worktree-spi-release-hygiene

gh pr create -R Cyoda-platform/cyoda-go \
  --title "docs(superpowers): spec and plan for SPI release-day hygiene workstream" \
  --base main \
  --head worktree-spi-release-hygiene \
  --body "Spec + implementation plan for the SPI release-day hygiene workstream landing as symmetric v0.7.1 across cyoda-go-spi and cyoda-go. Subsequent feature branches branch off main once this merges."
```

- [ ] **Step 2: After CI green, merge**

After this PR merges, all subsequent cyoda-go feature branches in this plan branch off the new `origin/main` (which contains spec+plan).



## File inventory

**`cyoda-go-spi` — created**
- `.github/workflows/ci.yml`
- `.github/workflows/codeql.yml`
- `.github/dependabot.yml`
- `.github/PULL_REQUEST_TEMPLATE.md`
- `MAINTAINING.md`
- `CHANGELOG.md`
- `KNOWN_CONSUMERS.md`
- `spitest/README.md` (or extend if exists)

**`cyoda-go-spi` — modified**
- `README.md` (add Versioning & Compatibility section)

**`cyoda-go` — modified**
- `plugins/memory/go.mod`
- `plugins/postgres/go.mod`
- `plugins/sqlite/go.mod`
- `go.mod` (root)
- `go.sum` files (auto from `go mod tidy`)
- `MAINTAINING.md` (add SPI lockstep rule + drift gate reference)
- `CONTRIBUTING.md` (one-line note re drift gate)
- `COMPATIBILITY.md` (v0.7.1 row)
- `Makefile` (new `check-spi-pin-sync` target)
- `.github/workflows/ci.yml` (new job)

**`cyoda-go` — created**
- `scripts/check-spi-pin-sync.sh`

**Tags created**
- `cyoda-go-spi`: `v0.7.1` (annotated+signed, first ever)
- `cyoda-go`: `plugins/memory/v0.7.1`, `plugins/postgres/v0.7.1`, `plugins/sqlite/v0.7.1`, `v0.7.1` (all annotated+signed)

---

## Phase 1a: PR-S1 — SPI CI + supply-chain

**Files:**
- Create: `cyoda-go-spi/.github/workflows/ci.yml`
- Create: `cyoda-go-spi/.github/workflows/codeql.yml`
- Create: `cyoda-go-spi/.github/dependabot.yml`
- Create: `cyoda-go-spi/.github/PULL_REQUEST_TEMPLATE.md`

### Task 1: Set up SPI working branch

- [ ] **Step 1: Switch to SPI repo, fetch latest, branch off main**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git fetch origin
git checkout main
git pull --ff-only origin main
git checkout -b release-hygiene/ci-supply-chain
```

- [ ] **Step 2: Confirm baseline is clean**

```bash
git status
go vet ./...
go test ./...
```

Expected: clean working tree; `go vet` silent; tests pass.

### Task 2: Create CI workflow

- [ ] **Step 1: Write `cyoda-go-spi/.github/workflows/ci.yml`**

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]
  workflow_dispatch:

permissions:
  contents: read

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6.0.2

      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod

      - name: Vet
        run: go vet ./...

      - name: Build
        run: go build ./...

      - name: Test
        run: go test ./... -v

      - name: Test (race)
        run: go test -race ./...

  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6.0.2

      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod

      - uses: golangci/golangci-lint-action@v8
        with:
          version: latest
```

- [ ] **Step 2: Verify YAML parses locally**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml'))"
```

Expected: silent (no exception).

### Task 3: Create CodeQL workflow

- [ ] **Step 1: Write `cyoda-go-spi/.github/workflows/codeql.yml`**

```yaml
name: CodeQL

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]
  schedule:
    - cron: '23 4 * * 1'  # Mondays 04:23 UTC

permissions:
  contents: read
  security-events: write
  actions: read

jobs:
  analyze:
    runs-on: ubuntu-latest
    strategy:
      fail-fast: false
      matrix:
        language: [go]
    steps:
      - uses: actions/checkout@v6.0.2

      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod

      - uses: github/codeql-action/init@v3
        with:
          languages: ${{ matrix.language }}

      - uses: github/codeql-action/autobuild@v3

      - uses: github/codeql-action/analyze@v3
        with:
          category: "/language:${{ matrix.language }}"
```

- [ ] **Step 2: Verify YAML parses**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/codeql.yml'))"
```

### Task 4: Create Dependabot config

- [ ] **Step 1: Write `cyoda-go-spi/.github/dependabot.yml`**

```yaml
version: 2
updates:
  - package-ecosystem: gomod
    directory: "/"
    schedule:
      interval: weekly
      day: monday
    open-pull-requests-limit: 10
    groups:
      minor-and-patch:
        update-types:
          - minor
          - patch

  - package-ecosystem: github-actions
    directory: "/"
    schedule:
      interval: weekly
      day: monday
    open-pull-requests-limit: 5
```

- [ ] **Step 2: Verify YAML parses**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/dependabot.yml'))"
```

### Task 5: Create PR template

- [ ] **Step 1: Write `cyoda-go-spi/.github/PULL_REQUEST_TEMPLATE.md`**

```markdown
## Summary

<!-- One or two sentences. Why is this change needed? -->

## Public-symbol checklist

- [ ] No public-symbol changes, OR
- [ ] `CHANGELOG.md` updated under `[Unreleased]` (Added / Changed / Deprecated / Removed / Fixed / Breaking)
- [ ] If breaking: each consumer in `KNOWN_CONSUMERS.md` notified (issue, comment, or DM); confirmation linked here
- [ ] If deprecating: symbols carry `// Deprecated:` comments with migration guidance

## Test plan

<!-- How was this verified? -->
```

### Task 6: Commit, push, open PR-S1

- [ ] **Step 1: Stage and commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add .github/
git commit -m "$(cat <<'EOF'
chore(ci): add CI, CodeQL, Dependabot, PR template

First .github/ contents in this repo. Establishes self-contained CI
(go vet/build/test/race + golangci-lint), weekly CodeQL, weekly
gomod + github-actions Dependabot updates, and a PR template that
prompts for CHANGELOG + KNOWN_CONSUMERS hygiene on public-symbol
changes.

Part of the release-day hygiene workstream landing as cyoda-go-spi
v0.7.1 (first annotated+signed tag).
EOF
)"
```

- [ ] **Step 2: Push branch**

```bash
GH_TOKEN_TARGET="${GH_TOKEN:-$(gh auth token)}"
git -c credential.helper="!f() { echo username=x-access-token; echo password=$GH_TOKEN_TARGET; }; f" \
    push -u origin release-hygiene/ci-supply-chain
```

- [ ] **Step 3: Open PR-S1**

```bash
gh pr create -R Cyoda-platform/cyoda-go-spi \
  --title "chore(ci): add CI, CodeQL, Dependabot, PR template" \
  --body "$(cat <<'EOF'
## Summary

First `.github/` contents in this repo. Adds:
- `ci.yml` — `go vet`, `go build`, `go test`, race detector, `golangci-lint`
- `codeql.yml` — weekly + on PR
- `dependabot.yml` — weekly gomod + github-actions
- PR template prompting CHANGELOG + KNOWN_CONSUMERS hygiene

Part of the release-day hygiene workstream (spec: cyoda-go `docs/superpowers/specs/2026-05-05-spi-release-hygiene-design.md`) landing as v0.7.1.

## Test plan

- [ ] CI green on this PR (validates the workflow itself runs)
- [ ] CodeQL green (validates the workflow runs; findings on a 400-LOC interface module are expected to be sparse)
EOF
)"
```

- [ ] **Step 4: Wait for CI green, request review, merge**

After CI passes and review is complete: merge with squash. Subsequent SPI tasks branch off the new main.

---

## Phase 1b: PR-S2 — SPI governance docs

**Files:**
- Create: `cyoda-go-spi/MAINTAINING.md`
- Create: `cyoda-go-spi/CHANGELOG.md`
- Create: `cyoda-go-spi/KNOWN_CONSUMERS.md`
- Modify: `cyoda-go-spi/README.md`

### Task 7: Branch for governance docs

- [ ] **Step 1: Refresh main, branch**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git checkout main
git pull --ff-only origin main
git checkout -b release-hygiene/governance-docs
```

### Task 8: Write MAINTAINING.md

- [ ] **Step 1: Create `cyoda-go-spi/MAINTAINING.md`**

```markdown
# Maintaining cyoda-go-spi

This document is the canonical reference for releasing `cyoda-go-spi` and
governing changes to its public surface. It complements `CONTRIBUTING.md`
(which covers contributing changes) and `KNOWN_CONSUMERS.md` (which lists
projects that depend on this module's stability).

## Release process

`cyoda-go-spi` is a small interface module with no release artefacts other
than the Git tag itself. We do not use a `release/` branch — the repo is
small enough that direct-to-main is appropriate.

### 1. Cut a release

From an up-to-date `main`:

1. Make sure all merged-since-last-tag changes are reflected in `CHANGELOG.md`'s `[Unreleased]` section.
2. As a final preparation commit, rename `[Unreleased]` to `[X.Y.Z] - YYYY-MM-DD` and push to `main`.
3. Create the annotated, signed tag:
   ```bash
   git tag -s -a vX.Y.Z -m "Release vX.Y.Z"
   git push origin vX.Y.Z
   ```
4. Verify the tag is annotated + signed:
   ```bash
   git verify-tag vX.Y.Z
   ```
   Expected: signature verifies. (Lightweight tags will print `cannot verify a non-tag object of type commit`.)

### 2. Notify known consumers

Every entry in `KNOWN_CONSUMERS.md` should be notified of the new tag,
especially if the release contains breaking changes or deprecations.

## Versioning

`cyoda-go-spi` follows Go module versioning rules. **Tags are immutable.**
sum.golang.org caches the SHA of every tag it serves, so tags cannot be
moved or reused. Each new release uses a strictly greater version than the
previous tag.

## Deprecation policy

This is a pre-1.0 module. The following rules govern how the public surface
evolves:

**Pre-1.0 (current era):**

- Minor versions are **additive-only by default**. New methods, types, or
  options can be added in any minor release.
- Breaking changes are permitted in a minor release iff:
  - The change is called out in `CHANGELOG.md` under `### Breaking` with
    explicit migration notes.
  - Where feasible, deprecated symbols carry `// Deprecated: <reason>`
    comments for at least one prior minor release before removal.
  - Each consumer listed in `KNOWN_CONSUMERS.md` has been notified
    before the breaking PR is merged. The notification is linked from
    the PR description.
- Patch versions (`vX.Y.Z` where `Z > 0`) are reserved for fixes and
  metadata-only changes (e.g. updated README, security advisories).
  Patch versions never change the public surface.

**Post-1.0:**

- Standard semver applies.
- Breaking changes require a major version bump.
- Deprecated symbols carry `// Deprecated:` comments for at least one
  full minor release before removal.

## Fixing forward

Tags `v0.1.0` through `v0.7.0` are **lightweight** (commit-pointer) tags
by design — they are immutable per Go module checksum stability and we
do not retroactively modify them. Beginning with **v0.7.1**, all tags
are annotated and signed.

The new regime is forward-only: existing history stays as-is, new
releases follow the new rules. Reviewers and consumers should treat
this as a clean line drawn at v0.7.1, not a defect in the historical
tags.
```

### Task 9: Write CHANGELOG.md

- [ ] **Step 1: Create `cyoda-go-spi/CHANGELOG.md`**

```markdown
# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to the deprecation policy documented in
[MAINTAINING.md](MAINTAINING.md#deprecation-policy).

For the rationale behind the absence of CHANGELOG entries before v0.7.1,
see the [Fixing forward](MAINTAINING.md#fixing-forward) section of
MAINTAINING.md.

## [Unreleased]

### Added

- `.github/workflows/ci.yml`: self-contained CI running `go vet`,
  `go build`, `go test`, race detector, and `golangci-lint`.
- `.github/workflows/codeql.yml`: weekly CodeQL analysis + on-PR.
- `.github/dependabot.yml`: weekly Dependabot updates for gomod and
  github-actions ecosystems.
- `.github/PULL_REQUEST_TEMPLATE.md`: PR template prompting CHANGELOG
  and KNOWN_CONSUMERS hygiene on public-symbol changes.
- `MAINTAINING.md`: release process, deprecation policy, and the
  fixing-forward statement establishing the new regime.
- `CHANGELOG.md`: this file.
- `KNOWN_CONSUMERS.md`: opt-in registry of projects depending on
  this module.
- `README.md`: Versioning & Compatibility section linking to the
  three documents above.
- `spitest/README.md`: third-party plugin authoring guide with a
  copy-pasteable conformance CI snippet.

### Changed

- Tags from this release forward are annotated and signed. Tags
  `v0.1.0` through `v0.7.0` remain lightweight per the
  fixing-forward rule.
```

### Task 10: Write KNOWN_CONSUMERS.md

- [ ] **Step 1: Create `cyoda-go-spi/KNOWN_CONSUMERS.md`**

```markdown
# Known consumers of cyoda-go-spi

This file lists projects that depend on `cyoda-go-spi` and have asked to
be notified before breaking changes ship. Inclusion is opt-in — open a
PR to add an entry. The notification etiquette for breaking-change PRs
is documented in [MAINTAINING.md](MAINTAINING.md#deprecation-policy).

## How to add your project

Open a PR adding an entry below in this format:

```
- **org/repo** — claims compliance with vX.Y.Z; contact @handle (issues / DM)
```

Maintainers will merge if the entry is well-formed.

## Current consumers

- **cyoda-platform/cyoda-go** — in-tree storage plugins (memory, postgres, sqlite); claims compliance with v0.7.0+; contact @cyoda-platform maintainers via repo issues.
- **cyoda-platform/cyoda-go-cassandra** — Cassandra storage plugin; claims compliance with v0.6.0+; contact @cyoda-platform maintainers via repo issues.
```

### Task 11: Update README.md with pointer

- [ ] **Step 1: Read current README.md**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
wc -l README.md
```

- [ ] **Step 2: Append a Versioning & Compatibility section**

Add at the end of `README.md` (or before the License section if one exists):

```markdown
## Versioning & Compatibility

- **Releases & changes:** see [`CHANGELOG.md`](CHANGELOG.md).
- **Release process & deprecation policy:** see [`MAINTAINING.md`](MAINTAINING.md).
- **Notifications for breaking changes:** open an issue or PR your project
  into [`KNOWN_CONSUMERS.md`](KNOWN_CONSUMERS.md).
```

### Task 12: Commit and open PR-S2

- [ ] **Step 1: Commit**

```bash
git add MAINTAINING.md CHANGELOG.md KNOWN_CONSUMERS.md README.md
git commit -m "$(cat <<'EOF'
docs: add MAINTAINING.md, CHANGELOG.md, KNOWN_CONSUMERS.md

Establishes the cyoda-go-spi governance regime:
- MAINTAINING.md: release process, deprecation policy (pre-1.0:
  additive-only by default; breaking minors permitted with
  CHANGELOG callout, deprecation markers, and consumer
  notification), and the fixing-forward statement explaining
  why v0.1.0-v0.7.0 stay lightweight while v0.7.1+ are
  annotated+signed.
- CHANGELOG.md: Keep-a-Changelog format; first [Unreleased]
  collects the hygiene additions for v0.7.1.
- KNOWN_CONSUMERS.md: opt-in registry seeded with cyoda-go and
  cyoda-go-cassandra.
- README.md: Versioning & Compatibility pointer.

Part of the release-day hygiene workstream.
EOF
)"
```

- [ ] **Step 2: Push and open PR-S2**

```bash
GH_TOKEN_TARGET="${GH_TOKEN:-$(gh auth token)}"
git -c credential.helper="!f() { echo username=x-access-token; echo password=$GH_TOKEN_TARGET; }; f" \
    push -u origin release-hygiene/governance-docs

gh pr create -R Cyoda-platform/cyoda-go-spi \
  --title "docs: add MAINTAINING.md, CHANGELOG.md, KNOWN_CONSUMERS.md" \
  --body "$(cat <<'EOF'
## Summary

Establishes the cyoda-go-spi governance regime:
- `MAINTAINING.md`: release process + deprecation policy + fixing-forward
- `CHANGELOG.md`: Keep-a-Changelog with `[Unreleased]` collecting v0.7.1 additions
- `KNOWN_CONSUMERS.md`: opt-in registry, seeded with cyoda-go + cyoda-go-cassandra
- `README.md`: pointer section

Spec: cyoda-go `docs/superpowers/specs/2026-05-05-spi-release-hygiene-design.md`.

Depends on PR-S1 (CI workflow). Once this merges, PR-S3 (third-party on-ramp) lands next.

## Test plan

- [ ] CI green
- [ ] Reviewer reads MAINTAINING.md fixing-forward paragraph and confirms it reads as current-state-and-intent (not a hedge)
EOF
)"
```

- [ ] **Step 3: After CI green and review, merge**

---

## Phase 1c: PR-S3 — Third-party on-ramp

**Files:**
- Create or modify: `cyoda-go-spi/spitest/README.md`

### Task 13: Branch for spitest README

- [ ] **Step 1: Refresh main, branch**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git checkout main && git pull --ff-only origin main
git checkout -b release-hygiene/spitest-readme
```

- [ ] **Step 2: Check if `spitest/README.md` already exists**

```bash
ls spitest/
```

If `spitest/README.md` exists, edit it. If not, create it.

### Task 14: Write spitest README with conformance CI snippet

- [ ] **Step 1: Write `cyoda-go-spi/spitest/README.md`**

```markdown
# spitest — conformance test harness for storage plugins

`spitest` is a Go test harness that exercises the cyoda-go-spi contract
against a backend implementation. It is the canonical way for a storage
plugin to verify its compliance with a given SPI version.

## Using spitest in your plugin

In your plugin's tests, import the relevant `spitest` subpackages and
hand them a constructor that produces a fresh instance of your backend.
See `cyoda-platform/cyoda-go/plugins/memory` for an idiomatic example.

The harness covers the full SPI surface: entity persistence, audit,
async search, transactions, workflow plugin contracts, and key/value
extension hooks.

## Conformance against latest SPI HEAD (recommended for plugin authors)

If you maintain a third-party storage plugin, add a nightly job to your
own CI that exercises `spitest` against the latest SPI `main`. This
catches contract regressions before SPI tags a release.

The snippet below is a ready-to-paste GitHub Actions workflow. Drop it
into your plugin repo at `.github/workflows/spi-head-conformance.yml`,
adjust `MY_PLUGIN_TEST_PATH` to the test directory in your repo that
exercises `spitest`, and you'll get a nightly check.

```yaml
name: SPI HEAD conformance

on:
  schedule:
    - cron: '0 6 * * *'  # daily 06:00 UTC
  workflow_dispatch:

permissions:
  contents: read

env:
  MY_PLUGIN_TEST_PATH: ./...    # adjust to your tests' import path

jobs:
  spi-head:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6.0.2
        with:
          path: plugin

      - uses: actions/checkout@v6.0.2
        with:
          repository: cyoda-platform/cyoda-go-spi
          ref: main
          path: spi-head

      - uses: actions/setup-go@v6
        with:
          go-version-file: plugin/go.mod

      - name: Replace SPI dep with HEAD
        working-directory: plugin
        run: |
          go mod edit -replace github.com/cyoda-platform/cyoda-go-spi=../spi-head
          go mod tidy

      - name: Run spitest against your backend
        working-directory: plugin
        run: go test -v $MY_PLUGIN_TEST_PATH

      - name: Restore go.mod (in case the job continues)
        working-directory: plugin
        run: |
          go mod edit -dropreplace github.com/cyoda-platform/cyoda-go-spi
          go mod tidy
```

If the nightly run goes red, please open an issue against
[cyoda-go-spi](https://github.com/Cyoda-platform/cyoda-go-spi/issues)
referencing the failing commit and your plugin's import path.

## Registering your plugin

If you'd like to be notified before SPI breaking changes ship, open a
PR adding your plugin to [`KNOWN_CONSUMERS.md`](../KNOWN_CONSUMERS.md).
The deprecation policy in [`MAINTAINING.md`](../MAINTAINING.md)
requires SPI maintainers to notify each registered consumer before a
breaking change merges.
```

### Task 15: Validate the snippet out-of-tree

The SPI repo has no in-tree backend, so the snippet can't be exercised in SPI's own CI. Validate manually before opening the PR.

- [ ] **Step 1: Validate by copy-paste into a scratch checkout of cyoda-go**

```bash
# In a scratch dir (separate from the worktree)
cd /tmp
rm -rf spi-snippet-check
git clone /Users/paul/go-projects/cyoda-light/cyoda-go spi-snippet-check
cd spi-snippet-check/plugins/memory
go mod edit -replace github.com/cyoda-platform/cyoda-go-spi=/Users/paul/go-projects/cyoda-light/cyoda-go-spi
go mod tidy
go test -v ./...
```

Expected: tests pass. Confirms the snippet's path-replace mechanism is sound.

- [ ] **Step 2: Restore the scratch dir's go.mod or just delete it**

```bash
cd /tmp && rm -rf spi-snippet-check
```

### Task 16: Commit and open PR-S3

- [ ] **Step 1: Stage, commit, push, open PR**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add spitest/README.md
git commit -m "$(cat <<'EOF'
docs(spitest): add plugin authoring guide + conformance CI snippet

Documents how third-party storage plugin authors use spitest to verify
SPI compliance, and provides a ready-to-paste GitHub Actions workflow
for nightly conformance against SPI HEAD via go.mod replace directive.

The snippet lives in spitest/README.md and is exercised out-of-tree
(SPI has no in-tree backend). Cross-referenced from KNOWN_CONSUMERS.md
for the registration on-ramp.
EOF
)"

GH_TOKEN_TARGET="${GH_TOKEN:-$(gh auth token)}"
git -c credential.helper="!f() { echo username=x-access-token; echo password=$GH_TOKEN_TARGET; }; f" \
    push -u origin release-hygiene/spitest-readme

gh pr create -R Cyoda-platform/cyoda-go-spi \
  --title "docs(spitest): plugin authoring guide + conformance CI snippet" \
  --body "$(cat <<'EOF'
## Summary

Documents how third-party plugin authors use spitest to verify SPI
compliance, and provides a copy-pasteable GitHub Actions workflow for
nightly conformance against SPI HEAD.

The snippet was validated by manual copy into a scratch checkout of
cyoda-go (replace-to-local SPI worktree) — plugins/memory tests pass
under the snippet's mechanism.

Spec: cyoda-go `docs/superpowers/specs/2026-05-05-spi-release-hygiene-design.md`.

This is the last PR in Phase 1; after merge, the v0.7.1 release-prep
commit + tag follows (Phase 2).

## Test plan

- [ ] CI green
- [ ] Reviewer copy-pastes the YAML into a scratch repo and confirms it parses
EOF
)"
```

- [ ] **Step 2: After CI green and review, merge**

---

## Phase 2: SPI v0.7.1 release

### Task 17: Release-prep commit

- [ ] **Step 1: Refresh main**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git checkout main && git pull --ff-only origin main
git checkout -b release-hygiene/v0.7.1-prep
```

- [ ] **Step 2: Rename `[Unreleased]` → `[0.7.1] - YYYY-MM-DD` in CHANGELOG.md**

Edit `CHANGELOG.md`. Replace `## [Unreleased]` with `## [0.7.1] - 2026-MM-DD` (use today's date in ISO-8601).

If new entries have accumulated under `[Unreleased]` since PR-S2 merged (e.g. from PR-S3), they all become part of the v0.7.1 row.

- [ ] **Step 3: Commit, push, open the prep PR**

```bash
git add CHANGELOG.md
git commit -m "$(cat <<'EOF'
chore(release): prep CHANGELOG for v0.7.1

Renames [Unreleased] to [0.7.1] - YYYY-MM-DD. The v0.7.1 tag
follows immediately on merge.

This is the first annotated+signed tag in the repo per
MAINTAINING.md's fixing-forward rule. Lightweight tags
v0.1.0-v0.7.0 remain immutable.
EOF
)"

GH_TOKEN_TARGET="${GH_TOKEN:-$(gh auth token)}"
git -c credential.helper="!f() { echo username=x-access-token; echo password=$GH_TOKEN_TARGET; }; f" \
    push -u origin release-hygiene/v0.7.1-prep

gh pr create -R Cyoda-platform/cyoda-go-spi \
  --title "chore(release): prep CHANGELOG for v0.7.1" \
  --body "Renames [Unreleased] to [0.7.1] - $(date -u +%Y-%m-%d). v0.7.1 tag follows on merge."
```

- [ ] **Step 4: After CI green, merge**

### Task 18: Tag v0.7.1

- [ ] **Step 1: Pull merged main, verify CHANGELOG**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git checkout main && git pull --ff-only origin main
head -25 CHANGELOG.md
```

Expected: `## [0.7.1] - YYYY-MM-DD` is the first version section.

- [ ] **Step 2: Create annotated, signed tag**

```bash
git tag -s -a v0.7.1 -m "Release v0.7.1

First annotated+signed tag. Establishes release governance regime:
- MAINTAINING.md: release process, deprecation policy, fixing-forward
- CHANGELOG.md: Keep-a-Changelog format
- KNOWN_CONSUMERS.md: opt-in registry
- spitest conformance harness CI snippet for plugin authors
- .github/: CI, CodeQL, Dependabot, PR template

Lightweight tags v0.1.0-v0.7.0 remain immutable per Go module
checksum stability rules. v0.7.1+ all annotated and signed."
```

If `git tag -s` fails because no signing key is configured, the operator must configure GPG signing first (`gpg --list-secret-keys` to list, `git config --global user.signingkey <KEY>`). Do not fall back to unsigned — the regime requires signed tags.

- [ ] **Step 3: Verify locally**

```bash
git verify-tag v0.7.1
```

Expected: signature verifies. (If "cannot verify a non-tag object" appears, the tag is lightweight — re-create with `-s -a`.)

- [ ] **Step 4: Push tag**

```bash
GH_TOKEN_TARGET="${GH_TOKEN:-$(gh auth token)}"
git -c credential.helper="!f() { echo username=x-access-token; echo password=$GH_TOKEN_TARGET; }; f" \
    push origin v0.7.1
```

- [ ] **Step 5: Confirm via GitHub API**

```bash
gh api repos/Cyoda-platform/cyoda-go-spi/git/refs/tags/v0.7.1
```

Expected: `"object": { "type": "tag", ... }` (annotated). For comparison, `v0.7.0` will show `"type": "commit"` (lightweight).

### Task 19: Confirm proxy availability

- [ ] **Step 1: Wait for sum.golang.org to pick up the tag (~minutes)**

```bash
curl -s "https://proxy.golang.org/github.com/cyoda-platform/cyoda-go-spi/@v/v0.7.1.info"
```

Expected (after a few minutes): `{"Version":"v0.7.1","Time":"..."}`. Retry up to ~5 minutes if 404.

---

## Phase 3a: PR-C1 — cyoda-go SPI pin sync

Switch back to the cyoda-go worktree.

**Files:**
- Modify: `cyoda-go/plugins/memory/go.mod`
- Modify: `cyoda-go/plugins/postgres/go.mod`
- Modify: `cyoda-go/plugins/sqlite/go.mod`
- Modify: `cyoda-go/go.mod`
- (Modified by `go mod tidy`): `go.sum` files

### Task 20: Branch for pin sync

The docs PR (Pre-phase) has merged, so `origin/main` now contains spec+plan. Branch fresh from there for pin-sync.

- [ ] **Step 1: Switch to cyoda-go worktree, branch off updated main**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/spi-release-hygiene
git fetch origin main
git checkout -b release-hygiene/spi-pin-sync origin/main
```

The pin-sync PR will be small and self-contained — go.mod + go.sum changes only.

### Task 21: Bump SPI pin in plugin go.mods

- [ ] **Step 1: plugins/memory**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/spi-release-hygiene/plugins/memory
go mod edit -require=github.com/cyoda-platform/cyoda-go-spi@v0.7.1
go mod tidy
grep "cyoda-go-spi" go.mod
```

Expected: `github.com/cyoda-platform/cyoda-go-spi v0.7.1`

- [ ] **Step 2: plugins/postgres**

```bash
cd ../postgres
go mod edit -require=github.com/cyoda-platform/cyoda-go-spi@v0.7.1
go mod tidy
grep "cyoda-go-spi" go.mod
```

- [ ] **Step 3: plugins/sqlite**

```bash
cd ../sqlite
go mod edit -require=github.com/cyoda-platform/cyoda-go-spi@v0.7.1
go mod tidy
grep "cyoda-go-spi" go.mod
```

- [ ] **Step 4: Root go.mod**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/spi-release-hygiene
go mod edit -require=github.com/cyoda-platform/cyoda-go-spi@v0.7.1
go mod tidy
grep "cyoda-go-spi" go.mod plugins/*/go.mod
```

Expected: every line shows `cyoda-go-spi v0.7.1`. No mixed versions.

### Task 22: Run full test suite

- [ ] **Step 1: Make test-all**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/spi-release-hygiene
make test-all
```

Expected: all tests pass (root + memory + sqlite + postgres). Docker required for postgres testcontainers.

- [ ] **Step 2: go vet**

```bash
go vet ./...
```

Expected: silent.

### Task 23: Commit and open PR-C1

- [ ] **Step 1: Stage modified manifests + sums**

```bash
git add go.mod go.sum plugins/memory/go.mod plugins/memory/go.sum \
        plugins/postgres/go.mod plugins/postgres/go.sum \
        plugins/sqlite/go.mod plugins/sqlite/go.sum
```

- [ ] **Step 2: Commit**

```bash
git commit -m "$(cat <<'EOF'
chore(deps): bump cyoda-go-spi to v0.7.1 across root and plugins

Closes the SPI version drift between cyoda-go's root go.mod (v0.7.0)
and plugins/{memory,postgres,sqlite}/go.mod (v0.6.1). Drift was
masked at runtime by Go MVS resolution but appeared in
per-submodule manifests and per-submodule CI runs.

cyoda-go-spi v0.7.1 itself adds no API surface vs v0.7.0; it is
the first annotated+signed tag and anchors the new release-day
hygiene regime documented in cyoda-go-spi/MAINTAINING.md. Bumping
to v0.7.1 (rather than just to v0.7.0) signals lockstep with the
new regime.

Drift gate that prevents recurrence ships in the next PR.
EOF
)"
```

- [ ] **Step 3: Push and open PR-C1**

```bash
git push -u origin worktree-spi-release-hygiene  # or release-hygiene/spi-pin-sync if branched separately

gh pr create -R Cyoda-platform/cyoda-go \
  --title "chore(deps): bump cyoda-go-spi to v0.7.1 across root and plugins" \
  --base main \
  --body "$(cat <<'EOF'
## Summary

Closes the SPI pin drift between root (v0.7.0) and `plugins/*/go.mod` (v0.6.1). Bumps everyone to **v0.7.1** — the first annotated+signed SPI tag, anchoring the new release-day hygiene regime.

API impact: none. v0.7.1 of cyoda-go-spi is metadata-only over v0.7.0.

The drift gate that prevents recurrence ships in the next PR (PR-C2).

Spec: `docs/superpowers/specs/2026-05-05-spi-release-hygiene-design.md`.
Plan: `docs/superpowers/plans/2026-05-05-spi-release-hygiene.md`.

## Test plan

- [ ] `make test-all` green locally (root + memory + sqlite + postgres)
- [ ] CI green
- [ ] `grep "cyoda-go-spi v" go.mod plugins/*/go.mod` shows exactly one distinct version: v0.7.1
EOF
)"
```

- [ ] **Step 4: After CI green and review, merge**

---

## Phase 3b: PR-C2 — Drift gate + MAINTAINING.md amendments

**Files:**
- Create: `cyoda-go/scripts/check-spi-pin-sync.sh`
- Create or modify: `cyoda-go/Makefile` (add `check-spi-pin-sync` target)
- Modify: `cyoda-go/.github/workflows/ci.yml` (add gate job)
- Modify: `cyoda-go/MAINTAINING.md`
- Modify: `cyoda-go/CONTRIBUTING.md`
- Modify: `cyoda-go/COMPATIBILITY.md`
- Test: `cyoda-go/scripts/check-spi-pin-sync_test.sh` (TDD harness for the script)

### Task 24: Branch for drift gate

- [ ] **Step 1: Refresh main, branch**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/spi-release-hygiene
git fetch origin main
git checkout -b release-hygiene/drift-gate origin/main
```

### Task 25: TDD — write a failing test for the drift gate

- [ ] **Step 1: Create `scripts/check-spi-pin-sync_test.sh`**

```bash
mkdir -p scripts
```

Write the file with these contents:

```bash
#!/usr/bin/env bash
# Test harness for scripts/check-spi-pin-sync.sh.
# Creates a scratch repo with deliberately-drifted go.mods and asserts the
# gate exits non-zero. Then aligns the manifests and asserts the gate exits zero.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
SCRIPT="$REPO_ROOT/scripts/check-spi-pin-sync.sh"

[[ -x "$SCRIPT" ]] || { echo "FAIL: $SCRIPT is missing or not executable"; exit 1; }

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

# Drift case: root v0.7.1, one plugin v0.6.1 → must fail
mkdir -p "$scratch/drift/plugins/a" "$scratch/drift/plugins/b"
cat > "$scratch/drift/go.mod" <<'EOF'
module example.com/root
go 1.26
require github.com/cyoda-platform/cyoda-go-spi v0.7.1
EOF
cat > "$scratch/drift/plugins/a/go.mod" <<'EOF'
module example.com/root/plugins/a
go 1.26
require github.com/cyoda-platform/cyoda-go-spi v0.7.1
EOF
cat > "$scratch/drift/plugins/b/go.mod" <<'EOF'
module example.com/root/plugins/b
go 1.26
require github.com/cyoda-platform/cyoda-go-spi v0.6.1
EOF

if (cd "$scratch/drift" && "$SCRIPT") >/dev/null 2>&1; then
  echo "FAIL: drift case did not fail (expected non-zero)"; exit 1
fi
echo "PASS: drift case correctly failed"

# Aligned case: every manifest at v0.7.1 → must pass
mkdir -p "$scratch/aligned/plugins/a" "$scratch/aligned/plugins/b"
for f in "$scratch/aligned/go.mod" "$scratch/aligned/plugins/a/go.mod" "$scratch/aligned/plugins/b/go.mod"; do
  cat > "$f" <<'EOF'
module example.com
go 1.26
require github.com/cyoda-platform/cyoda-go-spi v0.7.1
EOF
done

if ! (cd "$scratch/aligned" && "$SCRIPT") >/dev/null 2>&1; then
  echo "FAIL: aligned case unexpectedly failed"; exit 1
fi
echo "PASS: aligned case correctly succeeded"

echo "OK: scripts/check-spi-pin-sync.sh exhibits expected behavior"
```

```bash
chmod +x scripts/check-spi-pin-sync_test.sh
```

- [ ] **Step 2: Run the test — expect FAIL because the script doesn't exist yet**

```bash
./scripts/check-spi-pin-sync_test.sh
```

Expected: `FAIL: scripts/check-spi-pin-sync.sh is missing or not executable` and exit non-zero.

### Task 26: Implement the drift gate script

- [ ] **Step 1: Create `scripts/check-spi-pin-sync.sh`**

```bash
#!/usr/bin/env bash
# Verifies every go.mod in this repo (root + plugins/*) pins
# github.com/cyoda-platform/cyoda-go-spi to the same version.
#
# Exits 0 if all pins agree.
# Exits 1 if any go.mod disagrees, with a readable diff.
#
# Used as a CI gate to prevent the v0.6.1-vs-v0.7.0 drift recurring.

set -euo pipefail

# Use null-delimited globbing to handle shells that lack ** safely.
mapfile -t mod_files < <(find . -mindepth 1 -maxdepth 4 -name go.mod -not -path '*/.*' -not -path '*/vendor/*' | sort)

declare -A versions_by_path
declare -A files_by_version

for mod in "${mod_files[@]}"; do
  ver=$(awk '/[[:space:]]github\.com\/cyoda-platform\/cyoda-go-spi v/ { print $NF; exit }' "$mod" || true)
  if [[ -n "${ver:-}" ]]; then
    versions_by_path["$mod"]="$ver"
    files_by_version["$ver"]="${files_by_version[$ver]:-} $mod"
  fi
done

distinct=$(printf '%s\n' "${versions_by_path[@]}" | sort -u | wc -l | tr -d ' ')

if (( distinct == 0 )); then
  echo "check-spi-pin-sync: no go.mod files reference cyoda-go-spi (nothing to check)"
  exit 0
fi

if (( distinct == 1 )); then
  the_version=$(printf '%s\n' "${versions_by_path[@]}" | sort -u | head -1)
  echo "check-spi-pin-sync: OK — all manifests pin cyoda-go-spi $the_version"
  exit 0
fi

echo "check-spi-pin-sync: FAIL — cyoda-go-spi pin drift detected"
echo
for ver in "${!files_by_version[@]}"; do
  echo "  $ver:"
  for f in ${files_by_version[$ver]}; do
    echo "    $f"
  done
done
echo
echo "Resolution: bump cyoda-go-spi to a single version across root and plugins/*."
exit 1
```

```bash
chmod +x scripts/check-spi-pin-sync.sh
```

- [ ] **Step 2: Re-run the test harness — expect PASS**

```bash
./scripts/check-spi-pin-sync_test.sh
```

Expected:

```
PASS: drift case correctly failed
PASS: aligned case correctly succeeded
OK: scripts/check-spi-pin-sync.sh exhibits expected behavior
```

- [ ] **Step 3: Run the gate against the actual repo (which is currently aligned at v0.7.1 from PR-C1)**

```bash
./scripts/check-spi-pin-sync.sh
```

Expected: `check-spi-pin-sync: OK — all manifests pin cyoda-go-spi v0.7.1`

### Task 27: Add Makefile target

- [ ] **Step 1: Read current Makefile to find the right insertion point**

```bash
grep -n "^[a-z].*:" Makefile | head -20
```

- [ ] **Step 2: Append target to Makefile**

Add to `Makefile`:

```makefile
.PHONY: check-spi-pin-sync
check-spi-pin-sync:
	@./scripts/check-spi-pin-sync.sh
```

- [ ] **Step 3: Verify**

```bash
make check-spi-pin-sync
```

Expected: same OK message.

### Task 28: Wire into CI

- [ ] **Step 1: Read existing `.github/workflows/ci.yml`**

```bash
grep -n "^  [a-z].*:" .github/workflows/ci.yml | head -20
```

Identify where to add a new job.

- [ ] **Step 2: Append a `pin-sync` job**

Add to `.github/workflows/ci.yml` under `jobs:`:

```yaml
  pin-sync:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6.0.2

      - name: Verify SPI pin sync
        run: ./scripts/check-spi-pin-sync.sh
```

- [ ] **Step 3: Verify YAML parses**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml'))"
```

### Task 29: Update MAINTAINING.md

- [ ] **Step 1: Locate the right section**

```bash
grep -n "^##\|^###" MAINTAINING.md | head -30
```

- [ ] **Step 2: Add a "Bumping cyoda-go-spi" subsection**

Find the section that covers dependency hygiene during a release (or a sensible adjacent location), and append:

```markdown
### Bumping cyoda-go-spi

`cyoda-go-spi` must be pinned to the same version in every `go.mod` in
this repo: the root and all `plugins/*/go.mod`. The CI gate
`make check-spi-pin-sync` (workflow job `pin-sync`) enforces this and
will fail on PR if any manifest disagrees.

When you bump cyoda-go-spi:

1. Bump in `go.mod` (root).
2. Bump identically in `plugins/memory/go.mod`, `plugins/postgres/go.mod`, `plugins/sqlite/go.mod`.
3. Run `go mod tidy` in each module.
4. Run `make test-all` to verify cross-plugin interactions.
5. Run `make check-spi-pin-sync` locally to confirm green.

This rule is in addition to the existing **plugin-version lockstep**
rule (plugin submodule tags use the same version as the umbrella).
The two rules together ensure that consumers see consistent
SPI-and-plugin pinning at every umbrella tag.
```

### Task 30: Update CONTRIBUTING.md

- [ ] **Step 1: Add a one-line note**

Find a "Dependencies" or "Updating dependencies" section in `CONTRIBUTING.md` and append:

```markdown
- When bumping `cyoda-go-spi` in the root `go.mod`, bump it identically in every `plugins/*/go.mod` in the same PR. The `check-spi-pin-sync` CI gate enforces this. See [`MAINTAINING.md`](MAINTAINING.md#bumping-cyoda-go-spi) for the full procedure.
```

(If no existing section fits, add a new "Dependency hygiene" subsection.)

### Task 31: Update COMPATIBILITY.md (PR-C2 cells only)

- [ ] **Step 1: Read current matrix**

```bash
head -40 COMPATIBILITY.md
```

- [ ] **Step 2: Add a row for v0.7.1, filling only the cells known at this point**

Append a row to the version matrix. At PR-C2 time, the known cells are:

- cyoda-go v0.7.1
- cyoda-go-spi v0.7.1 (consumed via the bump landed in PR-C1)
- in-tree plugins v0.7.1 (will be tagged in Phase 4 / Task 36; record as planned)

The chart `appVersion` cell will only be definitive after the chart-bump PR merges in Task 39 — leave it as `v0.7.1` (the planned value) and note in the PR-C2 description that Task 39 will reconcile if reality diverges.

Replicate the formatting convention used by the existing rows in the file.

### Task 32: Race-detector run before PR

Per project memory, `go test -race` is end-of-deliverable, not per-step.

- [ ] **Step 1: Run race detector across the root module**

```bash
go test -race ./...
```

Expected: green.

- [ ] **Step 2: Run race detector in each plugin submodule**

```bash
(cd plugins/memory && go test -race ./...)
(cd plugins/sqlite && go test -race ./...)
(cd plugins/postgres && go test -race ./...)
```

Expected: green. (postgres requires Docker for testcontainers.)

### Task 33: Verification before completion

- [ ] **Step 1: Full test-all**

```bash
make test-all
go vet ./...
make check-spi-pin-sync
```

Expected: all green, gate prints OK.

- [ ] **Step 2: Soft gate 15 — deliberately break the gate to confirm it fires**

```bash
# Sabotage one plugin manifest temporarily
sed -i.bak 's/cyoda-go-spi v0.7.1/cyoda-go-spi v0.6.1/' plugins/sqlite/go.mod

./scripts/check-spi-pin-sync.sh
echo "exit=$?"

# Restore
mv plugins/sqlite/go.mod.bak plugins/sqlite/go.mod
```

Expected first run: gate exits 1 with a "FAIL — cyoda-go-spi pin drift detected" diff. After restore, `./scripts/check-spi-pin-sync.sh` returns OK.

### Task 34: Commit and open PR-C2

- [ ] **Step 1: Stage**

```bash
git add scripts/check-spi-pin-sync.sh scripts/check-spi-pin-sync_test.sh \
        Makefile .github/workflows/ci.yml \
        MAINTAINING.md CONTRIBUTING.md COMPATIBILITY.md
```

- [ ] **Step 2: Commit**

```bash
git commit -m "$(cat <<'EOF'
ci: add SPI pin-sync drift gate; document lockstep-bump rule

Adds:
- scripts/check-spi-pin-sync.sh: greps every go.mod in this repo
  and fails if cyoda-go-spi is pinned to more than one version.
- Makefile target check-spi-pin-sync.
- ci.yml job pin-sync running the gate on every push/PR.
- scripts/check-spi-pin-sync_test.sh: TDD harness exercising drift
  and aligned cases against scratch fixtures.

Documents:
- MAINTAINING.md: new "Bumping cyoda-go-spi" subsection making the
  lockstep-bump rule explicit (root + every plugin/*/go.mod in
  same PR; gate enforces).
- CONTRIBUTING.md: one-line note pointing at the procedure.
- COMPATIBILITY.md: v0.7.1 cross-repo row.

Spec: docs/superpowers/specs/2026-05-05-spi-release-hygiene-design.md.
Closes the recurrence vector for the SPI pin drift fixed in PR-C1.
EOF
)"
```

- [ ] **Step 3: Push and open PR-C2**

```bash
git push -u origin release-hygiene/drift-gate

gh pr create -R Cyoda-platform/cyoda-go \
  --title "ci: add SPI pin-sync drift gate; document lockstep-bump rule" \
  --base main \
  --body "$(cat <<'EOF'
## Summary

Adds `make check-spi-pin-sync` and a CI job that fails if root and `plugins/*/go.mod` disagree on the SPI pin. Documents the lockstep-bump rule in MAINTAINING.md and CONTRIBUTING.md. Updates COMPATIBILITY.md with the v0.7.1 cross-repo row.

Companion to PR-C1 (which corrected the actual drift). PR-C2 prevents recurrence.

The gate has been deliberately exercised against a sabotaged manifest to confirm it fires (soft gate 15 from the spec).

Spec: `docs/superpowers/specs/2026-05-05-spi-release-hygiene-design.md`.
Plan: `docs/superpowers/plans/2026-05-05-spi-release-hygiene.md`.

## Test plan

- [ ] `make test-all` green
- [ ] `go test -race ./...` green (root + each plugin submodule)
- [ ] `./scripts/check-spi-pin-sync_test.sh` passes
- [ ] CI `pin-sync` job runs and is green
- [ ] Reviewer mentally confirms the gate's failure mode reads sensibly (run `./scripts/check-spi-pin-sync.sh` after temporarily editing a plugin go.mod)
EOF
)"
```

- [ ] **Step 4: After CI green and review, merge**

---

## Phase 4: cyoda-go v0.7.1 release

This phase follows the existing `MAINTAINING.md` "Cutting a release" procedure, **but executes the lockstep plugin-tagging step that has not been honored for the previous five minor versions**. It is the first time MAINTAINING.md's plugin-tag-first procedure runs end-to-end.

Read `MAINTAINING.md` sections "Cutting a release" through "Publish the Helm chart" carefully before proceeding. The steps below summarize but defer authority to MAINTAINING.md if anything conflicts.

### Task 35: Refresh main and confirm at the release commit

- [ ] **Step 1: Refresh main**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/spi-release-hygiene
git fetch origin main
git checkout main 2>/dev/null || git checkout -b main origin/main
git pull --ff-only origin main
```

- [ ] **Step 2: Confirm at the commit you intend to release**

The commit at `HEAD` is what will be tagged `v0.7.1` (and `plugins/*/v0.7.1`). Verify that PR-C1 and PR-C2 are merged.

```bash
git log --oneline -10
make check-spi-pin-sync
```

### Task 36: Tag plugin submodules first (per MAINTAINING.md)

Per `MAINTAINING.md` § "1. Plugin submodule tags first", plugins must be tagged **before** the umbrella release tag, and at the **same** version number.

- [ ] **Step 1: Create three annotated, signed plugin tags**

```bash
V=v0.7.1
for plug in memory postgres sqlite; do
  git tag -s -a "plugins/$plug/$V" -m "Release plugins/$plug/$V

First lockstep plugin submodule tag in five minor versions
(legacy plugins/{memory,postgres}/v0.1.0; plugins/sqlite never
tagged before).

This tag pins cyoda-go-spi v0.7.1 and is published in lockstep
with cyoda-go umbrella v0.7.1 per MAINTAINING.md."
done

git tag -l "plugins/*/$V"
```

Expected: three tags listed.

- [ ] **Step 2: Verify all three are annotated+signed**

```bash
for plug in memory postgres sqlite; do
  echo "--- plugins/$plug/v0.7.1 ---"
  git verify-tag "plugins/$plug/v0.7.1"
done
```

- [ ] **Step 3: Push plugin tags**

```bash
GH_TOKEN_TARGET="${GH_TOKEN:-$(gh auth token)}"
git -c credential.helper="!f() { echo username=x-access-token; echo password=$GH_TOKEN_TARGET; }; f" \
    push origin "plugins/memory/v0.7.1" "plugins/postgres/v0.7.1" "plugins/sqlite/v0.7.1"
```

### Task 37: Drop replace directives, pin plugin modules at v0.7.1

Per `MAINTAINING.md` § "3. Drop the `replace` directives and pin plugin module versions", the umbrella release commit must drop the development-time `replace` directives so the release build resolves plugins to their tags.

- [ ] **Step 1: Refresh, branch, drop replaces**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/spi-release-hygiene
git checkout -b release-hygiene/v0.7.1-prep
go mod edit -dropreplace github.com/cyoda-platform/cyoda-go/plugins/memory
go mod edit -dropreplace github.com/cyoda-platform/cyoda-go/plugins/postgres
go mod edit -dropreplace github.com/cyoda-platform/cyoda-go/plugins/sqlite
go mod edit -require=github.com/cyoda-platform/cyoda-go/plugins/memory@v0.7.1
go mod edit -require=github.com/cyoda-platform/cyoda-go/plugins/postgres@v0.7.1
go mod edit -require=github.com/cyoda-platform/cyoda-go/plugins/sqlite@v0.7.1
go mod tidy
```

- [ ] **Step 2: Verify replaces are gone and plugin pins are at v0.7.1**

```bash
grep "replace " go.mod || echo "(no replaces — good)"
grep "cyoda-go/plugins" go.mod
```

Expected: no `replace` directives, plugin modules pinned at v0.7.1.

- [ ] **Step 3: Verify build still works under tag-resolved plugin modules**

```bash
go build -o /tmp/cyoda-build-check ./cmd/cyoda
go test ./... -short
```

Expected: green.

- [ ] **Step 4: Reconcile COMPATIBILITY.md v0.7.1 row**

Per `MAINTAINING.md` § "6.5. Update COMPATIBILITY.md": the v0.7.1 row was added during PR-C2 (Task 31) with planned values. Reconcile it now against reality:

- cyoda-go v0.7.1 ✓ (about to be tagged in Task 38)
- cyoda-go-spi v0.7.1 ✓ (in go.mod after PR-C1)
- in-tree plugins v0.7.1 ✓ (tagged in Task 36)
- chart appVersion ✓ (will be reconciled after Task 39's chart-bump PR merges; if it differs from planned, follow up with a one-line PR fixing the cell)

- [ ] **Step 5: Commit prep**

```bash
git add go.mod go.sum COMPATIBILITY.md
git commit -m "$(cat <<'EOF'
chore: drop replace directives; pin plugin modules at v0.7.1

Per MAINTAINING.md "Cutting a release" procedure, drops the
dev-time replace directives for plugins/{memory,postgres,sqlite}
and pins each at v0.7.1. The plugin submodule tags were created
in a preceding commit (plugins/<name>/v0.7.1).

Also fills v0.7.1 cross-repo row in COMPATIBILITY.md.

The umbrella v0.7.1 tag is created and pushed after this commit
merges to main.
EOF
)"
```

- [ ] **Step 6: Push and open the prep PR**

```bash
git push -u origin release-hygiene/v0.7.1-prep
gh pr create -R Cyoda-platform/cyoda-go \
  --title "chore(release): drop replaces, pin plugins at v0.7.1, COMPATIBILITY row" \
  --base main \
  --body "Release-prep for v0.7.1. After merge, the umbrella v0.7.1 tag is pushed."
```

- [ ] **Step 7: After CI green, merge**

### Task 38: Tag and push umbrella v0.7.1

- [ ] **Step 1: Refresh main**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/spi-release-hygiene
git checkout main && git pull --ff-only origin main
```

- [ ] **Step 2: Tag**

```bash
git tag -s -a v0.7.1 -m "Release v0.7.1

Patch release closing release-day hygiene gaps:
- plugins/{memory,postgres,sqlite}/go.mod now pin cyoda-go-spi v0.7.1
  (was v0.6.1; behavior previously masked locally via Go MVS).
- New scripts/check-spi-pin-sync.sh and CI gate prevent recurrence.
- Plugin submodule tags plugins/{memory,postgres,sqlite}/v0.7.1
  created (closes the five-version gap; sqlite gets its first
  ever tag).
- MAINTAINING.md amended with SPI lockstep-bump rule; existing
  plugin-version lockstep rule preserved."
```

- [ ] **Step 3: Verify**

```bash
git verify-tag v0.7.1
```

- [ ] **Step 4: Push**

```bash
GH_TOKEN_TARGET="${GH_TOKEN:-$(gh auth token)}"
git -c credential.helper="!f() { echo username=x-access-token; echo password=$GH_TOKEN_TARGET; }; f" \
    push origin v0.7.1
```

- [ ] **Step 5: Watch `release.yml` run**

```bash
gh run list -R Cyoda-platform/cyoda-go --workflow=release.yml --limit=3
gh run watch -R Cyoda-platform/cyoda-go --exit-status
```

If `release.yml` fails: pause, surface the error, do **not** delete or move the tag. Diagnose and resolve via a follow-up patch tag (v0.7.2) if necessary.

### Task 39: Merge auto-opened chart-bump PR

Per project memory, the chart-bump PR is intentionally human-gated and not auto-merged.

- [ ] **Step 1: Find the auto-opened chart-bump PR**

```bash
gh pr list -R Cyoda-platform/cyoda-go --search "appVersion v0.7.1" --state open
```

- [ ] **Step 2: Review and merge**

Read the diff, confirm the chart bumps `version:` and `appVersion:` correctly per `MAINTAINING.md` chart-publish guidance, merge.

- [ ] **Step 3: Confirm chart publishes**

```bash
gh run list -R Cyoda-platform/cyoda-go --workflow=release-chart.yml --limit=3
```

Expected: green run for v0.7.1 chart.

---

## Phase 5: cyoda-go-cassandra notification

### Task 40: Notify cassandra maintainers

- [ ] **Step 1: Open a notification issue or comment in cassandra**

```bash
gh issue create -R Cyoda-platform/cyoda-go-cassandra \
  --title "Heads up: cyoda-go-spi v0.7.1 available; consider bumping" \
  --body "$(cat <<'EOF'
Hello — opening this as a courtesy notification per the new
[KNOWN_CONSUMERS.md](https://github.com/Cyoda-platform/cyoda-go-spi/blob/main/KNOWN_CONSUMERS.md)
etiquette in cyoda-go-spi.

**What changed:** cyoda-go-spi v0.7.1 is the first annotated+signed tag and anchors a new release-day hygiene regime documented in [MAINTAINING.md](https://github.com/Cyoda-platform/cyoda-go-spi/blob/main/MAINTAINING.md). v0.7.1 has **no API change** vs v0.7.0 — it is metadata-only.

**Why mention it:** your `go.mod` pins `cyoda-go-spi v0.6.0`. Bumping to v0.7.1 closes a two-minor-version lag and aligns you with cyoda-go's own pin (also v0.7.1 as of this release). The bump should be a one-line `go mod edit` + `go mod tidy`. We're flagging it; we're not blocking on it — your release cadence is yours.

If you'd like a draft PR, ping us and we'll open one. If you'd like to be subscribed for future SPI releases, your repo is already in `KNOWN_CONSUMERS.md`.

(Filed by cyoda-go release v0.7.1.)
EOF
)"
```

- [ ] **Step 2: Optionally draft the bump PR**

If you have time and the cassandra maintainers consent:

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-cassandra
git fetch origin
git checkout -b bump/cyoda-go-spi-v0.7.1 origin/main
go mod edit -require=github.com/cyoda-platform/cyoda-go-spi@v0.7.1
go mod tidy
go test ./...
git add go.mod go.sum
git commit -m "chore(deps): bump cyoda-go-spi to v0.7.1"
git push -u origin bump/cyoda-go-spi-v0.7.1
gh pr create -R Cyoda-platform/cyoda-go-cassandra \
  --title "chore(deps): bump cyoda-go-spi to v0.7.1" \
  --body "Drafted as a courtesy. v0.7.1 has no API change vs v0.7.0; metadata-only. See https://github.com/Cyoda-platform/cyoda-go-spi/issues for the regime change."
```

- [ ] **Step 3: Either way, link the issue/PR back**

Comment on (or amend) the cyoda-go v0.7.1 release notes and `KNOWN_CONSUMERS.md` if appropriate to reflect that cassandra has been notified.

---

## Final verification (success criteria from spec)

### Task 41: Verify all hard gates

- [ ] **Step 1: SPI side hard gates**

```bash
gh api repos/Cyoda-platform/cyoda-go-spi/contents/.github >/dev/null && echo "✓ .github/ exists"

cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git fetch origin --tags
git verify-tag v0.7.1 && echo "✓ v0.7.1 annotated+signed"

[[ -f CHANGELOG.md ]] && grep -q "^## \[0.7.1\]" CHANGELOG.md && echo "✓ CHANGELOG v0.7.1 row"
[[ -f MAINTAINING.md ]] && grep -q "Fixing forward" MAINTAINING.md && echo "✓ MAINTAINING.md with fix-forward"
[[ -f KNOWN_CONSUMERS.md ]] && grep -q "cyoda-go-cassandra" KNOWN_CONSUMERS.md && echo "✓ KNOWN_CONSUMERS.md seeded"
[[ -f spitest/README.md ]] && grep -q "spi-head-conformance" spitest/README.md && echo "✓ spitest README + snippet"
```

- [ ] **Step 2: cyoda-go side hard gates**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/spi-release-hygiene
distinct=$(grep "cyoda-go-spi v" go.mod plugins/*/go.mod | awk '{print $NF}' | sort -u | wc -l | tr -d ' ')
[[ "$distinct" == "1" ]] && echo "✓ SPI pin sync: 1 distinct version"

make check-spi-pin-sync && echo "✓ make check-spi-pin-sync green"

git tag -l "plugins/*/v0.7.1" | wc -l | grep -q "^[[:space:]]*3$" && echo "✓ 3 plugin v0.7.1 tags"

git verify-tag v0.7.1 && echo "✓ cyoda-go v0.7.1 annotated+signed"
gh run list -R Cyoda-platform/cyoda-go --workflow=release.yml --limit=1 --json conclusion --jq '.[0].conclusion' | grep -q "success" && echo "✓ release.yml green"
```

- [ ] **Step 3: Soft gates**

For each soft gate, confirm by inspection:

- Soft gate 13 (fix-forward reads as current-state-and-intent): re-read `cyoda-go-spi/MAINTAINING.md` § "Fixing forward". If it reads as a hedge, revise.
- Soft gate 14 (conformance snippet works): already validated in Task 15.
- Soft gate 15 (drift gate fires when sabotaged): already exercised in Task 33.
- Soft gate 16 (cassandra team notified): confirmed in Task 40.

### Task 42: Close out

- [ ] **Step 1: Confirm the worktree branch is in a known state**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/spi-release-hygiene
git status
```

Expected: clean working tree on main (after release-hygiene/* PRs merged and main is pulled).

- [ ] **Step 2: Per project memory ("close release-branch issues at merge time")**

This is not a release branch (direct-to-main), but if any GitHub issues link to this work as `Closes #N`, manually close them or confirm they're auto-closed by the merge commits.

- [ ] **Step 3: Confirm spec and plan are on `main`**

The Pre-phase docs PR landed both files on `main`. Confirm:

```bash
git log --oneline main -- docs/superpowers/specs/2026-05-05-spi-release-hygiene-design.md docs/superpowers/plans/2026-05-05-spi-release-hygiene.md
```

Expected: at least one commit per file. If empty, the Pre-phase docs PR was missed — open it now.

---

## Risk reminders during execution

- **Never force-move a tag.** v0.1.0–v0.7.0 (SPI) and `plugins/{memory,postgres}/v0.1.0` (cyoda-go) stay as legacy artifacts. If v0.7.1 needs to be re-cut, use v0.7.2 instead.
- **Phase 2 must complete before Phase 3a starts.** PR-C1 pulls SPI v0.7.1 from the proxy; if the tag isn't published the bump will fail.
- **Phase 4 is the first end-to-end exercise of MAINTAINING.md's lockstep plugin-tagging in five versions.** Expect rough edges. Pause if `release.yml` fails; do not delete the tag — diagnose and patch with v0.7.2.
- **Plugin tags `plugins/*/v0.7.1` jump from v0.1.0** (or no tag for sqlite). HEAD-consumers of those submodule paths get a new SPI version. The CHANGELOG and tag annotations document the jump.
