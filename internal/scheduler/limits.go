package scheduler

import (
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const (
	// watchdogSlack is what W leaves for clock rate and scheduling.
	watchdogSlack = 10 * time.Second
	// heartbeatBudgetMax caps one heartbeat call, connection acquire included.
	heartbeatBudgetMax = 10 * time.Second
)

// MinStaleAfter is the smallest CYODA_SCHEDULER_STALE_AFTER a heartbeat
// interval allows: the commit budget, the watchdog slack, one heartbeat's
// budget and three intervals. With it, one slow or failed heartbeat never
// makes a pnode cancel its own runs.
func MinStaleAfter(heartbeat time.Duration) time.Duration {
	return common.CommitBudget + watchdogSlack + heartbeatBudgetMax + 3*heartbeat
}

// watchdogWindow is W: how long after a successful heartbeat's recorded start
// the pnode keeps its runs. A commit already under way when W passes still has
// the commit budget to land before another pnode can consider this one stale.
func watchdogWindow(staleAfter time.Duration) time.Duration {
	return staleAfter - common.CommitBudget - watchdogSlack
}

// heartbeatBudget bounds one heartbeat call: the connection acquire and the
// statement together.
func heartbeatBudget(interval time.Duration) time.Duration {
	return min(heartbeatBudgetMax, interval)
}
