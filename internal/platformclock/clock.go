// Package platformclock provides the platform's clock implementations.
package platformclock

import (
	"math"
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
	// shared is the newest setting this node may hand to a peer: one it
	// published or adopted from a peer, never a local change still pending.
	// nil until there is one.
	shared atomic.Pointer[Setting]
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
	now, _ := c.reading()
	return now
}

// reading returns the platform time and the setting it was computed from, off
// one load of the state.
func (c *Test) reading() (time.Time, Setting) {
	st := c.st.Load()
	return time.UnixMicro(st.platformMicro(c.src())), st.setting
}

// Setting returns the current setting.
func (c *Test) Setting() Setting { return c.st.Load().setting }

// Apply adopts s if it is newer than the current setting, converting its wall
// anchor once into a local monotonic anchor, and records s as shareable with
// peers. It reports whether s was taken.
func (c *Test) Apply(s Setting) bool {
	for {
		cur := c.st.Load()
		if !s.Newer(cur.setting) {
			return false
		}
		now := c.src()
		// The skew saturates at the Duration range (about 292 years): an anchor
		// further away than that would overflow the multiplication.
		const maxSkewMicro = int64(math.MaxInt64 / int64(time.Microsecond))
		skewMicro := max(-maxSkewMicro, min(maxSkewMicro, now.UnixMicro()-s.AnchorUnixMicro))
		skew := time.Duration(skewMicro) * time.Microsecond
		next := &state{setting: s, anchor: now.Add(-skew)}
		if c.st.CompareAndSwap(cur, next) {
			c.share(s)
			return true
		}
	}
}

// set makes a local change: at (nil keeps the current platform time) and
// ratePPM (nil keeps the rate). RatePPM must lie within [minRatePPM,
// MaxRatePPM]; callers validate, set does not. It bumps the version, stamps
// Writer with this node's id and returns the new setting.
func (c *Test) set(at *time.Time, ratePPM *int64) Setting {
	_, next := c.setState(at, ratePPM)
	return next.setting
}

// setState is set returning the state it replaced and the state it installed.
func (c *Test) setState(at *time.Time, ratePPM *int64) (prev, next *state) {
	prev, next, _ = c.install(func(current int64) (int64, bool) {
		if at == nil {
			return current, true
		}
		return at.UnixMicro(), true
	}, ratePPM)
	return prev, next
}

// setForward sets the clock to at if at is still ahead of it and applies
// ratePPM (nil keeps the rate). The comparison and the change are one atomic
// step, so the clock never moves back. With at not ahead, the rate alone is
// applied at the current time; with no rate either, nothing changes and it
// reports false.
func (c *Test) setForward(at time.Time, ratePPM *int64) (Setting, bool) {
	_, next, ok := c.install(func(current int64) (int64, bool) {
		if at.UnixMicro() > current {
			return at.UnixMicro(), true
		}
		return current, ratePPM != nil
	}, ratePPM)
	if !ok {
		return Setting{}, false
	}
	return next.setting, true
}

// install replaces the state in one compare-and-swap. start is given the
// current platform time (Unix microseconds) and returns the new start and
// whether to change at all; ratePPM nil keeps the rate.
func (c *Test) install(start func(current int64) (int64, bool), ratePPM *int64) (prev, next *state, ok bool) {
	for {
		cur := c.st.Load()
		now := c.src()
		begin, change := start(cur.platformMicro(now))
		if !change {
			return cur, nil, false
		}
		rate := cur.setting.RatePPM
		if ratePPM != nil {
			rate = *ratePPM
		}
		next := &state{
			setting: Setting{
				StartUnixMicro:  begin,
				AnchorUnixMicro: now.UnixMicro(),
				RatePPM:         rate,
				Version:         cur.setting.Version + 1,
				Writer:          c.nodeID,
			},
			anchor: now,
		}
		if c.st.CompareAndSwap(cur, next) {
			return cur, next, true
		}
	}
}

// restore puts prev back if installed is still the current state, undoing a
// local change that was never published. A setting adopted since is kept.
func (c *Test) restore(installed, prev *state) {
	c.st.CompareAndSwap(installed, prev)
}

// share records s as shareable with peers unless a newer one already is.
func (c *Test) share(s Setting) {
	for {
		cur := c.shared.Load()
		if cur != nil && !s.Newer(*cur) {
			return
		}
		if c.shared.CompareAndSwap(cur, &s) {
			return
		}
	}
}

// shareable returns the newest setting this node published or adopted from
// a peer, or nil when there is none.
func (c *Test) shareable() *Setting {
	return c.shared.Load()
}
