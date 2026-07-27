package memory

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/tidwall/gjson"
)

// This file implements spi.Iterable and spi.GroupedAggregator on
// *EntityStore for the grouped entity statistics query endpoint
// (POST /api/entity/stats/{name}/{version}/query). The design follows
// spec §6.1 (decisions D11, D14, D18, D20):
//
//   - D20: the snapshot is []*spi.Entity (the .entity field of each matching
//     entityVersion), NOT []*entityVersion. The pointers are heap-stable —
//     saveUnlocked assigns a fresh *spi.Entity per save and never mutates it
//     afterwards (see entityVersion's invariant godoc). This lets iterators
//     walk the snapshot lock-free after the read-lock is released.
//
//   - D11: in-tx callers (spi.GetTransaction(ctx) != nil) overlay tx.Buffer
//     and exclude tx.Deletes, matching the GetAll in-tx branch in
//     entity_store.go.
//
//   - D14: the read lock is held only for the snapshot build (one append per
//     matching entity). Filter evaluation and bucket tally happen lock-free.
//
//   - D18: encodeGroupKey produces the spec-mandated collision-free encoding
//     (sentinel byte 0x00=null / 0x01=string + 8-byte big-endian length +
//     raw bytes). This MUST stay bit-for-bit identical with
//     internal/domain/entity/grouped_stats_accumulator.go::buildGroupKey —
//     the two encoders feed independent code paths but represent the same
//     contract.
//
//   - D4: scalar group-key extraction uses res.Raw for numbers and booleans
//     so the canonical JSON text (e.g. "1", "true") is the bucket key,
//     matching the service-layer's buildGroupKeyFromEntity. Object/array/
//     missing values coerce to nil.
//
//   - Filter evaluation (msMatchFilter, below) delegates to the shared
//     spi.MatchFilter kernel — the same evaluator plugins/sqlite/
//     post_filter.go and plugins/postgres/grouped_stats.go delegate to —
//     so all backends agree bit-for-bit on filter semantics. There is no
//     plugin-local leaf evaluator here; see msMatchFilter's doc comment.

// Iterate implements spi.Iterable. Snapshots matching *spi.Entity pointers
// under the entity read-lock, releases the lock, and yields them through
// the iterator with the filter applied inside Next() (D14, D20, §6.1).
func (s *EntityStore) Iterate(
	ctx context.Context,
	model spi.ModelRef,
	filter spi.Filter,
	opts spi.IterateOptions,
) (spi.Iterator, error) {
	snapshot, err := s.buildSnapshot(ctx, model, opts.PointInTime)
	if err != nil {
		return nil, err
	}
	return &memoryIter{
		snapshot: snapshot,
		filter:   filter,
		ctx:      ctx,
	}, nil
}

// buildSnapshot captures all entities matching modelRef visible to this
// caller, returning a slice of *spi.Entity pointers. For in-tx callers
// the snapshot reflects the tx-merged view (committed-at-snapshot-time
// minus tx.Deletes plus tx.Buffer), matching GetAll's in-tx branch.
// Non-tx callers see the latest committed version per entity.
//
// PIT (opts.PointInTime, when non-nil) reads the historical snapshot at
// the requested instant, ignoring any in-flight tx — consistent with the
// rest of the SPI's historical-read semantics.
func (s *EntityStore) buildSnapshot(ctx context.Context, model spi.ModelRef, pit *time.Time) ([]*spi.Entity, error) {
	// PIT path: historical read, bypass tx overlay.
	if pit != nil {
		var snapshot []*spi.Entity
		func() {
			s.factory.entityMu.RLock()
			defer s.factory.entityMu.RUnlock()
			snapshot = s.getAllSnapshotPointersUnlocked(model, *pit)
		}()
		return snapshot, nil
	}

	tx := spi.GetTransaction(ctx)
	if tx != nil {
		// Mirror GetAll's in-tx branch: hold tx.OpMu.RLock across the
		// snapshot read AND the buffer/deletes overlay so Commit/Rollback
		// (tx.OpMu.Lock) can't race with us. Lock order: tx.OpMu before
		// factory.entityMu.
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return nil, fmt.Errorf("Iterate: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}

		var mainEntities []*spi.Entity
		func() {
			s.factory.entityMu.RLock()
			defer s.factory.entityMu.RUnlock()
			mainEntities = s.getAllSnapshotPointersUnlocked(model, tx.SnapshotTime)
		}()

		merged := make(map[string]*spi.Entity, len(mainEntities))
		for _, e := range mainEntities {
			if !tx.Deletes[e.Meta.ID] {
				merged[e.Meta.ID] = e
				tx.ReadSet[e.Meta.ID] = true
			}
		}
		// Overlay tx.Buffer. The buffered *spi.Entity is owned by the tx
		// and not yet committed; for snapshot semantics we copy it so the
		// iterator can read it lock-free without aliasing live tx state.
		for id, e := range tx.Buffer {
			if e.Meta.ModelRef == model {
				merged[id] = copyEntity(e)
				tx.ReadSet[id] = true
			}
		}

		snapshot := make([]*spi.Entity, 0, len(merged))
		for _, e := range merged {
			snapshot = append(snapshot, e)
		}
		return snapshot, nil
	}

	// Non-tx: latest committed version per entity matching the model.
	var snapshot []*spi.Entity
	func() {
		s.factory.entityMu.RLock()
		defer s.factory.entityMu.RUnlock()
		snapshot = make([]*spi.Entity, 0)
		for _, versions := range s.factory.entityData[s.tenant] {
			if len(versions) == 0 {
				continue
			}
			latest := versions[len(versions)-1]
			if latest.deleted {
				continue
			}
			if latest.entity.Meta.ModelRef != model {
				continue
			}
			// D20: capture the *spi.Entity pointer directly. The entityVersion
			// invariant (immutable post-publish) guarantees lock-free read
			// safety after we release the lock.
			snapshot = append(snapshot, latest.entity)
		}
	}()
	return snapshot, nil
}

// getAllSnapshotPointersUnlocked is the *spi.Entity-pointer-returning
// counterpart of getAllSnapshotUnlocked (which copies). For Iterate /
// GroupedAggregate we deliberately do NOT copy — the *spi.Entity
// pointer is heap-stable and the entityVersion immutability invariant
// makes lock-free read safe.
//
// Caller must hold at least s.factory.entityMu.RLock().
func (s *EntityStore) getAllSnapshotPointersUnlocked(modelRef spi.ModelRef, snapshotTime time.Time) []*spi.Entity {
	var result []*spi.Entity
	for _, versions := range s.factory.entityData[s.tenant] {
		if len(versions) == 0 {
			continue
		}
		var found *spi.Entity
		for _, v := range versions {
			if !v.submitTime.After(snapshotTime) {
				if v.deleted {
					found = nil
				} else {
					found = v.entity
				}
			} else {
				break
			}
		}
		if found != nil && found.Meta.ModelRef == modelRef {
			result = append(result, found)
		}
	}
	return result
}

// memoryIter walks a pre-built snapshot, applying the filter inside Next()
// before yielding each entity. Per the SPI contract: Err() is sticky,
// Close() is idempotent, ctx cancellation is observed.
type memoryIter struct {
	snapshot []*spi.Entity
	filter   spi.Filter
	ctx      context.Context
	idx      int
	cur      *spi.Entity
	err      error
	closed   bool
}

func (it *memoryIter) Next() bool {
	if it.err != nil || it.closed {
		return false
	}
	if err := it.ctx.Err(); err != nil {
		it.err = err
		return false
	}
	for it.idx < len(it.snapshot) {
		e := it.snapshot[it.idx]
		it.idx++
		if !msMatchFilter(it.filter, e) {
			continue
		}
		it.cur = e
		return true
	}
	return false
}

func (it *memoryIter) Entity() *spi.Entity { return it.cur }
func (it *memoryIter) Err() error          { return it.err }

func (it *memoryIter) Close() error {
	if it.closed {
		return nil
	}
	it.closed = true
	it.snapshot = nil
	it.cur = nil
	return nil
}

// GroupedAggregate implements spi.GroupedAggregator. Walks the same
// snapshot used by Iterate, applies the filter, and tallies per-bucket
// counts and aggregations inline. Returns ErrGroupCardinalityExceeded
// the moment a new key would push the bucket map past opts.MaxBuckets.
//
// Sorting/limiting is handled by the service-layer postProcessPushdown
// (and a nil-vs-zero aggregations map there); the plugin returns raw
// per-bucket results.
func (s *EntityStore) GroupedAggregate(
	ctx context.Context,
	model spi.ModelRef,
	groupBy []spi.GroupExpr,
	filter spi.Filter,
	opts spi.GroupedAggregationsOptions,
) ([]spi.GroupedAggregateBucket, error) {
	snapshot, err := s.buildSnapshot(ctx, model, opts.PointInTime)
	if err != nil {
		return nil, err
	}

	buckets := make(map[string]*memBucket)
	for _, e := range snapshot {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !msMatchFilter(filter, e) {
			continue
		}
		rawVals, keys := extractGroupKey(groupBy, e)
		k := encodeGroupKey(rawVals)
		b, ok := buckets[k]
		if !ok {
			if opts.MaxBuckets > 0 && len(buckets) >= opts.MaxBuckets {
				return nil, spi.ErrGroupCardinalityExceeded
			}
			b = &memBucket{groupKey: keys, aggs: make([]*memAcc, len(opts.Aggregations))}
			for i, a := range opts.Aggregations {
				b.aggs[i] = &memAcc{op: a.Op, alias: a.Alias, field: a.Field}
			}
			buckets[k] = b
		}
		b.observe(e.Data)
	}

	out := make([]spi.GroupedAggregateBucket, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, b.toBucket(opts.Aggregations))
	}
	return out, nil
}

// memBucket is the per-group accumulator state. Plugin-local; the
// service layer translates spi.GroupedAggregateBucket into its own DTOs.
type memBucket struct {
	groupKey []spi.GroupKeyEntry
	count    int64
	aggs     []*memAcc
}

// memAcc holds running per-aggregation stats: count, sum, min, max, and
// the Welford n/mean/m2 recurrence for stable sample stdev (spec D9).
// Bit-for-bit parity with internal/domain/entity/grouped_stats_accumulator.go
// is required.
type memAcc struct {
	op    spi.AggregateOp
	alias string
	field string

	n    int64
	sum  float64
	minV float64
	maxV float64
	mean float64
	m2   float64
	init bool
}

func (b *memBucket) observe(data []byte) {
	b.count++
	for _, a := range b.aggs {
		res := gjson.GetBytes(data, gjsonPath(a.field))
		if !res.Exists() || res.Type != gjson.Number {
			continue
		}
		x := res.Float()
		if math.IsNaN(x) || math.IsInf(x, 0) {
			continue
		}
		a.observeFloat(x)
	}
}

func (a *memAcc) observeFloat(x float64) {
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
	// Welford recurrence — spec §4, D9.
	delta := x - a.mean
	a.mean += delta / float64(a.n)
	delta2 := x - a.mean
	a.m2 += delta * delta2
}

func (b *memBucket) toBucket(aggs []spi.AggregateExpr) spi.GroupedAggregateBucket {
	var aggMap map[string]any
	if len(aggs) > 0 {
		aggMap = make(map[string]any, len(aggs))
		for i, a := range aggs {
			aggMap[a.Alias] = b.aggs[i].result()
		}
	}
	return spi.GroupedAggregateBucket{
		GroupKey:     b.groupKey,
		Count:        b.count,
		Aggregations: aggMap,
	}
}

// result returns the aggregation value, or nil for "no numeric samples"
// (or n<2 for stdev). nil here propagates to a JSON null in the response,
// matching the service-layer accumulator.
func (a *memAcc) result() any {
	switch a.op {
	case spi.AggSum:
		if a.n == 0 {
			return nil
		}
		return a.sum
	case spi.AggAvg:
		if a.n == 0 {
			return nil
		}
		return a.sum / float64(a.n)
	case spi.AggMin:
		if a.n == 0 {
			return nil
		}
		return a.minV
	case spi.AggMax:
		if a.n == 0 {
			return nil
		}
		return a.maxV
	case spi.AggStdev:
		if a.n < 2 {
			return nil
		}
		return math.Sqrt(a.m2 / float64(a.n-1))
	}
	return nil
}

// extractGroupKey returns the raw key values (for map-key encoding) and
// the response group-key entries. D4 coercion: scalar values become
// strings (using res.Raw for numbers/booleans so the canonical JSON text
// is preserved); objects/arrays/missing/null coerce to nil.
//
// Parity with internal/domain/entity/grouped_stats_service.go::
// buildGroupKeyFromEntity is required — group-key identity must match
// across pushdown and streaming paths.
func extractGroupKey(groups []spi.GroupExpr, e *spi.Entity) ([]any, []spi.GroupKeyEntry) {
	rawVals := make([]any, len(groups))
	keys := make([]spi.GroupKeyEntry, len(groups))
	for i, g := range groups {
		var path string
		var val any
		if g.Kind == spi.GroupExprState {
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
				// Canonical JSON-text representation, matching the postgres
				// `doc->>'field'` cast and the service-layer extractor.
				val = res.Raw
			case res.Type == gjson.True || res.Type == gjson.False:
				val = res.Raw
			default:
				// gjson.Null, gjson.JSON (object/array): null per D4.
				val = nil
			}
		}
		rawVals[i] = val
		keys[i] = spi.GroupKeyEntry{Path: path, Value: val}
	}
	return rawVals, keys
}

// encodeGroupKey produces the collision-free encoding of a group-key
// tuple as a Go string. MUST stay bit-for-bit identical to
// internal/domain/entity/grouped_stats_accumulator.go::buildGroupKey
// (spec D18): sentinel byte 0x00=null / 0x01=string + 8-byte big-endian
// length prefix + raw bytes.
func encodeGroupKey(values []any) string {
	var size int
	for _, v := range values {
		if v == nil {
			size++
			continue
		}
		s, ok := v.(string)
		if !ok {
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

// gjsonPath converts our normalized JSONPath ("$.foo.bar" or "foo.bar")
// to gjson syntax ("foo.bar"). Parity with the service-layer helper of
// the same name.
func gjsonPath(p string) string {
	if len(p) >= 2 && p[0] == '$' && p[1] == '.' {
		return p[2:]
	}
	return p
}

// msMatchFilter evaluates an spi.Filter against an entity by delegating to
// the shared spi.MatchFilter kernel — the same evaluator the sqlite
// (plugins/sqlite/post_filter.go) and postgres (plugins/postgres/
// grouped_stats.go) backends use, so all three backends agree bit-for-bit
// on filter semantics (including CoerceTemporal and the canonical
// client-name meta vocabulary, e.g. "creationDate"/"lastUpdateTime").
// spi.MatchFilter already returns true for a zero-value (empty Op) filter,
// matching the historical "no filter" contract.
func msMatchFilter(f spi.Filter, e *spi.Entity) bool {
	return spi.MatchFilter(f, e.Data, e.Meta)
}
