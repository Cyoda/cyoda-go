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
			wantKind: contract.Terminal, wantMsg: "the compute member's response could not be read",
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
			wantKind: contract.Terminal, wantMsg: "the compute member's response could not be read",
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

// A function's answer: the same default. Its result is relayed, not judged, so
// there is no verdict here for the default to stand in for.
func TestDispatchFunction_SuccessDefaultsToTrue(t *testing.T) {
	for name, tc := range map[string]struct {
		body     string
		wantKind string
		wantVal  string
		wantMsg  string
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
			body:    `{"requestId":%q,"success":false,"error":{"message":"the member says no"}}`,
			wantMsg: "the member says no",
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
				if failure.Kind != contract.MemberFailed {
					t.Errorf("kind = %s; want %s", failure.Kind, contract.MemberFailed)
				}
				if failure.Message != tc.wantMsg {
					t.Errorf("message = %q; want %q", failure.Message, tc.wantMsg)
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
