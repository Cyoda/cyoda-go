package postgres

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Unique keys and the model an entity write needs are checked when the write
// commits, from the transaction's final entities and the model as stored:
//
//  1. Lock the lifecycle row (model_lifecycle) of the model of every entity
//     whose last write is a save, FOR SHARE, in (name, version) byte order,
//     and read each model's state and unique keys from the models row. A model
//     that does not exist or is not LOCKED fails with spi.ErrModelNotLocked.
//     An entity whose last write is a delete needs no model.
//  2. Release the claims of every entity written.
//  3. Compute the claims of every entity whose last write is a save, from the
//     bytes that save was given and its model's keys. A partial composite key
//     fails with spi.ErrPartialUniqueKey.
//  4. Insert those claims in (model, key id, signature) order. The immediate
//     index unique_claims_uq refuses a value claimed twice or held by an
//     entity outside the writes: 23505, spi.ErrUniqueViolation once
//     classified.
//
// The lifecycle row, not the models row, is what a commit locks: every
// change of a model's state or unique keys bumps it, and nothing else does.
// A schema extension and a change-level change write the models row only,
// so they neither wait for an entity commit nor make one wait or fail. Every
// model operation writes the models row before it bumps the lifecycle row,
// and a commit takes lifecycle rows only and reads models rows without a
// lock, so no transaction waits for a models row while it holds a lifecycle
// row.
//
// Every step runs before the commit stamp: nothing after cyoda_stamp may
// wait on a lock (stampCommitInstant).

// commitClaims runs steps 1-4 inside a caller's transaction, at Commit.
// written is the transaction's record of the entities it wrote
// (txState.WrittenEntities): the model and bytes of each entity's last save,
// or a delete. Both save paths store the entity under the model the save
// names (the upsert's model guard, replacePendingSave's
// spi.ErrEntityModelMismatch), so the recorded model is the stored one.
//
// The transaction is REPEATABLE READ, and its snapshot is older than the
// commit. Step 1 is one statement here, since every read shares that
// snapshot (modelsInSnapshot). The FOR SHARE makes the snapshot's model the
// current one: a lifecycle row bumped and committed after the snapshot fails
// it with 40001 (a retryable spi.ErrConflict once classified), and an
// uncommitted bump makes it wait for the outcome. So the state and keys read
// from the snapshot's models row are the ones in force, and stay so until the
// commit ends.
//
// A lifecycle row the snapshot does not see is either a model that does not
// exist or one created after the snapshot. The probe insert tells them apart:
// a row it inserts is a missing model (spi.ErrModelNotLocked; the
// transaction rolls back and takes the row with it), and a conflict with a
// row the snapshot cannot see fails it with 40001.
func commitClaims(ctx context.Context, q Querier, tenantID spi.TenantID, written map[string]writtenEntity) error {
	return applyClaims(ctx, q, tenantID, written, true)
}

// applyClaims is steps 1-4 on q. inSnapshot is true inside a caller's
// REPEATABLE READ transaction (commitClaims). It is false for a write outside
// a transaction (replaceClaims), which runs READ COMMITTED: there every
// statement takes a fresh snapshot, so the FOR SHARE of each lifecycle row is
// a statement of its own, a missing lifecycle row is a missing model, and the
// models reads, statements after the FOR SHARE, see the commit of any bump
// the FOR SHARE waited for (modelsReadCommitted).
func applyClaims(ctx context.Context, q Querier, tenantID spi.TenantID, written map[string]writtenEntity, inSnapshot bool) error {
	if len(written) == 0 {
		return nil
	}

	// Step 1.
	models := savedModels(written)
	var keys map[spi.ModelRef][]spi.UniqueKey
	var err error
	if inSnapshot {
		keys, err = modelsInSnapshot(ctx, q, tenantID, models)
	} else {
		keys, err = modelsReadCommitted(ctx, q, tenantID, models)
	}
	if err != nil {
		return err
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
// a save, by name, then version, in byte order (the order of COLLATE "C").
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
	slices.SortFunc(out, func(a, b spi.ModelRef) int {
		return cmp.Or(cmp.Compare(a.EntityName, b.EntityName), cmp.Compare(a.ModelVersion, b.ModelVersion))
	})
	return out
}

// modelsInSnapshot is step 1 inside a caller's transaction: one statement
// locks the lifecycle rows of models FOR SHARE, in (name, version) byte order,
// and reads each model's state and keys from its models row in the same
// snapshot. A model whose lifecycle row the snapshot does not see is probed
// (probeLifecycle), and refused either way. It returns the keys of every
// model, each of which is LOCKED.
func modelsInSnapshot(ctx context.Context, q Querier, tenantID spi.TenantID, models []spi.ModelRef) (map[spi.ModelRef][]spi.UniqueKey, error) {
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
		`SELECT l.model_name, l.model_version, m.doc->>'state', m.doc->'uniqueKeys'
		   FROM model_lifecycle l
		   JOIN unnest($2::text[], $3::text[]) AS r(model_name, model_version)
		     ON l.model_name = r.model_name AND l.model_version = r.model_version
		   LEFT JOIN models m
		     ON m.tenant_id = l.tenant_id AND m.model_name = l.model_name AND m.model_version = l.model_version
		  WHERE l.tenant_id = $1
		  ORDER BY l.model_name COLLATE "C", l.model_version COLLATE "C"
		    FOR SHARE OF l`,
		string(tenantID), names, versions)
	if err != nil {
		return nil, fmt.Errorf("lock and read the models: %w", err)
	}
	// The models row exists wherever the lifecycle row does: a model's Save
	// and Delete write both in one transaction, and the migration filled the
	// lifecycle rows of existing models. A NULL state would fail the scan.
	type modelRow struct {
		state   string
		rawKeys []byte
	}
	found := make(map[spi.ModelRef]modelRow, len(models))
	for rows.Next() {
		var ref spi.ModelRef
		var r modelRow
		if err := rows.Scan(&ref.EntityName, &ref.ModelVersion, &r.state, &r.rawKeys); err != nil {
			rows.Close()
			return nil, fmt.Errorf("lock and read the models: %w", err)
		}
		found[ref] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock and read the models: %w", err)
	}
	for _, ref := range models {
		r, ok := found[ref]
		if !ok {
			continue
		}
		k, err := lockedKeys(ref, r.state, r.rawKeys)
		if err != nil {
			return nil, err
		}
		keys[ref] = k
	}
	for _, ref := range models {
		if _, ok := found[ref]; !ok {
			return nil, probeLifecycle(ctx, q, tenantID, ref)
		}
	}
	return keys, nil
}

// probeLifecycle tells a model that does not exist from one created after the
// snapshot, for a lifecycle row the snapshot does not see (see
// commitClaims), and refuses the commit either way: spi.ErrModelNotLocked for
// the first, 40001 for the second.
func probeLifecycle(ctx context.Context, q Querier, tenantID spi.TenantID, ref spi.ModelRef) error {
	var one int
	err := q.QueryRow(ctx,
		`INSERT INTO model_lifecycle (tenant_id, model_name, model_version)
		 VALUES ($1, $2, $3)
		 ON CONFLICT DO NOTHING
		 RETURNING 1`,
		string(tenantID), ref.EntityName, ref.ModelVersion).Scan(&one)
	if err == nil {
		return fmt.Errorf("model %s does not exist: %w", ref, spi.ErrModelNotLocked)
	}
	// A conflict with a row this snapshot cannot see is 40001. No row
	// returned would be a conflict with a row it can see, which the FOR SHARE
	// did not find; it is an error like any other.
	return fmt.Errorf("probe the lifecycle of model %s: %w", ref, err)
}

// modelsReadCommitted is step 1 for a write outside a transaction (READ
// COMMITTED): the FOR SHARE of each model's lifecycle row is a statement of
// its own, in (name, version) order, and each models read is a later
// statement, whose snapshot includes the commit of any bump the FOR SHARE
// waited for. A missing lifecycle row is a missing model.
func modelsReadCommitted(ctx context.Context, q Querier, tenantID spi.TenantID, models []spi.ModelRef) (map[spi.ModelRef][]spi.UniqueKey, error) {
	for _, ref := range models {
		var one int
		err := q.QueryRow(ctx,
			`SELECT 1 FROM model_lifecycle
			  WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3
			    FOR SHARE`,
			string(tenantID), ref.EntityName, ref.ModelVersion).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("model %s does not exist: %w", ref, spi.ErrModelNotLocked)
		}
		if err != nil {
			return nil, fmt.Errorf("lock the lifecycle of model %s: %w", ref, err)
		}
	}
	keys := make(map[spi.ModelRef][]spi.UniqueKey, len(models))
	for _, ref := range models {
		var state string
		var rawKeys []byte
		err := q.QueryRow(ctx,
			`SELECT doc->>'state', doc->'uniqueKeys' FROM models
			  WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
			string(tenantID), ref.EntityName, ref.ModelVersion).Scan(&state, &rawKeys)
		// The FOR SHARE held on the lifecycle row keeps a Delete of the
		// model from committing, so the models row is there.
		if err != nil {
			return nil, fmt.Errorf("read model %s: %w", ref, err)
		}
		k, err := lockedKeys(ref, state, rawKeys)
		if err != nil {
			return nil, err
		}
		keys[ref] = k
	}
	return keys, nil
}

// lockedKeys returns the unique keys of a model read with the given state and
// keys document, refusing a model that is not LOCKED with
// spi.ErrModelNotLocked.
func lockedKeys(ref spi.ModelRef, state string, rawKeys []byte) ([]spi.UniqueKey, error) {
	if state != string(spi.ModelLocked) {
		return nil, fmt.Errorf("model %s is %s: %w", ref, state, spi.ErrModelNotLocked)
	}
	var keys []spi.UniqueKey
	if len(rawKeys) > 0 {
		if err := json.Unmarshal(rawKeys, &keys); err != nil {
			return nil, fmt.Errorf("read the unique keys of model %s: %w", ref, err)
		}
	}
	return keys, nil
}
