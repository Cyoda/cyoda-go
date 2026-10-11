package entity

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"

	"github.com/tidwall/gjson"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/consistency"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/match"
)

// ErrInvalidCondition wraps a predicate.ParseCondition failure on the
// conditional-delete path. The HTTP handler maps it (via errors.Is) to 400
// INVALID_CONDITION. The sentinel is needed because the predicate package
// returns plain fmt.Errorf values, so the handler has no other typed signal
// to tell a malformed condition (client fault) from a storage error.
var ErrInvalidCondition = errors.New("invalid condition")

// GroupedStatsService is the per-request dispatcher described in spec §4.
// It decides between native pushdown (spi.GroupedAggregator) and the
// streaming-tally fallback (EntityStore.Iterate + in-process accumulator).
type GroupedStatsService struct {
	maxBuckets int
	cons       *consistency.Service
}

// NewGroupedStatsService constructs a service with the configured
// cardinality ceiling. The ceiling is the value of CYODA_STATS_GROUP_MAX
// and is enforced inside both the pushdown and the streaming branches.
func NewGroupedStatsService(maxBuckets int, cons *consistency.Service) *GroupedStatsService {
	if cons == nil {
		panic("entity.NewGroupedStatsService: nil consistency service")
	}
	return &GroupedStatsService{maxBuckets: maxBuckets, cons: cons}
}

// QueryGroupedStats runs the grouped-stats query and translates the known
// domain/SPI sentinels into client-facing *common.AppError before
// returning, so every transport (HTTP now, gRPC later) surfaces the
// documented status without a per-handler switch. Unknown storage/driver
// errors are returned unchanged and surface as 500 via the transport's
// Internal fallback.
func (s *GroupedStatsService) QueryGroupedStats(
	ctx context.Context,
	store spi.EntityStore,
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

// classifyGroupedStatsError maps the known sentinels to operational
// AppErrors (each wrapping the sentinel via WithCause so errors.Is still
// holds); any other error is returned unchanged (surfaces as 500 at the
// transport).
func classifyGroupedStatsError(err error) error {
	switch {
	case errors.Is(err, spi.ErrGroupCardinalityExceeded):
		return common.Operational(http.StatusUnprocessableEntity, common.ErrCodeGroupCardinalityExceeded,
			"group cardinality exceeds the configured maximum").WithCause(err)
	case errors.Is(err, spi.ErrInvalidFilterPath),
		errors.Is(err, spi.ErrUnevaluableLeaf),
		errors.Is(err, spi.ErrInvalidPattern),
		errors.Is(err, match.ErrUnevaluableLeaf),
		errors.Is(err, match.ErrUnsupportedOperator):
		// spi.ErrInvalidFilterPath is a backend's own backstop rejecting a path
		// outside the model's syntax; request validation has already refused
		// such a path, so it surfaces here only from a backend.
		//
		// spi.ErrUnevaluableLeaf / spi.ErrInvalidPattern reach here from the
		// streaming tally's own store.Iterate call (tallyStreaming passes it
		// the pushdown Filter — with Declared possibly empty for a
		// bare-typeless field), or from a GroupedAggregator pushdown attempt.
		//
		// match.ErrUnevaluableLeaf / match.ErrUnsupportedOperator reach here
		// from the criterion evaluator this package shares, not from grouped
		// stats itself: the streaming tally has no in-process predicate of
		// its own any more.
		//
		// All five used to propagate raw, with no case here to catch them, so
		// they fell through to the generic 500 below — the identical defect
		// SearchService.Search's own match.Prepare/Iterate call sites had
		// before search.ClassifyStoreQueryError learned these sentinels (see
		// that function's doc). Delegating keeps one mapping table instead of
		// several.
		return search.ClassifyStoreQueryError(err)
	}
	return err
}

// queryGroupedStatsInner dispatches a validated grouped-stats request
// against any storage backend. Every backend implements Iterate, so the
// query always has an execution path; GroupedAggregator is the optional
// pushdown on top of it.
//
// Decision tree (spec §4, decisions D11/D14):
//  1. Native pushdown — only when (a) store implements GroupedAggregator,
//     (b) the store accepts the shape, AND (c) we're not inside a
//     transaction (D11: tx visibility requires the streaming path).
//  2. Otherwise stream Iterate with the translated filter and tally.
//
// A condition that will not translate is refused before either branch, so
// neither ever sees a filter that does not match the request. Spec §4's D15
// — pass a zero-value filter and re-apply the predicate per yielded entity —
// is retired with the whole-model-scan family it belonged to.
func (s *GroupedStatsService) queryGroupedStatsInner(
	ctx context.Context,
	store spi.EntityStore,
	model spi.ModelRef,
	fields map[string]schema.FieldDescriptor,
	req *ValidatedGroupedStatsRequest,
) ([]GroupedStatsBucket, error) {
	// A nil Condition is the "match all" case (no predicate filtering). The
	// request carries it already parsed.
	parsedCond := req.Condition

	// The condition was validated by the handler through
	// search.ValidateConditionForModel (structure, patterns, paths, types),
	// the one sequence every surface runs; this layer translates and
	// executes it.

	// Translate to a pushdown Filter. A nil parsedCond yields the zero-value
	// Filter ("match all").
	//
	// One path, as on search and conditional delete: the condition either
	// translates or the request is refused. A translation failure is
	// unreachable from input that cleared search.ValidateConditionForModel —
	// the same pairing search runs, checked by
	// TestDeleteAndGroupedStats_ClearingImpliesTranslates — so refusing
	// costs no valid request.
	//
	// Translating is not the same as pushing down: a wildcard leaf still has
	// no SQL form on either backend (each SQL planner's isLeafPushable
	// routes it to that backend's own residual, see
	// spi.ErrAggregationNotPushdownable below), so a
	// successfully-translated wildcard Filter can still fall through to the
	// streaming branch below. That residual is the backend's, applied to a
	// filter it was actually given — not a whole-model scan re-matched in
	// this process.
	var pushFilter spi.Filter
	if parsedCond != nil {
		f, terr := spi.ConditionToFilter(parsedCond, fields)
		if terr != nil {
			if appErr := search.ClassifyStoreQueryError(terr); appErr != nil {
				return nil, appErr
			}
			return nil, common.Operational(http.StatusBadRequest,
				common.ErrCodeInvalidCondition,
				"condition cannot be translated to a backend predicate")
		}
		pushFilter = f
	}

	// Every request error above wins over the fence; a read at an instant
	// later than the consistency time is refused before any backend runs.
	if req.PointInTime != nil {
		if err := s.cons.Fence(ctx, *req.PointInTime); err != nil {
			return nil, err
		}
	}

	inTx := spi.GetTransaction(ctx) != nil

	// 1. Native pushdown branch.
	if ga, ok := store.(spi.GroupedAggregator); ok && !inTx {
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

	// 2. Stream and tally.
	return s.tallyStreaming(ctx, store, model, fields, req, pushFilter)
}

// tallyStreaming implements the spec §4 streaming branch: iterate the
// translated filter, group, accumulate, materialize.
//
// It used to carry a residual of its own: when the condition would not
// translate, it passed a zero-value filter and re-applied the predicate per
// yielded entity — a whole-model scan matched in this process. The caller
// refuses a translation failure now, so pushFilter is always what the
// condition translated to and every yielded entity already matches.
func (s *GroupedStatsService) tallyStreaming(
	ctx context.Context,
	store spi.EntityStore,
	model spi.ModelRef,
	fields map[string]schema.FieldDescriptor,
	req *ValidatedGroupedStatsRequest,
	pushFilter spi.Filter,
) ([]GroupedStatsBucket, error) {
	iter, err := store.Iterate(ctx, model, pushFilter, spi.IterateOptions{PointInTime: req.PointInTime})
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
		// `iter.Err()` call that executed first) can miss it. Err() is
		// read after Close() everywhere an iterator is drained.
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
