package scheduledtask

import (
	"encoding/base64"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestCursor_RoundTrip(t *testing.T) {
	for name, c := range map[string]spi.ScheduledTaskCursor{
		"typical":         {ScheduledTime: 1700000000123, ID: "0123456789abcdef0123456789abcdef"},
		"zero time":       {ScheduledTime: 0, ID: "a"},
		"negative time":   {ScheduledTime: -1, ID: "b"},
		"largest time":    {ScheduledTime: 1<<63 - 1, ID: "0123456789abcdef0123456789abcdef"},
		"id needs escape": {ScheduledTime: 5, ID: `a"<b>`},
	} {
		got, err := decodeCursor(encodeCursor(c))
		if err != nil || got != c {
			t.Errorf("%s: round trip %+v -> %+v, %v", name, c, got, err)
		}
	}
}

// TestCursor_LongestRealFitsCap: the longest cursor a store can hand out — the
// most negative time and a 32-character task id — stays under the cap.
func TestCursor_LongestRealFitsCap(t *testing.T) {
	s := encodeCursor(spi.ScheduledTaskCursor{ScheduledTime: -1 << 63, ID: "0123456789abcdef0123456789abcdef"})
	if len(s) > maxCursorLen {
		t.Fatalf("longest real cursor is %d characters, want <= %d", len(s), maxCursorLen)
	}
	if _, err := decodeCursor(s); err != nil {
		t.Fatalf("longest real cursor does not decode: %v", err)
	}
}

func TestCursor_Rejects(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, c := range map[string]string{
		"empty":           "",
		"over the cap":    strings.Repeat("A", maxCursorLen+1),
		"not base64url":   "!!!",
		"padded base64":   base64.URLEncoding.EncodeToString([]byte(`{"v":1,"t":1,"i":"ab"}`)),
		"not json":        enc("nope"),
		"wrong version":   enc(`{"v":2,"t":1,"i":"a"}`),
		"no version":      enc(`{"t":1,"i":"a"}`),
		"empty id":        enc(`{"v":1,"t":1,"i":""}`),
		"no id":           enc(`{"v":1,"t":1}`),
		"time as string":  enc(`{"v":1,"t":"1","i":"a"}`),
		"time fractional": enc(`{"v":1,"t":1.5,"i":"a"}`),
		"time exponent":   enc(`{"v":1,"t":1e3,"i":"a"}`),
		"unknown field":   enc(`{"v":1,"t":1,"i":"a","x":1}`),
		"trailing data":   enc(`{"v":1,"t":1,"i":"a"}x`),
		"whitespace":      enc(`{"v":1, "t":1,"i":"a"}`),
		"key order":       enc(`{"t":1,"v":1,"i":"a"}`),
		"upper-case key":  enc(`{"V":1,"t":1,"i":"a"}`),
		"no time":         enc(`{"v":1,"i":"a"}`),
		"leading zero":    enc(`{"v":1,"t":01,"i":"a"}`),
	} {
		if _, err := decodeCursor(c); err == nil {
			t.Errorf("%s: cursor %q accepted, want errBadCursor", name, c)
		}
	}
}
