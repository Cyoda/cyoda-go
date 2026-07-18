package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// resolveSchedule parses the raw JSON value of a scheduled-transition
// Function callout's "Schedule" result and applies the fire/expiry
// resolution rules:
//
//   - Exactly one of fireAt (absolute unix-millis) or fireAfterMs (relative
//     to armMs, the moment reconcileScheduledTasks is dispatching) is
//     required; a past fireAt is not an error — the transition simply arms
//     to fire immediately.
//   - At most one of expireAt (absolute) or expireAfterMs (relative to the
//     resolved fire time) may be present.
//   - If a resolved expiry is at or before the resolved fire time, the
//     transition is "born expired": it must never be armed. Callers signal
//     this by discarding scheduledTime/timeoutMs and instead cancelling any
//     existing row for the transition and recording an EXPIRE audit event.
//   - Otherwise timeoutMs is the gap between expiry and fire time (nil when
//     no expiry was supplied — never expires).
//
// A structurally invalid result (wrong field combination, non-numeric
// value, or an unknown field) returns a *common.AppError classified 500 —
// the failure is in the compute node's response, not the caller's request.
func resolveSchedule(raw json.RawMessage, armMs int64) (scheduledTime int64, timeoutMs *int64, bornExpired bool, err error) {
	var s struct {
		FireAt        *int64 `json:"fireAt"`
		FireAfterMs   *int64 `json:"fireAfterMs"`
		ExpireAt      *int64 `json:"expireAt"`
		ExpireAfterMs *int64 `json:"expireAfterMs"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if decErr := dec.Decode(&s); decErr != nil {
		return 0, nil, false, invalidScheduleResult("malformed Schedule result: %v", decErr)
	}
	if (s.FireAt == nil) == (s.FireAfterMs == nil) {
		return 0, nil, false, invalidScheduleResult("exactly one of fireAt/fireAfterMs required")
	}
	if s.ExpireAt != nil && s.ExpireAfterMs != nil {
		return 0, nil, false, invalidScheduleResult("at most one of expireAt/expireAfterMs")
	}

	sched := armMs
	if s.FireAt != nil {
		sched = *s.FireAt
	} else {
		sched = armMs + *s.FireAfterMs
	}

	var expiry *int64
	switch {
	case s.ExpireAt != nil:
		expiry = s.ExpireAt
	case s.ExpireAfterMs != nil:
		v := sched + *s.ExpireAfterMs
		expiry = &v
	}

	if expiry != nil && *expiry <= sched {
		return 0, nil, true, nil // born expired: never armed
	}

	if expiry != nil {
		v := *expiry - sched
		timeoutMs = &v
	}
	return sched, timeoutMs, false, nil
}

// invalidScheduleResult builds the 500 SCHEDULE_FUNCTION_INVALID_RESULT
// AppError shared by resolveSchedule and its caller (the resultKind check
// in reconcileScheduledTasks).
func invalidScheduleResult(format string, a ...any) error {
	return common.InternalWithCode(common.ErrCodeScheduleFunctionInvalidResult, fmt.Sprintf(format, a...), nil)
}
