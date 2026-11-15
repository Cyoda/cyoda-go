package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	sqlite3 "github.com/ncruces/go-sqlite3"
)

type modelStore struct {
	db        *sql.DB
	readDB    *sql.DB // deferred-transaction reader pool; Get's snapshot read
	tenantID  spi.TenantID
	applyFunc ApplyFunc
	cfg       config // plugin config — read SchemaSavepointInterval from here
}

// modelDoc is the JSON representation stored in the doc BLOB column.
// Field names match the postgres plugin: camelCase, ref nested.
type modelDoc struct {
	Ref struct {
		EntityName   string `json:"entityName"`
		ModelVersion string `json:"modelVersion"`
	} `json:"ref"`
	State       spi.ModelState  `json:"state"`
	ChangeLevel spi.ChangeLevel `json:"changeLevel"`
	UpdateDate  string          `json:"updateDate"`
	Schema      []byte          `json:"schema"`
	UniqueKeys  []spi.UniqueKey `json:"uniqueKeys,omitempty"`
}

func (s *modelStore) Save(ctx context.Context, desc *spi.ModelDescriptor) error {
	var doc modelDoc
	doc.Ref.EntityName = desc.Ref.EntityName
	doc.Ref.ModelVersion = desc.Ref.ModelVersion
	doc.State = desc.State
	doc.ChangeLevel = desc.ChangeLevel
	doc.UpdateDate = desc.UpdateDate.UTC().Format(time.RFC3339Nano)
	doc.Schema = desc.Schema
	doc.UniqueKeys = desc.UniqueKeys
	raw, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("failed to marshal model descriptor: %w", err)
	}

	// The extension log of a model that is not LOCKED is empty — Unlock folds
	// it into the schema and clears it — so a Save over a non-LOCKED model
	// has no log to clear, and a Save over a LOCKED one is refused.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyError(fmt.Errorf("begin save tx for %s: %w", desc.Ref, err))
	}
	defer func() { _ = tx.Rollback() }()

	if locked, err := storedModelLocked(ctx, tx, s.tenantID, desc.Ref); err != nil {
		return fmt.Errorf("failed to save model %s: %w", desc.Ref, classifyError(err))
	} else if locked {
		return fmt.Errorf("model %s is locked: %w", desc.Ref, spi.ErrModelLocked)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO models (tenant_id, model_name, model_version, doc)
		 VALUES (?, ?, ?, jsonb(?))`,
		string(s.tenantID), desc.Ref.EntityName, desc.Ref.ModelVersion, raw); err != nil {
		return fmt.Errorf("failed to save model %s: %w", desc.Ref, classifyError(err))
	}
	if err := tx.Commit(); err != nil {
		return classifyError(fmt.Errorf("commit save of %s: %w", desc.Ref, err))
	}
	return nil
}

// storedModelLocked reports whether the stored model of ref is LOCKED, read
// in tx. An absent model is not locked.
func storedModelLocked(ctx context.Context, tx *sql.Tx, tenantID spi.TenantID, ref spi.ModelRef) (bool, error) {
	var state string
	err := tx.QueryRowContext(ctx,
		`SELECT json_extract(doc, '$.state') FROM models WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(tenantID), ref.EntityName, ref.ModelVersion).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return spi.ModelState(state) == spi.ModelLocked, nil
}

// clearExtensionLog deletes every extension row — deltas and savepoints —
// of ref in tx.
func clearExtensionLog(ctx context.Context, tx *sql.Tx, tenantID spi.TenantID, ref spi.ModelRef) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM model_schema_extensions
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(tenantID), ref.EntityName, ref.ModelVersion); err != nil {
		return classifyError(fmt.Errorf("clear extension log for %s: %w", ref, err))
	}
	return nil
}

// Get reads the model doc and folds its extension log inside one deferred
// read transaction, so both reads see one snapshot: a concurrent Unlock
// (fold into the doc, clear the log) is seen whole or not at all. The read
// handle is used, never BeginTx on the _txlock=immediate writer.
func (s *modelStore) Get(ctx context.Context, modelRef spi.ModelRef) (*spi.ModelDescriptor, error) {
	tx, err := s.readDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get model %s: %w", modelRef, classifyError(fmt.Errorf("begin read tx: %w", err)))
	}
	defer func() { _ = tx.Rollback() }()

	var raw []byte
	err = tx.QueryRowContext(ctx,
		`SELECT json(doc) FROM models WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("model %s not found: %w", modelRef, spi.ErrNotFound)
		}
		return nil, fmt.Errorf("failed to get model %s: %w", modelRef, classifyError(err))
	}
	desc, err := unmarshalModelDoc(raw)
	if err != nil {
		return nil, err
	}

	folded, _, err := s.foldLocked(ctx, tx, modelRef, desc.Schema, false)
	if err != nil {
		return nil, fmt.Errorf("fold extension log for %s: %w", modelRef, err)
	}
	desc.Schema = folded
	return desc, nil
}

func (s *modelStore) GetAll(ctx context.Context) ([]spi.ModelRef, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT model_name, model_version FROM models WHERE tenant_id = ?`,
		string(s.tenantID))
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	defer rows.Close()

	refs := make([]spi.ModelRef, 0)
	for rows.Next() {
		var name, version string
		if err := rows.Scan(&name, &version); err != nil {
			return nil, fmt.Errorf("failed to scan model row: %w", err)
		}
		refs = append(refs, spi.ModelRef{EntityName: name, ModelVersion: version})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}
	return refs, nil
}

// Delete removes an UNLOCKED model. The extension log of a model that is
// not LOCKED is empty (Unlock folds it into the schema and clears it), so
// there is no log to remove; a LOCKED model is refused.
func (s *modelStore) Delete(ctx context.Context, modelRef spi.ModelRef) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyError(fmt.Errorf("begin delete tx for %s: %w", modelRef, err))
	}
	defer func() { _ = tx.Rollback() }()

	if locked, err := storedModelLocked(ctx, tx, s.tenantID, modelRef); err != nil {
		return fmt.Errorf("failed to delete model %s: %w", modelRef, classifyError(err))
	} else if locked {
		return fmt.Errorf("model %s is locked: %w", modelRef, spi.ErrModelLocked)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM models WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion)
	if err != nil {
		return fmt.Errorf("failed to delete model %s: %w", modelRef, classifyError(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("model %s not found: %w", modelRef, spi.ErrNotFound)
	}
	if err := tx.Commit(); err != nil {
		return classifyError(fmt.Errorf("commit delete of %s: %w", modelRef, err))
	}
	return nil
}

// Lock transitions the model to the LOCKED state. It is the state update
// only: it writes no savepoint and touches no extension row. Locking an
// already-locked model is a no-op.
func (s *modelStore) Lock(ctx context.Context, modelRef spi.ModelRef) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyError(fmt.Errorf("begin lock tx: %w", err))
	}
	defer func() { _ = tx.Rollback() }()

	// Read current doc inside the tx.
	var raw []byte
	err = tx.QueryRowContext(ctx,
		`SELECT json(doc) FROM models WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("model %s not found: %w", modelRef, spi.ErrNotFound)
		}
		return fmt.Errorf("failed to lock model %s: %w", modelRef, classifyError(err))
	}

	var doc modelDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("failed to unmarshal model doc: %w", err)
	}

	// Idempotence: already-locked is a no-op.
	if doc.State == spi.ModelLocked {
		if err := tx.Commit(); err != nil {
			return classifyError(fmt.Errorf("commit lock (no-op): %w", err))
		}
		return nil
	}

	// Flip state to LOCKED.
	doc.State = spi.ModelLocked
	updated, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("failed to marshal model doc: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE models SET doc = jsonb(?) WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		updated, string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion); err != nil {
		return fmt.Errorf("failed to lock model %s: %w", modelRef, classifyError(err))
	}

	if err := tx.Commit(); err != nil {
		return classifyError(fmt.Errorf("commit lock: %w", err))
	}
	return nil
}

// Unlock transitions the model to the UNLOCKED state and folds its
// extension log into the stored schema, in one transaction: the schema a
// Get returns does not change, and the log is empty afterwards. The
// extensions entity writes made while the model was LOCKED are part of
// the model's schema; Unlock compacts them, it does not discard them.
func (s *modelStore) Unlock(ctx context.Context, modelRef spi.ModelRef) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyError(fmt.Errorf("begin unlock tx for %s: %w", modelRef, err))
	}
	defer func() { _ = tx.Rollback() }()

	var raw []byte
	err = tx.QueryRowContext(ctx,
		`SELECT json(doc) FROM models WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("model %s not found: %w", modelRef, spi.ErrNotFound)
		}
		return fmt.Errorf("failed to unlock model %s: %w", modelRef, classifyError(err))
	}
	var doc modelDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("failed to unmarshal model doc: %w", err)
	}

	var live int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entities WHERE tenant_id = ? AND model_name = ? AND model_version = ? AND NOT deleted`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion).Scan(&live); err != nil {
		return fmt.Errorf("failed to count entities of %s: %w", modelRef, classifyError(err))
	}
	if live > 0 {
		return fmt.Errorf("unlock %s: %w", modelRef, &spi.ModelHasEntitiesError{Ref: modelRef, Count: live})
	}

	folded, _, err := s.foldLocked(ctx, tx, modelRef, doc.Schema, false)
	if err != nil {
		return classifyError(fmt.Errorf("unlock fold for %s: %w", modelRef, err))
	}
	if err := clearExtensionLog(ctx, tx, s.tenantID, modelRef); err != nil {
		return fmt.Errorf("Unlock: %w", err)
	}

	doc.State = spi.ModelUnlocked
	doc.Schema = folded
	updated, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("failed to marshal model doc: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE models SET doc = jsonb(?) WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		updated, string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion); err != nil {
		return fmt.Errorf("failed to unlock model %s: %w", modelRef, classifyError(err))
	}
	if err := tx.Commit(); err != nil {
		return classifyError(fmt.Errorf("commit unlock of %s: %w", modelRef, err))
	}
	return nil
}

func (s *modelStore) IsLocked(ctx context.Context, modelRef spi.ModelRef) (bool, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT json(doc) FROM models WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("model %s not found: %w", modelRef, spi.ErrNotFound)
		}
		return false, fmt.Errorf("failed to check lock status for model %s: %w", modelRef, err)
	}

	desc, err := unmarshalModelDoc(raw)
	if err != nil {
		return false, err
	}
	return desc.State == spi.ModelLocked, nil
}

func (s *modelStore) SetChangeLevel(ctx context.Context, modelRef spi.ModelRef, level spi.ChangeLevel) error {
	// One statement that sets only the change level, so a concurrent
	// Lock, Unlock or Save is never overwritten by a stale copy of the doc.
	res, err := s.db.ExecContext(ctx,
		`UPDATE models SET doc = jsonb_set(doc, '$.changeLevel', ?)
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(level), string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion)
	if err != nil {
		return fmt.Errorf("failed to set change level for model %s: %w", modelRef, classifyError(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("model %s not found: %w", modelRef, spi.ErrNotFound)
	}
	return nil
}

func unmarshalModelDoc(raw []byte) (*spi.ModelDescriptor, error) {
	var doc modelDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("failed to unmarshal model doc: %w", err)
	}

	updateDate, err := time.Parse(time.RFC3339Nano, doc.UpdateDate)
	if err != nil {
		return nil, fmt.Errorf("failed to parse update date %q: %w", doc.UpdateDate, err)
	}

	return &spi.ModelDescriptor{
		Ref: spi.ModelRef{
			EntityName:   doc.Ref.EntityName,
			ModelVersion: doc.Ref.ModelVersion,
		},
		State:       doc.State,
		ChangeLevel: doc.ChangeLevel,
		UpdateDate:  updateDate,
		Schema:      doc.Schema,
		UniqueKeys:  doc.UniqueKeys,
	}, nil
}

// ExtendSchema appends a 'delta' row to model_schema_extensions, on a LOCKED
// model only (spi.ErrModelNotLocked otherwise, spi.ErrNotFound when absent).
// While the model is LOCKED the schema stored in models.doc does not change;
// Get folds the log on top of it via the injected ApplyFunc. Unlock and Save
// rewrite it (Unlock with the folded schema) and clear the log.
//
// The write runs in its own transaction and is not part of the caller's
// ambient transaction. A delta that changes nothing — applying it to the
// folded schema gives the folded schema's bytes — writes nothing. SQLITE_BUSY
// is retried until ctx ends; there is no retry budget.
//
// A missing ApplyFunc fails fast so wiring mistakes surface at extend-time.
func (s *modelStore) ExtendSchema(ctx context.Context, ref spi.ModelRef, delta spi.SchemaDelta) error {
	if len(delta) == 0 {
		return nil
	}
	if s.applyFunc == nil {
		return fmt.Errorf("sqlite: ApplyFunc not wired (call WithApplyFunc when creating StoreFactory)")
	}
	// SQLite's per-connection busy_timeout absorbs most writer contention;
	// the residual SQLITE_BUSY is retried until ctx ends, after a short
	// ctx-aware pause so a zero busy_timeout cannot hot-spin. Any other error
	// returns at once. The last BUSY error is rendered with %v, never %w:
	// contention is not a client-retryable conflict, so the error returned
	// when ctx ends must not satisfy errors.Is(_, spi.ErrConflict).
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("ExtendSchema cancelled after %d attempts: %w", attempt-1, err)
		}
		err := s.extendSchemaAttempt(ctx, ref, delta)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sqlite3.BUSY) {
			if cerr := ctx.Err(); cerr != nil {
				return fmt.Errorf("ExtendSchema cancelled after %d attempts: %w (last: %v)", attempt, cerr, err)
			}
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ExtendSchema cancelled after %d attempts: %w (last: %v)", attempt, ctx.Err(), err)
		case <-time.After(busyRetryPause):
		}
	}
}

// busyRetryPause is the pause between ExtendSchema attempts after SQLITE_BUSY.
const busyRetryPause = 5 * time.Millisecond

func (s *modelStore) extendSchemaAttempt(ctx context.Context, ref spi.ModelRef, delta spi.SchemaDelta) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyError(fmt.Errorf("begin tx: %w", err))
	}
	defer func() { _ = tx.Rollback() }()

	doc, err := s.modelDocInTx(ctx, tx, ref)
	if err != nil {
		return err
	}
	if doc.State != spi.ModelLocked {
		return fmt.Errorf("model %s is not locked: %w", ref, spi.ErrModelNotLocked)
	}
	base := doc.Schema

	// The fold, in the apply function's encoding even with no delta to
	// apply: one apply call for the whole log after the savepoint, whose seq
	// it returns.
	current, lastSP, err := s.foldLocked(ctx, tx, ref, base, true)
	if err != nil {
		return classifyError(fmt.Errorf("pre-persist fold for %s: %w", ref, err))
	}
	// A malformed delta is rejected before it reaches the log.
	applied, err := s.applyFunc(current, []spi.SchemaDelta{delta})
	if err != nil {
		return fmt.Errorf("applyFunc rejected delta for %s: %w", ref, err)
	}
	// No-op check: both sides are outputs of the same encoding, so a delta
	// that changes nothing gives the fold's own bytes and writes nothing.
	if bytes.Equal(applied, current) {
		return nil
	}

	var nextSeq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM model_schema_extensions
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(s.tenantID), ref.EntityName, ref.ModelVersion).Scan(&nextSeq); err != nil {
		return classifyError(fmt.Errorf("next seq lookup: %w", err))
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO model_schema_extensions
		    (tenant_id, model_name, model_version, seq, kind, payload)
		 VALUES (?, ?, ?, ?, 'delta', ?)`,
		string(s.tenantID), ref.EntityName, ref.ModelVersion, nextSeq, []byte(delta)); err != nil {
		return classifyError(fmt.Errorf("append delta for %s: %w", ref, err))
	}

	// A savepoint follows once SchemaSavepointInterval of the model's deltas
	// follow its latest savepoint (nextSeq - lastSP counts them). The
	// savepoint row sits at nextSeq+1 so it clusters right after the delta it
	// folds, with no PK collision.
	if nextSeq-lastSP >= int64(s.cfg.SchemaSavepointInterval) {
		// applied is the fold of every delta up to and including this
		// one: the savepoint stores it.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO model_schema_extensions
			    (tenant_id, model_name, model_version, seq, kind, payload)
			 VALUES (?, ?, ?, ?, 'savepoint', ?)`,
			string(s.tenantID), ref.EntityName, ref.ModelVersion, nextSeq+1, applied); err != nil {
			return classifyError(fmt.Errorf("append savepoint for %s (seq=%d): %w", ref, nextSeq+1, err))
		}
	}

	if err := tx.Commit(); err != nil {
		return classifyError(fmt.Errorf("commit: %w", err))
	}
	return nil
}

// modelDocInTx reads the model doc within the given tx. Returns
// spi.ErrNotFound wrapped when the model row is absent.
func (s *modelStore) modelDocInTx(ctx context.Context, tx *sql.Tx, ref spi.ModelRef) (*modelDoc, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx,
		`SELECT json(doc) FROM models WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(s.tenantID), ref.EntityName, ref.ModelVersion).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("model %s not found: %w", ref, spi.ErrNotFound)
	}
	if err != nil {
		return nil, classifyError(fmt.Errorf("model doc lookup for %s: %w", ref, err))
	}
	var doc modelDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("model doc unmarshal for %s: %w", ref, err)
	}
	return &doc, nil
}
