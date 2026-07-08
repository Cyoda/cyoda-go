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
//  1. Early-return if no unique keys are declared for this entity (optimization).
//  2. ComputeClaims derives one UniqueClaim per UniqueKey. Returns
//     ErrPartialUniqueKey if any key has a non-scalar or partially-populated
//     value — propagated to the caller unchanged.
//  3. DELETE the entity's existing claim rows (always, when keys are declared).
//     This frees the old value even when all key fields go null/absent on an
//     update (the "all-null exempt" transition); skipping the DELETE here would
//     leave an orphaned claim that blocks future entities from claiming that value.
//  4. INSERT one row per claim (zero rows on all-null exempt). INSERT errors are
//     mapped via classifyClaimError so a duplicate signature surfaces as
//     spi.ErrUniqueViolation (not spi.ErrConflict, which classifyError would
//     produce for entity-PK violations).
//
// Called for both the buffered flush path (flushToSQLite) and the direct
// non-transactional save path (saveDirectly). In both cases the caller supplies
// the enclosing *sql.Tx.
func replaceClaims(ctx context.Context, sqlTx *sql.Tx, tid string, e *spi.Entity, keys []spi.UniqueKey) error {
	if len(keys) == 0 {
		return nil // no declared keys — no claim work (optimization)
	}
	claims, err := spi.ComputeClaims(keys, e.Data)
	if err != nil {
		return err // ErrPartialUniqueKey family — caller maps to 422
	}
	// ALWAYS delete-first when keys are declared: frees any old claim even
	// when all key fields go null/absent on an update (all-null exempt).
	// Then insert zero-or-more new claims.
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
