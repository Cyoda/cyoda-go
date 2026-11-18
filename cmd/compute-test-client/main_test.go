package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cepb "github.com/cyoda/cyoda-go/api/grpc/cloudevents"
)

// TestCatalog_NoopReturnsEntityUnchanged verifies the noop processor
// dispatches without modifying the entity.
func TestCatalog_NoopReturnsEntityUnchanged(t *testing.T) {
	cat := newCatalog(nil, nil)
	proc, ok := cat.processor("noop")
	if !ok {
		t.Fatal("noop processor not registered")
	}
	in := &Entity{
		ID:   "ent-1",
		Data: json.RawMessage(`{"x":1}`),
	}
	out, err := proc(context.Background(), in, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("noop returned error: %v", err)
	}
	if string(out.Data) != string(in.Data) {
		t.Errorf("noop changed data: got %s, want %s", string(out.Data), string(in.Data))
	}
}

// TestCatalog_AlwaysTrueReturnsTrue verifies the always-true criterion.
func TestCatalog_AlwaysTrueReturnsTrue(t *testing.T) {
	cat := newCatalog(nil, nil)
	crit, ok := cat.criterion("always-true")
	if !ok {
		t.Fatal("always-true criterion not registered")
	}
	got, err := crit(context.Background(), &Entity{ID: "ent-1"}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("always-true returned error: %v", err)
	}
	if !got {
		t.Error("always-true returned false")
	}
}

// TestCatalog_AlwaysFalseReturnsFalse verifies the always-false criterion.
func TestCatalog_AlwaysFalseReturnsFalse(t *testing.T) {
	cat := newCatalog(nil, nil)
	crit, ok := cat.criterion("always-false")
	if !ok {
		t.Fatal("always-false criterion not registered")
	}
	got, err := crit(context.Background(), &Entity{ID: "ent-1"}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("always-false returned error: %v", err)
	}
	if got {
		t.Error("always-false returned true")
	}
}

// TestCatalog_TagWithFoo verifies tag-with-foo adds tag:"foo" to entity data.
func TestCatalog_TagWithFoo(t *testing.T) {
	cat := newCatalog(nil, nil)
	proc, ok := cat.processor("tag-with-foo")
	if !ok {
		t.Fatal("tag-with-foo processor not registered")
	}
	in := &Entity{
		ID:   "ent-2",
		Data: json.RawMessage(`{"x":1}`),
	}
	out, err := proc(context.Background(), in, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("tag-with-foo returned error: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(out.Data, &data); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if data["tag"] != "foo" {
		t.Errorf("tag-with-foo: got tag=%v, want foo", data["tag"])
	}
}

// TestCatalog_BumpAmount verifies bump-amount increments data.amount by 1.
func TestCatalog_BumpAmount(t *testing.T) {
	cat := newCatalog(nil, nil)
	proc, ok := cat.processor("bump-amount")
	if !ok {
		t.Fatal("bump-amount processor not registered")
	}
	in := &Entity{
		ID:   "ent-3",
		Data: json.RawMessage(`{"amount":5}`),
	}
	out, err := proc(context.Background(), in, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("bump-amount returned error: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(out.Data, &data); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if data["amount"] != float64(6) {
		t.Errorf("bump-amount: got amount=%v, want 6", data["amount"])
	}
}

// TestCatalog_InjectError verifies inject-error returns a non-nil error.
func TestCatalog_InjectError(t *testing.T) {
	cat := newCatalog(nil, nil)
	proc, ok := cat.processor("inject-error")
	if !ok {
		t.Fatal("inject-error processor not registered")
	}
	_, err := proc(context.Background(), &Entity{ID: "ent-4", Data: json.RawMessage(`{}`)}, json.RawMessage(`{}`))
	if err == nil {
		t.Error("inject-error returned nil error, want non-nil")
	}
}

// TestCatalog_SlowConfigurableZeroMS verifies slow-configurable with 0ms returns immediately.
func TestCatalog_SlowConfigurableZeroMS(t *testing.T) {
	cat := newCatalog(nil, nil)
	proc, ok := cat.processor("slow-configurable")
	if !ok {
		t.Fatal("slow-configurable processor not registered")
	}
	in := &Entity{ID: "ent-5", Data: json.RawMessage(`{}`)}
	out, err := proc(context.Background(), in, json.RawMessage(`{"sleep_ms":0}`))
	if err != nil {
		t.Fatalf("slow-configurable returned error: %v", err)
	}
	if out.ID != in.ID {
		t.Errorf("slow-configurable changed entity ID: got %s, want %s", out.ID, in.ID)
	}
}

// TestCatalog_SetField verifies set-field sets the specified field.
func TestCatalog_SetField(t *testing.T) {
	cat := newCatalog(nil, nil)
	proc, ok := cat.processor("set-field")
	if !ok {
		t.Fatal("set-field processor not registered")
	}
	in := &Entity{ID: "ent-6", Data: json.RawMessage(`{"x":1}`)}
	cfg := json.RawMessage(`{"field":"status","value":"active"}`)
	out, err := proc(context.Background(), in, cfg)
	if err != nil {
		t.Fatalf("set-field returned error: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(out.Data, &data); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if data["status"] != "active" {
		t.Errorf("set-field: got status=%v, want active", data["status"])
	}
}

// TestCatalog_AmountGt100 verifies amount-gt-100 returns true for 200 and false for 50.
func TestCatalog_AmountGt100(t *testing.T) {
	cat := newCatalog(nil, nil)
	crit, ok := cat.criterion("amount-gt-100")
	if !ok {
		t.Fatal("amount-gt-100 criterion not registered")
	}

	got, err := crit(context.Background(), &Entity{ID: "ent-7", Data: json.RawMessage(`{"amount":200}`)}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("amount-gt-100 returned error: %v", err)
	}
	if !got {
		t.Error("amount-gt-100: got false for amount=200, want true")
	}

	got, err = crit(context.Background(), &Entity{ID: "ent-8", Data: json.RawMessage(`{"amount":50}`)}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("amount-gt-100 returned error: %v", err)
	}
	if got {
		t.Error("amount-gt-100: got true for amount=50, want false")
	}
}

// TestCatalog_FieldEquals verifies field-equals returns true for matching and false for non-matching.
func TestCatalog_FieldEquals(t *testing.T) {
	cat := newCatalog(nil, nil)
	crit, ok := cat.criterion("field-equals")
	if !ok {
		t.Fatal("field-equals criterion not registered")
	}

	cfg := json.RawMessage(`{"field":"color","value":"blue"}`)
	got, err := crit(context.Background(), &Entity{ID: "ent-9", Data: json.RawMessage(`{"color":"blue"}`)}, cfg)
	if err != nil {
		t.Fatalf("field-equals returned error: %v", err)
	}
	if !got {
		t.Error("field-equals: got false for matching value, want true")
	}

	got, err = crit(context.Background(), &Entity{ID: "ent-10", Data: json.RawMessage(`{"color":"red"}`)}, cfg)
	if err != nil {
		t.Fatalf("field-equals returned error: %v", err)
	}
	if got {
		t.Error("field-equals: got true for non-matching value, want false")
	}
}

// TestCatalog_EchoContextToField verifies the pass-through Context (delivered
// as a JSON-string in parameters) is recorded into _context.
func TestCatalog_EchoContextToField(t *testing.T) {
	cat := newCatalog(nil, nil)
	proc, ok := cat.processor("echo-context-to-field")
	if !ok {
		t.Fatal("echo-context-to-field processor not registered")
	}

	// Context present: a JSON-string config like `"premium"` ends up in _context.
	out, err := proc(context.Background(), &Entity{ID: "e-1", Data: json.RawMessage(`{"k":1}`)}, json.RawMessage(`"premium"`))
	if err != nil {
		t.Fatalf("echo-context-to-field: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(out.Data, &data); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if data["_context"] != "premium" {
		t.Errorf("expected _context=premium, got %v", data["_context"])
	}

	// Context absent: no _context field written.
	out2, err := proc(context.Background(), &Entity{ID: "e-2", Data: json.RawMessage(`{"k":1}`)}, nil)
	if err != nil {
		t.Fatalf("echo-context-to-field (empty config): %v", err)
	}
	var data2 map[string]any
	if err := json.Unmarshal(out2.Data, &data2); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if _, ok := data2["_context"]; ok {
		t.Error("expected no _context field when config is empty")
	}

	// Malformed (non-string JSON) surfaces as an error.
	if _, err := proc(context.Background(), &Entity{ID: "e-3", Data: json.RawMessage(`{"k":1}`)}, json.RawMessage(`{"role":"premium"}`)); err == nil {
		t.Error("expected error for non-string JSON config")
	}
}

// TestCatalog_CallbackProcessorFailsWhenNoCB verifies that a cb-* callback
// processor returns the "callback client unavailable" error when the catalog
// is constructed with a nil HTTP callback client.
func TestCatalog_CallbackProcessorFailsWhenNoCB(t *testing.T) {
	cat := newCatalog(nil, nil)
	proc, ok := cat.callbackProcessor("cb-create-secondary")
	if !ok {
		t.Fatal("cb-create-secondary callback processor not registered")
	}
	entity := &Entity{ID: "ent-cb", Data: []byte(`{}`)}
	_, err := proc(context.Background(), entity, cbConfig{}, "", nil)
	if err == nil {
		t.Fatal("expected error for nil callback client, got nil")
	}
	const want = "callback client unavailable"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err.Error(), want)
	}
}

// TestCatalog_ContextEquals verifies the context-equals criterion matches only
// when the pass-through Context equals the literal "match".
func TestCatalog_ContextEquals(t *testing.T) {
	cat := newCatalog(nil, nil)
	crit, ok := cat.criterion("context-equals")
	if !ok {
		t.Fatal("context-equals criterion not registered")
	}

	got, err := crit(context.Background(), &Entity{ID: "e-1"}, json.RawMessage(`"match"`))
	if err != nil {
		t.Fatalf("context-equals match: %v", err)
	}
	if !got {
		t.Error("context-equals: expected true for context=\"match\"")
	}

	got, err = crit(context.Background(), &Entity{ID: "e-2"}, json.RawMessage(`"nope"`))
	if err != nil {
		t.Fatalf("context-equals nope: %v", err)
	}
	if got {
		t.Error("context-equals: expected false for context=\"nope\"")
	}

	// No context: false.
	got, err = crit(context.Background(), &Entity{ID: "e-3"}, nil)
	if err != nil {
		t.Fatalf("context-equals nil: %v", err)
	}
	if got {
		t.Error("context-equals: expected false when context absent")
	}
}

// TestCatalog_SchedFnResolve verifies every schedMode "sched-fn-resolve"
// serves for the e2e/parity/scheduledfunction parity scenarios: it must
// read entity.Data (funcSchedData) and return the matching resolveSchedule
// Schedule result — see catalog.go's "sched-fn-resolve" entry doc comment.
func TestCatalog_SchedFnResolve(t *testing.T) {
	cat := newCatalog(nil, nil)
	fn, ok := cat.function("sched-fn-resolve")
	if !ok {
		t.Fatal("sched-fn-resolve function not registered")
	}

	call := func(t *testing.T, data string) map[string]any {
		t.Helper()
		ce := &cepb.CloudEvent{Attributes: map[string]*cepb.CloudEvent_CloudEventAttributeValue{
			"time": {Attr: &cepb.CloudEvent_CloudEventAttributeValue_CeTimestamp{CeTimestamp: timestamppb.Now()}},
		}}
		_, result, err := fn(withRequestTime(context.Background(), ce), &Entity{ID: "e-1", Data: json.RawMessage(data)}, nil)
		if err != nil {
			t.Fatalf("sched-fn-resolve(%s): %v", data, err)
		}
		return result
	}

	t.Run("absolute default offset", func(t *testing.T) {
		before := time.Now().UnixMilli()
		result := call(t, `{"schedMode":"absolute","offsetMs":0,"expireOffsetMs":0}`)
		after := time.Now().UnixMilli()
		fireAt, ok := result["fireAt"].(int64)
		if !ok {
			t.Fatalf("expected fireAt int64, got %T: %v", result["fireAt"], result["fireAt"])
		}
		if fireAt < before+300 || fireAt > after+300 {
			t.Errorf("fireAt=%d not in [%d,%d] (now+300ms default)", fireAt, before+300, after+300)
		}
		if _, present := result["fireAfterMs"]; present {
			t.Error("absolute mode must not set fireAfterMs")
		}
	})

	t.Run("absolute explicit offset", func(t *testing.T) {
		before := time.Now().UnixMilli()
		result := call(t, `{"schedMode":"absolute","offsetMs":1000,"expireOffsetMs":0}`)
		after := time.Now().UnixMilli()
		fireAt := result["fireAt"].(int64)
		if fireAt < before+1000 || fireAt > after+1000 {
			t.Errorf("fireAt=%d not in [%d,%d]", fireAt, before+1000, after+1000)
		}
	})

	t.Run("relative default offset", func(t *testing.T) {
		result := call(t, `{"schedMode":"relative","offsetMs":0,"expireOffsetMs":0}`)
		if got := result["fireAfterMs"]; got != int64(300) {
			t.Errorf("fireAfterMs=%v, want 300 (default)", got)
		}
		if _, present := result["fireAt"]; present {
			t.Error("relative mode must not set fireAt")
		}
	})

	t.Run("relative explicit offset", func(t *testing.T) {
		result := call(t, `{"schedMode":"relative","offsetMs":500,"expireOffsetMs":0}`)
		if got := result["fireAfterMs"]; got != int64(500) {
			t.Errorf("fireAfterMs=%v, want 500", got)
		}
	})

	t.Run("bornExpired default offset", func(t *testing.T) {
		result := call(t, `{"schedMode":"bornExpired","offsetMs":0,"expireOffsetMs":0}`)
		if got := result["fireAfterMs"]; got != int64(1000) {
			t.Errorf("fireAfterMs=%v, want 1000 (default)", got)
		}
		if got := result["expireAfterMs"]; got != int64(0) {
			t.Errorf("expireAfterMs=%v, want 0 (born expired: expiry == fire time)", got)
		}
	})

	t.Run("pastFire default offset", func(t *testing.T) {
		before := time.Now().UnixMilli()
		result := call(t, `{"schedMode":"pastFire","offsetMs":0,"expireOffsetMs":0}`)
		after := time.Now().UnixMilli()
		fireAt := result["fireAt"].(int64)
		if fireAt < before-5000 || fireAt > after-5000 {
			t.Errorf("fireAt=%d not in [%d,%d] (now-5000ms default, already due)", fireAt, before-5000, after-5000)
		}
	})

	t.Run("expiryElapsed default offsets", func(t *testing.T) {
		before := time.Now().UnixMilli()
		result := call(t, `{"schedMode":"expiryElapsed","offsetMs":0,"expireOffsetMs":0}`)
		after := time.Now().UnixMilli()
		fireAt, ok := result["fireAt"].(int64)
		if !ok {
			t.Fatalf("expected fireAt int64, got %T: %v", result["fireAt"], result["fireAt"])
		}
		if fireAt < before-5000 || fireAt > after-5000 {
			t.Errorf("fireAt=%d not in [%d,%d] (now-5000ms default)", fireAt, before-5000, after-5000)
		}
		expireAt, ok := result["expireAt"].(int64)
		if !ok {
			t.Fatalf("expected expireAt int64, got %T: %v", result["expireAt"], result["expireAt"])
		}
		if want := fireAt + 110; expireAt != want {
			t.Errorf("expireAt=%d, want fireAt+110=%d (expiry > fire time, but lateness already vast)", expireAt, want)
		}
	})

	t.Run("unknown schedMode errors", func(t *testing.T) {
		_, _, err := fn(context.Background(), &Entity{ID: "e-1", Data: json.RawMessage(`{"schedMode":"bogus"}`)}, nil)
		if err == nil {
			t.Fatal("expected an error for an unrecognized schedMode")
		}
	})
}

// TestCatalog_CreateSecondaryRecord_RecordsTheAnswerAndSucceeds: the processor
// makes a joined create of the secondary and records the callback's status and
// body into the primary's data, and succeeds whatever the callback answered —
// so the owner goes on to commit, and a scenario can assert both the refusal
// and what the owner's commit leaves behind.
func TestCatalog_CreateSecondaryRecord_RecordsTheAnswerAndSucceeds(t *testing.T) {
	const refusal = `{"status":409,"properties":{"errorCode":"COMMIT_IN_JOINED_TRANSACTION"}}`
	var gotToken, gotPath string
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken, gotPath = r.Header.Get("X-Tx-Token"), r.URL.Path
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(refusal))
	}))
	defer door.Close()

	cat := newCatalog(nil, nil)
	proc, ok := cat.callbackProcessor("cb-create-secondary-record")
	if !ok {
		t.Fatal("cb-create-secondary-record callback processor not registered")
	}
	out, err := proc(context.Background(), &Entity{ID: "ent", Data: []byte(`{"name":"p"}`)},
		cbConfig{SecondaryModel: "sec", SecondaryVersion: 1, Marker: "m"}, "the-pass", newCallbackClient(door.URL, staticToken("bearer")))
	if err != nil {
		t.Fatalf("processor must succeed whatever the callback answered: %v", err)
	}
	if gotToken != "the-pass" || gotPath != "/api/entity/JSON/sec/1" {
		t.Fatalf("callback token=%q path=%q; want the pass on a joined create of sec/1", gotToken, gotPath)
	}
	var data map[string]any
	if err := json.Unmarshal(out.Data, &data); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if data["hopStatus"] != float64(http.StatusConflict) || data["hopBody"] != refusal || data["tokenWasEmpty"] != false {
		t.Fatalf("recorded data = %+v; want hopStatus 409, the body, tokenWasEmpty false", data)
	}
}

// targetCall is one call a targetDoor received.
type targetCall struct{ token, method, path, query, body string }

const targetNotFound = `{"status":404,"properties":{"errorCode":"ENTITY_NOT_FOUND"}}`

// targetDoor is a callback door that answers a GET of the target with its
// envelope, a PUT with a write answer naming tx-1, a by-transaction GET with
// a 404 and anything else (the record) with a create answer. It records every
// call.
func targetDoor(calls *[]targetCall) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*calls = append(*calls, targetCall{r.Header.Get("X-Tx-Token"), r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		switch {
		case r.Method == http.MethodGet && r.URL.RawQuery != "":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(targetNotFound))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"meta":{"id":"tgt"},"data":{"name":"t","status":"new"}}`))
		case r.Method == http.MethodPut:
			_, _ = w.Write([]byte(`{"transactionId":"tx-1","entityIds":["tgt"]}`))
		default:
			_, _ = w.Write([]byte(`[{"transactionId":"tx-rec","entityIds":["rec"]}]`))
		}
	}))
}

// TestCatalog_WriteTarget_WritesTheTargetThroughTheCallback: the processor
// reads the entity named by targetId through a joined GET and writes it back
// through a joined update with status set to the marker; the anchor is
// returned unchanged and never written.
func TestCatalog_WriteTarget_WritesTheTargetThroughTheCallback(t *testing.T) {
	var calls []targetCall
	door := targetDoor(&calls)
	defer door.Close()

	proc, ok := newCatalog(nil, nil).callbackProcessor("cb-write-target")
	if !ok {
		t.Fatal("cb-write-target callback processor not registered")
	}
	anchor := &Entity{ID: "ent", Data: []byte(`{"name":"p","targetId":"tgt"}`)}
	out, err := proc(context.Background(), anchor, cbConfig{Marker: "written"}, "the-pass", newCallbackClient(door.URL, staticToken("bearer")))
	if err != nil {
		t.Fatalf("processor: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %+v; want a joined read and a joined update of the target", calls)
	}
	if c := calls[0]; c.token != "the-pass" || c.method != http.MethodGet || c.path != "/api/entity/tgt" {
		t.Fatalf("first call = %+v; want the joined read of tgt", c)
	}
	var data map[string]any
	if c := calls[1]; c.token != "the-pass" || c.method != http.MethodPut || c.path != "/api/entity/JSON/tgt" ||
		json.Unmarshal([]byte(c.body), &data) != nil || data["status"] != "written" || data["name"] != "t" {
		t.Fatalf("second call = %+v; want the joined update of tgt with its data and status set to the marker", c)
	}
	if out != anchor {
		t.Fatalf("result = %+v; want the anchor unchanged", out)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer failing.Close()
	if _, err := proc(context.Background(), anchor, cbConfig{Marker: "written"}, "the-pass", newCallbackClient(failing.URL, staticToken("bearer"))); err == nil {
		t.Fatal("a refused read or write must fail the processor")
	}
	if _, err := proc(context.Background(), &Entity{ID: "ent", Data: []byte(`{}`)}, cbConfig{Marker: "written"}, "the-pass", newCallbackClient(door.URL, staticToken("bearer"))); err == nil {
		t.Fatal("an anchor with no targetId must fail the processor")
	}
}

// TestCatalog_WriteTargetReadByTx_ReadsTheJoinedTransactionAndRecords: the
// processor writes the target through a joined update, reads the target
// through a joined GET with ?transactionId= naming the transaction the update
// answered, records that read's answer outside the transaction, and returns
// the anchor unchanged.
func TestCatalog_WriteTargetReadByTx_ReadsTheJoinedTransactionAndRecords(t *testing.T) {
	var calls []targetCall
	door := targetDoor(&calls)
	defer door.Close()

	proc, ok := newCatalog(nil, nil).callbackProcessor("cb-write-target-read-by-tx")
	if !ok {
		t.Fatal("cb-write-target-read-by-tx callback processor not registered")
	}
	anchor := &Entity{ID: "ent", Data: []byte(`{"name":"p","targetId":"tgt"}`)}
	out, err := proc(context.Background(), anchor, cbConfig{Marker: "m", RecordModel: "rec"}, "the-pass", newCallbackClient(door.URL, staticToken("bearer")))
	if err != nil {
		t.Fatalf("processor: %v", err)
	}
	if len(calls) != 4 {
		t.Fatalf("calls = %+v; want a joined read, a joined update, a joined by-transaction read and the record", calls)
	}
	if c := calls[1]; c.token != "the-pass" || c.method != http.MethodPut || c.path != "/api/entity/JSON/tgt" {
		t.Fatalf("second call = %+v; want the joined update of tgt", c)
	}
	if c := calls[2]; c.token != "the-pass" || c.method != http.MethodGet || c.path != "/api/entity/tgt" || c.query != "transactionId=tx-1" {
		t.Fatalf("third call = %+v; want a joined GET of tgt with transactionId=tx-1", c)
	}
	var rec map[string]any
	if c := calls[3]; c.token != "" || c.method != http.MethodPost || c.path != "/api/entity/JSON/rec/1" || json.Unmarshal([]byte(c.body), &rec) != nil {
		t.Fatalf("fourth call = %+v; want the record created outside the transaction", c)
	}
	if rec["marker"] != "m" || rec["hopStatus"] != float64(http.StatusNotFound) || rec["hopBody"] != targetNotFound {
		t.Fatalf("record = %+v; want marker m with the read's 404 and body", rec)
	}
	if out != anchor {
		t.Fatalf("result = %+v; want the anchor unchanged", out)
	}

	noTx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":{"name":"t"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"entityIds":["tgt"]}`))
	}))
	defer noTx.Close()
	if _, err := proc(context.Background(), anchor, cbConfig{Marker: "m", RecordModel: "rec"}, "the-pass", newCallbackClient(noTx.URL, staticToken("bearer"))); err == nil {
		t.Fatal("a write answer with no transactionId must fail the processor")
	}
}

// TestCatalog_MoveStatus_WritesEachMoveThroughTheTransaction: for each move
// on the anchor's data, in order, the processor reads the named entity through
// a joined GET and writes it back through a joined update with status set to
// the move's value; it answers with the anchor unchanged. A refused read or
// write fails the processor.
func TestCatalog_MoveStatus_WritesEachMoveThroughTheTransaction(t *testing.T) {
	type call struct{ token, method, path, body string }
	var calls []call
	stored := map[string]string{
		"/api/entity/a": `{"meta":{"id":"a"},"data":{"name":"a","status":"x"}}`,
		"/api/entity/b": `{"meta":{"id":"b"},"data":{"name":"b","status":"y"}}`,
	}
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, call{r.Header.Get("X-Tx-Token"), r.Method, r.URL.Path, string(b)})
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(stored[r.URL.Path]))
			return
		}
		_, _ = w.Write([]byte(`{"transactionId":"tx-1","entityIds":["a"]}`))
	}))
	defer door.Close()

	proc, ok := newCatalog(nil, nil).callbackProcessor("cb-move-status")
	if !ok {
		t.Fatal("cb-move-status callback processor not registered")
	}
	anchor := []byte(`{"name":"d","moves":[{"id":"a","status":"y"},{"id":"b","status":"x"}]}`)
	out, err := proc(context.Background(), &Entity{ID: "d", Data: anchor}, cbConfig{}, "the-pass", newCallbackClient(door.URL, staticToken("bearer")))
	if err != nil {
		t.Fatalf("processor: %v", err)
	}
	want := []struct{ method, path, status string }{
		{http.MethodGet, "/api/entity/a", ""},
		{http.MethodPut, "/api/entity/JSON/a", "y"},
		{http.MethodGet, "/api/entity/b", ""},
		{http.MethodPut, "/api/entity/JSON/b", "x"},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v; want a joined read and update of a, then of b", calls)
	}
	for i, w := range want {
		c := calls[i]
		if c.token != "the-pass" || c.method != w.method || c.path != w.path {
			t.Fatalf("call %d = %+v; want a joined %s %s", i, c, w.method, w.path)
		}
		if w.method != http.MethodPut {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(c.body), &data); err != nil || data["status"] != w.status || data["name"] == nil {
			t.Fatalf("call %d body = %s; want the entity's data with status %s", i, c.body, w.status)
		}
	}
	if out == nil || string(out.Data) != string(anchor) {
		t.Fatalf("result = %+v; want the anchor unchanged", out)
	}

	for _, refused := range []string{http.MethodGet, http.MethodPut} {
		failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == refused {
				w.WriteHeader(http.StatusConflict)
				return
			}
			_, _ = w.Write([]byte(stored["/api/entity/a"]))
		}))
		_, err := proc(context.Background(), &Entity{ID: "d", Data: anchor}, cbConfig{}, "the-pass", newCallbackClient(failing.URL, staticToken("bearer")))
		failing.Close()
		if err == nil {
			t.Fatalf("a refused %s must fail the processor", refused)
		}
	}
}

// An absolute schedule is computed from the request's time attribute, which is
// platform time, not from the test client's own clock.
func TestCatalog_SchedFnResolve_UsesRequestTime(t *testing.T) {
	cat := newCatalog(nil, nil)
	fn, _ := cat.function("sched-fn-resolve")
	at := time.Date(2015, 3, 1, 0, 0, 0, 0, time.UTC)
	ce := &cepb.CloudEvent{Attributes: map[string]*cepb.CloudEvent_CloudEventAttributeValue{
		"time": {Attr: &cepb.CloudEvent_CloudEventAttributeValue_CeTimestamp{CeTimestamp: timestamppb.New(at)}},
	}}
	ctx := withRequestTime(context.Background(), ce)

	_, result, err := fn(ctx, &Entity{ID: "e-1", Data: json.RawMessage(`{"schedMode":"absolute","offsetMs":1000}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := result["fireAt"], at.UnixMilli()+1000; got != want {
		t.Errorf("fireAt = %v, want %v", got, want)
	}

	// Without the attribute there is no time to compute from: an error.
	_, _, err = fn(withRequestTime(context.Background(), &cepb.CloudEvent{}), &Entity{ID: "e-1", Data: json.RawMessage(`{"schedMode":"absolute","offsetMs":1000}`)}, nil)
	if err == nil {
		t.Error("a request without a time attribute must fail")
	}
}

// anchorDoor is a callback door for an anchor "ent" of model "m": a GET of the
// anchor, a direct search, a grouped count and a page of m each answer with
// the anchor in READY with status "new" (the page and the search also list an
// entity "other"). It records every call.
func anchorDoor(calls *[]targetCall) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*calls = append(*calls, targetCall{r.Header.Get("X-Tx-Token"), r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/entity/ent":
			_, _ = w.Write([]byte(`{"meta":{"id":"ent","state":"READY"},"data":{"name":"a","status":"new"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/search/direct/m/1":
			_, _ = w.Write([]byte("{\"meta\":{\"id\":\"other\"},\"data\":{\"status\":\"new\"}}\n{\"meta\":{\"id\":\"ent\"},\"data\":{\"status\":\"new\"}}\n"))
		case r.Method == http.MethodPost && r.URL.Path == "/api/entity/stats/m/1/query":
			_, _ = w.Write([]byte(`[{"groupKey":[{"path":"$.status","value":"old"}],"count":4},{"groupKey":[{"path":"$.status","value":"new"}],"count":2}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/entity/m/1":
			_, _ = w.Write([]byte(`[{"meta":{"id":"other"},"data":{"status":"old"}},{"meta":{"id":"ent"},"data":{"status":"new"}}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestCatalog_ReadAnchor_RecordsEveryJoinedReadOfTheAnchor: cb-read-anchor
// reads its own entity through the joined GET, search, grouped count and page
// — every one carrying the token — and answers with the entity's data plus
// what each read saw of it. It writes nothing.
func TestCatalog_ReadAnchor_RecordsEveryJoinedReadOfTheAnchor(t *testing.T) {
	var calls []targetCall
	door := anchorDoor(&calls)
	defer door.Close()

	proc, ok := newCatalog(nil, nil).callbackProcessor("cb-read-anchor")
	if !ok {
		t.Fatal("cb-read-anchor callback processor not registered")
	}
	anchor := &Entity{ID: "ent", State: "READY", Data: []byte(`{"name":"a","status":"new"}`)}
	out, err := proc(context.Background(), anchor, cbConfig{AnchorModel: "m"}, "the-pass", newCallbackClient(door.URL, staticToken("bearer")))
	if err != nil {
		t.Fatalf("processor: %v", err)
	}
	if len(calls) != 4 {
		t.Fatalf("calls = %+v; want a GET, a search, a count and a page", calls)
	}
	for _, c := range calls {
		if c.token != "the-pass" {
			t.Errorf("call %s %s carried token %q; want the pass", c.method, c.path, c.token)
		}
		if c.method == http.MethodPut || c.method == http.MethodDelete || c.method == http.MethodPatch {
			t.Errorf("call %s %s writes; cb-read-anchor only reads", c.method, c.path)
		}
	}
	if !strings.Contains(calls[1].body, `"value":"new"`) {
		t.Errorf("search body = %s; want a condition on the anchor's current status", calls[1].body)
	}
	var data map[string]any
	if err := json.Unmarshal(out.Data, &data); err != nil {
		t.Fatalf("result data: %v", err)
	}
	want := map[string]any{
		"name": "a", "status": "new",
		"anchorGetStatus": float64(200), "anchorGetState": "READY", "anchorGetMarker": "new",
		"anchorSearchHits": float64(1), "anchorCountByMarker": float64(2), "anchorPageMarker": "new",
	}
	for k, v := range want {
		if data[k] != v {
			t.Errorf("%s = %v; want %v", k, data[k], v)
		}
	}

	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer missing.Close()
	out, err = proc(context.Background(), anchor, cbConfig{AnchorModel: "m"}, "the-pass", newCallbackClient(missing.URL, staticToken("bearer")))
	if err != nil {
		t.Fatalf("a refused read is recorded, not a failure: %v", err)
	}
	_ = json.Unmarshal(out.Data, &data)
	if data["anchorGetStatus"] != float64(404) || data["anchorGetMarker"] != "" || data["anchorSearchHits"] != float64(0) || data["anchorPageMarker"] != "" {
		t.Errorf("data = %+v; want the 404 recorded and nothing found", data)
	}
}

// TestCatalog_CriterionReadsAnchor_MatchesTheAnchorsCurrentStatus: the
// criterion GETs its own entity under the token and matches only when the
// read's data.status is the marker.
func TestCatalog_CriterionReadsAnchor_MatchesTheAnchorsCurrentStatus(t *testing.T) {
	var calls []targetCall
	door := anchorDoor(&calls)
	defer door.Close()

	crit, ok := newCatalog(nil, nil).callbackCriterion("cb-criterion-reads-anchor")
	if !ok {
		t.Fatal("cb-criterion-reads-anchor callback criterion not registered")
	}
	anchor := &Entity{ID: "ent", Data: []byte(`{"status":"new"}`)}
	cb := newCallbackClient(door.URL, staticToken("bearer"))
	if m, err := crit(context.Background(), anchor, cbConfig{Marker: "new"}, "the-pass", cb); err != nil || !m {
		t.Fatalf("marker new: matches=%v err=%v; want a match", m, err)
	}
	if c := calls[0]; c.token != "the-pass" || c.method != http.MethodGet || c.path != "/api/entity/ent" {
		t.Fatalf("call = %+v; want the joined GET of the anchor", c)
	}
	if m, err := crit(context.Background(), anchor, cbConfig{Marker: "old"}, "the-pass", cb); err != nil || m {
		t.Fatalf("marker old: matches=%v err=%v; want no match", m, err)
	}
	if m, err := crit(context.Background(), &Entity{ID: "gone"}, cbConfig{Marker: "new"}, "the-pass", cb); err != nil || m {
		t.Fatalf("anchor not found: matches=%v err=%v; want no match", m, err)
	}
}
