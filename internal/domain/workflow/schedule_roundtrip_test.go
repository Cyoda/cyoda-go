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

// TestSchedule_RoundTrip_Function asserts spi.TransitionSchedule.Function
// (spi.ScheduleFunction) round-trips every field through JSON marshal/
// unmarshal byte-identically — the wire shape encoding/json produces for
// spi.ScheduleFunction is exactly what the OpenAPI ScheduleFunctionDto
// documents (name, resultKind, calculationNodesTags, attachEntity, context,
// responseTimeoutMs), so no separate DTO struct is needed on the import/
// export path; the workflow-import handler decodes/encodes spi types
// directly (see ImportEntityModelWorkflow / ExportEntityModelWorkflow).
func TestSchedule_RoundTrip_Function(t *testing.T) {
	fn := spi.ScheduleFunction{
		Name:                 "computeNextFireTime",
		ResultKind:           "Schedule",
		CalculationNodesTags: "billing",
		AttachEntity:         true,
		Context:              "role=nightly",
		ResponseTimeoutMs:    5000,
	}
	tr := spi.TransitionDefinition{
		Name:   "AutoClose",
		Next:   "Closed",
		Manual: false,
		Schedule: &spi.TransitionSchedule{
			Function: &fn,
		},
	}

	bs, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	// A function-driven schedule has no static delay, and the published
	// TransitionScheduleDto gives `delayMs` `minimum: 1` and documents it as
	// mutually exclusive with `function` — so the wire shape must omit the key
	// rather than emit a meaningless zero. DelayMs > 0 is an invariant of the
	// static shape (validate.go's delayMs/function XOR), so `omitempty` maps
	// "no static delay" to "absent" both ways.
	if strings.Contains(string(bs), "delayMs") {
		t.Errorf("expected no delayMs key on a function-driven schedule: %s", bs)
	}
	for _, want := range []string{
		`"name":"computeNextFireTime"`,
		`"resultKind":"Schedule"`,
		`"calculationNodesTags":"billing"`,
		`"context":"role=nightly"`,
		`"responseTimeoutMs":5000`,
	} {
		if !strings.Contains(string(bs), want) {
			t.Errorf("expected JSON to contain %q; got %s", want, bs)
		}
	}

	var back spi.TransitionDefinition
	if err := json.Unmarshal(bs, &back); err != nil {
		t.Fatal(err)
	}
	if back.Schedule == nil || back.Schedule.Function == nil {
		t.Fatal("Schedule.Function lost on round-trip")
	}
	got := *back.Schedule.Function
	if got != fn {
		t.Errorf("Function round-trip mismatch: got %+v, want %+v", got, fn)
	}

	// The wire shape a real export would produce (delayMs omitted, function
	// present) must import unchanged through the real import validation —
	// not just decode without error. An absent delayMs and a function are
	// the accepted shape validate.go's delayMs/function XOR expects, so a
	// workflow built from the round-tripped transition must pass
	// validateImportRequest cleanly.
	wf := spi.WorkflowDefinition{
		Version: "1.5", Name: "wf-schedule-roundtrip", InitialState: "S0", Active: true,
		States: map[string]spi.StateDefinition{
			"S0":     {Transitions: []spi.TransitionDefinition{back}},
			"Closed": {},
		},
	}
	if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
		t.Errorf("exported-then-reimported function schedule must pass import validation unchanged: %v", err)
	}
}
