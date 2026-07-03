// Package oasdiffcheck verifies the premise of the ADR 0003 breaking-change CI
// gate: that oasdiff correctly classifies additive response-schema changes as
// non-breaking and sealed (additionalProperties: false) changes as breaking.
//
// This test is deliberately isolated from internal/e2e, whose TestMain boots a
// Postgres container that this test does not need.
package oasdiffcheck_test

import (
	"os/exec"
	"testing"
)

// testdataDir is relative to the package directory; Go test sets cwd to the
// package being tested, so this path is stable across machines.
const testdataDir = "testdata/oasdiff"

func TestOasdiffGatePremise(t *testing.T) {
	bin, err := exec.LookPath("oasdiff")
	if err != nil {
		t.Skip("oasdiff not in PATH; install with: go install github.com/oasdiff/oasdiff@latest")
	}

	base := testdataDir + "/base.yaml"
	additive := testdataDir + "/additive.yaml"
	breaking := testdataDir + "/breaking.yaml"

	t.Run("additive_is_not_breaking", func(t *testing.T) {
		cmd := exec.Command(bin, "breaking", base, additive, "--fail-on", "ERR")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("oasdiff breaking base→additive: expected exit 0 (no breaking change), got %v\noutput:\n%s", err, out)
		}
		t.Logf("oasdiff output (base→additive): %s", out)
	})

	t.Run("sealing_additionalProperties_is_breaking", func(t *testing.T) {
		cmd := exec.Command(bin, "breaking", base, breaking, "--fail-on", "ERR")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("oasdiff breaking base→breaking: expected non-zero exit (breaking change detected), got exit 0\noutput:\n%s", out)
		}
		t.Logf("oasdiff output (base→breaking): %s", out)
	})
}
