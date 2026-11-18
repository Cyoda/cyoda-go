#!/bin/sh
# cyoda installer — downloads the latest (or pinned) release for the
# current OS/arch, verifies SHA256 (and cosign keyless signature when
# cosign is available), installs to ~/.local/bin/cyoda, and runs
# 'cyoda init' to enable sqlite persistence.
#
# Usage:
#   curl -fsSL https://github.com/cyoda/cyoda-go/releases/latest/download/install.sh | sh
#
# The settings below are read by this script, so they go on `sh`, after
# the pipe (a variable set before `curl` reaches curl only):
#
# Pin a version:
#   curl -fsSL ... | CYODA_VERSION=v0.2.0 sh
#
# Pin a different install directory:
#   curl -fsSL ... | CYODA_INSTALL_DIR=~/bin sh
#
# Cosign verification:
#   - Auto-on when `cosign` is on PATH. Verifies the archive and
#     SHA256SUMS against keyless Sigstore signatures issued by the
#     cyoda/cyoda-go release.yml workflow on a tag push.
#   - Disable explicitly: curl ... | CYODA_COSIGN_VERIFY=false sh
#   - Force-fail-without-cosign: curl ... | CYODA_COSIGN_VERIFY=required sh
#     (Aborts if cosign is missing rather than falling back to SHA256-only.)
#
# Auto-mode trade-off (security note): cosign signing was added in
# v0.7.0; older releases have no .sig files. v0.7.0 and v0.7.1 were
# signed before the GitHub org was renamed: their certificates name
# `Cyoda-platform`, which this script does not trust (the old name is no
# longer the org's and could be claimed), so with cosign on PATH they are
# refused. Install v0.7.4 or later. To preserve backward
# compat with the curl-pipe-to-sh UX for users without cosign, auto
# mode treats a 404 on the .sig as "release predates cosign signing"
# and falls back to SHA256-only. This means an attacker who controls
# the GitHub release page on a v0.7.0+ release can bypass cosign by
# deleting the .sig along with their archive swap — SHA256 against
# their own SHA256SUMS would still pass. For hardened/production
# deployments, set CYODA_COSIGN_VERIFY=required: missing .sig is
# then a hard error, not a fallback.

set -eu

REPO="cyoda/cyoda-go"
INSTALL_DIR="${CYODA_INSTALL_DIR:-$HOME/.local/bin}"
# Cosign keyless verification expects the signing identity to come from
# this OIDC issuer (GitHub Actions) and the cert subject to match the
# release.yml workflow path under our org+repo, fired from a tag push
# (not workflow_dispatch from an arbitrary branch). release.yml is the
# only workflow with `id-token: write`, and we additionally pin to the
# `refs/tags/v*` ref-suffix so a future branch-based dispatch (even
# from main with proper protection) can't mint a cert that this
# verifier accepts. The owner is spelt as GitHub writes it in the
# certificate, `Cyoda`; the match is case-sensitive.
COSIGN_OIDC_ISSUER="https://token.actions.githubusercontent.com"
COSIGN_IDENTITY_REGEX="^https://github\.com/Cyoda/cyoda-go/\.github/workflows/release\.yml@refs/tags/v"
# Bind to push-triggered runs from a tag ref. release.yml's `on:`
# allows both `push: tags` and `workflow_dispatch`; only the former
# can produce signed release artifacts that consumers should trust,
# since workflow_dispatch can be triggered from any branch by anyone
# with write access.
COSIGN_GH_WORKFLOW_TRIGGER="push"
# The workflow ref is pinned to the exact tag being installed
# (--certificate-github-workflow-ref, in cosign_verify): a signature from
# another release does not verify this one.

err() { printf 'install.sh: error: %s\n' "$*" >&2; }
warn() { printf 'install.sh: warning: %s\n' "$*" >&2; }
info() { printf '%s\n' "$*"; }

require() {
    for cmd in "$@"; do
        if ! command -v "$cmd" >/dev/null 2>&1; then
            err "required command not found: $cmd"
            exit 1
        fi
    done
}
require curl tar
# Probe for GNU coreutils sha256sum first (Linux), fall back to macOS's /usr/bin/shasum.
# At least one must be present.
if command -v sha256sum >/dev/null 2>&1; then
    sha256_check() { sha256sum -c -; }
elif command -v shasum >/dev/null 2>&1; then
    sha256_check() { shasum -a 256 -c -; }
else
    err "need sha256sum (Linux) or shasum (macOS) to verify checksums"
    exit 1
fi

detect_os() {
    os=$(uname -s)
    case "$os" in
        Linux)  printf 'linux' ;;
        Darwin) printf 'darwin' ;;
        *)
            err "unsupported OS: $os (expected Linux or Darwin)"
            exit 1
            ;;
    esac
}

detect_arch() {
    arch=$(uname -m)
    case "$arch" in
        x86_64|amd64) printf 'amd64' ;;
        aarch64|arm64) printf 'arm64' ;;
        *)
            err "unsupported arch: $arch (expected x86_64/amd64 or aarch64/arm64)"
            exit 1
            ;;
    esac
}

resolve_version() {
    if [ -n "${CYODA_VERSION:-}" ]; then
        printf '%s' "$CYODA_VERSION"
        return
    fi
    # github.com/<repo>/releases/latest redirects to the Latest release's tag
    # page. No API call: the API's per-address rate limit does not apply.
    rv_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") || return 0
    case "$rv_url" in
        */releases/tag/*) printf '%s' "${rv_url##*/}" ;;
    esac
}

# cosign_fetch downloads a release asset and prints its HTTP status to
# stdout (so callers can distinguish 200, 404, and other failures).
# Unlike curl -fsSL, a 404 here does NOT terminate the script. Redirects are
# followed: GitHub answers a release download with a 302 to its storage.
cosign_fetch() {
    cf_url=$1
    cf_dest=$2
    curl -sSL -o "$cf_dest" -w "%{http_code}" "$cf_url" 2>/dev/null || printf '000'
}

# cosign_verify runs keyless Sigstore verification of the archive and
# SHA256SUMS using sibling .sig + .pem assets published by GoReleaser.
# Default behaviour:
#   - cosign on PATH AND CYODA_COSIGN_VERIFY != "false" → verify; abort on
#     verification failure or transient infrastructure errors. A 404 on
#     the sibling .sig (release predates cosign signing in v0.7.0) is a
#     soft fall-back to SHA256-only with a warning.
#   - CYODA_COSIGN_VERIFY = "required" → cosign must be on PATH AND every
#     sibling artifact must be present. A 404 on the .sig is a hard error.
#   - cosign missing in auto mode → SHA256-only with a hint about cosign.
#   - CYODA_COSIGN_VERIFY = "false" → skip entirely.
cosign_verify() {
    cv_tmp=$1
    cv_archive=$2
    cv_version=$3

    cv_mode="${CYODA_COSIGN_VERIFY:-auto}"
    case "$cv_mode" in
        false|0|no) info "Skipping cosign verification (CYODA_COSIGN_VERIFY=$cv_mode)"; return 0 ;;
    esac

    if ! command -v cosign >/dev/null 2>&1; then
        if [ "$cv_mode" = "required" ]; then
            err "CYODA_COSIGN_VERIFY=required but cosign is not on PATH. Install: https://docs.sigstore.dev/cosign/installation/"
            exit 1
        fi
        warn "cosign is not on PATH; skipping signature verification (SHA256-only). Install cosign for keyless Sigstore verification: https://docs.sigstore.dev/cosign/installation/"
        return 0
    fi

    info "Verifying cosign signatures (keyless)"
    for cv_target in "$cv_archive" SHA256SUMS; do
        cv_sig_url="https://github.com/$REPO/releases/download/$cv_version/${cv_target}.sig"
        cv_pem_url="https://github.com/$REPO/releases/download/$cv_version/${cv_target}.pem"

        cv_sig_status=$(cosign_fetch "$cv_sig_url" "$cv_tmp/${cv_target}.sig")
        if [ "$cv_sig_status" = "404" ]; then
            if [ "$cv_mode" = "required" ]; then
                err "CYODA_COSIGN_VERIFY=required but ${cv_target}.sig is not on this release ($cv_version). Cosign signing was added in v0.7.0; older releases are unsigned."
                exit 1
            fi
            warn "release $cv_version predates cosign signing (added in v0.7.0); falling back to SHA256-only verification"
            return 0
        fi
        if [ "$cv_sig_status" != "200" ]; then
            err "could not download signature ($cv_sig_status): $cv_sig_url"
            exit 1
        fi

        cv_pem_status=$(cosign_fetch "$cv_pem_url" "$cv_tmp/${cv_target}.pem")
        if [ "$cv_pem_status" != "200" ]; then
            err "could not download certificate ($cv_pem_status): $cv_pem_url"
            exit 1
        fi

        cv_log="$cv_tmp/cosign-${cv_target}.log"
        if ! cosign verify-blob \
            --certificate="$cv_tmp/${cv_target}.pem" \
            --signature="$cv_tmp/${cv_target}.sig" \
            --certificate-identity-regexp="$COSIGN_IDENTITY_REGEX" \
            --certificate-oidc-issuer="$COSIGN_OIDC_ISSUER" \
            --certificate-github-workflow-trigger="$COSIGN_GH_WORKFLOW_TRIGGER" \
            --certificate-github-workflow-ref="refs/tags/$cv_version" \
            "$cv_tmp/$cv_target" >"$cv_log" 2>&1; then
            err "cosign verification FAILED for $cv_target — refusing to install."
            err "cosign output (last 5 lines):"
            tail -n 5 "$cv_log" >&2 || :
            err "Re-run with CYODA_COSIGN_VERIFY=false to skip (not recommended)."
            exit 1
        fi
    done
    info "Signatures verified (cosign)"
}

# check_cosign_mode refuses an unknown CYODA_COSIGN_VERIFY before anything is
# downloaded. An unknown value must not fall back to auto mode: an operator
# who wrote `true` or `REQUIRED` expected enforcement.
check_cosign_mode() {
    case "${CYODA_COSIGN_VERIFY:-auto}" in
        auto|required|false|0|no) : ;;
        *)
            err "CYODA_COSIGN_VERIFY=$CYODA_COSIGN_VERIFY is not one of auto, required, false, 0, no."
            exit 1
            ;;
    esac
}

main() {
    check_cosign_mode
    os=$(detect_os)
    arch=$(detect_arch)
    version=$(resolve_version)
    if [ -z "$version" ]; then
        err "could not resolve latest version from GitHub API"
        exit 1
    fi
    version_bare="${version#v}"

    info "Installing cyoda $version for $os/$arch into $INSTALL_DIR"

    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT

    archive="cyoda_${version_bare}_${os}_${arch}.tar.gz"
    url="https://github.com/$REPO/releases/download/$version/$archive"
    sums_url="https://github.com/$REPO/releases/download/$version/SHA256SUMS"

    info "Downloading $url"
    if ! curl -fsSL -o "$tmp/$archive" "$url"; then
        err "download failed: $url"
        exit 1
    fi
    if ! curl -fsSL -o "$tmp/SHA256SUMS" "$sums_url"; then
        err "download failed: $sums_url"
        exit 1
    fi

    info "Verifying checksum"
    if ! (cd "$tmp" && grep " $archive\$" SHA256SUMS | sha256_check); then
        err "SHA256 verification failed for $archive"
        exit 1
    fi

    cosign_verify "$tmp" "$archive" "$version"

    info "Extracting"
    tar -xzf "$tmp/$archive" -C "$tmp"

    mkdir -p "$INSTALL_DIR"
    mv "$tmp/cyoda" "$INSTALL_DIR/cyoda"
    chmod +x "$INSTALL_DIR/cyoda"

    case ":$PATH:" in
        *":$INSTALL_DIR:"*) : ;;
        *)
            warn "$INSTALL_DIR is not on your PATH."
            warn "Add it by running (for bash):"
            warn "  echo 'export PATH=\"$INSTALL_DIR:\$PATH\"' >> ~/.bashrc"
            warn "or (for zsh):"
            warn "  echo 'export PATH=\"$INSTALL_DIR:\$PATH\"' >> ~/.zshrc"
            ;;
    esac

    info "Running cyoda init"
    if ! "$INSTALL_DIR/cyoda" init; then
        warn "cyoda init failed; cyoda is installed but no user config was written."
        warn "Re-run 'cyoda init' manually once the issue is resolved."
    fi

    info ""
    info "cyoda $version installed."
    info "Start with:"
    info "  cyoda"
    info "See README:"
    info "  https://github.com/$REPO#quick-start"
}

main "$@"
