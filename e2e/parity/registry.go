package parity

import "testing"

// Total parity scenarios: 220 (guarded by TestParityScenarioCount — bump
// wantParityScenarioCount in registry_count_test.go when adding/removing an
// entry, or the test fails).
// (Phase 1 smoke + Phase 4a CRUD/persistence + Phase 4b workflow/compute +
// distributed-safety contracts + schema extensions + grouped stats +
// unknown-model 404 contract).
// ExternalAPI scenarios registered via parity.Register() in e2e/parity/externalapi/,
// scheduled-transition-runtime scenarios registered via parity.Register()
// in e2e/parity/scheduledtransition/, and scheduled-transition Function
// scenarios registered via parity.Register() in e2e/parity/scheduledfunction/
// are additional to this count.
//
// Unmigrated internal/e2e/ tests (40 remaining): entity lifecycle,
// model extension, transaction stress tests, workflow failure paths,
// workflow loopback/cascade-depth/multi-processor, search string operators,
// message batch delete, workflow overwrite/export-empty. These continue
// to run as postgres-only tests and will be migrated to the parity suite
// in a follow-up effort.

// NamedTest is a single parity scenario plus the name under which it
// shows up in subtest output.
type NamedTest struct {
	Name string
	Fn   func(t *testing.T, fixture BackendFixture)
}

// allTests is the canonical list of parity scenarios. Per-backend
// wrappers iterate this list and run every entry against their fixture.
//
// Adding a scenario: add one new entry here AND create the corresponding
// Run* function in a topical file (e.g. entity.go, workflow_proc.go).
// Every backend wrapper picks the new entry up automatically — there is
// no per-backend wiring to forget.
var allTests = []NamedTest{
	// Phase 1 — smoke test
	{"SmokeTest", RunSmokeTest},

	// Phase 4a — model lifecycle (Task 4a.1)
	{"ModelImportAndExport", RunModelImportAndExport},
	{"ModelLockAndUnlock", RunModelLockAndUnlock},
	{"ModelListModels", RunModelListModels},
	{"ModelDelete", RunModelDelete},
	{"WorkflowImportExport", RunWorkflowImportExport},
	// What import accepts and refuses in a callout's configuration, and that
	// the accepted fields survive each backend's storage.
	{"WorkflowImportCalloutRetryPolicyValidated", RunWorkflowImportCalloutRetryPolicyValidated},
	{"WorkflowImportResponseTimeoutBounded", RunWorkflowImportResponseTimeoutBounded},
	{"WorkflowCalloutFieldsRoundTrip", RunWorkflowCalloutFieldsRoundTrip},
	{"WorkflowAnnotationsRoundTrip", RunWorkflowAnnotationsRoundTrip},
	{"WorkflowProcCriterionAnnotationsRoundTrip", RunWorkflowProcCriterionAnnotationsRoundTrip},
	{"WorkflowProcAttachEntityDefaultRoundTrip", RunWorkflowProcAttachEntityDefaultRoundTrip},

	// Phase 4a — entity CRUD (Task 4a.2)
	{"EntityCreateAndGet", RunEntityCreateAndGet},
	{"EntityNulPayloadRejected", RunEntityNulPayloadRejected},
	{"EntityUnstorableTextRejected", RunEntityUnstorableTextRejected},
	{"EntityEmptyDocumentRoundTrips", RunEntityEmptyDocumentRoundTrips},
	{"EntityNumberOutOfRangeRejected", RunEntityNumberOutOfRangeRejected},
	{"EntityDuplicateKeysRejected", RunEntityDuplicateKeysRejected},
	{"EntityDelete", RunEntityDelete},
	{"EntityDeleteAllPointInTime", RunEntityDeleteAllPointInTime},
	{"EntityDeleteAllVerbose", RunEntityDeleteAllVerbose},
	{"EntityListByModel", RunEntityListByModel},
	{"EntityMetaShape", RunEntityMetaShape},
	{"GetAllEntitiesAsAt", RunGetAllEntitiesAsAt},
	// GetPage paging contract (task E5): determinism, page0++page1 ==
	// double-wide page, and set-equality with the full model — deliberately
	// NOT a specific cross-engine id sequence, since GetPage's canonical
	// order is per-engine (documented on spi.EntityStore.GetPage).
	{"ListEntitiesPagingConsistency", RunListEntitiesPagingConsistency},
	{"EntityConditionalDeleteInTx", RunEntityConditionalDeleteInTx},
	{"EntityUpdateCollectionHappyPath", RunEntityUpdateCollectionHappyPath},
	{"EntityUpdateCollectionRollback", RunEntityUpdateCollectionRollback},

	// Phase 4a — bi-temporal (Task 4a.3)
	{"TemporalPointInTimeRetrieval", RunTemporalPointInTimeRetrieval},
	{"TemporalGetAsAtPopulatesFullMeta", RunTemporalGetAsAtPopulatesFullMeta},
	{"PITBoundaryExactT", RunPITBoundaryExactT},
	{"PITDeletedSinceInstant", RunPITDeletedSinceInstant},
	{"PITCreatedAfterInstant", RunPITCreatedAfterInstant},

	// Phase 4a — audit (Task 4a.4)
	{"AuditEntityHistory", RunAuditEntityHistory},
	{"AuditWorkflowEvents", RunAuditWorkflowEvents},
	{"AuditPostTxIdMatchesWorkflowFinished", RunAuditPostTxIdMatchesWorkflowFinished},
	{"AuditCommitInstantSharedWithVersionHistory", RunAuditCommitInstantSharedWithVersionHistory},
	{"AuditIdentityJoinedSaves", RunAuditIdentityJoinedSaves},
	{"AuditCursorWalkOverTie", RunAuditCursorWalkOverTie},
	{"AuditFinishedEventIDMatchesSearch", RunAuditFinishedEventIDMatchesSearch},
	{"AuditFinishedEventIsLatestOfTransaction", RunAuditFinishedEventIsLatestOfTransaction},

	// History reads (task E6): getEntityChangesMetadata's
	// newest-first/Version-DESC-tiebreak/tombstone-HasEntity contract and
	// getOneEntity's by-transaction lookup, both now backed by
	// spi.EntityStore.GetVersionMetadata / GetVersionByTransaction.
	{"HistoryReadsChangesMetadataAndTransactionLookup", RunHistoryReadsChangesMetadataAndTransactionLookup},

	// Phase 4a — tenant isolation (Task 4a.5)
	{"TenantIsolationEntities", RunTenantIsolationEntities},
	{"TenantIsolationModels", RunTenantIsolationModels},
	// v0.6.3 — temporal-query tenant isolation (existence-oracle pinning;
	// companions to the tenant-isolation helpers). Structurally guaranteed today;
	// pinned here so a future refactor cannot silently regress.
	{"TenantIsolationTransactionIDInvisible", RunTenantIsolationTransactionIDInvisible},
	{"TenantIsolationTransitionsTransactionIDRejected", RunTenantIsolationTransitionsTransactionIDRejected},
	{"TenantIsolationPointInTimeInvisible", RunTenantIsolationPointInTimeInvisible},
	{"TenantIsolationChangesAtPITInvisible", RunTenantIsolationChangesAtPITInvisible},
	{"TenantIsolationWorkflowFinishedInvisible", RunTenantIsolationWorkflowFinishedInvisible},

	// Phase 4a — messaging (Task 4a.6)
	{"MessageCreateAndGet", RunMessageCreateAndGet},
	{"MessageDelete", RunMessageDelete},
	{"MessageLargePayload", RunMessageLargePayload},
	// A batch delete naming an absent id alongside a present one still
	// succeeds and deletes the present one (storage-SPI absent-key Delete
	// contract).
	{"MessageDeleteBatchWithAbsentID", RunMessageDeleteBatchWithAbsentID},

	// Edge message — flat metaData round-trip (Task 7, group 4)
	{"MessageRoundTrip", RunMessageRoundTrip},

	// Phase 4a — schema symmetry (Task 4a.7)
	{"DeepSchemaSymmetry", RunDeepSchemaSymmetry},

	// Phase 4a — empty tenant + search consistency (Task 4a.8)
	{"EmptyTenantOperations", RunEmptyTenantOperations},
	{"SearchIndexImmediateConsistency", RunSearchIndexImmediateConsistency},

	// Phase 4b — workflow + processors + criteria (Tasks 4b.2-5)
	{"WorkflowProcessorChainOnCreation", RunWorkflowProcessorChainOnCreation},
	{"WorkflowCriteriaMatch", RunWorkflowCriteriaMatch},
	{"WorkflowCriteriaNoMatch", RunWorkflowCriteriaNoMatch},
	{"WorkflowMultiStateCascade", RunWorkflowMultiStateCascade},
	{"WorkflowManualTransition", RunWorkflowManualTransition},

	// Phase 4b — search scenarios (Task 4b.6-8)
	{"SearchSimpleCondition", RunSearchSimpleCondition},
	{"SearchFunctionCondition400", RunSearchFunctionCondition400},
	{"SearchFunctionConditionNestedInGroup400", RunSearchFunctionConditionNestedInGroup400},
	{"SearchBoolCondition", RunSearchBoolCondition},
	{"SearchLifecycleCondition", RunSearchLifecycleCondition},
	{"SearchGroupCondition", RunSearchGroupCondition},
	{"SearchNoMatches", RunSearchNoMatches},
	{"SearchAfterUpdate", RunSearchAfterUpdate},
	{"SearchPointInTime", RunSearchPointInTime},
	{"SearchDirectBoundedOrFail", RunSearchDirectBoundedOrFail},

	// Temporal search filters — chronological date-typed meta
	// compare + meta-vocabulary reconciliation, cross-backend.
	{"SearchTemporalCreationDate", RunSearchTemporalCreationDate},
	{"SearchTemporalLastUpdateTime", RunSearchTemporalLastUpdateTime},
	{"SearchUnknownMetaField400", RunSearchUnknownMetaField400},

	// Field-path spelling and resolution. A jsonPath is JSON Path
	// nomenclature — the "$." leader is required on every path surface a
	// request carries — while an array-subscripted path stays served by the
	// in-memory fallback; and the storage layer's own _meta block is not
	// addressable as entity data. All were backend-visible, so all belong
	// here rather than in a single-backend test.
	{"SearchPathRequiresJSONPathLeader", RunSearchPathRequiresJSONPathLeader},
	{"SearchArraySubscriptPathStillServed", RunSearchArraySubscriptPathStillServed},
	{"SearchPathTypeMismatch400", RunSearchPathTypeMismatch400},
	// The same grammar governs a workflow/transition criterion, enforced at
	// workflow import; and a path addressing one array element by position
	// resolves to that element on every surface. Both are per-backend claims:
	// a criterion is stored per backend and re-read on every write, and which
	// plan a subscripted query takes differs per backend.
	{"WorkflowCriterionPathRequiresJSONPathLeader", RunWorkflowCriterionPathRequiresJSONPathLeader},
	{"PositionalSubscriptPathResolves", RunPositionalSubscriptPathResolves},
	// ...and a path whose LAST hop is a wildcard addresses the array's
	// ELEMENTS, not its length, on both surfaces.
	{"SearchTrailingWildcardPathResolves", RunSearchTrailingWildcardPathResolves},
	{"GroupedStatsPathRequiresJSONPathLeader", RunGroupedStatsPathRequiresJSONPathLeader},
	{"SearchMetaBlockNotMatchableAsDataPath", RunSearchMetaBlockNotMatchableAsDataPath},
	{"SearchStringMetaVocabulary", RunSearchStringMetaVocabulary},
	{"SearchBetweenArity400", RunSearchBetweenArity400},
	// Group-identity contract: an explicit empty AND matches everything, an
	// explicit empty OR matches nothing — standalone and nested. The SQL
	// planners previously pushed a childless OR as an empty WHERE fragment,
	// flipping "match nothing" into "match everything".
	{"SearchEmptyGroupIdentities", RunSearchEmptyGroupIdentities},
	// Type-directed contract: a scalar comparison on a PURE-container path (a
	// known structural interior with substructure but no scalar observation)
	// is rejected with HTTP 400 INVALID_FIELD_PATH uniformly across backends —
	// fail-closed rather than the pre-fix silent empty-result degradation.
	{"SearchScalarOnContainerPath400", RunSearchScalarOnContainerPath400},

	// Path grammar — addressing rules (docs/cloud-parity/path-grammar.md
	// §§3-5, 8). ArrayClausePositional reproduces the original defect: an
	// "array" clause's positional leaf resolved to a DOTTED index, which a
	// dotted numeric segment is a field name (not an index) for — memory's
	// evaluator matched anyway, both SQL backends did not. Only a scenario
	// asserting an exact count across all three backends catches that
	// asymmetry. PathAddressingByDeclaredShape and PathVacuity are the
	// union rule and the presence/nullness table, which apply to the array
	// clause's desugared form the same as to every other path.
	{"ArrayClausePositional", RunArrayClausePositional},
	{"PathAddressingByDeclaredShape", RunPathAddressingByDeclaredShape},
	{"PathVacuity", RunPathVacuity},

	// Phase 4b — workflow selection (Task 4b.7). Selection applies on every
	// engine door, so the post-creation doors are pinned alongside creation.
	{"WorkflowCriteriaSelectingWorkflow", RunWorkflowCriteriaSelectingWorkflow},
	{"WorkflowSelectionAfterCreation", RunWorkflowSelectionAfterCreation},

	// Phase 4b — distributed-safety contracts (Tasks 4b.9-10)
	{"ConcurrentConflictingUpdate", RunConcurrentConflictingUpdate},
	{"ConcurrentTransitionsDifferentEntities", RunConcurrentTransitionsDifferentEntities},

	// Compute-node callback transaction-join — backend-agnostic join
	// invariants driven through the callback-capable compute-test-client.
	// Concurrency/torn-write cases are intentionally NOT here (isolated e2e).
	{"CallbackTxJoin_SyncWriteAtomic", RunCallbackSyncWriteAtomic},
	{"CallbackTxJoin_SyncReadYourWrites", RunCallbackSyncReadYourWrites},
	{"CallbackTxJoin_GRPCSearchReadYourWrites", RunCallbackGRPCSearchReadYourWrites},
	{"CallbackTxJoin_CriteriaReadYourWrites", RunCallbackCriteriaReadYourWrites},
	{"CallbackTxJoin_IfMatchUpdate", RunCallbackIfMatchUpdate},
	{"CallbackTxJoin_EmptyTokenStandalone", RunCallbackEmptyTokenStandalone},
	{"CallbackTxJoin_CBDPostJoinsTxPost", RunCallback_CBDPostJoinsTxPost},
	{"CallbackTxJoin_AsyncNewTxDiscardOnFailure", RunCallback_AsyncNewTxDiscardOnFailure},
	{"CallbackTxJoin_PITCommittedOnly", RunPITCommittedOnlyInJoinedTx},
	{"CallbackTxJoin_CommitBeforeDispatchRefused", RunCallbackJoinedCommitBeforeDispatchRefused},

	// Compute-client capability self-tests: a fixture that can start further
	// compute clients proves it here; one that cannot skips.
	{"ComputeClientJoinServeLeave", RunComputeClientJoinServeLeave},
	{"ComputeClientBehaviours", RunComputeClientBehaviours},

	// Callout failover: what ends a callout and what the client is told. Each
	// case starts its own compute clients under a fresh tenant; a fixture that
	// cannot start them skips.
	{"CalloutNoAnswerNotIdempotentStops", RunCalloutNoAnswerNotIdempotentStops},
	{"CalloutCriterionFailsOver", RunCalloutCriterionFailsOver},
	{"CalloutFunctionFailsOver", RunCalloutFunctionFailsOver},
	{"CalloutMemberFailedStops", RunCalloutMemberFailedStops},
	{"CalloutRetryPolicyNoneOneTry", RunCalloutRetryPolicyNoneOneTry},
	{"CalloutEveryTryUsed", RunCalloutEveryTryUsed},

	// A.1 — numeric classifier parity (HTTP round-trip)
	{"NumericClassification18DigitDecimal", RunNumericClassification18DigitDecimal},
	{"NumericClassification20DigitDecimal", RunNumericClassification20DigitDecimal},
	{"NumericClassificationLargeInteger", RunNumericClassificationLargeInteger},
	{"NumericClassificationIntegerSchemaAcceptsInteger", RunNumericClassificationIntegerSchemaAcceptsInteger},
	{"NumericClassificationIntegerSchemaRejectsDecimal", RunNumericClassificationIntegerSchemaRejectsDecimal},
	{"NumericClassificationDoubleSchemaAcceptsWholeNumber", RunNumericClassificationDoubleSchemaAcceptsWholeNumber},

	// Schema extensions — sequential fold across requests
	{"SchemaExtensionsSequentialFoldAcrossRequests", RunSchemaExtensionsSequentialFoldAcrossRequests},
	{"SchemaExtensionCrossBackendByteIdentity", RunSchemaExtensionCrossBackendByteIdentity},
	{"SchemaExtensionAtomicRejection", RunSchemaExtensionAtomicRejection},
	{"SchemaExtensionConcurrentConvergence", RunSchemaExtensionConcurrentConvergence},
	{"SchemaExtensionSavepointOnLockFoldEquivalence", RunSchemaExtensionSavepointOnLockFoldEquivalence},
	{"SchemaExtensionLocalCacheInvalidationOnCommit", RunSchemaExtensionLocalCacheInvalidationOnCommit},
	{"SchemaExtensionByteIdentityProperty", RunSchemaExtensionByteIdentityProperty},
	{"SchemaNumericFoldCarveout", RunSchemaNumericFoldCarveout},

	// Type admission (design §4-9): one traversal judging each value against
	// the stored model, rather than converting the document to a throwaway
	// model and comparing labels. See type_admission.go.
	{"TypeAdmissionHeldValueUnchanged", RunTypeAdmissionHeldValueUnchanged},
	{"TypeAdmissionHeldThenFound", RunTypeAdmissionHeldThenFound},
	{"TypeAdmissionDoubleCeiling", RunTypeAdmissionDoubleCeiling},
	{"TypeAdmissionMixedKindArray", RunTypeAdmissionMixedKindArray},
	{"TypeAdmissionSearchEqualsTrailingZeros", RunTypeAdmissionSearchEqualsTrailingZeros},
	{"TypeAdmissionRegistrationYieldsStringLocalDate", RunTypeAdmissionRegistrationYieldsStringLocalDate},
	{"TypeAdmissionStrictNeverMorePermissiveThanArrayLength", RunTypeAdmissionStrictNeverMorePermissiveThanArrayLength},
	{"TypeAdmissionLongerArrayHeldAtEveryLevel", RunTypeAdmissionLongerArrayHeldAtEveryLevel},

	{"ModelFieldNameRejected", RunModelFieldNameRejected},
	{"ModelKindEnforcementRejected", RunModelKindEnforcementRejected},
	{"ModelKindBranchExtension", RunModelKindBranchExtension},
	{"ModelSampleDataCollectionImport", RunModelSampleDataCollectionImport},

	// Entity PATCH (RFC 7386 merge-patch) — cross-backend contract matrix.
	// Normal operation.
	{"EntityPatchMergePreservesFields", RunEntityPatchMergePreservesFields},
	{"EntityPatchNullDeletesKey", RunEntityPatchNullDeletesKey},
	{"EntityPatchNestedMerge", RunEntityPatchNestedMerge},
	{"EntityPatchArrayWholesaleReplace", RunEntityPatchArrayWholesaleReplace},
	{"EntityPatchEmptyNoOp", RunEntityPatchEmptyNoOp},
	{"EntityPatchNumberFidelity", RunEntityPatchNumberFidelity},
	{"EntityPatchStarUnconditional", RunEntityPatchStarUnconditional},
	{"EntityPatchWithTransition", RunEntityPatchWithTransition},
	// Error scenarios.
	{"EntityPatchNotFound", RunEntityPatchNotFound},
	{"EntityPatchStaleTokenIs412", RunEntityPatchStaleTokenIs412},
	{"EntityPatchXMLFormatIs415", RunEntityPatchXMLFormatIs415},
	{"EntityPatchWrongContentTypeIs415", RunEntityPatchWrongContentTypeIs415},
	{"EntityPatchMissingIfMatchIs428", RunEntityPatchMissingIfMatchIs428},
	{"EntityPatchJSONPatchNotImplemented", RunEntityPatchJSONPatchNotImplemented},
	{"EntityPatchTypeMismatchIs400", RunEntityPatchTypeMismatchIs400},
	{"EntityPatchStrictRejectsUnknownField", RunEntityPatchStrictRejectsUnknownField},
	// Cross-tenant isolation.
	{"EntityPatchCrossTenantIsNotFound", RunEntityPatchCrossTenantIsNotFound},

	// Composite unique keys — cross-backend parity matrix (spec §8.3).
	// Capability gate in each scenario: SetUniqueKeysRaw on the unlocked model;
	// if status==422+COMPOSITE_KEY_UNSUPPORTED → t.Skip (commercial backend
	// safe skip). All three in-repo backends run all assertions.
	{"UniqueKeys_CreateDuplicate", RunUniqueKeys_CreateDuplicate},
	{"UniqueKeys_SoftDeleteFreesValue", RunUniqueKeys_SoftDeleteFreesValue},
	{"UniqueKeys_PartialKey", RunUniqueKeys_PartialKey},
	{"UniqueKeys_AllNullExempt", RunUniqueKeys_AllNullExempt},
	{"UniqueKeys_DeleteAllFreesValues", RunUniqueKeys_DeleteAllFreesValues},
	{"UniqueKeys_MultipleKeys", RunUniqueKeys_MultipleKeys},
	{"UniqueKeys_UpdateClearsAllKeyFields", RunUniqueKeys_UpdateClearsAllKeyFields},
	// Centerpiece: a processor rewrites the key field → enforcement is on the
	// POST-MERGE value, not the input (two distinct inputs collide → 409).
	{"UniqueKeys_ProcessorRewritesKeyField", RunUniqueKeys_ProcessorRewritesKeyField},

	// Search sort — cross-backend sort ordering contract (Task 15).
	// Each scenario seeds entities, issues a sorted sync search, and asserts
	// the exact entity-id sequence.  Divergence here is a real bug in the
	// backend comparator or default-order path, not a test weakness.
	{"SearchSortDataText", RunSearchSortDataText},
	{"SearchSortDataTemporalLexical", RunSearchSortDataTemporalLexical},
	{"SearchSortDataNumeric", RunSearchSortDataNumeric},
	{"SearchSortDataBool", RunSearchSortDataBool},
	{"SearchSortMetaCreationDate", RunSearchSortMetaCreationDate},
	{"SearchSortMetaState", RunSearchSortMetaState},
	{"SearchSortMultiKeyTiebreaker", RunSearchSortMultiKeyTiebreaker},
	{"SearchSortNullsLast", RunSearchSortNullsLast},
	{"SearchSortPointInTime", RunSearchSortPointInTime},
	{"SearchSortDataFieldNamedMeta", RunSearchSortDataFieldNamedMeta},
	{"SearchNoSortDefaultOrder", RunSearchNoSortDefaultOrder},

	// Grouped statistics — cross-backend parity matrix (spec §7).
	// Each scenario asserts an OBSERVABLE response: every backend
	// (memory / sqlite / postgres / out-of-tree plugins) must produce the
	// same buckets for the same fixture corpus modulo float tolerance.
	{"GroupedStats_CountByState", RunParityGroupedStats_CountByState},
	{"GroupedStats_CountByDataField", RunParityGroupedStats_CountByDataField},
	{"GroupedStats_MultiDimGroupBy", RunParityGroupedStats_MultiDimGroupBy},
	{"GroupedStats_WithCondition", RunParityGroupedStats_WithCondition},
	{"GroupedStats_BoolCondition", RunParityGroupedStats_BoolCondition},
	{"GroupedStats_PointInTime", RunParityGroupedStats_PointInTime},
	{"GroupedStats_AggregationsTier1", RunParityGroupedStats_AggregationsTier1},
	{"GroupedStats_StdevLowVarianceHighMean", RunParityGroupedStats_StdevLowVarianceHighMean},
	{"GroupedStats_NonNumericSkipped", RunParityGroupedStats_NonNumericSkipped},
	{"GroupedStats_NonScalarCoercesToNull", RunParityGroupedStats_NonScalarCoercesToNull},
	{"GroupedStats_CardinalityExceeded", RunParityGroupedStats_CardinalityExceeded},

	// Unknown-model contract — unified 404 MODEL_NOT_FOUND (stats/audit/search slice).
	// One representative op (GET stats query) suffices: every backend must reject
	// an unknown model before doing any query work. (The GET stats endpoint runs
	// the model-existence check first; grouped-stats validates groupBy beforehand.)
	{"UnknownModel404", RunUnknownModel404},

	// Criterion rejection reason — inline (non-FUNCTION) criterion default
	// audit reason is backend-agnostic (durable via the AUTOMATED cascade path).
	{"CriterionReasonInlineDefault", RunCriterionReasonInlineDefault},

	// Follow-on-action attribution — the {attributed principal, executor} pair
	// recorded on change history must be IDENTICAL on every backend. Guards the
	// 3-way delete-tombstone divergence staying fixed, plus the executor
	// round-trip, scheduled-fire (system executor), and joined-cascade
	// (origin-propagated, distinct executor) contracts.
	{"AttributionTombstoneUniformity", RunAttributionTombstoneUniformity},
	{"AttributionExecutorRoundTrip", RunAttributionExecutorRoundTrip},
	{"AttributionScheduledArmedByFire", RunAttributionScheduledArmedByFire},
	{"AttributionCascadeJoinedWrite", RunAttributionCascadeJoinedWrite},

	// Spec §10 backend-agnostic scenarios that lacked a dedicated named
	// parity scenario (search_type_directed.go). Data-field temporal
	// resolution is registered separately below.
	//
	// SearchPolymorphicIntStringExpansion guards the polymorphic [INTEGER,
	// STRING] pushdown soundness fix: an operand matching stored values of
	// different SQLite storage classes (int-30 and string-"30") must return
	// both on every backend. sqlite achieves this by routing polymorphic
	// comparison leaves to the residual (kernel-evaluated) rather than pushing
	// a single-storage-class-bound WHERE that under-selects — see
	// plugins/sqlite/query_planner.go isLeafPushable.
	{"SearchPolymorphicIntStringExpansion", RunSearchPolymorphicIntStringExpansion},
	{"SearchNumericBucketRounding", RunSearchNumericBucketRounding},
	{"SearchLikeAnchoredEscapedGlob", RunSearchLikeAnchoredEscapedGlob},
	{"SearchMalformedPatternRejected", RunSearchMalformedPatternRejected},
	{"SearchStringOpsCaseSensitivityAndNonTextual", RunSearchStringOpsCaseSensitivityAndNonTextual},
	{"SearchNegativeOpOnAbsentField", RunSearchNegativeOpOnAbsentField},
	{"SearchIsNullAbsentVsPresentNull", RunSearchIsNullAbsentVsPresentNull},

	// Task 1 fix (cyoda-go-spi eval_leaf.go): an unsatisfiable comparison
	// answers by operator polarity rather than a blanket false, including the
	// polymorphic [INTEGER, String] carve-out. See negation.go.
	{"SearchUnsatisfiableComparisonPolarity", RunSearchUnsatisfiableComparisonPolarity},

	// NOT-node plan Task 13: cross-backend NOT scenarios. See negation.go.
	{"SearchNotOverSimpleCondition", RunSearchNotOverSimpleCondition},
	{"SearchNotOverAndGroup", RunSearchNotOverAndGroup},
	{"SearchNotOverOrGroup", RunSearchNotOverOrGroup},
	{"SearchNotUniversalQuantifierOverWildcard", RunSearchNotUniversalQuantifierOverWildcard},
	{"SearchNotVsNegativeTwinDiffer", RunSearchNotVsNegativeTwinDiffer},
	{"SearchNotOverAbsentField", RunSearchNotOverAbsentField},
	{"SearchNotIsNullDiffersFromNotNullOnWildcard", RunSearchNotIsNullDiffersFromNotNullOnWildcard},
	{"DeleteConditionalNotOverCondition", RunDeleteConditionalNotOverCondition},
	{"SearchBadPathInsideNot", RunSearchBadPathInsideNot},

	// Spec §4 — data-field temporal (subsumes the earlier standalone
	// temporal-search-on-data-fields work). Model discovery content-sniffs
	// ISO-8601 sample strings into a temporal subtype, so a data field
	// compares chronologically with cross-subtype resolution: a LocalDate
	// field's `>= 2024-09-09` is a chronological compare, and a Year field
	// resolves that same operand to `> 2024` (imprecise-floor op mutation) —
	// matching 2025, not 2024. See search_type_directed.go.
	{"SearchDataFieldTemporalResolution", RunSearchDataFieldTemporalResolution},

	// Async-search result ordering (task E7.2, design §9 row 19): per-backend
	// deterministic order respecting the requested sort key, with entity-ID
	// tie-break — set+pairwise-key assertions, no cross-engine sequence
	// compare (see async_ordering.go's doc comment).
	{"AsyncOrderingRespected", RunAsyncOrderingRespected},

	// GET /scheduled-tasks: paging, filters and tenant isolation of the
	// scheduled-task query.
	{"ScheduledTasksQueryPaging", RunScheduledTasksQueryPaging},
	{"ScheduledTasksQueryFilters", RunScheduledTasksQueryFilters},
	{"ScheduledTasksQueryTenantIsolation", RunScheduledTasksQueryTenantIsolation},

	// Signing key-pair lifecycle: issue, current, JWKS, invalidate,
	// reactivate, delete — the same lifecycle now round-trips through
	// each plugin's shared KV store rather than a per-node in-memory one.
	// Signing key pairs are server-global, not tenant-scoped, and there is
	// no audience to isolate a scenario's own key from the others: other
	// scenarios must not leave an issued key pair active inside its window
	// on the shared server, or rotate/invalidate the server's keys; use
	// their own cluster/stack for that.
	{"SigningKeyPairLifecycle", RunSigningKeyPairLifecycle},
	{"KeyPairNoAudience", RunKeyPairNoAudience},
	{"PlatformOperatorGate", RunPlatformOperatorGate},

	// M2M clients in each backend's own spi.KeyValueStore: create, token,
	// list, reset and delete, with another tenant's id answering 404 on
	// delete and reset and never listed; and the per-tenant cap — refused
	// at the cap with 400 M2M_CLIENT_CAP_REACHED, a delete freeing a slot.
	{"M2MClientLifecycle", RunM2MClientLifecycle},
	{"M2MClientCap", RunM2MClientCap},
}

// Register appends additional NamedTests to the canonical list at init time.
// Use this from sub-packages that cannot be imported by registry.go without
// creating an import cycle (e.g. e2e/parity/externalapi imports parity for
// BackendFixture). Call Register from an init() function in those packages,
// and add a blank import in each backend test file to trigger the side effect.
//
// Per-backend test wrappers (memory, sqlite, postgres, and any out-of-tree
// plugin like cyoda-go-cassandra) MUST blank-import every parity-extension
// package — otherwise the extension's init() never runs and the wrapper
// silently misses the entire scenario set. Current extension packages are
// `e2e/parity/externalapi`, `e2e/parity/scheduledtransition`, and
// `e2e/parity/scheduledfunction`. New parity-extension packages added in
// future tranches must be added to all backend wrappers in lockstep.
func Register(tests ...NamedTest) {
	allTests = append(allTests, tests...)
}

// AllTests returns the canonical list of parity scenarios in registration
// order. The returned slice is a defensive copy — callers may iterate or
// filter it freely without affecting subsequent calls.
//
// Note: all init() functions in imported packages run before TestMain, so
// tests registered via Register are visible by the time TestParity runs.
func AllTests() []NamedTest {
	out := make([]NamedTest, len(allTests))
	copy(out, allTests)
	return out
}
