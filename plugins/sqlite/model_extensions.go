package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// foldLocked returns the fully-folded schema for ref, read through tx, and the
// seq of the savepoint it started from (0 if none). It starts from the
// most-recent savepoint payload (if any), else from baseSchema
// (models.doc.schema), and applies every later delta row, in seq order, in one
// call of the injected ApplyFunc. With no extensions it returns that start
// verbatim and needs no ApplyFunc, unless canonical is set; otherwise a nil
// ApplyFunc is refused. With canonical set the start goes through the
// ApplyFunc even when no delta follows it, so the result is always in the
// apply function's encoding: an extension compares it byte for byte with the
// result of applying the new delta, and counts its deltas from the savepoint
// seq. It reads the latest savepoint, then the deltas after it, in two
// statements of the caller's transaction; the caller supplies the base schema.
func (s *modelStore) foldLocked(ctx context.Context, q *sql.Tx, ref spi.ModelRef, baseSchema []byte, canonical bool) ([]byte, int64, error) {
	var savepointSeq int64
	var savepointPayload []byte
	err := q.QueryRowContext(ctx, `
		SELECT seq, payload FROM model_schema_extensions
		WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND kind = 'savepoint'
		ORDER BY seq DESC LIMIT 1`,
		string(s.tenantID), ref.EntityName, ref.ModelVersion).Scan(&savepointSeq, &savepointPayload)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		savepointSeq = 0
		savepointPayload = nil
	case err != nil:
		return nil, 0, fmt.Errorf("savepoint lookup: %w", err)
	}

	current := savepointPayload
	if current == nil {
		current = baseSchema
	}

	rows, err := q.QueryContext(ctx, `
		SELECT payload FROM model_schema_extensions
		WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND kind = 'delta' AND seq > ?
		ORDER BY seq ASC`,
		string(s.tenantID), ref.EntityName, ref.ModelVersion, savepointSeq)
	if err != nil {
		return nil, 0, fmt.Errorf("delta scan: %w", err)
	}
	defer rows.Close()

	var deltas []spi.SchemaDelta
	for rows.Next() {
		var deltaBytes []byte
		if err := rows.Scan(&deltaBytes); err != nil {
			return nil, 0, fmt.Errorf("scan delta: %w", err)
		}
		deltas = append(deltas, spi.SchemaDelta(deltaBytes))
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("delta iteration: %w", err)
	}
	// A read with no delta to apply needs no apply function; anything else
	// does.
	if len(deltas) == 0 && !canonical {
		return current, savepointSeq, nil
	}
	if s.applyFunc == nil {
		return nil, 0, fmt.Errorf("cannot fold the schema of %s: ApplyFunc is not wired on the factory", ref)
	}
	current, err = s.applyFunc(current, deltas)
	if err != nil {
		return nil, 0, fmt.Errorf("apply %d deltas after seq %d: %w", len(deltas), savepointSeq, err)
	}
	return current, savepointSeq, nil
}
