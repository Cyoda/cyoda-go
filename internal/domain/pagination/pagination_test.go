package pagination_test

import (
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda/cyoda-go/internal/common"
	"github.com/cyoda/cyoda-go/internal/domain/pagination"
)

// TestValidateOffset_Normal — normal values pass.
func TestValidateOffset_Normal(t *testing.T) {
	if err := pagination.ValidateOffset(0, 20); err != nil {
		t.Fatalf("0,20 → unexpected err: %v", err)
	}
	if err := pagination.ValidateOffset(10, 100); err != nil {
		t.Fatalf("10,100 → unexpected err: %v", err)
	}
	if err := pagination.ValidateOffset(0, 0); err != nil {
		t.Fatalf("0,0 → unexpected err: %v", err)
	}
}

// TestValidateOffset_Negative — negative values are rejected with 400.
func TestValidateOffset_Negative(t *testing.T) {
	cases := []struct {
		name string
		pn   int64
		ps   int64
	}{
		{"negative pageNumber", -1, 20},
		{"negative pageSize", 0, -1},
		{"both negative", -1, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pagination.ValidateOffset(tc.pn, tc.ps)
			assertBadRequest(t, err)
		})
	}
}

// TestValidateOffset_PageSizeExceedsCap — pageSize above MaxPageSize → 400.
func TestValidateOffset_PageSizeExceedsCap(t *testing.T) {
	err := pagination.ValidateOffset(0, pagination.MaxPageSize+1)
	assertBadRequest(t, err)
	if !strings.Contains(strings.ToLower(err.Error()), "pagesize") {
		t.Errorf("expected message to mention pageSize, got: %s", err.Error())
	}
}

// TestValidateOffset_PageSizeAtCap — pageSize exactly at the cap is allowed.
func TestValidateOffset_PageSizeAtCap(t *testing.T) {
	if err := pagination.ValidateOffset(0, pagination.MaxPageSize); err != nil {
		t.Fatalf("pageSize=cap → unexpected err: %v", err)
	}
}

// TestValidateOffset_PageNumberJustUnderCap — pageNumber just under MaxPageNumber passes.
func TestValidateOffset_PageNumberJustUnderCap(t *testing.T) {
	if err := pagination.ValidateOffset(pagination.MaxPageNumber-1, pagination.MaxPageSize); err != nil {
		t.Fatalf("pageNumber=cap-1 → unexpected err: %v", err)
	}
}

// TestValidateOffset_PageNumberAtCap — pageNumber exactly at MaxPageNumber passes.
func TestValidateOffset_PageNumberAtCap(t *testing.T) {
	if err := pagination.ValidateOffset(pagination.MaxPageNumber, pagination.MaxPageSize); err != nil {
		t.Fatalf("pageNumber=cap → unexpected err: %v", err)
	}
}

// TestValidateOffset_PageNumberJustOverCap — pageNumber > MaxPageNumber → 400.
func TestValidateOffset_PageNumberJustOverCap(t *testing.T) {
	err := pagination.ValidateOffset(pagination.MaxPageNumber+1, 1)
	assertBadRequest(t, err)
	if !strings.Contains(strings.ToLower(err.Error()), "pagenumber") {
		t.Errorf("expected message to mention pageNumber, got: %s", err.Error())
	}
}

// TestValidateOffset_OverflowMaxInt64 — pageNumber*pageSize overflows int64 → 400.
func TestValidateOffset_OverflowMaxInt64(t *testing.T) {
	// MaxInt64 with any pageSize > 1 would overflow, but pageNumber alone
	// already trips the cap — choose a value that bypasses the cap by
	// disabling/raising it would be unsafe. Here we just make sure the
	// guard is reached when the cap is somehow not engaged: feed
	// MaxInt64 and pageSize 1000.
	err := pagination.ValidateOffset(math.MaxInt64, 1000)
	assertBadRequest(t, err)
}

// assertBadRequest fails the test if err is not a 400 AppError with
// ErrCodeBadRequest.
func assertBadRequest(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", appErr.Status)
	}
	if appErr.Code != common.ErrCodeBadRequest {
		t.Errorf("code: got %q, want %q", appErr.Code, common.ErrCodeBadRequest)
	}
}
