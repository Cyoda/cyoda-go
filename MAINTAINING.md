# Maintaining cyoda-go

Notes for cyoda-go maintainers on tasks that aren't part of the regular
development workflow.

## One-time setup: Homebrew tap release automation

Before the first `v*` tag triggers the GoReleaser Homebrew-publishing
job, these steps must be completed once.

### 1. Create the empty tap repository

- New repo: `cyoda/homebrew-cyoda-go` (public, empty).
- `README.md` in the tap repo: a short paragraph explaining the tap
  and linking back to this main repo. GoReleaser will push `cyoda.rb`
  on every release.

### 2. Create the GitHub App

A GitHub App (not a personal access token) mints short-lived
installation tokens for the release workflow. Advantages over a PAT:
org-owned, no human account attached, no expiration to track, audit
trail is clean.

1. Navigate to `https://github.com/organizations/cyoda/settings/apps`.
2. Click **New GitHub App**.
3. Fill in:
   - App name: `cyoda-platform-release-bot` (must be globally unique
     across all GitHub Apps; add a suffix if taken).
   - Homepage URL: `https://github.com/cyoda/cyoda-go`
   - Webhook: uncheck **Active** (no webhook needed).
   - Permissions → **Repository permissions**:
     - **Contents**: Read and write
   - Permissions → **Account permissions**: (leave all unset)
   - Where can this GitHub App be installed?: **Only on this account**.
4. Click **Create GitHub App**.
5. After creation, note the numeric **App ID** at the top of the App
   settings page (typically 6–7 digits).
6. Scroll to **Private keys** and click **Generate a private key**. A
   `.pem` file downloads to your browser — keep it for the next step.

### 3. Install the App on the tap repo

1. On the App settings page, click **Install App** in the left sidebar.
2. Choose the `cyoda` org.
3. Under **Repository access**, select **Only select repositories** and
   add `cyoda/homebrew-cyoda-go`. Do NOT install on the whole
   org — the App's scope must be minimal.
4. Click **Install**.

### 4. Configure secrets in the cyoda-go repo

1. Navigate to `https://github.com/cyoda/cyoda-go/settings/secrets/actions`.
2. Add secret `HOMEBREW_TAP_APP_ID`: the numeric App ID from step 2.5.
3. Add secret `HOMEBREW_TAP_APP_KEY`: the full contents of the `.pem`
   file from step 2.6, including the `-----BEGIN PRIVATE KEY-----`
   and `-----END PRIVATE KEY-----` lines.
4. Delete the local `.pem` file from your machine. The private key
   only needs to live in the Actions secret now.

### 5. Verify

On the next non-prerelease `v*` tag push, the release workflow's
**Generate Homebrew tap token** step mints a short-lived installation
token, GoReleaser uses it to push `cyoda.rb` to `homebrew-cyoda-go`,
and the tap repo's commit history shows `cyoda-platform-release-bot`
as the commit author.

If the step fails with a 401: check that the App is installed on the
tap repo (step 3), and that `HOMEBREW_TAP_APP_ID` / `HOMEBREW_TAP_APP_KEY`
are both set.

## Key rotation

If the private key is compromised or simply needs rotation:

1. App settings → **Private keys** → **Generate a private key** for a
   new key.
2. Immediately update `HOMEBREW_TAP_APP_KEY` in the cyoda-go repo
   secrets with the new `.pem` contents.
3. App settings → delete the old private key.
4. Delete the local `.pem` from your machine.

No release-workflow code changes are needed — the App ID is stable
across rotations.

## Cutting a release

Releases are **intentional**, not per-merge. Milestone PRs merge into the
release branch `release/vX.Y.Z`. When you decide to cut a release, the
release PR merges that branch into `main`, and a pushed `v*` tag fires
`release.yml`, which builds artifacts, signs, and publishes.

### Release at a glance

The numbered steps below hold the commands. This table gives the order.
**Preparer** is anyone preparing the release. **Key holder** is a maintainer
with the signing key ([Who can cut a tag](#who-can-cut-a-tag)); the preparer
gives the key holder each tag command with the exact SHA. Start a row only
when the check of the row before it passes.

| # | Step | Who | Where | Merge | Check | Details |
|---|---|---|---|---|---|---|
| 1 | SPI: apply pending Dependabot bumps, rename `CHANGELOG.md` `[Unreleased]` to `[X.Y.Z] - <date>`, push | Preparer | SPI `main`, or SPI `release/vX.Y.Z` for a patch while the next minor is open | — | SPI CI green on that commit | [Coordinated release](#coordinated-release-across-sibling-repos) 1 |
| 2 | SPI tag | Key holder | the commit of row 1 | — | tag signed: `git cat-file -p <tag>` shows `BEGIN SSH SIGNATURE` | same |
| 3 | SPI GitHub Release | Preparer | the SPI tag | — | the Go proxy and `sum.golang.org` serve the tag | same |
| 4 | Release-prep commit: SPI pin, Dependabot bumps, tidy, `dependabot.yml` `target-branch` (minor only), `CHANGELOG.md`, release notes, `COMPATIBILITY.md` | Preparer | `release/vX.Y.Z` | — | `make check-spi-pin-sync`, `make test`, CI green | [Gate 0](#0-reconcile-dependencies-on-the-release-branch-gate), [6.5](#65-update-compatibilitymd) |
| 5 | Release PR `release/vX.Y.Z` → `main` | Preparer | PR | merge commit | CI green on the PR head | [Merging a release PR](#merging-a-release-pr) |
| 6 | Three plugin tags | Key holder | the release merge commit on `main` | — | each tag signed | [1](#1-plugin-submodule-tags-first) |
| 7 | Pin commit: root `go.mod` requires the plugin tags | Preparer | pushed to `main` | — | CI green on that commit | [3](#3-pin-plugin-module-versions-to-the-release-tags), [5](#5-verify-ci-is-green) |
| 8 | Binary tag | Key holder | the pin commit | — | `release.yml` green, first `Verify release` run green, Latest mark on the new release | [7](#7-cut-the-release), [9](#9-verify-the-published-release-gate) |
| 9 | Chart PR: push chart `version:` and the `COMPATIBILITY.md` chart row to the bot's PR | Preparer | PR | squash | checks green on the pushed head | [8](#8-publish-the-helm-chart) |
| 10 | Chart tag `cyoda-X.Y.Z` | Key holder | the chart squash-merge commit on `main` | — | `release-chart.yml` green, second `Verify release` run green | [8](#8-publish-the-helm-chart), [9](#9-verify-the-published-release-gate) |
| 11 | Announcement: post the release notes as a discussion in Announcements | Preparer | GitHub Discussions | — | the discussion shows the notes | [9.5](#95-announce-the-release) |
| 12 | Open the next release branch, close the milestone | Preparer | `main` | — | the `target-branch` check prints the branch | [10](#10-open-the-next-release-branch) |

A patch release while the next minor is open changes rows 4 and 12 and adds a
wrap-up with two forward merges (merge commits, never squash): see
[Patch release while the next minor is open](#patch-release-while-the-next-minor-is-open).
`cyoda-go-cassandra` follows in its own repo
([Coordinated release](#coordinated-release-across-sibling-repos) 3).

### Merging a release PR

Merge release PRs through the REST endpoint, not `gh pr merge`. `gh pr merge`
uses the GraphQL API, and with `--delete-branch` it switches the local checkout
to the base branch, which fails when a worktree holds that branch.

```bash
# release PR and forward merges: merge commit
gh api -X PUT repos/cyoda/cyoda-go/pulls/<n>/merge -f merge_method=merge
# chart PR: squash
gh api -X PUT repos/cyoda/cyoda-go/pulls/<n>/merge -f merge_method=squash
```

| PR | Method | Why |
|---|---|---|
| Release PR `release/vX.Y.Z` → `main` | merge commit | The plugin tags go on the merge commit, and the release branch's commits, which the milestone's pseudo-version pins name, stay reachable from `main`. |
| Chart PR | squash | One commit to tag. |
| Forward merge after a patch release | merge commit, never squash | The patch tag must become an ancestor of the open branch ([why](#patch-release-while-the-next-minor-is-open)). |

**After a 5xx or `405 Merge already in progress`, stop sending merge calls.**
A 5xx does not mean that the merge failed. A stuck merge job on GitHub's side
can hold the PR's merge lock for an hour, and while it does, every merge call
fails. Instead:

1. Check whether the merge happened:
   ```bash
   gh api repos/cyoda/cyoda-go/pulls/<n> -q '.merged, .merged_at'
   git ls-remote origin refs/heads/<base>
   ```
2. If the PR is still unmerged, wait a few minutes and check again.
3. Retry once. A `405 Merge already in progress` means that the lock is
   still held: go back to 1. Never retry in a loop.
4. If the lock does not clear, merge in the web UI.

### Versioning

Semver with a leading `v`: `v0.6.0`, `v1.2.3`. Pre-v1, the **minor
component is the breaking-change signal**: bump `0.MINOR.0` for a
backward-incompatible change to that module's public contract, and
`0.x.PATCH` for any backward-compatible change — **including new
features**. Additive API parameters, new endpoints, new optional SPI
fields, and bug fixes all ship as patches. The discipline this rests
on: **a breaking change never ships in a patch.** (This is the
"leftmost non-zero component is the de-facto major" convention; see
[`README.md`](README.md#versioning) for the consumer-facing statement.)
`cyoda-go-spi` `v0.8.3`, `v0.8.4` and `v0.9.1` are the recorded exceptions;
see [Bumping cyoda-go-spi](#bumping-cyoda-go-spi).

New version must be **strictly greater** than the previous tag — Go
module tags are write-once-per-value on the proxy (`proxy.golang.org`
caches the SHA of every tag it serves), so you cannot retag or reuse a
version number, even on repos with no production consumers.

**Each module versions on its own axis.** `cyoda-go-spi`, the
`cyoda-go` binary, the in-tree plugins, and the Helm chart are **not**
required to share a version number, and the binary must **never** skip
or burn a version number merely to mirror the SPI's (as `v0.8.0` was
once skipped — see [`COMPATIBILITY.md`](COMPATIBILITY.md)). The binary
pins whatever SPI version it needs and versions independently; their
compatible combinations are recorded in the compatibility matrix, not
encoded in a shared digit.

"Greenfield" means we promise nothing about backward compatibility
between versions yet. It does **not** mean we can reset version
numbers — the one-way-ratchet applies from the first tag onward.

#### The never-re-cut rule (non-negotiable)

A module version is **tagged exactly once, at the final verified commit, and
never re-cut.** This follows directly from the write-once property above, and
it is absolute:

1. **Never tag speculatively or mid-milestone.** The instant a tag is pushed
   and anything fetches it, `proxy.golang.org` and `sum.golang.org` bind that
   version to that commit **permanently and globally**. There is no cache-bust.
2. **A bad or premature tag is recovered ONLY by the next version number.**
   Deleting the git tag does *not* clean the proxy/checksum-database binding —
   it just creates a mismatch between git and the proxy. Re-cutting the same
   version at a new commit produces a permanently-poisoned version that every
   consumer must then bypass with `GOPRIVATE` forever. **Don't.** Skip to the
   next patch/minor and tag that, cleanly, once.
3. **Pre-tag gate:** before pushing any `cyoda-go-spi` / plugin / binary tag,
   confirm the milestone work is fully merged and CI is green on the exact
   commit being tagged. A tag is a publish, not a checkpoint.

> **2026-06 incident (why this rule is in caps):** `cyoda-go-spi v0.8.0` was
> tagged prematurely at an incomplete commit, fetched through the proxy, then
> "retracted" and re-cut at the finished commit. The proxy still served the
> premature commit for `v0.8.0`, so every build broke until `GOPRIVATE` was
> wired into every Go context — and even then the public version stayed
> poisoned. The clean fix was to abandon `v0.8.0` and release `v0.8.1` (fresh,
> never-seen) for both the SPI and the binary. Cost: a burned version number
> and a day of churn. Prevention: this rule.

### Who can cut a tag

**Every release tag in all four namespaces is annotated and SSH-signed**, and
only a maintainer holding the signing key can create one:

| Namespace | Example | Fires |
|---|---|---|
| `cyoda-go-spi` | `v0.8.4` | nothing; the tag *is* the release |
| Binary | `v0.8.4` | `release.yml` |
| Plugin submodules | `plugins/postgres/v0.8.4` | nothing; consumed by the root pin |
| Helm chart | `cyoda-0.8.4` | `release-chart.yml` |

A full coordinated release therefore needs **six signed tags**: one SPI, three
plugin submodules, one binary, one chart.

This is not a formality. `cyoda-go-spi`'s `CHANGELOG.md` states the commitment
in prose, and a Go module version is bound to its commit permanently the first
time the proxy serves it — an unsigned tag cannot be corrected afterwards
without burning the version number. Anyone preparing a release who cannot sign
should get the branch to a verified-green commit and then hand the exact
`git tag -s -a … <sha>` command to someone who can, rather than substituting
`gh release create` or a lightweight tag.

### 0. Reconcile dependencies on the release branch (gate)

**Dependency hygiene is a release gate, not post-release cleanup.** Before
any tag is cut, the release branch must already be at the dependency versions
you intend to ship — otherwise the release goes out stale and Dependabot
re-raises the deferred bumps immediately after, manufacturing technical debt
at the moment of release.

Do this as a single consolidated **release-prep** commit on the release branch,
*after* the coordinated `cyoda-go-spi` tag exists (so the pin resolves) but
*before* the merge-to-main and tag:

1. Bump the `cyoda-go-spi` pin to the freshly cut `vX.Y.Z` in all four
   `go.mod` files (this is the maintainer-owned step — Dependabot does **not**
   touch the SPI pin; see "Bumping cyoda-go-spi" below). This *swaps out* the
   pseudo-version pin the branch carries during the milestone: "after the tag
   exists" governs this release-prep swap, not whether a feature branch can
   compile before a tag.
2. Apply every pending Dependabot minor/patch bump (root + each plugin
   submodule). If a grouped PR was closed because it was entangled with the
   then-unresolvable SPI pin, apply its third-party updates by hand here.
   Re-read each PR's current versions (`gh pr diff <n>`) before you apply it
   or close it as superseded: Dependabot moves its open PRs to newer
   releases, so the versions can differ from the ones it first proposed.
3. `go mod tidy` in every module.
4. Point `.github/dependabot.yml`'s `target-branch` at the **next** release
   branch (`release/vX.Y+1.0`), every entry together. The branch itself is
   opened in step 10, from `main` after the chart merge. Skip this for a
   [patch release while the next minor is open](#patch-release-while-the-next-minor-is-open).
5. Stamp `CHANGELOG.md` `[Unreleased]` as `[X.Y.Z] — YYYY-MM-DD`, add
   `docs/release-notes/vX-Y-Z.md`, and add the `COMPATIBILITY.md` row
   ([6.5](#65-update-compatibilitymd)). The date, in both files, is the day
   the binary tag will be cut. If the tag lands on another day, correct both
   on the open release branch before the announcement
   ([9.5](#95-announce-the-release)).
6. Verify: `make check-spi-pin-sync` and `make test` locally, then push.
   CI runs on every push to `release/**` and is the full-suite gate (CLAUDE.md
   Gate 5): do not also run `make test-full` or `make race` locally.

After this commit the branch is at latest on everything, so Dependabot's next
run finds nothing to raise — a clean release with no immediate follow-on churn.

### 1. Plugin submodule tags first

Plugin modules (`plugins/memory`, `plugins/postgres`, `plugins/sqlite`)
live in this repo but have their own `go.mod` files and their own
tag namespaces (`plugins/<name>/vX.Y.Z`). They must be tagged
**before** the root module's `go.mod` can pin them — otherwise
`go mod tidy` at step 3 has nothing to resolve against.

Tag each plugin submodule at the release's merge commit on `main`. The
root tag goes on the next commit, the one step 3 makes to pin these tags.
Pick the same version number as the root tag to keep the mental model
simple:

```bash
# pick per release
V=v0.6.0
# the release merge commit on main
SHA=<release merge commit>
for p in memory postgres sqlite; do
  git tag -s -a "plugins/$p/$V" -m "Release plugins/$p/$V" "$SHA"
done
git push origin "plugins/memory/$V" "plugins/postgres/$V" "plugins/sqlite/$V"
```

Signed and annotated, like every other release tag — see
[Who can cut a tag](#who-can-cut-a-tag).

### 3. Pin plugin module versions to the release tags

The root `go.mod` pins the three plugin submodules. During the pre-release
window it pins them to a **pseudo-version of the in-dev HEAD** (set with
`make repin-plugins`; see the window note below). At release cut, re-pin them
to the tags you cut in step 1:

```bash
# the root release version
V=v0.6.0
for p in memory postgres sqlite; do
  go mod edit -require="github.com/cyoda/cyoda-go/plugins/$p@$V"
done
# refresh go.sum against the tagged modules
GOWORK=off go mod download
GOWORK=off go mod tidy
```

Review the diff — the `require` block should pin each plugin at the release
tag, replacing the dev-window pseudo-versions.

```bash
git add go.mod go.sum
git commit -m "chore: pin plugin modules at $V"
git push origin main
```

There are **no** `replace` directives to drop — the repo composes locally via
`go.work`, not `replace` (the release workflow's pre-flight rejects any
`replace`).

**Dev-window plugin composition (`make repin-plugins`).** When the root depends
on plugin code that isn't tagged yet — e.g. an SPI change adds a **required**
method to a plugin-implemented interface (`StoreFactory`, `EntityStore`) that
the old plugin tags don't implement — the `test` job stays green via `go.work`
(local plugins), but the `GOWORK=off` jobs (`per-module-hygiene`,
`smoke`/goreleaser) resolve the **root `go.mod` pins as a consumer would**, so
those pins must point at the in-dev plugin code. On the feature branch, after
pushing your plugin changes, run **`make repin-plugins`** to pseudo-version-pin
all three plugins to HEAD and tidy the root module, then commit. The SPI is pinned
the same way (see "Bumping cyoda-go-spi"). Pseudo-pins are used deliberately
rather than `replace` directives: they keep the `GOWORK=off` jobs a genuine
consumer-resolution test, whereas `replace` (local dirs) would make those jobs
a redundant copy of the `go.work` `test` job — and would have to be unwound at
release cut anyway. Forgetting this step is the classic failure mode: the SPI
gets pseudo-pinned but the plugins are left at the old tags, and only the
`GOWORK=off` jobs go red.

### 4. Homebrew tap setup (one-time)

Nothing to do per release once the tap exists. For a new tap, see
"One-time setup: Homebrew tap release automation" above.

### 5. Verify CI is green

Push the step 3 pin commit to `main` and confirm that CI is green on that
exact commit — every job, including `per-module-hygiene`, which resolves the
new plugin pins as a consumer would. Don't push the release tag if CI is
red.

### 6. Image signing

`docker_signs` in `.goreleaser.yaml` (`artifacts: images`) signs each
tag's multi-platform image: the signature is on the index digest a tag
names, not on the per-arch manifests, so verify a tag or its index digest. The
`release-smoke.yml` job checks on every PR touching `.goreleaser.yaml` or
the Dockerfile that this selector resolves to at least one artifact, so
there is no manual step before the tag. After the release, step 9
verifies the published image.

### 6.5. Update COMPATIBILITY.md

Add a row to the `cyoda-go × cyoda-go-spi` matrix in [`COMPATIBILITY.md`](./COMPATIBILITY.md) for the new `cyoda-go` tag. Capture the SPI pin (pin-sync keeps the root and plugin pins equal) and a one-line summary of the SPI surface added in this release (or `—` if binary-only).

If the chart `version:` or `appVersion:` changed in this cycle, update the "Helm chart × binary" table.

If out-of-tree-plugin guidance changed (e.g. cassandra adopted a new SPI pin), update the "Out-of-tree plugins" table.

The update lands in the release-prep commit (gate 0).

### 7. Cut the release

**Push the signed tag; `release.yml` creates the Release.** GoReleaser
publishes the GitHub Release from the tag (`release:` in `.goreleaser.yaml`),
so there is no `gh release create` step for the binary. Never create the
Release before the tag exists: `gh release create` on a missing tag creates
one server-side, and that tag is **lightweight and unsigned**. That is how
`v0.8.1` and `v0.8.2` shipped unsigned after the project had already
committed to signing every tag.

```bash
# or whatever this release is
V=v0.6.0

# The tag. Signed and annotated, at the exact verified commit.
git tag -s -a "$V" -m "Release $V" <sha>
git push origin "$V"
```

Confirm the tag really is signed before moving on — a published version
cannot be re-cut:

```bash
git cat-file -p "$V" | grep -q "BEGIN SSH SIGNATURE" && echo signed
```

The changelog is **not** the GitHub Release. `CHANGELOG.md` is maintained
per release, and the narrative notes live in
`docs/release-notes/v<X-Y-Z>.md`; they are published to the docs site and as
the release announcement ([9.5](#95-announce-the-release)). Both are written
before the tag, as part of the release-prep commit.

Pushing the tag fires `release.yml`:
pre-flight module verification, build binaries, multi-arch image to
GHCR, keyless cosign signing, SBOM attachment, Homebrew formula
commit to the tap, GitHub Release artifacts attached.

The workflow runs ~5 minutes. Watch it in the Actions tab and
verify:

- Release appears on the Releases page with all expected archives,
  `.deb`/`.rpm` packages, `SHA256SUMS`, cosign signatures, SBOMs.
- `ghcr.io/cyoda/cyoda:${V#v}` and `:latest` manifests exist. **Note the stripped `v`**: `.goreleaser.yaml` tags images from `{{ .Version }}`, which is the bare version, so `:v0.8.4` does not exist and `:0.8.4` does. Checking the prefixed form reports a good release as a failed one. The chart agrees with the registry here — it builds its image reference from `.Chart.AppVersion`, which is also unprefixed.
- `cyoda/homebrew-cyoda-go` shows a new commit updating `cyoda.rb` (at the
  tap's root) to `$V`.
- A PR titled `chore(helm): bump chart appVersion to $V` (labels:
  `helm`, `chart-release`) is opened automatically by
  `bump-chart-appversion.yml`. Proceed to step 8 to publish the chart.

The SPI has no build workflow, so its Release is still made by hand, after its
tag exists, with `gh release create $V --verify-tag` (the full command is in
"Coordinated release across sibling repos").

### Coordinated release across sibling repos

Some features span `cyoda-go-spi`, `cyoda-go`, and `cyoda-go-cassandra`.
The dependency chain is:

```
cyoda-go-spi   ← cyoda-go   ← cyoda-go-cassandra
              ← cyoda-go-cassandra
```

Release in topological order:

1. **`cyoda-go-spi`**: on the branch the release is cut from (`main`, or
   `release/vX.Y.Z` for a patch while the next minor is open), apply the
   pending Dependabot bumps, rename the `[Unreleased]` changelog section to
   `[X.Y.Z] - YYYY-MM-DD`, push, and
   wait for CI to pass on that commit. The key holder tags that commit. Only
   then create the GitHub Release. No build workflow fires; the release is
   the tag itself.
   ```bash
   # this SPI release and the previous one
   V=v0.9.1
   PREV=v0.9.0
   # key holder
   git tag -s -a "$V" -m "Release $V" <sha>
   git push origin "$V"
   # preparer, after the tag exists
   gh release create "$V" --repo cyoda/cyoda-go-spi --verify-tag \
     --generate-notes --notes-start-tag "$PREV" --latest
   ```
   `--notes-start-tag` makes the notes cover exactly the changes since
   `$PREV`. `--latest` puts the Latest mark on this release. Then check that
   the Go proxy and the checksum database serve the tag; the first command
   prints the version, the second its hashes:
   ```bash
   GOWORK=off go list -m "github.com/cyoda/cyoda-go-spi@$V"
   curl -fsS "https://sum.golang.org/lookup/github.com/cyoda/cyoda-go-spi@$V"
   ```

2. **`cyoda-go`**: the release-prep commit on `release/vX.Y.Z` pins the new
   SPI tag in all four `go.mod` files ([gate 0](#0-reconcile-dependencies-on-the-release-branch-gate)).
   Remove any local `go.work` `use` entry for the SPI checkout first; never
   commit it. Then follow [Release at a glance](#release-at-a-glance) from
   row 5: release PR, plugin tags (step 1), pin commit (step 3), binary tag
   (step 7), chart (step 8).

3. **`cyoda-go-cassandra`**: on its feature branch, bump `go.mod`'s
   `cyoda-go-spi`, `cyoda-go`, and `cyoda-go/plugins/*` require
   lines to the new versions. Merge, then signed tag first and
   `gh release create $V --verify-tag ...` after it.

Each step waits for the prior tag to land before its `go mod tidy`
can resolve. In practice: merge, tag, wait for CI, then move on.

### Patch release while the next minor is open

A patch `vX.Y.Z` of the released line can ship while the next minor's branch
`release/vX.Y+1.0` is open. `main` still holds the last release, so the patch
starts from there. The examples use `V=v0.9.1`, `PREV=v0.9.0` (the previous
SPI tag) and `V_NEXT=v0.10.0` (the open minor).

**Branches.**

| Repo | Patch branch | Created from | Fix PRs target |
|---|---|---|---|
| `cyoda-go` | `release/$V` | `main` | `release/$V` |
| `cyoda-go-spi` | `release/$V` | the previous SPI tag `$PREV` | `release/$V` |

```bash
# cyoda-go
git fetch origin
git branch "release/$V" origin/main
git push origin "release/$V"
# cyoda-go-spi
git fetch origin --tags
git branch "release/$V" "$PREV"
git push origin "release/$V"
```

CI runs on `release/**` in both repos, so PRs into the patch branches get
checks.

**During the window.**

1. Each fix merges into the patch branch first.
2. Forward-port each fix as soon as it merges: cherry-pick it onto a branch
   from `release/$V_NEXT` (cyoda-go) or SPI `main`, and open a PR there.
   ```bash
   git switch -c "port/<topic>" "origin/release/$V_NEXT"
   git cherry-pick -x <sha>
   ```
   Conflicts are usually only in `CHANGELOG.md`, `COMPATIBILITY.md` and the
   `go.mod`/`go.sum` files. Keep the open branch's pins, then re-pin and tidy.
3. cyoda-go on `release/$V` pins a commit on SPI `release/$V`
   ([Bumping cyoda-go-spi](#bumping-cyoda-go-spi)).

**Changes to the numbered steps.** All other steps run unchanged, including
the release PR `release/$V` → `main` with a merge commit.

- **Gate 0 step 2:** Dependabot PRs target the open minor branch (its
  `target-branch`), not the patch branch. Apply each pending bump to the
  patch branch by hand; re-read each PR's current versions first. Leave the
  PRs open; they merge into the open branch, or close as superseded at the
  wrap-up. The SPI's Dependabot targets SPI `main`; apply its pending bumps
  on SPI `release/$V` the same way.
- **Gate 0 step 4:** skip. `target-branch` stays on the open minor branch.
- **Step 10:** skip. The next release branch exists already.

**Wrap-up**, after the chart tag (or after the binary tag when no chart
release follows):

1. **SPI:** merge `release/$V` into SPI `main` with a merge commit.
   ```bash
   git fetch origin
   git switch -c "merge/$V" origin/main
   git merge --no-ff "origin/release/$V"
   ```
   Resolve the conflicts, push, open a PR into `main`, and merge it with
   `merge_method=merge`.
2. **cyoda-go:** merge `main` into `release/$V_NEXT` with a merge commit.
   ```bash
   git fetch origin
   git switch -c "merge/$V" "origin/release/$V_NEXT"
   git merge --no-ff origin/main
   ```
   The conflicts follow one pattern:
   - `CHANGELOG.md`: the forward-ported entries move from `[Unreleased]` into
     the `[X.Y.Z]` section that the merge brings.
   - `COMPATIBILITY.md`: keep the open branch's "Currently pinned" paragraph;
     update it after the re-pin.
   - `go.mod`/`go.sum`: keep the open branch's pins, re-apply the patch's
     dependency bumps, then re-pin.

   Commit the merge.
3. **Re-pin cyoda-go** to SPI `main` after step 1: re-pin as in
   [Bumping cyoda-go-spi](#bumping-cyoda-go-spi), then commit and push. Then
   re-pin the plugins:
   ```bash
   make repin-plugins
   git add go.mod go.sum
   git commit -m "chore: repin plugin modules to the branch head"
   git push origin "merge/$V"
   ```
   Run `make test`, open a PR into `release/$V_NEXT`, and merge it with
   `merge_method=merge` ([Merging a release PR](#merging-a-release-pr)).
4. Close the Dependabot PRs that the merge superseded. Re-read each PR's
   current versions first.
5. Delete `release/$V` in both repos (`git push origin --delete "release/$V"`)
   and close the milestone.

**Why merge commits, never squash.** Go derives a pseudo-version from the
highest tag that is an ancestor of the commit. Before the wrap-up, the tag
`$V` is on a side branch, so commits on SPI `main` and on `release/$V_NEXT`
get pseudo-versions `vX.Y.Z-0.<time>-<sha>`, and Go sorts those **below**
`vX.Y.Z`. A module graph that also requires `vX.Y.Z` (the plugin tags require
the SPI tag) then selects the tag and loses the newer surface. A merge commit
makes the tag an ancestor, and the pseudo-versions become
`vX.Y.(Z+1)-0.<time>-<sha>`. A squash or rebase merge does not.

### Bumping cyoda-go-spi

`cyoda-go-spi` must be pinned to the same version in every `go.mod` in
this repo: the root and all `plugins/*/go.mod`. The CI gate
`make check-spi-pin-sync` (workflow job `pin-sync`) enforces this and
will fail on PR if any manifest disagrees.

When you bump cyoda-go-spi:

1. Bump in `go.mod` (root).
2. Bump identically in `plugins/memory/go.mod`, `plugins/postgres/go.mod`, `plugins/sqlite/go.mod`.
3. Run `go mod tidy` in each module.
4. Run `make check-spi-pin-sync` and `make test` locally to confirm green.
5. Push; CI's `test`, `plugins`, `race` and `per-module-hygiene` jobs verify
   the cross-plugin interactions.

This rule is in addition to the existing **plugin-version lockstep**
rule (plugin submodule tags use the same version as the umbrella).
The two rules together ensure that consumers see consistent
SPI-and-plugin pinning at every umbrella tag.

**These two rules are repo-internal hygiene, not a binary↔SPI version
coupling.** "Pin-sync" means every `go.mod` *in this repo* agrees on one
SPI version; "plugin-version lockstep" means the plugin *tags* match the
umbrella *tag*. Neither ties the `cyoda-go` version number to the
`cyoda-go-spi` version number — those move independently (see the
"Versioning" section above). `cyoda-go-spi` versions on its **own** axis
under the same convention as everything else: a **patch** for additive
interface surface (which is what every SPI release `v0.5.0…v0.8.2`
actually was), a **minor** only for a breaking interface change. So the
SPI minor stays put across many binary releases; pick the next SPI
version by whether the SPI *interface* broke, never to match the
binary's number.

"Broke" covers the conformance suite, not just signatures. `spitest` gaining a
case that existing implementors fail is a breaking change to the contract this
module defines — a backend that compiled and passed now goes red on nothing but
a pin bump. Cut a **minor** for that, as for any interface break. `v0.8.3`
and `v0.8.4` shipped breaking changes as patches, before this rule covered the
SPI. `v0.9.1` is a breaking patch, as an exception: it backs out the
rollback-only mark that `v0.9.0` added to `TransactionCoordinator` (a bug fix)
and adds the conformance cases its `### Breaking` section lists.

**The SPI pin is maintainer-owned, not Dependabot-owned.** During a milestone
the root and plugin `go.mod` files pin a pseudo-version of a `cyoda-go-spi`
commit on the branch the milestone targets (`main` for a minor,
`release/vX.Y.Z` for a patch while the next minor is open), and the final
bump to the tagged `vX.Y.Z` happens by hand at release time (gate 0 above).
`.github/dependabot.yml`
therefore carries an `ignore` rule for `github.com/cyoda/cyoda-go-spi` in
every `gomod` block. This is deliberate: the SPI pin moves in lock-step with a coordinated
cross-repo release (gate 0), and a Dependabot group that bundles the SPI pin
with routine third-party bumps gets **mass-closed** if the pin can't resolve at
that moment — taking good third-party updates down with it. Keeping the SPI pin
out of Dependabot's hands keeps the grouped third-party PRs always-resolvable.

**Pin a commit on the target branch.** The SPI squash-merges its PRs and
deletes their branches, so a commit on an SPI PR branch ends up on no branch.
A cyoda-go PR can pin the SPI PR's head while both PRs are open; re-pin to the
merged commit as soon as the SPI PR merges:

```bash
# the commit on the SPI target branch
SPI_SHA=<sha>
for d in . plugins/memory plugins/postgres plugins/sqlite; do
  (cd "$d" && GOWORK=off go get "github.com/cyoda/cyoda-go-spi@$SPI_SHA" && GOWORK=off go mod tidy)
done
make check-spi-pin-sync
```

If the plugins need the new surface, push and run `make repin-plugins`
(step 3). The script tidies the root module itself; no further tidy is needed.

**Dependabot targets the active release branch, never `main`.** Work is staged
on `release/vX.Y.Z`, so a bump raised against `main` lands where nothing is
being built and has to be re-applied by hand on the release branch — which is
exactly what gate 0 exists to avoid. `dependabot.yml` therefore carries
`target-branch` on every entry, and moving it is a numbered release step, not
an ad-hoc chore: gate 0 edits the field, step 10 opens the branch it names.
Dependabot fails **silently** against a branch that does not exist, so the two
must not drift apart.

### 8. Publish the Helm chart

Frontline binary releases auto-open a PR from
`bump-chart-appversion.yml` titled
`chore(helm): bump chart appVersion to vX.Y.Z` (labels: `helm`,
`chart-release`). That PR by default updates only `appVersion` in
`deploy/helm/cyoda/Chart.yaml`. The chart's rendered output uses
`.Chart.AppVersion` for the image tag, so for the new binary to
actually reach `helm repo update && helm upgrade` consumers, a new
chart artefact must be published to `gh-pages` — which requires
bumping chart `version:` and tagging `cyoda-X.Y.Z`.

Two distinct tag namespaces — easy to get wrong:

- Binary tag: `vX.Y.Z` (e.g. `v0.6.3`) — fires `release.yml`.
- Chart tag:  `cyoda-X.Y.Z` (e.g. `cyoda-0.6.3`, **no `v` prefix**) —
  fires `release-chart.yml`. The workflow asserts the suffix matches
  `Chart.yaml.version` and fails loudly on mismatch.

Per-release procedure:

1. **Amend the auto-PR**: also bump chart `version:` in
   `deploy/helm/cyoda/Chart.yaml` to match `$V` without the `v`
   prefix, unless a chart change in the milestone already set it. Both
   fields end up the same value — same convention as plugin submodule
   tags. Record the released pair in `COMPATIBILITY.md`'s "Helm chart ×
   binary" table in the same PR. Push to the bot's branch:
   ```bash
   # the binary tag you just released
   V=v0.6.3
   git fetch origin
   git switch -c "chore/bump-chart-appversion-$V" "origin/chore/bump-chart-appversion-$V"
   # edit Chart.yaml version: and COMPATIBILITY.md, commit
   git push origin "chore/bump-chart-appversion-$V"
   ```
   The PR is opened by `github-actions[bot]`, so its runs on the bot's commit
   stay at `action_required`. Do not approve them: they test a head the PR
   no longer has. Your push starts fresh runs on the new head, and those runs
   gate the merge.
2. **Squash-merge** the PR once the checks on the new head pass
   ([Merging a release PR](#merging-a-release-pr)).
3. **Tag the chart** at the squash-merge commit on `main`:
   ```bash
   # the binary tag you just released
   V=v0.6.3
   CHART_V=${V#v}
   git tag -s -a "cyoda-$CHART_V" -m "Release chart cyoda-$CHART_V" <sha>
   git push origin "cyoda-$CHART_V"
   ```
4. **Verify**:
   - `release-chart.yml` succeeded (Actions tab) — runs `helm lint`,
     `helm template`, `kubeconform`, then `chart-releaser` packages
     and publishes to `gh-pages`.
   - The chart appears in the index:
     ```bash
     helm repo add cyoda https://cyoda.github.io/cyoda-go
     helm repo update
     helm search repo cyoda --versions | head
     ```
   - The `Verify release` run that follows the chart release is green
     (step 9): it checks the chart, and that the binary release still
     holds GitHub's Latest mark.

#### When NOT to bump chart `version:`

Skip the chart `version` bump (merge the auto-PR as-is, no `cyoda-*`
tag) only if the binary release should not be advertised to chart
consumers — e.g. an out-of-band binary patch you don't want in the
chart index yet. The next chart release will carry the change. With no
push to the PR, its only runs are the bot's: approve them in the PR
("Approve and run") and merge once they pass.

#### Chart-only releases

If templates change but no binary release is involved, open a manual
PR bumping chart `version:` only (leave `appVersion` alone), merge,
and tag `cyoda-X.Y.Z`. `bump-chart-appversion.yml` is not involved.

#### First-time prerequisite: enable GitHub Pages

Before the first `cyoda-*` tag is pushed, the maintainer must enable
GitHub Pages on the repo:

1. Repo Settings → Pages
2. Source: "Deploy from a branch"
3. Branch: `gh-pages` / `(root)`
4. Save

The `gh-pages` branch is created by `chart-releaser-action` on first
release and does not need to pre-exist. `release-chart.yml` verifies
Pages is configured via `gh api repos/:owner/:repo/pages` and fails
fast with an actionable error if not.

### 9. Verify the published release (gate)

The release is not done until it installs the way users install it. The
`Verify release` workflow (`.github/workflows/verify-release.yml`) runs
`scripts/verify-release.sh` after the `Release` run and after the
`Release chart` run. For every release it checks:

- `SHA256SUMS` is signed by `release.yml` for exactly this tag, and lists
  every asset it checks (`install.sh` from v0.9.0 on);
- the release's `install.sh`, with `CYODA_COSIGN_VERIFY=required`,
  installs this version;
- the `.deb` and `.rpm` install in clean containers;
- the image verifies with `cosign` and runs by digest;
- the help asset carries the version.

For the newest stable release (`--latest`) it also checks that GitHub's
Latest release is this tag, that `/releases/latest/download/SHA256SUMS` and
`install.sh` are this release's, that `install.sh` without `CYODA_VERSION`
installs it, and that the image tag `latest` is signed by this release run;
the workflow also installs it with Homebrew on macOS. A pre-release, or an
older release checked by hand, gets the pinned checks only. Every workflow
run, whatever the tag, first checks that GitHub's Latest mark is on the
newest stable release; if it is not, the run stops there.

The `Release chart` run checks the chart it published: the chart version
from its tag (`cyoda-C.D.E`) is served, downloads and renders, and its
appVersion's binary release passes the checks above. A chart version need
not equal the binary version.

Every verification run of the release must be green (two when a chart
release follows the binary release, one when it does not). If one fails,
fix it before announcing the release. A wrong Latest mark is fixed with
`gh release edit vX.Y.Z --repo cyoda/cyoda-go --latest` (the failure message
gives the command); then re-run the workflow. Anything else usually needs a
patch release. To run the checks by hand (needs `curl`, `cosign`, `docker`,
`jq`, `helm`):

```bash
scripts/verify-release.sh vX.Y.Z --latest --chart C.D.E
```

The workflow also runs weekly against the newest stable release (and any
chart for it), so drift (the Latest mark moving, an asset replaced) shows up
without a release.

### 9.5. Announce the release

Post the release notes as a discussion in the repository's **Announcements**
category, titled `Cyoda-go vX.Y.Z released`, after every `Verify release` run
of step 9 is green. `scripts/release-announcement.sh` builds the body from
`docs/release-notes/vX-Y-Z.md` and posts it:

```bash
V=v0.9.1
# dry run: print the body and read it
scripts/release-announcement.sh "$V"
# post it
scripts/release-announcement.sh "$V" --post
```

The body is the notes with each wrapped paragraph and list item joined onto
one line, because a discussion renders a single line break as a hard break.
The script refuses to post a title that already exists. Discussions have no
REST API, so it posts with `gh api graphql`.

Check the `_Released <date>_` line first: it is the day the binary tag was
cut. If the release-prep date is wrong, correct the notes and `CHANGELOG.md`
on the open release branch and post from that corrected file
(`--notes <file>` takes a path).

### 10. Open the next release branch

Skip this step for a
[patch release while the next minor is open](#patch-release-while-the-next-minor-is-open);
run its wrap-up instead.

The `target-branch` edit from gate 0 is now on `main`, naming a branch that
does not exist yet. Create it from `main` now, at the chart squash-merge commit:

```bash
# the next milestone
V_NEXT=v0.11.0
git fetch origin
git branch "release/${V_NEXT}" origin/main
git push origin "release/${V_NEXT}"
```

Delete the shipped release branch; its commits are on `main` through the
release merge commit:

```bash
git push origin --delete "release/vX.Y.Z"
```

Then confirm the two agree — this is the check that catches a silent
Dependabot outage:

```bash
git ls-remote --heads origin "refs/heads/$(grep -m1 'target-branch:' .github/dependabot.yml | awk '{print $2}')"
```

An empty result means every Dependabot ecosystem is pointed at a dead branch
and will raise nothing until it is fixed. Finally, close the shipped
milestone and open the next one.

## Pre-release testing

Before cutting a release you can exercise the release pipeline via a
prerelease tag:

```bash
git tag v0.6.0-rc.1
git push origin v0.6.0-rc.1
```

This fires the full release workflow, producing a prerelease GitHub
Release, images tagged `:0.6.0-rc.1` (but NOT `:latest`), cosign
signatures, and SBOMs. The Homebrew tap, chart appVersion bump, and
`install.sh` / `.deb` / `.rpm` user-facing paths are all unaffected
because:

- `brews:` has `skip_upload: auto` — prereleases don't commit to the tap.
- the `latest` image has `disable: "{{ .Prerelease }}"` — doesn't move.
- `bump-chart-appversion.yml` filters out tags containing `-`.
- `install.sh` follows `github.com/.../releases/latest`, which never names a prerelease.

Delete the rc release afterwards if desired:

```bash
gh release delete v0.6.0-rc.1 --cleanup-tag --yes
```

## Maintenance of older release lines

Cyoda-go is pre-1.0 and **older release lines are not maintained**. No back-port branches exist by default. Patch bumps within the active line are non-breaking; minor bumps may break wire format, configuration, or operational surface.

If a real consumer needs a fix on an older line:

1. Open an issue describing the constraint (which version, which fix, why an upgrade is not viable).
2. The maintainers will consider creating an official maintenance branch for that line.
3. If accepted, the branch is named `release/vX.Y.x` (e.g. `release/v0.6.x`) and is cut from the relevant tag.

Until a maintenance branch is created and announced, treat older lines as frozen.

## Gotcha: snapshot-testing from a local clone

GoReleaser generates the Homebrew formula's `url` fields from the git
`origin` remote. If you snapshot-test from a temp clone of a local path
(`git clone --local ...`), `origin` will be the local filesystem path
and the formula URLs come out garbled.

Fix before running `goreleaser release --snapshot`:

```bash
cd /path/to/temp/clone
git remote set-url origin https://github.com/cyoda/cyoda-go.git
# Also disable push from this temp clone so an absent-minded "git push"
# does not accidentally shove a local branch to the real upstream:
git remote set-url --push origin NO_PUSH
```

After that, the generated `dist/homebrew/cyoda.rb` will have correct
download URLs and `brew audit --strict` can run against it meaningfully.
The `NO_PUSH` sentinel is an unroutable value — any `git push` from the
temp clone fails cleanly rather than reaching GitHub.
