package importer

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInferXMLValue_Numeric(t *testing.T) {
	cases := []struct {
		input string
		want  json.Number
	}{
		{"0", "0"},
		{"-0", "-0"},
		{"-0.0", "-0.0"},
		{"42", "42"},
		{"9007199254740993", "9007199254740993"}, // > 2^53, must NOT round
		{"-123", "-123"},
		{"123.456", "123.456"},
		{"-0.5", "-0.5"},
		{"1e10", "1e10"},
		{"1.5e-5", "1.5e-5"},
		{"1E2", "1E2"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := inferXMLValue(tc.input)
			n, ok := got.(json.Number)
			if !ok {
				t.Fatalf("inferXMLValue(%q) = %T (%v), want json.Number", tc.input, got, got)
			}
			if string(n) != string(tc.want) {
				t.Errorf("inferXMLValue(%q) = %q, want %q", tc.input, n, tc.want)
			}
		})
	}
}

func TestInferXMLValue_Bool(t *testing.T) {
	if got := inferXMLValue("true"); got != true {
		t.Errorf("true: got %v (%T)", got, got)
	}
	if got := inferXMLValue("false"); got != false {
		t.Errorf("false: got %v (%T)", got, got)
	}
}

func TestInferXMLValue_RejectedNumerics(t *testing.T) {
	// JSON-grammar edge cases that MUST NOT be accepted as json.Number.
	rejected := []string{
		"007", "00", "01.5", // leading zeros
		"-",         // lone minus
		"+1",        // leading plus
		"1.",        // trailing dot
		".5",        // no integer part
		"1e", "1e+", // incomplete exponent
		"NaN", "Inf", "-Inf", // float literals not in JSON grammar
		"0x1a",  // hex
		"",      // empty
		"hello", // non-numeric
	}
	for _, s := range rejected {
		t.Run(s, func(t *testing.T) {
			got := inferXMLValue(s)
			if _, isNum := got.(json.Number); isNum {
				t.Errorf("inferXMLValue(%q) returned json.Number; expected string or bool", s)
			}
		})
	}
}

func TestInferXMLValue_String(t *testing.T) {
	if got := inferXMLValue("hello"); got != "hello" {
		t.Errorf("hello: got %v (%T)", got, got)
	}
	if got := inferXMLValue(""); got != "" {
		t.Errorf("empty: got %v (%T)", got, got)
	}
}

func TestParseXML_JSON_Symmetry_LargeInt(t *testing.T) {
	// XML and JSON parsers must produce structurally identical trees for
	// numeric leaves.
	xmlPayload := `<root><big>9007199254740993</big></root>`
	jsonPayload := `{"big": 9007199254740993}`

	xmlAny, err := ParseXML(strings.NewReader(xmlPayload))
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	jsonAny, err := ParseJSON(strings.NewReader(jsonPayload))
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}

	xmlMap := xmlAny.(map[string]any)
	jsonMap := jsonAny.(map[string]any)

	xmlVal, _ := xmlMap["big"].(json.Number)
	jsonVal, _ := jsonMap["big"].(json.Number)
	if string(xmlVal) != string(jsonVal) {
		t.Errorf("XML %q != JSON %q for large int", xmlVal, jsonVal)
	}
	if string(xmlVal) != "9007199254740993" {
		t.Errorf("XML rendered %q, expected %q", xmlVal, "9007199254740993")
	}
}
