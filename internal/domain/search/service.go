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
// the backend's search statement ceiling. Fixed and non-revealing: GetJob serves
// this string straight back, so a raw driver error here would put SQL, a
// SQLSTATE and connection detail in a caller-facing record. Which ceiling and
// which setting stays in the log.
const searchCeilingMessage = "search exceeded the search statement ceiling"

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
// The record is the only report a caller of the async API ever gets, and GetJob
// serves it back verbatim, so it follows the same 4xx/5xx split as every other
// response: a classified client error carries its own already-safe text, and
// everything else — a storage failure, a driver error, an unclassified
// wrapper — collapses to a fixed string with the detail left in the log.
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

	// registryMu guards registry, the jobID -> in-process cancel handle map
	// used by CancelRunning (in-process immediate cancel) and
	// AbortRegisteredJobs (shutdown drain). An entry exists for the
	// lifetime of a job on this node: from registerJob at submit time
	// (queued or executing) to deregisterJob in the executor's own defer.
	registryMu sync.Mutex
	registry   map[string]*asyncJobHandle
}

// asyncJobHandle is what the cancel registry keeps per in-flight (queued or
// executing) job on this node.
type asyncJobHandle struct {
	// cancel cancels the job's own context (jobCtx), which is what the
	// heartbeat ticker and the executor's scan/save loop both observe —
	// NOT the worker pool's lifetime context (see WithAsyncPool's doc
	// comment on why the two are independent).
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
func (s *SearchService) registerJob(jobID string, cancel context.CancelFunc, uc *spi.UserContext) {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	if s.registry == nil {
		s.registry = make(map[string]*asyncJobHandle)
	}
	s.registry[jobID] = &asyncJobHandle{cancel: cancel, uc: uc}
}

// deregisterJob removes jobID's entry. Idempotent — a missing entry is a
// no-op, so both the queue-full submit path and the executor's own defer can
// call it without coordinating who runs first.
func (s *SearchService) deregisterJob(jobID string) {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	delete(s.registry, jobID)
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
// or executing) and marks each FAILED via an epoch-1 fenced write carrying
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
// Search/SubmitAsync boundary: an object-operand shape violation
// (ErrInvalidCondition, spec §6/§8) maps to INVALID_CONDITION; every other
// structural failure (unknown operatorType, malformed BETWEEN arity) keeps
// the existing BAD_REQUEST classification these two entry points have
// always used.
func structuralConditionErrCode(cErr error) string {
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

	if vErr := s.validateConditionPaths(ctx, modelRef, cond); vErr != nil {
		return nil, vErr
	}
	if rErr := ValidateRegexPatterns(cond); rErr != nil {
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition,
			fmt.Sprintf("invalid regex pattern in condition: %v", rErr))
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

	// Delegate to the plugin Searcher whenever it's available. Searcher.Search
	// is transaction-aware on every OSS backend (RYW), so this is safe with or
	// without an active transaction in ctx — see the Search doc comment.
	// opts.Limit <= 0 ("no explicit limit" — async submit's internal caller,
	// or a scoped conditional delete's Limit:-1) cannot be pushed down:
	// Searcher.Search's contract requires Limit >= 1 and treats anything
	// else as a caller error, not "unbounded" — there is no zero-means-
	// unbounded sentinel at that interface. The engine is the one place
	// that resolves a bound before calling it (see the Searcher doc
	// comment), so an unbounded request skips the pushdown attempt
	// entirely (including its FieldsMap load — the fallback below loads
	// its own) and takes the same GetAll + in-memory-match fallback a
	// translate failure does, which already tolerates opts.Limit <= 0 (see
	// the bounded-or-fail comment below).
	if searcher, ok := store.(spi.Searcher); ok && opts.Limit > 0 {
		fields, _ := loadFieldsMap(ctx, modelStore, modelRef) // best-effort; nil-tolerant
		filter, translateErr := spi.ConditionToFilter(cond, fields)
		if translateErr == nil {
			res, sErr := searcher.Search(ctx, filter, spi.SearchOptions{
				ModelName:    modelRef.EntityName,
				ModelVersion: modelRef.ModelVersion,
				PointInTime:  opts.PointInTime,
				Limit:        opts.Limit,
				OrderBy:      orderBy,
				TrackingRead: opts.TrackingRead,
			})
			switch {
			case errors.Is(sErr, spi.ErrSearchResultLimitExceeded):
				return nil, common.Operational(http.StatusBadRequest,
					common.ErrCodeSearchResultLimit,
					"matched result count exceeds the configured limit").WithCause(sErr)
			case errors.Is(sErr, spi.ErrScanBudgetExhausted):
				return nil, common.Operational(http.StatusBadRequest,
					common.ErrCodeScanBudgetExhausted,
					"search scan budget exhausted; narrow the query or add an indexable predicate").WithCause(sErr)
			}
			return res, sErr
		}
		// Fall through to in-memory filtering if translation fails.
		slog.Debug("condition-to-filter translation failed, falling back to in-memory",
			"pkg", "search", "error", translateErr)
	}

	// Fallback: GetAll/GetAllAsAt + in-memory filtering. In-tx, this path is a
	// rare edge (a store without Searcher, or a translate-failure condition):
	// GetAll unconditionally records every returned entity into the
	// transaction's read-set (unlike the Searcher's TrackingRead-gated
	// pushdown path above), so a translate-failure search conservatively
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
		// returns nil types, and comparison leaves degrade to non-match as intended.
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

	if vErr := s.validateConditionPaths(ctx, modelRef, cond); vErr != nil {
		return "", vErr
	}
	if rErr := ValidateRegexPatterns(cond); rErr != nil {
		return "", common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition,
			fmt.Sprintf("invalid regex pattern in condition: %v", rErr))
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

	// jobCtx (not the pool's own lifetime ctx — see WithAsyncPool) is what
	// the heartbeat ticker and the executor's scan/save loop observe.
	// Registered — and the heartbeat ticker started — before the job is
	// handed to the pool: the submitter owns the queue entry, so both span
	// the queued state, not just execution.
	jobCtx, cancel := context.WithCancel(bgCtx)
	s.registerJob(jobID, cancel, uc)
	s.startHeartbeat(jobCtx, cancel, jobID)

	submitErr := s.asyncPool().Submit(func(context.Context) {
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
				if err := s.searchStore.Heartbeat(jobCtx, jobID, 1); err != nil {
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
// result set first, and records a single epoch-1-fenced terminal write.
// cancel stops the heartbeat ticker (via jobCtx) on every exit path.
func (s *SearchService) runAsyncJob(jobCtx context.Context, cancel context.CancelFunc, jobID string, modelRef spi.ModelRef, cond predicate.Condition, opts SearchOptions, resolvedOrderBy []spi.OrderSpec) {
	defer cancel()
	defer s.deregisterJob(jobID)

	const epoch = 1
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
			// context.WithoutCancel: jobCtx may or may not be cancelled at
			// this point, but the write must land regardless — stripping
			// cancellation while keeping the UserContext value lets a store
			// that aborts in-flight work on ctx.Err() still accept it.
			s.writeAsyncFailure(context.WithoutCancel(jobCtx), jobID, jobFailureFallback, time.Now(), 0)
		}
	}()

	modelStore, err := s.factory.ModelStore(jobCtx)
	if err != nil {
		s.writeAsyncFailure(jobCtx, jobID, jobFailureMessage(err), time.Now(), time.Since(start).Milliseconds())
		return
	}
	fields, _ := loadFieldsMap(jobCtx, modelStore, modelRef) // best-effort; nil-tolerant, mirrors Search
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
			saveErr = s.searchStore.SaveResults(jobCtx, jobID, epoch, slices.Values(ids))
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
			prodErr = iterErr
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
				defer func() {
					if closeErr := it.Close(); closeErr != nil {
						slog.Warn("failed to close async search iterator", "pkg", "search", "jobID", jobID, "err", closeErr)
					}
					pErr = it.Err()
				}()
				seq := func(yield func(string) bool) {
					for it.Next() {
						n++
						if !yield(it.Entity().Meta.ID) {
							return
						}
					}
				}
				sErr = s.searchStore.SaveResults(jobCtx, jobID, epoch, seq)
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
		// fencing failure. context.WithoutCancel so the recovery read/write
		// below is not itself aborted by the same cancellation.
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

	if err := s.searchStore.UpdateJobStatus(jobCtx, jobID, epoch, "SUCCESSFUL", count, "", finishTime, calcTimeMs); err != nil {
		if errors.Is(err, spi.ErrAlreadyTerminal) || errors.Is(err, spi.ErrStaleClaim) {
			slog.Warn("async search terminal write lost the race; state already settled", "pkg", "search", "jobID", jobID, "err", err)
			return
		}
		slog.Error("failed to update search job status", "pkg", "search", "jobID", jobID, "err", err)
	}
}

// writeAsyncFailure records jobID FAILED via an epoch-1 fenced write. A lost
// race against the job's own (or a takeover's) terminal write
// (ErrAlreadyTerminal/ErrStaleClaim) is expected and logged at Warn, not
// treated as a failure of the caller — the correct state is already
// recorded.
func (s *SearchService) writeAsyncFailure(ctx context.Context, jobID, msg string, finishTime time.Time, calcTimeMs int64) {
	if err := s.searchStore.UpdateJobStatus(ctx, jobID, 1, "FAILED", 0, msg, finishTime, calcTimeMs); err != nil {
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
