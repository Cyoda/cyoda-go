package search_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// outageDSN stands in for the connection detail a real pool error carries.
// Synthetic — it exists only so the Gate-3 leak assertions have something
// recognisable to look for in a response body.
const outageDSN = "postgres://u:p@db/cyoda"

// outageErr carries the storage layer's transient-unavailability marker, the
// same shape a plugin returns when a connection cannot be acquired.
type outageErr struct{}

func (outageErr) Error() string            { return "acquire timed out: " + outageDSN }
func (outageErr) StorageUnavailable() bool { return true }

// storageOutage wraps outageErr the way a plugin's own wrapping would, so the
// marker is only reachable through the chain rather than at the top level.
func storageOutage(op string) error { return fmt.Errorf("%s: %w", op, outageErr{}) }

// jobMiss is what every backend returns for a job that genuinely is not there:
// memory, sqlite, postgres and the commercial backend all put spi.ErrNotFound
// in the chain.
func jobMiss(jobID string) error {
	return fmt.Errorf("search job %q not found: %w", jobID, spi.ErrNotFound)
}

// stubSearchStore serves a canned job, or a canned failure, for the async-job
// lookup paths. Embedding the interface leaves every other method unimplemented
// — none of them are on these code paths, and a call would panic loudly rather
// than pass silently.
type stubSearchStore struct {
	spi.AsyncSearchStore
	job          *spi.SearchJob
	getJobErr    error
	resultIDsErr error

	// vanishAfterFirstGet models the job disappearing between the two store
	// calls GetAsyncResults makes — an expiry reap or a concurrent cancel
	// landing in the window.
	vanishAfterFirstGet bool
	getJobCalls         int
}

func (s *stubSearchStore) GetJob(_ context.Context, jobID string) (*spi.SearchJob, error) {
	s.getJobCalls++
	if s.vanishAfterFirstGet && s.getJobCalls > 1 {
		return nil, jobMiss(jobID)
	}
	if s.job != nil {
		return s.job, nil
	}
	return nil, s.getJobErr
}

func (s *stubSearchStore) GetResultIDs(_ context.Context, _ string, _, _ int) ([]string, int, error) {
	return nil, 0, s.resultIDsErr
}

func stubService(store spi.AsyncSearchStore) *search.SearchService {
	// factory and uuid generator are untouched on every path exercised here.
	return search.NewSearchService(nil, nil, store)
}

// ---------------------------------------------------------------------------
// Service layer — the cause has to survive the lookup
// ---------------------------------------------------------------------------

func serviceCalls(svc *search.SearchService, ctx context.Context, jobID string) map[string]func() error {
	return map[string]func() error{
		"GetAsyncStatus":  func() error { _, err := svc.GetAsyncStatus(ctx, jobID); return err },
		"GetAsyncResults": func() error { _, err := svc.GetAsyncResults(ctx, jobID, search.ResultOptions{}); return err },
		"CancelAsync":     func() error { _, err := svc.CancelAsync(ctx, jobID); return err },
	}
}

// A storage outage is not a missing job. Reporting one as the other tells the
// client its job is gone, so it stops retrying — the opposite of the truth.
func TestAsyncJobLookup_StorageOutage_KeepsTheCause(t *testing.T) {
	ctx := tenantCtx("tenant-1")
	jobID := uuid.New().String()
	svc := stubService(&stubSearchStore{getJobErr: storageOutage("GetJob")})

	for name, call := range serviceCalls(svc, ctx, jobID) {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("expected an error when the store cannot be reached")
			}
			if errors.Is(err, search.ErrSearchJobNotFound) {
				t.Errorf("storage outage reported as %v: %v", search.ErrSearchJobNotFound, err)
			}
			if common.StorageUnavailable(err) == nil {
				t.Errorf("storage-unavailability marker did not survive the lookup: %v", err)
			}
		})
	}
}

// The other direction: a job that genuinely is not there must still be
// ErrSearchJobNotFound, or every 404 on these endpoints turns into a 500.
func TestAsyncJobLookup_UnknownJob_StaysNotFound(t *testing.T) {
	ctx := tenantCtx("tenant-1")
	jobID := uuid.New().String()
	svc := stubService(&stubSearchStore{getJobErr: jobMiss(jobID)})

	for name, call := range serviceCalls(svc, ctx, jobID) {
		t.Run(name, func(t *testing.T) {
			err := call()
			if !errors.Is(err, search.ErrSearchJobNotFound) {
				t.Errorf("missing job reported as %v, want ErrSearchJobNotFound", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Handler layer — status class, error code, retryability, and no leakage
// ---------------------------------------------------------------------------

func expectRetryable503(t *testing.T, resp *http.Response, body string) {
	t.Helper()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", resp.StatusCode, body)
	}
	commontest.ExpectErrorCode(t, resp, common.ErrCodeStorageUnavailable)
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("decode problem detail: %v; body: %s", err, body)
	}
	if r, _ := pd.Properties["retryable"].(bool); !r {
		t.Errorf("503 is not advertised as retryable; body: %s", body)
	}
	// Gate 3: a pool error carries host, user and database. None of it belongs
	// in a client-facing body.
	if strings.Contains(body, outageDSN) {
		t.Errorf("response leaked storage internals: %s", body)
	}
}

func callStatus(t *testing.T, svc *search.SearchService, jobID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/search/async/"+jobID.String()+"/status", nil)
	search.NewHandler(svc).GetAsyncSearchStatus(w, r, jobID)
	return w
}

func callResults(t *testing.T, svc *search.SearchService, jobID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/search/async/"+jobID.String(), nil)
	search.NewHandler(svc).GetAsyncSearchResults(w, r, jobID, genapi.GetAsyncSearchResultsParams{})
	return w
}

func callCancel(t *testing.T, svc *search.SearchService, jobID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/search/async/"+jobID.String()+"/cancel", nil)
	search.NewHandler(svc).CancelAsyncSearch(w, r, jobID)
	return w
}

func TestGetAsyncSearchStatus_StorageOutage_Returns503(t *testing.T) {
	jobID := uuid.New()
	w := callStatus(t, stubService(&stubSearchStore{getJobErr: storageOutage("GetJob")}), jobID)
	expectRetryable503(t, w.Result(), w.Body.String())
}

func TestGetAsyncSearchStatus_UnknownJob_Still404(t *testing.T) {
	jobID := uuid.New()
	w := callStatus(t, stubService(&stubSearchStore{getJobErr: jobMiss(jobID.String())}), jobID)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
	commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeSearchJobNotFound)
}

func TestGetAsyncSearchResults_StorageOutage_Returns503(t *testing.T) {
	jobID := uuid.New()
	w := callResults(t, stubService(&stubSearchStore{getJobErr: storageOutage("GetJob")}), jobID)
	expectRetryable503(t, w.Result(), w.Body.String())
}

// The results page reads the job, then the result IDs. The second read is a
// separate storage round-trip and had its own swallow: the handler interpolated
// whatever came back into a 400 body.
func TestGetAsyncSearchResults_ResultIDsStorageOutage_Returns503(t *testing.T) {
	jobID := uuid.New()
	store := &stubSearchStore{
		job:          &spi.SearchJob{ID: jobID.String(), Status: "SUCCESSFUL"},
		resultIDsErr: storageOutage("GetResultIDs"),
	}
	w := callResults(t, stubService(store), jobID)
	expectRetryable503(t, w.Result(), w.Body.String())
}

// A job can be reaped between the status read and the result-ID read. No
// backend tags that miss, so the only honest way to tell it from a store
// failure is to ask the one call that does tag — and only an affirmative miss
// may answer 404.
func TestGetAsyncSearchResults_JobVanishesMidRead_Returns404(t *testing.T) {
	jobID := uuid.New()
	store := &stubSearchStore{
		job:                 &spi.SearchJob{ID: jobID.String(), Status: "SUCCESSFUL"},
		resultIDsErr:        fmt.Errorf("search job %q not found", jobID),
		vanishAfterFirstGet: true,
	}
	w := callResults(t, stubService(store), jobID)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
	commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeSearchJobNotFound)
}

// The inverse, and the reason the re-read must not be a guess: when the store
// is failing rather than the job being gone, the re-read cannot confirm a miss,
// so the answer stays a retryable 503 — never a 404 inferred from a failure.
func TestGetAsyncSearchResults_StoreDownDuringResultIDs_StaysRetryable(t *testing.T) {
	jobID := uuid.New()
	store := &stubSearchStore{
		job:          &spi.SearchJob{ID: jobID.String(), Status: "SUCCESSFUL"},
		resultIDsErr: storageOutage("GetResultIDs"),
	}
	w := callResults(t, stubService(store), jobID)
	expectRetryable503(t, w.Result(), w.Body.String())
}

func TestGetAsyncSearchResults_UnknownJob_Still404(t *testing.T) {
	jobID := uuid.New()
	w := callResults(t, stubService(&stubSearchStore{getJobErr: jobMiss(jobID.String())}), jobID)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
	commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeSearchJobNotFound)
}

// A job that exists but has not finished is a client error and stays one — the
// caller asked for results too early, and the status it is in is domain detail
// the caller is entitled to.
func TestGetAsyncSearchResults_JobStillRunning_Still400(t *testing.T) {
	jobID := uuid.New()
	store := &stubSearchStore{job: &spi.SearchJob{ID: jobID.String(), Status: "RUNNING"}}
	w := callResults(t, stubService(store), jobID)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "RUNNING") {
		t.Errorf("400 body dropped the job status: %s", w.Body.String())
	}
}

func TestCancelAsyncSearch_StorageOutage_Returns503(t *testing.T) {
	jobID := uuid.New()
	w := callCancel(t, stubService(&stubSearchStore{getJobErr: storageOutage("GetJob")}), jobID)
	expectRetryable503(t, w.Result(), w.Body.String())
}

func TestCancelAsyncSearch_UnknownJob_Still404(t *testing.T) {
	jobID := uuid.New()
	w := callCancel(t, stubService(&stubSearchStore{getJobErr: jobMiss(jobID.String())}), jobID)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
	commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeSearchJobNotFound)
}

// ---------------------------------------------------------------------------
// Result-page hydration — a page that is short because the store failed is a
// substituted answer, not a result
// ---------------------------------------------------------------------------

// resultIDsStore serves a SUCCESSFUL job plus a fixed result-ID list, so the
// hydration loop is reachable.
type resultIDsStore struct {
	spi.AsyncSearchStore
	ids []string
}

func (s *resultIDsStore) GetJob(_ context.Context, jobID string) (*spi.SearchJob, error) {
	return &spi.SearchJob{ID: jobID, Status: "SUCCESSFUL", ResultCount: len(s.ids)}, nil
}

func (s *resultIDsStore) GetResultIDs(_ context.Context, _ string, _, _ int) ([]string, int, error) {
	return s.ids, len(s.ids), nil
}

// hydrateEntityStore answers GetAsAt per id: an id in errs fails with the mapped
// error, anything else resolves.
type hydrateEntityStore struct {
	spi.EntityStore
	errs map[string]error
}

func (s *hydrateEntityStore) GetAsAt(_ context.Context, id string, _ time.Time) (*spi.Entity, error) {
	if err, ok := s.errs[id]; ok {
		return nil, err
	}
	return &spi.Entity{Meta: spi.EntityMeta{ID: id}}, nil
}

type hydrateFactory struct {
	spi.StoreFactory
	store spi.EntityStore
}

func (f *hydrateFactory) EntityStore(context.Context) (spi.EntityStore, error) {
	return f.store, nil
}

func hydrateService(ids []string, errs map[string]error) *search.SearchService {
	return search.NewSearchService(
		&hydrateFactory{store: &hydrateEntityStore{errs: errs}}, nil, &resultIDsStore{ids: ids})
}

// A storage outage during hydration used to be logged and skipped, so the caller
// got 200 with a page that is silently short and a `total` that disagrees with
// it — the same substituted answer as a 404 for an outage, one layer down. It
// must fail the page instead, with the marker intact so the door answers a
// retryable 503.
func TestGetAsyncResults_HydrationOutage_FailsThePage(t *testing.T) {
	ctx := tenantCtx("tenant-1")
	jobID := uuid.New().String()
	ids := []string{"id-1", "id-2"}
	svc := hydrateService(ids, map[string]error{"id-2": storageOutage("GetAsAt")})

	page, err := svc.GetAsyncResults(ctx, jobID, search.ResultOptions{})
	if err == nil {
		t.Fatalf("a page short by a failed read was returned as success: %d of %d results",
			len(page.Results), page.Total)
	}
	if common.StorageUnavailable(err) == nil {
		t.Errorf("storage-unavailability marker did not survive hydration: %v", err)
	}
	if errors.Is(err, search.ErrSearchJobNotFound) {
		t.Errorf("storage outage reported as a missing job: %v", err)
	}
}

// The other direction, and the reason the skip exists: an id recorded at scan
// time whose entity has since been hard-deleted is a genuine miss, not a
// failure. It is skipped and the rest of the page is served, exactly as before.
func TestGetAsyncResults_HardDeletedEntity_IsSkipped(t *testing.T) {
	ctx := tenantCtx("tenant-1")
	jobID := uuid.New().String()
	ids := []string{"id-1", "id-2"}
	svc := hydrateService(ids, map[string]error{
		"id-2": fmt.Errorf("entity id-2: %w", spi.ErrNotFound),
	})

	page, err := svc.GetAsyncResults(ctx, jobID, search.ResultOptions{})
	if err != nil {
		t.Fatalf("a hard-deleted result id failed the page: %v", err)
	}
	if len(page.Results) != 1 || page.Results[0].Meta.ID != "id-1" {
		t.Fatalf("results = %+v, want just id-1", page.Results)
	}
	if page.Total != len(ids) {
		t.Errorf("total = %d, want %d — total counts recorded ids, not hydrated ones", page.Total, len(ids))
	}
}
