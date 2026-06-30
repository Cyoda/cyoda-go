package schema_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// TestInferDataType_LargeExponent_DoS reproduces the pre-fix hang:
// a huge-exponent number like 1e1000000000 previously triggered
// big.Int.Exp(10, 1_000_000_000, nil) — materialising a billion-digit
// integer and hanging the server on any entity write containing such a
// value (authenticated-client DoS).  The fix bounds the digit count
// BEFORE materialising and returns UnboundInteger directly.
func TestInferDataType_LargeExponent_DoS(t *testing.T) {
	ch := make(chan schema.DataType, 1)
	go func() {
		ch <- schema.InferDataType(json.Number("1e1000000000"))
	}()
	select {
	case got := <-ch:
		if got != schema.UnboundInteger {
			t.Errorf("InferDataType(1e1000000000) = %s, want UnboundInteger", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inferDataType hung on large exponent (DoS: big.Int.Exp not guarded)")
	}
}

// TestInferDataType_BoundaryValues checks that the magnitude guard does
// not disturb classification of in-range numbers, and that values at and
// above the 40-digit threshold are classified as UnboundInteger.
func TestInferDataType_BoundaryValues(t *testing.T) {
	tests := []struct {
		input string
		want  schema.DataType
	}{
		// Normal 32-bit integers.
		{"0", schema.Integer},
		{"42", schema.Integer},
		{"-2147483648", schema.Integer}, // Int32 min.
		{"2147483647", schema.Integer},  // Int32 max.
		// Crosses into Long.
		{"2147483648", schema.Long},
		{"9223372036854775807", schema.Long}, // Int64 max.
		// Crosses into BigInteger.
		{"9223372036854775808", schema.BigInteger},
		// Scientific notation: within the 40-digit guard, materalised safely.
		{"1e10", schema.Long},       // 10^10 < Int64 max.
		{"1e18", schema.Long},       // 10^18 < Int64 max (9.2e18).
		{"1e19", schema.BigInteger}, // 10^19 > Int64 max.
		{"1e38", schema.BigInteger}, // 10^38 < Int128 max (~1.7e38).
		// At the 40-digit threshold: 10^39 > Int128 max → UnboundInteger.
		{"1e39", schema.UnboundInteger},
		// Well beyond the threshold.
		{"1e1000000000", schema.UnboundInteger},
		// Negative large exponent.
		{"-1e1000000000", schema.UnboundInteger},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			got := schema.InferDataType(json.Number(tc.input))
			if got != tc.want {
				t.Errorf("InferDataType(%q) = %s, want %s", tc.input, got, tc.want)
			}
		})
	}
}
