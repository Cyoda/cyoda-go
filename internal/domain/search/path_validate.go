package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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
// "[*]" to mark array-wildcard hops).
//
// Nothing downstream re-checks what this misses. The matcher has no
// field-path check at all: an inaccessible path resolves to nothing and
// answers an empty page, which the caller cannot tell from a legitimate
// empty result. Path syntax is grammar-checked at the model-independent
// condition boundary (ValidateCondition) before this walk runs, so a
// surviving path is well-formed and the walk drops nothing.
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
func normalisePath(raw string) string { return spi.NormalisePath(raw) }

// findUnknownPaths returns the subset of paths absent from the supplied
// FieldsMap, in the caller's own spelling. See [isPathKnown] for what counts
// as present.
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
//
// A POSITIONAL subscript ("$.arr[0]") resolves too. It is valid JSON Path, the
// boundary grammar accepts it, and the in-memory evaluator serves it — but the
// schema records an array's element once under the wildcard key and has no
// per-index entry to find, so a raw comparison rejected the condition 400 for a
// field the model declares. Only the LOOKUP is canonicalised; the caller's
// original spelling is what gets reported, so a diagnostic names the path the
// request actually sent.
func isPathKnown(p string, fields map[string]schema.FieldDescriptor) bool {
	if pathOrContainerKnown(p, fields) {
		return true
	}
	if canon := schema.CanonicalFieldPath(p); canon != p {
		return pathOrContainerKnown(canon, fields)
	}
	return false
}

// pathOrContainerKnown reports whether p is a recorded leaf, or an interior
// node — object OR array — with at least one recorded leaf beneath it.
//
// Both descent forms count, via the shared [isKnownContainerPath] predicate.
// Testing only the dotted one missed the ARRAY container: FieldsMap records an
// array's element under the "[*]" key and never the container itself, so for
// `tags: ["red","blue"]` the only entry is "$.tags[*]" and a condition on
// "$.tags" — the natural spelling for an ArrayCondition — was reported unknown
// and answered 400 INVALID_FIELD_PATH for a field the model declares.
func pathOrContainerKnown(p string, fields map[string]schema.FieldDescriptor) bool {
	if _, ok := fields[p]; ok {
		return true
	}
	// Prefix-match: a condition path may address an interior node that itself
	// is not a leaf in FieldsMap (which only records leaves). We accept it when
	// at least one recorded leaf descends from it — evidence that the
	// structural field exists in the schema.
	return isKnownContainerPath(p, fields)
}

// findUnknownSortPaths is [findUnknownPaths]' SORT-key counterpart: it
// applies resolveOrderBy's own membership test — an EXACT key in fields,
// with no container or array-wildcard widening — rather than isPathKnown's
// permissive CONDITION-path test.
//
// A sort key must denote a single scalar leaf (resolveOrderBy rejects a
// container or an array path with errUnknownSortField), so the negative
// cache resolveSortKeys populates from this result must agree with that
// exactly. Using findUnknownPaths/isPathKnown here instead — as this
// function replaced — silently no-ops the cache for those two shapes: a
// sort key naming an interior node ("$.address" when only
// "$.address.street" is declared) or an array container ("$.tags" when the
// schema records only the wildcard leaf "$.tags[*]") is "known" under the
// condition-path predicate's prefix matching, so the computed missing set
// is empty, markPathsAbsent is a no-op, and every repeat request pays a
// full authoritative model-store read plus a schema re-parse instead of
// short-circuiting on the negative cache.
func findUnknownSortPaths(paths []string, fields map[string]schema.FieldDescriptor) []string {
	var unknown []string
	for _, p := range paths {
		if isSortPathKnown(p, fields) {
			continue
		}
		unknown = append(unknown, p)
	}
	return unknown
}

// isSortPathKnown reports whether p is recorded in fields as an exact key —
// the same lookup resolveOrderBy performs (`fields[key]`) before it ever
// checks IsArray or resolves an ordering kind. Canonicalisation still
// applies, matching resolveOrderBy's own normalisePath call on the key, but
// no container or array-container widening: a sort key names a leaf field
// exactly, or it is unknown to this predicate.
func isSortPathKnown(p string, fields map[string]schema.FieldDescriptor) bool {
	if _, ok := fields[p]; ok {
		return true
	}
	if canon := schema.CanonicalFieldPath(p); canon != p {
		_, ok := fields[canon]
		return ok
	}
	return false
}

// FindUnknownFieldPaths returns the data-field JSONPaths cond references
// that are absent from fields (typically obtained via LoadFieldsMap).
// Exported so a caller outside this package that selects entities via its
// own Iterate drain instead of Search — currently entity.Handler's
// delete paths, which reuse the search condition primitive per design §6.1
// but cannot call Search's unexported validateConditionPaths directly —
// can still reject a condition naming an unknown schema field before
// ConditionToFilter would otherwise silently under-match it (see
// ConditionToFilter's doc comment on why an unknown path degrades instead
// of erroring). Mirrors validateConditionPaths' path-existence check
// without its negative-cache/refresh-retry optimisation, which is a
// Search-hot-path concern; a caller that also needs the refresh-retry
// behaviour should route through Search instead.
func FindUnknownFieldPaths(cond predicate.Condition, fields map[string]schema.FieldDescriptor) []string {
	return findUnknownPaths(extractFieldPaths(cond), fields)
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
// Returns (nil, nil) when the descriptor has no schema bound — a model
// declaring no fields, which is a real answer and not a failure. Every other
// error propagates and every caller FAILS the request on it: the schema is
// what decides whether a condition's paths exist, so proceeding without it
// answers with a filter whose comparison leaves annihilate while its string
// leaves keep matching (see spi.ConditionToFilter).
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

// RefreshFieldsMap is the exported entry point for refreshFieldsMap, mirroring
// LoadFieldsMap's exported-wrapper pattern. Callers outside this package that
// reuse the search condition primitive on their own Iterate drain
// (currently entity.Handler's delete path) need the same bounded
// single-refresh-then-fail behaviour validateConditionPaths gives Search,
// rather than declaring a field unknown against a possibly-stale cached
// descriptor — see FindUnknownFieldPaths' doc comment for why the caller
// can't reach validateConditionPaths directly. Returns (nil, false, nil)
// when the store has no refresh capability; the caller should then treat the
// pre-refresh unknown-paths result as authoritative, exactly as
// validateConditionPaths does.
func RefreshFieldsMap(ctx context.Context, store spi.ModelStore, ref spi.ModelRef) (map[string]schema.FieldDescriptor, bool, error) {
	return refreshFieldsMap(ctx, store, ref)
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

// ConditionFieldPaths returns every data-field JSONPath cond names, normalised
// to the FieldsMap key convention. Lifecycle, function and nil sub-conditions
// contribute nothing. Duplicates are folded out.
//
// Exported so a caller outside this package can assemble a path set spanning
// more than a condition — grouped stats also holds its groupBy paths and
// aggregate fields to the model — and hand the whole set to
// [ValidateKnownPaths] in one call.
func ConditionFieldPaths(cond predicate.Condition) []string {
	return extractFieldPaths(cond)
}

// ValidateKnownPaths holds every path in paths to the fields the model
// declares, returning the fields map they validated against.
//
// A path absent from fields triggers exactly ONE RefreshAndGet before the
// request is refused, and the recheck decides. That refresh is the correctness
// half, not an optimisation: on a cluster, node A can extend a model with a new
// field while node B's cached descriptor still predates the schema-change
// event, and without it node B answers 400 for a field the model genuinely has
// while the identical request succeeds on node A. Bounded to one attempt so a
// misconfigured client cannot amplify into a refresh storm.
//
// A nil fields map is not "nothing to check against": it is a model declaring
// no fields, in which every path is unknown.
//
// The returned error is either a 400 INVALID_FIELD_PATH *common.AppError
// naming the paths that remain unknown, or — when the bounded refresh itself
// failed — a plain error wrapping [ErrPathRefreshInfra] (see that sentinel's
// doc for why the two are deliberately NOT the same classification). On
// success the returned map is the refreshed one when a refresh happened, so
// the caller types its leaves against the schema the paths were actually
// validated against rather than the stale one.
func ValidateKnownPaths(
	ctx context.Context,
	modelStore spi.ModelStore,
	ref spi.ModelRef,
	paths []string,
	fields map[string]schema.FieldDescriptor,
) (map[string]schema.FieldDescriptor, error) {
	if len(paths) == 0 {
		return fields, nil
	}
	unknown := findUnknownPaths(paths, fields)
	if len(unknown) == 0 {
		return fields, nil
	}

	freshFields, refreshed, refreshErr := RefreshFieldsMap(ctx, modelStore, ref)
	switch {
	case !refreshed:
		// No cache layer to refresh — the miss is authoritative.
	case refreshErr != nil && errors.Is(refreshErr, spi.ErrNotFound):
		// The model was deleted between the two reads, so there is no
		// schema authority left and the miss stands.
	case refreshErr != nil:
		// A refresh failure that is NOT "the model is gone" means we cannot
		// tell "this field is genuinely undeclared" from "the cache is
		// merely stale and we couldn't confirm which" — the two are
		// indistinguishable without a successful refresh. Per
		// correctness-over-availability this is infrastructure, not a
		// client fault, and must not fold into the same 400
		// INVALID_FIELD_PATH the genuine-unknown-path case gets below: a
		// caller that did that would report a model-store outage as the
		// caller's own mistake, in exactly the peer-added-field window this
		// refresh exists to serve.
		slog.Warn("schema refresh failed during field-path validation; reporting infra, not a client fault",
			"pkg", "search", "entityName", ref.EntityName,
			"modelVersion", ref.ModelVersion, "error", refreshErr)
		return nil, fmt.Errorf("%w: schema refresh failed for %s/%s: %w",
			ErrPathRefreshInfra, ref.EntityName, ref.ModelVersion, refreshErr)
	case freshFields != nil:
		unknown = findUnknownPaths(unknown, freshFields)
		fields = freshFields
	}
	if len(unknown) > 0 {
		return nil, invalidPathError(unknown)
	}
	return fields, nil
}

// ErrPathRefreshInfra marks a failed bounded schema refresh inside
// [ValidateKnownPaths]: RefreshAndGet itself errored (for any reason other
// than [spi.ErrNotFound], which means the model was legitimately deleted
// between the two reads). A caller must classify this as a server-side
// infrastructure failure (5xx), not fold it into the client-facing 400
// INVALID_FIELD_PATH the genuine-unknown-path case returns — the whole point
// of the bounded refresh is to tell "genuinely undeclared" apart from "cache
// is stale", and a failed refresh answers neither question.
//
// Deliberately a plain sentinel-wrapped error, not a pre-classified
// *common.AppError: this package has no HTTP-status opinion of its own for
// an infra failure (each caller already has its own ticketed 5xx convention
// — internal/domain/entity's grouped-stats handler and classifyError, and
// the workflow engine's ErrCriterionTypingInfra), and every existing caller
// of ValidateKnownPaths already defaults a non-AppError error to a 5xx
// (verified: grouped_stats_handler.go's `errors.As` fallback to
// common.Internal, and entity/service.go's classifyError catch-all) — so no
// caller needed new code to pick this branch up correctly, only workflow's
// evaluateCriterion (which has its own, more specific ErrCriterionTypingInfra
// wrapping) needed a deliberate carve-out. Wrapped with %w on both sides so
// errors.Is(err, ErrPathRefreshInfra) AND errors.Is(err, <the underlying
// refresh error>) both hold — a context.DeadlineExceeded riding inside the
// refresh failure must stay reachable for a caller further up the chain that
// specifically detects it (e.g. common.ClassifyRequestTimeout).
var ErrPathRefreshInfra = errors.New("model schema refresh failed")
