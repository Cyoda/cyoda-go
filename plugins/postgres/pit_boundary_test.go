package postgres_test

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const (
	pitTenant = "entity-tenant"
	pitBaseTS = "2026-01-01 00:00:00+00"        // millisecond-aligned
	pitNextTS = "2026-01-01 00:00:00.000300+00" // +300µs, same millisecond
)

func pitParseBase(t *testing.T) time.Time {
	t.Helper()
	bt, err := time.Parse(time.RFC3339Nano, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	return bt
}

// pitSetup saves v1 then v2 on one entity, then forces deterministic,
// same-millisecond valid_times (v1 at pitBaseTS, v2 300µs later).
func pitSetup(t *testing.T) (*spi.Entity, spi.EntityStore, func() time.Time) {
	t.Helper()
	factory := setupEntityTest(t)
	ctx := ctxWithTenant(pitTenant)
	store, _ := factory.EntityStore(ctx)
	pool := factory.Pool()

	ent := makeEntity("ent-pit")
	ent.Data = []byte(`{"value":"v1"}`)
	if _, err := store.Save(ctx, ent); err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	ent.Data = []byte(`{"value":"v2"}`)
	if _, err := store.Save(ctx, ent); err != nil {
		t.Fatalf("Save v2: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE entity_versions SET valid_time=$1, transaction_time=$1
		 WHERE tenant_id=$2 AND entity_id=$3 AND version=1`,
		pitBaseTS, pitTenant, "ent-pit"); err != nil {
		t.Fatalf("update v1 valid_time: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE entity_versions SET valid_time=$1, transaction_time=$1
		 WHERE tenant_id=$2 AND entity_id=$3 AND version=2`,
		pitNextTS, pitTenant, "ent-pit"); err != nil {
		t.Fatalf("update v2 valid_time: %v", err)
	}
	return ent, store, func() time.Time { return pitParseBase(t) }
}

func TestPostgresPIT_GetAsAt_InclusiveNoRoundUp(t *testing.T) {
	_, store, base := pitSetup(t)
	ctx := ctxWithTenant(pitTenant)

	got, err := store.GetAsAt(ctx, "ent-pit", base())
	if err != nil {
		t.Fatalf("GetAsAt(base): %v", err)
	}
	if string(got.Data) != `{"value":"v1"}` {
		t.Errorf("GetAsAt(base) = %s, want v1 (round-up over-included same-ms v2)", got.Data)
	}
}

func TestPostgresPIT_GetAllAsAt_InclusiveNoRoundUp(t *testing.T) {
	_, store, base := pitSetup(t)
	ctx := ctxWithTenant(pitTenant)
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	got, err := store.GetAllAsAt(ctx, ref, base())
	if err != nil {
		t.Fatalf("GetAllAsAt(base): %v", err)
	}
	if len(got) != 1 || string(got[0].Data) != `{"value":"v1"}` {
		t.Errorf("GetAllAsAt(base) = %d entities, want 1 with v1", len(got))
	}
}
