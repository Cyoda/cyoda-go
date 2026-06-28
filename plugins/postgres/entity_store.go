package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"time"

	"github.com/jackc/pgx/v5"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// entityStore implements spi.EntityStore backed by PostgreSQL with
// dual-table writes: entities (current state) + entity_versions (history).
type entityStore struct {
	q        Querier
	tenantID spi.TenantID
	tm       *TransactionManager
}

// SaveAll delegates to Save per-entity via spi.DefaultSaveAll; each Save
// call records in writeSet (for updates) via recordWriteIfInTx. If this
// is ever optimized to a batch INSERT, the writeSet hooks must be preserved.
func (s *entityStore) SaveAll(ctx context.Context, entities iter.Seq[*spi.Entity]) ([]int64, error) {
	return spi.DefaultSaveAll(s, ctx, entities)
}

func (s *entityStore) Save(ctx context.Context, entity *spi.Entity) (int64, error) {
	// Defensive copy — stores own their copies (Ownership Rule 4).
	e := *entity
	if entity.Data != nil {
		e.Data = make([]byte, len(entity.Data))
		copy(e.Data, entity.Data)
	}
	entity = &e

	tid := string(s.tenantID)
	eid := entity.Meta.ID

	entity.Meta.TenantID = s.tenantID

	// Stamp the transaction ID from context so callers can read it back
	// after commit (required by the SPI conformance contract).
	if tx := spi.GetTransaction(ctx); tx != nil && entity.Meta.TransactionID == "" {
		entity.Meta.TransactionID = tx.ID
	}

	// Get DB timestamps first: CURRENT_TIMESTAMP (stable within tx) for
	// valid_time/transaction_time, clock_timestamp() (actual wall clock) for
	// wall_clock_time.
	var dbNow, wallClockTime time.Time
	if err := s.q.QueryRow(ctx, `SELECT CURRENT_TIMESTAMP, clock_timestamp()`).Scan(&dbNow, &wallClockTime); err != nil {
		return 0, fmt.Errorf("failed to get DB timestamps: %w", err)
	}

	// Atomically upsert the entities row, incrementing version in the database
	// without a prior SELECT. The single-statement upsert keeps version
	// allocation inside one tuple-level operation — under REPEATABLE READ,
	// concurrent inserts of distinct entities never contend, and concurrent
	// writers to the same (tenant_id, entity_id) serialise via row locks
	// (the loser sees 40001, classifyError → spi.ErrConflict).
	//
	// We insert a placeholder doc first, then update it below once we know the
	// version. The (xmax = 0) expression is true for newly inserted rows and
	// false for updated rows, letting us distinguish CREATED vs UPDATED.
	var nextVersion int64
	var isNew bool
	err := s.q.QueryRow(ctx,
		`INSERT INTO entities (tenant_id, entity_id, model_name, model_version, version, deleted, doc)
		 VALUES ($1, $2, $3, $4, 1, false, 'null'::jsonb)
		 ON CONFLICT (tenant_id, entity_id) DO UPDATE SET
		   model_name = EXCLUDED.model_name,
		   model_version = EXCLUDED.model_version,
		   version = entities.version + 1,
		   deleted = false,
		   doc = entities.doc
		 RETURNING version, (xmax = 0)`,
		tid, eid,
		entity.Meta.ModelRef.EntityName, entity.Meta.ModelRef.ModelVersion).Scan(&nextVersion, &isNew)
	if err != nil {
		return 0, fmt.Errorf("failed to upsert entity: %w", classifyError(err))
	}

	entity.Meta.Version = nextVersion

	// Record writes only for updates (not fresh inserts). Fresh inserts
	// (isNew=true) are not tracked in writeSet: the UPSERT's ON CONFLICT DO
	// UPDATE means concurrent inserts are gracefully converted to updates by
	// the database — no insert race can produce a false conflict. Tracking
	// fresh inserts would falsely fire because validateInChunks runs inside the
	// current transaction and sees the tx's own uncommitted writes.
	if s.tm != nil && !isNew {
		s.tm.recordWriteIfInTx(ctx, eid, nextVersion-1)
	}

	// Set metadata based on whether this is a new or updated entity.
	if isNew {
		entity.Meta.ChangeType = "CREATED"
		entity.Meta.CreationDate = dbNow
	} else {
		if entity.Meta.ChangeType == "" || entity.Meta.ChangeType == "CREATED" {
			entity.Meta.ChangeType = "UPDATED"
		}
	}
	entity.Meta.LastModifiedDate = dbNow

	// Marshal document with the now-known version.
	doc, err := marshalEntityDoc(entity, dbNow, dbNow, wallClockTime, false)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal entity doc: %w", err)
	}

	// Update the entities row with the final marshaled document.
	_, err = s.q.Exec(ctx,
		`UPDATE entities SET doc = $1 WHERE tenant_id = $2 AND entity_id = $3`,
		doc, tid, eid)
	if err != nil {
		return 0, fmt.Errorf("failed to update entity doc: %w", err)
	}

	// Insert version row (explicit wall_clock_time to match _meta value).
	_, err = s.q.Exec(ctx,
		`INSERT INTO entity_versions (tenant_id, entity_id, model_name, model_version, version, valid_time, wall_clock_time, doc)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		tid, eid,
		entity.Meta.ModelRef.EntityName, entity.Meta.ModelRef.ModelVersion,
		nextVersion, dbNow, wallClockTime, doc)
	if err != nil {
		return 0, fmt.Errorf("failed to insert entity version: %w", err)
	}

	return nextVersion, nil
}

func (s *entityStore) CompareAndSave(ctx context.Context, entity *spi.Entity, expectedTxID string) (int64, error) {
	tid := string(s.tenantID)
	eid := entity.Meta.ID

	// Check current transaction ID.
	var currentTxID *string
	err := s.q.QueryRow(ctx,
		`SELECT doc->'_meta'->>'transaction_id' FROM entities WHERE tenant_id = $1 AND entity_id = $2`,
		tid, eid).Scan(&currentTxID)
	if err != nil && err != pgx.ErrNoRows {
		return 0, fmt.Errorf("failed to check transaction ID: %w", err)
	}

	// If entity exists and txID doesn't match, conflict.
	if err == nil && currentTxID != nil && *currentTxID != expectedTxID {
		return 0, fmt.Errorf("entity %s transaction ID mismatch (current=%q, expected=%q): %w",
			eid, *currentTxID, expectedTxID, spi.ErrConflict)
	}

	return s.Save(ctx, entity)
}

func (s *entityStore) Get(ctx context.Context, entityID string) (*spi.Entity, error) {
	var doc []byte
	err := s.q.QueryRow(ctx,
		`SELECT doc FROM entities WHERE tenant_id = $1 AND entity_id = $2 AND NOT deleted`,
		string(s.tenantID), entityID).Scan(&doc)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("ENTITY_NOT_FOUND: entity %s not found: %w", entityID, spi.ErrNotFound)
		}
		return nil, fmt.Errorf("failed to get entity %s: %w", entityID, err)
	}
	entity, err := unmarshalEntityDoc(doc)
	if err != nil {
		return nil, err
	}
	if s.tm != nil {
		s.tm.recordReadIfInTx(ctx, entity.Meta.ID, entity.Meta.Version)
	}
	return entity, nil
}

// Deliberately not tracked in readSet: historical reads target immutable versions. See spec §Known limitation.
func (s *entityStore) GetAsAt(ctx context.Context, entityID string, asAt time.Time) (*spi.Entity, error) {
	var doc []byte
	err := s.q.QueryRow(ctx,
		`SELECT doc FROM entity_versions
		 WHERE tenant_id = $1 AND entity_id = $2
		   AND valid_time <= $3
		   AND transaction_time <= CURRENT_TIMESTAMP
		 ORDER BY valid_time DESC, transaction_time DESC
		 LIMIT 1`,
		string(s.tenantID), entityID, asAt).Scan(&doc)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("ENTITY_NOT_FOUND: entity %s not found at %v: %w", entityID, asAt, spi.ErrNotFound)
		}
		return nil, fmt.Errorf("failed to get entity %s as-at %v: %w", entityID, asAt, err)
	}

	// Check if the version is deleted.
	var docMap map[string]json.RawMessage
	if err := json.Unmarshal(doc, &docMap); err != nil {
		return nil, fmt.Errorf("failed to parse entity doc: %w", err)
	}
	if metaRaw, ok := docMap["_meta"]; ok {
		var meta entityMeta
		if err := json.Unmarshal(metaRaw, &meta); err != nil {
			return nil, fmt.Errorf("failed to parse _meta: %w", err)
		}
		if meta.Deleted {
			return nil, fmt.Errorf("ENTITY_NOT_FOUND: entity %s deleted at %v: %w", entityID, asAt, spi.ErrNotFound)
		}
	}

	return unmarshalEntityDoc(doc)
}

func (s *entityStore) GetAll(ctx context.Context, modelRef spi.ModelRef) ([]*spi.Entity, error) {
	rows, err := s.q.Query(ctx,
		`SELECT doc FROM entities WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to query entities: %w", err)
	}
	defer rows.Close()

	entities, err := scanEntities(rows)
	if err != nil {
		return nil, err
	}
	if s.tm != nil {
		for _, e := range entities {
			s.tm.recordReadIfInTx(ctx, e.Meta.ID, e.Meta.Version)
		}
	}
	return entities, nil
}

// Deliberately not tracked in readSet: historical reads target immutable versions. See spec §Known limitation.
func (s *entityStore) GetAllAsAt(ctx context.Context, modelRef spi.ModelRef, asAt time.Time) ([]*spi.Entity, error) {
	rows, err := s.q.Query(ctx,
		`SELECT v.doc
		 FROM entities e
		 CROSS JOIN LATERAL (
		     SELECT doc FROM entity_versions ev
		     WHERE ev.tenant_id = e.tenant_id AND ev.entity_id = e.entity_id
		       AND ev.valid_time <= $4
		       AND ev.transaction_time <= CURRENT_TIMESTAMP
		     ORDER BY ev.valid_time DESC, ev.transaction_time DESC
		     LIMIT 1
		 ) v
		 WHERE e.tenant_id = $1 AND e.model_name = $2 AND e.model_version = $3`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion, asAt)
	if err != nil {
		return nil, fmt.Errorf("failed to query entities as-at: %w", err)
	}
	defer rows.Close()

	return scanEntitiesFilterDeleted(rows)
}

func (s *entityStore) Delete(ctx context.Context, entityID string) error {
	tid := string(s.tenantID)

	// Get current entity (doc + version) in a single point-lookup on the PK.
	// Fetching both avoids a second round-trip for the version.
	var doc []byte
	var maxVersion int64
	err := s.q.QueryRow(ctx,
		`SELECT doc, version FROM entities WHERE tenant_id = $1 AND entity_id = $2 AND NOT deleted`,
		tid, entityID).Scan(&doc, &maxVersion)
	if err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("ENTITY_NOT_FOUND: entity %s not found: %w", entityID, spi.ErrNotFound)
		}
		return fmt.Errorf("failed to get entity %s for delete: %w", entityID, err)
	}

	if s.tm != nil {
		s.tm.recordWriteIfInTx(ctx, entityID, maxVersion)
	}

	current, err := unmarshalEntityDoc(doc)
	if err != nil {
		return fmt.Errorf("failed to unmarshal entity for delete: %w", err)
	}

	nextVersion := maxVersion + 1

	// Get DB timestamp.
	var dbNow, wallClockTime time.Time
	if err := s.q.QueryRow(ctx, `SELECT CURRENT_TIMESTAMP, clock_timestamp()`).Scan(&dbNow, &wallClockTime); err != nil {
		return fmt.Errorf("failed to get DB timestamps: %w", err)
	}

	// Prepare delete entity.
	current.Meta.Version = nextVersion
	current.Meta.ChangeType = "DELETED"
	current.Meta.LastModifiedDate = dbNow

	deleteDoc, err := marshalEntityDoc(current, dbNow, dbNow, wallClockTime, true)
	if err != nil {
		return fmt.Errorf("failed to marshal delete doc: %w", err)
	}

	// Insert delete version.
	_, err = s.q.Exec(ctx,
		`INSERT INTO entity_versions (tenant_id, entity_id, model_name, model_version, version, valid_time, wall_clock_time, doc)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		tid, entityID,
		current.Meta.ModelRef.EntityName, current.Meta.ModelRef.ModelVersion,
		nextVersion, dbNow, wallClockTime, deleteDoc)
	if err != nil {
		return fmt.Errorf("failed to insert delete version: %w", err)
	}

	// Update entities table to mark deleted.
	_, err = s.q.Exec(ctx,
		`UPDATE entities SET version = $1, deleted = true, doc = $2 WHERE tenant_id = $3 AND entity_id = $4`,
		nextVersion, deleteDoc, tid, entityID)
	if err != nil {
		return fmt.Errorf("failed to mark entity deleted: %w", err)
	}

	return nil
}

func (s *entityStore) DeleteAll(ctx context.Context, modelRef spi.ModelRef) error {
	tid := string(s.tenantID)

	// Get all entity IDs for this model.
	rows, err := s.q.Query(ctx,
		`SELECT entity_id FROM entities WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted`,
		tid, modelRef.EntityName, modelRef.ModelVersion)
	if err != nil {
		return fmt.Errorf("failed to query entities for deleteAll: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("failed to scan entity ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("row iteration error: %w", err)
	}

	for _, id := range ids {
		if err := s.Delete(ctx, id); err != nil {
			return fmt.Errorf("failed to delete entity %s: %w", id, err)
		}
	}

	return nil
}

// Deliberately not tracked in readSet: boolean probe; no version to validate.
func (s *entityStore) Exists(ctx context.Context, entityID string) (bool, error) {
	var exists bool
	err := s.q.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM entities WHERE tenant_id = $1 AND entity_id = $2 AND NOT deleted)`,
		string(s.tenantID), entityID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check existence of entity %s: %w", entityID, err)
	}
	return exists, nil
}

// Deliberately not tracked in readSet: aggregate with no per-row identity. See spec §Known limitation (phantom reads).
func (s *entityStore) Count(ctx context.Context, modelRef spi.ModelRef) (int64, error) {
	var count int64
	err := s.q.QueryRow(ctx,
		`SELECT count(*) FROM entities WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count entities: %w", err)
	}
	return count, nil
}

// CountByState returns counts of non-deleted entities grouped by state for the
// given model. See SPI godoc on EntityStore.CountByState for filter semantics.
//
// State is stored inside the doc JSONB at $._meta.state. An indexed expression
// (e.g. CREATE INDEX ON entities ((doc->'_meta'->>'state')) WHERE NOT deleted)
// is a future optimization (out of scope for this issue).
//
// Deliberately not tracked in readSet: aggregate with no per-row identity. See
// Count's note on phantom reads.
func (s *entityStore) CountByState(ctx context.Context, modelRef spi.ModelRef, states []string) (map[string]int64, error) {
	if states != nil && len(states) == 0 {
		return map[string]int64{}, nil
	}

	args := []any{string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion}
	// Entities with no $._meta.state are bucketed under "" rather than dropped,
	// preserving them for diagnostic visibility. This matches the in-tx Go path
	// which reads e.Meta.State (also "" if unset).
	q := `SELECT COALESCE(doc -> '_meta' ->> 'state', '') AS state, COUNT(*)
	      FROM entities
	      WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted`

	if states != nil {
		// pgx encodes []string as text[] for the ANY() comparison; no manual casting needed.
		args = append(args, states)
		q += ` AND doc -> '_meta' ->> 'state' = ANY($4)`
	}
	q += ` GROUP BY state`

	rows, err := s.q.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to count entities by state: %w", err)
	}
	defer rows.Close()

	result := make(map[string]int64)
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, fmt.Errorf("failed to scan count by state row: %w", err)
		}
		result[st] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate count by state rows: %w", err)
	}
	return result, nil
}

// Deliberately not tracked in readSet: observational reads of version history.
func (s *entityStore) GetVersionHistory(ctx context.Context, entityID string) ([]spi.EntityVersion, error) {
	rows, err := s.q.Query(ctx,
		`SELECT doc, version, valid_time FROM entity_versions
		 WHERE tenant_id = $1 AND entity_id = $2
		 ORDER BY version ASC`,
		string(s.tenantID), entityID)
	if err != nil {
		return nil, fmt.Errorf("failed to query version history: %w", err)
	}
	defer rows.Close()

	var history []spi.EntityVersion
	for rows.Next() {
		var doc []byte
		var version int64
		var validTime time.Time
		if err := rows.Scan(&doc, &version, &validTime); err != nil {
			return nil, fmt.Errorf("failed to scan version row: %w", err)
		}
		ver, err := unmarshalEntityVersion(doc, version, validTime)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal version %d: %w", version, err)
		}
		history = append(history, *ver)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}

	if len(history) == 0 {
		return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}

	return history, nil
}

// scanEntities reads all Entity rows from a result set.
func scanEntities(rows pgx.Rows) ([]*spi.Entity, error) {
	var result []*spi.Entity
	for rows.Next() {
		var doc []byte
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("failed to scan entity row: %w", err)
		}
		ent, err := unmarshalEntityDoc(doc)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal entity: %w", err)
		}
		result = append(result, ent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}
	if result == nil {
		result = []*spi.Entity{}
	}
	return result, nil
}

// scanEntitiesFilterDeleted reads Entity rows and filters out deleted ones.
func scanEntitiesFilterDeleted(rows pgx.Rows) ([]*spi.Entity, error) {
	var result []*spi.Entity
	for rows.Next() {
		var doc []byte
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("failed to scan entity row: %w", err)
		}

		// Check deleted flag in _meta.
		var docMap map[string]json.RawMessage
		if err := json.Unmarshal(doc, &docMap); err != nil {
			return nil, fmt.Errorf("failed to parse entity doc: %w", err)
		}
		if metaRaw, ok := docMap["_meta"]; ok {
			var meta entityMeta
			if err := json.Unmarshal(metaRaw, &meta); err != nil {
				return nil, fmt.Errorf("failed to parse _meta: %w", err)
			}
			if meta.Deleted {
				continue
			}
		}

		ent, err := unmarshalEntityDoc(doc)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal entity: %w", err)
		}
		result = append(result, ent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}
	if result == nil {
		result = []*spi.Entity{}
	}
	return result, nil
}
