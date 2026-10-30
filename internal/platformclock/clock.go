// Package platformclock provides the platform's clock implementations.
package platformclock

import (
	"math/bits"
	"sync/atomic"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

var (
	_ spi.Clock = (*Test)(nil)
	_ spi.Clock = realClock{}
)

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Real returns a clock backed by time.Now().
func Real() spi.Clock { return realClock{} }

// state is immutable once published.
type state struct {
	setting Setting
	anchor  time.Time // local anchor carrying a monotonic reading
}

// Test is a controllable clock. Elapsed time is measured on the source's
// monotonic clock, so wall-clock steps do not move platform time.
type Test struct {
	nodeID string
	src    func() time.Time
	st     atomic.Pointer[state]
}

// NewTest returns an identity test clock (platform time tracks src). src
// defaults to time.Now and must return values with monotonic readings.
func NewTest(nodeID string, src func() time.Time) *Test {
	if src == nil {
		src = time.Now
	}
	now := src()
	c := &Test{nodeID: nodeID, src: src}
	c.st.Store(&state{
		setting: Setting{
			StartUnixMicro:  now.UnixMicro(),
			AnchorUnixMicro: now.UnixMicro(),
			RatePPM:         1_000_000,
		},
		anchor: now,
	})
	return c
}

// platformMicro returns platform time in Unix microseconds as of src instant now.
func (s *state) platformMicro(now time.Time) int64 {
	elapsed := now.Sub(s.anchor).Microseconds()
	if elapsed < 0 {
		elapsed = 0
	}
	rate := s.setting.RatePPM
	if rate < 0 {
		rate = 0
	}
	// elapsed*rate can exceed 64 bits; use a 128-bit product.
	hi, lo := bits.Mul64(uint64(elapsed), uint64(rate))
	var scaled uint64
	if hi >= 1_000_000 {
		scaled = 1<<63 - 1 // quotient would not fit; saturate
	} else {
		scaled, _ = bits.Div64(hi, lo, 1_000_000)
	}
	if scaled > 1<<62 {
		scaled = 1 << 62
	}
	return s.setting.StartUnixMicro + int64(scaled)
}

// Now returns the current platform time.
func (c *Test) Now() time.Time {
	return time.UnixMicro(c.st.Load().platformMicro(c.src()))
}

// Setting returns the current setting.
func (c *Test) Setting() Setting { return c.st.Load().setting }

// Apply adopts s if it is newer than the current setting, converting its wall
// anchor once into a local monotonic anchor. It reports whether s was taken.
func (c *Test) Apply(s Setting) bool {
	for {
		cur := c.st.Load()
		if !s.Newer(cur.setting) {
			return false
		}
		now := c.src()
		skew := time.Duration(now.UnixMicro()-s.AnchorUnixMicro) * time.Microsecond
		next := &state{setting: s, anchor: now.Add(-skew)}
		if c.st.CompareAndSwap(cur, next) {
			return true
		}
	}
}

// Set makes a local change: at (nil keeps the current platform time) and
// ratePPM (nil keeps the rate). RatePPM must lie within [MinRatePPM,
// MaxRatePPM]; callers validate, Set does not. It bumps the version, stamps
// Writer with this node's id and returns the new setting.
func (c *Test) Set(at *time.Time, ratePPM *int64) Setting {
	_, next := c.set(at, ratePPM)
	return next.setting
}

// set is Set returning the state it replaced and the state it installed.
func (c *Test) set(at *time.Time, ratePPM *int64) (prev, next *state) {
	for {
		cur := c.st.Load()
		now := c.src()
		start := cur.platformMicro(now)
		if at != nil {
			start = at.UnixMicro()
		}
		rate := cur.setting.RatePPM
		if ratePPM != nil {
			rate = *ratePPM
		}
		next := &state{
			setting: Setting{
				StartUnixMicro:  start,
				AnchorUnixMicro: now.UnixMicro(),
				RatePPM:         rate,
				Version:         cur.setting.Version + 1,
				Writer:          c.nodeID,
			},
			anchor: now,
		}
		if c.st.CompareAndSwap(cur, next) {
			return cur, next
		}
	}
}

// restore puts prev back if installed is still the current state, undoing a
// local change that was never published. A setting adopted since is kept.
func (c *Test) restore(installed, prev *state) {
	c.st.CompareAndSwap(installed, prev)
}
