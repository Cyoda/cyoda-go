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
//     whose last write is a save, FOR SHARE, in (name, version) order; then
//     read each model's state and unique keys from the models row. A model
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
// written is the transaction's bytes record (txState.WrittenEntities): the
// bytes of each written entity's last save, or nil for a delete. refs holds
// the model of every entity whose last write is a save (writtenModelRefs).
//
// The transaction is REPEATABLE READ, and its snapshot is older than the
// commit. The FOR SHARE makes the snapshot's model the current one: a
// lifecycle row bumped and committed after the snapshot fails it with 40001
// (a retryable spi.ErrConflict once classified), and an uncommitted bump
// makes it wait for the outcome. So the state and keys read from the
// snapshot's models row are the ones in force, and stay so until the commit
// ends.
//
// A lifecycle row the snapshot does not see is either a model that does not
// exist or one created after the snapshot. The probe insert tells them apart:
// a row it inserts is a missing model (spi.ErrModelNotLocked; the
// transaction rolls back and takes the row with it), and a conflict with a
// row the snapshot cannot see fails it with 40001.
func commitClaims(ctx context.Context, q Querier, tenantID spi.TenantID, written map[string][]byte, refs map[string]spi.ModelRef) error {
	return applyClaims(ctx, q, tenantID, written, refs, true)
}

// writtenModelRefs reads the model of every entity whose last write in the
// transaction is a save, from the transaction's own entities rows. An
// entity's model is fixed at creation, so the row's model is the one the
// save wrote.
func writtenModelRefs(ctx context.Context, q Querier, tenantID spi.TenantID, written map[string][]byte) (map[string]spi.ModelRef, error) {
	ids := make([]string, 0, len(written))
	for id, data := range written {
		if data != nil {
			ids = append(ids, id)
		}
	}
	refs := make(map[string]spi.ModelRef, len(ids))
	if len(ids) == 0 {
		return refs, nil
	}
	rows, err := q.Query(ctx,
		`SELECT entity_id, model_name, model_version FROM entities
		  WHERE tenant_id = $1 AND entity_id = ANY($2::text[])`,
		string(tenantID), ids)
	if err != nil {
		return nil, fmt.Errorf("read the models of the written entities: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var ref spi.ModelRef
		if err := rows.Scan(&id, &ref.EntityName, &ref.ModelVersion); err != nil {
			return nil, fmt.Errorf("read the models of the written entities: %w", err)
		}
		refs[id] = ref
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the models of the written entities: %w", err)
	}
	if len(refs) != len(ids) {
		return nil, fmt.Errorf("read the models of the written entities: %d of %d entity rows found", len(refs), len(ids))
	}
	return refs, nil
}

// applyClaims is steps 1-4 on q. inSnapshot is true inside a caller's
// REPEATABLE READ transaction (commitClaims), where a lifecycle row the
// snapshot does not see is probed. It is false for a write outside a
// transaction (replaceClaims), which runs READ COMMITTED: there every
// statement takes a fresh snapshot, so a missing lifecycle row is a missing
// model, and the models reads, statements after the FOR SHARE, see the commit
// of any bump the FOR SHARE waited for.
func applyClaims(ctx context.Context, q Querier, tenantID spi.TenantID, written map[string][]byte, refs map[string]spi.ModelRef, inSnapshot bool) error {
	if len(written) == 0 {
		return nil
	}

	// Step 1.
	models := distinctRefs(refs)
	for _, ref := range models {
		if err := shareLifecycle(ctx, q, tenantID, ref, inSnapshot); err != nil {
			return err
		}
	}
	keys := make(map[spi.ModelRef][]spi.UniqueKey, len(models))
	for _, ref := range models {
		k, err := lockedModelKeys(ctx, q, tenantID, ref)
		if err != nil {
			return err
		}
		keys[ref] = k
	}

	// Step 3, before any write: a partial key fails the whole commit.
	var claims []claimRow
	for id, data := range written {
		if data == nil {
			continue
		}
		ref := refs[id]
		k := keys[ref]
		if len(k) == 0 {
			continue
		}
		cs, err := spi.ComputeClaims(k, data)
		if err != nil {
			return fmt.Errorf("entity %s: %w", id, err)
		}
		for _, c := range cs {
			claims = append(claims, claimRow{ref: ref, keyID: c.KeyID, signature: c.Signature, entityID: id})
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

// distinctRefs returns the distinct models of refs, by name, then version.
func distinctRefs(refs map[string]spi.ModelRef) []spi.ModelRef {
	seen := make(map[spi.ModelRef]struct{}, len(refs))
	out := make([]spi.ModelRef, 0, len(refs))
	for _, ref := range refs {
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	slices.SortFunc(out, func(a, b spi.ModelRef) int {
		return cmp.Or(cmp.Compare(a.EntityName, b.EntityName), cmp.Compare(a.ModelVersion, b.ModelVersion))
	})
	return out
}

// shareLifecycle takes FOR SHARE on ref's lifecycle row, as a statement of
// its own. With no row, a write outside a transaction is refused at once; in
// a snapshot the probe insert decides (see commitClaims).
func shareLifecycle(ctx context.Context, q Querier, tenantID spi.TenantID, ref spi.ModelRef, inSnapshot bool) error {
	var one int
	err := q.QueryRow(ctx,
		`SELECT 1 FROM model_lifecycle
		  WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3
		    FOR SHARE`,
		string(tenantID), ref.EntityName, ref.ModelVersion).Scan(&one)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lock the lifecycle of model %s: %w", ref, err)
	}
	if !inSnapshot {
		return fmt.Errorf("model %s does not exist: %w", ref, spi.ErrModelNotLocked)
	}
	err = q.QueryRow(ctx,
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
	// above just did not find; it is an error like any other.
	return fmt.Errorf("probe the lifecycle of model %s: %w", ref, err)
}

// lockedModelKeys reads ref's state and unique keys from its models row and
// returns the keys, refusing a model that does not exist or is not LOCKED
// with spi.ErrModelNotLocked.
func lockedModelKeys(ctx context.Context, q Querier, tenantID spi.TenantID, ref spi.ModelRef) ([]spi.UniqueKey, error) {
	var state string
	var rawKeys []byte
	err := q.QueryRow(ctx,
		`SELECT doc->>'state', doc->'uniqueKeys' FROM models
		  WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(tenantID), ref.EntityName, ref.ModelVersion).Scan(&state, &rawKeys)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("model %s does not exist: %w", ref, spi.ErrModelNotLocked)
	}
	if err != nil {
		return nil, fmt.Errorf("read model %s: %w", ref, err)
	}
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
