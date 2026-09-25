package fixtureutil

import "time"

// The scheduler timing every parity and multi-node fixture runs with. The
// scenarios read these to size their waits; the fixtures pass them to the
// server through TunedServerEnv / TunedClusterEnv.
//
//   - TunedHeartbeatInterval 1s (default 15s): a pnode's liveness record is
//     refreshed every second.
//   - TunedStaleAfter 53s (default 2m): the lowest value the server accepts
//     with a 1s heartbeat (50s + 3 x heartbeat). A lost-owner scenario waits
//     at least this long; nothing shorter is valid.
//   - TunedRetryDelay 1s (default 30s) and TunedRetryDelayMax 4s (default
//     15m): a safe failure is retried after 1s, 2s, 4s, 4s, …. One second is
//     long enough that a scenario polling every 50ms sees "attempts 1"
//     before the second attempt.
const (
	TunedHeartbeatInterval = time.Second
	TunedStaleAfter        = 53 * time.Second
	TunedRetryDelay        = time.Second
	TunedRetryDelayMax     = 4 * time.Second
)

// TunedServerEnv returns the server settings every single-node parity fixture
// runs with, so the shared scenario set stays fast. Every fixture — in-tree
// and out-of-tree — appends it to its backend env; the values live here only.
//
//   - CYODA_SCHEDULER_SCAN_INTERVAL=50ms (default 1s): the scheduled-transition
//     scenarios observe fires within a small poll window. Harmless to every
//     other scenario — an empty claim is a cheap no-op query.
//   - CYODA_DISPATCH_WAIT_TIMEOUT=200ms (default 5s): how long a callout waits
//     for a compute node to exist. On a single node nothing needs waiting for
//     — a compute client is registered before it is told so — and the
//     scenarios that end with "no compute node" would otherwise each cost the
//     full default on every backend. Scenarios must not depend on a compute
//     client appearing within this window.
//   - the scheduler timing above.
func TunedServerEnv() []string {
	return append([]string{
		"CYODA_SCHEDULER_SCAN_INTERVAL=50ms",
		"CYODA_DISPATCH_WAIT_TIMEOUT=200ms",
	}, schedulerTimingEnv()...)
}

// TunedClusterEnv returns the settings every cluster parity fixture adds per
// node. The wait is longer than on a single node because in a cluster it also
// covers the gossip delay between a compute client joining one node and the
// others learning its tags. The scan interval and the scheduler timing are
// the single-node values.
func TunedClusterEnv() []string {
	return append([]string{
		"CYODA_DISPATCH_WAIT_TIMEOUT=2s",
		"CYODA_SCHEDULER_SCAN_INTERVAL=50ms",
	}, schedulerTimingEnv()...)
}

func schedulerTimingEnv() []string {
	return []string{
		"CYODA_SCHEDULER_HEARTBEAT_INTERVAL=" + TunedHeartbeatInterval.String(),
		"CYODA_SCHEDULER_STALE_AFTER=" + TunedStaleAfter.String(),
		"CYODA_SCHEDULER_RETRY_DELAY=" + TunedRetryDelay.String(),
		"CYODA_SCHEDULER_RETRY_DELAY_MAX=" + TunedRetryDelayMax.String(),
	}
}
