package auth

import (
	"sync"
	"time"

	"golang.org/x/time/rate"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// clientKey names a client: ids are unique only within a tenant.
type clientKey struct {
	tenant spi.TenantID
	id     string
}

// clientBuckets limits each client's token requests on one node: a token
// bucket per client of a tenant holding n tokens, refilled at n per minute.
//
// A bucket that has refilled is the same as a fresh one, and every bucket
// refills within a minute of its last request, so buckets idle for a minute
// are dropped: the map holds only the clients served in the last two
// minutes or so.
type clientBuckets struct {
	n         int // requests per minute and burst; <= 0: no limit
	mu        sync.Mutex
	limiters  map[clientKey]*rate.Limiter
	lastSweep time.Time
}

func newClientBuckets(n int) *clientBuckets {
	return &clientBuckets{n: n, limiters: make(map[clientKey]*rate.Limiter)}
}

// allow takes one token from k's bucket at now. When the bucket is
// empty nothing is taken, and wait is how long until a token is there.
func (b *clientBuckets) allow(k clientKey, now time.Time) (ok bool, wait time.Duration) {
	if b.n <= 0 {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Sub(b.lastSweep) >= time.Minute {
		b.sweep(now)
	}
	lim, found := b.limiters[k]
	if !found {
		lim = rate.NewLimiter(rate.Every(time.Minute/time.Duration(b.n)), b.n)
		b.limiters[k] = lim
	}
	r := lim.ReserveN(now, 1)
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return false, d
	}
	return true, 0
}

// sweep drops the buckets that are full at now. The caller holds b.mu.
func (b *clientBuckets) sweep(now time.Time) {
	for k, lim := range b.limiters {
		if lim.TokensAt(now) >= float64(b.n) {
			delete(b.limiters, k)
		}
	}
	b.lastSweep = now
}
