package help

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"sync/atomic"

	cyodaschemas "github.com/cyoda/cyoda-go/docs/cyoda/schema"
)

// emitCloudEventsJSON is the CLI action for `cyoda help cloudevents json`.
// Emits the embedded JSON Schema tree as a single JSON document whose
// shape is pinned as:
//
//	{ schema: 1, version, specVersion, baseId, schemas: { <path>: {...} } }
//
// The binary version is read at action-registration time via a package-
// level indirection so tests can inject a fixed value. See actions.go.
func emitCloudEventsJSON(w io.Writer) int {
	return emitCloudEventsSchemasTo(w, binaryVersion())
}

// emitCloudEventsSchemasTo is the version-injectable core. Tests drive
// this directly with a fixed version so envelope assertions are stable
// regardless of the test-harness ldflag state.
func emitCloudEventsSchemasTo(w io.Writer, version string) int {
	schemas, err := loadEmbeddedSchemas()
	if err != nil {
		fmt.Fprintf(w, "cyoda help cloudevents json: %v\n", err)
		return 1
	}
	// Deterministic output: keys sorted lexicographically. Go's
	// encoding/json already sorts map[string]T keys on marshal, but
	// pin it defensively by emitting an ordered intermediate.
	keys := make([]string, 0, len(schemas))
	for k := range schemas {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	ordered := make(map[string]json.RawMessage, len(schemas))
	for _, k := range keys {
		ordered[k] = schemas[k]
	}

	env := struct {
		Schema      int                        `json:"schema"`
		Version     string                     `json:"version"`
		SpecVersion string                     `json:"specVersion"`
		BaseID      string                     `json:"baseId"`
		Schemas     map[string]json.RawMessage `json:"schemas"`
	}{
		Schema:      1,
		Version:     version,
		SpecVersion: cyodaschemas.MetaSchemaURL,
		BaseID:      cyodaschemas.BaseID,
		Schemas:     ordered,
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		fmt.Fprintf(w, "cyoda help cloudevents json: encode: %v\n", err)
		return 1
	}
	return 0
}

// loadEmbeddedSchemas walks the embedded tree and returns a map whose
// keys are relative paths (e.g. "common/BaseEvent.json") and whose
// values are the raw bytes of each schema file, parsed-and-remarshaled
// so the emitted document has stable formatting (source files vary in
// trailing-newline and indentation).
func loadEmbeddedSchemas() (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage)
	err := fs.WalkDir(cyodaschemas.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".json") {
			return nil
		}
		raw, err := fs.ReadFile(cyodaschemas.FS, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		// Parse to validate (catch corruption at test time) and
		// re-marshal to canonicalize formatting. Structural content is
		// preserved — JSON object key order is not semantically
		// significant; downstream consumers deep-equal parsed docs.
		//
		// UseNumber() keeps numeric literals as json.Number instead of
		// coercing to float64 — no current schema carries values above
		// 2^53, but a future `multipleOf` or long-integer default would
		// silently lose precision through a naive float round-trip
		// (the same class of defect). Remarshal of json.Number preserves
		// the original lexical form byte-for-byte.
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var doc any
		if err := dec.Decode(&doc); err != nil {
			return fmt.Errorf("%s: invalid JSON: %w", p, err)
		}
		canon, err := json.Marshal(doc)
		if err != nil {
			return fmt.Errorf("%s: re-marshal: %w", p, err)
		}
		out[p] = canon
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// binaryVersionFn is the accessor used at runtime to stamp the envelope
// `version` field. Package main wires it to the ldflag-injected string
// via SetBinaryVersion (parallel pattern to other help actions).
//
// Stored via atomic.Pointer so concurrent callers of binaryVersion()
// observe a consistent function value; reset mid-read by a late
// SetBinaryVersion can't data-race. The default — returned before any
// setter runs — reports "dev".
var binaryVersionFn atomic.Pointer[func() string]

func init() {
	defaultFn := func() string { return "dev" }
	binaryVersionFn.Store(&defaultFn)
}

func binaryVersion() string {
	fn := binaryVersionFn.Load()
	if fn == nil {
		return "dev"
	}
	return (*fn)()
}

// SetBinaryVersion wires the binary version reporter. Called from
// package main at program start once the ldflag-injected `version`
// variable is available. Safe to call concurrently with binaryVersion()
// — the atomic swap guarantees readers see either the old or new fn,
// never a torn value.
func SetBinaryVersion(fn func() string) {
	if fn == nil {
		return
	}
	binaryVersionFn.Store(&fn)
}
