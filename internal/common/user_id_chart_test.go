package common

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
	"unicode/utf8"
)

// TestUserIDRule_MatchesChartSchema keeps the Helm chart's bootstrap.userId
// schema and ValidateUserID in agreement, so helm install rejects exactly the
// values the binary would refuse at startup. The pattern is compiled with Go's
// regexp, the engine the chart's JSON-schema validator uses, and compared with
// the rule on every Unicode scalar value.
func TestUserIDRule_MatchesChartSchema(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/helm/cyoda/values.schema.json")
	if err != nil {
		t.Fatalf("read chart schema: %v", err)
	}
	var schema struct {
		Properties struct {
			Bootstrap struct {
				Properties struct {
					UserID struct {
						MinLength int    `json:"minLength"`
						MaxLength int    `json:"maxLength"`
						Pattern   string `json:"pattern"`
						Not       struct {
							Pattern string `json:"pattern"`
						} `json:"not"`
					} `json:"userId"`
				} `json:"properties"`
			} `json:"bootstrap"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("parse chart schema: %v", err)
	}
	u := schema.Properties.Bootstrap.Properties.UserID
	if u.MinLength != 1 || u.MaxLength != MaxUserIDLen {
		t.Errorf("chart userId length = [%d, %d], want [1, %d]", u.MinLength, u.MaxLength, MaxUserIDLen)
	}
	re, err := regexp.Compile(u.Pattern)
	if err != nil {
		t.Fatalf("compile chart userId pattern: %v", err)
	}
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue // surrogates are not scalar values; no UTF-8 string holds one
		}
		id := "a" + string(r)
		chart := re.MatchString(id)
		binary := ValidateUserID(id) == nil
		if chart != binary {
			t.Errorf("U+%04X: chart admits=%v, ValidateUserID admits=%v", r, chart, binary)
		}
	}

	// The bootstrap user id is first-party, so the chart also reserves the
	// OIDC prefix, through a "not" pattern.
	if u.Not.Pattern == "" {
		t.Fatal("chart userId has no \"not\" pattern reserving the OIDC prefix")
	}
	reserved, err := regexp.Compile(u.Not.Pattern)
	if err != nil {
		t.Fatalf("compile chart userId not-pattern: %v", err)
	}
	for _, id := range []string{"oidc:x", "OIDC:x", "oIdC:x", "oidc:", "oidc", "oidc-x", "x-oidc:y", "admin"} {
		chart := re.MatchString(id) && !reserved.MatchString(id)
		binary := ValidateFirstPartyUserID(id) == nil
		if chart != binary {
			t.Errorf("%q: chart admits=%v, ValidateFirstPartyUserID admits=%v", id, chart, binary)
		}
	}
}
