package consistency

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// fakeTM stands in for the store. The value a call returns is fixed at call
// entry (its ordinal), not at release, so tests do not depend on release order.
type fakeTM struct {
	spi.TransactionManager
	calls    atomic.Int64
	inflight atomic.Int64
	maxIn    atomic.Int64
	gate     chan struct{} // nil: return at once
	entered  chan int64    // receives each call's ordinal on entry; buffered by the test
	next     func(entry int64) (time.Time, error)
}

func (f *fakeTM) ConsistencyTime(ctx context.Context) (time.Time, error) {
	entry := f.calls.Add(1)
	n := f.inflight.Add(1)
	defer f.inflight.Add(-1)
	for {
		m := f.maxIn.Load()
		if n <= m || f.maxIn.CompareAndSwap(m, n) {
			break
		}
	}
	if f.entered != nil {
		f.entered <- entry
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		}
	}
	return f.next(entry)
}

func tctx(tenant string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u", Tenant: spi.Tenant{ID: spi.TenantID(tenant), Name: tenant},
	})
}

func fixed(c time.Time) func(int64) (time.Time, error) {
	return func(int64) (time.Time, error) { return c, nil }
}

func awaitEntry(t *testing.T, ch chan int64, want int64) {
	t.Helper()
	select {
	case got := <-ch:
		require.Equal(t, want, got)
	case <-time.After(10 * time.Second):
		t.Fatalf("store call %d never entered", want)
	}
}

func awaitAnyEntry(t *testing.T, ch chan int64) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("no store call entered")
	}
}

func TestFence_PassesBelowCachedHighWithoutAStoreCall(t *testing.T) {
	c := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	tm := &fakeTM{next: fixed(c)}
	s := New(tm)
	_, err := s.Fresh(tctx("a"))
	require.NoError(t, err)
	require.NoError(t, s.Fence(tctx("a"), c.Add(-time.Hour)))
	require.Equal(t, int64(1), tm.calls.Load())
}

func TestFence_RefusesLaterInstantWithC(t *testing.T) {
	c := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s := New(&fakeTM{next: fixed(c)})
	err := s.Fence(tctx("a"), c.Add(time.Millisecond))
	var appErr *common.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, http.StatusBadRequest, appErr.Status)
	require.Equal(t, common.ErrCodePointInTimeAfterConsistencyTime, appErr.Code)
	require.Equal(t, c.Format(time.RFC3339Nano), appErr.Props["consistencyTime"])
	require.False(t, appErr.Retryable)
}

// An instant with a non-UTC offset equal to C is served.
func TestFence_ComparesInstantsNotStrings(t *testing.T) {
	c := time.Date(2026, 10, 5, 14, 3, 7, 123456000, time.UTC)
	s := New(&fakeTM{next: fixed(c)})
	same := c.In(time.FixedZone("+02", 2*3600))
	require.NoError(t, s.Fence(tctx("a"), same))
}

// An ancient instant on a fresh node passes.
func TestFence_AncientInstantPasses(t *testing.T) {
	s := New(&fakeTM{next: fixed(time.Now())})
	require.NoError(t, s.Fence(tctx("a"), time.Time{}.Add(time.Nanosecond)))
	require.NoError(t, s.Fence(tctx("a"), time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)))
}

// Tenants are keyed exactly.
func TestFence_TenantKeysAreExact(t *testing.T) {
	c := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	tm := &fakeTM{next: fixed(c)}
	s := New(tm)
	_, err := s.Fresh(tctx("Acme"))
	require.NoError(t, err)
	require.NoError(t, s.Fence(tctx("acme"), c.Add(-time.Hour)))
	require.Equal(t, int64(2), tm.calls.Load(), "acme must not use Acme's cached C")
}

// A burst of misses holds at most two calls in flight.
func TestFence_BurstHoldsAtMostTwoCalls(t *testing.T) {
	gate := make(chan struct{})
	c := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	tm := &fakeTM{gate: gate, entered: make(chan int64, 1000), next: fixed(c)}
	s := New(tm)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, _ = s.Fresh(tctx("a"))
			} else {
				_ = s.Fence(tctx("a"), c.Add(-time.Second))
			}
		}(i)
	}
	awaitAnyEntry(t, tm.entered)
	// Give the rest of the burst time to try to open more calls; the bound is
	// asserted on what the store saw, so a short settle only widens the check.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	require.LessOrEqual(t, tm.maxIn.Load(), int64(2))
}

func TestFresh_DoesNotJoinACallStartedBeforeIt(t *testing.T) {
	// Call 1 is held; Fresh starts call 2 and returns call 2's result.
	gate := make(chan struct{})
	tm := &fakeTM{gate: gate, entered: make(chan int64, 8), next: func(entry int64) (time.Time, error) {
		return time.Unix(entry, 0), nil
	}}
	s := New(tm)
	go func() { _, _ = s.Fresh(tctx("a")) }()
	awaitEntry(t, tm.entered, 1)
	got := make(chan time.Time, 1)
	go func() { c, _ := s.Fresh(tctx("a")); got <- c }()
	awaitEntry(t, tm.entered, 2)
	close(gate)
	require.Equal(t, int64(2), (<-got).Unix(), "Fresh must use a call that started after it")
}

func TestFresh_CallerCancelDoesNotFailSharers(t *testing.T) {
	gate := make(chan struct{})
	c := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	tm := &fakeTM{gate: gate, entered: make(chan int64, 8), next: fixed(c)}
	s := New(tm)
	cctx, cancel := context.WithCancel(tctx("a"))
	errA := make(chan error, 1)
	go func() { _, err := s.Fresh(cctx); errA <- err }()
	awaitEntry(t, tm.entered, 1)
	resB := make(chan error, 1)
	go func() { resB <- s.Fence(tctx("a"), c.Add(-time.Second)) }()
	cancel()
	require.ErrorIs(t, <-errA, context.Canceled)
	close(gate)
	require.NoError(t, <-resB)
}

// A waiter whose own context ends returns its own error while the shared call
// carries on for the others.
func TestFence_WaiterHonoursItsOwnContext(t *testing.T) {
	gate := make(chan struct{})
	c := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	tm := &fakeTM{gate: gate, entered: make(chan int64, 8), next: fixed(c)}
	s := New(tm)
	go func() { _, _ = s.Fresh(tctx("a")) }()
	awaitEntry(t, tm.entered, 1)
	wctx, cancel := context.WithCancel(tctx("a"))
	res := make(chan error, 1)
	go func() { res <- s.Fence(wctx, c.Add(-time.Second)) }()
	cancel()
	require.ErrorIs(t, <-res, context.Canceled)
	close(gate)
}

// Every sharer of a call receives that call's error.
func TestFence_JoinerGetsTheJoinedCallsError(t *testing.T) {
	gate := make(chan struct{})
	tm := &fakeTM{gate: gate, entered: make(chan int64, 8), next: func(int64) (time.Time, error) {
		return time.Time{}, spi.ErrConsistencyTimeUnavailable
	}}
	s := New(tm)
	first := make(chan error, 1)
	go func() { _, err := s.Fresh(tctx("a")); first <- err }()
	awaitEntry(t, tm.entered, 1)
	second := make(chan error, 1)
	go func() { second <- s.Fence(tctx("a"), time.Now()) }()
	close(gate)
	for _, ch := range []chan error{first, second} {
		var appErr *common.AppError
		require.ErrorAs(t, <-ch, &appErr)
		require.Equal(t, common.ErrCodeConsistencyTimeUnavailable, appErr.Code)
	}
}

// The cached high only grows: a later call returning a lower C does not lower it.
func TestFence_HighOnlyGrows(t *testing.T) {
	hi := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	lo := hi.Add(-time.Hour)
	tm := &fakeTM{next: func(entry int64) (time.Time, error) {
		if entry == 1 {
			return hi, nil
		}
		return lo, nil
	}}
	s := New(tm)
	c1, err := s.Fresh(tctx("a"))
	require.NoError(t, err)
	require.True(t, c1.Equal(hi))
	_, err = s.Fresh(tctx("a"))
	require.NoError(t, err)
	require.NoError(t, s.Fence(tctx("a"), hi))
	require.Equal(t, int64(2), tm.calls.Load(), "hi must still cover hi without another call")
}

// Heavy mixed use completes: no deadlock, no starvation.
func TestService_ManyCallersAllComplete(t *testing.T) {
	c := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	gate := make(chan struct{})
	tm := &fakeTM{gate: gate, entered: make(chan int64, 1000), next: fixed(c)}
	s := New(tm)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(tctx(fmt.Sprintf("t%d", i%3)), 20*time.Second)
			defer cancel()
			if i%2 == 0 {
				_, err := s.Fresh(ctx)
				require.NoError(t, err)
			} else {
				require.NoError(t, s.Fence(ctx, c.Add(-time.Second)))
			}
		}(i)
	}
	awaitAnyEntry(t, tm.entered)
	close(gate)
	wg.Wait()
}

func TestErrors_UnavailableIsRetryable503(t *testing.T) {
	s := New(&fakeTM{next: func(int64) (time.Time, error) {
		return time.Time{}, fmt.Errorf("x: %w", spi.ErrConsistencyTimeUnavailable)
	}})
	_, err := s.Fresh(tctx("a"))
	var appErr *common.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, http.StatusServiceUnavailable, appErr.Status)
	require.Equal(t, common.ErrCodeConsistencyTimeUnavailable, appErr.Code)
	require.True(t, appErr.Retryable)
}

type suErr struct{}

func (suErr) Error() string            { return "down" }
func (suErr) StorageUnavailable() bool { return true }

func TestErrors_StorageUnavailableMarker(t *testing.T) {
	s := New(&fakeTM{next: func(int64) (time.Time, error) { return time.Time{}, suErr{} }})
	_, err := s.Fresh(tctx("a"))
	var appErr *common.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, common.ErrCodeStorageUnavailable, appErr.Code)
}

func TestErrors_OtherIsInternal(t *testing.T) {
	s := New(&fakeTM{next: func(int64) (time.Time, error) { return time.Time{}, errors.New("boom") }})
	_, err := s.Fresh(tctx("a"))
	var appErr *common.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, http.StatusInternalServerError, appErr.Status)
}

func TestNew_NilTransactionManagerPanics(t *testing.T) {
	require.Panics(t, func() { New(nil) })
}
