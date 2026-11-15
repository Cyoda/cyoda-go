package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// commitClaims reads the models' keys and writes the unique-key claims of one
// commit, in the commit's own sqlTx: the transaction flush, or the single
// write of a save outside a transaction.
//
// saves are the entities whose final write in the commit is a save; released
// are the ids of every entity the commit writes or deletes. In order:
//
//  1. Read the unique keys of the stored model of every entity in saves,
//     one read per distinct model. The model's state is not checked (the
//     caller saves entities only into a LOCKED model), and a model not found
//     contributes no keys. A delete needs no model.
//  2. Release the claims of every entity in released.
//  3. Compute the claims of every entity in saves from the bytes it was
//     given, with its model's stored unique keys. A partial composite key
//     fails with spi.ErrPartialUniqueKey.
//  4. Insert every claim. A value two entities of the commit claim, or one
//     an entity outside the commit holds, violates the unique index and
//     fails with spi.ErrUniqueViolation.
//
// Every claim is released before any is inserted, so a swap or a chain of
// moves inside one commit is judged only on its final state. Any error
// leaves the caller to roll sqlTx back, with every other write of the commit.
//
// The models are read with sqlTx, never through the model store: the model
// store writes through the writer pool, which has one connection, and the
// commit already holds it.
func commitClaims(ctx context.Context, sqlTx *sql.Tx, tid string, saves []*spi.Entity, released []string) error {
	keys, err := modelKeys(ctx, sqlTx, tid, saves)
	if err != nil {
		return err
	}

	for _, id := range released {
		if err := releaseClaims(ctx, sqlTx, tid, id); err != nil {
			return err
		}
	}

	type claimRow struct {
		e *spi.Entity
		c spi.UniqueClaim
	}
	var rows []claimRow
	for _, e := range saves {
		modelKeys := keys[e.Meta.ModelRef]
		if len(modelKeys) == 0 {
			continue
		}
		claims, err := spi.ComputeClaims(modelKeys, e.Data)
		if err != nil {
			return fmt.Errorf("unique keys of entity %s: %w", e.Meta.ID, err)
		}
		for _, c := range claims {
			rows = append(rows, claimRow{e: e, c: c})
		}
	}

	for _, r := range rows {
		if _, err := sqlTx.ExecContext(ctx,
			`INSERT INTO unique_claims (tenant_id, model_name, model_version, key_id, signature, entity_id)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			tid, r.e.Meta.ModelRef.EntityName, r.e.Meta.ModelRef.ModelVersion,
			r.c.KeyID, r.c.Signature, r.e.Meta.ID,
		); err != nil {
			return fmt.Errorf("insert claim for %s: %w", r.e.Meta.ID, classifyClaimError(err))
		}
	}
	return nil
}

// modelKeys reads, in sqlTx, the unique keys of the stored model of every
// entity in saves, one read per distinct model. A model not found
// contributes no keys.
func modelKeys(ctx context.Context, sqlTx *sql.Tx, tid string, saves []*spi.Entity) (map[spi.ModelRef][]spi.UniqueKey, error) {
	keys := make(map[spi.ModelRef][]spi.UniqueKey)
	for _, e := range saves {
		ref := e.Meta.ModelRef
		if _, read := keys[ref]; read {
			continue
		}
		var rawKeys sql.NullString
		err := sqlTx.QueryRowContext(ctx,
			`SELECT json_extract(doc, '$.uniqueKeys')
			 FROM models WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
			tid, ref.EntityName, ref.ModelVersion).Scan(&rawKeys)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("read model %s: %w", ref, err)
		}
		var uk []spi.UniqueKey
		if rawKeys.Valid {
			if err := json.Unmarshal([]byte(rawKeys.String), &uk); err != nil {
				return nil, fmt.Errorf("decode unique keys of model %s: %w", ref, err)
			}
		}
		keys[ref] = uk
	}
	return keys, nil
}

// releaseClaims removes all unique-key claims held by entityID within the
// caller's SQL transaction.
func releaseClaims(ctx context.Context, sqlTx *sql.Tx, tid string, entityID string) error {
	if _, err := sqlTx.ExecContext(ctx,
		`DELETE FROM unique_claims WHERE tenant_id = ? AND entity_id = ?`,
		tid, entityID,
	); err != nil {
		return fmt.Errorf("release claims for %s: %w", entityID, err)
	}
	return nil
}
