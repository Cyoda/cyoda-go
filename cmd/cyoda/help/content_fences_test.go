package help

import (
	"io/fs"
	"strings"
	"testing"
)

// TestHelpContent_FencesClose: every code fence in a help topic is closed by
// a line holding only the backticks. A closing fence with text after it does
// not close the block, so the rest of the topic renders as code — in the
// terminal, on the web mirror and in the help archive of a release.
func TestHelpContent_FencesClose(t *testing.T) {
	err := fs.WalkDir(embeddedContent, "content", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		data, err := fs.ReadFile(embeddedContent, path)
		if err != nil {
			return err
		}
		open := 0
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "```") {
				continue
			}
			if open == 0 {
				open = i + 1
				continue
			}
			if trimmed != "```" {
				t.Errorf("%s:%d: the code block opened at line %d is closed by a line with text after the backticks: %q", path, i+1, open, trimmed)
			}
			open = 0
		}
		if open != 0 {
			t.Errorf("%s:%d: code block never closed", path, open)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
