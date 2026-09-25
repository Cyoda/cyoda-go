package fixtureutil_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

func TestIncarnationFromLog(t *testing.T) {
	const id = "0b6f4a2e-3c1d-4e5f-8a9b-0c1d2e3f4a5b"
	line := `time=2026-09-24T10:00:00.000Z level=INFO msg="scheduler started" pkg=scheduler incarnation=` + id + "\n"
	got, err := fixtureutil.IncarnationFromLog("noise\n" + line + "more noise\n")
	if err != nil || got != uuid.MustParse(id) {
		t.Fatalf("IncarnationFromLog = %v, %v; want %s", got, err, id)
	}
	if _, err := fixtureutil.IncarnationFromLog("no scheduler here\n"); err == nil {
		t.Error("a log without the line gave an incarnation")
	}
	if _, err := fixtureutil.IncarnationFromLog(line + line); err == nil {
		t.Error("a log with two start lines gave an incarnation; one process starts one scheduler")
	}
}

// TestAwaitIncarnation polls: a line that appears after a few reads is found,
// and a log that never announces one is an error once the bound passes.
func TestAwaitIncarnation(t *testing.T) {
	const id = "0b6f4a2e-3c1d-4e5f-8a9b-0c1d2e3f4a5b"
	var reads atomic.Int32
	late := func() string {
		if reads.Add(1) < 3 {
			return "starting\n"
		}
		return `level=INFO msg="scheduler started" pkg=scheduler incarnation=` + id + "\n"
	}
	got, err := fixtureutil.AwaitIncarnation(late, 5*time.Second)
	if err != nil || got != uuid.MustParse(id) {
		t.Fatalf("AwaitIncarnation = %v, %v; want %s", got, err, id)
	}
	if reads.Load() < 3 {
		t.Errorf("read the log %d times; the line appears on the third read", reads.Load())
	}

	start := time.Now()
	if _, err := fixtureutil.AwaitIncarnation(func() string { return "no scheduler\n" }, 200*time.Millisecond); err == nil {
		t.Error("a log without the line gave an incarnation")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("gave up after %s; the bound was 200ms", elapsed)
	}
}
