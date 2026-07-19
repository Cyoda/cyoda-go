package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/pagination"
	"github.com/cyoda-platform/cyoda-go/internal/match"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// ErrSearchJobNotFound is returned by the async-job lookup paths
// (GetAsyncStatus, GetAsyncResults, CancelAsync) when the job UUID is not
// known. Handlers map this to HTTP 404 + SEARCH_JOB_NOT_FOUND — callers
// can use errors.Is to branch (issue #93).
var ErrSearchJobNotFound = errors.New("search job not found")

// SearchOptions controls search behavior.
type SearchOptions struct {
	PointInTime     *time.Time
	Limit           int
	Offset          int
	PerShardTimeout *time.Duration // nil means use node default; ignored by memory/postgres
	AllowUnbounded  bool           // opt into "no per-shard timeout"; ignored by memory/postgres
	OrderBy         []OrderKey     // sort keys; empty ⇒ entity_id asc

	// TrackingRead, when true and a transaction is active, records the
	// entities this search returns into the transaction's read-set, so
	// commit-time first-committer-wins validates them (a FOR-SHARE /
	// locking read, implemented optimistically). Default false: a plain
	// snapshot predicate read that records nothing.
	TrackingRead bool
}

// ResultOptions controls pagination when retrieving async search results.
type ResultOptions struct {
	Limit  int
	Offset int
}

// SearchJobStatus reports the current state of an async search job.
type SearchJobStatus struct {
	JobID      string
	Status     string // "RUNNING", "SUCCESSFUL", "FAILED", "CANCELLED"
	Total      int
	CreateTime time.Time
	FinishTime *time.Time
	CalcTimeMs int64
}

// SnapshotStatus is a transport-friendly summary of an async search job's state.
type SnapshotStatus struct {
	SnapshotID string
	// Status is one of: RUNNING, SUCCESSFUL, FAILED, CANCELLED, NOT_FOUND.
	// NOT_FOUND is emitted by the commercial self-executing search store on
	// snapshot-expiry races (documented in the getAsyncSearchStatus spec); it
	// is intentionally retained in the contract — do NOT remove this value
	// even though OSS backends never set it.
	Status        string
	EntitiesCount int
}

// SearchService provides synchronous and asynchronous entity search over
// the in-memory entity store, evaluating predicate conditions.
type SearchService struct {
	factory     spi.StoreFactory
	uuids       spi.UUIDGenerator
	searchStore spi.AsyncSearchStore

	// pathCache is an optional negative cache for unknown field-path
	// validation. nil-safe: when unset, validation falls back to the
	// inner-store Get + bounded RefreshAndGet pair on every request.
	pathCache *PathValidationCache

	// maxSortKeys caps the number of sort keys accepted per request across
	// all entry points (HTTP, gRPC, sync, async). Zero means use the
	// built-in default of 16.
	maxSortKeys int
}

// NewSearchService creates a SearchService backed by the given store factory.
func NewSearchService(factory spi.StoreFactory, uuids spi.UUIDGenerator, searchStore spi.AsyncSearchStore) *SearchService {
	return &SearchService{
		factory:     factory,
		uuids:       uuids,
		searchStore: searchStore,
	}
}

// WithPathValidationCache wires a negative cache for field-path
// validation. Returns the receiver so the call can chain after
// NewSearchService. The cache is optional; without it every
// validation attempt routes through the inner ModelStore. With it,
// confirmed-absent paths short-circuit until a schema-change event
// invalidates the (tenant, modelRef) bucket.
func (s *SearchService) WithPathValidationCache(c *PathValidationCache) *SearchService {
	s.pathCache = c
	return s
}

// WithMaxSortKeys sets the per-request sort-key cap enforced by
// resolveSortKeys. A value ≤ 0 restores the built-in default (16).
// Returns the receiver for chaining after NewSearchService.
func (s *SearchService) WithMaxSortKeys(n int) *SearchService {
	s.maxSortKeys = n
	return s
}

// Search performs a synchronous entity search, returning matching entities.
//
// When the plugin's EntityStore implements spi.Searcher, Search delegates to
// the plugin for SQL predicate pushdown — tx or not. Every OSS backend's
// Searcher.Search is transaction-aware: called with an active transaction in
// ctx, it honors the transaction's buffered writes and produces
// read-your-own-writes results equal to GetAll+match, so the engine no
// longer needs to special-case "in a transaction" to preserve correctness.
// The GetAll/GetAllAsAt + in-memory match fallback below now serves only two
// cases: (1) a store that does not implement spi.Searcher at all, and (2) a
// condition ConditionToFilter cannot translate to a pushdownable filter.
//
// Pre-execution path validation: every condition path is checked against
// the cached model schema's FieldsMap. When a path is unknown, the
// schema cache is refreshed exactly once via RefreshAndGet (mirroring
// entity.Handler.ValidateWithRefresh's bounded-retry contract) so a
// search referencing a peer's freshly-extended path succeeds after one
// authoritative read. Truly-unknown paths surface as 4xx BAD_REQUEST.
// Unregistered models surface as 404 MODEL_NOT_FOUND.
func (s *SearchService) Search(ctx context.Context, modelRef spi.ModelRef, cond predicate.Condition, opts SearchOptions) ([]*spi.Entity, error) {
	// Defense-in-depth: enforce the limit cap at the service layer so every
	// entry point (HTTP, gRPC, future transports) sees the same rejection.
	// The HTTP handler checks this already; gRPC does not — placing the check
	// here closes that gap without altering the unbounded (limit<0) semantics.
	if opts.Limit > pagination.MaxPageSize {
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			fmt.Sprintf("limit exceeds maximum %d", pagination.MaxPageSize))
	}

	modelStore, err := s.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}
	if appErr := common.EnsureModelRegistered(ctx, modelStore, modelRef); appErr != nil {
		return nil, appErr
	}

	if vErr := s.validateConditionPaths(ctx, modelRef, cond); vErr != nil {
		return nil, vErr
	}
	if rErr := ValidateRegexPatterns(cond); rErr != nil {
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition,
			fmt.Sprintf("invalid regex pattern in condition: %v", rErr))
	}

	orderBy, oerr := s.resolveSortKeys(ctx, modelRef, opts.OrderBy)
	if oerr != nil {
		return nil, oerr
	}

	store, err := s.factory.EntityStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get entity store: %w", err)
	}

	// Delegate to the plugin Searcher whenever it's available. Searcher.Search
	// is transaction-aware on every OSS backend (RYW), so this is safe with or
	// without an active transaction in ctx — see the Search doc comment.
	if searcher, ok := store.(spi.Searcher); ok {
		filter, translateErr := ConditionToFilter(cond)
		if translateErr == nil {
			// Map Limit < 0 (unbounded) to 0 for the SPI; SPI Limit==0 means
			// "no explicit limit" in all store implementations.
			spiLimit := opts.Limit
			if spiLimit < 0 {
				spiLimit = 0
			}
			return searcher.Search(ctx, filter, spi.SearchOptions{
				ModelName:    modelRef.EntityName,
				ModelVersion: modelRef.ModelVersion,
				PointInTime:  opts.PointInTime,
				Limit:        spiLimit,
				Offset:       opts.Offset,
				OrderBy:      orderBy,
				TrackingRead: opts.TrackingRead,
			})
		}
		// Fall through to in-memory filtering if translation fails.
		slog.Debug("condition-to-filter translation failed, falling back to in-memory",
			"pkg", "search", "error", translateErr)
	}

	// Fallback: GetAll + in-memory filtering. In-tx, this path is a rare
	// edge (a store without Searcher, or a translate-failure condition):
	// GetAll unconditionally records every returned entity into the
	// transaction's read-set (unlike the Searcher's TrackingRead-gated
	// pushdown path above), so a translate-failure search conservatively
	// widens the read-set to the whole model regardless of opts.TrackingRead.
	var entities []*spi.Entity
	if opts.PointInTime != nil {
		entities, err = store.GetAllAsAt(ctx, modelRef, *opts.PointInTime)
	} else {
		entities, err = store.GetAll(ctx, modelRef)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve entities: %w", err)
	}

	var matches []*spi.Entity
	for _, e := range entities {
		ok, matchErr := match.Match(cond, e.Data, e.Meta)
		if matchErr != nil {
			return nil, fmt.Errorf("predicate match failed: %w", matchErr)
		}
		if ok {
			matches = append(matches, e)
		}
	}

	sortEntities(matches, orderBy)

	// Apply offset.
	if opts.Offset > 0 {
		if opts.Offset >= len(matches) {
			return nil, nil
		}
		matches = matches[opts.Offset:]
	}

	// Apply limit. Default 1000 when zero; negative means unbounded (no cap).
	limit := opts.Limit
	if limit == 0 {
		limit = 1000
	}
	if limit > 0 && limit < len(matches) {
		matches = matches[:limit]
	}

	return matches, nil
}

// SubmitAsync starts an asynchronous search job and returns the job ID.
//
// Pre-execution path validation runs synchronously before the job is
// recorded (issue #77) — a request that names paths the model does not
// know about returns a 4xx without ever creating a job, sparing the
// client a round-trip through the polling endpoint.
func (s *SearchService) SubmitAsync(ctx context.Context, modelRef spi.ModelRef, cond predicate.Condition, opts SearchOptions) (string, error) {
	// Defense-in-depth: same cap as Search so the async path also fails fast
	// rather than creating a job that will fail in the background.
	if opts.Limit > pagination.MaxPageSize {
		return "", common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest,
			fmt.Sprintf("limit exceeds maximum %d", pagination.MaxPageSize))
	}

	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return "", fmt.Errorf("no user context — cannot determine tenant")
	}

	modelStore, err := s.factory.ModelStore(ctx)
	if err != nil {
		return "", common.Internal("failed to access model store", err)
	}
	if appErr := common.EnsureModelRegistered(ctx, modelStore, modelRef); appErr != nil {
		return "", appErr
	}

	if vErr := s.validateConditionPaths(ctx, modelRef, cond); vErr != nil {
		return "", vErr
	}
	if rErr := ValidateRegexPatterns(cond); rErr != nil {
		return "", common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition,
			fmt.Sprintf("invalid regex pattern in condition: %v", rErr))
	}

	// Resolve sort keys synchronously so a bad field path returns 400
	// before the job is ever created — the client gets an actionable error
	// without a polling round-trip. The resolved, Kind-bearing specs are
	// what we persist so a SelfExecutingSearchStore (which executes from
	// the persisted opts and never runs the domain resolver) orders with
	// the correct comparison class.
	orderBy, oerr := s.resolveSortKeys(ctx, modelRef, opts.OrderBy)
	if oerr != nil {
		return "", oerr
	}

	if opts.PointInTime == nil {
		now := time.Now()
		opts.PointInTime = &now
	}

	jobID := uuid.UUID(s.uuids.NewTimeUUID()).String()
	now := time.Now()

	condJSON, err := json.Marshal(cond)
	if err != nil {
		return "", fmt.Errorf("failed to marshal search condition: %w", err)
	}

	// spi.OrderSpec has no json tags, so the OrderBy slice serializes with
	// PascalCase field names (Path/Source/Desc/Kind). SelfExecutingSearchStore
	// implementations that decode this blob must expect that casing.
	optsJSON, err := json.Marshal(struct {
		Limit       int             `json:"limit"`
		Offset      int             `json:"offset"`
		PointInTime *time.Time      `json:"pointInTime,omitempty"`
		OrderBy     []spi.OrderSpec `json:"orderBy,omitempty"`
	}{
		Limit:       opts.Limit,
		Offset:      opts.Offset,
		PointInTime: opts.PointInTime,
		OrderBy:     orderBy,
	})
	if err != nil {
		return "", fmt.Errorf("failed to marshal search options: %w", err)
	}

	job := &spi.SearchJob{
		ID:          jobID,
		TenantID:    uc.Tenant.ID,
		Status:      "RUNNING",
		ModelRef:    modelRef,
		Condition:   condJSON,
		SearchOpts:  optsJSON,
		PointInTime: *opts.PointInTime,
		CreateTime:  now,
	}

	if err := s.searchStore.CreateJob(ctx, job); err != nil {
		return "", fmt.Errorf("failed to create search job: %w", err)
	}

	// Self-executing stores handle per-shard execution and result persistence
	// themselves via their own consumer/executor pipeline. Skip the in-process
	// goroutine for them — calling SaveResults or UpdateJobStatus on a
	// self-executing store is an error.
	if _, ok := s.searchStore.(spi.SelfExecutingSearchStore); ok {
		return jobID, nil
	}

	// Create a background context with the same UserContext so the search
	// can proceed after the HTTP request completes.
	bgCtx := spi.WithUserContext(context.Background(), uc)

	go func() {
		start := time.Now()
		results, searchErr := s.Search(bgCtx, modelRef, cond, opts)
		elapsed := time.Since(start)
		finishTime := time.Now()
		calcTimeMs := elapsed.Milliseconds()

		// Check if cancelled before saving results.
		currentJob, getErr := s.searchStore.GetJob(bgCtx, jobID)
		if getErr != nil {
			slog.Error("failed to get search job for status check", "pkg", "search", "jobID", jobID, "err", getErr)
			return
		}
		if currentJob.Status == "CANCELLED" {
			return
		}

		if searchErr != nil {
			if err := s.searchStore.UpdateJobStatus(bgCtx, jobID, "FAILED", 0, searchErr.Error(), finishTime, calcTimeMs); err != nil {
				slog.Error("failed to update search job status", "pkg", "search", "jobID", jobID, "err", err)
			}
			return
		}

		var ids []string
		for _, e := range results {
			ids = append(ids, e.Meta.ID)
		}

		if err := s.searchStore.SaveResults(bgCtx, jobID, ids); err != nil {
			slog.Error("failed to save search results", "pkg", "search", "jobID", jobID, "err", err)
			_ = s.searchStore.UpdateJobStatus(bgCtx, jobID, "FAILED", 0, err.Error(), finishTime, calcTimeMs)
			return
		}

		// Re-check status after SaveResults to guard against cancel race:
		// CancelAsync may have set CANCELLED between the first check and here.
		currentJob, getErr = s.searchStore.GetJob(bgCtx, jobID)
		if getErr != nil {
			slog.Error("failed to re-check search job status", "pkg", "search", "jobID", jobID, "err", getErr)
			return
		}
		if currentJob.Status != "RUNNING" {
			slog.Debug("search job status changed during execution, skipping update", "pkg", "search", "jobID", jobID, "status", currentJob.Status)
			return
		}

		if err := s.searchStore.UpdateJobStatus(bgCtx, jobID, "SUCCESSFUL", len(ids), "", finishTime, calcTimeMs); err != nil {
			slog.Error("failed to update search job status", "pkg", "search", "jobID", jobID, "err", err)
		}
	}()

	return jobID, nil
}

// GetAsyncStatus returns the current status of an async search job.
func (s *SearchService) GetAsyncStatus(ctx context.Context, jobID string) (SearchJobStatus, error) {
	job, err := s.searchStore.GetJob(ctx, jobID)
	if err != nil {
		return SearchJobStatus{}, fmt.Errorf("%w: %s", ErrSearchJobNotFound, jobID)
	}

	return SearchJobStatus{
		JobID:      job.ID,
		Status:     job.Status,
		Total:      job.ResultCount,
		CreateTime: job.CreateTime,
		FinishTime: job.FinishTime,
		CalcTimeMs: job.CalcTimeMs,
	}, nil
}

// AsyncResultsPage holds a page of async search results along with the total count.
type AsyncResultsPage struct {
	Results []*spi.Entity
	Total   int
}

// GetAsyncResults returns the results of a completed async search job.
func (s *SearchService) GetAsyncResults(ctx context.Context, jobID string, opts ResultOptions) (AsyncResultsPage, error) {
	job, err := s.searchStore.GetJob(ctx, jobID)
	if err != nil {
		return AsyncResultsPage{}, fmt.Errorf("%w: %s", ErrSearchJobNotFound, jobID)
	}

	if job.Status != "SUCCESSFUL" {
		return AsyncResultsPage{}, fmt.Errorf("job %s is not complete (status: %s)", jobID, job.Status)
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 1000
	}

	ids, total, err := s.searchStore.GetResultIDs(ctx, jobID, opts.Offset, limit)
	if err != nil {
		return AsyncResultsPage{}, fmt.Errorf("failed to get result IDs: %w", err)
	}

	entityStore, err := s.factory.EntityStore(ctx)
	if err != nil {
		return AsyncResultsPage{}, fmt.Errorf("failed to get entity store: %w", err)
	}

	var results []*spi.Entity
	for _, id := range ids {
		e, err := entityStore.GetAsAt(ctx, id, job.PointInTime)
		if err != nil {
			slog.Warn("failed to fetch entity for async result", "pkg", "search", "entityId", id, "err", err)
			continue
		}
		results = append(results, e)
	}

	return AsyncResultsPage{Results: results, Total: total}, nil
}

// CancelResult holds the outcome of a cancel attempt.
type CancelResult struct {
	Cancelled     bool
	CurrentStatus string
}

// CancelAsync attempts to cancel a running async search job.
// Returns a CancelResult indicating whether the job was cancelled and its current status.
func (s *SearchService) CancelAsync(ctx context.Context, jobID string) (CancelResult, error) {
	job, err := s.searchStore.GetJob(ctx, jobID)
	if err != nil {
		return CancelResult{}, fmt.Errorf("%w: %s", ErrSearchJobNotFound, jobID)
	}

	if job.Status != "RUNNING" {
		return CancelResult{Cancelled: false, CurrentStatus: job.Status}, nil
	}

	finishTime := time.Now()
	if err := s.searchStore.UpdateJobStatus(ctx, jobID, "CANCELLED", 0, "", finishTime, 0); err != nil {
		return CancelResult{}, fmt.Errorf("failed to cancel job: %w", err)
	}

	return CancelResult{Cancelled: true, CurrentStatus: "CANCELLED"}, nil
}

// ---------------------------------------------------------------------------
// Transport-independent service methods (for gRPC / non-HTTP callers)
// ---------------------------------------------------------------------------

// SubmitAsyncSearch starts an asynchronous search job and returns the job ID.
// This is an alias for SubmitAsync, provided for transport-independent callers.
func (s *SearchService) SubmitAsyncSearch(ctx context.Context, modelRef spi.ModelRef, cond predicate.Condition, opts SearchOptions) (string, error) {
	return s.SubmitAsync(ctx, modelRef, cond, opts)
}

// DirectSearch performs a synchronous entity search, returning matching entities.
// This is an alias for Search, provided for transport-independent callers.
func (s *SearchService) DirectSearch(ctx context.Context, modelRef spi.ModelRef, cond predicate.Condition, opts SearchOptions) ([]*spi.Entity, error) {
	return s.Search(ctx, modelRef, cond, opts)
}

// GetAsyncSearchStatus returns a transport-friendly SnapshotStatus for the given job.
func (s *SearchService) GetAsyncSearchStatus(ctx context.Context, snapshotID string) (*SnapshotStatus, error) {
	status, err := s.GetAsyncStatus(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	return &SnapshotStatus{
		SnapshotID:    status.JobID,
		Status:        status.Status,
		EntitiesCount: status.Total,
	}, nil
}

// GetAsyncSearchResults returns a page of results for a completed async search job.
func (s *SearchService) GetAsyncSearchResults(ctx context.Context, snapshotID string, page, size int) ([]*spi.Entity, error) {
	if size <= 0 {
		size = 1000
	}
	opts := ResultOptions{
		Offset: page * size,
		Limit:  size,
	}
	resultPage, err := s.GetAsyncResults(ctx, snapshotID, opts)
	if err != nil {
		return nil, err
	}
	return resultPage.Results, nil
}

// CancelAsyncSearch attempts to cancel a running async search job.
func (s *SearchService) CancelAsyncSearch(ctx context.Context, snapshotID string) error {
	_, err := s.CancelAsync(ctx, snapshotID)
	return err
}

// validateConditionPaths runs the pre-execution field-path validation
// step for Search. The cached schema is consulted first; if any path
// referenced by the condition is absent, exactly one RefreshAndGet is
// issued (when the store implements it) and the recheck decides the
// outcome. This mirrors the semantics of
// entity.Handler.ValidateWithRefresh — a stale-schema search referencing
// a peer's freshly-extended path succeeds after one bounded refresh,
// and a truly-unknown path surfaces as 4xx without a refresh loop.
//
// Returns nil when validation passes or when no data-field paths are
// addressed (lifecycle-only conditions). Validator failures surface as a
// 4xx common.AppError with the missing paths listed.
func (s *SearchService) validateConditionPaths(ctx context.Context, modelRef spi.ModelRef, cond predicate.Condition) error {
	paths := extractFieldPaths(cond)
	if len(paths) == 0 {
		return nil
	}

	// Negative cache fast-path: if any path is recorded as confirmed
	// absent for this (tenant, modelRef) at the current generation,
	// short-circuit without touching the inner store. This collapses
	// a serial flood of bad requests into one inner-store round-trip
	// per (tenant, modelRef, path) tuple between schema events.
	tenant := common.TenantFromContext(ctx)
	if cachedMissing := s.cachedAbsentPaths(tenant, modelRef, paths); len(cachedMissing) > 0 {
		return invalidPathError(cachedMissing)
	}

	modelStore, err := s.factory.ModelStore(ctx)
	if err != nil {
		// A factory that cannot produce a ModelStore cannot validate;
		// log and proceed so the search itself can still surface a
		// useful error from the matcher.
		slog.Debug("model store unavailable; skipping pre-execution path validation",
			"pkg", "search", "error", err)
		return nil
	}

	fields, err := loadFieldsMap(ctx, modelStore, modelRef)
	if err != nil {
		// Model existence is guaranteed by EnsureModelRegistered before we
		// reach here; a schema-decode failure is upstream — log and proceed
		// so the matcher's own error path can still surface a useful error.
		slog.Debug("failed to load schema for pre-execution validation",
			"pkg", "search",
			"entityName", modelRef.EntityName,
			"modelVersion", modelRef.ModelVersion,
			"error", err)
		return nil
	}
	if fields == nil {
		// Descriptor returned nil — no schema bound to validate against.
		return nil
	}

	missing := findUnknownPaths(paths, fields)
	if len(missing) == 0 {
		s.markPathsPresent(tenant, modelRef, paths)
		return nil
	}

	// Some paths are unknown to the cached schema. Refresh exactly once
	// before declaring the request invalid — the bound is required by
	// issue #77 to avoid amplifying a misconfigured client into a
	// refresh storm.
	freshFields, refreshed, refreshErr := refreshFieldsMap(ctx, modelStore, modelRef)
	if !refreshed {
		// Store has no cache layer — the cached miss is authoritative.
		s.markPathsAbsent(tenant, modelRef, missing)
		return invalidPathError(missing)
	}
	if refreshErr != nil {
		if errors.Is(refreshErr, spi.ErrNotFound) {
			// Model was deleted between Get and RefreshAndGet — fall
			// back to the cached fields outcome (paths are unknown
			// because there is no model). Do NOT populate the negative
			// cache: there is no schema authority to invalidate against.
			return invalidPathError(missing)
		}
		slog.Debug("schema refresh failed during pre-execution validation",
			"pkg", "search",
			"entityName", modelRef.EntityName,
			"modelVersion", modelRef.ModelVersion,
			"error", refreshErr)
		return invalidPathError(missing)
	}
	if freshFields == nil {
		return invalidPathError(missing)
	}

	stillMissing := findUnknownPaths(missing, freshFields)
	if len(stillMissing) == 0 {
		s.markPathsPresent(tenant, modelRef, paths)
		return nil
	}
	s.markPathsAbsent(tenant, modelRef, stillMissing)
	return invalidPathError(stillMissing)
}

// cachedAbsentPaths returns the subset of paths recorded as confirmed
// absent in the negative cache for (tenant, modelRef) at the current
// generation. Returns nil when the cache is unset or no path matches.
func (s *SearchService) cachedAbsentPaths(tenant string, ref spi.ModelRef, paths []string) []string {
	if s.pathCache == nil {
		return nil
	}
	var out []string
	for _, p := range paths {
		if s.pathCache.IsAbsent(tenant, ref, p) {
			out = append(out, p)
		}
	}
	return out
}

// markPathsAbsent records each path as confirmed absent for (tenant,
// modelRef). No-op when the cache is unset.
func (s *SearchService) markPathsAbsent(tenant string, ref spi.ModelRef, paths []string) {
	if s.pathCache == nil {
		return
	}
	for _, p := range paths {
		s.pathCache.MarkAbsent(tenant, ref, p)
	}
}

// markPathsPresent removes each path from the negative cache for
// (tenant, modelRef). Defensive: ensures a path that previously
// resolved as absent and now resolves as present is reflected without
// waiting for an invalidation event. No-op when the cache is unset.
func (s *SearchService) markPathsPresent(tenant string, ref spi.ModelRef, paths []string) {
	if s.pathCache == nil {
		return
	}
	for _, p := range paths {
		s.pathCache.MarkPresent(tenant, ref, p)
	}
}

// resolveSortKeys turns the request OrderKeys into typed OrderSpecs, validating
// scalar-leaf data paths and the meta allowlist. Returns a 400-classified
// AppError on bad input.
func (s *SearchService) resolveSortKeys(ctx context.Context, modelRef spi.ModelRef, keys []OrderKey) ([]spi.OrderSpec, error) {
	if len(keys) == 0 {
		return nil, nil
	}

	// Enforce the sort-key cap and reject duplicates before touching the
	// model store. This bounds every entry point (HTTP, gRPC, sync, async)
	// uniformly and fails fast on clearly-invalid requests.
	effMax := s.maxSortKeys
	if effMax <= 0 {
		effMax = 16
	}
	keys, cerr := capAndDedupOrderKeys(keys, effMax)
	if cerr != nil {
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidFieldPath, cerr.Error())
	}

	modelStore, err := s.factory.ModelStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get model store: %w", err)
	}
	fields, err := loadFieldsMap(ctx, modelStore, modelRef)
	if err != nil {
		return nil, fmt.Errorf("failed to load schema for sort validation: %w", err)
	}
	specs, rerr := resolveOrderBy(keys, fields)
	if rerr != nil {
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidFieldPath, rerr.Error())
	}
	return specs, nil
}

// invalidPathError builds the 4xx response surfaced when one or more
// condition paths cannot be resolved against the (refreshed) model
// schema. The message lists each offending path so clients can correct
// their request without round-tripping to the support team.
func invalidPathError(paths []string) error {
	return common.Operational(
		http.StatusBadRequest,
		common.ErrCodeInvalidFieldPath,
		fmt.Sprintf("condition references unknown field path(s): %s", strings.Join(paths, ", ")),
	)
}
