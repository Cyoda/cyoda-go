package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func TestGossipRegistry_UnparseableMetadata_NotAlive_HTTP503(t *testing.T) {
	ctx := context.Background()
	r := startInternalGossip(t, "badmeta-1", 200*time.Millisecond)

	// A member of the gossip cluster whose metadata is not a pnode's.
	startRawPeer(t, "badmeta-stranger", []byte("not json"), gossipAddr(r))
	waitFor(t, 5*time.Second, "memberlist on badmeta-1 has the stranger as an alive member", func() bool {
		_, ok := r.member("badmeta-stranger")
		return ok
	})

	addr, alive, err := r.Lookup(ctx, "badmeta-stranger")
	if err != nil {
		t.Fatalf("Lookup returned an error for unparseable metadata: %v — callers turn that into a 500", err)
	}
	if alive || addr != "" {
		t.Errorf("Lookup = (%q, %v), want not alive", addr, alive)
	}
	nodes, err := r.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, n := range nodes {
		if n.NodeID == "badmeta-stranger" {
			t.Error("List reports a member whose metadata does not parse")
		}
	}

	signer, err := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := signer.Issue(token.Claims{NodeID: "badmeta-stranger", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-1", Major: 1})
	if err != nil {
		t.Fatal(err)
	}
	handler := proxy.HTTPRouting(signer, r, "badmeta-1", 5*time.Second, true)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodGet, "/api/entity/1", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), common.ErrCodeTransactionNodeUnavailable) {
		t.Errorf("body %s does not carry %s", rec.Body.String(), common.ErrCodeTransactionNodeUnavailable)
	}
}
