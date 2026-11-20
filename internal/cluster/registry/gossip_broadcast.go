package registry

import (
	"encoding/binary"
	"log/slog"

	"github.com/hashicorp/memberlist"

	spi "github.com/cyoda/cyoda-go-spi"
)

// MaxTopicMsgSize caps the total encoded size of a gossip topic message
// (uvarint(len(topic)) + topic bytes + payload bytes). The cap is well
// above any practical gossip payload (cluster-state updates, cache
// invalidations, clock ticks — all tens of bytes) and well below
// math.MaxInt32, which guarantees the capacity argument to
// make([]byte, 0, N) in encodeTopicMsg cannot overflow (CWE-190).
//
// memberlist's own per-message cap is configurable but defaults to a
// few MiB for TCP push/pull; UDP is ~1400 bytes. 64 MiB is generous
// enough that no legitimate caller hits it, while small enough that an
// attacker-crafted topic+payload can't trigger an allocator overflow.
const MaxTopicMsgSize = 64 << 20

// Gossip satisfies spi.ClusterBroadcaster by multiplexing topics over the
// same memberlist instance it uses for membership. Every broadcast is sent
// on a single memberlist "user message" channel; the message is prefixed
// with a uvarint-length topic so the receiver's gossipDelegate can route
// it to the right handler. Semantics match the spi.ClusterBroadcaster
// contract: fire-and-forget, best-effort, no ordering or persistence.
var _ spi.ClusterBroadcaster = (*Gossip)(nil)

// Broadcast enqueues payload for fan-out delivery to every reachable node
// subscribed to topic. Returns immediately; delivery is asynchronous.
//
// Oversized messages (per MaxTopicMsgSize) are dropped with a logged
// ERROR rather than enqueued — encodeTopicMsg returns nil in that case.
func (g *Gossip) Broadcast(topic string, payload []byte) {
	msg := encodeTopicMsg(topic, payload)
	if msg == nil {
		return
	}
	g.delegate.queue.QueueBroadcast(&topicBroadcast{msg: msg})
}

// Subscribe registers handler for every broadcast received on topic. Multiple
// subscribers for the same topic are supported; handlers run on the
// memberlist receive goroutine (serialized per-message), so they must not
// block indefinitely.
func (g *Gossip) Subscribe(topic string, handler func(payload []byte)) {
	g.delegate.subscribe(topic, handler)
}

// topicBroadcast implements memberlist.Broadcast. Invalidates returns false
// because topic messages are semantically additive (clock updates, cache
// invalidations, etc.) — no instance of this type supersedes another.
type topicBroadcast struct{ msg []byte }

func (t *topicBroadcast) Invalidates(memberlist.Broadcast) bool { return false }
func (t *topicBroadcast) Message() []byte                       { return t.msg }
func (t *topicBroadcast) Finished()                             {}

// encodeTopicMsg prefixes payload with [uvarint(len(topic))][topic bytes]
// so the receiver can recover the topic without a separate envelope type.
//
// Returns nil if the encoded message would exceed MaxTopicMsgSize — this
// guards the make([]byte, 0, N) capacity computation against integer
// overflow (CWE-190) and drops the message before it hits the gossip
// queue. Callers MUST treat nil as drop.
func encodeTopicMsg(topic string, payload []byte) []byte {
	var lenBuf [binary.MaxVarintLen32]byte
	n := binary.PutUvarint(lenBuf[:], uint64(len(topic)))
	// Widen to int64 to avoid overflow in the addition itself when a
	// caller passes an attacker-controlled payload near math.MaxInt.
	total := int64(n) + int64(len(topic)) + int64(len(payload))
	if total > MaxTopicMsgSize {
		slog.Error("gossip topic message dropped: exceeds MaxTopicMsgSize",
			"pkg", "registry", "topic", topic, "size", total, "max", int64(MaxTopicMsgSize))
		return nil
	}
	out := make([]byte, 0, int(total))
	out = append(out, lenBuf[:n]...)
	out = append(out, topic...)
	out = append(out, payload...)
	return out
}

// decodeTopicMsg reverses encodeTopicMsg. Returns ok=false on any malformed
// input; callers should drop such messages (they cannot be safely dispatched).
func decodeTopicMsg(msg []byte) (topic string, payload []byte, ok bool) {
	topicLen, n := binary.Uvarint(msg)
	if n <= 0 {
		return "", nil, false
	}
	end := n + int(topicLen)
	if end > len(msg) {
		return "", nil, false
	}
	return string(msg[n:end]), msg[end:], true
}
