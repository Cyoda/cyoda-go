package common_test

import (
	"bytes"
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
	err := common.DecodeBoundedJSON(w, r, 1<<20, &d)
	if err == nil {
		t.Fatal("expected error")
	}
	// The limit in the server's words, not net/http's.
	if want := "request body exceeds 1048576 bytes"; err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}
}

// TestDecodeBoundedJSON_RefusesInexactMemberNames: the bounded decoder is
// strictjson's (whose own tests cover the rule), not encoding/json's.
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

// TestInvalidBody_NamesWhere: a refused request body is a 400 whose message
// carries the decoder's words, which name the JSON location.
func TestInvalidBody_NamesWhere(t *testing.T) {
	var d struct {
		X int `json:"x"`
	}
	r := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(`{"X":7}`)))
	err := common.DecodeBoundedJSON(httptest.NewRecorder(), r, 1<<10, &d)
	appErr := common.InvalidBody(err)
	if appErr.Status != 400 || appErr.Code != common.ErrCodeBadRequest {
		t.Errorf("got %d %s", appErr.Status, appErr.Code)
	}
	if want := `BAD_REQUEST: invalid request body: invalid JSON at "": unknown member name "X"`; appErr.Message != want {
		t.Errorf("message\n got: %s\nwant: %s", appErr.Message, want)
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
