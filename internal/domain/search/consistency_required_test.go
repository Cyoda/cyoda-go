package search_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

func TestNewSearchService_NilConsistencyPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("search.NewSearchService with a nil consistency service did not panic")
		}
	}()
	search.NewSearchService(nil, nil, nil, nil)
}
