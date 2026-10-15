package match

import (
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// operandsToStrings renders each element of a BETWEEN operand slice through
// spi.OperandString (the single shared operand→string normalization).
func operandsToStrings(vs []any) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = spi.OperandString(v)
	}
	return out
}
