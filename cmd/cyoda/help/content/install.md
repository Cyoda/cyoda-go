---
topic: install
title: "install — install and upgrade the cyoda binary"
stability: stable
see_also:
  - quickstart
  - cli.init
  - run
  - helm
---

# install

## NAME

install — ways to install the `cyoda` binary, and how to upgrade it.

## SYNOPSIS

```
brew install cyoda/cyoda-go/cyoda
curl -fsSL https://github.com/cyoda/cyoda-go/releases/latest/download/install.sh | sh
go install github.com/cyoda/cyoda-go/cmd/cyoda@latest
```

## DESCRIPTION

cyoda is one binary. Every release on `https://github.com/cyoda/cyoda-go/releases` has archives for Linux and macOS (`amd64`, `arm64`) and Windows (`.zip`), `.deb` and `.rpm` packages for Linux, a `SHA256SUMS` file, and keyless Sigstore (`cosign`) signatures for the archives and `SHA256SUMS`. The container image is `ghcr.io/cyoda/cyoda`. A binary installed by any route below runs with the in-memory backend until it is given a configuration: `cyoda init` writes a user config that selects sqlite (see `cyoda help cli init`), and the `.deb`/`.rpm` packages ship a system config that does.

To run cyoda on Kubernetes, use the Helm chart instead (`cyoda help helm`); it runs the container image.

## ROUTES

**Homebrew (macOS, Linux):**

```
brew install cyoda/cyoda-go/cyoda
cyoda init
```

**Install script (Linux, macOS):**

```
curl -fsSL https://github.com/cyoda/cyoda-go/releases/latest/download/install.sh | sh
```

A pinned release, with the signature check required:

```
curl -fsSL https://github.com/cyoda/cyoda-go/releases/latest/download/install.sh | CYODA_VERSION=v0.9.0 CYODA_COSIGN_VERIFY=required sh
```

From v0.9.0 the release's signed `SHA256SUMS` lists `install.sh` too: to check the script before you run it, download it with `SHA256SUMS` and verify both as for the packages below.

The script downloads the archive for the host's OS and architecture, checks it against `SHA256SUMS`, installs `cyoda` to `~/.local/bin`, and runs `cyoda init`. When `cosign` is on `PATH`, it also verifies the signatures of the archive and of `SHA256SUMS`. The settings are environment variables of `sh`, so they go after the pipe, as above; a variable set before `curl` reaches `curl` only:

- `CYODA_VERSION` — the release tag to install, for example `v0.9.0`; default the latest release.
- `CYODA_INSTALL_DIR` — the directory to install into; default `~/.local/bin`.
- `CYODA_COSIGN_VERIFY=required` — stop if `cosign` is not on `PATH`, instead of checking `SHA256SUMS` only.

Without `CYODA_COSIGN_VERIFY=required`, a release that has no signature file is checked against `SHA256SUMS` only. For a production host, set `CYODA_COSIGN_VERIFY=required`. `CYODA_COSIGN_VERIFY` takes `auto` (the default: verify when `cosign` is on `PATH`), `required`, or `false` (also `0` or `no`: skip the signature check); any other value stops the script. Releases before v0.7.0 are not signed; v0.7.0 and v0.7.1 were signed under the GitHub organization's earlier name and do not verify, so with `cosign` on `PATH` the script refuses them.

**Debian, Ubuntu (`.deb`) and Fedora, RHEL (`.rpm`):**

Set `V` to the release you install (the release page lists them). Download the package and the release's `SHA256SUMS` with its signature. Check with `cosign` that the release workflow signed `SHA256SUMS` from that release's tag, as the install script does, then check the package against it:

```
V=v0.9.0
R=https://github.com/cyoda/cyoda-go/releases/download/$V
curl -fsSLO "$R/cyoda_linux_amd64.deb"
curl -fsSLO "$R/SHA256SUMS" -O "$R/SHA256SUMS.sig" -O "$R/SHA256SUMS.pem"
cosign verify-blob --certificate SHA256SUMS.pem --signature SHA256SUMS.sig \
  --certificate-identity "https://github.com/Cyoda/cyoda-go/.github/workflows/release.yml@refs/tags/$V" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-github-workflow-trigger push \
  --certificate-github-workflow-ref "refs/tags/$V" \
  SHA256SUMS \
  && sha256sum --ignore-missing -c SHA256SUMS \
  && sudo dpkg -i cyoda_linux_amd64.deb
```

For Fedora or RHEL, download `cyoda_linux_amd64.rpm` instead and end with `sudo rpm -i cyoda_linux_amd64.rpm`. Each step runs only when the one before it succeeded. Pinning the tag matters: a signature of another release does not verify this one, so an older signed package cannot be passed off as the one you asked for. The owner is spelt `Cyoda`, as in the certificate. Use `arm64` in place of `amd64` on ARM hosts. The package installs `/usr/bin/cyoda` and the system config `/etc/cyoda/cyoda.env`, which selects sqlite. An upgrade keeps a changed `/etc/cyoda/cyoda.env`.

**Container image:** `ghcr.io/cyoda/cyoda`. Each release is tagged with its version without the `v` (for example `0.9.0`); `latest` is the newest release that is not a pre-release. The release workflow signs the image. A tag can be moved, so check the signature for the release you want and run the image by the digest that check printed (this needs `jq`):

```
V=0.9.0
OUT=$(cosign verify "ghcr.io/cyoda/cyoda:$V" \
  --certificate-identity "https://github.com/Cyoda/cyoda-go/.github/workflows/release.yml@refs/tags/v$V" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-github-workflow-trigger push \
  --certificate-github-workflow-ref "refs/tags/v$V") \
  && D=$(printf '%s' "$OUT" | jq -r '.[0].critical.image."docker-manifest-digest"') \
  && docker pull "ghcr.io/cyoda/cyoda@$D" \
  && echo "run ghcr.io/cyoda/cyoda@$D"
```

For running the image, see `cyoda help run` (Docker, Docker Compose).

**Windows:** download the `windows` `.zip` archive for your architecture (`cyoda_<version>_windows_amd64.zip`) from the release page, with `SHA256SUMS`, `SHA256SUMS.sig` and `SHA256SUMS.pem`. Check `SHA256SUMS` with `cosign` (one line, any shell; put your release in both places): `cosign verify-blob --certificate SHA256SUMS.pem --signature SHA256SUMS.sig --certificate-identity https://github.com/Cyoda/cyoda-go/.github/workflows/release.yml@refs/tags/v0.9.0 --certificate-oidc-issuer https://token.actions.githubusercontent.com --certificate-github-workflow-trigger push --certificate-github-workflow-ref refs/tags/v0.9.0 SHA256SUMS`. Then compare the archive's hash (`certutil -hashfile <archive> SHA256`) with its line in `SHA256SUMS`. Use the archive only when both match; then put `cyoda.exe` on `PATH`.

**From source** (needs the Go version in `go.mod`):

```
go install github.com/cyoda/cyoda-go/cmd/cyoda@latest
```

## UPGRADE

Install the newer release by the same route:

- Homebrew: `brew upgrade cyoda`.
- Install script: run it again; with `CYODA_VERSION` it installs that release.
- `.deb`/`.rpm`: verify the newer package as above, then install it (`sudo dpkg -i …`, `sudo rpm -U …`).
- Container image: verify the newer release's image and pull it by digest, as above, then restart the container with that digest.
- From source: `go install …@latest` again.

Read the release notes before an upgrade. A storage backend's schema migrates when the new binary starts (sqlite and postgres auto-migrate by default; see `cyoda help cli migrate`).

## SEE ALSO

- quickstart
- cli.init
- run
- helm
