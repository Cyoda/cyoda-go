package workflow

import (
	"math"
	"testing"
)

// TestAddScheduleMs covers the single overflow-safe-add-plus-range-check
// helper shared by resolveSchedule (Function results) and
// reconcileScheduledTasks (the static Schedule.DelayMs path). Both a value
// supplied directly (delta=0) and one computed by addition go through the
// same range gate: [minScheduleMs, maxScheduleMs], the span
// time.Time.MarshalJSON can render as RFC 3339 (years 0000-9999).
func TestAddScheduleMs(t *testing.T) {
	cases := []struct {
		name    string
		base    int64
		delta   int64
		wantSum int64
		wantOK  bool
	}{
		{"ordinary add", 1_000_000, 5_000, 1_005_000, true},
		{"direct value in range (delta 0)", 42, 0, 42, true},
		{"min boundary accepted", 0, 0, 0, true},
		{"below min rejected", -1, 0, 0, false},
		{"max boundary accepted", maxScheduleMs, 0, maxScheduleMs, true},
		{"above max rejected", maxScheduleMs + 1, 0, 0, false},
		{"add lands one past max rejected", maxScheduleMs - 100, 101, 0, false},
		{"add lands exactly at max accepted", maxScheduleMs - 100, 100, maxScheduleMs, true},
		{"positive overflow rejected", math.MaxInt64, 1, 0, false},
		{"negative delta still in range", 1_000_000, -500_000, 500_000, true},
		{"negative delta underflows base", math.MinInt64, -1, 0, false},
		{"negative delta pushes below min", 100, -200, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sum, ok := addScheduleMs(c.base, c.delta)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (base=%d delta=%d)", ok, c.wantOK, c.base, c.delta)
			}
			if ok && sum != c.wantSum {
				t.Errorf("sum = %d, want %d", sum, c.wantSum)
			}
		})
	}
}
