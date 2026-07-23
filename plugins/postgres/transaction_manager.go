package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const submitTimeTTL = 1 * time.Hour

// TransactionManager implements spi.TransactionManager backed by PostgreSQL
// with REPEATABLE READ isolation plus application-layer row-granular
// first-committer-wins validation. Each Begin() acquires a real pgx.Tx,
// registers it in the txRegistry, and allocates a *txState for read/write
// bookkeeping used by Commit.
type TransactionManager struct {
	pool     *pgxpool.Pool
	registry *txRegistry
	uuids    spi.UUIDGenerator
	mu       sync.Mutex
	// submitTimes records the database timestamp captured at commit time.
	// Evicted after submitTimeTTL.
	submitTimes map[string]time.Time
	// tenants records the tenant for each active transaction so Join can
	// reconstruct the TransactionState without requiring tenant in the
	// joining context.
	tenants map[string]spi.TenantID
	// origins records the attribution root (spi.TransactionState.Origin)
	// captured at Begin for each active transaction. Unlike memory/sqlite,
	// which share a single *spi.TransactionState pointer across Join calls,
	// postgres rebuilds a brand-new TransactionState{ID,TenantID} on every
	// Join (see Join below) — without this map, that rebuild would silently
	// drop Origin, breaking cross-node/cross-goroutine cascade attribution
	// (the primary acceptance criterion of the follow-on-action attribution
	// feature). Populated at Begin, read at Join, deleted at Commit/
	// Rollback — same lifecycle and mutex (tm.mu) as tenants.
	origins    map[string]spi.Principal
	txStatesMu sync.RWMutex
	txStates   map[string]*txState
}

// NewTransactionManager creates a new PostgreSQL-backed TransactionManager.
func NewTransactionManager(pool *pgxpool.Pool, uuids spi.UUIDGenerator) *TransactionManager {
	return &TransactionManager{
		pool:        pool,
		registry:    newTxRegistry(),
		uuids:       uuids,
		submitTimes: make(map[string]time.Time),
		tenants:     make(map[string]spi.TenantID),
		origins:     make(map[string]spi.Principal),
		txStates:    make(map[string]*txState),
	}
}

// Begin starts a new REPEATABLE READ transaction (snapshot isolation) and
// returns the transaction ID and a context carrying the TransactionState.
//
// Row-granular first-committer-wins is enforced in application code via
// txState bookkeeping (readSet/writeSet) and commit-time validation — see
// Commit() and docs/superpowers/specs/2026-04-15-postgres-si-first-committer-wins-design.md.
func (tm *TransactionManager) Begin(ctx context.Context) (string, context.Context, error) {
	tenantID, err := resolveTenant(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("Begin: %w", err)
	}

	txID := uuid.UUID(tm.uuids.NewTimeUUID()).String()

	pgxTx, err := tm.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return "", nil, fmt.Errorf("Begin: failed to start transaction: %w", err)
	}

	// Set the current tenant for RLS policies. We use set_config(name, value, is_local)
	// rather than `SET LOCAL app.current_tenant = $1` because PostgreSQL's SET statement
	// does not accept bound parameters under pgx's extended-query protocol.
	if _, err := pgxTx.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", string(tenantID)); err != nil {
		_ = pgxTx.Rollback(ctx)
		return "", nil, fmt.Errorf("Begin: failed to set tenant: %w", err)
	}

	tm.registry.Register(txID, pgxTx)

	// Origin: the attribution root for the whole tx, per ResolveOrigin's
	// documented precedence (parent-tx > ambient > UserContext). Resolved
	// from the Begin caller's ctx and stored in tm.origins so Join can
	// repopulate it later (see the origins field godoc above).
	origin := spi.ResolveOrigin(ctx)

	func() {
		tm.mu.Lock()
		defer tm.mu.Unlock()
		tm.tenants[txID] = tenantID
		tm.origins[txID] = origin
	}()

	func() {
		tm.txStatesMu.Lock()
		defer tm.txStatesMu.Unlock()
		tm.txStates[txID] = newTxState(tenantID)
	}()

	// ReadSet/WriteSet/Buffer/Deletes/DeleteAttribution are left nil:
	// postgres's own persistence never reads them back (see
	// EntityStore.Delete and Search's documented assumption in searcher.go)
	// — real row visibility is governed by PostgreSQL's own transaction/
	// SAVEPOINT machinery, not an in-process buffer. The SPI conformance
	// contract is the committed outcome (GetVersionHistory), never these
	// maps' contents.
	txSpiState := &spi.TransactionState{
		ID:       txID,
		TenantID: tenantID,
		Origin:   origin,
	}

	return txID, spi.WithTransaction(ctx, txSpiState), nil
}

// Commit commits the transaction and records its submit time.
// Returns spi.ErrConflict on serialization failure (PostgreSQL error 40001)
// or when the application-layer first-committer-wins validation detects a
// stale read or write set.
//
// Tenant isolation (issue #199 PR-C2): rejects callers whose UserContext
// tenant does not match the transaction's tenant. RLS protects data-path
// access (every DML is row-level filtered) but does not extend to
// transaction-lifecycle commands (BEGIN/COMMIT/ROLLBACK/SAVEPOINT/etc.) —
// those operate on connections and don't trigger any policy. So the
// application-layer tenant gate is the only protection against a caller
// authenticated as tenant A committing tenant B's in-flight work.
func (tm *TransactionManager) Commit(ctx context.Context, txID string) error {
	pgxTx, ok := tm.registry.Lookup(txID)
	if !ok {
		return fmt.Errorf("Commit: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	state, ok := tm.lookupTxState(txID)
	if !ok {
		return fmt.Errorf("Commit: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if err := verifyTenant(ctx, state.tenantID, "Commit", txID); err != nil {
		return err
	}

	// --- First-committer-wins validation (read-set) ---
	// Re-read the current committed versions of all entities we read in this tx
	// and compare against the captured readSet. A version mismatch or missing row
	// means a concurrent committer changed data we made decisions on; abort with
	// ErrConflict so the caller can retry on a fresh snapshot.
	//
	// Write-write conflicts (writeSet) are handled by PostgreSQL's own tuple-level
	// locks from the DML statements (INSERT/UPDATE/DELETE) — they raise SQLSTATE
	// 40001 at DML time or commit time, which classifyError maps to ErrConflict.
	// We do NOT validate writeSet versions here: the validateInChunks query runs
	// inside the current transaction and therefore sees the tx's own uncommitted
	// writes, making writeSet version comparison unreliable.
	readIDs := state.SortedReadIDs()
	if len(readIDs) > 0 {
		current, err := tm.validateInChunks(ctx, pgxTx, state.tenantID, readIDs, 0)
		if err != nil {
			tm.cleanupTx(txID)
			_ = pgxTx.Rollback(context.Background())
			return classifyError(fmt.Errorf("Commit: validate: %w", err))
		}
		if verr := state.ValidateReadSet(current); verr != nil {
			tm.cleanupTx(txID)
			_ = pgxTx.Rollback(context.Background())
			return fmt.Errorf("%w: Commit: %w", spi.ErrConflict, verr)
		}
	}

	// Capture the database timestamp before committing.
	// If the transaction is already in an aborted state (e.g. an earlier Exec
	// returned 40001 and left the tx aborted), the SELECT will fail with
	// SQLSTATE 25P02 (in_failed_sql_transaction). In that case we rollback
	// and surface ErrConflict, since the abort was most likely caused by a
	// serialization failure. We use time.Now() as a stand-in; it is never
	// stored on an error path.
	var submitTime time.Time
	if tsErr := pgxTx.QueryRow(ctx, "SELECT CURRENT_TIMESTAMP").Scan(&submitTime); tsErr != nil {
		tm.cleanupTx(txID)
		// Only classify as ErrConflict when the probe fails specifically because
		// the transaction is already in an aborted state (SQLSTATE 25P02:
		// in_failed_sql_transaction). Any other error (context cancellation,
		// network failure, etc.) is returned as-is so callers are not misled
		// into treating a transient infrastructure error as a retryable conflict.
		var pgErr *pgconn.PgError
		if errors.As(tsErr, &pgErr) && pgErr.Code == pgerrcode.InFailedSQLTransaction {
			_ = pgxTx.Rollback(context.Background())
			return fmt.Errorf("%w: Commit: transaction aborted: %w", spi.ErrConflict, tsErr)
		}
		// For non-25P02 errors (e.g. network failures, context deadline exceeded)
		// roll back with a fresh context so we don't leak the connection, then
		// return the raw error without wrapping it as ErrConflict.
		_ = pgxTx.Rollback(context.Background())
		return fmt.Errorf("Commit: failed to capture submit time: %w", tsErr)
	}

	if err := pgxTx.Commit(ctx); err != nil {
		// On commit failure the transaction is already aborted server-side, but
		// the pgx.Tx still holds the connection. Rollback explicitly to release
		// it back to the pool; ignore the rollback error (tx is already invalid).
		_ = pgxTx.Rollback(ctx)
		tm.cleanupTx(txID)
		return classifyError(fmt.Errorf("Commit: %w", err))
	}

	tm.cleanupTx(txID)

	func() {
		tm.mu.Lock()
		defer tm.mu.Unlock()
		tm.submitTimes[txID] = submitTime
		evictBefore := time.Now().Add(-submitTimeTTL)
		for id, t := range tm.submitTimes {
			if t.Before(evictBefore) {
				delete(tm.submitTimes, id)
			}
		}
	}()

	return nil
}

// Rollback aborts the transaction.
//
// Tenant isolation (issue #199 PR-C2): rejects mismatched-tenant callers.
// See Commit's godoc for the design rationale.
func (tm *TransactionManager) Rollback(ctx context.Context, txID string) error {
	pgxTx, ok := tm.registry.Lookup(txID)
	if !ok {
		return fmt.Errorf("Rollback: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}

	tenantID, ok := tm.lookupTenant(txID)
	if !ok {
		return fmt.Errorf("Rollback: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if err := verifyTenant(ctx, tenantID, "Rollback", txID); err != nil {
		return err
	}

	err := pgxTx.Rollback(ctx)
	tm.cleanupTx(txID)

	if err != nil {
		return fmt.Errorf("Rollback: %w", err)
	}
	return nil
}

// Join attaches to an existing transaction, returning a context carrying its
// TransactionState.
//
// Tenant isolation (issue #199 PR-C2): rejects mismatched-tenant callers.
// Returning a context for another tenant's tx would let the joining caller
// drive arbitrary lifecycle operations on that tx — see Commit's godoc.
func (tm *TransactionManager) Join(ctx context.Context, txID string) (context.Context, error) {
	_, ok := tm.registry.Lookup(txID)
	if !ok {
		return nil, fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}

	tenantID, ok := tm.lookupTenant(txID)
	if !ok {
		return nil, fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if err := verifyTenant(ctx, tenantID, "Join", txID); err != nil {
		return nil, err
	}

	// Join rebuilds TransactionState from scratch (postgres does not share
	// a single *spi.TransactionState pointer across Join calls the way
	// memory/sqlite do) — Origin MUST be repopulated from tm.origins here,
	// or a joined caller's writes silently lose attribution to the tx's
	// causal root. This is the load-bearing case for cross-node/cross-
	// goroutine cascade attribution; see the origins field godoc.
	origin, ok := tm.lookupOrigin(txID)
	if !ok {
		return nil, fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}

	// ReadSet/WriteSet/Buffer/Deletes/DeleteAttribution are left nil on the
	// rebuilt TransactionState too — same rationale as Begin above.
	txState := &spi.TransactionState{
		ID:       txID,
		TenantID: tenantID,
		Origin:   origin,
	}
	return spi.WithTransaction(ctx, txState), nil
}

// GetSubmitTime returns the database timestamp recorded when the transaction
// was committed.
func (tm *TransactionManager) GetSubmitTime(_ context.Context, txID string) (time.Time, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	t, ok := tm.submitTimes[txID]
	if !ok {
		return time.Time{}, fmt.Errorf("GetSubmitTime: transaction %s has no submit time (not yet committed or unknown)", txID)
	}
	return t, nil
}

// LookupTx exposes the registry lookup for use in tests and by the store
// layer (resolveQuerier). Production code should prefer resolveQuerier.
func (tm *TransactionManager) LookupTx(txID string) (pgx.Tx, bool) {
	return tm.registry.Lookup(txID)
}

// cleanupTx removes all per-transaction state (registry, tenant, txState).
// Called on every Commit/Rollback exit path.
func (tm *TransactionManager) cleanupTx(txID string) {
	tm.registry.Remove(txID)
	tm.removeTenant(txID)
	tm.removeOrigin(txID)
	tm.removeTxState(txID)
}

// removeTenant cleans up the tenant mapping for a completed transaction.
func (tm *TransactionManager) removeTenant(txID string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	delete(tm.tenants, txID)
}

// removeOrigin cleans up the origin mapping for a completed transaction.
// Called by cleanupTx on every Commit/Rollback exit path so no per-tx origin
// entry ever leaks past the transaction's lifetime.
func (tm *TransactionManager) removeOrigin(txID string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	delete(tm.origins, txID)
}

// lookupOrigin returns the origin recorded for a transaction at Begin, or
// false if the txID is not active. Used by Join to repopulate Origin on the
// freshly rebuilt TransactionState.
func (tm *TransactionManager) lookupOrigin(txID string) (spi.Principal, bool) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	origin, ok := tm.origins[txID]
	return origin, ok
}

// removeTxState removes the txState entry for a completed transaction.
func (tm *TransactionManager) removeTxState(txID string) {
	tm.txStatesMu.Lock()
	defer tm.txStatesMu.Unlock()
	delete(tm.txStates, txID)
}

// lookupTxState returns the txState for the given txID.
func (tm *TransactionManager) lookupTxState(txID string) (*txState, bool) {
	tm.txStatesMu.RLock()
	defer tm.txStatesMu.RUnlock()
	s, ok := tm.txStates[txID]
	return s, ok
}

// Savepoint creates a named savepoint within the given PostgreSQL transaction
// and pushes a snapshot of the current readSet/writeSet onto the txState stack.
//
// Tenant isolation (issue #199 PR-C2): rejects mismatched-tenant callers.
func (tm *TransactionManager) Savepoint(ctx context.Context, txID string) (string, error) {
	pgxTx, ok := tm.registry.Lookup(txID)
	if !ok {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	state, ok := tm.lookupTxState(txID)
	if !ok {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if err := verifyTenant(ctx, state.tenantID, "Savepoint", txID); err != nil {
		return "", err
	}
	spID := uuid.UUID(tm.uuids.NewTimeUUID()).String()
	spName := "sp_" + spID
	if _, err := pgxTx.Exec(ctx, "SAVEPOINT "+pgx.Identifier{spName}.Sanitize()); err != nil {
		return "", fmt.Errorf("Savepoint: %w", err)
	}

	state.PushSavepoint(spID)
	return spID, nil
}

// RollbackToSavepoint rolls back all work done since the named savepoint and
// restores the txState readSet/writeSet to the snapshot captured at that savepoint.
//
// Tenant isolation (issue #199 PR-C2): rejects mismatched-tenant callers —
// destructive on tx-state.
func (tm *TransactionManager) RollbackToSavepoint(ctx context.Context, txID string, savepointID string) error {
	pgxTx, ok := tm.registry.Lookup(txID)
	if !ok {
		return fmt.Errorf("RollbackToSavepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	state, ok := tm.lookupTxState(txID)
	if !ok {
		return fmt.Errorf("RollbackToSavepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if err := verifyTenant(ctx, state.tenantID, "RollbackToSavepoint", txID); err != nil {
		return err
	}
	// Validate the savepoint exists in the in-memory snapshot stack BEFORE
	// issuing the SQL command. PostgreSQL would surface a missing savepoint as
	// SQLSTATE 3B001, which is opaque to errors.Is(err, spi.ErrSavepointNotFound).
	// Checking first guarantees the SPI sentinel is wrapped consistently
	// regardless of whether the txState snapshot stack and the DB savepoint
	// stack ever diverge (they shouldn't, but the contract is the sentinel).
	if !state.HasSavepoint(savepointID) {
		return fmt.Errorf("RollbackToSavepoint: %w (txID=%s, savepointID=%s)", spi.ErrSavepointNotFound, txID, savepointID)
	}
	spName := "sp_" + savepointID
	if _, err := pgxTx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+pgx.Identifier{spName}.Sanitize()); err != nil {
		return fmt.Errorf("RollbackToSavepoint: %w", err)
	}
	if err := state.RestoreSavepoint(savepointID); err != nil {
		return fmt.Errorf("RollbackToSavepoint: %w", err)
	}
	return nil
}

// ReleaseSavepoint releases a savepoint, merging its work into the parent transaction.
// The txState snapshot for this savepoint is dropped; work done after the push is kept.
//
// Tenant isolation (issue #199 PR-C2): rejects mismatched-tenant callers.
func (tm *TransactionManager) ReleaseSavepoint(ctx context.Context, txID string, savepointID string) error {
	pgxTx, ok := tm.registry.Lookup(txID)
	if !ok {
		return fmt.Errorf("ReleaseSavepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	state, ok := tm.lookupTxState(txID)
	if !ok {
		return fmt.Errorf("ReleaseSavepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if err := verifyTenant(ctx, state.tenantID, "ReleaseSavepoint", txID); err != nil {
		return err
	}
	// Validate the savepoint exists before issuing the SQL command — see
	// RollbackToSavepoint for rationale.
	if !state.HasSavepoint(savepointID) {
		return fmt.Errorf("ReleaseSavepoint: %w (txID=%s, savepointID=%s)", spi.ErrSavepointNotFound, txID, savepointID)
	}
	spName := "sp_" + savepointID
	if _, err := pgxTx.Exec(ctx, "RELEASE SAVEPOINT "+pgx.Identifier{spName}.Sanitize()); err != nil {
		return fmt.Errorf("ReleaseSavepoint: %w", err)
	}
	if err := state.ReleaseSavepoint(savepointID); err != nil {
		return fmt.Errorf("ReleaseSavepoint: %w", err)
	}
	return nil
}

// lookupTenant returns the tenant recorded for a transaction, or false if
// the txID is not active. Used by Rollback / Join where a txState lookup
// is not otherwise needed.
func (tm *TransactionManager) lookupTenant(txID string) (spi.TenantID, bool) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tenantID, ok := tm.tenants[txID]
	return tenantID, ok
}

// verifyTenant compares the caller's UserContext tenant against the
// transaction's tenant. Returns a "tenant mismatch" error on mismatch
// or when no UserContext is present. Mirrors the pattern used by the
// memory and sqlite plugins for application-layer tenant gating on
// TM lifecycle methods.
//
// RLS (PostgreSQL row-level security) protects data-path access but does
// NOT extend to transaction-lifecycle commands (BEGIN/COMMIT/ROLLBACK/
// SAVEPOINT/etc.) — those operate on connections and don't trigger any
// policy. The application-layer check is the only enforcement against a
// caller authenticated as tenant A driving lifecycle operations on
// tenant B's in-flight transaction.
func verifyTenant(ctx context.Context, txTenantID spi.TenantID, op string, txID string) error {
	uc := spi.GetUserContext(ctx)
	if uc == nil || uc.Tenant.ID != txTenantID {
		return fmt.Errorf("%s: %w (txID=%s)", op, spi.ErrTxTenantMismatch, txID)
	}
	return nil
}

// classifyError maps PostgreSQL errors that mean "this transaction was fully
// rolled back by the database before any external work stuck — a retry on a
// fresh snapshot is safe" to spi.ErrConflict. Everything else passes through.
//
// Retryable codes:
//   - serialization_failure (40001) — under REPEATABLE READ, raised when a
//     concurrent committer has already modified a row this tx is updating
//     (PostgreSQL: "could not serialize access due to concurrent update")
//   - deadlock_detected (40P01) — deadlock victim chosen by the server
//
// Both sentinels stay reachable: spi.ErrConflict satisfies handler-level
// errors.Is checks, and the original *pgconn.PgError stays in the chain so
// observability and logging can type-assert via errors.As.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == pgerrcode.SerializationFailure || pgErr.Code == pgerrcode.DeadlockDetected:
			return fmt.Errorf("%w: %w", spi.ErrConflict, err)
		case pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == "unique_claims_uq":
			return fmt.Errorf("%w: %w", spi.ErrUniqueViolation, err)
		}
	}
	return err
}
