package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// modelStore implements spi.ModelStore backed by PostgreSQL. Every method
// runs on the model pool (model_pool.go) and never joins a transaction on
// ctx: a read returns the latest committed model, and a write commits on its
// own.
type modelStore struct {
	models    *modelPool
	q         Querier // single statements on the model pool; never joins a transaction on ctx
	tenantID  spi.TenantID
	applyFunc ApplyFunc // folds the extension log; ExtendSchema refuses to run without it
	cfg       config    // plugin config — read SchemaSavepointInterval from here
}

func newModelStore(models *modelPool, tenantID spi.TenantID, applyFunc ApplyFunc, cfg config) *modelStore {
	return &modelStore{
		models:    models,
		q:         unjoinedQuerier{pool: models.pool, acquireTimeout: models.acquireTimeout, what: "model"},
		tenantID:  tenantID,
		applyFunc: applyFunc,
		cfg:       cfg,
	}
}

// modelDoc is the JSON representation stored in the doc JSONB column.
// Field names match the spec: camelCase, ref nested.
type modelDoc struct {
	Ref struct {
		EntityName   string `json:"entityName"`
		ModelVersion string `json:"modelVersion"`
	} `json:"ref"`
	State       spi.ModelState  `json:"state"`
	ChangeLevel spi.ChangeLevel `json:"changeLevel"`
	UpdateDate  string          `json:"updateDate"` // RFC3339Nano
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
	// the ref goes with it, in the same statement: a Get after Save returns
	// the saved schema alone. Through the model state machine the log is
	// already empty here — Unlock folds it into the schema and Delete
	// removes it — so rows found here were written by a caller that
	// bypassed the state machine, and are reported.
	var stale int64
	err = s.q.QueryRow(ctx, `
		WITH ext AS (
			DELETE FROM model_schema_extensions
			WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3
			RETURNING 1
		), saved AS (
			INSERT INTO models (tenant_id, model_name, model_version, doc)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (tenant_id, model_name, model_version) DO UPDATE SET doc = EXCLUDED.doc
			RETURNING 1
		)
		SELECT (SELECT COUNT(*) FROM ext)`,
		string(s.tenantID), desc.Ref.EntityName, desc.Ref.ModelVersion, raw).Scan(&stale)
	if err != nil {
		return fmt.Errorf("failed to save model %s: %w", desc.Ref, err)
	}
	if stale != 0 {
		slog.Warn("Save cleared stale extension rows",
			"pkg", "postgres", "ref", desc.Ref, "count", stale)
	}
	return nil
}

// Get returns the latest committed model with its extension log folded
// into the schema, read in one statement.
func (s *modelStore) Get(ctx context.Context, modelRef spi.ModelRef) (*spi.ModelDescriptor, error) {
	m, err := foldModel(ctx, s.q, s.tenantID, modelRef, s.applyFunc, false)
	if err != nil {
		return nil, err
	}
	return m.desc, nil
}

func (s *modelStore) GetAll(ctx context.Context) ([]spi.ModelRef, error) {
	rows, err := s.q.Query(ctx,
		`SELECT model_name, model_version FROM models WHERE tenant_id = $1`,
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

// Delete removes the model and, in the same statement, its extension
// log: a model imported later under the same reference starts from its
// imported schema alone. Through the model state machine the log is
// already empty here (Delete requires UNLOCKED, and Unlock folds the log
// into the schema), so rows found here are reported.
func (s *modelStore) Delete(ctx context.Context, modelRef spi.ModelRef) error {
	var deleted, stale int64
	err := s.q.QueryRow(ctx, `
		WITH m AS (
			DELETE FROM models
			WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3
			RETURNING 1
		), ext AS (
			DELETE FROM model_schema_extensions
			WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3
			  AND EXISTS (SELECT 1 FROM m)
			RETURNING 1
		)
		SELECT (SELECT COUNT(*) FROM m), (SELECT COUNT(*) FROM ext)`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion).Scan(&deleted, &stale)
	if err != nil {
		return fmt.Errorf("failed to delete model %s: %w", modelRef, err)
	}
	if deleted == 0 {
		return fmt.Errorf("model %s not found: %w", modelRef, spi.ErrNotFound)
	}
	if stale != 0 {
		slog.Warn("Delete cleared stale extension rows",
			"pkg", "postgres", "ref", modelRef, "count", stale)
	}
	return nil
}

// Lock transitions the model to the LOCKED state. It is the state update
// alone: no extension reaches an UNLOCKED model, and Unlock, Save and a new
// model leave the extension log empty, so there is nothing to fold.
func (s *modelStore) Lock(ctx context.Context, modelRef spi.ModelRef) error {
	return updateStateField(ctx, s.q, s.tenantID, modelRef, spi.ModelLocked, "lock")
}

// Unlock transitions the model to the UNLOCKED state and folds its
// extension log into the stored schema, in one transaction: the schema a
// Get returns does not change, and the log is empty afterwards. The
// extensions entity writes made while the model was LOCKED are part of
// the model's schema; Unlock compacts them, it does not discard them.
//
// The state update runs first: it takes the model row's lock, which every
// extension takes too, so the fold sees every extension committed before it
// and no extension commits after it.
func (s *modelStore) Unlock(ctx context.Context, modelRef spi.ModelRef) error {
	return s.inModelTx(ctx, fmt.Sprintf("Unlock(%s)", modelRef), func(q Querier) error {
		if err := updateStateField(ctx, q, s.tenantID, modelRef, spi.ModelUnlocked, "unlock"); err != nil {
			return err
		}
		m, err := foldModel(ctx, q, s.tenantID, modelRef, s.applyFunc, false)
		if err != nil {
			return fmt.Errorf("Unlock: %w", err)
		}

		tag, err := q.Exec(ctx, `
			DELETE FROM model_schema_extensions
			WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
			string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion)
		if err != nil {
			return fmt.Errorf("Unlock: clear extension log for %s: %w", modelRef, err)
		}
		if tag.RowsAffected() == 0 {
			return nil // nothing was folded; the stored schema is current
		}

		var doc modelDoc
		if err := json.Unmarshal(m.doc, &doc); err != nil {
			return fmt.Errorf("Unlock: unmarshal model %s: %w", modelRef, err)
		}
		doc.Schema = m.desc.Schema
		updated, err := json.Marshal(doc)
		if err != nil {
			return fmt.Errorf("Unlock: marshal model %s: %w", modelRef, err)
		}
		if _, err := q.Exec(ctx,
			`UPDATE models SET doc = $4
			 WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
			string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion, updated); err != nil {
			return fmt.Errorf("Unlock: store folded schema for %s: %w", modelRef, err)
		}
		return nil
	})
}

func (s *modelStore) IsLocked(ctx context.Context, modelRef spi.ModelRef) (bool, error) {
	var raw []byte
	err := s.q.QueryRow(ctx,
		`SELECT doc FROM models WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
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
	tag, err := s.q.Exec(ctx,
		`UPDATE models
		 SET doc = jsonb_set(doc, '{changeLevel}', to_jsonb($4::text))
		 WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion, string(level))
	if err != nil {
		return fmt.Errorf("failed to set change level for model %s: %w", modelRef, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("model %s not found: %w", modelRef, spi.ErrNotFound)
	}
	return nil
}

// updateStateField updates the state field in the doc JSONB column for Lock/Unlock.
func updateStateField(ctx context.Context, q Querier, tenantID spi.TenantID, modelRef spi.ModelRef, state spi.ModelState, op string) error {
	tag, err := q.Exec(ctx,
		`UPDATE models
		 SET doc = jsonb_set(doc, '{state}', to_jsonb($4::text))
		 WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(tenantID), modelRef.EntityName, modelRef.ModelVersion, string(state))
	if err != nil {
		return fmt.Errorf("failed to %s model %s: %w", op, modelRef, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("model %s not found: %w", modelRef, spi.ErrNotFound)
	}
	return nil
}

// unmarshalModelDoc deserializes the JSONB doc column into a ModelDescriptor.
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

// ExtendSchema commits delta to the model's extension log in a short
// READ COMMITTED transaction of its own on the model pool. It never joins a
// transaction on ctx: when it returns nil the delta is committed, whatever
// happens to the caller's transaction.
//
// The transaction takes the model row's lock (SELECT ... FOR NO KEY UPDATE).
// The lock writes no row version; it serialises the extenders of one model and
// conflicts with the UPDATE of Lock, Unlock, SetChangeLevel and Save and with
// Delete. Under the lock the model must be LOCKED (else
// spi.ErrModelNotLocked), and every later statement sees every earlier
// extension's commit, so seq order is commit order and a savepoint folded
// under the lock holds every delta with a lower seq. The fold is a statement
// of its own after the lock statement: under READ COMMITTED a statement that
// waited for a lock re-checks only the locked row, so a fold merged into it
// would use the snapshot from before the wait.
//
// A delta the schema already holds writes nothing: applying it to the folded
// schema gives the folded schema's own bytes. Otherwise the delta row is
// inserted, and a savepoint holding the folded schema follows it once
// SchemaSavepointInterval of the model's deltas follow its latest savepoint.
//
// The transaction takes one row lock and runs at READ COMMITTED, so it cannot
// fail with a serialization failure or a deadlock: contention with other
// extenders is a wait, bounded by ctx and the statement ceiling. The wait for
// an extension slot lasts until ctx ends.
//
// An empty or nil delta is a no-op. A nil apply function is refused.
func (s *modelStore) ExtendSchema(ctx context.Context, ref spi.ModelRef, delta spi.SchemaDelta) error {
	if len(delta) == 0 {
		return nil
	}
	if s.applyFunc == nil {
		return fmt.Errorf("cannot extend model %s: ApplyFunc is not wired on the factory", ref)
	}

	conn, release, err := s.models.acquireExtension(ctx)
	if err != nil {
		return fmt.Errorf("failed to extend model %s: %w", ref, err)
	}
	defer release()

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return classifyError(fmt.Errorf("failed to begin the extension of %s: %w", ref, err))
	}
	// Rollback is idempotent in pgx: no-op once Commit has landed. It runs on
	// an uncancelled context, so the connection goes back to the pool idle
	// even when ctx has ended.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := s.extend(ctx, classifiedQuerier{inner: tx}, ref, delta); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return classifyError(fmt.Errorf("failed to commit the extension of %s: %w", ref, err))
	}
	return nil
}

// extend is ExtendSchema's transaction body on q.
func (s *modelStore) extend(ctx context.Context, q Querier, ref spi.ModelRef, delta spi.SchemaDelta) error {
	if err := setTenant(ctx, q, s.tenantID); err != nil {
		return fmt.Errorf("failed to set tenant for the extension of %s: %w", ref, err)
	}

	var state string
	err := q.QueryRow(ctx,
		`SELECT doc->>'state' FROM models
		 WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3
		 FOR NO KEY UPDATE`,
		string(s.tenantID), ref.EntityName, ref.ModelVersion).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("model %s not found: %w", ref, spi.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("failed to lock model %s for extension: %w", ref, err)
	}
	if state != string(spi.ModelLocked) {
		return fmt.Errorf("model %s is not locked: %w", ref, spi.ErrModelNotLocked)
	}

	// Two apply calls under the lock, however long the log: the fold (in the
	// apply function's encoding even with no delta to apply) and the fold
	// plus this delta.
	m, err := foldModel(ctx, q, s.tenantID, ref, s.applyFunc, true)
	if err != nil {
		return err
	}
	current := m.desc.Schema
	applied, err := s.applyFunc(current, []spi.SchemaDelta{delta})
	if err != nil {
		return fmt.Errorf("applyFunc rejected delta for %s: %w", ref, err)
	}
	if bytes.Equal(applied, current) {
		return nil // the schema already holds the delta
	}

	if _, err := q.Exec(ctx,
		`INSERT INTO model_schema_extensions (tenant_id, model_name, model_version, kind, payload)
		 VALUES ($1, $2, $3, 'delta', $4)`,
		string(s.tenantID), ref.EntityName, ref.ModelVersion, []byte(delta)); err != nil {
		return fmt.Errorf("failed to append schema delta for %s: %w", ref, err)
	}
	if m.pending+1 < s.cfg.SchemaSavepointInterval {
		return nil
	}
	if _, err := q.Exec(ctx,
		`INSERT INTO model_schema_extensions (tenant_id, model_name, model_version, kind, payload)
		 VALUES ($1, $2, $3, 'savepoint', $4)`,
		string(s.tenantID), ref.EntityName, ref.ModelVersion, applied); err != nil {
		return fmt.Errorf("failed to write savepoint for %s: %w", ref, err)
	}
	return nil
}

// inModelTx runs body in a READ COMMITTED transaction of its own on the model
// pool, never the caller's. op names the operation in error messages.
func (s *modelStore) inModelTx(ctx context.Context, op string, body func(q Querier) error) error {
	// The deadline bounds the acquire and is cancelled the instant Begin
	// returns, so the transaction handle cannot inherit it.
	acquireCtx, cancelAcquire := newAcquireContext(ctx, s.models.acquireTimeout)
	tx, err := s.models.pool.BeginTx(acquireCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	cancelAcquire()
	if err != nil {
		return classifyAcquireErr(ctx, acquireCtx, fmt.Sprintf("failed to begin %s", op), err)
	}
	// Rollback is idempotent in pgx: no-op once Commit has landed. It runs on
	// an uncancelled context, so the connection goes back to the pool idle
	// even when ctx has ended.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	q := classifiedQuerier{inner: tx}
	if err := setTenant(ctx, q, s.tenantID); err != nil {
		return fmt.Errorf("failed to set tenant for %s: %w", op, err)
	}
	if err := body(q); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return classifyError(fmt.Errorf("failed to commit %s: %w", op, err))
	}
	return nil
}

// setTenant sets the RLS tenant for the rest of the transaction on q, as
// every transaction this plugin opens does (TransactionManager.Begin, the
// async-search scan). The owner role bypasses the policies (row-level
// security is not supported), and keeping the setting on every transaction
// keeps the plugin's transactions alike.
func setTenant(ctx context.Context, q Querier, tenantID spi.TenantID) error {
	_, err := q.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", string(tenantID))
	return err
}
