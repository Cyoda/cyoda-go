// Package renderer provides the glamour-based markdown renderer and the
// content-discipline linter for the cyoda help subsystem.
// See /docs/superpowers/specs/2026-04-23-cyoda-help-subsystem-design.md
// §Supported markdown subset.
//
// CLI output from this package uses fmt.Fprint to injected writers.
// This is NOT operational logging — the project's log/slog-exclusive
// rule does not apply here.
package renderer

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// Issue describes a disallowed-markdown site.
type Issue struct {
	Line int
	Kind string
	Text string
}

// String formats an Issue for human-readable output.
func (i Issue) String() string { return fmt.Sprintf("line %d: %s (%q)", i.Line, i.Kind, i.Text) }

// FindUnsupported scans for disallowed markdown constructs. Returns
// non-empty issues when any are present.
func FindUnsupported(src []byte) []Issue {
	var issues []Issue
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	lineNum := 0
	inFenced := false
	for sc.Scan() {
		lineNum++
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFenced = !inFenced
			continue
		}
		if inFenced {
			continue
		}
		// Pipe table rows.
		if strings.HasPrefix(trimmed, "|") && strings.Count(trimmed, "|") >= 2 {
			issues = append(issues, Issue{Line: lineNum, Kind: "pipe table", Text: line})
			continue
		}
		// HTML blocks.
		if strings.HasPrefix(trimmed, "<") && strings.HasSuffix(trimmed, ">") && len(trimmed) > 2 {
			issues = append(issues, Issue{Line: lineNum, Kind: "html block", Text: line})
			continue
		}
		// Nested list (indented bullet or numbered item).
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			t := strings.TrimLeft(line, " \t")
			if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || isNumberedItem(t) {
				issues = append(issues, Issue{Line: lineNum, Kind: "nested list", Text: line})
				continue
			}
		}
		// Blockquote.
		if strings.HasPrefix(trimmed, "> ") {
			issues = append(issues, Issue{Line: lineNum, Kind: "blockquote", Text: line})
			continue
		}
	}
	// sc.Err() is intentionally not checked; the source is always a
	// bytes.Reader (we control the call sites), which cannot produce
	// an I/O error.
	return issues
}

// isNumberedItem reports whether s starts with a numbered list marker:
// one or more digits, a period, and a space.
func isNumberedItem(s string) bool {
	digits := len(s) - len(strings.TrimLeft(s, "0123456789"))
	return digits > 0 && strings.HasPrefix(s[digits:], ". ")
}
