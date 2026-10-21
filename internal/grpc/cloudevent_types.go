package grpc

// CloudEvent type constants matching Cyoda's CloudEventType enum values.
// These must match exactly what the Java SDK sends.
//
// This file declares every event type the server accepts or sends, and only
// event types: each one has a schema of its own name in the published tree
// and a value in common/CloudEventType.json (cloudevent_schema_coverage_test.go
// reads this file to check both). Declare a new event type here, not elsewhere.

// Streaming / calculation types.
const (
	CalculationMemberJoinEvent      = "CalculationMemberJoinEvent"
	CalculationMemberGreetEvent     = "CalculationMemberGreetEvent"
	CalculationMemberKeepAliveEvent = "CalculationMemberKeepAliveEvent"

	EntityProcessorCalculationRequest  = "EntityProcessorCalculationRequest"
	EntityProcessorCalculationResponse = "EntityProcessorCalculationResponse"

	EntityCriteriaCalculationRequest  = "EntityCriteriaCalculationRequest"
	EntityCriteriaCalculationResponse = "EntityCriteriaCalculationResponse"

	EntityFunctionCalculationRequest  = "EntityFunctionCalculationRequest"
	EntityFunctionCalculationResponse = "EntityFunctionCalculationResponse"

	EventAckResponse = "EventAckResponse"
)

// Entity management types.
const (
	EntityCreateRequest           = "EntityCreateRequest"
	EntityCreateCollectionRequest = "EntityCreateCollectionRequest"
	EntityUpdateRequest           = "EntityUpdateRequest"
	EntityUpdateCollectionRequest = "EntityUpdateCollectionRequest"
	EntityTransactionResponse     = "EntityTransactionResponse"
	EntityDeleteRequest           = "EntityDeleteRequest"
	EntityDeleteResponse          = "EntityDeleteResponse"
	EntityDeleteAllRequest        = "EntityDeleteAllRequest"
	EntityDeleteAllResponse       = "EntityDeleteAllResponse"
	EntityTransitionRequest       = "EntityTransitionRequest"
	EntityTransitionResponse      = "EntityTransitionResponse"
	EntityPatchRequest            = "EntityPatchRequest"
)

// Model management types.
const (
	EntityModelImportRequest         = "EntityModelImportRequest"
	EntityModelImportResponse        = "EntityModelImportResponse"
	EntityModelExportRequest         = "EntityModelExportRequest"
	EntityModelExportResponse        = "EntityModelExportResponse"
	EntityModelTransitionRequest     = "EntityModelTransitionRequest"
	EntityModelTransitionResponse    = "EntityModelTransitionResponse"
	EntityModelDeleteRequest         = "EntityModelDeleteRequest"
	EntityModelDeleteResponse        = "EntityModelDeleteResponse"
	EntityModelGetAllRequest         = "EntityModelGetAllRequest"
	EntityModelGetAllResponse        = "EntityModelGetAllResponse"
	EntityModelSetUniqueKeysRequest  = "EntityModelSetUniqueKeysRequest"
	EntityModelSetUniqueKeysResponse = "EntityModelSetUniqueKeysResponse"
)

// Search / query types.
const (
	EntityGetRequest    = "EntityGetRequest"
	EntityGetAllRequest = "EntityGetAllRequest"

	EntitySnapshotSearchRequest  = "EntitySnapshotSearchRequest"
	EntitySnapshotSearchResponse = "EntitySnapshotSearchResponse"

	EntityResponse = "EntityResponse"

	SnapshotCancelRequest    = "SnapshotCancelRequest"
	SnapshotGetRequest       = "SnapshotGetRequest"
	SnapshotGetStatusRequest = "SnapshotGetStatusRequest"

	EntitySearchRequest = "EntitySearchRequest"

	EntityStatsGetRequest           = "EntityStatsGetRequest"
	EntityStatsResponse             = "EntityStatsResponse"
	EntityStatsByStateGetRequest    = "EntityStatsByStateGetRequest"
	EntityStatsByStateResponse      = "EntityStatsByStateResponse"
	EntityChangesMetadataGetRequest = "EntityChangesMetadataGetRequest"
	EntityChangesMetadataResponse   = "EntityChangesMetadataResponse"

	EntityConsistencyTimeGetRequest = "EntityConsistencyTimeGetRequest"
	EntityConsistencyTimeResponse   = "EntityConsistencyTimeResponse"
)
