package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
)

// cluster_admin_refused_test.go — in a cluster, a request for model or
// workflow administration that carries a transaction token is refused by the
// node that receives it, before the token is verified and before the request
// is forwarded to the transaction's owner. The answer is 400
// MODEL_ADMIN_IN_JOINED_TRANSACTION whatever the token: tampered, valid for
// this node, or valid for a peer. Without the refusal the cluster routing
// layer would answer a tampered token 401 and forward a peer's token to the
// peer — which the entity-route control at the end shows it still does for a
// request that is not administration.

func TestCluster_ModelAdministration_RefusedBeforeRouting(t *testing.T) {
	const tenantID = "test-tenant"
	sfx := randSuffix(t)
	tag := "s-adm-" + sfx
	peer := newStandInPeer(t, "adm-peer-"+sfx, tenantID, []string{tag}, nil, peerAnswerDoesNotOpen)
	ownerNodeID := "adm-owner-" + sfx
	h := newLostHandOverHarness(t, ownerNodeID, []string{peer.gossipAddr})
	awaitPeerTag(t, h, tenantID, tag, 1)

	model := "adm-cluster-" + sfx
	resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model), workflowSampleModel, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import model: %d %s", resp.StatusCode, body)
	}

	signer, err := token.NewSigner(clusterHMACSecret32)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	other, err := token.NewSigner([]byte("other-secret-key-at-least-32-bytes"))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	claims := func(node string) token.Claims {
		return token.Claims{NodeID: node, TxRef: "tx-adm-" + sfx, ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-adm", Major: 1}
	}
	selfTok, _ := signer.Issue(claims(ownerNodeID))
	peerTok, _ := signer.Issue(claims(peer.nodeID))
	tamperedTok, _ := other.Issue(claims(ownerNodeID))

	base := "/api/model/" + model + "/1"
	ops := []struct{ id, method, path, body string }{
		{"importEntityModel", http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model), workflowSampleModel},
		{"deleteEntityModel", http.MethodDelete, base, ""},
		{"setEntityModelChangeLevel", http.MethodPost, base + "/changeLevel/STRUCTURAL", ""},
		{"lockEntityModel", http.MethodPut, base + "/lock", ""},
		{"unlockEntityModel", http.MethodPut, base + "/unlock", ""},
		{"setEntityModelUniqueKeys", http.MethodPut, base + "/unique-keys", `[{"id":"uk","fields":["$.name"]}]`},
		{"importEntityModelWorkflow", http.MethodPost, base + "/workflow/import", secondaryWorkflow},
	}
	for _, tc := range []struct{ name, tok string }{
		{"tampered", tamperedTok}, {"valid-for-self", selfTok}, {"valid-for-peer", peerTok},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, op := range ops {
				resp := h.DoAuth(t, op.method, op.path, op.body, tc.tok)
				body := h.readBody(t, resp)
				if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"errorCode":"MODEL_ADMIN_IN_JOINED_TRANSACTION"`) {
					t.Errorf("%s: status=%d body=%s; want 400 MODEL_ADMIN_IN_JOINED_TRANSACTION", op.id, resp.StatusCode, body)
				}
			}
		})
	}
	if faults := peer.faultList(); len(faults) != 0 {
		t.Fatalf("the peer received %v; an administration request is never proxied", faults)
	}

	// The model is untouched: still unlocked, so a lock without a token succeeds.
	resp = h.DoAuth(t, http.MethodPut, base+"/lock", "", "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock without a token after the refusals: %d %s", resp.StatusCode, body)
	}

	// Control: the peer's token on an entity route is still routed to the
	// peer, which shows the refusal above came before routing, not instead of
	// it. The stand-in serves no API, so it records the arrival and answers 404.
	resp = h.DoAuth(t, http.MethodGet, "/api/entity/"+uuid.NewString(), "", peerTok)
	h.readBody(t, resp)
	faults := peer.faultList()
	if resp.StatusCode != http.StatusNotFound || len(faults) != 1 || !strings.Contains(faults[0], "GET /api/entity/") {
		t.Fatalf("entity route under the peer's token: status=%d, peer saw %v; want it proxied to the peer", resp.StatusCode, faults)
	}
}
