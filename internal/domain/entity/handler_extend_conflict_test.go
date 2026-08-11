package entity

import (
	"fmt"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/ingest"
)

// TestClassifyValidateOrExtendErr_ExtendConflict_Is409Retryable — a
// concurrent ExtendSchema on the same model now surfaces spi.ErrConflict
// (the postgres plugin serialises schema writers per (tenant, model) and
// rejects the loser, first committer wins). ValidateOrExtend wraps store
// failures in ErrInternalSchema, whose branch routes through
// common.Internal — which must keep recognising the conflict through
// both wraps and answer a retryable 409, not a 500: the loser's write
// succeeds on retry against a fresh snapshot.
func TestClassifyValidateOrExtendErr_ExtendConflict_Is409Retryable(t *testing.T) {
	// Same double-wrap shape ValidateOrExtend produces around a plugin
	// conflict error.
	underlying := fmt.Errorf("%w: failed to extend schema: %w",
		ingest.ErrInternalSchema,
		fmt.Errorf("serialize schema writers for E/1: %w", spi.ErrConflict))

	appErr := classifyValidateOrExtendErr(underlying)
	if appErr == nil {
		t.Fatal("classifyValidateOrExtendErr returned nil")
	}
	if appErr.Status != http.StatusConflict {
		t.Errorf("status = %d, want %d", appErr.Status, http.StatusConflict)
	}
	if appErr.Code != common.ErrCodeConflict {
		t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeConflict)
	}
	if !appErr.Retryable {
		t.Error("conflict from a concurrent schema extend must be retryable")
	}
}
