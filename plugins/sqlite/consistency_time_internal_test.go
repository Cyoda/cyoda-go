package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ConsistencyTime waits for a commit that holds the gate (stamped, rows not
// yet visible) and returns only after it.
func TestConsistencyTime_WaitsForInFlightCommit(t *testing.T) {
	f, err := NewStoreFactoryForTest(context.Background(), filepath.Join(t.TempDir(), "ct.db"))
	require.NoError(t, err)
	defer f.Close()
	m := f.tm
	ctx := attrInternalCtx("tenant-A", "alice", "USER")

	require.NoError(t, m.acquireCommitGate(context.Background()))
	stamp := time.Now().Add(5 * time.Second).UnixMicro()
	m.mu.Lock()
	m.lastSubmitTime = stamp // the in-flight stamp
	m.mu.Unlock()

	type res struct {
		c   time.Time
		err error
	}
	done := make(chan res, 1)
	go func() {
		c, err := m.ConsistencyTime(ctx)
		done <- res{c, err}
	}()
	select {
	case <-done:
		t.Fatal("ConsistencyTime returned while a commit held the gate")
	case <-time.After(150 * time.Millisecond):
	}
	m.releaseCommitGate()
	r := <-done
	require.NoError(t, r.err)
	require.GreaterOrEqual(t, r.c.UnixMicro(), stamp)
}

func TestConsistencyTime_HonoursCallerContext(t *testing.T) {
	f, err := NewStoreFactoryForTest(context.Background(), filepath.Join(t.TempDir(), "ct.db"))
	require.NoError(t, err)
	defer f.Close()
	ctx := attrInternalCtx("tenant-A", "alice", "USER")
	require.NoError(t, f.tm.acquireCommitGate(context.Background()))
	defer f.tm.releaseCommitGate()
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = f.tm.ConsistencyTime(cctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// Monotonic across a restart whose wall clock stepped back: a C handed out
// before the restart stays at or below every later C and stamp.
func TestConsistencyTime_MonotonicAcrossRestartWithClockBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ct.db")
	future := time.Now().Add(time.Hour)
	f1, err := NewStoreFactoryForTest(context.Background(), path, WithClock(NewTestClockAt(future)))
	require.NoError(t, err)
	ctx := attrInternalCtx("tenant-A", "alice", "USER")
	c1, err := f1.tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	require.NoError(t, f1.Close())

	f2, err := NewStoreFactoryForTest(context.Background(), path, WithClock(NewTestClockAt(time.Now())))
	require.NoError(t, err)
	defer f2.Close()
	c2, err := f2.tm.ConsistencyTime(ctx)
	require.NoError(t, err)
	require.False(t, c2.Before(c1), "C after restart %v below C before %v", c2, c1)
}

func TestConsistencyTime_RequiresTenant(t *testing.T) {
	f, err := NewStoreFactoryForTest(context.Background(), filepath.Join(t.TempDir(), "ct.db"))
	require.NoError(t, err)
	defer f.Close()
	_, err = f.tm.ConsistencyTime(context.Background())
	require.Error(t, err)
}

func TestSeed_FloorsAtSubmitTimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ct.db")
	f1, err := NewStoreFactoryForTest(context.Background(), path)
	require.NoError(t, err)
	require.NoError(t, f1.Close())

	db, err := sql.Open("sqlite3", "file:"+path)
	require.NoError(t, err)
	stampAt := time.Now().Add(2 * time.Hour).UnixMicro()
	_, err = db.Exec(`INSERT INTO submit_times (tx_id, tenant_id, submit_time) VALUES ('tx-seed', 'tenant-A', ?)`, stampAt)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	f2, err := NewStoreFactoryForTest(context.Background(), path, WithClock(NewTestClockAt(time.Now())))
	require.NoError(t, err)
	defer f2.Close()
	c, err := f2.tm.ConsistencyTime(attrInternalCtx("tenant-A", "alice", "USER"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, c.UnixMicro(), stampAt)
}

// A search job's point_in_time is not a stamp: before consistency time it
// held the caller's pointInTime as sent, with no check against the future, so
// flooring at it would let one old async submit dated far ahead push every
// tenant's stamps there for good. The seed ignores it; every C handed out is
// already covered by consistency_floor.
func TestSeed_IgnoresSearchJobInstants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ct.db")
	f1, err := NewStoreFactoryForTest(context.Background(), path)
	require.NoError(t, err)
	require.NoError(t, f1.Close())

	db, err := sql.Open("sqlite3", "file:"+path)
	require.NoError(t, err)
	jobAt := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro()
	_, err = db.Exec(`INSERT INTO search_jobs (tenant_id, job_id, model_name, model_version, point_in_time, create_time)
		VALUES ('tenant-A', 'job-seed', 'm', '1', ?, 0)`, jobAt)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	now := time.Now()
	f2, err := NewStoreFactoryForTest(context.Background(), path, WithClock(NewTestClockAt(now)))
	require.NoError(t, err)
	defer f2.Close()
	c, err := f2.tm.ConsistencyTime(attrInternalCtx("tenant-A", "alice", "USER"))
	require.NoError(t, err)
	require.Less(t, c.UnixMicro(), now.Add(time.Hour).UnixMicro(),
		"C %v was raised by a search job's instant, which is not a stamp", c)
}

func TestSeed_QueryErrorFailsConstruction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ct.db")
	f1, err := NewStoreFactoryForTest(context.Background(), path)
	require.NoError(t, err)
	require.NoError(t, f1.Close())

	db, err := sql.Open("sqlite3", "file:"+path)
	require.NoError(t, err)
	_, err = db.Exec(`DROP TABLE submit_times`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = NewStoreFactoryForTest(context.Background(), path)
	require.Error(t, err)
}
