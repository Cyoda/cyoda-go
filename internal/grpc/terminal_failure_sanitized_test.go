package grpc

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// A compute member's response that does not decode is Terminal — spec §3's
// site table assigns "response payload unmarshal" Terminal, not MemberFailed
// (MemberFailed means the cnode itself answered success=false, with its OWN
// message and verdict; here it answered success, and the message is ours).
// The client-visible failure must carry a fixed, sanitized message: never
// the raw json error text, which can quote a byte of the member's own
// (mis-)formatted response.
func TestRunLocal_MemberResponseUnreadable_IsTerminal_NoMarkerLeak(t *testing.T) {
	const marker = "SECRET-MARKER-123"
	reg := NewMemberRegistry()
	bad := ProcessingResponse{Success: true, Payload: json.RawMessage(marker + " this is not valid json")}
	attach(t, reg, "m-1", testTenantID, "x", answers(bad))
	d := newTestDispatcher(t, reg)

	res := d.RunLocal(testContext(), processorCall("x", true, 5*time.Second), 1)

	if res.Failure == nil || res.Failure.Kind != contract.Terminal {
		t.Fatalf("failure = %+v, want Terminal", res.Failure)
	}
	const wantMsg = "the compute member's response could not be read: the payload did not decode"
	if res.Failure.Message != wantMsg {
		t.Errorf("Message = %q, want %q", res.Failure.Message, wantMsg)
	}
	if len(res.Attempts) != 1 || res.Attempts[0].Cause != wantMsg {
		t.Errorf("Attempts = %+v, want one attempt with Cause %q", res.Attempts, wantMsg)
	}
	for name, text := range map[string]string{
		"Failure.Message": res.Failure.Message,
		"Attempts[0].Cause": func() string {
			if len(res.Attempts) > 0 {
				return res.Attempts[0].Cause
			}
			return ""
		}(),
		"Err().Error()": res.Err().Error(),
	} {
		if strings.Contains(text, marker) {
			t.Errorf("%s leaked the marker: %q", name, text)
		}
	}
}

// This pnode failing to build its OWN request — here, a cloud event that
// cannot be marshalled because the entity's own data is not valid JSON — is
// Terminal and internal: it would fail identically on any cnode, and no
// cnode is at fault. The client-visible failure must carry a fixed,
// sanitized message: never the raw json marshal error, which can quote a
// byte of the tenant's entity payload. errors.Is/As must still reach the
// real cause behind it.
func TestRunLocal_BuildRequestFails_NoValueLeak(t *testing.T) {
	const marker = "SECRET-MARKER-123"
	reg := NewMemberRegistry()
	attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1")) // must exist to be tried; never actually sent to
	d := newTestDispatcher(t, reg)

	entity := &spi.Entity{
		Meta: spi.EntityMeta{ID: "entity-bad", TenantID: testTenantID},
		Data: []byte(marker + " this is not valid json"),
	}
	processor := spi.ProcessorDefinition{
		Name:   "my-proc",
		Config: spi.ProcessorConfig{AttachEntity: true, CalculationNodesTags: "x"},
	}
	call := armed(NewProcessorCallout(testTenantID, entity, processor, "wf1", "t1", "tx-1"), false, 5*time.Second)

	res := d.RunLocal(testContext(), call, 1)

	if res.Failure == nil || res.Failure.Kind != contract.Terminal {
		t.Fatalf("failure = %+v, want Terminal", res.Failure)
	}
	const wantMsg = "SERVER_ERROR: internal error"
	if res.Failure.Message != wantMsg {
		t.Errorf("Message = %q, want %q", res.Failure.Message, wantMsg)
	}
	if len(res.Attempts) != 1 || res.Attempts[0].Cause != wantMsg {
		t.Errorf("Attempts = %+v, want one attempt with Cause %q", res.Attempts, wantMsg)
	}
	for name, text := range map[string]string{
		"Failure.Message": res.Failure.Message,
		"Attempts[0].Cause": func() string {
			if len(res.Attempts) > 0 {
				return res.Attempts[0].Cause
			}
			return ""
		}(),
		"Err().Error()": res.Err().Error(),
	} {
		if strings.Contains(text, marker) {
			t.Errorf("%s leaked the marker: %q", name, text)
		}
	}

	var appErr *common.AppError
	if !errors.As(res.Err(), &appErr) || appErr.Level != common.LevelInternal {
		t.Fatalf("errors.As(res.Err(), &appErr) = %v, %v; want a LevelInternal AppError", appErr, res.Err())
	}
	var syn *json.SyntaxError
	if !errors.As(res.Err(), &syn) {
		t.Errorf("errors.As(res.Err(), &syn) found nothing: the underlying json.SyntaxError must still be reachable")
	}
}
