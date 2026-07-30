package help

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// The `workflows` CRITERIA section and the `search` Condition-DSL section
// describe the same evaluation kernel (predicate.ParseCondition ->
// match.Match). These guards pin both topics to what that kernel accepts, so
// the two cannot drift apart, or away from the code, again.
//
// The lifecycle-field vocabulary is taken from match.matchLifecycle because
// that is the evaluator the topics describe. Its agreement with
// search.sortableMetaFields — the allowlist the API boundary and
// workflow-criterion import validate against, and the documented source of
// truth for the vocabulary — is pinned separately by
// TestMetaVocabulary_EvaluatorMatchesAllowlist.

var (
	// backticked captures each `token` in a markdown span.
	backticked = regexp.MustCompile("`([A-Za-z]+)`")
	// searchConditionHeading captures the **XCondition** headings that form
	// the search topic's condition-type catalogue.
	searchConditionHeading = regexp.MustCompile(`(?m)^\*\*([A-Za-z]+)Condition\*\*`)
)

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
	return commontest.SortedKeys(seen)
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
	accepted := commontest.MatchLifecycleFields(t)

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
	if want := commontest.SortedKeys(catalogued); !reflect.DeepEqual(documented, want) {
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
