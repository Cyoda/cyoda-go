package search

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// An operator that takes an operand refuses a null or missing one: left
// through, the operand compared as the empty string (EQUALS matched "", and
// CONTAINS / STARTS_WITH matched every string — on a conditional delete,
// every entity with a string at the path). IS_NULL and NOT_NULL take no
// operand and stay accepted without one. Both entry points refuse it with
// ErrInvalidCondition.
func TestValidateCondition_MissingOperandRefusedExceptNullTests(t *testing.T) {
	noOperand := map[string]bool{"IS_NULL": true, "NOT_NULL": true}
	for _, op := range spi.OperatorNames() {
		for _, body := range []string{
			`{"type":"simple","jsonPath":"$.n","operatorType":"` + op + `","value":null}`,
			`{"type":"simple","jsonPath":"$.n","operatorType":"` + op + `"}`,
			`{"type":"lifecycle","field":"state","operatorType":"` + op + `","value":null}`,
			`{"type":"lifecycle","field":"state","operatorType":"` + op + `"}`,
		} {
			c, err := predicate.ParseCondition([]byte(body))
			if err != nil {
				t.Fatalf("%s: parse: %v", body, err)
			}
			for name, err := range map[string]error{
				"ValidateCondition":          ValidateCondition(c),
				"ValidateCriterionCondition": ValidateCriterionCondition(c),
			} {
				if noOperand[op] {
					if err != nil {
						t.Errorf("%s: %s: unexpected error %v", name, body, err)
					}
					continue
				}
				if !errors.Is(err, ErrInvalidCondition) {
					t.Errorf("%s: %s: want ErrInvalidCondition, got %v", name, body, err)
				}
			}
		}
	}
}
