package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// TestMain gives this suite the database it is about. Nearly every test here
// exercises real SQL against real PostgreSQL, so a missing database is not a
// reason to run fewer tests — the suite used to skip itself when
// CYODA_TEST_DB_URL was unset, which reported a green run of a few hundred
// skips and hid the plugin's whole surface from `make test-full`.
//
// So: when CYODA_TEST_DB_URL is set — CI, which supplies a service container —
// it is used unchanged. When it is not, this starts a testcontainer and
// publishes its DSN under the same variable, which is the only channel the
// fixtures read. A database that cannot be provisioned fails the run.
//
// The container settings mirror internal/testpg, which the root module's E2E
// and parity fixtures share. This plugin is its own Go module and cannot
// import that internal package, so the few lines are restated here; keep them
// in step.
func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

func runSuite(m *testing.M) int {
	if os.Getenv("CYODA_TEST_DB_URL") != "" {
		return m.Run()
	}

	ctx := context.Background()
	// ShmSize 256MB (Docker default 64MB) plus a smaller shared_buffers and
	// no parallel query: a constrained runner otherwise OOM-kills the backend
	// mid-test, which surfaces as an unexplained wall of connection failures.
	c, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("cyoda_plugin_test"),
		tcpostgres.WithUsername("cyoda"),
		tcpostgres.WithPassword("cyoda"),
		tcpostgres.BasicWaitStrategies(),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.ShmSize = 256 * 1024 * 1024
		}),
		testcontainers.WithCmdArgs(
			"-c", "shared_buffers=32MB",
			"-c", "max_parallel_workers_per_gather=0",
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"plugins/postgres: these tests need PostgreSQL and none could be started: %v\n"+
				"Start Docker, or point CYODA_TEST_DB_URL at a database to use instead.\n", err)
		return 1
	}
	defer func() { _ = c.Terminate(ctx) }()

	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugins/postgres: could not read the container's connection string: %v\n", err)
		return 1
	}
	os.Setenv("CYODA_TEST_DB_URL", dsn)

	return m.Run()
}
