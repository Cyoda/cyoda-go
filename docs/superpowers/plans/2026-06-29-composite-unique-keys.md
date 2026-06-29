# Composite Unique Keys Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. **If a task surfaces an unspecified decision, STOP and surface it — never assume or descope.**

**Goal:** Let an entity model declare composite UNIQUE keys over scalar fields, enforced on create/update across the memory, sqlite, and postgres engines.

**Architecture (settled — see spec):** Key DEFINITIONS live on `spi.ModelDescriptor.UniqueKeys` (inside each model store's existing descriptor blob, no migration). The handler resolves keys from the descriptor it already loads for validation and sets them on the request **context** (`spi.WithUniqueKeys`); the workflow engine's internal CBD saves inherit them via `context.WithoutCancel`. The **store** reads keys from context inside `Save`, computes claims from the **live** `entity.Data` via the shared SPI helper `spi.ComputeClaims`, and enforces (postgres/sqlite `unique_claims` side table + UNIQUE index; memory map at the commit critical section). A backend advertises support via an additive optional interface; declaring keys on an unsupported backend is rejected at the one declaration endpoint.

**Tech Stack:** Go 1.26, `cyoda-go-spi` (external module, composed via `go.work`), pgx/v5, modernc/mattn sqlite, testcontainers-go, stdlib `math/big`, `tidwall/gjson` (root only; SPI uses segment traversal).

**Design spec (authoritative):** `docs/superpowers/specs/2026-06-28-composite-unique-keys-design.md` — read it first.

## Global Constraints

- Go 1.26.4 across root + all `plugins/*` modules (each own `go.mod`).
- **SPI is external** — composed locally via `go.work` (`skip-worktree`, never a committed `replace`). SPI changes tag FIRST; cyoda-go pin bump is the FINAL commit (Phase 9). See `MAINTAINING.md`.
- `log/slog` only; wrap errors `fmt.Errorf("...: %w", err)`; `uuid.UUID` not `string`.
- 4xx: full domain detail + code; 5xx: generic + ticket. **All new error codes are non-retryable.**
- Every new `ErrCode*` needs `cmd/cyoda/help/content/errors/<CODE>.md` (`TestErrCode_Parity`, `cmd/cyoda/help/help_test.go:532`).
- Uniqueness scope: per `(tenant, model, version)`, **live entities only**; all-or-nothing null; **byte-exact** strings; precision-preserving numbers (bounded input, then stdlib `math/big`; never unbounded `big.Rat`).
- Commercial (Cassandra) backend out of scope — must NOT implement the capability interface.
- TDD: every task RED → GREEN → commit. `go vet ./...` + `go build ./...` green before each commit. E2E/parity need Docker.

---

## Phase 0 — SPI additive changes (composed via go.work)

> All Phase 0 edits are in the **cyoda-go-spi** repo, composed via `go.work`. They ship in the SPI tag (Phase 9).

### Task 0.1: Locate/clone cyoda-go-spi and wire go.work

**Files:** Modify `go.work` (repo root — local only, skip-worktree)

- [ ] **Step 1:** Locate or clone the SPI checkout:
```bash
ls ../cyoda-go-spi/go.mod 2>/dev/null || git clone https://github.com/Cyoda-platform/cyoda-go-spi.git ../cyoda-go-spi
( cd ../cyoda-go-spi && git checkout main && git pull --ff-only )
```
- [ ] **Step 2:** Add to go.work, protect from commits:
```bash
go work edit -use <relative-path-to-cyoda-go-spi-checkout>
git update-index --skip-worktree go.work
go build ./...   # builds against local SPI
```
Expected: build succeeds; `git status` does not show `go.work`.
- [ ] **Step 3:** No commit (go.work is skip-worktree).

### Task 0.2: SPI types + `ModelDescriptor.UniqueKeys` field

**Files (../cyoda-go-spi):** Create `unique.go`; Modify `types.go` (`ModelDescriptor`); Test `unique_test.go`

**Interfaces — Produces:** `spi.UniqueKey{ ID string; Fields []string }`, `spi.UniqueClaim{ KeyID string; Signature string }`, and `ModelDescriptor.UniqueKeys []UniqueKey` (durable). **No `spi.Entity` field.**

- [ ] **Step 1: failing test** (`unique_test.go`):
```go
package spi
import "testing"
func TestModelDescriptorUniqueKeys(t *testing.T) {
	d := ModelDescriptor{UniqueKeys: []UniqueKey{{ID: "byEmail", Fields: []string{"$.email"}}}}
	if d.UniqueKeys[0].ID != "byEmail" || d.UniqueKeys[0].Fields[0] != "$.email" {
		t.Fatalf("unique keys not carried: %+v", d.UniqueKeys)
	}
	_ = UniqueClaim{KeyID: "byEmail", Signature: "s5:Alice"}
}
```
- [ ] **Step 2:** `cd ../cyoda-go-spi && go test ./... -run TestModelDescriptorUniqueKeys` → FAIL (undefined).
- [ ] **Step 3:** `unique.go`:
```go
package spi

// UniqueKey is a model-level composite unique key over scalar leaf fields.
// Fields are ordered dotted JSONPath leaves (same form as the schema's field paths).
type UniqueKey struct {
	ID     string
	Fields []string
}

// UniqueClaim is a computed assertion: the store must guarantee no OTHER live
// entity in the same (tenant, model name, model version) holds the same
// (KeyID, Signature). Signature is an opaque, type-tagged canonical encoding.
type UniqueClaim struct {
	KeyID     string
	Signature string
}
```
In `types.go`, add to `ModelDescriptor` (keep Ref/State/ChangeLevel/UpdateDate/Schema):
```go
	// UniqueKeys are the model's composite unique-key definitions. Additive;
	// persisted inside the descriptor by each model store. Empty = none.
	UniqueKeys []UniqueKey
```
- [ ] **Step 4:** test PASS.
- [ ] **Step 5:** `cd ../cyoda-go-spi && git add types.go unique.go unique_test.go && git commit -m "feat: add UniqueKey/UniqueClaim + ModelDescriptor.UniqueKeys"`

### Task 0.3: Sentinels + capability interface

**Files (../cyoda-go-spi):** Modify `errors.go`, `unique.go`; Test `unique_test.go`

**Interfaces — Produces:** `spi.ErrUniqueViolation` (distinct from `ErrConflict`); `spi.ErrPartialUniqueKey` (the **umbrella** for all `ComputeClaims` value-invalid errors); `spi.CompositeUniqueKeyCapable interface { SupportsCompositeUniqueKeys() bool }`.

- [ ] **Step 1: failing test:**
```go
import "errors"
func TestUniqueSentinels(t *testing.T) {
	if errors.Is(ErrUniqueViolation, ErrConflict) { t.Fatal("must not equal ErrConflict") }
	if errors.Is(ErrPartialUniqueKey, ErrUniqueViolation) { t.Fatal("partial != violation") }
}
type capYes struct{}
func (capYes) SupportsCompositeUniqueKeys() bool { return true }
func TestCapable(t *testing.T) {
	var v any = capYes{}
	if c, ok := v.(CompositeUniqueKeyCapable); !ok || !c.SupportsCompositeUniqueKeys() { t.Fatal("not capable") }
}
```
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3:** `errors.go`:
```go
// ErrUniqueViolation: a write would duplicate a declared composite unique key.
// Deterministic, NON-retryable (distinct from ErrConflict).
var ErrUniqueViolation = errors.New("composite unique key violation")

// ErrPartialUniqueKey is the umbrella for every ComputeClaims VALUE-invalid
// error — a partially-filled key, an over-bound numeric literal, or a
// non-scalar value at a key path. All map to 422 INVALID_UNIQUE_KEY.
var ErrPartialUniqueKey = errors.New("invalid composite unique key value")
```
`unique.go`:
```go
// CompositeUniqueKeyCapable is OPTIONAL on a StoreFactory: advertises composite
// unique-key support. Absence (or false) = unsupported. Additive; NOT part of
// the StoreFactory interface.
type CompositeUniqueKeyCapable interface {
	SupportsCompositeUniqueKeys() bool
}
```
- [ ] **Step 4:** test PASS.
- [ ] **Step 5:** commit (SPI repo): `feat: add ErrUniqueViolation/ErrPartialUniqueKey + CompositeUniqueKeyCapable`

### Task 0.4: Context helpers

**Files (../cyoda-go-spi):** Create `unique_context.go`; Test `unique_context_test.go`

**Interfaces — Produces:** `spi.WithUniqueKeys(ctx, []UniqueKey) context.Context`, `spi.UniqueKeysFromContext(ctx) []UniqueKey` (nil if absent).

- [ ] **Step 1: failing test:**
```go
package spi
import ("context"; "testing")
func TestUniqueKeysContext(t *testing.T) {
	ctx := WithUniqueKeys(context.Background(), []UniqueKey{{ID: "k", Fields: []string{"$.a"}}})
	if got := UniqueKeysFromContext(ctx); len(got) != 1 || got[0].ID != "k" { t.Fatalf("got %+v", got) }
	if UniqueKeysFromContext(context.Background()) != nil { t.Fatal("absent must be nil") }
}
```
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3:** `unique_context.go`:
```go
package spi
import "context"
type uniqueKeysCtxKey struct{}
func WithUniqueKeys(ctx context.Context, keys []UniqueKey) context.Context {
	return context.WithValue(ctx, uniqueKeysCtxKey{}, keys)
}
func UniqueKeysFromContext(ctx context.Context) []UniqueKey {
	if v, ok := ctx.Value(uniqueKeysCtxKey{}).([]UniqueKey); ok { return v }
	return nil
}
```
- [ ] **Step 4:** test PASS. — [ ] **Step 5:** commit (SPI): `feat: add WithUniqueKeys/UniqueKeysFromContext context helpers`

### Task 0.5: `ComputeClaims` signature helper (segment extraction, bounded canonicalization)

**Files (../cyoda-go-spi):** Create `unique_signature.go`; Test `unique_signature_test.go`

**Interfaces — Produces:** `spi.ComputeClaims(keys []UniqueKey, doc []byte) ([]UniqueClaim, error)`. Emits a claim only for a fully-present key; all-null/absent ⇒ none; partial / over-bound numeric / non-scalar-at-path ⇒ an error wrapping `ErrPartialUniqueKey`.

> Resolve paths **by segment**, splitting each dotted path the way the schema *constructs* it (no raw gjson query). Canonicalize numbers with **bounded input + stdlib `math/big`** (no new dep). Constants: `maxNumDigits`, `maxNumExp` (pin sane values, e.g. 64 / 6144).

- [ ] **Step 1: failing test** (`unique_signature_test.go`) — covers fully-present, numeric-canonical (`1`/`1.0`/`1e0`/`1E0`/`-0` collide), large-int (>2^53 distinct), type-tag (`"1"`≠`1`), byte-exact strings, all-null exempt, partial ⇒ `ErrPartialUniqueKey`, over-bound numeric ⇒ `ErrPartialUniqueKey`, non-scalar (object/array at path) ⇒ `ErrPartialUniqueKey`, nested path `$.a.b`:
```go
package spi
import ("errors"; "testing")
func ks() []UniqueKey { return []UniqueKey{{ID: "k", Fields: []string{"$.email", "$.age"}}} }
func TestComputeClaims_Full(t *testing.T){ c,e:=ComputeClaims(ks(),[]byte(`{"email":"a@x.com","age":42}`)); if e!=nil||len(c)!=1{t.Fatalf("%+v %v",c,e)} }
func TestComputeClaims_NumCanon(t *testing.T){
	a,_:=ComputeClaims(ks(),[]byte(`{"email":"a","age":42}`)); b,_:=ComputeClaims(ks(),[]byte(`{"email":"a","age":42.0}`)); d,_:=ComputeClaims(ks(),[]byte(`{"email":"a","age":4.2e1}`))
	if a[0].Signature!=b[0].Signature||b[0].Signature!=d[0].Signature{t.Fatal("42/42.0/4.2e1 must collide")}
}
func TestComputeClaims_BigInt(t *testing.T){ a,_:=ComputeClaims(ks(),[]byte(`{"email":"a","age":9007199254740993}`)); b,_:=ComputeClaims(ks(),[]byte(`{"email":"a","age":9007199254740992}`)); if a[0].Signature==b[0].Signature{t.Fatal(">2^53 must differ")} }
func TestComputeClaims_TypeTag(t *testing.T){ a,_:=ComputeClaims([]UniqueKey{{ID:"k",Fields:[]string{"$.v"}}},[]byte(`{"v":"1"}`)); b,_:=ComputeClaims([]UniqueKey{{ID:"k",Fields:[]string{"$.v"}}},[]byte(`{"v":1}`)); if a[0].Signature==b[0].Signature{t.Fatal(`"1" != 1`)} }
func TestComputeClaims_AllNull(t *testing.T){ c,e:=ComputeClaims(ks(),[]byte(`{"email":null,"age":null}`)); if e!=nil||len(c)!=0{t.Fatalf("exempt: %+v %v",c,e)} }
func TestComputeClaims_Partial(t *testing.T){ _,e:=ComputeClaims(ks(),[]byte(`{"email":"a"}`)); if !errors.Is(e,ErrPartialUniqueKey){t.Fatalf("got %v",e)} }
func TestComputeClaims_OverBound(t *testing.T){ _,e:=ComputeClaims([]UniqueKey{{ID:"k",Fields:[]string{"$.v"}}},[]byte(`{"v":1e1000000000}`)); if !errors.Is(e,ErrPartialUniqueKey){t.Fatalf("over-bound must reject pre-materialization, got %v",e)} }
func TestComputeClaims_NonScalar(t *testing.T){ _,e:=ComputeClaims([]UniqueKey{{ID:"k",Fields:[]string{"$.v"}}},[]byte(`{"v":{"x":1}}`)); if !errors.Is(e,ErrPartialUniqueKey){t.Fatalf("non-scalar must reject, got %v",e)} }
func TestComputeClaims_Nested(t *testing.T){ c,e:=ComputeClaims([]UniqueKey{{ID:"k",Fields:[]string{"$.a.b"}}},[]byte(`{"a":{"b":7}}`)); if e!=nil||len(c)!=1{t.Fatalf("nested: %+v %v",c,e)} }
```
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3:** `unique_signature.go` — decode with `encoding/json` + `UseNumber()` to a `map[string]any` (segment walk; `json.Number` preserved). Per field: split path on `.` (drop leading `$`), walk maps by segment; missing/`nil` ⇒ absent; a `map`/`[]any` at the leaf ⇒ wrap `ErrPartialUniqueKey` (non-scalar). Scalars → type-tagged token: string `s<len>:<bytes>`; bool `b:true`/`b:false`; number via `canonNum`. All-absent ⇒ skip; any-present-but-not-all ⇒ wrap `ErrPartialUniqueKey`. Join tokens with `\x1f`.
```go
func canonNum(n json.Number) (string, error) {
	s := string(n)
	// Bound BEFORE materialization (DoS): reject oversized coefficient/exponent.
	if digits, exp := countDigitsExp(s); digits > maxNumDigits || exp > maxNumExp || exp < -maxNumExp {
		return "", fmt.Errorf("%w: numeric literal out of bounds", ErrPartialUniqueKey)
	}
	r, ok := new(big.Rat).SetString(s) // safe: input is bounded
	if !ok { return "", fmt.Errorf("%w: uncanonicalizable number %q", ErrPartialUniqueKey, s) }
	return "n:" + r.RatString(), nil // lowest terms; integers => "a"; -0 => "0"
}
```
> `countDigitsExp` parses the literal's significant-digit count and exponent from the string (handle `e`/`E`, sign, leading zeros) without materializing. Pin `maxNumDigits`/`maxNumExp` and test the boundary.
- [ ] **Step 4:** test PASS (all cases). — [ ] **Step 5:** commit (SPI): `feat: ComputeClaims segment extraction + bounded canonicalization`

---

## Phase 1 — Validation + per-engine descriptor persistence (cyoda-go)

### Task 1.1: `schema.UniqueKey` alias + `ValidateUniqueKeys`

**Files:** Create `internal/domain/model/schema/uniquekey.go`, `uniquekey_validate.go`; Test `uniquekey_validate_test.go`

**Interfaces — Produces:** `type UniqueKey = spi.UniqueKey` (alias); `schema.ValidateUniqueKeys(n *ModelNode, keys []spi.UniqueKey) error` → `*UniqueKeyDefError`.

- [ ] **Step 1: failing tests** — OK case; unknown path; array path rejected; empty fields; dup id; dup field within key; non-scalar (object) path rejected. (Mirror the structure used in `field_test.go`; build a `ModelNode` with scalar leaves + an array via `NewObjectNode`/`NewLeafNode`.)
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3:** `uniquekey.go`: `package schema; import spi "github.com/cyoda-platform/cyoda-go-spi"; type UniqueKey = spi.UniqueKey`. `uniquekey_validate.go`: `UniqueKeyDefError{Reason string}`; `ValidateUniqueKeys` walks `n.Fields()` (the canonical leaf paths), builds the scalar-leaf set (reject `IsArray` and any path containing `[`/`*`), then checks each key: id non-empty + unique; fields non-empty + distinct; every field ∈ scalar-leaf set (else reason "not a known scalar leaf").
- [ ] **Step 4:** PASS; `go test ./internal/domain/model/schema/` green. — [ ] **Step 5:** commit: `feat(schema): UniqueKey alias + ValidateUniqueKeys`

### Task 1.2: Per-engine model-store persistence of `UniqueKeys` (NO migration)

**Files:** Modify `plugins/postgres/model_store.go` (private `modelDoc` struct + marshal/unmarshal), `plugins/sqlite/model_store.go` (same), `plugins/memory/model_store.go` (`cloneDescriptor` deep-copy). Tests in each plugin.

**Interfaces — Produces:** `ModelStore.Save`/`Get` round-trip `ModelDescriptor.UniqueKeys` for every backend.

- [ ] **Step 1: failing test** (per plugin): Save a descriptor with `UniqueKeys`, `Get` it back, assert equal. For sqlite **also** test a lifecycle RMW (`Lock` then `Get`) preserves `UniqueKeys` (the #9 strip hazard). For memory, mutate the returned slice and assert the stored one is unaffected (deep-copy).
- [ ] **Step 2:** run (per plugin module) → FAIL.
- [ ] **Step 3:** Add `UniqueKeys []spi.UniqueKey` to the private `modelDoc` struct (postgres `model_store.go:28-37`, sqlite analogous) and copy it in both the marshal (`Save`) and unmarshal (`Get`/`unmarshalModelDoc`) paths — **the struct field is what makes the RMW ops preserve it**. Memory: extend `cloneDescriptor` (`plugins/memory/model_store.go:16-23`) to `append([]spi.UniqueKey(nil), d.UniqueKeys...)`. **No DDL** — it rides in the existing `doc`/blob.
- [ ] **Step 4:** PASS in all three plugin modules (postgres needs Docker). — [ ] **Step 5:** commit: `feat(plugins): persist ModelDescriptor.UniqueKeys in the descriptor blob`

---

## Phase 2 — Declaration surface + capability gate

### Task 2.1: Per-plugin capability advertisement

**Files:** Modify each plugin's `StoreFactory` (`store_factory.go`); Test per plugin.

- [ ] **Step 1: failing test** (per plugin): assert `*StoreFactory` satisfies `spi.CompositeUniqueKeyCapable` and returns true.
- [ ] **Step 2:** FAIL. — [ ] **Step 3:** add `func (f *StoreFactory) SupportsCompositeUniqueKeys() bool { return true }` to memory/sqlite/postgres (real receiver names). — [ ] **Step 4:** PASS. — [ ] **Step 5:** commit: `feat(plugins): advertise CompositeUniqueKeyCapable`

### Task 2.2: `SetUniqueKeys` service method (capability + validation + persist + preserve)

**Files:** Modify `internal/domain/model/service.go` (new `SetUniqueKeys`; teach `ImportModel` to preserve `UniqueKeys` like `ChangeLevel` + re-validate); Test `service_test.go`.

**Interfaces — Consumes:** `schema.ValidateUniqueKeys`, `spi.CompositeUniqueKeyCapable`, `ModelStore.Get/Save`. **Produces:** `func (h *Handler) SetUniqueKeys(ctx, entityName, modelVersion string, keys []spi.UniqueKey) (*ModelTransitionResult, error)`.

- [ ] **Step 1: failing tests** — unsupported backend ⇒ 422 `COMPOSITE_KEY_UNSUPPORTED`; LOCKED model ⇒ 409 `MODEL_ALREADY_LOCKED`; unknown field ⇒ 422 `INVALID_UNIQUE_KEY_DEFINITION`; happy path persists to `desc.UniqueKeys`; **re-import preserves keys** (set keys, re-`ImportModel`, assert keys still present); **re-import dropping a key's field is rejected**.
- [ ] **Step 2:** FAIL (codes from Task 3.1 must exist — land 3.1 first if doing strict order).
- [ ] **Step 3:** `SetUniqueKeys` mirrors `LockModel`'s shape: capability gate FIRST (`h.factory.(spi.CompositeUniqueKeyCapable)`); `getModelFresh`; not-found → 404; `desc.State == ModelLocked` → 409 `MODEL_ALREADY_LOCKED`; `schema.Unmarshal(desc.Schema)` → `ValidateUniqueKeys(node, keys)` (`*UniqueKeyDefError` → 422 `INVALID_UNIQUE_KEY_DEFINITION`); set `desc.UniqueKeys = keys`; `store.Save`. In `ImportModel` (`service.go:148-156`), when `existing != nil` copy `existing.UniqueKeys` forward (like `ChangeLevel`) and re-run `ValidateUniqueKeys` against the merged node (reject the import if a referenced field vanished).
- [ ] **Step 4:** PASS. — [ ] **Step 5:** commit: `feat(model): SetUniqueKeys + preserve keys across re-import`

### Task 2.3: HTTP endpoint + OpenAPI + gRPC event + ExportModel includes keys

**Files:** Modify `api/openapi.yaml` (+ regenerate `api/generated.go`), `internal/domain/model/handler.go` (`SetEntityModelUniqueKeys`), `internal/grpc/model.go` (new `EntityModelManage` event), `internal/domain/model/service.go` (`ExportModel` includes `UniqueKeys`); Tests `handler_test.go`, `grpc/model_test.go`.

- [ ] **Step 1: failing tests** — HTTP `PUT /model/{entityName}/{modelVersion}/unique-keys`: 200 valid; 409 locked; 422 bad field; 422 unsupported backend. gRPC: envelope `Success=false`, `Error.Code` for each. Export: a model with keys, exported, includes `UniqueKeys`.
- [ ] **Step 2:** FAIL. — [ ] **Step 3:** add the OpenAPI path (request body `{ "uniqueKeys": [ { "id", "fields[] } ] }`, lowercase JSON → mapped to `[]spi.UniqueKey`) + regenerate; wire handler → `h.svc.SetUniqueKeys`; add gRPC event branch; extend `ExportModel` to emit `UniqueKeys`. — [ ] **Step 4:** PASS (`go test ./internal/domain/model/... ./internal/grpc/...`). — [ ] **Step 5:** commit: `feat(model): unique-keys declaration endpoint (HTTP+gRPC) + export`

---

## Phase 3 — Error codes & mapping (incl. C2 + F3)

### Task 3.1: Codes + help topics + non-retryable mapping + classifyWorkflowError

**Files:** Modify `internal/common/error_codes.go`, `internal/common/errors.go` (`Internal` branches), `internal/domain/entity/service.go` (`classifyWorkflowError`); Create the four `errors/<CODE>.md`; Tests `errors_test.go`, plus `TestErrCode_Parity`.

**Interfaces — Produces:** `ErrCodeUniqueViolation="UNIQUE_VIOLATION"`, `ErrCodeInvalidUniqueKey="INVALID_UNIQUE_KEY"`, `ErrCodeCompositeKeyUnsupported="COMPOSITE_KEY_UNSUPPORTED"`, `ErrCodeInvalidUniqueKeyDefinition="INVALID_UNIQUE_KEY_DEFINITION"`.

- [ ] **Step 1: failing tests:**
```go
func TestInternal_UniqueViolation(t *testing.T){ e:=Internal("save",fmt.Errorf("w: %w",spi.ErrUniqueViolation)); if e.Status!=409||e.Code!=ErrCodeUniqueViolation||e.Retryable{t.Fatalf("%d %s retry=%v",e.Status,e.Code,e.Retryable)} }
func TestInternal_PartialKey(t *testing.T){ e:=Internal("save",fmt.Errorf("w: %w",spi.ErrPartialUniqueKey)); if e.Status!=422||e.Code!=ErrCodeInvalidUniqueKey||e.Retryable{t.Fatalf("%d %s",e.Status,e.Code)} }
func TestInternal_ConflictStillRetryable(t *testing.T){ e:=Internal("c",fmt.Errorf("w: %w",spi.ErrConflict)); if e.Code!=ErrCodeConflict||!e.Retryable{t.Fatal("conflict retryable")} }
```
Plus a `classifyWorkflowError` test: an error wrapping `spi.ErrUniqueViolation` → 409 (not 400 `WORKFLOW_FAILED`); wrapping `spi.ErrPartialUniqueKey` → 422; with **no raw `err.Error()` in the message**.
- [ ] **Step 2:** FAIL; `go test ./cmd/cyoda/help/ -run TestErrCode_Parity` FAIL once codes added (drives the `.md` files).
- [ ] **Step 3:** add the four constants. In `errors.go` `Internal`, add **before** the `ErrConflict` branch (more specific first): `errors.Is(err, spi.ErrUniqueViolation)` → `Operational(409, UNIQUE_VIOLATION, "…")` (no `.AsRetryable()`); `errors.Is(err, spi.ErrPartialUniqueKey)` → `Operational(422, INVALID_UNIQUE_KEY, "…")`. In `classifyWorkflowError` (`service.go:1534`), **before** the `WORKFLOW_FAILED` catch-all (`:1541`), add the same two `errors.Is` branches returning the **same fixed sanitized messages** (never `err.Error()`). Create the four topics (follow `CONFLICT.md`; Retryable `no`; HTTP 409 for UNIQUE_VIOLATION, 422 for the rest).
- [ ] **Step 4:** PASS (`go test ./internal/common/ ./internal/domain/entity/ ./cmd/cyoda/help/`). — [ ] **Step 5:** commit: `feat(errors): composite-key codes + non-retryable + C2 workflow routing`

---

## Phase 4 — Service: resolve keys, thread via context, partial pre-check

### Task 4.1: Single writes — set context + pre-check

**Files:** Modify `internal/domain/entity/service.go` (`CreateEntity`, `updateEntityCore`); Test `service_test.go`.

**Interfaces — Consumes:** `spi.WithUniqueKeys`, `spi.ComputeClaims`, `spi.ErrPartialUniqueKey`. **Produces:** every write context carries the model's keys; partial input ⇒ 422 before `engine.Execute`.

- [ ] **Step 1: failing tests** — create with a partial key ⇒ 422 `INVALID_UNIQUE_KEY`; create with a full key ⇒ context carries keys (assert via a spy store reading `spi.UniqueKeysFromContext`); PATCH that nulls a key field on the merged doc ⇒ 422.
- [ ] **Step 2:** FAIL. — [ ] **Step 3:** add a helper:
```go
// withUniqueKeys attaches the model's keys to ctx and pre-checks the input doc.
func (h *Handler) withUniqueKeys(ctx context.Context, desc *spi.ModelDescriptor, inputDoc []byte) (context.Context, error) {
	if len(desc.UniqueKeys) == 0 { return ctx, nil }
	if _, err := spi.ComputeClaims(desc.UniqueKeys, inputDoc); err != nil {
		if errors.Is(err, spi.ErrPartialUniqueKey) {
			return ctx, common.Operational(http.StatusUnprocessableEntity, common.ErrCodeInvalidUniqueKey, "composite unique key incomplete")
		}
		return ctx, common.Internal("failed to evaluate unique keys", err)
	}
	return spi.WithUniqueKeys(ctx, desc.UniqueKeys), nil
}
```
`CreateEntity`: after `desc` load + body parse, **before `txMgr.Begin`**, `ctx, err = h.withUniqueKeys(ctx, desc, bodyBytes)` → on err return it; thread the returned ctx into `Begin`/`Execute`/`Save`. `updateEntityCore`: after the PATCH merge (`bodyBytes` is the merged doc) and validation, `txCtx, err = h.withUniqueKeys(txCtx, desc, bodyBytes)`.
- [ ] **Step 4:** PASS. — [ ] **Step 5:** commit: `feat(entity): thread unique keys via context + partial pre-check (single writes)`

### Task 4.2: Batch writes — per-item context

**Files:** Modify `internal/domain/entity/service.go` (`CreateEntityCollection`, `UpdateEntityCollection`); Test `service_test.go`.

- [ ] **Step 1: failing test** — a **mixed-model batch** where item A's model has a key and item B's doesn't (or a different key); assert each item's save sees the right keys (spy store), and a partial key on one item ⇒ 422 for the batch.
- [ ] **Step 2:** FAIL. — [ ] **Step 3:** `CreateEntityCollection`: add `uniqueKeys []spi.UniqueKey` to the `parsedItem` struct (`service.go:824`); in the validation loop (which already holds `desc` at `:838`) set `parsed[i].uniqueKeys = desc.UniqueKeys` and run the partial pre-check on `item.payloadBytes` (422 on partial). In the execution loop, before each item's `engine.Execute`, `currentCtx = spi.WithUniqueKeys(currentCtx, item.uniqueKeys)`. `UpdateEntityCollection`: `desc` is loaded inside the loop (`:1360`) — set `currentCtx = spi.WithUniqueKeys(currentCtx, desc.UniqueKeys)` right there + pre-check.
- [ ] **Step 4:** PASS. — [ ] **Step 5:** commit: `feat(entity): per-item unique-key context for batch writes`

---

## Phase 5 — PostgreSQL enforcement

### Task 5.1: `unique_claims` migration + UNIQUE index + RLS

**Files:** Create `plugins/postgres/migrations/000003_unique_claims.up.sql` / `.down.sql`; migration smoke test.

- [ ] **Step 1:** test asserting the table + named index exist after migration (testcontainer). FAIL.
- [ ] **Step 3 (up):**
```sql
CREATE TABLE IF NOT EXISTS unique_claims (
    tenant_id TEXT NOT NULL, model_name TEXT NOT NULL, model_version TEXT NOT NULL,
    key_id TEXT NOT NULL, signature TEXT NOT NULL, entity_id TEXT NOT NULL,
    PRIMARY KEY (tenant_id, entity_id, key_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS unique_claims_uq
    ON unique_claims (tenant_id, model_name, model_version, key_id, signature);
ALTER TABLE unique_claims ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_unique_claims ON unique_claims
    USING (tenant_id = current_setting('app.current_tenant', true));
```
down: `DROP TABLE IF EXISTS unique_claims;`
- [ ] **Step 4:** PASS. — [ ] **Step 5:** commit: `feat(postgres): unique_claims table + UNIQUE index + RLS`

### Task 5.2: Postgres claim maintenance + constraint-name classification

**Files:** Modify `plugins/postgres/entity_store.go` (Save/Delete/DeleteAll), `transaction_manager.go` (`classifyError`); Create `plugins/postgres/unique_claims.go`; Test `unique_claims_test.go`.

- [ ] **Step 1: failing tests** (testcontainer, one tenant/model): two saves same signature/distinct ids ⇒ second `spi.ErrUniqueViolation`; soft-delete first then re-save ⇒ ok (freed); `DeleteAll` then re-save ⇒ ok; update moves key frees old; a `23505` on the `entities` PK still NOT a unique-violation (only the claim constraint maps); non-scalar at a key path ⇒ `ErrPartialUniqueKey`.
- [ ] **Step 2:** FAIL. — [ ] **Step 3:** `classifyError`: extend to `case pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == "unique_claims_uq": return fmt.Errorf("%w: %w", spi.ErrUniqueViolation, err)` (keep the 40001/40P01 branch). `unique_claims.go`:
```go
func (s *entityStore) replaceClaims(ctx context.Context, e *spi.Entity) error {
	claims, err := spi.ComputeClaims(spi.UniqueKeysFromContext(ctx), e.Data)
	if err != nil { return err } // ErrPartialUniqueKey family bubbles up
	tid := string(s.tenantID)
	if _, err := s.q.Exec(ctx, `DELETE FROM unique_claims WHERE tenant_id=$1 AND entity_id=$2`, tid, e.Meta.ID); err != nil { return fmt.Errorf("clear claims: %w", err) }
	for _, c := range claims {
		if _, err := s.q.Exec(ctx, `INSERT INTO unique_claims (tenant_id,model_name,model_version,key_id,signature,entity_id) VALUES ($1,$2,$3,$4,$5,$6)`,
			tid, e.Meta.ModelRef.EntityName, e.Meta.ModelRef.ModelVersion, c.KeyID, c.Signature, e.Meta.ID); err != nil { return fmt.Errorf("insert claim: %w", err) } // already classified
	}
	return nil
}
func (s *entityStore) releaseClaims(ctx context.Context, entityID string) error {
	_, err := s.q.Exec(ctx, `DELETE FROM unique_claims WHERE tenant_id=$1 AND entity_id=$2`, string(s.tenantID), entityID); return err
}
```
Call `replaceClaims` in `Save` (after the version-row insert, before return); `releaseClaims` on soft-delete; in `DeleteAll`, `DELETE FROM unique_claims WHERE tenant_id=$1 AND model_name=$2 AND model_version=$3` in the same tx. (`s.q` is the classifying querier, so the index violation arrives pre-classified.)
- [ ] **Step 4:** PASS (`cd plugins/postgres && go test ./... -run UniqueClaims`). — [ ] **Step 5:** commit: `feat(postgres): enforce composite unique keys via unique_claims`

---

## Phase 6 — SQLite enforcement

### Task 6.1: Migration + claim maintenance in flush + UNIQUE detection

**Files:** Create `plugins/sqlite/migrations/000002_unique_claims.up.sql` / `.down.sql`; Modify `plugins/sqlite/entity_store.go`/`txmanager.go` (`flushToSQLite`), `errors.go`; Create `plugins/sqlite/unique_claims.go`; Test.

- [ ] **Step 1:** failing tests mirroring 5.2 against sqlite. FAIL.
- [ ] **Step 3:** migration: `unique_claims` table + `unique_claims_uq` UNIQUE index (STRICT). `errors.go`: add `classifyClaimError` mapping `sqlite3.CONSTRAINT_UNIQUE` → `spi.ErrUniqueViolation` (used ONLY by claim writes, so the generic `classifyError`'s retryable-conflict mapping for entities/PK is not consulted). `unique_claims.go`: `replaceClaims(keys, e)` computes `spi.ComputeClaims(keys, e.Data)` then delete+insert, wrapping the INSERT in `classifyClaimError`. **Per-item keys must be captured at Save (buffer) time, NOT read from the flush ctx** — `flushToSQLite` runs once at Commit with a single context (the last item's keys), but the buffer can hold entities from a mixed-model batch with different keys. So: add a per-tx side map on the `transactionManager` (`txUniqueKeys map[string]map[string][]spi.UniqueKey` = txID→entityID→keys, under `mu`); the **buffer-path `Save`** records `spi.UniqueKeysFromContext(ctx)` for its entity into that map (last-write-wins, matching `tx.Buffer`); `flushToSQLite` (`txmanager.go:300`) looks the keys up per buffered entity (`:311`) and calls `replaceClaims(keys, entity)`, per tombstone (`:379`) `releaseClaims`; clean up the tx's key entry at the end of commit/rollback. The **non-tx `saveDirectly`** has no buffer (single entity) — it reads keys from ctx directly and maintains claims in its own sql tx.
- [ ] **Step 4:** PASS (`cd plugins/sqlite && go test ./... -run UniqueClaims`). — [ ] **Step 5:** commit: `feat(sqlite): enforce composite unique keys via unique_claims`

---

## Phase 7 — Memory enforcement

### Task 7.1: Signature map under `entityMu` at commit; release on both flush loops + non-tx

**Files:** Modify `plugins/memory/store_factory.go` (claim map), `txmanager.go` (commit flush: Buffer + Deletes loops), `entity_store.go` (non-tx Save/Delete/DeleteAll); Test `unique_claims_test.go`.

- [ ] **Step 1: failing tests** — scenarios from 5.2 against memory PLUS the concurrency winner/loser (two goroutines, same signature, distinct ids → exactly one `ErrUniqueViolation`) and an intra-batch duplicate within one `CreateEntityCollection`. Memory-plugin suite only (NOT parity).
- [ ] **Step 2:** FAIL. — [ ] **Step 3:** factory (guarded by `entityMu`): `uniqueClaims map[claimKey]string` (claimKey = `{tenant,model,version,keyID,signature}` → entityID) + `claimsByEntity map[string][]claimKey`. **Per-item keys captured at Save, NOT from the commit ctx** (same reason as sqlite — the commit flush sees one ctx but the buffer can hold a mixed-model batch): the buffer-path `Save` records `spi.UniqueKeysFromContext(ctx)` into a per-tx side map (txID→entityID→keys) under `entityMu`/the tx lock; the flush reads from it. In `txmanager.Commit`, inside the `entityMu.Lock()` critical section, **deterministic order** (sort buffered entity ids): for each, `claims, err := spi.ComputeClaims(capturedKeysFor(entityID), entity.Data)` (err → unwind locks exactly as the FCW `ErrConflict` branch does, return err); validate against `uniqueClaims` ignoring snapshot (holder != this entity ⇒ `ErrUniqueViolation`; intra-batch `pending` map ⇒ `ErrUniqueViolation`); then release each written entity's prior claims + each tombstoned id's claims (**the Deletes loop too**, #10) and insert the new set, maintaining `claimsByEntity`. Use the **IIFE mutex discipline** (`.claude/rules/go-mutex-discipline.md`) — no bare unlock. Non-tx paths do the same under their existing `entityMu.Lock()`.
- [ ] **Step 4:** PASS (`cd plugins/memory && go test ./... -run UniqueClaims`; `-race` on the concurrency test). — [ ] **Step 5:** commit: `feat(memory): enforce composite unique keys in the commit critical section`

### Task 7.2: Type-widening guard (schema-extend rejects widening a keyed field)

**Files:** Modify the schema-extend/apply path (where `ExtendSchema`/`Merge` runs — `internal/domain/entity/handler.go` validate-or-extend, or `schema` apply); Test.

- [ ] **Step 1: failing test** — a model with a unique key on a scalar field; a schema-extending write that would widen that field to an object/array ⇒ rejected (4xx). FAIL.
- [ ] **Step 3:** at the merge/extend point, after computing the would-be merged node, reject if any `desc.UniqueKeys` field path is no longer a scalar leaf (reuse `schema.ValidateUniqueKeys` against the merged node) → a 422. (Belt: `ComputeClaims` already rejects a non-scalar at runtime, Task 0.5.)
- [ ] **Step 4:** PASS. — [ ] **Step 5:** commit: `feat(schema): reject widening a unique-key field post-declaration`

---

## Phase 8 — Cross-stack coverage

### Task 8.1: HTTP e2e (postgres) — every status code

**Files:** Create `internal/e2e/unique_keys_test.go`. Cover (spec §6/§7): declare (200); declare-on-locked (409); bad field (422 `INVALID_UNIQUE_KEY_DEFINITION`); create duplicate (409 `UNIQUE_VIOLATION`); partial key (422 `INVALID_UNIQUE_KEY`); over-bound numeric (422); update-moves-key (409 / 200); PATCH-nulls-key (422); soft-delete frees value; `DeleteAll` frees values; `CreateEntityCollection` intra-batch dup (409) + mixed-model batch; multiple keys; schema-extend-after-lock doesn't drop keys; processor rewrites a key field ⇒ final value enforced; **CBD-segmenting violation (plain + If-Match) ⇒ 409 not 400/raw-text**; **processor emits over-bound value ⇒ 422 via classifyWorkflowError**; ASYNC_NEW_TX writes dup ⇒ no duplicate persisted; export includes keys / re-import preserves. RED → GREEN → commit.

### Task 8.2: gRPC coverage
**Files:** `internal/grpc/entity_test.go`, `model_test.go` — assert the envelope (`Success=false`, `Error.Code`) for `UNIQUE_VIOLATION`, `INVALID_UNIQUE_KEY`, `COMPOSITE_KEY_UNSUPPORTED`, `INVALID_UNIQUE_KEY_DEFINITION`. RED → GREEN → commit.

### Task 8.3: Cross-backend parity
**Files:** `e2e/parity/unique_keys.go` + register in `registry.go`. Scenarios: create-dup→409, soft-delete-frees, partial→422, all-null-exempt, DeleteAll-frees, multiple-keys, mixed-model-batch. Capability-gate positive scenarios (skip on unsupported). `COMPOSITE_KEY_UNSUPPORTED` is **NOT** parity-testable in-repo (all three support it) → cover via a **unit test with a fake `StoreFactory`** that doesn't implement `CompositeUniqueKeyCapable` (S4); commercial backend asserts it on its next dep update. RED → GREEN → commit.

### Task 8.4: Concurrency (isolated, single-backend)
**Files:** `internal/e2e/unique_keys_concurrency_test.go` — two concurrent creates same value-set on postgres → exactly one 201, other 409, no torn write; assert exactly one live holder. NOT in parity. RED → GREEN → commit.

---

## Phase 9 — Docs + coordinated SPI release

### Task 9.1: Gate-4 documentation
**Files:** `README.md` (composite keys — endpoint, semantics, byte-exact); operator docs (**document the §3.8 multi-node-teardown staleness limitation** + point to `#353`); `CHANGELOG.md`; `docs/workflow-schema-versioning.md` (export-DTO `UniqueKeys` addition); help topics for the new endpoint + error codes. Run `go test ./cmd/cyoda/help/...` green. Commit.

### Task 9.2: Coordinated SPI release + pin bump (FINAL)
> Only after ALL prior phases are green locally via `go.work`.
- [ ] In `../cyoda-go-spi`: ensure Phase 0 commits on `main`, push, tag a fresh version (per `MAINTAINING.md`; never force-move a tag).
- [ ] Bump the `cyoda-go-spi` require pin to the new tag in root + all three plugin `go.mod`; `go mod tidy` each.
- [ ] Update `COMPATIBILITY.md`.
- [ ] `make test-all` + `make race` + `go test ./internal/e2e/...` green.
- [ ] One commit bumping all four pins + COMPATIBILITY: `chore: pin cyoda-go-spi <tag> for composite unique keys`

---

## Self-Review

- **Spec coverage:** §2 semantics (0.5/1.1/4/8), §3.1 store-computes/context-thread (0.2/0.4/0.5/4/5-7), §3.2 canonicalization (0.5), §3.3 claim storage (5/6/7), §3.4 update-moves-key (5.2/6/7), §3.5 errors incl. C2/F3 (3.1/5.2/6.1), §3.6 capability (0.3/2.1/2.2), §3.7 collection (4.2/5/6/7/8), §3.8 staleness (documented; 9.1 operator doc; #353), §4 declaration (1.1/2.2/2.3), §5 dataflow (4/5-7), §6 error table (3.1/8), §7 matrix (Phase 8), §8 cross-cutting (1.2/9). ✅ Type-widening §4 (7.2). ✅
- **Type consistency:** `spi.UniqueKey`(=`schema.UniqueKey` alias), `spi.UniqueClaim`, `spi.ComputeClaims`, `spi.ErrUniqueViolation`/`ErrPartialUniqueKey` (umbrella), `spi.WithUniqueKeys`/`UniqueKeysFromContext`, `spi.ModelDescriptor.UniqueKeys`, `unique_claims_uq`, `ValidateUniqueKeys`/`UniqueKeyDefError`, `ErrCode*` — used identically. **No `spi.Entity` field, no rev token.** ✅
- **No silent-skip:** keys flow via context set at the handler (single + per-item batch); engine CBD saves inherit; non-tx `store.Save` (the only context-less path) is asserted-not-a-user-write by a guard test (Task 4.x). ✅
- **STOP-don't-assume:** any task hitting an unspecified decision surfaces it, never assumes/descopes (per the header).
