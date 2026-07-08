package fixtureutil

import (
	"errors"
	"testing"
	"time"
)

// TestRetryLaunch_SucceedsOnLastAttempt verifies that retryLaunch stops as
// soon as fn returns nil and reports success, having called fn exactly the
// number of times needed (attempts-1 failures then a success).
func TestRetryLaunch_SucceedsOnLastAttempt(t *testing.T) {
	const attempts = 3
	calls := 0
	err := retryLaunch(attempts, func() error {
		calls++
		if calls < attempts {
			return errors.New("transient bind collision")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryLaunch: got err %v, want nil", err)
	}
	if calls != attempts {
		t.Fatalf("fn call count: got %d, want %d", calls, attempts)
	}
}

// TestRetryLaunch_ReturnsLastErrorAfterExhausting verifies that when fn always
// fails, retryLaunch tries exactly `attempts` times and returns the LAST error.
func TestRetryLaunch_ReturnsLastErrorAfterExhausting(t *testing.T) {
	const attempts = 3
	calls := 0
	sentinel := errors.New("last error")
	err := retryLaunch(attempts, func() error {
		calls++
		if calls == attempts {
			return sentinel
		}
		return errors.New("earlier error")
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("retryLaunch: got err %v, want last error %v", err, sentinel)
	}
	if calls != attempts {
		t.Fatalf("fn call count: got %d, want %d", calls, attempts)
	}
}

// TestRetryLaunch_SingleAttemptFloor verifies attempts < 1 is treated as one
// attempt rather than zero (which would silently skip fn and return nil).
func TestRetryLaunch_SingleAttemptFloor(t *testing.T) {
	calls := 0
	sentinel := errors.New("boom")
	err := retryLaunch(0, func() error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("retryLaunch: got err %v, want %v", err, sentinel)
	}
	if calls != 1 {
		t.Fatalf("fn call count: got %d, want 1", calls)
	}
}

// TestNodeOutcome_HealthReady verifies the happy path: the health probe
// finishes first with nil, so the node is deemed healthy.
func TestNodeOutcome_HealthReady(t *testing.T) {
	healthDoneCh := make(chan error, 1)
	exitedCh := make(chan struct{})
	healthDoneCh <- nil
	if err := nodeOutcome(0, healthDoneCh, exitedCh, func() error {
		t.Fatal("exitErrFn should not be called when health is ready")
		return nil
	}); err != nil {
		t.Fatalf("nodeOutcome: got err %v, want nil", err)
	}
}

// TestNodeOutcome_HealthTimeout verifies a health probe that finishes with a
// timeout error is surfaced verbatim (not masked as an exit).
func TestNodeOutcome_HealthTimeout(t *testing.T) {
	healthDoneCh := make(chan error, 1)
	exitedCh := make(chan struct{})
	timeoutErr := errors.New("health check did not return 200 within 120s")
	healthDoneCh <- timeoutErr
	err := nodeOutcome(0, healthDoneCh, exitedCh, nil)
	if !errors.Is(err, timeoutErr) {
		t.Fatalf("nodeOutcome: got err %v, want timeout err %v", err, timeoutErr)
	}
}

// TestNodeOutcome_NodeExitedFirst verifies that when the node process exits
// before the health probe finishes, nodeOutcome fails fast and wraps the
// exit error rather than blocking on the (never-arriving) health result.
func TestNodeOutcome_NodeExitedFirst(t *testing.T) {
	healthDoneCh := make(chan error, 1) // never fed — simulates a blind 120s wait
	exitedCh := make(chan struct{})
	exitErr := errors.New("exit status 1")
	close(exitedCh)

	done := make(chan error, 1)
	go func() {
		done <- nodeOutcome(2, healthDoneCh, exitedCh, func() error { return exitErr })
	}()

	select {
	case err := <-done:
		if !errors.Is(err, exitErr) {
			t.Fatalf("nodeOutcome: got err %v, want wrapped exit err %v", err, exitErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nodeOutcome blocked instead of failing fast on node exit")
	}
}

// TestNodeOutcome_NodeExitedNilError verifies a clean (exit 0) early exit is
// still a failure and produces a sensible message without a %!w(<nil>) artifact.
func TestNodeOutcome_NodeExitedNilError(t *testing.T) {
	healthDoneCh := make(chan error, 1)
	exitedCh := make(chan struct{})
	close(exitedCh)
	err := nodeOutcome(1, healthDoneCh, exitedCh, func() error { return nil })
	if err == nil {
		t.Fatal("nodeOutcome: got nil, want failure on early exit")
	}
	if got := err.Error(); got == "" || containsPercentW(got) {
		t.Fatalf("nodeOutcome error message malformed: %q", got)
	}
}

func containsPercentW(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == '%' && s[i+1] == '!' && s[i+2] == 'w' {
			return true
		}
	}
	return false
}
