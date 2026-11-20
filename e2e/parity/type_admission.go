package parity

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

// type_admission.go covers the design's backend-agnostic §10 rows: held
// values leave the model byte-identical, a held value is findable
// afterward, the DOUBLE-ceiling gated boundary, mixed-kind arrays judge
// each element individually, the search-side EQUALS/precision fix,
// registration still yielding {STRING, LOCAL_DATE}, and strict validation
// never being more permissive than ARRAY_LENGTH. See
// docs/superpowers/specs/2026-09-04-555-type-admission-design.md.
//
// Task 11's RunSchemaNumericFoldCarveout and schema_numeric_fold_carveout.go
// already cover the numeric-leaf fold's order-dependence carve-out;
// numeric_classification.go's RunNumericClassificationDoubleSchemaAcceptsWholeNumber
// already covers the DOUBLE whole-number invariant and its
// 2147483648/9007199254740993 mantissa boundary. Neither is repeated here.

// typeAdmissionAutoWorkflow is a minimal auto-transition workflow, reused
// wherever a scenario needs an entity to reach a searchable state.
const typeAdmissionAutoWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "typeadm-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
			"CREATED": {}
		}
	}]
}`

// RunTypeAdmissionHeldValueUnchanged asserts §4's first arrow — "the field
// holds it ⟹ the model is unchanged" — for a held write under STRICT
// validation (no changeLevel at all), the case no configuration could work
// around. Both numeric (a whole number past 2^31 into a DOUBLE leaf) and
// temporal-shaped string (a date-shaped string into a STRING leaf, §4(b))
// families are asserted.
func RunTypeAdmissionHeldValueUnchanged(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	t.Run("whole number past 2^31 into DOUBLE, strict", func(t *testing.T) {
		const modelName = "typeadm-held-unchanged-double"
		const modelVersion = 1
		if err := c.ImportModel(t, modelName, modelVersion, `{"amount":10.5}`); err != nil {
			t.Fatalf("ImportModel: %v", err)
		}
		if err := c.LockModel(t, modelName, modelVersion); err != nil {
			t.Fatalf("LockModel: %v", err)
		}
		// No SetChangeLevel call at all: strict validation.

		before, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
		if err != nil {
			t.Fatalf("ExportModel before: %v", err)
		}
		status, body, err := c.CreateEntityRaw(t, modelName, modelVersion, `{"amount":2147483648}`)
		if err != nil {
			t.Fatalf("CreateEntityRaw: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("2147483648 is held by a DOUBLE leaf; got %d: %s", status, body)
		}
		after, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
		if err != nil {
			t.Fatalf("ExportModel after: %v", err)
		}
		if string(before) != string(after) {
			t.Errorf("a held value must not move the model\n  before: %s\n  after:  %s", before, after)
		}
	})

	t.Run("date-shaped string into STRING, strict", func(t *testing.T) {
		const modelName = "typeadm-held-unchanged-string"
		const modelVersion = 1
		if err := c.ImportModel(t, modelName, modelVersion, `{"note":"hello"}`); err != nil {
			t.Fatalf("ImportModel: %v", err)
		}
		if err := c.LockModel(t, modelName, modelVersion); err != nil {
			t.Fatalf("LockModel: %v", err)
		}

		before, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
		if err != nil {
			t.Fatalf("ExportModel before: %v", err)
		}
		status, body, err := c.CreateEntityRaw(t, modelName, modelVersion, `{"note":"2026-03-01"}`)
		if err != nil {
			t.Fatalf("CreateEntityRaw: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("a date-shaped string is held by a STRING leaf; got %d: %s", status, body)
		}
		after, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
		if err != nil {
			t.Fatalf("ExportModel after: %v", err)
		}
		if string(before) != string(after) {
			t.Errorf("a held value must not move the model\n  before: %s\n  after:  %s", before, after)
		}
	})
}

// RunTypeAdmissionHeldThenFound asserts §4's full invariant — "the field
// holds it ⟹ the model is unchanged ⟹ the value is findable" — for the two
// families whose admission predicate is not "inside the range": a DOUBLE
// leaf holding 2147483648, and a BIG_DECIMAL leaf holding a value whose
// scale exceeds what the leaf was registered with (BIG_DECIMAL admission is
// magnitude-only, §5).
func RunTypeAdmissionHeldThenFound(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	t.Run("DOUBLE", func(t *testing.T) {
		const modelName = "typeadm-held-found-double"
		const modelVersion = 1
		if err := c.ImportModel(t, modelName, modelVersion, `{"amount":10.5}`); err != nil {
			t.Fatalf("ImportModel: %v", err)
		}
		if err := c.LockModel(t, modelName, modelVersion); err != nil {
			t.Fatalf("LockModel: %v", err)
		}
		if err := c.ImportWorkflow(t, modelName, modelVersion, typeAdmissionAutoWorkflow); err != nil {
			t.Fatalf("ImportWorkflow: %v", err)
		}
		if _, err := c.CreateEntity(t, modelName, modelVersion, `{"amount":2147483648}`); err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		hits, err := c.SyncSearch(t, modelName, modelVersion,
			`{"type":"simple","jsonPath":"$.amount","operatorType":"EQUALS","value":2147483648}`)
		if err != nil {
			t.Fatalf("SyncSearch: %v", err)
		}
		if len(hits) != 1 {
			t.Errorf("2147483648 was held by DOUBLE; it must be findable; got %d hits", len(hits))
		}

		// The out-of-range NotNull residual (§7(i)): the stored-value filter
		// must judge 2147483648 the same way admission did, or a comparison
		// that should match it silently drops the row.
		residual, err := c.SyncSearch(t, modelName, modelVersion,
			`{"type":"simple","jsonPath":"$.amount","operatorType":"LESS_THAN","value":1e300}`)
		if err != nil {
			t.Fatalf("SyncSearch LESS_THAN: %v", err)
		}
		if len(residual) != 1 {
			t.Errorf("[DOUBLE] < 1e300 must find the stored 2147483648; got %d hits", len(residual))
		}

		// §7(i) names evalBetween explicitly as one of the two stored-value-
		// filter rewrite sites (alongside evalCompare) — this is changed
		// behaviour, not the "expandBetween is already clean" operand-
		// normalisation half of §7 that stays untouched.
		between, err := c.SyncSearch(t, modelName, modelVersion,
			`{"type":"simple","jsonPath":"$.amount","operatorType":"BETWEEN_INCLUSIVE","value":[2147483647,2147483649]}`)
		if err != nil {
			t.Fatalf("SyncSearch BETWEEN_INCLUSIVE: %v", err)
		}
		if len(between) != 1 {
			t.Errorf("BETWEEN_INCLUSIVE [2147483647, 2147483649] must find the stored 2147483648; got %d hits", len(between))
		}
	})

	t.Run("BIG_DECIMAL high scale", func(t *testing.T) {
		const modelName = "typeadm-held-found-bigdecimal"
		const modelVersion = 1
		// Registers BIG_DECIMAL (precision 19, scale 18, exp 1 ≤ 20).
		if err := c.ImportModel(t, modelName, modelVersion, `{"amount":1.234567890123456789}`); err != nil {
			t.Fatalf("ImportModel: %v", err)
		}
		registered, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
		if err != nil {
			t.Fatalf("ExportModel after registration: %v", err)
		}
		if !strings.Contains(string(registered), "BIG_DECIMAL") {
			t.Fatalf("sample must register amount as BIG_DECIMAL: %s", registered)
		}
		if err := c.LockModel(t, modelName, modelVersion); err != nil {
			t.Fatalf("LockModel: %v", err)
		}
		if err := c.ImportWorkflow(t, modelName, modelVersion, typeAdmissionAutoWorkflow); err != nil {
			t.Fatalf("ImportWorkflow: %v", err)
		}
		before, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
		if err != nil {
			t.Fatalf("ExportModel before write: %v", err)
		}

		const highScale = "1.23456789012345678901234567890"
		status, body, err := c.CreateEntityRaw(t, modelName, modelVersion, `{"amount":`+highScale+`}`)
		if err != nil {
			t.Fatalf("CreateEntityRaw: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("a high-scale value is held by BIG_DECIMAL's magnitude-only admission; got %d: %s", status, body)
		}
		after, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
		if err != nil {
			t.Fatalf("ExportModel after: %v", err)
		}
		if string(before) != string(after) {
			t.Errorf("a held value must not move the model\n  before: %s\n  after:  %s", before, after)
		}
		hits, err := c.SyncSearch(t, modelName, modelVersion,
			fmt.Sprintf(`{"type":"simple","jsonPath":"$.amount","operatorType":"EQUALS","value":%s}`, highScale))
		if err != nil {
			t.Fatalf("SyncSearch: %v", err)
		}
		if len(hits) != 1 {
			t.Errorf("the high-scale value was held; it must be findable; got %d hits", len(hits))
		}
	})
}

// RunTypeAdmissionDoubleCeiling asserts the DOUBLE magnitude boundary from
// both sides: at the ceiling (9.99999999999999e292) the value is held and
// findable; just past it the write is a genuine type change and stays
// gated below TYPE.
func RunTypeAdmissionDoubleCeiling(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	t.Run("at the ceiling: held and findable", func(t *testing.T) {
		const modelName = "typeadm-double-ceiling-held"
		const modelVersion = 1
		if err := c.ImportModel(t, modelName, modelVersion, `{"amount":10.5}`); err != nil {
			t.Fatalf("ImportModel: %v", err)
		}
		if err := c.LockModel(t, modelName, modelVersion); err != nil {
			t.Fatalf("LockModel: %v", err)
		}
		if err := c.ImportWorkflow(t, modelName, modelVersion, typeAdmissionAutoWorkflow); err != nil {
			t.Fatalf("ImportWorkflow: %v", err)
		}
		const ceiling = "9.99999999999999e292"
		before, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
		if err != nil {
			t.Fatalf("ExportModel before: %v", err)
		}
		if _, err := c.CreateEntity(t, modelName, modelVersion, `{"amount":`+ceiling+`}`); err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		after, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
		if err != nil {
			t.Fatalf("ExportModel after: %v", err)
		}
		if string(before) != string(after) {
			t.Errorf("the ceiling value must not move the model\n  before: %s\n  after:  %s", before, after)
		}
		hits, err := c.SyncSearch(t, modelName, modelVersion,
			`{"type":"simple","jsonPath":"$.amount","operatorType":"EQUALS","value":`+ceiling+`}`)
		if err != nil {
			t.Fatalf("SyncSearch: %v", err)
		}
		if len(hits) != 1 {
			t.Errorf("the ceiling value must be findable; got %d hits", len(hits))
		}
	})

	t.Run("above the ceiling: still gated", func(t *testing.T) {
		const modelName = "typeadm-double-ceiling-gated"
		const modelVersion = 1
		if err := c.ImportModel(t, modelName, modelVersion, `{"amount":10.5}`); err != nil {
			t.Fatalf("ImportModel: %v", err)
		}
		if err := c.LockModel(t, modelName, modelVersion); err != nil {
			t.Fatalf("LockModel: %v", err)
		}
		if err := c.SetChangeLevel(t, modelName, modelVersion, "ARRAY_LENGTH"); err != nil {
			t.Fatalf("SetChangeLevel: %v", err)
		}
		status, body, err := c.CreateEntityRaw(t, modelName, modelVersion, `{"amount":9.99999999999999e300}`)
		if err != nil {
			t.Fatalf("CreateEntityRaw: %v", err)
		}
		if status != http.StatusBadRequest {
			t.Errorf("above the ceiling is a genuine type change; got %d: %s", status, body)
		}
	})
}

// RunTypeAdmissionMixedKindArray asserts §6's structural change: array
// elements are judged individually rather than fused into one description
// before judging. [2147483648, "hello"] into a [DOUBLE] element leaves
// 2147483648 held (contributing no label) and only "hello" needs
// ARRAY_ELEMENTS to add STRING — so the resulting element type is
// {DOUBLE, STRING}, never a fused large-integer label like LONG or
// UNBOUND_DECIMAL.
func RunTypeAdmissionMixedKindArray(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "typeadm-mixed-kind-array"
	const modelVersion = 1
	if err := c.ImportModel(t, modelName, modelVersion, `{"amounts":[10.5]}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.SetChangeLevel(t, modelName, modelVersion, "ARRAY_ELEMENTS"); err != nil {
		t.Fatalf("SetChangeLevel: %v", err)
	}

	status, body, err := c.CreateEntityRaw(t, modelName, modelVersion, `{"amounts":[2147483648,"hello"]}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("2147483648 is held by DOUBLE and \"hello\" needs only ARRAY_ELEMENTS; got %d: %s", status, body)
	}

	exported, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel: %v", err)
	}
	if !strings.Contains(string(exported), "DOUBLE") || !strings.Contains(string(exported), "STRING") {
		t.Errorf("the element type must declare both DOUBLE and STRING: %s", exported)
	}
	if strings.Contains(string(exported), "LONG") || strings.Contains(string(exported), "UNBOUND_DECIMAL") {
		t.Errorf("the element type must not show a fused numeric label — that would mean 2147483648 was judged by the whole array's fusion rather than on its own: %s", exported)
	}
}

// RunTypeAdmissionSearchEqualsTrailingZeros asserts §7(ii): EQUALS 5.0 finds
// a stored 5 on an INTEGER leaf (ingestion strips trailing zeros before
// classifying; search now does too), and EQUALS 5.000...0 finds a stored 5
// on a DOUBLE leaf (the same fix applied to the decimal family, which has
// the identical precision-computed-on-the-unstripped-operand defect).
func RunTypeAdmissionSearchEqualsTrailingZeros(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	t.Run("INTEGER", func(t *testing.T) {
		const modelName = "typeadm-search-equals-int"
		const modelVersion = 1
		if err := c.ImportModel(t, modelName, modelVersion, `{"amount":10}`); err != nil {
			t.Fatalf("ImportModel: %v", err)
		}
		if err := c.LockModel(t, modelName, modelVersion); err != nil {
			t.Fatalf("LockModel: %v", err)
		}
		if err := c.ImportWorkflow(t, modelName, modelVersion, typeAdmissionAutoWorkflow); err != nil {
			t.Fatalf("ImportWorkflow: %v", err)
		}
		if _, err := c.CreateEntity(t, modelName, modelVersion, `{"amount":5}`); err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		hits, err := c.SyncSearch(t, modelName, modelVersion,
			`{"type":"simple","jsonPath":"$.amount","operatorType":"EQUALS","value":5.0}`)
		if err != nil {
			t.Fatalf("SyncSearch EQUALS: %v", err)
		}
		if len(hits) != 1 {
			t.Errorf("EQUALS 5.0 must find the stored 5; got %d hits", len(hits))
		}
		misses, err := c.SyncSearch(t, modelName, modelVersion,
			`{"type":"simple","jsonPath":"$.amount","operatorType":"NOT_EQUAL","value":5.0}`)
		if err != nil {
			t.Fatalf("SyncSearch NOT_EQUAL: %v", err)
		}
		if len(misses) != 0 {
			t.Errorf("NOT_EQUAL 5.0 must not match the stored 5; got %d hits", len(misses))
		}
	})

	t.Run("DOUBLE", func(t *testing.T) {
		const modelName = "typeadm-search-equals-double"
		const modelVersion = 1
		if err := c.ImportModel(t, modelName, modelVersion, `{"amount":10.5}`); err != nil {
			t.Fatalf("ImportModel: %v", err)
		}
		if err := c.LockModel(t, modelName, modelVersion); err != nil {
			t.Fatalf("LockModel: %v", err)
		}
		if err := c.ImportWorkflow(t, modelName, modelVersion, typeAdmissionAutoWorkflow); err != nil {
			t.Fatalf("ImportWorkflow: %v", err)
		}
		if _, err := c.CreateEntity(t, modelName, modelVersion, `{"amount":5}`); err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		hits, err := c.SyncSearch(t, modelName, modelVersion,
			`{"type":"simple","jsonPath":"$.amount","operatorType":"EQUALS","value":5.000000000000000000}`)
		if err != nil {
			t.Fatalf("SyncSearch: %v", err)
		}
		if len(hits) != 1 {
			t.Errorf("EQUALS 5.000...0 must find the stored 5 on a DOUBLE leaf; got %d hits", len(hits))
		}
	})
}

// RunTypeAdmissionRegistrationYieldsStringLocalDate asserts §8: registration
// is unchanged. Registering "hello" then "2026-03-01" still yields a
// {STRING, LOCAL_DATE} leaf, and a later "2026" write into that LOCKED leaf
// is held by the STRING branch without growing a YEAR branch the way
// registration would — ingestion changed, registration did not.
func RunTypeAdmissionRegistrationYieldsStringLocalDate(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "typeadm-registration-string-localdate"
	const modelVersion = 1
	if err := c.ImportModel(t, modelName, modelVersion, `{"note":"hello"}`); err != nil {
		t.Fatalf("ImportModel #1: %v", err)
	}
	if err := c.ImportModel(t, modelName, modelVersion, `{"note":"2026-03-01"}`); err != nil {
		t.Fatalf("ImportModel #2: %v", err)
	}

	registered, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel: %v", err)
	}
	if !strings.Contains(string(registered), "STRING") || !strings.Contains(string(registered), "LOCAL_DATE") {
		t.Fatalf("registration must yield {STRING, LOCAL_DATE}: %s", registered)
	}

	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	before, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel before: %v", err)
	}
	status, body, err := c.CreateEntityRaw(t, modelName, modelVersion, `{"note":"2026"}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf(`"2026" is held by the STRING branch; got %d: %s`, status, body)
	}
	after, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Fatalf("ExportModel after: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("a held value must not move the model\n  before: %s\n  after:  %s", before, after)
	}
}

// RunTypeAdmissionStrictNeverMorePermissiveThanArrayLength asserts strict
// validation is never more permissive than ARRAY_LENGTH: ARRAY_LENGTH is
// the lowest active rank, one above strict's "nothing", so it must grant no
// permission a TYPE-level change needs. 9007199254740993 into DOUBLE (past
// the mantissa boundary, requiring TYPE) is refused identically at strict
// and at ARRAY_LENGTH. A longer array cannot serve this row: an array's
// length is not part of the model, so it is held at every level, strict
// included.
func RunTypeAdmissionStrictNeverMorePermissiveThanArrayLength(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	for _, tc := range []struct{ name, level string }{
		{"strict", ""},
		{"ARRAY_LENGTH", "ARRAY_LENGTH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modelName := "typeadm-strict-vs-arraylength-" + tc.name
			const modelVersion = 1
			if err := c.ImportModel(t, modelName, modelVersion, `{"amount":10.5}`); err != nil {
				t.Fatalf("ImportModel: %v", err)
			}
			if err := c.LockModel(t, modelName, modelVersion); err != nil {
				t.Fatalf("LockModel: %v", err)
			}
			if tc.level != "" {
				if err := c.SetChangeLevel(t, modelName, modelVersion, tc.level); err != nil {
					t.Fatalf("SetChangeLevel: %v", err)
				}
			}
			status, body, err := c.CreateEntityRaw(t, modelName, modelVersion, `{"amount":9007199254740993}`)
			if err != nil {
				t.Fatalf("CreateEntityRaw: %v", err)
			}
			if status != http.StatusBadRequest {
				t.Errorf("ARRAY_LENGTH must grant no TYPE-level permission; got %d: %s", status, body)
			}
		})
	}
}

// RunTypeAdmissionLongerArrayHeldAtEveryLevel asserts that an array's length
// is not part of the model: a longer array is held by the array that
// declared its element, at ARRAY_LENGTH — the floor of the ladder, which
// permits no schema change at all — and under strict validation with no
// changeLevel set, and the exported model is byte-identical afterwards on
// every backend.
func RunTypeAdmissionLongerArrayHeldAtEveryLevel(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	for _, tc := range []struct{ name, level string }{
		{"ARRAY_LENGTH", "ARRAY_LENGTH"},
		{"strict", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modelName := "typeadm-longer-array-" + strings.ToLower(tc.name)
			const modelVersion = 1
			if err := c.ImportModel(t, modelName, modelVersion, `{"tags":["a","b"]}`); err != nil {
				t.Fatalf("ImportModel: %v", err)
			}
			if err := c.LockModel(t, modelName, modelVersion); err != nil {
				t.Fatalf("LockModel: %v", err)
			}
			if tc.level != "" {
				if err := c.SetChangeLevel(t, modelName, modelVersion, tc.level); err != nil {
					t.Fatalf("SetChangeLevel: %v", err)
				}
			}
			before, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
			if err != nil {
				t.Fatalf("ExportModel before: %v", err)
			}
			status, body, err := c.CreateEntityRaw(t, modelName, modelVersion, `{"tags":["a","b","c","d","e"]}`)
			if err != nil {
				t.Fatalf("CreateEntityRaw: %v", err)
			}
			if status != http.StatusOK {
				t.Fatalf("a longer array must be held at %q; got %d: %s", tc.name, status, body)
			}
			after, err := c.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
			if err != nil {
				t.Fatalf("ExportModel after: %v", err)
			}
			if string(before) != string(after) {
				t.Errorf("a longer array must not move the model\n  before: %s\n  after:  %s", before, after)
			}
		})
	}
}
