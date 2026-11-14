package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// foldLockedInTx returns the fully-folded schema for ref, read through tx:
// it starts from the most-recent savepoint payload (if any), else from
// baseSchema (models.doc.schema), and applies every later delta row, in seq
// order, in one call of the injected ApplyFunc. With no extensions it
// returns that start verbatim and needs no ApplyFunc; otherwise a nil
// ApplyFunc is refused. Mirrors postgres's fold; dialect differences are
// placeholders and the explicit seq column.
func (s *modelStore) foldLockedInTx(ctx context.Context, tx *sql.Tx, ref spi.ModelRef, baseSchema []byte) ([]byte, error) {
	return foldLockedOn(ctx, tx, s.tenantID, s.applyFunc, ref, baseSchema, false)
}

// lastSavepointSeqInTx returns the seq of the most-recent savepoint row of
// ref as seen by tx, or 0 if none exists; ExtendSchema's savepoint trigger
// uses it.
func (s *modelStore) lastSavepointSeqInTx(ctx context.Context, tx *sql.Tx, ref spi.ModelRef) (int64, error) {
	return lastSavepointSeqOn(ctx, tx, s.tenantID, ref)
}

func lastSavepointSeqOn(ctx context.Context, q *sql.Tx, tenantID spi.TenantID, ref spi.ModelRef) (int64, error) {
	var seq int64
	err := q.QueryRowContext(ctx, `
		SELECT seq FROM model_schema_extensions
		WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND kind = 'savepoint'
		ORDER BY seq DESC LIMIT 1`,
		string(tenantID), ref.EntityName, ref.ModelVersion).Scan(&seq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("lastSavepointSeq: %w", err)
	default:
		return seq, nil
	}
}

// foldLockedOn is the fold of foldLockedInTx. With canonical set the start
// goes through applyFunc even when no delta follows it, so the result is
// always in the apply function's encoding: an extension compares it byte for
// byte with the result of applying the new delta.
func foldLockedOn(ctx context.Context, q *sql.Tx, tenantID spi.TenantID, applyFunc ApplyFunc, ref spi.ModelRef, baseSchema []byte, canonical bool) ([]byte, error) {
	var savepointSeq int64
	var savepointPayload []byte
	err := q.QueryRowContext(ctx, `
		SELECT seq, payload FROM model_schema_extensions
		WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND kind = 'savepoint'
		ORDER BY seq DESC LIMIT 1`,
		string(tenantID), ref.EntityName, ref.ModelVersion).Scan(&savepointSeq, &savepointPayload)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		savepointSeq = 0
		savepointPayload = nil
	case err != nil:
		return nil, fmt.Errorf("savepoint lookup: %w", err)
	}

	current := savepointPayload
	if current == nil {
		current = baseSchema
	}

	rows, err := q.QueryContext(ctx, `
		SELECT payload FROM model_schema_extensions
		WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND kind = 'delta' AND seq > ?
		ORDER BY seq ASC`,
		string(tenantID), ref.EntityName, ref.ModelVersion, savepointSeq)
	if err != nil {
		return nil, fmt.Errorf("delta scan: %w", err)
	}
	defer rows.Close()

	var deltas []spi.SchemaDelta
	for rows.Next() {
		var deltaBytes []byte
		if err := rows.Scan(&deltaBytes); err != nil {
			return nil, fmt.Errorf("scan delta: %w", err)
		}
		deltas = append(deltas, spi.SchemaDelta(deltaBytes))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("delta iteration: %w", err)
	}
	// A read with no delta to apply needs no apply function; anything else
	// does.
	if len(deltas) == 0 && !canonical {
		return current, nil
	}
	if applyFunc == nil {
		return nil, fmt.Errorf("cannot fold the schema of %s: ApplyFunc is not wired on the factory", ref)
	}
	current, err = applyFunc(current, deltas)
	if err != nil {
		return nil, fmt.Errorf("apply %d deltas after seq %d: %w", len(deltas), savepointSeq, err)
	}
	return current, nil
}
