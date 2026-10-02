package app_test

import (
	"testing"
	"time"
)

// TestApp_Close_StopsAuthLoopAndWaitReturnsAtOnceAfterward is the App-level
// wiring proof for the fix: Close() must actually reach stopAuthLoops (the
// cancel-then-Wait closure app.New wires up around the signing-key store's
// background work), not just leave it constructed. If Close ran without
// stopping that loop, AuthService().Wait() called afterward would still
// block on whatever is in flight; here it must return at once, since Close
// already did the waiting.
func TestApp_Close_StopsAuthLoopAndWaitReturnsAtOnceAfterward(t *testing.T) {
	a := jwtApp(t)

	if err := a.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	done := make(chan struct{})
	go func() {
		a.AuthService().Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("AuthService().Wait() blocked after Close had already stopped the loop")
	}
}
