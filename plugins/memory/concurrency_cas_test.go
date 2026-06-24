package memory_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestCompareAndSave_TxBufferWriteVsCommit_NoRace exercises the locking
// discipline that CompareAndSave's tx-path must follow: an in-flight tx
// mutation must hold tx.OpMu.RLock so it is serialised against Commit /
// Rollback (which take tx.OpMu.Lock).
//
// The contract on TransactionState (see txmanager.Join godoc) is that callers
// must coordinate concurrent writes within a single tx themselves; the race
// the plugin must defend against is plugin-side tx access vs Commit/Rollback.
// Without tx.OpMu.RLock acquisition in CompareAndSave:
//
//   - The writer's read of tx.RolledBack races with Rollback's write to
//     tx.RolledBack. Rollback takes tx.OpMu.Lock specifically so in-flight
//     ops can finish first; honoring that contract requires the op to take
//     tx.OpMu.RLock.
//   - The writer's writes to tx.Buffer / tx.WriteSet race with Commit's
//     reads of those maps under tx.OpMu.Lock.
//
// The race detector flags either pattern; the fixed path passes clean.
//
// This test is most useful with -race.
func TestCompareAndSave_TxBufferWriteVsCommit_NoRace(t *testing.T) {
	// Iterate to give the scheduler many chances to interleave the writer's
	// buffer write with the committer's flush.
	const iterations = 50

	for iter := 0; iter < iterations; iter++ {
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
			modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

			// Pre-seed an entity with a known TransactionID so the
			// CompareAndSave call has a committed predecessor to match
			// against.
			seed := &spi.Entity{
				Meta: spi.EntityMeta{
					ID:            "e-cas",
					TenantID:      "tenant-A",
					ModelRef:      modelRef,
					State:         "NEW",
					TransactionID: "tx-seed",
				},
				Data: []byte(`{"v":0}`),
			}
			if _, err := store.Save(ctx, seed); err != nil {
				t.Fatalf("seed Save failed: %v", err)
			}

			txID, txCtx, err := txMgr.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin failed: %v", err)
			}

			var wg sync.WaitGroup
			start := make(chan struct{})

			// Single writer goroutine driving CompareAndSave.
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				e := &spi.Entity{
					Meta: spi.EntityMeta{
						ID:            "e-cas",
						TenantID:      "tenant-A",
						ModelRef:      modelRef,
						State:         "NEW",
						TransactionID: "tx-new",
					},
					Data: []byte(fmt.Sprintf(`{"iter":%d}`, iter)),
				}
				_, cerr := store.CompareAndSave(txCtx, e, "tx-seed")
				if cerr == nil || errors.Is(cerr, spi.ErrConflict) || isToleratedClosedTxErr(cerr) {
					return
				}
				t.Errorf("CompareAndSave failed: %v", cerr)
			}()

			// Rollback goroutine fires Rollback concurrently with the
			// writer's buffer mutation. Rollback writes tx.RolledBack
			// under tx.OpMu.Lock; CompareAndSave reads tx.RolledBack and
			// must take tx.OpMu.RLock to read it without a race.
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

// TestSave_TxBufferWriteVsRollback_NoRace mirrors the CompareAndSave race
// test against Save(). The memory-plugin Save() tx-path also reads
// tx.RolledBack and writes tx.Buffer / tx.WriteSet, and must take
// tx.OpMu.RLock for the same reason CompareAndSave does. Resolving the
// CompareAndSave bug exposed that Save had the identical defect (per Gate 6
// in CLAUDE.md, "resolve, don't defer").
func TestSave_TxBufferWriteVsRollback_NoRace(t *testing.T) {
	const iterations = 50

	for iter := 0; iter < iterations; iter++ {
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
			modelRef := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

			txID, txCtx, err := txMgr.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin failed: %v", err)
			}

			var wg sync.WaitGroup
			start := make(chan struct{})

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				e := &spi.Entity{
					Meta: spi.EntityMeta{
						ID:            "e-save",
						TenantID:      "tenant-A",
						ModelRef:      modelRef,
						State:         "NEW",
						TransactionID: "tx-new",
					},
					Data: []byte(fmt.Sprintf(`{"iter":%d}`, iter)),
				}
				_, serr := store.Save(txCtx, e)
				if serr == nil || isToleratedClosedTxErr(serr) {
					return
				}
				t.Errorf("Save failed: %v", serr)
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
