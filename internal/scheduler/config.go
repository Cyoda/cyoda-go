package scheduler

import "time"

// Config is the scheduler's settings. app.SchedulerConfig has the same fields,
// with the same names, types and order, and converts to it with a plain
// conversion; keep the two in step.
type Config struct {
	Enabled           bool
	ScanInterval      time.Duration
	MaxRuns           int
	MaxRunsPerTenant  int
	HeartbeatInterval time.Duration
	StaleAfter        time.Duration
	MaxLostOwners     int
	RetryDelay        time.Duration
	RetryDelayMax     time.Duration
	ShutdownDrain     time.Duration
}
