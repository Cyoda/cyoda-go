#!/usr/bin/env bash
# Test harness for scripts/check-spi-pin-sync.sh.
# Creates a scratch repo with deliberately-drifted go.mods and asserts the
# gate exits non-zero. Then aligns the manifests and asserts the gate exits zero.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
SCRIPT="$REPO_ROOT/scripts/check-spi-pin-sync.sh"

[[ -x "$SCRIPT" ]] || { echo "FAIL: $SCRIPT is missing or not executable"; exit 1; }

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

# Drift case: root v0.7.1, one plugin v0.6.1 → must fail
mkdir -p "$scratch/drift/plugins/a" "$scratch/drift/plugins/b"
cat > "$scratch/drift/go.mod" <<'EOF'
module example.com/root
go 1.26
require github.com/cyoda-platform/cyoda-go-spi v0.7.1
EOF
cat > "$scratch/drift/plugins/a/go.mod" <<'EOF'
module example.com/root/plugins/a
go 1.26
require github.com/cyoda-platform/cyoda-go-spi v0.7.1
EOF
cat > "$scratch/drift/plugins/b/go.mod" <<'EOF'
module example.com/root/plugins/b
go 1.26
require github.com/cyoda-platform/cyoda-go-spi v0.6.1
EOF

if (cd "$scratch/drift" && "$SCRIPT") >/dev/null 2>&1; then
  echo "FAIL: drift case did not fail (expected non-zero)"; exit 1
fi
echo "PASS: drift case correctly failed"

# Aligned case: every manifest at v0.7.1 → must pass
mkdir -p "$scratch/aligned/plugins/a" "$scratch/aligned/plugins/b"
for f in "$scratch/aligned/go.mod" "$scratch/aligned/plugins/a/go.mod" "$scratch/aligned/plugins/b/go.mod"; do
  cat > "$f" <<'EOF'
module example.com
go 1.26
require github.com/cyoda-platform/cyoda-go-spi v0.7.1
EOF
done

if ! (cd "$scratch/aligned" && "$SCRIPT") >/dev/null 2>&1; then
  echo "FAIL: aligned case unexpectedly failed"; exit 1
fi
echo "PASS: aligned case correctly succeeded"

echo "OK: scripts/check-spi-pin-sync.sh exhibits expected behavior"
