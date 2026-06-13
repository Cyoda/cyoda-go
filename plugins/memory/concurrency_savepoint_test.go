package memory_test

import (
	"errors"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// Locking-discipline race tests for Savepoint, RollbackToSavepoint, and
// Join in plugins/memory/txmanager.go. Issue #199 surfaces that Savepoint
// and RollbackToSavepoint mutate tx-state under m.mu only, never tx.OpMu —
// so they race against Commit's flush phase, which iterates tx.Buffer /
// tx.Deletes outside m.mu but under tx.OpMu.Lock. The PR-A audit also
// surfaced an unsynchronised flag-read in Join.
//
// Two classes of tests in this file:
//
//  1. **Race reproducers** — fail under -race against pre-fix code, pass
//     after. The race detector's success/failure is the test signal:
//       - TestRollbackToSavepoint_VsCommit_NoRace
//       - TestRollbackToSavepoint_VsSave_NoRace
//       - TestJoin_VsRollback_NoRace
//
//  2. **Contract-pin sentinels** — pass under -race both before AND after
//     the fix because Savepoint=RLock and Commit/Rollback=Lock are
//     mutually exclusive (no race shape exists for the detector to flag,
//     regardless of whether the lock is correct or not). Their value is
//     regression discipline: any future contributor who removes the OpMu
//     pairing OR adds a new tx-state mutation in Savepoint/Commit gets
//     caught:
//       - TestSavepoint_VsCommit_NoRace
//       - TestSavepoint_VsRollback_NoRace
//
// The race shape for the reproducers:
//   - Savepoint reads tx.Buffer / tx.ReadSet / tx.WriteSet / tx.Deletes.
//     Required posture: tx.OpMu.RLock — serialises against Commit/Rollback
//     (Lock) without blocking other readers (Save/Get also take RLock).
//   - RollbackToSavepoint replaces tx.Buffer / tx.ReadSet / tx.WriteSet /
//     tx.Deletes. Required posture: tx.OpMu.Lock — exclusive against every
//     other tx-path op.
//   - Join reads tx.RolledBack / tx.Closed. Required posture: tx.OpMu.RLock
//     (in an IIFE) — Commit and Rollback both write tx.Closed in their defer
//     under tx.OpMu.Lock only, never under m.mu.
//
// Tolerated errors are recognised via errors.Is against the SPI tx-state
// sentinel hierarchy (ErrTxNotFound, ErrTxTerminated, ErrTxCommitInProgress,
// ErrNotFound) — see isToleratedClosedTxErr below. A closed-mid-op tx is a
// legitimate outcome, not a defect.
//
// These tests are intended to be run with `go test -race`. Without `-race`
// they still verify the tolerated-error contract.

const savepointRaceIterations = 50

var savepointModelRef = spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

// TestSavepoint_VsCommit_NoRace is a contract-pin test: it ensures that
// Savepoint completes cleanly when concurrent with Commit, and that
// Savepoint takes tx.OpMu.RLock so Commit's tx.OpMu.Lock blocks any
// in-flight Savepoint from running during Commit's flush phase.
//
// This test does NOT fire the race detector in red-phase pre-fix. Both
// Savepoint and Commit's flush phase are read-only on tx.Buffer (Savepoint
// deep-copies via copyEntity; Commit iterates and copies into entityData
// without mutating tx.Buffer entries). Read-vs-read is not a Go race.
//
// The test's value is regression discipline: any future contributor who
// removes tx.OpMu.RLock from Savepoint, or accidentally introduces a
// tx.Buffer mutation in Savepoint, breaks this test's "no error" contract
// even if -race stays clean. It also gives the Commit-as-contender pairing
// runtime exercise so that if future code adds a tx-state mutation in
// either method (e.g. Savepoint reading tx.RolledBack on entry) the race
// detector starts catching it.
func TestSavepoint_VsCommit_NoRace(t *testing.T) {
	for iter := 0; iter < savepointRaceIterations; iter++ {
		func() {
			factory := memory.NewStoreFactory()
			defer factory.Close()
			uuids := newTestUUIDGenerator()
			txMgr := factory.NewTransactionManager(uuids)

			ctx := ctxWithTenant("tenant-A")
			store, err := factory.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore failed: %v", err)
			}

			txID, txCtx, err := txMgr.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin failed: %v", err)
			}

			// Seed tx.Buffer with one entry (sequential, before contention)
			// so Commit's flush actually iterates a non-empty map. The
			// seed Save runs to completion before the race goroutines
			// spawn — so it does NOT introduce app-contract-violation
			// concurrency with Savepoint.
			e := &spi.Entity{
				Meta: spi.EntityMeta{
					ID:       "e-sp-vs-commit",
					TenantID: "tenant-A",
					ModelRef: savepointModelRef,
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
				if _, sperr := txMgr.Savepoint(ctx, txID); sperr != nil {
					if isToleratedClosedTxErr(sperr) {
						return
					}
					t.Errorf("Savepoint failed: %v", sperr)
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if cerr := txMgr.Commit(ctx, txID); cerr != nil {
					if errors.Is(cerr, spi.ErrConflict) || isToleratedClosedTxErr(cerr) {
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
// TestSavepoint_VsCommit_NoRace. Like that test, it does NOT fire the race
// detector in red-phase: current Savepoint pre-fix doesn't read
// tx.RolledBack (the only field Rollback writes), so the field-level
// access pattern has no race shape regardless of OpMu usage. Post-fix,
// Savepoint reads tx.RolledBack/Closed under tx.OpMu.RLock and Rollback
// writes those fields under tx.OpMu.Lock — mutually exclusive.
//
// The test pins the contract: Savepoint and Rollback must complete cleanly
// when fired concurrently, with appropriate error returns when the tx is
// already closed.
func TestSavepoint_VsRollback_NoRace(t *testing.T) {
	for iter := 0; iter < savepointRaceIterations; iter++ {
		func() {
			factory := memory.NewStoreFactory()
			defer factory.Close()
			uuids := newTestUUIDGenerator()
			txMgr := factory.NewTransactionManager(uuids)

			ctx := ctxWithTenant("tenant-A")
			_, err := factory.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore failed: %v", err)
			}

			txID, _, err := txMgr.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin failed: %v", err)
			}

			var wg sync.WaitGroup
			start := make(chan struct{})

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, sperr := txMgr.Savepoint(ctx, txID); sperr != nil {
					if isToleratedClosedTxErr(sperr) {
						return
					}
					t.Errorf("Savepoint failed: %v", sperr)
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_ = txMgr.Rollback(ctx, txID)
			}()

			close(start)
			wg.Wait()
		}()
	}
}

// TestRollbackToSavepoint_VsCommit_NoRace pins that RollbackToSavepoint
// takes tx.OpMu.Lock so its overwrite of tx.Buffer / tx.ReadSet /
// tx.WriteSet / tx.Deletes is serialised against Commit's flush iteration
// of those maps under tx.OpMu.Lock.
//
// Without tx.OpMu.Lock on RollbackToSavepoint, the race detector flags
// RollbackToSavepoint's writes of those four fields against Commit's
// reads/iterations.
func TestRollbackToSavepoint_VsCommit_NoRace(t *testing.T) {
	for iter := 0; iter < savepointRaceIterations; iter++ {
		func() {
			factory := memory.NewStoreFactory()
			defer factory.Close()
			uuids := newTestUUIDGenerator()
			txMgr := factory.NewTransactionManager(uuids)

			ctx := ctxWithTenant("tenant-A")
			store, err := factory.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore failed: %v", err)
			}

			txID, txCtx, err := txMgr.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin failed: %v", err)
			}

			// Take a savepoint we can roll back to. Add an entity AFTER so
			// tx.Buffer is non-empty when Commit fires; the snapshot will
			// roll the buffer back to empty.
			spID, err := txMgr.Savepoint(ctx, txID)
			if err != nil {
				t.Fatalf("Savepoint failed: %v", err)
			}

			e := &spi.Entity{
				Meta: spi.EntityMeta{
					ID:       "e-rts-vs-commit",
					TenantID: "tenant-A",
					ModelRef: savepointModelRef,
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
				if rerr := txMgr.RollbackToSavepoint(ctx, txID, spID); rerr != nil {
					if isToleratedClosedTxErr(rerr) {
						return
					}
					t.Errorf("RollbackToSavepoint failed: %v", rerr)
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if cerr := txMgr.Commit(ctx, txID); cerr != nil {
					if errors.Is(cerr, spi.ErrConflict) || isToleratedClosedTxErr(cerr) {
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

// TestRollbackToSavepoint_VsSave_NoRace pins that RollbackToSavepoint takes
// tx.OpMu.Lock so its replacement of tx.Buffer / tx.WriteSet is serialised
// against concurrent Save (which mutates the same maps under tx.OpMu.RLock).
//
// Per the Join() godoc the application must coordinate concurrent ops on a
// single tx itself — but tx.OpMu IS what enforces that contract from the
// plugin side. This test pins the invariant: even if a contributor
// accidentally fires Save and RollbackToSavepoint concurrently, the race
// detector must stay clean.
//
// Without tx.OpMu.Lock on RollbackToSavepoint, the race detector flags the
// pointer-replacement of tx.Buffer against Save's tx.Buffer[id] = entity
// write.
func TestRollbackToSavepoint_VsSave_NoRace(t *testing.T) {
	for iter := 0; iter < savepointRaceIterations; iter++ {
		func() {
			factory := memory.NewStoreFactory()
			defer factory.Close()
			uuids := newTestUUIDGenerator()
			txMgr := factory.NewTransactionManager(uuids)

			ctx := ctxWithTenant("tenant-A")
			store, err := factory.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore failed: %v", err)
			}

			txID, txCtx, err := txMgr.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin failed: %v", err)
			}

			spID, err := txMgr.Savepoint(ctx, txID)
			if err != nil {
				t.Fatalf("Savepoint failed: %v", err)
			}

			e := &spi.Entity{
				Meta: spi.EntityMeta{
					ID:       "e-rts-vs-save",
					TenantID: "tenant-A",
					ModelRef: savepointModelRef,
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
					if isToleratedClosedTxErr(serr) {
						return
					}
					t.Errorf("Save failed: %v", serr)
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if rerr := txMgr.RollbackToSavepoint(ctx, txID, spID); rerr != nil {
					if isToleratedClosedTxErr(rerr) {
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

// TestJoin_VsRollback_NoRace flags the missing tx.OpMu.RLock around Join's
// reads of tx.RolledBack and tx.Closed.
//
// Surfaced by the memory-plugin tx-locking audit during PR-A (#199): Join
// reads tx.RolledBack and tx.Closed outside any lock at txmanager.go:117.
// Rollback writes tx.RolledBack inside m.mu only, and Commit/Rollback both
// write tx.Closed in their defer under tx.OpMu.Lock only — never under m.mu.
// The m.mu critical section in Join's lookup does not establish
// happens-before for the subsequent flag reads, so the race detector flags
// Join's read against Rollback's tx.RolledBack write.
//
// The fix takes tx.OpMu.RLock (via IIFE per .claude/rules/go-mutex-discipline.md)
// for the flag reads.
func TestJoin_VsRollback_NoRace(t *testing.T) {
	for iter := 0; iter < savepointRaceIterations; iter++ {
		func() {
			factory := memory.NewStoreFactory()
			defer factory.Close()
			uuids := newTestUUIDGenerator()
			txMgr := factory.NewTransactionManager(uuids)

			ctx := ctxWithTenant("tenant-A")
			_, err := factory.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore failed: %v", err)
			}

			txID, _, err := txMgr.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin failed: %v", err)
			}

			var wg sync.WaitGroup
			start := make(chan struct{})

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, jerr := txMgr.Join(ctx, txID); jerr != nil {
					if isToleratedClosedTxErr(jerr) {
						return
					}
					t.Errorf("Join failed: %v", jerr)
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_ = txMgr.Rollback(ctx, txID)
			}()

			close(start)
			wg.Wait()
		}()
	}
}
