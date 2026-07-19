package memory

import (
	"context"
	"fmt"
	"sort"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Compile-time check that *EntityStore implements spi.Searcher.
var _ spi.Searcher = (*EntityStore)(nil)

// Search implements spi.Searcher for the in-memory entity store. It produces
// the same result set that GetAll + spi.MatchFilter would for the same
// transaction state, but filters/orders/pages with the canonical SPI helpers
// (spi.MatchFilter, spi.LessByOrder, spi.MergePage) so every backend agrees.
//
// Three branches:
//   - non-tx: iterate the current committed model (or the PIT snapshot when
//     opts.PointInTime is set), filter, sort, page. No read-set.
//   - in-tx with PointInTime: committed-only snapshot at the PIT — no buffer
//     overlay, no read-set (mirrors GetAllAsAt).
//   - in-tx, PointInTime==nil: read-your-own-writes overlay — a k-way merge of
//     the committed snapshot (suppressing tx.Deletes and buffered ids) with the
//     matching buffer entries. Returned committed ids enter tx.ReadSet ONLY
//     when opts.TrackingRead is set (unlike GetAll, which records every read
//     unconditionally).
func (s *EntityStore) Search(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	modelRef := spi.ModelRef{EntityName: opts.ModelName, ModelVersion: opts.ModelVersion}
	tx := spi.GetTransaction(ctx)

	if tx == nil {
		// Non-transaction: snapshot the committed model under entityMu, then
		// filter/sort/page. IIFE so the unlock runs via defer even though the
		// filter/sort work happens after we release the lock.
		var committed []*spi.Entity
		func() {
			s.factory.entityMu.RLock()
			defer s.factory.entityMu.RUnlock()
			if opts.PointInTime != nil {
				committed = s.getAllSnapshotUnlocked(modelRef, *opts.PointInTime)
			} else {
				committed = s.currentStateMatchesUnlocked(modelRef)
			}
		}()
		return matchSortPage(filter, committed, opts.OrderBy, opts.Offset, opts.Limit), nil
	}

	// In-transaction: hold tx.OpMu.RLock for the whole operation so Commit/
	// Rollback (which take tx.OpMu.Lock) cannot race with our reads of
	// tx.Buffer / tx.Deletes and our write to tx.ReadSet. Lock order:
	// tx.OpMu before factory.entityMu (matches Save/GetAll and txmanager.Commit).
	tx.OpMu.RLock()
	defer tx.OpMu.RUnlock()
	if tx.RolledBack {
		return nil, fmt.Errorf("Search: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
	}

	if opts.PointInTime != nil {
		// In-tx point-in-time: committed-only, no buffer overlay, no read-set
		// (mirrors GetAllAsAt). Snapshot under entityMu via IIFE.
		var committed []*spi.Entity
		func() {
			s.factory.entityMu.RLock()
			defer s.factory.entityMu.RUnlock()
			committed = s.getAllSnapshotUnlocked(modelRef, *opts.PointInTime)
		}()
		return matchSortPage(filter, committed, opts.OrderBy, opts.Offset, opts.Limit), nil
	}

	// In-tx read-your-own-writes overlay. Snapshot the committed model at the
	// tx snapshot time, filter it, and sort it — this is the lazy `next`
	// source for the merge. copyEntity happens inside getAllSnapshotUnlocked,
	// so no raw store pointer escapes the lock.
	var committed []*spi.Entity
	func() {
		s.factory.entityMu.RLock()
		defer s.factory.entityMu.RUnlock()
		committed = s.getAllSnapshotUnlocked(modelRef, tx.SnapshotTime)
	}()
	filteredCommitted := make([]*spi.Entity, 0, len(committed))
	for _, e := range committed {
		if spi.MatchFilter(filter, e.Data, e.Meta) {
			filteredCommitted = append(filteredCommitted, e)
		}
	}
	sortByOrder(filteredCommitted, opts.OrderBy)

	// adds = matching buffered writes for this model (own-writes), excluding
	// anything staged for delete. Buffer entries are copied so store-internal
	// pointers never escape.
	adds := make([]*spi.Entity, 0, len(tx.Buffer))
	for id, e := range tx.Buffer {
		if tx.Deletes[id] {
			continue
		}
		if e.Meta.ModelRef != modelRef {
			continue
		}
		if spi.MatchFilter(filter, e.Data, e.Meta) {
			adds = append(adds, copyEntity(e))
		}
	}
	sortByOrder(adds, opts.OrderBy)

	// A committed row is suppressed if it is staged for delete OR shadowed by a
	// buffered own-write (the buffered version, if it matches, comes in via adds).
	deleted := func(id string) bool {
		if tx.Deletes[id] {
			return true
		}
		_, buffered := tx.Buffer[id]
		return buffered
	}

	i := 0
	next := func() (*spi.Entity, bool, error) {
		if i >= len(filteredCommitted) {
			return nil, false, nil
		}
		e := filteredCommitted[i]
		i++
		return e, true, nil
	}

	page, err := spi.MergePage(next, adds, deleted, opts.OrderBy, opts.Offset, opts.Limit)
	if err != nil {
		return nil, err
	}

	// Read-set recording is CONDITIONAL on TrackingRead (GetAll records
	// unconditionally). Only committed rows (not in the buffer — those are
	// own-writes already in the write-set) enter the read-set. Still under
	// tx.OpMu.RLock (held via defer for the whole function).
	if opts.TrackingRead {
		for _, e := range page {
			if _, buffered := tx.Buffer[e.Meta.ID]; !buffered {
				tx.ReadSet[e.Meta.ID] = true
			}
		}
	}
	return page, nil
}

// currentStateMatchesUnlocked returns copies of the latest non-deleted versions
// matching modelRef. Caller must hold at least s.factory.entityMu.RLock().
// Mirrors the non-tx branch of GetAll.
func (s *EntityStore) currentStateMatchesUnlocked(modelRef spi.ModelRef) []*spi.Entity {
	result := make([]*spi.Entity, 0)
	for _, versions := range s.factory.entityData[s.tenant] {
		if len(versions) == 0 {
			continue
		}
		latest := versions[len(versions)-1]
		if latest.deleted {
			continue
		}
		if latest.entity.Meta.ModelRef == modelRef {
			result = append(result, copyEntity(latest.entity))
		}
	}
	return result
}

// matchSortPage filters rows with spi.MatchFilter, orders with spi.LessByOrder,
// and applies offset/limit with the same semantics as spi.MergePage
// (offset >= len ⇒ empty). Used by the non-tx and in-tx PIT branches.
func matchSortPage(filter spi.Filter, rows []*spi.Entity, order []spi.OrderSpec, offset, limit int) []*spi.Entity {
	filtered := make([]*spi.Entity, 0, len(rows))
	for _, e := range rows {
		if spi.MatchFilter(filter, e.Data, e.Meta) {
			filtered = append(filtered, e)
		}
	}
	sortByOrder(filtered, order)
	if offset > 0 {
		if offset >= len(filtered) {
			return nil
		}
		filtered = filtered[offset:]
	}
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered
}

// sortByOrder sorts entities in place by the canonical spi.LessByOrder
// comparator. LessByOrder is a strict total order (entity_id ascending
// tiebreaker), so a plain sort is deterministic across backends.
func sortByOrder(rows []*spi.Entity, order []spi.OrderSpec) {
	sort.Slice(rows, func(i, j int) bool {
		return spi.LessByOrder(rows[i], rows[j], order)
	})
}
