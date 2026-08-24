package search_test

// Per-tenant async-search in-flight cap: the worker pool is one per node and
// shared by every tenant, so without a per-tenant bound one tenant's burst
// occupies every worker AND fills the queue, and every other tenant on the
// node gets 503 SEARCH_QUEUE_FULL until those jobs finish — up to the
// backend's async-scan ceiling. These tests pin the cap, the fairness it
// buys, and that the count is released on every terminal path.

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// blockingSaveStore wraps a real AsyncSearchStore and parks SaveResults
// until release is closed (or the job's own context is cancelled), so a
// submitted job stays in flight — queued or running — for as long as the
// test needs it to. saveErr, when set, is returned instead of delegating,
// which drives the job to FAILED.
type blockingSaveStore struct {
	spi.AsyncSearchStore
	release chan struct{}
	saveErr error
	deletes atomic.Int32
}

func (s *blockingSaveStore) SaveResults(ctx context.Context, jobID string, epoch int64, entityIDs iter.Seq[string]) error {
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.AsyncSearchStore.SaveResults(ctx, jobID, epoch, entityIDs)
}

// DeleteJob counts the submit-rejection cleanup: a submission the engine
// refuses must not leave a RUNNING job row behind.
func (s *blockingSaveStore) DeleteJob(ctx context.Context, jobID string) error {
	s.deletes.Add(1)
	return s.AsyncSearchStore.DeleteJob(ctx, jobID)
}

// matchEverything is translatable to a spi.Filter, so the executor takes the
// Iterate -> SaveResults path (where blockingSaveStore's park applies)
// rather than the untranslatable fallback.
var matchEverything = &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}

// newCappedService wires a service whose pool is wide enough that the pool's
// own bounds are never what rejects a submission — only the per-tenant cap
// is — and returns it alongside the store whose release channel unparks
// SaveResults.
func newCappedService(t *testing.T, base *memory.StoreFactory, maxPerTenant int, saveErr error) (*search.SearchService, *blockingSaveStore) {
	t.Helper()
	realAsync, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &blockingSaveStore{AsyncSearchStore: realAsync, release: make(chan struct{}), saveErr: saveErr}

	pool := search.NewWorkerPool(4, 64)
	t.Cleanup(func() { pool.Drain(context.Background()) })

	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), store).
		WithAsyncPool(pool).
		WithAsyncMaxPerTenant(maxPerTenant).
		WithHeartbeat(50 * time.Millisecond)
	return svc, store
}

// submitEventually retries SubmitAsync until it is accepted or the deadline
// elapses — used where the expectation is "this tenant's slot frees up",
// which happens in the executor's own defer, just after the terminal write
// the caller polls for.
func submitEventually(t *testing.T, svc *search.SearchService, ctx context.Context, ref spi.ModelRef, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		jobID, err := svc.SubmitAsync(ctx, ref, matchEverything, search.SearchOptions{})
		if err == nil {
			return jobID
		}
		if time.Now().After(deadline) {
			t.Fatalf("SubmitAsync never became accepted again within %s: %v", timeout, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func assertQueueFull(t *testing.T, err error) {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("err = %v (%T), want the shared *common.AppError from search.QueueFullError()", err, err)
	}
	if appErr.Status != http.StatusServiceUnavailable || appErr.Code != common.ErrCodeSearchQueueFull || !appErr.Retryable {
		t.Errorf("err = %d/%s (retryable=%v), want %d/%s retryable — the existing queue-full mapping, not a new code",
			appErr.Status, appErr.Code, appErr.Retryable, http.StatusServiceUnavailable, common.ErrCodeSearchQueueFull)
	}
}

// TestSubmitAsync_PerTenantCap_RejectsOverCapAndSparesOtherTenants is the
// core of the feature: a tenant at its cap is rejected with the EXISTING
// queue-full error while a different tenant, on the same node and the same
// pool, still submits successfully.
func TestSubmitAsync_PerTenantCap_RejectsOverCapAndSparesOtherTenants(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ref := spi.ModelRef{EntityName: "capitem", ModelVersion: "1"}
	ctxA := tenantCtx("tenant-a")
	ctxB := tenantCtx("tenant-b")
	saveMinimalModel(t, ctxA, base, ref)
	saveMinimalModel(t, ctxB, base, ref)

	svc, store := newCappedService(t, base, 2, nil)
	defer close(store.release)

	for i := 0; i < 2; i++ {
		if _, err := svc.SubmitAsync(ctxA, ref, matchEverything, search.SearchOptions{}); err != nil {
			t.Fatalf("tenant-a submit %d: %v", i, err)
		}
	}

	_, err := svc.SubmitAsync(ctxA, ref, matchEverything, search.SearchOptions{})
	if err == nil {
		t.Fatal("tenant-a's third submit was accepted; the per-tenant in-flight cap (2) is not enforced")
	}
	assertQueueFull(t, err)

	// The whole point of the cap: tenant-b is unaffected by tenant-a's burst.
	if _, err := svc.SubmitAsync(ctxB, ref, matchEverything, search.SearchOptions{}); err != nil {
		t.Fatalf("tenant-b submit was rejected because tenant-a filled the node: %v", err)
	}

	// A rejected submission must leave no RUNNING job row behind — the same
	// cleanup the pool's own queue-full rejection does.
	if got := store.deletes.Load(); got != 1 {
		t.Errorf("DeleteJob calls = %d, want 1 (the rejected submission's job row must be removed, not left RUNNING)", got)
	}
}

// TestSubmitAsync_PerTenantCap_ReleasedOnEveryTerminalPath pins that the
// in-flight count is decremented however a job ends — success, failure, or
// cancel — so a tenant is never permanently locked out by a job that already
// finished.
func TestSubmitAsync_PerTenantCap_ReleasedOnEveryTerminalPath(t *testing.T) {
	ref := spi.ModelRef{EntityName: "capitem", ModelVersion: "1"}

	t.Run("success", func(t *testing.T) {
		base := memory.NewStoreFactory()
		defer base.Close()
		ctx := tenantCtx("tenant-success")
		saveMinimalModel(t, ctx, base, ref)

		svc, store := newCappedService(t, base, 1, nil)
		jobID, err := svc.SubmitAsync(ctx, ref, matchEverything, search.SearchOptions{})
		if err != nil {
			t.Fatalf("SubmitAsync: %v", err)
		}
		if _, err := svc.SubmitAsync(ctx, ref, matchEverything, search.SearchOptions{}); err == nil {
			t.Fatal("second submit accepted while the first is still in flight; cap=1 is not enforced")
		}
		close(store.release)
		if status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second); status.Status != "SUCCESSFUL" {
			t.Fatalf("status = %q, want SUCCESSFUL", status.Status)
		}
		submitEventually(t, svc, ctx, ref, 5*time.Second)
	})

	t.Run("failure", func(t *testing.T) {
		base := memory.NewStoreFactory()
		defer base.Close()
		ctx := tenantCtx("tenant-failure")
		saveMinimalModel(t, ctx, base, ref)

		svc, store := newCappedService(t, base, 1, errors.New("save exploded"))
		jobID, err := svc.SubmitAsync(ctx, ref, matchEverything, search.SearchOptions{})
		if err != nil {
			t.Fatalf("SubmitAsync: %v", err)
		}
		close(store.release)
		if status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second); status.Status != "FAILED" {
			t.Fatalf("status = %q, want FAILED", status.Status)
		}
		submitEventually(t, svc, ctx, ref, 5*time.Second)
	})

	t.Run("cancel", func(t *testing.T) {
		base := memory.NewStoreFactory()
		defer base.Close()
		ctx := tenantCtx("tenant-cancel")
		saveMinimalModel(t, ctx, base, ref)

		svc, store := newCappedService(t, base, 1, nil)
		defer close(store.release)

		jobID, err := svc.SubmitAsync(ctx, ref, matchEverything, search.SearchOptions{})
		if err != nil {
			t.Fatalf("SubmitAsync: %v", err)
		}
		if _, err := svc.CancelAsync(ctx, jobID); err != nil {
			t.Fatalf("CancelAsync: %v", err)
		}
		submitEventually(t, svc, ctx, ref, 5*time.Second)
	})
}

// TestSubmitAsync_PerTenantCap_DisabledByZero pins the documented escape
// hatch: a cap of zero (CYODA_SEARCH_ASYNC_MAX_PER_TENANT=0) turns the
// per-tenant bound off entirely, leaving only the pool's own bounds.
func TestSubmitAsync_PerTenantCap_DisabledByZero(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ref := spi.ModelRef{EntityName: "capitem", ModelVersion: "1"}
	ctx := tenantCtx("tenant-uncapped")
	saveMinimalModel(t, ctx, base, ref)

	svc, store := newCappedService(t, base, 0, nil)
	defer close(store.release)

	for i := 0; i < 8; i++ {
		if _, err := svc.SubmitAsync(ctx, ref, matchEverything, search.SearchOptions{}); err != nil {
			t.Fatalf("submit %d rejected with the cap disabled: %v", i, err)
		}
	}
}
