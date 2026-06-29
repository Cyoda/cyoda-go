# Composite Unique Keys Implementation Plan

> **⚠️ SUPERSEDED — DO NOT EXECUTE YET.** This plan reflects the *second* design draft
> (transient `spi.Entity.UniqueKeys` field; keys stored inside `Schema` bytes). The design
> has since been revised (commit `1954905`): keys live on `spi.ModelDescriptor.UniqueKeys`
> (fold-proof) and the entity store reads them from the model descriptor on `Save`; plus
> C2 error-routing and S1–S5 fixes. The spec
> (`docs/superpowers/specs/2026-06-28-composite-unique-keys-design.md`) is authoritative.
> This plan will be regenerated to match once the design passes its (in-progress) third
> independent review. Key deltas to expect: Task 0.2 swaps the SPI field
> (Entity→ModelDescriptor); Task 1.1's codec wrapper is dropped (keys no longer in schema
> bytes); a new per-engine model-store-persistence phase is added; Tasks 5–7 read keys via
> a cached model lookup instead of a transient field; Phase 3 gains the
> `classifyWorkflowError` C2 wiring.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an entity model declare composite UNIQUE keys over scalar fields, enforced on create/update across the memory, sqlite, and postgres engines.

**Architecture:** The model's unique-key DEFINITIONS ride on an additive transient `spi.Entity.UniqueKeys` field (the service attaches them on write paths). The **store** computes claims `{keyId, signature}` from `(UniqueKeys + live entity.Data)` via the shared SPI helper `spi.ComputeClaims` inside `Save`, then enforces engine-internally (postgres/sqlite native `UNIQUE` index on a `unique_claims` side table; memory map under the commit critical section). Computing in the store — the one chokepoint every save funnels through, including the engine's internal post-processor saves — means a processor that mutates a key field is enforced on its final value. A backend advertises support via an additive optional SPI interface; declaring keys on an unsupported backend is rejected at declaration time.

**Tech Stack:** Go 1.26, `cyoda-go-spi` (external module, composed locally via `go.work`), pgx/v5 (postgres), modernc/mattn sqlite, testcontainers-go (e2e), `math/big` (canonicalization).

**Design spec:** `docs/superpowers/specs/2026-06-28-composite-unique-keys-design.md` — read it first.

## Global Constraints

- Go 1.26.4 across root + all `plugins/*` modules (each own `go.mod`).
- **SPI is an external module** (`github.com/cyoda-platform/cyoda-go-spi`). During development it is composed locally via `go.work` (`skip-worktree`, **never** a committed `replace`). The SPI changes are tagged FIRST; the cyoda-go pin bump is the FINAL commit (Phase 9). See `MAINTAINING.md` "Coordinated release across sibling repos".
- Use `log/slog` only. Wrap errors `fmt.Errorf("...: %w", err)`. `uuid.UUID` not `string`.
- 4xx: full domain detail + error code; 5xx: generic + ticket. New error codes are **non-retryable**.
- Every new `ErrCode*` in `internal/common/error_codes.go` needs a `cmd/cyoda/help/content/errors/<CODE>.md` topic (`TestErrCode_Parity`, `cmd/cyoda/help/help_test.go:532`).
- Uniqueness scope: per `(tenant, model name, model version)`, **live entities only**; all-or-nothing null rule; **byte-exact** string comparison; precision-preserving numeric canonicalization (never `float64`).
- Commercial (Cassandra) backend is out of scope — it must NOT implement the capability interface (tracked in its own repo).
- TDD: every task is RED → GREEN → commit. Run `go vet ./...` and `go build ./...` green before each commit. E2E/parity require Docker.

---

## Phase 0 — SPI additive changes (composed via go.work)

> All Phase 0 edits are in the **cyoda-go-spi** repo, composed into this build via `go.work`. They ship in the SPI tag created in Phase 9; the cyoda-go pin bump is the final commit. Until then `go build ./...` resolves them through `go.work`.

### Task 0.1: Locate/clone cyoda-go-spi and wire go.work

**Files:**
- Modify: `go.work` (repo root — local only, do NOT commit changes that point at a sibling checkout)

- [ ] **Step 1: Locate or clone the SPI checkout**

```bash
# Prefer an existing sibling checkout; else clone next to the repo.
ls ../cyoda-go-spi/go.mod 2>/dev/null || \
  git clone https://github.com/Cyoda-platform/cyoda-go-spi.git ../cyoda-go-spi
( cd ../cyoda-go-spi && git checkout main && git pull --ff-only )
```

- [ ] **Step 2: Add the local SPI to go.work and protect it from commits**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-composite-unique-keys
go work edit -use ../../../../cyoda-go-spi   # adjust relative path to the actual checkout
git update-index --skip-worktree go.work     # keep the local use-directive out of commits
go build ./...                               # Expected: builds against local SPI
```

Expected: build succeeds; `git status` does NOT show `go.work` as modified.

- [ ] **Step 3: Commit** — nothing to commit here (go.work change is skip-worktree). Proceed.

---

### Task 0.2: SPI — `UniqueKey`/`UniqueClaim` types + `Entity.UniqueKeys` transient field

**Files (in ../cyoda-go-spi):**
- Modify: `types.go` (the `Entity` struct)
- Create: `unique.go`
- Test: `unique_test.go`

**Interfaces:**
- Produces: `spi.UniqueKey{ ID string; Fields []string }`; `spi.UniqueClaim{ KeyID string; Signature string }`; `Entity.UniqueKeys []UniqueKey` (transient — model-level key DEFINITIONS the service attaches; the store computes claims from them + live `Data`; never serialized).

- [ ] **Step 1: Write the failing test** (`../cyoda-go-spi/unique_test.go`)

```go
package spi

import "testing"

func TestEntityUniqueKeysFieldExists(t *testing.T) {
	e := Entity{UniqueKeys: []UniqueKey{{ID: "byEmail", Fields: []string{"$.email"}}}}
	if e.UniqueKeys[0].ID != "byEmail" || e.UniqueKeys[0].Fields[0] != "$.email" {
		t.Fatalf("unique keys not carried: %+v", e.UniqueKeys)
	}
	_ = UniqueClaim{KeyID: "byEmail", Signature: "s5:Alice"}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `cd ../cyoda-go-spi && go test ./... -run TestEntityUniqueKeysFieldExists`
Expected: FAIL — `UniqueKey`/`UniqueClaim` undefined, `UniqueKeys` not a field.

- [ ] **Step 3: Add the types and field**

`../cyoda-go-spi/unique.go`:
```go
package spi

// UniqueKey is a model-level composite unique key over scalar leaf fields.
// Fields are ordered JSONPath leaves (e.g. "$.email", "$.region"). The cyoda-go
// service attaches the active model's keys to Entity.UniqueKeys on write paths;
// the store computes claims from them and the live Data.
type UniqueKey struct {
	ID     string
	Fields []string
}

// UniqueClaim is a computed composite-unique-key assertion: the store must
// guarantee no OTHER live entity in the same (tenant, model name, model
// version) holds the same (KeyID, Signature). Signature is an opaque,
// type-tagged canonical encoding produced by ComputeClaims — compared
// byte-for-byte, never interpreted.
type UniqueClaim struct {
	KeyID     string
	Signature string
}
```

In `types.go`, add the field to `Entity` (keep `Meta`, `Data`):
```go
type Entity struct {
	Meta EntityMeta
	Data []byte
	// UniqueKeys are the active model's composite-unique-key DEFINITIONS,
	// attached transiently by the service on write paths so the store can
	// compute claims from the live Data. NOT part of the durable doc and
	// MUST NOT be serialized into storage. Empty on read.
	UniqueKeys []UniqueKey
}
```

- [ ] **Step 4: Run it to verify pass** — Expected: PASS.

- [ ] **Step 5: Commit (in the SPI repo)**

```bash
cd ../cyoda-go-spi
git add types.go unique.go unique_test.go
git commit -m "feat: add UniqueKey/UniqueClaim types and transient Entity.UniqueKeys field"
```

---

### Task 0.3: SPI — `ErrUniqueViolation` + `ErrPartialUniqueKey` sentinels + `CompositeUniqueKeyCapable` interface

**Files (in ../cyoda-go-spi):**
- Modify: `errors.go`
- Modify: `unique.go`
- Test: `unique_test.go`

**Interfaces:**
- Produces: `spi.ErrUniqueViolation` and `spi.ErrPartialUniqueKey` (sentinels, both distinct from `ErrConflict`); `spi.CompositeUniqueKeyCapable interface { SupportsCompositeUniqueKeys() bool }`.

> When implementing Step 3 below, also add, in `errors.go`:
> ```go
> // ErrPartialUniqueKey indicates a composite unique key had SOME but not all
> // fields present (all-or-nothing rule). NON-retryable client error → 422.
> var ErrPartialUniqueKey = errors.New("composite unique key partially populated")
> ```
> and assert in the test that it is distinct from `ErrConflict` and `ErrUniqueViolation`.

- [ ] **Step 1: Write the failing test** (append to `../cyoda-go-spi/unique_test.go`)

```go
import "errors"

func TestErrUniqueViolationDistinctFromConflict(t *testing.T) {
	if errors.Is(ErrUniqueViolation, ErrConflict) {
		t.Fatal("ErrUniqueViolation must NOT wrap/equal ErrConflict (different retry semantics)")
	}
}

type fakeCapable struct{}

func (fakeCapable) SupportsCompositeUniqueKeys() bool { return true }

func TestCompositeUniqueKeyCapable(t *testing.T) {
	var v any = fakeCapable{}
	c, ok := v.(CompositeUniqueKeyCapable)
	if !ok || !c.SupportsCompositeUniqueKeys() {
		t.Fatal("CompositeUniqueKeyCapable not satisfied")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd ../cyoda-go-spi && go test ./... -run 'TestErrUniqueViolation|TestCompositeUniqueKeyCapable'`
Expected: FAIL — undefined `ErrUniqueViolation`, `CompositeUniqueKeyCapable`.

- [ ] **Step 3: Implement**

In `errors.go` add (standalone — NOT wrapping `ErrConflict`):
```go
// ErrUniqueViolation indicates a write would violate a declared composite
// unique key: another live entity in the same (tenant, model, version)
// already holds the same value-set. Deterministic and NON-retryable —
// distinct from ErrConflict (a retryable serialization abort).
var ErrUniqueViolation = errors.New("composite unique key violation")
```

In `unique.go` add:
```go
// CompositeUniqueKeyCapable is an OPTIONAL interface a StoreFactory may
// implement to advertise composite-unique-key enforcement. Absence (or a
// false return) means unsupported: the service rejects key declarations on
// that backend. Additive — it is NOT part of the StoreFactory interface.
type CompositeUniqueKeyCapable interface {
	SupportsCompositeUniqueKeys() bool
}
```

- [ ] **Step 4: Run to verify pass** — Expected: PASS.

- [ ] **Step 5: Commit (SPI repo)**

```bash
cd ../cyoda-go-spi && git add errors.go unique.go unique_test.go
git commit -m "feat: add ErrUniqueViolation sentinel and CompositeUniqueKeyCapable interface"
```

---

## Phase 1 — Codec, validation (cyoda-go), + SPI signature helper

> `schema.UniqueKey` is a **type alias** for `spi.UniqueKey` (one type end-to-end: the codec
> stores it, the validator checks it, `spi.ComputeClaims` consumes it directly — no
> conversion). Task 1.3 lives in the SPI repo (see its note).

### Task 1.1: `UniqueKey` alias + codec wrapper with back-compat read

**Files:**
- Create: `internal/domain/model/schema/uniquekey.go`
- Modify: `internal/domain/model/schema/codec.go` (`Marshal`/`Unmarshal`)
- Test: `internal/domain/model/schema/uniquekey_test.go`, `internal/domain/model/schema/codec_test.go`

**Interfaces:**
- Produces: `schema.UniqueKey{ ID string; Fields []string }`; `schema.MarshalModel(n *ModelNode, keys []UniqueKey) ([]byte, error)`; `schema.UnmarshalModel(data []byte) (*ModelNode, []UniqueKey, error)`. Existing `Marshal`/`Unmarshal` remain (node-only) and are kept for callers that don't touch keys.

- [ ] **Step 1: Write the failing test** (`uniquekey_test.go`)

```go
package schema

import "testing"

func TestMarshalModel_RoundTripsKeys(t *testing.T) {
	root := NewObjectNode()
	root.children["email"] = NewLeafNode(String)
	keys := []UniqueKey{{ID: "byEmail", Fields: []string{"$.email"}}}

	b, err := MarshalModel(root, keys)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, gotKeys, err := UnmarshalModel(b)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(gotKeys) != 1 || gotKeys[0].ID != "byEmail" || gotKeys[0].Fields[0] != "$.email" {
		t.Fatalf("keys not round-tripped: %+v", gotKeys)
	}
}

func TestUnmarshalModel_BareNodeBackCompat(t *testing.T) {
	// Old payloads are a bare wireNode (no wrapper). Must read as zero keys.
	root := NewObjectNode()
	root.children["x"] = NewLeafNode(Integer)
	bare, err := Marshal(root) // existing bare-node marshal
	if err != nil {
		t.Fatalf("marshal bare: %v", err)
	}
	n, keys, err := UnmarshalModel(bare)
	if err != nil {
		t.Fatalf("unmarshal bare: %v", err)
	}
	if n == nil || len(keys) != 0 {
		t.Fatalf("bare-node back-compat broken: keys=%+v", keys)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/domain/model/schema/ -run 'TestMarshalModel|TestUnmarshalModel_BareNode'`
Expected: FAIL — undefined `UniqueKey`, `MarshalModel`, `UnmarshalModel`.

- [ ] **Step 3: Implement**

`uniquekey.go`:
```go
package schema

import spi "github.com/cyoda-platform/cyoda-go-spi"

// UniqueKey is a model-level composite unique key over scalar leaf fields.
// Alias of spi.UniqueKey so the codec, validator, and spi.ComputeClaims all
// share ONE type with no conversion. Fields are ordered JSONPath leaves.
type UniqueKey = spi.UniqueKey
```

> The `modelEnvelope.UniqueKeys` field then serializes `[]spi.UniqueKey`. `spi.UniqueKey`
> has no JSON tags, so the envelope marshals fields as `ID`/`Fields`; that's fine for the
> internal `Schema` blob. The *wire DTO* for the declaration endpoint (Task 2.3) maps the
> lowercase `id`/`fields` JSON to `spi.UniqueKey` explicitly.

In `codec.go`, add a wrapper envelope and the two new functions (keep `Marshal`/`Unmarshal`):
```go
// modelEnvelope is the wire wrapper carrying the schema tree plus model-level
// metadata. Legacy payloads are a bare wireNode (no envelope); UnmarshalModel
// detects this and returns zero keys.
type modelEnvelope struct {
	Root       *wireNode   `json:"root"`
	UniqueKeys []UniqueKey `json:"uniqueKeys,omitempty"`
}

// MarshalModel serializes a node tree plus unique keys as a wrapped envelope.
func MarshalModel(n *ModelNode, keys []UniqueKey) ([]byte, error) {
	wn, err := toWire(n) // existing helper used by Marshal; reuse it
	if err != nil {
		return nil, err
	}
	return json.Marshal(modelEnvelope{Root: wn, UniqueKeys: keys})
}

// UnmarshalModel parses either a wrapped envelope or a legacy bare wireNode.
func UnmarshalModel(data []byte) (*ModelNode, []UniqueKey, error) {
	var env modelEnvelope
	if err := json.Unmarshal(data, &env); err == nil && env.Root != nil {
		n, err := fromWire(env.Root) // existing helper used by Unmarshal
		if err != nil {
			return nil, nil, err
		}
		return n, env.UniqueKeys, nil
	}
	// Legacy bare node.
	n, err := Unmarshal(data)
	if err != nil {
		return nil, nil, err
	}
	return n, nil, nil
}
```

> Note: confirm the exact names of the existing wire↔node helpers in `codec.go` (`Marshal` calls them — likely `toWire`/`fromWire`); reuse those rather than re-implementing.

- [ ] **Step 4: Run to verify pass** — Expected: PASS. Also run the full schema package: `go test ./internal/domain/model/schema/` (Expected: PASS — back-compat preserved).

- [ ] **Step 5: Commit**

```bash
git add internal/domain/model/schema/uniquekey.go internal/domain/model/schema/codec.go internal/domain/model/schema/uniquekey_test.go
git commit -m "feat(schema): UniqueKey type + back-compat codec envelope"
```

---

### Task 1.2: Unique-key definition validation

**Files:**
- Create: `internal/domain/model/schema/uniquekey_validate.go`
- Test: `internal/domain/model/schema/uniquekey_validate_test.go`

**Interfaces:**
- Consumes: `schema.UniqueKey`, `ModelNode.Fields() []FieldDescriptor`, `FieldDescriptor{Path, Types, IsArray}`.
- Produces: `schema.ValidateUniqueKeys(n *ModelNode, keys []UniqueKey) error` — returns a typed error `*UniqueKeyDefError` (has `.Reason` string) on the first problem.

- [ ] **Step 1: Write the failing test**

```go
package schema

import (
	"errors"
	"testing"
)

func objWithScalars() *ModelNode {
	root := NewObjectNode()
	root.children["email"] = NewLeafNode(String)
	root.children["region"] = NewLeafNode(String)
	arr := &ModelNode{kind: KindArray, types: NewTypeSet(), element: NewLeafNode(String)}
	root.children["tags"] = arr
	return root
}

func TestValidateUniqueKeys_OK(t *testing.T) {
	err := ValidateUniqueKeys(objWithScalars(), []UniqueKey{{ID: "k", Fields: []string{"$.email", "$.region"}}})
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestValidateUniqueKeys_UnknownPath(t *testing.T) {
	err := ValidateUniqueKeys(objWithScalars(), []UniqueKey{{ID: "k", Fields: []string{"$.nope"}}})
	var de *UniqueKeyDefError
	if !errors.As(err, &de) {
		t.Fatalf("expected UniqueKeyDefError, got %v", err)
	}
}

func TestValidateUniqueKeys_ArrayPathRejected(t *testing.T) {
	if err := ValidateUniqueKeys(objWithScalars(), []UniqueKey{{ID: "k", Fields: []string{"$.tags"}}}); err == nil {
		t.Fatal("array path must be rejected")
	}
}

func TestValidateUniqueKeys_EmptyFields(t *testing.T) {
	if err := ValidateUniqueKeys(objWithScalars(), []UniqueKey{{ID: "k", Fields: nil}}); err == nil {
		t.Fatal("empty fields must be rejected")
	}
}

func TestValidateUniqueKeys_DupID(t *testing.T) {
	keys := []UniqueKey{{ID: "k", Fields: []string{"$.email"}}, {ID: "k", Fields: []string{"$.region"}}}
	if err := ValidateUniqueKeys(objWithScalars(), keys); err == nil {
		t.Fatal("duplicate key id must be rejected")
	}
}

func TestValidateUniqueKeys_DupFieldWithinKey(t *testing.T) {
	keys := []UniqueKey{{ID: "k", Fields: []string{"$.email", "$.email"}}}
	if err := ValidateUniqueKeys(objWithScalars(), keys); err == nil {
		t.Fatal("duplicate field within a key must be rejected")
	}
}
```

- [ ] **Step 2: Run to verify failure** — Expected: FAIL (undefined `ValidateUniqueKeys`, `UniqueKeyDefError`).

- [ ] **Step 3: Implement** (`uniquekey_validate.go`)

```go
package schema

import "fmt"

// UniqueKeyDefError is a definition-time validation failure for a unique key.
type UniqueKeyDefError struct{ Reason string }

func (e *UniqueKeyDefError) Error() string { return e.Reason }

// scalarLeafPaths returns the set of field paths that are non-array scalar leaves.
func scalarLeafPaths(n *ModelNode) map[string]bool {
	out := map[string]bool{}
	for _, f := range n.Fields() {
		if f.IsArray {
			continue
		}
		// A leaf has at least one concrete scalar DataType and no children.
		out[f.Path] = true
	}
	return out
}

// ValidateUniqueKeys checks that every key references known scalar leaves,
// has non-empty distinct fields, and that key ids are unique.
func ValidateUniqueKeys(n *ModelNode, keys []UniqueKey) error {
	leaves := scalarLeafPaths(n)
	seenID := map[string]bool{}
	for _, k := range keys {
		if k.ID == "" {
			return &UniqueKeyDefError{Reason: "unique key id must be non-empty"}
		}
		if seenID[k.ID] {
			return &UniqueKeyDefError{Reason: fmt.Sprintf("duplicate unique key id %q", k.ID)}
		}
		seenID[k.ID] = true
		if len(k.Fields) == 0 {
			return &UniqueKeyDefError{Reason: fmt.Sprintf("unique key %q has no fields", k.ID)}
		}
		seenField := map[string]bool{}
		for _, p := range k.Fields {
			if seenField[p] {
				return &UniqueKeyDefError{Reason: fmt.Sprintf("unique key %q repeats field %q", k.ID, p)}
			}
			seenField[p] = true
			if !leaves[p] {
				return &UniqueKeyDefError{Reason: fmt.Sprintf("unique key %q field %q is not a known scalar leaf", k.ID, p)}
			}
		}
	}
	return nil
}
```

> Note: verify `FieldDescriptor` semantics for "scalar leaf" — `Fields()` already enumerates leaves; if object/array intermediate nodes are excluded from `Fields()`, the `IsArray` check plus membership is sufficient. Add a guard for array-element leaf paths (paths containing `[*]`) if `Fields()` emits them: reject any path containing `[` or `*`.

- [ ] **Step 4: Run to verify pass** — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/model/schema/uniquekey_validate.go internal/domain/model/schema/uniquekey_validate_test.go
git commit -m "feat(schema): validate unique-key definitions against scalar leaves"
```

---

### Task 1.3: SPI signature helper `ComputeClaims` (canonicalization + all-or-nothing)

> **This task is in the `../cyoda-go-spi` repo** (composed via go.work). The helper lives in
> the SPI so all three engine submodules — which compute claims from the live doc inside
> `Save` — call ONE canonicalization path. Add `github.com/tidwall/gjson` to the SPI
> `go.mod` (`cd ../cyoda-go-spi && go get github.com/tidwall/gjson@v1.19.0`). Reuses the
> `ErrPartialUniqueKey` sentinel from Task 0.3.

**Files (in ../cyoda-go-spi):**
- Create: `unique_signature.go`
- Modify: `go.mod` (add gjson)
- Test: `unique_signature_test.go`

**Interfaces:**
- Consumes: `UniqueKey`, `UniqueClaim`, `ErrPartialUniqueKey` (all `spi` package).
- Produces: `spi.ComputeClaims(keys []UniqueKey, doc []byte) ([]UniqueClaim, error)`. Returns `ErrPartialUniqueKey` when a key is partially filled; emits a claim only for fully-present keys; all-null keys emit none.

- [ ] **Step 1: Write the failing test** (`../cyoda-go-spi/unique_signature_test.go`)

```go
package spi

import (
	"errors"
	"testing"
)

func keys() []UniqueKey {
	return []UniqueKey{{ID: "k", Fields: []string{"$.email", "$.age"}}}
}

func TestComputeClaims_FullyPresent(t *testing.T) {
	claims, err := ComputeClaims(keys(), []byte(`{"email":"a@x.com","age":42}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(claims) != 1 || claims[0].KeyID != "k" {
		t.Fatalf("expected 1 claim, got %+v", claims)
	}
}

func TestComputeClaims_NumericCanonical(t *testing.T) {
	a, _ := ComputeClaims(keys(), []byte(`{"email":"a@x.com","age":42}`))
	b, _ := ComputeClaims(keys(), []byte(`{"email":"a@x.com","age":42.0}`))
	c, _ := ComputeClaims(keys(), []byte(`{"email":"a@x.com","age":4.2e1}`))
	if a[0].Signature != b[0].Signature || b[0].Signature != c[0].Signature {
		t.Fatalf("42 / 42.0 / 4.2e1 must collide: %q %q %q", a[0].Signature, b[0].Signature, c[0].Signature)
	}
}

func TestComputeClaims_LargeIntPrecision(t *testing.T) {
	a, _ := ComputeClaims(keys(), []byte(`{"email":"a@x.com","age":9007199254740993}`))
	b, _ := ComputeClaims(keys(), []byte(`{"email":"a@x.com","age":9007199254740992}`))
	if a[0].Signature == b[0].Signature {
		t.Fatal("ints above 2^53 must stay distinct (no float64 rounding)")
	}
}

func TestComputeClaims_TypeTagged(t *testing.T) {
	a, _ := ComputeClaims([]UniqueKey{{ID: "k", Fields: []string{"$.v"}}}, []byte(`{"v":"1"}`))
	b, _ := ComputeClaims([]UniqueKey{{ID: "k", Fields: []string{"$.v"}}}, []byte(`{"v":1}`))
	if a[0].Signature == b[0].Signature {
		t.Fatal(`string "1" and number 1 must not collide`)
	}
}

func TestComputeClaims_ByteExactStrings(t *testing.T) {
	a, _ := ComputeClaims([]UniqueKey{{ID: "k", Fields: []string{"$.v"}}}, []byte(`{"v":"Alice"}`))
	b, _ := ComputeClaims([]UniqueKey{{ID: "k", Fields: []string{"$.v"}}}, []byte(`{"v":"alice"}`))
	if a[0].Signature == b[0].Signature {
		t.Fatal("strings are case-sensitive / byte-exact")
	}
}

func TestComputeClaims_AllNullExempt(t *testing.T) {
	claims, err := ComputeClaims(keys(), []byte(`{"other":1}`))
	if err != nil || len(claims) != 0 {
		t.Fatalf("all-absent key must be exempt (0 claims, no err); got %+v, %v", claims, err)
	}
	claims, err = ComputeClaims(keys(), []byte(`{"email":null,"age":null}`))
	if err != nil || len(claims) != 0 {
		t.Fatalf("all-null key must be exempt; got %+v, %v", claims, err)
	}
}

func TestComputeClaims_PartialRejected(t *testing.T) {
	_, err := ComputeClaims(keys(), []byte(`{"email":"a@x.com"}`)) // age missing
	if !errors.Is(err, ErrPartialUniqueKey) {
		t.Fatalf("partial key must return ErrPartialUniqueKey, got %v", err)
	}
}

func TestComputeClaims_NegativeZero(t *testing.T) {
	a, _ := ComputeClaims([]UniqueKey{{ID: "k", Fields: []string{"$.v"}}}, []byte(`{"v":-0}`))
	b, _ := ComputeClaims([]UniqueKey{{ID: "k", Fields: []string{"$.v"}}}, []byte(`{"v":0}`))
	if a[0].Signature != b[0].Signature {
		t.Fatal("-0 and 0 must collide")
	}
}
```

- [ ] **Step 2: Run to verify failure** — Run `cd ../cyoda-go-spi && go test ./... -run TestComputeClaims`. Expected: FAIL (undefined `ComputeClaims`).

- [ ] **Step 3: Implement** (`../cyoda-go-spi/unique_signature.go`)

```go
package spi

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/tidwall/gjson"
)

// ComputeClaims returns one claim per fully-present key. Keys with all fields
// absent/null are exempt (no claim). A partially-filled key returns
// ErrPartialUniqueKey. The signature is a type-tagged, precision-preserving
// canonical encoding so equal values collide byte-for-byte across every engine.
func ComputeClaims(keys []UniqueKey, doc []byte) ([]UniqueClaim, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	parsed := gjson.ParseBytes(doc)
	out := make([]UniqueClaim, 0, len(keys))
	for _, k := range keys {
		var parts []string
		present, anyPresent := 0, false
		for _, path := range k.Fields {
			// gjson path: strip leading "$." → dotted path.
			r := parsed.Get(strings.TrimPrefix(path, "$."))
			if !r.Exists() || r.Type == gjson.Null {
				parts = append(parts, "") // placeholder; key is incomplete unless all present
				continue
			}
			anyPresent = true
			present++
			tok, err := canonToken(r)
			if err != nil {
				return nil, fmt.Errorf("unique key %q field %q: %w", k.ID, path, err)
			}
			parts = append(parts, tok)
		}
		switch {
		case present == 0 && !anyPresent:
			continue // all-null/absent → exempt
		case present != len(k.Fields):
			return nil, fmt.Errorf("%w: key %q", ErrPartialUniqueKey, k.ID)
		}
		out = append(out, UniqueClaim{KeyID: k.ID, Signature: strings.Join(parts, "\x1f")})
	}
	return out, nil
}

// canonToken renders a single scalar as a type-tagged canonical token.
func canonToken(r gjson.Result) (string, error) {
	switch r.Type {
	case gjson.String:
		// Byte-exact, length-prefixed to avoid delimiter ambiguity.
		return fmt.Sprintf("s%d:%s", len(r.Str), r.Str), nil
	case gjson.True:
		return "b:true", nil
	case gjson.False:
		return "b:false", nil
	case gjson.Number:
		// Precision-preserving: parse the RAW literal as big.Rat (never float64),
		// normalize to a canonical rational string so 42 / 42.0 / 4.2e1 / -0 collide
		// and large ints keep full precision.
		rat, ok := new(big.Rat).SetString(r.Raw)
		if !ok {
			return "", fmt.Errorf("non-canonicalizable number %q", r.Raw)
		}
		return "n:" + rat.RatString(), nil // RatString => "a/b" lowest terms; integers => "a"
	default:
		return "", fmt.Errorf("unsupported scalar type %v", r.Type)
	}
}
```

> Note: `gjson.Result.Raw` holds the literal number text (e.g. `4.2e1`); `big.Rat.SetString` accepts decimal/exponent forms and normalizes, giving `-0`→`0`, `42.0`→`42`, `4.2e1`→`42`.

- [ ] **Step 4: Run to verify pass** — `cd ../cyoda-go-spi && go test ./... -run TestComputeClaims`. Expected: PASS (all cases).

- [ ] **Step 5: Commit (SPI repo)**

```bash
cd ../cyoda-go-spi && git add unique_signature.go unique_signature_test.go go.mod go.sum
git commit -m "feat: ComputeClaims precision-preserving composite-key signature helper"
```

---

## Phase 2 — Declaration surface (capability gate, endpoint, persistence, export)

### Task 2.1: Per-plugin capability advertisement

**Files:**
- Modify: `plugins/memory/store_factory.go`, `plugins/sqlite/store_factory.go`, `plugins/postgres/store_factory.go` (the concrete `StoreFactory` types)
- Test: one test per plugin asserting the factory satisfies `spi.CompositeUniqueKeyCapable` and returns true.

**Interfaces:**
- Produces: each plugin's `*StoreFactory` implements `SupportsCompositeUniqueKeys() bool` (returns `true`).

- [ ] **Step 1: Write the failing test** (memory; mirror for sqlite/postgres)

`plugins/memory/capability_test.go`:
```go
package memory

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi"
)

func TestFactory_SupportsCompositeUniqueKeys(t *testing.T) {
	var f any = &StoreFactory{} // adjust to the real exported factory type/ctor
	c, ok := f.(spi.CompositeUniqueKeyCapable)
	if !ok || !c.SupportsCompositeUniqueKeys() {
		t.Fatal("memory factory must advertise composite unique key support")
	}
}
```

- [ ] **Step 2: Run to verify failure** — Expected: FAIL (method missing). Run per plugin from its module dir, e.g. `cd plugins/memory && go test ./... -run SupportsComposite`.

- [ ] **Step 3: Implement** — add to each plugin factory:
```go
// SupportsCompositeUniqueKeys advertises composite-unique-key enforcement.
func (f *StoreFactory) SupportsCompositeUniqueKeys() bool { return true }
```
(Use the actual factory receiver type/name in each plugin.)

- [ ] **Step 4: Run to verify pass** — Expected: PASS in all three plugin modules.

- [ ] **Step 5: Commit**

```bash
git add plugins/memory plugins/sqlite plugins/postgres
git commit -m "feat(plugins): advertise CompositeUniqueKeyCapable on memory/sqlite/postgres"
```

---

### Task 2.2: Model service `SetUniqueKeys` (capability + validation + persist)

**Files:**
- Modify: `internal/domain/model/service.go` (new method `SetUniqueKeys`)
- Modify: `internal/common/error_codes.go` (add codes — see Task 3.1; if doing strict TDD order, land Task 3.1 first)
- Test: `internal/domain/model/service_test.go`

**Interfaces:**
- Consumes: `schema.UnmarshalModel`, `schema.MarshalModel`, `schema.ValidateUniqueKeys`, `spi.CompositeUniqueKeyCapable`, `ModelStore.Get/Save`, model `State`.
- Produces: `func (h *Handler) SetUniqueKeys(ctx, entityName, modelVersion string, keys []schema.UniqueKey) (*ModelTransitionResult, error)`.

- [ ] **Step 1: Write the failing test** (table: unsupported-backend → 422 `COMPOSITE_KEY_UNSUPPORTED`; locked model → 409 `MODEL_ALREADY_LOCKED`; bad field → 422 `INVALID_UNIQUE_KEY_DEFINITION`; happy path → persisted + re-read via export). Use the in-memory factory fixture used elsewhere in `service_test.go`.

```go
func TestSetUniqueKeys_RejectsLockedModel(t *testing.T) {
	h, ref := newLockedModelFixture(t) // existing helper pattern in service_test.go
	_, err := h.SetUniqueKeys(ctxFor(t), ref.EntityName, ref.ModelVersion,
		[]schema.UniqueKey{{ID: "k", Fields: []string{"$.email"}}})
	assertAppErr(t, err, http.StatusConflict, common.ErrCodeModelAlreadyLocked)
}

func TestSetUniqueKeys_RejectsUnknownField(t *testing.T) {
	h, ref := newUnlockedModelWithEmail(t)
	_, err := h.SetUniqueKeys(ctxFor(t), ref.EntityName, ref.ModelVersion,
		[]schema.UniqueKey{{ID: "k", Fields: []string{"$.nope"}}})
	assertAppErr(t, err, http.StatusUnprocessableEntity, common.ErrCodeInvalidUniqueKeyDefinition)
}

func TestSetUniqueKeys_PersistsIntoSchema(t *testing.T) {
	h, ref := newUnlockedModelWithEmail(t)
	if _, err := h.SetUniqueKeys(ctxFor(t), ref.EntityName, ref.ModelVersion,
		[]schema.UniqueKey{{ID: "k", Fields: []string{"$.email"}}}); err != nil {
		t.Fatalf("set: %v", err)
	}
	desc, _ := h.modelStoreFor(t).Get(ctxFor(t), ref)
	_, keys, _ := schema.UnmarshalModel(desc.Schema)
	if len(keys) != 1 || keys[0].ID != "k" {
		t.Fatalf("keys not persisted: %+v", keys)
	}
}
```

- [ ] **Step 2: Run to verify failure** — Expected: FAIL (method/codes missing).

- [ ] **Step 3: Implement** `SetUniqueKeys` (model `service.go`), mirroring `LockModel`'s structure:

```go
func (h *Handler) SetUniqueKeys(ctx context.Context, entityName, modelVersion string, keys []schema.UniqueKey) (*ModelTransitionResult, error) {
	// Capability gate FIRST — reject on backends that can't enforce.
	if c, ok := h.factory.(spi.CompositeUniqueKeyCapable); !ok || !c.SupportsCompositeUniqueKeys() {
		return nil, common.Operational(http.StatusUnprocessableEntity,
			common.ErrCodeCompositeKeyUnsupported, "composite unique keys are not supported by the active storage backend")
	}

	store, err := h.factory.ModelStore(ctx)
	if err != nil {
		return nil, common.Internal("failed to access model store", err)
	}
	ver := parseVersion(modelVersion)
	ref := modelRef(entityName, ver)

	desc, err := getModelFresh(ctx, store, ref)
	if err != nil {
		return nil, classifyGetErr("set unique keys", entityName, ver, err)
	}
	if desc == nil {
		return nil, modelNotFound(entityName, ver)
	}
	if desc.State == spi.ModelLocked {
		appErr := common.Operational(http.StatusConflict, common.ErrCodeModelAlreadyLocked,
			fmt.Sprintf("cannot edit unique keys: model %s v%d is LOCKED", entityName, ver))
		appErr.Props = map[string]any{"entityName": entityName, "entityVersion": ver}
		return nil, appErr
	}

	node, _, err := schema.UnmarshalModel(desc.Schema)
	if err != nil {
		return nil, common.Internal("failed to unmarshal model schema", err)
	}
	if vErr := schema.ValidateUniqueKeys(node, keys); vErr != nil {
		var de *schema.UniqueKeyDefError
		if errors.As(vErr, &de) {
			return nil, common.Operational(http.StatusUnprocessableEntity,
				common.ErrCodeInvalidUniqueKeyDefinition, de.Reason)
		}
		return nil, common.Internal("unique key validation failed", vErr)
	}

	newSchema, err := schema.MarshalModel(node, keys)
	if err != nil {
		return nil, common.Internal("failed to marshal model schema", err)
	}
	desc.Schema = newSchema
	desc.UpdateDate = time.Now()
	if err := store.Save(ctx, desc); err != nil {
		return nil, common.Internal("failed to save model", err)
	}
	return &ModelTransitionResult{ModelID: deterministicID(ref).String(), State: string(desc.State)}, nil
}
```

- [ ] **Step 4: Run to verify pass** — Expected: PASS (after Task 3.1 codes exist).

- [ ] **Step 5: Commit**

```bash
git add internal/domain/model/service.go internal/domain/model/service_test.go
git commit -m "feat(model): SetUniqueKeys with capability gate + definition validation"
```

---

### Task 2.3: HTTP endpoint + OpenAPI + gRPC event + export round-trip

**Files:**
- Modify: `api/openapi.yaml` (add `PUT /model/{entityName}/{modelVersion}/unique-keys` + request schema); regenerate `api/generated.go` per the repo's codegen step.
- Modify: `internal/domain/model/handler.go` (new handler `SetEntityModelUniqueKeys`)
- Modify: `internal/grpc/model.go` (new `EntityModelManage` event branch `handleModelSetUniqueKeys`)
- Modify: `internal/domain/model/service.go` ExportMetadata path to include keys (verify `ExportMetadata`/`ExportModel` serialization includes the codec envelope — since keys live in `Schema`, exporting the schema already carries them; assert this in a test).
- Test: `internal/domain/model/handler_test.go`, `internal/grpc/model_test.go`

**Interfaces:**
- Consumes: `Handler.SetUniqueKeys`.
- Produces: HTTP `PUT /model/{entityName}/{modelVersion}/unique-keys`; gRPC event type `MODEL_SET_UNIQUE_KEYS` (or the repo's event-name convention).

- [ ] **Step 1: Write the failing tests** — HTTP: 200 on valid set, 409 on locked, 422 on bad field, 422 on unsupported backend. gRPC: envelope `Success=false`, `Error.Code` for the 422/409 cases. Export: a model with keys set, exported, re-imported/parsed, keys present.

(Write concrete `httptest` requests against the in-process server fixture used in `handler_test.go`; assert status + `errorCode` body field. For gRPC, build the CloudEvent payload and assert the response envelope, mirroring existing `handleModelTransition` tests.)

- [ ] **Step 2: Run to verify failure** — Expected: FAIL (route/handler/event missing).

- [ ] **Step 3: Implement** — add the OpenAPI path + regenerate; wire the handler to `h.svc.SetUniqueKeys`; add the gRPC event branch; confirm export carries `Schema` verbatim.

- [ ] **Step 4: Run to verify pass** — Expected: PASS. Run `go test ./internal/domain/model/... ./internal/grpc/...`.

- [ ] **Step 5: Commit**

```bash
git add api/openapi.yaml api/generated.go internal/domain/model/handler.go internal/grpc/model.go internal/domain/model/service.go internal/domain/model/handler_test.go internal/grpc/model_test.go
git commit -m "feat(model): unique-keys declaration endpoint (HTTP+gRPC) + export round-trip"
```

---

## Phase 3 — Error codes & mapping

### Task 3.1: Error codes + help topics + non-retryable mapping

**Files:**
- Modify: `internal/common/error_codes.go`
- Modify: `internal/common/errors.go` (`Internal` branch for `spi.ErrUniqueViolation`)
- Create: `cmd/cyoda/help/content/errors/UNIQUE_VIOLATION.md`, `INVALID_UNIQUE_KEY.md`, `COMPOSITE_KEY_UNSUPPORTED.md`, `INVALID_UNIQUE_KEY_DEFINITION.md`
- Test: `internal/common/errors_test.go`, and `TestErrCode_Parity` (already exists — must stay green)

**Interfaces:**
- Produces: `common.ErrCodeUniqueViolation = "UNIQUE_VIOLATION"`, `ErrCodeInvalidUniqueKey = "INVALID_UNIQUE_KEY"`, `ErrCodeCompositeKeyUnsupported = "COMPOSITE_KEY_UNSUPPORTED"`, `ErrCodeInvalidUniqueKeyDefinition = "INVALID_UNIQUE_KEY_DEFINITION"`. `common.Internal(_, err)` maps a wrapped `spi.ErrUniqueViolation` → non-retryable 409 `UNIQUE_VIOLATION`.

- [ ] **Step 1: Write the failing test** (`errors_test.go`)

```go
func TestInternal_MapsUniqueViolation(t *testing.T) {
	e := Internal("save", fmt.Errorf("wrap: %w", spi.ErrUniqueViolation))
	if e.Status != http.StatusConflict || e.Code != ErrCodeUniqueViolation {
		t.Fatalf("want 409 UNIQUE_VIOLATION, got %d %s", e.Status, e.Code)
	}
	if e.Retryable {
		t.Fatal("UNIQUE_VIOLATION must NOT be retryable")
	}
}

func TestInternal_ConflictStillRetryable(t *testing.T) {
	e := Internal("commit", fmt.Errorf("wrap: %w", spi.ErrConflict))
	if e.Code != ErrCodeConflict || !e.Retryable {
		t.Fatalf("CONFLICT must remain retryable; got %s retry=%v", e.Code, e.Retryable)
	}
}
```

- [ ] **Step 2: Run to verify failure** — Expected: FAIL (codes/branch missing). Also `go test ./cmd/cyoda/help/ -run TestErrCode_Parity` will FAIL once codes are added without topics — drives Step 3's `.md` files.

- [ ] **Step 3: Implement**

Add to `error_codes.go`:
```go
	// Composite unique keys
	ErrCodeUniqueViolation            = "UNIQUE_VIOLATION"
	ErrCodeInvalidUniqueKey           = "INVALID_UNIQUE_KEY"
	ErrCodeCompositeKeyUnsupported    = "COMPOSITE_KEY_UNSUPPORTED"
	ErrCodeInvalidUniqueKeyDefinition = "INVALID_UNIQUE_KEY_DEFINITION"
```

In `errors.go` `Internal`, add the unique-violation branch BEFORE the existing `ErrConflict` branch (more specific first):
```go
func Internal(message string, err error) *AppError {
	if err != nil && errors.Is(err, spi.ErrUniqueViolation) {
		return Operational(http.StatusConflict, ErrCodeUniqueViolation,
			"a composite unique key already exists with these values") // non-retryable: no .AsRetryable()
	}
	if err != nil && errors.Is(err, spi.ErrConflict) {
		return Operational(http.StatusConflict, ErrCodeConflict, "transaction conflict — retry").AsRetryable()
	}
	// ... unchanged ...
}
```

Create the four `.md` topics (follow `CONFLICT.md` shape; set Retryable `no` for all four; correct HTTP codes — 409 for UNIQUE_VIOLATION, 422 for the others). Example `UNIQUE_VIOLATION.md`:
```markdown
---
topic: errors.UNIQUE_VIOLATION
title: "UNIQUE_VIOLATION — composite unique key conflict"
stability: stable
see_also:
  - errors
  - errors.INVALID_UNIQUE_KEY
  - errors.CONFLICT
---

# errors.UNIQUE_VIOLATION

## NAME

UNIQUE_VIOLATION — a write would duplicate a declared composite unique key.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`.

## DESCRIPTION

Another live entity of the same model (within the tenant) already holds the same value-set for the named composite unique key. Deterministic — retrying the identical payload will fail again. Change the conflicting field values or update the existing entity. The response names the violated key id, not the incumbent entity.

## SEE ALSO

- errors
- errors.INVALID_UNIQUE_KEY
- errors.CONFLICT
```

- [ ] **Step 4: Run to verify pass** — `go test ./internal/common/ ./cmd/cyoda/help/` — Expected: PASS (parity bijection holds).

- [ ] **Step 5: Commit**

```bash
git add internal/common/error_codes.go internal/common/errors.go internal/common/errors_test.go cmd/cyoda/help/content/errors/
git commit -m "feat(errors): composite-unique-key error codes + non-retryable 409 mapping"
```

---

## Phase 4 — Service attaches key definitions + partial-key pre-check

### Task 4.1: Attach key DEFINITIONS to the entity + partial-key pre-check

**Files:**
- Modify: `internal/domain/entity/service.go` (CreateEntity, updateEntityCore, CreateEntityCollection)
- Test: `internal/domain/entity/service_test.go`

**Interfaces:**
- Consumes: `spi.ComputeClaims`, `spi.ErrPartialUniqueKey`, `schema.UnmarshalModel`.
- Produces: entities passed to write paths carry `entity.UniqueKeys` (the model's key definitions); a **partial key on the input doc** → 422 `INVALID_UNIQUE_KEY` *before* `engine.Execute`. The **store** computes claims from the live doc (Phases 5–7) — the service does NOT precompute claims.

> Design note (why definitions, not claims): the engine saves the primary entity internally
> after processors may mutate key fields (`engine_processors.go:311,345`), so enforcement
> lives in the store and reads the LIVE doc. The service only (a) attaches the stable key
> definitions so the store has them, and (b) pre-checks the *input* doc for a partial key so
> the common client-error case is a clean 422 even on a segmenting workflow.

- [ ] **Step 1: Write the failing test**

```go
func TestCreateEntity_PartialKeyRejected(t *testing.T) {
	h := newEntityHandlerWithUniqueKey(t, []string{"$.email", "$.age"}) // sets a unique key on the model
	_, err := h.CreateEntity(ctxFor(t), CreateEntityInput{
		EntityName: "C", ModelVersion: "1", Format: "JSON", Data: `{"email":"a@x.com"}`,
	})
	assertAppErr(t, err, http.StatusUnprocessableEntity, common.ErrCodeInvalidUniqueKey)
}

func TestCreateEntity_AttachesUniqueKeysToEntity(t *testing.T) {
	spy := &spyEntityStore{} // captures the *spi.Entity passed to Save
	h := newEntityHandlerWithUniqueKeyAndStore(t, []string{"$.email"}, spy)
	_, err := h.CreateEntity(ctxFor(t), CreateEntityInput{
		EntityName: "C", ModelVersion: "1", Format: "JSON", Data: `{"email":"a@x.com"}`,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(spy.lastSaved.UniqueKeys) != 1 || spy.lastSaved.UniqueKeys[0].ID == "" {
		t.Fatalf("entity.UniqueKeys not attached: %+v", spy.lastSaved.UniqueKeys)
	}
}
```

- [ ] **Step 2: Run to verify failure** — Expected: FAIL.

- [ ] **Step 3: Implement** — add a helper and call it on each write path:

```go
// prepareUniqueKeys attaches the model's unique-key DEFINITIONS to the entity
// (so the store can compute claims from the live doc) and pre-checks the input
// doc: a partially-filled key is a fast 422 before any engine work.
func (h *Handler) prepareUniqueKeys(desc *spi.ModelDescriptor, inputDoc []byte, entity *spi.Entity) error {
	_, keys, err := schema.UnmarshalModel(desc.Schema)
	if err != nil {
		return common.Internal("failed to read model schema", err)
	}
	if len(keys) == 0 {
		return nil
	}
	entity.UniqueKeys = keys
	// Pre-check the input doc (clean 422 for the common client-error case).
	if _, err := spi.ComputeClaims(keys, inputDoc); err != nil {
		if errors.Is(err, spi.ErrPartialUniqueKey) {
			return common.Operational(http.StatusUnprocessableEntity, common.ErrCodeInvalidUniqueKey, err.Error())
		}
		return common.Internal("failed to evaluate unique keys", err)
	}
	return nil
}
```

Call sites (place the call AFTER `desc` is loaded and the body parsed, BEFORE `engine.Execute`):
- `CreateEntity`: `if err := h.prepareUniqueKeys(desc, bodyBytes, entity); err != nil { return nil, err }` — `desc` is loaded before `Begin`; do the pre-check before `txMgr.Begin` so a partial key needs no rollback.
- `updateEntityCore`: after the PATCH merge produces `bodyBytes` and validation, call `prepareUniqueKeys(desc, bodyBytes, updated)` — the merged doc is the input.
- `CreateEntityCollection`: per item, `prepareUniqueKeys(itemDesc, item.payloadBytes, entity)` (load each item's `desc` — already loaded in the validation loop; thread it through).

> Note (processor-mutates-key-field): no special handling needed here — the store recomputes
> claims from the final `entity.Data`, so a processor that rewrites a key field is enforced
> on its final value automatically. Add a Phase 8 e2e test with a processor that rewrites a
> key field and assert the FINAL value is the one enforced (create a second entity with the
> rewritten value → 409). `copyEntity` (memory) must copy `UniqueKeys` (Task 7.1).

- [ ] **Step 4: Run to verify pass** — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/entity/service.go internal/domain/entity/service_test.go
git commit -m "feat(entity): attach unique-key definitions + partial-key pre-check"
```

---

## Phase 5 — PostgreSQL enforcement

### Task 5.1: Migration — `unique_claims` table + UNIQUE index + RLS

**Files:**
- Create: `plugins/postgres/migrations/000003_unique_claims.up.sql`, `..._down.sql`
- Test: covered by Task 5.2 integration (a migration smoke assertion in the postgres suite)

- [ ] **Step 1: Write the failing test** — in `plugins/postgres` add a test that opens a migrated test DB (existing testcontainer harness) and asserts the `unique_claims` table + the named unique index exist. Expected FAIL pre-migration.

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement** (`000003_unique_claims.up.sql`):
```sql
CREATE TABLE IF NOT EXISTS unique_claims (
    tenant_id     TEXT NOT NULL,
    model_name    TEXT NOT NULL,
    model_version TEXT NOT NULL,
    key_id        TEXT NOT NULL,
    signature     TEXT NOT NULL,
    entity_id     TEXT NOT NULL,
    PRIMARY KEY (tenant_id, entity_id, key_id)
);

-- Enforcement index: at most one LIVE entity per value-set. Plain UNIQUE —
-- claim rows exist only for live entities (deleted on soft-delete), so no
-- partial predicate is needed.
CREATE UNIQUE INDEX IF NOT EXISTS unique_claims_uq
    ON unique_claims (tenant_id, model_name, model_version, key_id, signature);

ALTER TABLE unique_claims ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_unique_claims ON unique_claims
    USING (tenant_id = current_setting('app.current_tenant', true));
```
Down: `DROP TABLE IF EXISTS unique_claims;`

- [ ] **Step 4: Run to verify pass.**

- [ ] **Step 5: Commit**

```bash
git add plugins/postgres/migrations/000003_unique_claims.up.sql plugins/postgres/migrations/000003_unique_claims.down.sql plugins/postgres/*_test.go
git commit -m "feat(postgres): unique_claims table + UNIQUE index + RLS"
```

---

### Task 5.2: Postgres claim maintenance + constraint-name classification

**Files:**
- Modify: `plugins/postgres/entity_store.go` (Save, Delete, DeleteAll, SaveAll) to maintain claims
- Modify: `plugins/postgres/transaction_manager.go` `classifyError` (constraint-name → `ErrUniqueViolation`)
- Create: `plugins/postgres/unique_claims.go` (claim upsert/release helpers)
- Test: `plugins/postgres/unique_claims_test.go` (integration, testcontainer)

**Interfaces:**
- Consumes: `entity.UniqueKeys`, `entity.Data`, `spi.ComputeClaims`, `spi.ErrPartialUniqueKey`, `classifyError`.
- Produces: the store computes claims from `(entity.UniqueKeys, entity.Data)` inside `Save` and maintains claim rows in the same tx; a `unique_claims_uq` violation → `spi.ErrUniqueViolation`. A partial key produced by a processor → `spi.ErrPartialUniqueKey` (bubbles up; the handler/engine sanitizes it).

- [ ] **Step 1: Write the failing test** — within one tenant/model: (a) two saves with the same claim signature on different entity ids → second returns `spi.ErrUniqueViolation`; (b) soft-delete the first, then save the second with that signature → succeeds (value freed); (c) `DeleteAll` then re-save same signature → succeeds; (d) update entity to a new signature frees the old; (e) a `23505` on the `entities` PK still surfaces as a non-unique-violation error (sanity that only the claim constraint maps).

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement**

`classifyError` — extend (keep existing 40001/40P01 branch):
```go
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == "unique_claims_uq":
			return fmt.Errorf("%w: %w", spi.ErrUniqueViolation, err)
		case pgErr.Code == pgerrcode.SerializationFailure || pgErr.Code == pgerrcode.DeadlockDetected:
			return fmt.Errorf("%w: %w", spi.ErrConflict, err)
		}
	}
	return err
}
```

`unique_claims.go` — helpers run through `s.q` (the classifying querier), so the index violation arrives pre-classified. The store computes claims from the LIVE doc here (not from a precomputed field):
```go
// replaceClaims computes this entity's claims from its live doc, deletes its
// existing claim rows, and inserts the new set, in the current tx. A
// unique_claims_uq collision surfaces (via the classifying querier) as
// spi.ErrUniqueViolation. ErrPartialUniqueKey bubbles up unchanged.
func (s *entityStore) replaceClaims(ctx context.Context, e *spi.Entity) error {
	claims, err := spi.ComputeClaims(e.UniqueKeys, e.Data)
	if err != nil {
		return err // ErrPartialUniqueKey or a canonicalization error
	}
	tid := string(s.tenantID)
	if _, err := s.q.Exec(ctx, `DELETE FROM unique_claims WHERE tenant_id=$1 AND entity_id=$2`, tid, e.Meta.ID); err != nil {
		return fmt.Errorf("clear claims: %w", err)
	}
	for _, c := range claims {
		if _, err := s.q.Exec(ctx,
			`INSERT INTO unique_claims (tenant_id, model_name, model_version, key_id, signature, entity_id)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			tid, e.Meta.ModelRef.EntityName, e.Meta.ModelRef.ModelVersion, c.KeyID, c.Signature, e.Meta.ID); err != nil {
			return fmt.Errorf("insert claim: %w", err) // already classified
		}
	}
	return nil
}

// releaseClaims removes all claim rows for an entity (soft-delete / DeleteAll).
func (s *entityStore) releaseClaims(ctx context.Context, entityID string) error {
	_, err := s.q.Exec(ctx, `DELETE FROM unique_claims WHERE tenant_id=$1 AND entity_id=$2`, string(s.tenantID), entityID)
	return err
}
```

Wire into `Save` (call `s.replaceClaims(ctx, entity)` after the version row insert, before `return nextVersion`), into the soft-delete path (`releaseClaims`), into `DeleteAll` (release for every affected entity id — capture ids before bulk delete), and `SaveAll` (per entity — the loop already calls Save per entity; ensure claims attached on each).

> Note: `DeleteAll` currently bulk-soft-deletes. Fetch the affected entity ids first (the service's `DeleteAllEntities` already calls `GetAll` before `DeleteAll`; but enforcement must live in the store to stay atomic). Implement `DeleteAll` to `DELETE FROM unique_claims WHERE tenant_id=$1 AND (model_name,model_version)=($2,$3)` in the same tx as the soft-delete — simplest and atomic.

- [ ] **Step 4: Run to verify pass** — `cd plugins/postgres && go test ./... -run UniqueClaims` (Docker).

- [ ] **Step 5: Commit**

```bash
git add plugins/postgres/entity_store.go plugins/postgres/transaction_manager.go plugins/postgres/unique_claims.go plugins/postgres/unique_claims_test.go
git commit -m "feat(postgres): enforce composite unique keys via unique_claims side table"
```

---

## Phase 6 — SQLite enforcement

### Task 6.1: Migration + claim maintenance + UNIQUE detection

**Files:**
- Create: `plugins/sqlite/migrations/000002_unique_claims.up.sql`, `..._down.sql`
- Modify: `plugins/sqlite/entity_store.go` (claim maintenance in Save/Delete/DeleteAll), `plugins/sqlite/errors.go` (`classifyError` — distinguish the `unique_claims` index)
- Create: `plugins/sqlite/unique_claims.go`
- Test: `plugins/sqlite/unique_claims_test.go`

**Interfaces:** mirror Task 5.2.

- [ ] **Step 1: Write the failing test** — same scenarios as 5.2 (a–d), against the sqlite store.

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement**

Migration `000002_unique_claims.up.sql`:
```sql
CREATE TABLE unique_claims (
    tenant_id     TEXT NOT NULL,
    model_name    TEXT NOT NULL,
    model_version TEXT NOT NULL,
    key_id        TEXT NOT NULL,
    signature     TEXT NOT NULL,
    entity_id     TEXT NOT NULL,
    PRIMARY KEY (tenant_id, entity_id, key_id)
) STRICT;

CREATE UNIQUE INDEX unique_claims_uq
    ON unique_claims (tenant_id, model_name, model_version, key_id, signature);
```

`errors.go` — a `unique_claims_uq` violation must map to `ErrUniqueViolation`, NOT the existing retryable `ErrConflict`. SQLite reports the index/columns in the message ("UNIQUE constraint failed: unique_claims.signature" / names the index). Add a dedicated wrapper used by the claim helpers so the generic classifier is not relied on:
```go
// classifyClaimError maps a UNIQUE violation on unique_claims to the
// non-retryable spi.ErrUniqueViolation. Used only by claim writes so the
// generic classifyError (which treats UNIQUE as retryable ErrConflict for
// the entities/PK path) is not consulted here.
func classifyClaimError(err error) error {
	if err == nil {
		return nil
	}
	var xcode sqlite3.ExtendedErrorCode
	if errors.As(err, &xcode) && xcode == sqlite3.CONSTRAINT_UNIQUE {
		return fmt.Errorf("%w: %w", spi.ErrUniqueViolation, err)
	}
	return err
}
```

`unique_claims.go` — same shape as postgres: `replaceClaims` calls `spi.ComputeClaims(e.UniqueKeys, e.Data)`, then deletes the entity's existing rows and inserts the computed set, wrapping the INSERT in `classifyClaimError`. Enforcement happens in the **sqlite commit flush** (`flushToSQLite`, `plugins/sqlite/txmanager.go:300`), where the real sql tx exists, so claim INSERTs and entity INSERTs are one atomic sql tx: in the `tx.Buffer` loop (`txmanager.go:311`) call `replaceClaims` per entity; in the `tx.Deletes` loop (`txmanager.go:379`) call `releaseClaims` per tombstoned id. Also wire the non-tx `saveDirectly` path. Because `tx.Buffer` holds the buffered `*spi.Entity` (carrying `UniqueKeys` + final `Data`), the flush computes claims from the live doc.

- [ ] **Step 4: Run to verify pass** — `cd plugins/sqlite && go test ./... -run UniqueClaims`.

- [ ] **Step 5: Commit**

```bash
git add plugins/sqlite/migrations/000002_unique_claims.up.sql plugins/sqlite/migrations/000002_unique_claims.down.sql plugins/sqlite/entity_store.go plugins/sqlite/errors.go plugins/sqlite/unique_claims.go plugins/sqlite/unique_claims_test.go
git commit -m "feat(sqlite): enforce composite unique keys via unique_claims side table"
```

---

## Phase 7 — Memory enforcement

### Task 7.1: Signature map under entityMu in commit; release on delete paths

**Files:**
- Modify: `plugins/memory/store_factory.go` (add `uniqueClaims map[claimKey]string` + the mutex is the existing `entityMu`)
- Modify: `plugins/memory/entity_store.go` (`copyEntity` must also copy `UniqueKeys`; non-tx Save/Delete/DeleteAll paths maintain claims under `entityMu`)
- Modify: `plugins/memory/txmanager.go` (commit flush: compute + enforce + record; tombstone loop: release)
- Test: `plugins/memory/unique_claims_test.go`

> **First** update `copyEntity` (`entity_store.go:37-41`) to copy the `UniqueKeys` slice — otherwise buffered entities lose their key definitions and the flush computes zero claims:
> ```go
> func copyEntity(e *spi.Entity) *spi.Entity {
> 	cp := &spi.Entity{Meta: e.Meta, Data: make([]byte, len(e.Data))}
> 	copy(cp.Data, e.Data)
> 	if len(e.UniqueKeys) > 0 {
> 		cp.UniqueKeys = append([]spi.UniqueKey(nil), e.UniqueKeys...)
> 	}
> 	return cp
> }
> ```

**Interfaces:**
- Produces: in-memory uniqueness enforced at the commit critical section; `spi.ErrUniqueViolation` on collision.

- [ ] **Step 1: Write the failing test** — scenarios (a–d) from 5.2 against the memory store, PLUS the concurrency winner/loser (two goroutines, same signature, distinct ids → exactly one `ErrUniqueViolation`). Keep this concurrency test in the memory plugin's own suite (NOT parity).

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement**

Add to the factory:
```go
type claimKey struct{ tenant, model, version, keyID, signature string }
// guarded by entityMu:
uniqueClaims map[claimKey]string // -> entityID currently holding it
// reverse index for release: entityID -> []claimKey
claimsByEntity map[string][]claimKey
```

In `txmanager.Commit`, inside the `entityMu.Lock()` critical section (after FCW conflict detection, during/after the buffer flush, BEFORE releasing `entityMu`):
```go
// Enforce + record claims for buffered (written) entities, ignoring snapshot
// time — compare against CURRENT committed claims. Release for tombstones.
// First pass: validate all claims for this tx (so a collision aborts before
// any mutation), then apply. Claims are computed HERE from the live buffered
// doc via the shared SPI helper.
pending := map[claimKey]string{}
for _, entity := range tx.Buffer {
	claims, err := spi.ComputeClaims(entity.UniqueKeys, entity.Data)
	if err != nil {
		m.factory.entityMu.Unlock() // unwind as in the FCW abort branch
		return err // ErrPartialUniqueKey bubbles up
	}
	for _, c := range claims {
		ck := claimKey{string(tid), entity.Meta.ModelRef.EntityName, entity.Meta.ModelRef.ModelVersion, c.KeyID, c.Signature}
		// Held by another entity? (ignore a claim currently held by THIS entity — moving/no-op)
		if holder, ok := m.factory.uniqueClaims[ck]; ok && holder != entity.Meta.ID {
			// release locks, abort:
			delete(m.committing, txID); /* ...unwind active/savepoints as in the FCW branch... */
			m.factory.entityMu.Unlock()
			return spi.ErrUniqueViolation
		}
		// Intra-batch duplicate within this same commit?
		if other, ok := pending[ck]; ok && other != entity.Meta.ID {
			m.factory.entityMu.Unlock()
			return spi.ErrUniqueViolation
		}
		pending[ck] = entity.Meta.ID
	}
}
```
Then, when applying the flush: first release every claim previously held by each written entity id (so a moved key frees its old signature) and every tombstoned id, then insert the `pending` set. Maintain `claimsByEntity` alongside.

For the **non-tx** Save/Delete/DeleteAll paths (`entity_store.go`), perform the same enforce-record / release under `entityMu.Lock()` already held there.

> Note: replicate the exact unwind (delete from `m.committing`/`m.active`/`m.savepoints`, unlock `m.mu` if held, unlock `entityMu`) used by the existing FCW `return spi.ErrConflict` branch in `Commit` so locks are released identically on the unique-violation abort.

- [ ] **Step 4: Run to verify pass** — `cd plugins/memory && go test ./... -run UniqueClaims`. Also run with `-race` for the concurrency test: `go test -race ./... -run UniqueClaims`.

- [ ] **Step 5: Commit**

```bash
git add plugins/memory/store_factory.go plugins/memory/txmanager.go plugins/memory/entity_store.go plugins/memory/unique_claims_test.go
git commit -m "feat(memory): enforce composite unique keys in commit critical section"
```

---

## Phase 8 — Cross-stack coverage (e2e, gRPC, parity, concurrency)

### Task 8.1: HTTP e2e (postgres) — every status code on a running backend

**Files:**
- Create: `internal/e2e/unique_keys_test.go`

**Coverage (spec §6/§7):** declare key (200); declare on locked model (409); bad field (422 `INVALID_UNIQUE_KEY_DEFINITION`); create duplicate (409 `UNIQUE_VIOLATION`); partial key (422 `INVALID_UNIQUE_KEY`); update moves key (409 on collision, 200 when free); PATCH nulls a key field (422); soft-delete frees value (re-create 201); `DeleteAll` frees values; `SaveAll` intra-batch duplicate (409); multiple keys per model.

- [ ] **Step 1: Write the tests** (full happy + every error path, asserting HTTP status + `errorCode` body). Use the existing `internal/e2e` `TestMain` harness (testcontainer Postgres + httptest server).
- [ ] **Step 2: Run — Expected FAIL until wired** (most pass after Phases 2–5; this task is the consolidated assertion).
- [ ] **Step 3:** fix any gaps surfaced.
- [ ] **Step 4: Run to verify pass** — `go test ./internal/e2e/... -run UniqueKeys -v`.
- [ ] **Step 5: Commit.**

### Task 8.2: gRPC coverage

**Files:** `internal/grpc/entity_test.go`, `internal/grpc/model_test.go`
- [ ] Assert the envelope (`Success=false`, `Error.Code`) for `UNIQUE_VIOLATION`, `INVALID_UNIQUE_KEY`, `COMPOSITE_KEY_UNSUPPORTED`, `INVALID_UNIQUE_KEY_DEFINITION` on the gRPC entry points. RED → GREEN → commit.

### Task 8.3: Cross-backend parity

**Files:** `e2e/parity/unique_keys.go` (scenarios) + register in `e2e/parity/registry.go`
- [ ] Scenarios (backend-agnostic): create-dup→409; soft-delete-frees-value; partial-key→422; all-null-exempt; `DeleteAll`-frees-values; multiple-keys. Register via `Register(NamedTest{...})`.
- [ ] **Capability-gate the positive scenarios:** skip when the backend reports unsupported; assert `COMPOSITE_KEY_UNSUPPORTED` on unsupported backends (the commercial backend picks this up on its next dep update). Use a `fixture`-level capability probe.
- [ ] RED → GREEN → commit. Run `go test ./e2e/parity/... -run UniqueKeys` across memory/sqlite/postgres.

### Task 8.4: Concurrency (isolated, single-backend)

**Files:** `internal/e2e/unique_keys_concurrency_test.go`
- [ ] Two concurrent creates with the same value-set on postgres → exactly one 201, the other 409 `UNIQUE_VIOLATION`, no torn write; assert exactly one live entity holds the value. NOT in the parity suite (per `.claude/rules/concurrency-tests-not-in-parity`). RED → GREEN → commit.

---

## Phase 9 — Docs, schema-versioning, and coordinated SPI release

### Task 9.1: Gate-4 documentation

**Files:**
- Modify: `README.md` (composite unique keys — declaration endpoint, semantics, byte-exact note)
- Modify: `docs/workflow-schema-versioning.md` (the codec envelope/back-compat note; if the model-schema import surface has its own version stamp, bump per the rubric)
- Modify: `CHANGELOG.md` (Added: composite unique keys; new error codes)
- Modify: `cmd/cyoda/help/content/...` (a topic for the `unique-keys` endpoint if the help tree documents model endpoints; verify and add)
- [ ] Update each; run `go test ./cmd/cyoda/help/...` (help topic tests) green. Commit.

### Task 9.2: Coordinated SPI release + pin bump (FINAL)

> Do this only after ALL prior phases are green locally via `go.work`.

**Files:**
- Modify: `go.mod` (root), `plugins/memory/go.mod`, `plugins/sqlite/go.mod`, `plugins/postgres/go.mod` (bump `cyoda-go-spi` pin)
- Modify: `COMPATIBILITY.md`

- [ ] **Step 1:** In `../cyoda-go-spi`: ensure Phase 0 commits are on `main`, push, and tag a fresh version (e.g. `v0.8.2`) per `MAINTAINING.md` — never force-move an existing tag.
- [ ] **Step 2:** Bump the `cyoda-go-spi` require pin to the new tag in all four `go.mod` files. `go mod tidy` in root + each plugin.
- [ ] **Step 3:** Remove the local `go.work` use-directive for the SPI (or confirm CI ignores `go.work`); `git update-index --no-skip-worktree go.work` only if you intentionally changed it — otherwise leave it untouched.
- [ ] **Step 4:** Update `COMPATIBILITY.md` (cyoda-go × cyoda-go-spi row).
- [ ] **Step 5:** `make test-all` (root + plugins) green; `make race` green; `go test ./internal/e2e/...` green.
- [ ] **Step 6: Commit** — one commit bumping all four pins + COMPATIBILITY.

```bash
git add go.mod go.sum plugins/*/go.mod plugins/*/go.sum COMPATIBILITY.md
git commit -m "chore: pin cyoda-go-spi <new-tag> for composite unique keys"
```

---

## Self-Review (run before execution)

- **Spec coverage:** every spec section maps to a task — §2 semantics (1.3/2.2/4.1/8.x), §3 architecture (0.2/1.3/5/6/7), §3.5 error classification (3.1/5.2/6.1), §3.6 capability (0.3/2.1/2.2), §3.7 SaveAll (5.2/6.1/8.1), §4 declaration (1.1/2.2/2.3), §6 error table (3.1/8.1/8.2), §7 coverage matrix (Phase 8), §8 cross-cutting (9.x). ✅
- **Type consistency:** `spi.UniqueKey`(= `schema.UniqueKey` alias), `spi.UniqueClaim{KeyID,Signature}`, `spi.ComputeClaims`, `spi.ErrPartialUniqueKey`, `spi.ErrUniqueViolation`, `spi.Entity.UniqueKeys`, `ValidateUniqueKeys`/`UniqueKeyDefError`, `MarshalModel`/`UnmarshalModel`, `unique_claims_uq`, `ErrCode*` constants — used identically across tasks. ✅
- **Open verification notes — all resolved from code** and pinned in the tasks: codec helpers are `toWire`/`fromWire` (`codec.go:17,44`); processors **can** mutate key fields → enforcement is in the store on the live doc (§3.1, Phase 5–7); sqlite flush is `flushToSQLite` (`txmanager.go:300`, buffer loop `:311`, deletes loop `:379`); memory factory is `StoreFactory`/`NewStoreFactory`; `transitions.go:19` is read-only (not a save path); `gjson v1.19.0` already a root dep (add to SPI go.mod). The only environment-specific value left is the local `go.work` path to the SPI checkout (Task 0.1) — a dev-setup path, not a code decision.
- **No open questions remain for executors.** A subagent that nonetheless encounters an unspecified decision must STOP and surface it (per the execution protocol) — never assume or descope.
