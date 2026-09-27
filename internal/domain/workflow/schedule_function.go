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
//   - The resolved fire time, and the resolved expiry when present, must
//     each land in [minScheduleMs, maxScheduleMs] (see addScheduleMs) —
//     the range a time.Time can render as RFC 3339 — computed without
//     int64 overflow; a negative fireAfterMs/expireAfterMs is rejected the
//     same way, before any arithmetic.
//
// A structurally invalid result (wrong field combination, non-numeric
// value, an unknown field, or a fire/expiry time outside the renderable
// range) returns a *common.AppError classified 500 — the failure is in the
// compute node's response, not the caller's request.
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

	var sched int64
	if s.FireAt != nil {
		v, ok := addScheduleMs(*s.FireAt, 0)
		if !ok {
			return 0, nil, false, invalidScheduleResult("fireAt is outside the renderable range")
		}
		sched = v
	} else {
		if *s.FireAfterMs < 0 {
			return 0, nil, false, invalidScheduleResult("fireAfterMs must not be negative")
		}
		v, ok := addScheduleMs(armMs, *s.FireAfterMs)
		if !ok {
			return 0, nil, false, invalidScheduleResult("fireAfterMs produces an unrenderable fire time")
		}
		sched = v
	}

	var expiry *int64
	switch {
	case s.ExpireAt != nil:
		v, ok := addScheduleMs(*s.ExpireAt, 0)
		if !ok {
			return 0, nil, false, invalidScheduleResult("expireAt is outside the renderable range")
		}
		expiry = &v
	case s.ExpireAfterMs != nil:
		if *s.ExpireAfterMs < 0 {
			return 0, nil, false, invalidScheduleResult("expireAfterMs must not be negative")
		}
		v, ok := addScheduleMs(sched, *s.ExpireAfterMs)
		if !ok {
			return 0, nil, false, invalidScheduleResult("expireAfterMs produces an unrenderable expiry time")
		}
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
