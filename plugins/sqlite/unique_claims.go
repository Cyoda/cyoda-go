package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// replaceClaims recomputes the unique-key claims for entity e and atomically
// refreshes them in the unique_claims table within the caller's SQL transaction.
//
// Flow:
//  1. ComputeClaims derives one UniqueClaim per UniqueKey. Returns
//     ErrPartialUniqueKey if any key has a non-scalar or partially-populated
//     value — propagated to the caller unchanged.
//  2. If claims is empty (no keys declared or all fields absent) → no-op.
//  3. DELETE the entity's existing claim rows (idempotent; handles the
//     update-moves-key case — old value is freed, same-entity re-save
//     does not self-collide).
//  4. INSERT one row per claim. INSERT errors are mapped via classifyClaimError
//     so a duplicate signature surfaces as spi.ErrUniqueViolation (not
//     spi.ErrConflict, which classifyError would produce for entity-PK violations).
//
// Called for both the buffered flush path (flushToSQLite) and the direct
// non-transactional save path (saveDirectly). In both cases the caller supplies
// the enclosing *sql.Tx.
func replaceClaims(ctx context.Context, sqlTx *sql.Tx, tid string, e *spi.Entity, keys []spi.UniqueKey) error {
	claims, err := spi.ComputeClaims(keys, e.Data)
	if err != nil {
		return err // ErrPartialUniqueKey family — caller maps to 422
	}
	if len(claims) == 0 {
		return nil // no keys declared or all fields absent — nothing to maintain
	}

	// Delete-first: free any previously-held claim rows for this entity so that
	// an update that moves a key value releases the old slot before inserting
	// the new one, and a re-save with the same value does not self-collide.
	if _, err := sqlTx.ExecContext(ctx,
		`DELETE FROM unique_claims WHERE tenant_id = ? AND entity_id = ?`,
		tid, e.Meta.ID,
	); err != nil {
		return fmt.Errorf("clear claims for %s: %w", e.Meta.ID, err)
	}

	for _, c := range claims {
		if _, err := sqlTx.ExecContext(ctx,
			`INSERT INTO unique_claims (tenant_id, model_name, model_version, key_id, signature, entity_id)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			tid, e.Meta.ModelRef.EntityName, e.Meta.ModelRef.ModelVersion,
			c.KeyID, c.Signature, e.Meta.ID,
		); err != nil {
			return fmt.Errorf("insert claim for %s: %w", e.Meta.ID, classifyClaimError(err))
		}
	}
	return nil
}

// releaseClaims removes all unique-key claims held by entityID within the
// caller's SQL transaction. Called from the delete path so the freed values
// can be claimed by another entity immediately.
func releaseClaims(ctx context.Context, sqlTx *sql.Tx, tid string, entityID string) error {
	if _, err := sqlTx.ExecContext(ctx,
		`DELETE FROM unique_claims WHERE tenant_id = ? AND entity_id = ?`,
		tid, entityID,
	); err != nil {
		return fmt.Errorf("release claims for %s: %w", entityID, err)
	}
	return nil
}
