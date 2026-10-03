package help

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// crossReferencedArtefacts are the shipped files outside the help tree that also
// tell a reader to run `cyoda help ...`. api/openapi.yaml is served to every API
// client and rendered in generated docs; api/generated.go carries the same prose
// into every Go consumer's editor tooltips; README.md is the project's front
// door. A dead invocation in any of them costs more than one in a help topic,
// not less, so the guard covers them under the same rule.
//
// api/generated.go is included even though it is generated FROM openapi.yaml.
// Generated output is still shipped output, and leaving it unscanned is exactly
// how a spec fix reads as complete while the artefact every consumer actually
// imports keeps the dead invocation until someone runs `go generate`.
//
// docs/superpowers/** is deliberately out of scope: plans and specs are
// historical records of what was proposed at the time, not living documents.
var crossReferencedArtefacts = []string{
	"api/openapi.yaml",
	"api/generated.go",
	"README.md",
}

// inlineInvocation matches a `cyoda help ...` reference written as
// backtick-delimited inline code — the form used throughout the help
// content, api/openapi.yaml, api/generated.go and README.md. It also
// matches the bare synopsis placeholder (`cyoda help [<topic>...]`), which
// tokenizeHelpArgs below reduces to zero real arguments.
var inlineInvocation = regexp.MustCompile("`cyoda help([^`]*)`")

// fencedBlock matches a ``` ... ``` fenced code block (any language tag, or
// none), to find invocations shown as a whole shell line rather than
// wrapped in inline code (e.g. the EXAMPLES section of cli.help).
var fencedBlock = regexp.MustCompile("(?s)```[^\n]*\n(.*?)\n```")

// fencedLineInvocation matches a fenced-block line that IS a `cyoda help`
// invocation — the line starts with it, optionally after a shell prompt.
// Requiring the line to start with the invocation (rather than searching
// for the phrase anywhere) is what keeps this from firing on a JSON
// example elsewhere in the same file whose "title" or "body" field merely
// contains the words "cyoda help" as prose.
var fencedLineInvocation = regexp.MustCompile(`^\$?\s*cyoda help(?:\s+(.*))?$`)

// helpArgToken is a single legitimate cyoda-help argument: a topic-path
// segment, a dotted topic id, an action name, an error code, or a
// --format=... flag. Anything else — a shell comment (#...), a pipe, a
// line continuation (\), prose punctuation, or placeholder syntax like
// <topic> or [<fmt>] — ends the invocation. What came before it is still
// tested, so a partial-but-real prefix (e.g. just "openapi" out of
// "cyoda help openapi <slug>") is still checked rather than skipped
// outright.
var helpArgToken = regexp.MustCompile(`^[A-Za-z0-9_.=:-]+$`)

// tokenizeHelpArgs turns the text following a `cyoda help` invocation into
// the argument list RunHelp would receive, truncating at the first token
// that isn't a legitimate argument.
func tokenizeHelpArgs(rest string) []string {
	var args []string
	for _, f := range strings.Fields(rest) {
		if !helpArgToken.MatchString(f) {
			break
		}
		args = append(args, f)
	}
	return args
}

// helpInvocation is one `cyoda help ...` reference found in a scanned
// artefact, reduced to the argument list it would pass to RunHelp.
type helpInvocation struct {
	source string // "<file>: cyoda help <args>", for failure messages
	args   []string
}

// extractHelpInvocations finds every `cyoda help ...` reference in body,
// both inline-code and fenced-block forms.
func extractHelpInvocations(name string, body []byte) []helpInvocation {
	text := string(body)
	var found []helpInvocation

	add := func(rest string) {
		args := tokenizeHelpArgs(rest)
		found = append(found, helpInvocation{
			source: name + ": `cyoda help " + strings.Join(args, " ") + "`",
			args:   args,
		})
	}

	for _, m := range inlineInvocation.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, block := range fencedBlock.FindAllStringSubmatch(text, -1) {
		for _, line := range strings.Split(block[1], "\n") {
			if m := fencedLineInvocation.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				add(m[1])
			}
		}
	}
	return found
}

// TestHelpContent_CrossReferencesResolve — a `cyoda help ...` reference in a
// shipped artefact is a command an operator or a see_also link will run. If it
// exits non-zero, the pointer has sent them nowhere, which is worse than not
// having offered one. This resolves every reference found (dotted or spaced,
// including topic actions like "openapi json" and "config all") against the
// real topic tree, rather than asserting a single spelling convention — the
// convention changed (dotted ids are now valid; see 4c6d4003) and a guard tied
// to one spelling broke the moment that changed instead of catching the
// defect it exists for.
func TestHelpContent_CrossReferencesResolve(t *testing.T) {
	var invocations []helpInvocation

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
		invocations = append(invocations, extractHelpInvocations(path, data)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walking embedded help content: %v", err)
	}

	root := repoRoot(t)
	for _, rel := range crossReferencedArtefacts {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		invocations = append(invocations, extractHelpInvocations(rel, data)...)
	}

	var offenders []string
	for _, inv := range invocations {
		var buf bytes.Buffer
		if code := RunHelp(DefaultTree, inv.args, &buf, "test", false, ""); code != 0 {
			offenders = append(offenders, inv.source+" -> exit "+strconv.Itoa(code)+": "+strings.TrimSpace(buf.String()))
		}
	}

	if len(offenders) > 0 {
		t.Fatalf("`cyoda help` references that don't resolve:\n%s", strings.Join(offenders, "\n"))
	}
}

// TestHelpContent_MigrateTopicCarriesTheDirtyRecovery — the postgres and sqlite
// plugins both refuse to start on a dirty schema and both tell the operator to
// run `cyoda help cli migrate` for the recovery procedure. That pointer only
// earns its place if the topic carries one: an operator holding a half-migrated
// database is the least good audience for a dead end.
func TestHelpContent_MigrateTopicCarriesTheDirtyRecovery(t *testing.T) {
	data, err := fs.ReadFile(embeddedContent, "content/cli/migrate.md")
	if err != nil {
		t.Fatalf("read the migrate topic: %v", err)
	}
	body := string(data)

	for _, want := range []string{
		// The refusal an operator arrives with, so the topic is searchable by it.
		"database migration state is dirty",
		// Clearing the flag, which is the whole of the generic recovery.
		"schema_migrations",
		// The PostgreSQL-specific cause and its cleanup.
		"CREATE INDEX CONCURRENTLY",
		"indisvalid",
		"DROP INDEX CONCURRENTLY",
		// The convention the guard in the postgres plugin enforces.
		"## ADDING AN INDEX MIGRATION",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the migrate topic is missing %q, so the refusal's pointer resolves to nothing useful", want)
		}
	}
}
