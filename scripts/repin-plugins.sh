#!/usr/bin/env bash
# Pseudo-version-pin the in-repo plugin submodules (memory, sqlite, postgres) in
# the ROOT go.mod to the current (pushed) HEAD, then tidy the root module so
# go.sum carries no stale plugin lines.
#
# Why: during a coordinated-release window the root module depends on plugin
# code that isn't tagged yet — e.g. an SPI change adds a required method to a
# plugin-implemented interface (StoreFactory, EntityStore) that the old plugin
# tags don't implement. The `test` job stays green via go.work (local plugins),
# but the GOWORK=off jobs (per-module-hygiene, smoke/goreleaser) resolve the
# ROOT go.mod pins as a consumer would — so those pins must point at the in-dev
# plugin code. This pins them to a pseudo-version of HEAD, which must already be
# pushed (go list resolves the commit from origin).
#
# Usage: push your plugin changes, run `make repin-plugins`, then commit the
# resulting go.mod / go.sum; no further tidy is needed. At release cut these
# pseudo-pins are replaced by the real plugin tags (see MAINTAINING.md §3).
#
# Compatible with bash 3.2+ (macOS system bash).
set -euo pipefail

MODULE_BASE="github.com/cyoda/cyoda-go/plugins"
PLUGINS=(memory sqlite postgres)

SHA="$(git rev-parse HEAD)"
echo "Pinning plugins to a pseudo-version of HEAD ${SHA:0:12} (must be pushed to origin)..."

mods=()
for p in "${PLUGINS[@]}"; do
	mod="${MODULE_BASE}/${p}"
	ver="$(GOWORK=off GOFLAGS=-mod=mod go list -m -f '{{.Version}}' "${mod}@${SHA}" 2>/dev/null || true)"
	if [ -z "$ver" ]; then
		echo "ERROR: could not resolve a pseudo-version for ${p}@${SHA}." >&2
		echo "       Is HEAD pushed to origin? (go list resolves the commit from the remote.)" >&2
		exit 1
	fi
	echo "  ${p} -> ${ver}"
	go mod edit -require="${mod}@${ver}"
	mods+=("${mod}")
done

echo "Refreshing go.sum and tidying the root module (GOWORK=off)..."
GOWORK=off go mod download "${mods[@]}"
# download only adds lines; tidy drops the previous pins' lines from go.sum.
GOWORK=off go mod tidy

echo "Verifying GOWORK=off root build and SPI pin-sync..."
GOWORK=off go build ./... >/dev/null
./scripts/check-spi-pin-sync.sh >/dev/null
echo "  pin-sync: OK"

echo "OK: plugins pseudo-pinned to HEAD. Review, then: git add go.mod go.sum && commit."
