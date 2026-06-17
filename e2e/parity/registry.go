package parity

import "testing"

// Total parity scenarios: 135
// (Phase 1 smoke + Phase 4a CRUD/persistence + Phase 4b workflow/compute +
// distributed-safety contracts + schema extensions + Phase 9.2 OIDC CRUD/authz
// + Phase 9.3 OIDC JWT validation + Phase 9.4 OIDC divergences
// + Phase 9.5 OIDC SSRF/D19/D20/D23/D25/D21/I9/state/E2E
// + Phase 9.6 Audit fixes + grouped stats).
// ExternalAPI scenarios registered via parity.Register() in e2e/parity/externalapi/
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

	// Phase 4a — entity CRUD (Task 4a.2)
	{"EntityCreateAndGet", RunEntityCreateAndGet},
	{"EntityDelete", RunEntityDelete},
	{"EntityListByModel", RunEntityListByModel},
	{"EntityUpdateCollectionHappyPath", RunEntityUpdateCollectionHappyPath},
	{"EntityUpdateCollectionRollback", RunEntityUpdateCollectionRollback},

	// Phase 4a — bi-temporal (Task 4a.3)
	{"TemporalPointInTimeRetrieval", RunTemporalPointInTimeRetrieval},
	{"TemporalGetAsAtPopulatesFullMeta", RunTemporalGetAsAtPopulatesFullMeta},

	// Phase 4a — audit (Task 4a.4)
	{"AuditEntityHistory", RunAuditEntityHistory},
	{"AuditWorkflowEvents", RunAuditWorkflowEvents},
	{"AuditPostTxIdMatchesWorkflowFinished", RunAuditPostTxIdMatchesWorkflowFinished},

	// Phase 4a — tenant isolation (Task 4a.5)
	{"TenantIsolationEntities", RunTenantIsolationEntities},
	{"TenantIsolationModels", RunTenantIsolationModels},
	// v0.6.3 — temporal-query tenant isolation (existence-oracle pinning;
	// companions to PR #161/#164/#165). Structurally guaranteed today;
	// pinned here so a future refactor cannot silently regress.
	{"TenantIsolationTransactionIDInvisible", RunTenantIsolationTransactionIDInvisible},
	{"TenantIsolationPointInTimeInvisible", RunTenantIsolationPointInTimeInvisible},
	{"TenantIsolationChangesAtPITInvisible", RunTenantIsolationChangesAtPITInvisible},

	// Phase 4a — messaging (Task 4a.6)
	{"MessageCreateAndGet", RunMessageCreateAndGet},
	{"MessageDelete", RunMessageDelete},
	{"MessageLargePayload", RunMessageLargePayload},

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
	{"SearchLifecycleCondition", RunSearchLifecycleCondition},
	{"SearchGroupCondition", RunSearchGroupCondition},
	{"SearchNoMatches", RunSearchNoMatches},
	{"SearchAfterUpdate", RunSearchAfterUpdate},

	// Phase 4b — workflow selection (Task 4b.7)
	{"WorkflowCriteriaSelectingWorkflow", RunWorkflowCriteriaSelectingWorkflow},

	// Phase 4b — distributed-safety contracts (Tasks 4b.9-10)
	{"ConcurrentConflictingUpdate", RunConcurrentConflictingUpdate},
	{"ConcurrentTransitionsDifferentEntities", RunConcurrentTransitionsDifferentEntities},

	// A.1 — numeric classifier parity (HTTP round-trip)
	{"NumericClassification18DigitDecimal", RunNumericClassification18DigitDecimal},
	{"NumericClassification20DigitDecimal", RunNumericClassification20DigitDecimal},
	{"NumericClassificationLargeInteger", RunNumericClassificationLargeInteger},
	{"NumericClassificationIntegerSchemaAcceptsInteger", RunNumericClassificationIntegerSchemaAcceptsInteger},
	{"NumericClassificationIntegerSchemaRejectsDecimal", RunNumericClassificationIntegerSchemaRejectsDecimal},

	// Schema extensions — sequential fold across requests
	{"SchemaExtensionsSequentialFoldAcrossRequests", RunSchemaExtensionsSequentialFoldAcrossRequests},
	{"SchemaExtensionCrossBackendByteIdentity", RunSchemaExtensionCrossBackendByteIdentity},
	{"SchemaExtensionAtomicRejection", RunSchemaExtensionAtomicRejection},
	{"SchemaExtensionConcurrentConvergence", RunSchemaExtensionConcurrentConvergence},
	{"SchemaExtensionSavepointOnLockFoldEquivalence", RunSchemaExtensionSavepointOnLockFoldEquivalence},
	{"SchemaExtensionLocalCacheInvalidationOnCommit", RunSchemaExtensionLocalCacheInvalidationOnCommit},
	{"SchemaExtensionByteIdentityProperty", RunSchemaExtensionByteIdentityProperty},

	// Phase 9.2 — OIDC CRUD + authz (#284)
	// Rows 1-6: CRUD happy-path.
	{"OidcRegister", RunOidcRegister},
	{"OidcListAll", RunOidcListAll},
	{"OidcListActiveOnly", RunOidcListActiveOnly},
	{"OidcUpdateIssuers", RunOidcUpdateIssuers},
	{"OidcInvalidate", RunOidcInvalidate},
	{"OidcDelete", RunOidcDelete},
	// Rows 7-10: CRUD negative (404 / duplicate).
	{"OidcUpdateNonExistent", RunOidcUpdateNonExistent},
	{"OidcInvalidateNonExistent", RunOidcInvalidateNonExistent},
	{"OidcReactivateNonExistent", RunOidcReactivateNonExistent},
	{"OidcDuplicateRegister", RunOidcDuplicateRegister},
	// Rows 11-16: Authz negative — non-admin token → 403 FORBIDDEN.
	{"OidcNonAdminRegister", RunOidcNonAdminRegister},
	{"OidcNonAdminUpdate", RunOidcNonAdminUpdate},
	{"OidcNonAdminInvalidate", RunOidcNonAdminInvalidate},
	{"OidcNonAdminReactivate", RunOidcNonAdminReactivate},
	{"OidcNonAdminDelete", RunOidcNonAdminDelete},
	{"OidcNonAdminReload", RunOidcNonAdminReload},

	// Phase 9.3 — OIDC validation + rotation + isolation (rows 17-27) (#284)
	// JWT validation integration (rows 17-20): register mock IdP, sign JWT,
	// assert accept/reject across lifecycle state changes.
	{"OidcJWTValidation_RegisterAndAccept", RunOidcJWTValidation_RegisterAndAccept},
	{"OidcJWTValidation_InvalidateRejects", RunOidcJWTValidation_InvalidateRejects},
	{"OidcJWTValidation_ReactivateRecovers", RunOidcJWTValidation_ReactivateRecovers},
	{"OidcJWTValidation_DeletePermanent", RunOidcJWTValidation_DeletePermanent},
	// Issuer-list update affects validation (row 21).
	{"OidcJWTValidation_IssuerListUpdate", RunOidcJWTValidation_IssuerListUpdate},
	// Key rotation/revocation (rows 22-26b).
	{"OidcKeyRotation_NewKidAccepted", RunOidcKeyRotation_NewKidAccepted},
	{"OidcKeyRotation_OldKidStillAccepted", RunOidcKeyRotation_OldKidStillAccepted},
	{"OidcKeyRevocation_RevokedKidRejected", RunOidcKeyRevocation_RevokedKidRejected},
	{"OidcKeyRotation_ColdStartReturnsErrUnknownKID", RunOidcKeyRotation_ColdStartReturnsErrUnknownKID},
	{"OidcReactivate_RemoteRemovalSync", RunOidcReactivate_RemoteRemovalSync},
	{"OidcReactivate_RemoteKeysPreservedSync", RunOidcReactivate_RemoteKeysPreservedSync},
	// Multi-provider isolation (row 27).
	{"OidcMultiProvider_Isolation", RunOidcMultiProvider_Isolation},

	// Phase 9.4 — OIDC divergences (rows 28-46) (#284)
	// D5 inactive-update (row 28).
	{"OidcInactiveUpdate_Returns409Conflict", RunOidcInactiveUpdate_Returns409Conflict},
	// Tenant isolation (rows 29-30).
	{"OidcCrossTenantManagementIsolation", RunOidcCrossTenantManagementIsolation},
	{"OidcTenantBindingViaOwnerLegalEntityID", RunOidcTenantBindingViaOwnerLegalEntityID},
	// D17 iat-binding accidental (row 31).
	{"OidcD17_IatBindingPreTransition", RunOidcD17_IatBindingPreTransition},
	// D17 mandatory iss-validation (rows 32-33).
	{"OidcD17_KidCollisionRoutesByIss", RunOidcD17_KidCollisionRoutesByIss},
	{"OidcD17_EmptyIssuersUsesDiscoveryDoc", RunOidcD17_EmptyIssuersUsesDiscoveryDoc},
	// D17 iat skew (rows 34-35).
	{"OidcD17_IatWithinSkewAccepted", RunOidcD17_IatWithinSkewAccepted},
	{"OidcD17_IatOutsideSkewRejected", RunOidcD17_IatOutsideSkewRejected},
	// D3 chain order (row 36).
	{"OidcD3_ChainOrderJWKSValidatorFirst", RunOidcD3_ChainOrderJWKSValidatorFirst},
	// D6 self-heal (rows 37, 37b).
	{"OidcD6_MaliciousTenantPublishesFirstPartyKid", RunOidcD6_MaliciousTenantPublishesFirstPartyKid},
	{"OidcD6_ColdPathTwoIssEligibleCandidates", RunOidcD6_ColdPathTwoIssEligibleCandidates},
	// D11 register race (rows 38a, 38b, 39).
	{"OidcD11_SequentialRegisterDeterministic", RunOidcD11_SequentialRegisterDeterministic},
	{"OidcD11_ConcurrentRegisterFaultInjected", RunOidcD11_ConcurrentRegisterFaultInjected},
	{"OidcD11_OrphanIndexCleanup", RunOidcD11_OrphanIndexCleanup},
	// D8 two-phase warmup (rows 40-42).
	{"OidcD8_ListenerBindsBeforeWarmup", RunOidcD8_ListenerBindsBeforeWarmup},
	{"OidcD8_Phase2FailureNonFatal", RunOidcD8_Phase2FailureNonFatal},
	{"OidcD8_Phase2PendingFallsThroughToErrUnknownKID", RunOidcD8_Phase2PendingFallsThroughToErrUnknownKID},
	// D18 broadcast (rows 43-46).
	{"OidcD18_HandlerPanicIsolation", RunOidcD18_HandlerPanicIsolation},
	{"OidcD18_SingleflightDebounce", RunOidcD18_SingleflightDebounce},
	{"OidcD18_ReloadInvalidateSerializeLocally", RunOidcD18_ReloadInvalidateSerializeLocally},
	{"OidcD18_ReloadAllSerializesWithReloadOne", RunOidcD18_ReloadAllSerializesWithReloadOne},

	// Phase 9.5 — OIDC SSRF/D19/D20/D23/D25/D21/I9/state/E2E (rows 47-68) (#284)
	// D10 SSRF (rows 47-49).
	{"OidcD10_SSRF_FetchTimeDNSRebind", RunOidcD10_SSRF_FetchTimeDNSRebind},
	{"OidcD10_SSRF_IPv6BlockedRanges", RunOidcD10_SSRF_IPv6BlockedRanges},
	{"OidcD10_SSRF_NoRedirectFollowing", RunOidcD10_SSRF_NoRedirectFollowing},
	// D10 malicious jwks_uri in discovery doc (unit-level coverage; skipped at parity level).
	{"OidcD10_MaliciousDiscoveryJWKSURI_Skip", RunOidcD10_MaliciousDiscoveryJWKSURI_Skip},
	// Critical-2 fix — non-UUID tenant rejection (unit-level coverage; skipped at parity level).
	{"OidcInvalidTenantUUIDRejected_Skip", RunOidcInvalidTenantUUIDRejected_Skip},
	// D19 reactivate (rows 50-51).
	{"OidcD19_ReactivateSuccessPath", RunOidcD19_ReactivateSuccessPath},
	{"OidcD19_ReactivateWithFailedUpstreamPreservesCache", RunOidcD19_ReactivateWithFailedUpstreamPreservesCache},
	// D20 audience (rows 52-53).
	{"OidcD20_AudienceMismatchRejected", RunOidcD20_AudienceMismatchRejected},
	{"OidcD20_EmptyExpectedAudiencesAcceptsAny", RunOidcD20_EmptyExpectedAudiencesAcceptsAny},
	// D23 UserContext (rows 54-56).
	{"OidcD23_CrossIdPSubCollisionDistinctUserIDs", RunOidcD23_CrossIdPSubCollisionDistinctUserIDs},
	{"OidcD23_PerProviderRolesClaim", RunOidcD23_PerProviderRolesClaim},
	{"OidcD23_RolesParsingMultiFormat", RunOidcD23_RolesParsingMultiFormat},
	// D23 sub bounds (rows 57-59).
	{"OidcD23_SubControlCharRejected", RunOidcD23_SubControlCharRejected},
	{"OidcD23_SubTooLong", RunOidcD23_SubTooLong},
	{"OidcD23_SubContainingColonAccepted", RunOidcD23_SubContainingColonAccepted},
	// D25 ownership transition (rows 60-62).
	{"OidcD25_CrossTenantRegisterEmitsAuditLog", RunOidcD25_CrossTenantRegisterEmitsAuditLog},
	{"OidcD25_RestartSurvivesInKV", RunOidcD25_RestartSurvivesInKV},
	{"OidcD25_ReceivingNodeDoesNotReEmitAudit", RunOidcD25_ReceivingNodeDoesNotReEmitAudit},
	// D21 list authz (row 63).
	{"OidcD21_NonAdminTenantMemberCanList", RunOidcD21_NonAdminTenantMemberCanList},
	// I9 broadcast (row 64).
	{"OidcI9_BroadcastForUnknownProviderHandledGracefully", RunOidcI9_BroadcastForUnknownProviderHandledGracefully},
	// State transitions (rows 65-66).
	{"OidcStateTransitions_ActiveInvalidatedDeleted", RunOidcStateTransitions_ActiveInvalidatedDeleted},
	{"OidcStateTransitions_InvalidatedReactivatedInvalidated", RunOidcStateTransitions_InvalidatedReactivatedInvalidated},
	// E2E coverage (rows 67-68).
	{"OidcE2E_TokenValidation", RunOidcE2E_TokenValidation},
	{"OidcE2E_MultiNodeEviction", RunOidcE2E_MultiNodeEviction},

	// Phase 9.6 — Audit fixes (#284)
	// Critical: non-deterministic cross-tenant routing fix (audience disambiguation).
	{"OidcCriticalAuditFix_AudienceDisambiguatesSharedIdP", RunOidcCriticalAuditFix_AudienceDisambiguatesSharedIdP},
	{"OidcCriticalAuditFix_AmbiguousProviderRejected_Skip", RunOidcCriticalAuditFix_AmbiguousProviderRejected_Skip},

	// Grouped statistics — cross-backend parity matrix (spec §7).
	// Each scenario asserts an OBSERVABLE response: every backend
	// (memory / sqlite / postgres / out-of-tree plugins) must produce the
	// same buckets for the same fixture corpus modulo float tolerance.
	{"GroupedStats_CountByState", RunParityGroupedStats_CountByState},
	{"GroupedStats_CountByDataField", RunParityGroupedStats_CountByDataField},
	{"GroupedStats_MultiDimGroupBy", RunParityGroupedStats_MultiDimGroupBy},
	{"GroupedStats_WithCondition", RunParityGroupedStats_WithCondition},
	{"GroupedStats_PointInTime", RunParityGroupedStats_PointInTime},
	{"GroupedStats_AggregationsTier1", RunParityGroupedStats_AggregationsTier1},
	{"GroupedStats_StdevLowVarianceHighMean", RunParityGroupedStats_StdevLowVarianceHighMean},
	{"GroupedStats_NonNumericSkipped", RunParityGroupedStats_NonNumericSkipped},
	{"GroupedStats_NonScalarCoercesToNull", RunParityGroupedStats_NonScalarCoercesToNull},
	{"GroupedStats_CardinalityExceeded", RunParityGroupedStats_CardinalityExceeded},
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
// silently misses the entire scenario set. Currently the only extension
// package is `e2e/parity/externalapi`. New parity-extension packages added
// in future tranches must be added to all backend wrappers in lockstep.
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
