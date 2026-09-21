package grpc

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// TagChangeFunc is called when the set of connected members changes, with the
// computed aggregate tags (tenantID → deduplicated tag list). A non-nil error
// means the tags were not published; the registry does not count that version
// as published, so the next change republishes.
type TagChangeFunc func(tags map[string][]string) error

// SendFunc is a function that sends a CloudEvent to a connected member's stream.
type SendFunc func(ce *cepb.CloudEvent) error

// ProcessingResponse holds the response from a processor or criteria calculation.
type ProcessingResponse struct {
	Payload json.RawMessage
	Success bool
	// NullSuccess is true when the answer's `success` key carried the literal
	// null: neither the schema's default, which belongs to an absent key, nor
	// a boolean. Such an answer cannot be read at all, so the dispatch refuses
	// it before it reads a verdict, a payload or a result out of it (see
	// reportedSuccess, in streaming.go). Success is left false beside it, so a
	// reader that knows only the flag still fails closed.
	NullSuccess bool
	Error       string
	Matches *bool // for criteria responses (nil for processor responses)
	// Reason is the criteria-response explanation for a matches=false result
	// (EntityCriteriaCalculationResponse.reason). Empty for processor
	// responses and for criteria that supply no reason.
	Reason string
	// Result is the function-response result payload (raw JSON object) from
	// EntityFunctionCalculationResponse.result. Nil/empty for processor and
	// criteria responses.
	Result json.RawMessage
	// ResultKind is the function-response discriminator string from
	// EntityFunctionCalculationResponse.resultKind. Empty for processor and
	// criteria responses.
	ResultKind string
	Warnings   []string // warnings from processor/criteria, propagated to client
	// Retryable carries the member-supplied retryable flag from the inbound
	// CloudEvent error shape (api/grpc/events/types.go: every *EventJsonError
	// variant declares Retryable *bool). The pointer is nil when the wire
	// omitted the key or when no error was present, distinguishing "wire
	// said so" from "wire didn't say". A failed try carries it on as
	// contract.CalloutFailure.Retryable: it never decides whether another
	// cnode is tried, only whether the client is told a re-run may help.
	Retryable *bool
	// Disconnected is true when this response was synthesized by
	// failAllPending because the member's stream dropped while the request
	// was in flight, rather than a substantive failure returned by the
	// member. dispatchCalloutToMember uses this to surface a distinguishable
	// 503 COMPUTE_MEMBER_DISCONNECTED instead of a generic failure.
	Disconnected bool
}

// ErrMemberEvicted is returned by Send, TrySend and TrackRequest once the
// member's stream is being torn down. A dispatcher that sees it reports
// COMPUTE_MEMBER_DISCONNECTED (retryable) rather than waiting out its timeout.
var ErrMemberEvicted = errors.New("compute member evicted")

// outboxItem is one event waiting for the writer, together with the context
// of the caller that queued it. The writer skips an item whose caller has
// already given up: a dispatcher that timed out has rolled back, so sending
// its request would only make the compute node do work nobody is waiting for.
type outboxItem struct {
	ce  *cepb.CloudEvent
	ctx context.Context
}

// Member represents a connected calculation member.
//
// Every write to the member's gRPC stream is performed by exactly one
// goroutine, the writer (writeLoop), which drains an unbuffered outbox.
// Callers hand events to the writer through Send/TrySend and never touch the
// stream. That is what makes concurrent dispatch, keep-alive and greet safe on
// one stream (grpc-go forbids concurrent SendMsg), and it is what keeps a
// frozen consumer from wedging anyone but the writer: a raw send blocked on a
// full HTTP/2 write window holds no lock any other goroutine wants.
//
// Eviction is a closed channel. The writer, the keep-alive loop, the receive
// goroutine, the stream handler and every blocked sender select on it; the
// stream handler returning is what makes grpc-go cancel the stream and unblock
// a raw send stuck in the write window.
type Member struct {
	ID          string
	TenantID    spi.TenantID
	Tags        []string
	ConnectedAt time.Time
	// pickStamp is the registry's pick counter at the moment this member was
	// last chosen for a try; 0 means never. Guarded by MemberRegistry.pickMu.
	pickStamp uint64

	send       SendFunc // raw stream write; called ONLY by writeLoop
	outbox     chan outboxItem
	evicted    chan struct{}
	evictOnce  sync.Once
	evictErr   error // written once inside evictOnce, before evicted is closed
	writerDone chan struct{}
	// writeStartedAt is the unix-nanosecond time the in-flight raw send began,
	// or 0 while the writer is idle. The keep-alive loop reads it: one write
	// that has been in flight longer than the keep-alive timeout means the
	// member is not draining, whatever its inbound traffic says.
	writeStartedAt atomic.Int64

	lastSeen   time.Time
	lastSeenMu sync.RWMutex

	pendingReqs map[string]chan *ProcessingResponse
	pendingMu   sync.Mutex
	// closed is set under pendingMu by Evict. TrackRequest refuses once it is
	// set, which closes the window between a dispatcher choosing this member
	// and registering its request against it.
	closed bool
}

func newMember(id string, tenantID spi.TenantID, tags []string, send SendFunc) *Member {
	now := time.Now()
	return &Member{
		ID:          id,
		TenantID:    tenantID,
		Tags:        tags,
		ConnectedAt: now,
		send:        send,
		outbox:      make(chan outboxItem),
		evicted:     make(chan struct{}),
		writerDone:  make(chan struct{}),
		lastSeen:    now,
		pendingReqs: make(map[string]chan *ProcessingResponse),
	}
}

// Send hands ce to the writer. It returns nil once the writer holds the
// event, ErrMemberEvicted if the member is gone, or ctx.Err() if the caller's
// deadline passed first. It never blocks on the stream itself.
func (m *Member) Send(ctx context.Context, ce *cepb.CloudEvent) error {
	select {
	case <-m.evicted:
		return ErrMemberEvicted
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.outbox <- outboxItem{ce: ce, ctx: ctx}:
		return nil
	case <-m.evicted:
		return ErrMemberEvicted
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TrySend is Send without waiting: true if the writer took the event
// immediately, false if it is busy or the member is gone. The keep-alive loop
// uses it so a ping never waits behind a stalled write.
func (m *Member) TrySend(ce *cepb.CloudEvent) bool {
	select {
	case <-m.evicted:
		return false
	default:
	}
	select {
	case m.outbox <- outboxItem{ce: ce, ctx: context.Background()}:
		return true
	default:
		return false
	}
}

// Evict marks the member gone: it refuses new tracked requests, fails every
// pending one with Disconnected, records err as the status the stream handler
// returns, and closes the evicted channel. Idempotent; the first error wins.
func (m *Member) Evict(err error) {
	m.evictOnce.Do(func() {
		m.evictErr = err
		m.failAllPending("member disconnected")
		close(m.evicted)
	})
}

// Evicted is closed once Evict has run.
func (m *Member) Evicted() <-chan struct{} { return m.evicted }

// EvictErr is the error passed to the first Evict. Only valid after Evicted()
// has fired; the channel close is what publishes the write.
func (m *Member) EvictErr() error { return m.evictErr }

// gone reports whether the member has been evicted. The closed channel is the
// one source of truth for it, so this needs no lock and no second flag.
func (m *Member) gone() bool {
	select {
	case <-m.evicted:
		return true
	default:
		return false
	}
}

// WriteInFlightSince is when the writer's current raw send began, or the zero
// time when no send is in flight.
func (m *Member) WriteInFlightSince() time.Time {
	n := m.writeStartedAt.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// WriterDone is closed when the writer goroutine has exited.
func (m *Member) WriterDone() <-chan struct{} { return m.writerDone }

// writeLoop is the member's single writer. first, when non-nil, is written
// before anything queued (the greet), so it is the first event on the wire.
func (m *Member) writeLoop(first *cepb.CloudEvent) {
	defer close(m.writerDone)
	defer func() {
		if rec := recover(); rec != nil {
			m.Evict(panicStatus(rec, "Member.writeLoop"))
		}
	}()
	if first != nil && !m.write(first) {
		return
	}
	for {
		select {
		case item := <-m.outbox:
			if item.ctx.Err() != nil {
				continue
			}
			if !m.write(item.ce) {
				return
			}
		case <-m.evicted:
			return
		}
	}
}

// write performs one raw send, bracketing it with writeStartedAt so the
// keep-alive loop can see a stall. A failed send evicts the member.
func (m *Member) write(ce *cepb.CloudEvent) bool {
	m.writeStartedAt.Store(time.Now().UnixNano())
	defer m.writeStartedAt.Store(0)
	err := m.send(ce)
	if err != nil {
		m.Evict(status.Error(codes.Unavailable, "send failed: "+err.Error()))
		return false
	}
	return true
}

// TrackRequest creates a buffered channel for the given requestID and stores
// it in the pending requests map. Returns ErrMemberEvicted once the member is
// gone, so a dispatcher that chose this member an instant before it left
// learns so immediately instead of waiting out its timeout.
func (m *Member) TrackRequest(requestID string) (chan *ProcessingResponse, error) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	if m.closed {
		return nil, ErrMemberEvicted
	}
	ch := make(chan *ProcessingResponse, 1)
	m.pendingReqs[requestID] = ch
	return ch, nil
}

// CompleteRequest delivers resp to the channel associated with requestID and
// removes the entry from the pending map. If the requestID is not found, this
// is a no-op.
func (m *Member) CompleteRequest(requestID string, resp *ProcessingResponse) {
	ch, ok := func() (chan *ProcessingResponse, bool) {
		m.pendingMu.Lock()
		defer m.pendingMu.Unlock()
		ch, ok := m.pendingReqs[requestID]
		if ok {
			delete(m.pendingReqs, requestID)
		}
		return ch, ok
	}()
	if ok {
		ch <- resp
	}
}

// AbandonRequest removes the requestID entry from the pending map, if still
// present, without touching the channel. Spec D11: dispatchCalloutToMember
// defers this call so every exit that does not consume a delivered response
// (Send failure, response timeout, ctx cancellation) still clears the entry
// — otherwise a late compute-node reply finds a dangling map entry that is
// never removed. Calling this after the response arm has already run
// CompleteRequest is a no-op (the entry is already gone), and a late reply
// arriving after this runs finds CompleteRequest's own presence check false,
// so it becomes a no-op too: no phantom delivery, no panic.
func (m *Member) AbandonRequest(requestID string) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	delete(m.pendingReqs, requestID)
}

// PendingCount returns the number of currently tracked pending requests.
// Exported for tests that need to observe pending-map cleanup without a race
// on the unexported field.
func (m *Member) PendingCount() int {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	return len(m.pendingReqs)
}

// failAllPending marks the member closed to new tracked requests, sends an
// error response to every pending request channel and clears the pending map.
// Called only by Evict, which is what makes "no request is ever registered
// against a member that is already going away" hold for every caller.
func (m *Member) failAllPending(errMsg string) {
	reqs := func() map[string]chan *ProcessingResponse {
		m.pendingMu.Lock()
		defer m.pendingMu.Unlock()
		m.closed = true
		reqs := m.pendingReqs
		m.pendingReqs = make(map[string]chan *ProcessingResponse)
		return reqs
	}()

	for _, ch := range reqs {
		ch <- &ProcessingResponse{
			Success:      false,
			Error:        errMsg,
			Disconnected: true,
		}
	}
}

// UpdateLastSeen sets lastSeen to the current time.
func (m *Member) UpdateLastSeen() {
	m.lastSeenMu.Lock()
	defer m.lastSeenMu.Unlock()
	m.lastSeen = time.Now()
}

// LastSeen returns the time the member was last seen.
func (m *Member) LastSeen() time.Time {
	m.lastSeenMu.RLock()
	defer m.lastSeenMu.RUnlock()
	return m.lastSeen
}

// MemberRegistry manages connected calculation members.
type MemberRegistry struct {
	mu       sync.RWMutex
	members  map[string]*Member
	onChange TagChangeFunc
	// tagsVersion increments under mu on every membership change. Publishes
	// carry the version of the snapshot they took; publishMu serialises them
	// and publishedVersion records the newest one that succeeded, so a slow
	// goroutine holding an older snapshot can never overwrite a newer one.
	tagsVersion      uint64
	publishMu        sync.Mutex
	publishedVersion uint64
	// pickMu makes "find the least recently picked and stamp it" one step.
	pickMu      sync.Mutex
	pickCounter uint64
	// changed is closed and replaced on every membership change. A callout
	// that found no cnode waits on it instead of polling.
	changed *common.ChangeSignal
}

// NewMemberRegistry creates a new, empty MemberRegistry.
func NewMemberRegistry() *MemberRegistry {
	return &MemberRegistry{
		members: make(map[string]*Member),
		changed: common.NewChangeSignal(),
	}
}

// Changed returns the channel that is closed on the next membership change —
// a cnode attaching or detaching on this pnode. Take it before looking at
// Candidates: a change between the look and the wait then still ends the
// wait.
func (r *MemberRegistry) Changed() <-chan struct{} {
	return r.changed.Changed()
}

// SetOnChange registers a callback that is invoked (in a goroutine) whenever
// the set of connected members changes. The callback receives the aggregate
// tags computed from all current members.
func (r *MemberRegistry) SetOnChange(fn TagChangeFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onChange = fn
}

// Register creates the member, publishes it to the registry, and only then
// starts its writer with greet as the first event on the wire. The greet is
// the member's "you are registered" signal, so the member must already be
// visible when it goes out; starting the writer first let a fast writer send
// the greet before the map insert, and a lookup at that instant found
// nothing. A dispatch routed between publication and the writer's start
// waits in the unbuffered outbox under its own deadline and is written after
// the greet, because writeLoop writes greet before it drains the outbox.
// greet may be nil (test fixtures).
//
// Re-registering an ID that is already present displaces the old member, whose
// writer would otherwise run forever and whose pending requests would wait out
// their timeouts with nobody left to answer them: it is evicted, which stops
// its writer and fails its waiters at once. The displaced member's own handler
// still runs its deferred Unregister afterwards; that is why Unregister takes
// the member and removes the entry only while it is still that member, rather
// than deleting whatever now holds the ID.
func (r *MemberRegistry) Register(memberID string, tenantID spi.TenantID, tags []string, send SendFunc, greet *cepb.CloudEvent) *Member {
	m := newMember(memberID, tenantID, tags, send)
	displaced := func() *Member {
		r.mu.Lock()
		defer r.mu.Unlock()
		old := r.members[memberID]
		r.members[memberID] = m
		r.tagsVersion++
		r.changed.Fire()
		return old
	}()
	go m.writeLoop(greet)
	if displaced != nil {
		displaced.Evict(status.Error(codes.Unavailable, "member id re-registered"))
	}
	r.notifyChange()
	return m
}

// Unregister removes m and evicts it, which fails all its pending requests
// and stops its writer.
//
// It takes the member, not its ID, and removes the map entry only while that
// entry is still m: a displaced member's handler runs its own deferred
// Unregister after the member that displaced it is already published under
// the same ID, and deleting by ID there would drop the live connection from
// the registry while its client believes it is registered. m is still
// evicted either way — a member the registry no longer holds must not be
// left with a running writer and stranded waiters.
func (r *MemberRegistry) Unregister(m *Member) {
	// A nil here is a programming error, but panicking would unwind inside
	// the stream handler where the interceptor latches the node, so this is
	// a no-op instead.
	if m == nil {
		return
	}
	found := func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		cur, ok := r.members[m.ID]
		if !ok || cur != m {
			return false
		}
		delete(r.members, m.ID)
		r.tagsVersion++
		r.changed.Fire()
		return true
	}()
	m.Evict(status.Error(codes.Unavailable, "member unregistered"))
	// notifyChange is gated on the entry actually having been removed, same
	// as the tagsVersion bump above: an unregister that removed nothing is a
	// no-op, not a membership change, so it should not spawn a publish
	// goroutine. Reviewed, not unit-tested — notifyChange's own version
	// dedup already makes the two behaviors unobservable via onChange (an
	// unregister that removed nothing never advances tagsVersion, so an
	// ungated publish goroutine finds nothing new to publish and returns
	// immediately), and there is no black-box way to assert "no goroutine
	// was spawned" without a production test hook.
	if found {
		r.notifyChange()
	}
}

// Get returns the member with the given ID, or nil if not found.
func (r *MemberRegistry) Get(memberID string) *Member {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.members[memberID]
}

// List returns all connected members.
func (r *MemberRegistry) List() []*Member {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*Member, 0, len(r.members))
	for _, m := range r.members {
		result = append(result, m)
	}
	return result
}

// Candidates returns every member of the tenant whose tags overlap tagsCSV —
// every member of the tenant when tagsCSV is empty — ordered by (ConnectedAt,
// ID), so that the order is the same on every call. A member of another tenant
// is never a candidate, whatever its tags.
//
// A member that has been evicted is not a candidate either. Eviction comes
// first and the registration is removed only when the member's stream handler
// returns, so between the two the member is still in the map while every
// request against it already fails: a try given to it would be a try spent on a
// cnode known to be gone, and an attempt reported that was never made. The
// remaining window — evicted between this look and the try registering its
// request — is inherent, and TrackRequest closes it with ErrMemberEvicted.
func (r *MemberRegistry) Candidates(tenantID spi.TenantID, tagsCSV string) []*Member {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Member
	for _, m := range r.members {
		if m.TenantID == tenantID && !m.gone() && common.TagsOverlap(m.Tags, tagsCSV) {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(x, y *Member) int {
		if c := x.ConnectedAt.Compare(y.ConnectedAt); c != 0 {
			return c
		}
		return cmp.Compare(x.ID, y.ID)
	})
	return out
}

// pickLeastRecent returns the candidate with the lowest pick stamp — the first
// such in the order given — and stamps it with the next counter value.
func (r *MemberRegistry) pickLeastRecent(candidates []*Member) *Member {
	r.pickMu.Lock()
	defer r.pickMu.Unlock()
	best := candidates[0]
	for _, m := range candidates[1:] {
		if m.pickStamp < best.pickStamp {
			best = m
		}
	}
	r.pickCounter++
	best.pickStamp = r.pickCounter
	return best
}

// notifyChange publishes the current aggregate tags in a goroutine. The
// snapshot and its version are taken under one read lock so they agree.
func (r *MemberRegistry) notifyChange() {
	fn, version, tags := func() (TagChangeFunc, uint64, map[string][]string) {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.onChange, r.tagsVersion, r.computeTagsLocked()
	}()
	if fn == nil {
		return
	}
	go func() {
		defer func() {
			if rv := recover(); rv != nil {
				slog.Error("onChange callback panicked",
					"pkg", "grpc/members",
					"panic", rv,
				)
			}
		}()
		r.publishMu.Lock()
		defer r.publishMu.Unlock()
		if version <= r.publishedVersion {
			return // a newer snapshot has already been published
		}
		if err := fn(tags); err != nil {
			slog.Error("failed to publish member tags",
				"pkg", "grpc/members",
				"version", version,
				"err", err,
			)
			return
		}
		r.publishedVersion = version
	}()
}

// computeTagsLocked builds an aggregate map of tenantID → deduplicated tags
// from all currently connected members. Caller holds r.mu (read or write).
//
// These are the tags this pnode tells the cluster it can serve, so they answer
// the same question Candidates does and skip a member for the same reason: an
// evicted member serves nothing, and advertising its tags would invite a peer to
// hand work over for a cnode that is already gone.
func (r *MemberRegistry) computeTagsLocked() map[string][]string {
	// Use a set per tenant for deduplication.
	sets := make(map[string]map[string]struct{})
	for _, m := range r.members {
		if m.gone() {
			continue // evicted: it serves nothing, and Candidates skips it too
		}
		tid := string(m.TenantID)
		if sets[tid] == nil {
			sets[tid] = make(map[string]struct{})
		}
		for _, t := range m.Tags {
			sets[tid][t] = struct{}{}
		}
	}

	result := make(map[string][]string, len(sets))
	for tid, tagSet := range sets {
		tags := make([]string, 0, len(tagSet))
		for t := range tagSet {
			tags = append(tags, t)
		}
		result[tid] = tags
	}
	return result
}
