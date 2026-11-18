package modelcache_test

import (
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go/internal/cluster/modelcache"
)

func TestInvalidation_RoundTrip(t *testing.T) {
	ref := spi.ModelRef{EntityName: "E", ModelVersion: "3"}
	raw, err := modelcache.EncodeInvalidation("tenant-A", ref)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	gotTenant, gotRef, ok := modelcache.DecodeInvalidation(raw)
	if !ok || gotTenant != "tenant-A" || gotRef != ref {
		t.Errorf("round trip: tenant=%q ref=%+v ok=%v", gotTenant, gotRef, ok)
	}
}

func TestInvalidation_DecodeRejectsBlanks(t *testing.T) {
	if _, _, ok := modelcache.DecodeInvalidation([]byte(`{"t":"","n":"","v":""}`)); ok {
		t.Error("decode should reject empty fields")
	}
	if _, _, ok := modelcache.DecodeInvalidation([]byte(`{"t":"x","n":"","v":"1"}`)); ok {
		t.Error("decode should reject blank EntityName")
	}
	if _, _, ok := modelcache.DecodeInvalidation([]byte(`{not-json`)); ok {
		t.Error("decode should reject malformed JSON")
	}
}
