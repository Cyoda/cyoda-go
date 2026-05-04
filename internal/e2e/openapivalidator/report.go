package openapivalidator

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// WriteReport renders the conformance report to path. Returns nil even if
// there are mismatches — the report file always reflects the current state;
// failure handling is the caller's responsibility.
func WriteReport(path string, mismatches []Mismatch, exercised map[string]bool, allOps []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# OpenAPI Conformance Report\n\n")
	fmt.Fprintf(&b, "Generated: %s\n\n", time.Now().UTC().Format(time.RFC3339))

	fmt.Fprintf(&b, "## Mismatches (%d)\n\n", len(mismatches))
	if len(mismatches) == 0 {
		fmt.Fprintln(&b, "_None._")
		fmt.Fprintln(&b)
	} else {
		// Group by operation for readability.
		byOp := map[string][]Mismatch{}
		for _, m := range mismatches {
			byOp[m.Operation] = append(byOp[m.Operation], m)
		}
		ops := make([]string, 0, len(byOp))
		for op := range byOp {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		for _, op := range ops {
			fmt.Fprintf(&b, "### %s\n\n", op)
			for _, m := range byOp[op] {
				fmt.Fprintf(&b, "- `%s %s -> %d`", m.Method, m.Path, m.Status)
				if m.TestName != "" && m.TestName != "unknown" {
					fmt.Fprintf(&b, " (test: `%s`)", m.TestName)
				}
				fmt.Fprintf(&b, "\n  - %s\n", m.Reason)
			}
			fmt.Fprintln(&b)
		}
	}

	uncovered := []string{}
	for _, op := range allOps {
		if !exercised[op] {
			uncovered = append(uncovered, op)
		}
	}
	sort.Strings(uncovered)
	fmt.Fprintf(&b, "## Uncovered Operations (%d)\n\n", len(uncovered))
	if len(uncovered) == 0 {
		fmt.Fprintln(&b, "_All declared operations were exercised._")
	} else {
		for _, op := range uncovered {
			fmt.Fprintf(&b, "- %s\n", op)
		}
	}

	return os.WriteFile(path, []byte(b.String()), 0o644)
}
