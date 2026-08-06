package help

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// dottedInvocationPattern matches a cross-reference written as
// `cyoda help <a>.<b>`. The topic tree is addressed by path segments, so the
// dotted form is not a shorter spelling of anything — it exits non-zero with
// "no such topic". A topic ID may legitimately contain a dot (topic: cli.migrate);
// it is only the invocation that must be spelled with a space.
var dottedInvocationPattern = regexp.MustCompile(`cyoda help [0-9A-Za-z_-]+\.[0-9A-Za-z_.-]+`)

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

// TestHelpContent_CrossReferencesUseAWorkingInvocation — a cross-reference in a
// shipped artefact is a command an operator will type. If it exits non-zero, the
// pointer has sent them nowhere, which is worse than not having offered one.
func TestHelpContent_CrossReferencesUseAWorkingInvocation(t *testing.T) {
	var offenders []string

	scan := func(name string, body []byte) {
		for i, line := range strings.Split(string(body), "\n") {
			if m := dottedInvocationPattern.FindString(line); m != "" {
				offenders = append(offenders, name+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(m))
			}
		}
	}

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
		scan(path, data)
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
		scan(rel, data)
	}

	if len(offenders) > 0 {
		t.Fatalf("help topics are addressed by path segments — `cyoda help a b`, not `cyoda help a.b`, "+
			"which exits non-zero with \"no such topic\":\n%s", strings.Join(offenders, "\n"))
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
