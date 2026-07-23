package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const submitTimeTTL = 1 * time.Hour

// committedTx records a committed transaction in the in-memory log.
type committedTx struct {
	id         string
	submitTime time.Time
	writeSet   map[string]bool
}

// savepointSnapshot holds a deep copy of transaction state at savepoint time.
type savepointSnapshot struct {
	buffer            map[string]*spi.Entity
	readSet           map[string]bool
	writeSet          map[string]bool
	deletes           map[string]bool
	deleteAttribution map[string]spi.WriteAttribution // paired 1:1 with deletes — see TransactionState godoc

	// scheduledTaskOpsLen is len(transactionManager.scheduledTaskOps[txID]) at
	// the moment this savepoint was taken. scheduledTaskOps is append-only
	// (see stageScheduledTaskOp), so — unlike the maps above, which are
	// deep-copied and restored wholesale — RollbackToSavepoint restores it by
	// truncating back to this recorded length instead of snapshotting it.
	scheduledTaskOpsLen int
}

// transactionManager implements spi.TransactionManager with application-layer
// Snapshot Isolation + First-Committer-Wins (SI+FCW). In-memory committedLog
// tracks conflicts; SQLite is the persistence layer.
//
// Commit ordering: acquire commitMu -> validate SI+FCW -> capture submitTime ->
// BEGIN IMMEDIATE -> flush -> COMMIT -> append committedLog -> prune -> release commitMu.
type transactionManager struct {
	factory  *StoreFactory
	uuids    spi.UUIDGenerator
	commitMu sync.Mutex // serializes the entire commit path for SI+FCW correctness
	mu       sync.Mutex // protects active, committedLog, committing, submitTimes, savepoints, txUniqueKeys

	active         map[string]*spi.TransactionState
	committedLog   []committedTx
	committing     map[string]bool
	submitTimes    map[string]time.Time
	savepoints     map[string]map[string]savepointSnapshot
	lastSubmitTime int64 // monotonic submit time in microseconds; written under commitMu, read under mu

	// txUniqueKeys holds per-entity unique keys captured at Save (buffer) time.
	// Keys are recorded when an entity is buffered so that flushToSQLite can
	// apply the correct keys per entity even in a mixed-model batch where each
	// Save may carry a different set of keys in its context.
	// Protected by mu. Cleaned up after commit or rollback.
	txUniqueKeys map[string]map[string][]spi.UniqueKey // txID → entityID → keys

	// scheduledTaskOps holds ScheduledTaskStore ops staged while the
	// transaction is open (mirrors txUniqueKeys's staging pattern — it
	// exists because *spi.TransactionState is a shared cyoda-go-spi type
	// plugins may not add fields to). Applied inside flushToSQLite's single
	// sqlTx, after the entity buffer/delete flush, so it commits atomically
	// with the entity write; discarded, never applied, on Rollback and on
	// every mid-Commit abort path (FCW conflict, flush error). Also
	// savepoint-scoped like tx.Buffer/ReadSet/WriteSet/Deletes: Savepoint
	// records the current length and RollbackToSavepoint truncates back to
	// it, so an op staged after a savepoint that is then rolled back is
	// discarded too, never orphaned from the entity work it must be atomic
	// with. Protected by mu. Cleaned up after commit or rollback (no leak).
	scheduledTaskOps map[string][]scheduledTaskOp // txID → staged ops
}

// Verify interface compliance at compile time.
var _ spi.TransactionManager = (*transactionManager)(nil)

func newTransactionManager(factory *StoreFactory, uuids spi.UUIDGenerator) *transactionManager {
	return &transactionManager{
		factory:          factory,
		uuids:            uuids,
		active:           make(map[string]*spi.TransactionState),
		committing:       make(map[string]bool),
		submitTimes:      make(map[string]time.Time),
		savepoints:       make(map[string]map[string]savepointSnapshot),
		txUniqueKeys:     make(map[string]map[string][]spi.UniqueKey),
		scheduledTaskOps: make(map[string][]scheduledTaskOp),
	}
}

// recordUniqueKeys stores the unique keys for entityID under txID so that
// flushToSQLite can look them up per entity during commit. Last-write-wins,
// matching the semantics of tx.Buffer. Protected by mu.
func (m *transactionManager) recordUniqueKeys(txID, entityID string, keys []spi.UniqueKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.txUniqueKeys[txID] == nil {
		m.txUniqueKeys[txID] = make(map[string][]spi.UniqueKey)
	}
	m.txUniqueKeys[txID][entityID] = keys
}

// uniqueKeysFor retrieves the unique keys recorded for entityID under txID.
// Returns nil if none were recorded. Protected by mu.
func (m *transactionManager) uniqueKeysFor(txID, entityID string) []spi.UniqueKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.txUniqueKeys[txID][entityID]
}

// stageScheduledTaskOp appends a staged ScheduledTaskStore op for txID.
// flushToSQLite applies the accumulated ops inside the same sqlTx as the
// entity buffer flush (atomically with it); every abort path — FCW
// conflict, flush error, and Rollback — discards them unapplied.
// Protected by mu.
func (m *transactionManager) stageScheduledTaskOp(txID string, op scheduledTaskOp) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scheduledTaskOps[txID] = append(m.scheduledTaskOps[txID], op)
}

// scheduledTaskOpsFor retrieves the ops staged for txID. Protected by mu.
func (m *transactionManager) scheduledTaskOpsFor(txID string) []scheduledTaskOp {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scheduledTaskOps[txID]
}

// seedLastSubmitTime reads the maximum submit_time from entity_versions
// so that lastSubmitTime is monotonic across process restarts.
func (m *transactionManager) seedLastSubmitTime() {
	var maxTime sql.NullInt64
	err := m.factory.db.QueryRow(
		"SELECT MAX(submit_time) FROM entity_versions").Scan(&maxTime)
	if err == nil && maxTime.Valid {
		m.lastSubmitTime = maxTime.Int64
	}
}

// Begin starts a new transaction. It resolves the tenant from the context,
// generates a unique transaction ID, captures a snapshot time, and returns
// a new context carrying the TransactionState.
func (m *transactionManager) Begin(ctx context.Context) (string, context.Context, error) {
	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return "", ctx, fmt.Errorf("no user context — cannot begin transaction")
	}
	if uc.Tenant.ID == "" {
		return "", ctx, fmt.Errorf("user context has no tenant — cannot begin transaction")
	}

	txID := uuid.UUID(m.uuids.NewTimeUUID()).String()
	nowMicro := m.factory.clock.Now().UnixMicro()

	tx := &spi.TransactionState{
		ID:                txID,
		TenantID:          uc.Tenant.ID,
		Origin:            spi.ResolveOrigin(ctx),
		ReadSet:           make(map[string]bool),
		WriteSet:          make(map[string]bool),
		Buffer:            make(map[string]*spi.Entity),
		Deletes:           make(map[string]bool),
		DeleteAttribution: make(map[string]spi.WriteAttribution),
	}

	// Snapshot time must be at least lastSubmitTime so that the transaction
	// sees all previously committed data. Without this floor, a monotonic
	// submit-time bump could push a commit past the next Begin's raw clock
	// value, making committed entities invisible to new transactions.
	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if nowMicro < m.lastSubmitTime {
			nowMicro = m.lastSubmitTime
		}
		tx.SnapshotTime = time.UnixMicro(nowMicro)
		m.active[txID] = tx
	}()

	return txID, spi.WithTransaction(ctx, tx), nil
}

// Join returns a context carrying the TransactionState for an existing active
// transaction. This allows multiple goroutines to participate in the same
// transaction.
func (m *transactionManager) Join(ctx context.Context, txID string) (context.Context, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tx, ok := m.active[txID]
	if !ok {
		return nil, fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if tx.RolledBack {
		return nil, fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxRolledBack, txID)
	}
	if tx.Closed {
		return nil, fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxAlreadyCommitted, txID)
	}

	// Verify tenant matches. Strict — rejects nil UserContext to match
	// Commit/Rollback's gate (#199 PR-C2 review L-3). Pre-PR-C2 this was
	// permissive on nil UC, allowing any caller without a UserContext to
	// Join an arbitrary active tx.
	uc := spi.GetUserContext(ctx)
	if uc == nil || uc.Tenant.ID != tx.TenantID {
		return nil, fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
	}

	return spi.WithTransaction(ctx, tx), nil
}

// Commit validates the transaction against the committed log for SI+FCW conflicts,
// flushes the write buffer and deletes to SQLite, and records the commit in the
// log.
//
// The commitMu serializes the entire commit path. This is required for SI+FCW
// correctness -- without it, two commits could both validate against a stale
// committedLog and both succeed, missing a conflict.
func (m *transactionManager) Commit(ctx context.Context, txID string) error {
	// 1. Look up the active transaction and mark as committing (TOCTOU guard).
	uc := spi.GetUserContext(ctx)
	m.mu.Lock()
	tx, ok := m.active[txID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("Commit: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if uc == nil || uc.Tenant.ID != tx.TenantID {
		m.mu.Unlock()
		return fmt.Errorf("Commit: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
	}
	if m.committing[txID] {
		m.mu.Unlock()
		return fmt.Errorf("Commit: %w (txID=%s)", spi.ErrTxCommitInProgress, txID)
	}
	m.committing[txID] = true
	m.mu.Unlock()

	// 1b. Acquire transaction operation write lock -- waits for in-flight operations.
	tx.OpMu.Lock()
	defer func() {
		tx.Closed = true
		tx.OpMu.Unlock()
	}()

	// 2. Acquire commitMu -- serializes the entire commit path.
	m.commitMu.Lock()
	defer m.commitMu.Unlock()

	// 3. Conflict detection: check committed log for overlapping write sets.
	// Unlike the memory plugin which uses entityMu to serialize CAS checks
	// against commits, the SQLite plugin uses commitMu. The SI+FCW check uses
	// !Before (>=) rather than After (>) for the submit time comparison.
	// This catches write-write conflicts even when commits happen at the
	// same clock tick (e.g., frozen TestClock). The memory plugin avoids
	// this by holding entityMu.Lock during commit, which blocks concurrent
	// CompareAndSave reads until the commit is visible.
	if err := func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, committed := range m.committedLog {
			if !committed.submitTime.Before(tx.SnapshotTime) {
				for entityID := range committed.writeSet {
					if tx.ReadSet[entityID] || tx.WriteSet[entityID] {
						delete(m.committing, txID)
						delete(m.active, txID)
						delete(m.savepoints, txID)
						delete(m.txUniqueKeys, txID)
						delete(m.scheduledTaskOps, txID)
						return spi.ErrConflict
					}
				}
			}
		}
		return nil
	}(); err != nil {
		return err
	}

	// 4. Capture submit time with monotonicity guarantee.
	// max(clock.Now().UnixMicro(), lastSubmitTime + 1) ensures forward
	// progress even under NTP steps, VM pause/migrate, or leap-second
	// smearing. commitMu serializes the commit path; m.mu protects
	// lastSubmitTime for reads in Begin.
	submitTime := func() time.Time {
		m.mu.Lock()
		defer m.mu.Unlock()
		nowMicro := m.factory.clock.Now().UnixMicro()
		if nowMicro <= m.lastSubmitTime {
			nowMicro = m.lastSubmitTime + 1
		}
		m.lastSubmitTime = nowMicro
		return time.UnixMicro(nowMicro)
	}()

	// 4.5. Snapshot staged ScheduledTaskStore ops for this tx. Safe to read
	// without extending m.mu across the whole flush: tx.OpMu.Lock (held
	// since step 1b) blocks every stage() call (which requires
	// tx.OpMu.RLock) from appending more ops for the duration of Commit,
	// so the slice is stable once captured here.
	scheduledOps := m.scheduledTaskOpsFor(txID)

	// 5. Flush buffer, deletes, and staged scheduled-task ops to SQLite.
	if err := m.flushToSQLite(ctx, tx, submitTime, scheduledOps); err != nil {
		// On flush failure, clean up the transaction.
		func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			tx.RolledBack = true
			delete(m.active, txID)
			delete(m.committing, txID)
			delete(m.savepoints, txID)
			delete(m.txUniqueKeys, txID)
			delete(m.scheduledTaskOps, txID)
		}()
		// ErrUniqueViolation from claim writes must not be re-classified —
		// classifyError passes through non-sqlite errors unchanged.
		return fmt.Errorf("flush to sqlite: %w", classifyError(err))
	}

	// 6. Record in committed log, submit times, and prune.
	func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		m.committedLog = append(m.committedLog, committedTx{
			id:         txID,
			submitTime: submitTime,
			writeSet:   tx.WriteSet,
		})
		m.submitTimes[txID] = submitTime

		// Evict old submit times beyond TTL.
		evictBefore := m.factory.clock.Now().Add(-submitTimeTTL)
		for id, t := range m.submitTimes {
			if t.Before(evictBefore) {
				delete(m.submitTimes, id)
			}
		}

		// Prune: find oldest active transaction's snapshot, remove older entries.
		delete(m.active, txID)
		delete(m.committing, txID)
		delete(m.savepoints, txID)
		delete(m.txUniqueKeys, txID)
		delete(m.scheduledTaskOps, txID)
		var oldest time.Time
		for _, activeTx := range m.active {
			if oldest.IsZero() || activeTx.SnapshotTime.Before(oldest) {
				oldest = activeTx.SnapshotTime
			}
		}
		if !oldest.IsZero() {
			pruned := m.committedLog[:0]
			for _, c := range m.committedLog {
				if !c.submitTime.Before(oldest) {
					pruned = append(pruned, c)
				}
			}
			m.committedLog = pruned
		} else {
			// No active transactions -- all entries can be pruned.
			m.committedLog = m.committedLog[:0]
		}
	}()

	// Prune old submit_times from SQLite (best-effort).
	evictBefore := m.factory.clock.Now().Add(-submitTimeTTL)
	_, _ = m.factory.db.ExecContext(ctx,
		"DELETE FROM submit_times WHERE submit_time < ?",
		timeToMicro(evictBefore))

	return nil
}

// flushToSQLite performs the atomic write of the transaction's buffered
// entities and deletes to SQLite within a single SQLite transaction.
func (m *transactionManager) flushToSQLite(ctx context.Context, tx *spi.TransactionState, submitTime time.Time, scheduledOps []scheduledTaskOp) error {
	sqlTx, err := m.factory.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sqlite tx: %w", err)
	}
	defer sqlTx.Rollback()

	submitMicro := timeToMicro(submitTime)
	tid := string(tx.TenantID)

	// Release unique-key claims held by entities deleted in THIS transaction
	// BEFORE flushing buffered creates/updates (which insert claims). Otherwise a
	// same-transaction "delete A holding value V, create B wanting V" sees A's
	// claim still present when B's is inserted and wrongly fails with a unique
	// violation. The whole flush is one sqlite tx, so this early release rolls
	// back atomically with everything else on any later error. releaseClaims is a
	// no-op (idempotent DELETE) for a delete target that holds no claims.
	for entityID := range tx.Deletes {
		if err := releaseClaims(ctx, sqlTx, tid, entityID); err != nil {
			return fmt.Errorf("release claims on delete %s: %w", entityID, err)
		}
	}

	// Flush buffered entities.
	for entityID, entity := range tx.Buffer {
		var existingVersion sql.NullInt64
		var existingCreatedAt sql.NullInt64
		err := sqlTx.QueryRowContext(ctx,
			"SELECT version, created_at FROM entities WHERE tenant_id = ? AND entity_id = ?",
			tid, entityID).Scan(&existingVersion, &existingCreatedAt)
		isNew := err == sql.ErrNoRows
		if err != nil && !isNew {
			return fmt.Errorf("check entity %s: %w", entityID, err)
		}

		var nextVersion int64
		changeType := "CREATED"
		createdAtMicro := submitMicro
		if !isNew {
			nextVersion = existingVersion.Int64 + 1
			changeType = "UPDATED"
			createdAtMicro = existingCreatedAt.Int64
		}

		// Preserve the entity's change type if explicitly set (e.g. by workflow).
		if entity.Meta.ChangeType != "" && entity.Meta.ChangeType != "CREATED" && !isNew {
			changeType = entity.Meta.ChangeType
		}

		entity.Meta.Version = nextVersion
		entity.Meta.LastModifiedDate = submitTime
		entity.Meta.TransactionID = tx.ID
		entity.Meta.ChangeType = changeType
		entity.Meta.TenantID = tx.TenantID
		if isNew {
			entity.Meta.CreationDate = submitTime
		} else {
			entity.Meta.CreationDate = microToTime(createdAtMicro)
		}

		metaJSON, err := marshalEntityMeta(&entity.Meta)
		if err != nil {
			return fmt.Errorf("marshal meta for %s: %w", entityID, err)
		}

		_, err = sqlTx.ExecContext(ctx,
			`INSERT OR REPLACE INTO entities
			 (tenant_id, entity_id, model_name, model_version, version, data, meta, deleted, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, jsonb(?), jsonb(?), 0, ?, ?)`,
			tid, entityID,
			entity.Meta.ModelRef.EntityName, entity.Meta.ModelRef.ModelVersion,
			nextVersion, string(entity.Data), string(metaJSON),
			createdAtMicro, submitMicro)
		if err != nil {
			return fmt.Errorf("upsert entity %s: %w", entityID, err)
		}

		_, err = sqlTx.ExecContext(ctx,
			`INSERT INTO entity_versions
			 (tenant_id, entity_id, model_name, model_version, version, data, meta, change_type, transaction_id, submit_time, user_id)
			 VALUES (?, ?, ?, ?, ?, jsonb(?), jsonb(?), ?, ?, ?, ?)`,
			tid, entityID,
			entity.Meta.ModelRef.EntityName, entity.Meta.ModelRef.ModelVersion,
			nextVersion, string(entity.Data), string(metaJSON),
			changeType, tx.ID, submitMicro,
			entity.Meta.ChangeUser)
		if err != nil {
			return fmt.Errorf("insert version %s: %w", entityID, err)
		}

		// Maintain unique-key claims using keys captured at Save (buffer) time.
		// Keys must not be read from ctx here: flushToSQLite runs once at Commit
		// with a single context (the last committer's), which would be wrong for
		// entities buffered with different key contexts in a mixed-model batch.
		keys := m.uniqueKeysFor(tx.ID, entityID)
		if err := replaceClaims(ctx, sqlTx, tid, entity, keys); err != nil {
			return fmt.Errorf("replace claims %s: %w", entityID, err)
		}
	}

	// Flush deletes.
	for entityID := range tx.Deletes {
		// Get current entity info for the delete version record.
		var curVersion int64
		var modelName, modelVersion string
		err := sqlTx.QueryRowContext(ctx,
			"SELECT version, model_name, model_version FROM entities WHERE tenant_id = ? AND entity_id = ?",
			tid, entityID).Scan(&curVersion, &modelName, &modelVersion)
		if err != nil {
			if err == sql.ErrNoRows {
				continue // Entity does not exist; skip.
			}
			return fmt.Errorf("check entity for delete %s: %w", entityID, err)
		}

		nextVersion := curVersion + 1

		_, err = sqlTx.ExecContext(ctx,
			"UPDATE entities SET deleted = 1, updated_at = ?, version = ? WHERE tenant_id = ? AND entity_id = ?",
			submitMicro, nextVersion, tid, entityID)
		if err != nil {
			return fmt.Errorf("soft delete entity %s: %w", entityID, err)
		}

		// Attribution: prefer tx.DeleteAttribution[entityID], captured at
		// stage time (the STAGER's context, under the same OpMu section
		// that set tx.Deletes[entityID] — see entityStore.Delete/DeleteAll).
		// Fall back to spi.AttributionFor(ctx) — this Commit call's own
		// ctx, i.e. the committer — only when no staged entry exists (a
		// caller that mutated tx.Deletes directly, bypassing EntityStore).
		// This is what fixes the prior bug: the tombstone's user_id column
		// was always written as '' — no actor at all, staged or committer.
		attribution, staged := tx.DeleteAttribution[entityID]
		if !staged {
			a, e := spi.AttributionFor(ctx)
			attribution = spi.WriteAttribution{Attributed: a, Executor: e}
		}
		tombstoneMeta, err := marshalTombstoneMeta(attribution.Attributed.Kind, attribution.Executor)
		if err != nil {
			return fmt.Errorf("marshal tombstone meta %s: %w", entityID, err)
		}

		_, err = sqlTx.ExecContext(ctx,
			`INSERT INTO entity_versions
			 (tenant_id, entity_id, model_name, model_version, version, data, meta, change_type, transaction_id, submit_time, user_id)
			 VALUES (?, ?, ?, ?, ?, NULL, jsonb(?), 'DELETED', ?, ?, ?)`,
			tid, entityID,
			modelName, modelVersion,
			nextVersion, string(tombstoneMeta), tx.ID, submitMicro,
			attribution.Attributed.ID)
		if err != nil {
			return fmt.Errorf("insert delete version %s: %w", entityID, err)
		}
		// (Claims for deleted entities are released in the pre-pass above, before
		// the buffer flush, so a same-tx delete+reclaim does not falsely conflict.)
	}

	// Record submit time.
	_, err = sqlTx.ExecContext(ctx,
		"INSERT OR REPLACE INTO submit_times (tx_id, submit_time) VALUES (?, ?)",
		tx.ID, submitMicro)
	if err != nil {
		return fmt.Errorf("record submit time: %w", err)
	}

	// Apply staged ScheduledTaskStore ops. Still inside sqlTx, which is what
	// makes the scheduled-task arm/cancel commit atomically with the entity
	// write above (and, symmetrically, why every early-return in this
	// function rolls the ops back too via the deferred sqlTx.Rollback()).
	for _, op := range scheduledOps {
		if err := applyScheduledTaskOp(ctx, sqlTx, op); err != nil {
			return fmt.Errorf("apply scheduled task op %s: %w", op.id, err)
		}
	}

	return sqlTx.Commit()
}

// Rollback discards an active transaction without committing any changes.
func (m *transactionManager) Rollback(ctx context.Context, txID string) error {
	uc := spi.GetUserContext(ctx)
	m.mu.Lock()
	tx, ok := m.active[txID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("Rollback: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if uc == nil || uc.Tenant.ID != tx.TenantID {
		m.mu.Unlock()
		return fmt.Errorf("Rollback: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
	}
	m.mu.Unlock()

	// Acquire transaction operation write lock -- waits for in-flight operations.
	tx.OpMu.Lock()
	defer func() {
		tx.Closed = true
		tx.OpMu.Unlock()
	}()

	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		tx.RolledBack = true
		delete(m.active, txID)
		delete(m.committing, txID)
		delete(m.savepoints, txID)
		delete(m.txUniqueKeys, txID)
		delete(m.scheduledTaskOps, txID) // discard staged ops unapplied — see field doc
	}()
	return nil
}

// GetSubmitTime returns the submit time of a committed transaction.
// Checks in-memory cache first, then falls back to the submit_times table.
func (m *transactionManager) GetSubmitTime(_ context.Context, txID string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.active[txID]; ok {
		return time.Time{}, fmt.Errorf("transaction not yet committed: %s", txID)
	}

	if t, ok := m.submitTimes[txID]; ok {
		return t, nil
	}

	// Fall back to persisted submit_times table.
	var micro int64
	err := m.factory.db.QueryRow(
		"SELECT submit_time FROM submit_times WHERE tx_id = ?", txID).Scan(&micro)
	if err != nil {
		return time.Time{}, fmt.Errorf("GetSubmitTime: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	return time.UnixMicro(micro), nil
}

// CommittedLogLen returns the current length of the committed log.
// Exported for testing only.
func (m *transactionManager) CommittedLogLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.committedLog)
}

// Savepoint creates a named savepoint within the given transaction by
// deep-copying the transaction's buffer maps.
//
// Locking discipline (issue #199 PR-C1, mirrors memory plugin PR-A):
// Savepoint reads tx.Buffer / tx.ReadSet / tx.WriteSet / tx.Deletes — the
// same fields Commit's flush phase iterates under tx.OpMu.Lock and that
// other tx-path ops mutate under tx.OpMu.RLock. Savepoint must therefore
// hold tx.OpMu.RLock across those reads. Lock interleaving with m.mu
// follows Commit's pattern: drop m.mu before taking tx.OpMu, re-take m.mu
// briefly for the m.savepoints update.
//
// Tenant isolation: rejects callers whose UserContext tenant does not
// match the transaction's tenant.
//
// NOTE: txUniqueKeys is intentionally not snapshotted here. Unique-key
// DEFINITIONS are static per model (set once at model-lock time, never
// mutated within a transaction), so a RollbackToSavepoint that reverts
// the buffer cannot produce a situation where the keys for a re-saved
// entity differ from the keys captured at the earlier Save call. The only
// scenario that would require snapshotting — the same entity re-saved with
// a different Fields set across a savepoint boundary — is not a supported
// pattern. RollbackToSavepoint therefore also leaves txUniqueKeys untouched.
func (m *transactionManager) Savepoint(ctx context.Context, txID string) (string, error) {
	uc := spi.GetUserContext(ctx)
	m.mu.Lock()
	tx, ok := m.active[txID]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if uc == nil || uc.Tenant.ID != tx.TenantID {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
	}

	tx.OpMu.RLock()
	defer tx.OpMu.RUnlock()

	if tx.RolledBack {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxRolledBack, txID)
	}
	if tx.Closed {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxAlreadyCommitted, txID)
	}

	spID := uuid.UUID(m.uuids.NewTimeUUID()).String()

	// Deep-copy the buffer maps under tx.OpMu.RLock so we are serialised
	// against Commit/Rollback (Lock).
	bufCopy := make(map[string]*spi.Entity, len(tx.Buffer))
	for k, v := range tx.Buffer {
		bufCopy[k] = copyEntity(v)
	}
	readCopy := make(map[string]bool, len(tx.ReadSet))
	for k, v := range tx.ReadSet {
		readCopy[k] = v
	}
	writeCopy := make(map[string]bool, len(tx.WriteSet))
	for k, v := range tx.WriteSet {
		writeCopy[k] = v
	}
	delCopy := make(map[string]bool, len(tx.Deletes))
	for k, v := range tx.Deletes {
		delCopy[k] = v
	}
	delAttrCopy := make(map[string]spi.WriteAttribution, len(tx.DeleteAttribution))
	for k, v := range tx.DeleteAttribution {
		delAttrCopy[k] = v
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.savepoints[txID] == nil {
		m.savepoints[txID] = make(map[string]savepointSnapshot)
	}
	m.savepoints[txID][spID] = savepointSnapshot{
		buffer:              bufCopy,
		readSet:             readCopy,
		writeSet:            writeCopy,
		deletes:             delCopy,
		deleteAttribution:   delAttrCopy,
		scheduledTaskOpsLen: len(m.scheduledTaskOps[txID]),
	}
	return spID, nil
}

// RollbackToSavepoint restores the transaction's buffer maps from the snapshot
// captured when the savepoint was created, then removes the snapshot.
//
// Locking discipline (issue #199 PR-C1): replaces tx.Buffer / tx.ReadSet /
// tx.WriteSet / tx.Deletes — exclusive against every other tx-path op.
// Holds tx.OpMu.Lock (write) for the duration of the field replacement.
//
// Tenant isolation: rejects mismatched-tenant callers — RollbackToSavepoint
// is destructive on tx-state.
func (m *transactionManager) RollbackToSavepoint(ctx context.Context, txID string, savepointID string) error {
	uc := spi.GetUserContext(ctx)
	m.mu.Lock()
	tx, ok := m.active[txID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("RollbackToSavepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if uc == nil || uc.Tenant.ID != tx.TenantID {
		return fmt.Errorf("RollbackToSavepoint: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
	}

	tx.OpMu.Lock()
	defer tx.OpMu.Unlock()

	if tx.RolledBack {
		return fmt.Errorf("RollbackToSavepoint: %w (txID=%s)", spi.ErrTxRolledBack, txID)
	}
	if tx.Closed {
		return fmt.Errorf("RollbackToSavepoint: %w (txID=%s)", spi.ErrTxAlreadyCommitted, txID)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	txSavepoints, ok := m.savepoints[txID]
	if !ok {
		return fmt.Errorf("RollbackToSavepoint: %w (txID=%s, savepointID=%s)", spi.ErrSavepointNotFound, txID, savepointID)
	}
	snap, ok := txSavepoints[savepointID]
	if !ok {
		return fmt.Errorf("RollbackToSavepoint: %w (txID=%s, savepointID=%s)", spi.ErrSavepointNotFound, txID, savepointID)
	}

	tx.Buffer = snap.buffer
	tx.ReadSet = snap.readSet
	tx.WriteSet = snap.writeSet
	tx.Deletes = snap.deletes
	tx.DeleteAttribution = snap.deleteAttribution

	// Truncate staged scheduled-task ops back to the length recorded at the
	// savepoint — append-only, so truncation (not replacement) is how it is
	// "restored". Clamp to the current length defensively: rolling back to a
	// savepoint ID whose recorded length exceeds what's currently staged
	// cannot happen via the normal linear-nesting flow, but truncating past
	// slice bounds would panic.
	if opsLen := snap.scheduledTaskOpsLen; opsLen < len(m.scheduledTaskOps[txID]) {
		m.scheduledTaskOps[txID] = m.scheduledTaskOps[txID][:opsLen]
	}

	delete(txSavepoints, savepointID)
	return nil
}

// ReleaseSavepoint releases a savepoint. The work done since the savepoint is
// already in the parent transaction's buffer, so this just removes the snapshot.
//
// Locking discipline (issue #199 PR-C1): does not touch any field of
// TransactionState — only mutates m.savepoints. Holds m.mu only;
// tx.OpMu is not required.
//
// Tenant isolation: rejects mismatched-tenant callers — m.savepoints is
// tenant-scoped state.
func (m *transactionManager) ReleaseSavepoint(ctx context.Context, txID string, savepointID string) error {
	uc := spi.GetUserContext(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()

	tx, ok := m.active[txID]
	if !ok {
		return fmt.Errorf("ReleaseSavepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if uc == nil || uc.Tenant.ID != tx.TenantID {
		return fmt.Errorf("ReleaseSavepoint: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
	}

	txSavepoints, ok := m.savepoints[txID]
	if !ok {
		return fmt.Errorf("ReleaseSavepoint: %w (txID=%s, savepointID=%s)", spi.ErrSavepointNotFound, txID, savepointID)
	}
	if _, ok := txSavepoints[savepointID]; !ok {
		return fmt.Errorf("ReleaseSavepoint: %w (txID=%s, savepointID=%s)", spi.ErrSavepointNotFound, txID, savepointID)
	}

	delete(txSavepoints, savepointID)
	return nil
}
