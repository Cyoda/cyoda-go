package search_test

// The reclaim sweep's second pass: the writes a sweep owes jobs it claimed but
// will not run.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func newSecondPassService(t *testing.T, factory *memory.StoreFactory, store spi.AsyncSearchStore) *search.SearchService {
	t.Helper()
	pool := search.NewWorkerPool(2, 8)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	return search.NewSearchService(factory, common.NewTestUUIDGenerator(), store, newTestConsistency(t, factory)).
		WithAsyncPool(pool).
		WithHeartbeat(time.Hour)
}

func jobStatus(t *testing.T, store spi.AsyncSearchStore, tenant, id string) string {
	t.Helper()
	job, err := store.GetJob(tenantCtx(tenant), id)
	if err != nil {
		t.Fatalf("GetJob(%s/%s): %v", tenant, id, err)
	}
	return job.Status
}

// Two tenants may each have a job with the same id. When one sweep claims
// both at their cap, each is owed its own FAILED write.
func TestReclaimStaleJobs_SameJobIDInTwoTenants_BothOwedWritesLand(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	base, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := newMainPoolBlockedStore(base)
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	createStaleReclaimJob(t, base, "tenant-a", "job-shared-id", ref, cond, time.Now())
	createStaleReclaimJob(t, base, "tenant-b", "job-shared-id", ref, cond, time.Now())

	svc := newSecondPassService(t, factory, store)
	t.Cleanup(store.release)

	store.blocked.Store(true)
	if _, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, 1); err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	store.release()
	svc.WaitReclaimSecondPass()

	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if got := jobStatus(t, base, tenant, "job-shared-id"); got != "FAILED" {
			t.Errorf("%s/job-shared-id status = %s, want FAILED: its owed write was lost", tenant, got)
		}
	}
}

// panicOnFirstFailStore panics on the first FAILED write it is sent.
type panicOnFirstFailStore struct {
	spi.AsyncSearchStore
	panicked atomic.Bool
}

func (s *panicOnFirstFailStore) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	if status == "FAILED" && s.panicked.CompareAndSwap(false, true) {
		panic("store fault")
	}
	return s.AsyncSearchStore.UpdateJobStatus(ctx, jobID, epoch, status, resultCount, errMsg, finishTime, calcTimeMs)
}

// A panic in one owed write ends the second pass, but the writes of the same
// batch it had not sent yet stay owed: the next sweep's pass sends them, even
// when that sweep owes nothing itself. The write that panicked is not retried;
// its job stays RUNNING and is claimed again once stale.
func TestReclaimStaleJobs_PanicInSecondPass_KeepsUnsentWritesOwed(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	base, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &panicOnFirstFailStore{AsyncSearchStore: base}
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
	ids := []string{"job-capped-1", "job-capped-2"}
	for _, id := range ids {
		createStaleReclaimJob(t, base, "tenant-a", id, ref, cond, time.Now())
	}

	health := &atomic.Bool{}
	health.Store(true)
	svc := newSecondPassService(t, factory, store).WithHealthFlag(health)

	if _, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, 1); err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	svc.WaitReclaimSecondPass()
	if !store.panicked.Load() {
		t.Fatal("no owed write was sent")
	}
	if health.Load() {
		t.Error("healthFlag = true after a panic in the second pass")
	}

	// Both jobs were just claimed, so this sweep claims nothing and owes
	// nothing; it still starts a pass for what is left owed.
	if _, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, 1); err != nil {
		t.Fatalf("ReclaimStaleJobs: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		svc.WaitReclaimSecondPass()
		failed := 0
		for _, id := range ids {
			if jobStatus(t, base, "tenant-a", id) == "FAILED" {
				failed++
			}
		}
		if failed == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("FAILED jobs = %d, want 1: the write the panic left unsent was lost", failed)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// recordingHandler keeps every log record at or above WARN.
type recordingHandler struct {
	mu      sync.Mutex
	records []string
}

func (h *recordingHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, fmt.Sprintf("%s %s", r.Level, r.Message))
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.records...)
}

// captureWarnings routes the default logger to a recorder for this test.
func captureWarnings(t *testing.T) *recordingHandler {
	t.Helper()
	h := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// refusingFailStore refuses every FAILED write with refusal.
type refusingFailStore struct {
	*orderedClaimStore
	refusal error
}

func (s *refusingFailStore) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	if status == "FAILED" {
		return fmt.Errorf("fail %s: %w", jobID, s.refusal)
	}
	return s.orderedClaimStore.UpdateJobStatus(ctx, jobID, epoch, status, resultCount, errMsg, finishTime, calcTimeMs)
}

// An owed FAILED write the store refuses as stale, terminal or missing means
// the job is no longer this claim's to settle: an ordinary end, logged below
// WARN, for the attempt-cap write and the undecodable job's write alike.
func TestReclaimStaleJobs_FencedRefusalOfOwedFailIsNotAFault(t *testing.T) {
	for _, refusal := range []error{spi.ErrStaleClaim, spi.ErrAlreadyTerminal, spi.ErrNotFound} {
		for _, kind := range []string{"capped", "undecodable"} {
			t.Run(kind+"/"+refusal.Error(), func(t *testing.T) {
				factory := memory.NewStoreFactory()
				t.Cleanup(func() { factory.Close() })
				base, err := factory.AsyncSearchStore(context.Background())
				if err != nil {
					t.Fatalf("AsyncSearchStore: %v", err)
				}
				ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
				cond := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "Alice"}
				ordered := &orderedClaimStore{mainPoolBlockedStore: newMainPoolBlockedStore(base)}
				maxAttempts := 1
				if kind == "undecodable" {
					// Released, so the claim is not counted and the job
					// is not at its cap; its options do not decode.
					createRunningReclaimJobAt(t, base, "tenant-a", "job-refused", ref, cond, time.Now(), time.Now())
					if err := base.Release(tenantCtx("tenant-a"), "job-refused", 1); err != nil {
						t.Fatalf("Release: %v", err)
					}
					ordered.undecodable = "job-refused"
				} else {
					createStaleReclaimJob(t, base, "tenant-a", "job-refused", ref, cond, time.Now())
				}
				store := &refusingFailStore{orderedClaimStore: ordered, refusal: refusal}
				svc := newSecondPassService(t, factory, store)

				logs := captureWarnings(t)
				if _, err := svc.ReclaimStaleJobs(context.Background(), 5*time.Minute, maxAttempts); err != nil {
					t.Fatalf("ReclaimStaleJobs: %v", err)
				}
				svc.WaitReclaimSecondPass()

				for _, rec := range logs.snapshot() {
					if kind == "undecodable" && rec == "ERROR failed to decode claimed search job; failing" {
						continue // the defect itself, logged at claim time
					}
					t.Errorf("logged %q for an owed FAILED write refused with %v; want below WARN", rec, refusal)
				}
			})
		}
	}
}
