package postgres

import (
	"context"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// foldedModel is a model as one statement read it: the stored doc, the
// descriptor with the extension log folded into its schema, and the number
// of deltas the next savepoint would fold.
type foldedModel struct {
	// doc is models.doc as stored.
	doc []byte
	// desc is the descriptor from doc, its Schema replaced by the fold.
	desc *spi.ModelDescriptor
	// pending counts the deltas after the latest savepoint.
	pending int
}

// foldModelSQL reads, in one statement and so in one snapshot, the model's
// doc (part 0), its latest savepoint (part 1) and the deltas after that
// savepoint (part 2), in that order. Separate statements could combine a doc
// from before an Unlock with the log that Unlock cleared.
const foldModelSQL = `
	WITH sp AS (
		SELECT seq, payload FROM model_schema_extensions
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND kind = 'savepoint'
		ORDER BY seq DESC LIMIT 1
	)
	SELECT 0 AS part, 0::bigint AS seq, doc AS payload FROM models
	WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3
	UNION ALL
	SELECT 1, seq, payload FROM sp
	UNION ALL
	SELECT 2, e.seq, e.payload FROM model_schema_extensions e
	WHERE e.tenant_id = $1 AND e.model_name = $2 AND e.model_version = $3 AND e.kind = 'delta'
	  AND e.seq > COALESCE((SELECT seq FROM sp), 0)
	ORDER BY part, seq`

// foldModel reads ref with foldModelSQL on q and folds the log: from the
// latest savepoint if there is one, else from the doc's schema, applying
// every later delta, in seq order, in one call of apply. A model without
// deltas to apply folds without an apply function, and its schema is the
// start verbatim — unless canonical is set, when the start goes through apply
// with an empty list, so the result is always in the apply function's
// encoding (an extension compares it byte for byte with the result of
// applying the new delta). A missing model is spi.ErrNotFound.
func foldModel(ctx context.Context, q Querier, tenantID spi.TenantID, ref spi.ModelRef, apply ApplyFunc, canonical bool) (*foldedModel, error) {
	rows, err := q.Query(ctx, foldModelSQL, string(tenantID), ref.EntityName, ref.ModelVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to read model %s: %w", ref, err)
	}
	defer rows.Close()

	var m foldedModel
	var start []byte
	var deltas []spi.SchemaDelta
	for rows.Next() {
		var (
			part    int
			seq     int64
			payload []byte
		)
		if err := rows.Scan(&part, &seq, &payload); err != nil {
			return nil, fmt.Errorf("failed to read model %s: %w", ref, err)
		}
		if part != 0 && m.desc == nil {
			break // the doc sorts first: log rows without one belong to no model
		}
		switch part {
		case 0:
			m.doc = payload
			if m.desc, err = unmarshalModelDoc(payload); err != nil {
				return nil, err
			}
			start = m.desc.Schema
		case 1:
			start = payload
		default:
			deltas = append(deltas, spi.SchemaDelta(payload))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read model %s: %w", ref, err)
	}
	if m.desc == nil {
		return nil, fmt.Errorf("model %s not found: %w", ref, spi.ErrNotFound)
	}
	m.pending = len(deltas)
	if len(deltas) == 0 && !canonical {
		m.desc.Schema = start
		return &m, nil
	}
	if apply == nil {
		return nil, fmt.Errorf("model %s has pending schema deltas but ApplyFunc is not wired on the factory", ref)
	}
	if m.desc.Schema, err = apply(start, deltas); err != nil {
		return nil, fmt.Errorf("fold extension log for %s: apply %d deltas: %w", ref, len(deltas), err)
	}
	return &m, nil
}
