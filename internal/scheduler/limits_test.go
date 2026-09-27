package scheduler

import (
	"testing"
	"time"
)

func TestMinStaleAfter_IsFiftySecondsPlusThreeHeartbeats(t *testing.T) {
	for _, tt := range []struct{ heartbeat, want time.Duration }{
		{15 * time.Second, 95 * time.Second},
		{time.Second, 53 * time.Second},
		{time.Minute, 230 * time.Second},
	} {
		if got := MinStaleAfter(tt.heartbeat); got != tt.want {
			t.Errorf("MinStaleAfter(%s) = %s, want %s", tt.heartbeat, got, tt.want)
		}
	}
}

func TestHeartbeatBudget_IsTheSmallerOfTenSecondsAndTheInterval(t *testing.T) {
	for _, tt := range []struct{ interval, want time.Duration }{
		{15 * time.Second, 10 * time.Second},
		{10 * time.Second, 10 * time.Second},
		{5 * time.Second, 5 * time.Second},
	} {
		if got := heartbeatBudget(tt.interval); got != tt.want {
			t.Errorf("heartbeatBudget(%s) = %s, want %s", tt.interval, got, tt.want)
		}
	}
}

// At the smallest valid STALE_AFTER, W must cover two heartbeat intervals and
// one heartbeat's budget with a full interval to spare: then one slow or failed
// heartbeat is always followed by another that lands inside the window.
func TestWatchdogWindow_OneSlowOrFailedHeartbeatNeverSelfCancels(t *testing.T) {
	for _, h := range []time.Duration{time.Second, 10 * time.Second, 15 * time.Second, time.Minute} {
		w := watchdogWindow(MinStaleAfter(h))
		if need := 3*h + heartbeatBudget(h); w < need {
			t.Errorf("heartbeat %s: W = %s, want >= %s", h, w, need)
		}
	}
	if got := watchdogWindow(2 * time.Minute); got != 80*time.Second {
		t.Errorf("W at the 2m default = %s, want 80s (120s - 30s commit budget - 10s slack)", got)
	}
}
