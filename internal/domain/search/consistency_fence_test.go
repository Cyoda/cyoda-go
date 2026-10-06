package search_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/consistency"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// chosenTM answers the consistency time (or error) the test chose, never a
// clock reading of its own.
type chosenTM struct {
	spi.TransactionManager
	at  time.Time
	err error
}

func (c chosenTM) ConsistencyTime(context.Context) (time.Time, error) { return c.at, c.err }

type fenceSearchFixture struct {
	base  *memory.StoreFactory
	store spi.AsyncSearchStore
	svc   *search.SearchService
	ctx   context.Context
	ref   spi.ModelRef
	c     time.Time
}

func newFenceSearchFixture(t *testing.T, tenant string, cTimeErr error) *fenceSearchFixture {
	t.Helper()
	base := memory.NewStoreFactory()
	t.Cleanup(func() { base.Close() })
	ctx := tenantCtx(tenant)
	ref := spi.ModelRef{EntityName: "fenceitem", ModelVersion: "1"}
	saveMinimalModel(t, ctx, base, ref)
	realTM, err := base.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	c := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	cons := consistency.New(chosenTM{TransactionManager: realTM, at: c, err: cTimeErr})
	store, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	svc := search.NewSearchService(base, common.NewTestUUIDGenerator(), store, cons)
	return &fenceSearchFixture{base: base, store: store, svc: svc, ctx: ctx, ref: ref, c: c}
}

func (f *fenceSearchFixture) after() *time.Time { t := f.c.Add(time.Millisecond); return &t }

func wantFenceErr(t *testing.T, err error, status int, code string) {
	t.Helper()
	var ae *common.AppError
	if !errors.As(err, &ae) {
		t.Fatalf("want AppError %d %s, got %v", status, code, err)
	}
	if ae.Status != status || ae.Code != code {
		t.Fatalf("got %d %s (%s), want %d %s", ae.Status, ae.Code, ae.Message, status, code)
	}
}

func TestSearch_FenceRefusesLaterInstant(t *testing.T) {
	f := newFenceSearchFixture(t, "fence-search", nil)
	_, err := f.svc.Search(f.ctx, f.ref, matchEverything, search.SearchOptions{Limit: 10, PointInTime: f.after()})
	wantFenceErr(t, err, http.StatusBadRequest, common.ErrCodePointInTimeAfterConsistencyTime)

	at := f.c
	if _, err := f.svc.Search(f.ctx, f.ref, matchEverything, search.SearchOptions{Limit: 10, PointInTime: &at}); err != nil {
		t.Fatalf("Search at C: %v", err)
	}
}

func TestSubmitAsync_FenceRefusesLaterInstant(t *testing.T) {
	f := newFenceSearchFixture(t, "fence-submit", nil)
	_, err := f.svc.SubmitAsync(f.ctx, f.ref, matchEverything, search.SearchOptions{Limit: 10, PointInTime: f.after()})
	wantFenceErr(t, err, http.StatusBadRequest, common.ErrCodePointInTimeAfterConsistencyTime)
}

func TestSubmitAsync_DefaultInstantComesFromTheStore(t *testing.T) {
	f := newFenceSearchFixture(t, "fence-default", nil)
	jobID, err := f.svc.SubmitAsync(f.ctx, f.ref, matchEverything, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}
	job, err := f.store.GetJob(f.ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !job.PointInTime.Equal(f.c) {
		t.Errorf("job PointInTime = %v, want the store's consistency time %v", job.PointInTime, f.c)
	}
}

func TestSubmitAsync_UnavailableCreatesNoJob(t *testing.T) {
	f := newFenceSearchFixture(t, "fence-unavail", nil)
	counting := &createCountingStore{AsyncSearchStore: f.store}
	realTM, _ := f.base.TransactionManager(f.ctx)
	svc := search.NewSearchService(f.base, common.NewTestUUIDGenerator(), counting,
		consistency.New(chosenTM{TransactionManager: realTM, err: spi.ErrConsistencyTimeUnavailable}))

	for _, pit := range []*time.Time{nil, f.after()} {
		_, err := svc.SubmitAsync(f.ctx, f.ref, matchEverything, search.SearchOptions{Limit: 10, PointInTime: pit})
		wantFenceErr(t, err, http.StatusServiceUnavailable, common.ErrCodeConsistencyTimeUnavailable)
	}
	if n := counting.creates.Load(); n != 0 {
		t.Errorf("CreateJob called %d times; an unavailable consistency time must create no job", n)
	}
}

func TestSubmitAsync_CapPreCheckWinsOverFence(t *testing.T) {
	f := newFenceSearchFixture(t, "fence-cap", nil)
	blocking := &blockingSaveStore{AsyncSearchStore: f.store, release: make(chan struct{})}
	defer close(blocking.release)
	realTM, _ := f.base.TransactionManager(f.ctx)
	pool := search.NewWorkerPool(4, 64)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	svc := search.NewSearchService(f.base, common.NewTestUUIDGenerator(), blocking,
		consistency.New(chosenTM{TransactionManager: realTM, at: f.c})).
		WithAsyncPool(pool).
		WithAsyncMaxPerTenant(1).
		WithHeartbeat(50 * time.Millisecond)

	if _, err := svc.SubmitAsync(f.ctx, f.ref, matchEverything, search.SearchOptions{Limit: 10}); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	_, err := svc.SubmitAsync(f.ctx, f.ref, matchEverything, search.SearchOptions{Limit: 10, PointInTime: f.after()})
	assertQueueFull(t, err)
}
