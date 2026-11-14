package testpg_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/cyoda-platform/cyoda-go/internal/testpg"
)

// TestHardenedOptions_Applied proves the CI-hardening knobs actually reach the
// running postgres server — the whole point of the helper. If these regress,
// the OOM-flake mitigation is silently gone.
func TestHardenedOptions_Applied(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker")
	}
	ctx := context.Background()

	opts := append([]testcontainers.ContainerCustomizer{
		tcpostgres.WithDatabase("hardening_test"),
		tcpostgres.WithUsername("u"),
		tcpostgres.WithPassword("p"),
	}, testpg.HardenedOptions()...)
	c, err := tcpostgres.Run(ctx, "postgres:17-alpine", opts...)
	if err != nil {
		t.Fatalf("run postgres: %v", err)
	}
	defer func() {
		testpg.DumpDiagnosticsIfDied(ctx, c)
		_ = c.Terminate(ctx)
	}()

	connStr, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	for _, tc := range []struct{ setting, want string }{
		{"shared_buffers", "32MB"},
		{"max_parallel_workers_per_gather", "0"},
	} {
		var got string
		if err := conn.QueryRow(ctx, "SHOW "+tc.setting).Scan(&got); err != nil {
			t.Fatalf("SHOW %s: %v", tc.setting, err)
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q — hardening not applied", tc.setting, got, tc.want)
		}
	}
}

// TestHardenedOptions_OwnPassword proves that a fixture's database refuses a
// client holding another fixture's credentials. A container's host port is
// taken from the host's ephemeral range and reused by later containers, and a
// cyoda-go node left running by a test binary that died (it is in a process
// group of its own, so nothing kills it) keeps dialling its old database's
// address. With one password for every fixture, such a node logs in to
// whichever fixture's database now holds that port and claims its scheduled
// tasks under an owner that fixture never started.
func TestHardenedOptions_OwnPassword(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker")
	}
	ctx := context.Background()
	run := func() string {
		t.Helper()
		opts := append([]testcontainers.ContainerCustomizer{
			tcpostgres.WithDatabase("isolation_test"),
			tcpostgres.WithUsername("u"),
		}, testpg.HardenedOptions()...)
		c, err := tcpostgres.Run(ctx, "postgres:17-alpine", opts...)
		if err != nil {
			t.Fatalf("run postgres: %v", err)
		}
		t.Cleanup(func() { _ = c.Terminate(ctx) })
		connStr, err := c.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			t.Fatalf("connection string: %v", err)
		}
		return connStr
	}
	stale, own := run(), run()

	staleCfg, err := pgx.ParseConfig(stale)
	if err != nil {
		t.Fatalf("parse the first fixture's connection string: %v", err)
	}
	ownCfg, err := pgx.ParseConfig(own)
	if err != nil {
		t.Fatalf("parse the second fixture's connection string: %v", err)
	}
	conn, err := pgx.Connect(ctx, own)
	if err != nil {
		t.Fatalf("connect with the fixture's own credentials: %v", err)
	}
	_ = conn.Close(ctx)

	// The first fixture's credentials at the second fixture's address: what a
	// node of the first holds once the second takes over its port.
	staleCfg.Host, staleCfg.Port = ownCfg.Host, ownCfg.Port
	if conn, err := pgx.ConnectConfig(ctx, staleCfg); err == nil {
		_ = conn.Close(ctx)
		t.Fatal("a client with another fixture's credentials logged in to this fixture's database")
	}
}
