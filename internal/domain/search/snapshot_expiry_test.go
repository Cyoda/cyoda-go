package search_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// clockAt is a business clock that reads a settable instant.
type clockAt struct{ now time.Time }

func (c *clockAt) Now() time.Time { return c.now }

var expiryCreate = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

func expiryService(store spi.AsyncSearchStore, clk spi.Clock, ttl time.Duration) *search.SearchService {
	return search.NewSearchService(nil, nil, store, newFixedConsistency(), clk).WithSnapshotTTL(ttl)
}

func finishedJob(id string, finish time.Time) *spi.SearchJob {
	return &spi.SearchJob{ID: id, Status: "SUCCESSFUL", CreateTime: expiryCreate, FinishTime: &finish}
}

// A finished job's expirationDate is its finish time plus the snapshot TTL:
// the instant the reaper may delete it, not a fixed offset from its create.
func TestAsyncStatus_ExpirationDate_IsFinishPlusTTL(t *testing.T) {
	ctx := tenantCtx("tenant-1")
	jobID := uuid.New().String()
	finish := expiryCreate.Add(3 * time.Second)
	clk := &clockAt{now: finish.Add(time.Minute)}
	svc := expiryService(&stubSearchStore{job: finishedJob(jobID, finish)}, clk, 2*time.Hour)

	st, err := svc.GetAsyncStatus(ctx, jobID)
	if err != nil {
		t.Fatalf("GetAsyncStatus: %v", err)
	}
	if want := finish.Add(2 * time.Hour); !st.ExpirationDate.Equal(want) {
		t.Fatalf("ExpirationDate = %s; want finish + TTL = %s", st.ExpirationDate, want)
	}

	snap, err := svc.GetAsyncSearchStatus(ctx, jobID)
	if err != nil {
		t.Fatalf("GetAsyncSearchStatus: %v", err)
	}
	if want := finish.Add(2 * time.Hour); !snap.ExpirationDate.Equal(want) {
		t.Fatalf("SnapshotStatus.ExpirationDate = %s; want %s", snap.ExpirationDate, want)
	}
}

// A running job has no finish time yet. It cannot expire before it finishes,
// so now + TTL is a lower bound on its expiration.
func TestAsyncStatus_ExpirationDate_RunningIsNowPlusTTL(t *testing.T) {
	ctx := tenantCtx("tenant-1")
	jobID := uuid.New().String()
	clk := &clockAt{now: expiryCreate.Add(5 * time.Minute)}
	job := &spi.SearchJob{ID: jobID, Status: "RUNNING", CreateTime: expiryCreate}
	svc := expiryService(&stubSearchStore{job: job}, clk, time.Hour)

	st, err := svc.GetAsyncStatus(ctx, jobID)
	if err != nil {
		t.Fatalf("GetAsyncStatus: %v", err)
	}
	if want := clk.now.Add(time.Hour); !st.ExpirationDate.Equal(want) {
		t.Fatalf("ExpirationDate = %s; want now + TTL = %s", st.ExpirationDate, want)
	}
}

// From its expirationDate on, a job is gone on every read, whether or not
// the reaper has run yet: status, results and cancel answer not found.
func TestAsyncJob_PastExpiration_IsNotFound(t *testing.T) {
	ctx := tenantCtx("tenant-1")
	jobID := uuid.New().String()
	finish := expiryCreate.Add(time.Second)
	ttl := time.Hour

	for _, tc := range []struct {
		name    string
		now     time.Time
		expired bool
	}{
		{"just before", finish.Add(ttl - time.Nanosecond), false},
		{"at expiration", finish.Add(ttl), true},
		{"after", finish.Add(ttl + time.Minute), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// FAILED, so a live job's results read stops at "not complete"
			// before it needs an entity store.
			job := finishedJob(jobID, finish)
			job.Status = "FAILED"
			svc := expiryService(&stubSearchStore{job: job}, &clockAt{now: tc.now}, ttl)
			for name, call := range serviceCalls(svc, ctx, jobID) {
				err := call()
				if got := errors.Is(err, search.ErrSearchJobNotFound); got != tc.expired {
					t.Errorf("%s: err = %v; want not-found = %v", name, err, tc.expired)
				}
			}
		})
	}
}

// The reap uses the same TTL as the expirationDate the service reports.
func TestReapExpiredSnapshots_UsesTheServiceTTL(t *testing.T) {
	store := &reapRecordingStore{}
	svc := expiryService(store, &clockAt{now: expiryCreate}, 7*time.Minute)
	if _, err := svc.ReapExpiredSnapshots(tenantCtx("tenant-1")); err != nil {
		t.Fatalf("ReapExpiredSnapshots: %v", err)
	}
	if store.ttl != 7*time.Minute {
		t.Fatalf("ReapExpired got ttl %s; want the service's 7m", store.ttl)
	}
}

// Without a TTL (or with a non-positive one) the service uses the default,
// the same as CYODA_SEARCH_SNAPSHOT_TTL's.
func TestWithSnapshotTTL_NonPositiveIsTheDefault(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		store := &reapRecordingStore{}
		svc := expiryService(store, &clockAt{now: expiryCreate}, ttl)
		if _, err := svc.ReapExpiredSnapshots(tenantCtx("tenant-1")); err != nil {
			t.Fatalf("ReapExpiredSnapshots: %v", err)
		}
		if store.ttl != time.Hour {
			t.Errorf("WithSnapshotTTL(%s): ReapExpired got ttl %s; want the 1h default", ttl, store.ttl)
		}
	}
}

// reapRecordingStore records the TTL ReapExpired is called with.
type reapRecordingStore struct {
	spi.AsyncSearchStore
	ttl time.Duration
}

func (s *reapRecordingStore) ReapExpired(_ context.Context, ttl time.Duration) (int, error) {
	s.ttl = ttl
	return 0, nil
}

// The HTTP status answer carries the service's expirationDate, finish + TTL.
func TestAsyncStatusHandler_ExpirationDate(t *testing.T) {
	jobID := uuid.New()
	finish := expiryCreate.Add(3 * time.Second)
	svc := expiryService(&stubSearchStore{job: finishedJob(jobID.String(), finish)}, &clockAt{now: finish}, 2*time.Hour)

	w := callStatus(t, svc, jobID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d %s", w.Code, w.Body)
	}
	var body struct {
		ExpirationDate time.Time `json:"expirationDate"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	if want := finish.Add(2 * time.Hour); !body.ExpirationDate.Equal(want) {
		t.Fatalf("expirationDate = %s; want %s", body.ExpirationDate, want)
	}
}

// Past its expiration, the HTTP status and results answer 404 SEARCH_JOB_NOT_FOUND.
func TestAsyncHandlers_PastExpiration_404(t *testing.T) {
	jobID := uuid.New()
	finish := expiryCreate.Add(3 * time.Second)
	svc := expiryService(&stubSearchStore{job: finishedJob(jobID.String(), finish)}, &clockAt{now: finish.Add(time.Hour)}, time.Hour)

	for name, w := range map[string]*httptest.ResponseRecorder{
		"status":  callStatus(t, svc, jobID),
		"results": callResults(t, svc, jobID),
		"cancel":  callCancel(t, svc, jobID),
	} {
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "SEARCH_JOB_NOT_FOUND") {
			t.Errorf("%s: %d %s; want 404 SEARCH_JOB_NOT_FOUND", name, w.Code, w.Body)
		}
	}
}
