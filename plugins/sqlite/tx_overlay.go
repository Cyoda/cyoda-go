package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// txOverlayProjection selects which columns the committed cursor reads.
type txOverlayProjection int

const (
	// projectFull reads the whole row: id, model, version, data, meta, submit_time.
	projectFull txOverlayProjection = iota
	// projectIDState reads entity_id and the meta state only — no payload
	// bytes cross the driver. Used by the in-tx counts.
	//
	// Because it does not read data, it cannot evaluate a residual
	// post-filter: openTxOverlay refuses the combination rather than
	// silently counting rows the residual would have excluded.
	projectIDState
)

// txOverlay is the merged (committed snapshot ∪ transaction buffer − staged
// deletes) pull-stream for one model inside one transaction. It is the ONE
// in-transaction read path: Iterate, GetPage, Count and CountByState all
// consume it, so they cannot disagree on what the transaction sees.
//
// The committed cursor runs on readDB, never on the single writer connection:
// a second statement on the writer while a cursor is open would deadlock,
// and the SPI forbids holding a write-blocking lock for an iterator's
// lifetime. Reading committed rows at tx.SnapshotTime on readDB is correct
// because Begin waits for any in-flight commit's flush before flooring the
// snapshot (txmanager.go, Begin) — so every commit with submit_time <=
// SnapshotTime is visible on every connection.
//
// The buffer and the delete set are copied into locals at open, under the
// tx.OpMu.RLock the caller holds: the overlay is a snapshot at call time
// (spi.Iterable contract), so later mutation of the transaction does not
// change what an already-open stream yields.
type txOverlay struct {
	pull func() (*spi.Entity, bool, error)
	rows *sql.Rows
}

// openTxOverlay opens the stream. Caller holds tx.OpMu.RLock and has checked
// tx.RolledBack. Entities are yielded in byte-wise entity-ID order.
func (s *entityStore) openTxOverlay(ctx context.Context, tx *spi.TransactionState, modelRef spi.ModelRef, filter spi.Filter, proj txOverlayProjection) (*txOverlay, error) {
	plan, err := planFor(filter)
	if err != nil {
		return nil, err
	}
	// The id/state projection reads no payload, so evaluateFilter cannot run
	// over its rows. Declining is defence-in-depth: today's only caller passes
	// the zero filter (which never leaves a residual), and a future one that
	// forgets that must get an error, not an over-count.
	if proj == projectIDState && plan.preparedPostFilter != nil {
		return nil, fmt.Errorf("tx overlay: id/state projection cannot evaluate a residual filter")
	}
	// planFor's success already ran spi.Prepare on this filter; this call
	// only obtains the prepared value for the buffer side.
	pf, _ := spi.Prepare(filter)

	opts := spi.SearchOptions{ModelName: modelRef.EntityName, ModelVersion: modelRef.ModelVersion}
	var query string
	var args []any
	switch proj {
	case projectIDState:
		query, args = s.snapshotIDStateBase(opts, timeToMicro(tx.SnapshotTime))
	default:
		query, args = s.searchSnapshotBase(opts, timeToMicro(tx.SnapshotTime))
	}
	if plan.where != "" {
		query += " AND (" + plan.where + ")"
		args = append(args, plan.args...)
	}
	query += " ORDER BY ev.entity_id"

	// Snapshot the transaction's view: buffered adds for this model that
	// match, and the set of ids whose committed row must be suppressed —
	// staged deletes plus every buffered id (the buffered version, if it
	// matches, arrives through adds; if it does not match, the committed
	// row must not stand in for it).
	adds := make([]*spi.Entity, 0, len(tx.Buffer))
	suppressed := make(map[string]struct{}, len(tx.Buffer)+len(tx.Deletes))
	for id := range tx.Deletes {
		suppressed[id] = struct{}{}
	}
	for id, e := range tx.Buffer {
		if e.Meta.ModelRef != modelRef {
			continue
		}
		suppressed[id] = struct{}{}
		if _, del := tx.Deletes[id]; del {
			continue
		}
		if pf.Match(e.Data, e.Meta) {
			if proj == projectIDState {
				adds = append(adds, &spi.Entity{Meta: spi.EntityMeta{ID: e.Meta.ID, State: e.Meta.State, ModelRef: e.Meta.ModelRef}})
			} else {
				adds = append(adds, copyEntity(e))
			}
		}
	}
	if err := sortEntitiesByOrder(ctx, adds, nil); err != nil {
		return nil, err
	}

	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("tx overlay query: %w", err)
	}

	scanned := 0
	next := func() (*spi.Entity, bool, error) {
		for rows.Next() {
			if scanned&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, false, err
				}
			}
			scanned++
			var e *spi.Entity
			var scanErr error
			if proj == projectIDState {
				e, scanErr = scanIDState(rows, modelRef)
			} else {
				e, scanErr = scanVersionEntity(rows)
			}
			if scanErr != nil {
				return nil, false, scanErr
			}
			if plan.preparedPostFilter != nil && !evaluateFilter(*plan.preparedPostFilter, e) {
				continue
			}
			return e, true, nil
		}
		if err := rows.Err(); err != nil {
			if cErr := ctx.Err(); cErr != nil {
				return nil, false, cErr
			}
			return nil, false, fmt.Errorf("row iteration: %w", err)
		}
		return nil, false, nil
	}
	isSuppressed := func(id string) bool { _, ok := suppressed[id]; return ok }
	cmp := func(a, b *spi.Entity) int { return strings.Compare(a.Meta.ID, b.Meta.ID) }

	return &txOverlay{
		pull: spi.MergeOrdered(next, adds, isSuppressed, cmp),
		rows: rows,
	}, nil
}

// Close releases the committed cursor. Idempotent.
func (o *txOverlay) Close() error {
	if o.rows == nil {
		return nil
	}
	rows := o.rows
	o.rows = nil
	return rows.Close()
}

// snapshotIDStateBase is searchSnapshotBase's projection twin: the same
// latest-version-at-snapshot join, selecting only the entity id and the
// meta state. The alias "ev" is kept so plan.where fragments apply unchanged.
func (s *entityStore) snapshotIDStateBase(opts spi.SearchOptions, snapshotMicro int64) (string, []any) {
	query := `SELECT ev.entity_id, json_extract(json(ev.meta), '$.state')
	          FROM entity_versions ev
	          INNER JOIN (
	              SELECT entity_id, MAX(version) AS max_ver
	              FROM entity_versions
	              WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND submit_time <= ?
	              GROUP BY entity_id
	          ) latest ON ev.entity_id = latest.entity_id AND ev.version = latest.max_ver
	          WHERE ev.tenant_id = ? AND ev.change_type != 'DELETED'`
	args := []any{string(s.tenantID), opts.ModelName, opts.ModelVersion, snapshotMicro, string(s.tenantID)}
	return query, args
}

// scanIDState scans the projectIDState row shape: entity_id and the meta
// state, with the model taken from the query the caller built.
func scanIDState(row interface{ Scan(...any) error }, modelRef spi.ModelRef) (*spi.Entity, error) {
	var id string
	var state sql.NullString
	if err := row.Scan(&id, &state); err != nil {
		return nil, fmt.Errorf("scan id/state: %w", err)
	}
	return &spi.Entity{Meta: spi.EntityMeta{ID: id, State: state.String, ModelRef: modelRef}}, nil
}

// iterateTx opens the overlay under tx.OpMu.RLock (held only for the open —
// the stream is pulled lock-free) and returns the iterator over it.
func (s *entityStore) iterateTx(ctx context.Context, tx *spi.TransactionState, model spi.ModelRef, filter spi.Filter, trackingRead bool) (spi.Iterator, error) {
	var overlay *txOverlay
	var bufferedIDs map[string]struct{}
	err := func() error {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return fmt.Errorf("Iterate: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		if tx.Closed {
			return fmt.Errorf("Iterate: %w (txID=%s)", spi.ErrTxAlreadyCommitted, tx.ID)
		}
		bufferedIDs = make(map[string]struct{}, len(tx.Buffer))
		for id := range tx.Buffer {
			bufferedIDs[id] = struct{}{}
		}
		var oErr error
		overlay, oErr = s.openTxOverlay(ctx, tx, model, filter, projectFull)
		if oErr != nil {
			return fmt.Errorf("Iterate: %w", oErr)
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}
	return &sqliteTxIter{ctx: ctx, tx: tx, overlay: overlay, trackingRead: trackingRead, bufferedIDs: bufferedIDs}, nil
}

// sqliteTxIter adapts a txOverlay to spi.Iterator. With trackingRead, each
// yielded COMMITTED entity is recorded into the read-set under a short
// tx.OpMu.RLock that also checks the transaction is still open: Commit takes
// OpMu.Lock between two yields, and an iterator must not record into a
// transaction that has since closed.
type sqliteTxIter struct {
	ctx          context.Context
	tx           *spi.TransactionState
	overlay      *txOverlay
	trackingRead bool
	bufferedIDs  map[string]struct{} // own-writes are never read-set entries
	cur          *spi.Entity
	err          error
	closed       bool
}

func (it *sqliteTxIter) Next() bool {
	if it.err != nil || it.closed {
		return false
	}
	if err := it.ctx.Err(); err != nil {
		it.err = err
		return false
	}
	e, ok, err := it.overlay.pull()
	if err != nil {
		it.err = err
		return false
	}
	if !ok {
		return false
	}
	if it.trackingRead {
		if _, buffered := it.bufferedIDs[e.Meta.ID]; !buffered {
			if err := it.record(e.Meta.ID); err != nil {
				it.err = err
				return false
			}
		}
	}
	it.cur = e
	return true
}

func (it *sqliteTxIter) record(id string) error {
	it.tx.OpMu.RLock()
	defer it.tx.OpMu.RUnlock()
	if it.tx.RolledBack {
		return fmt.Errorf("Iterate: %w (txID=%s)", spi.ErrTxRolledBack, it.tx.ID)
	}
	if it.tx.Closed {
		return fmt.Errorf("Iterate: %w (txID=%s)", spi.ErrTxAlreadyCommitted, it.tx.ID)
	}
	it.tx.ReadSet[id] = true
	return nil
}

func (it *sqliteTxIter) Entity() *spi.Entity { return it.cur }
func (it *sqliteTxIter) Err() error          { return it.err }

func (it *sqliteTxIter) Close() error {
	if it.closed {
		return nil
	}
	it.closed = true
	it.cur = nil
	return it.overlay.Close()
}
