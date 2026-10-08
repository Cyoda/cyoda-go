package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The high-water write is an upsert: a missing singleton row cannot make it a
// silent no-op that a restart would then seed past.
func TestConsistencyTime_PersistsHighWaterWhenRowMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ct.db")
	f, err := NewStoreFactoryForTest(context.Background(), path, WithClock(NewTestClockAt(time.Now().Add(time.Hour))))
	require.NoError(t, err)
	defer f.Close()
	_, err = f.db.Exec(`DELETE FROM consistency_floor`)
	require.NoError(t, err)
	c, err := f.tm.ConsistencyTime(attrInternalCtx("tenant-A", "alice", "USER"))
	require.NoError(t, err)
	var micros int64
	require.NoError(t, f.db.QueryRow(`SELECT micros FROM consistency_floor WHERE id = 1`).Scan(&micros))
	require.GreaterOrEqual(t, micros, c.UnixMicro())
}
