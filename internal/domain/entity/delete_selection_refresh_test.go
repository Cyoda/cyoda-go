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

// TestPlanDeleteSelection_SchemaLessModel_RejectsTheDataPath closes a
// divergence between conditional delete and /search/direct.
//
// planDeleteSelection guarded its whole path check with `if fields != nil`,
// citing validateConditionPaths as doing the same. It no longer does: a model
// declaring no fields is a model in which the named path does not exist, and
// search answers 400 INVALID_FIELD_PATH. Delete accepted the path and then
// translated the condition against a nil fields map — which does not make the
// filter inert, it makes it SKEWED, because an empty declared-type set
// annihilates the comparison operators while the string operators keep
// matching.
//
// That skew decides which rows get DELETED. Of the three endpoints sharing
// this contract, this is the one where guessing is least acceptable.
func TestPlanDeleteSelection_SchemaLessModel_RejectsTheDataPath(t *testing.T) {
	h := &Handler{}
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "1"}
	// A descriptor carrying no schema at all — not an empty schema.
	bare := &spi.ModelDescriptor{Ref: ref, State: spi.ModelLocked}
	ms := &refreshingStore{
		getQueue:     []*spi.ModelDescriptor{bare},
		refreshQueue: []*spi.ModelDescriptor{bare},
	}

	cond, err := predicate.ParseCondition([]byte(`{"type":"simple","jsonPath":"$.name","operatorType":"EQUALS","value":"x"}`))
	if err != nil {
		t.Fatalf("ParseCondition: %v", err)
	}

	_, planErr := h.planDeleteSelection(context.Background(), ms, ref, cond)
	var appErr *common.AppError
	if !errors.As(planErr, &appErr) {
		t.Fatalf("conditional delete accepted a data path against a model declaring "+
			"no fields (err = %v); /search/direct rejects it 400 INVALID_FIELD_PATH", planErr)
	}
	if appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeInvalidFieldPath)
	}
}
