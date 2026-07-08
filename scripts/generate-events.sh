#!/usr/bin/env bash
# Generates Go structs from Cyoda JSON Schema definitions.
# Requires: go install github.com/atombender/go-jsonschema@latest
#
# Usage: ./scripts/generate-events.sh

set -euo pipefail
cd "$(dirname "$0")/.."

TOOL="${GOPATH:-$HOME/go}/bin/go-jsonschema"
SCHEMA_DIR="docs/cyoda/schema"
OUT="api/grpc/events/types.go"
CLEAN_DIR=$(mktemp -d)

trap 'rm -rf "$CLEAN_DIR"' EXIT

# Copy schemas and pre-process for Go code generation.
cp -r "$SCHEMA_DIR"/* "$CLEAN_DIR/"
python3 -c "
import os, json, glob, re

CLEAN = '$CLEAN_DIR'

# Step 1: Load BaseEvent properties and required fields.
base_event_path = os.path.join(CLEAN, 'common', 'BaseEvent.json')
with open(base_event_path) as fh:
    base_event = json.load(fh)
base_props = base_event.get('properties', {})
base_required = base_event.get('required', [])

# Step 2: Process all schemas.
for f in glob.glob(os.path.join(CLEAN, '**', '*.json'), recursive=True):
    with open(f) as fh:
        content = fh.read()

    # Fix Java-specific 'type': 'any' (not valid JSON Schema).
    content = re.sub(r',?\s*\"existingJavaType\":\s*\"[^\"]*\"', '', content)
    content = content.replace('\"type\": \"any\"', '\"description\": \"arbitrary JSON\"')
    content = re.sub(r',(\s*[}\]])', r'\1', content)

    # Step 3: Inline BaseEvent fields into schemas that extend it.
    try:
        schema = json.loads(content)
    except json.JSONDecodeError:
        with open(f, 'w') as fh:
            fh.write(content)
        continue

    extends = schema.get('extends', {})
    ref = extends.get('\$ref', '') if isinstance(extends, dict) else ''
    if 'BaseEvent.json' in ref:
        # Merge BaseEvent properties into this schema.
        props = schema.get('properties', {})
        for k, v in base_props.items():
            if k not in props:
                props[k] = v
        schema['properties'] = props

        # Merge required fields.
        req = schema.get('required', [])
        for r_field in base_required:
            if r_field not in req:
                req.append(r_field)
        schema['required'] = req

        # Remove the extends field (not standard JSON Schema).
        del schema['extends']

        content = json.dumps(schema, indent=2)

    with open(f, 'w') as fh:
        fh.write(content)
"

mkdir -p "$(dirname "$OUT")"

"$TOOL" \
  --package events \
  --capitalization ID \
  --capitalization UUID \
  --output "$OUT" \
  "$CLEAN_DIR"/common/BaseEvent.json \
  "$CLEAN_DIR"/common/ModelSpec.json \
  "$CLEAN_DIR"/common/DataPayload.json \
  "$CLEAN_DIR"/common/ModelInfo.json \
  "$CLEAN_DIR"/common/DataFormat.json \
  "$CLEAN_DIR"/common/PatchFormat.json \
  "$CLEAN_DIR"/common/ModelConverterType.json \
  "$CLEAN_DIR"/common/EntityChangeMeta.json \
  "$CLEAN_DIR"/common/statemachine/WorkflowInfo.json \
  "$CLEAN_DIR"/common/statemachine/TransitionInfo.json \
  "$CLEAN_DIR"/common/statemachine/ProcessorInfo.json \
  "$CLEAN_DIR"/entity/*.json \
  "$CLEAN_DIR"/model/*.json \
  "$CLEAN_DIR"/search/*.json \
  "$CLEAN_DIR"/processing/*.json

# Post-process: remove omitempty from the 'success' bool field.
# With omitempty, false values are dropped from JSON — but error responses
# MUST include "success": false explicitly.
sed -i '' 's/Success bool `json:"success,omitempty"/Success bool `json:"success"/g' "$OUT"

# Post-process: route every generated UnmarshalJSON call through
# decodeWithUseNumber (see api/grpc/events/use_number.go) so numeric
# literals in freeform fields (map[string]interface{} / interface{}) are
# preserved as json.Number rather than coerced to float64 — precision
# loss above 2^53 otherwise breaks search filters with large-integer
# values (issue #79).
sed -i '' 's|json\.Unmarshal(value, &raw)|decodeWithUseNumber(value, \&raw)|g; s|json\.Unmarshal(value, &plain)|decodeWithUseNumber(value, \&plain)|g' "$OUT"

echo "Generated $OUT ($(wc -l < "$OUT") lines)"
