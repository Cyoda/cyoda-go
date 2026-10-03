package e2e_test

// The per-node bound on client-secret (bcrypt) work, on a running backend:
// with CYODA_IAM_TOKEN_MAX_CONCURRENT_SECRET_CHECKS=1, a flood of token
// requests with a wrong secret fills the one slot, and a request that gets no
// slot within 1 second is refused with 503 and Retry-After: 1 —
// temporarily_unavailable on /oauth/token, SERVER_BUSY on POST /clients.
//
// The flood is a concurrency test, so it runs on a stack of its own
// (newCalloutHarness), never on the shared server or in parity. It asserts
// consistency, not an interleave: at least one refusal of each kind, and
// every other token answer a 401.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// secretFloodWorkers is the number of concurrent wrong-secret token requests.
// With one slot and a bcrypt comparison of tens of milliseconds, the slot
// serves a few dozen requests a second, so most of this many waiters reach
// the 1 s slot wait and are refused.
const secretFloodWorkers = 64

// floodJitter randomises when each request arrives. Without it every worker
// cycles in about one slot wait, the arrivals settle into fixed phases, and
// the same requests are served every cycle: a POST /clients in a served phase
// would get a slot again and again.
func floodJitter(max time.Duration) time.Duration { return rand.N(max) }

// secretFloodDeadline bounds how long the test waits for each refusal.
const secretFloodDeadline = 60 * time.Second

// tokenFlood is the result of the wrong-secret flood, shared by its workers.
type tokenFlood struct {
	refused   atomic.Int64 // 503 temporarily_unavailable with Retry-After: 1
	rejected  atomic.Int64 // 401 invalid_client
	firstBusy chan struct{}
	busyOnce  sync.Once
	mu        sync.Mutex
	unexpect  []string // any other answer
}

func (f *tokenFlood) unexpected(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unexpect = append(f.unexpect, fmt.Sprintf(format, args...))
}

// record classifies one token answer.
func (f *tokenFlood) record(resp *http.Response) {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	switch {
	case resp.StatusCode == http.StatusUnauthorized && e.Error == "invalid_client":
		f.rejected.Add(1)
	case resp.StatusCode == http.StatusServiceUnavailable && e.Error == "temporarily_unavailable" && resp.Header.Get("Retry-After") == "1":
		f.refused.Add(1)
		f.busyOnce.Do(func() { close(f.firstBusy) })
	default:
		f.unexpected("%d Retry-After=%q %s", resp.StatusCode, resp.Header.Get("Retry-After"), raw)
	}
}

func TestSecretCheckBound_NoFreeSlot_503(t *testing.T) {
	h := newCalloutHarness(t, func(cfg *app.Config) {
		cfg.IAM.TokenMaxConcurrentSecretChecks = 1
	})

	// A cold client: created on this stack and never authenticated, so no
	// verified secret is cached for it; a wrong secret always pays bcrypt.
	code, raw := h.postClient(t, h.token(t))
	if code != http.StatusOK {
		t.Fatalf("create client: %d %s", code, withheld(code, raw))
	}
	cred := decodeCredential(t, "create client", raw)
	deleteClientAtCleanup(t, h.baseURL, cred.id, func() string { return h.token(t) })

	flood := &tokenFlood{firstBusy: make(chan struct{})}
	ctx, stop := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	form := url.Values{"grant_type": {"client_credentials"}}
	for i := range secretFloodWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				time.Sleep(floodJitter(100 * time.Millisecond))
				resp, err := postTokenRaw(ctx, h.baseURL, form, cred.id, fmt.Sprintf("wrong-secret-%d", i))
				if err != nil {
					if ctx.Err() == nil {
						flood.unexpected("token request: %v", err)
					}
					return
				}
				flood.record(resp)
			}
		}()
	}
	stopFlood := sync.OnceFunc(func() { stop(); wg.Wait() })
	t.Cleanup(stopFlood)

	deadline := time.Now().Add(secretFloodDeadline)

	// POST /clients during the flood: its secret hashing waits for the same
	// slot. Retry until one is refused; a create that got a slot is deleted.
	busy := false
	for !busy && time.Now().Before(deadline) {
		time.Sleep(floodJitter(500 * time.Millisecond))
		resp := h.doAuthBearer(t, h.token(t), http.MethodPost, "/api/clients", "", "")
		body := []byte(h.readBody(t, resp))
		switch resp.StatusCode {
		case http.StatusOK:
			created := decodeCredential(t, "create client during the flood", body)
			deleteClientAtCleanup(t, h.baseURL, created.id, func() string { return h.token(t) })
		case http.StatusServiceUnavailable:
			var pd struct {
				Properties map[string]any `json:"properties"`
			}
			if err := json.Unmarshal(body, &pd); err != nil {
				t.Fatalf("POST /clients 503: decode: %v; body=%s", err, body)
			}
			if got := fmt.Sprint(pd.Properties["errorCode"]); got != "SERVER_BUSY" {
				t.Fatalf("POST /clients 503: errorCode %q, want SERVER_BUSY; body=%s", got, body)
			}
			if got := resp.Header.Get("Retry-After"); got != "1" {
				t.Fatalf("POST /clients 503: Retry-After %q, want 1", got)
			}
			busy = true
		default:
			t.Fatalf("POST /clients during the flood: %d %s", resp.StatusCode, body)
		}
	}
	if !busy {
		t.Fatalf("POST /clients was never refused with 503 SERVER_BUSY within %v", secretFloodDeadline)
	}

	select {
	case <-flood.firstBusy:
	case <-time.After(time.Until(deadline)):
		t.Fatalf("no token request was refused with 503 temporarily_unavailable within %v", secretFloodDeadline)
	}
	stopFlood()

	flood.mu.Lock()
	defer flood.mu.Unlock()
	if len(flood.unexpect) > 0 {
		t.Fatalf("%d token answers were neither 401 invalid_client nor 503 temporarily_unavailable; first: %s", len(flood.unexpect), flood.unexpect[0])
	}
	t.Logf("token flood: %d refused (503), %d rejected (401)", flood.refused.Load(), flood.rejected.Load())
}
