package postgres_test

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// drainAll reads every entity of modelRef through Iterate with a zero-value
// filter — the streamed replacement for the whole-model reads this package no
// longer has. asAt nil reads the live view (in-transaction: the merged
// view); asAt non-nil reads the committed-only snapshot at that instant.
func drainAll(t *testing.T, ctx context.Context, store spi.EntityStore, modelRef spi.ModelRef, asAt *time.Time) []*spi.Entity {
	t.Helper()
	it, err := store.Iterate(ctx, modelRef, spi.Filter{}, spi.IterateOptions{PointInTime: asAt})
	if err != nil {
		t.Fatalf("Iterate(%s): %v", modelRef.EntityName, err)
	}
	var out []*spi.Entity
	for it.Next() {
		out = append(out, it.Entity())
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Iterate(%s) Err: %v", modelRef.EntityName, err)
	}
	if out == nil {
		out = []*spi.Entity{}
	}
	return out
}
