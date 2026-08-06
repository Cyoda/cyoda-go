package postgres

// classify_test.go — the two server-side aborts the connection ceilings raise.
//
// They are classified DIFFERENTLY, because they differ in whether retrying
// helps. An idle-in-transaction abort (25P03) is transient contention: the
// transaction is gone, but the same request on a fresh one may well succeed, so
// it carries the storage-unavailable marker the application layer turns into a
// retryable 503. A cancelled statement (57014) is not: re-running a statement
// that just exceeded the ceiling will exceed it again, so it stays a 500 with a
// ticket and must never acquire a retryable sentinel on the way there.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// hasStorageUnavailableMarker reports whether err carries the marker the
// application layer matches with errors.As to produce a retryable 503. Declared
// here rather than imported: the marker is an interface by design, so that no
// package has to own the type.
func hasStorageUnavailableMarker(err error) bool {
	var su interface{ StorageUnavailable() bool }
	return errors.As(err, &su) && su.StorageUnavailable()
}

// TestClassifyError_IdleInTxAbort_BothShapes — 25P03 terminates the SESSION, not
// merely the transaction, so what the next operation sees depends on whether pgx
// read the buffered ErrorResponse before noticing the socket had gone: either a
// PgError carrying 25P03, or a transport error. Both shapes are the same event
// and both must classify. See isConnectionTorn for the shapes observed live.
func TestClassifyError_IdleInTxAbort_BothShapes(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"server error response", &pgconn.PgError{
			Code:     pgerrcode.IdleInTransactionSessionTimeout,
			Severity: "FATAL",
			Message:  "terminating connection due to idle-in-transaction timeout",
		}},
		{"driver closed the connection first", pgconn.ErrConnClosed},
		{"driver closed it under pgx's own wrapping", fmt.Errorf(
			"failed to deallocate cached statement(s): %w", pgconn.ErrConnClosed)},
		{"unexpected EOF", io.ErrUnexpectedEOF},
		{"broken pipe", &net.OpError{Op: "write", Err: syscall.EPIPE}},
		{"connection reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}},
		{"socket already closed", net.ErrClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// As the stores see it: wrapped at least once by the time a
			// classifier runs.
			wrapped := fmt.Errorf("failed to get DB timestamps: %w", tc.err)

			if !isIdleInTxAbort(wrapped) {
				t.Fatalf("not classified as an idle-in-transaction abort: %v", tc.err)
			}
			if !hasStorageUnavailableMarker(classifyError(wrapped)) {
				t.Fatalf("classifyError did not mark it storage-unavailable, so it would surface as a 500 rather than a retryable 503: %v", tc.err)
			}
		})
	}
}

// TestClassifyError_CallerGoingAwayIsNotAnIdleAbort is the boundary Task 10 drew
// for the acquire deadline, held here too: a request whose own context ended is
// not a server-side outage, and reporting it as a retryable 503 would blame the
// server for the client's departure.
func TestClassifyError_CallerGoingAwayIsNotAnIdleAbort(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"caller cancelled", context.Canceled},
		{"caller deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("query failed: %w", tc.err)
			if isIdleInTxAbort(err) {
				t.Fatalf("a caller going away was classified as an idle-in-transaction abort: %v", err)
			}
			if hasStorageUnavailableMarker(classifyError(err)) {
				t.Fatalf("a caller going away was reported as a retryable server outage: %v", err)
			}
		})
	}
}

// TestClassifyError_StatementTimeout_IsNotRetryable is coverage row 12's unit
// half.
func TestClassifyError_StatementTimeout_IsNotRetryable(t *testing.T) {
	err := classifyError(&pgconn.PgError{
		Code:    pgerrcode.QueryCanceled,
		Message: "canceling statement due to statement timeout",
	})
	if hasStorageUnavailableMarker(err) {
		t.Fatal("statement timeout advertised as retryable; re-running it would exceed the ceiling again")
	}
	if !isStatementTimeout(err) {
		t.Fatal("statement timeout not recognised; it would surface as an unexplained failure")
	}
}

// TestClassifyError_StatementTimeoutIsNotAConflict guards the boundary against
// the existing 40001/40P01 mapping — and against common.Internal, which routes
// any ErrConflict-bearing cause to a RETRYABLE 409. Acquiring that sentinel
// anywhere in the chain would turn this 500 into exactly the lie the split
// exists to avoid.
func TestClassifyError_StatementTimeoutIsNotAConflict(t *testing.T) {
	err := classifyError(&pgconn.PgError{Code: pgerrcode.QueryCanceled})
	if errors.Is(err, spi.ErrConflict) {
		t.Fatal("statement timeout mapped to a retryable conflict")
	}
	if errors.Is(err, spi.ErrUniqueViolation) || errors.Is(err, spi.ErrPartialUniqueKey) {
		t.Fatal("statement timeout acquired a unique-key sentinel; common.Internal would downgrade it to a 4xx")
	}
}

// TestClassifyError_IdleInTxAbortIsNotAConflict — the same guard on the other
// side. A 503 that also carried ErrConflict would be re-routed to a 409 by
// common.Internal before the storage-unavailable branch ever saw it.
func TestClassifyError_IdleInTxAbortIsNotAConflict(t *testing.T) {
	err := classifyError(&pgconn.PgError{Code: pgerrcode.IdleInTransactionSessionTimeout})
	if errors.Is(err, spi.ErrConflict) {
		t.Fatal("idle-in-transaction abort mapped to a conflict")
	}
}

// TestClassifyError_NamesTheCeilingThatFired — for the cancelled statement the
// log line IS the change: the response stays a deliberately opaque 500 with a
// ticket, so an operator correlating that ticket has nothing else to go on. Both
// ceilings therefore name the setting that fired, using the setting's own name
// so it can be grepped straight back to the configuration.
func TestClassifyError_NamesTheCeilingThatFired(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    string
		setting string
	}{
		{"cancelled statement", pgerrcode.QueryCanceled, "statement_timeout"},
		{"reclaimed transaction", pgerrcode.IdleInTransactionSessionTimeout, "idle_in_transaction_session_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			classifyError(&pgconn.PgError{Code: tc.code})

			if !strings.Contains(buf.String(), tc.setting) {
				t.Fatalf("the log does not name %s, so the failure stays unexplained: %s", tc.setting, buf.String())
			}
		})
	}
}

// TestClassifyCommitError_TornSocketStaysNonRetryable — a torn socket raised by
// the COMMIT itself is not proof the transaction is gone. If the backend
// received the COMMIT and the connection died before its response, the
// transaction COMMITTED; the outcome is in doubt. Entity ids are minted per
// attempt, so a client honouring retryable=true would create a duplicate. Only
// the server saying 25P03 proves nothing was applied.
func TestClassifyCommitError_TornSocketStaysNonRetryable(t *testing.T) {
	tm := NewTransactionManager(nil, newTestUUIDGenerator())

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"unexpected EOF", io.ErrUnexpectedEOF},
		{"broken pipe", &net.OpError{Op: "write", Err: syscall.EPIPE}},
		{"connection reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}},
		{"driver closed the connection", pgconn.ErrConnClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tm.classifyCommitError("tx-1", fmt.Errorf("Commit: %w", tc.err))
			if hasStorageUnavailableMarker(err) {
				t.Fatalf("an in-doubt commit was advertised as retryable; a retry would double-apply it: %v", err)
			}
		})
	}
}

// TestClassifyCommitError_ReclaimedTransactionIsStillRetryable — the shape that
// DOES prove the transaction was reclaimed keeps its promotion, because nothing
// was applied and a retry is genuinely safe.
func TestClassifyCommitError_ReclaimedTransactionIsStillRetryable(t *testing.T) {
	tm := NewTransactionManager(nil, newTestUUIDGenerator())

	err := tm.classifyCommitError("tx-1", fmt.Errorf("Commit: %w",
		&pgconn.PgError{Code: pgerrcode.IdleInTransactionSessionTimeout}))
	if !hasStorageUnavailableMarker(err) {
		t.Fatalf("a transaction the server said it reclaimed was not reported retryable: %v", err)
	}
}

// TestClassifyCommitError_KeepsTheConflictMapping — the commit site still has to
// recognise a serialization abort, which is the ordinary reason a COMMIT fails.
func TestClassifyCommitError_KeepsTheConflictMapping(t *testing.T) {
	tm := NewTransactionManager(nil, newTestUUIDGenerator())

	err := tm.classifyCommitError("tx-1", fmt.Errorf("Commit: %w",
		&pgconn.PgError{Code: pgerrcode.SerializationFailure}))
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("a serialization failure at commit lost its conflict sentinel: %v", err)
	}
}

// TestIsConnectionTorn_ExcludesAFailedConnect — "could not establish a
// connection" is a different condition from "the session went away underneath
// this operation". It covers causes a retry never clears (bad password, pg_hba
// rejection), and inside classifyTxError it would make discardTx roll back and
// de-register a transaction that is perfectly alive.
func TestIsConnectionTorn_ExcludesAFailedConnect(t *testing.T) {
	err := fmt.Errorf("dial: %w", &pgconn.ConnectError{Config: &pgconn.Config{}})
	if isConnectionTorn(err) {
		t.Fatal("a failed connect was classified as a torn session")
	}
	if isIdleInTxAbort(err) {
		t.Fatal("a failed connect was classified as an idle-in-transaction abort")
	}
}

// TestIsIdleInTxAbort_RecognisesItsOwnType — the predicate and the marker it
// mints must be each other's inverse, or a caller that classifies twice gets a
// different answer the second time.
func TestIsIdleInTxAbort_RecognisesItsOwnType(t *testing.T) {
	minted := classifyError(&pgconn.PgError{Code: pgerrcode.IdleInTransactionSessionTimeout})
	if !isIdleInTxAbort(minted) {
		t.Fatalf("classifyError minted a marker its own predicate does not recognise: %#v", minted)
	}
	if !isIdleInTxAbort(fmt.Errorf("wrapped: %w", minted)) {
		t.Fatal("the marker stops being recognised once wrapped")
	}
}

// TestDeadTxError_DoesNotClaimTheCeilingReclaimedIt — a registry miss has
// several possible causes (a reclaimed session, a failed commit, a transaction
// already finished), and the operator log must not assert one of them. It still
// carries the storage-unavailable marker: the transaction is gone either way.
func TestDeadTxError_DoesNotClaimTheCeilingReclaimedIt(t *testing.T) {
	err := deadTxQuerier{txID: "tx-1"}.err()

	if !hasStorageUnavailableMarker(err) {
		t.Fatal("a statement on a transaction that no longer exists was not reported storage-unavailable")
	}
	if strings.Contains(err.Error(), "reclaimed") {
		t.Fatalf("the message asserts a cause it cannot know: %v", err)
	}
	var idle *idleInTxAbortError
	if errors.As(err, &idle) {
		t.Fatal("a registry miss reuses the idle-ceiling type, so the log blames a ceiling that may not have fired")
	}
}

// --- live server -----------------------------------------------------------

// abortFixture is a manager plus the store-facing querier, on a pool whose
// idle-in-transaction ceiling is short enough to fire inside a test.
type abortFixture struct {
	tm   *TransactionManager
	q    Querier
	pool *pgxpool.Pool
}

func newAbortFixture(t *testing.T, idle time.Duration) *abortFixture {
	t.Helper()
	pool := openCeilingPool(t, ceilingEnv(skipIfNoLiveDB(t), map[string]string{
		"CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT": idle.String(),
	}))
	tm := NewTransactionManager(pool, newTestUUIDGenerator())
	f := NewStoreFactory(pool)
	f.setTransactionManager(tm)
	return &abortFixture{tm: tm, q: f.querier(), pool: pool}
}

// newStatementCeilingFixture is newAbortFixture for the other ceiling: a pool
// whose statement_timeout is short enough to cancel a statement inside a test.
// A zero ceiling disables it, for scenarios that abort the transaction some
// other way.
func newStatementCeilingFixture(t *testing.T, limit time.Duration) *abortFixture {
	t.Helper()
	pool := openCeilingPool(t, ceilingEnv(skipIfNoLiveDB(t), map[string]string{
		"CYODA_POSTGRES_STATEMENT_TIMEOUT": limit.String(),
	}))
	tm := NewTransactionManager(pool, newTestUUIDGenerator())
	f := NewStoreFactory(pool)
	f.setTransactionManager(tm)
	return &abortFixture{tm: tm, q: f.querier(), pool: pool}
}

func classifyTestCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "classify-user",
		Tenant: spi.Tenant{ID: "classify-tenant", Name: "classify-tenant"},
	})
}

// beginThenLetTheCeilingFire begins a transaction, sits idle past the ceiling
// and returns the transaction's ID together with the error the next operation
// raised — which is the thing under classification.
func (fx *abortFixture) beginThenLetTheCeilingFire(t *testing.T) (string, context.Context, error) {
	t.Helper()
	txID, txCtx, err := fx.tm.Begin(classifyTestCtx())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	time.Sleep(2 * time.Second) // well past the fixture's ceiling

	var one int
	opErr := fx.q.QueryRow(txCtx, "SELECT 1").Scan(&one)
	if opErr == nil {
		t.Fatal("the transaction survived an idle gap past the ceiling; there is nothing to classify")
	}
	return txID, txCtx, opErr
}

// TestLive_IdleInTxCeiling_ErrorShape settles by observation which of the two
// documented shapes pgx surfaces — for the FIRST operation after the ceiling
// fires and for the one after that. It runs on a raw pgx transaction, with no
// classifier in the way, so what it records is the driver's behaviour rather
// than this plugin's reading of it. The observed shapes are written up in the
// comment above isConnectionTorn.
func TestLive_IdleInTxCeiling_ErrorShape(t *testing.T) {
	pool := openCeilingPool(t, ceilingEnv(skipIfNoLiveDB(t), map[string]string{
		"CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT": "300ms",
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var one int
	if err := tx.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("first statement in tx: %v", err)
	}

	time.Sleep(2 * time.Second) // well past the ceiling

	first := tx.QueryRow(ctx, "SELECT 1").Scan(&one)
	if first == nil {
		t.Fatal("the transaction survived an idle gap past the ceiling; there is nothing to classify")
	}
	t.Logf("first operation after the ceiling fired: %[1]T %#[1]v\n  Error() = %[1]v", first)
	if !isIdleInTxAbort(first) {
		t.Fatalf("first shape not classified: %v", first)
	}

	second := tx.QueryRow(ctx, "SELECT 1").Scan(&one)
	if second == nil {
		t.Fatal("a second operation succeeded on a terminated session")
	}
	t.Logf("second operation on the dead session: %[1]T %#[1]v\n  Error() = %[1]v", second)
	if !isIdleInTxAbort(second) {
		t.Fatalf("second shape not classified: %v", second)
	}
}

// TestIdleInTxAbort_LaterStatementsDoNotEscapeToThePool — reclaiming the
// transaction must not leave the context pointing at nothing, because a store
// resolving no transaction would fall back to the pool and run the rest of the
// request's statements outside it, committing each on its own.
func TestIdleInTxAbort_LaterStatementsDoNotEscapeToThePool(t *testing.T) {
	fx := newAbortFixture(t, 300*time.Millisecond)

	_, txCtx, _ := fx.beginThenLetTheCeilingFire(t)

	var one int
	err := fx.q.QueryRow(txCtx, "SELECT 1").Scan(&one)
	if err == nil {
		t.Fatal("a statement succeeded after the transaction was reclaimed; it ran on the pool, outside any transaction")
	}
	if !hasStorageUnavailableMarker(err) {
		t.Fatalf("a statement on a reclaimed transaction was not reported as storage-unavailable: %v", err)
	}
}

// TestIdleInTxAbort_ClearsPerTransactionState is coverage row 11f. PostgreSQL
// kills the session; cleanupTx only ever runs from Commit/Rollback, so without
// an explicit reclaim here the registry, tenants, origins and txStates entries —
// the last carrying the read and write sets — survive indefinitely.
func TestIdleInTxAbort_ClearsPerTransactionState(t *testing.T) {
	fx := newAbortFixture(t, 300*time.Millisecond)

	txID, _, _ := fx.beginThenLetTheCeilingFire(t)

	registry, tenant, origin, state := fx.tm.txResidue(txID)
	if registry || tenant || origin || state {
		t.Fatalf("per-transaction bookkeeping survived the abort: registry=%v tenant=%v origin=%v txState=%v",
			registry, tenant, origin, state)
	}
}

// TestCommitAfterStatementTimeout_IsNotAConflict is the whole point of the
// split, at the one place it was still being undone.
//
// A cancelled statement leaves the PostgreSQL transaction aborted, so every
// later statement — including Commit's own submit-time probe — comes back
// 25P02 in_failed_sql_transaction, which says only "something earlier failed".
// Reading that as a serialization conflict hands the caller a retryable 409 for
// a statement that would be cancelled again. A caller that swallows a per-item
// failure and commits anyway (DeleteEntitiesConditional does exactly that)
// reaches this every time.
func TestCommitAfterStatementTimeout_IsNotAConflict(t *testing.T) {
	fx := newStatementCeilingFixture(t, 300*time.Millisecond)

	ctx := classifyTestCtx()
	txID, txCtx, err := fx.tm.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	var slept string
	stmtErr := fx.q.QueryRow(txCtx, "SELECT pg_sleep(3)").Scan(&slept)
	if stmtErr == nil {
		t.Fatal("a 3s statement completed under a 300ms ceiling; nothing aborted the transaction")
	}
	if !isStatementTimeout(stmtErr) {
		t.Fatalf("the statement did not fail with 57014, so this scenario proves nothing: %v", stmtErr)
	}

	commitErr := fx.tm.Commit(ctx, txID)
	if commitErr == nil {
		t.Fatal("a transaction aborted by the statement ceiling committed")
	}
	t.Logf("commit after a cancelled statement: %v", commitErr)

	if errors.Is(commitErr, spi.ErrConflict) {
		t.Fatal("reported as a retryable conflict; the client would retry a statement the ceiling cancels again")
	}
	if !isStatementTimeout(commitErr) {
		t.Fatalf("the commit failure does not name the cancelled statement, so nothing downstream can explain it: %v", commitErr)
	}
	if hasStorageUnavailableMarker(commitErr) {
		t.Fatal("reported as a transient storage outage, which is the same lie in another shape")
	}
}

// TestCommitAfterSerializationFailure_IsStillAConflict is the control: the
// ordinary reason a transaction is found aborted must keep its retryable 409.
func TestCommitAfterSerializationFailure_IsStillAConflict(t *testing.T) {
	fx := newStatementCeilingFixture(t, 0) // no ceiling; the abort comes from bad SQL

	ctx := classifyTestCtx()
	txID, txCtx, err := fx.tm.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	// Any failed statement aborts the transaction; this one is not a ceiling.
	var n int
	if err := fx.q.QueryRow(txCtx, "SELECT 1/0").Scan(&n); err == nil {
		t.Fatal("division by zero succeeded")
	}

	commitErr := fx.tm.Commit(ctx, txID)
	if commitErr == nil {
		t.Fatal("an aborted transaction committed")
	}
	if !errors.Is(commitErr, spi.ErrConflict) {
		t.Fatalf("an aborted transaction with no ceiling involved lost its conflict mapping: %v", commitErr)
	}
}

// TestIdleInTxAbort_ReturnsThePooledConnection — reclaiming the bookkeeping must
// not orphan the pgx handle. Dropping the registry entry without rolling the
// transaction back would leave pgxpool believing the connection is still checked
// out, so the pool would shrink by one on every reclaimed transaction until it
// could serve nothing at all.
func TestIdleInTxAbort_ReturnsThePooledConnection(t *testing.T) {
	fx := newAbortFixture(t, 300*time.Millisecond)

	fx.beginThenLetTheCeilingFire(t)

	// puddle destroys a released-but-dead resource on its own goroutine, so the
	// hand-back is observable promptly rather than instantly.
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := fx.pool.Stat().AcquiredConns()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connection(s) still checked out after the transaction was reclaimed; the pgx handle was orphaned", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
