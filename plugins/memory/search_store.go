package memory

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type searchJobEntry struct {
	job       spi.SearchJob
	entityIDs []string
}

// AsyncSearchStore is a tenant-scoped, in-memory implementation of spi.AsyncSearchStore.
type AsyncSearchStore struct {
	mu    sync.RWMutex
	clock Clock
	data  map[spi.TenantID]map[string]*searchJobEntry
}

// NewAsyncSearchStore creates a new in-memory AsyncSearchStore using the wall clock.
// Prefer newAsyncSearchStore for internal construction so the factory clock is shared.
func NewAsyncSearchStore() *AsyncSearchStore {
	return newAsyncSearchStore(wallClock{})
}

// newAsyncSearchStore creates a new in-memory AsyncSearchStore with the given clock.
func newAsyncSearchStore(clock Clock) *AsyncSearchStore {
	return &AsyncSearchStore{
		clock: clock,
		data:  make(map[spi.TenantID]map[string]*searchJobEntry),
	}
}

func (s *AsyncSearchStore) resolveTenant(ctx context.Context) (spi.TenantID, error) {
	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return "", fmt.Errorf("no user context in request — tenant cannot be resolved")
	}
	if uc.Tenant.ID == "" {
		return "", fmt.Errorf("user context has no tenant")
	}
	return uc.Tenant.ID, nil
}

func (s *AsyncSearchStore) CreateJob(ctx context.Context, job *spi.SearchJob) error {
	tid, err := s.resolveTenant(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tenantJobs := s.data[tid]
	if tenantJobs == nil {
		tenantJobs = make(map[string]*searchJobEntry)
		s.data[tid] = tenantJobs
	}

	// Defensive copy. Epoch is always persisted as 1, regardless of the
	// value set on job.Epoch by the caller.
	copied := *job
	copied.Epoch = 1
	tenantJobs[job.ID] = &searchJobEntry{job: copied}
	return nil
}

func (s *AsyncSearchStore) GetJob(ctx context.Context, jobID string) (*spi.SearchJob, error) {
	tid, err := s.resolveTenant(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	tenantJobs := s.data[tid]
	if tenantJobs == nil {
		return nil, fmt.Errorf("search job %q not found: %w", jobID, spi.ErrNotFound)
	}
	entry, ok := tenantJobs[jobID]
	if !ok {
		return nil, fmt.Errorf("search job %q not found: %w", jobID, spi.ErrNotFound)
	}

	// Return a defensive copy
	copied := entry.job
	return &copied, nil
}

// findEntryLocked looks up a job entry within tenantID's map. Caller must
// hold s.mu (read or write lock). Returns spi.ErrNotFound when the tenant
// has no jobs at all, or the specific job is absent.
func (s *AsyncSearchStore) findEntryLocked(tid spi.TenantID, jobID string) (*searchJobEntry, error) {
	tenantJobs := s.data[tid]
	if tenantJobs == nil {
		return nil, fmt.Errorf("search job %q not found: %w", jobID, spi.ErrNotFound)
	}
	entry, ok := tenantJobs[jobID]
	if !ok {
		return nil, fmt.Errorf("search job %q not found: %w", jobID, spi.ErrNotFound)
	}
	return entry, nil
}

// guardWrite applies the missing/terminal/epoch-fencing checks shared by
// UpdateJobStatus, Heartbeat, and SaveResults (checked at least at chunk
// boundaries). Caller must hold s.mu (write lock).
func (s *AsyncSearchStore) guardWrite(tid spi.TenantID, jobID string, epoch int64) (*searchJobEntry, error) {
	entry, err := s.findEntryLocked(tid, jobID)
	if err != nil {
		return nil, err
	}
	if terminalStatuses[entry.job.Status] {
		return nil, fmt.Errorf("search job %q is in a terminal status: %w", jobID, spi.ErrAlreadyTerminal)
	}
	if epoch != entry.job.Epoch {
		return nil, fmt.Errorf("search job %q: claim epoch %d does not match current epoch %d: %w", jobID, epoch, entry.job.Epoch, spi.ErrStaleClaim)
	}
	return entry, nil
}

func (s *AsyncSearchStore) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	tid, err := s.resolveTenant(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, err := s.guardWrite(tid, jobID, epoch)
	if err != nil {
		return err
	}

	entry.job.Status = status
	entry.job.ResultCount = resultCount
	entry.job.Error = errMsg
	if finishTime.IsZero() {
		entry.job.FinishTime = nil
	} else {
		ft := finishTime
		entry.job.FinishTime = &ft
	}
	entry.job.CalcTimeMs = calcTimeMs
	return nil
}

// saveResultsChunkSize is the batch size at which SaveResults re-runs the
// write guard (missing/terminal/epoch fencing) while draining entityIDs.
const saveResultsChunkSize = 1024

// SaveResults drains entityIDs, appending each chunk to the job's persisted
// result set under the lock. The write guard (existence, terminal status,
// epoch) is re-checked at every chunk boundary — including once up front,
// so a stale/terminal/missing job fails even against an empty sequence —
// and ctx cancellation is observed between chunks.
func (s *AsyncSearchStore) SaveResults(ctx context.Context, jobID string, epoch int64, entityIDs iter.Seq[string]) error {
	tid, err := s.resolveTenant(ctx)
	if err != nil {
		return err
	}

	guardAndAppend := func(chunk []string) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		entry, err := s.guardWrite(tid, jobID, epoch)
		if err != nil {
			return err
		}
		if len(chunk) > 0 {
			entry.entityIDs = append(entry.entityIDs, chunk...)
		}
		return nil
	}

	if err := guardAndAppend(nil); err != nil {
		return err
	}

	buf := make([]string, 0, saveResultsChunkSize)
	for id := range entityIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		buf = append(buf, id)
		if len(buf) >= saveResultsChunkSize {
			if err := guardAndAppend(buf); err != nil {
				return err
			}
			buf = buf[:0]
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return guardAndAppend(buf)
}

func (s *AsyncSearchStore) GetResultIDs(ctx context.Context, jobID string, offset, limit int) ([]string, int, error) {
	tid, err := s.resolveTenant(ctx)
	if err != nil {
		return nil, 0, err
	}
	if offset < 0 || limit < 1 {
		return nil, 0, fmt.Errorf("search job %q: offset must be >= 0 and limit must be >= 1, got offset=%d limit=%d", jobID, offset, limit)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, err := s.findEntryLocked(tid, jobID)
	if err != nil {
		return nil, 0, err
	}

	total := len(entry.entityIDs)
	if offset >= total {
		return []string{}, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}

	// Return a copy of the slice
	result := make([]string, end-offset)
	copy(result, entry.entityIDs[offset:end])
	return result, total, nil
}

func (s *AsyncSearchStore) DeleteJob(ctx context.Context, jobID string) error {
	tid, err := s.resolveTenant(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tenantJobs := s.data[tid]
	if tenantJobs == nil {
		return nil
	}
	delete(tenantJobs, jobID)
	return nil
}

// terminalStatuses is the set of statuses that represent a completed job.
var terminalStatuses = map[string]bool{
	"SUCCESSFUL": true,
	"FAILED":     true,
	"CANCELLED":  true,
}

// Cancel marks the job CANCELLED and stamps finishTime on the transition.
// Idempotent: cancelling a job already in a terminal state returns nil and
// does not overwrite the existing finish time. Cancelling a non-existent
// job returns spi.ErrNotFound.
func (s *AsyncSearchStore) Cancel(ctx context.Context, jobID string, finishTime time.Time) error {
	tid, err := s.resolveTenant(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, err := s.findEntryLocked(tid, jobID)
	if err != nil {
		return err
	}

	// Idempotent: already terminal — do nothing, and do not overwrite the
	// existing finish time.
	if terminalStatuses[entry.job.Status] {
		return nil
	}

	entry.job.Status = "CANCELLED"
	ft := finishTime
	entry.job.FinishTime = &ft
	return nil
}

// Heartbeat stamps HeartbeatTime (using the store's clock — the same clock
// domain ClaimStale compares staleness against) on the job, fenced by epoch.
// Guarded like UpdateJobStatus: missing → ErrNotFound, terminal →
// ErrAlreadyTerminal, epoch mismatch → ErrStaleClaim.
func (s *AsyncSearchStore) Heartbeat(ctx context.Context, jobID string, epoch int64) error {
	tid, err := s.resolveTenant(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, err := s.guardWrite(tid, jobID, epoch)
	if err != nil {
		return err
	}

	now := s.clock.Now()
	entry.job.HeartbeatTime = &now
	return nil
}

// ClaimStale atomically claims up to limit RUNNING jobs, across ALL
// tenants, whose staleness baseline — HeartbeatTime, falling back to
// CreateTime when never heartbeated — is older than staleAfter (measured
// against the store's clock, the same domain Heartbeat stamps into). A
// claimed job has its Epoch bumped and HeartbeatTime refreshed to now, so a
// concurrent or immediately-following claim cannot re-take it. Terminal
// jobs are never claimed, regardless of staleness.
func (s *AsyncSearchStore) ClaimStale(ctx context.Context, staleAfter time.Duration, limit int) ([]*spi.SearchJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	cutoff := now.Add(-staleAfter)

	var claimed []*spi.SearchJob
	for _, tenantJobs := range s.data {
		for _, entry := range tenantJobs {
			if len(claimed) >= limit {
				return claimed, nil
			}
			// RUNNING is the only non-terminal status; this also excludes
			// terminal jobs from ever being claimed.
			if entry.job.Status != "RUNNING" {
				continue
			}
			baseline := entry.job.CreateTime
			if entry.job.HeartbeatTime != nil {
				baseline = *entry.job.HeartbeatTime
			}
			if !baseline.Before(cutoff) {
				continue
			}

			entry.job.Epoch++
			hb := now
			entry.job.HeartbeatTime = &hb

			copied := entry.job
			claimed = append(claimed, &copied)
		}
	}
	return claimed, nil
}

// ClearResults deletes the job's persisted result IDs. Idempotent: a
// missing job is a no-op, matching the interface contract.
func (s *AsyncSearchStore) ClearResults(ctx context.Context, jobID string) error {
	tid, err := s.resolveTenant(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, err := s.findEntryLocked(tid, jobID)
	if err != nil {
		// Unknown job: idempotent no-op, not an error.
		return nil
	}
	entry.entityIDs = nil
	return nil
}

func (s *AsyncSearchStore) ReapExpired(ctx context.Context, ttl time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := s.clock.Now().Add(-ttl)
	reaped := 0

	for _, tenantJobs := range s.data {
		for id, entry := range tenantJobs {
			// Never reap running jobs
			if entry.job.Status == "RUNNING" {
				continue
			}
			// Reap if finish time is before cutoff
			if entry.job.FinishTime != nil && entry.job.FinishTime.Before(cutoff) {
				delete(tenantJobs, id)
				reaped++
			}
		}
	}

	return reaped, nil
}
