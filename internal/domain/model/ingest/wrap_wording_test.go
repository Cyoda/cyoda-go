package ingest

import (
	"context"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// Final review M4: Extend can fail for reasons that are not a change-level
// refusal at all — a document nested past MaxValidationDepth, in particular
// — and ValidateOrExtend's non-level wrap ("change level violation: %w")
// mislabeled every one of them, which is dishonest: the remedy for "your
// document is too deeply nested" is nothing like the remedy for "raise your
// changeLevel". Only a genuine changeLevelError refusal should wear that
// label; anything else gets a neutral wrap.
func TestValidateOrExtend_NonLevelFailureIsNotLabeledChangeLevel(t *testing.T) {
	b, err := schema.Marshal(schema.NewObjectNode())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	desc := &spi.ModelDescriptor{
		Ref:         spi.ModelRef{EntityName: "wrap-wording", ModelVersion: "1"},
		Schema:      b,
		ChangeLevel: spi.ChangeLevelStructural,
	}

	var doc any = "leaf"
	for i := 0; i < schema.MaxValidationDepth+5; i++ {
		doc = map[string]any{"nested": doc}
	}
	root := map[string]any{"a": doc}

	_, err = ValidateOrExtend(context.Background(), &recordingModelStore{}, desc, root)
	if err == nil {
		t.Fatal("want an error for a document nested past MaxValidationDepth")
	}
	if strings.Contains(err.Error(), "change level violation") {
		t.Errorf("a depth-exceeded failure must not wear the change-level label: %v", err)
	}
}

// The genuine case — a change that costs more than the configured level
// permits — keeps the "change level violation" label; only non-level
// failures were mislabeled.
func TestValidateOrExtend_GenuineLevelViolationKeepsItsLabel(t *testing.T) {
	b, err := schema.Marshal(schema.NewObjectNode())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	desc := &spi.ModelDescriptor{
		Ref:         spi.ModelRef{EntityName: "wrap-wording-level", ModelVersion: "1"},
		Schema:      b,
		ChangeLevel: spi.ChangeLevelArrayLength, // the most restrictive level
	}

	_, err = ValidateOrExtend(context.Background(), &recordingModelStore{}, desc, map[string]any{"amount": "y"})
	if err == nil {
		t.Fatal("want a change-level violation for a new field at the most restrictive level")
	}
	if !strings.Contains(err.Error(), "change level violation") {
		t.Errorf("a genuine change-level violation must keep its label: %v", err)
	}
}
