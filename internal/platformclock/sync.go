package platformclock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// topic is the gossip broadcast topic carrying the test clock's setting.
const topic = "platform.clock"

// StateExchanger is the gossip push/pull state channel: local supplies this
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
// (real time), as a move throttles its own: a move publishes one setting per
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

// Attach subscribes c to settings broadcast on b and registers c's setting
// with the push/pull state exchange, so a joining node converges before it
// finishes joining. The join-time exchange only covers a node that calls
// Attach before Register.
func Attach(c *Test, b spi.ClusterBroadcaster, ex StateExchanger) {
	adopted := &adoptLog{}
	b.Subscribe(topic, func(payload []byte) { receive(c, payload, adopted) })
	ex.SetStateExchange(
		func() []byte {
			// Only a published or adopted setting: a local change still
			// pending (a set-back awaiting the store) may yet be undone.
			cur := c.shareable()
			if cur == nil {
				return nil // nothing published or adopted: nothing to carry
			}
			raw, err := json.Marshal(*cur)
			if err != nil {
				slog.Warn("cannot encode platform clock setting",
					"pkg", "platformclock", "error", fmt.Sprint(err))
				return nil
			}
			return raw
		},
		func(payload []byte) {
			if len(payload) == 0 {
				return
			}
			receive(c, payload, adopted)
		},
	)
}

// Publish broadcasts s to the cluster.
func Publish(b spi.ClusterBroadcaster, s Setting) {
	raw, err := json.Marshal(s)
	if err != nil {
		slog.Warn("cannot encode platform clock setting",
			"pkg", "platformclock", "error", fmt.Sprint(err))
		return
	}
	b.Broadcast(topic, raw)
}
