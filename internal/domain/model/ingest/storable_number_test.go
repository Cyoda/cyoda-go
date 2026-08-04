package ingest

import (
	"strings"
	"testing"
)

// TestCheckJSONNumberToken pins the storability bound for JSON numbers.
//
// PostgreSQL's numeric type — which jsonb parses every JSON number into — holds
// at most 131072 digits before the decimal point and 16383 after. Past either
// limit the write fails inside the store with SQLSTATE 22003, so the payload
// arrives as a 500 for what is a client input error, while memory and sqlite
// accept it.
//
// The bound is on the EFFECTIVE weight and scale, not on the digits as written,
// and that distinction is the whole difficulty:
//
//   - 1e131071 fits; 1e131072 does not — the exponent moves the weight.
//   - 12e131071 does NOT fit: two integer digits plus the exponent is 131073.
//   - 1.5e-16383 does NOT fit, despite having a single fraction digit and an
//     exponent inside the limit — the exponent moves the scale too.
//   - 0.0001e131075 DOES fit: leading zeros are not significant, so the value
//     is really 1e131071.
//   - A zero coefficient is exempt from the weight limit (0e999999999 is just
//     zero) but NOT from the scale limit.
func TestCheckJSONNumberToken(t *testing.T) {
	cases := []struct {
		literal string
		wantBad bool
	}{
		// --- ordinary values ---
		{"0", false},
		{"-1", false},
		{"1.5", false},
		{"123456789012345678901234567890.123456789", false},
		{"1e400", false},
		{"-1.5e-400", false},
		{"1E+3", false},

		// --- weight boundary ---
		{"1e131071", false},
		{"1e131072", true},
		{"1e1000000", true},
		{"12e131071", true},      // 2 int digits + exp = 131073
		{"-12e131071", true},     // sign must not change the count
		{"0.0001e131075", false}, // leading zeros not significant -> 1e131071
		{"0.0001e131076", true},

		// --- scale boundary ---
		{"1e-16383", false},
		{"1e-16384", true},
		{"1.5e-16383", true}, // 1 fraction digit, |exp| in range, still overflows
		{"1.5e-16382", false},
		{"10e-16384", true},         // value is 1e-16383 but scale is computed lexically
		{"1.00000000e-16383", true}, // trailing zeros count toward scale

		// --- zero coefficient ---
		// A zero coefficient has no weight, but PostgreSQL still bounds the
		// exponent itself: 0e2000000000 overflows on input.
		{"0e999999999", false},
		{"0e1073741823", false},
		{"0e1073741824", true},
		{"0e2000000000", true},
		{"-0e2000000000", true},
		{"0e-999999999", true}, // NOT exempt from the scale limit
		{"0.0", false},

		// --- must not allocate or hang on absurd exponents ---
		{"1e99999999999999999999", true},
		{"1e-99999999999999999999", true},
	}

	for _, tc := range cases {
		t.Run(tc.literal, func(t *testing.T) {
			reason := checkJSONNumberToken([]byte(tc.literal))
			if tc.wantBad && reason == "" {
				t.Fatalf("%s accepted; PostgreSQL numeric cannot hold it", tc.literal)
			}
			if !tc.wantBad && reason != "" {
				t.Fatalf("%s rejected (%s); it is storable", tc.literal, reason)
			}
		})
	}
}

// TestRejectUnstorablePayload_Numbers checks the guard reaches numbers wherever
// they sit in the document, and reports the field path like the other reasons.
func TestRejectUnstorablePayload_Numbers(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		wantPath string
	}{
		{"top-level field", `{"big":1e1000000}`, "big"},
		{"nested", `{"outer":{"inner":1e1000000}}`, "outer.inner"},
		{"in an array", `{"xs":[1,1e1000000]}`, "xs[1]"},
		{"bare number payload", `1e1000000`, "(root)"},
		{"scale overflow", `{"d":1.5e-16383}`, "d"},

		{"ordinary number accepted", `{"n":1.5}`, ""},
		{"large but storable accepted", `{"n":1e131071}`, ""},
		{"precision preserved value accepted", `{"n":123456789012345678901234567890.123456789}`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RejectUnstorable([]byte(tc.payload))
			if tc.wantPath == "" {
				if err != nil {
					t.Fatalf("payload %s rejected but is storable: %v", tc.payload, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("payload %s accepted but is not storable", tc.payload)
			}
			if !strings.Contains(err.Error(), tc.wantPath) {
				t.Errorf("error %q does not name the offending path %q", err, tc.wantPath)
			}
		})
	}
}
