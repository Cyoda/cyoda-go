package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/pagination"
	"github.com/cyoda-platform/cyoda-go/internal/match"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// ErrSearchJobNotFound is returned by the async-job lookup paths
// (GetAsyncStatus, GetAsyncResults, CancelAsync) when the job UUID is not
// known. Handlers map this to HTTP 404 + SEARCH_JOB_NOT_FOUND — callers
// can use errors.Is to branch.
var ErrSearchJobNotFound = errors.New("search job not found")

// ErrSearchJobNotComplete is returned by GetAsyncResults when the job exists but
// has not reached SUCCESSFUL. It is a client error — the caller asked too early
// — and only stays one because it is distinguishable from a lookup failure.
var ErrSearchJobNotComplete = errors.New("search job is not complete")

// jobLookupErr preserves why an async-job lookup failed.
//
// A job that genuinely is not there keeps ErrSearchJobNotFound, which the
// transports report as 404. Every other failure is returned with its cause
// wrapped: a storage outage then reaches common.Internal still carrying the
// storage-unavailability marker and is answered with a retryable 503. Collapsing
// both into "not found" told a client during a database outage that its job did
// not exist, so it stopped retrying — a substituted answer where the contract
// requires a rejection.
//
// Every backend signals a genuine miss with spi.ErrNotFound in the chain, so the
// discriminator is the sentinel rather than the absence of a marker: a scan or
// deserialization failure is a server-side failure too, not a missing job.
func jobLookupErr(jobID string, err error) error {
	if errors.Is(err, spi.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrSearchJobNotFound, jobID)
	}
	return fmt.Errorf("failed to look up search job %s: %w", jobID, err)
}

// searchCeilingMessage is what a caller sees when their own async job exceeded
// the backend's async search ceiling. Fixed and non-revealing: GetJob serves
// this string straight back, so a raw driver error here would put SQL, a
// SQLSTATE and connection detail in a caller-facing record. Which ceiling and
// which setting stays in the log.
//
// The async status response carries no error-code field, so this string is the
// caller's entire report — it names both ways out (narrow the query, or have the
// operator change the ceiling) rather than only stating that a limit fired.
// Backend-neutral by design: any backend that bounds its async scan returns the
// same marker, so nothing here may name postgres or a driver.
const searchCeilingMessage = "search exceeded the backend's async search ceiling — " +
	"narrow the query, or have the operator raise or disable the ceiling " +
	"(see the config.database help topic)"

// jobFailureFallback replaces any failure described in terms the caller has no
// business seeing — a driver error, a recovered panic. That text is operator
// information, and the job record is caller-facing.
const jobFailureFallback = "search failed unexpectedly"

// searchCeilingExceeded is the marker a backend attaches when the async-search
// scan exceeded the ceiling that workload runs under. Matched with errors.As on
// an interface rather than a sentinel value: the marker is a plugin-side type
// this package must not import, and a backend opts in by returning the same
// shape — no SPI change, so no coordinated cross-repo release. Mirrors
// common.StorageUnavailable.
type searchCeilingExceeded interface{ SearchCeilingExceeded() bool }

// asyncScanScoper is implemented by an AsyncSearchStore whose backend bounds the
// async-search scan separately from interactive statements. It hands back a
// context the backend's own scan recognises. Backends without a separate ceiling
// simply do not implement it.
type asyncScanScoper interface {
	AsyncScanContext(ctx context.Context) context.Context
}

// jobFailureMessage is what gets written into the job record when an async
// search fails.
//
// It follows the same 4xx/5xx split as every other response: a classified
// client error carries its own already-safe text, and everything else — a
// storage failure, a driver error, an unclassified wrapper — collapses to a
// fixed string with the detail left in the log.
//
// No status surface serves this string today: neither SearchJobStatus nor
// SnapshotStatus carries a failure message, so an async caller sees FAILED and
// no reason. Hold it to the response contract regardless — it is a persisted,
// servable artefact, and the day a status surface does carry it must not be
// the day its sanitisation is first considered.
func jobFailureMessage(err error) string {
	var ceiling searchCeilingExceeded
	if errors.As(err, &ceiling) && ceiling.SearchCeilingExceeded() {
		return searchCeilingMessage
	}
	// AppError.Error() returns the client-safe Message alone; Operational
	// captures no internal detail and Internal keeps it in Detail, not Message.
	var appErr *common.AppError
	if errors.As(err, &appErr) {
		return appErr.Error()
	}
	return jobFailureFallback
}

// SearchOptions controls search behavior.
type SearchOptions struct {
	PointInTime     *time.Time
	Limit           int
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

	// healthFlag is the process-wide node-health flag the HTTP and gRPC
	// recovery paths latch false on a recovered panic. The async-search
	// executor latches the same one: a panic there runs the same engine
	// and store code, so it is the same evidence of unverified state.
	// nil-safe — unit tests that do not care about node health leave it
	// unset.
	healthFlag *atomic.Bool

	// pool is the bounded worker pool async submissions run on. Set via
	// WithAsyncPool; when unset, asyncPool() lazily constructs a small
	// built-in pool so callers that never wire one (most unit tests, and
	// packages across the tree that only care about async-job outcomes,
	// not pool sizing) still work. Production wires a config-sized pool
	// via app.go.
	pool     *WorkerPool
	poolOnce sync.Once

	// heartbeatInterval is the cadence WithHeartbeat sets. <= 0 (including
	// the zero value when WithHeartbeat is never called) falls back to
	// defaultHeartbeatInterval via heartbeatEvery().
	heartbeatInterval time.Duration

	// maxPerTenant caps how many async-search jobs one tenant may have in
	// flight on this node — queued and executing together, since the
	// registry spans both. <= 0 disables the cap. Set via
	// WithAsyncMaxPerTenant; production wires
	// app.SearchAsyncConfig.MaxPerTenant.
	//
	// Without it the pool is first-come-first-served across tenants: one
	// tenant's burst takes every worker AND fills the queue, so every other
	// tenant on the node is answered 503 SEARCH_QUEUE_FULL until those jobs
	// finish — which, for an async search, can be the backend's whole
	// async-scan ceiling.
	maxPerTenant int

	// registryMu guards registry and tenantInFlight, the jobID ->
	// in-process cancel handle map used by CancelRunning (in-process
	// immediate cancel) and AbortRegisteredJobs (shutdown drain), and the
	// per-tenant count derived from it. An entry exists for the lifetime of
	// a job on this node: from registerJob at submit time (queued or
	// executing) to deregisterJob in the executor's own defer.
	registryMu sync.Mutex
	registry   map[string]*asyncJobHandle
	// tenantInFlight counts registry entries per tenant. Kept alongside the
	// registry (not derived by scanning it) so the cap check is O(1) under
	// the same lock that makes check-then-register atomic.
	tenantInFlight map[spi.TenantID]int
}

// asyncJobHandle is what the cancel registry keeps per in-flight (queued or
// executing) job on this node.
type asyncJobHandle struct {
	// cancel cancels the job's own context (jobCtx), which is what the
	// heartbeat ticker and the executor's scan/save loop both observe. It
	// is the ONLY cancellation source a job has: the pool deliberately
	// keeps none of its own (see jobFunc in pool.go).
	cancel context.CancelFunc
	// uc is the submitting user's tenant context, needed to build a fresh
	// (non-cancelled) ctx for a shutdown-time fenced write after cancel has
	// already been called on jobCtx.
	uc *spi.UserContext
}

// defaultAsyncPoolWorkers/defaultAsyncPoolQueue size the built-in pool
// asyncPool() lazily constructs when no WithAsyncPool call ever wires one in
// — deliberately small since it only exists so library callers (tests,
// other packages) that don't care about pool sizing keep working; production
// always wires app.SearchAsyncConfig via WithAsyncPool.
const (
	defaultAsyncPoolWorkers = 4
	defaultAsyncPoolQueue   = 64
	// defaultHeartbeatInterval is heartbeatEvery()'s fallback when
	// WithHeartbeat is never called or is called with a non-positive value
	// — same library-default rationale as the pool constants above, and
	// otherwise unrelated to app.Config's own CYODA_SEARCH_JOB_HEARTBEAT_INTERVAL
	// default (15s, see app/config.go's DefaultConfig): that one sizes the
	// wired-in production interval via WithHeartbeat, this one only ever
	// applies when WithHeartbeat is skipped entirely.
	defaultHeartbeatInterval = 5 * time.Second
)

// initialEpoch is the claim epoch every engine-executed async-search job
// runs under. A job is created once and executed once by the node that
// submitted it, so its epoch never advances: the heartbeat, the SaveResults
// call and the terminal write all fence against this same value, and only
// the reaper's ClaimStale takeover (which supplies the epoch it claimed
// with) ever uses another. Named rather than repeated as a literal so the
// coupling between those four sites is visible — and so re-execution, when
// it lands, is one place to change instead of four.
const initialEpoch = 1

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

// WithHealthFlag wires the node-health flag the async-search goroutine latches
// false when it recovers a panic — the same flag the HTTP and gRPC recovery
// paths hold, so any door reaching it takes the node out of service. Returns
// the receiver for chaining after NewSearchService.
func (s *SearchService) WithHealthFlag(f *atomic.Bool) *SearchService {
	s.healthFlag = f
	return s
}

// WithMaxSortKeys sets the per-request sort-key cap enforced by
// resolveSortKeys. A value ≤ 0 restores the built-in default (16).
// Returns the receiver for chaining after NewSearchService.
func (s *SearchService) WithMaxSortKeys(n int) *SearchService {
	s.maxSortKeys = n
	return s
}

// WithAsyncPool wires the bounded worker pool async submissions run on.
// Chain immediately after NewSearchService (before any SubmitAsync call) —
// asyncPool() lazily constructs a built-in default pool on first use if this
// is never called, and that default is discarded (its workers leak, parked
// forever on an empty channel) if WithAsyncPool is called afterward. Returns
// the receiver for chaining.
func (s *SearchService) WithAsyncPool(p *WorkerPool) *SearchService {
	s.pool = p
	return s
}

// WithAsyncMaxPerTenant caps how many async-search jobs a single tenant may
// have in flight (queued or executing) on this node; further submissions
// from that tenant are rejected with the same retryable 503
// SEARCH_QUEUE_FULL the pool's own backpressure produces, until one of its
// jobs finishes. n <= 0 disables the cap, restoring the unbounded
// first-come-first-served behaviour. Returns the receiver for chaining
// after NewSearchService.
func (s *SearchService) WithAsyncMaxPerTenant(n int) *SearchService {
	s.maxPerTenant = n
	return s
}

// WithHeartbeat sets the interval the async executor stamps job liveness on
// (spi.AsyncSearchStore.Heartbeat) and polls for cross-node cancel/terminal
// status, starting at submit time. interval <= 0 restores the built-in
// default (defaultHeartbeatInterval) via heartbeatEvery(). Returns the
// receiver for chaining after NewSearchService.
func (s *SearchService) WithHeartbeat(interval time.Duration) *SearchService {
	s.heartbeatInterval = interval
	return s
}

// asyncPool returns the wired pool, lazily constructing a small built-in
// default (defaultAsyncPoolWorkers/defaultAsyncPoolQueue) the first time it
// is needed if WithAsyncPool was never called. sync.Once-guarded so a
// WithAsyncPool call racing the very first SubmitAsync can't leave two pools
// half-installed.
func (s *SearchService) asyncPool() *WorkerPool {
	s.poolOnce.Do(func() {
		if s.pool == nil {
			s.pool = NewWorkerPool(defaultAsyncPoolWorkers, defaultAsyncPoolQueue)
		}
	})
	return s.pool
}

// heartbeatEvery returns the configured heartbeat interval, or
// defaultHeartbeatInterval when WithHeartbeat was never called (or was
// called with a non-positive value).
func (s *SearchService) heartbeatEvery() time.Duration {
	if s.heartbeatInterval > 0 {
		return s.heartbeatInterval
	}
	return defaultHeartbeatInterval
}

// registerJob records jobID's in-process cancel handle so CancelRunning and
// AbortRegisteredJobs can find it. Called once at submit time, before the
// job is handed to the pool — the submitter owns the queue entry, so the
// registration (and the heartbeat ticker) span the queued state too, not
// just execution.
//
// Returns false when uc's tenant already holds maxPerTenant in-flight jobs
// on this node, or when jobID is already registered; nothing is registered in
// either case and the caller must reject the submission (SubmitAsync answers
// the shared QueueFullError, the same 503 the pool's own backpressure
// produces). The check and the increment happen under one acquisition of
// registryMu, so concurrent submissions from one tenant cannot both observe a
// free slot — this is the AUTHORITY on the cap; [SearchService.SubmitAsync]'s
// pre-check ahead of CreateJob is only a cheap filter.
//
// The duplicate-jobID guard exists so the accounting is sound structurally
// rather than by call-site discipline. Assigning unconditionally would drop
// the first handle — leaking its cancel func, so nothing could cancel that job
// — while charging the tenant twice for one map entry; the single matching
// deregister then leaves the tenant permanently one slot short. Submit's fresh
// time-UUIDs make that unreachable today, which is a property of the caller,
// not of this function.
func (s *SearchService) registerJob(jobID string, cancel context.CancelFunc, uc *spi.UserContext) bool {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	if s.registry == nil {
		s.registry = make(map[string]*asyncJobHandle)
	}
	if s.tenantInFlight == nil {
		s.tenantInFlight = make(map[spi.TenantID]int)
	}
	if _, dup := s.registry[jobID]; dup {
		return false
	}
	tenant := tenantOf(uc)
	if s.maxPerTenant > 0 && s.tenantInFlight[tenant] >= s.maxPerTenant {
		return false
	}
	s.registry[jobID] = &asyncJobHandle{cancel: cancel, uc: uc}
	s.tenantInFlight[tenant]++
	return true
}

// tenantAtCap reports whether tenant currently holds its full share of this
// node's async capacity. It is a CHEAP, NON-AUTHORITATIVE pre-check: the
// answer can go stale the instant registryMu is released, so a false here
// promises nothing and [SearchService.registerJob]'s atomic
// check-and-register remains the decision.
//
// It exists because the authoritative check runs after CreateJob, so a tenant
// hammering a node it is already capped on otherwise paid a full validation
// pass, an INSERT and a DELETE per rejected submit — write churn every other
// tenant on the node contends with, on precisely the axis the cap exists to
// sever. Skipping that work when the tenant is visibly at its cap is free and
// cannot produce a wrong ACCEPT: it only ever short-circuits to the same
// rejection registerJob would have reached.
func (s *SearchService) tenantAtCap(uc *spi.UserContext) bool {
	if s.maxPerTenant <= 0 {
		return false
	}
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	return s.tenantInFlight[tenantOf(uc)] >= s.maxPerTenant
}

// deregisterJob removes jobID's entry and releases its tenant's in-flight
// slot. Idempotent — a missing entry is a no-op, so both the queue-full
// submit path and the executor's own defer can call it without coordinating
// who runs first, and neither can double-decrement the count.
func (s *SearchService) deregisterJob(jobID string) {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	entry, ok := s.registry[jobID]
	if !ok {
		return
	}
	delete(s.registry, jobID)
	tenant := tenantOf(entry.uc)
	if n := s.tenantInFlight[tenant]; n <= 1 {
		delete(s.tenantInFlight, tenant)
	} else {
		s.tenantInFlight[tenant] = n - 1
	}
}

// tenantOf is the per-tenant cap's bucket key. SubmitAsync rejects a
// missing UserContext before it ever registers anything, so the empty
// tenant is unreachable from there; it is defined anyway so the accounting
// stays total for any other caller.
func tenantOf(uc *spi.UserContext) spi.TenantID {
	if uc == nil {
		return ""
	}
	return uc.Tenant.ID
}

// CancelRunning cancels jobID's in-process context if this node currently
// has it registered (queued or executing), returning true. Returns false
// when the job is not registered here — not yet started on this node,
// already finished, or owned by a different node in the cluster. Used by
// CancelAsync for an immediate in-process abort that does not wait for the
// next heartbeat poll to observe the store's CANCELLED write.
func (s *SearchService) CancelRunning(jobID string) bool {
	// IIFE so the lock is released via defer before entry.cancel() runs —
	// cancel() must not be called while holding registryMu, since it can
	// synchronously wake the heartbeat goroutine or the executor, either of
	// which may itself call deregisterJob (registryMu.Lock) before this
	// call returns.
	entry, ok := func() (*asyncJobHandle, bool) {
		s.registryMu.Lock()
		defer s.registryMu.Unlock()
		e, ok := s.registry[jobID]
		return e, ok
	}()
	if !ok {
		return false
	}
	entry.cancel()
	return true
}

// AbortRegisteredJobs cancels every job still registered on this node (queued
// or executing) and marks each FAILED via an initialEpoch-fenced write carrying
// the safe fallback message — called from App.Shutdown after pool.Drain, so
// a job that did not finish in the drain budget is not left RUNNING forever
// against a process that is going away. Interim disposition: the job is not
// re-queued for another node to pick up (see the shutdown re-execution
// follow-up noted in the caller). A lost race against the job's own terminal
// write (ErrAlreadyTerminal/ErrStaleClaim) is expected and logged at Warn,
// not treated as a failure of this call. Returns the number of jobs this
// call attempted to abort.
func (s *SearchService) AbortRegisteredJobs(ctx context.Context) int {
	// IIFE so the lock is released via defer before entry.cancel() runs
	// below — same reasoning as CancelRunning.
	entries := func() map[string]*asyncJobHandle {
		s.registryMu.Lock()
		defer s.registryMu.Unlock()
		snap := make(map[string]*asyncJobHandle, len(s.registry))
		for id, e := range s.registry {
			snap[id] = e
		}
		return snap
	}()

	for jobID, entry := range entries {
		entry.cancel()
		writeCtx := ctx
		if entry.uc != nil {
			writeCtx = spi.WithUserContext(ctx, entry.uc)
		}
		s.writeAsyncFailure(writeCtx, jobID, jobFailureFallback, time.Now(), 0)
	}
	return len(entries)
}

// structuralConditionErrCode classifies a ValidateCondition error for the
// Search/SubmitAsync boundary: a jsonPath outside JSON Path nomenclature
// (errInvalidFieldPath) maps to INVALID_FIELD_PATH — the same code the
// schema-driven path check emits, because both mean "that is not a field this
// request can address"; an object-operand shape violation, an unknown or
// missing operatorType (operator-semantics.md §4: "on every surface that
// carries a condition"), and an unknown group operator all wrap
// ErrInvalidCondition and map to INVALID_CONDITION; any other structural
// failure (e.g. condition depth exceeded) keeps the BAD_REQUEST default —
// nothing in the current validator set reaches it besides that one guard.
func structuralConditionErrCode(cErr error) string {
	if errors.Is(cErr, errInvalidFieldPath) {
		return common.ErrCodeInvalidFieldPath
	}
	if errors.Is(cErr, ErrInvalidCondition) {
		return common.ErrCodeInvalidCondition
	}
	return common.ErrCodeBadRequest
}

// StructuralConditionErrCode is the exported entry point for
// structuralConditionErrCode, so a caller outside this package that
// validates a condition via the exported ValidateCondition — currently
// entity.Handler's delete paths, which select entities via their own
// spi.Iterable drain instead of Search and so must replicate Search's
// pre-execution validation rather than inherit it as a side effect —
// classifies a ValidateCondition failure identically to Search/SubmitAsync
// instead of drifting onto a coarser code of its own. Mirrors the
// LoadFieldsMap/loadFieldsMap exported-wrapper shape already used in this
// package (path_validate.go).
func StructuralConditionErrCode(cErr error) string {
	return structuralConditionErrCode(cErr)
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
// authoritative read. Truly-unknown paths surface as 400 INVALID_FIELD_PATH.
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

	// Structural condition validation (canonical operator set, BETWEEN
	// arity) — model-independent, so it runs before any model-store access.
	// This is the single boundary every transport (HTTP, gRPC) funnels
	// through; the HTTP handler no longer duplicates this check.
	if cErr := ValidateCondition(cond); cErr != nil {
		return nil, common.Operational(http.StatusBadRequest, structuralConditionErrCode(cErr), cErr.Error())
	}

	modelStore, err := s.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}
	if appErr := common.EnsureModelRegistered(ctx, modelStore, modelRef); appErr != nil {
		return nil, appErr
	}

	validatedFields, vErr := s.validateConditionPaths(ctx, modelStore, modelRef, cond)
	if vErr != nil {
		return nil, vErr
	}
	if rErr := ValidatePatterns(cond); rErr != nil {
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition,
			rErr.Error())
	}
	// Condition type-soundness (correctness-over-availability): every
	// transport funnels through Search, so this is the single boundary that
	// closes the gap where gRPC previously bypassed HTTP-only validation.
	if tErr := s.validateConditionTypes(ctx, modelStore, modelRef, cond); tErr != nil {
		return nil, tErr
	}

	orderBy, oerr := s.resolveSortKeys(ctx, modelRef, opts.OrderBy)
	if oerr != nil {
		return nil, oerr
	}

	store, err := s.factory.EntityStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get entity store: %w", err)
	}

	// Delegate to the plugin for predicate pushdown whenever a translatable
	// filter and a capable store are both available — tx or not; every OSS
	// backend's Searcher and Iterable are transaction-aware (RYW), see the
	// Search doc comment. Two capabilities cover the two shapes a pushdown
	// request can take:
	//
	//   - opts.Limit > 0: spi.Searcher.Search, bounded-or-fail. Its contract
	//     requires Limit >= 1 and treats anything else as a caller error —
	//     there is no zero-means-unbounded sentinel at that interface.
	//   - opts.Limit <= 0 ("no explicit limit" — an internal caller that
	//     genuinely wants every match, unbounded: e.g. this Search entry
	//     point called directly rather than through an HTTP/gRPC handler,
	//     both of which resolve a default before reaching here): the
	//     matched entities are still read via the plugin's predicate
	//     pushdown, but streamed through spi.Iterable.Iterate instead of
	//     Searcher (which would reject the sub-1 Limit). This is the same
	//     capability the streaming async executor and streamed-delete paths
	//     already use for the identical "unbounded, want every match" shape
	//     — not a special case invented here. Iterate's TrackingRead gate
	//     records each entity into the tx read-set per-YIELD as it streams
	//     (see the Iterate doc comment). Whether that ends up tracking
	//     exactly the matched set or the whole model is per-backend, not a
	//     property of routing through Iterate itself: postgres pushes filter
	//     down into the scan, so only matches are yielded and tracked; the
	//     in-tx Iterate on memory and sqlite pushes no filter down and yields
	//     (and so tracks) every row it scans, byte-identical to what the
	//     GetAll fallback below would track. Closing that gap for memory/
	//     sqlite is tracked as follow-up work, not done here.
	//
	// Both shapes need the same FieldsMap-driven condition->filter
	// translation; a translate failure (untranslatable condition) falls
	// through to the GetAll + in-memory-match fallback below either way.
	searcher, storeIsSearcher := store.(spi.Searcher)
	iterableStore, storeIsIterable := store.(spi.Iterable)
	if (storeIsSearcher && opts.Limit > 0) || (storeIsIterable && opts.Limit <= 0) {
		// Reuse the map validateConditionPaths already loaded and validated
		// the condition's paths against. Loading it a second time here both
		// repeated the work and discarded its error, so a schema that became
		// unreadable between the two loads translated against nil — which is
		// not "no types", it is a filter whose comparison leaves annihilate
		// while its string leaves keep matching.
		filter, translateErr := spi.ConditionToFilter(cond, validatedFields)
		if translateErr == nil {
			if opts.Limit > 0 {
				res, sErr := searcher.Search(ctx, filter, spi.SearchOptions{
					ModelName:    modelRef.EntityName,
					ModelVersion: modelRef.ModelVersion,
					PointInTime:  opts.PointInTime,
					Limit:        opts.Limit,
					OrderBy:      orderBy,
					TrackingRead: opts.TrackingRead,
				})
				if appErr := ClassifyStoreQueryError(sErr); appErr != nil {
					return nil, appErr
				}
				return res, sErr
			}

			// Unbounded (Limit <= 0): stream every match via Iterate. OrderBy
			// is deliberately NOT threaded into IterateOptions here — Iterate
			// treats a non-empty OrderBy inside a transaction as an error
			// (its stronger "honour explicitly or refuse" contract, unlike
			// Searcher/GetAll), and this fallback has always sorted in Go
			// after the fact instead (see sortEntities below); leaving
			// IterateOptions.OrderBy empty keeps an in-tx ordered unbounded
			// search working exactly as it did through the GetAll fallback.
			matches, iErr := drainIterate(ctx, iterableStore, modelRef, filter, opts)
			if iErr != nil {
				// Same sentinel classification the bounded branch above
				// applies: the store is reached through a different method
				// here, but a plugin rejecting the request answers with the
				// same cross-backend sentinels and the caller-facing status
				// must not depend on which branch the request took.
				if appErr := ClassifyStoreQueryError(iErr); appErr != nil {
					return nil, appErr
				}
				return nil, iErr
			}
			sortEntities(matches, orderBy)
			return matches, nil
		}
		// Fall through to in-memory filtering if translation fails.
		slog.Debug("condition-to-filter translation failed, falling back to in-memory",
			"pkg", "search", "error", translateErr)
	}

	// Fallback: GetAll/GetAllAsAt + in-memory filtering. This path is reached
	// only when no capability fits the request shape (a store without
	// Searcher for a bounded request, or without Iterable for an unbounded
	// one) or the condition doesn't translate to a pushdownable filter. In-tx,
	// this is a rare edge: GetAll unconditionally records every returned
	// entity into the transaction's read-set (unlike the TrackingRead-gated
	// pushdown paths above), so a translate-failure search conservatively
	// widens the read-set to the whole model regardless of opts.TrackingRead.
	// The GetAllAsAt (point-in-time) branch of this same fallback records no
	// read-set at all, matching GetAsAt/GetAllAsAt's historical-read semantics.
	//
	// Two consequences of GetAll running before any bound can be evaluated,
	// worth keeping in mind reading the bounded-or-fail check below: (1) it is
	// a correctness fix, not a resource-protection one — GetAll has already
	// materialised the entire model into memory by the time the oversized
	// match set is detected, so the fix stops a truncated answer from being
	// returned, it does not avoid the memory cost of computing it; and (2)
	// in-transaction, GetAll has also already recorded every entity into the
	// transaction's read-set before the bound can raise, so a request that
	// ends in a 400 here still leaves the transaction holding a model-wide
	// read-set, same as a request that succeeds.
	var entities []*spi.Entity
	if opts.PointInTime != nil {
		entities, err = store.GetAllAsAt(ctx, modelRef, *opts.PointInTime)
	} else {
		entities, err = store.GetAll(ctx, modelRef)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve entities: %w", err)
	}

	// A nil condition matches every entity — the "no filtering" case, same
	// answer the pre-split evaluator gave (it never faulted on a nil
	// condition; it only ever evaluated one once a row reached it, and a nil
	// condition never reached the per-row evaluation at all). match.Prepare
	// has no clause for a nil predicate.Condition — every concrete type in the
	// sum type is a struct, never nil — so calling it here would report
	// "unknown condition type: <nil>" and turn an empty model's "no filter"
	// query into a 500. Not reachable today (predicate.ParseCondition never
	// returns (nil, nil), and the one nil-capable caller short-circuits
	// earlier), but the guard is cheap and keeps the fallback's answer
	// independent of that being true forever.
	var matches []*spi.Entity
	if cond == nil {
		matches = entities
	} else {
		// Declared-type resolver for the predicate evaluator: the type-directed
		// kernel compares temporal data fields temporally (not lexically) only when
		// the model supplies their declared subtype. Load the model's FieldsMap so
		// this in-memory fallback path matches the pushdown's typing. A genuine
		// store/schema-load error fails closed (correctness-over-availability): the
		// model schema is a required input for correct typing, so we surface the
		// error rather than silently under-match with untyped leaves. The
		// no-schema-registered case is (nil, nil) — fields stays nil, the resolver
		// returns nil types, and a comparison/range leaf on that path now fails
		// Prepare (see the prepErr check right below) rather than degrading to a
		// silent non-match: an unevaluable leaf is a structural fault in the
		// condition, not a row-dependent answer to guess at.
		fallbackFields, ffErr := loadFieldsMap(ctx, modelStore, modelRef)
		if ffErr != nil {
			return nil, fmt.Errorf("failed to load model field types: %w", ffErr)
		}
		fieldTypes := func(p string) []spi.DataType {
			if fd, ok := fallbackFields[p]; ok {
				return fd.Types
			}
			return nil
		}

		// Prepared once for the whole scan. Everything the leaf evaluator can fault
		// on is a structural property of the condition, so it surfaces here rather
		// than on whichever row happens to reach it first.
		prepared, prepErr := match.Prepare(cond, fieldTypes)
		if prepErr != nil {
			// Classify before the generic wrap: match.ErrUnevaluableLeaf /
			// match.ErrUnsupportedOperator are client-input faults
			// (ClassifyStoreQueryError's own doc explains the mapping), not
			// a storage failure — an unclassified wrap wrongly answered 500
			// plus a support ticket for input that is simply malformed.
			if appErr := ClassifyStoreQueryError(prepErr); appErr != nil {
				return nil, appErr
			}
			return nil, fmt.Errorf("predicate match failed: %w", prepErr)
		}

		// Amortized cancellation check (spec D9): a client-requested
		// timeoutMillis must abort this scan rather than let it run to
		// completion and return results computed past the deadline. This
		// branch is reached only when the store does not implement
		// spi.Searcher — all three OSS backends do, so it is a rare edge
		// (translate-failure or a non-Searcher store) — but a match set can
		// still be large, so the check runs every 1024 entities (i&1023==0,
		// true at i==0 too) rather than on every iteration, keeping the
		// per-entity cost off the hot path while still bounding how much
		// stale work a pre-expired or since-expired ctx can produce.
		for i, e := range entities {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, fmt.Errorf("search aborted: %w", err)
				}
			}
			if prepared.Match(e.Data, e.Meta) {
				matches = append(matches, e)
			}
		}
	}

	sortEntities(matches, orderBy)

	// Bounded-or-fail, same contract as the Searcher path above. A truncated
	// prefix here would be indistinguishable from a complete result, so an
	// oversized match set is an error rather than a silently shortened one.
	// Limit <= 0 is unbounded (async submit, scoped delete) and never raises;
	// the direct entry points resolve an omitted client limit to
	// DefaultDirectSearchLimit before reaching the service, so 0 here means an
	// explicit store-all (async submit or an internal caller), never "client
	// omitted".
	if opts.Limit > 0 && len(matches) > opts.Limit {
		return nil, common.Operational(http.StatusBadRequest,
			common.ErrCodeSearchResultLimit,
			"matched result count exceeds the configured limit").WithCause(spi.ErrSearchResultLimitExceeded)
	}

	return matches, nil
}

// ClassifyStoreQueryError maps the cross-backend sentinels a storage plugin
// may return from a pushdown query (Searcher.Search, Iterable.Iterate,
// GroupedAggregator.GroupedAggregate) onto operational AppErrors, and returns
// nil for anything it does not recognise so the caller can pass the error
// through — an unrecognised store error is genuinely a 500.
//
// Each mapping preserves the sentinel via WithCause, so an errors.Is check
// further up still holds.
//
// spi.ErrInvalidFilterPath is the one that is easy to omit and expensive to
// get wrong. It is a plugin's BACKSTOP against a path outside the model's
// syntax — input the engine boundary should already have rejected 400. Left
// unclassified it surfaced as a 500 plus a support ticket for input that is
// simply malformed, and it contradicted the contract COMPATIBILITY.md
// documents: the engine uses the sentinel to tell "invalid input, 400" from
// "valid but unpushdownable, fall back". Reaching it means the boundary grammar
// and a plugin's own check disagree, which is worth a WARN — but the caller's
// answer is still 400, because the input is what is wrong.
//
// spi.ErrUnevaluableLeaf and spi.ErrInvalidPattern are the SPI kernel's own
// Prepare (spi.Filter side) refusing an operand it cannot type-check or a
// pattern it cannot compile — the § 14.4 commercial-backend obligation this
// mapping exists to satisfy. Both are the same class of backstop as
// ErrInvalidFilterPath (input the boundary should already have rejected) and
// both collapse to 400 INVALID_CONDITION: the leaf is malformed input, not a
// storage fault, and neither sentinel is granular enough to say whether the
// underlying cause was a type mismatch, a path problem, or an uncompilable
// pattern (spi.ErrUnevaluableLeaf's own doc lists all three as one cause).
//
// match.ErrUnevaluableLeaf and match.ErrUnsupportedOperator are
// internal/match's OWN Prepare (predicate.Condition side, a different
// evaluator entirely from spi.Prepare — see prepared.go's own package doc:
// "this package's error set is its own, not a mirror of spi.ErrUnevaluableLeaf,
// but the disposition is the same") reached through search.Service.Search's
// GetAll+match fallback, entity's conditional-delete planner, and
// grouped-stats' streaming tally. Every caller of those previously wrapped
// the error generically ("predicate match failed: %w") or propagated it raw,
// which classified as an unrecognised 500 — the identical defect
// ErrInvalidFilterPath's omission was, just on the residual-evaluator side
// rather than the pushdown side.
//
// Both match.ErrUnevaluableLeaf and match.ErrUnsupportedOperator map to 400
// INVALID_CONDITION — the SAME code as their SPI-side counterparts, not
// CONDITION_TYPE_MISMATCH. Two reasons: (1) match.ErrUnevaluableLeaf itself
// wraps three distinct causes (prepared.go's leafNode expansion failure,
// its empty-leaf-path guard, its path-outside-grammar guard) and only the
// first is ever a type mismatch, so a single dedicated code would be wrong
// on its own terms for the other two; (2) a NOT condition can route the
// identical leaf through either evaluator depending on whether
// spi.ConditionToFilter can translate it — the query PLAN, not anything
// about the input — so the client-visible status must not depend on which
// evaluator happened to run, exactly the invariant this whole feature's
// pushdown/residual split must preserve one level up.
//
// match.ErrUnevaluableLeaf and spi.ErrUnevaluableLeaf share both a name and
// an error message ("unevaluable leaf") by design — two evaluators, one
// disposition — so the two cases below are logged with an explicit "source"
// field: a caller reaching for the wrong sentinel in a log-line search would
// otherwise have no way to tell which evaluator actually rejected the leaf.
//
// Exported so callers outside this package that drive a store with an
// engine-translated Filter (entity's grouped-stats service) or run
// internal/match's residual evaluator directly (entity's conditional-delete
// planner, grouped-stats' streaming tally) classify identically instead of
// maintaining a second copy of the table.
func ClassifyStoreQueryError(err error) *common.AppError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, spi.ErrSearchResultLimitExceeded):
		return common.Operational(http.StatusBadRequest,
			common.ErrCodeSearchResultLimit,
			"matched result count exceeds the configured limit").WithCause(err)
	case errors.Is(err, spi.ErrInvalidFilterPath):
		slog.Warn("storage backend rejected a path the boundary grammar accepted",
			"pkg", "search", "err", err)
		return common.Operational(http.StatusBadRequest,
			common.ErrCodeInvalidFieldPath,
			"condition or sort references an invalid field path").WithCause(err)
	case errors.Is(err, spi.ErrUnevaluableLeaf):
		slog.Warn("storage backend could not evaluate a condition leaf the boundary accepted",
			"pkg", "search", "source", "spi.Prepare", "err", err)
		return common.Operational(http.StatusBadRequest,
			common.ErrCodeInvalidCondition,
			"condition contains a leaf the backend cannot evaluate").WithCause(err)
	case errors.Is(err, spi.ErrInvalidPattern):
		slog.Warn("storage backend could not compile a condition pattern the boundary accepted",
			"pkg", "search", "source", "spi.ValidateLeafPattern", "err", err)
		return common.Operational(http.StatusBadRequest,
			common.ErrCodeInvalidCondition,
			"condition contains a pattern operand the backend cannot compile").WithCause(err)
	case errors.Is(err, match.ErrUnevaluableLeaf):
		slog.Warn("residual evaluator could not evaluate a condition leaf the boundary accepted",
			"pkg", "search", "source", "match.Prepare", "err", err)
		return common.Operational(http.StatusBadRequest,
			common.ErrCodeInvalidCondition,
			"condition contains a leaf the evaluator cannot evaluate").WithCause(err)
	case errors.Is(err, match.ErrUnsupportedOperator):
		return common.Operational(http.StatusBadRequest,
			common.ErrCodeInvalidCondition,
			"condition uses an operator the evaluator does not support").WithCause(err)
	}
	return nil
}

// drainIterate runs an unbounded (Limit <= 0) pushdown search via
// spi.Iterable.Iterate, draining the iterator fully into a slice for
// Search's synchronous return contract. TrackingRead and PointInTime forward
// unchanged; OrderBy is intentionally omitted (see the call site's comment)
// — the caller sorts the drained slice itself, matching the GetAll
// fallback's own sort-after-collect shape.
//
// Err() is read after Close(), not before: some Iterator implementations
// only surface a sticky scan error at Close (mirrors the same ordering the
// streaming async executor uses at its own drain site).
func drainIterate(ctx context.Context, store spi.Iterable, modelRef spi.ModelRef, filter spi.Filter, opts SearchOptions) ([]*spi.Entity, error) {
	it, err := store.Iterate(ctx, modelRef, filter, spi.IterateOptions{
		PointInTime:  opts.PointInTime,
		TrackingRead: opts.TrackingRead,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to iterate entities: %w", err)
	}

	var matches []*spi.Entity
	var scanErr error
	func() {
		defer func() {
			if closeErr := it.Close(); closeErr != nil && scanErr == nil {
				scanErr = closeErr
			}
			if errErr := it.Err(); errErr != nil {
				scanErr = errErr
			}
		}()
		for it.Next() {
			matches = append(matches, it.Entity())
		}
	}()
	if scanErr != nil {
		return nil, fmt.Errorf("failed to iterate entities: %w", scanErr)
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

	// Structural condition validation (canonical operator set, BETWEEN
	// arity) — same single boundary as Search, so an async job is never
	// created for a structurally-malformed condition regardless of transport.
	if cErr := ValidateCondition(cond); cErr != nil {
		return "", common.Operational(http.StatusBadRequest, structuralConditionErrCode(cErr), cErr.Error())
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

	if _, vErr := s.validateConditionPaths(ctx, modelStore, modelRef, cond); vErr != nil {
		return "", vErr
	}
	if rErr := ValidatePatterns(cond); rErr != nil {
		return "", common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition,
			rErr.Error())
	}
	// Condition type-soundness (correctness-over-availability): same
	// single-boundary guard as Search, so an async job is never created for
	// a type-unsound condition regardless of transport.
	if tErr := s.validateConditionTypes(ctx, modelStore, modelRef, cond); tErr != nil {
		return "", tErr
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

	// Cheap non-authoritative cap pre-check, placed here so that a tenant
	// already at its cap costs the store NOTHING: without it every rejected
	// submit still ran an INSERT and a compensating DELETE, write churn every
	// other tenant on the node contends with — on precisely the axis the cap
	// exists to sever. It is sequenced AFTER the 4xx validation above so a
	// malformed request still gets its actionable 400 rather than a 503 that
	// hides it. registerJob below remains the authority; this can only
	// short-circuit to the same rejection it would have reached.
	if s.tenantAtCap(uc) {
		slog.Warn("async search submission rejected: tenant at its in-flight cap",
			"pkg", "search", "tenant", uc.Tenant.ID, "maxPerTenant", s.maxPerTenant)
		return "", QueueFullError()
	}

	jobID := uuid.UUID(s.uuids.NewTimeUUID()).String()
	now := time.Now()

	// The job record carries the client's condition in DOMAIN wire syntax,
	// untranslated, by design. A SelfExecutingSearchStore is its only reader
	// and MUST translate it itself through the SPI's own ConditionToFilter
	// over FieldsMapFromSchema — which is why both live there. Persisting an
	// already-translated spi.Filter here was considered and rejected.
	condJSON, err := json.Marshal(cond)
	if err != nil {
		return "", fmt.Errorf("failed to marshal search condition: %w", err)
	}

	// spi.OrderSpec has no json tags, so the OrderBy slice serializes with
	// PascalCase field names (Path/Source/Desc/Kind). SelfExecutingSearchStore
	// implementations that decode this blob must expect that casing.
	optsJSON, err := json.Marshal(struct {
		Limit       int             `json:"limit"`
		PointInTime *time.Time      `json:"pointInTime,omitempty"`
		OrderBy     []spi.OrderSpec `json:"orderBy,omitempty"`
	}{
		Limit:       opts.Limit,
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

	// Async search is the one workload whose purpose is to run long. A backend
	// that bounds it separately from its interactive statements marks the
	// context here, so the scan below runs under that ceiling instead of the one
	// sized for a user waiting on a response.
	if scoper, ok := s.searchStore.(asyncScanScoper); ok {
		bgCtx = scoper.AsyncScanContext(bgCtx)
	}

	// jobCtx is what the heartbeat ticker and the executor's scan/save
	// loop observe; the pool contributes no context of its own (jobFunc).
	// Registered — and the heartbeat ticker started — before the job is
	// handed to the pool: the submitter owns the queue entry, so both span
	// the queued state, not just execution.
	jobCtx, cancel := context.WithCancel(bgCtx)
	if !s.registerJob(jobID, cancel, uc) {
		// This tenant already holds its full share of this node's async
		// capacity. Same disposition as a pool rejection below — the job
		// never entered the queue, so the row is deleted rather than left
		// RUNNING — and the same caller-facing error, so HTTP and gRPC stay
		// in lock-step through QueueFullError's single source of truth.
		cancel()
		if delErr := s.searchStore.DeleteJob(bgCtx, jobID); delErr != nil {
			slog.Error("failed to delete search job after per-tenant cap rejection", "pkg", "search", "jobID", jobID, "err", delErr)
		}
		slog.Warn("async search submission rejected: tenant at its in-flight cap",
			"pkg", "search", "tenant", uc.Tenant.ID, "maxPerTenant", s.maxPerTenant)
		return "", QueueFullError()
	}
	s.startHeartbeat(jobCtx, cancel, jobID)

	submitErr := s.asyncPool().Submit(func() {
		s.runAsyncJob(jobCtx, cancel, jobID, modelRef, cond, opts, orderBy)
	})
	if submitErr != nil {
		// The job never entered the queue, so there was never a claim to
		// fence a terminal write against — delete the row rather than
		// writing FAILED, so it does not linger RUNNING.
		cancel()
		s.deregisterJob(jobID)
		if delErr := s.searchStore.DeleteJob(bgCtx, jobID); delErr != nil {
			slog.Error("failed to delete search job after queue rejection", "pkg", "search", "jobID", jobID, "err", delErr)
		}
		return "", submitErr
	}

	return jobID, nil
}

// startHeartbeat runs the dedicated heartbeat ticker goroutine for a job,
// from submit time (queued or executing) until jobCtx is done. Every tick it
// stamps liveness (Heartbeat) and polls GetJob for any terminal status —
// cross-node cancel and terminal abort in one poll — cancelling jobCtx (and
// so stopping itself) on either a Heartbeat error (fenced out — a stale
// claim or an already-terminal job) or an observed non-RUNNING status.
func (s *SearchService) startHeartbeat(jobCtx context.Context, cancel context.CancelFunc, jobID string) {
	interval := s.heartbeatEvery()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				if err := s.searchStore.Heartbeat(jobCtx, jobID, initialEpoch); err != nil {
					slog.Warn("async search heartbeat failed; aborting job", "pkg", "search", "jobID", jobID, "err", err)
					cancel()
					return
				}
				job, err := s.searchStore.GetJob(jobCtx, jobID)
				if err != nil {
					slog.Warn("async search heartbeat status poll failed", "pkg", "search", "jobID", jobID, "err", err)
					continue
				}
				if job.Status != "RUNNING" {
					cancel()
					return
				}
			}
		}
	}()
}

// runAsyncJob is the executor: it runs once a worker picks jobID up off the
// pool (or, for a test driving it directly, whenever called). It streams
// matches through Iterate → SaveResults instead of materializing the full
// result set first, and records a single initialEpoch-fenced terminal write.
// cancel stops the heartbeat ticker (via jobCtx) on every exit path.
func (s *SearchService) runAsyncJob(jobCtx context.Context, cancel context.CancelFunc, jobID string, modelRef spi.ModelRef, cond predicate.Condition, opts SearchOptions, resolvedOrderBy []spi.OrderSpec) {
	defer cancel()
	defer s.deregisterJob(jobID)

	start := time.Now()

	// A panic anywhere in this function (or anything it calls) runs with no
	// HTTP handler above it to recover it — net/http's per-connection
	// recover has nothing to do with a pool worker goroutine. Left
	// unrecovered, it takes the whole process down, the same class of gap
	// the gRPC and HTTP mux doors had. Mirrors the scheduler's own dispatch
	// goroutine (internal/scheduler/service.go): log the full panic detail
	// (value + stack) and record the job FAILED with a non-revealing
	// message — a job left RUNNING forever after its executor died would be
	// its own defect (Gate 3: no panic value or stack leaves the log).
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic recovered in async search job", "pkg", "search",
				"jobID", jobID, "err", fmt.Errorf("panic: %v", rec),
				"stack", string(debug.Stack()))
			// Same latch as the HTTP and gRPC doors: this executor runs the
			// same engine and store code, so a panic here is the same
			// evidence that the node's state is unverified. Nothing resets
			// it — the node reports 503 on /health and /readyz and stops
			// taking client traffic.
			if s.healthFlag != nil {
				s.healthFlag.Store(false)
			}
			// jobCtx may or may not be cancelled at this point, but the write
			// must land regardless — writeAsyncFailure strips cancellation
			// itself (keeping the UserContext value), so a store that aborts
			// in-flight work on ctx.Err() still accepts it.
			s.writeAsyncFailure(jobCtx, jobID, jobFailureFallback, time.Now(), 0)
		}
	}()

	modelStore, err := s.factory.ModelStore(jobCtx)
	if err != nil {
		s.writeAsyncFailure(jobCtx, jobID, jobFailureMessage(err), time.Now(), time.Since(start).Milliseconds())
		return
	}
	// Fail the job rather than answering without the schema. This load is
	// SEPARATE from the one submit-time validation performed, so the schema
	// can become unreadable in between — and a nil fields map does not make
	// the condition unevaluable, it makes it evaluate WRONGLY: empty Declared
	// annihilates the eight comparison and ordering leaves to a non-match
	// while the other eighteen keep matching (see spi.ConditionToFilter), so
	// the job would record a short result set as SUCCESSFUL.
	fields, fieldsErr := loadFieldsMap(jobCtx, modelStore, modelRef)
	if fieldsErr != nil {
		// Classify before recording. A raw error falls through
		// jobFailureMessage to the generic fallback, so a tenant whose model
		// store is down would read an actionable code on /search/direct and
		// "search failed unexpectedly" here, for one and the same outage.
		// common.Internal always returns a non-nil *AppError, and only its
		// Message reaches the persisted job record — never its Detail.
		appErr := common.Internal("failed to load model schema for condition validation", fieldsErr)
		s.writeAsyncFailure(jobCtx, jobID, jobFailureMessage(appErr), time.Now(), time.Since(start).Milliseconds())
		return
	}
	filter, translateErr := spi.ConditionToFilter(cond, fields)

	var (
		count   int
		prodErr error
		saveErr error
	)

	if translateErr != nil {
		// Untranslatable condition: unchanged interim fallback. Search's own
		// Searcher-pushdown branch independently attempts (and, for the same
		// reason, fails) the same translation, so calling it here reaches
		// its GetAll + in-memory-match branch — the same code, not a
		// duplicate of it — and the resulting IDs are streamed through the
		// same SaveResults call as the Iterate path below.
		results, searchErr := s.Search(jobCtx, modelRef, cond, opts)
		if searchErr != nil {
			prodErr = searchErr
		} else {
			ids := make([]string, len(results))
			for i, e := range results {
				ids[i] = e.Meta.ID
			}
			count = len(ids)
			saveErr = s.searchStore.SaveResults(jobCtx, jobID, initialEpoch, slices.Values(ids))
		}
	} else {
		entityStore, err := s.factory.EntityStore(jobCtx)
		if err != nil {
			s.writeAsyncFailure(jobCtx, jobID, jobFailureMessage(err), time.Now(), time.Since(start).Milliseconds())
			return
		}
		iterableStore, ok := entityStore.(spi.Iterable)
		if !ok {
			// Fail closed (correctness-over-availability): every in-house
			// store implements spi.Iterable; a store that implements only
			// Searcher cannot serve the engine-executed streaming async
			// path, and there is no lesser-quality answer to fall back to.
			slog.Error("async search store does not implement spi.Iterable", "pkg", "search", "jobID", jobID)
			s.writeAsyncFailure(jobCtx, jobID, jobFailureFallback, time.Now(), time.Since(start).Milliseconds())
			return
		}

		orderBy := resolvedOrderBy
		if len(orderBy) == 0 {
			// Iterate's own empty-OrderBy contract is merely "unspecified"
			// (unlike Search/Searcher, where empty already means the
			// engine's canonical entity-ID order) — request that order
			// explicitly so async results keep today's default order.
			orderBy = []spi.OrderSpec{{Source: spi.SourceMeta, Path: "id"}}
		}

		it, iterErr := iterableStore.Iterate(jobCtx, modelRef, filter, spi.IterateOptions{
			PointInTime: opts.PointInTime,
			OrderBy:     orderBy,
		})
		if iterErr != nil {
			// Classify exactly as the synchronous door does. The job record
			// is the only report an async caller gets, and jobFailureMessage
			// renders an *AppError's client-safe text while collapsing
			// anything else to the generic fallback — so an unclassified
			// sentinel turned a client's own malformed request into
			// "search failed unexpectedly". Reaching a plugin's path
			// rejection at all means the boundary grammar and that plugin
			// disagree; the caller is still owed the 400.
			prodErr = iterErr
			if appErr := ClassifyStoreQueryError(iterErr); appErr != nil {
				prodErr = appErr
			}
		} else {
			// IIFE so `defer it.Close()` fires at the end of THIS scope —
			// before the terminal write below, and unconditionally
			// (including if SaveResults or the scan loop panics: the
			// defer still runs during the panic unwind, ahead of the
			// panic-recovery defer above) — rather than at runAsyncJob's
			// own return, which would run after the terminal write.
			count, saveErr, prodErr = func() (n int, sErr, pErr error) {
				// Named returns: the deferred closure sets pErr AFTER
				// Close() runs — some implementations only surface a
				// sticky scan error at Close, not at the last Next(), so
				// reading it.Err() in the function body (before Close)
				// would miss it.
				//
				// A Close() error is fatal here, not merely logged: for
				// database/sql-backed iterators (e.g. sqliteIter), Close()
				// returns rows.Close()'s error and that error is NOT folded
				// into Rows.Err() — so it.Err() alone can stay nil while a
				// mid-scan driver error truncated the result set. Treating
				// Close's error as advisory would let this job land
				// SUCCESSFUL with a truncated result set, indistinguishable
				// from a complete one (matches drainIterate's ordering).
				defer func() {
					if closeErr := it.Close(); closeErr != nil {
						slog.Warn("failed to close async search iterator", "pkg", "search", "jobID", jobID, "err", closeErr)
						pErr = closeErr
					}
					if errErr := it.Err(); errErr != nil {
						pErr = errErr
						// A sticky scan error carries the same
						// cross-backend sentinels Iterate's own error
						// does — classify it identically rather than
						// letting the door it surfaced on decide.
						if appErr := ClassifyStoreQueryError(errErr); appErr != nil {
							pErr = appErr
						}
					}
				}()
				seq := func(yield func(string) bool) {
					for it.Next() {
						// Counted AFTER the yield returns true: a
						// false return means the consumer declined
						// this id, so it is not part of the result
						// set and must not be reported as one — the
						// job's status would otherwise advertise a
						// result GetAsyncResults cannot serve.
						if !yield(it.Entity().Meta.ID) {
							return
						}
						n++
					}
				}
				sErr = s.searchStore.SaveResults(jobCtx, jobID, initialEpoch, seq)
				return
			}()
		}
	}

	finishTime := time.Now()
	calcTimeMs := time.Since(start).Milliseconds()

	switch {
	case jobCtx.Err() != nil:
		// Cancelled — in-process CancelRunning, a cross-node cancel or
		// terminal status the heartbeat poll observed, or a heartbeat
		// fencing failure. context.WithoutCancel so the recovery READ below
		// is not itself aborted by the same cancellation (the write that
		// follows strips cancellation on its own).
		recoveryCtx := context.WithoutCancel(jobCtx)
		job, getErr := s.searchStore.GetJob(recoveryCtx, jobID)
		if getErr == nil && job.Status == "CANCELLED" {
			// The store already stamped the terminal write (Cancel, or a
			// takeover's own terminal write) — nothing left to record.
			return
		}
		s.writeAsyncFailure(recoveryCtx, jobID, jobFailureFallback, finishTime, calcTimeMs)
		return
	case prodErr != nil:
		slog.Warn("async search job failed", "pkg", "search", "jobID", jobID, "err", prodErr)
		s.writeAsyncFailure(jobCtx, jobID, jobFailureMessage(prodErr), finishTime, calcTimeMs)
		return
	case saveErr != nil:
		slog.Error("failed to save search results", "pkg", "search", "jobID", jobID, "err", saveErr)
		s.writeAsyncFailure(jobCtx, jobID, jobFailureMessage(saveErr), finishTime, calcTimeMs)
		return
	}

	// context.WithoutCancel, exactly as the panic path above: the switch has
	// just established that jobCtx was live, but the heartbeat goroutine
	// cancels it from a different goroutine — a fenced-out Heartbeat or a
	// poll that observes a terminal status — and a cancel landing in the
	// window between that check and this call would abort the write and
	// leave a finished job RUNNING until the stale-job reaper failed it. The
	// UserContext (and so the tenant scope) is preserved.
	if err := s.searchStore.UpdateJobStatus(context.WithoutCancel(jobCtx), jobID, initialEpoch, "SUCCESSFUL", count, "", finishTime, calcTimeMs); err != nil {
		if errors.Is(err, spi.ErrAlreadyTerminal) || errors.Is(err, spi.ErrStaleClaim) {
			slog.Warn("async search terminal write lost the race; state already settled", "pkg", "search", "jobID", jobID, "err", err)
			return
		}
		slog.Error("failed to update search job status", "pkg", "search", "jobID", jobID, "err", err)
	}
}

// writeAsyncFailure records jobID FAILED via an initialEpoch-fenced write. A lost
// race against the job's own (or a takeover's) terminal write
// (ErrAlreadyTerminal/ErrStaleClaim) is expected and logged at Warn, not
// treated as a failure of the caller — the correct state is already
// recorded.
//
// The write runs on context.WithoutCancel(ctx), and that is decided HERE
// rather than at each of the six call sites: this is a terminal write by
// definition, so no caller wants it cancellable. Two of the call sites are
// reached only after `jobCtx.Err() != nil` evaluated false, which makes them a
// TOCTOU — the heartbeat goroutine cancels jobCtx from another goroutine (a
// fenced-out Heartbeat, or a poll that observes a terminal status), and a
// cancel landing between the check and the write aborts it, leaving a finished
// job RUNNING until the stale-job reaper fails it with a generic message
// instead of the real reason recorded here. Stripping cancellation keeps the
// UserContext (and so the tenant scope) intact; wrapping an already-stripped
// context again is a no-op, so the cancellation-recovery call site loses
// nothing by it.
func (s *SearchService) writeAsyncFailure(ctx context.Context, jobID, msg string, finishTime time.Time, calcTimeMs int64) {
	ctx = context.WithoutCancel(ctx)
	if err := s.searchStore.UpdateJobStatus(ctx, jobID, initialEpoch, "FAILED", 0, msg, finishTime, calcTimeMs); err != nil {
		if errors.Is(err, spi.ErrAlreadyTerminal) || errors.Is(err, spi.ErrStaleClaim) {
			slog.Warn("async search terminal write lost the race; state already settled", "pkg", "search", "jobID", jobID, "err", err)
			return
		}
		slog.Error("failed to update search job status", "pkg", "search", "jobID", jobID, "err", err)
	}
}

// GetAsyncStatus returns the current status of an async search job.
func (s *SearchService) GetAsyncStatus(ctx context.Context, jobID string) (SearchJobStatus, error) {
	job, err := s.searchStore.GetJob(ctx, jobID)
	if err != nil {
		return SearchJobStatus{}, jobLookupErr(jobID, err)
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
		return AsyncResultsPage{}, jobLookupErr(jobID, err)
	}

	if job.Status != "SUCCESSFUL" {
		return AsyncResultsPage{}, fmt.Errorf("%w: %s (status: %s)", ErrSearchJobNotComplete, jobID, job.Status)
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 1000
	}

	ids, total, err := s.searchStore.GetResultIDs(ctx, jobID, opts.Offset, limit)
	if err != nil {
		// The job was there a moment ago, so this is either a store failure or a
		// job reaped in the window between the two reads. No backend tags the
		// latter on this call, so ask the one that does. Only an affirmative miss
		// answers 404: a store that is merely failing cannot confirm one, and the
		// cause is returned intact instead of a not-found inferred from it.
		if _, getErr := s.searchStore.GetJob(ctx, jobID); errors.Is(getErr, spi.ErrNotFound) {
			return AsyncResultsPage{}, fmt.Errorf("%w: %s", ErrSearchJobNotFound, jobID)
		}
		return AsyncResultsPage{}, fmt.Errorf("failed to get result IDs: %w", err)
	}

	entityStore, err := s.factory.EntityStore(ctx)
	if err != nil {
		return AsyncResultsPage{}, fmt.Errorf("failed to get entity store: %w", err)
	}

	// A result id whose entity is genuinely gone — hard-deleted since the scan
	// recorded it — is skipped, and the page comes back short by it while `total`
	// still counts the recorded ids. That is the documented shape of this
	// endpoint and is unchanged.
	//
	// A read that merely FAILED is not that. Skipping it too would answer 200
	// with a page silently short by however many entities the store could not
	// serve, which is a wrong-but-available result
	// (.claude/rules/correctness-over-availability.md) and the same substituted
	// answer as reporting an outage as not-found. It fails the page instead,
	// carrying the cause so a storage outage reaches the door as a retryable 503.
	var results []*spi.Entity
	for _, id := range ids {
		e, err := entityStore.GetAsAt(ctx, id, job.PointInTime)
		if err != nil {
			if !errors.Is(err, spi.ErrNotFound) {
				return AsyncResultsPage{}, fmt.Errorf("failed to fetch entity %s for async result: %w", id, err)
			}
			slog.Warn("async result id has no entity — hard-deleted since the scan recorded it",
				"pkg", "search", "entityId", id, "err", err)
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
		return CancelResult{}, jobLookupErr(jobID, err)
	}

	if job.Status != "RUNNING" {
		return CancelResult{Cancelled: false, CurrentStatus: job.Status}, nil
	}

	finishTime := time.Now()
	if err := s.searchStore.Cancel(ctx, jobID, finishTime); err != nil {
		return CancelResult{}, fmt.Errorf("failed to cancel job: %w", err)
	}

	// Best-effort in-process abort: if this node happens to be running (or
	// still has queued) the job, stop it immediately rather than waiting up
	// to one heartbeat interval for its own poll to observe the CANCELLED
	// write above. A cross-node cancel (the job runs on a different node)
	// still lands — that node's own heartbeat poll picks up the terminal
	// status within one interval.
	s.CancelRunning(jobID)

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
func (s *SearchService) validateConditionPaths(ctx context.Context, modelStore spi.ModelStore, modelRef spi.ModelRef, cond predicate.Condition) (map[string]schema.FieldDescriptor, error) {
	paths := extractFieldPaths(cond)
	if len(paths) == 0 {
		// The model schema is a dependency of a condition exactly when the
		// condition addresses a DATA path. This one addresses none — it is
		// lifecycle-only — so there is nothing to validate and nothing the
		// caller needs the fields map for: a meta leaf takes its type from
		// the static meta vocabulary, not from the map (see
		// spi.ConditionToFilter, "Meta leaves are unaffected"). Loading the
		// schema here anyway would fail requests that are answerable, which
		// is availability spent for no correctness.
		return nil, nil
	}

	// Negative cache fast-path: if any path is recorded as confirmed
	// absent for this (tenant, modelRef) at the current generation,
	// short-circuit without touching the inner store. This collapses
	// a serial flood of bad requests into one inner-store round-trip
	// per (tenant, modelRef, path) tuple between schema events.
	tenant := common.TenantFromContext(ctx)
	if cachedMissing := s.cachedAbsentPaths(tenant, modelRef, surfaceCondition, paths); len(cachedMissing) > 0 {
		return nil, invalidPathError(cachedMissing)
	}

	fields, err := loadFieldsMap(ctx, modelStore, modelRef)
	if err != nil {
		// Fail closed. The schema is what decides whether this condition's
		// paths exist, and nothing downstream re-asks: the matcher has no
		// field-path check, so an unvalidated search answers an empty page
		// for a path that is wrong and for a path that simply matched
		// nothing, identically. Worse, translating against a nil fields map
		// stamps an empty Declared on every leaf, which annihilates the
		// eight comparison and ordering operators to a non-match while the
		// other eighteen keep evaluating (see spi.ConditionToFilter) — so
		// the result set is not merely unvalidated, it is short.
		//
		// Per .claude/rules/correctness-over-availability.md, a dependency a
		// correct result requires fails the operation rather than
		// downgrading it.
		return nil, common.Internal("failed to load model schema for condition validation", err)
	}
	// A nil fields map is NOT "nothing to validate against" — it is a model
	// declaring no fields, against which every data path the condition names
	// is unknown. findUnknownPaths reports exactly that, and the bounded
	// single-refresh retry below still gets its chance to discover a schema
	// this node has not yet seen. Returning early here accepted any path at
	// all on such a model.
	missing := findUnknownPaths(paths, fields)
	if len(missing) == 0 {
		s.markPathsPresent(tenant, modelRef, surfaceCondition, paths)
		return fields, nil
	}

	// Some paths are unknown to the cached schema. Refresh exactly once
	// before declaring the request invalid — the bound is required by
	// issue #77 to avoid amplifying a misconfigured client into a
	// refresh storm.
	freshFields, refreshed, refreshErr := refreshFieldsMap(ctx, modelStore, modelRef)
	if !refreshed {
		// Store has no cache layer — the cached miss is authoritative.
		s.markPathsAbsent(tenant, modelRef, surfaceCondition, missing)
		return nil, invalidPathError(missing)
	}
	if refreshErr != nil {
		if errors.Is(refreshErr, spi.ErrNotFound) {
			// Model was deleted between Get and RefreshAndGet — fall
			// back to the cached fields outcome (paths are unknown
			// because there is no model). Do NOT populate the negative
			// cache: there is no schema authority to invalidate against.
			return nil, invalidPathError(missing)
		}
		slog.Debug("schema refresh failed during pre-execution validation",
			"pkg", "search",
			"entityName", modelRef.EntityName,
			"modelVersion", modelRef.ModelVersion,
			"error", refreshErr)
		return nil, invalidPathError(missing)
	}
	if freshFields == nil {
		// The refresh produced no schema, so the descriptor carries none.
		// Deliberately NOT negative-cached, for the same reason as the
		// ErrNotFound branch above: the cache records "this path is absent
		// from a schema we read", and here there is no schema to have read
		// it from. The cost is a Get + RefreshAndGet per repeat request on
		// such a model. Unreachable through the model API — every Save
		// writes marshalled schema bytes, and an empty schema still yields a
		// non-nil fields map that takes the cached route above — so this is
		// a bound on an out-of-band or legacy row, not on anything a caller
		// can provoke.
		return nil, invalidPathError(missing)
	}

	stillMissing := findUnknownPaths(missing, freshFields)
	if len(stillMissing) == 0 {
		s.markPathsPresent(tenant, modelRef, surfaceCondition, paths)
		// The refresh is authoritative — hand back the schema the paths
		// actually validated against, not the stale one.
		return freshFields, nil
	}
	s.markPathsAbsent(tenant, modelRef, surfaceCondition, stillMissing)
	return nil, invalidPathError(stillMissing)
}

// pathSurface discriminates WHICH validation surface asked the negative
// cache about a path. The condition surface (validateConditionPaths) and
// the sort-key surface (resolveSortKeys) share one *PathValidationCache
// instance, but they do NOT agree on what "absent" means for the same
// spelling: a sort key must denote an exact scalar leaf, so resolveOrderBy
// (via findUnknownSortPaths) rejects a container path ("$.address" when
// only "$.address.street" is declared) or an array-container path
// ("$.tags" when the schema records only "$.tags[*]") that the CONDITION
// surface deliberately ACCEPTS (a bare path may legitimately address a
// container or array field for a condition — see isPathKnown /
// TestSearch_SimpleConditionOnContainerPath_NotNull_IsAccepted).
//
// Without a namespace, a rejected sort key on "$.tags" would call
// markPathsAbsent("$.tags"), and the very next legitimate condition on
// "$.tags" would hit cachedAbsentPaths and 400 — a valid search
// permanently broken by an unrelated sort request on a stable schema,
// until a schema change fires InvalidateRef or otter evicts. Namespacing
// the cache KEY (not the underlying otter cache/bucket — one instance,
// one bucket per (tenant, ref), namespaced keys within it) keeps each
// surface reading back only what it wrote. sort→sort and condition→sort
// need no isolation (resolveOrderBy can never reject a key
// findUnknownPaths would also reject, and condition→sort is a strict
// subset), so ONLY the sort surface needs its own namespace; the
// condition surface keeps the bare, unnamespaced spelling other callers
// (see FindUnknownFieldPaths) already reason about.
type pathSurface string

const (
	// surfaceCondition is validateConditionPaths' namespace — the bare
	// path spelling, unchanged from before this type existed.
	surfaceCondition pathSurface = ""
	// surfaceSort is resolveSortKeys' namespace.
	surfaceSort pathSurface = "sort"
)

// namespacedCacheKey prefixes path with surface's discriminator so the
// condition and sort surfaces can never read back an entry the other
// wrote. surfaceCondition's empty discriminator keeps its keys identical
// to the path itself — no behavior change for the surface that owned this
// cache before resolveSortKeys started using it.
func namespacedCacheKey(surface pathSurface, path string) string {
	if surface == surfaceCondition {
		return path
	}
	return string(surface) + "\x00" + path
}

// cachedAbsentPaths returns the subset of paths recorded as confirmed
// absent in the negative cache for (tenant, modelRef, surface) at the
// current generation. Returns nil when the cache is unset or no path
// matches. Returned paths keep the caller's own spelling — only the
// cache KEY is namespaced.
func (s *SearchService) cachedAbsentPaths(tenant string, ref spi.ModelRef, surface pathSurface, paths []string) []string {
	if s.pathCache == nil {
		return nil
	}
	var out []string
	for _, p := range paths {
		if s.pathCache.IsAbsent(tenant, ref, namespacedCacheKey(surface, p)) {
			out = append(out, p)
		}
	}
	return out
}

// markPathsAbsent records each path as confirmed absent for (tenant,
// modelRef, surface). No-op when the cache is unset.
func (s *SearchService) markPathsAbsent(tenant string, ref spi.ModelRef, surface pathSurface, paths []string) {
	if s.pathCache == nil {
		return
	}
	for _, p := range paths {
		s.pathCache.MarkAbsent(tenant, ref, namespacedCacheKey(surface, p))
	}
}

// markPathsPresent removes each path from the negative cache for
// (tenant, modelRef, surface). Defensive: ensures a path that previously
// resolved as absent and now resolves as present is reflected without
// waiting for an invalidation event. No-op when the cache is unset.
func (s *SearchService) markPathsPresent(tenant string, ref spi.ModelRef, surface pathSurface, paths []string) {
	if s.pathCache == nil {
		return
	}
	for _, p := range paths {
		s.pathCache.MarkPresent(tenant, ref, namespacedCacheKey(surface, p))
	}
}

// resolveSortKeys turns the request OrderKeys into typed OrderSpecs, validating
// scalar-leaf data paths and the meta allowlist. Returns a 400-classified
// AppError on bad input.
//
// A DATA sort key absent from the cached schema is refreshed exactly once
// before it is refused — mirroring validateConditionPaths' bounded-refresh
// contract (issue #77) for condition paths. Without this, a field a peer
// node had just added sorted successfully on the node that already saw the
// extension and 400'd on one still running the stale cache — the same field
// answering two ways depending only on which node's cache happened to be
// warm. The bound is required: an unbounded refresh turns a misconfigured
// client naming a field that will never exist into a refresh storm.
//
// The refresh bound is per-REQUEST on its own — one RefreshAndGet per call —
// which is not enough: a repeated bogus sort key would still pay one
// RefreshAndGet (an authoritative model-store read plus a full schema
// re-parse, and RefreshAndGet repopulates the shared model-descriptor cache,
// pushing the cost onto legitimate concurrent readers of the same model)
// PER REQUEST, indefinitely. So this routes through the same negative cache
// validateConditionPaths uses — s.cachedAbsentPaths / s.markPathsAbsent /
// s.markPathsPresent — bounding it per (tenant, model, path) the way the
// condition-path equivalent already is.
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

	// Negative cache fast-path, mirroring validateConditionPaths: if every
	// DATA sort key this request names is already recorded absent for this
	// (tenant, modelRef) at the current generation, refuse without touching
	// the model store at all. Only SourceData keys are candidates — a META
	// key's outcome never depends on the model schema, so it is never
	// negative-cached and never gates this fast path.
	tenant := common.TenantFromContext(ctx)
	dataPaths := normalisedDataSortPaths(keys)
	if cachedMissing := s.cachedAbsentPaths(tenant, modelRef, surfaceSort, dataPaths); len(cachedMissing) > 0 {
		return nil, unknownSortFieldError(cachedMissing)
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
	if rerr == nil {
		s.markPathsPresent(tenant, modelRef, surfaceSort, dataPaths)
		return specs, nil
	}
	if !errors.Is(rerr, errUnknownSortField) {
		// Grammar, an unknown META field, an array field or an unresolvable
		// sort kind — none of these can change on refresh, and none of them
		// is schema-path absence, so none of them is negative-cached.
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidFieldPath, rerr.Error())
	}

	// The exact missing subset, independent of resolveOrderBy's
	// first-error short-circuit. findUnknownSortPaths — not
	// validateConditionPaths' findUnknownPaths — applies resolveOrderBy's
	// own exact-key membership test: a sort key must denote a single
	// scalar leaf, so the CONDITION-path predicate's container/array
	// widening (correct there, where a bare path may legitimately address
	// a container) would silently under-report what is actually missing
	// here and leave the negative cache never engaging for those shapes.
	missing := findUnknownSortPaths(dataPaths, fields)

	freshFields, refreshed, refreshErr := refreshFieldsMap(ctx, modelStore, modelRef)
	if !refreshed {
		// Store has no cache layer — the cached miss is authoritative,
		// exactly the validateConditionPaths case of the same name.
		s.markPathsAbsent(tenant, modelRef, surfaceSort, missing)
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidFieldPath, rerr.Error())
	}
	if refreshErr != nil || freshFields == nil {
		// The refresh itself failed, or it produced no schema. Deliberately
		// NOT negative-cached, same as validateConditionPaths: there is no
		// schema authority to invalidate this entry against later.
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidFieldPath, rerr.Error())
	}
	specs, rerr = resolveOrderBy(keys, freshFields)
	if rerr != nil {
		if errors.Is(rerr, errUnknownSortField) {
			s.markPathsAbsent(tenant, modelRef, surfaceSort, findUnknownSortPaths(missing, freshFields))
		}
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidFieldPath, rerr.Error())
	}
	s.markPathsPresent(tenant, modelRef, surfaceSort, dataPaths)
	return specs, nil
}

// normalisedDataSortPaths returns the deduplicated, canonicalised
// (spi.NormalisePath) paths of the DATA sort keys in keys — the subset the
// negative cache applies to. A META key carries no data-schema dependency
// and is never included.
func normalisedDataSortPaths(keys []OrderKey) []string {
	seen := make(map[string]struct{}, len(keys))
	var out []string
	for _, k := range keys {
		if k.Source == spi.SourceMeta {
			continue
		}
		p := normalisePath(k.Path)
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// unknownSortFieldError builds the 4xx response for one or more sort-key
// paths the negative cache already knows to be absent from the model schema.
func unknownSortFieldError(paths []string) error {
	return common.Operational(
		http.StatusBadRequest,
		common.ErrCodeInvalidFieldPath,
		fmt.Sprintf("unknown sort field(s): %s", strings.Join(paths, ", ")),
	)
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
