package dispatch_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// maxStorableEntity is the largest payload an entity write may carry:
// internal/domain/entity's maxEntityBodySize, which bounds the whole request
// body and therefore the payload inside it. Duplicated rather than exported
// across the boundary — this package must not import the entity domain — and
// pinned here so the ceiling's derivation is checkable beside it.
const maxStorableEntity = 10 * 1024 * 1024

// echoRunner answers a hand-over with the entity it was given, and keeps the
// bytes it received. That is what a processor with nothing to change does,
// and it is the path on which the owner persists what the hand-over delivered.
type echoRunner struct {
	got    []byte
	answer []byte
	tries  int
}

func (r *echoRunner) RunLocal(_ context.Context, call internalgrpc.Callout, _ int) internalgrpc.LocalResult {
	r.tries++
	r.got = call.Source.Entity.Data
	data := r.answer
	if data == nil {
		data = call.Source.Entity.Data
	}
	return internalgrpc.LocalResult{
		TriesUsed: 1,
		Result:    internalgrpc.CalloutResult{Entity: &spi.Entity{Meta: call.Source.Entity.Meta, Data: data}},
	}
}

// handOverEntity sends one hand-over carrying entity through a real forwarder to
// a real handler, and returns what the receiving node's local procedure was
// given and what came back to the owner.
func handOverEntity(t *testing.T, entity []byte, answer []byte) (got []byte, back []byte) {
	t.Helper()
	auth := newTestPeerAuth(t)
	runner := &echoRunner{answer: answer}
	mux := http.NewServeMux()
	dispatch.NewDispatchHandler(runner, auth).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req := makeProcessorReq()
	req.Entity = entity
	req.RequestID, req.TriesLeft, req.AnswerLimitMs, req.OwnerNodeID, req.Major = "rid-bytes", 1, 1000, "node-owner", 1

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 5*time.Second).AllowLoopbackForTesting()
	resp, err := f.ForwardCallout(context.Background(), testNodeID, srv.URL, req)
	if err != nil {
		t.Fatalf("ForwardCallout: %v", err)
	}
	if runner.tries != 1 {
		t.Fatalf("the local procedure ran %d times", runner.tries)
	}
	if resp.Outcome != dispatch.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", resp.Outcome)
	}
	return runner.got, resp.EntityData
}

// An entity's payload is persisted as the bytes it arrived as, and a hand-over
// must deliver those bytes to the compute member unchanged — and bring a
// member's answer back unchanged. Every transformation on the way is a silent
// rewrite of a tenant's data: HTML escaping turns "<" into six bytes, and
// encoding/json compacts a json.RawMessage, dropping the insignificant
// whitespace a stored payload may well carry (a processor with nothing to change
// answers with the entity it was given, and the owner persists that answer).
func TestHandOver_DeliversTheEntityByteForByte(t *testing.T) {
	cases := []struct {
		name   string
		entity []byte
	}{
		{"the characters HTML escaping would rewrite", []byte(`{"t":"a<b>c&d"}`)},
		{"a non-BMP character, an accent, and a line separator", []byte("{\"t\":\"\U0001D11E \u00e9 \u2028\"}")},
		{"insignificant whitespace, as a stored payload may carry it", []byte("{\n  \"t\": \"a<b>c&d\",\n  \"n\": 1\n}")},
		{"a deliberately awkward mix", []byte("{\n\t\"t\" : \"<&>\U0001D11E\",\n\t\"n\" : [1, 2]\n}")},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, back := handOverEntity(t, tt.entity, nil)
			if !bytes.Equal(got, tt.entity) {
				t.Errorf("the compute member was given\n  %q\nwant\n  %q", got, tt.entity)
			}
			if !bytes.Equal(back, tt.entity) {
				t.Errorf("the answer brought back\n  %q\nwant\n  %q", back, tt.entity)
			}
		})
	}
}

// A member's own answer travels back byte for byte too, whatever it contains.
func TestHandOver_BringsTheMembersAnswerBackByteForByte(t *testing.T) {
	answer := []byte("{\n  \"out\": \"<&>\U0001D11E\",\n  \"n\": 1\n}")
	_, back := handOverEntity(t, []byte(`{"in":1}`), answer)
	if !bytes.Equal(back, answer) {
		t.Errorf("the answer came back\n  %q\nwant\n  %q", back, answer)
	}
}

// Every node-to-node body is encoded with HTML escaping off. Left on, a tenant's
// text in any field — a criterion, a function's result, a compute member's
// message — is multiplied by six wherever it holds "<", ">" or "&", and the
// envelope ceiling is derived without that multiplier.
func TestEncodePeerBody_DoesNotEscapeForHTML(t *testing.T) {
	body, err := dispatch.EncodePeerBody(struct {
		Text string `json:"text"`
	}{Text: "a<b>c&d"})
	if err != nil {
		t.Fatalf("EncodePeerBody: %v", err)
	}
	if !bytes.Contains(body, []byte("a<b>c&d")) {
		t.Errorf("the body escaped what needs no escaping between two nodes: %s", body)
	}
	// The six bytes json.Marshal writes for "<", spelled as bytes so that
	// nothing can rewrite the assertion itself.
	if escaped := []byte{92, 'u', '0', '0', '3', 'c'}; bytes.Contains(body, escaped) {
		t.Errorf("the body still HTML-escapes: %s", body)
	}
}

// The envelope of the largest storable entity fits the ceiling with the
// headroom the ceiling is derived to leave — the property N1 found the fixed
// ceiling did not have, stated as arithmetic rather than as a hand-over.
func TestMaxEnvelopeSize_HoldsTheLargestStorableEntity(t *testing.T) {
	onWire := ((maxStorableEntity + 2) / 3) * 4 // base64: 4 bytes out per 3 in
	if onWire >= dispatch.MaxEnvelopeSize {
		t.Fatalf("base64 of a %d-byte entity is %d bytes and the envelope holds %d", maxStorableEntity, onWire, dispatch.MaxEnvelopeSize)
	}
	if head := dispatch.MaxEnvelopeSize - onWire; head < 256*1024 {
		t.Errorf("headroom for the meta, the definition, the roles and the tags is %d bytes", head)
	}
}

// An entity at the API's own size limit can be handed over. The ceiling is
// derived from that limit and what the wire does to it, so a payload the API
// stores must never make a callout that no peer can take while a local compute
// member serves the same callout — I1's whole point. The payload here is the
// one character the wire used to inflate sixfold, which is why padding with
// "x" could not see this.
func TestHandOver_AnEntityAtTheStorableLimitIsSent(t *testing.T) {
	entity := []byte(`{"t":"` + strings.Repeat("<", maxStorableEntity-len(`{"t":""}`)) + `"}`)
	if len(entity) != maxStorableEntity {
		t.Fatalf("the test's entity is %d bytes, want %d", len(entity), maxStorableEntity)
	}

	auth := newTestPeerAuth(t)
	runner := &echoRunner{answer: []byte(`{"ok":1}`)}
	mux := http.NewServeMux()
	dispatch.NewDispatchHandler(runner, auth).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req := makeProcessorReq()
	req.Entity = entity
	req.RequestID, req.TriesLeft, req.AnswerLimitMs, req.OwnerNodeID, req.Major = "rid-max", 1, 1000, "node-owner", 1

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 5*time.Second).AllowLoopbackForTesting()
	resp, err := f.ForwardCallout(context.Background(), testNodeID, srv.URL, req)
	if err != nil {
		t.Fatalf("an entity at the storable limit could not be handed over: %v", err)
	}
	if resp.Outcome != dispatch.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", resp.Outcome)
	}
	if !bytes.Equal(runner.got, entity) {
		t.Error("the entity at the storable limit did not arrive byte for byte")
	}
}
