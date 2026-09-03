package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Commit bumps lastSubmitTime (step 4) before its sqlTx commits, holding
// the commit gate across the whole flush. Begin floors SnapshotTime to
// lastSubmitTime. If Begin does not wait for the gate, a transaction begun
// mid-flush has SnapshotTime >= that commit's submit time while the rows
// are not yet visible on readDB — and the conflict check at Commit then
// treats that commit as "before my snapshot" and skips it: a lost update.
//
// The test stands in for a mid-flush commit by holding the gate and
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

	// Stand in for a commit between step 4 and sqlTx.Commit: hold the gate
	// while bumping lastSubmitTime, start Begin concurrently, and assert it
	// is still blocked after 150ms — all inside the gate hold. A
	// t.Fatalf in the select still runs the deferred release via
	// runtime.Goexit.
	var bumped int64
	func() {
		_ = m.acquireCommitGate(context.Background())
		defer m.releaseCommitGate()

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
			t.Fatalf("Begin returned while a commit was in flight (txID=%q err=%v); it must wait for the commit gate", r.txID, r.err)
		case <-time.After(150 * time.Millisecond):
			// Blocked, as required.
		}
	}()

	var r beginResult
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Begin did not return after the commit gate was released")
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

// A transaction begun while another transaction's commit is in flight must
// see that commit's rows once Begin returns: Begin queues behind the commit gate,
// so its snapshot floor is captured only after the flush is committed on
// every connection. The invariant under test is conditional — a snapshot
// floored at or after a commit's submit time implies that commit's rows are
// visible — and the test asserts both directions, so it does not depend on
// which of the two queued goroutines the runtime wakes first.
//
// No production hook is used to order the two goroutines: the test holds
// the gate itself, queues T_A's Commit behind it, then queues T_B's Begin,
// and releases. Go's channel wait queue is FIFO, so T_A commits first in
// practice; the assertions do not rely on it.
func TestBegin_SeesConcurrentCommitRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "begin-sees-commit.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()
	m := f.tm
	ctx := attrInternalCtx("tenant-gate", "alice", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-gate", ModelVersion: "1"}

	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	// A committed row whose transaction ID predates T_A.
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-seed", TenantID: "tenant-gate", ModelRef: ref, State: "open"},
		Data: []byte(`{"n":0}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	seed, err := store.Get(ctx, "e-seed")
	if err != nil {
		t.Fatalf("seed Get: %v", err)
	}
	preTxID := seed.Meta.TransactionID

	// T_A: a transaction with one brand-new row, staged and ready to commit.
	txA, ctxA, err := m.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin T_A: %v", err)
	}
	if _, err := store.Save(ctxA, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-fromA", TenantID: "tenant-gate", ModelRef: ref, State: "open"},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("T_A Save: %v", err)
	}
	if _, err := store.Save(ctxA, &spi.Entity{
		Meta: seed.Meta,
		Data: []byte(`{"n":2}`),
	}); err != nil {
		t.Fatalf("T_A Save of the seed: %v", err)
	}

	commitDone := make(chan error, 1)
	type beginResult struct {
		txID string
		ctx  context.Context
		err  error
	}
	beginDone := make(chan beginResult, 1)

	func() {
		_ = m.acquireCommitGate(context.Background())
		defer m.releaseCommitGate()
		go func() { commitDone <- m.Commit(ctxA, txA) }()
		// Give T_A's Commit time to reach the gate before T_B's Begin
		// queues behind it.
		time.Sleep(100 * time.Millisecond)
		go func() {
			id, c, err := m.Begin(ctx)
			beginDone <- beginResult{id, c, err}
		}()
		time.Sleep(100 * time.Millisecond)
	}()

	select {
	case err := <-commitDone:
		if err != nil {
			t.Fatalf("T_A Commit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("T_A Commit did not return")
	}
	var rb beginResult
	select {
	case rb = <-beginDone:
	case <-time.After(10 * time.Second):
		t.Fatal("T_B Begin did not return")
	}
	if rb.err != nil {
		t.Fatalf("Begin T_B: %v", rb.err)
	}
	defer func() { _ = m.Rollback(rb.ctx, rb.txID) }()

	submitA := func() int64 {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.lastSubmitTime
	}()

	// T_B's overlay read.
	iterable := store
	it, err := iterable.Iterate(rb.ctx, ref, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("T_B Iterate: %v", err)
	}
	seen := map[string]bool{}
	for it.Next() {
		seen[it.Entity().Meta.ID] = true
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Iterate Err: %v", err)
	}

	txB := spi.GetTransaction(rb.ctx)
	if txB.SnapshotTime.UnixMicro() >= submitA {
		// The snapshot is floored at or after T_A's commit, so T_A's rows
		// MUST be visible on readDB — that is the whole guarantee Begin's
		// gate hold buys.
		if !seen["e-fromA"] {
			t.Fatalf("T_B SnapshotTime %d >= T_A submit time %d but T_A's row is invisible; seen=%v",
				txB.SnapshotTime.UnixMicro(), submitA, seen)
		}
	} else {
		// T_B's Begin won the gate ahead of T_A's commit: T_A's row is
		// legitimately outside T_B's snapshot.
		if seen["e-fromA"] {
			t.Fatalf("T_B SnapshotTime %d < T_A submit time %d yet T_A's row is visible; seen=%v",
				txB.SnapshotTime.UnixMicro(), submitA, seen)
		}
		t.Logf("T_B began ahead of T_A's commit (snapshot %d < submit %d); the visible-rows branch did not run",
			txB.SnapshotTime.UnixMicro(), submitA)
	}

	// T_A's commit has returned, so the committed row now carries T_A's
	// transaction ID: a compare-and-save against the pre-T_A ID conflicts.
	_, err = store.CompareAndSave(rb.ctx, &spi.Entity{Meta: seed.Meta, Data: []byte(`{"n":3}`)}, preTxID)
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("CompareAndSave against the pre-commit transaction ID: err = %v, want ErrConflict", err)
	}
}

// A direct (non-transactional) write stamps a submit_time and commits its
// own sqlTx. It holds the commit gate across both, so a Begin that follows cannot
// floor its snapshot at or past that submit time while the row is still
// invisible on readDB.
func TestDirectSave_ThenBegin_SeesRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "direct-then-begin.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()
	m := f.tm
	ctx := attrInternalCtx("tenant-direct", "alice", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-direct", ModelVersion: "1"}

	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-direct", TenantID: "tenant-direct", ModelRef: ref, State: "open"},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("direct Save: %v", err)
	}

	txID, txCtx, err := m.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = m.Rollback(txCtx, txID) }()

	iterable := store
	it, err := iterable.Iterate(txCtx, ref, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	var n int
	for it.Next() {
		if it.Entity().Meta.ID == "e-direct" {
			n++
		}
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Iterate Err: %v", err)
	}
	if n != 1 {
		t.Fatalf("in-tx overlay saw the directly-saved row %d times, want 1", n)
	}
}

// A context already done on entry must lose deterministically, even when the
// gate is free: a bare select between a free gate and a done context picks at
// random, so a caller that has given up would sometimes still be handed a
// transaction it can no longer roll back.
func TestBegin_DoneContextNeverGetsATransaction(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "begin-done-ctx.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()
	m := f.tm

	doneCtx, cancel := context.WithCancel(attrInternalCtx("tenant-done", "alice", spi.PrincipalUser))
	cancel()

	// The gate is free throughout: every attempt below is the coin-flip case.
	for i := range 200 {
		txID, _, err := m.Begin(doneCtx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("attempt %d: Begin with a cancelled context: err = %v, want context.Canceled", i, err)
		}
		if txID != "" {
			t.Fatalf("attempt %d: Begin returned txID %q after failing", i, txID)
		}
	}
	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if len(m.active) != 0 {
			t.Fatalf("failed Begins registered %d transaction(s)", len(m.active))
		}
	}()
}

// Releasing a gate nobody holds is a discipline error. It must fail loudly at
// the call site rather than block there — a silent hang would stall the
// releasing goroutine and every writer queued behind the gate.
func TestReleaseCommitGate_UnheldPanics(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gate-release.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()
	m := f.tm

	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		m.releaseCommitGate()
	}()

	select {
	case r := <-done:
		if r == nil {
			t.Fatal("releasing an unheld commit gate did not panic")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("releasing an unheld commit gate blocked instead of panicking")
	}
}

// Begin queues behind the commit gate, and a commit's flush can take as long
// as the write does. A caller whose context ends while Begin waits must get
// its own context error back rather than waiting indefinitely, and must leave
// no transaction registered behind it.
func TestBegin_ReturnsCallerContextErrorWhileGateHeld(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "begin-ctx.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()
	m := f.tm
	ctx := attrInternalCtx("tenant-ctx", "alice", spi.PrincipalUser)

	type beginResult struct {
		txID string
		err  error
	}

	func() {
		_ = m.acquireCommitGate(context.Background())
		defer m.releaseCommitGate()

		waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()

		done := make(chan beginResult, 1)
		go func() {
			id, _, err := m.Begin(waitCtx)
			done <- beginResult{id, err}
		}()

		select {
		case r := <-done:
			if !errors.Is(r.err, context.DeadlineExceeded) {
				t.Fatalf("Begin with an expired context while the gate is held: err = %v, want context.DeadlineExceeded", r.err)
			}
			if r.txID != "" {
				t.Fatalf("Begin returned txID %q after failing", r.txID)
			}
		case <-time.After(time.Second):
			t.Fatal("Begin ignored its caller's context and kept waiting for the commit gate")
		}

		func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if len(m.active) != 0 {
				t.Fatalf("a failed Begin registered %d transaction(s)", len(m.active))
			}
		}()
	}()

	// Gate released: Begin succeeds again.
	txID, txCtx, err := m.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin after the gate was released: %v", err)
	}
	defer func() { _ = m.Rollback(txCtx, txID) }()
	if spi.GetTransaction(txCtx) == nil {
		t.Fatal("Begin returned a context with no transaction")
	}
}

// A direct (non-transactional) write must stamp its submit_time under the
// same monotonic floor a commit uses. lastSubmitTime can stand ahead of the
// wall clock — several commits inside one microsecond each bump it by one,
// and a test clock can be frozen outright — and Begin floors a new
// transaction's SnapshotTime to it. A direct write that stamped the raw
// clock value would land BELOW a snapshot already open, and that
// transaction's snapshot read would then see a row written after it began.
func TestDirectSave_StampsAboveAnOpenSnapshot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "direct-floor.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()
	m := f.tm
	ctx := attrInternalCtx("tenant-floor", "alice", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-floor", ModelVersion: "1"}

	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	// Stand in for a run of commits that pushed the monotonic submit time
	// past the wall clock.
	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.lastSubmitTime = m.factory.clock.Now().UnixMicro() + 5_000_000 // 5s ahead
	}()

	txID, txCtx, err := m.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = m.Rollback(txCtx, txID) }()
	snapshot := spi.GetTransaction(txCtx).SnapshotTime.UnixMicro()

	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-floor", TenantID: "tenant-floor", ModelRef: ref, State: "open"},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("direct Save: %v", err)
	}

	var stamped int64
	if err := f.db.QueryRow(
		"SELECT submit_time FROM entity_versions WHERE entity_id = ?", "e-floor").Scan(&stamped); err != nil {
		t.Fatalf("read stamped submit_time: %v", err)
	}
	if stamped <= snapshot {
		t.Fatalf("direct write stamped submit_time %d at or below the open snapshot %d", stamped, snapshot)
	}

	iterable := store
	it, err := iterable.Iterate(txCtx, ref, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	var seen int
	for it.Next() {
		if it.Entity().Meta.ID == "e-floor" {
			seen++
		}
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Iterate Err: %v", err)
	}
	if seen != 0 {
		t.Fatalf("the open transaction saw a row written after it began (%d times)", seen)
	}
}
