package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/semaphore"
)

// SecretCheckLimit bounds bcrypt work on one node: comparing a presented
// secret, and hashing a new one.
type SecretCheckLimit struct {
	Slots   int                // concurrent bcrypt operations; ≥ 1
	Wait    time.Duration      // longest wait for a slot; production: time.Second
	Metrics SecretCheckMetrics // counts refusals; nil: no metrics
}

// SecretCheckMetrics counts the bcrypt operations refused because no slot
// freed up within the wait. A refusal is not logged: under load there would
// be one line per request.
type SecretCheckMetrics interface {
	SecretCheckRefused()
}

// otelSecretCheckMetrics implements SecretCheckMetrics over an OTel counter.
type otelSecretCheckMetrics struct {
	refused metric.Int64Counter
}

// NewOTelSecretCheckMetrics builds the OTel-backed SecretCheckMetrics,
// publishing the counter "cyoda.auth.secret_checks.refused".
func NewOTelSecretCheckMetrics(meter metric.Meter) (SecretCheckMetrics, error) {
	refused, err := meter.Int64Counter("cyoda.auth.secret_checks.refused",
		metric.WithDescription("Client-secret bcrypt operations refused because no slot freed up within the wait"))
	if err != nil {
		return nil, fmt.Errorf("failed to create the secret-check refusal counter: %w", err)
	}
	return &otelSecretCheckMetrics{refused: refused}, nil
}

func (m *otelSecretCheckMetrics) SecretCheckRefused() {
	m.refused.Add(context.Background(), 1)
}

// secretCheckWait is how long a token request waits for a secret-check slot
// before it is refused with 503.
const secretCheckWait = time.Second

// ErrSecretCheckBusy is returned by M2MClientStore.Authenticate, Create and
// ResetSecret when no secret-check slot frees up within the wait; nothing
// was written. The token endpoint answers it with 503
// temporarily_unavailable, the /clients endpoints with 503 SERVER_BUSY,
// both with Retry-After: 1.
var ErrSecretCheckBusy = errors.New("secret check capacity exhausted")

// maxVerifiedSecrets bounds the verified-secret cache of one node. Dropping
// an entry costs that client one bcrypt on its next request, never a wrong
// answer, so the bound only caps memory: clients that were deleted, or whose
// secret was reset, leave entries no request removes.
const maxVerifiedSecrets = 65536

// secretSlots bounds the bcrypt operations running at once on one node.
type secretSlots struct {
	sem     *semaphore.Weighted
	wait    time.Duration
	metrics SecretCheckMetrics // nil: no metrics
}

func newSecretSlots(limit SecretCheckLimit) *secretSlots {
	return &secretSlots{sem: semaphore.NewWeighted(int64(limit.Slots)), wait: limit.Wait, metrics: limit.Metrics}
}

// run runs work in a slot. A slot that does not free up within the wait, or
// a caller that gives up first, is ErrSecretCheckBusy, counted, and work
// does not run.
func (s *secretSlots) run(ctx context.Context, work func()) error {
	wctx, cancel := context.WithTimeout(ctx, s.wait)
	defer cancel()
	if err := s.sem.Acquire(wctx, 1); err != nil {
		if s.metrics != nil {
			s.metrics.SecretCheckRefused()
		}
		return ErrSecretCheckBusy
	}
	defer s.sem.Release(1)
	work()
	return nil
}

// verifiedSecret is a secret that matched a client's stored hash: the hash
// and the SHA-256 of the secret. The secret itself is never kept.
type verifiedSecret struct {
	hashed string
	sum    [32]byte
}

// verifiedSecretCache maps a client of a tenant to the secret that last matched its
// stored hash on this node.
type verifiedSecretCache struct {
	mu      sync.Mutex
	max     int
	entries map[clientKey]verifiedSecret
}

func newVerifiedSecretCache(max int) *verifiedSecretCache {
	return &verifiedSecretCache{max: max, entries: make(map[clientKey]verifiedSecret)}
}

// hit reports whether sum is the SHA-256 of a secret that matched hashed,
// the hash the client's record holds now. The sums are compared in constant
// time.
func (c *verifiedSecretCache) hit(k clientKey, hashed string, sum [32]byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	return ok && e.hashed == hashed && subtle.ConstantTimeCompare(e.sum[:], sum[:]) == 1
}

// put records that the secret with SHA-256 sum matched hashed. At the bound,
// an arbitrary other entry is dropped first.
func (c *verifiedSecretCache) put(k clientKey, hashed string, sum [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[k]; !ok && len(c.entries) >= c.max {
		for k := range c.entries {
			delete(c.entries, k)
			break
		}
	}
	c.entries[k] = verifiedSecret{hashed: hashed, sum: sum}
}

// drop removes k's entry.
func (c *verifiedSecretCache) drop(k clientKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, k)
}
