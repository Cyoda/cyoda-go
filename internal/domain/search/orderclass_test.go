package search

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func TestClassifyType(t *testing.T) {
	cases := []struct {
		name  string
		in    []schema.DataType
		want  spi.OrderKind
		isErr bool
	}{
		{"int", []schema.DataType{schema.Integer}, spi.OrderNumeric, false},
		{"double", []schema.DataType{schema.Double}, spi.OrderNumeric, false},
		{"numeric union same class", []schema.DataType{schema.Integer, schema.Long}, spi.OrderNumeric, false},
		{"string", []schema.DataType{schema.String}, spi.OrderText, false},
		{"uuid", []schema.DataType{schema.UUIDType}, spi.OrderText, false},
		{"localdate is text", []schema.DataType{schema.LocalDate}, spi.OrderText, false},
		{"year is text", []schema.DataType{schema.Year}, spi.OrderText, false},
		{"yearmonth is text", []schema.DataType{schema.YearMonth}, spi.OrderText, false},
		{"bool", []schema.DataType{schema.Boolean}, spi.OrderBool, false},
		{"nullable string", []schema.DataType{schema.String, schema.Null}, spi.OrderText, false},
		{"bytearray rejected", []schema.DataType{schema.ByteArray}, 0, true},
		{"disagreeing union rejected", []schema.DataType{schema.Integer, schema.String}, 0, true},
		{"null only rejected", []schema.DataType{schema.Null}, 0, true},
		{"empty rejected", nil, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := classifyType(c.in)
			if c.isErr {
				if err == nil {
					t.Fatalf("want error, got Kind=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("Kind = %v, want %v", got, c.want)
			}
		})
	}
}

func TestResolveMetaField(t *testing.T) {
	mf, ok := resolveMetaField("creationDate")
	if !ok || mf.Source != spi.SourceMeta || mf.Kind != spi.OrderTemporal || mf.Path != "creationDate" {
		t.Fatalf("creationDate resolved to %+v ok=%v", mf, ok)
	}
	if _, ok := resolveMetaField("state"); !ok {
		t.Fatal("state should resolve")
	}
	if _, ok := resolveMetaField("nope"); ok {
		t.Fatal("unknown meta field must not resolve")
	}
	if _, ok := resolveMetaField("label.position.x"); ok {
		t.Fatal("nested meta path must not resolve")
	}
}
