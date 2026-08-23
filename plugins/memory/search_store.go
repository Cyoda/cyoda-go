package memory

import (
	"context"
	"fmt"
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

	// Defensive copy
	copied := *job
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

func (s *AsyncSearchStore) UpdateJobStatus(ctx context.Context, jobID string, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	tid, err := s.resolveTenant(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tenantJobs := s.data[tid]
	if tenantJobs == nil {
		return fmt.Errorf("search job %q not found", jobID)
	}
	entry, ok := tenantJobs[jobID]
	if !ok {
		return fmt.Errorf("search job %q not found", jobID)
	}

	entry.job.Status = status
	entry.job.ResultCount = resultCount
	entry.job.Error = errMsg
	ft := finishTime
	entry.job.FinishTime = &ft
	entry.job.CalcTimeMs = calcTimeMs
	return nil
}

func (s *AsyncSearchStore) SaveResults(ctx context.Context, jobID string, entityIDs []string) error {
	tid, err := s.resolveTenant(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tenantJobs := s.data[tid]
	if tenantJobs == nil {
		return fmt.Errorf("search job %q not found", jobID)
	}
	entry, ok := tenantJobs[jobID]
	if !ok {
		return fmt.Errorf("search job %q not found", jobID)
	}

	// Defensive copy of entity IDs
	copied := make([]string, len(entityIDs))
	copy(copied, entityIDs)
	entry.entityIDs = copied
	return nil
}

func (s *AsyncSearchStore) GetResultIDs(ctx context.Context, jobID string, offset, limit int) ([]string, int, error) {
	tid, err := s.resolveTenant(ctx)
	if err != nil {
		return nil, 0, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	tenantJobs := s.data[tid]
	if tenantJobs == nil {
		return nil, 0, fmt.Errorf("search job %q not found", jobID)
	}
	entry, ok := tenantJobs[jobID]
	if !ok {
		return nil, 0, fmt.Errorf("search job %q not found", jobID)
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

	tenantJobs := s.data[tid]
	if tenantJobs == nil {
		return fmt.Errorf("search job %q not found: %w", jobID, spi.ErrNotFound)
	}
	entry, ok := tenantJobs[jobID]
	if !ok {
		return fmt.Errorf("search job %q not found: %w", jobID, spi.ErrNotFound)
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
