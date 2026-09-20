package fixtureutil

// TunedServerEnv returns the server settings every single-node parity fixture
// runs with, so the shared scenario set stays fast. Every fixture — in-tree
// and out-of-tree — appends it to its backend env; the values live here only.
//
//   - CYODA_SCHEDULER_SCAN_INTERVAL=50ms (default 1s): the scheduled-transition
//     scenarios observe fires within a small poll window. Harmless to every
//     other scenario — an empty scan is a cheap no-op query.
//   - CYODA_DISPATCH_WAIT_TIMEOUT=200ms (default 5s): how long a callout waits
//     for a compute node to exist. On a single node nothing needs waiting for
//     — a compute client is registered before it is told so — and the
//     scenarios that end with "no compute node" would otherwise each cost the
//     full default on every backend. Scenarios must not depend on a compute
//     client appearing within this window.
func TunedServerEnv() []string {
	return []string{
		"CYODA_SCHEDULER_SCAN_INTERVAL=50ms",
		"CYODA_DISPATCH_WAIT_TIMEOUT=200ms",
	}
}

// TunedClusterEnv returns the settings every cluster parity fixture adds per
// node. The wait is longer than on a single node because in a cluster it also
// covers the gossip delay between a compute client joining one node and the
// others learning its tags.
func TunedClusterEnv() []string {
	return []string{"CYODA_DISPATCH_WAIT_TIMEOUT=2s"}
}
