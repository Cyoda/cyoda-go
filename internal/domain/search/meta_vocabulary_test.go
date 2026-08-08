package search

import (
	"reflect"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// TestMetaVocabulary_EvaluatorMatchesAllowlist pins the invariant
// workflow/validate.go's doc comment asserts but nothing enforced: the meta
// vocabulary this package validates against (spi.MetaFieldNames, plus the
// previousTransition alias) is exactly the vocabulary match.matchLifecycle
// evaluates. internal/match cannot import this package — the reverse import
// already exists — so the evaluator keeps a hand-written second copy of the
// switch and only a test can hold the two in step.
//
// The two directions fail differently, and both are bugs:
//   - allowlist ⊃ evaluator: the boundary accepts a field the evaluator then
//     rejects as unknown, turning a valid-looking condition into a 500.
//   - evaluator ⊃ allowlist: a field that works in-process is refused at the
//     API boundary as an unknown meta filter field.
func TestMetaVocabulary_EvaluatorMatchesAllowlist(t *testing.T) {
	allowed := map[string]bool{"previousTransition": true}
	for _, name := range spi.MetaFieldNames() {
		allowed[name] = true
	}
	want := commontest.SortedKeys(allowed)

	got := commontest.MatchLifecycleFields(t)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("matchLifecycle accepts %v; this package's meta allowlist is %v", got, want)
	}
}
