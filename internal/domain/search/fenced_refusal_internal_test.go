package search

import (
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestFencedRefusal pins which store answers end a claim: a stale claim, a
// terminal job and a gone job. A busy row and a transport error are not
// refusals — the heartbeat ticker treats them as a missed tick.
func TestFencedRefusal(t *testing.T) {
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
		{"plain transport error", errors.New("connection reset by peer"), false},
		{"stale claim", fmt.Errorf("heartbeat search job job-1: %w", spi.ErrStaleClaim), true},
		{"already terminal", fmt.Errorf("heartbeat search job job-1: %w", spi.ErrAlreadyTerminal), true},
		{"not found", fmt.Errorf("search job %q not found: %w", "job-1", spi.ErrNotFound), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fencedRefusal(tc.err); got != tc.want {
				t.Errorf("fencedRefusal(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
