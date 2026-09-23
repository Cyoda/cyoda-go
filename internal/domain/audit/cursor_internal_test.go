package audit

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCursor_RoundTrip(t *testing.T) {
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	instants := map[string]time.Time{
		"sub-second":     base.Add(123456789 * time.Nanosecond),
		"whole second":   base,                                  // RFC3339Nano trims the fractional part entirely
		"trailing zeros": base.Add(120000000 * time.Nanosecond), // .120000000s → RFC3339Nano trims to ".12"
	}
	for name, at := range instants {
		for _, k := range []eventKey{
			{at: at, kind: "EntityChange", version: 7},
			{at: at, kind: "StateMachine", eventID: uuid.MustParse("5f1c1b0e-6d1a-11f1-8000-000000000001")},
		} {
			got, err := decodeCursor(encodeCursor(k))
			if err != nil || compareKeys(got, k) != 0 || !got.at.Equal(k.at) {
				t.Fatalf("%s: round trip %+v → %+v, %v", name, k, got, err)
			}
		}
	}
}

// TestCursor_LongestReal_FitsCap encodes the longest canonical cursor shape
// (a StateMachine key with a 9-digit-nanosecond timestamp) and checks it
// fits under maxCursorLen with room to spare, and that it still round-trips.
func TestCursor_LongestReal_FitsCap(t *testing.T) {
	at := time.Date(2026, 9, 23, 10, 0, 0, 123456789, time.UTC)
	k := eventKey{at: at, kind: "StateMachine", eventID: uuid.MustParse("5f1c1b0e-6d1a-11f1-8000-000000000001")}
	s := encodeCursor(k)
	if len(s) > maxCursorLen {
		t.Fatalf("longest real cursor is %d chars, want <= %d: %q", len(s), maxCursorLen, s)
	}
	got, err := decodeCursor(s)
	if err != nil || compareKeys(got, k) != 0 {
		t.Fatalf("round trip %+v -> %+v, %v", k, got, err)
	}
}

// TestCursor_RejectsOverLength confirms a cursor longer than maxCursorLen is
// rejected before any base64/JSON decoding is attempted, so an oversized
// cursor cannot force wasted decode work.
func TestCursor_RejectsOverLength(t *testing.T) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	long := make([]byte, maxCursorLen+1)
	for i := range long {
		long[i] = alphabet[i%len(alphabet)]
	}
	if _, err := decodeCursor(string(long)); err == nil {
		t.Errorf("%d-char cursor of valid base64url characters accepted, want errBadCursor", len(long))
	}
}

func TestCursor_Rejects(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, c := range map[string]string{
		"old offset":      "20",
		"not base64":      "!!!",
		"not json":        enc("nope"),
		"wrong version":   enc(`{"v":2,"t":"2026-09-23T10:00:00Z","k":"EntityChange","n":1}`),
		"bad time":        enc(`{"v":1,"t":"yesterday","k":"EntityChange","n":1}`),
		"unknown kind":    enc(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"System","n":1}`),
		"version zero":    enc(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"EntityChange","n":0}`),
		"ec with eventId": enc(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"EntityChange","n":1,"e":"5f1c1b0e-6d1a-11f1-8000-000000000001"}`),
		"sm bad eventId":  enc(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"StateMachine","e":"x"}`),
		"sm with version": enc(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"StateMachine","n":1,"e":"5f1c1b0e-6d1a-11f1-8000-000000000001"}`),
		"unknown field":   enc(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"EntityChange","n":1,"x":1}`),

		// One spelling per position: each of these decodes to a structurally
		// valid key but is not the canonical encoding of it, so the
		// encodeCursor(k) == s round-trip check must reject it.
		"trailing data":     enc(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"EntityChange","n":1}` + "x"),
		"uppercase key":     enc(`{"V":1,"t":"2026-09-23T10:00:00Z","k":"EntityChange","n":1}`),
		"non-UTC offset":    enc(`{"v":1,"t":"2026-09-23T10:00:00+02:00","k":"EntityChange","n":1}`),
		"braced uuid":       enc(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"StateMachine","e":"{5f1c1b0e-6d1a-11f1-8000-000000000001}"}`),
		"re-encoded padded": base64.URLEncoding.EncodeToString([]byte(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"EntityChange","n":1}`)),

		// The nil UUID: a spelling that WOULD round-trip (its canonical
		// string is exactly what encodeCursor would emit) but is rejected
		// outright — no store ever assigns it.
		"nil uuid": enc(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"StateMachine","e":"00000000-0000-0000-0000-000000000000"}`),
	} {
		if _, err := decodeCursor(c); err == nil {
			t.Errorf("%s: %q accepted", name, c)
		}
	}
}
