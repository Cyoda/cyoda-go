# Compute-node Callback Transaction Join — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make an external compute node's CRUD callbacks (gRPC and HTTP) join the originating workflow transition's transaction, so callback writes are atomic with it and callback reads see its uncommitted writes.

**Architecture:** The tx-owning node mints an HMAC tx-token `{NodeID, TxRef=txID}` and attaches it to the outbound calc request as a CloudEvent attribute; the compute-node SDK echoes it on callbacks (`tx-token` gRPC metadata / `X-Tx-Token` HTTP header). A callback is routed to the owner (local `Join` when `NodeID==self`, else reverse-proxy), joined via `spi.TransactionManager.Join`, and executed inside the owner's transaction. The shared `entity.Handler` participates instead of `Begin`-ning when the ctx already carries a joined tx; a per-tx application gate (`internal/txgate`) serialises concurrent callbacks and the owner's commit. The signer always exists (ephemeral secret in single-node) so one code path covers single-node and cluster.

**Tech Stack:** Go 1.26, `log/slog`, `github.com/google/uuid`, pgx v5 (postgres plugin), gRPC + CloudEvents protobuf, testcontainers-go (e2e).

## Global Constraints

- Go 1.26+; `log/slog` only (never `log`/`fmt.Printf`). `uuid.UUID`, not `string`, for UUIDs.
- 4xx: full domain detail + error code. 5xx: generic message + ticket UUID. No internals/tokens in responses or logs (Gate 3).
- No new error codes: reuse `TRANSACTION_NOT_FOUND` (404), `TRANSACTION_EXPIRED` (410), `TRANSACTION_NODE_UNAVAILABLE` (503), `UNAUTHORIZED` (401), `FORBIDDEN` (403). No `#NNN` issue IDs in shipped artefacts (code, help, responses).
- **cyoda-go is primarily multi-node.** Cluster-off-by-default is an onboarding affordance; multi-node correctness is first-class and is delivered in this change, not deferred.
- No SPI change (the concurrency gate is an application contract per `cyoda-go-spi/transaction.go:46-54`), therefore no coordinated SPI release.
- Every task is red→green→refactor; commit at the end of each task. Run `go build ./... && go vet ./...` before each commit. Full verification (`go test ./... -v`, plugins, `make race`) is Task 16.
- Worktree: `.claude/worktrees/feat+287-processor-callback-tx-join` (branch `worktree-feat+287-processor-callback-tx-join`). All commands run from the worktree root unless noted.

---

## File Structure

**New files**
- `internal/txgate/txgate.go` — per-txID keyed exclusive mutex registry (ref-counted, self-cleaning).
- `internal/txgate/txgate_test.go`
- `internal/grpc/txtoken.go` — `AttachTxToken` (CloudEvent attribute) + `WithTxToken`/`TxTokenFromContext` (ctx carrier for pre-minted token) + attribute-name const.
- `internal/grpc/txtoken_test.go`
- `internal/domain/txjoin/txjoin.go` — transport-agnostic `JoinFromToken(ctx, signer, txMgr, tok)` verify+Join+error-map helper.
- `internal/domain/txjoin/txjoin_test.go`
- `internal/grpc/txroute_interceptor.go` — gRPC unary/stream interceptor: ResolveTarget → proxy-to-owner (new gRPC forward) or local Join.
- `internal/grpc/txroute_interceptor_test.go`
- `internal/cluster/proxy/grpc_forward.go` — gRPC `EntityManage` reverse-forward transport (B→A).
- `internal/httpmw/txjoin_mw.go` — always-on HTTP middleware: extract `X-Tx-Token` → Join into request ctx.
- `internal/httpmw/txjoin_mw_test.go`
- `internal/e2e/callback_txjoin_test.go`, `internal/e2e/callback_txjoin_modes_test.go`, `internal/e2e/callback_txjoin_errors_test.go`
- `e2e/parity/callback_txjoin.go` (+ registry entry)
- `e2e/parity/multinode/callback_route.go`
- `cmd/cyoda/help/content/errors/` — edits (no new files).

**Modified files**
- `app/app.go` — always build `tokenSigner` + `selfNodeID`; inject signer/nodeID/gate into dispatcher(s), CloudEventsService, entity Handler; wire gRPC interceptor + HTTP TxJoin middleware.
- `internal/grpc/dispatch.go` — `ProcessorDispatcher` gains signer/selfNodeID/ttl; mint+attach in `DispatchProcessor`/`DispatchCriteria`.
- `internal/cluster/dispatch/cluster_dispatcher.go` — pre-mint token, thread to local ctx + forward.
- `internal/cluster/dispatch/types.go` — `TxToken` field on both request DTOs.
- `internal/cluster/dispatch/handler.go` — receiver puts `req.TxToken` into ctx.
- `internal/domain/entity/handler.go` — `Handler` gains `gate *txgate.Registry`; constructor updated.
- `internal/domain/entity/service.go` — `beginOrJoin`/`finishOwned` helpers; all 6 Begin flows participate-if-joined; gate around owner commit.
- `internal/grpc/service.go` (CloudEventsServiceImpl) — carry signer/registry/selfNodeID for the interceptor.
- `internal/cluster/proxy/http.go` — align `handleTokenError` (tampered/invalid → `UNAUTHORIZED` 401).
- `cmd/compute-test-client/*` — echo `tx-token`; optional callback mode for e2e.
- `docs/PROCESSOR_EXECUTION_MODES.md`, `docs/ARCHITECTURE.md`, `docs/PRD.md`, `.claude/rules/multi-node-primary.md` (new), cluster config help topic, `DefaultConfig()` comment, `COMPATIBILITY.md`, `CHANGELOG.md`.

---

## Task 1: `internal/txgate` — per-txID exclusive gate

**Files:**
- Create: `internal/txgate/txgate.go`, `internal/txgate/txgate_test.go`

**Interfaces:**
- Produces: `type Registry struct{...}`; `func New() *Registry`; `func (r *Registry) Acquire(txID string) (release func())`. `Acquire` blocks until the caller holds the exclusive gate for `txID`; `release` frees it. Ref-counted so the internal map entry is deleted when no holders/waiters remain (no unbounded growth).

- [ ] **Step 1: Write the failing test**

```go
package txgate

import (
	"sync"
	"testing"
	"time"
)

func TestRegistry_SerialisesSameTxID(t *testing.T) {
	r := New()
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	rel1 := r.Acquire("tx-1")
	wg.Add(1)
	go func() {
		defer wg.Done()
		rel := r.Acquire("tx-1") // must block until rel1() runs
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
		rel()
	}()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	order = append(order, 1)
	mu.Unlock()
	rel1()
	wg.Wait()
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("expected serialized order [1 2], got %v", order)
	}
}

func TestRegistry_DifferentTxIDsDoNotBlock(t *testing.T) {
	r := New()
	rel1 := r.Acquire("tx-1")
	done := make(chan struct{})
	go func() { rel2 := r.Acquire("tx-2"); rel2(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Acquire on a different txID blocked")
	}
	rel1()
}

func TestRegistry_ReleasesMapEntry(t *testing.T) {
	r := New()
	r.Acquire("tx-1")() // acquire+release
	if n := r.len(); n != 0 {
		t.Fatalf("expected empty gate map after release, got %d", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/txgate/ -run TestRegistry -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Write minimal implementation**

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/txgate/ -run TestRegistry -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
go build ./... && go vet ./internal/txgate/
git add internal/txgate/
git commit -m "feat(txgate): per-txID exclusive gate registry for callback/commit serialisation"
```

---

## Task 2: Always construct the token signer + self node id

**Files:**
- Modify: `app/app.go:108-121` (signer construction), and add a `selfNodeID` resolution helper.
- Test: `app/app_signer_test.go` (create)

**Interfaces:**
- Produces: after `App` build, `a.TokenSigner()` is non-nil in **both** single-node and cluster mode. New unexported `a.selfNodeID string` field = `cfg.Cluster.NodeID` when cluster enabled, else `"local"`.

- [ ] **Step 1: Write the failing test**

```go
package app

import "testing"

func TestSigner_AlwaysPresentSingleNode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Cluster.Enabled = false
	a, err := New(cfg) // adjust to the real constructor used in existing app tests
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.TokenSigner() == nil {
		t.Fatal("expected non-nil TokenSigner in single-node mode")
	}
	if a.selfNodeID != "local" {
		t.Fatalf("expected selfNodeID 'local', got %q", a.selfNodeID)
	}
}
```

(If `New(cfg)` is not the real entrypoint, mirror the construction path used by existing `app` tests; the assertion is what matters.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./app/ -run TestSigner_AlwaysPresentSingleNode -v`
Expected: FAIL — `a.TokenSigner()` nil / `selfNodeID` undefined.

- [ ] **Step 3: Write minimal implementation**

In `app/app.go`, add field `selfNodeID string` to the `App` struct. Replace the cluster-only signer block (lines ~108-121) so the signer is always built:

```go
	// Transaction routing token signer. Always present: in cluster mode it uses
	// the configured HMAC secret so tokens verify across nodes; in single-node
	// mode it uses a per-process ephemeral secret (tokens only round-trip
	// through this process, and a tx never outlives the process).
	a.selfNodeID = "local"
	secret := cfg.Cluster.HMACSecret
	if cfg.Cluster.Enabled {
		validateClusterConfig(cfg.Cluster)
		a.selfNodeID = cfg.Cluster.NodeID
		gossipReg = mustNewGossip(cfg.Cluster)
	} else {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			slog.Error("failed to generate ephemeral token secret", "pkg", "cluster", "err", err)
			os.Exit(1)
		}
	}
	var signerErr error
	a.tokenSigner, signerErr = token.NewSigner(secret)
	if signerErr != nil {
		slog.Error("failed to create token signer", "pkg", "cluster", "err", signerErr)
		os.Exit(1)
	}
```

Add `"crypto/rand"` to imports. Keep `gossipReg` declared as before.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./app/ -run TestSigner_AlwaysPresentSingleNode -v`
Expected: PASS. Then `go test ./app/ -run TestSigner -count=1` and existing app tests stay green.

- [ ] **Step 5: Commit**

```bash
go build ./... && go vet ./app/
git add app/
git commit -m "feat(app): always construct tx-token signer (ephemeral secret single-node)"
```

---

## Task 3: CloudEvent tx-token attach + ctx carrier

**Files:**
- Create: `internal/grpc/txtoken.go`, `internal/grpc/txtoken_test.go`

**Interfaces:**
- Produces:
  - `const TxTokenAttr = "cyodatxtoken"` (CloudEvent attribute name; lowercase, no separators — CloudEvents attribute-name rule).
  - `func AttachTxToken(ce *cepb.CloudEvent, tok string)` — sets the attribute when `tok != ""`.
  - `func TxTokenFromCloudEvent(ce *cepb.CloudEvent) string` — reads it back ("" if absent).
  - `func WithTxToken(ctx context.Context, tok string) context.Context` / `func TxTokenFromContext(ctx context.Context) string` — carry a pre-minted token through the dispatch call chain.

- [ ] **Step 1: Write the failing test**

```go
package grpc

import (
	"context"
	"testing"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

func TestAttachAndReadTxToken(t *testing.T) {
	ce := &cepb.CloudEvent{}
	AttachTxToken(ce, "tok-abc")
	if got := TxTokenFromCloudEvent(ce); got != "tok-abc" {
		t.Fatalf("got %q", got)
	}
}

func TestAttachTxToken_EmptyIsNoop(t *testing.T) {
	ce := &cepb.CloudEvent{}
	AttachTxToken(ce, "")
	if got := TxTokenFromCloudEvent(ce); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestTxTokenContextRoundTrip(t *testing.T) {
	ctx := WithTxToken(context.Background(), "tok-ctx")
	if got := TxTokenFromContext(ctx); got != "tok-ctx" {
		t.Fatalf("got %q", got)
	}
	if got := TxTokenFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/grpc/ -run 'TxToken' -v`
Expected: FAIL — `undefined: AttachTxToken`.

- [ ] **Step 3: Write minimal implementation**

```go
package grpc

import (
	"context"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// TxTokenAttr is the CloudEvent extension attribute carrying the signed
// transaction routing token on an outbound processor/criteria calc request.
// The compute-node SDK echoes it as tx-token gRPC metadata / X-Tx-Token HTTP
// header on any callback into cyoda-go.
const TxTokenAttr = "cyodatxtoken"

func AttachTxToken(ce *cepb.CloudEvent, tok string) {
	if ce == nil || tok == "" {
		return
	}
	if ce.Attributes == nil {
		ce.Attributes = make(map[string]*cepb.CloudEvent_CloudEventAttributeValue)
	}
	ce.Attributes[TxTokenAttr] = &cepb.CloudEvent_CloudEventAttributeValue{
		Attr: &cepb.CloudEvent_CloudEventAttributeValue_CeString{CeString: tok},
	}
}

func TxTokenFromCloudEvent(ce *cepb.CloudEvent) string {
	if ce == nil || ce.Attributes == nil {
		return ""
	}
	v, ok := ce.Attributes[TxTokenAttr]
	if !ok {
		return ""
	}
	return v.GetCeString()
}

type txTokenCtxKey struct{}

func WithTxToken(ctx context.Context, tok string) context.Context {
	return context.WithValue(ctx, txTokenCtxKey{}, tok)
}

func TxTokenFromContext(ctx context.Context) string {
	tok, _ := ctx.Value(txTokenCtxKey{}).(string)
	return tok
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/grpc/ -run 'TxToken' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go build ./... && go vet ./internal/grpc/
git add internal/grpc/txtoken.go internal/grpc/txtoken_test.go
git commit -m "feat(grpc): tx-token CloudEvent attribute + ctx carrier helpers"
```

---

## Task 4: ProcessorDispatcher mints + attaches the tx-token

**Files:**
- Modify: `internal/grpc/dispatch.go:26-39` (struct+ctor), `:43-141` (`DispatchProcessor`), `:168-294` (`DispatchCriteria`).
- Test: `internal/grpc/dispatch_txtoken_test.go` (create)

**Interfaces:**
- Consumes: `AttachTxToken`, `TxTokenFromContext` (Task 3); `token.Signer.Issue` (`internal/cluster/token`).
- Produces: `NewProcessorDispatcher(registry *MemberRegistry, uuids spi.UUIDGenerator, signer *token.Signer, selfNodeID string, tokenTTL time.Duration)`. On dispatch with non-empty `txID`, the CloudEvent carries `TxTokenAttr` = a token whose `TxRef==txID`; with empty `txID`, no attribute. A ctx-carried token (from ClusterDispatcher) overrides self-minting.

- [ ] **Step 1: Write the failing test**

```go
package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestDispatch_MintsTxTokenFromTxID(t *testing.T) {
	signer, _ := token.NewSigner(make32(t))
	d := NewProcessorDispatcher(NewMemberRegistry(nil), fakeUUIDs{}, signer, "node-A", time.Minute)

	// Build the calc CloudEvent as DispatchProcessor would, then assert the
	// helper it will call attaches a token whose TxRef == txID. (Unit-level:
	// call the extracted mint helper directly if DispatchProcessor needs a live
	// member; see Step 3 for the resolveTxToken helper under test.)
	tok := d.resolveTxToken(context.Background(), "tx-42")
	claims, err := signer.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.NodeID != "node-A" || claims.TxRef != "tx-42" {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestDispatch_EmptyTxIDNoToken(t *testing.T) {
	signer, _ := token.NewSigner(make32(t))
	d := NewProcessorDispatcher(NewMemberRegistry(nil), fakeUUIDs{}, signer, "node-A", time.Minute)
	if tok := d.resolveTxToken(context.Background(), ""); tok != "" {
		t.Fatalf("expected empty token, got %q", tok)
	}
}

func TestDispatch_CtxTokenOverridesSelfMint(t *testing.T) {
	signer, _ := token.NewSigner(make32(t))
	d := NewProcessorDispatcher(NewMemberRegistry(nil), fakeUUIDs{}, signer, "node-B", time.Minute)
	ctx := WithTxToken(context.Background(), "pre-minted-A")
	if tok := d.resolveTxToken(ctx, "tx-42"); tok != "pre-minted-A" {
		t.Fatalf("expected ctx token, got %q", tok)
	}
}
```

Add small test helpers `make32(t)` (32-byte secret) and reuse the package's existing `fakeUUIDs`/member-registry constructors (check `dispatch_test.go` / `members_test.go` for the real names and adapt).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/grpc/ -run TestDispatch_ -v`
Expected: FAIL — `NewProcessorDispatcher` arity / `resolveTxToken` undefined.

- [ ] **Step 3: Write minimal implementation**

Update the struct + constructor:

```go
type ProcessorDispatcher struct {
	registry   *MemberRegistry
	uuids      spi.UUIDGenerator
	signer     *token.Signer
	selfNodeID string
	tokenTTL   time.Duration
}

func NewProcessorDispatcher(registry *MemberRegistry, uuids spi.UUIDGenerator, signer *token.Signer, selfNodeID string, tokenTTL time.Duration) *ProcessorDispatcher {
	return &ProcessorDispatcher{registry: registry, uuids: uuids, signer: signer, selfNodeID: selfNodeID, tokenTTL: tokenTTL}
}

// resolveTxToken returns the tx-token to attach to a calc request. A token
// pre-minted by an upstream ClusterDispatcher (carried on ctx, NodeID = owner)
// wins so a forwarded dispatch routes callbacks to the owner, not this node.
// Otherwise self-mint {selfNodeID, txID}. Empty txID → no token (standalone).
func (d *ProcessorDispatcher) resolveTxToken(ctx context.Context, txID string) string {
	if tok := TxTokenFromContext(ctx); tok != "" {
		return tok
	}
	if txID == "" || d.signer == nil {
		return ""
	}
	tok, err := d.signer.Issue(d.selfNodeID, txID, time.Now().Add(d.tokenTTL))
	if err != nil {
		slog.Error("failed to mint tx-token", "pkg", "grpc", "err", err)
		return ""
	}
	return tok
}
```

Add `"time"`, `"context"`, and the `token` import if missing. In `DispatchProcessor`, immediately after `AttachAuthContext(ctx, ce)` (line ~98) add:

```go
	AttachTxToken(ce, d.resolveTxToken(ctx, txID))
```

Do the identical addition in `DispatchCriteria` after its `AttachAuthContext(ctx, ce)` (line ~248).

Update `NewProcessorDispatcher` call site in `app/app.go` to pass `a.tokenSigner, a.selfNodeID, cfg.Cluster.TxTokenTTL` (add a `TxTokenTTL` config with a sane default, e.g. `30s`, ≥ the dispatch response timeout; document in Task 15). Update any other constructor call sites (grep `NewProcessorDispatcher`).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/grpc/ -run TestDispatch_ -v && go build ./...`
Expected: PASS; build green.

- [ ] **Step 5: Commit**

```bash
go vet ./internal/grpc/ ./app/
git add internal/grpc/dispatch.go internal/grpc/dispatch_txtoken_test.go app/app.go
git commit -m "feat(grpc): mint+attach tx-token on processor/criteria dispatch"
```

---

## Task 5: ClusterDispatcher pre-mint + forward the owner token

**Files:**
- Modify: `internal/cluster/dispatch/cluster_dispatcher.go` (struct+ctor `:21-51`, `DispatchProcessor` `:53-98`, `DispatchCriteria` `:100-141`, `buildProcessorRequest`/`buildCriteriaRequest` `:203+`).
- Modify: `internal/cluster/dispatch/types.go:9-53` (add `TxToken` field to both request DTOs).
- Modify: `internal/cluster/dispatch/handler.go` (receiver: put `req.TxToken` into ctx before local dispatch).
- Test: `internal/cluster/dispatch/cluster_txtoken_test.go` (create)

**Interfaces:**
- Consumes: `token.Signer.Issue`; `grpcpkg.WithTxToken` (Task 3, import `internal/grpc`).
- Produces: `NewClusterDispatcher(..., signer *token.Signer, tokenTTL time.Duration)`. On dispatch with non-empty `txID`, mints `{selfNodeID, txID}` once; passes it to the local path via ctx and to the forward path via `DispatchProcessorRequest.TxToken`. The receiver node re-injects it into ctx so its local grpc dispatcher attaches the owner's token verbatim.

- [ ] **Step 1: Write the failing test**

```go
package dispatch

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
)

func TestBuildProcessorRequest_CarriesOwnerToken(t *testing.T) {
	signer, _ := token.NewSigner(make32(t))
	tok, _ := signer.Issue("node-A", "tx-9", time.Now().Add(time.Minute))
	d := &ClusterDispatcher{selfNodeID: "node-A", signer: signer, tokenTTL: time.Minute}
	req := d.buildProcessorRequest(sampleEntity(), sampleProc(), "wf", "tr", "tx-9", sampleUC(), "tag", tok)
	if req.TxToken != tok {
		t.Fatalf("expected owner token on forwarded request, got %q", req.TxToken)
	}
}
```

Reuse existing test fixtures in the package (`sampleEntity`, etc.) or inline minimal ones.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cluster/dispatch/ -run CarriesOwnerToken -v`
Expected: FAIL — `req.TxToken` undefined / `buildProcessorRequest` arity.

- [ ] **Step 3: Write minimal implementation**

In `types.go`, add to both `DispatchProcessorRequest` and `DispatchCriteriaRequest`:

```go
	TxToken string `json:"txToken,omitempty"`
```

Add `signer *token.Signer` and `tokenTTL time.Duration` fields to `ClusterDispatcher` + `NewClusterDispatcher` params. In `DispatchProcessor`, at the very top mint once and thread it:

```go
	tok := ""
	if txID != "" && d.signer != nil {
		if t, err := d.signer.Issue(d.selfNodeID, txID, time.Now().Add(d.tokenTTL)); err == nil {
			tok = t
		} else {
			slog.Error("failed to mint tx-token", "pkg", "dispatch", "err", err)
		}
	}
	ctx = grpcpkg.WithTxToken(ctx, tok) // local path attaches this
```

Change `buildProcessorRequest` to take `tok string` and set `TxToken: tok`. Do the mirror in `DispatchCriteria`/`buildCriteriaRequest`. Update the `app/app.go` `NewClusterDispatcher(...)` call to pass `a.tokenSigner, cfg.Cluster.TxTokenTTL`.

In `internal/cluster/dispatch/handler.go` (the receiver of the forwarded POST), after decoding `req` and before invoking the local dispatcher, inject the token so the local grpc dispatcher attaches the owner's token rather than self-minting:

```go
	ctx = grpcpkg.WithTxToken(ctx, req.TxToken)
```

(Locate the exact call site: where `handler.go` builds the `UserContext` ctx and calls `d.local.DispatchProcessor(ctx, ...)`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cluster/dispatch/ -run CarriesOwnerToken -v && go build ./...`
Expected: PASS; build green.

- [ ] **Step 5: Commit**

```bash
go vet ./internal/cluster/dispatch/ ./app/
git add internal/cluster/dispatch/ app/app.go
git commit -m "feat(cluster): pre-mint owner tx-token and thread it through local+forward dispatch"
```

---

## Task 6: `txjoin.JoinFromToken` inbound helper + error mapping

**Files:**
- Create: `internal/domain/txjoin/txjoin.go`, `internal/domain/txjoin/txjoin_test.go`

**Interfaces:**
- Consumes: `token.Signer.Verify`, `spi.TransactionManager.Join`, spi tx sentinels.
- Produces: `func JoinFromToken(ctx context.Context, signer *token.Signer, txMgr spi.TransactionManager, tok string) (context.Context, error)`. Empty `tok` → returns `ctx` unchanged, nil error (standalone downstream). Non-empty → Verify → `Join(ctx, claims.TxRef)` → joined ctx. Errors are mapped to `common.Operational` with the reused codes:
  - `token.ErrTokenExpired` → 410 `TRANSACTION_EXPIRED`
  - `token.ErrTokenTampered`/`ErrTokenInvalid` → 401 `UNAUTHORIZED`
  - `spi.ErrTxTenantMismatch` → 403 `FORBIDDEN`
  - `spi.ErrTxNotFound`/`ErrTxRolledBack`/`ErrTxAlreadyCommitted` → 404 `TRANSACTION_NOT_FOUND`

- [ ] **Step 1: Write the failing test**

```go
package txjoin

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type fakeTM struct{ joinErr error }

func (f fakeTM) Join(ctx context.Context, txID string) (context.Context, error) {
	if f.joinErr != nil {
		return nil, f.joinErr
	}
	return spi.WithTransaction(ctx, &spi.TransactionState{ID: txID}), nil
}

// ... embed a no-op impl of the rest of spi.TransactionManager ...

func TestJoinFromToken_EmptyPassThrough(t *testing.T) {
	ctx, err := JoinFromToken(context.Background(), nil, fakeTM{}, "")
	if err != nil || spi.GetTransaction(ctx) != nil {
		t.Fatalf("empty token must be a pass-through; err=%v", err)
	}
}

func TestJoinFromToken_JoinsValid(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("local", "tx-1", time.Now().Add(time.Minute))
	ctx, err := JoinFromToken(context.Background(), s, fakeTM{}, tok)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if tx := spi.GetTransaction(ctx); tx == nil || tx.ID != "tx-1" {
		t.Fatalf("expected joined tx tx-1, got %+v", tx)
	}
}

func TestJoinFromToken_NotFoundMaps404(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("local", "tx-x", time.Now().Add(time.Minute))
	_, err := JoinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxNotFound}, tok)
	var op *common.OperationalError // adjust to the real type returned by common.Operational
	if !errors.As(err, &op) || op.HTTPStatus() != http.StatusNotFound || op.Code() != common.ErrCodeTransactionNotFound {
		t.Fatalf("expected 404 TRANSACTION_NOT_FOUND, got %v", err)
	}
}

func TestJoinFromToken_TenantMaps403(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("local", "tx-x", time.Now().Add(time.Minute))
	_, err := JoinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxTenantMismatch}, tok)
	var op *common.OperationalError
	if !errors.As(err, &op) || op.HTTPStatus() != http.StatusForbidden {
		t.Fatalf("expected 403 FORBIDDEN, got %v", err)
	}
}
```

Adjust `common.OperationalError`/`HTTPStatus()`/`Code()` to the real accessors (inspect `internal/common/errors.go`; `common.Operational(status, code, msg)` is already used across `service.go`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/txjoin/ -v`
Expected: FAIL — `undefined: JoinFromToken`.

- [ ] **Step 3: Write minimal implementation**

```go
// Package txjoin turns an inbound transaction routing token into a joined
// transaction context, mapping verify/join failures to reusable operational
// error codes. Transport-agnostic: used by both the gRPC interceptor and the
// HTTP middleware.
package txjoin

import (
	"context"
	"errors"
	"net/http"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func JoinFromToken(ctx context.Context, signer *token.Signer, txMgr spi.TransactionManager, tok string) (context.Context, error) {
	if tok == "" {
		return ctx, nil
	}
	claims, err := signer.Verify(tok)
	if err != nil {
		switch {
		case errors.Is(err, token.ErrTokenExpired):
			return ctx, common.Operational(http.StatusGone, common.ErrCodeTransactionExpired, "transaction token has expired")
		default: // tampered / invalid
			return ctx, common.Operational(http.StatusUnauthorized, common.ErrCodeUnauthorized, "invalid transaction token")
		}
	}
	joined, err := txMgr.Join(ctx, claims.TxRef)
	if err != nil {
		switch {
		case errors.Is(err, spi.ErrTxTenantMismatch):
			return ctx, common.Operational(http.StatusForbidden, common.ErrCodeForbidden, "transaction belongs to a different tenant")
		default: // ErrTxNotFound / ErrTxRolledBack / ErrTxAlreadyCommitted
			return ctx, common.Operational(http.StatusNotFound, common.ErrCodeTransactionNotFound, "transaction not found or no longer active")
		}
	}
	return joined, nil
}
```

Confirm the exact `common.ErrCode*` constant names (`ErrCodeTransactionExpired`, `ErrCodeTransactionNotFound`, `ErrCodeUnauthorized`, `ErrCodeForbidden`) in `internal/common/error_codes.go`; adjust if different.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/domain/txjoin/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go build ./... && go vet ./internal/domain/txjoin/
git add internal/domain/txjoin/
git commit -m "feat(txjoin): token->Join helper with reused tx error-code mapping"
```

---

## Task 7: Shared-service participate-not-Begin + owner gate

**Files:**
- Modify: `internal/domain/entity/handler.go:84-93` (add `gate *txgate.Registry`, constructor).
- Modify: `internal/domain/entity/service.go` — Begin sites (259,592,716,927,1069,1390), Commit sites (326,621,758,1018,1261,1579), matching Rollback sites.
- Test: `internal/domain/entity/service_txjoin_test.go` (create; memory backend).

**Interfaces:**
- Consumes: `spi.GetTransaction`, `txgate.Registry` (Task 1).
- Produces: `New(factory, txMgr, uuids, engine, gate)`. New helpers on `Handler`:
  - `func (h *Handler) beginOrJoin(ctx) (txID string, txCtx context.Context, owned bool, err error)` — if `spi.GetTransaction(ctx) != nil`, return its `ID`, ctx, `owned=false`, nil; else `Begin` (owned=true).
  - `func (h *Handler) commitOwned(ctx, txID string, owned bool) error` — `Commit` iff owned; else nil.
  - `func (h *Handler) rollbackOwned(ctx, txID string, owned bool)` — `Rollback` iff owned; else nil.
  - Owner commit is wrapped by `h.gate.Acquire(txID)` (released after commit). The gate is acquired **only around the final Save+Commit**, never around `engine.Execute` (deadlock invariant).

- [ ] **Step 1: Write the failing test**

```go
package entity

import (
	"context"
	"testing"
	// memory plugin + test harness imports used elsewhere in this package
)

// When ctx already carries a joined tx, CreateEntity must NOT open a new tx and
// must NOT commit — the write stays in the joined tx's buffer for the owner to
// commit.
func TestCreateEntity_ParticipatesInJoinedTx(t *testing.T) {
	h, txMgr := newTestHandlerMemory(t) // helper: wires memory factory+txMgr+engine+gate
	ownerTxID, ownerCtx, _ := txMgr.Begin(userCtx(t))

	// Simulate a callback joining the owner tx.
	joinedCtx, err := txMgr.Join(userCtx(t), ownerTxID)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	res, err := h.CreateEntity(joinedCtx, sampleCreateInput(t))
	if err != nil {
		t.Fatalf("create in joined tx: %v", err)
	}
	if res.TransactionID != ownerTxID {
		t.Fatalf("participating write should report owner txID %q, got %q", ownerTxID, res.TransactionID)
	}
	// Before the owner commits, the entity is NOT visible outside the tx.
	if visibleOutsideTx(t, h, res.EntityIDs[0]) {
		t.Fatal("joined-callback write leaked before owner commit")
	}
	// Owner commits ownerCtx -> now visible.
	if err := txMgr.Commit(ownerCtx, ownerTxID); err != nil {
		t.Fatalf("owner commit: %v", err)
	}
	if !visibleOutsideTx(t, h, res.EntityIDs[0]) {
		t.Fatal("write not visible after owner commit")
	}
}
```

Write `newTestHandlerMemory`, `userCtx`, `sampleCreateInput`, `visibleOutsideTx` using the memory plugin (mirror existing `service_test.go` harness in this package).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/entity/ -run ParticipatesInJoinedTx -v`
Expected: FAIL — `New` arity / participation not implemented (currently `CreateEntity` always Begins+Commits, so the write commits independently and `visibleOutsideTx` is true before owner commit).

- [ ] **Step 3: Write minimal implementation**

Add helpers to `handler.go`:

```go
type Handler struct {
	factory spi.StoreFactory
	txMgr   spi.TransactionManager
	uuids   spi.UUIDGenerator
	engine  *wfengine.Engine
	gate    *txgate.Registry
}

func New(factory spi.StoreFactory, txMgr spi.TransactionManager, uuids spi.UUIDGenerator, engine *wfengine.Engine, gate *txgate.Registry) *Handler {
	return &Handler{factory: factory, txMgr: txMgr, uuids: uuids, engine: engine, gate: gate}
}

func (h *Handler) beginOrJoin(ctx context.Context) (string, context.Context, bool, error) {
	if tx := spi.GetTransaction(ctx); tx != nil {
		return tx.ID, ctx, false, nil
	}
	txID, txCtx, err := h.txMgr.Begin(ctx)
	return txID, txCtx, true, err
}

func (h *Handler) commitOwned(ctx context.Context, txID string, owned bool) error {
	if !owned {
		return nil // joined callback: owner commits
	}
	release := h.gate.Acquire(txID)
	defer release()
	return h.txMgr.Commit(ctx, txID)
}

func (h *Handler) rollbackOwned(ctx context.Context, txID string, owned bool) {
	if !owned {
		return
	}
	_ = h.txMgr.Rollback(ctx, txID)
}
```

Then, per flow (CreateEntity shown; apply the same transformation to UpdateEntity, DeleteEntity, DeleteAllEntities, CreateEntities, UpdateCollection):
- Replace `txID, txCtx, err := h.txMgr.Begin(ctx)` with `txID, txCtx, owned, err := h.beginOrJoin(ctx)`.
- Replace every `h.txMgr.Rollback(<ctx>, <txID>)` on that flow with `h.rollbackOwned(<ctx>, <txID>, owned)`.
- Replace the final `h.txMgr.Commit(<finalCtx>, <finalTxID>)` with `h.commitOwned(<finalCtx>, <finalTxID>, owned)`.

For CBD-segmenting flows (Create/Update), the engine may advance `finalTxID` past the entry `txID`. When `owned==false` (joined callback), the callback path must not itself be running a segmenting cascade that commits — a callback is a plain single-segment entity op, so `finalTxID==txID` for joined calls; assert this holds (add `if !owned && finalTxID != txID { return internal error }` guard to make the invariant explicit).

Also wrap the joined-callback body with the gate so concurrent callbacks on one tx serialise. At the top of each service method, after `beginOrJoin`, when `!owned`:

```go
	if !owned {
		release := h.gate.Acquire(txID)
		defer release()
	}
```

Update `entity.New(...)` call site in `app/app.go` to pass the shared `a.txGate` (construct `a.txGate = txgate.New()` once in app build).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/domain/entity/ -run ParticipatesInJoinedTx -v && go test ./internal/domain/entity/ -count=1`
Expected: PASS; no regressions in the package.

- [ ] **Step 5: Commit**

```bash
go build ./... && go vet ./internal/domain/entity/ ./app/
git add internal/domain/entity/ app/app.go
git commit -m "feat(entity): participate in joined tx (no Begin/Commit); gate owner commit"
```

---

## Task 8: gRPC inbound interceptor — route + Join + B→A forward

**Files:**
- Create: `internal/grpc/txroute_interceptor.go`, `internal/grpc/txroute_interceptor_test.go`
- Create: `internal/cluster/proxy/grpc_forward.go`
- Modify: `internal/grpc/service.go` (`CloudEventsServiceImpl` carries signer/registry/selfNodeID/txMgr for the interceptor), `app/app.go` (register interceptor when serving `EntityManage`).

**Interfaces:**
- Consumes: `proxy.ExtractGRPCToken`, `proxy.ResolveTarget` (`internal/cluster/proxy/grpc.go`), `txjoin.JoinFromToken`.
- Produces: a unary+stream interceptor that, for `EntityManage`/`EntityManageCollection` RPCs: `tok := ExtractGRPCToken(ctx)`; `addr, shouldProxy, err := ResolveTarget(...)`; if `shouldProxy` → forward the call to `addr` via `proxy.ForwardEntityManage(...)` and return its response; else `ctx, err = txjoin.JoinFromToken(...)` and proceed. Errors become gRPC `status` errors carrying the operational code.

- [ ] **Step 1: Write the failing test**

```go
package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"google.golang.org/grpc/metadata"
)

// A valid self-node token results in a joined ctx handed to the handler.
func TestTxRouteInterceptor_LocalJoin(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("local", "tx-1", time.Now().Add(time.Minute))
	ic := newTxRouteInterceptor(s, fakeRegistry{}, "local", fakeJoinTM{})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))

	var sawTx string
	handler := func(ctx context.Context, req any) (any, error) {
		if tx := getJoined(ctx); tx != nil {
			sawTx = tx.ID
		}
		return "ok", nil
	}
	_, err := ic.unary()(ctx, nil, entityManageInfo(), handler)
	if err != nil || sawTx != "tx-1" {
		t.Fatalf("expected local join tx-1, sawTx=%q err=%v", sawTx, err)
	}
}
```

Add a second test `TestTxRouteInterceptor_ForeignProxies` asserting that a token for a different, alive node triggers `ForwardEntityManage` (use a fake forwarder recording the target addr).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/grpc/ -run TxRouteInterceptor -v`
Expected: FAIL — `newTxRouteInterceptor` undefined.

- [ ] **Step 3: Write minimal implementation**

`internal/cluster/proxy/grpc_forward.go` — a thin gRPC client that redials the owner and replays the unary `EntityManage`:

```go
package proxy

import (
	"context"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// ForwardEntityManage dials the owning node and replays a unary EntityManage
// call, propagating auth + tx-token metadata. Connections are cached per addr.
func ForwardEntityManage(ctx context.Context, pool *ClientPool, addr string, ce *cepb.CloudEvent) (*cepb.CloudEvent, error) {
	conn, err := pool.Get(addr)
	if err != nil {
		return nil, err
	}
	client := cyodapb.NewCloudEventsServiceClient(conn)
	// Preserve incoming metadata (auth + tx-token) onto the outgoing call.
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		ctx = metadata.NewOutgoingContext(ctx, md)
	}
	return client.EntityManage(ctx, ce)
}
```

Add a small `ClientPool` (map addr→*grpc.ClientConn guarded by a mutex; `insecure` creds for intra-cluster, matching the existing HTTP forwarder's transport posture — confirm whether cluster peer comms use TLS and match it). Then the interceptor in `internal/grpc/txroute_interceptor.go`:

```go
package grpc

import (
	"context"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	spi "github.com/cyoda-platform/cyoda-go-spi"
	"google.golang.org/grpc"
)

type txRouteInterceptor struct {
	signer     *token.Signer
	registry   contract.NodeRegistry
	selfNodeID string
	txMgr      spi.TransactionManager
	pool       *proxy.ClientPool
}

func newTxRouteInterceptor(signer *token.Signer, reg contract.NodeRegistry, selfNodeID string, txMgr spi.TransactionManager) *txRouteInterceptor {
	return &txRouteInterceptor{signer: signer, registry: reg, selfNodeID: selfNodeID, txMgr: txMgr, pool: proxy.NewClientPool()}
}

func (i *txRouteInterceptor) unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !isEntityManage(info.FullMethod) {
			return handler(ctx, req)
		}
		tok := proxy.ExtractGRPCToken(ctx)
		addr, shouldProxy, err := proxy.ResolveTarget(ctx, i.signer, i.registry, i.selfNodeID, tok)
		if err != nil {
			return nil, common.ToGRPCStatus(err) // maps operational code -> status
		}
		if shouldProxy {
			ce, ok := req.(*cepb.CloudEvent)
			if !ok {
				return handler(ctx, req)
			}
			return proxy.ForwardEntityManage(ctx, i.pool, addr, ce)
		}
		joinedCtx, jerr := txjoin.JoinFromToken(ctx, i.signer, i.txMgr, tok)
		if jerr != nil {
			return nil, common.ToGRPCStatus(jerr)
		}
		return handler(joinedCtx, req)
	}
}
```

Add the stream interceptor variant for `EntityManageCollection` (wrap `grpc.ServerStream` with a context override; proxying a stream re-issues the server-streaming call and copies frames). Provide `common.ToGRPCStatus(err)` if not present (map `common.Operational` HTTP status/code → `codes.*` + message; check `internal/grpc/errors.go` for an existing mapper like `entityTransactionError` to reuse). Register `i.unary()`/`i.stream()` on the gRPC server in `app/app.go` (always — single-node uses the self-token local-join path).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/grpc/ -run TxRouteInterceptor -v && go build ./...`
Expected: PASS; build green.

- [ ] **Step 5: Commit**

```bash
go vet ./internal/grpc/ ./internal/cluster/proxy/ ./app/
git add internal/grpc/txroute_interceptor.go internal/grpc/txroute_interceptor_test.go internal/cluster/proxy/grpc_forward.go internal/grpc/service.go app/app.go
git commit -m "feat(grpc): tx-token route interceptor (local Join or B->A EntityManage forward)"
```

---

## Task 9: HTTP TxJoin middleware (always-on) + align proxy token errors

**Files:**
- Create: `internal/httpmw/txjoin_mw.go`, `internal/httpmw/txjoin_mw_test.go`
- Modify: `internal/cluster/proxy/http.go:80-101` (`handleTokenError` — tampered/invalid → `UNAUTHORIZED` 401).
- Modify: `app/app.go` (install TxJoin middleware on the entity routes, inside the proxy layer, always).

**Interfaces:**
- Consumes: `txjoin.JoinFromToken`, `proxy.TxTokenHeader`.
- Produces: `func TxJoin(signer *token.Signer, txMgr spi.TransactionManager) func(http.Handler) http.Handler`. Reads `X-Tx-Token`; empty → passthrough; else `JoinFromToken` → replace `r` with the joined-ctx request; on error → `common.WriteError`. Runs on the owner node (cluster: after HTTPRouting has already proxied foreign-owner requests here; single-node: the only node).

- [ ] **Step 1: Write the failing test**

```go
package httpmw

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestTxJoin_JoinsAndPassesCtx(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("local", "tx-1", time.Now().Add(time.Minute))
	var sawTx string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tx := spi.GetTransaction(r.Context()); tx != nil {
			sawTx = tx.ID
		}
		w.WriteHeader(200)
	})
	h := TxJoin(s, fakeJoinTM{})(next)
	req := httptest.NewRequest("POST", "/entity", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	req = withUserCtx(req)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if sawTx != "tx-1" {
		t.Fatalf("expected joined tx-1, got %q", sawTx)
	}
}

func TestTxJoin_NotFoundReturns404(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue("local", "tx-x", time.Now().Add(time.Minute))
	h := TxJoin(s, fakeJoinTM{joinErr: spi.ErrTxNotFound})(okHandler())
	req := httptest.NewRequest("POST", "/entity", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	req = withUserCtx(req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/httpmw/ -run TxJoin -v`
Expected: FAIL — `undefined: TxJoin`.

- [ ] **Step 3: Write minimal implementation**

```go
package httpmw

import (
	"net/http"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TxJoin(signer *token.Signer, txMgr spi.TransactionManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := r.Header.Get(proxy.TxTokenHeader)
			if tok == "" {
				next.ServeHTTP(w, r)
				return
			}
			ctx, err := txjoin.JoinFromToken(r.Context(), signer, txMgr, tok)
			if err != nil {
				common.WriteError(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
```

Update `handleTokenError` in `proxy/http.go` so `ErrTokenTampered`/`ErrTokenInvalid` map to 401 `UNAUTHORIZED` (align with the spec error table); update its existing test. Install `httpmw.TxJoin(a.tokenSigner, a.txMgr)` in `app/app.go` around the entity handlers (inner to the CORS/proxy layers, always installed).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/httpmw/ -run TxJoin -v && go test ./internal/cluster/proxy/ -count=1`
Expected: PASS; proxy tests green after the `handleTokenError` update.

- [ ] **Step 5: Commit**

```bash
go build ./... && go vet ./internal/httpmw/ ./internal/cluster/proxy/ ./app/
git add internal/httpmw/ internal/cluster/proxy/http.go app/app.go
git commit -m "feat(http): always-on tx-token join middleware; align proxy token errors to 401"
```

---

## Task 10: e2e — SYNC callback atomicity + read-your-writes (postgres)

**Files:**
- Create: `internal/e2e/callback_txjoin_test.go`
- Modify: `internal/e2e/` test harness — a callback-capable in-process compute member (extend the existing localproc harness noted in the skipped `TestWorkflowProc_UpdateWithCBD_TrueBranch_SecondaryEntityWritten`).

**Interfaces:**
- Consumes: the full HTTP+gRPC stack from `TestMain` (real Postgres). A test compute member that, on a processor calc request, reads the `cyodatxtoken` attribute and issues a secondary-entity callback presenting it as `tx-token` metadata, then replies success.

- [ ] **Step 1: Write the failing tests**

Two tests:
1. `TestCallback_SyncWrite_AtomicWithTransition` — a transition runs a SYNC processor whose callback **creates** a secondary entity. Assert: after the primary transition **succeeds**, the secondary entity exists and shares the primary's txID lineage. Then a variant where the **processor fails after the callback**: assert the secondary entity does **not** exist (rolled back with T).
2. `TestCallback_SyncRead_SeesUncommittedCascadeWrite` — the processor's callback **reads** the primary entity mid-cascade and observes the uncommitted in-flight state (echoed back in the processor response / asserted via a marker the processor writes).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/e2e/ -run TestCallback_Sync -v`
Expected: FAIL — before wiring, the callback opened its own tx (secondary write survived a rolled-back primary; read saw committed state only).

- [ ] **Step 3: Implement**

No new production code beyond Tasks 1-9; this task delivers the **harness** (callback-capable compute member) and proves the behavior end-to-end. If a gap surfaces (e.g. token not reaching the in-process member), fix in the owning task's file and note it.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/e2e/ -run TestCallback_Sync -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/
git commit -m "test(e2e): SYNC callback atomicity + read-your-writes over full stack"
```

---

## Task 11: e2e — mode matrix (ASYNC_NEW_TX / CBD-post / CBD-default)

**Files:**
- Create: `internal/e2e/callback_txjoin_modes_test.go`

- [ ] **Step 1: Write the failing tests**
1. `TestCallback_AsyncNewTx_DiscardedOnProcessorFailure` — ASYNC_NEW_TX processor whose callback creates a secondary entity, then the processor **fails**. Assert: secondary entity is **discarded** (savepoint rollback), the **pipeline continues**, and the **primary transition commits** without the secondary. (Provisional-ack semantics.)
2. `TestCallback_AsyncNewTx_KeptOnSuccess` — same but processor succeeds → secondary persists.
3. `TestCallback_CBDPost_JoinsTxPost` — CBD `startNewTxOnDispatch=true`; callback write commits atomically with `TX_post`.
4. `TestCallback_CBDDefault_RunsStandalone` — CBD default (no open tx during dispatch); the callback presents **no** token → runs standalone and commits independently (regression guard that empty-txID → Begin).

- [ ] **Step 2: Run to verify fail** — `go test ./internal/e2e/ -run TestCallback_Async -run TestCallback_CBD -v` (before Tasks 1-9 land, or by asserting the new semantics).
- [ ] **Step 3: Implement** — harness scenarios only; production behavior from Tasks 1-9.
- [ ] **Step 4: Run to verify pass.**
- [ ] **Step 5: Commit**

```bash
git add internal/e2e/callback_txjoin_modes_test.go
git commit -m "test(e2e): callback join across ASYNC_NEW_TX / CBD-post / CBD-default modes"
```

---

## Task 12: e2e + gRPC — loud-fail error codes

**Files:**
- Create: `internal/e2e/callback_txjoin_errors_test.go` (HTTP), and gRPC cases in `internal/grpc/` (envelope assertions).

- [ ] **Step 1: Write the failing tests** (one per row of the spec error table, both HTTP and gRPC):
  - non-empty token, unknown/closed txID → 404 `TRANSACTION_NOT_FOUND`.
  - expired token → 410 `TRANSACTION_EXPIRED`.
  - forged/bad-HMAC token → 401 `UNAUTHORIZED`.
  - cross-tenant token → 403 `FORBIDDEN`.
  - empty token → 2xx standalone (control).
  For gRPC assert the `Success=false` + `Error.Code` envelope; for HTTP assert status + JSON error code.
- [ ] **Step 2: Run to verify fail.**
- [ ] **Step 3: Implement** — assertions only.
- [ ] **Step 4: Run to verify pass.**
- [ ] **Step 5: Commit**

```bash
git add internal/e2e/callback_txjoin_errors_test.go internal/grpc/
git commit -m "test(e2e,grpc): loud-fail codes for unjoinable/expired/forged/cross-tenant callback tokens"
```

---

## Task 13: Cross-backend parity scenarios

**Files:**
- Create: `e2e/parity/callback_txjoin.go`
- Modify: `e2e/parity/registry.go` (register the scenario)

- [ ] **Step 1: Write the failing parity scenario** — backend-agnostic behaviors across memory/sqlite/postgres(+commercial): (a) SYNC callback write is atomic with the transition; (b) SYNC callback read sees the uncommitted cascade write; (c) criteria callback read-your-writes joins T; (d) callback `Update` with `If-Match` against an in-T uncommitted version succeeds; (e) empty-token callback runs standalone. Register in `registry.go`.
- [ ] **Step 2: Run to verify fail** — `go test ./e2e/parity/... -run CallbackTxJoin -v`.
- [ ] **Step 3: Implement** — scenario code; behavior from Tasks 1-9. Keep concurrency OUT of parity (Task 14).
- [ ] **Step 4: Run to verify pass** across all registered backends.
- [ ] **Step 5: Commit**

```bash
git add e2e/parity/callback_txjoin.go e2e/parity/registry.go
git commit -m "test(parity): compute-node callback join across all backends"
```

---

## Task 14: Multi-node + concurrency isolated tests

**Files:**
- Create: `e2e/parity/multinode/callback_route.go` (or `internal/e2e/callback_multinode_test.go` following the two-App pattern from `e2e/parity/multinode/cbd_tx_pinning.go`).

- [ ] **Step 1: Write the failing tests** (isolated, not in the shared parity suite):
  1. `TestCallback_ForwardedDispatch_TokenRoutesToOwner_HTTP` — two Apps (cluster on, shared HMAC secret); processor with tags only a peer advertises → dispatch forwards A→B; the compute node on B fires an **HTTP** callback with `X-Tx-Token` (owner=A); assert it proxies to A, joins A's tx, and the secondary entity is atomic with A's transition.
  2. `TestCallback_ForwardedDispatch_TokenRoutesToOwner_GRPC` — same via a **gRPC** callback landing on B → B→A `EntityManage` forward.
  3. `TestCallback_OwnerNodeDown_503` — owner unreachable → `TRANSACTION_NODE_UNAVAILABLE`.
  4. `TestCallback_ConcurrentCallbacks_Serialise` — one processor fires N parallel callbacks on the same tx; assert no torn write / no "concurrent map writes" fatal and a consistent final buffer (the txgate at work).
  5. `TestDispatch_NeverHoldsGateAcrossDispatch` — unit/integration asserting the owner does not hold `txgate` while `engine.Execute` blocks (e.g. a callback can Join+Save while a SYNC processor is mid-dispatch). This encodes the H3 deadlock invariant.
- [ ] **Step 2: Run to verify fail.**
- [ ] **Step 3: Implement** — harness/assertions; production behavior from Tasks 1-9.
- [ ] **Step 4: Run to verify pass.**
- [ ] **Step 5: Commit**

```bash
git add e2e/parity/multinode/callback_route.go
git commit -m "test(multinode): forwarded-dispatch callback routing (HTTP+gRPC), node-down, concurrency, gate invariant"
```

---

## Task 15: Gate-4 documentation

**Files:**
- Modify: `cmd/cyoda/help/content/errors/{TX_COORDINATOR_NOT_CONFIGURED,TX_NO_STATE,TX_REQUIRED,TX_CONFLICT}.md`
- Modify: `docs/PROCESSOR_EXECUTION_MODES.md`, `docs/ARCHITECTURE.md`, `docs/PRD.md`
- Create: `.claude/rules/multi-node-primary.md`
- Modify: cluster config help topic (`cmd/cyoda/help/content/config/*.md`), `DefaultConfig()` comment for `Cluster.Enabled`/`TxTokenTTL`, `COMPATIBILITY.md`, `CHANGELOG.md`
- Modify: `cmd/compute-test-client/*` (echo `tx-token`)

- [ ] **Step 1: Phantom-code correction.** Rewrite the four `errors/*.md` topics so they describe the real model (request-scoped tx today; tx-token-routed live transactions as the design) and stop asserting a distributed 2PC coordinator. Keep the codes (parity intact). Run `go test ./... -run TestErrCode_Parity -v` — expected PASS (no code added/removed).
- [ ] **Step 2: Contract docs.** Update `PROCESSOR_EXECUTION_MODES.md` "Transaction-bound callbacks" + the relevant `ARCHITECTURE.md`/`PRD.md` sections to the implemented mechanism (mint-on-owner, echo, Join-not-Begin, provisional ack, ASYNC_NEW_TX savepoint scoping). Keep prose compact.
- [ ] **Step 3: Multi-node-primary note.** Create `.claude/rules/multi-node-primary.md`:

```markdown
# cyoda-go is primarily multi-node

Cluster mode is the primary operating target. `CYODA_CLUSTER_ENABLED=false` is
the default only to make getting started easy — it is NOT a signal that
cluster/HA features are secondary or descopable. Do not defer or descope
multi-node correctness (proxy routing, tx-affinity, cross-node callback join,
peer failover) on proportionality grounds. Design cross-node correctness in
from the start; reviewers must not treat single-node as "the common case".
```

Add a one-line pointer in `docs/ARCHITECTURE.md` and a comment at the `Cluster.Enabled` default in `DefaultConfig()` + the cluster config help topic.
- [ ] **Step 4: TxTokenTTL config docs.** Document the new `CYODA_CLUSTER_TX_TOKEN_TTL` (default `30s`, ≥ dispatch response timeout) in the config help topic, `README.md`, and `DefaultConfig()` together (Gate 4). SDK: document the callback echo requirement; extend `cmd/compute-test-client` to echo `tx-token`.
- [ ] **Step 5: COMPATIBILITY + CHANGELOG + commit.**

```bash
go test ./... -run TestErrCode_Parity -v
git add cmd/ docs/ .claude/rules/multi-node-primary.md COMPATIBILITY.md CHANGELOG.md README.md app/
git commit -m "docs: correct phantom 2PC-coordinator topics; document tx-token callback contract + multi-node-primary"
```

---

## Task 16: Full verification

- [ ] **Step 1:** `go build ./... && go vet ./...`
- [ ] **Step 2:** `go test ./... -v` (root incl. e2e — Docker required). Expected: green.
- [ ] **Step 3:** `make test-all` (root + memory/sqlite/postgres plugins). Expected: green.
- [ ] **Step 4:** `make race` (CI-parity scope) + `go test -race -timeout=20m ./internal/e2e/...`. Expected: no data races (validates the txgate + join concurrency).
- [ ] **Step 5:** Commit any fixes; then hand off to `superpowers:verification-before-completion` → `requesting-code-review` → `security-review`.

---

## Self-Review — spec coverage

- Signed tx-token handle distinct from `transactionId`: Tasks 2-6. Mint-on-owner-before-forward (H1): Task 5. Local Join / proxy routing: Tasks 6/8/9. gRPC forward transport (H5): Task 8. Shared-service Join-not-Begin + empty-vs-unjoinable rule: Tasks 6/7/12. Per-tx gate + owner-drains-commit (H2) + never-hold-across-dispatch (H3): Tasks 1/7/14. ASYNC_NEW_TX savepoint scoping + provisional ack (H4/H6): Task 11. Criteria callbacks (H7): Tasks 5/13. Collection streaming (H8): Task 7 (per-item participates) + coverage in 10-13. If-Match in joined tx (H9): Task 13. Redaction (H10): tokens never logged — verified in Tasks 3/4 (attribute, not payload preview) and Task 15 SDK docs; add an explicit assertion in Task 12 that no token appears in error responses. Error table (all rows, HTTP+gRPC): Task 12. Coverage matrix U/E/P/G/MN: Tasks 1-9 (U/G), 10-12 (E/G), 13 (P), 14 (MN). Gate-4 docs incl. phantom-code + multi-node-primary + TxTokenTTL: Task 15. No new error codes; `TestErrCode_Parity` guarded in Task 15.
