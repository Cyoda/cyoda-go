package importer

import (
	"errors"
	"fmt"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// ErrInvalidFieldName marks a field name that the wire jsonPath grammar cannot
// spell. Both ingresses that establish a model's field set — the sample-data
// model import and the ChangeLevel-driven schema extension on an entity write —
// funnel through [Walk], so this sentinel is the single classification signal
// their handlers key on to answer 400 VALIDATION_FAILED.
//
// The rule exists because the two halves of the platform must agree on which
// fields exist. The query surface addresses a field by a jsonPath whose
// segments are ASCII letters, digits, "_" and "-", and offers no escape hatch:
// bracket-quoted access is rejected, and no evaluator in the stack resolves it.
// Recording a field outside that charset would therefore guarantee data that
// can be stored and never queried — an answer that is silently wrong rather
// than unavailable. cyoda-go fails closed instead: the field is refused at the
// door, with a diagnostic naming the key to rename.
var ErrInvalidFieldName = errors.New("invalid field name")

// validateFieldName reports whether name is usable as a single jsonPath
// segment, i.e. whether a query could ever address the field. parent is the
// canonical path of the object that declares it, carried only so the
// diagnostic can point at the offending location.
//
// The check delegates to [schema.IsSegmentName], the one definition of the
// segment charset the query-side path grammar is built from. A whole-segment
// check is exactly the rule a bare field name needs: it admits no subscript
// (a field name must denote one node) and no "." (that would spell two
// segments and name a field no lookup could distinguish from a nested one).
func validateFieldName(parent, name string) error {
	if schema.IsSegmentName(name) {
		return nil
	}
	return fmt.Errorf("%w: %q in object at %q — a field name must be addressable as a jsonPath segment: "+
		"ASCII letters, digits, %q and %q only, and not empty; rename the field",
		ErrInvalidFieldName, name, parent, "_", "-")
}
