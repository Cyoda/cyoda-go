package importer

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/cyoda-platform/cyoda-go-spi/strictjson"
)

// ParseJSON parses a JSON reader into a generic any tree.
// Numbers are preserved as json.Number to maintain precision for large
// integers and big decimals. The data has one reading: a member written
// twice in one object, and anything after the value, are refused.
func ParseJSON(r io.Reader) (any, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("json parse: %w", err)
	}
	var raw any
	if err := strictjson.Decode(data, &raw, strictjson.PreserveNumbers); err != nil {
		return nil, fmt.Errorf("json parse: %w", err)
	}
	return raw, nil
}

// ParseXML parses an XML reader into a generic any tree (map[string]any).
// Repeated elements become []any. Attributes become fields.
func ParseXML(r io.Reader) (any, error) {
	dec := xml.NewDecoder(r)
	var root map[string]any
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("xml parse: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			child, err := parseXMLElement(dec, se)
			if err != nil {
				return nil, err
			}
			if m, ok := child.(map[string]any); ok {
				root = m
			} else {
				root = map[string]any{"_value": child}
			}
			break
		}
	}
	if root == nil {
		return nil, fmt.Errorf("xml parse: empty document")
	}
	return root, nil
}

func parseXMLElement(dec *xml.Decoder, start xml.StartElement) (any, error) {
	fields := make(map[string]any)
	for _, attr := range start.Attr {
		fields[attr.Name.Local] = inferXMLValue(attr.Value)
	}
	var textParts []string
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("xml parse element %s: %w", start.Name.Local, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			child, err := parseXMLElement(dec, t)
			if err != nil {
				return nil, err
			}
			name := t.Name.Local
			if existing, ok := fields[name]; ok {
				if arr, isArr := existing.([]any); isArr {
					fields[name] = append(arr, child)
				} else {
					fields[name] = []any{existing, child}
				}
			} else {
				fields[name] = child
			}
		case xml.CharData:
			s := strings.TrimSpace(string(t))
			if s != "" {
				textParts = append(textParts, s)
			}
		case xml.EndElement:
			if len(fields) == 0 && len(textParts) > 0 {
				return inferXMLValue(strings.Join(textParts, " ")), nil
			}
			if len(textParts) > 0 {
				fields["_text"] = inferXMLValue(strings.Join(textParts, " "))
			}
			return fields, nil
		}
	}
}

func inferXMLValue(s string) any {
	// Defer numeric coercion: keep numbers as json.Number so callers can
	// choose Int64() vs Float64() vs string preservation. Mirrors
	// ParseJSON's UseNumber() — XML and JSON imports produce structurally
	// identical trees for numeric leaves.
	if isJSONNumber(s) {
		return json.Number(s)
	}
	if b, err := strconv.ParseBool(s); err == nil {
		return b
	}
	return s
}

// isJSONNumber reports whether s is a valid JSON number per RFC 8259 §6.
// Delegates to encoding/json so the validation rules stay aligned with the
// authority that downstream code uses to round-trip the value.
func isJSONNumber(s string) bool {
	var n json.Number
	return json.Unmarshal([]byte(s), &n) == nil
}
