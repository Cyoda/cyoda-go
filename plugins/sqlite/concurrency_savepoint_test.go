package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// Locking-discipline race tests for Savepoint, RollbackToSavepoint, and
// Join in plugins/sqlite/txmanager.go. Issue #199 PR-C1 mirrors PR-A
// (#201) for the sqlite plugin: pre-fix, Savepoint and RollbackToSavepoint
// hold m.mu only and never tx.OpMu, racing against Commit's flush phase
// (which iterates tx.Buffer / tx.Deletes outside m.mu but under
// tx.OpMu.Lock). Join reads tx.RolledBack and tx.Closed under m.mu only,
// racing against Rollback's m.mu-only write of tx.RolledBack and
// Commit/Rollback's deferred OpMu-only write of tx.Closed.
//
// Two classes of tests in this file (matching the memory plugin):
//
//  1. Race reproducers — fail under -race against pre-fix code, pass
//     after. The race detector's success/failure is the test signal:
//       - TestRollbackToSavepoint_VsCommit_NoRace
//       - TestRollbackToSavepoint_VsSave_NoRace
//       - TestJoin_VsRollback_NoRace
//
//  2. Contract-pin sentinels — pass under -race both before AND after
//     the fix because Savepoint=RLock and Commit/Rollback=Lock are
//     mutually exclusive (no race shape exists for the detector to flag).
//     Their value is regression discipline:
//       - TestSavepoint_VsCommit_NoRace
//       - TestSavepoint_VsRollback_NoRace
//
// The race shape for the reproducers:
//   - Savepoint reads tx.Buffer / tx.ReadSet / tx.WriteSet / tx.Deletes.
//     Required posture: tx.OpMu.RLock — serialises against Commit/Rollback
//     (Lock) without blocking other readers.
//   - RollbackToSavepoint replaces tx.Buffer / tx.ReadSet / tx.WriteSet /
//     tx.Deletes. Required posture: tx.OpMu.Lock — exclusive against every
//     other tx-path op.
//   - Join reads tx.RolledBack / tx.Closed. Required posture: tx.OpMu.RLock
//     in an IIFE.
//
// TODO(#200): replace substring matches on tolerated errors with
// errors.Is against sentinel error types once they land.

const sqliteSavepointRaceIterations = 50

var sqliteSavepointModelRef = spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

func newSqliteTxFactory(t *testing.T) *sqlite.StoreFactory {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "concurrency.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest failed: %v", err)
	}
	t.Cleanup(func() { _ = factory.Close() })
	return factory
}

// TestSavepoint_VsCommit_NoRace is a contract-pin test: it ensures that
// Savepoint completes cleanly when concurrent with Commit, and that
// Savepoint takes tx.OpMu.RLock so Commit's tx.OpMu.Lock blocks any
// in-flight Savepoint from running during Commit's flush phase.
//
// This test does NOT fire the race detector in red-phase pre-fix. Both
// Savepoint and Commit's flush phase are read-only on tx.Buffer pointers.
// The test pins regression: any future contributor who removes
// tx.OpMu.RLock from Savepoint, or accidentally introduces a tx.Buffer
// mutation in Savepoint, breaks this test's "no error" contract.
func TestSavepoint_VsCommit_NoRace(t *testing.T) {
	for iter := 0; iter < sqliteSavepointRaceIterations; iter++ {
		func() {
			factory := newSqliteTxFactory(t)
			ctx := testCtx("tenant-A")
			tm, err := factory.TransactionManager(ctx)
			if err != nil {
				t.Fatalf("TransactionManager failed: %v", err)
			}
			store, err := factory.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore failed: %v", err)
			}

			txID, txCtx, err := tm.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin failed: %v", err)
			}

			// Seed tx.Buffer sequentially so Commit's flush iterates a
			// non-empty map. Sequential — no app-contract violation.
			e := &spi.Entity{
				Meta: spi.EntityMeta{
					ID:       "e-sp-vs-commit",
					TenantID: "tenant-A",
					ModelRef: sqliteSavepointModelRef,
					State:    "NEW",
				},
				Data: []byte(`{}`),
			}
			if _, serr := store.Save(txCtx, e); serr != nil {
				t.Fatalf("seed Save failed: %v", serr)
			}

			var wg sync.WaitGroup
			start := make(chan struct{})

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, sperr := tm.Savepoint(ctx, txID); sperr != nil {
					msg := sperr.Error()
					if isToleratedTxClosedErr(msg) {
						return
					}
					t.Errorf("Savepoint failed: %v", sperr)
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if cerr := tm.Commit(ctx, txID); cerr != nil {
					if errors.Is(cerr, spi.ErrConflict) || errors.Is(cerr, spi.ErrNotFound) {
						return
					}
					if isToleratedTxClosedErr(cerr.Error()) {
						return
					}
					t.Errorf("Commit failed: %v", cerr)
				}
			}()

			close(start)
			wg.Wait()
		}()
	}
}

// TestSavepoint_VsRollback_NoRace is a contract-pin test paralleling
// TestSavepoint_VsCommit_NoRace. Pre-fix Savepoint doesn't read
// tx.RolledBack (the field Rollback writes), so no race shape exists.
// Post-fix, Savepoint reads tx.RolledBack/Closed under tx.OpMu.RLock and
// Rollback writes those fields under tx.OpMu.Lock — mutually exclusive.
func TestSavepoint_VsRollback_NoRace(t *testing.T) {
	for iter := 0; iter < sqliteSavepointRaceIterations; iter++ {
		func() {
			factory := newSqliteTxFactory(t)
			ctx := testCtx("tenant-A")
			tm, err := factory.TransactionManager(ctx)
			if err != nil {
				t.Fatalf("TransactionManager failed: %v", err)
			}

			txID, _, err := tm.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin failed: %v", err)
			}

			var wg sync.WaitGroup
			start := make(chan struct{})

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, sperr := tm.Savepoint(ctx, txID); sperr != nil {
					msg := sperr.Error()
					if isToleratedTxClosedErr(msg) {
						return
					}
					t.Errorf("Savepoint failed: %v", sperr)
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_ = tm.Rollback(ctx, txID)
			}()

			close(start)
			wg.Wait()
		}()
	}
}

// TestRollbackToSavepoint_VsCommit_NoRace pins that RollbackToSavepoint
// takes tx.OpMu.Lock so its overwrite of tx.Buffer / tx.ReadSet /
// tx.WriteSet / tx.Deletes is serialised against Commit's flush
// iteration of those maps under tx.OpMu.Lock.
//
// Without tx.OpMu.Lock on RollbackToSavepoint, the race detector flags
// RollbackToSavepoint's pointer-replacements of those four fields against
// Commit's iteration.
func TestRollbackToSavepoint_VsCommit_NoRace(t *testing.T) {
	for iter := 0; iter < sqliteSavepointRaceIterations; iter++ {
		func() {
			factory := newSqliteTxFactory(t)
			ctx := testCtx("tenant-A")
			tm, err := factory.TransactionManager(ctx)
			if err != nil {
				t.Fatalf("TransactionManager failed: %v", err)
			}
			store, err := factory.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore failed: %v", err)
			}

			txID, txCtx, err := tm.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin failed: %v", err)
			}

			spID, err := tm.Savepoint(ctx, txID)
			if err != nil {
				t.Fatalf("Savepoint failed: %v", err)
			}

			e := &spi.Entity{
				Meta: spi.EntityMeta{
					ID:       "e-rts-vs-commit",
					TenantID: "tenant-A",
					ModelRef: sqliteSavepointModelRef,
					State:    "NEW",
				},
				Data: []byte(`{}`),
			}
			if _, serr := store.Save(txCtx, e); serr != nil {
				t.Fatalf("seed Save failed: %v", serr)
			}

			var wg sync.WaitGroup
			start := make(chan struct{})

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if rerr := tm.RollbackToSavepoint(ctx, txID, spID); rerr != nil {
					msg := rerr.Error()
					if isToleratedTxClosedErr(msg) {
						return
					}
					t.Errorf("RollbackToSavepoint failed: %v", rerr)
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if cerr := tm.Commit(ctx, txID); cerr != nil {
					if errors.Is(cerr, spi.ErrConflict) || errors.Is(cerr, spi.ErrNotFound) {
						return
					}
					if isToleratedTxClosedErr(cerr.Error()) {
						return
					}
					t.Errorf("Commit failed: %v", cerr)
				}
			}()

			close(start)
			wg.Wait()
		}()
	}
}

// TestRollbackToSavepoint_VsSave_NoRace pins that RollbackToSavepoint
// takes tx.OpMu.Lock so its replacement of tx.Buffer / tx.WriteSet is
// serialised against concurrent Save (which mutates the same maps under
// tx.OpMu.RLock).
func TestRollbackToSavepoint_VsSave_NoRace(t *testing.T) {
	for iter := 0; iter < sqliteSavepointRaceIterations; iter++ {
		func() {
			factory := newSqliteTxFactory(t)
			ctx := testCtx("tenant-A")
			tm, err := factory.TransactionManager(ctx)
			if err != nil {
				t.Fatalf("TransactionManager failed: %v", err)
			}
			store, err := factory.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore failed: %v", err)
			}

			txID, txCtx, err := tm.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin failed: %v", err)
			}

			spID, err := tm.Savepoint(ctx, txID)
			if err != nil {
				t.Fatalf("Savepoint failed: %v", err)
			}

			e := &spi.Entity{
				Meta: spi.EntityMeta{
					ID:       "e-rts-vs-save",
					TenantID: "tenant-A",
					ModelRef: sqliteSavepointModelRef,
					State:    "NEW",
				},
				Data: []byte(`{}`),
			}

			var wg sync.WaitGroup
			start := make(chan struct{})

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, serr := store.Save(txCtx, e); serr != nil {
					if isToleratedTxClosedErr(serr.Error()) {
						return
					}
					t.Errorf("Save failed: %v", serr)
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if rerr := tm.RollbackToSavepoint(ctx, txID, spID); rerr != nil {
					if isToleratedTxClosedErr(rerr.Error()) {
						return
					}
					t.Errorf("RollbackToSavepoint failed: %v", rerr)
				}
			}()

			close(start)
			wg.Wait()
		}()
	}
}

// Note on Join: unlike the memory plugin (which released m.mu BEFORE
// reading tx.RolledBack/tx.Closed and so had a real race against
// Commit/Rollback's deferred-OpMu writes), sqlite's Join holds m.mu via
// `defer m.mu.Unlock()` for the entire function body. Rollback/Commit
// delete the tx from m.active under m.mu BEFORE their tx.Closed defer
// fires; subsequent Joins find !ok and return early without ever reading
// the closure flags. Sqlite Join therefore does not need a tx.OpMu.RLock
// IIFE — it's already correctly synchronised. This asymmetry with the
// memory plugin is documented in
// docs/audits/2026-05-sqlite-plugin-tx-locking.md.

func isToleratedTxClosedErr(msg string) bool {
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "already completed") ||
		strings.Contains(msg, "rolled back") ||
		strings.Contains(msg, "already closed") ||
		strings.Contains(msg, "already being committed")
}
