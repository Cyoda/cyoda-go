# OpenAPI Conformance Audit — 2026-04-29

Per #21 design Section 3. One row per operationId. Disposition values:
`match`, `fix-spec`, `fix-server`, `fix-both`, `out-of-scope`. Empty
disposition means TBD — populated by the per-domain commits (Tasks 3.1
through 10.1).

**Totals:** 83 operations declared in `api/openapi.yaml` (was 81; 2 ops added by Task 5.1 — see note below).
22 are excluded from codegen by `api/config.yaml` `exclude-tags` (`Stream Data`, `SQL-Schema`)
and marked out-of-scope. 61 are in scope for #21.

**Note (Task 5.1):** 2 ops (`getEntityTransitions`, `fetchEntityTransitions`) were previously
mounted by the server but undocumented. Added to the spec by Task 5.1 commit — total now 83 declared ops.

**Note (Task 6.1):** `api/generated.go` contains a gzip+base64-encoded snapshot of `api/openapi.yaml`
returned by `api.GetSwagger()` (the runtime validator's spec source). Since oapi-codegen v2.6.0 is
incompatible with Go 1.26, the snapshot is manually re-encoded after each spec edit (commit 7ec63f3).
The embedded spec now reflects all Task 6.1 changes: Envelope schema, entityIds→string[], corrected
content-types, new error status declarations.

**Inputs:**
- Spec: `api/openapi.yaml`
- Validator's record-mode output: `internal/e2e/_openapi-conformance-report.md` (gitignored — regenerate via `go test ./internal/e2e/... -count=1`)
- Validator wired in commit 95e3589 (Task 1.9), hardened in commit 444103c.

---

## Entity Management

| operationId | method | path | handler | spec response (today) | server response (today) | disposition | resolved-by-commit |
|---|---|---|---|---|---|---|---|
| create | POST | /entity/{format}/{entityName}/{modelVersion} | `internal/domain/entity/handler.go:268` | `type:array + $ref EntityTransactionResponse` (malformed — array with sibling $ref) | `[]any{map{transactionId,entityIds}}` — array wrapping one object | fix-spec | 9c721b4 |
| createCollection | POST | /entity/{format} | `internal/domain/entity/handler.go:551` | `type:array + $ref EntityTransactionResponse` (malformed — array with sibling $ref) | `[]any{map{transactionId,entityIds}}` — array wrapping one object | fix-spec | 9c721b4 |
| deleteEntities | DELETE | /entity/{entityName}/{modelVersion} | `internal/domain/entity/handler.go:488` | `$ref StreamDeleteResult` — object with `entityModelClassId`, `deleteResult`, optional `ids` | ~~`[]map{deleteResult,entityModelClassId}` — array not matching spec object~~ → `map{entityModelClassId,deleteResult}` (single object, handler fixed) | fix-server → resolved | 9c721b4 |
| deleteSingleEntity | DELETE | /entity/{entityId} | `internal/domain/entity/handler.go:447` | `$ref SingleDeleteResult` — object with `id`, `modelKey`, `transactionId` | `map{id,modelKey,transactionId}` — shape matches SingleDeleteResult | match | 9c721b4 |
| getAllEntities | GET | /entity/{entityName}/{modelVersion} | `internal/domain/entity/handler.go:508` | ~~`application/x-ndjson; type:array + $ref JsonNode`~~ → `application/json; type:array items:$ref Envelope` | `[]map{type,data,meta}` via `application/json` WriteJSON — now matches corrected spec | fix-spec (content-type + schema corrected) | 9c721b4 |
| getEntityChangesMetadata | GET | /entity/{entityId}/changes | `internal/domain/entity/handler.go:465` | `type:array + $ref EntityChangeMeta` (malformed — array with sibling $ref) | `[]map{changeType,timeOfChange,user,...}` — array | fix-spec | 9c721b4 |
| getEntityStatistics | GET | /entity/stats | `internal/domain/entity/handler.go:360` | `type:array + $ref ModelStatsDto` (malformed — array with sibling $ref) | `[]genapi.ModelStatsDto` | fix-spec | 9c721b4 |
| getEntityStatisticsByState | GET | /entity/stats/states | `internal/domain/entity/handler.go:380` | `type:array + $ref ModelStateStatsDto` (malformed — array with sibling $ref) | `[]genapi.ModelStateStatsDto` | fix-spec | 9c721b4 |
| getEntityStatisticsByStateForModel | GET | /entity/stats/states/{entityName}/{modelVersion} | `internal/domain/entity/handler.go:406` | `type:array + $ref ModelStateStatsDto` (malformed — array with sibling $ref) | `[]genapi.ModelStateStatsDto` | fix-spec | 9c721b4 |
| getEntityStatisticsForModel | GET | /entity/stats/{entityName}/{modelVersion} | `internal/domain/entity/handler.go:431` | `$ref ModelStatsDto` — single object | `genapi.ModelStatsDto` — matches | match | 9c721b4 |
| getOneEntity | GET | /entity/{entityId} | `internal/domain/entity/handler.go:326` | ~~`type:object` (loose — no named schema)~~ → `$ref Envelope` (named schema `{type,data,meta}` added) | `map{type,data,meta}` — now matches Envelope spec | fix-spec (Envelope schema added) | 9c721b4 |
| updateCollection | PUT | /entity/{format} | `internal/domain/entity/handler.go:604` | `type:array + $ref EntityTransactionResponse` (malformed — array with sibling $ref) | `[]any{map{transactionId,entityIds}}` — array wrapping one object | fix-spec | 9c721b4 |
| updateSingle | PUT | /entity/{format}/{entityId}/{transition} | `internal/domain/entity/handler.go:704` | ~~`$ref EntityTransactionResponse` — object with `transactionId`, `entityIds[]object`~~ → `entityIds` now `array of string` | `map{transactionId,entityIds}` where entityIds is `[]string` — now matches | fix-both (spec corrected to string[]) | 9c721b4 |
| updateSingleWithLoopback | PUT | /entity/{format}/{entityId} | `internal/domain/entity/handler.go:671` | ~~`$ref EntityTransactionResponse` — object with `transactionId`, `entityIds[]object`~~ → `entityIds` now `array of string` | `map{transactionId,entityIds}` where entityIds is `[]string` — now matches | fix-both (spec corrected to string[]) | 9c721b4 |
| getEntityTransitions | GET | /entity/{entityId}/transitions | `internal/domain/entity/transitions_handler.go:14` | `$ref TransitionNameList` — array of strings (added by Task 5.1) | `[]string` | fix-spec (added) | 302ba1e |
| fetchEntityTransitions | GET | /platform-api/entity/fetch/transitions | `internal/domain/entity/transitions_handler.go:73` | `$ref TransitionNameList` — array of strings (added by Task 5.1) | `[]string` | fix-spec (added) | 302ba1e |

---

## Edge Message

| operationId | method | path | handler | spec response (today) | server response (today) | disposition | resolved-by-commit |
|---|---|---|---|---|---|---|---|
| deleteMessage | DELETE | /message/{messageId} | `internal/domain/messaging/handler.go:190` | ~~`$ref EntityTransactionResponse` — object with `transactionId`, `entityIds[]object`~~ → `$ref MessageDeleteResponse` (`{entityIds:[]string}`, no transactionId) | `map{entityIds:[]string}` (no transactionId; entityIds is []string not []object) | fix-both → spec fixed to `MessageDeleteResponse`; 401/403/default added | e9d6985 |
| deleteMessages | DELETE | /message | `internal/domain/messaging/handler.go:222` | ~~`type:string`~~ → `type:array items:$ref MessageDeleteBatchResponse` | `[]map{entityIds,success}` — array now matches corrected spec | fix-spec → schema fixed to array of `MessageDeleteBatchResponse`; 401/403/default added | e9d6985 |
| getMessage | GET | /message/{messageId} | `internal/domain/messaging/handler.go:115` | ~~`content: type:string`~~ → `content: $ref EdgeMessagePayload` (polymorphic); ~~404: `ErrorResponse` with `application/json`~~ → `ProblemDetail` with `application/problem+json` | `map{header,metaData,content}` where content is now embedded JSON via `json.RawMessage` (was JSON-in-string); 404 uses `application/problem+json` | fix-both — JSON-in-string defect fixed in handler; EdgeMessagePayload schema added; 404 Content-Type spec corrected; 401/403/default added | e9d6985 |
| newMessage | POST | /message/new/{subject} | `internal/domain/messaging/handler.go:32` | ~~`type:string`~~ → `type:array items:$ref EntityTransactionResponse` | `[]map{entityIds,transactionId}` — array now matches corrected spec | fix-spec → response schema fixed to array; dead-code "not valid JSON" fallback replaced with explicit invariant-broken 500; 401/403/default added | e9d6985 |

---

## Entity Audit

| operationId | method | path | handler | spec response (today) | server response (today) | disposition | resolved-by-commit |
|---|---|---|---|---|---|---|---|
| getStateMachineFinishedEvent | GET | /audit/entity/{entityId}/workflow/{transactionId}/finished | `internal/domain/audit/handler.go:231` | `object{state,stopReason,success}` (required: state, stopReason, success) | `map{auditEventType,eventType,severity,utcTime,entityId,details,data,...}` — missing `stopReason`, `success` at top level; extra fields present | fix-spec | 04d6721 |
| searchEntityAuditEvents | GET | /audit/entity/{entityId} | `internal/domain/audit/handler.go:26` | `$ref EntityAuditEventsResponseDto` — paginated object with `items[]` (discriminated by eventType) | `map{items,pagination}` — outer shape matches; items mixin of EntityChange and StateMachine events; StateMachine events use non-spec eventType values (TRANSITION_MAKE vs TRANSITION_MADE) and null data field violations | fix-spec | 04d6721 |

---

## Entity Model

| operationId | method | path | handler | spec response (today) | server response (today) | disposition | resolved-by-commit |
|---|---|---|---|---|---|---|---|
| deleteEntityModel | DELETE | /model/{entityName}/{modelVersion} | `internal/domain/model/handler.go:142` | `$ref EntityModelActionResultDto` — object with `success`, `modelKey`, `message`, `modelId` | `genapi.EntityModelActionResultDto` struct | match | Task 8.1 |
| exportMetadata | GET | /model/export/{converter}/{entityName}/{modelVersion} | `internal/domain/model/handler.go:103` | `type:string` (loose — spec says string; content is actually JSON object) | raw JSON bytes written directly to response writer | fix-spec | Task 8.1 |
| getAvailableEntityModels | GET | /model/ | `internal/domain/model/handler.go:168` | `type:array + $ref EntityModelDto` (malformed — array with sibling $ref) | `[]genapi.EntityModelDto` | fix-spec | Task 8.1 |
| importEntityModel | POST | /model/import/{dataFormat}/{converter}/{entityName}/{modelVersion} | `internal/domain/model/handler.go:78` | `type:string, format:uuid` — bare UUID string | `result.ModelID` (UUID value) | match | Task 8.1 |
| lockEntityModel | PUT | /model/{entityName}/{modelVersion}/lock | `internal/domain/model/handler.go:116` | `$ref EntityModelActionResultDto` | `genapi.EntityModelActionResultDto` struct | match | Task 8.1 |
| setEntityModelChangeLevel | POST | /model/{entityName}/{modelVersion}/changeLevel/{changeLevel} | `internal/domain/model/handler.go:155` | `$ref EntityModelActionResultDto` | `genapi.EntityModelActionResultDto` struct | match | Task 8.1 |
| unlockEntityModel | PUT | /model/{entityName}/{modelVersion}/unlock | `internal/domain/model/handler.go:129` | `$ref EntityModelActionResultDto` | `genapi.EntityModelActionResultDto` struct | match | Task 8.1 |
| validateEntityModel | POST | /model/validate/{entityName}/{modelVersion} | `internal/domain/model/handler.go:191` | `$ref EntityModelActionResultDto` | `genapi.EntityModelActionResultDto` struct | fix-spec (404 used ErrorResponseDto→ProblemDetail; added 400 + 401/403/default) | Task 8.1 |

---

## Entity Model, Workflow

| operationId | method | path | handler | spec response (today) | server response (today) | disposition | resolved-by-commit |
|---|---|---|---|---|---|---|---|
| exportEntityModelWorkflow | GET | /model/{entityName}/{modelVersion}/workflow/export | `internal/domain/workflow/handler.go:122` | `$ref WorkflowExportResponseDto` | `map{entityName,modelVersion,workflows}` — shape matches; replaced inline ErrorResponseDto 401/403/500 with shared ProblemDetail $refs | fix-spec | Task 8.1 |
| importEntityModelWorkflow | POST | /model/{entityName}/{modelVersion}/workflow/import | `internal/domain/workflow/handler.go:33` | `content:{}` (no body — 200 with empty body) | `map{success:true}` — non-empty body when spec declares empty | fix-spec (server-is-truth: spec now declares `WorkflowImportSuccessDto {success:bool}`) | Task 8.1 |

---

## Search

| operationId | method | path | handler | spec response (today) | server response (today) | disposition | resolved-by-commit |
|---|---|---|---|---|---|---|---|
| cancelAsyncSearch | PUT | /search/async/{jobId}/cancel | `internal/domain/search/handler.go:338` | `$ref CancelAsyncSearchDto`; `400` (not-running); `404` (not-found); `401/403/default` added | `map{isCancelled,cancelled,currentSearchJobStatus}` on success; `ProblemDetailDto`-shaped body on 400 (already-completed); 404 on not-found | match (CancelAsyncSearchDto corrected: `cancelled` field `writeOnly:true` removed — server includes it in 200 response; 401/403/default added) | Task 9.1 |
| getAsyncSearchResults | GET | /search/async/{jobId} | `internal/domain/search/handler.go:254` | `$ref PagedEntityResultsDto` — `{content:[]EntityResultDto, page:PageMetadataDto}`; `400/404/401/403/default` declared | `map{content,page{number,size,totalElements,totalPages}}` — shape matches `PagedEntityResultsDto` | match (spec already correct; 401/403/default added) | Task 9.1 |
| getAsyncSearchStatus | GET | /search/async/{jobId}/status | `internal/domain/search/handler.go:239` | `$ref AsyncSearchStatusDto`; `404`; `401/403/default` added | `map{searchJobStatus,createTime,entitiesCount,calculationTimeMillis,expirationDate,finishTime?}` — shape matches `AsyncSearchStatusDto` | match (spec already correct; 401/403/default added) | Task 9.1 |
| searchEntities | POST | /search/direct/{entityName}/{modelVersion} | `internal/domain/search/handler.go:103` | `application/x-ndjson` AND `application/json`; 200 `type:array items:$ref EntityResultDto`; 400/404/408 (dual content-type); `401/403/default` added | ndjson stream of `map{type,data,meta}` — validator skips body for ndjson; status+headers validated for both content-types | match (`EntityMetadataDto` updated to add `lastUpdateTime` and `transactionId` matching actual server output; 401/403/default added) | Task 9.1 |
| submitAsyncSearchJob | POST | /search/async/{entityName}/{modelVersion} | `internal/domain/search/handler.go:186` | `type:string, format:uuid` — bare UUID; `400` (bad condition/type-mismatch/invalid-path) added; `401/403/default` added | `jobID` string value (200); `ProblemDetailDto`-shaped 400 for invalid conditions | fix-spec (400 was missing — test was hitting it but spec didn't declare it; now added; 401/403/default added) | Task 9.1 |

---

## OAuth, Keys

| operationId | method | path | handler | spec response (today) | server response (today) | disposition | resolved-by-commit |
|---|---|---|---|---|---|---|---|
| deleteJwtKeyPair | DELETE | /oauth/keys/keypair/{keyId} | `internal/domain/account/handler.go:93` | `content:{}` (empty body); `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| deleteTrustedKey | DELETE | /oauth/keys/trusted/{keyId} | `internal/domain/account/handler.go:113` | `content:{}` (empty body); `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| getCurrentJwtKeyPair | GET | /oauth/keys/keypair/current | `internal/domain/account/handler.go:89` | `$ref JwtKeyPairResponseDto`; `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| invalidateJwtKeyPair | POST | /oauth/keys/keypair/{keyId}/invalidate | `internal/domain/account/handler.go:97` | `content:{}` (empty body); `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| invalidateTrustedKey | POST | /oauth/keys/trusted/{keyId}/invalidate | `internal/domain/account/handler.go:117` | `content:{}` (empty body); `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| issueJwtKeyPair | POST | /oauth/keys/keypair | `internal/domain/account/handler.go:85` | `$ref JwtKeyPairResponseDto`; `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| listTrustedKeys | GET | /oauth/keys/trusted | `internal/domain/account/handler.go:105` | `type:array items:$ref TrustedKeyResponseDto`; `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| reactivateJwtKeyPair | POST | /oauth/keys/keypair/{keyId}/reactivate | `internal/domain/account/handler.go:101` | `$ref JwtKeyPairResponseDto`; `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| reactivateTrustedKey | POST | /oauth/keys/trusted/{keyId}/reactivate | `internal/domain/account/handler.go:121` | `$ref TrustedKeyResponseDto`; `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| registerTrustedKey | POST | /oauth/keys/trusted | `internal/domain/account/handler.go:109` | `$ref TrustedKeyResponseDto`; `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |

---

## OAuth, OIDC Providers

| operationId | method | path | handler | spec response (today) | server response (today) | disposition | resolved-by-commit |
|---|---|---|---|---|---|---|---|
| deleteOidcProvider | DELETE | /oauth/oidc/providers/{id} | `internal/domain/account/handler.go:137` | `content:{}` (empty body); `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| invalidateOidcProvider | POST | /oauth/oidc/providers/{id}/invalidate | `internal/domain/account/handler.go:145` | `content:{}` (empty body); `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| listOidcProviders | GET | /oauth/oidc/providers | `internal/domain/account/handler.go:125` | `type:array items:$ref OidcProviderResponseDto`; `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| reactivateOidcProvider | POST | /oauth/oidc/providers/{id}/reactivate | `internal/domain/account/handler.go:149` | `$ref OidcProviderResponseDto`; `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| registerOidcProvider | POST | /oauth/oidc/providers | `internal/domain/account/handler.go:129` | `$ref OidcProviderResponseDto`; `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| reloadOidcProviders | POST | /oauth/oidc/providers/reload | `internal/domain/account/handler.go:133` | `content:{}` (empty body); `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| updateOidcProvider | PATCH | /oauth/oidc/providers/{id} | `internal/domain/account/handler.go:141` | `$ref OidcProviderResponseDto`; `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |

---

## User, Account

| operationId | method | path | handler | spec response (today) | server response (today) | disposition | resolved-by-commit |
|---|---|---|---|---|---|---|---|
| accountGet | GET | /account | `internal/domain/account/handler.go:27` | `$ref UserAccountInfoResponseDto`; `401` added; `UserRoleDto.desc` made optional (server has no role desc) | `map{userAccountInfo{userId,userName,legalEntity,roles[{id}],currentSubscription}}` — matches spec after desc fix | match | 766df8b |
| accountSubscriptionsGet | GET | /account/subscriptions | `internal/domain/account/handler.go:61` | `$ref SubscriptionsResponseDto`; `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |

---

## User, Machine

| operationId | method | path | handler | spec response (today) | server response (today) | disposition | resolved-by-commit |
|---|---|---|---|---|---|---|---|
| createTechnicalUser | POST | /clients | `internal/domain/account/handler.go:69` | `$ref TechnicalUserCredentialsDto`; `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| deleteTechnicalUser | DELETE | /clients/{clientId} | `internal/domain/account/handler.go:73` | `$ref DeleteTechnicalUser200ResponseDto`; `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| getTechnicalUserToken | POST | /oauth/token | real impl in `internal/auth/token.go` (account handler stub bypassed) | `$ref TokenResponseDto`; 200/400/401/403/500 declared — matches server for `client_credentials`; note: `issued_token_type` enum only has `access_token` but server returns `jwt` for token-exchange (fix-spec minor) | real impl via `auth/token.go`; `client_credentials` wire matches spec; `token-exchange` `issued_token_type` enum drift noted | fix-spec (minor: issued_token_type enum) | 766df8b |
| listTechnicalUsers | GET | /clients | `internal/domain/account/handler.go:65` | `type:array items:$ref TechnicalUserDto`; `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |
| resetTechnicalUserSecret | PUT | /clients/{clientId}/secret | `internal/domain/account/handler.go:77` | `$ref TechnicalUserCredentialsDto`; `501` added | `stub → 501` | out-of-scope-not-implemented (#194) | 766df8b |

---

## Excluded: Stream Data (13 ops)

| operationId | method | path | handler | spec response (today) | server response (today) | disposition | resolved-by-commit |
|---|---|---|---|---|---|---|---|
| delete | DELETE | /stream-data/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| deleteStreamDataConfig | DELETE | /stream-data/config/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| exportAll | GET | /stream-data/export/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| exportAll_1 | GET | /stream-data/export/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| exportByIds | POST | /stream-data/export/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| getAllConfigs | GET | /stream-data/config/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| getIndexs | GET | /stream-data/index/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| getQueryPlan | GET | /stream-data/query-plan/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| getStreamData | POST | /stream-data/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| getStreamDataConfig | GET | /stream-data/config/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| importContainer | POST | /stream-data/import/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| saveStreamDataConfig | POST | /stream-data/config/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| updateStreamDataConfig | PUT | /stream-data/config/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |

---

## Excluded: SQL-Schema (9 ops)

| operationId | method | path | handler | spec response (today) | server response (today) | disposition | resolved-by-commit |
|---|---|---|---|---|---|---|---|
| deleteSchema | DELETE | /sql/schema/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| deleteSchemaByName | DELETE | /sql/schema/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| genTables | POST | /sql/schema/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| getSchema | GET | /sql/schema/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| getSchemaByName | GET | /sql/schema/ | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| getSchemas | GET | /sql/schema/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| putSchema | PUT | /sql/schema/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| saveSchema | POST | /sql/schema/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |
| updateTables | PUT | /sql/schema/... | out-of-scope | n/a (excluded by api/config.yaml) | n/a (excluded by api/config.yaml) | out-of-scope | |

---

## Validator state at end of #21

- Mode: `ModeEnforce` (flipped in Task 11.2)
- Mismatches: 0
- Uncovered ops accepted via `knownUncoveredOps` (in `internal/e2e/zzz_openapi_conformance_test.go`):
  - 22 stub-implemented IAM/account ops — see #194
  - 1 transitions handler outside generated ServerInterface — see Task 5.1 commit `302ba1e` Option B note
- Excluded-tag ops (Stream Data, CQL Execution Statistics, SQL-Schema) filtered out of `allOperationIds` in `internal/e2e/e2e_test.go`.

Future readers: if a new operation is added to the spec and not yet covered by
E2E, the conformance test will fail with that operationId in the uncovered
list. Either add an E2E test or, if intentionally uncovered, add the operationId
to `knownUncoveredOps` with a comment explaining why (and link to the tracking
issue).
