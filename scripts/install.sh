#!/bin/sh
# cyoda installer — downloads the latest (or pinned) release for the
# current OS/arch, verifies SHA256 (and cosign keyless signature when
# cosign is available), installs to ~/.local/bin/cyoda, and runs
# 'cyoda init' to enable sqlite persistence.
#
# Usage:
#   curl -fsSL https://github.com/cyoda-platform/cyoda-go/releases/latest/download/install.sh | sh
#
# Pin a version:
#   CYODA_VERSION=v0.2.0 curl -fsSL ... | sh
#
# Pin a different install directory:
#   CYODA_INSTALL_DIR=~/bin curl -fsSL ... | sh
#
# Cosign verification (issue #47):
#   - Auto-on when `cosign` is on PATH. Verifies the archive and
#     SHA256SUMS against keyless Sigstore signatures issued by the
#     cyoda-platform/cyoda-go release.yml workflow on a tag push.
#   - Disable explicitly: CYODA_COSIGN_VERIFY=false curl ... | sh
#   - Force-fail-without-cosign: CYODA_COSIGN_VERIFY=required curl ... | sh
#     (Aborts if cosign is missing rather than falling back to SHA256-only.)
#
# Auto-mode trade-off (security note): cosign signing was added in
# v0.7.0; older releases have no .sig files. To preserve backward
# compat with the curl-pipe-to-sh UX for users without cosign, auto
# mode treats a 404 on the .sig as "release predates cosign signing"
# and falls back to SHA256-only. This means an attacker who controls
# the GitHub release page on a v0.7.0+ release can bypass cosign by
# deleting the .sig along with their archive swap — SHA256 against
# their own SHA256SUMS would still pass. For hardened/production
# deployments, set CYODA_COSIGN_VERIFY=required: missing .sig is
# then a hard error, not a fallback.

set -eu

REPO="cyoda-platform/cyoda-go"
INSTALL_DIR="${CYODA_INSTALL_DIR:-$HOME/.local/bin}"
# Cosign keyless verification expects the signing identity to come from
# this OIDC issuer (GitHub Actions) and the cert subject to match the
# release.yml workflow path under our org+repo, fired from a tag push
# (not workflow_dispatch from an arbitrary branch). release.yml is the
# only workflow with `id-token: write`, and we additionally pin to the
# `refs/tags/v*` ref-suffix so a future branch-based dispatch (even
# from main with proper protection) can't mint a cert that this
# verifier accepts.
COSIGN_OIDC_ISSUER="https://token.actions.githubusercontent.com"
COSIGN_IDENTITY_REGEX="^https://github\.com/cyoda-platform/cyoda-go/\.github/workflows/release\.yml@refs/tags/v"
# Bind to push-triggered runs from a tag ref. release.yml's `on:`
# allows both `push: tags` and `workflow_dispatch`; only the former
# can produce signed release artifacts that consumers should trust,
# since workflow_dispatch can be triggered from any branch by anyone
# with write access.
COSIGN_GH_WORKFLOW_TRIGGER="push"
COSIGN_GH_WORKFLOW_REF_REGEX="^refs/tags/v"

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
    curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
        | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' \
        | head -n1
}

# cosign_fetch downloads a release asset and prints its HTTP status to
# stdout (so callers can distinguish 200, 404, and other failures).
# Unlike curl -fsSL, a 404 here does NOT terminate the script.
cosign_fetch() {
    cf_url=$1
    cf_dest=$2
    curl -sS -o "$cf_dest" -w "%{http_code}" "$cf_url" 2>/dev/null || printf '000'
}

# cosign_verify runs keyless Sigstore verification of the archive and
# SHA256SUMS using sibling .sig + .pem assets published by GoReleaser
# (issue #47). Default behaviour:
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
            --certificate-github-workflow-ref-regexp="$COSIGN_GH_WORKFLOW_REF_REGEX" \
            "$cv_tmp/$cv_target" >"$cv_log" 2>&1; then
            err "cosign verification FAILED for $cv_target — refusing to install."
            err "cosign output (last 5 lines):"
            tail -n 5 "$cv_log" >&2 || :
            err "Re-run with CYODA_COSIGN_VERIFY=false to skip (not recommended)."
            exit 1
        fi
    done
}

main() {
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
            warn "  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.bashrc"
            warn "or (for zsh):"
            warn "  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.zshrc"
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
