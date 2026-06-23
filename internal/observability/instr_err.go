package observability

import "log/slog"

// instrErr logs a non-nil OTel instrument-creation error. Instrument
// constructors only error on programming mistakes (invalid names), so callers
// proceed with the (nil) instrument; logging makes a missing metric diagnosable
// rather than silent.
func instrErr(name string, err error) {
	if err != nil {
		slog.Error("failed to create metric instrument",
			"pkg", "observability", "instrument", name, "error", err)
	}
}
