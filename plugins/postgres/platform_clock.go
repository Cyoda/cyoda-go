package postgres

import (
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// The business clock (spi.WithClock) is nil in production: business stamps
// then come from the database clock, as they always have, and each helper
// below returns the value that keeps that path unchanged. With a clock, the
// clock's reading takes the database clock's place in the business stamps
// only; liveness and timeouts stay on physical time.

// stampAtArg is the at_us argument of cyoda_stamp and cyoda_consistency_time
// (migration 000017): nil — SQL NULL, so the function reads clock_timestamp()
// — without a clock, else the clock's reading in microseconds.
func stampAtArg(c spi.Clock) *int64 {
	if c == nil {
		return nil
	}
	us := c.Now().UnixMicro()
	return &us
}

// placeholderAtArg is the argument of the placeholder-date select: nil — SQL
// NULL, so the select falls back to CURRENT_TIMESTAMP — without a clock, else
// the clock's reading.
func placeholderAtArg(c spi.Clock) *time.Time {
	if c == nil {
		return nil
	}
	now := c.Now()
	return &now
}

// businessNow is the instant business stamps are compared with: the clock's
// reading, or the process clock without one.
func businessNow(c spi.Clock) time.Time {
	if c == nil {
		return time.Now() // realtime: no test clock configured; production's business clock is real time (DB clock in SQL), a test clock is passed when the feature is on
	}
	return c.Now()
}
