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

// knownUncoveredOps are operationIds that have no E2E coverage by design:
// either they're stub-implemented (#194) or mounted outside the generated
// ServerInterface dispatch (transitions handler — #21 design Section 8 noted
// the wart). Audit table at docs/superpowers/audits/2026-04-29-openapi-
// conformance-audit.md captures the full disposition for each.
var knownUncoveredOps = map[string]bool{
	// Stub IAM/account ops — implementation tracked in #194.
	"accountSubscriptionsGet":  true,
	"createTechnicalUser":      true,
	"deleteTechnicalUser":      true,
	"listTechnicalUsers":       true,
	"resetTechnicalUserSecret": true,
	"deleteJwtKeyPair":         true,
	"deleteTrustedKey":         true,
	"getCurrentJwtKeyPair":     true,
	"invalidateJwtKeyPair":     true,
	"invalidateTrustedKey":     true,
	"issueJwtKeyPair":          true,
	"listTrustedKeys":          true,
	"reactivateJwtKeyPair":     true,
	"reactivateTrustedKey":     true,
	"registerTrustedKey":       true,
	"deleteOidcProvider":       true,
	"invalidateOidcProvider":   true,
	"listOidcProviders":        true,
	"reactivateOidcProvider":   true,
	"registerOidcProvider":     true,
	"reloadOidcProviders":      true,
	"updateOidcProvider":       true,
	// Outside the generated ServerInterface dispatch — see #21 design
	// Section 8, Task 5.1's Option B note. Tracked as future cleanup.
	"fetchEntityTransitions": true,
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
			uncovered := []string{}
			for _, op := range allOperationIds {
				if !exercised[op] && !knownUncoveredOps[op] {
					uncovered = append(uncovered, op)
				}
			}
			if len(uncovered) > 0 {
				t.Fatalf("openapi conformance: %d operations have no E2E coverage; see %s",
					len(uncovered), reportPath)
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
