# Stream P — the hand-over between pnodes (`internal/cluster/dispatch`)

Spec: §6 in full, D9 and D12 (§2), §15 (mixed versions), the hand-over counter of
§12, and the U layer of the hand-over rows of §13.

## What the code says today (read, not assumed)

- `types.go:17-47, 54-85` — `DispatchCalloutRequest` carries `TxToken`; the
  response is `Success`/`Error` plus the `ErrorCode/ErrorStatus/ErrorRetryable`
  trio and the result union. `Warnings` exists on the response but **the handler
  never fills it** (`handler.go` installs no diagnostics), so a cnode's warnings
  are lost across pnodes today.
- `handler.go:31, 88, 99, 115` — the handler holds a
  `contract.ExternalProcessingService` and calls its three `Dispatch*` methods.
  In production that value is the local `*ProcessorDispatcher`
  (`app/app.go:785, 798`), never the `ClusterDispatcher`, so "never hands on"
  holds by wiring only. `handler.go:75` re-injects `req.TxToken`.
  `handler.go:51, 70, 127` answer a bare 400; `:142` a bare 403 for every
  `Verify` error, a replayed nonce and a full cache included
  (`aead_peer_auth.go:186-188`, `nonce_cache.go:40-58`).
- `forwarder.go:31-43` — one shared `http.Client` with `Timeout` =
  `CYODA_DISPATCH_FORWARD_TIMEOUT` and kept-alive connections
  (`MaxIdleConnsPerHost: 5`). `:58` validates the address, `:82` marshals, `:99`
  signs — the three failures that can be proved before connecting. `:117` decodes
  a **plaintext** JSON response.
- `aead_peer_auth.go:196-204` — associated data is `method\npath\nts`; no
  direction label. `Sign`/`Verify` are also what `internal/cluster/scheduler_rpc.go:190, 255`
  use, and one e2e helper signs by hand
  (`internal/e2e/tx_lifecycle_e2e_test.go:636`).
- `cluster_dispatcher.go:74, 122, 167, 209-219` — pre-mints a pass and puts it on
  ctx; `:242-283` `forwardWithFailover`; `:288-310` `findPeerWithPolling`;
  `:427-437` `remintPeerError`; `:447-459` `extractCriteriaTags` (a second parser
  of the criterion envelope).
- `selector.go:10-12` — `PeerSelector.Select` picks **one** of a slice; there is
  no ordering call. `PeerRouter.Peers` gets "selector order" by selecting
  repeatedly from what is left.
- `internal/grpc` after stream L: a `Callout` is built only by
  `NewProcessorCallout / NewCriteriaCallout / NewFunctionCallout`, whose request
  builder and response mapper are **closures over the entity and the definition**
  (L-6). Its exported fields do not include the entity, the processor
  definition, the criterion JSON, the function, or the workflow and transition
  names. `HandOver(ctx, peer, call, …)` therefore cannot put a callout on the
  wire from a `Callout` as L leaves it — Task P-3 adds `Callout.Source`.

**V-3 (metric naming), settled for the hand-over counter.** Dotted lower case
under `cyoda.<area>` (`internal/observability/dispatch_tracing.go:41-47`); the
existing dispatch instruments are `cyoda.dispatch.duration` and
`cyoda.dispatch.count`. A package that owns an instrument takes a `metric.Meter`
in its constructor and returns the instrument error, which `app.go` treats as a
startup failure (`internal/auth/reconcile_metrics.go:33-43`); `app.go` passes
`observability.Meter()`; tests read through `sdkmetric.NewManualReader()`
(`internal/observability/tx_tracing_test.go:103`). The counter is
`cyoda.dispatch.handovers`, attribute `outcome`.

## Order, and what each task needs from other streams

| Task | Needs |
|---|---|
| P-1, P-2 | nothing — can start at once |
| P-3 | L-6 (`Callout`, the three builders) |
| P-4 | P-3; L-1 (`contract.CalloutFailure`…), L-8 (`LocalResult`, `NewMinorNumberer`), L-10 (`Callout.Outer`); F (`token.Pair`) |
| P-5 | P-4; M-5 (`contract.NodeRegistry.Changed()`) |
| P-6 | P-2, P-5; L-10 landed (every try mints its own pass); C-2 (`cfg.Callout.HandoverAllowance`) |
| P-7 | P-6; C-2 (`cfg.Cluster.DispatchConnectTimeout`) |
| P-8 | P-7 |

P-6 is the one task in which the wire format changes; it carries the mechanical
adaptation that keeps `ClusterDispatcher` working. After P-6 nothing outside
`internal/grpc` uses `WithTxToken` / `TxTokenFromContext`, which unblocks L-11,
and `app.go:554` no longer reads `cfg.Cluster.TxTokenTTL` (C-11 then has
`app.go:441` left, which L-10 removes).

`Signer.Issue` changes signature in stream F. Its caller at
`cluster_dispatcher.go:213` is deleted by P-6. If F lands first it adapts that
call; if P-6 lands first F has one caller fewer. Both orders compile.

## Coverage — §13 rows of this stream (U layer)

| Row | Task · test |
|---|---|
| A pnode that receives a hand-over never hands on | P-6 `TestHandler_NoLocalCnode_AnswersNoHandOff` + the type: the handler holds a `LocalRunner` (one method, `RunLocal`) and no router |
| Hand-over answer lost → one try counted; … 503 `DISPATCH_FORWARD_FAILED` | P-4 `TestLostAnswer`; P-5 `TestHandOver_Classification` (rows *lost*); P-7 `TestHandOver_WaitRunsOut_IsOneTry`. The "not repeat-safe → the operation fails with it" half is the owner's loop (O) |
| `triesUsed` out of range → `no_answer` | P-4 `TestReadAnswer_TriesUsedOutOfRange` |
| Peer cannot be connected to → no try used | P-5 `TestHandOver_PeerRefusesConnection_NoTryUsed` (a real closed port). "next peer" is O's. M: waived in the spec (w³) |
| Non-2xx / truncated / unauthenticated answer → `no_answer` | P-5 `TestHandOver_OverTheWire_BadAnswersAreNoAnswer` (403, 500, 502, truncated, plaintext, sealed for another request, no `outcome`) |
| cnode message and verdict survive the hand-over | P-4 `TestResponseFromLocal_MemberFailed` + `TestReadAnswer_MemberFailed`; P-6 `TestHandOver_ThroughTheHandler_MemberMessageAndVerdictSurvive`. M is O's |
| Response protection: forged, reflected or replayed answer refused; nonce never reused | P-1 `aead_response_test.go` (all of it); P-2 forwarder tests |
| Hand-over: peer makes two tries in one exchange; honours the owner's answer limit | P-6 `TestHandler_GivesRunLocalTheOwnersTriesAndAnswerLimit`; P-4 `TestReadAnswer_TwoTriesInOneExchange`. That `RunLocal` really makes two tries under rising minors is L-10 `TestRunLocal_HandOverNumbering_…`. M is O's |
| Pass minted by another pnode joins the owner's transaction (minting half) | P-4 `TestToCallout_NumbersOwnerAndOuterComeFromTheRequest`; P-6 `TestHandler_BuildsTheCalloutFromTheRequest`. The joining half is F's and O's |
| §15: no `outcome` → `no_answer`; no `triesUsed` → one try | P-4 `TestReadAnswer_AnswerFromAnOlderVersion` |
| §12: hand-overs by outcome | P-5 `TestPeerRouter_CountsHandOversByOutcome` |

Concurrency: nothing in this stream races by design — a `PeerRouter` holds no
mutable state, and the nonce cache keeps its existing mutex. No test of this
stream belongs in the parity suites.

---

### Task P-1: `PeerAuth` — requests carry a direction label; an answer can be sealed for exactly one request

**Spec:** §6 Transport, last bullet ("The response is authenticated and encrypted … a fresh random nonce of its own … a direction label (`"response"`; requests gain `"request"` …), the path, and the request's nonce and timestamp … Responses do not enter the replay cache"); §6 "where the peer refuses a request it has already opened and authenticated — a replayed nonce, a full replay cache" (this task makes that case *recognisable*; P-6 answers it).

**Files:**
- Modify: `internal/cluster/dispatch/peer_auth.go` (`PeerAuth` interface; new `ResponseBinding`, `ErrReplayRefused`)
- Modify: `internal/cluster/dispatch/aead_peer_auth.go` (`Sign` :121-139, `Verify` :144-191, `buildAD` :196-204 → `buildRequestAD`, `buildResponseAD`; new `SealResponse`, `OpenResponse`)
- Modify: `internal/cluster/dispatch/forwarder.go` (`forward` :99 — the extra return value, discarded until P-2)
- Modify: `internal/cluster/dispatch/handler.go` (`verifyRequest` :136 — likewise)
- Modify: `internal/cluster/scheduler_rpc.go` (`ExecuteScheduledTask` :190, `handle` :255 — the extra return value, discarded; **nothing else** in the scheduler path changes, its responses stay plain JSON and its client keeps its own `http.Client`)
- Modify (tests, mechanical): `internal/cluster/dispatch/aead_peer_auth_test.go`, `handler_test.go:82`, `internal/e2e/tx_lifecycle_e2e_test.go:636`
- Test: `internal/cluster/dispatch/aead_response_test.go` (new, `package dispatch`)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type ResponseBinding struct{ /* path, request nonce, request timestamp */ }`
  - `var ErrReplayRefused error`
  - `PeerAuth.Sign(req *http.Request, body []byte) (wireBody []byte, binding ResponseBinding, err error)`
  - `PeerAuth.Verify(r *http.Request) (body []byte, identity PeerIdentity, binding ResponseBinding, err error)` — `binding` is usable when `err == nil` **and** when `errors.Is(err, ErrReplayRefused)`: the request was opened and authenticated and only the replay cache refused it.
  - `PeerAuth.SealResponse(h http.Header, binding ResponseBinding, body []byte) (wireBody []byte, err error)`
  - `PeerAuth.OpenResponse(h http.Header, binding ResponseBinding, wireBody []byte) (body []byte, err error)`

Both pnodes of a pair change together (§15: no mixed-version clusters): a request
signed without the label no longer verifies.

- [ ] **Step 1: Write the failing tests**

`internal/cluster/dispatch/aead_response_test.go`:

```go
package dispatch

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

const handOverPath = "/internal/dispatch/callout"

// newBoundRequest signs body as the owner would and returns the request the peer
// receives, the bytes that went on the wire, and the binding the owner keeps.
func newBoundRequest(t *testing.T, a *AEADPeerAuth, path string, body []byte) (*http.Request, []byte, ResponseBinding) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	wire, binding, err := a.Sign(req, body)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(wire))
	req.ContentLength = int64(len(wire))
	return req, wire, binding
}

func TestAEADResponse_RoundTrip(t *testing.T) {
	owner, peer := newAEAD(t), newAEAD(t)
	req, _, ownerBinding := newBoundRequest(t, owner, handOverPath, []byte(`{"q":1}`))

	_, _, peerBinding, err := peer.Verify(req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	h := http.Header{}
	wire, err := peer.SealResponse(h, peerBinding, []byte(`{"a":2}`))
	if err != nil {
		t.Fatalf("SealResponse: %v", err)
	}
	if got := h.Get("Content-Type"); got != DispatchContentType {
		t.Errorf("Content-Type = %q, want %q", got, DispatchContentType)
	}
	if bytes.Contains(wire, []byte(`"a"`)) {
		t.Error("the answer is on the wire in the clear")
	}
	got, err := owner.OpenResponse(h, ownerBinding, wire)
	if err != nil {
		t.Fatalf("OpenResponse: %v", err)
	}
	if string(got) != `{"a":2}` {
		t.Errorf("opened %q", got)
	}
}

func TestAEADResponse_Refused(t *testing.T) {
	owner, peer := newAEAD(t), newAEAD(t)
	stranger, err := NewAEADPeerAuth(bytes.Repeat([]byte{0xCD}, 32), 30*time.Second)
	if err != nil {
		t.Fatalf("NewAEADPeerAuth: %v", err)
	}

	reqA, wireA, bindingA := newBoundRequest(t, owner, handOverPath, []byte(`{"q":"A"}`))
	_, _, peerBindingA, err := peer.Verify(reqA)
	if err != nil {
		t.Fatalf("Verify A: %v", err)
	}
	_, _, bindingB := newBoundRequest(t, owner, handOverPath, []byte(`{"q":"B"}`))

	sealed := func(a *AEADPeerAuth, b ResponseBinding) (http.Header, []byte) {
		h := http.Header{}
		wire, err := a.SealResponse(h, b, []byte(`{"outcome":"no_handoff"}`))
		if err != nil {
			t.Fatalf("SealResponse: %v", err)
		}
		return h, wire
	}
	goodHeader, good := sealed(peer, peerBindingA)
	tampered := append([]byte(nil), good...)
	tampered[len(tampered)-1] ^= 0x01
	_, forged := sealed(stranger, peerBindingA)

	tests := []struct {
		name    string
		header  http.Header
		binding ResponseBinding
		wire    []byte
	}{
		{"forged with another key", goodHeader, bindingA, forged},
		{"tampered", goodHeader, bindingA, tampered},
		{"the request reflected back as its own answer", goodHeader, bindingA, wireA},
		{"replayed onto another request", goodHeader, bindingB, good},
		{"truncated", goodHeader, bindingA, good[:len(good)/2]},
		{"shorter than an envelope", goodHeader, bindingA, []byte{1, 2, 3}},
		{"not marked as a dispatch envelope", http.Header{"Content-Type": {"application/json"}}, bindingA, good},
		{"opened without a binding", goodHeader, ResponseBinding{}, good},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := owner.OpenResponse(tt.header, tt.binding, tt.wire); err == nil {
				t.Fatal("OpenResponse accepted it")
			}
		})
	}
	// and the good one still opens — the table above is not vacuous
	if _, err := owner.OpenResponse(goodHeader, bindingA, good); err != nil {
		t.Fatalf("the genuine answer must open: %v", err)
	}
}

func TestAEADResponse_CannotBePresentedAsARequest(t *testing.T) {
	owner, peer := newAEAD(t), newAEAD(t)
	req, _, _ := newBoundRequest(t, owner, handOverPath, []byte(`{}`))
	ts := req.Header.Get(DispatchTimestampHdr)
	_, _, binding, err := peer.Verify(req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	wire, err := peer.SealResponse(http.Header{}, binding, []byte(`{}`))
	if err != nil {
		t.Fatalf("SealResponse: %v", err)
	}
	reflected := httptest.NewRequest(http.MethodPost, handOverPath, bytes.NewReader(wire))
	reflected.Header.Set("Content-Type", DispatchContentType)
	reflected.Header.Set(DispatchTimestampHdr, ts)
	if _, _, _, err := owner.Verify(reflected); err == nil {
		t.Fatal("an answer was accepted as a request")
	}
}

func TestAEADResponse_NonceIsFreshEveryTime(t *testing.T) {
	owner, peer := newAEAD(t), newAEAD(t)
	req, reqWire, _ := newBoundRequest(t, owner, handOverPath, []byte(`{}`))
	_, _, binding, err := peer.Verify(req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	first, err := peer.SealResponse(http.Header{}, binding, []byte(`{}`))
	if err != nil {
		t.Fatalf("SealResponse: %v", err)
	}
	second, err := peer.SealResponse(http.Header{}, binding, []byte(`{}`))
	if err != nil {
		t.Fatalf("SealResponse: %v", err)
	}
	const nonceSize = 12
	if bytes.Equal(first[:nonceSize], reqWire[:nonceSize]) {
		t.Error("the answer reuses the request's nonce under the same key")
	}
	if bytes.Equal(first[:nonceSize], second[:nonceSize]) {
		t.Error("two answers share a nonce")
	}
}

func TestAEADResponse_SealNeedsABinding(t *testing.T) {
	if _, err := newAEAD(t).SealResponse(http.Header{}, ResponseBinding{}, []byte(`{}`)); err == nil {
		t.Fatal("sealed an answer bound to no request")
	}
}

// A request in the format of before the direction label no longer verifies.
func TestAEADRequest_WithoutDirectionLabelRefused(t *testing.T) {
	a := newAEAD(t)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := make([]byte, a.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	unlabelled := []byte(http.MethodPost + "\n" + handOverPath + "\n" + ts)
	wire := append(append([]byte(nil), nonce...), a.gcm.Seal(nil, nonce, []byte(`{}`), unlabelled)...)

	req := httptest.NewRequest(http.MethodPost, handOverPath, bytes.NewReader(wire))
	req.Header.Set(DispatchTimestampHdr, ts)
	if _, _, _, err := a.Verify(req); err == nil {
		t.Fatal("a request without the direction label verified")
	}
}

// A replay is refused, but — unlike a request that does not authenticate — the
// refusal comes with the binding, so the peer can say so under seal.
func TestAEADVerify_ReplayIsRefusedWithAUsableBinding(t *testing.T) {
	owner, peer := newAEAD(t), newAEAD(t)
	first, wire, ownerBinding := newBoundRequest(t, owner, handOverPath, []byte(`{}`))
	if _, _, _, err := peer.Verify(first); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	again := httptest.NewRequest(http.MethodPost, handOverPath, bytes.NewReader(wire))
	again.Header.Set(DispatchTimestampHdr, first.Header.Get(DispatchTimestampHdr))

	body, _, binding, err := peer.Verify(again)
	if !errors.Is(err, ErrReplayRefused) {
		t.Fatalf("err = %v, want ErrReplayRefused", err)
	}
	if body != nil {
		t.Error("a refused request must not yield its body")
	}
	h := http.Header{}
	sealed, err := peer.SealResponse(h, binding, []byte(`{"outcome":"no_handoff"}`))
	if err != nil {
		t.Fatalf("SealResponse under the refused request's binding: %v", err)
	}
	if _, err := owner.OpenResponse(h, ownerBinding, sealed); err != nil {
		t.Fatalf("the owner cannot open the sealed refusal: %v", err)
	}
}

func TestAEADVerify_FullReplayCacheIsRefusedWithAUsableBinding(t *testing.T) {
	owner, peer := newAEAD(t), newAEAD(t)
	peer.nonces = newNonceCache(time.Minute, 1, time.Now)

	one, _, _ := newBoundRequest(t, owner, handOverPath, []byte(`{}`))
	if _, _, _, err := peer.Verify(one); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	two, _, _ := newBoundRequest(t, owner, handOverPath, []byte(`{}`))
	_, _, binding, err := peer.Verify(two)
	if !errors.Is(err, ErrReplayRefused) {
		t.Fatalf("err = %v, want ErrReplayRefused", err)
	}
	if _, err := peer.SealResponse(http.Header{}, binding, []byte(`{}`)); err != nil {
		t.Fatalf("binding not usable: %v", err)
	}
}

// A request that does not authenticate yields no binding: there is nothing to
// seal an answer to, and the peer answers a bare 403.
func TestAEADVerify_UnauthenticatedYieldsNoBinding(t *testing.T) {
	a := newAEAD(t)
	req := httptest.NewRequest(http.MethodPost, handOverPath, bytes.NewReader(bytes.Repeat([]byte{7}, 64)))
	req.Header.Set(DispatchTimestampHdr, strconv.FormatInt(time.Now().Unix(), 10))
	_, _, binding, err := a.Verify(req)
	if err == nil || errors.Is(err, ErrReplayRefused) {
		t.Fatalf("err = %v, want an authentication failure", err)
	}
	if _, err := a.SealResponse(http.Header{}, binding, []byte(`{}`)); err == nil {
		t.Error("a binding came back from a request that did not authenticate")
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/cluster/dispatch/... -run 'TestAEADResponse|TestAEADRequest_WithoutDirectionLabel|TestAEADVerify_'`
Expected: FAIL (build) — `assignment mismatch: 3 variables but a.Sign returns 2 values`, `peer.SealResponse undefined`.

- [ ] **Step 3: Implement**

`peer_auth.go` — replace the interface and add the two new names. The doc
comment's "Contract between PeerAuth and HTTPForwarder.forward" block stays,
with one bullet added: *"forward keeps the binding Sign returned and opens the
answer with it; a transport-auth-only impl returns the body unchanged from both
SealResponse and OpenResponse."*

```go
// ErrReplayRefused is returned by Verify when a request was opened and
// authenticated and only the replay cache refused it — its nonce was seen
// before, or the cache is full. The binding returned beside it is valid, so the
// handler can answer under seal instead of with a bare status.
var ErrReplayRefused = errors.New("request refused by the replay cache")

// ResponseBinding is what ties an answer to the one request it answers: the
// request's path, nonce and timestamp. Sign returns it to the sender; Verify
// returns the same value to the receiver. The zero value binds to nothing and
// neither seals nor opens.
type ResponseBinding struct {
	path  string
	nonce []byte
	ts    string
}

type PeerAuth interface {
	// Sign transforms the plaintext body into an on-the-wire body, setting
	// any required headers on req, and returns the binding under which the
	// answer to this request must be opened. The returned slice replaces
	// req.Body at the call site. Implementations MAY write to req.Header
	// (including overriding Content-Type) but MUST NOT capture or retain req
	// past the call.
	Sign(req *http.Request, body []byte) (wireBody []byte, binding ResponseBinding, err error)

	// Verify reads the request body, validates it, and returns the
	// authenticated plaintext, the peer's identity and the binding for the
	// answer. A non-nil error means the request is refused. The binding is
	// valid when err is nil and when errors.Is(err, ErrReplayRefused); for any
	// other error it is the zero value and the caller must respond with 403.
	Verify(r *http.Request) (body []byte, identity PeerIdentity, binding ResponseBinding, err error)

	// SealResponse wraps an answer for the request binding names, setting the
	// headers the wire format needs on h.
	SealResponse(h http.Header, binding ResponseBinding, body []byte) (wireBody []byte, err error)

	// OpenResponse is the inverse, on the side that sent the request. It
	// fails for an answer that was not sealed, with this key, for exactly the
	// request binding names.
	OpenResponse(h http.Header, binding ResponseBinding, wireBody []byte) (body []byte, err error)
}
```

(`peer_auth.go` gains `"errors"` in its imports.)

`aead_peer_auth.go` — the type comment's second paragraph becomes:

```go
// On the wire, a body is [nonce(12) || ciphertext||tag], in both directions.
// The associated data of a request binds a direction label, the HTTP method,
// the path and the timestamp; that of an answer binds the other direction
// label, the path, and the timestamp and nonce of the request it answers. So an
// envelope cannot be replayed across endpoints, reflected back in the other
// direction, or moved onto another request. A sliding nonce cache rejects
// repeated requests within the skew window; answers need none, being bound to a
// request nonce their receiver chose.
```

Replace `Sign`, `Verify` and `buildAD` (`:118-204`) with:

```go
const (
	directionRequest  = "request"
	directionResponse = "response"
)

// Sign wraps body in an AEAD envelope, sets the Content-Type and timestamp
// headers, and returns the wire bytes and the binding for the answer.
func (a *AEADPeerAuth) Sign(req *http.Request, body []byte) ([]byte, ResponseBinding, error) {
	ts := strconv.FormatInt(a.clockFn().Unix(), 10)

	nonce := make([]byte, a.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, ResponseBinding{}, fmt.Errorf("failed to generate nonce: %w", err)
	}

	ct := a.gcm.Seal(nil, nonce, body, buildRequestAD(req.Method, req.URL.Path, ts))
	wire := make([]byte, 0, len(nonce)+len(ct))
	wire = append(wire, nonce...)
	wire = append(wire, ct...)

	req.Header.Set("Content-Type", DispatchContentType)
	req.Header.Set(DispatchTimestampHdr, ts)
	return wire, ResponseBinding{path: req.URL.Path, nonce: nonce, ts: ts}, nil
}

// Verify validates the request's timestamp skew, AEAD envelope, and nonce
// freshness. On success it returns the decrypted plaintext, a PeerIdentity
// describing the authenticated peer, and the binding for the answer.
func (a *AEADPeerAuth) Verify(r *http.Request) ([]byte, PeerIdentity, ResponseBinding, error) {
	none := ResponseBinding{}
	tsStr := r.Header.Get(DispatchTimestampHdr)
	if tsStr == "" {
		return nil, PeerIdentity{}, none, errors.New("missing X-Dispatch-Timestamp header")
	}
	tsUnix, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return nil, PeerIdentity{}, none, fmt.Errorf("malformed timestamp: %w", err)
	}
	tsTime := time.Unix(tsUnix, 0)

	// Skew check happens first — cheap rejection of stale/future envelopes
	// before we touch the body.
	diff := a.clockFn().Sub(tsTime)
	if diff < 0 {
		diff = -diff
	}
	if diff > a.skew {
		return nil, PeerIdentity{}, none, fmt.Errorf("timestamp outside skew window: %v > %v", diff, a.skew)
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, dispatchMaxBodySize))
	if err != nil {
		return nil, PeerIdentity{}, none, fmt.Errorf("failed to read body: %w", err)
	}
	nonceSize := a.gcm.NonceSize()
	if len(body) < nonceSize+a.gcm.Overhead() {
		return nil, PeerIdentity{}, none, errors.New("body too short for AEAD envelope")
	}
	nonce := append([]byte(nil), body[:nonceSize]...)

	pt, err := a.gcm.Open(nil, nonce, body[nonceSize:], buildRequestAD(r.Method, r.URL.Path, tsStr))
	if err != nil {
		return nil, PeerIdentity{}, none, fmt.Errorf("AEAD open failed: %w", err)
	}
	identity := PeerIdentity{authMethod: authMethodAEADv1}
	binding := ResponseBinding{path: r.URL.Path, nonce: nonce, ts: tsStr}

	// Record the nonce only after successful decrypt. A flood of bogus
	// nonces that fail AEAD.Open never enters the cache. From here on the
	// sender is known to hold the key, so a refusal can be answered under seal.
	if a.nonces.checkAndRecord(nonce, tsTime) {
		return nil, identity, binding, fmt.Errorf("duplicate nonce or replay cache full: %w", ErrReplayRefused)
	}
	return pt, identity, binding, nil
}

// SealResponse wraps an answer under a nonce of its own — the request's nonce
// must never be used twice under the one key — and binds it to the request.
func (a *AEADPeerAuth) SealResponse(h http.Header, binding ResponseBinding, body []byte) ([]byte, error) {
	if len(binding.nonce) == 0 {
		return nil, errors.New("no request to bind the answer to")
	}
	nonce := make([]byte, a.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}
	ct := a.gcm.Seal(nil, nonce, body, buildResponseAD(binding))
	wire := make([]byte, 0, len(nonce)+len(ct))
	wire = append(wire, nonce...)
	wire = append(wire, ct...)
	h.Set("Content-Type", DispatchContentType)
	return wire, nil
}

// OpenResponse opens an answer sealed for the request binding names.
func (a *AEADPeerAuth) OpenResponse(h http.Header, binding ResponseBinding, wireBody []byte) ([]byte, error) {
	if len(binding.nonce) == 0 {
		return nil, errors.New("no request the answer could be bound to")
	}
	if ct := h.Get("Content-Type"); ct != DispatchContentType {
		return nil, fmt.Errorf("answer is not a dispatch envelope: Content-Type %q", ct)
	}
	nonceSize := a.gcm.NonceSize()
	if len(wireBody) < nonceSize+a.gcm.Overhead() {
		return nil, errors.New("answer too short for AEAD envelope")
	}
	pt, err := a.gcm.Open(nil, wireBody[:nonceSize], wireBody[nonceSize:], buildResponseAD(binding))
	if err != nil {
		return nil, fmt.Errorf("AEAD open failed: %w", err)
	}
	return pt, nil
}

// buildRequestAD is the associated data of a request. Binding the direction,
// method, path and timestamp to the ciphertext prevents reflection,
// cross-endpoint and timestamp-strip replays without an outer signature.
func buildRequestAD(method, path, ts string) []byte {
	return []byte(directionRequest + "\n" + method + "\n" + path + "\n" + ts)
}

// buildResponseAD is the associated data of an answer: the other direction
// label, the path, and the timestamp and nonce of the request it answers. The
// nonce comes last because it is binary and of fixed length.
func buildResponseAD(b ResponseBinding) []byte {
	ad := []byte(directionResponse + "\n" + b.path + "\n" + b.ts + "\n")
	return append(ad, b.nonce...)
}
```

Call sites, one blank each (no behaviour change in this task):
- `forwarder.go:99` — `wire, _, err := f.auth.Sign(httpReq, plain)`
- `handler.go:136` — `body, identity, _, err := h.auth.Verify(r)`
- `scheduler_rpc.go:190` — `wire, _, err := c.auth.Sign(httpReq, plain)`
- `scheduler_rpc.go:255` — `body, identity, _, err := h.auth.Verify(r)`
- `internal/e2e/tx_lifecycle_e2e_test.go:636` — `wire, _, err := auth.Sign(req, plain)`
- `handler_test.go:82` — `wire, _, err := auth.Sign(req, plain)`
- `aead_peer_auth_test.go` — every `a.Sign(` / `a1.Sign(` / `auth.Sign(` (`:30, 133, 160, 187, 230, 302, 344`) gains a middle blank (`wire, _, err :=`; `:187` `wire, _, _ :=`); every `Verify(` (`:50, 81, 93, 108, 122, 147, 173, 176, 194, 245, 252, 260, 270, 280, 291, 314, 331`) gains a third blank (`got, id, _, err :=`; `_, _, _, err`; `pt, _, _, err :=`).

Existing tests, decision: **all of `aead_peer_auth_test.go` stays** with that
mechanical change — cross-endpoint, method, skew, replay, short-body and
different-secret behaviour is unchanged. `internal/cluster/scheduler_rpc_test.go`
stays untouched: `TestExecutor_ForwardsWithPeerAuth` is the proof that the
scheduler's client and handler changed together, and the 403 tests still hold.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/cluster/...` and `go vet ./internal/cluster/... ./internal/e2e/...`   Expected: PASS / clean.
Exit check: `grep -rn 'buildAD(' internal/` → no hits.

- [ ] **Step 5: Commit**

```
git add internal/cluster/dispatch/peer_auth.go internal/cluster/dispatch/aead_peer_auth.go internal/cluster/dispatch/aead_response_test.go internal/cluster/dispatch/aead_peer_auth_test.go internal/cluster/dispatch/forwarder.go internal/cluster/dispatch/handler.go internal/cluster/dispatch/handler_test.go internal/cluster/scheduler_rpc.go internal/e2e/tx_lifecycle_e2e_test.go
git commit -m "feat(dispatch): requests carry a direction label; an answer can be sealed for one request (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task P-2: both legs of `/internal/dispatch/callout` are sealed

**Spec:** §6 Transport — "The response is authenticated and encrypted with the key the request uses … `Content-Type: application/cyoda-dispatch-v1` on both legs. An answer that fails to open is `no_answer`" (the classification is P-5's; here an answer that fails to open is a forward error, which `ClusterDispatcher` already treats as one).

**Files:**
- Modify: `internal/cluster/dispatch/handler.go` (`handleCallout`, `verifyRequest`; `writeJSON` :211-217 → `writeSealed`)
- Modify: `internal/cluster/dispatch/forwarder.go` (`forward` :81-121)
- Test: `internal/cluster/dispatch/forwarder_test.go`, `handler_test.go`

**Interfaces:**
- Consumes: P-1.
- Produces: test helpers used by every later task —
  - `package dispatch_test`: `sealingHandler(t, auth dispatch.PeerAuth, answer func(r *http.Request, plain []byte) any) http.Handler` and `sealingPeer(…) *httptest.Server` built from it
  - `package dispatch`: `signedRequestWithBinding(t, auth, method, path, plain) (*http.Request, ResponseBinding)`, `decodeSealed(t, auth, binding, rec) DispatchCalloutResponse`

The payload shape does not change in this task.

- [ ] **Step 1: Write the failing tests**

`forwarder_test.go` — add the helper and three tests; `"sync"` joins the imports:

```go
// sealingHandler verifies the request and seals whatever answer returns, as
// the real handler does.
func sealingHandler(t *testing.T, auth dispatch.PeerAuth, answer func(r *http.Request, plain []byte) any) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plain, _, binding, err := auth.Verify(r)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		out, err := json.Marshal(answer(r, plain))
		if err != nil {
			t.Errorf("marshal answer: %v", err)
			return
		}
		wire, err := auth.SealResponse(w.Header(), binding, out)
		if err != nil {
			t.Errorf("SealResponse: %v", err)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(wire)
	})
}

// sealingPeer is a running peer built from sealingHandler.
func sealingPeer(t *testing.T, auth dispatch.PeerAuth, answer func(r *http.Request, plain []byte) any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(sealingHandler(t, auth, answer))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPForwarder_PlaintextAnswerRefused(t *testing.T) {
	auth := newTestPeerAuth(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, _, err := auth.Verify(r); err != nil {
			t.Errorf("Verify: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 5*time.Second).AllowLoopbackForTesting()
	if _, err := f.ForwardCallout(context.Background(), srv.URL, makeProcessorReq()); err == nil {
		t.Fatal("an answer that was not sealed was accepted")
	}
}

func TestHTTPForwarder_AnswerSealedForAnotherRequestRefused(t *testing.T) {
	auth := newTestPeerAuth(t)
	var mu sync.Mutex
	var firstWire []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, binding, err := auth.Verify(r)
		if err != nil {
			t.Errorf("Verify: %v", err)
			return
		}
		wire, err := auth.SealResponse(w.Header(), binding, []byte(`{"success":true}`))
		if err != nil {
			t.Errorf("SealResponse: %v", err)
			return
		}
		func() {
			mu.Lock()
			defer mu.Unlock()
			if firstWire == nil {
				firstWire = wire
			}
			wire = firstWire // every later request is answered with the first answer
		}()
		_, _ = w.Write(wire)
	}))
	defer srv.Close()

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 5*time.Second).AllowLoopbackForTesting()
	if _, err := f.ForwardCallout(context.Background(), srv.URL, makeProcessorReq()); err != nil {
		t.Fatalf("first hand-over: %v", err)
	}
	if _, err := f.ForwardCallout(context.Background(), srv.URL, makeProcessorReq()); err == nil {
		t.Fatal("an answer replayed from an earlier request was accepted")
	}
}

func TestHTTPForwarder_TruncatedAnswerRefused(t *testing.T) {
	auth := newTestPeerAuth(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, binding, err := auth.Verify(r)
		if err != nil {
			t.Errorf("Verify: %v", err)
			return
		}
		wire, _ := auth.SealResponse(w.Header(), binding, []byte(`{"success":true,"entityData":"AAAAAAAAAAAAAAAA"}`))
		_, _ = w.Write(wire[:len(wire)/2])
	}))
	defer srv.Close()

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 5*time.Second).AllowLoopbackForTesting()
	if _, err := f.ForwardCallout(context.Background(), srv.URL, makeProcessorReq()); err == nil {
		t.Fatal("a truncated answer was accepted")
	}
}
```

`handler_test.go` — replace `signedRequest` (`:79-89`) with the pair below, add
`decodeSealed` and one test:

```go
// signedRequestWithBinding builds an AEAD-wrapped request ready for the handler
// to verify, and returns the binding its answer opens under.
func signedRequestWithBinding(t *testing.T, auth *AEADPeerAuth, method, path string, plain []byte) (*http.Request, ResponseBinding) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	wire, binding, err := auth.Sign(req, plain)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(wire))
	req.ContentLength = int64(len(wire))
	return req, binding
}

// signedRequest is signedRequestWithBinding for a test that does not read the answer.
func signedRequest(t *testing.T, auth *AEADPeerAuth, method, path string, plain []byte) *http.Request {
	t.Helper()
	req, _ := signedRequestWithBinding(t, auth, method, path, plain)
	return req
}

// decodeSealed opens the handler's answer as the owner would.
func decodeSealed(t *testing.T, auth *AEADPeerAuth, binding ResponseBinding, rec *httptest.ResponseRecorder) DispatchCalloutResponse {
	t.Helper()
	plain, err := auth.OpenResponse(rec.Header(), binding, rec.Body.Bytes())
	if err != nil {
		t.Fatalf("the answer does not open under its request's binding: %v (status %d)", err, rec.Code)
	}
	var resp DispatchCalloutResponse
	if err := json.Unmarshal(plain, &resp); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	return resp
}

func TestHandler_AnswerIsSealedForItsRequest(t *testing.T) {
	auth := newAEAD(t)
	handler := NewDispatchHandler(&fakeLocalDispatcher{
		processorResult: &spi.Entity{Meta: spi.EntityMeta{ID: "ent-1"}, Data: []byte(`{"output":42}`)},
	}, auth)
	mux := http.NewServeMux()
	handler.Register(mux)

	processor := spi.ProcessorDefinition{Name: "proc1"}
	plain, _ := json.Marshal(DispatchCalloutRequest{
		Kind: "processor", Entity: json.RawMessage(`{}`),
		EntityMeta: spi.EntityMeta{ID: "ent-1", TenantID: "tenant-a"}, TenantID: "tenant-a",
		Processor: &processor, UserID: "u", PrincipalKind: spi.PrincipalUser,
	})
	httpReq, binding := signedRequestWithBinding(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != DispatchContentType {
		t.Errorf("Content-Type = %q, want %q", got, DispatchContentType)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("output")) {
		t.Error("the answer is on the wire in the clear")
	}
	if resp := decodeSealed(t, auth, binding, rec); string(resp.EntityData) != `{"output":42}` {
		t.Errorf("EntityData = %s", resp.EntityData)
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/cluster/dispatch/... -run 'TestHTTPForwarder_(Plaintext|AnswerSealed|Truncated)|TestHandler_AnswerIsSealed'`
Expected: FAIL — `an answer that was not sealed was accepted`; `the answer does not open under its request's binding`.

- [ ] **Step 3: Implement**

`handler.go` — `verifyRequest` returns the binding; `writeJSON` is replaced:

```go
func (h *DispatchHandler) verifyRequest(w http.ResponseWriter, r *http.Request) ([]byte, PeerIdentity, ResponseBinding, bool) {
	body, identity, binding, err := h.auth.Verify(r)
	if err != nil {
		slog.Warn("dispatch request auth failed",
			"pkg", "dispatch",
			"remoteAddr", r.RemoteAddr,
			"err", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, PeerIdentity{}, ResponseBinding{}, false
	}
	return body, identity, binding, true
}

// writeSealed answers the request binding names, under seal. The owner trusts
// nothing else: a status line or a body it cannot open tells it only that the
// answer was lost.
func (h *DispatchHandler) writeSealed(w http.ResponseWriter, binding ResponseBinding, v DispatchCalloutResponse) {
	plain, err := json.Marshal(v)
	if err != nil {
		slog.Error("failed to marshal dispatch answer", "pkg", "dispatch", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	wire, err := h.auth.SealResponse(w.Header(), binding, plain)
	if err != nil {
		slog.Error("failed to seal dispatch answer", "pkg", "dispatch", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(wire); err != nil {
		slog.Warn("failed to write dispatch answer", "pkg", "dispatch", "err", err)
	}
}
```

In `handleCallout`: `body, identity, binding, ok := h.verifyRequest(w, r)`; each of
the six `writeJSON(w, http.StatusOK, X)` calls becomes `h.writeSealed(w, binding, X)`.
The three bare 400s stay until P-6.

`forwarder.go` — in `forward`, keep the binding and open the answer. Before:

```go
	wire, _, err := f.auth.Sign(httpReq, plain)
	…
	if err := json.NewDecoder(httpResp.Body).Decode(respBody); err != nil {
		return fmt.Errorf("dispatch forward: decode response from %s: %w", url, err)
	}
	return nil
```

After:

```go
	wire, binding, err := f.auth.Sign(httpReq, plain)
	…
	sealed, err := io.ReadAll(io.LimitReader(httpResp.Body, dispatchMaxBodySize+1))
	if err != nil {
		return fmt.Errorf("dispatch forward: read response from %s: %w", url, err)
	}
	opened, err := f.auth.OpenResponse(httpResp.Header, binding, sealed)
	if err != nil {
		return fmt.Errorf("dispatch forward: open response from %s: %w", url, err)
	}
	if err := json.Unmarshal(opened, respBody); err != nil {
		return fmt.Errorf("dispatch forward: decode response from %s: %w", url, err)
	}
	return nil
```

and the function comment's last sentence ("Response bodies are not AEAD-wrapped
…") becomes: *"The answer comes back sealed for this request and is opened with
the binding Sign returned."*

Existing tests, decisions:
- `forwarder_test.go` `TestHTTPForwarder_ProcessorSuccess`, `_CriteriaSuccess`, `_WireBodyIsEncrypted`, `_AddrWithoutScheme`: **rewritten** to serve through `sealingPeer(t, auth, func(r, plain) any { … return wantResp })`, keeping their assertions on path, headers (`verifyAEADHeaders(t, r)` is called inside `answer`) and on the decoded result; where a test decoded the request body itself it now reads the `plain` argument. `_PeerUnreachable`, `_PeerReturnsError` and all of `forwarder_ssrf_test.go`: **stay**.
- `handler_test.go`: every test that did `json.NewDecoder(rec.Body).Decode(&resp)` — `TestHandler_ProcessorSuccess`, `_CriteriaSuccess`, `_ProcessorError_SanitizedResponse`, `TestHandleCriteria_PropagatesReason`, `_CriteriaError_SanitizedResponse`, `_ErrorTaxonomy_AppError`, `_ErrorTaxonomy_NoMatchingMember` — takes its request from `signedRequestWithBinding` and its answer from `decodeSealed`. The two sanitisation tests assert on `rec.Body.String()`; they assert on the opened JSON instead (`plain, _ := auth.OpenResponse(…)`). The 403/400 tests **stay**.
- `cluster_dispatcher_test.go`, `cluster_dispatcher_failover_test.go`, `integration_test.go`, `integration_txtoken_test.go`: **stay** — they run the real handler against the real forwarder, which changed together.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/cluster/...`, `go vet ./internal/cluster/...`   Expected: PASS.
Exit check: `grep -n 'func writeJSON' internal/cluster/dispatch/` → no hits.

- [ ] **Step 5: Commit**

```
git add internal/cluster/dispatch/handler.go internal/cluster/dispatch/forwarder.go internal/cluster/dispatch/handler_test.go internal/cluster/dispatch/forwarder_test.go
git commit -m "feat(dispatch): the answer to a hand-over is sealed for its request (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task P-3: a `Callout` carries what it was built from

**Spec:** §6 (the hand-over must put the callout on the wire) against the seam `HandOver(ctx, peer, call internalgrpc.Callout, …)`. See "What the code says today", last bullet, and Open point 1.

**Files:**
- Modify: `internal/grpc/callout.go` (`Callout`; the three builders, as L-6 leaves them)
- Test: `internal/grpc/callout_test.go`

**Interfaces:**
- Consumes: L-6.
- Produces:
  ```go
  // CalloutSource is what a Callout was built from: what another pnode needs to
  // build the same Callout with the same builder.
  type CalloutSource struct {
      Entity                       *spi.Entity
      WorkflowName, TransitionName string
      Processor                    *spi.ProcessorDefinition // ProcessorCallout
      Criterion                    json.RawMessage          // CriteriaCallout
      Target, ProcessorName        string                   // CriteriaCallout
      Function                     *spi.ScheduleFunction    // FunctionCallout
  }
  ```
  and the field `Callout.Source CalloutSource`, filled by all three builders.

- [ ] **Step 1: Write the failing test**

Append to `internal/grpc/callout_test.go`:

```go
func TestCallout_SourceIsWhatItWasBuiltFrom(t *testing.T) {
	entity := testEntity()
	processor := testProcessor("python", 1500)
	criterion := json.RawMessage(`{"type":"function","function":{"name":"c","config":{"calculationNodesTags":"python"}}}`)
	fn := spi.ScheduleFunction{Name: "f", CalculationNodesTags: "python"}

	p := NewProcessorCallout(testTenantID, entity, processor, "wf1", "t1", "tx-1")
	if p.Source.Entity != entity || p.Source.WorkflowName != "wf1" || p.Source.TransitionName != "t1" ||
		p.Source.Processor == nil || p.Source.Processor.Name != processor.Name {
		t.Errorf("processor source = %+v", p.Source)
	}
	if p.Source.Criterion != nil || p.Source.Function != nil {
		t.Errorf("a processor callout carries only a processor: %+v", p.Source)
	}

	c, failure := NewCriteriaCallout(testTenantID, entity, criterion, "transition", "wf1", "t1", "proc-1", "tx-1")
	if failure != nil {
		t.Fatalf("NewCriteriaCallout: %v", failure)
	}
	if c.Source.Entity != entity || string(c.Source.Criterion) != string(criterion) ||
		c.Source.Target != "transition" || c.Source.ProcessorName != "proc-1" ||
		c.Source.WorkflowName != "wf1" || c.Source.TransitionName != "t1" {
		t.Errorf("criteria source = %+v", c.Source)
	}

	f := NewFunctionCallout(testTenantID, entity, fn, "wf1", "t1", "tx-1")
	if f.Source.Entity != entity || f.Source.Function == nil || f.Source.Function.Name != "f" ||
		f.Source.WorkflowName != "wf1" || f.Source.TransitionName != "t1" {
		t.Errorf("function source = %+v", f.Source)
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/grpc/... -run TestCallout_SourceIsWhatItWasBuiltFrom`   Expected: FAIL (build) — `p.Source undefined`.

- [ ] **Step 3: Implement**

`internal/grpc/callout.go` — add the `CalloutSource` type exactly as under
**Produces**, the field in `Callout` after `Outer`:

```go
	// Source is what the callout was built from. A hand-over sends it, and the
	// pnode that receives it builds the same Callout with the same builder.
	Source CalloutSource
```

and one line in each builder's composite literal:

```go
// NewProcessorCallout
		Source: CalloutSource{Entity: entity, WorkflowName: workflowName, TransitionName: transitionName, Processor: &processor},
// NewCriteriaCallout
		Source: CalloutSource{Entity: entity, WorkflowName: workflowName, TransitionName: transitionName,
			Criterion: criterion, Target: target, ProcessorName: processorName},
// NewFunctionCallout
		Source: CalloutSource{Entity: entity, WorkflowName: workflowName, TransitionName: transitionName, Function: &fn},
```

(`processor` and `fn` are the builders' by-value parameters, so the pointers are
to copies the caller cannot change.)

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/grpc/...`, `go vet ./internal/grpc/...`   Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/grpc/callout.go internal/grpc/callout_test.go
git commit -m "feat(grpc): a Callout carries what it was built from, for the hand-over (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task P-4: the hand-over's fields, and how each side reads them

**Spec:** §6 both tables; "How the owner reads an answer" (the parts that concern a *decoded* answer: `outcome`, `triesUsed` range, a missing `outcome` or `triesUsed`); D9, D12; §15.

**Files:**
- Modify: `internal/cluster/dispatch/types.go` (new fields; the old ones stay until P-6 removes them with their last reader)
- Create: `internal/cluster/dispatch/handover.go`
- Create: `internal/cluster/dispatch/fixtures_test.go` (`package dispatch`) — **move**, unchanged, from `cluster_dispatcher_test.go:81-144`: `stubNodeRegistry` and its methods, `testContext`, `testEntity`, `testProcessor`, `testCriterion`, `testFunction`. The owner's-loop stream deletes `cluster_dispatcher_test.go` with the code it tests; the fixtures this stream's tests share must not go with it. (M-5 adds `Changed()` to `stubNodeRegistry`; whichever lands second edits the file the type is in then.)
- Test: `internal/cluster/dispatch/handover_test.go` (new, `package dispatch`), `types_test.go`

**Interfaces:**
- Consumes: P-3 `Callout.Source`; L-1 `contract.CalloutFailure`, `CalloutFailureKind` (+ `String()`), `CalloutAttempt`; L-8 `internalgrpc.LocalResult`, `internalgrpc.NewMinorNumberer`; L-10 `Callout.Outer []token.Pair`; F `token.Pair{Callout string; Major, Minor uint32}`.
- Produces:
  - `type HandOverAnswer struct{…}` — exactly the seam.
  - `type WirePair struct{ Callout string; Major, Minor uint32 }`, `type WireAttempt struct{ MemberID, Kind, Cause string }`
  - `const OutcomeOK = "ok"`; the other four outcomes are `contract.CalloutFailureKind.String()`.
  - unexported, used by P-5/P-6: `newHandOverRequest`, `(*DispatchCalloutRequest).validate`, `(*DispatchCalloutRequest).toCallout`, `responseFromLocal`, `refusal`, `readAnswer`, `lostAnswer`, `notConnected`, `provedBeforeConnecting`.

`triesUsed` is a `*int` on the wire so that "absent" (one try, §15) and `0`
(nothing was tried) are different answers.

- [ ] **Step 1: Write the failing tests**

`internal/cluster/dispatch/handover_test.go`:

```go
package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

func intPtr(n int) *int { return &n }

// ownerCallout is a callout as the owner has it when it hands over.
func ownerCallout(t *testing.T, kind string) internalgrpc.Callout {
	t.Helper()
	var call internalgrpc.Callout
	switch kind {
	case "processor":
		call = internalgrpc.NewProcessorCallout("tenant-1", testEntity(), testProcessor(), "wf", "tr", "tx-1")
	case "criteria":
		var failure *contract.CalloutFailure
		call, failure = internalgrpc.NewCriteriaCallout("tenant-1", testEntity(), testCriterion(), "TRANSITION", "wf", "tr", "proc", "tx-1")
		if failure != nil {
			t.Fatalf("NewCriteriaCallout: %v", failure)
		}
	case "function":
		call = internalgrpc.NewFunctionCallout("tenant-1", testEntity(), testFunction(), "wf", "tr", "tx-1")
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	call.RequestID = "rid-1"
	call.AnswerLimit = 1500 * time.Millisecond
	call.OwnerNodeID = "owner-node"
	call.Outer = []token.Pair{{Callout: "outer-rid", Major: 3, Minor: 1}}
	return call
}

func TestNewHandOverRequest(t *testing.T) {
	uc := spi.MustGetUserContext(testContext())
	for _, kind := range []string{"processor", "criteria", "function"} {
		t.Run(kind, func(t *testing.T) {
			call := ownerCallout(t, kind)
			call.RepeatSafe = kind != "processor"
			req, err := newHandOverRequest(uc, "owner-node", call, 3, 7)
			if err != nil {
				t.Fatalf("newHandOverRequest: %v", err)
			}
			if req.Kind != kind || req.RequestID != "rid-1" || req.TriesLeft != 3 || req.AnswerLimitMs != 1500 ||
				req.OwnerNodeID != "owner-node" || req.Major != 7 || req.RepeatSafe != call.RepeatSafe {
				t.Errorf("hand-over fields wrong: %+v", req)
			}
			if len(req.Outer) != 1 || req.Outer[0] != (WirePair{Callout: "outer-rid", Major: 3, Minor: 1}) {
				t.Errorf("Outer = %+v", req.Outer)
			}
			if req.TenantID != "tenant-1" || string(req.EntityMeta.TenantID) != "tenant-1" || req.TxID != "tx-1" ||
				req.Tags != "python" || req.UserID != "user-1" || req.PrincipalKind != spi.PrincipalUser ||
				req.WorkflowName != "wf" || req.TransitionName != "tr" || string(req.Entity) != `{"key":"value"}` {
				t.Errorf("shared fields wrong: %+v", req)
			}
			switch kind {
			case "processor":
				if req.Processor == nil || req.Processor.Name != "myProcessor" {
					t.Errorf("Processor = %+v", req.Processor)
				}
			case "criteria":
				if string(req.Criterion) != string(testCriterion()) || req.Target != "TRANSITION" || req.ProcessorName != "proc" {
					t.Errorf("criteria fields wrong: %+v", req)
				}
			case "function":
				if req.Function == nil || req.Function.Name != "myScheduleFn" {
					t.Errorf("Function = %+v", req.Function)
				}
			}
			if err := req.validate(); err != nil {
				t.Errorf("what the owner builds must validate on the peer: %v", err)
			}
		})
	}
}

func TestNewHandOverRequest_RefusesWhatCannotBeHandedOver(t *testing.T) {
	uc := spi.MustGetUserContext(testContext())
	tests := []struct {
		name   string
		mutate func(*internalgrpc.Callout)
		tries  int
		major  uint32
	}{
		{"no request id", func(c *internalgrpc.Callout) { c.RequestID = "" }, 1, 1},
		{"no answer limit", func(c *internalgrpc.Callout) { c.AnswerLimit = 0 }, 1, 1},
		{"no tries left", func(*internalgrpc.Callout) {}, 0, 1},
		{"no fencing number", func(*internalgrpc.Callout) {}, 1, 0},
		{"no source", func(c *internalgrpc.Callout) { c.Source = internalgrpc.CalloutSource{} }, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := ownerCallout(t, "processor")
			tt.mutate(&call)
			if _, err := newHandOverRequest(uc, "owner-node", call, tt.tries, tt.major); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func validRequest(t *testing.T, kind string) DispatchCalloutRequest {
	t.Helper()
	req, err := newHandOverRequest(spi.MustGetUserContext(testContext()), "owner-node", ownerCallout(t, kind), 2, 5)
	if err != nil {
		t.Fatalf("newHandOverRequest: %v", err)
	}
	return req
}

func TestRequestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*DispatchCalloutRequest)
	}{
		{"unknown kind", func(r *DispatchCalloutRequest) { r.Kind = "bogus" }},
		{"entity of another tenant", func(r *DispatchCalloutRequest) { r.EntityMeta.TenantID = "tenant-2" }},
		{"entity with no tenant", func(r *DispatchCalloutRequest) { r.EntityMeta.TenantID = "" }},
		{"no request id", func(r *DispatchCalloutRequest) { r.RequestID = "" }},
		{"no tries", func(r *DispatchCalloutRequest) { r.TriesLeft = 0 }},
		{"no answer limit", func(r *DispatchCalloutRequest) { r.AnswerLimitMs = 0 }},
		{"no owner", func(r *DispatchCalloutRequest) { r.OwnerNodeID = "" }},
		{"no fencing number", func(r *DispatchCalloutRequest) { r.Major = 0 }},
		{"processor missing", func(r *DispatchCalloutRequest) { r.Processor = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validRequest(t, "processor")
			tt.mutate(&req)
			err := req.validate()
			if err == nil {
				t.Fatal("expected an error")
			}
			// Both tenants are peer-supplied: the refusal names neither.
			if msg := err.Error(); strings.Contains(msg, "tenant-1") || strings.Contains(msg, "tenant-2") {
				t.Errorf("the refusal names a tenant: %q", msg)
			}
		})
	}
}

func TestToCallout_NumbersOwnerAndOuterComeFromTheRequest(t *testing.T) {
	for _, kind := range []string{"processor", "criteria", "function"} {
		t.Run(kind, func(t *testing.T) {
			req := validRequest(t, kind)
			req.RepeatSafe = true
			call, failure := req.toCallout()
			if failure != nil {
				t.Fatalf("toCallout: %v", failure)
			}
			if call.Kind.String() != kind || call.RequestID != "rid-1" || call.AnswerLimit != 1500*time.Millisecond ||
				call.OwnerNodeID != "owner-node" || !call.RepeatSafe || call.TxID != "tx-1" ||
				call.TenantID != "tenant-1" || call.Tags != "python" {
				t.Errorf("callout = %+v", call)
			}
			if len(call.Outer) != 1 || call.Outer[0] != (token.Pair{Callout: "outer-rid", Major: 3, Minor: 1}) {
				t.Errorf("Outer = %+v", call.Outer)
			}
			if major, minor := call.Number.Next(); major != 5 || minor != 1 {
				t.Errorf("first try numbered (%d,%d), want (5,1)", major, minor)
			}
			if major, minor := call.Number.Next(); major != 5 || minor != 2 {
				t.Errorf("second try numbered (%d,%d), want (5,2)", major, minor)
			}
		})
	}
}

// The owner decides whether a callout is repeat-safe; the peer does not decide
// again from the kind.
func TestToCallout_RepeatSafeIsTheOwners(t *testing.T) {
	req := validRequest(t, "criteria")
	req.RepeatSafe = false
	call, failure := req.toCallout()
	if failure != nil {
		t.Fatalf("toCallout: %v", failure)
	}
	if call.RepeatSafe {
		t.Error("RepeatSafe was decided again by the peer")
	}
}

func TestToCallout_CriterionThatDoesNotParseIsTerminal(t *testing.T) {
	req := validRequest(t, "criteria")
	req.Criterion = json.RawMessage(`{"function":`)
	if _, failure := req.toCallout(); failure == nil || failure.Kind != contract.Terminal {
		t.Fatalf("failure = %+v, want Terminal", failure)
	}
}

func timeoutFailure(kind contract.CalloutFailureKind) *contract.CalloutFailure {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
		"processor dispatch timed out after 1500ms: no response").AsRetryable()
	return &contract.CalloutFailure{Kind: kind, Code: appErr.Code, Message: appErr.Message, Err: appErr}
}

func TestResponseFromLocal(t *testing.T) {
	timeout := timeoutFailure(contract.NoAnswer)
	tests := []struct {
		name  string
		kind  string
		res   internalgrpc.LocalResult
		check func(t *testing.T, r DispatchCalloutResponse)
	}{
		{"processor ok", "processor",
			internalgrpc.LocalResult{TriesUsed: 1, Result: internalgrpc.CalloutResult{Entity: &spi.Entity{Data: []byte(`{"out":1}`)}}},
			func(t *testing.T, r DispatchCalloutResponse) {
				if r.Outcome != OutcomeOK || *r.TriesUsed != 1 || string(r.EntityData) != `{"out":1}` {
					t.Errorf("%+v", r)
				}
			}},
		{"criteria ok", "criteria",
			internalgrpc.LocalResult{TriesUsed: 1, Result: internalgrpc.CalloutResult{Matches: true, Reason: "big"}},
			func(t *testing.T, r DispatchCalloutResponse) {
				if r.Outcome != OutcomeOK || r.Matches == nil || !*r.Matches || r.Reason != "big" {
					t.Errorf("%+v", r)
				}
			}},
		{"function ok", "function",
			internalgrpc.LocalResult{TriesUsed: 2, Result: internalgrpc.CalloutResult{Function: contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"fireAfterMs":1}`)}},
				Attempts: []contract.CalloutAttempt{{MemberID: "m1", Kind: contract.NoAnswer, Cause: "c"}}},
			func(t *testing.T, r DispatchCalloutResponse) {
				if r.Outcome != OutcomeOK || *r.TriesUsed != 2 || r.ResultKind != "Schedule" || string(r.Result) != `{"fireAfterMs":1}` {
					t.Errorf("%+v", r)
				}
				if len(r.Attempts) != 1 || r.Attempts[0] != (WireAttempt{MemberID: "m1", Kind: "no_answer", Cause: "c"}) {
					t.Errorf("Attempts = %+v", r.Attempts)
				}
			}},
		{"no matching cnode", "processor",
			internalgrpc.LocalResult{Failure: &contract.CalloutFailure{Kind: contract.NoHandOff, Code: common.ErrCodeNoComputeMemberForTag,
				Err: fmt.Errorf("%w: tags %q", contract.ErrNoMatchingMember, "python")}},
			func(t *testing.T, r DispatchCalloutResponse) {
				if r.Outcome != "no_handoff" || *r.TriesUsed != 0 || r.ErrorCode != common.ErrCodeNoComputeMemberForTag ||
					r.ErrorStatus != http.StatusServiceUnavailable || !r.ErrorRetryable {
					t.Errorf("%+v", r)
				}
			}},
		{"no answer", "processor",
			internalgrpc.LocalResult{TriesUsed: 1, Failure: timeout,
				Attempts: []contract.CalloutAttempt{{MemberID: "m1", Kind: contract.NoAnswer, Cause: timeout.Message}}},
			func(t *testing.T, r DispatchCalloutResponse) {
				if r.Outcome != "no_answer" || r.ErrorCode != common.ErrCodeDispatchTimeout || r.ErrorStatus != 503 || !r.ErrorRetryable {
					t.Errorf("%+v", r)
				}
			}},
		{"terminal, auth context", "processor",
			internalgrpc.LocalResult{TriesUsed: 1, Failure: &contract.CalloutFailure{Kind: contract.Terminal,
				Message: "auth context unavailable for dispatch", Err: fmt.Errorf("attach: %w", contract.ErrAuthContextUnavailable)}},
			func(t *testing.T, r DispatchCalloutResponse) {
				if r.Outcome != "terminal" || r.ErrorStatus != http.StatusInternalServerError || r.ErrorCode != common.ErrCodeServerError {
					t.Errorf("%+v", r)
				}
			}},
		{"terminal, plain", "processor",
			internalgrpc.LocalResult{TriesUsed: 1, Failure: &contract.CalloutFailure{Kind: contract.Terminal, Message: "bad payload", Err: errors.New("bad payload")},
				Attempts: []contract.CalloutAttempt{{MemberID: "m1", Kind: contract.Terminal, Cause: "bad payload"}}},
			func(t *testing.T, r DispatchCalloutResponse) {
				if r.Outcome != "terminal" || r.ErrorCode != "" || r.ErrorStatus != 0 {
					t.Errorf("%+v", r)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, responseFromLocal(ownerCallout(t, tt.kind), tt.res, []string{"w1"}, nil))
		})
	}
}

func TestResponseFromLocal_MemberFailed(t *testing.T) {
	yes := true
	res := internalgrpc.LocalResult{TriesUsed: 1,
		Failure:  &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", Retryable: &yes},
		Attempts: []contract.CalloutAttempt{{MemberID: "m1", Kind: contract.MemberFailed, Cause: "card declined"}}}
	r := responseFromLocal(ownerCallout(t, "processor"), res, []string{"processor p: slow"}, []string{"processor p: card declined"})
	if r.Outcome != "member_failed" || r.MemberError != "card declined" || r.MemberRetryable == nil || !*r.MemberRetryable {
		t.Errorf("%+v", r)
	}
	if r.ErrorCode != "" {
		t.Errorf("a cnode's own failure carries no code of this pnode's: %q", r.ErrorCode)
	}
	if len(r.Warnings) != 1 || len(r.Errors) != 1 {
		t.Errorf("diagnostics lost: %+v / %+v", r.Warnings, r.Errors)
	}
}

// The peer's context ended: the owner hung up, nobody reads the answer. It is
// still a well-formed one.
func TestResponseFromLocal_CallerWentAway(t *testing.T) {
	r := responseFromLocal(ownerCallout(t, "processor"), internalgrpc.LocalResult{TriesUsed: 1, CtxErr: errors.New("context canceled")}, nil, nil)
	if r.Outcome != "no_answer" || *r.TriesUsed != 1 {
		t.Errorf("%+v", r)
	}
}

func TestReadAnswer_OK(t *testing.T) {
	yes := true
	t.Run("processor", func(t *testing.T) {
		call := ownerCallout(t, "processor")
		a := readAnswer(call, &DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1), EntityData: []byte(`{"out":1}`), Warnings: []string{"w"}}, 3)
		if !a.Connected || a.Failure != nil || a.TriesUsed != 1 || a.Result == nil ||
			string(a.Result.Entity.Data) != `{"out":1}` || a.Result.Entity.Meta.ID != call.Source.Entity.Meta.ID {
			t.Errorf("%+v", a)
		}
		if len(a.Warnings) != 1 {
			t.Errorf("Warnings = %v", a.Warnings)
		}
	})
	t.Run("criteria", func(t *testing.T) {
		a := readAnswer(ownerCallout(t, "criteria"), &DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1), Matches: &yes, Reason: "big"}, 3)
		if a.Failure != nil || !a.Result.Matches || a.Result.Reason != "big" {
			t.Errorf("%+v", a)
		}
	})
	t.Run("function", func(t *testing.T) {
		a := readAnswer(ownerCallout(t, "function"), &DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1), ResultKind: "Schedule", Result: json.RawMessage(`{}`)}, 3)
		if a.Failure != nil || a.Result.Function.Kind != "Schedule" {
			t.Errorf("%+v", a)
		}
	})
}

func TestReadAnswer_TwoTriesInOneExchange(t *testing.T) {
	a := readAnswer(ownerCallout(t, "criteria"), &DispatchCalloutResponse{
		Outcome: OutcomeOK, TriesUsed: intPtr(2), Matches: new(bool),
		Attempts: []WireAttempt{{MemberID: "m1", Kind: "no_answer", Cause: "DISPATCH_TIMEOUT: criteria dispatch timed out after 1500ms: no response"}},
	}, 3)
	if a.Failure != nil || a.TriesUsed != 2 || len(a.Attempts) != 1 || a.Attempts[0].MemberID != "m1" || a.Attempts[0].Kind != contract.NoAnswer {
		t.Errorf("%+v", a)
	}
}

func TestReadAnswer_MemberFailed(t *testing.T) {
	no := false
	a := readAnswer(ownerCallout(t, "processor"), &DispatchCalloutResponse{
		Outcome: "member_failed", TriesUsed: intPtr(1), MemberError: "card declined", MemberRetryable: &no,
		Attempts: []WireAttempt{{MemberID: "m1", Kind: "member_failed", Cause: "card declined"}},
	}, 3)
	if a.Failure == nil || a.Failure.Kind != contract.MemberFailed || a.Failure.Message != "card declined" ||
		a.Failure.Retryable == nil || *a.Failure.Retryable || a.Failure.Err != nil || a.Failure.Code != "" {
		t.Errorf("Failure = %+v", a.Failure)
	}
	if !a.Connected || a.TriesUsed != 1 {
		t.Errorf("%+v", a)
	}
}

func TestReadAnswer_ClassifiedFailures(t *testing.T) {
	cause := "DISPATCH_TIMEOUT: processor dispatch timed out after 1500ms: no response"
	tests := []struct {
		name       string
		resp       DispatchCalloutResponse
		wantKind   contract.CalloutFailureKind
		wantCode   string
		wantStatus int
		wantRetry  bool
		wantMsg    string
		wantTries  int
		wantConn   bool
	}{
		{"no answer keeps the try's own code and text",
			DispatchCalloutResponse{Outcome: "no_answer", TriesUsed: intPtr(1), ErrorCode: common.ErrCodeDispatchTimeout, ErrorStatus: 503, ErrorRetryable: true,
				Attempts: []WireAttempt{{MemberID: "m1", Kind: "no_answer", Cause: cause}}},
			contract.NoAnswer, common.ErrCodeDispatchTimeout, 503, true, cause, 1, true},
		{"no matching cnode on the peer: nothing connected to a cnode, no try",
			DispatchCalloutResponse{Outcome: "no_handoff", TriesUsed: intPtr(0), ErrorCode: common.ErrCodeNoComputeMemberForTag, ErrorStatus: 503, ErrorRetryable: true},
			contract.NoHandOff, common.ErrCodeNoComputeMemberForTag, 503, true, "", 0, false},
		{"the peer tried two cnodes and handed off to neither",
			DispatchCalloutResponse{Outcome: "no_handoff", TriesUsed: intPtr(2), ErrorCode: common.ErrCodeComputeMemberDisconnected, ErrorStatus: 503, ErrorRetryable: true},
			contract.NoHandOff, common.ErrCodeComputeMemberDisconnected, 503, true, "", 2, true},
		{"terminal with a ticketed 500",
			DispatchCalloutResponse{Outcome: "terminal", TriesUsed: intPtr(1), ErrorCode: common.ErrCodeServerError, ErrorStatus: 500},
			contract.Terminal, common.ErrCodeServerError, 500, false, "", 1, true},
		{"terminal refused before any try",
			DispatchCalloutResponse{Outcome: "terminal", TriesUsed: intPtr(0), ErrorCode: common.ErrCodeServerError, ErrorStatus: 500},
			contract.Terminal, common.ErrCodeServerError, 500, false, "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := tt.resp
			a := readAnswer(ownerCallout(t, "processor"), &resp, 3)
			if a.Failure == nil || a.Failure.Kind != tt.wantKind || a.Failure.Code != tt.wantCode {
				t.Fatalf("Failure = %+v", a.Failure)
			}
			var appErr *common.AppError
			if !errors.As(a.Failure, &appErr) || appErr.Status != tt.wantStatus || appErr.Retryable != tt.wantRetry || appErr.Code != tt.wantCode {
				t.Errorf("AppError = %+v", appErr)
			}
			if tt.wantMsg != "" && a.Failure.Message != tt.wantMsg {
				t.Errorf("Message = %q, want %q (the code must not be prefixed twice)", a.Failure.Message, tt.wantMsg)
			}
			if a.TriesUsed != tt.wantTries || a.Connected != tt.wantConn {
				t.Errorf("TriesUsed/Connected = %d/%v, want %d/%v", a.TriesUsed, a.Connected, tt.wantTries, tt.wantConn)
			}
		})
	}
}

// A terminal failure with no code of its own is a plain error on the owner too,
// so that it is classified exactly as it would have been had it happened there.
func TestReadAnswer_TerminalWithoutACode(t *testing.T) {
	a := readAnswer(ownerCallout(t, "processor"), &DispatchCalloutResponse{Outcome: "terminal", TriesUsed: intPtr(1),
		Attempts: []WireAttempt{{MemberID: "m1", Kind: "terminal", Cause: "bad payload"}}}, 3)
	var appErr *common.AppError
	if a.Failure == nil || a.Failure.Kind != contract.Terminal || errors.As(a.Failure, &appErr) || a.Failure.Error() != "bad payload" {
		t.Errorf("Failure = %+v", a.Failure)
	}
}

func assertLost(t *testing.T, a HandOverAnswer) {
	t.Helper()
	if !a.Connected || a.TriesUsed != 1 || a.Result != nil {
		t.Errorf("a lost answer is connected, one try, no result: %+v", a)
	}
	if a.Failure == nil || a.Failure.Kind != contract.NoAnswer || a.Failure.Code != common.ErrCodeDispatchForwardFailed {
		t.Fatalf("Failure = %+v", a.Failure)
	}
	var appErr *common.AppError
	if !errors.As(a.Failure, &appErr) || appErr.Status != http.StatusServiceUnavailable || !appErr.Retryable {
		t.Errorf("AppError = %+v, want a retryable 503", appErr)
	}
	if a.Failure.Message != common.ErrCodeDispatchForwardFailed+": "+forwardFailedClientMessage {
		t.Errorf("Message = %q", a.Failure.Message)
	}
	if len(a.Attempts) != 1 || a.Attempts[0].MemberID != "-" || a.Attempts[0].Kind != contract.NoAnswer {
		t.Errorf("Attempts = %+v, want one attempt with member \"-\"", a.Attempts)
	}
}

func TestLostAnswer(t *testing.T) { assertLost(t, lostAnswer()) }

func TestReadAnswer_TriesUsedOutOfRange(t *testing.T) {
	for _, used := range []int{-1, 4, 1000} {
		t.Run(fmt.Sprint(used), func(t *testing.T) {
			assertLost(t, readAnswer(ownerCallout(t, "processor"),
				&DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(used), EntityData: []byte(`{}`)}, 3))
		})
	}
}

// An answer that claims a cnode answered, failed or went silent, with no try
// made, contradicts itself.
func TestReadAnswer_OutcomeThatNeedsATryWithNone(t *testing.T) {
	for _, outcome := range []string{OutcomeOK, "no_answer", "member_failed"} {
		t.Run(outcome, func(t *testing.T) {
			assertLost(t, readAnswer(ownerCallout(t, "processor"), &DispatchCalloutResponse{Outcome: outcome, TriesUsed: intPtr(0)}, 3))
		})
	}
}

// §15: a pnode of an earlier version answers without outcome and triesUsed.
func TestReadAnswer_AnswerFromAnOlderVersion(t *testing.T) {
	t.Run("no outcome", func(t *testing.T) {
		assertLost(t, readAnswer(ownerCallout(t, "processor"), &DispatchCalloutResponse{EntityData: []byte(`{}`)}, 3))
	})
	t.Run("unknown outcome", func(t *testing.T) {
		assertLost(t, readAnswer(ownerCallout(t, "processor"), &DispatchCalloutResponse{Outcome: "maybe", TriesUsed: intPtr(1)}, 3))
	})
	t.Run("no triesUsed counts as one", func(t *testing.T) {
		a := readAnswer(ownerCallout(t, "processor"), &DispatchCalloutResponse{Outcome: OutcomeOK, EntityData: []byte(`{}`)}, 3)
		if a.Failure != nil || a.TriesUsed != 1 {
			t.Errorf("%+v", a)
		}
	})
	t.Run("nil answer", func(t *testing.T) {
		assertLost(t, readAnswer(ownerCallout(t, "processor"), nil, 3))
	})
}

func TestNotConnected(t *testing.T) {
	a := notConnected()
	if a.Connected || a.TriesUsed != 0 || len(a.Attempts) != 0 || a.Failure == nil || a.Failure.Kind != contract.NoHandOff ||
		a.Failure.Code != common.ErrCodeNoComputeMemberForTag {
		t.Errorf("%+v", a)
	}
}

func TestProvedBeforeConnecting(t *testing.T) {
	a := provedBeforeConnecting(errors.New("forbidden peer address: 169.254.1.1"))
	if a.Connected || a.TriesUsed != 0 || a.Failure == nil || a.Failure.Kind != contract.Terminal {
		t.Fatalf("%+v", a)
	}
	var appErr *common.AppError
	if !errors.As(a.Failure, &appErr) || appErr.Status != http.StatusInternalServerError {
		t.Errorf("AppError = %+v, want a ticketed 500", appErr)
	}
	if strings.Contains(a.Failure.Message, "169.254") {
		t.Errorf("the client-safe message names a peer address: %q", a.Failure.Message)
	}
}
```

`types_test.go` — add, beside the existing round-trips:

```go
func TestDispatchCalloutRequest_HandOverFieldsRoundTrip(t *testing.T) {
	req := dispatch.DispatchCalloutRequest{
		Kind: "processor", RequestID: "rid", TriesLeft: 3, AnswerLimitMs: 30000, OwnerNodeID: "n1",
		Major: 4, RepeatSafe: true, Outer: []dispatch.WirePair{{Callout: "o", Major: 2, Minor: 1}},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"requestID":"rid"`, `"triesLeft":3`, `"answerLimitMs":30000`, `"ownerNodeID":"n1"`, `"major":4`, `"repeatSafe":true`, `"outer":[{"callout":"o","major":2,"minor":1}]`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("wire form lacks %s: %s", key, raw)
		}
	}
	var got dispatch.DispatchCalloutRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RequestID != "rid" || got.TriesLeft != 3 || got.Major != 4 || len(got.Outer) != 1 {
		t.Errorf("round trip lost fields: %+v", got)
	}
}

func TestDispatchCalloutResponse_TriesUsedZeroIsNotAbsent(t *testing.T) {
	zero := 0
	with, _ := json.Marshal(dispatch.DispatchCalloutResponse{Outcome: "no_handoff", TriesUsed: &zero})
	without, _ := json.Marshal(dispatch.DispatchCalloutResponse{Outcome: "no_handoff"})
	if !strings.Contains(string(with), `"triesUsed":0`) {
		t.Errorf("triesUsed 0 must be on the wire: %s", with)
	}
	if strings.Contains(string(without), "triesUsed") {
		t.Errorf("an absent triesUsed must stay absent: %s", without)
	}
}
```

(`types_test.go` gains `"strings"` if it does not import it.)

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/cluster/dispatch/... -run 'TestNewHandOverRequest|TestRequestValidate|TestToCallout|TestResponseFromLocal|TestReadAnswer|TestLostAnswer|TestNotConnected|TestProvedBeforeConnecting|TestDispatchCallout(Request_HandOver|Response_TriesUsed)'`
Expected: FAIL (build) — `undefined: newHandOverRequest`, `unknown field RequestID`.

- [ ] **Step 3: Implement**

`types.go` — in `DispatchCalloutRequest`, after `Roles`:

```go
	// RequestID is created by the owner and sent on every try, by every pnode.
	RequestID string `json:"requestID"`
	// TriesLeft is the most tries the receiving pnode may make; at least 1.
	TriesLeft int `json:"triesLeft"`
	// AnswerLimitMs is the answer limit as the owner resolved it, so that two
	// pnodes cannot disagree about it.
	AnswerLimitMs int64 `json:"answerLimitMs"`
	// OwnerNodeID is the pnode that holds the transaction; the receiving pnode
	// puts it in every pass it mints, so that callbacks are routed there.
	OwnerNodeID string `json:"ownerNodeID"`
	// Major is the fencing number of this hand-over. The receiving pnode
	// numbers its tries minor = 1, 2, … under it.
	Major uint32 `json:"major"`
	// Outer names the enclosing callouts, copied into every pass minted; empty
	// unless the callout was made from inside a callback.
	Outer []WirePair `json:"outer,omitempty"`
	// RepeatSafe is the owner's decision whether the work may be given to
	// another cnode after a hand-off.
	RepeatSafe bool `json:"repeatSafe"`
```

In `DispatchCalloutResponse`, before `Success`:

```go
	// Outcome is "ok", or the kind of the failure: "no_handoff", "no_answer",
	// "member_failed", "terminal". Only an authenticated "no_handoff" tells the
	// owner that nothing was handed to a cnode.
	Outcome string `json:"outcome"`
	// TriesUsed is the number of tries made. A pointer, because absent (read
	// as one try) and zero (nothing was tried) are different answers.
	TriesUsed *int `json:"triesUsed,omitempty"`
	// Attempts is one entry per failed try.
	Attempts []WireAttempt `json:"attempts,omitempty"`
	// MemberError and MemberRetryable are the cnode's own message and verdict
	// ("member_failed" only).
	MemberError     string `json:"memberError,omitempty"`
	MemberRetryable *bool  `json:"memberRetryable,omitempty"`
```

after `Warnings`:

```go
	// Errors are the diagnostics the tries added on the answering pnode.
	Errors []string `json:"errors,omitempty"`
```

and at the end of the file:

```go
// OutcomeOK is the outcome of a hand-over that a cnode answered. The other
// outcomes are the words of contract.CalloutFailureKind.
const OutcomeOK = "ok"

// WirePair is one (callout, major, minor) on the wire.
type WirePair struct {
	Callout string `json:"callout"`
	Major   uint32 `json:"major"`
	Minor   uint32 `json:"minor"`
}

// WireAttempt is one failed try on the wire.
type WireAttempt struct {
	MemberID string `json:"memberID"`
	Kind     string `json:"kind"`
	Cause    string `json:"cause"`
}
```

`internal/cluster/dispatch/handover.go`:

```go
package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// forwardFailedClientMessage is the sanitized, client-facing message for a
// hand-over whose answer was lost. The transport error behind it embeds the
// peer's address, port and route — that detail must never reach the client; it
// is logged where the hand-over is made.
const forwardFailedClientMessage = "forwarding the callout to a peer node failed"

// peerFailedClientMessage stands in where the answering pnode gave no
// client-safe text of its own.
const peerFailedClientMessage = "peer node dispatch failed"

// HandOverAnswer is what the owner learns from one hand-over.
type HandOverAnswer struct {
	// Connected is false when the connection to the peer could not be opened
	// (a dial error, the connect timeout included), when the hand-over was
	// refused before connecting, or when the peer sent an authenticated
	// no_handoff having tried no cnode: nothing was handed to a cnode and no
	// try is used.
	Connected bool
	// Result is set when the outcome is ok.
	Result *internalgrpc.CalloutResult
	// Failure is nil when the outcome is ok. Its Kind is NoHandOff, NoAnswer,
	// MemberFailed or Terminal. A lost answer is NoAnswer with code
	// DISPATCH_FORWARD_FAILED and the sanitised message.
	Failure *contract.CalloutFailure
	// TriesUsed is 0 when Connected is false, and for a Terminal refusal made
	// before any try; otherwise 1..triesLeft. A lost or out-of-range answer
	// counts as exactly 1.
	TriesUsed int
	Attempts  []contract.CalloutAttempt
	Warnings  []string

	// peerErrors are the answering pnode's error diagnostics; HandOver adds
	// them to the owner's request diagnostics.
	peerErrors []string
}

// newHandOverRequest puts call on the wire for a peer that may make triesLeft
// tries under fencing number major. An error means the callout cannot be handed
// over at all — to any peer.
func newHandOverRequest(uc *spi.UserContext, ownerNodeID string, call internalgrpc.Callout, triesLeft int, major uint32) (DispatchCalloutRequest, error) {
	src := call.Source
	switch {
	case uc == nil:
		return DispatchCalloutRequest{}, errors.New("no user context")
	case src.Entity == nil:
		return DispatchCalloutRequest{}, errors.New("callout has no source")
	case call.RequestID == "":
		return DispatchCalloutRequest{}, errors.New("callout has no request id")
	case call.AnswerLimit < time.Millisecond:
		return DispatchCalloutRequest{}, fmt.Errorf("answer limit %s is below one millisecond", call.AnswerLimit)
	case triesLeft < 1:
		return DispatchCalloutRequest{}, fmt.Errorf("tries left is %d", triesLeft)
	case major < 1:
		return DispatchCalloutRequest{}, errors.New("hand-over has no fencing number")
	}
	req := DispatchCalloutRequest{
		Kind:           call.Kind.String(),
		Entity:         json.RawMessage(src.Entity.Data),
		EntityMeta:     src.Entity.Meta,
		WorkflowName:   src.WorkflowName,
		TransitionName: src.TransitionName,
		TxID:           call.TxID,
		TenantID:       string(call.TenantID),
		Tags:           call.Tags,
		UserID:         uc.UserID,
		PrincipalKind:  uc.Kind,
		Roles:          uc.Roles,
		RequestID:      call.RequestID,
		TriesLeft:      triesLeft,
		AnswerLimitMs:  call.AnswerLimit.Milliseconds(),
		OwnerNodeID:    ownerNodeID,
		Major:          major,
		RepeatSafe:     call.RepeatSafe,
		Processor:      src.Processor,
		Criterion:      src.Criterion,
		Target:         src.Target,
		ProcessorName:  src.ProcessorName,
		Function:       src.Function,
	}
	for _, p := range call.Outer {
		req.Outer = append(req.Outer, WirePair{Callout: p.Callout, Major: p.Major, Minor: p.Minor})
	}
	if err := req.validate(); err != nil {
		return DispatchCalloutRequest{}, err
	}
	return req, nil
}

// validate is the receiving pnode's check of a hand-over it has authenticated.
//
// A request carries two tenants: TenantID, which becomes the UserContext the
// callout runs as, and EntityMeta.TenantID, the entity's own. They must agree,
// or the callout runs as one tenant over another's entity. The equality is
// unconditional, an absent EntityMeta.TenantID included: every callout is built
// from a live stored entity whose tenant is always set, so an empty one can
// only come from a hand-crafted body. The error names neither value: both are
// peer-supplied.
func (req *DispatchCalloutRequest) validate() error {
	switch {
	case string(req.EntityMeta.TenantID) != req.TenantID:
		return errors.New("entity tenant does not match request tenant")
	case req.RequestID == "":
		return errors.New("requestID is empty")
	case req.TriesLeft < 1:
		return errors.New("triesLeft is below 1")
	case req.AnswerLimitMs < 1:
		return errors.New("answerLimitMs is below 1")
	case req.OwnerNodeID == "":
		return errors.New("ownerNodeID is empty")
	case req.Major < 1:
		return errors.New("major is below 1")
	}
	switch req.Kind {
	case internalgrpc.ProcessorCallout.String():
		if req.Processor == nil {
			return errors.New("processor callout without a processor")
		}
	case internalgrpc.CriteriaCallout.String():
		if len(req.Criterion) == 0 {
			return errors.New("criteria callout without a criterion")
		}
	case internalgrpc.FunctionCallout.String():
		if req.Function == nil {
			return errors.New("function callout without a function")
		}
	default:
		return errors.New("unknown callout kind")
	}
	return nil
}

// toCallout builds, on the receiving pnode, the Callout the owner built — with
// the same builder — and fills what the hand-over carried: the request id, the
// answer limit, whether it is repeat-safe, the owner's id for the passes, the
// enclosing pairs, and a numberer that counts minor = 1, 2, … under the
// hand-over's major. The request must have passed validate.
func (req *DispatchCalloutRequest) toCallout() (internalgrpc.Callout, *contract.CalloutFailure) {
	entity := &spi.Entity{Meta: req.EntityMeta, Data: []byte(req.Entity)}
	tenant := spi.TenantID(req.TenantID)

	var call internalgrpc.Callout
	switch req.Kind {
	case internalgrpc.ProcessorCallout.String():
		call = internalgrpc.NewProcessorCallout(tenant, entity, *req.Processor, req.WorkflowName, req.TransitionName, req.TxID)
	case internalgrpc.CriteriaCallout.String():
		var failure *contract.CalloutFailure
		call, failure = internalgrpc.NewCriteriaCallout(tenant, entity, req.Criterion, req.Target, req.WorkflowName, req.TransitionName, req.ProcessorName, req.TxID)
		if failure != nil {
			return internalgrpc.Callout{}, failure
		}
	default:
		call = internalgrpc.NewFunctionCallout(tenant, entity, *req.Function, req.WorkflowName, req.TransitionName, req.TxID)
	}
	call.RequestID = req.RequestID
	call.AnswerLimit = time.Duration(req.AnswerLimitMs) * time.Millisecond
	call.RepeatSafe = req.RepeatSafe
	call.OwnerNodeID = req.OwnerNodeID
	call.Number = internalgrpc.NewMinorNumberer(req.Major)
	for _, p := range req.Outer {
		call.Outer = append(call.Outer, token.Pair{Callout: p.Callout, Major: p.Major, Minor: p.Minor})
	}
	return call, nil
}

// responseFromLocal is the answer to a hand-over: what the local procedure
// produced, in the words of the wire.
func responseFromLocal(call internalgrpc.Callout, res internalgrpc.LocalResult, warnings, errs []string) DispatchCalloutResponse {
	used := res.TriesUsed
	resp := DispatchCalloutResponse{TriesUsed: &used, Warnings: warnings, Errors: errs}
	for _, a := range res.Attempts {
		resp.Attempts = append(resp.Attempts, WireAttempt{MemberID: a.MemberID, Kind: a.Kind.String(), Cause: a.Cause})
	}
	switch {
	case res.CtxErr != nil:
		// The owner hung up. Nobody reads this; it is well-formed all the same.
		resp.Outcome = contract.NoAnswer.String()
	case res.Failure != nil:
		fillFailure(&resp, res.Failure)
	default:
		resp.Outcome = OutcomeOK
		switch call.Kind {
		case internalgrpc.ProcessorCallout:
			if res.Result.Entity != nil {
				resp.EntityData = res.Result.Entity.Data
			}
		case internalgrpc.CriteriaCallout:
			matches := res.Result.Matches
			resp.Matches = &matches
			resp.Reason = res.Result.Reason
		case internalgrpc.FunctionCallout:
			resp.Result = res.Result.Function.Value
			resp.ResultKind = res.Result.Function.Kind
		}
	}
	return resp
}

// refusal is the answer of a pnode that tried no cnode.
func refusal(failure *contract.CalloutFailure) DispatchCalloutResponse {
	zero := 0
	resp := DispatchCalloutResponse{TriesUsed: &zero}
	fillFailure(&resp, failure)
	return resp
}

// fillFailure states a failure in the words of the wire. A classified error
// travels as its code, status and retryable flag; its client-safe text travels
// in the attempts. A cnode's own failure travels as its message and verdict.
func fillFailure(resp *DispatchCalloutResponse, failure *contract.CalloutFailure) {
	resp.Outcome = failure.Kind.String()
	if failure.Kind == contract.MemberFailed {
		resp.MemberError = failure.Message
		resp.MemberRetryable = failure.Retryable
		return
	}
	var appErr *common.AppError
	switch {
	case errors.As(failure, &appErr):
		resp.ErrorCode = appErr.Code
		resp.ErrorStatus = appErr.Status
		resp.ErrorRetryable = appErr.Retryable
	case errors.Is(failure, contract.ErrNoMatchingMember):
		// The same trio a single pnode answers for "no cnode".
		resp.ErrorCode = common.ErrCodeNoComputeMemberForTag
		resp.ErrorStatus = http.StatusServiceUnavailable
		resp.ErrorRetryable = true
	case errors.Is(failure, contract.ErrAuthContextUnavailable):
		// A ticketed 500 where it happens locally; the same on the owner.
		resp.ErrorCode = common.ErrCodeServerError
		resp.ErrorStatus = http.StatusInternalServerError
	}
}

// readAnswer reads a decoded, authenticated answer. Only an outcome of
// no_handoff says that nothing was handed to a cnode; whatever is not ok,
// member_failed or terminal either is no_answer. An answer without triesUsed
// counts as one try, never zero, so the owner's loop always makes progress; one
// whose triesUsed is outside 0..triesLeft is not believed at all.
func readAnswer(call internalgrpc.Callout, resp *DispatchCalloutResponse, triesLeft int) HandOverAnswer {
	if resp == nil {
		return lostAnswer()
	}
	used := 1
	if resp.TriesUsed != nil {
		used = *resp.TriesUsed
	}
	if used < 0 || used > triesLeft {
		return lostAnswer()
	}
	ans := HandOverAnswer{Connected: true, TriesUsed: used, Warnings: resp.Warnings, peerErrors: resp.Errors}
	for _, a := range resp.Attempts {
		ans.Attempts = append(ans.Attempts, contract.CalloutAttempt{MemberID: a.MemberID, Kind: kindFromWire(a.Kind), Cause: a.Cause})
	}

	switch resp.Outcome {
	case OutcomeOK:
		if used == 0 {
			return lostAnswer()
		}
		result := internalgrpc.CalloutResult{}
		switch call.Kind {
		case internalgrpc.ProcessorCallout:
			result.Entity = &spi.Entity{Meta: call.Source.Entity.Meta, Data: resp.EntityData}
		case internalgrpc.CriteriaCallout:
			result.Matches = resp.Matches != nil && *resp.Matches
			result.Reason = resp.Reason
		case internalgrpc.FunctionCallout:
			result.Function = contract.FunctionResult{Kind: resp.ResultKind, Value: resp.Result}
		}
		ans.Result = &result
	case contract.NoHandOff.String():
		ans.Connected = used > 0
		ans.Failure = classifiedFailure(contract.NoHandOff, resp, ans.Attempts)
	case contract.NoAnswer.String():
		if used == 0 {
			return lostAnswer()
		}
		ans.Failure = classifiedFailure(contract.NoAnswer, resp, ans.Attempts)
	case contract.MemberFailed.String():
		if used == 0 {
			return lostAnswer()
		}
		msg := resp.MemberError
		if msg == "" {
			msg = peerFailedClientMessage
		}
		ans.Failure = &contract.CalloutFailure{Kind: contract.MemberFailed, Message: msg, Retryable: resp.MemberRetryable}
	case contract.Terminal.String():
		ans.Failure = classifiedFailure(contract.Terminal, resp, ans.Attempts)
	default:
		return lostAnswer()
	}
	return ans
}

// classifiedFailure re-mints, on the owner, the error the answering pnode
// classified: the same code, status and retryable flag, with the client-safe
// text of the last try.
func classifiedFailure(kind contract.CalloutFailureKind, resp *DispatchCalloutResponse, attempts []contract.CalloutAttempt) *contract.CalloutFailure {
	text := peerFailedClientMessage
	if n := len(attempts); n > 0 && attempts[n-1].Cause != "" {
		text = attempts[n-1].Cause
	}
	if resp.ErrorCode == "" {
		switch kind {
		case contract.NoHandOff:
			return noCnodeFailure()
		case contract.NoAnswer:
			return lostAnswer().Failure
		default:
			err := errors.New(text)
			return &contract.CalloutFailure{Kind: kind, Message: text, Err: err}
		}
	}
	// A classified error's text already starts with its code.
	text = strings.TrimPrefix(text, resp.ErrorCode+": ")
	var appErr *common.AppError
	switch {
	case resp.ErrorStatus == http.StatusInternalServerError:
		appErr = common.InternalWithCode(resp.ErrorCode, text, nil)
	case resp.ErrorStatus >= 400 && resp.ErrorStatus <= 599:
		appErr = common.Operational(resp.ErrorStatus, resp.ErrorCode, text)
	default:
		appErr = common.Operational(http.StatusServiceUnavailable, resp.ErrorCode, text)
	}
	if resp.ErrorRetryable {
		appErr = appErr.AsRetryable()
	}
	return &contract.CalloutFailure{Kind: kind, Code: appErr.Code, Message: appErr.Message, Err: appErr}
}

func kindFromWire(s string) contract.CalloutFailureKind {
	for _, k := range []contract.CalloutFailureKind{contract.NoHandOff, contract.NoAnswer, contract.MemberFailed, contract.Terminal} {
		if k.String() == s {
			return k
		}
	}
	// An attempt of a kind this version does not know: the careful reading.
	return contract.NoAnswer
}

func noCnodeFailure() *contract.CalloutFailure {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeNoComputeMemberForTag,
		"no compute member took the callout on the peer node").AsRetryable()
	return &contract.CalloutFailure{Kind: contract.NoHandOff, Code: appErr.Code, Message: appErr.Message, Err: appErr}
}

// lostAnswer is a hand-over whose answer never arrived, did not authenticate,
// or cannot be believed. The peer may have handed the work to a cnode — to more
// than one — so it counts as one try and is NoAnswer.
func lostAnswer() HandOverAnswer {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchForwardFailed, forwardFailedClientMessage).AsRetryable()
	return HandOverAnswer{
		Connected: true,
		TriesUsed: 1,
		Failure:   &contract.CalloutFailure{Kind: contract.NoAnswer, Code: appErr.Code, Message: appErr.Message, Err: appErr},
		Attempts:  []contract.CalloutAttempt{{MemberID: "-", Kind: contract.NoAnswer, Cause: forwardFailedClientMessage}},
	}
}

// notConnected is a peer the connection to which could not be opened: nothing
// left this pnode, and no try is used.
func notConnected() HandOverAnswer {
	return HandOverAnswer{Failure: noCnodeFailure()}
}

// provedBeforeConnecting is a hand-over that failed before any connection was
// attempted — an address that fails validation, a request that cannot be built,
// marshalled or signed. It would fail identically on every try: Terminal.
func provedBeforeConnecting(err error) HandOverAnswer {
	appErr := common.Internal("the callout could not be handed over", err)
	return HandOverAnswer{Failure: &contract.CalloutFailure{Kind: contract.Terminal, Code: appErr.Code, Message: appErr.Message, Err: appErr}}
}
```

`cluster_dispatcher.go` — delete the `forwardFailedClientMessage` constant and
its comment from the `const` block (`:22-28`); it now lives in `handover.go`.
Exit check: `grep -rn 'forwardFailedClientMessage =' internal/` → one hit.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/cluster/dispatch/...`, `go vet ./internal/cluster/dispatch/...`   Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/cluster/dispatch/types.go internal/cluster/dispatch/types_test.go internal/cluster/dispatch/handover.go internal/cluster/dispatch/handover_test.go internal/cluster/dispatch/fixtures_test.go internal/cluster/dispatch/cluster_dispatcher.go internal/cluster/dispatch/cluster_dispatcher_test.go
git commit -m "feat(dispatch): the hand-over's fields, and how each pnode reads them (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task P-5: `PeerRouter` — `Peers`, `HandOver`, `Changed`; the hand-over counter

**Spec:** §6 "How the owner reads an answer" (everything that is *not* a decoded answer: a connection that could not be opened; a transport error after it was; non-2xx; truncated; an answer that does not open) and "What the owner can prove *before* it connects"; §5 "for each alive peer advertising the tag, in selector order"; §12 "hand-overs by outcome" (V-3, settled above).

**Files:**
- Create: `internal/cluster/dispatch/peer_router.go`
- Modify: `internal/cluster/dispatch/forwarder.go` (`ForwardCallout` :57-66, `forward`; new `ForwardError`, `ForwardStage`, `isDialError`)
- Modify: `cmd/cyoda/help/content/telemetry.md` (**Metrics**, after the `cyoda.dispatch.count` line, `:89`; label list `:132`)
- Test: `internal/cluster/dispatch/peer_router_test.go` (new, `package dispatch`)

**Interfaces:**
- Consumes: P-4; M-5 `contract.NodeRegistry.Changed() <-chan struct{}` and `common.NewChangeSignal()` (M-1); `observability.Meter()` at the wiring site (P-6).
- Produces — the seam, exactly:
  - `func NewPeerRouter(registry contract.NodeRegistry, selfNodeID string, selector PeerSelector, forwarder DispatchForwarder, meter metric.Meter) (*PeerRouter, error)` — a nil `meter` is a no-op meter.
  - `func (r *PeerRouter) Peers(tenantID, tagsCSV string) []contract.NodeInfo`
  - `func (r *PeerRouter) HandOver(ctx context.Context, peer contract.NodeInfo, call internalgrpc.Callout, triesLeft int, major uint32) HandOverAnswer`
  - `func (r *PeerRouter) Changed() <-chan struct{}`
  - `type ForwardStage int` (`StageBeforeConnect`, `StageNotConnected`, `StageAfterConnect`); `type ForwardError struct{ Stage ForwardStage; Err error }` — every error `HTTPForwarder.ForwardCallout` returns is one. A `DispatchForwarder` that returns any other error is read as `StageAfterConnect`: the careful reading.
  - Instrument `cyoda.dispatch.handovers` — `Int64Counter`, attribute `outcome` = `ok` | `no_handoff` | `no_answer` | `member_failed` | `terminal` | `not_connected`.

`HandOver` sends the owner's id from the router's own `selfNodeID`, not from
`call.OwnerNodeID`: the pnode that hands over *is* the owner.

- [ ] **Step 1: Write the failing tests**

`internal/cluster/dispatch/peer_router_test.go`:

```go
package dispatch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// inOrderSelector picks the first candidate, so "selector order" is list order.
type inOrderSelector struct{}

func (inOrderSelector) Select(c []contract.NodeInfo) (contract.NodeInfo, error) {
	if len(c) == 0 {
		return contract.NodeInfo{}, errors.New("no candidates")
	}
	return c[0], nil
}

// answeringForwarder answers every hand-over with resp/err and keeps the request.
type answeringForwarder struct {
	resp  *DispatchCalloutResponse
	err   error
	got   DispatchCalloutRequest
	calls int
}

func (f *answeringForwarder) ForwardCallout(_ context.Context, _ string, req DispatchCalloutRequest) (*DispatchCalloutResponse, error) {
	f.calls++
	f.got = req
	return f.resp, f.err
}

func newTestRouter(t *testing.T, registry contract.NodeRegistry, fwd DispatchForwarder) *PeerRouter {
	t.Helper()
	r, err := NewPeerRouter(registry, "self-node", inOrderSelector{}, fwd, nil)
	if err != nil {
		t.Fatalf("NewPeerRouter: %v", err)
	}
	return r
}

func node(id string, alive bool, tenant string, tags ...string) contract.NodeInfo {
	return contract.NodeInfo{NodeID: id, Addr: "http://" + id, Alive: alive, Tags: map[string][]string{tenant: tags}}
}

func TestPeerRouter_Peers(t *testing.T) {
	registry := &stubNodeRegistry{nodes: []contract.NodeInfo{
		node("self-node", true, "tenant-1", "python"),
		node("dead", false, "tenant-1", "python"),
		node("other-tenant", true, "tenant-2", "python"),
		node("other-tag", true, "tenant-1", "java"),
		node("peer-b", true, "tenant-1", "python", "ml"),
		node("peer-a", true, "tenant-1", "python"),
		{NodeID: "no-list-yet", Addr: "http://x", Alive: true, Tags: map[string][]string{}},
	}}
	got := newTestRouter(t, registry, &answeringForwarder{}).Peers("tenant-1", "python,go")
	if len(got) != 2 || got[0].NodeID != "peer-b" || got[1].NodeID != "peer-a" {
		t.Fatalf("Peers = %+v, want [peer-b peer-a] — alive, not self, this tenant, any tag overlapping, in selector order", got)
	}
}

type failingRegistry struct{ stubNodeRegistry }

func (failingRegistry) List(context.Context) ([]contract.NodeInfo, error) {
	return nil, errors.New("registry down")
}

func TestPeerRouter_Peers_RegistryErrorIsNoPeers(t *testing.T) {
	if got := newTestRouter(t, &failingRegistry{}, &answeringForwarder{}).Peers("tenant-1", "python"); len(got) != 0 {
		t.Fatalf("Peers = %+v, want none", got)
	}
}

type signalRegistry struct {
	stubNodeRegistry
	sig *common.ChangeSignal
}

func (r *signalRegistry) Changed() <-chan struct{} { return r.sig.Changed() }

func TestPeerRouter_Changed_IsTheNodeRegistrys(t *testing.T) {
	registry := &signalRegistry{sig: common.NewChangeSignal()}
	ch := newTestRouter(t, registry, &answeringForwarder{}).Changed()
	select {
	case <-ch:
		t.Fatal("closed before any change")
	default:
	}
	registry.sig.Fire()
	select {
	case <-ch:
	default:
		t.Fatal("not closed by the registry's change")
	}
}

func TestHandOver_Classification(t *testing.T) {
	okResp := &DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1), EntityData: []byte(`{"out":1}`)}
	tests := []struct {
		name  string
		resp  *DispatchCalloutResponse
		err   error
		check func(t *testing.T, a HandOverAnswer)
	}{
		{"answered", okResp, nil, func(t *testing.T, a HandOverAnswer) {
			if a.Failure != nil || !a.Connected || a.TriesUsed != 1 || string(a.Result.Entity.Data) != `{"out":1}` {
				t.Errorf("%+v", a)
			}
		}},
		{"lost: an error of no known stage", nil, errors.New("read tcp 10.0.0.5:8080: connection reset"), assertLost},
		{"lost: after the connection was opened", nil, &ForwardError{Stage: StageAfterConnect, Err: errors.New("peer returned 502")}, assertLost},
		{"not connected: no try", nil, &ForwardError{Stage: StageNotConnected, Err: errors.New("dial tcp: connection refused")}, func(t *testing.T, a HandOverAnswer) {
			if a.Connected || a.TriesUsed != 0 || a.Failure == nil || a.Failure.Kind != contract.NoHandOff || len(a.Attempts) != 0 {
				t.Errorf("%+v", a)
			}
		}},
		{"proved before connecting: terminal", nil, &ForwardError{Stage: StageBeforeConnect, Err: ErrForbiddenPeerAddress}, func(t *testing.T, a HandOverAnswer) {
			if a.Connected || a.TriesUsed != 0 || a.Failure == nil || a.Failure.Kind != contract.Terminal {
				t.Errorf("%+v", a)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fwd := &answeringForwarder{resp: tt.resp, err: tt.err}
			a := newTestRouter(t, &stubNodeRegistry{}, fwd).HandOver(testContext(), node("peer-1", true, "tenant-1", "python"), ownerCallout(t, "processor"), 3, 2)
			tt.check(t, a)
			if fwd.calls != 1 {
				t.Errorf("forwarder called %d times", fwd.calls)
			}
		})
	}
}

func TestHandOver_SendsTheHandOverFields(t *testing.T) {
	fwd := &answeringForwarder{resp: &DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1)}}
	call := ownerCallout(t, "criteria")
	call.OwnerNodeID = "" // the router knows who the owner is
	newTestRouter(t, &stubNodeRegistry{}, fwd).HandOver(testContext(), node("peer-1", true, "tenant-1", "python"), call, 3, 2)
	if fwd.got.TriesLeft != 3 || fwd.got.Major != 2 || fwd.got.OwnerNodeID != "self-node" || fwd.got.RequestID != "rid-1" || fwd.got.AnswerLimitMs != 1500 {
		t.Errorf("request = %+v", fwd.got)
	}
}

func TestHandOver_NothingToHandOver_IsTerminal_NothingSent(t *testing.T) {
	fwd := &answeringForwarder{}
	router := newTestRouter(t, &stubNodeRegistry{}, fwd)
	peer := node("peer-1", true, "tenant-1", "python")

	noUser := router.HandOver(context.Background(), peer, ownerCallout(t, "processor"), 3, 2)
	noTries := router.HandOver(testContext(), peer, ownerCallout(t, "processor"), 0, 2)
	for name, a := range map[string]HandOverAnswer{"no user context": noUser, "no tries left": noTries} {
		if a.Connected || a.TriesUsed != 0 || a.Failure == nil || a.Failure.Kind != contract.Terminal {
			t.Errorf("%s: %+v", name, a)
		}
	}
	if fwd.calls != 0 {
		t.Errorf("forwarder called %d times", fwd.calls)
	}
}

func TestHandOver_AddsThePeersErrorsToTheRequestDiagnostics(t *testing.T) {
	no := false
	fwd := &answeringForwarder{resp: &DispatchCalloutResponse{Outcome: "member_failed", TriesUsed: intPtr(1),
		MemberError: "card declined", MemberRetryable: &no,
		Warnings: []string{"processor p: slow"}, Errors: []string{"processor p: card declined"}}}
	ctx := common.WithDiagnostics(testContext())
	a := newTestRouter(t, &stubNodeRegistry{}, fwd).HandOver(ctx, node("peer-1", true, "tenant-1", "python"), ownerCallout(t, "processor"), 1, 1)

	if got := common.GetDiagnostics(ctx).GetErrors(); len(got) != 1 || got[0] != "processor p: card declined" {
		t.Errorf("errors = %v", got)
	}
	if len(a.Warnings) != 1 || a.Warnings[0] != "processor p: slow" {
		t.Errorf("Warnings = %v — returned for the caller to add, as the seam says", a.Warnings)
	}
	if got := common.GetDiagnostics(ctx).GetWarnings(); len(got) != 0 {
		t.Errorf("warnings were added twice over: %v", got)
	}
}

// --- over the wire ---

func realRouter(t *testing.T, loopback bool) *PeerRouter {
	t.Helper()
	fwd := NewHTTPForwarder(newAEAD(t), 5*time.Second)
	if loopback {
		fwd = fwd.AllowLoopbackForTesting()
	}
	return newTestRouter(t, &stubNodeRegistry{}, fwd)
}

func TestHandOver_PeerRefusesConnection_NoTryUsed(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close() // the port now refuses connections

	a := realRouter(t, true).HandOver(testContext(), contract.NodeInfo{NodeID: "gone", Addr: addr}, ownerCallout(t, "processor"), 3, 1)
	if a.Connected || a.TriesUsed != 0 || a.Failure == nil || a.Failure.Kind != contract.NoHandOff || len(a.Attempts) != 0 {
		t.Fatalf("%+v", a)
	}
}

func TestHandOver_ForbiddenAddress_IsTerminal(t *testing.T) {
	a := realRouter(t, false).HandOver(testContext(), contract.NodeInfo{NodeID: "p", Addr: "http://127.0.0.1:9"}, ownerCallout(t, "processor"), 3, 1)
	if a.Connected || a.TriesUsed != 0 || a.Failure == nil || a.Failure.Kind != contract.Terminal {
		t.Fatalf("%+v", a)
	}
}

func TestHandOver_OverTheWire_BadAnswersAreNoAnswer(t *testing.T) {
	peerAuth := newAEAD(t)
	var replay []byte
	tests := []struct {
		name   string
		answer func(w http.ResponseWriter, binding ResponseBinding)
	}{
		{"403 before anything was done", func(w http.ResponseWriter, _ ResponseBinding) { http.Error(w, "forbidden", http.StatusForbidden) }},
		{"500 from panic recovery", func(w http.ResponseWriter, _ ResponseBinding) { http.Error(w, "boom", http.StatusInternalServerError) }},
		{"502 from an intermediary", func(w http.ResponseWriter, _ ResponseBinding) { http.Error(w, "bad gateway", http.StatusBadGateway) }},
		{"200 in the clear", func(w http.ResponseWriter, _ ResponseBinding) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"outcome":"no_handoff","triesUsed":0}`))
		}},
		{"sealed, truncated", func(w http.ResponseWriter, b ResponseBinding) {
			wire, _ := peerAuth.SealResponse(w.Header(), b, []byte(`{"outcome":"no_handoff","triesUsed":0}`))
			_, _ = w.Write(wire[:len(wire)-5])
		}},
		{"sealed for an earlier request", func(w http.ResponseWriter, b ResponseBinding) {
			wire, _ := peerAuth.SealResponse(w.Header(), b, []byte(`{"outcome":"no_handoff","triesUsed":0}`))
			if replay == nil {
				replay = wire
			}
			_, _ = w.Write(replay)
		}},
		{"sealed, without an outcome", func(w http.ResponseWriter, b ResponseBinding) {
			wire, _ := peerAuth.SealResponse(w.Header(), b, []byte(`{"success":true}`))
			_, _ = w.Write(wire)
		}},
		{"sealed, not JSON", func(w http.ResponseWriter, b ResponseBinding) {
			wire, _ := peerAuth.SealResponse(w.Header(), b, []byte(`<html>`))
			_, _ = w.Write(wire)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _, binding, err := peerAuth.Verify(r)
				if err != nil {
					t.Errorf("Verify: %v", err)
				}
				tt.answer(w, binding)
			}))
			defer srv.Close()
			router := realRouter(t, true)
			peer := contract.NodeInfo{NodeID: "p", Addr: srv.URL}
			if tt.name == "sealed for an earlier request" {
				// the first answer is genuine — a sealed no_handoff — and is believed
				if first := router.HandOver(testContext(), peer, ownerCallout(t, "processor"), 3, 1); first.Connected || first.TriesUsed != 0 {
					t.Fatalf("first answer: %+v", first)
				}
			}
			assertLost(t, router.HandOver(testContext(), peer, ownerCallout(t, "processor"), 3, 1))
		})
	}
}

func TestPeerRouter_CountsHandOversByOutcome(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	fwd := &answeringForwarder{}
	router, err := NewPeerRouter(&stubNodeRegistry{}, "self-node", inOrderSelector{}, fwd, mp.Meter("test"))
	if err != nil {
		t.Fatalf("NewPeerRouter: %v", err)
	}
	peer := node("peer-1", true, "tenant-1", "python")
	script := []struct {
		resp *DispatchCalloutResponse
		err  error
	}{
		{&DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1)}, nil},
		{&DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1)}, nil},
		{&DispatchCalloutResponse{Outcome: "no_handoff", TriesUsed: intPtr(0)}, nil},
		{&DispatchCalloutResponse{Outcome: "member_failed", TriesUsed: intPtr(1), MemberError: "x"}, nil},
		{&DispatchCalloutResponse{Outcome: "terminal", TriesUsed: intPtr(1)}, nil},
		{nil, errors.New("reset")},
		{nil, &ForwardError{Stage: StageNotConnected, Err: errors.New("refused")}},
	}
	for _, s := range script {
		fwd.resp, fwd.err = s.resp, s.err
		router.HandOver(testContext(), peer, ownerCallout(t, "processor"), 3, 1)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "cyoda.dispatch.handovers" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				outcome, _ := dp.Attributes.Value("outcome")
				got[outcome.AsString()] = dp.Value
			}
		}
	}
	want := map[string]int64{"ok": 2, "no_handoff": 1, "member_failed": 1, "terminal": 1, "no_answer": 1, "not_connected": 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("outcome %q = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/cluster/dispatch/... -run 'TestPeerRouter|TestHandOver'`   Expected: FAIL (build) — `undefined: NewPeerRouter`, `undefined: ForwardError`.

- [ ] **Step 3: Implement**

`forwarder.go` — add, and give every return of `ForwardCallout`/`forward` a stage:

```go
// ForwardStage says how far a hand-over got before it failed. It is what lets
// the owner tell "nothing left this pnode" from "the peer may have the work".
type ForwardStage int

const (
	// StageBeforeConnect: the address failed validation, or the request could
	// not be built, marshalled or signed. No connection was attempted, and the
	// same would happen on every try.
	StageBeforeConnect ForwardStage = iota
	// StageNotConnected: the connection could not be opened — a dial error,
	// the connect timeout included.
	StageNotConnected
	// StageAfterConnect: anything later — a transport error, the wait running
	// out, a non-2xx status, an answer that is truncated or does not open.
	StageAfterConnect
)

// ForwardError is the error of a hand-over that produced no decoded answer.
type ForwardError struct {
	Stage ForwardStage
	Err   error
}

func (e *ForwardError) Error() string { return e.Err.Error() }
func (e *ForwardError) Unwrap() error { return e.Err }

func stageErr(stage ForwardStage, err error) error { return &ForwardError{Stage: stage, Err: err} }

// isDialError reports whether err is the connection failing to open. With a
// proxy the same failure is reported as "proxyconnect", and a failed TLS
// handshake is not a dial error either: both count as after-connect, which errs
// on the safe side.
func isDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}
```

`ForwardCallout`: `return nil, stageErr(StageBeforeConnect, err)` for the address.
In `forward`: the marshal, `NewRequestWithContext` and `Sign` failures return
`stageErr(StageBeforeConnect, fmt.Errorf(…))`; then

```go
	httpResp, err := f.client.Do(httpReq)
	if err != nil {
		stage := StageAfterConnect
		if isDialError(err) {
			stage = StageNotConnected
		}
		return stageErr(stage, fmt.Errorf("dispatch forward: HTTP POST %s: %w", url, err))
	}
```

and the four later returns (status, read, open, decode) become
`stageErr(StageAfterConnect, fmt.Errorf(…))`. Imports gain `"errors"`, `"net"`.
`errors.Is(err, ErrForbiddenPeerAddress)` in `forwarder_ssrf_test.go` keeps
working through `Unwrap`. `ClusterDispatcher` treats every forward error alike,
so its behaviour does not change in this task.

`internal/cluster/dispatch/peer_router.go`:

```go
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

const outcomeNotConnected = "not_connected"

// PeerRouter is the owner's view of the other pnodes: which of them advertise
// a tag, and the hand-over of a callout to one of them. It holds no state of
// its own.
type PeerRouter struct {
	registry   contract.NodeRegistry
	selfNodeID string
	selector   PeerSelector
	forwarder  DispatchForwarder
	handovers  metric.Int64Counter
}

// NewPeerRouter constructs a PeerRouter. A nil meter records nothing.
func NewPeerRouter(registry contract.NodeRegistry, selfNodeID string, selector PeerSelector, forwarder DispatchForwarder, meter metric.Meter) (*PeerRouter, error) {
	if meter == nil {
		meter = noop.NewMeterProvider().Meter("")
	}
	handovers, err := meter.Int64Counter("cyoda.dispatch.handovers",
		metric.WithDescription("Callouts handed over to another node, by outcome"))
	if err != nil {
		return nil, fmt.Errorf("failed to create the hand-over counter: %w", err)
	}
	return &PeerRouter{registry: registry, selfNodeID: selfNodeID, selector: selector, forwarder: forwarder, handovers: handovers}, nil
}

// Peers returns the alive pnodes other than self that advertise any of tagsCSV
// for tenantID, in selector order: the selector picks one of those left, again
// and again. A registry that cannot be read has no peers.
func (r *PeerRouter) Peers(tenantID, tagsCSV string) []contract.NodeInfo {
	nodes, err := r.registry.List(context.Background())
	if err != nil {
		slog.Debug("failed to list cluster nodes", "pkg", "dispatch", "err", err)
		return nil
	}
	var left []contract.NodeInfo
	for _, n := range nodes {
		if n.NodeID != r.selfNodeID && n.Alive && common.TagsOverlap(n.Tags[tenantID], tagsCSV) {
			left = append(left, n)
		}
	}
	ordered := make([]contract.NodeInfo, 0, len(left))
	for len(left) > 0 {
		pick, err := r.selector.Select(left)
		if err != nil {
			slog.Debug("peer selection failed", "pkg", "dispatch", "err", err)
			break
		}
		ordered = append(ordered, pick)
		for i, n := range left {
			if n.NodeID == pick.NodeID {
				left = append(left[:i:i], left[i+1:]...)
				break
			}
		}
	}
	return ordered
}

// Changed is the node registry's change signal: closed when a peer's list
// arrives with different tags, a peer joins, or a peer leaves. Take it before
// calling Peers, so that no change is missed.
func (r *PeerRouter) Changed() <-chan struct{} { return r.registry.Changed() }

// HandOver passes call to peer with triesLeft tries under fencing number major.
// ctx bounds the wait for the answer: the caller puts the deadline on it
// (triesLeft × answer limit + the hand-over allowance, never past the callout's
// deadline); the transport has no timeout of its own beyond opening the
// connection.
//
// The peer's warnings are returned for the caller to add; the diagnostics the
// tries raised as errors on the peer are added to ctx here.
func (r *PeerRouter) HandOver(ctx context.Context, peer contract.NodeInfo, call internalgrpc.Callout, triesLeft int, major uint32) HandOverAnswer {
	ans, outcome := r.handOver(ctx, peer, call, triesLeft, major)
	r.handovers.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	for _, e := range ans.peerErrors {
		common.AddError(ctx, e)
	}
	slog.Debug("hand-over", "pkg", "dispatch", "kind", call.Kind.String(), "name", call.Name, "requestId", call.RequestID,
		"peer", peer.NodeID, "triesLeft", triesLeft, "major", major, "outcome", outcome, "triesUsed", ans.TriesUsed)
	return ans
}

func (r *PeerRouter) handOver(ctx context.Context, peer contract.NodeInfo, call internalgrpc.Callout, triesLeft int, major uint32) (HandOverAnswer, string) {
	req, err := newHandOverRequest(spi.GetUserContext(ctx), r.selfNodeID, call, triesLeft, major)
	if err != nil {
		slog.Error("callout cannot be handed over", "pkg", "dispatch", "kind", call.Kind.String(), "name", call.Name, "requestId", call.RequestID, "err", err)
		return provedBeforeConnecting(err), contract.Terminal.String()
	}

	resp, err := r.forwarder.ForwardCallout(ctx, peer.Addr, req)
	if err != nil {
		stage := StageAfterConnect
		var fe *ForwardError
		if errors.As(err, &fe) {
			stage = fe.Stage
		}
		switch stage {
		case StageBeforeConnect:
			slog.Error("hand-over refused before connecting", "pkg", "dispatch", "peer", peer.NodeID, "requestId", call.RequestID, "err", err)
			return provedBeforeConnecting(err), contract.Terminal.String()
		case StageNotConnected:
			slog.Warn("peer could not be connected to; no try used", "pkg", "dispatch", "peer", peer.NodeID, "requestId", call.RequestID, "err", err)
			return notConnected(), outcomeNotConnected
		default:
			// The address and route are in err: logged here, never returned.
			slog.Warn("hand-over answer lost; counted as one try", "pkg", "dispatch", "peer", peer.NodeID, "requestId", call.RequestID, "err", err)
			return lostAnswer(), contract.NoAnswer.String()
		}
	}

	ans := readAnswer(call, resp, triesLeft)
	if ans.Failure == nil {
		return ans, OutcomeOK
	}
	return ans, ans.Failure.Kind.String()
}
```

`telemetry.md` — after the `cyoda.dispatch.count` line:

```markdown
- `cyoda.dispatch.handovers` — `Int64Counter` — callouts handed over to another node; labeled by `outcome` (`ok`, `no_handoff`, `no_answer`, `member_failed`, `terminal`, `not_connected`). `not_connected` is a node the connection to which could not be opened — no try was used; `no_answer` includes every hand-over whose answer was lost (no reply, a non-2xx status, an answer that does not authenticate). A rising `no_answer` share with healthy compute nodes points at the network between nodes or at node clocks more than 30 s apart.
```

and in the label list: *"`outcome` — hand-over outcome label for `cyoda.dispatch.handovers`"*.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/cluster/...`, `go test ./cmd/cyoda/help/...`, `go vet ./internal/cluster/...`   Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/cluster/dispatch/peer_router.go internal/cluster/dispatch/peer_router_test.go internal/cluster/dispatch/forwarder.go cmd/cyoda/help/content/telemetry.md
git commit -m "feat(dispatch): PeerRouter hands a callout over and says what became of it (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task P-6: the wire changes — the peer runs the local procedure; `TxToken`, `Success` and `Error` go

**Spec:** §6 "The peer handler calls `RunLocal` with `triesLeft` and never the `Coordinator`"; "where the peer refuses a request it has already opened and authenticated — a replayed nonce, a full replay cache — it answers with an authenticated `no_handoff` rather than a bare 403"; the restated response; §7 "`DispatchCalloutRequest.TxToken` … removed"; D5, D9, D12. Sequencing: `ClusterDispatcher` keeps working, on `PeerRouter.HandOver` with `triesLeft = 1`.

**Files:**
- Modify: `internal/cluster/dispatch/handler.go` (the whole of `handleCallout`; `NewDispatchHandler`; delete `dispatchErrorResponse` :174-208)
- Modify: `internal/cluster/dispatch/types.go` (delete `TxToken`, `Success`, `Error`; rewrite the two type comments)
- Modify: `internal/cluster/dispatch/cluster_dispatcher.go` (fields, constructor, the three `Dispatch*`, `forwardWithFailover`, `findPeer…`; delete `mintTxToken`, the three `build*Request`, `remintPeerError`, `extractCriteriaTags`)
- Modify: `app/app.go` (`:540-555`)
- Delete: `internal/cluster/dispatch/integration_txtoken_test.go`, `internal/cluster/dispatch/cluster_txtoken_test.go`
- Test: `internal/cluster/dispatch/handler_test.go`, `peer_router_test.go`; adapted: `cluster_dispatcher_test.go`, `cluster_dispatcher_failover_test.go`, `integration_test.go`, `types_test.go`, `forwarder_test.go`

**Interfaces:**
- Consumes: P-2, P-4, P-5; L-8 `(*ProcessorDispatcher).RunLocal`, `ResolveAnswerLimit`; L-10 (every try mints its own pass from `call.OwnerNodeID`, so nothing needs to pre-mint); C-2 `cfg.Callout.HandoverAllowance`.
- Produces:
  - `type LocalRunner interface { RunLocal(ctx context.Context, call internalgrpc.Callout, maxTries int) internalgrpc.LocalResult }` — `*internalgrpc.ProcessorDispatcher` satisfies it.
  - `func NewDispatchHandler(local LocalRunner, auth PeerAuth) *DispatchHandler`
  - transitional, deleted by the owner's-loop stream with the type: `type AnswerLimitResolver func(responseTimeoutMs int64) (time.Duration, *contract.CalloutFailure)`; `func NewClusterDispatcher(local contract.ExternalProcessingService, router *PeerRouter, answerLimit AnswerLimitResolver, waitTimeout, handoverAllowance time.Duration) *ClusterDispatcher`

What the handler answers, all after the request was opened and authenticated, all
HTTP 200 under seal: the local procedure's result; `no_handoff` with
`triesUsed: 0` for a request the replay cache refused; `terminal` with
`triesUsed: 0` for a body that does not parse or does not validate (unknown
kind, the two tenants disagreeing, a missing hand-over field). Those last were
bare 400s, which the owner would now read as a lost answer and report as a
retryable 503 — for a request that fails identically everywhere (Open point 3).
Only a request that does **not** authenticate is still a bare 403.

- [ ] **Step 1: Write the failing tests**

`handler_test.go` — delete `fakeLocalDispatcher` (`:21-63`) and add:

```go
// fakeRunner is the local procedure of the pnode that receives a hand-over.
type fakeRunner struct {
	result   internalgrpc.LocalResult
	onRun    func(ctx context.Context)
	gotCall  internalgrpc.Callout
	gotTries int
	gotCtx   context.Context
	calls    int
}

func (f *fakeRunner) RunLocal(ctx context.Context, call internalgrpc.Callout, maxTries int) internalgrpc.LocalResult {
	f.calls++
	f.gotCtx, f.gotCall, f.gotTries = ctx, call, maxTries
	if f.onRun != nil {
		f.onRun(ctx)
	}
	return f.result
}

func newHandlerMux(t *testing.T, runner LocalRunner, auth PeerAuth) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	NewDispatchHandler(runner, auth).Register(mux)
	return mux
}

// postHandOver sends req as the owner would and opens the sealed answer.
func postHandOver(t *testing.T, mux *http.ServeMux, auth *AEADPeerAuth, req DispatchCalloutRequest) DispatchCalloutResponse {
	t.Helper()
	plain, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	httpReq, binding := signedRequestWithBinding(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	return decodeSealed(t, auth, binding, rec)
}

func TestHandler_Outcomes(t *testing.T) {
	yes := true
	tests := []struct {
		name   string
		kind   string
		result internalgrpc.LocalResult
		check  func(t *testing.T, r DispatchCalloutResponse)
	}{
		{"processor answered", "processor",
			internalgrpc.LocalResult{TriesUsed: 1, Result: internalgrpc.CalloutResult{Entity: &spi.Entity{Data: []byte(`{"output":42}`)}}},
			func(t *testing.T, r DispatchCalloutResponse) {
				if r.Outcome != OutcomeOK || string(r.EntityData) != `{"output":42}` {
					t.Errorf("%+v", r)
				}
			}},
		{"criterion answered, with its reason", "criteria",
			internalgrpc.LocalResult{TriesUsed: 1, Result: internalgrpc.CalloutResult{Matches: true, Reason: "amount 5 below minimum 10"}},
			func(t *testing.T, r DispatchCalloutResponse) {
				if r.Outcome != OutcomeOK || r.Matches == nil || !*r.Matches || r.Reason != "amount 5 below minimum 10" {
					t.Errorf("%+v", r)
				}
			}},
		{"function answered", "function",
			internalgrpc.LocalResult{TriesUsed: 1, Result: internalgrpc.CalloutResult{Function: contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"fireAfterMs":5}`)}}},
			func(t *testing.T, r DispatchCalloutResponse) {
				if r.Outcome != OutcomeOK || r.ResultKind != "Schedule" || string(r.Result) != `{"fireAfterMs":5}` {
					t.Errorf("%+v", r)
				}
			}},
		{"the cnode said it failed", "processor",
			internalgrpc.LocalResult{TriesUsed: 1, Failure: &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", Retryable: &yes}},
			func(t *testing.T, r DispatchCalloutResponse) {
				if r.Outcome != "member_failed" || r.MemberError != "card declined" || r.MemberRetryable == nil || !*r.MemberRetryable {
					t.Errorf("%+v", r)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := newAEAD(t)
			runner := &fakeRunner{result: tt.result}
			tt.check(t, postHandOver(t, newHandlerMux(t, runner, auth), auth, validRequest(t, tt.kind)))
			if runner.calls != 1 {
				t.Errorf("RunLocal called %d times", runner.calls)
			}
		})
	}
}

func TestHandler_GivesRunLocalTheOwnersTriesAndAnswerLimit(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{result: internalgrpc.LocalResult{
		TriesUsed: 2,
		Result:    internalgrpc.CalloutResult{Matches: true},
		Attempts:  []contract.CalloutAttempt{{MemberID: "m1", Kind: contract.NoAnswer, Cause: "DISPATCH_TIMEOUT: criteria dispatch timed out after 1500ms: no response"}},
	}}
	req := validRequest(t, "criteria") // triesLeft 2, answerLimitMs 1500
	resp := postHandOver(t, newHandlerMux(t, runner, auth), auth, req)

	if runner.gotTries != 2 {
		t.Errorf("maxTries = %d, want the hand-over's triesLeft", runner.gotTries)
	}
	if runner.gotCall.AnswerLimit != 1500*time.Millisecond {
		t.Errorf("AnswerLimit = %s, want the owner's", runner.gotCall.AnswerLimit)
	}
	if resp.Outcome != OutcomeOK || resp.TriesUsed == nil || *resp.TriesUsed != 2 || len(resp.Attempts) != 1 || resp.Attempts[0].MemberID != "m1" {
		t.Errorf("two tries in one exchange must be reported as two: %+v", resp)
	}
}

func TestHandler_BuildsTheCalloutFromTheRequest(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{result: internalgrpc.LocalResult{TriesUsed: 1, Result: internalgrpc.CalloutResult{Entity: &spi.Entity{Data: []byte(`{}`)}}}}
	req := validRequest(t, "processor") // owner-node, major 5, outer (outer-rid,3,1)
	postHandOver(t, newHandlerMux(t, runner, auth), auth, req)

	call := runner.gotCall
	if call.OwnerNodeID != "owner-node" || call.RequestID != "rid-1" || call.TxID != "tx-1" {
		t.Errorf("callout = %+v", call)
	}
	if len(call.Outer) != 1 || call.Outer[0] != (token.Pair{Callout: "outer-rid", Major: 3, Minor: 1}) {
		t.Errorf("Outer = %+v", call.Outer)
	}
	if major, minor := call.Number.Next(); major != 5 || minor != 1 {
		t.Errorf("first try numbered (%d,%d), want (5,1)", major, minor)
	}
	uc := spi.GetUserContext(runner.gotCtx)
	if uc == nil || uc.Tenant.ID != "tenant-1" || uc.UserID != "user-1" || uc.Kind != spi.PrincipalUser {
		t.Errorf("user context = %+v", uc)
	}
	if id, ok := PeerIdentityFromContext(runner.gotCtx); !ok || id.AuthMethod() != "aead-v1" {
		t.Errorf("peer identity = %+v, %v", id, ok)
	}
}

// The handler holds a LocalRunner and nothing that could reach another pnode:
// with no cnode of its own it says so, and the owner asks the next pnode.
func TestHandler_NoLocalCnode_AnswersNoHandOff(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{result: internalgrpc.LocalResult{Failure: &contract.CalloutFailure{
		Kind: contract.NoHandOff, Code: common.ErrCodeNoComputeMemberForTag,
		Err: fmt.Errorf("%w: tags %q", contract.ErrNoMatchingMember, "python")}}}
	resp := postHandOver(t, newHandlerMux(t, runner, auth), auth, validRequest(t, "processor"))
	if resp.Outcome != "no_handoff" || resp.TriesUsed == nil || *resp.TriesUsed != 0 || resp.ErrorCode != common.ErrCodeNoComputeMemberForTag {
		t.Errorf("%+v", resp)
	}
}

func TestHandler_DiagnosticsOfTheTriesTravelBack(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{
		result: internalgrpc.LocalResult{TriesUsed: 1, Failure: &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined"}},
		onRun: func(ctx context.Context) {
			common.AddWarning(ctx, "processor myProcessor: slow")
			common.AddError(ctx, "processor myProcessor: card declined")
		},
	}
	resp := postHandOver(t, newHandlerMux(t, runner, auth), auth, validRequest(t, "processor"))
	if len(resp.Warnings) != 1 || resp.Warnings[0] != "processor myProcessor: slow" || len(resp.Errors) != 1 || resp.Errors[0] != "processor myProcessor: card declined" {
		t.Errorf("warnings %v, errors %v", resp.Warnings, resp.Errors)
	}
}

// What the peer logged for itself stays there: only client-safe text travels.
func TestHandler_InternalDetailStaysOnThePeer(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{result: internalgrpc.LocalResult{TriesUsed: 1,
		Failure:  &contract.CalloutFailure{Kind: contract.Terminal, Message: "auth context unavailable for dispatch", Err: errors.New("principal svc-7 at 10.0.0.5:5432")},
		Attempts: []contract.CalloutAttempt{{MemberID: "m1", Kind: contract.Terminal, Cause: "auth context unavailable for dispatch"}}}}
	mux := newHandlerMux(t, runner, auth)
	plain, _ := json.Marshal(validRequest(t, "processor"))
	httpReq, binding := signedRequestWithBinding(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httpReq)
	opened, err := auth.OpenResponse(rec.Header(), binding, rec.Body.Bytes())
	if err != nil {
		t.Fatalf("OpenResponse: %v", err)
	}
	if strings.Contains(string(opened), "10.0.0.5") || strings.Contains(string(opened), "svc-7") {
		t.Errorf("the answer carries the peer's internal detail: %s", opened)
	}
}

func TestHandler_ReplayedRequest_IsAnAuthenticatedNoHandOff(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{result: internalgrpc.LocalResult{TriesUsed: 1, Result: internalgrpc.CalloutResult{Entity: &spi.Entity{Data: []byte(`{}`)}}}}
	mux := newHandlerMux(t, runner, auth)

	plain, _ := json.Marshal(validRequest(t, "processor"))
	first, binding := signedRequestWithBinding(t, auth, http.MethodPost, "/internal/dispatch/callout", plain)
	wire, _ := io.ReadAll(first.Body)
	build := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/internal/dispatch/callout", bytes.NewReader(wire))
		r.Header.Set("Content-Type", DispatchContentType)
		r.Header.Set(DispatchTimestampHdr, first.Header.Get(DispatchTimestampHdr))
		return r
	}

	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, build())
	if resp := decodeSealed(t, auth, binding, rec1); resp.Outcome != OutcomeOK {
		t.Fatalf("first: %+v", resp)
	}
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, build())
	if rec2.Code != http.StatusOK {
		t.Fatalf("replay: status %d, want a sealed 200", rec2.Code)
	}
	resp := decodeSealed(t, auth, binding, rec2)
	if resp.Outcome != "no_handoff" || resp.TriesUsed == nil || *resp.TriesUsed != 0 {
		t.Errorf("replay: %+v", resp)
	}
	if runner.calls != 1 {
		t.Errorf("RunLocal called %d times: the replay reached a cnode", runner.calls)
	}
}

func TestHandler_FullReplayCache_IsAnAuthenticatedNoHandOff(t *testing.T) {
	auth := newAEAD(t)
	auth.nonces = newNonceCache(time.Minute, 1, time.Now)
	runner := &fakeRunner{result: internalgrpc.LocalResult{TriesUsed: 1, Result: internalgrpc.CalloutResult{Entity: &spi.Entity{Data: []byte(`{}`)}}}}
	mux := newHandlerMux(t, runner, auth)

	if resp := postHandOver(t, mux, auth, validRequest(t, "processor")); resp.Outcome != OutcomeOK {
		t.Fatalf("first: %+v", resp)
	}
	resp := postHandOver(t, mux, auth, validRequest(t, "processor"))
	if resp.Outcome != "no_handoff" || *resp.TriesUsed != 0 || runner.calls != 1 {
		t.Errorf("second: %+v, RunLocal calls %d", resp, runner.calls)
	}
}

func TestHandler_RequestThatCannotBeRun_IsAnAuthenticatedTerminal(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*DispatchCalloutRequest)
	}{
		{"unknown kind", func(r *DispatchCalloutRequest) { r.Kind = "bogus" }},
		{"entity of another tenant", func(r *DispatchCalloutRequest) { r.EntityMeta.TenantID = "tenant-2" }},
		{"entity with no tenant", func(r *DispatchCalloutRequest) { r.EntityMeta.TenantID = "" }},
		{"no tries", func(r *DispatchCalloutRequest) { r.TriesLeft = 0 }},
		{"no answer limit", func(r *DispatchCalloutRequest) { r.AnswerLimitMs = 0 }},
		{"no owner", func(r *DispatchCalloutRequest) { r.OwnerNodeID = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := newAEAD(t)
			runner := &fakeRunner{}
			req := validRequest(t, "processor")
			tt.mutate(&req)
			resp := postHandOver(t, newHandlerMux(t, runner, auth), auth, req)
			if resp.Outcome != "terminal" || resp.TriesUsed == nil || *resp.TriesUsed != 0 || resp.ErrorStatus != http.StatusInternalServerError {
				t.Errorf("%+v", resp)
			}
			if runner.calls != 0 {
				t.Error("RunLocal was called for a request that must not run")
			}
			raw, _ := json.Marshal(resp)
			if strings.Contains(string(raw), "tenant-2") {
				t.Errorf("the answer names a peer-supplied tenant: %s", raw)
			}
		})
	}
}

func TestHandler_BodyThatDoesNotParse_IsAnAuthenticatedTerminal(t *testing.T) {
	auth := newAEAD(t)
	runner := &fakeRunner{}
	httpReq, binding := signedRequestWithBinding(t, auth, http.MethodPost, "/internal/dispatch/callout", []byte(`{"kind":`))
	rec := httptest.NewRecorder()
	newHandlerMux(t, runner, auth).ServeHTTP(rec, httpReq)
	if resp := decodeSealed(t, auth, binding, rec); resp.Outcome != "terminal" || runner.calls != 0 {
		t.Errorf("%+v", resp)
	}
}
```

`peer_router_test.go` — the two ends together:

```go
// The cnode's own message and verdict reach the owner through a hand-over, and
// the peer's two tries are counted as two.
func TestHandOver_ThroughTheHandler_MemberMessageAndVerdictSurvive(t *testing.T) {
	yes := true
	peerAuth := newAEAD(t)
	runner := &fakeRunner{result: internalgrpc.LocalResult{
		TriesUsed: 2,
		Failure:   &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", Retryable: &yes},
		Attempts: []contract.CalloutAttempt{
			{MemberID: "m1", Kind: contract.NoHandOff, Cause: "COMPUTE_MEMBER_DISCONNECTED: processor compute member disconnected"},
			{MemberID: "m2", Kind: contract.MemberFailed, Cause: "card declined"},
		}}}
	srv := httptest.NewServer(newHandlerMux(t, runner, peerAuth))
	defer srv.Close()

	a := realRouter(t, true).HandOver(testContext(), contract.NodeInfo{NodeID: "peer-1", Addr: srv.URL}, ownerCallout(t, "processor"), 3, 4)

	if a.Failure == nil || a.Failure.Kind != contract.MemberFailed || a.Failure.Message != "card declined" || a.Failure.Retryable == nil || !*a.Failure.Retryable {
		t.Fatalf("Failure = %+v", a.Failure)
	}
	if !a.Connected || a.TriesUsed != 2 || len(a.Attempts) != 2 || a.Attempts[1].MemberID != "m2" {
		t.Errorf("%+v", a)
	}
	if runner.gotTries != 3 || runner.gotCall.OwnerNodeID != "self-node" {
		t.Errorf("peer got tries=%d owner=%q", runner.gotTries, runner.gotCall.OwnerNodeID)
	}
	if major, minor := runner.gotCall.Number.Next(); major != 4 || minor != 1 {
		t.Errorf("peer numbers its tries (%d,%d), want (4,1)", major, minor)
	}
}
```

(`handler_test.go` imports gain `internalgrpc`, `token`; `peer_router_test.go` gains `internalgrpc`.)

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/cluster/dispatch/... -run 'TestHandler_|TestHandOver_ThroughTheHandler'`
Expected: FAIL (build) — `undefined: LocalRunner`; after a stub of it, `replay: status 403, want a sealed 200` and `Outcome ""`.

- [ ] **Step 3: Implement**

`handler.go` — the type, constructor and `handleCallout`:

```go
// LocalRunner is the local procedure: the callout tried on this pnode's own
// cnodes. It is everything a pnode that receives a hand-over may do with it —
// the handler holds nothing through which it could hand the callout on.
type LocalRunner interface {
	RunLocal(ctx context.Context, call internalgrpc.Callout, maxTries int) internalgrpc.LocalResult
}

// DispatchHandler serves POST /internal/dispatch/callout: a callout handed over
// by the pnode that owns its transaction. Requests are authenticated via
// PeerAuth and every answer is sealed for its request; the owner believes
// nothing else.
type DispatchHandler struct {
	local LocalRunner
	auth  PeerAuth
}

func NewDispatchHandler(local LocalRunner, auth PeerAuth) *DispatchHandler {
	return &DispatchHandler{local: local, auth: auth}
}

func (h *DispatchHandler) handleCallout(w http.ResponseWriter, r *http.Request) {
	body, identity, binding, err := h.auth.Verify(r)
	switch {
	case errors.Is(err, ErrReplayRefused):
		// Opened and authenticated, then refused by the replay cache: nothing
		// was handed to a cnode, and the owner can be told so under seal. A
		// bare status would read as a lost answer and fail an operation that
		// is not repeat-safe — which a saturated cache must not do.
		slog.Warn("hand-over refused by the replay cache", "pkg", "dispatch", "remoteAddr", r.RemoteAddr)
		h.writeSealed(w, binding, refusal(noCnodeFailure()))
		return
	case err != nil:
		slog.Warn("dispatch request auth failed", "pkg", "dispatch", "remoteAddr", r.RemoteAddr, "err", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req DispatchCalloutRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.refuse(w, binding, fmt.Errorf("failed to parse hand-over: %w", err))
		return
	}
	if err := req.validate(); err != nil {
		h.refuse(w, binding, fmt.Errorf("failed to validate hand-over: %w", err))
		return
	}
	call, failure := req.toCallout()
	if failure != nil {
		h.writeSealed(w, binding, refusal(failure))
		return
	}

	ctx := common.WithDiagnostics(h.buildContext(r, identity, req.TenantID, req.UserID, req.PrincipalKind, req.Roles))
	res := h.local.RunLocal(ctx, call, req.TriesLeft)

	diag := common.GetDiagnostics(ctx)
	resp := responseFromLocal(call, res, diag.GetWarnings(), diag.GetErrors())
	slog.Debug("hand-over answered", "pkg", "dispatch", "kind", req.Kind, "requestId", req.RequestID,
		"owner", req.OwnerNodeID, "outcome", resp.Outcome, "triesUsed", res.TriesUsed)
	h.writeSealed(w, binding, resp)
}

// refuse answers a hand-over that was authenticated but cannot be run. It would
// be refused identically by every pnode: terminal, with no try made. The reason
// is logged here; the answer carries none of it, since the body is peer-supplied.
func (h *DispatchHandler) refuse(w http.ResponseWriter, binding ResponseBinding, err error) {
	slog.Error("hand-over refused", "pkg", "dispatch", "err", err)
	appErr := common.Internal("the hand-over was refused", err)
	h.writeSealed(w, binding, refusal(&contract.CalloutFailure{Kind: contract.Terminal, Code: appErr.Code, Message: appErr.Message, Err: appErr}))
}
```

Delete `verifyRequest` (folded in above, so that the replay arm has the binding)
and `dispatchErrorResponse`. `buildContext` and `writeSealed` stay. `GetWarnings`
and `GetErrors` return empty non-nil slices; `omitempty` keeps them off the wire.

`types.go` — delete `TxToken` from the request and `Success`, `Error` from the
response; the response comment becomes:

```go
// DispatchCalloutResponse is the answer to a hand-over. Outcome says what
// became of the callout; the result union mirrors DispatchCalloutRequest's Kind
// and is set when Outcome is "ok":
//   - Kind == "processor": EntityData.
//   - Kind == "criteria": Matches and Reason.
//   - Kind == "function": Result and ResultKind.
//
// ErrorCode, ErrorStatus and ErrorRetryable classify a failure the answering
// pnode classified itself, in the taxonomy a single pnode uses, so that the
// owner re-mints the same *common.AppError. They are empty for "member_failed",
// whose message and verdict are the cnode's own, and for a "terminal" failure
// that has no code.
```

`cluster_dispatcher.go` — the transitional adaptation. Fields and constructor:

```go
// AnswerLimitResolver resolves a callout's stored responseTimeoutMs to its
// answer limit; in production (*grpc.ProcessorDispatcher).ResolveAnswerLimit.
type AnswerLimitResolver func(responseTimeoutMs int64) (time.Duration, *contract.CalloutFailure)

type ClusterDispatcher struct {
	local             contract.ExternalProcessingService
	router            *PeerRouter
	answerLimit       AnswerLimitResolver
	waitTimeout       time.Duration
	handoverAllowance time.Duration
}

func NewClusterDispatcher(local contract.ExternalProcessingService, router *PeerRouter, answerLimit AnswerLimitResolver, waitTimeout, handoverAllowance time.Duration) *ClusterDispatcher {
	return &ClusterDispatcher{local: local, router: router, answerLimit: answerLimit, waitTimeout: waitTimeout, handoverAllowance: handoverAllowance}
}
```

The three methods lose the pre-minting (every try mints its own pass, on
whichever pnode makes it) and build a `Callout` instead of a request:

```go
func (d *ClusterDispatcher) DispatchProcessor(ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName string, transitionName string, txID string) (*spi.Entity, error) {
	result, err := d.local.DispatchProcessor(ctx, entity, processor, workflowName, transitionName, txID)
	if err == nil || !isNoMatchingMember(err) {
		return result, err
	}
	uc := spi.MustGetUserContext(ctx)
	res, err := d.forwardWithFailover(ctx, internalgrpc.NewProcessorCallout(uc.Tenant.ID, entity, processor, workflowName, transitionName, txID))
	if err != nil {
		return nil, err
	}
	return res.Entity, nil
}

func (d *ClusterDispatcher) DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target string, workflowName string, transitionName string, processorName string, txID string) (bool, string, error) {
	matches, reason, err := d.local.DispatchCriteria(ctx, entity, criterion, target, workflowName, transitionName, processorName, txID)
	if err == nil || !isNoMatchingMember(err) {
		return matches, reason, err
	}
	uc := spi.MustGetUserContext(ctx)
	call, failure := internalgrpc.NewCriteriaCallout(uc.Tenant.ID, entity, criterion, target, workflowName, transitionName, processorName, txID)
	if failure != nil {
		return false, "", failure
	}
	res, err := d.forwardWithFailover(ctx, call)
	if err != nil {
		return false, "", err
	}
	return res.Matches, res.Reason, nil
}

func (d *ClusterDispatcher) DispatchFunction(ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction, workflowName string, transitionName string, txID string) (contract.FunctionResult, error) {
	result, err := d.local.DispatchFunction(ctx, entity, fn, workflowName, transitionName, txID)
	if err == nil || !isNoMatchingMember(err) {
		return result, err
	}
	uc := spi.MustGetUserContext(ctx)
	res, err := d.forwardWithFailover(ctx, internalgrpc.NewFunctionCallout(uc.Tenant.ID, entity, fn, workflowName, transitionName, txID))
	if err != nil {
		return contract.FunctionResult{}, err
	}
	return res.Function, nil
}

// forwardWithFailover hands the callout over with one try, to one peer after
// another for as long as the failure allows another cnode to be tried: always
// when nothing was handed off, and after a hand-off only if the callout is
// repeat-safe. Each peer is asked at most once; the last failure surfaces.
func (d *ClusterDispatcher) forwardWithFailover(ctx context.Context, call internalgrpc.Callout) (internalgrpc.CalloutResult, error) {
	limit, failure := d.answerLimit(call.ResponseTimeoutMs)
	if failure != nil {
		return internalgrpc.CalloutResult{}, failure
	}
	call.RequestID = uuid.NewString()
	call.AnswerLimit = limit

	tenantID := string(call.TenantID)
	tried := make(map[string]bool)
	peer, err := d.findPeerWithPolling(ctx, tenantID, call.Tags, tried)
	if err != nil {
		return internalgrpc.CalloutResult{}, err
	}
	for {
		ans := func() HandOverAnswer {
			hctx, cancel := context.WithTimeout(ctx, limit+d.handoverAllowance)
			defer cancel()
			return d.router.HandOver(hctx, peer, call, 1, 1)
		}()
		for _, w := range ans.Warnings {
			common.AddWarning(ctx, w)
		}
		if ans.Failure == nil {
			return *ans.Result, nil
		}
		tried[peer.NodeID] = true
		if !ans.Failure.Kind.MayTryAnother(call.RepeatSafe) || ctx.Err() != nil {
			return internalgrpc.CalloutResult{}, ans.Failure
		}
		next, found := d.findPeer(tenantID, call.Tags, tried)
		if !found {
			return internalgrpc.CalloutResult{}, ans.Failure
		}
		peer = next
	}
}
```

`findPeerWithPolling` keeps its body with `d.findPeer(tenantID, tags, exclude)`
(no `ctx`); `findPeer` becomes:

```go
func (d *ClusterDispatcher) findPeer(tenantID, tags string, exclude map[string]bool) (contract.NodeInfo, bool) {
	for _, n := range d.router.Peers(tenantID, tags) {
		if !exclude[n.NodeID] {
			return n, true
		}
	}
	return contract.NodeInfo{}, false
}
```

Delete `mintTxToken`, `buildProcessorRequest`, `buildCriteriaRequest`,
`buildFunctionRequest`, `remintPeerError`, `extractCriteriaTags`, and the three
`slog.Debug("local … found no member …")` lines (the router logs each hand-over).
Imports: drop `token` and `log/slog`; `errors` stays (for `isNoMatchingMember`),
`fmt` and `net/http` stay (for `findPeerWithPolling`); add
`"github.com/google/uuid"`. `gossipPollInterval`, `forwardWithFailover`,
`findPeerWithPolling` and the type itself stay for the owner's-loop stream to
delete; `remintPeerError` is already gone when it gets there.

`app/app.go:540-555`:

```go
		forwarder := clusterdispatch.NewHTTPForwarder(peerAuth, cfg.Cluster.DispatchForwardTimeout)
		if cfg.Cluster.DispatchAllowLoopback {
			// Test-only: multi-node E2E fixtures run every node on 127.0.0.1.
			// Never set in production (SSRF guard stays active by default).
			forwarder = forwarder.AllowLoopbackForTesting()
		}
		peerRouter, err := clusterdispatch.NewPeerRouter(a.nodeRegistry, cfg.Cluster.NodeID,
			clusterdispatch.NewRandomSelector(), forwarder, observability.Meter())
		if err != nil {
			slog.Error("failed to construct the peer router", "pkg", "cluster", "err", err)
			os.Exit(1)
		}
		extProc = clusterdispatch.NewClusterDispatcher(localDispatcher, peerRouter,
			localDispatcher.ResolveAnswerLimit, cfg.Cluster.DispatchWaitTimeout, cfg.Callout.HandoverAllowance)
```

`app.go:785, 798` compile unchanged: `localDispatcher` is a
`*ProcessorDispatcher`, which has `RunLocal`.

**Existing tests — the decision for each**

- `integration_txtoken_test.go` (`TestIntegration_HandlerReinjectsTxToken_*`) and `cluster_txtoken_test.go` (`TestBuildProcessorRequest_CarriesOwnerToken`): **deleted** — they pin the pre-minted pass travelling in the request. What replaces the behaviour is tested by `TestHandler_BuildsTheCalloutFromTheRequest` (this task) and L-10's `TestRunLocal_EveryTryMints…`.
- `handler_test.go`: `TestHandler_ProcessorSuccess`, `_CriteriaSuccess`, `TestHandleCriteria_PropagatesReason` → **replaced by** `TestHandler_Outcomes`. `_ProcessorError_SanitizedResponse`, `_CriteriaError_SanitizedResponse` → **replaced by** `TestHandler_InternalDetailStaysOnThePeer`. `_ErrorTaxonomy_AppError`, `_ErrorTaxonomy_NoMatchingMember` → **replaced by** P-4 `TestResponseFromLocal` and `TestHandler_NoLocalCnode_AnswersNoHandOff`. `_RejectsReplayedRequest` → **replaced by** `TestHandler_ReplayedRequest_IsAnAuthenticatedNoHandOff`. `_UnknownCalloutKind`, `TestHandleCallout_RejectsEntityMetaTenantMismatch`, `_RejectsAbsentEntityMetaTenant` → **replaced by** `TestHandler_RequestThatCannotBeRun_IsAnAuthenticatedTerminal`. `_PopulatesPeerIdentityInContext`, `_ReconstructsPrincipalKindInContext` → **rewritten** on `fakeRunner` + `validRequest` (`fake.capturedCtx` → `runner.gotCtx`; the principal-kind table sets `req.PrincipalKind` on a `validRequest`). `_MissingAEADHeaders`, `_RejectsPlainJSONWithoutAEAD`, `TestNewAEADPeerAuth_SecretTooShort`, `TestHandler_AnswerIsSealedForItsRequest` → **stay**, with `&fakeRunner{…}` for `&fakeLocalDispatcher{…}`.
- `types_test.go`: the response round-trips replace `Success: true` by `Outcome: "ok"` and `Success: false, Error: …` by `Outcome: "no_answer"` plus the trio; `TestDispatchCalloutRequest_*` drop `TxToken` where they set it. **Stay** otherwise.
- `forwarder_test.go`: `wantResp` literals and the two `Encode(DispatchCalloutResponse{Success: true})` become `Outcome: "ok"`. **Stay** otherwise.
- `cluster_dispatcher_test.go`, `cluster_dispatcher_failover_test.go`, `integration_test.go` — **adapted mechanically, then left for the owner's-loop stream to delete with `ClusterDispatcher`**. Add to `cluster_dispatcher_test.go`:

  ```go
  func testAnswerLimit(ms int64) (time.Duration, *contract.CalloutFailure) {
  	if ms > 0 {
  		return time.Duration(ms) * time.Millisecond, nil
  	}
  	return 5 * time.Second, nil
  }

  func newTestClusterDispatcher(t *testing.T, local contract.ExternalProcessingService, registry contract.NodeRegistry, selfNodeID string, selector PeerSelector, fwd DispatchForwarder, wait time.Duration) *ClusterDispatcher {
  	t.Helper()
  	router, err := NewPeerRouter(registry, selfNodeID, selector, fwd, nil)
  	if err != nil {
  		t.Fatalf("NewPeerRouter: %v", err)
  	}
  	return NewClusterDispatcher(local, router, testAnswerLimit, wait, time.Second)
  }

  // stubRunner lets a stubDispatcher stand for the peer's local procedure.
  type stubRunner struct{ stub *stubDispatcher }

  func (s stubRunner) RunLocal(ctx context.Context, call internalgrpc.Callout, _ int) internalgrpc.LocalResult {
  	src := call.Source
  	var res internalgrpc.CalloutResult
  	var err error
  	switch call.Kind {
  	case internalgrpc.ProcessorCallout:
  		res.Entity, err = s.stub.DispatchProcessor(ctx, src.Entity, *src.Processor, src.WorkflowName, src.TransitionName, call.TxID)
  	case internalgrpc.CriteriaCallout:
  		res.Matches, res.Reason, err = s.stub.DispatchCriteria(ctx, src.Entity, src.Criterion, src.Target, src.WorkflowName, src.TransitionName, src.ProcessorName, call.TxID)
  	default:
  		res.Function, err = s.stub.DispatchFunction(ctx, src.Entity, *src.Function, src.WorkflowName, src.TransitionName, call.TxID)
  	}
  	switch {
  	case err == nil:
  		return internalgrpc.LocalResult{Result: res, TriesUsed: 1}
  	case errors.Is(err, internalgrpc.ErrNoMatchingMember):
  		return internalgrpc.LocalResult{Failure: &contract.CalloutFailure{Kind: contract.NoHandOff, Code: common.ErrCodeNoComputeMemberForTag, Message: err.Error(), Err: err}}
  	default:
  		return internalgrpc.LocalResult{TriesUsed: 1,
  			Failure:  &contract.CalloutFailure{Kind: contract.NoAnswer, Message: err.Error(), Err: err},
  			Attempts: []contract.CalloutAttempt{{MemberID: "m1", Kind: contract.NoAnswer, Cause: err.Error()}}}
  	}
  }
  ```

  Then, throughout the three files: `NewClusterDispatcher(L, R, S, SEL, F, W, nil, 0)` → `newTestClusterDispatcher(t, L, R, S, SEL, F, W)`; `NewDispatchHandler(peerLocal, auth)` → `NewDispatchHandler(stubRunner{peerLocal}, auth)`; a scripted `&DispatchCalloutResponse{Success: true, …}` → `{Outcome: OutcomeOK, TriesUsed: intPtr(1), …}`; `{Success: false, Error: …, ErrorCode…}` → `{Outcome: "no_answer", TriesUsed: intPtr(1), ErrorCode…}` (the 500 case: `Outcome: "terminal"`); `noMemberResponse()` → `{Outcome: "no_handoff", TriesUsed: intPtr(0), ErrorCode…}`. Two expectations change with the semantics, and only these:
  - `cluster_dispatcher_failover_test.go:88` `FailoverOnTransportError` and `:315` `FailoverOverWire`: a scripted `errors.New("connection refused")` becomes `&ForwardError{Stage: StageNotConnected, Err: errors.New("connection refused")}` — a peer that cannot be *connected to* is still routed around; a lost answer no longer is, for a processor (`TestClusterDispatcher_ForwardFailure`, `:477`, and `CtxCancelledMidForwardKeepsTaxonomy`, `:284`, keep asserting `DISPATCH_FORWARD_FAILED` with a plain error and stay as they are). `FailoverOverWire` needs no edit: a closed port is a dial error.
  - `:211` `FailoverExhaustion/all_transport_errors_surface_forward_failed` → renamed `all_peers_unreachable_surface_no_compute_member`, scripted with `StageNotConnected`, asserting `NO_COMPUTE_MEMBER_FOR_TAG` (§8.2 "No peer could be connected to, and no local cnode").
  - `:155` `FailoverOnPeerNoMember`, `:179` `NoFailoverOnExecutedCalloutFailure`, the second half of `:211`: **stay** on the new literals.
  - In `TestClusterDispatcher_RemintsPeerErrorTaxonomy` any sub-test that scripted `Success: false` with an **empty** `ErrorCode` and expected the plain `"peer dispatch failed"` error now scripts `{Outcome: "member_failed", TriesUsed: intPtr(1), MemberError: "boom"}` and asserts `errors.As(err, &failure)` with `failure.Kind == contract.MemberFailed` and `failure.Message == "boom"` — D12, the behaviour R§3 records as lost today.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/cluster/...`, `go build ./...`, `go vet ./internal/cluster/... ./app/...`   Expected: PASS.
Exit checks:
- `grep -rn 'TxToken' internal/cluster/dispatch/` → no hits
- `grep -rn 'WithTxToken\|TxTokenFromContext' --include='*.go' . | grep -v '^./internal/grpc/'` → no hits (unblocks L-11)
- `grep -rn 'remintPeerError\|dispatchErrorResponse\|extractCriteriaTags\|mintTxToken' internal/` → no hits
- `grep -rn '\.Success\b\|Success:' internal/cluster/dispatch/` → no hits
- `grep -n 'TxTokenTTL' app/app.go` → only the `NewProcessorDispatcher` line, if L-10 has not removed it yet

- [ ] **Step 5: Commit**

```
git add internal/cluster/dispatch app/app.go
git commit -m "feat(dispatch): a hand-over runs the local procedure on the peer and says what became of it (#254)" -m "The request carries the request id, tries left, answer limit, owner, fencing number and enclosing pairs; the answer states its outcome and tries used. TxToken, Success and Error are gone. A request refused by the replay cache is answered with a sealed no_handoff." -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task P-7: every hand-over opens its own connection; the wait is the context's

**Spec:** §6 Transport, bullets 1–3: `DisableKeepAlives`; `Proxy` nil; TLS handshake timeout = connect timeout; `CYODA_DISPATCH_CONNECT_TIMEOUT` on the dialer; "a per-request deadline on `ctx`; the hand-over client has no client-wide `Timeout`"; `CYODA_DISPATCH_FORWARD_TIMEOUT` "no longer governs hand-overs".

**Files:**
- Modify: `internal/cluster/dispatch/forwarder.go` (`NewHTTPForwarder` :31-43)
- Modify: `app/app.go` (the `NewHTTPForwarder` call)
- Test: `internal/cluster/dispatch/forwarder_transport_test.go` (new, `package dispatch_test`), `peer_router_test.go`

**Interfaces:**
- Consumes: P-6 (every caller now puts a deadline on `ctx`); C-2 `cfg.Cluster.DispatchConnectTimeout`.
- Produces: `func NewHTTPForwarder(auth PeerAuth, connectTimeout time.Duration) *HTTPForwarder` — same shape, the duration now bounds opening the connection (TCP, and the TLS handshake) and nothing else. `internal/cluster/scheduler_rpc.go` is **not** touched: its client keeps `http.Client{Timeout: CYODA_DISPATCH_FORWARD_TIMEOUT}`.

A redirect is not followed: a 3xx is a non-2xx and the answer is lost. Following
one would re-send a signed hand-over to an address the registry never gave.

**TDD waiver, recorded:** that the dialer's `Timeout` is the value passed and
that `Proxy` is nil are not asserted by a test. A connect timeout needs an
address that silently drops SYNs, which no CI network guarantees, and the proxy
is read from the process environment once (`http.ProxyFromEnvironment`) and
never for loopback. Both are one line each in the constructor below and are for
the reviewer; no seam is added to production code to observe them.

- [ ] **Step 1: Write the failing tests**

`internal/cluster/dispatch/forwarder_transport_test.go`:

```go
package dispatch_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
)

func okAnswer(*http.Request, []byte) any {
	one := 1
	return dispatch.DispatchCalloutResponse{Outcome: dispatch.OutcomeOK, TriesUsed: &one}
}

func TestHTTPForwarder_EveryHandOverOpensItsOwnConnection(t *testing.T) {
	auth := newTestPeerAuth(t)
	var opened atomic.Int32
	srv := httptest.NewUnstartedServer(sealingHandler(t, auth, okAnswer))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), time.Second).AllowLoopbackForTesting()
	for i := 0; i < 3; i++ {
		if _, err := f.ForwardCallout(context.Background(), srv.URL, makeProcessorReq()); err != nil {
			t.Fatalf("hand-over %d: %v", i, err)
		}
	}
	if got := opened.Load(); got != 3 {
		t.Fatalf("3 hand-overs opened %d connections, want 3", got)
	}
}

func TestHTTPForwarder_TheWaitIsNotBoundedByTheConnectTimeout(t *testing.T) {
	auth := newTestPeerAuth(t)
	srv := sealingPeer(t, auth, func(r *http.Request, p []byte) any {
		time.Sleep(400 * time.Millisecond) // a cnode thinking, far longer than the connect timeout
		return okAnswer(r, p)
	})
	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 50*time.Millisecond).AllowLoopbackForTesting()
	if _, err := f.ForwardCallout(context.Background(), srv.URL, makeProcessorReq()); err != nil {
		t.Fatalf("the connect timeout cut off the wait for the answer: %v", err)
	}
}

func TestHTTPForwarder_TheWaitIsTheContextsDeadline(t *testing.T) {
	auth := newTestPeerAuth(t)
	release := make(chan struct{})
	var once sync.Once
	srv := sealingPeer(t, auth, func(r *http.Request, p []byte) any {
		<-release
		return okAnswer(r, p)
	})
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), time.Second).AllowLoopbackForTesting()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := f.ForwardCallout(ctx, srv.URL, makeProcessorReq())

	var fe *dispatch.ForwardError
	if !errors.As(err, &fe) || fe.Stage != dispatch.StageAfterConnect {
		t.Fatalf("err = %v, want an after-connect forward error", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("returned after %s, want the context's 150ms", elapsed)
	}
}

// With an https:// node address the connection opens and the handshake never
// completes. That is not a dial error — the peer may be there — and it is
// bounded by the connect timeout, not by the whole wait.
func TestHTTPForwarder_StalledTLSHandshake_IsAfterConnect_AndBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			func() {
				mu.Lock()
				defer mu.Unlock()
				held = append(held, c) // accept, then say nothing
			}()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 200*time.Millisecond).AllowLoopbackForTesting()
	start := time.Now()
	_, err = f.ForwardCallout(context.Background(), "https://"+ln.Addr().String(), makeProcessorReq())

	var fe *dispatch.ForwardError
	if !errors.As(err, &fe) || fe.Stage != dispatch.StageAfterConnect {
		t.Fatalf("err = %v, want an after-connect forward error", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("a stalled handshake held the hand-over for %s", elapsed)
	}
}

func TestHTTPForwarder_DoesNotFollowARedirect(t *testing.T) {
	var reached atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/internal/dispatch/callout", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), time.Second).AllowLoopbackForTesting()
	_, err := f.ForwardCallout(context.Background(), srv.URL, makeProcessorReq())
	var fe *dispatch.ForwardError
	if !errors.As(err, &fe) || fe.Stage != dispatch.StageAfterConnect {
		t.Fatalf("err = %v, want an after-connect forward error", err)
	}
	if reached.Load() {
		t.Fatal("the signed hand-over was re-sent to the redirect target")
	}
}
```

`peer_router_test.go`:

```go
func TestHandOver_WaitRunsOut_IsOneTry(t *testing.T) {
	peerAuth := newAEAD(t)
	release := make(chan struct{})
	runner := &fakeRunner{onRun: func(context.Context) { <-release }}
	srv := httptest.NewServer(newHandlerMux(t, runner, peerAuth))
	t.Cleanup(func() { close(release); srv.Close() })

	ctx, cancel := context.WithTimeout(testContext(), 150*time.Millisecond)
	defer cancel()
	assertLost(t, realRouter(t, true).HandOver(ctx, contract.NodeInfo{NodeID: "slow", Addr: srv.URL}, ownerCallout(t, "processor"), 3, 1))
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/cluster/dispatch/... -run 'TestHTTPForwarder_(EveryHandOver|TheWait|StalledTLS|DoesNotFollow)|TestHandOver_WaitRunsOut'`
Expected: FAIL — `3 hand-overs opened 1 connections, want 3`; `the connect timeout cut off the wait for the answer` (the client-wide `Timeout` of 50 ms); `the signed hand-over was re-sent to the redirect target`. `TestHandOver_WaitRunsOut_IsOneTry` and `_TheWaitIsTheContextsDeadline` already pass — they pin what must stay true once the client `Timeout` is gone.

- [ ] **Step 3: Implement**

`forwarder.go`:

```go
// NewHTTPForwarder constructs an HTTPForwarder. connectTimeout bounds opening
// the connection — the TCP connect and, for an https:// node address, the TLS
// handshake — and nothing else: how long to wait for the answer is the
// deadline on the context of each hand-over. Loopback peer addresses are
// rejected by default; see AllowLoopbackForTesting.
func NewHTTPForwarder(auth PeerAuth, connectTimeout time.Duration) *HTTPForwarder {
	dialer := &net.Dialer{Timeout: connectTimeout}
	return &HTTPForwarder{
		auth: auth,
		client: &http.Client{
			// No Timeout: a hand-over may rightly take several answer limits.
			Transport: &http.Transport{
				// Every hand-over opens its own connection. On a kept-alive one
				// a peer that died is discovered only when the read fails —
				// after the whole wait, and indistinguishable from a peer that
				// took the work and then died. On a fresh one "could not
				// connect" is proof that nothing left this pnode.
				DisableKeepAlives: true,
				DialContext:       dialer.DialContext,
				// A failed handshake is not a dial error; it is at least
				// bounded like one.
				TLSHandshakeTimeout: connectTimeout,
				// Never a proxy: through one, a peer that is down is reported
				// as "proxyconnect", not "dial", and the rule above is lost.
				Proxy: nil,
			},
			// A redirect is an answer that is not 2xx. Following it would send
			// a signed hand-over to an address the registry never gave.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}
```

`app/app.go`: `clusterdispatch.NewHTTPForwarder(peerAuth, cfg.Cluster.DispatchConnectTimeout)`.
`app.go:592` (`NewSchedulerRPCClient(peerAuth, cfg.Cluster.DispatchForwardTimeout)`) stays.

Existing tests: every `NewHTTPForwarder(auth, d)` in the package compiles
unchanged and **stays**; `d` is now a connect timeout, generous in all of them.
`forwarder_ssrf_test.go:108` (`50*time.Millisecond`, a routable address that
does not answer) stays: it asserts only that the error is not the SSRF one.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/cluster/...`, `go build ./...`, `go vet ./internal/cluster/... ./app/...`   Expected: PASS.
Exit check: `grep -n 'MaxIdleConns\|IdleConnTimeout\|Timeout: timeout' internal/cluster/dispatch/forwarder.go` → no hits.

- [ ] **Step 5: Commit**

```
git add internal/cluster/dispatch/forwarder.go internal/cluster/dispatch/forwarder_transport_test.go internal/cluster/dispatch/peer_router_test.go app/app.go
git commit -m "feat(dispatch): every hand-over opens its own connection; the wait is the context's deadline (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task P-8: documentation of the hand-over

**Spec:** §14 — `errors/DISPATCH_FORWARD_FAILED.md` (revised), `cluster.md`, `docs/ARCHITECTURE.md` (the parts that describe the wire and the endpoint), `CHANGELOG.md`. The settings' own help lines (`config/cluster.md`, `README.md`, `config_registry.go`) are C-2's; `telemetry.md` was done in P-5; the `CROSS-NODE DISPATCH FAILOVER` section of `cluster.md` and the dispatch-flow block and failure-mode rows of `ARCHITECTURE.md` (`:604-662, 857-861`) describe the owner's loop and are that stream's.

**Files:**
- Modify: `cmd/cyoda/help/content/errors/DISPATCH_FORWARD_FAILED.md`
- Modify: `cmd/cyoda/help/content/cluster.md` (`## DISPATCH REPLAY PROTECTION`, `:76-78`; `SECRET ROTATION` `:72` stays true)
- Modify: `docs/ARCHITECTURE.md` (`:541-549` the AEAD paragraph; `:630-632` the transport sentence; `:634-645` the endpoint bullets; the "Dispatch request/response types" paragraph that follows)
- Modify: `CHANGELOG.md` (`[Unreleased]`)

**Interfaces:** consumes P-1…P-7 as built. Produces nothing for other streams.

No RED/GREEN cycle of its own: the help tree's tests (`go test ./cmd/cyoda/help/...`, front-matter and see-also integrity, `TestErrCode_Parity`) are the check, and they must stay green.

- [ ] **Step 1: `errors/DISPATCH_FORWARD_FAILED.md`** — NAME and DESCRIPTION become:

```markdown
## NAME

DISPATCH_FORWARD_FAILED — a callout was handed over to another node and the answer was lost.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

The node that owns the transaction handed a processor, criterion or function callout to another node and did not get an answer it can trust: no reply within the time allowed, a connection that broke after it was opened, a status other than `2xx` (from the node itself or from anything between the two), or a reply that does not authenticate — which includes node clocks more than 30 seconds apart.

**The other node may have given the work to a compute node, and the compute node may have run it.** A node that could not be *connected to* does not produce this error — nothing left the owner, no try is counted, and the next node is asked. This error means the connection was opened and what happened afterwards is unknown. It therefore counts as one try. A criterion, a function, or a processor declared `idempotent` is then given to another compute node; any other processor stops here, the operation fails with this error, and its transaction is rolled back.

Retryable, as far as cyoda's own state is concerned — see `errors.DISPATCH_TIMEOUT` for what `retryable` does and does not promise about effects outside cyoda. Persistent occurrences point at the network between nodes, at a proxy or sidecar answering `502`–`504` for a node that is down, or at clock skew.
```

- [ ] **Step 2: `cluster.md`, `## DISPATCH REPLAY PROTECTION`** — replace the section body:

```markdown
A callout handed over to another node travels as an AES-256-GCM envelope, and so does the answer. Both are keyed from `CYODA_HMAC_SECRET`. A request binds its direction, method, path and timestamp; an answer binds its direction, the path, and the timestamp and nonce of the one request it answers — so an envelope cannot be replayed onto another endpoint, reflected back in the other direction, or moved onto another request. Every hand-over opens a connection of its own.

Each node keeps an in-memory replay cache of request nonces: entries live for 60 seconds (twice the 30-second timestamp-skew window) and the cache holds at most 100 000 nonces, per node. The cache is fail-closed: when full, new requests are refused until entries expire. A request refused by the cache has already been authenticated, so the node answers — under seal — that it handed the work to no compute node; the owner counts no try and asks the next node. A saturated cache therefore does not fail operations. Answers need no cache: each opens only under the nonce of a request the owner itself chose. A request that does **not** authenticate is refused with a bare `403`, which the owner cannot trust and counts as a lost answer (`DISPATCH_FORWARD_FAILED`). The ceiling admits roughly 1 600 sustained inbound hand-overs per second per node — far above realistic callout rates; reaching it indicates a flood, not normal load.
```

- [ ] **Step 3: `docs/ARCHITECTURE.md`** (present tense, no history):
  - `:541-549` → "Inter-node dispatch authentication uses AEAD (AES-256-GCM) over an HKDF-SHA256-derived key (info string `"cyoda-dispatch-v1"`), which separates the dispatch key from the raw gossip-encryption secret despite both being derived from the same `CYODA_HMAC_SECRET`. Wire format is `[nonce(12) || ciphertext||tag]` with Content-Type `application/cyoda-dispatch-v1`, in both directions. A request's associated data is the label `request`, the HTTP method, the path and `X-Dispatch-Timestamp`; an answer's is the label `response`, the path, and the timestamp and nonce of the request it answers, under a fresh nonce of its own. That prevents cross-endpoint replay, reflection, and an answer being moved onto another request. A bounded, TTL-evicted nonce cache rejects replayed requests within the skew window; the scheduler's peer RPC signs its requests the same way and answers in plain JSON."
  - `:630-632` → "Every hand-over opens its own connection (`DisableKeepAlives`), so that a node that cannot be connected to is told apart from one that took the work and then died. Opening the connection is bounded by `CYODA_DISPATCH_CONNECT_TIMEOUT` (TCP connect and TLS handshake); the wait for the answer is a deadline on the request's context, set by the owner. The transport uses no proxy and follows no redirect. `CYODA_DISPATCH_FORWARD_TIMEOUT` bounds the scheduler's peer RPC only."
  - endpoint bullets: keep "Single route…", "Authenticated and encrypted… §4.2" (append "— the answer too"), "10MB max body size", "Reconstruct `UserContext`…"; replace the two-tenants bullet's "a mismatch is `400`" by "a mismatch is answered, under seal, as a `terminal` refusal with no try made — as is any authenticated request that cannot be run"; add "- Runs the local procedure (`RunLocal`) with the tries the owner allows, and never hands the callout on" and "- A request that does not authenticate is a bare `403`; one that authenticates but is refused by the replay cache is answered, under seal, `no_handoff`".
  - "Dispatch request/response types": the request additionally carries `requestID`, `triesLeft`, `answerLimitMs`, `ownerNodeID`, `major`, `outer`, `repeatSafe`; the response carries `outcome`, `triesUsed`, `attempts`, the cnode's `memberError`/`memberRetryable`, the classified-error trio, the result union, `warnings` and `errors`. Remove any mention of `txToken`, `success`, `error`.
  - Audit the rest of §4 for `txToken` on the dispatch request and "pre-minted"/"re-injected" token wording; correct in place. Exit check: `grep -n 'TxToken\|txToken' docs/ARCHITECTURE.md` shows only the client-facing `X-Tx-Token` header and the token type.

- [ ] **Step 4: `CHANGELOG.md` `[Unreleased]`**

Under `### Breaking`:

```markdown
- **Nodes of different versions cannot share a cluster.** The message by which
  one node hands a callout to another changed: requests are bound to their
  direction, the answer is encrypted and authenticated like the request, and the
  payload states the outcome explicitly. A node of this version treats an answer
  it cannot authenticate as lost. The scheduler's peer RPC signs with the same
  envelope and changes with it. Stop the cluster to upgrade it.
- **`CYODA_DISPATCH_FORWARD_TIMEOUT` no longer governs handing a callout to
  another node.** Opening the connection is bounded by the new
  `CYODA_DISPATCH_CONNECT_TIMEOUT`; the wait for the answer follows from the
  callout's tries and answer limit plus `CYODA_CALLOUT_HANDOVER_ALLOWANCE`. The
  setting keeps its name and meaning for the scheduler's peer RPC.
```

Under `### Fixed`:

```markdown
- **A compute node's own failure message and its `retryable` verdict now reach
  the client when the compute node is attached to another node.** They were
  replaced by `peer dispatch failed` on the way. Its warnings, which were
  dropped on the same path, arrive too.
- **A node whose dispatch replay cache is full no longer fails callouts handed
  to it**; it answers that it took no work, and the next node is asked.
```

Under `### Added`: `cyoda.dispatch.handovers` (counter, by `outcome`).

- [ ] **Step 5: Verify and commit**

Run: `go test ./cmd/cyoda/help/...`   Expected: PASS.

```
git add cmd/cyoda/help/content/errors/DISPATCH_FORWARD_FAILED.md cmd/cyoda/help/content/cluster.md docs/ARCHITECTURE.md CHANGELOG.md
git commit -m "docs(dispatch): the hand-over — sealed answers, own connection, what a lost answer means (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## Stream interface summary

**Other streams may consume from P** (`internal/cluster/dispatch` unless said otherwise)

- `type HandOverAnswer struct { Connected bool; Result *internalgrpc.CalloutResult; Failure *contract.CalloutFailure; TriesUsed int; Attempts []contract.CalloutAttempt; Warnings []string }` — as the seam, with two precisions:
  - `Connected == false` ⇒ `TriesUsed == 0`, and it is one of: the peer could not be connected to (`Failure.Kind == NoHandOff`); an authenticated `no_handoff` with no try made (`NoHandOff`); or the hand-over was proved impossible before connecting (`Failure.Kind == Terminal`, a ticketed 500). The loop must read `Failure.Kind`, not `Connected` alone.
  - `Connected == true` with `TriesUsed == 0` happens for exactly one answer: the peer refusing, under seal, a request that cannot be run (`Terminal`). Every answer that allows the loop to go on has either `TriesUsed ≥ 1` or `Kind == NoHandOff` for a peer the pass will not ask again, so the loop always makes progress.
  - A peer that tried cnodes and handed off to none answers `no_handoff` with `TriesUsed ≥ 1`: `Connected == true`, `Kind == NoHandOff`, tries counted.
  - Every `Failure` other than `MemberFailed` and a code-less `Terminal` carries an `*common.AppError` in `Err`. `NoHandOff` carries `NO_COMPUTE_MEMBER_FOR_TAG` (503, retryable); a lost answer carries `DISPATCH_FORWARD_FAILED` (503, retryable), message `DISPATCH_FORWARD_FAILED: forwarding the callout to a peer node failed`, and one attempt with member `-`.
- `func NewPeerRouter(registry contract.NodeRegistry, selfNodeID string, selector PeerSelector, forwarder DispatchForwarder, meter metric.Meter) (*PeerRouter, error)`
- `func (r *PeerRouter) Peers(tenantID, tagsCSV string) []contract.NodeInfo`
- `func (r *PeerRouter) HandOver(ctx context.Context, peer contract.NodeInfo, call internalgrpc.Callout, triesLeft int, major uint32) HandOverAnswer`
  - `ctx` must carry the user context and the deadline of the wait. `HandOver` does not report the caller's context ending as such (the seam has no field for it): a hand-over cut off by `ctx` is a lost answer. After it returns, the owner checks its **own** context — `ctx.Err()` with `context.Cause(ctx) != contract.ErrCalloutDeadline` means the caller went away and that error is returned unchanged.
  - `Warnings` are returned for the caller to add with `common.AddWarning`; the peer's *error* diagnostics are added to `ctx` by `HandOver` itself (Open point 4).
  - The request's `ownerNodeID` is the router's `selfNodeID`; `call.RequestID`, `call.AnswerLimit` (≥ 1 ms), `call.RepeatSafe`, `call.Outer`, `call.TxID` and `call.Source` must be set.
- `func (r *PeerRouter) Changed() <-chan struct{}`
- `type LocalRunner interface { RunLocal(context.Context, internalgrpc.Callout, int) internalgrpc.LocalResult }`; `func NewDispatchHandler(local LocalRunner, auth PeerAuth) *DispatchHandler`
- `func NewHTTPForwarder(auth PeerAuth, connectTimeout time.Duration) *HTTPForwarder`; `type ForwardError struct{ Stage ForwardStage; Err error }`
- `internal/grpc`: `type CalloutSource struct{…}`, `Callout.Source` (P-3).
- Instrument `cyoda.dispatch.handovers` (`outcome`).
- **Left for the owner's-loop stream to delete, with their tests:** `ClusterDispatcher`, `NewClusterDispatcher`, `AnswerLimitResolver`, `forwardWithFailover`, `findPeerWithPolling`, `findPeer`, `isNoMatchingMember`, `gossipPollInterval`; `cluster_dispatcher.go`, `cluster_dispatcher_test.go`, `cluster_dispatcher_failover_test.go`, `integration_test.go`. `remintPeerError` is already deleted by P-6. The fixtures this stream's tests share were moved to `fixtures_test.go` in P-4 and must stay. That stream also re-points `app.go` from `NewClusterDispatcher(…)` to its Coordinator, keeping the `NewPeerRouter(…)` lines.

**P consumes**

- **L:** `contract.CalloutFailure`, `CalloutFailureKind` (`String`, `MayTryAnother`), `CalloutAttempt`, `contract.ErrNoMatchingMember`, `contract.ErrAuthContextUnavailable` (existing); `internalgrpc.Callout` and its three builders (L-6), `CalloutKind`, `CalloutResult`, `LocalResult`, `NewMinorNumberer`, `(*ProcessorDispatcher).RunLocal`, `.ResolveAnswerLimit` (L-5, L-8); `Callout.Outer` and per-try minting (L-10). P-6 must land before L-11.
- **F:** `token.Pair{Callout string; Major, Minor uint32}`.
- **M:** `contract.NodeRegistry.Changed()` on the interface and on `stubNodeRegistry` (M-5); `common.NewChangeSignal()` (M-1).
- **C:** `cfg.Cluster.DispatchConnectTimeout`, `cfg.Callout.HandoverAllowance` (C-2). C-11 can remove `CYODA_TX_TOKEN_TTL` once P-6 and L-10 are in.
- **H:** nothing at the U layer.

## Open points

1. **The seam cannot be implemented on `Callout` as L-6 leaves it.** `HandOver` receives a `Callout`, whose request builder and response mapper are closures over the entity and the definition (L-6, `internal/grpc/callout.go`); none of the entity, the processor definition, the criterion JSON, the function, or the workflow and transition names is an exported field, and all of them must go on the wire (`types.go:20-46`). P-3 adds `Callout.Source`. It touches a file of stream L; the alternative — a second parameter on `HandOver` — would change the seam.
2. **A sealed `no_handoff` for a *replayed nonce* lets an active attacker on the inter-pnode network turn "unknown" into "nothing happened".** Sequence: capture request R in flight, let the peer run it, replay R to the peer, receive a sealed `no_handoff` bound to R's nonce, and deliver *that* to the owner in place of the real answer. The owner then gives a non-idempotent processor to a second cnode. Before this design the same attacker could only make the answer be lost. The **full-cache** case the spec gives as the reason (§6, "so that a saturated cache does not fail non-repeat-safe operations") is not exposed the same way: an attacker without the key cannot fill the cache, since only requests that open are recorded (`aead_peer_auth.go:184-188`) and a captured one is a duplicate, not a new entry. A genuine duplicate nonce never occurs between honest pnodes (96 random bits; with `DisableKeepAlives` the transport does not re-send). Proposal for Paul: keep the bare 403 for a **duplicate** nonce and answer under seal only for a **full** cache. Planned as the spec says (both sealed); the change would be `nonceCache.checkAndRecord` returning which of the two it was, and one more sentinel — about ten lines in P-1 and one arm in P-6.
3. **§6 leaves the peer's bare 400s unaddressed** (`handler.go:51, 70, 127`: a body that does not parse, the two tenants disagreeing, an unknown kind). Under "any non-2xx is `no_answer`" they would surface as a retryable 503 `DISPATCH_FORWARD_FAILED` and use a try, for a request that every pnode refuses identically. Planned (P-6): an authenticated `terminal` with `triesUsed: 0`, reported by the owner as a ticketed 500. `docs/ARCHITECTURE.md`'s "a mismatch is `400`" changes with it.
4. **Diagnostics across the hand-over.** The seam returns `Warnings`; it has no place for the `common.AddError` diagnostics a failed try raises on the peer (`dispatch.go:180`), and today's handler carries neither (`Warnings` is declared and never filled). Planned: the response carries both; `HandOver` returns the warnings, as the seam says, and adds the errors to `ctx` itself. One rule for both would be cleaner — either `HandOver` adds both, or the answer gains `Errors []string`. The owner's-loop plan should say which it assumes; changing P is three lines.
5. **§6 "a peer address that fails validation … is `Terminal`".** An address is a property of one peer, not of the callout: another peer's may be valid. Planned as written — it fails closed, and a registry handing out a loopback or link-local address is a misconfiguration worth failing loudly on — but it is not "would fail identically for every try" in the sense the other two cases are.
6. **`Peers` takes no context** in the seam, while `contract.NodeRegistry.List` takes one. `PeerRouter.Peers` passes `context.Background()`; `Gossip.List` and `Local.List` read memory and ignore it.
7. **Message of a re-minted peer failure.** Today the owner substitutes `peer node dispatch failed` for the peer's text (`cluster_dispatcher.go:428`). §8.2 wants "the try's own code … message as today" for a `NoAnswer` — the *single-pnode* message. Planned: the owner re-mints with the client-safe `Cause` of the peer's last attempt (L guarantees `CalloutFailure.Message` is client-safe), falling back to the generic text only when the peer sent none. Node ids and peer addresses still never appear.
