package grpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// callout_success_default_test.go pins the reading of `success` that the
// published schema states: the field is optional with the default `true`
// (docs/cyoda/schema/common/BaseEvent.json), so a member that omits it has
// answered success and a member reporting a failure sends `success: false`.
//
// What decides something is unaffected. The default resolves a flag the member
// left out; it never stands in for a verdict the member never gave, so an
// answer that carries no verdict, and an answer that cannot be read, are
// refused exactly as before — see the "no verdict" and "unreadable" cases here.
//
// The default belongs to an absent key alone. An explicit `success: null` is
// not a boolean and not the default: it is an answer that cannot be read, and
// nothing else in it — no verdict, no payload, no result — is read either. The
// "explicit null" case of each of the three callouts pins that.

// replyOnWire answers the one request the member is sent with the bytes a
// compute member would put on the wire, read by the same decoder the stream
// uses. Unlike replyOnce, which hands the callout a ProcessingResponse already
// built, this drives the decode — which is where an absent field is either kept
// or resolved.
func replyOnWire(t *testing.T, registry *MemberRegistry, memberID string, sentCh chan *cepb.CloudEvent,
	decode func(*Member, json.RawMessage), body string) {
	t.Helper()
	go func() {
		ce := <-sentCh
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return
		}
		decode(registry.Get(memberID), json.RawMessage(fmt.Sprintf(body, reqID)))
	}()
}

// The two client-safe sentences an unreadable answer ends with, written out
// here rather than read from the production constants: what the client is told
// is the contract these tests pin.
const (
	wantUnreadableMessage  = "the compute member's response could not be read"
	wantNullSuccessMessage = wantUnreadableMessage + ": success was null"
	wantUndecodableMessage = wantUnreadableMessage + ": the answer did not decode"
	wantNoVerdictMessage   = wantUnreadableMessage + ": matches was missing"
	wantBadPayloadMessage  = wantUnreadableMessage + ": the payload did not decode"
)

// memberFailure asserts that err is a MemberFailed carrying the member's own
// message, and returns it.
func memberFailure(t *testing.T, err error) *contract.CalloutFailure {
	t.Helper()
	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error is not a *contract.CalloutFailure: %v", err)
	}
	return failure
}

// decodeAnswer runs one of the three response decoders over the bytes a member
// put on the wire and returns the ProcessingResponse it produced.
func decodeAnswer(t *testing.T, decode func(*Member, json.RawMessage), body string) *ProcessingResponse {
	t.Helper()
	registry := NewMemberRegistry()
	member := registry.Register("m-1", testTenantID, []string{"python"},
		func(*cepb.CloudEvent) error { return nil }, nil)
	ch, err := member.TrackRequest("r-1")
	if err != nil {
		t.Fatalf("TrackRequest: %v", err)
	}
	decode(member, json.RawMessage(body))
	return <-ch
}

// The three states of the `success` key stay apart in the decode, for every one
// of the three answer shapes: absent is the schema's default and reports
// success, a boolean reports itself, and the literal null reports nothing at
// all. A *bool cannot tell the first from the last — it is nil for both.
// A null answer is left reporting no success as well as unreadable, so a reader
// that only knows the flag still fails closed.
func TestHandleResponses_SuccessKeepsItsThreeStates(t *testing.T) {
	for kind, decode := range map[string]func(*Member, json.RawMessage){
		"processor": handleProcessorResponse,
		"criteria":  handleCriteriaResponse,
		"function":  handleFunctionResponse,
	} {
		for state, tc := range map[string]struct {
			body            string
			wantSuccess     bool
			wantNullSuccess bool
		}{
			"absent": {`{"requestId":"r-1"}`, true, false},
			"true":   {`{"requestId":"r-1","success":true}`, true, false},
			"false":  {`{"requestId":"r-1","success":false}`, false, false},
			"null":   {`{"requestId":"r-1","success":null}`, false, true},
		} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				resp := decodeAnswer(t, decode, tc.body)
				if resp.Success != tc.wantSuccess {
					t.Errorf("success = %t; want %t", resp.Success, tc.wantSuccess)
				}
				gotUnreadable := resp.Unreadable != ""
				if gotUnreadable != tc.wantNullSuccess {
					t.Errorf("unreadable = %q; want unreadable=%t", resp.Unreadable, tc.wantNullSuccess)
				}
			})
		}
	}
}

// A processor's answer: the flag may be left out, and leaving it out is not a
// failure. An answer that cannot be read is still refused, whether or not the
// flag was there to read — the default resolves the flag, not the payload.
func TestDispatchProcessor_SuccessDefaultsToTrue(t *testing.T) {
	const unchanged = `{"foo":"bar"}`
	for name, tc := range map[string]struct {
		body     string // the member's response, with %q for the request id
		wantData string // the entity's data after the callout
		wantKind contract.CalloutFailureKind
		wantMsg  string
	}{
		"omitted, nothing else said": {
			body: `{"requestId":%q}`, wantData: unchanged,
		},
		"omitted, with data": {
			body: `{"requestId":%q,"payload":{"data":{"foo":"changed"}}}`, wantData: `{"foo":"changed"}`,
		},
		"explicit true": {
			body: `{"requestId":%q,"success":true,"payload":{"data":{"foo":"changed"}}}`, wantData: `{"foo":"changed"}`,
		},
		"explicit false": {
			body:     `{"requestId":%q,"success":false,"error":{"message":"the member says no"}}`,
			wantKind: contract.MemberFailed, wantMsg: "the member says no",
		},
		"omitted, unreadable payload": {
			body:     `{"requestId":%q,"payload":"not-an-object"}`,
			wantKind: contract.Terminal, wantMsg: wantBadPayloadMessage,
		},
		"explicit null": {
			body:     `{"requestId":%q,"success":null,"payload":{"data":{"foo":"changed"}}}`,
			wantKind: contract.Terminal, wantMsg: wantNullSuccessMessage,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
			replyOnWire(t, registry, memberID, sentCh, handleProcessorResponse, tc.body)

			entity, err := dispatchProcessor(dispatcher, testContext(), testEntity(),
				testProcessor("python", 5000), "wf1", "t1", "tx-1")
			if tc.wantMsg != "" {
				if err == nil {
					t.Fatalf("the callout succeeded with data %s; want the failure %q", entity.Data, tc.wantMsg)
				}
				failure := memberFailure(t, err)
				if failure.Kind != tc.wantKind {
					t.Errorf("kind = %s; want %s", failure.Kind, tc.wantKind)
				}
				if failure.Message != tc.wantMsg {
					t.Errorf("message = %q; want %q", failure.Message, tc.wantMsg)
				}
				// A failed callout returns no entity: nothing the answer
				// carried was read out of it.
				if entity != nil {
					t.Errorf("entity data = %s; want no entity from a failed callout", entity.Data)
				}
				return
			}
			if err != nil {
				t.Fatalf("the member answered without a `success` key and the callout failed: %v", err)
			}
			if string(entity.Data) != tc.wantData {
				t.Errorf("entity data = %s; want %s", entity.Data, tc.wantData)
			}
		})
	}
}

// An `error` object on its own does not report a failure. The flag is what
// says "failed"; `error` only says what went wrong once it has. With the flag
// absent the answer is a success, and the member's text is not merely
// disregarded as a verdict — it is dropped, because the dispatch reads it only
// on the failure branch, not even as a warning. Cyoda Cloud reads these bytes
// the same way, so this is the agreed reading and not a cyoda-go choice; it is
// pinned here because it is the shape a member author is most likely to send
// by mistake, and `cyoda help grpc` says so for the same reason.
func TestDispatchProcessor_ErrorObjectAloneIsNotAFailure(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := common.WithDiagnostics(testContext())
	replyOnWire(t, registry, memberID, sentCh, handleProcessorResponse,
		`{"requestId":%q,"error":{"code":"E_BOOM","message":"the member meant to report a failure"}}`)

	entity, err := dispatchProcessor(dispatcher, ctx, testEntity(),
		testProcessor("python", 5000), "wf1", "t1", "tx-1")
	if err != nil {
		t.Fatalf("an answer carrying only an `error` object failed the callout: %v", err)
	}
	if string(entity.Data) != `{"foo":"bar"}` {
		t.Errorf("entity data = %s; want the entity unchanged", entity.Data)
	}
	diag := common.GetDiagnostics(ctx)
	if got := diag.GetErrors(); len(got) != 0 {
		t.Errorf("errors = %v; want none: the member's text is read only on the failure branch", got)
	}
	if got := diag.GetWarnings(); len(got) != 0 {
		t.Errorf("warnings = %v; want none: the text is dropped, not downgraded to a warning", got)
	}
}

// A criterion's answer: the same default, and the same refusal of an answer
// with no verdict. `matches` is the verdict; `success` is not, so resolving the
// one must not invent the other.
func TestDispatchCriteria_SuccessDefaultsToTrue(t *testing.T) {
	for name, tc := range map[string]struct {
		body        string
		wantMatches bool
		wantReason  string
		wantKind    contract.CalloutFailureKind
		wantMsg     string
	}{
		"omitted, matches": {
			body: `{"requestId":%q,"matches":true}`, wantMatches: true,
		},
		"omitted, does not match": {
			body: `{"requestId":%q,"matches":false,"reason":"too small"}`, wantReason: "too small",
		},
		"explicit true": {
			body: `{"requestId":%q,"success":true,"matches":true}`, wantMatches: true,
		},
		"explicit false": {
			body:     `{"requestId":%q,"success":false,"error":{"message":"the member says no"}}`,
			wantKind: contract.MemberFailed, wantMsg: "the member says no",
		},
		"omitted, no verdict": {
			body:     `{"requestId":%q,"reason":"no verdict here"}`,
			wantKind: contract.Terminal, wantMsg: wantNoVerdictMessage,
		},
		// The shape where reading the wrong field would do real damage: a
		// verdict is there to be read, and reading it would let a member whose
		// answer says nothing about success decide a transition.
		"explicit null": {
			body:     `{"requestId":%q,"success":null,"matches":true}`,
			wantKind: contract.Terminal, wantMsg: wantNullSuccessMessage,
		},
		// The same class of defect as the null: the answer arrived and cannot
		// be read. It must end the callout where the null does, at once — not
		// by going unanswered until the answer limit runs out, which spends
		// the whole retry budget and reports a retryable 503 about a member
		// that did in fact answer.
		"success is not a boolean": {
			body:     `{"requestId":%q,"success":"yes","matches":true}`,
			wantKind: contract.Terminal, wantMsg: wantUndecodableMessage,
		},
		"matches is not a boolean": {
			body:     `{"requestId":%q,"success":true,"matches":"maybe"}`,
			wantKind: contract.Terminal, wantMsg: wantUndecodableMessage,
		},
		// The id AFTER the key that fails, which is the only ordering that
		// exercises the recovery: encoding/json fills fields in the order the
		// document lists them, so an id that comes first is already set when
		// the decode gives up, and the callout would end even without the
		// second, narrower decode that exists for this case.
		"success is not a boolean, id last": {
			body:     `{"success":"yes","matches":true,"requestId":%q}`,
			wantKind: contract.Terminal, wantMsg: wantUndecodableMessage,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
			replyOnWire(t, registry, memberID, sentCh, handleCriteriaResponse, tc.body)

			matches, reason, err := dispatchCriteria(dispatcher, testContext(), testEntity(),
				json.RawMessage(answerCriterion), "transition", "wf1", "t1", "", "tx-1")
			if tc.wantMsg != "" {
				if err == nil {
					t.Fatalf("the callout succeeded with matches=%t; want the failure %q", matches, tc.wantMsg)
				}
				failure := memberFailure(t, err)
				if failure.Kind != tc.wantKind {
					t.Errorf("kind = %s; want %s", failure.Kind, tc.wantKind)
				}
				if failure.Message != tc.wantMsg {
					t.Errorf("message = %q; want %q", failure.Message, tc.wantMsg)
				}
				// No verdict is read out of an answer the callout refused.
				if matches || reason != "" {
					t.Errorf("matches = %t reason = %q; want neither read from a refused answer", matches, reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("the member answered without a `success` key and the callout failed: %v", err)
			}
			if matches != tc.wantMatches {
				t.Errorf("matches = %t; want %t", matches, tc.wantMatches)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q; want %q", reason, tc.wantReason)
			}
		})
	}
}

// Nothing in an answer that cannot be read is read — not even its warnings. A
// warning is the member's own text, and an answer whose `success` is null has
// said nothing the platform can act on: surfacing that text while discarding
// the verdict beside it would be trusting the same answer it has just refused.
func TestDispatchCriteria_NullSuccessSurfacesNothing(t *testing.T) {
	dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := common.WithDiagnostics(testContext())
	replyOnWire(t, registry, memberID, sentCh, handleCriteriaResponse,
		`{"requestId":%q,"success":null,"matches":true,"warnings":["the member warns"]}`)

	matches, _, err := dispatchCriteria(dispatcher, ctx, testEntity(),
		json.RawMessage(answerCriterion), "transition", "wf1", "t1", "", "tx-1")
	if err == nil {
		t.Fatalf("an answer whose `success` was null was accepted as matches=%t", matches)
	}
	diag := common.GetDiagnostics(ctx)
	if got := diag.GetWarnings(); len(got) != 0 {
		t.Errorf("warnings = %v; want none: nothing is read out of an answer that could not be read", got)
	}
	if got := diag.GetErrors(); len(got) != 0 {
		t.Errorf("errors = %v; want none: the member's text is read only on the failure branch", got)
	}
}

// A function's answer: the same default. Its result is relayed, not judged, so
// there is no verdict here for the default to stand in for.
func TestDispatchFunction_SuccessDefaultsToTrue(t *testing.T) {
	for name, tc := range map[string]struct {
		body         string
		wantKind     string
		wantVal      string
		wantFailKind contract.CalloutFailureKind
		wantMsg      string
	}{
		"omitted": {
			body:     `{"requestId":%q,"resultKind":"Schedule","result":{"fireAfterMs":1}}`,
			wantKind: "Schedule", wantVal: `{"fireAfterMs":1}`,
		},
		"explicit true": {
			body:     `{"requestId":%q,"success":true,"resultKind":"Schedule","result":{"fireAfterMs":1}}`,
			wantKind: "Schedule", wantVal: `{"fireAfterMs":1}`,
		},
		"explicit false": {
			body:         `{"requestId":%q,"success":false,"error":{"message":"the member says no"}}`,
			wantFailKind: contract.MemberFailed, wantMsg: "the member says no",
		},
		"explicit null": {
			body:         `{"requestId":%q,"success":null,"resultKind":"Schedule","result":{"fireAfterMs":1}}`,
			wantFailKind: contract.Terminal, wantMsg: wantNullSuccessMessage,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dispatcher, registry, memberID, sentCh := setupTestDispatcher(t)
			replyOnWire(t, registry, memberID, sentCh, handleFunctionResponse, tc.body)

			res, err := dispatchFunction(dispatcher, testContext(), testEntity(), spi.ScheduleFunction{
				Name:                 "my-schedule-fn",
				ResultKind:           "Schedule",
				CalculationNodesTags: "python",
				ResponseTimeoutMs:    5000,
			}, "wf1", "t1", "tx-1")
			if tc.wantMsg != "" {
				if err == nil {
					t.Fatalf("the callout succeeded with resultKind %q; want the failure %q", res.Kind, tc.wantMsg)
				}
				failure := memberFailure(t, err)
				if failure.Kind != tc.wantFailKind {
					t.Errorf("kind = %s; want %s", failure.Kind, tc.wantFailKind)
				}
				if failure.Message != tc.wantMsg {
					t.Errorf("message = %q; want %q", failure.Message, tc.wantMsg)
				}
				// Nothing is relayed out of an answer the callout refused.
				if res.Kind != "" || len(res.Value) != 0 {
					t.Errorf("resultKind = %q result = %s; want neither read from a refused answer", res.Kind, res.Value)
				}
				return
			}
			if err != nil {
				t.Fatalf("the member answered without a `success` key and the callout failed: %v", err)
			}
			if res.Kind != tc.wantKind {
				t.Errorf("resultKind = %q; want %q", res.Kind, tc.wantKind)
			}
			if string(res.Value) != tc.wantVal {
				t.Errorf("result = %s; want %s", res.Value, tc.wantVal)
			}
		})
	}
}

// The recovery of the request id is the mechanism that turns an answer which
// does not decode into a callout that ends now rather than one that waits out
// its answer limit. These drive the three decoders directly, so what is pinned
// is the completion itself: which reason arrives, and whether anything arrives
// at all.
func TestHandleResponses_UndecodableAnswerCompletesTheRequest(t *testing.T) {
	for kind, decode := range map[string]func(*Member, json.RawMessage){
		"processor": handleProcessorResponse,
		"criteria":  handleCriteriaResponse,
		"function":  handleFunctionResponse,
	} {
		for state, body := range map[string]string{
			// The id before and after the key that fails: only the second
			// needs the narrower decode, and both must end the callout.
			"id first": `{"requestId":"r-1","success":"yes"}`,
			"id last":  `{"success":"yes","requestId":"r-1"}`,
		} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				resp := decodeAnswer(t, decode, body)
				if resp.Unreadable != "the answer did not decode" {
					t.Errorf("unreadable = %q; want the undecodable reason", resp.Unreadable)
				}
				if resp.Success {
					t.Error("success = true on an answer that did not decode; a flag-only reader must fail closed")
				}
			})
		}
	}
}

// An answer that names no request cannot be matched to the callout waiting for
// it, so nothing is completed and the answer limit stays the only bound. The
// callout must not be ended by some other request's id, and the decoder must
// not panic on bytes that are not JSON at all.
func TestHandleResponses_AnswerWithNoRequestIDCompletesNothing(t *testing.T) {
	for kind, decode := range map[string]func(*Member, json.RawMessage){
		"processor": handleProcessorResponse,
		"criteria":  handleCriteriaResponse,
		"function":  handleFunctionResponse,
	} {
		for state, body := range map[string]string{
			"not JSON at all":    `this is not json`,
			"no requestId":       `{"success":"yes"}`,
			"requestId is empty": `{"requestId":"","success":"yes"}`,
			"requestId not text": `{"requestId":7,"success":"yes"}`,
		} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				registry := NewMemberRegistry()
				member := registry.Register("m-1", testTenantID, []string{"python"},
					func(*cepb.CloudEvent) error { return nil }, nil)
				ch, err := member.TrackRequest("r-1")
				if err != nil {
					t.Fatalf("TrackRequest: %v", err)
				}
				decode(member, json.RawMessage(body))
				select {
				case got := <-ch:
					t.Fatalf("the pending request was completed with %+v; an answer naming no request must complete nothing", got)
				default:
				}
			})
		}
	}
}
