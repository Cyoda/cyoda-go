package entity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// --- fakes ---

// blockingEntityStore blocks Save until ctx is Done and returns ctx.Err().
// Deterministic by construction: the test never races a wall-clock sleep
// against the deadline, it waits on the deadline's own cancellation signal.
type blockingEntityStore struct {
	spi.EntityStore
	mu    sync.Mutex
	saves int
}

func (s *blockingEntityStore) Save(ctx context.Context, _ *spi.Entity) (int64, error) {
	s.mu.Lock()
	s.saves++
	s.mu.Unlock()
	<-ctx.Done()
	return 0, ctx.Err()
}

// blockingFactory wraps a real spi.StoreFactory but always hands out the
// given blockingEntityStore from EntityStore(); every other accessor
// (ModelStore, StateMachineAuditStore, WorkflowStore, ...) delegates to the
// wrapped factory so model setup and workflow-engine bookkeeping behave
// normally.
type blockingFactory struct {
	spi.StoreFactory
	entityStore *blockingEntityStore
}

func (f *blockingFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.entityStore, nil
}

// countingModelStoreFactory wraps a real spi.StoreFactory but counts every
// ModelStore.Get call across the wrapped store's lifetime. CreateEntityCollection
// calls modelStore.Get exactly once per item, BEFORE it ever begins a
// transaction or touches the workflow engine — so this count is a proxy that
// distinguishes "the chunk was never attempted" (loop-head check short-circuits
// before CreateEntityCollection is even called — Get count does not advance)
// from "the chunk was attempted and only failed once inside the engine's own,
// separately-existing cascade-level D9 check" (Get count DOES advance, since
// Get runs before that check). Every other accessor delegates unchanged.
type countingModelStoreFactory struct {
	spi.StoreFactory
	mu   sync.Mutex
	gets int
}

func (f *countingModelStoreFactory) ModelStore(ctx context.Context) (spi.ModelStore, error) {
	real, err := f.StoreFactory.ModelStore(ctx)
	if err != nil {
		return nil, err
	}
	return &countingModelStore{ModelStore: real, f: f}, nil
}

func (f *countingModelStoreFactory) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}

type countingModelStore struct {
	spi.ModelStore
	f *countingModelStoreFactory
}

func (s *countingModelStore) Get(ctx context.Context, ref spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.f.mu.Lock()
	s.f.gets++
	s.f.mu.Unlock()
	return s.ModelStore.Get(ctx, ref)
}

// recordingTxMgr wraps a real spi.TransactionManager, recording every
// Commit/Rollback call and optionally running onCommit while a commit is
// genuinely in flight (before delegating to the real Commit, or before
// returning commitErr directly when set — simulating a commit that fails
// without needing a real backing store outcome).
type recordingTxMgr struct {
	spi.TransactionManager
	mu         sync.Mutex
	begins     int
	committed  []string
	rolledBack []string
	onCommit   func(ctx context.Context)
	// commitErr, when non-nil, is returned directly instead of delegating to
	// the real TransactionManager — lets a test simulate a commit failure
	// (a nested backend deadline, a conflict, an already-interrupted commit)
	// without needing to reproduce the real backend condition that would
	// normally produce it.
	commitErr error
}

// Begin counts every Begin call before delegating. Lets a test distinguish
// "a downstream call was never even entered" (begins count doesn't advance)
// from "it was entered and only failed after opening its own transaction" —
// the same distinction the audit-store-accessor count draws for engine.Execute
// below, applied to a call whose own entry point is Begin rather than Execute.
func (m *recordingTxMgr) Begin(ctx context.Context) (string, context.Context, error) {
	m.mu.Lock()
	m.begins++
	m.mu.Unlock()
	return m.TransactionManager.Begin(ctx)
}

func (m *recordingTxMgr) beginCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.begins
}

func (m *recordingTxMgr) Commit(ctx context.Context, txID string) error {
	if m.onCommit != nil {
		m.onCommit(ctx)
	}
	if m.commitErr != nil {
		return m.commitErr
	}
	m.mu.Lock()
	m.committed = append(m.committed, txID)
	m.mu.Unlock()
	return m.TransactionManager.Commit(ctx, txID)
}

func (m *recordingTxMgr) Rollback(ctx context.Context, txID string) error {
	m.mu.Lock()
	m.rolledBack = append(m.rolledBack, txID)
	m.mu.Unlock()
	return m.TransactionManager.Rollback(ctx, txID)
}

// newReqTimeoutHandler builds a Handler wired to the given factory/txMgr with
// the "Person" model ({name: String, age: Integer}) imported and locked, and
// a context carrying a test user. Mirrors newPatchTestHandler but lets the
// caller supply fakes for the store/tx-manager seams this file exercises.
func newReqTimeoutHandler(t *testing.T, factory spi.StoreFactory, txMgr spi.TransactionManager) (*Handler, context.Context) {
	t.Helper()

	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "reqtimeout-test-user",
		UserName: "ReqTimeout Test",
		Tenant:   spi.Tenant{ID: "reqtimeout-tenant", Name: "ReqTimeout"},
		Roles:    []string{"user"},
	})

	engine := wfengine.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	h := New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), nil)

	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	node.SetChild("age", schema.NewLeafNode(schema.Integer))
	raw, mErr := schema.Marshal(node)
	if mErr != nil {
		t.Fatalf("schema.Marshal: %v", mErr)
	}

	modelStore, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := modelStore.Save(ctx, &spi.ModelDescriptor{
		Ref:    spi.ModelRef{EntityName: "Person", ModelVersion: "1"},
		State:  spi.ModelLocked,
		Schema: raw,
	}); err != nil {
		t.Fatalf("ModelStore.Save: %v", err)
	}

	return h, ctx
}

// --- table-driven request builders for all 8 write ops ---

// reqTimeoutOp invokes one write-op handler directly (bypassing the
// generated router) with the given ctx and TransactionTimeoutMillis pointer,
// returning the recorded HTTP response. entityID is used only in the request
// path/body — the invalid/joined cases below never reach the service layer,
// so it need not name a real entity.
type reqTimeoutOp struct {
	name   string
	invoke func(h *Handler, ctx context.Context, entityID string, millis *int64) *httptest.ResponseRecorder
}

func reqTimeoutOps() []reqTimeoutOp {
	star := "*"
	return []reqTimeoutOp{
		{
			name: "Create",
			invoke: func(h *Handler, ctx context.Context, entityID string, millis *int64) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/entity/JSON/Person/1", strings.NewReader(`{"name":"A","age":1}`)).WithContext(ctx)
				w := httptest.NewRecorder()
				h.Create(w, r, "JSON", "Person", 1, genapi.CreateParams{TransactionTimeoutMillis: millis})
				return w
			},
		},
		{
			name: "CreateCollection",
			invoke: func(h *Handler, ctx context.Context, entityID string, millis *int64) *httptest.ResponseRecorder {
				body := `[{"model":{"name":"Person","version":1},"payload":"{\"name\":\"A\",\"age\":1}"}]`
				r := httptest.NewRequest(http.MethodPost, "/entity/JSON", strings.NewReader(body)).WithContext(ctx)
				w := httptest.NewRecorder()
				h.CreateCollection(w, r, "JSON", genapi.CreateCollectionParams{TransactionTimeoutMillis: millis})
				return w
			},
		},
		{
			name: "UpdateCollection",
			invoke: func(h *Handler, ctx context.Context, entityID string, millis *int64) *httptest.ResponseRecorder {
				body := fmt.Sprintf(`[{"id":%q,"payload":"{\"name\":\"B\"}"}]`, entityID)
				r := httptest.NewRequest(http.MethodPut, "/entity/JSON", strings.NewReader(body)).WithContext(ctx)
				w := httptest.NewRecorder()
				h.UpdateCollection(w, r, genapi.UpdateCollectionParamsFormatJSON, genapi.UpdateCollectionParams{TransactionTimeoutMillis: millis})
				return w
			},
		},
		{
			name: "UpdateSingleWithLoopback",
			invoke: func(h *Handler, ctx context.Context, entityID string, millis *int64) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPut, "/entity/JSON/"+entityID, strings.NewReader(`{"name":"B"}`)).WithContext(ctx)
				w := httptest.NewRecorder()
				h.UpdateSingleWithLoopback(w, r, "JSON", openapi_types.UUID(mustUUID(entityID)), genapi.UpdateSingleWithLoopbackParams{TransactionTimeoutMillis: millis, IfMatch: &star})
				return w
			},
		},
		{
			name: "UpdateSingle",
			invoke: func(h *Handler, ctx context.Context, entityID string, millis *int64) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPut, "/entity/JSON/"+entityID+"/loopback", strings.NewReader(`{"name":"B"}`)).WithContext(ctx)
				w := httptest.NewRecorder()
				h.UpdateSingle(w, r, "JSON", openapi_types.UUID(mustUUID(entityID)), "loopback", genapi.UpdateSingleParams{TransactionTimeoutMillis: millis, IfMatch: &star})
				return w
			},
		},
		{
			name: "PatchSingleWithLoopback",
			invoke: func(h *Handler, ctx context.Context, entityID string, millis *int64) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPatch, "/entity/JSON/"+entityID, strings.NewReader(`{"name":"B"}`)).WithContext(ctx)
				r.Header.Set("Content-Type", "application/merge-patch+json")
				w := httptest.NewRecorder()
				h.PatchSingleWithLoopback(w, r, "JSON", openapi_types.UUID(mustUUID(entityID)), genapi.PatchSingleWithLoopbackParams{TransactionTimeoutMillis: millis, IfMatch: &star})
				return w
			},
		},
		{
			name: "PatchSingle",
			invoke: func(h *Handler, ctx context.Context, entityID string, millis *int64) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPatch, "/entity/JSON/"+entityID+"/loopback", strings.NewReader(`{"name":"B"}`)).WithContext(ctx)
				r.Header.Set("Content-Type", "application/merge-patch+json")
				w := httptest.NewRecorder()
				h.PatchSingle(w, r, "JSON", openapi_types.UUID(mustUUID(entityID)), "loopback", genapi.PatchSingleParams{TransactionTimeoutMillis: millis, IfMatch: &star})
				return w
			},
		},
	}
}

// --- Step 1 (RED) tests, now GREEN ---

// TestWriteOps_TransactionTimeoutMillis_Invalid400 pins that every one of the
// eight write ops rejects a non-positive transactionTimeoutMillis with 400
// BAD_REQUEST before doing any I/O. None of these requests name a real
// entity — resolveRequestTimeout runs before the handler ever reaches the
// service layer, so the invalid param is the only thing that can decide the
// response.
func TestWriteOps_TransactionTimeoutMillis_Invalid400(t *testing.T) {
	factory := memory.NewStoreFactory()
	h, _ := newReqTimeoutHandler(t, factory, mustTxMgr(t, factory))
	for _, op := range reqTimeoutOps() {
		for _, bad := range []int64{0, -1} {
			bad := bad
			t.Run(fmt.Sprintf("%s/%d", op.name, bad), func(t *testing.T) {
				w := op.invoke(h, context.Background(), sampleUUID, &bad)
				if w.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
				}
				var pd struct {
					Properties map[string]any `json:"properties"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				if pd.Properties["errorCode"] != common.ErrCodeBadRequest {
					t.Fatalf("errorCode = %v, want %v", pd.Properties["errorCode"], common.ErrCodeBadRequest)
				}
			})
		}
	}
}

// TestWriteOps_TransactionTimeoutMillis_JoinedRejected400 pins that a request
// carrying a joined transaction (spi.GetTransaction(ctx) != nil — how a
// routed compute-node callback presents at param-resolution time) is
// rejected with 400 rather than silently honoring or silently ignoring the
// client-supplied deadline. The response names the param and explains why.
func TestWriteOps_TransactionTimeoutMillis_JoinedRejected400(t *testing.T) {
	factory := memory.NewStoreFactory()
	h, _ := newReqTimeoutHandler(t, factory, mustTxMgr(t, factory))
	millis := int64(5000)
	for _, op := range reqTimeoutOps() {
		t.Run(op.name, func(t *testing.T) {
			ctx := spi.WithTransaction(context.Background(), &spi.TransactionState{ID: "tx-1"})
			w := op.invoke(h, ctx, sampleUUID, &millis)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
			var pd struct {
				Detail     string         `json:"detail"`
				Properties map[string]any `json:"properties"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if !strings.Contains(pd.Detail, "transactionTimeoutMillis") {
				t.Fatalf("body does not mention the param name: %q", pd.Detail)
			}
			if !strings.Contains(pd.Detail, "joins an open transaction") {
				t.Fatalf("body does not mention the joined transaction: %q", pd.Detail)
			}
			if pd.Properties["errorCode"] != common.ErrCodeBadRequest {
				t.Fatalf("errorCode = %v, want %v", pd.Properties["errorCode"], common.ErrCodeBadRequest)
			}
		})
	}
}

// TestCreate_TransactionTimeout_408NothingCommitted pins the D2/D8 contract
// end to end through the HTTP handler: an expired feature deadline surfaces
// as 408 TRANSACTION_TIMEOUT (retryable), and nothing commits — Save blocks
// until the deadline fires and the pre-commit check in txScope.Commit never
// lets a Commit be attempted, so the deferred Release rolls back instead.
func TestCreate_TransactionTimeout_408NothingCommitted(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	realTxMgr := mustTxMgr(t, realFactory)
	rtm := &recordingTxMgr{TransactionManager: realTxMgr}
	blocking := &blockingEntityStore{}
	factory := &blockingFactory{StoreFactory: realFactory, entityStore: blocking}

	h, ctx := newReqTimeoutHandler(t, factory, rtm)

	millis := int64(1)
	r := httptest.NewRequest(http.MethodPost, "/entity/JSON/Person/1", strings.NewReader(`{"name":"A","age":1}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.Create(w, r, "JSON", "Person", 1, genapi.CreateParams{TransactionTimeoutMillis: &millis})

	if w.Code != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408; body: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if pd.Properties["errorCode"] != common.ErrCodeTransactionTimeout {
		t.Fatalf("errorCode = %v, want %v", pd.Properties["errorCode"], common.ErrCodeTransactionTimeout)
	}
	if pd.Properties["retryable"] != true {
		t.Fatalf("retryable = %v, want true", pd.Properties["retryable"])
	}

	rtm.mu.Lock()
	committed, rolledBack := rtm.committed, rtm.rolledBack
	rtm.mu.Unlock()
	if len(committed) != 0 {
		t.Fatalf("commit ran despite the expired deadline: %v", committed)
	}
	if len(rolledBack) != 1 {
		t.Fatalf("expected exactly one rollback, got %d: %v", len(rolledBack), rolledBack)
	}
}

// TestCreate_CommitWins_DeadlineAfterCommit200 pins that once Commit has
// legitimately started (the pre-commit check passed while the request ctx
// was still live), the commit runs on the shielded common.CommitContext
// derivative and is not aborted by the request deadline firing mid-commit —
// the response is 200, matching spec D2's "an interrupted commit is an
// in-doubt outcome, never a rollback-able one".
func TestCreate_CommitWins_DeadlineAfterCommit200(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	realTxMgr := mustTxMgr(t, realFactory)

	var sawCancellation bool
	rtm := &recordingTxMgr{TransactionManager: realTxMgr}
	rtm.onCommit = func(ctx context.Context) {
		// Outlives the request deadline below by a wide margin; the shielded
		// ctx must show no cancellation regardless of how long this runs.
		time.Sleep(50 * time.Millisecond)
		if ctx.Err() != nil {
			sawCancellation = true
		}
	}

	h, ctx := newReqTimeoutHandler(t, realFactory, rtm)

	millis := int64(10)
	r := httptest.NewRequest(http.MethodPost, "/entity/JSON/Person/1", strings.NewReader(`{"name":"A","age":1}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.Create(w, r, "JSON", "Person", 1, genapi.CreateParams{TransactionTimeoutMillis: &millis})

	if sawCancellation {
		t.Fatal("commit ctx observed the request deadline mid-commit; commit must run shielded")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	rtm.mu.Lock()
	committed := rtm.committed
	rtm.mu.Unlock()
	if len(committed) != 1 {
		t.Fatalf("commit did not run: %v", committed)
	}
}

// TestCreate_CommitBornDeadlineError_ClientDeadlineLive_Stays500 is the
// non-overlap half of the Critical-2 regression: Commit itself fails with an
// error whose chain contains context.DeadlineExceeded (e.g. a nested
// backend-side statement/pool-acquire timeout, unrelated to the client's own
// budget) while the client's transactionTimeoutMillis is large and still
// live. The ours-actually-expired conjunct in common.ClassifyRequestTimeout
// must reject this on its own (ctx.Err() is nil, not DeadlineExceeded) —
// this commit-born error must never be misread as the client's clean 408.
func TestCreate_CommitBornDeadlineError_ClientDeadlineLive_Stays500(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	realTxMgr := mustTxMgr(t, realFactory)
	rtm := &recordingTxMgr{
		TransactionManager: realTxMgr,
		commitErr:          fmt.Errorf("nested pool-acquire timeout: %w", context.DeadlineExceeded),
	}
	h, ctx := newReqTimeoutHandler(t, realFactory, rtm)

	millis := int64(60000) // large — never expires during this test
	r := httptest.NewRequest(http.MethodPost, "/entity/JSON/Person/1", strings.NewReader(`{"name":"A","age":1}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.Create(w, r, "JSON", "Person", 1, genapi.CreateParams{TransactionTimeoutMillis: &millis})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if pd.Properties["errorCode"] == common.ErrCodeTransactionTimeout {
		t.Fatalf("errorCode must not be TRANSACTION_TIMEOUT here: %v", pd.Properties["errorCode"])
	}
}

// TestCreate_CommitInterrupted_OverlapCase_Stays500Not408 is the overlap half
// of the Critical-2 regression: the client's own deadline ALSO happens to
// have genuinely expired by the time Commit returns (isolating the
// ErrCommitInterrupted sentinel as what disqualifies 408 here — the
// ours-actually-expired conjunct alone would NOT have rejected this, since
// ctx really is expired). Commit is given a generous deadline margin (50ms)
// before it is invoked, so the pre-commit check in txScope.Commit reliably
// lets the commit attempt start, then sleeps well past that deadline before
// returning an error that a genuinely-interrupted commitOwned would have
// produced (ErrCommitInterrupted wrapping DeadlineExceeded) — simulating the
// outcome without waiting out the real 30s commitBudget.
func TestCreate_CommitInterrupted_OverlapCase_Stays500Not408(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	realTxMgr := mustTxMgr(t, realFactory)
	rtm := &recordingTxMgr{TransactionManager: realTxMgr}
	rtm.onCommit = func(_ context.Context) {
		time.Sleep(500 * time.Millisecond)
	}
	rtm.commitErr = fmt.Errorf("%w: %w", common.ErrCommitInterrupted, fmt.Errorf("commit: %w", context.DeadlineExceeded))

	h, ctx := newReqTimeoutHandler(t, realFactory, rtm)

	millis := int64(200) // generous margin for the pre-commit check to pass; well under the 500ms sleep above
	r := httptest.NewRequest(http.MethodPost, "/entity/JSON/Person/1", strings.NewReader(`{"name":"A","age":1}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.Create(w, r, "JSON", "Person", 1, genapi.CreateParams{TransactionTimeoutMillis: &millis})

	if w.Code == http.StatusRequestTimeout {
		t.Fatalf("must never be 408 in the overlap case; body: %s", w.Body.String())
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
}

// TestCreate_CommitConflict_Preserved409 is test 3 from the Critical-2
// regression list: a clean commit failure (spi.ErrConflict) on a still-live
// commitCtx must keep its existing 409 classification — WrapIfCommitInterrupted
// only wraps when commitCtx itself is the thing that failed, which a fast
// synchronous ErrConflict return never trips.
func TestCreate_CommitConflict_Preserved409(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	realTxMgr := mustTxMgr(t, realFactory)
	rtm := &recordingTxMgr{TransactionManager: realTxMgr, commitErr: spi.ErrConflict}
	h, ctx := newReqTimeoutHandler(t, realFactory, rtm)

	millis := int64(60000)
	r := httptest.NewRequest(http.MethodPost, "/entity/JSON/Person/1", strings.NewReader(`{"name":"A","age":1}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.Create(w, r, "JSON", "Person", 1, genapi.CreateParams{TransactionTimeoutMillis: &millis})

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (spi.ErrConflict must keep its classification); body: %s", w.Code, w.Body.String())
	}
}

// mustTxMgr returns factory's real TransactionManager, failing the test on error.
func mustTxMgr(t *testing.T, factory spi.StoreFactory) spi.TransactionManager {
	t.Helper()
	txMgr, err := factory.TransactionManager(context.Background())
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	return txMgr
}

// --- Task 6: domain-loop cancellation checks (spec D9) ---

// TestRunChunkedCreate_CtxExpiredBetweenChunks pins that runChunkedCreate's
// chunk loop observes ctx.Err() generically at each iteration head, not only
// via a failing store call. Chunk 0 legitimately commits while the client's
// deadline is still live — the fake txMgr's Commit hook lets the pre-commit
// check in txScope.Commit pass, then sleeps well past the deadline before
// returning (the same "generous margin, then overshoot" pattern used by
// TestCreate_CommitInterrupted_OverlapCase_Stays500Not408 above, so the test
// never races a close wall-clock deadline). By the time the loop reaches
// chunk 1's iteration head, the deadline has genuinely, naturally expired
// (DeadlineExceeded, not a manual Canceled) — the check must catch it there,
// BEFORE ever calling CreateEntityCollection for chunk 1, and route it
// through the identical error-element path a genuine chunk failure takes
// (D3): chunk 0 stays durable, chunk 1 surfaces as a TRANSACTION_TIMEOUT
// error element, and chunk 2 is never attempted.
func TestRunChunkedCreate_CtxExpiredBetweenChunks(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	realTxMgr := mustTxMgr(t, realFactory)
	rtm := &recordingTxMgr{TransactionManager: realTxMgr}
	countingFactory := &countingModelStoreFactory{StoreFactory: realFactory}

	h, ctx := newReqTimeoutHandler(t, countingFactory, rtm)

	millis := int64(200) // generous margin for chunk 0's pre-commit check to pass
	opCtx, cancel := common.WithRequestTimeout(ctx, millis)
	defer cancel()

	commits := 0
	rtm.onCommit = func(_ context.Context) {
		commits++
		if commits == 1 {
			// Overshoots the 200ms deadline by a wide margin so it has
			// definitely, naturally expired by the time this returns.
			time.Sleep(500 * time.Millisecond)
		}
	}

	items := []CollectionItem{
		{ModelName: "Person", ModelVersion: 1, Payload: json.RawMessage(`{"name":"A","age":1}`)},
		{ModelName: "Person", ModelVersion: 1, Payload: json.RawMessage(`{"name":"B","age":2}`)},
		{ModelName: "Person", ModelVersion: 1, Payload: json.RawMessage(`{"name":"C","age":3}`)},
	}

	results, firstChunkErr := h.runChunkedCreate(opCtx, items, 1)
	if firstChunkErr != nil {
		t.Fatalf("runChunkedCreate returned a request-level error (must not turn a post-first-chunk expiry into one): %v", firstChunkErr)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d entries, want 2 (chunk 0 success + chunk 1 error element); got %+v", len(results), results)
	}
	if results[0].Error != nil {
		t.Fatalf("chunk 0 unexpectedly failed: %+v", results[0].Error)
	}
	if len(results[0].EntityIDs) != 1 {
		t.Fatalf("chunk 0 EntityIDs = %v, want exactly 1 (chunk 0 must be durable)", results[0].EntityIDs)
	}
	if results[1].Error == nil {
		t.Fatal("chunk 1 expected a TRANSACTION_TIMEOUT error element, got none")
	}
	if results[1].Error.Code != common.ErrCodeTransactionTimeout {
		t.Fatalf("chunk 1 error code = %q, want %q", results[1].Error.Code, common.ErrCodeTransactionTimeout)
	}
	if results[1].Error.ChunkIndex != 1 {
		t.Fatalf("chunk 1 error ChunkIndex = %d, want 1", results[1].Error.ChunkIndex)
	}
	if commits != 1 {
		t.Fatalf("commits = %d, want exactly 1 (chunk 1/2 must never be attempted)", commits)
	}
	// The decisive assertion: modelStore.Get runs BEFORE CreateEntityCollection
	// ever begins a transaction or touches the workflow engine, so exactly one
	// Get means chunk 1 was never even entered — caught at the loop head,
	// before any I/O — not merely failed inside CreateEntityCollection's own
	// (separately-existing) engine-level cascade check.
	if got := countingFactory.getCount(); got != 1 {
		t.Fatalf("modelStore.Get called %d times, want exactly 1 — chunk 1 must be caught at the loop head, never entering CreateEntityCollection", got)
	}
}

// TestCreateEntity_MemoryBackend_PreExpiredDeadline408 is a regression guard
// on the real memory plugin store: memory has no cancellable syscall, so
// nothing in its own read/write path would ever notice an already-expired
// ctx on its own. The only thing standing between that and a silently
// "successful" write past the client's requested deadline is the pre-commit
// check in txScope.Commit (D2/D8). This drives that path end to end with the
// real backend instead of a mock, waiting on the deadline's own Done signal
// rather than a wall-clock sleep.
func TestCreateEntity_MemoryBackend_PreExpiredDeadline408(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	realTxMgr := mustTxMgr(t, realFactory)
	rtm := &recordingTxMgr{TransactionManager: realTxMgr}

	h, ctx := newReqTimeoutHandler(t, realFactory, rtm)

	opCtx, cancel := common.WithRequestTimeout(ctx, 1)
	defer cancel()
	<-opCtx.Done()

	_, err := h.CreateEntity(opCtx, CreateEntityInput{
		EntityName:   "Person",
		ModelVersion: "1",
		Format:       "JSON",
		Data:         json.RawMessage(`{"name":"A","age":1}`),
	})
	if err == nil {
		t.Fatal("expected an error for a pre-expired deadline, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error chain does not contain context.DeadlineExceeded: %v", err)
	}

	rtm.mu.Lock()
	committed := rtm.committed
	rtm.mu.Unlock()
	if len(committed) != 0 {
		t.Fatalf("commit ran despite the pre-expired deadline: %v", committed)
	}
}

// cancelingEntityStore wraps a real spi.EntityStore; after its Nth Delete
// call it invokes cancel() before returning, so the loop's next iteration
// observes an already-cancelled ctx.
type cancelingEntityStore struct {
	spi.EntityStore
	mu       sync.Mutex
	deletes  int
	cancel   context.CancelFunc
	cancelAt int
}

func (s *cancelingEntityStore) Delete(ctx context.Context, id string) error {
	err := s.EntityStore.Delete(ctx, id)
	s.mu.Lock()
	s.deletes++
	n := s.deletes
	s.mu.Unlock()
	if n == s.cancelAt {
		s.cancel()
	}
	return err
}

func (s *cancelingEntityStore) deleteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deletes
}

// Iterate passes through to the embedded real store. spi.EntityStore is
// embedded by interface value, which does NOT promote spi.Iterable's
// Iterate method (a separate, optional interface) onto *cancelingEntityStore
// even though the underlying concrete store implements it — an explicit
// passthrough is required so DeleteEntitiesConditional's own
// entityStore.(spi.Iterable) capability check succeeds against this spy the
// same way it does against the real store.
func (s *cancelingEntityStore) Iterate(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
	return s.EntityStore.(spi.Iterable).Iterate(ctx, model, filter, opts)
}

// cancelingEntityFactory hands out the given cancelingEntityStore from
// EntityStore(); every other accessor delegates to the wrapped factory.
type cancelingEntityFactory struct {
	spi.StoreFactory
	store *cancelingEntityStore
}

func (f *cancelingEntityFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.store, nil
}

// searchStubEntityStore wraps a real spi.EntityStore and implements
// spi.Searcher by returning a fixed result set, standing in for the search
// service's normal query path (mirrors the package's searcherEntityStore
// pattern in service_test.go, duplicated here since that one lives in
// entity_test and this file needs a same-package handle on unexported types).
type searchStubEntityStore struct {
	spi.EntityStore
	results []*spi.Entity
}

func (s *searchStubEntityStore) Search(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
	return s.results, nil
}

type searchStubEntityFactory struct {
	spi.StoreFactory
	store *searchStubEntityStore
}

func (f *searchStubEntityFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.store, nil
}

// TestDeleteEntitiesConditional_CtxCancelledMidLoop_RollsBackFailClosed pins
// the brief's explicit fail-closed requirement: on a ctx error inside the
// per-id delete loop, the enclosing IIFE must fail with the error (so the
// pending tx rolls back), NOT break out of the loop and let the existing
// commit path run with whatever partial progress was made. Break-and-commit
// would durably remove some matched entities while silently leaving others —
// a partial delete masquerading as either full success or a clean no-op,
// violating correctness-over-availability. This drives real Delete calls
// against the real memory backend (buffered per tx, applied only on Commit)
// so the assertion is "did the store actually still hold the goods after
// rollback", not just "was the right error code returned".
func TestDeleteEntitiesConditional_CtxCancelledMidLoop_RollsBackFailClosed(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	realTxMgr := mustTxMgr(t, realFactory)

	hCreate, ctx := newReqTimeoutHandler(t, realFactory, realTxMgr)

	// Create 3 real, durable entities to match and (attempt to) delete.
	ids := make([]string, 0, 3)
	for _, age := range []int{1, 2, 3} {
		res, err := hCreate.CreateEntity(ctx, CreateEntityInput{
			EntityName:   "Person",
			ModelVersion: "1",
			Format:       "JSON",
			Data:         json.RawMessage(fmt.Sprintf(`{"name":"P","age":%d}`, age)),
		})
		if err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		ids = append(ids, res.EntityIDs[0])
	}

	realEntityStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	matched := make([]*spi.Entity, 0, len(ids))
	for _, id := range ids {
		e, err := realEntityStore.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		matched = append(matched, e)
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Cancels ctx right after the FIRST delete lands in the tx buffer, so the
	// loop's second iteration head sees an already-cancelled ctx before ever
	// touching the second or third entity.
	cancelingStore := &cancelingEntityStore{EntityStore: realEntityStore, cancel: cancel, cancelAt: 1}
	deleteFactory := &cancelingEntityFactory{StoreFactory: realFactory, store: cancelingStore}

	searchStubStore := &searchStubEntityStore{EntityStore: realEntityStore, results: matched}
	searchFactory := &searchStubEntityFactory{StoreFactory: realFactory, store: searchStubStore}
	searchStore, err := realFactory.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	searchSvc := search.NewSearchService(searchFactory, common.NewDefaultUUIDGenerator(), searchStore)

	hDelete := New(deleteFactory, realTxMgr, common.NewDefaultUUIDGenerator(), nil, txgate.New(), searchSvc)

	cond := []byte(`{"type":"simple","jsonPath":"$.age","operatorType":"GREATER_OR_EQUAL","value":0}`)
	_, delErr := hDelete.DeleteEntitiesConditional(cancelCtx, "Person", "1", cond, nil, true, 0)
	if delErr == nil {
		t.Fatal("expected an error from the cancelled-mid-loop delete, got nil")
	}

	if got := cancelingStore.deleteCount(); got != 1 {
		t.Fatalf("Delete called %d times, want exactly 1 (fail-closed must stop at the first cancellation observation, not attempt every matched id)", got)
	}

	// Decisive assertion: fail-closed means the tx never committed, so ALL
	// three entities — including the one whose Delete call landed in the
	// buffer before cancellation — must still be readable, not partially
	// removed.
	for _, id := range ids {
		if _, err := realEntityStore.Get(ctx, id); err != nil {
			t.Fatalf("entity %s missing after rollback (fail-closed was violated — a partial delete was committed): %v", id, err)
		}
	}
}

// --- Fix-up: pin the per-item/per-chunk checks that runChunkedCreate's and
// DeleteEntitiesConditional's tests above never actually drove, per review
// finding (three of the five D9 sites had correct code but no test that
// failed without it: CreateEntityCollection's and UpdateEntityCollection's
// own per-item loop checks, and UpdateCollection handler's chunk loop). ---

// fixedEntityStoreFactory wraps a real spi.StoreFactory but always hands out
// the given store from EntityStore(); every other accessor delegates.
// Generic over the store type (unlike blockingFactory/cancelingEntityFactory
// above, which are each pinned to one concrete wrapper) so it is reusable
// across the hook shapes the tests below need.
type fixedEntityStoreFactory struct {
	spi.StoreFactory
	store spi.EntityStore
}

func (f *fixedEntityStoreFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.store, nil
}

// cancelAfterNSaves wraps a real spi.EntityStore; after its Nth Save call it
// invokes cancel() before returning, so the caller's NEXT loop iteration
// observes an already-cancelled ctx. Records the entity ID of every
// successful Save so a test can verify none of them survive a rollback.
type cancelAfterNSaves struct {
	spi.EntityStore
	mu       sync.Mutex
	saves    int
	savedIDs []string
	cancel   context.CancelFunc
	cancelAt int
}

func (s *cancelAfterNSaves) Save(ctx context.Context, e *spi.Entity) (int64, error) {
	v, err := s.EntityStore.Save(ctx, e)
	s.mu.Lock()
	s.saves++
	n := s.saves
	if err == nil {
		s.savedIDs = append(s.savedIDs, e.Meta.ID)
	}
	s.mu.Unlock()
	if n == s.cancelAt {
		s.cancel()
	}
	return v, err
}

func (s *cancelAfterNSaves) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

// instrumentedEngineFactory wraps a real spi.StoreFactory, serving a fixed
// EntityStore and counting every StateMachineAuditStore accessor call.
//
// This is the piece that makes the Create/Update per-item tests below
// actually decisive. Both CreateEntityCollection and UpdateEntityCollection
// call the REAL workflow engine's Execute per item — and Execute's cascade
// loop (internal/domain/workflow/engine.go's cascadeAutomated) already has
// its own, separately-existing "Spec D9" ctx.Err() check at the top of its
// loop, added by an earlier task, that fires unconditionally on every
// Execute call (even for a model with no imported workflow — a synthetic
// default workflow still runs one cascade iteration). Since the same
// already-cancelled ctx flows into both checks, a naive test asserting only
// "Save was never called for item 2" or "an error came back" passes
// identically whether THIS task's per-item loop-head check fired, or
// whether it was skipped entirely and the engine's own check caught it one
// level down instead — that redundancy is real, but it means such a test
// doesn't actually pin the code this task added.
//
// engine.Execute resolves its audit store via e.factory.StateMachineAuditStore
// exactly once, right at the top, strictly BEFORE it reaches resolveWorkflow
// or cascadeAutomated's own check. So the audit-accessor call count is a
// clean proxy for "was Execute entered at all for this item" — the one
// signal that actually distinguishes the two checks. It requires
// constructing the engine against this same wrapping factory (not reusing
// newReqTimeoutHandler's already-built one, which is bound to the plain,
// unwrapped factory).
type instrumentedEngineFactory struct {
	spi.StoreFactory
	entityStore spi.EntityStore
	mu          sync.Mutex
	auditCalls  int
}

func (f *instrumentedEngineFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.entityStore, nil
}

func (f *instrumentedEngineFactory) StateMachineAuditStore(ctx context.Context) (spi.StateMachineAuditStore, error) {
	f.mu.Lock()
	f.auditCalls++
	f.mu.Unlock()
	return f.StoreFactory.StateMachineAuditStore(ctx)
}

func (f *instrumentedEngineFactory) auditCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auditCalls
}

// TestCreateEntityCollection_CtxCancelledAfterFirstItem_StopsEarlyFailClosed
// pins CreateEntityCollection's OWN per-item loop check directly — not via
// runChunkedCreate, whose test above always used window=1, so cancellation
// was caught one level up (the chunk loop) before CreateEntityCollection's
// per-item loop ever got a second item to observe it. Calling
// CreateEntityCollection directly with a 3-item batch is the only way to
// actually exercise its own check — and, per instrumentedEngineFactory's
// doc comment above, the audit-accessor count is what makes this actually
// decisive rather than redundant with the engine's own equivalent check.
func TestCreateEntityCollection_CtxCancelledAfterFirstItem_StopsEarlyFailClosed(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	realTxMgr := mustTxMgr(t, realFactory)

	// Reuse newReqTimeoutHandler purely to import+lock the Person model on
	// realFactory. Its own handler/engine are discarded below in favor of
	// ones bound to instrumentedEngineFactory — the model data lives in
	// realFactory's shared underlying maps, so re-wrapping afterward is fine.
	_, ctx := newReqTimeoutHandler(t, realFactory, realTxMgr)

	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	realEntityStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	saveStore := &cancelAfterNSaves{EntityStore: realEntityStore, cancel: cancel, cancelAt: 1}
	instrumented := &instrumentedEngineFactory{StoreFactory: realFactory, entityStore: saveStore}
	engine := wfengine.NewEngine(instrumented, common.NewDefaultUUIDGenerator(), realTxMgr)
	h := New(instrumented, realTxMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), nil)

	items := []CollectionItem{
		{ModelName: "Person", ModelVersion: 1, Payload: json.RawMessage(`{"name":"A","age":1}`)},
		{ModelName: "Person", ModelVersion: 1, Payload: json.RawMessage(`{"name":"B","age":2}`)},
		{ModelName: "Person", ModelVersion: 1, Payload: json.RawMessage(`{"name":"C","age":3}`)},
	}

	result, err := h.CreateEntityCollection(cancelCtx, items)
	if err == nil {
		t.Fatal("expected an error from the cancelled-mid-loop create, got nil")
	}
	if result != nil {
		t.Fatalf("expected nil result on failure, got %+v", result)
	}
	if got := saveStore.saveCount(); got != 1 {
		t.Fatalf("Save called %d times, want exactly 1 (item 1 must never reach Save)", got)
	}
	// Decisive: item 1's engine.Execute must never even have been entered —
	// not just failed once inside it (see instrumentedEngineFactory doc).
	if got := instrumented.auditCallCount(); got != 1 {
		t.Fatalf("engine.Execute's audit-store accessor was called %d times, want exactly 1 (item 1's own per-item loop-head check must stop CreateEntityCollection before engine.Execute is ever entered for item 2/3)", got)
	}

	// Fail-closed: item 0's Save landed in the tx buffer but the tx never
	// committed (the function returned before reaching the post-loop commit
	// block), so the deferred scope.Release() must have rolled it back.
	if len(saveStore.savedIDs) != 1 {
		t.Fatalf("savedIDs = %v, want exactly 1 entry", saveStore.savedIDs)
	}
	if _, getErr := realEntityStore.Get(ctx, saveStore.savedIDs[0]); getErr == nil {
		t.Fatalf("entity %s is readable after the batch failed — rollback did not happen (fail-closed violated)", saveStore.savedIDs[0])
	} else if !errors.Is(getErr, spi.ErrNotFound) {
		t.Fatalf("Get(%s) = %v, want spi.ErrNotFound", saveStore.savedIDs[0], getErr)
	}
}

// TestUpdateEntityCollection_CtxCancelledAfterFirstItem_StopsEarlyFailClosed
// is UpdateEntityCollection's own-entry-point sibling to the Create test
// above, for the same reason (and the same instrumentedEngineFactory
// decisiveness rationale): the review finding was that no test drove
// UpdateEntityCollection's per-item loop check directly.
func TestUpdateEntityCollection_CtxCancelledAfterFirstItem_StopsEarlyFailClosed(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	realTxMgr := mustTxMgr(t, realFactory)

	h, ctx := newReqTimeoutHandler(t, realFactory, realTxMgr)

	// Seed 3 real entities to update, via the ORIGINAL (unwrapped) handler —
	// the wrapping factory below is only swapped in for the UpdateEntityCollection
	// call under test.
	ids := make([]string, 0, 3)
	for _, age := range []int{1, 2, 3} {
		res, err := h.CreateEntity(ctx, CreateEntityInput{
			EntityName: "Person", ModelVersion: "1", Format: "JSON",
			Data: json.RawMessage(fmt.Sprintf(`{"name":"orig","age":%d}`, age)),
		})
		if err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		ids = append(ids, res.EntityIDs[0])
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	realEntityStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	saveStore := &cancelAfterNSaves{EntityStore: realEntityStore, cancel: cancel, cancelAt: 1}
	instrumented := &instrumentedEngineFactory{StoreFactory: realFactory, entityStore: saveStore}
	engine := wfengine.NewEngine(instrumented, common.NewDefaultUUIDGenerator(), realTxMgr)
	hUpdate := New(instrumented, realTxMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), nil)

	items := []UpdateCollectionItem{
		{EntityID: ids[0], Payload: json.RawMessage(`{"name":"X"}`)},
		{EntityID: ids[1], Payload: json.RawMessage(`{"name":"Y"}`)},
		{EntityID: ids[2], Payload: json.RawMessage(`{"name":"Z"}`)},
	}

	result, err := hUpdate.UpdateEntityCollection(cancelCtx, items)
	if err == nil {
		t.Fatal("expected an error from the cancelled-mid-loop update, got nil")
	}
	if result != nil {
		t.Fatalf("expected nil result on failure, got %+v", result)
	}
	if got := saveStore.saveCount(); got != 1 {
		t.Fatalf("Save called %d times, want exactly 1 (item 1 must never reach Save)", got)
	}
	// Decisive: item 1's engine.Execute must never even have been entered.
	if got := instrumented.auditCallCount(); got != 1 {
		t.Fatalf("engine.Execute's audit-store accessor was called %d times, want exactly 1 (item 1's own per-item loop-head check must stop UpdateEntityCollection before engine.Execute is ever entered for item 2/3)", got)
	}

	// Fail-closed: item 0's update landed in the tx buffer but never
	// committed; the entity must still read back its ORIGINAL payload.
	e0, getErr := realEntityStore.Get(ctx, ids[0])
	if getErr != nil {
		t.Fatalf("Get(%s): %v", ids[0], getErr)
	}
	var payload struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(e0.Data, &payload); err != nil {
		t.Fatalf("decode entity 0 payload: %v; data=%s", err, e0.Data)
	}
	if payload.Name != "orig" {
		t.Fatalf("entity 0 name = %q after rollback, want \"orig\" (fail-closed violated — the buffered update was committed)", payload.Name)
	}
}

// TestUpdateCollection_HandlerChunkLoop_CtxCancelledAfterFirstChunkCommit
// pins the UpdateCollection HTTP handler's own chunk loop check — the review
// finding was that no test drove it either. window=1 with 3 items forces 3
// chunks; the fake txMgr's Commit hook cancels the request ctx right after
// chunk 0's commit lands, so chunk 1's loop-head check must catch it before
// ever calling UpdateEntityCollection for chunk 1 — routed through the same
// per-chunk error-element shape (collectionChunkError) the handler's
// existing chunk-failure branch already produces, per that endpoint's
// contract (matches D3: chunk 0 stays durable, chunk 2 is never attempted).
func TestUpdateCollection_HandlerChunkLoop_CtxCancelledAfterFirstChunkCommit(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	realTxMgr := mustTxMgr(t, realFactory)
	rtm := &recordingTxMgr{TransactionManager: realTxMgr}

	h, ctx := newReqTimeoutHandler(t, realFactory, rtm)

	// Seed 3 real entities to update.
	ids := make([]string, 0, 3)
	for _, age := range []int{1, 2, 3} {
		res, err := h.CreateEntity(ctx, CreateEntityInput{
			EntityName: "Person", ModelVersion: "1", Format: "JSON",
			Data: json.RawMessage(fmt.Sprintf(`{"name":"orig","age":%d}`, age)),
		})
		if err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		ids = append(ids, res.EntityIDs[0])
	}
	// Baseline AFTER seeding (each seed CreateEntity call begins its own tx
	// too) — the decisive assertion below counts Begin calls from here.
	beginsBeforeUpdate := rtm.beginCount()

	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	commits := 0
	rtm.onCommit = func(_ context.Context) {
		commits++
		if commits == 1 {
			cancel()
		}
	}

	body := fmt.Sprintf(
		`[{"id":%q,"payload":"{\"name\":\"X\"}"},{"id":%q,"payload":"{\"name\":\"Y\"}"},{"id":%q,"payload":"{\"name\":\"Z\"}"}]`,
		ids[0], ids[1], ids[2],
	)
	window := int32(1)
	r := httptest.NewRequest(http.MethodPut, "/entity/JSON", strings.NewReader(body)).WithContext(cancelCtx)
	w := httptest.NewRecorder()
	h.UpdateCollection(w, r, genapi.UpdateCollectionParamsFormatJSON, genapi.UpdateCollectionParams{TransactionWindow: &window})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (partial-progress shape); body: %s", w.Code, w.Body.String())
	}
	var results []collectionChunkResult
	if err := json.Unmarshal(w.Body.Bytes(), &results); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, w.Body.String())
	}
	if len(results) != 2 {
		t.Fatalf("results = %d entries, want 2 (chunk 0 success + chunk 1 error element); got %+v", len(results), results)
	}
	if results[0].Error != nil {
		t.Fatalf("chunk 0 unexpectedly failed: %+v", results[0].Error)
	}
	if len(results[0].EntityIDs) != 1 {
		t.Fatalf("chunk 0 EntityIDs = %v, want exactly 1", results[0].EntityIDs)
	}
	if results[1].Error == nil {
		t.Fatal("chunk 1 expected an error element (matching the endpoint's existing per-chunk contract), got none")
	}
	if results[1].Error.ChunkIndex != 1 {
		t.Fatalf("chunk 1 error ChunkIndex = %d, want 1", results[1].Error.ChunkIndex)
	}
	if commits != 1 {
		t.Fatalf("commits = %d, want exactly 1 (chunk 2 must never be attempted)", commits)
	}
	// Decisive: UpdateEntityCollection must never even have been CALLED for
	// chunk 1. This matters because UpdateEntityCollection has its OWN
	// per-item loop-head check (this same task, tested directly above) —
	// if the HANDLER's chunk-loop check were removed but UpdateEntityCollection
	// were still called for chunk 1, that inner check would immediately catch
	// the same already-cancelled ctx at item index 0 and produce an
	// indistinguishable error-element result, masking whether the handler's
	// own check fired. Begin is the first thing UpdateEntityCollection's
	// beginScope does, so "no new Begin beyond the seed calls + chunk 0"
	// proves chunk 1's call never happened at all.
	if got := rtm.beginCount() - beginsBeforeUpdate; got != 1 {
		t.Fatalf("Begin called %d times during the update (want exactly 1, for chunk 0) — chunk 1 must never call UpdateEntityCollection at all, not merely fail inside it", got)
	}

	// Chunk 0 durable: entity 0 really has the new payload.
	realEntityStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	e0, getErr := realEntityStore.Get(ctx, ids[0])
	if getErr != nil {
		t.Fatalf("Get(%s): %v", ids[0], getErr)
	}
	var payload struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(e0.Data, &payload); err != nil {
		t.Fatalf("decode entity 0 payload: %v; data=%s", err, e0.Data)
	}
	if payload.Name != "X" {
		t.Fatalf("entity 0 name = %q, want \"X\" (chunk 0's commit must be durable)", payload.Name)
	}

	// Chunks 1/2 never attempted: entities 1 and 2 keep their ORIGINAL payload.
	for i := 1; i < 3; i++ {
		e, getErr := realEntityStore.Get(ctx, ids[i])
		if getErr != nil {
			t.Fatalf("Get(%s): %v", ids[i], getErr)
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(e.Data, &p); err != nil {
			t.Fatalf("decode entity %d payload: %v; data=%s", i, err, e.Data)
		}
		if p.Name != "orig" {
			t.Fatalf("entity %d name = %q, want \"orig\" (chunk %d must never have been attempted)", i, p.Name, i)
		}
	}
}
