package grpc

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// TagChangeFunc is called when the set of connected members changes,
// with the computed aggregate tags (tenantID → deduplicated tag list).
type TagChangeFunc func(tags map[string][]string)

// SendFunc is a function that sends a CloudEvent to a connected member's stream.
type SendFunc func(ce *cepb.CloudEvent) error

// ProcessingResponse holds the response from a processor or criteria calculation.
type ProcessingResponse struct {
	Payload json.RawMessage
	Success bool
	Error   string
	Matches *bool // for criteria responses (nil for processor responses)
	// Reason is the criteria-response explanation for a matches=false result
	// (EntityCriteriaCalculationResponse.reason). Empty for processor
	// responses and for criteria that supply no reason.
	Reason   string
	Warnings []string // warnings from processor/criteria, propagated to client
	// Retryable carries the member-supplied retryable flag from the inbound
	// CloudEvent error shape (api/grpc/events/types.go: every *EventJsonError
	// variant declares Retryable *bool). The pointer is nil when the wire
	// omitted the key or when no error was present, distinguishing "wire
	// said so" from "wire didn't say". Captured here for the future retry
	// loop; the current dispatcher is single-shot and does not consult
	// this field.
	Retryable *bool
	// Disconnected is true when this response was synthesized by
	// FailAllPending because the member's stream dropped while the request
	// was in flight, rather than a substantive failure returned by the
	// member. dispatchCalloutToMember uses this to surface a distinguishable
	// 503 COMPUTE_MEMBER_DISCONNECTED instead of a generic failure.
	Disconnected bool
}

// Member represents a connected calculation member.
type Member struct {
	ID          string
	TenantID    spi.TenantID
	Tags        []string
	ConnectedAt time.Time

	send        SendFunc
	sendMu      sync.Mutex
	lastSeen    time.Time
	lastSeenMu  sync.RWMutex
	pendingReqs map[string]chan *ProcessingResponse
	pendingMu   sync.Mutex
}

// Send sends a CloudEvent to the member's stream, serializing access.
func (m *Member) Send(ce *cepb.CloudEvent) error {
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	return m.send(ce)
}

// TrackRequest creates a buffered channel for the given requestID and stores it
// in the pending requests map. Returns the channel on which the caller should
// wait for the response.
func (m *Member) TrackRequest(requestID string) chan *ProcessingResponse {
	ch := make(chan *ProcessingResponse, 1)
	m.pendingMu.Lock()
	m.pendingReqs[requestID] = ch
	m.pendingMu.Unlock()
	return ch
}

// CompleteRequest delivers resp to the channel associated with requestID and
// removes the entry from the pending map. If the requestID is not found, this
// is a no-op.
func (m *Member) CompleteRequest(requestID string, resp *ProcessingResponse) {
	m.pendingMu.Lock()
	ch, ok := m.pendingReqs[requestID]
	if ok {
		delete(m.pendingReqs, requestID)
	}
	m.pendingMu.Unlock()
	if ok {
		ch <- resp
	}
}

// FailAllPending sends an error response to every pending request channel and
// clears the pending map.
func (m *Member) FailAllPending(errMsg string) {
	m.pendingMu.Lock()
	reqs := m.pendingReqs
	m.pendingReqs = make(map[string]chan *ProcessingResponse)
	m.pendingMu.Unlock()

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
	m.lastSeen = time.Now()
	m.lastSeenMu.Unlock()
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
}

// NewMemberRegistry creates a new, empty MemberRegistry.
func NewMemberRegistry() *MemberRegistry {
	return &MemberRegistry{
		members: make(map[string]*Member),
	}
}

// SetOnChange registers a callback that is invoked (in a goroutine) whenever
// the set of connected members changes. The callback receives the aggregate
// tags computed from all current members.
func (r *MemberRegistry) SetOnChange(fn TagChangeFunc) {
	r.mu.Lock()
	r.onChange = fn
	r.mu.Unlock()
}

// Register creates a new Member with a generated UUID, stores it, and returns
// the member ID.
func (r *MemberRegistry) Register(tenantID spi.TenantID, tags []string, send SendFunc) string {
	id := uuid.NewString()
	now := time.Now()
	m := &Member{
		ID:          id,
		TenantID:    tenantID,
		Tags:        tags,
		ConnectedAt: now,
		send:        send,
		lastSeen:    now,
		pendingReqs: make(map[string]chan *ProcessingResponse),
	}
	r.mu.Lock()
	r.members[id] = m
	r.mu.Unlock()
	r.notifyChange()
	return id
}

// Unregister removes the member with the given ID and fails all its pending
// requests.
func (r *MemberRegistry) Unregister(memberID string) {
	r.mu.Lock()
	m, ok := r.members[memberID]
	if ok {
		delete(r.members, memberID)
	}
	r.mu.Unlock()
	if ok {
		m.FailAllPending("member disconnected")
	}
	r.notifyChange()
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

// FindByTags returns the first member matching the given tenant whose tags
// overlap with tagsCSV. If tagsCSV is empty, any member for that tenant
// matches.
func (r *MemberRegistry) FindByTags(tenantID spi.TenantID, tagsCSV string) *Member {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.members {
		if m.TenantID == tenantID && common.TagsOverlap(m.Tags, tagsCSV) {
			return m
		}
	}
	return nil
}

// notifyChange fires the onChange callback (if set) in a goroutine with the
// current aggregate tags.
func (r *MemberRegistry) notifyChange() {
	r.mu.RLock()
	fn := r.onChange
	r.mu.RUnlock()
	if fn == nil {
		return
	}
	tags := r.computeTags()
	go func() {
		defer func() {
			if rv := recover(); rv != nil {
				slog.Error("onChange callback panicked",
					"pkg", "grpc/members",
					"panic", rv,
				)
			}
		}()
		fn(tags)
	}()
}

// computeTags builds an aggregate map of tenantID → deduplicated tags from all
// currently connected members.
func (r *MemberRegistry) computeTags() map[string][]string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Use a set per tenant for deduplication.
	sets := make(map[string]map[string]struct{})
	for _, m := range r.members {
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
