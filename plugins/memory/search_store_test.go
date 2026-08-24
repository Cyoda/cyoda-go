package memory_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func tenantCtx(tid spi.TenantID) context.Context {
	uc := &spi.UserContext{
		UserID:   "test-user",
		UserName: "Test User",
		Tenant:   spi.Tenant{ID: tid, Name: string(tid)},
		Roles:    []string{"USER"},
	}
	return spi.WithUserContext(context.Background(), uc)
}

func newSearchStore(t *testing.T) spi.AsyncSearchStore {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	ctx := tenantCtx("test-tenant")
	store, err := factory.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore() error: %v", err)
	}
	if store == nil {
		t.Fatal("AsyncSearchStore() returned nil")
	}
	return store
}

func TestMemorySearchStore_CreateAndGetJob(t *testing.T) {
	store := newSearchStore(t)
	ctx := tenantCtx("test-tenant")

	now := time.Now().UTC().Truncate(time.Millisecond)
	job := &spi.SearchJob{
		ID:          "job-001",
		TenantID:    "test-tenant",
		Status:      "RUNNING",
		ModelRef:    spi.ModelRef{EntityName: "Person", ModelVersion: "1"},
		Condition:   []byte(`{"field":"name","op":"eq","value":"Alice"}`),
		PointInTime: now,
		SearchOpts:  []byte(`{"sort":"asc"}`),
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
	if got.TenantID != "test-tenant" {
		t.Errorf("TenantID = %q, want %q", got.TenantID, "test-tenant")
	}
	if got.Status != "RUNNING" {
		t.Errorf("Status = %q, want %q", got.Status, "RUNNING")
	}
	if got.ModelRef.EntityName != "Person" || got.ModelRef.ModelVersion != "1" {
		t.Errorf("ModelRef = %v, want Person.1", got.ModelRef)
	}
	if string(got.Condition) != `{"field":"name","op":"eq","value":"Alice"}` {
		t.Errorf("Condition = %s", got.Condition)
	}
	if string(got.SearchOpts) != `{"sort":"asc"}` {
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

func TestMemorySearchStore_GetJob_NotFound(t *testing.T) {
	store := newSearchStore(t)
	ctx := tenantCtx("test-tenant")

	_, err := store.GetJob(ctx, "nonexistent")
	if err == nil {
		t.Fatal("GetJob() expected error for missing job, got nil")
	}
}

func TestMemorySearchStore_TenantIsolation(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })

	ctxA := tenantCtx("tenant-a")
	ctxB := tenantCtx("tenant-b")

	storeA, err := factory.AsyncSearchStore(ctxA)
	if err != nil {
		t.Fatalf("AsyncSearchStore(A) error: %v", err)
	}
	storeB, err := factory.AsyncSearchStore(ctxB)
	if err != nil {
		t.Fatalf("AsyncSearchStore(B) error: %v", err)
	}

	now := time.Now().UTC()
	job := &spi.SearchJob{
		ID:         "job-iso",
		TenantID:   "tenant-a",
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "X", ModelVersion: "1"},
		CreateTime: now,
	}
	if err := storeA.CreateJob(ctxA, job); err != nil {
		t.Fatalf("CreateJob(A) error: %v", err)
	}

	// Tenant A can see it
	got, err := storeA.GetJob(ctxA, "job-iso")
	if err != nil {
		t.Fatalf("GetJob(A) error: %v", err)
	}
	if got.ID != "job-iso" {
		t.Errorf("tenant A should see job-iso")
	}

	// Tenant B cannot see it
	_, err = storeB.GetJob(ctxB, "job-iso")
	if err == nil {
		t.Fatal("tenant B should NOT see tenant A's job")
	}
}

func TestMemorySearchStore_UpdateJobStatus(t *testing.T) {
	store := newSearchStore(t)
	ctx := tenantCtx("test-tenant")

	now := time.Now().UTC()
	job := &spi.SearchJob{
		ID:         "job-upd",
		TenantID:   "test-tenant",
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "Y", ModelVersion: "1"},
		CreateTime: now,
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob() error: %v", err)
	}

	finishTime := now.Add(5 * time.Second)
	err := store.UpdateJobStatus(ctx, "job-upd", 1, "SUCCESSFUL", 42, "", finishTime, 1234)
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
	if got.FinishTime == nil || !got.FinishTime.Equal(finishTime) {
		t.Errorf("FinishTime = %v, want %v", got.FinishTime, finishTime)
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want empty", got.Error)
	}

	// A second UpdateJobStatus against the now-terminal job must be refused
	// (terminal statuses are write-once) — a separate, still-RUNNING job is
	// used to exercise the error-message path.
	failJob := &spi.SearchJob{
		ID:         "job-upd-2",
		TenantID:   "test-tenant",
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "Y", ModelVersion: "1"},
		CreateTime: now,
	}
	if err := store.CreateJob(ctx, failJob); err != nil {
		t.Fatalf("CreateJob(failJob) error: %v", err)
	}
	err = store.UpdateJobStatus(ctx, "job-upd-2", 1, "FAILED", 0, "something broke", finishTime, 999)
	if err != nil {
		t.Fatalf("UpdateJobStatus(FAILED) error: %v", err)
	}
	got, _ = store.GetJob(ctx, "job-upd-2")
	if got.Status != "FAILED" {
		t.Errorf("Status = %q, want FAILED", got.Status)
	}
	if got.Error != "something broke" {
		t.Errorf("Error = %q, want 'something broke'", got.Error)
	}

	// The original job-upd, now terminal, must reject a further write.
	err = store.UpdateJobStatus(ctx, "job-upd", 1, "FAILED", 0, "should be refused", finishTime, 1)
	if !errors.Is(err, spi.ErrAlreadyTerminal) {
		t.Errorf("UpdateJobStatus() on terminal job: err = %v, want ErrAlreadyTerminal", err)
	}
}

func TestMemorySearchStore_SaveAndGetResults(t *testing.T) {
	store := newSearchStore(t)
	ctx := tenantCtx("test-tenant")

	now := time.Now().UTC()
	job := &spi.SearchJob{
		ID:         "job-res",
		TenantID:   "test-tenant",
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "Z", ModelVersion: "1"},
		CreateTime: now,
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob() error: %v", err)
	}

	ids := []string{"e1", "e2", "e3", "e4", "e5"}
	if err := store.SaveResults(ctx, "job-res", 1, slices.Values(ids)); err != nil {
		t.Fatalf("SaveResults() error: %v", err)
	}

	// Get all results
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

	// Paginated: offset=1, limit=2
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

	// Offset beyond end
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

func TestMemorySearchStore_DeleteJob(t *testing.T) {
	store := newSearchStore(t)
	ctx := tenantCtx("test-tenant")

	now := time.Now().UTC()
	job := &spi.SearchJob{
		ID:         "job-del",
		TenantID:   "test-tenant",
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "W", ModelVersion: "1"},
		CreateTime: now,
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob() error: %v", err)
	}
	if err := store.SaveResults(ctx, "job-del", 1, slices.Values([]string{"e1", "e2"})); err != nil {
		t.Fatalf("SaveResults() error: %v", err)
	}
	// Terminal statuses are write-once; results must be saved before the
	// job transitions to SUCCESSFUL.
	if err := store.UpdateJobStatus(ctx, "job-del", 1, "SUCCESSFUL", 2, "", now, 0); err != nil {
		t.Fatalf("UpdateJobStatus() error: %v", err)
	}

	if err := store.DeleteJob(ctx, "job-del"); err != nil {
		t.Fatalf("DeleteJob() error: %v", err)
	}

	// Job should be gone
	_, err := store.GetJob(ctx, "job-del")
	if err == nil {
		t.Fatal("GetJob() expected error after delete")
	}

	// Results should be gone too
	ids, total, err := store.GetResultIDs(ctx, "job-del", 0, 100)
	if err == nil && total > 0 {
		t.Errorf("expected no results after delete, got total=%d ids=%v", total, ids)
	}
}

func TestMemorySearchStore_ReapExpired(t *testing.T) {
	store := newSearchStore(t)
	ctx := tenantCtx("test-tenant")

	oldTime := time.Now().UTC().Add(-2 * time.Hour)
	newTime := time.Now().UTC()

	// Old completed job
	oldJob := &spi.SearchJob{
		ID:         "job-old",
		TenantID:   "test-tenant",
		Status:     "SUCCESSFUL",
		ModelRef:   spi.ModelRef{EntityName: "A", ModelVersion: "1"},
		CreateTime: oldTime,
	}
	ft := oldTime.Add(time.Second)
	oldJob.FinishTime = &ft
	if err := store.CreateJob(ctx, oldJob); err != nil {
		t.Fatalf("CreateJob(old) error: %v", err)
	}

	// New completed job
	newJob := &spi.SearchJob{
		ID:         "job-new",
		TenantID:   "test-tenant",
		Status:     "SUCCESSFUL",
		ModelRef:   spi.ModelRef{EntityName: "B", ModelVersion: "1"},
		CreateTime: newTime,
	}
	ft2 := newTime
	newJob.FinishTime = &ft2
	if err := store.CreateJob(ctx, newJob); err != nil {
		t.Fatalf("CreateJob(new) error: %v", err)
	}

	// Reap with 1h TTL: old job should be reaped, new should survive
	reaped, err := store.ReapExpired(ctx, 1*time.Hour)
	if err != nil {
		t.Fatalf("ReapExpired() error: %v", err)
	}
	if reaped != 1 {
		t.Errorf("reaped = %d, want 1", reaped)
	}

	// Old job gone
	_, err = store.GetJob(ctx, "job-old")
	if err == nil {
		t.Error("old job should have been reaped")
	}

	// New job still there
	_, err = store.GetJob(ctx, "job-new")
	if err != nil {
		t.Errorf("new job should still exist: %v", err)
	}
}

func TestMemorySearchStore_Cancel_NotFound(t *testing.T) {
	store := newSearchStore(t)
	ctx := tenantCtx("test-tenant")

	err := store.Cancel(ctx, "no-such-job", time.Now().UTC())
	if err == nil {
		t.Fatal("Cancel() expected error for missing job, got nil")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("Cancel() expected ErrNotFound, got %v", err)
	}
}

func TestMemorySearchStore_Cancel_Idempotent(t *testing.T) {
	store := newSearchStore(t)
	ctx := tenantCtx("test-tenant")

	now := time.Now().UTC()
	job := &spi.SearchJob{
		ID:         "job-cancel",
		TenantID:   "test-tenant",
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "V", ModelVersion: "1"},
		CreateTime: now,
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob() error: %v", err)
	}

	// First cancel should succeed and stamp the finish time.
	firstFinish := now.Add(1 * time.Minute)
	if err := store.Cancel(ctx, "job-cancel", firstFinish); err != nil {
		t.Fatalf("first Cancel() error: %v", err)
	}

	// Job should now be CANCELLED with the stamped finish time.
	got, err := store.GetJob(ctx, "job-cancel")
	if err != nil {
		t.Fatalf("GetJob() after cancel error: %v", err)
	}
	if got.Status != "CANCELLED" {
		t.Errorf("Status = %q, want CANCELLED", got.Status)
	}
	if got.FinishTime == nil {
		t.Fatal("FinishTime should be set after Cancel")
	}
	if !got.FinishTime.Equal(firstFinish) {
		t.Errorf("FinishTime = %v, want %v", got.FinishTime, firstFinish)
	}

	// Second cancel with a LATER time should be idempotent (return nil) and
	// must not overwrite the original finish time.
	laterFinish := firstFinish.Add(1 * time.Hour)
	if err := store.Cancel(ctx, "job-cancel", laterFinish); err != nil {
		t.Errorf("second Cancel() should be idempotent, got error: %v", err)
	}
	got2, err := store.GetJob(ctx, "job-cancel")
	if err != nil {
		t.Fatalf("GetJob() after second cancel error: %v", err)
	}
	if got2.FinishTime == nil || !got2.FinishTime.Equal(firstFinish) {
		t.Errorf("re-cancel must not overwrite FinishTime: got %v, want %v", got2.FinishTime, firstFinish)
	}
}

func TestMemorySearchStore_Cancel_AlreadyTerminal(t *testing.T) {
	store := newSearchStore(t)
	ctx := tenantCtx("test-tenant")

	now := time.Now().UTC()
	for _, terminalStatus := range []string{"SUCCESSFUL", "FAILED", "CANCELLED"} {
		job := &spi.SearchJob{
			ID:         "job-terminal-" + terminalStatus,
			TenantID:   "test-tenant",
			Status:     terminalStatus,
			ModelRef:   spi.ModelRef{EntityName: "U", ModelVersion: "1"},
			CreateTime: now,
		}
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob(%s) error: %v", terminalStatus, err)
		}

		// Cancel on terminal job should be idempotent (return nil) and must
		// not stamp a finish time onto a job that never had one.
		if err := store.Cancel(ctx, "job-terminal-"+terminalStatus, now.Add(1*time.Hour)); err != nil {
			t.Errorf("Cancel(%s) expected nil (idempotent), got: %v", terminalStatus, err)
		}

		// Status should remain unchanged.
		got, err := store.GetJob(ctx, "job-terminal-"+terminalStatus)
		if err != nil {
			t.Fatalf("GetJob(%s) error: %v", terminalStatus, err)
		}
		if got.Status != terminalStatus {
			t.Errorf("Status = %q, want %q (should not change terminal status)", got.Status, terminalStatus)
		}
		if got.FinishTime != nil {
			t.Errorf("FinishTime = %v, want nil (Cancel on already-terminal job must not stamp a finish time)", got.FinishTime)
		}
	}
}

func TestMemorySearchStore_ReapDoesNotReapRunning(t *testing.T) {
	store := newSearchStore(t)
	ctx := tenantCtx("test-tenant")

	oldTime := time.Now().UTC().Add(-2 * time.Hour)

	// Old RUNNING job -- should NOT be reaped
	runningJob := &spi.SearchJob{
		ID:         "job-running",
		TenantID:   "test-tenant",
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "C", ModelVersion: "1"},
		CreateTime: oldTime,
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

	// Running job still there
	got, err := store.GetJob(ctx, "job-running")
	if err != nil {
		t.Fatalf("GetJob(running) error: %v", err)
	}
	if got.Status != "RUNNING" {
		t.Errorf("Status = %q, want RUNNING", got.Status)
	}
}
