package e2e_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// lateChild is the payload assertRefusedOnAllDoors uses locally (S-1) for its
// late-write door; there is no package-level constant of that name today, so
// this scenario defines its own copy of the same shape (deviation: the brief
// names "lateChild is S-8's constant", but no such symbol exists in the tree).
const lateChild = `{"name":"late-child","amount":1,"status":"late"}`

// passClaims decodes the claims of a recorded pass. The claims are not secret;
// the signature is what protects them.
func passClaims(t *testing.T, pass string) token.Claims {
	t.Helper()
	head, _, ok := strings.Cut(pass, ".")
	if !ok {
		t.Fatal("recorded pass has no signature part")
	}
	raw, err := base64.RawURLEncoding.DecodeString(head)
	if err != nil {
		t.Fatalf("decode pass claims: %v", err)
	}
	var c token.Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("unmarshal pass claims: %v", err)
	}
	return c
}

// TestCalloutPass (shape F-ended): with processor B's callout in progress and
// processor A's ended, on one open transaction.
func TestCalloutPass(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	_ = h.token(t)
	const model, secondary, tagA, tagB = "s10-pass", "s10-pass-secondary", "s10-pass-a", "s10-pass-b"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s10-pass-wf",
		procSpec{"s10-proc-a", "SYNC", map[string]any{"calculationNodesTags": tagA}},
		procSpec{"s10-proc-b", "SYNC", map[string]any{"calculationNodesTags": tagB}}))

	release := make(chan struct{})
	releaseNow := closeOnce(release)
	t.Cleanup(releaseNow)
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: scriptHold(release, answerOK())})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()
	live := awaitCnodeReceived(t, b, 1, 15*time.Second)[0]
	ended := a.Received()[0]
	claims := passClaims(t, live.Pass())
	if claims.Callout == "" || claims.Major == 0 {
		t.Fatalf("a minted pass carries callout=%q major=%d; want the callout's request id and a number", claims.Callout, claims.Major)
	}
	if claims.Callout != live.RequestID {
		t.Errorf("pass callout = %q; want the callout's request id %q", claims.Callout, live.RequestID)
	}

	t.Run("no-callout-and-number-401", func(t *testing.T) {
		bare := token.Claims{NodeID: claims.NodeID, TxRef: claims.TxRef, ExpiresAt: claims.ExpiresAt}
		pass, err := h.app.TokenSigner().Issue(bare)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		assertRefusedOnAllDoors(t, h, pass, secondary, live.EntityID, http.StatusUnauthorized, "UNAUTHORIZED", "invalid transaction token")
	})

	t.Run("expired-410", func(t *testing.T) {
		old := claims
		old.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		pass, err := h.app.TokenSigner().Issue(old)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		assertRefusedOnAllDoors(t, h, pass, secondary, live.EntityID, http.StatusGone, "TRANSACTION_EXPIRED", "")
	})

	t.Run("another-tenant-403", func(t *testing.T) {
		clientB, secretB := h.provisionTenant(t, "s10-tenant-b", "s10-user-b")
		bearerB := h.fetchTokenFor(t, clientB, secretB)
		// A current pass and a superseded one: another tenant learns the same
		// nothing from both — never CALLOUT_SUPERSEDED.
		for name, pass := range map[string]string{"current pass": live.Pass(), "ended pass": ended.Pass()} {
			resp := h.doAuthBearer(t, bearerB, http.MethodGet, "/api/entity/"+live.EntityID, "", pass)
			assertProblem(t, resp.StatusCode, h.readBody(t, resp), http.StatusForbidden, "FORBIDDEN", false)
			resp = h.doAuthBearer(t, bearerB, http.MethodPost, "/api/entity/JSON/"+secondary+"/1", lateChild, pass)
			assertProblem(t, resp.StatusCode, h.readBody(t, resp), http.StatusForbidden, "FORBIDDEN", false)

			ctx := metadata.AppendToOutgoingContext(context.Background(),
				"authorization", "Bearer "+bearerB, internalgrpcTxTokenKey, pass)
			reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityGetRequest, map[string]any{"id": "s10-stolen", "entityId": live.EntityID})
			if err != nil {
				t.Fatalf("build get request: %v", err)
			}
			respCE, err := cyodapb.NewCloudEventsServiceClient(h.apiConn).EntitySearch(ctx, reqCE)
			if err != nil {
				t.Fatalf("%s, gRPC: %v", name, err)
			}
			env, err := parseTxEnvelope(respCE)
			assertEnvelope(t, name+", gRPC read", env, err, "FORBIDDEN", false)
		}
	})

	releaseNow()
	if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200 — none of the refused passes touched the operation", res.status, res.body)
	}
	if n := h.countEntities(t, secondary); n != 0 {
		t.Errorf("%d secondary entities committed; want 0", n)
	}
}
