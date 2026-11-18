package help

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	cyodaschemas "github.com/cyoda/cyoda-go/docs/cyoda/schema"
)

// envelope matches the documented output shape for `cyoda help cloudevents json`.
// Keep `schemas` as json.RawMessage-ordered via json.Number-safe decode so
// tests can reason about structure without losing numeric precision.
type envelope struct {
	Schema      int                        `json:"schema"`
	Version     string                     `json:"version"`
	SpecVersion string                     `json:"specVersion"`
	BaseID      string                     `json:"baseId"`
	Schemas     map[string]json.RawMessage `json:"schemas"`
}

// TestCloudEventsJSON_CountMatchesEmbed pins that every file in the
// embedded tree surfaces as a key in the emitted map — no silent drops.
func TestCloudEventsJSON_CountMatchesEmbed(t *testing.T) {
	env := emitAndParse(t)

	want := countEmbeddedSchemas(t)
	if len(env.Schemas) != want {
		t.Errorf("schemas count = %d; embed contains %d JSON files", len(env.Schemas), want)
	}
}

// TestCloudEventsJSON_Envelope pins the envelope fields. `version` is
// supplied by the caller at emit time; the test driver uses "test".
func TestCloudEventsJSON_Envelope(t *testing.T) {
	env := emitAndParseWithVersion(t, "v0.6.2-test")

	if env.Schema != 1 {
		t.Errorf("envelope.schema = %d, want 1", env.Schema)
	}
	if env.Version != "v0.6.2-test" {
		t.Errorf("envelope.version = %q, want the supplied version", env.Version)
	}
	if env.SpecVersion != cyodaschemas.MetaSchemaURL {
		t.Errorf("envelope.specVersion = %q, want %q", env.SpecVersion, cyodaschemas.MetaSchemaURL)
	}
	if env.BaseID != cyodaschemas.BaseID {
		t.Errorf("envelope.baseId = %q, want %q", env.BaseID, cyodaschemas.BaseID)
	}
}

// TestCloudEventsJSON_KeysSortedAndShapeValid pins that keys are
// lexicographically sorted in the serialized JSON (diff-stable across
// builds) and that each key looks like a relative path.
func TestCloudEventsJSON_KeysSortedAndShapeValid(t *testing.T) {
	raw := emitRaw(t)

	// Go's encoding/json serializes map keys in sorted order by default.
	// Pin this by re-parsing and walking the raw bytes for the schemas
	// section in order of appearance, then asserting that sequence is
	// already sorted.
	keys := extractSchemaKeysInSerializedOrder(t, raw)
	if len(keys) == 0 {
		t.Fatal("no schema keys found in emitted JSON")
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(keys, sorted) {
		t.Errorf("schema keys not emitted in sorted order:\n got: %v\nwant: %v", keys, sorted)
	}
	for _, k := range keys {
		if strings.HasPrefix(k, "/") || strings.HasPrefix(k, "../") {
			t.Errorf("key %q is not a relative path", k)
		}
		if !strings.HasSuffix(k, ".json") {
			t.Errorf("key %q does not end with .json", k)
		}
	}
}

// TestCloudEventsJSON_ValuesStructurallyMatchEmbed pins the strongest
// acceptance criterion: each emitted value structurally equals the
// corresponding embedded file's parsed JSON (deep equal after parse,
// ignoring whitespace and key ordering).
func TestCloudEventsJSON_ValuesStructurallyMatchEmbed(t *testing.T) {
	env := emitAndParse(t)

	var missing, mismatched []string
	err := fs.WalkDir(cyodaschemas.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".json") {
			return nil
		}
		raw, err := fs.ReadFile(cyodaschemas.FS, p)
		if err != nil {
			return err
		}
		var fromFile any
		if err := json.Unmarshal(raw, &fromFile); err != nil {
			t.Errorf("embedded %s is not valid JSON: %v", p, err)
			return nil
		}
		rawEmit, ok := env.Schemas[p]
		if !ok {
			missing = append(missing, p)
			return nil
		}
		var fromEmit any
		if err := json.Unmarshal(rawEmit, &fromEmit); err != nil {
			t.Errorf("emitted value for %s is not valid JSON: %v", p, err)
			return nil
		}
		if !reflect.DeepEqual(fromFile, fromEmit) {
			mismatched = append(mismatched, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embed: %v", err)
	}
	if len(missing) > 0 {
		t.Errorf("%d embedded schemas missing from output: %v", len(missing), missing[:min(5, len(missing))])
	}
	if len(mismatched) > 0 {
		t.Errorf("%d embedded schemas do not structurally match their emitted value: %v", len(mismatched), mismatched[:min(5, len(mismatched))])
	}
}

// TestCloudEventsJSON_EverySchemaHasSchemaAndID pins that every emitted
// schema carries `$schema` and `$id`, and that `$id` matches the
// expected `BaseID + <relative-path>` pattern.
//
// The structural-equality test would not catch a future regeneration
// that dropped `$schema` or `$id` from every file simultaneously (both
// sides of deep-equal lack it → pass). This test is the positive guard
// against that class of upstream drift.
func TestCloudEventsJSON_EverySchemaHasSchemaAndID(t *testing.T) {
	env := emitAndParse(t)

	for path, rawValue := range env.Schemas {
		var obj map[string]any
		if err := json.Unmarshal(rawValue, &obj); err != nil {
			t.Errorf("%s: value is not a JSON object: %v", path, err)
			continue
		}
		// $schema is required by Draft 2020-12 for standalone schemas.
		if _, ok := obj["$schema"]; !ok {
			t.Errorf("%s: missing $schema field", path)
		}
		// $id is required by the cyoda schema tree convention —
		// every file has one and it must match BaseID + relative-path.
		idRaw, ok := obj["$id"]
		if !ok {
			t.Errorf("%s: missing $id field", path)
			continue
		}
		id, ok := idRaw.(string)
		if !ok {
			t.Errorf("%s: $id is not a string: %v", path, idRaw)
			continue
		}
		want := cyodaschemas.BaseID + path
		if id != want {
			t.Errorf("%s: $id = %q, want %q", path, id, want)
		}
	}
}

// TestCloudEventsJSON_RefsRemainRelative pins that no `$ref` is
// rewritten to an absolute URL — downstream tooling materializes the
// tree to disk and expects relative resolution.
func TestCloudEventsJSON_RefsRemainRelative(t *testing.T) {
	raw := emitRaw(t)
	for _, match := range refRegex.FindAllStringSubmatch(string(raw), -1) {
		target := match[1]
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
			t.Errorf("absolute $ref found (should have stayed relative): %s", target)
		}
	}
}

// TestEmitCloudEventsJSONAction_ExitZero pins that the registered
// action handler writes valid JSON and returns 0 on the happy path.
func TestEmitCloudEventsJSONAction_ExitZero(t *testing.T) {
	var buf bytes.Buffer
	rc := emitCloudEventsJSON(&buf)
	if rc != 0 {
		t.Fatalf("emitCloudEventsJSON rc = %d, output:\n%s", rc, buf.String())
	}
	var env envelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(env.Schemas) != countEmbeddedSchemas(t) {
		t.Errorf("schemas count = %d; embed contains %d", len(env.Schemas), countEmbeddedSchemas(t))
	}
}

// --- helpers ---

func emitRaw(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if rc := emitCloudEventsSchemasTo(&buf, "test"); rc != 0 {
		t.Fatalf("emit rc = %d, output:\n%s", rc, buf.String())
	}
	return buf.Bytes()
}

func emitAndParse(t *testing.T) envelope {
	return emitAndParseWithVersion(t, "test")
}

func emitAndParseWithVersion(t *testing.T, version string) envelope {
	t.Helper()
	var buf bytes.Buffer
	if rc := emitCloudEventsSchemasTo(&buf, version); rc != 0 {
		t.Fatalf("emit rc = %d, output:\n%s", rc, buf.String())
	}
	var env envelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("parse envelope: %v\nraw: %s", err, buf.String())
	}
	return env
}

func countEmbeddedSchemas(t *testing.T) int {
	t.Helper()
	n := 0
	err := fs.WalkDir(cyodaschemas.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".json") {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embed: %v", err)
	}
	return n
}

// refRegex matches `"$ref": "<value>"` across whitespace.
var refRegex = regexp.MustCompile(`"\$ref"\s*:\s*"([^"]+)"`)

// extractSchemaKeysInSerializedOrder returns the "schemas": {…} map
// keys in the exact order they appear in the serialized bytes. Relies
// on encoding/json's deterministic map-key sort to verify ordering.
func extractSchemaKeysInSerializedOrder(t *testing.T, raw []byte) []string {
	t.Helper()
	// Find the "schemas":{ start, then parse keys until the matching }.
	idx := bytes.Index(raw, []byte(`"schemas":`))
	if idx < 0 {
		t.Fatal("emitted JSON has no schemas field")
	}
	// Advance past "schemas": and optional whitespace.
	rest := raw[idx+len(`"schemas":`):]
	rest = bytes.TrimLeft(rest, " \t\r\n")
	if len(rest) == 0 || rest[0] != '{' {
		t.Fatalf("schemas field is not an object; starts with %q", rest[:min(20, len(rest))])
	}
	// Parse the object slice into a decoder to preserve key order.
	dec := json.NewDecoder(bytes.NewReader(rest))
	// Open brace.
	if _, err := dec.Token(); err != nil {
		t.Fatalf("schemas open brace: %v", err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("read key: %v", err)
		}
		key, ok := tok.(string)
		if !ok {
			t.Fatalf("expected string key, got %T %v", tok, tok)
		}
		keys = append(keys, key)
		// Consume the value (any shape).
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			t.Fatalf("consume value for %s: %v", key, err)
		}
	}
	return keys
}
