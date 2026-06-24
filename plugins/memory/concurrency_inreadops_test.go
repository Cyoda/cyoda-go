package memory_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// Locking-discipline race tests for the remaining six tx-path operations
// in plugins/memory/entity_store.go. PR #153 (v0.6.3) fixed Save and
// CompareAndSave; issue #176 covers Get, GetAll, Delete, DeleteAll,
// Exists, and Count.
//
// Each operation reads tx.RolledBack and at least one of
// tx.Buffer / tx.WriteSet / tx.Deletes / tx.ReadSet. Commit and Rollback
// take tx.OpMu.Lock and write to those fields; in-flight ops must take
// tx.OpMu.RLock to be serialised against them. Without the RLock, the
// race detector flags the in-flight read of tx.RolledBack against
// Rollback's write of the same field.
//
// Each test runs many iterations to give the scheduler chances to
// interleave the in-flight op with Rollback. Tolerated errors are the
// legitimate outcomes of a tx that closed mid-op, recognised via
// errors.Is against the SPI tx-state sentinels (issue #200):
//   - spi.ErrTxTerminated      ("already completed" / "rolled back")
//   - spi.ErrTxNotFound        ("not found")
//   - spi.ErrTxCommitInProgress ("already being committed")
//
// See isToleratedClosedTxErr in concurrency_helpers_test.go.
//
// These tests are intended to be run with `go test -race`. Without
// `-race`, they will not exercise the data-race detector but still
// verify the tolerated-error contract.

const inReadOpsRaceIterations = 50

var raceModelRef = spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

// runOpVsRollback drives the standard pattern: spawn a goroutine that
// calls op against the tx context, spawn a goroutine that Rolls Back
// the tx, release both at once. If op returns a non-tolerated error,
// fail. The setup callback runs before the goroutines spawn — use it
// to seed entities the op needs to find.
func runOpVsRollback(
	t *testing.T,
	name string,
	setup func(t *testing.T, ctx context.Context, store spi.EntityStore),
	op func(txCtx context.Context, store spi.EntityStore) error,
) {
	t.Helper()
	for iter := 0; iter < inReadOpsRaceIterations; iter++ {
		func() {
			factory := memory.NewStoreFactory()
			defer factory.Close()
			uuids := newTestUUIDGenerator()
			txMgr := factory.NewTransactionManager(uuids)

			ctx := ctxWithTenant("tenant-A")
			store, err := factory.EntityStore(ctx)
			if err != nil {
				t.Fatalf("%s: EntityStore failed: %v", name, err)
			}

			if setup != nil {
				setup(t, ctx, store)
			}

			txID, txCtx, err := txMgr.Begin(ctx)
			if err != nil {
				t.Fatalf("%s: Begin failed: %v", name, err)
			}

			var wg sync.WaitGroup
			start := make(chan struct{})

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if oerr := op(txCtx, store); oerr != nil {
					if errors.Is(oerr, spi.ErrConflict) || isToleratedClosedTxErr(oerr) {
						return
					}
					t.Errorf("%s: op failed: %v", name, oerr)
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

func raceSeedOne(t *testing.T, ctx context.Context, store spi.EntityStore, id string) {
	t.Helper()
	e := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       id,
			TenantID: "tenant-A",
			ModelRef: raceModelRef,
			State:    "NEW",
		},
		Data: []byte(`{}`),
	}
	if _, err := store.Save(ctx, e); err != nil {
		t.Fatalf("seed Save failed: %v", err)
	}
}

// TestGet_VsRollback_NoRace flags the missing tx.OpMu.RLock around
// Get's reads of tx.RolledBack, tx.Deletes, tx.Buffer, and write to
// tx.ReadSet.
func TestGet_VsRollback_NoRace(t *testing.T) {
	runOpVsRollback(t, "Get",
		func(t *testing.T, ctx context.Context, store spi.EntityStore) {
			raceSeedOne(t, ctx, store, "e-get")
		},
		func(txCtx context.Context, store spi.EntityStore) error {
			_, err := store.Get(txCtx, "e-get")
			return err
		},
	)
}

// TestGetAll_VsRollback_NoRace flags the missing tx.OpMu.RLock around
// GetAll's reads of tx.RolledBack, tx.Buffer, tx.Deletes, and writes
// to tx.ReadSet.
func TestGetAll_VsRollback_NoRace(t *testing.T) {
	runOpVsRollback(t, "GetAll",
		func(t *testing.T, ctx context.Context, store spi.EntityStore) {
			raceSeedOne(t, ctx, store, "e-getall")
		},
		func(txCtx context.Context, store spi.EntityStore) error {
			_, err := store.GetAll(txCtx, raceModelRef)
			return err
		},
	)
}

// TestDelete_VsRollback_NoRace flags the missing tx.OpMu.RLock around
// Delete's read of tx.RolledBack and writes to tx.Deletes / tx.Buffer
// / tx.WriteSet.
func TestDelete_VsRollback_NoRace(t *testing.T) {
	runOpVsRollback(t, "Delete",
		func(t *testing.T, ctx context.Context, store spi.EntityStore) {
			raceSeedOne(t, ctx, store, "e-del")
		},
		func(txCtx context.Context, store spi.EntityStore) error {
			return store.Delete(txCtx, "e-del")
		},
	)
}

// TestDeleteAll_VsRollback_NoRace flags the missing tx.OpMu.RLock
// around DeleteAll's read of tx.RolledBack, iteration of tx.Buffer,
// and writes to tx.Deletes / tx.Buffer / tx.WriteSet.
func TestDeleteAll_VsRollback_NoRace(t *testing.T) {
	runOpVsRollback(t, "DeleteAll",
		func(t *testing.T, ctx context.Context, store spi.EntityStore) {
			raceSeedOne(t, ctx, store, "e-delall")
		},
		func(txCtx context.Context, store spi.EntityStore) error {
			return store.DeleteAll(txCtx, raceModelRef)
		},
	)
}

// TestExists_VsRollback_NoRace flags the missing tx.OpMu.RLock around
// Exists's reads of tx.RolledBack, tx.Deletes, tx.Buffer.
func TestExists_VsRollback_NoRace(t *testing.T) {
	runOpVsRollback(t, "Exists",
		func(t *testing.T, ctx context.Context, store spi.EntityStore) {
			raceSeedOne(t, ctx, store, "e-exists")
		},
		func(txCtx context.Context, store spi.EntityStore) error {
			_, err := store.Exists(txCtx, "e-exists")
			return err
		},
	)
}

// TestCount_VsRollback_NoRace flags the missing tx.OpMu.RLock in
// Count's tx-path. Count delegates to GetAll, so the race lives in
// GetAll; this test pins that delegation correctness.
func TestCount_VsRollback_NoRace(t *testing.T) {
	runOpVsRollback(t, "Count",
		func(t *testing.T, ctx context.Context, store spi.EntityStore) {
			raceSeedOne(t, ctx, store, "e-count")
		},
		func(txCtx context.Context, store spi.EntityStore) error {
			_, err := store.Count(txCtx, raceModelRef)
			return err
		},
	)
}

// TestGetAsAt_VsRollback_NoRace flags the missing tx.OpMu.RLock in
// GetAsAt's tx-path. Surfaced by code review on PR #198 as the same
// race-shape defect issue #176 fixes for the six core ops: GetAsAt
// reads tx.RolledBack and writes tx.ReadSet inside an entityMu.RLock
// region without tx.OpMu.RLock, AND has an inverted lock order
// (entityMu before tx.OpMu). The fix takes tx.OpMu.RLock first when a
// tx is present, then entityMu.RLock — matching every other tx-path
// method.
func TestGetAsAt_VsRollback_NoRace(t *testing.T) {
	runOpVsRollback(t, "GetAsAt",
		func(t *testing.T, ctx context.Context, store spi.EntityStore) {
			raceSeedOne(t, ctx, store, "e-asat")
		},
		func(txCtx context.Context, store spi.EntityStore) error {
			_, err := store.GetAsAt(txCtx, "e-asat", time.Now())
			return err
		},
	)
}

// runOpVsCommit is the Commit-as-contender variant of runOpVsRollback.
// Commit iterates tx.Buffer (txmanager.go flush) and tx.Deletes under
// tx.OpMu.Lock; an in-flight op that mutates those maps without
// tx.OpMu.RLock races against the iteration. This contender exposes
// races that runOpVsRollback (which only writes tx.RolledBack/Closed)
// does not.
func runOpVsCommit(
	t *testing.T,
	name string,
	setup func(t *testing.T, ctx context.Context, store spi.EntityStore),
	op func(txCtx context.Context, store spi.EntityStore) error,
) {
	t.Helper()
	for iter := 0; iter < inReadOpsRaceIterations; iter++ {
		func() {
			factory := memory.NewStoreFactory()
			defer factory.Close()
			uuids := newTestUUIDGenerator()
			txMgr := factory.NewTransactionManager(uuids)

			ctx := ctxWithTenant("tenant-A")
			store, err := factory.EntityStore(ctx)
			if err != nil {
				t.Fatalf("%s: EntityStore failed: %v", name, err)
			}

			if setup != nil {
				setup(t, ctx, store)
			}

			txID, txCtx, err := txMgr.Begin(ctx)
			if err != nil {
				t.Fatalf("%s: Begin failed: %v", name, err)
			}

			var wg sync.WaitGroup
			start := make(chan struct{})

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if oerr := op(txCtx, store); oerr != nil {
					if errors.Is(oerr, spi.ErrConflict) || isToleratedClosedTxErr(oerr) {
						return
					}
					t.Errorf("%s: op failed: %v", name, oerr)
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_ = txMgr.Commit(ctx, txID)
			}()

			close(start)
			wg.Wait()
		}()
	}
}

// TestDelete_VsCommit_NoRace pairs Delete against Commit. Commit's
// flush iterates tx.Buffer and tx.Deletes under tx.OpMu.Lock; Delete
// writes both maps. Without tx.OpMu.RLock on Delete, the writes race
// against Commit's iteration. The Rollback-paired test catches the
// tx.RolledBack-flag race only; this test catches the map-mutation
// race directly.
func TestDelete_VsCommit_NoRace(t *testing.T) {
	runOpVsCommit(t, "Delete",
		func(t *testing.T, ctx context.Context, store spi.EntityStore) {
			raceSeedOne(t, ctx, store, "e-del-vs-commit")
		},
		func(txCtx context.Context, store spi.EntityStore) error {
			return store.Delete(txCtx, "e-del-vs-commit")
		},
	)
}

// TestDeleteAll_VsCommit_NoRace pairs DeleteAll against Commit for
// the same reason as TestDelete_VsCommit_NoRace: DeleteAll iterates
// AND writes tx.Buffer / tx.Deletes / tx.WriteSet, all of which Commit
// also touches under tx.OpMu.Lock during flush.
func TestDeleteAll_VsCommit_NoRace(t *testing.T) {
	runOpVsCommit(t, "DeleteAll",
		func(t *testing.T, ctx context.Context, store spi.EntityStore) {
			raceSeedOne(t, ctx, store, "e-delall-vs-commit")
		},
		func(txCtx context.Context, store spi.EntityStore) error {
			return store.DeleteAll(txCtx, raceModelRef)
		},
	)
}
