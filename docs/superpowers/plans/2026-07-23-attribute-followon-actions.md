# Attribute Deferred & Cascaded Workflow Actions — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Attribute cascaded and scheduled follow-on actions to the originating principal (while recording the executor), per the spec `docs/superpowers/specs/2026-07-22-attribute-followon-actions-design.md`.

**Architecture:** One `Principal{ID,Kind}` model; origin captured once at root `Begin` on `TransactionState` and read at every join (memory/sqlite via shared pointer, postgres via per-tx map); scheduled fires carry a durable `ArmedBy` seeded as ambient origin at the single engine door; CBD-detached callouts are deliberately handed to the application (no mechanism). Executor recorded beside the attributed principal on every durable write and surfaced on `/changes`.

**Tech Stack:** Go 1.26+, cyoda-go-spi (sibling checkout `../cyoda-go-spi`, composed via go.work), plugins memory/sqlite/postgres (own go.mods), testcontainers e2e.

## Global Constraints

- Spec is authoritative: `docs/superpowers/specs/2026-07-22-attribute-followon-actions-design.md`. Section refs (§N) below point there.
- Branch: `feat/attribute-followon-actions` (cyoda-go, already checked out). SPI work on branch `feat/attribution` in `../cyoda-go-spi`. Single cyoda-go PR onto `release/v0.8.3`. **No SPI tag now** — pseudo-version pin; tag at milestone end per `../cyoda-go-spi/MAINTAINING.md`.
- **No new error codes are introduced** (verified against spec §13) → no `errors/<CODE>.md` tasks; `TestErrCode_Parity` must stay green.
- `log/slog` only; wrap errors `fmt.Errorf("...: %w", err)`; no issue IDs in shipped artefacts (code, help, OpenAPI); never log credentials.
- Fail closed: unset `Kind`/nil UserContext at callout emission fails the dispatch (§3.3). Never substitute a wrong-but-available value.
- Zero `Principal{}` means "absent" at every consumer; legacy rows/tasks render without the new fields (§5.1, §6.2).
- Concurrency tests are isolated single-backend e2e — never in the shared parity suite.
- `go.work` is committed in CI-safe form; the local `../cyoda-go-spi` `use` line stays uncommitted — **never `git add -A`**; stage files explicitly.
- Line numbers in this plan are anchors from the spec's audit; re-locate by symbol if drifted.

---

### Task 1: SPI — Principal, Kind, ambient origin, resolution helpers

**Files:**
- Modify: `../cyoda-go-spi/context.go`
- Create: `../cyoda-go-spi/principal_test.go`

**Interfaces (Produces):**
```go
type PrincipalKind string
const (
    PrincipalUser    PrincipalKind = "user"
    PrincipalService PrincipalKind = "service"
    PrincipalSystem  PrincipalKind = "system"
)
type Principal struct {
    ID   string        `json:"id"`
    Kind PrincipalKind `json:"kind"`
}
// UserContext gains: Kind PrincipalKind
func WithAmbientOrigin(ctx context.Context, p Principal) context.Context
func GetAmbientOrigin(ctx context.Context) Principal            // zero if absent
func ResolveOrigin(ctx context.Context) Principal               // §4.1: parent-tx > ambient > UserContext
func AttributionFor(ctx context.Context) (attributed, executor Principal) // §7 stamp rule
```

- [ ] **Step 1: `cd ../cyoda-go-spi && git checkout -b feat/attribution` (from main).**

- [ ] **Step 2: Write failing tests** in `principal_test.go`:

```go
func TestResolveOrigin_Precedence(t *testing.T) {
	user := Principal{ID: "u1", Kind: PrincipalUser}
	svc := Principal{ID: "svc", Kind: PrincipalService}
	origin := Principal{ID: "root", Kind: PrincipalUser}

	// UserContext fallback (root direct caller)
	ctx := WithUserContext(context.Background(), &UserContext{UserID: "u1", Kind: PrincipalUser, Tenant: Tenant{ID: "t"}})
	if got := ResolveOrigin(ctx); got != user {
		t.Fatalf("uc branch: got %+v", got)
	}
	// Ambient beats UserContext (scheduled fire seed)
	ctx = WithAmbientOrigin(ctx, origin)
	if got := ResolveOrigin(ctx); got != origin {
		t.Fatalf("ambient branch: got %+v", got)
	}
	// Zero ambient is absent
	ctx2 := WithAmbientOrigin(WithUserContext(context.Background(), &UserContext{UserID: "u1", Kind: PrincipalUser}), Principal{})
	if got := ResolveOrigin(ctx2); got != user {
		t.Fatalf("zero ambient must be absent: got %+v", got)
	}
	// Parent tx beats ambient
	ctx = WithTransaction(ctx, &TransactionState{ID: "tx1", Origin: Principal{ID: "parent", Kind: PrincipalUser}})
	if got := ResolveOrigin(ctx); got.ID != "parent" {
		t.Fatalf("parent-tx branch: got %+v", got)
	}
	_ = svc
}

func TestAttributionFor_StampRule(t *testing.T) {
	origin := Principal{ID: "root", Kind: PrincipalUser}
	// user-kind executor records itself even inside a foreign tx (D3)
	ctx := WithTransaction(
		WithUserContext(context.Background(), &UserContext{UserID: "obo", Kind: PrincipalUser}),
		&TransactionState{ID: "tx", Origin: origin})
	a, e := AttributionFor(ctx)
	if a.ID != "obo" || e.ID != "obo" {
		t.Fatalf("D3: got a=%+v e=%+v", a, e)
	}
	// service executor inherits tx origin
	ctx = WithTransaction(
		WithUserContext(context.Background(), &UserContext{UserID: "svc", Kind: PrincipalService}),
		&TransactionState{ID: "tx", Origin: origin})
	a, e = AttributionFor(ctx)
	if a != origin || e.ID != "svc" || e.Kind != PrincipalService {
		t.Fatalf("inherit: got a=%+v e=%+v", a, e)
	}
	// service executor, no tx → attributed = executor (non-joined)
	ctx = WithUserContext(context.Background(), &UserContext{UserID: "svc", Kind: PrincipalService})
	a, e = AttributionFor(ctx)
	if a != e || a.ID != "svc" {
		t.Fatalf("non-joined: got a=%+v e=%+v", a, e)
	}
	// unset kind treated as user (conservative)
	ctx = WithTransaction(
		WithUserContext(context.Background(), &UserContext{UserID: "legacy"}),
		&TransactionState{ID: "tx", Origin: origin})
	a, _ = AttributionFor(ctx)
	if a.ID != "legacy" {
		t.Fatalf("unset kind: got %+v", a)
	}
}
```

- [ ] **Step 3: Run `go test ./... -run 'ResolveOrigin|AttributionFor' -v` — expect FAIL (undefined symbols).**

- [ ] **Step 4: Implement** in `context.go` — add `Kind PrincipalKind` to `UserContext`; add:

```go
type PrincipalKind string

const (
	PrincipalUser    PrincipalKind = "user"
	PrincipalService PrincipalKind = "service"
	PrincipalSystem  PrincipalKind = "system"
)

// Principal identifies an actor and its explicit kind. The zero value means "absent".
type Principal struct {
	ID   string        `json:"id"`
	Kind PrincipalKind `json:"kind"`
}

const ambientOriginKey contextKey = "ambientOrigin"

// WithAmbientOrigin seeds the origin for a causal-chain root that has no
// transaction yet. Single legitimate seed site: the scheduled fire, from the
// durable task row. A zero Principal is never seeded.
func WithAmbientOrigin(ctx context.Context, p Principal) context.Context {
	if p == (Principal{}) {
		return ctx
	}
	return context.WithValue(ctx, ambientOriginKey, p)
}

func GetAmbientOrigin(ctx context.Context) Principal {
	p, _ := ctx.Value(ambientOriginKey).(Principal)
	return p
}

// ResolveOrigin is the single shared origin-precedence implementation:
// parent-tx > ambient > UserContext. All backends MUST use it at Begin —
// divergence here is an attribution bug.
func ResolveOrigin(ctx context.Context) Principal {
	if tx := GetTransaction(ctx); tx != nil && tx.Origin != (Principal{}) {
		return tx.Origin
	}
	if amb := GetAmbientOrigin(ctx); amb != (Principal{}) {
		return amb
	}
	if uc := GetUserContext(ctx); uc != nil {
		return Principal{ID: uc.UserID, Kind: uc.Kind}
	}
	return Principal{}
}

// AttributionFor returns (attributed, executor) for a durable write staged
// under ctx. Origin inheritance engages only for service/system executors
// inside a transaction; a user-kind (or legacy unset-kind) executor records
// itself. Never elevates a non-joined write to a claimed user.
func AttributionFor(ctx context.Context) (attributed, executor Principal) {
	if uc := GetUserContext(ctx); uc != nil {
		executor = Principal{ID: uc.UserID, Kind: uc.Kind}
	}
	if executor.Kind == PrincipalService || executor.Kind == PrincipalSystem {
		if tx := GetTransaction(ctx); tx != nil && tx.Origin != (Principal{}) {
			return tx.Origin, executor
		}
	}
	return executor, executor
}
```

- [ ] **Step 5: Run tests again — expect PASS. Then `go vet ./...`.**
- [ ] **Step 6: Commit** (in `../cyoda-go-spi`): `git add context.go principal_test.go && git commit -m "feat: Principal/PrincipalKind, ambient origin, ResolveOrigin/AttributionFor"`

---

### Task 2: SPI — durable-record fields (TransactionState, ScheduledTask, EntityMeta/EntityVersion)

**Files:**
- Modify: `../cyoda-go-spi/txcontext.go` (struct at :85; godoc immutable-set at :53-54; savepoint note near :12)
- Modify: `../cyoda-go-spi/types.go` (`EntityMeta` :18, `EntityVersion` :38, `ScheduledTask` :293)
- Create: `../cyoda-go-spi/attribution_fields_test.go`

**Interfaces (Produces):**
```go
type WriteAttribution struct{ Attributed, Executor Principal }
// TransactionState gains:
//   Origin Principal                              — immutable after Begin
//   DeleteAttribution map[string]WriteAttribution — entityID → actors; OpMu posture identical to Deletes;
//                                                   savepoints snapshot/restore it PAIRED with Deletes
// ScheduledTask gains: ArmedBy Principal `json:"armedBy,omitempty"`
// EntityMeta gains:    ChangeUserKind PrincipalKind; ChangeExecutor Principal
// EntityVersion gains: AttributedKind PrincipalKind; Executor Principal
```

- [ ] **Step 1: Write failing test** `attribution_fields_test.go`:

```go
func TestScheduledTask_ArmedBy_JSONRoundTrip(t *testing.T) {
	in := ScheduledTask{ID: "id1", TenantID: "t", Type: ScheduledTaskFireTransition,
		ArmedBy: Principal{ID: "u1", Kind: PrincipalUser}}
	b, err := json.Marshal(in)
	if err != nil { t.Fatal(err) }
	var out ScheduledTask
	if err := json.Unmarshal(b, &out); err != nil { t.Fatal(err) }
	if out.ArmedBy != in.ArmedBy { t.Fatalf("round-trip: %+v", out.ArmedBy) }
	// legacy JSON (no armedBy) → zero value
	var legacy ScheduledTask
	if err := json.Unmarshal([]byte(`{"id":"x","tenantId":"t"}`), &legacy); err != nil { t.Fatal(err) }
	if legacy.ArmedBy != (Principal{}) { t.Fatalf("legacy must be zero: %+v", legacy.ArmedBy) }
}
```

- [ ] **Step 2: Run — FAIL (no `ArmedBy` field). Implement all field additions:**
  - `TransactionState`: `Origin Principal` and `DeleteAttribution map[string]WriteAttribution` (define `WriteAttribution` above the struct). Update the **immutable-after-Begin godoc list** (`ID, TenantID, SnapshotTime` → add `Origin`), add `DeleteAttribution` to the OpMu contract comment with the same posture as `Deletes`, and extend the savepoint sentence: savepoints snapshot/restore `DeleteAttribution` **paired with** `Deletes`.
  - `ScheduledTask`: `ArmedBy Principal \`json:"armedBy,omitempty"\`` with doc: "arming principal (chain origin at arm time, §spec); zero on legacy rows — fire treats zero as the system principal. omitempty does not omit a zero struct; readers rely on the zero-value check, never field absence."
  - `EntityMeta`: `ChangeUserKind PrincipalKind` (kind of the attributed `ChangeUser`; empty on legacy rows) and `ChangeExecutor Principal`.
  - `EntityVersion`: `AttributedKind PrincipalKind`, `Executor Principal` (populated independently of `Entity` — Entity is nil for DELETED versions on some backends).
- [ ] **Step 3: Run `go test ./... -v` (whole SPI) — PASS; `go vet ./...`.**
- [ ] **Step 4: Commit:** `git add txcontext.go types.go attribution_fields_test.go && git commit -m "feat: attribution fields on TransactionState, ScheduledTask, EntityMeta/EntityVersion"`

---

### Task 3: SPI — conformance suites

**Files:**
- Modify: `../cyoda-go-spi/spitest/transaction.go` (suite runner `runTransactionSuite` :15)
- Modify: `../cyoda-go-spi/spitest/entity.go`
- Modify: `../cyoda-go-spi/scheduled_task_store_conformance.go` (root harness — scheduled-task conformance does NOT live in spitest/)
- Modify: `../cyoda-go-spi/spitest/spitest.go` (`tenantContext` :154 — new tests set `Kind` explicitly)

**Interfaces (Consumes):** Task 1-2 symbols. **Produces:** conformance every backend (incl. commercial) must pass.

- [ ] **Step 1: Add failing conformance tests** (they run against the in-repo reference impl if any; primarily they compile the contract and will be exercised by each plugin in Tasks 9-11):
  - `spitest/transaction.go` — `testTxOriginCaptureAndJoin`: Begin under a `user`-kind UserContext → `GetTransaction(txCtx).Origin == Principal{ID:uc.UserID, Kind:PrincipalUser}`; `Join` from a second context (same tenant, `service`-kind uc) → joined state's `Origin` equals the original (postgres-style rebuilds must repopulate). `testTxOriginAmbientRoot`: Begin with no parent tx but `WithAmbientOrigin(ctx, seed)` → `Origin == seed`.
  - `spitest/transaction.go` — `testTxDeleteAttributionSavepoint`: stage delete A, `Savepoint`, stage delete B, `RollbackToSavepoint` → `DeleteAttribution` contains exactly A's entry (paired with `Deletes`); commit → tombstone A carries the staged attribution.
  - `spitest/entity.go` — `testEntityExecutorRoundTrip`: save entity with `ChangeUser/ChangeUserKind/ChangeExecutor` set → `GetVersionHistory` returns `EntityVersion{AttributedKind, Executor}` equal to what was written; a DELETED version's `Executor` is readable without touching `Entity`.
  - `scheduled_task_store_conformance.go` — extend with `ArmedBy` in the upsert fixture; `Get` returns it; a legacy row (upsert without ArmedBy via raw JSON where the harness allows, else zero-value task) yields zero.
- [ ] **Step 2: Register the new tests in their suite runners; `go test ./... -v` — the spitest package itself must compile and pass (plugin execution happens in Tasks 9-11).**
- [ ] **Step 3: Update `../cyoda-go-spi/CHANGELOG.md` `[Unreleased]`:** Added — `Principal`/`PrincipalKind`, `UserContext.Kind`, `TransactionState.Origin`+`DeleteAttribution`, `ScheduledTask.ArmedBy`, `EntityMeta`/`EntityVersion` executor fields, `WithAmbientOrigin`/`GetAmbientOrigin`/`ResolveOrigin`/`AttributionFor`, conformance coverage.
- [ ] **Step 4: Commit; push branch; open SPI PR → merge to SPI main** (no tag). Record the merged commit SHA for Task 21.

---

### Task 4: cyoda-go — principal-kind constructors

**Files:**
- Modify: `internal/auth/validator.go` (`buildUserContext` ~104-134)
- Modify: `internal/auth/oidc/usercontext.go` (~32-69)
- Modify: `app/app.go` (system principal ~243-247; mock default ~376-385)
- Modify: `app/config.go` (IAM config `MockRoles` :117; defaults ~:258)
- Modify: `cmd/cyoda/help/config_registry.go` (mock config help entries ~:81/:98 region) + the matching `cmd/cyoda/help/content/config/*.md` topic + `README.md` env table
- Test: `internal/auth/validator_test.go` (extend existing)

**Interfaces (Produces):** every non-test `spi.UserContext{}` construction sets `Kind`. New env var `CYODA_IAM_MOCK_KIND` (config `IAM.MockKind string`, default `"user"`).

- [ ] **Step 1: Write failing validator tests** (table-driven, extend existing token-fixture helpers in `validator_test.go`):

```go
// claims fixtures → expected Kind
// {"user_roles": [...]}                       → spi.PrincipalUser
// {"user_roles": []}  (empty array present)   → spi.PrincipalUser   // key-presence, not len
// {"scopes": [...]}                           → spi.PrincipalService
// {"user_roles": [...], "scopes": [...]}      → spi.PrincipalUser   // both → user
// {}  (neither)                               → spi.PrincipalUser   // neither → user (attribution-safe)
```

- [ ] **Step 2: Run — FAIL. Implement in `buildUserContext`,** branching on claim-**key presence** BEFORE the `len(roles)==0 { roles = scopes }` collapse:

```go
kind := spi.PrincipalUser
if _, hasUserRoles := claims["user_roles"]; !hasUserRoles {
	if _, hasScopes := claims["scopes"]; hasScopes {
		kind = spi.PrincipalService
	}
}
// ... existing role extraction unchanged ...
// add Kind: kind to the returned &spi.UserContext{...}
```

- [ ] **Step 3: Set Kind at the remaining constructors:** OIDC → `Kind: spi.PrincipalUser`; app system principal (`app/app.go:243`) → `Kind: spi.PrincipalSystem`; mock default user → `Kind: spi.PrincipalKind(cfg.IAM.MockKind)`. Add `MockKind string` to the IAM config struct, default `"user"` via `CYODA_IAM_MOCK_KIND`, registered in `config_registry.go`; document in the config help topic and `README.md` (Gate 4).
- [ ] **Step 4: Run `go test ./internal/auth/... ./app/... -v` — PASS. `go vet ./...`.**
- [ ] **Step 5: Commit:** `git add internal/auth app/config.go app/app.go cmd/cyoda/help README.md && git commit -m "feat(auth): explicit principal kind on every UserContext constructor"`

---

### Task 5: cyoda-go — scheduler system principal (drop the fake "scheduler" user)

**Files:**
- Modify: `internal/scheduler/executor.go` (:13, :29-36)
- Modify: `internal/scheduler/executor_test.go` (:33-34 asserts `"scheduler"`)

**Interfaces (Produces):** `scheduler.SystemPrincipal() spi.Principal` = `{ID:"system", Kind:PrincipalSystem}` (reuses the app system principal id — §16 resolution: no new config key, YAGNI); `SystemUserContext` carries it.

- [ ] **Step 1: Update the test** to assert `uc.UserID == "system"`, `uc.Kind == spi.PrincipalSystem`, tenant preserved. Run — FAIL.
- [ ] **Step 2: Implement:**

```go
// systemPrincipalID identifies the platform system principal (same identity
// as the app-level system context). Never a real end-user; kind=system.
const systemPrincipalID = "system"

func SystemPrincipal() spi.Principal {
	return spi.Principal{ID: systemPrincipalID, Kind: spi.PrincipalSystem}
}

func SystemUserContext(tenant spi.TenantID) context.Context {
	uc := &spi.UserContext{
		UserID:   systemPrincipalID,
		UserName: systemPrincipalID,
		Kind:     spi.PrincipalSystem,
		Tenant:   spi.Tenant{ID: tenant},
	}
	return spi.WithUserContext(context.Background(), uc)
}
```

Keep the existing doc comment about deriving from `context.Background()`; update its identity wording. Grep `"scheduler"` across root module for other identity assertions and update them.
- [ ] **Step 3: Run `go test ./internal/scheduler/... -v` — PASS. Commit** `feat(scheduler): fire as the real system principal, not a fake "scheduler" user`.

---

### Task 6: cyoda-go — Kind-driven authtype, fail-loud emission

**Files:**
- Modify: `internal/grpc/cloudevent.go` (`AttachAuthContext` :35-67)
- Modify: `internal/grpc/dispatch.go` (call site :113 — handle new error)
- Modify: `internal/grpc/dispatch_test.go` (:138-144 covers only `user`)
- Modify: `cmd/cyoda/help/content/grpc.md` (:359 names `service_account`)

**Interfaces (Produces):** `func AttachAuthContext(ctx context.Context, ce *cepb.CloudEvent) error` — emits `authtype ∈ {user,service,system}` from `uc.Kind`; returns error on nil uc or unset Kind (dispatch fails, callout not sent).

- [ ] **Step 1: Write failing tests** in `dispatch_test.go` (alongside the existing `user` case): service-kind uc → `authtype == "service"`; system-kind → `"system"`; a uc **with `ROLE_M2M` but Kind user** → `"user"` (regression: role-sniffing dead); nil uc → dispatch returns error, no callout emitted; unset Kind → same.
- [ ] **Step 2: Run — FAIL. Implement:** replace the role-sniff block (:45-52) with:

```go
func AttachAuthContext(ctx context.Context, ce *cepb.CloudEvent) error {
	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return errors.New("attach auth context: no user context on dispatch path")
	}
	if uc.Kind == "" {
		return fmt.Errorf("attach auth context: principal kind unset for principal %q", uc.UserID)
	}
	if ce == nil {
		return errors.New("attach auth context: nil cloud event")
	}
	// ... existing attribute map init ...
	ce.Attributes["authtype"] = &cepb.CloudEvent_CloudEventAttributeValue{
		Attr: &cepb.CloudEvent_CloudEventAttributeValue_CeString{CeString: string(uc.Kind)},
	}
	// authid / authclaims unchanged
	return nil
}
```

At `dispatch.go:113`, propagate the error (fail the dispatch — the transition fails closed with rollback per the engine's existing dispatch-error handling).
- [ ] **Step 3: Update `grpc.md:359`:** `authtype` values are `user` / `service` / `system` (was `service_account`), driven by explicit principal kind. Note the wire change plainly.
- [ ] **Step 4: Run `go test ./internal/grpc/... -v` — PASS. Commit** `feat(grpc)!: authtype from explicit principal kind; fail loud on unset kind`.

---

### Task 7: cyoda-go — cross-node Kind forwarding

**Files:**
- Modify: `internal/cluster/dispatch/types.go` (`DispatchCalloutRequest` :17)
- Modify: `internal/cluster/dispatch/cluster_dispatcher.go` (build sites ~:329, :349, :367)
- Modify: `internal/cluster/dispatch/handler.go` (`buildContext` :134-145)
- Test: `internal/cluster/dispatch/handler_test.go`

**Interfaces (Produces):** `DispatchCalloutRequest.Kind string \`json:"kind"\``; `buildContext` sets `UserContext.Kind`.

- [ ] **Step 1: Failing test:** round-trip a `DispatchCalloutRequest{UserID:"svc", Kind:"service", ...}` through `buildContext` → resulting ctx's `UserContext.Kind == spi.PrincipalService`; then `AttachAuthContext` on that ctx emits `authtype == "service"` (this is the cross-node G-assertion — without forwarding it would fail loud).
- [ ] **Step 2: Implement:** add the field; set `Kind: string(uc.Kind)` at all three build sites; `Kind: spi.PrincipalKind(req.Kind)` in `buildContext`.
- [ ] **Step 3: Run `go test ./internal/cluster/... -v` — PASS. Commit** `feat(cluster): forward principal kind on cross-node callout dispatch`.

---

### Task 8: cyoda-go — engine stamp rule at the four create/update sites

**Files:**
- Modify: `internal/domain/entity/service.go` (sites :289, :1190, :1404-1421, :1690-1755)
- Test: `internal/domain/entity/service_test.go` (extend)

**Interfaces (Consumes):** `spi.AttributionFor(txCtx)`. **Produces:** every create/update stamps `ChangeUser`=attributed.ID, `ChangeUserKind`=attributed.Kind, `ChangeExecutor`=executor.

- [ ] **Step 1: Failing unit test:** drive a create and an update through the service with (a) a plain user ctx → `ChangeUser==userID`, `ChangeUserKind==user`, `ChangeExecutor.ID==userID`; (b) a service-kind uc joined to a tx whose `Origin` is a user → `ChangeUser==origin user`, `ChangeExecutor.Kind==service`. Use the existing service-test harness (memory store) — the tx for (b) is set up via the store's `Begin` under the user ctx, then the write issued under the service ctx joined to it.
- [ ] **Step 2: Implement.** At each of the four sites, compute the pair **on the tx-carrying context** (the ctx that went through `beginOrJoin`, NOT the pre-Begin outer ctx — the :1404/:1690 reads move accordingly):

```go
attributed, executor := spi.AttributionFor(txCtx)
// in the EntityMeta literal:
ChangeUser:     attributed.ID,
ChangeUserKind: attributed.Kind,
ChangeExecutor: executor,
```

Delete the now-dead `changeUser := ""` guard blocks at :1404-1408 and :1690-1694 (`AttributionFor` is nil-safe).
- [ ] **Step 3: Run `go test ./internal/domain/entity/... -v` — PASS. Commit** `feat(entity): stamp attributed+executor per the attribution rule`.

---

### Task 9: memory plugin — origin at Begin, executor persistence, delete attribution

**Files (all under `plugins/memory/`, own go.mod — run tests from that dir):**
- Modify: `txmanager.go` (Begin :130-158; delete flush :397-412)
- Modify: `entity_store.go` (tx delete stage :479+; non-tx delete :526-537; DeleteAll :593-613; version write ~:280; version read ~:768)
- Test: `txmanager_test.go`, `entity_store_test.go` (extend); wire the new spitest suites (Task 3) into the plugin's conformance test entrypoint.

- [ ] **Step 1: Failing tests:** (a) `Begin` under user ctx → `tx.Origin == {user}`; Begin under ambient-seeded ctx → seeded origin; (b) staged tx delete by service-kind uc joined to a user-origin tx, committed by the *user* ctx → tombstone version has `User==origin user`, `Executor.Kind==service` (stager, not committer — fixes the committer stamp at :397-412); (c) non-tx delete → attributed==executor==caller; (d) save/read round-trip of `ChangeUserKind`/`ChangeExecutor` through `GetVersionHistory` (DELETED row's `Executor` populated with `Entity==nil`).
- [ ] **Step 2: Implement:**
  - Begin: `Origin: spi.ResolveOrigin(ctx)` in the `TransactionState` literal; init `DeleteAttribution: make(map[string]spi.WriteAttribution)`.
  - Tx delete stage: under the same OpMu section that sets `tx.Deletes[id] = true`, add `a, e := spi.AttributionFor(ctx); tx.DeleteAttribution[id] = spi.WriteAttribution{Attributed: a, Executor: e}`.
  - Flush (:397-412): use `tx.DeleteAttribution[id]` when present; fall back to `spi.AttributionFor(ctx)` (commit ctx) when absent; stamp `user`, attributed-kind, executor into the internal `entityVersion` record (add the two fields to that struct and every copy site).
  - Non-tx delete + DeleteAll: stamp via `spi.AttributionFor(ctx)`.
  - Version write/read: thread `ChangeUserKind`/`ChangeExecutor` ↔ `EntityVersion.AttributedKind`/`Executor` (populate independently of `Entity`).
  - Savepoints: wherever `Deletes` is snapshot/restored, snapshot/restore `DeleteAttribution` in the same block.
- [ ] **Step 3: `cd plugins/memory && go test ./... -v` — PASS (incl. the new spitest suites). Commit** `feat(memory): tx origin + delete/executor attribution`.

---

### Task 10: sqlite plugin — same, plus tombstone meta blob

**Files (under `plugins/sqlite/`):**
- Modify: `txmanager.go` (Begin :148-184; delete flush :509 — today hardcodes `user_id=''`)
- Modify: `entity_store.go` (`entityMetaDB` :39-53; tx delete stage :587+; non-tx delete :642-653 — tombstone inserts `meta=NULL` at :649-653; DeleteAll; read supplement :985-1012)
- Test: extend plugin tests; wire new spitest suites.

- [ ] **Step 1: Failing tests:** mirror Task 9 (a)-(d), plus: (e) a new tombstone row **has a meta blob** carrying attributed-kind/executor, while a legacy NULL-meta tombstone reads back with zero `Executor` and empty `AttributedKind` (no error).
- [ ] **Step 2: Implement:** Begin origin + `DeleteAttribution` init; stage-time capture identical to Task 9; flush at :509 writes `user_id = attribution.Attributed.ID` (bug fix — was `''`) and a marshaled meta blob (extend `entityMetaDB` with `ChangeUserKind`, `ChangeExecutor` fields) for tombstones; non-tx delete same; read supplement lifts `AttributedKind`/`Executor` from the blob (NULL blob → zero values; `User` continues to come from the `user_id` column). Savepoint pairing as Task 9. **No `ALTER TABLE`** — everything rides existing columns.
- [ ] **Step 3: `cd plugins/sqlite && go test ./... -v` — PASS. Commit** `feat(sqlite): tx origin + delete/executor attribution; tombstones stamp the deleter`.

---

### Task 11: postgres plugin — origin map lifecycle + doc serialization + delete stamping

**Files (under `plugins/postgres/`):**
- Modify: `transaction_manager.go` (Begin :60-101; Join :241-260; Commit/Rollback cleanup — mirror `tm.tenants` lifecycle)
- Modify: `entity_doc.go` (`entityMeta` :13-30; `marshalEntityDoc` :35-52; `unmarshalEntityDoc` :116-131; `unmarshalEntityVersion` :154-161)
- Modify: `entity_store.go` (`Delete` :266-337 — today re-marshals the PRIOR doc, so tombstones record the previous writer)
- Test: extend plugin tests (testcontainers — Docker required); wire new spitest suites.

- [ ] **Step 1: Failing tests:** (a) Begin captures origin AND a **Join from a second ctx returns state with the same `Origin`** (this is the load-bearing postgres case — Join rebuilds the struct); (b) after Commit/Rollback the per-tx origin entry is gone (no leak); (c) tombstone written via `Delete` records the deleting attribution, not the prior doc's writer; (d) meta round-trip of the new fields through `marshalEntityDoc`/`unmarshalEntityDoc`/`unmarshalEntityVersion` (version executor populated independently of `Entity`; legacy docs without the fields → zeros).
- [ ] **Step 2: Implement:** add `origins map[string]spi.Principal` beside `tm.tenants` (same mutex, same populate-at-Begin / delete-at-Commit-and-Rollback lifecycle); Begin sets `txSpiState.Origin = spi.ResolveOrigin(ctx)` and stores it; Join repopulates `Origin` from the map. Extend `entityMeta` with `ChangeUserKind string \`json:"changeUserKind,omitempty"\`` and `ChangeExecutorID`/`ChangeExecutorKind` (or an embedded object — match the doc's existing style), thread through all four (un)marshal functions. In `Delete`, overwrite the tombstone doc's attribution fields from `spi.AttributionFor(ctx)` before writing.
- [ ] **Step 3: `cd plugins/postgres && go test ./... -v` — PASS. Commit** `feat(postgres): per-tx origin repopulated at Join; tombstones stamp the deleter`.

---

### Task 12: cyoda-go — ArmedBy capture at both arm sites

**Files:**
- Modify: `internal/domain/workflow/arm.go` (:127-139 static; :235-247 function-resolved)
- Test: `internal/domain/workflow/arm_test.go` (extend)

- [ ] **Step 1: Failing test:** arming under (a) a user ctx inside that user's tx → task `ArmedBy == {user}`; (b) a service-kind ctx joined to a user-origin tx → `ArmedBy == chain origin (user)` (§5.2: arming uses origin, even though the OBO/service write-stamp may differ); (c) verify `armViaFunction`'s Function result affects **timing only** — `ArmedBy` unchanged by any callout output.
- [ ] **Step 2: Implement:** add `ArmedBy: spi.ResolveOrigin(ctx),` to both `spi.ScheduledTask` literals.
- [ ] **Step 3: Run `go test ./internal/domain/workflow/... -v` — PASS. Commit** `feat(workflow): capture arming principal (chain origin) on scheduled tasks`.

---

### Task 13: cyoda-go — fire path: durable seed, verify-or-abort, anchor stamp

**Files:**
- Modify: `internal/domain/workflow/fire_scheduled.go` (single engine door :53; Begin :75; in-tx re-read guard ~:115-152; anchor Save/CompareAndSave :321/:325)
- Test: `internal/domain/workflow/fire_scheduled_test.go` (extend)

**Interfaces (Consumes):** `spi.ScheduledTaskStore.Get(ctx, id)` (`../cyoda-go-spi/persistence.go:51`); `scheduler.SystemPrincipal()` — the workflow package must not import `internal/scheduler`; define the same principal locally or accept it via the engine's constructor (follow the existing seam used for the task store).

- [ ] **Step 1: Failing tests:** (a) fire of a task whose durable row has `ArmedBy={user}` → anchor version `User==user.ID`, `AttributedKind==user`, `Executor=={system}`; a cascade write during the fire also attributes to the user; (b) legacy row (zero ArmedBy) → attributed==system, never `"scheduler"`; (c) **verify-or-abort**: mutate the stored `ArmedBy` between the point-read and the in-tx re-read (test hook or direct store write) → fire returns an error, nothing committed; (d) the RPC-supplied `task` argument's `ArmedBy` is IGNORED — pass a forged value, assert the durable row wins (negative test, §9).
- [ ] **Step 2: Implement in `FireScheduledTransition`, before `Begin`:**

```go
// Seed the causal origin from the DURABLE row — never from the argument:
// the peer path passes an RPC-deserialized task (scheduler_rpc.go), and only
// task.ID is trusted from the argument (see contract above).
seeded := spi.Principal{}
if pre, found, err := e.taskStore.Get(ctx, task.ID); err != nil {
	return "", fmt.Errorf("point-read scheduled task before fire: %w", err)
} else if found {
	seeded = pre.ArmedBy
}
ctx = spi.WithAmbientOrigin(ctx, seeded) // zero → no seed → origin = system executor
```

Inside the tx, extend the existing re-read guard: `if cur.ArmedBy != seeded { rollback; return error "scheduled task re-armed concurrently (arming principal changed); will retry" }` (fail closed; scan-loop backoff retries). At the anchor Save/CompareAndSave:

```go
armed := cur.ArmedBy
if armed == (spi.Principal{}) {
	armed = systemPrincipal // {ID:"system", Kind:spi.PrincipalSystem}
}
entity.Meta.ChangeUser = armed.ID
entity.Meta.ChangeUserKind = armed.Kind
entity.Meta.ChangeExecutor = systemPrincipal
```

- [ ] **Step 3: Run `go test ./internal/domain/workflow/... ./internal/scheduler/... ./internal/cluster/... -v` — PASS. Commit** `feat(workflow): scheduled fire attributes to durable ArmedBy, executed by system`.

---

### Task 14: cyoda-go — /changes read API + OpenAPI

**Files:**
- Modify: `internal/domain/entity/service.go` (`EntityChangeEntry` :111-118; mapping :755-763)
- Modify: `internal/domain/entity/handler.go` (`GetEntityChangesMetadata` :590-611)
- Modify: `api/openapi.yaml` (`EntityChangeMeta` ~:10513-10536 — `user` stays **required**)
- Test: `internal/domain/entity/handler_test.go` (extend)

- [ ] **Step 1: Failing test:** history containing (a) a modern row → JSON entry has `"user"`, `"attributedKind"`, `"executedBy":{"id","kind"}`; (b) a legacy row (zero kind/executor) → **neither** `attributedKind` nor `executedBy` key present (no JSON null); `"user"` unchanged.
- [ ] **Step 2: Implement:** add `AttributedKind string` + `Executor spi.Principal` to `EntityChangeEntry`; map from `EntityVersion`; in the handler:

```go
if e.AttributedKind != "" {
	entry["attributedKind"] = e.AttributedKind
}
if e.Executor.ID != "" {
	entry["executedBy"] = map[string]any{"id": e.Executor.ID, "kind": string(e.Executor.Kind)}
}
```

OpenAPI: add optional `attributedKind` (string) and `executedBy` (`{id: string, kind: string}`) to `EntityChangeMeta` — typed-but-open policy (no `additionalProperties:false`), `user` remains required.
- [ ] **Step 3: Run `go test ./internal/domain/entity/... ./internal/api/... -v` — PASS (conformance/oasdiff-affecting tests included). Commit** `feat(api): surface attributedKind + executedBy on change history`.

---

### Task 15: cyoda-go — authctx SDK helper

**Files:**
- Create: `api/grpc/authctx/authctx.go`, `api/grpc/authctx/authctx_test.go` (§16 resolution: lives beside the CloudEvents types Go compute nodes already import)

**Interfaces (Produces):**
```go
func Type(ce *cepb.CloudEvent) string    // authtype attr, "" if absent
func ID(ce *cepb.CloudEvent) string      // authid attr
func Roles(ce *cepb.CloudEvent) []string // authclaims split on ","; nil if absent/empty
func Require(ce *cepb.CloudEvent, role string) bool // FAIL-CLOSED
```

- [ ] **Step 1: Failing tests:** `Require` returns false for: nil event; absent/empty `authclaims`; `authtype=="system"` even when the role is listed; role not in claims. Returns true only when authtype is `user`/`service` AND the role is present. `Roles` round-trips a comma-joined list.
- [ ] **Step 2: Implement** (package doc must state the trust basis verbatim from §10.1: rely on AuthContext only over a server-authenticated TLS connection; fail closed on empty claims — which includes the system case):

```go
func Require(ce *cepb.CloudEvent, role string) bool {
	if ce == nil || Type(ce) == string(spi.PrincipalSystem) {
		return false
	}
	rs := Roles(ce)
	if len(rs) == 0 {
		return false
	}
	return slices.Contains(rs, role)
}
```

- [ ] **Step 3: Run `go test ./api/grpc/authctx/... -v` — PASS. Commit** `feat(sdk): authctx helper with fail-closed role gate`.

---

### Task 16: E2E — cascade, CBD handover, D3, deletes, negative (single-backend, `internal/e2e`)

**Files:**
- Create: `internal/e2e/attribution_test.go` (use the compute-callback harness — `internal/e2e/callback_harness_test.go` — and existing workflow-import helpers)

- [ ] **Step 1: Write the scenarios as failing e2e tests** (each asserts via `GET /entity/{id}/changes`):
  - `TestAttribution_CascadeServiceProcessor`: user token transitions X; processor (M2M service token, **no user token anywhere on the callback**) saves Y inside the joined tx → Y's change: `user==<user>`, `attributedKind=="user"`, `executedBy.kind=="service"`.
  - `TestAttribution_TwoHopCascade`: X→Y→Z segmented (CBD `startNewTx=true` workflow) → Z attributes to the original user.
  - `TestAttribution_CBDDetachedHandover`: CBD `startNewTx=false`; processor callback saves Y with its service token → `user==<service id>`, `executedBy.kind=="service"` (handover, §4.3); repeat presenting an OBO user token → that user.
  - `TestAttribution_D3_OBOKeepsOwnUser`: OBO user-Y token joined into user-X's tx → write records Y; a timer armed in that same call carries chain origin X (assert after fire or by task inspection).
  - `TestAttribution_DeleteCascade`: processor deletes a second entity in the joined tx → tombstone `user==origin`, `executedBy.kind=="service"`.
  - `TestAttribution_NoRequestFieldSetsOrigin` (negative): entity save bodies / processor callout responses carrying spoofed `user`/`attributedKind`/`executedBy`/`armedBy` fields → recorded attribution unaffected.
- [ ] **Step 2: Run `go test ./internal/e2e/... -run TestAttribution -v` (Docker required) — make each pass; fix only test-harness issues here (engine behavior landed in Tasks 4-14).**
- [ ] **Step 3: Commit** `test(e2e): attribution scenarios — cascade, CBD handover, D3, deletes, negative`.

---

### Task 17: E2E — scheduled attribution (single-backend) 

**Files:**
- Extend: `internal/e2e/attribution_test.go` (or the existing scheduled-transition e2e file if one exists — check `internal/e2e/` for the schedule tests from the v0.8.3 scheduled-transition work and co-locate)

- [ ] **Step 1: Failing tests:** `TestAttribution_ScheduledUserArmed` — user arms via transition with a short `schedule.delayMs`; after fire, anchor change shows `user==<user>`, `executedBy=={"system","system"...}` — and **no version anywhere records `"scheduler"`**. `TestAttribution_ScheduledSystemChain` — scheduled fire re-arms a next hop; second fire still attributes faithfully (user-rooted → user). `TestAttribution_ScheduledAnchorStamped` — anchor's change user differs from the previous version's writer (regression for the stale-meta bug).
- [ ] **Step 2: Run, make pass, commit** `test(e2e): scheduled-fire attribution (user-armed, chains, anchor stamp)`.

---

### Task 18: Parity scenarios (cross-backend)

**Files:**
- Modify: `e2e/parity/registry.go` + new scenario file `e2e/parity/attribution.go`

- [ ] **Step 1:** Add parity scenarios for the backend-agnostic contract rows: tombstone attribution uniformity (delete under tx + non-tx → same recorded actors on every backend), executor round-trip on `/changes` incl. legacy-row omission, scheduled `ArmedBy` fire attribution, cascade attribution **if** the parity harness supports compute callbacks — check `e2e/parity/` for the callback member used by existing processor parity scenarios and reuse it. If (and only if) the harness has no compute-callback support, cover the cascade rows in `internal/e2e` per-backend instead and record the one-line waiver in the PR body per `.claude/rules/test-coverage.md`.
- [ ] **Step 2:** Register in `registry.go`; run parity across memory/sqlite/postgres: `go test ./e2e/parity/... -v`. Commit `test(parity): attribution scenarios across backends`.

---

### Task 19: Multinode e2e — cross-node cascade + scheduled fire

**Files:**
- Extend the existing multinode e2e suite (locate `TestMultiNode*` under `internal/e2e/`)

- [ ] **Step 1: Failing tests:** (a) cross-node proxied-join cascade — processor callback lands on the non-owner node; joined write attributes to the originating user identically to same-node (postgres Join repopulation is the load-bearing path); (b) cross-node scheduled fire — task armed on node A by a user, fired via peer RPC on node B → attributed to the user (durable `ArmedBy`, ambient seed inside `FireScheduledTransition` which the peer path enters directly); (c) cross-node callout `authtype` — peer-dispatched processor receives the executor's true kind (Task 7 forwarding).
- [ ] **Step 2: Run, make pass, commit** `test(e2e): multinode attribution parity (proxied join, peer fire, authtype)`.

---

### Task 20: Docs — cloud-parity contract, execution-modes boundary, README/help

**Files:**
- Create: `docs/cloud-parity/authcontext-attribution.md`
- Modify: `docs/PROCESSOR_EXECUTION_MODES.md` (+ the corresponding help topic if one exists under `cmd/cyoda/help/content/`)
- Verify (done in earlier tasks, re-check now): `cmd/cyoda/help/content/grpc.md`, config topic + `README.md` for `CYODA_IAM_MOCK_KIND`

- [ ] **Step 1: Write `docs/cloud-parity/authcontext-attribution.md`** (compact, per the repo's one-file-per-behaviour convention): the pinned AuthContext contract (`authtype ∈ {user,service,system}` from explicit kind — `service_account` retired; `authid`; `authclaims`; fail-loud emission; TLS trust basis; fail-closed application checks), the attributed/executor pair on change history (`user`/`attributedKind`/`executedBy`), attribution semantics per follow-on kind (joined cascade → tx origin; scheduled → durable arming principal; CBD-detached → handed to the application), and the wire break note (`service_account`→`service`).
- [ ] **Step 2: Add to `PROCESSOR_EXECUTION_MODES.md`** under COMMIT_BEFORE_DISPATCH: with `startNewTx=false`, callback writes are ordinary direct requests — the platform records the identity the callback presents; applications wanting user attribution present it themselves (the callout's AuthContext carries the causal principal). No issue IDs; compact prose.
- [ ] **Step 3: Commit** `docs: authcontext-attribution cloud-parity contract + CBD handover boundary`.

---

### Task 21: SPI repin + full verification

**Files:**
- Modify: `go.mod` + `plugins/memory/go.mod` + `plugins/sqlite/go.mod` + `plugins/postgres/go.mod` (pseudo-version pins)

- [ ] **Step 1: Repin** all four modules to the SPI commit merged in Task 3: in each module dir, `GOFLAGS=-mod=mod go get github.com/cyoda-platform/cyoda-go-spi@<SHA> && go mod tidy`. Check for a `make repin-plugins` target first and use it if present. Stage the four `go.mod`/`go.sum` pairs **explicitly** (never `git add -A` — go.work rule).
- [ ] **Step 2: Full verification (Gate 5):**
  - `make test-all` (root + all plugins; Docker running) — green.
  - `go vet ./...` at root and in each plugin dir — clean.
  - `make race` (CI-parity scope, one-shot end-of-deliverable) — green.
  - `go test ./internal/e2e/... -v` — green (includes TestErrCode_Parity: no new codes).
  - Confirm the OpenAPI breaking-change check passes (`user` still required; additions optional).
- [ ] **Step 3: Coverage-matrix audit:** walk spec §14 row by row; every row maps to a test added in Tasks 4-19 (or a recorded waiver from Task 18). Fix any gap before proceeding.
- [ ] **Step 4: Commit** `chore: repin cyoda-go-spi across all modules` — then hand off to superpowers:verification-before-completion → requesting-code-review → security-review → PR onto `release/v0.8.3` (milestone `v0.8.3`, `Closes #430` in the body).

---

## Self-review notes (performed)

- **Spec coverage:** §2/§3→Tasks 1,4-7; §4.1-4.2→Tasks 1,9-11; §4.3→Tasks 16,20 (no mechanism — handover); §4.4→Task 16; §5→Tasks 2,12,13,17; §6→Tasks 2,14; §7→Tasks 1,8-11; §8→Tasks 7,19; §9→Tasks 13(d),16; §10.1→Tasks 6,20; §10.2→Task 15; §11→Tasks 1-3,21; §12→legacy-row/zero-value tests in Tasks 2,9-11,14; §13→no new codes (Task 6 fail-loud is a dispatch failure inside existing envelopes); §14→Tasks 16-19 + §21 audit; §16 items resolved: authctx location = `api/grpc/authctx` (Task 15), scheduler id = reuse `"system"` (Task 5), audit-read inheritance confirmed intended (derives from `v.User`, Task 14 asserts).
- **Type consistency:** `Principal`/`WriteAttribution`/`DeleteAttribution`/`ArmedBy`/`ChangeUserKind`/`ChangeExecutor`/`AttributedKind`/`Executor`/`Kind` names used identically across all tasks; the only cross-repo seam is Task 3's SHA consumed by Task 21.
