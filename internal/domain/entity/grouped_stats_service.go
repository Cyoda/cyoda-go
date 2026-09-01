package entity

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"

	"github.com/tidwall/gjson"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/match"
)

// ErrBackendNotSupported is returned when the storage backend supports
// neither spi.Iterable nor spi.GroupedAggregator. The HTTP handler maps
// this to 501 NOT_IMPLEMENTED_BY_BACKEND.
var ErrBackendNotSupported = errors.New("backend supports neither Iterable nor GroupedAggregator")

// ErrInvalidCondition wraps any predicate.ParseCondition failure that
// surfaces from the service-layer dispatch. The HTTP handler maps this
// (via errors.Is) to 400 INVALID_CONDITION per spec §3. We need the
// sentinel because the predicate package returns plain fmt.Errorf
// values, so the handler has no other typed signal to distinguish a
// malformed condition (client fault) from an upstream/storage error
// (5xx with ticket).
var ErrInvalidCondition = errors.New("invalid condition")

// GroupedStatsService is the per-request dispatcher described in spec §4.
// It decides between native pushdown (spi.GroupedAggregator) and the
// streaming-tally fallback (spi.Iterable + in-process accumulator).
type GroupedStatsService struct {
	maxBuckets int
}

// NewGroupedStatsService constructs a service with the configured
// cardinality ceiling. The ceiling is the value of CYODA_STATS_GROUP_MAX
// and is enforced inside both the pushdown and the streaming branches.
func NewGroupedStatsService(maxBuckets int) *GroupedStatsService {
	return &GroupedStatsService{maxBuckets: maxBuckets}
}

// QueryGroupedStats runs the grouped-stats query and translates the known
// domain/SPI sentinels into client-facing *common.AppError before
// returning, so every transport (HTTP now, gRPC later) surfaces the
// documented status without a per-handler switch. Unknown storage/driver
// errors are returned unchanged and surface as 500 via the transport's
// Internal fallback.
func (s *GroupedStatsService) QueryGroupedStats(
	ctx context.Context,
	store any,
	model spi.ModelRef,
	fields map[string]schema.FieldDescriptor,
	req *ValidatedGroupedStatsRequest,
) ([]GroupedStatsBucket, error) {
	buckets, err := s.queryGroupedStatsInner(ctx, store, model, fields, req)
	if err != nil {
		return nil, classifyGroupedStatsError(err)
	}
	return buckets, nil
}

// classifyGroupedStatsError maps the seven known sentinels to operational
// AppErrors (each wrapping the sentinel via WithCause so errors.Is still
// holds); any other error is returned unchanged (surfaces as 500 at the
// transport).
func classifyGroupedStatsError(err error) error {
	switch {
	case errors.Is(err, ErrBackendNotSupported):
		return common.Operational(http.StatusNotImplemented, common.ErrCodeNotImplementedByBackend,
			"backend does not support grouped stats").WithCause(err)
	case errors.Is(err, spi.ErrGroupCardinalityExceeded):
		return common.Operational(http.StatusUnprocessableEntity, common.ErrCodeGroupCardinalityExceeded,
			"group cardinality exceeds the configured maximum").WithCause(err)
	case errors.Is(err, ErrInvalidCondition):
		return common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition, err.Error()).WithCause(err)
	case errors.Is(err, search.ErrInvalidCondition):
		// The queryGroupedStatsInner ValidateConditionValueTypes call
		// (below) propagates this sentinel unwrapped, not re-wrapped under
		// this package's own ErrInvalidCondition above — same disposition
		// (400 INVALID_CONDITION) via the search package's own sentinel, for
		// an operator that is not a supported predicate for the field it's
		// applied to (e.g. a string/pattern operator on a temporal meta
		// field, operator-semantics.md §4/§7).
		return common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition, err.Error()).WithCause(err)
	case errors.Is(err, search.ErrInvalidFieldPath):
		return common.Operational(http.StatusBadRequest, common.ErrCodeInvalidFieldPath, err.Error()).WithCause(err)
	case errors.Is(err, spi.ErrInvalidFilterPath),
		errors.Is(err, spi.ErrUnevaluableLeaf),
		errors.Is(err, spi.ErrInvalidPattern),
		errors.Is(err, match.ErrUnevaluableLeaf),
		errors.Is(err, match.ErrUnsupportedOperator):
		// spi.ErrInvalidFilterPath is the PLUGIN-side twin of the arm above: a
		// backend's own backstop rejecting a path outside the model's syntax.
		//
		// spi.ErrUnevaluableLeaf / spi.ErrInvalidPattern reach here from the
		// streaming-tally fallback's own store.Iterate call (tallyStreaming
		// passes it the pushdown Filter — with Declared possibly empty for a
		// bare-typeless field — the same way SearchService.Search's Iterate
		// branch does), or from a GroupedAggregator pushdown attempt.
		//
		// match.ErrUnevaluableLeaf / match.ErrUnsupportedOperator reach here
		// from tallyStreaming's OWN match.Prepare call (the residual
		// evaluator, used only when the condition doesn't translate to a
		// pushdown Filter at all).
		//
		// All five used to propagate raw, with no case here to catch them, so
		// they fell through to the generic 500 below — the identical defect
		// SearchService.Search's own match.Prepare/Iterate call sites had
		// before search.ClassifyStoreQueryError learned these sentinels (see
		// that function's doc). Delegating keeps one mapping table instead of
		// several.
		return search.ClassifyStoreQueryError(err)
	case errors.Is(err, search.ErrConditionTypeMismatch):
		return common.Operational(http.StatusBadRequest, common.ErrCodeConditionTypeMismatch, err.Error()).WithCause(err)
	}
	return err
}

// queryGroupedStatsInner dispatches a validated grouped-stats request
// against any storage backend. The store parameter is intentionally `any`
// — capabilities are detected via type assertion so a backend can satisfy
// one or both of spi.Iterable / spi.GroupedAggregator.
//
// Decision tree (spec §4, decisions D11/D14/D15):
//  1. Native pushdown — only when (a) store implements GroupedAggregator,
//     (b) the request's Condition translates cleanly to spi.Filter, AND
//     (c) we're not inside a transaction (D11: tx visibility requires the
//     streaming path).
//  2. Streaming fallback — when store implements Iterable. If the filter
//     translates, push it; otherwise pass zero-value and re-apply the
//     prepared predicate (match.Prepare/(Prepared).Match) per yielded
//     entity (D15).
//  3. Neither — return ErrBackendNotSupported (handler maps to 501).
func (s *GroupedStatsService) queryGroupedStatsInner(
	ctx context.Context,
	store any,
	model spi.ModelRef,
	fields map[string]schema.FieldDescriptor,
	req *ValidatedGroupedStatsRequest,
) ([]GroupedStatsBucket, error) {
	// Parse Condition once. A nil/empty Condition is the "match all" case
	// (no predicate filtering). Any parse error here is the first sign of
	// a malformed condition — surface it so the handler can return 400.
	var parsedCond predicate.Condition
	if len(req.Condition) > 0 {
		c, err := predicate.ParseCondition(req.Condition)
		if err != nil {
			// Wrap with ErrInvalidCondition sentinel so the handler can
			// route this to 400 INVALID_CONDITION via errors.Is. The
			// underlying predicate error message is preserved in the
			// chain (and surfaces in server-side logs) but the client
			// sees a stable error code.
			return nil, fmt.Errorf("%w: %v", ErrInvalidCondition, err)
		}
		parsedCond = c
	}

	// Structural condition validation (canonical operator set, BETWEEN
	// arity) — model-independent, mirrors the single boundary the search
	// path enforces in SearchService.Search/SubmitAsync via the same
	// search.ValidateCondition call. Without this, a malformed-arity BETWEEN
	// (or an unknown operatorType) slips past every downstream layer here:
	// ConditionToFilter's translate-time check catches some shapes, but both
	// it and match.Prepare now FAIL CLOSED WITH AN ERROR on an unevaluable
	// leaf (never silently non-matching) — so without this earlier,
	// better-classified check the request would surface as a generic
	// internal error instead of a clean 400 INVALID_CONDITION naming the
	// actual structural fault.
	if parsedCond != nil {
		if cErr := search.ValidateCondition(parsedCond); cErr != nil {
			// A jsonPath outside JSON Path nomenclature is propagated
			// unwrapped so classifyGroupedStatsError sees the
			// search.ErrInvalidFieldPath sentinel and emits INVALID_FIELD_PATH
			// — the same code /search returns for the same input. Re-wrapping
			// in ErrInvalidCondition would shadow it: that arm is tested first.
			if errors.Is(cErr, search.ErrInvalidFieldPath) {
				return nil, cErr
			}
			return nil, fmt.Errorf("%w: %v", ErrInvalidCondition, cErr)
		}
	}

	// Reject a MATCHES_PATTERN or LIKE operand the kernel cannot compile,
	// before any backend runs. Every plugin's residual filter evaluator
	// (sqlite's evaluateFilter, postgres's evalPostFilter) delegates to the
	// error-free spi.PreparedFilter.Match kernel, which returns false
	// (non-match) rather than erroring on a bad pattern — so an unvalidated
	// malformed pattern would silently under-include buckets instead of
	// failing the request. Validating here, in the backend-independent domain
	// layer, makes every backend reject identically. It runs after the
	// structural validation above, as it does on the search path: the pattern
	// error names the leaf by the jsonPath the caller wrote, and that string
	// should have cleared the path grammar first.
	if parsedCond != nil {
		if rErr := search.ValidatePatterns(parsedCond); rErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidCondition, rErr)
		}
	}

	// Condition type-soundness (correctness-over-availability): mirrors the
	// search path's validateConditionTypes boundary for its model-independent
	// parts — the lifecycle/temporal/meta-field rules validateLifecycleType
	// enforces (known meta field; a supported operator + RFC3339 operand on
	// temporal fields), which need no schema. Without this, e.g. a CONTAINS
	// operator against the temporal creationDate meta field would silently
	// produce an empty result here instead of the 400 INVALID_CONDITION the
	// equivalent /search request returns.
	//
	// A nil model is passed because this layer has none. The SCHEMA-dependent
	// arm — an operand parsing into none of a declared field's types — runs at
	// the handler, which holds the model store and validates the condition's,
	// groupBy's and aggregates' paths against the model in the same place. A
	// direct caller of this service that bypasses the handler gets only the
	// model-independent half, which is why the handler is the boundary.
	if parsedCond != nil {
		if tErr := search.ValidateConditionValueTypes(nil, parsedCond); tErr != nil {
			// Propagate tErr directly (not re-wrapped): it already wraps
			// search.ErrConditionTypeMismatch, search.ErrInvalidCondition or
			// search.ErrInvalidFieldPath, so classifyGroupedStatsError
			// classifies it via errors.Is against those same exported
			// sentinels — the identical classification the search path's
			// validateConditionTypes performs — and maps to the matching
			// CONDITION_TYPE_MISMATCH / INVALID_CONDITION / INVALID_FIELD_PATH
			// code.
			return nil, tErr
		}
	}

	// Try to translate to a pushdown-friendly Filter. A nil parsedCond
	// yields the zero-value Filter ("match all"); a parsedCond that the
	// translator can't handle (a function condition — the kernel now
	// resolves a subscripted/wildcard path directly, see spi.ResolvePath, so
	// that shape TRANSLATES like any other rather than erroring here)
	// returns an error — in that case the streaming branch will re-apply the
	// prepared predicate (match.Prepare/(Prepared).Match) per entity.
	// Translating is not the same as pushing down: a wildcard leaf still has
	// no SQL form on either backend (each SQL planner's isLeafPushable
	// routes it to the residual, see spi.ErrAggregationNotPushdownable
	// below), so a successfully-translated wildcard Filter can still fall
	// through to the streaming branch below.
	var pushFilter spi.Filter
	pushable := true
	if parsedCond != nil {
		f, terr := spi.ConditionToFilter(parsedCond, fields)
		if terr != nil {
			pushable = false
		} else {
			pushFilter = f
		}
	}

	inTx := spi.GetTransaction(ctx) != nil

	// 1. Native pushdown branch.
	if ga, ok := store.(spi.GroupedAggregator); ok && !inTx && pushable {
		spiGroups := translateGroupBy(req.GroupBy)
		spiAggs := translateAggregations(req.Aggregations)
		out, err := ga.GroupedAggregate(ctx, model, spiGroups, pushFilter, spi.GroupedAggregationsOptions{
			PointInTime:  req.PointInTime,
			MaxBuckets:   s.maxBuckets,
			Aggregations: spiAggs,
		})
		if err == nil {
			return postProcessPushdown(out, req), nil
		}
		if !errors.Is(err, spi.ErrAggregationNotPushdownable) {
			return nil, err
		}
		// Plugin declined this shape; fall through to streaming.
	}

	// 2. Streaming fallback.
	if it, ok := store.(spi.Iterable); ok {
		return s.tallyStreaming(ctx, it, model, fields, req, pushFilter, pushable, parsedCond)
	}

	// 3. Neither capability.
	return nil, ErrBackendNotSupported
}

// tallyStreaming implements the spec §4 streaming branch: iterate, apply
// any unpushable residual via a prepared match.Prepared, group, accumulate,
// materialize.
func (s *GroupedStatsService) tallyStreaming(
	ctx context.Context,
	it spi.Iterable,
	model spi.ModelRef,
	fields map[string]schema.FieldDescriptor,
	req *ValidatedGroupedStatsRequest,
	pushFilter spi.Filter,
	pushable bool,
	parsedCond predicate.Condition,
) ([]GroupedStatsBucket, error) {
	// Declared-type resolver for the residual predicate evaluation below, so the
	// streaming path types data leaves consistently with the pushdown filter
	// (both stamped from `fields`). A nil `fields` yields a nil-returning
	// resolver, which the evaluator tolerates.
	fieldTypes := func(p string) []spi.DataType {
		if fd, ok := fields[p]; ok {
			return fd.Types
		}
		return nil
	}
	// Prepared once, only when there is actually a residual to apply. The
	// guard mirrors the one in the loop below exactly: preparing an unused
	// condition would resolve declared types for a query that never evaluates
	// it.
	var residual match.Prepared
	if !pushable && parsedCond != nil {
		p, err := match.Prepare(parsedCond, fieldTypes)
		if err != nil {
			return nil, err
		}
		residual = p
	}

	// D15: if the filter wasn't pushable, pass zero-value to the iterator
	// (match-all) and re-apply the residual inside the loop. Otherwise
	// trust the plugin to apply pushFilter itself.
	iterFilter := pushFilter
	if !pushable {
		iterFilter = spi.Filter{}
	}

	iter, err := it.Iterate(ctx, model, iterFilter, spi.IterateOptions{PointInTime: req.PointInTime})
	if err != nil {
		return nil, err
	}

	acc := newAccumulators(req)
	// bucketErr (a definitive business-logic stop, not a scan fault) takes
	// priority over scanErr when both are set — it mirrors the pre-fix
	// code's immediate `return nil, spi.ErrGroupCardinalityExceeded`, which
	// never consulted iter.Err() at all.
	var bucketErr, scanErr error
	func() {
		// Close() before Err(), inside a defer so both run even on the
		// early return below (bucketErr) — the trap this reorders away
		// from: some iterator implementations only surface a sticky scan
		// error at Close, not at the last Next(), so reading Err() before
		// Close() runs (the previous shape here, with a bare
		// `defer iter.Close()` registered ahead of a same-function
		// `iter.Err()` call that executed first) can miss it. Mirrors
		// drainIterate's ordering (internal/domain/search/service.go).
		defer func() {
			if closeErr := iter.Close(); closeErr != nil {
				scanErr = closeErr
			}
			if errErr := iter.Err(); errErr != nil {
				scanErr = errErr
			}
		}()
		for iter.Next() {
			e := iter.Entity()

			// Residual predicate evaluation: only when the original condition
			// was not pushable and we therefore need to filter per entity.
			if !pushable && parsedCond != nil && !residual.Match(e.Data, e.Meta) {
				continue
			}

			keyValues, groupKey := buildGroupKeyFromEntity(req.GroupBy, e)
			k := buildGroupKey(keyValues)
			if !acc.has(k) && acc.len() >= s.maxBuckets {
				bucketErr = spi.ErrGroupCardinalityExceeded
				return
			}
			numerics := extractNumerics(req.Aggregations, e.Data)
			acc.observe(k, groupKey, numerics)
		}
	}()
	if bucketErr != nil {
		return nil, bucketErr
	}
	if scanErr != nil {
		return nil, scanErr
	}
	return acc.materialize(), nil
}

// buildGroupKeyFromEntity extracts the per-entry values for both the map
// key (raw any slice) and the response groupKey ([]GroupKeyEntryWire).
//
// Per spec D4, only object/array runtime values (non-scalar) coerce to
// null; scalar strings, numbers, and booleans become real key values.
// Numbers and booleans use their JSON text representation (res.Raw),
// matching the postgres equivalent `doc->>'field'` which returns the
// canonical text form ("1", "true"). Missing fields and explicit JSON
// null also coerce to nil.
func buildGroupKeyFromEntity(groups []GroupExprValidated, e *spi.Entity) ([]any, []GroupKeyEntryWire) {
	rawVals := make([]any, len(groups))
	keys := make([]GroupKeyEntryWire, len(groups))
	for i, g := range groups {
		var path string
		var val any
		if g.IsState {
			path = "state"
			if e.Meta.State != "" {
				val = e.Meta.State
			}
		} else {
			path = g.Path
			res := resolveScalarPath(e.Data, g.Path)
			switch {
			case !res.Exists():
				val = nil
			case res.Type == gjson.String:
				val = res.String()
			case res.Type == gjson.Number:
				// Canonical text form of the JSON number — matches
				// postgres's `doc->>'field'` behaviour for cross-backend
				// consistency.
				val = res.Raw
			case res.Type == gjson.True || res.Type == gjson.False:
				// "true" / "false" verbatim.
				val = res.Raw
			default:
				// gjson.Null, gjson.JSON (object/array) — coerce to nil
				// per spec D4 (non-scalar runtime values).
				val = nil
			}
		}
		rawVals[i] = val
		keys[i] = GroupKeyEntryWire{Path: path, Value: val}
	}
	return rawVals, keys
}

// extractNumerics returns one float64 per aggregation. NaN signals
// "skip" (non-numeric, missing, null) — observe() in the accumulator
// drops NaN/Inf samples per D4.
func extractNumerics(aggs []AggregationExprValidated, data []byte) []float64 {
	out := make([]float64, len(aggs))
	for i, a := range aggs {
		res := resolveScalarPath(data, a.Field)
		if !res.Exists() || res.Type != gjson.Number {
			out[i] = math.NaN()
			continue
		}
		out[i] = res.Float()
	}
	return out
}

// resolveScalarPath resolves a normalized groupBy/aggregation JSONPath
// ("$.foo.bar" or "foo.bar") against data through [spi.ParseFilterPath] and
// [spi.ResolvePath] — the same addressing rule every other resolver in the
// stack applies — rather than gjson's own path syntax, which resolves an
// all-digit segment against an ARRAY receiver as a positional index. That
// divergence let "$.obj.0" over {"obj":["X","Y"]} return "X" here while
// spi.ResolvePath, and both SQL backends, correctly report it absent (a
// field literally named "0" is not the same address as element 0).
//
// ValidateScalarJSONPath has already rejected any subscript on this surface,
// so this is always a 0-or-1-value resolution: absent, or the single value
// the path names. The reserved token "state" is handled by callers via
// IsState and never reaches here.
func resolveScalarPath(data []byte, p string) gjson.Result {
	if len(p) >= 2 && p[0] == '$' && p[1] == '.' {
		p = p[2:]
	}
	hops, err := spi.ParseFilterPath(p)
	if err != nil {
		return gjson.Result{}
	}
	results := spi.ResolvePath(data, hops)
	if len(results) != 1 {
		return gjson.Result{}
	}
	return results[0]
}

// translateGroupBy maps the validation-layer types to the SPI types used
// by the pushdown plugin. The leading "$." (kept by normalizeScalarPath
// for the wire-shape group-key Path) is stripped here because every
// plugin's validateJSONPath rejects "$" as a disallowed character — the
// SPI contract is a bare dotted-identifier path ("foo.bar"), and the
// "$." prefix lives only at the public surface (response GroupKeyEntry.Path
// and the in-process resolveScalarPath call, which strips it the same way).
func translateGroupBy(groups []GroupExprValidated) []spi.GroupExpr {
	out := make([]spi.GroupExpr, len(groups))
	for i, g := range groups {
		if g.IsState {
			out[i] = spi.GroupExpr{Kind: spi.GroupExprState}
		} else {
			out[i] = spi.GroupExpr{Kind: spi.GroupExprDataPath, Path: stripJSONPathPrefix(g.Path)}
		}
	}
	return out
}

// translateAggregations applies the same "$." stripping rule as
// translateGroupBy to the aggregation field — the plugin's path
// validator is shared between group-by and aggregation paths.
func translateAggregations(aggs []AggregationExprValidated) []spi.AggregateExpr {
	out := make([]spi.AggregateExpr, len(aggs))
	for i, a := range aggs {
		out[i] = spi.AggregateExpr{
			Op:    spi.AggregateOp(a.Op),
			Field: stripJSONPathPrefix(a.Field),
			Alias: a.Alias,
		}
	}
	return out
}

// stripJSONPathPrefix removes the leading "$." that normalizeScalarPath
// preserves for the wire-shape group-key. Plugins (memory's own
// resolveScalarPath helper in plugins/memory/grouped_stats.go, sqlite/
// postgres's validateJSONPath) all expect bare dotted-identifier paths —
// plugins/memory is a separate Go module with no dependency on the root
// module, so it cannot import internal/match at all; its group-by path
// handling is entirely local (built on spi.ParseFilterPath/spi.ResolvePath,
// the same SPI both this package and plugins/memory already depend on). A
// path without the prefix is returned unchanged so the helper is idempotent
// — re-applying it is safe.
func stripJSONPathPrefix(p string) string {
	if len(p) >= 2 && p[0] == '$' && p[1] == '.' {
		return p[2:]
	}
	return p
}

// restoreJSONPathPrefix re-attaches the "$." prefix on the pushdown
// response path so it matches the streaming-branch wire shape. We use
// the validated request's GroupBy as the authoritative source rather
// than re-deriving the prefix from the plugin's response: the reserved
// token "state" never carries a "$." prefix even though it is also a
// bare identifier; the request's IsState flag is what tells us which
// shape to emit.
func restoreJSONPathPrefix(pluginPath string, groups []GroupExprValidated, i int) string {
	if i < len(groups) && groups[i].IsState {
		return "state"
	}
	if len(pluginPath) >= 2 && pluginPath[0] == '$' && pluginPath[1] == '.' {
		return pluginPath
	}
	return "$." + pluginPath
}

// postProcessPushdown converts the plugin's []GroupedAggregateBucket into
// the service's []GroupedStatsBucket, applies the D12 total order, and
// truncates to req.Limit. The plugin is responsible for the aggregations
// values themselves; we only re-shape the keys, normalize missing
// alias entries to JSON null, sort, and limit.
//
// The bucket's GroupKey path is rewritten from the plugin's bare-dotted
// form ("variantId") back to the wire-shape "$." prefix form ("$.variantId")
// so that the response matches what the streaming branch produces. The
// reserved token "state" is preserved verbatim — IsState entries never
// carry the "$." prefix.
func postProcessPushdown(buckets []spi.GroupedAggregateBucket, req *ValidatedGroupedStatsRequest) []GroupedStatsBucket {
	out := make([]GroupedStatsBucket, 0, len(buckets))
	for _, b := range buckets {
		keys := make([]GroupKeyEntryWire, len(b.GroupKey))
		for i, k := range b.GroupKey {
			keys[i] = GroupKeyEntryWire{Path: restoreJSONPathPrefix(k.Path, req.GroupBy, i), Value: k.Value}
		}
		bucket := GroupedStatsBucket{
			GroupKey: keys,
			Count:    b.Count,
		}
		if len(req.Aggregations) > 0 {
			bucket.Aggregations = make(map[string]any, len(req.Aggregations))
			for _, a := range req.Aggregations {
				if v, ok := b.Aggregations[a.Alias]; ok {
					bucket.Aggregations[a.Alias] = v
				} else {
					bucket.Aggregations[a.Alias] = nil
				}
			}
		}
		out = append(out, bucket)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return compareGroupKey(out[i].GroupKey, out[j].GroupKey) < 0
	})
	if req.Limit != nil && *req.Limit < len(out) {
		out = out[:*req.Limit]
	}
	return out
}
