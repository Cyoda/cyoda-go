package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// modelStore implements spi.ModelStore backed by PostgreSQL.
type modelStore struct {
	q         Querier
	pool      *pgxpool.Pool // used by ExtendSchema to self-wrap a pgx.Tx when no ambient tx is in ctx
	tenantID  spi.TenantID
	applyFunc ApplyFunc // optional; required when the extension log is non-empty
	cfg       config    // plugin config — read SchemaSavepointInterval from here
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

	_, err = s.q.Exec(ctx,
		`INSERT INTO models (tenant_id, model_name, model_version, doc)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (tenant_id, model_name, model_version) DO UPDATE SET doc = EXCLUDED.doc`,
		string(s.tenantID), desc.Ref.EntityName, desc.Ref.ModelVersion, raw)
	if err != nil {
		return fmt.Errorf("failed to save model %s: %w", desc.Ref, err)
	}

	// Save is the full-replace lifecycle path. By the operator contract
	// (docs/CONSISTENCY.md) the extension log must already be empty for
	// this ref — ExtendSchema is disjoint from Save via the state
	// machine. Defensively DELETE: in dev builds a non-zero RowsAffected
	// here is a fatal assertion; in production it logs a warning and
	// proceeds, because Save itself is the authoritative schema source
	// at this moment.
	tag, err := s.q.Exec(ctx, `
		DELETE FROM model_schema_extensions
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(s.tenantID), desc.Ref.EntityName, desc.Ref.ModelVersion)
	if err != nil {
		return fmt.Errorf("Save: clear extension log for %s: %w", desc.Ref, err)
	}
	if n := tag.RowsAffected(); n != 0 {
		if buildIsDev() {
			return fmt.Errorf("Save found %d stale extension rows for %s; operator-contract violation (see docs/CONSISTENCY.md)", n, desc.Ref)
		}
		slog.Warn("Save cleared stale extension rows",
			"pkg", "postgres", "ref", desc.Ref, "count", n)
	}
	return nil
}

func (s *modelStore) Get(ctx context.Context, modelRef spi.ModelRef) (*spi.ModelDescriptor, error) {
	var raw []byte
	err := s.q.QueryRow(ctx,
		`SELECT doc FROM models WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("model %s not found: %w", modelRef, spi.ErrNotFound)
		}
		return nil, fmt.Errorf("failed to get model %s: %w", modelRef, err)
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

func (s *modelStore) Delete(ctx context.Context, modelRef spi.ModelRef) error {
	tag, err := s.q.Exec(ctx,
		`DELETE FROM models WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion)
	if err != nil {
		return fmt.Errorf("failed to delete model %s: %w", modelRef, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("model %s not found: %w", modelRef, spi.ErrNotFound)
	}
	return nil
}

// Lock transitions the model to the LOCKED state and, atomically in the
// same context (= same ambient entity transaction), writes a savepoint
// row holding the pre-lock folded schema. Post-lock Gets start from the
// savepoint rather than replaying the full log, keeping fold cost
// bounded independent of the pre-lock extension count.
//
// Lock is idempotent: if the model is already locked, it is a no-op
// (no duplicate savepoint is written). If the base schema is absent
// (nil/empty), no savepoint is written either — there is nothing to
// replay from and a null JSONB payload would violate the column's
// NOT NULL constraint.
//
// Save-on-lock is deliberately asymmetric with Unlock (no save-on-unlock):
// Unlock drains the extension log wholesale per the operator contract,
// so a second savepoint at that moment would be load-bearing-free noise.
func (s *modelStore) Lock(ctx context.Context, modelRef spi.ModelRef) error {
	// Check pre-state so we can skip the savepoint write on the
	// idempotent re-lock path (already-locked model).
	wasUnlocked, err := s.isCurrentlyUnlocked(ctx, modelRef)
	if err != nil {
		return err
	}

	// Compute the fold BEFORE flipping state so the savepoint captures
	// the schema as-of pre-lock. ExtendSchema is the only writer to
	// model_schema_extensions and it is disjoint from the LOCKED phase
	// by the state machine, so there is no concurrent writer to race.
	var folded []byte
	if wasUnlocked {
		base, err := s.baseSchema(ctx, modelRef)
		if err != nil {
			return fmt.Errorf("lock fold base for %s: %w", modelRef, err)
		}
		folded, err = s.foldLocked(ctx, modelRef, base)
		if err != nil {
			return fmt.Errorf("lock fold for %s: %w", modelRef, err)
		}
	}

	txID := ""
	if tx := spi.GetTransaction(ctx); tx != nil {
		txID = tx.ID
	}

	if err := s.updateStateField(ctx, modelRef, spi.ModelLocked, "lock"); err != nil {
		return err
	}

	// Only write the savepoint on the first Lock (unlocked → locked)
	// and only when we have a non-empty folded payload to persist.
	if !wasUnlocked || len(folded) == 0 {
		return nil
	}

	if _, err := s.q.Exec(ctx,
		`INSERT INTO model_schema_extensions
		    (tenant_id, model_name, model_version, kind, payload, tx_id)
		 VALUES ($1, $2, $3, 'savepoint', $4, $5)`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion, folded, txID); err != nil {
		return fmt.Errorf("lock savepoint for %s: %w", modelRef, err)
	}
	return nil
}

// isCurrentlyUnlocked reports whether the model exists and is currently
// in the UNLOCKED state. Used by Lock to gate the save-on-lock path
// behind the unlocked→locked transition edge.
func (s *modelStore) isCurrentlyUnlocked(ctx context.Context, modelRef spi.ModelRef) (bool, error) {
	locked, err := s.IsLocked(ctx, modelRef)
	if err != nil {
		return false, err
	}
	return !locked, nil
}

func (s *modelStore) Unlock(ctx context.Context, modelRef spi.ModelRef) error {
	if err := s.updateStateField(ctx, modelRef, spi.ModelUnlocked, "unlock"); err != nil {
		return err
	}
	// Operator contract: all writers (ExtendSchema) must be drained
	// before Unlock. Lifecycle savepoints (including the one Lock
	// writes) are expected and not operator-contract violations —
	// only stale DELTA rows indicate a writer that bypassed the state
	// machine. Both are cleared here; the dev-mode assertion only
	// fires on delta remnants.
	var staleDeltas int
	if err := s.q.QueryRow(ctx, `
		SELECT COUNT(*) FROM model_schema_extensions
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND kind = 'delta'`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion).Scan(&staleDeltas); err != nil {
		return fmt.Errorf("Unlock: count stale deltas for %s: %w", modelRef, err)
	}

	if _, err := s.q.Exec(ctx, `
		DELETE FROM model_schema_extensions
		WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion); err != nil {
		return fmt.Errorf("Unlock: clear extension log for %s: %w", modelRef, err)
	}
	if staleDeltas != 0 {
		if buildIsDev() {
			return fmt.Errorf("Unlock found %d live extension rows for %s; operator-contract violation — concurrent writers did not drain before Unlock", staleDeltas, modelRef)
		}
		slog.Warn("Unlock drained stale extension rows",
			"pkg", "postgres", "ref", modelRef, "count", staleDeltas)
	}
	return nil
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
func (s *modelStore) updateStateField(ctx context.Context, modelRef spi.ModelRef, state spi.ModelState, op string) error {
	tag, err := s.q.Exec(ctx,
		`UPDATE models
		 SET doc = jsonb_set(doc, '{state}', to_jsonb($4::text))
		 WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion, string(state))
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

// ExtendSchema appends a delta row to model_schema_extensions. When
// the count of deltas since the most recent savepoint reaches the
// configured interval (cfg.SchemaSavepointInterval, default 64),
// a savepoint row holding the fully-folded schema is inserted in
// the same transaction so future Gets can start from there rather
// than replaying the entire log.
//
// Under REPEATABLE READ there is no schema-write conflict surface:
// concurrent writers both succeed, and A.2 I2 (commutativity)
// guarantees the fold is equivalent regardless of interleaving.
// No retry wrapper.
//
// Empty or nil deltas are a no-op.
func (s *modelStore) ExtendSchema(ctx context.Context, ref spi.ModelRef, delta spi.SchemaDelta) error {
	if len(delta) == 0 {
		return nil
	}

	// Atomicity (B-I6): the delta INSERT and the triggered savepoint
	// fold/INSERT must be all-or-nothing. If the fold fails (applyFunc
	// rejects — e.g. simulated ChangeLevel violation), neither row
	// may persist. When the caller supplies an ambient transaction via
	// spi.GetTransaction(ctx), rely on the caller's boundary. Otherwise
	// self-wrap in a pgx.Tx so the boundary exists.
	if spi.GetTransaction(ctx) != nil {
		return s.extendSchemaBody(ctx, s.q, ref, delta)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin self-wrap tx for ExtendSchema(%s): %w", ref, err)
	}
	// Rollback is idempotent in pgx: no-op once Commit has landed.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.extendSchemaBody(ctx, tx, ref, delta); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit self-wrap tx for ExtendSchema(%s): %w", ref, err)
	}
	return nil
}

// extendSchemaBody is the transactional body of ExtendSchema, parameterised
// on an explicit Querier (pgx.Tx when self-wrapping, or s.q when deferring
// to a caller-supplied ambient tx). It inserts the delta row and, if the
// savepoint interval has been crossed, folds + inserts a savepoint row —
// all using the same q so both writes succeed or fail together.
func (s *modelStore) extendSchemaBody(ctx context.Context, q Querier, ref spi.ModelRef, delta spi.SchemaDelta) error {
	txID := ""
	if tx := spi.GetTransaction(ctx); tx != nil {
		txID = tx.ID
	}

	// Use a shadow modelStore bound to q so the helpers (lastSavepointSeq,
	// baseSchema, foldLocked) see the same transactional view as the
	// delta INSERT below. Copying by value is cheap and keeps the helpers
	// unchanged.
	shadow := *s
	shadow.q = q

	// Pre-persist Apply check: reject deltas that applyFunc refuses before
	// they reach the extension log. Mirrors the memory plugin's
	// apply-inline behavior (plugins/memory/model_store.go:ExtendSchema)
	// so all three plugins share the same contract: malformed deltas fail
	// fast at ExtendSchema-time rather than lying dormant in the log until
	// a later fold-on-Get surfaces the error.
	//
	// Only run the check when applyFunc is wired. An unwired applyFunc is
	// valid on factories with zero-delta models; the log-empty case is
	// indistinguishable here from "applyFunc not needed yet", so we skip
	// the check and let the downstream fold-on-Get detect the wiring gap
	// (same as today's foldLocked behaviour).
	if s.applyFunc != nil {
		base, err := shadow.baseSchema(ctx, ref)
		if err != nil {
			return fmt.Errorf("pre-persist base read for %s: %w", ref, err)
		}
		current, err := shadow.foldLocked(ctx, ref, base)
		if err != nil {
			return fmt.Errorf("pre-persist fold for %s: %w", ref, err)
		}
		if _, err := s.applyFunc(current, delta); err != nil {
			return fmt.Errorf("applyFunc rejected delta for %s: %w", ref, err)
		}
	}

	var newSeq int64
	err := q.QueryRow(ctx,
		`INSERT INTO model_schema_extensions
		    (tenant_id, model_name, model_version, kind, payload, tx_id)
		 VALUES ($1, $2, $3, 'delta', $4, $5)
		 RETURNING seq`,
		string(s.tenantID), ref.EntityName, ref.ModelVersion, []byte(delta), txID).Scan(&newSeq)
	if err != nil {
		return fmt.Errorf("failed to append schema delta for %s: %w", ref, err)
	}

	lastSP, err := shadow.lastSavepointSeq(ctx, ref)
	if err != nil {
		return fmt.Errorf("savepoint trigger lookup for %s: %w", ref, err)
	}
	if newSeq-lastSP >= int64(s.cfg.SchemaSavepointInterval) {
		base, err := shadow.baseSchema(ctx, ref)
		if err != nil {
			return fmt.Errorf("savepoint base-schema read for %s: %w", ref, err)
		}
		folded, err := shadow.foldLocked(ctx, ref, base)
		if err != nil {
			return fmt.Errorf("savepoint fold for %s (seq=%d): %w", ref, newSeq, err)
		}
		if _, err := q.Exec(ctx,
			`INSERT INTO model_schema_extensions
			    (tenant_id, model_name, model_version, kind, payload, tx_id)
			 VALUES ($1, $2, $3, 'savepoint', $4, $5)`,
			string(s.tenantID), ref.EntityName, ref.ModelVersion, folded, txID); err != nil {
			return fmt.Errorf("failed to write savepoint for %s (seq=%d): %w", ref, newSeq, err)
		}
	}
	return nil
}

// baseSchema loads the stable base schema stored in models.doc.schema
// for the given ref. Used both by Get (pre-fold) and by ExtendSchema's
// savepoint branch.
func (s *modelStore) baseSchema(ctx context.Context, ref spi.ModelRef) ([]byte, error) {
	var raw []byte
	err := s.q.QueryRow(ctx,
		`SELECT doc FROM models WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3`,
		string(s.tenantID), ref.EntityName, ref.ModelVersion).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("model %s not found: %w", ref, spi.ErrNotFound)
		}
		return nil, fmt.Errorf("base schema lookup for %s: %w", ref, err)
	}
	desc, err := unmarshalModelDoc(raw)
	if err != nil {
		return nil, err
	}
	return desc.Schema, nil
}
