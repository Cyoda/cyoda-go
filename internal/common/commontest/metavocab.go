package commontest

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// The lifecycle (meta) field vocabulary exists in three places that must agree:
// search.sortableMetaFields (the allowlist the search boundary and
// workflow-criterion import validate against), match.Prepare's
// prepareLifecycle switch (compiled once per query; (Prepared).Match then
// evaluates the compiled leaf per row), and the `workflows` / `search` help
// topics. internal/match cannot import internal/domain/search — the reverse
// import already exists — so the evaluator's copy is hand-kept and only a
// test can hold the three in step. This helper supplies the evaluator's copy,
// read out of its source, to whichever package is doing the comparing.
var (
	// prepareLifecycleFunc isolates the body of prepareLifecycle. The body's
	// closing brace is the only "}" at column 0 after the signature, so the
	// non-greedy match stops there.
	prepareLifecycleFunc = regexp.MustCompile(`(?s)func prepareLifecycle\(.*?\n}`)
	// caseClause captures everything between `case ` and its colon, so a
	// multi-label clause (`case "a", "b":`) yields both labels rather than
	// silently matching neither.
	caseClause = regexp.MustCompile(`case ([^\n:]+):`)
	// quoted captures each string literal within a case clause.
	quoted = regexp.MustCompile(`"([^"]*)"`)
	// lifecycleAlias captures the `field == "x"` -> `field = "y"` alias
	// normalisation performed on entry.
	lifecycleAlias = regexp.MustCompile(`field == "([^"]+)"\s*\{\s*field = "([^"]+)"`)
)

// MatchLifecycleFields returns, sorted, every lifecycle field name
// match.Prepare's prepareLifecycle accepts: each label of its switch plus
// each alias it normalises on entry. Derived from the source rather than
// exercised through the function, so that a field the evaluator gained but
// nobody documented is visible to the caller instead of being invisible to an
// enumeration test.
func MatchLifecycleFields(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(RepoRoot(t), "internal/match/prepared.go"))
	if err != nil {
		t.Fatalf("read prepared.go: %v", err)
	}
	body := prepareLifecycleFunc.Find(src)
	if body == nil {
		t.Fatal("prepareLifecycle not found in internal/match/prepared.go")
	}
	fields := map[string]bool{}
	for _, clause := range caseClause.FindAllStringSubmatch(string(body), -1) {
		for _, label := range quoted.FindAllStringSubmatch(clause[1], -1) {
			fields[label[1]] = true
		}
	}
	for _, m := range lifecycleAlias.FindAllStringSubmatch(string(body), -1) {
		if !fields[m[2]] {
			t.Errorf("alias %q normalises to %q, which is not a switch case", m[1], m[2])
		}
		fields[m[1]] = true
	}
	if len(fields) == 0 {
		t.Fatal("no lifecycle fields extracted from prepareLifecycle")
	}
	return SortedKeys(fields)
}

// SortedKeys returns the keys of a set in sorted order, for stable comparison
// and readable failure messages.
func SortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RepoRoot walks up from the working directory to the directory holding
// go.mod. Skips the test if there is none.
func RepoRoot(t *testing.T) string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(root, "go.mod")); statErr == nil {
			return root
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Skip("cannot locate repo root; test skipped")
			return ""
		}
		root = parent
	}
}
