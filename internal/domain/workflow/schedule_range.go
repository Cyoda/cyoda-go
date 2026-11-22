package workflow

const (
	// minScheduleMs is the lower bound a scheduled-transition time (fire or
	// expiry) must satisfy. No legitimate scheduled task fires before the
	// Unix epoch, so a resolved value below it is rejected the same as one
	// that overflows or exceeds maxScheduleMs — there is no meaningful
	// "negative" scheduled time in this domain.
	minScheduleMs int64 = 0

	// maxScheduleMs is 9999-12-31T23:59:59.999Z UTC in Unix millis — the
	// last instant time.Time.MarshalJSON can render as RFC 3339 (a 4-digit
	// year). A ScheduledTask whose scheduledTime or scheduledTime+timeoutMs
	// falls past this is unrenderable: the GET /scheduled-tasks response
	// would fail to marshal after its 200 header is already written.
	maxScheduleMs int64 = 253402300799999
)

// addScheduleMs adds delta to base and reports whether the sum is free of
// int64 overflow/underflow AND within [minScheduleMs, maxScheduleMs]. sum is
// meaningless when ok is false.
//
// Passing delta=0 checks a value supplied directly (fireAt, expireAt)
// against the same range without any arithmetic — the one helper both
// resolveSchedule (a Function result's fireAt/fireAfterMs/expireAt/
// expireAfterMs) and reconcileScheduledTasks (the static
// armMs+Schedule.DelayMs, and its own scheduledTime+TimeoutMs) use, so the
// range constant and the overflow check exist in exactly one place.
//
// Only the negative-delta (underflow) case needs an explicit guard. A
// positive-delta overflow always wraps the sum to a negative number, which
// the plain range check below already rejects (minScheduleMs is 0), so no
// separate branch is reachable there. A negative-delta underflow can wrap
// the sum back UP past base — e.g. two operands both near math.MinInt64 can
// wrap all the way around to a small positive number that would otherwise
// read as "in range" — so it is caught before the range check ever sees it.
func addScheduleMs(base, delta int64) (sum int64, ok bool) {
	sum = base + delta
	if delta < 0 && sum > base {
		return 0, false // underflow: wrapped back up past base
	}
	return sum, sum >= minScheduleMs && sum <= maxScheduleMs
}
