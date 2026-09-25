package entity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestCollectionOps_TransactionWindow_JoinedRejected400: transactionWindow
// asks for a commit after every N items. A request that joined an open
// transaction commits nothing — its owner does — so the parameter is refused
// with 400, like transactionSize and transactionTimeoutMillis, instead of
// being honoured with chunk results that report commits that never happened.
func TestCollectionOps_TransactionWindow_JoinedRejected400(t *testing.T) {
	factory := memory.NewStoreFactory()
	txMgr := mustTxMgr(t, factory)
	h, baseCtx := newReqTimeoutHandler(t, factory, txMgr)
	window := int32(10)
	ops := []struct {
		name   string
		invoke func(ctx context.Context) *httptest.ResponseRecorder
	}{
		{"Create (array body)", func(ctx context.Context) *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodPost, "/entity/JSON/Person/1", strings.NewReader(`[{"name":"A","age":1}]`)).WithContext(ctx)
			w := httptest.NewRecorder()
			h.Create(w, r, "JSON", "Person", 1, genapi.CreateParams{TransactionWindow: &window})
			return w
		}},
		{"Create (single object)", func(ctx context.Context) *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodPost, "/entity/JSON/Person/1", strings.NewReader(`{"name":"A","age":1}`)).WithContext(ctx)
			w := httptest.NewRecorder()
			h.Create(w, r, "JSON", "Person", 1, genapi.CreateParams{TransactionWindow: &window})
			return w
		}},
		{"CreateCollection", func(ctx context.Context) *httptest.ResponseRecorder {
			body := `[{"model":{"name":"Person","version":1},"payload":"{\"name\":\"A\",\"age\":1}"}]`
			r := httptest.NewRequest(http.MethodPost, "/entity/JSON", strings.NewReader(body)).WithContext(ctx)
			w := httptest.NewRecorder()
			h.CreateCollection(w, r, "JSON", genapi.CreateCollectionParams{TransactionWindow: &window})
			return w
		}},
		{"UpdateCollection", func(ctx context.Context) *httptest.ResponseRecorder {
			body := fmt.Sprintf(`[{"id":%q,"payload":"{\"name\":\"B\"}"}]`, sampleUUID)
			r := httptest.NewRequest(http.MethodPut, "/entity/JSON", strings.NewReader(body)).WithContext(ctx)
			w := httptest.NewRecorder()
			h.UpdateCollection(w, r, genapi.UpdateCollectionParamsFormatJSON, genapi.UpdateCollectionParams{TransactionWindow: &window})
			return w
		}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			ownerTxID, _, err := txMgr.Begin(baseCtx)
			if err != nil {
				t.Fatalf("owner Begin: %v", err)
			}
			t.Cleanup(func() { _ = txMgr.Rollback(baseCtx, ownerTxID) })
			ctx, err := txMgr.Join(baseCtx, ownerTxID)
			if err != nil {
				t.Fatalf("Join: %v", err)
			}
			w := op.invoke(ctx)
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
			if !strings.Contains(pd.Detail, "transactionWindow") || !strings.Contains(pd.Detail, "joins an open transaction") {
				t.Fatalf("detail must name the param and the joined transaction: %q", pd.Detail)
			}
			if pd.Properties["errorCode"] != common.ErrCodeBadRequest {
				t.Fatalf("errorCode = %v, want %v", pd.Properties["errorCode"], common.ErrCodeBadRequest)
			}
		})
	}
}

// TestCollectionOps_JoinedCollection_OneUnit: without transactionWindow, a
// joined collection larger than the default window is not chunked. A failure
// in any item is the request's failure, never a 200 whose chunk results claim
// that the items before it were committed — a joined request commits nothing.
func TestCollectionOps_JoinedCollection_OneUnit(t *testing.T) {
	factory := memory.NewStoreFactory()
	txMgr := mustTxMgr(t, factory)
	h, baseCtx := newReqTimeoutHandler(t, factory, txMgr)

	ownerTxID, _, err := txMgr.Begin(baseCtx)
	if err != nil {
		t.Fatalf("owner Begin: %v", err)
	}
	t.Cleanup(func() { _ = txMgr.Rollback(baseCtx, ownerTxID) })
	ctx, err := txMgr.Join(baseCtx, ownerTxID)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	// collectionDefaultWindow good items, then one whose "age" is not an integer.
	items := make([]string, 0, collectionDefaultWindow+1)
	for i := 0; i < collectionDefaultWindow; i++ {
		items = append(items, `{"model":{"name":"Person","version":1},"payload":"{\"name\":\"A\",\"age\":1}"}`)
	}
	items = append(items, `{"model":{"name":"Person","version":1},"payload":"{\"name\":\"A\",\"age\":\"old\"}"}`)
	r := httptest.NewRequest(http.MethodPost, "/entity/JSON", strings.NewReader("["+strings.Join(items, ",")+"]")).WithContext(ctx)
	w := httptest.NewRecorder()
	h.CreateCollection(w, r, "JSON", genapi.CreateCollectionParams{})

	if w.Code == http.StatusOK {
		t.Fatalf("status 200 for a joined collection whose item %d failed: %s", collectionDefaultWindow, w.Body.String())
	}
	if w.Code < 400 || w.Code >= 500 {
		t.Fatalf("status = %d; want the failing item's 4xx: %s", w.Code, w.Body.String())
	}
}
