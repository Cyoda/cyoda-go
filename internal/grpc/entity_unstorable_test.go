package grpc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// The unstorable-payload guard (ingest.RejectUnstorable) inspects the RAW
// bytes the client sent: Go's decoder silently rewrites unpaired UTF-16
// surrogates and invalid UTF-8 to U+FFFD and collapses duplicate keys, so a
// decode→re-marshal round-trip destroys the evidence and stores data the
// client never sent. These tests pin that every gRPC entity write event
// carries the client's payload bytes to the guard untouched.

// unstorablePlaceholderID never resolves to a real entity; the guard must
// reject before any entity lookup, so update/patch tests don't create one.
const unstorablePlaceholderID = "00000000-0000-0000-0000-000000000002"

// makeRawCE builds a CloudEvent whose payload is the given raw bytes,
// verbatim. BinaryData rather than TextData: proto3 strings must be valid
// UTF-8 on the wire, so raw invalid bytes can only genuinely arrive through
// the bytes variant — and ParseCloudEvent accepts both identically.
func makeRawCE(eventType string, payload []byte) *cepb.CloudEvent {
	return &cepb.CloudEvent{
		Id:          "test-req-1",
		Source:      "test",
		SpecVersion: "1.0",
		Type:        eventType,
		Data:        &cepb.CloudEvent_BinaryData{BinaryData: payload},
	}
}

// unstorableOp describes one gRPC entity write event and how to wrap a raw
// data snippet into its CloudEvent payload, byte-for-byte.
type unstorableOp struct {
	name       string
	eventType  string
	collection bool
	envelope   func(data string) string
}

func unstorableOps() []unstorableOp {
	return []unstorableOp{
		{
			name:      "Create",
			eventType: EntityCreateRequest,
			envelope: func(data string) string {
				return fmt.Sprintf(`{"id":"test","dataFormat":"JSON","payload":{"model":{"name":"person","version":1},"data":%s}}`, data)
			},
		},
		{
			name:      "Update",
			eventType: EntityUpdateRequest,
			envelope: func(data string) string {
				return fmt.Sprintf(`{"id":"test","dataFormat":"JSON","payload":{"entityId":"%s","data":%s}}`, unstorablePlaceholderID, data)
			},
		},
		{
			name:      "Patch",
			eventType: EntityPatchRequest,
			envelope: func(data string) string {
				return fmt.Sprintf(`{"id":"test","patchFormat":"MERGE_PATCH","payload":{"entityId":"%s","ifMatch":"*","patch":%s}}`, unstorablePlaceholderID, data)
			},
		},
		{
			name:       "CreateCollection",
			eventType:  EntityCreateCollectionRequest,
			collection: true,
			envelope: func(data string) string {
				return fmt.Sprintf(`{"id":"test","dataFormat":"JSON","payloads":[{"model":{"name":"person","version":1},"data":%s}]}`, data)
			},
		},
		{
			name:       "UpdateCollection",
			eventType:  EntityUpdateCollectionRequest,
			collection: true,
			envelope: func(data string) string {
				return fmt.Sprintf(`{"id":"test","dataFormat":"JSON","payloads":[{"entityId":"%s","data":%s}]}`, unstorablePlaceholderID, data)
			},
		},
	}
}

// invokeRaw sends the raw CloudEvent payload through the right RPC and
// returns the decoded transaction envelope.
func (op unstorableOp) invokeRaw(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, payload []byte) events.EntityTransactionResponseJson {
	t.Helper()
	ce := makeRawCE(op.eventType, payload)

	var typed events.EntityTransactionResponseJson
	if op.collection {
		stream := &mockManageStream{ctx: ctx}
		if err := svc.EntityManageCollection(ce, stream); err != nil {
			t.Fatalf("%s: unexpected gRPC error: %v", op.name, err)
		}
		if len(stream.sent) != 1 {
			t.Fatalf("%s: expected 1 response, got %d", op.name, len(stream.sent))
		}
		validateResponse(t, stream.sent[0], &typed)
		return typed
	}

	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("%s: unexpected gRPC error: %v", op.name, err)
	}
	validateResponse(t, resp, &typed)
	return typed
}

// TestRPC_EntityWrite_UnstorablePayloadRejected pins that each rejection
// class of the unstorable-payload guard fires on every gRPC entity write
// event, returning an error envelope instead of storing substituted or
// silently rewritten data.
func TestRPC_EntityWrite_UnstorablePayloadRejected(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	classes := []struct {
		name       string
		data       string // raw JSON snippet, embedded verbatim
		wantReason string // substring of the guard's rejection message
	}{
		{
			name:       "UnpairedSurrogate",
			data:       `{"name":"a\ud800b"}`,
			wantReason: "unpaired UTF-16 surrogate",
		},
		{
			name:       "InvalidUTF8",
			data:       "{\"name\":\"a\xffb\"}",
			wantReason: "not valid UTF-8",
		},
		{
			name:       "DuplicateKey",
			data:       `{"name":"a","name":"b"}`,
			wantReason: "appears more than once",
		},
		{
			name:       "NUL",
			data:       `{"name":"a\u0000b"}`,
			wantReason: "NUL character",
		},
		{
			name:       "NumericRange",
			data:       `{"name":1e131073}`,
			wantReason: "too many digits before the decimal point",
		},
	}

	for _, op := range unstorableOps() {
		for _, class := range classes {
			t.Run(op.name+"/"+class.name, func(t *testing.T) {
				resp := op.invokeRaw(t, svc, ctx, []byte(op.envelope(class.data)))
				if resp.Success {
					t.Fatal("expected success=false, got success=true: unstorable payload was accepted")
				}
				if resp.Error == nil {
					t.Fatal("expected error field to be populated")
				}
				if resp.Error.Code != "CLIENT_ERROR" {
					t.Errorf("error code = %q, want CLIENT_ERROR", resp.Error.Code)
				}
				if !strings.HasPrefix(resp.Error.Message, common.ErrCodeBadRequest+":") {
					t.Errorf("message = %q, want prefix %q", resp.Error.Message, common.ErrCodeBadRequest+":")
				}
				if !strings.Contains(resp.Error.Message, class.wantReason) {
					t.Errorf("message = %q, want reason substring %q", resp.Error.Message, class.wantReason)
				}
			})
		}
	}
}

// TestRPC_EntityCreate_StorablePayloadStillAccepted pins the flip side of
// the guard: content the guard must NOT fire on — a correctly PAIRED
// surrogate escape (\ud83d\ude00, U+1F600) and a client-sent U+FFFD escape
// (\ufffd) — remains valid payload, exactly as documented for the HTTP
// ingress.
func TestRPC_EntityCreate_StorablePayloadStillAccepted(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	payload := `{"id":"test","dataFormat":"JSON","payload":{"model":{"name":"person","version":1},"data":{"name":"a\ud83d\ude00b \ufffd c"}}}`

	resp := unstorableOps()[0]
	typed := resp.invokeRaw(t, svc, ctx, []byte(payload))
	if !typed.Success {
		msg := ""
		if typed.Error != nil {
			msg = typed.Error.Message
		}
		t.Fatalf("expected success, got error: %s", msg)
	}
	if len(typed.TransactionInfo.EntityIds) != 1 {
		t.Fatalf("expected 1 entity id, got %d", len(typed.TransactionInfo.EntityIds))
	}
}
