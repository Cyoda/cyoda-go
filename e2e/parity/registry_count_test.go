package parity

import "testing"

// wantParityScenarioCount is the number of entries in allTests. The count in
// the registry.go header comment drifted silently for many PRs because nothing
// asserted it; this test converts that drift into a failure. Bump this constant
// (and the header comment) in the same change that adds or removes a scenario.
const wantParityScenarioCount = 199

// TestParityScenarioCount guards the documented scenario count against silent
// drift.
func TestParityScenarioCount(t *testing.T) {
	if got := len(allTests); got != wantParityScenarioCount {
		t.Errorf("parity scenario count = %d, want %d; update wantParityScenarioCount and the registry.go header comment together", got, wantParityScenarioCount)
	}
}

// TestParityScenarioNamesUnique guards against copy-paste duplicate scenario
// names — two entries sharing a Name collide as subtests and one silently
// shadows the other in test output.
func TestParityScenarioNamesUnique(t *testing.T) {
	seen := make(map[string]int, len(allTests))
	for i, tc := range allTests {
		if prev, dup := seen[tc.Name]; dup {
			t.Errorf("duplicate parity scenario name %q at indices %d and %d", tc.Name, prev, i)
		}
		seen[tc.Name] = i
	}
}
