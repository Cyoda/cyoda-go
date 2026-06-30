// Package parity — oracle.go
//
// Deterministic in-memory oracles used by B parity tests. These
// helpers produce the bytes that a byte-identical fold MUST return
// for the named input sequence, computed via importer.Walk +
// schema.Extend + exporter.SimpleViewExporter.Export. Backends
// matching these bytes satisfy B-I1 at the HTTP boundary.
package parity

import (
	"bytes"
	"encoding/json"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/exporter"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// expectedSimpleViewFromBodies computes the canonical SIMPLE_VIEW
// bytes for a sequence of JSON bodies applied sequentially at
// ChangeLevelStructural. The first body seeds the schema; each
// subsequent body is Walk + Extend.
//
// currentState is baked into the exporter output ("LOCKED" for
// post-lock tests, "UNLOCKED" otherwise).
func expectedSimpleViewFromBodies(bodies []map[string]string, currentState string) ([]byte, error) {
	var current *schema.ModelNode
	for i, body := range bodies {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("oracle: marshal body %d: %w", i, err)
		}
		// importer.Walk requires json.Number for numeric values — strings here
		// won't hit that path, but UseNumber keeps the oracle robust if the
		// sequence is extended with numeric fields in the future.
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var parsed any
		if err := dec.Decode(&parsed); err != nil {
			return nil, fmt.Errorf("oracle: parse body %d: %w", i, err)
		}
		walked, err := importer.Walk(parsed)
		if err != nil {
			return nil, fmt.Errorf("oracle: walk body %d: %w", i, err)
		}
		if current == nil {
			current = walked
			continue
		}
		next, err := schema.Extend(current, walked, spi.ChangeLevelStructural)
		if err != nil {
			return nil, fmt.Errorf("oracle: extend body %d: %w", i, err)
		}
		current = next
	}
	if current == nil {
		return nil, nil
	}
	return exporter.NewSimpleViewExporter(currentState).Export(current)
}

// expectedSimpleViewFromSequence computes the canonical SIMPLE_VIEW
// bytes for the n-field-widening sequence used by B-I1 byte-identity
// tests. Body 0 has only field_0; body n-1 has field_0..field_{n-1}.
func expectedSimpleViewFromSequence(n int, currentState string) ([]byte, error) {
	bodies := make([]map[string]string, 0, n)
	for i := 0; i < n; i++ {
		body := map[string]string{}
		for j := 0; j <= i; j++ {
			body[fmt.Sprintf("field_%d", j)] = fmt.Sprintf("v%d", j)
		}
		bodies = append(bodies, body)
	}
	return expectedSimpleViewFromBodies(bodies, currentState)
}

// expectedSimpleViewFromExtensions computes the canonical SIMPLE_VIEW
// bytes for a sequence of arbitrary JSON-parsed values (e.g. produced by
// gentree.GenValue). Each value is run through the same pipeline the
// backend runs on a CreateEntity under a structural ChangeLevel:
//
//  1. importer.Walk           (handler.validateOrExtend)
//  2. schema.Extend           (handler.validateOrExtend)
//  3. schema.Diff             (handler.validateOrExtend)
//  4. schema.Apply            (plugin's injected ApplyFunc on ExtendSchema)
//
// The extension is accepted only when ALL four steps succeed. Any
// earlier step failing → the schema is kept unchanged and the next
// extension is attempted, mirroring the HTTP rollback on rejection.
// The post-Apply node is used as the new current, mirroring the
// plugin's behaviour: the on-disk schema bytes are what Apply(base,
// delta) produces, NOT the Extend output. These can differ when Diff
// chooses a non-identity delta encoding of the same logical widening.
//
// Accepted indices are returned so callers can cross-check with
// backend acceptance for divergence diagnostics.
func expectedSimpleViewFromExtensions(extensions []any, currentState string) ([]byte, []int, error) {
	var current *schema.ModelNode
	accepted := make([]int, 0, len(extensions))
	for i, ext := range extensions {
		walked, err := importer.Walk(ext)
		if err != nil {
			// A Walk error means the extension is structurally malformed —
			// the HTTP stack will reject it with the same root cause. Skip.
			continue
		}
		if current == nil {
			// Backend parity: ImportModel persists
			// schema.Marshal(walked) and every subsequent read goes
			// through schema.Unmarshal, so the in-memory ModelNode seen
			// by the next Extend call is the codec round-trip of the
			// initial Walk output, not the raw Walk output. That
			// round-trip can normalize representations (e.g. array
			// multiset cardinality rewrites) — the oracle must mirror
			// it on the seed path too.
			seedBytes, err := schema.Marshal(walked)
			if err != nil {
				return nil, accepted, fmt.Errorf("oracle: marshal seed %d: %w", i, err)
			}
			normalized, err := schema.Unmarshal(seedBytes)
			if err != nil {
				return nil, accepted, fmt.Errorf("oracle: unmarshal seed %d: %w", i, err)
			}
			current = normalized
			accepted = append(accepted, i)
			continue
		}
		extended, err := schema.Extend(current, walked, spi.ChangeLevelStructural)
		if err != nil {
			// Shape-incompatible at STRUCTURAL — oracle and backend both
			// reject here. Keep current unchanged.
			continue
		}
		// Backend parity: handler computes schema.Diff(current, extended)
		// and 500s on error. A Diff failure is therefore a rejection in
		// the oracle too, even when Extend succeeded.
		delta, err := schema.Diff(current, extended)
		if err != nil {
			continue
		}
		if delta == nil {
			// Semantic no-op — handler returns early without updating
			// the schema. Oracle keeps current unchanged but records
			// acceptance (the HTTP call returns 200).
			accepted = append(accepted, i)
			continue
		}
		// Backend parity: the plugin's ExtendSchema replay function (see
		// app.makeSchemaApply) runs schema.Marshal(schema.Apply(
		// schema.Unmarshal(baseBytes), delta)), so the stored schema is
		// round-tripped through the codec after every accepted extension.
		// That round-trip normalizes representations (e.g. array
		// multiset cardinality rewrites to the canonical codec form),
		// which the oracle must mirror to byte-match. An Apply error
		// surfaces as a 500 and leaves the schema unchanged.
		baseBytes, err := schema.Marshal(current)
		if err != nil {
			return nil, accepted, fmt.Errorf("oracle: marshal base before apply %d: %w", i, err)
		}
		base, err := schema.Unmarshal(baseBytes)
		if err != nil {
			return nil, accepted, fmt.Errorf("oracle: unmarshal base before apply %d: %w", i, err)
		}
		applied, err := schema.Apply(base, delta)
		if err != nil {
			continue
		}
		appliedBytes, err := schema.Marshal(applied)
		if err != nil {
			return nil, accepted, fmt.Errorf("oracle: marshal after apply %d: %w", i, err)
		}
		normalized, err := schema.Unmarshal(appliedBytes)
		if err != nil {
			return nil, accepted, fmt.Errorf("oracle: unmarshal after apply %d: %w", i, err)
		}
		current = normalized
		accepted = append(accepted, i)
	}
	if current == nil {
		return nil, accepted, nil
	}
	out, err := exporter.NewSimpleViewExporter(currentState).Export(current)
	if err != nil {
		return nil, accepted, fmt.Errorf("oracle: export: %w", err)
	}
	return out, accepted, nil
}
