package modelcache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// fakeInner is a plugin model store with controllable latency and call
// counters. Get blocks while getGate is open (non-nil and not closed) and
// reports every call it starts on getEntered. ExtendSchema runs extFn, which
// receives the call's 1-based index, and tracks how many calls overlap.
type fakeInner struct {
	spi.ModelStore

	mu      sync.Mutex
	desc    *spi.ModelDescriptor
	getGate chan struct{}
	gets    int
	ext     int
	extLive int
	extPeak int
	extFn   func(ctx context.Context, call int) error
	getFn   func(ctx context.Context, call int) error

	getEntered chan struct{}
	extEntered chan struct{}
}

func newFakeInner(desc *spi.ModelDescriptor) *fakeInner {
	return &fakeInner{
		desc:       desc,
		getEntered: make(chan struct{}, 64),
		extEntered: make(chan struct{}, 64),
	}
}

func (f *fakeInner) Get(ctx context.Context, _ spi.ModelRef) (*spi.ModelDescriptor, error) {
	gate, desc, call, fn := func() (chan struct{}, *spi.ModelDescriptor, int, func(context.Context, int) error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.gets++
		return f.getGate, f.desc, f.gets, f.getFn
	}()
	f.getEntered <- struct{}{}
	if gate != nil {
		<-gate
	}
	if fn != nil {
		if err := fn(ctx, call); err != nil {
			return nil, err
		}
	}
	return desc, nil
}

func (f *fakeInner) ExtendSchema(ctx context.Context, _ spi.ModelRef, _ spi.SchemaDelta) error {
	call, fn := func() (int, func(context.Context, int) error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.ext++
		f.extLive++
		if f.extLive > f.extPeak {
			f.extPeak = f.extLive
		}
		return f.ext, f.extFn
	}()
	defer func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.extLive--
	}()
	f.extEntered <- struct{}{}
	if fn == nil {
		return nil
	}
	return fn(ctx, call)
}

func (f *fakeInner) setGate(g chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getGate = g
}

func (f *fakeInner) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}

func (f *fakeInner) extCount() (calls, peak int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ext, f.extPeak
}

// recordingBroadcaster records every payload broadcast on the invalidation
// topic.
type recordingBroadcaster struct {
	mu       sync.Mutex
	payloads [][]byte
}

func (b *recordingBroadcaster) Broadcast(topic string, payload []byte) {
	if topic != topicModelInvalidate {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.payloads = append(b.payloads, append([]byte(nil), payload...))
}

func (b *recordingBroadcaster) Subscribe(string, func([]byte)) {}

func (b *recordingBroadcaster) sent() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([][]byte(nil), b.payloads...)
}

const testTenant = "tenant-a"

var testRef = spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

func testKey() cacheKey { return cacheKey{tenant: testTenant, ref: testRef} }

func tenantCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u",
		Tenant: spi.Tenant{ID: spi.TenantID(testTenant)},
	})
}

func lockedDesc() *spi.ModelDescriptor {
	return &spi.ModelDescriptor{Ref: testRef, State: spi.ModelLocked, Schema: []byte(`{"ok":true}`)}
}

func newTestCache(bc spi.ClusterBroadcaster) *CachingModelStore {
	return New(bc, nil, time.Hour)
}

func (c *CachingModelStore) testGeneration(key cacheKey) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.generations[key]
}

func (c *CachingModelStore) testHasEntry(key cacheKey) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.entries[key]
	return ok
}

// waitSignal waits for one signal on ch or fails the test.
func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// settle gives goroutines that were just started time to reach their
// single-flight join. Joining cannot be observed without a production hook.
func settle() { time.Sleep(50 * time.Millisecond) }

// TestFillGuard_DropsFillThatStraddlesInvalidation: a miss read that began
// before an invalidation must not store what it read, whether the invalidation
// was local, received by broadcast, or followed by an eviction.
func TestFillGuard_DropsFillThatStraddlesInvalidation(t *testing.T) {
	cases := map[string]func(c *CachingModelStore){
		"local bump": func(c *CachingModelStore) { c.bump(testKey()) },
		"broadcast received": func(c *CachingModelStore) {
			payload, err := EncodeInvalidation(testTenant, testRef)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			c.handleInvalidation(payload)
		},
		"bump then evict": func(c *CachingModelStore) {
			c.bump(testKey())
			c.evict(testKey())
		},
	}
	for name, invalidate := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestCache(nil)
			inner := newFakeInner(lockedDesc())
			gate := make(chan struct{})
			inner.setGate(gate)
			s := &requestScopedStore{cache: c, inner: inner}

			done := make(chan error, 1)
			go func() {
				_, err := s.Get(tenantCtx(), testRef)
				done <- err
			}()
			waitSignal(t, inner.getEntered, "the miss read to reach storage")
			invalidate(c)
			close(gate)
			if err := <-done; err != nil {
				t.Fatalf("Get: %v", err)
			}

			if c.testHasEntry(testKey()) {
				t.Fatal("a fill that straddled an invalidation was stored")
			}
			if _, err := s.Get(tenantCtx(), testRef); err != nil {
				t.Fatalf("second Get: %v", err)
			}
			if got := inner.getCount(); got != 2 {
				t.Fatalf("inner Get calls = %d, want 2 (the straddling fill must not be served)", got)
			}
		})
	}
}

// TestMissFlight_PerGeneration: concurrent misses at one generation share one
// read; a miss that starts after an invalidation never joins a read that
// started before it.
func TestMissFlight_PerGeneration(t *testing.T) {
	c := newTestCache(nil)
	inner := newFakeInner(lockedDesc())
	gate := make(chan struct{})
	inner.setGate(gate)
	s := &requestScopedStore{cache: c, inner: inner}

	var wg sync.WaitGroup
	get := func() {
		defer wg.Done()
		if _, err := s.Get(tenantCtx(), testRef); err != nil {
			t.Errorf("Get: %v", err)
		}
	}
	defer func() {
		close(gate)
		wg.Wait()
	}()

	wg.Add(1)
	go get() // reader A
	waitSignal(t, inner.getEntered, "reader A to reach storage")

	wg.Add(1)
	go get() // reader A2, same generation
	settle()
	if got := inner.getCount(); got != 1 {
		t.Fatalf("inner Get calls = %d after a second miss at the same generation, want 1 (coalesced)", got)
	}

	c.bump(testKey())
	wg.Add(1)
	go get() // reader B, next generation
	waitSignal(t, inner.getEntered, "reader B to read storage on its own")
	if got := inner.getCount(); got != 2 {
		t.Fatalf("inner Get calls = %d, want 2 (B must not join A)", got)
	}
}

// TestMissFlight_WaiterLeavesOnOwnContext: a reader waiting on another
// reader's storage read returns its own context error as soon as its context
// ends.
func TestMissFlight_WaiterLeavesOnOwnContext(t *testing.T) {
	c := newTestCache(nil)
	inner := newFakeInner(lockedDesc())
	gate := make(chan struct{})
	inner.setGate(gate)
	s := &requestScopedStore{cache: c, inner: inner}

	leader := make(chan error, 1)
	go func() {
		_, err := s.Get(tenantCtx(), testRef)
		leader <- err
	}()
	waitSignal(t, inner.getEntered, "the first reader to reach storage")

	ctx, cancel := context.WithCancel(tenantCtx())
	waiter := make(chan error, 1)
	go func() {
		_, err := s.Get(ctx, testRef)
		waiter <- err
	}()
	settle()
	cancel()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("waiter stayed blocked on another reader's read after its own context ended")
	}
	close(gate)
	if err := <-leader; err != nil {
		t.Errorf("first reader: %v", err)
	}
	if got := inner.getCount(); got != 1 {
		t.Errorf("inner Get calls = %d, want 1", got)
	}
}

// TestMissFlight_LeaderCancelled_LiveWaiterReadsAgain: a read that failed
// after its first caller's context ended — with an error that does not wrap
// context.Canceled — is not an answer for a waiter whose context is live; the
// waiter reads again.
func TestMissFlight_LeaderCancelled_LiveWaiterReadsAgain(t *testing.T) {
	c := newTestCache(nil)
	inner := newFakeInner(lockedDesc())
	inner.getFn = func(ctx context.Context, call int) error {
		if call == 1 {
			<-ctx.Done()
			return fmt.Errorf("failed to get model: %w", errors.New("driver: connection closed"))
		}
		return nil
	}
	s := &requestScopedStore{cache: c, inner: inner}

	leaderCtx, cancel := context.WithCancel(tenantCtx())
	defer cancel()
	leader := make(chan error, 1)
	go func() {
		_, err := s.Get(leaderCtx, testRef)
		leader <- err
	}()
	waitSignal(t, inner.getEntered, "the first reader to reach storage")

	waiter := make(chan error, 1)
	go func() {
		desc, err := s.Get(tenantCtx(), testRef)
		if err == nil && desc == nil {
			err = errors.New("nil descriptor")
		}
		waiter <- err
	}()
	settle()
	if got := inner.getCount(); got != 1 {
		t.Fatalf("inner Get calls = %d before the cancel, want 1 (the waiter joins)", got)
	}
	cancel()
	if err := <-leader; err == nil {
		t.Error("first reader returned nil after its context was cancelled")
	}
	select {
	case err := <-waiter:
		if err != nil {
			t.Errorf("waiter error = %v, want nil (it must read again on its live context)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not return")
	}
	if got := inner.getCount(); got != 2 {
		t.Errorf("inner Get calls = %d, want 2", got)
	}
}

// TestRefresh_DoesNotEvict: a refresh reads storage but neither evicts the
// cached entry nor bumps the generation, so concurrent readers keep hitting
// the cache.
func TestRefresh_DoesNotEvict(t *testing.T) {
	c := newTestCache(nil)
	inner := newFakeInner(lockedDesc())
	s := &requestScopedStore{cache: c, inner: inner}

	if _, err := s.Get(tenantCtx(), testRef); err != nil {
		t.Fatalf("warm Get: %v", err)
	}
	<-inner.getEntered
	genBefore := c.testGeneration(testKey())

	gate := make(chan struct{})
	inner.setGate(gate)
	refreshed := make(chan error, 1)
	go func() {
		_, err := s.RefreshAndGet(tenantCtx(), testRef)
		refreshed <- err
	}()
	waitSignal(t, inner.getEntered, "the refresh to read storage")

	got := make(chan error, 1)
	go func() {
		_, err := s.Get(tenantCtx(), testRef)
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Errorf("concurrent Get: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("concurrent Get blocked on storage: the refresh evicted the cached entry")
	}
	if n := inner.getCount(); n != 2 {
		t.Errorf("inner Get calls = %d, want 2 (warm + refresh; the concurrent Get must hit the cache)", n)
	}
	close(gate)
	if err := <-refreshed; err != nil {
		t.Fatalf("RefreshAndGet: %v", err)
	}
	if got := c.testGeneration(testKey()); got != genBefore {
		t.Fatalf("generation = %d after refresh, want %d (unchanged)", got, genBefore)
	}
	if !c.testHasEntry(testKey()) {
		t.Fatal("refresh left no cached entry")
	}
}

// TestCache_JitterKeepsEntriesWithinBand: a stored entry's expiry lies within
// lease ±10%.
func TestCache_JitterKeepsEntriesWithinBand(t *testing.T) {
	clk := &fixedClock{now: time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)}
	lease := time.Hour
	c := New(nil, clk, lease)
	s := &requestScopedStore{cache: c, inner: newFakeInner(lockedDesc())}

	if _, err := s.Get(tenantCtx(), testRef); err != nil {
		t.Fatalf("Get: %v", err)
	}
	exp := func() time.Time {
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.entries[testKey()].expiresAt
	}()
	minExp := clk.now.Add(time.Duration(float64(lease) * 0.9))
	maxExp := clk.now.Add(time.Duration(float64(lease) * 1.1))
	if exp.Before(minExp) || exp.After(maxExp) {
		t.Errorf("expiry %v outside ±10%% jitter band [%v, %v]", exp, minExp, maxExp)
	}
}

type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time { return c.now }
