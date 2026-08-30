package search_test

// Tests for the streaming async executor (task E2): SubmitAsync's Iterate ->
// SaveResults pipeline, the heartbeat ticker, the in-process cancel
// registry, and the untranslatable-condition fallback. Each test below maps
// to one of the E2.1 scenarios (a)-(f) in
// .superpowers/sdd/2026-08-22-472-search-spi-surface/task-E2-brief.md, plus
// (g) SubmitAsync's own ErrQueueFull cleanup branch (fix-round-1 gap).

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
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// mustOccupyPool submits job to pool, retrying past the narrow startup race
// between NewWorkerPool returning and its worker goroutine actually
// reaching its channel receive (a fresh pool's first Submit can spuriously
// see ErrQueueFull for a few microseconds — see pool_test.go's
// mustSubmitEventually, unavailable here since it's unexported in a
// different test package).
func mustOccupyPool(t *testing.T, pool *search.WorkerPool, job func()) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		err := pool.Submit(job)
		if err == nil {
			return
		}
		if !errors.Is(err, search.ErrQueueFull) || time.Now().After(deadline) {
			t.Fatalf("occupy pool: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

// newTinyPool builds a small bounded pool for executor tests and drains it
// at test cleanup so no worker goroutines outlive the test.
func newTinyPool(t *testing.T) *search.WorkerPool {
	t.Helper()
	p := search.NewWorkerPool(2, 8)
	t.Cleanup(func() { p.Drain(context.Background()) })
	return p
}

// pollUntilTerminal polls GetAsyncStatus until the job leaves RUNNING (or
// the deadline elapses) and returns the last observed status.
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

// countingIterator wraps a real spi.Iterator, counting Next() calls via a
// shared pointer so a test can observe how far the producer has advanced
// relative to what the consumer (SaveResults) has pulled from it so far —
// the interleave assertion for (b).
type countingIterator struct {
	spi.Iterator
	calls *int64
}

func (c *countingIterator) Next() bool {
	ok := c.Iterator.Next()
	if ok {
		atomic.AddInt64(c.calls, 1)
	}
	return ok
}

// delayIterator wraps a real spi.Iterator, sleeping delay before every
// Next() call (simulating a slow scan for the cancel-mid-flight and
// heartbeat-while-scanning scenarios) and recording whether Close was ever
// called.
type delayIterator struct {
	spi.Iterator
	delay  time.Duration
	closed atomic.Bool
}

func (d *delayIterator) Next() bool {
	time.Sleep(d.delay)
	return d.Iterator.Next()
}

func (d *delayIterator) Close() error {
	d.closed.Store(true)
	return d.Iterator.Close()
}

// wrapIterate builds an iterableEntityStore (defined in service_test.go)
// around base's real memory Iterate, running each returned Iterator through
// decorate before handing it to the caller. Used to inject the counting/
// delay instrumentation above without touching production code.
func wrapIterate(t *testing.T, base *memory.StoreFactory, ctx context.Context, decorate func(spi.Iterator) spi.Iterator) *iterableEntityStore {
	t.Helper()
	realStore, err := base.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	iterableReal, ok := realStore.(spi.Iterable)
	if !ok {
		t.Fatal("precondition: memory EntityStore must implement spi.Iterable")
	}
	return &iterableEntityStore{
		EntityStore: realStore,
		iterateFn: func(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
			it, err := iterableReal.Iterate(ctx, model, filter, opts)
			if err != nil {
				return nil, err
			}
			return decorate(it), nil
		},
	}
}

// seedNumberedEntities saves n entities named "e00000".."e0000<n-1>" (fixed
// width so byte-wise ID order is the numeric order) each carrying an
// integer field "val" set to n-1-i — the REVERSE of ID order — so a test
// can distinguish "default entity-ID order" from "order by val" results.
func seedNumberedEntities(t *testing.T, ctx context.Context, factory *memory.StoreFactory, ref spi.ModelRef, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("e%05d", i)
		val := n - 1 - i
		saveEntity(t, ctx, factory, ref, id, []byte(fmt.Sprintf(`{"val":%d}`, val)))
	}
}

// (a) results order = requested order incl. default entity-ID order.
func TestExecutor_ResultOrder_DefaultAndExplicit(t *testing.T) {
	const n = 50
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "orderitem", ModelVersion: "1"}
	saveModelWithFields(t, ctx, base, ref, map[string]schema.DataType{"val": schema.Integer})
	seedNumberedEntities(t, ctx, base, ref, n)

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	pool := newTinyPool(t)
	svc := search.NewSearchService(base, uuids, searchStore).
		WithAsyncPool(pool).
		WithHeartbeat(50 * time.Millisecond)

	cond := &predicate.SimpleCondition{JsonPath: "$.val", OperatorType: "GREATER_THAN", Value: float64(-1)}

	t.Run("default order is entity-ID ascending", func(t *testing.T) {
		jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
		if err != nil {
			t.Fatalf("SubmitAsync: %v", err)
		}
		status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second)
		if status.Status != "SUCCESSFUL" {
			t.Fatalf("status = %q, want SUCCESSFUL", status.Status)
		}
		ids, total, err := searchStore.GetResultIDs(ctx, jobID, 0, n)
		if err != nil {
			t.Fatalf("GetResultIDs: %v", err)
		}
		if total != n {
			t.Fatalf("total = %d, want %d", total, n)
		}
		for i, id := range ids {
			want := fmt.Sprintf("e%05d", i)
			if id != want {
				t.Fatalf("ids[%d] = %q, want %q (default order must be entity-ID ascending)", i, id, want)
			}
		}
	})

	t.Run("explicit sort by val ascending reverses ID order", func(t *testing.T) {
		jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{
			OrderBy: []search.OrderKey{{Path: "val", Source: spi.SourceData}},
		})
		if err != nil {
			t.Fatalf("SubmitAsync: %v", err)
		}
		status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second)
		if status.Status != "SUCCESSFUL" {
			t.Fatalf("status = %q, want SUCCESSFUL", status.Status)
		}
		ids, _, err := searchStore.GetResultIDs(ctx, jobID, 0, n)
		if err != nil {
			t.Fatalf("GetResultIDs: %v", err)
		}
		// val = n-1-i, so ascending-by-val is descending-by-i: the LAST
		// seeded entity (e00049, val=0) comes first.
		for i, id := range ids {
			want := fmt.Sprintf("e%05d", n-1-i)
			if id != want {
				t.Fatalf("ids[%d] = %q, want %q (order-by-val must reverse the ID order given this fixture)", i, id, want)
			}
		}
	})
}

// (b) incremental streaming: the engine must never hand SaveResults a
// materialized slice. A fake AsyncSearchStore wrapper consumes the
// iter.Seq[string] itself, sleeping 1ms every 1000 ids pulled, and — via a
// counting decorator on the underlying Iterator — asserts the producer has
// not already run to completion by the first such checkpoint. If the engine
// materialized `[]string` up front (e.g. `slices.Values(ids)` fed from a
// fully-drained slice), the counting iterator's call count would already
// equal the total before SaveResults pulled a single id.
func TestExecutor_StreamsIncrementally(t *testing.T) {
	const total = 10000
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "streamitem", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	for i := 0; i < total; i++ {
		saveEntity(t, ctx, base, ref, fmt.Sprintf("e%06d", i), []byte(`{}`))
	}

	var nextCalls int64
	ies := wrapIterate(t, base, ctx, func(it spi.Iterator) spi.Iterator {
		return &countingIterator{Iterator: it, calls: &nextCalls}
	})
	factory := &iterableFactory{StoreFactory: base, entityStore: ies}

	realAsync, _ := base.AsyncSearchStore(context.Background())
	observer := &streamObserverStore{AsyncSearchStore: realAsync, producerCalls: &nextCalls, total: total}

	uuids := common.NewTestUUIDGenerator()
	pool := newTinyPool(t)
	svc := search.NewSearchService(factory, uuids, observer).
		WithAsyncPool(pool).
		WithHeartbeat(200 * time.Millisecond)

	// Matches every entity: LifecycleCondition against the default state
	// saveEntity stamps ("NEW"), translatable to spi.Filter, so the
	// executor takes the Iterate path (not the untranslatable fallback).
	cond := &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}

	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	status := pollUntilTerminal(t, svc, ctx, jobID, 20*time.Second)
	if status.Status != "SUCCESSFUL" {
		t.Fatalf("status = %q, want SUCCESSFUL", status.Status)
	}
	if status.Total != total {
		t.Fatalf("status.Total = %d, want %d", status.Total, total)
	}
	if !observer.sawIncompleteProducer.Load() {
		t.Fatal("engine appears to have produced the full result set before SaveResults began consuming it — no interleaving observed, suggesting a materialized slice rather than a live pull")
	}
}

// streamObserverStore wraps the real memory AsyncSearchStore. SaveResults
// pulls from entityIDs itself (rather than delegating the range loop),
// sleeping 1ms every 1000 ids and recording — via producerCalls, the shared
// counter the test's countingIterator decorator increments — whether the
// underlying Iterator's Next() had already been called `total` times by
// that first checkpoint. A materialized-slice implementation would already
// show the full count; a live pull shows partial progress.
type streamObserverStore struct {
	spi.AsyncSearchStore
	producerCalls         *int64
	total                 int
	sawIncompleteProducer atomic.Bool
}

func (s *streamObserverStore) SaveResults(ctx context.Context, jobID string, epoch int64, entityIDs iter.Seq[string]) error {
	var buf []string
	consumed := 0
	checkpointed := false
	for id := range entityIDs {
		buf = append(buf, id)
		consumed++
		if consumed%1000 == 0 {
			time.Sleep(time.Millisecond)
			if !checkpointed {
				checkpointed = true
				if atomic.LoadInt64(s.producerCalls) < int64(s.total) {
					s.sawIncompleteProducer.Store(true)
				}
			}
		}
	}
	return s.AsyncSearchStore.SaveResults(ctx, jobID, epoch, func(yield func(string) bool) {
		for _, id := range buf {
			if !yield(id) {
				return
			}
		}
	})
}

// (c) cancel mid-flight: a slow (yield-delayed) store, CancelRunning +
// store Cancel racing an in-progress scan — job ends CANCELLED, the
// iterator is closed, and the terminal write is observed write-once (no
// SUCCESSFUL overwrite).
func TestExecutor_CancelMidFlight(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "cancelitem", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	for i := 0; i < 50; i++ {
		saveEntity(t, ctx, base, ref, fmt.Sprintf("e%03d", i), []byte(`{}`))
	}

	var iter *delayIterator
	ies := wrapIterate(t, base, ctx, func(it spi.Iterator) spi.Iterator {
		iter = &delayIterator{Iterator: it, delay: 30 * time.Millisecond}
		return iter
	})
	factory := &iterableFactory{StoreFactory: base, entityStore: ies}

	searchStore, _ := base.AsyncSearchStore(context.Background())
	uuids := common.NewTestUUIDGenerator()
	pool := newTinyPool(t)
	svc := search.NewSearchService(factory, uuids, searchStore).
		WithAsyncPool(pool).
		WithHeartbeat(20 * time.Millisecond)

	cond := &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}
	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}

	// Let the scan get underway (a handful of 30ms-delayed rows) before
	// cancelling.
	time.Sleep(100 * time.Millisecond)

	svc.CancelRunning(jobID)
	if err := searchStore.Cancel(ctx, jobID, time.Now()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	status := pollUntilTerminal(t, svc, ctx, jobID, 10*time.Second)
	if status.Status != "CANCELLED" {
		t.Fatalf("status = %q, want CANCELLED", status.Status)
	}

	// Give the executor's own (now-fenced-by-terminal-status) write attempt
	// a moment to lose the terminal-write race, then confirm it never won.
	time.Sleep(200 * time.Millisecond)
	final, err := svc.GetAsyncStatus(ctx, jobID)
	if err != nil {
		t.Fatalf("GetAsyncStatus: %v", err)
	}
	if final.Status != "CANCELLED" {
		t.Fatalf("final status = %q, want CANCELLED (no SUCCESSFUL overwrite of a terminal write)", final.Status)
	}

	if iter == nil {
		t.Fatal("iterator was never constructed")
	}
	if !iter.closed.Load() {
		t.Error("iterator was never closed")
	}
}

// (d) heartbeat recorded while queued and while scanning.
func TestExecutor_HeartbeatRecordedWhileQueuedAndScanning(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "hbitem", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	for i := 0; i < 20; i++ {
		saveEntity(t, ctx, base, ref, fmt.Sprintf("e%03d", i), []byte(`{}`))
	}

	ies := wrapIterate(t, base, ctx, func(it spi.Iterator) spi.Iterator {
		return &delayIterator{Iterator: it, delay: 25 * time.Millisecond}
	})
	factory := &iterableFactory{StoreFactory: base, entityStore: ies}

	searchStore, _ := base.AsyncSearchStore(context.Background())
	uuids := common.NewTestUUIDGenerator()

	// A single-worker pool occupied by a dummy blocking job keeps the
	// submitted job QUEUED (not yet picked up by a worker) until the test
	// releases it, so the "while queued" half of the assertion is genuine.
	pool := search.NewWorkerPool(1, 4)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	release := make(chan struct{})
	started := make(chan struct{})
	if err := pool.Submit(func() {
		close(started)
		<-release
	}); err != nil {
		t.Fatalf("occupy pool: %v", err)
	}
	<-started

	const heartbeatInterval = 20 * time.Millisecond
	svc := search.NewSearchService(factory, uuids, searchStore).
		WithAsyncPool(pool).
		WithHeartbeat(heartbeatInterval)

	cond := &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}
	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}

	// While queued: wait a few heartbeat intervals, then confirm
	// HeartbeatTime has been stamped even though the job has not started
	// scanning (the worker is still blocked on release).
	time.Sleep(6 * heartbeatInterval)
	queuedJob, err := searchStore.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob (queued): %v", err)
	}
	if queuedJob.Status != "RUNNING" {
		t.Fatalf("queued job status = %q, want RUNNING", queuedJob.Status)
	}
	if queuedJob.HeartbeatTime == nil {
		t.Fatal("HeartbeatTime is nil while queued — heartbeat must start at submit time, not pool pickup")
	}
	firstHeartbeat := *queuedJob.HeartbeatTime

	// Release the worker so the job starts scanning (each row delayed
	// 25ms), and confirm HeartbeatTime keeps advancing.
	close(release)
	time.Sleep(6 * heartbeatInterval)
	scanningJob, err := searchStore.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob (scanning): %v", err)
	}
	if !scanningJob.HeartbeatTime.After(firstHeartbeat) {
		t.Fatalf("HeartbeatTime did not advance while scanning: first=%s, later=%s", firstHeartbeat, scanningJob.HeartbeatTime)
	}

	status := pollUntilTerminal(t, svc, ctx, jobID, 10*time.Second)
	if status.Status != "SUCCESSFUL" {
		t.Fatalf("status = %q, want SUCCESSFUL", status.Status)
	}
}

// (e) heartbeat error aborts: fencing the job (bumping its epoch directly
// in the store, simulating another node's takeover claim) makes the
// executor abort without corrupting results — its own writes are rejected
// under the stale epoch rather than silently succeeding against the wrong
// claim.
func TestExecutor_HeartbeatFencingAborts(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "fenceitem", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	for i := 0; i < 20; i++ {
		saveEntity(t, ctx, base, ref, fmt.Sprintf("e%03d", i), []byte(`{}`))
	}

	ies := wrapIterate(t, base, ctx, func(it spi.Iterator) spi.Iterator {
		return &delayIterator{Iterator: it, delay: 25 * time.Millisecond}
	})
	factory := &iterableFactory{StoreFactory: base, entityStore: ies}

	searchStore, _ := base.AsyncSearchStore(context.Background())
	uuids := common.NewTestUUIDGenerator()
	pool := newTinyPool(t)
	const heartbeatInterval = 15 * time.Millisecond
	svc := search.NewSearchService(factory, uuids, searchStore).
		WithAsyncPool(pool).
		WithHeartbeat(heartbeatInterval)

	cond := &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}
	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}

	// Let the scan get underway, then fence the job out from under the
	// executor: ClaimStale with staleAfter=0 claims any RUNNING job
	// immediately, bumping its epoch — the same mechanism a real takeover
	// would use (interim disposition: nothing re-executes the claimed job
	// in this test, it is just claimed-and-abandoned to prove the original
	// executor backs off cleanly).
	time.Sleep(80 * time.Millisecond)
	claimed, err := searchStore.ClaimStale(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("ClaimStale: %v", err)
	}
	var claimedThisJob bool
	for _, j := range claimed {
		if j.ID == jobID {
			claimedThisJob = true
			if j.Epoch < 2 {
				t.Fatalf("claimed job epoch = %d, want >= 2", j.Epoch)
			}
		}
	}
	if !claimedThisJob {
		t.Fatalf("ClaimStale did not claim job %s — it must still have been RUNNING under epoch 1 when this ran", jobID)
	}

	// Give the original executor's heartbeat/terminal-write attempts time
	// to observe the fencing and back off.
	time.Sleep(500 * time.Millisecond)

	final, err := searchStore.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if final.Epoch < 2 {
		t.Fatalf("final epoch = %d, want >= 2 (the claim must stick — the original executor's epoch-1 writes must be rejected, not overwrite it)", final.Epoch)
	}
	if final.Status == "SUCCESSFUL" {
		t.Fatal("job reached SUCCESSFUL under the original (fenced-out) executor — its writes should have been rejected once ClaimStale bumped the epoch")
	}
}

// (f) an OR condition (one always-true member, one on a wildcard array path)
// still completes and saves its results correctly, whichever route it takes.
// This used to name and pin a translate-FAILURE route specifically — the
// wildcard-array-path member was untranslatable, forcing the GetAll
// fallback. It no longer is (spi.ConditionToFilter pushes a wildcard array
// path down like any other; see matchAllFixtureCondition's doc comment), so
// this condition now clears translation and the pushdown path executes it
// instead. The assertions below (job SUCCESSFUL, Total==5, 5 result IDs)
// never depended on which route ran — both must produce the same
// correct answer — so this test still holds, just no longer as a fallback
// pin specifically; TestSearch_FallbackBranchIsBounded_TranslateFailureRoute
// is now the one that isolates a genuine translate failure.
func TestExecutor_OrConditionCompletesAndSaves(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "fallbackitem", ModelVersion: "1"}
	saveModelWithValAndItemsArray(t, ctx, base, ref)
	for i := 0; i < 5; i++ {
		saveEntity(t, ctx, base, ref, fmt.Sprintf("e%d", i), []byte(fmt.Sprintf(`{"val":%d}`, i)))
	}

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	pool := newTinyPool(t)
	svc := search.NewSearchService(base, uuids, searchStore).
		WithAsyncPool(pool).
		WithHeartbeat(50 * time.Millisecond)

	jobID, err := svc.SubmitAsync(ctx, ref, matchAllFixtureCondition(t), search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second)
	if status.Status != "SUCCESSFUL" {
		t.Fatalf("status = %q, want SUCCESSFUL", status.Status)
	}
	if status.Total != 5 {
		t.Fatalf("status.Total = %d, want 5", status.Total)
	}
	ids, total, err := searchStore.GetResultIDs(ctx, jobID, 0, 10)
	if err != nil {
		t.Fatalf("GetResultIDs: %v", err)
	}
	if total != 5 || len(ids) != 5 {
		t.Fatalf("got %d/%d results, want 5/5", len(ids), total)
	}
}

// deleteObserverStore wraps the real memory AsyncSearchStore, recording the
// ID CreateJob assigned and every ID DeleteJob is called with — used by (g)
// to identify the job SubmitAsync created and then tore down, without
// depending on the deterministic TestUUIDGenerator sequence (SubmitAsync
// returns "" on rejection, not the generated ID).
type deleteObserverStore struct {
	spi.AsyncSearchStore

	mu         sync.Mutex
	createdID  string
	deletedIDs []string
}

func (d *deleteObserverStore) CreateJob(ctx context.Context, job *spi.SearchJob) error {
	err := d.AsyncSearchStore.CreateJob(ctx, job)
	if err == nil {
		func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.createdID = job.ID
		}()
	}
	return err
}

func (d *deleteObserverStore) DeleteJob(ctx context.Context, jobID string) error {
	func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.deletedIDs = append(d.deletedIDs, jobID)
	}()
	return d.AsyncSearchStore.DeleteJob(ctx, jobID)
}

// snapshot returns a copy of the observed state under lock, for tests to
// read without racing CreateJob/DeleteJob.
func (d *deleteObserverStore) snapshot() (createdID string, deletedIDs []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.createdID, append([]string(nil), d.deletedIDs...)
}

// (g) SubmitAsync's own ErrQueueFull cleanup branch (service.go's
// `if submitErr != nil { cancel(); s.deregisterJob(jobID); s.searchStore.DeleteJob(...) }`):
// a job that never entered the queue must not linger — no fenced RUNNING/FAILED
// row, no cancel-registry entry, and ErrQueueFull propagated to the caller.
func TestExecutor_SubmitAsync_QueueFullCleansUp(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "queuefullitem", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)

	realAsync, _ := base.AsyncSearchStore(context.Background())
	observer := &deleteObserverStore{AsyncSearchStore: realAsync}

	// A single-worker, zero-queue pool occupied by a blocking dummy job:
	// the very next Submit call (SubmitAsync's own) has nowhere to land and
	// returns ErrQueueFull immediately.
	pool := search.NewWorkerPool(1, 0)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	release := make(chan struct{})
	started := make(chan struct{})
	mustOccupyPool(t, pool, func() {
		close(started)
		<-release
	})
	<-started
	t.Cleanup(func() { close(release) })

	uuids := common.NewTestUUIDGenerator()
	svc := search.NewSearchService(base, uuids, observer).
		WithAsyncPool(pool).
		WithHeartbeat(20 * time.Millisecond)

	cond := &predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "NEW"}
	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if !errors.Is(err, search.ErrQueueFull) {
		t.Fatalf("SubmitAsync error = %v, want ErrQueueFull", err)
	}
	if jobID != "" {
		t.Fatalf("SubmitAsync jobID = %q, want empty on queue-full rejection", jobID)
	}

	createdID, deletedIDs := observer.snapshot()

	if createdID == "" {
		t.Fatal("CreateJob was never called — precondition for this test is that a row was created before the queue-full rejection")
	}
	if len(deletedIDs) != 1 || deletedIDs[0] != createdID {
		t.Fatalf("DeleteJob calls = %v, want exactly one call with jobID %q", deletedIDs, createdID)
	}

	// The row is gone — not a fenced FAILED write left behind.
	if _, getErr := realAsync.GetJob(ctx, createdID); !errors.Is(getErr, spi.ErrNotFound) {
		t.Fatalf("GetJob(%s) error = %v, want spi.ErrNotFound (the job row must be deleted, not left RUNNING/FAILED)", createdID, getErr)
	}

	// The cancel-registry entry is gone too: CancelRunning must report
	// nothing to cancel.
	if svc.CancelRunning(createdID) {
		t.Fatalf("CancelRunning(%s) = true, want false (the registry entry must be removed on queue-full rejection)", createdID)
	}
}
