// Package txgate provides per-transaction exclusive gates. A joined callback
// and the transaction owner's commit both Acquire the same txID's gate so their
// access to the shared tx buffer / pgx.Tx is serialised. This is the
// application-side concurrency contract the SPI delegates
// (cyoda-go-spi transaction.go: "the application must serialise its own
// concurrent in-flight ops on the same tx").
package txgate

import "sync"

type gate struct {
	mu   sync.Mutex
	refs int
}

// Registry hands out exclusive per-txID gates, cleaning up entries once no
// goroutine holds or waits on them.
type Registry struct {
	mu    sync.Mutex
	gates map[string]*gate
}

func New() *Registry { return &Registry{gates: make(map[string]*gate)} }

// Acquire blocks until the caller holds the exclusive gate for txID, then
// returns a release func. Empty txID returns a no-op release (never gated).
func (r *Registry) Acquire(txID string) func() {
	if txID == "" {
		return func() {}
	}
	r.mu.Lock()
	g := r.gates[txID]
	if g == nil {
		g = &gate{}
		r.gates[txID] = g
	}
	g.refs++
	r.mu.Unlock()

	g.mu.Lock()

	return func() {
		g.mu.Unlock()
		r.mu.Lock()
		g.refs--
		if g.refs == 0 {
			delete(r.gates, txID)
		}
		r.mu.Unlock()
	}
}

func (r *Registry) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.gates)
}
