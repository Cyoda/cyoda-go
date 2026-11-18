// internal/domain/model/schema/property_budget_test.go
package schema_test

import (
	"os/exec"
	"testing"
	"time"
)

// TestPropertyBudget runs the property suite end-to-end and asserts it
// completes within the CI-ceiling of 60 s. Advisory local target is 45 s
// — surfaced via t.Logf but not enforced.
//
// This test invokes `go test -short -run 'TestRoundtrip|TestCommutativity|TestMonotonicity|TestIdempotence|TestPermutation'`
// as a subprocess so the measurement excludes TestPropertyBudget's own cost.
func TestPropertyBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("budget meta-test is a slow sanity check, skipped under -short")
	}
	start := time.Now()
	cmd := exec.Command("go", "test", "-short",
		"-run", "TestRoundtrip|TestCommutativity|TestMonotonicity|TestIdempotence|TestPermutation",
		"github.com/cyoda/cyoda-go/internal/domain/model/schema")
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("subprocess failed: %v\n%s", err, out)
	}
	t.Logf("property suite: %v (advisory target <= 45s local)", elapsed)
	if elapsed > 60*time.Second {
		t.Fatalf("property suite exceeded CI budget: %v > 60s", elapsed)
	}
}
