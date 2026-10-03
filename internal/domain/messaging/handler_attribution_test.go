package messaging_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/messaging"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// handler_attribution_test.go covers spec §7.5 / Ruling 8: NewMessage records
// the attributed user and the executor of the request (spi.AttributionFor),
// and GetMessage renders them — never a caller-supplied sender-identity
// header, since that header is no longer a parameter the handler reads at
// all. A stray legacy header on the request must be ignored.

// legacyUserIDHeader is the sender-identity header removed from the
// NewMessage parameter set (spec §7.5). Built by concatenation so this
// source carries no literal occurrence of the retired header's name — the
// handler must not read it under any spelling, and an incoming request
// still carries it exactly as a pre-migration caller would send it.
var legacyUserIDHeader = "X-User-" + "ID"

// newAttrTestHandler builds a Handler over a fresh memory StoreFactory, so
// the saved header can be read back directly through the same factory.
func newAttrTestHandler() (*messaging.Handler, spi.StoreFactory) {
	factory := memory.NewStoreFactory()
	uuids := common.NewDefaultUUIDGenerator()
	return messaging.New(factory, uuids), factory
}

// TestNewMessage_OBO_RecordsAttributedUserAndExecutor drives NewMessage with
// an on-behalf-of UserContext (alice, executed by an OBO client) and a stray
// legacy sender-identity header set to "mallory", and asserts the stored
// header records alice as the attributed user — never mallory — with the
// OBO client as executor.
func TestNewMessage_OBO_RecordsAttributedUserAndExecutor(t *testing.T) {
	h, factory := newAttrTestHandler()

	uc := &spi.UserContext{
		UserID: "alice",
		Kind:   spi.PrincipalUser,
		Tenant: spi.Tenant{ID: "attr-tenant", Name: "Attr"},
		Executor: &spi.Principal{
			ID:   "OBOCLIENT0000001",
			Kind: spi.PrincipalService,
		},
	}

	body := `{"payload":{"k":1}}`
	req := httptest.NewRequest(http.MethodPost, "/message/new/attr-subject", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(legacyUserIDHeader, "mallory") // stray header: must never be read
	req = req.WithContext(spi.WithUserContext(req.Context(), uc))

	rr := httptest.NewRecorder()
	h.NewMessage(rr, req, "attr-subject", genapi.NewMessageParams{
		ContentType:   "application/json",
		ContentLength: int64(len(body)),
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("NewMessage: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var results []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &results); err != nil {
		t.Fatalf("decode NewMessage response: %v", err)
	}
	ids, _ := results[0]["entityIds"].([]any)
	if len(ids) == 0 {
		t.Fatalf("NewMessage response has no entityIds: %s", rr.Body.String())
	}
	msgID, _ := ids[0].(string)
	if msgID == "" {
		t.Fatalf("NewMessage response entityIds[0] is not a string: %s", rr.Body.String())
	}

	store, err := factory.MessageStore(req.Context())
	if err != nil {
		t.Fatalf("MessageStore: %v", err)
	}
	header, _, rc, err := store.Get(req.Context(), msgID)
	if err != nil {
		t.Fatalf("Get(%s): %v", msgID, err)
	}
	rc.Close()

	if header.UserID != "alice" {
		t.Errorf("header.UserID = %q, want %q (attributed user, not the legacy sender-identity header)", header.UserID, "alice")
	}
	if header.AttributedKind != spi.PrincipalUser {
		t.Errorf("header.AttributedKind = %q, want %q", header.AttributedKind, spi.PrincipalUser)
	}
	wantExecutor := spi.Principal{ID: "OBOCLIENT0000001", Kind: spi.PrincipalService}
	if header.Executor != wantExecutor {
		t.Errorf("header.Executor = %+v, want %+v", header.Executor, wantExecutor)
	}
}

// TestGetMessage_RendersAttributedKindAndExecutedBy drives NewMessage with a
// plain service UserContext (no Executor — a direct client request), then
// asserts GetMessage's response renders userId, attributedKind and
// executedBy for the direct-write case: executor == the client itself.
func TestGetMessage_RendersAttributedKindAndExecutedBy(t *testing.T) {
	h, _ := newAttrTestHandler()

	uc := &spi.UserContext{
		UserID: "client-42",
		Kind:   spi.PrincipalService,
		Tenant: spi.Tenant{ID: "attr-tenant-2", Name: "Attr2"},
	}

	body := `{"payload":{"k":2}}`
	req := httptest.NewRequest(http.MethodPost, "/message/new/attr-subject-2", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(spi.WithUserContext(req.Context(), uc))

	rr := httptest.NewRecorder()
	h.NewMessage(rr, req, "attr-subject-2", genapi.NewMessageParams{
		ContentType:   "application/json",
		ContentLength: int64(len(body)),
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("NewMessage: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var results []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &results); err != nil {
		t.Fatalf("decode NewMessage response: %v", err)
	}
	ids, _ := results[0]["entityIds"].([]any)
	msgID, _ := ids[0].(string)
	if msgID == "" {
		t.Fatalf("NewMessage response has no entityIds: %s", rr.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/message/"+msgID, nil)
	getReq = getReq.WithContext(spi.WithUserContext(getReq.Context(), uc))
	getRR := httptest.NewRecorder()
	msgUUID, err := uuid.Parse(msgID)
	if err != nil {
		t.Fatalf("parse message id %q: %v", msgID, err)
	}
	h.GetMessage(getRR, getReq, msgUUID)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GetMessage: status=%d body=%s", getRR.Code, getRR.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(getRR.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode GetMessage response: %v", err)
	}
	header, ok := resp["header"].(map[string]any)
	if !ok {
		t.Fatalf("GetMessage response has no header object: %s", getRR.Body.String())
	}
	if got := header["userId"]; got != "client-42" {
		t.Errorf("header.userId = %v, want client-42", got)
	}
	if got := header["attributedKind"]; got != "service" {
		t.Errorf("header.attributedKind = %v, want service", got)
	}
	executedBy, ok := header["executedBy"].(map[string]any)
	if !ok {
		t.Fatalf("header.executedBy missing or not an object: %v", header["executedBy"])
	}
	if got := executedBy["id"]; got != "client-42" {
		t.Errorf("header.executedBy.id = %v, want client-42 (direct write: executor == attributed principal)", got)
	}
	if got := executedBy["kind"]; got != "service" {
		t.Errorf("header.executedBy.kind = %v, want service", got)
	}
}
