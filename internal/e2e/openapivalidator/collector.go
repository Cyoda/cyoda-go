package openapivalidator

import "sync"

// Mismatch describes one validation failure: which operation, where in the
// response body, what was wrong.
type Mismatch struct {
	Operation string // operationId from the spec
	Method    string // HTTP method (GET, POST, ...)
	Path      string // request path that matched
	Status    int    // actual response status code
	JSONPath  string // JSON path within the response body (empty for non-body issues)
	Reason    string // human-readable diff
	TestName  string // t.Name() at request time, "unknown" if not captured
}

// collector accumulates Mismatch entries and tracks which operationIds
// were exercised during the run. Safe for concurrent use.
type collector struct {
	mu         sync.Mutex
	mismatches []Mismatch
	exercised  map[string]struct{}
}

// the package-level singleton used by the middleware. Tests may construct
// their own via newCollector.
var defaultCollector = newCollector()

func newCollector() *collector {
	return &collector{exercised: make(map[string]struct{})}
}

func (c *collector) append(m Mismatch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mismatches = append(c.mismatches, m)
}

func (c *collector) recordExercised(operationId string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exercised[operationId] = struct{}{}
}

// drain returns all accumulated mismatches and resets the slice.
func (c *collector) drain() []Mismatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.mismatches
	c.mismatches = nil
	return out
}

// exerciseSet returns a copy of the exercised-operations set.
func (c *collector) exerciseSet() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]bool, len(c.exercised))
	for k := range c.exercised {
		out[k] = true
	}
	return out
}

// DrainAndExercised drains the package-level collector and returns the
// snapshot together with the exercised-operations set. Used by the
// TestOpenAPIConformanceReport test in zzz_openapi_conformance_test.go
// (added in Task 1.9).
func DrainAndExercised() ([]Mismatch, map[string]bool) {
	return defaultCollector.drain(), defaultCollector.exerciseSet()
}
