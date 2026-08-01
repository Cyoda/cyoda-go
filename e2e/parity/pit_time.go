package parity

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// Point-in-time boundaries must be derived from the SERVER's timeline, never
// from the test process's clock.
//
// Version timestamps are stamped by the backend: the postgres plugin takes them
// from the database (`SELECT CURRENT_TIMESTAMP`), so on a testcontainer they
// come from the Docker VM's clock, not the host's. Under CPU load that clock
// has been measured lagging the host by 10–13 ms — more than the sleep margins
// these scenarios used to rely on. A `time.Now()` boundary compared against a
// server-stamped `valid_time` is therefore a two-clock comparison, and it
// resolves to the wrong version whenever the skew exceeds the sleep.
//
// The helpers below read timestamps back from the server so every comparison
// happens on a single clock. Sleeps around writes remain, but they now only
// guarantee that consecutive versions land in distinct (and, for the commercial
// backend, distinct millisecond) instants — which is a property of the server's
// own clock and so holds regardless of skew.

// LatestChangeTime returns the server-stamped time of the entity's most recent
// version. Use it instead of time.Now() when building a point-in-time boundary.
func LatestChangeTime(t *testing.T, c *client.Client, id uuid.UUID) time.Time {
	t.Helper()
	changes, err := c.GetEntityChanges(t, id)
	if err != nil {
		t.Fatalf("GetEntityChanges: %v", err)
	}
	return MaxChangeTime(t, changes)
}

// MaxChangeTime returns the newest TimeOfChange in an already-fetched change
// list. Callers holding a different client abstraction (e.g. the external-API
// driver) fetch the list themselves and reduce it here.
func MaxChangeTime(t *testing.T, changes []client.EntityChangeMeta) time.Time {
	t.Helper()
	if len(changes) == 0 {
		t.Fatal("entity change list is empty")
	}
	latest := changes[0].TimeOfChange
	for _, ch := range changes[1:] {
		if ch.TimeOfChange.After(latest) {
			latest = ch.TimeOfChange
		}
	}
	return latest
}

// MinChangeTime returns the oldest TimeOfChange in an already-fetched change
// list — the entity's creation instant on the server's clock.
func MinChangeTime(t *testing.T, changes []client.EntityChangeMeta) time.Time {
	t.Helper()
	if len(changes) == 0 {
		t.Fatal("entity change list is empty")
	}
	earliest := changes[0].TimeOfChange
	for _, ch := range changes[1:] {
		if ch.TimeOfChange.Before(earliest) {
			earliest = ch.TimeOfChange
		}
	}
	return earliest
}

// MidpointBetween returns an instant at or after earlier and strictly before
// later — a point-in-time boundary that resolves to the version written at
// earlier. (For sub-nanosecond gaps it degenerates to earlier itself, which
// still resolves correctly under the inclusive <= rule.)
//
// It fails the test if the two are not strictly ordered — that means the writes
// were not spaced far enough apart for the backend's timestamp resolution, and
// surfacing it here is clearer than an ambiguous as-at assertion downstream.
func MidpointBetween(t *testing.T, earlier, later time.Time) time.Time {
	t.Helper()
	if !earlier.Before(later) {
		t.Fatalf("version timestamps not strictly increasing: earlier=%s later=%s — "+
			"space the writes further apart for this backend's timestamp resolution",
			earlier.Format(time.RFC3339Nano), later.Format(time.RFC3339Nano))
	}
	return earlier.Add(later.Sub(earlier) / 2)
}
