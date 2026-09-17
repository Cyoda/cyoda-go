package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const submitTimeTTL = 1 * time.Hour

// submitTimePruneInterval rate-limits the submit_times table housekeeping
// delete (see pruneSubmitTimes) to at most once per interval, rather than
// running it on every commit.
const submitTimePruneInterval = 5 * time.Minute

// submitTimeEntry pairs a committed transaction's submit time with the
// tenant that owns it, so GetSubmitTime can enforce the same tenant gate
// as every other tx-lifecycle method after cleanupTx has removed the
// active-tx state.
type submitTimeEntry struct {
	submitTime time.Time
	tenantID   spi.TenantID
}

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
	// submitTimes records the database timestamp captured at commit time,
	// paired with the owning tenant. Evicted after submitTimeTTL.
	submitTimes map[string]submitTimeEntry
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
	// acquireTimeout bounds Begin's wait for a pooled connection.
	acquireTimeout time.Duration
	// lastSubmitTimePruneNano rate-limits pruneSubmitTimes (UnixNano since
	// epoch; zero means "never pruned"). Accessed without tm.mu: it gates an
	// independent housekeeping statement on the pool, not the maps tm.mu
	// protects, so a plain atomic keeps the common (skip) path lock-free.
	lastSubmitTimePruneNano atomic.Int64
}

// TransactionManagerOption configures a TransactionManager at construction.
type TransactionManagerOption func(*TransactionManager)

// WithAcquireTimeout bounds how long Begin waits for a pooled connection before
// failing with the storage-unavailable marker. Zero disables the deadline,
// matching the convention the GUC ceilings use. Production wiring passes
// cfg.AcquireTimeout (CYODA_POSTGRES_ACQUIRE_TIMEOUT); without this option the
// manager still gets the shipped default rather than an unbounded wait.
func WithAcquireTimeout(d time.Duration) TransactionManagerOption {
	return func(tm *TransactionManager) { tm.acquireTimeout = d }
}

// NewTransactionManager creates a new PostgreSQL-backed TransactionManager.
func NewTransactionManager(pool *pgxpool.Pool, uuids spi.UUIDGenerator, opts ...TransactionManagerOption) *TransactionManager {
	tm := &TransactionManager{
		pool:           pool,
		registry:       newTxRegistry(),
		uuids:          uuids,
		submitTimes:    make(map[string]submitTimeEntry),
		tenants:        make(map[string]spi.TenantID),
		origins:        make(map[string]spi.Principal),
		txStates:       make(map[string]*txState),
		acquireTimeout: defaultAcquireTimeout,
	}
	for _, apply := range opts {
		apply(tm)
	}
	return tm
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

	// The deadline bounds the acquire ONLY. It must not reach the context this
	// function returns: that one is derived from the caller's ctx below and
	// carries the transaction for its whole life, so a deadline on it would
	// cancel the transaction the moment the acquire window closed.
	//
	// pool.BeginTx and the set_config round-trip both return before the caller
	// touches the transaction, so bounding them leaks nothing into the handle.
	acquireCtx, cancelAcquire := tm.acquireContext(ctx)
	defer cancelAcquire()

	pgxTx, err := tm.pool.BeginTx(acquireCtx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return "", nil, classifyAcquireErr(ctx, acquireCtx, "Begin: failed to start transaction", err)
	}

	// Set the current tenant for RLS policies. We use set_config(name, value, is_local)
	// rather than `SET LOCAL app.current_tenant = $1` because PostgreSQL's SET statement
	// does not accept bound parameters under pgx's extended-query protocol.
	if _, err := pgxTx.Exec(acquireCtx, "SELECT set_config('app.current_tenant', $1, true)", string(tenantID)); err != nil {
		// The rollback runs on a context derived WithoutCancel: acquireCtx may be
		// the very thing that just expired, and a rollback on an expired context
		// destroys the pooled connection instead of returning it.
		_ = pgxTx.Rollback(context.WithoutCancel(ctx))
		return "", nil, classifyAcquireErr(ctx, acquireCtx, "Begin: failed to set tenant", err)
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
	// contract is the committed outcome (GetPage, GetVersionByTransaction,
	// GetVersionMetadata), never these maps' contents.
	txSpiState := &spi.TransactionState{
		ID:       txID,
		TenantID: tenantID,
		Origin:   origin,
	}

	return txID, spi.WithTransaction(ctx, txSpiState), nil // derived from the CALLER's ctx
}

// acquireContext returns the acquire-only deadline context for this manager.
// See newAcquireContext for why it must never reach the transaction handle.
func (tm *TransactionManager) acquireContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return newAcquireContext(ctx, tm.acquireTimeout)
}

// Commit commits the transaction and records its submit time.
// Returns spi.ErrConflict on serialization failure (PostgreSQL error 40001)
// or when the application-layer first-committer-wins validation detects a
// stale read or write set.
//
// Tenant isolation: rejects callers whose UserContext
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
			return tm.classifyTxError(txID, fmt.Errorf("Commit: validate: %w", err))
		}
		if verr := state.ValidateReadSet(current); verr != nil {
			tm.cleanupTx(txID)
			_ = pgxTx.Rollback(context.Background())
			return fmt.Errorf("%w: Commit: %w", spi.ErrConflict, verr)
		}
	}

	// Fix the transaction's instant and stamp it onto every row the
	// transaction wrote, immediately before COMMIT.
	//
	// If the transaction is already in an aborted state (e.g. an earlier Exec
	// returned 40001 and left the tx aborted), the first statement of the
	// stamp will fail with SQLSTATE 25P02 (in_failed_sql_transaction). In that
	// case we rollback and surface ErrConflict, since the abort was most
	// likely caused by a serialization failure — the same classification the
	// bare timestamp probe this replaced already had.
	submitTime, tsErr := tm.stampCommitInstant(ctx, pgxTx, state.tenantID, txID)
	if tsErr != nil {
		tm.cleanupTx(txID)
		// Only classify as ErrConflict when the probe fails specifically because
		// the transaction is already in an aborted state (SQLSTATE 25P02:
		// in_failed_sql_transaction). Any other error (context cancellation,
		// network failure, etc.) is returned as-is so callers are not misled
		// into treating a transient infrastructure error as a retryable conflict.
		var pgErr *pgconn.PgError
		if errors.As(tsErr, &pgErr) && pgErr.Code == pgerrcode.InFailedSQLTransaction {
			_ = pgxTx.Rollback(context.Background())
			// 25P02 says only "something earlier in this transaction failed".
			// When that something was a ceiling, classifyTxError recorded it,
			// and reporting the real cause is what keeps a cancelled statement
			// off the retryable-conflict path — a retry would cancel again.
			if cause := state.AbortCause(); cause != nil {
				return fmt.Errorf("Commit: transaction aborted: %w", cause)
			}
			return fmt.Errorf("%w: Commit: transaction aborted: %w", spi.ErrConflict, tsErr)
		}
		// For non-25P02 errors: roll back with a fresh context so we don't leak
		// the connection, then classify before returning. classifyError only
		// reclassifies specific SQLSTATEs (40001/40P01 to spi.ErrConflict,
		// transport-loss shapes to the idle-in-tx marker) and passes everything
		// else through unchanged, so a genuine transient infrastructure error
		// (context cancellation, network failure) still isn't misreported as
		// retryable. Every statement in stampCommitInstant is currently scoped
		// to this transaction's own rows and so shouldn't itself raise 40001 —
		// but that scoping is a property of the SQL text, not something this
		// error path can verify, so classification stays generic rather than
		// assuming today's statements are the only ones this function will
		// ever run.
		_ = pgxTx.Rollback(context.Background())
		return fmt.Errorf("Commit: failed to stamp the commit instant: %w", classifyError(tsErr))
	}

	if err := pgxTx.Commit(ctx); err != nil {
		// On commit failure the transaction is already aborted server-side, but
		// the pgx.Tx still holds the connection. Rollback explicitly to release
		// it back to the pool; ignore the rollback error (tx is already invalid).
		_ = pgxTx.Rollback(ctx)
		tm.cleanupTx(txID)
		return tm.classifyCommitError(txID, fmt.Errorf("Commit: %w", err))
	}

	// Table housekeeping, deliberately AFTER commit and on the pool, never
	// inside pgxTx — see pruneSubmitTimes for why sharing this transaction's
	// snapshot would let unrelated tenants' commits abort each other.
	tm.pruneSubmitTimes(ctx)

	// Record the submit time BEFORE cleanupTx removes the active-tx state:
	// a concurrent GetSubmitTime racing this Commit then observes the tx in
	// at least one of the two maps at all times (its committed branch wins
	// during the overlap), never a transient "neither map" ErrTxNotFound
	// for a transaction that just committed successfully.
	func() {
		tm.mu.Lock()
		defer tm.mu.Unlock()
		tm.submitTimes[txID] = submitTimeEntry{submitTime: submitTime, tenantID: state.tenantID}
		evictBefore := time.Now().Add(-submitTimeTTL)
		for id, e := range tm.submitTimes {
			if e.submitTime.Before(evictBefore) {
				delete(tm.submitTimes, id)
			}
		}
	}()

	tm.cleanupTx(txID)

	return nil
}

// stampCommitInstant fixes the transaction's instant and applies it to every
// row the transaction wrote, immediately before COMMIT.
//
// CURRENT_TIMESTAMP is fixed at transaction START, so it dates a write when
// the transaction opened rather than when it became visible. clock_timestamp()
// read here is the closest a transaction can get to its own commit instant.
//
// The rows are found by transaction_id rather than from the in-memory write
// set, which is not authoritative: after a savepoint rollback the write set
// and the table disagree, and the table is right.
//
// Lock note: validateInChunks' FOR SHARE covers the READ set; the entity and
// version updates touch rows this transaction already holds exclusively, so no
// lock upgrade occurs and they cannot deadlock against the validation that
// precedes them. That reasoning holds only while each WHERE stays scoped to
// rows this transaction actually wrote.
//
// The audit update is the one statement that is NOT so scoped — it matches by
// LABEL (see its own comment), and an event can be labelled with a transaction
// that did not record it. It is still safe, but for a second reason rather than
// the first: this transaction runs at REPEATABLE READ, so a row another
// transaction inserted after this snapshot is invisible here and is not
// locked at all, and a visible row concurrently modified raises 40001
// (→ spi.ErrConflict) instead of waiting. Audit rows are otherwise
// append-only, so nothing else ever holds one under a conflicting lock.
//
// Five things would break this reasoning: widening any WHERE beyond this
// transaction's own rows; moving the stamp before the validation while that
// validation still takes FOR SHARE on rows the stamp updates; adding a
// statement that locks a row this transaction only read; a future FK, trigger
// or index on the stamped columns that reaches a parent row; and — the one
// already live above — a statement whose WHERE matches rows this transaction
// did not write, which needs its own argument every time it is added.
func (tm *TransactionManager) stampCommitInstant(ctx context.Context, tx pgx.Tx, tenantID spi.TenantID, txID string) (time.Time, error) {
	var instant time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&instant); err != nil {
		return time.Time{}, fmt.Errorf("read commit instant: %w", err)
	}
	tid := string(tenantID)

	// creation_date is stamped on EVERY row of an entity whose first version
	// belongs to this transaction — not only on version 1. A transaction that
	// creates an entity and then updates it carries the creation date forward
	// by reading inside the transaction, so the later version holds the
	// provisional value; stamping only version 1 would leave them disagreeing.
	//
	// The CASE must stay conditional in BOTH directions. By the time this runs,
	// the version INSERT already sources creation_date by sub-select from the
	// entity's own entities.creation_date, so a version written by a LATER
	// transaction correctly inherits the entity's original creation date.
	// Replacing this CASE with an unconditional SET creation_date = $1 would
	// restamp every updated entity's creation as that update's instant — which
	// is exactly the defect found when this column was first projected into
	// reads: history reporting when a revision was written rather than when the
	// entity was created, and diverging from the memory backend, which
	// preserves the original.
	if _, err := tx.Exec(ctx,
		`UPDATE entity_versions SET valid_time = $1, transaction_time = $1,
		        creation_date = CASE WHEN entity_id IN (
		            SELECT entity_id FROM entity_versions
		             WHERE tenant_id = $2 AND transaction_id = $3 AND version = 1
		        ) THEN $1 ELSE creation_date END
		  WHERE tenant_id = $2 AND transaction_id = $3`,
		instant, tid, txID); err != nil {
		return time.Time{}, fmt.Errorf("stamp entity versions: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE entities e SET last_modified = $1,
		        creation_date = CASE WHEN EXISTS (
		            SELECT 1 FROM entity_versions v
		             WHERE v.tenant_id = e.tenant_id AND v.entity_id = e.entity_id
		               AND v.transaction_id = $3 AND v.version = 1
		        ) THEN $1 ELSE e.creation_date END
		  WHERE e.tenant_id = $2 AND e.entity_id IN (
		            SELECT entity_id FROM entity_versions
		             WHERE tenant_id = $2 AND transaction_id = $3)`,
		instant, tid, txID); err != nil {
		return time.Time{}, fmt.Errorf("stamp entities: %w", err)
	}

	// Audit events LABELLED with this transaction share its instant, so the
	// audit trail and the version history cannot drift apart or invert.
	//
	// "Labelled with", not "written by", and the difference is real rather
	// than pedantic: the engine records some events under a transaction id
	// that is not the one recording them — EmitTransitionAborted carries the
	// CASCADE ENTRY's id (internal/domain/workflow, reached both from the
	// engine after a segment flush has already failed and from the entity
	// service with its own clock and possibly no ambient transaction). Such an
	// event is matched here if its label happens to name a transaction that
	// later commits, and is otherwise never stamped at all: it keeps the
	// recording process's clock while being ordered, and now reported, from
	// this column. No value regresses — nothing stamped it before either —
	// but the audit trail is not uniformly on the commit clock, and claiming
	// otherwise would be false.
	//
	// This is also a point-in-time sweep rather than a write barrier: an
	// INSERT into this table after this statement, in this same transaction,
	// is not matched and keeps its recorded clock. It does not arise in the
	// normal path — recordEvent runs on the goroutine driving the transaction,
	// which is inside Commit here — but the property is "every event recorded
	// before the commit phase", not "every event this transaction labels".
	// The index this filters on is idx_sm_events_tenant_tx (migration 000013);
	// 000001's idx_sm_events_tx cannot serve it, because entity_id sits
	// between the two columns constrained here.
	if _, err := tx.Exec(ctx,
		`UPDATE sm_audit_events SET timestamp = $1
		  WHERE tenant_id = $2 AND transaction_id = $3`,
		instant, tid, txID); err != nil {
		return time.Time{}, fmt.Errorf("stamp audit events: %w", err)
	}

	// The durable record of the same instant: the in-process map answers only
	// on the node that committed, and only until a restart.
	if _, err := tx.Exec(ctx,
		`INSERT INTO submit_times (tenant_id, tx_id, submit_time) VALUES ($1, $2, $3)
		 ON CONFLICT (tenant_id, tx_id) DO UPDATE SET submit_time = EXCLUDED.submit_time`,
		tid, txID, instant); err != nil {
		return time.Time{}, fmt.Errorf("record submit time: %w", err)
	}

	return instant, nil
}

// pruneSubmitTimes deletes expired submit_times rows on the pool, called
// only AFTER the caller's own commit has already succeeded — never from
// inside pgxTx/stampCommitInstant.
//
// It must run outside the committing transaction. Every other statement in
// stampCommitInstant is scoped to this transaction's own rows, by
// tenant_id+transaction_id (see that function's lock-note comment); two
// concurrent commits can never touch the same row there. A DELETE scoped
// only by submit_time has no such boundary: two concurrent commits whose
// expiry ranges overlap the same stale row would serialize under
// REPEATABLE READ, and the loser gets 40001 — meaning two commits from
// completely unrelated tenants could abort each other over shared
// housekeeping, on the hot commit path of a system whose primary target is
// multi-node with concurrent writers. Running this as an independent
// statement, after commit, on its own connection, removes that coupling: it
// can now only conflict with another concurrent prune, and even then it
// just loses this window and tries again next time (see the rate limit and
// the swallowed error below).
//
// Rate-limited to once per submitTimePruneInterval: this is opportunistic
// housekeeping like the in-process map's own sweep in Commit, not a
// per-commit obligation, so almost every commit skips it after one atomic
// load. idx_submit_times_pruning (migration 000012) keeps the occasional
// real delete a bounded range scan rather than a full-table scan.
//
// Failure is logged and swallowed, never returned: a business commit that
// has already succeeded must not be reported as failed because deleting old
// bookkeeping rows didn't work.
//
// The DELETE carries no tenant predicate on purpose — this is global
// housekeeping over every tenant's expired rows, not a tenant-scoped
// operation — which, like getSubmitTimeFromTable's read, requires the
// owner role this plugin runs as. Under a non-owner, RLS-subject role
// (not a supported posture: see rls_test.go and getSubmitTimeFromTable)
// the policy would admit no row, this would delete nothing on every run,
// and nothing would say so: zero rows deleted is not an error, and the
// error path here is swallowed by design.
func (tm *TransactionManager) pruneSubmitTimes(ctx context.Context) {
	now := time.Now()
	last := tm.lastSubmitTimePruneNano.Load()
	if now.Sub(time.Unix(0, last)) < submitTimePruneInterval {
		return
	}
	if !tm.lastSubmitTimePruneNano.CompareAndSwap(last, now.UnixNano()) {
		return // another goroutine already claimed this window
	}

	// A fresh, short-lived context: the caller's Commit is about to return
	// its own result regardless of this outcome, so this must not inherit a
	// deadline or cancellation meant for the business transaction — and must
	// not be allowed to hang indefinitely either.
	pruneCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := tm.pool.Exec(pruneCtx,
		`DELETE FROM submit_times WHERE submit_time < $1`,
		now.Add(-submitTimeTTL)); err != nil {
		slog.Warn("prune submit_times failed", "pkg", "postgres", "err", err)
	}
}

// Rollback aborts the transaction.
//
// Tenant isolation: rejects mismatched-tenant callers.
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
// Tenant isolation: rejects mismatched-tenant callers.
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
//
// Tenant isolation: like every other tx-lifecycle method, the caller's
// tenant must match the transaction's tenant. The check runs before any
// state-dependent response so a cross-tenant caller learns neither the
// submit time nor whether the transaction is in flight or committed.
func (tm *TransactionManager) GetSubmitTime(ctx context.Context, txID string) (time.Time, error) {
	// Single critical section for both maps; verifyTenant runs outside it
	// (it takes no locks, but keeping the section minimal matches the rest
	// of this file).
	entry, committed, activeTenant, active := func() (submitTimeEntry, bool, spi.TenantID, bool) {
		tm.mu.Lock()
		defer tm.mu.Unlock()
		e, ok := tm.submitTimes[txID]
		tid, act := tm.tenants[txID]
		return e, ok, tid, act
	}()

	switch {
	case committed:
		if err := verifyTenant(ctx, entry.tenantID, "GetSubmitTime", txID); err != nil {
			return time.Time{}, err
		}
		return entry.submitTime, nil
	case active:
		if err := verifyTenant(ctx, activeTenant, "GetSubmitTime", txID); err != nil {
			return time.Time{}, err
		}
		return time.Time{}, fmt.Errorf("transaction not yet committed: %s", txID)
	default:
		return tm.getSubmitTimeFromTable(ctx, txID)
	}
}

// getSubmitTimeFromTable answers a lookup that missed both in-process maps:
// a transaction this node never began or committed, because it committed on
// a different node, or because this node restarted since. The map is
// node-local and dies with the process; the submit_times table (written
// inside Commit, see stampCommitInstant) is the durable authority a lookup
// routed anywhere, at any time after commit, resolves from.
//
// The row's own tenant_id is read unconditionally — the query is not scoped
// to the caller's tenant — so that a cross-tenant caller is told "wrong
// tenant" rather than the misleading "not found": verifyTenant runs against
// whatever tenant actually owns the row, exactly like the committed and
// active branches above run it against tenant state they already hold.
// Reporting node-local ignorance as ErrTxNotFound instead of consulting the
// table would be a wrong definitive answer, which this project's
// correctness-over-availability design rejects.
//
// Role posture — this read requires a database role NOT subject to RLS.
// Like every other pool-routed statement in this plugin it carries no
// app.current_tenant GUC (set_config's is_local flag scopes that setting to a
// transaction, so no non-transactional statement has ever carried it), and
// submit_times has a tenant-isolation policy like every other table. That is
// safe in the posture cyoda-go actually runs and supports: the application
// connects as the table owner, RLS is ENABLEd but not FORCEd (migrate_test.go
// pins both), and an owner bypasses every policy. A non-owner, RLS-subject
// role is NOT a supported deployment today — see rls_test.go — and this
// lookup is one of the reasons: under such a role
// current_setting('app.current_tenant', true) is NULL on a pooled connection,
// the policy admits no row, and this function would answer ErrTxNotFound for
// a transaction that demonstrably committed. That is a wrong definitive
// answer, not a degraded one, so the non-owner mode cannot be enabled by
// changing this query: it needs the tenant set on the pool path for the whole
// plugin (a pgxpool AfterConnect/BeforeAcquire hook) before any pool-routed
// read can be trusted under RLS.
func (tm *TransactionManager) getSubmitTimeFromTable(ctx context.Context, txID string) (time.Time, error) {
	var tenantID string
	var submit time.Time
	// The table's primary key is (tenant_id, tx_id), not tx_id alone, so this
	// WHERE tx_id = $1 is a query, not a key lookup. It's safe only because
	// txID is a fresh UUID minted per Begin (see NewTransactionManager's
	// uuids field) and collision across tenants is astronomically unlikely;
	// LIMIT 1 makes that assumption explicit rather than relying on
	// QueryRow's own "first row wins" behaviour to paper over a collision.
	err := tm.pool.QueryRow(ctx,
		`SELECT tenant_id, submit_time FROM submit_times WHERE tx_id = $1 LIMIT 1`,
		txID).Scan(&tenantID, &submit)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("GetSubmitTime: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("GetSubmitTime: %w", classifyError(err))
	}
	if err := verifyTenant(ctx, spi.TenantID(tenantID), "GetSubmitTime", txID); err != nil {
		return time.Time{}, err
	}
	return submit, nil
}

// LookupTx exposes the registry lookup for use in tests and by the store
// layer (resolveQuerier). Production code should prefer resolveQuerier.
func (tm *TransactionManager) LookupTx(txID string) (pgx.Tx, bool) {
	return tm.registry.Lookup(txID)
}

// wasCommitted reports whether txID is recorded in submitTimes — i.e. it
// committed successfully and no later Commit's sweep has evicted the entry
// yet. It is the store layer's way of telling "this transaction is gone
// because it committed" apart from "this transaction is gone for an unknown
// reason" at the resolveRaw seam: cleanupTx purges the registry on every
// exit path (Commit and Rollback alike), so a registry miss alone cannot
// distinguish committed from rolled-back from reclaimed. submitTimes,
// populated only on the Commit success path and kept deliberately past
// cleanupTx (see Commit's comment on the race with GetSubmitTime), is the
// one piece of post-cleanup state that answers "committed" affirmatively.
//
// Eviction past submitTimeTTL is opportunistic, not a wall-clock deadline:
// the sweep runs only inside a later Commit's own submitTimes write (see
// Commit), so on a quiet system with no other transaction committing, an
// entry can outlive the TTL considerably. That only ever makes wasCommitted
// stay precise for longer — it never turns a true answer false — so the
// eventual degradation to ErrTxNotFound at the resolveRaw seam is a floor on
// how long the precise sentinel is guaranteed to be available, not a
// deadline by which it is guaranteed to be gone.
//
// No tenant check: this only steers which SPI sentinel a same-context store
// operation gets classified as, never anything that discloses the submit
// time or any other value to a caller who doesn't already hold the ID.
func (tm *TransactionManager) wasCommitted(txID string) bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	_, ok := tm.submitTimes[txID]
	return ok
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
// Tenant isolation: rejects mismatched-tenant callers.
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
// Tenant isolation: rejects mismatched-tenant callers —
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
// Tenant isolation: rejects mismatched-tenant callers.
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
//
// The two connection ceilings are classified here too, and deliberately NOT
// alike, because they differ in whether retrying helps:
//   - idle_in_transaction_session_timeout (25P03, plus the torn-socket shape of
//     the same event) → the storage-unavailable marker, i.e. a retryable 503.
//   - statement_timeout (57014) → passed through unmarked, so it lands on the
//     500-with-a-ticket path. Re-running a statement that just exceeded the
//     ceiling will exceed it again; calling that retryable would be a lie.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	if classified, ok := classifySQLState(err); ok {
		return classified
	}
	// No server response to read: the session was already gone by the time pgx
	// looked. Same event as 25P03, second face — see isConnectionTorn.
	if isConnectionTorn(err) {
		return &idleInTxAbortError{cause: err}
	}
	return err
}

// classifySQLState is the half of classification that depends only on what the
// SERVER said. It is separated out because it is the only half that is safe
// where the outcome of an in-flight statement is in doubt — see
// classifyCommitError.
//
// Reports false when no branch matched, so callers can decide for themselves
// what to make of an error the server never answered.
func classifySQLState(err error) (error, bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err, false
	}
	switch {
	case pgErr.Code == pgerrcode.SerializationFailure || pgErr.Code == pgerrcode.DeadlockDetected:
		return fmt.Errorf("%w: %w", spi.ErrConflict, err), true
	case pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == "unique_claims_uq":
		return fmt.Errorf("%w: %w", spi.ErrUniqueViolation, err), true
	case pgErr.Code == pgerrcode.IdleInTransactionSessionTimeout:
		// The server said which ceiling fired, so the log can name it — the
		// torn-socket shape cannot.
		slog.Warn("transaction reclaimed after exceeding the configured ceiling",
			"pkg", "postgres", "setting", "idle_in_transaction_session_timeout", "err", err)
		return &idleInTxAbortError{cause: err}, true
	case pgErr.Code == pgerrcode.QueryCanceled:
		// NOT retryable, and deliberately not marked so. The 500 this becomes
		// carries a ticket; this log line is the whole user-visible benefit,
		// turning an unexplained failure into a named cause.
		slog.Warn("statement cancelled after exceeding the configured ceiling",
			"pkg", "postgres", "setting", "statement_timeout", "err", err)
		return err, true
	}
	return err, false
}

// classifyTxError is classifyError for an error raised against a specific
// transaction. When the session was reclaimed, that transaction no longer exists
// server-side and Commit/Rollback will never run to tidy up after it — so the
// pgx handle and the per-transaction bookkeeping are reclaimed here instead.
//
// It also records a cancelled statement on the txState. PostgreSQL answers every
// later statement in an aborted transaction — Commit's own probe included — with
// 25P02, which says only "something earlier failed"; without this the commit
// would report the ceiling as a retryable conflict.
func (tm *TransactionManager) classifyTxError(txID string, err error) error {
	classified := classifyError(err)
	if isIdleInTxAbort(classified) {
		tm.discardTx(txID)
		return classified
	}
	if isStatementTimeout(classified) {
		if state, ok := tm.lookupTxState(txID); ok {
			state.RecordAbort(classified)
		}
	}
	return classified
}

// classifyCommitError classifies a failure of the COMMIT itself, where a torn
// socket means something different from what it means anywhere else.
//
// If the backend received the COMMIT and the connection died before its response
// arrived, the transaction COMMITTED. The outcome is in doubt, and a caller told
// to retry would apply the work twice — entity ids are minted per attempt, so
// the retry creates a duplicate rather than colliding. Only the server saying
// 25P03 proves the transaction was reclaimed and nothing was applied.
//
// So this classifies on the server's response alone: every shape the server did
// not answer keeps the non-retryable 500 it has always had.
func (tm *TransactionManager) classifyCommitError(txID string, err error) error {
	if err == nil {
		return nil
	}
	classified, _ := classifySQLState(err)
	var pgErr *pgconn.PgError
	if errors.As(classified, &pgErr) && pgErr.Code == pgerrcode.IdleInTransactionSessionTimeout {
		tm.discardTx(txID)
	}
	return classified
}

// discardTx releases a transaction whose session the server has already
// terminated.
//
// The rollback cannot reach the server and is expected to fail; it is issued
// because that is what hands the pooled connection back — pgxpool releases it
// inside Rollback regardless of outcome. Dropping the registry entry without it
// would leave pgxpool believing the connection is still checked out, so the pool
// would shrink by one on every reclaimed transaction.
//
// It runs on a background context: the caller's may be the very one that just
// expired, and a rollback on an expired context destroys the pooled connection
// instead of returning it.
func (tm *TransactionManager) discardTx(txID string) {
	if pgxTx, ok := tm.registry.Lookup(txID); ok {
		_ = pgxTx.Rollback(context.Background())
	}
	tm.cleanupTx(txID)
}
