package entity

import (
	"encoding/json"
	"time"
)

// GroupedStatsRequest is the body of POST
// /api/entity/stats/{entityName}/{modelVersion}/query.
//
// See docs/superpowers/specs/2026-06-14-issue-299-grouped-stats-design.md §3.
type GroupedStatsRequest struct {
	GroupBy      []string              `json:"groupBy"`
	Condition    json.RawMessage       `json:"condition,omitempty"`
	Aggregations []AggregationExprWire `json:"aggregations,omitempty"`
	PointInTime  *time.Time            `json:"pointInTime,omitempty"`
	Limit        *int                  `json:"limit,omitempty"`
}

// AggregationExprWire is one requested aggregation in the request body.
type AggregationExprWire struct {
	Op    string `json:"op"`
	Field string `json:"field"`
	As    string `json:"as,omitempty"`
}

// GroupedStatsBucket is one row of the response array.
type GroupedStatsBucket struct {
	GroupKey     []GroupKeyEntryWire `json:"groupKey"`
	Count        int64               `json:"count"`
	Aggregations map[string]any      `json:"aggregations,omitempty"`
}

// GroupKeyEntryWire is one (path, value) pair in a bucket's key.
type GroupKeyEntryWire struct {
	Path  string `json:"path"`
	Value any    `json:"value"`
}
