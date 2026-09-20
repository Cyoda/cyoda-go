package registry

import (
	"maps"
	"slices"
	"sync"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// listVersion names one state of one pnode's tag list. Epoch is the pnode's
// process start in unix nanoseconds; it only has to differ between two lives
// of the same pnode, and epochs of different pnodes are never compared. Seq
// counts the changes within one life.
//
// Versions are compared for equality. They are ordered only within one epoch.
// A pnode that restarts under the same id with a clock that stepped backwards
// announces a lower epoch than its previous life, and must not be mistaken
// for an older self.
type listVersion struct {
	Epoch int64  `json:"e"`
	Seq   uint64 `json:"s"`
}

type heldList struct {
	version listVersion
	tags    map[string][]string
}

// acceptList decides whether a received list replaces what is held for its
// pnode. The version the pnode announces in its metadata is the authority: a
// list is taken when it carries exactly that version, or when it is a later
// seq of the announced epoch (it arrived before its metadata did) and also
// later than the seq already held (two such lists can arrive out of order).
// With nothing announced there is nothing to measure the list against, and
// the join event for that pnode fetches it.
func acceptList(msg, announced listVersion, haveAnnounced bool, held listVersion, haveHeld bool) bool {
	if !haveAnnounced {
		return false
	}
	if msg == announced {
		return true
	}
	if msg.Epoch != announced.Epoch || msg.Seq < announced.Seq {
		return false
	}
	if haveHeld && held.Epoch == msg.Epoch && msg.Seq <= held.Seq {
		return false
	}
	return true
}

// isCurrent reports whether the held list needs no fetch: it is the announced
// one, or a later seq of the announced epoch whose metadata has not arrived.
func isCurrent(held listVersion, haveHeld bool, announced listVersion) bool {
	if !haveHeld {
		return false
	}
	if held == announced {
		return true
	}
	return held.Epoch == announced.Epoch && held.Seq > announced.Seq
}

// normaliseTags returns a private copy with every tag slice sorted and
// de-duplicated, so that one set of tags has one representation. A tenant
// with no tags is kept: its cnode still serves callouts that ask for no tag.
func normaliseTags(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for tenant, tags := range in {
		cp := make([]string, len(tags))
		copy(cp, tags)
		slices.Sort(cp)
		out[tenant] = slices.Compact(cp)
	}
	return out
}

func copyTags(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for tenant, tags := range in {
		out[tenant] = slices.Clone(tags)
	}
	return out
}

func tagsEqual(a, b map[string][]string) bool {
	return maps.EqualFunc(a, b, func(x, y []string) bool { return slices.Equal(x, y) })
}

// tagStore holds this pnode's own list and the lists it holds for its peers.
// Stored maps are never mutated after they are stored; readers get copies.
type tagStore struct {
	self   string
	signal *common.ChangeSignal

	mu   sync.RWMutex
	own  heldList
	held map[string]heldList
}

func newTagStore(self string, epoch int64, signal *common.ChangeSignal) *tagStore {
	return &tagStore{
		self:   self,
		signal: signal,
		own:    heldList{version: listVersion{Epoch: epoch}, tags: map[string][]string{}},
		held:   make(map[string]heldList),
	}
}

// setOwn replaces this pnode's list. The next version, the announcement of it
// and the list are one locked step, so one version never names two lists. An
// equal set changes nothing and announces nothing. announce runs under the
// store's lock and must not call back into the store.
func (s *tagStore) setOwn(tags map[string][]string, announce func(listVersion) error) (bool, error) {
	norm := normaliseTags(tags)
	s.mu.Lock()
	defer s.mu.Unlock()
	if tagsEqual(s.own.tags, norm) {
		return false, nil
	}
	next := listVersion{Epoch: s.own.version.Epoch, Seq: s.own.version.Seq + 1}
	if err := announce(next); err != nil {
		return false, err
	}
	s.own = heldList{version: next, tags: norm}
	return true, nil
}

// ownList returns this pnode's version and a copy of its list, read together.
func (s *tagStore) ownList() (listVersion, map[string][]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.own.version, copyTags(s.own.tags)
}

// put stores a received list if acceptList allows it, and fires the change
// signal when the tags held for that pnode are different afterwards. A list
// naming this pnode is never accepted.
func (s *tagStore) put(node string, version listVersion, tags map[string][]string, announced listVersion, haveAnnounced bool) bool {
	if node == s.self {
		return false
	}
	norm := normaliseTags(tags)
	stored, changed := func() (bool, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		cur, have := s.held[node]
		if !acceptList(version, announced, haveAnnounced, cur.version, have) {
			return false, false
		}
		s.held[node] = heldList{version: version, tags: norm}
		return true, !have || !tagsEqual(cur.tags, norm)
	}()
	if changed {
		s.signal.Fire()
	}
	return stored
}

// current reports whether the list held for node needs no fetch.
func (s *tagStore) current(node string, announced listVersion) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cur, have := s.held[node]
	return isCurrent(cur.version, have, announced)
}

// tagsOf returns a copy of the tags held for node — this pnode's own list for
// its own id — and an empty map when none is held yet.
func (s *tagStore) tagsOf(node string) map[string][]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if node == s.self {
		return copyTags(s.own.tags)
	}
	return copyTags(s.held[node].tags)
}

// drop forgets the list of a pnode that left.
func (s *tagStore) drop(node string) bool {
	dropped := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, have := s.held[node]
		delete(s.held, node)
		return have
	}()
	if dropped {
		s.signal.Fire()
	}
	return dropped
}

// retain forgets every held list whose pnode is not in alive, and returns how
// many it dropped. It is the scan's repair for a leave event that was lost.
func (s *tagStore) retain(alive map[string]struct{}) int {
	n := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		n := 0
		for node := range s.held {
			if _, ok := alive[node]; !ok {
				delete(s.held, node)
				n++
			}
		}
		return n
	}()
	if n > 0 {
		s.signal.Fire()
	}
	return n
}
