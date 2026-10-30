package platformclock

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Topic is the gossip broadcast topic carrying the test clock's setting.
const Topic = "platform.clock"

// StateExchanger is the gossip push/pull state channel: local supplies this
// node's state to a peer, merge receives a peer's. merge runs on the gossip
// goroutine and must not block.
type StateExchanger interface {
	SetStateExchange(local func() []byte, merge func([]byte))
}

var (
	minMicro = int64(0)
	maxMicro = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC).UnixMicro()
)

// decode parses and validates a setting received from the network.
func decode(payload []byte) (Setting, error) {
	var s Setting
	if err := json.Unmarshal(payload, &s); err != nil {
		return Setting{}, errors.New("undecodable payload")
	}
	switch {
	case s.RatePPM < MinRatePPM || s.RatePPM > MaxRatePPM:
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
func receive(c *Test, payload []byte) {
	s, err := decode(payload)
	if err != nil {
		slog.Warn("dropping invalid platform clock setting",
			"pkg", "platformclock", "reason", err.Error(), "size", len(payload))
		return
	}
	c.Apply(s)
}

// Attach subscribes c to settings broadcast on b and registers c's setting
// with the push/pull state exchange, so a joining node converges before it
// finishes joining. The join-time exchange only covers a node that calls
// Attach before Register.
func Attach(c *Test, b spi.ClusterBroadcaster, ex StateExchanger) {
	b.Subscribe(Topic, func(payload []byte) { receive(c, payload) })
	ex.SetStateExchange(
		func() []byte {
			cur := c.Setting()
			if cur.Version == 0 {
				return nil // never set: identity, nothing to carry
			}
			raw, err := json.Marshal(cur)
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
			receive(c, payload)
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
	b.Broadcast(Topic, raw)
}
