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
