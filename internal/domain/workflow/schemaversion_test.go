package workflow

import (
	"errors"
	"strings"
	"testing"
)

func TestCurrentSchemaVersionIsSupported(t *testing.T) {
	// Not parallel: reads production-default SupportedSchemaRanges;
	// running concurrently with TestSupports (which swaps the global)
	// would cause a data race.
	maj, min, err := ParseSchemaVersion(CurrentSchemaVersion)
	if err != nil {
		t.Fatalf("CurrentSchemaVersion %q does not parse: %v", CurrentSchemaVersion, err)
	}
	if err := Supports(maj, min); err != nil {
		t.Fatalf("CurrentSchemaVersion %q not inside SupportedSchemaRanges: %v", CurrentSchemaVersion, err)
	}
}

func TestParseSchemaVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantMajor int
		wantMinor int
		wantErr   bool
	}{
		{"1.0", 1, 0, false},
		{"0.0", 0, 0, false},
		{"12.345", 12, 345, false},
		{"2.0", 2, 0, false},
		// rejected
		{"", 0, 0, true},
		{"1", 0, 0, true},
		{"1.0.0", 0, 0, true},
		{" 1.0 ", 0, 0, true},
		{"v1.0", 0, 0, true},
		{"1.x", 0, 0, true},
		{"01.0", 0, 0, true},
		{"1.00", 0, 0, true},
		{"-1.0", 0, 0, true},
		{"1.-0", 0, 0, true},
		{"1.0\n", 0, 0, true},
		{"99999999999999999999.0", 0, 0, true}, // exceeds int range
		{"１.0", 0, 0, true},                    // fullwidth digit
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			maj, min, err := ParseSchemaVersion(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSchemaVersion(%q) = (%d, %d, nil); want error", tc.in, maj, min)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSchemaVersion(%q) = error %v; want (%d, %d, nil)", tc.in, err, tc.wantMajor, tc.wantMinor)
			}
			if maj != tc.wantMajor || min != tc.wantMinor {
				t.Fatalf("ParseSchemaVersion(%q) = (%d, %d); want (%d, %d)", tc.in, maj, min, tc.wantMajor, tc.wantMinor)
			}
		})
	}
}

func TestSupports(t *testing.T) {
	// Not parallel: mutates package-level SupportedSchemaRanges; running
	// concurrently with TestSupportsMessageNamesSupportedMajors (which
	// also swaps the global) would cause a data race.
	// Stash & swap ranges so the test is independent of the global ranges
	// the production code uses.
	orig := SupportedSchemaRanges
	SupportedSchemaRanges = []SchemaRange{
		{Major: 1, MinMinor: 1, MaxMinor: 3},
	}
	t.Cleanup(func() { SupportedSchemaRanges = orig })

	cases := []struct {
		name    string
		major   int
		minor   int
		wantErr error
	}{
		{"in range low", 1, 1, nil},
		{"in range high", 1, 3, nil},
		{"minor too old", 1, 0, ErrSchemaMinorTooOld},
		{"minor too new", 1, 4, ErrSchemaMinorTooNew},
		{"major absent", 2, 0, ErrSchemaMajorUnsupported},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Supports(tc.major, tc.minor)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Supports(%d, %d) = %v; want nil", tc.major, tc.minor, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Supports(%d, %d) = %v; want errors.Is(_, %v)", tc.major, tc.minor, err, tc.wantErr)
			}
		})
	}
}

func TestSupportsMessageNamesSupportedMajors(t *testing.T) {
	// Not parallel: mutates package-level SupportedSchemaRanges; see
	// TestSupports for the same rationale.
	orig := SupportedSchemaRanges
	SupportedSchemaRanges = []SchemaRange{
		{Major: 1, MinMinor: 0, MaxMinor: 0},
		{Major: 3, MinMinor: 0, MaxMinor: 0},
	}
	t.Cleanup(func() { SupportedSchemaRanges = orig })

	err := Supports(2, 0)
	if err == nil {
		t.Fatalf("Supports(2,0) = nil; want major-unsupported error")
	}
	msg := err.Error()
	for _, want := range []string{"2", "1", "3"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Supports(2,0) message %q missing %q", msg, want)
		}
	}
}
