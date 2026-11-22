package platformclock

import "time"

// A setting received from a peer lies within the years 1970 to 9999, the range
// a stored timestamp carries. A change may set or move the clock no later than
// maxSetTime, the end of the year 9000: from there the clock needs about ten
// years of running at the highest rate to reach the end of 9999.
var (
	minTime    = time.Unix(0, 0).UTC()
	maxTime    = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	maxSetTime = time.Date(9000, 12, 31, 23, 59, 59, 999999000, time.UTC)
)

// Rate bounds for Setting.RatePPM (parts per million; 1_000_000 is real time).
// The clock does not enforce them; the API layer validates before changing it.
const (
	minRatePPM = 100_000
	maxRatePPM = 100_000_000
)

// Setting is a replicable description of the test clock: at the wall instant
// AnchorUnixMicro the platform time was StartUnixMicro, advancing at RatePPM.
type Setting struct {
	StartUnixMicro  int64
	AnchorUnixMicro int64
	RatePPM         int64
	Version         uint64
	Writer          string
}

// Newer reports whether a supersedes b, ordering by (Version, Writer).
func (a Setting) Newer(b Setting) bool {
	if a.Version != b.Version {
		return a.Version > b.Version
	}
	return a.Writer > b.Writer
}
