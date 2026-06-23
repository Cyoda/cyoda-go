// Package testpg centralises the PostgreSQL testcontainer configuration shared
// by the E2E and parity test fixtures.
//
// It exists to fix an intermittent CI flake: under memory pressure on the
// constrained CI runner, the postgres backend was OOM-killed mid-test, which
// surfaces to clients as SQLSTATE 57P01 ("terminating connection due to
// unexpected postmaster exit") and cascades into a wall of 500s that fails the
// whole suite. The container previously ran with Docker defaults (64MB
// /dev/shm, 128MB shared_buffers, parallel query enabled) and discarded its
// logs, so a crash left no evidence.
package testpg

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// HardenedOptions returns the testcontainers options every postgres fixture
// should pass to tcpostgres.Run. They tune the container for a constrained CI
// runner so its backend is not an OOM-killer victim:
//
//   - ShmSize 256MB (Docker default is 64MB): headroom for dynamic shared
//     memory so no operation can exhaust /dev/shm.
//   - shared_buffers=32MB (default 128MB): shrinks the resident set so the
//     container has a lower OOM score; test datasets are tiny.
//   - max_parallel_workers_per_gather=0: disables parallel query — the main
//     /dev/shm and extra-process consumer — with no effect on correctness.
//
// (fsync=off is already set by the testcontainers postgres module.)
//
// BasicWaitStrategies is included so callers pass exactly this slice instead of
// remembering to add it separately.
func HardenedOptions() []testcontainers.ContainerCustomizer {
	return []testcontainers.ContainerCustomizer{
		tcpostgres.BasicWaitStrategies(),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.ShmSize = 256 * 1024 * 1024
		}),
		testcontainers.WithCmdArgs(
			"-c", "shared_buffers=32MB",
			"-c", "max_parallel_workers_per_gather=0",
		),
	}
}

// DumpDiagnosticsIfDied inspects the postgres container and, if it exited
// unexpectedly (not running, OOM-killed, or non-zero exit code), writes the
// exit state and full container logs to stderr. This turns the previously
// evidence-free flake into a self-diagnosing one: any recurrence in CI prints
// oomKilled / exitCode plus the postgres server log. Call it during teardown,
// before terminating the container.
func DumpDiagnosticsIfDied(ctx context.Context, c testcontainers.Container) {
	state, err := c.State(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testpg: could not inspect postgres container state: %v\n", err)
		return
	}
	if state.Running && !state.OOMKilled && state.ExitCode == 0 {
		return // healthy — nothing to report
	}
	fmt.Fprintf(os.Stderr,
		"testpg: postgres container died unexpectedly: running=%v oomKilled=%v exitCode=%d error=%q\n",
		state.Running, state.OOMKilled, state.ExitCode, state.Error)
	rc, err := c.Logs(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testpg: could not read postgres container logs: %v\n", err)
		return
	}
	defer func() { _ = rc.Close() }()
	logs, _ := io.ReadAll(rc)
	fmt.Fprintf(os.Stderr, "=== postgres container logs ===\n%s\n=== end postgres container logs ===\n", logs)
}
