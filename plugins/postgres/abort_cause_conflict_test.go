package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
)

// A serialization failure aborts the whole transaction. PostgreSQL answers
// every later statement in it with 25P02, which says only "something earlier
// failed". The caller that issues one — a workflow run re-reading the entity a
// joined callback just failed to write — must still be told it lost a race,
// not handed an unclassified driver error it reports as an internal fault.

// TestStatementAfterSerializationFailure_IsAConflict provokes a real 40001:
// two transactions race one row, the second writer loses, and the statement
// it runs next must carry spi.ErrConflict.
func TestStatementAfterSerializationFailure_IsAConflict(t *testing.T) {
	fx := newStatementCeilingFixture(t, 0)
	ctx := classifyTestCtx()

	table := fmt.Sprintf("abort_cause_race_%s", uuid.NewString()[:8])
	if _, err := fx.pool.Exec(ctx, "CREATE TABLE "+table+" (id int PRIMARY KEY, n int NOT NULL)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { _, _ = fx.pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	if _, err := fx.pool.Exec(ctx, "INSERT INTO "+table+" VALUES (1, 0)"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	txID, txCtx := beginGuarded(t, fx.tm, ctx)

	// Fix the snapshot, then let a rival commit a change to the row under it.
	var n int
	if err := fx.q.QueryRow(txCtx, "SELECT n FROM "+table+" WHERE id = 1").Scan(&n); err != nil {
		t.Fatalf("snapshot read: %v", err)
	}
	if _, err := fx.pool.Exec(ctx, "UPDATE "+table+" SET n = n + 1 WHERE id = 1"); err != nil {
		t.Fatalf("rival update: %v", err)
	}

	_, err := fx.q.Exec(txCtx, "UPDATE "+table+" SET n = n + 10 WHERE id = 1")
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("the losing write is not a conflict, so this scenario proves nothing: %v", err)
	}
	if errors.Is(err, spi.ErrTxAborted) {
		t.Fatalf("the losing write is the conflict itself, not a statement after one: %v", err)
	}

	assertLaterStatementIsConflict(t, fx, txCtx)

	commitErr := fx.tm.Commit(ctx, txID)
	requireConflictCause(t, commitErr, pgerrcode.SerializationFailure)
}

// TestCommitWithReadSetAfterSerializationFailure_IsAConflict: a transaction
// that read an entity validates its read set before anything else at Commit,
// so the validation query is the first statement to meet the abort. It must
// report the conflict that caused it, as the stamp step does.
func TestCommitWithReadSetAfterSerializationFailure_IsAConflict(t *testing.T) {
	fx := newStatementCeilingFixture(t, 0)
	ctx := classifyTestCtx()
	txID, txCtx := beginGuarded(t, fx.tm, ctx)

	state, ok := fx.tm.lookupTxState(txID)
	if !ok {
		t.Fatal("no txState for the transaction just begun")
	}
	state.RecordRead(uuid.NewString(), 1)

	if _, err := fx.q.Exec(txCtx, "DO $$ BEGIN RAISE EXCEPTION 'conflict' USING ERRCODE = '40001'; END $$"); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("40001 is not a conflict, so this scenario proves nothing: %v", err)
	}

	requireConflictCause(t, fx.tm.Commit(ctx, txID), pgerrcode.SerializationFailure)
}

// requireConflictCause asserts err is spi.ErrConflict and names the SQLSTATE
// that aborted the transaction, not only the 25P02 that followed it.
func requireConflictCause(t *testing.T, err error, code string) {
	t.Helper()
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("commit after the conflict lost its conflict mapping: %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("commit error does not carry the %s that aborted the transaction: %v", code, err)
	}
}

// TestStatementAfterDeadlock_IsAConflict covers 40P01, the other code that
// aborts a transaction because of a concurrent writer. The server raises it
// directly: arranging a real deadlock needs two lock orders racing, and the
// transaction state it leaves is the same.
func TestStatementAfterDeadlock_IsAConflict(t *testing.T) {
	fx := newStatementCeilingFixture(t, 0)
	ctx := classifyTestCtx()
	_, txCtx := beginGuarded(t, fx.tm, ctx)

	_, err := fx.q.Exec(txCtx, "DO $$ BEGIN RAISE EXCEPTION 'deadlock' USING ERRCODE = '40P01'; END $$")
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("40P01 is not a conflict, so this scenario proves nothing: %v", err)
	}

	assertLaterStatementIsConflict(t, fx, txCtx)
}

// TestStatementAfterNonConflictAbort_IsNotAConflict is the control: a
// transaction aborted by something other than a concurrent writer keeps the
// statement error it has today. Only a recorded conflict turns 25P02 into one.
func TestStatementAfterNonConflictAbort_IsNotAConflict(t *testing.T) {
	fx := newStatementCeilingFixture(t, 0)
	ctx := classifyTestCtx()
	_, txCtx := beginGuarded(t, fx.tm, ctx)

	var n int
	if err := fx.q.QueryRow(txCtx, "SELECT 1/0").Scan(&n); err == nil {
		t.Fatal("division by zero succeeded")
	}
	err := fx.q.QueryRow(txCtx, "SELECT 1").Scan(&n)
	if err == nil {
		t.Fatal("a statement succeeded on an aborted transaction")
	}
	if errors.Is(err, spi.ErrConflict) {
		t.Fatalf("an abort no concurrent writer caused was reported as a conflict: %v", err)
	}
}

// TestConflictSurvivesSavepointRollback: a conflict anywhere in the
// transaction is a conflict of the transaction. ROLLBACK TO SAVEPOINT makes the
// session usable again, and later statements run, but the write that lost the
// race is gone and the transaction cannot commit as the caller meant it. Commit
// must refuse it with the recorded conflict and write nothing — the answer a
// backend that detects the conflict at commit gives for the same work.
func TestConflictSurvivesSavepointRollback(t *testing.T) {
	fx := newStatementCeilingFixture(t, 0)
	ctx := classifyTestCtx()
	// Commit stamps the entity tables, so a transaction here can only commit
	// on a migrated schema — which the control below proves it does.
	if err := runMigrations(ctx, fx.pool, defaultMigrateLockTimeout); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	table := fmt.Sprintf("abort_cause_sp_%s", uuid.NewString()[:8])
	if _, err := fx.pool.Exec(ctx, "CREATE TABLE "+table+" (id int PRIMARY KEY)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { _, _ = fx.pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	count := func(id int) int {
		t.Helper()
		var n int
		if err := fx.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE id = $1", id).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	// run: savepoint, the failing statement, rollback to the savepoint, a
	// write of id, then Commit.
	run := func(id int, failing string) error {
		t.Helper()
		txID, txCtx := beginGuarded(t, fx.tm, ctx)
		spID, err := fx.tm.Savepoint(ctx, txID)
		if err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		if _, err := fx.q.Exec(txCtx, failing); err == nil {
			t.Fatalf("%q succeeded", failing)
		}
		if err := fx.tm.RollbackToSavepoint(ctx, txID, spID); err != nil {
			t.Fatalf("rollback to savepoint: %v", err)
		}
		// The session is usable again.
		if _, err := fx.q.Exec(txCtx, "INSERT INTO "+table+" VALUES ($1)", id); err != nil {
			t.Fatalf("the transaction did not recover from the savepoint rollback: %v", err)
		}
		return fx.tm.Commit(ctx, txID)
	}

	// Control: an abort that is not a conflict is undone by the rollback, and
	// the transaction commits.
	if err := run(1, "SELECT 1/0"); err != nil {
		t.Fatalf("control: a transaction whose non-conflict abort was rolled back did not commit: %v", err)
	}
	if count(1) != 1 {
		t.Fatal("control: the committed row is missing")
	}

	err := run(2, "DO $$ BEGIN RAISE EXCEPTION 'conflict' USING ERRCODE = '40001'; END $$")
	requireConflictCause(t, err, pgerrcode.SerializationFailure)
	if n := count(2); n != 0 {
		t.Fatalf("a transaction that lost a race committed %d row(s) after a savepoint rollback", n)
	}
}

// TestStatementAfterRolledBackConflict_IsTxAborted: the conflict a savepoint
// rollback kept also explains a later abort. The transaction cannot commit
// whatever aborted it next, so a statement refused after that is refused
// because of the conflict.
func TestStatementAfterRolledBackConflict_IsTxAborted(t *testing.T) {
	fx := newStatementCeilingFixture(t, 0)
	ctx := classifyTestCtx()
	txID, txCtx := beginGuarded(t, fx.tm, ctx)

	spID, err := fx.tm.Savepoint(ctx, txID)
	if err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	if _, err := fx.q.Exec(txCtx, "DO $$ BEGIN RAISE EXCEPTION 'conflict' USING ERRCODE = '40001'; END $$"); !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("40001 is not a conflict: %v", err)
	}
	if err := fx.tm.RollbackToSavepoint(ctx, txID, spID); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}

	var n int
	if err := fx.q.QueryRow(txCtx, "SELECT 1/0").Scan(&n); err == nil {
		t.Fatal("division by zero succeeded")
	}
	assertLaterStatementIsConflict(t, fx, txCtx)
	requireConflictCause(t, fx.tm.Commit(ctx, txID), pgerrcode.SerializationFailure)
}

// assertLaterStatementIsConflict runs a statement of each querier shape on the
// aborted transaction and requires every one to report spi.ErrTxAborted —
// which is also spi.ErrConflict — with the refusing 25P02 still in the chain.
func assertLaterStatementIsConflict(t *testing.T, fx *abortFixture, txCtx context.Context) {
	t.Helper()
	stmtCtx, cancel := context.WithTimeout(txCtx, 10*time.Second)
	defer cancel()

	var one int
	requireTxAborted(t, "QueryRow", fx.q.QueryRow(stmtCtx, "SELECT 1").Scan(&one))
	_, err := fx.q.Exec(stmtCtx, "SELECT 1")
	requireTxAborted(t, "Exec", err)
	rows, err := fx.q.Query(stmtCtx, "SELECT 1")
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
	}
	requireTxAborted(t, "Query", err)
}

func requireTxAborted(t *testing.T, op string, err error) {
	t.Helper()
	if !errors.Is(err, spi.ErrTxAborted) || !errors.Is(err, spi.ErrConflict) {
		t.Errorf("%s after the conflict: %v, want spi.ErrTxAborted and spi.ErrConflict", op, err)
		return
	}
	if !hasPgCode(err, pgerrcode.InFailedSQLTransaction) {
		t.Errorf("%s after the conflict: the 25P02 is not in the chain: %v", op, err)
	}
}

// hasPgCode reports whether any PgError in err's tree carries code.
func hasPgCode(err error, code string) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if pe, ok := err.(*pgconn.PgError); ok {
		pgErr = pe
	}
	if pgErr != nil && pgErr.Code == code {
		return true
	}
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if hasPgCode(e, code) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return hasPgCode(u.Unwrap(), code)
	}
	return false
}
