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
//
// Close itself runs inside the timed goroutine, alongside the Wait it
// gates, so a hang in either one fails this test in 2s rather than
// surfacing as the package's full test timeout. Close's error is reported
// from the main goroutine only, via a buffered channel: calling a *T method
// from the background goroutine after a timeout has already ended the test
// (the hang case) is itself unsafe, so no t.* call happens there.
func TestApp_Close_StopsAuthLoopAndWaitReturnsAtOnceAfterward(t *testing.T) {
	a := jwtApp(t)

	closeErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		closeErr <- a.Close()
		a.AuthService().Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close and/or the following AuthService().Wait() did not return within 2s")
	}
	if err := <-closeErr; err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
}
