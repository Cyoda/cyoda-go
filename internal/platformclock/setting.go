package platformclock

// Rate bounds for Setting.RatePPM (parts per million; 1_000_000 is real time).
// Set does not enforce them; the API layer validates before calling Set.
const (
	MinRatePPM = 100_000
	MaxRatePPM = 100_000_000
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
