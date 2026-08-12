package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// --- fakes ---

// fakeDeleteBatchStore records every DeleteBatch call (as a defensive copy of
// the ids slice, since the handler must not retain aliasing across chunks)
// and fails the call whose 0-based index is present in failOn.
type fakeDeleteBatchStore struct {
	spi.MessageStore
	mu     sync.Mutex
	calls  [][]string
	failOn map[int]error
}

func (s *fakeDeleteBatchStore) DeleteBatch(_ context.Context, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := len(s.calls)
	cp := append([]string(nil), ids...)
	s.calls = append(s.calls, cp)
	return s.failOn[idx]
}

func (s *fakeDeleteBatchStore) snapshot() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.calls))
	copy(out, s.calls)
	return out
}

// fixedDeleteBatchFactory hands out a fixed MessageStore; every other
// accessor delegates to the wrapped factory unchanged.
type fixedDeleteBatchFactory struct {
	spi.StoreFactory
	store spi.MessageStore
}

func (f *fixedDeleteBatchFactory) MessageStore(_ context.Context) (spi.MessageStore, error) {
	return f.store, nil
}

// --- helpers ---

func newDeleteBatchHandler(store spi.MessageStore) *Handler {
	factory := &fixedDeleteBatchFactory{StoreFactory: memory.NewStoreFactory(), store: store}
	return New(factory, common.NewDefaultUUIDGenerator())
}

func newDeleteMessagesRequest(ctx context.Context, body string) *http.Request {
	r := httptest.NewRequest(http.MethodDelete, "/message", strings.NewReader(body))
	if ctx != nil {
		r = r.WithContext(ctx)
	}
	return r
}

func genIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = uuid.New().String()
	}
	return ids
}

func idsBody(ids []string) string {
	b, _ := json.Marshal(ids)
	return string(b)
}

func int32ptr(v int32) *int32 { return &v }

// --- Step 1 (RED) tests ---

// (a1) transactionSize=0 -> 400.
func TestDeleteMessages_TransactionSizeZero_400(t *testing.T) {
	store := &fakeDeleteBatchStore{}
	h := newDeleteBatchHandler(store)

	ids := genIDs(3)
	w := httptest.NewRecorder()
	h.DeleteMessages(w, newDeleteMessagesRequest(nil, idsBody(ids)), genapi.DeleteMessagesParams{TransactionSize: int32ptr(0)})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	if len(store.snapshot()) != 0 {
		t.Fatalf("DeleteBatch must not be called on validation failure; got %d calls", len(store.snapshot()))
	}
}

// (a2) joined transaction + transactionSize present -> 400 (uniform rule, spec D7).
func TestDeleteMessages_JoinedTransaction_Rejected400(t *testing.T) {
	store := &fakeDeleteBatchStore{}
	h := newDeleteBatchHandler(store)

	ids := genIDs(3)
	ctx := spi.WithTransaction(context.Background(), &spi.TransactionState{ID: "tx-1"})
	w := httptest.NewRecorder()
	h.DeleteMessages(w, newDeleteMessagesRequest(ctx, idsBody(ids)), genapi.DeleteMessagesParams{TransactionSize: int32ptr(2)})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	if len(store.snapshot()) != 0 {
		t.Fatalf("DeleteBatch must not be called on validation failure; got %d calls", len(store.snapshot()))
	}
}

// (b) 5 ids, transactionSize=2 -> 3 calls [2,2,1], 200 with 3 elements all success:true.
func TestDeleteMessages_Batched_Success(t *testing.T) {
	store := &fakeDeleteBatchStore{}
	h := newDeleteBatchHandler(store)

	ids := genIDs(5)
	w := httptest.NewRecorder()
	h.DeleteMessages(w, newDeleteMessagesRequest(nil, idsBody(ids)), genapi.DeleteMessagesParams{TransactionSize: int32ptr(2)})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	calls := store.snapshot()
	if len(calls) != 3 {
		t.Fatalf("DeleteBatch call count = %d, want 3", len(calls))
	}
	wantSizes := []int{2, 2, 1}
	for i, want := range wantSizes {
		if len(calls[i]) != want {
			t.Errorf("call %d size = %d, want %d", i, len(calls[i]), want)
		}
	}

	var resp []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, w.Body.String())
	}
	if len(resp) != 3 {
		t.Fatalf("response element count = %d, want 3", len(resp))
	}
	for i, el := range resp {
		if el["success"] != true {
			t.Errorf("element %d success = %v, want true", i, el["success"])
		}
		entityIds, ok := el["entityIds"].([]any)
		if !ok || len(entityIds) != wantSizes[i] {
			t.Errorf("element %d entityIds = %v, want %d ids", i, el["entityIds"], wantSizes[i])
		}
	}
}

// (c) same as (b) but chunk index 1 (the second call) fails -> 200, elements
// success = [true, false, true], and the third chunk was STILL attempted.
func TestDeleteMessages_Batched_PartialFailure_LaterChunksStillAttempted(t *testing.T) {
	store := &fakeDeleteBatchStore{failOn: map[int]error{1: errors.New("boom")}}
	h := newDeleteBatchHandler(store)

	ids := genIDs(5)
	w := httptest.NewRecorder()
	h.DeleteMessages(w, newDeleteMessagesRequest(nil, idsBody(ids)), genapi.DeleteMessagesParams{TransactionSize: int32ptr(2)})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	calls := store.snapshot()
	if len(calls) != 3 {
		t.Fatalf("DeleteBatch call count = %d, want 3 (later chunks must still be attempted)", len(calls))
	}

	var resp []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, w.Body.String())
	}
	if len(resp) != 3 {
		t.Fatalf("response element count = %d, want 3", len(resp))
	}
	wantSuccess := []bool{true, false, true}
	for i, want := range wantSuccess {
		if resp[i]["success"] != want {
			t.Errorf("element %d success = %v, want %v", i, resp[i]["success"], want)
		}
	}
}

// (c2) ctx is cancelled by the store while chunk 1 is in flight -> the loop
// must not attempt chunk 2/3 (call count stays 1) and must fail the whole
// request (5xx), never fabricate elements for chunks that were never
// attempted. Mirrors the entity-side batch loop's between-batches ctx check
// (internal/domain/entity/service.go deleteBatched): fail closed on the
// response, even though chunk 1's delete is already durable.
func TestDeleteMessages_CtxCancelledBetweenChunks_AbortsRemainingChunks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &cancelOnFirstCallDeleteBatchStore{cancel: cancel}
	h := newDeleteBatchHandler(store)

	ids := genIDs(5)
	w := httptest.NewRecorder()
	h.DeleteMessages(w, newDeleteMessagesRequest(ctx, idsBody(ids)), genapi.DeleteMessagesParams{TransactionSize: int32ptr(2)})

	if w.Code < 500 {
		t.Fatalf("status = %d, want 5xx (fail closed on cancellation); body: %s", w.Code, w.Body.String())
	}

	calls := store.snapshot()
	if len(calls) != 1 {
		t.Fatalf("DeleteBatch call count = %d, want 1 (chunks 2/3 must never be attempted after cancellation)", len(calls))
	}
}

// cancelOnFirstCallDeleteBatchStore records calls like fakeDeleteBatchStore
// but cancels the request ctx (via cancel) right after its first DeleteBatch
// call returns, simulating the client disconnecting mid-batch.
type cancelOnFirstCallDeleteBatchStore struct {
	spi.MessageStore
	cancel context.CancelFunc
	mu     sync.Mutex
	calls  [][]string
}

func (s *cancelOnFirstCallDeleteBatchStore) DeleteBatch(_ context.Context, ids []string) error {
	s.mu.Lock()
	cp := append([]string(nil), ids...)
	s.calls = append(s.calls, cp)
	s.mu.Unlock()
	s.cancel()
	return nil
}

func (s *cancelOnFirstCallDeleteBatchStore) snapshot() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.calls))
	copy(out, s.calls)
	return out
}

// (d) no transactionSize param + store error -> 500 (unchanged existing behavior).
func TestDeleteMessages_NoParam_StoreError_500(t *testing.T) {
	store := &fakeDeleteBatchStore{failOn: map[int]error{0: errors.New("boom")}}
	h := newDeleteBatchHandler(store)

	ids := genIDs(3)
	w := httptest.NewRecorder()
	h.DeleteMessages(w, newDeleteMessagesRequest(nil, idsBody(ids)), genapi.DeleteMessagesParams{})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
	calls := store.snapshot()
	if len(calls) != 1 {
		t.Fatalf("DeleteBatch call count = %d, want 1 (single-call path, unchanged)", len(calls))
	}
}

// (e) no transactionSize param + success -> single DeleteBatch call, single
// response element echoing all ids — unchanged response shape.
func TestDeleteMessages_NoParam_Success_SingleCallSingleElement(t *testing.T) {
	store := &fakeDeleteBatchStore{}
	h := newDeleteBatchHandler(store)

	ids := genIDs(3)
	w := httptest.NewRecorder()
	h.DeleteMessages(w, newDeleteMessagesRequest(nil, idsBody(ids)), genapi.DeleteMessagesParams{})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	calls := store.snapshot()
	if len(calls) != 1 {
		t.Fatalf("DeleteBatch call count = %d, want 1", len(calls))
	}
	if len(calls[0]) != 3 {
		t.Fatalf("single call ids count = %d, want 3", len(calls[0]))
	}

	var resp []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, w.Body.String())
	}
	if len(resp) != 1 {
		t.Fatalf("response element count = %d, want 1", len(resp))
	}
	if resp[0]["success"] != true {
		t.Errorf("success = %v, want true", resp[0]["success"])
	}
	entityIds, ok := resp[0]["entityIds"].([]any)
	if !ok || len(entityIds) != 3 {
		t.Errorf("entityIds = %v, want 3 ids", resp[0]["entityIds"])
	}
}
