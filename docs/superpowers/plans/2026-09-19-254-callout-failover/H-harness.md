# Stream H — test harnesses

These tasks come before the feature code. Every one is verified against
**today's single-shot server**: no task below asserts anything the feature
changes (which of two same-tag cnodes is chosen, a retryable flag reaching the
client, `CALLOUT_SUPERSEDED`, a wait on a single pnode). Where a self-test
observes a server answer it asserts only what holds both today and after the
feature, and says so.

**Local-machine facts for every task here.**
- `internal/e2e` needs Docker (its `TestMain` starts a PostgreSQL testcontainer),
  including for the pure unit tests that live in that package.
- The parity fixtures build `cmd/cyoda` and `cmd/compute-test-client` in a
  `go build` subprocess the Go test cache cannot see. A `(cached)` line for a
  parity package after a change under `cmd/` or `internal/` is not evidence;
  the tiers (`make test`) run those packages uncached and are the final word.
  Do not add `-count=1` or `-v` to the commands below.
- Verification points: no **V-n** of the spec falls in §13's harness paragraph.

**What the harness deliberately does not offer (waivers ¹ ² ³ of spec §13).**
¹ No helper makes a hand-off fail from outside the process — unit tests drive
`Member.Send` (stream U). ² The parity capability is not for "no cnode → one
attaches → success": patience is fixed per package and a subprocess start races
it; that scenario uses `AttachCnode` in `internal/e2e` (Task H-3). ³ The
multi-node capability does not expose killing a pnode.

---

### Task H-1: e2e callback harness — a stack with no cnode, and an API connection of its own

**Spec:** §13 "What each harness can do" (first bullet)

**Files:**
- Modify: `internal/e2e/callback_harness_test.go` (`callbackHarness` struct :133-155; `newCallbackHarnessConfigured` :174-245)
- Modify: `internal/e2e/callback_txjoin_grpc_search_test.go` (:72, :99), `internal/e2e/search_intx_test.go` (:93), `internal/e2e/storage_ceilings_e2e_test.go` (:659, :686, :705) — `h.member.conn` → `h.apiConn`
- Test: `internal/e2e/callout_harness_selftest_test.go` (new; every e2e harness self-test of this stream goes here)

**Interfaces:**
- Consumes: nothing from other streams.
- Produces:
  - `func newCalloutHarness(t *testing.T, configure func(*app.Config)) *callbackHarness` — the full stack (PostgreSQL, HTTP, gRPC, JWT) with **no** cnode attached; `h.member == nil`. `configure` may be nil; it is where a test sets patience, tries and answer-limit settings on `app.Config`.
  - `callbackHarness.grpcAddr string`, `callbackHarness.apiConn *grpc.ClientConn` — every gRPC API helper uses `apiConn`, never a cnode's connection.
  - `newCallbackHarness` / `newCallbackHarnessConfigured` unchanged in signature and behaviour (stack + the default cnode).

Existing tests: every user of `newCallbackHarness*` stays. The six `h.member.conn` sites are rewritten to `h.apiConn` (a cnode's connection is no longer the API connection, so a harness with no cnode, or whose cnode closed its stream, can still call the gRPC API).

- [ ] **Step 1: Write the failing test**

```go
package e2e_test

import (
	"testing"
)

// callout_harness_selftest_test.go holds the self-tests of the callout test
// harness itself: each proves one harness capability against the running
// stack, so a scenario built on the harness cannot pass or fail because of
// the harness.

// TestCalloutHarness_StartsWithNoCnode proves newCalloutHarness attaches no
// cnode and that the gRPC API is reachable without one.
func TestCalloutHarness_StartsWithNoCnode(t *testing.T) {
	h := newCalloutHarness(t, nil)

	if h.member != nil {
		t.Fatal("newCalloutHarness attached a default cnode; it must attach none")
	}
	if n := len(h.app.MemberRegistry().List()); n != 0 {
		t.Fatalf("member registry holds %d cnodes on a fresh callout harness; want 0", n)
	}

	const model = "h1-no-cnode"
	h.SetupModelWithWorkflow(t, model, secondaryWorkflow)
	env, err := h.createEntityGRPC(model, 1, workflowSampleModel)
	if err != nil {
		t.Fatalf("gRPC create over the harness API connection: %v", err)
	}
	if !env.Success {
		code := ""
		if env.Error != nil {
			code = env.Error.Code + ": " + env.Error.Message
		}
		t.Fatalf("gRPC create over the harness API connection failed: %s", code)
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/e2e/ -run 'TestCalloutHarness_StartsWithNoCnode'`
Expected: FAIL (build) with `undefined: newCalloutHarness`.

- [ ] **Step 3: Implement**

`callbackHarness` gains two fields (after `baseURL`):

```go
	// grpcAddr is the stack's gRPC listener address; cnodes dial it.
	grpcAddr string
	// apiConn carries the harness's own gRPC API calls (EntityManage,
	// EntitySearch, …). It belongs to no cnode, so it works on a stack with no
	// cnode attached and survives a cnode closing its stream.
	apiConn *grpc.ClientConn
	// member is the default cnode; nil on a harness built by newCalloutHarness.
	member *computeMember
```

`newCallbackHarnessConfigured` is split. Everything it does today up to and
including `t.Cleanup(a.Shutdown)` moves, unchanged, into `newCalloutHarness`,
which then dials `apiConn` and seeds the cached bearer:

```go
// newCalloutHarness stands up the full stack with NO cnode attached. Tests
// that script their own cnodes — several of them, attached and detached
// mid-test — start here. configure may be nil.
func newCalloutHarness(t *testing.T, configure func(*app.Config)) *callbackHarness {
	t.Helper()

	// … the body of today's newCallbackHarnessConfigured from "Fresh JWT
	// signing key" through "t.Cleanup(a.Shutdown)", unchanged …

	h.grpcAddr = grpcLis.Addr().String()
	apiConn, err := grpc.NewClient(h.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial gRPC API connection: %v", err)
	}
	h.apiConn = apiConn
	t.Cleanup(func() { _ = apiConn.Close() })

	// Seed the cached bearer on the test goroutine: callback() and grpcCtx()
	// read it from other goroutines and cannot fetch it themselves.
	h.token(t)
	return h
}

func newCallbackHarnessConfigured(t *testing.T, configure func(*app.Config)) *callbackHarness {
	t.Helper()
	h := newCalloutHarness(t, configure)
	h.member = newComputeMember(t, h, h.grpcAddr)
	t.Cleanup(h.member.stop)
	return h
}
```

(The doc comment of `newCallbackHarnessConfigured` stays on it; the
"move unchanged" block is lines 177-238 as found.) Cleanup order is preserved:
`member.stop` is still registered last, so it still runs first.

In the three listed test files replace each
`cyodapb.NewCloudEventsServiceClient(h.member.conn)` with
`cyodapb.NewCloudEventsServiceClient(h.apiConn)` and drop "(the member's
connection)" from the three doc comments in `storage_ceilings_e2e_test.go`.

Exit check: `grep -rn 'h\.member\.conn' internal/e2e/` → no hits.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/e2e/...` — the whole package, all green.

- [ ] **Step 5: Commit**

```
git add internal/e2e/callback_harness_test.go internal/e2e/callout_harness_selftest_test.go \
  internal/e2e/callback_txjoin_grpc_search_test.go internal/e2e/search_intx_test.go internal/e2e/storage_ceilings_e2e_test.go
git commit -m "test(e2e): callout harness with no cnode and an API connection of its own (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task H-2: e2e callback harness — one cnode implementation: join with given tags and bearer, one request parser, one reply model

**Spec:** §13 "What each harness can do" (first bullet: "answer ok / answer failure with or without a retryable verdict / never answer / close the stream")

**Files:**
- Modify: `internal/e2e/callback_harness_test.go` (`computeMember` :514-528; `newComputeMember` :533-651; `handleCalcRequest` :677-747, `handleCriteriaRequest` :753-819, `handleFunctionRequest` :828-895 — the three are deleted and replaced)
- Test: `internal/e2e/callout_harness_selftest_test.go`

**Interfaces:**
- Consumes: `newCalloutHarness`, `h.grpcAddr` (H-1); `h.provisionTenant`, `h.fetchTokenFor` (`callback_txjoin_errors_test.go:62, 83`, existing).
- Produces:
  - `const calloutProcessor, calloutCriterion, calloutFunction = "processor", "criterion", "function"`
  - `type memberSpec struct { bearer string; tags []string; handle calcHandler }`
  - `type calcHandler func(m *computeMember, send func(*cepb.CloudEvent) error, req calcRequest)`
  - `func newComputeMember(t *testing.T, h *callbackHarness, spec memberSpec) *computeMember` (signature changed; one caller)
  - `computeMember.id string` (the member id from the greet), `computeMember.ctx context.Context` (ends when the cnode stops or closes its stream), `func (m *computeMember) closeStream()`
  - `type calcRequest struct { kind, name, requestID, replyID, eventID string; rc *reqCtx }`
  - `type cnodeReply` with constructors `answerOK()`, `answerData(map[string]any)`, `answerMatches(bool)`, `answerResult(resultKind string, result map[string]any)`, `answerFail(msg string)`, `answerFailVerdict(msg string, retryable bool)`, `neverAnswer()`, `closeStream()`
  - `func (r cnodeReply) cloudEvent(req calcRequest) (*cepb.CloudEvent, error)` — `(nil, nil)` for `neverAnswer` and `closeStream`

Existing tests: all users of `RegisterProc` / `RegisterCriteria` / `RegisterFunction` stay and must pass unchanged — the default cnode answers exactly as before (same reply payloads; the join now sends `joinedLegalEntityId: ""`, which the server resolves from the bearer, `streaming.go:63-66`).

- [ ] **Step 1: Write the failing tests** (append to `callout_harness_selftest_test.go`; add imports `encoding/json`, `slices`, `internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"`)

```go
// TestComputeMember_JoinsWithGivenTagsAndBearer proves a cnode joins under the
// tags and the tenant it was given, and learns its member id from the greet.
func TestComputeMember_JoinsWithGivenTagsAndBearer(t *testing.T) {
	h := newCalloutHarness(t, nil)

	own := newComputeMember(t, h, memberSpec{tags: []string{"h2-a", "h2-b"}, handle: h.handleRegistered})
	t.Cleanup(own.stop)
	if own.id == "" {
		t.Fatal("member id from the greet is empty")
	}
	got := h.app.MemberRegistry().Get(own.id)
	if got == nil {
		t.Fatalf("registry has no member %s", own.id)
	}
	if !slices.Equal(got.Tags, []string{"h2-a", "h2-b"}) {
		t.Errorf("tags = %v; want [h2-a h2-b]", got.Tags)
	}
	if string(got.TenantID) != "test-tenant" {
		t.Errorf("tenant = %q; want test-tenant", got.TenantID)
	}

	clientID, secret := h.provisionTenant(t, "h2-other-tenant", "h2-user")
	other := newComputeMember(t, h, memberSpec{
		bearer: h.fetchTokenFor(t, clientID, secret),
		tags:   []string{"h2-a"},
		handle: h.handleRegistered,
	})
	t.Cleanup(other.stop)
	if m := h.app.MemberRegistry().Get(other.id); m == nil || string(m.TenantID) != "h2-other-tenant" {
		t.Errorf("second cnode did not join under the bearer's tenant: %+v", m)
	}
}

// TestCnodeReply_CloudEvent pins the wire shape of every reply a cnode can give.
func TestCnodeReply_CloudEvent(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		reply    cnodeReply
		wantType string
		want     string // the reply payload, as JSON; "" = nothing is sent
	}{
		{"processor ok, unchanged", calloutProcessor, answerOK(), internalgrpc.EntityProcessorCalculationResponse,
			`{"requestId":"r-1","success":true}`},
		{"processor ok, data", calloutProcessor, answerData(map[string]any{"k": "v"}), internalgrpc.EntityProcessorCalculationResponse,
			`{"requestId":"r-1","success":true,"payload":{"data":{"k":"v"}}}`},
		{"criterion ok", calloutCriterion, answerMatches(true), internalgrpc.EntityCriteriaCalculationResponse,
			`{"requestId":"r-1","success":true,"matches":true}`},
		{"function ok", calloutFunction, answerResult("Schedule", map[string]any{"fireAfterMs": 5}), internalgrpc.EntityFunctionCalculationResponse,
			`{"requestId":"r-1","success":true,"resultKind":"Schedule","result":{"fireAfterMs":5}}`},
		{"failure, no verdict", calloutProcessor, answerFail("boom"), internalgrpc.EntityProcessorCalculationResponse,
			`{"requestId":"r-1","success":false,"error":{"message":"boom"}}`},
		{"failure, retryable", calloutCriterion, answerFailVerdict("boom", true), internalgrpc.EntityCriteriaCalculationResponse,
			`{"requestId":"r-1","success":false,"error":{"message":"boom","retryable":true}}`},
		{"failure, not retryable", calloutFunction, answerFailVerdict("boom", false), internalgrpc.EntityFunctionCalculationResponse,
			`{"requestId":"r-1","success":false,"error":{"message":"boom","retryable":false}}`},
		{"never answer", calloutProcessor, neverAnswer(), "", ""},
		{"close stream", calloutProcessor, closeStream(), "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ce, err := tc.reply.cloudEvent(calcRequest{kind: tc.kind, replyID: "r-1"})
			if err != nil {
				t.Fatalf("cloudEvent: %v", err)
			}
			if tc.want == "" {
				if ce != nil {
					t.Fatalf("reply sends %s; want nothing sent", ce.GetType())
				}
				return
			}
			if ce.GetType() != tc.wantType {
				t.Errorf("type = %s; want %s", ce.GetType(), tc.wantType)
			}
			var got, want any
			if err := json.Unmarshal([]byte(ce.GetTextData()), &got); err != nil {
				t.Fatalf("reply payload is not JSON: %v", err)
			}
			_ = json.Unmarshal([]byte(tc.want), &want)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("payload = %s; want %s", gotJSON, wantJSON)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/e2e/ -run 'TestComputeMember_JoinsWithGivenTagsAndBearer|TestCnodeReply_CloudEvent'`
Expected: FAIL (build) with `undefined: memberSpec` (and `cnodeReply`, `calcRequest`, `answerOK`, …).

- [ ] **Step 3: Implement** (all in `callback_harness_test.go`)

Request parsing and the reply model — replace the three `handle*Request`
functions (:677-895) with:

```go
// The three kinds of callout a cnode receives.
const (
	calloutProcessor = "processor"
	calloutCriterion = "criterion"
	calloutFunction  = "function"
)

// calcRequest is one calculation request as a cnode received it.
type calcRequest struct {
	kind      string // calloutProcessor | calloutCriterion | calloutFunction
	name      string // processor / criterion / function name
	requestID string // the payload's requestId, exactly as sent
	replyID   string // what the reply echoes: requestId, else the payload id
	eventID   string // the CloudEvent id
	rc        *reqCtx
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// parseCalcRequest decodes a calculation request of any kind. The pass rides
// as a CloudEvent attribute and lands in rc.token; it is never logged.
func (h *callbackHarness) parseCalcRequest(evtType string, ce *cepb.CloudEvent, payload []byte) calcRequest {
	var body struct {
		RequestID     string `json:"requestId"`
		ID            string `json:"id"`
		EntityID      string `json:"entityId"`
		ProcessorName string `json:"processorName"`
		ProcessorID   string `json:"processorId"`
		CriteriaName  string `json:"criteriaName"`
		CriteriaID    string `json:"criteriaId"`
		FunctionName  string `json:"functionName"`
		FunctionID    string `json:"functionId"`
		Payload       *struct {
			Data json.RawMessage `json:"data"`
			Meta map[string]any  `json:"meta"`
		} `json:"payload"`
	}
	_ = json.Unmarshal(payload, &body)

	req := calcRequest{
		requestID: body.RequestID,
		replyID:   firstNonEmpty(body.RequestID, body.ID),
		eventID:   ce.GetId(),
	}
	switch evtType {
	case internalgrpc.EntityCriteriaCalculationRequest:
		req.kind, req.name = calloutCriterion, firstNonEmpty(body.CriteriaName, body.CriteriaID)
	case internalgrpc.EntityFunctionCalculationRequest:
		req.kind, req.name = calloutFunction, firstNonEmpty(body.FunctionName, body.FunctionID)
	default:
		req.kind, req.name = calloutProcessor, firstNonEmpty(body.ProcessorName, body.ProcessorID)
	}
	req.rc = &reqCtx{
		token:     internalgrpc.TxTokenFromCloudEvent(ce),
		requestID: req.replyID,
		entityID:  body.EntityID,
		h:         h,
	}
	if body.Payload != nil {
		req.rc.entityMeta = body.Payload.Meta
		var d map[string]any
		if json.Unmarshal(body.Payload.Data, &d) == nil {
			req.rc.entityData = d
		}
	}
	return req
}

type cnodeReplyKind int

const (
	replyOK cnodeReplyKind = iota
	replyFail
	replySilent
	replyCloseStream
)

// cnodeReply is what a cnode does with one callout.
type cnodeReply struct {
	kind       cnodeReplyKind
	data       map[string]any // replyOK, processor: the entity's new data; nil = unchanged
	matches    bool           // replyOK, criterion
	resultKind string         // replyOK, function
	result     map[string]any // replyOK, function
	message    string         // replyFail
	retryable  *bool          // replyFail: the cnode's verdict; nil = none given
}

// answerOK answers success: a processor leaves the entity unchanged, a
// criterion matches. A function needs answerResult — it has no neutral answer.
func answerOK() cnodeReply                       { return cnodeReply{kind: replyOK, matches: true} }
func answerData(data map[string]any) cnodeReply { return cnodeReply{kind: replyOK, data: data} }
func answerMatches(m bool) cnodeReply           { return cnodeReply{kind: replyOK, matches: m} }
func answerResult(resultKind string, result map[string]any) cnodeReply {
	return cnodeReply{kind: replyOK, resultKind: resultKind, result: result}
}

// answerFail answers success=false with no verdict on retrying.
func answerFail(msg string) cnodeReply { return cnodeReply{kind: replyFail, message: msg} }

// answerFailVerdict answers success=false and states whether a retry is worthwhile.
func answerFailVerdict(msg string, retryable bool) cnodeReply {
	return cnodeReply{kind: replyFail, message: msg, retryable: &retryable}
}

// neverAnswer takes the work and stays silent; the stream stays open.
func neverAnswer() cnodeReply { return cnodeReply{kind: replySilent} }

// closeStream closes the cnode's stream on receiving the work, without answering.
func closeStream() cnodeReply { return cnodeReply{kind: replyCloseStream} }

// cloudEvent builds the reply for req, or (nil, nil) when nothing is sent.
func (r cnodeReply) cloudEvent(req calcRequest) (*cepb.CloudEvent, error) {
	if r.kind == replySilent || r.kind == replyCloseStream {
		return nil, nil
	}
	respType := internalgrpc.EntityProcessorCalculationResponse
	switch req.kind {
	case calloutCriterion:
		respType = internalgrpc.EntityCriteriaCalculationResponse
	case calloutFunction:
		respType = internalgrpc.EntityFunctionCalculationResponse
	}
	body := map[string]any{"requestId": req.replyID, "success": r.kind == replyOK}
	if r.kind == replyFail {
		e := map[string]any{"message": r.message}
		if r.retryable != nil {
			e["retryable"] = *r.retryable
		}
		body["error"] = e
		return internalgrpc.NewCloudEvent(respType, body)
	}
	switch req.kind {
	case calloutCriterion:
		body["matches"] = r.matches
	case calloutFunction:
		body["resultKind"] = r.resultKind
		body["result"] = r.result
	default:
		if r.data != nil {
			body["payload"] = map[string]any{"data": r.data}
		}
	}
	return internalgrpc.NewCloudEvent(respType, body)
}

// sendReply puts reply on the wire. A reply that cannot be built is reported
// to the server as a failure rather than dropped.
func sendReply(send func(*cepb.CloudEvent) error, req calcRequest, reply cnodeReply) {
	ce, err := reply.cloudEvent(req)
	if err != nil {
		ce, _ = answerFail(fmt.Sprintf("failed to build response: %v", err)).cloudEvent(req)
	}
	if ce != nil {
		_ = send(ce)
	}
}

// registeredReply runs the closure registered under name (RegisterProc /
// RegisterCriteria / RegisterFunction) and turns its outcome into a reply.
func (h *callbackHarness) registeredReply(kind, name string, rc *reqCtx) cnodeReply {
	switch kind {
	case calloutCriterion:
		fn, ok := h.lookupCrit(name)
		if !ok {
			return answerFail(fmt.Sprintf("no callback criterion registered for %q", name))
		}
		matches, err := fn(rc)
		if err != nil {
			return answerFail(err.Error())
		}
		return answerMatches(matches)
	case calloutFunction:
		fn, ok := h.lookupFunc(name)
		if !ok {
			return answerFail(fmt.Sprintf("no callback function registered for %q", name))
		}
		resultKind, result, err := fn(rc)
		if err != nil {
			return answerFail(err.Error())
		}
		return answerResult(resultKind, result)
	default:
		fn, ok := h.lookupProc(name)
		if !ok {
			return answerFail(fmt.Sprintf("no callback processor registered for %q", name))
		}
		data, err := fn(rc)
		if err != nil {
			return answerFail(err.Error())
		}
		return answerData(data)
	}
}

// handleRegistered is the default cnode's handler.
func (h *callbackHarness) handleRegistered(_ *computeMember, send func(*cepb.CloudEvent) error, req calcRequest) {
	sendReply(send, req, h.registeredReply(req.kind, req.name, req.rc))
}
```

The cnode itself — replace `computeMember` and `newComputeMember`:

```go
// calcHandler handles one calculation request a cnode received. It runs on a
// goroutine of its own, so it may block and must not call t.Fatal.
type calcHandler func(m *computeMember, send func(*cepb.CloudEvent) error, req calcRequest)

// memberSpec says how a cnode joins and what it does with work.
type memberSpec struct {
	bearer string   // M2M bearer to join with; "" = the harness's own tenant
	tags   []string // join tags
	handle calcHandler
}

type computeMember struct {
	id     string // member id the server gave in the greet
	conn   *grpc.ClientConn
	ctx    context.Context // ends when the cnode stops or closes its stream
	cancel context.CancelFunc
	done   chan struct{}

	// sendMu serialises stream.Send — gRPC bidi streams are not safe for
	// concurrent Send, and calc requests are handled on concurrent goroutines
	// (a depth-2 nested cascade needs the cnode to run the inner processor
	// while the outer processor's callback is still in flight).
	sendMu sync.Mutex
	// handlers tracks in-flight handlers so teardown can drain them.
	handlers sync.WaitGroup
}

// closeStream ends the cnode's stream from the client side, as a crashed or
// partitioned compute program would. The server sees the stream end and evicts
// the member; work it was given and has not answered fails as disconnected.
func (m *computeMember) closeStream() { m.cancel() }

// newComputeMember dials the stack's gRPC server, opens StartStreaming with
// spec.bearer, joins with spec.tags, waits for the greet, then runs a receive
// loop handing each calculation request to spec.handle on its own goroutine.
func newComputeMember(t *testing.T, h *callbackHarness, spec memberSpec) *computeMember {
	t.Helper()
	bearer := spec.bearer
	if bearer == "" {
		bearer = h.token(t)
	}

	conn, err := grpc.NewClient(h.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial gRPC: %v", err)
	}
	client := cyodapb.NewCloudEventsServiceClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.StartStreaming(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+bearer))
	if err != nil {
		cancel()
		conn.Close()
		t.Fatalf("StartStreaming: %v", err)
	}

	// joinedLegalEntityId is left empty: the server takes the tenant from the
	// bearer, so one join shape serves every tenant.
	joinCE, err := internalgrpc.NewCloudEvent(internalgrpc.CalculationMemberJoinEvent, map[string]any{
		"id":                  "callback-member-join",
		"tags":                spec.tags,
		"joinedLegalEntityId": "",
	})
	if err != nil {
		cancel()
		conn.Close()
		t.Fatalf("build join event: %v", err)
	}
	if err := stream.Send(joinCE); err != nil {
		cancel()
		conn.Close()
		t.Fatalf("send join: %v", err)
	}

	greeted := make(chan struct{})
	m := &computeMember{conn: conn, ctx: ctx, cancel: cancel, done: make(chan struct{})}

	send := func(ce *cepb.CloudEvent) error {
		m.sendMu.Lock()
		defer m.sendMu.Unlock()
		return stream.Send(ce)
	}

	go func() {
		defer close(m.done)
		var greetOnce sync.Once
		for {
			ce, err := stream.Recv()
			if err != nil {
				return // stream closed / context cancelled
			}
			evtType, payload, perr := internalgrpc.ParseCloudEvent(ce)
			if perr != nil {
				continue
			}
			switch evtType {
			case internalgrpc.CalculationMemberGreetEvent:
				greetOnce.Do(func() {
					var greet struct {
						MemberID string `json:"memberId"`
					}
					_ = json.Unmarshal(payload, &greet)
					// Written before greeted closes and before any handler
					// goroutine starts: the server writes the greet first.
					m.id = greet.MemberID
					close(greeted)
				})
			case internalgrpc.CalculationMemberKeepAliveEvent:
				ka, kerr := internalgrpc.NewCloudEvent(internalgrpc.CalculationMemberKeepAliveEvent, map[string]any{
					"id":      ce.Id,
					"success": true,
				})
				if kerr == nil {
					_ = send(ka)
				}
			case internalgrpc.EntityProcessorCalculationRequest,
				internalgrpc.EntityCriteriaCalculationRequest,
				internalgrpc.EntityFunctionCalculationRequest:
				// Handled concurrently: a handler may block on a callback that
				// drives a further callout to this same cnode.
				req := h.parseCalcRequest(evtType, ce, payload)
				m.handlers.Add(1)
				go func() {
					defer m.handlers.Done()
					spec.handle(m, send, req)
				}()
			default:
				// ignore other server events
			}
		}
	}()

	select {
	case <-greeted:
	case <-time.After(10 * time.Second):
		cancel()
		conn.Close()
		t.Fatal("compute member: timed out waiting for greet")
	}
	return m
}
```

`stop()` is unchanged. The one caller, in `newCallbackHarnessConfigured`, becomes:

```go
	// Tagged "sched-fn" so a schedule.function callout — whose
	// calculationNodesTags is validated non-empty at import — can route to it.
	// Processor/criteria tests configure calculationNodesTags:"" which matches
	// any cnode of the tenant.
	h.member = newComputeMember(t, h, memberSpec{tags: []string{"sched-fn"}, handle: h.handleRegistered})
```

Exit checks:
`grep -n 'func (h \*callbackHarness) handleCalcRequest\|handleCriteriaRequest\|handleFunctionRequest' internal/e2e/` → no hits.
`grep -rn '"joinedLegalEntityId": *"test-tenant"' internal/e2e/` → no hits.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/e2e/...` — the whole package. The callback, scheduled-function, attribution and transaction-control tests are the regression net for the default cnode.

- [ ] **Step 5: Commit**

```
git add internal/e2e/callback_harness_test.go internal/e2e/callout_harness_selftest_test.go
git commit -m "test(e2e): one cnode implementation — given tags and bearer, one parser, one reply model (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task H-3: e2e callback harness — several scripted cnodes, attached and detached mid-test, recording what they receive

**Spec:** §13 "What each harness can do" (first bullet); enables the **E** column of the rows listed in the stream interface summary.

**Files:**
- Create: `internal/e2e/scripted_cnode_test.go`
- Modify: `internal/e2e/callback_harness_test.go` (`callbackHarness` struct: one field; `newCallbackHarnessConfigured`: the default cnode is attached through `AttachCnode`; `handleRegistered` deleted)
- Test: `internal/e2e/callout_harness_selftest_test.go`

**Interfaces:**
- Consumes: `newCalloutHarness` (H-1); `memberSpec`, `newComputeMember`, `calcRequest`, `cnodeReply` and its constructors, `sendReply`, `registeredReply` (H-2); `scheduleFunctionWorkflowJSON` (`scheduled_function_test.go:57`), `problemErrorCode` (`callback_txjoin_errors_test.go:39`), `workflowSampleModel` (`workflow_test.go:11`) — existing.
- Produces:
  - `type cnodeSpec struct { name string; tags []string; bearer string; script cnodeScript }` — `name` required; `bearer ""` = the harness's tenant; `script nil` = `scriptAlways(answerOK())`
  - `type cnodeScript func(ctx context.Context, call receivedCallout, rc *reqCtx) cnodeReply` — runs on a per-callout goroutine; may block (on a channel the test closes) and may make callbacks through `rc` before replying; `ctx` ends when the cnode is detached or closes its stream
  - `func scriptAlways(r cnodeReply) cnodeScript`
  - `func scriptSequence(replies ...cnodeReply) cnodeScript` — the n-th callout this cnode receives gets the n-th reply; the last repeats
  - `type receivedCallout struct { Seq int; Cnode, MemberID, Kind, Name, RequestID, EventID, EntityID string }` plus `func (r receivedCallout) Pass() string`; `String()`/`GoString()` omit the pass. `Seq` is 1-based arrival order across **every** scripted cnode of the harness.
  - `func (h *callbackHarness) AttachCnode(t *testing.T, spec cnodeSpec) *scriptedCnode` — returns once the server has registered the cnode (the greet is the proof, `members.go:388`); callable at any point in a test
  - `func (c *scriptedCnode) Detach(t *testing.T)` — closes the connection and returns once the server's registry no longer holds the member; idempotent
  - `func (c *scriptedCnode) AwaitGone(t *testing.T)` — the same wait alone, for a cnode whose script closed its stream
  - `func (c *scriptedCnode) MemberID() string`, `func (c *scriptedCnode) Received() []receivedCallout`
  - `func (h *callbackHarness) ReceivedCallouts() []receivedCallout`, `func (h *callbackHarness) AwaitCallouts(t *testing.T, n int, within time.Duration) []receivedCallout`
  - `func procWorkflowJSON(wfName, procName, mode string, config map[string]any) string` — NONE→DONE with one processor; `config` is merged over `{"attachEntity": true, "calculationNodesTags": ""}`, so a feature test passes `idempotent`, `retryPolicy`, `responseTimeoutMs`, tags

Existing tests: all stay. The default cnode becomes a scripted cnode named `default`, so its callouts are recorded too; `h.member` keeps its type (`*computeMember`) and its `t.Cleanup` ordering, which `transaction_control_test.go:36-41, 73-78` rely on.

Two facts of today's server the self-tests are shaped by: a callout whose `calculationNodesTags` is empty matches **any** cnode of the tenant (`common.TagsOverlap`), and among several matching cnodes the choice is map-iteration order (`members.go:470-481`). So each self-test gives every cnode a tag of its own and every processor exactly one matching cnode.

- [ ] **Step 1: Write the failing tests** (append to `callout_harness_selftest_test.go`; add imports `fmt`, `net/http`, `strings`, `time`)

```go
// TestScriptedCnodes_EachReceivesOnlyItsTag: two cnodes with different tags
// each receive only their tag's callout, and the harness records request id,
// CloudEvent id, pass and arrival order.
func TestScriptedCnodes_EachReceivesOnlyItsTag(t *testing.T) {
	h := newCalloutHarness(t, nil)
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{"h3-tag-a"}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{"h3-tag-b"}})

	h.SetupModelWithWorkflow(t, "h3-model-a", procWorkflowJSON("h3-wf-a", "h3-proc-a", "SYNC",
		map[string]any{"calculationNodesTags": "h3-tag-a"}))
	h.SetupModelWithWorkflow(t, "h3-model-b", procWorkflowJSON("h3-wf-b", "h3-proc-b", "SYNC",
		map[string]any{"calculationNodesTags": "h3-tag-b"}))

	for _, model := range []string{"h3-model-a", "h3-model-b"} {
		if _, status, body := h.CreateEntity(t, model, 1, workflowSampleModel); status != http.StatusOK {
			t.Fatalf("create %s: %d %s", model, status, body)
		}
	}

	recs := h.ReceivedCallouts()
	if len(recs) != 2 {
		t.Fatalf("recorded %d callouts; want 2: %v", len(recs), recs)
	}
	want := []struct {
		cnode    *scriptedCnode
		name     string
		procName string
	}{{a, "a", "h3-proc-a"}, {b, "b", "h3-proc-b"}}
	for i, w := range want {
		r := recs[i]
		if r.Seq != i+1 || r.Cnode != w.name || r.Name != w.procName || r.Kind != calloutProcessor {
			t.Errorf("record %d = %v; want seq %d on cnode %s for %s", i, r, i+1, w.name, w.procName)
		}
		if r.MemberID != w.cnode.MemberID() || r.MemberID == "" {
			t.Errorf("record %d member id = %q; want %q", i, r.MemberID, w.cnode.MemberID())
		}
		if r.RequestID == "" || r.EventID == "" || r.EntityID == "" {
			t.Errorf("record %d is missing an id: %v", i, r)
		}
		if r.Pass() == "" {
			t.Errorf("record %d carries no pass; a SYNC processor callout always has one", i)
		}
		if got := w.cnode.Received(); len(got) != 1 || got[0].Seq != r.Seq {
			t.Errorf("cnode %s Received() = %v; want exactly record %d", w.name, got, i)
		}
	}
	if s := fmt.Sprintf("%v %+v %#v", recs[0], recs[0], recs[0]); strings.Contains(s, recs[0].Pass()) {
		t.Error("formatting a receivedCallout prints the pass")
	}
}

// TestScriptedCnode_Replies: each scripted reply reaches the client as the
// status and code today's single-shot dispatch gives it.
func TestScriptedCnode_Replies(t *testing.T) {
	h := newCalloutHarness(t, nil)
	cases := []struct {
		name       string
		reply      cnodeReply
		timeoutMs  int
		wantStatus int
		wantCode   string
		wantInBody string
	}{
		{"ok", answerOK(), 0, http.StatusOK, "", ""},
		{"fail", answerFail("h3 boom"), 0, http.StatusBadRequest, "WORKFLOW_FAILED", "h3 boom"},
		{"fail-verdict", answerFailVerdict("h3 verdict boom", true), 0, http.StatusBadRequest, "WORKFLOW_FAILED", "h3 verdict boom"},
		{"never-answer", neverAnswer(), 300, http.StatusServiceUnavailable, "DISPATCH_TIMEOUT", ""},
		{"close-stream", closeStream(), 0, http.StatusServiceUnavailable, "COMPUTE_MEMBER_DISCONNECTED", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tag, model := "h3-r-"+tc.name, "h3-replies-"+tc.name
			c := h.AttachCnode(t, cnodeSpec{name: tc.name, tags: []string{tag}, script: scriptAlways(tc.reply)})
			defer c.Detach(t)

			cfg := map[string]any{"calculationNodesTags": tag}
			if tc.timeoutMs > 0 {
				cfg["responseTimeoutMs"] = tc.timeoutMs
			}
			h.SetupModelWithWorkflow(t, model, procWorkflowJSON("h3-replies-wf-"+tc.name, "h3-proc", "SYNC", cfg))

			_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
			if status != tc.wantStatus {
				t.Fatalf("status = %d; want %d (body: %s)", status, tc.wantStatus, body)
			}
			if tc.wantCode != "" {
				if code := problemErrorCode(body); code != tc.wantCode {
					t.Errorf("errorCode = %q; want %q (body: %s)", code, tc.wantCode, body)
				}
			}
			if tc.wantInBody != "" && !strings.Contains(body, tc.wantInBody) {
				t.Errorf("body does not carry the cnode's message %q: %s", tc.wantInBody, body)
			}
			if got := c.Received(); len(got) != 1 {
				t.Errorf("cnode received %d callouts; want 1", len(got))
			}
		})
	}
}

// TestScriptedCnode_AttachAndDetachMidTest: no cnode -> refused; attach ->
// served; detach -> refused again. Detach returns only once the server has
// dropped the member, so the third step needs no polling.
func TestScriptedCnode_AttachAndDetachMidTest(t *testing.T) {
	h := newCalloutHarness(t, nil)
	const model, tag = "h3-attach-detach", "h3-ad"
	h.SetupModelWithWorkflow(t, model, procWorkflowJSON("h3-ad-wf", "h3-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag}))

	expectNoCnode := func(step string) {
		t.Helper()
		_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
		if status != http.StatusServiceUnavailable || problemErrorCode(body) != "NO_COMPUTE_MEMBER_FOR_TAG" {
			t.Fatalf("%s: got %d %s; want 503 NO_COMPUTE_MEMBER_FOR_TAG", step, status, body)
		}
	}

	expectNoCnode("before any cnode attached")

	c := h.AttachCnode(t, cnodeSpec{name: "late", tags: []string{tag}})
	if _, status, body := h.CreateEntity(t, model, 1, workflowSampleModel); status != http.StatusOK {
		t.Fatalf("with the cnode attached: %d %s", status, body)
	}

	c.Detach(t)
	if m := h.app.MemberRegistry().Get(c.MemberID()); m != nil {
		t.Fatal("Detach returned while the server still holds the member")
	}
	expectNoCnode("after Detach")
	c.Detach(t) // idempotent
}

// TestScriptedCnode_RecordsCriterionAndFunction: the recorder names the kind
// of every callout, not only processors, and scriptSequence hands out replies
// in order.
func TestScriptedCnode_RecordsCriterionAndFunction(t *testing.T) {
	h := newCalloutHarness(t, nil)
	const tag = "h3-kinds"
	h.AttachCnode(t, cnodeSpec{name: "kinds", tags: []string{tag}, script: scriptSequence(
		answerMatches(true),
		answerResult("Schedule", map[string]any{"fireAfterMs": int64(3600000)}),
	)})

	critWF := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "h3-crit-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "DONE", "manual": false,
					"criterion": {"type": "function", "function": {"name": "h3-crit",
						"config": {"calculationNodesTags": %q}}}}]},
				"DONE": {}
			}
		}]
	}`, tag)
	h.SetupModelWithWorkflow(t, "h3-kinds-crit", critWF)
	h.SetupModelWithWorkflow(t, "h3-kinds-fn", scheduleFunctionWorkflowJSON("h3-fn-wf",
		fmt.Sprintf(`{"name":"h3-fn","resultKind":"Schedule","calculationNodesTags":%q}`, tag)))

	critID, status, body := h.CreateEntity(t, "h3-kinds-crit", 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("create behind a criterion: %d %s", status, body)
	}
	if st, _ := h.GetEntityState(t, critID); st != "DONE" {
		t.Errorf("state = %q; want DONE (the scripted criterion matched)", st)
	}
	if _, status, body := h.CreateEntity(t, "h3-kinds-fn", 1, workflowSampleModel); status != http.StatusOK {
		t.Fatalf("create arming a schedule function: %d %s", status, body)
	}

	recs := h.AwaitCallouts(t, 2, 5*time.Second)
	if recs[0].Kind != calloutCriterion || recs[0].Name != "h3-crit" {
		t.Errorf("first record = %v; want criterion h3-crit", recs[0])
	}
	if recs[1].Kind != calloutFunction || recs[1].Name != "h3-fn" {
		t.Errorf("second record = %v; want function h3-fn", recs[1])
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/e2e/ -run 'TestScriptedCnode'`
Expected: FAIL (build) with `undefined: cnodeSpec` (and `AttachCnode`, `procWorkflowJSON`, `scriptAlways`, …).

- [ ] **Step 3: Implement**

`internal/e2e/scripted_cnode_test.go`:

```go
package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// scripted_cnode_test.go lets a test attach SEVERAL cnodes to one callout
// harness, each with tags of its own and a script deciding what it does with
// each callout, and records what every cnode received. A pass is held in
// memory only and is never printed (Gate 3).

// receivedCallout is one callout as a scripted cnode received it.
type receivedCallout struct {
	Seq       int    // 1-based arrival order across every scripted cnode of the harness
	Cnode     string // cnodeSpec.name
	MemberID  string // the id the server gave this cnode in its greet
	Kind      string // calloutProcessor | calloutCriterion | calloutFunction
	Name      string // processor / criterion / function name
	RequestID string // the payload's requestId
	EventID   string // the CloudEvent id
	EntityID  string

	pass string
}

// Pass returns the transaction token the callout carried ("" when it carried
// none), for presenting it again later. Never log or print it.
func (r receivedCallout) Pass() string { return r.pass }

// String and GoString keep the pass out of every fmt verb.
func (r receivedCallout) String() string {
	return fmt.Sprintf("{#%d cnode=%s member=%s %s %q requestId=%s eventId=%s entity=%s pass=%t}",
		r.Seq, r.Cnode, r.MemberID, r.Kind, r.Name, r.RequestID, r.EventID, r.EntityID, r.pass != "")
}
func (r receivedCallout) GoString() string { return r.String() }

// calloutLog is the harness-wide record, in arrival order.
type calloutLog struct {
	mu   sync.Mutex
	recs []receivedCallout
}

func (l *calloutLog) add(r receivedCallout) receivedCallout {
	l.mu.Lock()
	defer l.mu.Unlock()
	r.Seq = len(l.recs) + 1
	l.recs = append(l.recs, r)
	return r
}

func (l *calloutLog) snapshot() []receivedCallout {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]receivedCallout(nil), l.recs...)
}

// cnodeScript decides what a cnode does with one callout. It runs on a
// goroutine of its own: it may block (typically on a channel the test closes)
// and may make callbacks through rc before it replies, but it must not call
// t.Fatal. ctx ends when the cnode is detached or closes its stream.
type cnodeScript func(ctx context.Context, call receivedCallout, rc *reqCtx) cnodeReply

// scriptAlways gives every callout the same reply.
func scriptAlways(r cnodeReply) cnodeScript {
	return func(context.Context, receivedCallout, *reqCtx) cnodeReply { return r }
}

// scriptSequence gives the n-th callout this cnode receives the n-th reply;
// the last reply repeats.
func scriptSequence(replies ...cnodeReply) cnodeScript {
	var n atomic.Int64
	return func(context.Context, receivedCallout, *reqCtx) cnodeReply {
		i := int(n.Add(1)) - 1
		if i >= len(replies) {
			i = len(replies) - 1
		}
		return replies[i]
	}
}

// cnodeSpec describes one scripted cnode.
type cnodeSpec struct {
	name   string      // label in records and failure messages; required
	tags   []string    // join tags
	bearer string      // M2M bearer to join with; "" = the harness's own tenant
	script cnodeScript // nil = scriptAlways(answerOK())
}

type scriptedCnode struct {
	h      *callbackHarness
	name   string
	script cnodeScript
	m      *computeMember
}

// AttachCnode connects a scripted cnode and returns once the server has
// registered it. It may be called at any point in a test.
func (h *callbackHarness) AttachCnode(t *testing.T, spec cnodeSpec) *scriptedCnode {
	t.Helper()
	if spec.name == "" {
		t.Fatal("cnodeSpec.name is required")
	}
	c := &scriptedCnode{h: h, name: spec.name, script: spec.script}
	if c.script == nil {
		c.script = scriptAlways(answerOK())
	}
	c.m = newComputeMember(t, h, memberSpec{bearer: spec.bearer, tags: spec.tags, handle: c.handle})
	t.Cleanup(c.m.stop)
	return c
}

// handle records the callout, runs the script and carries out its reply. It
// takes the member from its argument, not from c.m: work can arrive before
// AttachCnode has returned.
func (c *scriptedCnode) handle(m *computeMember, send func(*cepb.CloudEvent) error, req calcRequest) {
	call := c.h.callouts.add(receivedCallout{
		Cnode: c.name, MemberID: m.id, Kind: req.kind, Name: req.name,
		RequestID: req.requestID, EventID: req.eventID, EntityID: req.rc.entityID,
		pass: req.rc.token,
	})
	reply := c.script(m.ctx, call, req.rc)
	if reply.kind == replyCloseStream {
		m.closeStream()
		return
	}
	sendReply(send, req, reply)
}

func (c *scriptedCnode) MemberID() string { return c.m.id }

// Received returns what this cnode received, in arrival order.
func (c *scriptedCnode) Received() []receivedCallout {
	var out []receivedCallout
	for _, r := range c.h.callouts.snapshot() {
		if r.MemberID == c.m.id {
			out = append(out, r)
		}
	}
	return out
}

// AwaitGone returns once the server's registry no longer holds this cnode.
func (c *scriptedCnode) AwaitGone(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for c.h.app.MemberRegistry().Get(c.m.id) != nil {
		if time.Now().After(deadline) {
			t.Fatalf("cnode %s (member %s) is still registered 5s after its stream ended", c.name, c.m.id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Detach closes the cnode's connection and returns once the server has
// dropped it. Calling it again is harmless.
func (c *scriptedCnode) Detach(t *testing.T) {
	t.Helper()
	c.m.stop()
	c.AwaitGone(t)
}

// ReceivedCallouts returns every callout any scripted cnode of this harness
// received, in arrival order.
func (h *callbackHarness) ReceivedCallouts() []receivedCallout { return h.callouts.snapshot() }

// AwaitCallouts waits until at least n callouts are recorded and returns them.
func (h *callbackHarness) AwaitCallouts(t *testing.T, n int, within time.Duration) []receivedCallout {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		recs := h.callouts.snapshot()
		if len(recs) >= n {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("recorded %d callouts within %s; want at least %d: %v", len(recs), within, n, recs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// registeredScript is the default cnode's script: it runs the closure
// registered under the callout's name.
func (h *callbackHarness) registeredScript(_ context.Context, call receivedCallout, rc *reqCtx) cnodeReply {
	return h.registeredReply(call.Kind, call.Name, rc)
}

// procWorkflowJSON builds a NONE -> DONE workflow whose one automated
// transition carries one processor. config is merged over
// {"attachEntity": true, "calculationNodesTags": ""}.
func procWorkflowJSON(wfName, procName, mode string, config map[string]any) string {
	cfg := map[string]any{"attachEntity": true, "calculationNodesTags": ""}
	for k, v := range config {
		cfg[k] = v
	}
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.1", "name": wfName, "initialState": "NONE", "active": true,
			"states": map[string]any{
				"NONE": map[string]any{"transitions": []any{map[string]any{
					"name": "init", "next": "DONE", "manual": false,
					"processors": []any{map[string]any{
						"type": "calculator", "name": procName, "executionMode": mode, "config": cfg,
					}},
				}}},
				"DONE": map[string]any{},
			},
		}},
	})
	return string(b)
}
```

`stop()` on an already-stopped `computeMember` is already harmless (`cancel`
is idempotent, a second `conn.Close` returns an error that is ignored, `done`
stays closed), which is what makes `Detach` idempotent.

In `callback_harness_test.go`: add `callouts calloutLog` to `callbackHarness`
(the zero value is ready), delete `handleRegistered`, and attach the default
cnode through the same door:

```go
func newCallbackHarnessConfigured(t *testing.T, configure func(*app.Config)) *callbackHarness {
	t.Helper()
	h := newCalloutHarness(t, configure)
	// Tagged "sched-fn" so a schedule.function callout — whose
	// calculationNodesTags is validated non-empty at import — can route to it.
	// Processor/criteria tests configure calculationNodesTags:"" which matches
	// any cnode of the tenant.
	h.member = h.AttachCnode(t, cnodeSpec{name: "default", tags: []string{"sched-fn"}, script: h.registeredScript}).m
	return h
}
```

In `TestComputeMember_JoinsWithGivenTagsAndBearer` (H-2) replace
`handle: h.handleRegistered` with
`handle: func(*computeMember, func(*cepb.CloudEvent) error, calcRequest) {}` —
that test gives its cnodes no work.

Exit check: `grep -rn 'handleRegistered' internal/e2e/` → no hits.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/e2e/...` — the whole package.

- [ ] **Step 5: Commit**

```
git add internal/e2e/scripted_cnode_test.go internal/e2e/callback_harness_test.go internal/e2e/callout_harness_selftest_test.go
git commit -m "test(e2e): several scripted cnodes per stack, attach and detach mid-test, recorded callouts (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task H-4: e2e callback harness — a callback on a signal from the test, and a recorded pass replayed on both doors

**Spec:** §13 "What each harness can do" (first bullet); enables the rows "Late callback … HTTP and gRPC doors, write and read", "Late callback in `ASYNC_NEW_TX`", "Owner gives the work to a second cnode … refused at once", "A callback in progress when its cnode is replaced".

**Files:**
- Modify: `internal/e2e/scripted_cnode_test.go` (append)
- Modify: `internal/e2e/storage_ceilings_e2e_test.go` (`createEntityGRPC` :641-664 delegates to the joined form)
- Test: `internal/e2e/callout_harness_selftest_test.go`

**Interfaces:**
- Consumes: H-1..H-3; `h.callback` (`callback_harness_test.go:354`), `h.grpcCtx` (`callback_txjoin_grpc_search_test.go:54`), `txEnvelope`, `parseTxEnvelope` (`storage_ceilings_e2e_test.go:632, 717`), `secondaryWorkflow` — existing.
- Produces:
  - `func scriptLateCallback(release <-chan struct{}, callback func(rc *reqCtx), then cnodeReply) cnodeScript` — on a callout: wait for `release` (or the cnode's end), run `callback` with the pass the callout was given, then give `then`
  - `func (h *callbackHarness) ReplayCreateHTTP(pass, entityName string, version int, payload string) (callbackResult, error)`
  - `func (h *callbackHarness) ReplayGetHTTP(pass, entityID string) (callbackResult, error)`
  - `func (h *callbackHarness) ReplayCreateGRPC(pass, model string, version int, payload string) (txEnvelope, error)`
  - `func (h *callbackHarness) ReplayGetGRPC(pass, entityID string) (txEnvelope, error)`
  - all four are goroutine-safe (no `*testing.T`) and refuse an empty pass with an error, so a test cannot pass vacuously on an unjoined request
  - `func (h *callbackHarness) createEntityGRPCJoined(model string, version int, payload, joinTok string) (txEnvelope, error)`

Existing tests: `createEntityGRPC`'s two callers (`storage_ceilings_e2e_test.go:189, 288`) stay; the function keeps its signature and delegates.

What the self-test asserts about the replay is only "the door evaluated the pass and refused it" (HTTP status ≠ 200; gRPC `success=false`), with an unjoined control that succeeds. Today that refusal is 404 `TRANSACTION_NOT_FOUND`; which code it is after the feature belongs to the fencing stream, and this test holds either way.

- [ ] **Step 1: Write the failing test** (append)

```go
// TestScriptedCnode_LateCallbackAndReplay: a cnode makes a callback with the
// pass it was given only after the test says so, and the recorded pass can be
// presented again on the HTTP door and on the gRPC door.
func TestScriptedCnode_LateCallbackAndReplay(t *testing.T) {
	h := newCalloutHarness(t, nil)
	const primary, secondary, tag = "h4-primary", "h4-secondary", "h4-tag"
	const child = `{"name":"child","amount":1,"status":"h4"}`
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, primary, procWorkflowJSON("h4-wf", "h4-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag}))

	release := make(chan struct{})
	inCallout := make(chan callbackResult, 1)
	h.AttachCnode(t, cnodeSpec{name: "late", tags: []string{tag}, script: scriptLateCallback(release,
		func(rc *reqCtx) {
			res, err := rc.CreateEntity(secondary, 1, child)
			if err != nil {
				res = callbackResult{StatusCode: -1, Body: err.Error()}
			}
			inCallout <- res
		}, answerOK())})

	created := make(chan createEntityResult, 1)
	go func() { created <- h.CreateEntityRaw(primary, 1, workflowSampleModel) }()

	recs := h.AwaitCallouts(t, 1, 10*time.Second)
	select {
	case res := <-inCallout:
		t.Fatalf("the cnode called back before the test released it: %d", res.StatusCode)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)

	if res := <-inCallout; res.StatusCode != http.StatusOK {
		t.Fatalf("callback made during the callout: %d %s; want 200 (it joins the open transaction)", res.StatusCode, res.Body)
	}
	if res := <-created; res.err != nil || res.status != http.StatusOK {
		t.Fatalf("primary create: status=%d err=%v body=%s", res.status, res.err, res.body)
	}

	// The callout has ended and its transaction is committed. The same pass,
	// presented again, is evaluated and refused on both doors.
	pass := recs[0].Pass()
	httpRes, err := h.ReplayCreateHTTP(pass, secondary, 1, child)
	if err != nil {
		t.Fatalf("ReplayCreateHTTP: %v", err)
	}
	if httpRes.StatusCode == http.StatusOK {
		t.Error("HTTP door accepted a pass whose callout has ended")
	}
	if got, err := h.ReplayGetHTTP(pass, recs[0].EntityID); err != nil || got.StatusCode == http.StatusOK {
		t.Errorf("HTTP read with the ended pass: status=%d err=%v; want a refusal", got.StatusCode, err)
	}
	grpcRes, err := h.ReplayCreateGRPC(pass, secondary, 1, child)
	if err != nil {
		t.Fatalf("ReplayCreateGRPC: %v", err)
	}
	if grpcRes.Success {
		t.Error("gRPC door accepted a pass whose callout has ended")
	}
	if got, err := h.ReplayGetGRPC(pass, recs[0].EntityID); err != nil || got.Success {
		t.Errorf("gRPC read with the ended pass: success=%t err=%v; want a refusal", got.Success, err)
	}

	// Control: the same requests without a pass succeed, so the refusals above
	// are about the pass and nothing else.
	if res, err := h.callback(http.MethodPost, "/api/entity/JSON/"+secondary+"/1", child, ""); err != nil || res.StatusCode != http.StatusOK {
		t.Errorf("unjoined HTTP create: status=%d err=%v", res.StatusCode, err)
	}
	if env, err := h.createEntityGRPC(secondary, 1, child); err != nil || !env.Success {
		t.Errorf("unjoined gRPC create: success=%t err=%v", env.Success, err)
	}

	if _, err := h.ReplayCreateHTTP("", secondary, 1, child); err == nil {
		t.Error("ReplayCreateHTTP accepted an empty pass; it must refuse, or a test could pass unjoined")
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/e2e/ -run 'TestScriptedCnode_LateCallbackAndReplay'`
Expected: FAIL (build) with `undefined: scriptLateCallback` (and the four `Replay*` methods).

- [ ] **Step 3: Implement**

Append to `scripted_cnode_test.go` (add imports `errors`, `net/http`,
`cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"`,
`internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"`):

```go
// scriptLateCallback holds a callout until release is closed (or the cnode
// ends), then runs callback with the pass the callout was given, then gives
// then. callback runs off the test goroutine: hand results back on a channel.
func scriptLateCallback(release <-chan struct{}, callback func(rc *reqCtx), then cnodeReply) cnodeScript {
	return func(ctx context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		select {
		case <-release:
		case <-ctx.Done():
			return neverAnswer()
		}
		callback(rc)
		return then
	}
}

var errReplayNeedsPass = errors.New("replay needs a recorded pass; an empty one would send an unjoined request")

// ReplayCreateHTTP presents a recorded pass on the HTTP door with an entity create.
func (h *callbackHarness) ReplayCreateHTTP(pass, entityName string, version int, payload string) (callbackResult, error) {
	if pass == "" {
		return callbackResult{}, errReplayNeedsPass
	}
	return h.callback(http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/%d", entityName, version), payload, pass)
}

// ReplayGetHTTP presents a recorded pass on the HTTP door with an entity read.
func (h *callbackHarness) ReplayGetHTTP(pass, entityID string) (callbackResult, error) {
	if pass == "" {
		return callbackResult{}, errReplayNeedsPass
	}
	return h.callback(http.MethodGet, "/api/entity/"+entityID, "", pass)
}

// ReplayCreateGRPC presents a recorded pass on the gRPC door with EntityManage
// (EntityCreateRequest) and returns the response envelope.
func (h *callbackHarness) ReplayCreateGRPC(pass, model string, version int, payload string) (txEnvelope, error) {
	if pass == "" {
		return txEnvelope{}, errReplayNeedsPass
	}
	return h.createEntityGRPCJoined(model, version, payload, pass)
}

// ReplayGetGRPC presents a recorded pass on the gRPC door with EntitySearch
// (EntityGetRequest). EntityResponse carries the same success/error envelope
// as EntityTransactionResponse, so txEnvelope reads it.
func (h *callbackHarness) ReplayGetGRPC(pass, entityID string) (txEnvelope, error) {
	if pass == "" {
		return txEnvelope{}, errReplayNeedsPass
	}
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityGetRequest, map[string]any{
		"id":       "replay-grpc-get",
		"entityId": entityID,
	})
	if err != nil {
		return txEnvelope{}, fmt.Errorf("failed to build get request: %w", err)
	}
	respCE, err := cyodapb.NewCloudEventsServiceClient(h.apiConn).EntitySearch(h.grpcCtx(pass), reqCE)
	if err != nil {
		return txEnvelope{}, fmt.Errorf("failed to call EntitySearch: %w", err)
	}
	return parseTxEnvelope(respCE)
}
```

In `storage_ceilings_e2e_test.go`, `createEntityGRPC` keeps its doc comment and becomes:

```go
func (h *callbackHarness) createEntityGRPC(model string, version int, payload string) (txEnvelope, error) {
	return h.createEntityGRPCJoined(model, version, payload, "")
}

// createEntityGRPCJoined is createEntityGRPC presenting joinTok as the
// tx-token metadata, so the create joins that transaction ("" = unjoined).
func (h *callbackHarness) createEntityGRPCJoined(model string, version int, payload, joinTok string) (txEnvelope, error) {
	var data map[string]any
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		return txEnvelope{}, err
	}
	reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityCreateRequest, map[string]any{
		"id":         "harness-grpc-create",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": model, "version": version},
			"data":  data,
		},
	})
	if err != nil {
		return txEnvelope{}, err
	}
	client := cyodapb.NewCloudEventsServiceClient(h.apiConn)
	respCE, err := client.EntityManage(h.grpcCtx(joinTok), reqCE)
	if err != nil {
		return txEnvelope{}, err
	}
	return parseTxEnvelope(respCE)
}
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/e2e/...` — the whole package.

- [ ] **Step 5: Commit**

```
git add internal/e2e/scripted_cnode_test.go internal/e2e/storage_ceilings_e2e_test.go internal/e2e/callout_harness_selftest_test.go
git commit -m "test(e2e): callback on the test's signal, and a recorded pass replayed on both doors (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task H-5: compute-test-client — a catalog entry can fail with a verdict

**Spec:** §13 "What each harness can do" (second bullet); R§7 "Parity harness" ("a process-lifetime catalog whose `processorFunc` signature cannot express `retryable`, `catalog.go:40`").

**Decision on the signature.** The verdict travels in the **error**, not in a new
return value: a catalog entry returns `*verdictError` and the dispatcher reads
it with `errors.As`. `processorFunc`, `criterionFunc`, `functionFunc`,
`callbackProcessorFunc` and `callbackCriterionFunc` keep their signatures, so
none of the ~25 existing entries changes, and all five kinds gain the
capability at once. What changes is the three response builders, which gain a
`retryable *bool`.

**Files:**
- Create: `cmd/compute-test-client/verdict.go`
- Modify: `cmd/compute-test-client/dispatch.go` (`handleProcessorRequest` :240-296, `buildProcessorResponse` :299-319, `handleCriteriaRequest` :323-376, `handleFunctionRequest` :381-421, `buildFunctionResponse` :424-443, `buildCriteriaResponse` :446-461)
- Modify: `cmd/compute-test-client/catalog.go` (new entries in `processors`, `criteria`, `functions`)
- Test: `cmd/compute-test-client/verdict_test.go` (new)

**Interfaces:**
- Consumes: nothing.
- Produces (catalog names the parity scenarios of other streams may use with the **default** shared client, tenant `system-tenant`, no extra process):
  - processors `inject-error-retryable`, `inject-error-not-retryable` (beside the existing `inject-error`, which gives no verdict)
  - criterion `inject-criterion-error-retryable`
  - function `inject-fn-error-retryable`
  - each answers `success=false` with `error.retryable` set accordingly and `error.message` = `<name>: deliberate failure`

Existing tests: `main_test.go` (all `TestCatalog_*`) and `dispatch_send_test.go` stay untouched and green — no catalog signature changes.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// replyBody is the part of a calculation response these tests read.
type replyBody struct {
	RequestID string `json:"requestId"`
	Success   bool   `json:"success"`
	Error     *struct {
		Message   string `json:"message"`
		Retryable *bool  `json:"retryable"`
	} `json:"error"`
}

func decodeReply(t *testing.T, ce *cepb.CloudEvent) replyBody {
	t.Helper()
	if ce == nil {
		t.Fatal("no reply was produced")
	}
	var body replyBody
	if err := json.Unmarshal([]byte(ce.GetTextData()), &body); err != nil {
		t.Fatalf("reply is not JSON: %v", err)
	}
	return body
}

func assertVerdict(t *testing.T, body replyBody, want *bool) {
	t.Helper()
	if body.Success {
		t.Fatal("reply says success=true; want a failure")
	}
	if body.Error == nil {
		t.Fatal("failure reply carries no error node")
	}
	switch {
	case want == nil && body.Error.Retryable != nil:
		t.Errorf("error.retryable = %t; want it absent", *body.Error.Retryable)
	case want != nil && body.Error.Retryable == nil:
		t.Errorf("error.retryable absent; want %t", *want)
	case want != nil && *body.Error.Retryable != *want:
		t.Errorf("error.retryable = %t; want %t", *body.Error.Retryable, *want)
	}
}

func TestCatalogFailure_CarriesVerdict(t *testing.T) {
	yes, no := true, false
	d := &dispatcher{cat: newCatalog(nil, nil)}
	ctx := context.Background()

	processors := []struct {
		name string
		want *bool
	}{
		{"inject-error", nil},
		{"inject-error-retryable", &yes},
		{"inject-error-not-retryable", &no},
	}
	for _, tc := range processors {
		t.Run("processor/"+tc.name, func(t *testing.T) {
			payload := json.RawMessage(fmt.Sprintf(
				`{"requestId":"r-1","entityId":"e-1","processorName":%q,"payload":{"data":{}}}`, tc.name))
			ce, err := d.handleProcessorRequest(ctx, payload, "", "")
			if err != nil {
				t.Fatalf("handleProcessorRequest: %v", err)
			}
			assertVerdict(t, decodeReply(t, ce), tc.want)
		})
	}

	t.Run("criterion", func(t *testing.T) {
		ce, err := d.handleCriteriaRequest(ctx, json.RawMessage(
			`{"requestId":"r-2","entityId":"e-1","criteriaName":"inject-criterion-error-retryable"}`), "")
		if err != nil {
			t.Fatalf("handleCriteriaRequest: %v", err)
		}
		assertVerdict(t, decodeReply(t, ce), &yes)
	})

	t.Run("function", func(t *testing.T) {
		ce, err := d.handleFunctionRequest(ctx, json.RawMessage(
			`{"requestId":"r-3","entityId":"e-1","functionName":"inject-fn-error-retryable"}`), "")
		if err != nil {
			t.Fatalf("handleFunctionRequest: %v", err)
		}
		assertVerdict(t, decodeReply(t, ce), &yes)
	})
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./cmd/compute-test-client/ -run 'TestCatalogFailure_CarriesVerdict'`
Expected: FAIL — `processor/inject-error-retryable`: `error.retryable absent; want true` (the reply today is "unknown processor"), and likewise for the other new names. `processor/inject-error` passes.

- [ ] **Step 3: Implement**

`cmd/compute-test-client/verdict.go`:

```go
package main

import "errors"

// verdictError fails a callout and carries this compute node's own verdict on
// whether the failure is worth retrying. A catalog entry that returns any
// other error fails the callout with no verdict.
type verdictError struct {
	msg       string
	retryable bool
}

func (e *verdictError) Error() string { return e.msg }

// verdictOf returns the verdict err carries, or nil when it carries none.
func verdictOf(err error) *bool {
	var ve *verdictError
	if errors.As(err, &ve) {
		v := ve.retryable
		return &v
	}
	return nil
}

// errorNode builds the "error" object of a failed calculation response.
func errorNode(code, msg string, retryable *bool) map[string]any {
	node := map[string]any{"code": code, "message": msg}
	if retryable != nil {
		node["retryable"] = *retryable
	}
	return node
}
```

`dispatch.go` — the three builders gain the verdict as their last parameter and use `errorNode`:

```go
func (d *dispatcher) buildProcessorResponse(requestID, entityID string, data json.RawMessage, success bool, errMsg string, retryable *bool) (*cepb.CloudEvent, error) {
	resp := map[string]any{
		"id":        uuid.NewString(),
		"requestId": requestID,
		"entityId":  entityID,
		"success":   success,
	}
	if data != nil {
		resp["payload"] = map[string]any{
			"type": "JSON",
			"data": json.RawMessage(data),
		}
	}
	if errMsg != "" {
		resp["error"] = errorNode("PROCESSOR_ERROR", errMsg, retryable)
	}
	return newCloudEvent(ceTypeProcessorResponse, resp)
}
```

`buildFunctionResponse(requestID, resultKind string, result map[string]any, success bool, errMsg string, retryable *bool)`
and `buildCriteriaResponse(requestID, entityID string, matches, success bool, errMsg string, retryable *bool)`
change the same way: the new last parameter, and their `resp["error"] = map[string]any{…}`
block becomes `resp["error"] = errorNode("FUNCTION_ERROR", errMsg, retryable)` /
`errorNode("CRITERIA_ERROR", errMsg, retryable)`.

Every call site, as found:

| Site (`dispatch.go`) | Before (tail of the call) | After |
|---|---|---|
| :276 callback config parse failed | `nil, false, err.Error())` | `nil, false, err.Error(), nil)` |
| :280 callback processor failed | `nil, false, err.Error())` | `nil, false, err.Error(), verdictOf(err))` |
| :282 callback processor ok | `result.Data, true, "")` | `result.Data, true, "", nil)` |
| :287 unknown processor | `nil, false, fmt.Sprintf(…))` | `nil, false, fmt.Sprintf(…), nil)` |
| :292 processor failed | `nil, false, err.Error())` | `nil, false, err.Error(), verdictOf(err))` |
| :295 processor ok | `result.Data, true, "")` | `result.Data, true, "", nil)` |
| :356 callback config parse failed | `false, false, err.Error())` | `false, false, err.Error(), nil)` |
| :360 callback criterion failed | `false, false, err.Error())` | `false, false, err.Error(), verdictOf(err))` |
| :362 callback criterion ok | `matches, true, "")` | `matches, true, "", nil)` |
| :367 unknown criterion | `false, false, fmt.Sprintf(…))` | `false, false, fmt.Sprintf(…), nil)` |
| :372 criterion failed | `false, false, err.Error())` | `false, false, err.Error(), verdictOf(err))` |
| :375 criterion ok | `matches, true, "")` | `matches, true, "", nil)` |
| :412 unknown function | `"", nil, false, fmt.Sprintf(…))` | `"", nil, false, fmt.Sprintf(…), nil)` |
| :417 function failed | `"", nil, false, err.Error())` | `"", nil, false, err.Error(), verdictOf(err))` |
| :420 function ok | `resultKind, result, true, "")` | `resultKind, result, true, "", nil)` |

`catalog.go` — add, after `"inject-error"` in `processors`:

```go
			// inject-error-retryable / -not-retryable fail like inject-error and
			// add this compute node's verdict (error.retryable) to the response.
			"inject-error-retryable": func(ctx context.Context, entity *Entity, config json.RawMessage) (*Entity, error) {
				return nil, &verdictError{msg: "inject-error-retryable: deliberate failure", retryable: true}
			},
			"inject-error-not-retryable": func(ctx context.Context, entity *Entity, config json.RawMessage) (*Entity, error) {
				return nil, &verdictError{msg: "inject-error-not-retryable: deliberate failure", retryable: false}
			},
```

after `"always-false"` in `criteria`:

```go
			"inject-criterion-error-retryable": func(ctx context.Context, entity *Entity, config json.RawMessage) (bool, error) {
				return false, &verdictError{msg: "inject-criterion-error-retryable: deliberate failure", retryable: true}
			},
```

and after `"sched-fn-resolve"` in `functions`:

```go
			"inject-fn-error-retryable": func(ctx context.Context, entity *Entity, config json.RawMessage) (string, map[string]any, error) {
				return "", nil, &verdictError{msg: "inject-fn-error-retryable: deliberate failure", retryable: true}
			},
```

Also correct the stale half of the three skip messages that cite this gap
(`e2e/parity/externalapi/workflow_externalization.go:228, 234, 240`): they say
the processor signature "does not expose the gRPC ProcessorResponse
warnings/errors path". The verdict is now expressible; the warnings/errors
flags those three scenarios need are still not. Reword each to
`"pending: cmd/compute-test-client has no processor that sets the response's warnings/errors flags."`

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./cmd/compute-test-client/...` and `go vet ./cmd/compute-test-client/ ./e2e/parity/externalapi/`

- [ ] **Step 5: Commit**

```
git add cmd/compute-test-client/verdict.go cmd/compute-test-client/verdict_test.go cmd/compute-test-client/dispatch.go \
  cmd/compute-test-client/catalog.go e2e/parity/externalapi/workflow_externalization.go
git commit -m "test(compute-test-client): a catalog entry can fail with a retryable verdict (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task H-6: compute-test-client — tags and a behaviour from the environment; every callout recorded

**Spec:** §13 "What each harness can do" (second bullet: tags and behaviour `stall`, `fail`, `fail-retryable`, `late-callback`, `drop`).

**Files:**
- Create: `cmd/compute-test-client/behaviour.go`, `cmd/compute-test-client/recorder.go`
- Modify: `cmd/compute-test-client/dispatch.go` (`dispatcher` :35-46, `newDispatcher` :56-62, `connect` :87-100, `run` :130-200)
- Modify: `cmd/compute-test-client/main.go` (package comment; env reading; construction order)
- Test: `cmd/compute-test-client/behaviour_test.go` (new)

**Interfaces:**
- Consumes: `verdictOf`, the six-argument response builders (H-5).
- Produces:
  - env `CYODA_TEST_COMPUTE_TAGS` — comma-separated join tags; unset or empty → `compute-test-client` (today's value)
  - env `CYODA_TEST_COMPUTE_BEHAVIOUR` — `stall` | `fail` | `fail-retryable` | `late-callback` | `drop`; unset or empty → serve the catalog (today's behaviour); anything else → the client exits 1 at start
  - The `CYODA_TEST_` prefix is deliberate: `TestConfig_EnvVarCoverage` (`cmd/cyoda/help/help_test.go:489`) scans `cmd/` and demands a help entry for every `CYODA_*` name except the test-only prefixes (`:444`). These two must not appear in user-facing help, and need no `config_registry.go` row.
  - `func newDispatcher(endpoint, token string, cat *catalog, gcb *grpcCallbackClient, tags []string, beh behaviour, rec *recorder) *dispatcher`
  - `func (d *dispatcher) handleCallout(ctx context.Context, msg *cepb.CloudEvent, payload json.RawMessage) (reply *cepb.CloudEvent, drop bool, err error)`
  - `type recorder`, `type calloutRecord`, `type callbackOutcome` (wire shape in H-7)

Behaviour semantics, applied to **every** calculation request of any kind:

| Behaviour | On receiving work |
|---|---|
| *(unset)* | serve it from the catalog |
| `stall` | record it; never answer; keep the stream and the keep-alives going |
| `fail` | answer `success=false`, no verdict |
| `fail-retryable` | answer `success=false`, `error.retryable=true` |
| `late-callback` | record it and hold its pass in memory; never answer; call back when told to (H-7) |
| `drop` | record it, close the gRPC connection without answering, do not reconnect; the process and its control endpoint stay up so the record can still be read |

Existing tests: `TestDispatcher_SendIsSerialised` builds `&dispatcher{}` directly and stays. No test calls `newDispatcher`.

- [ ] **Step 1: Write the failing tests**

```go
package main

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

func TestParseTags(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{"compute-test-client"}},
		{" , ", []string{"compute-test-client"}},
		{"failover-a", []string{"failover-a"}},
		{" a , b,", []string{"a", "b"}},
	}
	for _, tc := range cases {
		if got := parseTags(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("parseTags(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseBehaviour(t *testing.T) {
	for _, ok := range []string{"", "stall", "fail", "fail-retryable", "late-callback", "drop", " drop "} {
		if _, err := parseBehaviour(ok); err != nil {
			t.Errorf("parseBehaviour(%q): %v", ok, err)
		}
	}
	if _, err := parseBehaviour("explode"); err == nil {
		t.Error("parseBehaviour accepted an unknown behaviour")
	}
}

func TestJoinPayload_CarriesConfiguredTags(t *testing.T) {
	d := newDispatcher("", "", newCatalog(nil, nil), nil, []string{"x", "y"}, behaviourCatalog, newRecorder())
	tags, _ := d.joinPayload()["tags"].([]string)
	if !slices.Equal(tags, []string{"x", "y"}) {
		t.Errorf("join tags = %v; want [x y]", tags)
	}
}

// processorRequest builds a processor calculation request carrying a pass.
func processorRequest(t *testing.T, requestID, processor, pass, parameters string) (*cepb.CloudEvent, json.RawMessage) {
	t.Helper()
	body := map[string]any{"requestId": requestID, "entityId": "e-1", "processorName": processor,
		"payload": map[string]any{"data": map[string]any{"k": 1}}}
	if parameters != "" {
		body["parameters"] = parameters
	}
	ce, err := newCloudEvent(ceTypeProcessorRequest, body)
	if err != nil {
		t.Fatalf("newCloudEvent: %v", err)
	}
	if pass != "" {
		ce.Attributes = map[string]*cepb.CloudEvent_CloudEventAttributeValue{
			txTokenAttr: {Attr: &cepb.CloudEvent_CloudEventAttributeValue_CeString{CeString: pass}},
		}
	}
	payload, err := extractTextData(ce)
	if err != nil {
		t.Fatalf("extractTextData: %v", err)
	}
	return ce, payload
}

func TestHandleCallout_Behaviours(t *testing.T) {
	yes := true
	cases := []struct {
		name        string
		beh         behaviour
		wantReply   bool
		wantDrop    bool
		wantSuccess bool
		wantVerdict *bool
		wantHeld    int
	}{
		{"catalog", behaviourCatalog, true, false, true, nil, 0},
		{"stall", behaviourStall, false, false, false, nil, 0},
		{"fail", behaviourFail, true, false, false, nil, 0},
		{"fail-retryable", behaviourFailRetryable, true, false, false, &yes, 0},
		{"late-callback", behaviourLateCallback, false, false, false, nil, 1},
		{"drop", behaviourDrop, false, true, false, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder()
			d := newDispatcher("", "", newCatalog(nil, nil), nil, []string{"x"}, tc.beh, rec)
			ce, payload := processorRequest(t, "r-1", "noop", "pass-value", "")

			reply, drop, err := d.handleCallout(context.Background(), ce, payload)
			if err != nil {
				t.Fatalf("handleCallout: %v", err)
			}
			if (reply != nil) != tc.wantReply {
				t.Fatalf("reply produced = %t; want %t", reply != nil, tc.wantReply)
			}
			if drop != tc.wantDrop {
				t.Errorf("drop = %t; want %t", drop, tc.wantDrop)
			}
			if tc.wantReply {
				body := decodeReply(t, reply)
				if body.RequestID != "r-1" {
					t.Errorf("reply requestId = %q; want r-1", body.RequestID)
				}
				if body.Success != tc.wantSuccess {
					t.Errorf("success = %t; want %t", body.Success, tc.wantSuccess)
				}
				if !tc.wantSuccess {
					assertVerdict(t, body, tc.wantVerdict)
				}
			}

			got := rec.snapshot().Received
			if len(got) != 1 {
				t.Fatalf("recorded %d callouts; want 1", len(got))
			}
			r := got[0]
			if r.Seq != 1 || r.Kind != "processor" || r.Name != "noop" || r.RequestID != "r-1" ||
				r.EventID != ce.Id || r.EntityID != "e-1" || !r.PassPresent {
				t.Errorf("record = %+v", r)
			}
			if held := len(d.takeHeld()); held != tc.wantHeld {
				t.Errorf("held callouts = %d; want %d", held, tc.wantHeld)
			}
		})
	}
}

func TestHandleCallout_RecordsCriterionAndFunction(t *testing.T) {
	rec := newRecorder()
	d := newDispatcher("", "", newCatalog(nil, nil), nil, []string{"x"}, behaviourStall, rec)
	for _, c := range []struct{ ceType, nameField, name string }{
		{ceTypeCriteriaRequest, "criteriaName", "always-true"},
		{ceTypeFunctionRequest, "functionName", "sched-fn-resolve"},
	} {
		ce, err := newCloudEvent(c.ceType, map[string]any{"requestId": "r", "entityId": "e", c.nameField: c.name})
		if err != nil {
			t.Fatalf("newCloudEvent: %v", err)
		}
		payload, _ := extractTextData(ce)
		if _, _, err := d.handleCallout(context.Background(), ce, payload); err != nil {
			t.Fatalf("handleCallout: %v", err)
		}
	}
	got := rec.snapshot().Received
	if len(got) != 2 || got[0].Kind != "criterion" || got[0].Name != "always-true" ||
		got[1].Kind != "function" || got[1].Name != "sched-fn-resolve" || got[1].Seq != 2 {
		t.Errorf("records = %+v", got)
	}
	if got[0].PassPresent {
		t.Error("a request without the pass attribute was recorded as carrying one")
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./cmd/compute-test-client/ -run 'TestParse|TestJoinPayload|TestHandleCallout'`
Expected: FAIL (build) with `undefined: parseTags` (and `parseBehaviour`, `newRecorder`, `handleCallout`, …).

- [ ] **Step 3: Implement**

`cmd/compute-test-client/behaviour.go`:

```go
package main

import (
	"fmt"
	"strings"
)

// behaviour is what this client does with every calculation request it
// receives, in place of serving it from the catalog. It exists so a parity
// scenario can attach a compute node that misbehaves in one known way.
type behaviour string

const (
	behaviourCatalog       behaviour = ""               // serve the catalog
	behaviourStall         behaviour = "stall"          // take the work, never answer
	behaviourFail          behaviour = "fail"           // answer success=false, no verdict
	behaviourFailRetryable behaviour = "fail-retryable" // answer success=false, retryable=true
	behaviourLateCallback  behaviour = "late-callback"  // never answer; call back with the pass when told to
	behaviourDrop          behaviour = "drop"           // close the stream on receiving work
)

func parseBehaviour(s string) (behaviour, error) {
	switch b := behaviour(strings.TrimSpace(s)); b {
	case behaviourCatalog, behaviourStall, behaviourFail, behaviourFailRetryable, behaviourLateCallback, behaviourDrop:
		return b, nil
	default:
		return "", fmt.Errorf("unknown behaviour %q (want stall, fail, fail-retryable, late-callback, drop, or unset)", s)
	}
}

// defaultTag is the tag the client joins with when none is configured.
const defaultTag = "compute-test-client"

// parseTags splits a comma-separated tag list; empty means the default tag.
func parseTags(csv string) []string {
	var tags []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			tags = append(tags, p)
		}
	}
	if len(tags) == 0 {
		return []string{defaultTag}
	}
	return tags
}
```

`cmd/compute-test-client/recorder.go`:

```go
package main

import "sync"

// calloutRecord is one calculation request as this client received it. The
// pass itself is never recorded — only whether one came with the work.
type calloutRecord struct {
	Seq         int              `json:"seq"` // 1-based arrival order at this client
	Kind        string           `json:"kind"` // processor | criterion | function
	Name        string           `json:"name"`
	RequestID   string           `json:"requestId"`
	EventID     string           `json:"eventId"` // the CloudEvent id
	EntityID    string           `json:"entityId"`
	PassPresent bool             `json:"passPresent"`
	Callback    *callbackOutcome `json:"callback,omitempty"`
}

// callbackOutcome is what the two doors answered to a late callback.
type callbackOutcome struct {
	HTTPStatus       int    `json:"httpStatus"`
	HTTPErrorCode    string `json:"httpErrorCode,omitempty"`
	GRPCAttempted    bool   `json:"grpcAttempted"`
	GRPCSuccess      bool   `json:"grpcSuccess"`
	GRPCErrorCode    string `json:"grpcErrorCode,omitempty"`
	GRPCErrorMessage string `json:"grpcErrorMessage,omitempty"`
	Error            string `json:"error,omitempty"` // the callback could not be made at all
}

// recordDoc is what the control endpoint serves.
type recordDoc struct {
	MemberID string          `json:"memberId"`
	Received []calloutRecord `json:"received"`
}

type recorder struct {
	mu       sync.Mutex
	memberID string
	records  []calloutRecord
}

func newRecorder() *recorder { return &recorder{} }

func (r *recorder) setMemberID(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.memberID = id
}

// add appends rec and returns the sequence number it was given.
func (r *recorder) add(rec calloutRecord) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec.Seq = len(r.records) + 1
	r.records = append(r.records, rec)
	return rec.Seq
}

func (r *recorder) setCallback(seq int, out callbackOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if seq >= 1 && seq <= len(r.records) {
		r.records[seq-1].Callback = &out
	}
}

func (r *recorder) snapshot() recordDoc {
	r.mu.Lock()
	defer r.mu.Unlock()
	return recordDoc{MemberID: r.memberID, Received: append([]calloutRecord{}, r.records...)}
}
```

`dispatch.go` — the dispatcher:

```go
type dispatcher struct {
	endpoint  string
	token     string
	cat       *catalog
	gcb       *grpcCallbackClient
	tags      []string
	behaviour behaviour
	rec       *recorder
	conn      *grpc.ClientConn
	memberID  string

	// sendMu serialises every write to the stream: the request loop and the
	// keep-alive ticker both send, and grpc-go forbids concurrent SendMsg on
	// one stream. Compute-node implementations must do the same.
	sendMu sync.Mutex

	// held are the callouts a late-callback client took and has not yet
	// called back for.
	heldMu sync.Mutex
	held   []heldCallout
}

// heldCallout is a callout whose pass is kept, in memory only, for a late callback.
type heldCallout struct {
	seq    int
	cfg    cbConfig
	cfgErr string
	pass   string
}

func newDispatcher(endpoint, token string, cat *catalog, gcb *grpcCallbackClient, tags []string, beh behaviour, rec *recorder) *dispatcher {
	return &dispatcher{endpoint: endpoint, token: token, cat: cat, gcb: gcb, tags: tags, behaviour: beh, rec: rec}
}

func (d *dispatcher) hold(hc heldCallout) {
	d.heldMu.Lock()
	defer d.heldMu.Unlock()
	d.held = append(d.held, hc)
}

// takeHeld returns the held callouts and forgets them.
func (d *dispatcher) takeHeld() []heldCallout {
	d.heldMu.Lock()
	defer d.heldMu.Unlock()
	held := d.held
	d.held = nil
	return held
}

// joinPayload is the join event's payload.
func (d *dispatcher) joinPayload() map[string]any {
	return map[string]any{
		"id":                  uuid.NewString(),
		"tags":                d.tags,
		"joinedLegalEntityId": "",
		"success":             true,
	}
}
```

In `connect`, the inline `joinPayload := map[string]any{…}` literal (:88-93) is
deleted and `newCloudEvent(ceTypeJoin, joinPayload)` becomes
`newCloudEvent(ceTypeJoin, d.joinPayload())`; after `d.memberID = greet.MemberID`
add `d.rec.setMemberID(d.memberID)`.

`handleCallout`, and the `run` loop that calls it:

```go
// handleCallout records one calculation request and decides what this client
// does with it: a reply to send (nil = stay silent) and whether to close the
// stream. The pass is read here and goes no further than the callback that
// presents it; it is never logged or recorded.
func (d *dispatcher) handleCallout(ctx context.Context, msg *cepb.CloudEvent, payload json.RawMessage) (*cepb.CloudEvent, bool, error) {
	var head struct {
		RequestID     string          `json:"requestId"`
		EntityID      string          `json:"entityId"`
		ProcessorName string          `json:"processorName"`
		ProcessorID   string          `json:"processorId"`
		CriteriaName  string          `json:"criteriaName"`
		CriteriaID    string          `json:"criteriaId"`
		FunctionName  string          `json:"functionName"`
		FunctionID    string          `json:"functionId"`
		Parameters    json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(payload, &head); err != nil {
		return nil, false, fmt.Errorf("failed to unmarshal calculation request: %w", err)
	}
	kind, name := "processor", head.ProcessorName
	if name == "" {
		name = head.ProcessorID
	}
	switch msg.Type {
	case ceTypeCriteriaRequest:
		kind, name = "criterion", head.CriteriaName
		if name == "" {
			name = head.CriteriaID
		}
	case ceTypeFunctionRequest:
		kind, name = "function", head.FunctionName
		if name == "" {
			name = head.FunctionID
		}
	}

	pass := txTokenFromCloudEvent(msg)
	seq := d.rec.add(calloutRecord{
		Kind: kind, Name: name, RequestID: head.RequestID, EventID: msg.Id,
		EntityID: head.EntityID, PassPresent: pass != "",
	})

	switch d.behaviour {
	case behaviourStall:
		return nil, false, nil
	case behaviourDrop:
		return nil, true, nil
	case behaviourLateCallback:
		hc := heldCallout{seq: seq, pass: pass}
		cfg, err := parseCallbackConfig(head.Parameters)
		if err != nil {
			hc.cfgErr = err.Error()
		}
		hc.cfg = cfg
		d.hold(hc)
		return nil, false, nil
	case behaviourFail, behaviourFailRetryable:
		var verdict *bool
		if d.behaviour == behaviourFailRetryable {
			v := true
			verdict = &v
		}
		msgText := "scripted failure: " + string(d.behaviour)
		var reply *cepb.CloudEvent
		var err error
		switch kind {
		case "criterion":
			reply, err = d.buildCriteriaResponse(head.RequestID, head.EntityID, false, false, msgText, verdict)
		case "function":
			reply, err = d.buildFunctionResponse(head.RequestID, "", nil, false, msgText, verdict)
		default:
			reply, err = d.buildProcessorResponse(head.RequestID, head.EntityID, nil, false, msgText, verdict)
		}
		return reply, false, err
	}

	var reply *cepb.CloudEvent
	var err error
	switch msg.Type {
	case ceTypeCriteriaRequest:
		reply, err = d.handleCriteriaRequest(ctx, payload, pass)
	case ceTypeFunctionRequest:
		reply, err = d.handleFunctionRequest(ctx, payload, pass)
	default:
		// authtype carries the executor's principal kind; processors see it as
		// Entity.AuthType.
		reply, err = d.handleProcessorRequest(ctx, payload, pass, authTypeFromCloudEvent(msg))
	}
	return reply, false, err
}
```

In `run`: derive a cancellable context so the keep-alive loop ends with the
dispatch loop (`ctx, cancel := context.WithCancel(ctx); defer cancel()` as the
first two lines, before `go d.keepAliveLoop(ctx, stream)`), delete the
`txToken :=` / `authType :=` locals (:151-160; `handleCallout` reads them), and
replace the three request cases of the `switch msg.Type` (:163-191) with one:

```go
		case ceTypeProcessorRequest, ceTypeCriteriaRequest, ceTypeFunctionRequest:
			reply, drop, err := d.handleCallout(ctx, msg, payload)
			if err != nil {
				slog.Error("calculation request failed", "pkg", "compute-test-client", "type", msg.Type, "error", err)
				continue
			}
			if drop {
				slog.Info("closing the stream on receiving work", "pkg", "compute-test-client", "type", msg.Type)
				d.close()
				return nil
			}
			if reply == nil {
				continue
			}
			if err := d.send(stream, reply); err != nil {
				slog.Error("failed to send calculation response", "pkg", "compute-test-client", "type", msg.Type, "error", err)
			}
```

`main.go` — read the two variables before the catalog is built, and build the
dispatcher before the health server (H-7 hands the health server the
dispatcher's release function):

```go
	tags := parseTags(os.Getenv("CYODA_TEST_COMPUTE_TAGS"))
	beh, err := parseBehaviour(os.Getenv("CYODA_TEST_COMPUTE_BEHAVIOUR"))
	if err != nil {
		slog.Error("invalid CYODA_TEST_COMPUTE_BEHAVIOUR", "pkg", "compute-test-client", "error", err)
		os.Exit(1)
	}
```

and `disp := newDispatcher(endpoint, token, cat)` (:81) becomes, moved up to
just after the `slog.Info("catalog loaded", …)` call:

```go
	rec := newRecorder()
	disp := newDispatcher(endpoint, token, cat, gcb, tags, beh, rec)
	slog.Info("behaviour", "pkg", "compute-test-client", "tags", tags, "behaviour", string(beh))
```

Add to the package comment: "Two optional variables let a fixture start
further clients for one scenario: CYODA_TEST_COMPUTE_TAGS (comma-separated
join tags) and CYODA_TEST_COMPUTE_BEHAVIOUR (stall, fail, fail-retryable,
late-callback, drop). Unset, the client joins as `compute-test-client` and
serves its catalog." — and correct "(e2e/parity/{memory,postgres})" to
"(e2e/parity/{memory,sqlite,postgres})".

Exit check: `grep -n '"compute-test-client"}' cmd/compute-test-client/dispatch.go` → no hits (the tag literal lives in `behaviour.go` only).

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./cmd/compute-test-client/...` and `go build ./cmd/compute-test-client`

- [ ] **Step 5: Commit**

```
git add cmd/compute-test-client/
git commit -m "test(compute-test-client): tags and a scripted behaviour from the environment; callouts recorded (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task H-7: compute-test-client — a control surface: read the record, release the late callbacks

**Spec:** §13 "What each harness can do" (second bullet, `late-callback`); R§7 ("no control surface beyond `/healthz`").

**Files:**
- Modify: `cmd/compute-test-client/health.go` (`newHealthServer` :21-42)
- Modify: `cmd/compute-test-client/dispatch.go` (append `release`, `lateCallback`)
- Modify: `cmd/compute-test-client/callback.go` (append `problemErrorCode`)
- Modify: `cmd/compute-test-client/main.go` (`newHealthServer()` call)
- Test: `cmd/compute-test-client/control_test.go` (new)

**Interfaces:**
- Consumes: `recorder`, `heldCallout`, `takeHeld` (H-6); `callbackClient.createSecondary` (`callback.go:155`), `grpcCallbackClient.createSecondary` (`grpc_callback.go:82`), `cbConfig` — existing.
- Produces, on the address the client already prints as `HEALTH_ADDR=`:
  - `GET /record` → `{"memberId": "...", "received": [calloutRecord…]}` (shape in H-6). The pass never appears in it.
  - `POST /release` → makes the late callbacks for every held callout, **then** answers with the same document, so the caller reads the outcomes from the response. For each held callout: an HTTP `POST /api/entity/JSON/{secondaryModel}/{version}` with the pass as `X-Tx-Token`, then (when a gRPC callback client exists) a gRPC `EntityManage` create with the pass as `tx-token` metadata. `secondaryModel`, `secondaryVersion` and `marker` come from the callout's `context`, the same JSON the `cb-*` processors read (`callback.go:69-94`); a callout without them records `callback.error`.
  - `func newHealthServer(rec *recorder, release func(context.Context)) (*healthServer, error)`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestControlSurface_RecordAndRelease(t *testing.T) {
	const pass = "pass-value-never-shown"

	// Stands in for cyoda's HTTP door: notes the pass it was shown, refuses.
	var sawPass atomic.Bool
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tx-Token") == pass && r.URL.Path == "/api/entity/JSON/late-secondary/1" {
			sawPass.Store(true)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"status":404,"properties":{"errorCode":"TRANSACTION_NOT_FOUND"}}`)
	}))
	defer door.Close()

	rec := newRecorder()
	rec.setMemberID("member-1")
	cat := newCatalog(newCallbackClient(door.URL, "bearer"), nil)
	d := newDispatcher("", "", cat, nil, []string{"x"}, behaviourLateCallback, rec)

	hs, err := newHealthServer(rec, d.release)
	if err != nil {
		t.Fatalf("newHealthServer: %v", err)
	}
	hs.start()
	defer hs.stop()
	base := "http://" + hs.addr()

	ce, payload := processorRequest(t, "r-1", "noop", pass, `{"secondaryModel":"late-secondary","secondaryVersion":1,"marker":"m"}`)
	if reply, _, err := d.handleCallout(context.Background(), ce, payload); err != nil || reply != nil {
		t.Fatalf("late-callback must stay silent: reply=%v err=%v", reply, err)
	}

	fetch := func(method, path string) (recordDoc, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
		}
		var doc recordDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return doc, string(raw)
	}

	before, _ := fetch(http.MethodGet, "/record")
	if before.MemberID != "member-1" || len(before.Received) != 1 || before.Received[0].Callback != nil {
		t.Fatalf("record before release = %+v", before)
	}
	if sawPass.Load() {
		t.Fatal("the callback was made before /release")
	}

	after, raw := fetch(http.MethodPost, "/release")
	if !sawPass.Load() {
		t.Fatal("/release did not present the held pass on the HTTP door")
	}
	cb := after.Received[0].Callback
	if cb == nil {
		t.Fatal("/release recorded no callback outcome")
	}
	if cb.HTTPStatus != http.StatusNotFound || cb.HTTPErrorCode != "TRANSACTION_NOT_FOUND" {
		t.Errorf("outcome = %+v; want 404 TRANSACTION_NOT_FOUND", *cb)
	}
	if cb.GRPCAttempted {
		t.Error("gRPC callback reported as attempted with no gRPC callback client")
	}
	if strings.Contains(raw, pass) {
		t.Error("the control surface exposes the pass")
	}

	// A second release has nothing left to do and changes nothing.
	again, _ := fetch(http.MethodPost, "/release")
	if again.Received[0].Callback == nil || again.Received[0].Callback.HTTPStatus != http.StatusNotFound {
		t.Errorf("second release altered the outcome: %+v", again.Received[0].Callback)
	}

	if resp, err := http.Get(base + "/release"); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET /release = %d; want 405", resp.StatusCode)
		}
	}
}
```

`processorRequest` stores `parameters` as the JSON **string** the engine sends
(`parseCallbackConfig` expects a JSON string holding JSON), which is what the
helper from H-6 does when given a non-empty `parameters`.

- [ ] **Step 2: Run to verify RED**

Run: `go test ./cmd/compute-test-client/ -run 'TestControlSurface_RecordAndRelease'`
Expected: FAIL (build) with `too many arguments in call to newHealthServer` and `d.release undefined`.

- [ ] **Step 3: Implement**

`health.go` — `newHealthServer` takes the recorder and the release function and serves two more paths (add imports `context`, `encoding/json`, `time`):

```go
// newHealthServer constructs the client's local HTTP server, bound to an
// ephemeral port: /healthz for the fixture's readiness probe, /record and
// /release as the test's control surface.
func newHealthServer(rec *recorder, release func(context.Context)) (*healthServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	hs := &healthServer{
		listener: ln,
		ready:    new(atomic.Bool),
	}
	writeRecord := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rec.snapshot())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !hs.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"DOWN"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"UP"}`))
	})
	// /record: every calculation request this client received, in order.
	mux.HandleFunc("/record", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeRecord(w)
	})
	// /release: make the held late callbacks, then answer with the record,
	// which now carries their outcomes. Not bound to the request's context: a
	// caller that gives up must not cut a callback off half-way.
	mux.HandleFunc("/release", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		release(ctx)
		writeRecord(w)
	})
	hs.srv = &http.Server{Handler: mux}
	return hs, nil
}
```

`dispatch.go` — append:

```go
// release makes the late callback for every held callout and records what
// each door answered.
func (d *dispatcher) release(ctx context.Context) {
	for _, hc := range d.takeHeld() {
		d.rec.setCallback(hc.seq, d.lateCallback(ctx, hc))
	}
}

// lateCallback presents a held pass on the HTTP door and, when a gRPC callback
// client exists, on the gRPC door, with an entity create on each.
func (d *dispatcher) lateCallback(ctx context.Context, hc heldCallout) callbackOutcome {
	var out callbackOutcome
	switch {
	case hc.cfgErr != "":
		out.Error = hc.cfgErr
		return out
	case hc.cfg.SecondaryModel == "":
		out.Error = "late-callback needs secondaryModel in the callout's context"
		return out
	case d.cat.cb == nil:
		out.Error = "callback client unavailable (CYODA_COMPUTE_HTTP_BASE unset)"
		return out
	}
	res, _, _, err := d.cat.cb.createSecondary(ctx, hc.cfg, hc.pass, hc.cfg.Marker)
	if err != nil {
		out.Error = "http callback: " + err.Error()
		return out
	}
	out.HTTPStatus = res.Status
	out.HTTPErrorCode = problemErrorCode(res.Body)

	if d.gcb == nil {
		return out
	}
	out.GRPCAttempted = true
	g, err := d.gcb.createSecondary(ctx, hc.cfg, hc.pass, hc.cfg.Marker)
	if err != nil {
		out.Error = "grpc callback: " + err.Error()
		return out
	}
	out.GRPCSuccess = g.Success
	out.GRPCErrorCode = g.ErrorCode
	out.GRPCErrorMessage = g.ErrorMsg
	return out
}
```

(`net/http` and gRPC transport errors name the URL or the status, never a
header or metadata value, so `out.Error` cannot carry the pass.)

`callback.go` — append:

```go
// problemErrorCode extracts properties.errorCode from an RFC 9457 problem
// body, or "" when the body is not one.
func problemErrorCode(body string) string {
	var pd struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
		} `json:"properties"`
	}
	if json.Unmarshal([]byte(body), &pd) != nil {
		return ""
	}
	return pd.Properties.ErrorCode
}
```

`main.go` — `hs, err := newHealthServer()` becomes `hs, err := newHealthServer(rec, disp.release)`
(the dispatcher is already built above it since H-6); in the package comment,
"A separate /healthz HTTP endpoint" becomes "A separate local HTTP endpoint
(/healthz for readiness; /record and /release for a scenario to read what the
client received and to trigger a late-callback client's callbacks)".

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./cmd/compute-test-client/...` and `go build ./cmd/compute-test-client`

- [ ] **Step 5: Commit**

```
git add cmd/compute-test-client/
git commit -m "test(compute-test-client): control surface — read the record, release late callbacks (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task H-8: fixtureutil — one way to start a compute client, for any tenant, with tags and a behaviour

**Spec:** §13 "What each harness can do" (second bullet: "the fixtures gain an optional capability to start and stop extra compute clients").

**Files:**
- Create: `e2e/parity/compute_client.go` (the types only; the capability interface and scenarios come in H-9)
- Create: `e2e/parity/fixtureutil/compute_client.go`
- Modify: `e2e/parity/fixtureutil/fixtureutil.go` (`MintM2MJWT` :209-223; `LaunchResult` :499-504; the default-client launch in `LaunchCyodaAndComputeWithBinaries` :651-711; `ClusterLaunchResult` :718-748; the default-client launch in `LaunchCyodaClusterAndComputeWithBinaries` :1071-1136)
- Test: `e2e/parity/fixtureutil/compute_client_test.go` (new)

**Interfaces:**
- Consumes: the client's env and control surface (H-6, H-7).
- Produces:
  - in `parity`: `ComputeClientSpec`, the `ComputeBehaviour*` constants, `ReceivedCallout`, `LateCallbackOutcome`, `ComputeClient` (exact definitions below)
  - `func fixtureutil.MintM2MJWTForTenant(ks *JWTKeySet, tenantID string) (string, error)`
  - `type fixtureutil.ComputeClientOpts struct { ComputeBin, GRPCEndpoint, HTTPBase, Token string; Tags []string; Behaviour string; ReadyTimeout time.Duration }`
  - `func fixtureutil.StartComputeClient(opts ComputeClientOpts) (*ComputeClientProc, error)`
  - `*ComputeClientProc` implements `parity.ComputeClient`; also `Cmd() *exec.Cmd`, `ControlURL() string`
  - `func fixtureutil.StartComputeClientForFixture(t *testing.T, ks *JWTKeySet, computeBin, grpcEndpoint, httpBase string, spec parity.ComputeClientSpec) parity.ComputeClient` — what a fixture's capability method calls
  - `LaunchResult.ComputeBin string`; `ClusterLaunchResult.ComputeBin string`, `ClusterLaunchResult.GRPCEndpoints []string` (one per node, same order as `BaseURLs`)

**Consolidation.** Today the default client is launched by two copies of the
same thirty lines (single node :658-703, cluster :1078-1122). A third copy for
extra clients is not added: both are replaced by `StartComputeClient`.
Signatures used by the commercial backend's suite
(`LaunchCyodaAndComputeWithBinaries`, `BuildComputeBinary`, `GenerateJWTKeySet`,
`MintTenantJWT`, `MintComputeTenantJWT`, `LaunchOpts`) do not change; the result
structs only gain fields.

Existing tests: `cluster_with_binaries_test.go:61` asserts `result.ComputeCmd != nil` — stays true (`proc.Cmd()`). `launch_single_retry_test.go` fails in the cyoda launch, before any compute client — unaffected. `launch_retry_test.go` — unaffected.

- [ ] **Step 1: Write the failing tests**

```go
package fixtureutil_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// TestMintM2MJWTForTenant: the token names the given tenant and carries
// ROLE_M2M, which StartStreaming requires.
func TestMintM2MJWTForTenant(t *testing.T) {
	ks, err := fixtureutil.GenerateJWTKeySet()
	if err != nil {
		t.Fatalf("GenerateJWTKeySet: %v", err)
	}
	tok, err := fixtureutil.MintM2MJWTForTenant(ks, "tenant-xyz")
	if err != nil {
		t.Fatalf("MintM2MJWTForTenant: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts; want 3", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims struct {
		Org    string   `json:"caas_org_id"`
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Org != "tenant-xyz" {
		t.Errorf("caas_org_id = %q; want tenant-xyz", claims.Org)
	}
	hasM2M := false
	for _, s := range claims.Scopes {
		hasM2M = hasM2M || s == "ROLE_M2M"
	}
	if !hasM2M {
		t.Errorf("scopes = %v; want ROLE_M2M among them", claims.Scopes)
	}
}

// TestStartComputeClient_JoinsRecordsAndStops: an extra client started beside
// the fixture's own joins the server (it holds a member id), serves its
// control surface, and is gone after Stop.
func TestStartComputeClient_JoinsRecordsAndStops(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess launch under -short")
	}
	ks, err := fixtureutil.GenerateJWTKeySet()
	if err != nil {
		t.Fatalf("GenerateJWTKeySet: %v", err)
	}
	result, cleanup, err := fixtureutil.LaunchCyodaAndCompute(ks, []string{"CYODA_STORAGE_BACKEND=memory"})
	if err != nil {
		t.Fatalf("LaunchCyodaAndCompute: %v", err)
	}
	t.Cleanup(cleanup)
	if result.ComputeBin == "" {
		t.Fatal("LaunchResult.ComputeBin is empty; a fixture needs it to start further clients")
	}

	var cc parity.ComputeClient = fixtureutil.StartComputeClientForFixture(t, ks, result.ComputeBin,
		result.GRPCEndpoint, result.BaseURL, parity.ComputeClientSpec{
			TenantID:  "h8-extra-tenant",
			Tags:      []string{"h8-extra"},
			Behaviour: parity.ComputeBehaviourStall,
		})
	if cc.MemberID() == "" {
		t.Error("the extra client reports no member id; it did not complete its join")
	}
	if got := cc.Received(t); len(got) != 0 {
		t.Errorf("a fresh client has received %d callouts; want 0", len(got))
	}

	proc := cc.(*fixtureutil.ComputeClientProc)
	cc.Stop()
	cc.Stop() // idempotent
	hc := &http.Client{Timeout: 2 * time.Second}
	if resp, err := hc.Get(proc.ControlURL() + "/healthz"); err == nil {
		resp.Body.Close()
		t.Error("the client's control endpoint still answers after Stop")
	}
}

// TestStartComputeClient_RejectsUnknownBehaviour: the client refuses to start,
// and the launcher reports it instead of waiting out the readiness timeout.
func TestStartComputeClient_RejectsUnknownBehaviour(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess launch under -short")
	}
	bin, err := fixtureutil.BuildComputeBinary()
	if err != nil {
		t.Fatalf("BuildComputeBinary: %v", err)
	}
	start := time.Now()
	_, err = fixtureutil.StartComputeClient(fixtureutil.ComputeClientOpts{
		ComputeBin: bin, GRPCEndpoint: "127.0.0.1:1", Token: "x", Behaviour: "explode",
	})
	if err == nil {
		t.Fatal("StartComputeClient accepted an unknown behaviour")
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %s to report a client that exits at once", time.Since(start))
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./e2e/parity/fixtureutil/ -run 'TestMintM2MJWTForTenant|TestStartComputeClient'`
Expected: FAIL (build) with `undefined: fixtureutil.MintM2MJWTForTenant` (and `parity.ComputeClientSpec`, `fixtureutil.StartComputeClient`, …).

- [ ] **Step 3: Implement**

`e2e/parity/compute_client.go`:

```go
package parity

import "testing"

// Behaviours a further compute client can be started with (see
// cmd/compute-test-client/behaviour.go). Each applies to every calculation
// request the client receives.
const (
	ComputeBehaviourCatalog       = ""               // serve the catalog, like the fixture's own client
	ComputeBehaviourStall         = "stall"          // take the work, never answer
	ComputeBehaviourFail          = "fail"           // answer success=false, no verdict
	ComputeBehaviourFailRetryable = "fail-retryable" // answer success=false, retryable=true
	ComputeBehaviourLateCallback  = "late-callback"  // never answer; call back with the pass on Release
	ComputeBehaviourDrop          = "drop"           // close the stream on receiving work
)

// ComputeClientSpec describes a further compute client for one scenario.
type ComputeClientSpec struct {
	// TenantID is the tenant the client joins under. Required. Use a FRESH
	// tenant (fixture.NewTenant) unless the scenario is about the shared one:
	// a callout with empty calculationNodesTags matches every compute client
	// of its tenant, so a misbehaving client under the shared compute tenant
	// would be handed other scenarios' work.
	TenantID string
	// Tags are the client's join tags. Required. Keep them short.
	Tags []string
	// Behaviour is one of the ComputeBehaviour* constants.
	Behaviour string
}

// ReceivedCallout is one calculation request as a compute client received it.
// The pass is never exposed — only whether one came with the work.
type ReceivedCallout struct {
	Seq         int                  `json:"seq"` // 1-based arrival order at this client
	Kind        string               `json:"kind"` // processor | criterion | function
	Name        string               `json:"name"`
	RequestID   string               `json:"requestId"`
	EventID     string               `json:"eventId"` // the CloudEvent id
	EntityID    string               `json:"entityId"`
	PassPresent bool                 `json:"passPresent"`
	Callback    *LateCallbackOutcome `json:"callback,omitempty"`
}

// LateCallbackOutcome is what the two doors answered when a late-callback
// client presented, after Release, the pass it had been given.
type LateCallbackOutcome struct {
	HTTPStatus       int    `json:"httpStatus"`
	HTTPErrorCode    string `json:"httpErrorCode,omitempty"`
	GRPCAttempted    bool   `json:"grpcAttempted"`
	GRPCSuccess      bool   `json:"grpcSuccess"`
	GRPCErrorCode    string `json:"grpcErrorCode,omitempty"`
	GRPCErrorMessage string `json:"grpcErrorMessage,omitempty"`
	Error            string `json:"error,omitempty"` // the callback could not be made at all
}

// ComputeClient is a handle on a further compute client.
type ComputeClient interface {
	// MemberID is the id the server gave the client when it joined.
	MemberID() string
	// Received returns every calculation request the client has received, in
	// arrival order. It still works after a "drop" client closed its stream.
	Received(t *testing.T) []ReceivedCallout
	// Release tells a late-callback client to make its callbacks now, waits
	// for them, and returns the record, which then carries their outcomes.
	Release(t *testing.T) []ReceivedCallout
	// Stop ends the client's process and returns once it is reaped. The server
	// notices the closed stream a moment later, so a scenario asserting that
	// the client is gone polls for it. Calling Stop again is harmless.
	Stop()
}
```

`e2e/parity/fixtureutil/compute_client.go`:

```go
package fixtureutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
)

// ComputeClientOpts says how to start one compute-test-client.
type ComputeClientOpts struct {
	ComputeBin   string // absolute path of the built compute-test-client
	GRPCEndpoint string // host:port of the cyoda node the client attaches to
	HTTPBase     string // HTTP base its callbacks go to
	Token        string // M2M bearer for the tenant it joins under; never logged
	Tags         []string
	Behaviour    string
	// ReadyTimeout bounds the wait for the client to report ready (it does so
	// once it holds the greet). Zero means 30s.
	ReadyTimeout time.Duration
}

// ComputeClientProc is a running compute-test-client. It implements
// parity.ComputeClient.
type ComputeClientProc struct {
	cmd        *exec.Cmd
	controlURL string
	memberID   string
	stopOnce   sync.Once
}

var _ parity.ComputeClient = (*ComputeClientProc)(nil)

// StartComputeClient starts one compute-test-client, waits until it has joined
// the server, and returns a handle on it. The fixture's own client and every
// further one a scenario asks for are started here.
func StartComputeClient(opts ComputeClientOpts) (*ComputeClientProc, error) {
	ready := opts.ReadyTimeout
	if ready == 0 {
		ready = 30 * time.Second
	}
	cmd := exec.Command(opts.ComputeBin)
	cmd.WaitDelay = 3 * time.Second
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("CYODA_COMPUTE_GRPC_ENDPOINT=%s", opts.GRPCEndpoint),
		fmt.Sprintf("CYODA_COMPUTE_TOKEN=%s", opts.Token),
		fmt.Sprintf("CYODA_COMPUTE_HTTP_BASE=%s", opts.HTTPBase),
		fmt.Sprintf("CYODA_TEST_COMPUTE_TAGS=%s", strings.Join(opts.Tags, ",")),
		fmt.Sprintf("CYODA_TEST_COMPUTE_BEHAVIOUR=%s", opts.Behaviour),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create compute stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start compute-test-client: %w", err)
	}
	p := &ComputeClientProc{cmd: cmd}

	// ParseHealthAddr returns at once when stdout closes without the line,
	// which is what a client that refuses to start looks like.
	healthAddr, err := ParseHealthAddr(stdout, defaultComputeHealthAddrTimeout)
	if err != nil {
		p.Stop()
		return nil, fmt.Errorf("failed to parse HEALTH_ADDR from compute-test-client: %w", err)
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	p.controlURL = "http://" + healthAddr

	if err := WaitForHTTPHealth(p.controlURL+"/healthz", ready); err != nil {
		p.Stop()
		return nil, fmt.Errorf("compute-test-client health probe failed: %w", err)
	}
	doc, err := p.fetch(http.MethodGet, "/record")
	if err != nil {
		p.Stop()
		return nil, fmt.Errorf("failed to read compute-test-client record: %w", err)
	}
	p.memberID = doc.MemberID
	return p, nil
}

// computeRecordDoc mirrors the client's /record document.
type computeRecordDoc struct {
	MemberID string                   `json:"memberId"`
	Received []parity.ReceivedCallout `json:"received"`
}

func (p *ComputeClientProc) fetch(method, path string) (computeRecordDoc, error) {
	req, err := http.NewRequest(method, p.controlURL+path, nil)
	if err != nil {
		return computeRecordDoc{}, fmt.Errorf("failed to build %s %s: %w", method, path, err)
	}
	// /release waits for the callbacks it triggers; each is bounded at 15s.
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return computeRecordDoc{}, fmt.Errorf("failed to call %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return computeRecordDoc{}, fmt.Errorf("failed to read %s %s: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return computeRecordDoc{}, fmt.Errorf("%s %s answered %d: %s", method, path, resp.StatusCode, raw)
	}
	var doc computeRecordDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return computeRecordDoc{}, fmt.Errorf("failed to decode %s %s: %w", method, path, err)
	}
	return doc, nil
}

// MemberID implements parity.ComputeClient.
func (p *ComputeClientProc) MemberID() string { return p.memberID }

// Received implements parity.ComputeClient.
func (p *ComputeClientProc) Received(t *testing.T) []parity.ReceivedCallout {
	t.Helper()
	doc, err := p.fetch(http.MethodGet, "/record")
	if err != nil {
		t.Fatalf("compute client record: %v", err)
	}
	return doc.Received
}

// Release implements parity.ComputeClient.
func (p *ComputeClientProc) Release(t *testing.T) []parity.ReceivedCallout {
	t.Helper()
	doc, err := p.fetch(http.MethodPost, "/release")
	if err != nil {
		t.Fatalf("compute client release: %v", err)
	}
	return doc.Received
}

// Stop implements parity.ComputeClient. The process has no monitor goroutine,
// so KillProcessGroup — which owns the one Wait — is the right teardown, once.
func (p *ComputeClientProc) Stop() {
	p.stopOnce.Do(func() { KillProcessGroup(p.cmd) })
}

// Cmd returns the client's process handle, for diagnostics.
func (p *ComputeClientProc) Cmd() *exec.Cmd { return p.cmd }

// ControlURL returns the base URL of the client's local control endpoint.
func (p *ComputeClientProc) ControlURL() string { return p.controlURL }

// StartComputeClientForFixture is what a fixture's StartComputeClient calls:
// it checks the spec, mints an M2M bearer for the spec's tenant from the
// fixture's key set, and starts the client. It fails the test on any error.
func StartComputeClientForFixture(t *testing.T, ks *JWTKeySet, computeBin, grpcEndpoint, httpBase string, spec parity.ComputeClientSpec) parity.ComputeClient {
	t.Helper()
	if spec.TenantID == "" || len(spec.Tags) == 0 {
		t.Fatalf("ComputeClientSpec needs a TenantID and at least one tag: %+v", spec)
	}
	token, err := MintM2MJWTForTenant(ks, spec.TenantID)
	if err != nil {
		t.Fatalf("failed to mint M2M JWT for the compute client: %v", err)
	}
	p, err := StartComputeClient(ComputeClientOpts{
		ComputeBin: computeBin, GRPCEndpoint: grpcEndpoint, HTTPBase: httpBase,
		Token: token, Tags: spec.Tags, Behaviour: spec.Behaviour,
	})
	if err != nil {
		t.Fatalf("failed to start compute client: %v", err)
	}
	return p
}
```

An empty `CYODA_TEST_COMPUTE_TAGS=` / `CYODA_TEST_COMPUTE_BEHAVIOUR=` is the
unset case for the client (`parseTags`, `parseBehaviour`), so the fixture's own
client is started with both empty and behaves exactly as today.

`fixtureutil.go`:

```go
// MintM2MJWT creates the M2M JWT of the fixture's own compute-test-client.
func MintM2MJWT(ks *JWTKeySet) (string, error) { return MintM2MJWTForTenant(ks, ComputeTenantID) }

// MintM2MJWTForTenant creates an M2M JWT under which a compute-test-client
// joins as a compute node of tenantID.
func MintM2MJWTForTenant(ks *JWTKeySet, tenantID string) (string, error) {
	now := time.Now()
	claims := map[string]any{
		"sub":          "compute-test",
		"iss":          ks.Issuer,
		"caas_user_id": "compute-admin",
		"caas_org_id":  tenantID,
		"scopes":       []string{"ROLE_ADMIN", "ROLE_M2M"},
		"caas_tier":    "unlimited",
		"exp":          now.Add(2 * time.Hour).Unix(),
		"iat":          now.Unix(),
		"jti":          uuid.NewString(),
	}
	return auth.Sign(claims, ks.Key, ks.Kid)
}
```

In `LaunchCyodaAndComputeWithBinaries`, everything from `// Launch compute-test-client.`
through the `slog.Info("compute-test-client is ready", …)` line (:658-703) becomes:

```go
	grpcEndpoint := fmt.Sprintf("127.0.0.1:%d", grpcPort)
	// Callbacks target the same single node that dispatched them.
	compute, err := StartComputeClient(ComputeClientOpts{
		ComputeBin: computeBin, GRPCEndpoint: grpcEndpoint, HTTPBase: baseURL, Token: m2mToken,
	})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	cleanup = func() {
		// The client owns its own Wait; cyoda is reaped by its monitor
		// goroutine, so it is torn down kill-only + exit-signal wait.
		compute.Stop()
		killCyoda()
	}
	slog.Info("compute-test-client is ready", "pkg", "fixtureutil", "controlURL", compute.ControlURL())
```

and the returned `LaunchResult` sets `ComputeCmd: compute.Cmd(), ComputeBin: computeBin`.
`LaunchResult` gains:

```go
	// ComputeBin is the built compute-test-client, so a fixture can start
	// further clients for a scenario (fixtureutil.StartComputeClientForFixture).
	ComputeBin string
```

In `LaunchCyodaClusterAndComputeWithBinaries`, the block from
`// Compute-test-client points at node 0's gRPC.` through the
`slog.Info("compute-test-client (cluster) is ready", …)` line (:1078-1122) becomes:

```go
	// The fixture's own client attaches to node 0, and its callbacks target
	// node 0; cross-node callback forwarding is covered by scenarios.
	grpcEndpoints := make([]string, n)
	for i := 0; i < n; i++ {
		grpcEndpoints[i] = fmt.Sprintf("127.0.0.1:%d", grpcPorts[i])
	}
	compute, err := StartComputeClient(ComputeClientOpts{
		ComputeBin: computeBin, GRPCEndpoint: grpcEndpoints[0],
		HTTPBase: fmt.Sprintf("http://127.0.0.1:%d", httpPorts[0]), Token: m2mToken,
		ReadyTimeout: defaultCyodaReadinessTimeout,
	})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	cleanup = func() {
		compute.Stop()
		killNodes(nodes)
	}
	slog.Info("compute-test-client (cluster) is ready", "pkg", "fixtureutil", "controlURL", compute.ControlURL(), "nodes", n)
```

and the returned `ClusterLaunchResult` sets `GRPCEndpoint: grpcEndpoints[0]`,
`GRPCEndpoints: grpcEndpoints`, `ComputeCmd: compute.Cmd()`, `ComputeBin: computeBin`.
`ClusterLaunchResult` gains:

```go
	// GRPCEndpoints is the per-node gRPC endpoint list, same order as BaseURLs.
	GRPCEndpoints []string
	// ComputeBin is the built compute-test-client (see LaunchResult.ComputeBin).
	ComputeBin string
```

Exit checks:
`grep -n 'exec.Command(computeBin)' e2e/parity/fixtureutil/` → no hits.
`grep -n 'CYODA_COMPUTE_GRPC_ENDPOINT' e2e/parity/fixtureutil/*.go` → one hit, in `compute_client.go`.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./e2e/parity/fixtureutil/...` then, because every fixture launches through the changed code, `go test ./e2e/parity/memory/... ./e2e/parity/sqlite/...` (no Docker needed for these two). `go vet ./e2e/...`.

- [ ] **Step 5: Commit**

```
git add e2e/parity/compute_client.go e2e/parity/fixtureutil/
git commit -m "test(parity): one compute-client launcher — any tenant, tags and a behaviour (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task H-9: parity — the optional capability to start and stop compute clients, on all three single-node fixtures

**Spec:** §13 "What each harness can do" (second bullet, incl. "A backend fixture without the capability skips those scenarios, so the commercial backend's suite is not broken by their arrival").

**Files:**
- Modify: `e2e/parity/compute_client.go` (append the capability, the helpers and two scenarios)
- Modify: `e2e/parity/fixture.go` (the `BackendFixture` doc comment :14-15)
- Modify: `e2e/parity/registry.go` (two entries; header count), `e2e/parity/registry_count_test.go` (`wantParityScenarioCount` 275 → 277)
- Modify: `e2e/parity/memory/fixture.go`, `e2e/parity/sqlite/fixture.go`, `e2e/parity/postgres/fixture.go` (a `computeBin` field, set from `result.ComputeBin`; the capability method)
- Test: `e2e/parity/compute_client_test.go` (new)

**Interfaces:**
- Consumes: H-8; `cbWorkflowDoc`, `cbProc`, `cbSetupModel`, `cbContext`, `cbSampleNoWriteback`, `cbSampleSecondary`, `cbSecondaryWorkflow` (`callback_txjoin.go`), `containsErrorCode` (`grouped_stats.go:841`) — existing.
- Produces:
  - `type parity.ComputeClientFixture interface { StartComputeClient(t *testing.T, spec ComputeClientSpec) ComputeClient }`
  - `func parity.StartComputeClientOrSkip(t *testing.T, fixture BackendFixture, spec ComputeClientSpec) ComputeClient` — skips when the fixture lacks the capability; registers `t.Cleanup(cc.Stop)`
  - `func parity.AwaitReceived(t *testing.T, cc ComputeClient, n int, within time.Duration) []ReceivedCallout`
  - `func parity.ComputeClientWorkflow(wfName, procName, tag, contextValue string, extraConfig map[string]any) string` — NONE→ACTIVE, one SYNC processor on `tag`; `extraConfig` is where a scenario puts `idempotent`, `retryPolicy`, `responseTimeoutMs`
  - registered scenarios `ComputeClientJoinServeLeave`, `ComputeClientBehaviours`

**How a failover scenario isolates itself (for the streams that write them).**
Each uses a **fresh tenant** (`fixture.NewTenant`) and a tag of its own, and
starts its clients one after the other. The fresh tenant is what keeps a
stalling client from being handed another scenario's work (empty
`calculationNodesTags` matches every compute client of the tenant), keeps the
selector's state from carrying over, and makes a client that is still being
evicted when the next scenario starts harmless. Scenarios run sequentially (no
`t.Parallel()` anywhere under `e2e/parity`), and `t.Cleanup` stops the clients.

Both scenarios assert only what holds today and after the feature: a stalled or
dropped try on a processor that is not `idempotent` ends the callout with the
try's own code (spec §8.2 rows 2-3); a failure answer is 400 `WORKFLOW_FAILED`;
no compute client for the tag is 503 `NO_COMPUTE_MEMBER_FOR_TAG` (after the
patience, which H-11 makes short).

- [ ] **Step 1: Write the failing tests**

`e2e/parity/compute_client_test.go`:

```go
package parity

import "testing"

// stubNoComputeClients is a BackendFixture WITHOUT the compute-client
// capability — the shape of an out-of-tree backend that has not wired it yet.
type stubNoComputeClients struct{}

func (stubNoComputeClients) BaseURL() string                 { return "http://127.0.0.1:1" }
func (stubNoComputeClients) GRPCEndpoint() string            { return "127.0.0.1:1" }
func (stubNoComputeClients) NewTenant(*testing.T) Tenant     { return Tenant{ID: "t"} }
func (stubNoComputeClients) ComputeTenant(*testing.T) Tenant { return Tenant{ID: "t"} }

var _ BackendFixture = stubNoComputeClients{}

// TestComputeClientScenariosSkipWithoutCapability: a fixture that cannot start
// compute clients sees these scenarios as skipped, never failed, and before
// they touch the server.
func TestComputeClientScenariosSkipWithoutCapability(t *testing.T) {
	scenarios := map[string]func(*testing.T, BackendFixture){
		"ComputeClientJoinServeLeave": RunComputeClientJoinServeLeave,
		"ComputeClientBehaviours":     RunComputeClientBehaviours,
	}
	for name, fn := range scenarios {
		skipped := t.Run(name, func(t *testing.T) {
			fn(t, stubNoComputeClients{})
			t.Fatalf("scenario %s did not skip on a fixture without the capability", name)
		})
		if !skipped {
			t.Errorf("scenario %s must t.Skip when the fixture cannot start compute clients", name)
		}
	}
}

func TestComputeClientScenariosRegistered(t *testing.T) {
	want := map[string]bool{"ComputeClientJoinServeLeave": false, "ComputeClientBehaviours": false}
	for _, nt := range allTests {
		if _, ok := want[nt.Name]; ok {
			want[nt.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("scenario %s is not in the registry", name)
		}
	}
}
```

The scenarios themselves (append to `e2e/parity/compute_client.go`; imports
become `net/http`, `testing`, `time`, and
`"github.com/cyoda-platform/cyoda-go/e2e/parity/client"`):

```go
// ComputeClientFixture is an OPTIONAL capability: a fixture that implements it
// can start further compute clients for one scenario. It is not part of
// BackendFixture, so an out-of-tree backend that has not wired it keeps
// compiling against the shared registry; scenarios that need it skip there.
type ComputeClientFixture interface {
	// StartComputeClient starts a compute client per spec and returns once it
	// has joined the server. Implementations MUST call t.Helper() and t.Fatal
	// on failure. In-tree fixtures delegate to
	// fixtureutil.StartComputeClientForFixture.
	StartComputeClient(t *testing.T, spec ComputeClientSpec) ComputeClient
}

func requireComputeClients(t *testing.T, fixture BackendFixture) ComputeClientFixture {
	t.Helper()
	cf, ok := fixture.(ComputeClientFixture)
	if !ok {
		t.Skip("fixture cannot start further compute clients; scenario pending on this backend")
	}
	return cf
}

// StartComputeClientOrSkip starts a further compute client for this scenario,
// or skips the scenario when the fixture lacks the capability. The client is
// stopped when the scenario ends.
func StartComputeClientOrSkip(t *testing.T, fixture BackendFixture, spec ComputeClientSpec) ComputeClient {
	t.Helper()
	cc := requireComputeClients(t, fixture).StartComputeClient(t, spec)
	t.Cleanup(cc.Stop)
	return cc
}

// AwaitReceived polls until cc has received at least n calculation requests.
func AwaitReceived(t *testing.T, cc ComputeClient, n int, within time.Duration) []ReceivedCallout {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := cc.Received(t)
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("compute client %s received %d requests within %s; want at least %d", cc.MemberID(), len(got), within, n)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// ComputeClientWorkflow builds a NONE -> ACTIVE workflow whose automated
// transition carries one SYNC processor routed to tag. extraConfig is merged
// into the processor's config.
func ComputeClientWorkflow(wfName, procName, tag, contextValue string, extraConfig map[string]any) string {
	cfg := map[string]any{"calculationNodesTags": tag}
	for k, v := range extraConfig {
		cfg[k] = v
	}
	return cbWorkflowDoc(wfName, map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "init", "next": "ACTIVE", "manual": false,
			"processors": []any{cbProc(procName, "SYNC", contextValue, cfg)},
		}}},
		"ACTIVE": map[string]any{},
	})
}

const computeClientEntity = `{"name":"Test","amount":10,"status":"new"}`

// RunComputeClientJoinServeLeave proves the capability end to end: before the
// client starts its tag has no compute node; once started it is given the
// work; once stopped the tag has no compute node again.
func RunComputeClientJoinServeLeave(t *testing.T, fixture BackendFixture) {
	requireComputeClients(t, fixture)
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const model, tag = "h-cc-join-leave", "h-cc-jl"
	cbSetupModel(t, c, model, cbSampleNoWriteback, ComputeClientWorkflow("h-cc-jl-wf", "noop", tag, "", nil))

	noComputeNode := func() bool {
		status, body, err := c.CreateEntityRaw(t, model, 1, computeClientEntity)
		if err != nil {
			t.Fatalf("CreateEntityRaw: %v", err)
		}
		return status == http.StatusServiceUnavailable && containsErrorCode(body, "NO_COMPUTE_MEMBER_FOR_TAG")
	}
	if !noComputeNode() {
		t.Fatal("before any client started: want 503 NO_COMPUTE_MEMBER_FOR_TAG")
	}

	cc := StartComputeClientOrSkip(t, fixture, ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}})
	if cc.MemberID() == "" {
		t.Fatal("the started client reports no member id")
	}
	if _, err := c.CreateEntity(t, model, 1, computeClientEntity); err != nil {
		t.Fatalf("create with the client attached: %v", err)
	}
	got := cc.Received(t)
	if len(got) != 1 {
		t.Fatalf("client received %d requests; want 1: %+v", len(got), got)
	}
	if r := got[0]; r.Seq != 1 || r.Kind != "processor" || r.Name != "noop" || r.RequestID == "" || r.EventID == "" || !r.PassPresent {
		t.Errorf("record = %+v", r)
	}

	cc.Stop()
	// The server learns of the closed stream a moment after the process is
	// gone; until then a create may be routed to the dying member.
	deadline := time.Now().Add(10 * time.Second)
	for !noComputeNode() {
		if time.Now().After(deadline) {
			t.Fatal("10s after Stop the server still routes to the stopped client")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// RunComputeClientBehaviours proves each scripted behaviour reaches the server
// as intended. Every case runs under a tenant of its own.
func RunComputeClientBehaviours(t *testing.T, fixture BackendFixture) {
	requireComputeClients(t, fixture)

	cases := []struct {
		behaviour  string
		timeoutMs  int
		wantStatus int
		wantCode   string
	}{
		{ComputeBehaviourStall, 300, http.StatusServiceUnavailable, "DISPATCH_TIMEOUT"},
		{ComputeBehaviourFail, 0, http.StatusBadRequest, "WORKFLOW_FAILED"},
		{ComputeBehaviourFailRetryable, 0, http.StatusBadRequest, "WORKFLOW_FAILED"},
		{ComputeBehaviourDrop, 0, http.StatusServiceUnavailable, "COMPUTE_MEMBER_DISCONNECTED"},
		{ComputeBehaviourLateCallback, 300, http.StatusServiceUnavailable, "DISPATCH_TIMEOUT"},
	}
	for _, tc := range cases {
		t.Run(tc.behaviour, func(t *testing.T) {
			tenant := fixture.NewTenant(t)
			c := client.NewClient(fixture.BaseURL(), tenant.Token)
			model, secondary, tag := "h-cc-beh-"+tc.behaviour, "h-cc-beh-sec-"+tc.behaviour, "h-cc-"+tc.behaviour

			cbSetupModel(t, c, secondary, cbSampleSecondary, cbSecondaryWorkflow)
			var extra map[string]any
			if tc.timeoutMs > 0 {
				extra = map[string]any{"responseTimeoutMs": tc.timeoutMs}
			}
			cbSetupModel(t, c, model, cbSampleNoWriteback,
				ComputeClientWorkflow("h-cc-beh-wf", "noop", tag, cbContext(secondary, "h-cc-late"), extra))

			cc := StartComputeClientOrSkip(t, fixture, ComputeClientSpec{
				TenantID: tenant.ID, Tags: []string{tag}, Behaviour: tc.behaviour,
			})

			status, body, err := c.CreateEntityRaw(t, model, 1, computeClientEntity)
			if err != nil {
				t.Fatalf("CreateEntityRaw: %v", err)
			}
			if status != tc.wantStatus || !containsErrorCode(body, tc.wantCode) {
				t.Fatalf("got %d %s; want %d %s", status, body, tc.wantStatus, tc.wantCode)
			}
			// The record outlives the stream: a "drop" client still answers.
			if got := AwaitReceived(t, cc, 1, 5*time.Second); !got[0].PassPresent {
				t.Errorf("record = %+v; want a pass to have come with the work", got[0])
			}

			if tc.behaviour != ComputeBehaviourLateCallback {
				return
			}
			got := cc.Release(t)
			cb := got[0].Callback
			if cb == nil {
				t.Fatal("Release recorded no callback outcome")
			}
			if cb.Error != "" {
				t.Fatalf("the late callback could not be made: %s", cb.Error)
			}
			// The callout has ended and its transaction is rolled back. Both
			// doors evaluate the pass and refuse; which code they give is the
			// fencing scenarios' subject, not this one's.
			if cb.HTTPStatus == 0 || cb.HTTPStatus == http.StatusOK {
				t.Errorf("HTTP door answered %d to a pass whose callout has ended", cb.HTTPStatus)
			}
			if !cb.GRPCAttempted || cb.GRPCSuccess {
				t.Errorf("gRPC door: attempted=%t success=%t; want attempted and refused", cb.GRPCAttempted, cb.GRPCSuccess)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./e2e/parity/ -run 'TestComputeClientScenarios|TestParityScenarioCount'`
Expected: with only the test file written, FAIL (build) `undefined: RunComputeClientJoinServeLeave`. With the scenarios written but not registered: `TestComputeClientScenariosRegistered` FAILS "scenario … is not in the registry". With them registered but the count not bumped: `TestParityScenarioCount` FAILS "parity scenario count = 277, want 275". With all of that done but the fixtures not yet implementing the capability, `go test ./e2e/parity/memory/...` reports both scenarios **SKIP** — the state the commercial backend will be in, and the reason the fixture step below is part of this task.

- [ ] **Step 3: Implement**

`registry.go` — after the `Workflow…`/compute block of `allTests` add:

```go
	// Compute-client capability self-tests: a fixture that can start further
	// compute clients proves it here; one that cannot skips.
	{"ComputeClientJoinServeLeave", RunComputeClientJoinServeLeave},
	{"ComputeClientBehaviours", RunComputeClientBehaviours},
```

and change the header's "Total parity scenarios: 275" to 277;
`registry_count_test.go`: `const wantParityScenarioCount = 277`.

`fixture.go` — the `BackendFixture` comment's second bullet is no longer true as
written; replace it:

```go
//   - There is no compute-client handle on this interface — the fixture's own
//     compute-test-client is a separate subprocess reached via gRPC. A fixture
//     that can start FURTHER compute clients for a scenario advertises it
//     through the optional ComputeClientFixture (compute_client.go).
```

Each of the three fixtures gains a field and a method. `memory/fixture.go`:

```go
type memoryFixture struct {
	baseURL      string
	grpcEndpoint string
	keySet       *fixtureutil.JWTKeySet
	computeBin   string
}

// StartComputeClient implements parity.ComputeClientFixture.
func (f *memoryFixture) StartComputeClient(t *testing.T, spec parity.ComputeClientSpec) parity.ComputeClient {
	t.Helper()
	return fixtureutil.StartComputeClientForFixture(t, f.keySet, f.computeBin, f.grpcEndpoint, f.baseURL, spec)
}
```

and in `setup()` the literal gains `computeBin: result.ComputeBin,`.
`sqlite/fixture.go` (`sqliteFixture`) and `postgres/fixture.go`
(`postgresFixture`) get the same field, the same method on their
own receiver type, and the same line in their `setup()` literal. In each of the
three packages add the compile-time assertion next to the struct:
`var _ parity.ComputeClientFixture = (*memoryFixture)(nil)` (resp.
`*sqliteFixture`, `*postgresFixture`).

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./e2e/parity/` (unit: skip, registered, count), then
`go test ./e2e/parity/memory/... ./e2e/parity/sqlite/... ./e2e/parity/postgres/...` (postgres needs Docker). In the output the two scenarios must be PASS, not SKIP, on all three: check with
`go test ./e2e/parity/memory/ -run 'TestParity/ComputeClient' -json | grep -c '"Action":"skip"'` → `0`.

- [ ] **Step 5: Commit**

```
git add e2e/parity/compute_client.go e2e/parity/compute_client_test.go e2e/parity/fixture.go e2e/parity/registry.go \
  e2e/parity/registry_count_test.go e2e/parity/memory/fixture.go e2e/parity/sqlite/fixture.go e2e/parity/postgres/fixture.go
git commit -m "test(parity): optional fixture capability to start and stop compute clients (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task H-10: multi-pnode parity — start a compute client on a chosen pnode

**Spec:** §13 "What each harness can do" (second bullet), applied to the **M** column: "Owner's cnode fails → hand-over succeeds", "Hand-over: peer makes two tries", "cnode message and verdict survive the hand-over", "Pass minted by another pnode…", "Two tenants share a tag…", "Tries made by another pnode…".

**Files:**
- Create: `e2e/parity/multinode/compute_client.go`
- Modify: `e2e/parity/postgres/multinode_fixture.go` (`pgMultiNode` :24-39; the literal at the end of `MustSetupMultiNodeWithEnv` :185-191)
- Test: `e2e/parity/multinode/compute_client_skip_test.go` (new)

**Interfaces:**
- Consumes: H-8 (`ClusterLaunchResult.GRPCEndpoints`, `.ComputeBin`, `StartComputeClientForFixture`), H-9 (`parity.ComputeClientWorkflow`, `parity.AwaitReceived`).
- Produces:
  - `type multinode.ComputeClientCapable interface { StartComputeClient(t *testing.T, node int, spec parity.ComputeClientSpec) parity.ComputeClient }` — the client attaches to pnode `node`'s gRPC endpoint and sends its callbacks to pnode `node`'s HTTP base
  - `func multinode.StartComputeClientOrSkip(t *testing.T, fixture MultiNodeFixture, node int, spec parity.ComputeClientSpec) parity.ComputeClient` — skips without the capability; registers `t.Cleanup(cc.Stop)`
  - registered scenario `ComputeClient_AttachToNode`

The scenario asserts what today's cluster dispatch already does — an owner with
no local compute client forwards to the pnode that advertises the tag — and
what the owner's loop will do too. It does not assert "leave" across pnodes:
that path (`findPeerWithPolling`, `forwardWithFailover`) is what the feature
replaces; leaving is proven single-node in H-9.

Keep tags and the number of simultaneously attached tenants small in scenarios
that run before the membership fix lands: until then tenants and tags ride in
the 512-byte memberlist metadata (R§12); one UUID tenant and one short tag cost
about 70 bytes.

- [ ] **Step 1: Write the failing test**

```go
package multinode

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
)

// stubNoComputeClients is a MultiNodeFixture without ComputeClientCapable.
type stubNoComputeClients struct{}

func (stubNoComputeClients) BaseURLs() []string                     { return []string{"http://a", "http://b"} }
func (stubNoComputeClients) NodeCount() int                         { return 2 }
func (stubNoComputeClients) NewTenant(*testing.T) parity.Tenant     { return parity.Tenant{ID: "t"} }
func (stubNoComputeClients) ComputeTenant(*testing.T) parity.Tenant { return parity.Tenant{ID: "t"} }

var _ MultiNodeFixture = stubNoComputeClients{}

func TestComputeClientScenarioSkipsWithoutCapability(t *testing.T) {
	skipped := t.Run("ComputeClient_AttachToNode", func(t *testing.T) {
		RunComputeClient_AttachToNode(t, stubNoComputeClients{})
		t.Fatal("scenario did not skip on a fixture without ComputeClientCapable")
	})
	if !skipped {
		t.Error("ComputeClient_AttachToNode must t.Skip when the fixture cannot start compute clients")
	}
}

func TestComputeClientScenarioRegistered(t *testing.T) {
	for _, nt := range AllTests() {
		if nt.Name == "ComputeClient_AttachToNode" {
			return
		}
	}
	t.Error("ComputeClient_AttachToNode is not in the multinode registry")
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./e2e/parity/multinode/ -run 'TestComputeClientScenario'`
Expected: FAIL (build) with `undefined: RunComputeClient_AttachToNode`.

- [ ] **Step 3: Implement**

`e2e/parity/multinode/compute_client.go`:

```go
package multinode

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func init() {
	Register(NamedTest{Name: "ComputeClient_AttachToNode", Fn: RunComputeClient_AttachToNode})
}

// ComputeClientCapable is the OPTIONAL capability a cluster fixture implements
// to start a further compute client attached to a chosen pnode. Like
// AttributionCapable it is not folded into MultiNodeFixture: a cluster-capable
// backend that has not wired it still consumes the shared registry, and the
// scenarios that need it skip there.
type ComputeClientCapable interface {
	// StartComputeClient starts a compute client attached to pnode node (its
	// gRPC endpoint for the stream, its HTTP base for callbacks) and returns
	// once it has joined. Implementations MUST call t.Helper() and t.Fatal on
	// failure, including an out-of-range node.
	StartComputeClient(t *testing.T, node int, spec parity.ComputeClientSpec) parity.ComputeClient
}

// StartComputeClientOrSkip starts a compute client on pnode node for this
// scenario, or skips the scenario when the fixture lacks the capability. The
// client is stopped when the scenario ends.
func StartComputeClientOrSkip(t *testing.T, fixture MultiNodeFixture, node int, spec parity.ComputeClientSpec) parity.ComputeClient {
	t.Helper()
	cap, ok := fixture.(ComputeClientCapable)
	if !ok {
		t.Skip("cluster fixture cannot start further compute clients; scenario pending on this backend")
	}
	cc := cap.StartComputeClient(t, node, spec)
	t.Cleanup(cc.Stop)
	return cc
}

// RunComputeClient_AttachToNode proves a compute client started on one pnode
// is given work that arrives at another pnode and work that arrives at its own.
func RunComputeClient_AttachToNode(t *testing.T, fixture MultiNodeFixture) {
	if _, ok := fixture.(ComputeClientCapable); !ok {
		t.Skip("cluster fixture cannot start further compute clients; scenario pending on this backend")
	}
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("needs at least 2 pnodes, got %d", len(urls))
	}
	tenant := fixture.NewTenant(t)
	const model, tag = "h-mn-cc-attach", "h-mn-cc"
	host := len(urls) - 1

	cbRouteSetupModel(t, client.NewClient(urls[0], tenant.Token), model, cbRouteSampleNoWriteback,
		parity.ComputeClientWorkflow("h-mn-cc-wf", "noop", tag, "", nil))

	cc := StartComputeClientOrSkip(t, fixture, host, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}})

	// Arrives at pnode 0, which has no compute client for this tenant: the
	// work crosses to the hosting pnode. The wait for the tag to reach pnode 0
	// is the server's own (CYODA_DISPATCH_WAIT_TIMEOUT).
	if _, err := client.NewClient(urls[0], tenant.Token).CreateEntity(t, model, 1, `{"name":"Test","amount":10,"status":"new"}`); err != nil {
		t.Fatalf("create via pnode 0 (work crosses to pnode %d): %v", host, err)
	}
	if got := parity.AwaitReceived(t, cc, 1, 5*time.Second); got[0].Name != "noop" || !got[0].PassPresent {
		t.Errorf("record = %+v", got[0])
	}

	// Arrives at the hosting pnode itself: served locally.
	if _, err := client.NewClient(urls[host], tenant.Token).CreateEntity(t, model, 1, `{"name":"Test","amount":10,"status":"new"}`); err != nil {
		t.Fatalf("create via the hosting pnode %d: %v", host, err)
	}
	if got := parity.AwaitReceived(t, cc, 2, 5*time.Second); got[1].Seq != 2 {
		t.Errorf("second record = %+v", got[1])
	}
}
```

`postgres/multinode_fixture.go` — `pgMultiNode` gains two fields and the method; the literal sets them:

```go
	// computeBin and grpcEndpoints back the optional ComputeClientCapable
	// capability: a further compute client attached to a chosen pnode.
	computeBin    string
	grpcEndpoints []string
```

```go
var _ multinode.ComputeClientCapable = (*pgMultiNode)(nil)

// StartComputeClient implements multinode.ComputeClientCapable.
func (f *pgMultiNode) StartComputeClient(t *testing.T, node int, spec parity.ComputeClientSpec) parity.ComputeClient {
	t.Helper()
	if node < 0 || node >= len(f.baseURLs) || node >= len(f.grpcEndpoints) {
		t.Fatalf("StartComputeClient: pnode %d out of range (cluster has %d)", node, len(f.baseURLs))
	}
	return fixtureutil.StartComputeClientForFixture(t, f.keySet, f.computeBin, f.grpcEndpoints[node], f.baseURLs[node], spec)
}
```

and in the returned literal: `computeBin: result.ComputeBin, grpcEndpoints: result.GRPCEndpoints,`.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./e2e/parity/multinode/...` (unit), then `go test ./e2e/parity/postgres/ -run 'TestMultiNode'` (Docker; three pnodes) — `ComputeClient_AttachToNode` must be PASS, not SKIP.

- [ ] **Step 5: Commit**

```
git add e2e/parity/multinode/compute_client.go e2e/parity/multinode/compute_client_skip_test.go e2e/parity/postgres/multinode_fixture.go
git commit -m "test(parity): start a compute client on a chosen pnode of the cluster fixture (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task H-11: parity fixtures — a short patience for the whole package, stated once

**Spec:** §13 "What each harness can do" (third bullet); R§7 ("a fixture-wide constant, in lockstep across `e2e/parity/{memory,sqlite,postgres}/fixture.go` and the multinode fixture — precedent `CYODA_SCHEDULER_SCAN_INTERVAL=50ms`").

**Files:**
- Create: `e2e/parity/fixtureutil/tuned_env.go`
- Modify: `e2e/parity/memory/fixture.go` (:61-69), `e2e/parity/sqlite/fixture.go` (:82-92), `e2e/parity/postgres/fixture.go` (:95-105), `e2e/parity/postgres/multinode_fixture.go` (:166-170)
- Test: `e2e/parity/fixtureutil/tuned_env_test.go` (new)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func fixtureutil.TunedServerEnv() []string` — `CYODA_SCHEDULER_SCAN_INTERVAL=50ms`, `CYODA_DISPATCH_WAIT_TIMEOUT=200ms`
  - `func fixtureutil.TunedClusterEnv() []string` — `CYODA_DISPATCH_WAIT_TIMEOUT=2s`
  - A scenario may rely on: single-node patience 200 ms; cluster patience 2 s.

**Lockstep is made structural.** The precedent is the same comment and literal
pasted into three files. Here the values live in one function and a test reads
the four fixture files to prove each calls it and none spells the variables
out itself, so a fifth fixture cannot drift silently.

**Why two values.** On a single pnode a compute client is in the registry
before its greet goes out, so nothing needs waiting for; 200 ms only bounds
what "no compute node" costs. In a cluster the patience is also what absorbs
the gossip delay between a compute client joining one pnode and another pnode
learning its tag (H-10's scenario depends on it); 2 s leaves room for that and
is still well under today's 5 s. Today the variable is read only in cluster
mode (`app/config.go:411` → `ClusterDispatcher`), so on the single-node
fixtures this task changes nothing observable until the owner's loop lands —
which is why its test is about the fixtures, not the server.

- [ ] **Step 1: Write the failing test**

```go
package fixtureutil_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

func envValue(t *testing.T, env []string, key string) string {
	t.Helper()
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	t.Fatalf("%s is not set in %v", key, env)
	return ""
}

func TestTunedEnv_PatienceIsShortAndValid(t *testing.T) {
	single, err := time.ParseDuration(envValue(t, fixtureutil.TunedServerEnv(), "CYODA_DISPATCH_WAIT_TIMEOUT"))
	if err != nil || single <= 0 || single > 500*time.Millisecond {
		t.Errorf("single-node patience = %v (err %v); want within (0, 500ms]", single, err)
	}
	cluster, err := time.ParseDuration(envValue(t, fixtureutil.TunedClusterEnv(), "CYODA_DISPATCH_WAIT_TIMEOUT"))
	if err != nil || cluster < time.Second || cluster >= 5*time.Second {
		t.Errorf("cluster patience = %v (err %v); want within [1s, 5s): room for gossip, below the default", cluster, err)
	}
	if scan, err := time.ParseDuration(envValue(t, fixtureutil.TunedServerEnv(), "CYODA_SCHEDULER_SCAN_INTERVAL")); err != nil || scan != 50*time.Millisecond {
		t.Errorf("scan interval = %v (err %v); want 50ms", scan, err)
	}
}

// TestFixturesTakeTheirTuningFromOnePlace keeps the in-tree fixtures in
// lockstep: each calls the shared function, none spells a tuned variable out.
func TestFixturesTakeTheirTuningFromOnePlace(t *testing.T) {
	root := fixtureutil.FindModuleRoot()
	cases := []struct{ file, call string }{
		{"e2e/parity/memory/fixture.go", "fixtureutil.TunedServerEnv()"},
		{"e2e/parity/sqlite/fixture.go", "fixtureutil.TunedServerEnv()"},
		{"e2e/parity/postgres/fixture.go", "fixtureutil.TunedServerEnv()"},
		{"e2e/parity/postgres/multinode_fixture.go", "fixtureutil.TunedClusterEnv()"},
	}
	for _, tc := range cases {
		raw, err := os.ReadFile(filepath.Join(root, tc.file))
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		src := string(raw)
		if !strings.Contains(src, tc.call) {
			t.Errorf("%s does not call %s", tc.file, tc.call)
		}
		for _, literal := range []string{"CYODA_SCHEDULER_SCAN_INTERVAL", "CYODA_DISPATCH_WAIT_TIMEOUT"} {
			if strings.Contains(src, literal) {
				t.Errorf("%s spells out %s; it belongs in fixtureutil/tuned_env.go only", tc.file, literal)
			}
		}
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./e2e/parity/fixtureutil/ -run 'TestTunedEnv|TestFixturesTakeTheirTuning'`
Expected: FAIL (build) with `undefined: fixtureutil.TunedServerEnv`; once the function exists and before the fixtures change, `TestFixturesTakeTheirTuningFromOnePlace` FAILS with "does not call fixtureutil.TunedServerEnv()" and "spells out CYODA_SCHEDULER_SCAN_INTERVAL".

- [ ] **Step 3: Implement**

`e2e/parity/fixtureutil/tuned_env.go`:

```go
package fixtureutil

// TunedServerEnv returns the server settings every single-node parity fixture
// runs with, so the shared scenario set stays fast. Every fixture — in-tree
// and out-of-tree — appends it to its backend env; the values live here only.
//
//   - CYODA_SCHEDULER_SCAN_INTERVAL=50ms (default 1s): the scheduled-transition
//     scenarios observe fires within a small poll window. Harmless to every
//     other scenario — an empty scan is a cheap no-op query.
//   - CYODA_DISPATCH_WAIT_TIMEOUT=200ms (default 5s): how long a callout waits
//     for a compute node to exist. On a single node nothing needs waiting for
//     — a compute client is registered before it is told so — and the
//     scenarios that end with "no compute node" would otherwise each cost the
//     full default on every backend. Scenarios must not depend on a compute
//     client appearing within this window.
func TunedServerEnv() []string {
	return []string{
		"CYODA_SCHEDULER_SCAN_INTERVAL=50ms",
		"CYODA_DISPATCH_WAIT_TIMEOUT=200ms",
	}
}

// TunedClusterEnv returns the settings every cluster parity fixture adds per
// node. The wait is longer than on a single node because in a cluster it also
// covers the gossip delay between a compute client joining one node and the
// others learning its tags.
func TunedClusterEnv() []string {
	return []string{"CYODA_DISPATCH_WAIT_TIMEOUT=2s"}
}
```

`memory/fixture.go` `setup()`:

```go
	result, cleanup, err := fixtureutil.LaunchCyodaAndCompute(ks,
		append([]string{"CYODA_STORAGE_BACKEND=memory"}, fixtureutil.TunedServerEnv()...))
```

(the eight comment lines about the scan interval go; the explanation now lives
on `TunedServerEnv`). `sqlite/fixture.go`:

```go
	result, processCleanup, err := fixtureutil.LaunchCyodaAndCompute(ks, append([]string{
		"CYODA_STORAGE_BACKEND=sqlite",
		"CYODA_SQLITE_PATH=" + dbPath,
		"CYODA_SQLITE_AUTO_MIGRATE=true",
	}, fixtureutil.TunedServerEnv()...))
```

`postgres/fixture.go`:

```go
	result, processCleanup, err := fixtureutil.LaunchCyodaAndCompute(ks, append([]string{
		"CYODA_STORAGE_BACKEND=postgres",
		fmt.Sprintf("CYODA_POSTGRES_URL=%s", connStr),
		"CYODA_POSTGRES_AUTO_MIGRATE=true",
	}, fixtureutil.TunedServerEnv()...))
```

`postgres/multinode_fixture.go`:

```go
	launchEnv := append([]string{
		"CYODA_STORAGE_BACKEND=postgres",
		fmt.Sprintf("CYODA_POSTGRES_URL=%s", connStr),
		"CYODA_POSTGRES_AUTO_MIGRATE=true",
	}, fixtureutil.TunedClusterEnv()...)
	// extraEnv comes last so a caller can override any of the above.
	launchEnv = append(launchEnv, extraEnv...)
```

Two comments elsewhere point at the old location and are corrected:
`e2e/parity/scheduledtransition/scheduledtransition.go:73` and
`e2e/parity/scheduledfunction/scheduledfunction.go:80` say the fixtures set the
scan interval "see each backend's fixture.go" — change to "see
fixtureutil.TunedServerEnv".

Exit check: `grep -rn 'CYODA_SCHEDULER_SCAN_INTERVAL=\|CYODA_DISPATCH_WAIT_TIMEOUT=' e2e/` → hits in `e2e/parity/fixtureutil/tuned_env.go` only.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./e2e/parity/fixtureutil/...`, then `go test ./e2e/parity/memory/... ./e2e/parity/sqlite/... ./e2e/parity/postgres/...` (Docker for postgres; `TestMultiNode` runs with the 2 s patience — the cross-pnode scenarios are the ones to watch).

- [ ] **Step 5: Commit**

```
git add e2e/parity/fixtureutil/tuned_env.go e2e/parity/fixtureutil/tuned_env_test.go e2e/parity/memory/fixture.go \
  e2e/parity/sqlite/fixture.go e2e/parity/postgres/fixture.go e2e/parity/postgres/multinode_fixture.go \
  e2e/parity/scheduledtransition/scheduledtransition.go e2e/parity/scheduledfunction/scheduledfunction.go
git commit -m "test(parity): short callout patience for every fixture, stated once (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## Gate 4 for this stream

Nothing user-facing changes: no server env var, error code, help topic, README
or OpenAPI is touched. The two new variables are test-only and carry the
`CYODA_TEST_` prefix that `TestConfig_EnvVarCoverage` exempts; they are
documented where their reader is — the `cmd/compute-test-client` package
comment (H-6, H-7) — and on `parity.ComputeClientSpec`. `CHANGELOG.md` gets no
entry from this stream. Courtesy note for the commercial backend's suite, to be
passed on by whoever lands the plan: its fixture keeps compiling and the new
scenarios skip there; to run them it adds a `computeBin` field, implements
`StartComputeClient` via `fixtureutil.StartComputeClientForFixture`, and appends
`fixtureutil.TunedServerEnv()` to its launch env.

## Stream interface summary

**Consumed from other streams:** nothing. Stream H can be executed first, in
the order H-1 … H-11 (H-1→H-4 and H-5→H-11 are independent chains and may run
concurrently in separate worktrees; H-11 touches the fixture files H-9/H-10
also touch, so it goes after them).

**Produced — `internal/e2e` (package `e2e_test`), for the E and G rows:**
- `newCalloutHarness(t *testing.T, configure func(*app.Config)) *callbackHarness` — stack with no cnode; `configure` sets patience / tries / answer-limit on `app.Config`
- `(h) AttachCnode(t, cnodeSpec{name, tags, bearer, script}) *scriptedCnode`; `(c) Detach(t)`; `(c) AwaitGone(t)`; `(c) MemberID() string`; `(c) Received() []receivedCallout`
- `cnodeScript func(ctx context.Context, call receivedCallout, rc *reqCtx) cnodeReply`; `scriptAlways(r)`; `scriptSequence(r...)`; `scriptLateCallback(release <-chan struct{}, callback func(rc *reqCtx), then cnodeReply)`
- replies: `answerOK()`, `answerData(map[string]any)`, `answerMatches(bool)`, `answerResult(kind string, result map[string]any)`, `answerFail(msg)`, `answerFailVerdict(msg string, retryable bool)`, `neverAnswer()`, `closeStream()`
- record: `receivedCallout{Seq, Cnode, MemberID, Kind, Name, RequestID, EventID, EntityID}` + `Pass()`; `(h) ReceivedCallouts()`; `(h) AwaitCallouts(t, n, within)`
- replay: `(h) ReplayCreateHTTP(pass, entityName, version, payload) (callbackResult, error)`, `ReplayGetHTTP(pass, entityID)`, `ReplayCreateGRPC(pass, model, version, payload) (txEnvelope, error)`, `ReplayGetGRPC(pass, entityID)`
- `procWorkflowJSON(wfName, procName, mode string, config map[string]any) string`; `h.apiConn`; `calloutProcessor|calloutCriterion|calloutFunction`
- already there and useful: `h.provisionTenant`, `h.fetchTokenFor` (a second tenant's bearer for `cnodeSpec.bearer` — the "two tenants share a tag" and "stolen pass" rows), `h.CreateEntityRaw` (drive an operation from a goroutine), `problemErrorCode`

How the rows map: "same request id on every try" → compare `RequestID`/`EventID` across `ReceivedCallouts()`; "which cnode was tried first" / "round robin; a new cnode goes first" → `Seq` + `Cnode`; "no cnode → waits, one attaches → succeeds" → `CreateEntityRaw` in a goroutine, then `AttachCnode`; "cnode drops after hand-off" → `closeStream()`; "`NoAnswer`" → `neverAnswer()` with a small `responseTimeoutMs`; "`MemberFailed` verdict true / false / absent" → `answerFailVerdict` / `answerFail`; late-callback rows → `scriptLateCallback` (in progress) or `Replay*` (after the end); "a callback in progress when its cnode is replaced" → a script that blocks inside `rc.CreateEntity`… while a second cnode is scripted to answer.

**Produced — `cmd/compute-test-client`:** env `CYODA_TEST_COMPUTE_TAGS`, `CYODA_TEST_COMPUTE_BEHAVIOUR` (`stall|fail|fail-retryable|late-callback|drop`); catalog names `inject-error-retryable`, `inject-error-not-retryable`, `inject-criterion-error-retryable`, `inject-fn-error-retryable` (usable with the fixture's own client, no extra process); control surface `GET /record`, `POST /release`.

**Produced — parity (P and M rows):**
- `parity.ComputeClientSpec{TenantID string; Tags []string; Behaviour string}`; `parity.ComputeBehaviour{Catalog,Stall,Fail,FailRetryable,LateCallback,Drop}`
- `parity.ComputeClient{ MemberID() string; Received(t) []ReceivedCallout; Release(t) []ReceivedCallout; Stop() }`; `parity.ReceivedCallout`, `parity.LateCallbackOutcome`
- `parity.StartComputeClientOrSkip(t, fixture BackendFixture, spec) ComputeClient`; `parity.AwaitReceived(t, cc, n, within)`; `parity.ComputeClientWorkflow(wfName, procName, tag, contextValue string, extraConfig map[string]any) string`
- `multinode.StartComputeClientOrSkip(t, fixture MultiNodeFixture, node int, spec) parity.ComputeClient`
- `fixtureutil.TunedServerEnv()` (patience 200 ms), `fixtureutil.TunedClusterEnv()` (patience 2 s)
- Un-skipping 09_09/10/11 needs no count bump (they are registered already); H-9 bumps `wantParityScenarioCount` 275 → 277 for its own two.

## Open points

1. **Fresh tenant, not only "a tag of its own".** Spec §13 isolates each
   failover scenario by tag. A tag is not enough today: a callout with empty
   `calculationNodesTags` matches every cnode of its tenant
   (`internal/common/tags.go:8-10`), seven parity scenarios use the empty tag
   under the shared `system-tenant`, and a stopped client is evicted
   asynchronously — so a stalling client under `system-tenant` could be handed
   the next scenario's work. `ComputeClientSpec.TenantID` is therefore required
   and scenarios are told to use `fixture.NewTenant`. It is also what the M row
   "two tenants share a tag" needs. This departs from the letter of §13, not
   its intent.
2. **Two patience values, not one.** The assignment asks for one low value in
   lockstep across all fixtures. In a cluster the patience is what absorbs the
   gossip delay before another pnode knows a new client's tag, so the cluster
   fixture gets 2 s and the single-node fixtures 200 ms (H-11). If Paul prefers
   zero added flake risk on `TestMultiNode` (a known-flaky entry), leave the
   cluster at the 5 s default: delete `TunedClusterEnv` and its fixture line;
   nothing else in this stream depends on it.
3. **`processorFunc` signature not changed.** R§7 and the assignment say the
   signature "cannot express `retryable`". H-5 expresses the verdict through a
   typed error instead, leaving all five catalog signatures alone. Same
   capability, no churn in ~25 entries.
4. **No `fail-not-retryable` behaviour.** §13 lists five behaviours; the row
   "`MemberFailed` verdict false / absent" is served in P by `fail` (absent) and
   by the catalog's `inject-error-not-retryable` with the fixture's own client
   (false). An explicit-false *extra* client is not offered; add the constant
   if a scenario turns out to need one.
5. **What a late callback is answered with after a rolled-back callout.** §7
   orders the entry checks verify → `txMgr.Join` → `Fence.Admit`. When the
   callout ended because its operation failed, the transaction is gone and
   `Join` answers 404 `TRANSACTION_NOT_FOUND` before `Admit` is reached — so
   §8.2's 410 `CALLOUT_SUPERSEDED` is observable only while the transaction is
   still open (a later processor running, `ASYNC_NEW_TX`, a replaced cnode).
   The harness self-tests assert "refused" only; the fencing stream should say
   which of its E rows are 410 and which stay 404.
6. **CloudEvent id ≠ request id today** (`internal/grpc/cloudevent.go:24` mints
   a fresh uuid). §4 wants the request id in both; the harness records both
   (`RequestID`, `EventID`) so the stream that changes it can assert equality.
7. **The default cnode's join no longer names the tenant** (H-2 sends
   `joinedLegalEntityId: ""`). The server derives the tenant from the bearer and
   only cross-checks a non-empty value (`streaming.go:63-66`), so nothing is
   lost; the mismatch branch keeps its existing coverage in `internal/grpc/streaming_test.go`.
