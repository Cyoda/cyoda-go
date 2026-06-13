package memory

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const submitTimeTTL = 1 * time.Hour

// committedTx records a committed transaction for SI+FCW conflict detection.
type committedTx struct {
	id         string
	submitTime time.Time
	writeSet   map[string]bool
}

// savepointSnapshot captures the state of a transaction's buffer maps at the
// time a savepoint is created. Used by RollbackToSavepoint to restore state.
type savepointSnapshot struct {
	buffer   map[string]*spi.Entity
	readSet  map[string]bool
	writeSet map[string]bool
	deletes  map[string]bool
}

// TransactionManager implements spi.TransactionManager using Snapshot Isolation
// with First-Committer-Wins (SI+FCW) — see docs/CONSISTENCY.md for the contract.
// It lives in the memory package because it needs direct access to StoreFactory's
// entityData map and mu lock for the atomic commit flush.
type TransactionManager struct {
	factory      *StoreFactory
	uuids        spi.UUIDGenerator
	mu           sync.Mutex // protects active, committedLog, committing, submitTimes, and savepoints
	active       map[string]*spi.TransactionState
	committedLog []committedTx
	committing   map[string]bool                         // tracks txIDs currently being committed
	submitTimes  map[string]time.Time                    // txID -> submitTime, survives log pruning. Evicted after submitTimeTTL.
	savepoints   map[string]map[string]savepointSnapshot // txID -> spID -> snapshot
}

// Verify interface compliance at compile time.
var _ spi.TransactionManager = (*TransactionManager)(nil)

// NewTransactionManager creates and registers a TransactionManager on the StoreFactory.
func (f *StoreFactory) NewTransactionManager(uuids spi.UUIDGenerator) *TransactionManager {
	tm := &TransactionManager{
		factory:      f,
		uuids:        uuids,
		active:       make(map[string]*spi.TransactionState),
		committedLog: nil,
		committing:   make(map[string]bool),
		submitTimes:  make(map[string]time.Time),
		savepoints:   make(map[string]map[string]savepointSnapshot),
	}
	f.txManager = tm
	return tm
}

// GetTransactionManager returns the registered TransactionManager, or nil.
func (f *StoreFactory) GetTransactionManager() spi.TransactionManager {
	if f.txManager == nil {
		return nil
	}
	return f.txManager
}

// Begin starts a new transaction. It resolves the tenant from the context,
// generates a unique transaction ID, captures a snapshot time, and returns
// a new context carrying the TransactionState.
func (m *TransactionManager) Begin(ctx context.Context) (string, context.Context, error) {
	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return "", ctx, fmt.Errorf("no user context — cannot begin transaction")
	}
	if uc.Tenant.ID == "" {
		return "", ctx, fmt.Errorf("user context has no tenant — cannot begin transaction")
	}

	txID := uuid.UUID(m.uuids.NewTimeUUID()).String()
	now := m.factory.clock.Now()

	tx := &spi.TransactionState{
		ID:           txID,
		TenantID:     uc.Tenant.ID,
		SnapshotTime: now,
		ReadSet:      make(map[string]bool),
		WriteSet:     make(map[string]bool),
		Buffer:       make(map[string]*spi.Entity),
		Deletes:      make(map[string]bool),
	}

	m.mu.Lock()
	m.active[txID] = tx
	m.mu.Unlock()

	txCtx := spi.WithTransaction(ctx, tx)
	return txID, txCtx, nil
}

// Join returns a context carrying the TransactionState for an existing active
// transaction. This allows multiple goroutines to participate in the same
// transaction. Callers must coordinate access to the transaction's Buffer,
// ReadSet, WriteSet, and Deletes maps.
//
// Locking discipline (issue #199 audit): Rollback writes tx.RolledBack
// inside m.mu only; Commit and Rollback both write tx.Closed in their
// defer under tx.OpMu.Lock only. Reading those fields requires
// tx.OpMu.RLock to be synchronised against the Closed-write — m.mu alone
// is not sufficient because Commit's defer runs outside the m.mu region.
func (m *TransactionManager) Join(ctx context.Context, txID string) (context.Context, error) {
	m.mu.Lock()
	tx, ok := m.active[txID]
	m.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}

	rolledBack, closed := func() (bool, bool) {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		return tx.RolledBack, tx.Closed
	}()
	if rolledBack {
		return nil, fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxRolledBack, txID)
	}
	if closed {
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
// flushes the write buffer and deletes to the entity store, and records the
// commit in the log.
func (m *TransactionManager) Commit(ctx context.Context, txID string) error {
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

	// 1b. Acquire transaction operation write lock — waits for in-flight operations.
	tx.OpMu.Lock()
	defer func() {
		tx.Closed = true
		tx.OpMu.Unlock()
	}()

	// 2. Acquire the factory write lock for atomic flush.
	m.factory.entityMu.Lock()

	// 3. Conflict detection: check committed log for overlapping write sets.
	m.mu.Lock()
	for _, committed := range m.committedLog {
		if committed.submitTime.After(tx.SnapshotTime) {
			for entityID := range committed.writeSet {
				if tx.ReadSet[entityID] || tx.WriteSet[entityID] {
					delete(m.committing, txID)
					delete(m.active, txID)
					delete(m.savepoints, txID)
					m.mu.Unlock()
					m.factory.entityMu.Unlock()
					return spi.ErrConflict
				}
			}
		}
	}
	m.mu.Unlock()

	// 4. Flush buffer to entity store.
	submitTime := m.factory.clock.Now()
	tid := tx.TenantID

	for entityID, entity := range tx.Buffer {
		if m.factory.entityData[tid] == nil {
			m.factory.entityData[tid] = make(map[string][]entityVersion)
		}

		versions := m.factory.entityData[tid][entityID]
		var nextVersion int64 = 1
		for i := len(versions) - 1; i >= 0; i-- {
			if !versions[i].deleted && versions[i].entity != nil {
				nextVersion = versions[i].entity.Meta.Version + 1
				break
			}
		}

		saved := copyEntity(entity)
		saved.Meta.Version = nextVersion
		saved.Meta.LastModifiedDate = submitTime
		saved.Meta.TransactionID = txID
		saved.Meta.TenantID = tid

		// Preserve CreationDate from existing versions.
		if len(versions) > 0 && versions[0].entity != nil {
			saved.Meta.CreationDate = versions[0].entity.Meta.CreationDate
		} else if saved.Meta.CreationDate.IsZero() {
			saved.Meta.CreationDate = submitTime
		}

		m.factory.entityData[tid][entityID] = append(versions, entityVersion{
			entity:        saved,
			transactionID: txID,
			submitTime:    submitTime,
			changeType:    entity.Meta.ChangeType,
			user:          entity.Meta.ChangeUser,
		})
	}

	// 5. Apply deletes.
	userName := ""
	if uc != nil {
		userName = uc.UserID
	}
	for entityID := range tx.Deletes {
		if m.factory.entityData[tid] == nil {
			m.factory.entityData[tid] = make(map[string][]entityVersion)
		}
		versions := m.factory.entityData[tid][entityID]
		m.factory.entityData[tid][entityID] = append(versions, entityVersion{
			entity:        nil,
			transactionID: txID,
			submitTime:    submitTime,
			deleted:       true,
			changeType:    "DELETED",
			user:          userName,
		})
	}

	// 6. Record in committed log, submit times, and prune.
	m.mu.Lock()
	m.committedLog = append(m.committedLog, committedTx{
		id:         txID,
		submitTime: submitTime,
		writeSet:   tx.WriteSet,
	})
	m.submitTimes[txID] = submitTime
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
		// No active transactions — all entries can be pruned.
		m.committedLog = m.committedLog[:0]
	}
	m.mu.Unlock()

	m.factory.entityMu.Unlock()

	return nil
}

// Rollback discards an active transaction without committing any changes.
func (m *TransactionManager) Rollback(ctx context.Context, txID string) error {
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

	// Acquire transaction operation write lock — waits for in-flight operations.
	tx.OpMu.Lock()
	defer func() {
		tx.Closed = true
		tx.OpMu.Unlock()
	}()

	m.mu.Lock()
	tx.RolledBack = true
	delete(m.active, txID)
	delete(m.committing, txID)
	delete(m.savepoints, txID)
	m.mu.Unlock()
	return nil
}

// GetSubmitTime returns the submit time of a committed transaction.
// Returns an error if the transaction is still active or not found.
func (m *TransactionManager) GetSubmitTime(_ context.Context, txID string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.active[txID]; ok {
		return time.Time{}, fmt.Errorf("transaction not yet committed: %s", txID)
	}

	if t, ok := m.submitTimes[txID]; ok {
		return t, nil
	}

	return time.Time{}, fmt.Errorf("GetSubmitTime: %w (txID=%s)", spi.ErrTxNotFound, txID)
}

// CommittedLogLen returns the current length of the committed log.
// Exported for testing only.
func (m *TransactionManager) CommittedLogLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.committedLog)
}

// Savepoint creates a named savepoint within the given transaction by
// deep-copying the transaction's buffer maps.
//
// Locking discipline (issue #199): Savepoint reads tx.Buffer / tx.ReadSet /
// tx.WriteSet / tx.Deletes — the same fields Commit's flush phase iterates
// under tx.OpMu.Lock and that other tx-path ops (Save, Get, Delete, ...)
// mutate under tx.OpMu.RLock. Savepoint must therefore hold tx.OpMu.RLock
// across those reads. The lock interleaving with m.mu follows Commit's
// pattern: drop m.mu before taking tx.OpMu, re-take m.mu briefly for the
// m.savepoints update.
//
// Tenant isolation (issue #199 PR-A review I-1): rejects callers whose
// UserContext tenant does not match the transaction's tenant, mirroring
// Commit/Rollback. Without this guard a caller authenticated as tenant A
// who learned a tenant B txID could record a snapshot against tenant B's
// tx-state.
func (m *TransactionManager) Savepoint(ctx context.Context, txID string) (string, error) {
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

	// Commit and Rollback set tx.Closed/RolledBack inside their tx.OpMu.Lock
	// region; once we hold tx.OpMu.RLock those flags are stable and reading
	// them tells us whether the tx was closed during our OpMu acquisition.
	if tx.RolledBack {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxRolledBack, txID)
	}
	if tx.Closed {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxAlreadyCommitted, txID)
	}

	spID := uuid.UUID(m.uuids.NewTimeUUID()).String()

	// Deep-copy the buffer maps under tx.OpMu.RLock so we are serialised
	// against Commit/Rollback (Lock). Per the Join() godoc the application
	// is responsible for serialising its own concurrent ops on a single tx,
	// so concurrent Save+Savepoint is an application contract violation,
	// not a plugin defect — RLock here intentionally allows other readers.
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

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.savepoints[txID] == nil {
		m.savepoints[txID] = make(map[string]savepointSnapshot)
	}
	m.savepoints[txID][spID] = savepointSnapshot{
		buffer:   bufCopy,
		readSet:  readCopy,
		writeSet: writeCopy,
		deletes:  delCopy,
	}
	return spID, nil
}

// RollbackToSavepoint restores the transaction's buffer maps from the snapshot
// captured when the savepoint was created, then removes the snapshot.
//
// Locking discipline (issue #199): RollbackToSavepoint replaces tx.Buffer /
// tx.ReadSet / tx.WriteSet / tx.Deletes — exclusive against every other
// tx-path op. Holds tx.OpMu.Lock (write) for the duration of the field
// replacement. Lock interleaving with m.mu follows Commit's pattern.
//
// Tenant isolation (issue #199 PR-A review I-1): rejects cross-tenant
// callers — RollbackToSavepoint is destructive on tx-state.
func (m *TransactionManager) RollbackToSavepoint(ctx context.Context, txID string, savepointID string) error {
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

	delete(txSavepoints, savepointID)
	return nil
}

// ReleaseSavepoint releases a savepoint. The work done since the savepoint is
// already in the parent transaction's buffer, so this just removes the snapshot.
//
// Locking discipline (issue #199): ReleaseSavepoint does not read or write
// tx.Buffer / tx.ReadSet / tx.WriteSet / tx.Deletes — it only mutates
// m.savepoints. Holds m.mu only; tx.OpMu is not required because there is
// no tx-state field to coordinate against Commit/Rollback.
//
// Tenant isolation (issue #199 PR-A review I-1): rejects cross-tenant
// callers — m.savepoints is tenant-scoped state.
func (m *TransactionManager) ReleaseSavepoint(ctx context.Context, txID string, savepointID string) error {
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
