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
	// All six canonical meta sort fields must resolve with exact Source, Path, and Kind.
	// A copy-paste error on Kind (e.g. Text vs Temporal) will fail this table.
	cases := []struct {
		name string
		kind spi.OrderKind
	}{
		{"state", spi.OrderText},
		{"creationDate", spi.OrderTemporal},
		{"lastUpdateTime", spi.OrderTemporal},
		{"transitionForLatestSave", spi.OrderText},
		{"transactionId", spi.OrderText},
		{"id", spi.OrderText},
	}
	for _, c := range cases {
		mf, ok := resolveMetaField(c.name)
		if !ok {
			t.Errorf("%s: should resolve, got ok=false", c.name)
			continue
		}
		if mf.Source != spi.SourceMeta {
			t.Errorf("%s: Source = %v, want SourceMeta", c.name, mf.Source)
		}
		if mf.Path != c.name {
			t.Errorf("%s: Path = %q, want %q", c.name, mf.Path, c.name)
		}
		if mf.Kind != c.kind {
			t.Errorf("%s: Kind = %v, want %v", c.name, mf.Kind, c.kind)
		}
	}

	// Negative: unknown and nested paths must not resolve.
	if _, ok := resolveMetaField("nope"); ok {
		t.Fatal("unknown meta field must not resolve")
	}
	// A dotted name is not a map key — this enforces "no nested meta paths".
	if _, ok := resolveMetaField("label.position.x"); ok {
		t.Fatal("nested meta path must not resolve")
	}
}
