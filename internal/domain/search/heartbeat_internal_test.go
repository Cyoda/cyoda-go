package search

import (
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestHeartbeatFencedOut pins the one exception to "any Heartbeat error
// aborts the job": spi.ErrTaskBusy, and only spi.ErrTaskBusy, is a missed
// tick rather than a lost claim.
func TestHeartbeatFencedOut(t *testing.T) {
	// The shape plugins/postgres's searchJobBusy actually wraps: the busy
	// marker chained with the underlying pg error (55P03) that caused it.
	busyWithPGError := fmt.Errorf("heartbeat search job job-1: %w: %w",
		spi.ErrTaskBusy, errors.New("ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)"))

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"busy row wrapping a pg error", busyWithPGError, false},
		{"stale claim", fmt.Errorf("heartbeat search job job-1: %w", spi.ErrStaleClaim), true},
		{"already terminal", fmt.Errorf("heartbeat search job job-1: %w", spi.ErrAlreadyTerminal), true},
		{"not found", fmt.Errorf("search job %q not found: %w", "job-1", spi.ErrNotFound), true},
		{"plain error", errors.New("connection reset by peer"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := heartbeatFencedOut(tc.err); got != tc.want {
				t.Errorf("heartbeatFencedOut(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
