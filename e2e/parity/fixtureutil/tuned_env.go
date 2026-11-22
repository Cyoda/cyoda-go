package fixtureutil

import "time"

// The timing every parity and multi-node fixture runs with, and the e2e
// scheduler stacks too. The scenarios read these to size their waits; the
// fixtures pass them to the server through TunedServerEnv / TunedClusterEnv.
//
//   - TunedScanInterval 50ms (default 1s): the scheduled-transition scenarios
//     observe fires within a small poll window. Harmless to every other
//     scenario — an empty claim is a cheap no-op query.
//
//   - TunedDispatchWaitTimeout 200ms (default 5s): how long a callout waits
//     for a compute node to exist, on a single node. Nothing needs waiting
//     for there — a compute client is registered before it is told so — and
//     the scenarios that end with "no compute node" would otherwise each cost
//     the full default on every backend. Scenarios must not depend on a
//     compute client appearing within this window.
//
//   - TunedClusterDispatchWaitTimeout 2s: the same wait in a cluster, longer
//     because it also covers the gossip delay between a compute client
//     joining one node and the others learning its tags.
//
//   - TunedHeartbeatInterval 1s (default 15s): a pnode's liveness record is
//     refreshed every second.
//
//   - TunedStaleAfter 53s (default 2m): the lowest value the server accepts
//     with a 1s heartbeat (50s + 3 x heartbeat). A lost-owner scenario waits
//     at least this long; nothing shorter is valid.
//
//   - TunedRetryDelay 1s (default 30s) and TunedRetryDelayMax 4s (default
//     15m): a safe failure is retried after 1s, 2s, 4s, 4s, …. One second is
//     long enough that a scenario polling every 50ms sees "attempts 1"
//     before the second attempt.
const (
	TunedScanInterval               = 50 * time.Millisecond
	TunedDispatchWaitTimeout        = 200 * time.Millisecond
	TunedClusterDispatchWaitTimeout = 2 * time.Second

	TunedHeartbeatInterval = time.Second
	TunedStaleAfter        = 53 * time.Second
	TunedRetryDelay        = time.Second
	TunedRetryDelayMax     = 4 * time.Second
)

// TunedServerEnv returns the server settings every single-node parity fixture
// runs with, so the shared scenario set stays fast. Every fixture — in-tree
// and out-of-tree — appends it to its backend env; the values live here only.
func TunedServerEnv() []string {
	return append([]string{
		"CYODA_SCHEDULER_SCAN_INTERVAL=" + TunedScanInterval.String(),
		"CYODA_DISPATCH_WAIT_TIMEOUT=" + TunedDispatchWaitTimeout.String(),
	}, schedulerTimingEnv()...)
}

// TunedClusterEnv returns the settings every cluster parity fixture adds per
// node: the single-node values, with the cluster's dispatch wait.
func TunedClusterEnv() []string {
	return append([]string{
		"CYODA_SCHEDULER_SCAN_INTERVAL=" + TunedScanInterval.String(),
		"CYODA_DISPATCH_WAIT_TIMEOUT=" + TunedClusterDispatchWaitTimeout.String(),
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
