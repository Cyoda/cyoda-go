package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Commit bumps lastSubmitTime (step 4) before its sqlTx commits, holding
// commitMu across the whole flush. Begin floors SnapshotTime to
// lastSubmitTime. If Begin does not wait for commitMu, a transaction begun
// mid-flush has SnapshotTime >= that commit's submit time while the rows
// are not yet visible on readDB — and the conflict check at Commit then
// treats that commit as "before my snapshot" and skips it: a lost update.
//
// The test stands in for a mid-flush commit by holding commitMu and
// bumping lastSubmitTime, exactly the state Commit is in between step 4
// and sqlTx.Commit.
func TestBegin_WaitsForInFlightCommit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "begin-gate.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()
	m := f.tm
	ctx := attrInternalCtx("tenant-A", "alice", spi.PrincipalUser)

	type beginResult struct {
		txID string
		ctx  context.Context
		err  error
	}
	done := make(chan beginResult, 1)

	// Stand in for a commit between step 4 and sqlTx.Commit: hold commitMu
	// while bumping lastSubmitTime, start Begin concurrently, and assert it
	// is still blocked after 150ms — all inside the commitMu hold. A
	// t.Fatalf in the select still runs the deferred Unlock via
	// runtime.Goexit.
	var bumped int64
	func() {
		m.commitMu.Lock()
		defer m.commitMu.Unlock()

		bumped = func() int64 {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.lastSubmitTime = m.factory.clock.Now().UnixMicro() + 5_000_000 // 5s ahead
			return m.lastSubmitTime
		}()

		go func() {
			id, txCtx, err := m.Begin(ctx)
			done <- beginResult{id, txCtx, err}
		}()

		select {
		case r := <-done:
			t.Fatalf("Begin returned while a commit was in flight (txID=%q err=%v); it must wait for commitMu", r.txID, r.err)
		case <-time.After(150 * time.Millisecond):
			// Blocked, as required.
		}
	}()

	var r beginResult
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Begin did not return after commitMu was released")
	}
	if r.err != nil {
		t.Fatalf("Begin: %v", r.err)
	}
	defer func() { _ = m.Rollback(r.ctx, r.txID) }()

	tx := spi.GetTransaction(r.ctx)
	if got := tx.SnapshotTime.UnixMicro(); got < bumped {
		t.Fatalf("SnapshotTime %d is below the flushed commit's submit time %d", got, bumped)
	}
}
