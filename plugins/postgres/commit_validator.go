package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	spi "github.com/cyoda/cyoda-go-spi"
)

const defaultValidateChunkSize = 1000

// validateInChunks issues SELECT entity_id, version FROM entities WHERE
// tenant_id=$1 AND entity_id=ANY($2::text[]) FOR SHARE over sortedIDs,
// chunked at chunkSize.
//
// Returns map of entity_id → current committed version. Entities absent
// from the DB are absent from the returned map.
//
// FOR SHARE (not FOR UPDATE): concurrent readers are not blocked; only
// concurrent writers are detected. Write-write conflicts on DML rows
// are caught by postgres's own tuple-level locks upstream; this query
// validates the read-set staleness + locks the rows for the
// validate-then-commit window. See design spec for the dual-mechanism
// argument.
func (tm *TransactionManager) validateInChunks(
	ctx context.Context, tx pgx.Tx, tenantID spi.TenantID, sortedIDs []string, chunkSize int,
) (map[string]int64, error) {
	if chunkSize <= 0 {
		chunkSize = defaultValidateChunkSize
	}
	current := make(map[string]int64, len(sortedIDs))
	for i := 0; i < len(sortedIDs); i += chunkSize {
		end := i + chunkSize
		if end > len(sortedIDs) {
			end = len(sortedIDs)
		}
		chunk := sortedIDs[i:end]
		rows, err := tx.Query(ctx, `
			SELECT entity_id, version
			  FROM entities
			 WHERE tenant_id = $1
			   AND entity_id = ANY($2::text[])
			 FOR SHARE
		`, string(tenantID), chunk)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var v int64
			if err := rows.Scan(&id, &v); err != nil {
				rows.Close()
				return nil, err
			}
			current[id] = v
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return current, nil
}
