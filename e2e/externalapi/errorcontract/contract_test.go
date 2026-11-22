package errorcontract_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cyoda/cyoda-go/e2e/externalapi/errorcontract"
)

func TestMatch_StatusAndErrorCode_RFC9457Body(t *testing.T) {
	body := []byte(`{
		"type":"about:blank","title":"Conflict","status":409,
		"detail":"already locked","instance":"/api/model/x/1/lock",
		"properties":{"errorCode":"MODEL_ALREADY_LOCKED","retryable":false}
	}`)
	errorcontract.Match(t, 409, body, errorcontract.ExpectedError{
		HTTPStatus: 409,
		ErrorCode:  "MODEL_ALREADY_LOCKED",
	})
}

func TestMatch_StatusMismatch_Fails(t *testing.T) {
	ft := &fakeT{}
	body := []byte(`{"type":"about:blank","title":"Bad Request","status":400,
		"properties":{"errorCode":"BAD"}}`)
	errorcontract.Match(ft, 400, body, errorcontract.ExpectedError{
		HTTPStatus: 409, ErrorCode: "BAD",
	})
	if !ft.failed {
		t.Fatal("expected Match to fail on status mismatch")
	}
	if !containsMsg(ft.msgs, "http_status mismatch") {
		t.Fatalf("expected message containing %q; got %v", "http_status mismatch", ft.msgs)
	}
}

func TestMatch_ErrorCodeMismatch_Fails(t *testing.T) {
	ft := &fakeT{}
	body := []byte(`{"type":"about:blank","status":400,
		"properties":{"errorCode":"GOT"}}`)
	errorcontract.Match(ft, 400, body, errorcontract.ExpectedError{
		HTTPStatus: 400, ErrorCode: "WANT",
	})
	if !ft.failed {
		t.Fatal("expected Match to fail on errorCode mismatch")
	}
	if !containsMsg(ft.msgs, "error_code mismatch") {
		t.Fatalf("expected message containing %q; got %v", "error_code mismatch", ft.msgs)
	}
}

func TestMatch_EmptyErrorCode_SkipsAssertion(t *testing.T) {
	body := []byte(`{"type":"about:blank","status":400,
		"properties":{"errorCode":"ANY"}}`)
	// Empty ErrorCode means "don't assert error_code".
	errorcontract.Match(t, 400, body, errorcontract.ExpectedError{HTTPStatus: 400})
}

func TestMatch_NilFields_SkipsFieldAssertion(t *testing.T) {
	body := []byte(`{"type":"about:blank","status":400,
		"properties":{"errorCode":"VALIDATION_FAILED",
			"fields":[{"path":"$.x","value":1,"entityName":"m","entityVersion":1}]}}`)
	errorcontract.Match(t, 400, body, errorcontract.ExpectedError{
		HTTPStatus: 400, ErrorCode: "VALIDATION_FAILED",
	})
}

func TestMatch_FieldsPresent_Asserted(t *testing.T) {
	body := []byte(`{"type":"about:blank","status":400,
		"properties":{"errorCode":"VALIDATION_FAILED",
			"fields":[{"path":"$.age","value":"abc","entityName":"family","entityVersion":1}]}}`)
	errorcontract.Match(t, 400, body, errorcontract.ExpectedError{
		HTTPStatus: 400, ErrorCode: "VALIDATION_FAILED",
		Fields: []errorcontract.ErrorField{
			{Path: "$.age", Value: "abc", EntityName: "family", EntityVersion: 1},
		},
	})
}

func TestMatch_MalformedBody_Fails(t *testing.T) {
	ft := &fakeT{}
	errorcontract.Match(ft, 500, []byte("not-json"), errorcontract.ExpectedError{
		HTTPStatus: 500, ErrorCode: "ANY",
	})
	if !ft.failed {
		t.Fatal("expected Match to fail on malformed body")
	}
	if !containsMsg(ft.msgs, "not valid JSON") {
		t.Fatalf("expected message containing %q; got %v", "not valid JSON", ft.msgs)
	}
}

func TestSchemaJSON_IsValidJSON(t *testing.T) {
	b, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatalf("read schema.json: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("schema.json is not valid JSON: %v", err)
	}
	if v["$schema"] == nil {
		t.Fatal("schema.json missing $schema key")
	}
}

func TestMatch_FieldsWrongLength_Fails(t *testing.T) {
	ft := &fakeT{}
	// body has 1 field; want expects 2.
	body := []byte(`{"type":"about:blank","status":400,
		"properties":{"errorCode":"VALIDATION_FAILED",
			"fields":[{"path":"$.x","value":1,"entityName":"m","entityVersion":1}]}}`)
	errorcontract.Match(ft, 400, body, errorcontract.ExpectedError{
		HTTPStatus: 400, ErrorCode: "VALIDATION_FAILED",
		Fields: []errorcontract.ErrorField{
			{Path: "$.x", Value: 1, EntityName: "m", EntityVersion: 1},
			{Path: "$.y", Value: 2, EntityName: "m", EntityVersion: 1},
		},
	})
	if !ft.failed {
		t.Fatal("expected Match to fail on fields length mismatch")
	}
	if !containsMsg(ft.msgs, "fields length") {
		t.Fatalf("expected message containing %q; got %v", "fields length", ft.msgs)
	}
}

func TestMatch_FieldContentMismatch_Fails(t *testing.T) {
	ft := &fakeT{}
	// body has path "$.x"; want expects "$.y".
	body := []byte(`{"type":"about:blank","status":400,
		"properties":{"errorCode":"VALIDATION_FAILED",
			"fields":[{"path":"$.x","value":1,"entityName":"m","entityVersion":1}]}}`)
	errorcontract.Match(ft, 400, body, errorcontract.ExpectedError{
		HTTPStatus: 400, ErrorCode: "VALIDATION_FAILED",
		Fields: []errorcontract.ErrorField{
			{Path: "$.y", Value: 1, EntityName: "m", EntityVersion: 1},
		},
	})
	if !ft.failed {
		t.Fatal("expected Match to fail on fields[0].path mismatch")
	}
	if !containsMsg(ft.msgs, "fields[0].path") {
		t.Fatalf("expected message containing %q; got %v", "fields[0].path", ft.msgs)
	}
}

func TestMatch_FieldEntityVersionMismatch_Fails(t *testing.T) {
	ft := &fakeT{}
	// body has entityVersion 1; want expects 2.
	body := []byte(`{"type":"about:blank","status":400,
		"properties":{"errorCode":"VALIDATION_FAILED",
			"fields":[{"path":"$.x","value":1,"entityName":"m","entityVersion":1}]}}`)
	errorcontract.Match(ft, 400, body, errorcontract.ExpectedError{
		HTTPStatus: 400, ErrorCode: "VALIDATION_FAILED",
		Fields: []errorcontract.ErrorField{
			{Path: "$.x", Value: 1, EntityName: "m", EntityVersion: 2},
		},
	})
	if !ft.failed {
		t.Fatal("expected Match to fail on fields[0].entityVersion mismatch")
	}
	if !containsMsg(ft.msgs, "fields[0].entityVersion") {
		t.Fatalf("expected message containing %q; got %v", "fields[0].entityVersion", ft.msgs)
	}
}

// containsMsg reports whether any element of msgs contains sub.
func containsMsg(msgs []string, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// fakeT captures failure without the real testing harness aborting.
type fakeT struct {
	failed bool
	msgs   []string
}

func (f *fakeT) Errorf(format string, args ...any) {
	f.failed = true
	f.msgs = append(f.msgs, fmt.Sprintf(format, args...))
}
func (f *fakeT) Fatalf(format string, args ...any) {
	f.failed = true
	f.msgs = append(f.msgs, fmt.Sprintf(format, args...))
}
func (f *fakeT) Helper() {}
