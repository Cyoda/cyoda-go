#!/usr/bin/env bash
# Test harness for scripts/events-schema-prep.py, the pre-processing step of
# scripts/generate-events.sh. Builds a scratch schema tree and asserts what the
# step does to a composition: BaseEvent's properties and required list are
# inlined, and only the members that were inlined are removed — a schema
# composing with BaseEvent AND something else keeps the something else for the
# generator to resolve.
#
# The real tree has no multi-member allOf today, so that case lives here
# rather than in docs/cyoda/schema's Go tests, which read the tree as shipped.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
PREP="$REPO_ROOT/scripts/events-schema-prep.py"

[[ -x "$PREP" ]] || { echo "FAIL: $PREP is missing or not executable"; exit 1; }

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

mkdir -p "$scratch/common" "$scratch/processing"

cat > "$scratch/common/BaseEvent.json" <<'EOF'
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://cyoda.com/cloud/event/common/BaseEvent.json",
  "type": "object",
  "properties": {
    "success": {"type": "boolean"},
    "id": {"type": "string"}
  },
  "required": ["id"]
}
EOF

cat > "$scratch/common/Extra.json" <<'EOF'
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://cyoda.com/cloud/event/common/Extra.json",
  "type": "object",
  "properties": {"extra": {"type": "string"}}
}
EOF

# Composes with BaseEvent alone — the shape all 47 shipped schemas have.
cat > "$scratch/processing/One.json" <<'EOF'
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://cyoda.com/cloud/event/processing/One.json",
  "type": "object",
  "allOf": [
    {"$ref": "../common/BaseEvent.json"}
  ],
  "properties": {"matches": {"type": "boolean"}},
  "required": ["matches"]
}
EOF

# Composes with BaseEvent AND something else.
cat > "$scratch/processing/Two.json" <<'EOF'
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://cyoda.com/cloud/event/processing/Two.json",
  "type": "object",
  "allOf": [
    {"$ref": "../common/BaseEvent.json"},
    {"$ref": "../common/Extra.json"}
  ],
  "properties": {"matches": {"type": "boolean"}},
  "required": ["matches"]
}
EOF

python3 "$PREP" "$scratch"

# jq is not assumed; read the results with python3, which the step needs anyway.
check() {
  local label="$1" expr="$2"
  if ! python3 - "$scratch" "$expr" <<'EOF'
import json, sys
scratch, expr = sys.argv[1], sys.argv[2]
one = json.load(open(scratch + '/processing/One.json'))
two = json.load(open(scratch + '/processing/Two.json'))
sys.exit(0 if eval(expr) else 1)
EOF
  then
    echo "FAIL: $label"; exit 1
  fi
  echo "PASS: $label"
}

check "BaseEvent's properties are inlined into a single-member composition" \
  "'success' in one['properties'] and 'id' in one['properties']"
check "BaseEvent's required list is merged, keeping the schema's own" \
  "sorted(one['required']) == ['id', 'matches']"
check "a composition with nothing left to resolve drops allOf entirely" \
  "'allOf' not in one"

check "BaseEvent's properties are inlined into a multi-member composition" \
  "'success' in two['properties'] and 'id' in two['properties']"
check "the other member of a multi-member composition survives" \
  "two.get('allOf') == [{'\$ref': '../common/Extra.json'}]"

echo "OK: scripts/events-schema-prep.py exhibits expected behavior"
