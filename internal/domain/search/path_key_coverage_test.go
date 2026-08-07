package search

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func keyTestFields() map[string]schema.FieldDescriptor {
	return map[string]schema.FieldDescriptor{
		"$.amount":       {Path: "$.amount", Types: []spi.DataType{spi.Integer}},
		"$.city":         {Path: "$.city", Types: []spi.DataType{spi.String}},
		"$.address.city": {Path: "$.address.city", Types: []spi.DataType{spi.String}},
		"$.tags":         {Path: "$.tags", Types: []spi.DataType{spi.String}, IsArray: true},
	}
}

// The type-soundness check indexes the FieldsMap, whose keys carry the "$."
// prefix. Looking a condition's path up raw made the check skip any prefix-less
// leaf entirely — so an operand that must be rejected 400 CONDITION_TYPE_MISMATCH
// was accepted and answered with an empty page, while the identical condition
// written "$.amount" was rejected. This is a status-code difference between two
// spellings of the same query.
func TestValidateSimpleConditionType_RejectsBadOperandOnPrefixlessPath(t *testing.T) {
	for _, path := range []string{"amount", "$.amount"} {
		t.Run(path, func(t *testing.T) {
			err := validateSimpleConditionType(keyTestFields(), &predicate.SimpleCondition{
				JsonPath:     path,
				OperatorType: "GREATER_THAN",
				Value:        "not-a-number",
			})
			if err == nil {
				t.Fatal("expected a type mismatch; an unresolved leaf skips the check and answers 200 with an empty page")
			}
			if !errors.Is(err, errConditionTypeMismatch) {
				t.Errorf("expected errConditionTypeMismatch, got %v", err)
			}
		})
	}
}

// The container-path rejection uses the same key and had the same defect.
func TestValidateSimpleConditionType_RejectsContainerOnPrefixlessPath(t *testing.T) {
	for _, path := range []string{"address", "$.address"} {
		t.Run(path, func(t *testing.T) {
			err := validateSimpleConditionType(keyTestFields(), &predicate.SimpleCondition{
				JsonPath:     path,
				OperatorType: "EQUALS",
				Value:        "x",
			})
			if err == nil {
				t.Fatal("expected a container path to be rejected")
			}
			if !errors.Is(err, errInvalidFieldPath) {
				t.Errorf("expected errInvalidFieldPath, got %v", err)
			}
		})
	}
}

// A well-typed operand must still pass, both spellings — the fix must not turn
// the check into a blanket rejection.
func TestValidateSimpleConditionType_AcceptsWellTypedOperand(t *testing.T) {
	for _, path := range []string{"amount", "$.amount"} {
		t.Run(path, func(t *testing.T) {
			if err := validateSimpleConditionType(keyTestFields(), &predicate.SimpleCondition{
				JsonPath:     path,
				OperatorType: "GREATER_THAN",
				Value:        10,
			}); err != nil {
				t.Errorf("well-typed operand rejected: %v", err)
			}
		})
	}
}

// An unknown path carries no type constraint here — the separate field-path
// pass classifies it. Preserved for both spellings.
func TestValidateSimpleConditionType_UnknownPathCarriesNoConstraint(t *testing.T) {
	for _, path := range []string{"nosuch", "$.nosuch"} {
		t.Run(path, func(t *testing.T) {
			if err := validateSimpleConditionType(keyTestFields(), &predicate.SimpleCondition{
				JsonPath:     path,
				OperatorType: "GREATER_THAN",
				Value:        "not-a-number",
			}); err != nil {
				t.Errorf("unknown path must carry no constraint here, got %v", err)
			}
		})
	}
}

// Sort-key resolution prepends "$." by hand, which is not idempotent. HTTP
// strips the prefix before building the key (sortparam.go), but gRPC passes the
// client's path through verbatim (internal/grpc/search.go), so a gRPC client
// sorting by "$.city" produced the lookup key "$.$.city" and a 400 for a field
// that exists — the two transports disagreeing on the same request.
func TestResolveSortKeys_PrefixedPathResolvesOnBothSpellings(t *testing.T) {
	for _, path := range []string{"city", "$.city"} {
		t.Run(path, func(t *testing.T) {
			specs, err := resolveOrderBy([]OrderKey{{Path: path, Source: spi.SourceData}}, keyTestFields())
			if err != nil {
				t.Fatalf("resolveOrderBy(%q): %v", path, err)
			}
			if len(specs) != 1 || specs[0].Path != "city" {
				t.Fatalf("unexpected specs: %+v", specs)
			}
		})
	}
}

func TestResolveSortKeys_UnknownFieldStillRejected(t *testing.T) {
	if _, err := resolveOrderBy([]OrderKey{{Path: "nosuch", Source: spi.SourceData}}, keyTestFields()); err == nil {
		t.Error("expected an unknown sort field to be rejected")
	}
}

func TestResolveSortKeys_ArrayFieldStillRejected(t *testing.T) {
	for _, path := range []string{"tags", "$.tags"} {
		t.Run(path, func(t *testing.T) {
			if _, err := resolveOrderBy([]OrderKey{{Path: path, Source: spi.SourceData}}, keyTestFields()); err == nil {
				t.Error("expected an array sort field to be rejected")
			}
		})
	}
}
