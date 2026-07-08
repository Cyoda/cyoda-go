package help

import (
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// issueRefPattern matches an issue reference that must never appear in shipped
// help content (see .claude rule "No issue IDs in shipped artefacts"):
//
//   - a "#<digits>" issue reference: the "#" must be preceded by start-of-line
//     or a non-word character, so genuine tokens like "PKCS#8" / "PKCS#1"
//     (alphanumeric immediately before "#") are NOT flagged; and
//   - a "/issues/<n>" issue-tracker URL fragment.
//
// (?m) makes "^" match the start of each line so a "#123" at line start is caught.
var issueRefPattern = regexp.MustCompile(`(?m)(^|[^0-9A-Za-z_])#[0-9]+|/issues/[0-9]+`)

// TestHelpContent_NoIssueIDs is a Gate-6 regression guard. Issue IDs (#NNN /
// /issues/NNN) are reserved for PR bodies, commits, and spec docs — they must
// never leak into user-facing shipped artefacts such as embedded help topics.
// A leak was fixed once; this test fails the build if any reappears.
func TestHelpContent_NoIssueIDs(t *testing.T) {
	var offenders []string

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
		for i, line := range strings.Split(string(data), "\n") {
			if m := issueRefPattern.FindString(line); m != "" {
				offenders = append(offenders, path+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line)+" (matched "+strings.TrimSpace(m)+")")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking embedded help content: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("shipped help content must not contain issue IDs (#NNN or /issues/NNN); "+
			"move the reference to the PR/commit/spec instead.\n%s", strings.Join(offenders, "\n"))
	}
}
