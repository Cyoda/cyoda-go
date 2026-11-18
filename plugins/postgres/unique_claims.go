package postgres

import (
	"context"

	spi "github.com/cyoda/cyoda-go-spi"
)

// replaceClaims refreshes the unique-key claims of e, a save outside any
// transaction, in the transaction that save opened for itself. It is steps
// 1-4 of the commit-time check (commit_claims.go) for one entity: the keys
// are read from the stored model on that READ COMMITTED transaction, which
// sees the latest committed model; a partial composite key is
// spi.ErrPartialUniqueKey; a value held by another entity is
// spi.ErrUniqueViolation, classified by the querier.
//
// The claims are computed from e.Data, the bytes the save was given. The
// entity's old claims are always released, so a value its new data no longer
// holds is freed. It runs before stampOwnCommitInstant, so no lock wait
// follows the stamp. A save inside a caller's transaction does no claim work:
// the transaction's Commit runs the same steps over its final entities
// (TransactionManager.commitWrittenClaims).
func (s *entityStore) replaceClaims(ctx context.Context, e *spi.Entity) error {
	written := map[string]writtenEntity{e.Meta.ID: {ref: e.Meta.ModelRef, data: e.Data}}
	if err := releaseClaims(ctx, s.q, s.tenantID, []string{e.Meta.ID}); err != nil {
		return err
	}
	keys, err := modelKeys(ctx, s.q, s.tenantID, savedModels(written))
	if err != nil {
		return err
	}
	return insertClaims(ctx, s.q, s.tenantID, written, keys)
}
