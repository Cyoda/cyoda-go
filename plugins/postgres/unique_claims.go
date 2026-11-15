package postgres

import (
	"context"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// replaceClaims refreshes the unique-key claims of e, a save outside any
// transaction, in the transaction that save opened for itself. It is steps
// 1-4 of the commit-time check (commitClaims) for one entity: the keys are
// read from the stored model on that READ COMMITTED transaction, which sees
// the latest committed model; a partial composite key is
// spi.ErrPartialUniqueKey; a value held by another entity is
// spi.ErrUniqueViolation, classified by the querier.
//
// The claims are computed from e.Data, the bytes the save was given. The
// entity's old claims are always released, so a value its new data no longer
// holds is freed. It runs before stampOwnCommitInstant, so no lock wait
// follows the stamp. A save inside a caller's transaction does no claim work:
// the transaction's Commit runs commitClaims over its final entities.
func (s *entityStore) replaceClaims(ctx context.Context, e *spi.Entity) error {
	return commitClaims(ctx, s.q, s.q, s.tenantID,
		map[string]writtenEntity{e.Meta.ID: {ref: e.Meta.ModelRef, data: e.Data}})
}

// releaseClaims removes all unique-key claims held by entityID. Called from a
// delete outside any transaction, so the freed values can be claimed as soon
// as it commits. A delete inside a caller's transaction releases nothing
// itself: the transaction's Commit releases the claims of every entity it
// wrote (commitClaims).
func (s *entityStore) releaseClaims(ctx context.Context, entityID string) error {
	_, err := s.q.Exec(ctx,
		`DELETE FROM unique_claims WHERE tenant_id=$1 AND entity_id=$2`,
		string(s.tenantID), entityID,
	)
	return err
}
