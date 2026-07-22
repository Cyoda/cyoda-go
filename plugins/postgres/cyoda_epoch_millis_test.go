package postgres_test

import (
	"context"
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// TestCyodaEpochMillis covers the offset-mandatory RFC3339->epoch-ms rule
// (spec §8.1): a valid offset-bearing instant (Z or ±hh:mm, with or without
// fractional seconds) converts to floored epoch-milliseconds; anything else
// (offset-less, garbage) returns NULL rather than raising — the function must
// be total over text input so it can be used directly in a WHERE clause
// without per-row validation.
func TestCyodaEpochMillis(t *testing.T) {
	pool := newTestPool(t)

	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() {
		_ = postgres.DropSchemaForTest(pool)
	})

	cases := []struct {
		name     string
		in       string
		wantNull bool
		wantVal  int64
	}{
		{"z-no-fraction", "2021-01-01T00:00:00Z", false, 1609459200000},
		{"z-with-millis", "2021-01-01T00:00:00.000Z", false, 1609459200000},
		{"positive-offset", "2021-06-01T14:00:00+02:00", false, 1622548800000},
		{"offset-less", "2021-01-01T00:00:00", true, 0},
		{"garbage", "not-a-date", true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got *int64
			err := pool.QueryRow(context.Background(),
				"SELECT cyoda_epoch_millis($1)", tc.in).Scan(&got)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if tc.wantNull {
				if got != nil {
					t.Fatalf("got %v, want NULL", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got NULL, want %v", tc.wantVal)
			}
			if *got != tc.wantVal {
				t.Fatalf("got %v, want %v", *got, tc.wantVal)
			}
		})
	}
}
