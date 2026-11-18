package help

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// quotedText and trailingComment find a ` #` comment after a command, outside
// any quoted text.
var (
	quotedText      = regexp.MustCompile(`'[^']*'|"[^"]*"`)
	trailingComment = regexp.MustCompile(`\s#(\s|$)`)
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

// TestHelpContent_ShellCommentsPasteInZsh: a comment line in a shell code
// block holds no unbalanced single quote and no command substitution. Interactive zsh does not treat `#`
// as a comment unless INTERACTIVE_COMMENTS is set, so an apostrophe there
// ("kubectl's") opens a quote that never closes, and the pasted block stops
// at the `quote>` prompt without running the commands after it; and a
// backtick or `$(` in a comment line runs the command it names, a `;`, `|`
// or `&` runs the words after it, and `<` or `>` redirects. A comment after a
// command is passed to the command as arguments.
func TestHelpContent_ShellCommentsPasteInZsh(t *testing.T) {
	shellLang := map[string]bool{"": true, "bash": true, "sh": true, "shell": true, "zsh": true, "console": true}
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
		inBlock, shell := false, false
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "```") {
				if !inBlock {
					inBlock, shell = true, shellLang[strings.TrimPrefix(trimmed, "```")]
				} else {
					inBlock = false
				}
				continue
			}
			if !inBlock || !shell {
				continue
			}
			if !strings.HasPrefix(trimmed, "#") {
				if trailingComment.MatchString(quotedText.ReplaceAllString(trimmed, "")) {
					t.Errorf("%s:%d: a comment after a command is passed to that command as arguments in interactive zsh: %q", path, i+1, trimmed)
				}
				continue
			}
			if strings.ContainsAny(trimmed, ";|&<>") {
				t.Errorf("%s:%d: a comment line with ; | & < or > runs or redirects when pasted into interactive zsh: %q", path, i+1, trimmed)
			}
			if strings.Count(trimmed, "'")%2 == 1 || strings.Count(trimmed, `"`)%2 == 1 {
				t.Errorf("%s:%d: a comment line with an unbalanced quote breaks a paste into interactive zsh: %q", path, i+1, trimmed)
			}
			if strings.Contains(trimmed, "`") || strings.Contains(trimmed, "$(") {
				t.Errorf("%s:%d: a comment line with a command substitution runs that command when pasted into interactive zsh: %q", path, i+1, trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
