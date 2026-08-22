# Auth-Cache Reconciliation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give both per-node auth caches (trusted keys, OIDC providers) a broadcast fast path, a periodic jittered KV-reconcile backstop, and a fail-closed staleness bound, so revocations and registrations converge cluster-wide.

**Architecture:** Per spec `docs/superpowers/specs/2026-08-21-483-auth-cache-reconcile-design.md` (READ IT FIRST — §11 lists settled decisions; do not re-litigate). Trusted keys gain a `Reconcile` (off-lock KV List + generation-guarded swap, serialized by a dedicated mutex), a payload-free `auth.trustedkeys` gossip ping with a coalescing handler, a jittered reconcile loop, and a staleness breaker. The OIDC `Registry.ReloadAll` gains the same serialization + generation guard + skip-swap-on-no-change, a reconcile loop, and a breaker in `ResolveKey`.

**Tech Stack:** Go 1.26, `log/slog`, `sync/atomic`, `math/rand/v2`, OTel metrics via existing patterns, `plugins/memory` KV for tests.

## Global Constraints

- TDD mandatory: every step RED before GREEN. Run scoped tests only during iteration (`go test ./internal/auth/... -run <Name> -v`); full suite is end-of-deliverable, `make race` once before PR.
- Mutex discipline: every `Lock()`/`RLock()` immediately followed by `defer Unlock()` on the next line; IIFE for early release (`.claude/rules/go-mutex-discipline.md`).
- `log/slog` only. Never log credentials, key material, or broadcast payload bytes.
- No issue numbers (`#483`) in code, comments, log messages, or docs content — commit messages and this plan only.
- Error wrapping: `fmt.Errorf("failed to X: %w", err)`.
- No new error codes, endpoints, or OpenAPI changes. The only new wire surface is the payload-free `auth.trustedkeys` gossip topic.
- Constants (spec-settled): interval default `60s`, config floor `1s`, staleness bound `10 ×` interval, reconcile retry budget `5`, jitter `interval × [0.9, 1.1)`.
- Commit after every task with a conventional-commit message; each commit ends with the Co-Authored-By + Claude-Session trailer already configured for this session.

**Execution streams** (per `feedback_parallelise_independent_sdd_streams`): Stream A = Tasks 1–4 (`internal/auth`), Stream B = Tasks 5–7 (`internal/auth/oidc`), Stream C1 = Task 8 (config). A, B, C1 are mutually independent and may run in parallel worktrees or sequentially. Task 9 (wiring) depends on A+B+C1. Task 10 (docs) depends on 9.

---

### Task 1: Trusted-key `Reconcile` — generation counter, serialized off-lock rebuild, error taxonomy

**Files:**
- Modify: `internal/auth/kv_trusted_store.go`
- Test: `internal/auth/kv_trusted_store_reconcile_test.go` (new)

**Interfaces:**
- Consumes: existing `KVTrustedKeyStore` internals (`s.mu`, `s.keys`, `s.kv`, `loadAll`, `deserializeTrustedKey`).
- Produces: `func (s *KVTrustedKeyStore) Reconcile(ctx context.Context) error` (exported; Tasks 2–4 call it), unexported `gen atomic.Uint64` bumped by every cache-commit, `reconcileMu sync.Mutex`, and package-level `errReconcileContention` sentinel (Task 4 needs it to exempt churn from breaker accounting).

- [ ] **Step 1: Write the failing tests**

Create `internal/auth/kv_trusted_store_reconcile_test.go`. Test helpers at top (external test package, reuse `systemCtx()` from `kv_trusted_store_test.go` — same package `auth_test`, so it is visible):

```go
package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// hookKV wraps a real KeyValueStore with fault-injection and interception
// hooks for reconcile tests.
type hookKV struct {
	spi.KeyValueStore
	mu      sync.Mutex
	listErr error
	onList  func() // runs before each delegated List
	overlay map[string][]byte // extra entries merged into List results
}

func (h *hookKV) List(ctx context.Context, ns string) (map[string][]byte, error) {
	h.mu.Lock()
	le, ol := h.listErr, h.onList
	h.mu.Unlock()
	if le != nil {
		return nil, le
	}
	if ol != nil {
		ol()
	}
	m, err := h.KeyValueStore.List(ctx, ns)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, v := range h.overlay {
		m[k] = v
	}
	return m, nil
}

func (h *hookKV) setListErr(err error)  { h.mu.Lock(); defer h.mu.Unlock(); h.listErr = err }
func (h *hookKV) setOnList(f func())    { h.mu.Lock(); defer h.mu.Unlock(); h.onList = f }
func (h *hookKV) setOverlay(k string, v []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.overlay == nil {
		h.overlay = map[string][]byte{}
	}
	h.overlay[k] = v
}

func newTrustedKey(t *testing.T, kid string, tenant spi.TenantID) *auth.TrustedKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	exp := time.Now().Add(24 * time.Hour)
	return &auth.TrustedKey{
		KID: kid, TenantID: tenant, PublicKey: &key.PublicKey,
		Audience: "api://test", Active: true,
		ValidFrom: time.Now().Add(-time.Hour), ValidTo: &exp,
	}
}

// twoStores returns two KVTrustedKeyStore instances over one shared KV —
// the two-nodes-one-database seam.
func twoStores(t *testing.T) (*auth.KVTrustedKeyStore, *auth.KVTrustedKeyStore, *hookKV, context.Context) {
	t.Helper()
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	hkv := &hookKV{KeyValueStore: kv}
	s1, err := auth.NewKVTrustedKeyStore(ctx, hkv)
	if err != nil {
		t.Fatalf("store 1: %v", err)
	}
	s2, err := auth.NewKVTrustedKeyStore(ctx, hkv)
	if err != nil {
		t.Fatalf("store 2: %v", err)
	}
	return s1, s2, hkv, ctx
}

// T1: cross-node convergence via Reconcile for every mutation kind.
func TestKVTrustedKeyStore_Reconcile_ConvergesAcrossNodes(t *testing.T) {
	s1, s2, _, ctx := twoStores(t)
	tk := newTrustedKey(t, "conv-key", spi.SystemTenantID)

	// Register on node 1 → invisible to node 2's enumeration → reconcile fixes.
	if err := s1.Register(tk, auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := s2.ListForVerification(); len(got) != 0 {
		t.Fatalf("pre-reconcile: node 2 already sees %d keys", len(got))
	}
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := s2.ListForVerification(); len(got) != 1 || got[0].KID != "conv-key" {
		t.Fatalf("post-reconcile: want [conv-key], got %v", got)
	}
	if got := s2.List(spi.SystemTenantID); len(got) != 1 {
		t.Fatalf("List after reconcile: want 1, got %d", len(got))
	}

	// Invalidate on node 1 → node 2 reconcile picks up Active=false.
	if err := s1.Invalidate(spi.SystemTenantID, "conv-key", 0); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// gracePeriod 0 ⇒ ValidTo=now ⇒ excluded from verification listing.
	if got := s2.ListForVerification(); len(got) != 0 {
		t.Fatalf("invalidated key still verifiable on node 2: %v", got)
	}

	// Delete on node 1 → gone from node 2 after reconcile (Get included).
	if err := s1.Delete(spi.SystemTenantID, "conv-key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := s2.Get(spi.SystemTenantID, "conv-key"); err == nil {
		t.Fatal("deleted key still Get-able on node 2 after reconcile")
	}
}

// T2a: a corrupt record is skipped; the rest reconcile.
func TestKVTrustedKeyStore_Reconcile_SkipsCorruptRecord(t *testing.T) {
	s1, s2, hkv, ctx := twoStores(t)
	if err := s1.Register(newTrustedKey(t, "good-key", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	hkv.setOverlay(string(spi.SystemTenantID)+":corrupt", []byte("{not json"))
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile with corrupt record must not fail: %v", err)
	}
	if got := s2.ListForVerification(); len(got) != 1 || got[0].KID != "good-key" {
		t.Fatalf("good key lost during lenient reconcile: %v", got)
	}
}

// T2b: transport failure aborts the tick and keeps last-known state.
func TestKVTrustedKeyStore_Reconcile_TransportFailureKeepsState(t *testing.T) {
	s1, _, hkv, ctx := twoStores(t)
	if err := s1.Register(newTrustedKey(t, "keep-key", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s1.Reconcile(ctx); err != nil {
		t.Fatalf("baseline Reconcile: %v", err)
	}
	hkv.setListErr(errors.New("kv down"))
	if err := s1.Reconcile(ctx); err == nil {
		t.Fatal("Reconcile must return the transport error")
	}
	if got := s1.ListForVerification(); len(got) != 1 {
		t.Fatalf("state lost on transport failure: %v", got)
	}
}

// T3: a mutation landing between List and swap is not clobbered.
func TestKVTrustedKeyStore_Reconcile_GenerationGuard(t *testing.T) {
	s1, s2, hkv, ctx := twoStores(t)
	if err := s1.Register(newTrustedKey(t, "doomed", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatalf("warm-up Reconcile: %v", err)
	}
	// First List of the next reconcile sees "doomed"; the hook then deletes it
	// THROUGH s2 (bumping s2's generation) so the stale snapshot must be
	// discarded and rebuilt.
	fired := false
	hkv.setOnList(func() {
		if fired {
			return
		}
		fired = true
		if err := s2.Delete(spi.SystemTenantID, "doomed"); err != nil {
			t.Errorf("hook Delete: %v", err)
		}
	})
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := s2.Get(spi.SystemTenantID, "doomed"); err == nil {
		t.Fatal("generation guard failed: deleted key resurrected by stale snapshot")
	}
}

// T4: overlapping Reconcile calls are serialized; final state matches KV.
func TestKVTrustedKeyStore_Reconcile_ConcurrentReconcilesSerialized(t *testing.T) {
	s1, s2, _, ctx := twoStores(t)
	if err := s1.Register(newTrustedKey(t, "racer", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s2.Reconcile(ctx)
		}()
	}
	wg.Wait()
	if got := s2.ListForVerification(); len(got) != 1 {
		t.Fatalf("concurrent reconciles corrupted state: %v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/auth/ -run 'TestKVTrustedKeyStore_Reconcile' -v`
Expected: compile FAIL — `s2.Reconcile undefined`.

- [ ] **Step 3: Implement**

In `internal/auth/kv_trusted_store.go`:

3a. Add imports `"sync/atomic"` (keep existing). Add fields to `KVTrustedKeyStore`:

```go
	// gen counts cache-mutating commits (Register/Delete/Invalidate/
	// Reactivate/loadOne and every reconcile swap). Reconcile snapshots it
	// before its KV List and discards the built snapshot if it changed by
	// swap time — a mutation that lands mid-rebuild always wins.
	gen atomic.Uint64
	// reconcileMu serializes reconcile executions (periodic tick, gossip
	// ping, explicit calls) so two overlapping rebuilds can never commit
	// KV snapshots out of order.
	reconcileMu sync.Mutex
```

3b. Bump the generation at every cache-commit point (all already under `s.mu`):
- `loadOne`: after `s.keys[kid] = tk` add `s.gen.Add(1)`.
- `Register`: after `s.keys[copied.KID] = &copied` add `s.gen.Add(1)`. (One bump per call is sufficient — the whole mutation holds `s.mu`, so it is atomic with respect to the swap check; sibling-flip commits need no extra bumps.)
- `Delete`: after `delete(s.keys, kid)` add `s.gen.Add(1)`.
- `Invalidate`: after `s.keys[kid] = &updated` add `s.gen.Add(1)`.
- `Reactivate`: after `s.keys[kid] = &updated` add `s.gen.Add(1)`.

3c. Extract a lenient map-builder next to `loadAll` and add `Reconcile`:

```go
// maxReconcileAttempts bounds the generation-guard retry inside Reconcile.
// Only continuous mutation churn (which itself proves KV is reachable)
// can exhaust it.
const maxReconcileAttempts = 5

// errReconcileContention marks a reconcile that gave up because mutations
// kept landing mid-rebuild. It is not a KV failure: callers must not count
// it toward staleness accounting.
var errReconcileContention = errors.New("trusted-key reconcile: retry budget exhausted under concurrent mutation")

// buildTrustedKeyMap deserializes a KV List result into a fresh cache map.
// Legacy un-tenanted entries are skipped (pre-tenant-scoping layout). When
// strict, the first undeserializable record fails the build (construction
// fail-fast); otherwise bad records are skipped with an ERROR log so one
// corrupt write can never disable reconciliation.
func buildTrustedKeyMap(entries map[string][]byte, strict bool) (map[string]*TrustedKey, int, error) {
	keys := make(map[string]*TrustedKey, len(entries))
	skipped := 0
	for kvKey, data := range entries {
		if !strings.Contains(kvKey, ":") {
			skipped++
			continue
		}
		tk, err := deserializeTrustedKey(data)
		if err != nil {
			if strict {
				return nil, 0, fmt.Errorf("failed to deserialize trusted key %q: %w", kvKey, err)
			}
			slog.Error("trusted-key reconcile: skipping undeserializable record",
				"pkg", "auth", "kvKey", kvKey, "error", err.Error())
			continue
		}
		if tk.TenantID == "" {
			skipped++
			continue
		}
		keys[tk.KID] = tk
	}
	return keys, skipped, nil
}

// Reconcile rebuilds the in-memory cache from the authoritative KV store.
// The List and deserialization run off-lock; the swap takes s.mu and is
// discarded (and retried) if any mutation committed since the pre-List
// generation snapshot. Serialized against concurrent reconciles by
// reconcileMu. On a transport-level List failure the previous cache state
// is kept and the error returned — bounded staleness beats dropping every
// key over an infrastructure blip; the staleness bound is enforced
// separately by the callers' breaker accounting.
func (s *KVTrustedKeyStore) Reconcile(ctx context.Context) error {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	for attempt := 0; attempt < maxReconcileAttempts; attempt++ {
		gen := s.gen.Load()
		entries, err := s.kv.List(ctx, trustedKeysNamespace)
		if err != nil {
			return fmt.Errorf("failed to list trusted keys for reconcile: %w", err)
		}
		fresh, _, err := buildTrustedKeyMap(entries, false)
		if err != nil {
			return err // unreachable in lenient mode; kept for signature honesty
		}
		swapped := func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.gen.Load() != gen {
				return false
			}
			s.keys = fresh
			s.gen.Add(1)
			return true
		}()
		if swapped {
			return nil
		}
	}
	slog.Warn("trusted-key reconcile: giving up after repeated mid-rebuild mutations",
		"pkg", "auth", "attempts", maxReconcileAttempts)
	return errReconcileContention
}
```

3d. Rewrite `loadAll` to delegate: list, `buildTrustedKeyMap(entries, true)`, assign `s.keys`, keep the existing skipped-legacy WARN using the returned count.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/auth/ -run 'TestKVTrustedKeyStore' -v` (all existing + new)
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/kv_trusted_store.go internal/auth/kv_trusted_store_reconcile_test.go
git commit -m "feat(auth): trusted-key cache Reconcile with generation-guarded KV rebuild"
```

---

### Task 2: Trusted-key gossip ping — coalescing runner + `auth.trustedkeys` topic

**Files:**
- Create: `internal/auth/coalesce.go`
- Modify: `internal/auth/kv_trusted_store.go`
- Test: `internal/auth/coalesce_test.go` (new), `internal/auth/kv_trusted_store_reconcile_test.go` (extend)

**Interfaces:**
- Consumes: `Reconcile(ctx)` from Task 1; `spi.ClusterBroadcaster` (`Broadcast(topic string, payload []byte)` / `Subscribe(topic string, handler func([]byte))`).
- Produces: `WithTrustedKeyBroadcaster(b spi.ClusterBroadcaster) KVTrustedKeyStoreOption` (Task 9 wires it); `coalescingRunner` with `Trigger(run func())`; unexported `topicTrustedKeys = "auth.trustedkeys"`; unexported `s.reconcileOnce()` (Task 3's loop reuses it). `reconcileOnce` uses `s.reconcileInterval` for its timeout — Task 3 introduces the option that sets it; until then it defaults to `defaultReconcileInterval` (declare `const defaultReconcileInterval = 60 * time.Second` in this task; Task 3 references it from config-default docs).

- [ ] **Step 1: Write the failing tests**

`internal/auth/coalesce_test.go`:

```go
package auth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A trigger during an in-flight run coalesces to exactly one trailing run.
func TestCoalescingRunner_TrailingRun(t *testing.T) {
	var runs atomic.Int32
	inFirst := make(chan struct{})
	release := make(chan struct{})
	var c coalescingRunner

	c.Trigger(func() {
		runs.Add(1)
		close(inFirst)
		<-release
	})
	<-inFirst
	c.Trigger(func() { runs.Add(1) }) // arrives mid-flight → dirty flag
	c.Trigger(func() { runs.Add(1) }) // second mid-flight → still one trailing
	close(release)

	deadline := time.After(2 * time.Second)
	for runs.Load() != 2 {
		select {
		case <-deadline:
			t.Fatalf("want exactly 2 runs (1 + 1 trailing), got %d", runs.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := runs.Load(); got != 2 {
		t.Fatalf("extra trailing runs: got %d", got)
	}
}

// Concurrent triggers never lose the final state and never deadlock.
func TestCoalescingRunner_ConcurrentTriggers(t *testing.T) {
	var runs atomic.Int32
	var c coalescingRunner
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Trigger(func() { runs.Add(1) })
		}()
	}
	wg.Wait()
	deadline := time.After(2 * time.Second)
	for {
		if !c.busy() {
			break
		}
		select {
		case <-deadline:
			t.Fatal("runner still busy after all triggers")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if runs.Load() < 1 {
		t.Fatal("no run executed")
	}
}
```

Extend `kv_trusted_store_reconcile_test.go` (add `fakeBroadcaster`, modeled on `internal/cluster/modelcache/integration_test.go`):

```go
// fakeBroadcaster delivers published messages synchronously to every
// subscriber, including the publisher's own node (mirrors gossip loopback
// being absent — so tests subscribe a SECOND store to observe propagation).
type fakeBroadcaster struct {
	mu       sync.Mutex
	handlers map[string][]func([]byte)
}

func newFakeBroadcaster() *fakeBroadcaster {
	return &fakeBroadcaster{handlers: map[string][]func([]byte){}}
}

func (b *fakeBroadcaster) Broadcast(topic string, payload []byte) {
	b.mu.Lock()
	hs := append([]func([]byte){}, b.handlers[topic]...)
	b.mu.Unlock()
	for _, h := range hs {
		h(payload)
	}
}

func (b *fakeBroadcaster) Subscribe(topic string, h func([]byte)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[topic] = append(b.handlers[topic], h)
}

func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// T5: a mutation on node 1 pings node 2, which reconciles.
func TestKVTrustedKeyStore_PingTriggersReconcile(t *testing.T) {
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	b := newFakeBroadcaster()
	s1, err := auth.NewKVTrustedKeyStore(ctx, kv, auth.WithTrustedKeyBroadcaster(b))
	if err != nil {
		t.Fatalf("store 1: %v", err)
	}
	s2, err := auth.NewKVTrustedKeyStore(ctx, kv, auth.WithTrustedKeyBroadcaster(b))
	if err != nil {
		t.Fatalf("store 2: %v", err)
	}

	if err := s1.Register(newTrustedKey(t, "ping-key", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	eventually(t, 3*time.Second, func() bool {
		return len(s2.ListForVerification()) == 1
	}, "node 2 never saw the registered key after ping")

	if err := s1.Delete(spi.SystemTenantID, "ping-key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	eventually(t, 3*time.Second, func() bool {
		return len(s2.ListForVerification()) == 0
	}, "node 2 never dropped the deleted key after ping")
}

// T5: a garbage payload is ignored (never parsed, never logged) and still
// triggers a harmless reconcile; the handler never panics.
func TestKVTrustedKeyStore_PingIgnoresPayload(t *testing.T) {
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	b := newFakeBroadcaster()
	s, err := auth.NewKVTrustedKeyStore(ctx, kv, auth.WithTrustedKeyBroadcaster(b))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	b.Broadcast("auth.trustedkeys", []byte("\x00garbage\xff of arbitrary peer bytes"))
	// Nothing to assert beyond absence of panic and continued liveness:
	if got := s.ListForVerification(); len(got) != 0 {
		t.Fatalf("unexpected keys: %v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/auth/ -run 'TestCoalescingRunner|TestKVTrustedKeyStore_Ping' -v`
Expected: compile FAIL — `coalescingRunner` / `WithTrustedKeyBroadcaster` undefined.

- [ ] **Step 3: Implement**

3a. Create `internal/auth/coalesce.go`:

```go
package auth

import "sync"

// coalescingRunner runs at most one execution of a job at a time and
// coalesces triggers that arrive mid-run into exactly one trailing rerun.
// Unlike a drop-style singleflight, a trigger is never lost: state observed
// after the triggering event is always re-read by the trailing run. Used by
// the trusted-key gossip ping handler, where a dropped trigger would
// silently downgrade revocation propagation to backstop latency.
type coalescingRunner struct {
	mu      sync.Mutex
	running bool
	dirty   bool
}

// Trigger schedules run. If an execution is in flight, it marks the runner
// dirty and returns — the in-flight goroutine will run once more when it
// finishes. run executes on a fresh goroutine; Trigger never blocks.
func (c *coalescingRunner) Trigger(run func()) {
	start := func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.running {
			c.dirty = true
			return false
		}
		c.running = true
		return true
	}()
	if !start {
		return
	}
	go func() {
		for {
			run()
			again := func() bool {
				c.mu.Lock()
				defer c.mu.Unlock()
				if c.dirty {
					c.dirty = false
					return true
				}
				c.running = false
				return false
			}()
			if !again {
				return
			}
		}
	}()
}

// busy is a test-only inspector.
func (c *coalescingRunner) busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}
```

3b. In `kv_trusted_store.go` add:

```go
// topicTrustedKeys is the gossip topic for trusted-key change pings. The
// payload is always empty and receivers must never read or log it — it is
// arbitrary peer-controlled bytes; the ping's only information is that it
// arrived.
const topicTrustedKeys = "auth.trustedkeys"

// defaultReconcileInterval mirrors the config default; the store falls back
// to it when no interval option is supplied (tests, callers predating the
// reconcile loop).
const defaultReconcileInterval = 60 * time.Second
```

Fields on the struct: `broadcaster spi.ClusterBroadcaster`, `reconcileInterval time.Duration`, `pingCoalescer coalescingRunner`. Config struct gains `broadcaster spi.ClusterBroadcaster`; option:

```go
// WithTrustedKeyBroadcaster wires the cluster gossip fast path: mutations
// publish a payload-free ping on topicTrustedKeys, and received pings
// trigger a coalesced Reconcile. Nil (single-node) disables the fast path;
// the periodic reconcile loop is unaffected.
func WithTrustedKeyBroadcaster(b spi.ClusterBroadcaster) KVTrustedKeyStoreOption {
	return func(c *kvTrustedKeyStoreConfig) {
		c.broadcaster = b
	}
}
```

Constructor: set `reconcileInterval: defaultReconcileInterval` (overridden in Task 3), copy broadcaster, and after the successful `loadAll`:

```go
	if s.broadcaster != nil {
		s.broadcaster.Subscribe(topicTrustedKeys, s.handlePing)
	}
```

Handler + helpers:

```go
// handlePing runs on the broadcaster's shared receive goroutine: it must
// not block and must not let a panic escape. The payload is deliberately
// ignored and never logged (arbitrary peer bytes).
func (s *KVTrustedKeyStore) handlePing(_ []byte) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("trusted-key ping handler panic", "pkg", "auth", "panic", rec)
		}
	}()
	s.pingCoalescer.Trigger(s.reconcileOnce)
}

// reconcileOnce runs a single bounded reconcile attempt. Each attempt gets
// its own deadline (one interval) so a hung KV call aborts the attempt
// instead of wedging the loop/coalescer permanently — the store's own ctx
// is deliberately deadline-free and must not be used raw for periodic I/O.
func (s *KVTrustedKeyStore) reconcileOnce() {
	ctx, cancel := context.WithTimeout(s.ctx, s.reconcileInterval)
	defer cancel()
	_ = s.Reconcile(ctx) // failures logged/accounted inside Reconcile paths
}

// broadcastChanged publishes the payload-free change ping. No-op without a
// broadcaster.
func (s *KVTrustedKeyStore) broadcastChanged() {
	if s.broadcaster == nil {
		return
	}
	s.broadcaster.Broadcast(topicTrustedKeys, nil)
}
```

Call `s.broadcastChanged()` at the end of `Register` (after the new key commit — broadcast even when the sibling-flip partially failed: the new key IS committed), `Delete`, `Invalidate`, and `Reactivate` (success paths). `Broadcast` is non-blocking per the SPI contract, so calling while `s.mu` is held (defer-unlock style) is fine.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/auth/ -run 'TestCoalescingRunner|TestKVTrustedKeyStore' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/coalesce.go internal/auth/coalesce_test.go internal/auth/kv_trusted_store.go internal/auth/kv_trusted_store_reconcile_test.go
git commit -m "feat(auth): trusted-key change ping on auth.trustedkeys with coalescing reconcile handler"
```

---

### Task 3: Trusted-key reconcile loop — jitter, once-guard, ctx cancel

**Files:**
- Modify: `internal/auth/kv_trusted_store.go`
- Test: `internal/auth/kv_trusted_store_reconcile_test.go` (extend)

**Interfaces:**
- Consumes: `reconcileOnce` (Task 2).
- Produces: `WithReconcileInterval(d time.Duration) KVTrustedKeyStoreOption`, `func (s *KVTrustedKeyStore) StartReconcileLoop(ctx context.Context) bool` (Task 9 wires both; Task 4's breaker arms off the loop-started flag it sets).

- [ ] **Step 1: Write the failing tests**

```go
// T7: the loop converges a peer mutation without any ping, respects the
// once-guard, and stops on ctx cancel.
func TestKVTrustedKeyStore_ReconcileLoop(t *testing.T) {
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	s1, err := auth.NewKVTrustedKeyStore(ctx, kv)
	if err != nil {
		t.Fatalf("store 1: %v", err)
	}
	s2, err := auth.NewKVTrustedKeyStore(ctx, kv, auth.WithReconcileInterval(20*time.Millisecond))
	if err != nil {
		t.Fatalf("store 2: %v", err)
	}
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !s2.StartReconcileLoop(loopCtx) {
		t.Fatal("first StartReconcileLoop returned false")
	}
	if s2.StartReconcileLoop(loopCtx) {
		t.Fatal("second StartReconcileLoop must be a no-op returning false")
	}

	if err := s1.Register(newTrustedKey(t, "loop-key", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	eventually(t, 3*time.Second, func() bool {
		return len(s2.ListForVerification()) == 1
	}, "loop never converged the peer registration")

	// Cancel, then mutate again: node 2 must NOT converge (loop stopped).
	cancel()
	time.Sleep(100 * time.Millisecond) // let the loop goroutine observe cancel
	if err := s1.Delete(spi.SystemTenantID, "loop-key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := s2.ListForVerification(); len(got) != 1 {
		t.Fatalf("loop still reconciling after ctx cancel: %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/auth/ -run 'TestKVTrustedKeyStore_ReconcileLoop' -v`
Expected: compile FAIL — `WithReconcileInterval` / `StartReconcileLoop` undefined.

- [ ] **Step 3: Implement**

Option (config struct gains `reconcileInterval time.Duration`, default `defaultReconcileInterval` in the constructor's cfg init):

```go
// WithReconcileInterval overrides the periodic KV-reconcile interval
// (default 60s, matching CYODA_AUTH_CACHE_RECONCILE_INTERVAL). Values <= 0
// fall back to the default. The actual per-tick wait is jittered ±10% to
// avoid a cross-node reconcile herd.
func WithReconcileInterval(d time.Duration) KVTrustedKeyStoreOption {
	return func(c *kvTrustedKeyStoreConfig) {
		if d > 0 {
			c.reconcileInterval = d
		}
	}
}
```

Struct field `reconcileLoopStarted atomic.Bool` and:

```go
// StartReconcileLoop starts the periodic KV-reconcile backstop: every
// interval (jittered ±10%) the cache is rebuilt from the authoritative KV
// store, bounding the staleness a dropped gossip ping can cause. Returns
// false (starting nothing) if the loop already runs. The loop exits when
// ctx is cancelled; callers pass a process-lifetime context.
func (s *KVTrustedKeyStore) StartReconcileLoop(ctx context.Context) bool {
	if !s.reconcileLoopStarted.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		for {
			timer := time.NewTimer(jitteredInterval(s.reconcileInterval))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				s.reconcileOnce()
			}
		}
	}()
	return true
}

// jitteredInterval returns d × [0.9, 1.1) — the model-cache herd-avoidance
// convention.
func jitteredInterval(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
}
```

Import `"math/rand/v2"` (its top-level `Float64` is concurrency-safe; no seeding).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/auth/ -run 'TestKVTrustedKeyStore' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/kv_trusted_store.go internal/auth/kv_trusted_store_reconcile_test.go
git commit -m "feat(auth): jittered periodic reconcile loop for the trusted-key cache"
```

---

### Task 4: Trusted-key staleness breaker + reconcile metrics

**Files:**
- Modify: `internal/auth/kv_trusted_store.go`
- Create: `internal/auth/reconcile_metrics.go`
- Test: `internal/auth/kv_trusted_store_reconcile_test.go` (extend)

**Interfaces:**
- Consumes: Task 1's `Reconcile`/`errReconcileContention`, Task 3's loop flag.
- Produces:
  - `type ReconcileMetrics interface { SetReconcileConsecutiveFailures(n int); SetReconcileStalenessSeconds(sec float64) }`, `NopReconcileMetrics{}`, `NewOTelReconcileMetrics(meter metric.Meter) (ReconcileMetrics, error)` — Task 9 wires the OTel one.
  - `WithReconcileMetrics(m ReconcileMetrics) KVTrustedKeyStoreOption`.
  - Behaviour: `stalenessMultiplier = 10`; when the loop has started and `now − lastSuccessfulReconcile > 10 × interval`, `ListForVerification` returns empty and `Get` bypasses the cache-hit path (forced `loadOne` read-through). WARN per failed tick, ERROR from the 4th consecutive failure.

- [ ] **Step 1: Write the failing tests**

```go
// T6: breaker fails the enumeration path closed, Get keeps serving KV
// ground truth, recovery restores, and the breaker is inert without a loop.
func TestKVTrustedKeyStore_StalenessBreaker(t *testing.T) {
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	hkv := &hookKV{KeyValueStore: kv}
	s, err := auth.NewKVTrustedKeyStore(ctx, hkv, auth.WithReconcileInterval(20*time.Millisecond))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.Register(newTrustedKey(t, "breaker-key", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !s.StartReconcileLoop(loopCtx) {
		t.Fatal("StartReconcileLoop")
	}

	// Healthy loop: key verifiable.
	eventually(t, 2*time.Second, func() bool {
		return len(s.ListForVerification()) == 1
	}, "key not verifiable under healthy loop")

	// Kill List. Bound = 10×20ms = 200ms; wait comfortably past it.
	hkv.setListErr(errors.New("kv outage"))
	eventually(t, 5*time.Second, func() bool {
		return len(s.ListForVerification()) == 0
	}, "breaker never tripped: enumeration still serves stale keys")

	// Get during the trip: forced read-through still serves KV truth
	// (kv.Get is healthy — only List fails).
	if _, err := s.Get(spi.SystemTenantID, "breaker-key"); err != nil {
		t.Fatalf("Get during breaker trip must serve KV ground truth: %v", err)
	}

	// Recovery: List heals → breaker resets.
	hkv.setListErr(nil)
	eventually(t, 5*time.Second, func() bool {
		return len(s.ListForVerification()) == 1
	}, "breaker never recovered after KV healed")
}

func TestKVTrustedKeyStore_BreakerInertWithoutLoop(t *testing.T) {
	ctx := systemCtx()
	kv, err := memory.NewStoreFactory().KeyValueStore(ctx)
	if err != nil {
		t.Fatalf("KeyValueStore: %v", err)
	}
	s, err := auth.NewKVTrustedKeyStore(ctx, kv, auth.WithReconcileInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.Register(newTrustedKey(t, "no-loop-key", spi.SystemTenantID), auth.RotateOptions{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // ≫ 10× interval, but no loop running
	if got := s.ListForVerification(); len(got) != 1 {
		t.Fatalf("breaker tripped without a reconcile loop: %v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/auth/ -run 'TestKVTrustedKeyStore_StalenessBreaker|TestKVTrustedKeyStore_BreakerInert' -v`
Expected: `StalenessBreaker` FAILS (enumeration keeps serving keys — no breaker yet); `BreakerInert` passes trivially (that is fine — it pins the arming rule against regression).

- [ ] **Step 3: Implement**

3a. Create `internal/auth/reconcile_metrics.go`:

```go
package auth

import (
	"context"

	"go.opentelemetry.io/otel/metric"
)

// ReconcileMetrics receives auth-cache reconcile health signals so
// operators can alert on staleness well before the fail-closed bound.
type ReconcileMetrics interface {
	SetReconcileConsecutiveFailures(n int)
	SetReconcileStalenessSeconds(sec float64)
}

// NopReconcileMetrics is the default no-op implementation.
type NopReconcileMetrics struct{}

func (NopReconcileMetrics) SetReconcileConsecutiveFailures(int)   {}
func (NopReconcileMetrics) SetReconcileStalenessSeconds(float64) {}

var _ ReconcileMetrics = NopReconcileMetrics{}

// otelReconcileMetrics implements ReconcileMetrics over OTel gauges.
type otelReconcileMetrics struct {
	consecutiveFailures metric.Int64Gauge
	stalenessSeconds    metric.Float64Gauge
}

// NewOTelReconcileMetrics builds the OTel-backed ReconcileMetrics for the
// trusted-key cache.
func NewOTelReconcileMetrics(meter metric.Meter) (ReconcileMetrics, error) {
	m := &otelReconcileMetrics{}
	var err error
	if m.consecutiveFailures, err = meter.Int64Gauge("auth.trustedkeys.reconcile_consecutive_failures"); err != nil {
		return nil, err
	}
	if m.stalenessSeconds, err = meter.Float64Gauge("auth.trustedkeys.reconcile_staleness_seconds"); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *otelReconcileMetrics) SetReconcileConsecutiveFailures(n int) {
	m.consecutiveFailures.Record(context.Background(), int64(n))
}

func (m *otelReconcileMetrics) SetReconcileStalenessSeconds(sec float64) {
	m.stalenessSeconds.Record(context.Background(), sec)
}

var _ ReconcileMetrics = (*otelReconcileMetrics)(nil)
```

(Verify `go.opentelemetry.io/otel/metric` is already a root-module dependency — it is, via `internal/auth/oidc/metrics_otel.go`.)

3b. In `kv_trusted_store.go`:

```go
// stalenessMultiplier × reconcileInterval is the fail-closed bound: once
// the reconcile loop is running, an enumeration cache older than this stops
// serving — an unbounded-stale answer on the verification path would be
// wrong-but-available.
const stalenessMultiplier = 10

// errorEscalationThreshold is the consecutive-failure count from which
// reconcile failures log at ERROR instead of WARN.
const errorEscalationThreshold = 3
```

Struct fields: `lastReconcileNanos atomic.Int64`, `consecutiveFailures atomic.Int64`, `metrics ReconcileMetrics` (config default `NopReconcileMetrics{}`, option `WithReconcileMetrics`). Constructor: after successful `loadAll`, `s.lastReconcileNanos.Store(time.Now().UnixNano())`.

Accounting inside `Reconcile` (replace the bare returns from Task 1):
- List failure path:

```go
		if err != nil {
			n := s.consecutiveFailures.Add(1)
			s.metrics.SetReconcileConsecutiveFailures(int(n))
			s.metrics.SetReconcileStalenessSeconds(s.reconcileAge().Seconds())
			msg := "trusted-key reconcile failed; serving last-known state until it succeeds"
			if n > errorEscalationThreshold {
				slog.Error(msg, "pkg", "auth", "consecutiveFailures", n, "error", err.Error())
			} else {
				slog.Warn(msg, "pkg", "auth", "consecutiveFailures", n, "error", err.Error())
			}
			return fmt.Errorf("failed to list trusted keys for reconcile: %w", err)
		}
```

- Successful swap path (before `return nil`):

```go
			s.lastReconcileNanos.Store(time.Now().UnixNano())
			s.consecutiveFailures.Store(0)
			s.metrics.SetReconcileConsecutiveFailures(0)
			s.metrics.SetReconcileStalenessSeconds(0)
```

- Contention exhaustion keeps its Task 1 behaviour (no accounting — KV was reachable).

Helpers + read-path gates:

```go
// reconcileAge returns the time since the last successful reconcile.
func (s *KVTrustedKeyStore) reconcileAge() time.Duration {
	return time.Since(time.Unix(0, s.lastReconcileNanos.Load()))
}

// reconcileStale reports whether the fail-closed staleness bound is
// exceeded. Always false until the reconcile loop has started: a store
// without a loop (tests, bootstrap) must not fail auth on healthy KV.
func (s *KVTrustedKeyStore) reconcileStale() bool {
	if !s.reconcileLoopStarted.Load() {
		return false
	}
	return s.reconcileAge() > stalenessMultiplier*s.reconcileInterval
}
```

`ListForVerification`: first line —

```go
	if s.reconcileStale() {
		// Fail closed: the cache can no longer prove these keys were not
		// revoked. The reconcile loop is already logging at ERROR.
		return []*TrustedKey{}
	}
```

`Get`: gate the cache-hit fast path. Replace the initial cache lookup with:

```go
	// When the reconcile is stale past the fail-closed bound the cached
	// entry can no longer be trusted — force the KV read-through below so
	// admin flows keep operating on ground truth instead of failing closed.
	stale := s.reconcileStale()
	var cached *TrustedKey
	var ok bool
	if !stale {
		cached, ok = func() (*TrustedKey, bool) {
			s.mu.RLock()
			defer s.mu.RUnlock()
			v, o := s.keys[kid]
			return v, o
		}()
	}
```

(The rest of `Get` — cross-tenant check, `loadOne`, re-read — is unchanged; when `stale` is true, `ok` is false and control flows into `loadOne` naturally.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/auth/ -run 'TestKVTrustedKeyStore' -v`
Expected: PASS, including breaker trip + recovery + inert-without-loop.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/kv_trusted_store.go internal/auth/reconcile_metrics.go internal/auth/kv_trusted_store_reconcile_test.go
git commit -m "feat(auth): fail-closed staleness bound and reconcile health metrics for trusted keys"
```

---

### Task 5: OIDC `ReloadAll` — serialization, generation guard, skip-swap, orphan prune

**Files:**
- Modify: `internal/auth/oidc/registry.go`
- Test: `internal/auth/oidc/registry_reconcile_test.go` (new)

**Interfaces:**
- Consumes: existing `Registry` internals; `newTestRegistry(t)`, `fakeDiscovery`, `installForTest`, `addToProviderMap` (all package-internal — tests live in package `oidc`).
- Produces: `mapGen atomic.Uint64` bumped by `addToProviderMap`, `invalidateOne`, `installForTest`, and every `ReloadAll` swap; `reconcileMu sync.Mutex` serializing `ReloadAll`; skip-swap semantics (DeepEqual snapshot ⇒ keep `kidIndex`/sources, prune orphaned sources, still refresh the provider metric); `errReloadContention` sentinel; `lastReconcileNanos`/`consecutiveFailures` fields updated by `ReloadAll` (Task 7's breaker reads them). Success (including skip-swap) stores `lastReconcileNanos` and zeroes `consecutiveFailures`.

- [ ] **Step 1: Write the failing tests**

Create `internal/auth/oidc/registry_reconcile_test.go` (package `oidc`). Use the existing `newTestStore(t)` / `newTestRegistry(t)` fixtures from `registry_test.go` and a store-backed registry:

```go
package oidc

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// registryOverStore builds a Registry over a KV-backed provider store with
// a fakeDiscovery, plus the store handle for direct mutations.
func registryOverStore(t *testing.T) (*Registry, OidcProviderStore, *fakeDiscovery) {
	t.Helper()
	store := newTestStore(t)
	disc := &fakeDiscovery{docs: map[string]*DiscoveryDoc{}}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil, RegistryConfig{AllowPrivateNetworks: true})
	return r, store, disc
}

func testProvider(t *testing.T, uri string) *OidcProvider {
	t.Helper()
	return &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: uri,
		CreatedAt:          time.Now().UTC(),
		OwnerLegalEntityID: uuid.New(),
	}
}

// A provider deleted straight in the store (no broadcast) disappears from
// the registry after ReloadAll; a surviving provider's installed source is
// carried over untouched.
func TestReloadAll_DropsDeletedProvider_CarriesSurvivorSource(t *testing.T) {
	r, store, _ := registryOverStore(t)
	ctx := context.Background()

	pDoomed := testProvider(t, "https://doomed.example/.well-known/openid-configuration")
	pKeep := testProvider(t, "https://keep.example/.well-known/openid-configuration")
	if err := store.Register(ctx, pDoomed); err != nil {
		t.Fatalf("Register doomed: %v", err)
	}
	if err := store.Register(ctx, pKeep); err != nil {
		t.Fatalf("Register keep: %v", err)
	}
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}
	keepTenant := spi.TenantID(pKeep.OwnerLegalEntityID.String())
	// Install a sentinel source for the survivor so carry-over is observable.
	r.installForTest(pKeep, stubKeySource{}, &DiscoveryDoc{Issuer: "https://keep.example"})

	if err := store.Delete(ctx, spi.TenantID(pDoomed.OwnerLegalEntityID.String()), pDoomed.ID.String()); err != nil {
		t.Fatalf("store Delete: %v", err)
	}
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll after delete: %v", err)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, still := r.providers[spi.TenantID(pDoomed.OwnerLegalEntityID.String())][pDoomed.WellKnownConfigURI]; still {
		t.Fatal("deleted provider survived ReloadAll")
	}
	src := r.sources[keepTenant][pKeep.WellKnownConfigURI]
	if src == nil || src.discoveryDoc == nil {
		t.Fatal("survivor's warm source was not carried over")
	}
}

// T9: identical snapshot ⇒ swap skipped: kidIndex survives, sources kept,
// orphaned sources pruned, success accounting still updated.
func TestReloadAll_SkipSwapOnIdenticalSnapshot(t *testing.T) {
	r, store, _ := registryOverStore(t)
	ctx := context.Background()
	p := testProvider(t, "https://stable.example/.well-known/openid-configuration")
	if err := store.Register(ctx, p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}
	tenant := spi.TenantID(p.OwnerLegalEntityID.String())

	// Seed kidIndex and an orphan source.
	func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.kidIndex["kid-1"] = []providerRef{{tenant: tenant, uri: p.WellKnownConfigURI}}
		if r.sources["ghost-tenant"] == nil {
			r.sources["ghost-tenant"] = map[string]*providerSource{}
		}
		r.sources["ghost-tenant"]["https://ghost.example"] = &providerSource{}
	}()

	genBefore := r.mapGen.Load()
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("quiet-tick ReloadAll: %v", err)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.kidIndex["kid-1"]) != 1 {
		t.Fatal("skip-swap tick reset kidIndex")
	}
	if _, still := r.sources["ghost-tenant"]["https://ghost.example"]; still {
		t.Fatal("orphaned source not pruned on skip-swap tick")
	}
	if r.mapGen.Load() != genBefore {
		t.Fatal("skip-swap must not bump the generation (no map change)")
	}
	if r.lastReconcileNanos.Load() == 0 {
		t.Fatal("skip-swap tick must still count as a successful reconcile")
	}
}

// T10: a mutation racing ReloadAll's build window is not clobbered.
func TestReloadAll_GenerationGuard(t *testing.T) {
	r, store, _ := registryOverStore(t)
	ctx := context.Background()
	p := testProvider(t, "https://raced.example/.well-known/openid-configuration")
	if err := store.Register(ctx, p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}
	tenant := spi.TenantID(p.OwnerLegalEntityID.String())

	// loadAllHook fires inside ReloadAll after store.LoadAll returns (see
	// implementation) — simulate a dropped-invalidate peer delete landing
	// locally mid-build.
	fired := false
	r.loadAllHook = func() {
		if fired {
			return
		}
		fired = true
		if err := store.Delete(ctx, tenant, p.ID.String()); err != nil {
			t.Errorf("hook Delete: %v", err)
		}
		r.invalidateOne(tenant, p.WellKnownConfigURI)
	}
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll with racing mutation: %v", err)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, resurrected := r.providers[tenant][p.WellKnownConfigURI]; resurrected {
		t.Fatal("stale snapshot resurrected a provider deleted mid-build")
	}
}

// T10: overlapping ReloadAll executions serialize; nothing resurrects.
func TestReloadAll_ConcurrentCallsSerialized(t *testing.T) {
	r, store, _ := registryOverStore(t)
	ctx := context.Background()
	p := testProvider(t, "https://conc.example/.well-known/openid-configuration")
	if err := store.Register(ctx, p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.ReloadAll(ctx)
		}()
	}
	wg.Wait()
	r.mu.RLock()
	defer r.mu.RUnlock()
	tenant := spi.TenantID(p.OwnerLegalEntityID.String())
	if _, ok := r.providers[tenant][p.WellKnownConfigURI]; !ok {
		t.Fatal("provider lost under concurrent ReloadAll")
	}
}
```

Add a minimal `stubKeySource` if none exists in package tests (check first — `registry_test.go` likely has one; reuse it if so):

```go
type stubKeySource struct{}

func (stubKeySource) GetKey(kid string) (*rsa.PublicKey, error) { return nil, auth.ErrKeyNotFound }
```

(Match the actual `auth.KeySource` interface signature — verify with `go doc ./internal/auth KeySource` before writing.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/auth/oidc/ -run 'TestReloadAll' -v`
Expected: compile FAIL — `mapGen`, `lastReconcileNanos`, `loadAllHook` undefined.

- [ ] **Step 3: Implement**

In `registry.go` add fields to `Registry`:

```go
	// mapGen counts direct provider-map mutations AND ReloadAll swaps.
	// ReloadAll snapshots it before store.LoadAll and discards its built
	// snapshot if it changed by swap time — a mutation or a faster reload
	// landing mid-build always wins over the older snapshot.
	mapGen atomic.Uint64
	// reconcileMu serializes ReloadAll executions (periodic reconcile,
	// reload_all broadcast, admin endpoint) so overlapping rebuilds cannot
	// commit KV snapshots out of order.
	reconcileMu sync.Mutex

	// Reconcile health (read by the ResolveKey staleness breaker).
	lastReconcileNanos  atomic.Int64
	consecutiveFailures atomic.Int64

	// loadAllHook, when non-nil, runs after store.LoadAll inside ReloadAll.
	// Test-only seam for generation-guard races.
	loadAllHook func()
```

Bump `mapGen` in `addToProviderMap` (after the map write), `invalidateOne` (after the deletes), and `installForTest` (after the writes).

Rewrite `ReloadAll`:

```go
// maxReloadAttempts bounds the generation-guard retry. Only continuous
// provider-map churn (rare admin operations) can exhaust it.
const maxReloadAttempts = 5

// errReloadContention marks a reload that gave up because mutations kept
// landing mid-rebuild. Not a KV failure; excluded from staleness accounting.
var errReloadContention = errors.New("oidc registry reload: retry budget exhausted under concurrent mutation")

func (r *Registry) ReloadAll(ctx context.Context) error {
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()
	for attempt := 0; attempt < maxReloadAttempts; attempt++ {
		gen := r.mapGen.Load()
		providers, err := r.store.LoadAll(ctx)
		if err != nil {
			r.noteReconcileFailure(err)
			return err
		}
		if r.loadAllHook != nil {
			r.loadAllHook()
		}

		// Build fresh maps off-lock so the critical section is just the swap.
		newProv := map[spi.TenantID]map[string]*OidcProvider{}
		newSrc := map[spi.TenantID]map[string]*providerSource{}
		for _, p := range providers {
			tenant := spi.TenantID(p.OwnerLegalEntityID.String())
			if newProv[tenant] == nil {
				newProv[tenant] = map[string]*OidcProvider{}
				newSrc[tenant] = map[string]*providerSource{}
			}
			newProv[tenant][p.WellKnownConfigURI] = p
		}

		done := func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.mapGen.Load() != gen {
				return false
			}
			// Quiet tick: identical snapshot ⇒ skip the swap so kidIndex and
			// sources stay warm. reflect.DeepEqual false-negatives (e.g. a
			// locally-registered provider's monotonic-clock CreatedAt vs its
			// KV round-trip) merely cause one extra swap and then stabilize.
			if reflect.DeepEqual(newProv, r.providers) {
				r.pruneOrphanSourcesLocked()
				r.metrics.SetRegistryProviders(len(providers))
				return true
			}
			for tenant, byURI := range r.sources {
				for uri, src := range byURI {
					if _, still := newProv[tenant][uri]; still {
						newSrc[tenant][uri] = src
					}
				}
			}
			r.providers = newProv
			r.sources = newSrc
			r.kidIndex = map[string][]providerRef{}
			r.mapGen.Add(1)
			r.metrics.SetRegistryProviders(len(providers))
			return true
		}()
		if done {
			r.noteReconcileSuccess()
			return nil
		}
	}
	r.logger.Warn("oidc registry reload: giving up after repeated mid-rebuild mutations",
		"pkg", "oidc", "attempts", maxReloadAttempts)
	return errReloadContention
}

// pruneOrphanSourcesLocked drops sources whose provider is gone. A
// reloadOne install racing an earlier swap can strand one; the always-swap
// path used to collect it implicitly, the skip-swap path does it here.
// Caller must hold r.mu (write).
func (r *Registry) pruneOrphanSourcesLocked() {
	for tenant, byURI := range r.sources {
		for uri := range byURI {
			if _, ok := r.providers[tenant][uri]; !ok {
				delete(byURI, uri)
			}
		}
	}
}

func (r *Registry) noteReconcileSuccess() {
	r.lastReconcileNanos.Store(time.Now().UnixNano())
	r.consecutiveFailures.Store(0)
	r.metrics.SetReconcileConsecutiveFailures(0)
	r.metrics.SetReconcileStalenessSeconds(0)
}

func (r *Registry) noteReconcileFailure(err error) {
	n := r.consecutiveFailures.Add(1)
	r.metrics.SetReconcileConsecutiveFailures(int(n))
	r.metrics.SetReconcileStalenessSeconds(r.reconcileAge().Seconds())
	msg := "oidc provider reconcile failed; serving last-known providers until it succeeds"
	if n > errorEscalationThreshold {
		r.logger.Error(msg, "pkg", "oidc", "consecutiveFailures", n, "error", err.Error())
	} else {
		r.logger.Warn(msg, "pkg", "oidc", "consecutiveFailures", n, "error", err.Error())
	}
}

// reconcileAge returns time since the last successful ReloadAll.
func (r *Registry) reconcileAge() time.Duration {
	return time.Since(time.Unix(0, r.lastReconcileNanos.Load()))
}

// errorEscalationThreshold is the consecutive-failure count from which
// reconcile failures log at ERROR instead of WARN.
const errorEscalationThreshold = 3
```

NOTE: `noteReconcileSuccess`/`noteReconcileFailure` call two `Metrics` methods that do not exist yet — Task 7 adds them to the interface. To keep THIS task compiling and green standalone, add the interface methods + `NopMetrics` stubs in this task, and leave the OTel implementation + fixture updates to Task 7 if the compile requires them now (it will: `otelMetrics` must satisfy the interface). Pragmatic split: **add the two methods to `Metrics`, `NopMetrics`, and `otelMetrics` in this task** (mechanical), keep Task 7 focused on the breaker. Any package-local test fake implementing `Metrics` (grep `Metrics interface` implementers in `internal/auth/oidc/*_test.go`, e.g. a counting fake in `metrics_otel_test.go` or `registry_test.go`) gains the two no-op methods here too.

`otelMetrics` additions (in `metrics_otel.go`, following the `SetRegistryProviders` pattern):

```go
	// fields
	reconcileFailures  metric.Int64Gauge
	reconcileStaleness metric.Float64Gauge
	// in the constructor
	if m.reconcileFailures, err = meter.Int64Gauge("oidc.registry.reconcile_consecutive_failures"); err != nil {
		return nil, err
	}
	if m.reconcileStaleness, err = meter.Float64Gauge("oidc.registry.reconcile_staleness_seconds"); err != nil {
		return nil, err
	}
	// methods
func (m *otelMetrics) SetReconcileConsecutiveFailures(n int) {
	m.reconcileFailures.Record(context.Background(), int64(n))
}
func (m *otelMetrics) SetReconcileStalenessSeconds(sec float64) {
	m.reconcileStaleness.Record(context.Background(), sec)
}
```

Also verify lock ordering: `ReloadAllAndWarm` holds `reloadWarmMu` → calls `ReloadAll` (acquires `reconcileMu`) → `WarmJWKS`. The periodic loop acquires only `reconcileMu`. Order is strictly `reloadWarmMu → reconcileMu`, never reversed — no inversion.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/auth/oidc/ -v`
Expected: PASS (whole package — `ReloadAll` semantics changed; existing reload/warmup tests must stay green; the D18 ReloadAll tests in `registry_test.go` and `reload_warmup_test.go` exercise carry-over and must not regress).

- [ ] **Step 5: Commit**

```bash
git add internal/auth/oidc/registry.go internal/auth/oidc/metrics_otel.go internal/auth/oidc/observability.go internal/auth/oidc/registry_reconcile_test.go
git commit -m "feat(oidc): serialize ReloadAll with generation guard, skip-swap on quiet ticks, reconcile health accounting"
```

---

### Task 6: OIDC reconcile loop + warmup composition

**Files:**
- Modify: `internal/auth/oidc/registry.go`
- Test: `internal/auth/oidc/registry_reconcile_test.go` (extend)

**Interfaces:**
- Consumes: Task 5's `ReloadAll` accounting.
- Produces: `RegistryConfig.ReconcileInterval time.Duration` (defaulted to 60s in `NewRegistry` like the other cfg fields), `func (r *Registry) StartReconcileLoop(ctx context.Context) bool` (Task 9 wires it), unexported `reconcileLoopStarted atomic.Bool` (Task 7's breaker arms off it).

- [ ] **Step 1: Write the failing tests**

```go
// T8: the loop converges a store-level delete with no broadcast at all.
func TestRegistry_ReconcileLoop_ConvergesStoreDelete(t *testing.T) {
	store := newTestStore(t)
	disc := &fakeDiscovery{docs: map[string]*DiscoveryDoc{}}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil, RegistryConfig{
		AllowPrivateNetworks: true,
		ReconcileInterval:    20 * time.Millisecond,
	})
	ctx := context.Background()
	p := testProvider(t, "https://loop.example/.well-known/openid-configuration")
	if err := store.Register(ctx, p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !r.StartReconcileLoop(loopCtx) {
		t.Fatal("StartReconcileLoop returned false on first call")
	}
	if r.StartReconcileLoop(loopCtx) {
		t.Fatal("second StartReconcileLoop must return false")
	}

	tenant := spi.TenantID(p.OwnerLegalEntityID.String())
	if err := store.Delete(ctx, tenant, p.ID.String()); err != nil {
		t.Fatalf("store Delete: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		gone := func() bool {
			r.mu.RLock()
			defer r.mu.RUnlock()
			_, ok := r.providers[tenant][p.WellKnownConfigURI]
			return !ok
		}()
		if gone {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("reconcile loop never dropped the store-deleted provider")
}

// T11: composition — a provider registered on a peer (no broadcast) becomes
// resolvable via reconcile discovery + warmup-retry warm.
func TestRegistry_ReconcilePlusWarmupRetry_Composition(t *testing.T) {
	idp := NewFixtureIdP(t)
	store := newTestStore(t)
	disc := NewHTTPDiscovery(DiscoveryConfig{AllowPrivateNetworks: true})
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil, RegistryConfig{
		AllowPrivateNetworks: true,
		ReconcileInterval:    20 * time.Millisecond,
		WarmupRetryInterval:  20 * time.Millisecond,
	})
	ctx := context.Background()
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !r.StartReconcileLoop(loopCtx) {
		t.Fatal("StartReconcileLoop")
	}
	if !r.StartWarmupRetryLoop(loopCtx) {
		t.Fatal("StartWarmupRetryLoop")
	}

	// "Peer" registers straight into the store — this node hears nothing.
	p := testProvider(t, idp.Server.URL+"/.well-known/openid-configuration")
	if err := store.Register(ctx, p); err != nil {
		t.Fatalf("Register: %v", err)
	}

	kid := "default"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if res, err := r.ResolveKey(kid, idp.Issuer, ""); err == nil && res != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("peer-registered provider never became resolvable via reconcile+warmup composition")
}
```

(Adapt the `NewFixtureIdP` / `NewHTTPDiscovery` calls to the exact fixture signatures in `fixture_test.go` / `discovery.go` — both exist; check `DiscoveryConfig` field names before writing.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/auth/oidc/ -run 'TestRegistry_Reconcile' -v`
Expected: compile FAIL — `ReconcileInterval` / `StartReconcileLoop` undefined.

- [ ] **Step 3: Implement**

`RegistryConfig` gains:

```go
	// ReconcileInterval is the tick interval of StartReconcileLoop, the
	// periodic KV-reconcile backstop behind the best-effort provider
	// broadcast. Zero or negative values default to 60 s (matching
	// CYODA_AUTH_CACHE_RECONCILE_INTERVAL). The per-tick wait is jittered
	// ±10% to avoid a cross-node herd.
	ReconcileInterval time.Duration
```

`NewRegistry` default: `if cfg.ReconcileInterval <= 0 { cfg.ReconcileInterval = 60 * time.Second }`.

Registry field `reconcileLoopStarted atomic.Bool` and:

```go
// StartReconcileLoop starts the periodic KV-reconcile backstop: every
// ReconcileInterval (jittered ±10%) the provider map is rebuilt from the
// authoritative store via ReloadAll, bounding the staleness a dropped
// broadcast can cause. NOT ReloadAllAndWarm: no per-tick outbound IdP
// traffic — JWKS key freshness stays governed by the per-source cache, and
// the warmup-retry loop warms any provider the reconcile discovers cold.
// Returns false (starting nothing) if already running; exits on ctx cancel.
func (r *Registry) StartReconcileLoop(ctx context.Context) bool {
	if !r.reconcileLoopStarted.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		for {
			timer := time.NewTimer(jitteredInterval(r.cfg.ReconcileInterval))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				tickCtx, cancel := context.WithTimeout(ctx, r.cfg.ReconcileInterval)
				_ = r.ReloadAll(tickCtx) // failures logged/accounted inside
				cancel()
			}
		}
	}()
	return true
}

// jitteredInterval returns d × [0.9, 1.1).
func jitteredInterval(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
}
```

Import `"math/rand/v2"`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/auth/oidc/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/oidc/registry.go internal/auth/oidc/registry_reconcile_test.go
git commit -m "feat(oidc): jittered periodic reconcile loop composing with warmup retry"
```

---

### Task 7: OIDC staleness breaker in `ResolveKey`

**Files:**
- Modify: `internal/auth/oidc/registry.go`
- Test: `internal/auth/oidc/registry_reconcile_test.go` (extend)

**Interfaces:**
- Consumes: Task 5's `lastReconcileNanos`/`reconcileAge`, Task 6's `reconcileLoopStarted`.
- Produces: `var ErrRegistryStale = errors.New("oidc provider registry stale: reconcile has not succeeded within the staleness bound")`. It must NOT wrap `auth.ErrUnknownKID` — any non-ErrUnknownKID error hard-fails the validator chain into the uniform 401 (`internal/auth/chain.go`), which is the intended fail-closed surface.

- [ ] **Step 1: Write the failing tests**

```go
// T12: ResolveKey fails closed past the staleness bound and recovers.
func TestRegistry_ResolveKey_StalenessBreaker(t *testing.T) {
	store := newTestStore(t)
	disc := &fakeDiscovery{docs: map[string]*DiscoveryDoc{}}
	r := NewRegistry(store, disc, nil, NopMetrics{}, nil, RegistryConfig{
		AllowPrivateNetworks: true,
		ReconcileInterval:    20 * time.Millisecond,
	})
	ctx := context.Background()
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}

	// Not started → breaker inert even with an ancient lastReconcile.
	r.lastReconcileNanos.Store(time.Now().Add(-time.Hour).UnixNano())
	if _, err := r.ResolveKey("any-kid", "https://iss.example", ""); errors.Is(err, ErrRegistryStale) {
		t.Fatal("breaker tripped before the reconcile loop ever started")
	}

	// Started + stale → ErrRegistryStale.
	loopCtx, cancel := context.WithCancel(ctx)
	cancel() // loop exits immediately; started-flag remains set
	r.StartReconcileLoop(loopCtx)
	r.lastReconcileNanos.Store(time.Now().Add(-time.Hour).UnixNano())
	if _, err := r.ResolveKey("any-kid", "https://iss.example", ""); !errors.Is(err, ErrRegistryStale) {
		t.Fatalf("want ErrRegistryStale past the bound, got %v", err)
	}

	// Recovery: a successful reconcile resets the clock.
	if err := r.ReloadAll(ctx); err != nil {
		t.Fatalf("recovery ReloadAll: %v", err)
	}
	if _, err := r.ResolveKey("any-kid", "https://iss.example", ""); errors.Is(err, ErrRegistryStale) {
		t.Fatal("breaker still tripped after successful reconcile")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/auth/oidc/ -run 'TestRegistry_ResolveKey_StalenessBreaker' -v`
Expected: compile FAIL — `ErrRegistryStale` undefined.

- [ ] **Step 3: Implement**

In `registry.go`:

```go
// stalenessMultiplier × ReconcileInterval is the fail-closed bound: once
// the reconcile loop runs, a provider map older than this stops resolving
// keys — an unbounded-stale answer on the verification path would be
// wrong-but-available.
const stalenessMultiplier = 10

// ErrRegistryStale is returned by ResolveKey when the reconcile loop has
// not succeeded within the staleness bound. It deliberately does NOT wrap
// auth.ErrUnknownKID: the validator chain hard-fails on it, collapsing to
// the uniform 401.
var ErrRegistryStale = errors.New("oidc provider registry stale: reconcile has not succeeded within the staleness bound")

// reconcileStale reports whether the fail-closed bound is exceeded. Always
// false until StartReconcileLoop has been called.
func (r *Registry) reconcileStale() bool {
	if !r.reconcileLoopStarted.Load() {
		return false
	}
	return r.reconcileAge() > stalenessMultiplier*r.cfg.ReconcileInterval
}
```

First lines of `ResolveKey`:

```go
	if r.reconcileStale() {
		return nil, ErrRegistryStale
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/auth/oidc/ -v`, then the consumers: `go test ./internal/auth/ -run 'TestChain|TestDelegating' -v`
Expected: PASS (chain tests confirm non-ErrUnknownKID errors already hard-fail → uniform 401; no chain change needed).

- [ ] **Step 5: Commit**

```bash
git add internal/auth/oidc/registry.go internal/auth/oidc/registry_reconcile_test.go
git commit -m "feat(oidc): fail closed in ResolveKey once the reconcile staleness bound is exceeded"
```

---

### Task 8: Config — `CYODA_AUTH_CACHE_RECONCILE_INTERVAL`

**Files:**
- Modify: `app/config.go`, `cmd/cyoda/help/config_registry.go`, `cmd/cyoda/help/content/config/auth.md`, `README.md`
- Test: `app/config_test.go` (extend — find the existing `ValidateIAM` tests and follow their table style)

**Interfaces:**
- Produces: `IAMConfig.AuthCacheReconcileInterval time.Duration`; default `60s` via `envDuration("CYODA_AUTH_CACHE_RECONCILE_INTERVAL", 60*time.Second)`; `ValidateIAM` rejects `< 1s` **unconditionally** (before the mock-mode early return — a bad explicit value is a config error in any mode). Task 9 consumes the field.

- [ ] **Step 1: Write the failing tests**

In `app/config_test.go` (adapt names to the file's existing conventions):

```go
func TestDefaultConfig_AuthCacheReconcileInterval(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.IAM.AuthCacheReconcileInterval != 60*time.Second {
		t.Fatalf("default AuthCacheReconcileInterval: want 60s, got %s", cfg.IAM.AuthCacheReconcileInterval)
	}
}

func TestValidateIAM_ReconcileIntervalFloor(t *testing.T) {
	iam := DefaultConfig().IAM
	iam.AuthCacheReconcileInterval = 500 * time.Millisecond
	if err := ValidateIAM(iam); err == nil {
		t.Fatal("sub-second reconcile interval must be rejected")
	}
	// Rejected in mock mode too — a bad explicit value is a config error
	// regardless of mode.
	iam.Mode = "mock"
	if err := ValidateIAM(iam); err == nil {
		t.Fatal("sub-second reconcile interval must be rejected in mock mode")
	}
	iam.AuthCacheReconcileInterval = time.Second
	iam.Mode = "mock"
	if err := ValidateIAM(iam); err != nil {
		t.Fatalf("1s interval must be accepted: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./app/ -run 'AuthCacheReconcile|ReconcileIntervalFloor' -v`
Expected: compile FAIL — field undefined.

- [ ] **Step 3: Implement**

`IAMConfig` (place near `TrustedKeyMaxPerTenant`):

```go
	// AuthCacheReconcileInterval is the shared periodic KV-reconcile
	// interval for the per-node auth caches (trusted keys, OIDC providers).
	// Jittered ±10% per tick; the fail-closed staleness bound is fixed at
	// 10× this value. CYODA_AUTH_CACHE_RECONCILE_INTERVAL, default 60s,
	// floor 1s.
	AuthCacheReconcileInterval time.Duration
```

`DefaultConfig()` inside the `IAM:` literal:

```go
			AuthCacheReconcileInterval: envDuration("CYODA_AUTH_CACHE_RECONCILE_INTERVAL", 60*time.Second),
```

`ValidateIAM` — insert BEFORE the `iam.Mode == "mock"` early return:

```go
	// Unconditional: a bad explicit interval is a config error in any mode.
	if iam.AuthCacheReconcileInterval < time.Second {
		return fmt.Errorf("CYODA_AUTH_CACHE_RECONCILE_INTERVAL must be >= 1s, got %s", iam.AuthCacheReconcileInterval)
	}
```

`cmd/cyoda/help/config_registry.go` — add alongside the other `auth`-topic vars:

```go
	{Name: "CYODA_AUTH_CACHE_RECONCILE_INTERVAL", Topic: "auth", Type: "duration", Default: "60s", Description: "Periodic KV-reconcile interval for the trusted-key and OIDC-provider caches; jittered ±10%; verification fails closed after 10× this without a successful reconcile."},
```

`cmd/cyoda/help/content/config/auth.md` — add a compact section following the file's existing style (read it first), stating: shared by both auth caches; broadcast fast path + reconcile backstop; 10× fail-closed bound; floor 1s. Keep prose compact per project convention.

`README.md` — add one row to the env-var table (find the IAM/auth rows; match column format):

```markdown
| `CYODA_AUTH_CACHE_RECONCILE_INTERVAL` | `60s` | Reconcile interval for the trusted-key / OIDC-provider caches; verification fails closed after 10× this without a successful KV reconcile. |
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./app/ -v` and `go test ./cmd/... -v` (help registry has parity tests over topics/content)
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add app/config.go app/config_test.go cmd/cyoda/help/config_registry.go cmd/cyoda/help/content/config/auth.md README.md
git commit -m "feat(config): CYODA_AUTH_CACHE_RECONCILE_INTERVAL for auth-cache reconciliation"
```

---

### Task 9: Wiring in `app/app.go`

**Files:**
- Modify: `app/app.go` (jwt-mode block, ~lines 240–330)

**Interfaces:**
- Consumes: `auth.WithTrustedKeyBroadcaster`, `auth.WithReconcileInterval`, `auth.WithReconcileMetrics`, `auth.NewOTelReconcileMetrics`, `trustedKeyStore.StartReconcileLoop`, `oidc.RegistryConfig.ReconcileInterval`, `oidcRegistry.StartReconcileLoop`, `cfg.IAM.AuthCacheReconcileInterval`.

- [ ] **Step 1: Wire (no new unit test — behaviour is covered by Tasks 1–8; the full E2E suite boots this code path in jwt mode and must stay green)**

9a. Trusted-key store construction — replace the current call:

```go
		authReconcileMetrics, err := auth.NewOTelReconcileMetrics(observability.Meter())
		if err != nil {
			slog.Error("startup failure", "phase", "auth-reconcile-metrics-init", "error", err.Error())
			os.Exit(1)
		}
		trustedOpts := []auth.KVTrustedKeyStoreOption{
			auth.WithMaxTrustedKeys(cfg.IAM.TrustedKeyMaxPerTenant),
			auth.WithReconcileInterval(cfg.IAM.AuthCacheReconcileInterval),
			auth.WithReconcileMetrics(authReconcileMetrics),
		}
		if gossipReg != nil {
			// Typed-nil guard: only append when non-nil (same rationale as
			// cacheBroadcaster above).
			trustedOpts = append(trustedOpts, auth.WithTrustedKeyBroadcaster(gossipReg))
		}
		trustedKeyStore, err := auth.NewKVTrustedKeyStore(systemCtx, kvStore, trustedOpts...)
		if err != nil {
			slog.Error("startup failure",
				"phase", "kv-trusted-store-bootstrap",
				"error", err.Error())
			os.Exit(1)
		}
		// Periodic KV-reconcile backstop; systemCtx is process-lifetime, so
		// the loop runs until exit (same lifecycle as the warmup retry loop).
		trustedKeyStore.StartReconcileLoop(systemCtx)
```

9b. OIDC registry config — add the interval to the existing `oidc.RegistryConfig{...}` literal:

```go
			ReconcileInterval:    cfg.IAM.AuthCacheReconcileInterval,
```

9c. `pendingWarmJWKS` — start the reconcile loop with the others:

```go
		pendingWarmJWKS = func() {
			oidcRegistry.WarmJWKS(systemCtx)
			oidcRegistry.StartWarmupRetryLoop(systemCtx)
			oidcRegistry.StartReconcileLoop(systemCtx)
		}
```

- [ ] **Step 2: Build + scoped verification**

Run: `go build ./... && go vet ./... && go test ./app/ ./internal/auth/... -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add app/app.go
git commit -m "feat(app): wire auth-cache reconcile loops, trusted-key gossip, and reconcile metrics"
```

---

### Task 10: Documentation — ARCHITECTURE §7.2 / §7.3

**Files:**
- Modify: `docs/ARCHITECTURE.md`

- [ ] **Step 1: Rewrite §7.2's trusted-key cache paragraph** (currently ~line 1119). Replace the single paragraph beginning "`KVTrustedKeyStore`'s cache is populated once…" with (present tense, reference style, no issue IDs):

```markdown
`KVTrustedKeyStore` keeps a per-node cache over the KV store and converges across the cluster on three layers: mutations publish a payload-free change ping on the `auth.trustedkeys` gossip topic (receivers re-read KV, coalescing concurrent pings); a periodic reconcile loop (`CYODA_AUTH_CACHE_RECONCILE_INTERVAL`, default 60s, jittered ±10%) rebuilds the cache from KV as the backstop when a ping drops; and `Get` reads through to KV on a cache miss. If reconciliation has not succeeded for 10× the interval — KV unavailable that long means the platform is effectively down — verification-path enumeration fails closed (trusted-key-signed grants are rejected) while `Get` switches to KV read-through, keeping admin operations on ground truth. First-party JWT validation is unaffected (in-process key source, no KV dependency).
```

Note: the replaced paragraph's claim that a registered key "stays invisible to node B until B restarts" was wrong even pre-change (`Get` always had KV read-through; only the enumeration paths were restart-bound) — do not carry any variant of it forward.

- [ ] **Step 2: Rewrite §7.3's final paragraph** ("JWKS caching and cache eviction"). Replace the sentence "The broadcast is best-effort and fire-and-forget, and unlike the model cache (§4.1) there is no TTL lease behind it — a peer that misses the message keeps serving its cached keys until the next explicit reload." with:

```markdown
The broadcast is best-effort and fire-and-forget; behind it, the same reconcile backstop as the trusted-key cache (`CYODA_AUTH_CACHE_RECONCILE_INTERVAL`, default 60s, jittered ±10%) periodically rebuilds the provider map from KV, so a dropped message costs at most one interval of staleness rather than persisting until an explicit reload. Warm JWKS sources are carried over on reconcile — the backstop never causes IdP re-fetch traffic; key freshness stays governed by the per-source JWKS cache TTL. If reconciliation has not succeeded for 10× the interval, `ResolveKey` fails closed and OIDC-issued tokens are rejected with the uniform 401 until a reconcile succeeds.
```

- [ ] **Step 3: Audit both touched sections** for other stale claims (present tense, delete anything unverifiable — per the ARCHITECTURE-is-a-reference rule). Verify §4.1 cross-references still hold.

- [ ] **Step 4: Commit**

```bash
git add docs/ARCHITECTURE.md
git commit -m "docs(architecture): auth-cache reconciliation layers in 7.2/7.3"
```

---

## Coverage matrix (carried from spec §10 — gap check at review time)

| Spec scenario | Plan location |
|---|---|
| T1 cross-node convergence (all mutation kinds) | Task 1 test 1 |
| T2 corrupt-record skip / transport keep-state | Task 1 tests 2a/2b |
| T3 generation guard (trusted) | Task 1 test 3 |
| T4 reconcile-vs-reconcile serialization (trusted) | Task 1 test 4 (+ `make race` at deliverable end) |
| T5 ping publish/receive/coalesce/payload-ignored | Task 2 tests |
| T6 breaker trip / Get read-through / recovery / inert-without-loop | Task 4 tests |
| T7 loop converge / once-guard / ctx cancel | Task 3 test |
| T8 OIDC store-delete convergence, survivor source carry-over | Task 5 test 1 + Task 6 test 1 |
| T9 skip-swap keeps kidIndex, prunes orphans, counts success | Task 5 test 2 |
| T10 OIDC generation guard + concurrent ReloadAll | Task 5 tests 3/4 |
| T11 reconcile ∘ warmup composition | Task 6 test 2 |
| T12 OIDC breaker trip + recovery + inert-without-loop | Task 7 test |
| T13 config default / floor / wiring | Task 8 tests; wiring exercised by E2E jwt boot (Task 9) |

No HTTP/gRPC surface changes ⇒ no new e2e/parity scenarios; no new error codes ⇒ no `errors/<CODE>.md`; COMPATIBILITY.md untouched (no SPI pin, release, or chart change); CHANGELOG via the v0.8.4 milestone convention.

## End-of-deliverable verification (after Task 10, before PR)

1. `go test ./... -v` (root module, includes E2E — Docker required)
2. `go vet ./...`
3. `make race` (once)
4. `make test-short-all` (plugin submodules untouched, but confirm no cross-module breakage)
