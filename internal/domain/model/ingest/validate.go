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
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
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

	incomingModel, err := importer.Walk(parsedData)
	if err != nil {
		return fmt.Errorf("failed to walk data: %w", err)
	}
	extended, err := schema.Extend(modelNode, incomingModel, desc.ChangeLevel)
	if err != nil {
		// Polymorphic-slot rejections cannot be resolved by raising ChangeLevel
		// and so must not wear the "change level violation" prefix — the phrase
		// misleads clients into tuning a setting that wouldn't help.
		if errors.Is(err, schema.ErrPolymorphicSlot) {
			return err
		}
		return fmt.Errorf("change level violation: %w", err)
	}

	// Guard: if any unique key field would become non-scalar in the extended
	// schema, reject the write now. This catches the null-only-leaf → object/array
	// widening case (a TYPE-level change permitted by Structural ChangeLevel)
	// that would otherwise surface as an opaque Diff "kind change" 5xx. The
	// unique keys were valid when declared; the schema extension must not
	// silently invalidate them.
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
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	joined := fmt.Sprintf("validation failed: %s", strings.Join(msgs, "; "))
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
