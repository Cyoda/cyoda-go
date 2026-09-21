package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		{"no request tenant", func(r *DispatchCalloutRequest) { r.TenantID = "" }},
		// The equality alone passes this one, and the callout would run under no
		// tenant at all.
		{"no tenant on either half", func(r *DispatchCalloutRequest) { r.TenantID, r.EntityMeta.TenantID = "", "" }},
		{"no request id", func(r *DispatchCalloutRequest) { r.RequestID = "" }},
		{"no tries", func(r *DispatchCalloutRequest) { r.TriesLeft = 0 }},
		{"no answer limit", func(r *DispatchCalloutRequest) { r.AnswerLimitMs = 0 }},
		{"no owner", func(r *DispatchCalloutRequest) { r.OwnerNodeID = "" }},
		{"no fencing number", func(r *DispatchCalloutRequest) { r.Major = 0 }},
		{"processor missing", func(r *DispatchCalloutRequest) { r.Processor = nil }},
		{"no entity id", func(r *DispatchCalloutRequest) { r.EntityMeta.ID = "" }},
		{"no entity", func(r *DispatchCalloutRequest) { r.Entity = nil }},
		// time.Duration(ms) * time.Millisecond wraps negative above this.
		{"answer limit that does not fit a duration", func(r *DispatchCalloutRequest) { r.AnswerLimitMs = 9_300_000_000_000 }},
		{"enclosing pair with no callout", func(r *DispatchCalloutRequest) {
			r.Outer = []WirePair{{Callout: "", Major: 2, Minor: 1}}
		}},
		{"enclosing pair with no fencing number", func(r *DispatchCalloutRequest) {
			r.Outer = []WirePair{{Callout: "outer-rid", Major: 0, Minor: 1}}
		}},
		// json.Unmarshal reads "entity": null into the four bytes `null`, which
		// is not an empty slice. It is no entity all the same.
		{"entity that is JSON null", func(r *DispatchCalloutRequest) { r.Entity = json.RawMessage(`null`) }},
		{"more enclosing pairs than can be sane", func(r *DispatchCalloutRequest) {
			r.Outer = outerPairs(maxOuterPairs + 1)
		}},
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

// outerPairs builds n distinct enclosing pairs, each at a usable number.
func outerPairs(n int) []WirePair {
	out := make([]WirePair, n)
	for i := range out {
		out[i] = WirePair{Callout: fmt.Sprintf("outer-%d", i), Major: 1}
	}
	return out
}

// The nesting bound is a bound: exactly as many enclosing pairs as are allowed
// still validate, one more does not. Nothing in the tree nests callbacks this
// deep; the bound is on untrusted input, since every pair is copied into every
// pass the receiving pnode mints.
func TestRequestValidate_EnclosingPairsBound(t *testing.T) {
	req := validRequest(t, "processor")
	req.Outer = outerPairs(maxOuterPairs)
	if err := req.validate(); err != nil {
		t.Errorf("%d enclosing pairs must validate: %v", maxOuterPairs, err)
	}
	req.Outer = outerPairs(maxOuterPairs + 1)
	if err := req.validate(); err == nil {
		t.Errorf("%d enclosing pairs must be refused", maxOuterPairs+1)
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

// validate refuses all of these before toCallout is reached; toCallout refuses
// them again rather than dereference a definition that is not there.
func TestToCallout_RefusesWhatItCannotBuild(t *testing.T) {
	tests := []struct {
		name   string
		kind   string
		mutate func(*DispatchCalloutRequest)
	}{
		{"unknown kind", "processor", func(r *DispatchCalloutRequest) { r.Kind = "bogus" }},
		{"no kind at all", "processor", func(r *DispatchCalloutRequest) { r.Kind = "" }},
		{"processor missing", "processor", func(r *DispatchCalloutRequest) { r.Processor = nil }},
		{"function missing", "function", func(r *DispatchCalloutRequest) { r.Function = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validRequest(t, tt.kind)
			tt.mutate(&req)
			call, failure := req.toCallout()
			if failure == nil || failure.Kind != contract.Terminal {
				t.Fatalf("failure = %+v, want Terminal", failure)
			}
			if call.RequestID != "" {
				t.Errorf("a refused request still built a callout: %+v", call)
			}
		})
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

// A request the receiving pnode can open but not accept is refused with a
// sealed answer of zero tries, so that it neither uses a try nor is reported as
// a retryable 503.
func TestRefusal(t *testing.T) {
	r := refusal(&contract.CalloutFailure{Kind: contract.Terminal, Message: "unknown callout kind",
		Err: common.Internal("the hand-over could not be accepted", errors.New("unknown callout kind"))})
	if r.Outcome != "terminal" || r.TriesUsed == nil || *r.TriesUsed != 0 ||
		r.ErrorStatus != http.StatusInternalServerError || r.ErrorCode != common.ErrCodeServerError {
		t.Errorf("%+v", r)
	}
	if len(r.Attempts) != 0 {
		t.Errorf("a refusal made before any try has no attempts: %+v", r.Attempts)
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

// An ok answer that carries no result is malformed: a genuine peer always
// sends the entity and the criterion's verdict. Reading the absence as an
// empty entity or as "does not match" would put an invented value where the
// cnode's answer belongs.
func TestReadAnswer_OKWithNoResultIsNotBelieved(t *testing.T) {
	tests := []struct {
		name string
		kind string
		resp DispatchCalloutResponse
	}{
		{"processor, no entity data", "processor", DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1)}},
		{"processor, empty entity data", "processor", DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1), EntityData: []byte{}}},
		{"criteria, no verdict", "criteria", DispatchCalloutResponse{Outcome: OutcomeOK, TriesUsed: intPtr(1), Reason: "big"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := tt.resp
			assertLost(t, readAnswer(ownerCallout(t, tt.kind), &resp, 3))
		})
	}
}

// A failure outcome that also carries a payload: the payload is ignored and
// the failure stands. Nothing a failed answer carries can become a result.
func TestReadAnswer_FailureThatCarriesAPayload(t *testing.T) {
	yes := true
	for _, outcome := range []string{"no_handoff", "no_answer", "member_failed", "terminal"} {
		t.Run(outcome, func(t *testing.T) {
			a := readAnswer(ownerCallout(t, "processor"), &DispatchCalloutResponse{
				Outcome: outcome, TriesUsed: intPtr(1), ErrorCode: common.ErrCodeDispatchTimeout, ErrorStatus: 503, ErrorRetryable: true,
				EntityData: []byte(`{"out":1}`), Matches: &yes, Reason: "big", ResultKind: "Schedule", Result: json.RawMessage(`{}`),
			}, 3)
			if a.Result != nil {
				t.Errorf("a failed answer produced a result: %+v", a.Result)
			}
			if a.Failure == nil {
				t.Fatal("the failure did not stand")
			}
		})
	}
}

// The owner's budget must not be understated: an answer it cannot read still
// says how many tries the peer spent, and that number is believed when it is
// in range.
func TestReadAnswer_UnreadableAnswerSpendsTheTriesItClaims(t *testing.T) {
	t.Run("in range", func(t *testing.T) {
		a := readAnswer(ownerCallout(t, "processor"), &DispatchCalloutResponse{Outcome: "maybe", TriesUsed: intPtr(2)}, 3)
		if a.Failure == nil || a.Failure.Code != common.ErrCodeDispatchForwardFailed || a.TriesUsed != 2 {
			t.Errorf("%+v", a)
		}
	})
	t.Run("zero still counts as one", func(t *testing.T) {
		assertLost(t, readAnswer(ownerCallout(t, "processor"), &DispatchCalloutResponse{Outcome: "maybe", TriesUsed: intPtr(0)}, 3))
	})
	t.Run("absent counts as one", func(t *testing.T) {
		assertLost(t, readAnswer(ownerCallout(t, "processor"), &DispatchCalloutResponse{Outcome: "maybe"}, 3))
	})
	t.Run("out of range counts as one", func(t *testing.T) {
		assertLost(t, readAnswer(ownerCallout(t, "processor"), &DispatchCalloutResponse{Outcome: "maybe", TriesUsed: intPtr(9)}, 3))
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
			DispatchCalloutResponse{Outcome: "no_handoff", TriesUsed: intPtr(2), ErrorCode: common.ErrCodeComputeMemberDisconnected, ErrorStatus: 503, ErrorRetryable: true,
				Attempts: []WireAttempt{{MemberID: "m1", Kind: "no_handoff", Cause: "gone"}, {MemberID: "m2", Kind: "no_handoff", Cause: "gone"}}},
			contract.NoHandOff, common.ErrCodeComputeMemberDisconnected, 503, true, "", 2, true},
		{"terminal with a ticketed 500",
			DispatchCalloutResponse{Outcome: "terminal", TriesUsed: intPtr(1), ErrorCode: common.ErrCodeServerError, ErrorStatus: 500,
				Attempts: []WireAttempt{{MemberID: "m1", Kind: "terminal", Cause: "internal error"}}},
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
		t.Fatalf("Attempts = %+v, want one attempt with member \"-\"", a.Attempts)
	}
	// The attempt's cause carries the code, as every local try's does
	// (run_local.go uses failure.Message): CALLOUT_FAILED renders each entry as
	// "[member<->: CODE: text]" and the help topic documents that shape.
	if want := common.ErrCodeDispatchForwardFailed + ": " + forwardFailedClientMessage; a.Attempts[0].Cause != want {
		t.Errorf("Cause = %q, want %q", a.Attempts[0].Cause, want)
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

// A peer that spent a try on a failure always names the try that failed
// (responseFromLocal). An answer with a failure, tries spent and no attempt
// contradicts itself: it is not believed, and is read as a lost answer that
// spent the tries it claims.
func TestReadAnswer_FailureWithTriesAndNoAttempt(t *testing.T) {
	for _, outcome := range []string{"no_answer", "member_failed", "no_handoff", "terminal"} {
		t.Run(outcome, func(t *testing.T) {
			assertLost(t, readAnswer(ownerCallout(t, "processor"), &DispatchCalloutResponse{Outcome: outcome, TriesUsed: intPtr(1),
				ErrorCode: common.ErrCodeDispatchTimeout, ErrorStatus: http.StatusServiceUnavailable, MemberError: "the rates service is down"}, 3))
		})
	}
}

// A pnode of an earlier version answers without outcome and triesUsed.
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
	// A peer that was never reached did not run out of compute members; the
	// message the client sees must say what actually happened.
	if strings.Contains(a.Failure.Message, "compute member") {
		t.Errorf("a peer that was never reached is reported as having no compute member: %q", a.Failure.Message)
	}
	if !strings.Contains(a.Failure.Message, "could not be reached") {
		t.Errorf("Message = %q", a.Failure.Message)
	}
}

// A hand-over proved impossible before any connection — a request that cannot
// be marshalled or signed — is Terminal, and whatever the error behind it says
// never reaches the client-safe message. (An unusable peer *address* does not
// come here: that peer is skipped, not connected and no try used.)
func TestProvedBeforeConnecting(t *testing.T) {
	a := provedBeforeConnecting(errors.New("sign hand-over for peer at 169.254.1.1: seal failed"))
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

// Nothing the constructor puts in the error may reach a client in ANY error
// mode. Verbose mode returns AppError.Detail in the response body, so the
// error's own text goes in the wrapped cause, which no renderer reads.
func TestProvedBeforeConnecting_NothingRendersThePeerAddress(t *testing.T) {
	common.SetErrorResponseMode("verbose")
	t.Cleanup(func() { common.SetErrorResponseMode("sanitized") })

	a := provedBeforeConnecting(errors.New("dial peer at 169.254.1.1:8080: seal failed"))
	var appErr *common.AppError
	if !errors.As(a.Failure, &appErr) {
		t.Fatalf("Failure = %+v", a.Failure)
	}
	rec := httptest.NewRecorder()
	common.WriteError(rec, httptest.NewRequest(http.MethodPost, "/entity", nil), appErr)
	if body := rec.Body.String(); strings.Contains(body, "169.254") || strings.Contains(body, "8080") {
		t.Errorf("the rendered error names the peer address: %s", body)
	}
	// The cause stays inspectable for the caller that logs it.
	if !strings.Contains(errors.Unwrap(appErr).Error(), "169.254.1.1") {
		t.Error("the cause was dropped instead of kept out of the response")
	}
}
