package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"github.com/cyoda-platform/cyoda-go/app"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// callback_txjoin_grpc_bounds_test.go proves CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES
// on the gRPC UNARY door (txRouteInterceptor.unary(), the EntitySearch RPC
// carrying an EntityGetRequest), against a real Postgres-backed stack. Before
// this file, the ceiling on that door was proved only at the interceptor-unit
// level (txroute_interceptor_test.go) with a mocked handler and joiner, which
// pins the interceptor's own logic but not that MaxResponseBytes() actually
// reaches the gRPC server built by app.New — a wiring gap that a mock cannot
// see: if the config value never reached the interceptor, every existing unit
// test would still pass and the door would ship unbounded.
//
// callback_txjoin_bounds_test.go proves the equivalent contract on the HTTP
// door; this is its gRPC-unary counterpart, same shape: lower the ceiling far
// below any entity this stack can return, so an ordinary joined unary read
// passes it and is refused with JOINED_RESPONSE_TOO_LARGE — nothing of the
// answer sent, the transaction otherwise unharmed.

// getEntityGRPCRaw issues a unary EntitySearch (EntityGetRequest) for entityID,
// exactly as getEntityGRPC (callback_txjoin_grpc_search_test.go) does, but
// returns the raw response CloudEvent instead of decoding it as a success. A
// ceiling refusal is a Success=false EntityResponse envelope, not a transport
// error, and getEntityGRPC's parseEntityResponse discards the error's code and
// message on that path — exactly the two fields this cell asserts on.
func (h *callbackHarness) getEntityGRPCRaw(entityID, joinTok string) (*cepb.CloudEvent, error) {
	client := cyodapb.NewCloudEventsServiceClient(h.apiConn)
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityGetRequest, map[string]any{
		"id":       "cb-grpc-get-bounds",
		"entityId": entityID,
	})
	if err != nil {
		return nil, fmt.Errorf("build get request: %w", err)
	}
	respCE, err := client.EntitySearch(h.grpcCtx(joinTok), reqCE)
	if err != nil {
		return nil, fmt.Errorf("EntitySearch: %w", err)
	}
	return respCE, nil
}

// entityResponseErrorFields decodes an EntityResponse CloudEvent's error.code
// and error.message. Both come back empty for a successful response or one
// that fails to parse — a refusal is the only shape this cell reads them from.
func entityResponseErrorFields(ce *cepb.CloudEvent) (code, message string) {
	_, payload, err := internalgrpc.ParseCloudEvent(ce)
	if err != nil {
		return "", ""
	}
	var resp struct {
		Success bool `json:"success"`
		Error   *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &resp) != nil || resp.Success || resp.Error == nil {
		return "", ""
	}
	return resp.Error.Code, resp.Error.Message
}

// TestCallbackBounds_GRPCUnaryAnswerOverTheCeiling_JoinedResponseTooLarge is
// the running-backend row for the gRPC unary door: a SYNC processor on the
// compute member reads a committed entity over the joined unary EntitySearch
// RPC. With the ceiling lowered below the entity's encoded size, the door
// refuses it with 413-equivalent JOINED_RESPONSE_TOO_LARGE (never a truncated
// answer), and the primary's transition still commits.
func TestCallbackBounds_GRPCUnaryAnswerOverTheCeiling_JoinedResponseTooLarge(t *testing.T) {
	const ceiling = 64 // bytes; smaller than any entity this stack can return
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		cfg.Callout.JoinedResponseMaxBytes = ceiling
	})

	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	primary, secondary := "cbb-grpc-ceiling-primary-"+sfx, "cbb-grpc-ceiling-secondary-"+sfx
	proc := "cbb-grpc-read-over-ceiling-" + sfx
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	// A committed entity for the callback to read. Created without a token, so
	// it is an ordinary request and the ceiling does not bear on it.
	readID, status, body := h.CreateEntity(t, secondary, 1, `{"name":"target","amount":1,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("seed create: status=%d body=%s", status, body)
	}

	type readResult struct {
		code    string
		message string
	}
	got := make(chan readResult, 1)
	h.RegisterProc(proc, func(rc *reqCtx) (map[string]any, error) {
		respCE, err := rc.h.getEntityGRPCRaw(readID, rc.token)
		if err != nil {
			return nil, fmt.Errorf("joined gRPC get failed at the transport: %w", err)
		}
		code, message := entityResponseErrorFields(respCE)
		got <- readResult{code, message}
		// The refusal is the member's to handle: swallowing it here proves the
		// callout and its transaction carry on.
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, boundsWorkflow("cbb-grpc-ceiling-wf-"+sfx, proc))

	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("primary create: status=%d body=%s", status, body)
	}

	var r readResult
	select {
	case r = <-got:
	case <-time.After(15 * time.Second):
		t.Fatal("timeout: the processor's callback never returned")
	}
	// An operational refusal's envelope carries the generic "CLIENT_ERROR"
	// class in error.code; the specific code rides as the "CODE: detail"
	// prefix of error.message (the same convention txroute_interceptor_test.go's
	// assertEnvelopeCode pins at the unit level).
	if r.code != "CLIENT_ERROR" {
		t.Fatalf("errorCode = %q; want CLIENT_ERROR (message: %s)", r.code, r.message)
	}
	if !strings.HasPrefix(r.message, "JOINED_RESPONSE_TOO_LARGE:") {
		t.Fatalf("error.message = %q; want a JOINED_RESPONSE_TOO_LARGE: prefix", r.message)
	}
	// The caller is told the ceiling it passed — the one thing it can act on.
	if !strings.Contains(r.message, strconv.Itoa(ceiling)) {
		t.Errorf("the refusal does not name the %d-byte ceiling (message: %s)", ceiling, r.message)
	}

	// The transaction is unharmed: the refused answer was dropped before it was
	// sent, nothing of the transaction was touched, and the transition
	// committed.
	if st, code := h.GetEntityState(t, primaryID); code != http.StatusOK || st != "ACTIVE" {
		t.Fatalf("primary state = %q (http %d); want ACTIVE — a refused gRPC answer must not harm T", st, code)
	}
	if st, code := h.GetEntityState(t, readID); code != http.StatusOK || st != "STORED" {
		t.Fatalf("read target state = %q (http %d); want STORED", st, code)
	}
}
