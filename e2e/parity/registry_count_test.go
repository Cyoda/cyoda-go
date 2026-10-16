package parity

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// wantParityScenarioCount is the number of entries in allTests, which the
// registry.go header comment states too. Change both in the same change that
// adds or removes a scenario; TestParityScenarioCount fails until they agree.
const wantParityScenarioCount = 247

// headerCount finds the count in the registry.go header comment.
var headerCount = regexp.MustCompile(`// allTests holds (\d+) scenarios`)

// TestParityScenarioCount keeps allTests, wantParityScenarioCount and the
// count in the registry.go header comment the same.
func TestParityScenarioCount(t *testing.T) {
	if got := len(allTests); got != wantParityScenarioCount {
		t.Errorf("parity scenario count = %d, want %d; update wantParityScenarioCount and the registry.go header comment together", got, wantParityScenarioCount)
	}
	src, err := os.ReadFile("registry.go")
	if err != nil {
		t.Fatalf("read registry.go: %v", err)
	}
	m := headerCount.FindSubmatch(src)
	if m == nil {
		t.Fatalf("registry.go header comment does not state the count as %s", headerCount)
	}
	if n, _ := strconv.Atoi(string(m[1])); n != wantParityScenarioCount {
		t.Errorf("registry.go header comment states %d scenarios, want %d", n, wantParityScenarioCount)
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
