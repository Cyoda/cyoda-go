package scheduler

import (
	"testing"
	"time"
)

func TestRealClock_NowTracksWallClock(t *testing.T) {
	c := NewRealClock()
	before := time.Now()
	got := c.Now()
	after := time.Now()

	if got.Before(before) || got.After(after) {
		t.Errorf("Now() = %v, want between %v and %v", got, before, after)
	}
}

func TestRealClock_ImplementsClock(t *testing.T) {
	var _ Clock = NewRealClock()
	var _ Clock = realClock{}
}
