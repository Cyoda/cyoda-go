package search

import (
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// An AppError that GetAsyncResults already classified reaches the door as
// itself, not wrapped in a second "job lookup failed" internal error.
func TestJobLookupError_PassesAppErrorThrough(t *testing.T) {
	inner := common.Internal("failed to read an async search result",
		fmt.Errorf("entity id-2: %w", spi.ErrNotFound))
	got := jobLookupError(fmt.Errorf("page: %w", inner))
	if got != inner {
		t.Fatalf("jobLookupError wrapped the AppError: got %q, want %q", got.Message, inner.Message)
	}
	if errors.Is(got, ErrSearchJobNotFound) {
		t.Errorf("result-read failure reported as a missing job")
	}
}
