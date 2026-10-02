package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// SecretCheckLimit bounds bcrypt work on one node.
type SecretCheckLimit struct {
	Slots int           // concurrent comparisons; ≥ 1
	Wait  time.Duration // longest wait for a slot; production: time.Second
}

// secretCheckWait is how long a token request waits for a secret-check slot
// before it is refused with 503.
const secretCheckWait = time.Second

// ErrSecretCheckBusy is returned by M2MClientStore.Authenticate when no
// secret-check slot frees up within the wait. The token endpoint answers it
// with 503 temporarily_unavailable and Retry-After.
var ErrSecretCheckBusy = errors.New("secret check capacity exhausted")

// maxVerifiedSecrets bounds the verified-secret cache of one node. Dropping
// an entry costs that client one bcrypt on its next request, never a wrong
// answer, so the bound only caps memory: clients that were deleted, or whose
// secret was reset, leave entries no request removes.
const maxVerifiedSecrets = 65536

// secretSlots bounds the bcrypt comparisons running at once on one node.
type secretSlots struct {
	sem  *semaphore.Weighted
	wait time.Duration
}

func newSecretSlots(limit SecretCheckLimit) *secretSlots {
	return &secretSlots{sem: semaphore.NewWeighted(int64(limit.Slots)), wait: limit.Wait}
}

// run runs compare in a slot. A slot that does not free up within the wait,
// or a caller that gives up first, is ErrSecretCheckBusy and compare does
// not run.
func (s *secretSlots) run(ctx context.Context, compare func()) error {
	wctx, cancel := context.WithTimeout(ctx, s.wait)
	defer cancel()
	if err := s.sem.Acquire(wctx, 1); err != nil {
		return ErrSecretCheckBusy
	}
	defer s.sem.Release(1)
	compare()
	return nil
}

// verifiedSecret is a secret that matched a client's stored hash: the hash
// and the SHA-256 of the secret. The secret itself is never kept.
type verifiedSecret struct {
	hashed string
	sum    [32]byte
}

// verifiedSecretCache maps a client id to the secret that last matched its
// stored hash on this node.
type verifiedSecretCache struct {
	mu      sync.Mutex
	max     int
	entries map[string]verifiedSecret
}

func newVerifiedSecretCache(max int) *verifiedSecretCache {
	return &verifiedSecretCache{max: max, entries: make(map[string]verifiedSecret)}
}

// hit reports whether sum is the SHA-256 of a secret that matched hashed,
// the hash the client's record holds now. The sums are compared in constant
// time.
func (c *verifiedSecretCache) hit(clientID, hashed string, sum [32]byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[clientID]
	return ok && e.hashed == hashed && subtle.ConstantTimeCompare(e.sum[:], sum[:]) == 1
}

// put records that the secret with SHA-256 sum matched hashed. At the bound,
// an arbitrary other entry is dropped first.
func (c *verifiedSecretCache) put(clientID, hashed string, sum [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[clientID]; !ok && len(c.entries) >= c.max {
		for k := range c.entries {
			delete(c.entries, k)
			break
		}
	}
	c.entries[clientID] = verifiedSecret{hashed: hashed, sum: sum}
}

// drop removes clientID's entry.
func (c *verifiedSecretCache) drop(clientID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, clientID)
}
