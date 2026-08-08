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

// LoadFieldsMap is the exported entry point that resolves a model's declared
// field-type map (path → FieldDescriptor). In-process predicate evaluators
// (the workflow engine's criterion matcher, the grouped-stats streaming
// residual) use it to type their leaves consistently with the search path, so
// the type-directed kernel compares temporal data fields temporally rather than
// lexically. Returns (nil, nil) when the model has no schema bound; genuine
// store errors propagate for the caller to surface (fail closed).
//
// The returned map may be owned by the model-descriptor cache and shared with
// every other reader holding the same lease. It is read-only — do not mutate it.
func LoadFieldsMap(ctx context.Context, store spi.ModelStore, ref spi.ModelRef) (map[string]schema.FieldDescriptor, error) {
	return loadFieldsMap(ctx, store, ref)
}

// loadFieldsMap fetches and parses the cached schema for ref, returning
// the path → FieldDescriptor view used by pre-execution validation.
//
// Returns (nil, nil) when the descriptor has no schema bound. Other errors
// propagate so the caller can log and skip validation rather than
// mistakenly reject the search.
func loadFieldsMap(ctx context.Context, store spi.ModelStore, ref spi.ModelRef) (map[string]schema.FieldDescriptor, error) {
	// Prefer the cached derived parse when the store offers one. Rebuilding it
	// per call is 80-99% of a criterion evaluation and scales with schema size;
	// FieldsMap() off an already-parsed node is effectively free. Stores that
	// do not implement the interface (tests, plain in-memory stores) keep the
	// original behaviour.
	if p, ok := store.(schemaNodeProvider); ok {
		node, err := p.SchemaNode(ctx, ref)
		if err != nil {
			return nil, err
		}
		if node == nil {
			return nil, nil
		}
		return node.FieldsMap(), nil
	}
	desc, err := store.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	return fieldsFromDescriptor(desc)
}

// schemaNodeProvider is the optional capability a caching model store exposes to
// hand back the schema it has already parsed, instead of the bytes to parse
// again. Same optional-interface shape as the RefreshAndGet probe below.
type schemaNodeProvider interface {
	SchemaNode(context.Context, spi.ModelRef) (*schema.ModelNode, error)
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
	// RefreshAndGet repopulates the cache entry, parsed node included, so read
	// the parse back rather than re-deriving it from the bytes we just caused
	// to be cached — that reparse is exactly what the cache exists to remove.
	if p, ok := store.(schemaNodeProvider); ok {
		node, nerr := p.SchemaNode(ctx, ref)
		if nerr != nil {
			return nil, true, nerr
		}
		if node == nil {
			return nil, true, nil
		}
		return node.FieldsMap(), true, nil
	}
	fm, err := fieldsFromDescriptor(desc)
	if err != nil {
		return nil, true, err
	}
	return fm, true, nil
}

// fieldsFromDescriptor unmarshals desc.Schema and returns its FieldsMap.
// A nil descriptor OR a descriptor with no schema bound (empty Schema bytes)
// is treated as "no schema to type against" and yields a nil map without error
// — the (nil,nil) case callers degrade on. This mirrors loadModelNode's
// `len(desc.Schema) == 0` guard, so the two schema-load entry points agree on
// what counts as "no schema" versus a genuine parse error (non-empty but
// unparseable bytes), which still surfaces so callers can fail closed.
func fieldsFromDescriptor(desc *spi.ModelDescriptor) (map[string]schema.FieldDescriptor, error) {
	if desc == nil || len(desc.Schema) == 0 {
		return nil, nil
	}
	node, err := schema.Unmarshal(desc.Schema)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal model schema: %w", err)
	}
	return node.FieldsMap(), nil
}
