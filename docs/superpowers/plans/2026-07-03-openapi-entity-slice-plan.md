# OpenAPI Contract Reconciliation — Entity Slice Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reconcile the `api/openapi.yaml` **entity group** with the running server under an
error-code-coverage validator: typed-but-open `meta`, a real conditional `deleteEntities`,
`pointInTime`-honouring list reads, tightened request bodies, documented unique-key codes, and
HTTP↔gRPC parity for `changeType`/`meta`.

**Architecture:** Extend the existing `internal/e2e/openapivalidator` collector with
errorCode-string granularity (never a parallel system). Reconcile each entity finding per ADR
0003 (classify → fix losing side via TDD → lock via tightened schema and/or matrix entry →
record in `docs/cloud-parity/`). Tighten schemas **typed-but-open** (enumerate properties,
never `additionalProperties: false`). Reuse existing primitives for conditional delete
(`predicate.ParseCondition` + `SearchService.Search` + per-id `EntityStore.Delete`).

**Tech Stack:** Go 1.26+, `log/slog`, kin-openapi (getkin) v0.140.0 runtime response validation,
oapi-codegen v2.7.0 (types only; spec is `//go:embed`'d verbatim — **no regen**), testcontainers
Postgres (E2E), cross-backend parity harness (memory/sqlite/postgres + commercial).

Derived from the reviewed design doc
`docs/superpowers/specs/2026-07-02-openapi-contract-reconciliation-design.md` (findings E1–E9,
§6.1 delete, §6.3 HTTP↔gRPC, §7 error tables, §8 coverage matrix, §9 gate obligations). Runs
**after** Spec 1 (merged, `174fcf8`), which established the marker-aware coverage gate
(`markedOps`) this slice's error-code matrix layers on. Governed by ADR 0003.

## Global Constraints

- **Base branch:** `release/v0.8.2`; this worktree branch is `worktree-feat-openapi-entity-slice`
  at `174fcf8`. Never commit `replace` directives; SPI pin stays as-is (`v0.8.2-0.20260701194338-f0b2305c794b`).
- **Enforce mode is ON.** `openapivalidator.Mode == ModeEnforce` (compile-time). Any schema you
  tighten is validated against **every** E2E response for that operation in the same test run —
  a schema/behaviour mismatch fails CI. Schema-tightening + the server change that satisfies it
  must land in the **same commit** (never leave an interim red state).
- **Typed-but-open, always.** Enumerate every emitted property (correct type, required vs
  optional) but **never** `additionalProperties: false` and **never** `unevaluatedProperties`
  on entity schemas. `TestSpecHasNoSealedSchemas` (Spec 1) enforces no direct sealing. Genuinely
  open values (`Envelope.data`, `ProblemDetail.properties`) stay open by design.
- **No spec→server auto-sync.** Classify each finding's direction per ADR 0003 Decision 7.
- **No regen.** `api/spec.go` embeds `openapi.yaml` verbatim via `//go:embed`; `api/generated.go`
  is types + `ServerInterface` only and does **not** contain the spec. Hand-edit `openapi.yaml`.
  After any schema edit run `go build ./... && go vet ./...` — entity handlers read bodies from
  `r.Body`, so `ServerInterface` signatures do not change and `generated.go` needs no regen. If a
  build breaks, STOP and surface it (do not `go generate` — the Go 1.26 regen caveat is unverified).
- **errorCode location:** the ProblemDetail body carries the code at `properties.errorCode`
  (nested), written by `common.WriteError` as `pd.Props["errorCode"] = appErr.Code`
  (`internal/common/errors.go:234`). It is NOT a top-level field.
- **Go conventions (CLAUDE.md):** `log/slog` only; wrap errors `fmt.Errorf("...: %w", err)`;
  `uuid.UUID` not `string`; 4xx = full domain detail + code, 5xx = generic + ticket UUID.
- **No issue IDs** in any shipped artefact (code, comments, error strings, OpenAPI/help content).
- **TDD (Gate 1):** every task is RED → GREEN → REFACTOR. Commit per task.
- **Docs (Gate 4):** new error-code constant → `cmd/cyoda/help/content/errors/<CODE>.md`
  (`TestErrCode_Parity` bijection). Update CHANGELOG. Check COMPATIBILITY (no SPI/chart bump here).
- **Cloud-parity (Gate 7):** each reconciled behaviour → an entry in the existing
  `docs/cloud-parity/openapi-conformance.md`; two deferred decisions (E6, D2) → open questions there.

---

## Ground-truth code map (verified 2026-07-03)

Exact identifiers and line numbers the tasks below depend on. (Line numbers may drift as you
edit; the identifiers are stable — grep if a number is stale.)

**Validator (`internal/e2e/openapivalidator/`)**
- `collector.go`: `type collector struct { mu; mismatches []Mismatch; exercised map[string]struct{}; saw2xx map[string]struct{} }` (l.19-24); methods `append` (37), `recordExercised` (43), `recordStatus` (49, 2xx-only). Exported: `Success2xxSet()` (69), `DrainAndExercised()` (95). `type Mismatch struct { Operation; Method; Path; Status; JSONPath; Reason; TestName }` (7-15; `JSONPath` currently unused).
- `validator.go`: `func (v *Validator) Validate(ctx, req, resp) []Mismatch` (74-176); resolves `opId := route.Operation.OperationID` and calls `recordExercised(opId)` + `recordStatus(opId, resp.StatusCode)` (129-131); builds `openapi3filter.ResponseValidationInput{...Body: resp.Body...}` and calls `ValidateResponse` (162-174). **`resp.Body` is consumed here** — buffer it if you also parse errorCode.
- `middleware.go`: `NewMiddleware(v)` (81-130) tees the response, synthesizes `*http.Response` from `cs.captureBytes()`/`cs.captureStatus()` (102-107), calls `v.Validate` (108).
- Gate: `internal/e2e/zzz_openapi_conformance_test.go` — `uncoveredOps(all []string, exercised, success2xx map[string]bool, marked map[string]string) (unmarkedUncovered, staleMarkers []string)` (25-36); driver `TestOpenAPIConformanceReport` (41-87). Marker vars in `internal/e2e/e2e_test.go`: `allOperationIds []string`, `markedOps map[string]string` (33-40); `readCyodaStatus(op)` reads `x-cyoda-status` (42-53); populated in `TestMain` (182-192). Validator wired at `e2e_test.go:176`.

**Entity domain**
- `internal/domain/entity/handler.go`: `type Handler struct { factory spi.StoreFactory; txMgr; uuids; engine *wfengine.Engine; gate *txgate.Registry }` (85-95); `func New(factory, txMgr, uuids, engine, gate) *Handler` (93). `DeleteEntities` (592-611) → calls `h.DeleteAllEntities`, ignores body/pointInTime/verbose, hardcodes `numberOfEntitites == numberOfEntititesRemoved == TotalCount`. `GetAllEntities` (613-654) → ignores `params.PointInTime`, calls `h.ListEntities`. `GetEntityChangesMetadata` (569-590) → HTTP shim over `h.GetChangesMetadata`, emits `changeType` **verbatim** (no mapping), no `fieldsChangedCount`.
- `internal/domain/entity/service.go`: `type EntityEnvelope struct { Type string; Data any; Meta map[string]any }` (64-69). Meta built inline: `GetEntity` (433-446, **includes** `modelKey`), `ListEntities` (875-884, **omits** `modelKey`); neither emits `pointInTime` or `previousTransition`; both always emit `transactionId`, conditionally `transitionForLatestSave`. `DeleteAllEntities` (748-822): 404s on unregistered model (778-785), counts via `GetAll` (788-792), wipes via `entityStore.DeleteAll(txCtx, ref)` (800). `GetChangesMetadata` (689-745): truncates to ≤pointInTime, sorts **newest-first** (720-722), caps 1000; builds entries from `v.ChangeType/Timestamp/User/Entity!=nil/Entity.Meta.TransactionID`; no `fieldsChangedCount`.
- Reuse primitives: `predicate.ParseCondition(body []byte) (Condition, error)` (SPI `predicate` pkg); `match.Match(cond predicate.Condition, entityData []byte, entityMeta spi.EntityMeta) (bool, error)` (`internal/match/match.go:17`); `SearchService.Search(ctx, modelRef spi.ModelRef, cond predicate.Condition, opts SearchOptions) ([]*spi.Entity, error)` (`internal/domain/search/service.go:122`); ids off `e.Meta.ID`; honours `opts.PointInTime` via `GetAllAsAt` (161); **tx-pushdown bypass at 140-141** (`tx == nil` gate) ⇒ inside a tx it uses `GetAll`/`GetAllAsAt` + in-memory `match.Match` (tx-visible). `SearchOptions{ PointInTime *time.Time; Limit; Offset; ... }` (29-36).
- `EntityStore` SPI (`persistence.go`): per-id `Delete(ctx, entityID string) error` (37), `DeleteAll(ctx, modelRef) error` (38), `GetAllAsAt(ctx, modelRef, asAt time.Time) ([]*Entity, error)` (36). **No batch delete.**
- **Existing in-package precedent for condition-based selection with tx visibility:**
  `grouped_stats_service.go` already imports `search` + `match` + `predicate`, defines
  `var ErrInvalidCondition = errors.New("invalid condition")` (routed to 400 INVALID_CONDITION via
  `errors.Is`), and iterates entities applying `match.Match`. Follow this precedent.
- **entity → search import already exists** (`grouped_stats_service.go:14`) — no cycle risk.
- **Production wiring:** `app/app.go:535` — `entityHandler := entity.New(a.storeFactory, a.transactionManager, common.NewDefaultUUIDGenerator(), a.workflowEngine, a.txGate)`. `a.searchService *search.SearchService` already built at `app/app.go:409`. Other `entity.New` call site: `internal/grpc/rpc_test.go:42`.

**gRPC**
- `internal/grpc/search.go`: `buildEntityMeta(e *spi.Entity) map[string]any` (576-589) — **omits** `modelKey` + `pointInTime`. `mapChangeType(ct string) string` (591-605) — maps `CREATED→CREATE` etc.; result cast to `events.EntityChangeMetaJsonChangeType` and set on `events.EntityChangeMetaJson.ChangeType` (search.go:540).
- `internal/grpc/entity.go`: `EntityDeleteRequest` (173-201, unconditional, `req.EntityID` only), `EntityDeleteAllRequest` (391-423, unconditional). **No conditional delete (D2).**
- `internal/grpc/errors.go`: `buildErrorFields(err) (code, message string, retryable *bool)` (24-49) — operational → `code = "CLIENT_ERROR"`, `message = appErr.Message` (already `"CODE: detail"`); internal → `"SERVER_ERROR"` + ticket. **N1: the specific code lives in `Error.Message`, not `Error.Code`.** Envelope per RPC: `*ResponseJson{ Success bool; Error *...ResponseJsonError{ Code, Message, Retryable } }`.

**Error codes / help topics**
- `internal/common/error_codes.go`: EXIST (constant + `.md`): `ErrCodeModelNotFound`, `ErrCodeEntityNotFound`, `ErrCodeUniqueViolation`, `ErrCodeInvalidUniqueKey`, `ErrCodeInvalidUniqueKeyDefinition`, `ErrCodeIncompatibleType`, `ErrCodePolymorphicSlot`.
- **NET-NEW** (currently raw string literals only, no constant, no `.md`): `MALFORMED_REQUEST`, `INVALID_CONDITION` — used inline in `grouped_stats_handler.go:79,94,116,167` (backed by `grouped_stats_service.go` `ErrInvalidCondition`).
- `TestErrCode_Parity` (`cmd/cyoda/help/help_test.go:542-590`): regex `ErrCode[A-Z][A-Za-z0-9]+\s*=\s*"([A-Z0-9_]+)"` → strict bijection between `ErrCode*` constants and `cmd/cyoda/help/content/errors/<CODE>.md`. Raw literals are invisible to it; promoting a code to a constant **requires** the matching `.md`.
- `common.WriteError(w, r, appErr)` (`internal/common/errors.go:173`); `common.Operational(status, code, message)` sets `Code` and `Message = "CODE: detail"` (81-88); `common.Internal` pre-maps SPI sentinels (`spi.ErrUniqueViolation`→409 `UNIQUE_VIOLATION`, `spi.ErrPartialUniqueKey`→422 `INVALID_UNIQUE_KEY`).

**OpenAPI (`api/openapi.yaml`)**
- `Envelope` (7951-7976): `meta` is an opaque `type: object` + `additionalProperties: true` bag; description names `previousTransition`. `previousTransition` also in `getAllEntities` examples (1731, 1756) and search prose (6207, out of entity scope). `transitionForLatestSave` appears nowhere.
- `EntityChangeMeta` component (10149-10172): enum `CREATED/UPDATED/DELETED`, `fieldsChangedCount: int32`. `getEntityChangesMetadata` example (1490-1500) uses `CREATED/UPDATED`.
- `StreamDeleteResult` (10995-11010): already has `entityModelClassId`, `ids` (array uuid), `deleteResult → $ref StreamDataDeleteResult` — **no response-schema change needed for E2**.
- `getOneEntity` (1248, 200 example shows only `{id,state}`), `getAllEntities` (1651), `deleteEntities` (1807, body `$ref AbstractConditionDto`), `create` (2994, body `type: object`), `createCollection` (2177, body `type: object`), `updateCollection` (1968), update/patch (2346/2514/2660/2838), `getEntityChangesMetadata` (1454).
- `x-cyoda-status` marker syntax: top-level key on the operation object, sibling to `operationId` (e.g. `accountSubscriptionsGet` at 342-352). No `additionalProperties: false` or `unevaluatedProperties` anywhere (both counts = 0).
- Canonical schemas (source of truth, **do not** author a 4th copy): `docs/cyoda/schema/common/EntityMetadata.json` (required `{id,state,creationDate,lastUpdateTime}`, optional `{modelKey,pointInTime,transitionForLatestSave,transactionId}`); `docs/cyoda/schema/common/EntityChangeMeta.json` (enum `CREATE/UPDATE/DELETE`, has `fieldsChangedCount`). Parity mirror: `e2e/parity/client/types.go` — `EntityMetadata` (37-46) already matches canonical (`TransitionForLatestSave`, `PointInTime *time.Time`, `ModelKey *ModelSpec`); `EntityChangeMeta.ChangeType` is plain `string` (comment already says `CREATE/UPDATE/DELETE`).

**Test harness patterns**
- E2E: `internal/e2e/entity_conformance_test.go` (header comment l.6 "server-is-source-of-truth" → fix per §2); helpers `doAuth(t, method, path, body)`, `readBody`, `createEntityE2E` (`entity_test.go:12`), `importModel`, `setupModelWithWorkflow`, `getEntityData(t,id,pit)`, `getEntityState`; every request via `e2eNewRequest` attaches `*testing.T` so the enforce hook fails that test. `TestDeleteEntities_ResponseShape` (24-77). Guard each with `if testing.Short() { t.Skip(...) }`.
- Parity: register one `NamedTest{Name, Fn}` in `e2e/parity/registry.go` `allTests` + a `Run*(t *testing.T, fixture BackendFixture)` in a topical `e2e/parity/*.go`. Client `e2e/parity/client/http.go`: `CreateEntity`, `CreateEntityRaw`, `CreateEntityWithTxID`, `GetEntity`, `GetEntityAt`, `ListEntitiesByModel` (**no as-at variant yet — add one**), `GetEntityChanges`/`GetEntityChangesAt`, `DeleteEntity`, `DeleteEntityRaw`, `SetUniqueKeysRaw`, `SyncSearchAt`. Unique-key idiom: `ukAssertErrCode(t, raw, "CODE")` + `ukCapabilityGateOrSkip` (`e2e/parity/unique_keys.go`). **Tx-scoped parity** uses the compute-callback-join pattern (`e2e/parity/callback_txjoin.go`, `RunCallbackCriteriaReadYourWrites` 247-288) with `fixture.ComputeTenant(t)` — the only way to run a read/delete *inside* an uncommitted tx over the HTTP boundary.
- gRPC: `internal/grpc/rpc_test.go` — `newTestEnv(t) (*CloudEventsServiceImpl, context.Context)` (27-57), `importAndLockModel`, `makeCE(eventType, fields)`, `validateResponse(t, ce, target)`; `TestRPC_EntityCreate` (125-158), `TestRPC_EntityDelete` (160-207), `TestRPC_EntityCreate_UniqueViolation` (651-703, asserts `Error.Code=="CLIENT_ERROR"` + `strings.Contains(Error.Message,"UNIQUE_VIOLATION")`).

---

## File Structure

**New files**
- `internal/e2e/openapivalidator/errorcode.go` — errorCode extraction + collector triple store + `ObservedErrorTriples()` readout.
- `internal/e2e/openapivalidator/errorcode_test.go` — unit tests for extraction + recording.
- `internal/e2e/zzz_errorcode_matrix_test.go` — `EntityErrorCodeMatrix` + `producible`/`declared` suite-end checks (the `zzz_` prefix makes it run after all endpoint tests have recorded their triples).
- `internal/e2e/errorcode_matrix_logic_test.go` — unit tests for the `producible`/`declared` check funcs (synthetic data, `-short`-safe).
- `e2e/parity/entity_slice.go` — new parity scenarios (meta shape, getAll as-at, request-body, conditional-delete-in-tx, gRPC-delete-unconditional coverage lives in grpc test not here).
- `cmd/cyoda/help/content/errors/INVALID_CONDITION.md`.

**Error-code reconciliation decision (bounded, per ADR 0003).** Only `INVALID_CONDITION` is
genuinely new for this slice (E2's conditional delete; it also promotes `grouped_stats`'
`ErrInvalidCondition` sentinel to a first-class documented code). The design §7 tables label
some 400s `MALFORMED_REQUEST`, but the running entity handlers already emit the **existing**
`ErrCodeBadRequest` ("BAD_REQUEST") for malformed bodies (`handler.go:434,366,375`), while
`grouped_stats` separately emits a raw `"MALFORMED_REQUEST"` literal. Rather than churn an
emitted code, the matrix + OpenAPI error tables document the **codes actually emitted**
(`BAD_REQUEST`), and the `BAD_REQUEST`↔`MALFORMED_REQUEST` naming unification (spanning create,
collection, patch, and the stats/search + model slices) is recorded as a **follow-on open
question** in `docs/cloud-parity/openapi-conformance.md` — it would balloon this slice.

**Modified files**
- `api/openapi.yaml` — `EntityMetadata` schema; `Envelope.meta` repoint; drop `previousTransition`; `getOneEntity`/`getAllEntities` examples; `deleteEntities`/`create`/`createCollection` request bodies + error tables; `EntityChangeMeta` enum; `getEntityChangesMetadata` ordering prose + example; unique-key codes.
- `internal/common/error_codes.go` — add `ErrCodeInvalidCondition`.
- `internal/domain/entity/grouped_stats_handler.go` / `grouped_stats_service.go` — use the new `ErrCodeInvalidCondition` constant in place of the raw `"INVALID_CONDITION"` literal (Gate 6).
- `internal/domain/entity/service.go` — conditional delete method; `pointInTime` in `ListEntities`; `meta.pointInTime`; changeType mapping on HTTP.
- `internal/domain/entity/handler.go` — `New` gains `*search.SearchService`; `DeleteEntities`/`GetAllEntities`/`GetEntityChangesMetadata` rewires; fix l.601 comment.
- `internal/grpc/search.go` — `buildEntityMeta` adds `modelKey` (+ `pointInTime` param).
- `app/app.go:535`, `internal/grpc/rpc_test.go:42` — pass the search service to `entity.New`.
- `internal/e2e/openapivalidator/validator.go` — buffer body + record errorCode triple.
- `internal/e2e/entity_conformance_test.go` — thin redundant assertions; fix header comment.
- `e2e/parity/client/http.go` + `e2e/parity/client/types.go` — `ListEntitiesByModelAt`; reconcile mirrors if a wire shape changes.
- `e2e/parity/registry.go` — register new scenarios.
- `docs/cloud-parity/openapi-conformance.md` — reconciliation entries + 2 open questions.
- `CHANGELOG.md`.

---

## Phase 1 — Pillar A: error-code coverage matrix

### Task 1: Collector records `(operationId, status, errorCode)` triples

**Files:**
- Create: `internal/e2e/openapivalidator/errorcode.go`
- Create: `internal/e2e/openapivalidator/errorcode_test.go`
- Modify: `internal/e2e/openapivalidator/collector.go` (add field + init + method + readout)
- Modify: `internal/e2e/openapivalidator/validator.go:129-131` (buffer error body, record triple; add `bytes` import)

**Interfaces:**
- Produces: `type ErrorTriple struct { Operation string; Status int; ErrorCode string }`;
  `func (c *collector) recordErrorCode(op string, status int, code string)`;
  `func ObservedErrorTriples() []ErrorTriple`; `func extractErrorCode(body []byte) string`.

- [ ] **Step 1: Write the failing test** — `internal/e2e/openapivalidator/errorcode_test.go`:

```go
package openapivalidator

import "testing"

func TestExtractErrorCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"nested errorCode", `{"type":"about:blank","status":400,"properties":{"errorCode":"INVALID_CONDITION"}}`, "INVALID_CONDITION"},
		{"no properties", `{"type":"about:blank","status":404}`, ""},
		{"properties without errorCode", `{"properties":{"retryable":true}}`, ""},
		{"empty body", ``, ""},
		{"not json", `<html>nope</html>`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractErrorCode([]byte(tc.body)); got != tc.want {
				t.Errorf("extractErrorCode(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestRecordAndReadErrorTriples(t *testing.T) {
	c := newCollector()
	c.recordErrorCode("deleteEntities", 400, "INVALID_CONDITION")
	c.recordErrorCode("deleteEntities", 400, "INVALID_CONDITION") // dedup
	c.recordErrorCode("createEntity", 409, "UNIQUE_VIOLATION")
	got := c.observedErrorTriples()
	if len(got) != 2 {
		t.Fatalf("expected 2 distinct triples, got %d: %+v", len(got), got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/e2e/openapivalidator/ -run 'TestExtractErrorCode|TestRecordAndReadErrorTriples' -v`
Expected: FAIL — `undefined: extractErrorCode`, `undefined: observedErrorTriples`.

- [ ] **Step 3: Create `internal/e2e/openapivalidator/errorcode.go`:**

```go
package openapivalidator

import "encoding/json"

// ErrorTriple identifies one observed (operationId, status, errorCode)
// combination. ErrorCode is the value at properties.errorCode of a
// ProblemDetail body; empty for success responses or bodies without it.
type ErrorTriple struct {
	Operation string
	Status    int
	ErrorCode string
}

// extractErrorCode reads properties.errorCode from a ProblemDetail JSON body.
// Returns "" when the body is not JSON, has no properties object, or carries no
// errorCode string. Mirrors internal/common.ProblemDetail, where the code is
// nested under "properties" (see internal/common/errors.go WriteError).
func extractErrorCode(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(body, &pd); err != nil {
		return ""
	}
	if code, ok := pd.Properties["errorCode"].(string); ok {
		return code
	}
	return ""
}

func (c *collector) recordErrorCode(operationID string, status int, code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errorTriples[ErrorTriple{Operation: operationID, Status: status, ErrorCode: code}] = struct{}{}
}

func (c *collector) observedErrorTriples() []ErrorTriple {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ErrorTriple, 0, len(c.errorTriples))
	for tr := range c.errorTriples {
		out = append(out, tr)
	}
	return out
}

// ObservedErrorTriples returns the (operationId, status, errorCode) triples
// observed across the run. Read at suite end by the error-code matrix test.
func ObservedErrorTriples() []ErrorTriple { return defaultCollector.observedErrorTriples() }
```

- [ ] **Step 4: Add the field + init to `internal/e2e/openapivalidator/collector.go`:**

In the `collector` struct (l.19-24) add `errorTriples map[ErrorTriple]struct{}`:

```go
type collector struct {
	mu           sync.Mutex
	mismatches   []Mismatch
	exercised    map[string]struct{}
	saw2xx       map[string]struct{}
	errorTriples map[ErrorTriple]struct{}
}
```

In `newCollector()` (l.30-35) add the init:

```go
func newCollector() *collector {
	return &collector{
		exercised:    make(map[string]struct{}),
		saw2xx:       make(map[string]struct{}),
		errorTriples: make(map[ErrorTriple]struct{}),
	}
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/e2e/openapivalidator/ -run 'TestExtractErrorCode|TestRecordAndReadErrorTriples' -v`
Expected: PASS.

- [ ] **Step 6: Wire extraction into `Validate`** — `internal/e2e/openapivalidator/validator.go`, immediately after `defaultCollector.recordStatus(opId, resp.StatusCode)` (l.131), insert:

```go
	// Error-code coverage (Pillar A): for error responses, record the
	// (operationId, status, errorCode) triple from the ProblemDetail body
	// (properties.errorCode). Buffer the body so ValidateResponse still sees
	// it. Only >=400 — success and streaming bodies carry no errorCode.
	if resp.StatusCode >= 400 && resp.Body != nil {
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		if code := extractErrorCode(bodyBytes); code != "" {
			defaultCollector.recordErrorCode(opId, resp.StatusCode, code)
		}
	}
```

Add `"bytes"` to the import block if not already present (`io` is already imported).

- [ ] **Step 7: Verify the package still builds and existing validator tests pass**

Run: `go test ./internal/e2e/openapivalidator/ -v`
Expected: PASS (all, including the two new tests). Then `go vet ./internal/e2e/openapivalidator/`.

- [ ] **Step 8: Commit**

```bash
git add internal/e2e/openapivalidator/
git commit -m "test(openapi): record (operationId,status,errorCode) triples in the conformance collector"
```

---

### Task 2: `EntityErrorCodeMatrix` + producible/declared suite-end checks

The matrix declares, per in-scope entity operationId, the `(status, errorCode)` combinations the
server is expected to produce. Two checks run at suite end: **producible** (every declared cell
was observed — catches fictional codes) and **declared** (every observed cell for a matrix op is
declared — catches undocumented codes). Seed with ONE fully-pinned row plus its producing test;
later E-tasks extend the matrix as they add codes. This is the reconciliation loop's lock step.

**Files:**
- Create: `internal/e2e/zzz_errorcode_matrix_test.go` (matrix data + suite-end checks + one producing e2e test)
- Create: `internal/e2e/errorcode_matrix_logic_test.go` (pure unit tests, `-short`-safe)

**Interfaces:**
- Produces: `var EntityErrorCodeMatrix map[string][]codeCell` where
  `type codeCell struct { Status int; Code string }`;
  `func producibleGaps(matrix map[string][]codeCell, observed []openapivalidator.ErrorTriple) []string`;
  `func declaredGaps(matrix map[string][]codeCell, observed []openapivalidator.ErrorTriple) []string`.
  Later tasks add rows to `EntityErrorCodeMatrix`.

- [ ] **Step 1: Write the failing unit test** — `internal/e2e/errorcode_matrix_logic_test.go`:

```go
package e2e_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
)

func TestProducibleGaps(t *testing.T) {
	m := map[string][]codeCell{
		"getOneEntity": {{404, "ENTITY_NOT_FOUND"}, {400, "BAD_REQUEST"}},
	}
	observed := []openapivalidator.ErrorTriple{
		{Operation: "getOneEntity", Status: 404, ErrorCode: "ENTITY_NOT_FOUND"},
	}
	gaps := producibleGaps(m, observed)
	if len(gaps) != 1 { // 400/BAD_REQUEST declared but never observed
		t.Fatalf("expected 1 producible gap, got %d: %v", len(gaps), gaps)
	}
}

func TestDeclaredGaps(t *testing.T) {
	m := map[string][]codeCell{
		"getOneEntity": {{404, "ENTITY_NOT_FOUND"}},
	}
	observed := []openapivalidator.ErrorTriple{
		{Operation: "getOneEntity", Status: 404, ErrorCode: "ENTITY_NOT_FOUND"}, // declared → OK
		{Operation: "getOneEntity", Status: 400, ErrorCode: "BAD_REQUEST"},      // undeclared → gap
		{Operation: "searchEntities", Status: 400, ErrorCode: "WHATEVER"},        // op not in matrix → ignored
	}
	gaps := declaredGaps(m, observed)
	if len(gaps) != 1 {
		t.Fatalf("expected 1 declared gap, got %d: %v", len(gaps), gaps)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/e2e/ -short -run 'TestProducibleGaps|TestDeclaredGaps' -v`
Expected: FAIL — `undefined: codeCell`, `undefined: producibleGaps`, `undefined: declaredGaps`.

- [ ] **Step 3: Create `internal/e2e/zzz_errorcode_matrix_test.go`:**

```go
package e2e_test

import (
	"flag"
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
)

// codeCell is one documented (status, errorCode) combination for an operation.
type codeCell struct {
	Status int
	Code   string
}

// EntityErrorCodeMatrix declares, per in-scope entity operationId, the
// (status, errorCode) combinations the spec's per-endpoint error tables
// promise (design §7). The suite-end checks assert bidirectional agreement
// with what the run actually produced. Out-of-scope operationIds are absent
// and therefore exempt — the marker-aware coverage gate governs their
// coverage. Rows are added by later tasks as each endpoint gains codes.
var EntityErrorCodeMatrix = map[string][]codeCell{
	// Seeded row: getOneEntity's error surface, pinned by
	// TestErrCodeMatrix_GetOneEntityNotFound below and existing lifecycle tests.
	"getOneEntity": {
		{Status: 404, Code: "ENTITY_NOT_FOUND"},
		{Status: 400, Code: "BAD_REQUEST"}, // conflicting pointInTime+transactionId
	},
}

func hasTriple(observed []openapivalidator.ErrorTriple, op string, c codeCell) bool {
	for _, tr := range observed {
		if tr.Operation == op && tr.Status == c.Status && tr.ErrorCode == c.Code {
			return true
		}
	}
	return false
}

// producibleGaps returns "op status code" strings for every declared cell that
// was never observed (fictional / unexercised documented codes).
func producibleGaps(matrix map[string][]codeCell, observed []openapivalidator.ErrorTriple) []string {
	var gaps []string
	for op, cells := range matrix {
		for _, c := range cells {
			if !hasTriple(observed, op, c) {
				gaps = append(gaps, fmt.Sprintf("%s %d %s", op, c.Status, c.Code))
			}
		}
	}
	sort.Strings(gaps)
	return gaps
}

// declaredGaps returns "op status code" strings for every observed error triple
// whose operation is IN the matrix but whose (status, code) is undocumented.
func declaredGaps(matrix map[string][]codeCell, observed []openapivalidator.ErrorTriple) []string {
	var gaps []string
	for _, tr := range observed {
		cells, inScope := matrix[tr.Operation]
		if !inScope {
			continue // out-of-scope op — exempt
		}
		found := false
		for _, c := range cells {
			if c.Status == tr.Status && c.Code == tr.ErrorCode {
				found = true
				break
			}
		}
		if !found {
			gaps = append(gaps, fmt.Sprintf("%s %d %s", tr.Operation, tr.Status, tr.ErrorCode))
		}
	}
	sort.Strings(gaps)
	return gaps
}

// TestZZZErrorCodeMatrix runs at suite end (zzz_ prefix orders it last, after
// all endpoint tests have recorded their error triples) and asserts the
// entity-scope error-code matrix is neither over- nor under-declared.
func TestZZZErrorCodeMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires the full running-backend suite")
	}
	// Bail under -shuffle: the zzz_ ordering that guarantees all endpoint tests
	// have recorded their triples first does not hold when execution is
	// shuffled (same idiom as the sibling TestOpenAPIConformanceReport guard).
	if v := flag.Lookup("test.shuffle"); v != nil && v.Value.String() != "off" {
		t.Skip("error-code matrix depends on suite ordering; skipped under -shuffle")
	}
	observed := openapivalidator.ObservedErrorTriples()
	if gaps := producibleGaps(EntityErrorCodeMatrix, observed); len(gaps) > 0 {
		t.Errorf("documented error codes never produced by any E2E (fictional?): %v", gaps)
	}
	if gaps := declaredGaps(EntityErrorCodeMatrix, observed); len(gaps) > 0 {
		t.Errorf("error codes produced but undocumented in EntityErrorCodeMatrix (add the cell + its §7 table entry): %v", gaps)
	}
}

// TestErrCodeMatrix_GetOneEntity makes both seeded getOneEntity cells producible:
// 404 ENTITY_NOT_FOUND (unknown id) and 400 BAD_REQUEST (conflicting
// pointInTime+transactionId). ExpectErrorCode re-buffers the body, so it is
// called on the live resp (no readBody first).
func TestErrCodeMatrix_GetOneEntity(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	// 404 ENTITY_NOT_FOUND — random unknown id.
	nf := doAuth(t, http.MethodGet, "/api/entity/"+uuid.NewString(), "")
	if nf.StatusCode != http.StatusNotFound {
		t.Fatalf("getOneEntity unknown id: expected 404, got %d", nf.StatusCode)
	}
	commontest.ExpectErrorCode(t, nf, "ENTITY_NOT_FOUND")

	// 400 BAD_REQUEST — pointInTime and transactionId are mutually exclusive
	// (handler.go:434, common.ErrCodeBadRequest).
	id := uuid.NewString()
	pit := "2035-01-01T12:00:00Z"
	tx := uuid.NewString()
	br := doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s?pointInTime=%s&transactionId=%s", id, pit, tx), "")
	if br.StatusCode != http.StatusBadRequest {
		t.Fatalf("getOneEntity conflicting params: expected 400, got %d", br.StatusCode)
	}
	commontest.ExpectErrorCode(t, br, "BAD_REQUEST")
}
```

(`fmt`, `net/http`, `github.com/google/uuid`, and `.../internal/common/commontest` are already in
this file's import block from Step 3. Verify the conflicting-params handler returns 400 *before*
the 404 not-found check — if it validates existence first, use a real entity id created in the
test so the 400 fires. Confirm the emitted code string is `BAD_REQUEST` (`common.ErrCodeBadRequest`);
if it differs, update the seeded cell to match reality.)

- [ ] **Step 4: Create the pure unit-test file** — already written in Step 1 (`errorcode_matrix_logic_test.go`). No production code besides Step 3.

- [ ] **Step 5: Run the unit tests to verify they pass**

Run: `go test ./internal/e2e/ -short -run 'TestProducibleGaps|TestDeclaredGaps' -v`
Expected: PASS.

- [ ] **Step 6: Run the full e2e suite to verify the matrix is green against reality**

Run: `go test ./internal/e2e/ -run 'TestZZZErrorCodeMatrix|TestErrCodeMatrix_GetOneEntity|TestEntityLifecycle' -v` (Docker required)
Expected: PASS — both seeded getOneEntity cells are now produced by `TestErrCodeMatrix_GetOneEntity`.
If `TestZZZErrorCodeMatrix` reports a **declared** gap for `getOneEntity` (an observed code not in
the seed row), that code is real — add its cell to the row (and later its §7 OpenAPI table entry).
A **producible** gap means a seeded cell is not emitted with that exact `(status, code)` — fix the
producing test or correct the cell to the emitted code (per §2, the server is emitting it).

- [ ] **Step 7: `go vet` + commit**

```bash
go vet ./internal/e2e/
git add internal/e2e/zzz_errorcode_matrix_test.go internal/e2e/errorcode_matrix_logic_test.go
git commit -m "test(openapi): entity error-code coverage matrix (producible + declared checks)"
```

---

## Phase 2 — E1/E9 typed-but-open `meta` (HTTP) + §6.3 gRPC `meta` reconcile

### Task 3: `EntityMetadata` schema, `Envelope.meta` typed-but-open, drop `previousTransition`, enrich examples (E1 + E9)

Only `getOneEntity` (openapi.yaml:1283) and `getAllEntities` (1705) reference `#/components/schemas/Envelope`; both HTTP handlers emit `id/state/creationDate/lastUpdateTime` (+ optional `transactionId`, `transitionForLatestSave`; `modelKey` on getOne only). HTTP search uses a different schema — untouched. So tightening `Envelope.meta` is safe under enforce mode.

**Files:**
- Create: `api/openapi_entity_meta_test.go` (structural RED test, package `api`)
- Modify: `api/openapi.yaml` — add `EntityMetadata` component; repoint `Envelope.meta`; drop `previousTransition` from `Envelope.meta` description + `getAllEntities` examples (1731, 1756); enrich `getOneEntity` 200 example (E9).

**Interfaces:**
- Produces: component schema `EntityMetadata` mirroring `docs/cyoda/schema/common/EntityMetadata.json`; `Envelope.meta` = `$ref EntityMetadata`.

- [ ] **Step 1: Write the failing structural test** — `api/openapi_entity_meta_test.go`:

```go
package api

import "testing"

// TestEnvelopeMetaTypedButOpen asserts Envelope.meta is a typed-but-open schema
// mirroring the canonical EntityMetadata.json: required id/state/creationDate/
// lastUpdateTime, optional modelKey/pointInTime/transitionForLatestSave/
// transactionId, never sealed, and no previousTransition fossil.
func TestEnvelopeMetaTypedButOpen(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	env := doc.Components.Schemas["Envelope"]
	if env == nil || env.Value == nil {
		t.Fatal("Envelope schema missing")
	}
	metaRef := env.Value.Properties["meta"]
	if metaRef == nil || metaRef.Value == nil {
		t.Fatal("Envelope.meta missing or unresolved")
	}
	meta := metaRef.Value

	req := map[string]bool{}
	for _, r := range meta.Required {
		req[r] = true
	}
	for _, want := range []string{"id", "state", "creationDate", "lastUpdateTime"} {
		if !req[want] {
			t.Errorf("meta.required missing %q", want)
		}
	}
	for _, want := range []string{"id", "state", "creationDate", "lastUpdateTime", "modelKey", "pointInTime", "transitionForLatestSave", "transactionId"} {
		if meta.Properties[want] == nil {
			t.Errorf("meta.properties missing %q", want)
		}
	}
	if meta.AdditionalProperties.Has != nil && !*meta.AdditionalProperties.Has {
		t.Error("meta must be typed-but-open, never additionalProperties:false")
	}
	if meta.Properties["previousTransition"] != nil {
		t.Error("previousTransition fossil must be removed from meta")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./api/ -run TestEnvelopeMetaTypedButOpen -v`
Expected: FAIL — `meta.properties missing "id"` etc. (meta is currently an opaque `additionalProperties:true` bag).

- [ ] **Step 3: Add the `EntityMetadata` component to `api/openapi.yaml`** (in `components.schemas`, alphabetically near `EntityChangeMeta`):

```yaml
    EntityMetadata:
      type: object
      description: |
        System-managed entity metadata. Mirrors the canonical
        docs/cyoda/schema/common/EntityMetadata.json. Typed-but-open: known
        fields are enumerated and validated; the object is not sealed, so
        additive fields remain non-breaking.
      required:
        - id
        - state
        - creationDate
        - lastUpdateTime
      properties:
        id:
          type: string
          format: uuid
          description: Entity id (invariant against point-in-time).
        modelKey:
          type: object
          description: Model of the entity. Present on single-entity reads.
          properties:
            name:
              type: string
            version:
              type: integer
              format: int32
        state:
          type: string
          description: Entity state at the given point-in-time.
        creationDate:
          type: string
          format: date-time
        lastUpdateTime:
          type: string
          format: date-time
          description: Last update as-at the point-in-time; equals creationDate if never updated.
        pointInTime:
          type: string
          format: date-time
          description: The as-at point-in-time for which the entity was retrieved, when supplied.
        transitionForLatestSave:
          type: string
          description: Transition applied when the entity was last saved as-at the point-in-time.
        transactionId:
          type: string
          format: uuid
```

- [ ] **Step 4: Repoint `Envelope.meta`** — in the `Envelope` schema (openapi.yaml ~7951-7976) replace the opaque `meta` bag:

```yaml
        meta:
          type: object
          additionalProperties: true
          description: |
            System-managed metadata: id, state, creationDate, previousTransition, etc.
```

with:

```yaml
        meta:
          $ref: "#/components/schemas/EntityMetadata"
```

- [ ] **Step 5: Remove `previousTransition` from `getAllEntities` examples** — grep `previousTransition` in `api/openapi.yaml`; delete the two example lines inside `getAllEntities` (formerly ~1731, 1756). Leave the search-prose occurrence (searchEntities description) — that is out of the entity slice's scope (a follow-on group owns it).

- [ ] **Step 6: Enrich the `getOneEntity` 200 example (E9)** — in the `getOneEntity` 200 response, replace the `{id,state}`-only `meta` example with the full shape:

```yaml
                    meta:
                      id: 3fa85f64-5717-4562-b3fc-2c963f66afa6
                      modelKey:
                        name: Order
                        version: 1
                      state: CREATED
                      creationDate: "2026-07-01T12:00:00.000Z"
                      lastUpdateTime: "2026-07-01T12:00:00.000Z"
                      transactionId: 9f1b0c2d-1111-2222-3333-444455556666
```

- [ ] **Step 7: Run the structural test to verify it passes**

Run: `go test ./api/ -run TestEnvelopeMetaTypedButOpen -v`
Expected: PASS.

- [ ] **Step 8: Verify build + enforce-mode conformance on the real endpoints**

Run: `go build ./... && go vet ./...`
Then (Docker required): `go test ./internal/e2e/ -run 'TestGetOneEntity_ReturnsEnvelope|TestGetAllEntities_ReturnsJSONArray|TestEntityLifecycle' -v`
Expected: PASS — the enforce-mode validator now binds the tightened `meta`, and the emitted responses conform. If any fails with an `openapi conformance` error naming a missing/mistyped meta field, the schema and the emitted meta disagree — fix the mismatch (adjust the schema to reality per §2, since the server is emitting it) before proceeding.

- [ ] **Step 9: Run `TestSpecHasNoSealedSchemas`** (Spec 1 guard)

Run: `go test ./internal/e2e/ -run TestSpecHasNoSealedSchemas -v`
Expected: PASS (EntityMetadata sets no `additionalProperties:false`).

- [ ] **Step 10: Commit**

```bash
git add api/openapi.yaml api/openapi_entity_meta_test.go
git commit -m "feat(openapi): typed-but-open Envelope.meta mirroring canonical EntityMetadata; drop previousTransition fossil; enrich getOneEntity example (E1/E9)"
```

---

### Task 4: gRPC `buildEntityMeta` adds `modelKey` (+ `pointInTime`) — §6.3 meta parity

HTTP `getOne` meta includes `modelKey`; gRPC `buildEntityMeta` omits it. Reconcile so both entry points emit the same metadata shape.

**Files:**
- Modify: `internal/grpc/search.go:577-589` (`buildEntityMeta`), call sites `375`, `662`.
- Modify/Create test: `internal/grpc/rpc_test.go` (or a new `internal/grpc/entity_meta_test.go`) — assert gRPC entity-get meta includes `modelKey{name,version}`.

**Interfaces:**
- Consumes: `spi.Entity.Meta.ModelRef{EntityName, ModelVersion string}`, `spi.Entity.Meta.Version int64`.
- Produces: `buildEntityMeta(e *spi.Entity, pointInTime *time.Time) map[string]any`.

- [ ] **Step 1: Write the failing test** — add to `internal/grpc/rpc_test.go`:

```go
func TestRPC_EntityGet_MetaIncludesModelKey(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice", "age": 30})

	ceCreate := makeCE(EntityCreateRequest, map[string]any{
		"id":         "test",
		"dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": "Alice", "age": 30},
		},
	})
	respCreate, err := svc.EntityManage(ctx, ceCreate)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	entityID := parseResponsePayload(t, respCreate).TransactionInfo.EntityIds[0]

	ceGet := makeCE(EntityGetRequest, map[string]any{"id": "test", "entityId": entityID})
	respGet, err := svc.EntityManage(ctx, ceGet)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	meta := parseEntityResponseMeta(t, respGet) // helper: unmarshals payload.meta into map[string]any
	mk, ok := meta["modelKey"].(map[string]any)
	if !ok {
		t.Fatalf("gRPC entity meta missing modelKey; got meta=%v", meta)
	}
	if mk["name"] != "person" {
		t.Errorf("modelKey.name = %v, want person", mk["name"])
	}
}
```

Use the existing `EntityGetRequest` event constant and `parseResponsePayload` helper; add a small
`parseEntityResponseMeta(t, ce)` helper that unmarshals the response `TextData` into
`events.EntityResponseJson` and returns `Payload.Meta` as `map[string]any` (mirror
`validateResponse`). If the exact create→get event names differ, follow `TestRPC_EntityDelete`
(rpc_test.go:160-207), which already does create-then-fetch-id.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/grpc/ -run TestRPC_EntityGet_MetaIncludesModelKey -v`
Expected: FAIL — `gRPC entity meta missing modelKey`.

- [ ] **Step 3: Update `buildEntityMeta`** — `internal/grpc/search.go:577`:

```go
// buildEntityMeta builds the meta map for an entity response. Mirrors the HTTP
// meta shape (design §6.3): includes modelKey, and pointInTime when the read
// was as-at a supplied point-in-time.
func buildEntityMeta(e *spi.Entity, pointInTime *time.Time) map[string]any {
	meta := map[string]any{
		"id":             e.Meta.ID,
		"modelKey":       map[string]any{"name": e.Meta.ModelRef.EntityName, "version": e.Meta.Version},
		"state":          e.Meta.State,
		"creationDate":   e.Meta.CreationDate.UTC().Format(time.RFC3339Nano),
		"lastUpdateTime": e.Meta.LastModifiedDate.UTC().Format(time.RFC3339Nano),
		"transactionId":  e.Meta.TransactionID,
	}
	if e.Meta.TransitionForLatestSave != "" {
		meta["transitionForLatestSave"] = e.Meta.TransitionForLatestSave
	}
	if pointInTime != nil {
		meta["pointInTime"] = pointInTime.UTC().Format(time.RFC3339Nano)
	}
	return meta
}
```

- [ ] **Step 4: Update both call sites** — `internal/grpc/search.go:375` and `662`. Change `buildEntityMeta(e)` → `buildEntityMeta(e, nil)` (these entity-get/response paths carry no as-at point-in-time; pass `nil`). If either handler has a request `pointInTime` in scope, thread it instead of `nil`.

- [ ] **Step 5: Run the test + package tests**

Run: `go test ./internal/grpc/ -run TestRPC_EntityGet_MetaIncludesModelKey -v`
Expected: PASS. Then `go test ./internal/grpc/ -v` and `go vet ./internal/grpc/`.

- [ ] **Step 6: Commit**

```bash
git add internal/grpc/search.go internal/grpc/rpc_test.go
git commit -m "feat(grpc): entity meta includes modelKey (+ pointInTime) — HTTP/gRPC meta parity (§6.3)"
```

---

### Task 5: cross-backend parity — entity `meta` shape (backend-agnostic)

**Files:**
- Create: `e2e/parity/entity_slice.go` (new topical file for this slice's scenarios)
- Modify: `e2e/parity/registry.go` — register the scenario.

**Interfaces:**
- Consumes: `client.Client` (`GetEntity`, `CreateEntity`), `client.EntityMetadata` (already mirrors canonical: `ID/State/CreationDate/LastUpdateTime` + optional `ModelKey/PointInTime/TransitionForLatestSave/TransactionID`).
- Produces: `func RunEntityMetaShape(t *testing.T, fixture BackendFixture)`.

- [ ] **Step 1: Write the failing scenario** — `e2e/parity/entity_slice.go`:

```go
package parity

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunEntityMetaShape asserts the single-entity GET meta carries the canonical
// typed fields on every backend (E1): id/state/creationDate/lastUpdateTime
// always, modelKey on the single-entity read.
func RunEntityMetaShape(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "entity-meta-shape"
	const modelVersion = 1
	setupSimpleWorkflow(t, c, modelName, modelVersion)

	id, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":30,"status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.ID != id.String() {
		t.Errorf("meta.id = %q, want %q", got.Meta.ID, id)
	}
	if got.Meta.State == "" {
		t.Error("meta.state empty")
	}
	if got.Meta.CreationDate.IsZero() {
		t.Error("meta.creationDate zero")
	}
	if got.Meta.LastUpdateTime.IsZero() {
		t.Error("meta.lastUpdateTime zero")
	}
	if got.Meta.ModelKey == nil || got.Meta.ModelKey.Name != modelName {
		t.Errorf("meta.modelKey missing/wrong on single-entity GET: %+v", got.Meta.ModelKey)
	}
	_ = uuid.Nil // keep import if unused after edits
}
```

- [ ] **Step 2: Register it** — in `e2e/parity/registry.go` `allTests`, near the entity CRUD block, add:

```go
	{Name: "EntityMetaShape", Fn: RunEntityMetaShape},
```

- [ ] **Step 3: Run to verify it fails, then passes** (it should already pass on memory after Task 3/4, since the server emits the shape — this scenario is the backend-agnostic lock)

Run: `go test ./e2e/parity/memory/ -run 'TestParity/EntityMetaShape' -v`
Expected: PASS on memory. If `ModelKey` is nil, the client mirror or handler drift needs fixing.
Then sqlite + postgres: `go test ./e2e/parity/sqlite/ ./e2e/parity/postgres/ -run 'TestParity/EntityMetaShape' -v` (Docker for postgres).
Expected: PASS on all three.

- [ ] **Step 4: `go vet` + commit**

```bash
go vet ./e2e/parity/...
git add e2e/parity/entity_slice.go e2e/parity/registry.go
git commit -m "test(parity): entity meta shape is backend-agnostic (E1)"
```

---

## Phase 3 — E3: `getAllEntities` honours `pointInTime` (via `GetAllAsAt`)

### Task 6: plumb `pointInTime` into the list read + populate `meta.pointInTime`

`GetAllEntities` (handler.go:613-654) ignores `params.PointInTime`; `ListEntities` (service.go:825) always calls `entityStore.GetAll`. Use the model-scoped list-PIT primitive `GetAllAsAt` (persistence.go:36) when a point-in-time is supplied, and stamp `meta.pointInTime` (E1 already declared it optional).

**Files:**
- Modify: `internal/domain/entity/service.go:825` (`ListEntities` gains `pointInTime *time.Time`; `GetAllAsAt` branch; `meta.pointInTime`).
- Modify: `internal/domain/entity/handler.go:635` (pass `params.PointInTime`).
- Modify: `internal/grpc/search.go:275` (pass `nil`, or the request PIT if in scope).
- Modify: `e2e/parity/client/http.go` (add `ListEntitiesByModelAt`).
- Create test: `internal/e2e/entity_asat_test.go` (as-at list e2e).
- Modify: `e2e/parity/entity_slice.go` + `e2e/parity/registry.go` (parity as-at scenario).

**Interfaces:**
- Produces: `func (h *Handler) ListEntities(ctx, entityName, modelVersion string, page PaginationParams, pointInTime *time.Time) ([]EntityEnvelope, error)`;
  `func (c *Client) ListEntitiesByModelAt(t, modelName string, modelVersion int, pointInTime time.Time) ([]EntityResult, error)`.

- [ ] **Step 1: Write the failing e2e test** — `internal/e2e/entity_asat_test.go`:

```go
package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestGetAllEntities_AsAt asserts the model-scoped list read honours
// pointInTime (E3): a list as-at a time before an update reflects the
// pre-update state, and meta.pointInTime echoes the requested time.
func TestGetAllEntities_AsAt(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-getall-asat"
	wf := `{ "importMode": "REPLACE", "workflows": [{
		"version": "1.1", "name": "asat-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
			"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]},
			"APPROVED": {}
		}}]}`
	setupModelWithWorkflow(t, model, wf)

	id := createEntityE2E(t, model, 1, `{"name":"Bob","status":"draft"}`)
	time.Sleep(10 * time.Millisecond)
	midpoint := time.Now().UTC().Format(time.RFC3339Nano)
	time.Sleep(10 * time.Millisecond)

	// Update via manual transition so state + data change after midpoint.
	up := doAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s/approve", id), `{"name":"Bob","status":"approved"}`)
	if up.StatusCode != http.StatusOK {
		t.Fatalf("transition: %d: %s", up.StatusCode, readBody(t, up))
	}

	// List as-at midpoint — must show CREATED (pre-update) + meta.pointInTime.
	resp := doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/1?pointInTime=%s", model, midpoint), "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getAllEntities as-at: %d: %s", resp.StatusCode, body)
	}
	var envs []map[string]any
	if err := json.Unmarshal([]byte(body), &envs); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if len(envs) != 1 {
		t.Fatalf("expected 1 entity as-at midpoint, got %d", len(envs))
	}
	meta := envs[0]["meta"].(map[string]any)
	if meta["state"] != "CREATED" {
		t.Errorf("as-at state = %v, want CREATED (pre-update)", meta["state"])
	}
	if meta["pointInTime"] == nil {
		t.Error("meta.pointInTime not populated on as-at list read")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/e2e/ -run TestGetAllEntities_AsAt -v` (Docker)
Expected: FAIL — as-at state is `APPROVED` (pointInTime ignored) and/or `meta.pointInTime` nil.

- [ ] **Step 3: Update `ListEntities`** — `internal/domain/entity/service.go:825`. Add the param and branch:

```go
func (h *Handler) ListEntities(ctx context.Context, entityName string, modelVersion string, page PaginationParams, pointInTime *time.Time) ([]EntityEnvelope, error) {
```

Replace the store read:

```go
	entities, err := entityStore.GetAll(ctx, ref)
	if err != nil {
		return nil, common.Internal("failed to get entities", err)
	}
```

with:

```go
	var entities []*spi.Entity
	if pointInTime != nil {
		entities, err = entityStore.GetAllAsAt(ctx, ref, *pointInTime)
	} else {
		entities, err = entityStore.GetAll(ctx, ref)
	}
	if err != nil {
		return nil, common.Internal("failed to get entities", err)
	}
```

In the meta-building loop, after the `transitionForLatestSave` block, add:

```go
		if pointInTime != nil {
			entMeta["pointInTime"] = pointInTime.UTC().Format(time.RFC3339Nano)
		}
```

- [ ] **Step 4: Pass `pointInTime` from the HTTP handler** — `internal/domain/entity/handler.go:635`:

```go
	envelopes, err := h.ListEntities(r.Context(), entityName, fmt.Sprintf("%d", modelVersion), PaginationParams{
		PageSize:   pageSize,
		PageNumber: pageNumber,
	}, params.PointInTime)
```

- [ ] **Step 5: Update the gRPC caller** — `internal/grpc/search.go:275`: append `, nil` as the new last argument (the gRPC snapshot-search list path carries no as-at point-in-time; if `req` exposes a pointInTime, thread it instead).

- [ ] **Step 6: Run the e2e test + build**

Run: `go build ./... && go test ./internal/e2e/ -run TestGetAllEntities_AsAt -v` (Docker)
Expected: PASS. Also re-run `TestGetAllEntities_ReturnsJSONArray` (enforce-mode meta conformance with the new optional `pointInTime`).

- [ ] **Step 7: Add the parity client method** — `e2e/parity/client/http.go`, after `ListEntitiesByModel`:

```go
func (c *Client) ListEntitiesByModelAt(t *testing.T, modelName string, modelVersion int, pointInTime time.Time) ([]EntityResult, error) {
	t.Helper()
	path := fmt.Sprintf("/api/entity/%s/%d?pointInTime=%s", modelName, modelVersion, pointInTime.Format(time.RFC3339Nano))
	var entities []EntityResult
	if _, err := c.doJSON(t, http.MethodGet, path, nil, &entities); err != nil {
		return nil, err
	}
	return entities, nil
}
```

- [ ] **Step 8: Add + register the parity scenario** — append to `e2e/parity/entity_slice.go`:

```go
// RunGetAllEntitiesAsAt asserts the model-scoped list read honours pointInTime
// on every backend (E3).
func RunGetAllEntitiesAsAt(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "getall-asat-parity"
	const modelVersion = 1
	setupSimpleWorkflow(t, c, modelName, modelVersion)

	id, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob","amount":1,"status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	midpoint := time.Now().UTC()
	time.Sleep(10 * time.Millisecond)
	if err := c.UpdateEntityData(t, id, `{"name":"Bob","amount":2,"status":"active"}`); err != nil {
		t.Fatalf("UpdateEntityData: %v", err)
	}

	asAt, err := c.ListEntitiesByModelAt(t, modelName, modelVersion, midpoint)
	if err != nil {
		t.Fatalf("ListEntitiesByModelAt: %v", err)
	}
	if len(asAt) != 1 {
		t.Fatalf("expected 1 entity as-at midpoint, got %d", len(asAt))
	}
	if got := asAt[0].Data["amount"]; got != float64(1) {
		t.Errorf("as-at amount = %v, want 1 (pre-update)", got)
	}
	if asAt[0].Meta.PointInTime == nil {
		t.Error("meta.pointInTime not populated on as-at list read")
	}
}
```

Register in `e2e/parity/registry.go`: `{Name: "GetAllEntitiesAsAt", Fn: RunGetAllEntitiesAsAt},`.
(Confirm `UpdateEntityData` exists on the client — http.go:557; if its signature differs, adapt.)

- [ ] **Step 9: Run parity on all three backends**

Run: `go test ./e2e/parity/memory/ ./e2e/parity/sqlite/ ./e2e/parity/postgres/ -run 'TestParity/GetAllEntitiesAsAt' -v` (Docker for postgres)
Expected: PASS on all three.

- [ ] **Step 10: `go vet` + commit**

```bash
go vet ./...
git add internal/domain/entity/service.go internal/domain/entity/handler.go internal/grpc/search.go internal/e2e/entity_asat_test.go e2e/parity/client/http.go e2e/parity/entity_slice.go e2e/parity/registry.go
git commit -m "feat(entity): getAllEntities honours pointInTime via GetAllAsAt; meta.pointInTime (E3)"
```

---

## Phase 4 — E2: conditional `deleteEntities` (data-loss fix, §6.1)

### Task 7: promote `INVALID_CONDITION` to a first-class code + help topic (Gate 4/6)

`INVALID_CONDITION` is currently a raw string literal (`grouped_stats_handler.go:167`), backed by the `ErrInvalidCondition` sentinel (`grouped_stats_service.go:30`). E2 needs it as a documented code the error-code matrix and OpenAPI table reference. Promote it to an `ErrCode*` constant, add the help topic (`TestErrCode_Parity` bijection), and route the existing literal through the constant.

**Files:**
- Modify: `internal/common/error_codes.go` (add `ErrCodeInvalidCondition`).
- Modify: `internal/domain/entity/grouped_stats_handler.go:167` (use the constant).
- Create: `cmd/cyoda/help/content/errors/INVALID_CONDITION.md`.

- [ ] **Step 1: Run the parity test to confirm current green, then write the RED** — add `ErrCodeInvalidCondition` to `internal/common/error_codes.go` (in the composite-unique-key block or a small new block):

```go
	// ErrCodeInvalidCondition is returned when a request body condition
	// (AbstractConditionDto) cannot be parsed. Non-retryable: the client
	// must fix the malformed condition.
	ErrCodeInvalidCondition = "INVALID_CONDITION"
```

- [ ] **Step 2: Run `TestErrCode_Parity` to verify it now FAILS**

Run: `go test ./cmd/cyoda/help/ -run TestErrCode_Parity -v`
Expected: FAIL — `ErrCode "INVALID_CONDITION" defined in error_codes.go but no errors/INVALID_CONDITION.md`.

- [ ] **Step 3: Create `cmd/cyoda/help/content/errors/INVALID_CONDITION.md`:**

```markdown
---
topic: errors.INVALID_CONDITION
title: "INVALID_CONDITION — request condition could not be parsed"
stability: stable
see_also:
  - errors
  - errors.BAD_REQUEST
---

# errors.INVALID_CONDITION

## NAME

INVALID_CONDITION — a request body condition (AbstractConditionDto) was malformed and could not be parsed.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

Endpoints that accept a search-style condition in the request body — grouped statistics and the conditional form of delete-by-model — reject a body whose condition cannot be parsed. The condition type is unrecognised, a nested clause is malformed, or the JSON does not match the expected condition envelope.

To resolve: correct the condition body to a valid `AbstractConditionDto` (see `cyoda help search`).

## SEE ALSO

- errors
- errors.BAD_REQUEST
```

- [ ] **Step 4: Route the existing literal through the constant** — `internal/domain/entity/grouped_stats_handler.go:167`, replace the raw `"INVALID_CONDITION"` argument with `common.ErrCodeInvalidCondition`.

- [ ] **Step 5: Run the parity + affected tests to verify green**

Run: `go test ./cmd/cyoda/help/ -run TestErrCode_Parity -v && go test ./internal/domain/entity/ -run Grouped -v && go build ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/common/error_codes.go internal/domain/entity/grouped_stats_handler.go cmd/cyoda/help/content/errors/INVALID_CONDITION.md
git commit -m "feat(errors): promote INVALID_CONDITION to a first-class code + help topic"
```

---

### Task 8: implement conditional `deleteEntities` (select-then-delete, §6.1)

Reuse existing primitives: `predicate.ParseCondition` → `SearchService.Search` (tx-visible inside the delete tx) → per-id `EntityStore.Delete`. Empty/absent body preserves today's delete-all. Present condition scopes the delete; `verbose` returns ids; `pointInTime` selects as-at. Decouple matched vs removed counts.

**Files:**
- Modify: `internal/domain/entity/handler.go` — `New` gains `searchSvc *search.SearchService`; `Handler` gains the field; `DeleteEntities` rewired; fix the `handler.go:601` "server is source of truth" comment.
- Modify: `internal/domain/entity/service.go` — new `DeleteEntitiesConditional` method + `DeleteResult` type.
- Modify: `app/app.go:535` (pass `a.searchService`); `internal/grpc/rpc_test.go:42` (pass `searchService`).
- Modify: `api/openapi.yaml` — `deleteEntities` description/error table: document the condition-scoped delete, `verbose` ids, and the `400 INVALID_CONDITION` / `404 MODEL_NOT_FOUND` codes (E5-adjacent). No response-schema change (`StreamDeleteResult` already has `ids` + `deleteResult`).
- Modify: `internal/e2e/zzz_errorcode_matrix_test.go` — add the `deleteEntities` row.
- Create test: `internal/e2e/entity_delete_conditional_test.go`.

**Interfaces:**
- Consumes: `predicate.ParseCondition([]byte) (predicate.Condition, error)`; `search.SearchService.Search(ctx, spi.ModelRef, predicate.Condition, search.SearchOptions) ([]*spi.Entity, error)`; `spi.EntityStore.Delete(ctx, entityID string) error`; `ErrInvalidCondition` sentinel (same package).
- Produces: `func New(factory, txMgr, uuids, engine, gate, searchSvc *search.SearchService) *Handler`;
  `func (h *Handler) DeleteEntitiesConditional(ctx, entityName, modelVersion string, condBody []byte, pointInTime *time.Time, verbose bool) (*DeleteResult, error)` returning
  `type DeleteResult struct { EntityModelID string; MatchedCount int; RemovedCount int; IDToError map[string]string; IDs []string }`.

- [ ] **Step 1: Write the failing e2e test** — `internal/e2e/entity_delete_conditional_test.go`:

```go
package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/google/uuid"
)

// TestDeleteEntities_Conditional_SubsetSurvives asserts a condition-scoped
// delete removes only matching entities and leaves the rest (E2 data-loss fix).
func TestDeleteEntities_Conditional_SubsetSurvives(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-delcond-1"
	importModel(t, model, 1)
	keepID := createEntityE2E(t, model, 1, `{"status":"keep","n":1}`)
	dropID := createEntityE2E(t, model, 1, `{"status":"drop","n":2}`)

	// Delete only status=drop. AbstractConditionDto simple-condition form.
	cond := `{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"drop"}`
	resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1?verbose=true", model), cond)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("conditional delete: %d: %s", resp.StatusCode, body)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	dr := obj["deleteResult"].(map[string]any)
	if got := dr["numberOfEntititesRemoved"]; got != float64(1) {
		t.Errorf("removed = %v, want 1", got)
	}
	// verbose ids must contain the dropped id, not the kept one.
	ids, _ := obj["ids"].([]any)
	if len(ids) != 1 || ids[0] != dropID {
		t.Errorf("verbose ids = %v, want [%s]", ids, dropID)
	}

	// Kept entity still readable; dropped entity is gone.
	if r := doAuth(t, http.MethodGet, "/api/entity/"+keepID, ""); r.StatusCode != http.StatusOK {
		t.Errorf("kept entity should survive, got %d", r.StatusCode)
	}
	if r := doAuth(t, http.MethodGet, "/api/entity/"+dropID, ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("dropped entity should be gone, got %d", r.StatusCode)
	}
}

// TestDeleteEntities_InvalidCondition asserts a malformed condition body → 400 INVALID_CONDITION.
func TestDeleteEntities_InvalidCondition(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-delcond-2"
	importModel(t, model, 1)
	resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), `{"type":"NONSENSE"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed condition: expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	commontest.ExpectErrorCode(t, resp, "INVALID_CONDITION")
}

// TestDeleteEntities_UnknownModel asserts deleting an unregistered model → 404 MODEL_NOT_FOUND.
func TestDeleteEntities_UnknownModel(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp := doAuth(t, http.MethodDelete, "/api/entity/never-registered-"+uuid.NewString()+"/1", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model delete: expected 404, got %d", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "MODEL_NOT_FOUND")
}
```

(Confirm the `AbstractConditionDto` simple-condition field names — `type`/`jsonPath`/`operatorType`/`value` — against `predicate.ParseCondition` / an existing search e2e; adjust the `cond` literal to whatever the parser accepts. Grep `e2e/parity` or `internal/e2e` for an existing `"type":"simple"` body.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/e2e/ -run 'TestDeleteEntities_Conditional_SubsetSurvives|TestDeleteEntities_InvalidCondition|TestDeleteEntities_UnknownModel' -v` (Docker)
Expected: FAIL — the current handler ignores the body and wipes the whole model (both survive-check and removed-count assertions fail); no INVALID_CONDITION path.

- [ ] **Step 3: Inject `SearchService` into the Handler** — `internal/domain/entity/handler.go`:

Add the field to `Handler` (l.85-95):

```go
	searchSvc *search.SearchService
```

Update `New` (l.93):

```go
func New(factory spi.StoreFactory, txMgr spi.TransactionManager, uuids spi.UUIDGenerator, engine *wfengine.Engine, gate *txgate.Registry, searchSvc *search.SearchService) *Handler {
	return &Handler{factory: factory, txMgr: txMgr, uuids: uuids, engine: engine, gate: gate, searchSvc: searchSvc}
}
```

Ensure `internal/domain/search` is imported in `handler.go` (it is already imported in the package via `grouped_stats_service.go`; add the import to `handler.go` if needed).

- [ ] **Step 4: Update the two `entity.New` call sites**
  - `app/app.go:535`: `entity.New(a.storeFactory, a.transactionManager, common.NewDefaultUUIDGenerator(), a.workflowEngine, a.txGate, a.searchService)`.
  - `internal/grpc/rpc_test.go:42`: `entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), searchService)` — `searchService` is already constructed in `newTestEnv` (rpc_test.go).

- [ ] **Step 5: Add the `DeleteEntitiesConditional` service method** — `internal/domain/entity/service.go` (near `DeleteAllEntities`):

```go
// DeleteResult reports a conditional delete: MatchedCount entities matched the
// condition (or all, for an empty body), RemovedCount were actually deleted,
// IDToError maps any per-id delete failures, and IDs lists the matched ids when
// verbose was requested.
type DeleteResult struct {
	EntityModelID string
	MatchedCount  int
	RemovedCount  int
	IDToError     map[string]string
	IDs           []string
}

// DeleteEntitiesConditional deletes entities of a model. An empty condBody
// deletes all (backward-compatible). A present condBody is parsed and only
// matching entities (as-at pointInTime, when supplied) are deleted — reusing
// the search condition primitive so no special engine rights are claimed
// (design §6.1). Selection and deletion run inside one transaction; because
// SearchService.Search bypasses backend pushdown when a tx is on the context,
// buffered writes are visible to the selection.
func (h *Handler) DeleteEntitiesConditional(ctx context.Context, entityName, modelVersion string, condBody []byte, pointInTime *time.Time, verbose bool) (*DeleteResult, error) {
	ref := spi.ModelRef{EntityName: entityName, ModelVersion: modelVersion}

	// Parse the condition (if any) BEFORE opening a tx — a parse error is a
	// 400 that must not start a transaction. Empty/whitespace body ⇒ delete-all.
	var cond predicate.Condition
	if len(bytes.TrimSpace(condBody)) > 0 {
		c, err := predicate.ParseCondition(condBody)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidCondition, err)
		}
		cond = c
	}

	// Delete-all fast path preserves existing behaviour + response shape.
	if cond == nil {
		all, err := h.DeleteAllEntities(ctx, entityName, modelVersion)
		if err != nil {
			return nil, err
		}
		return &DeleteResult{
			EntityModelID: all.EntityModelID,
			MatchedCount:  all.TotalCount,
			RemovedCount:  all.TotalCount,
			IDToError:     map[string]string{},
		}, nil
	}

	txID, txCtx, owned, err := h.beginOrJoin(ctx)
	if err != nil {
		return nil, common.Internal("failed to begin transaction", err)
	}
	if !owned {
		defer h.gate.Acquire(txID)()
	}

	modelStore, err := h.factory.ModelStore(txCtx)
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		return nil, common.Internal("failed to access model store", err)
	}
	if _, err := modelStore.Get(txCtx, ref); err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		if errors.Is(err, spi.ErrNotFound) {
			return nil, common.Operational(http.StatusNotFound, common.ErrCodeModelNotFound,
				fmt.Sprintf("cannot find model entityName=%s, version=%s", entityName, modelVersion))
		}
		return nil, common.Internal("failed to load model", err)
	}

	entityStore, err := h.factory.EntityStore(txCtx)
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		return nil, common.Internal("failed to access entity store", err)
	}

	// Select matching ids (tx-visible; honours pointInTime).
	matched, err := h.searchSvc.Search(txCtx, ref, cond, search.SearchOptions{PointInTime: pointInTime})
	if err != nil {
		h.rollbackOwned(txCtx, txID, owned)
		return nil, common.Internal("failed to select entities for delete", err)
	}

	result := &DeleteResult{
		EntityModelID: deterministicModelID(ref).String(),
		MatchedCount:  len(matched),
		IDToError:     map[string]string{},
	}

	// Finalize: gate the per-id deletes + commit against a concurrent joined
	// callback's buffer write (mirror DeleteAllEntities).
	if appErr := func() *common.AppError {
		if owned {
			defer h.gate.Acquire(txID)()
		}
		for _, e := range matched {
			id := e.Meta.ID
			if verbose {
				result.IDs = append(result.IDs, id)
			}
			if err := entityStore.Delete(txCtx, id); err != nil {
				result.IDToError[id] = err.Error()
				continue
			}
			result.RemovedCount++
		}
		if err := h.commitOwned(txCtx, txID, owned); err != nil {
			// Do NOT roll back here — a failed commit has already aborted the
			// tx. Mirrors DeleteAllEntities (service.go:800-820), which returns
			// the AppError directly on this path without an extra rollback.
			if errors.Is(err, spi.ErrConflict) {
				return common.Operational(http.StatusConflict, common.ErrCodeConflict, "transaction conflict — retry").AsRetryable()
			}
			return common.Internal("failed to commit transaction", err)
		}
		return nil
	}(); appErr != nil {
		return nil, appErr
	}

	return result, nil
}
```

In the condition-present guard (Step 5, near the top of the method) inline `bytes.TrimSpace`
directly — do NOT add a `bytesTrimSpace` wrapper:

```go
	if len(bytes.TrimSpace(condBody)) > 0 {
```

**Imports (compile-blocker — name them explicitly):** `internal/domain/entity/service.go` must
import `bytes` AND `github.com/cyoda-platform/cyoda-go/internal/domain/search` (for
`search.SearchOptions` and `h.searchSvc.Search`). Imports are per-file, so the package already
importing `search` via `grouped_stats_service.go` does not help `service.go`. `e.Meta.ID` is the
entity id string; keep the `IDs`/`IDToError` keys as strings to match the current response shape.

- [ ] **Step 6: Rewire the `DeleteEntities` HTTP handler** — `internal/domain/entity/handler.go:592-611`:

```go
func (h *Handler) DeleteEntities(w http.ResponseWriter, r *http.Request, entityName string, modelVersion int32, params genapi.DeleteEntitiesParams) {
	condBody, err := io.ReadAll(r.Body)
	if err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read request body"))
		return
	}

	verbose := params.Verbose != nil && *params.Verbose
	result, err := h.DeleteEntitiesConditional(r.Context(), entityName, fmt.Sprintf("%d", modelVersion), condBody, params.PointInTime, verbose)
	if err != nil {
		if errors.Is(err, ErrInvalidCondition) {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidCondition, err.Error()))
			return
		}
		common.WriteError(w, r, classifyError(err))
		return
	}

	// StreamDeleteResult: single object with entityModelClassId, deleteResult,
	// and optional ids (verbose). numberOfEntitites = matched, ...Removed =
	// actually deleted (decoupled — a condition may match more than it removes
	// if a per-id delete fails). Reconciled to the per-finding policy (design §2).
	deleteResult := map[string]any{
		"idToError":                result.IDToError,
		"numberOfEntitites":        result.MatchedCount,
		"numberOfEntititesRemoved": result.RemovedCount,
	}
	resp := map[string]any{
		"entityModelClassId": result.EntityModelID,
		"deleteResult":       deleteResult,
	}
	if verbose {
		resp["ids"] = result.IDs
	}
	common.WriteJSON(w, http.StatusOK, resp)
}
```

Ensure `io` and `errors` are imported in `handler.go`. Remove the stale "Server is source of truth per design §3" comment (the old l.601) — the per-finding reconciliation policy (§2) now governs.

- [ ] **Step 7: Run the e2e tests + the response-shape regression**

Run: `go build ./... && go test ./internal/e2e/ -run 'TestDeleteEntities' -v` (Docker)
Expected: PASS — including the pre-existing `TestDeleteEntities_ResponseShape` (empty-body delete-all still returns the single object with matched==removed==total).

- [ ] **Step 8: Update the `deleteEntities` OpenAPI error table + example** — `api/openapi.yaml` `deleteEntities` operation: in the description, document that an `AbstractConditionDto` body scopes the delete to matching entities (empty body ⇒ all), `verbose=true` returns the deleted `ids`, and `numberOfEntitites` vs `numberOfEntititesRemoved` distinguish matched from removed. Add `400` `INVALID_CONDITION` (malformed condition) to the error prose (the 400 response already exists — no new status). Confirm `StreamDeleteResult` remains the 200 schema. Run `go test ./api/ -v` + the enforce-mode delete tests to confirm conformance.

- [ ] **Step 9: Add the `deleteEntities` matrix row** — `internal/e2e/zzz_errorcode_matrix_test.go`, extend `EntityErrorCodeMatrix`:

```go
	"deleteEntities": {
		{Status: 400, Code: "INVALID_CONDITION"},
		{Status: 404, Code: "MODEL_NOT_FOUND"},
	},
```

Run the suite: `go test ./internal/e2e/ -run 'TestZZZErrorCodeMatrix|TestDeleteEntities' -v`. If `declared` reports another observed deleteEntities code, add its cell (it's real). If `producible` reports a gap, ensure the Step-1 tests cover it.

- [ ] **Step 10: `go vet` + commit**

```bash
go vet ./...
git add internal/domain/entity/handler.go internal/domain/entity/service.go app/app.go internal/grpc/rpc_test.go api/openapi.yaml internal/e2e/entity_delete_conditional_test.go internal/e2e/zzz_errorcode_matrix_test.go
git commit -m "feat(entity): conditional deleteEntities — select-then-delete honouring condition/pointInTime/verbose (E2, closes data-loss)"
```

---

### Task 9: parity (conditional delete inside a tx) + gRPC delete coverage (D2)

Per §6.1, `SearchService.Search` bypasses pushdown only when a tx is on the context, so parity for conditional delete **must** exercise it inside a joined transaction — use the compute-callback-join pattern. gRPC has no conditional delete (D2, deferred); cover its existing unconditional delete so the entry point is not waived.

**Files:**
- Modify: `e2e/parity/entity_slice.go` — `RunEntityConditionalDeleteInTx` (callback-join pattern).
- Modify: `e2e/parity/registry.go` — register it.
- Modify: `internal/grpc/rpc_test.go` — assert `EntityDeleteAllRequest` unconditional delete-all behaviour (if not already covered).

**Interfaces:**
- Consumes: `callback_txjoin.go` helpers (`cbSetupModel`, `cbContext`, `fixture.ComputeTenant`), `client.Client`.
- Produces: `func RunEntityConditionalDeleteInTx(t *testing.T, fixture BackendFixture)`.

- [ ] **Step 1: Write the parity scenario** — append to `e2e/parity/entity_slice.go`. Model a SYNC processor whose callback issues the conditional delete over the HTTP boundary while carrying `X-Tx-Token` (mirror `RunCallbackCriteriaReadYourWrites`, `callback_txjoin.go:247-288`), then assert only the matching subset was deleted **after commit**. Because the tx-visibility path is what §6.1 flags, the callback must perform the conditional delete inside the joined tx `T`:

```go
// RunEntityConditionalDeleteInTx exercises conditional delete inside a joined
// transaction (the path where SearchService.Search bypasses pushdown, §6.1),
// asserting the matching subset — and only it — is removed after commit.
func RunEntityConditionalDeleteInTx(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	// ... build a workflow with a SYNC processor whose callback (carrying
	// X-Tx-Token) DELETEs /api/entity/{model}/1 with a condition selecting a
	// subset created earlier in T; then transition, commit, and assert the
	// survivors. Follow cbSetupModel / cbContext / cbAssertSameTxID usage in
	// callback_txjoin.go. If the callback-join harness cannot express a
	// model-scoped conditional delete from a processor callback, fall back to
	// an isolated single-backend e2e (NOT the parity suite) that opens a tx via
	// the same join mechanism, and register that instead — record the reason
	// inline. Do NOT weaken this to a non-tx delete: the tx path is the point.
}
```

**Note to implementer:** this is the subtlest test in the slice. Budget time to study
`callback_txjoin.go` first. The invariant to prove: inside `T`, the conditional selection sees
buffered writes and deletes exactly the matching subset; after commit, non-matching entities
survive on **every** backend. If expressing it through the callback harness proves infeasible,
implement it as an isolated single-backend e2e in `internal/e2e/` (consistency assertion, not an
interleave — per `.claude/rules/test-coverage.md`) and note the deviation in the plan's execution
log; keep the backend-agnostic meta/as-at/request-body parity scenarios (Tasks 5, 6, 10).

- [ ] **Step 2: Register + run**

Register `{Name: "EntityConditionalDeleteInTx", Fn: RunEntityConditionalDeleteInTx}` in `registry.go`.
Run: `go test ./e2e/parity/memory/ ./e2e/parity/sqlite/ ./e2e/parity/postgres/ -run 'TestParity/EntityConditionalDeleteInTx' -v` (Docker for postgres).
Expected: PASS on all three (the commercial backend picks it up on its next dependency update).

- [ ] **Step 3: gRPC unconditional delete-all coverage (D2)** — in `internal/grpc/rpc_test.go`, confirm/add a test that `EntityDeleteAllRequest` deletes all entities of a model and returns `Success` (mirror `TestRPC_EntityDelete`). This documents that gRPC stays unconditional; the conditional form is HTTP-only (D2 recorded in cloud-parity, Task 15).

```go
func TestRPC_EntityDeleteAll_Unconditional(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice", "age": 30})
	// create two entities, then EntityDeleteAllRequest for the model, assert
	// Success and that a subsequent get/list is empty. gRPC has no condition
	// field — this is delete-all by contract (D2).
}
```

- [ ] **Step 4: Run gRPC tests + `go vet` + commit**

Run: `go test ./internal/grpc/ -run 'TestRPC_EntityDelete' -v && go vet ./...`

```bash
git add e2e/parity/entity_slice.go e2e/parity/registry.go internal/grpc/rpc_test.go
git commit -m "test(parity/grpc): conditional delete inside tx (E2) + gRPC unconditional delete-all coverage (D2)"
```

---

## Phase 5 — E4: tighten `create` / `createCollection` request bodies

The validator validates responses only, so this is a documentation + behaviour-lock finding: the request schemas (`type: object`) erase the real shape. Tighten them to the shapes the server already parses (spec-incomplete), and lock with e2e/parity asserting accept-good/reject-bad against the server's own validation.

### Task 10: request-body schemas + accept/reject tests

**Files:**
- Modify: `api/openapi.yaml` — `create` requestBody schema (~3066) and `createCollection` requestBody schema (~2229).
- Create test: `internal/e2e/entity_request_body_test.go`.
- Modify: `e2e/parity/entity_slice.go` + `registry.go` — collection-create parity scenario (if not already covered by an existing scenario — grep first).

- [ ] **Step 1: Write the failing e2e test** — `internal/e2e/entity_request_body_test.go`:

```go
package e2e_test

import (
	"fmt"
	"net/http"
	"testing"
)

// TestCreate_BatchArrayAccepted asserts the create endpoint accepts a JSON
// array of entity objects (batch form documented by E4).
func TestCreate_BatchArrayAccepted(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-create-batch"
	importModel(t, model, 1)
	body := `[{"n":1},{"n":2}]`
	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch create: expected 200, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
}

// TestCreateCollection_RejectsNonArray asserts the collection endpoint rejects
// a non-array body (documented as an array of {model,payload}).
func TestCreateCollection_RejectsNonArray(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp := doAuth(t, http.MethodPost, "/api/entity/JSON", `{"not":"an array"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-array collection: expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
}
```

- [ ] **Step 2: Run to verify current behaviour** (these may already pass — the server already parses arrays; the point is to pin the documented shapes)

Run: `go test ./internal/e2e/ -run 'TestCreate_BatchArrayAccepted|TestCreateCollection_RejectsNonArray' -v` (Docker)
Expected: PASS (behaviour) — if `TestCreateCollection_RejectsNonArray` returns 200, the server accepts a non-array and the schema/behaviour disagree; reconcile (fix whichever is wrong per §2) before tightening the schema.

- [ ] **Step 3: Tighten the `create` request schema** — `api/openapi.yaml` (~3066), replace:

```yaml
            schema:
              type: object
```

with:

```yaml
            schema:
              description: |
                A single entity JSON object, or a JSON array of entity objects
                (batch create). The model is identified by the path; the payload
                shape is polymorphic per model, so items stay open.
              oneOf:
                - type: object
                  additionalProperties: true
                - type: array
                  items:
                    type: object
                    additionalProperties: true
```

- [ ] **Step 4: Tighten the `createCollection` request schema** — `api/openapi.yaml` (~2229), replace:

```yaml
            schema:
              type: object
              format: json
```

with:

```yaml
            schema:
              type: array
              description: |
                Batch of entities to create across one or more models. Each item
                names its model and carries the entity payload as a JSON string.
              items:
                type: object
                required:
                  - model
                  - payload
                properties:
                  model:
                    type: object
                    required:
                      - name
                      - version
                    properties:
                      name:
                        type: string
                      version:
                        type: integer
                        format: int32
                  payload:
                    type: string
```

- [ ] **Step 5: Build + verify no enforce-mode regression + tests pass**

Run: `go build ./... && go vet ./... && go test ./api/ -v && go test ./internal/e2e/ -run 'TestCreate|TestCreateCollection' -v` (Docker)
Expected: PASS (request schemas are not response-validated, so no enforce-mode impact; the create/collection response tests still conform).

- [ ] **Step 6: Parity (collection-create backend-agnostic)** — grep `e2e/parity` for an existing `RunEntityCreateCollection`/`CreateEntitiesCollection` scenario; if present, no new scenario needed (note it). If absent, add `RunEntityCreateCollectionShape` to `entity_slice.go` using `c.CreateEntitiesCollection` (http.go:587) with the documented array-of-`{model,payload}` and assert success across backends; register it.

- [ ] **Step 7: Commit**

```bash
git add api/openapi.yaml internal/e2e/entity_request_body_test.go e2e/parity/
git commit -m "feat(openapi): tighten create/createCollection request bodies to their real shapes (E4)"
```

---

## Phase 6 — E5: document composite-unique-key `409`/`422` codes

The codes (`UNIQUE_VIOLATION` 409, `INVALID_UNIQUE_KEY` / `INVALID_UNIQUE_KEY_DEFINITION` 422) already exist (constants + help topics + emission) and are exercised by `internal/e2e/unique_keys_test.go`, `e2e/parity/unique_keys.go`, and gRPC `TestRPC_EntityCreate_UniqueViolation` / `_InvalidUniqueKey`. This finding is spec-incomplete: the endpoints' OpenAPI error tables don't list them, and the matrix doesn't yet declare them. Close both.

### Task 11: OpenAPI error tables + matrix rows for unique-key codes

**Files:**
- Modify: `api/openapi.yaml` — `create`, `createCollection`, `updateCollection`, `updateSingle[WithLoopback]`, `patchSingle[WithLoopback]` operation descriptions/error prose to list `409 UNIQUE_VIOLATION` and `422 INVALID_UNIQUE_KEY` / `INVALID_UNIQUE_KEY_DEFINITION` (no new status codes — 409/422 responses already exist on these ops or are added as prose to the existing error family).
- Modify: `internal/e2e/zzz_errorcode_matrix_test.go` — add rows for these ops.
- Verify: existing `internal/e2e/unique_keys_test.go` produces the codes on the `create` operation (so the matrix `producible` check is satisfied). If a code is produced on an operation the matrix names but a status/code cell is missing, add it.

- [ ] **Step 1: Add matrix rows (RED via producible/declared)** — extend `EntityErrorCodeMatrix` in `internal/e2e/zzz_errorcode_matrix_test.go`:

```go
	"create": {
		{Status: 409, Code: "UNIQUE_VIOLATION"},
		{Status: 422, Code: "INVALID_UNIQUE_KEY"},
		{Status: 404, Code: "MODEL_NOT_FOUND"},
	},
	"createCollection": {
		{Status: 409, Code: "UNIQUE_VIOLATION"},
		{Status: 422, Code: "INVALID_UNIQUE_KEY"},
	},
```

(Start with the cells you can pin from existing unique-key e2e. `INVALID_UNIQUE_KEY_DEFINITION` is emitted on the model set-unique-keys path, not create — include it only under the operation that actually emits it; the `declared` check will name any observed cell you missed.)

- [ ] **Step 2: Run the suite to see producible/declared gaps**

Run: `go test ./internal/e2e/ -run 'TestZZZErrorCodeMatrix|TestUniqueKey|Unique' -v` (Docker)
Expected: the matrix drives the reconciliation. Resolve each reported gap:
  - **producible gap** (declared cell never observed) → either the code isn't emitted on that op (remove the cell) or no e2e exercises it (add a minimal one modelled on `unique_keys_test.go`).
  - **declared gap** (observed cell undocumented) → add the cell (it's a real emitted code) and its OpenAPI error-table entry.
Iterate until green.

- [ ] **Step 3: Update the OpenAPI error tables** — for each op in scope, add prose to the operation description (and confirm the 409/422 responses are declared; the create/collection family already declares 400/404/409 — add 422 to the responses map if a `422` response object is missing, referencing `ProblemDetail`). Keep prose compact: one line per code.

- [ ] **Step 4: gRPC envelope coverage** — confirm `TestRPC_EntityCreate_UniqueViolation` (rpc_test.go:651) and `TestRPC_EntityCreate_InvalidUniqueKey` (708) exist and assert `Error.Code == "CLIENT_ERROR"` + `strings.Contains(Error.Message, "<CODE>")` (N1). No change needed unless missing.

- [ ] **Step 5: Build, full entity/e2e run, vet, commit**

Run: `go build ./... && go vet ./... && go test ./internal/e2e/ -run 'TestZZZErrorCodeMatrix|Unique|Create' -v`

```bash
git add api/openapi.yaml internal/e2e/zzz_errorcode_matrix_test.go internal/e2e/
git commit -m "feat(openapi): document composite-unique-key 409/422 codes on entity write endpoints (E5)"
```

---

## Phase 7 — E7 changes ordering + E8 `changeType` spelling

### Task 12: E7 — fix `getEntityChangesMetadata` ordering prose (reverse-chronological)

The server sorts changes **newest-first** (`service.go:720-722`); the spec says "chronological order" (openapi.yaml:1448). Direction: spec-stale (fix the doc).

**Files:**
- Modify: `api/openapi.yaml:1448`.
- Create test: `internal/e2e/entity_changes_order_test.go`.

- [ ] **Step 1: Write the failing e2e test** — `internal/e2e/entity_changes_order_test.go`: create an entity, update it, GET `/api/entity/{id}/changes`, assert the first element's `timeOfChange` is **after** the last element's (newest-first).

```go
package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestGetEntityChanges_NewestFirst(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-changes-order"
	wf := `{ "importMode": "REPLACE", "workflows": [{
		"version": "1.1", "name": "co-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
			"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]},
			"APPROVED": {}
		}}]}`
	setupModelWithWorkflow(t, model, wf)
	id := createEntityE2E(t, model, 1, `{"n":1}`)
	up := doAuth(t, http.MethodPut, "/api/entity/JSON/"+id+"/approve", `{"n":2}`)
	if up.StatusCode != http.StatusOK {
		t.Fatalf("transition: %d", up.StatusCode)
	}
	resp := doAuth(t, http.MethodGet, "/api/entity/"+id+"/changes", "")
	body := readBody(t, resp)
	var changes []map[string]any
	if err := json.Unmarshal([]byte(body), &changes); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if len(changes) < 2 {
		t.Fatalf("expected >=2 changes, got %d", len(changes))
	}
	first, _ := time.Parse(time.RFC3339Nano, changes[0]["timeOfChange"].(string))
	last, _ := time.Parse(time.RFC3339Nano, changes[len(changes)-1]["timeOfChange"].(string))
	if !first.After(last) {
		t.Errorf("expected newest-first: first %v should be after last %v", first, last)
	}
}
```

- [ ] **Step 2: Run to verify it passes against the server** (the server already sorts newest-first, so this test PASSES immediately — it is the executable pin for the prose fix). Run: `go test ./internal/e2e/ -run TestGetEntityChanges_NewestFirst -v` (Docker). Expected: PASS.

- [ ] **Step 3: Fix the prose** — `api/openapi.yaml:1448`, change `Changes are returned in chronological order` to `Changes are returned in reverse-chronological order (newest first)`.

- [ ] **Step 4: Commit**

```bash
git add api/openapi.yaml internal/e2e/entity_changes_order_test.go
git commit -m "docs(openapi): getEntityChangesMetadata returns reverse-chronological order (E7)"
```

---

### Task 13: E8 — align `changeType` spelling to canonical `CREATE/UPDATE/DELETE`

**Decision (needs-decision → decided).** Canonical spelling is `CREATE/UPDATE/DELETE`: the canonical `EntityChangeMeta.json`, the gRPC surface (`mapChangeType`), and the parity client mirror already use it; only OpenAPI + the HTTP handler diverge (`CREATED/UPDATED/DELETED`). Align OpenAPI + HTTP to the canonical spelling (removing/changing a Cloud-consumed canonical field is the higher-risk direction — §6.2 reasoning). Record in cloud-parity (Task 14). **Atomic:** the OpenAPI enum change + the HTTP handler mapping must land in one commit, or enforce-mode fails the interim state.

**Files:**
- Modify: `internal/domain/entity/handler.go:569-590` (`GetEntityChangesMetadata` — map `changeType`).
- Modify: `api/openapi.yaml` — `EntityChangeMeta` enum (10160-10165) + `getEntityChangesMetadata` example (1490-1500).
- Create test: `internal/e2e/entity_change_type_test.go`.

**Interfaces:**
- Produces: unexported `func canonicalChangeType(ct string) string` in the entity package (maps `CREATED→CREATE`, `UPDATED→UPDATE`, `DELETED→DELETE`, else passthrough — mirrors gRPC `mapChangeType`).

- [ ] **Step 1: Write the failing e2e test** — `internal/e2e/entity_change_type_test.go`: create an entity, GET `/changes`, assert the create record's `changeType == "CREATE"` (present tense, canonical).

```go
package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestGetEntityChanges_CanonicalChangeType(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-changetype"
	importModel(t, model, 1)
	id := createEntityE2E(t, model, 1, `{"n":1}`)
	resp := doAuth(t, http.MethodGet, "/api/entity/"+id+"/changes", "")
	body := readBody(t, resp)
	var changes []map[string]any
	if err := json.Unmarshal([]byte(body), &changes); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if len(changes) < 1 {
		t.Fatalf("expected >=1 change")
	}
	// The create record must use the canonical present-tense spelling.
	last := changes[len(changes)-1] // oldest = create (newest-first order)
	if last["changeType"] != "CREATE" {
		t.Errorf("changeType = %v, want CREATE (canonical)", last["changeType"])
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/e2e/ -run TestGetEntityChanges_CanonicalChangeType -v` (Docker)
Expected: FAIL — server emits `CREATED`.

- [ ] **Step 3: Add the mapper + apply it in the HTTP handler** — `internal/domain/entity/handler.go`, add:

```go
// canonicalChangeType maps the internal change-type spelling
// (CREATED/UPDATED/DELETED) to the canonical wire spelling
// (CREATE/UPDATE/DELETE) shared with the canonical schema, gRPC, and the
// parity client (design §6.3 / E8). Unknown values pass through.
func canonicalChangeType(ct string) string {
	switch ct {
	case "CREATED":
		return "CREATE"
	case "UPDATED":
		return "UPDATE"
	case "DELETED":
		return "DELETE"
	default:
		return ct
	}
}
```

In `GetEntityChangesMetadata` (l.569-590) change `"changeType": e.ChangeType,` to `"changeType": canonicalChangeType(e.ChangeType),`.

- [ ] **Step 4: Change the OpenAPI enum + example (same commit)** — `api/openapi.yaml`:
  - `EntityChangeMeta` component enum (10160-10165): `CREATED/UPDATED/DELETED` → `CREATE/UPDATE/DELETE`.
  - `getEntityChangesMetadata` example (1490-1500): `changeType: CREATED` → `CREATE`, `UPDATED` → `UPDATE`.

- [ ] **Step 5: Run the e2e test + enforce-mode conformance**

Run: `go build ./... && go test ./internal/e2e/ -run 'TestGetEntityChanges' -v` (Docker)
Expected: PASS — HTTP now emits `CREATE`, matching the tightened enum (enforce-mode validates the enum).

- [ ] **Step 6: gRPC enum assertion** — add/confirm a gRPC test asserting the changes-metadata stream emits `CREATE` (gRPC already maps via `mapChangeType`; assert it so the entry point is covered). Reference `search.go:540`.

- [ ] **Step 7: `go vet` + commit**

```bash
go vet ./...
git add internal/domain/entity/handler.go api/openapi.yaml internal/e2e/entity_change_type_test.go internal/grpc/
git commit -m "feat(openapi): align changeType to canonical CREATE/UPDATE/DELETE across OpenAPI + HTTP (E8)"
```

---

## Phase 8 — documentation, seed cleanup, verification (Gates 4/6/7)

### Task 14: cloud-parity reconciliation entries + two open questions (Gate 7)

**Files:**
- Modify: `docs/cloud-parity/openapi-conformance.md` — append a reconciliation section.
- Modify: `e2e/parity/client/types.go` — reconcile the mirror IF a wire shape changed (E1 `EntityMetadata` already matches; `EntityChangeMeta.ChangeType` is `string`, comment already `CREATE/UPDATE/DELETE` — verify, adjust only if needed).

- [ ] **Step 1: Append the reconciliation section** to `docs/cloud-parity/openapi-conformance.md`:

```markdown
## Entity-slice reconciliations (2026-07)

Per-finding contract decisions from the entity reconciliation slice (design
`docs/superpowers/specs/2026-07-02-openapi-contract-reconciliation-design.md`,
ADR 0003). Each states the direction and what Cloud must mirror.

- **E1 — `Envelope.meta` typed-but-open.** `meta` now mirrors the canonical
  `EntityMetadata.json` (required `id/state/creationDate/lastUpdateTime`; optional
  `modelKey/pointInTime/transitionForLatestSave/transactionId`); the
  `previousTransition` fossil is removed. Direction: spec-incomplete + spec-stale.
  Cloud MUST emit the same typed-but-open meta.
- **E2 — conditional `deleteEntities` (HTTP).** `DELETE /entity/{name}/{version}`
  with an `AbstractConditionDto` body deletes only matching entities (empty body ⇒
  all); `verbose=true` returns deleted ids; `numberOfEntitites` (matched) and
  `numberOfEntititesRemoved` (removed) are distinct. Direction: server-gap (closed a
  data-loss defect). Cloud MUST honour the condition — never ignore it.
- **E3 — `getAllEntities` as-at.** The model-scoped list read honours `pointInTime`
  (via the list-PIT primitive) and stamps `meta.pointInTime`. Direction: server-gap.
- **E8 — `changeType` spelling = `CREATE/UPDATE/DELETE`.** Canonical, gRPC, HTTP, and
  OpenAPI now agree on the present-tense spelling (was `CREATED/UPDATED/DELETED` on
  HTTP+OpenAPI). Direction: needs-decision → decided (canonical/gRPC already used it;
  changing the Cloud-consumed canonical field was the higher-risk alternative).
  Cloud MUST emit `CREATE/UPDATE/DELETE`.

## Open questions (Cloud-fact-blocked)

Decided once the Cloud facts are gathered (Gate 7):

- **E6 — `EntityChangeMeta.fieldsChangedCount`.** Declared in canonical
  `EntityChangeMeta.json`, never emitted by cyoda-go. Question: **does Cloud emit
  `fieldsChangedCount`?** If yes → implement emission (server-gap); if no → remove
  from the canonical schema (spec-stale). Leaning: implement (removing a
  Cloud-consumed canonical field is higher-risk). Not implemented in this slice.
- **D2 — gRPC conditional delete.** The conditional delete is an HTTP-contract
  feature; the gRPC/proto contract exposes only unconditional delete-all + per-entity
  delete. Question: **does Cloud expect gRPC conditional-delete parity?** If yes →
  add it to the proto contract; if no → HTTP-only is correct. Not implemented here.

- **Error-code naming — `BAD_REQUEST` vs `MALFORMED_REQUEST`.** Entity write handlers
  emit `BAD_REQUEST` for malformed bodies; `grouped_stats` emits `MALFORMED_REQUEST`
  for the same class. Unifying spans create/collection/patch and the stats/search +
  model slices. Deferred to a cross-slice follow-on; the entity error tables document
  the codes actually emitted.
```

- [ ] **Step 2: Verify the parity mirror** — inspect `e2e/parity/client/types.go`; confirm `EntityMetadata` already carries `ModelKey`, `PointInTime`, `TransitionForLatestSave`, `TransactionID` (it does) and `EntityChangeMeta.ChangeType` handling. Change only if a field is missing/misnamed. If no change is needed, note it and skip.

- [ ] **Step 3: Commit**

```bash
git add docs/cloud-parity/openapi-conformance.md e2e/parity/client/types.go
git commit -m "docs(cloud-parity): entity-slice reconciliations + E6/D2/error-naming open questions (Gate 7)"
```

---

### Task 15: seed cleanup — thin `entity_conformance_test.go`, fix its policy comment (§4.3, Gate 6)

Once `meta`/envelope are enforced by the ambient validator (Task 3), the hand-parsed key assertions in `entity_conformance_test.go` (`TestGetOneEntity_ReturnsEnvelope`, `TestGetAllEntities_ReturnsJSONArray`) are largely redundant. Thin them (don't delete the tests — they still exercise the endpoints so enforce-mode runs), and correct the "server-is-source-of-truth" header comment to the per-finding reconciliation policy (§2).

**Files:**
- Modify: `internal/e2e/entity_conformance_test.go`.

- [ ] **Step 1: Fix the header comment** (l.1-6) — replace the `server-is-source-of-truth policy` wording:

```go
// entity_conformance_test.go — E2E tests that exercise the entity endpoints so
// the ambient openapivalidator (enforce mode) binds their response shapes. Where
// spec and server disagreed, each finding was reconciled per ADR 0003 Decision 7
// (fix whichever side left the contract), not by assuming the server is always
// right — see docs/superpowers/specs/2026-07-02-openapi-contract-reconciliation-design.md §2.
```

- [ ] **Step 2: Thin redundant assertions** — in `TestGetOneEntity_ReturnsEnvelope` and `TestGetAllEntities_ReturnsJSONArray`, keep the request + a minimal presence check (status 200, response decodes, has `type`/`data`/`meta`), and remove hand-rolled per-field meta assertions now covered by the enforce-mode `EntityMetadata` schema. Leave one comment noting the validator now enforces the meta shape. Do NOT remove tests that assert behaviour the schema can't (e.g. `TestUpdateSingle_EntityIdsIsArrayOfStrings` array-of-strings, `TestDeleteEntities_ResponseShape`).

- [ ] **Step 3: Run to confirm still green under enforce mode**

Run: `go test ./internal/e2e/ -run 'TestGetOneEntity_ReturnsEnvelope|TestGetAllEntities_ReturnsJSONArray' -v` (Docker)
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/e2e/entity_conformance_test.go
git commit -m "refactor(e2e): thin entity conformance seed now enforced by the validator; fix policy comment (§4.3)"
```

---

### Task 16: CHANGELOG + full-suite verification (Gate 5)

**Files:**
- Modify: `CHANGELOG.md` (Unreleased § Added/Changed/Fixed).

- [ ] **Step 1: Add CHANGELOG entries** — under `## [Unreleased]`:

```markdown
### Added

- **Conditional `deleteEntities`** — `DELETE /api/entity/{entityName}/{modelVersion}` now honours
  an `AbstractConditionDto` request body, deleting only matching entities (empty body ⇒ all).
  `verbose=true` returns the deleted ids; `numberOfEntitites` (matched) and
  `numberOfEntititesRemoved` (removed) are reported separately. Closes a data-loss defect where
  the condition was ignored and the whole model was wiped. New error code `INVALID_CONDITION` (400).
- **`getAllEntities` point-in-time** — the model-scoped list read honours `pointInTime`, returning
  entities as-at the supplied time and stamping `meta.pointInTime`.
- **OpenAPI error-code conformance** — the E2E conformance validator now enforces documented
  error codes (`errorCode` string granularity) for the entity endpoints, in addition to response
  shapes.

### Changed

- **Entity `meta` is typed-but-open** — `Envelope.meta` mirrors the canonical `EntityMetadata`
  (typed properties, never sealed); the obsolete `previousTransition` field is removed.
- **`changeType` spelling** — entity change records now use the canonical `CREATE/UPDATE/DELETE`
  across HTTP, gRPC, and the OpenAPI schema (HTTP previously emitted `CREATED/UPDATED/DELETED`).
- **gRPC entity `meta`** — now includes `modelKey` (and `pointInTime` when as-at), matching HTTP.
- Tightened the `create`/`createCollection` request-body schemas to their real shapes; documented
  unique-key `409`/`422` codes and reverse-chronological change ordering on the entity endpoints.
```

Keep it compact; the detail lives in the spec + PR. Note the `changeType` HTTP spelling change is
behaviour-visible — call it out under Changed.

- [ ] **Step 2: Verify no COMPATIBILITY / help-topic / README drift** — this slice bumps no SPI pin, no chart version, adds no env var. Confirm `TestErrCode_Parity` green (INVALID_CONDITION topic added in Task 7). No COMPATIBILITY.md change required (state so in the PR).

- [ ] **Step 3: Full verification (Gate 5)** — use `superpowers:verification-before-completion`:

```bash
go build ./...
go vet ./...
go test -short ./... -v          # fast unit pass
go test ./internal/e2e/... -v    # full E2E incl. enforce-mode gate + error-code matrix (Docker)
go test ./e2e/parity/memory/ ./e2e/parity/sqlite/ ./e2e/parity/postgres/ -v   # cross-backend (Docker)
go test ./internal/grpc/... -v
```

Then the plugin submodules (Go `./...` does not cross module boundaries):

```bash
make test-all        # root + plugins/memory|sqlite|postgres
```

Expected: all green. Confirm specifically: `TestOpenAPIConformanceReport` (marker gate),
`TestZZZErrorCodeMatrix` (producible + declared), `TestSpecHasNoSealedSchemas`, `TestErrCode_Parity`.

- [ ] **Step 4: Race detector one-shot (pre-PR, per `.claude/rules/race-testing.md`)**

```bash
make race
```

Expected: green (CI-parity scope; excludes `internal/e2e`).

- [ ] **Step 5: Commit**

```bash
git add CHANGELOG.md
git commit -m "docs(changelog): entity OpenAPI reconciliation slice (E1-E9 minus E6)"
```

---

## Coverage self-check (spec §8 matrix → tasks)

| Design item | Task(s) | Layers covered |
|---|---|---|
| Pillar A error-code matrix (producible + declared) | 1, 2 (+rows in 8, 11) | unit (collector, check funcs) + e2e aggregate |
| E1 meta typed-but-open (mirror canonical; no `additionalProperties:false`; no `previousTransition`) | 3 (HTTP+schema), 4 (gRPC), 5 (parity) | e2e + parity + gRPC |
| E2 conditional delete (matching subset) | 8 (impl+e2e), 9 (parity-in-tx + gRPC D2) | unit (selection) + e2e + parity-in-tx + gRPC coverage |
| E2 `pointInTime` + `verbose` | 8 | e2e + parity |
| E3 `getAllEntities` as-at (`GetAllAsAt`) | 6 | e2e + parity |
| E4 request-body shape | 10 | e2e + parity |
| E5 unique-key `409`/`422` | 11 | e2e (per code) + parity (existing) + gRPC (N1 envelope) |
| E7 changes ordering | 12 | e2e (gate-validated) |
| E8 `changeType` spelling (HTTP+gRPC+OpenAPI) | 13 | e2e enum + gRPC enum |
| E9 getOneEntity example | 3 | validated structurally by E1 |
| §6.3 gRPC `buildEntityMeta` modelKey+pointInTime | 4 | gRPC |
| Gate 4 new code `INVALID_CONDITION` + topic | 7 | `TestErrCode_Parity` |
| Gate 7 cloud-parity entries + E6/D2 open questions | 14 | doc |
| §4.3 seed cleanup + §2 policy comment | 15 | refactor |
| Deferred: E6 `fieldsChangedCount`, D2 gRPC conditional delete | — (recorded) | open questions (Task 14) |

**Concurrency:** no isolated concurrency test needed — conditional delete is select-then-delete
over a snapshot; parity asserts the surviving subset, not an interleave (spec §8).
