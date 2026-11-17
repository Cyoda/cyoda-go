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
docker pull ghcr.io/cyoda/cyoda:<version>
go install github.com/cyoda-platform/cyoda-go/cmd/cyoda@latest
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

The script downloads the archive for the host's OS and architecture, checks it against `SHA256SUMS`, installs `cyoda` to `~/.local/bin`, and runs `cyoda init`. When `cosign` is on `PATH`, it also verifies the signatures of the archive and of `SHA256SUMS`. Settings, as environment variables of `sh`:

- `CYODA_VERSION` — the release tag to install, for example `v0.9.0`; default the latest release.
- `CYODA_INSTALL_DIR` — the directory to install into; default `~/.local/bin`.
- `CYODA_COSIGN_VERIFY=required` — stop if `cosign` is not on `PATH`, instead of checking `SHA256SUMS` only.

Without `CYODA_COSIGN_VERIFY=required`, a release that has no signature file is checked against `SHA256SUMS` only. For a production host, set `CYODA_COSIGN_VERIFY=required`.

**Debian, Ubuntu (`.deb`) and Fedora, RHEL (`.rpm`):**

```
curl -fsSLO https://github.com/cyoda/cyoda-go/releases/latest/download/cyoda_linux_amd64.deb
sudo dpkg -i cyoda_linux_amd64.deb

curl -fsSLO https://github.com/cyoda/cyoda-go/releases/latest/download/cyoda_linux_amd64.rpm
sudo rpm -i cyoda_linux_amd64.rpm
```

Use `arm64` in place of `amd64` on ARM hosts. The package installs `/usr/bin/cyoda` and the system config `/etc/cyoda/cyoda.env`, which selects sqlite. An upgrade keeps a changed `/etc/cyoda/cyoda.env`.

**Container image:**

```
docker pull ghcr.io/cyoda/cyoda:<version>
```

Each release is tagged with its version without the `v` (for example `0.9.0`); `latest` is the newest release that is not a pre-release. For running the image, see `cyoda help run` (Docker, Docker Compose).

**Windows:** download the `windows` `.zip` archive for your architecture from the release page and put `cyoda.exe` on `PATH`.

**From source** (needs the Go version in `go.mod`):

```
go install github.com/cyoda-platform/cyoda-go/cmd/cyoda@latest
```

## UPGRADE

Install the newer release by the same route:

- Homebrew: `brew upgrade cyoda`.
- Install script: run it again; with `CYODA_VERSION` it installs that release.
- `.deb`/`.rpm`: install the newer package (`sudo dpkg -i …`, `sudo rpm -U …`).
- Container image: pull the newer tag and restart the container.
- From source: `go install …@latest` again.

Read the release notes before an upgrade. A storage backend's schema migrates when the new binary starts (sqlite and postgres auto-migrate by default; see `cyoda help cli migrate`).

## SEE ALSO

- quickstart
- cli.init
- run
- helm
