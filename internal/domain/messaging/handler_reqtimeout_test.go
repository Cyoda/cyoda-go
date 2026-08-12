package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// --- fakes ---

// blockingFactoryMessageStore wraps a real spi.StoreFactory but overrides
// MessageStore(ctx) to block until ctx is Done, then return ctx.Err() —
// simulating a store-factory resolution that itself observes the client's
// deadline before ever handing back a store. Every other accessor delegates
// to the wrapped factory unchanged. Deterministic by construction: the test
// never races a wall-clock sleep against the deadline, it waits on the
// deadline's own Done signal.
type blockingFactoryMessageStore struct {
	spi.StoreFactory
}

func (f *blockingFactoryMessageStore) MessageStore(ctx context.Context) (spi.MessageStore, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// fixedMessageStoreFactory wraps a real spi.StoreFactory but always hands out
// the given store from MessageStore(); every other accessor delegates.
type fixedMessageStoreFactory struct {
	spi.StoreFactory
	store spi.MessageStore
}

func (f *fixedMessageStoreFactory) MessageStore(_ context.Context) (spi.MessageStore, error) {
	return f.store, nil
}

// shieldAssertingMessageStore's Save sleeps well past the request's own short
// deadline before checking its OWN ctx — the one actually handed to Save —
// for cancellation. Save succeeding without ever observing a cancellation
// there is the decisive evidence that the handler runs the save on a
// shielded context (common.ShieldedCommit), not the raw request-deadline ctx.
type shieldAssertingMessageStore struct {
	spi.MessageStore
	sleep time.Duration
	mu    sync.Mutex
	saved bool
	saw   error
}

func (s *shieldAssertingMessageStore) Save(ctx context.Context, _ string, _ spi.MessageHeader, _ spi.MessageMetaData, _ io.Reader) error {
	time.Sleep(s.sleep)
	s.mu.Lock()
	s.saved = true
	s.saw = ctx.Err()
	s.mu.Unlock()
	return nil
}

func (s *shieldAssertingMessageStore) snapshot() (saved bool, saw error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved, s.saw
}

// --- helpers ---

func newReqTimeoutMessagingHandler(factory spi.StoreFactory) *Handler {
	return New(factory, common.NewDefaultUUIDGenerator())
}

func newMessageRequest(ctx context.Context, subject, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/message/new/"+subject, strings.NewReader(body))
	if ctx != nil {
		r = r.WithContext(ctx)
	}
	return r
}

func decodeProblem(t *testing.T, w *httptest.ResponseRecorder) (detail string, properties map[string]any) {
	t.Helper()
	var pd struct {
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode body: %v; body: %s", err, w.Body.String())
	}
	return pd.Detail, pd.Properties
}

// --- Step 1 (RED) tests ---

// (a) transactionTimeoutMillis=0 -> 400, before any I/O.
func TestNewMessage_TransactionTimeoutMillis_Invalid400(t *testing.T) {
	h := newReqTimeoutMessagingHandler(memory.NewStoreFactory())

	for _, bad := range []int64{0, -1} {
		bad := bad
		t.Run(fmt.Sprintf("millis=%d", bad), func(t *testing.T) {
			w := httptest.NewRecorder()
			h.NewMessage(w, newMessageRequest(nil, "bad.timeout", `{"payload":{"x":1}}`), "bad.timeout",
				genapi.NewMessageParams{TransactionTimeoutMillis: &bad, ContentType: "application/json"})

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
			_, props := decodeProblem(t, w)
			if props["errorCode"] != common.ErrCodeBadRequest {
				t.Fatalf("errorCode = %v, want %v", props["errorCode"], common.ErrCodeBadRequest)
			}
		})
	}
}

// (b) transactionTimeoutMillis with a joined tx already on ctx -> 400,
// uniform rule (spec D7) even though messaging has no tx of its own — the
// txjoin middleware is global.
func TestNewMessage_JoinedTransaction_Rejected400(t *testing.T) {
	h := newReqTimeoutMessagingHandler(memory.NewStoreFactory())

	millis := int64(5000)
	ctx := spi.WithTransaction(context.Background(), &spi.TransactionState{ID: "tx-1"})
	w := httptest.NewRecorder()
	h.NewMessage(w, newMessageRequest(ctx, "joined", `{"payload":{"x":1}}`), "joined",
		genapi.NewMessageParams{TransactionTimeoutMillis: &millis, ContentType: "application/json"})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	detail, props := decodeProblem(t, w)
	if !strings.Contains(detail, "transactionTimeoutMillis") {
		t.Fatalf("body does not mention the param name: %q", detail)
	}
	if !strings.Contains(detail, "joins an open transaction") {
		t.Fatalf("body does not mention the joined transaction: %q", detail)
	}
	if props["errorCode"] != common.ErrCodeBadRequest {
		t.Fatalf("errorCode = %v, want %v", props["errorCode"], common.ErrCodeBadRequest)
	}
}

// (c) pre-expired deadline: the store-factory resolution itself blocks until
// the 1ms deadline naturally fires and returns ctx.Err() — 408
// TRANSACTION_TIMEOUT, and the message store is never even obtained (so
// nothing can have been saved).
func TestNewMessage_PreExpiredDeadline_408NothingSaved(t *testing.T) {
	factory := &blockingFactoryMessageStore{StoreFactory: memory.NewStoreFactory()}
	h := newReqTimeoutMessagingHandler(factory)

	millis := int64(1)
	w := httptest.NewRecorder()
	h.NewMessage(w, newMessageRequest(nil, "pre.expired", `{"payload":{"x":1}}`), "pre.expired",
		genapi.NewMessageParams{TransactionTimeoutMillis: &millis, ContentType: "application/json"})

	if w.Code != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408; body: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	_, props := decodeProblem(t, w)
	if props["errorCode"] != common.ErrCodeTransactionTimeout {
		t.Fatalf("errorCode = %v, want %v", props["errorCode"], common.ErrCodeTransactionTimeout)
	}
	if props["retryable"] != true {
		t.Fatalf("retryable = %v, want true", props["retryable"])
	}
}

// (d) save-wins: once the store is resolved and the pre-Save check has
// passed while the deadline was still live, Save runs on a shielded ctx that
// must show no cancellation even though it sleeps well past the request's
// own short deadline — and the request still succeeds (200).
func TestNewMessage_SaveWins_ShieldedFromDeadline200(t *testing.T) {
	store := &shieldAssertingMessageStore{sleep: 50 * time.Millisecond}
	factory := &fixedMessageStoreFactory{StoreFactory: memory.NewStoreFactory(), store: store}
	h := newReqTimeoutMessagingHandler(factory)

	millis := int64(10) // fires well before the store's 50ms Save sleep completes
	w := httptest.NewRecorder()
	h.NewMessage(w, newMessageRequest(nil, "save.wins", `{"payload":{"x":1}}`), "save.wins",
		genapi.NewMessageParams{TransactionTimeoutMillis: &millis, ContentType: "application/json"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	saved, saw := store.snapshot()
	if !saved {
		t.Fatal("Save was never called")
	}
	if saw != nil {
		t.Fatalf("save ctx observed cancellation (%v); Save must run shielded from the request deadline", saw)
	}
}
