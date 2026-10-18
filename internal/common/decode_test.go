package common_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func TestDecodeBoundedJSON_Happy(t *testing.T) {
	type dst struct {
		X int `json:"x"`
	}
	r := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(`{"x":7}`)))
	w := httptest.NewRecorder()
	var d dst
	if err := common.DecodeBoundedJSON(w, r, 1<<10, &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.X != 7 {
		t.Errorf("got x=%d", d.X)
	}
}

func TestDecodeBoundedJSON_OverSize(t *testing.T) {
	type dst struct {
		X string `json:"x"`
	}
	big := strings.Repeat("a", 1<<20+1)
	r := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(`{"x":"`+big+`"}`)))
	w := httptest.NewRecorder()
	var d dst
	if err := common.DecodeBoundedJSON(w, r, 1<<20, &d); err == nil {
		t.Fatal("expected error")
	}
}

// TestDecodeJSON_RefusesInexactMemberNames pins the property every typed
// request body relies on: a member name must spell a field exactly, once.
// encoding/json (v1) matched names ignoring case (and Unicode folding, so
// "tranſitions" with U+017F was read as "transitions") and let a later
// duplicate overwrite or merge into an earlier one.
func TestDecodeJSON_RefusesInexactMemberNames(t *testing.T) {
	type transition struct {
		Manual bool `json:"manual"`
	}
	type body struct {
		Transitions []transition `json:"transitions"`
	}
	cases := []struct {
		name, in, want string
	}{
		{"case variant", `{"transitions":[{"Manual":true}]}`, `invalid JSON at "/transitions/0": unknown member name "Manual"`},
		{"unicode fold variant", `{"tran` + "\u017F" + `itions":[]}`, `invalid JSON at "": unknown member name "tranſitions"`},
		{"duplicate", `{"transitions":[{"manual":true,"manual":false}]}`, `invalid JSON at "/transitions/0": duplicate member name "manual"`},
		{"case-variant duplicate", `{"transitions":[],"TRANSITIONS":[]}`, `invalid JSON at "": unknown member name "TRANSITIONS"`},
		{"unknown", `{"transitions":[],"extra":1}`, `invalid JSON at "": unknown member name "extra"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b body
			err := common.DecodeJSON([]byte(tc.in), &b)
			if err == nil {
				t.Fatalf("accepted %s as %+v", tc.in, b)
			}
			if err.Error() != tc.want {
				t.Errorf("error\n got: %s\nwant: %s", err, tc.want)
			}
		})
	}
}

// TestDecodeJSON_ErrorNamesNoGoTypes: the error text goes into a 400 detail,
// so it names the JSON location and never a Go type or package.
func TestDecodeJSON_ErrorNamesNoGoTypes(t *testing.T) {
	type body struct {
		N int `json:"n"`
	}
	for in, want := range map[string]string{
		`{"n":"x"}`:                  `invalid JSON at "/n": a JSON string where an integer is expected`,
		`{"n":1.5}`:                  `invalid JSON at "/n": a JSON number where an integer is expected`,
		`{"n":99999999999999999999}`: `invalid JSON at "/n": a JSON number out of range`,
		`{"n":1e3}`:                  `invalid JSON at "/n": a JSON number where an integer is expected`,
		`{"n":[1]}`:                  `invalid JSON at "/n": a JSON array where an integer is expected`,
		`{"n":1} x`:                  `invalid JSON at offset 8: invalid character 'x' after top-level value`,
		`{"n":}`:                     `invalid JSON at offset 5: invalid character '}' at start of value`,
		``:                           `invalid JSON at offset 0: unexpected EOF`,
	} {
		var b body
		err := common.DecodeJSON([]byte(in), &b)
		if err == nil {
			t.Fatalf("accepted %q", in)
		}
		if err.Error() != want {
			t.Errorf("%q:\n got: %s\nwant: %s", in, err, want)
		}
	}
}

func TestDecodeJSON_Accepts(t *testing.T) {
	type body struct {
		Manual *bool `json:"manual"`
	}
	var b body
	if err := common.DecodeJSON([]byte(` {"manual":true} `), &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b.Manual == nil || !*b.Manual {
		t.Errorf("got %+v", b)
	}
}

func TestDecodeBoundedJSON_RefusesInexactMemberNames(t *testing.T) {
	type dst struct {
		X int `json:"x"`
	}
	r := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(`{"X":7}`)))
	w := httptest.NewRecorder()
	var d dst
	if err := common.DecodeBoundedJSON(w, r, 1<<10, &d); err == nil {
		t.Fatalf("accepted a case-variant name as %+v", d)
	}
}

func TestDecodeBoundedJSON_BadJSON(t *testing.T) {
	type dst struct{}
	r := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(`not-json`)))
	w := httptest.NewRecorder()
	var d dst
	if err := common.DecodeBoundedJSON(w, r, 1<<10, &d); err == nil {
		t.Fatal("expected error")
	}
}

// TestDecodeJSON_PreserveNumbers: with PreserveNumbers a JSON number held in
// an `any` — at any depth — keeps its literal as a json.Number, so an
// integer above 2^53 survives (float64 would round it).
func TestDecodeJSON_PreserveNumbers(t *testing.T) {
	type body struct {
		Params map[string]any `json:"params"`
		Any    any            `json:"any"`
	}
	var b body
	in := `{"params":{"big":9007199254740993,"list":[1.50,{"n":-0}]},"any":2e3}`
	if err := common.DecodeJSON([]byte(in), &b, common.PreserveNumbers); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := b.Params["big"]; got != json.Number("9007199254740993") {
		t.Errorf("big: got %#v", got)
	}
	list := b.Params["list"].([]any)
	if list[0] != json.Number("1.50") {
		t.Errorf("list[0]: got %#v", list[0])
	}
	if got := list[1].(map[string]any)["n"]; got != json.Number("-0") {
		t.Errorf("list[1].n: got %#v", got)
	}
	if b.Any != json.Number("2e3") {
		t.Errorf("any: got %#v", b.Any)
	}
}

// TestDecodeJSON_AllowUnknownMembers: an open shape (a probe that reads a
// few members, or a JWK, which RFC 7517 lets carry members of its own)
// ignores members it does not name, but a name still matches exactly and a
// duplicate is still refused.
func TestDecodeJSON_AllowUnknownMembers(t *testing.T) {
	type probe struct {
		Kty string `json:"kty"`
	}
	var p probe
	if err := common.DecodeJSON([]byte(`{"kty":"RSA","use":"sig"}`), &p, common.AllowUnknownMembers); err != nil || p.Kty != "RSA" {
		t.Fatalf("got %+v, %v", p, err)
	}
	p = probe{}
	if err := common.DecodeJSON([]byte(`{"KTY":"RSA"}`), &p, common.AllowUnknownMembers); err != nil || p.Kty != "" {
		t.Errorf("a case variant must not fill kty: got %+v, %v", p, err)
	}
	if err := common.DecodeJSON([]byte(`{"kty":"RSA","kty":"EC"}`), &p, common.AllowUnknownMembers); err == nil {
		t.Error("accepted a duplicate member name")
	}
}

// selfDecoding decodes itself through DecodeJSON, as a generated event type
// does from its UnmarshalJSON method.
type selfDecoding struct {
	Name string `json:"name"`
}

func (n *selfDecoding) UnmarshalJSON(b []byte) error {
	type plain selfDecoding
	var p plain
	if err := common.DecodeJSON(b, &p); err != nil {
		return err
	}
	*n = selfDecoding(p)
	return nil
}

type alwaysRequired struct{}

func (*alwaysRequired) UnmarshalJSON([]byte) error { return errors.New("field name: required") }

// TestDecodeJSON_NestedDecoderErrors: an error from a value that decodes
// itself names the whole path from the top-level value, and an error the
// method writes itself reaches the client unchanged.
func TestDecodeJSON_NestedDecoderErrors(t *testing.T) {
	var b struct {
		Items []selfDecoding `json:"items"`
	}
	err := common.DecodeJSON([]byte(`{"items":[{"name":"a"},{"Name":"b"}]}`), &b)
	if want := `invalid JSON at "/items/1": unknown member name "Name"`; err == nil || err.Error() != want {
		t.Errorf("got %v\nwant %s", err, want)
	}
	var r struct {
		Model alwaysRequired `json:"model"`
	}
	err = common.DecodeJSON([]byte(`{"model":{}}`), &r)
	if want := `invalid JSON at "/model": field name: required`; err == nil || err.Error() != want {
		t.Errorf("got %v\nwant %s", err, want)
	}
}
