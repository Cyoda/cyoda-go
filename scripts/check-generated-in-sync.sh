#!/usr/bin/env bash
# Verifies api/generated.go is in sync with api/openapi.yaml — i.e. that
# `go generate ./api/...` produces no change relative to the committed file.
#
# Exits 0 if in sync.
# Exits 1 if regeneration changes generated.go, with a readable diff, so an
# edit to api/openapi.yaml that was not followed by `go generate ./api/...`
# cannot merge.
#
# Used as a CI gate to prevent the generated.go drift that accumulated across
# several PRs (spec edited, generated.go not regenerated) from recurring.
# Compatible with bash 3.2+ (macOS system bash).

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$repo_root"

echo "Regenerating api/generated.go from api/openapi.yaml..."
go generate ./api/...

if git diff --quiet -- api/generated.go; then
	echo "OK: api/generated.go is in sync with api/openapi.yaml."
	exit 0
fi

cat >&2 <<'EOF'
ERROR: api/generated.go is out of sync with api/openapi.yaml.

An edit to api/openapi.yaml was not followed by regeneration. Run:

    go generate ./api/...

and commit the updated api/generated.go. Diff:
EOF
git --no-pager diff -- api/generated.go >&2
exit 1
