package search

import (
	"context"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// FieldsLoader yields the model's path -> descriptor map. It is called only
// when the condition or extraPaths name a data path, and at most once.
type FieldsLoader func() (map[string]schema.FieldDescriptor, error)

// ValidateConditionForModel is the one validation sequence every surface that
// takes a condition runs — search (sync and async), conditional delete and
// grouped stats — so one malformed condition gets one code everywhere. The
// order is fixed: structure, then patterns, then the model's paths, then the
// model's types. The checks that need no model come first, so a structural
// or pattern fault is INVALID_CONDITION whatever the model declares; a path
// the model lacks is INVALID_FIELD_PATH; an operand or operator the declared
// types cannot take is CONDITION_TYPE_MISMATCH. The caller has already
// resolved the model: an unknown model is a 404 before any of this runs.
//
// load supplies the model's fields; the path step checks the condition's
// paths — plus extraPaths, the data paths the request names outside its
// condition (grouped stats' groupBy and aggregate fields) — against them in
// one pass, refreshing the model at most once on a miss (a peer may have
// extended it). It returns the refreshed map, which is the map the types are
// checked against and the caller translates with. A request that names no
// data path never calls load. A nil cond skips the condition checks and
// validates extraPaths alone.
func ValidateConditionForModel(ctx context.Context, modelStore spi.ModelStore, ref spi.ModelRef, cond predicate.Condition, extraPaths []string, load FieldsLoader) (map[string]schema.FieldDescriptor, error) {
	return validateConditionSequence(cond, func() (map[string]schema.FieldDescriptor, error) {
		var paths []string
		seen := map[string]struct{}{}
		for _, p := range append(conditionPathsOf(cond), extraPaths...) {
			if _, dup := seen[p]; !dup && p != "" {
				seen[p] = struct{}{}
				paths = append(paths, p)
			}
		}
		if len(paths) == 0 {
			return nil, nil
		}
		fields, err := load()
		if err != nil {
			return nil, err
		}
		return ValidateKnownPaths(ctx, modelStore, ref, paths, fields)
	})
}

func conditionPathsOf(cond predicate.Condition) []string {
	if cond == nil {
		return nil
	}
	return ConditionFieldPaths(cond)
}

// validateConditionSequence holds the order stated on ValidateConditionForModel;
// modelPaths is the surface's path step (search's carries its negative cache).
func validateConditionSequence(cond predicate.Condition, modelPaths func() (map[string]schema.FieldDescriptor, error)) (map[string]schema.FieldDescriptor, error) {
	if cond != nil {
		if cErr := ValidateCondition(cond); cErr != nil {
			return nil, common.Operational(http.StatusBadRequest, structuralConditionErrCode(cErr), cErr.Error())
		}
		if rErr := ValidatePatterns(cond); rErr != nil {
			return nil, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition, rErr.Error())
		}
	}
	fields, err := modelPaths()
	if err != nil {
		return nil, err
	}
	if cond != nil {
		if tErr := ValidateConditionValueTypes(fields, cond); tErr != nil {
			return nil, common.Operational(http.StatusBadRequest, classifyConditionTypeErrCode(tErr), tErr.Error())
		}
	}
	return fields, nil
}

// validateConditionForModel is the search service's run of the sequence. The
// model is resolved first (404 MODEL_NOT_FOUND before any condition verdict);
// its path step uses the service's negative cache.
func (s *SearchService) validateConditionForModel(ctx context.Context, modelRef spi.ModelRef, cond predicate.Condition) (map[string]schema.FieldDescriptor, error) {
	modelStore, err := s.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}
	if appErr := common.EnsureModelRegistered(ctx, modelStore, modelRef); appErr != nil {
		return nil, appErr
	}
	return validateConditionSequence(cond, func() (map[string]schema.FieldDescriptor, error) {
		return s.validateConditionPaths(ctx, modelStore, modelRef, cond)
	})
}
