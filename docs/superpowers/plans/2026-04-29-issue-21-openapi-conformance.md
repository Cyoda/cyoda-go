# Issue #21 — OpenAPI Server-Spec Conformance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reconcile `api/openapi.yaml` with actual server behavior across all 81 operations, add a runtime response-shape validator at the E2E test boundary, fix surfaced handler defects, close E2E coverage gaps, and lock the validator into enforce mode on `main`.

**Architecture:** Hand-edited spec stays canonical. A `kin-openapi/openapi3filter`-based validator wraps the test server in `internal/e2e/`, capturing every response and validating it against the matched operation's declared schema. Two modes: `record` (collects, writes report file, never fails) for the spec-fix work; `enforce` (fails on mismatch) once the spec converges. Per-domain commits land sequentially; final commit flips the mode.

**Tech Stack:** Go 1.26, `oapi-codegen` v2.6.0 (existing, unchanged), `kin-openapi` v0.137.0 (existing direct dep), Docker for postgres testcontainers.

**Spec:** `docs/superpowers/specs/2026-04-29-issue-21-openapi-conformance-design.md`
**ADR:** `docs/adr/0001-openapi-server-spec-conformance.md`
**Reviews (4):** `docs/superpowers/reviews/2026-04-29-issue-21-openapi-conformance-review-{01..04}.md`

---

## File structure

**New packages and files:**

- `internal/e2e/openapivalidator/` — new package
  - `mode.go` — `Mode` constant (`ModeRecord` / `ModeEnforce`)
  - `mode_test.go` — pins `Mode == ModeEnforce` (lands in commit 11 only)
  - `validator.go` — kin-openapi router, validation entry point
  - `validator_test.go` — fixture-pinning tests (commit 1)
  - `middleware.go` — tee-writer + handler wrapper
  - `middleware_test.go` — middleware behavior tests
  - `tee.go` — `http.ResponseWriter` wrapper that delegates `Flusher`/`Hijacker`/`ReaderFrom`
  - `collector.go` — process-level mismatch collector with mutex
  - `report.go` — markdown report emission
  - `testname.go` — context-key plumbing for capturing `*testing.T` per request
  - `doc.go` — package documentation
- `internal/e2e/zzz_openapi_conformance_test.go` — `TestOpenAPIConformanceReport`; sorts last alphabetically across files
- `internal/e2e/_openapi-conformance-report.md` — runtime-generated, gitignored
- `docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md` — checked-in audit table

**Modified existing files (per-domain commits):**

- `api/openapi.yaml` — schema additions, named components, error declarations, basicAuth, polymorphic markers
- `internal/domain/<domain>/handler.go` — defect fixes per audit
- `internal/e2e/<domain>_test.go` — new happy-path tests for uncovered ops
- `internal/e2e/helpers_test.go` — `e2e.NewRequest(t, ...)` helper that captures test name
- `internal/e2e/e2e_test.go` — wires the validator middleware into the server handler
- `internal/e2e/.gitignore` — add `_openapi-conformance-report.md`
- `e2e/parity/client/types.go`, `e2e/parity/registry.go`, scenarios — update for corrected wire shapes
- `cmd/cyoda/help/content/openapi.md` — narrative pass

---

## Phase 0 — Pre-implementation reading

### Task 0.1: Verify spec, ADR, and prior-review context

No commit — orientation only.

- [ ] **Step 1: Read the design doc end-to-end**

`docs/superpowers/specs/2026-04-29-issue-21-openapi-conformance-design.md` — all 9 sections.

- [ ] **Step 2: Read ADR 0001**

`docs/adr/0001-openapi-server-spec-conformance.md` — confirms why runtime validation, why not strict-server, why not ogen.

- [ ] **Step 3: Skim review-04 (the convergence-signal review)**

`docs/superpowers/reviews/2026-04-29-issue-21-openapi-conformance-review-04.md` — confirms which empirical claims have been verified (Go test mechanics, `flag.Lookup("test.shuffle")`, `t.Errorf` panic semantics, dead-branch reasoning).

- [ ] **Step 4: Read the four files that the design's S1/S2 mitigation depends on**

```bash
grep -n "TestMain\|httptest.NewServer\|http.NewRequest\|http.DefaultClient" internal/e2e/e2e_test.go internal/e2e/helpers_test.go | head -30
```

Confirm:
- `serverURL` is a package-level `string` set in `TestMain` from `srv.URL`.
- Tests use `http.DefaultClient.Do(req)` or `http.Get(serverURL + "/...")` directly.
- `srv.Config.Handler = a.Handler()` is the insertion point for the validator wrapper.

---

## Commit 1 — Foundation: validator package, middleware, fixtures, test-name capture

**Goal:** Lay down the entire validator infrastructure in `ModeRecord`. Every existing test still passes; the validator collects mismatches against the (still-drifted) spec and writes the first report. Pattern-fixture unit tests prove the validator catches what we need it to catch.

### Task 1.1: Create the validator package skeleton with `Mode` constant

**Files:**
- Create: `internal/e2e/openapivalidator/mode.go`
- Create: `internal/e2e/openapivalidator/doc.go`

- [ ] **Step 1: Write `mode.go`**

`doc.go` (Step 2) carries the package narrative; `mode.go` does not repeat the package comment. Type and constants come before the derived `Mode` alias for top-down readability.

```go
package openapivalidator

// ModeKind is an int because comparing constants by string would tempt
// runtime configuration via env var, which we explicitly rejected (see ADR).
type ModeKind int

const (
	ModeRecord ModeKind = iota
	ModeEnforce
)

// Mode controls whether validation failures fail the suite.
//
// ModeRecord: collect mismatches, write the report file, do NOT fail.
// ModeEnforce: same, plus fail TestOpenAPIConformanceReport (full suite)
// or t.Errorf the requesting test (-run-filtered single-test workflow).
//
// Default is ModeRecord during the conformance work (commits 1-10 of #21).
// The final commit flips this to ModeEnforce. See
// docs/adr/0001-openapi-server-spec-conformance.md.
const Mode = ModeRecord
```

- [ ] **Step 2: Write `doc.go`**

```go
/*
Package openapivalidator validates HTTP responses captured during E2E tests
against the OpenAPI 3.1 spec embedded in github.com/cyoda-platform/cyoda-go/api.

Architecture:

  - middleware.go wraps the test server's http.Handler. Every response is
    captured via tee.go (an http.ResponseWriter that delegates Flusher,
    Hijacker, and ReaderFrom to the underlying writer).
  - validator.go runs the captured response through kin-openapi's
    openapi3filter.ValidateResponse with IncludeResponseStatus=true and
    MultiError=true.
  - collector.go appends mismatches to a process-level slice (mutex-guarded).
  - report.go writes the collector's contents to
    internal/e2e/_openapi-conformance-report.md after the suite ends.
  - testname.go plumbs the current *testing.T through request context so
    the middleware can call t.Errorf in -run-filtered enforce mode.

Streaming responses (application/x-ndjson) skip body validation but still
validate status code and headers. Detection uses the matched operation's
*declared* content-type for the actual status code, NOT the response's
Content-Type header (header-based detection is fragile against auto-sniffed
types and future SSE additions).

Mode is a compile-time constant (mode.go) — there is intentionally no env
var or runtime toggle. See docs/adr/0001-openapi-server-spec-conformance.md
for why.

This suite is incompatible with `go test -shuffle on`. The conformance test
detects shuffle and fails with an explanatory error.
*/
package openapivalidator
```

- [ ] **Step 3: Build to verify the package compiles**

```bash
go build ./internal/e2e/openapivalidator/...
```

Expected: no output (success).

- [ ] **Step 4: Commit**

```bash
git add internal/e2e/openapivalidator/mode.go internal/e2e/openapivalidator/doc.go
git commit -m "feat(e2e): scaffold openapivalidator package with Mode constant"
```

### Task 1.2: Implement the tee-writer

> **Implementation note (post-spec):** `httptest.NewRecorder()` IS an `http.Flusher` in Go 1.26, which made the original spec design (returning a `*teeWriter` only when none of the optional interfaces was supported) unworkable for tests using a bare recorder. Resolution: `Flush()` is implemented directly on `*teeWriter` as a delegate-or-noop, eliminating the `teeF` and `teeFH` variants. Only `teeH`, `teeR`, and `teeHR` remain as variants for the conditional Hijacker/ReaderFrom interfaces.

**Files:**
- Create: `internal/e2e/openapivalidator/tee.go`
- Test: `internal/e2e/openapivalidator/tee_test.go`

- [ ] **Step 1: Write the failing test**

```go
package openapivalidator

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTee_CapturesBytesAndForwards(t *testing.T) {
	rec := httptest.NewRecorder()
	tee := newTeeWriter(rec)

	tee.WriteHeader(201)
	if _, err := tee.Write([]byte(`{"ok":true}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := rec.Code; got != 201 {
		t.Errorf("forwarded status = %d, want 201", got)
	}
	if got := rec.Body.String(); got != `{"ok":true}` {
		t.Errorf("forwarded body = %q, want %q", got, `{"ok":true}`)
	}
	if got := tee.captured.String(); got != `{"ok":true}` {
		t.Errorf("captured body = %q, want %q", got, `{"ok":true}`)
	}
	if got := tee.status; got != 201 {
		t.Errorf("captured status = %d, want 201", got)
	}
}

type flusherRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flusherRecorder) Flush() { f.flushed = true; f.ResponseRecorder.Flush() }

func TestTee_DelegatesFlusher(t *testing.T) {
	rec := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	tee := newTeeWriter(rec)

	flusher, ok := tee.(http.Flusher)
	if !ok {
		t.Fatal("tee does not implement http.Flusher when underlying does")
	}
	flusher.Flush()
	if !rec.flushed {
		t.Error("Flush did not delegate to underlying writer")
	}
}

type readerFromRecorder struct {
	*httptest.ResponseRecorder
	readFromCalled bool
}

func (r *readerFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.readFromCalled = true
	return io.Copy(r.ResponseRecorder, src)
}

func TestTee_DelegatesReaderFrom(t *testing.T) {
	rec := &readerFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	tee := newTeeWriter(rec)

	rf, ok := tee.(io.ReaderFrom)
	if !ok {
		t.Fatal("tee does not implement io.ReaderFrom when underlying does")
	}
	src := strings.NewReader("hello")
	if _, err := rf.ReadFrom(src); err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if !rec.readFromCalled {
		t.Error("ReadFrom did not delegate to underlying writer")
	}
	// Captured bytes should still reflect what was written.
	if got := tee.(*teeWriter).captured.String(); got != "hello" {
		t.Errorf("captured = %q, want %q", got, "hello")
	}
}

type hijackerRecorder struct {
	*httptest.ResponseRecorder
}

func (h *hijackerRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}

func TestTee_DelegatesHijacker(t *testing.T) {
	rec := &hijackerRecorder{ResponseRecorder: httptest.NewRecorder()}
	tee := newTeeWriter(rec)

	if _, ok := tee.(http.Hijacker); !ok {
		t.Fatal("tee does not implement http.Hijacker when underlying does")
	}
}

func TestTee_DefaultStatusIs200(t *testing.T) {
	rec := httptest.NewRecorder()
	tee := newTeeWriter(rec).(*teeWriter)
	if _, err := tee.Write([]byte("body")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if tee.status != 200 {
		t.Errorf("default status after implicit WriteHeader = %d, want 200", tee.status)
	}
}

var _ = bytes.NewReader // keep import in case future tests need it
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/e2e/openapivalidator/ -run TestTee -v
```

Expected: build error — `newTeeWriter` undefined.

- [ ] **Step 3: Implement `tee.go`**

```go
package openapivalidator

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
)

// teeWriter wraps an http.ResponseWriter, forwarding writes to the underlying
// writer while also capturing the response status and body for validation.
//
// It conditionally delegates http.Flusher, http.Hijacker, and io.ReaderFrom
// to the underlying writer when supported. http.Pusher and
// http.CloseNotifier are intentionally NOT delegated:
//   - Pusher (HTTP/2 server push) is unused in cyoda-go.
//   - CloseNotifier is deprecated.
type teeWriter struct {
	w        http.ResponseWriter
	captured bytes.Buffer
	status   int
	written  bool
}

func newTeeWriter(w http.ResponseWriter) http.ResponseWriter {
	t := &teeWriter{w: w, status: http.StatusOK}
	// Pick the right wrapper based on which optional interfaces the
	// underlying writer implements.
	_, isFlusher := w.(http.Flusher)
	_, isHijacker := w.(http.Hijacker)
	_, isReaderFrom := w.(io.ReaderFrom)
	switch {
	case isFlusher && isHijacker && isReaderFrom:
		return &teeFHR{teeWriter: t}
	case isFlusher && isHijacker:
		return &teeFH{teeWriter: t}
	case isFlusher && isReaderFrom:
		return &teeFR{teeWriter: t}
	case isHijacker && isReaderFrom:
		return &teeHR{teeWriter: t}
	case isFlusher:
		return &teeF{teeWriter: t}
	case isHijacker:
		return &teeH{teeWriter: t}
	case isReaderFrom:
		return &teeR{teeWriter: t}
	default:
		return t
	}
}

func (t *teeWriter) Header() http.Header { return t.w.Header() }

func (t *teeWriter) Write(p []byte) (int, error) {
	if !t.written {
		t.written = true
	}
	t.captured.Write(p)
	return t.w.Write(p)
}

func (t *teeWriter) WriteHeader(code int) {
	t.status = code
	t.written = true
	t.w.WriteHeader(code)
}

// Flusher / Hijacker / ReaderFrom variants. We embed *teeWriter so all the
// base methods are inherited and we add the optional-interface methods
// only on the variants that need them.

type teeF struct{ *teeWriter }

func (t *teeF) Flush() { t.w.(http.Flusher).Flush() }

type teeH struct{ *teeWriter }

func (t *teeH) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return t.w.(http.Hijacker).Hijack()
}

type teeR struct{ *teeWriter }

func (t *teeR) ReadFrom(src io.Reader) (int64, error) {
	// Tee while delegating: copy through a tee-reader so captured grows.
	return t.w.(io.ReaderFrom).ReadFrom(io.TeeReader(src, &t.captured))
}

type teeFH struct{ *teeWriter }

func (t *teeFH) Flush()                                          { t.w.(http.Flusher).Flush() }
func (t *teeFH) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return t.w.(http.Hijacker).Hijack()
}

type teeFR struct{ *teeWriter }

func (t *teeFR) Flush() { t.w.(http.Flusher).Flush() }
func (t *teeFR) ReadFrom(src io.Reader) (int64, error) {
	return t.w.(io.ReaderFrom).ReadFrom(io.TeeReader(src, &t.captured))
}

type teeHR struct{ *teeWriter }

func (t *teeHR) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return t.w.(http.Hijacker).Hijack()
}
func (t *teeHR) ReadFrom(src io.Reader) (int64, error) {
	return t.w.(io.ReaderFrom).ReadFrom(io.TeeReader(src, &t.captured))
}

type teeFHR struct{ *teeWriter }

func (t *teeFHR) Flush()                                          { t.w.(http.Flusher).Flush() }
func (t *teeFHR) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return t.w.(http.Hijacker).Hijack()
}
func (t *teeFHR) ReadFrom(src io.Reader) (int64, error) {
	return t.w.(io.ReaderFrom).ReadFrom(io.TeeReader(src, &t.captured))
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/e2e/openapivalidator/ -run TestTee -v
```

Expected: PASS for all five tests.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/openapivalidator/tee.go internal/e2e/openapivalidator/tee_test.go
git commit -m "feat(e2e): add tee-writer that delegates Flusher/Hijacker/ReaderFrom"
```

### Task 1.3: Implement the test-name context plumbing

**Files:**
- Create: `internal/e2e/openapivalidator/testname.go`
- Test: `internal/e2e/openapivalidator/testname_test.go`

- [ ] **Step 1: Write the failing test**

```go
package openapivalidator

import (
	"context"
	"testing"
)

func TestWithTestT_RoundTrips(t *testing.T) {
	ctx := WithTestT(context.Background(), t)
	got := TestTFromContext(ctx)
	if got != t {
		t.Errorf("got %v, want %v", got, t)
	}
}

func TestTestTFromContext_NilWhenAbsent(t *testing.T) {
	if got := TestTFromContext(context.Background()); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./internal/e2e/openapivalidator/ -run TestWithTestT -v
```

Expected: build error (`WithTestT`, `TestTFromContext` undefined).

- [ ] **Step 3: Implement `testname.go`**

```go
package openapivalidator

import (
	"context"
	"testing"
)

type testTKey struct{}

// WithTestT attaches a *testing.T to ctx so the validator middleware
// can call t.Errorf when a -run-filtered enforce-mode validation fails.
//
// E2E tests should construct their requests via the e2e.NewRequest helper
// in helpers_test.go, which calls this function and attaches the request's
// context to the http.Request.
func WithTestT(ctx context.Context, t *testing.T) context.Context {
	return context.WithValue(ctx, testTKey{}, t)
}

// TestTFromContext returns the *testing.T attached via WithTestT, or nil
// if none is present (e.g. requests issued without going through the
// helper, or after the test has exited).
func TestTFromContext(ctx context.Context) *testing.T {
	t, _ := ctx.Value(testTKey{}).(*testing.T)
	return t
}
```

- [ ] **Step 4: Run to verify pass**

```bash
go test ./internal/e2e/openapivalidator/ -run TestWithTestT -v
go test ./internal/e2e/openapivalidator/ -run TestTestTFromContext -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/openapivalidator/testname.go internal/e2e/openapivalidator/testname_test.go
git commit -m "feat(e2e): add WithTestT/TestTFromContext for per-request test capture"
```

### Task 1.4: Implement the collector

**Files:**
- Create: `internal/e2e/openapivalidator/collector.go`
- Test: `internal/e2e/openapivalidator/collector_test.go`

- [ ] **Step 1: Write the failing test**

```go
package openapivalidator

import (
	"sync"
	"testing"
)

func TestCollector_AppendAndDrain(t *testing.T) {
	c := newCollector()
	c.append(Mismatch{Operation: "op1", Status: 200, Reason: "r1"})
	c.append(Mismatch{Operation: "op2", Status: 400, Reason: "r2"})

	out := c.drain()
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Operation != "op1" || out[1].Operation != "op2" {
		t.Errorf("ordering mismatch: %+v", out)
	}
	if remaining := c.drain(); len(remaining) != 0 {
		t.Errorf("collector not drained: %d remaining", len(remaining))
	}
}

func TestCollector_RecordExercised(t *testing.T) {
	c := newCollector()
	c.recordExercised("opA")
	c.recordExercised("opB")
	c.recordExercised("opA")
	exercised := c.exerciseSet()
	if len(exercised) != 2 {
		t.Errorf("got %d unique ops, want 2", len(exercised))
	}
	if !exercised["opA"] || !exercised["opB"] {
		t.Errorf("missing keys: %v", exercised)
	}
}

func TestCollector_ConcurrentAppend(t *testing.T) {
	c := newCollector()
	var wg sync.WaitGroup
	const n = 1000
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.append(Mismatch{Operation: "op"})
			c.recordExercised("op")
		}()
	}
	wg.Wait()
	if got := len(c.drain()); got != n {
		t.Errorf("got %d mismatches, want %d", got, n)
	}
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./internal/e2e/openapivalidator/ -run TestCollector -v
```

Expected: build error.

- [ ] **Step 3: Implement `collector.go`**

```go
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
	mu        sync.Mutex
	mismatches []Mismatch
	exercised map[string]struct{}
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
```

- [ ] **Step 4: Run to verify pass**

```bash
go test ./internal/e2e/openapivalidator/ -run TestCollector -v
```

Expected: all three tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/openapivalidator/collector.go internal/e2e/openapivalidator/collector_test.go
git commit -m "feat(e2e): add mismatch collector with exercised-ops tracking"
```

### Task 1.5: Implement the validator (router + ValidateResponse wrapper)

**Files:**
- Create: `internal/e2e/openapivalidator/validator.go`
- Test: `internal/e2e/openapivalidator/validator_test.go`

- [ ] **Step 1: Write the failing fixture-pinning test**

This is the **load-bearing fixture suite** — proves the validator catches each pattern we depend on. Per the design's Section 9 risk register row 1, the validator MUST catch all four #21 defects + the polymorphic and discriminator-union patterns before being trusted.

```go
package openapivalidator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// fixtureSpec returns a tiny in-memory OpenAPI 3.1 spec used by fixture tests.
func fixtureSpec(t *testing.T) *openapi3.T {
	t.Helper()
	const yaml = `openapi: 3.1.0
info: { title: fixture, version: "1" }
paths:
  /single:
    get:
      operationId: getSingle
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
                required: [transactionId]
                properties:
                  transactionId: { type: string }
                  entityIds: { type: array, items: { type: string } }
  /array:
    get:
      operationId: getArray
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: array
                items:
                  type: object
                  required: [transactionId]
                  properties:
                    transactionId: { type: string }
                    entityIds: { type: array, items: { type: string } }
  /envelope:
    get:
      operationId: getEnvelope
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
                required: [type, data, meta]
                properties:
                  type: { type: string }
                  data:
                    type: object
                    additionalProperties: true
                  meta:
                    type: object
                    additionalProperties: true
  /poly:
    get:
      operationId: getPoly
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
                additionalProperties: true
  /audit:
    get:
      operationId: getAudit
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                oneOf:
                  - $ref: '#/components/schemas/SMEvent'
                  - $ref: '#/components/schemas/CHEvent'
                discriminator:
                  propertyName: type
                  mapping:
                    StateMachine: '#/components/schemas/SMEvent'
                    EntityChange: '#/components/schemas/CHEvent'
components:
  schemas:
    SMEvent:
      type: object
      required: [type, transition]
      properties:
        type: { type: string, enum: [StateMachine] }
        transition: { type: string }
    CHEvent:
      type: object
      required: [type, fieldPath]
      properties:
        type: { type: string, enum: [EntityChange] }
        fieldPath: { type: string }
`
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(yaml))
	if err != nil {
		t.Fatalf("load fixture spec: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate fixture spec: %v", err)
	}
	return doc
}

func newFixtureValidator(t *testing.T) *Validator {
	t.Helper()
	v, err := NewValidator(fixtureSpec(t))
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v
}

func mkResp(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    nil,
	}
}

// Fixture #1 — POST returns array, spec says single object.
// Mirrors #21 confirmed defect.
func TestValidator_PostArrayShape(t *testing.T) {
	v := newFixtureValidator(t)
	req, _ := http.NewRequest("GET", "http://x/single", nil)
	resp := mkResp(200, "application/json", `[{"transactionId":"abc","entityIds":["e1"]}]`)
	mismatches := v.Validate(context.Background(), req, resp)
	if len(mismatches) == 0 {
		t.Fatal("expected mismatch for array-vs-object body, got none")
	}
}

// Fixture #2 — GET returns envelope, spec says raw entity (here: spec says
// envelope, server returns raw entity — same shape mismatch direction).
func TestValidator_EnvelopeMissingFields(t *testing.T) {
	v := newFixtureValidator(t)
	req, _ := http.NewRequest("GET", "http://x/envelope", nil)
	resp := mkResp(200, "application/json", `{"category":"physics"}`)
	mismatches := v.Validate(context.Background(), req, resp)
	if len(mismatches) == 0 {
		t.Fatal("expected mismatch for missing required envelope fields, got none")
	}
}

// Fixture #3 — JSON-in-string content. Spec declares object; server returns
// a JSON string holding the object's representation.
func TestValidator_JSONInStringContent(t *testing.T) {
	v := newFixtureValidator(t)
	req, _ := http.NewRequest("GET", "http://x/single", nil)
	resp := mkResp(200, "application/json", `"{\"transactionId\":\"abc\"}"`)
	mismatches := v.Validate(context.Background(), req, resp)
	if len(mismatches) == 0 {
		t.Fatal("expected mismatch for stringified object body, got none")
	}
}

// Fixture #4 — undeclared status code. Catches the IncludeResponseStatus
// load-bearing claim.
func TestValidator_UndeclaredStatus(t *testing.T) {
	v := newFixtureValidator(t)
	req, _ := http.NewRequest("GET", "http://x/single", nil)
	resp := mkResp(418, "application/json", `{}`)
	mismatches := v.Validate(context.Background(), req, resp)
	if len(mismatches) == 0 {
		t.Fatal("expected mismatch for undeclared status 418, got none")
	}
}

// Fixture #5 — polymorphic body (additionalProperties: true) accepts ANY
// JSON shape. The validator must NOT raise a mismatch for arbitrary content.
func TestValidator_PolymorphicAcceptsAnyJSON(t *testing.T) {
	v := newFixtureValidator(t)
	for _, body := range []string{
		`{}`,
		`{"a":1,"b":[1,2,3],"c":{"nested":"object"}}`,
		`{"surprising":["completely","unexpected","fields"]}`,
	} {
		req, _ := http.NewRequest("GET", "http://x/poly", nil)
		resp := mkResp(200, "application/json", body)
		mismatches := v.Validate(context.Background(), req, resp)
		if len(mismatches) > 0 {
			t.Errorf("body %q raised %d mismatches; expected 0", body, len(mismatches))
			for _, m := range mismatches {
				t.Errorf("  - %s", m.Reason)
			}
		}
	}
}

// Fixture #6 — discriminator union accepts each declared variant; rejects
// undeclared.
func TestValidator_DiscriminatorVariants(t *testing.T) {
	v := newFixtureValidator(t)
	cases := []struct {
		name        string
		body        string
		wantMismatch bool
	}{
		{"sm", `{"type":"StateMachine","transition":"approve"}`, false},
		{"ch", `{"type":"EntityChange","fieldPath":"a.b"}`, false},
		{"undeclared", `{"type":"System","payload":"x"}`, true},
		{"sm-missing-required", `{"type":"StateMachine"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", "http://x/audit", nil)
			resp := mkResp(200, "application/json", tc.body)
			mismatches := v.Validate(context.Background(), req, resp)
			if tc.wantMismatch && len(mismatches) == 0 {
				t.Errorf("expected mismatch for body %q, got none", tc.body)
			}
			if !tc.wantMismatch && len(mismatches) > 0 {
				t.Errorf("unexpected mismatch for body %q: %+v", tc.body, mismatches)
			}
		})
	}
}

// Helpers used by the fixture tests above.
var _ = bytes.NewReader
var _ = json.Marshal
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./internal/e2e/openapivalidator/ -run TestValidator_ -v
```

Expected: build error — `Validator`, `NewValidator`, `Validate` undefined.

- [ ] **Step 3: Implement `validator.go`**

```go
package openapivalidator

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

// Validator wraps the spec's router and validates HTTP responses against
// the matched operation's declared schema.
//
// IncludeResponseStatus=true is load-bearing: openapi3filter's default
// behavior is to silently pass undeclared status codes (verified against
// kin-openapi v0.137.0 openapi3filter/validate_response.go:48-58). Without
// this flag the validator misses an entire class of drift.
//
// MultiError=true accumulates all schema errors per response rather than
// failing on the first.
type Validator struct {
	doc    *openapi3.T
	router routers.Router
	opts   *openapi3filter.Options
}

// NewValidator builds a Validator from a parsed OpenAPI 3.1 document.
func NewValidator(doc *openapi3.T) (*Validator, error) {
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		return nil, fmt.Errorf("build router: %w", err)
	}
	return &Validator{
		doc:    doc,
		router: router,
		opts: &openapi3filter.Options{
			IncludeResponseStatus: true,
			MultiError:            true,
			AuthenticationFunc: func(ctx context.Context, ai *openapi3filter.AuthenticationInput) error {
				return nil // skip auth checks; we validate shape only
			},
		},
	}, nil
}

// Validate runs the response through openapi3filter.ValidateResponse and
// returns any mismatches it finds. Returns an empty slice on success.
//
// Records the matched operationId in the package's exercised set, regardless
// of whether validation passed.
func (v *Validator) Validate(ctx context.Context, req *http.Request, resp *http.Response) []Mismatch {
	route, _, err := v.router.FindRoute(req)
	if err != nil {
		// No matching route — the request hit a path the spec doesn't declare.
		// This is a real mismatch (handler exists for an undeclared route).
		return []Mismatch{{
			Method: req.Method,
			Path:   req.URL.Path,
			Status: resp.StatusCode,
			Reason: fmt.Sprintf("no spec route matches %s %s: %v", req.Method, req.URL.Path, err),
		}}
	}
	opId := route.Operation.OperationID
	defaultCollector.recordExercised(opId)

	// Streaming check: if the matched operation declares
	// application/x-ndjson for the actual status code, skip body validation.
	if v.isStreaming(route, resp.StatusCode) {
		// Still validate that the status code is declared at all.
		input := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{
				Request: req,
				Route:   route,
				Options: v.opts,
			},
			Status: resp.StatusCode,
			Header: resp.Header,
		}
		if err := openapi3filter.ValidateResponse(ctx, input); err != nil {
			return v.toMismatches(err, opId, req, resp.StatusCode)
		}
		return nil
	}

	// Read response body for validation. The middleware passed the captured
	// bytes via resp.Body; we consume them here.
	//
	// IMPORTANT: Options must be set on ResponseValidationInput (not on the
	// nested RequestValidationInput.Options) — ValidateResponse reads from
	// input.Options directly. Verified by fixture test #4 (TestValidator_
	// UndeclaredStatus); a misplaced Options field silently lets undeclared
	// status codes through.
	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: req,
			Route:   route,
		},
		Status:  resp.StatusCode,
		Header:  resp.Header,
		Body:    resp.Body,
		Options: v.opts,
	}
	if err := openapi3filter.ValidateResponse(ctx, input); err != nil {
		return v.toMismatches(err, opId, req, resp.StatusCode)
	}
	return nil
}

// isStreaming reports whether the matched operation declares
// application/x-ndjson for the given status code.
func (v *Validator) isStreaming(route *routers.Route, status int) bool {
	if route.Operation == nil || route.Operation.Responses == nil {
		return false
	}
	resp := route.Operation.Responses.Status(status)
	if resp == nil || resp.Value == nil {
		return false
	}
	for ct := range resp.Value.Content {
		if ct == "application/x-ndjson" {
			return true
		}
	}
	return false
}

// toMismatches converts the kin-openapi error tree into one or more Mismatch
// records. MultiError is unwrapped so each schema problem becomes its own row.
func (v *Validator) toMismatches(err error, opId string, req *http.Request, status int) []Mismatch {
	var multi openapi3.MultiError
	if errors.As(err, &multi) {
		out := make([]Mismatch, 0, len(multi))
		for _, e := range multi {
			out = append(out, Mismatch{
				Operation: opId,
				Method:    req.Method,
				Path:      req.URL.Path,
				Status:    status,
				Reason:    e.Error(),
			})
		}
		return out
	}
	return []Mismatch{{
		Operation: opId,
		Method:    req.Method,
		Path:      req.URL.Path,
		Status:    status,
		Reason:    err.Error(),
	}}
}
```

- [ ] **Step 4: Run the fixture suite**

```bash
go test ./internal/e2e/openapivalidator/ -run TestValidator_ -v
```

Expected: all six fixture tests PASS. **If any fail, stop and surface — the validator does not catch a pattern the design depends on.**

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/openapivalidator/validator.go internal/e2e/openapivalidator/validator_test.go
git commit -m "feat(e2e): add Validator with kin-openapi response validation + fixture suite"
```

### Task 1.6: Implement the middleware

**Files:**
- Create: `internal/e2e/openapivalidator/middleware.go`
- Test: `internal/e2e/openapivalidator/middleware_test.go`

- [ ] **Step 1: Write the failing test**

```go
package openapivalidator

import (
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiddleware_ForwardsResponseAndCaptures(t *testing.T) {
	v := newFixtureValidator(t)
	defaultCollector = newCollector() // reset for test

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"transactionId":"abc","entityIds":["e1"]}`)
	})
	wrapped := NewMiddleware(v)(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/single", nil)
	wrapped.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("forwarded status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"transactionId"`) {
		t.Errorf("body not forwarded: %q", rec.Body.String())
	}
	exercised := defaultCollector.exerciseSet()
	if !exercised["getSingle"] {
		t.Errorf("getSingle not recorded as exercised: %v", exercised)
	}
	if got := len(defaultCollector.drain()); got != 0 {
		t.Errorf("expected 0 mismatches, got %d", got)
	}
}

func TestMiddleware_RecordsMismatchOnDriftedResponse(t *testing.T) {
	v := newFixtureValidator(t)
	defaultCollector = newCollector()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `[{"transactionId":"abc","entityIds":["e1"]}]`) // array, but spec says single object
	})
	wrapped := NewMiddleware(v)(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/single", nil)
	wrapped.ServeHTTP(rec, req)

	mismatches := defaultCollector.drain()
	if len(mismatches) == 0 {
		t.Fatal("expected mismatch for array-vs-object response, got none")
	}
	if mismatches[0].Operation != "getSingle" {
		t.Errorf("operation = %q, want getSingle", mismatches[0].Operation)
	}
}

// In ModeRecord, the middleware never calls t.Errorf on the captured *T,
// even if a -run filter is active.
func TestMiddleware_RecordModeNeverFailsTest(t *testing.T) {
	if Mode != ModeRecord {
		t.Skip("test assumes default Mode == ModeRecord; skipped after the commit-11 flip")
	}
	v := newFixtureValidator(t)
	defaultCollector = newCollector()

	// Force the run-filter detection to true for this test by passing a
	// pretend filter via flag.
	if err := flag.Set("test.run", "Forced"); err != nil {
		t.Fatalf("set test.run: %v", err)
	}
	defer flag.Set("test.run", "")

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `[{"oops":"array"}]`)
	})
	wrapped := NewMiddleware(v)(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/single", nil)
	ctx := WithTestT(req.Context(), t) // captured *T is the surrounding test
	req = req.WithContext(ctx)
	wrapped.ServeHTTP(rec, req)

	// In record mode this test must NOT fail despite the drift. Verified by
	// the fact that the test reaches this point without t.Errorf having
	// fired (would have marked it failed).
	if t.Failed() {
		t.Fatal("middleware called t.Errorf in record mode; should only fire in enforce mode")
	}
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./internal/e2e/openapivalidator/ -run TestMiddleware -v
```

Expected: build error.

- [ ] **Step 3: Implement `middleware.go`**

```go
package openapivalidator

import (
	"bytes"
	"flag"
	"io"
	"net/http"
	"strings"
)

// runFilterActive returns true if the suite was started with -run set to
// any non-empty value. Computed once on first call.
func runFilterActive() bool {
	f := flag.Lookup("test.run")
	if f == nil {
		return false
	}
	return f.Value.String() != ""
}

// NewMiddleware returns an http.Handler middleware that:
//
//  1. Wraps the response writer with a teeWriter to capture status + body.
//  2. Calls the wrapped handler.
//  3. Synthesizes an *http.Response from the captured bytes and runs it
//     through the validator.
//  4. Appends mismatches to the package-level collector.
//  5. In ModeEnforce + -run-filtered runs: also calls t.Errorf on the
//     captured *testing.T (if any) so the requesting test fails immediately.
//     Wrapped in defer recover() in case the test has exited (fire-and-
//     forget pattern; captured *T may no longer be valid).
func NewMiddleware(v *Validator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tw := newTeeWriter(w).(interface {
				http.ResponseWriter
				captureBytes() []byte
				captureStatus() int
			})
			next.ServeHTTP(tw, r)

			// Build a synthetic *http.Response for the validator.
			resp := &http.Response{
				StatusCode: tw.captureStatus(),
				Header:     w.Header(),
				Body:       io.NopCloser(bytes.NewReader(tw.captureBytes())),
				Request:    r,
			}
			mismatches := v.Validate(r.Context(), r, resp)
			for _, m := range mismatches {
				if t := TestTFromContext(r.Context()); t != nil {
					m.TestName = t.Name()
				} else {
					m.TestName = "unknown"
				}
				defaultCollector.append(m)

				if Mode == ModeEnforce && runFilterActive() {
					func() {
						defer func() { _ = recover() }()
						if t := TestTFromContext(r.Context()); t != nil {
							t.Errorf("openapi conformance: %s %s -> %d: %s",
								m.Method, m.Path, m.Status,
								strings.TrimSpace(m.Reason))
						}
					}()
				}
			}
		})
	}
}
```

To support the test, `teeWriter` needs to expose its captured bytes and status. Add accessor methods to `tee.go`:

```go
// Add to tee.go (alongside existing methods):
func (t *teeWriter) captureBytes() []byte { return t.captured.Bytes() }
func (t *teeWriter) captureStatus() int   { return t.status }
```

And mirror them on each variant struct (Go's embedding handles this since each variant embeds `*teeWriter`).

- [ ] **Step 4: Run the tests**

```bash
go test ./internal/e2e/openapivalidator/ -run TestMiddleware -v
```

Expected: all three tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/openapivalidator/middleware.go internal/e2e/openapivalidator/middleware_test.go internal/e2e/openapivalidator/tee.go
git commit -m "feat(e2e): add validator middleware with mode-aware enforcement"
```

### Task 1.7: Implement the report writer

**Files:**
- Create: `internal/e2e/openapivalidator/report.go`
- Test: `internal/e2e/openapivalidator/report_test.go`

- [ ] **Step 1: Write the failing test**

```go
package openapivalidator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReport_FormatsMismatches(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "report.md")

	mm := []Mismatch{
		{Operation: "getOne", Method: "GET", Path: "/x/1", Status: 200,
			Reason: "missing required field 'transactionId'", TestName: "TestEntity_Create"},
		{Operation: "create", Method: "POST", Path: "/x", Status: 418,
			Reason: "status not declared", TestName: "TestEntity_BadCase"},
	}
	exercised := map[string]bool{"getOne": true, "create": true}
	all := []string{"getOne", "create", "deleteOne"} // deleteOne is uncovered

	if err := WriteReport(out, mm, exercised, all); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(got)

	for _, must := range []string{
		"OpenAPI Conformance Report",
		"# Mismatches (2)",
		"GET /x/1 -> 200",
		"POST /x -> 418",
		"# Uncovered Operations (1)",
		"deleteOne",
	} {
		if !strings.Contains(body, must) {
			t.Errorf("report missing %q\n--- got ---\n%s", must, body)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./internal/e2e/openapivalidator/ -run TestWriteReport -v
```

Expected: build error — `WriteReport` undefined.

- [ ] **Step 3: Implement `report.go`**

```go
package openapivalidator

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// WriteReport renders the conformance report to path. Returns nil even if
// there are mismatches — the report file always reflects the current state;
// failure handling is the caller's responsibility.
func WriteReport(path string, mismatches []Mismatch, exercised map[string]bool, allOps []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# OpenAPI Conformance Report\n\n")
	fmt.Fprintf(&b, "Generated: %s\n\n", time.Now().UTC().Format(time.RFC3339))

	fmt.Fprintf(&b, "## Mismatches (%d)\n\n", len(mismatches))
	if len(mismatches) == 0 {
		fmt.Fprintln(&b, "_None._\n")
	} else {
		// Group by operation for readability.
		byOp := map[string][]Mismatch{}
		for _, m := range mismatches {
			byOp[m.Operation] = append(byOp[m.Operation], m)
		}
		ops := make([]string, 0, len(byOp))
		for op := range byOp {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		for _, op := range ops {
			fmt.Fprintf(&b, "### %s\n\n", op)
			for _, m := range byOp[op] {
				fmt.Fprintf(&b, "- `%s %s -> %d`", m.Method, m.Path, m.Status)
				if m.TestName != "" && m.TestName != "unknown" {
					fmt.Fprintf(&b, " (test: `%s`)", m.TestName)
				}
				fmt.Fprintf(&b, "\n  - %s\n", m.Reason)
			}
			fmt.Fprintln(&b)
		}
	}

	uncovered := []string{}
	for _, op := range allOps {
		if !exercised[op] {
			uncovered = append(uncovered, op)
		}
	}
	sort.Strings(uncovered)
	fmt.Fprintf(&b, "## Uncovered Operations (%d)\n\n", len(uncovered))
	if len(uncovered) == 0 {
		fmt.Fprintln(&b, "_All declared operations were exercised._")
	} else {
		for _, op := range uncovered {
			fmt.Fprintf(&b, "- %s\n", op)
		}
	}

	return os.WriteFile(path, []byte(b.String()), 0o644)
}
```

- [ ] **Step 4: Run the test**

```bash
go test ./internal/e2e/openapivalidator/ -run TestWriteReport -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/openapivalidator/report.go internal/e2e/openapivalidator/report_test.go
git commit -m "feat(e2e): add markdown report writer for conformance findings"
```

### Task 1.8: Add the test-name capture helper to `helpers_test.go` and migrate existing E2E tests

**Files:**
- Modify: `internal/e2e/helpers_test.go` (add helper, update `authRequest`)
- Modify: every `internal/e2e/*_test.go` that constructs `*http.Request` directly

- [ ] **Step 1: Inventory the migration scope**

```bash
grep -rn "http.NewRequest\b\|http.Get(\|http.Post(\|http.DefaultClient.Do" internal/e2e/ | grep -v helpers_test.go | wc -l
```

Note the count. Expected: ~60-100 sites across ~14 files.

- [ ] **Step 2: Write a test that pins the helper's behavior**

Add to `internal/e2e/helpers_test.go`:

```go
// (in helpers_test.go — add this near the top)

// e2eNewRequest creates an http.Request with the test name attached to the
// request context via openapivalidator.WithTestT. The validator middleware
// uses the captured *testing.T to call t.Errorf in -run-filtered enforce
// mode (see openapivalidator/doc.go).
func e2eNewRequest(t *testing.T, method, urlStr string, body io.Reader) (*http.Request, error) {
	t.Helper()
	req, err := http.NewRequest(method, urlStr, body)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(openapivalidator.WithTestT(req.Context(), t))
	return req, nil
}
```

Add the import:

```go
import (
	// ... existing imports ...
	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
)
```

- [ ] **Step 3: Update `authRequest` and `getToken` to use the helper**

In `internal/e2e/helpers_test.go`, change:

```go
req, err := http.NewRequest("POST", serverURL+"/api/oauth/token", strings.NewReader(data.Encode()))
```

to:

```go
req, err := e2eNewRequest(t, "POST", serverURL+"/api/oauth/token", strings.NewReader(data.Encode()))
```

And in `authRequest`:

```go
req, err := http.NewRequest(method, serverURL+path, body)
```

becomes:

```go
req, err := e2eNewRequest(t, method, serverURL+path, body)
```

- [ ] **Step 4: Migrate every other call site**

For each `*_test.go` in `internal/e2e/`, replace direct `http.NewRequest` / `http.Get` / `http.Post` calls with `e2eNewRequest` constructing the request and then `http.DefaultClient.Do(req)`. Tests that already use `authRequest` / `doAuth` are already covered.

The mechanical pattern:

```go
// before
resp, err := http.Get(serverURL + "/api/health")

// after
req, err := e2eNewRequest(t, "GET", serverURL+"/api/health", nil)
if err != nil {
    t.Fatalf("new request: %v", err)
}
resp, err := http.DefaultClient.Do(req)
```

For each file, run the file's tests after editing to confirm nothing broke.

- [ ] **Step 5: Build and run all E2E tests to verify nothing broke**

```bash
go test ./internal/e2e/... -v -count=1
```

Expected: all existing tests still pass (validator middleware is not yet wired — this commit only adds the helper).

- [ ] **Step 6: Commit**

```bash
git add internal/e2e/helpers_test.go internal/e2e/*_test.go
git commit -m "refactor(e2e): route all test requests through e2eNewRequest helper"
```

### Task 1.9: Wire the validator into `TestMain`, add the conformance test file, gitignore the report

**Files:**
- Modify: `internal/e2e/e2e_test.go`
- Create: `internal/e2e/zzz_openapi_conformance_test.go`
- Create: `internal/e2e/.gitignore`

- [ ] **Step 1: Add `.gitignore`**

```bash
cat > internal/e2e/.gitignore <<'EOF'
# Generated by openapivalidator at end of every E2E run.
_openapi-conformance-report.md
EOF
```

- [ ] **Step 2: Modify `TestMain` to wrap the handler with the validator middleware and parse the spec**

Edit `internal/e2e/e2e_test.go`. After the existing imports, add:

```go
import (
	// ... existing ...
	"github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
)
```

Replace the line `srv.Config.Handler = a.Handler()` with:

```go
// Build the conformance validator from the embedded spec. Wraps the
// production handler; failures collected end-to-end and reported by
// TestOpenAPIConformanceReport (zzz_openapi_conformance_test.go).
swagger, err := api.GetSwagger()
if err != nil {
    log.Fatalf("get swagger: %v", err)
}
validator, err := openapivalidator.NewValidator(swagger)
if err != nil {
    log.Fatalf("build validator: %v", err)
}
srv.Config.Handler = openapivalidator.NewMiddleware(validator)(a.Handler())
```

Also add a package-level `validator` accessor for the conformance test to read all operationIds from the spec:

```go
var allOperationIds []string

// Inside TestMain, after building swagger:
for _, item := range swagger.Paths.Map() {
    for _, op := range item.Operations() {
        if op.OperationID != "" {
            allOperationIds = append(allOperationIds, op.OperationID)
        }
    }
}
```

- [ ] **Step 3: Create the conformance test file**

```go
// internal/e2e/zzz_openapi_conformance_test.go
//
// Filename intentionally starts with "zzz_" so this file processes LAST in
// alphabetical ordering — Go runs tests in source-declaration order within
// a file, processing files in alphabetical filename order. Function name
// has no effect on ordering. See
// docs/superpowers/specs/2026-04-29-issue-21-openapi-conformance-design.md
// Section 2 for the rationale.

package e2e_test

import (
	"flag"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
)

// TestOpenAPIConformanceReport runs after every other E2E test, drains the
// validator's collector, writes the markdown report, and (in ModeEnforce)
// fails if any mismatches were collected.
func TestOpenAPIConformanceReport(t *testing.T) {
	// `-shuffle on` defeats the file-ordering trick that ensures this test
	// runs last. Detect and bail out cleanly.
	if v := flag.Lookup("test.shuffle"); v != nil && v.Value.String() != "off" {
		t.Fatalf("openapi conformance suite is not compatible with -shuffle; rerun without it")
	}

	mismatches, exercised := openapivalidator.DrainAndExercised()
	reportPath := filepath.Join("_openapi-conformance-report.md")
	if err := openapivalidator.WriteReport(reportPath, mismatches, exercised, allOperationIds); err != nil {
		t.Fatalf("write report: %v", err)
	}

	t.Logf("openapi conformance report: %s (%d mismatches)", reportPath, len(mismatches))

	if openapivalidator.Mode != openapivalidator.ModeEnforce {
		// Record mode: report-only.
		return
	}

	if len(mismatches) == 0 {
		// Enforce mode, no mismatches: also check coverage. Skip the
		// coverage check when -run is set (single-test workflow).
		if !runFilterSet() {
			uncovered := []string{}
			for _, op := range allOperationIds {
				if !exercised[op] {
					uncovered = append(uncovered, op)
				}
			}
			if len(uncovered) > 0 {
				t.Fatalf("openapi conformance: %d operations have no E2E coverage; see %s",
					len(uncovered), reportPath)
			}
		}
		return
	}

	// Enforce mode, mismatches present: fail with summary of first 20.
	limit := len(mismatches)
	if limit > 20 {
		limit = 20
	}
	var summary string
	for _, m := range mismatches[:limit] {
		summary += fmt.Sprintf("\n  %s %s -> %d: %s", m.Method, m.Path, m.Status, m.Reason)
	}
	t.Fatalf("openapi conformance: %d mismatches (first %d shown); full report at %s%s",
		len(mismatches), limit, reportPath, summary)
}

func runFilterSet() bool {
	f := flag.Lookup("test.run")
	return f != nil && f.Value.String() != ""
}
```

Add a small accessor to the validator package so the conformance test can drain:

```go
// internal/e2e/openapivalidator/collector.go — add at the end:

// DrainAndExercised drains the package-level collector and returns the
// snapshot together with the exercised-operations set.
func DrainAndExercised() ([]Mismatch, map[string]bool) {
	return defaultCollector.drain(), defaultCollector.exerciseSet()
}
```

- [ ] **Step 4: Build and run a single existing E2E test to verify the wiring**

```bash
go test ./internal/e2e/... -run TestHealth -v
```

Expected: PASS. The health test issues a `GET /api/health`; the validator records it as exercised; the conformance test runs but `-run TestHealth` excludes it from this invocation (which is fine — single-test workflow).

- [ ] **Step 5: Run the full E2E suite to see the first conformance report**

```bash
go test ./internal/e2e/... -v -count=1
```

Expected: existing tests PASS, `TestOpenAPIConformanceReport` PASSES (mode is `ModeRecord`, so even with mismatches it doesn't fail). The report file `internal/e2e/_openapi-conformance-report.md` should now exist with the mismatch list.

- [ ] **Step 6: Inspect the report**

```bash
cat internal/e2e/_openapi-conformance-report.md | head -80
```

Expected: a markdown report with mismatches grouped by operation, plus an "Uncovered Operations" section. **This list is the input to the audit table (Task 2.1) and the per-domain commits.**

- [ ] **Step 7: Commit**

```bash
git add internal/e2e/.gitignore internal/e2e/e2e_test.go internal/e2e/zzz_openapi_conformance_test.go internal/e2e/openapivalidator/collector.go
git commit -m "feat(e2e): wire openapi validator into TestMain + conformance test (record mode)"
```

---

## Commit 2 — Audit table

### Task 2.1: Populate the per-operation audit table

**Files:**
- Create: `docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md`

The table cross-checks the human-recorded "server response" column against the validator's record-mode output (Section 3 of the spec). Process per the spec.

- [ ] **Step 1: Run the validator to produce the cross-check input**

```bash
go test ./internal/e2e/... -v -count=1 2>&1 | tail -20
cat internal/e2e/_openapi-conformance-report.md
```

Keep the report open while filling in the audit table.

- [ ] **Step 2: Enumerate every operationId in `api/openapi.yaml`**

```bash
python3 - <<'EOF'
import yaml
with open('api/openapi.yaml') as f:
    spec = yaml.safe_load(f)
rows = []
for path, item in spec['paths'].items():
    for method, op in item.items():
        if isinstance(op, dict) and 'operationId' in op:
            rows.append((op['operationId'], method.upper(), path, op.get('tags', ['?'])[0] if op.get('tags') else '?'))
for r in sorted(rows):
    print(*r, sep='\t')
EOF
```

Expected: ~81 rows.

- [ ] **Step 3: For each operationId, find the implementing handler**

```bash
grep -n "func (h \*Handler) <OperationName>" internal/domain/*/handler.go
```

(Method names in `api/generated.go`'s `ServerInterface` match the handler method names exactly; cross-reference there if grep misses.)

- [ ] **Step 4: For each operationId, characterize the spec response and the actual server response**

Read each handler's `WriteJSON` / `WriteError` calls. Read each spec block's `responses.<status>.content.application/json.schema`. Use the validator report to cross-check.

- [ ] **Step 5: Write the audit table**

```markdown
# OpenAPI Conformance Audit — 2026-04-29

Per #21 design Section 3. One row per operationId. Disposition values:
`match`, `fix-spec`, `fix-server`, `fix-both`. `resolved-by-commit`
filled in as commits land.

| operationId | method | path | handler | spec response (today) | server response (today) | disposition | resolved-by-commit |
|---|---|---|---|---|---|---|---|
| accountGet | GET | /account | `internal/domain/account/handler.go:42` | `AccountInfo` (loose object) | `{tenantId, userId, ...}` | _TBD_ | |
| ... | | | | | | | |
```

(81 rows total. Disposition starts empty — filled in during per-domain commits.)

- [ ] **Step 6: Commit**

```bash
git add docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md
git commit -m "docs(audit): add per-operation OpenAPI conformance audit table"
```

---

## Commits 3-10 — Per-domain spec + handler + tests

Each per-domain commit follows the same pattern. Domains in alphabetical order per design Section 8: account / IAM, audit, dispatch / health, entity, messaging, model, search, workflow.

### Task 3.1: account / IAM domain — spec changes

**Files:**
- Modify: `api/openapi.yaml`
- Modify: `docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md` (fill in dispositions)

- [ ] **Step 1: List the operations in scope**

From the audit table, all rows tagged `Account` and `IAM` (~10 operations: `accountGet`, `accountSubscriptionsGet`, `listTechnicalUsers`, `createTechnicalUser`, `deleteTechnicalUser`, `resetTechnicalUserSecret`, `getCurrentJwtKeyPair`, `listOidcProviders`, `getTechnicalUserToken`, etc.).

- [ ] **Step 2: For each operation in the audit table, apply the disposition**

For each `fix-spec` row: open `api/openapi.yaml`, replace the loose `type: object` block with the appropriate named schema or `additionalProperties: true` + `description: polymorphic by intent`.

For each `fix-server` row: open the handler, fix the wire shape (TDD: write a failing test that asserts the corrected wire shape; run; fix the handler; run; pass).

For each operation: declare per-emitted error statuses via the `components.responses` shared fragments:

```yaml
responses:
  "200":
    description: ok
    content:
      application/json:
        schema:
          $ref: '#/components/schemas/AccountInfo'
  "401":
    $ref: '#/components/responses/Unauthorized'
  "403":
    $ref: '#/components/responses/Forbidden'
  default:
    $ref: '#/components/responses/InternalServerError'
```

If `components/responses` doesn't yet have `Unauthorized` / `Forbidden` / `InternalServerError`, add them in this commit:

```yaml
components:
  responses:
    Unauthorized:
      description: Unauthorized — missing or invalid bearer token
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/ProblemDetail'
    Forbidden:
      description: Forbidden — caller does not have permission for this resource
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/ProblemDetail'
    InternalServerError:
      description: Internal Server Error — generic message with ticketId for correlation
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/ProblemDetail'
```

- [ ] **Step 3: Add the basicAuth security scheme** (if not already present from a prior commit)

Search `api/openapi.yaml:9197-9202` for `securitySchemes`. Add the `basicAuth` declaration:

```yaml
components:
  securitySchemes:
    basicAuth:
      type: http
      scheme: basic
    bearerAuth:
      type: http
      scheme: bearer
      bearerFormat: JWT
      name: bearerAuth
```

- [ ] **Step 4: Add E2E happy-path tests for any uncovered operations in this domain**

For each operationId reported as uncovered by the validator: add a minimal happy-path test in the appropriate `internal/e2e/<domain>_test.go`. If no domain file exists, create `internal/e2e/account_test.go`.

Pattern:

```go
func TestAccount_Get(t *testing.T) {
    req := authRequest(t, "GET", "/api/account", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("GET /account: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        body, _ := io.ReadAll(resp.Body)
        t.Fatalf("status=%d body=%s", resp.StatusCode, body)
    }
    // Validator middleware will check the response shape against the spec.
}
```

- [ ] **Step 5: Regenerate `api/generated.go`**

```bash
go generate ./api/...
```

Expected: `api/generated.go` updates with the new types.

- [ ] **Step 6: Update `e2e/parity/client/types.go` and parity scenarios for any wire-shape changes in this domain**

```bash
grep -rn "Account\|TechnicalUser\|JwtKeyPair\|OidcProvider" e2e/parity/ --include="*.go" | head -20
```

For each file that references these types: update to match the corrected wire shape.

- [ ] **Step 7: Build and run E2E tests**

```bash
go build ./...
go test ./internal/e2e/... -v -count=1 -run "TestAccount|TestTechnicalUser|TestOidc|TestJwt"
```

Expected: all account/IAM tests PASS; the conformance report shows fewer mismatches than before for this domain.

- [ ] **Step 8: Update the audit table — fill in disposition + resolved-by-commit for every account/IAM row**

- [ ] **Step 9: Commit**

```bash
git add api/openapi.yaml api/generated.go internal/domain/account/ internal/domain/iam/ \
        internal/e2e/account_test.go internal/e2e/iam_test.go \
        e2e/parity/ docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md
git commit -m "fix(account+iam): reconcile spec with server, add E2E coverage, declare error responses"
```

### Task 4.1: audit domain — spec changes

Same structure as Task 3.1 but for the audit domain (~4 operations). The `AuditEvent` discriminator-union schema is the new piece — declare it as:

```yaml
AuditEvent:
  oneOf:
    - $ref: '#/components/schemas/StateMachineAuditEvent'
    - $ref: '#/components/schemas/EntityChangeAuditEvent'
    - $ref: '#/components/schemas/SystemAuditEvent'
  discriminator:
    propertyName: type
    mapping:
      StateMachine: '#/components/schemas/StateMachineAuditEvent'
      EntityChange: '#/components/schemas/EntityChangeAuditEvent'
      System: '#/components/schemas/SystemAuditEvent'
```

with the three variant schemas defined alongside.

If `oapi-codegen` v2.6.0's emission of this oneOf+discriminator is unworkable (per design Section 9 risk register row 1's fallback), collapse to a single concrete type with `type` + `Payload json.RawMessage` and document the schema as `oneOf` for clients but a tagged-union for server. Decision criterion: if `go build ./...` succeeds with the natural form, keep it; otherwise apply the fallback within the same commit.

- [ ] **Step 1-9:** Mirror Task 3.1's structure for the audit domain operations.

- [ ] **Step 10: Commit**

```bash
git commit -m "fix(audit): reconcile spec with server, add AuditEvent discriminator union"
```

### Task 5.1: dispatch / health domain — spec changes

Trivial domain (~4 operations, mostly health/readiness probes). Same structure as Task 3.1, smaller scope.

- [ ] **Step 1-9:** Mirror Task 3.1.

- [ ] **Step 10: Commit**

```bash
git commit -m "fix(dispatch+health): reconcile spec with server"
```

### Task 6.1: entity domain — spec changes

Largest domain (~14 operations). Includes the original #21 confirmed defects:

- POST `/entity/{format}/{entityName}/{modelVersion}` returns array, spec says single object — fix the spec via `type: array, items: { $ref: ... }` (well-formed; no malformed `type: array`+sibling `$ref`).
- GET `/entity/{entityId}` returns envelope, spec says raw entity — declare `Envelope` as a named schema and reference it.

Other entity ops likely have the same `type: array`+`$ref` malformation per the audit. All 5-6 sites get fixed in this commit.

The `EntityEnvelope` Go type collapse to `genapi.Envelope` is per design Section 4 — apply the rule: collapse where the domain type adds zero value (this case qualifies); keep+`ToWire()` otherwise.

- [ ] **Step 1-9:** Mirror Task 3.1 for the entity domain.

- [ ] **Step 10: Commit**

```bash
git commit -m "fix(entity): reconcile spec with server (closes original #21 confirmed defects)"
```

### Task 7.1: messaging domain — spec changes (includes JSON-in-string fix)

5 operations. The JSON-in-string fix at `internal/domain/messaging/handler.go:183` and the dead-branch removal at `:59-66` both land in this commit.

- [ ] **Step 1: Write the failing test that pins the corrected wire shape**

In `internal/e2e/message_test.go`:

```go
func TestMessage_GetMessage_ContentIsEmbeddedJSON(t *testing.T) {
    // Create a message with a JSON object payload.
    body := `{"payload": {"sample": "value", "n": 42}, "meta-data": {}}`
    createReq := authRequest(t, "POST", "/api/message/test-subject?contentType=application/json&contentLength=99",
        strings.NewReader(body))
    createResp, err := http.DefaultClient.Do(createReq)
    if err != nil {
        t.Fatalf("create: %v", err)
    }
    defer createResp.Body.Close()
    if createResp.StatusCode != http.StatusOK {
        b, _ := io.ReadAll(createResp.Body)
        t.Fatalf("create status=%d body=%s", createResp.StatusCode, b)
    }
    var createOut []map[string]any
    if err := json.NewDecoder(createResp.Body).Decode(&createOut); err != nil {
        t.Fatalf("decode create: %v", err)
    }
    msgID := createOut[0]["entityIds"].([]any)[0].(string)

    // Retrieve the message; assert content is an embedded object, not a string.
    getReq := authRequest(t, "GET", "/api/message/"+msgID, nil)
    getResp, err := http.DefaultClient.Do(getReq)
    if err != nil {
        t.Fatalf("get: %v", err)
    }
    defer getResp.Body.Close()
    raw, _ := io.ReadAll(getResp.Body)
    var msg map[string]any
    if err := json.Unmarshal(raw, &msg); err != nil {
        t.Fatalf("decode message: %v\nbody: %s", err, raw)
    }
    content, ok := msg["content"].(map[string]any)
    if !ok {
        t.Fatalf("content is not an object; type=%T value=%v", msg["content"], msg["content"])
    }
    if content["sample"] != "value" {
        t.Errorf("content.sample = %v, want \"value\"", content["sample"])
    }
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./internal/e2e/... -run TestMessage_GetMessage_ContentIsEmbeddedJSON -v -count=1
```

Expected: FAIL — `content` is currently a string.

- [ ] **Step 3: Fix the handler — `internal/domain/messaging/handler.go:183`**

Change:

```go
"content": string(payloadBytes),
```

to:

```go
"content": json.RawMessage(payloadBytes),
```

Add `"encoding/json"` import if not present.

- [ ] **Step 4: Remove the dead-code branch at `internal/domain/messaging/handler.go:59-66`**

Replace:

```go
var compacted bytes.Buffer
if err := json.Compact(&compacted, envelope.Payload); err != nil {
    // Not valid JSON — store as-is (payload is opaque).
    compacted.Reset()
    compacted.Write(envelope.Payload)
}
```

with:

```go
var compacted bytes.Buffer
if err := json.Compact(&compacted, envelope.Payload); err != nil {
    // Unreachable in practice: json.Unmarshal above validated envelope.Payload
    // as a JSON value. This guard documents the invariant and surfaces the
    // bug as a 500 if a future code path violates it (e.g. constructing
    // envelope.Payload by hand instead of via json.Unmarshal).
    common.WriteError(w, r, common.Internal("payload validation invariant broken", err))
    return
}
```

- [ ] **Step 5: Run the new test and existing messaging tests**

```bash
go test ./internal/e2e/... -run "TestMessage" -v -count=1
go test ./internal/domain/messaging/... -v -count=1
```

Expected: all PASS.

- [ ] **Step 6: Update spec — declare `EdgeMessagePayload` as polymorphic**

In `api/openapi.yaml`, find the `getMessage` response schema. Add the `EdgeMessagePayload` named schema and reference it for the `content` field:

```yaml
components:
  schemas:
    EdgeMessagePayload:
      description: |
        Polymorphic by intent — accepts any JSON value. The contentType field
        in the message header is informational; it does not affect storage or
        retrieval format. Clients needing to store non-JSON content should
        stringify it (e.g. base64 for binary, plain string with escape for
        non-JSON text). See Cyoda issue #193 for proper content-type support
        as a future feature.
      additionalProperties: true
```

Reference from the `getMessage` 200 response:

```yaml
"200":
  content:
    application/json:
      schema:
        type: object
        properties:
          content:
            $ref: '#/components/schemas/EdgeMessagePayload'
          contentType:
            type: string
            description: Informational only; see EdgeMessagePayload note. See #193 for proper handling.
          # ... other fields
```

- [ ] **Step 7: Repeat Task 3.1 Steps 5-9 for the messaging domain**

- [ ] **Step 8: Commit**

```bash
git commit -m "fix(messaging): JSON-in-string content + dead-branch removal + EdgeMessagePayload schema"
```

### Task 8.1: model domain — spec changes

12 operations. Includes export/import paths that may use XML — handle as the audit table dictates.

- [ ] **Step 1-9:** Mirror Task 3.1.

- [ ] **Step 10: Commit**

```bash
git commit -m "fix(model): reconcile spec with server, declare error responses"
```

### Task 9.1: search domain — spec changes (includes ndjson streaming)

6 operations. The streaming `application/x-ndjson` variant of `getAllEntities` and `searchEntities`'s direct synchronous mode are validated by status + headers only (per design Section 2 streaming subsection).

- [ ] **Step 1-9:** Mirror Task 3.1. Confirm via the validator report that streaming endpoints are status-only validated (the report Reason field will not include body-level diffs for those operations).

- [ ] **Step 10: Commit**

```bash
git commit -m "fix(search): reconcile spec with server, document ndjson streaming validator coverage"
```

### Task 10.1: workflow domain — spec changes

8 operations.

- [ ] **Step 1-9:** Mirror Task 3.1.

- [ ] **Step 10: Commit**

```bash
git commit -m "fix(workflow): reconcile spec with server"
```

---

## Commit 11 — Mode flip + final consistency check + Mode test

### Task 11.1: Add the mode-pinning test

**Files:**
- Create: `internal/e2e/openapivalidator/mode_test.go`

- [ ] **Step 1: Write the test**

```go
package openapivalidator

import "testing"

// TestModeIsEnforce pins the validator's Mode constant. Any future PR that
// flips back to ModeRecord must also remove or change this test — visible
// in code review, not silent.
func TestModeIsEnforce(t *testing.T) {
    if Mode != ModeEnforce {
        t.Fatalf("Mode = %v; expected ModeEnforce on main. Re-flipping to ModeRecord requires explicit PR review.", Mode)
    }
}
```

- [ ] **Step 2: Run to verify it fails (because Mode is still ModeRecord)**

```bash
go test ./internal/e2e/openapivalidator/ -run TestModeIsEnforce -v
```

Expected: FAIL.

### Task 11.2: Flip the Mode constant

**Files:**
- Modify: `internal/e2e/openapivalidator/mode.go`

- [ ] **Step 1: Change `mode.go`**

Change:

```go
const Mode = ModeRecord
```

to:

```go
const Mode = ModeEnforce
```

- [ ] **Step 2: Run the mode test to verify it now passes**

```bash
go test ./internal/e2e/openapivalidator/ -run TestModeIsEnforce -v
```

Expected: PASS.

- [ ] **Step 3: Run the full E2E suite**

```bash
go test ./internal/e2e/... -v -count=1
```

Expected: all tests PASS, `TestOpenAPIConformanceReport` PASSES (zero mismatches and zero uncovered operations because per-domain commits 3-10 fixed everything). **If any mismatch remains, the per-domain commits are incomplete — return to the relevant domain commit.**

### Task 11.3: Final derived-artefact pass

**Files:**
- Modify: `cmd/cyoda/help/content/openapi.md`

- [ ] **Step 1: Read the help narrative**

```bash
cat cmd/cyoda/help/content/openapi.md
```

- [ ] **Step 2: Update any references to corrected fields**

Where the narrative mentions wire shapes that changed (e.g. envelope, JSON-in-string content), update the prose. The `cyoda help openapi {json,yaml,tags}` action outputs auto-emit from the embedded spec via `genapi.GetSwagger()` — no code change needed.

- [ ] **Step 3: Final consistency check — verify ADR 0001 is unchanged**

```bash
git log --oneline docs/adr/0001-openapi-server-spec-conformance.md
```

Expected: only the original commit; no decision drift during implementation.

- [ ] **Step 4: Verify every audit table row has a non-empty disposition + resolved-by-commit**

```bash
grep -E "^\|.*\|.*\|.*\|.*\|.*\|.*\| _TBD_ \|" docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md
```

Expected: no output (no rows still TBD).

- [ ] **Step 5: Run the full test suite end-to-end**

```bash
make test-all
```

Expected: all green across root + memory + sqlite + postgres plugins.

- [ ] **Step 6: Run race detector once (pre-PR sanity check)**

```bash
go test -race ./...
```

Expected: green.

- [ ] **Step 7: Inspect the final conformance report**

```bash
cat internal/e2e/_openapi-conformance-report.md
```

Expected: zero mismatches, zero uncovered operations.

- [ ] **Step 8: Commit**

```bash
git add internal/e2e/openapivalidator/mode.go internal/e2e/openapivalidator/mode_test.go \
        cmd/cyoda/help/content/openapi.md docs/superpowers/audits/2026-04-29-openapi-conformance-audit.md
git commit -m "feat(e2e): flip openapi validator to ModeEnforce + final consistency pass

Closes #21."
```

---

## Acceptance gate (PR description checklist)

Before opening the PR, confirm in the PR description:

- [ ] Validator's `Mode` constant is `ModeEnforce` (not `ModeRecord`).
- [ ] `TestOpenAPIConformanceReport` reports zero mismatches and zero uncovered ops.
- [ ] `TestModeIsEnforce` exists and passes.
- [ ] All audit table rows have non-empty disposition + resolved-by-commit.
- [ ] `make test-all` green; `go test -race ./...` green.
- [ ] Original #21 confirmed defects fixed (POST array shape, GET envelope, `messaging.GetMessage.content` JSON-in-string, `basicAuth` declaration).
- [ ] All 6 malformed `type: array`+`$ref` sites in the spec are well-formed.
- [ ] `e2e/parity` tests pass against memory + sqlite + postgres backends.
- [ ] ADR 0001 unchanged.

---

## Self-review — done before publishing this plan

- **Spec coverage:** Section 1 (scope) → covered by Tasks 1.1-1.9 (validator) + 2.1 (audit) + 3.1-10.1 (per-domain) + 11.1-11.3 (final). Section 2 (validator architecture) → 1.1-1.9. Section 3 (audit table) → 2.1. Section 4 (spec changes) → 3.1-10.1. Section 5 (handler defects) → 7.1 (messaging) + per-domain commits as audit surfaces them. Section 6 (E2E coverage) → per-domain Step 4. Section 7 (derived artefacts) → per-domain Step 6 + 11.3. Section 8 (commit topology) → matches plan layout exactly. Section 9 (risk register) → mitigations folded into per-task instructions.
- **Placeholder scan:** the only `_TBD_` is in the audit table template (Task 2.1 Step 5) — that's the literal value the implementing agent will fill in for each row. Not a plan placeholder.
- **Type consistency:** `Validator` struct, `NewValidator`, `Validate`, `Mismatch`, `Mode`, `ModeKind`, `ModeRecord`, `ModeEnforce`, `WithTestT`, `TestTFromContext`, `WriteReport`, `DrainAndExercised`, `NewMiddleware` — all defined in earlier tasks before being referenced in later ones.
