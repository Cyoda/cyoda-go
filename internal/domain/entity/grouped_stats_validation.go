package entity

import (
	"fmt"
	"strings"
	"time"
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
		return nil, &GroupedStatsValidationError{Code: "MISSING_GROUP_BY", Message: "groupBy is required"}
	}
	seen := make(map[string]struct{}, len(r.GroupBy))
	groups := make([]GroupExprValidated, 0, len(r.GroupBy))
	for _, raw := range r.GroupBy {
		norm, err := normalizeScalarPath(raw)
		if err != nil {
			return nil, &GroupedStatsValidationError{Code: "INVALID_GROUP_BY_PATH", Message: err.Error()}
		}
		if _, dup := seen[norm]; dup {
			return nil, &GroupedStatsValidationError{Code: "DUPLICATE_GROUP_BY", Message: "duplicate groupBy entry: " + norm}
		}
		seen[norm] = struct{}{}
		if norm == "state" {
			groups = append(groups, GroupExprValidated{IsState: true})
		} else {
			groups = append(groups, GroupExprValidated{Path: norm})
		}
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
			return nil, &GroupedStatsValidationError{Code: "INVALID_AGGREGATION_OP", Message: "unknown op: " + a.Op}
		}
		field, err := normalizeScalarPath(a.Field)
		if err != nil {
			return nil, &GroupedStatsValidationError{Code: "INVALID_AGGREGATION_FIELD", Message: a.Field}
		}
		pair := [2]string{a.Op, field}
		alias := a.As
		if alias == "" {
			alias = a.Op + "_" + field
		}
		if _, dup := seenPair[pair]; dup {
			// identical (op, field) pair: silently dedupe.
			continue
		}
		if owner, taken := aliasOwner[alias]; taken && owner != pair {
			return nil, &GroupedStatsValidationError{Code: "DUPLICATE_AGGREGATION_ALIAS", Message: alias}
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
				Code:    "INVALID_LIMIT",
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

// normalizeScalarPath canonicalizes a JSONPath. Accepts dotted notation
// and bracket-quoted property access. Rejects array projections.
//
// Returns the reserved token "state" unchanged when seen.
func normalizeScalarPath(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("path is empty")
	}
	if s == "state" {
		return s, nil
	}
	// Normalize $['x']['y'] / $.['x'].['y'] to $.x.y.
	norm := s
	for {
		before := norm
		norm = strings.ReplaceAll(norm, "['", ".")
		norm = strings.ReplaceAll(norm, "']", "")
		norm = strings.ReplaceAll(norm, "..", ".")
		if norm == before {
			break
		}
	}
	if strings.ContainsAny(norm, "[]") {
		return "", fmt.Errorf("array projection not supported: %s", s)
	}
	return norm, nil
}
