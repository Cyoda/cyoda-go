package postgres

import (
	"context"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// replaceClaims recomputes the unique-key claims for entity e and atomically
// refreshes them in the unique_claims table within the caller's transaction.
//
// Flow:
//  1. Early-return if no unique keys are declared for this entity (optimization).
//  2. ComputeClaims derives one UniqueClaim per UniqueKey in context.
//     Returns ErrPartialUniqueKey if any key has a non-scalar or partially-
//     populated value — propagated to the caller unchanged.
//  3. DELETE the entity's existing claim rows (always, when keys are declared).
//     This frees the old value even when all key fields go null/absent on an
//     update (the "all-null exempt" transition); skipping the DELETE here would
//     leave an orphaned claim that blocks future entities from claiming that value.
//  4. INSERT one row per claim (zero rows on all-null exempt). The
//     unique_claims_uq index enforces uniqueness; a duplicate INSERT arrives
//     pre-wrapped as spi.ErrUniqueViolation via ctxQuerier / classifyError —
//     no manual wrapping needed here.
//
// Called at the end of Save (after the entity + version rows are written).
func (s *entityStore) replaceClaims(ctx context.Context, e *spi.Entity) error {
	keys := spi.UniqueKeysFromContext(ctx)
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
	tid := string(s.tenantID)
	if _, err := s.q.Exec(ctx,
		`DELETE FROM unique_claims WHERE tenant_id=$1 AND entity_id=$2`,
		tid, e.Meta.ID,
	); err != nil {
		return fmt.Errorf("clear claims: %w", err)
	}
	for _, c := range claims {
		if _, err := s.q.Exec(ctx,
			`INSERT INTO unique_claims (tenant_id,model_name,model_version,key_id,signature,entity_id)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			tid, e.Meta.ModelRef.EntityName, e.Meta.ModelRef.ModelVersion,
			c.KeyID, c.Signature, e.Meta.ID,
		); err != nil {
			return fmt.Errorf("insert claim: %w", err) // already classified by ctxQuerier
		}
	}
	return nil
}

// releaseClaims removes all unique-key claims held by entityID.
// Called from Delete (soft-delete) so the freed values can be claimed immediately.
func (s *entityStore) releaseClaims(ctx context.Context, entityID string) error {
	_, err := s.q.Exec(ctx,
		`DELETE FROM unique_claims WHERE tenant_id=$1 AND entity_id=$2`,
		string(s.tenantID), entityID,
	)
	return err
}
