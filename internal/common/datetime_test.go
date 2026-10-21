package common_test

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// TestParseDateTime: a date-time outside a body is read as a body's
// date-time member is, so one value has one reading at every door. RFC 3339
// separates fractional seconds with "." only, needs the time and the offset,
// and bounds the offset.
func TestParseDateTime(t *testing.T) {
	for _, s := range []string{
		"2024-01-02T03:04:05Z",
		"2024-01-02T03:04:05.5Z",
		"2024-01-02T03:04:05.123456789+01:00",
	} {
		if _, err := common.ParseDateTime(s); err != nil {
			t.Errorf("%q: %v, want accepted", s, err)
		}
	}
	got, err := common.ParseDateTime("2024-01-02T03:04:05.5+01:00")
	if want := time.Date(2024, 1, 2, 2, 4, 5, 500_000_000, time.UTC); err != nil || !got.Equal(want) {
		t.Errorf("got %v (err %v), want %v", got, err, want)
	}
	for _, s := range []string{
		"2024-01-02T03:04:05,5Z",
		"2024-01-02",
		"2024-01-02T03:04:05+24:00",
		"2024-01-02t03:04:05z",
		"",
	} {
		if _, err := common.ParseDateTime(s); err == nil {
			t.Errorf("%q: accepted, want refused", s)
		}
	}
}
