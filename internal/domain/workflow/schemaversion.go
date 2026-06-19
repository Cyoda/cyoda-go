// Package workflow — schema-version contract for the workflow-import
// DTO shape. Independent of the cyoda-go binary version and the
// OpenAPI document version. See docs/workflow-schema-versioning.md
// for the bump rules and the per-version changelog.
package workflow

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// CurrentSchemaVersion is stamped on every exported workflow. Bump
// only per docs/workflow-schema-versioning.md. MUST be inside one of
// SupportedSchemaRanges; schemaversion_test.go asserts this.
const CurrentSchemaVersion = "1.0"

// SchemaRange is a closed integer interval [MinMinor..MaxMinor] on
// the MINOR axis of a given MAJOR. A range models a single contiguous
// supported window — when a MINOR ages out, raise MinMinor; when an
// older MAJOR is retired, drop its range entirely.
type SchemaRange struct {
	Major    int `json:"major"`
	MinMinor int `json:"minMinor"`
	MaxMinor int `json:"maxMinor"`
}

// SupportedSchemaRanges is the closed set of (MAJOR, MINOR) pairs the
// server accepts on import. To add a new MINOR within a MAJOR, raise
// MaxMinor. To retire old MINORs, raise MinMinor. To add a new MAJOR,
// append a new SchemaRange.
//
// Tests may overwrite this variable (with t.Cleanup restoration) to
// exercise alternative range configurations without changing
// production defaults.
var SupportedSchemaRanges = []SchemaRange{
	{Major: 1, MinMinor: 0, MaxMinor: 0},
}

// Sentinel errors returned by Supports. Callers use errors.Is to
// branch on sub-case and produce a precise client-facing message.
var (
	ErrSchemaMajorUnsupported = errors.New("workflow schema major version unsupported")
	ErrSchemaMinorTooNew      = errors.New("workflow schema minor version too new")
	ErrSchemaMinorTooOld      = errors.New("workflow schema minor version no longer accepted")
)

// ParseSchemaVersion parses a MAJOR.MINOR string into integers. The
// accepted shape is the regex ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ —
// no leading zeros (except a single "0"), no whitespace, no PATCH
// suffix, no sign. Errors mention the offending input so the import
// handler can surface it verbatim to the client.
func ParseSchemaVersion(s string) (major, minor int, err error) {
	if s == "" {
		return 0, 0, fmt.Errorf("workflow schema version is empty; required MAJOR.MINOR form")
	}
	parts := strings.Split(s, ".")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("workflow schema version %q is not in MAJOR.MINOR form", s)
	}
	parseSegment := func(seg string) (int, error) {
		if seg == "" {
			return 0, fmt.Errorf("empty segment")
		}
		// Reject leading zeros for non-zero values: "01" is invalid but
		// "0" is fine.
		if len(seg) > 1 && seg[0] == '0' {
			return 0, fmt.Errorf("leading zero")
		}
		for _, c := range seg {
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("non-digit character %q", c)
			}
		}
		n, convErr := strconv.Atoi(seg)
		if convErr != nil {
			return 0, convErr
		}
		return n, nil
	}
	maj, e1 := parseSegment(parts[0])
	if e1 != nil {
		return 0, 0, fmt.Errorf("workflow schema version %q is not in MAJOR.MINOR form: %w", s, e1)
	}
	min, e2 := parseSegment(parts[1])
	if e2 != nil {
		return 0, 0, fmt.Errorf("workflow schema version %q is not in MAJOR.MINOR form: %w", s, e2)
	}
	return maj, min, nil
}

// Supports reports whether (major, minor) is inside any supported
// range. On failure, the returned error wraps one of
// ErrSchemaMajorUnsupported, ErrSchemaMinorTooNew, or
// ErrSchemaMinorTooOld, and its message is suitable for client-facing
// surfacing — it names the offending pair and the supported window.
func Supports(major, minor int) error {
	var matchedMajor bool
	for _, r := range SupportedSchemaRanges {
		if r.Major != major {
			continue
		}
		matchedMajor = true
		if minor < r.MinMinor {
			return fmt.Errorf("%w: workflow schema %d.%d is no longer accepted on this server; minimum supported in major %d: %d.%d",
				ErrSchemaMinorTooOld, major, minor, r.Major, r.Major, r.MinMinor)
		}
		if minor > r.MaxMinor {
			return fmt.Errorf("%w: this server supports workflow schema up to %d.%d; payload declares %d.%d. Upgrade cyoda-go or regenerate the file against an older schema",
				ErrSchemaMinorTooNew, r.Major, r.MaxMinor, major, minor)
		}
		return nil
	}
	if !matchedMajor {
		majors := make([]int, 0, len(SupportedSchemaRanges))
		for _, r := range SupportedSchemaRanges {
			majors = append(majors, r.Major)
		}
		return fmt.Errorf("%w: workflow schema major version %d unsupported on this server; supported majors: %v",
			ErrSchemaMajorUnsupported, major, majors)
	}
	return nil
}
