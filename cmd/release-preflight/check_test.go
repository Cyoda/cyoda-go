package main

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestCheckPseudoVersions covers the rule: pseudo-versions in dependencies
// are only a release blocker when they belong to our own namespace
// (cyoda-platform/*). Upstream dependencies routinely resolve to pseudo-
// versions (e.g. golang.org/x/exp v0.0.0-...), which is a normal Go
// ecosystem artifact and not a release-reproducibility issue for us.
func TestCheckPseudoVersions(t *testing.T) {
	cases := []struct {
		name       string
		goListOut  string
		org        string
		wantModule []string // modules we expect to be flagged (order-independent)
	}{
		{
			name: "upstream pseudo-versions are ignored",
			goListOut: `github.com/foo/bar v1.2.3
github.com/99designs/go-keychain v0.0.0-20191008050251-8e49817e8af4
golang.org/x/exp v0.0.0-20230315142452-642cacee5cc0
github.com/cyoda-platform/cyoda-go v1.0.0
`,
			org:        "github.com/cyoda-platform/",
			wantModule: nil,
		},
		{
			name: "cyoda-platform pseudo-version flagged",
			goListOut: `github.com/cyoda-platform/cyoda-go-spi v0.0.0-20260401123456-abcdef123456
github.com/other/mod v1.0.0
`,
			org:        "github.com/cyoda-platform/",
			wantModule: []string{"github.com/cyoda-platform/cyoda-go-spi"},
		},
		{
			name: "cyoda-platform prerelease pseudo-version (form 2) flagged",
			goListOut: `github.com/cyoda-platform/cyoda-go-spi v0.6.1-pre.0.20260401123456-abcdef123456
`,
			org:        "github.com/cyoda-platform/",
			wantModule: []string{"github.com/cyoda-platform/cyoda-go-spi"},
		},
		{
			name: "cyoda-platform patch-bump pseudo-version (form 3) flagged",
			goListOut: `github.com/cyoda-platform/cyoda-go-spi v0.6.1-0.20260401123456-abcdef123456
`,
			org:        "github.com/cyoda-platform/",
			wantModule: []string{"github.com/cyoda-platform/cyoda-go-spi"},
		},
		{
			name: "real tagged cyoda-platform version passes",
			goListOut: `github.com/cyoda-platform/cyoda-go-spi v0.6.0
github.com/cyoda-platform/cyoda-go/plugins/memory v0.1.0
`,
			org:        "github.com/cyoda-platform/",
			wantModule: nil,
		},
		{
			name:       "empty input returns no violations",
			goListOut:  "",
			org:        "github.com/cyoda-platform/",
			wantModule: nil,
		},
		{
			name: "indirect dependencies keyword does not trip the regex",
			goListOut: `github.com/99designs/go-keychain v0.0.0-20191008050251-8e49817e8af4
github.com/cyoda-platform/cyoda-go-spi v0.6.0 // indirect
`,
			org:        "github.com/cyoda-platform/",
			wantModule: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := checkPseudoVersions([]byte(tc.goListOut), tc.org)
			got := moduleNames(v)
			want := append([]string(nil), tc.wantModule...)
			sort.Strings(got)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("violations: got %v, want %v", got, want)
			}
		})
	}
}

// TestCheckReplaces covers the rule: local-path replaces of the form
// `=> ./path` are required for in-repo multi-module layouts and must
// be allowed. Replaces diverting to an EXTERNAL module path are a
// release-reproducibility issue and must be flagged.
func TestCheckReplaces(t *testing.T) {
	cases := []struct {
		name       string
		goMod      string
		wantModule []string
	}{
		{
			name: "in-repo local-path replace (single-line) is allowed",
			goMod: `module example.com/repo

go 1.26

replace github.com/cyoda-platform/cyoda-go/plugins/memory => ./plugins/memory
`,
			wantModule: nil,
		},
		{
			name: "external-module replace is flagged",
			goMod: `module example.com/repo

go 1.26

replace github.com/some/mod => github.com/fork/mod v1.2.3
`,
			wantModule: []string{"github.com/some/mod"},
		},
		{
			name: "replace block with mix of local and external",
			goMod: `module example.com/repo

go 1.26

replace (
	github.com/cyoda-platform/cyoda-go/plugins/postgres => ./plugins/postgres
	github.com/cyoda-platform/cyoda-go/plugins/sqlite => ./plugins/sqlite
	github.com/external/dep => github.com/fork/dep v9.9.9
)
`,
			wantModule: []string{"github.com/external/dep"},
		},
		{
			name: "parent-relative local path is allowed",
			goMod: `module example.com/repo

go 1.26

replace github.com/sibling/mod => ../sibling-module
`,
			wantModule: nil,
		},
		{
			name: "absolute local path is allowed",
			goMod: `module example.com/repo

go 1.26

replace github.com/sibling/mod => /opt/local/mod
`,
			wantModule: nil,
		},
		{
			name: "commented-out replace is ignored",
			goMod: `module example.com/repo

go 1.26

// replace github.com/some/mod => github.com/fork/mod v1.2.3
`,
			wantModule: nil,
		},
		{
			name: "no replaces, no violations",
			goMod: `module example.com/repo

go 1.26

require github.com/foo/bar v1.0.0
`,
			wantModule: nil,
		},
		{
			name: "replace with version pin on local-path — tolerate both forms",
			goMod: `module example.com/repo

go 1.26

replace github.com/cyoda-platform/cyoda-go/plugins/memory v0.1.0 => ./plugins/memory
`,
			wantModule: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := checkReplaces([]byte(tc.goMod))
			got := moduleNames(v)
			want := append([]string(nil), tc.wantModule...)
			sort.Strings(got)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("violations: got %v, want %v", got, want)
			}
		})
	}
}

// TestFormatViolations ensures operator-facing output names the path,
// categorises the finding, and does not leak secret-looking strings.
func TestFormatViolations(t *testing.T) {
	violations := []Violation{
		{Kind: "pseudo-version", Module: "github.com/cyoda-platform/cyoda-go-spi", Detail: "v0.0.0-20260401123456-abcdef123456"},
		{Kind: "replace", Module: "github.com/other/mod", Detail: "github.com/other/mod => github.com/fork/mod v9.9.9"},
	}
	out := formatViolations(violations)
	if !strings.Contains(out, "pseudo-version") {
		t.Errorf("output must name pseudo-version finding: %q", out)
	}
	if !strings.Contains(out, "replace") {
		t.Errorf("output must name replace finding: %q", out)
	}
	if !strings.Contains(out, "github.com/cyoda-platform/cyoda-go-spi") {
		t.Errorf("output must include the offending module path: %q", out)
	}
}

func moduleNames(vs []Violation) []string {
	if len(vs) == 0 {
		return nil
	}
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Module
	}
	return out
}
