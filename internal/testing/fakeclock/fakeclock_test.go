package fakeclock

import (
	"testing"
	"time"
)

func TestFakeClock_SetAdvance(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c := New(t0)
	if !c.Now().Equal(t0) {
		t.Fatalf("Now = %v, want %v", c.Now(), t0)
	}
	c.Advance(90 * time.Second)
	if want := t0.Add(90 * time.Second); !c.Now().Equal(want) {
		t.Fatalf("after Advance Now = %v, want %v", c.Now(), want)
	}
	t1 := t0.Add(-time.Hour)
	c.Set(t1)
	if !c.Now().Equal(t1) {
		t.Fatalf("after Set Now = %v, want %v", c.Now(), t1)
	}
}
