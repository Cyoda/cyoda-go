package sqlite_test

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// TestSearcher_RejectsMaliciousFilterPath confirms that user-supplied Filter
// paths containing SQL-injection payloads are rejected at the Search()
// boundary — before they can be interpolated into a json_extract expression.
//
// Regression test for issue #64 (SQLite JSON-path SQL injection via
// Filter/OrderSpec Path). Pre-fix, these payloads reached
// fmt.Sprintf("json_extract(json(meta), '$.%s')", path) and broke out of
// the single-quoted JSON-path literal, injecting arbitrary SQL.
func TestSearcher_RejectsMaliciousFilterPath(t *testing.T) {
	factory, ctx := setupSearcherTest(t)
	store, _ := factory.EntityStore(ctx)
	searcher := store.(spi.Searcher)

	payloads := []string{
		"state')--",
		"state') UNION SELECT 1 --",
		"a'b",
		"a;DROP TABLE entities",
		// NOTE: "a--b" (hyphen) is NOT an injection vector — hyphens are
		// inert inside single-quoted SQLite string literals. It is accepted
		// as a valid JSON key character (see TestValidateJSONPath_AcceptsHyphenatedSegments).
	}

	for _, payload := range payloads {
		_, err := searcher.Search(ctx, spi.Filter{
			Op:     spi.FilterEq,
			Path:   payload,
			Source: spi.SourceData,
			Value:  "x",
		}, spi.SearchOptions{
			ModelName:    "person",
			ModelVersion: "1",
			Limit:        10,
		})
		if err == nil {
			t.Errorf("Search with malicious Filter.Path %q returned nil error (injection not blocked)", payload)
			continue
		}
		if !errors.Is(err, sqlite.ErrInvalidFilterPath) {
			t.Errorf("Search with malicious Filter.Path %q returned err=%v, want wraps ErrInvalidFilterPath", payload, err)
		}
	}
}

// TestSearcher_RejectsMaliciousOrderByPath mirrors the above for OrderSpec.Path,
// which reached orderByFieldExpr unvalidated pre-fix.
func TestSearcher_RejectsMaliciousOrderByPath(t *testing.T) {
	factory, ctx := setupSearcherTest(t)
	store, _ := factory.EntityStore(ctx)
	searcher := store.(spi.Searcher)

	_, err := searcher.Search(ctx, spi.Filter{
		Op:     spi.FilterEq,
		Path:   "city",
		Source: spi.SourceData,
		Value:  "Berlin",
	}, spi.SearchOptions{
		ModelName:    "person",
		ModelVersion: "1",
		Limit:        10,
		OrderBy: []spi.OrderSpec{{
			Path:   "name') --",
			Source: spi.SourceData,
		}},
	})
	if err == nil {
		t.Fatalf("Search with malicious OrderBy.Path returned nil error (injection not blocked)")
	}
	if !errors.Is(err, sqlite.ErrInvalidFilterPath) {
		t.Fatalf("Search with malicious OrderBy.Path returned err=%v, want wraps ErrInvalidFilterPath", err)
	}
}
