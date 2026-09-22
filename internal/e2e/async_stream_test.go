package e2e_test

// async_stream_test.go — isolated single-backend (postgres) e2e coverage for
// the search-SPI-surface milestone's async-search executor: requested-
// order results across pages, worker-pool queue-full backpressure, shutdown
// drain, and the stale-job reaper. These are process-local/timing-sensitive
// scenarios and so belong here, NOT in the shared e2e/parity suite (see
// .claude/rules/test-coverage.md: "Concurrency/race: isolated single-backend
// e2e, never the shared parity suite"). Cancel-mid-flight and cross-node
// cancel live in the sibling async_cancel_multinode_test.go.
//
// --- Design §9 coverage-matrix reconciliation (task E7.4) -----------------
//
// Every row of docs/superpowers/specs/2026-08-22-472-search-spi-surface-design.md
// §9, with the test(s) that fill each non-dash cell. "spitest" cells live in
// the cyoda-go-spi repo's conformance suite, out of this repo's scope.
//
//  1. Requested-order Iterate (tie-break/residual/ctx-cancel): spitest;
//     parity implied by spitest running on every backend (design note).
//  2. Overlay snapshot-at-open / TrackingRead gating: spitest only.
//  3. Terminal statuses write-once (ErrAlreadyTerminal): spitest only.
//  4. Search rejects Limit<=0: spitest; e2e TestSearchDirect_LimitZero_Returns400
//     (internal/e2e/search_bounded_test.go); gRPC TestDirectSearch_NonPositiveLimit_ClientError
//     (internal/grpc/search_test.go).
//  5. Streamed SaveResults (order/chunk-seq/ctx-abort): spitest only.
//  6. Async results incremental; heap O(batch): engine
//     TestExecutor_StreamsIncrementally (internal/domain/search/executor_test.go)
//     — WAIVER (verbatim, sanctioned by the E7 brief): "O(batch) heap
//     asserted structurally via the E2.1 interleave fake, not via
//     allocation counters." TestExecutor_StreamsIncrementally's
//     countingIterator + streamObserverStore prove the SaveResults consumer
//     interleaves with the Iterate producer rather than draining a
//     materialized slice — the same structural property a heap bound relies
//     on, without instrumenting allocations directly.
//  7. Cancel stops scan mid-flight; cross-node on postgres: engine registry
//     TestExecutor_CancelMidFlight (internal/domain/search/executor_test.go);
//     e2e (isolated, not parity) TestE2E_AsyncSearch_CancelMidFlight and
//     TestE2E_AsyncSearch_CrossNodeCancel (async_cancel_multinode_test.go);
//     gRPC cancel envelope TestEntitySearch_SnapshotCancel_Envelope
//     (internal/grpc/search_test.go).
//  8. Heartbeat + ClaimStale orphan RE-EXECUTION: spitest + engine unit
//     (TestReclaimStaleJobs_* in internal/domain/search/reaper_test.go,
//     TestExecutor_HeartbeatRecordedWhileQueuedAndScanning /
//     TestExecutor_HeartbeatFencingAborts in executor_test.go); e2e
//     TestE2E_AsyncSearch_OrphanReExecuted (this file) — synthesises the
//     owner-is-gone shape (a RUNNING row with no executor behind it,
//     created_at backdated past SearchJobStaleAfter, real entities seeded
//     behind its model) and asserts app.New's wired reaper ticker (app.go's
//     stopSearchReaper loop calling ReclaimStaleJobs) CLAIMS, ClearResults,
//     re-enqueues, and completes it SUCCESSFUL with the direct-search result
//     count — a crashed node's job is now finished, not failed;
//     TestE2E_AsyncSearch_AttemptCap_Fails (this file) — a job whose
//     stale_claims has reached SearchJobMaxAttempts is FAILED with the
//     "search abandoned: executor lost repeatedly" message, the one path that
//     still fails an orphan; TestE2E_AsyncSearch_CrashMidScan_PeerCompletes /
//     _CrashMidSave_PartialCleared / _SingleNodeRestart_Reclaims (this file)
//     — a peer or restarted node completes a crashed node's job, with
//     ClearResults wiping a committed partial page so the final set has no
//     duplicates; TestE2E_AsyncSearch_DeposedExecutorFenced
//     (async_cancel_multinode_test.go) — a peer legitimately reclaims a
//     slow-heartbeating live executor and completes it, and the deposed node's
//     resumed write is fenced (single author). And the inverse guarantee
//     TestE2E_AsyncSearch_StaleJobReaper_SparesLiveExecutor (this file) — a job
//     blocked inside Iterate for longer than SearchJobStaleAfter is NOT
//     claimed, because its executor's heartbeat ticker runs independent of scan
//     progress. All run under a production-valid cadence (staleAfter == the
//     enforced 4x floor, or a wider bound).
//  9. Epoch fencing (stale-epoch Heartbeat/SaveResults/UpdateJobStatus
//     refused; ClearResults idempotent): spitest only.
// 10. Shutdown drain then release-for-reclaim: engine
//     ReleaseRegisteredJobs is exercised by App.Shutdown itself; e2e
//     TestE2E_AsyncSearch_ShutdownReleases_NoFailedWrite (this file) — after
//     Shutdown the in-flight job is RUNNING (released), never FAILED, and a
//     peer App with a live backend and a 1h stale bound still reclaims it via
//     the released flag (proving Release, not staleness, drove the handoff)
//     and completes it SUCCESSFUL.
// 11. Worker pool (<=poolSize concurrent, excess queue): engine
//     TestWorkerPool_ConcurrencyBound / TestWorkerPool_BoundedQueue_QueueFull
//     (internal/domain/search/pool_test.go); isolated e2e
//     TestE2E_AsyncSearch_QueueFull_503 (this file) — real HTTP submit
//     against WORKERS=1/QUEUE=1 and a store whose Iterate blocks, asserting
//     the third submit gets HTTP 503 SEARCH_QUEUE_FULL end-to-end.
// 12. GetResultIDs degenerate inputs (no panic): spitest only.
// 13. GetPage ordering/limit/offset; fail-fast: spitest; unit (NOT a running
//     backend — memory.NewStoreFactory in-process)
//     TestListEntities_PagesViaGetPage / TestListEntities_PagePastEnd_ReturnsEmpty
//     (internal/domain/entity/service_list_test.go); running-backend e2e
//     TestListIntxReadSet_* (internal/e2e/list_intx_readset_test.go — real
//     postgres, pins GetPage's read-set FOOTPRINT: only the returned page
//     enters the transaction's conflict read-set, the behaviour change the
//     CHANGELOG advertises and design §5 states); parity
//     ListEntitiesPagingConsistency (e2e/parity/list_paging.go, extended by
//     task E7 with the offset-past-end assertion sqlite/postgres lacked) —
//     the paging/ordering cell's genuine running-backend coverage;
//     gRPC TestRPC_EntityGetAll / TestRPC_EntityGetAll_Page*ExceedsCap
//     (internal/grpc/rpc_test.go, search_pagination_test.go).
// 14. ?pageSize=1 latency bounded by page, not N (query-shape assert): e2e —
//     WAIVER (verbatim, sanctioned by the E7 brief): "query-shape asserts
//     implemented as plugin-level EXPLAIN tests (Q3/P3), not HTTP e2e —
//     layer shift, same guarantee." Already present:
//     TestGetPage_NonTx_UsesModelIDIndex (plugins/sqlite/entity_page_plan_test.go)
//     and TestGetPage_NonTx_UsesModelEntityIDIndex (plugins/postgres/entity_page_plan_test.go).
// 15. GetVersionByTransaction earliest-wins / empty txID rejected / 404:
//     spitest — the sole home of earliest-wins and of empty-txID rejection.
//     ?transactionId= never reaches the store as "": genapi types the
//     query parameter *openapi_types.UUID (api/generated.go,
//     GetOneEntityParams), so an empty value fails parameter binding and
//     the handler never runs.
//     Earliest-wins needs two versions sharing one transaction id, which
//     spitest arranges at the store; e2e
//     TestEntityLifecycle_TemporalByTransactionID
//     (internal/e2e/entity_lifecycle_test.go) covers the wire half only —
//     200 for the create transaction's id, 404 for an unknown one; parity
//     HistoryReadsChangesMetadataAndTransactionLookup (e2e/parity/history_reads.go)
//     adds per-transaction resolution with no cross-transaction bleed and
//     404 on the tombstone's transaction id; no gRPC surface (design note).
// 16. GetVersionByTransaction pushdown latency (query-shape assert): e2e —
//     same WAIVER as row 14. Already present:
//     TestGetVersionByTransaction_StaysWithinEntityVersions
//     (plugins/sqlite/entity_page_plan_test.go) and
//     TestGetVersionByTransaction_StaysWithinEntityVersionsPK
//     (plugins/postgres/entity_page_plan_test.go).
// 17. GetVersionMetadata window/limit/order; Deleted canonical: spitest
//     (sole home of the Version DESC tie-break — no wire payload carries a
//     version, so no transport-level test can observe a tie); e2e
//     TestGetEntityChanges_NewestFirst (internal/e2e/entity_changes_order_test.go);
//     parity HistoryReadsChangesMetadataAndTransactionLookup
//     (e2e/parity/history_reads.go — newest-first change-type sequence plus
//     the tombstone's omitted transactionId); gRPC TestRPC_EntityChangesMetadata
//     (internal/grpc/rpc_test.go — create+update+delete over the gRPC door,
//     asserting newest-first order and canonical Deleted, i.e. DELETE
//     changeType with transactionId omitted).
// 18. Conditional delete over large model (O(IDs) atomic / O(page) batched):
//     engine TestDeleteEntitiesConditional_Batched_* (internal/domain/entity/service_delete_batched_test.go);
//     e2e TestTransactionControl_DeleteEntities_BatchedHappyPath
//     (internal/e2e/transaction_control_test.go); parity
//     EntityConditionalDeleteInTx (e2e/parity/entity_slice.go).
// 19. Async result ordering respected end-to-end: e2e
//     TestE2E_AsyncSearch_OrderedAcrossPages (this file — also the ONLY home
//     of the byte-wise entity-id tie-break assertion, which is a per-engine
//     contract, not a cross-engine one); parity AsyncOrderingRespected
//     (e2e/parity/async_ordering.go, task E7.2 — asserts the requested
//     user-field key order, set equality, no duplicates, and repeat-run
//     order stability; deliberately NOT the byte-wise tie-break, which a
//     conforming timeuuid-ordered backend would fail); gRPC
//     TestEntitySearch_SnapshotSearch_OrderBy_ResultsOrdered
//     (internal/grpc/search_test.go — submit -> poll -> SnapshotGetRequest,
//     asserting the streamed result order asc AND desc; the sibling
//     _OrderBy_SourceMeta / _OrderBy_ValidField tests assert only that the
//     orderBy payload is ACCEPTED and are not the ordering cell).
//
// No row has a silently-missing cell as of this task.
// -----------------------------------------------------------------------

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/app"
)

// ---------------------------------------------------------------------------
// Blocking-Iterate test backend
//
// app.Config has no storage-factory injection point (only a StorageBackend
// name resolved via spi.GetPlugin), so a deterministic "a store whose
// Iterate blocks" (the brief's phrase for the queue-full/shutdown-drain
// scenarios) requires a real, if test-only, spi.Plugin: it wraps the real
// "postgres" plugin's factory and gates EntityStore.Iterate open/closed via
// a channel the test controls. Registered under a name unique to each
// caller (spi.Register panics on a name collision), so tests never share
// gate state even when they run in the same package binary.
// ---------------------------------------------------------------------------

// iterateGate lets a test hold a store's Iterate call open (blocked) until
// released, and observe both when a call actually entered the block and
// when it resumed (released, or its ctx was cancelled — whichever first).
type iterateGate struct {
	mu      sync.Mutex
	blocked bool
	entered chan struct{}
	release chan struct{}
	resumed chan struct{}
	once    *sync.Once
}

// Block arms the gate: the next call(s) to wait will block until Release or
// ctx cancellation. Must be called before the Iterate call it is meant to
// catch.
func (g *iterateGate) Block() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.blocked = true
	g.entered = make(chan struct{})
	g.release = make(chan struct{})
	g.resumed = make(chan struct{})
	g.once = &sync.Once{}
}

// Release lets a blocked wait call proceed. Idempotent no-op if not armed.
func (g *iterateGate) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blocked {
		close(g.release)
		g.blocked = false
	}
}

// Entered returns a channel closed the moment a call starts waiting on the
// gate — proof the worker actually reached the blocked Iterate call, not
// merely that the job was submitted/queued.
func (g *iterateGate) Entered() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.entered
}

// Resumed returns a channel closed the first time a blocked call proceeds
// (Release or ctx cancellation) since the most recent Block.
func (g *iterateGate) Resumed() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.resumed
}

func (g *iterateGate) wait(ctx context.Context) {
	blocked, enteredCh, releaseCh, resumedCh, once := func() (bool, chan struct{}, chan struct{}, chan struct{}, *sync.Once) {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.blocked, g.entered, g.release, g.resumed, g.once
	}()
	if !blocked {
		return
	}
	select {
	case <-enteredCh:
	default:
		close(enteredCh)
	}
	select {
	case <-releaseCh:
	case <-ctx.Done():
	}
	if once != nil {
		once.Do(func() { close(resumedCh) })
	}
}

// blockingIterateStore wraps a real spi.EntityStore, gating Iterate on the
// given gate before delegating to the real implementation.
type blockingIterateStore struct {
	spi.EntityStore
	gate *iterateGate
}

func (s *blockingIterateStore) Iterate(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
	s.gate.wait(ctx)
	return s.EntityStore.Iterate(ctx, model, filter, opts)
}

type blockingIterateFactory struct {
	spi.StoreFactory
	gate *iterateGate
}

func (f *blockingIterateFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	real, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return &blockingIterateStore{EntityStore: real, gate: f.gate}, nil
}

type blockingIteratePlugin struct {
	name  string
	inner spi.Plugin
	gate  *iterateGate
}

func (p *blockingIteratePlugin) Name() string { return p.name }

func (p *blockingIteratePlugin) NewFactory(ctx context.Context, getenv func(string) string, opts ...spi.FactoryOption) (spi.StoreFactory, error) {
	f, err := p.inner.NewFactory(ctx, getenv, opts...)
	if err != nil {
		return nil, err
	}
	return &blockingIterateFactory{StoreFactory: f, gate: p.gate}, nil
}

// newBlockingIterateBackend registers a fresh postgres-backed plugin (same
// live CYODA_POSTGRES_URL TestMain set) under a name unique to this call,
// whose EntityStore.Iterate blocks whenever the returned gate is armed.
// Returns the backend name to set as cfg.StorageBackend and the gate.
func newBlockingIterateBackend(t *testing.T) (backendName string, gate *iterateGate) {
	t.Helper()
	inner, ok := spi.GetPlugin("postgres")
	if !ok {
		t.Fatal("postgres plugin not registered")
	}
	gate = &iterateGate{}
	name := "postgres-blocking-iterate-" + uuid.NewString()
	spi.Register(&blockingIteratePlugin{name: name, inner: inner, gate: gate})
	return name, gate
}

// ---------------------------------------------------------------------------
// (a) requested-order results across pages
// ---------------------------------------------------------------------------

// TestE2E_AsyncSearch_OrderedAcrossPages submits an async search with an
// explicit sort key over a model with repeated values (forcing entity-ID
// tie-breaks), waits for SUCCESSFUL, and walks every result page asserting:
// ascending "amount", ascending entity-id within a tied "amount", and
// totalPages/totalElements arithmetic consistent with pageSize.
//
// This is the ONLY home for the byte-wise entity-id tie-break assertion.
// Canonical entity-ID order is per-engine (postgres pins it to COLLATE "C"
// via getPageCurrent; the commercial backend's is native timeuuid order —
// docs/cloud-parity/2026-08-22-async-ordering-and-list-order.md §2), so the
// cross-backend parity scenario (e2e/parity/async_ordering.go) deliberately
// asserts only the engine-independent part. Byte-wise ordering belongs
// here, scoped to the one engine that contracts for it.
func TestE2E_AsyncSearch_OrderedAcrossPages(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "async-ordering-e2e"
	h.setupModelSampleWithWorkflow(t, model, `{"name":"seed","amount":0,"status":"new"}`, secondaryWorkflow)

	const total = 9
	for i := 0; i < total; i++ {
		amount := i % 3 // ties: 0,1,2,0,1,2,0,1,2
		payload := fmt.Sprintf(`{"name":"e%d","amount":%d,"status":"new"}`, i, amount)
		if _, status, body := h.CreateEntity(t, model, 1, payload); status != http.StatusOK {
			t.Fatalf("create %d: %d %s", i, status, body)
		}
	}

	resp := h.DoAuth(t, http.MethodPost, "/api/search/async/"+model+"/1?sort=amount",
		`{"type":"group","operator":"AND","conditions":[]}`, "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit: %d %s", resp.StatusCode, body)
	}
	jobID := strings.Trim(strings.TrimSpace(body), `"`)

	if status := h.waitForAsyncTerminal(t, jobID, 20*time.Second); status != "SUCCESSFUL" {
		t.Fatalf("job settled %s, want SUCCESSFUL", status)
	}

	const pageSize = 4
	var amounts []float64
	var ids []string
	var totalElements, totalPages int
	for page := 0; ; page++ {
		r := h.DoAuth(t, http.MethodGet,
			fmt.Sprintf("/api/search/async/%s?pageSize=%d&pageNumber=%d", jobID, pageSize, page), "", "")
		b := h.readBody(t, r)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("results page %d: %d %s", page, r.StatusCode, b)
		}
		var parsed struct {
			Content []map[string]any `json:"content"`
			Page    struct {
				TotalElements int `json:"totalElements"`
				TotalPages    int `json:"totalPages"`
			} `json:"page"`
		}
		if err := json.Unmarshal([]byte(b), &parsed); err != nil {
			t.Fatalf("decode results page %d: %v; body=%s", page, err, b)
		}
		totalElements = parsed.Page.TotalElements
		totalPages = parsed.Page.TotalPages
		if len(parsed.Content) == 0 {
			break
		}
		for _, e := range parsed.Content {
			data, _ := e["data"].(map[string]any)
			meta, _ := e["meta"].(map[string]any)
			amounts = append(amounts, data["amount"].(float64))
			ids = append(ids, meta["id"].(string))
		}
		if page+1 >= parsed.Page.TotalPages {
			break
		}
	}

	if totalElements != total {
		t.Fatalf("totalElements = %d, want %d", totalElements, total)
	}
	wantPages := (total + pageSize - 1) / pageSize
	if totalPages != wantPages {
		t.Fatalf("totalPages = %d, want %d", totalPages, wantPages)
	}
	if len(amounts) != total {
		t.Fatalf("collected %d results across pages, want %d", len(amounts), total)
	}
	ties := 0
	for i := 1; i < len(amounts); i++ {
		if amounts[i] < amounts[i-1] {
			t.Fatalf("amount not ascending at index %d: %v then %v", i, amounts[i-1], amounts[i])
		}
		if amounts[i] == amounts[i-1] {
			ties++
			if ids[i] <= ids[i-1] {
				t.Fatalf("tie-break not ascending id at index %d: %s then %s (amount=%v)", i, ids[i-1], ids[i], amounts[i])
			}
		}
	}
	// The seeding (amount = i%3 over 9 entities) guarantees 3 groups of 3,
	// i.e. 6 adjacent tied pairs. Assert the tie-break assertion above was
	// actually exercised — otherwise a change to the seeding could silently
	// make it vacuous.
	if wantTies := total - 3; ties != wantTies {
		t.Fatalf("observed %d adjacent tied pairs, want %d — the tie-break assertion was not exercised as intended (amounts=%v)",
			ties, wantTies, amounts)
	}
}

// ---------------------------------------------------------------------------
// (d) queue-full backpressure
// ---------------------------------------------------------------------------

// TestE2E_AsyncSearch_QueueFull_503 configures WORKERS=1/QUEUE=1 against a
// backend whose Iterate blocks: the first submit occupies the sole worker,
// the second fills the queue, and the third — real HTTP, real pool — must
// get 503 SEARCH_QUEUE_FULL.
func TestE2E_AsyncSearch_QueueFull_503(t *testing.T) {
	backend, gate := newBlockingIterateBackend(t)
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		cfg.StorageBackend = backend
		cfg.SearchAsync = app.SearchAsyncConfig{Workers: 1, QueueLen: 1}
	})

	const model = "queuefull-e2e"
	h.setupModelSampleWithWorkflow(t, model, `{"name":"Alice","amount":1,"status":"new"}`, secondaryWorkflow)
	if _, status, body := h.CreateEntity(t, model, 1, `{"name":"Alice","amount":1,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("seed: %d %s", status, body)
	}

	gate.Block()
	defer gate.Release()

	submitOne := func() (int, string) {
		resp := h.DoAuth(t, http.MethodPost, "/api/search/async/"+model+"/1",
			`{"type":"group","operator":"AND","conditions":[]}`, "")
		return resp.StatusCode, h.readBody(t, resp)
	}

	// First submit: the sole worker picks it up and blocks in Iterate.
	if status, body := submitOne(); status != http.StatusOK {
		t.Fatalf("first submit: %d %s", status, body)
	}
	select {
	case <-gate.Entered():
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached the blocked Iterate call")
	}

	// Second submit: fills the queue (capacity 1).
	if status, body := submitOne(); status != http.StatusOK {
		t.Fatalf("second submit (should fill the queue): %d %s", status, body)
	}

	// Third submit: worker busy, queue full -> 503 SEARCH_QUEUE_FULL.
	status, body := submitOne()
	if status != http.StatusServiceUnavailable {
		t.Fatalf("third submit status = %d, want 503; body=%s", status, body)
	}
	var pd struct {
		Detail string         `json:"detail"`
		Props  map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("problem detail is not JSON: %v; body=%s", err, body)
	}
	if code, _ := pd.Props["errorCode"].(string); code != "SEARCH_QUEUE_FULL" {
		t.Errorf("errorCode = %q, want SEARCH_QUEUE_FULL; body=%s", code, body)
	}
	if retryable, _ := pd.Props["retryable"].(bool); !retryable {
		t.Errorf("SEARCH_QUEUE_FULL not advertised retryable; body=%s", body)
	}
}

// ---------------------------------------------------------------------------
// (e) shutdown-release, crash, and restart reclaim
// ---------------------------------------------------------------------------

// seedStandalone imports+locks a model on the standalone app and creates n
// committed entities behind it, so a search over the model has results.
func seedStandalone(t *testing.T, a *standaloneApp, model string, n int) {
	t.Helper()
	setupSimpleModelWorkflow(t, a.doAuth, model)
	for i := 0; i < n; i++ {
		if resp := a.doAuth(http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model),
			fmt.Sprintf(`{"name":"e%d","amount":%d,"status":"new"}`, i, i)); resp.StatusCode != http.StatusOK {
			t.Fatalf("seed %d: %d %s", i, resp.StatusCode, readHTTPBody(t, resp))
		}
	}
}

// TestE2E_AsyncSearch_ShutdownReleases_NoFailedWrite submits over a
// blocking-Iterate backend, calls App.Shutdown, and asserts the in-flight job
// is RUNNING (released for reclaim) — NOT the pre-reclaim FAILED disposition.
// A peer with a live backend and a 1h stale bound then reclaims it via the
// released flag within one heartbeat interval: staleness alone would not fire
// for an hour, so a completion proves Release, not staleness, drove the handoff.
func TestE2E_AsyncSearch_ShutdownReleases_NoFailedWrite(t *testing.T) {
	backend, gate := newBlockingIterateBackend(t)
	a := newStandaloneApp(t, func(cfg *app.Config) {
		cfg.StorageBackend = backend
	})

	// Repeated runs (go test -count=N) share this package's Postgres
	// testcontainer, so a fixed model name meets itself as MODEL_ALREADY_LOCKED.
	model := "shutdown-release-e2e-" + randSuffix(t)
	const seeded = 4
	seedStandalone(t, a, model, seeded)

	gate.Block()
	defer gate.Release()

	jobID := a.submitAsync(t, model)
	select {
	case <-gate.Entered():
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached the blocked Iterate call")
	}

	// Shutdown's pool.Drain waits its budget for the permanently-blocked
	// worker, times out, then ReleaseRegisteredJobs marks the job released
	// (RUNNING) and cancels its ctx — never a FAILED terminal write.
	a.app.Shutdown()

	if st := a.jobStatus(t, jobID); st != "RUNNING" {
		t.Fatalf("status after shutdown = %s, want RUNNING (released for reclaim, not FAILED)", st)
	}
	if !persistedJobReleased(t, jobID) {
		t.Fatalf("job %s not marked released after shutdown", jobID)
	}

	peer := newStandaloneApp(t, func(cfg *app.Config) {
		cfg.SearchJobHeartbeatInterval = 250 * time.Millisecond
		cfg.SearchJobStaleAfter = time.Hour // staleness would not fire for an hour
	})
	if st := peer.waitTerminal(t, jobID, 15*time.Second); st != "SUCCESSFUL" {
		t.Fatalf("peer settled released job %s = %s, want SUCCESSFUL", jobID, st)
	}
	if got, want := persistedJobResultCount(t, jobID), directSearchCount(t, peer.doAuth, model); got != want {
		t.Fatalf("reclaimed job result_count = %d, want %d", got, want)
	}
}

// TestE2E_AsyncSearch_CrashMidScan_PeerCompletes stands up node A over a
// blocking-Iterate backend and node B over a live backend on the SAME Postgres.
// A submits and enters Iterate, then A is Close()d WITHOUT Shutdown — a crash:
// no Release, no terminal write, A's heartbeat simply stops. Once A's job goes
// stale, B's reaper claims and re-executes it on B's live backend, completing
// it SUCCESSFUL with the direct-search count. Asserts the OUTCOME (B finished
// the crashed node's job), not which of A's calls was interrupted.
func TestE2E_AsyncSearch_CrashMidScan_PeerCompletes(t *testing.T) {
	backend, gate := newBlockingIterateBackend(t)
	a := newStandaloneApp(t, func(cfg *app.Config) {
		cfg.StorageBackend = backend
		reaperFastCadence(cfg)
	})
	b := newStandaloneApp(t, reaperFastCadence)

	const model = "crash-midscan-e2e"
	const seeded = 5
	seedStandalone(t, a, model, seeded)

	gate.Block()
	defer gate.Release()

	jobID := a.submitAsync(t, model)
	select {
	case <-gate.Entered():
	case <-time.After(5 * time.Second):
		t.Fatal("A's worker never reached the blocked Iterate call")
	}

	// Crash A: Close without Shutdown. A's reaper stops, its store pool closes,
	// so its job stops heartbeating and no terminal write is made. The job is
	// left RUNNING at epoch 1 with a dead owner — exactly the orphan shape.
	a.closeNow()

	// B's reaper claims the now-stale job and re-executes it on B's live
	// backend. Budget: A's last heartbeat + staleAfter + a few reap ticks +
	// scan time.
	if st := b.waitTerminal(t, jobID, 20*time.Second); st != "SUCCESSFUL" {
		t.Fatalf("B settled crashed-node job %s = %s, want SUCCESSFUL", jobID, st)
	}
	if got, want := persistedJobResultCount(t, jobID), directSearchCount(t, b.doAuth, model); got != want {
		t.Fatalf("peer-completed job result_count = %d, want %d", got, want)
	}
}

// TestE2E_AsyncSearch_CrashMidSave_PartialCleared synthesises a stale RUNNING
// job (epoch 1) AND a committed partial page of search_job_results for it — the
// "executor crashed mid-SaveResults" shape, produced by SQL rather than a
// >1000-match scan. One App (fast cadence) reclaims it: reclaim's ClearResults
// must wipe the partial page before re-running, or the re-execution collides on
// the (tenant_id, job_id, seq) PK (SaveResults restarts seq at 0) or leaves
// duplicates. Asserts SUCCESSFUL, the result_count equals a direct search, and
// the actual result set carries none of the synthetic partial ids.
func TestE2E_AsyncSearch_CrashMidSave_PartialCleared(t *testing.T) {
	a := newStandaloneApp(t, reaperFastCadence)

	const model = "crash-midsave-e2e"
	const seeded = 6
	seedStandalone(t, a, model, seeded)
	want := directSearchCount(t, a.doAuth, model)
	if want != seeded {
		t.Fatalf("direct search returned %d, want %d seeded entities", want, seeded)
	}

	jobID := insertOrphanRunningJob(t, model)
	// A committed partial page from the (now dead) prior epoch: entity ids that
	// are NOT real entities of this model, so any that survive into the final
	// result set are unmistakably leftovers.
	partial := []string{"stale-partial-0", "stale-partial-1", "stale-partial-2"}
	insertPartialResultRows(t, jobID, partial)
	backdateJobCreatedAt(t, jobID, time.Hour)

	if st := a.waitTerminal(t, jobID, 20*time.Second); st != "SUCCESSFUL" {
		t.Fatalf("status = %s, want SUCCESSFUL (reclaim never re-executed the crashed-mid-save job)", st)
	}
	if got := persistedJobResultCount(t, jobID); got != want {
		t.Fatalf("result_count = %d, want %d (direct search over the same condition)", got, want)
	}
	got := a.resultIDs(t, jobID)
	if len(got) != want {
		t.Fatalf("collected %d result ids, want %d", len(got), want)
	}
	stale := map[string]bool{"stale-partial-0": true, "stale-partial-1": true, "stale-partial-2": true}
	for _, id := range got {
		if stale[id] {
			t.Fatalf("stale partial id %q survived into the result set — ClearResults did not wipe the crashed epoch's rows", id)
		}
	}
}

// TestE2E_AsyncSearch_SingleNodeRestart_Reclaims proves the D8 restart path:
// node 1 submits over a blocking backend and is Shutdown before the scan
// finishes (releasing the job, RUNNING+released), then a SECOND App on the same
// Postgres with a live backend reclaims it — via its STARTUP sweep, which runs
// once before the first ticker — and completes it SUCCESSFUL.
func TestE2E_AsyncSearch_SingleNodeRestart_Reclaims(t *testing.T) {
	backend, gate := newBlockingIterateBackend(t)
	a := newStandaloneApp(t, func(cfg *app.Config) {
		cfg.StorageBackend = backend
	})

	const model = "restart-reclaim-e2e"
	const seeded = 4
	seedStandalone(t, a, model, seeded)

	gate.Block()
	defer gate.Release()

	jobID := a.submitAsync(t, model)
	select {
	case <-gate.Entered():
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached the blocked Iterate call")
	}

	a.app.Shutdown() // releases the job: RUNNING+released
	if st := a.jobStatus(t, jobID); st != "RUNNING" {
		t.Fatalf("status after shutdown = %s, want RUNNING (released)", st)
	}

	// The "restarted" node: a fresh App on the same Postgres with a live
	// backend. Its startup sweep reclaims the released job at boot.
	restarted := newStandaloneApp(t, reaperFastCadence)
	if st := restarted.waitTerminal(t, jobID, 20*time.Second); st != "SUCCESSFUL" {
		t.Fatalf("restarted node settled job %s = %s, want SUCCESSFUL", jobID, st)
	}
	if got, want := persistedJobResultCount(t, jobID), directSearchCount(t, restarted.doAuth, model); got != want {
		t.Fatalf("reclaimed job result_count = %d, want %d", got, want)
	}
}

// ---------------------------------------------------------------------------
// (row 8) stale-job reaper
// ---------------------------------------------------------------------------

// Reaper cadence both stale-job tests below run app.New under. It is a
// production-VALID configuration — app.Config.Validate (called at the top of
// app.New) enforces SearchJobStaleAfter >= 4 x SearchJobHeartbeatInterval,
// the mechanical form of spi.AsyncSearchStore.ClaimStale's "interval must be
// dominated by staleAfter" contract — just scaled down so a whole staleness
// window fits inside a test's budget. reaperStaleAfter sits exactly ON the
// enforced floor, so these tests also fail loudly if the floor is ever
// raised without them being revisited.
const (
	reaperHeartbeat    = 500 * time.Millisecond
	reaperStaleAfter   = 4 * reaperHeartbeat // exactly app.ValidateSearchJobStaleAfter's floor
	reaperReapInterval = 250 * time.Millisecond
)

func reaperFastCadence(cfg *app.Config) {
	cfg.SearchJobHeartbeatInterval = reaperHeartbeat
	cfg.SearchJobStaleAfter = reaperStaleAfter
	cfg.SearchReapInterval = reaperReapInterval
}

// TestE2E_AsyncSearch_OrphanReExecuted pins app.New's wired reaper ticker
// (app.go's stopSearchReaper loop calling search.ReclaimStaleJobs) as a
// genuinely running e2e path: an orphaned job is now CLAIMED and RE-EXECUTED
// to SUCCESSFUL, not failed.
//
// The subject is the case ClaimStale exists for: a RUNNING job whose owning
// executor is GONE — the node holding it crashed or was killed — so nothing
// will ever heartbeat it, complete it, or write its terminal status. That
// shape is synthesised directly, by writing the job row the postgres store's
// CreateJob writes (RUNNING, epoch 1, heartbeat_time NULL) with no executor
// behind it and a created_at backdated far past SearchJobStaleAfter, with real
// entities seeded behind its model so the re-execution has something to return.
// Nothing in this process owns the job, so the ONLY thing that can move it off
// RUNNING is the app-wired reaper — which claims it, ClearResults, re-enqueues
// it on this node, and completes it. The result_count must equal a direct
// search over the same condition: the reclaim ran the real scan, not a stub.
//
// Deliberately NOT a job whose Iterate call is blocked: such a job's owner is
// alive and heartbeating, ClaimStale correctly refuses to claim it, and the
// only way to make one look orphaned is to switch its heartbeat off with a
// heartbeat interval no production deployment would run. That configuration
// is now rejected at startup, and the test it supported was asserting an
// artefact of it. The live-but-blocked executor is the INVERSE guarantee and
// gets its own test below.
func TestE2E_AsyncSearch_OrphanReExecuted(t *testing.T) {
	h := newCallbackHarnessConfigured(t, reaperFastCadence)

	const model = "orphan-reexec-e2e"
	h.setupModelSampleWithWorkflow(t, model, `{"name":"Alice","amount":1,"status":"new"}`, secondaryWorkflow)

	const seeded = 5
	for i := 0; i < seeded; i++ {
		if _, status, body := h.CreateEntity(t, model, 1, fmt.Sprintf(`{"name":"e%d","amount":%d,"status":"new"}`, i, i)); status != http.StatusOK {
			t.Fatalf("seed %d: %d %s", i, status, body)
		}
	}

	// Ground truth: a synchronous search over the same match-all condition,
	// through the same app that will re-execute the orphan.
	doAuth := func(method, path, body string) *http.Response { return h.DoAuth(t, method, path, body, "") }
	want := directSearchCount(t, doAuth, model)
	if want != seeded {
		t.Fatalf("direct search returned %d, want %d seeded entities", want, seeded)
	}

	jobID := insertOrphanRunningJob(t, model)
	backdateJobCreatedAt(t, jobID, time.Hour)

	if status := h.waitForAsyncTerminal(t, jobID, 15*time.Second); status != "SUCCESSFUL" {
		t.Fatalf("status = %s, want SUCCESSFUL (reaper never re-executed the orphaned job)", status)
	}
	if got := persistedJobResultCount(t, jobID); got != want {
		t.Fatalf("re-executed orphan result_count = %d, want %d (direct search over the same condition)", got, want)
	}
}

// TestE2E_AsyncSearch_StaleJobReaper_SparesLiveExecutor is the inverse
// guarantee, and the one a false positive in the reaper would violate: a job
// whose executor is ALIVE but making no scan progress — blocked inside
// Iterate for longer than SearchJobStaleAfter — must never be claimed and
// failed. The executor's heartbeat runs on its own ticker, independent of
// scan progress (service.go's startHeartbeat), so the job stays fresh and
// ClaimStale keeps skipping it while the reaper ticks throughout.
//
// Without this, "the reaper fails stale jobs" would be satisfied just as well
// by a reaper that fails every RUNNING job it sees.
func TestE2E_AsyncSearch_StaleJobReaper_SparesLiveExecutor(t *testing.T) {
	backend, gate := newBlockingIterateBackend(t)
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		cfg.StorageBackend = backend
		reaperFastCadence(cfg)
	})

	const model = "stalereaper-live-e2e"
	h.setupModelSampleWithWorkflow(t, model, `{"name":"Alice","amount":1,"status":"new"}`, secondaryWorkflow)
	if _, status, body := h.CreateEntity(t, model, 1, `{"name":"Alice","amount":1,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("seed: %d %s", status, body)
	}

	gate.Block()
	defer gate.Release()

	resp := h.DoAuth(t, http.MethodPost, "/api/search/async/"+model+"/1",
		`{"type":"group","operator":"AND","conditions":[]}`, "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit: %d %s", resp.StatusCode, body)
	}
	jobID := strings.Trim(strings.TrimSpace(body), `"`)

	select {
	case <-gate.Entered():
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached the blocked Iterate call")
	}

	// Watch across two full staleness windows (≈8 heartbeat ticks, ≈16 reaper
	// ticks) with the executor making zero scan progress the whole time.
	deadline := time.Now().Add(2 * reaperStaleAfter)
	for time.Now().Before(deadline) {
		if status := h.asyncJobStatus(t, jobID); status != "RUNNING" {
			t.Fatalf("status = %s, want RUNNING: the reaper claimed a job whose executor is alive and heartbeating", status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestE2E_AsyncSearch_AttemptCap_Fails is the one path that still FAILS an
// orphan: a job whose staleness claims have reached SearchJobMaxAttempts is a
// crash-looping job — every node that claimed it also died mid-scan — and is
// failed with the "search abandoned: executor lost repeatedly" message rather
// than re-executed forever. The App runs with SearchJobMaxAttempts=1, so the
// FIRST staleness claim (bumping stale_claims from the seeded 0 to 1) hits the
// cap. maxAttempts bounds executor losses (stale_claims), never graceful
// handoffs, so the seeded row is stale (not released).
func TestE2E_AsyncSearch_AttemptCap_Fails(t *testing.T) {
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		reaperFastCadence(cfg)
		cfg.SearchJobMaxAttempts = 1 // first staleness claim caps
	})

	const model = "attempt-cap-e2e"
	h.setupModelSampleWithWorkflow(t, model, `{"name":"Alice","amount":1,"status":"new"}`, secondaryWorkflow)

	// stale_claims = MaxAttempts - 1 = 0: the next staleness claim reaches the
	// cap. (0 is also the default, but seed it explicitly so intent survives a
	// change to the default MaxAttempts.)
	jobID := insertRunningJobRow(t, model, 0)
	backdateJobCreatedAt(t, jobID, time.Hour)

	if status := h.waitForAsyncTerminal(t, jobID, 15*time.Second); status != "FAILED" {
		t.Fatalf("status = %s, want FAILED (attempt cap not enforced)", status)
	}
	if msg := persistedJobErrorFor(t, jobID); msg != "search abandoned: executor lost repeatedly" {
		t.Errorf("persisted error = %q, want the attempts-exhausted message", msg)
	}
}

// ---------------------------------------------------------------------------
// Small helpers for this file's scenarios. (The blocking-Iterate backend and
// iterateGate above ARE shared with async_cancel_multinode_test.go; the direct
// job-row helpers below are not.)
// ---------------------------------------------------------------------------

// insertOrphanRunningJob writes a RUNNING search-job row with no executor
// behind it — the crashed-or-killed-node shape spi.AsyncSearchStore.ClaimStale
// exists to recover — straight into the shared testcontainer. The column set
// and values mirror the postgres store's CreateJob exactly (epoch persisted
// as 1 per its contract, heartbeat_time left NULL so staleness is measured
// from created_at, the SPI's documented baseline), so the row is
// indistinguishable from one a real submit produced; what is missing is only
// the executor, which is the point.
//
// The row is written under the callback harness's bootstrap tenant, so it is
// visible to that stack's authenticated status reads. Returns the job id.
func insertOrphanRunningJob(t *testing.T, model string) string {
	t.Helper()
	return insertRunningJobRow(t, model, 0)
}

// insertRunningJobRow is insertOrphanRunningJob generalised over the
// stale_claims counter (the attempt-cap input): staleClaims is the number of
// prior staleness claims recorded against the job, so a caller can seed a job
// one claim short of SearchJobMaxAttempts and watch the next sweep cap it.
//
// search_opts carries the store-all options a real match-all async submit
// writes (limit 0 — "Async submit intentionally leaves the limit unset",
// handler.go — and no orderBy). A real submit resolves PointInTime to now() and
// writes it BOTH into search_opts (what the executor scans at) AND into the
// point_in_time column (what GetAsyncResults' GetAsAt fetches entities at), so
// the two must agree here too — otherwise the scan counts N but the results
// endpoint fetches them as-of the zero time and returns nothing. The PIT is set
// a minute ahead of every seeded commit (so any small Go/Postgres clock skew
// still leaves it after them); a PIT past all commits resolves to "all
// currently committed", exactly the match-all set. Re-execution now decodes
// search_opts (decodeStoredJob), so unlike the pre-reclaim disposition neither
// column can be NULL/zero.
func insertRunningJobRow(t *testing.T, model string, staleClaims int64) string {
	t.Helper()
	const tenantID = "test-tenant" // callbackHarness's cfg.Bootstrap.TenantID
	jobID := uuid.NewString()

	pit := time.Now().Add(time.Minute).UTC()
	optsJSON, err := json.Marshal(struct {
		Limit       int       `json:"limit"`
		PointInTime time.Time `json:"pointInTime"`
	}{Limit: 0, PointInTime: pit})
	if err != nil {
		t.Fatalf("marshal search opts: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, withAppName(t, pgURLFromEnv(t), "stale-reaper-orphan-writer"))
	if err != nil {
		t.Fatalf("open orphan-writer pool: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx,
		`INSERT INTO search_jobs (id, tenant_id, status, model_name, model_ver, condition, point_in_time, search_opts, result_count, error, created_at, calc_ms, epoch, stale_claims)
		 VALUES ($1, $2, 'RUNNING', $3, '1', $4, $5, $6, 0, '', now(), 0, 1, $7)`,
		jobID, tenantID, model,
		[]byte(`{"type":"group","operator":"AND","conditions":[]}`),
		pit,
		optsJSON,
		staleClaims); err != nil {
		t.Fatalf("insert orphan search job: %v", err)
	}
	return jobID
}

// insertPartialResultRows writes a committed partial page of search_job_results
// for jobID by SQL — the "executor crashed mid-SaveResults" shape, without
// needing a scan large enough to page. entityIDs are seq 0..n-1. ClearResults
// (which reclaim runs before re-executing) must wipe these, or the re-execution
// either collides on the (tenant_id, job_id, seq) PK (SaveResults restarts seq
// at 0) or leaves duplicates in the final result set.
func insertPartialResultRows(t *testing.T, jobID string, entityIDs []string) {
	t.Helper()
	const tenantID = "test-tenant"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, withAppName(t, pgURLFromEnv(t), "stale-reaper-partial-writer"))
	if err != nil {
		t.Fatalf("open partial-writer pool: %v", err)
	}
	defer pool.Close()
	for seq, id := range entityIDs {
		if _, err := pool.Exec(ctx,
			`INSERT INTO search_job_results (job_id, tenant_id, seq, entity_id) VALUES ($1, $2, $3, $4)`,
			jobID, tenantID, seq, id); err != nil {
			t.Fatalf("insert partial result row seq=%d: %v", seq, err)
		}
	}
}

// backdateJobCreatedAt pushes a search job's created_at back by age via a
// direct connection to the shared testcontainer, so the reaper's staleness
// check (HeartbeatTime, or CreateTime as the baseline when never
// heartbeated) treats it as long orphaned without waiting real wall-clock
// time.
func backdateJobCreatedAt(t *testing.T, jobID string, age time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, withAppName(t, pgURLFromEnv(t), "stale-reaper-backdater"))
	if err != nil {
		t.Fatalf("open backdater pool: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `UPDATE search_jobs SET created_at = created_at - ($2 * interval '1 microsecond') WHERE id = $1`,
		jobID, age.Microseconds()); err != nil {
		t.Fatalf("backdate search job %s: %v", jobID, err)
	}
}

// persistedJobErrorFor mirrors storage_ceilings_e2e_test.go's
// persistedJobError (kept package-private there without exporting) so this
// file can read the job's persisted error message directly, independent of
// which harness/app instance submitted it — the row is visible to any
// connection against the shared Postgres backend.
func persistedJobErrorFor(t *testing.T, jobID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, withAppName(t, pgURLFromEnv(t), "async-stream-job-reader"))
	if err != nil {
		t.Fatalf("open reader pool: %v", err)
	}
	defer pool.Close()
	var msg string
	if err := pool.QueryRow(ctx, `SELECT error FROM search_jobs WHERE id = $1`, jobID).Scan(&msg); err != nil {
		t.Fatalf("read search job %s: %v", jobID, err)
	}
	return msg
}

// persistedJobResultCount reads the result_count a completed job recorded,
// straight from the shared Postgres — independent of which app instance
// executed it. UpdateJobStatus writes it on the terminal SUCCESSFUL transition.
func persistedJobResultCount(t *testing.T, jobID string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, withAppName(t, pgURLFromEnv(t), "async-stream-count-reader"))
	if err != nil {
		t.Fatalf("open reader pool: %v", err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow(ctx, `SELECT result_count FROM search_jobs WHERE id = $1`, jobID).Scan(&n); err != nil {
		t.Fatalf("read search job %s result_count: %v", jobID, err)
	}
	return n
}

// persistedJobReleased reads the released flag — the shutdown-release tests
// assert Shutdown left the job RUNNING+released (for reclaim), not FAILED.
func persistedJobReleased(t *testing.T, jobID string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, withAppName(t, pgURLFromEnv(t), "async-stream-released-reader"))
	if err != nil {
		t.Fatalf("open reader pool: %v", err)
	}
	defer pool.Close()
	var released bool
	if err := pool.QueryRow(ctx, `SELECT released FROM search_jobs WHERE id = $1`, jobID).Scan(&released); err != nil {
		t.Fatalf("read search job %s released: %v", jobID, err)
	}
	return released
}

// fetchClientToken obtains a JWT via client_credentials grant against an
// arbitrary base URL — the package-level getTokenRaw hardcodes the shared
// TestMain serverURL, which the standalone shutdown-drain app does not use.
func fetchClientToken(t *testing.T, baseURL, clientID, clientSecret string) string {
	t.Helper()
	form := "grant_type=client_credentials"
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/oauth/token", strings.NewReader(form))
	if err != nil {
		t.Fatalf("new token request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, clientSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("token request: %v", err)
	}
	body := readHTTPBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token request: %d %s", resp.StatusCode, body)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("decode token response: %v; body=%s", err, body)
	}
	token, _ := result["access_token"].(string)
	if token == "" {
		t.Fatalf("no access_token in response: %s", body)
	}
	return token
}

// doAuthAgainst issues an authenticated request against an arbitrary base
// URL with a caller-supplied bearer token.
func doAuthAgainst(t *testing.T, baseURL, token, method, path, body string) *http.Response {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// setupSimpleModelWorkflow imports+locks a model from sample data and
// imports secondaryWorkflow, using an arbitrary authenticated-request
// closure rather than a *callbackHarness (the shutdown-drain test's
// standalone app has no harness).
func setupSimpleModelWorkflow(t *testing.T, doAuthOn func(method, path, body string) *http.Response, entityName string) {
	t.Helper()
	resp := doAuthOn(http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", entityName), `{"name":"Alice","amount":1,"status":"new"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import model %s: %d %s", entityName, resp.StatusCode, readHTTPBody(t, resp))
	}
	resp = doAuthOn(http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", entityName), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lock model %s: %d %s", entityName, resp.StatusCode, readHTTPBody(t, resp))
	}
	resp = doAuthOn(http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", entityName), secondaryWorkflow)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import workflow %s: %d %s", entityName, resp.StatusCode, readHTTPBody(t, resp))
	}
}

func readHTTPBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// Standalone app.App — the crash / shutdown-release / restart scenarios need
// to drive Shutdown() and Close() themselves (a crash calls Close WITHOUT
// Shutdown; a graceful stop calls Shutdown then lets cleanup Close).
// newCallbackHarnessConfigured is unusable for these: it registers
// t.Cleanup(a.Shutdown) AND t.Cleanup(a.Close), so a test that also shut the
// app down would double-drive teardown (a second Shutdown double-closes the
// reaper channel; the harness guards that ordering for the common case, not
// for a test that owns the lifecycle). Each standaloneApp shares the package
// Postgres testcontainer under the same "test-tenant", so a peer sees another
// peer's jobs, models, and entities.
// ---------------------------------------------------------------------------

type standaloneApp struct {
	app      *app.App
	baseURL  string
	doAuth   func(method, path, body string) *http.Response
	closeNow func() // idempotent a.Close(); safe from the test body and again from cleanup
}

// newStandaloneApp builds an app.App on the shared Postgres, applies configure
// (a blocking-Iterate backend, a reaper cadence, …), serves its HTTP handler,
// and mints an admin token. Only a once-guarded Close is wired into t.Cleanup;
// the caller drives Shutdown()/Close() itself.
func newStandaloneApp(t *testing.T, configure func(*app.Config)) *standaloneApp {
	t.Helper()

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}))

	cfg := app.DefaultConfig()
	cfg.ContextPath = "/api"
	cfg.StorageBackend = "postgres" // CYODA_POSTGRES_URL is set by TestMain and still live.
	cfg.IAM.Mode = "jwt"
	cfg.IAM.JWTSigningKey = keyPEM
	cfg.IAM.JWTIssuer = "cyoda-standalone-test"
	cfg.IAM.JWTExpiry = 3600
	cfg.Bootstrap = app.BootstrapConfig{
		ClientID: "standalone-client-" + uuid.NewString(), ClientSecret: "standalone-secret",
		TenantID: "test-tenant", UserID: "standalone-admin", Roles: "ROLE_ADMIN,ROLE_M2M",
	}

	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	t.Cleanup(srv.Close)
	cfg.HTTPPort = srv.Listener.Addr().(*net.TCPAddr).Port

	if configure != nil {
		configure(&cfg)
	}

	a := app.New(cfg)
	srv.Config.Handler = a.Handler()

	var once sync.Once
	closeNow := func() { once.Do(func() { _ = a.Close() }) }
	t.Cleanup(closeNow)

	token := fetchClientToken(t, srv.URL, cfg.Bootstrap.ClientID, cfg.Bootstrap.ClientSecret)
	doAuth := func(method, path, body string) *http.Response {
		return doAuthAgainst(t, srv.URL, token, method, path, body)
	}
	return &standaloneApp{app: a, baseURL: srv.URL, doAuth: doAuth, closeNow: closeNow}
}

// submitAsync submits a match-all async search over model on this app and
// returns the job id.
func (s *standaloneApp) submitAsync(t *testing.T, model string) string {
	t.Helper()
	resp := s.doAuth(http.MethodPost, "/api/search/async/"+model+"/1", `{"type":"group","operator":"AND","conditions":[]}`)
	body := readHTTPBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit async on %s: %d %s", s.baseURL, resp.StatusCode, body)
	}
	return strings.Trim(strings.TrimSpace(body), `"`)
}

// jobStatus reads searchJobStatus for jobID over this app's HTTP door.
func (s *standaloneApp) jobStatus(t *testing.T, jobID string) string {
	t.Helper()
	resp := s.doAuth(http.MethodGet, "/api/search/async/"+jobID+"/status", "")
	body := readHTTPBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("async status: %d %s", resp.StatusCode, body)
	}
	var st struct {
		SearchJobStatus string `json:"searchJobStatus"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("decode status: %v; body=%s", err, body)
	}
	return st.SearchJobStatus
}

// waitTerminal polls this app's status endpoint until the job leaves RUNNING.
func (s *standaloneApp) waitTerminal(t *testing.T, jobID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st := s.jobStatus(t, jobID); st != "RUNNING" {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job %s never left RUNNING within %v (via %s)", jobID, timeout, s.baseURL)
	return ""
}

// resultIDs pages this app's async-results endpoint and returns the entity ids,
// in order — the actual persisted result set (search_job_results), the read
// that would expose a stale partial page reclaim failed to clear.
func (s *standaloneApp) resultIDs(t *testing.T, jobID string) []string {
	t.Helper()
	var ids []string
	const pageSize = 100
	for page := 0; ; page++ {
		resp := s.doAuth(http.MethodGet,
			fmt.Sprintf("/api/search/async/%s?pageSize=%d&pageNumber=%d", jobID, pageSize, page), "")
		body := readHTTPBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("results page %d: %d %s", page, resp.StatusCode, body)
		}
		var parsed struct {
			Content []map[string]any `json:"content"`
			Page    struct {
				TotalPages int `json:"totalPages"`
			} `json:"page"`
		}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			t.Fatalf("decode results page %d: %v; body=%s", page, err, body)
		}
		for _, e := range parsed.Content {
			meta, _ := e["meta"].(map[string]any)
			id, _ := meta["id"].(string)
			ids = append(ids, id)
		}
		if len(parsed.Content) == 0 || page+1 >= parsed.Page.TotalPages {
			break
		}
	}
	return ids
}

// directSearchCount runs a synchronous search over the same match-all
// condition and counts the ndjson result lines — the ground truth an
// async re-execution's result_count must equal. doAuth is any app's authed
// request closure (the one that has the model + entities).
func directSearchCount(t *testing.T, doAuth func(method, path, body string) *http.Response, model string) int {
	t.Helper()
	resp := doAuth(http.MethodPost, fmt.Sprintf("/api/search/direct/%s/1", model), `{"type":"group","operator":"AND","conditions":[]}`)
	body := readHTTPBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("direct search over %s: %d %s", model, resp.StatusCode, body)
	}
	return countNDJSONLines(body)
}
