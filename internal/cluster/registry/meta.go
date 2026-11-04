package registry

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/cyoda-platform/cyoda-go/internal/lifecycle"
)

func marshalMeta(cfg GossipConfig, version listVersion, state lifecycle.State) ([]byte, error) {
	return json.Marshal(nodeMeta{ID: cfg.NodeID, Addr: cfg.NodeAddr, GRPCAddr: cfg.GRPCNodeAddr, Tags: version, State: state})
}

// metaBuilder composes this pnode's metadata from the two things in it that
// change: the version of its tag list and its lifecycle state. It holds both
// under one lock and stores metadata marshalled from both, so a change to one
// never stores a stale copy of the other.
//
// The lock order is the tag store, then the builder: setVersion runs inside
// tagStore.setOwn's announce callback, under the tag lock, and the builder
// never calls the tag store. setState runs under the lifecycle's lock; the
// builder never calls the lifecycle either.
type metaBuilder struct {
	cfg      GossipConfig
	delegate *gossipDelegate

	mu      sync.Mutex
	version listVersion
	state   lifecycle.State
}

// newMetaBuilder returns a builder holding version and state, and stores the
// metadata they make.
func newMetaBuilder(cfg GossipConfig, delegate *gossipDelegate, version listVersion, state lifecycle.State) (*metaBuilder, error) {
	raw, err := marshalMeta(cfg, version, state)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal node metadata: %w", err)
	}
	delegate.updateMeta(raw)
	return &metaBuilder{cfg: cfg, delegate: delegate, version: version, state: state}, nil
}

// setVersion stores metadata naming version and the current state. It is
// tagStore.setOwn's announce callback.
func (b *metaBuilder) setVersion(version listVersion) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.storeLocked(version, b.state); err != nil {
		return err
	}
	b.version = version
	return nil
}

// setState stores metadata naming the current version and state. changed is
// false, and nothing is stored, when state is the current one.
func (b *metaBuilder) setState(state lifecycle.State) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if state == b.state {
		return false, nil
	}
	if err := b.storeLocked(b.version, state); err != nil {
		return false, err
	}
	b.state = state
	return true, nil
}

// storeLocked marshals version and state and hands the metadata to the
// delegate. The caller records version and state only once it succeeds. b.mu
// must be held.
func (b *metaBuilder) storeLocked(version listVersion, state lifecycle.State) error {
	raw, err := marshalMeta(b.cfg, version, state)
	if err != nil {
		return fmt.Errorf("failed to marshal node metadata: %w", err)
	}
	b.delegate.updateMeta(raw)
	return nil
}
