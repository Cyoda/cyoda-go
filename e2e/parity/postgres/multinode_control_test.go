package postgres

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// pingDB opens a fresh connection and runs one statement within budget.
func pingDB(connStr string, budget time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var one int
	return conn.QueryRow(ctx, "SELECT 1").Scan(&one)
}

// TestMultiNodeFixture_ControlSurface pins what the scheduler scenarios do to
// a cluster: per-node env (node 1 runs without a scheduler), the incarnation
// each scheduler announced, room for every pnode's pools, a database pause,
// and a graceful stop of one node.
func TestMultiNodeFixture_ControlSurface(t *testing.T) {
	t.Parallel()
	fix, cleanup := MustSetupMultiNodeWithOpts(t, 2, nil, fixtureutil.LaunchOpts{
		NodeEnv: func(i int) []string {
			if i == 1 {
				return []string{"CYODA_SCHEDULER_ENABLED=false"}
			}
			return nil
		},
	})
	defer cleanup()
	f := fix.(*pgMultiNode)

	if id := f.Incarnation(t, 0); id == uuid.Nil {
		t.Error("node 0 announced a nil incarnation")
	}
	if _, err := fixtureutil.IncarnationFromLog(f.NodeLogs(1)); err == nil {
		t.Error("node 1 announced a scheduler; its NodeEnv turned it off")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, f.ConnString())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var maxConns string
	err = conn.QueryRow(ctx, "SHOW max_connections").Scan(&maxConns)
	conn.Close(context.Background())
	if err != nil {
		t.Fatalf("SHOW max_connections: %v", err)
	}
	if maxConns != "400" {
		t.Errorf("max_connections = %s; want 400", maxConns)
	}

	f.PauseDatabase(t)
	paused := pingDB(f.ConnString(), 2*time.Second)
	f.UnpauseDatabase(t)
	if paused == nil {
		t.Error("a statement completed while the database was paused")
	}
	if err := pingDB(f.ConnString(), 10*time.Second); err != nil {
		t.Errorf("the database does not answer after unpause: %v", err)
	}

	if err := f.SignalNode(1, syscall.SIGTERM); err != nil {
		t.Fatalf("SignalNode: %v", err)
	}
	if err := f.AwaitNodeExit(1, 60*time.Second); err != nil {
		t.Fatalf("node 1 did not exit on SIGTERM: %v", err)
	}
	if err := f.AwaitNodeExit(0, 0); err == nil {
		t.Error("node 0 reported exited; only node 1 was signalled")
	}
}
