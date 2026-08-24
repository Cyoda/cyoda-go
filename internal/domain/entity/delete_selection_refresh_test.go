package entity

import (
	"context"
	"errors"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// TestPlanDeleteSelection_RefreshableUnknownPath_Succeeds pins the fix for
// the final-review finding that a conditional delete lost the bounded
// schema-refresh SearchService.Search's validateConditionPaths has, and so
// falsely 400ed on a field a peer node had already extended the model with
// (multi-node is this project's primary target — see
// .claude/rules/multi-node-primary.md). planDeleteSelection's cached
// descriptor map only knows field "a"; the store's RefreshAndGet reveals a
// second field "b" the condition references. One bounded refresh must be
// enough to accept it — exactly what /search/direct already gets via
// validateConditionPaths.
func TestPlanDeleteSelection_RefreshableUnknownPath_Succeeds(t *testing.T) {
	h := &Handler{}
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	stale := buildDescriptorWithFields(t, ref, "a")
	fresh := buildDescriptorWithFields(t, ref, "a", "b")
	ms := &refreshingStore{
		getQueue:     []*spi.ModelDescriptor{stale},
		refreshQueue: []*spi.ModelDescriptor{fresh},
	}

	cond, err := predicate.ParseCondition([]byte(`{"type":"simple","jsonPath":"$.b","operatorType":"EQUALS","value":"x"}`))
	if err != nil {
		t.Fatalf("ParseCondition: %v", err)
	}

	_, planErr := h.planDeleteSelection(context.Background(), ms, ref, cond)
	if planErr != nil {
		t.Fatalf("expected the refreshable field path to be accepted, got %v", planErr)
	}
	if ms.RefreshCount() != 1 {
		t.Errorf("expected exactly 1 refresh (bounded), got %d", ms.RefreshCount())
	}
}

// TestPlanDeleteSelection_GenuinelyUnknownPath_StillRejects is the negative
// counterpart: a field absent from BOTH the cached and the refreshed
// descriptor is still rejected as INVALID_FIELD_PATH, and the refresh
// remains bounded to one attempt.
func TestPlanDeleteSelection_GenuinelyUnknownPath_StillRejects(t *testing.T) {
	h := &Handler{}
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	stale := buildDescriptorWithFields(t, ref, "a")
	stillStale := buildDescriptorWithFields(t, ref, "a")
	ms := &refreshingStore{
		getQueue:     []*spi.ModelDescriptor{stale},
		refreshQueue: []*spi.ModelDescriptor{stillStale},
	}

	cond, err := predicate.ParseCondition([]byte(`{"type":"simple","jsonPath":"$.doesNotExist","operatorType":"EQUALS","value":"x"}`))
	if err != nil {
		t.Fatalf("ParseCondition: %v", err)
	}

	_, planErr := h.planDeleteSelection(context.Background(), ms, ref, cond)
	var appErr *common.AppError
	if !errors.As(planErr, &appErr) {
		t.Fatalf("got err %v, want *common.AppError", planErr)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", appErr.Status, http.StatusBadRequest)
	}
	if appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Errorf("code = %s, want %s", appErr.Code, common.ErrCodeInvalidFieldPath)
	}
	if ms.RefreshCount() != 1 {
		t.Errorf("expected exactly 1 refresh (bounded), got %d", ms.RefreshCount())
	}
}
