// Package consistency fences point-in-time reads with the store's
// consistency time: a read at an instant later than the consistency time is
// refused, and a read whose instant cyoda-go chooses uses a fresh one.
package consistency

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// storeCallDeadline bounds one store call: the store's own wait budget (10 s)
// plus a margin, so a shared call cannot outlive its callers' patience by much.
const storeCallDeadline = 11 * time.Second

// maxInFlight is the number of store calls one tenant may have open at once
// on this node, so a commit held in its commit phase cannot drain the
// connection pool.
const maxInFlight = 2

type call struct {
	seq  uint64
	ctx  context.Context // the starter's context without its cancellation; carries the tenant
	done chan struct{}
	c    time.Time // written before done is closed; read only after
	err  error     // likewise
}

type tenantState struct {
	hi       time.Time // highest C seen; a returned C stays final forever
	inflight []*call   // oldest first, at most maxInFlight
}

// Service fences reads with the consistency time. It is safe for concurrent
// use; one instance serves the whole process.
type Service struct {
	txMgr   spi.TransactionManager
	mu      sync.Mutex
	seq     uint64 // last call sequence number handed out
	tenants map[spi.TenantID]*tenantState
}

// New returns a Service over txMgr. A nil txMgr panics: there is no fallback
// clock.
func New(txMgr spi.TransactionManager) *Service {
	if txMgr == nil {
		panic("consistency.New: nil TransactionManager")
	}
	return &Service{txMgr: txMgr, tenants: make(map[spi.TenantID]*tenantState)}
}

func tenantOf(ctx context.Context) (spi.TenantID, error) {
	uc := spi.GetUserContext(ctx)
	if uc == nil || uc.Tenant.ID == "" {
		return "", errors.New("no tenant in context")
	}
	return uc.Tenant.ID, nil
}

// Fresh returns a consistency time from a store call that started after
// Fresh was called (completeness needs that).
func (s *Service) Fresh(ctx context.Context) (time.Time, error) {
	tenant, err := tenantOf(ctx)
	if err != nil {
		return time.Time{}, common.Internal("failed to obtain the consistency time", err)
	}
	entrySeq := func() uint64 {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.seq
	}()
	for {
		cl, wait := s.freshCall(ctx, tenant, entrySeq)
		if wait != nil { // two calls in flight, both older than us
			if err := waitOn(ctx, wait); err != nil {
				return time.Time{}, err
			}
			continue
		}
		if err := waitOn(ctx, cl); err != nil {
			return time.Time{}, err
		}
		if cl.err != nil {
			return time.Time{}, classify(cl.err)
		}
		return cl.c, nil
	}
}

// Fence returns nil when t is at or before a consistency time; otherwise the
// refusal, or the error that prevented getting one.
func (s *Service) Fence(ctx context.Context, t time.Time) error {
	tenant, err := tenantOf(ctx)
	if err != nil {
		return common.Internal("failed to obtain the consistency time", err)
	}
	covered, joined := func() (bool, *call) {
		s.mu.Lock()
		defer s.mu.Unlock()
		st := s.state(tenant)
		if !t.After(st.hi) {
			return true, nil
		}
		if n := len(st.inflight); n > 0 {
			return false, st.inflight[n-1]
		}
		return false, nil
	}()
	if covered {
		return nil
	}
	if joined != nil {
		if err := waitOn(ctx, joined); err != nil {
			return err
		}
		if joined.err != nil {
			return classify(joined.err)
		}
		if !t.After(joined.c) {
			return nil
		}
		// t is later than the joined call's C, which may be old; only a call
		// that started after this one can justify a refusal.
	}
	c, err := s.Fresh(ctx)
	if err != nil {
		return err
	}
	if t.After(c) {
		return refusal(t, c)
	}
	return nil
}

// state returns the tenant's state; s.mu must be held.
func (s *Service) state(tenant spi.TenantID) *tenantState {
	st, ok := s.tenants[tenant]
	if !ok {
		st = &tenantState{}
		s.tenants[tenant] = st
	}
	return st
}

// freshCall returns a call that started after entrySeq to join, starting one
// if allowed; or, when two older calls are in flight, the newest to wait for.
func (s *Service) freshCall(ctx context.Context, tenant spi.TenantID, entrySeq uint64) (cl, wait *call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(tenant)
	for i := len(st.inflight) - 1; i >= 0; i-- {
		if st.inflight[i].seq > entrySeq {
			return st.inflight[i], nil
		}
	}
	if len(st.inflight) >= maxInFlight {
		return nil, st.inflight[len(st.inflight)-1]
	}
	s.seq++
	// One caller's cancel must not fail the others who share this call.
	cl = &call{seq: s.seq, ctx: context.WithoutCancel(ctx), done: make(chan struct{})}
	st.inflight = append(st.inflight, cl)
	go s.run(tenant, cl)
	return cl, nil
}

func (s *Service) run(tenant spi.TenantID, cl *call) {
	ctx, cancel := context.WithTimeout(cl.ctx, storeCallDeadline)
	defer cancel()
	cl.c, cl.err = s.txMgr.ConsistencyTime(ctx)
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		st := s.state(tenant)
		if cl.err == nil && cl.c.After(st.hi) {
			st.hi = cl.c
		}
		for i, x := range st.inflight {
			if x == cl {
				st.inflight = append(st.inflight[:i], st.inflight[i+1:]...)
				break
			}
		}
	}()
	close(cl.done)
}

func waitOn(ctx context.Context, cl *call) error {
	select {
	case <-cl.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func refusal(t, c time.Time) *common.AppError {
	cs := c.UTC().Format(time.RFC3339Nano)
	e := common.Operational(http.StatusBadRequest, common.ErrCodePointInTimeAfterConsistencyTime,
		fmt.Sprintf("pointInTime %s is later than the consistency time %s; read at or before the consistency time (GET /entity/consistency-time returns the current one)",
			t.UTC().Format(time.RFC3339Nano), cs))
	e.Props = map[string]any{"consistencyTime": cs}
	return e
}

func classify(err error) error {
	if errors.Is(err, spi.ErrConsistencyTimeUnavailable) {
		return common.Operational(http.StatusServiceUnavailable, common.ErrCodeConsistencyTimeUnavailable,
			"the consistency time is not available yet: a save is still committing — retry").AsRetryable().WithCause(err)
	}
	if appErr := common.StorageUnavailable(err); appErr != nil {
		return appErr
	}
	return common.Internal("failed to obtain the consistency time", err)
}
