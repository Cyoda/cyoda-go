// Filename intentionally starts with "zzz_" so this file processes LAST in
// alphabetical ordering — Go runs tests in source-declaration order within
// a file, processing files in alphabetical filename order. Function name
// has no effect on ordering. See
// docs/superpowers/specs/2026-04-29-issue-21-openapi-conformance-design.md
// Section 2 for the rationale.

package e2e_test

import (
	"flag"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
)

// uncoveredOps computes two disjoint problem sets from the full operation
// list:
//   - unmarkedUncovered: ops that were neither exercised by an E2E test nor
//     carry an x-cyoda-status marker (silent 404 or untested live op).
//   - staleMarkers: ops that carry an x-cyoda-status marker but returned a
//     2xx response during the run (marker is stale — the op is now live).
func uncoveredOps(all []string, exercised, success2xx map[string]bool, marked map[string]string) (unmarkedUncovered, staleMarkers []string) {
	for _, op := range all {
		_, isMarked := marked[op]
		switch {
		case !exercised[op] && !isMarked:
			unmarkedUncovered = append(unmarkedUncovered, op) // silent 404 or untested live op
		case isMarked && success2xx[op]:
			staleMarkers = append(staleMarkers, op) // marker on an op that is actually live
		}
	}
	return
}

// TestOpenAPIConformanceReport runs after every other E2E test, drains the
// validator's collector, writes the markdown report, and (in ModeEnforce)
// fails if any mismatches were collected.
func TestOpenAPIConformanceReport(t *testing.T) {
	// `-shuffle on` defeats the file-ordering trick that ensures this test
	// runs last. Detect and bail out cleanly.
	if v := flag.Lookup("test.shuffle"); v != nil && v.Value.String() != "off" {
		t.Fatalf("openapi conformance suite is not compatible with -shuffle; rerun without it")
	}

	mismatches, exercised := openapivalidator.DrainAndExercised()
	reportPath := filepath.Join("_openapi-conformance-report.md")
	if err := openapivalidator.WriteReport(reportPath, mismatches, exercised, allOperationIds); err != nil {
		t.Fatalf("write report: %v", err)
	}

	t.Logf("openapi conformance report: %s (%d mismatches)", reportPath, len(mismatches))

	if openapivalidator.Mode != openapivalidator.ModeEnforce {
		// Record mode: report-only.
		return
	}

	if len(mismatches) == 0 {
		// Enforce mode, no mismatches: also check coverage. Skip the
		// coverage check when -run is set (single-test workflow).
		if !openapivalidator.RunFilterActive() {
			unmarked, stale := uncoveredOps(allOperationIds, exercised, openapivalidator.Success2xxSet(), markedOps)
			if len(unmarked) > 0 {
				t.Fatalf("openapi conformance: %d published ops are neither exercised nor x-cyoda-status-marked (silent 404 or untested live op): %v", len(unmarked), unmarked)
			}
			if len(stale) > 0 {
				t.Fatalf("openapi conformance: %d x-cyoda-status-marked ops returned 2xx (marker is stale — op is live): %v", len(stale), stale)
			}
		}
		return
	}

	// Enforce mode, mismatches present: fail with summary of first 20.
	limit := len(mismatches)
	if limit > 20 {
		limit = 20
	}
	var summary string
	for _, m := range mismatches[:limit] {
		summary += fmt.Sprintf("\n  %s %s -> %d: %s", m.Method, m.Path, m.Status, m.Reason)
	}
	t.Fatalf("openapi conformance: %d mismatches (first %d shown); full report at %s%s",
		len(mismatches), limit, reportPath, summary)
}
