package callout

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// The router the wiring passes is the cluster's own.
var _ PeerRouter = (*dispatch.PeerRouter)(nil)

// handOver is one call the owner made to the router.
type handOver struct {
	peer      string
	requestID string
	triesLeft int
	major     uint32
	wait      time.Duration // the deadline on ctx, from the moment of the call
}

// scriptedRouter stands for the other pnodes: which exist, and what each
// answers, in order. A peer with no answer left cannot be connected to.
type scriptedRouter struct {
	mu      sync.Mutex
	peers   []string
	answers map[string][]func(ctx context.Context) dispatch.HandOverAnswer
	calls   []handOver
	changed chan struct{}
}

func newScriptedRouter(peers ...string) *scriptedRouter {
	return &scriptedRouter{peers: peers, answers: map[string][]func(context.Context) dispatch.HandOverAnswer{}, changed: make(chan struct{})}
}

func (r *scriptedRouter) script(peer string, answers ...func(context.Context) dispatch.HandOverAnswer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers[peer] = append(r.answers[peer], answers...)
}

func (r *scriptedRouter) Peers(string, string) []contract.NodeInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]contract.NodeInfo, 0, len(r.peers))
	for _, id := range r.peers {
		out = append(out, contract.NodeInfo{NodeID: id, Alive: true})
	}
	return out
}

func (r *scriptedRouter) Changed() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.changed
}

// fire signals a change in the cluster's membership.
func (r *scriptedRouter) fire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *scriptedRouter) HandOver(ctx context.Context, peer contract.NodeInfo, call internalgrpc.Callout, triesLeft int, major uint32) dispatch.HandOverAnswer {
	deadline, _ := ctx.Deadline()
	next := func() func(context.Context) dispatch.HandOverAnswer {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, handOver{peer: peer.NodeID, requestID: call.RequestID, triesLeft: triesLeft, major: major, wait: time.Until(deadline)})
		queue := r.answers[peer.NodeID]
		if len(queue) == 0 {
			return nil
		}
		r.answers[peer.NodeID] = queue[1:]
		return queue[0]
	}()
	if next == nil {
		return dispatch.HandOverAnswer{} // could not be connected to
	}
	return next(ctx)
}

func (r *scriptedRouter) made() []handOver {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]handOver(nil), r.calls...)
}

func peerAnswers(by string) func(context.Context) dispatch.HandOverAnswer {
	return func(context.Context) dispatch.HandOverAnswer {
		return dispatch.HandOverAnswer{Connected: true, TriesUsed: 1, Result: &internalgrpc.CalloutResult{
			Function: contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"by":"` + by + `"}`)},
		}}
	}
}

func peerFails(failure *contract.CalloutFailure, triesUsed int, attempts ...contract.CalloutAttempt) func(context.Context) dispatch.HandOverAnswer {
	return func(context.Context) dispatch.HandOverAnswer {
		return dispatch.HandOverAnswer{Connected: true, TriesUsed: triesUsed, Failure: failure, Attempts: attempts}
	}
}

// lostAnswer is what the router reports when the peer was connected to and no
// usable answer came back: one try, NoAnswer, DISPATCH_FORWARD_FAILED, no cnode
// known.
func lostAnswer() *contract.CalloutFailure {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchForwardFailed,
		"forwarding the callout to a peer node failed").AsRetryable()
	return &contract.CalloutFailure{Kind: contract.NoAnswer, Code: appErr.Code, Message: appErr.Message, Err: appErr}
}

// hangs is a peer that was connected to and never answers: the router gives up
// when the owner's wait on ctx runs out.
func hangs() func(context.Context) dispatch.HandOverAnswer {
	return func(ctx context.Context) dispatch.HandOverAnswer {
		<-ctx.Done()
		return dispatch.HandOverAnswer{Connected: true, TriesUsed: 1, Failure: lostAnswer()}
	}
}

// dialFailsAfter is a peer whose connection takes d to fail: nothing was handed
// over, no try is used, and the callout is that much older.
func dialFailsAfter(d time.Duration) func(context.Context) dispatch.HandOverAnswer {
	return func(ctx context.Context) dispatch.HandOverAnswer {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		return dispatch.HandOverAnswer{}
	}
}

// dialNeverCompletes is a peer whose connection is still being opened when the
// owner gives up on it: the dial ends with the owner's wait, so nothing was
// handed over and no try is used.
func dialNeverCompletes() func(context.Context) dispatch.HandOverAnswer {
	return dialFailsAfter(time.Minute)
}

// provedUnhandoverable is what the router reports for a hand-over this pnode
// cannot make at all — a request it cannot marshal or sign. Nothing was sent
// and no try was used, and it would fail identically for every peer: Terminal.
func provedUnhandoverable() func(context.Context) dispatch.HandOverAnswer {
	return func(context.Context) dispatch.HandOverAnswer {
		appErr := common.Internal("the callout could not be handed over", nil)
		return dispatch.HandOverAnswer{Failure: &contract.CalloutFailure{
			Kind: contract.Terminal, Code: appErr.Code, Message: appErr.Message, Err: appErr,
		}}
	}
}

func newClusterEnv(t *testing.T, cfg Config, router PeerRouter) *env {
	t.Helper()
	e := newEnv(t, cfg)
	e.owner = New(e.owner.local, e.reg, router, e.fence, common.NewTestUUIDGenerator(), e.owner.cfg)
	return e
}

func TestOwner_OwnersCnodeFails_HandOverSucceeds(t *testing.T) {
	router := newScriptedRouter("p-1")
	router.script("p-1", peerAnswers("cnode-on-p-1"))
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)
	own := e.attach(t, "m-1", tenantA, "x", detaches())

	by, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if err != nil || by != "cnode-on-p-1" {
		t.Fatalf("answered by %q, err %v; want the peer's cnode", by, err)
	}
	calls := router.made()
	if len(calls) != 1 {
		t.Fatalf("hand-overs = %+v, want one", calls)
	}
	ids, _ := own.seen()
	if calls[0].requestID != ids[0] {
		t.Errorf("hand-over carries request id %q, the local try had %q: one id for the whole callout", calls[0].requestID, ids[0])
	}
	if calls[0].triesLeft != 3 {
		t.Errorf("triesLeft = %d, want the 3 tries left of 4", calls[0].triesLeft)
	}
	if calls[0].major != 2 {
		t.Errorf("major = %d, want 2: the hand-over draws from the counter the local try drew 1 from", calls[0].major)
	}
	// 3 tries × 60 ms + the allowance
	if want := 3*limitMs*time.Millisecond + time.Second; calls[0].wait > want || calls[0].wait < want-500*time.Millisecond {
		t.Errorf("owner's wait = %v, want about %v", calls[0].wait, want)
	}
}

func TestOwner_LocalCnodeAnswers_NoHandOver(t *testing.T) {
	router := newScriptedRouter("p-1")
	router.script("p-1", peerAnswers("cnode-on-p-1"))
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)
	e.attach(t, "m-1", tenantA, "x", answers("m-1"))

	by, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if err != nil || by != "m-1" {
		t.Fatalf("answered by %q, err %v; want the owner's own cnode", by, err)
	}
	if calls := router.made(); len(calls) != 0 {
		t.Errorf("hand-overs = %+v, want none: the owner runs the local procedure first", calls)
	}
}

// A caller that goes away during a hand-over ends the callout with its own
// context error — not with a retryable 503, and not by asking the next peer.
func TestOwner_CallerGoesAwayDuringAHandOver_CtxErrUnchanged(t *testing.T) {
	router := newScriptedRouter("p-1", "p-2")
	router.script("p-1", hangs())
	router.script("p-2", peerAnswers("cnode-on-p-2"))
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: 30 * time.Second}, router)
	ctx, cancel := context.WithCancel(userCtx(tenantA))
	defer cancel()
	time.AfterFunc(30*time.Millisecond, cancel)

	_, err := e.dispatchFunction(ctx, "x", "")

	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled unchanged", err)
	}
	if calls := router.made(); len(calls) != 1 {
		t.Errorf("hand-overs = %+v, want p-2 never asked", calls)
	}
}

// Three reasons a callout's context can end, three outcomes — here with the
// context ending while a hand-over is in progress. The same three during a try
// and during a wait are in coordinator_test.go and patience_test.go, except the
// callout's own deadline: during a try it is
// TestOwner_CalloutDeadline_CutsOffATryInProgress, during a wait
// TestOwner_CalloutDeadline_DuringAWait_FallsToThePrecedence.
func TestOwner_WhoseContextEndedDuringAHandOver(t *testing.T) {
	tests := []struct {
		name  string
		cfg   Config
		ctx   func(t *testing.T, e *env) context.Context
		check func(t *testing.T, err error)
	}{
		{
			name: "the caller went away: its own error, unchanged",
			cfg:  Config{FixedNumRetries: 1, HandoverAllowance: 30 * time.Second},
			ctx: func(t *testing.T, _ *env) context.Context {
				ctx, cancel := context.WithCancel(userCtx(tenantA))
				t.Cleanup(cancel)
				time.AfterFunc(40*time.Millisecond, cancel)
				return ctx
			},
			check: func(t *testing.T, err error) {
				if err != context.Canceled {
					t.Errorf("err = %v, want context.Canceled unchanged", err)
				}
			},
		},
		{
			name: "the fence released the callout: CALLOUT_SUPERSEDED",
			cfg:  Config{FixedNumRetries: 1, HandoverAllowance: 30 * time.Second},
			ctx: func(t *testing.T, e *env) context.Context {
				_, endOuter := e.fence.Begin(context.Background(), "outer-callout", "tx-1", nil)
				t.Cleanup(endOuter)
				e.fence.Advance("outer-callout", 1)
				callback, err := e.fence.Admit(userCtx(tenantA), []fence.Pair{{Callout: "outer-callout", Major: 1}})
				if err != nil {
					t.Fatalf("Admit: %v", err)
				}
				time.AfterFunc(40*time.Millisecond, func() { e.fence.Advance("outer-callout", 2) })
				return callback
			},
			check: func(t *testing.T, err error) {
				if !errors.Is(err, fence.ErrSuperseded) {
					t.Fatalf("err = %v, want fence.ErrSuperseded", err)
				}
				if got := appErrOf(t, err).Code; got != common.ErrCodeCalloutSuperseded {
					t.Errorf("code = %s, want CALLOUT_SUPERSEDED", got)
				}
			},
		},
		{
			// 2 tries of 60 ms and 50 ms of allowance: the callout may take
			// 170 ms, which is also the whole of the owner's wait for the first
			// hand-over. The hand-over's answer is therefore lost to the
			// callout's own deadline, and "nothing more can be done" is
			// reported as what was on record — never as a bare deadline.
			name: "the callout's own deadline passed: the attempt on record",
			cfg:  Config{FixedNumRetries: 1, HandoverAllowance: 50 * time.Millisecond},
			ctx:  func(*testing.T, *env) context.Context { return userCtx(tenantA) },
			check: func(t *testing.T, err error) {
				if errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("err = %v: the callout's own deadline must never be returned as one", err)
				}
				appErr := appErrOf(t, err)
				if appErr.Status != 503 || appErr.Code != common.ErrCodeDispatchForwardFailed || !appErr.Retryable {
					t.Errorf("got %d %s, want a retryable 503 DISPATCH_FORWARD_FAILED", appErr.Status, appErr.Code)
				}
				var failure *contract.CalloutFailure
				if !errors.As(err, &failure) || len(failure.Attempts) != 1 || failure.Attempts[0].MemberID != "-" {
					t.Errorf("attempts = %+v, want the lost hand-over's one attempt, under member -", failure)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := newScriptedRouter("p-1", "p-2")
			router.script("p-1", hangs())
			router.script("p-2", peerAnswers("cnode-on-p-2"))
			e := newClusterEnv(t, tt.cfg, router)

			_, err := e.dispatchFunction(tt.ctx(t, e), "x", "")

			tt.check(t, err)
			if calls := router.made(); len(calls) != 1 {
				t.Errorf("hand-overs = %+v, want p-2 never asked: the callout had ended", calls)
			}
		})
	}
}

// The callout's deadline passing during a wait is not a bare deadline either:
// nothing was ever tried, so it is "no compute member".
func TestOwner_CalloutDeadline_DuringAWait_FallsToThePrecedence(t *testing.T) {
	// One try of 60 ms, 400 ms of patience, 10 ms of allowance: the callout may
	// take 470 ms, and one hand-over 70 ms. Three peers the owner gives up on
	// dialling spend 210 ms of that without using a try, so the 400 ms of
	// patience that follow outlive the callout by 140 ms.
	peers := []string{"p-1", "p-2", "p-3"}
	router := newScriptedRouter(peers...)
	for _, peer := range peers {
		router.script(peer, dialNeverCompletes())
	}
	e := newClusterEnv(t, Config{Patience: 400 * time.Millisecond, HandoverAllowance: 10 * time.Millisecond}, router)
	start := time.Now()

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "NONE")

	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v: the callout's own deadline must never be returned as one", err)
	}
	if !errors.Is(err, contract.ErrNoMatchingMember) {
		t.Fatalf("err = %v, want ErrNoMatchingMember: no try was ever made", err)
	}
	if calls := router.made(); len(calls) != len(peers) {
		t.Errorf("hand-overs = %+v, want each peer asked once", calls)
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond || elapsed > 590*time.Millisecond {
		t.Errorf("the callout took %v, want it cut off at its 470 ms rather than at the patience", elapsed)
	}
}

func TestOwner_LocalNoAnswer_ProcessorNotIdempotent_IsNotHandedOver(t *testing.T) {
	router := newScriptedRouter("p-1")
	router.script("p-1", peerAnswers("cnode-on-p-1"))
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)
	e.attach(t, "m-1", tenantA, "x", nil)

	_, err := e.owner.DispatchProcessor(userCtx(tenantA), testEntity(), processorDef("x", "", false), "wf1", "t1", "tx-1")

	if got := appErrOf(t, err).Code; got != common.ErrCodeDispatchTimeout {
		t.Errorf("code = %s, want the try's own DISPATCH_TIMEOUT", got)
	}
	if calls := router.made(); len(calls) != 0 {
		t.Errorf("hand-overs = %+v, want none", calls)
	}
}

func TestOwner_PeerCannotBeConnectedTo_UsesNoTry_NextPeerIsAsked(t *testing.T) {
	router := newScriptedRouter("p-1", "p-2")
	router.script("p-2", peerAnswers("cnode-on-p-2")) // p-1 has no answer: not connected
	e := newClusterEnv(t, Config{FixedNumRetries: 0, HandoverAllowance: time.Second}, router)
	ctx, stats := contract.WithCalloutStats(userCtx(tenantA))

	by, err := e.dispatchFunction(ctx, "x", "") // one try in all

	if err != nil || by != "cnode-on-p-2" {
		t.Fatalf("answered by %q, err %v; want p-2's cnode", by, err)
	}
	calls := router.made()
	if len(calls) != 2 || calls[0].peer != "p-1" || calls[1].peer != "p-2" || calls[1].triesLeft != 1 {
		t.Errorf("hand-overs = %+v, want p-1 then p-2 with the one try still unused", calls)
	}
	// The number rises before every hand-over, the one that could not be made
	// included: shutting a cnode out that never got the work is harmless, and
	// the rule "the number rises before the work moves" has no exception.
	if len(calls) == 2 && (calls[0].major != 1 || calls[1].major != 2) {
		t.Errorf("majors = %d, %d, want 1 then 2", calls[0].major, calls[1].major)
	}
	if got := strings.Join(stats.HandOvers, ","); got != "unreachable,ok" {
		t.Errorf("HandOvers = %s, want unreachable,ok", got)
	}
}

// A hand-over this pnode proved it cannot make would fail identically for every
// peer: no try is used, and no other peer is asked.
func TestOwner_HandOverProvedImpossible_IsTerminal_NoOtherPeerIsAsked(t *testing.T) {
	router := newScriptedRouter("p-1", "p-2")
	router.script("p-1", provedUnhandoverable())
	router.script("p-2", peerAnswers("cnode-on-p-2"))
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if got := appErrOf(t, err).Status; got != http.StatusInternalServerError {
		t.Errorf("status = %d, want a ticketed 500", got)
	}
	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) || failure.Kind != contract.Terminal {
		t.Fatalf("failure = %+v, want Terminal", failure)
	}
	// Nothing was handed to a cnode, so there is no attempt to report: a
	// hand-over that used no try records none.
	if len(failure.Attempts) != 0 {
		t.Errorf("attempts = %+v, want none: no try was used", failure.Attempts)
	}
	if calls := router.made(); len(calls) != 1 {
		t.Errorf("hand-overs = %+v, want p-2 never asked", calls)
	}
}

func TestOwner_NoPeerCanBeConnectedTo_NoLocalCnode_IsNoComputeMember(t *testing.T) {
	router := newScriptedRouter("p-1", "p-2")
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if !errors.Is(err, contract.ErrNoMatchingMember) {
		t.Fatalf("err = %v, want ErrNoMatchingMember: no try was ever made", err)
	}
	if calls := router.made(); len(calls) != 2 {
		t.Errorf("hand-overs = %+v, want each peer asked once", calls)
	}
}

// The asked set belongs to one pass: after a change a peer that could not be
// connected to is asked again.
func TestOwner_ANewPassAsksEveryPeerAgain(t *testing.T) {
	router := newScriptedRouter("p-1")
	e := newClusterEnv(t, Config{FixedNumRetries: 3, Patience: 5 * time.Second, HandoverAllowance: time.Second}, router)
	time.AfterFunc(50*time.Millisecond, func() {
		router.script("p-1", peerAnswers("cnode-on-p-1"))
		router.fire() // p-1's list arrived
	})

	by, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if err != nil || by != "cnode-on-p-1" {
		t.Fatalf("answered by %q, err %v; want p-1's cnode on the second pass", by, err)
	}
	if calls := router.made(); len(calls) != 2 || calls[1].major != 2 {
		t.Errorf("hand-overs = %+v, want p-1 asked twice, the number rising each time", calls)
	}
}

func TestOwner_HandOverAnswerLost(t *testing.T) {
	t.Run("not repeat-safe: DISPATCH_FORWARD_FAILED, recorded under member -", func(t *testing.T) {
		router := newScriptedRouter("p-1", "p-2")
		router.script("p-1", peerFails(lostAnswer(), 1))
		router.script("p-2", peerAnswers("cnode-on-p-2"))
		e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)

		_, err := e.owner.DispatchProcessor(userCtx(tenantA), testEntity(), processorDef("x", "", false), "wf1", "t1", "tx-1")

		appErr := appErrOf(t, err)
		if appErr.Status != 503 || appErr.Code != common.ErrCodeDispatchForwardFailed || !appErr.Retryable {
			t.Errorf("got %d %s, want a retryable 503 DISPATCH_FORWARD_FAILED", appErr.Status, appErr.Code)
		}
		var failure *contract.CalloutFailure
		if !errors.As(err, &failure) || len(failure.Attempts) != 1 || failure.Attempts[0].MemberID != "-" {
			t.Errorf("attempts = %+v, want one, under member -", failure)
		}
		if calls := router.made(); len(calls) != 1 {
			t.Errorf("hand-overs = %+v: the work may have run, so no other pnode is asked", calls)
		}
	})
	t.Run("repeat-safe: one try counted, the next peer is asked", func(t *testing.T) {
		router := newScriptedRouter("p-1", "p-2")
		router.script("p-1", peerFails(lostAnswer(), 1))
		router.script("p-2", peerFails(lostAnswer(), 1))
		e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)

		_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

		calls := router.made()
		if len(calls) != 2 || calls[0].triesLeft != 4 || calls[1].triesLeft != 3 {
			t.Fatalf("hand-overs = %+v, want two, the second with one try fewer", calls)
		}
		want := "CALLOUT_FAILED: the callout could not be completed, got 2 failures: " +
			"[member<->: DISPATCH_FORWARD_FAILED: forwarding the callout to a peer node failed (2 times)]"
		if got := appErrOf(t, err).Message; got != want {
			t.Errorf("message\n got %s\nwant %s", got, want)
		}
	})
}

func TestOwner_PeersCnodeFailed_ItsMessageAndVerdictSurvive_NobodyElseIsAsked(t *testing.T) {
	yes := true
	router := newScriptedRouter("p-1", "p-2")
	router.script("p-1", peerFails(
		&contract.CalloutFailure{Kind: contract.MemberFailed, Message: "rates service is down", Retryable: &yes}, 2,
		contract.CalloutAttempt{MemberID: "r-1", Kind: contract.NoHandOff, Cause: "COMPUTE_MEMBER_DISCONNECTED: compute member disconnected during function dispatch"},
		contract.CalloutAttempt{MemberID: "r-2", Kind: contract.MemberFailed, Cause: "rates service is down"}))
	router.script("p-2", peerAnswers("cnode-on-p-2"))
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)
	ctx, stats := contract.WithCalloutStats(userCtx(tenantA))

	_, err := e.dispatchFunction(ctx, "x", "")

	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) || failure.Kind != contract.MemberFailed || failure.Message != "rates service is down" || failure.Retryable == nil || !*failure.Retryable {
		t.Fatalf("failure = %+v, want the peer's cnode's own message and verdict", failure)
	}
	if calls := router.made(); len(calls) != 1 {
		t.Errorf("hand-overs = %+v, want p-2 never asked", calls)
	}
	if got := strings.Join(stats.Tries, ","); got != "no_handoff,member_failed" {
		t.Errorf("Tries = %s, want the peer's two tries", got)
	}
}

// Whenever a pass records attempts it also records the failure they belong to:
// the error for "nothing more can be done" reads the latest failure that was
// actually made, and a pass that left it behind would be a nil dereference.
func TestOwner_AHandOverThatRecordsAnAttemptAlsoRecordsItsFailure(t *testing.T) {
	router := newScriptedRouter("p-1")
	router.script("p-1", peerFails(lostAnswer(), 1))
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)

	// Three tries are left and no peer is, so the callout stops with only the
	// lost hand-over on record.
	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if got := appErrOf(t, err).Code; got != common.ErrCodeDispatchForwardFailed {
		t.Errorf("code = %s, want the one attempt's own DISPATCH_FORWARD_FAILED", got)
	}
}

// A failure the owner returns owns its attempts: the peer's slice is the
// router's, and nothing the router does to it afterwards may change what the
// caller already holds.
func TestOwner_AFailureFromAPeerDoesNotShareThePeersAttempts(t *testing.T) {
	yes := true
	attempts := []contract.CalloutAttempt{{MemberID: "r-1", Kind: contract.MemberFailed, Cause: "rates service is down"}}
	router := newScriptedRouter("p-1")
	router.script("p-1", func(context.Context) dispatch.HandOverAnswer {
		return dispatch.HandOverAnswer{Connected: true, TriesUsed: 1, Attempts: attempts,
			Failure: &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "rates service is down", Retryable: &yes}}
	})
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) || len(failure.Attempts) != 1 {
		t.Fatalf("failure = %+v, want the one attempt on it", failure)
	}
	attempts[0].MemberID = "overwritten"
	if failure.Attempts[0].MemberID != "r-1" {
		t.Errorf("the returned failure changed under the caller: %+v", failure.Attempts)
	}
}

// The tries are a budget: every hand-over is granted the tries that remain,
// what it used is taken off, and the total never exceeds the setting.
func TestOwner_TheBudgetSpansLocalTriesAndPeers_AndIsNeverExceeded(t *testing.T) {
	router := newScriptedRouter("p-1", "p-2", "p-3")
	router.script("p-1", peerFails(lostAnswer(), 2))
	router.script("p-2", peerFails(lostAnswer(), 1))
	router.script("p-3", peerAnswers("never-asked"))
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)
	e.attach(t, "m-1", tenantA, "x", detaches()) // one local try

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	calls := router.made()
	if len(calls) != 2 || calls[0].triesLeft != 3 || calls[1].triesLeft != 1 {
		t.Fatalf("hand-overs = %+v, want p-1 with 3 tries, p-2 with 1, and p-3 never asked", calls)
	}
	for _, c := range calls {
		if c.triesLeft < 1 {
			t.Errorf("hand-over to %s was granted %d tries: a peer refuses a hand-over with none left", c.peer, c.triesLeft)
		}
	}
	// 1 local try + 2 on p-1 + 1 on p-2 = the 4 the setting allows. The two
	// lost answers are one attempt each, so three are listed.
	appErr := appErrOf(t, err)
	if appErr.Code != common.ErrCodeCalloutFailed || !strings.Contains(appErr.Message, "got 3 failures") {
		t.Errorf("error = %s, want CALLOUT_FAILED over the local try and the two lost answers", appErr.Message)
	}
}

// The wait the owner puts on a hand-over is derived from the callout's own
// context, so it can never outlive the callout's deadline.
func TestOwner_AHandOversWaitNeverOutlivesTheCalloutDeadline(t *testing.T) {
	// One try of 60 ms and 300 ms of allowance: the callout may take 360 ms,
	// and so may the first hand-over. p-1's connection spends 200 ms of it.
	router := newScriptedRouter("p-1", "p-2")
	router.script("p-1", dialFailsAfter(200*time.Millisecond))
	router.script("p-2", peerAnswers("cnode-on-p-2"))
	e := newClusterEnv(t, Config{HandoverAllowance: 300 * time.Millisecond}, router)

	by, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if err != nil || by != "cnode-on-p-2" {
		t.Fatalf("answered by %q, err %v; want p-2's cnode", by, err)
	}
	calls := router.made()
	if len(calls) != 2 {
		t.Fatalf("hand-overs = %+v, want two", calls)
	}
	if full := limitMs*time.Millisecond + 300*time.Millisecond; calls[1].wait >= full-150*time.Millisecond {
		t.Errorf("the second hand-over's wait = %v, want well under its own %v: the callout's deadline is nearer", calls[1].wait, full)
	}
}

// A Coordinator on a single pnode has no router: the loop must run without one,
// and nothing in it may dereference the absent peers.
func TestOwner_NoPeerRouter_TheLoopRuns(t *testing.T) {
	t.Run("a cnode answers", func(t *testing.T) {
		e := newEnv(t, Config{FixedNumRetries: 3})
		e.attach(t, "m-1", tenantA, "x", answers("m-1"))

		by, err := e.dispatchFunction(userCtx(tenantA), "x", "")

		if err != nil || by != "m-1" {
			t.Fatalf("answered by %q, err %v; want m-1", by, err)
		}
	})
	t.Run("no cnode, and the patience is waited out", func(t *testing.T) {
		e := newEnv(t, Config{FixedNumRetries: 3, Patience: 50 * time.Millisecond})

		_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

		if !errors.Is(err, contract.ErrNoMatchingMember) {
			t.Fatalf("err = %v, want ErrNoMatchingMember", err)
		}
	})
}

// The time a callout may take is a hard limit. A peer that hangs costs the
// owner its whole wait and one try; the try that follows is cut off at the
// callout's deadline, as NoAnswer.
func TestOwner_CalloutDeadline_CutsOffATryInProgress(t *testing.T) {
	// Two tries of 400 ms, 100 ms of patience, 100 ms of allowance: the callout
	// may take 2 × 400 + 100 + 100 = 1000 ms.
	const limit = 400
	router := newScriptedRouter("p-1")
	e := newClusterEnv(t, Config{FixedNumRetries: 1, Patience: 100 * time.Millisecond, HandoverAllowance: 100 * time.Millisecond}, router)
	router.script("p-1", func(ctx context.Context) dispatch.HandOverAnswer {
		answer := hangs()(ctx) // the owner's wait: 2 × 400 + 100 = 900 ms, one try
		// As the owner gives up on p-1, a cnode that will not answer attaches
		// locally. Its try starts at about 900 ms and has 400 ms to answer.
		e.attach(t, "m-late", tenantA, "x", nil)
		return answer
	})
	fn := functionDef("x", "")
	fn.ResponseTimeoutMs = limit
	start := time.Now()

	_, err := e.owner.DispatchFunction(userCtx(tenantA), testEntity(), fn, "wf1", "t1", "tx-1")

	elapsed := time.Since(start)
	if elapsed < 1000*time.Millisecond || elapsed > 1200*time.Millisecond {
		t.Errorf("the callout took %v, want it cut off at its 1000 ms — not run to 900 + 400", elapsed)
	}
	appErr := appErrOf(t, err)
	if appErr.Code != common.ErrCodeCalloutFailed || !strings.Contains(appErr.Message, "got 2 failures") {
		t.Fatalf("error = %s, want CALLOUT_FAILED with the lost hand-over and the try that was cut off", appErr.Message)
	}
	if !strings.Contains(appErr.Message, "member<m-late>: DISPATCH_TIMEOUT: function dispatch cut off at the callout deadline: no response") {
		t.Errorf("error = %s, want the cut-off try reported as such", appErr.Message)
	}
	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) || failure.Kind != contract.NoAnswer {
		t.Errorf("failure = %+v, want NoAnswer: a try cut off by the deadline was handed off and not answered", failure)
	}
}

func TestOwner_PanicInsideTheLoop_TheCalloutIsStillEnded(t *testing.T) {
	router := newScriptedRouter("p-1")
	router.script("p-1", func(context.Context) dispatch.HandOverAnswer { panic("boom") })
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)
	own := e.attach(t, "m-1", tenantA, "x", detaches())

	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected the panic to reach the caller")
			}
		}()
		_, _ = e.dispatchFunction(userCtx(tenantA), "x", "")
	}()

	_, passes := own.seen()
	calloutID := pairOf(t, e, passes[0]).Callout
	for major := uint32(1); major <= 2; major++ {
		if _, err := e.fence.Admit(context.Background(), []fence.Pair{{Callout: calloutID, Major: major}}); !errors.Is(err, fence.ErrSuperseded) {
			t.Errorf("Admit(major %d) = %v, want every pass of a callout ended by a panic refused", major, err)
		}
	}
}
