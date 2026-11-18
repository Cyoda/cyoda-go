package schema_test

// type_admission_property_test.go states the design's two invariant
// properties (§4, §10) as executable tests over the full type × value
// matrix — numeric, string and temporal families together, not just
// numeric:
//
//   - held ⟹ declared types byte-identical, asserted in documents that ALSO
//     carry a gated change in another field. Without the second field this
//     would pass vacuously against §6's defect: a throwaway-model traversal
//     that discards values before judging them cannot tell "held" from
//     "the document just happened to write nothing new", and a model with
//     only one field never exercises that distinction.
//   - held ⟹ findable, over the same matrix, including the DOUBLE
//     precision-16 and scale-292 boundaries where a range-only admission
//     rule would have wrongly admitted a value EQUALS can never find (§5).
//     Findability is driven through the SPI kernel itself
//     (spi.ExpandLeaf/spi.EvalLeaf) — not spi.AdmitsNumeric a second time,
//     which would only prove the predicate agrees with itself.
//
// num() is admit_test.go's helper, shared within this package.

import (
	"encoding/json"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/tidwall/gjson"

	"github.com/cyoda/cyoda-go/internal/domain/model/schema"
)

// admissionMatrixCase is one row of the type × value matrix: a declared
// type, a value as schema.Admit sees it (json.Number / string / bool), the
// raw literal spi.ExpandLeaf/spi.EvalLeaf expect for the same value, and
// whether the type is expected to hold it.
type admissionMatrixCase struct {
	name     string
	declared schema.DataType
	value    any
	operand  string
	wantHeld bool
}

// admissionMatrix is the full type × value matrix both properties below
// draw from. Numeric boundaries mirror admit_test.go/numeric_admit_test.go
// (cyoda-go-spi) so the three legs — SPI unit, cyoda-go unit, and the two
// properties here — agree on the same literals; the string/temporal rows
// are new to this file, since neither existing suite states the property
// over that family.
var admissionMatrix = []admissionMatrixCase{
	// Numeric family.
	{"INTEGER at its ceiling", schema.Integer, num("2147483647"), "2147483647", true},
	{"whole number past 2^31 into DOUBLE", schema.Double, num("2147483648"), "2147483648", true},
	{"DOUBLE at the magnitude ceiling", schema.Double, num("9.99999999999999e292"), "9.99999999999999e292", true},
	{"DOUBLE holding 15 significant digits", schema.Double, num("1.23456789012345"), "1.23456789012345", true},
	{"DOUBLE at the scale-292 boundary", schema.Double, num("1e-292"), "1e-292", true},
	{"DOUBLE refuses 16 significant digits (precision boundary)", schema.Double, num("1.234567890123456"), "1.234567890123456", false},
	{"DOUBLE refuses scale past 292", schema.Double, num("1e-400"), "1e-400", false},
	{"DOUBLE refuses above the magnitude ceiling", schema.Double, num("9.99999999999999e300"), "9.99999999999999e300", false},
	{"BIG_DECIMAL holds a high scale (magnitude-only admission)", schema.BigDecimal, num("1.23456789012345678901234567890"), "1.23456789012345678901234567890", true},
	{"UNBOUND_INTEGER holds a huge whole number", schema.UnboundInteger, num("1e40"), "1e40", true},
	{"UNBOUND_INTEGER refuses a fraction", schema.UnboundInteger, num("1.5"), "1.5", false},
	{"UNBOUND_DECIMAL is the sink", schema.UnboundDecimal, num("9.99999999999999e300"), "9.99999999999999e300", true},

	// String / temporal family — the same implication, stated over a
	// different JSON kind and a different admission rule (§4's kind table
	// and exact-classification rule, not §5's numeric predicate).
	{"STRING holds a date-shaped string", schema.String, "2026-03-01", "2026-03-01", true},
	{"STRING holds an ordinary string", schema.String, "hello", "hello", true},
	{"STRING refuses a number", schema.String, num("5"), "5", false},
	{"ZONED_DATE_TIME holds a timestamp", schema.ZonedDateTime, "2026-03-01T10:00:00Z", "2026-03-01T10:00:00Z", true},
	{"ZONED_DATE_TIME refuses a year", schema.ZonedDateTime, "2026", "2026", false},

	// Boolean.
	{"BOOLEAN holds true", schema.Boolean, true, "true", true},
}

// dataTypesEqual compares two DataType slices for exact byte-for-byte
// (i.e. member-for-member, in order) equality.
func dataTypesEqual(a, b []schema.DataType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTypeAdmissionProperty_HeldValueDeclaredTypesByteIdentical is the
// design's first property: held ⟹ the model does not move. Every held case
// in admissionMatrix is written into a two-field document alongside a
// "gated" field carrying a value declared INTEGER never admits (a value
// past BIG_INTEGER's Int128 bound, forcing a genuine TYPE-level widen) — so
// each run proves two things at once: the held field's declared types stay
// byte-identical, and the write as a whole was NOT a no-op (the gated field
// really did change). Without the second field this would pass vacuously:
// a single-field document gives the traversal nothing else to fold in, so
// "declared types unchanged" would hold even for the broken throwaway-model
// comparison §6 replaces.
func TestTypeAdmissionProperty_HeldValueDeclaredTypesByteIdentical(t *testing.T) {
	for _, tc := range admissionMatrix {
		if !tc.wantHeld {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			root := schema.NewObjectNode()
			root.SetChild("held", schema.NewLeafNode(tc.declared))
			root.SetChild("gated", schema.NewLeafNode(schema.Integer))

			heldBefore := append([]schema.DataType(nil), root.Object().Child("held").DeclaredTypes()...)
			gatedBefore := append([]schema.DataType(nil), root.Object().Child("gated").DeclaredTypes()...)

			doc := map[string]any{
				"held": tc.value,
				// Past INTEGER's ceiling AND past LONG's — genuinely
				// BIG_INTEGER, so INTEGER cannot hold it at any level below
				// TYPE. Unrelated to "held"'s type, so this never
				// coincidentally becomes held itself.
				"gated": num("99999999999999999999"),
			}
			extended, err := schema.Extend(root, doc, spi.ChangeLevelType)
			if err != nil {
				t.Fatalf("Extend: %v", err)
			}

			heldAfter := extended.Object().Child("held").DeclaredTypes()
			if !dataTypesEqual(heldBefore, heldAfter) {
				t.Errorf("held field's declared types moved: %v -> %v", heldBefore, heldAfter)
			}

			// Non-vacuousness check: the gated field must actually have
			// changed, or this run proves nothing about the held field
			// either (a no-op Extend call trivially leaves everything
			// byte-identical).
			gatedAfter := extended.Object().Child("gated").DeclaredTypes()
			if dataTypesEqual(gatedBefore, gatedAfter) {
				t.Fatalf("the gated field did not actually change — this run is vacuous; gated types: %v", gatedAfter)
			}
		})
	}
}

// TestTypeAdmissionProperty_HeldValueIsFindable is the design's second
// property: held ⟹ findable. For every row admissionMatrix marks as held,
// EQUALS on the declared type over the stored value must match — driven
// through spi.ExpandLeaf and spi.EvalLeaf, the same kernel search uses, not
// spi.AdmitsNumeric a second time. Rows marked NOT held (the DOUBLE
// precision-16 and scale-past-292 boundaries, where a range-only rule
// would have wrongly admitted the value) are asserted not held and then
// skipped — the implication is vacuous for them, and asserting "not held"
// is what pins the boundary itself.
func TestTypeAdmissionProperty_HeldValueIsFindable(t *testing.T) {
	for _, tc := range admissionMatrix {
		t.Run(tc.name, func(t *testing.T) {
			leaf := schema.NewLeafNode(tc.declared)
			_, changes, err := schema.Admit(leaf, tc.value)
			if err != nil {
				t.Fatalf("Admit: %v", err)
			}
			held := len(changes) == 0
			if held != tc.wantHeld {
				t.Fatalf("held = %v, want %v (changes: %+v)", held, tc.wantHeld, changes)
			}
			if !held {
				return // vacuous: nothing to find for a value that was not held.
			}

			exp, err := spi.ExpandLeaf(spi.FilterEq, tc.operand, nil, []schema.DataType{tc.declared})
			if err != nil {
				t.Fatalf("%s admits %q but ExpandLeaf errored: %v", tc.declared, tc.operand, err)
			}

			var stored gjson.Result
			switch v := tc.value.(type) {
			case json.Number:
				// A bare gjson.Parse keeps it a numeric node; json.Marshal
				// on json.Number (a defined string type with no custom
				// MarshalJSON) would wrongly quote it.
				stored = gjson.Parse(string(v))
			default:
				b, merr := json.Marshal(v)
				if merr != nil {
					t.Fatalf("marshal %v: %v", v, merr)
				}
				stored = gjson.ParseBytes(b)
			}

			if !spi.EvalLeaf(exp, stored) {
				t.Errorf("%s admits %q but EQUALS cannot find it", tc.declared, tc.operand)
			}
		})
	}
}
