package memory

import (
	"context"
	"fmt"
	"sort"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Compile-time check that *EntityStore implements spi.EntityStore.
var _ spi.EntityStore = (*EntityStore)(nil)

// Search implements spi.EntityStore.Search for the in-memory entity store. It produces
// the same result set that GetPage + spi.Prepare(filter).Match would for the
// same transaction state, but filters/orders/bounds with the canonical SPI
// helpers (spi.Prepare/PreparedFilter.Match, spi.LessByOrder,
// spi.MergeBounded) so every backend agrees. Search is bounded-or-fail:
// opts.Limit >= 1 is REQUIRED and caps the matched set; a matched set larger
// than the limit is spi.ErrSearchResultLimitExceeded, never a truncated
// prefix. opts.Limit <= 0 is a contract violation, rejected up front.
//
// Three branches:
//   - non-tx: iterate the current committed model (or the PIT snapshot when
//     opts.PointInTime is set), filter, sort, bound. No read-set.
//   - in-tx with PointInTime: committed-only snapshot at the PIT — no buffer
//     overlay, no read-set (mirrors GetPage's committed-only PIT branch).
//   - in-tx, PointInTime==nil: read-your-own-writes overlay — a k-way merge of
//     the committed snapshot (suppressing tx.Deletes and buffered ids) with the
//     matching buffer entries, bounded by spi.MergeBounded. Returned committed
//     ids enter tx.ReadSet ONLY when opts.TrackingRead is set (unlike GetPage,
//     which records every read unconditionally).
func (s *EntityStore) Search(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	if opts.Limit <= 0 {
		return nil, fmt.Errorf("search: limit must be >= 1")
	}
	// Same path checks the sqlite and postgres backends run at their Search
	// boundary, in the same order, so a malformed filter path, an unknown
	// meta sort path or a malformed data sort path is classified identically
	// on every backend rather than degrading to an empty page here.
	if err := validateFilterPaths(filter); err != nil {
		return nil, err
	}
	if err := validateOrderSpecs(opts.OrderBy); err != nil {
		return nil, err
	}

	modelRef := spi.ModelRef{EntityName: opts.ModelName, ModelVersion: opts.ModelVersion}
	tx := spi.GetTransaction(ctx)

	// Prepare once per query. Every branch below evaluates the same filter, so
	// a single prepared value serves the non-tx scan, the PIT scan, and both
	// loops of the read-your-own-writes overlay. A leaf spi.Prepare genuinely
	// cannot evaluate (a malformed pattern, an unrecognized meta path, ...)
	// fails the search outright rather than degrading to an empty page — see
	// .claude/rules/correctness-over-availability.md.
	pf, err := spi.Prepare(filter)
	if err != nil {
		return nil, fmt.Errorf("Search: %w", err)
	}

	if tx == nil {
		// Non-transaction: snapshot the committed model under entityMu, then
		// filter/sort/page. IIFE so the unlock runs via defer even though the
		// filter/sort work happens after we release the lock.
		var committed []*spi.Entity
		var snapErr error
		func() {
			s.factory.entityMu.RLock()
			defer s.factory.entityMu.RUnlock()
			if opts.PointInTime != nil {
				committed, snapErr = s.getAllSnapshotPointersUnlocked(ctx, modelRef, *opts.PointInTime)
			} else {
				committed, snapErr = s.currentStatePointersUnlocked(ctx, modelRef)
			}
		}()
		if snapErr != nil {
			return nil, fmt.Errorf("Search: %w", snapErr)
		}
		return matchSortBounded(ctx, pf, committed, opts.OrderBy, opts.Limit)
	}

	// In-transaction: hold tx.OpMu.RLock for the whole operation so Commit/
	// Rollback (which take tx.OpMu.Lock) cannot race with our reads of
	// tx.Buffer / tx.Deletes and our write to tx.ReadSet. Lock order:
	// tx.OpMu before factory.entityMu (matches Save/GetPage and txmanager.Commit).
	tx.OpMu.RLock()
	defer tx.OpMu.RUnlock()
	if tx.RolledBack {
		return nil, fmt.Errorf("Search: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
	}
	if tx.Closed {
		return nil, fmt.Errorf("Search: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
	}

	if opts.PointInTime != nil {
		// In-tx point-in-time: committed-only, no buffer overlay, no read-set
		// (mirrors GetPage's committed-only PIT branch). Snapshot under entityMu via IIFE.
		var committed []*spi.Entity
		var snapErr error
		func() {
			s.factory.entityMu.RLock()
			defer s.factory.entityMu.RUnlock()
			committed, snapErr = s.getAllSnapshotPointersUnlocked(ctx, modelRef, *opts.PointInTime)
		}()
		if snapErr != nil {
			return nil, fmt.Errorf("Search: %w", snapErr)
		}
		return matchSortBounded(ctx, pf, committed, opts.OrderBy, opts.Limit)
	}

	// In-tx read-your-own-writes overlay. Snapshot the committed model at the
	// tx snapshot time, filter it, and sort it — this is the lazy `next`
	// source for the merge. The snapshot is pointers; survivors are copied
	// before they are returned, so no raw store pointer escapes the lock.
	var committed []*spi.Entity
	var snapErr error
	func() {
		s.factory.entityMu.RLock()
		defer s.factory.entityMu.RUnlock()
		committed, snapErr = s.getAllSnapshotPointersUnlocked(ctx, modelRef, tx.SnapshotTime)
	}()
	if snapErr != nil {
		return nil, fmt.Errorf("Search: %w", snapErr)
	}
	filteredCommitted := make([]*spi.Entity, 0, len(committed))
	for i, e := range committed {
		// Amortized cancellation check (spec D5): the memory plugin IS
		// spi.EntityStore.Search, so this loop (and its siblings below) is the real
		// search-scan path for this backend, not a fallback. Check every
		// 1024 rows (i&1023==0, true at i==0 too) so a pre-expired or
		// since-expired ctx aborts promptly without paying a per-row cost.
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("Search: %w", err)
			}
		}
		if pf.Match(e.Data, e.Meta) {
			filteredCommitted = append(filteredCommitted, e)
		}
	}
	if err := sortByOrder(ctx, filteredCommitted, opts.OrderBy); err != nil {
		return nil, fmt.Errorf("Search: %w", err)
	}

	// adds = matching buffered writes for this model (own-writes), excluding
	// anything staged for delete. Buffer entries are copied so store-internal
	// pointers never escape.
	adds := make([]*spi.Entity, 0, len(tx.Buffer))
	addI := 0
	for id, e := range tx.Buffer {
		if addI&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("Search: %w", err)
			}
		}
		addI++
		if tx.Deletes[id] {
			continue
		}
		if e.Meta.ModelRef != modelRef {
			continue
		}
		if pf.Match(e.Data, e.Meta) {
			adds = append(adds, copyEntity(e))
		}
	}
	if err := sortByOrder(ctx, adds, opts.OrderBy); err != nil {
		return nil, fmt.Errorf("Search: %w", err)
	}

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
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, false, fmt.Errorf("Search: %w", err)
			}
		}
		e := filteredCommitted[i]
		i++
		return e, true, nil
	}

	page, err := spi.MergeBounded(next, adds, deleted, opts.OrderBy, opts.Limit)
	if err != nil {
		return nil, err
	}

	// page holds pointers for the committed survivors (filteredCommitted was
	// never copied) and copies for the buffered adds (already copied above).
	// Copy the committed survivors now so no raw store pointer escapes.
	for i, e := range page {
		if _, buffered := tx.Buffer[e.Meta.ID]; !buffered {
			page[i] = copyEntity(e)
		}
	}

	// Read-set recording is CONDITIONAL on TrackingRead (GetPage records
	// unconditionally). Only committed rows (not in the buffer — those are
	// own-writes already in the write-set) enter the read-set. Bounded-or-fail
	// means page is exactly the matched set — there is no smaller page to
	// under-record against. Still under tx.OpMu.RLock (held via defer for the
	// whole function).
	if opts.TrackingRead {
		for _, e := range page {
			if _, buffered := tx.Buffer[e.Meta.ID]; !buffered {
				tx.ReadSet[e.Meta.ID] = true
			}
		}
	}
	return page, nil
}

// matchSortBounded filters rows with a prepared filter, orders with
// spi.LessByOrder, and enforces the bounded-or-fail cap: the whole matched
// set must fit within limit (>= 1, validated by the caller), or the result
// is spi.ErrSearchResultLimitExceeded rather than a truncated prefix. Used
// by the non-tx and in-tx PIT branches; the RYW overlay branch gets the same
// bound from spi.MergeBounded.
//
// It takes an already-prepared filter so the caller pays the operand parse,
// type bucketing and regex compilation once per query rather than once per row.
func matchSortBounded(ctx context.Context, pf spi.PreparedFilter, rows []*spi.Entity, order []spi.OrderSpec, limit int) ([]*spi.Entity, error) {
	filtered := make([]*spi.Entity, 0, len(rows))
	for i, e := range rows {
		// Amortized cancellation check (spec D5): checked every 1024 rows
		// (i&1023==0, true at i==0 too) so an already-expired or
		// since-expired ctx aborts the scan instead of returning a full
		// result set computed past the client's deadline.
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("Search: %w", err)
			}
		}
		if pf.Match(e.Data, e.Meta) {
			filtered = append(filtered, copyEntity(e))
			// Short-circuit before sorting: the result is an error either way.
			if len(filtered) > limit {
				return nil, fmt.Errorf("search: more than %d matches: %w", limit, spi.ErrSearchResultLimitExceeded)
			}
		}
	}
	if err := sortByOrder(ctx, filtered, order); err != nil {
		return nil, fmt.Errorf("Search: %w", err)
	}
	return filtered, nil
}

// sortByOrder sorts entities in place by the canonical spi.LessByOrder
// comparator. LessByOrder is a strict total order (entity_id ascending
// tiebreaker), so a plain sort is deterministic across backends. A single
// ctx check gates the O(n log n) sort itself — the filter loop that built
// rows already paid for amortized checks over the scan, but the sort is a
// separate unit of work worth gating on its own before it runs.
func sortByOrder(ctx context.Context, rows []*spi.Entity, order []spi.OrderSpec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sort.Slice(rows, func(i, j int) bool {
		return spi.LessByOrder(rows[i], rows[j], order)
	})
	return nil
}
