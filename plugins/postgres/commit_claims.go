package postgres

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Unique keys are checked when a write commits, from the transaction's final
// entities and the unique keys of their models as stored:
//
//  1. Read the unique keys of the model of every entity whose last write is
//     a save, in one statement, from the models rows, as they are committed
//     at that moment. The model's state is not checked (the caller saves
//     entities only into a LOCKED model), and a model the read does not find
//     contributes no keys. An entity whose last write is a delete needs no
//     model.
//  2. Release the claims of every entity written.
//  3. Compute the claims of every entity whose last write is a save, from the
//     bytes that save was given and its model's keys. A partial composite key
//     fails with spi.ErrPartialUniqueKey.
//  4. Insert those claims in (model, key id, signature) order. The immediate
//     index unique_claims_uq refuses a value claimed twice or held by an
//     entity outside the writes: 23505, spi.ErrUniqueViolation once
//     classified.
//
// Step 1 takes no lock, and it must not read in the transaction's snapshot.
// An entity transaction is REPEATABLE READ, and its snapshot is older than
// the commit: a model created and locked after the snapshot has no row there,
// and a model re-keyed after it (unlocked, given new keys and locked again
// while it had no entities) has its old keys there. Either is a LOCKED model
// the API lets the transaction write into, and the commit must enforce the
// keys the model holds now, as memory and sqlite do. So the transactional
// commit reads them on the model pool, outside its transaction, and gives
// the connection back before steps 2-4 (TransactionManager.readModelKeys):
// step 4 can wait on another transaction's uncommitted claim, and a model
// connection held through that wait would bound entity commits by the model
// pool's size and starve the model reads the pool protects. A write outside
// a transaction reads them on its own READ COMMITTED transaction, which sees
// the latest committed model too (replaceClaims).
//
// Every step runs before the commit stamp: nothing after cyoda_stamp may
// wait on a lock (stampCommitInstant).

// commitClaims runs steps 2-4 on q, with keys, the unique keys step 1 read
// (modelKeys) for the models of the entities whose last write is a save
// (savedModels). It runs at Commit inside a caller's transaction, and for a
// write outside a transaction (replaceClaims) in the transaction that write
// opened for itself. written is the record of the entities written
// (txState.WrittenEntities): the model and bytes of each entity's last save,
// or a delete. Both save paths store the entity under the model the save
// names (the upsert's model guard, replacePendingSave's
// spi.ErrEntityModelMismatch), so the recorded model is the stored one.
func commitClaims(ctx context.Context, q Querier, tenantID spi.TenantID, written map[string]writtenEntity, keys map[spi.ModelRef][]spi.UniqueKey) error {
	if len(written) == 0 {
		return nil
	}

	// Step 3, before any write: a partial key fails the whole commit.
	var claims []claimRow
	for id, w := range written {
		if w.deleted {
			continue
		}
		k := keys[w.ref]
		if len(k) == 0 {
			continue
		}
		cs, err := spi.ComputeClaims(k, w.data)
		if err != nil {
			return fmt.Errorf("entity %s: %w", id, err)
		}
		for _, c := range cs {
			claims = append(claims, claimRow{ref: w.ref, keyID: c.KeyID, signature: c.Signature, entityID: id})
		}
	}
	slices.SortFunc(claims, func(a, b claimRow) int {
		return cmp.Or(
			cmp.Compare(a.ref.EntityName, b.ref.EntityName),
			cmp.Compare(a.ref.ModelVersion, b.ref.ModelVersion),
			cmp.Compare(a.keyID, b.keyID),
			cmp.Compare(a.signature, b.signature),
			cmp.Compare(a.entityID, b.entityID),
		)
	})

	// Step 2.
	ids := make([]string, 0, len(written))
	for id := range written {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if _, err := q.Exec(ctx,
		`DELETE FROM unique_claims WHERE tenant_id = $1 AND entity_id = ANY($2::text[])`,
		string(tenantID), ids); err != nil {
		return fmt.Errorf("release unique claims: %w", err)
	}

	// Step 4.
	if len(claims) == 0 {
		return nil
	}
	names := make([]string, len(claims))
	versions := make([]string, len(claims))
	keyIDs := make([]string, len(claims))
	sigs := make([]string, len(claims))
	owners := make([]string, len(claims))
	for i, c := range claims {
		names[i], versions[i], keyIDs[i], sigs[i], owners[i] =
			c.ref.EntityName, c.ref.ModelVersion, c.keyID, c.signature, c.entityID
	}
	// The rows are inserted in the order of the arrays, which is the sorted
	// order above: two commits inserting overlapping values meet in the same
	// order and wait instead of deadlocking.
	if _, err := q.Exec(ctx,
		`INSERT INTO unique_claims (tenant_id, model_name, model_version, key_id, signature, entity_id)
		 SELECT $1, c.model_name, c.model_version, c.key_id, c.signature, c.entity_id
		   FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[])
		        WITH ORDINALITY AS c(model_name, model_version, key_id, signature, entity_id, ord)
		  ORDER BY c.ord`,
		string(tenantID), names, versions, keyIDs, sigs, owners); err != nil {
		return fmt.Errorf("insert unique claims: %w", err)
	}
	return nil
}

// claimRow is one unique_claims row a commit inserts.
type claimRow struct {
	ref       spi.ModelRef
	keyID     string
	signature string
	entityID  string
}

// savedModels returns the distinct models of the entities whose last write is
// a save.
func savedModels(written map[string]writtenEntity) []spi.ModelRef {
	seen := make(map[spi.ModelRef]struct{}, len(written))
	out := make([]spi.ModelRef, 0, len(written))
	for _, w := range written {
		if w.deleted {
			continue
		}
		if _, ok := seen[w.ref]; ok {
			continue
		}
		seen[w.ref] = struct{}{}
		out = append(out, w.ref)
	}
	return out
}

// modelKeys is step 1: one statement reads the unique keys of every model in
// models from its models row. A model it does not find contributes no keys.
func modelKeys(ctx context.Context, q Querier, tenantID spi.TenantID, models []spi.ModelRef) (map[spi.ModelRef][]spi.UniqueKey, error) {
	keys := make(map[spi.ModelRef][]spi.UniqueKey, len(models))
	if len(models) == 0 {
		return keys, nil
	}
	names := make([]string, len(models))
	versions := make([]string, len(models))
	for i, ref := range models {
		names[i], versions[i] = ref.EntityName, ref.ModelVersion
	}
	rows, err := q.Query(ctx,
		`SELECT m.model_name, m.model_version, m.doc->'uniqueKeys'
		   FROM models m
		   JOIN unnest($2::text[], $3::text[]) AS r(model_name, model_version)
		     ON m.model_name = r.model_name AND m.model_version = r.model_version
		  WHERE m.tenant_id = $1`,
		string(tenantID), names, versions)
	if err != nil {
		return nil, fmt.Errorf("read the unique keys of the models: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref spi.ModelRef
		var rawKeys []byte
		if err := rows.Scan(&ref.EntityName, &ref.ModelVersion, &rawKeys); err != nil {
			return nil, fmt.Errorf("read the unique keys of the models: %w", err)
		}
		var k []spi.UniqueKey
		if len(rawKeys) > 0 {
			if err := json.Unmarshal(rawKeys, &k); err != nil {
				return nil, fmt.Errorf("read the unique keys of model %s: %w", ref, err)
			}
		}
		keys[ref] = k
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the unique keys of the models: %w", err)
	}
	return keys, nil
}
