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
#     cyoda-platform/cyoda-go release workflow.
#   - Disable explicitly: CYODA_COSIGN_VERIFY=false curl ... | sh
#   - Force-fail-without-cosign: CYODA_COSIGN_VERIFY=required curl ... | sh
#     (Aborts if cosign is missing rather than falling back to SHA256-only.)

set -eu

REPO="cyoda-platform/cyoda-go"
INSTALL_DIR="${CYODA_INSTALL_DIR:-$HOME/.local/bin}"
# Cosign keyless verification expects the signing identity to come from
# this OIDC issuer (GitHub Actions) and the cert subject to match a
# workflow path under our org+repo. The release.yml workflow runs with
# `id-token: write` and is the only ref that can mint a matching cert.
COSIGN_OIDC_ISSUER="https://token.actions.githubusercontent.com"
COSIGN_IDENTITY_REGEX="^https://github.com/cyoda-platform/cyoda-go/"

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

# cosign_verify runs keyless Sigstore verification of the archive and
# SHA256SUMS using sibling .sig + .pem assets published by GoReleaser
# (issue #47). Default behaviour:
#   - cosign on PATH AND CYODA_COSIGN_VERIFY != "false" → verify; abort on failure.
#   - cosign missing AND CYODA_COSIGN_VERIFY = "required" → abort.
#   - otherwise → skip with a hint about how to enable.
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
        if ! curl -fsSL -o "$cv_tmp/${cv_target}.sig" "$cv_sig_url"; then
            err "could not download signature: $cv_sig_url"
            exit 1
        fi
        if ! curl -fsSL -o "$cv_tmp/${cv_target}.pem" "$cv_pem_url"; then
            err "could not download certificate: $cv_pem_url"
            exit 1
        fi
        if ! cosign verify-blob \
            --certificate="$cv_tmp/${cv_target}.pem" \
            --signature="$cv_tmp/${cv_target}.sig" \
            --certificate-identity-regexp="$COSIGN_IDENTITY_REGEX" \
            --certificate-oidc-issuer="$COSIGN_OIDC_ISSUER" \
            "$cv_tmp/$cv_target" >/dev/null 2>&1; then
            err "cosign verification FAILED for $cv_target — refusing to install."
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
