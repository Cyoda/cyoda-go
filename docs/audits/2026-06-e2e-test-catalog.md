# E2E Test Coverage Catalog — cyoda-go v0.8.0

Generated: 2026-06-16
Method: eight parallel Explore agents read every E2E and parity test file in
scope and produced per-test summaries (aim / flow / ops / asserts) against the
actual code, not test names. This document is a snapshot of what the suite
*does* at the HTTP/gRPC boundary, intended for the v0.8.0 release-readiness
audit.

## Scope

Files catalogued:

- `internal/e2e/` — HTTP-stack E2E, runs against the in-process httptest
  server on a real PostgreSQL container (27 files)
- `e2e/parity/` — cross-backend scenarios that the registry runs against
  **memory, sqlite, postgres** (~27 files; commercial backend picks them up via
  its dependency update)
- `e2e/parity/externalapi/` — Cyoda Cloud external-API contract scenarios
  driven by a Cloud-shaped client (15 files)
- `e2e/parity/multinode/` — multi-node scenarios that require ≥2 cyoda-go
  binaries sharing the same PostgreSQL (4 files)

Out of scope: unit tests under `internal/`, plugin-internal tests under
`plugins/`, the standalone `cmd/compute-test-client` gRPC harness binary
(itself exercised *as a real subprocess* by every externalapi/parity test
that touches processors or criteria).

## Gaps and observations

Read these as "things worth deciding about before tagging v0.8.0", not as
release blockers.

1. **13 OpenAPI operations are intentionally uncovered.** Per
   `_openapi-conformance-report.md`, the uncovered set is *all* IAM/OIDC
   stub endpoints plus `accountSubscriptionsGet`, `fetchEntityTransitions`,
   `*TechnicalUser`. The conformance test (`zzz_openapi_conformance_test.go`)
   allow-lists these as `knownUncoveredOps`. None of them are claimed as
   v0.8.0 deliverables, but it is the explicit list of "we ship the spec
   surface, we don't test it."
2. **Search operator coverage is uneven.** At the HTTP boundary only
   STARTS_WITH, CONTAINS, EQUALS, NOT_EQUAL, GREATER_THAN /
   GREATER_OR_EQUAL are exercised. AND and OR group composition are both
   covered. No E2E asserts LESS_THAN/LESS_OR_EQUAL, BETWEEN, IN, NOT_IN,
   IS_NULL/IS_NOT_NULL, ENDS_WITH, REGEX. If any of those operators have
   moved in v0.8.0, there is no E2E net under them.
3. **Internalized processors are rejection-only.** The only test
   (`workflow_internalized_test.go`) asserts that an internalized processor
   on a manual transition returns 400 — there is no positive path because
   the feature is not implemented yet. This is per design; mentioned only
   so the catalog isn't read as "internalized works."
4. **Scheduled transitions are shape-only.** The v0.8.0 scheduled-transition
   work (#259) is covered for import round-trip, structural validation
   (manual+schedule mutually exclusive, delayMs>0), and explicit-fire
   rejection (`TestE2E_ExplicitFireOfScheduledTransition_ReturnsTransitionNotFound`).
   There is no test of an actual scheduled firing because that is not yet
   wired (planned post-v0.8.0). Same shape applies to asyncResult /
   crossoverToAsyncMs — rejection-only.
5. **CBD coverage is strong but PostgreSQL-only.** All CBD semantics
   (TX_pre commit, mid-dispatch visibility, stale-ifMatch abort before
   dispatch, TX_post pinning to home node, Loopback entry-point) live in
   `internal/e2e/workflow_proc_test.go` — postgres-only. The parity suite
   does not yet exercise CBD; only `e2e/parity/multinode/cbd_tx_pinning.go`
   does, and only with the registry's home-node assumption.
6. **Stress ceiling is 25 concurrent creates.** Largest concurrency in the
   suite: `TestTransaction_StressTest` (25 parallel creates),
   `TestModelSchemaExtensions_ConcurrentUpdatesNoConflict` (8 parallel
   schema extends), `RunSchemaExtensionConcurrentConvergence` (10
   parallel). Multi-node parity caps at N×N reads where N is "cluster
   size". There is no sustained-load test, no soak test, no QPS-target
   test. If v0.8.0 needs to claim a performance posture, the suite cannot
   substantiate it.
7. **Polymorphism tests document divergence from Cloud, two are skipped.**
   `RunExternalAPI_14_01` (mixed object-or-string at same path) and
   `RunExternalAPI_14_04` (polymorphic timestamp array) are skipped against
   cyoda-go because we either strict-reject or coarse-classify where Cloud
   stores discriminated types. Worse-class deltas vs. Cloud; intentional
   per code comments, but a v0.8.0 release note may be worth it.
8. **Tenant-isolation coverage is strong on the read paths.** Entity-by-id,
   model-export, audit history, `?transactionId=` oracle, `?pointInTime=`
   oracle, and `/changes?pointInTime=` oracle are all parity-tested across
   backends. M2M-client cross-tenant DELETE/PUT and trusted-key
   cross-tenant POST also covered. No test covers OIDC provider isolation
   because the endpoints are stubs (see #1).
9. **Delete-at-point-in-time is skipped pending #124.** `RunExternalAPI_06_06`
   is `t.Skip`. The handler ignores `pointInTime` on the
   `DeleteEntitiesByModel` endpoint — an SPI bump is the prerequisite, not
   a v0.8.0 deliverable.
10. **External-API change-level governance and negative-validation suites
    exist but are large.** `change_level_governance.go` (200 lines) and
    `negative_validation.go` (270 lines) are catalogued under
    "workflow / model" sections; they're worth a separate read if you want
    to verify the lock/changeLevel state machine is fully nailed before
    tagging.

## Verdict for v0.8.0

For the milestone's actual deliverables — workflow-import strict-decoder
(#264), workflow-import structural validation (#255), silent-default
semantics (#256), scheduled-transition shape (#259), asyncResult shape
rejection (#261), `/clients` OpenAPI conformance (#282), `/oauth/keys/*`
OpenAPI conformance (#281), grouped statistics (#299), tx-state sentinel
errors (#200), processor context pass-through (#253), processor type split
(#250), import/export hygiene + sweep (#257, #258), retry-policy validation
(#262) — the catalog shows direct E2E coverage in every case, and most have
both an `internal/e2e/` test (postgres-on-httptest) and a parity entry
(memory/sqlite/postgres). The two "intentionally rejection-only" features
(scheduled transitions firing, internalized processors) are accurately
catalogued as such.

The gap that *might* warrant a v0.8.0 patch decision is item 2 above
(search-operator E2E coverage); the rest are either out-of-scope by design
or correctly-marked future work.

---

## Table of contents

1. [Workflow import / export / validation](#workflow-import--export--validation)
2. [Workflow runtime — processors and criteria (gRPC compute)](#workflow-runtime--processors-and-criteria-grpc-compute)
3. [Entity ingestion and collection operations](#entity-ingestion-and-collection-operations)
4. [Entity update/delete, messaging, polymorphism, numeric types](#entity-updatedelete-messaging-polymorphism-numeric-types)
5. [Model and schema configuration management](#model-and-schema-configuration-management)
6. [Authentication, IAM, tenant isolation, CORS](#authentication-iam-tenant-isolation-cors)
7. [Querying, temporal/point-in-time, statistics](#querying-temporalpoint-in-time-statistics)
8. [Audit, transactions, concurrency / stress, multi-node, smoke, OpenAPI conformance](#audit-transactions-concurrency--stress-multi-node-smoke-openapi-conformance)

---

## Workflow import / export / validation

### Happy-path round-trip

#### TestWorkflow_OverwriteWorkflow (internal/e2e/workflow_test.go:109)
**Aim:** Importing a workflow with REPLACE mode completely overwrites previously imported workflow(s).
**Flow:**
  1. POST /api/model/import/JSON/SAMPLE_DATA/{entityName}/{modelVersion} — import model
  2. PUT /api/model/{entityName}/{modelVersion}/lock — lock model
  3. POST /api/model/{entityName}/{modelVersion}/workflow/import — import workflow v1 (REPLACE)
  4. GET /api/model/{entityName}/{modelVersion}/workflow/export — verify v1 exported
  5. POST /api/model/{entityName}/{modelVersion}/workflow/import — import workflow v2 (REPLACE)
  6. GET /api/model/{entityName}/{modelVersion}/workflow/export — verify v2 present, v1 absent
**Ops:** model/import, model/lock, workflow/import, workflow/export
**Asserts:** 200 on import; export payload contains exactly 1 workflow with name "order-workflow-v2"; v2 has 4 states (REPLACE fully overwrites v1's 3 states)

#### RunWorkflowImportExport (e2e/parity/workflow.go:33)
**Aim:** Round-trip import and export of a workflow across all backends preserves core fields (name, initialState, active, state structure, transitions).
**Flow:**
  1. ImportModel(modelName, 1, sample); LockModel; ImportWorkflow; ExportWorkflow — decode and verify structure
**Ops:** model/import, model/lock, workflow/import, workflow/export
**Asserts:** Export returns 200; workflows array has 1 entry; workflow.name="round-trip-wf", initialState="NONE", active=true
**Notes:** (parity — runs against memory/sqlite/postgres)

#### RunExternalAPI_08_01_SimpleAutomatedTransition (e2e/parity/externalapi/workflow_import_export.go:77)
**Aim:** Round-trip of a simple automated (non-manual) transition preserves the transition name.
**Flow:** CreateModelFromSample → ImportWorkflow(minimal "simple") → ExportWorkflow.
**Ops:** model/create-from-sample, workflow/import, workflow/export
**Asserts:** Export body contains "PUBLISH"
**Notes:** (parity); schema adaptation: "to"→"next", "automated":true→"manual":false

#### RunExternalAPI_08_02_DefaultsAppliedAndReturned (e2e/parity/externalapi/workflow_import_export.go:101)
**Aim:** Workflow import with omitted manual/criterion fields applies defaults; export returns the workflow with intact transition name.
**Flow:** CreateModelFromSample → ImportWorkflow(omit "manual") → ExportWorkflow.
**Asserts:** Export contains "MOVE"; omitted "manual" defaults to false
**Notes:** (parity); only `active=true` is server-default

#### RunExternalAPI_08_03_AdvancedCriteriaAndProcessors (e2e/parity/externalapi/workflow_import_export.go:154)
**Aim:** Advanced features (group criterion with AND, nested simple criteria, processors) survive round-trip.
**Flow:** CreateModelFromSample → ImportWorkflow(group criterion + processor) → ExportWorkflow.
**Asserts:** Export contains "ADVANCE", "group", "noop"
**Notes:** (parity); schema adaptations: "clauses"→"conditions", "jsonpath"→"simple", processor "type" required

#### RunExternalAPI_08_07_ScheduledTransitionRoundtrip (e2e/parity/externalapi/workflow_import_export.go:367)
**Aim:** Scheduled transitions with pointer-state TimeoutMs (nil, zero, positive) round-trip with exact fidelity across all backends.
**Flow:** Import 3 transitions: WithPositiveTimeout (90000000), WithZeroTimeout (0), WithoutTimeout (nil); ExportWorkflow; parse and inspect.
**Asserts:** Positive → "timeoutMs":90000000; zero → "timeoutMs":0 (non-nil zero preserved); nil → omitted entirely
**Notes:** (parity); TimeoutMs pointer semantics critical for idempotency

### Import mode semantics (REPLACE / ACTIVATE / MERGE)

#### RunExternalAPI_08_04_StrategyReplace (e2e/parity/externalapi/workflow_import_export.go:215)
**Aim:** importMode=REPLACE removes all workflows before adding new ones.
**Asserts:** Export contains "second", does NOT contain "first"
**Notes:** (parity)

#### RunExternalAPI_08_05_StrategyActivate (e2e/parity/externalapi/workflow_import_export.go:253)
**Aim:** importMode=ACTIVATE keeps existing workflows but deactivates those not in import list; activates incoming.
**Asserts:** Export contains both "first" (deactivated) and "second" (active)
**Notes:** (parity)

#### RunExternalAPI_08_06_StrategyMerge (e2e/parity/externalapi/workflow_import_export.go:291)
**Aim:** importMode=MERGE updates workflows by name and appends new ones; untouched workflows are left as-is.
**Asserts:** Export contains both "baseline" (updated) and "newone" (appended)
**Notes:** (parity)

### Strict-decoder rejection

#### TestWorkflow_Import_UnknownField_Returns400 (internal/e2e/workflow_test.go:209)
**Aim:** Typo'd nested field in workflow import is rejected by strict decoder at HTTP 400 BAD_REQUEST with field name in error detail.
**Flow:** Import model+lock → import with "transitionn" (typo) in StateDefinition.
**Asserts:** 400 BAD_REQUEST; RFC 9457 body contains "BAD_REQUEST" and "transitionn"
**Notes:** Issue #264

### Unknown model / missing workflow validation

#### TestWorkflow_ImportUnknownModel (internal/e2e/workflow_test.go:178)
**Aim:** Importing a workflow targeting a non-existent model returns 404 MODEL_NOT_FOUND.
**Asserts:** 404 NOT_FOUND; detail contains "MODEL_NOT_FOUND"
**Notes:** Resolves issue #131

#### TestWorkflow_ExportEmpty (internal/e2e/workflow_test.go:248)
**Aim:** Exporting workflow for a model with no imported workflows returns 404 WORKFLOW_NOT_FOUND.
**Asserts:** 404 NOT_FOUND; detail contains "WORKFLOW_NOT_FOUND"

#### RunExternalAPI_12_10_ImportWorkflowOnUnknownModel (e2e/parity/externalapi/negative_validation.go:258)
**Aim:** Workflow import on an unregistered model returns 404 MODEL_NOT_FOUND.
**Asserts:** 404 NOT_FOUND; error code MODEL_NOT_FOUND
**Notes:** (parity); resolves #131

### Structural validation: manual + schedule, delayMs > 0

#### RunExternalAPI_08_08_ScheduledTransitionRejects (e2e/parity/externalapi/workflow_import_export.go:467)
**Aim:** Two validation rules: (1) manual and schedule are mutually exclusive; (2) schedule.delayMs must be > 0.
**Asserts:** Sub-case 1: 400 VALIDATION_FAILED + "manual and scheduled are mutually exclusive". Sub-case 2: 400 + "delayMs must be > 0"
**Notes:** (parity)

### Cycles and allowCycles bypass

#### RunExternalAPI_08_09_AllowCyclesBypass (e2e/parity/externalapi/workflow_import_export.go:534)
**Aim:** Unguarded cycles in automated transitions are rejected by default; envelope flag allowCycles=true opts in.
**Asserts:** Sub-case 1: 400 VALIDATION_FAILED. Sub-case 2: 200 success
**Notes:** (parity); scheduled transitions (delayMs rate-limiting) exempt from cycle runaway rules via allowCycles

### Async-result rejection

#### TestE2E_AsyncResultTrueRejectedAtImport (internal/e2e/async_result_import_reject_test.go:18)
**Aim:** Processor with asyncResult=true is rejected at import with 400 VALIDATION_FAILED.
**Asserts:** 400; body contains `processor "p"` and "asyncResult=true is not supported"
**Notes:** Issue #261

#### RunExternalAPI_08_10_AsyncResultTrueRejects (e2e/parity/externalapi/workflow_import_export.go:602)
**Aim:** Parity equivalent of the above.
**Asserts:** 400 VALIDATION_FAILED; detail contains "asyncResult=true is not supported"
**Notes:** (parity)

#### TestE2E_CrossoverToAsyncMsRejectedAtImport (internal/e2e/async_result_import_reject_test.go:73)
**Aim:** Processor config with orphan crossoverToAsyncMs (absent asyncResult=true) is rejected at import.
**Asserts:** 400; body contains `processor "p"` and "crossoverToAsyncMs is not supported"

#### RunExternalAPI_08_11_CrossoverOrphanRejects (e2e/parity/externalapi/workflow_import_export.go:640)
**Aim:** Parity equivalent.
**Asserts:** 400 VALIDATION_FAILED; detail contains "crossoverToAsyncMs is not supported"
**Notes:** (parity)

### Scheduled-transition execution semantics

#### TestE2E_ExplicitFireOfScheduledTransition_ReturnsTransitionNotFound (internal/e2e/scheduled_transition_test.go:23)
**Aim:** Explicit fire (PUT) of a scheduled, non-manual transition is rejected with 400 TRANSITION_NOT_FOUND; cascade silently skips it; entity remains in source state.
**Flow:** Import model+workflow with one scheduled (delayMs=1000, manual=false) transition "AutoClose" → POST entity → verify still "Open" → PUT explicit fire → verify still "Open".
**Asserts:** 400 BAD_REQUEST; properties.errorCode="TRANSITION_NOT_FOUND" and substring "scheduled transitions are not yet implemented"
**Notes:** Issue #259; mirrors disabled-transition precedent

### Processor failure handling

#### TestWorkflowFailure_ProcessorError (internal/e2e/workflow_failure_test.go:16)
**Aim:** Processor returning error causes entity creation to fail; entity is not persisted.
**Asserts:** Entity creation returns non-200; DB entity count is 0

#### TestWorkflowFailure_ProcessorWarnings (internal/e2e/workflow_failure_test.go:58)
**Aim:** Processor that succeeds but emits warnings via context; warnings are propagated; entity is created.
**Asserts:** 200 success; DB entity count is 1

#### TestWorkflowFailure_CriteriaError (internal/e2e/workflow_failure_test.go:118)
**Aim:** Criteria function returning error during cascade causes entity creation to fail; entity not persisted.
**Asserts:** Non-200; DB entity count is 0

#### TestWorkflowFailure_ProcessorContextCancelled (internal/e2e/workflow_failure_test.go:160)
**Aim:** Processor returning context.DeadlineExceeded causes entity creation to fail.
**Asserts:** Non-200

#### TestWorkflowFailure_ProcessorPanic (internal/e2e/workflow_failure_test.go:196)
**Aim:** Processor panic is recovered; server remains stable and healthy.
**Asserts:** Entity creation returns non-200; subsequent GET /api/health reachable (not connection error)

#### TestWorkflowFailure_AsyncNewTxProcessorFailure (internal/e2e/workflow_failure_test.go:243)
**Aim:** ASYNC_NEW_TX processor failure is non-fatal; pipeline continues and entity creation succeeds.
**Asserts:** 200 success

#### TestWorkflowFailure_AsyncNewTxSuccessThenCascadeFails (internal/e2e/workflow_failure_test.go:279)
**Aim:** ASYNC_NEW_TX succeeds (data modified), then sync cascade processor fails; overall entity creation fails.
**Asserts:** Non-200 (sync failure kills cascade despite async success)

---

## Workflow runtime — processors and criteria (gRPC compute)

### Test infrastructure setup

Internal E2E tests use an in-process `localproc.LocalProcessingService` to register mock processors and criteria via `procSvc.RegisterProcessor()` and `procSvc.RegisterCriteria()` callbacks — runs synchronously inside the test process, allowing deterministic control via channels and atomic counters. Parity tests use the **external `compute-test-client` subprocess** (a real gRPC server) running the `catalog.go` processor suite (`noop`, `tag-with-foo`, `bump-amount`, `inject-error`, `echo-context-to-field`, …) to validate cross-backend behaviour.

### Externalized processor execution

#### RunExternalAPI_09_01_SyncProcessorSuccess (e2e/parity/externalapi/workflow_externalization.go:107)
**Aim:** SYNC externalized processor `noop` executes successfully; entity transitions CREATED→PROCESSED.
**Compute:** `noop` performs identity transform.
**Asserts:** status=200, meta.state=PROCESSED
**Notes:** (parity)

#### RunExternalAPI_09_02_SyncProcessorExceptionRollsBack (e2e/parity/externalapi/workflow_externalization.go:142)
**Aim:** SYNC processor `inject-error` throws; cascade aborts; entity creation fails with 400 WORKFLOW_FAILED.
**Asserts:** status=400, errorCode=WORKFLOW_FAILED; entity not persisted
**Notes:** equiv_or_better — cyoda-go returns 4xx with stable error code rather than implicit 5xx

#### RunExternalAPI_09_03_AsyncSameTxExceptionRollsBack (e2e/parity/externalapi/workflow_externalization.go:167)
**Aim:** ASYNC_SAME_TX processor `inject-error` behaves identically to SYNC; cascade aborts with 400.
**Asserts:** status=400, errorCode=WORKFLOW_FAILED
**Notes:** ASYNC_SAME_TX semantically equivalent to SYNC in cyoda-go implementation

#### RunExternalAPI_09_04_AsyncNewTxExceptionKeepsInitialSave (e2e/parity/externalapi/workflow_externalization.go:202)
**Aim:** ASYNC_NEW_TX processor `inject-error` fails non-fatally; cascade continues and entity reaches target state.
**Asserts:** status=200, meta.state=PROCESSED; entity durable
**Notes:** ASYNC_NEW_TX failures do not abort the cascade

### Internalized processor execution

#### TestE2E_InternalizedRejection_ManualTransitionReturns400 (internal/e2e/workflow_internalized_test.go:16)
**Aim:** Internalized processor on manual transition is rejected with 400 WORKFLOW_FAILED; entity state unchanged.
**Asserts:** status=400, errorCode=WORKFLOW_FAILED, meta.state=NONE
**Notes:** Internalized type is a placeholder; not yet implemented (positive path does not exist)

### Criteria evaluation

#### RunWorkflowCriteriaMatch (e2e/parity/workflow_proc.go:75)
**Aim:** Auto-transition gated by externalized criterion returns true; transition fires.
**Compute:** `always-true` returns hardcoded bool(true).
**Asserts:** meta.state=APPROVED
**Notes:** (parity)

#### RunWorkflowCriteriaNoMatch (e2e/parity/workflow_proc.go:115)
**Aim:** Auto-transition gated by externalized criterion returns false; transition does not fire.
**Asserts:** meta.state=CREATED
**Notes:** (parity)

#### TestWorkflowProc_Loopback (internal/e2e/workflow_proc_test.go:67)
**Aim:** Loopback (data-only PUT) re-evaluates automated transitions; criteria can trigger CBD cascade.
**Flow:** POST entity amount=50 (lands CREATED, criterion fails) → PUT loopback amount=200 → loopback engine re-evaluates → criterion now true → reaches PREMIUM.
**Asserts:** meta.state=PREMIUM after loopback
**Notes:** Loopback is the third engine entry-point

#### RunExternalAPI_09_12_ExternalizedCriterionSkipsCall (e2e/parity/externalapi/workflow_externalization.go:326)
**Aim:** Auto-transition guarded by externalized `always-false` does not fire.
**Asserts:** meta.state=CREATED
**Notes:** (parity); dictionary says call should be suppressed; cyoda-go actually dispatches but criterion returns false — observable outcome identical

#### RunExternalAPI_09_14_CriterionContextPassesThrough (e2e/parity/externalapi/workflow_externalization.go:442)
**Aim:** FunctionCondition.config.context string is forwarded as gRPC parameters; criterion observes it.
**Asserts:** meta.state=PROCESSED (criterion matched "match")
**Notes:** (parity)

#### RunExternalAPI_09_15_CriterionContextNegative (e2e/parity/externalapi/workflow_externalization.go:471)
**Aim:** Criterion context mismatch returns false; transition does not fire.
**Asserts:** meta.state=CREATED
**Notes:** (parity); confirms context value is actually forwarded and evaluated

### Processor data mutation and chaining

#### TestWorkflowProc_ProcessorModifiesData (internal/e2e/workflow_proc_test.go:180)
**Aim:** Processor mutates entity data; mutation is durable.
**Compute:** `compute-total` reads `amount`, writes `total=amount*1.1`.
**Asserts:** entity.data.total≈110

#### TestWorkflowProc_MultipleProcessorsSameTransition (internal/e2e/workflow_proc_test.go:219)
**Aim:** Multiple processors on one transition execute in sequence; each sees prior processor's output.
**Asserts:** entity.data.step1==true, entity.data.step2==true

#### RunWorkflowMultiStateCascade (e2e/parity/workflow_proc.go:156)
**Aim:** Multi-state auto-cascade NONE→CREATED→ENRICHED→APPROVED with processors on each; all mutations durable.
**Asserts:** meta.state=APPROVED, data.tag="foo", data.amount=11
**Notes:** (parity)

#### RunWorkflowManualTransition (e2e/parity/workflow_proc.go:211)
**Aim:** Manual transition does not auto-fire; PUT with transition name applies processor and advances state.
**Asserts:** state=CREATED after POST, state=APPROVED after PUT, data.tag="foo"
**Notes:** (parity)

#### RunWorkflowProcessorChainOnCreation (e2e/parity/workflow_proc.go:29)
**Aim:** Processor executes during auto-transition on entity creation; mutation is durable.
**Asserts:** meta.state=CREATED, data.tag="foo"
**Notes:** (parity)

### Processor context passthrough

#### RunExternalAPI_09_13_ProcessorContextPassesThrough (e2e/parity/externalapi/workflow_externalization.go:387)
**Aim:** ProcessorConfig.context is forwarded as gRPC `parameters`; processor sees exact string.
**Asserts:** data._context=="premium-role"
**Notes:** (parity); Issue #253

### COMMIT_BEFORE_DISPATCH semantics

#### TestWorkflowProc_UpdateWithCBD_DurablyCommitsPostCascadeState (internal/e2e/workflow_proc_test.go:370)
**Aim:** Manual transition with CBD processor durably commits post-cascade state and processor mutations.
**Asserts:** state=APPROVED, data.enriched==true, data.enrichedAmount==999.0
**Notes:** Spec §16 Task 13 happy-path

#### TestWorkflowProc_UpdateWithCBD_StaleIfMatchAbortsBeforeDispatch (internal/e2e/workflow_proc_test.go:451)
**Aim:** Stale If-Match on CBD cascade aborts before any external dispatch fires.
**Asserts:** status=412, errorCode=ENTITY_MODIFIED, dispatchCount==0, state=PENDING
**Notes:** Issue #27, Spec §4.1 strictly-earlier-enforcement

#### TestWorkflowProc_CreateWithCBD_DurablyCommitsPostCascadeState (internal/e2e/workflow_proc_test.go:589)
**Aim:** CREATE cascade with CBD processor: both post-cascade state and processor mutation are durable; audit endpoint resolves with POST txID.
**Asserts:** state=APPROVED, data.enriched==true, enrichedAmount==777.0, audit endpoint 200
**Notes:** Case A, Spec §16; response txID is cascade-entry (TX_pre), not final (TX_post)

#### TestWorkflowProc_LoopbackWithCBD (internal/e2e/workflow_proc_test.go:932)
**Aim:** Loopback entry-point with CBD processor: criteria match triggers CBD cascade and commits TX_post.
**Asserts:** state=UPGRADED, enriched==true, upgradedBy=="loopback-cbd"
**Notes:** Case G, Spec §16

#### TestWorkflowProc_UpdateWithoutCBD_RegressionBound (internal/e2e/workflow_proc_test.go:810)
**Aim:** SYNC-only cascade (no CBD): single txID, no segmentation, all SM audit events share response txID.
**Asserts:** state=APPROVED, enriched==true, all SM audit events share putTxID
**Notes:** Regression bound for Task 5/12/13/14 refactor

#### TestWorkflowProc_SearchSeesPreCalloutStateDuringDispatch (internal/e2e/workflow_proc_test.go:716)
**Aim:** During CBD mid-dispatch (TX_pre committed, processor executing, TX_post not yet committed), independent reader sees pre-callout state.
**Asserts:** mid-dispatch reader sees state=PENDING, enriched==null; post-dispatch reader sees state=APPROVED, enriched==true
**Notes:** Case D, Spec §4.2 visibility

### Cascade depth limit and default fallback

#### TestWorkflowProc_CascadeDepthLimit (internal/e2e/workflow_proc_test.go:135)
**Aim:** Workflow with runtime-looping transitions (criteria always-true on both legs of A↔B cycle) aborts on cascade depth exceeded.
**Asserts:** status!=200; entity not persisted

#### TestWorkflowProc_DefaultWorkflowFallback (internal/e2e/workflow_proc_test.go:118)
**Aim:** Entity creation with no workflow imported uses default workflow (NONE→CREATED→DELETED).
**Asserts:** status=200, state=CREATED

### Audit event assertions

#### TestWorkflowProc_FullAuditTrail (internal/e2e/workflow_proc_test.go:274)
**Aim:** Full state-machine cascade generates complete audit trail: START, WORKFLOW_FOUND, TRANSITION_MAKE events, FINISH.
**Asserts:** final state=DONE, STATE_MACHINE_START>=1, STATE_MACHINE_FINISH>=1, TRANSITION_MAKE>=2

#### TestWorkflowProc_PostTxIdMatchesAuditEndpoint (internal/e2e/workflow_proc_test.go:530)
**Aim:** POST /api/entity response transactionId matches audit event key.
**Asserts:** audit endpoint 200, event state=="DONE"
**Notes:** Issue #20

#### RunExternalAPI_05_TransitionAbortedAuditEventPaired (e2e/parity/externalapi/transition_aborted_audit.go:50)
**Aim:** Stale If-Match on single PUT generates paired TRANSITION_ABORTED event with reason=ENTITY_MODIFIED, expectedTxId, actualTxId.
**Asserts:** status=412 on stale PUT, no payload leak, audit delta matches backend type (TX-bound postgres rolls back entry+abort; non-TX-bound preserves paired START+ABORTED)
**Notes:** Issue #228

---

## Entity ingestion and collection operations

### Single-entity create + read

#### TestGetOneEntity_ReturnsEnvelope (internal/e2e/entity_conformance_test.go:100)
**Aim:** GET /entity/{id} returns envelope object with type, data, meta fields per OpenAPI spec.
**Asserts:** 200 OK, response has top-level type/data/meta (envelope shape)
**Notes:** Issue #21

#### TestGetAllEntities_ReturnsJSONArray (internal/e2e/entity_conformance_test.go:136)
**Aim:** GET /entity/{name}/{version} returns JSON array of envelope objects (not ndjson).
**Asserts:** 200 OK, response is JSON array, each element has type/data/meta
**Notes:** Issue #21

#### RunEntityCreateAndGet (e2e/parity/entity.go:49)
**Aim:** Single-entity create→get round-trip with state-machine and version history.
**Asserts:** 200; data round-trips exactly; Meta.ID/State/CreationDate/LastUpdateTime populated; changes array ≥1 entry
**Notes:** (parity)

#### RunExternalAPI_03_01_CreateEntitySuccess (e2e/parity/externalapi/entity_ingestion_single.go:37)
**Aim:** Create simple entity and verify data round-trip with non-zero UUID.
**Asserts:** 200; returned ID non-zero; data.key1 == 42
**Notes:** (parity)

### Single-entity delete

#### TestDeleteEntities_ResponseShape (internal/e2e/entity_conformance_test.go:24)
**Aim:** DELETE /entity/{name}/{version} returns single StreamDeleteResult object (not array per OpenAPI spec).
**Asserts:** 200; response is single object with entityModelClassId and deleteResult.{numberOfEntitites, numberOfEntititesRemoved, idToError}

#### RunEntityDelete (e2e/parity/entity.go:110)
**Aim:** Delete operation makes entity unretrievable via subsequent GET.
**Asserts:** DELETE 200; subsequent GET returns error (404)
**Notes:** (parity)

### Single-entity update + conditional

#### TestEntityLifecycle_Update (internal/e2e/entity_lifecycle_test.go:14)
**Aim:** PUT /api/entity/JSON/{entityID} (loopback) updates entity data without transition.
**Asserts:** 200 OK; re-fetch shows updated data

#### TestUpdateSingle_EntityIdsIsArrayOfStrings (internal/e2e/entity_conformance_test.go:177)
**Aim:** EntityTransactionResponse.entityIds is array of UUID strings, not objects.
**Asserts:** 200; entityIds is []string

### Single-entity transitions + lifecycle

#### TestEntityLifecycle_UpdateWithTransition (internal/e2e/entity_lifecycle_test.go:53)
**Aim:** Manual transition advances workflow state.
**Asserts:** state advances in subsequent GET

#### TestEntityLifecycle_AvailableTransitions (internal/e2e/entity_lifecycle_test.go:90)
**Aim:** GET /api/entity/{entityID}/transitions lists manual transitions available from current state.
**Asserts:** 200 OK; contains at least {approve, reject}

#### TestEntityLifecycle_InvalidTransition (internal/e2e/entity_lifecycle_test.go:154)
**Aim:** Non-existent transition name fails.
**Asserts:** Non-200; no state change

#### TestEntityLifecycle_DisabledTransition (internal/e2e/entity_lifecycle_test.go:184)
**Aim:** transition.disabled=true rejects at request time; entity stays in source state.
**Asserts:** Non-200; state remains CREATED

#### TestEntityLifecycle_TransitionNotFound (internal/e2e/entity_lifecycle_test.go:141)
**Aim:** Transition on non-existent entity returns 404 or 400.
**Asserts:** 404 or 400

### Entity history & temporal queries

#### TestEntityLifecycle_VersionHistory (internal/e2e/entity_lifecycle_test.go:221)
**Aim:** GET /api/entity/{entityID}/changes returns version history.
**Asserts:** changes array ≥2 entries (creation + update)

#### TestEntityLifecycle_TemporalAsAt (internal/e2e/entity_lifecycle_test.go:265)
**Aim:** GET /api/entity/{entityID}?asAt={timestamp} returns entity data as it existed at that point in time.
**Asserts:** Current shows v2, as-at query shows v1

#### TestEntityLifecycle_TemporalByTransactionID (internal/e2e/entity_lifecycle_test.go:313)
**Aim:** GET /api/entity/{entityID}?transactionId={txID} returns snapshot at specific transaction ID.
**Asserts:** at-txID GET 200 with v1 and meta.transactionId==createTxID; bogus txID GET 404 with ENTITY_NOT_FOUND
**Notes:** Issue #150

### List & query operations

#### RunEntityListByModel (e2e/parity/entity.go:139)
**Aim:** GET /api/entity/{name}/{version} returns all entities of a model after bulk create.
**Asserts:** 200; returned array has ≥2 elements
**Notes:** (parity)

### Bulk collection create

#### TestEntityLifecycle_BatchCreate (internal/e2e/entity_lifecycle_test.go:378)
**Aim:** POST /api/entity/JSON with array creates multiple entities atomically.
**Asserts:** 200; 1 result with transactionId and entityIds array of 3 UUIDs

#### TestCreateCollection_InitialStateDerivation (internal/e2e/collection_workflow_test.go:70)
**Aim:** Items inherit workflow.initialState, not hardcoded "CREATED".
**Asserts:** both entities have state==PENDING (matching workflow initialState)
**Notes:** Issue #227

#### TestCreateCollection_AuditEventsEmitted (internal/e2e/collection_workflow_test.go:113)
**Aim:** Per-item state-machine audit events emit during collection create.
**Asserts:** each entity has ≥1 STATE_MACHINE_START, FINISH, TRANSITION_MAKE
**Notes:** Issue #227

#### TestCreateCollection_AutomatedCascadeFires (internal/e2e/collection_workflow_test.go:169)
**Aim:** Automated transitions from initialState fire per item.
**Asserts:** both entities reach state==DONE (cascaded from NONE)
**Notes:** Issue #227

#### TestCreateCollection_FailureRollsBackBatch (internal/e2e/collection_workflow_test.go:217)
**Aim:** Collection create is atomic; failure of one item rolls back entire batch.
**Asserts:** non-200; entity count unchanged (zero new rows)
**Notes:** Issue #227

#### TestCreateCollection_TenantScoped (internal/e2e/collection_workflow_test.go:280)
**Aim:** Collection-created entities remain scoped to calling tenant.
**Asserts:** entities found via GET (tenant context preserved per item)

#### TestCreateCollection_NoWorkflowDefaultsToCreated (internal/e2e/collection_workflow_test.go:327)
**Aim:** Model without imported workflow falls back to default workflow (state="CREATED").
**Asserts:** both entities have state==CREATED

#### RunExternalAPI_04_01_FamilyAndPets (e2e/parity/externalapi/entity_ingestion_collection.go:26)
**Aim:** POST heterogeneous collection (2 different models) creates one entity per item.
**Asserts:** POST 200 with 2 entityIds; per-model lists show one entity each
**Notes:** (parity)

#### RunExternalAPI_04_04_TransactionWindow (e2e/parity/externalapi/entity_ingestion_collection.go:149)
**Aim:** ?transactionWindow=W chunks N items into ceil(N/W) response elements.
**Asserts:** 200; 3 chunks of sizes [3,3,1] for W=3, N=7; total 7 entityIds
**Notes:** (parity)

### Bulk collection update — per-item ifMatch

#### TestUpdateCollection_IfMatch_HappyPath (internal/e2e/collection_update_ifmatch_test.go:112)
**Aim:** All fresh ifMatch tokens; chunk commits cleanly.
**Asserts:** 200; entityIds=[both]; no `failed` field
**Notes:** Issue #228

#### TestUpdateCollection_IfMatch_PerItemStaleIsolated (internal/e2e/collection_update_ifmatch_test.go:169)
**Aim:** Stale ifMatch on one item surfaces in `failed` with ENTITY_MODIFIED; sibling commits.
**Asserts:** 1 success + 1 failure(code=ENTITY_MODIFIED, itemIndex=0); audit has STATE_MACHINE_START + TRANSITION_ABORTED(reason=ENTITY_MODIFIED, expectedTxId, actualTxId)
**Notes:** Issue #228 I1+S1

#### TestUpdateCollection_IfMatch_MixedPresence (internal/e2e/collection_update_ifmatch_test.go:284)
**Aim:** Some items carry ifMatch, others omit it.
**Asserts:** 200; 2 successes, 1 failure (stale ifMatch only); no-ifMatch item commits unconditionally
**Notes:** Issue #228 backward-compat

#### TestUpdateCollection_IfMatch_AllStale (internal/e2e/collection_update_ifmatch_test.go:338)
**Aim:** All items stale; chunk commits as zero-write transaction; `entityIds=[]`; `failed` has all items.
**Asserts:** 200; transactionId present; entityIds=[]; `failed` has 2 ENTITY_MODIFIED; audit paired START+ABORTED for both
**Notes:** Issue #228 I2

#### TestUpdateCollection_IfMatch_CBDStaleAbortsBeforeDispatch (internal/e2e/collection_update_ifmatch_test.go:418)
**Aim:** Stale ifMatch on CBD processor item aborts before dispatch fires.
**Asserts:** dispatchCount==0; state unchanged; audit shows TRANSITION_ABORTED before any dispatch
**Notes:** Issue #228 §4.1

#### TestUpdateCollection_IfMatch_NonConflictRollsBackChunk (internal/e2e/collection_update_ifmatch_test.go:503)
**Aim:** Non-conflict per-item failures still roll back entire chunk (per-item isolation reserved for ENTITY_MODIFIED only).
**Asserts:** 404; entity 1 unchanged on disk
**Notes:** Issue #228

#### TestUpdateCollection_IfMatch_MultiChunkPerItemFailures (internal/e2e/collection_update_ifmatch_test.go:533)
**Aim:** With ?transactionWindow=2, four items split into two chunks; per-item isolation scoped per chunk; itemIndex chunk-relative.
**Asserts:** 200; chunk 0: 1 success + 1 failure (no chunk-wide error); chunk 1: 2 successes
**Notes:** Issue #228

#### TestUpdateCollection_IfMatch_AbsentRegression (internal/e2e/collection_update_ifmatch_test.go:609)
**Aim:** All items omit ifMatch; pre-#228 behavior preserved.
**Asserts:** 200; no `failed` field; both updated

#### RunEntityUpdateCollectionHappyPath (e2e/parity/entity_update_collection.go:18)
**Aim:** Two-entity collection update lands cleanly on every backend.
**Asserts:** 200; both entities carry updated names
**Notes:** (parity)

#### RunEntityUpdateCollectionRollback (e2e/parity/entity_update_collection.go:78)
**Aim:** Atomicity: missing sibling causes entire update to roll back.
**Asserts:** UpdateCollection returns error; valid entity unchanged on disk
**Notes:** (parity)

#### RunExternalAPI_04_02_UpdateCollectionAge (e2e/parity/externalapi/entity_ingestion_collection.go:80)
**Aim:** UpdateEntitiesCollection on heterogeneous models; loopback (Transition:"") performs data-only update.
**Asserts:** 200; both entities reflect new values
**Notes:** (parity)

### Statistics & large-integer precision

#### TestEntityLifecycle_Statistics (internal/e2e/entity_lifecycle_test.go:420)
**Aim:** Entity stats endpoints return counts and per-state breakdown; ?states= filter narrows results.
**Asserts:** count≥3; state breakdown contains CREATED + APPROVED; filtered endpoint omits APPROVED

#### TestEntityLifecycle_PreservesLargeIntPrecision (internal/e2e/entity_lifecycle_test.go:501)
**Aim:** 9007199254740993 (2^53+1) round-trips without precision loss.
**Asserts:** response body literal "9007199254740993" (not float-rounded "9007199254740992")

### Bulk collection create — large batches

#### TestEntityLifecycle_CollectionCreate50 (internal/e2e/entity_lifecycle_test.go:562)
**Aim:** 50-item collection create succeeds atomically; all retrievable.
**Asserts:** 200; 1 chunk with 50 entityIds; all 50 GET requests succeed

### OpenAPI envelope conformance

#### TestCreate_Returns400ForInvalidModel (internal/e2e/entity_conformance_test.go:82)
**Aim:** POST to unknown model returns 400 or 404.
**Asserts:** Non-200 (400 BAD_REQUEST or 404 MODEL_NOT_FOUND)

---

## Entity update/delete, messaging, polymorphism, numeric types

### Message create/get/delete shape & envelope

#### TestMessage_GetMessage_ContentIsEmbeddedJSON (internal/e2e/message_test.go:47)
**Aim:** Message payload stored as embedded JSON object in "content" field, not JSON-encoded string.
**Asserts:** content is map[string]any (not string); content.sample == "value"
**Notes:** Issue #21

#### TestMessage_DeleteMessage_Shape (internal/e2e/message_test.go:82)
**Aim:** DELETE /api/message/{id} response shape: {entityIds: []}, no transactionId.
**Asserts:** 200; response.entityIds is []any with deleted ID; transactionId absent

#### TestMessage_DeleteMessages_Shape (internal/e2e/message_test.go:114)
**Aim:** Batch DELETE response is JSON array, not string.
**Asserts:** 200; response unmarshals as []any

#### TestMessage_NewMessage_Shape (internal/e2e/message_test.go:137)
**Aim:** POST /api/message/new returns array with entityIds, not bare string.
**Asserts:** 200; response is []map[string]any with entityIds

#### TestMessage_GetMessage_404_ContentType (internal/e2e/message_test.go:162)
**Aim:** 404 from GET /api/message/{id} uses Content-Type: application/problem+json (RFC 9457).
**Asserts:** 404; Content-Type starts with "application/problem+json"
**Notes:** Issue #21

#### TestMessage_DeleteBatch (internal/e2e/message_test.go:176)
**Aim:** Batch delete removes specified messages while leaving others intact.
**Asserts:** id3 remains 200; id1, id2 return 404

#### RunMessageCreateAndGet (e2e/parity/message.go:17)
**Aim:** Message create+retrieve round-trip header.subject and embedded content JSON.
**Asserts:** 200; header.subject == "parity.test"; content is map[string]any
**Notes:** (parity)

#### RunMessageDelete (e2e/parity/message.go:57)
**Aim:** Deleted message returns 404 on subsequent GET.
**Notes:** (parity)

#### RunMessageLargePayload (e2e/parity/message.go:82)
**Aim:** 200KB message payload survives full round-trip.
**Asserts:** 200; re-encoded content ≥200KB
**Notes:** (parity)

### Entity update: nested array, field mutation, transition semantics

#### RunExternalAPI_05_01_NestedArrayAppendAndModify (e2e/parity/externalapi/entity_update.go:24)
**Aim:** Modify first array element and append second element.
**Asserts:** arrayField length == 2; first element mutated
**Notes:** (parity)

#### RunExternalAPI_05_02_NestedArrayShrinkAndModify (e2e/parity/externalapi/entity_update.go:55)
**Aim:** Shrink array from two to one element and update top-level field.
**Asserts:** field1=="a-updated"; arrayField length == 1
**Notes:** (parity)

#### RunExternalAPI_05_03_RemoveObjectAndArrayKeepOneField (e2e/parity/externalapi/entity_update.go:89)
**Aim:** Drop nested object and array entirely; retain only field1.
**Asserts:** field1=="a-updated"; objectField/arrayField absent
**Notes:** (parity)

#### RunExternalAPI_05_04_PopulateMinimalIntoFull (e2e/parity/externalapi/entity_update.go:126)
**Aim:** Minimal entity then update to add nested object and array.
**Asserts:** objectField.field2=="b"; arrayField length == 2
**Notes:** (parity)

#### RunExternalAPI_05_05_LoopbackAbsentTransition (e2e/parity/externalapi/entity_update.go:163)
**Aim:** PUT /entity/JSON/{id} (no transition segment) performs loopback update.
**Asserts:** arrayField length == 2; payload applied without workflow transition
**Notes:** (parity)

#### RunExternalAPI_05_06_UnchangedPayloadStillTransitions (e2e/parity/externalapi/entity_update.go:196)
**Aim:** PUT identical payload still advances workflow transition; entity count remains 1.
**Asserts:** 200; entity list length == 1 (idempotent transition)
**Notes:** (parity)

### Entity update with conflict handling (IfMatch) — externalapi parity

#### RunExternalAPI_05_BulkUpdateIfMatchPerItemIsolation (e2e/parity/externalapi/entity_update_collection_ifmatch.go:51)
**Aim:** Bulk update with per-item ifMatch: stale on item 1 surfaces in failed[], siblings commit.
**Asserts:** entityIds=[id0, id2]; failed=[{entityId: id1, error.code: "ENTITY_MODIFIED", itemIndex: 1}]; id1 reflects intervening loopback (NOT bulk update)
**Notes:** (parity); Issue #228

#### RunExternalAPI_05_BulkUpdateChunkRollback (e2e/parity/externalapi/entity_update_collection_ifmatch.go:216)
**Aim:** Non-ENTITY_MODIFIED per-item failure rolls entire chunk back.
**Asserts:** 4xx; all 3 entities retain original names
**Notes:** (parity); Issue #228

### Entity delete semantics

#### RunExternalAPI_06_01_DeleteSingle (e2e/parity/externalapi/entity_delete.go:22)
**Aim:** Delete entity by ID; subsequent GET errors.
**Notes:** (parity)

#### RunExternalAPI_06_02_DeleteByModel (e2e/parity/externalapi/entity_delete.go:47)
**Aim:** Delete all entities for a model; ListEntitiesByModel returns empty.
**Asserts:** Delete 200; list length == 0
**Notes:** (parity)

#### RunExternalAPI_06_06_DeleteAtPointInTime (e2e/parity/externalapi/entity_delete.go:85)
**Aim:** DeleteEntitiesByModel with pointInTime selectively removes pre-T1 entities.
**Notes:** **t.Skip pending #124.** Handler ignores PointInTime; SPI bump required.

### Message headers & envelope (edge cases)

#### RunExternalAPI_11_01_SaveSingle (e2e/parity/externalapi/edge_message.go:40)
**Aim:** Message with headers round-trips through CreateMessageWithHeaders and GET.
**Asserts:** all header fields (userId, messageId, replyTo, recipient, correlationId, contentType) round-trip
**Notes:** (parity)

#### RunExternalAPI_11_02_DeleteSingle (e2e/parity/externalapi/edge_message.go:96)
**Aim:** Delete single message; subsequent GET 404.
**Notes:** (parity)

#### RunExternalAPI_11_03_DeleteCollection (e2e/parity/externalapi/edge_message.go:121)
**Aim:** Batch delete two messages via DELETE /api/message with JSON-array body.
**Asserts:** returned deleted IDs match requested IDs; both GET 404
**Notes:** (parity); Issue #134

### Polymorphic entity shapes

#### RunExternalAPI_14_01_MixedObjectOrStringAtSamePath (e2e/parity/externalapi/polymorphism.go:76)
**Aim:** Same JSONPath exhibits mixed object-or-string types.
**Notes:** **Worse-class divergence.** cyoda-go enforces strict type from first element (HTTP 400 "expected object, got string" on mixed POST). Cloud stores both. Pending controller decision.

#### RunExternalAPI_14_03_PolymorphicValueArray (e2e/parity/externalapi/polymorphism.go:139)
**Aim:** PolymorphicArray field accepts mixed (STRING, DOUBLE, BOOLEAN, UUID).
**Asserts:** round-trip JSON matches; SIMPLE_VIEW contains "STRING", "DOUBLE", "BOOLEAN"
**Notes:** (parity); cyoda-go classifies UUID as STRING (no distinct UUID DataType)

#### RunExternalAPI_14_04_PolymorphicTimestampArray (e2e/parity/externalapi/polymorphism.go:204)
**Aim:** ObjectArray accepts mixed temporal types.
**Notes:** **t.Skip — worse-class.** cyoda-go classifies all temporal variants as STRING. Cloud expects [LOCAL_DATE, YEAR_MONTH, ZONED_DATE_TIME].

#### RunExternalAPI_14_05_TrinoSearchOnPolymorphicScalarRESTHalf (e2e/parity/externalapi/polymorphism.go:213)
**Aim:** Polymorphic scalar (numeric-string vs UUID-string in same field) searchable via both branches.
**Asserts:** 200; search returns 2 results
**Notes:** (parity); RSocket leg unreachable in cyoda-go; REST-equivalent only

#### RunExternalAPI_14_06_RejectWrongTypeCondition (e2e/parity/externalapi/polymorphism.go:259)
**Aim:** Condition with wrong type for field rejected at search entry.
**Asserts:** 400; errorCode == "CONDITION_TYPE_MISMATCH"
**Notes:** (parity); equiv_or_better — cyoda-go now validates condition value types

### Numeric type classification & precision

#### RunExternalAPI_13_01_IntegerLandsInDoubleField (e2e/parity/externalapi/numeric_types.go:151)
**Aim:** JSON integer (13) accepted into DOUBLE field.
**Notes:** (parity)

#### RunExternalAPI_13_04_DefaultIntegerScopeINTEGER (e2e/parity/externalapi/numeric_types.go:175)
**Aim:** Default ParsingSpec classifies {"key1":"abc","key2":123} as STRING / INTEGER.
**Notes:** (parity)

#### RunExternalAPI_13_05ext_PolymorphicMergeWithDefaultScopes (e2e/parity/externalapi/numeric_types.go:211)
**Aim:** Two-sample merge produces polymorphic types [INTEGER, STRING], [INTEGER, BOOLEAN].
**Asserts:** SIMPLE_VIEW: $.key1=="[INTEGER, STRING]" (sorted), $.key2=="[INTEGER, BOOLEAN]"
**Notes:** (parity); TypeSet sorts by DataType iota

#### RunExternalAPI_13_06_DoubleAtMaxBoundary (e2e/parity/externalapi/numeric_types.go:244)
**Aim:** DOUBLE accepts Double.MAX_VALUE.
**Notes:** (parity)

#### RunExternalAPI_13_07_BigDecimal20Plus18 (e2e/parity/externalapi/numeric_types.go:278)
**Aim:** 38-significant-digit decimal lands as BIG_DECIMAL.
**Asserts:** big.Float Cmp == 0; SIMPLE_VIEW "BIG_DECIMAL"
**Notes:** (parity); UseNumber decoder essential

#### RunExternalAPI_13_08_UnboundDecimalGT18Frac (e2e/parity/externalapi/numeric_types.go:329)
**Aim:** 19-fractional-digit value lands as UNBOUND_DECIMAL.
**Notes:** (parity)

#### RunExternalAPI_13_09_BigInteger38Digit (e2e/parity/externalapi/numeric_types.go:379)
**Aim:** 38-digit integer lands as BIG_INTEGER.
**Notes:** (parity)

#### RunExternalAPI_13_10_UnboundInteger40Digit (e2e/parity/externalapi/numeric_types.go:425)
**Aim:** 40-digit integer lands as UNBOUND_INTEGER.
**Notes:** (parity)

#### RunExternalAPI_13_11_SearchIntegerAgainstDouble (e2e/parity/externalapi/numeric_types.go:475)
**Aim:** INTEGER condition value (70) on DOUBLE field returns all matching entities.
**Asserts:** Direct: 4 results; Async: 4 results
**Notes:** (parity)

---

## Model and schema configuration management

### Model lifecycle: import, lock, unlock, delete, export

#### TestModelExtension_Structural (internal/e2e/model_extension_test.go:37)
**Aim:** STRUCTURAL changeLevel allows new fields after lock.
**Asserts:** Entity created with extra_field; model schema extended in export

#### TestModelExtension_TypePromotion (internal/e2e/model_extension_test.go:69)
**Aim:** TYPE changeLevel allows numeric type widening (integer→float).

#### TestModelExtension_ArrayElements (internal/e2e/model_extension_test.go:89)
**Aim:** ARRAY_ELEMENTS changeLevel allows array element type expansion.

#### TestModelExtension_ArrayLength (internal/e2e/model_extension_test.go:111)
**Aim:** ARRAY_LENGTH changeLevel allows only array length variation.

#### TestModelExtension_StrictRejectsNewField (internal/e2e/model_extension_test.go:133)
**Aim:** Locked model without changeLevel rejects unknown fields.

#### TestModelExtension_ProgressiveExtension (internal/e2e/model_extension_test.go:152)
**Aim:** Multiple entities progressively extend schema under STRUCTURAL.
**Asserts:** Both field_a and field_b appear in final exported schema

#### RunModelImportAndExport (e2e/parity/model.go:14)
**Aim:** Import/export round-trip; model appears in list.
**Notes:** (parity)

#### RunModelLockAndUnlock (e2e/parity/model.go:62)
**Aim:** Lock/unlock state transitions visible via ListModels.
**Notes:** (parity)

#### RunModelListModels (e2e/parity/model.go:93)
**Aim:** ListModels includes all imported models.
**Notes:** (parity)

#### RunModelDelete (e2e/parity/model.go:126)
**Aim:** Model deletion removes it from accessible state.
**Notes:** (parity)

### Schema extension and concurrent state transitions

#### TestModelSchemaExtensions_ConcurrentUpdatesNoConflict (internal/e2e/model_schema_extensions_test.go:25)
**Aim:** 8 parallel entity creates under STRUCTURAL do not cause 409 CONFLICT.
**Asserts:** All 8 return 200; folded schema contains all field_0..field_7
**Notes:** Regression for hot-row serialization bug

#### TestModelSchemaExtensions_SequentialFoldAcrossRequests (internal/e2e/model_schema_extensions_test.go:105)
**Aim:** 6 sequential entity creates accumulate schema extensions.
**Asserts:** Each create 200; exported schema contains all seq_field_0..seq_field_5

### Numeric type classification

#### TestNumericClassification_HTTP_18DigitDecimal (internal/e2e/numeric_classification_test.go:27)
**Aim:** 18-fractional-digit decimal → BIG_DECIMAL.

#### TestNumericClassification_HTTP_20DigitDecimal (internal/e2e/numeric_classification_test.go:45)
**Aim:** 20-fractional-digit decimal → UNBOUND_DECIMAL.

#### TestNumericClassification_HTTP_LargeInteger (internal/e2e/numeric_classification_test.go:62)
**Aim:** 2^53+1 integer → LONG.

#### TestNumericClassification_HTTP_IntegerSchemaAcceptsInteger (internal/e2e/numeric_classification_test.go:82)
**Aim:** Strict validation accepts integer on integer schema.

#### TestNumericClassification_HTTP_IntegerSchemaRejectsDecimal (internal/e2e/numeric_classification_test.go:102)
**Aim:** Strict validation rejects decimal on integer schema.

#### RunNumericClassification18DigitDecimal / 20DigitDecimal / LargeInteger (e2e/parity/numeric_classification.go:14, :40, :65)
**Aim:** Parity equivalents of the three classification tests above.
**Notes:** (parity)

#### RunNumericClassificationIntegerSchemaAcceptsInteger / RejectsDecimal (e2e/parity/numeric_classification.go:91, :112)
**Aim:** Parity equivalents of the strict-validation tests above.
**Notes:** (parity)

### Schema byte identity, atomicity, cache invalidation

#### RunSchemaExtensionAtomicRejection (e2e/parity/schema_atomic_rejection.go:20)
**Aim:** Rejected schema extensions do not mutate state (B-I6).
**Asserts:** Pre-export bytes == post-export bytes
**Notes:** (parity)

#### RunSchemaExtensionCrossBackendByteIdentity (e2e/parity/schema_byte_identity.go:18)
**Aim:** 20-field widening produces byte-identical export across backends (B-I1).
**Asserts:** Export bytes == oracle bytes
**Notes:** (parity)

#### RunSchemaExtensionLocalCacheInvalidationOnCommit (e2e/parity/schema_cache_invalidation.go:17)
**Aim:** Local cache is invalidated after ExtendSchema commits (B-I8).
**Asserts:** schema1 != schema2 (cache invalidated)
**Notes:** (parity)

#### RunSchemaExtensionConcurrentConvergence (e2e/parity/schema_concurrent_convergence.go:24)
**Aim:** 10 concurrent extensions converge to same bytes as serial replay (B-I7).
**Asserts:** Export bytes == oracle bytes (canonical order)
**Notes:** (parity); permutation invariance

#### RunSchemaExtensionByteIdentityProperty (e2e/parity/schema_extension_property.go:43)
**Aim:** Property-based: 50 seeded extension sequences match oracle (B-I1).
**Notes:** (parity); skipped under -short; deterministic fuzzing

#### RunSchemaExtensionPropertyBudget (e2e/parity/schema_extension_property_budget.go:20)
**Aim:** Property test completes within per-backend CI wall-clock budget (≤40s).
**Notes:** (parity); skipped under -short

#### RunSchemaExtensionSavepointOnLockFoldEquivalence (e2e/parity/schema_save_on_lock.go:25)
**Aim:** Savepoint-on-lock does not change observable fold (B-I2/B-I3).
**Notes:** (parity)

### Schema symmetry: import↔export round-trip

#### RunDeepSchemaSymmetry (e2e/parity/schema_symmetry.go:16)
**Aim:** Deeply nested objects, arrays, edge-case numerics, unicode round-trip.
**Asserts:** Retrieved entity data == input data (DeepEqual after normalization)
**Notes:** (parity); exercises null handling, empty strings, 2KB strings, unicode combining chars, int/float boundary

### External API model lifecycle (parity tests)

#### RunExternalAPI_01_01..01_13 (e2e/parity/externalapi/model_lifecycle.go:46..313)
A coherent 13-scenario suite covering: register-from-sample (01_01), upsert-extends-schema (01_02), upsert-incompatible-type (01_03), idempotent re-register (01_04), lock (01_05), unlock (01_06), lock-twice-rejected with 409 + MODEL_ALREADY_LOCKED (01_07), delete (01_08), empty-list on fresh tenant (01_09), list-non-empty (01_10), export metadata views (01_11), nested-array sample (01_12 Nobel Laureates), nested-object sample (01_13 LEI).
**Notes:** (parity); shape contract pinning + state-machine constraint coverage

---

## Authentication, IAM, tenant isolation, CORS

### Account endpoints

#### TestAccount_Get (internal/e2e/account_test.go:8)
**Aim:** GET /api/account returns user account info with correct schema.
**Asserts:** 200; validates against OpenAPI UserAccountInfoResponseDto schema

### M2M clients (/clients admin)

#### TestE2E_Clients_ListEmpty (internal/e2e/clients_test.go:23)
**Aim:** GET /clients returns bootstrapped M2M client.
**Asserts:** 200; bootstrap "testclient" present

#### TestE2E_Clients_CreateListRoundtrip (internal/e2e/clients_test.go:49)
**Aim:** POST /clients creates client, returns credentials, appears in list.
**Asserts:** 200; clientId/clientSecret non-empty; grant_type=client_credentials; ROLE_M2M present

#### TestE2E_Clients_TokenExchangeRoundtrip (internal/e2e/clients_test.go:99)
**Aim:** Created M2M client can exchange credentials for JWT.
**Asserts:** 200; JWT sub claim matches clientId; scopes include ROLE_M2M

#### TestE2E_Clients_ResetSecretRotates (internal/e2e/clients_test.go:124)
**Aim:** PUT /clients/{id}/secret rotates secret; invalidates old credentials.
**Asserts:** new secret differs; old secret rejected; new secret authenticates
**Notes:** security-critical

#### TestE2E_Clients_DeleteInvalidatesToken (internal/e2e/clients_test.go:157)
**Aim:** DELETE /clients/{id} invalidates M2M client credentials.
**Asserts:** delete 200; subsequent token exchange non-200
**Notes:** security-critical

#### TestE2E_Clients_WithAdminRoleFlagOn (internal/e2e/clients_test.go:174)
**Aim:** ?withAdminRole=true adds ROLE_ADMIN to client scopes.
**Asserts:** JWT scopes include both ROLE_ADMIN and ROLE_M2M

#### TestE2E_Clients_CrossTenantIsolation_404 (internal/e2e/clients_test.go:245)
**Aim:** Tenant B admin cannot modify tenant A's M2M clients.
**Asserts:** 404 for both DELETE and PUT from tenant B; tenant A's client unaffected
**Notes:** security-critical

### OAuth keypair management (/oauth/keys/keypair*)

#### TestE2E_IssueJwtKeyPair_Happy (internal/e2e/oauth_keys_test.go:73)
**Aim:** Issue JWT keypair, return keyId + public key.
**Asserts:** 200; keyId non-empty; algorithm=RS256; publicKey non-empty

#### TestE2E_GetCurrentJwtKeyPair_Happy (internal/e2e/oauth_keys_test.go:96)
**Aim:** GET /oauth/keys/keypair/current retrieves active keypair by audience.

#### TestE2E_DeleteJwtKeyPair_Happy (internal/e2e/oauth_keys_test.go:120)
**Aim:** DELETE /oauth/keys/keypair/{keyId} removes keypair.

#### TestE2E_InvalidateJwtKeyPair_Happy (internal/e2e/oauth_keys_test.go:139)
**Aim:** POST /oauth/keys/keypair/{keyId}/invalidate marks keypair inactive.

#### TestE2E_ReactivateJwtKeyPair_Happy (internal/e2e/oauth_keys_test.go:158)
**Aim:** POST /oauth/keys/keypair/{keyId}/reactivate with {"validTo":"RFC3339"} restores invalidated keypair.
**Notes:** v0.8.0 breaking change — reactivate-with-body semantics required (#281)

#### TestE2E_GracePeriodRoundTrip (internal/e2e/oauth_keys_test.go:358)
**Aim:** Grace-period keys appear in JWKS during transition window and disappear after ValidTo.
**Flow:** issue A → issue B with invalidateCurrent=true, invalidateGracePeriodSec=2 → JWKS (both present) → sleep 3s → JWKS (only B).
**Asserts:** A's keyId present during grace window, absent after expiry
**Notes:** Spec §3.2 #1

#### TestE2E_KeypairBodySizeLimit (internal/e2e/oauth_keys_test.go:410)
**Aim:** Reject oversized request body (>1 MiB).
**Asserts:** 400 Bad Request

### Trusted-key registration (/oauth/keys/trusted*)

#### TestE2E_RegisterTrustedKey_Happy (internal/e2e/oauth_keys_test.go:186)
**Aim:** POST /oauth/keys/trusted registers third-party RSA JWK.
**Asserts:** 200; keyId matches; legalEntityId populated

#### TestE2E_ListTrustedKeys_Happy (internal/e2e/oauth_keys_test.go:211)
**Aim:** GET /oauth/keys/trusted returns registered trusted keys.

#### TestE2E_DeleteTrustedKey_Happy (internal/e2e/oauth_keys_test.go:247)
**Aim:** DELETE /oauth/keys/trusted/{keyId} removes trusted key.

#### TestE2E_InvalidateTrustedKey_Happy (internal/e2e/oauth_keys_test.go:268)
**Aim:** POST /oauth/keys/trusted/{keyId}/invalidate marks key inactive.

#### TestE2E_ReactivateTrustedKey_Happy (internal/e2e/oauth_keys_test.go:289)
**Aim:** POST /oauth/keys/trusted/{keyId}/reactivate restores invalidated trusted key.
**Notes:** v0.8.0 breaking change (validTo required)

#### TestE2E_TrustedKeyBodySizeLimit (internal/e2e/oauth_keys_test.go:435)
**Aim:** Reject oversized request body (>1 MiB).

#### TestE2E_CrossTenant_TrustedKey_409 (internal/e2e/oauth_keys_test.go:522)
**Aim:** Two tenants cannot register the same trusted keyId; second gets 409.
**Asserts:** tenant A 200; tenant B 409 + KEY_OWNED_BY_DIFFERENT_TENANT
**Notes:** security-critical

### CORS

#### TestCORS_E2E_PreflightAcrossGroups_LoopbackMode (internal/e2e/cors_e2e_test.go:59)
**Aim:** CORS preflight (OPTIONS) succeeds across all route groups in loopback mode.
**Asserts:** 204; ACAO=http://localhost:3000; Allow-Methods/Headers/Max-Age present; Vary: Origin
**Notes:** Covers entity, search, messaging, account, admin, help, health, discovery

#### TestCORS_E2E_LoopbackRejectsRemoteOrigin (internal/e2e/cors_e2e_test.go:97)
**Aim:** Loopback mode rejects remote origins without CORS headers.
**Asserts:** 200; ACAO empty; Vary: Origin
**Notes:** security-critical

#### TestCORS_E2E_AllowlistMode (internal/e2e/cors_e2e_test.go:120)
**Aim:** Allowlist permits only configured origins; does not auto-allow loopback.
**Asserts:** configured origin gets ACAO; loopback gets empty ACAO

#### TestCORS_E2E_WildcardMode (internal/e2e/cors_e2e_test.go:154)
**Aim:** Wildcard mode sets ACAO=* for all origins.
**Asserts:** 200; ACAO=*
**Notes:** security-critical

#### TestCORS_E2E_DisabledNoHeaders (internal/e2e/cors_e2e_test.go:174)
**Aim:** Disabled CORS middleware does not intercept preflight or emit CORS headers.

### Tenant isolation — entities, models, temporal oracles

#### RunTenantIsolationEntities (e2e/parity/tenant_isolation.go:25)
**Aim:** Entities created by tenant A are invisible/unmodifiable/audit-invisible to tenant B.
**Asserts:** tenant B GETs return 404; tenant A's entity unmodified after B's failed attempts
**Notes:** (parity); security-critical — three isolation vectors (GET, DELETE, audit)

#### RunTenantIsolationModels (e2e/parity/tenant_isolation.go:88)
**Aim:** Models created by A are invisible to B; B can create same name independently.
**Notes:** (parity); security-critical — model namespace is per-tenant

#### RunTenantIsolationTransactionIDInvisible (e2e/parity/tenant_isolation.go:159)
**Aim:** ?transactionId= query parameter cannot serve as existence oracle across tenants.
**Asserts:** both B requests return 404; error codes identical; response bodies byte-equal
**Notes:** (parity); security-critical — temporal query params must not leak existence

#### RunTenantIsolationPointInTimeInvisible (e2e/parity/tenant_isolation.go:226)
**Aim:** ?pointInTime= cannot serve as existence oracle across tenants.
**Asserts:** both B requests return 404; error codes identical; bodies byte-equal
**Notes:** (parity); security-critical

#### RunTenantIsolationChangesAtPITInvisible (e2e/parity/tenant_isolation.go:292)
**Aim:** ?pointInTime= on /entity/{id}/changes cannot serve as existence oracle.
**Notes:** (parity); security-critical

#### RunEmptyTenantOperations (e2e/parity/empty_tenant.go:14)
**Aim:** Read-side API returns sensible defaults on freshly minted tenant with zero data.
**Asserts:** listModels empty array (not error); getEntityStats 200; getAuditEvents 200/404 (not 500)
**Notes:** (parity); catches null-pointer/empty-collection divergences across backends

---

## Querying, temporal/point-in-time, statistics

### Search — string operators

#### TestSearch_StringOperators (internal/e2e/search_test.go:57)
**Aim:** STARTS_WITH and CONTAINS string operators return correct matches.
**Asserts:** STARTS_WITH matches 2 of 3; CONTAINS matches 1 of 3
**Notes:** Coverage gap: only STARTS_WITH and CONTAINS shown at HTTP boundary

### Search — numeric operators and composition

#### TestSearch_ORGroup (internal/e2e/search_test.go:82)
**Aim:** OR composition: name=="Alice" OR amount>100 matches 2 entities.

#### TestSearch_UnknownFieldPath_Returns400_InvalidFieldPath (internal/e2e/search_test.go:124)
**Aim:** Field-path validation rejects queries on absent JSONPath fields.
**Asserts:** 400; errorCode="INVALID_FIELD_PATH"; response names offending path
**Notes:** PR #162 / Issue #77

#### TestSearch_AsyncSubmit_UnknownFieldPath_Returns400_InvalidFieldPath (internal/e2e/search_test.go:157)
**Aim:** Async search path applies same field-path validator as sync.
**Asserts:** 400; errorCode="INVALID_FIELD_PATH"

### Search — async lifecycle

#### TestAsyncSearch_SubmitAndGetResults (internal/e2e/search_test.go:221)
**Aim:** End-to-end async search: submit, poll status, retrieve paginated results.
**Asserts:** Final status=SUCCESSFUL; results page contains 2 entities; totalElements present

#### TestAsyncSearch_GetStatus_NotFound (internal/e2e/search_test.go:267)
**Aim:** Status for non-existent job returns 404.

#### TestAsyncSearch_GetResults_NotFound (internal/e2e/search_test.go:279)
**Aim:** Results for non-existent job returns 404.

#### TestAsyncSearch_Cancel_AlreadyCompleted (internal/e2e/search_test.go:291)
**Aim:** Cancelling completed job returns 400 with current status.
**Asserts:** 400; body contains "SUCCESSFUL"

#### TestAsyncSearch_Cancel_NotFound (internal/e2e/search_test.go:318)
**Aim:** Cancelling non-existent job returns 404.

### Search — parity (memory/sqlite/postgres)

#### RunSearchSimpleCondition (e2e/parity/search.go:34)
**Aim:** Filter by equality on data field: status=="active" matches 2 of 3.
**Notes:** (parity); EQUALS

#### RunSearchLifecycleCondition (e2e/parity/search.go:67)
**Aim:** Filter by lifecycle state: state=="APPROVED" matches 1 of 2 after manual transition.
**Notes:** (parity)

#### RunSearchGroupCondition (e2e/parity/search.go:103)
**Aim:** AND composition: status=="active" AND amount>75 matches 1 of 3.
**Notes:** (parity); AND + EQUALS + GREATER_THAN

#### RunSearchNoMatches (e2e/parity/search.go:142)
**Aim:** Search for non-existent value returns empty result set (not error).
**Notes:** (parity)

#### RunSearchAfterUpdate (e2e/parity/search.go:169)
**Aim:** Search reflects newly-written data; critical for immediate visibility.
**Notes:** (parity); search-vs-write consistency

#### RunWorkflowCriteriaSelectingWorkflow (e2e/parity/search.go:213)
**Aim:** Search criterion selects between workflows; entity reaches second workflow's state.
**Notes:** (parity); workflow-selection logic, not strictly a search test

### Search consistency

#### RunSearchIndexImmediateConsistency (e2e/parity/search_consistency.go:14)
**Aim:** Search index returns newly-created entity immediately (no sleep/poll).
**Notes:** (parity); pins immediate read-after-write contract

### Grouped statistics — single and multi-dimensional grouping

#### TestGroupedStats_E2E_HappyPath (internal/e2e/grouped_stats_test.go:95)
**Aim:** Group entities by single data field; verify bucket count and per-bucket counts.
**Asserts:** 3 buckets; v1=2, v2=2, v3=1
**Notes:** Issue #299

#### TestGroupedStats_E2E_Aggregations (internal/e2e/grouped_stats_test.go:152)
**Aim:** sum/avg/stdev over numeric field grouped by variantId with lifecycle condition.
**Asserts:** count=4; sum=100; avg=25; stdev≈12.909944 (sample stdev, Welford cross-backend parity)

#### TestGroupedStats_E2E_ValidationError (internal/e2e/grouped_stats_test.go:219)
**Aim:** Empty groupBy returns 400 with errorCode=MISSING_GROUP_BY (RFC 9457).

### Grouped statistics — parity

#### RunParityGroupedStats_CountByState (e2e/parity/grouped_stats.go:194)
**Aim:** Group by lifecycle state; verify count transitions; buckets sorted by count desc.
**Notes:** (parity); D12 ordering

#### RunParityGroupedStats_CountByDataField (e2e/parity/grouped_stats.go:252)
**Aim:** Group by JSON path on string field.
**Notes:** (parity)

#### RunParityGroupedStats_MultiDimGroupBy (e2e/parity/grouped_stats.go:295)
**Aim:** Cartesian group-by over ($.variantId, state): 4 entities → 3 distinct groups.
**Notes:** (parity); multi-dimensional cartesian product

#### RunParityGroupedStats_WithCondition (e2e/parity/grouped_stats.go:363)
**Aim:** Predicate-filtered grouping: state != "SHIPPED" drops SHIPPED bucket.
**Notes:** (parity); NOT_EQUAL

#### RunParityGroupedStats_PointInTime (e2e/parity/grouped_stats.go:416)
**Aim:** Historical snapshot via pointInTime: post-update mutations invisible to PIT query.
**Asserts:** 1 bucket (CREATED); count=3 (pre-ship and pre-delete snapshot)
**Notes:** (parity); temporal query; deletion-marker versions excluded at T1

#### RunParityGroupedStats_AggregationsTier1 (e2e/parity/grouped_stats.go:475)
**Aim:** Tier-1 aggregations (sum/avg/min/max) are bit-identical across backends.
**Asserts:** sum=100 (exact); avg=25 (avgTol=1e-12); min=10; max=40
**Notes:** (parity)

#### RunParityGroupedStats_StdevLowVarianceHighMean (e2e/parity/grouped_stats.go:538)
**Aim:** stdev parity: Welford ↔ postgres STDDEV_SAMP within 1e-9 relative despite catastrophic-cancellation risk.
**Notes:** (parity); D9; sqlite falls through to streaming Welford

#### RunParityGroupedStats_NonNumericSkipped (e2e/parity/grouped_stats.go:596)
**Aim:** D4 rule: non-numeric/missing values silently dropped from aggregations but included in count.
**Asserts:** count=5 (all); sum/avg/min/max use finite subset only
**Notes:** (parity)

#### RunParityGroupedStats_NonScalarCoercesToNull (e2e/parity/grouped_stats.go:680)
**Aim:** D4: missing-path and runtime-object both coerce to null in group-key; 5 distinct rows merge into 1 null bucket.
**Notes:** (parity); D4 coercion rule across backends

#### RunParityGroupedStats_CardinalityExceeded (e2e/parity/grouped_stats.go:733)
**Aim:** Request limit exceeding CYODA_STATS_GROUP_MAX raises 400 INVALID_LIMIT at handler.
**Notes:** (parity); request-validation layer

### Point-in-time retrieval

#### RunTemporalPointInTimeRetrieval (e2e/parity/temporal.go:48)
**Aim:** Multi-version point-in-time reads: GetEntityAt returns correct snapshot at each state transition.
**Asserts:** Current=v3; PIT(afterCreate)=v1; PIT(afterUpdate1)=v2; PIT(beforeCreate)=404; version history ≥3
**Notes:** (parity); bi-temporal versioning

#### RunTemporalGetAsAtPopulatesFullMeta (e2e/parity/temporal.go:150)
**Aim:** PIT GET returns fully populated meta (state, creationDate, lastUpdateTime, transactionId, id), not zero values.
**Notes:** (parity); regression test for historical bug where wire-level stores returned zero values on PIT path

### Point-in-time retrieval — external API

#### RunExternalAPI_07_01_GetEntityAtPointInTime (e2e/parity/externalapi/point_in_time.go:27)
**Aim:** GET at three different timestamps returns three distinct versions.
**Notes:** (parity)

#### RunExternalAPI_07_02_GetEntityByTransactionID (e2e/parity/externalapi/point_in_time.go:83)
**Aim:** GET by transactionId returns entity snapshot as of that transaction.
**Notes:** (parity)

#### RunExternalAPI_07_03_ChangeHistoryFull (e2e/parity/externalapi/point_in_time.go:128)
**Aim:** Full change history lists all versions; CREATED is oldest entry.
**Notes:** (parity)

#### RunExternalAPI_07_04_ChangeHistoryAtPointInTime (e2e/parity/externalapi/point_in_time.go:167)
**Aim:** Change history at point-in-time truncates to entries ≤ timestamp.
**Notes:** (parity)

#### RunExternalAPI_07_05_ChangeHistoryNonExistent (e2e/parity/externalapi/point_in_time.go:217)
**Aim:** Change history for non-existent entity returns 404 ENTITY_NOT_FOUND.
**Notes:** (parity)

---

## Audit, transactions, concurrency / stress, multi-node, smoke, OpenAPI conformance

### Shared fixture / helpers

The E2E harness (internal/e2e) exposes: (1) HTTP client utilities — `doAuth()` which retries 409 Conflict if `properties.retryable=true`, `readBody()`, `getToken()` (client_credentials OAuth), `queryDB()` for direct postgres verification; (2) entity/model helpers — `createEntityE2E()`, `importModelE2E()`, `lockModelE2E()`, `importWorkflowE2E()`, `getEntityState()`, `getSMAuditEvents()`; (3) configuration — `TestMain` spins up postgres container, generates JWT RSA key, configures in-process `localproc.LocalProcessingService` for test processors/criteria, wires the OpenAPI conformance validator middleware that collects mismatches and exercised operationIds for the end-of-suite report.

The parity layer (e2e/parity) exports: `BackendFixture` interface yielding `NewTenant()` (fresh isolated tenant per test), `ComputeTenant()` (for gRPC dispatch), `BaseURL()`; `client.NewClient()` (typed HTTP driver: `CreateEntity`, `GetEntity`, `UpdateEntity`, `ImportModel`, `LockModel`, `ImportWorkflow`, `GetAuditEvents`, `GetEntityChanges`); discriminated audit types `AuditEvent` with `AsStateMachine()` / `AsEntityChange()` / `AsSystem()` subtype assertions (strict JSON unmarshalling via flat-alias pattern to enforce unknown-field rejection).

#### TestHealth (internal/e2e/e2e_test.go:193)
**Aim:** Server starts and responds to health checks.
**Asserts:** 200 OK on /api/health

### Transactions, concurrency, stress

#### TestTransaction_ConcurrentCreates (internal/e2e/transaction_test.go:17)
**Aim:** 10 concurrent entity creates all succeed without corruption.
**Scale:** 10 concurrent creators
**Asserts:** All 10 return 200; DB count = 10 exactly

#### TestTransaction_ConcurrentUpdates (internal/e2e/transaction_test.go:64)
**Aim:** Concurrent updates to same entity maintain consistency under conflict.
**Scale:** 5 concurrent updaters against 1 entity
**Asserts:** At least 1 succeeds; final state is one of racing payloads (not initial or merge)

#### TestTransaction_RollbackOnProcessorFailure (internal/e2e/transaction_test.go:116)
**Aim:** Transaction rollback on processor error leaves no orphaned entities.
**Asserts:** DB has zero entities after failure

#### TestTransaction_StressTest (internal/e2e/transaction_test.go:153)
**Aim:** 25 parallel creates succeed and version history is intact.
**Scale:** 25 concurrent creators
**Asserts:** All 25 succeed; DB count = 25; API listing responsive
**Notes:** This is the largest concurrency in the entire E2E suite

### Audit — entity history and workflow events

#### RunAuditEntityHistory (e2e/parity/audit.go:19)
**Aim:** Creating an entity with workflow produces both EntityChange and StateMachine audit events accessible via REST.
**Asserts:** EntityChange count ≥1; StateMachine count ≥2 (START + FINISH); all have transactionId
**Notes:** (parity)

#### RunAuditWorkflowEvents (e2e/parity/audit.go:108)
**Aim:** Audit events carry transactionId for cross-referencing.
**Notes:** (parity)

#### RunAuditPostTxIdMatchesWorkflowFinished (e2e/parity/audit.go:148)
**Aim:** POST /entity's returned transactionId is usable with /audit/entity/{id}/workflow/{txId}/finished.
**Asserts:** 200; response state=CREATED
**Notes:** (parity); Issue #20

### Concurrency contracts and safety

#### RunConcurrentConflictingUpdate (e2e/parity/contracts.go:22)
**Aim:** Concurrent conflicting updates to same entity maintain consistency under SI+FCW.
**Scale:** 2 concurrent updaters
**Asserts:** At least 1 succeeds; final state is one of the two values
**Notes:** (parity); memory may allow both (last-write-wins); postgres may serialize

#### RunConcurrentTransitionsDifferentEntities (e2e/parity/contracts.go:88)
**Aim:** 10 concurrent entity creates on different entities all succeed with processor mutations applied.
**Scale:** 10 concurrent creators on different entities
**Asserts:** All 10 created; all 10 have tag="foo"
**Notes:** (parity)

### Smoke test

#### RunSmokeTest (e2e/parity/smoke.go:29)
**Aim:** Minimal E2E proving parity fixture infrastructure (subprocess launch, JWT, HTTP client, fixture lifecycle) works against real cyoda-go binary.
**Asserts:** state=CREATED; data round-trips; all metadata fields populated
**Notes:** (parity); intentionally minimal — tests harness, not system features

### Multi-node: load balancing, consistency, transaction pinning

#### RunExternalAPI_10_01_LoadBalancerEndToEnd (e2e/parity/multinode/concurrency.go:25)
**Aim:** Round-robin entity creates across N cluster nodes all succeed and are readable from any node.
**Scale:** N nodes (≥2), N × N reads
**Asserts:** All creates succeed; all GETs from all nodes return matching k
**Notes:** Multi-node; requires ≥2 cyoda-go binaries sharing postgres

#### RunExternalAPI_10_02_ReadbackReachesAllReplicas (e2e/parity/multinode/concurrency.go:71)
**Aim:** Write to node A is immediately visible on read from node B (≠A) for all (A,B) pairs.
**Scale:** N nodes, N × (N-1) read-after-write pairs
**Asserts:** Every write immediately visible on every other node
**Notes:** Multi-node; tests cluster gossip + shared postgres coherence

#### RunExternalAPI_10_03_ParallelUpdatesSameEntity (e2e/parity/multinode/concurrency.go:118)
**Aim:** Concurrent updates from N nodes to same entity serialize without data loss (SI+FCW).
**Scale:** N nodes, N parallel writers
**Asserts:** Final counter is one of the racing values 1..N (not initial 0, not corrupted merge)
**Notes:** Multi-node; SI+FCW conflict serialization across cluster

#### RunWorkflowProc_CBD_TxPostPinnedToHomeNode (e2e/parity/multinode/cbd_tx_pinning.go:68)
**Aim:** CBD cascade's TX_post segment is pinned to home node, ensuring durability across cluster (Issue #27, Spec §16 case 14).
**Flow:** ≥2 cluster nodes; PUT update(approve) to home node → TX_pre.Commit → dispatch → TX_post.Begin/Commit all on node 0; verify all nodes see state=APPROVED, tag="foo", ≥2 distinct transactionIds in change history.
**Asserts:** Post-cascade state=APPROVED, tag="foo" visible on all nodes; ≥2 distinct transactionIds (CBD segmentation signature)
**Notes:** Multi-node; gRPC processor dispatch; harness gaps documented in code (strict same-node assertion deferred; forwarded-dispatch variant blocked by loopback SSRF guard)

### OpenAPI conformance and uncovered operations

#### TestOpenAPIConformanceReport (internal/e2e/zzz_openapi_conformance_test.go:43)
**Aim:** Collect OpenAPI spec mismatches and coverage gaps from all E2E tests; fail build on mismatches or uncovered ops.
**Flow:** Runs AFTER all other E2E tests (filename "zzz_" + flag.Lookup check) → drain validator middleware's collector and exercised set → write `_openapi-conformance-report.md` → in ModeEnforce, fail on mismatches OR incomplete coverage (skip coverage check if -run filtered).
**Asserts:** (ModeEnforce) No spec→response mismatches; all non-excluded operationIds exercised
**Notes:** Gates the build; `knownUncoveredOps` (stub IAM, OIDC, fetchEntityTransitions) explicitly allow-listed; incompatible with `-shuffle`

#### TestUncoveredOps_* (internal/e2e/uncovered_ops_test.go)
Eight minimal happy-path tests added per Task 10.1 / Issue #21 to cover ops that weren't exercised elsewhere:
- `getEntityStatistics` (line 21) — GET /api/entity/stats → 200, JSON array
- `getEntityStatisticsByState` (line 45) — GET /api/entity/stats/states → 200
- `deleteSingleEntity` (line 69) — DELETE /api/entity/{entityId} → 200 with id and transactionId
- `updateCollection` (line 100) — PUT /api/entity/{format} array → 200, JSON array
- `getAvailableEntityModels` (line 135) — GET /api/model/ → 200, JSON array
- `validateEntityModel` (line 159) — POST /api/model/validate/{name}/{version} → 200 with success field
- `deleteEntityModel` (line 188) — DELETE /api/model/{name}/{version} → 200 with success field
- `unlockEntityModel` (line 218) — PUT /api/model/{name}/{version}/unlock → 200 with success field

All check 2xx + schema via conformance middleware; intentionally minimal — they're there to clear `knownUncoveredOps` allow-list entries, not to assert behaviour.
