package audit

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCursor_RoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 23, 10, 0, 0, 123456789, time.UTC)
	for _, k := range []eventKey{
		{at: at, kind: "EntityChange", version: 7},
		{at: at, kind: "StateMachine", eventID: uuid.MustParse("5f1c1b0e-6d1a-11f1-8000-000000000001")},
	} {
		got, err := decodeCursor(encodeCursor(k))
		if err != nil || compareKeys(got, k) != 0 || !got.at.Equal(k.at) {
			t.Fatalf("round trip %+v → %+v, %v", k, got, err)
		}
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
	} {
		if _, err := decodeCursor(c); err == nil {
			t.Errorf("%s: %q accepted", name, c)
		}
	}
}
