package app

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
)

func storeAt(t time.Time, called *bool) func(context.Context) (time.Time, error) {
	return func(context.Context) (time.Time, error) {
		*called = true
		return t, nil
	}
}

func TestCheckClockAtStartup_RecordedAheadFails(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var called bool
	err := checkClockAtStartup(context.Background(), platformclock.NewTest("n1", nil), storeAt(now.Add(time.Hour), &called), now)
	if err == nil {
		t.Fatal("a store that recorded times ahead of real time passed the startup check")
	}
}

func TestCheckClockAtStartup_RecordedBehindPasses(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var called bool
	if err := checkClockAtStartup(context.Background(), platformclock.NewTest("n1", nil), storeAt(now.Add(-24*time.Hour), &called), now); err != nil {
		t.Fatalf("a store that recorded times behind real time failed the startup check: %v", err)
	}
	if !called {
		t.Fatal("the store was not asked")
	}
}

// A setting taken from a peer is the cluster's clock: the node runs on it and
// does not check the store against real time.
func TestCheckClockAtStartup_PeerSettingSkipsCheck(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := platformclock.NewTest("n1", nil)
	ahead := now.Add(48 * time.Hour).UnixMicro()
	if !c.Apply(platformclock.Setting{StartUnixMicro: ahead, AnchorUnixMicro: now.UnixMicro(), RatePPM: 1_000_000, Version: 1, Writer: "peer"}) {
		t.Fatal("setting not applied")
	}
	var called bool
	if err := checkClockAtStartup(context.Background(), c, storeAt(now.Add(time.Hour), &called), now); err != nil {
		t.Fatalf("startup check with a peer setting: %v", err)
	}
	if called {
		t.Fatal("the store was asked although a peer setting is present")
	}
}
