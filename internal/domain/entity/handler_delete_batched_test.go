package entity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// --- Task 10: DeleteEntities handler transactionSize resolution ---

func txSizePtr(v int32) *int32 { return &v }

// TestDeleteEntities_TransactionSizeZero_400 pins that transactionSize<1 is
// rejected before the body is even read.
func TestDeleteEntities_TransactionSizeZero_400(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	h, ctx := newReqTimeoutHandler(t, factory, mustTxMgr(t, factory))

	r := httptest.NewRequest(http.MethodDelete, "/entity/Person/1", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.DeleteEntities(w, r, "Person", 1, genapi.DeleteEntitiesParams{TransactionSize: txSizePtr(0)})

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
}

// TestDeleteEntities_JoinedTransaction_Rejected400 pins spec D7: a request
// carrying a joined transaction (spi.GetTransaction(ctx) != nil — how a
// routed compute-node callback presents at param-resolution time) is
// rejected with 400 rather than silently honoring or ignoring
// transactionSize. The response names the param and explains why.
func TestDeleteEntities_JoinedTransaction_Rejected400(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	h, ctx := newReqTimeoutHandler(t, factory, mustTxMgr(t, factory))
	joinedCtx := spi.WithTransaction(ctx, &spi.TransactionState{ID: "tx-1"})

	r := httptest.NewRequest(http.MethodDelete, "/entity/Person/1", nil).WithContext(joinedCtx)
	w := httptest.NewRecorder()
	h.DeleteEntities(w, r, "Person", 1, genapi.DeleteEntitiesParams{TransactionSize: txSizePtr(2)})

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
	if !strings.Contains(pd.Detail, "transactionSize") {
		t.Fatalf("body does not mention the param name: %q", pd.Detail)
	}
	if !strings.Contains(pd.Detail, "joins an open transaction") {
		t.Fatalf("body does not mention the joined transaction: %q", pd.Detail)
	}
	if pd.Properties["errorCode"] != common.ErrCodeBadRequest {
		t.Fatalf("errorCode = %v, want %v", pd.Properties["errorCode"], common.ErrCodeBadRequest)
	}
}

// TestDeleteEntities_TransactionSize_HappyPath pins the batched response
// shape end to end through the HTTP handler: transactionSize=2 over 3 seeded
// entities with verbose=true removes all 3, reports matched/removed counts,
// an empty idToError, and the full id list.
func TestDeleteEntities_TransactionSize_HappyPath(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	h, ctx := newReqTimeoutHandler(t, factory, mustTxMgr(t, factory))
	ids := seedPersons(t, h, ctx, 3)

	r := httptest.NewRequest(http.MethodDelete, "/entity/Person/1", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.DeleteEntities(w, r, "Person", 1, genapi.DeleteEntitiesParams{
		TransactionSize: txSizePtr(2),
		Verbose:         boolPtr(true),
	})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		EntityModelClassID string `json:"entityModelClassId"`
		DeleteResult       struct {
			IDToError                map[string]string `json:"idToError"`
			NumberOfEntitites        int               `json:"numberOfEntitites"`
			NumberOfEntititesRemoved int               `json:"numberOfEntititesRemoved"`
		} `json:"deleteResult"`
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; body: %s", err, w.Body.String())
	}
	if resp.DeleteResult.NumberOfEntitites != 3 {
		t.Errorf("numberOfEntitites = %d, want 3", resp.DeleteResult.NumberOfEntitites)
	}
	if resp.DeleteResult.NumberOfEntititesRemoved != 3 {
		t.Errorf("numberOfEntititesRemoved = %d, want 3", resp.DeleteResult.NumberOfEntititesRemoved)
	}
	if len(resp.DeleteResult.IDToError) != 0 {
		t.Errorf("idToError = %v, want empty", resp.DeleteResult.IDToError)
	}
	if len(resp.IDs) != 3 {
		t.Errorf("ids = %v, want 3 entries", resp.IDs)
	}

	entityStore, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	for _, id := range ids {
		if _, gErr := entityStore.Get(ctx, id); gErr == nil {
			t.Errorf("entity %s still exists after batched delete", id)
		}
	}
}

func boolPtr(v bool) *bool { return &v }
