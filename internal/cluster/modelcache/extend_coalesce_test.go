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

var testDelta = spi.SchemaDelta(`[{"op":"add","path":"$.x"}]`)

// TestExtend_EveryOutcomeEvictsBumpsBroadcasts: whatever the extension call
// returns, the cache drops its entry, bumps the generation and broadcasts one
// invalidation — the call may have committed even when it failed.
func TestExtend_EveryOutcomeEvictsBumpsBroadcasts(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"error", errors.New("storage unavailable")},
		{"no-op", nil}, // a delta the schema already holds also returns nil
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bc := &recordingBroadcaster{}
			c := newTestCache(bc)
			inner := newFakeInner(lockedDesc())
			inner.extFn = func(context.Context, int) error { return tc.err }
			s := &requestScopedStore{cache: c, inner: inner}

			if _, err := s.Get(tenantCtx(), testRef); err != nil {
				t.Fatalf("warm Get: %v", err)
			}
			if !c.testHasEntry(testKey()) {
				t.Fatal("setup: entry not cached")
			}
			genBefore := c.testGeneration(testKey())

			err := s.ExtendSchema(tenantCtx(), testRef, testDelta)
			if !errors.Is(err, tc.err) {
				t.Fatalf("ExtendSchema error = %v, want %v", err, tc.err)
			}
			if c.testHasEntry(testKey()) {
				t.Error("entry still cached after the extension call")
			}
			if got := c.testGeneration(testKey()); got != genBefore+1 {
				t.Errorf("generation = %d, want %d", got, genBefore+1)
			}
			sent := bc.sent()
			if len(sent) != 1 {
				t.Fatalf("broadcasts = %d, want 1", len(sent))
			}
			tenant, ref, ok := DecodeInvalidation(sent[0])
			if !ok || tenant != testTenant || ref != testRef {
				t.Errorf("broadcast payload = (%q, %v, %v), want (%q, %v, true)", tenant, ref, ok, testTenant, testRef)
			}
		})
	}
}

// TestExtendCoalesce_IdenticalDeltasOneCall: concurrent identical extensions
// on one node share one storage call, and so one invalidation.
func TestExtendCoalesce_IdenticalDeltasOneCall(t *testing.T) {
	bc := &recordingBroadcaster{}
	c := newTestCache(bc)
	inner := newFakeInner(lockedDesc())
	release := make(chan struct{})
	inner.extFn = func(context.Context, int) error {
		<-release
		return nil
	}

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := &requestScopedStore{cache: c, inner: inner}
			errs <- s.ExtendSchema(tenantCtx(), testRef, testDelta)
		}()
	}
	waitSignal(t, inner.extEntered, "the first extension to reach storage")
	settle()
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("ExtendSchema: %v", err)
		}
	}
	if calls, _ := inner.extCount(); calls != 1 {
		t.Errorf("inner ExtendSchema calls = %d, want 1", calls)
	}
	if got := len(bc.sent()); got != 1 {
		t.Errorf("broadcasts = %d, want 1", got)
	}
}

// TestExtendCoalesce_FollowerLeavesOnOwnContext: a caller waiting on another
// caller's extension returns its own context error as soon as its context
// ends; the shared call carries on for the first caller.
func TestExtendCoalesce_FollowerLeavesOnOwnContext(t *testing.T) {
	c := newTestCache(nil)
	inner := newFakeInner(lockedDesc())
	release := make(chan struct{})
	inner.extFn = func(context.Context, int) error {
		<-release
		return nil
	}
	s := &requestScopedStore{cache: c, inner: inner}

	leaderDone := make(chan error, 1)
	go func() { leaderDone <- s.ExtendSchema(tenantCtx(), testRef, testDelta) }()
	waitSignal(t, inner.extEntered, "the leader to reach storage")

	followerCtx, cancel := context.WithCancel(tenantCtx())
	followerDone := make(chan error, 1)
	go func() { followerDone <- s.ExtendSchema(followerCtx, testRef, testDelta) }()
	settle()
	cancel()

	select {
	case err := <-followerDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("follower error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("follower stayed blocked on the leader's call after its own context ended")
	}

	close(release)
	if err := <-leaderDone; err != nil {
		t.Errorf("leader error = %v, want nil", err)
	}
	if calls, _ := inner.extCount(); calls != 1 {
		t.Errorf("inner ExtendSchema calls = %d, want 1", calls)
	}
}

// TestExtendCoalesce_LeaderCancelled_LiveFollowerRetries: the shared call ran
// on the first caller's context; when that context ends and the call fails —
// here with a driver error that does not wrap context.Canceled — a caller whose
// own context is live starts a new call instead of inheriting the failure.
func TestExtendCoalesce_LeaderCancelled_LiveFollowerRetries(t *testing.T) {
	c := newTestCache(nil)
	inner := newFakeInner(lockedDesc())
	errDriver := errors.New("driver: connection closed")
	inner.extFn = func(ctx context.Context, call int) error {
		if call == 1 {
			<-ctx.Done()
			return fmt.Errorf("failed to extend schema: %w", errDriver)
		}
		return nil
	}
	s := &requestScopedStore{cache: c, inner: inner}

	leaderCtx, cancel := context.WithCancel(tenantCtx())
	defer cancel()
	leaderDone := make(chan error, 1)
	go func() { leaderDone <- s.ExtendSchema(leaderCtx, testRef, testDelta) }()
	waitSignal(t, inner.extEntered, "the leader to reach storage")

	followerDone := make(chan error, 1)
	go func() { followerDone <- s.ExtendSchema(tenantCtx(), testRef, testDelta) }()
	settle()
	cancel()

	if err := <-leaderDone; err == nil {
		t.Error("leader returned nil after its context was cancelled")
	}
	select {
	case err := <-followerDone:
		if err != nil {
			t.Errorf("follower error = %v, want nil (it must retry on its live context)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follower did not return")
	}
	calls, peak := inner.extCount()
	if calls != 2 {
		t.Errorf("inner ExtendSchema calls = %d, want 2", calls)
	}
	if peak != 1 {
		t.Errorf("overlapping inner ExtendSchema calls = %d, want 1 (the follower must wait on the leader's call)", peak)
	}
}

// TestExtend_InvalidatesAfterCallEndsEvenWhenEveryCallerLeft: the
// invalidation belongs to the shared call, after the storage call returns —
// not to the callers. A caller that left early must not invalidate before the
// extension can have committed (a read in between would re-cache the old
// schema), and its leaving must not skip the invalidation.
func TestExtend_InvalidatesAfterCallEndsEvenWhenEveryCallerLeft(t *testing.T) {
	bc := &recordingBroadcaster{}
	c := newTestCache(bc)
	inner := newFakeInner(lockedDesc())
	release := make(chan struct{})
	inner.extFn = func(context.Context, int) error {
		<-release
		return nil
	}
	s := &requestScopedStore{cache: c, inner: inner}

	ctx, cancel := context.WithCancel(tenantCtx())
	done := make(chan error, 1)
	go func() { done <- s.ExtendSchema(ctx, testRef, testDelta) }()
	waitSignal(t, inner.extEntered, "the extension to reach storage")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller error = %v, want context.Canceled", err)
	}

	// The extension has not returned; a read now caches the pre-extension
	// schema.
	if _, err := s.Get(tenantCtx(), testRef); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !c.testHasEntry(testKey()) {
		t.Fatal("setup: read during the extension was not cached")
	}
	genBefore := c.testGeneration(testKey())

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for len(bc.sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(bc.sent()); got != 1 {
		t.Fatalf("broadcasts = %d, want 1 after the storage call returned", got)
	}
	if c.testHasEntry(testKey()) {
		t.Error("entry cached during the extension survived the call's end")
	}
	if got := c.testGeneration(testKey()); got != genBefore+1 {
		t.Errorf("generation = %d, want %d", got, genBefore+1)
	}
}
