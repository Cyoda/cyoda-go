package postgres_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

func TestValidateInChunks_EmptyIDs(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx := ctxWithTenant("t1")
	txID, txCtx := beginGuarded(t, tm, ctx)
	tx, _ := tm.LookupTx(txID)
	current, err := postgres.ValidateInChunksForTest(tm, txCtx, tx, "t1", nil, 100)
	if err != nil {
		t.Fatalf("validateInChunks(nil): %v", err)
	}
	if len(current) != 0 {
		t.Errorf("want empty map, got %v", current)
	}
}

func TestValidateInChunks_SingleChunk(t *testing.T) {
	tm, pool := newTestTxManager(t)
	ctx := ctxWithTenant("t1")
	_, _ = pool.Exec(ctx, `
		INSERT INTO entities (tenant_id, entity_id, model_name, model_version, version, deleted, doc)
		VALUES ('t1', 'e1', 'M', '1', 3, false, '{}'::jsonb),
		       ('t1', 'e2', 'M', '1', 7, false, '{}'::jsonb)
	`)
	txID, txCtx := beginGuarded(t, tm, ctx)
	tx, _ := tm.LookupTx(txID)
	current, err := postgres.ValidateInChunksForTest(tm, txCtx, tx, "t1", []string{"e1", "e2"}, 100)
	if err != nil {
		t.Fatalf("validateInChunks: %v", err)
	}
	if current["e1"] != 3 || current["e2"] != 7 {
		t.Errorf("versions: want {e1:3,e2:7}, got %v", current)
	}
}

func TestValidateInChunks_MultipleChunks(t *testing.T) {
	tm, pool := newTestTxManager(t)
	ctx := ctxWithTenant("t1")
	for i, id := range []string{"a", "b", "c", "d", "e"} {
		_, _ = pool.Exec(ctx, `
			INSERT INTO entities (tenant_id, entity_id, model_name, model_version, version, deleted, doc)
			VALUES ('t1', $1, 'M', '1', $2, false, '{}'::jsonb)
		`, id, int64(i+1))
	}
	txID, txCtx := beginGuarded(t, tm, ctx)
	tx, _ := tm.LookupTx(txID)
	current, err := postgres.ValidateInChunksForTest(tm, txCtx, tx, "t1",
		[]string{"a", "b", "c", "d", "e"}, 2) // chunk size 2 → 3 chunks
	if err != nil {
		t.Fatalf("validateInChunks: %v", err)
	}
	if len(current) != 5 {
		t.Errorf("want 5, got %d: %v", len(current), current)
	}
}

func TestValidateInChunks_TenantScoped(t *testing.T) {
	tm, pool := newTestTxManager(t)
	ctx := ctxWithTenant("t1")
	_, _ = pool.Exec(ctx, `
		INSERT INTO entities (tenant_id, entity_id, model_name, model_version, version, deleted, doc)
		VALUES ('t1', 'e1', 'M', '1', 1, false, '{}'::jsonb),
		       ('t2', 'e1', 'M', '1', 99, false, '{}'::jsonb)
	`)
	txID, txCtx := beginGuarded(t, tm, ctx)
	tx, _ := tm.LookupTx(txID)
	current, err := postgres.ValidateInChunksForTest(tm, txCtx, tx, "t1", []string{"e1"}, 100)
	if err != nil {
		t.Fatalf("validateInChunks: %v", err)
	}
	if current["e1"] != 1 {
		t.Errorf("tenant scoping: want t1's e1=1, got %d", current["e1"])
	}
}
