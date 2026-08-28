# ModelNode holds a set of branches Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `ModelNode`'s single `kind NodeKind` label with the set of
branches it was always summarising, so every reader sees every branch a field
declares — and the two `changeLevel` limitations that followed from the label
disappear.

**Architecture:** A node holds `branches map[NodeKind]Branch` plus a `nullable`
flag. `mergeKind`'s arbitrary tiebreak, the "dominant kind", `ConcreteTypes` and
`isNullOnlyLeaf` are deleted; set union replaces them. `Extend`'s gate becomes a
subset test over branch sets, `Diff` becomes per-branch, and a new additive op
`add_kind_branch` records a branch being added. `ErrPolymorphicSlot` and the
`POLYMORPHIC_SLOT` error code have no remaining raise site and are decommissioned.

**Tech Stack:** Go 1.26, `internal/domain/model/schema`, `internal/domain/model/exporter`,
`internal/domain/model/schema/gentree`, `internal/e2e`, `e2e/parity`.

**Spec:** GitHub issue #534 (self-contained design). Read it in full before
starting: `gh issue view 534`. This plan resolves the points the issue left to
implementation; those resolutions are in **Design decisions** below and are
binding.

## Global Constraints

- **TDD is mandatory** (`.claude/rules/tdd.md`): every step is RED → GREEN. No
  implementation code without a failing test driving it.
- Go 1.26+; `log/slog` only; wrap errors with `fmt.Errorf("...: %w", err)`.
- **No issue IDs in shipped artefacts** — no `#534` in code comments, errors,
  logs, responses, OpenAPI or help content. Commit messages, PR bodies and this
  plan may reference it.
- Milestone **v0.8.4**, base branch **`release/v0.8.4`**. Backward compatibility
  is not a constraint for this release; a breaking change gets a `### Breaking`
  CHANGELOG entry.
- Run scoped tests during iteration (`go test ./internal/domain/model/...`);
  full suite (`go test ./... -v`), `make race`, `make test-all` and
  `go vet ./...` once, at end of deliverable.
- Docker is required for `internal/e2e`. See `reference_docker_pull_broken_use_local_images`:
  `postgres:17-alpine` is already local.

---

## Design decisions

These resolve what the issue left open. They are measured against the current
code, not assumed.

### D1 — The struct

```go
type ModelNode struct {
	branches   map[NodeKind]Branch
	nullable   bool
	fieldCache atomic.Pointer[cachedFields]
}

type Branch interface{ Kind() NodeKind }

type ScalarBranch struct{ types *TypeSet }              // NULL is NEVER stored here
type ObjectBranch struct{ children map[string]*ModelNode }
type ArrayBranch  struct {
	element *ModelNode
	info    *ArrayInfo
}
```

Accessors: `Scalar() *ScalarBranch`, `Object() *ObjectBranch`,
`Array() *ArrayBranch` (nil when absent); `Kinds() []NodeKind` in ascending
`NodeKind` order (LEAF, OBJECT, ARRAY); `IsPolymorphic() bool` == `len(branches) > 1`;
`Nullable() bool`. **`ModelNode.Kind()` is deleted** — there is no dominant kind.
`NodeKind` and `NodeKind.String()` stay (they name a branch).

### D2 — The nullable collapse rule

> **`nullable` is recorded only while the node has no scalar branch. Adding a
> scalar branch clears it; setting it while a scalar branch is present is a
> no-op.**

This is `spi.TypeSet.Add`'s existing rule ("NULL is dropped when any concrete
type is present") lifted to the node, and it is what makes the restructure
behaviour-preserving: today a leaf observed as `null` then `"x"` stores
`types={STRING}` and the nullability is already not recorded. A node carrying a
scalar branch admits `null` anyway (`validateLeaf` returns nil for `nil` data),
so nothing is lost.

Consequences:
- `null` only → `branches={}`, `nullable=true` — the nullable-marker node.
- `null` then `{"k":"v"}` → `branches={OBJECT}`, `nullable=true`.
- `null` then `"x"` → `branches={LEAF{STRING}}`, `nullable=false`.

### D3 — `Types()` stays, as a derived compatibility accessor

```go
// Types returns the node's DataTypes in the spelling the field walk, the
// exporters and the persisted form use: the scalar branch's concrete types, or
// the single NULL marker when the node is nullable and carries no scalar
// branch, or an empty set.
func (n *ModelNode) Types() *TypeSet
```

It returns a **fresh** TypeSet — callers must not mutate it. Every current
mutation site (`apply.go:106,134,142`) is converted in Task 1 to explicit
mutators (D4).

### D4 — Explicit mutators replace `Types().Add`

```go
func (n *ModelNode) AddScalarTypes(dts ...DataType)  // NULL among them sets nullable per D2
func (n *ModelNode) SetNullable()                    // no-op when a scalar branch is present
func (n *ModelNode) SetElement(e *ModelNode)         // creates the ARRAY branch if absent
```

### D5 — Wire format

```go
type wireNode struct {
	Kind     string               `json:"kind,omitempty"`
	Kinds    []string             `json:"kinds,omitempty"`
	Types    []string             `json:"types,omitempty"`
	Children map[string]*wireNode `json:"children,omitempty"`
	Element  *wireNode            `json:"element,omitempty"`
}
```

**Encode.** `Types` is `n.Types()` — unchanged semantics, NULL included as the
marker. When the node has at most one branch, emit `kind`: the single branch's
name, or `"LEAF"` when there is none (the nullable marker, and the unreachable
empty node). Otherwise emit `kinds`, ascending. Result: **every monomorphic
node, nullable or not, serialises byte-identically to today.**

**Decode is payload-driven as well as kind-driven.** The branch set is

```
kinds(from "kinds", else ["kind"])  ∪  {LEAF if concrete types}  ∪  {OBJECT if children}  ∪  {ARRAY if element}
```

with `NULL ∈ types` ⇒ `nullable`, and concrete types ⇒ the scalar branch's
types. Two consequences that matter:
- A model persisted under the old dominant-kind spelling
  (`{"kind":"OBJECT","types":["STRING"],"children":…,"element":…}`) restores all
  three branches. No migration.
- `{"kind":"LEAF","types":["NULL"]}` restores `branches={}, nullable=true` (the
  marker); `{"kind":"LEAF"}` restores an empty scalar branch; `{"kind":"ARRAY"}`
  restores an ARRAY branch with a nil element. Each round-trips to itself.

A node carrying neither a kind nor any payload is still rejected
(`unknown node kind`), unchanged: the codec fails closed.

### D6 — ChangeLevel for adding a branch

- The node **already declares ≥1 branch** → adding another creates a union →
  **`STRUCTURAL`** (the issue's settled decision).
- The node **declares none** (the nullable marker) → this is the existing
  nullable-promotion contract, unchanged: **`TYPE`** at node level,
  **`ARRAY_ELEMENTS`** directly on an array element.

The carve-out is not a concession to tests — it is today's documented contract
and it is what "adding a branch" means: promoting `{}` → `{OBJECT}` leaves the
node monomorphic, so no polymorphic slot is created. `TestExtend_ExistingLeafNull_AgainstIncomingArray_PromotesToArray`
and the `null` → `"x"` widening both depend on it.

### D7 — Which op records what

| transition | op | why |
|---|---|---|
| `nullable` false → true | `broaden_type` payload `["NULL"]` | today's spelling, unchanged |
| scalar types widen on an existing scalar branch | `broaden_type` | unchanged |
| **branchless** node gains a scalar branch | `broaden_type` with the concrete types | today's spelling for `null` → `"x"`; keeps deltas byte-identical |
| branchless node gains an OBJECT/ARRAY branch | `add_kind_branch` | today `Extend` accepts this and `Diff` then fails — see D8 |
| a node with ≥1 branch gains any branch | `add_kind_branch` | limitation 1 |
| ARRAY branch with a nil element gains a LEAF element | `add_array_item_type` | unchanged |
| ARRAY branch with a nil element gains an OBJECT/ARRAY element | `add_kind_branch` on the array's element path | closes the `TODO(A.3 / issue #85)` in `diffArray` |

`applyBroadenType` keeps its guard in the new vocabulary: non-NULL types may be
added only when the target **has a scalar branch or has no branches at all** —
which is exactly today's `target.Kind() == KindLeaf`.

### D8 — Two live defects this closes on the way

1. `existing = LEAF[NULL]`, `incoming = OBJECT` at `TYPE`: `Extend` accepts it,
   `Diff` then returns `kind change … (not additive)` and
   `ingest.validateOrExtend` turns that into a `500 ErrInternalSchema`. The
   comment at `internal/domain/model/ingest/validate.go:104-110` documents the
   unique-key guard as a partial workaround. `add_kind_branch` expresses it.
2. `existing = LEAF{STRING}`, `incoming = LEAF{NULL}` (writing `null` to a
   string field) below `TYPE` is rejected today with "type change requires TYPE
   level", although the resulting delta is empty. Under D2 it proposes nothing
   and is accepted at every level.

Both are behaviour changes. Both get a CHANGELOG line and a test.

### D9 — `ErrPolymorphicSlot` is decommissioned

After Task 5 there is no raise site: adding a branch is permitted at
`STRUCTURAL` and refused below it as an ordinary change-level violation, which
raising the level *does* resolve. The sentinel's whole meaning ("raising
`changeLevel` does not help") becomes false. `extend.go:32` already records
`TODO(#85): decommission this sentinel + common.ErrCodePolymorphicSlot` once
these semantics land — this is that. Leaving it in place would be dead code
(Gate 6).

Removed: `schema.ErrPolymorphicSlot`, `common.ErrCodePolymorphicSlot`,
`cmd/cyoda/help/content/errors/POLYMORPHIC_SLOT.md`, its row in
`errors.md`, its `see_also` entries in `INCOMPATIBLE_TYPE.md` and
`VALIDATION_FAILED.md`, its cell in `internal/e2e/zzz_errorcode_matrix_test.go`,
its mention in `api/openapi.yaml`, and the classification arm in
`internal/domain/entity/handler.go`. `TestErrCode_Parity` enforces the
constant↔topic bijection, so the constant and the topic must go together.

### D10 — Out of scope

- `ArrayInfo` persistence (a known separate gap; the restructure gives it a home
  in `ArrayBranch`, nothing more).
- Cloud's tagged-union parity and the rest of #85.

---

## File structure

**Modified**
- `internal/domain/model/schema/node.go` — the branch types, accessors, mutators
- `internal/domain/model/schema/merge.go` — set union; `mergeKind` deleted
- `internal/domain/model/schema/codec.go` — `kinds`, payload-driven decode
- `internal/domain/model/schema/extend.go` — subset gate; `isNullOnlyLeaf` and `ErrPolymorphicSlot` deleted
- `internal/domain/model/schema/diff.go` — per-branch diff
- `internal/domain/model/schema/apply.go` — per-branch resolve; `add_kind_branch` replay
- `internal/domain/model/schema/ops.go` — `KindAddKindBranch`, `NewAddKindBranch`, corrected wire-format comment
- `internal/domain/model/schema/validate.go` — branch dispatch via accessors; `hasArrayBranch`/`matchesScalarBranch`/`declaredKindNames` collapse
- `internal/domain/model/schema/field.go` — branch-driven walk; `ConcreteTypes`/`concreteTypes` deleted
- `internal/domain/model/schema/gentree/gentree.go` — branch-driven `mutateToValue`
- `internal/domain/model/exporter/simple_view.go`, `json_schema.go` — branch accessors
- `internal/domain/entity/handler.go`, `internal/domain/model/ingest/validate.go` — sentinel removal
- `internal/common/error_codes.go` — constant removal

**Docs**
- `docs/cloud-parity/model-kind-enforcement.md`, `docs/cloud-parity/validation-failure-code.md`,
  `docs/cloud-parity/README.md`, `cmd/cyoda/help/content/models.md`,
  `cmd/cyoda/help/content/errors.md`, `cmd/cyoda/help/content/errors/*.md`,
  `api/openapi.yaml`, `CHANGELOG.md`

---

### Task 1: Branch accessors on the current struct

Introduce the target accessor surface over today's four loose fields and convert
every call site. **No behaviour change** — the accessors reproduce, exactly, the
branch semantics the five readers fixed in the previous change already use.

**Files:**
- Modify: `internal/domain/model/schema/node.go`
- Modify: `internal/domain/model/schema/apply.go`, `validate.go`, `field.go`,
  `diff.go`, `extend.go`, `codec.go`
- Modify: `internal/domain/model/exporter/simple_view.go`, `json_schema.go`
- Modify: `internal/domain/model/schema/gentree/gentree.go`
- Test: `internal/domain/model/schema/node_branch_accessor_test.go` (create)

**Interfaces produced:**

```go
type Branch interface{ Kind() NodeKind }

type ScalarBranch struct{ /* unexported */ }
func (b *ScalarBranch) Kind() NodeKind      // KindLeaf
func (b *ScalarBranch) Types() *TypeSet     // concrete types; never contains NULL

type ObjectBranch struct{ /* unexported */ }
func (b *ObjectBranch) Kind() NodeKind      // KindObject
func (b *ObjectBranch) Children() map[string]*ModelNode  // shallow copy
func (b *ObjectBranch) Child(name string) *ModelNode

type ArrayBranch struct{ /* unexported */ }
func (b *ArrayBranch) Kind() NodeKind       // KindArray
func (b *ArrayBranch) Element() *ModelNode  // may be nil
func (b *ArrayBranch) Info() *ArrayInfo

func (n *ModelNode) Scalar() *ScalarBranch
func (n *ModelNode) Object() *ObjectBranch
func (n *ModelNode) Array()  *ArrayBranch
func (n *ModelNode) Kinds()  []NodeKind   // ascending NodeKind order
func (n *ModelNode) IsPolymorphic() bool
func (n *ModelNode) Nullable() bool
func (n *ModelNode) Types() *TypeSet      // D3
func (n *ModelNode) AddScalarTypes(dts ...DataType)
func (n *ModelNode) SetNullable()
func (n *ModelNode) SetElement(e *ModelNode)
```

In this task the accessors are computed from the existing fields:

| accessor | Task-1 implementation over the current fields |
|---|---|
| `Scalar()` | non-nil iff `n.kind == KindLeaf && !isNullOnlyLeaf(n)`, or `len(concreteTypes(n.types)) > 0`; `Types()` = `concreteTypes(n.types)` |
| `Object()` | non-nil iff `n.kind == KindObject` |
| `Array()` | non-nil iff `n.kind == KindArray \|\| n.element != nil` |
| `Nullable()` | `n.types` contains `Null` |
| `Kinds()` | the kinds of the non-nil branches, ascending |

`ModelNode.Kind()` is deleted in this task, which is what forces every one of the
44 call sites to be revisited.

- [ ] **Step 1: Write the failing accessor test**

Create `internal/domain/model/schema/node_branch_accessor_test.go`:

```go
package schema

import "testing"

// The accessors name the branches a node carries. A node can carry several;
// the five union shapes below are every shape Merge produces.
func TestBranchAccessors_NameEveryBranchTheNodeCarries(t *testing.T) {
	scalarThenArray := Merge(NewLeafNode(String), NewArrayNode(NewLeafNode(String)))
	if scalarThenArray.Scalar() == nil {
		t.Error("scalar-then-array union must carry a scalar branch")
	}
	if scalarThenArray.Array() == nil {
		t.Error("scalar-then-array union must carry an array branch")
	}
	if scalarThenArray.Object() != nil {
		t.Error("scalar-then-array union must not carry an object branch")
	}
	if !scalarThenArray.IsPolymorphic() {
		t.Error("a node carrying two branches is polymorphic")
	}

	objThenArr := Merge(NewObjectNode(), NewArrayNode(NewLeafNode(Integer)))
	if objThenArr.Object() == nil || objThenArr.Array() == nil {
		t.Errorf("object-then-array union must carry both branches; kinds=%v", objThenArr.Kinds())
	}

	nullOnly := NewLeafNode(Null)
	if nullOnly.Scalar() != nil {
		t.Error("the nullable marker carries no scalar branch — NULL is not a scalar observation")
	}
	if !nullOnly.Nullable() {
		t.Error("the nullable marker is nullable")
	}
	if len(nullOnly.Kinds()) != 0 {
		t.Errorf("the nullable marker declares no kind; got %v", nullOnly.Kinds())
	}

	nullThenArr := Merge(NewLeafNode(Null), NewArrayNode(NewLeafNode(String)))
	if nullThenArr.Array() == nil || nullThenArr.Scalar() != nil || !nullThenArr.Nullable() {
		t.Errorf("null-then-array is a nullable ARRAY; kinds=%v nullable=%v scalar=%v",
			nullThenArr.Kinds(), nullThenArr.Nullable(), nullThenArr.Scalar())
	}

	plain := NewLeafNode(String)
	if plain.IsPolymorphic() {
		t.Error("a monomorphic field is a set of one")
	}
	if got := plain.Kinds(); len(got) != 1 || got[0] != KindLeaf {
		t.Errorf("Kinds() = %v, want [LEAF]", got)
	}
}

// Kinds() is ordered so callers and encoders are deterministic.
func TestKinds_AscendingOrder(t *testing.T) {
	n := Merge(Merge(NewLeafNode(String), NewObjectNode()), NewArrayNode(NewLeafNode(String)))
	got := n.Kinds()
	want := []NodeKind{KindLeaf, KindObject, KindArray}
	if len(got) != len(want) {
		t.Fatalf("Kinds() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Kinds() = %v, want %v", got, want)
		}
	}
}

// The mutators replace Types().Add, which a derived Types() cannot support.
func TestMutators(t *testing.T) {
	n := NewObjectNode()
	n.SetNullable()
	if !n.Nullable() {
		t.Error("SetNullable on a pure container records the marker")
	}

	// D2: a scalar branch suppresses the marker, exactly as TypeSet.Add drops
	// NULL when a concrete type is present.
	m := NewLeafNode(String)
	m.SetNullable()
	if m.Nullable() {
		t.Error("a node carrying a scalar branch does not record the marker separately")
	}
	if got := m.Types().Types(); len(got) != 1 || got[0] != String {
		t.Errorf("Types() = %v, want [STRING]", got)
	}

	k := NewLeafNode(Null)
	k.AddScalarTypes(String)
	if k.Scalar() == nil || k.Nullable() {
		t.Error("adding a concrete type to the nullable marker establishes the scalar branch and clears the marker")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/domain/model/schema/ -run 'TestBranchAccessors|TestKinds_Ascending|TestMutators' -v`
Expected: FAIL to compile — `Scalar`, `Object`, `Array`, `Kinds`, `IsPolymorphic`,
`Nullable`, `AddScalarTypes`, `SetNullable` undefined.

- [ ] **Step 3: Add the branch types and accessors to `node.go`**

Add the `Branch` interface, the three branch structs, and the accessors from the
table above. Keep the existing fields, `Types()` (returning `n.types` for now)
and `Children()`/`Child()`/`SetChild()`/`Element()`/`Info()` untouched in this
step. Delete `func (n *ModelNode) Kind() NodeKind`.

`AddScalarTypes` in this task operates on `n.types` (which already implements D2
via `TypeSet.Add`) and, when `n.kind` is not `KindLeaf` and only NULL is being
added, leaves `n.kind` alone. `SetNullable` is `n.types.Add(Null)`.
`SetElement(e)` sets `n.element = e` and creates `n.info` when absent.

- [ ] **Step 4: Run the accessor test — expect PASS**

Run: `go test ./internal/domain/model/schema/ -run 'TestBranchAccessors|TestKinds_Ascending|TestMutators' -v`

- [ ] **Step 5: Convert every `.Kind()` call site**

Run `grep -rn '\.Kind()' --include='*.go' .` and convert each. The mechanical
rules — each is behaviour-identical to what the site does today:

| today | becomes |
|---|---|
| `n.Kind() == KindLeaf` (leaf-only fast path in `validateNode`, `field.go`, exporters, `diffNode`) | `n.Object() == nil && n.Array() == nil` |
| `n.Kind() == KindObject` | `n.Object() != nil` |
| `n.Kind() == KindArray \|\| n.Element() != nil` / `hasArrayBranch(n)` | `n.Array() != nil` |
| `switch node.Kind()` in `diffNode`, `gentree.mutateToValue` | branch-by-branch: handle `Scalar()`, `Object()`, `Array()` in turn |
| `existing.Kind() != incoming.Kind()` in `extend.go` | leave alone for now — Task 4 replaces it; use a local `dominantKind(n)` helper marked `// removed in the subset-gate change` |
| `%s` of a kind in an error message | `kindNames(n)` — `strings.Join` of `Kinds()` names, or `"no kind"` |

`validate.go`'s `hasArrayBranch`, `matchesScalarBranch` and `declaredKindNames`
become one-liners over the accessors; keep them as named helpers for now (Task 4
finishes them).

`apply.go`: `target.Types().Add(dt)` → `target.AddScalarTypes(dt)`;
`target.element = elem` → `target.SetElement(elem)`;
`elem.Types().Add(dt)` → `elem.AddScalarTypes(dt)`.

`codec.go`: `n.element = elem` → `n.SetElement(elem)`; `n.types.Add(dt)` →
`n.AddScalarTypes(dt)`. Leave `w.Kind = n.kind.String()` — Task 3 replaces it.

`field.go` and `merge.go` still touch the unexported fields directly; that is
fine, they live in the package and Task 2 rewrites them.

- [ ] **Step 6: Run the full model suite — expect PASS, no test edits**

Run: `go build ./... && go vet ./... && go test ./internal/domain/model/... ./internal/domain/entity/... ./e2e/parity/...`
Expected: PASS. A test that only fails to *compile* because it called
`node.Kind()` may be converted with the same table; a test whose **assertions**
change means the conversion was not behaviour-preserving — stop and fix the
conversion, not the test.

- [ ] **Step 7: Commit**

```bash
git add -A internal/domain/model internal/domain/entity
git commit -m "refactor(model): name a node's branches instead of one dominant kind

Introduce Scalar()/Object()/Array()/Kinds()/Nullable() over the existing
fields and convert every reader. ModelNode.Kind() is gone: the label could
only ever name one of three independent payload slots, so every reader that
dispatched on it lost a branch. No behaviour change."
```

---

### Task 2: Swap the internals to the branch set

Replace the four loose fields with `branches` + `nullable`. The accessors keep
their signatures. `mergeKind`, `ConcreteTypes`, `concreteTypes` and
`isNullOnlyLeaf` are deleted. **No behaviour change.**

**Files:**
- Modify: `internal/domain/model/schema/node.go`, `merge.go`, `field.go`,
  `codec.go`, `extend.go`, `validate.go`
- Modify: `internal/domain/model/exporter/simple_view.go`, `json_schema.go` (the
  three `schema.ConcreteTypes` call sites)
- Test: `internal/domain/model/schema/node_branch_set_test.go` (create)

**Interfaces consumed:** the Task 1 accessor surface. **Produces:** the same
surface, now backed by the set.

- [ ] **Step 1: Write the failing test**

Create `internal/domain/model/schema/node_branch_set_test.go`:

```go
package schema

import "testing"

// Set union is commutative by construction — mergeKind's precedence achieved
// that only by accident, and its OBJECT+ARRAY tiebreak lost the array branch
// from the label entirely.
func TestMerge_BranchUnionIsCommutative(t *testing.T) {
	kinds := func(n *ModelNode) string {
		s := ""
		for _, k := range n.Kinds() {
			s += k.String() + ","
		}
		return s
	}
	build := []struct {
		name string
		a, b func() *ModelNode
	}{
		{"scalar+array", func() *ModelNode { return NewLeafNode(String) }, func() *ModelNode { return NewArrayNode(NewLeafNode(String)) }},
		{"scalar+object", func() *ModelNode { return NewLeafNode(String) }, func() *ModelNode { return NewObjectNode() }},
		{"object+array", func() *ModelNode { return NewObjectNode() }, func() *ModelNode { return NewArrayNode(NewLeafNode(Integer)) }},
		{"null+array", func() *ModelNode { return NewLeafNode(Null) }, func() *ModelNode { return NewArrayNode(NewLeafNode(String)) }},
	}
	for _, c := range build {
		t.Run(c.name, func(t *testing.T) {
			ab := kinds(Merge(c.a(), c.b()))
			ba := kinds(Merge(c.b(), c.a()))
			if ab != ba {
				t.Errorf("Merge is not commutative on branch sets: a+b=%s b+a=%s", ab, ba)
			}
		})
	}
}

// A branch can be present and empty. Nothing in the payload distinguishes an
// empty object node from any other empty node — only the branch record does.
func TestBranchSet_KeepsAnEmptyBranch(t *testing.T) {
	n := NewObjectNode()
	if n.Object() == nil {
		t.Fatal("an object node with no children still declares the object branch")
	}
	if n.IsPolymorphic() {
		t.Error("one branch is not polymorphic")
	}
}

// Merging two leaves must not manufacture an object branch. Merge used to build
// its result from NewObjectNode(), which left every merged node holding an
// empty children map.
func TestMerge_TwoLeavesDeclareOnlyTheScalarBranch(t *testing.T) {
	n := Merge(NewLeafNode(String), NewLeafNode(Integer))
	if n.Object() != nil || n.Array() != nil {
		t.Errorf("leaf+leaf declares only the scalar branch; kinds=%v", n.Kinds())
	}
	if n.Scalar() == nil {
		t.Fatal("leaf+leaf declares the scalar branch")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/domain/model/schema/ -run 'TestMerge_BranchUnion|TestBranchSet_KeepsAnEmpty|TestMerge_TwoLeaves' -v`
Expected: `TestMerge_TwoLeaves…` FAILS (`Object() != nil`, because `Merge` builds
from `NewObjectNode()` and `Object()` is `kind == KindObject`… verify the actual
failure and record it; if it passes under the Task-1 accessors, keep the test —
it pins the invariant through the swap).

- [ ] **Step 3: Rewrite `node.go` on the branch set**

```go
type ModelNode struct {
	branches   map[NodeKind]Branch
	nullable   bool
	fieldCache atomic.Pointer[cachedFields]
}
```

- `NewObjectNode()` → one `ObjectBranch` with an empty children map.
- `NewLeafNode(dt)` → `dt == Null` ? `{branches: {}, nullable: true}` :
  one `ScalarBranch` seeded with `dt`.
- `NewArrayNode(e)` → one `ArrayBranch{element: e, info: NewArrayInfo()}`.
- `Children()`, `Child()`, `SetChild()`, `Element()`, `Info()` delegate to the
  object/array branch; `SetChild` creates the object branch when absent,
  `SetElement` the array branch.
- `Types()` per D3. `AddScalarTypes` / `SetNullable` per D2 and D4.

- [ ] **Step 4: Rewrite `Merge` as set union**

```go
func Merge(a, b *ModelNode) *ModelNode {
	// nil handling unchanged (Ownership Rule 7)
	result := &ModelNode{branches: make(map[NodeKind]Branch)}
	for _, k := range append(a.Kinds(), b.Kinds()...) {
		result.branches[k] = mergeBranch(result.branches[k], branchOf(a, k), branchOf(b, k))
	}
	result.nullable = a.nullable || b.nullable
	result.normalizeNullable() // D2
	return result
}
```

`mergeBranch` unions scalar TypeSets, merges children maps recursively via
`Merge`, and merges array elements via `Merge` plus `mergeArrayInfo` (which is
kept, unchanged). Delete `mergeKind` and its comment.

- [ ] **Step 5: Rewrite `field.go`'s `collectFields` on the branches**

Walk `Scalar()`, then `Object()`, then `Array()` — the same order and the same
descriptors it emits today. The branchless-nullable node emits
`FieldDescriptor{Types: [NULL]}`, which is what a `LEAF[NULL]` emits today, so
search's declared-type lookup is unchanged. Delete `ConcreteTypes` and
`concreteTypes`; the exporters' three call sites become
`node.Scalar()` / `node.Scalar().Types().Types()`.

- [ ] **Step 6: Point `codec.go`, `extend.go`, `validate.go` at the accessors**

`codec.go`'s `toWire`/`fromWire` still read/write `kind` in this task — only the
field access changes (`n.kind` → the branch set, via a temporary
`dominantKind(n)` that returns the single branch's kind, `KindObject` for a
multi-branch node, and `KindLeaf` for a branchless one — i.e. exactly
`mergeKind`'s outcome). Task 3 deletes it. Delete `isNullOnlyLeaf` and replace
its two uses in `extend.go` with `n.Nullable() && len(n.Kinds()) == 0`.

- [ ] **Step 7: Run the whole model suite plus the parity oracle**

Run: `go build ./... && go vet ./... && go test ./internal/domain/model/... ./internal/domain/entity/... ./e2e/parity/... ./internal/domain/search/...`
Expected: PASS with **no changes to existing test assertions**. `TestRoundtripRandomSeeds`
(1000 seeds), the commutativity/idempotence/monotonicity/permutation property
suites and `TestCodec_RoundTripPreservesEveryBranch` are the oracle for "no
behaviour change" — if one of them fails, the swap is wrong.

- [ ] **Step 8: Commit**

```bash
git add -A internal/domain/model
git commit -m "refactor(model): a node holds the set of branches it was observed as

branches+nullable replace kind/types/children/element. mergeKind's arbitrary
OBJECT-wins tiebreak, the dominant kind it produced, ConcreteTypes and
isNullOnlyLeaf are deleted: sets union, and nullability is a flag. No
behaviour change."
```

---

### Task 3: Teach the codec both spellings

**Files:**
- Modify: `internal/domain/model/schema/codec.go`
- Test: `internal/domain/model/schema/codec_kinds_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/domain/model/schema/codec_kinds_test.go`:

```go
package schema

import "testing"

// Byte-identity for every monomorphic node, nullable or not: this is nearly
// every node in every existing model, and there is no migration.
func TestCodec_MonomorphicNodesSerialiseAsBefore(t *testing.T) {
	cases := []struct {
		name string
		node *ModelNode
		want string
	}{
		{"leaf", NewLeafNode(String), `{"kind":"LEAF","types":["STRING"]}`},
		{"nullable marker", NewLeafNode(Null), `{"kind":"LEAF","types":["NULL"]}`},
		{"empty object", NewObjectNode(), `{"kind":"OBJECT"}`},
		{"array of string", NewArrayNode(NewLeafNode(String)),
			`{"kind":"ARRAY","element":{"kind":"LEAF","types":["STRING"]}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Marshal(c.node)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("Marshal = %s, want %s", got, c.want)
			}
		})
	}
}

func TestCodec_NullableContainerKeepsTheMarkerSpelling(t *testing.T) {
	n := Merge(NewObjectNode(), NewLeafNode(Null))
	got, err := Marshal(n)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(got) != `{"kind":"OBJECT","types":["NULL"]}` {
		t.Errorf("Marshal = %s, want the unchanged nullable-object spelling", got)
	}
}

// A union spells its whole set.
func TestCodec_UnionSpellsEveryKind(t *testing.T) {
	n := Merge(NewObjectNode(), NewArrayNode(NewLeafNode(Integer)))
	raw, err := Marshal(n)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := Unmarshal(raw)
	if err != nil {
		t.Fatalf("Unmarshal(%s): %v", raw, err)
	}
	if back.Object() == nil || back.Array() == nil {
		t.Errorf("round trip lost a branch: %s -> kinds=%v", raw, back.Kinds())
	}
	if len(back.Kinds()) != 2 {
		t.Errorf("kinds = %v, want exactly OBJECT and ARRAY", back.Kinds())
	}
}

// A model persisted under the old dominant-kind spelling restores every branch
// its payload carries. No migration.
func TestCodec_ReadsTheOldDominantKindSpelling(t *testing.T) {
	const legacy = `{"kind":"OBJECT","types":["STRING"],` +
		`"children":{"k":{"kind":"LEAF","types":["INTEGER"]}},` +
		`"element":{"kind":"LEAF","types":["BOOLEAN"]}}`
	n, err := Unmarshal([]byte(legacy))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if n.Scalar() == nil || n.Object() == nil || n.Array() == nil {
		t.Fatalf("legacy node must restore all three branches; kinds=%v", n.Kinds())
	}
}

// Every empty shape the wire form can carry round-trips to itself.
func TestCodec_EmptyBranchesRoundTrip(t *testing.T) {
	for _, raw := range []string{
		`{"kind":"OBJECT"}`,
		`{"kind":"ARRAY"}`,
		`{"kind":"LEAF"}`,
		`{"kind":"LEAF","types":["NULL"]}`,
	} {
		n, err := Unmarshal([]byte(raw))
		if err != nil {
			t.Fatalf("Unmarshal(%s): %v", raw, err)
		}
		out, err := Marshal(n)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(out) != raw {
			t.Errorf("round trip %s -> %s", raw, out)
		}
	}
}

// The codec fails closed on a node that names no kind and carries no payload.
func TestCodec_RejectsAKindlessNode(t *testing.T) {
	if _, err := Unmarshal([]byte(`{}`)); err == nil {
		t.Error("a node with neither a kind nor a payload must be rejected")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/domain/model/schema/ -run TestCodec_ -v`
Expected: the union and legacy cases FAIL.

- [ ] **Step 3: Implement D5 in `codec.go`**

Add `Kinds []string` to `wireNode`, make `Kind` `omitempty`, implement the
encode and decode rules from D5, and delete the temporary `dominantKind` helper
from Task 2. Keep `fromWire`'s existing doc comment intent — it is now the rule,
not a workaround.

- [ ] **Step 4: Run the codec tests and the full model suite**

Run: `go test ./internal/domain/model/... ./e2e/parity/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add -A internal/domain/model/schema
git commit -m "feat(model): the persisted node spells its whole kind set

wireNode gains \"kinds\"; \"kind\" is still written for a node with at most one
branch, so every monomorphic node — nullable or not — serialises exactly as
before and there is no migration. Decoding is payload-driven as well as
kind-driven, so a model stored under the old dominant-kind spelling restores
every branch it carries."
```

---

### Task 4: The gate is a subset test, and Diff is per-branch

Ends limitation 2 (a write matching a *declared* branch is refused when it is
not the dominant one) and makes `diffObject`/`diffArray` see the branches they
never looked at.

**Files:**
- Modify: `internal/domain/model/schema/extend.go`, `diff.go`
- Test: `internal/domain/model/schema/extend_branch_subset_test.go` (create)
- Test: `internal/domain/model/schema/diff_branch_test.go` (create)

- [ ] **Step 1: Write the failing tests**

Create `internal/domain/model/schema/extend_branch_subset_test.go`:

```go
package schema

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// A model with a multi-kind field was unusable with a changeLevel: half its own
// declared data was refused, with a message telling the client to send the
// declared kind — which is what the client sent.
func TestExtend_AcceptsEveryDeclaredBranch(t *testing.T) {
	levels := []spi.ChangeLevel{
		spi.ChangeLevelArrayLength,
		spi.ChangeLevelArrayElements,
		spi.ChangeLevelType,
		spi.ChangeLevelStructural,
	}
	cases := []struct {
		name     string
		declared func() *ModelNode
		incoming func() *ModelNode
	}{
		{
			"array-and-scalar union, scalar write",
			func() *ModelNode { return Merge(NewArrayNode(NewLeafNode(String)), NewLeafNode(String)) },
			func() *ModelNode { return NewLeafNode(String) },
		},
		{
			"object-and-scalar union, scalar write",
			func() *ModelNode { return Merge(NewObjectNode(), NewLeafNode(String)) },
			func() *ModelNode { return NewLeafNode(String) },
		},
		{
			"object-and-array union, array write",
			func() *ModelNode { return Merge(NewObjectNode(), NewArrayNode(NewLeafNode(Integer))) },
			func() *ModelNode { return NewArrayNode(NewLeafNode(Integer)) },
		},
		{
			"object-and-array union, object write",
			func() *ModelNode { return Merge(NewObjectNode(), NewArrayNode(NewLeafNode(Integer))) },
			func() *ModelNode { return NewObjectNode() },
		},
	}
	for _, c := range cases {
		for _, level := range levels {
			t.Run(c.name+"/"+string(level), func(t *testing.T) {
				existing := NewObjectNode()
				existing.SetChild("f", c.declared())
				incoming := NewObjectNode()
				incoming.SetChild("f", c.incoming())

				if _, err := Extend(existing, incoming, level); err != nil {
					t.Fatalf("a write matching a declared branch must be accepted at %q; got: %v", level, err)
				}
			})
		}
	}
}

// Adding a branch is a STRUCTURAL change — a new branch is strictly more
// fundamental than a new field — and raising the level does resolve it.
func TestExtend_AddingABranchRequiresStructural(t *testing.T) {
	cases := []struct {
		name     string
		existing func() *ModelNode
		incoming func() *ModelNode
	}{
		{"scalar gains object", func() *ModelNode { return NewLeafNode(Integer) }, func() *ModelNode { return NewObjectNode() }},
		{"scalar gains array", func() *ModelNode { return NewLeafNode(Integer) }, func() *ModelNode { return NewArrayNode(NewLeafNode(Integer)) }},
		{"object gains scalar", func() *ModelNode { return NewObjectNode() }, func() *ModelNode { return NewLeafNode(Integer) }},
		{"object gains array", func() *ModelNode { return NewObjectNode() }, func() *ModelNode { return NewArrayNode(NewLeafNode(Integer)) }},
		{"array gains scalar", func() *ModelNode { return NewArrayNode(NewLeafNode(Integer)) }, func() *ModelNode { return NewLeafNode(Integer) }},
		{"array gains object", func() *ModelNode { return NewArrayNode(NewLeafNode(Integer)) }, func() *ModelNode { return NewObjectNode() }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			build := func() (*ModelNode, *ModelNode) {
				e := NewObjectNode()
				e.SetChild("f", c.existing())
				i := NewObjectNode()
				i.SetChild("f", c.incoming())
				return e, i
			}

			e, i := build()
			if _, err := Extend(e, i, spi.ChangeLevelType); err == nil {
				t.Error("adding a branch below STRUCTURAL must be refused")
			}

			e, i = build()
			got, err := Extend(e, i, spi.ChangeLevelStructural)
			if err != nil {
				t.Fatalf("adding a branch at STRUCTURAL must be accepted; got: %v", err)
			}
			if !got.Child("f").IsPolymorphic() {
				t.Errorf("the extended field declares both kinds; kinds=%v", got.Child("f").Kinds())
			}
		})
	}
}

// D2/D8-2: writing null to a declared scalar proposes no schema change, so no
// level gates it.
func TestExtend_NullAgainstAScalarProposesNothing(t *testing.T) {
	for _, level := range []spi.ChangeLevel{spi.ChangeLevel(""), spi.ChangeLevelArrayLength} {
		existing := NewObjectNode()
		existing.SetChild("s", NewLeafNode(String))
		incoming := NewObjectNode()
		incoming.SetChild("s", NewLeafNode(Null))

		if _, err := Extend(existing, incoming, level); err != nil {
			t.Errorf("null against a declared scalar must be accepted at %q; got: %v", level, err)
		}
	}
}
```

Create `internal/domain/model/schema/diff_branch_test.go`:

```go
package schema

import "testing"

// diffObject walked children and returned — it never looked at the array
// branch. Harmless only while the gate refused every write that could reach it.
func TestDiff_SeesEveryBranchOfAUnion(t *testing.T) {
	old := NewObjectNode()
	old.SetChild("f", Merge(NewObjectNode(), NewArrayNode(NewLeafNode(Integer))))

	widened := Merge(NewObjectNode(), NewArrayNode(NewLeafNode(String)))
	next := NewObjectNode()
	next.SetChild("f", Merge(old.Child("f"), widened))

	delta, err := Diff(old, next)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if delta == nil {
		t.Fatal("widening the array branch of a union is a change; Diff returned a no-op")
	}
	applied, err := Apply(old, delta)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	before, _ := Marshal(next)
	after, _ := Marshal(applied)
	if string(before) != string(after) {
		t.Errorf("Apply(old, Diff(old,new)) != new\n  new     = %s\n  applied = %s", before, after)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/domain/model/schema/ -run 'TestExtend_Accepts|TestExtend_AddingABranch|TestExtend_NullAgainst|TestDiff_SeesEvery' -v`
Expected: FAIL — `ErrPolymorphicSlot` on the union cases, `kind change … (not additive)` on the Diff case.

- [ ] **Step 3: Rewrite `checkAndExtend` as a subset test**

```go
func checkAndExtend(existing, incoming *ModelNode, level spi.ChangeLevel, path string, scalarLevel spi.ChangeLevel) (bool, error)
```

`scalarLevel` is `spi.ChangeLevelType` normally and `spi.ChangeLevelArrayElements`
when `existing`/`incoming` are directly an array's element — that is the only
thing `checkElementWidening` did differently, so `checkElementWidening` is
deleted and its array-of-array recursion becomes a recursive call that keeps
`scalarLevel`, while descent into an object element's children resets it to
`ChangeLevelType`. Body:

1. **Nullability.** `incoming.Nullable() && !existing.Nullable() && existing.Scalar() == nil`
   → require `scalarLevel`, `changed = true`. Message unchanged:
   `"nullable marker at %s requires TYPE level, but level is %q"` (and the
   ARRAY_ELEMENTS wording on an element).
2. **Branches.** For each `k` in `incoming.Kinds()`:
   - `existing` has it → recurse into that branch (scalar: TypeSet difference →
     `scalarLevel`; object: new child → `STRUCTURAL`, existing child →
     `checkAndExtend(..., ChangeLevelType)`; array: element →
     `checkAndExtend(..., ChangeLevelArrayElements)` and width → `ChangeLevelArrayLength`).
   - `existing` has none at all (branchless marker) → require `scalarLevel` (D6).
   - otherwise → require `spi.ChangeLevelStructural`, message
     `"new %s branch at %s requires STRUCTURAL level, but level is %q"`.

Delete `ErrPolymorphicSlot`, its doc block and its `TODO(#85)`; Task 6 removes
the callers. Delete `isNullOnlyLeaf` if Task 2 left it.

- [ ] **Step 4: Make `Diff` per-branch**

`diffNode` stops switching on a kind:

```go
func diffNode(path string, oldN, newN *ModelNode, ops *[]SchemaOp) error {
	if newN.Nullable() && !oldN.Nullable() { /* broaden_type ["NULL"] */ }
	// scalar branch: D7 rows 2 and 3
	// object branch: diffObject, or add_kind_branch when old has none
	// array branch:  diffArray,  or add_kind_branch when old has none
	// a branch in old but not in new: "kind removal at %q is not additive"
}
```

`add_kind_branch` does not exist yet — in this task, emit an explicit
`fmt.Errorf("adding a %s branch at %q is not yet expressible", …)` for those
arms so the task stays honest, and Task 5 replaces the error with the op.
`TestExtend_AddingABranchRequiresStructural` therefore asserts `Extend` only;
`TestDiff_SeesEveryBranchOfAUnion` exercises the both-present path and must pass now.

- [ ] **Step 5: Run the model suite**

Run: `go test ./internal/domain/model/... ./e2e/parity/...`
Expected: the new tests PASS. Existing tests that assert `ErrPolymorphicSlot`
(`extend_kindmismatch_test.go`, `extend_polymorphic_error_test.go`,
`extend_array_element_test.go`, `extend_nullable_test.go:143`) now fail to
compile — **rewrite them in this step** to assert the new contract: rejected
below `STRUCTURAL` as a level violation, accepted at `STRUCTURAL`. Do not delete
a case; convert it.

- [ ] **Step 6: Commit**

```bash
git add -A internal/domain/model/schema
git commit -m "fix(model): a write matching a declared branch is accepted

The extension gate compared one kind per path, so a model with a multi-kind
field refused half its own declared data at every changeLevel — with a message
telling the client to send the declared kind, which is what it sent. The gate
is now a subset test over branch sets and Diff walks each branch, so
diffObject no longer skips an array branch it would have merged and then
silently not persisted."
```

---

### Task 5: `add_kind_branch`

Ends limitation 1: an entity write can create a multi-kind declaration.

**Files:**
- Modify: `internal/domain/model/schema/ops.go`, `diff.go`, `apply.go`
- Test: `internal/domain/model/schema/add_kind_branch_test.go` (create)
- Modify: `internal/domain/model/schema/axis2_kind_matrix_test.go` (un-skip)

**Interfaces produced:**

```go
const KindAddKindBranch SchemaOpKind = "add_kind_branch"
func NewAddKindBranch(targetPath string, branch []byte) SchemaOp
```

`Payload` is `schema.Marshal` of a node carrying **exactly one** branch. `Path`
targets the node. No `Name` — the payload names the kind, and storing it twice
is the derived-and-stored duplication that produced this defect class.

- [ ] **Step 1: Write the failing test**

Create `internal/domain/model/schema/add_kind_branch_test.go`:

```go
package schema

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// An entity write can now create a multi-kind declaration; only sample-data
// import could before.
func TestExtendDiffApply_AddsABranchEndToEnd(t *testing.T) {
	cases := []struct {
		name     string
		existing func() *ModelNode
		incoming func() *ModelNode
	}{
		{"scalar gains array", func() *ModelNode { return NewLeafNode(String) }, func() *ModelNode { return NewArrayNode(NewLeafNode(String)) }},
		{"scalar gains object", func() *ModelNode { return NewLeafNode(String) }, func() *ModelNode { return NewObjectNode() }},
		{"object gains array", func() *ModelNode { return NewObjectNode() }, func() *ModelNode { return NewArrayNode(NewLeafNode(Integer)) }},
		{"array gains object", func() *ModelNode { return NewArrayNode(NewLeafNode(Integer)) }, func() *ModelNode { return NewObjectNode() }},
		{"array gains scalar", func() *ModelNode { return NewArrayNode(NewLeafNode(Integer)) }, func() *ModelNode { return NewLeafNode(Integer) }},
		{"object gains scalar", func() *ModelNode { return NewObjectNode() }, func() *ModelNode { return NewLeafNode(Integer) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := NewObjectNode()
			old.SetChild("f", c.existing())
			incoming := NewObjectNode()
			incoming.SetChild("f", c.incoming())

			extended, err := Extend(old, incoming, spi.ChangeLevelStructural)
			if err != nil {
				t.Fatalf("Extend: %v", err)
			}
			delta, err := Diff(old, extended)
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			if delta == nil {
				t.Fatal("adding a branch is a change; Diff returned a no-op")
			}
			applied, err := Apply(old, delta)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			want, _ := Marshal(extended)
			got, _ := Marshal(applied)
			if string(want) != string(got) {
				t.Errorf("Apply(old, Diff(old,new)) != new\n  new     = %s\n  applied = %s", want, got)
			}
		})
	}
}

// The nullable marker gaining a container branch is the case Extend accepted
// and Diff then failed on, surfacing as a 500 from the ingest path.
func TestExtendDiffApply_NullableMarkerGainsAContainer(t *testing.T) {
	for _, c := range []struct {
		name     string
		incoming func() *ModelNode
	}{
		{"object", func() *ModelNode { return NewObjectNode() }},
		{"array", func() *ModelNode { return NewArrayNode(NewLeafNode(String)) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			old := NewObjectNode()
			old.SetChild("f", NewLeafNode(Null))
			incoming := NewObjectNode()
			incoming.SetChild("f", c.incoming())

			extended, err := Extend(old, incoming, spi.ChangeLevelType)
			if err != nil {
				t.Fatalf("promoting the nullable marker keeps the TYPE-level contract; got: %v", err)
			}
			delta, err := Diff(old, extended)
			if err != nil {
				t.Fatalf("Diff must express the promotion; got: %v", err)
			}
			applied, err := Apply(old, delta)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			want, _ := Marshal(extended)
			got, _ := Marshal(applied)
			if string(want) != string(got) {
				t.Errorf("Apply(old, Diff(old,new)) != new\n  new     = %s\n  applied = %s", want, got)
			}
		})
	}
}

// Replay is a union, so it is commutative and idempotent like every other op.
func TestApply_AddKindBranchIsIdempotent(t *testing.T) {
	old := NewObjectNode()
	old.SetChild("f", NewLeafNode(String))
	incoming := NewObjectNode()
	incoming.SetChild("f", NewArrayNode(NewLeafNode(String)))

	extended, err := Extend(old, incoming, spi.ChangeLevelStructural)
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}
	delta, err := Diff(old, extended)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	once, err := Apply(old, delta)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	twice, err := Apply(once, delta)
	if err != nil {
		t.Fatalf("Apply twice: %v", err)
	}
	a, _ := Marshal(once)
	b, _ := Marshal(twice)
	if string(a) != string(b) {
		t.Errorf("replaying add_kind_branch twice changed the model\n  once  = %s\n  twice = %s", a, b)
	}
}

// Apply fails closed on a payload that does not carry exactly one branch.
func TestApply_AddKindBranchRejectsAMultiBranchPayload(t *testing.T) {
	base := NewObjectNode()
	base.SetChild("f", NewLeafNode(String))

	multi, err := Marshal(Merge(NewObjectNode(), NewArrayNode(NewLeafNode(String))))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	delta, err := MarshalDelta([]SchemaOp{NewAddKindBranch("f", multi)})
	if err != nil {
		t.Fatalf("MarshalDelta: %v", err)
	}
	if _, err := Apply(base, delta); err == nil {
		t.Error("add_kind_branch carries exactly one branch; a multi-branch payload must be rejected")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/domain/model/schema/ -run 'TestExtendDiffApply_|TestApply_AddKindBranch' -v`
Expected: FAIL — `NewAddKindBranch` undefined; the Diff arms still return the
Task-4 "not yet expressible" error.

- [ ] **Step 3: Add the op to `ops.go`**

Add `KindAddKindBranch` with a doc comment stating: Path targets the node,
Payload is one encoded branch (same encoding as `schema.Marshal`), ChangeLevel
`STRUCTURAL` — except the promotion of a node that declares no kind at all (the
nullable marker), which keeps the `TYPE` / `ARRAY_ELEMENTS` contract. Add
`NewAddKindBranch`.

While in the file, **correct the misleading `SchemaOpKind` wire-format note**:
the kind strings are cyoda-go's vocabulary in a plugin-owned table. Plugins keep
deltas opaque and never parse op kinds, so there is nothing to coordinate with a
plugin migration story; `Apply` already fails closed on an unknown kind.

- [ ] **Step 4: Emit it from `Diff` and replay it in `Apply`**

`diff.go`: replace the Task-4 "not yet expressible" errors with
`NewAddKindBranch(path, encoded)` where `encoded` is `Marshal` of a node holding
only the new branch. Also replace `diffArray`'s
`array element materialization … requires LEAF element` error (and its
`TODO(A.3 / issue #85)`) with an `add_kind_branch` on the element path — the
same op expresses it.

`apply.go`: add `applyAddKindBranch` — resolve the path, `Unmarshal` the
payload, require exactly one branch, and merge it into the target's branch set
(`Merge` when the branch is already present, so replay is idempotent). Update
`resolvePath` to ask for the branch it needs (`[]` → `cur.Array()`, a name →
`cur.Object()`) rather than comparing a kind, and name the node's whole kind set
in the error.

- [ ] **Step 5: Un-skip the axis-2 kind-conflict cells**

In `internal/domain/model/schema/axis2_kind_matrix_test.go`, change the six
`"skip"` cells (`LO_leaf_to_object`, `LA_leaf_to_array`, `OL_object_to_leaf`,
`OA_object_to_array`, `AL_array_to_leaf`, `AO_array_to_object`) to
`"roundtrip"` and delete `polymorphicSlotIssue`. These are the matrix's own
statement that limitation 1 is gone.

- [ ] **Step 6: Run the model suite and the property suites**

Run: `go test ./internal/domain/model/... ./e2e/parity/... -v`
Expected: PASS, including `TestRoundtripRandomSeeds`, `TestCommutativity_ByKindPairAndPathRelationship`,
`TestValidationMonotonicity`, `TestDiffCoversCatalog` and `TestAxis2KindMatrix`.

- [ ] **Step 7: Commit**

```bash
git add -A internal/domain/model/schema
git commit -m "feat(model): an entity write can create a multi-kind declaration

add_kind_branch records a branch being added to a node, at STRUCTURAL — except
promoting a node that declares no kind at all, which keeps the existing
nullable-marker contract. Replay is a branch union, so it is commutative and
idempotent like every other op. This also expresses the LEAF[NULL] -> container
widening that Extend accepted and Diff then rejected, which reached the client
as a 500."
```

---

### Task 6: Decommission `POLYMORPHIC_SLOT`

Per D9. No raise site remains; the code's meaning ("raising `changeLevel` does
not help") is now false.

**Files:**
- Modify: `internal/domain/entity/handler.go` (classification arm + the comment at :225)
- Modify: `internal/domain/model/ingest/validate.go:97-103`
- Modify: `internal/common/error_codes.go:34`
- Delete: `cmd/cyoda/help/content/errors/POLYMORPHIC_SLOT.md`
- Modify: `cmd/cyoda/help/content/errors.md:98`,
  `cmd/cyoda/help/content/errors/INCOMPATIBLE_TYPE.md`,
  `cmd/cyoda/help/content/errors/VALIDATION_FAILED.md`
- Modify: `api/openapi.yaml:3415`
- Modify: `internal/e2e/zzz_errorcode_matrix_test.go:96`
- Delete: `internal/domain/entity/handler_polymorphic_test.go` (its subject is gone;
  keep and adapt any case in it that tests a *different* classification arm)

- [ ] **Step 1: Write the failing test**

Add to `internal/domain/model/schema/extend_branch_subset_test.go`:

```go
// The extension path has no answer left that raising the level does not
// resolve, so there is no polymorphic-slot sentinel to wrap.
func TestExtend_BelowStructuralIsAPlainLevelViolation(t *testing.T) {
	existing := NewObjectNode()
	existing.SetChild("f", NewArrayNode(NewLeafNode(String)))
	incoming := NewObjectNode()
	incoming.SetChild("f", NewLeafNode(String))

	_, err := Extend(existing, incoming, spi.ChangeLevelType)
	if err == nil {
		t.Fatal("adding a branch below STRUCTURAL must be refused")
	}
	if !strings.Contains(err.Error(), "STRUCTURAL") {
		t.Errorf("the message must name the level that resolves it; got: %v", err)
	}
}
```

(add `"strings"` to the imports)

- [ ] **Step 2: Run it**

Run: `go test ./internal/domain/model/schema/ -run TestExtend_BelowStructural -v`
Expected: PASS already if Task 4 landed — this test pins the replacement
contract before the removal, so run it and record the result.

- [ ] **Step 3: Remove the sentinel and the code**

Delete `common.ErrCodePolymorphicSlot`, the `errors.Is(err, schema.ErrPolymorphicSlot)`
arms in `handler.go` and `ingest/validate.go` (the latter's remaining branch is
the plain `change level violation: %w` wrap), the help topic file, and each doc
reference listed above. In `api/openapi.yaml`, drop the `POLYMORPHIC_SLOT:` clause
from the 400 description; leave the rest of the sentence intact.

- [ ] **Step 4: Verify the error-code parity and the e2e matrix**

Run: `go test ./cmd/cyoda/... -run 'TestErrCode' -v && go test ./internal/domain/... && go build ./...`
Expected: PASS. `TestErrCode_Parity` enforces the constant↔topic bijection, so a
leftover on either side fails here.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "fix(model)!: retire POLYMORPHIC_SLOT

The code meant \"raising changeLevel will not help\". Adding a kind branch is
now a STRUCTURAL change, so raising the level is exactly what resolves it and
the extension path has no rejection left that the sentinel described. The
constant, the help topic and the OpenAPI clause go with it."
```

---

### Task 7: Coverage on a running backend and across backends

Per `.claude/rules/test-coverage.md`. The behaviour reaching the client changed,
so `internal/e2e` and `e2e/parity` must say so.

**Files:**
- Modify: `internal/e2e/model_kind_enforcement_test.go`
- Create: `internal/e2e/model_kind_branch_extension_test.go`
- Modify: `e2e/parity/registry.go` + a new scenario file in `e2e/parity/`

- [ ] **Step 1: Write the failing e2e test**

Create `internal/e2e/model_kind_branch_extension_test.go` following the shape of
`model_kind_enforcement_test.go` (read it first — it has the model registration
and request helpers). Cover, on real Postgres, over HTTP:

1. `POST /api/entity/JSON/{name}/{version}` with `changeLevel=STRUCTURAL` where
   the payload gives a declared scalar field an array value → **200**, and the
   subsequent `SIMPLE_VIEW` export names **both** branches (`".f"` and `".f[*]"`).
2. The same write with `changeLevel=TYPE` → **400 VALIDATION_FAILED**, message
   naming `STRUCTURAL`.
3. On a model whose field declares both a scalar and an array (established by a
   sample-data collection import), a write of **either** kind with
   `changeLevel=TYPE` → **200** (limitation 2).
4. A write of a *third* kind (object) against that union with
   `changeLevel=TYPE` → **400**; at `STRUCTURAL` → **200**.
5. A field declared only by a `null` observation, then written as an object with
   `changeLevel=TYPE` → **200** (the former 500).
6. `changeLevel` unset: a write of a kind outside the declared set → **400
   VALIDATION_FAILED** (unchanged — the strict door is untouched).

In `model_kind_enforcement_test.go`, the case at :146 asserting the body carries
`POLYMORPHIC_SLOT` must assert `VALIDATION_FAILED` and the `STRUCTURAL` hint
instead.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/e2e/ -run 'ModelKindBranch|ModelKindEnforcement' -v`
Expected: FAIL before the handler/doc wiring is right; PASS once it is. Docker
must be running.

- [ ] **Step 3: Add the cross-backend parity scenario**

Backend-agnostic behaviour (it all sits above the SPI), so add one scenario —
`ModelKindBranchExtension` — asserting item 1 and item 3 above, and register it
in `e2e/parity/registry.go` beside `ModelKindEnforcementRejected`. Read
`e2e/parity/registry.go` and an existing model scenario for the exact shape.

- [ ] **Step 4: Run the parity suite on every in-tree backend**

Run: `go test ./e2e/parity/... -v` (note: this suite **skips** under `-short`;
`ok … 1.9s` means it was skipped).
Expected: PASS on memory, sqlite and postgres.

- [ ] **Step 5: gRPC**

Check whether the entity-write gRPC surface carries a distinct error envelope
for this path: `grep -rn 'PolymorphicSlot\|ValidationFailed' internal/grpc/`.
If the code is surfaced there, add one `internal/grpc` test per error class per
the rule; if the gRPC entity path shares `common.AppError` classification with
HTTP and has no code-specific arm, record that in the commit message as the
reason no gRPC test is added.

- [ ] **Step 6: Commit**

```bash
git add -A internal/e2e e2e/parity
git commit -m "test(model): cover branch extension on a running backend and across backends"
```

---

### Task 8: Documentation

Gate 4 and Gate 7. The two limitations are documented in three places as
current behaviour; all three describe something that no longer exists.

**Files:**
- Modify: `docs/cloud-parity/model-kind-enforcement.md`
- Modify: `docs/cloud-parity/validation-failure-code.md`, `docs/cloud-parity/README.md`
- Modify: `cmd/cyoda/help/content/models.md`
- Modify: `CHANGELOG.md`

- [ ] **Step 1: `docs/cloud-parity/model-kind-enforcement.md`**

- Delete the "**Known limitation of the `changeLevel` door in cyoda-go**" bullet.
- Rewrite the "**Both write doors reject; the codes differ deliberately**" bullet:
  the extension door now *accepts* a new kind at `STRUCTURAL` and refuses it
  below with a plain change-level violation; the strict door (no `changeLevel`,
  and PATCH) is unchanged and still answers `VALIDATION_FAILED`.
- Delete the sentence in the "not a ban on polymorphic fields" bullet about the
  object-and-array union folding onto a node "whose *dominant* kind is OBJECT" —
  there is no dominant kind. Replace with: a node records the set of kinds the
  field was observed as, and every surface reads the set.
- Update **Coverage** with the new e2e file and parity scenario.
- Keep the doc a present-tense reference, per
  `feedback_architecture_md_is_reference_not_history`: state what is, not what changed.

- [ ] **Step 2: `cmd/cyoda/help/content/models.md`**

- Line 56: the `changeLevel` sentence — with a `changeLevel` set, a payload
  proposing a kind the field does not declare is a schema change permitted at
  `STRUCTURAL` and refused below it as a change-level violation.
- Line 58: delete "**With a `changeLevel` set, a write matching any but the
  field's dominant kind is still refused** … Leave `changeLevel` unset on a model
  with multi-kind fields." Replace with: every declared kind is admissible at any
  `changeLevel`, and a new kind is added at `STRUCTURAL`.
- Keep it compact (`feedback_compact_prose`): the actionable core only.

- [ ] **Step 3: `docs/cloud-parity/validation-failure-code.md` and `README.md`**

Drop the `POLYMORPHIC_SLOT` row / mention; the remaining rows are unchanged.

- [ ] **Step 4: CHANGELOG**

Under the unreleased v0.8.4 section:

```markdown
### Breaking

- The `POLYMORPHIC_SLOT` error code is retired. It meant "raising `changeLevel`
  will not help"; adding a kind to a path is now a `STRUCTURAL` change, so
  raising the level is exactly what resolves it. Below `STRUCTURAL` the write is
  refused as an ordinary change-level violation (`VALIDATION_FAILED`).

### Fixed

- A write matching a kind the model *declares* is accepted with a `changeLevel`
  set. The extension gate compared one kind per path, so a model with a
  multi-kind field refused half its own declared data at every level.
- An entity write can establish a second kind for a field at `STRUCTURAL`;
  previously only sample-data import could.
- A field observed only as `null` and later written as an object or an array no
  longer fails with a `500`. The extension accepted it and the delta computation
  then could not express it.
- Writing `null` to a field declared as a scalar no longer requires `TYPE`
  level: it proposes no schema change.
```

- [ ] **Step 5: Verify the help tree and the docs build**

Run: `go test ./cmd/cyoda/... && go run ./cmd/cyoda help errors && go run ./cmd/cyoda help models`
Expected: PASS; no dangling `POLYMORPHIC_SLOT` topic.

- [ ] **Step 6: Commit**

```bash
git add -A docs cmd/cyoda/help CHANGELOG.md
git commit -m "docs(model): a node declares a set of kinds, and a write can add one"
```

---

### Task 9: End-of-deliverable verification

- [ ] **Step 1: Full suite**

Run: `go test ./... -v 2>&1 | tail -60`
Expected: all packages `ok`. Docker required.

- [ ] **Step 2: Plugin submodules**

Run: `make test-all`
Expected: root + `plugins/memory|sqlite|postgres` all green. No plugin depends
on `ModelNode` (the sqlite factory mentions the schema package only in a
comment, and the SPI's exchange type is the flat `FieldDescriptor`), so a
failure here means the SPI surface moved unintentionally.

- [ ] **Step 3: Vet, format, race**

Run: `go vet ./... && gofmt -l . && make race`
Expected: no output from `gofmt -l`; race clean.

- [ ] **Step 4: Cassandra plugin check**

The commercial backend at `../cyoda-go-cassandra` consumes the SPI, not
`ModelNode`. Confirm this change adds nothing to the SPI surface:
`git diff origin/release/v0.8.4 --stat -- go.mod` must show no `cyoda-go-spi`
bump. Record the result; do not open a PR there.

- [ ] **Step 5: Code review and security review**

Dispatch a fresh-context reviewer per `superpowers:requesting-code-review`
(CLAUDE.md makes this a standing request — do not run it inline), then
`antigravity-bundle-security-developer:cc-skill-security-review`.

- [ ] **Step 6: PR**

Base branch **`release/v0.8.4`** (milestone v0.8.4 — verify the merge-base before
`gh pr create`). Body must say `Closes #534` and carry the milestone.
