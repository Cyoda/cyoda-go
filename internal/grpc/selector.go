package grpc

// MemberSelector decides which of several matching cnodes is tried next.
type MemberSelector interface {
	// Select picks one of candidates. candidates is never empty, and cnodes
	// already tried in the current run of the local procedure are not in it.
	Select(candidates []*Member) *Member
}

// RoundRobinSelector picks the candidate that was picked longest ago; among
// candidates never picked, the first in the order it was given. A cnode that
// has just attached has never been picked and therefore goes first.
//
// It keeps no state of its own. The stamp lives on the Member and the counter
// on the registry, so nothing grows with the number of tags — tag strings are
// supplied by tenants — and a cnode that goes away takes its stamp with it.
type RoundRobinSelector struct {
	registry *MemberRegistry
}

func NewRoundRobinSelector(registry *MemberRegistry) *RoundRobinSelector {
	return &RoundRobinSelector{registry: registry}
}

func (s *RoundRobinSelector) Select(candidates []*Member) *Member {
	return s.registry.pickLeastRecent(candidates)
}
