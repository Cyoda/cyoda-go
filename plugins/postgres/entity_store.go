package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// entityStore implements spi.EntityStore backed by PostgreSQL with
// dual-table writes: entities (current state) + entity_versions (history).
type entityStore struct {
	q        Querier
	tenantID spi.TenantID
	tm       *TransactionManager

	// pool is for the three kinds of statement that must NOT join the caller's
	// transaction, and so cannot go through q (which resolves it per call):
	//
	//   - every point-in-time read, which is committed-only by contract and
	//     therefore runs pool-pinned via committedQuerier (search_base.go);
	//   - the async-search scan, which runs in a transaction of its own so it
	//     can raise its statement ceiling with SET LOCAL (searcher.go,
	//     grouped_stats.go);
	//   - a compare-and-save taken OUTSIDE a caller transaction, which runs in
	//     a transaction of its own so the check's row lock and the write it
	//     guards commit as one step (CompareAndSave).
	//
	// acquireTimeout bounds the wait for a connection on all of them: the two
	// own-transaction Begins, and the second connection an IN-TRANSACTION
	// point-in-time read takes while the caller still holds the transaction's
	// (the hold-and-wait unjoinedQuerier documents). It bounds getting the
	// connection only, never using it.
	//
	// Every other statement goes through q. acquireTimeout is zero on the
	// test-only construction in export_test.go, which opens no transaction of
	// its own and issues no point-in-time read.
	pool           *pgxpool.Pool
	acquireTimeout time.Duration
}

// SaveAll delegates to Save per-entity via spi.DefaultSaveAll; each Save
// call records in writeSet (for updates) via recordWriteIfInTx. If this
// is ever optimized to a batch INSERT, the writeSet hooks must be preserved.
func (s *entityStore) SaveAll(ctx context.Context, entities iter.Seq[*spi.Entity]) ([]int64, error) {
	return spi.DefaultSaveAll(s, ctx, entities)
}

// txTimeSource is the SQL expression a save reads its transaction-time stamp
// from. The two values differ only for a transaction that waits mid-flight.
type txTimeSource string

const (
	// stampAtTxStart is CURRENT_TIMESTAMP: the transaction's start time, one
	// value for every entity saved under it. That is what gives the entities a
	// caller writes in a single transaction a common valid_time.
	stampAtTxStart txTimeSource = "CURRENT_TIMESTAMP"

	// stampAtStatement is statement_timestamp(): the moment the stamping
	// statement itself runs. CompareAndSave's own transaction uses it because
	// that transaction fixes its start time BEFORE waiting on the row lock, so
	// a caller that queued behind another writer would otherwise date its
	// version earlier than the version it just read and superseded — and a
	// point-in-time read would order the two backwards. That transaction saves
	// one entity, so it has no common valid_time to hold together.
	stampAtStatement txTimeSource = "statement_timestamp()"
)

func (s *entityStore) Save(ctx context.Context, entity *spi.Entity) (int64, error) {
	return s.save(ctx, entity, stampAtTxStart)
}

func (s *entityStore) save(ctx context.Context, entity *spi.Entity, stampFrom txTimeSource) (int64, error) {
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

	// Get DB timestamps first: stampFrom (see txTimeSource) for
	// valid_time/transaction_time, clock_timestamp() (actual wall clock) for
	// wall_clock_time.
	var dbNow, wallClockTime time.Time
	if err := s.q.QueryRow(ctx, `SELECT `+string(stampFrom)+`, clock_timestamp()`).Scan(&dbNow, &wallClockTime); err != nil {
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
		// Already classified: every statement this store issues goes through
		// ctxQuerier, which is where classification lives. Re-classifying here
		// would nest the wrapper a second time.
		return 0, fmt.Errorf("failed to upsert entity: %w", err)
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

	// Maintain unique-key claims atomically within this transaction.
	// replaceClaims is a no-op when no keys are declared in context.
	if err := s.replaceClaims(ctx, entity); err != nil {
		return 0, fmt.Errorf("failed to maintain unique claims: %w", err)
	}

	return nextVersion, nil
}

func (s *entityStore) CompareAndSave(ctx context.Context, entity *spi.Entity, expectedTxID string) (int64, error) {
	// Inside the caller's transaction the check and the write already run on
	// one connection, under that transaction — the check reads the
	// transaction's own view, and neither half can commit before the caller
	// says so. Nothing to add here.
	if spi.GetTransaction(ctx) != nil {
		if err := s.compareTxID(ctx, s.q, entity.Meta.ID, expectedTxID, false); err != nil {
			return 0, err
		}
		return s.Save(ctx, entity)
	}

	// Outside a transaction every statement is separately auto-committed, so
	// a check taken on its own leaves a check-then-write window: several
	// callers naming the same expected transaction ID all read it, all pass,
	// and all write, each silently clobbering the last instead of getting
	// ErrConflict. One database transaction closes it — the check takes the
	// row lock (FOR UPDATE) and the write commits under it, so a concurrent
	// caller blocks on the check and then reads the winner's ID.
	//
	// Same scoping rule as every other acquire in this plugin: the deadline
	// bounds getting the connection and is cancelled the instant BeginTx
	// returns, so the transaction handle — which outlives it — cannot inherit
	// it (newAcquireContext).
	//
	// READ COMMITTED is explicit, not inherited from the server default or a
	// DSN override, because it IS the mechanism described above: a caller
	// queued on the row lock re-reads the row once the winner commits and sees
	// the winner's transaction ID. Under REPEATABLE READ it would instead read
	// its pre-lock snapshot and abort with a serialization failure — a coarser
	// answer for a condition this path reports precisely.
	acquireCtx, cancelAcquire := newAcquireContext(ctx, s.acquireTimeout)
	tx, err := s.pool.BeginTx(acquireCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	cancelAcquire() // BeginTx has returned; the handle must not inherit the deadline
	if err != nil {
		// Same classification as every other acquire in this plugin: a
		// saturated pool is transient contention and carries the
		// storage-unavailable marker the application layer turns into a
		// retryable 503, while a caller who gave up first does not.
		return 0, classifyAcquireErr(ctx, acquireCtx, "failed to begin compare-and-save transaction", err)
	}
	// Rollback after a successful Commit is a no-op, so this covers every
	// error return below without a second exit path. On a context derived
	// WithoutCancel: a caller that cancelled mid-save is exactly when this
	// runs, and a rollback issued on an expired context never reaches the
	// server — leaving the transaction status non-idle, after which
	// pgxpool.Release destroys the connection instead of returning it.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// The tenant the RLS policies read, as every other transaction this plugin
	// opens sets it (TransactionManager.Begin, ExtendSchema's self-wrap, the
	// async-search scan). set_config rather than SET LOCAL because
	// PostgreSQL's SET takes no bound parameters.
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.current_tenant', $1, true)", string(s.tenantID)); err != nil {
		return 0, fmt.Errorf("failed to set tenant for compare-and-save: %w", classifyError(err))
	}

	// The whole save runs on that transaction's connection rather than
	// through s.q, which would resolve the pool and auto-commit each
	// statement. classifiedQuerier is the plain funnel s.q applies outside a
	// transaction, so the errors callers see are unchanged.
	txStore := *s
	txStore.q = classifiedQuerier{inner: tx}

	if err := txStore.compareTxID(ctx, txStore.q, entity.Meta.ID, expectedTxID, true); err != nil {
		return 0, err
	}
	// stampAtStatement, so the write is dated after the lock wait rather than
	// at this transaction's start — see txTimeSource.
	version, err := txStore.save(ctx, entity, stampAtStatement)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, classifyError(fmt.Errorf("failed to commit compare-and-save: %w", err))
	}
	return version, nil
}

// compareTxID reports whether the stored entity still carries expectedTxID,
// returning spi.ErrConflict when it does not. expectedTxID is compared
// literally: the entity's current transaction ID is the row's, or "" when
// there is no entity — never written, or deleted. So a non-empty expected ID
// against a missing entity conflicts (it names a version that does not
// exist), and the empty expected ID is how a caller says "expect no entity"
// and creates one.
//
// The row is read with NOT deleted, as Get and Delete read it: a deleted
// entity is no entity, so its tombstone does not offer up the superseded
// version's transaction ID for a caller to match against and resurrect the
// row. A delete applied earlier in the caller's own transaction is visible on
// that transaction's connection, so the same rule covers it — matching what
// the buffered backends answer for a same-transaction delete.
//
// forUpdate locks the row for the rest of the caller's database transaction —
// what makes the non-transactional path's check and write indivisible. It is
// false inside the caller's own transaction, whose write is not committed
// here and must not hold a row lock the caller did not ask for.
func (s *entityStore) compareTxID(ctx context.Context, q Querier, entityID, expectedTxID string, forUpdate bool) error {
	query := `SELECT doc->'_meta'->>'transaction_id' FROM entities WHERE tenant_id = $1 AND entity_id = $2 AND NOT deleted`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var scanned *string
	err := q.QueryRow(ctx, query, string(s.tenantID), entityID).Scan(&scanned)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("failed to check transaction ID: %w", err)
	}
	currentTxID := ""
	if err == nil && scanned != nil {
		currentTxID = *scanned
	}
	if currentTxID != expectedTxID {
		return fmt.Errorf("entity %s transaction ID mismatch (current=%q, expected=%q): %w",
			entityID, currentTxID, expectedTxID, spi.ErrConflict)
	}
	return nil
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

// GetAsAt is committed-only: it runs through committedQuerier (pool-pinned)
// rather than s.q, so an ambient transaction's own uncommitted writes are
// invisible to it — see committedQuerier's doc comment for why the query's
// transaction_time guard cannot achieve that on its own.
//
// Deliberately not tracked in readSet: historical reads target immutable versions. See spec §Known limitation.
func (s *entityStore) GetAsAt(ctx context.Context, entityID string, asAt time.Time) (*spi.Entity, error) {
	var doc []byte
	err := s.committedQuerier().QueryRow(ctx,
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

// GetAllAsAt is committed-only, through committedQuerier — the collection form
// of GetAsAt's routing, and for the same reason.
//
// Deliberately not tracked in readSet: historical reads target immutable versions. See spec §Known limitation.
func (s *entityStore) GetAllAsAt(ctx context.Context, modelRef spi.ModelRef, asAt time.Time) ([]*spi.Entity, error) {
	rows, err := s.committedQuerier().Query(ctx,
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
	//
	// Attribution: stamp the tombstone from the DELETER's own attribution
	// (spi.AttributionFor(ctx) — the caller of THIS Delete, i.e. the
	// stager), never re-marshal the PRIOR doc's ChangeUser/ChangeUserKind/
	// ChangeExecutor as-is — those describe the entity's previous writer,
	// not who deleted it. Unlike memory/sqlite, postgres executes this
	// DELETE (and its tombstone version insert) immediately in SQL — there
	// is no buffer/flush split — so the ctx in scope here already IS the
	// stager's; no separate stage-time-vs-commit-time distinction applies.
	attributed, executor := spi.AttributionFor(ctx)
	current.Meta.Version = nextVersion
	current.Meta.ChangeType = "DELETED"
	current.Meta.ChangeUser = attributed.ID
	current.Meta.ChangeUserKind = attributed.Kind
	current.Meta.ChangeExecutor = executor
	current.Meta.LastModifiedDate = dbNow
	// TransactionID: same rationale as attribution above — the tombstone
	// must record the DELETING transaction's own ID, not carry over the
	// PRIOR write's (`current` was unmarshaled from the pre-delete doc, so
	// its TransactionID is stale unless overwritten here). Empty for a
	// non-transactional delete, matching Save's non-tx convention and
	// memory's tombstone semantics — GetVersionByTransaction's empty-txID
	// pre-query rejection means an empty stamp here can never accidentally
	// match a caller-supplied txID.
	if tx := spi.GetTransaction(ctx); tx != nil {
		current.Meta.TransactionID = tx.ID
	} else {
		current.Meta.TransactionID = ""
	}

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

	// Release unique-key claims so the freed values can be re-claimed immediately.
	if err := s.releaseClaims(ctx, entityID); err != nil {
		return fmt.Errorf("failed to release unique claims: %w", err)
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

	// Belt-and-suspenders: bulk-delete any remaining claim rows for the model.
	// Individual Delete calls already invoke releaseClaims; this catches any
	// orphaned rows (e.g. from a previous partial run) and ensures a clean slate.
	if _, err := s.q.Exec(ctx,
		`DELETE FROM unique_claims WHERE tenant_id=$1 AND model_name=$2 AND model_version=$3`,
		tid, modelRef.EntityName, modelRef.ModelVersion,
	); err != nil {
		return fmt.Errorf("failed to bulk-release unique claims for model: %w", err)
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

// pageSlice-equivalent bounds check shared by GetPage's asAt and non-asAt
// paths.
func validatePageBounds(limit, offset int) error {
	if limit < 1 {
		return fmt.Errorf("GetPage: limit must be >= 1")
	}
	if offset < 0 {
		return fmt.Errorf("GetPage: offset must be >= 0")
	}
	return nil
}

// GetPage returns a page of modelRef's entities in canonical (byte-wise,
// COLLATE "C") entity-ID order. See spi.EntityStore.GetPage's doc comment
// for the full contract: limit>=1 && offset>=0 is required; asAt==nil reads
// the live (in-tx overlay, when a transaction is ambient) view and,
// in-tx, unconditionally records every returned entity in the
// transaction's read-set; asAt!=nil ignores any ambient transaction and
// reads committed-only state as of that instant.
//
// Unlike memory/sqlite, postgres's asAt==nil path needs no Go-side overlay
// merge (spi.MergeOrdered) at all, tx or not: s.q (ctxQuerier) already
// resolves to the ambient pgx.Tx when one is in context, and a PostgreSQL
// transaction always sees its own uncommitted writes on its own connection
// — Save/Delete write straight into the entities table (see Save/Delete's
// doc comments: postgres has no buffer/flush split) — so ONE query against
// entities, issued through s.q, already reflects "committed-as-of-BEGIN
// union this tx's own writes" with the WHERE NOT deleted / ORDER BY / LIMIT
// / OFFSET all pushed into SQL. This also sidesteps the failure mode a
// buffer-merge approach has to defend against: MergeOrdered pulls extra
// committed rows to replace ones shadowed by staged deletes, so a
// LIMIT/OFFSET-bounded prefetch can under-fill a page when deletes land
// inside that prefetch. There is no prefetch here to under-fill — every
// delete already flipped `deleted` to true in the very rows this query
// scans, before this statement even runs.
func (s *entityStore) GetPage(ctx context.Context, modelRef spi.ModelRef, limit, offset int, asAt *time.Time) ([]*spi.Entity, error) {
	if err := validatePageBounds(limit, offset); err != nil {
		return nil, err
	}
	if asAt != nil {
		return s.getPageAsAt(ctx, modelRef, limit, offset, *asAt)
	}
	return s.getPageCurrent(ctx, modelRef, limit, offset)
}

// getPageCurrentQuery is the SQL getPageCurrent runs. It is a named constant
// rather than an inline literal for one reason: entity_page_plan_test.go's
// EXPLAIN assertion must plan the query that ACTUALLY runs. A test that
// re-types the SQL keeps passing after the production query changes underneath
// it — a dropped COLLATE "C", a reordered ORDER BY — and the plan guarantee it
// exists to protect silently stops being checked. Sharing the constant makes
// that impossible: any edit here moves the test with it.
//
// $1 tenant, $2 model name, $3 model version, $4 limit, $5 offset.
const getPageCurrentQuery = `SELECT doc FROM entities
	 WHERE tenant_id = $1 AND model_name = $2 AND model_version = $3 AND NOT deleted
	 ORDER BY entity_id COLLATE "C"
	 LIMIT $4 OFFSET $5`

// getPageCurrent is GetPage's asAt==nil path (see GetPage's doc comment for
// why tx and non-tx share this one query): idx_entities_model_entity_id
// (migrations/000008_entities_model_entity_id_index.up.sql) serves both the
// WHERE equality filter and the ORDER BY entity_id COLLATE "C" from one
// index, so the plan needs no separate sort step — asserted by
// entity_page_plan_test.go via EXPLAIN over getPageCurrentQuery itself.
func (s *entityStore) getPageCurrent(ctx context.Context, modelRef spi.ModelRef, limit, offset int) ([]*spi.Entity, error) {
	rows, err := s.q.Query(ctx, getPageCurrentQuery,
		string(s.tenantID), modelRef.EntityName, modelRef.ModelVersion, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("GetPage: query: %w", err)
	}
	defer rows.Close()

	page, err := scanEntities(rows)
	if err != nil {
		return nil, fmt.Errorf("GetPage: %w", err)
	}

	// Unconditional: every entity on the returned page enters the
	// transaction's read-set — no TrackingRead knob, per GetPage's SPI doc
	// comment (unlike Search/Iterate's opt-in TrackingRead). No-op when ctx
	// carries no transaction (recordReadIfInTx's own guard).
	if s.tm != nil {
		for _, e := range page {
			s.tm.recordReadIfInTx(ctx, e.Meta.ID, e.Meta.Version)
		}
	}
	return page, nil
}

// getPageAsAt is GetPage's asAt!=nil path: a committed-only snapshot built
// on searchBaseQuery's PIT base (the same base Search/Iterate/GetAllAsAt
// use), paged via ORDER BY entity_id COLLATE "C" LIMIT/OFFSET.
//
// Deliberately bypasses s.q (which would resolve an ambient transaction)
// and issues the query through the pool-pinned committedQuerier instead —
// GetPage's SPI contract requires asAt!=nil to "ignore any ambient
// transaction and read committed-only state as of that instant", and s.q
// would otherwise see this transaction's own uncommitted writes on its own
// connection (the same read-your-own-writes behavior getPageCurrent relies
// on, here the wrong thing).
func (s *entityStore) getPageAsAt(ctx context.Context, modelRef spi.ModelRef, limit, offset int, asAt time.Time) ([]*spi.Entity, error) {
	query, args := s.searchBaseQuery(modelRef.EntityName, modelRef.ModelVersion, &asAt)
	query += fmt.Sprintf(` ORDER BY entity_id COLLATE "C" LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	args = append(args, limit, offset)

	rows, err := s.committedQuerier().Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("GetPage: asAt query: %w", err)
	}
	defer rows.Close()

	page, err := scanEntities(rows)
	if err != nil {
		return nil, fmt.Errorf("GetPage: asAt: %w", err)
	}
	return page, nil
}

// getVersionByTransactionQuery is the SQL GetVersionByTransaction runs, named
// for the same reason as getPageCurrentQuery: the EXPLAIN test that guards its
// index usage must plan the query that actually runs, not a copy of it.
//
// $1 tenant, $2 entity id, $3 transaction id.
const getVersionByTransactionQuery = `SELECT doc, version, valid_time FROM entity_versions
	 WHERE tenant_id = $1 AND entity_id = $2
	   AND doc->'_meta'->>'transaction_id' = $3
	   AND (doc->'_meta'->>'deleted')::boolean IS NOT TRUE
	 ORDER BY version ASC
	 LIMIT 1`

// GetVersionByTransaction returns the earliest (lowest-Version) version of
// entityID written by transaction txID. DELETED tombstones never match —
// see spi.EntityStore.GetVersionByTransaction's doc comment — and an empty
// txID never matches, even a stored-empty one from a non-transactional
// write, so it is rejected pre-query rather than reaching SQL at all.
//
// The deleted predicate reuses scanEntitiesFilterDeleted's exact _meta
// probe (doc->'_meta'->>'deleted'), restated in SQL rather than a second,
// possibly-diverging convention.
//
// tenant_id/entity_id scope the scan to entity_versions' own PRIMARY KEY
// partition (tenant_id, entity_id, version) rather than a full table scan
// — asserted by entity_page_plan_test.go via EXPLAIN over
// getVersionByTransactionQuery itself. Deliberately not tracked in readSet:
// historical reads target immutable versions, matching GetAsAt/GetAllAsAt.
func (s *entityStore) GetVersionByTransaction(ctx context.Context, entityID, txID string) (*spi.EntityVersion, error) {
	if txID == "" {
		return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}

	var doc []byte
	var version int64
	var validTime time.Time
	err := s.q.QueryRow(ctx, getVersionByTransactionQuery,
		string(s.tenantID), entityID, txID).Scan(&doc, &version, &validTime)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
		}
		return nil, fmt.Errorf("GetVersionByTransaction: %w", err)
	}

	return unmarshalEntityVersion(doc, version, validTime)
}

// GetVersionMetadata returns entityID's version metadata — no entity
// payload, just the audit trail — newest first, ties broken by Version
// DESC. opts.From/opts.Until bound the window inclusively on valid_time (the
// same column GetAsAt/GetAllAsAt/GetVersionByTransaction treat as the
// canonical Timestamp); opts.Limit caps the row count (0 means all). The
// query projects doc->'_meta' alone — never the full doc — per
// spi.EntityStore.GetVersionMetadata's doc comment: this method surfaces
// audit metadata only.
//
// Existence is checked BEFORE the window filter is applied: an entity with
// a non-empty version history whose versions all fall outside
// [opts.From, opts.Until] returns an empty slice, not ErrNotFound.
// ErrNotFound is reserved for an entity with no version history at all —
// the memory plugin's canonical semantics for this method.
func (s *entityStore) GetVersionMetadata(ctx context.Context, entityID string, opts spi.VersionMetadataOptions) ([]spi.EntityVersionMeta, error) {
	tid := string(s.tenantID)

	var exists bool
	if err := s.q.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM entity_versions WHERE tenant_id = $1 AND entity_id = $2)`,
		tid, entityID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("GetVersionMetadata: existence check: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("entity %s: %w", entityID, spi.ErrNotFound)
	}

	query := `SELECT version, valid_time, doc->'_meta' FROM entity_versions WHERE tenant_id = $1 AND entity_id = $2`
	args := []any{tid, entityID}
	if opts.From != nil {
		args = append(args, *opts.From)
		query += fmt.Sprintf(" AND valid_time >= $%d", len(args))
	}
	if opts.Until != nil {
		args = append(args, *opts.Until)
		query += fmt.Sprintf(" AND valid_time <= $%d", len(args))
	}
	query += " ORDER BY version DESC"
	if opts.Limit > 0 {
		args = append(args, opts.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	rows, err := s.q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("GetVersionMetadata: query: %w", err)
	}
	defer rows.Close()

	result := []spi.EntityVersionMeta{}
	for rows.Next() {
		var version int64
		var validTime time.Time
		var metaRaw []byte
		if err := rows.Scan(&version, &validTime, &metaRaw); err != nil {
			return nil, fmt.Errorf("GetVersionMetadata: scan: %w", err)
		}
		var meta entityMeta
		if err := json.Unmarshal(metaRaw, &meta); err != nil {
			return nil, fmt.Errorf("GetVersionMetadata: parse _meta: %w", err)
		}
		result = append(result, spi.EntityVersionMeta{
			Version:        version,
			ChangeType:     meta.ChangeType,
			Timestamp:      validTime,
			User:           meta.ChangeUser,
			AttributedKind: spi.PrincipalKind(meta.ChangeUserKind),
			Executor:       spi.Principal{ID: meta.ChangeExecutorID, Kind: spi.PrincipalKind(meta.ChangeExecutorKind)},
			TransactionID:  meta.TransactionID,
			Deleted:        meta.ChangeType == "DELETED",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetVersionMetadata: row iteration: %w", err)
	}
	return result, nil
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
