package multinode

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func init() {
	Register(NamedTest{Name: "TrustedKeyInvalidationEveryNode", Fn: RunTrustedKeyInvalidationEveryNode})
}

// RunTrustedKeyInvalidationEveryNode: a trusted key is read from the shared
// store on every exchange, with no node copy, so invalidating it on one node
// ends it on every node at once. A key registered through node 0 serves an
// exchange through node 1; after it is invalidated through node 0, the very
// next exchange through node 1 is refused — no wait, no retry.
func RunTrustedKeyInvalidationEveryNode(t *testing.T, fixture MultiNodeFixture) {
	urls := fixture.BaseURLs()
	tenant := fixture.NewTenant(t)
	o := parity.NewOBOClient(t, urls[0], tenant)

	if code, body := o.Exchange(t, urls[1], "alice"); code != http.StatusOK {
		t.Fatalf("exchange on node 1 with a key registered on node 0: %d %s, want 200", code, body)
	}

	if code, body, err := client.NewClient(urls[0], tenant.Token).InvalidateTrustedKeyRaw(t, o.KID()); err != nil || code != http.StatusOK {
		t.Fatalf("invalidate on node 0: %d %v %s", code, err, body)
	}

	code, body := o.Exchange(t, urls[1], "alice")
	if code != http.StatusBadRequest || !strings.Contains(string(body), `"invalid_request"`) {
		t.Fatalf("exchange on node 1 after invalidating on node 0: %d %s, want 400 invalid_request", code, body)
	}
}
