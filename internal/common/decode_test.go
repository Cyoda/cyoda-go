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
	if err := common.DecodeBoundedJSON(w, r, 1<<20, &d); err == nil {
		t.Fatal("expected error")
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
