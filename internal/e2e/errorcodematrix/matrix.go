// Package errorcodematrix declares the entity error codes the API documents.
// The e2e suite checks the declaration against the codes it produces
// (zzz_errorcode_matrix_test.go in internal/e2e), and this package's test
// checks it against api/openapi.yaml, so a code is part of the contract only
// when both agree.
package errorcodematrix

// Cell is one documented (status, errorCode) combination for an operation.
type Cell struct {
	Status int
	Code   string
}

// EntityErrorCodeMatrix declares, per in-scope entity operationId, the
// (status, errorCode) combinations the operation documents: each cell names
// its code in the operation's response description for that status in
// api/openapi.yaml (TestEveryCellIsNamedInTheSpec). The e2e suite-end check
// asserts bidirectional agreement with what the run actually produced, from
// every server the suite starts. Out-of-scope operationIds are absent and
// therefore exempt — the marker-aware coverage gate governs their coverage.
// A new code on an in-scope operation needs its cell here and its name in
// the response description.
var EntityErrorCodeMatrix = map[string][]Cell{
	// getOneEntity's error surface, pinned by TestErrCodeMatrix_GetOneEntity
	// (internal/e2e/zzz_errorcode_matrix_test.go) and the tests named on the cells.
	"getOneEntity": {
		{Status: 503, Code: "CONSISTENCY_TIME_UNAVAILABLE"},         // TestConsistencyUnavailable: the store cannot certify the consistency time within its wait budget
		{Status: 400, Code: "POINT_IN_TIME_AFTER_CONSISTENCY_TIME"}, // TestConsistencyFence: pointInTime later than the consistency time
		{Status: 404, Code: "ENTITY_NOT_FOUND"},
		{Status: 400, Code: "BAD_REQUEST"},               // conflicting pointInTime+transactionId
		{Status: 403, Code: "FORBIDDEN"},                 // TestCallbackErr_LoudFailCodes/CrossTenant_403
		{Status: 404, Code: "TRANSACTION_NOT_FOUND"},     // TestCallbackErr_LoudFailCodes/NotFound_404
		{Status: 410, Code: "CALLOUT_SUPERSEDED"},        // TestCalloutFence_LateCallback
		{Status: 410, Code: "TRANSACTION_EXPIRED"},       // TestCallbackErr_LoudFailCodes/Expired_410
		{Status: 413, Code: "JOINED_RESPONSE_TOO_LARGE"}, // TestCallbackBounds_AnswerOverTheCeiling_413
		{Status: 503, Code: "TOO_MANY_JOINED_REQUESTS"},  // TestCallbackBounds_TooManyQueuedCallbacks_503
		{Status: 503, Code: "STORAGE_UNAVAILABLE"},       // TestE2E_TornConnection_AuditAndTrustedKeys503/getOneEntity
	},
	"deleteEntities": {
		{Status: 503, Code: "CONSISTENCY_TIME_UNAVAILABLE"},         // TestConsistencyUnavailable: the store cannot certify the consistency time within its wait budget
		{Status: 400, Code: "POINT_IN_TIME_AFTER_CONSISTENCY_TIME"}, // TestConsistencyFence: pointInTime later than the consistency time
		{Status: 400, Code: "INVALID_CONDITION"},
		{Status: 400, Code: "INVALID_FIELD_PATH"},      // TestDeleteEntities_UnknownFieldPath: selection-search 4xx forwarded, not buried as 500
		{Status: 400, Code: "CONDITION_TYPE_MISMATCH"}, // TestDeleteConditional_TextOperatorOnNumericField_ConditionTypeMismatch_NothingDeleted
		{Status: 400, Code: "BAD_REQUEST"},             // TestTransactionControl_InvalidParams400/DeleteEntities: invalid/joined transactionSize
		{Status: 404, Code: "MODEL_NOT_FOUND"},
		// TestDeleteEntities_Batched_NonConvergence_409: the batched delete's
		// progress guard. Produced on that test's own mount of this endpoint
		// (the cycle budget has to be lowered to reach it), behind the same
		// conformance validator — so the triple is recorded here like any
		// other.
		{Status: 409, Code: "DELETE_NOT_CONVERGED"},
	},
	// Stats / list / search ops. Three read ops
	// have a bounded, bidirectionally-checkable error surface — only
	// 404 MODEL_NOT_FOUND in the current suite — and are tracked as full keys:
	//   {getAllEntities, getEntityStatisticsForModel, getEntityStatisticsByStateForModel}.
	// Three further ops are deliberately NOT matrix keys because their full error
	// surface is large:
	//   searchEntities: 404 MODEL_NOT_FOUND proven by TestSearchEntities_UnknownModel_404
	//     (search_unknown_model_test.go); 400 INVALID_FIELD_PATH has 8+ sub-cases
	//     (condition + sort variants); full surface not tracked.
	//   submitAsyncSearchJob: 404 MODEL_NOT_FOUND proven by
	//     TestSubmitAsyncSearchJob_UnknownModel_404; same INVALID_FIELD_PATH family;
	//     full surface not tracked.
	//   queryGroupedEntityStatisticsForModel: 404 MODEL_NOT_FOUND proven by
	//     TestGroupedStats_UnknownModel_404 (grouped_stats_test.go); full surface
	//     (MISSING_GROUP_BY, GROUP_CARDINALITY_EXCEEDED)
	//     not tracked.
	"getAllEntities": {
		{Status: 503, Code: "CONSISTENCY_TIME_UNAVAILABLE"},         // TestConsistencyUnavailable: the store cannot certify the consistency time within its wait budget
		{Status: 400, Code: "POINT_IN_TIME_AFTER_CONSISTENCY_TIME"}, // TestConsistencyFence: pointInTime later than the consistency time
		{Status: 404, Code: "MODEL_NOT_FOUND"},                      // TestGetAllEntities_UnknownModel_404
	},
	"getEntityStatisticsForModel": {
		{Status: 503, Code: "CONSISTENCY_TIME_UNAVAILABLE"},         // TestConsistencyUnavailable: the store cannot certify the consistency time within its wait budget
		{Status: 400, Code: "POINT_IN_TIME_AFTER_CONSISTENCY_TIME"}, // TestConsistencyFence: pointInTime later than the consistency time
		{Status: 404, Code: "MODEL_NOT_FOUND"},                      // TestGetStatisticsForModel_UnknownModel_404
	},
	"getEntityStatisticsByStateForModel": {
		{Status: 503, Code: "CONSISTENCY_TIME_UNAVAILABLE"},         // TestConsistencyUnavailable: the store cannot certify the consistency time within its wait budget
		{Status: 400, Code: "POINT_IN_TIME_AFTER_CONSISTENCY_TIME"}, // TestConsistencyFence: pointInTime later than the consistency time
		{Status: 404, Code: "MODEL_NOT_FOUND"},                      // TestGetStatisticsByStateForModel_UnknownModel_404
	},
	// Entity write operations. The matrix tracks the ops with a bounded,
	// bidirectionally-checkable error surface:
	//   {getOneEntity, deleteEntities, create, createCollection, updateSingle,
	//    updateSingleWithLoopback, patchSingleWithLoopback}.
	// updateCollection and patchSingle are deliberately NOT matrix keys: tracking
	// them would force declaring their entire transition-error surface. Their
	// composite-unique-key codes (updateCollection 409/422, patchSingle 409/422)
	// are instead proven by explicit e2e assertions in
	// unique_keys_write_variants_test.go.
	// CONFLICT (409) is exempt from all rows: it is a retryable serialization
	// abort emitted non-deterministically by any write op under concurrency and
	// is therefore not a per-endpoint documented code (see universalCrossCuttingCodes).
	"create": {
		{Status: 400, Code: "BAD_REQUEST"},       // invalid payload, transactionWindow out of range
		{Status: 400, Code: "INCOMPATIBLE_TYPE"}, // payload type mismatches the model
		// TestEntityCreate_UnaddressableFieldName_400: a write that would extend
		// the model with a field name the wire jsonPath grammar cannot address.
		{Status: 400, Code: "VALIDATION_FAILED"},
		{Status: 400, Code: "WORKFLOW_FAILED"},    // workflow processor rejected the entity
		{Status: 403, Code: "FORBIDDEN"},          // TestRouteGuard_DataRouteRequiresM2M: the caller lacks ROLE_M2M
		{Status: 404, Code: "MODEL_NOT_FOUND"},    // model not registered
		{Status: 409, Code: "UNIQUE_VIOLATION"},   // TestUniqueKeys_CreateDuplicate et al.
		{Status: 422, Code: "INVALID_UNIQUE_KEY"}, // TestUniqueKeys_PartialKeyCreate, TestUniqueKeys_OverBoundNumeric
		// TestModelKindBranch_KeyedPathCannotGainAContainer: a STRUCTURAL write
		// that would give a keyed path a container branch.
		{Status: 422, Code: "INVALID_UNIQUE_KEY_DEFINITION"},
		{Status: 404, Code: "TRANSACTION_NOT_FOUND"},            // TestCalloutFence_LateCallback: the X-Tx-Token names a transaction that is no longer open
		{Status: 408, Code: "TRANSACTION_TIMEOUT"},              // TestTransactionControl_WriteOps_TransactionTimeout408/Create
		{Status: 409, Code: "COMMIT_IN_JOINED_TRANSACTION"},     // TestCallback_CommitBeforeDispatchInJoinedTransaction_Refused
		{Status: 410, Code: "CALLOUT_SUPERSEDED"},               // TestCalloutFence_FirstCnodeRefusedOnceReplaced
		{Status: 410, Code: "TRANSACTION_EXPIRED"},              // TestCalloutPass: the token has passed its own expiry
		{Status: 500, Code: "SCHEDULE_FUNCTION_INVALID_RESULT"}, // TestScheduledFunction_MalformedResult_Returns500
		{Status: 503, Code: "CALLOUT_FAILED"},                   // TestCalloutErrors_EveryTryUsed
		{Status: 503, Code: "COMPUTE_MEMBER_DISCONNECTED"},      // TestCalloutFailover_Processor
		{Status: 503, Code: "DISPATCH_FORWARD_FAILED"},          // TestCalloutHandOver_AnswerLost_NotRepeatSafe
		{Status: 503, Code: "DISPATCH_TIMEOUT"},                 // TestCalloutErrors_OneAttemptIsNotWrapped
		{Status: 503, Code: "NO_COMPUTE_MEMBER_FOR_TAG"},        // TestProcessorNoMember_Returns503
		{Status: 503, Code: "STORAGE_UNAVAILABLE"},              // TestE2E_SaturatedPool_HTTPReturns503/create
	},
	"createCollection": {
		{Status: 400, Code: "BAD_REQUEST"},                      // invalid JSON array or parameter
		{Status: 404, Code: "MODEL_NOT_FOUND"},                  // one or more models not registered
		{Status: 409, Code: "UNIQUE_VIOLATION"},                 // TestUniqueKeys_CollectionIntraBatchDuplicate, TestUniqueKeys_MixedModelBatch
		{Status: 422, Code: "INVALID_UNIQUE_KEY"},               // TestUniqueKeys_CollectionPartialKeyCreate
		{Status: 400, Code: "WORKFLOW_FAILED"},                  // TestCalloutEndpoints_EveryDoorEveryCode/create-collection/terminal
		{Status: 408, Code: "TRANSACTION_TIMEOUT"},              // TestTransactionControl_WriteOps_TransactionTimeout408/CreateCollection
		{Status: 409, Code: "COMMIT_IN_JOINED_TRANSACTION"},     // TestCallback_JoinedCollection_NotChunked
		{Status: 503, Code: "CALLOUT_FAILED"},                   // TestCalloutEndpoints_EveryDoorEveryCode/create-collection/every-try-used
		{Status: 503, Code: "COMPUTE_MEMBER_DISCONNECTED"},      // TestCalloutEndpoints_EveryDoorEveryCode/create-collection/one-try-dropped
		{Status: 503, Code: "DISPATCH_TIMEOUT"},                 // TestCalloutEndpoints_EveryDoorEveryCode/create-collection/one-try-no-answer
		{Status: 503, Code: "NO_COMPUTE_MEMBER_FOR_TAG"},        // TestCalloutEndpoints_EveryDoorEveryCode/create-collection/no-member
		{Status: 403, Code: "FORBIDDEN"},                        // TestCalloutPass/another-tenant-403: a pass of another tenant
		{Status: 404, Code: "TRANSACTION_NOT_FOUND"},            // TestCalloutFence_LateCallback
		{Status: 410, Code: "CALLOUT_SUPERSEDED"},               // TestCalloutFence_LateCallback
		{Status: 410, Code: "TRANSACTION_EXPIRED"},              // TestCalloutPass/expired-410
		{Status: 503, Code: "DISPATCH_FORWARD_FAILED"},          // TestCalloutHandOver_AnswerLost_NotRepeatSafe/*/http-collection
		{Status: 503, Code: "STORAGE_UNAVAILABLE"},              // TestE2E_SaturatedPool_HTTPReturns503/create-collection
		{Status: 500, Code: "SCHEDULE_FUNCTION_INVALID_RESULT"}, // TestScheduledFunction_MalformedResult_Returns500/create-collection
	},
	"updateSingleWithLoopback": {
		{Status: 400, Code: "BAD_REQUEST"},        // unparseable body, or a payload carrying U+0000 (TestEntity_NulInPayload_400)
		{Status: 400, Code: "VALIDATION_FAILED"},  // data that does not fit the model, e.g. not a JSON object (TestModelExtension_RootStaysAnObject)
		{Status: 400, Code: "WORKFLOW_FAILED"},    // engine rejected the loopback — e.g. an unevaluable workflow selection criterion (TestWorkflowSelection_UnevaluableCriterionFailsClosedOnEveryDoor)
		{Status: 409, Code: "UNIQUE_VIOLATION"},   // TestUniqueKeys_UpdateMovesKey
		{Status: 412, Code: "ENTITY_MODIFIED"},    // TestIfMatch_StaleLoopbackPut412
		{Status: 422, Code: "INVALID_UNIQUE_KEY"}, // TestUniqueKeys_LoopbackUpdatePartialKey
		// 409 CONFLICT is exempt (universalCrossCuttingCodes)
		{Status: 404, Code: "ENTITY_NOT_FOUND"},                 // TestE2E_UpdateSingleEntity_StorageFailureIsNotNotFound: a genuine miss is still a 404
		{Status: 404, Code: "TRANSACTION_NOT_FOUND"},            // TestCalloutFence_LateCallback
		{Status: 408, Code: "TRANSACTION_TIMEOUT"},              // TestTransactionControl_WriteOps_TransactionTimeout408/UpdateSingleWithLoopback
		{Status: 409, Code: "COMMIT_IN_JOINED_TRANSACTION"},     // TestCallback_CommitBeforeDispatchInJoinedTransaction_Refused
		{Status: 410, Code: "CALLOUT_SUPERSEDED"},               // TestCalloutFence_FirstCnodeRefusedOnceReplaced
		{Status: 503, Code: "CALLOUT_FAILED"},                   // TestCalloutEndpoints_EveryDoorEveryCode/loopback/every-try-used
		{Status: 503, Code: "COMPUTE_MEMBER_DISCONNECTED"},      // TestCalloutEndpoints_EveryDoorEveryCode/loopback/one-try-dropped
		{Status: 503, Code: "DISPATCH_TIMEOUT"},                 // TestCalloutEndpoints_EveryDoorEveryCode/loopback/one-try-no-answer
		{Status: 503, Code: "NO_COMPUTE_MEMBER_FOR_TAG"},        // TestCalloutEndpoints_EveryDoorEveryCode/loopback/no-member
		{Status: 403, Code: "FORBIDDEN"},                        // TestCalloutPass/another-tenant-403
		{Status: 410, Code: "TRANSACTION_EXPIRED"},              // TestCalloutPass/expired-410
		{Status: 503, Code: "STORAGE_UNAVAILABLE"},              // TestE2E_SaturatedPool_HTTPReturns503/loopback
		{Status: 500, Code: "SCHEDULE_FUNCTION_INVALID_RESULT"}, // TestScheduledFunction_MalformedResult_Returns500/loopback
	},
	"updateSingle": {
		{Status: 400, Code: "TRANSITION_NOT_FOUND"},             // named transition absent from the model
		{Status: 400, Code: "WORKFLOW_FAILED"},                  // workflow processor rejected the update
		{Status: 400, Code: "BAD_REQUEST"},                      // TestTransactionControl_InvalidParams400/UpdateSingle: invalid/joined transactionTimeoutMillis; TestUpdateSingle_TransitionNameHoldingNUL_400
		{Status: 404, Code: "ENTITY_NOT_FOUND"},                 // entity UUID not found
		{Status: 409, Code: "UNIQUE_VIOLATION"},                 // TestUniqueKeys_ProcessorRewrite_IfMatchUpdate_409
		{Status: 412, Code: "ENTITY_MODIFIED"},                  // If-Match mismatch
		{Status: 422, Code: "INVALID_UNIQUE_KEY"},               // TestUniqueKeys_TransitionUpdatePartialKey
		{Status: 408, Code: "TRANSACTION_TIMEOUT"},              // TestTransactionControl_WriteOps_TransactionTimeout408/UpdateSingle
		{Status: 409, Code: "COMMIT_IN_JOINED_TRANSACTION"},     // TestCallback_CommitBeforeDispatchInJoinedTransaction_Refused
		{Status: 503, Code: "CALLOUT_FAILED"},                   // TestCalloutEndpoints_EveryDoorEveryCode/update-transition/every-try-used
		{Status: 503, Code: "COMPUTE_MEMBER_DISCONNECTED"},      // TestCalloutEndpoints_EveryDoorEveryCode/update-transition/one-try-dropped
		{Status: 503, Code: "DISPATCH_TIMEOUT"},                 // TestCalloutEndpoints_EveryDoorEveryCode/update-transition/one-try-no-answer
		{Status: 503, Code: "NO_COMPUTE_MEMBER_FOR_TAG"},        // TestCalloutEndpoints_EveryDoorEveryCode/update-transition/no-member
		{Status: 403, Code: "FORBIDDEN"},                        // TestCalloutPass/another-tenant-403
		{Status: 404, Code: "TRANSACTION_NOT_FOUND"},            // TestCalloutFence_LateCallback
		{Status: 410, Code: "CALLOUT_SUPERSEDED"},               // TestCalloutFence_LateCallback
		{Status: 410, Code: "TRANSACTION_EXPIRED"},              // TestCalloutPass/expired-410
		{Status: 503, Code: "DISPATCH_FORWARD_FAILED"},          // TestCalloutHandOver_AnswerLost_NotRepeatSafe/*/http-update-transition
		{Status: 503, Code: "STORAGE_UNAVAILABLE"},              // TestE2E_SaturatedPool_HTTPReturns503/update-transition
		{Status: 500, Code: "SCHEDULE_FUNCTION_INVALID_RESULT"}, // TestScheduledFunction_MalformedResult_Returns500/update-transition
	},
	"patchSingleWithLoopback": {
		{Status: 400, Code: "BAD_REQUEST"}, // TestTransactionControl_InvalidParams400/PatchSingleWithLoopback: invalid/joined transactionTimeoutMillis
		// TestModelKindEnforcement_PatchRejectsKindMismatch: a patch is validated
		// strictly — it must never widen the model — so a value whose kind the
		// field does not declare fails against the registered model.
		{Status: 400, Code: "VALIDATION_FAILED"},
		{Status: 409, Code: "UNIQUE_VIOLATION"},                 // TestUniqueKeys_LoopbackPatchDuplicate
		{Status: 412, Code: "ENTITY_MODIFIED"},                  // If-Match transactionId no longer matches
		{Status: 415, Code: "UNSUPPORTED_MEDIA_TYPE"},           // non-JSON format or unrecognised Content-Type
		{Status: 422, Code: "INVALID_UNIQUE_KEY"},               // TestUniqueKeys_PatchNullsKeyField
		{Status: 428, Code: "PRECONDITION_REQUIRED"},            // If-Match header absent
		{Status: 501, Code: "NOT_IMPLEMENTED"},                  // application/json-patch+json not yet implemented
		{Status: 408, Code: "TRANSACTION_TIMEOUT"},              // TestTransactionControl_WriteOps_TransactionTimeout408/PatchSingleWithLoopback
		{Status: 409, Code: "COMMIT_IN_JOINED_TRANSACTION"},     // TestCallback_CommitBeforeDispatchInJoinedTransaction_Refused
		{Status: 403, Code: "FORBIDDEN"},                        // TestCalloutPass/another-tenant-403
		{Status: 404, Code: "TRANSACTION_NOT_FOUND"},            // TestCalloutFence_LateCallback
		{Status: 410, Code: "CALLOUT_SUPERSEDED"},               // TestCalloutFence_LateCallback
		{Status: 410, Code: "TRANSACTION_EXPIRED"},              // TestCalloutPass/expired-410
		{Status: 400, Code: "WORKFLOW_FAILED"},                  // TestCalloutEndpoints_EveryDoorEveryCode/patch/terminal
		{Status: 503, Code: "CALLOUT_FAILED"},                   // TestCalloutEndpoints_EveryDoorEveryCode/patch/every-try-used
		{Status: 503, Code: "COMPUTE_MEMBER_DISCONNECTED"},      // TestCalloutEndpoints_EveryDoorEveryCode/patch/one-try-dropped
		{Status: 503, Code: "DISPATCH_TIMEOUT"},                 // TestCalloutEndpoints_EveryDoorEveryCode/patch/one-try-no-answer
		{Status: 503, Code: "NO_COMPUTE_MEMBER_FOR_TAG"},        // TestCalloutEndpoints_EveryDoorEveryCode/patch/no-member
		{Status: 503, Code: "STORAGE_UNAVAILABLE"},              // TestE2E_SaturatedPool_HTTPReturns503/patch
		{Status: 500, Code: "SCHEDULE_FUNCTION_INVALID_RESULT"}, // TestScheduledFunction_MalformedResult_Returns500/patch
		{Status: 404, Code: "ENTITY_NOT_FOUND"},                 // TestE2E_EntityPatch_MergeAndPreconditions: an unknown entity id
	},
	// GET /scheduled-tasks. 401 and 500 are cross-cutting
	// (universalCrossCuttingCodes in internal/e2e/zzz_errorcode_matrix_test.go).
	"listScheduledTasks": {
		{Status: 400, Code: "BAD_REQUEST"},         // TestScheduledTasks_InvalidParameters_400
		{Status: 503, Code: "STORAGE_UNAVAILABLE"}, // TestScheduledTasks_TornConnection_503
	},
}
