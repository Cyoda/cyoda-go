package entity

import (
	"fmt"
	"strings"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// GroupedStatsValidationError is returned by ValidateGroupedStatsRequest.
// Code is one of the 400-error codes documented in spec §3.
type GroupedStatsValidationError struct {
	Code    string
	Message string
}

func (e *GroupedStatsValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ValidatedGroupedStatsRequest is the post-validation shape used by the
// service layer.
type ValidatedGroupedStatsRequest struct {
	GroupBy      []GroupExprValidated
	Aggregations []AggregationExprValidated
	// Condition is the raw bytes; the service layer parses via
	// predicate.ParseCondition.
	Condition   []byte
	PointInTime *time.Time
	Limit       *int
}

// GroupExprValidated is the normalized groupBy entry.
type GroupExprValidated struct {
	IsState bool
	Path    string // populated when !IsState; normalized dotted form
}

// AggregationExprValidated is the normalized aggregation entry.
type AggregationExprValidated struct {
	Op    AggregateOp
	Field string
	Alias string
}

// AggregateOp duplicates spi.AggregateOp; the service layer translates
// when handing off to a GroupedAggregator implementation. Keeping it
// local avoids leaking the SPI import into the validation layer.
type AggregateOp string

const (
	AggSum   AggregateOp = "sum"
	AggAvg   AggregateOp = "avg"
	AggMin   AggregateOp = "min"
	AggMax   AggregateOp = "max"
	AggStdev AggregateOp = "stdev"
)

// ValidateGroupedStatsRequest applies the rules from spec §3.
// maxBuckets is CYODA_STATS_GROUP_MAX (the cardinality ceiling); used to
// enforce `limit <= max`.
func ValidateGroupedStatsRequest(r GroupedStatsRequest, maxBuckets int) (*ValidatedGroupedStatsRequest, error) {
	if len(r.GroupBy) == 0 {
		return nil, &GroupedStatsValidationError{Code: common.ErrCodeMissingGroupBy, Message: "groupBy is required"}
	}
	seen := make(map[string]struct{}, len(r.GroupBy))
	groups := make([]GroupExprValidated, 0, len(r.GroupBy))
	for _, raw := range r.GroupBy {
		// "state" is a reserved TOKEN naming the entity's lifecycle state, not
		// a path into its data — so it is matched before the path grammar runs
		// and is exempt from the "$." leader. A groupBy on the DATA field
		// spelled "state" is written "$.state" and is an ordinary path.
		if raw == stateGroupToken {
			if _, dup := seen[raw]; dup {
				return nil, &GroupedStatsValidationError{Code: common.ErrCodeDuplicateGroupBy, Message: "duplicate groupBy entry: " + raw}
			}
			seen[raw] = struct{}{}
			groups = append(groups, GroupExprValidated{IsState: true})
			continue
		}
		norm, err := normalizeScalarPath(raw)
		if err != nil {
			return nil, &GroupedStatsValidationError{Code: common.ErrCodeInvalidGroupByPath, Message: err.Error()}
		}
		if _, dup := seen[norm]; dup {
			return nil, &GroupedStatsValidationError{Code: common.ErrCodeDuplicateGroupBy, Message: "duplicate groupBy entry: " + norm}
		}
		seen[norm] = struct{}{}
		groups = append(groups, GroupExprValidated{Path: norm})
	}

	// Aggregations: dedupe identical (op, field); reject distinct-(op,field)
	// colliding on explicit alias.
	aggs := make([]AggregationExprValidated, 0, len(r.Aggregations))
	seenPair := make(map[[2]string]string, len(r.Aggregations))   // (op,field) -> alias
	aliasOwner := make(map[string][2]string, len(r.Aggregations)) // alias -> (op,field)
	for _, a := range r.Aggregations {
		switch AggregateOp(a.Op) {
		case AggSum, AggAvg, AggMin, AggMax, AggStdev:
		default:
			return nil, &GroupedStatsValidationError{Code: common.ErrCodeInvalidAggregationOp, Message: "unknown op: " + a.Op}
		}
		field, err := normalizeScalarPath(a.Field)
		if err != nil {
			// Carry the reason, not just the offending field — symmetric with
			// INVALID_GROUP_BY_PATH, and the reasons are no longer guessable
			// from the input alone now that the full path grammar is enforced.
			// err already echoes the raw field.
			return nil, &GroupedStatsValidationError{Code: common.ErrCodeInvalidAggregationField, Message: err.Error()}
		}
		pair := [2]string{a.Op, field}
		alias := a.As
		if alias == "" {
			// Synthesized aliases must not embed the JSONPath leader: response
			// keys like `sum_$.amount` leak punctuation into a JSON object
			// name (cosmetic but ugly and breaks dotted access in clients).
			// The validated Field keeps the `$.` prefix because downstream
			// gjson extraction relies on it; only the response alias drops it.
			aliasField := field
			if strings.HasPrefix(aliasField, "$.") {
				aliasField = aliasField[2:]
			}
			alias = a.Op + "_" + aliasField
		}
		if _, dup := seenPair[pair]; dup {
			// identical (op, field) pair: silently dedupe.
			continue
		}
		if owner, taken := aliasOwner[alias]; taken && owner != pair {
			return nil, &GroupedStatsValidationError{Code: common.ErrCodeDuplicateAggregationAlias, Message: alias}
		}
		seenPair[pair] = alias
		aliasOwner[alias] = pair
		aggs = append(aggs, AggregationExprValidated{
			Op:    AggregateOp(a.Op),
			Field: field,
			Alias: alias,
		})
	}

	if r.Limit != nil {
		if *r.Limit <= 0 || *r.Limit > maxBuckets {
			return nil, &GroupedStatsValidationError{
				Code:    common.ErrCodeInvalidLimit,
				Message: fmt.Sprintf("limit must be positive and <= %d", maxBuckets),
			}
		}
	}

	return &ValidatedGroupedStatsRequest{
		GroupBy:      groups,
		Aggregations: aggs,
		Condition:    []byte(r.Condition),
		PointInTime:  r.PointInTime,
		Limit:        r.Limit,
	}, nil
}

// stateGroupToken is the reserved groupBy entry that buckets by the entity's
// lifecycle state. It is a token, not a path, so it is recognised only where
// it means something — the groupBy list. An aggregation over it is not
// defined, so as an aggregation field it is just a path missing its leader.
const stateGroupToken = "state"

// normalizeScalarPath validates a groupBy entry or aggregation field against
// the wire JSON Path grammar ([search.ValidateScalarJSONPath]): the "$."
// leader is required, and array subscripts/projections are rejected because
// the path must denote a single scalar.
//
// It is a validator, not a rewriter: a path that passes is returned unchanged.
// Bracket-quoted property access ("$['x']", "$.['x']") used to be folded into
// dotted form here; it is now rejected, because the condition surface rejects
// it too — accepting it in groupBy while 400ing it in condition would answer
// one request inconsistently across two of its own fields, and the response's
// group-key path would echo a spelling the request never sent.
func normalizeScalarPath(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("path is empty")
	}
	if err := search.ValidateScalarJSONPath(s); err != nil {
		return "", err
	}
	return s, nil
}
