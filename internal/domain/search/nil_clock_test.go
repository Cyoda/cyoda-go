package search_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/consistency"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

type nilClockTM struct{ spi.TransactionManager }

func TestNewSearchService_NilClockPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("search.NewSearchService with a nil clock did not panic")
		}
	}()
	search.NewSearchService(nil, nil, nil, consistency.New(nilClockTM{}), nil)
}
