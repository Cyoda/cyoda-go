// Package heartbeatpolicy proves the startHeartbeat busy-tick policy — a
// Heartbeat tick that meets a lock its own job's SaveResults holds is a
// distinct, transient outcome that must not abort a healthy job — against a
// fully synthetic StoreFactory/AsyncSearchStore pair, with no dependency on
// the memory or sqlite plugins.
//
// It exists because, at the time this fix landed, internal/domain/search's
// own test package (search_test) cannot compile in this workspace: several
// of its other _test.go files import plugins/memory and plugins/sqlite, and
// those plugins have not yet been updated for an in-progress ScheduledTaskStore
// SPI surface consumed here only through the worktree's local go.work line.
// The full-fidelity, plugin-backed equivalent of this test lives at
// internal/domain/search/heartbeat_busy_test.go and will start compiling and
// running once the memory and sqlite plugins catch up to that SPI surface —
// at which point this package becomes redundant with it and should be
// removed.
package heartbeatpolicy_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// ---------------------------------------------------------------------------
// Fakes — no plugins/memory, no plugins/sqlite.
// ---------------------------------------------------------------------------

func tenantCtx(tenantID string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "test-user",
		UserName: "Test User",
		Tenant: spi.Tenant{
			ID:   spi.TenantID(tenantID),
			Name: "Test Tenant",
		},
		Roles: []string{"ROLE_USER"},
	})
}

var matchEverything = &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}

// fakeModelStore answers every Get with the same minimal descriptor — a
// single String leaf "name", enough for loadFieldsMap to succeed. Every
// other ModelStore method is unused by SubmitAsync/runAsyncJob for a
// lifecycle-only condition like matchEverything, so they are left to the
// embedded nil interface.
type fakeModelStore struct {
	spi.ModelStore
	desc *spi.ModelDescriptor
}

func (s *fakeModelStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	return s.desc, nil
}

func newFakeModelStore(t *testing.T, ref spi.ModelRef) *fakeModelStore {
	t.Helper()
	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	return &fakeModelStore{desc: &spi.ModelDescriptor{Ref: ref, Schema: raw}}
}

// syntheticIterator produces n synthetic NEW-state entities, sleeping
// perItem before each one so a scan can be stretched across several
// heartbeat ticks.
type syntheticIterator struct {
	n, i    int
	perItem time.Duration
	cur     *spi.Entity
}

func (it *syntheticIterator) Next() bool {
	if it.i >= it.n {
		return false
	}
	time.Sleep(it.perItem)
	it.cur = &spi.Entity{Meta: spi.EntityMeta{ID: fmt.Sprintf("e%05d", it.i), State: "NEW"}}
	it.i++
	return true
}
func (it *syntheticIterator) Entity() *spi.Entity { return it.cur }
func (it *syntheticIterator) Err() error          { return nil }
func (it *syntheticIterator) Close() error        { return nil }

// fakeEntityStore's Iterate ignores the filter — there is no real backing
// store to filter against — and always yields the same n synthetic entities.
type fakeEntityStore struct {
	spi.EntityStore
	n       int
	perItem time.Duration
}

func (s *fakeEntityStore) Iterate(context.Context, spi.ModelRef, spi.Filter, spi.IterateOptions) (spi.Iterator, error) {
	return &syntheticIterator{n: s.n, perItem: s.perItem}, nil
}

type fakeFactory struct {
	spi.StoreFactory
	modelStore  spi.ModelStore
	entityStore spi.EntityStore
}

func (f *fakeFactory) ModelStore(context.Context) (spi.ModelStore, error) { return f.modelStore, nil }
func (f *fakeFactory) EntityStore(context.Context) (spi.EntityStore, error) {
	return f.entityStore, nil
}

// fakeAsyncStore is a minimal, single-process spi.AsyncSearchStore: enough
// fidelity (epoch fencing, terminal-status bookkeeping) to drive SubmitAsync
// end to end and observe its outcome, without any storage engine behind it.
type fakeAsyncStore struct {
	mu      sync.Mutex
	jobs    map[string]*spi.SearchJob
	results map[string][]string
}

func newFakeAsyncStore() *fakeAsyncStore {
	return &fakeAsyncStore{jobs: map[string]*spi.SearchJob{}, results: map[string][]string{}}
}

func notFound(jobID string) error {
	return fmt.Errorf("search job %q not found: %w", jobID, spi.ErrNotFound)
}

func (s *fakeAsyncStore) CreateJob(_ context.Context, job *spi.SearchJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *job
	cp.Epoch = 1
	s.jobs[job.ID] = &cp
	return nil
}

func (s *fakeAsyncStore) GetJob(_ context.Context, jobID string) (*spi.SearchJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return nil, notFound(jobID)
	}
	cp := *j
	return &cp, nil
}

func (s *fakeAsyncStore) UpdateJobStatus(_ context.Context, jobID string, epoch int64, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return notFound(jobID)
	}
	if j.Epoch != epoch {
		return fmt.Errorf("update search job %s: %w", jobID, spi.ErrStaleClaim)
	}
	j.Status = status
	j.ResultCount = resultCount
	j.Error = errMsg
	if !finishTime.IsZero() {
		ft := finishTime
		j.FinishTime = &ft
	}
	j.CalcTimeMs = calcTimeMs
	return nil
}

func (s *fakeAsyncStore) SaveResults(ctx context.Context, jobID string, epoch int64, entityIDs iter.Seq[string]) error {
	s.mu.Lock()
	j, ok := s.jobs[jobID]
	if !ok {
		s.mu.Unlock()
		return notFound(jobID)
	}
	if j.Epoch != epoch {
		s.mu.Unlock()
		return fmt.Errorf("save results for search job %s: %w", jobID, spi.ErrStaleClaim)
	}
	s.mu.Unlock()

	for id := range entityIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		s.results[jobID] = append(s.results[jobID], id)
		s.mu.Unlock()
	}
	return nil
}

func (s *fakeAsyncStore) GetResultIDs(_ context.Context, jobID string, offset, limit int) ([]string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := s.results[jobID]
	total := len(ids)
	if offset >= total {
		return nil, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return ids[offset:end], total, nil
}

func (s *fakeAsyncStore) DeleteJob(_ context.Context, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.jobs, jobID)
	delete(s.results, jobID)
	return nil
}

func (s *fakeAsyncStore) ReapExpired(context.Context, time.Duration) (int, error) { return 0, nil }

func (s *fakeAsyncStore) Cancel(_ context.Context, jobID string, finishTime time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return notFound(jobID)
	}
	if j.Status == "SUCCESSFUL" || j.Status == "FAILED" || j.Status == "CANCELLED" {
		return nil
	}
	j.Status = "CANCELLED"
	ft := finishTime
	j.FinishTime = &ft
	return nil
}

func (s *fakeAsyncStore) Heartbeat(_ context.Context, jobID string, epoch int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return notFound(jobID)
	}
	if j.Epoch != epoch {
		return fmt.Errorf("heartbeat search job %s: %w", jobID, spi.ErrStaleClaim)
	}
	now := time.Now()
	j.HeartbeatTime = &now
	return nil
}

func (s *fakeAsyncStore) ClaimStale(context.Context, time.Duration, int) ([]*spi.SearchJob, error) {
	return nil, nil
}

func (s *fakeAsyncStore) ClearResults(_ context.Context, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.results, jobID)
	return nil
}

func (s *fakeAsyncStore) Release(context.Context, string, int64) error { return nil }

var _ spi.AsyncSearchStore = (*fakeAsyncStore)(nil)

// scriptedHeartbeatStore serves a real (fake) store's Heartbeat except on
// the 1-indexed calls named in busyOnCalls or staleOnCalls, which answer a
// marked-busy or a stale-claim error instead without touching the store.
type scriptedHeartbeatStore struct {
	spi.AsyncSearchStore
	busyOnCalls  map[int]bool
	staleOnCalls map[int]bool

	calls int32
}

func (s *scriptedHeartbeatStore) Heartbeat(ctx context.Context, jobID string, epoch int64) error {
	n := int(atomic.AddInt32(&s.calls, 1))
	if s.busyOnCalls[n] {
		return fmt.Errorf("heartbeat search job %s: %w: row locked", jobID, spi.ErrTaskBusy)
	}
	if s.staleOnCalls[n] {
		return fmt.Errorf("heartbeat search job %s: %w", jobID, spi.ErrStaleClaim)
	}
	return s.AsyncSearchStore.Heartbeat(ctx, jobID, epoch)
}

func (s *scriptedHeartbeatStore) callCount() int { return int(atomic.LoadInt32(&s.calls)) }

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

func newSlowSearchService(t *testing.T, store spi.AsyncSearchStore, n int, perItem, heartbeatEvery time.Duration) (*search.SearchService, spi.ModelRef) {
	t.Helper()
	ref := spi.ModelRef{EntityName: "slowitem", ModelVersion: "1"}
	factory := &fakeFactory{
		modelStore:  newFakeModelStore(t, ref),
		entityStore: &fakeEntityStore{n: n, perItem: perItem},
	}
	pool := search.NewWorkerPool(2, 8)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store).
		WithAsyncPool(pool).
		WithHeartbeat(heartbeatEvery)
	return svc, ref
}

func pollUntilTerminal(t *testing.T, svc *search.SearchService, ctx context.Context, jobID string, timeout time.Duration) search.SearchJobStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var status search.SearchJobStatus
	for time.Now().Before(deadline) {
		var err error
		status, err = svc.GetAsyncStatus(ctx, jobID)
		if err != nil {
			t.Fatalf("GetAsyncStatus: %v", err)
		}
		if status.Status != "RUNNING" {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not leave RUNNING within %s (last status %q)", jobID, timeout, status.Status)
	return status
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// A busy tick must not cancel a healthy job: the ticker treats it as a
// missed heartbeat and keeps trying on the next tick, and the scan runs to
// completion.
func TestStartHeartbeat_BusyTickDoesNotCancelTheJob(t *testing.T) {
	ctx := tenantCtx("tenant-heartbeat-busy")
	store := &scriptedHeartbeatStore{
		AsyncSearchStore: newFakeAsyncStore(),
		busyOnCalls:      map[int]bool{1: true, 2: true},
	}
	svc, ref := newSlowSearchService(t, store, 30, 15*time.Millisecond, 20*time.Millisecond)

	jobID, err := svc.SubmitAsync(ctx, ref, matchEverything, search.SearchOptions{Limit: 30})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second)

	if status.Status != "SUCCESSFUL" {
		t.Fatalf("status = %q, want SUCCESSFUL — a busy heartbeat tick aborted a healthy job", status.Status)
	}
	if got := store.callCount(); got < 3 {
		t.Fatalf("Heartbeat was called %d times, want at least 3 — the ticker must keep retrying past the busy ticks, not stop after them", got)
	}
}

// A stale-claim answer is a genuine fencing refusal, not a missed tick, and
// must still abort the job exactly as before this fix.
func TestStartHeartbeat_StaleClaimStillAbortsTheJob(t *testing.T) {
	ctx := tenantCtx("tenant-heartbeat-stale")
	store := &scriptedHeartbeatStore{
		AsyncSearchStore: newFakeAsyncStore(),
		staleOnCalls:     map[int]bool{1: true},
	}
	svc, ref := newSlowSearchService(t, store, 30, 15*time.Millisecond, 20*time.Millisecond)

	jobID, err := svc.SubmitAsync(ctx, ref, matchEverything, search.SearchOptions{Limit: 30})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second)

	if status.Status != "FAILED" {
		t.Fatalf("status = %q, want FAILED — a stale-claim heartbeat answer must still abort the job", status.Status)
	}
}

// Sanity: errors.Is must see through the scripted busy wrapper's %w chain,
// the same way startHeartbeat's own check will.
func TestScriptedHeartbeatStore_BusyErrorIsMarked(t *testing.T) {
	store := &scriptedHeartbeatStore{AsyncSearchStore: newFakeAsyncStore(), busyOnCalls: map[int]bool{1: true}}
	err := store.Heartbeat(context.Background(), "job-x", 1)
	if !errors.Is(err, spi.ErrTaskBusy) {
		t.Fatalf("got %v, want an error marked spi.ErrTaskBusy", err)
	}
}
