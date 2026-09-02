package postgres_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// testDBURL returns the database every test in this package runs against.
// TestMain guarantees it — from the environment, or from a container it
// started — so an empty value is a broken fixture, not a reason to skip.
func testDBURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("CYODA_TEST_DB_URL")
	if url == "" {
		t.Fatal("CYODA_TEST_DB_URL is empty — TestMain must provision a database")
	}
	return url
}

func TestNewPool_Connects(t *testing.T) {
	dbURL := testDBURL(t)

	cfg := postgres.DBConfig{
		URL:             dbURL,
		MaxConns:        5,
		MinConns:        0, // 0 avoids backgroundHealthCheck goroutine interference
		MaxConnIdleTime: "1m",
	}

	pool, err := postgres.NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var result int
	err = pool.QueryRow(ctx, "SELECT 1").Scan(&result)
	if err != nil {
		t.Fatalf("ping failed: %v", err)
	}
	if result != 1 {
		t.Fatalf("expected 1, got %d", result)
	}
}

func TestNewPool_InvalidURL(t *testing.T) {
	cfg := postgres.DBConfig{
		URL:             "postgres://invalid:invalid@localhost:59999/nonexistent",
		MaxConns:        1,
		MinConns:        0,
		MaxConnIdleTime: "1m",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := postgres.NewPool(ctx, cfg)
	if err == nil {
		t.Fatal("expected error for invalid connection, got nil")
	}
}
