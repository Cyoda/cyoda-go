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
		{"localdate is temporal", []schema.DataType{schema.LocalDate}, spi.OrderTemporal, false},
		{"localdatetime is temporal", []schema.DataType{schema.LocalDateTime}, spi.OrderTemporal, false},
		{"localtime is temporal", []schema.DataType{schema.LocalTime}, spi.OrderTemporal, false},
		{"zoneddatetime is temporal", []schema.DataType{schema.ZonedDateTime}, spi.OrderTemporal, false},
		{"year is temporal", []schema.DataType{schema.Year}, spi.OrderTemporal, false},
		{"yearmonth is temporal", []schema.DataType{schema.YearMonth}, spi.OrderTemporal, false},
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

func TestSortKindForData(t *testing.T) {
	// The SORT path folds temporal onto OrderText (lexical ISO-8601 =
	// chronological, byte-identical across backends) while non-temporal classes
	// stay as classifyType assigns them. A [String, temporal] mix unifies to
	// OrderText rather than erroring, while a genuinely inconsistent mix
	// (numeric vs temporal/text) still errors.
	cases := []struct {
		name  string
		in    []schema.DataType
		want  spi.OrderKind
		isErr bool
	}{
		{"localdate sorts as text", []schema.DataType{schema.LocalDate}, spi.OrderText, false},
		{"localdatetime sorts as text", []schema.DataType{schema.LocalDateTime}, spi.OrderText, false},
		{"year sorts as text", []schema.DataType{schema.Year}, spi.OrderText, false},
		{"zoneddatetime sorts as text", []schema.DataType{schema.ZonedDateTime}, spi.OrderText, false},
		{"string stays text", []schema.DataType{schema.String}, spi.OrderText, false},
		{"integer stays numeric", []schema.DataType{schema.Integer}, spi.OrderNumeric, false},
		{"bool stays bool", []schema.DataType{schema.Boolean}, spi.OrderBool, false},
		{"nullable localdate sorts as text", []schema.DataType{schema.LocalDate, schema.Null}, spi.OrderText, false},
		{"mixed string+localdate unifies to text", []schema.DataType{schema.String, schema.LocalDate}, spi.OrderText, false},
		{"mixed localdate+string unifies to text", []schema.DataType{schema.LocalDate, schema.String}, spi.OrderText, false},
		{"mixed numeric+temporal still errors", []schema.DataType{schema.Integer, schema.LocalDate}, 0, true},
		{"mixed numeric+string still errors", []schema.DataType{schema.Integer, schema.String}, 0, true},
		{"bytearray rejected", []schema.DataType{schema.ByteArray}, 0, true},
		{"null only rejected", []schema.DataType{schema.Null}, 0, true},
		{"empty rejected", nil, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := sortKindForData(c.in)
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
