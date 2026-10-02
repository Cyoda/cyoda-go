package e2e_test

// Who may open a compute stream, and how long a stream outlives its client.
//
// Only a compute node's own client-credentials token opens a stream; an
// on-behalf-of token, or a token without ROLE_M2M, is refused with
// PermissionDenied. Every minute an open stream re-reads its client and
// closes with Unauthenticated once the client is deleted or its secret reset.
//
// Waiver: the "tenant differs" and "store error" re-check rows are covered by
// unit tests only (internal/grpc TestRecheckClient) — a running backend
// cannot move a client to another tenant, or fail the read, on demand.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"github.com/cyoda-platform/cyoda-go/app"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// streamCloseWithin bounds how long a test waits for the re-check to close a
// stream: one re-check interval and a margin.
const streamCloseWithin = 75 * time.Second

// openStreamCode opens a stream on h with bearer (no authorization metadata
// when it is empty), sends a join and returns the status the first Recv ends
// with: codes.OK when the server greets.
func openStreamCode(t *testing.T, h *callbackHarness, bearer string) codes.Code {
	t.Helper()
	conn, err := grpc.NewClient(h.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial gRPC: %v", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if bearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+bearer)
	}
	stream, err := cyodapb.NewCloudEventsServiceClient(conn).StartStreaming(ctx)
	if err != nil {
		t.Fatalf("StartStreaming: %v", err)
	}
	join, err := internalgrpc.NewCloudEvent(internalgrpc.CalculationMemberJoinEvent, map[string]any{
		"id": "stream-guard-join", "tags": []string{}, "joinedLegalEntityId": "",
	})
	if err != nil {
		t.Fatalf("build join event: %v", err)
	}
	// A refused stream may already be closed: the Recv below reports why.
	_ = stream.Send(join)
	_, err = stream.Recv()
	return status.Code(err)
}

func TestStream_NoOrInvalidToken_Unauthenticated(t *testing.T) {
	h := newCalloutHarness(t, nil)
	for _, tc := range []struct{ name, bearer string }{
		{"no token", ""},
		{"invalid token", "not-a-jwt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := openStreamCode(t, h, tc.bearer); got != codes.Unauthenticated {
				t.Fatalf("stream with %s: %v, want Unauthenticated", tc.name, got)
			}
		})
	}
}

// A user token (the shape `cyoda token` signs) is refused even with ROLE_M2M:
// only a client opens a stream.
func TestStream_UserToken_PermissionDenied(t *testing.T) {
	h := newCalloutHarness(t, nil)
	tok := h.mintUserToken(t, "alice", "ROLE_ADMIN", "ROLE_M2M")
	if got := openStreamCode(t, h, tok); got != codes.PermissionDenied {
		t.Fatalf("stream opened with a user token: %v, want PermissionDenied", got)
	}
}

func TestStream_OBOToken_PermissionDenied(t *testing.T) {
	// The OBO client needs a trusted key, and registering one needs the gate.
	h := newCalloutHarness(t, func(cfg *app.Config) { cfg.IAM.TrustedKeyRegistrationEnabled = true })
	tok := oboTokenOn(t, h.baseURL, h.token(t), "alice")
	if got := openStreamCode(t, h, tok); got != codes.PermissionDenied {
		t.Fatalf("stream opened with an on-behalf-of token: %v, want PermissionDenied", got)
	}
}

// A client token without ROLE_M2M cannot come from /oauth/token — every
// client holds ROLE_M2M — so it is signed here.
func TestStream_ClientTokenWithoutROLE_M2M(t *testing.T) {
	h := newCalloutHarness(t, nil)
	tok, err := signServiceToken(h.signKey, "cyoda-callback-test", h.audience, "no-m2m", "test-tenant", "no-m2m", []string{"ROLE_ADMIN"})
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	if got := openStreamCode(t, h, tok); got != codes.PermissionDenied {
		t.Fatalf("stream opened without ROLE_M2M: %v, want PermissionDenied", got)
	}
}

// A service token with ROLE_M2M but no cgen claim carries no client-token
// marker: it names no client generation the stream could re-check, so it is
// refused. Only the marker check refuses this token.
func TestStream_ServiceTokenWithoutCgen_PermissionDenied(t *testing.T) {
	h := newCalloutHarness(t, nil)
	tok, err := signServiceToken(h.signKey, "cyoda-callback-test", h.audience, "no-cgen", "test-tenant", "no-cgen", []string{"ROLE_M2M"})
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	if got := openStreamCode(t, h, tok); got != codes.PermissionDenied {
		t.Fatalf("stream opened without a client-token marker: %v, want PermissionDenied", got)
	}
}

func TestStream_ClientDeleted_ClosesUnauthenticated(t *testing.T) {
	t.Parallel()
	h := newCalloutHarness(t, nil)
	id, m := joinOwnClient(t, h)
	if code, body := h.deleteClient(t, h.token(t), id); code != http.StatusOK {
		t.Fatalf("delete the client: %d %s", code, body)
	}
	assertStreamEnds(t, m, codes.Unauthenticated)
}

func TestStream_SecretReset_ClosesUnauthenticated(t *testing.T) {
	t.Parallel()
	h := newCalloutHarness(t, nil)
	id, m := joinOwnClient(t, h)
	resp := h.doAuthBearer(t, h.token(t), http.MethodPut, "/api/clients/"+id+"/secret", "", "")
	if code := resp.StatusCode; code != http.StatusOK {
		t.Fatalf("reset the client's secret: %d %s", code, h.readBody(t, resp))
	}
	_ = resp.Body.Close() // the body holds the new secret: never read into a message
	assertStreamEnds(t, m, codes.Unauthenticated)
}

// joinOwnClient creates a client on h, joins a compute member with that
// client's own token and returns the client id and the member. The client is
// deleted when the test ends, if the test has not deleted it.
func joinOwnClient(t *testing.T, h *callbackHarness) (string, *computeMember) {
	t.Helper()
	code, raw := h.postClient(t, h.token(t))
	if code != http.StatusOK {
		t.Fatalf("create client: %d %s", code, withheld(code, raw))
	}
	cred := decodeCredential(t, "create client", raw)
	deleteClientAtCleanup(t, h.baseURL, cred.id, func() string { return h.token(t) })
	m := newComputeMember(t, h, memberSpec{bearer: h.fetchTokenFor(t, cred.id, cred.secret)})
	t.Cleanup(m.stop)
	return cred.id, m
}

// assertStreamEnds waits for m's stream to end and checks its status.
func assertStreamEnds(t *testing.T, m *computeMember, want codes.Code) {
	t.Helper()
	select {
	case <-m.done:
	case <-time.After(streamCloseWithin):
		t.Fatalf("the stream is still open %v later, want it closed with %v", streamCloseWithin, want)
	}
	if got := status.Code(m.endErr); got != want {
		t.Fatalf("the stream ended with %v (%v), want %v", got, m.endErr, want)
	}
}
