package platformclock

import (
	"sync"
	"testing"
	"time"
)

// fakeSrc is a controllable monotonic source: values derive from a real
// time.Now() base plus an offset, so they carry monotonic readings.
type fakeSrc struct {
	mu   sync.Mutex
	base time.Time
	off  time.Duration
}

func newFakeSrc() *fakeSrc { return &fakeSrc{base: time.Now()} }
func (f *fakeSrc) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.base.Add(f.off)
}
func (f *fakeSrc) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.off += d
}

func near(a, b time.Time, tol time.Duration) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d <= tol
}

func TestReal_TracksWallClock(t *testing.T) {
	if !near(Real().Now(), time.Now(), time.Second) {
		t.Fatal("Real().Now() far from time.Now()")
	}
}

func TestTest_IdentityTracksSource(t *testing.T) {
	s := newFakeSrc()
	c := NewTest("n1", s.now)
	if got := c.Setting(); got.RatePPM != 1_000_000 || got.Version != 0 {
		t.Fatalf("identity setting = %+v", got)
	}
	s.advance(7 * time.Second)
	if !near(c.Now(), s.now(), time.Microsecond) {
		t.Fatalf("Now %v != src %v", c.Now(), s.now())
	}
}

func TestTest_SetPast_RunsAtRate(t *testing.T) {
	s := newFakeSrc()
	c := NewTest("n1", s.now)
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	rate := int64(2_000_000)
	c.set(&start, &rate)
	if !near(c.Now(), start, time.Millisecond) {
		t.Fatalf("Now = %v want %v", c.Now(), start)
	}
	s.advance(10 * time.Second)
	if !near(c.Now(), start.Add(20*time.Second), time.Millisecond) {
		t.Fatalf("Now = %v want %v", c.Now(), start.Add(20*time.Second))
	}
}

func TestTest_RateChangeDoesNotJump(t *testing.T) {
	s := newFakeSrc()
	c := NewTest("n1", s.now)
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	rate := int64(2_000_000)
	c.set(&start, &rate)
	s.advance(10 * time.Second)
	before := c.Now()
	r2 := int64(500_000)
	c.set(nil, &r2)
	if !near(c.Now(), before, time.Millisecond) {
		t.Fatalf("jump: %v -> %v", before, c.Now())
	}
	s.advance(10 * time.Second)
	if !near(c.Now(), before.Add(5*time.Second), time.Millisecond) {
		t.Fatalf("Now = %v want %v", c.Now(), before.Add(5*time.Second))
	}
}

func TestTest_ApplyIgnoresOlder(t *testing.T) {
	s := newFakeSrc()
	c := NewTest("n1", s.now)
	r := int64(3_000_000)
	newer := c.set(nil, &r)
	if c.Apply(Setting{Version: 0, Writer: "z", RatePPM: 1_000_000}) {
		t.Fatal("older setting applied")
	}
	if c.Setting() != newer {
		t.Fatalf("setting changed: %+v", c.Setting())
	}
}

func TestTest_ApplyConvertsAnchor(t *testing.T) {
	s1, s2 := newFakeSrc(), newFakeSrc()
	a := NewTest("a", s1.now)
	b := NewTest("b", s2.now)
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	rate := int64(2_000_000)
	set := a.set(&start, &rate)
	if !b.Apply(set) {
		t.Fatal("apply refused")
	}
	if !near(a.Now(), b.Now(), time.Millisecond) {
		t.Fatalf("a=%v b=%v", a.Now(), b.Now())
	}
}

func TestTest_NegativeElapsedCountsAsZero(t *testing.T) {
	s := newFakeSrc()
	c := NewTest("n1", s.now)
	future := s.now().Add(time.Hour).UnixMicro()
	c.Apply(Setting{StartUnixMicro: 1_000_000_000_000, AnchorUnixMicro: future, RatePPM: 1_000_000, Version: 1, Writer: "x"})
	if got := c.Now().UnixMicro(); got != 1_000_000_000_000 {
		t.Fatalf("Now = %d", got)
	}
}

func TestTest_HugeElapsedNoOverflow(t *testing.T) {
	s := newFakeSrc()
	c := NewTest("n1", s.now)
	start := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	rate := int64(MaxRatePPM)
	c.set(&start, &rate)
	s.advance(100 * 365 * 24 * time.Hour)
	want := time.UnixMicro(start.UnixMicro() + int64(100*365*24*time.Hour/time.Microsecond)*100)
	if !near(c.Now(), want, time.Millisecond) {
		t.Fatalf("Now = %v want %v", c.Now(), want)
	}
}

func TestSetting_NewerTieBreakByWriter(t *testing.T) {
	a := Setting{Version: 2, Writer: "a"}
	b := Setting{Version: 2, Writer: "b"}
	if !b.Newer(a) || a.Newer(b) || a.Newer(a) {
		t.Fatal("tie-break wrong")
	}
	if !(Setting{Version: 3, Writer: "a"}).Newer(b) {
		t.Fatal("version should dominate")
	}
}

func TestTest_ConcurrentUse(t *testing.T) {
	c := NewTest("n", time.Now)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := int64(1_500_000)
			for j := 0; j < 200; j++ {
				c.Now()
				c.set(nil, &r)
				c.Apply(c.Setting())
			}
		}()
	}
	wg.Wait()
}

// A peer anchor far from local time must not overflow the skew: an anchor in
// 9999 has had no elapsed time yet, one in 1970 has had the whole interval.
func TestApply_FarAnchors(t *testing.T) {
	start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		anchor time.Time
		want   func(now time.Time) time.Time
	}{
		{"year 9999", time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
			func(time.Time) time.Time { return start }},
		{"year 1970", time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
			func(now time.Time) time.Time {
				return start.Add(now.Sub(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)))
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newFakeSrc()
			c := NewTest("n1", src.now)
			if !c.Apply(Setting{StartUnixMicro: start.UnixMicro(), AnchorUnixMicro: tc.anchor.UnixMicro(),
				RatePPM: 1_000_000, Version: 1, Writer: "peer"}) {
				t.Fatal("setting not taken")
			}
			if got, want := c.Now(), tc.want(src.now()); !near(got, want, time.Second) {
				t.Fatalf("Now = %v, want %v", got, want)
			}
		})
	}
}
