// Package multinode hosts parity scenarios that require a cyoda-go
// cluster — multiple cyoda-go subprocesses sharing the same backing
// storage. Backends that physically cannot share state across N
// processes (memory, sqlite single-file) do not implement
// MultiNodeFixture and never run these scenarios.
//
// The cluster-capable backend that runs these scenarios is postgres
// (e2e/parity/postgres): it provides a fixture implementation and a
// TestMultiNode entry that imports this package, which triggers
// init-time registration. The cassandra plugin runs the single-node
// parity list only; a multi-node fixture for it is not yet provided.
package multinode

import "testing"

// NamedTest is a single multi-node parity scenario plus the name it
// shows up as in subtest output.
type NamedTest struct {
	Name string
	Fn   func(t *testing.T, fixture MultiNodeFixture)
}

var allTests []NamedTest

// Register appends additional NamedTests to the canonical list at
// init time. Sub-packages call Register from init().
//
// Every cluster-capable backend's test wrapper (today only postgres)
// MUST import the multinode-extension packages — otherwise the
// extension's init() never runs and the wrapper silently misses the
// entire scenario set. Currently the only extension is this package
// itself; future cluster-shareable extension packages added by later
// tranches must be added to all cluster-capable backend wrappers in
// lockstep.
func Register(tests ...NamedTest) {
	allTests = append(allTests, tests...)
}

// AllTests returns the canonical list of multi-node scenarios in
// registration order. The returned slice is a defensive copy.
func AllTests() []NamedTest {
	out := make([]NamedTest, len(allTests))
	copy(out, allTests)
	return out
}
