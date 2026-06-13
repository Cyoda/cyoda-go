package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// entityStore implements spi.EntityStore backed by SQLite.
// Transactional reads use snapshot isolation against entity_versions;
// non-transactional reads query the entities table (current state).
type entityStore struct {
	db       *sql.DB
	tenantID spi.TenantID
	tm       *transactionManager
	clock    Clock
	cfg      config
}

// Verify interface compliance at compile time.
var _ spi.EntityStore = (*entityStore)(nil)

// microToTime converts Unix microseconds to time.Time.
func microToTime(us int64) time.Time {
	return time.UnixMicro(us)
}

// timeToMicro converts time.Time to Unix microseconds.
func timeToMicro(t time.Time) int64 {
	return t.UnixMicro()
}

// entityMetaDB is the serialized form of EntityMeta stored in the meta BLOB column.
type entityMetaDB struct {
	ID                      string `json:"id"`
	TenantID                string `json:"tenant_id"`
	ModelName               string `json:"model_name"`
	ModelVersion            string `json:"model_version"`
	State                   string `json:"state,omitempty"`
	Version                 int64  `json:"version"`
	CreationDate            int64  `json:"creation_date"`
	LastModifiedDate        int64  `json:"last_modified_date"`
	TransactionID           string `json:"transaction_id,omitempty"`
	ChangeType              string `json:"change_type,omitempty"`
	ChangeUser              string `json:"change_user,omitempty"`
	TransitionForLatestSave string `json:"transition_for_latest_save,omitempty"`
}

func marshalEntityMeta(m *spi.EntityMeta) ([]byte, error) {
	db := entityMetaDB{
		ID:                      m.ID,
		TenantID:                string(m.TenantID),
		ModelName:               m.ModelRef.EntityName,
		ModelVersion:            m.ModelRef.ModelVersion,
		State:                   m.State,
		Version:                 m.Version,
		CreationDate:            timeToMicro(m.CreationDate),
		LastModifiedDate:        timeToMicro(m.LastModifiedDate),
		TransactionID:           m.TransactionID,
		ChangeType:              m.ChangeType,
		ChangeUser:              m.ChangeUser,
		TransitionForLatestSave: m.TransitionForLatestSave,
	}
	return json.Marshal(db)
}

func unmarshalEntityMeta(data []byte) (spi.EntityMeta, error) {
	var db entityMetaDB
	if err := json.Unmarshal(data, &db); err != nil {
		return spi.EntityMeta{}, fmt.Errorf("unmarshal entity meta: %w", err)
	}
	return spi.EntityMeta{
		ID:                      db.ID,
		TenantID:                spi.TenantID(db.TenantID),
		ModelRef:                spi.ModelRef{EntityName: db.ModelName, ModelVersion: db.ModelVersion},
		State:                   db.State,
		Version:                 db.Version,
		CreationDate:            microToTime(db.CreationDate),
		LastModifiedDate:        microToTime(db.LastModifiedDate),
		TransactionID:           db.TransactionID,
		ChangeType:              db.ChangeType,
		ChangeUser:              db.ChangeUser,
		TransitionForLatestSave: db.TransitionForLatestSave,
	}, nil
}

// copyEntity creates a deep copy of an entity (ownership rule 4).
func copyEntity(e *spi.Entity) *spi.Entity {
	cp := &spi.Entity{Meta: e.Meta, Data: make([]byte, len(e.Data))}
	copy(cp.Data, e.Data)
	return cp
}

// scanEntityFromRow scans a single entity from a query result row.
// Expected columns: entity_id, model_name, model_version, version, json(data), json(meta), created_at, updated_at
func scanEntityFromRow(row interface{ Scan(...any) error }) (*spi.Entity, error) {
	var (
		entityID, modelName, modelVersion string
		version                           int64
		dataJSON                          []byte
		metaJSON                          sql.NullString
		createdAt, updatedAt              int64
	)
	if err := row.Scan(&entityID, &modelName, &modelVersion, &version,
		&dataJSON, &metaJSON, &createdAt, &updatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, spi.ErrNotFound
		}
		return nil, fmt.Errorf("scan entity: %w", err)
	}

	meta := spi.EntityMeta{
		ID:               entityID,
		ModelRef:         spi.ModelRef{EntityName: modelName, ModelVersion: modelVersion},
		Version:          version,
		CreationDate:     microToTime(createdAt),
		LastModifiedDate: microToTime(updatedAt),
	}

	// Populate additional meta from the stored meta BLOB.
	if metaJSON.Valid && metaJSON.String != "" {
		parsed, err := unmarshalEntityMeta([]byte(metaJSON.String))
		if err == nil {
			meta.TenantID = parsed.TenantID
			meta.State = parsed.State
			meta.TransactionID = parsed.TransactionID
			meta.ChangeType = parsed.ChangeType
			meta.ChangeUser = parsed.ChangeUser
			meta.TransitionForLatestSave = parsed.TransitionForLatestSave
		}
	}

	return &spi.Entity{Meta: meta, Data: dataJSON}, nil
}

// scanVersionEntity scans a versioned entity from entity_versions.
// Expected columns: entity_id, model_name, model_version, version, json(data), json(meta), submit_time
func scanVersionEntity(row interface{ Scan(...any) error }) (*spi.Entity, error) {
	var (
		entityID, modelName, modelVersion string
		version                           int64
		dataJSON                          sql.NullString
		metaJSON                          sql.NullString
		submitTimeMicro                   int64
	)
	if err := row.Scan(&entityID, &modelName, &modelVersion, &version,
		&dataJSON, &metaJSON, &submitTimeMicro); err != nil {
		if err == sql.ErrNoRows {
			return nil, spi.ErrNotFound
		}
		return nil, fmt.Errorf("scan version entity: %w", err)
	}

	meta := spi.EntityMeta{
		ID:               entityID,
		ModelRef:         spi.ModelRef{EntityName: modelName, ModelVersion: modelVersion},
		Version:          version,
		CreationDate:     microToTime(submitTimeMicro),
		LastModifiedDate: microToTime(submitTimeMicro),
	}

	// Populate additional meta from the stored meta BLOB.
	if metaJSON.Valid && metaJSON.String != "" {
		parsed, err := unmarshalEntityMeta([]byte(metaJSON.String))
		if err == nil {
			meta.TenantID = parsed.TenantID
			meta.State = parsed.State
			meta.TransactionID = parsed.TransactionID
			meta.ChangeType = parsed.ChangeType
			meta.ChangeUser = parsed.ChangeUser
			meta.TransitionForLatestSave = parsed.TransitionForLatestSave
			meta.CreationDate = parsed.CreationDate
			meta.LastModifiedDate = parsed.LastModifiedDate
		}
	}

	var data []byte
	if dataJSON.Valid {
		data = []byte(dataJSON.String)
	}

	return &spi.Entity{Meta: meta, Data: data}, nil
}

func (s *entityStore) SaveAll(ctx context.Context, entities iter.Seq[*spi.Entity]) ([]int64, error) {
	return spi.DefaultSaveAll(s, ctx, entities)
}

func (s *entityStore) Save(ctx context.Context, entity *spi.Entity) (int64, error) {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return 0, fmt.Errorf("Save: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		// Transaction mode: write to buffer, not main store.
		cp := copyEntity(entity)
		cp.Meta.TenantID = s.tenantID
		tx.Buffer[entity.Meta.ID] = cp
		tx.WriteSet[entity.Meta.ID] = true
		// If the entity was previously marked for deletion in this tx, unmark it.
		delete(tx.Deletes, entity.Meta.ID)
		return 0, nil // actual version assigned at commit
	}

	// Non-transaction mode: direct write (implicit auto-commit).
	return s.saveDirectly(ctx, entity)
}

func (s *entityStore) saveDirectly(ctx context.Context, entity *spi.Entity) (int64, error) {
	cp := copyEntity(entity)
	cp.Meta.TenantID = s.tenantID
	now := s.clock.Now()
	tid := string(s.tenantID)

	var existingVersion sql.NullInt64
	var existingCreatedAt sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		"SELECT version, created_at FROM entities WHERE tenant_id = ? AND entity_id = ?",
		tid, cp.Meta.ID).Scan(&existingVersion, &existingCreatedAt)
	isNew := err == sql.ErrNoRows
	if err != nil && !isNew {
		return 0, fmt.Errorf("check existing entity: %w", err)
	}

	var nextVersion int64
	changeType := "CREATED"
	createdAtMicro := timeToMicro(now)
	if !isNew {
		nextVersion = existingVersion.Int64 + 1
		changeType = "UPDATED"
		createdAtMicro = existingCreatedAt.Int64
	}

	cp.Meta.Version = nextVersion
	cp.Meta.LastModifiedDate = now
	cp.Meta.ChangeType = changeType
	if isNew {
		cp.Meta.CreationDate = now
	} else {
		cp.Meta.CreationDate = microToTime(createdAtMicro)
	}

	metaJSON, err := marshalEntityMeta(&cp.Meta)
	if err != nil {
		return 0, fmt.Errorf("marshal meta: %w", err)
	}

	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer sqlTx.Rollback()

	_, err = sqlTx.ExecContext(ctx,
		`INSERT OR REPLACE INTO entities
		 (tenant_id, entity_id, model_name, model_version, version, data, meta, deleted, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, jsonb(?), jsonb(?), 0, ?, ?)`,
		tid, cp.Meta.ID, cp.Meta.ModelRef.EntityName, cp.Meta.ModelRef.ModelVersion,
		nextVersion, string(cp.Data), string(metaJSON),
		createdAtMicro, timeToMicro(now))
	if err != nil {
		return 0, fmt.Errorf("upsert entity: %w", classifyError(err))
	}

	_, err = sqlTx.ExecContext(ctx,
		`INSERT INTO entity_versions
		 (tenant_id, entity_id, model_name, model_version, version, data, meta, change_type, transaction_id, submit_time, user_id)
		 VALUES (?, ?, ?, ?, ?, jsonb(?), jsonb(?), ?, '', ?, '')`,
		tid, cp.Meta.ID, cp.Meta.ModelRef.EntityName, cp.Meta.ModelRef.ModelVersion,
		nextVersion, string(cp.Data), string(metaJSON), changeType, timeToMicro(now))
	if err != nil {
		return 0, fmt.Errorf("insert version: %w", classifyError(err))
	}

	if err := sqlTx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", classifyError(err))
	}
	return nextVersion, nil
}

func (s *entityStore) CompareAndSave(ctx context.Context, entity *spi.Entity, expectedTxID string) (int64, error) {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return 0, fmt.Errorf("CompareAndSave: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		// Check CAS against committed store (not buffer).
		var currentTxID sql.NullString
		err := s.db.QueryRowContext(ctx,
			"SELECT json_extract(json(meta), '$.transaction_id') FROM entities WHERE tenant_id = ? AND entity_id = ? AND NOT deleted",
			string(s.tenantID), entity.Meta.ID).Scan(&currentTxID)
		if err != nil && err != sql.ErrNoRows {
			return 0, fmt.Errorf("check transaction ID: %w", classifyError(err))
		}
		if err == nil && currentTxID.Valid && currentTxID.String != expectedTxID {
			return 0, spi.ErrConflict
		}

		// Write to buffer.
		cp := copyEntity(entity)
		cp.Meta.TenantID = s.tenantID
		tx.Buffer[entity.Meta.ID] = cp
		tx.WriteSet[entity.Meta.ID] = true
		return 0, nil
	}

	// Non-transaction: check CAS then save.
	var currentTxID sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT json_extract(json(meta), '$.transaction_id') FROM entities WHERE tenant_id = ? AND entity_id = ? AND NOT deleted",
		string(s.tenantID), entity.Meta.ID).Scan(&currentTxID)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("check transaction ID: %w", classifyError(err))
	}
	if err == nil && currentTxID.Valid && currentTxID.String != expectedTxID {
		return 0, spi.ErrConflict
	}

	return s.saveDirectly(ctx, entity)
}

func (s *entityStore) Get(ctx context.Context, entityID string) (*spi.Entity, error) {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return nil, fmt.Errorf("Get: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		// Check if deleted in this transaction.
		if tx.Deletes[entityID] {
			return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
		}
		// Check buffer first (read-your-own-writes).
		if buffered, ok := tx.Buffer[entityID]; ok {
			tx.ReadSet[entityID] = true
			return copyEntity(buffered), nil
		}
		// Fall back to snapshot read from entity_versions.
		e, err := s.getSnapshot(ctx, entityID, tx.SnapshotTime)
		if err != nil {
			return nil, err
		}
		tx.ReadSet[entityID] = true
		return e, nil
	}

	// Non-transaction: latest committed state.
	return s.getDirect(ctx, entityID)
}

func (s *entityStore) getDirect(ctx context.Context, entityID string) (*spi.Entity, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT entity_id, model_name, model_version, version,
		        json(data), json(meta), created_at, updated_at
		 FROM entities
		 WHERE tenant_id = ? AND entity_id = ? AND NOT deleted`,
		string(s.tenantID), entityID)
	e, err := scanEntityFromRow(row)
	if err != nil {
		if err == spi.ErrNotFound {
			return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
		}
		return nil, classifyError(err)
	}
	return e, nil
}

// getSnapshot reads the entity from entity_versions at the given snapshot time.
//
// Snapshot-time convention: submit_time <= snapshotTime (non-strict).
// This matches the memory plugin's !v.submitTime.After(snapshotTime) and is
// used consistently across getSnapshot, getAllTx, DeleteAll tx, and
// searchPointInTimeBase. The separate GetAsAt/GetAllAsAt queries use strict <
// because they first round asAt up to the next millisecond boundary.
func (s *entityStore) getSnapshot(ctx context.Context, entityID string, snapshotTime time.Time) (*spi.Entity, error) {
	snapshotMicro := timeToMicro(snapshotTime)
	row := s.db.QueryRowContext(ctx,
		`SELECT ev.entity_id, ev.model_name, ev.model_version, ev.version,
		        json(ev.data), json(ev.meta), ev.submit_time
		 FROM entity_versions ev
		 INNER JOIN (
		     SELECT entity_id, MAX(version) AS max_ver
		     FROM entity_versions
		     WHERE tenant_id = ? AND entity_id = ? AND submit_time <= ?
		     GROUP BY entity_id
		 ) latest ON ev.entity_id = latest.entity_id AND ev.version = latest.max_ver
		 WHERE ev.tenant_id = ? AND ev.change_type != 'DELETED'`,
		string(s.tenantID), entityID, snapshotMicro,
		string(s.tenantID))
	e, err := scanVersionEntity(row)
	if err != nil {
		if err == spi.ErrNotFound {
			return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
		}
		return nil, err
	}
	return e, nil
}

func (s *entityStore) GetAsAt(ctx context.Context, entityID string, asAt time.Time) (*spi.Entity, error) {
	// Round asAt up to the next millisecond boundary. Clients work at
	// millisecond precision but submitTime has microsecond precision.
	asAt = asAt.Truncate(time.Millisecond).Add(time.Millisecond)

	// Track in read set if in tx.
	if tx := spi.GetTransaction(ctx); tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return nil, fmt.Errorf("GetAsAt: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		tx.ReadSet[entityID] = true
	}

	asAtMicro := timeToMicro(asAt)
	row := s.db.QueryRowContext(ctx,
		`SELECT ev.entity_id, ev.model_name, ev.model_version, ev.version,
		        json(ev.data), json(ev.meta), ev.submit_time
		 FROM entity_versions ev
		 INNER JOIN (
		     SELECT entity_id, MAX(version) AS max_ver
		     FROM entity_versions
		     WHERE tenant_id = ? AND entity_id = ? AND submit_time < ?
		     GROUP BY entity_id
		 ) latest ON ev.entity_id = latest.entity_id AND ev.version = latest.max_ver
		 WHERE ev.tenant_id = ?`,
		string(s.tenantID), entityID, asAtMicro,
		string(s.tenantID))
	e, err := scanVersionEntity(row)
	if err != nil {
		if err == spi.ErrNotFound {
			return nil, fmt.Errorf("no version of entity %s exists at %v: %w", entityID, asAt, spi.ErrNotFound)
		}
		return nil, err
	}

	// Check if the found version is a delete.
	if e.Meta.ChangeType == "DELETED" {
		return nil, fmt.Errorf("entity %s was deleted at requested time: %w", entityID, spi.ErrNotFound)
	}

	return e, nil
}

func (s *entityStore) GetAll(ctx context.Context, modelRef spi.ModelRef) ([]*spi.Entity, error) {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return nil, fmt.Errorf("GetAll: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		return s.getAllTx(ctx, tx, modelRef)
	}

	return s.getAllDirect(ctx, modelRef)
}

func (s *entityStore) getAllDirect(ctx context.Context, modelRef spi.ModelRef) ([]*spi.Entity, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT entity_id, model_name, model_version, version,
		        json(data), json(meta), created_at, updated_at
		 FROM entities
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND NOT deleted`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion)
	if err != nil {
		return nil, fmt.Errorf("query entities: %w", err)
	}
	defer rows.Close()

	result := []*spi.Entity{}
	for rows.Next() {
		e, err := scanEntityFromRow(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration: %w", err)
	}
	return result, nil
}

func (s *entityStore) getAllTx(ctx context.Context, tx *spi.TransactionState, modelRef spi.ModelRef) ([]*spi.Entity, error) {
	// Get snapshot from entity_versions.
	snapshotMicro := timeToMicro(tx.SnapshotTime)
	rows, err := s.db.QueryContext(ctx,
		`SELECT ev.entity_id, ev.model_name, ev.model_version, ev.version,
		        json(ev.data), json(ev.meta), ev.submit_time
		 FROM entity_versions ev
		 INNER JOIN (
		     SELECT entity_id, MAX(version) AS max_ver
		     FROM entity_versions
		     WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND submit_time <= ?
		     GROUP BY entity_id
		 ) latest ON ev.entity_id = latest.entity_id AND ev.version = latest.max_ver
		 WHERE ev.tenant_id = ? AND ev.change_type != 'DELETED'`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion, snapshotMicro,
		string(s.tenantID))
	if err != nil {
		return nil, fmt.Errorf("query snapshot entities: %w", err)
	}
	defer rows.Close()

	result := make(map[string]*spi.Entity)
	for rows.Next() {
		e, err := scanVersionEntity(rows)
		if err != nil {
			return nil, err
		}
		if !tx.Deletes[e.Meta.ID] {
			result[e.Meta.ID] = e
			tx.ReadSet[e.Meta.ID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration: %w", err)
	}

	// Overlay buffer.
	for id, e := range tx.Buffer {
		if e.Meta.ModelRef == modelRef {
			result[id] = copyEntity(e)
			tx.ReadSet[id] = true
		}
	}

	entities := make([]*spi.Entity, 0, len(result))
	for _, e := range result {
		entities = append(entities, e)
	}
	return entities, nil
}

func (s *entityStore) GetAllAsAt(ctx context.Context, modelRef spi.ModelRef, asAt time.Time) ([]*spi.Entity, error) {
	// Round asAt up to the next millisecond boundary.
	asAt = asAt.Truncate(time.Millisecond).Add(time.Millisecond)

	asAtMicro := timeToMicro(asAt)
	rows, err := s.db.QueryContext(ctx,
		`SELECT ev.entity_id, ev.model_name, ev.model_version, ev.version,
		        json(ev.data), json(ev.meta), ev.submit_time
		 FROM entity_versions ev
		 INNER JOIN (
		     SELECT entity_id, MAX(version) AS max_ver
		     FROM entity_versions
		     WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND submit_time < ?
		     GROUP BY entity_id
		 ) latest ON ev.entity_id = latest.entity_id AND ev.version = latest.max_ver
		 WHERE ev.tenant_id = ? AND ev.change_type != 'DELETED'`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion, asAtMicro,
		string(s.tenantID))
	if err != nil {
		return nil, fmt.Errorf("query entities as-at: %w", err)
	}
	defer rows.Close()

	result := []*spi.Entity{}
	for rows.Next() {
		e, err := scanVersionEntity(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration: %w", err)
	}
	return result, nil
}

func (s *entityStore) Delete(ctx context.Context, entityID string) error {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return fmt.Errorf("Delete: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		// Check existence: buffer first, then committed store.
		if _, inBuffer := tx.Buffer[entityID]; !inBuffer {
			// Check snapshot visibility.
			_, err := s.getSnapshot(ctx, entityID, tx.SnapshotTime)
			if err != nil {
				return fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
			}
		}
		tx.Deletes[entityID] = true
		delete(tx.Buffer, entityID)
		tx.WriteSet[entityID] = true
		return nil
	}

	// Non-transaction: begin SQLite transaction first, then check existence
	// and delete atomically (no race window between check and delete).
	tid := string(s.tenantID)
	now := s.clock.Now()
	nowMicro := timeToMicro(now)

	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer sqlTx.Rollback()

	var curVersion int64
	var modelName, modelVersion string
	err = sqlTx.QueryRowContext(ctx,
		"SELECT version, model_name, model_version FROM entities WHERE tenant_id = ? AND entity_id = ? AND NOT deleted",
		tid, entityID).Scan(&curVersion, &modelName, &modelVersion)
	if err == sql.ErrNoRows {
		return fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("get entity for delete: %w", err)
	}

	nextVersion := curVersion + 1

	_, err = sqlTx.ExecContext(ctx,
		"UPDATE entities SET deleted = 1, updated_at = ?, version = ? WHERE tenant_id = ? AND entity_id = ?",
		nowMicro, nextVersion, tid, entityID)
	if err != nil {
		return fmt.Errorf("soft delete entity: %w", err)
	}

	uc := spi.GetUserContext(ctx)
	userName := ""
	if uc != nil {
		userName = uc.UserID
	}

	_, err = sqlTx.ExecContext(ctx,
		`INSERT INTO entity_versions
		 (tenant_id, entity_id, model_name, model_version, version, data, meta, change_type, transaction_id, submit_time, user_id)
		 VALUES (?, ?, ?, ?, ?, NULL, NULL, 'DELETED', '', ?, ?)`,
		tid, entityID, modelName, modelVersion,
		nextVersion, nowMicro, userName)
	if err != nil {
		return fmt.Errorf("insert delete version: %w", classifyError(err))
	}

	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("commit delete: %w", classifyError(err))
	}
	return nil
}

func (s *entityStore) DeleteAll(ctx context.Context, modelRef spi.ModelRef) error {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return fmt.Errorf("DeleteAll: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		// Get all entities for the model (snapshot), mark each as deleted.
		snapshotMicro := timeToMicro(tx.SnapshotTime)
		rows, err := s.db.QueryContext(ctx,
			`SELECT ev.entity_id, ev.model_name, ev.model_version, ev.version,
			        json(ev.data), json(ev.meta), ev.submit_time
			 FROM entity_versions ev
			 INNER JOIN (
			     SELECT entity_id, MAX(version) AS max_ver
			     FROM entity_versions
			     WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND submit_time <= ?
			     GROUP BY entity_id
			 ) latest ON ev.entity_id = latest.entity_id AND ev.version = latest.max_ver
			 WHERE ev.tenant_id = ? AND ev.change_type != 'DELETED'`,
			string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion, snapshotMicro,
			string(s.tenantID))
		if err != nil {
			return fmt.Errorf("query snapshot entities for deleteAll: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			e, err := scanVersionEntity(rows)
			if err != nil {
				return err
			}
			tx.Deletes[e.Meta.ID] = true
			delete(tx.Buffer, e.Meta.ID)
			tx.WriteSet[e.Meta.ID] = true
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("row iteration: %w", err)
		}

		// Also delete any buffered entities for this model.
		toDelete := make([]string, 0)
		for id, e := range tx.Buffer {
			if e.Meta.ModelRef == modelRef {
				toDelete = append(toDelete, id)
			}
		}
		for _, id := range toDelete {
			delete(tx.Buffer, id)
			tx.Deletes[id] = true
			tx.WriteSet[id] = true
		}
		return nil
	}

	// Non-transaction: query all entity IDs for this model and delete each.
	tid := string(s.tenantID)
	rows, err := s.db.QueryContext(ctx,
		"SELECT entity_id FROM entities WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND NOT deleted",
		tid, modelRef.EntityName, modelRef.ModelVersion)
	if err != nil {
		return fmt.Errorf("query entities for deleteAll: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan entity ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("row iteration: %w", err)
	}

	for _, id := range ids {
		if err := s.Delete(ctx, id); err != nil {
			return fmt.Errorf("delete entity %s: %w", id, err)
		}
	}
	return nil
}

func (s *entityStore) Exists(ctx context.Context, entityID string) (bool, error) {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		if tx.RolledBack {
			return false, fmt.Errorf("Exists: %w (txID=%s)", spi.ErrTxRolledBack, tx.ID)
		}
		// Check deletes first.
		if tx.Deletes[entityID] {
			return false, nil
		}
		// Check buffer.
		if _, ok := tx.Buffer[entityID]; ok {
			return true, nil
		}
		// Fall back to snapshot.
		_, err := s.getSnapshot(ctx, entityID, tx.SnapshotTime)
		return err == nil, nil
	}

	// Non-transaction.
	var exists bool
	err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM entities WHERE tenant_id = ? AND entity_id = ? AND NOT deleted)",
		string(s.tenantID), entityID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check existence: %w", err)
	}
	return exists, nil
}

func (s *entityStore) Count(ctx context.Context, modelRef spi.ModelRef) (int64, error) {
	tx := spi.GetTransaction(ctx)
	if tx != nil {
		// Use the same logic as GetAll to get the merged view, then count.
		all, err := s.GetAll(ctx, modelRef)
		if err != nil {
			return 0, err
		}
		return int64(len(all)), nil
	}

	// Non-transaction.
	var count int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM entities WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND NOT deleted",
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count entities: %w", err)
	}
	return count, nil
}

// sqliteMaxVariableNumber matches the conservative default for
// SQLITE_MAX_VARIABLE_NUMBER. SQLite ≥3.32 raised the compiled-in default
// to 32766, but builds that haven't been recompiled (and embedded
// distributions older callers may load) still use 999. We pick the lower
// bound so the cap holds across all reachable SQLite builds without
// requiring runtime PRAGMA inspection.
const sqliteMaxVariableNumber = 999

// countByStateBaseParams is the number of bound parameters in the
// CountByState query that are NOT state values: tenant_id, model_name,
// model_version. Each consumed slot reduces what's left for the IN list.
const countByStateBaseParams = 3

// MaxStateFilterSize caps the number of state names accepted in a
// CountByState filter. The cap is derived from SQLite's bound-variable
// limit minus the count of other parameters bound in the same query, so
// the IN-clause `?`-list combined with the base params can never exceed
// SQLITE_MAX_VARIABLE_NUMBER. State values are already bound as
// parameters (no interpolation), but the bounded-input contract closes
// the door on a caller accidentally passing a huge list and triggering a
// driver-level "too many SQL variables" error rather than a clean
// helper-boundary rejection. See issue #68 (item 11) and #99.
const MaxStateFilterSize = sqliteMaxVariableNumber - countByStateBaseParams

// ErrStateFilterTooLarge is returned by CountByState when the caller
// supplies more state names than MaxStateFilterSize allows. Callers can
// detect it via errors.Is — wrapped with the actual and max sizes to aid
// diagnostics without leaking SQL driver internals.
var ErrStateFilterTooLarge = errors.New("state filter exceeds maximum size")

// inPlaceholders returns `?,?,?,...` with n markers. n must be > 0 —
// callers check for the empty case. The returned string is composed
// entirely of `?` and `,` characters; no caller-supplied data is ever
// interpolated.
func inPlaceholders(n int) string {
	buf := make([]byte, 0, 2*n)
	for i := 0; i < n; i++ {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '?')
	}
	return string(buf)
}

// CountByState returns counts of non-deleted entities grouped by state for the
// given model. See SPI godoc on EntityStore.CountByState for filter semantics.
//
// The state value is stored inside the meta BLOB as JSON; we extract it via
// json_extract(json(meta), '$.state'). An indexed expression on this extraction is
// a future optimization (out of scope for this issue).
//
// In-tx callers fall back to GetAll-then-count-in-Go to honour merged-view
// snapshot semantics, matching the existing Count method's pattern.
func (s *entityStore) CountByState(ctx context.Context, modelRef spi.ModelRef, states []string) (map[string]int64, error) {
	if states != nil && len(states) == 0 {
		return map[string]int64{}, nil
	}
	if len(states) > MaxStateFilterSize {
		return nil, fmt.Errorf("%w: got %d, max %d", ErrStateFilterTooLarge, len(states), MaxStateFilterSize)
	}

	tx := spi.GetTransaction(ctx)
	if tx != nil {
		// In-tx: use GetAll's merged-view logic (matches existing Count's in-tx fallback).
		all, err := s.GetAll(ctx, modelRef)
		if err != nil {
			return nil, err
		}
		var filter map[string]struct{}
		if states != nil {
			filter = make(map[string]struct{}, len(states))
			for _, st := range states {
				filter[st] = struct{}{}
			}
		}
		result := make(map[string]int64)
		for _, e := range all {
			st := e.Meta.State
			if filter != nil {
				if _, ok := filter[st]; !ok {
					continue
				}
			}
			result[st]++
		}
		return result, nil
	}

	// Non-transaction: aggregate at the database.
	// The base parameters bound here MUST match countByStateBaseParams —
	// MaxStateFilterSize is derived as sqliteMaxVariableNumber minus that
	// constant, so any drift here would silently let the IN-list-plus-base
	// total exceed the SQLite variable limit.
	args := make([]any, 0, countByStateBaseParams+len(states))
	args = append(args, string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion)
	if len(args) != countByStateBaseParams {
		// Defensive: if a future edit adds/removes a base bind, this trips
		// before the query reaches the driver.
		return nil, fmt.Errorf("count by state: base param count drift (got %d, want %d)",
			len(args), countByStateBaseParams)
	}
	// Entities with no $._meta.state are bucketed under "" rather than dropped,
	// preserving them for diagnostic visibility. This matches the in-tx Go path
	// which reads e.Meta.State (also "" if unset).
	q := `SELECT COALESCE(json_extract(json(meta), '$.state'), '') AS state, COUNT(*)
	      FROM entities
	      WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND NOT deleted`

	if states != nil {
		// Build IN (?, ?, ...) placeholder list. The string is built from
		// `?` markers only — no state value ever enters the query text.
		// State values are bound as SQL parameters below. Size is bounded
		// by MaxStateFilterSize above (derived from sqliteMaxVariableNumber
		// minus countByStateBaseParams).
		q += ` AND json_extract(json(meta), '$.state') IN (` + inPlaceholders(len(states)) + `)`
		for _, st := range states {
			args = append(args, st)
		}
	}
	q += ` GROUP BY state`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("count entities by state: %w", err)
	}
	defer rows.Close()

	result := make(map[string]int64)
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, fmt.Errorf("scan count by state: %w", err)
		}
		result[st] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate count by state: %w", err)
	}
	return result, nil
}

func (s *entityStore) GetVersionHistory(ctx context.Context, entityID string) ([]spi.EntityVersion, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ev.entity_id, ev.model_name, ev.model_version, ev.version,
		        json(ev.data), json(ev.meta), ev.submit_time,
		        ev.change_type, ev.user_id, ev.transaction_id
		 FROM entity_versions ev
		 WHERE ev.tenant_id = ? AND ev.entity_id = ?
		 ORDER BY ev.version ASC`,
		string(s.tenantID), entityID)
	if err != nil {
		return nil, fmt.Errorf("query version history: %w", err)
	}
	defer rows.Close()

	var history []spi.EntityVersion
	for rows.Next() {
		var (
			eid, modelName, modelVersion    string
			version                         int64
			dataJSON                        sql.NullString
			metaJSON                        sql.NullString
			submitTimeMicro                 int64
			changeType, userID, transaction string
		)
		if err := rows.Scan(&eid, &modelName, &modelVersion, &version,
			&dataJSON, &metaJSON, &submitTimeMicro,
			&changeType, &userID, &transaction); err != nil {
			return nil, fmt.Errorf("scan version row: %w", err)
		}

		deleted := changeType == "DELETED"
		ev := spi.EntityVersion{
			ChangeType: changeType,
			User:       userID,
			Timestamp:  microToTime(submitTimeMicro),
			Version:    version,
			Deleted:    deleted,
		}

		if !deleted && dataJSON.Valid {
			meta := spi.EntityMeta{
				ID:               eid,
				ModelRef:         spi.ModelRef{EntityName: modelName, ModelVersion: modelVersion},
				Version:          version,
				CreationDate:     microToTime(submitTimeMicro),
				LastModifiedDate: microToTime(submitTimeMicro),
				TransactionID:    transaction,
				ChangeType:       changeType,
				ChangeUser:       userID,
			}
			// Populate additional meta from the stored meta BLOB.
			if metaJSON.Valid && metaJSON.String != "" {
				parsed, parseErr := unmarshalEntityMeta([]byte(metaJSON.String))
				if parseErr == nil {
					meta.TenantID = parsed.TenantID
					meta.State = parsed.State
					meta.CreationDate = parsed.CreationDate
					meta.LastModifiedDate = parsed.LastModifiedDate
					meta.TransitionForLatestSave = parsed.TransitionForLatestSave
				}
			}

			ev.Entity = &spi.Entity{
				Meta: meta,
				Data: []byte(dataJSON.String),
			}
		}

		history = append(history, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration: %w", err)
	}

	if history == nil {
		return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}

	return history, nil
}
