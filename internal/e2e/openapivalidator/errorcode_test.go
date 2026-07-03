package openapivalidator

import "testing"

func TestExtractErrorCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"nested errorCode", `{"type":"about:blank","status":400,"properties":{"errorCode":"INVALID_CONDITION"}}`, "INVALID_CONDITION"},
		{"no properties", `{"type":"about:blank","status":404}`, ""},
		{"properties without errorCode", `{"properties":{"retryable":true}}`, ""},
		{"empty body", ``, ""},
		{"not json", `<html>nope</html>`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractErrorCode([]byte(tc.body)); got != tc.want {
				t.Errorf("extractErrorCode(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestRecordAndReadErrorTriples(t *testing.T) {
	c := newCollector()
	c.recordErrorCode("deleteEntities", 400, "INVALID_CONDITION")
	c.recordErrorCode("deleteEntities", 400, "INVALID_CONDITION") // dedup
	c.recordErrorCode("createEntity", 409, "UNIQUE_VIOLATION")
	got := c.observedErrorTriples()
	if len(got) != 2 {
		t.Fatalf("expected 2 distinct triples, got %d: %+v", len(got), got)
	}
}
