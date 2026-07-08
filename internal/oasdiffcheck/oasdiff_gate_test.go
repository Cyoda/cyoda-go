// Package oasdiffcheck verifies the ADR 0003 breaking-change CI gate.
//
// TestOasdiffGatePremise checks that the pinned oasdiff classifies an additive
// change as non-breaking and a client-breaking change (a request-parameter type
// narrowing) as breaking — the assumption the oasdiff gate rests on.
//
// TestSpecHasNoSealedSchemas enforces ADR 0003's "typed-but-open" rule directly.
// oasdiff does NOT flag sealing a response schema (additionalProperties: false)
// as breaking — it is not client-breaking — so oasdiff alone cannot enforce the
// never-seal rule; this direct check provides that teeth.
//
// This package is deliberately isolated from internal/e2e, whose TestMain boots a
// Postgres container these tests do not need.
package oasdiffcheck_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// testdataDir is relative to the package directory; Go test sets cwd to the
// package being tested, so this path is stable across machines.
const testdataDir = "testdata/oasdiff"

func TestOasdiffGatePremise(t *testing.T) {
	bin, err := exec.LookPath("oasdiff")
	if err != nil {
		t.Skip("oasdiff not in PATH; install with: go install github.com/oasdiff/oasdiff@v1.21.0")
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

	t.Run("request_param_type_change_is_breaking", func(t *testing.T) {
		cmd := exec.Command(bin, "breaking", base, breaking, "--fail-on", "ERR")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("oasdiff breaking base→breaking: expected non-zero exit (breaking change detected), got exit 0\noutput:\n%s", out)
		}
		t.Logf("oasdiff output (base→breaking): %s", out)
	})
}

// TestSpecHasNoSealedSchemas enforces ADR 0003 Decision 2 directly: the authored
// api/openapi.yaml must never seal a schema with `additionalProperties: false`.
// Because oasdiff does not catch response-sealing, this is the check with teeth
// for the never-seal rule — and it runs in the normal `go test` suite with no
// oasdiff binary required.
func TestSpecHasNoSealedSchemas(t *testing.T) {
	specPath := filepath.Join("..", "..", "api", "openapi.yaml")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	re := regexp.MustCompile(`(?m)additionalProperties:[ \t]*false\b`)
	locs := re.FindAllIndex(data, -1)
	if len(locs) == 0 {
		return
	}
	var lines []int
	for _, loc := range locs {
		lines = append(lines, 1+strings.Count(string(data[:loc[0]]), "\n"))
	}
	t.Fatalf("api/openapi.yaml seals %d schema(s) with `additionalProperties: false` at line(s) %v — ADR 0003 forbids this (use typed-but-open; if a composed allOf schema must close, use `unevaluatedProperties: false`).", len(lines), lines)
}
