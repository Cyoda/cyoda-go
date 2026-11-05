package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type modelStore struct {
	db        *sql.DB
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

	// Save replaces the model's schema wholesale, so the extension log of
	// the ref goes with it, in the same transaction: a Get after Save
	// returns the saved schema alone. Through the model state machine the
	// log is already empty here — Unlock folds it into the schema and
	// Delete removes it — so rows found here were written by a caller that
	// bypassed the state machine, and are reported.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyError(fmt.Errorf("begin save tx for %s: %w", desc.Ref, err))
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO models (tenant_id, model_name, model_version, doc)
		 VALUES (?, ?, ?, jsonb(?))`,
		string(s.tenantID), desc.Ref.EntityName, desc.Ref.ModelVersion, raw); err != nil {
		return fmt.Errorf("failed to save model %s: %w", desc.Ref, classifyError(err))
	}
	stale, err := clearExtensionLog(ctx, tx, s.tenantID, desc.Ref)
	if err != nil {
		return fmt.Errorf("Save: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return classifyError(fmt.Errorf("commit save of %s: %w", desc.Ref, err))
	}
	if stale != 0 {
		slog.Warn("Save cleared stale extension rows",
			"pkg", "sqlite", "ref", desc.Ref, "count", stale)
	}
	return nil
}

// clearExtensionLog deletes every extension row — deltas and savepoints —
// of ref in tx, and returns how many it deleted.
func clearExtensionLog(ctx context.Context, tx *sql.Tx, tenantID spi.TenantID, ref spi.ModelRef) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM model_schema_extensions
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(tenantID), ref.EntityName, ref.ModelVersion)
	if err != nil {
		return 0, classifyError(fmt.Errorf("clear extension log for %s: %w", ref, err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("clear extension log for %s: rows affected: %w", ref, err)
	}
	return n, nil
}

func (s *modelStore) Get(ctx context.Context, modelRef spi.ModelRef) (*spi.ModelDescriptor, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx,
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

	folded, err := s.foldLocked(ctx, modelRef, desc.Schema)
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

// Delete removes the model and, in the same transaction, its extension
// log: a model imported later under the same reference starts from its
// imported schema alone. Through the model state machine the log is
// already empty here (Delete requires UNLOCKED, and Unlock folds the log
// into the schema), so rows found here are reported.
func (s *modelStore) Delete(ctx context.Context, modelRef spi.ModelRef) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyError(fmt.Errorf("begin delete tx for %s: %w", modelRef, err))
	}
	defer func() { _ = tx.Rollback() }()

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
	stale, err := clearExtensionLog(ctx, tx, s.tenantID, modelRef)
	if err != nil {
		return fmt.Errorf("Delete: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return classifyError(fmt.Errorf("commit delete of %s: %w", modelRef, err))
	}
	if stale != 0 {
		slog.Warn("Delete cleared stale extension rows",
			"pkg", "sqlite", "ref", modelRef, "count", stale)
	}
	return nil
}

// Lock transitions the model to the LOCKED state and, on the UNLOCKED→LOCKED
// edge, atomically writes a savepoint row capturing the current fold. This
// materialises the ExtendSchema delta log into a single payload so downstream
// Get calls do not replay a growing log.
//
// Idempotence: calling Lock on an already-locked model is a no-op — no
// second savepoint is written.
//
// De-dup: if no new deltas have landed since the most recent savepoint
// (or there is no base schema and no deltas at all), the savepoint is
// skipped — there is nothing new to persist.
//
// Asymmetry with Unlock: Unlock does NOT write a savepoint. Unlock folds
// the extension log into the stored schema and clears it, so a savepoint
// written there would be removed in the same transaction.
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

	// Idempotence: already-locked is a no-op — no second savepoint.
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

	// Decide whether to write a savepoint. Skip when there's nothing new
	// to persist beyond the most recent savepoint.
	lastSP, err := s.lastSavepointSeqInTx(ctx, tx, modelRef)
	if err != nil {
		return classifyError(err)
	}
	var maxSeq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM model_schema_extensions
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion).Scan(&maxSeq); err != nil {
		return classifyError(fmt.Errorf("lock: max seq lookup: %w", err))
	}

	// De-dup: the last savepoint already reflects the current state.
	// This also covers the "no base, no deltas" case (both are 0).
	if maxSeq == lastSP {
		if err := tx.Commit(); err != nil {
			return classifyError(fmt.Errorf("commit lock: %w", err))
		}
		return nil
	}

	folded, err := s.foldLockedInTx(ctx, tx, modelRef, doc.Schema)
	if err != nil {
		return classifyError(fmt.Errorf("lock fold for %s: %w", modelRef, err))
	}
	// If the fold yields nothing meaningful (no base and no deltas
	// resolvable into bytes), skip — there is nothing to persist.
	if len(folded) == 0 {
		if err := tx.Commit(); err != nil {
			return classifyError(fmt.Errorf("commit lock: %w", err))
		}
		return nil
	}

	txID := ""
	if sptx := spi.GetTransaction(ctx); sptx != nil {
		txID = sptx.ID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO model_schema_extensions
		    (tenant_id, model_name, model_version, seq, kind, payload, tx_id)
		 VALUES (?, ?, ?, ?, 'savepoint', ?, ?)`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion, maxSeq+1, folded, txID); err != nil {
		return classifyError(fmt.Errorf("lock savepoint for %s: %w", modelRef, err))
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

	folded, err := s.foldLockedInTx(ctx, tx, modelRef, doc.Schema)
	if err != nil {
		return classifyError(fmt.Errorf("unlock fold for %s: %w", modelRef, err))
	}
	if _, err := clearExtensionLog(ctx, tx, s.tenantID, modelRef); err != nil {
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
	// SQLite has no jsonb_set; read-modify-write.
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT json(doc) FROM models WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("model %s not found: %w", modelRef, spi.ErrNotFound)
		}
		return fmt.Errorf("failed to read model %s: %w", modelRef, err)
	}

	var doc modelDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("failed to unmarshal model doc: %w", err)
	}
	doc.ChangeLevel = level
	updated, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("failed to marshal model doc: %w", err)
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE models SET doc = jsonb(?) WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		updated, string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion)
	if err != nil {
		return fmt.Errorf("failed to set change level for model %s: %w", modelRef, err)
	}
	n, _ := res.RowsAffected()
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

// ExtendSchema appends a 'delta' row to model_schema_extensions.
// Apply-in-place has been retired: the schema stored in models.doc is
// treated as an immutable base, and Get folds the extension log on top
// of it via foldLocked. This mirrors the postgres plugin's semantics
// (see plugins/postgres/model_store.go) adapted for sqlite's database/sql
// API and the single-writer transaction model (BEGIN IMMEDIATE).
//
// The whole operation runs inside a single internal sql.Tx so the
// delta append and (in Task 17) the savepoint fold are all-or-nothing.
// The ambient spi.Transaction, if any, does not wrap this tx — its ID
// is recorded in the tx_id column purely for diagnostic traceability.
//
// Empty or nil deltas are a no-op. Missing ApplyFunc fails fast so
// consumers notice wiring mistakes at extend-time rather than later
// at Get-time (even though foldLocked would also surface the error).
func (s *modelStore) ExtendSchema(ctx context.Context, ref spi.ModelRef, delta spi.SchemaDelta) error {
	if len(delta) == 0 {
		return nil
	}
	if s.applyFunc == nil {
		return fmt.Errorf("sqlite: ApplyFunc not wired (call WithApplyFunc when creating StoreFactory)")
	}
	// Transparent retry loop (B-I7 / §4.2 / §5.3).
	//
	// SQLite's per-connection busy_timeout already absorbs most
	// contention between concurrent writers; this loop handles the
	// residual SQLITE_BUSY that surfaces once the timeout is exceeded
	// (classifyError maps SQLITE_BUSY → spi.ErrConflict).
	//
	// Ctx semantics: cancellation observed between attempts returns
	// ctx.Err() wrapped with the attempt count — not ErrRetryExhausted.
	// Cancellation observed mid-attempt bubbles up from the driver; it
	// is not spi.ErrConflict, so the loop exits on the first iteration.
	// Only conflict-classified errors are retried. Budget exhaustion
	// without cancellation returns spi.ErrRetryExhausted wrapping the
	// last conflict.
	var lastErr error
	for attempt := 1; attempt <= s.cfg.SchemaExtendMaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("ExtendSchema cancelled after %d attempts: %w", attempt-1, err)
		}
		err := s.extendSchemaAttempt(ctx, ref, delta)
		if err == nil {
			return nil
		}
		if !errors.Is(err, spi.ErrConflict) {
			// Non-retryable (including ctx-cancellation bubbling up
			// from the driver mid-attempt). Surface immediately.
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("%w after %d attempts: %w",
		spi.ErrRetryExhausted, s.cfg.SchemaExtendMaxRetries, lastErr)
}

func (s *modelStore) extendSchemaAttempt(ctx context.Context, ref spi.ModelRef, delta spi.SchemaDelta) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyError(fmt.Errorf("begin tx: %w", err))
	}
	defer func() { _ = tx.Rollback() }()

	// Verify the model exists and capture base schema (used by the
	// savepoint-fold branch below).
	base, err := s.baseSchemaInTx(ctx, tx, ref)
	if err != nil {
		return err
	}

	// Pre-persist Apply check: reject deltas that applyFunc refuses
	// before they reach the extension log. Mirrors the memory plugin's
	// apply-inline behavior (plugins/memory/model_store.go:ExtendSchema)
	// so all three plugins share the same contract: malformed deltas fail
	// fast at ExtendSchema-time rather than lying dormant in the log until
	// a later fold-on-Get surfaces the error.
	//
	// applyFunc nil-guard lives in the public ExtendSchema wrapper (fails
	// fast), so by this point applyFunc is guaranteed non-nil.
	current, err := s.foldLockedInTx(ctx, tx, ref, base)
	if err != nil {
		return classifyError(fmt.Errorf("pre-persist fold for %s: %w", ref, err))
	}
	if _, err := s.applyFunc(current, delta); err != nil {
		return fmt.Errorf("applyFunc rejected delta for %s: %w", ref, err)
	}

	// Determine next seq for this (tenant, model, version).
	var nextSeq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM model_schema_extensions
		 WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(s.tenantID), ref.EntityName, ref.ModelVersion).Scan(&nextSeq); err != nil {
		return classifyError(fmt.Errorf("next seq lookup: %w", err))
	}

	txID := ""
	if sptx := spi.GetTransaction(ctx); sptx != nil {
		txID = sptx.ID
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO model_schema_extensions
		    (tenant_id, model_name, model_version, seq, kind, payload, tx_id)
		 VALUES (?, ?, ?, ?, 'delta', ?, ?)`,
		string(s.tenantID), ref.EntityName, ref.ModelVersion, nextSeq, []byte(delta), txID); err != nil {
		return classifyError(fmt.Errorf("append delta for %s: %w", ref, err))
	}

	// Savepoint trigger. Fires when (nextSeq - lastSavepointSeq) crosses
	// the configured interval. The savepoint row sits at nextSeq+1 so it
	// clusters immediately after the delta it is folding, avoiding a PK
	// collision on (tenant, name, version, seq).
	//
	// An interval of 0 disables the size-based trigger entirely (only
	// save-on-lock writes savepoints). Production configs enforce a
	// minimum of 1 via envIntMin1Fn; the 0 path is exercised by the
	// default test fixture to isolate save-on-lock behavior.
	if s.cfg.SchemaSavepointInterval > 0 {
		lastSP, err := s.lastSavepointSeqInTx(ctx, tx, ref)
		if err != nil {
			return classifyError(err)
		}
		if nextSeq-lastSP >= int64(s.cfg.SchemaSavepointInterval) {
			folded, err := s.foldLockedInTx(ctx, tx, ref, base)
			if err != nil {
				return classifyError(fmt.Errorf("savepoint fold for %s (seq=%d): %w", ref, nextSeq, err))
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO model_schema_extensions
				    (tenant_id, model_name, model_version, seq, kind, payload, tx_id)
				 VALUES (?, ?, ?, ?, 'savepoint', ?, ?)`,
				string(s.tenantID), ref.EntityName, ref.ModelVersion, nextSeq+1, folded, txID); err != nil {
				return classifyError(fmt.Errorf("append savepoint for %s (seq=%d): %w", ref, nextSeq+1, err))
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return classifyError(fmt.Errorf("commit: %w", err))
	}
	return nil
}

// baseSchemaInTx reads models.doc.schema within the given tx. Returns
// spi.ErrNotFound wrapped when the model row is absent.
func (s *modelStore) baseSchemaInTx(ctx context.Context, tx *sql.Tx, ref spi.ModelRef) ([]byte, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx,
		`SELECT json(doc) FROM models WHERE tenant_id = ? AND model_name = ? AND model_version = ?`,
		string(s.tenantID), ref.EntityName, ref.ModelVersion).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("model %s not found: %w", ref, spi.ErrNotFound)
	}
	if err != nil {
		return nil, classifyError(fmt.Errorf("base schema lookup for %s: %w", ref, err))
	}
	var doc modelDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("base schema unmarshal for %s: %w", ref, err)
	}
	return doc.Schema, nil
}
