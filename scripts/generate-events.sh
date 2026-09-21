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
python3 "$(dirname "$0")/events-schema-prep.py" "$CLEAN_DIR"

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
# shellcheck disable=SC2016 # the backticks are the Go struct tag sed matches
sed -i '' 's/Success bool `json:"success,omitempty"/Success bool `json:"success"/g' "$OUT"

# Post-process: route every generated UnmarshalJSON call through
# decodeWithUseNumber (see api/grpc/events/use_number.go) so numeric
# literals in freeform fields (map[string]interface{} / interface{}) are
# preserved as json.Number rather than coerced to float64 — precision
# loss above 2^53 otherwise breaks search filters with large-integer
# values (issue #79).
sed -i '' 's|json\.Unmarshal(value, &raw)|decodeWithUseNumber(value, \&raw)|g; s|json\.Unmarshal(value, &plain)|decodeWithUseNumber(value, \&plain)|g' "$OUT"

# Post-process: type the inbound entity payload fields as json.RawMessage so
# the client's bytes survive to the unstorable-payload guard
# (internal/domain/model/ingest.RejectUnstorable). Decoding them into
# interface{} and re-marshalling silently rewrites unpaired UTF-16
# surrogates and invalid UTF-8 to U+FFFD and collapses duplicate keys, so
# the guard cannot fire and substituted data is stored — exactly what the
# raw-bytes guard exists to prevent. Scoped per struct: only the payload
# fields the gRPC entity ingress forwards verbatim to the entity service.
perl -0777 -i -pe '
  s/(type EntityCreatePayloadJson struct \{[^}]*?\n\tData )interface\{\}/${1}json.RawMessage/s;
  s/(type EntityUpdatePayloadJson struct \{[^}]*?\n\tData )interface\{\}/${1}json.RawMessage/s;
  s/(type EntityPatchPayloadJson struct \{[^}]*?\n\tPatch )interface\{\}/${1}json.RawMessage/s;
' "$OUT"

# Fail loudly if the substitutions stop matching (e.g. a schema restructuring
# renames a field): exactly three payload fields must have been retyped.
# (The generator emits json.RawMessage on its own elsewhere, so count the
# retyped fields specifically, not every occurrence.)
RAW_COUNT=$(grep -cE '^	(Data|Patch) json\.RawMessage ' "$OUT")
if [ "$RAW_COUNT" -ne 3 ]; then
  echo "ERROR: expected exactly 3 retyped Data/Patch json.RawMessage fields in $OUT after post-processing, found $RAW_COUNT" >&2
  exit 1
fi

echo "Generated $OUT ($(wc -l < "$OUT") lines)"
