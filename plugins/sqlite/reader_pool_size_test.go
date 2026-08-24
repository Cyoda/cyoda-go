package sqlite

// reader_pool_size_test.go — CYODA_SQLITE_READER_POOL_SIZE.
//
// PRAGMA cache_size is per-connection, so the reader pool multiplies the page
// cache ceiling by however many connections it holds: with the default 64000
// KiB that is ~125 MiB for writer + one reader, but ~562 MiB for writer + 8.
// The pool size was derived from GOMAXPROCS and hardcoded, and GOMAXPROCS
// follows the CPU quota — it cannot see the memory cgroup. An 8-CPU container
// on a 512 MiB limit therefore had no way to bound the ceiling: the only
// existing lever, CYODA_SQLITE_CACHE_SIZE, also shrinks the writer's cache.

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestReaderPoolSize_ConfiguredFromEnv(t *testing.T) {
	cases := []struct {
		name string
		set  string
		want int
	}{
		{"unset falls back to the GOMAXPROCS-derived default", "", defaultReaderPoolSize()},
		{"explicit smaller pool for a memory-capped container", "2", 2},
		{"explicit larger pool", "16", 16},
		{"single reader is a legal, if serialising, choice", "1", 1},
		{"zero is not a usable pool — default applies", "0", defaultReaderPoolSize()},
		{"negative is not a usable pool — default applies", "-4", defaultReaderPoolSize()},
		{"unparseable — default applies", "many", defaultReaderPoolSize()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseConfig(func(key string) string {
				if key == "CYODA_SQLITE_READER_POOL_SIZE" {
					return tc.set
				}
				return ""
			})
			if err != nil {
				t.Fatalf("parseConfig: %v", err)
			}
			if cfg.ReaderPoolSize != tc.want {
				t.Errorf("CYODA_SQLITE_READER_POOL_SIZE=%q → ReaderPoolSize %d, want %d", tc.set, cfg.ReaderPoolSize, tc.want)
			}
		})
	}
}

// TestReadDB_HonoursConfiguredReaderPoolSize is the half that matters: the
// parsed value has to reach the reader pool, since capping the pool is the
// only way to cap the page-cache ceiling without also shrinking the writer's
// cache.
func TestReadDB_HonoursConfiguredReaderPoolSize(t *testing.T) {
	cfg := config{
		Path:                   filepath.Join(t.TempDir(), "reader_pool_size.db"),
		AutoMigrate:            true,
		BusyTimeout:            5 * time.Second,
		CacheSizeKiB:           64000,
		SearchScanLimit:        100_000,
		SchemaExtendMaxRetries: 8,
		ReaderPoolSize:         2,
	}
	f, err := newStoreFactory(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newStoreFactory: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if got := f.readDB.Stats().MaxOpenConnections; got != 2 {
		t.Errorf("readDB MaxOpenConnections = %d, want the configured 2", got)
	}
	// The writer keeps its own single connection and its own full cache — the
	// point of a separate knob is that bounding readers does not touch it.
	if got := f.db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("writer MaxOpenConnections = %d, want 1", got)
	}
}

// TestReadDB_ZeroConfiguredPoolIsNotUnlimited: database/sql reads
// SetMaxOpenConns(0) as UNLIMITED, so a config struct built without
// ReaderPoolSize (a test helper, an internal caller) would silently uncap the
// pool — and with it the page-cache ceiling. The factory normalises instead.
func TestReadDB_ZeroConfiguredPoolIsNotUnlimited(t *testing.T) {
	cfg := config{
		Path:                   filepath.Join(t.TempDir(), "reader_pool_zero.db"),
		AutoMigrate:            true,
		BusyTimeout:            5 * time.Second,
		CacheSizeKiB:           64000,
		SearchScanLimit:        100_000,
		SchemaExtendMaxRetries: 8,
		// ReaderPoolSize deliberately left at its zero value.
	}
	f, err := newStoreFactory(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newStoreFactory: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if got := f.readDB.Stats().MaxOpenConnections; got != defaultReaderPoolSize() {
		t.Errorf("readDB MaxOpenConnections = %d, want the default %d (0 means unlimited to database/sql)",
			got, defaultReaderPoolSize())
	}
}
