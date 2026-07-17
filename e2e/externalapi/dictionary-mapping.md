# External API Scenario Dictionary — cyoda-go mapping

Triage of all 100 scenarios in `e2e/externalapi/scenarios/` against the
current cyoda-go implementation. (The plan's estimate of ~85 was based on
an earlier draft; the authoritative count from `grep -cE "^\s*- id:"` is 100.)

Status vocabulary:

- `covered_by:<fn>` — already exists as a parity `Run*`.
- `new:<fn>` — implemented as part of tranche 1 (this PR).
- `pending:tranche-N` — planned for a later tranche; not implemented.
- `internal_only_skip` — tests platform internals not reachable via
  HTTPDriver (gRPC-only endpoint, internal facade call, or RSocket-only
  transport with no REST equivalent in this file).
- `shape_only_skip` — shape-only assertion better expressed as JSON
  Schema check than scenario run.
- `gap_on_our_side` — endpoint or capability missing in cyoda-go
  today; scenario cannot run. See `notes`.

---

## 01-model-lifecycle.yaml

| source_id | cyoda_go_status | notes |
|-----------|-----------------|-------|
| model-lifecycle/01-register-model-from-sample | new:RunExternalAPI_01_01_RegisterModel | tranche 1 |
| model-lifecycle/02-upsert-model-extends-schema | new:RunExternalAPI_01_02_UpsertExtendsSchema | tranche 1 |
| model-lifecycle/03-upsert-model-with-incompatible-type | new:RunExternalAPI_01_03_UpsertIncompatibleType | tranche 1 |
| model-lifecycle/04-reregister-same-schema | new:RunExternalAPI_01_04_ReregisterIdempotent | tranche 1 |
| model-lifecycle/05-lock-model | new:RunExternalAPI_01_05_LockModel | tranche 1 |
| model-lifecycle/06-unlock-model | new:RunExternalAPI_01_06_UnlockModel | tranche 1 |
| model-lifecycle/07-lock-twice-is-rejected | new:RunExternalAPI_01_07_LockTwiceRejected | tranche 1; #128 closed: relock now emits dictionary-aligned `MODEL_ALREADY_LOCKED` code. |
| model-lifecycle/08-delete-model | new:RunExternalAPI_01_08_DeleteModel | tranche 1 |
| model-lifecycle/09-list-models-empty | new:RunExternalAPI_01_09_ListModelsEmpty | tranche 1 |
| model-lifecycle/10-list-models-non-empty | new:RunExternalAPI_01_10_ListModelsNonEmpty | tranche 1 |
| model-lifecycle/11-export-metadata-as-json-schema | new:RunExternalAPI_01_11_ExportMetadataViews | tranche 1 |
| model-lifecycle/12-parse-nobel-laureates-sample | new:RunExternalAPI_01_12_NobelLaureatesSample | tranche 1 |
| model-lifecycle/13-parse-lei-data-sample | new:RunExternalAPI_01_13_LEISample | tranche 1 |

---

## 02-change-level-governance.yaml

| source_id | cyoda_go_status | notes |
|-----------|-----------------|-------|
| change-level/01-set-structural | new:RunExternalAPI_02_01_SetChangeLevelStructural | tranche 2 — happy path |
| change-level/02-structural-null-field-does-not-grow-changelog | new:RunExternalAPI_02_02_StructuralNullFieldNoChangelog | tranche 2 — null-array regression |
| change-level/03-type-widening-int-to-float-incompatible | new:RunExternalAPI_02_03_TypeWideningIntToFloat | tranche 2 negative path, `equiv_or_better` after #129: cyoda-go emits `INCOMPATIBLE_TYPE` @400 with structured `properties` (`fieldPath`, `expectedType`, `actualType`); dictionary's `FoundIncompatibleTypeWithEntityModelException` is matched semantically. Same code path as 12/02. |
| change-level/04-type-narrowing-float-to-int-compatible | new:RunExternalAPI_02_04_TypeNarrowingFloatToInt | tranche 2 — int-into-float accepted |
| change-level/05-updated-schema-on-unlocked-then-lock-and-save | new:RunExternalAPI_02_05_UpdatedSchemaThenLockAndSave | tranche 2 — schema-extend-then-lock |
| change-level/06-multinode-type-level-with-all-fields-model | new:RunExternalAPI_02_06_MultinodeTypeLevelAllFields | tranche 2 — N=10 bounded (dictionary specifies 100; parity smoke does not need load testing) |
| change-level/07-structural-concurrent-extend-30-versions | new:RunExternalAPI_02_07_ConcurrentExtendVersions | tranche 2 — N=5 bounded (dictionary specifies 30; parity smoke does not need load testing) |

---

## 03-entity-ingestion-single.yaml

| source_id | cyoda_go_status | notes |
|-----------|-----------------|-------|
| ingest-single/01-success-path | new:RunExternalAPI_03_01_CreateEntitySuccess | tranche 1 |
| ingest-single/02-import-list-of-objects-in-one-call | new:RunExternalAPI_03_02_ListOfObjects | tranche 1 |
| ingest-single/03-all-fields-model-round-trip | new:RunExternalAPI_03_03_AllFieldsRoundTrip | tranche 1 |
| ingest-single/04-save-family-rich-nested-array | new:RunExternalAPI_03_04_FamilyNested | tranche 1 |
| ingest-single/05-grpc-create-entity | internal_only_skip | endpoint block has `grpc:` only — no `rest:` line |
| ingest-single/06-grpc-multiple-entities-single-endpoint-warning | internal_only_skip | endpoint block has `grpc:` only — no `rest:` line |

---

## 04-entity-ingestion-collection.yaml

| source_id | cyoda_go_status | notes |
|-----------|-----------------|-------|
| ingest-collection/01-family-and-pets-single-transaction | new:RunExternalAPI_04_01_FamilyAndPets | tranche 1 |
| ingest-collection/02-update-collection-age-increment | new:RunExternalAPI_04_02_UpdateCollectionAge | tranche 1 |
| ingest-collection/03-grpc-create-multiple-by-collection-rpc | internal_only_skip | endpoint block has `grpc:` only — no `rest:` line |
| ingest-collection/04-parsing-spec-transaction-window | new:RunExternalAPI_04_04_TransactionWindow | tranche 1 |

---

## 05-entity-update.yaml

| source_id | cyoda_go_status | notes |
|-----------|-----------------|-------|
| update/01-nested-array-append-and-modify | new:RunExternalAPI_05_01_NestedArrayAppendAndModify | tranche 2 — uses UpdateEntity (UPDATE transition) per YAML's `update_entity_transition` |
| update/02-nested-array-shrink-and-modify-top-level | new:RunExternalAPI_05_02_NestedArrayShrinkAndModify | tranche 2 |
| update/03-remove-object-and-array-keep-one-field | new:RunExternalAPI_05_03_RemoveObjectAndArrayKeepOneField | tranche 2 — note: cyoda-go sets removed fields to null (does not drop) |
| update/04-populate-minimal-into-full | new:RunExternalAPI_05_04_PopulateMinimalIntoFull | tranche 2 — adapted for isolated-tenant: model seeded with full schema upfront. The YAML implies cross-scenario shared model state (01-04 share a model), which doesn't hold per-tenant. Worth proposing upstream as an explicit `preconditions:` block. |
| update/05-loopback-absent-transition | new:RunExternalAPI_05_05_LoopbackAbsentTransition | tranche 2 — uses UpdateEntityData (loopback path) |
| update/06-unchanged-payload-still-transitions | new:RunExternalAPI_05_06_UnchangedPayloadStillTransitions | tranche 2 — verifies no error on identical-payload PUT; deeper assertion (audit-event count growing by 1) is tranche-3 audit work |

---

## 06-entity-delete.yaml

| source_id | cyoda_go_status | notes |
|-----------|-----------------|-------|
| delete/01-single-by-id | new:RunExternalAPI_06_01_DeleteSingle | tranche 1 |
| delete/02-all-by-model-version | new:RunExternalAPI_06_02_DeleteByModel | tranche 1 |
| delete/03-by-condition-jsonpath-equals | gap_on_our_side | The OpenAPI generator emits `DeleteEntitiesJSONRequestBody = AbstractConditionDto` (`api/generated.go:DeleteEntitiesJSONRequestBody`), but `internal/domain/entity/handler.go:DeleteEntities` does not read the body — it only consults `DeleteEntitiesParams` (`transactionSize`/`pointInTime`/`verbose`) and calls `DeleteAllEntities(name, version)`. Implementing this means parsing the existing `AbstractConditionDto` typedef, extending the service with a condition-aware delete path, and propagating to the storage SPI. |
| delete/04-by-condition-not-null | gap_on_our_side | same as 06/03 — handler ignores the existing `AbstractConditionDto` body type |
| delete/05-by-condition-at-point-in-time-too-many-entities | gap_on_our_side | same as 06/03 + `entitySearchLimit` enforcement on condition+pointInTime deletes is missing |
| delete/06-all-by-model-at-point-in-time | new:RunExternalAPI_06_06_DeleteAtPointInTime (skipped pending #124) | tranche 1 — test body in place; t.Skip until #124 ships in v0.7.0. `Handler.DeleteEntities` ignores `params.PointInTime`; storage SPI has no `DeleteAllAsAt`. Cross-repo fix (SPI tag + plugin impls + handler wiring) tracked in #124. |

---

## 07-point-in-time-and-changelog.yaml

| source_id | cyoda_go_status | notes |
|-----------|-----------------|-------|
| pit/01-get-single-entity-at-point-in-time | new:RunExternalAPI_07_01_GetEntityAtPointInTime | tranche 2 — three timestamps, three states |
| pit/02-get-single-entity-by-transaction-id | new:RunExternalAPI_07_02_GetEntityByTransactionID | tranche 2 — server-side gap fixed in #150 (`GetOneEntity` now propagates `transactionId` and the service scans version history for the matching version, returning ENTITY_NOT_FOUND@404 on a miss). Parity-client `GetEntityByTransactionID` helper delivered via #132. |
| pit/03-entity-change-history-full | new:RunExternalAPI_07_03_ChangeHistoryFull | tranche 2 — note: cyoda-go uses `"CREATED"` / `"UPDATED"` enum values where the YAML spec uses `"CREATE"` / `"UPDATE"`. `different_naming_same_level`. Test asserts on cyoda-go's emission. |
| pit/04-entity-change-history-point-in-time | new:RunExternalAPI_07_04_ChangeHistoryAtPointInTime | tranche 2 — `equiv_or_better`. Surfaced and fixed cyoda-go bug #152: `GetEntityChangesMetadata` handler was silently dropping the `pointInTime` query param. Service-level filter now truncates the history to entries with `timeOfChange <= pointInTime`. Parity-client `GetEntityChangesAt` helper delivered via #132. |
| pit/05-change-history-non-existent-entity | new:RunExternalAPI_07_05_ChangeHistoryNonExistent | tranche 2 — string-based 404 detection (stopgap until GetEntityChangesRaw, added in Phase 5 of tranche 2, is wired in). NB: also surfaced and fixed a real parity bug in postgres plugin's `GetVersionHistory` (was returning empty slice instead of `spi.ErrNotFound` for unknown entities). |

---

## 08-workflow-import-export.yaml

| source_id | cyoda_go_status | notes |
|-----------|-----------------|-------|
| wf-import/01-simple-automated-transition | new:RunExternalAPI_08_01_SimpleAutomatedTransition | tranche 3; `different_naming_same_level` schema adaptations: dictionary `to`/`automated`/`criterion shape` → cyoda-go `next`/`manual:false`/`{type:"simple",jsonPath,operatorType,value}` |
| wf-import/02-defaults-applied-and-returned | new:RunExternalAPI_08_02_DefaultsAppliedAndReturned | tranche 3 |
| wf-import/03-advanced-criteria-and-processors | new:RunExternalAPI_08_03_AdvancedCriteriaAndProcessors | tranche 3; group-criterion `clauses` → `conditions` field |
| wf-import/04-strategy-replace | new:RunExternalAPI_08_04_StrategyReplace | tranche 3 |
| wf-import/05-strategy-activate | new:RunExternalAPI_08_05_StrategyActivate | tranche 3 |
| wf-import/06-strategy-merge | new:RunExternalAPI_08_06_StrategyMerge | tranche 3 |

---

## 09-workflow-externalization.yaml

The `parity.BackendFixture` exposes a compute-tenant matched to the running `cmd/compute-test-client`; the workflow externalization scenarios use it to exercise the gRPC streaming path without a separate process.

| source_id | cyoda_go_status | notes |
|-----------|-----------------|-------|
| ext/01-sync-processor-success | new:RunExternalAPI_09_01_SyncProcessorSuccess | tranche 3 |
| ext/02-sync-processor-exception-rolls-back | new:RunExternalAPI_09_02_SyncProcessorExceptionRollsBack | tranche 3 — `equiv_or_better`: cyoda-go emits `WORKFLOW_FAILED` @400 (specific code, transaction cancelled, entity not persisted); dictionary's implicit 5xx is more generic |
| ext/03-async-same-tx-exception-rolls-back | new:RunExternalAPI_09_03_AsyncSameTxExceptionRollsBack | tranche 3 — same `WORKFLOW_FAILED` @400 as 09/02 (engine treats SYNC + ASYNC_SAME_TX exceptions identically) |
| ext/04-async-new-tx-exception-keeps-initial-save | new:RunExternalAPI_09_04_AsyncNewTxExceptionKeepsInitial | tranche 3 — cyoda-go's `ASYNC_NEW_TX` failures are non-fatal per `engine.go`; entity reaches DONE matching dictionary's "initial save survives" semantic |
| ext/05-sync-error-flag-rolls-back | (skipped) | pending compute-test-client error-flag processor; processorFunc signature doesn't expose ProcessorResponse warnings/errors; out of tranche-3 scope |
| ext/06-async-same-tx-error-flag-rolls-back | (skipped) | same as 09/05 (ASYNC_SAME_TX) |
| ext/07-async-new-tx-error-flag-keeps-initial-save | (skipped) | same as 09/05 (ASYNC_NEW_TX) |
| ext/08-no-external-registered-fails | new:RunExternalAPI_09_08_NoExternalRegisteredFails | tranche 3 — `equiv_or_better`: cyoda-go emits `NO_COMPUTE_MEMBER_FOR_TAG` @503 retryable ("no matching calculation member"); fresh tenant has no calc member registered. Now classifies the no-member case as a transient compute-infra condition (retryable 503), not a client error — strictly stronger than the earlier `WORKFLOW_FAILED` @400 mapping, which mislabelled an unreachable/unregistered compute node as a bad request. |
| ext/09-external-disconnect-succeeds-on-retry | (skipped) | multi-member orchestration not in tranche-3 fixture |
| ext/10-external-timeout-failover | (skipped) | no per-call timeout config visible in cyoda-go; non-deterministic without one |
| ext/11-processing-node-disconnects-mid-request | (skipped) | needs deterministic mid-request gRPC disconnect (fixture orchestration) |
| ext/12-externalized-criterion-skips-call-when-not-matched | new:RunExternalAPI_09_12_ExternalizedCriterionSkipsCall | tranche 3 — uses externalized `always-false` criterion; entity stays at CREATED |

---

## 10-concurrency-and-multinode.yaml

| source_id | cyoda_go_status | notes |
|-----------|-----------------|-------|
| multi/01-create-and-delete-through-load-balancer | new:RunExternalAPI_10_01_LoadBalancerEndToEnd | tranche 3 — postgres-only (cluster-shareable via `e2e/parity/multinode/`); cassandra picks up via cyoda-go-cassandra#35 |
| multi/02-readback-reaches-all-replicas | new:RunExternalAPI_10_02_ReadbackReachesAllReplicas | tranche 3 — postgres-only; same notes as 10/01 |
| multi/03-parallel-updates-to-same-entity | new:RunExternalAPI_10_03_ParallelUpdatesSameEntity | tranche 3 — postgres-only; same notes as 10/01 |

---

## 11-edge-message.yaml

| source_id | cyoda_go_status | notes |
|-----------|-----------------|-------|
| edge-msg/01-save-single | new:RunExternalAPI_11_01_SaveSingle | tranche 3 — `different_naming_same_level` URL drift: cyoda-go uses `/api/message/new/{subject}` and reads X-* + Content-* fields from HTTP headers; dictionary uses `/edge-message` and embeds them in the body. Helper `client.CreateMessageWithHeaders` bridges the gap (Phase 4) |
| edge-msg/02-delete-single | new:RunExternalAPI_11_02_DeleteSingle | tranche 3 — same URL drift as 11/01 |
| edge-msg/03-delete-collection | new:RunExternalAPI_11_03_DeleteCollection | tranche 3 — same URL drift as 11/01; corrected Phase 0.2 misdiagnosis (handler at `messaging/handler.go:222` IS delete-by-id-list, not delete-all-paged); #134 closed |

---

## 12-negative-validation.yaml

| source_id | cyoda_go_status | notes |
|-----------|-----------------|-------|
| neg/01-create-entity-on-unlocked-model | new:RunExternalAPI_12_01_CreateEntityOnUnlockedModel | tranche 2 — `equiv_or_better`: cyoda-go emits `MODEL_NOT_LOCKED` @409 (specific); dictionary's `EntityModelWrongStateException` is more generic. Tightened assertion uses cyoda-go's code; propose upstream tightening. |
| neg/02-create-entity-with-incompatible-type | new:RunExternalAPI_12_02_CreateEntityWithIncompatibleType | tranche 2 negative path, `equiv_or_better` after #129: cyoda-go emits `INCOMPATIBLE_TYPE` @400 with structured `properties` (`fieldPath`, `expectedType`, `actualType`, `entityName`, `entityVersion`); dictionary's `FoundIncompatibleTypeWithEntityModelException` is matched semantically. Same code path as 02/03. |
| neg/03-set-change-level-invalid-enum | new:RunExternalAPI_12_03_SetChangeLevelInvalidEnum | tranche 2 — `equiv_or_better` after #130: cyoda-go emits `INVALID_CHANGE_LEVEL` @400 with structured `entityName`, `entityVersion`, `suppliedValue`, `validValues` props; dictionary requires generic "Invalid enum value" detail. Tightened assertion uses cyoda-go's specific code. |
| neg/04-get-single-entity-at-time-before-creation | new:RunExternalAPI_12_04_GetEntityAtTimeBeforeCreation | tranche 2 / #132 — parity-client surface delivered (`GetEntityAtRaw` now returns `(int, []byte, error)`); test asserts ENTITY_NOT_FOUND@404. equiv_or_better. |
| neg/05-get-single-entity-with-bogus-transaction-id | new:RunExternalAPI_12_05_GetEntityWithBogusTransactionID | tranche 2 — server-side gap fixed in #150: a bogus `transactionId` now returns `ENTITY_NOT_FOUND@404` (`equiv_or_better`, matches `EntityNotFoundException`). Parity-client `GetEntityByTransactionIDRaw` helper delivered via #132. |
| neg/06-get-changes-for-missing-entity | new:RunExternalAPI_12_06_GetChangesForMissingEntity | tranche 2 — `equiv_or_better`: cyoda-go emits `ENTITY_NOT_FOUND` @404; matches dictionary's `EntityNotFoundException` semantically. Tightened assertion. |
| neg/07-condition-delete-at-pit-too-many-matches | gap_on_our_side (#124) | tranche 2 — t.Skip pending #124. Whole delete-by-condition surface is a v0.7.0 server-side gap (handler ignores condition body and pointInTime). |
| neg/08-update-with-unknown-transition | new:RunExternalAPI_12_08_UpdateUnknownTransition | tranche 2 — `equiv_or_better` after wiring `TRANSITION_NOT_FOUND` into the engine-failure code path (review C1 fix). cyoda-go emits `TRANSITION_NOT_FOUND` @400 — matches dictionary's `(IllegalTransition\|TransitionNotFound)` semantically. |
| neg/09-get-model-after-delete | new:RunExternalAPI_12_09_GetModelAfterDelete | tranche 2 — `different_naming_same_level`: cyoda-go has no per-model GET endpoint; test verifies via `ListModels` and confirms absence. Semantically equivalent to per-model 404; reconcile in tranche-5 cloud smoke if cyoda-go ever adds a per-model GET. |
| neg/10-import-workflow-on-unknown-model | new:RunExternalAPI_12_10_ImportWorkflowOnUnknownModel | tranche 2 negative path; resolved by #131. cyoda-go now returns HTTP 404 with `MODEL_NOT_FOUND` for workflow imports on non-existent models, matching dictionary's `(ModelNotFound\|EntityModelNotFound)` regex. `equiv_or_better`. |

---

## 13-numeric-types.yaml

Note: `numeric/03` and `numeric/05` carry `internal_only: true` in the YAML — they
require `EntityModelFacade.upsert()` with a custom `ParsingSpec(intScope=BYTE,
decimalScope=FLOAT)`, which is not reachable through any REST or gRPC endpoint.
`numeric/02` is a cross-reference to neg/02 with no independent steps, so it is
`shape_only_skip`. `numeric/05ext` is the REST-reachable external equivalent of
`numeric/05` and is `pending:tranche-4`.

| source_id | cyoda_go_status | notes |
|-----------|-----------------|-------|
| numeric/01-compatible-int-lands-in-double-field | new:RunExternalAPI_13_01_IntegerLandsInDoubleField | tranche 4 |
| numeric/02-incompatible-decimal-after-int-cross-ref | cross_ref:neg/02 | tranche 2 — listed in 12/02; no independent test |
| numeric/03-parsing-spec-intScope-byte | internal_only_skip | requires `EntityModelFacade.upsert(ParsingSpec(intScope=BYTE))`; not on external surface |
| numeric/04-default-intScope-integer-external | new:RunExternalAPI_13_04_DefaultIntegerScopeINTEGER | tranche 4 |
| numeric/05-polymorphic-field-after-merge | internal_only_skip | requires `EntityModelFacade.upsert()` with custom `ParsingSpec`; not reachable via REST or gRPC |
| numeric/05ext-polymorphic-field-after-merge-external | new:RunExternalAPI_13_05ext_PolymorphicMergeWithDefaultScopes | tranche 4 — uses `UpdateModelFromSample` for the second sample; polymorphic ordering follows iota stability ([INTEGER, BOOLEAN], [INTEGER, STRING]) |
| numeric/06-double-at-max-boundary-round-trip | new:RunExternalAPI_13_06_DoubleAtMaxBoundary | tranche 4 |
| numeric/07-big-decimal-high-precision-round-trip | new:RunExternalAPI_13_07_BigDecimal20Plus18 | tranche 4 — `stripTrailingZeros` numeric comparison via `math/big.Float`; uses `GetEntityBodyRaw` to preserve precision |
| numeric/08-unbound-decimal-arbitrary-precision | new:RunExternalAPI_13_08_UnboundDecimalGT18Frac | tranche 4 — `toPlainString` numeric comparison |
| numeric/09-big-integer-38-digits | new:RunExternalAPI_13_09_BigInteger38Digit | tranche 4 |
| numeric/10-unbound-integer-40-digits | new:RunExternalAPI_13_10_UnboundInteger40Digit | tranche 4 |
| numeric/11-search-condition-integer-against-double-field | new:RunExternalAPI_13_11_SearchIntegerAgainstDouble | tranche 4 — uses async + direct search; sample value adapted to `100.1` (instead of dictionary's `100.0`) because cyoda-go's classifier routes scale=0 values to INTEGER |

---

## 14-polymorphism.yaml

Note: `poly/02` carries `internal_only: true` in the YAML — it tests the internal
TreeNode save/reconstruct API, not the REST surface. `poly/05` uses an RSocket
`treeNode.getData` transport as its primary assertion path but also includes a REST
direct-search fallback step; it is `pending:tranche-4` (the REST step is exercisable).
`poly/07` carries `shape_only: true` in the YAML.

| source_id | cyoda_go_status | notes |
|-----------|-----------------|-------|
| poly/01-mixed-object-or-string-at-same-path | new:RunExternalAPI_14_01_MixedObjectOrStringAtSamePath | tranche 4 — surfaced server-side gap; FIXED IN-TRANCHE via `validatePolymorphicFallback` in `internal/domain/model/schema/validate.go` |
| poly/02-tree-node-mixed-children-round-trip | internal_only_skip | `internal_only: true` in YAML; requires internal TreeNode save/reconstruct API |
| poly/03-polymorphic-value-array-in-all-fields-model | new:RunExternalAPI_14_03_PolymorphicValueArray | tranche 4 — STRING/DOUBLE/BOOLEAN classified; UUID detection deferred to v0.7.0 (#136); inline assertion omits UUID type-set entry |
| poly/04-polymorphic-timestamp-array-in-all-fields-model | gap_on_our_side (#137) | tranche 4 — `t.Skip("pending #137")`; cyoda-go classifies all temporal strings as STRING; LOCAL_DATE / YEAR_MONTH / ZONED_DATE_TIME subtype detection deferred to v0.7.0 (#137); round-trip itself works |
| poly/05-trino-search-on-polymorphic-scalar | new:RunExternalAPI_14_05_TrinoSearchOnPolymorphicScalarRESTHalf | tranche 4 — REST half only; RSocket leg unreachable (no cyoda-go analogue) |
| poly/06-reject-condition-with-wrong-scalar-type | new:RunExternalAPI_14_06_RejectWrongTypeCondition | tranche 4 — surfaced server-side gap (silent acceptance); FIXED IN-TRANCHE via search-time condition-value validator (`internal/domain/search/condition_type_validate.go` + `ErrCodeConditionTypeMismatch`); `equiv_or_better`: cyoda-go emits `CONDITION_TYPE_MISMATCH` @400 vs dictionary's `InvalidTypesInClientConditionException` |
| poly/07-error-body-shape-for-invalid-polymorphic-types | shape_only_skip | `shape_only: true` in YAML; shape contract verified by JSON Schema, not scenario run |

---

## 00-endpoints.yaml

`00-endpoints.yaml` is an endpoint reference catalogue, not a scenario file. It lists
the REST and gRPC surface of the External API (URLs, HTTP methods, gRPC service names
and request/response types) without defining any `id:`-keyed scenario sequences. It has
no rows to triage and does not belong in the per-file tables above.

---

## Reverse section — parity entries not yet in upstream dictionary

The following `Run*` functions are registered in `e2e/parity/registry.go`'s `allTests`
slice and cover behaviour the upstream `e2e/externalapi/scenarios/` dictionary does not
yet describe. They are candidates for future cyoda-cloud dictionary contributions.

All twelve entries listed in the plan were verified present in `allTests`:

| parity name | topic |
|-------------|-------|
| `NumericClassification18DigitDecimal` | 18-digit decimal classified as BIG_DECIMAL |
| `NumericClassification20DigitDecimal` | 20-digit decimal classified correctly |
| `NumericClassificationLargeInteger` | large integer classification boundary |
| `NumericClassificationIntegerSchemaAcceptsInteger` | integer schema accepts integer value |
| `NumericClassificationIntegerSchemaRejectsDecimal` | integer schema rejects decimal value |
| `SchemaExtensionsSequentialFoldAcrossRequests` | sequential schema fold across multiple import requests |
| `SchemaExtensionCrossBackendByteIdentity` | cross-backend byte-identity of stored schema |
| `SchemaExtensionAtomicRejection` | schema extension atomically rejected on invalid input |
| `SchemaExtensionConcurrentConvergence` | concurrent schema extensions converge to same result |
| `SchemaExtensionSavepointOnLockFoldEquivalence` | savepoint-on-lock fold is equivalent to sequential fold |
| `SchemaExtensionLocalCacheInvalidationOnCommit` | local cache invalidated when schema commit lands |
| `SchemaExtensionByteIdentityProperty` | byte-level identity of schema roundtrip |
