package e2e_test

// Token-endpoint cost bounds through the full HTTP stack: the verified-secret
// cache gives way to a secret reset at once, and the per-client token bucket
// answers 429 slow_down. Each test uses clients of its own, so no other test
// shares a cache entry or a bucket with it.
//
// The bcrypt-busy 503 (no secret-check slot within the wait) is covered on a
// stack of its own, by TestSecretCheckBound_NoFreeSlot_503.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	genapi "github.com/cyoda-platform/cyoda-go/api"
)

// tokenRequestsPerMinute is the shared server's per-client limit: the
// default of CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE, which TestMain keeps. It is
// also the bucket's burst.
const tokenRequestsPerMinute = 600

// TestTokenCache_GrantsAfterResetRefuseOldSecret: once a reset has answered,
// the old secret is refused on every request to this single-node server —
// even though its cache verified that secret a moment before — and the new
// secret's token carries the new generation.
func TestTokenCache_GrantsAfterResetRefuseOldSecret(t *testing.T) {
	id, secret := createClient(t, false, false)
	_ = getToken(t, id, secret) // warms the cache with the old secret

	resp := adminRequest(t, http.MethodPut, "/clients/"+id+"/secret", nil)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset: %d %s", resp.StatusCode, withheld(resp.StatusCode, raw))
	}
	var creds genapi.TechnicalUserCredentialsDto
	if err := json.Unmarshal(raw, &creds); err != nil || creds.ClientSecret == "" {
		t.Fatalf("reset: no secret in the response (%v)", err)
	}

	const workers, perWorker = 4, 5
	statuses := make([]int, workers*perWorker)
	errs := make([]error, workers*perWorker)
	ctx := e2eCtx(t) // goroutines never touch t
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < perWorker; i++ {
				k := w*perWorker + i
				r, err := postTokenRaw(ctx, serverURL, suiteTenant, url.Values{"grant_type": {"client_credentials"}}, id, secret)
				if err != nil {
					errs[k] = err
					continue
				}
				statuses[k] = r.StatusCode
				_, _ = io.Copy(io.Discard, r.Body)
				r.Body.Close()
			}
		}(w)
	}
	close(start)
	wg.Wait()
	for k := range statuses {
		if errs[k] != nil {
			t.Errorf("grant %d with the old secret: %v", k, errs[k])
			continue
		}
		if statuses[k] != http.StatusUnauthorized {
			t.Errorf("grant %d with the old secret after the reset: %d, want 401", k, statuses[k])
		}
	}

	claims := decodeJWTPayload(t, getToken(t, id, creds.ClientSecret))
	if claims["cgen"] != float64(2) {
		t.Errorf("cgen after one reset = %v, want 2", claims["cgen"])
	}
}

// TestToken_PerClientBucket_429_E2E: a client is served its burst, then
// refused with 429 slow_down and a Retry-After of at least one second; another
// client is still served. The bucket refills at tokenRequestsPerMinute/60
// per second while the loop runs, so the refusal is expected after the burst
// plus what refilled in the meantime.
func TestToken_PerClientBucket_429_E2E(t *testing.T) {
	id, secret := createClient(t, false, false)
	started := time.Now()
	refusedAt := 0
	for i := 1; refusedAt == 0; i++ {
		refilled := int(time.Since(started).Seconds()*tokenRequestsPerMinute/60) + 1
		if i > tokenRequestsPerMinute+refilled+1 {
			t.Fatalf("no refusal after %d grants in %s", i-1, time.Since(started))
		}
		resp, err := postTokenRaw(e2eCtx(t), serverURL, suiteTenant, url.Values{"grant_type": {"client_credentials"}}, id, secret)
		if err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
		if resp.StatusCode == http.StatusOK {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			continue
		}
		refusedAt = i
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || n < 1 {
			t.Errorf("Retry-After = %q, want an integer >= 1", resp.Header.Get("Retry-After"))
		}
		assertOAuthError(t, resp, http.StatusTooManyRequests, "slow_down")
	}
	if refusedAt <= tokenRequestsPerMinute {
		t.Errorf("refused at grant %d, within the burst of %d", refusedAt, tokenRequestsPerMinute)
	}

	other, otherSecret := createClient(t, false, false)
	if code := statusForToken(t, other, otherSecret); code != http.StatusOK {
		t.Errorf("another client while the first is limited: %d, want 200", code)
	}
}
