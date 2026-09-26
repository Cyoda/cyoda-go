package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestPGSearchStore_ClearResults_FenceAndDeleteAreAtomic: a clear from epoch 1
// races a claim that moves the job to epoch 2 and saves the new owner's rows
// in the same transaction. The claim holds the job row first, so the clear
// must wait for it and then refuse: it deletes nothing — neither the new
// owner's rows nor the rows it would have cleared had it read the fence
// before the claim committed.
func TestPGSearchStore_ClearResults_FenceAndDeleteAreAtomic(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "clear-tenant")
	ctx := ctxWithTenant("clear-tenant")

	job := &spi.SearchJob{
		ID:         "job-clear",
		TenantID:   "clear-tenant",
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "X", ModelVersion: "1"},
		Condition:  json.RawMessage(`{}`),
		CreateTime: time.Now().UTC().Truncate(time.Microsecond),
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := store.SaveResults(ctx, "job-clear", 1, slices.Values([]string{"old-1", "old-2"})); err != nil {
		t.Fatalf("SaveResults at epoch 1: %v", err)
	}

	actorCtx := context.Background()
	actorTx, err := factory.Pool().Begin(actorCtx)
	if err != nil {
		t.Fatalf("actor Begin: %v", err)
	}
	defer func() { _ = actorTx.Rollback(actorCtx) }()
	if _, err := actorTx.Exec(actorCtx,
		`SELECT 1 FROM search_jobs WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, "job-clear", "clear-tenant"); err != nil {
		t.Fatalf("actor lock: %v", err)
	}

	clearErr := make(chan error, 1)
	go func() { clearErr <- store.ClearResults(ctx, "job-clear", 1) }()

	// Room for the clear to reach the database and meet the lock; a clear
	// that read its fence without the lock would delete within this window.
	time.Sleep(150 * time.Millisecond)

	if _, err := actorTx.Exec(actorCtx,
		`UPDATE search_jobs SET epoch = 2 WHERE id = $1 AND tenant_id = $2`, "job-clear", "clear-tenant"); err != nil {
		t.Fatalf("actor claim: %v", err)
	}
	if _, err := actorTx.Exec(actorCtx,
		`INSERT INTO search_job_results (job_id, tenant_id, seq, entity_id) VALUES ($1, $2, 100, 'new-1')`,
		"job-clear", "clear-tenant"); err != nil {
		t.Fatalf("actor save: %v", err)
	}
	if err := actorTx.Commit(actorCtx); err != nil {
		t.Fatalf("actor commit: %v", err)
	}

	select {
	case err := <-clearErr:
		if !errors.Is(err, spi.ErrStaleClaim) {
			t.Fatalf("ClearResults(epoch 1) = %v, want ErrStaleClaim", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ClearResults did not return within 10s")
	}

	_, total, err := store.GetResultIDs(ctx, "job-clear", 0, 100)
	if err != nil {
		t.Fatalf("GetResultIDs: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3: a clear refused at a superseded epoch must delete nothing", total)
	}
}
