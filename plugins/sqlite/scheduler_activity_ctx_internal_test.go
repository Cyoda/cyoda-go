package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// SchedulerActivity is a pure read: a caller that gives up while the commit
// gate is held gets its context error back instead of waiting for the gate.
func TestSchedulerActivity_HonoursCallerContext(t *testing.T) {
	f, err := NewStoreFactoryForTest(context.Background(), filepath.Join(t.TempDir(), "sa.db"))
	require.NoError(t, err)
	defer f.Close()
	sts, err := f.ScheduledTaskStore(context.Background())
	require.NoError(t, err)
	require.NoError(t, f.tm.acquireCommitGate(context.Background()))
	defer f.tm.releaseCommitGate()
	cctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := sts.SchedulerActivity(cctx)
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(2 * time.Second):
		t.Fatal("SchedulerActivity waited on the held commit gate past its context")
	}
}
