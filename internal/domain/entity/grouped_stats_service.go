package entity

import (
	"context"
	"errors"
	"math"
	"sort"

	"github.com/tidwall/gjson"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/match"
)

// ErrBackendNotSupported is returned when the storage backend supports
// neither spi.Iterable nor spi.GroupedAggregator. The HTTP handler maps
// this to 501 NOT_IMPLEMENTED_BY_BACKEND.
var ErrBackendNotSupported = errors.New("backend supports neither Iterable nor GroupedAggregator")

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

// QueryGroupedStats dispatches a validated grouped-stats request against
// any storage backend. The store parameter is intentionally `any` —
// capabilities are detected via type assertion so a backend can satisfy
// one or both of spi.Iterable / spi.GroupedAggregator.
//
// Decision tree (spec §4, decisions D11/D14/D15):
//  1. Native pushdown — only when (a) store implements GroupedAggregator,
//     (b) the request's Condition translates cleanly to spi.Filter, AND
//     (c) we're not inside a transaction (D11: tx visibility requires the
//     streaming path).
//  2. Streaming fallback — when store implements Iterable. If the filter
//     translates, push it; otherwise pass zero-value and re-apply
//     match.Match per yielded entity (D15).
//  3. Neither — return ErrBackendNotSupported (handler maps to 501).
func (s *GroupedStatsService) QueryGroupedStats(
	ctx context.Context,
	store any,
	model spi.ModelRef,
	req *ValidatedGroupedStatsRequest,
) ([]GroupedStatsBucket, error) {
	// Parse Condition once. A nil/empty Condition is the "match all" case
	// (no predicate filtering). Any parse error here is the first sign of
	// a malformed condition — surface it so the handler can return 400.
	var parsedCond predicate.Condition
	if len(req.Condition) > 0 {
		c, err := predicate.ParseCondition(req.Condition)
		if err != nil {
			return nil, err
		}
		parsedCond = c
	}

	// Try to translate to a pushdown-friendly Filter. A nil parsedCond
	// yields the zero-value Filter ("match all"); a parsedCond that the
	// translator can't handle (e.g. function conditions, wildcard paths)
	// returns an error — in that case the streaming branch will re-apply
	// match.Match per entity.
	var pushFilter spi.Filter
	pushable := true
	if parsedCond != nil {
		f, terr := search.ConditionToFilter(parsedCond)
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
		return s.tallyStreaming(ctx, it, model, req, pushFilter, pushable, parsedCond)
	}

	// 3. Neither capability.
	return nil, ErrBackendNotSupported
}

// tallyStreaming implements the spec §4 streaming branch: iterate, apply
// any unpushable residual via match.Match, group, accumulate, materialize.
func (s *GroupedStatsService) tallyStreaming(
	ctx context.Context,
	it spi.Iterable,
	model spi.ModelRef,
	req *ValidatedGroupedStatsRequest,
	pushFilter spi.Filter,
	pushable bool,
	parsedCond predicate.Condition,
) ([]GroupedStatsBucket, error) {
	// D15: if the filter wasn't pushable, pass zero-value to the iterator
	// (match-all) and re-apply match.Match inside the loop. Otherwise
	// trust the plugin to apply pushFilter itself.
	iterFilter := pushFilter
	if !pushable {
		iterFilter = spi.Filter{}
	}

	iter, err := it.Iterate(ctx, model, iterFilter, spi.IterateOptions{PointInTime: req.PointInTime})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	acc := newAccumulators(req)
	for iter.Next() {
		e := iter.Entity()

		// Residual predicate evaluation: only when the original condition
		// was not pushable and we therefore need to filter per entity.
		if !pushable && parsedCond != nil {
			ok, mErr := match.Match(parsedCond, e.Data, e.Meta)
			if mErr != nil {
				return nil, mErr
			}
			if !ok {
				continue
			}
		}

		keyValues, groupKey := buildGroupKeyFromEntity(req.GroupBy, e)
		k := buildGroupKey(keyValues)
		if !acc.has(k) && acc.len() >= s.maxBuckets {
			return nil, spi.ErrGroupCardinalityExceeded
		}
		numerics := extractNumerics(req.Aggregations, e.Data)
		acc.observe(k, groupKey, numerics)
	}
	if err := iter.Err(); err != nil {
		return nil, err
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
			res := gjson.GetBytes(e.Data, gjsonPath(g.Path))
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
		res := gjson.GetBytes(data, gjsonPath(a.Field))
		if !res.Exists() || res.Type != gjson.Number {
			out[i] = math.NaN()
			continue
		}
		out[i] = res.Float()
	}
	return out
}

// gjsonPath converts our normalized JSONPath ("$.foo.bar" or "foo.bar")
// to gjson syntax ("foo.bar"). The reserved token "state" is handled by
// callers via IsState and never reaches here.
func gjsonPath(p string) string {
	if len(p) >= 2 && p[0] == '$' && p[1] == '.' {
		return p[2:]
	}
	return p
}

// translateGroupBy maps the validation-layer types to the SPI types used
// by the pushdown plugin.
func translateGroupBy(groups []GroupExprValidated) []spi.GroupExpr {
	out := make([]spi.GroupExpr, len(groups))
	for i, g := range groups {
		if g.IsState {
			out[i] = spi.GroupExpr{Kind: spi.GroupExprState}
		} else {
			out[i] = spi.GroupExpr{Kind: spi.GroupExprDataPath, Path: g.Path}
		}
	}
	return out
}

func translateAggregations(aggs []AggregationExprValidated) []spi.AggregateExpr {
	out := make([]spi.AggregateExpr, len(aggs))
	for i, a := range aggs {
		out[i] = spi.AggregateExpr{
			Op:    spi.AggregateOp(a.Op),
			Field: a.Field,
			Alias: a.Alias,
		}
	}
	return out
}

// postProcessPushdown converts the plugin's []GroupedAggregateBucket into
// the service's []GroupedStatsBucket, applies the D12 total order, and
// truncates to req.Limit. The plugin is responsible for the aggregations
// values themselves; we only re-shape the keys, normalize missing
// alias entries to JSON null, sort, and limit.
func postProcessPushdown(buckets []spi.GroupedAggregateBucket, req *ValidatedGroupedStatsRequest) []GroupedStatsBucket {
	out := make([]GroupedStatsBucket, 0, len(buckets))
	for _, b := range buckets {
		keys := make([]GroupKeyEntryWire, len(b.GroupKey))
		for i, k := range b.GroupKey {
			keys[i] = GroupKeyEntryWire{Path: k.Path, Value: k.Value}
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
