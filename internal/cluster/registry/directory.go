package registry

import (
	"bytes"
	"slices"
	"sync"

	"github.com/hashicorp/memberlist"
)

// directory is this pnode's own copy of the alive members, kept up to date by
// memberlist's event callbacks.
//
// It exists because memberlist.Members() hands out pointers into memberlist's
// node map, and memberlist rewrites those nodes' Meta, Addr and Port under a
// lock no caller can take: reading them from another goroutine is a data race
// on a slice header. Inside NotifyJoin, NotifyUpdate and NotifyLeave the node
// is handed over under that lock, so a copy taken there is safe, and it is the
// only place one can be taken. Everything else in this package reads members
// from here and never from memberlist.
//
// The callbacks run under memberlist's node lock, so set and remove do nothing
// but copy into a map under a mutex of their own, which is never held across a
// memberlist call or any I/O. Being state rather than a queue of events, the
// directory cannot lose a join or a leave.
type directory struct {
	mu      sync.RWMutex
	members map[string]memberlist.Node
}

func newDirectory() *directory {
	return &directory{members: make(map[string]memberlist.Node)}
}

// set records a private copy of n. It must be called from a memberlist event
// callback, where n is stable.
func (d *directory) set(n *memberlist.Node) {
	cp := *n
	cp.Addr = slices.Clone(n.Addr)
	cp.Meta = bytes.Clone(n.Meta)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.members[cp.Name] = cp
}

func (d *directory) remove(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.members, name)
}

// get returns a copy of the member called name. Its Meta and Addr are shared
// with the directory's copy and are never written after set.
func (d *directory) get(name string) (*memberlist.Node, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	n, ok := d.members[name]
	if !ok {
		return nil, false
	}
	return &n, true
}

// all returns a copy of every alive member.
func (d *directory) all() []*memberlist.Node {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]*memberlist.Node, 0, len(d.members))
	for _, n := range d.members {
		out = append(out, &n)
	}
	return out
}
