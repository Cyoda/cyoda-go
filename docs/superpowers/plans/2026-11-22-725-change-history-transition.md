# Transition Name on Entity Change History — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every entity change-history entry carries the transition its request named, the value is right on every door and backend, and the help's segregation-of-duties recipe reads it.

**Architecture:** The SPI's `EntityVersionMeta` gains `Transition`; each in-tree store already keeps the name per version and only returns it. The engine learns the request's name at each entry point (context value) and writes it on every save it makes, while workflow selection and criteria keep seeing the start version's value. Three wire surfaces expose the field; the help moves the approval recipe onto it.

**Tech Stack:** Go 1.27, cyoda-go-spi (separate repo `../cyoda-go-spi`, conformance suite `spitest`), memory/sqlite/postgres plugin modules, oapi-codegen (`go generate ./api`), go-jsonschema (`scripts/generate-events.sh`), testcontainers e2e.

**Spec:** `docs/superpowers/specs/2026-11-22-725-change-history-transition-design.md` (approved). Read it before any task; decision numbers below refer to it.

## Global Constraints

- Wire name `transition` on every history surface; present when non-empty, absent otherwise (spec decision 4).
- Values: the named transition; `loopback` for a save that named none; the scheduled task's transition for a scheduled firing; empty/absent on a `DELETED` tombstone (decision 5).
- What the run evaluates (workflow selection and transition criteria) sees the start version's `TransitionForLatestSave` for the whole run, boundary or not; every save cyoda makes for the request writes the request's name (decision 6).
- No new status or error code; no migration.
- TDD per `.claude/rules/tdd.md`: each behaviour change has a test watched failing first. Do not add `-count=1`; do not hand-roll `go test ./...` as verification (use `make test` and the touched packages).
- Business time from `spi.Clock` only; `log/slog` only; no issue numbers in shipped artefacts (code, help, OpenAPI, errors, logs).
- Help prose: plain STE English, the client's view; no storage behaviour in cases the API rules exclude.
- SPI mid-milestone: SPI PR into `cyoda-go-spi` `main`, no tag. During development the worktree's `go.work` gets an uncommitted `use ../../../../cyoda-go-spi` line; it is removed before the pin commit.
- Never `git stash` bare; never run tests in a worktree while editing it.

## Review Focus

1. **A scheduled firing that fires nothing** (cancelled, declined, expired, no fireable transition) must still write no version: the new stamping must not create one. → Task 5, `TestFireScheduled_NoFireWritesNoVersion`.
2. **The same entity twice in one collection update** (named item then loopback item) → one version, last item's name, same on every backend. → Task 6, e2e `TestChangeHistory_CollectionSameEntityTwice_LastItemWins`.
3. **A request that fails after a `COMMIT_BEFORE_DISPATCH` boundary** keeps the boundary's committed version, carrying the request's name. → Task 6, e2e `TestChangeHistory_FailureAfterBoundary_KeepsBoundaryVersionName`.
4. **Postgres tombstone clearing** must not change reads of a deleted entity or of its earlier versions (`GET /entity/{id}?pointInTime=` before the delete still shows the earlier name). → Task 4, `TestPostgres_TombstoneClear_EarlierVersionReadUnchanged`.
5. **A joined compute-node callback reading the entity mid-run** sees the request's name once the engine has saved the entity in the transaction (the save before its callout). → Task 6, e2e `TestDecision6_JoinedReadSeesRequestName`.

---

## Stream plan

- Stream A (SPI → plugins): Tasks 1–4. Task 1 first; 2, 3, 4 in parallel after it.
- Stream B (engine/service): Tasks 5–6. Independent of A (`TransitionForLatestSave` already exists); may run concurrently with A.
- Stream C (wire): Task 7, after A and B.
- Stream D (parity, docs, recipe): Tasks 8–9, after C.
- Task 10 (pin, changelog, Gate 4/7 files) last.

---

### Task 1: SPI — `EntityVersionMeta.Transition` and conformance

**Repo:** `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`, new branch `feat/version-meta-transition` from `main` (read `MAINTAINING.md` first: no tag).

**Files:**
- Modify: `types.go` (`EntityVersionMeta`, ~line 89)
- Modify: `persistence.go` (`GetVersionMetadata` doc comment, ~509-526)
- Modify: `spitest/entity.go` (register + new subtests), `spitest/entity_one_version.go`
- Modify: `CHANGELOG.md` (`[Unreleased]` → Added)

**Interfaces:**
- Produces: `spi.EntityVersionMeta.Transition string` — "the TransitionForLatestSave the version was saved with; empty for a DELETED tombstone."

- [ ] **Step 1: Write the failing conformance subtests** (register in `entity.go` next to `GetVersionMetadata/Ordering`; build entities with `newEntity` and set `Meta.TransitionForLatestSave`):
  - `GetVersionMetadata/TransitionPerVersion`: three saves in three transactions with transitions `"loopback"`, `"submit"`, `"approve"`; assert `metas[0..2].Transition == "approve","submit","loopback"` (newest first).
  - `GetVersionMetadata/TombstoneTransitionEmpty`: five cases, each on a fresh entity whose last save carried `"submit"`, each asserting the tombstone's `Transition == ""` and the prior version's `== "submit"`: delete outside a transaction; delete in its own transaction; save-then-delete in one transaction; create-then-delete in one transaction (only the tombstone exists or is listed — assert whatever row is `Deleted` has `""`); `DeleteAll` of the model.
  - `OneVersionPerTx/TransitionIsLastWrite` (in `entity_one_version.go`, beside `AttributionIsLastWrite`): two saves of one entity in one transaction, `"submit"` then `"loopback"` → one version, `Transition == "loopback"`.
- [ ] **Step 2: Add the field** `Transition string` to `EntityVersionMeta` with the doc comment above; extend the `GetVersionMetadata` doc comment to say it returns it.
- [ ] **Step 3: Build the SPI** — `go build ./... && go vet ./...` in the SPI repo. (spitest does not run in the SPI; it runs in each plugin, Tasks 2–4.)
- [ ] **Step 4: Commit** on the SPI branch: `feat: EntityVersionMeta.Transition`.
- [ ] **Step 5: Wire it locally**: in the cyoda-go worktree, add `use ../../../../cyoda-go-spi` to `go.work` (do not commit it). Run `cd plugins/memory && go test ./...` — Expected: the new subtests FAIL (`Transition` empty). This is the RED for Tasks 2–4.

---

### Task 2: memory plugin returns the transition

**Files:** Modify `plugins/memory/entity_store.go` (`GetVersionMetadata`, ~1245-1286).

**Interfaces:** Consumes `spi.EntityVersionMeta.Transition` (Task 1).

- [ ] **Step 1: Confirm RED**: `cd plugins/memory && go test ./... -run Conformance` — the Task 1 subtests fail.
- [ ] **Step 2: Implement**: set `Transition` from the version's stored entity copy (`v.entity.Meta.TransitionForLatestSave`); a tombstone's entity is nil → `""`.
- [ ] **Step 3: Verify**: same command — PASS.
- [ ] **Step 4: Commit** `feat(memory): return the transition in version metadata`.

---

### Task 3: sqlite plugin returns the transition and fails closed on bad meta

**Files:**
- Modify: `plugins/sqlite/entity_store.go` — `GetVersionMetadata` (~1536-1613), `GetVersionByTransaction` (~1476-1505), `scanEntityFromRow` (~169-182), `scanVersionEntity` (~213-229)
- Test: `plugins/sqlite/meta_integrity_internal_test.go` (new, package-internal so it can write a raw row)

**Interfaces:** Consumes Task 1.

- [ ] **Step 1: Confirm RED** for the Task 1 subtests: `cd plugins/sqlite && go test ./... -run Conformance`.
- [ ] **Step 2: Write failing tests** `TestMetaIntegrity_*` — for each of: unparseable meta (`'{"x":'`), NULL meta, empty-string meta — written straight into the `entities` and `entity_versions` rows of a saved entity: `Get`, `GetVersionMetadata`, `GetVersionByTransaction`, a search over the model, and grouped stats over the model each return a non-nil error (not a row with empty fields).
- [ ] **Step 3: Implement**: copy `parsed.TransitionForLatestSave` into `Transition` in `GetVersionMetadata` (tombstone meta has none → `""`); at all four sites return a wrapped error (`fmt.Errorf("failed to parse entity meta: %w", err)`) on a parse error or NULL/empty meta instead of skipping.
- [ ] **Step 4: Verify**: `cd plugins/sqlite && go test ./...` — PASS.
- [ ] **Step 5: Commit** `feat(sqlite): return the transition in version metadata; fail reads on bad meta`.

---

### Task 4: postgres plugin returns the transition and clears it on tombstones

**Files:**
- Modify: `plugins/postgres/entity_store.go` — `GetVersionMetadata` (~1402-1464), `deleteOn` (~939-1024)
- Test: `plugins/postgres/tombstone_transition_test.go` (new)

**Interfaces:** Consumes Task 1.

- [ ] **Step 1: Confirm RED** for the Task 1 subtests (postgres conformance needs Docker): `cd plugins/postgres && go test ./... -run Conformance`. Expected: `TransitionPerVersion` and `TombstoneTransitionEmpty` fail (tombstone carries `"submit"`).
- [ ] **Step 2: Write failing test** `TestPostgres_TombstoneClear_EarlierVersionReadUnchanged`: save with `"submit"`, delete; `GetAsAt` at an instant before the delete returns the entity with `TransitionForLatestSave == "submit"`; `Get` after the delete returns `spi.ErrNotFound`.
- [ ] **Step 3: Implement**: copy `meta.Transition` into `Transition` in `GetVersionMetadata`; in `deleteOn` set `current.Meta.TransitionForLatestSave = ""` before the tombstone doc is marshalled (covers the pending-version replace path too).
- [ ] **Step 4: Verify**: `cd plugins/postgres && go test ./...` — PASS.
- [ ] **Step 5: Commit** `feat(postgres): return the transition in version metadata; clear it on tombstones`.

---

### Task 5: engine stamps the request's name on every save it makes

**Files:**
- Create: `internal/domain/workflow/save_transition.go`
- Modify: `internal/domain/workflow/engine.go` (`Execute` ~302, `ManualTransition` ~392, `Loopback` ~476), `anchor.go` (`saveAnchorBeforeCallout` ~59-80), `engine_processors.go` (TX_post `CompareAndSave` ~490, boundary flush `es.Save` ~559), `fire_scheduled.go` (`fireScheduled` ~139; final `CompareAndSave` ~368)
- Test: `internal/domain/workflow/save_transition_test.go` (new), plus cases in the existing fire tests

**Interfaces:**
- Produces (package-internal):
  - `func withSaveTransition(ctx context.Context, name string) context.Context`
  - `func saveTransitionFrom(ctx context.Context) string`
  - `func stampedForSave(ctx context.Context, e *spi.Entity) *spi.Entity` — a shallow copy with `Meta.TransitionForLatestSave = saveTransitionFrom(ctx)`; returns `e` itself when the context carries no name. Never mutates `e` (the run keeps evaluating the start value).
- Entry points set the name: `Execute` → `"loopback"`, `Loopback` → `"loopback"`, `ManualTransition` → `transitionName`, `fireScheduled` → `task.Transition`.

- [ ] **Step 1: Write failing tests** (engine with the memory store factory the package's tests already use):
  - `TestSaveTransition_AnchorSaveWritesRequestName`: a named update whose processor is a callout and changes data; read the entity in the transaction at the callout → `TransitionForLatestSave == <named transition>`; the in-memory entity the criteria saw still holds the start value.
  - `TestSaveTransition_BoundaryFlushAndPostCASWriteRequestName`: a `COMMIT_BEFORE_DISPATCH` processor on a create; the committed TX_pre version and the TX_post pending version both carry `"loopback"`.
  - `TestSaveTransition_CriterionSeesStartValue`: lifecycle criterion `transitionForLatestSave EQUALS "submit"` on an automated transition; entity last saved by `"submit"`; a loopback update → the transition fires (start value `"submit"`), and the same criterion evaluated after a boundary in the same run still sees `"submit"`.
  - `TestSaveTransition_SelectionSeesStartValue`: two workflows, the first selected by `transitionForLatestSave EQUALS "x"`; entity last saved by `"x"`; a named update `"y"` runs the first workflow.
  - `TestFireScheduled_StampsScheduledTransition`: fire a task with `Transition "expire"` on an entity last saved by `"submit"` → final committed version carries `"expire"`; with a CBD processor, the boundary version too.
  - `TestFireScheduled_NoFireWritesNoVersion` (Review Focus 1): cancelled, declined and expired outcomes → version count unchanged.
- [ ] **Step 2: Run** `go test ./internal/domain/workflow/ -run 'SaveTransition|FireScheduled_(Stamps|NoFire)'` — Expected: FAIL.
- [ ] **Step 3: Implement** `save_transition.go`; set the name at the four entry points; route the three engine saves and the fire's final `CompareAndSave` through `stampedForSave` (the fire may instead set `entity.Meta.TransitionForLatestSave = task.Transition` just before its final save, since evaluation is over). Verify the TX_post context carries the value (it is derived from the run's context); if it does not, re-attach it where TX_post begins.
- [ ] **Step 4: Run** the same command — PASS; then `go test ./internal/domain/workflow/...`.
- [ ] **Step 5: Commit** `fix(workflow): every save of a run carries the request's transition name`.

---

### Task 6: service starts each run from the stored value; door-level tests

**Files:**
- Modify: `internal/domain/entity/service.go` — `updateEntityCore` construction (~2296-2311: `TransitionForLatestSave: existing.Meta.TransitionForLatestSave`), `UpdateEntityCollection` item construction (~2563-2577: add the same field). Create paths keep `""`; the post-run stamping lines (~356, ~2127, ~2334, ~2343, ~2655-2657) stay.
- Test: `internal/e2e/change_history_transition_test.go` (new), `internal/grpc/entity_transition_name_test.go` (new)

**Interfaces:** Consumes Task 5. Produces the observable behaviour of decisions 6–8.

- [ ] **Step 1: Write failing e2e tests** (postgres via the package's `TestMain`; read results via `GET /api/entity/{id}?pointInTime=` / `?transactionId=` for per-version `meta.transitionForLatestSave`, since the history field arrives in Task 7):
  - `TestDecision6_CriterionSeesStartValue_EachHTTPDoor`: model with an automated transition guarded by lifecycle `transitionForLatestSave EQUALS "submit"`; for each HTTP door — PUT named, PATCH named, PUT loopback, collection update item named and loopback, create, collection create — assert the guard result matches the start version's value (none on create).
  - `TestDecision6_SelectionSeesStartValue`: two workflow definitions, the first selected by `transitionForLatestSave EQUALS "x"`; entity last saved by `x`; a PUT naming `y` runs the first definition (its `y` transition), not the second.
  - `TestDecision6_CriterionAfterBoundarySeesStartValue`: a CBD processor on the named transition, then an automated transition guarded on the start value → it fires as before the boundary.
  - `TestDecision8_BoundaryVersionCarriesRequestName`: CBD processor; for create, loopback update, collection create item, HTTP collection update item (named) → the boundary version (`?transactionId=<TX_pre>`) carries the request's name.
  - `TestDecision7_ScheduledFiringStampsItsTransition`: scheduled transition `expire` after a `submit` → entity read shows `expire`.
  - `TestChangeHistory_CollectionSameEntityTwice_LastItemWins` (Review Focus 2).
  - `TestChangeHistory_FailureAfterBoundary_KeepsBoundaryVersionName` (Review Focus 3): a processor after the boundary fails → request answers its error; the boundary's version is committed and carries the request's name.
  - `TestDecision6_JoinedReadSeesRequestName` (Review Focus 5): a processor callout whose compute-node double reads the entity with the transaction token → `meta.transitionForLatestSave` is the request's name.
- [ ] **Step 2: Write failing gRPC tests** `TestDecision6_CriterionSeesStartValue_EachGRPCDoor`: update, patch, `EntityTransitionRequest`, collection create, collection update — same assertions; assert the envelope `Success`.
- [ ] **Step 3: Run** `go test ./internal/grpc/ -run Decision6` and `go test -timeout 30m ./internal/e2e/ -run 'Decision|ChangeHistory_'` — Expected: FAIL on the doors that today see the request's own name or `""`.
- [ ] **Step 4: Implement** the two construction changes.
- [ ] **Step 5: Run** the same commands — PASS; then `go test ./internal/domain/entity/...`.
- [ ] **Step 6: Commit** `fix(entity): a run evaluates the start version's transition on every door`.

---

### Task 7: wire — `transition` on `/changes`, gRPC `changeMeta`, `EntityChange` audit

**Files:**
- Modify: `internal/domain/entity/service.go` (`EntityChangeEntry` ~116 gains `Transition string`; `GetChangesMetadata` ~886 copies `v.Transition`)
- Modify: `internal/domain/entity/handler.go` (`GetEntityChangesMetadata` ~480: `entry["transition"]` when non-empty)
- Modify: `internal/grpc/search.go` (~667: `changeMeta.Transition` when non-empty)
- Modify: `internal/domain/audit/events.go` (`entityChangeItem` ~36: `body["transition"]` when non-empty)
- Modify: `api/openapi.yaml` (`EntityChangeMeta` ~11565 and `EntityChangeAuditEventDto` ~12327: optional `transition: string` with description), then `go generate ./api`
- Modify: `docs/cyoda/schema/common/EntityChangeMeta.json` (optional `transition`), then `./scripts/generate-events.sh`
- Modify: `e2e/parity/client/types.go` (`EntityChangeMeta.Transition string \`json:"transition,omitempty"\``), `api/clientprobe/samples/audit-entity-change.json`
- Test: `internal/e2e/change_history_transition_test.go` (extend), `internal/grpc/` changes-metadata test (extend the existing `EntityChangesMetadataGetRequest` test file)

**Interfaces:** Consumes Tasks 1–6. Produces `EntityChangeEntry.Transition`; JSON `transition` on the three surfaces.

- [ ] **Step 1: Write failing tests**:
  - e2e `TestChangesEndpoint_TransitionPerVersion`: create, PUT named `submit`, PUT loopback, DELETE → `/changes` newest first: `transition` absent (DELETE), `"loopback"`, `"submit"`, `"loopback"`.
  - e2e `TestAuditEntityChange_Transition`: same sequence → `GET /audit/entity/{id}?eventType=EntityChange` items carry the same values (absent on DELETE).
  - gRPC `TestEntityChangesMetadata_Transition`: same sequence → `changeMeta.transition`; absent on the tombstone; plus one `EntityTransitionRequest` naming `approve` → its entry carries `"approve"`.
- [ ] **Step 2: Run** them — FAIL.
- [ ] **Step 3: Implement** the code and schema changes; regenerate both generated files; `make check-codegen` passes.
- [ ] **Step 4: Run** them — PASS; `go test ./api/... ./internal/domain/... ./internal/grpc/...`.
- [ ] **Step 5: Commit** `feat: transition on entity change history (HTTP, gRPC, audit)`.

---

### Task 8: cross-backend parity

**Files:** Modify `e2e/parity/history_reads.go`, `attribution.go`, `entity_update_collection.go`, `multinode/attribution.go`; register new scenarios in `e2e/parity/registry.go` (and `multinode` registry).

**Interfaces:** Consumes Task 7's client field `EntityChangeMeta.Transition`.

- [ ] **Step 1: Write the failing scenarios / assertions**:
  - New `RunHistoryTransitionPerVersion`: the Task 7 sequence, asserted on `/changes` and audit; registered as `HistoryTransitionPerVersion`.
  - New `RunDecision6CriterionSeesStartValue`: the guard model from Task 6, named and loopback update; registered.
  - Extend `RunHistoryOneVersionPerTransaction` (boundary/one-version scenario): the history entry carries the request's name.
  - Extend `RunAttributionScheduledArmedByFire`: the fired version's `transition` is the scheduled transition.
  - Extend `RunAttributionTombstoneUniformity`: the tombstone entry has no `transition` on every backend.
  - Extend `RunEntityUpdateCollectionHappyPath`: each item's version carries its item's name.
  - Extend multinode `RunAttribution_ScheduledFire` and `RunAttribution_ProxiedJoinCascade`: `transition` on the versions, any node.
- [ ] **Step 2: Run** `make test` (runs parity uncached) — the new assertions fail only if Tasks 1–7 are incomplete; to prove RED, run them once with Task 4's `deleteOn` change reverted locally (tombstone case) and Task 5's fire stamping reverted (scheduled case), then restore.
- [ ] **Step 3: Verify** `make test` — PASS.
- [ ] **Step 4: Commit** `test(parity): transition on change history across backends`.

---

### Task 9: help, docs and the approval recipe

**Files:**
- Modify: `cmd/cyoda/help/content/audit.md`, `crud.md`, `grpc.md`, `search.md`, `workflows.md`, `auth/integration.md`
- Modify: `docs/access-to-the-cyoda-api.html`, `docs/ARCHITECTURE.md` (~410 anchor/boundary text, ~2319 history bullet)
- Modify: `api/openapi.yaml` and `docs/cyoda/schema/common/EntityMetadata.json` descriptions of `transitionForLatestSave` (decision 5 value set); regenerate as in Task 7
- Test: `internal/e2e/approval_recipe_test.go` (new)

**Interfaces:** Consumes Tasks 5–7.

- [ ] **Step 1: Write the failing recipe e2e** `TestApprovalRecipe_*` — model: `DRAFT --submit(manual)--> SUBMITTED --approve(manual, function criterion, attachEntity default)--> APPROVED`; the compute-node double implements the spec's check (newest `/changes` entry; `transition == "submit"`, `attributedKind == "user"`, `user != authid`; data equal to `GET /entity/{id}?transactionId=<entry tx>`; refuse on any read error or missing data). Cases:
  - `Passes`: alice submits (OBO), bob approves with `PATCH {}` + `If-Match` → 200, state `APPROVED`; and with gRPC `EntityTransitionRequest`.
  - `SameUserRefused`: alice approves her own submit → refused.
  - `WriteBetweenRefused`: a loopback update after the submit → bob's approval refused.
  - `DataChangeRefused`: bob approves with `PATCH {"amount": 2}` → refused.
  - `NoDataRefused`: criterion with `attachEntity: false` → refused.
  These pass only once Tasks 5–7 are in; RED is the run before Task 7 (no `transition` field → refusal of `Passes`).
- [ ] **Step 2: Rewrite the help** to the spec's *Help* section, item by item (each bullet there is a checklist line). The recipe text states the conditions, the check, what it proves, the scheduled and write-back attribution notes, and the 1000-entry cap on `/changes`. Delete the copy advice at `auth/integration.md` step 3 and `audit.md` *Recording*.
- [ ] **Step 3: Verify**: `go test ./cmd/cyoda/help/...`, the recipe e2e, `make check-codegen`; grep proves the copy advice is gone: `grep -rn "into the entity's data" cmd/cyoda/help/content docs/access-to-the-cyoda-api.html` → only non-recipe hits (show the output).
- [ ] **Step 4: Commit** `docs: change-history transition and the approval recipe`.

---

### Task 10: changelog, parity doc, pin

**Files:** `CHANGELOG.md` (new `## [Unreleased]` above `[0.9.0]`), `docs/cloud-parity/change-history-transition.md` (new), `go.mod` + plugin `go.mod`s, `COMPATIBILITY.md`, `go.work` (drop the local `use` line)

- [ ] **Step 1: CHANGELOG** `[Unreleased]`: `### Breaking` — decision 6 with the spec's migration note; `### Added` — `transition` on `/changes`, gRPC `changeMeta`, `EntityChange` audit; `### Fixed` — items 7–11 in the spec's today → after wording.
- [ ] **Step 2: Parity doc** — one file: the new field (values, absent on delete) and decision 6, in the style of the neighbouring `docs/cloud-parity/*.md`.
- [ ] **Step 3: Pin**: push the SPI branch, open the SPI PR into `main` (title only), merge it once green; in cyoda-go `go get github.com/cyoda/cyoda-go-spi@<merged main sha>` in the root and each plugin, `make repin-plugins`, `go mod tidy` per module; record the pseudo-pin in `COMPATIBILITY.md`; remove the `use ../../../../cyoda-go-spi` line from `go.work`.
- [ ] **Step 4: Verify** `make test`, `go vet ./...`, `make check-spi-pin-sync`.
- [ ] **Step 5: Commit** `chore: pin cyoda-go-spi main; changelog and parity doc`.

---

## After the plan (finishing workflow, not tasks)

Whole-branch fresh-context code review → `cyoda-go-security-audit` on the exact PR head → PR into `release/v0.10.0` → CI green on the head. File the Cassandra issue (spec item 12) and the CaaS ticket (Gate 7) at PR time.
