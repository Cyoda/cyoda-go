package main

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"google.golang.org/grpc"

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
	for _, ok := range []string{"", "stall", "fail", "fail-retryable", "late-callback", "drop", "hold", " drop "} {
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
		{"hold", behaviourHold, false, false, false, nil, 0},
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

// TestHandleCallout_HoldAnswersFromCatalogOnRelease: a hold client records the
// work, stays silent, and on release answers it as the catalog would.
func TestHandleCallout_HoldAnswersFromCatalogOnRelease(t *testing.T) {
	d := newDispatcher("", "", newCatalog(nil, nil), nil, []string{"x"}, behaviourHold, newRecorder())
	ce, payload := processorRequest(t, "r-1", "noop", "pass-value", "")

	reply, drop, err := d.handleCallout(context.Background(), ce, payload)
	if err != nil || reply != nil || drop {
		t.Fatalf("handleCallout = (%v, %t, %v); want a silent, open stream", reply, drop, err)
	}
	work := d.takeHeldWork()
	if len(work) != 1 {
		t.Fatalf("held work = %d; want 1", len(work))
	}
	answer, err := d.answer(context.Background(), work[0].msg, work[0].payload, work[0].pass)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if body := decodeReply(t, answer); body.RequestID != "r-1" || !body.Success {
		t.Errorf("released answer = %+v; want requestId r-1, success", body)
	}
	if again := d.takeHeldWork(); len(again) != 0 {
		t.Errorf("held work taken twice: %d left", len(again))
	}
}

// recordingStream keeps every event sent on it.
type recordingStream struct {
	grpc.BidiStreamingClient[cepb.CloudEvent, cepb.CloudEvent]
	mu   sync.Mutex
	sent []*cepb.CloudEvent
}

func (s *recordingStream) Send(ce *cepb.CloudEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, ce)
	return nil
}

func (s *recordingStream) events() []*cepb.CloudEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*cepb.CloudEvent(nil), s.sent...)
}

// TestRelease_HoldSendsOneAnswerOnTheStream: a hold client sends nothing on
// receiving work, sends exactly one catalog answer for it on release, and
// nothing more on a second release.
func TestRelease_HoldSendsOneAnswerOnTheStream(t *testing.T) {
	d := newDispatcher("", "", newCatalog(nil, nil), nil, []string{"x"}, behaviourHold, newRecorder())
	s := &recordingStream{}
	d.setStream(s)
	ce, payload := processorRequest(t, "r-1", "noop", "pass-value", "")

	if reply, drop, err := d.handleCallout(context.Background(), ce, payload); err != nil || reply != nil || drop {
		t.Fatalf("handleCallout = (%v, %t, %v); want a silent, open stream", reply, drop, err)
	}
	if n := len(s.events()); n != 0 {
		t.Fatalf("%d events sent before release; want 0", n)
	}

	d.release(context.Background())
	sent := s.events()
	if len(sent) != 1 {
		t.Fatalf("%d events sent on release; want 1", len(sent))
	}
	if sent[0].Type != ceTypeProcessorResponse {
		t.Errorf("released event type = %s; want %s", sent[0].Type, ceTypeProcessorResponse)
	}
	if body := decodeReply(t, sent[0]); body.RequestID != "r-1" || !body.Success {
		t.Errorf("released answer = %+v; want requestId r-1, success", body)
	}

	d.release(context.Background())
	if n := len(s.events()); n != 1 {
		t.Errorf("%d events after a second release; want still 1", n)
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
