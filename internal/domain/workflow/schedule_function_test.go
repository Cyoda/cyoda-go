package workflow

import (
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func ptr[T any](v T) *T { return &v }

func TestResolveSchedule(t *testing.T) {
	const arm = 1_000_000
	cases := []struct {
		name         string
		raw          string
		wantSched    int64
		wantTO       *int64 // nil = none
		wantBorn     bool
		wantErrACode string // "" = no error
	}{
		{"abs fire", `{"fireAt":2000000}`, 2000000, nil, false, ""},
		{"rel fire", `{"fireAfterMs":5000}`, 1005000, nil, false, ""},
		{"abs fire + abs expire", `{"fireAt":2000000,"expireAt":2000600}`, 2000000, ptr(int64(600)), false, ""},
		{"rel fire + rel expire", `{"fireAfterMs":5000,"expireAfterMs":600}`, 1005000, ptr(int64(600)), false, ""},
		{"abs fire + rel expire", `{"fireAt":2000000,"expireAfterMs":600}`, 2000000, ptr(int64(600)), false, ""},
		{"rel fire + abs expire", `{"fireAfterMs":5000,"expireAt":1005600}`, 1005000, ptr(int64(600)), false, ""},
		{"past fire ok", `{"fireAt":500000}`, 500000, nil, false, ""},
		{"born expired abs", `{"fireAt":2000000,"expireAt":1999999}`, 0, nil, true, ""},
		{"both fire fields", `{"fireAt":1,"fireAfterMs":2}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},
		{"no fire field", `{"expireAfterMs":600}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},
		{"both expiry fields", `{"fireAt":2000000,"expireAt":3,"expireAfterMs":4}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},
		{"non-numeric", `{"fireAt":"soon"}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},
		{"unknown field", `{"fireAt":1,"bogus":2}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},

		// Range/overflow: every resolved fire/expiry time must render as an
		// RFC 3339 time.Time (years 0000-9999), so it must land in
		// [0, 253402300799999] ms without int64 overflow along the way.
		{"fireAt one past max rejected", `{"fireAt":253402300800000}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},
		{"fireAt at max boundary accepted", `{"fireAt":253402300799999}`, 253402300799999, nil, false, ""},
		{"fireAt negative rejected", `{"fireAt":-1}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},
		{"fireAfterMs overflows int64 rejected", `{"fireAfterMs":9223372036854775807}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},
		{"fireAfterMs negative rejected", `{"fireAfterMs":-1}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},
		{"expireAfterMs overflows int64 rejected", `{"fireAt":2000000,"expireAfterMs":9223372036854775807}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},
		{"expireAfterMs negative rejected", `{"fireAt":2000000,"expireAfterMs":-1}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},
		{"expireAt out of range rejected", `{"fireAt":2000000,"expireAt":253402300800000}`, 0, nil, false, "SCHEDULE_FUNCTION_INVALID_RESULT"},
		{"expireAt at max boundary accepted", `{"fireAt":2000000,"expireAt":253402300799999}`, 2000000, ptr(int64(253402300799999 - 2000000)), false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sched, to, born, err := resolveSchedule(json.RawMessage(c.raw), arm)

			if c.wantErrACode != "" {
				if err == nil {
					t.Fatalf("expected error with code %q, got nil", c.wantErrACode)
				}
				appErr, ok := err.(*common.AppError)
				if !ok {
					t.Fatalf("expected *common.AppError, got %T: %v", err, err)
				}
				if appErr.Code != c.wantErrACode {
					t.Errorf("Code = %q, want %q", appErr.Code, c.wantErrACode)
				}
				if appErr.Status != 500 {
					t.Errorf("Status = %d, want 500", appErr.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if born != c.wantBorn {
				t.Errorf("bornExpired = %v, want %v", born, c.wantBorn)
			}
			if born {
				// scheduledTime/timeoutMs are unspecified when born-expired.
				return
			}
			if sched != c.wantSched {
				t.Errorf("scheduledTime = %d, want %d", sched, c.wantSched)
			}
			if (to == nil) != (c.wantTO == nil) {
				t.Fatalf("timeoutMs = %v, want %v", to, c.wantTO)
			}
			if to != nil && *to != *c.wantTO {
				t.Errorf("timeoutMs = %d, want %d", *to, *c.wantTO)
			}
		})
	}
}
