package registry

// LocalAddr exposes the address memberlist derived for this node, for tests
// only: a node bound on an ephemeral port must be looked up after the fact
// rather than assumed in advance, and this is how a test that seeds a second
// node off the first one learns what the OS gave it. Production code has no
// equivalent need — peers learn a node's address through the join exchange
// itself, not by asking it directly.
func (g *Gossip) LocalAddr() string {
	return g.list.LocalNode().Address()
}
