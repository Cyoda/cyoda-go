package common_test

import (
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/strictjson"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// TestJSONErrorShape_StrictJSON: a strictjson refusal logs its class, and
// never the member name it quotes — the name can be a tenant's data key.
func TestJSONErrorShape_StrictJSON(t *testing.T) {
	var b struct {
		N int `json:"n"`
	}
	err := strictjson.Decode([]byte(`{"n":1,"secretKeyName":2}`), &b)
	got := common.JSONErrorShape(err)
	if !strings.Contains(got, "unknown member") {
		t.Errorf("shape %q does not give the class", got)
	}
	if strings.Contains(got, "secretKeyName") {
		t.Errorf("shape %q quotes the member name", got)
	}
}
