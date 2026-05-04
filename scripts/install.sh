#!/bin/sh
# cyoda installer — downloads the latest (or pinned) release for the
# current OS/arch, verifies SHA256, installs to ~/.local/bin/cyoda,
# and runs 'cyoda init' to enable sqlite persistence.
#
# Usage:
#   curl -fsSL https://github.com/cyoda-platform/cyoda-go/releases/latest/download/install.sh | sh
#
# Pin a version:
#   CYODA_VERSION=v0.2.0 curl -fsSL ... | sh
#
# Pin a different install directory:
#   CYODA_INSTALL_DIR=~/bin curl -fsSL ... | sh

set -eu

REPO="cyoda-platform/cyoda-go"
INSTALL_DIR="${CYODA_INSTALL_DIR:-$HOME/.local/bin}"

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
