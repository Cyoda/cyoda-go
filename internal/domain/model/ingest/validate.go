// Package ingest holds the checks every entity payload passes before it is
// stored, wherever the payload came from.
//
// It lives below both internal/domain/entity and internal/domain/workflow so
// that a processor's returned data goes through exactly the same storability
// and schema checks as a client write. entity imports workflow, so the engine
// can never call back into the entity handler; and the scheduled-transition
// ingress has no handler at all. A shared leaf package is therefore the only
// wiring that reaches every ingress.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// ErrInternalSchema tags schema-processing errors inside validateOrExtend
// that represent internal failures (codec decode/encode, Diff computation,
// plugin-layer ExtendSchema write) rather than client-contract violations.
// The handler classifier uses errors.Is to route these to 5xx with a
// logged ticket. Using a sentinel rather than string-matching the wrap
// messages makes classification robust to future wording changes — the
// prior string-match classifier would have silently shifted a renamed
// "failed to extend schema" to 4xx.
var ErrInternalSchema = errors.New("internal schema processing failure")

// IncompatibleTypeError is the typed validation failure surfaced when at
// least one ValidationError carries ErrKindIncompatibleType (the
// dictionary-aligned "wrong DataType" signal — Cloud's
// FoundIncompatibleTypeWithEntityModelException).
//
// Rendered by classifyValidateOrExtendErr into a 400 INCOMPATIBLE_TYPE
// AppError with Props {fieldPath, expectedType, actualType} so SDKs can
// branch on the precondition without scraping the Message string.
type IncompatibleTypeError struct {
	Path          string
	ExpectedTypes []schema.DataType
	ActualType    schema.DataType
	Message       string
	EntityName    string // populated by enrichWithModelRef post-validation
	EntityVersion string // populated by enrichWithModelRef post-validation
}

func (e *IncompatibleTypeError) Error() string { return e.Message }

// enrichWithModelRef threads model identification (entity name, version)
// onto an *IncompatibleTypeError so the classifier can render those Props
// alongside the validator-supplied (path, expected/actualType). For all
// other error types the input is returned unchanged.
func enrichWithModelRef(err error, ref spi.ModelRef) error {
	var incompatErr *IncompatibleTypeError
	if errors.As(err, &incompatErr) {
		incompatErr.EntityName = ref.EntityName
		incompatErr.EntityVersion = ref.ModelVersion
	}
	return err
}

func ValidateOrExtend(ctx context.Context, modelStore spi.ModelStore, desc *spi.ModelDescriptor, parsedData any) error {
	modelNode, err := schema.Unmarshal(desc.Schema)
	if err != nil {
		return fmt.Errorf("%w: failed to unmarshal model schema: %w", ErrInternalSchema, err)
	}

	if desc.ChangeLevel == "" {
		errs := schema.Validate(modelNode, parsedData)
		if len(errs) > 0 {
			return enrichWithModelRef(ValidationErrorsToError(errs), desc.Ref)
		}
		return nil
	}

	extended, err := schema.Extend(modelNode, parsedData, desc.ChangeLevel)
	if err != nil {
		if errors.Is(err, schema.ErrInvalidFieldName) {
			// A field name the wire jsonPath grammar cannot address is a
			// client contract violation with a concrete remedy — rename the
			// key — so it gets the same 400 VALIDATION_FAILED the explicit
			// model import answers.
			return common.Operational(http.StatusBadRequest, common.ErrCodeValidationFailed, err.Error())
		}
		var levelErr *schema.ChangeLevelError
		if errors.As(err, &levelErr) {
			// Only a genuine changeLevelError refusal wears this label — its
			// remedy (raise the configured changeLevel) is specific to it.
			return fmt.Errorf("change level violation: %w", err)
		}
		var unsupportedErr *schema.UnsupportedValueError
		if errors.As(err, &unsupportedErr) {
			// A value schema.Admit's traversal cannot classify at all is a
			// caller-side contract violation (a raw float64 leaking through
			// without json.UseNumber decoding, or a Go type node never
			// produces), not a tenant-contract one — its own text names a Go
			// decoding instruction or a %T-formatted Go type, neither of
			// which belongs in a 4xx body. Every production ingress keeps
			// numbers as json.Number (json.UseNumber or
			// strictjson.PreserveNumbers), so this is unreachable today; marking it
			// here is what keeps it unreachable in the response too, should
			// that ever change (security review L1).
			return fmt.Errorf("%w: schema admission failed: %w", ErrInternalSchema, err)
		}
		// Anything else (validation depth exceeded, an internal admit
		// error) is a different failure with a different remedy; wrapping
		// it as a "change level violation" would be dishonest about what
		// went wrong (final review M4).
		return fmt.Errorf("schema admission failed: %w", err)
	}

	// Guard: if any unique key field would become non-scalar in the extended
	// schema, reject the write now. A write may add a kind to a path — a
	// unique-key leaf gaining an object or array branch is the reachable case
	// — and the unique keys were valid when declared, so the extension must
	// not silently invalidate them. The answer is a 422 naming the key, not a
	// write that leaves the model unable to enforce it.
	if len(desc.UniqueKeys) > 0 {
		if vErr := schema.ValidateUniqueKeys(extended, desc.UniqueKeys); vErr != nil {
			var de *schema.UniqueKeyDefError
			if errors.As(vErr, &de) {
				return common.Operational(http.StatusUnprocessableEntity, common.ErrCodeInvalidUniqueKeyDefinition,
					"schema change would invalidate a composite unique key: "+de.Reason)
			}
			return fmt.Errorf("%w: re-validate unique keys: %w", ErrInternalSchema, vErr)
		}
	}

	// Compute the additive delta. Diff returns (nil, nil) when the
	// extension is a semantic no-op, which is the common case on
	// every entity write.
	delta, err := schema.Diff(modelNode, extended)
	if err != nil {
		return fmt.Errorf("%w: failed to compute schema delta: %w", ErrInternalSchema, err)
	}
	if delta == nil {
		return nil
	}
	// Append to the extension log via the plugin. Participates in the
	// ambient entity transaction so visibility is commit-bound.
	if err := modelStore.ExtendSchema(ctx, desc.Ref, delta); err != nil {
		return fmt.Errorf("%w: failed to extend schema: %w", ErrInternalSchema, err)
	}
	return nil
}

// ValidateStrict validates parsedData against the model schema WITHOUT
// extending it. PATCH uses this: a sparse delta must never widen the tenant's
// model (a stray/typo'd key is rejected, not absorbed). Mirrors the
// ChangeLevel=="" branch of validateOrExtend.
func ValidateStrict(desc *spi.ModelDescriptor, parsedData any) error {
	modelNode, err := schema.Unmarshal(desc.Schema)
	if err != nil {
		return fmt.Errorf("%w: failed to unmarshal model schema: %w", ErrInternalSchema, err)
	}
	errs := schema.Validate(modelNode, parsedData)
	if len(errs) > 0 {
		return enrichWithModelRef(ValidationErrorsToError(errs), desc.Ref)
	}
	return nil
}

// ValidateDescriptor unmarshals desc.Schema and runs schema.Validate.
// Returns nil on success, or a []ValidationError on failure (including
// a descriptive entry if desc itself is malformed or nil).
func ValidateDescriptor(desc *spi.ModelDescriptor, data any) []schema.ValidationError {
	if desc == nil {
		return []schema.ValidationError{{Message: "nil descriptor"}}
	}
	node, err := schema.Unmarshal(desc.Schema)
	if err != nil {
		return []schema.ValidationError{{Message: fmt.Sprintf("unmarshal schema: %v", err)}}
	}
	return schema.Validate(node, data)
}

// maxRenderedValidationErrors bounds how many ValidationError entries
// ValidationErrorsToError renders verbatim into the joined message. Validate
// emits one entry per offending path with no cap of its own — a 10 MiB body
// of undeclared one-character fields can carry hundreds of thousands of them
// — so joining every entry into one string made the 400 response body
// proportional to the attacker's request size rather than bounded. Entries
// past the cap are summarized as "... and N more" instead of dropped
// silently, so the response still says how big the mismatch was. This bounds
// only the RENDERED STRING: HasUnknownSchemaElement and FirstIncompatibleType
// (see their own doc comments) read the full errs slice directly and are
// unaffected by where the string truncates.
const maxRenderedValidationErrors = 32

// ValidationErrorsToError converts a []ValidationError to a single error,
// preserving the concatenation style used by validateOrExtend.
//
// When at least one entry classifies as ErrKindIncompatibleType (the
// dictionary-aligned "wrong DataType" signal), the function returns a
// typed *IncompatibleTypeError carrying the first such entry's structured
// fields so classifyValidateOrExtendErr can render INCOMPATIBLE_TYPE Props
// without scraping the Message string. Other validation errors fall back
// to the generic "validation failed: ..." wrap, classified as
// BAD_REQUEST downstream.
func ValidationErrorsToError(errs []schema.ValidationError) error {
	rendered := errs
	truncated := false
	if len(rendered) > maxRenderedValidationErrors {
		rendered = rendered[:maxRenderedValidationErrors]
		truncated = true
	}
	msgs := make([]string, len(rendered))
	for i, e := range rendered {
		msgs[i] = e.Error()
	}
	joined := fmt.Sprintf("validation failed: %s", strings.Join(msgs, "; "))
	if truncated {
		joined = fmt.Sprintf("%s; ... and %d more", joined, len(errs)-maxRenderedValidationErrors)
	}
	if first := schema.FirstIncompatibleType(errs); first != nil {
		return &IncompatibleTypeError{
			Path:          first.Path,
			ExpectedTypes: first.ExpectedTypes,
			ActualType:    first.ActualType,
			Message:       joined,
		}
	}
	return fmt.Errorf("%s", joined)
}
