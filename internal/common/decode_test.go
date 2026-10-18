package common_test

import (
	"bytes"
	"errors"
	"net/http"
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

// TestDecodeOptionalBoundedJSON: an operation whose body is optional treats
// an empty or whitespace-only body as absent — whatever Content-Length says,
// a chunked request included — and decodes any other body strictly.
func TestDecodeOptionalBoundedJSON(t *testing.T) {
	type dst struct {
		X *int `json:"x"`
	}
	for name, tc := range map[string]struct {
		body    string
		chunked bool
		present bool
		wantErr bool
	}{
		"empty":          {"", false, false, false},
		"whitespace":     {" \t\r\n", false, false, false},
		"no-break space": {"\u00a0", false, true, true},
		"vertical tab":   {"\v", false, true, true},
		"empty, chunked": {"", true, false, false},
		"value":          {`{"x":1}`, false, true, false},
		"empty object":   {`{}`, true, true, false},
		"unknown member": {`{"y":1}`, false, true, true},
		"not JSON":       {`{`, false, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			if tc.chunked {
				r.ContentLength = -1
			}
			var d dst
			present, err := common.DecodeOptionalBoundedJSON(httptest.NewRecorder(), r, 1<<10, &d)
			if present != tc.present || (err != nil) != tc.wantErr {
				t.Errorf("present=%t err=%v, want present=%t err=%t", present, err, tc.present, tc.wantErr)
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("read tcp 10.0.0.1:8080->10.0.0.2:51890: i/o timeout")
}

// TestDecodeBoundedJSON_ReadFailure: a body that cannot be read is refused
// in fixed words. The transport's error names socket addresses, which do not
// belong in a response.
func TestDecodeBoundedJSON_ReadFailure(t *testing.T) {
	var d struct{}
	for name, decode := range map[string]func(*http.Request) error{
		"bounded": func(r *http.Request) error { return common.DecodeBoundedJSON(httptest.NewRecorder(), r, 1<<10, &d) },
		"optional": func(r *http.Request) error {
			_, err := common.DecodeOptionalBoundedJSON(httptest.NewRecorder(), r, 1<<10, &d)
			return err
		},
	} {
		err := decode(httptest.NewRequest("POST", "/", failingReader{}))
		if err == nil || err.Error() != "failed to read request body" {
			t.Errorf("%s: err = %v, want the fixed text", name, err)
		}
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
