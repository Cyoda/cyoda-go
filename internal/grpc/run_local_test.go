package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// --- scripted cnodes ---

// asked records what one scripted cnode was sent.
type asked struct {
	mu         sync.Mutex
	requestIDs []string // payload requestId of every request
	payloadIDs []string // payload id of every request
	passes     []string // the pass attribute of every request
}

func (a *asked) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requestIDs)
}

// seen returns copies of the request and payload ids recorded so far.
func (a *asked) seen() (requestIDs, payloadIDs []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.requestIDs...), append([]string(nil), a.payloadIDs...)
}

// script is what a scripted cnode does with a request; nil stays silent.
type script func(m *Member, requestID string)

func answers(resp ProcessingResponse) script {
	return func(m *Member, requestID string) { m.CompleteRequest(requestID, &resp) }
}

func answersAs(id string) script {
	return answers(ProcessingResponse{Success: true, Payload: json.RawMessage(`{"data":{"by":"` + id + `"}}`)})
}

func drops() script {
	return func(m *Member, _ string) { m.Evict(errors.New("stream dropped")) }
}

// attach registers a scripted cnode. Ids given in ascending order are tried in
// that order by a fresh registry: attached first, tried first, the id breaking
// a tie on the clock.
func attach(t *testing.T, reg *MemberRegistry, id string, tenant spi.TenantID, tag string, s script) (*Member, *asked) {
	t.Helper()
	a := &asked{}
	m := reg.Register(id, tenant, []string{tag}, func(ce *cepb.CloudEvent) error {
		_, payload, err := ParseCloudEvent(ce)
		if err != nil {
			t.Errorf("ParseCloudEvent: %v", err)
			return nil
		}
		var body struct {
			ID        string `json:"id"`
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Errorf("request payload: %v", err)
			return nil
		}
		func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.requestIDs = append(a.requestIDs, body.RequestID)
			a.payloadIDs = append(a.payloadIDs, body.ID)
			a.passes = append(a.passes, TxTokenFromCloudEvent(ce))
		}()
		if s != nil {
			if m := reg.Get(id); m != nil {
				s(m, body.RequestID)
			}
		}
		return nil
	}, nil)
	t.Cleanup(func() { reg.Unregister(m) })
	return m, a
}

// attachGone registers a cnode and evicts it without unregistering: it is still
// listed, and the try on it fails before the hand-off.
func attachGone(t *testing.T, reg *MemberRegistry, id string, tenant spi.TenantID, tag string) *asked {
	t.Helper()
	m, a := attach(t, reg, id, tenant, tag, nil)
	m.Evict(errors.New("gone"))
	return a
}

// countingNumberer is the owner's numbering: major rises before each try.
type countingNumberer struct{ major uint32 }

func (n *countingNumberer) Next() (uint32, uint32) { n.major++; return n.major, 0 }

// armed fills what the caller of a builder fills.
func armed(call Callout, repeatSafe bool, limit time.Duration) Callout {
	call.RequestID = "req-fixed"
	call.AnswerLimit = limit
	call.RepeatSafe = repeatSafe
	call.OwnerNodeID = "node-test"
	call.Number = &countingNumberer{}
	return call
}

func processorCall(tag string, repeatSafe bool, limit time.Duration) Callout {
	return armed(NewProcessorCallout(testTenantID, testEntity(), testProcessor(tag, 0), "wf1", "t1", "tx-1"), repeatSafe, limit)
}

func answeredBy(t *testing.T, res LocalResult) string {
	t.Helper()
	if !res.OK() {
		t.Fatalf("expected an answer, got failure=%v ctxErr=%v", res.Failure, res.CtxErr)
	}
	var data struct {
		By string `json:"by"`
	}
	if err := json.Unmarshal(res.Result.Entity.Data, &data); err != nil {
		t.Fatalf("result data: %v", err)
	}
	return data.By
}

func appCode(err error) string {
	var appErr *common.AppError
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return ""
}

// --- the §13 rows ---

func TestRunLocal_NoHandOff_NextCnodeAnswers(t *testing.T) {
	reg := NewMemberRegistry()
	gone := attachGone(t, reg, "m-1", testTenantID, "x")
	attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)

	// not repeat-safe: a failed hand-off permits another cnode all the same
	res := d.RunLocal(testContext(), processorCall("x", false, 5*time.Second), 4)
	if by := answeredBy(t, res); by != "m-2" {
		t.Errorf("answered by %s, want m-2", by)
	}
	if res.TriesUsed != 2 {
		t.Errorf("TriesUsed = %d, want 2", res.TriesUsed)
	}
	if len(res.Attempts) != 1 || res.Attempts[0].MemberID != "m-1" || res.Attempts[0].Kind != contract.NoHandOff {
		t.Errorf("Attempts = %+v, want one NoHandOff on m-1", res.Attempts)
	}
	if gone.count() != 0 {
		t.Error("nothing may have been sent to the cnode that was gone")
	}
}

func TestRunLocal_NoHandOff_OneTry_ReportsTheTrysOwnCode(t *testing.T) {
	t.Run("cnode gone", func(t *testing.T) {
		reg := NewMemberRegistry()
		attachGone(t, reg, "m-1", testTenantID, "x")
		_, second := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
		d := newTestDispatcher(t, reg)

		res := d.RunLocal(testContext(), processorCall("x", false, 5*time.Second), 1)
		if res.Failure == nil || res.Failure.Kind != contract.NoHandOff || appCode(res.Err()) != common.ErrCodeComputeMemberDisconnected {
			t.Fatalf("failure = %+v, want NoHandOff COMPUTE_MEMBER_DISCONNECTED", res.Failure)
		}
		if res.TriesUsed != 1 || second.count() != 0 {
			t.Errorf("TriesUsed = %d, second cnode asked %d times; want 1 and 0", res.TriesUsed, second.count())
		}
	})
	t.Run("cnode not draining", func(t *testing.T) {
		d, _ := newWedgedDispatcher(t) // one cnode, tag "python", writer parked
		res := d.RunLocal(testContext(), processorCall("python", false, 100*time.Millisecond), 1)
		if res.Failure == nil || res.Failure.Kind != contract.NoHandOff || appCode(res.Err()) != common.ErrCodeDispatchTimeout {
			t.Fatalf("failure = %+v, want NoHandOff DISPATCH_TIMEOUT", res.Failure)
		}
	})
}

func TestRunLocal_NoAnswer_NotRepeatSafe_Stops(t *testing.T) {
	reg := NewMemberRegistry()
	attach(t, reg, "m-1", testTenantID, "x", nil) // takes the work, never answers
	_, second := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)

	res := d.RunLocal(testContext(), processorCall("x", false, 50*time.Millisecond), 4)
	if res.Failure == nil || res.Failure.Kind != contract.NoAnswer || appCode(res.Err()) != common.ErrCodeDispatchTimeout {
		t.Fatalf("failure = %+v, want NoAnswer DISPATCH_TIMEOUT", res.Failure)
	}
	if res.TriesUsed != 1 || second.count() != 0 {
		t.Errorf("TriesUsed = %d, second cnode asked %d times; want 1 and 0", res.TriesUsed, second.count())
	}
}

func TestRunLocal_NoAnswer_RepeatSafe_NextCnodeAnswers(t *testing.T) {
	yes := true
	criterion := json.RawMessage(`{"type":"function","function":{"name":"check","config":{"calculationNodesTags":"x"}}}`)
	criteriaCall, failure := NewCriteriaCallout(testTenantID, testEntity(), criterion, "transition", "wf1", "t1", "", "tx-1")
	if failure != nil {
		t.Fatal(failure)
	}
	functionCall := NewFunctionCallout(testTenantID, testEntity(),
		spi.ScheduleFunction{Name: "calcFire", ResultKind: "Schedule", CalculationNodesTags: "x"}, "wf1", "t1", "tx-1")

	tests := []struct {
		name   string
		call   Callout
		answer ProcessingResponse
		check  func(t *testing.T, r CalloutResult)
	}{
		{"processor declared idempotent", processorCall("x", true, 50*time.Millisecond),
			ProcessingResponse{Success: true, Payload: json.RawMessage(`{"data":{"by":"m-2"}}`)},
			func(t *testing.T, r CalloutResult) {
				if string(r.Entity.Data) != `{"by":"m-2"}` {
					t.Errorf("entity data = %s", r.Entity.Data)
				}
			}},
		// the builders mark these repeat-safe; armed is told to keep that
		{"criterion", armed(criteriaCall, criteriaCall.RepeatSafe, 50*time.Millisecond),
			ProcessingResponse{Success: true, Matches: &yes},
			func(t *testing.T, r CalloutResult) {
				if !r.Matches {
					t.Error("expected matches=true from the second cnode")
				}
			}},
		{"function", armed(functionCall, functionCall.RepeatSafe, 50*time.Millisecond),
			ProcessingResponse{Success: true, ResultKind: "Schedule", Result: json.RawMessage(`{"fireAfterMs":1}`)},
			func(t *testing.T, r CalloutResult) {
				if r.Function.Kind != "Schedule" {
					t.Errorf("function result = %+v", r.Function)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := NewMemberRegistry()
			_, first := attach(t, reg, "m-1", testTenantID, "x", nil)
			attach(t, reg, "m-2", testTenantID, "x", answers(tt.answer))
			d := newTestDispatcher(t, reg)

			res := d.RunLocal(testContext(), tt.call, 4)
			if !res.OK() {
				t.Fatalf("failure=%v ctxErr=%v", res.Failure, res.CtxErr)
			}
			tt.check(t, res.Result)
			if res.TriesUsed != 2 || first.count() != 1 {
				t.Errorf("TriesUsed = %d, first cnode asked %d times; want 2 and 1", res.TriesUsed, first.count())
			}
			if len(res.Attempts) != 1 || res.Attempts[0].Kind != contract.NoAnswer || res.Attempts[0].MemberID != "m-1" {
				t.Errorf("Attempts = %+v", res.Attempts)
			}
		})
	}
}

func TestRunLocal_CnodeDropsAfterHandOff(t *testing.T) {
	for _, repeatSafe := range []bool{false, true} {
		name := "not repeat-safe: stop"
		if repeatSafe {
			name = "repeat-safe: next cnode answers"
		}
		t.Run(name, func(t *testing.T) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", drops())
			_, second := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
			d := newTestDispatcher(t, reg)

			res := d.RunLocal(testContext(), processorCall("x", repeatSafe, 5*time.Second), 4)
			if repeatSafe {
				if by := answeredBy(t, res); by != "m-2" || res.TriesUsed != 2 {
					t.Errorf("answered by %s after %d tries, want m-2 after 2", by, res.TriesUsed)
				}
				return
			}
			if res.Failure == nil || res.Failure.Kind != contract.NoAnswer || appCode(res.Err()) != common.ErrCodeComputeMemberDisconnected {
				t.Fatalf("failure = %+v, want NoAnswer COMPUTE_MEMBER_DISCONNECTED", res.Failure)
			}
			if res.TriesUsed != 1 || second.count() != 0 {
				t.Errorf("TriesUsed = %d, second cnode asked %d times; want 1 and 0", res.TriesUsed, second.count())
			}
		})
	}
}

func TestRunLocal_MemberFailed_StopsAfterOneTry_WithTheVerdict(t *testing.T) {
	yes, no := true, false
	for name, verdict := range map[string]*bool{"true": &yes, "false": &no, "absent": nil} {
		t.Run("verdict "+name, func(t *testing.T) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", answers(ProcessingResponse{Success: false, Error: "card declined", Retryable: verdict}))
			_, second := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
			d := newTestDispatcher(t, reg)

			// repeat-safe, to show that even then a cnode's "I failed" ends it
			res := d.RunLocal(testContext(), processorCall("x", true, 5*time.Second), 4)
			if res.Failure == nil || res.Failure.Kind != contract.MemberFailed || res.Failure.Message != "card declined" {
				t.Fatalf("failure = %+v", res.Failure)
			}
			if (res.Failure.Retryable == nil) != (verdict == nil) || (verdict != nil && *res.Failure.Retryable != *verdict) {
				t.Errorf("Retryable = %v, want %v", res.Failure.Retryable, verdict)
			}
			if res.TriesUsed != 1 || second.count() != 0 {
				t.Errorf("TriesUsed = %d, second cnode asked %d times; want 1 and 0", res.TriesUsed, second.count())
			}
		})
	}
}

func TestRunLocal_Terminal_Stops(t *testing.T) {
	reg := NewMemberRegistry()
	_, first := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
	_, second := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)
	noKind := spi.WithUserContext(context.Background(), &spi.UserContext{UserID: "u", Tenant: spi.Tenant{ID: testTenantID}})

	res := d.RunLocal(noKind, processorCall("x", true, 5*time.Second), 4)
	if res.Failure == nil || res.Failure.Kind != contract.Terminal || !errors.Is(res.Err(), contract.ErrAuthContextUnavailable) {
		t.Fatalf("failure = %+v, want Terminal (auth context)", res.Failure)
	}
	if res.TriesUsed != 1 || first.count()+second.count() != 0 {
		t.Errorf("TriesUsed = %d, requests sent = %d; want 1 and 0", res.TriesUsed, first.count()+second.count())
	}
}

func TestRunLocal_SameRequestIDOnEveryTry(t *testing.T) {
	reg := NewMemberRegistry()
	_, a1 := attach(t, reg, "m-1", testTenantID, "x", nil)
	_, a2 := attach(t, reg, "m-2", testTenantID, "x", nil)
	_, a3 := attach(t, reg, "m-3", testTenantID, "x", answersAs("m-3"))
	d := newTestDispatcher(t, reg)

	res := d.RunLocal(testContext(), processorCall("x", true, 50*time.Millisecond), 4)
	if by := answeredBy(t, res); by != "m-3" || res.TriesUsed != 3 {
		t.Fatalf("answered by %s after %d tries, want m-3 after 3", by, res.TriesUsed)
	}
	for i, a := range []*asked{a1, a2, a3} {
		requestIDs, payloadIDs := a.seen()
		if len(requestIDs) != 1 || requestIDs[0] != "req-fixed" || payloadIDs[0] != "req-fixed" {
			t.Errorf("cnode %d saw requestId=%v id=%v, want one request with both req-fixed", i+1, requestIDs, payloadIDs)
		}
	}
}

func TestRunLocal_NeverTheSameCnodeTwice_AndStopsWhenTheyAreUsedUp(t *testing.T) {
	reg := NewMemberRegistry()
	_, a1 := attach(t, reg, "m-1", testTenantID, "x", nil)
	_, a2 := attach(t, reg, "m-2", testTenantID, "x", nil)
	d := newTestDispatcher(t, reg)

	start := time.Now()
	res := d.RunLocal(testContext(), processorCall("x", true, 50*time.Millisecond), 4)
	if res.TriesUsed != 2 || a1.count() != 1 || a2.count() != 1 {
		t.Fatalf("TriesUsed = %d, asked = %d/%d; want 2 tries, one per cnode", res.TriesUsed, a1.count(), a2.count())
	}
	if res.Failure == nil || res.Failure.Kind != contract.NoAnswer || len(res.Attempts) != 2 || appCode(res.Err()) != common.ErrCodeDispatchTimeout {
		t.Errorf("failure = %+v attempts = %+v code = %q; want the last try's NoAnswer, its DISPATCH_TIMEOUT code surviving, and two attempts", res.Failure, res.Attempts, appCode(res.Err()))
	}
	if res.Failure.Attempts != nil {
		t.Error("RunLocal reports its tries in LocalResult.Attempts, not on the failure")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v: RunLocal must not wait for a cnode to appear", d)
	}
}

func TestRunLocal_MaxTriesBoundsTheRun(t *testing.T) {
	reg := NewMemberRegistry()
	var all []*asked
	for _, id := range []string{"m-1", "m-2", "m-3"} {
		_, a := attach(t, reg, id, testTenantID, "x", nil)
		all = append(all, a)
	}
	d := newTestDispatcher(t, reg)

	res := d.RunLocal(testContext(), processorCall("x", true, 50*time.Millisecond), 2)
	total := 0
	for _, a := range all {
		total += a.count()
	}
	if res.TriesUsed != 2 || total != 2 {
		t.Errorf("TriesUsed = %d, requests sent = %d; want 2 and 2", res.TriesUsed, total)
	}
}

func TestRunLocal_NoMatchingCnode_IsNoHandOffWithNoTry_AndDoesNotWait(t *testing.T) {
	d := newTestDispatcher(t, NewMemberRegistry())
	start := time.Now()
	res := d.RunLocal(testContext(), processorCall("x", false, 5*time.Second), 4)
	if res.Failure == nil || res.Failure.Kind != contract.NoHandOff || !errors.Is(res.Err(), ErrNoMatchingMember) {
		t.Fatalf("failure = %+v, want NoHandOff wrapping ErrNoMatchingMember", res.Failure)
	}
	if res.Failure.Code != common.ErrCodeNoComputeMemberForTag {
		t.Errorf("Code = %q", res.Failure.Code)
	}
	if res.TriesUsed != 0 || len(res.Attempts) != 0 {
		t.Errorf("TriesUsed = %d, Attempts = %+v; looking and finding none is not a try", res.TriesUsed, res.Attempts)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v: waiting is the owner's", d)
	}
}

func TestRunLocal_CallerCancelled_EndsTheRun(t *testing.T) {
	reg := NewMemberRegistry()
	ctx, cancel := context.WithCancel(testContext())
	attach(t, reg, "m-1", testTenantID, "x", func(*Member, string) { cancel() })
	_, second := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)

	res := d.RunLocal(ctx, processorCall("x", true, 5*time.Second), 4)
	if res.CtxErr != context.Canceled || res.Failure != nil || res.Err() != context.Canceled {
		t.Fatalf("CtxErr = %v Failure = %v, want context.Canceled unchanged and no failure", res.CtxErr, res.Failure)
	}
	if res.TriesUsed != 1 || second.count() != 0 {
		t.Errorf("TriesUsed = %d, second cnode asked %d times; want 1 and 0", res.TriesUsed, second.count())
	}
}

func TestRunLocal_TwoTenantsShareATag(t *testing.T) {
	reg := NewMemberRegistry()
	_, other := attach(t, reg, "m-other", "tenant-2", "shared", answersAs("m-other"))
	attachGone(t, reg, "m-mine", testTenantID, "shared")
	d := newTestDispatcher(t, reg)

	res := d.RunLocal(testContext(), processorCall("shared", true, 5*time.Second), 4)
	if res.OK() {
		t.Fatal("tenant-1's callout must not be answered by tenant-2's cnode")
	}
	if other.count() != 0 {
		t.Fatal("tenant-2's cnode was sent tenant-1's work")
	}
	if res.TriesUsed != 1 || len(res.Attempts) != 1 || res.Attempts[0].MemberID != "m-mine" {
		t.Errorf("TriesUsed = %d Attempts = %+v; want one attempt, naming only tenant-1's cnode", res.TriesUsed, res.Attempts)
	}
}

func TestRunLocal_RoundRobinAcrossCallouts_AndANewCnodeGoesFirst(t *testing.T) {
	reg := NewMemberRegistry()
	attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
	attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)
	run := func() string {
		return answeredBy(t, d.RunLocal(testContext(), processorCall("x", false, 5*time.Second), 4))
	}
	for i, want := range []string{"m-1", "m-2", "m-1"} {
		if got := run(); got != want {
			t.Fatalf("callout %d answered by %s, want %s", i, got, want)
		}
	}
	attach(t, reg, "m-3", testTenantID, "x", answersAs("m-3"))
	if got := run(); got != "m-3" {
		t.Fatalf("a cnode that has just attached goes first, got %s", got)
	}
}

// A cnode that attaches while the run is in progress is seen by the next try.
func TestRunLocal_SeesACnodeAttachedDuringTheRun(t *testing.T) {
	reg := NewMemberRegistry()
	attach(t, reg, "m-1", testTenantID, "x", func(*Member, string) {
		attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	})
	d := newTestDispatcher(t, reg)

	res := d.RunLocal(testContext(), processorCall("x", true, 100*time.Millisecond), 4)
	if by := answeredBy(t, res); by != "m-2" || res.TriesUsed != 2 {
		t.Errorf("answered by %s after %d tries, want m-2 after 2", by, res.TriesUsed)
	}
}

func TestRunLocal_NumbersEveryTryBeforeItIsMade(t *testing.T) {
	reg := NewMemberRegistry()
	attachGone(t, reg, "m-1", testTenantID, "x") // the hand-off fails: still a try, still numbered
	attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)
	call := processorCall("x", false, 5*time.Second)
	numberer := call.Number.(*countingNumberer)

	if res := d.RunLocal(testContext(), call, 4); !res.OK() || res.TriesUsed != 2 {
		t.Fatalf("res = %+v", res)
	}
	if numberer.major != 2 {
		t.Errorf("Next was called %d times, want once per try (2)", numberer.major)
	}
}

func TestMinorNumberer(t *testing.T) {
	n := NewMinorNumberer(7)
	for want := uint32(1); want <= 3; want++ {
		if major, minor := n.Next(); major != 7 || minor != want {
			t.Fatalf("Next() = (%d, %d), want (7, %d)", major, minor, want)
		}
	}
}
