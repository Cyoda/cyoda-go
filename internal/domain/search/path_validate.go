package search

import (
	"context"
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// extractFieldPaths walks a predicate.Condition tree and collects every
// JSONPath expression that names a data field. Lifecycle, function and
// nil sub-conditions are skipped — they do not address user-defined
// schema paths.
//
// Returned paths are normalised so they line up with the FieldsMap keys
// produced by *schema.ModelNode.FieldsMap (paths begin with "$." and use
// "[*]" to mark array-wildcard hops). Unrecognised path syntax is
// dropped — pre-execution validation is best-effort and the matcher
// will still fail the request downstream if the path is genuinely
// inaccessible.
//
// Duplicate paths are folded out so callers can rely on the slice as a
// set without further work.
func extractFieldPaths(cond predicate.Condition) []string {
	seen := make(map[string]struct{})
	var out []string
	walkConditionPaths(cond, seen, &out)
	return out
}

func walkConditionPaths(cond predicate.Condition, seen map[string]struct{}, out *[]string) {
	switch c := cond.(type) {
	case nil:
		return
	case *predicate.SimpleCondition:
		addPath(c.JsonPath, seen, out)
	case *predicate.ArrayCondition:
		addPath(c.JsonPath, seen, out)
	case *predicate.GroupCondition:
		for _, child := range c.Conditions {
			walkConditionPaths(child, seen, out)
		}
	case *predicate.LifecycleCondition, *predicate.FunctionCondition:
		// Lifecycle conditions match metadata fields; function conditions
		// are evaluated dynamically. Neither participates in schema-path
		// validation.
		return
	}
}

func addPath(raw string, seen map[string]struct{}, out *[]string) {
	p := normalisePath(raw)
	if p == "" {
		return
	}
	if _, ok := seen[p]; ok {
		return
	}
	seen[p] = struct{}{}
	*out = append(*out, p)
}

// normalisePath rewrites a user-supplied JSONPath into the canonical
// form used as a key by schema.ModelNode.FieldsMap. The canonical form
// always starts with "$." and represents array hops as "[*]". Inputs
// that already lead with "$." pass through untouched. Inputs that omit
// the dollar prefix get one prepended. Empty paths return "" so the
// caller can drop them.
func normalisePath(raw string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(p, "$."):
		return p
	case strings.HasPrefix(p, "$"):
		return p
	default:
		return "$." + p
	}
}

// findUnknownPaths returns the subset of paths absent from the supplied
// FieldsMap. Paths whose direct key is missing are also probed with a
// trailing "[*]" segment stripped, so a condition naming an array field
// (e.g. "$.tags[*]") still matches a leaf descriptor recorded as
// "$.tags[*]" — both representations are accepted to stay compatible
// with the matcher's input shapes.
func findUnknownPaths(paths []string, fields map[string]schema.FieldDescriptor) []string {
	var unknown []string
	for _, p := range paths {
		if isPathKnown(p, fields) {
			continue
		}
		unknown = append(unknown, p)
	}
	return unknown
}

// isPathKnown reports whether p (or a prefix of p that itself addresses
// a structural field) appears in fields. Wildcard suffixes are tolerated
// so "$.tags[*]" matches a leaf described as "$.tags[*]" exactly, and
// nested wildcards such as "$.tags[*].name" also resolve.
func isPathKnown(p string, fields map[string]schema.FieldDescriptor) bool {
	if _, ok := fields[p]; ok {
		return true
	}
	// Prefix-match: a condition path may address an interior object that
	// itself is not a leaf in FieldsMap (which only records leaves). We
	// accept it when at least one recorded leaf descends from the same
	// prefix — evidence that the structural field exists in the schema.
	prefix := p + "."
	for known := range fields {
		if strings.HasPrefix(known, prefix) {
			return true
		}
	}
	return false
}

// loadFieldsMap fetches and parses the cached schema for ref, returning
// the path → FieldDescriptor view used by pre-execution validation.
//
// Returns (nil, nil) when the descriptor has no schema bound. Other errors
// propagate so the caller can log and skip validation rather than
// mistakenly reject the search.
func loadFieldsMap(ctx context.Context, store spi.ModelStore, ref spi.ModelRef) (map[string]schema.FieldDescriptor, error) {
	desc, err := store.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	return fieldsFromDescriptor(desc)
}

// refreshFieldsMap forces a cache refresh via RefreshAndGet (when the
// store implements it) and returns the refreshed FieldsMap. Returns
// (nil, false, nil) when the store has no refresh capability — callers
// should treat that as "no further authority to consult".
func refreshFieldsMap(ctx context.Context, store spi.ModelStore, ref spi.ModelRef) (map[string]schema.FieldDescriptor, bool, error) {
	refresher, ok := store.(interface {
		RefreshAndGet(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error)
	})
	if !ok {
		return nil, false, nil
	}
	desc, err := refresher.RefreshAndGet(ctx, ref)
	if err != nil {
		return nil, true, err
	}
	fm, err := fieldsFromDescriptor(desc)
	if err != nil {
		return nil, true, err
	}
	return fm, true, nil
}

// fieldsFromDescriptor unmarshals desc.Schema and returns its FieldsMap.
// A nil descriptor is treated as "no model registered" and yields a nil
// map without error.
func fieldsFromDescriptor(desc *spi.ModelDescriptor) (map[string]schema.FieldDescriptor, error) {
	if desc == nil {
		return nil, nil
	}
	node, err := schema.Unmarshal(desc.Schema)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal model schema: %w", err)
	}
	return node.FieldsMap(), nil
}
