package search_test

// Hardening for the per-tenant in-flight cap's accounting: the registry's
// invariants held only because of what its single call site happens to do, and
// the cap was enforced only after a full round of validation plus an INSERT.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestRegisterJob_DuplicateIDIsRefused pins the registry's accounting as
// STRUCTURALLY sound rather than sound by call-site discipline.
//
// registerJob assigned into the map unconditionally after the cap check. A
// second registration of the same jobID would overwrite the first handle —
// leaking its cancel func, so nothing could ever cancel that job — while
// incrementing the tenant's in-flight count a second time. Deregistering then
// removes one map entry and decrements once, so the tenant is left permanently
// one slot short, for the rest of the process's life. Unreachable through
// SubmitAsync (fresh time-UUIDs, one call site), which is exactly why it needs
// a guard rather than a comment.
func TestRegisterJob_DuplicateIDIsRefused(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	searchStore, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), searchStore)

	uc := &spi.UserContext{Tenant: spi.Tenant{ID: "tenant-dup"}}
	firstCancelled := false
	first := func() { firstCancelled = true }
	if ok := svc.RegisterJobForTest("job-1", first, uc); !ok {
		t.Fatal("first registerJob = false, want true")
	}
	if ok := svc.RegisterJobForTest("job-1", func() {}, uc); ok {
		t.Error("registerJob accepted a duplicate jobID; the first cancel handle is dropped (nothing can cancel that job) and the tenant is charged twice for one slot")
	}
	if got := svc.TenantInFlightForTest("tenant-dup"); got != 1 {
		t.Errorf("tenantInFlight = %d, want 1 — one registered job is one slot", got)
	}
	if got := svc.RegisteredJobCountForTest(); got != 1 {
		t.Errorf("registry size = %d, want 1", got)
	}

	svc.DeregisterJobForTest("job-1")
	if got := svc.TenantInFlightForTest("tenant-dup"); got != 0 {
		t.Errorf("tenantInFlight after deregister = %d, want 0 — a double-count leaks the slot permanently", got)
	}
	if firstCancelled {
		t.Error("deregisterJob must not invoke the cancel handle")
	}
}

// createCountingStore counts CreateJob and DeleteJob, and can run a one-shot
// hook inside CreateJob so a test can open the window between the cheap
// pre-check and the authoritative check-and-register.
type createCountingStore struct {
	spi.AsyncSearchStore
	creates atomic.Int32
	deletes atomic.Int32

	hookArmed atomic.Bool
	hook      func()
}

func (s *createCountingStore) CreateJob(ctx context.Context, job *spi.SearchJob) error {
	s.creates.Add(1)
	if s.hook != nil && s.hookArmed.CompareAndSwap(true, false) {
		s.hook()
	}
	return s.AsyncSearchStore.CreateJob(ctx, job)
}

func (s *createCountingStore) DeleteJob(ctx context.Context, jobID string) error {
	s.deletes.Add(1)
	return s.AsyncSearchStore.DeleteJob(ctx, jobID)
}

// TestSubmitAsync_PerTenantCap_RejectsBeforeCreateJob pins that a capped
// tenant's rejected submit costs no store WRITES.
//
// The cap was enforced only at registerJob, which runs AFTER CreateJob. So a
// tenant hammering a node it is already capped on still paid a full validation
// pass, an INSERT and a DELETE per rejected submit — write churn every OTHER
// tenant on the node contends with, on precisely the axis the cap exists to
// sever. A cheap non-authoritative pre-check makes the steady-state rejection
// free; the check-and-register inside registerJob stays the authority.
func TestSubmitAsync_PerTenantCap_RejectsBeforeCreateJob(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ref := spi.ModelRef{EntityName: "precheckitem", ModelVersion: "1"}
	ctxA := tenantCtx("tenant-pre-a")
	saveMinimalModel(t, ctxA, base, ref)

	realAsync, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	blocking := &blockingSaveStore{AsyncSearchStore: realAsync, release: make(chan struct{})}
	store := &createCountingStore{AsyncSearchStore: blocking}
	defer close(blocking.release)

	pool := search.NewWorkerPool(4, 64)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), store).
		WithAsyncPool(pool).
		WithAsyncMaxPerTenant(1).
		WithHeartbeat(50 * time.Millisecond)

	if _, err := svc.SubmitAsync(ctxA, ref, matchEverything, search.SearchOptions{}); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	createsAfterAccepted := store.creates.Load()

	for i := 0; i < 3; i++ {
		_, err := svc.SubmitAsync(ctxA, ref, matchEverything, search.SearchOptions{})
		if err == nil {
			t.Fatalf("submit %d was accepted; the per-tenant cap (1) is not enforced", i)
		}
		assertQueueFull(t, err)
	}

	if got := store.creates.Load(); got != createsAfterAccepted {
		t.Errorf("CreateJob calls = %d, want %d — a submit the cap rejects must not reach the store at all",
			got, createsAfterAccepted)
	}
	if got := store.deletes.Load(); got != 0 {
		t.Errorf("DeleteJob calls = %d, want 0 — nothing was created, so there is nothing to compensate", got)
	}
}

// TestSubmitAsync_PerTenantCap_RegisterStaysTheAuthority is the other half:
// the pre-check is a cheap filter, NOT the decision. When the tenant's last
// free slot is taken between the pre-check and the register, the register must
// still reject — and the job row it already created must be deleted rather
// than left RUNNING.
//
// The window is opened deterministically: the store's CreateJob hook runs a
// competing submission (from another goroutine, so the executor's registration
// completes) while the first submission is mid-CreateJob.
func TestSubmitAsync_PerTenantCap_RegisterStaysTheAuthority(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ref := spi.ModelRef{EntityName: "authorityitem", ModelVersion: "1"}
	ctxA := tenantCtx("tenant-authority")
	saveMinimalModel(t, ctxA, base, ref)

	realAsync, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	blocking := &blockingSaveStore{AsyncSearchStore: realAsync, release: make(chan struct{})}
	store := &createCountingStore{AsyncSearchStore: blocking}
	defer close(blocking.release)

	pool := search.NewWorkerPool(4, 64)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), store).
		WithAsyncPool(pool).
		WithAsyncMaxPerTenant(1).
		WithHeartbeat(50 * time.Millisecond)

	competitorDone := make(chan error, 1)
	store.hook = func() {
		// Runs INSIDE the first submission's CreateJob, after its pre-check
		// saw a free slot. The competitor takes that slot and returns.
		go func() {
			_, cErr := svc.SubmitAsync(ctxA, ref, matchEverything, search.SearchOptions{})
			competitorDone <- cErr
		}()
		select {
		case cErr := <-competitorDone:
			if cErr != nil {
				t.Errorf("competing submit was rejected: %v", cErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("competing submit did not complete")
		}
	}
	store.hookArmed.Store(true)

	_, err = svc.SubmitAsync(ctxA, ref, matchEverything, search.SearchOptions{})
	if err == nil {
		t.Fatal("the submission whose slot was taken mid-CreateJob was accepted; registerJob is no longer the authority")
	}
	assertQueueFull(t, err)

	if got := store.deletes.Load(); got != 1 {
		t.Errorf("DeleteJob calls = %d, want 1 — a row created before an authoritative rejection must not be left RUNNING", got)
	}
}
