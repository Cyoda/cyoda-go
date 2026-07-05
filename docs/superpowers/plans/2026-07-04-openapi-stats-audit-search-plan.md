# OpenAPI stats/audit/search reconciliation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reconcile the Stats/Audit/Search area of `api/openapi.yaml` with the running server: one canonical `404 MODEL_NOT_FOUND` for unknown models, remove fictional/misplaced surface, fix mechanical spec-stale drift, and document intentionally-retained enums — with no dead code left behind.

**Architecture:** A single shared `common.EnsureModelRegistered` helper (bounded, multi-node-safe registry check) is applied at six model-scoped entry points. HTTP and gRPC inherit the fix through shared service methods; grouped-stats is fixed in its `app.go` resolver closure (also closing a latent multi-node bug). The rest is spec edits + targeted code-comment/dead-branch cleanup, each driven by a test.

**Tech Stack:** Go 1.26+, `log/slog`, testcontainers-go (Postgres e2e), kin-openapi enforce-mode validator, oasdiff CI gate.

## Global Constraints

- Go 1.26+; `log/slog` only (never `log.Printf`/`fmt.Printf`); wrap errors `fmt.Errorf("...: %w", err)`; `uuid.UUID` not `string`.
- 4xx = full domain detail + error code; 5xx = generic message + ticket UUID. `retryable:true` only for 409 tx-conflict.
- Schema policy: **typed-but-open** — never `additionalProperties:false`.
- Contract = cyoda-go leads; every Cloud-facing behaviour change → a `docs/cloud-parity/openapi-conformance.md` note.
- Multi-node is primary: the model-existence check must not falsely 404 a model just registered on a peer.
- No zombies: every removal cleans spec + code + comments + help topics + tests.
- TDD: red → green → commit per step. `go vet ./...` after signature changes (a plain `go build` skips test files). `make race` once before PR.
- No issue IDs in shipped artefacts (code/spec/help/response bodies); issue IDs only in commits/PR/plan/spec docs.
- Spec source of truth: `docs/superpowers/specs/2026-07-04-openapi-stats-audit-search-design.md`.
- **gRPC envelope assertion (Tasks 2-7):** cyoda-go maps operational (4xx) `AppError`s to
  `Error.Code == "CLIENT_ERROR"` with the domain code in `Error.Message` (`"MODEL_NOT_FOUND: detail"`),
  uniformly (`internal/grpc/errors.go` `buildErrorFields`). gRPC tests assert `Error.Code == "CLIENT_ERROR"`
  AND `Error.Message` contains `"MODEL_NOT_FOUND"` — NOT `Error.Code == "MODEL_NOT_FOUND"`. Do not change
  the envelope wrapper (issue #342 territory, out of scope).

---

## File structure

- **Create:** `internal/common/model_registration.go` (+ `_test.go`) — the shared helper.
- **Modify (guards):** `internal/domain/entity/service.go` (ListEntities, GetStatisticsForModel, GetStatisticsByStateForModel); `internal/domain/search/service.go` (Search, SubmitAsync + admit-branch/comment cleanup); `app/app.go` (grouped-stats resolver); `internal/domain/entity/grouped_stats_handler.go` (404 mapping).
- **Modify (spec):** `api/openapi.yaml` — grouped-stats 404, stats/list 404, searchEntities (drop application/json + timeoutMillis + 408 + timeout prose, document limit>10000 400), getAsyncSearchResults (drop pointInTime, page-size prose), getStateMachineFinishedEvent (drop v1-UUID prose + 400, ProblemDetail).
- **Modify (comments/docs):** `internal/domain/search/service.go:57`, `internal/domain/audit/handler.go:30` (do-not-remove notes); `cmd/cyoda/help/content/crud.md`; `docs/cloud-parity/openapi-conformance.md`; `CHANGELOG.md`.
- **Tests:** new e2e in `internal/e2e/`, gRPC envelope in `internal/grpc/`, one parity scenario in `e2e/parity/` + registry, isolated cluster test, matrix rows in `internal/e2e/zzz_errorcode_matrix_test.go`.

---

## Task 1: Shared `EnsureModelRegistered` helper

**Files:**
- Create: `internal/common/model_registration.go`
- Test: `internal/common/model_registration_test.go`

**Interfaces:**
- Consumes: `spi.ModelStore`, `spi.ModelRef`, `spi.ErrNotFound`, `common.AppError`, `common.Operational`, `common.Internal`, `common.ErrCodeModelNotFound`.
- Produces: `func EnsureModelRegistered(ctx context.Context, ms spi.ModelStore, ref spi.ModelRef) *AppError` — returns nil if registered (possibly after one bounded refresh), `Operational(404, MODEL_NOT_FOUND)` if not, `Internal(...)` on a non-NotFound store error.

- [ ] **Step 1: Write the failing test**

Create `internal/common/model_registration_test.go`. Use a small fake `spi.ModelStore` (only `Get` matters; other methods can panic/return zero). Add a second fake embedding the first plus a `RefreshAndGet` method to exercise the bounded-refresh path.

```go
package common

import (
	"context"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type fakeModelStore struct {
	spi.ModelStore // embed for unused methods; nil is fine (never called)
	getErr         error
}

func (f fakeModelStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &spi.ModelDescriptor{}, nil
}

type refreshingStore struct {
	fakeModelStore
	refreshErr error
}

func (r refreshingStore) RefreshAndGet(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	if r.refreshErr != nil {
		return nil, r.refreshErr
	}
	return &spi.ModelDescriptor{}, nil
}

func TestEnsureModelRegistered(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	if err := EnsureModelRegistered(context.Background(), fakeModelStore{}, ref); err != nil {
		t.Fatalf("registered model: want nil, got %v", err)
	}

	// Not found, no refresh capability → 404.
	err := EnsureModelRegistered(context.Background(), fakeModelStore{getErr: spi.ErrNotFound}, ref)
	if err == nil || err.Status != http.StatusNotFound || err.Code != ErrCodeModelNotFound {
		t.Fatalf("unknown model: want 404 MODEL_NOT_FOUND, got %v", err)
	}

	// Not found in cache but refresh finds it → nil (multi-node peer registration).
	if err := EnsureModelRegistered(context.Background(), refreshingStore{fakeModelStore: fakeModelStore{getErr: spi.ErrNotFound}}, ref); err != nil {
		t.Fatalf("peer-registered model: want nil after refresh, got %v", err)
	}

	// Not found in cache and refresh also not found → 404.
	err = EnsureModelRegistered(context.Background(), refreshingStore{fakeModelStore: fakeModelStore{getErr: spi.ErrNotFound}, refreshErr: spi.ErrNotFound}, ref)
	if err == nil || err.Status != http.StatusNotFound {
		t.Fatalf("refresh-miss: want 404, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/common/ -run TestEnsureModelRegistered -v`
Expected: FAIL — `EnsureModelRegistered` undefined.

- [ ] **Step 3: Write the helper**

Create `internal/common/model_registration.go`:

```go
package common

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// EnsureModelRegistered returns a 404 MODEL_NOT_FOUND AppError when ref names a
// model that is not registered for the caller's tenant, and nil otherwise.
//
// It performs at most one bounded RefreshAndGet (singleflight-collapsed and
// negative-cached inside the caching model store) so a model just registered on
// a peer node is not falsely rejected — mirroring the write path's
// ValidateWithRefresh and the search path-validator's one-shot refresh. When the
// store has no RefreshAndGet (single-node / memory), the cached Get is
// authoritative.
func EnsureModelRegistered(ctx context.Context, ms spi.ModelStore, ref spi.ModelRef) *AppError {
	_, err := ms.Get(ctx, ref)
	if err == nil {
		return nil
	}
	if !errors.Is(err, spi.ErrNotFound) {
		return Internal("failed to check model registration", err)
	}

	if refresher, ok := ms.(interface {
		RefreshAndGet(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error)
	}); ok {
		if _, rerr := refresher.RefreshAndGet(ctx, ref); rerr == nil {
			return nil
		}
	}

	return Operational(http.StatusNotFound, ErrCodeModelNotFound,
		fmt.Sprintf("model %s/%s not found", ref.EntityName, ref.ModelVersion))
}
```

Note: confirm `AppError` exposes `Status` and `Code` fields as used in the test; if the field names differ (check `internal/common/errors.go`), align the test assertions to the real field names.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/common/ -run TestEnsureModelRegistered -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/common/model_registration.go internal/common/model_registration_test.go
git commit -m "feat(common): EnsureModelRegistered — bounded multi-node-safe model existence check

Refs #369"
```

---

## Task 2: getAllEntities → 404 on unknown model

**Files:**
- Modify: `internal/domain/entity/service.go` (`ListEntities`, ~957)
- Test: `internal/e2e/entity_list_unknown_model_test.go` (create), `internal/grpc/search_test.go` (add gRPC case)

**Interfaces:**
- Consumes: `common.EnsureModelRegistered` (Task 1); `h.factory.ModelStore(ctx)`.

- [ ] **Step 1: Write the failing e2e test**

Mirror the assertion style of `internal/e2e/entity_delete_conditional_test.go:79` (`commontest.ExpectErrorCode(t, resp, "MODEL_NOT_FOUND")`). Create `internal/e2e/entity_list_unknown_model_test.go`:

```go
func TestGetAllEntities_UnknownModel_404(t *testing.T) {
	h := newHarness(t) // use the package's standard harness constructor; see existing tests
	resp := h.GET(t, "/entity/DoesNotExist/1")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "MODEL_NOT_FOUND")
}
```

Use the exact harness/helper names the neighbouring e2e tests use (open `entity_delete_conditional_test.go` for the request+assert helpers; do not invent new ones).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/e2e/ -run TestGetAllEntities_UnknownModel_404 -v`
Expected: FAIL — returns 200 with an empty list.

- [ ] **Step 3: Add the guard**

In `ListEntities` (`internal/domain/entity/service.go:957`), after obtaining the `ModelStore` and building `ref`, before the `entityStore.GetAll`/`GetAllAsAt` call:

```go
	modelStore, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}
	if appErr := common.EnsureModelRegistered(ctx, modelStore, ref); appErr != nil {
		return nil, appErr
	}
```

(Place after `ref` is constructed at ~972-975. `ListEntities` currently obtains only the entity store; add the model-store fetch.)

- [ ] **Step 4: Run e2e + vet**

Run: `go test ./internal/e2e/ -run TestGetAllEntities_UnknownModel_404 -v && go vet ./internal/domain/entity/...`
Expected: PASS; vet clean.

- [ ] **Step 5: Add the gRPC envelope case**

In `internal/grpc/search_test.go`, mirror an existing envelope assertion (search for `resp.Error` / `Error.Code`). Add a case: list via the gRPC `EntitySearchRequest`/list path for `DoesNotExist/1`, assert the CloudEvent envelope `Error.Code == "MODEL_NOT_FOUND"`.

- [ ] **Step 6: Run gRPC test**

Run: `go test ./internal/grpc/ -run Test... -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/domain/entity/service.go internal/e2e/entity_list_unknown_model_test.go internal/grpc/search_test.go
git commit -m "feat(entity): getAllEntities returns 404 MODEL_NOT_FOUND on unknown model

Refs #369"
```

---

## Task 3: getEntityStatisticsForModel → 404

**Files:**
- Modify: `internal/domain/entity/service.go` (`GetStatisticsForModel`, 594)
- Test: `internal/e2e/entity_stats_unknown_model_test.go` (create), `internal/grpc/search_test.go`

**Interfaces:** Consumes `common.EnsureModelRegistered`, `h.factory.ModelStore(ctx)`.

- [ ] **Step 1: Write the failing e2e test**

```go
func TestGetStatisticsForModel_UnknownModel_404(t *testing.T) {
	h := newHarness(t)
	resp := h.GET(t, "/entity/stats/DoesNotExist/1")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "MODEL_NOT_FOUND")
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/e2e/ -run TestGetStatisticsForModel_UnknownModel_404 -v`
Expected: FAIL — returns 200 `{count:0}`.

- [ ] **Step 3: Add the guard**

In `GetStatisticsForModel` (`service.go:594`), after building `ref` and before `entityStore.Count`:

```go
	modelStore, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}
	if appErr := common.EnsureModelRegistered(ctx, modelStore, ref); appErr != nil {
		return nil, appErr
	}
```

- [ ] **Step 4: Run e2e**

Run: `go test ./internal/e2e/ -run TestGetStatisticsForModel_UnknownModel_404 -v`
Expected: PASS.

- [ ] **Step 5: Add gRPC envelope case** for `EntityStatsGetRequest` on `DoesNotExist/1` → `Error.Code == "MODEL_NOT_FOUND"` (mirror `search.go:397` path).

- [ ] **Step 6: Run gRPC test** — Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/domain/entity/service.go internal/e2e/entity_stats_unknown_model_test.go internal/grpc/search_test.go
git commit -m "feat(entity): getEntityStatisticsForModel returns 404 on unknown model

Refs #369"
```

---

## Task 4: getEntityStatisticsByStateForModel → 404

**Files:**
- Modify: `internal/domain/entity/service.go` (`GetStatisticsByStateForModel`, 556)
- Test: append to `internal/e2e/entity_stats_unknown_model_test.go`, `internal/grpc/search_test.go`

**Interfaces:** Consumes `common.EnsureModelRegistered`.

- [ ] **Step 1: Write the failing e2e test**

```go
func TestGetStatisticsByStateForModel_UnknownModel_404(t *testing.T) {
	h := newHarness(t)
	resp := h.GET(t, "/entity/stats/states/DoesNotExist/1")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "MODEL_NOT_FOUND")
}
```

- [ ] **Step 2: Run to verify it fails** — Expected: FAIL (200 `[]`).

- [ ] **Step 3: Add the guard** in `GetStatisticsByStateForModel` (`service.go:556`), after `ref`, before `entityStore.CountByState` (same 4-line block as Task 3).

- [ ] **Step 4: Run e2e** — Expected: PASS.

- [ ] **Step 5: Add gRPC case** for `EntityStatsByStateGetRequest` (mirror `search.go:471`).

- [ ] **Step 6: Run gRPC test** — Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/domain/entity/service.go internal/e2e/entity_stats_unknown_model_test.go internal/grpc/search_test.go
git commit -m "feat(entity): getEntityStatisticsByStateForModel returns 404 on unknown model

Refs #369"
```

---

## Task 5: searchEntities → 404 + remove the dead admit-branch

**Files:**
- Modify: `internal/domain/search/service.go` (`Search` ~122; delete admit-branch at `:529-532`; fix comment `:496-500`, `:128-131`)
- Test: `internal/e2e/search_unknown_model_test.go` (create), `internal/grpc/search_test.go`

**Interfaces:** Consumes `common.EnsureModelRegistered`, `s.factory.ModelStore(ctx)`.

- [ ] **Step 1: Write the failing e2e test**

```go
func TestSearchEntities_UnknownModel_404(t *testing.T) {
	h := newHarness(t)
	resp := h.POST(t, "/search/direct/DoesNotExist/1", `{"type":"group","operator":"AND","conditions":[]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "MODEL_NOT_FOUND")
}
```

Use the exact empty/valid condition body the neighbouring search e2e tests use.

- [ ] **Step 2: Run to verify it fails** — Expected: FAIL (200, empty ndjson stream).

- [ ] **Step 3: Add the guard at the top of `Search`**

In `SearchService.Search` (`service.go:122`), as the first step (before `validateConditionPaths`):

```go
	modelStore, err := s.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}
	if appErr := common.EnsureModelRegistered(ctx, modelStore, modelRef); appErr != nil {
		return nil, appErr
	}
```

- [ ] **Step 4: Delete the now-dead admit-branch and fix stale comments**

In `validateConditionPaths` (`service.go:527-541`), the `errors.Is(err, spi.ErrNotFound) { return nil }` branch is unreachable for the unregistered case (existence is gated up front). Remove that branch, keeping the schema-decode-failure fallback:

```go
	fields, err := loadFieldsMap(ctx, modelStore, modelRef)
	if err != nil {
		// Model existence is guaranteed by EnsureModelRegistered before we
		// reach here; a schema-decode failure is upstream — log and proceed
		// so the matcher's own error path can still surface a useful error.
		slog.Debug("failed to load schema for pre-execution validation",
			"pkg", "search",
			"entityName", modelRef.EntityName,
			"modelVersion", modelRef.ModelVersion,
			"error", err)
		return nil
	}
```

Update the function doc (`service.go:496-500`) to drop the "model has not been registered … search must preserve that behaviour" clause. Also fix the `loadFieldsMap` doc (`path_validate.go:128-131`) — it no longer needs to "preserve" unregistered-admit at the search boundary (the refresh path can still legitimately return ErrNotFound on a deleted-mid-flight model, so keep that handling, just drop the "admit" framing).

- [ ] **Step 5: Run e2e + full search unit tests + vet**

Run: `go test ./internal/domain/search/... ./internal/e2e/ -run 'Search' -v && go vet ./internal/domain/search/...`
Expected: PASS; vet clean. (Confirm no existing search unit test asserted "unknown model admitted / empty result" — if one does, it encodes the old contract and must be updated to expect 404. Search for it: `grep -rn "unregistered\|unknown model\|admit" internal/domain/search/*_test.go`.)

- [ ] **Step 6: Add gRPC envelope case** — direct search (`EntitySearchRequest`) on `DoesNotExist/1` → `Error.Code == "MODEL_NOT_FOUND"`.

- [ ] **Step 7: Run gRPC test** — Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/domain/search/service.go internal/domain/search/path_validate.go internal/e2e/search_unknown_model_test.go internal/grpc/search_test.go
git commit -m "feat(search): searchEntities returns 404 on unknown model; drop dead admit-branch

Refs #369"
```

---

## Task 6: submitAsyncSearchJob → 404

**Files:**
- Modify: `internal/domain/search/service.go` (`SubmitAsync` ~214)
- Test: append to `internal/e2e/search_unknown_model_test.go`, `internal/grpc/search_test.go`

**Interfaces:** Consumes `common.EnsureModelRegistered`.

- [ ] **Step 1: Write the failing e2e test**

```go
func TestSubmitAsyncSearchJob_UnknownModel_404(t *testing.T) {
	h := newHarness(t)
	resp := h.POST(t, "/search/async/DoesNotExist/1", `{"type":"group","operator":"AND","conditions":[]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "MODEL_NOT_FOUND")
}
```

- [ ] **Step 2: Run to verify it fails** — Expected: FAIL (200 with a jobId).

- [ ] **Step 3: Add the guard at the top of `SubmitAsync`**

In `SubmitAsync` (`service.go:214`), after the user-context check (`:215-218`) and before `validateConditionPaths`:

```go
	modelStore, err := s.factory.ModelStore(ctx)
	if err != nil {
		return "", common.Internal("failed to access model store", err)
	}
	if appErr := common.EnsureModelRegistered(ctx, modelStore, modelRef); appErr != nil {
		return "", appErr
	}
```

(`SubmitAsync` returns `(string, error)`; return `""` with the AppError.)

- [ ] **Step 4: Run e2e** — Expected: PASS.

- [ ] **Step 5: Add gRPC case** — async submit (`EntitySnapshotSearchRequest`, `search.go:161`) on `DoesNotExist/1` → `Error.Code == "MODEL_NOT_FOUND"`.

- [ ] **Step 6: Run gRPC test** — Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/domain/search/service.go internal/e2e/search_unknown_model_test.go internal/grpc/search_test.go
git commit -m "feat(search): submitAsyncSearchJob returns 404 on unknown model

Refs #369"
```

---

## Task 7: grouped-stats → 404 MODEL_NOT_FOUND; retire UNKNOWN_MODEL

**Files:**
- Modify: `app/app.go` (resolver closure ~618-637), `internal/domain/entity/grouped_stats_handler.go` (~137), `cmd/cyoda/help/content/crud.md` (536, 621), `api/openapi.yaml` (grouped-stats op ~1132: add 404, drop UNKNOWN_MODEL from 400 enum at ~1130)
- Test: `internal/domain/entity/grouped_stats_handler_test.go:108` (flip), `internal/e2e/grouped_stats_test.go` (add/confirm 404 case)

**Interfaces:** Consumes `common.EnsureModelRegistered`.

- [ ] **Step 1: Flip the failing unit test**

In `grouped_stats_handler_test.go:108-109`, change the expectation:

```go
	if got := decodeProblemErrorCode(t, rec.Body.Bytes()); got != "MODEL_NOT_FOUND" {
		t.Fatalf("errorCode=%s, want MODEL_NOT_FOUND (body: %s)", got, rec.Body.String())
	}
```

Also update the asserted status in that test to `http.StatusNotFound` (find the `rec.Code`/status assertion in the same test function and change `400` → `404`).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/domain/entity/ -run TestGroupedStats -v` (use the exact test name)
Expected: FAIL — still gets `UNKNOWN_MODEL`/400.

- [ ] **Step 3: Add bounded refresh in the resolver**

In `app/app.go`, replace the `modelStore.Get` + `ErrNotFound` block in `groupedStatsResolver` (~628-633) with the shared helper:

```go
		ref := spi.ModelRef{EntityName: entityName, ModelVersion: modelVersion}
		if appErr := common.EnsureModelRegistered(ctx, modelStore, ref); appErr != nil {
			// Not registered (after one bounded refresh) → resolver reports
			// ok=false; the handler maps that to 404 MODEL_NOT_FOUND.
			return nil, ref, false, nil
		}
```

Update the closure's doc comment (~611-617) to say the handler maps `ok=false` to `404 MODEL_NOT_FOUND` (not `400 UNKNOWN_MODEL`), and note the refresh closes the multi-node stale-cache race.

- [ ] **Step 4: Change the handler mapping**

In `grouped_stats_handler.go:137-141`:

```go
	if !ok {
		common.WriteError(w, r, common.Operational(
			http.StatusNotFound, common.ErrCodeModelNotFound,
			"model not found",
		))
		return
	}
```

Update the comment at `grouped_stats_handler.go:24` (`400 UNKNOWN_MODEL` → `404 MODEL_NOT_FOUND`).

- [ ] **Step 5: Run unit + vet**

Run: `go test ./internal/domain/entity/ -run TestGroupedStats -v && go vet ./app/... ./internal/domain/entity/...`
Expected: PASS; vet clean.

- [ ] **Step 6: Update spec + help doc**

`api/openapi.yaml`: in the grouped-stats operation (`queryGroupedEntityStatisticsForModel`, ~1132), (a) remove `UNKNOWN_MODEL` from the `400` error-code enumeration prose (~1130), (b) add a `404` response block referencing `ProblemDetail` (description "Entity model not found"). `cmd/cyoda/help/content/crud.md`: remove `UNKNOWN_MODEL` at :536 and :621, adding a `404 MODEL_NOT_FOUND` note for the grouped-stats endpoint.

- [ ] **Step 7: Add/confirm the e2e 404 case**

In `internal/e2e/grouped_stats_test.go`, add:

```go
func TestGroupedStats_UnknownModel_404(t *testing.T) {
	h := newHarness(t)
	resp := h.POST(t, "/entity/stats/DoesNotExist/1/query", `{"groupBy":["x"]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "MODEL_NOT_FOUND")
}
```

Use the minimal valid grouped-stats body the existing tests use.

- [ ] **Step 8: Run e2e + confirm UNKNOWN_MODEL is fully gone**

Run: `go test ./internal/e2e/ -run TestGroupedStats -v && grep -rn "UNKNOWN_MODEL" internal/ app/ api/openapi.yaml cmd/cyoda/help/content/ | grep -v docs/`
Expected: PASS; grep returns **no** matches outside historical `docs/`.

- [ ] **Step 9: Commit**

```bash
git add app/app.go internal/domain/entity/grouped_stats_handler.go internal/domain/entity/grouped_stats_handler_test.go internal/e2e/grouped_stats_test.go cmd/cyoda/help/content/crud.md api/openapi.yaml
git commit -m "feat(stats): grouped-stats returns 404 MODEL_NOT_FOUND; retire UNKNOWN_MODEL; fix latent multi-node race

Refs #369"
```

---

## Task 8: cross-backend parity + isolated multi-node bounded-refresh test

**Files:**
- Create: `e2e/parity/unknown_model_test.go` + register in `e2e/parity/registry.go`
- Create: an isolated cluster test (place with the existing cluster/multi-node e2e; e.g. `internal/e2e/multinode_unknown_model_test.go`)

**Interfaces:** Uses the parity harness (see a neighbouring `e2e/parity/*_test.go` for the backend-parameterised runner + `registry.go` registration).

- [ ] **Step 1: Write the parity scenario**

One backend-agnostic scenario asserting the unknown-model rule on a representative op (e.g. `GET /entity/stats/DoesNotExist/1` → 404 MODEL_NOT_FOUND), following the exact shape of an existing parity test. Register it in `registry.go` so memory/sqlite/postgres/commercial all run it.

- [ ] **Step 2: Run parity** — `go test ./e2e/parity/ -run UnknownModel -v` (per the suite's invocation) — Expected: PASS on all registered backends.

- [ ] **Step 3: Write the isolated multi-node test**

Scenario: register a model on node A; issue a model-scoped data op (e.g. searchEntities) against node B whose cache is cold → `EnsureModelRegistered`'s bounded `RefreshAndGet` must make it succeed (no false 404). Mirror the existing multi-node/cluster test harness (search `internal/e2e` for `TestMultiNode`/two-node setups). Assert the op succeeds (not 404). This is NOT added to the parity suite.

- [ ] **Step 4: Run the cluster test** — Expected: PASS (op succeeds; no false 404).

- [ ] **Step 5: Commit**

```bash
git add e2e/parity/unknown_model_test.go e2e/parity/registry.go internal/e2e/multinode_unknown_model_test.go
git commit -m "test(search): cross-backend parity + isolated multi-node bounded-refresh for unknown-model 404

Refs #369"
```

---

## Task 9: Error-code matrix rows

**Files:**
- Modify: `internal/e2e/zzz_errorcode_matrix_test.go`

**Interfaces:** The matrix is the declared-vs-produced checklist (entity-slice learning: per-op completeness; run the WHOLE e2e suite to validate; exempt non-deterministic codes).

- [ ] **Step 1: Add the ops + rows**

Add matrix entries for the newly-declared `404 MODEL_NOT_FOUND` on getAllEntities, getEntityStatisticsForModel, getEntityStatisticsByStateForModel, searchEntities, submitAsyncSearchJob, and queryGroupedEntityStatisticsForModel — following the existing `{Status: 404, Code: "MODEL_NOT_FOUND"}` row shape (`zzz_errorcode_matrix_test.go:37`). Ensure each added op declares its FULL emitted error surface (per-op completeness); CONFLICT / non-deterministic codes stay exempt from the `declared` check.

- [ ] **Step 2: Run the WHOLE e2e suite**

Run: `go test ./internal/e2e/ -v`
Expected: PASS — the matrix has no undeclared/unproduced gaps for the touched ops.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/zzz_errorcode_matrix_test.go
git commit -m "test(e2e): declare 404 MODEL_NOT_FOUND in the error-code matrix for stats/search/list ops

Refs #369"
```

---

## Task 10: Remove `pointInTime` param from getAsyncSearchResults

**Files:**
- Modify: `api/openapi.yaml` (getAsyncSearchResults, `GET /search/async/{jobId}`)

**Interfaces:** none (spec-only; handler never reads it).

- [ ] **Step 1: Confirm no code reads it**

Run: `grep -rn "PointInTime" internal/domain/search/handler.go`
Expected: matches only at `:128`, `:220` (sync search + submit) — NOT in `GetAsyncSearchResults` (`:274-352`). If a `params.PointInTime` reference exists in the paging handler, stop and reassess.

- [ ] **Step 2: Remove the param from the spec**

In `api/openapi.yaml`, delete the `pointInTime` query parameter block from the `getAsyncSearchResults` operation only. Leave `pointInTime` on searchEntities and submitAsyncSearchJob untouched.

- [ ] **Step 3: Regenerate + build + validate**

Run: `go generate ./... 2>/dev/null; go build ./... && go test ./internal/e2e/ -run 'Async' -v`
Expected: builds; the generated `GetAsyncSearchResultsParams` no longer carries `PointInTime`; async tests pass. (`api/openapi.yaml` is `//go:embed`'d — no snapshot to regenerate; if codegen is wired, the params struct updates.)

- [ ] **Step 4: Commit**

```bash
git add api/openapi.yaml api/generated.go
git commit -m "fix(openapi): drop pointInTime from getAsyncSearchResults — captured at submit, ignored on paging

Refs #369"
```

---

## Task 11: getStateMachineFinishedEvent — remove fictional v1-UUID 400; ProblemDetail envelope

**Files:**
- Modify: `api/openapi.yaml` (getStateMachineFinishedEvent, ~487-560)
- Test: `internal/e2e/audit_finished_event_test.go` (create or extend)

**Interfaces:** none (handler already accepts any UUID; spec-only + a proving test).

- [ ] **Step 1: Write the proving e2e test**

Assert a well-formed **non-v1** UUID is accepted (not rejected with 400) — it yields 404 (no matching finished event) or 200, never 400:

```go
func TestGetStateMachineFinishedEvent_NonV1UUID_NotRejected(t *testing.T) {
	h := newHarness(t)
	// random v4 UUIDs (not time-based)
	resp := h.GET(t, "/audit/entity/00000000-0000-4000-8000-000000000000/workflow/00000000-0000-4000-8000-000000000001/finished")
	if resp.StatusCode == http.StatusBadRequest {
		t.Fatalf("non-v1 UUID rejected with 400; want 404/200 (handler does no version check)")
	}
}
```

- [ ] **Step 2: Run to verify current behaviour** — Expected: PASS already (handler does no v1 check). This test is a regression guard proving the spec's `400` is fictional; it does not require code change.

- [ ] **Step 3: Fix the spec**

In `api/openapi.yaml` (getStateMachineFinishedEvent): remove the "must be a valid time-based UUID (version 1)" prose (description ~502-503 and both param descriptions ~508-509, ~516-517); delete the `400` response block (~529-534); change this operation's remaining error responses (`401/403/404/500`) from `ErrorResponseDto` to `ProblemDetail` (`#/components/schemas/ProblemDetail`).

- [ ] **Step 4: Validate spec + e2e**

Run: `go test ./internal/e2e/ -run 'FinishedEvent|StateMachine' -v`
Expected: PASS; the enforce-mode validator accepts the ProblemDetail bodies.

- [ ] **Step 5: Commit**

```bash
git add api/openapi.yaml internal/e2e/audit_finished_event_test.go
git commit -m "fix(openapi): getStateMachineFinishedEvent — drop fictional v1-UUID 400; ProblemDetail envelope

Refs #369"
```

---

## Task 12: searchEntities — remove fictional timeoutMillis / 408 / timeout prose

**Files:**
- Modify: `api/openapi.yaml` (searchEntities: params ~6718-6724, response ~6783-6790, prose ~6656, ~6686)

**Interfaces:** none (spec-only; the impl is tracked in #372).

- [ ] **Step 1: Remove the fictional surface**

In `api/openapi.yaml` (searchEntities): delete the `timeoutMillis` query parameter (~6718-6724); delete the `408` response block (~6783-6790); remove the timeout prose bullets ("Has configurable timeout…" ~6656, "Default timeout: 60000 milliseconds" ~6686). Leave `404` and the unknown-model semantics (Task 5) intact.

- [ ] **Step 2: Build + validate**

Run: `go build ./... && go test ./internal/e2e/ -run 'Search' -v`
Expected: PASS; the generated `SearchEntitiesParams` no longer carries `TimeoutMillis` (confirm nothing referenced it: `grep -rn "TimeoutMillis" internal/` → no matches).

- [ ] **Step 3: Commit**

```bash
git add api/openapi.yaml api/generated.go
git commit -m "fix(openapi): remove fictional timeoutMillis/408 from searchEntities (impl tracked in #372)

Refs #369, #372"
```

---

## Task 13: searchEntities — x-ndjson only + document limit>10000 400

**Files:**
- Modify: `api/openapi.yaml` (searchEntities 200 content ~6752-6764; limit prose ~6681-6682; error content ~6767-6773)
- Test: `internal/e2e/search_content_type_test.go` (create)

**Interfaces:** none (server already correct).

- [ ] **Step 1: Write the failing/confirming e2e tests**

```go
func TestSearchEntities_ContentTypeIsNdjson(t *testing.T) {
	h := newHarness(t)
	// register a model + import so the search is valid; reuse existing helpers
	resp := h.POST(t, "/search/direct/"+model+"/1", `{"type":"group","operator":"AND","conditions":[]}`)
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("Content-Type = %q, want application/x-ndjson", ct)
	}
}

func TestSearchEntities_LimitOver10000_400(t *testing.T) {
	h := newHarness(t)
	resp := h.POST(t, "/search/direct/"+model+"/1?limit=10001", `{"type":"group","operator":"AND","conditions":[]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run** — Expected: PASS already (server streams ndjson; rejects limit>10000 at `handler.go:150`). These pin the real behaviour before the spec edit.

- [ ] **Step 3: Fix the spec**

In `api/openapi.yaml` (searchEntities): remove the `application/json` variant from the `200` response (keep only `application/x-ndjson`); remove `application/x-ndjson` from the error responses (errors are `problem+json` only); replace the "silently limit the result to 10000" prose (~6681-6682) with a statement that `limit>10000` is rejected with `400`.

- [ ] **Step 4: Verify gRPC limit-cap parity (open item from the spec)**

Run: `grep -rn "MaxPageSize\|limit\|Limit" internal/grpc/search.go`
Determine whether gRPC direct search enforces the same `limit>10000` cap. If it does NOT, add a task-local fix so the cap lives at the service layer (both entry points), or record the asymmetry explicitly in the spec's cloud-parity note — do not leave it silently divergent (coherence mandate).

- [ ] **Step 5: Validate** — `go test ./internal/e2e/ -run 'Search' -v` — Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add api/openapi.yaml internal/e2e/search_content_type_test.go
git commit -m "fix(openapi): searchEntities is x-ndjson only; document limit>10000 400

Refs #369"
```

---

## Task 14: getAsyncSearchResults — page-size prose 10 → 1000

**Files:**
- Modify: `api/openapi.yaml` (getAsyncSearchResults `pageSize` description)

- [ ] **Step 1: Fix the prose**

In `api/openapi.yaml`, change the `getAsyncSearchResults` `pageSize` default from `10` to `1000` (matches `handler.go:277`).

- [ ] **Step 2: Build/validate** — `go build ./...` — Expected: OK.

- [ ] **Step 3: Commit**

```bash
git add api/openapi.yaml
git commit -m "docs(openapi): getAsyncSearchResults default page size 10 -> 1000

Refs #369"
```

---

## Task 15: Document intentionally-retained enums in code

**Files:**
- Modify: `internal/domain/search/service.go:57`, `internal/domain/audit/handler.go:30`

**Interfaces:** none (comments only).

- [ ] **Step 1: Add the do-not-remove notes**

`search/service.go:57` (`SearchJobStatus.Status` field doc): expand to note `NOT_FOUND` is emitted by the commercial self-executing search store on snapshot-expiry races (documented in the spec) and is intentionally retained — do not remove from the contract even though OSS never sets it.

`audit/handler.go:30`: expand the `System` comment to note `System` is a reserved/commercial audit source retained in the contract — do not remove from the `eventType` enum even though OSS never emits it.

- [ ] **Step 2: Build** — `go build ./...` — Expected: OK.

- [ ] **Step 3: Commit**

```bash
git add internal/domain/search/service.go internal/domain/audit/handler.go
git commit -m "docs(search,audit): mark NOT_FOUND/System as intentionally-retained commercial enum values

Refs #369"
```

---

## Task 16: Cloud-parity + Gate-4 docs

**Files:**
- Modify: `docs/cloud-parity/openapi-conformance.md`, `cmd/cyoda/help/content/crud.md`, `CHANGELOG.md`

- [ ] **Step 1: Cloud-parity note**

Append to `docs/cloud-parity/openapi-conformance.md`: (a) unknown-model → `404 MODEL_NOT_FOUND` is the canonical contract across list/stats/search/grouped-stats (Cloud mirrors; was divergent — silent-empty and `400 UNKNOWN_MODEL`); (b) `searchEntityAuditEvents.changes` before/after diff is a documented gap (deferred feature); (c) `NOT_FOUND` search-job status retained because the commercial store emits it.

- [ ] **Step 2: Help-topic hygiene**

Confirm `cmd/cyoda/help/content/crud.md` reflects the new `404 MODEL_NOT_FOUND` for the stats/search/list endpoints and no longer mentions `UNKNOWN_MODEL` (already edited in Task 7 — verify here). Note the deferred message-op v1-UUID cleanup as a follow-on so the doc is internally consistent.

- [ ] **Step 3: CHANGELOG**

Add an entry under the current release section summarising: unknown-model unification to `404 MODEL_NOT_FOUND`; removal of fictional `timeoutMillis`/`408` (→ #372), `pointInTime` on async results, and the finished-event v1-UUID `400`; searchEntities `x-ndjson`-only.

- [ ] **Step 4: Commit**

```bash
git add docs/cloud-parity/openapi-conformance.md cmd/cyoda/help/content/crud.md CHANGELOG.md
git commit -m "docs: cloud-parity + changelog for stats/audit/search reconciliation

Refs #369"
```

---

## Task 17: oasdiff gate + final verification

**Files:**
- Modify (if needed): `.github/oasdiff-err-ignore.txt`

- [ ] **Step 1: Run the oasdiff gate locally**

Run the same oasdiff check CI runs (see `.github/workflows` for the exact invocation comparing against the base spec). For each edit flagged breaking — `application/json` removal on searchEntities 200, `timeoutMillis`/`pointInTime` param removals, `408`/finished-event `400` response removals — add a surgical entry to `.github/oasdiff-err-ignore.txt` with a one-line rationale (fail-closed; the E4 pattern). Additive changes (new 404s) need no entry.

- [ ] **Step 2: Full verification sweep**

Run:
```bash
go test ./... -v            # root incl. e2e (Docker up)
go vet ./...
make test-short-all         # plugin submodules
make race                   # once, CI-parity scope
```
Expected: all green; vet clean.

- [ ] **Step 3: Dead-code sweep confirmation**

Run:
```bash
grep -rn "UNKNOWN_MODEL" internal/ app/ api/openapi.yaml cmd/ | grep -v docs/     # empty
grep -rn "TimeoutMillis" internal/                                                # empty
grep -rn "timeoutMillis\|Default timeout\|configurable timeout" api/openapi.yaml  # empty (searchEntities)
```
Expected: no matches (all removals cleaned both sides).

- [ ] **Step 4: Commit**

```bash
git add .github/oasdiff-err-ignore.txt
git commit -m "ci(oasdiff): document reconciliation removals for the additive-only gate

Refs #369"
```

---

## Self-review notes (author)

- **Spec coverage:** §2 → Tasks 1-9; §3 → Tasks 10-12; §4 → Tasks 13-14; §5 → Task 15; §6 → Task 16; §7 error table → Tasks 2-7,11-13; §8 matrix → Tasks 2-9; §9 oasdiff → Task 17; §10 cloud-parity → Task 16; §11 sweep → Tasks 7,12,17.
- **No new error codes** (MODEL_NOT_FOUND exists; UNKNOWN_MODEL retired) — so no `errors/<CODE>.md` task; `TestErrCode_Parity` unaffected (verified: UNKNOWN_MODEL has no topic).
- **Type consistency:** `common.EnsureModelRegistered(ctx, spi.ModelStore, spi.ModelRef) *common.AppError` used identically in Tasks 2-7.
- **Open verify items surfaced, not buried:** gRPC `limit>10000` cap (Task 13 Step 4); `AppError` field names (Task 1 Step 3).
