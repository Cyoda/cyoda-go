package postgres

import (
	"context"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// replaceClaims checks the model and refreshes the unique-key claims of e, a
// save outside any transaction, in the transaction that save opened for
// itself (READ COMMITTED). It is steps 1-4 of the commit-time check
// (commit_claims.go) for one entity: the FOR SHARE of the model's lifecycle
// row as a statement of its own, then the models read as a later statement,
// whose snapshot includes the commit of any bump the FOR SHARE waited for.
// A model that does not exist or is not LOCKED is spi.ErrModelNotLocked; a
// partial composite key is spi.ErrPartialUniqueKey; a value held by another
// entity is spi.ErrUniqueViolation, classified by the querier.
//
// The claims are computed from e.Data, the bytes the save was given. The
// entity's old claims are always released, so a value its new data no longer
// holds is freed. It runs before stampOwnCommitInstant, so no lock wait
// follows the stamp. A save inside a caller's transaction does no claim work:
// the transaction's Commit runs commitClaims over its final entities.
func (s *entityStore) replaceClaims(ctx context.Context, e *spi.Entity) error {
	return applyClaims(ctx, s.q, s.tenantID,
		map[string][]byte{e.Meta.ID: savedBytes(e.Data)},
		map[string]spi.ModelRef{e.Meta.ID: e.Meta.ModelRef},
		false)
}

// savedBytes is the bytes record of a save: data, made non-nil, because nil
// records a delete.
func savedBytes(data []byte) []byte {
	if data == nil {
		return []byte{}
	}
	return data
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
