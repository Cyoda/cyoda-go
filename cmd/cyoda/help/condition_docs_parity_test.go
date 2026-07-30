package help

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// The `workflows` CRITERIA section and the `search` Condition-DSL section
// describe the same evaluation kernel (predicate.ParseCondition ->
// match.Match). These guards pin both topics to the source of truth so the
// two cannot drift apart, or away from the code, again.

var (
	// matchLifecycleFunc isolates the body of matchLifecycle in
	// internal/match/match.go. The body's closing brace is the only "}" at
	// column 0 after the signature, so the non-greedy match stops there.
	matchLifecycleFunc = regexp.MustCompile(`(?s)func matchLifecycle\(.*?\n}`)
	// caseLabel captures each `case "field":` label of that switch — the
	// canonical lifecycle-field vocabulary.
	caseLabel = regexp.MustCompile(`case "([A-Za-z]+)":`)
	// lifecycleAlias captures the `field == "x"` -> `field = "y"` alias
	// normalisation performed on entry.
	lifecycleAlias = regexp.MustCompile(`field == "([A-Za-z]+)"\s*\{\s*field = "([A-Za-z]+)"`)
	// backticked captures each `token` in a markdown span.
	backticked = regexp.MustCompile("`([A-Za-z]+)`")
	// searchConditionHeading captures the **XCondition** headings that form
	// the search topic's condition-type catalogue.
	searchConditionHeading = regexp.MustCompile(`(?m)^\*\*([A-Za-z]+)Condition\*\*`)
)

// acceptedLifecycleFields derives the lifecycle (meta) field vocabulary from
// matchLifecycle: every switch case, plus the alias it normalises on entry.
func acceptedLifecycleFields(t *testing.T, root string) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, "internal/match/match.go"))
	if err != nil {
		t.Fatalf("read match.go: %v", err)
	}
	body := matchLifecycleFunc.Find(src)
	if body == nil {
		t.Fatal("matchLifecycle not found in internal/match/match.go")
	}
	fields := map[string]bool{}
	for _, m := range caseLabel.FindAllStringSubmatch(string(body), -1) {
		fields[m[1]] = true
	}
	for _, m := range lifecycleAlias.FindAllStringSubmatch(string(body), -1) {
		if !fields[m[2]] {
			t.Errorf("alias %q normalises to %q, which is not a switch case", m[1], m[2])
		}
		fields[m[1]] = true
	}
	if len(fields) == 0 {
		t.Fatal("no lifecycle fields extracted from matchLifecycle")
	}
	return sortedKeys(fields)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// topicSentence returns the backticked tokens listed after marker in body, up
// to the end of that sentence. Doc lists are written as a run of `code` spans
// terminated by ". " (prose) or by the end of the line (a markdown list item),
// whichever comes first.
func topicSentence(t *testing.T, body, marker, topic string) []string {
	t.Helper()
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("%s: marker %q not found", topic, marker)
	}
	rest := body[i+len(marker):]
	for _, terminator := range []string{". ", "\n"} {
		if end := strings.Index(rest, terminator); end >= 0 {
			rest = rest[:end]
		}
	}
	seen := map[string]bool{}
	for _, m := range backticked.FindAllStringSubmatch(rest, -1) {
		seen[m[1]] = true
	}
	if len(seen) == 0 {
		t.Fatalf("%s: no `code` tokens after %q", topic, marker)
	}
	return sortedKeys(seen)
}

func readTopic(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "cmd/cyoda/help/content", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// TestHelpTopics_LifecycleFieldParity asserts that the lifecycle-field list in
// the `workflows` CRITERIA section and in the `search` LifecycleCondition
// section both match the field vocabulary matchLifecycle actually accepts.
// A criterion and a search filter share one evaluator, so a reader of either
// topic must see the same six fields plus the previousTransition alias.
func TestHelpTopics_LifecycleFieldParity(t *testing.T) {
	root := repoRoot(t)
	accepted := acceptedLifecycleFields(t, root)

	workflows := topicSentence(t,
		readTopic(t, root, "workflows.md"),
		"`lifecycle` criteria match entity metadata fields:",
		"workflows.md")
	if !reflect.DeepEqual(workflows, accepted) {
		t.Errorf("workflows.md documents lifecycle fields %v; matchLifecycle accepts %v", workflows, accepted)
	}

	search := topicSentence(t,
		readTopic(t, root, "search.md"),
		"- `field`:",
		"search.md")
	if !reflect.DeepEqual(search, accepted) {
		t.Errorf("search.md documents lifecycle fields %v; matchLifecycle accepts %v", search, accepted)
	}
}

// TestHelpTopics_ConditionTypeParity asserts the `workflows` CRITERIA section
// lists exactly the condition types the `search` topic catalogues, and that
// every one of them is a type ParseCondition accepts.
func TestHelpTopics_ConditionTypeParity(t *testing.T) {
	root := repoRoot(t)

	documented := topicSentence(t,
		readTopic(t, root, "workflows.md"),
		"condition types are supported:",
		"workflows.md")

	catalogued := map[string]bool{}
	for _, m := range searchConditionHeading.FindAllStringSubmatch(readTopic(t, root, "search.md"), -1) {
		catalogued[strings.ToLower(m[1])] = true
	}
	if len(catalogued) == 0 {
		t.Fatal("search.md: no **XCondition** headings found")
	}
	if want := sortedKeys(catalogued); !reflect.DeepEqual(documented, want) {
		t.Errorf("workflows.md lists condition types %v; search.md catalogues %v", documented, want)
	}

	for _, typ := range documented {
		if _, err := predicate.ParseCondition([]byte(`{"type":"` + typ + `"}`)); err != nil {
			t.Errorf("condition type %q documented but rejected by ParseCondition: %v", typ, err)
		}
	}
	if _, err := predicate.ParseCondition([]byte(`{"type":"no-such-condition"}`)); err == nil {
		t.Error("ParseCondition accepted an unknown condition type; the acceptance check above proves nothing")
	}
}
