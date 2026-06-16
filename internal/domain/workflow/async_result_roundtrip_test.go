package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestAsyncResult_RoundTrip_PointerStates covers the JSON round-trip
// behaviour of the validator-accepted AsyncResult states through the
// SPI ProcessorConfig type. Validator rejects AsyncResult=&true and
// non-nil CrossoverToAsyncMs, so only nil and &false are observable in
// the import→store→export path; both must round-trip byte-equivalent.
func TestAsyncResult_RoundTrip_PointerStates(t *testing.T) {
	ff := false

	cases := []struct {
		name          string
		cfg           spi.ProcessorConfig
		wantInJSON    string
		wantNotInJSON string
	}{
		{
			name:          "async_nil_omitted",
			cfg:           spi.ProcessorConfig{},
			wantNotInJSON: "asyncResult",
		},
		{
			name:       "async_explicit_false_preserved",
			cfg:        spi.ProcessorConfig{AsyncResult: &ff},
			wantInJSON: `"asyncResult":false`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bs, err := json.Marshal(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantInJSON != "" && !strings.Contains(string(bs), tc.wantInJSON) {
				t.Errorf("expected JSON to contain %q; got %s", tc.wantInJSON, bs)
			}
			if tc.wantNotInJSON != "" && strings.Contains(string(bs), tc.wantNotInJSON) {
				t.Errorf("expected JSON NOT to contain %q; got %s", tc.wantNotInJSON, bs)
			}

			var back spi.ProcessorConfig
			if err := json.Unmarshal(bs, &back); err != nil {
				t.Fatal(err)
			}
			// Pointer-state-preserving equality.
			if (back.AsyncResult == nil) != (tc.cfg.AsyncResult == nil) {
				t.Errorf("AsyncResult pointer-presence mismatched: got %v, want %v",
					back.AsyncResult, tc.cfg.AsyncResult)
			}
			if back.AsyncResult != nil && tc.cfg.AsyncResult != nil &&
				*back.AsyncResult != *tc.cfg.AsyncResult {
				t.Errorf("AsyncResult value mismatched: got %v, want %v",
					*back.AsyncResult, *tc.cfg.AsyncResult)
			}
		})
	}
}

// TestAsyncResult_LegacyData_RoundTrips verifies that a ProcessorConfig
// containing non-default AsyncResult / CrossoverToAsyncMs (which the
// validator rejects at import) round-trips byte-equivalent through the
// raw JSON marshalling path. Backs the spec §5.4 assertion that
// pre-existing stored data (which could only land via a store-direct
// write that bypasses the import handler) survives export unchanged.
// No engine consumer means runtime ignores it; export must preserve.
func TestAsyncResult_LegacyData_RoundTrips(t *testing.T) {
	tt := true
	cv := int64(5000)
	original := spi.ProcessorConfig{
		AsyncResult:        &tt,
		CrossoverToAsyncMs: &cv,
	}

	bs, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bs), `"asyncResult":true`) {
		t.Errorf("marshalled JSON missing asyncResult=true: %s", bs)
	}
	if !strings.Contains(string(bs), `"crossoverToAsyncMs":5000`) {
		t.Errorf("marshalled JSON missing crossoverToAsyncMs=5000: %s", bs)
	}

	var back spi.ProcessorConfig
	if err := json.Unmarshal(bs, &back); err != nil {
		t.Fatal(err)
	}
	if back.AsyncResult == nil || *back.AsyncResult != true {
		t.Errorf("AsyncResult lost on round-trip: got %v", back.AsyncResult)
	}
	if back.CrossoverToAsyncMs == nil || *back.CrossoverToAsyncMs != 5000 {
		t.Errorf("CrossoverToAsyncMs lost on round-trip: got %v", back.CrossoverToAsyncMs)
	}
}
