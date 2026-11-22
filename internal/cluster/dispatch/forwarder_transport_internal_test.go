package dispatch

import (
	"net/http"
	"testing"
	"time"
)

// The hand-over transport's three settings are read here rather than through
// behaviour: Go exempts loopback from proxying, so a test server on 127.0.0.1
// proves nothing about the proxy rule, and a client-wide Timeout is invisible
// until a wait longer than it. Read directly, a later
// http.DefaultTransport.Clone() — which proxies from the environment and keeps
// connections alive — cannot pass.
//
// Each one carries a rule of §6: no proxy, because through one a peer that is
// down is reported as "proxyconnect" rather than as a dial error and "could not
// connect → no hand-off" is lost, and because a sealed request must go only to
// the address the guard validated; no client Timeout, because the wait for an
// answer is the deadline the owner puts on the context and may rightly span
// several answer limits; no kept-alive connection, because on one a peer that
// died is indistinguishable from a peer that took the work and then died.
func TestHTTPForwarder_TransportRules(t *testing.T) {
	f := NewHTTPForwarder(newAEAD(t), 2*time.Second)

	tr, ok := f.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want a transport of the forwarder's own", f.client.Transport)
	}
	if tr.Proxy != nil {
		t.Error("the hand-over transport has a proxy: a sealed request would go to an address the guard never saw")
	}
	if !tr.DisableKeepAlives {
		t.Error("the hand-over transport keeps connections alive: a dead peer is then indistinguishable from a lost answer")
	}
	if f.client.Timeout != 0 {
		t.Errorf("the hand-over client has a whole-request Timeout of %v: the wait is the context's deadline", f.client.Timeout)
	}
	if f.client.CheckRedirect == nil {
		t.Error("the hand-over client follows redirects")
	}
}
