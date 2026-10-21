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
sed -i '' 's|json\.Unmarshal(value, &raw)|decodeWithUseNumber(value, \&raw)|g; s|json\.Unmarshal(value, &plain)|decodeWithUseNumber(value, \&plain)|g; s|json\.Unmarshal(value, &v)|decodeWithUseNumber(value, \&v)|g' "$OUT"

# Post-process: the messages these methods return reach the client inside the
# decode error, so they name the member and never a Go type, and a JSON null
# — which holds no member — is refused as missing every required one rather
# than skipping the checks. go-jsonschema writes `raw != nil && !ok`, which
# lets a null through as a zero event, names the struct in "field X in
# FooJson: required", and prints an enum's allowed values in Go syntax.
perl -0777 -i -pe '
  s/if _, ok := raw\["(\w+)"\]; raw != nil && !ok \{/if _, ok := raw["$1"]; !ok {/g;
  s/fmt\.Errorf\("field (\w+) in \w+: required"\)/fmt.Errorf("field $1: required")/g;
  s/fmt\.Errorf\("invalid value \(expected one of %#v\): %#v", (enumValues_\w+), v\)/fmt.Errorf("invalid value %q: expected one of %v", v, $1)/g;
' "$OUT"

# Post-process: a required member that is present but null is refused when its
# schema states a type, which no type here admits null for; go-jsonschema
# checks only that the key is present, so a null decoded as the zero value —
# `"uniqueKeys": null` read as "no keys" and removed every unique key of the
# model. A required member whose schema states no type (the entity data, the
# patch, the model-import sample) holds any JSON value, null included: it is
# the client's data, and the domain refuses a null there with its own code, as
# on HTTP. Those members are read from the schemas and keep the presence check.
ANY_VALUE=$(python3 - "$CLEAN_DIR" <<'EOF'
import json, pathlib, sys
typing = {"type", "$ref", "enum", "const", "oneOf", "anyOf", "allOf"}
out = []
for f in sorted(pathlib.Path(sys.argv[1]).rglob("*.json")):
    doc = json.loads(f.read_text())
    props = doc.get("properties", {})
    for member in doc.get("required", []):
        if member in props and not typing & props[member].keys():
            out.append(doc["title"] + "Json." + member)
print(",".join(out))
EOF
)
ANY_VALUE="$ANY_VALUE" perl -0777 -i -pe '
  BEGIN { %any = map { $_ => 1 } split /,/, $ENV{ANY_VALUE} }
  s{(func \(j \*(\w+)\) UnmarshalJSON\(value \[\]byte\) error \{\n.*?\n\}\n)}{
    my ($fn, $type) = ($1, $2);
    $fn =~ s/\tif _, ok := raw\["(\w+)"\]; !ok \{\n\t\treturn fmt\.Errorf\("field \1: required"\)\n\t\}\n/$any{"$type.$1"} ? $& : "\tif v, ok := raw[\"$1\"]; !ok {\n\t\treturn fmt.Errorf(\"field $1: required\")\n\t} else if v == nil {\n\t\treturn fmt.Errorf(\"field $1: must not be null\")\n\t}\n"/ge;
    $fn
  }gse;
' "$OUT"

# Fail loudly if a required check escaped that rewrite: the presence-only
# checks left are exactly the any-value members.
EXPECTED=$(printf '%s' "$ANY_VALUE" | tr ',' '\n' | grep -c . || true)
PRESENCE_ONLY=$(grep -c 'if _, ok := raw\["' "$OUT" || true)
if [ "$PRESENCE_ONLY" -ne "$EXPECTED" ]; then
  echo "ERROR: $PRESENCE_ONLY presence-only required check(s) in $OUT; want $EXPECTED, one per any-value member ($ANY_VALUE)" >&2
  grep -n 'if _, ok := raw\["' "$OUT" >&2
  exit 1
fi

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

# Post-process: an explicit `"success": null` is not the schema's default.
# go-jsonschema emits `if v, ok := raw["success"]; !ok || v == nil` — the key
# being ABSENT and the key being NULL take the same branch, and both become
# true. Only the first is the default: `success` is declared `"type":
# "boolean"`, so a null is an invalid instance, and the server refuses it as an
# answer it cannot read. A generated type that reads it as success is the one
# reading in the tree that nothing else agrees with. The message names no type,
# so one substitution serves every site.
perl -0777 -i -pe '
  s/\tif v, ok := raw\["success"\]; !ok \|\| v == nil \{\n\t\tplain\.Success = true\n\t\}\n/\tif v, ok := raw["success"]; ok \&\& v == nil {\n\t\treturn fmt.Errorf("field success: must not be null")\n\t} else if !ok {\n\t\tplain.Success = true\n\t}\n/g;
' "$OUT"

# Fail loudly if that substitution stops matching (e.g. go-jsonschema changes
# the shape it emits for a defaulted field): every site must have been rewritten.
LENIENT_NULL=$(grep -c 'raw\["success"\]; !ok || v == nil' "$OUT" || true)
if [ "$LENIENT_NULL" -ne 0 ]; then
  echo "ERROR: $LENIENT_NULL site(s) in $OUT still read an explicit \"success\": null as the default" >&2
  exit 1
fi
STRICT_NULL=$(grep -c 'field success: must not be null' "$OUT" || true)
if [ "$STRICT_NULL" -eq 0 ]; then
  echo "ERROR: no success-null refusals in $OUT; the post-processing substitution matched nothing" >&2
  exit 1
fi

# Fail loudly if a generated decode or message escaped the rewrites above: a
# v1 decode of client data, a null let through the required checks, or a
# message naming a Go type.
LEFT=$(grep -cE 'json\.Unmarshal\(|raw != nil && !ok|%T|%#v| in [A-Za-z0-9_]+: required' "$OUT" || true)
if [ "$LEFT" -ne 0 ]; then
  echo "ERROR: $LEFT generated line(s) in $OUT escaped the decode/message rewrites" >&2
  grep -nE 'json\.Unmarshal\(|raw != nil && !ok|%T|%#v| in [A-Za-z0-9_]+: required' "$OUT" >&2
  exit 1
fi

echo "Generated $OUT ($(wc -l < "$OUT") lines, $STRICT_NULL success-null refusals)"
