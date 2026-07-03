package client

import (
	"time"
)

// EntityResult is the parity view of an entity envelope returned by:
//   - HTTP GET /entity/{entityId}                       (single)
//   - HTTP GET /entity/{entityName}/{modelVersion}      (list)
//   - HTTP POST /search/direct/{entityName}/{modelVersion}  (sync search element)
//   - HTTP GET /search/async/{jobId}                    (async search element)
//   - gRPC EntityResponse.payload                       (DataPayload)
//
// Mirrors EntityResult in docs/cyoda/api/openapi-entity-search.yml line 761
// and DataPayload in docs/cyoda/schema/common/DataPayload.json.
//
// Per approved deviation A1, this envelope shape is what the live Cyoda
// Cloud system returns from entity GET endpoints, even though the
// published OpenAPI shows bare data.
type EntityResult struct {
	Type string         `json:"type"` // always "ENTITY"
	Data map[string]any `json:"data"`
	Meta EntityMetadata `json:"meta"`
}

// EntityMetadata mirrors docs/cyoda/schema/common/EntityMetadata.json.
//
// Required by canonical: id, state, creationDate, lastUpdateTime.
// Optional: modelKey, pointInTime, transitionForLatestSave, transactionId.
//
// modelKey asymmetry (approved deviation A2): getOneEntity includes
// modelKey, getAllEntities and search responses do NOT. ModelKey is
// therefore *ModelSpec with omitempty so a single type works for both
// endpoints. Tests that receive a search/list response and assert on
// entity.Meta.ModelKey get nil; tests that GET a single entity get the
// populated model spec.
type EntityMetadata struct {
	ID                      string     `json:"id"`
	ModelKey                *ModelSpec `json:"modelKey,omitempty"`
	State                   string     `json:"state"`
	CreationDate            time.Time  `json:"creationDate"`
	LastUpdateTime          time.Time  `json:"lastUpdateTime"`
	PointInTime             *time.Time `json:"pointInTime,omitempty"`
	TransitionForLatestSave string     `json:"transitionForLatestSave,omitempty"`
	TransactionID           string     `json:"transactionId,omitempty"`
}

// ModelSpec mirrors docs/cyoda/schema/common/ModelSpec.json.
type ModelSpec struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// EntityTransactionInfo mirrors
// docs/cyoda/schema/entity/EntityTransactionInfo.json.
//
// Returned by HTTP entity create / update / delete / transition
// operations. (gRPC wraps it in EntityTransactionResponse {requestId,
// transactionInfo}; HTTP unwraps to return EntityTransactionInfo at the
// top level.)
type EntityTransactionInfo struct {
	TransactionID string   `json:"transactionId,omitempty"`
	EntityIDs     []string `json:"entityIds"`
}

// EntityChangeMeta mirrors docs/cyoda/schema/common/EntityChangeMeta.json.
//
// Returned as a JSON array by HTTP GET /entity/{entityId}/changes.
//
// Required: timeOfChange, user, changeType. Optional: transactionId,
// fieldsChangedCount. changeType enum: CREATE | UPDATE | DELETE.
type EntityChangeMeta struct {
	TransactionID      string    `json:"transactionId,omitempty"`
	TimeOfChange       time.Time `json:"timeOfChange"`
	User               string    `json:"user"`
	ChangeType         string    `json:"changeType"`
	FieldsChangedCount int       `json:"fieldsChangedCount,omitempty"`
}

// EntityModelDto mirrors the EntityModelDto component schema in
// docs/cyoda/openapi.yml.
//
// Returned as a JSON array by HTTP GET /model/.
//
// Note: there is no canonical GET /model/{name}/{version}. To verify a
// single model exists or is in a particular state, list all models and
// find by modelName + modelVersion.
type EntityModelDto struct {
	ID              string    `json:"id"`
	ModelName       string    `json:"modelName"`
	ModelVersion    int       `json:"modelVersion"`
	CurrentState    string    `json:"currentState"` // LOCKED | UNLOCKED
	ModelUpdateDate time.Time `json:"modelUpdateDate"`
}

// PagedEntityResults mirrors the PagedEntityResults component schema in
// docs/cyoda/api/openapi-entity-search.yml line 731.
//
// Returned by HTTP GET /search/async/{jobId} (async search results).
type PagedEntityResults struct {
	Content []EntityResult `json:"content"`
	Page    PageMetadata   `json:"page"`
}

// PageMetadata mirrors PageMetadata in
// docs/cyoda/api/openapi-entity-search.yml line 743.
type PageMetadata struct {
	Size          int   `json:"size"`
	TotalElements int64 `json:"totalElements"`
	TotalPages    int   `json:"totalPages"`
	Number        int   `json:"number"`
}

// AsyncSearchStatus mirrors AsyncSearchStatus in
// docs/cyoda/api/openapi-entity-search.yml line 684.
//
// Returned by HTTP GET /search/async/{jobId}/status.
type AsyncSearchStatus struct {
	SearchJobStatus       string     `json:"searchJobStatus"` // RUNNING|FAILED|CANCELLED|SUCCESSFUL|NOT_FOUND
	ExpirationDate        time.Time  `json:"expirationDate"`
	EntitiesCount         int64      `json:"entitiesCount"`
	CalculationTimeMillis int64      `json:"calculationTimeMillis"`
	CreateTime            time.Time  `json:"createTime"`
	FinishTime            *time.Time `json:"finishTime,omitempty"`
}

// GroupedStatsBucket mirrors internal/domain/entity.GroupedStatsBucket —
// the response shape of POST /api/entity/stats/{entityName}/{modelVersion}/query.
//
// The handler returns a JSON array of buckets, sorted by spec §6 D12
// (count desc, then group-key lexicographic). Aggregations is the
// per-alias map; the field is omitted when the request supplied no
// aggregations (count-only request).
type GroupedStatsBucket struct {
	GroupKey     []GroupKeyEntry `json:"groupKey"`
	Count        int64           `json:"count"`
	Aggregations map[string]any  `json:"aggregations,omitempty"`
}

// GroupKeyEntry is one (path, value) pair in a bucket's group key.
type GroupKeyEntry struct {
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// GroupedStatsRequest is the body of POST
// /api/entity/stats/{entityName}/{modelVersion}/query. Mirrors
// internal/domain/entity.GroupedStatsRequest; kept in the parity
// client to avoid pulling the domain package into the e2e import
// graph.
//
// Condition is a json.RawMessage on the server side; on the client we
// emit it as a raw string so callers can pass any JSON they like
// without round-tripping through map[string]any.
type GroupedStatsRequest struct {
	GroupBy      []string          `json:"groupBy"`
	Condition    *AggregationCond  `json:"condition,omitempty"`
	Aggregations []AggregationExpr `json:"aggregations,omitempty"`
	PointInTime  *time.Time        `json:"pointInTime,omitempty"`
	Limit        *int              `json:"limit,omitempty"`
}

// AggregationCond is a passthrough for the predicate.Condition JSON
// shape. We model it as map[string]any so tests can build conditions
// from Go literals without a JSON-string round trip.
type AggregationCond map[string]any

// AggregationExpr is one requested aggregation.
type AggregationExpr struct {
	Op    string `json:"op"`
	Field string `json:"field"`
	As    string `json:"as,omitempty"`
}
