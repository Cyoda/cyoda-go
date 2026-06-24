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

Releases are **intentional**, not per-merge. Merge PRs to `main`
freely; when you decide to cut a release, push a `v*` tag. That
fires `release.yml`, which builds artifacts, signs, and publishes.

### Versioning

Semver with a leading `v`: `v0.6.0`, `v1.2.3`. Pre-v1 allows
breaking changes between minor versions. New version must be
**strictly greater** than the previous tag — Go module tags are
write-once-per-value on the proxy (`proxy.golang.org` caches the
SHA of every tag it serves), so you cannot retag or reuse a
version number, even on repos with no production consumers.

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
   touch the SPI pin; see "Bumping cyoda-go-spi" below).
2. Apply every pending Dependabot minor/patch bump (root + each plugin
   submodule). If a grouped PR was closed because it was entangled with the
   then-unresolvable SPI pin, apply its third-party updates by hand here.
3. `go mod tidy` in every module.
4. Verify: `make check-spi-pin-sync`, `make test-all`, `make race`.

After this commit the branch is at latest on everything, so Dependabot's next
run finds nothing to raise — a clean release with no immediate follow-on churn.

### 1. Plugin submodule tags first

Plugin modules (`plugins/memory`, `plugins/postgres`, `plugins/sqlite`)
live in this repo but have their own `go.mod` files and their own
tag namespaces (`plugins/<name>/vX.Y.Z`). They must be tagged
**before** the root module's `go.mod` can pin them — otherwise
`go mod tidy` at step 3 has nothing to resolve against.

Tag each plugin submodule at the same commit as the forthcoming root
release. Pick the same version number as the root tag to keep the
mental model simple:

```bash
# In cyoda-go (main branch, at the commit to be released):
V=v0.6.0   # pick per release
git tag "plugins/memory/$V"
git tag "plugins/postgres/$V"
git tag "plugins/sqlite/$V"
git push origin "plugins/memory/$V" "plugins/postgres/$V" "plugins/sqlite/$V"
```

### 3. Drop the `replace` directives and pin plugin module versions

Root `go.mod` currently has three `replace` directives pointing at
`./plugins/*` for dev-time convenience. Release builds must resolve
to published modules, not local paths — so these must be dropped.
The release workflow's pre-flight rejects any `replace` and would
abort cleanly, but removing them explicitly is cleaner:

```bash
go mod edit -dropreplace github.com/cyoda-platform/cyoda-go/plugins/memory
go mod edit -dropreplace github.com/cyoda-platform/cyoda-go/plugins/postgres
go mod edit -dropreplace github.com/cyoda-platform/cyoda-go/plugins/sqlite
go mod tidy
```

`go mod tidy` pins the plugin modules to the tags you just cut in
step 1. Review the diff to `go.mod` and `go.sum` — the `require`
block should gain entries for each plugin at the release version,
and the `replace` directives should be gone.

```bash
git add go.mod go.sum
git commit -m "chore: drop replace directives; pin plugin modules at $V"
git push origin main
```

**Why not delete the replace directives now?** Because pre-public
development uses `GOWORK=off go build ./...` occasionally (reviews,
snapshot checks) and the replaces make that work without requiring
every plugin change to be tagged and pushed. After the first release
they become vestigial and can stay dropped; dev-time workflows use
`go.work` for local composition going forward.

### 4. Homebrew tap setup (from the section above)

Create `cyoda/homebrew-cyoda-go` repo, create the GitHub App,
install on the tap repo, store the App ID and private key as Actions
secrets. See "One-time setup: Homebrew tap release automation" above.

### 5. Verify CI is green

Push a commit to `main` (or open a small PR) and confirm both the
`test` and `per-module-hygiene` CI jobs pass. Don't push the release
tag if CI is red.

### 6. Verify GoReleaser signing (one-time, before first release)

The `dockers_v2` migration in `.goreleaser.yaml` changed the Docker-artifact
pipeline. The `docker_signs: artifacts: manifests` selector was expected to
still resolve against the new artifact types, but this was NOT empirically
verified at migration time (Docker subshells were unavailable in the
authoring environment). Before the first `v*` tag push, confirm the
selector actually resolves:

```bash
# Clone to a scratch dir per the snapshot-testing gotcha section:
tmp=$(mktemp -d) && git clone --local . "$tmp/cyoda-go"
cd "$tmp/cyoda-go"
git remote set-url origin https://github.com/cyoda/cyoda-go.git
git remote set-url --push origin NO_PUSH

# Tag a non-prerelease snapshot:
git tag v0.0.0

# Run snapshot (no publish, no signing — just artifact generation):
goreleaser release --snapshot --clean --skip=publish --skip=sign

# Verify docker_signs' selector 'artifacts: manifests' will resolve:
python3 -c "
import json
artifacts = json.load(open('dist/artifacts.json'))
manifest_types = {'Docker Manifest', 'Published Docker Manifest', 'Docker Image V2', 'Docker Manifest List'}
matching = [a for a in artifacts if a.get('type') in manifest_types]
if not matching:
    print('FAIL: docker_signs selector will resolve to empty.')
    print('Artifact types produced:')
    for a in artifacts:
        print(f'  - {a.get(\"type\", \"?\"):30s} {a.get(\"name\", \"?\")}')
    raise SystemExit(1)
print(f'OK: {len(matching)} manifest artifact(s) — docker_signs will sign')
"
```

If the assertion fails, the `docker_signs.artifacts:` selector needs to
match one of the actual type names printed in the FAIL output. Update
`.goreleaser.yaml`'s `docker_signs:` block with the correct selector
(likely `Docker Manifest` or `all`) before pushing the release tag.

The CI smoke-test job `release-smoke.yml` runs this check automatically
on every PR touching `.goreleaser.yaml` or the Dockerfile — but this
manual step stays in the checklist as a final pre-release confirmation.

### 6.5. Update COMPATIBILITY.md

Add a row to the `cyoda-go × cyoda-go-spi` matrix in [`COMPATIBILITY.md`](./COMPATIBILITY.md) for the new `cyoda-go` tag. Capture: root `go.mod` SPI pin, plugin submodule SPI pins (these may differ from root if the submodules don't need new SPI fields), and a one-line summary of the SPI surface added in this release (or `—` if binary-only).

If the chart `version:` or `appVersion:` changed in this cycle, update the "Helm chart × binary" table.

If out-of-tree-plugin guidance changed (e.g. cassandra adopted a new SPI pin), update the "Out-of-tree plugins" table.

The update lands either on the release-prep PR (if the matrix data is known pre-tag) or on a follow-up `docs(compatibility): record v<X.Y.Z>` commit immediately after step 7. The latter is acceptable because the matrix records the pin observed AT a published tag.

### 7. Cut the release

Use `gh release create` rather than raw `git tag + git push`:

```bash
V=v0.6.0   # or whatever this release is
gh release create "$V" \
  --title "$V" \
  --generate-notes \
  --target main
```

`--generate-notes` drafts release notes from merged PR titles since
the previous tag. Edit in the browser before publishing if you want
a hand-written summary on top of the PR list. No separate
`CHANGELOG.md` is maintained — GitHub Releases are the canonical
changelog.

The release creation pushes the tag, which fires `release.yml`:
pre-flight module verification, build binaries, multi-arch image to
GHCR, keyless cosign signing, SBOM attachment, Homebrew formula
commit to the tap, GitHub Release artifacts attached.

The workflow runs ~5 minutes. Watch it in the Actions tab and
verify:

- Release appears on the Releases page with all expected archives,
  `.deb`/`.rpm` packages, `SHA256SUMS`, cosign signatures, SBOMs.
- `ghcr.io/cyoda/cyoda:$V` and `:latest` manifests exist.
- `cyoda/homebrew-cyoda-go` shows a new commit updating
  `Formula/cyoda.rb` to `$V`.
- A PR titled `chore(helm): bump chart appVersion to $V` (labels:
  `helm`, `chart-release`) is opened automatically by
  `bump-chart-appversion.yml`. Proceed to step 8 to publish the chart.

### Coordinated release across sibling repos

Some features span `cyoda-go-spi`, `cyoda-go`, and `cyoda-go-cassandra`.
The dependency chain is:

```
cyoda-go-spi   ← cyoda-go   ← cyoda-go-cassandra
              ← cyoda-go-cassandra
```

Release in topological order:

1. **`cyoda-go-spi`**: merge the feature branch, then
   `gh release create $V --generate-notes`. No build workflow to fire;
   the release is the tag itself. Wait 30s or so for `proxy.golang.org`
   to be able to serve the new version.

2. **`cyoda-go`**: on its feature branch, bump `go.mod`'s
   `cyoda-go-spi` require line to the new `$V`; similarly for each
   plugin submodule's `go.mod`. Remove the `go.work` entry pointing
   at the local SPI checkout. Verify tests still pass. Merge to
   `main`. Then run steps 1, 2, 3, 7 of this checklist (plugin
   submodule tags, drop replace directives, cut root release) using
   the same `$V`.

3. **`cyoda-go-cassandra`**: on its feature branch, bump `go.mod`'s
   `cyoda-go-spi`, `cyoda-go`, and `cyoda-go/plugins/*` require
   lines to the new versions. Merge. `gh release create $V ...`.

Each step waits for the prior tag to land before its `go mod tidy`
can resolve. In practice: merge, tag, wait for CI, then move on.

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

**The SPI pin is maintainer-owned, not Dependabot-owned.** During a milestone
the root and plugin `go.mod` files pin a pseudo-version of `cyoda-go-spi`
against `main` HEAD, and the final bump to the tagged `vX.Y.Z` happens by hand
at release time (gate 0 above). `.github/dependabot.yml` therefore carries an
`ignore` rule for `github.com/cyoda-platform/cyoda-go-spi` in every `gomod`
block. This is deliberate: the SPI pin moves in lock-step with a coordinated
cross-repo release (gate 0), and a Dependabot group that bundles the SPI pin
with routine third-party bumps gets **mass-closed** if the pin can't resolve at
that moment — taking good third-party updates down with it. Keeping the SPI pin
out of Dependabot's hands keeps the grouped third-party PRs always-resolvable.

**Retarget Dependabot after the milestone ships.** `dependabot.yml`'s
`target-branch` points at the active milestone branch (e.g. `release/v0.8.0`)
so bumps land where the work is. Once that milestone merges to `main` and is
tagged, flip `target-branch` to the next active branch (`main`, or the next
`release/vX.Y.0`) so Dependabot doesn't keep raising PRs against a dead branch.

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
   prefix. Both fields end up the same value — same convention as
   plugin submodule tags.
2. **Merge** the PR.
3. **Tag the chart** at the merge commit on `main`:
   ```bash
   V=v0.6.3   # the binary tag you just released
   CHART_V=${V#v}
   git tag "cyoda-$CHART_V"
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

#### When NOT to bump chart `version:`

Skip the chart `version` bump (merge the auto-PR as-is, no `cyoda-*`
tag) only if the binary release should not be advertised to chart
consumers — e.g. an out-of-band binary patch you don't want in the
chart index yet. The next chart release will carry the change.

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

### 9. (Optional) Smoke-test each install path

```bash
# Homebrew (macOS or Linux):
brew install cyoda/cyoda-go/cyoda
cyoda --help

# curl | sh (any Unix):
curl -fsSL https://raw.githubusercontent.com/cyoda/cyoda-go/main/scripts/install.sh | sh

# Debian:
wget https://github.com/cyoda/cyoda-go/releases/latest/download/cyoda_linux_amd64.deb
sudo dpkg -i cyoda_linux_amd64.deb
```

## Pre-release testing

Before cutting a release you can exercise the release pipeline via a
prerelease tag:

```bash
git tag v0.6.0-rc.1
git push origin v0.6.0-rc.1
```

This fires the full release workflow, producing a prerelease GitHub
Release, images tagged `:v0.1.0-rc.1` (but NOT `:latest`), cosign
signatures, and SBOMs. The Homebrew tap, chart appVersion bump, and
`install.sh` / `.deb` / `.rpm` user-facing paths are all unaffected
because:

- `brews:` has `skip_upload: auto` — prereleases don't commit to the tap.
- `:latest` manifest has `skip_push: '{{ .Prerelease }}'` — doesn't move.
- `bump-chart-appversion.yml` filters out tags containing `-`.
- `install.sh` uses the GitHub `/releases/latest` API which hides prereleases.

Delete the rc release afterwards if desired:

```bash
gh release delete v0.1.0-rc.1 --cleanup-tag --yes
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
# Also disable push from this temp clone so an absent-minded `git push`
# doesn't accidentally shove a local branch to the real upstream:
git remote set-url --push origin NO_PUSH
```

After that, the generated `dist/homebrew/cyoda.rb` will have correct
download URLs and `brew audit --strict` can run against it meaningfully.
The `NO_PUSH` sentinel is an unroutable value — any `git push` from the
temp clone fails cleanly rather than reaching GitHub.
