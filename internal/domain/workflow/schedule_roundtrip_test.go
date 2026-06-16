package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestSchedule_RoundTrip_TimeoutMsPointerStates(t *testing.T) {
	tm := int64(90000000)
	tmZero := int64(0)

	cases := []struct {
		name          string
		schedule      *spi.TransitionSchedule
		wantInJSON    string // substring that MUST appear in marshalled JSON
		wantNotInJSON string // substring that MUST NOT appear
	}{
		{
			name:       "timeoutMs_non_nil_positive",
			schedule:   &spi.TransitionSchedule{DelayMs: 86400000, TimeoutMs: &tm},
			wantInJSON: `"timeoutMs":90000000`,
		},
		{
			name:       "timeoutMs_non_nil_zero",
			schedule:   &spi.TransitionSchedule{DelayMs: 86400000, TimeoutMs: &tmZero},
			wantInJSON: `"timeoutMs":0`,
		},
		{
			name:          "timeoutMs_nil",
			schedule:      &spi.TransitionSchedule{DelayMs: 86400000},
			wantNotInJSON: "timeoutMs",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := spi.TransitionDefinition{
				Name:     "AutoClose",
				Next:     "Closed",
				Manual:   false,
				Schedule: tc.schedule,
			}
			bs, err := json.Marshal(tr)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantInJSON != "" && !strings.Contains(string(bs), tc.wantInJSON) {
				t.Errorf("expected JSON to contain %q; got %s", tc.wantInJSON, bs)
			}
			if tc.wantNotInJSON != "" && strings.Contains(string(bs), tc.wantNotInJSON) {
				t.Errorf("expected JSON NOT to contain %q; got %s", tc.wantNotInJSON, bs)
			}

			var back spi.TransitionDefinition
			if err := json.Unmarshal(bs, &back); err != nil {
				t.Fatal(err)
			}
			if back.Schedule == nil {
				t.Fatal("Schedule lost on round-trip")
			}
			if back.Schedule.DelayMs != tc.schedule.DelayMs {
				t.Errorf("DelayMs lost: got %d", back.Schedule.DelayMs)
			}
			// Pointer-state-preserving equality check.
			gotPtr := back.Schedule.TimeoutMs
			wantPtr := tc.schedule.TimeoutMs
			if (gotPtr == nil) != (wantPtr == nil) {
				t.Errorf("TimeoutMs pointer-presence mismatched: got %v, want %v", gotPtr, wantPtr)
			}
			if gotPtr != nil && wantPtr != nil && *gotPtr != *wantPtr {
				t.Errorf("TimeoutMs value mismatched: got %d, want %d", *gotPtr, *wantPtr)
			}
		})
	}
}
