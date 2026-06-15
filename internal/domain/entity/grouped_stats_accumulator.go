package entity

import (
	"encoding/binary"
	"math"
	"sort"
)

// buildGroupKey encodes a group-key tuple as a unique Go string suitable
// for use as a map key. Encoding per spec D18:
//
//	per entry: sentinel byte (0x00 = null, 0x01 = string)
//	           + 8-byte big-endian length prefix (only when sentinel = 0x01)
//	           + raw value bytes
//
// Concatenation across entries. Collision-free under arbitrary inputs.
//
// Callers pass either nil (null bucket — per spec D4 this covers
// object/array runtime values, JSON null, and missing fields) or a Go
// string (scalar value: original string, canonical number text, or
// "true"/"false"). Any other Go type is defensively treated as null.
func buildGroupKey(values []any) string {
	var size int
	for _, v := range values {
		if v == nil {
			size++
			continue
		}
		s, ok := v.(string)
		if !ok {
			// Non-scalar coerces to null per D4.
			size++
			continue
		}
		size += 1 + 8 + len(s)
	}
	buf := make([]byte, 0, size)
	var lenBuf [8]byte
	for _, v := range values {
		if v == nil {
			buf = append(buf, 0x00)
			continue
		}
		s, ok := v.(string)
		if !ok {
			buf = append(buf, 0x00)
			continue
		}
		buf = append(buf, 0x01)
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
		buf = append(buf, lenBuf[:]...)
		buf = append(buf, s...)
	}
	return string(buf)
}

// accumulator holds per-bucket per-aggregation state. It tracks count,
// running sum, running min/max, and the Welford n/mean/m2 recurrence for
// stable sample stdev (spec D9).
type accumulator struct {
	expr AggregationExprValidated
	n    int64
	sum  float64
	minV float64
	maxV float64
	mean float64
	m2   float64
	init bool // true once first sample observed (for min/max bootstrap)
}

func newAccumulator(expr AggregationExprValidated) *accumulator {
	return &accumulator{expr: expr}
}

// observeFloat updates the accumulator with a numeric sample.
// Non-numeric values are skipped at the caller.
func (a *accumulator) observeFloat(x float64) {
	a.n++
	a.sum += x
	if !a.init {
		a.minV, a.maxV = x, x
		a.init = true
	} else {
		if x < a.minV {
			a.minV = x
		}
		if x > a.maxV {
			a.maxV = x
		}
	}
	// Welford recurrence (spec §4, D9).
	delta := x - a.mean
	a.mean += delta / float64(a.n)
	delta2 := x - a.mean
	a.m2 += delta * delta2
}

// result returns the aggregation's resolved value. ok=false means the
// bucket had no numeric samples (or n<2 for stdev), in which case the
// response value should be JSON null.
func (a *accumulator) result() (float64, bool) {
	switch a.expr.Op {
	case AggSum:
		if a.n == 0 {
			return 0, false
		}
		return a.sum, true
	case AggAvg:
		if a.n == 0 {
			return 0, false
		}
		return a.sum / float64(a.n), true
	case AggMin:
		if a.n == 0 {
			return 0, false
		}
		return a.minV, true
	case AggMax:
		if a.n == 0 {
			return 0, false
		}
		return a.maxV, true
	case AggStdev:
		if a.n < 2 {
			return 0, false
		}
		return math.Sqrt(a.m2 / float64(a.n-1)), true
	}
	return 0, false
}

// bucketState is per-group-key state: the raw group-key tuple, the
// bucket count, and per-aggregation accumulators.
type bucketState struct {
	groupKey []GroupKeyEntryWire
	count    int64
	aggs     []*accumulator
}

// observe records one entity's contribution to this bucket.
// numerics[i] is the value for aggregation i; NaN/Inf are skipped per
// spec D4 (non-finite runtime values are treated as missing).
func (b *bucketState) observe(numerics []float64) {
	b.count++
	for i, x := range numerics {
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			b.aggs[i].observeFloat(x)
		}
	}
}

// accumulators holds all buckets keyed by the encoded group key.
type accumulators struct {
	req     *ValidatedGroupedStatsRequest
	buckets map[string]*bucketState
}

func newAccumulators(req *ValidatedGroupedStatsRequest) *accumulators {
	return &accumulators{
		req:     req,
		buckets: make(map[string]*bucketState),
	}
}

// has reports whether a bucket already exists for the encoded key. Used
// by the service layer's strict-> cardinality check before admitting a
// new bucket.
func (a *accumulators) has(k string) bool {
	_, ok := a.buckets[k]
	return ok
}

// len returns the current bucket count.
func (a *accumulators) len() int { return len(a.buckets) }

// observe records one entity in the bucket for k, creating the bucket
// on first sight.
func (a *accumulators) observe(k string, groupKey []GroupKeyEntryWire, numerics []float64) {
	b, ok := a.buckets[k]
	if !ok {
		b = &bucketState{
			groupKey: groupKey,
			aggs:     make([]*accumulator, len(a.req.Aggregations)),
		}
		for i, expr := range a.req.Aggregations {
			b.aggs[i] = newAccumulator(expr)
		}
		a.buckets[k] = b
	}
	b.observe(numerics)
}

// materialize converts the bucket map to a sorted []GroupedStatsBucket
// applying the D12 total order (count desc, then group-key lex) and the
// request's Limit.
func (a *accumulators) materialize() []GroupedStatsBucket {
	out := make([]GroupedStatsBucket, 0, len(a.buckets))
	for _, b := range a.buckets {
		bucket := GroupedStatsBucket{
			GroupKey: b.groupKey,
			Count:    b.count,
		}
		if len(a.req.Aggregations) > 0 {
			bucket.Aggregations = make(map[string]any, len(a.req.Aggregations))
			for i, expr := range a.req.Aggregations {
				v, ok := b.aggs[i].result()
				if ok {
					bucket.Aggregations[expr.Alias] = v
				} else {
					bucket.Aggregations[expr.Alias] = nil
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
	if a.req.Limit != nil && *a.req.Limit < len(out) {
		out = out[:*a.req.Limit]
	}
	return out
}

// compareGroupKey applies the D12 total order: pairwise by entry index;
// null < any string; strings compared byte-wise.
func compareGroupKey(a, b []GroupKeyEntryWire) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		ai, bi := a[i].Value, b[i].Value
		as, _ := ai.(string)
		bs, _ := bi.(string)
		switch {
		case ai == nil && bi == nil:
			continue
		case ai == nil:
			return -1
		case bi == nil:
			return 1
		default:
			if as < bs {
				return -1
			}
			if as > bs {
				return 1
			}
		}
	}
	return len(a) - len(b)
}
