package clusterstatus

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/lifecycle"
)

// reaskInterval is the longest a wait goes between rounds without a directory
// change: a member's metadata update, such as a state change, does not fire
// Changed().
const reaskInterval = time.Second

// localNodeID is the single-node member's id.
const localNodeID = "local"

// Member is one directory member as the status read sees it. An empty Addr
// means there is no usable address; an empty State means the member's
// metadata names none.
type Member struct {
	ID, Addr string
	State    lifecycle.State
}

// Directory lists every directory member, this node included. Listing cannot
// fail: it reads the node's own view of the directory.
type Directory interface {
	Members() []Member
	Changed() <-chan struct{}
}

// Asker asks one member for its status.
type Asker interface {
	AskStatus(ctx context.Context, nodeID, addr string) (NodeStatus, error)
}

// Reachability says whether a member answered the round.
type Reachability string

const (
	Up   Reachability = "UP"
	Down Reachability = "DOWN"
)

// Entry is one member's line in a report. Status is nil when the member is DOWN;
// State is then the one its directory metadata names, or empty.
type Entry struct {
	ID           string
	Reachability Reachability
	State        lifecycle.State
	Status       *NodeStatus
}

// Count is the number of entries with one (reachability, state) combination.
type Count struct {
	Reachability Reachability
	State        lifecycle.State
	Count        int
}

// Report is the cluster status: one entry per member, in ascending id order.
type Report struct {
	Operational bool
	Counts      []Count
	Nodes       []Entry
}

// Service reads the cluster status.
type Service struct {
	dir        Directory
	ask        Asker
	askTimeout time.Duration
}

// NewService builds the status read. Each member's ask is bounded by askTimeout.
func NewService(dir Directory, ask Asker, askTimeout time.Duration) *Service {
	return &Service{dir: dir, ask: ask, askTimeout: askTimeout}
}

// Read answers the cluster status. With waitFor 0 it answers after one round.
// Otherwise it runs rounds until one has at least waitFor UP/NORMAL entries, or
// until timeout has passed, when it answers with the last counted round (every
// ask returned before the deadline). The first round always completes. Its
// only error is common.ClientGone, when the caller's context ends during an
// ask or a wait.
func (s *Service) Read(ctx context.Context, waitFor int, timeout time.Duration) (Report, error) {
	// realtime: the wait for waitFor nodes is bounded by a wall-clock timeout
	deadline := time.Now().Add(timeout)
	changed := s.dir.Changed()
	last, cut := s.round(ctx)
	if cut {
		return Report{}, common.ClientGone(ctx.Err())
	}
	for waitFor > 0 && upNormal(last) < waitFor {
		// realtime: the wait for waitFor nodes is bounded by a wall-clock timeout
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		if err := wait(ctx, changed, min(reaskInterval, remaining)); err != nil {
			return Report{}, err
		}
		changed = s.dir.Changed()
		entries, cut, err := s.roundBefore(ctx, deadline)
		if err != nil {
			return Report{}, err
		}
		if cut {
			break
		}
		last = entries
	}
	return Report{Operational: operational(last, waitFor), Counts: counts(last), Nodes: last}, nil
}

// wait returns when changed fires or d passes, or ClientGone when ctx ends first.
func wait(ctx context.Context, changed <-chan struct{}, d time.Duration) error {
	// realtime: the re-ask interval and the wait's deadline are wall-clock bounds
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return common.ClientGone(ctx.Err())
	case <-changed:
	case <-timer.C:
	}
	return nil
}

// roundBefore runs a round that the deadline calls off. cut reports that one
// of its asks returned after the deadline, and the round's entries are then not
// an answer. A round whose asks all returned before the deadline counts, even
// when the round ends after it.
func (s *Service) roundBefore(ctx context.Context, deadline time.Time) (entries []Entry, cut bool, err error) {
	// realtime: asks still out at the wait's deadline are called off
	rctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	entries, cut = s.round(rctx)
	if !cut {
		return entries, false, nil
	}
	if ctx.Err() != nil {
		return nil, false, common.ClientGone(ctx.Err())
	}
	return nil, true, nil
}

// round lists the members and asks each one with an address for its status,
// all in parallel. A member that does not answer is DOWN with its directory
// state. cut reports that an ask returned after ctx ended: ctx's end may have
// decided that answer, and the entries are then not a round. A round whose
// asks all returned before ctx ended is complete, however late it ends.
func (s *Service) round(ctx context.Context) (entries []Entry, cut bool) {
	members := s.dir.Members()
	entries = make([]Entry, len(members))
	var cutAsk atomic.Bool
	var wg sync.WaitGroup
	for i, m := range members {
		entries[i] = Entry{ID: m.ID, Reachability: Down, State: m.State}
		if m.Addr == "" {
			slog.Debug("cluster status: member has no usable address", "pkg", "clusterstatus", "memberId", m.ID)
			continue
		}
		wg.Go(func() {
			st, ok, late := s.askOne(ctx, m)
			if late {
				cutAsk.Store(true)
				return
			}
			if ok {
				entries[i] = Entry{ID: m.ID, Reachability: Up, State: st.State, Status: &st}
			}
		})
	}
	wg.Wait()
	if cutAsk.Load() {
		return nil, true
	}
	slices.SortFunc(entries, func(a, b Entry) int { return cmp.Compare(a.ID, b.ID) })
	return entries, false
}

// askOne asks one member under the ask bound. An ask error covers an answer out
// of bounds: the peer asker validates the answer it receives, and the
// in-process asker answers this node's own status. late reports that the ask
// returned after ctx ended, which may have shaped its outcome: the in-process
// ask, for one, answers its database check failed. The address and the error
// text go to the log only: they can name hosts and ports.
func (s *Service) askOne(ctx context.Context, m Member) (st NodeStatus, ok, late bool) {
	// realtime: each member's ask is bounded by a wall-clock timeout
	actx, cancel := context.WithTimeout(ctx, s.askTimeout)
	defer cancel()
	st, err := s.ask.AskStatus(actx, m.ID, m.Addr)
	if ctx.Err() != nil {
		return NodeStatus{}, false, true
	}
	if err != nil {
		slog.Debug("cluster status: member did not answer", "pkg", "clusterstatus", "memberId", m.ID, "addr", m.Addr, "err", err)
		return NodeStatus{}, false, false
	}
	return st, true, false
}

func isUpNormal(e Entry) bool { return e.Reachability == Up && e.State == lifecycle.Normal }

func upNormal(entries []Entry) int {
	n := 0
	for _, e := range entries {
		if isUpNormal(e) {
			n++
		}
	}
	return n
}

// operational: at least one entry; every entry UP, NORMAL and with its database
// up; one version and one clock across them; and, with waitFor, at least
// waitFor UP/NORMAL entries.
func operational(entries []Entry, waitFor int) bool {
	if len(entries) == 0 || len(entries) < waitFor {
		return false
	}
	first := entries[0].Status
	for _, e := range entries {
		if !isUpNormal(e) || !e.Status.Database {
			return false
		}
		if e.Status.Version != first.Version || e.Status.Clock != first.Clock {
			return false
		}
	}
	return true
}

// counts gives one item per (reachability, state) present: UP before DOWN,
// then in lifecycle.States() order, with the stateless item last. With no
// entries it is empty, never nil: the answer's counts is a required array.
func counts(entries []Entry) []Count {
	rank := func(st lifecycle.State) int {
		if i := slices.Index(lifecycle.States(), st); i >= 0 {
			return i
		}
		return len(lifecycle.States())
	}
	reach := func(r Reachability) int {
		if r == Up {
			return 0
		}
		return 1
	}
	out := []Count{}
	for _, e := range entries {
		i := slices.IndexFunc(out, func(c Count) bool { return c.Reachability == e.Reachability && c.State == e.State })
		if i < 0 {
			out = append(out, Count{Reachability: e.Reachability, State: e.State, Count: 1})
			continue
		}
		out[i].Count++
	}
	slices.SortFunc(out, func(a, b Count) int {
		return cmp.Or(cmp.Compare(reach(a.Reachability), reach(b.Reachability)), cmp.Compare(rank(a.State), rank(b.State)))
	})
	return out
}

type inProcess struct{ self *Self }

// InProcess is the single-node asker: it reads this node's own status and
// ignores the address.
func InProcess(self *Self) Asker { return inProcess{self: self} }

func (a inProcess) AskStatus(ctx context.Context, _, _ string) (NodeStatus, error) {
	return a.self.Status(ctx), nil
}

type localDirectory struct{ lc *lifecycle.Lifecycle }

// LocalDirectory is the single-node directory: one member, local, whose
// Changed() never fires.
func LocalDirectory(lc *lifecycle.Lifecycle) Directory { return localDirectory{lc: lc} }

// Members lists the one local member. Its address is a placeholder: the
// in-process asker ignores it, and an empty address would read as unusable.
func (d localDirectory) Members() []Member {
	return []Member{{ID: localNodeID, Addr: "in-process", State: d.lc.State()}}
}

// Changed returns a nil channel, which never fires.
func (localDirectory) Changed() <-chan struct{} { return nil }
