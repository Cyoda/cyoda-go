package postgres

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	spi "github.com/cyoda/cyoda-go-spi"
)

// Unique keys are checked when a write commits, from the transaction's final
// entities and the unique-key definitions of their models as stored:
//
//  1. Release the claims of every entity written (releaseClaims).
//  2. Read the unique-key definitions (uniqueKeys) of the model of every
//     entity whose last write is a save, in one statement, from the models
//     rows (modelKeys). The model's state is not checked (the API, not the
//     store, keeps the model rules), and a model the read does not
//     find contributes no unique-key definitions. An entity whose last
//     write is a delete needs no model.
//  3. Compute the claims of every entity whose last write is a save, from the
//     bytes that save was given and its model's unique-key definitions. A
//     partial composite key fails with spi.ErrPartialUniqueKey (insertClaims).
//  4. Insert those claims in (model, key id, signature) order. The immediate
//     index unique_claims_uq refuses a value claimed twice or held by an
//     entity outside the writes: 23505, spi.ErrUniqueViolation once
//     classified (insertClaims).
//
// Step 1 is the first statement of this check (the first Commit issues when
// the read set is empty), so a transaction an earlier statement aborted meets
// its 25P02 there.
//
// Step 2 takes no lock and reads on the same transaction as the other steps.
// The transactional commit reads in its REPEATABLE READ snapshot, so it sees
// the models as they were when the transaction began. That is correct under
// the caller's rule: a model's state and its unique-key definitions change
// only while no transaction that will write entities of that model is open
// (spi.UniqueKey). If that rule is broken, the commit may check the unique-key
// definitions as they were when the transaction began. A read on the model
// pool, which sees the latest model, would buy nothing under that rule, and
// would make every commit with a save wait for a second connection, from the
// model pool, while it holds a main-pool one: a busy model pool would then
// fail commits as unavailable, and commits would starve the model reads that
// pool is kept apart for. A write outside a transaction reads on its own READ
// COMMITTED transaction, which sees the latest committed model
// (replaceClaims).
//
// The transactional commit (TransactionManager.commitWrittenClaims) and a
// save outside a transaction (replaceClaims) run the same steps in the same
// order. Every step runs before the commit stamp: nothing after cyoda_stamp
// may wait on a lock (stampCommitInstant).

// releaseClaims is step 1: it removes every unique-key claim held by the
// entities ids, on q. At Commit it runs inside the caller's transaction, for
// every entity the transaction wrote; for a write outside a transaction it
// runs in the transaction that write opened for itself.
func releaseClaims(ctx context.Context, q Querier, tenantID spi.TenantID, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := q.Exec(ctx,
		`DELETE FROM unique_claims WHERE tenant_id = $1 AND entity_id = ANY($2::text[])`,
		string(tenantID), ids); err != nil {
		return fmt.Errorf("failed to release unique claims: %w", err)
	}
	return nil
}

// insertClaims is steps 3-4 on q, with keys, the unique-key definitions step 2
// read (modelKeys) for the models of the entities whose last write is a save
// (savedModels). written is the record of the entities written
// (txState.WrittenEntities): the model and bytes of each entity's last save,
// or a delete. Both save paths store the entity under the model the save names
// (the upsert's model guard, replacePendingSave's spi.ErrEntityModelMismatch),
// so the recorded model is the stored one.
func insertClaims(ctx context.Context, q Querier, tenantID spi.TenantID, written map[string]writtenEntity, keys map[spi.ModelRef][]spi.UniqueKey) error {
	// Step 3: a partial key fails the whole commit.
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
	if len(claims) == 0 {
		return nil
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

	// Step 4.
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
	// order above: two commits that only insert overlapping values meet in
	// the same order and wait instead of deadlocking. A commit that claims a
	// value another commit frees (releaseClaims) can still deadlock with it;
	// PostgreSQL then fails one with 40P01, a retryable conflict.
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

// writtenIDs returns the ids of the entities written, sorted.
func writtenIDs(written map[string]writtenEntity) []string {
	ids := make([]string, 0, len(written))
	for id := range written {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
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

// modelKeys is step 2: one statement reads the unique-key definitions of
// every model in models from its models row. A model it does not find
// contributes no unique-key definitions.
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
		return nil, fmt.Errorf("read the unique-key definitions of the models: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref spi.ModelRef
		var rawKeys []byte
		if err := rows.Scan(&ref.EntityName, &ref.ModelVersion, &rawKeys); err != nil {
			return nil, fmt.Errorf("read the unique-key definitions of the models: %w", err)
		}
		var k []spi.UniqueKey
		if len(rawKeys) > 0 {
			if err := json.Unmarshal(rawKeys, &k); err != nil {
				return nil, fmt.Errorf("read the unique-key definitions of model %s: %w", ref, err)
			}
		}
		keys[ref] = k
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the unique-key definitions of the models: %w", err)
	}
	return keys, nil
}
