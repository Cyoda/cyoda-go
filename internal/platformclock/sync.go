package platformclock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// StateExchanger is the gossip state channel of a join: local supplies this
// node's state to a peer, merge receives a peer's. merge runs on the gossip
// goroutine and must not block.
type StateExchanger interface {
	SetStateExchange(local func() []byte, merge func([]byte))
}

var (
	minMicro = minTime.UnixMicro()
	maxMicro = maxTime.UnixMicro()
)

// decode parses and validates a setting received from the network.
func decode(payload []byte) (Setting, error) {
	var s Setting
	if err := json.Unmarshal(payload, &s); err != nil {
		return Setting{}, errors.New("undecodable payload")
	}
	switch {
	case s.RatePPM < minRatePPM || s.RatePPM > maxRatePPM:
		return Setting{}, errors.New("rate out of range")
	case s.AnchorUnixMicro < minMicro || s.AnchorUnixMicro > maxMicro:
		return Setting{}, errors.New("anchor out of range")
	case s.StartUnixMicro < minMicro || s.StartUnixMicro > maxMicro:
		return Setting{}, errors.New("start out of range")
	case s.Writer == "":
		return Setting{}, errors.New("empty writer")
	}
	return s, nil
}

// receive validates payload and applies it when newer. Invalid payloads are
// dropped with a WARN that never includes the payload.
func receive(c *Test, payload []byte, adopted *adoptLog) {
	s, err := decode(payload)
	if err != nil {
		slog.Warn("dropping invalid platform clock setting",
			"pkg", "platformclock", "reason", err.Error(), "size", len(payload))
		return
	}
	if c.Apply(s) {
		adopted.log(c.src, s, c.Now())
	}
}

// adoptLog throttles the adopted-setting INFO line to one per progressEvery
// (real time), as a move throttles its own: a move pushes one setting per
// step, and every peer adopts each of them. The lines in between are DEBUG.
type adoptLog struct {
	mu   sync.Mutex
	last time.Time
}

func (a *adoptLog) log(src func() time.Time, s Setting, now time.Time) {
	level := slog.LevelDebug
	func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if t := src(); t.Sub(a.last) >= progressEvery {
			a.last = t
			level = slog.LevelInfo
		}
	}()
	slog.Log(context.Background(), level, "platform clock setting adopted", "pkg", "platformclock",
		"version", s.Version, "writer", s.Writer, "now", stamp(now))
}

// Attach registers c's setting with gossip's join-time state exchange, so a
// joining node takes the cluster's setting before it finishes joining. The
// setting is exchanged on a join only: a change reaches the running nodes by
// its push, never by gossip. The exchange only covers a node that calls Attach
// before Register.
func Attach(c *Test, ex StateExchanger) {
	adopted := &adoptLog{}
	ex.SetStateExchange(
		func() []byte {
			// Only a pushed or adopted setting: a local change still
			// pending (a set-back awaiting the store) may yet be undone.
			cur := c.shareable()
			if cur == nil {
				return nil // nothing pushed or adopted: nothing to carry
			}
			raw, _ := json.Marshal(*cur) // integers and a string: cannot fail
			return raw
		},
		func(payload []byte) { receive(c, payload, adopted) },
	)
}

// ErrClockControlOff is what a node refuses a pushed setting with when it does
// not run a controllable clock.
var ErrClockControlOff = errors.New("clock control is off on this node")

// Holder holds a setting pushed to this node by a peer and answers with the
// setting it then holds.
type Holder struct {
	c       *Test
	adopted adoptLog
}

// NewHolder returns a Holder over c. A nil c means clock control is off.
func NewHolder(c *Test) *Holder { return &Holder{c: c} }

// Hold validates payload, applies it when newer, and returns the setting this
// node holds afterwards: payload's when it was adopted or equal, the held one
// when payload is older. An invalid payload is refused with an error that
// names the reason and never carries the payload; the caller logs it.
func (h *Holder) Hold(payload []byte) (Setting, error) {
	if h.c == nil {
		return Setting{}, ErrClockControlOff
	}
	s, err := decode(payload)
	if err != nil {
		return Setting{}, fmt.Errorf("invalid clock setting: %w", err)
	}
	if h.c.Apply(s) {
		h.adopted.log(h.c.src, s, h.c.Now())
	}
	return h.c.Setting(), nil
}
