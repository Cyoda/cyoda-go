package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

func setupSearchTest(t *testing.T) *postgres.StoreFactory {
	t.Helper()
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	return postgres.NewStoreFactory(pool)
}

func getSearchStore(t *testing.T, factory *postgres.StoreFactory, tid spi.TenantID) spi.AsyncSearchStore {
	t.Helper()
	ctx := ctxWithTenant(tid)
	store, err := factory.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	return store
}

func TestPGSearchStore_CreateAndGetJob(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "search-tenant")
	ctx := ctxWithTenant("search-tenant")

	now := time.Now().UTC().Truncate(time.Microsecond)
	job := &spi.SearchJob{
		ID:          "job-001",
		TenantID:    "search-tenant",
		Status:      "RUNNING",
		ModelRef:    spi.ModelRef{EntityName: "Person", ModelVersion: "1"},
		Condition:   json.RawMessage(`{"field":"name","op":"eq","value":"Alice"}`),
		PointInTime: now,
		SearchOpts:  json.RawMessage(`{"sort":"asc"}`),
		ResultCount: 0,
		CreateTime:  now,
	}

	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob() error: %v", err)
	}

	got, err := store.GetJob(ctx, "job-001")
	if err != nil {
		t.Fatalf("GetJob() error: %v", err)
	}

	if got.ID != "job-001" {
		t.Errorf("ID = %q, want %q", got.ID, "job-001")
	}
	if got.TenantID != "search-tenant" {
		t.Errorf("TenantID = %q, want %q", got.TenantID, "search-tenant")
	}
	if got.Status != "RUNNING" {
		t.Errorf("Status = %q, want %q", got.Status, "RUNNING")
	}
	if got.ModelRef.EntityName != "Person" || got.ModelRef.ModelVersion != "1" {
		t.Errorf("ModelRef = %v, want Person.1", got.ModelRef)
	}
	// Compare JSON semantically (not as strings) — Postgres normalizes key order.
	if !jsonEqual(got.Condition, json.RawMessage(`{"field":"name","op":"eq","value":"Alice"}`)) {
		t.Errorf("Condition = %s", got.Condition)
	}
	if !jsonEqual(got.SearchOpts, json.RawMessage(`{"sort":"asc"}`)) {
		t.Errorf("SearchOpts = %s", got.SearchOpts)
	}
	if !got.PointInTime.Equal(now) {
		t.Errorf("PointInTime = %v, want %v", got.PointInTime, now)
	}
	if !got.CreateTime.Equal(now) {
		t.Errorf("CreateTime = %v, want %v", got.CreateTime, now)
	}
	if got.FinishTime != nil {
		t.Errorf("FinishTime = %v, want nil", got.FinishTime)
	}
}

func TestPGSearchStore_TenantIsolation(t *testing.T) {
	factory := setupSearchTest(t)

	storeA := getSearchStore(t, factory, "tenant-a")
	storeB := getSearchStore(t, factory, "tenant-b")

	ctxA := ctxWithTenant("tenant-a")
	ctxB := ctxWithTenant("tenant-b")

	now := time.Now().UTC().Truncate(time.Microsecond)
	job := &spi.SearchJob{
		ID:          "job-iso",
		TenantID:    "tenant-a",
		Status:      "RUNNING",
		ModelRef:    spi.ModelRef{EntityName: "X", ModelVersion: "1"},
		Condition:   json.RawMessage(`{}`),
		PointInTime: now,
		CreateTime:  now,
	}
	if err := storeA.CreateJob(ctxA, job); err != nil {
		t.Fatalf("CreateJob(A) error: %v", err)
	}

	// Tenant A can see it.
	got, err := storeA.GetJob(ctxA, "job-iso")
	if err != nil {
		t.Fatalf("GetJob(A) error: %v", err)
	}
	if got.ID != "job-iso" {
		t.Errorf("tenant A should see job-iso")
	}

	// Tenant B cannot see it.
	_, err = storeB.GetJob(ctxB, "job-iso")
	if err == nil {
		t.Fatal("tenant B should NOT see tenant A's job")
	}
}

func TestPGSearchStore_UpdateJobStatus(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "search-tenant")
	ctx := ctxWithTenant("search-tenant")

	now := time.Now().UTC().Truncate(time.Microsecond)
	job := &spi.SearchJob{
		ID:          "job-upd",
		TenantID:    "search-tenant",
		Status:      "RUNNING",
		ModelRef:    spi.ModelRef{EntityName: "Y", ModelVersion: "1"},
		Condition:   json.RawMessage(`{}`),
		PointInTime: now,
		CreateTime:  now,
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob() error: %v", err)
	}

	finishTime := now.Add(5 * time.Second).Truncate(time.Microsecond)
	err := store.UpdateJobStatus(ctx, "job-upd", "SUCCESSFUL", 42, "", finishTime, 1234)
	if err != nil {
		t.Fatalf("UpdateJobStatus() error: %v", err)
	}

	got, err := store.GetJob(ctx, "job-upd")
	if err != nil {
		t.Fatalf("GetJob() error: %v", err)
	}
	if got.Status != "SUCCESSFUL" {
		t.Errorf("Status = %q, want SUCCESSFUL", got.Status)
	}
	if got.ResultCount != 42 {
		t.Errorf("ResultCount = %d, want 42", got.ResultCount)
	}
	if got.CalcTimeMs != 1234 {
		t.Errorf("CalcTimeMs = %d, want 1234", got.CalcTimeMs)
	}
	if got.FinishTime == nil || !got.FinishTime.Truncate(time.Microsecond).Equal(finishTime) {
		t.Errorf("FinishTime = %v, want %v", got.FinishTime, finishTime)
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want empty", got.Error)
	}

	// Update with error message.
	err = store.UpdateJobStatus(ctx, "job-upd", "FAILED", 0, "something broke", finishTime, 999)
	if err != nil {
		t.Fatalf("UpdateJobStatus(FAILED) error: %v", err)
	}
	got, _ = store.GetJob(ctx, "job-upd")
	if got.Status != "FAILED" {
		t.Errorf("Status = %q, want FAILED", got.Status)
	}
	if got.Error != "something broke" {
		t.Errorf("Error = %q, want 'something broke'", got.Error)
	}
}

func TestPGSearchStore_SaveAndGetResults(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "search-tenant")
	ctx := ctxWithTenant("search-tenant")

	now := time.Now().UTC().Truncate(time.Microsecond)
	job := &spi.SearchJob{
		ID:          "job-res",
		TenantID:    "search-tenant",
		Status:      "RUNNING",
		ModelRef:    spi.ModelRef{EntityName: "Z", ModelVersion: "1"},
		Condition:   json.RawMessage(`{}`),
		PointInTime: now,
		CreateTime:  now,
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob() error: %v", err)
	}

	ids := []string{"e1", "e2", "e3", "e4", "e5"}
	if err := store.SaveResults(ctx, "job-res", ids); err != nil {
		t.Fatalf("SaveResults() error: %v", err)
	}

	// Get all results.
	got, total, err := store.GetResultIDs(ctx, "job-res", 0, 100)
	if err != nil {
		t.Fatalf("GetResultIDs() error: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(got) != 5 {
		t.Errorf("len(got) = %d, want 5", len(got))
	}

	// Paginated: offset=1, limit=2.
	got, total, err = store.GetResultIDs(ctx, "job-res", 1, 2)
	if err != nil {
		t.Fatalf("GetResultIDs(1,2) error: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(got) != 2 {
		t.Errorf("len(got) = %d, want 2", len(got))
	}
	if got[0] != "e2" || got[1] != "e3" {
		t.Errorf("got = %v, want [e2 e3]", got)
	}

	// Offset beyond end.
	got, total, err = store.GetResultIDs(ctx, "job-res", 10, 5)
	if err != nil {
		t.Fatalf("GetResultIDs(10,5) error: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

func TestPGSearchStore_DeleteJob(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "search-tenant")
	ctx := ctxWithTenant("search-tenant")

	now := time.Now().UTC().Truncate(time.Microsecond)
	job := &spi.SearchJob{
		ID:          "job-del",
		TenantID:    "search-tenant",
		Status:      "SUCCESSFUL",
		ModelRef:    spi.ModelRef{EntityName: "W", ModelVersion: "1"},
		Condition:   json.RawMessage(`{}`),
		PointInTime: now,
		CreateTime:  now,
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob() error: %v", err)
	}
	if err := store.SaveResults(ctx, "job-del", []string{"e1", "e2"}); err != nil {
		t.Fatalf("SaveResults() error: %v", err)
	}

	if err := store.DeleteJob(ctx, "job-del"); err != nil {
		t.Fatalf("DeleteJob() error: %v", err)
	}

	// Job should be gone.
	_, err := store.GetJob(ctx, "job-del")
	if err == nil {
		t.Fatal("GetJob() expected error after delete")
	}

	// Results should be gone too (CASCADE).
	var count int
	pool := factory.Pool()
	err = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM search_job_results WHERE job_id = $1`, "job-del").Scan(&count)
	if err != nil {
		t.Fatalf("count query error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 result rows after cascade delete, got %d", count)
	}
}

func TestPGSearchStore_ReapExpired(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "search-tenant")
	ctx := ctxWithTenant("search-tenant")

	now := time.Now().UTC().Truncate(time.Microsecond)
	oldTime := now.Add(-2 * time.Hour)

	// Old completed job.
	oldFinish := oldTime.Add(time.Second)
	oldJob := &spi.SearchJob{
		ID:          "job-old",
		TenantID:    "search-tenant",
		Status:      "SUCCESSFUL",
		ModelRef:    spi.ModelRef{EntityName: "A", ModelVersion: "1"},
		Condition:   json.RawMessage(`{}`),
		PointInTime: oldTime,
		CreateTime:  oldTime,
		FinishTime:  &oldFinish,
	}
	if err := store.CreateJob(ctx, oldJob); err != nil {
		t.Fatalf("CreateJob(old) error: %v", err)
	}

	// New completed job.
	newFinish := now
	newJob := &spi.SearchJob{
		ID:          "job-new",
		TenantID:    "search-tenant",
		Status:      "SUCCESSFUL",
		ModelRef:    spi.ModelRef{EntityName: "B", ModelVersion: "1"},
		Condition:   json.RawMessage(`{}`),
		PointInTime: now,
		CreateTime:  now,
		FinishTime:  &newFinish,
	}
	if err := store.CreateJob(ctx, newJob); err != nil {
		t.Fatalf("CreateJob(new) error: %v", err)
	}

	// Reap with 1h TTL: old job should be reaped, new should survive.
	reaped, err := store.ReapExpired(ctx, 1*time.Hour)
	if err != nil {
		t.Fatalf("ReapExpired() error: %v", err)
	}
	if reaped != 1 {
		t.Errorf("reaped = %d, want 1", reaped)
	}

	// Old job gone.
	_, err = store.GetJob(ctx, "job-old")
	if err == nil {
		t.Error("old job should have been reaped")
	}

	// New job still there.
	_, err = store.GetJob(ctx, "job-new")
	if err != nil {
		t.Errorf("new job should still exist: %v", err)
	}
}

func TestPGSearchStore_SaveResults_TenantVerification(t *testing.T) {
	factory := setupSearchTest(t)
	storeA := getSearchStore(t, factory, "tenant-a")
	storeB := getSearchStore(t, factory, "tenant-b")

	ctxA := ctxWithTenant("tenant-a")
	ctxB := ctxWithTenant("tenant-b")

	now := time.Now().UTC().Truncate(time.Microsecond)
	job := &spi.SearchJob{
		ID:          "job-sv-tenant",
		TenantID:    "tenant-a",
		Status:      "RUNNING",
		ModelRef:    spi.ModelRef{EntityName: "X", ModelVersion: "1"},
		Condition:   json.RawMessage(`{}`),
		PointInTime: now,
		CreateTime:  now,
	}
	if err := storeA.CreateJob(ctxA, job); err != nil {
		t.Fatalf("CreateJob(A) error: %v", err)
	}

	// Tenant B tries to save results on tenant A's job → must fail.
	err := storeB.SaveResults(ctxB, "job-sv-tenant", []string{"e1", "e2"})
	if err == nil {
		t.Fatal("tenant B should NOT be able to SaveResults on tenant A's job")
	}

	// Tenant A saves results → must succeed.
	if err := storeA.SaveResults(ctxA, "job-sv-tenant", []string{"e1", "e2", "e3"}); err != nil {
		t.Fatalf("tenant A SaveResults() error: %v", err)
	}

	// Verify tenant A's results are there.
	ids, total, err := storeA.GetResultIDs(ctxA, "job-sv-tenant", 0, 100)
	if err != nil {
		t.Fatalf("GetResultIDs(A) error: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(ids) != 3 {
		t.Errorf("len(ids) = %d, want 3", len(ids))
	}
}

func TestPGSearchStore_ResultIDs_TenantIsolation(t *testing.T) {
	factory := setupSearchTest(t)
	storeA := getSearchStore(t, factory, "tenant-a")
	storeB := getSearchStore(t, factory, "tenant-b")

	ctxA := ctxWithTenant("tenant-a")
	ctxB := ctxWithTenant("tenant-b")

	now := time.Now().UTC().Truncate(time.Microsecond)

	// Create a job for tenant A with 3 results.
	jobA := &spi.SearchJob{
		ID:          "job-iso-a",
		TenantID:    "tenant-a",
		Status:      "RUNNING",
		ModelRef:    spi.ModelRef{EntityName: "X", ModelVersion: "1"},
		Condition:   json.RawMessage(`{}`),
		PointInTime: now,
		CreateTime:  now,
	}
	if err := storeA.CreateJob(ctxA, jobA); err != nil {
		t.Fatalf("CreateJob(A) error: %v", err)
	}
	if err := storeA.SaveResults(ctxA, "job-iso-a", []string{"a1", "a2", "a3"}); err != nil {
		t.Fatalf("SaveResults(A) error: %v", err)
	}

	// Create a job for tenant B with 1 result.
	jobB := &spi.SearchJob{
		ID:          "job-iso-b",
		TenantID:    "tenant-b",
		Status:      "RUNNING",
		ModelRef:    spi.ModelRef{EntityName: "Y", ModelVersion: "1"},
		Condition:   json.RawMessage(`{}`),
		PointInTime: now,
		CreateTime:  now,
	}
	if err := storeB.CreateJob(ctxB, jobB); err != nil {
		t.Fatalf("CreateJob(B) error: %v", err)
	}
	if err := storeB.SaveResults(ctxB, "job-iso-b", []string{"b1"}); err != nil {
		t.Fatalf("SaveResults(B) error: %v", err)
	}

	// Tenant A sees only its 3 results.
	idsA, totalA, err := storeA.GetResultIDs(ctxA, "job-iso-a", 0, 100)
	if err != nil {
		t.Fatalf("GetResultIDs(A) error: %v", err)
	}
	if totalA != 3 {
		t.Errorf("tenant A total = %d, want 3", totalA)
	}
	if len(idsA) != 3 {
		t.Errorf("tenant A len = %d, want 3", len(idsA))
	}

	// Tenant B sees only its 1 result.
	idsB, totalB, err := storeB.GetResultIDs(ctxB, "job-iso-b", 0, 100)
	if err != nil {
		t.Fatalf("GetResultIDs(B) error: %v", err)
	}
	if totalB != 1 {
		t.Errorf("tenant B total = %d, want 1", totalB)
	}
	if len(idsB) != 1 {
		t.Errorf("tenant B len = %d, want 1", len(idsB))
	}

	// Tenant B trying to get results for tenant A's job → must fail.
	_, _, err = storeB.GetResultIDs(ctxB, "job-iso-a", 0, 100)
	if err == nil {
		t.Fatal("tenant B should NOT be able to GetResultIDs on tenant A's job")
	}
}

func TestPGSearchStore_Cancel_NotFound(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "search-tenant")
	ctx := ctxWithTenant("search-tenant")

	err := store.Cancel(ctx, "no-such-job")
	if err == nil {
		t.Fatal("Cancel() expected error for missing job, got nil")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("Cancel() expected ErrNotFound, got %v", err)
	}
}

func TestPGSearchStore_Cancel_Idempotent(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "search-tenant")
	ctx := ctxWithTenant("search-tenant")

	now := time.Now().UTC().Truncate(time.Microsecond)
	job := &spi.SearchJob{
		ID:          "job-cancel",
		TenantID:    "search-tenant",
		Status:      "RUNNING",
		ModelRef:    spi.ModelRef{EntityName: "V", ModelVersion: "1"},
		Condition:   json.RawMessage(`{}`),
		PointInTime: now,
		CreateTime:  now,
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob() error: %v", err)
	}

	// First cancel should succeed.
	if err := store.Cancel(ctx, "job-cancel"); err != nil {
		t.Fatalf("first Cancel() error: %v", err)
	}

	// Job should now be CANCELLED.
	got, err := store.GetJob(ctx, "job-cancel")
	if err != nil {
		t.Fatalf("GetJob() after cancel error: %v", err)
	}
	if got.Status != "CANCELLED" {
		t.Errorf("Status = %q, want CANCELLED", got.Status)
	}

	// Second cancel should be idempotent (return nil).
	if err := store.Cancel(ctx, "job-cancel"); err != nil {
		t.Errorf("second Cancel() should be idempotent, got error: %v", err)
	}
}

func TestPGSearchStore_Cancel_AlreadyTerminal(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "search-tenant")
	ctx := ctxWithTenant("search-tenant")

	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, terminalStatus := range []string{"SUCCESSFUL", "FAILED", "CANCELLED"} {
		jobID := "job-terminal-" + terminalStatus
		job := &spi.SearchJob{
			ID:          jobID,
			TenantID:    "search-tenant",
			Status:      terminalStatus,
			ModelRef:    spi.ModelRef{EntityName: "U", ModelVersion: "1"},
			Condition:   json.RawMessage(`{}`),
			PointInTime: now,
			CreateTime:  now,
		}
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob(%s) error: %v", terminalStatus, err)
		}

		// Cancel on terminal job should be idempotent (return nil).
		if err := store.Cancel(ctx, jobID); err != nil {
			t.Errorf("Cancel(%s) expected nil (idempotent), got: %v", terminalStatus, err)
		}

		// Status should remain unchanged.
		got, err := store.GetJob(ctx, jobID)
		if err != nil {
			t.Fatalf("GetJob(%s) error: %v", terminalStatus, err)
		}
		if got.Status != terminalStatus {
			t.Errorf("Status = %q, want %q (should not change terminal status)", got.Status, terminalStatus)
		}
	}
}

func TestPGSearchStore_ReapDoesNotReapRunning(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "search-tenant")
	ctx := ctxWithTenant("search-tenant")

	oldTime := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)

	// Old RUNNING job -- should NOT be reaped.
	runningJob := &spi.SearchJob{
		ID:          "job-running",
		TenantID:    "search-tenant",
		Status:      "RUNNING",
		ModelRef:    spi.ModelRef{EntityName: "C", ModelVersion: "1"},
		Condition:   json.RawMessage(`{}`),
		PointInTime: oldTime,
		CreateTime:  oldTime,
	}
	if err := store.CreateJob(ctx, runningJob); err != nil {
		t.Fatalf("CreateJob(running) error: %v", err)
	}

	reaped, err := store.ReapExpired(ctx, 1*time.Hour)
	if err != nil {
		t.Fatalf("ReapExpired() error: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 (running jobs should not be reaped)", reaped)
	}

	// Running job still there.
	got, err := store.GetJob(ctx, "job-running")
	if err != nil {
		t.Fatalf("GetJob(running) error: %v", err)
	}
	if got.Status != "RUNNING" {
		t.Errorf("Status = %q, want RUNNING", got.Status)
	}
}
