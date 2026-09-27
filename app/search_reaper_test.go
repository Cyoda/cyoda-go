package app

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// nilJobClaimStore is a real AsyncSearchStore with one deliberate defect:
// ClaimStale hands back a slice containing a nil element. ReclaimStaleJobs
// dereferences each returned *spi.SearchJob (job.StaleClaims, job.TenantID),
// so a nil there panics — and, in an unrecovered goroutine, takes the whole
// process down. No current plugin returns nil, so this is hardening: the
// reclaim tick must survive a misbehaving store the way every other
// engine-work goroutine does.
type nilJobClaimStore struct {
	spi.AsyncSearchStore
}

func (s *nilJobClaimStore) ClaimStale(context.Context, time.Duration, int) ([]*spi.SearchJob, error) {
	return []*spi.SearchJob{nil}, nil
}

// newReclaimService builds a SearchService over store with a bounded pool, so
// reclaimStaleTick has the headroom (pool.Cap() - registrySize()) it needs to
// reach ClaimStale rather than returning early.
func newReclaimService(t *testing.T, factory *memory.StoreFactory, store spi.AsyncSearchStore) *search.SearchService {
	t.Helper()
	pool := search.NewWorkerPool(2, 8)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	return search.NewSearchService(factory, common.NewTestUUIDGenerator(), store).WithAsyncPool(pool)
}

// TestReclaimStaleTick_RecoversPanicAndLatchesHealth pins that one reclaim
// tick recovers a panic raised anywhere beneath it (here a nil job from a
// misbehaving ClaimStale), latches the node-health flag the same way the
// async-search executor's own recovery does, and returns normally so the
// ticker loop keeps running.
func TestReclaimStaleTick_RecoversPanicAndLatchesHealth(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	realAsync, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	svc := newReclaimService(t, factory, &nilJobClaimStore{AsyncSearchStore: realAsync})

	health := &atomic.Bool{}
	health.Store(true)

	// Must not panic out of the call.
	reclaimStaleTick(context.Background(), svc, 5*time.Minute, 3, health)

	if health.Load() {
		t.Error("healthFlag = true after a recovered panic in the reclaim tick; a node that has panicked has state nothing has verified")
	}
}

// TestReclaimStaleTick_HealthyStoreLeavesHealthAlone is the control: a tick
// against a well-behaved store (no stale jobs) must not latch the node
// unhealthy.
func TestReclaimStaleTick_HealthyStoreLeavesHealthAlone(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	realAsync, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	svc := newReclaimService(t, factory, realAsync)

	health := &atomic.Bool{}
	health.Store(true)

	reclaimStaleTick(context.Background(), svc, 5*time.Minute, 3, health)

	if !health.Load() {
		t.Error("healthFlag = false after an uneventful reclaim tick")
	}
}

// TestReapExpiredSnapshotsTick_HealthyStoreLeavesHealthAlone pins the split's
// other half: the snapshot-TTL sweep against a well-behaved store leaves
// health alone.
func TestReapExpiredSnapshotsTick_HealthyStoreLeavesHealthAlone(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	realAsync, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}

	health := &atomic.Bool{}
	health.Store(true)

	reapExpiredSnapshotsTick(context.Background(), realAsync, time.Hour, health)

	if !health.Load() {
		t.Error("healthFlag = false after an uneventful snapshot reaper tick")
	}
}

// reaperTestConfig sweeps fast: the snapshot reap every 10ms, the claim every
// 20ms.
func reaperTestConfig() *Config {
	return &Config{
		SearchReapInterval:         10 * time.Millisecond,
		SearchSnapshotTTL:          time.Hour,
		SearchJobHeartbeatInterval: 20 * time.Millisecond,
		SearchJobStaleAfter:        5 * time.Minute,
		SearchJobMaxAttempts:       1,
	}
}

// createReapTestJob persists a RUNNING job that decodes and re-runs, created
// createdAgo in the past.
func createReapTestJob(t *testing.T, store spi.AsyncSearchStore, id string, createdAgo time.Duration) {
	t.Helper()
	opts, err := json.Marshal(struct {
		Limit int `json:"limit"`
	}{Limit: 10})
	if err != nil {
		t.Fatalf("marshal opts: %v", err)
	}
	job := &spi.SearchJob{
		ID:         id,
		TenantID:   "tenant-reap",
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "person", ModelVersion: "1"},
		Condition:  []byte(`{"type":"group","operator":"AND","conditions":[]}`),
		SearchOpts: opts,
		CreateTime: time.Now().Add(-createdAgo),
	}
	if err := store.CreateJob(commontest.SystemUserContext("tenant-reap"), job); err != nil {
		t.Fatalf("CreateJob(%s): %v", id, err)
	}
}

// blockedReapStore blocks every ReapExpired, as a DELETE waiting on an
// exhausted main pool does, until its context ends or the test lets it go.
type blockedReapStore struct {
	spi.AsyncSearchStore
	reaping  chan struct{}
	letGo    chan struct{}
	letGoOne sync.Once
}

func (s *blockedReapStore) release() { s.letGoOne.Do(func() { close(s.letGo) }) }

func (s *blockedReapStore) ReapExpired(ctx context.Context, ttl time.Duration) (int, error) {
	select {
	case s.reaping <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-s.letGo:
		return 0, nil
	}
}

// awaitEpoch waits until the job's epoch reaches want.
func awaitEpoch(t *testing.T, store spi.AsyncSearchStore, id string, want int64, within time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	var epoch int64
	for time.Now().Before(deadline) {
		job, err := store.GetJob(commontest.SystemUserContext("tenant-reap"), id)
		if err != nil {
			t.Fatalf("GetJob(%s): %v", id, err)
		}
		if epoch = job.Epoch; epoch >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: epoch %d within %s, want %d", what, epoch, within, want)
}

// stopWithin calls stop and fails the test if it does not return in time.
func stopWithin(t *testing.T, stop func(), within time.Duration, what string) {
	t.Helper()
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(within):
		t.Fatalf("%s did not return within %s", what, within)
	}
}

// A snapshot reap blocked on the main pool does not stop the claim: a job
// released while the reap waits is still claimed.
func TestSearchReapers_BlockedReapDoesNotStopClaims(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	base, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &blockedReapStore{AsyncSearchStore: base, reaping: make(chan struct{}, 1), letGo: make(chan struct{})}
	svc := newReclaimService(t, factory, store)
	health := &atomic.Bool{}
	health.Store(true)

	stop := startSearchReapers(reaperTestConfig(), svc, store, health)
	t.Cleanup(stop)
	t.Cleanup(store.release) // runs before stop

	select {
	case <-store.reaping:
	case <-time.After(2 * time.Second):
		t.Fatal("no snapshot reap started within 2s")
	}
	createReapTestJob(t, base, "job-released", 0)
	if err := base.Release(commontest.SystemUserContext("tenant-reap"), "job-released", 1); err != nil {
		t.Fatalf("Release: %v", err)
	}
	awaitEpoch(t, base, "job-released", 2, 2*time.Second, "the claim of a released job while a snapshot reap is blocked")
}

// Stopping the sweeps ends a snapshot reap blocked on the main pool.
func TestSearchReapers_StopEndsBlockedReap(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	base, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &blockedReapStore{AsyncSearchStore: base, reaping: make(chan struct{}, 1), letGo: make(chan struct{})}
	t.Cleanup(store.release)
	svc := newReclaimService(t, factory, store)
	health := &atomic.Bool{}
	health.Store(true)

	stop := startSearchReapers(reaperTestConfig(), svc, store, health)
	select {
	case <-store.reaping:
	case <-time.After(2 * time.Second):
		t.Fatal("no snapshot reap started within 2s")
	}
	stopWithin(t, stop, 2*time.Second, "stopping the sweeps with a snapshot reap blocked")
}

// blockedFailStore blocks every FAILED write, as one waiting on an exhausted
// main pool does, until its context ends. ended is set once such a write has
// returned.
type blockedFailStore struct {
	spi.AsyncSearchStore
	failing chan struct{}
	ended   atomic.Bool
}

func (s *blockedFailStore) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	if status != "FAILED" {
		return s.AsyncSearchStore.UpdateJobStatus(ctx, jobID, epoch, status, resultCount, errMsg, finishTime, calcTimeMs)
	}
	select {
	case s.failing <- struct{}{}:
	default:
	}
	<-ctx.Done()
	s.ended.Store(true)
	return ctx.Err()
}

// Stopping the sweeps ends the second pass's blocked write and waits for it:
// when stop returns, no write the sweeps started is still in flight. The job
// it did not fail stays RUNNING at its claimed epoch, to be claimed again.
func TestSearchReapers_StopEndsAndAwaitsBlockedSecondPass(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	base, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &blockedFailStore{AsyncSearchStore: base, failing: make(chan struct{}, 1)}
	svc := newReclaimService(t, factory, store)
	health := &atomic.Bool{}
	health.Store(true)

	// Stale, and at its cap on the first claim (maxAttempts 1).
	createReapTestJob(t, base, "job-capped", time.Hour)
	stop := startSearchReapers(reaperTestConfig(), svc, store, health)
	select {
	case <-store.failing:
	case <-time.After(2 * time.Second):
		stop()
		t.Fatal("the capped job's FAILED write was not sent within 2s")
	}
	stopWithin(t, stop, 2*time.Second, "stopping the sweeps with a second-pass write blocked")
	if !store.ended.Load() {
		t.Fatal("stop returned while the second pass's write was still in flight")
	}
	job, err := base.GetJob(commontest.SystemUserContext("tenant-reap"), "job-capped")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != "RUNNING" || job.Epoch != 2 {
		t.Fatalf("job = (%s, epoch %d), want (RUNNING, epoch 2)", job.Status, job.Epoch)
	}
}
