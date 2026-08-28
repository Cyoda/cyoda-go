# ModelNode holds a set of branches Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `ModelNode`'s single `kind NodeKind` label with the set of
branches it was always summarising, so every reader — in both repositories —
sees every branch a field declares, and the two `changeLevel` limitations that
followed from the label disappear.

**Architecture:** There is **one** `ModelNode` definition, and it lives in
`cyoda-go-spi` alongside the wire codec and the field flattening — the same
consolidation `DataType`/`TypeSet` already had, for the same reason: two
structurally identical copies drift, and these two already did. The node holds
`branches map[NodeKind]Branch` plus a `nullable` flag. `cyoda-go` aliases the
type and keeps `Merge`/`Extend`/`Diff`/`Apply`/`Validate` on top of it.
`mergeKind`'s tiebreak, the dominant kind, `ConcreteTypes` and `isNullOnlyLeaf`
are deleted; set union replaces them. `Extend`'s gate becomes a subset test,
`Diff` becomes per-branch, a new additive op `add_kind_branch` records a branch
being added, and `ErrPolymorphicSlot` / `POLYMORPHIC_SLOT` are decommissioned.

**Tech Stack:** Go 1.26. Two repositories: `cyoda-go-spi`
(`/Users/paul/go-projects/cyoda-light/cyoda-go-spi`) and `cyoda-go` (this
worktree).

**Spec:** GitHub issue #534 (self-contained design). Read it in full before
starting: `gh issue view 534`. This plan resolves what the issue left to
implementation; the **Design decisions** below are binding and were reviewed by
two independent fresh-context reviewers.

## Global Constraints

- **TDD is mandatory** (`.claude/rules/tdd.md`): RED → GREEN, every step. No
  implementation code without a failing test driving it.
- Go 1.26+; `log/slog` only; wrap errors with `fmt.Errorf("...: %w", err)`.
- **No issue IDs in shipped artefacts** — no `#534` in code comments, errors,
  logs, responses, OpenAPI or help content. Commit messages, PR bodies and this
  plan may carry it.
- Milestone **v0.8.4**, base branch **`release/v0.8.4`**. Backward compatibility
  is not a constraint for this release; breaking changes get a `### Breaking`
  CHANGELOG entry.
- **Never `git add -A`.** `go.work` is tracked and will carry an uncommitted
  local `use ../../../../cyoda-go-spi` line for the whole of this work. Stage
  every path explicitly. Verify with `git status --short` before each commit
  that `go.work` is not staged.
- **Never force-move a Go module tag** — `sum.golang.org` caches per-tag SHAs.
  Always cut a fresh version.
- Run scoped tests while iterating; the full suite, `make race`, `make test-all`
  and `go vet ./...` once, at end of deliverable.
- `internal/e2e` needs Docker. `postgres:17-alpine` is already local; do not
  pull.
- `e2e/parity` **skips under `-short`**. `ok … 1.9s` means it was skipped.

---

## Design decisions

Each was measured against the code, not assumed.

### D0 — One node definition, in the SPI

`cyoda-go-spi/model_schema.go` today carries a **second, independent**
`ModelNode` + wire decoder + field flattening, because a storage backend that
self-executes a search must go from `ModelDescriptor.Schema` bytes to a fields
map without importing the engine. It is label-driven and never received the
branch-completeness fix the engine got, so **the two already disagree on today's
persisted bytes**:

```
WIRE: {"kind":"OBJECT","children":{
  "f":{"kind":"OBJECT","children":{"k":…},"element":{…}},   ← object ∪ array
  "g":{"kind":"OBJECT","types":["STRING"],"children":{…}},  ← object ∪ scalar
  "h":{"kind":"ARRAY","types":["STRING"],"element":{…}}}}   ← array ∪ scalar

engine FieldsMap: $.f.k  $.f[*]  $.g  $.g.in  $.h  $.h[*]
SPI    FieldsMap: $.f.k          $.g  $.g.in       $.h[*]
                  missing: $.f[*], $.h
```

Per that file's own header, this does not fail loudly: the path misses in the
map, the leaf comparison matches nothing, and "the search then returns fewer
rows with no error at all". That is a live search-correctness bug on a
self-executing backend, and this change would make union shapes the norm.

**Therefore the node, the codec and the flattening move to the SPI, and
`cyoda-go` aliases them** — exactly as `coretypes.go` already aliases
`DataType`, `TypeSet` and `FieldDescriptor`, and for the reason stated there:
"two structurally identical copies drift, and this one already did".

```go
// cyoda-go/internal/domain/model/schema/coretypes.go
type ModelNode = spi.ModelNode
type NodeKind  = spi.NodeKind
type Branch    = spi.Branch
type ScalarBranch = spi.ScalarBranch
type ObjectBranch = spi.ObjectBranch
type ArrayBranch  = spi.ArrayBranch
type ArrayInfo    = spi.ArrayInfo

var (
	NewObjectNode = spi.NewObjectNode
	NewLeafNode   = spi.NewLeafNode
	NewArrayNode  = spi.NewArrayNode
	Marshal       = spi.MarshalModelNode
	Unmarshal     = spi.UnmarshalModelNode
)
const (
	KindLeaf   = spi.KindLeaf
	KindObject = spi.KindObject
	KindArray  = spi.KindArray
)
```

`Merge`, `Extend`, `Diff`, `Apply`, `Validate`, the op catalog and unique-key
derivation **stay in the engine**: only the engine decides what a model's schema
becomes. The SPI exports the mutators the engine needs to build one (D4); that
is a responsibility split stated in prose, not enforced by unexported fields —
the same arrangement `TypeSet.Add` already has.

**Release shape** (`MAINTAINING.md` → "Coordinated release", and
`feedback_spi_coordinated_release_procedure`): develop both repositories
together behind an uncommitted `go.work` `use` line; at the end, merge and tag
`cyoda-go-spi` **first**, then bump the pin in all four `go.mod` files in **one**
commit.

### D1 — The struct

```go
type ModelNode struct {
	branches   map[NodeKind]Branch
	nullable   bool
	fieldCache atomic.Pointer[cachedFields]
}

type Branch interface{ Kind() NodeKind }

type ScalarBranch struct{ types *TypeSet }               // NULL is NEVER stored here
type ObjectBranch struct{ children map[string]*ModelNode }
type ArrayBranch  struct {
	element *ModelNode
	info    *ArrayInfo
}
```

Accessors: `Scalar() *ScalarBranch`, `Object() *ObjectBranch`,
`Array() *ArrayBranch` (nil when absent); `Kinds() []NodeKind` ascending
(LEAF, OBJECT, ARRAY); `IsPolymorphic() bool` == `len(branches) > 1`;
`Nullable() bool`. **`ModelNode.Kind()` is deleted** — there is no dominant
kind. `NodeKind` and `NodeKind.String()` stay: they name a branch.

A monomorphic field is a set of one. There is deliberately no `POLYMORPHIC`
member — it would store `len(branches) > 1` a second time.

### D2 — The nullable collapse rule

> **`nullable` is recorded only while the node has no scalar branch. Adding a
> scalar branch clears it; setting it while a scalar branch is present is a
> no-op.**

This is `spi.TypeSet.Add`'s existing rule ("NULL is dropped when any concrete
type is present", `datatype.go:118-145`) lifted to the node, and it is what
makes the restructure behaviour-preserving: today a leaf observed as `null` then
`"x"` stores `types={STRING}` and the nullability is already not recorded. A
node carrying a scalar branch admits `null` anyway (`validateLeaf` returns nil
for `nil` data), so nothing is lost.

- `null` only → `branches={}`, `nullable=true` — the nullable-marker node.
- `null` then `{"k":"v"}` → `branches={OBJECT}`, `nullable=true`.
- `null` then `"x"` → `branches={LEAF{STRING}}`, `nullable=false`.

### D3 — `Types() *TypeSet` is DELETED, not redefined

A node's types are now derived, so a `Types()` that keeps its name and signature
but returns a fresh set would **silently no-op** 4 production and 20 test
mutation sites — including
`internal/domain/search/trailing_wildcard_path_test.go:47`,
`internal/domain/search/condition_type_validate_test.go:185`, and
`internal/domain/model/schema/field_test.go:186`
(`obj.Types().Add(Null) // observed as null too`), which would keep passing while
asserting nothing. That is the one change in this plan that could fail silently,
so the name goes and the compiler enumerates every site:

```go
// DeclaredTypes returns the node's DataTypes in the spelling the field walk,
// the exporters and the persisted form use: the scalar branch's concrete
// types, or the single NULL marker when the node is nullable and carries no
// scalar branch, or nil. The slice is a copy.
func (n *ModelNode) DeclaredTypes() []DataType
```

Read sites: `x.Types().Types()` → `x.DeclaredTypes()`;
`x.Types().IsEmpty()` → `len(x.DeclaredTypes()) == 0`.
Mutation sites: D4.

### D4 — Explicit mutators

```go
func (n *ModelNode) AddScalarTypes(dts ...DataType) // NULL among them obeys D2
func (n *ModelNode) SetNullable()                   // no-op when a scalar branch is present
func (n *ModelNode) SetChild(name string, child *ModelNode) // creates the object branch
func (n *ModelNode) SetElement(e *ModelNode)        // creates the array branch
func (n *ModelNode) ObserveArrayWidth(width int)    // creates the array branch
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

**Encode.** `Types` is `DeclaredTypes()` — unchanged semantics, the NULL marker
included. When the node has at most one branch, emit `kind`: the single branch's
name, or `"LEAF"` when there is none. Otherwise emit `kinds`, ascending. Result:
**every monomorphic node, nullable or not, serialises byte-identically to
today.**

**Decode** — these are the rules, in order. They are stated rather than derived,
because `kind:"LEAF"` alone is ambiguous and the ambiguity must be resolved
explicitly:

1. `names` = `Kinds` when non-empty, else `[Kind]` when non-empty, else the node
   is rejected.
2. `nullable` = `"NULL" ∈ Types`. `concrete` = `Types \ {"NULL"}`.
3. **Scalar branch** present iff `len(concrete) > 0`, **or** `"LEAF" ∈ names`
   and not (`concrete` is empty and `nullable`). Its types are `concrete`.
   The second clause is what separates the two `kind:"LEAF"` spellings, and it
   is sound because D2 guarantees a scalar branch never holds NULL.
4. **Object branch** present iff `"OBJECT" ∈ names` or `len(Children) > 0`.
5. **Array branch** present iff `"ARRAY" ∈ names` or `Element != nil`.
6. A node that ends with no branch and is not nullable is **rejected**. The
   codec fails closed, as it does today for an unknown kind.

Every cell round-trips:

| wire | branches | nullable | re-encodes to |
|---|---|---|---|
| `{"kind":"LEAF","types":["NULL"]}` | `{}` | true | itself |
| `{"kind":"LEAF"}` | `{LEAF{}}` | false | itself |
| `{"kind":"LEAF","types":["STRING"]}` | `{LEAF{STRING}}` | false | itself |
| `{"kind":"OBJECT"}` | `{OBJECT{}}` | false | itself |
| `{"kind":"OBJECT","types":["NULL"]}` | `{OBJECT{}}` | true | itself |
| `{"kind":"ARRAY"}` | `{ARRAY{nil}}` | false | itself |
| `{"kind":"OBJECT","types":["STRING"],"children":…,"element":…}` (legacy union) | `{LEAF,OBJECT,ARRAY}` | false | `{"kinds":[…],…}` — semantically identical, no migration |

The one non-injective cell is `{"kind":"LEAF"}` with no types: it would also be
the spelling of a branchless non-nullable node. **The writer never emits that**,
because such a node is unreachable — every constructor establishes a branch or
sets `nullable`, `Merge` of two reachable nodes cannot produce it, and rule 6
rejects it on decode.

Decoding is payload-driven as well as kind-driven (rules 3-5), which is what
lets a model persisted under the old dominant-kind spelling restore every branch
it carries. No migration.

### D6 — ChangeLevel for adding a branch

- The node **already declares ≥1 branch** → adding another creates a union →
  **`STRUCTURAL`** (the issue's settled decision).
- The node **declares none** — the nullable marker, and an array element never
  observed with content — → no kind exists to conflict with, so establishing its
  kinds keeps today's contract: **`TYPE`** at node level, **`ARRAY_ELEMENTS`**
  directly on an array element.

**This is asymmetric on purpose, and the asymmetry is today's contract, not a
derivation.** A branchless array element can go straight to `{LEAF, OBJECT}` at
`ARRAY_ELEMENTS` (`[]` then `[{"a":1},"s"]`), while a node holding one branch
needs `STRUCTURAL` to gain a second. Do not "fix" this: tightening it breaks the
nullable-marker contract that
`TestExtend_ExistingLeafNull_AgainstIncomingArray_PromotesToArray` and the
`null` → `"x"` widening both depend on.

### D7 — Which op records what

| transition | op | why |
|---|---|---|
| `nullable` false → true | `broaden_type` payload `["NULL"]` | today's spelling |
| scalar types widen on an existing scalar branch | `broaden_type` | unchanged |
| **branchless** node gains a scalar branch | `broaden_type` with the concrete types | today's spelling for `null` → `"x"`; deltas stay byte-identical |
| branchless node gains an OBJECT/ARRAY branch | `add_kind_branch` at that node | today `Extend` accepts and `Diff` fails — D8-1 |
| a node with ≥1 branch gains any branch | `add_kind_branch` at that node | limitation 1 |
| ARRAY branch, nil element, gains a LEAF element | `add_array_item_type` | unchanged |
| ARRAY branch, nil element, gains an OBJECT/ARRAY element | `add_kind_branch` **at the array node**, payload = a node carrying only the ARRAY branch | closes `diffArray`'s `TODO(A.3 …)` |

**`add_kind_branch` never targets a `[]` segment whose element does not exist.**
`resolvePath` rejects `[]` when `Element() == nil` ("array has no element at
segment"), so the last row targets the array node and lets `Apply` materialise
the element by merging the array branch — the same shape
`applyAddArrayItemType` already uses. When the element *does* exist (including
the branchless `LEAF[NULL]` marker), `[]` resolves and the op may target
`path/[]`.

`applyBroadenType` keeps its guard in the new vocabulary: non-NULL types may be
added only when the target **has a scalar branch, or has no branches at all** (in
which case one is created). That is exactly today's `target.Kind() == KindLeaf`.

`SchemaOp` has no ChangeLevel field; "ChangeLevel: STRUCTURAL" is a doc-comment
convention, as it already is for `KindAddProperty`.

### D8 — Live defects this closes on the way

Three, all currently reaching the client:

1. `[]` then `[{"a":1}]`. `walker.go:110` gives an empty array `element = LEAF[NULL]`.
   `Extend` accepts the object elements at **every** level; `Diff` then returns
   `kind change at "f/[]": LEAF -> OBJECT (not additive)` and
   `ingest/validate.go:126` turns it into a **500**. "Field starts as an empty
   array, later gets object elements" is the commonest shape of the three.
   Same for `[]` then `[{"a":1},"s"]`.
2. `existing = LEAF[NULL]`, `incoming = OBJECT` at `TYPE`: accepted by `Extend`,
   `Diff` fails, **500**. The unique-key guard comment at
   `ingest/validate.go:105-112` documents this as a partial workaround; that
   rationale becomes false and the comment must be rewritten (the guard itself
   stays — see D9's new 422 path).
3. `existing = LEAF{STRING}`, `incoming = LEAF{NULL}` (writing `null` to a
   string field) below `TYPE` is rejected with "type change requires TYPE
   level", although the delta is empty. Under D2 it proposes nothing and is
   accepted at every level.

Each gets a test and a CHANGELOG line.

### D9 — `ErrPolymorphicSlot` is decommissioned

Exactly two raise sites — `extend.go:101` and `extend.go:196` — both label
comparisons the subset gate replaces. Every remaining `Extend` rejection
(nullability, scalar widening, new child, new branch, array width) names a level
that `STRUCTURAL` satisfies, so the sentinel's meaning ("raising `changeLevel`
does not help") evaporates. `extend.go:32` already carries
`TODO(#85): decommission this sentinel + common.ErrCodePolymorphicSlot` once
these semantics land. Leaving it would be dead code (Gate 6). **Confirmed by the
user.**

One consequence to cover rather than remove: a `STRUCTURAL` write that adds a
container branch to a unique-key field now reaches `ValidateUniqueKeys` and
answers `422 INVALID_UNIQUE_KEY_DEFINITION` (`uniquekey_validate.go:65-69`).
That path was previously unreachable from an entity write. It needs a test.

### D10 — `ArrayInfo` sheds its dead half

`ObserveElement`, `Elements` and `IsUniform` (and the `elements []*TypeSet`
field, and `mergeArrayInfo`'s per-position merge) have **no production caller** —
only tests. Only `Observe` and `MaxWidth` are live (`walker.go:127`,
`extend.go:153-154`, `simple_view.go:167`). Carrying dead API across a repo
boundary into a public SPI surface is not on, so `ArrayInfo` reduces to
`maxWidth int` and `mergeArrayInfo` to a max. Gate 6, bounded, surfaced by this
work.

`ArrayInfo` is still not persisted (a separate known gap); the restructure gives
it a home in `ArrayBranch`, nothing more. The SPI already owns the concept —
`FieldDescriptor.MaxWidth`, documented there as "a discovery-time statistic that
the wire format does not carry".

### D11 — One intentional behaviour addition

Today `checkElementWidening` does **not** check array width on a *nested* array,
so an inner width 1→5 passes even at level `""`. The unified walk applies the
`ARRAY_LENGTH` check at every array level. `Extend` is only ever called with a
non-empty level (`ingest/validate.go:78-88` returns early when `ChangeLevel ==
""`; `e2e/parity/oracle.go` uses `STRUCTURAL`), and `ARRAY_LENGTH` is the lowest
rank, so this is unobservable. Recorded so it is a decision, not an accident.

### D12 — Out of scope

- `ArrayInfo` persistence.
- Cloud's tagged-union parity and the rest of #85.
- `docs/superpowers/**` and `docs/plans/**` are historical records
  (`.claude/rules/documentation-hygiene.md`) and are not updated. That includes
  `docs/superpowers/specs/2026-07-23-431-evaluator-convergence-design.md:120`,
  which states the search evaluator's premise as "a write that would make a
  field a second type is rejected at ingest (`schema.ErrPolymorphicSlot`)". It
  is the one place a downstream subsystem records a dependency on the sentinel —
  but the premise was **already** false, because sample-data import has always
  been able to declare several kinds for a path. The living statement of that
  contract is `docs/cloud-parity/model-kind-enforcement.md`, which Task 11
  updates. Leave the spec as the record of what was believed when it was
  written.

---

## File structure

### `cyoda-go-spi` (repo at `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`)

- **Modify** `model_schema.go` — the branch set, accessors, mutators, the
  `kinds`-aware payload-driven codec, the branch-driven flattening, `ArrayInfo`
- **Modify** `model_schema_test.go` — 12 existing tests
- **Create** `model_schema_branch_test.go` — branch-set, codec and flattening tests
- **Modify** `CHANGELOG.md`

### `cyoda-go` (this worktree)

- **Modify** `internal/domain/model/schema/coretypes.go` — the aliases (D0)
- **Delete** `internal/domain/model/schema/node.go`, `codec.go` — now the SPI's
- **Modify** `internal/domain/model/schema/field.go` — keeps only `ValidateUniqueKeys`' helpers if any remain; the walk moves to the SPI
- **Modify** `merge.go`, `extend.go`, `diff.go`, `apply.go`, `ops.go`, `validate.go`
- **Modify** `gentree/gentree.go` — branch-driven, plus a kind-mutating mode
- **Modify** `exporter/simple_view.go`, `exporter/json_schema.go`
- **Modify** `importer/walker.go` — `Info().Observe` → `ObserveArrayWidth`
- **Modify** `internal/domain/entity/handler.go`, `internal/domain/model/ingest/validate.go`, `internal/common/error_codes.go`
- **Tests to convert** (they call `Kind()` or `Types().Add`):
  `schema/{node,codec,merge,diff,field,completeness,validate_branch_dispatch,field_kind_union,extend_*,apply}_test.go`,
  `schema/gentree/gentree_test.go`, `importer/{walker,sample_documents}_test.go`,
  `exporter/json_schema_test.go`,
  `internal/domain/search/{trailing_wildcard_path,condition_type_validate}_test.go`
- **Tests to add/extend**: `internal/e2e/model_kind_branch_extension_test.go`,
  `internal/e2e/model_kind_enforcement_test.go`,
  `internal/grpc/model_kind_enforcement_test.go`, `e2e/parity/` + `registry.go`
- **Docs**: `docs/cloud-parity/{model-kind-enforcement,validation-failure-code,README}.md`,
  `cmd/cyoda/help/content/models.md`, `errors.md`, `errors/*.md`,
  `api/openapi.yaml`, `CHANGELOG.md`, `COMPATIBILITY.md`

---

### Task 0: Wire the two repositories together

- [ ] **Step 1: Create the SPI feature branch**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git checkout -b feat/model-node-branch-set origin/main
```

- [ ] **Step 2: Add the local `use` line — and do not commit it**

In the cyoda-go worktree, append to `go.work`:

```
use ../../../../cyoda-go-spi
```

- [ ] **Step 3: Prove it resolves and record the baseline**

```bash
go build ./... && go test ./internal/domain/model/... 
git status --short   # go.work MUST show as modified and stay unstaged
```
Expected: build succeeds against the local SPI checkout; model tests pass.

---

### Task 1: The SPI node becomes a branch set

**Repo:** `cyoda-go-spi`. **Files:** `model_schema.go`, new
`model_schema_branch_test.go`.

**Produces** (consumed by every later task):

```go
type Branch interface{ Kind() NodeKind }
type ScalarBranch struct{ /* unexported */ }
func (b *ScalarBranch) Kind() NodeKind
func (b *ScalarBranch) Types() []DataType        // copy; never contains NULL
type ObjectBranch struct{ /* unexported */ }
func (b *ObjectBranch) Kind() NodeKind
func (b *ObjectBranch) Children() map[string]*ModelNode
func (b *ObjectBranch) Child(name string) *ModelNode
type ArrayBranch struct{ /* unexported */ }
func (b *ArrayBranch) Kind() NodeKind
func (b *ArrayBranch) Element() *ModelNode        // may be nil
func (b *ArrayBranch) MaxWidth() int

func (n *ModelNode) Scalar() *ScalarBranch
func (n *ModelNode) Object() *ObjectBranch
func (n *ModelNode) Array()  *ArrayBranch
func (n *ModelNode) Kinds()  []NodeKind
func (n *ModelNode) IsPolymorphic() bool
func (n *ModelNode) Nullable() bool
func (n *ModelNode) DeclaredTypes() []DataType
func (n *ModelNode) AddScalarTypes(dts ...DataType)
func (n *ModelNode) SetNullable()
func (n *ModelNode) SetChild(name string, child *ModelNode)
func (n *ModelNode) SetElement(e *ModelNode)
func (n *ModelNode) ObserveArrayWidth(width int)
```

`ModelNode.Kind()`, `ModelNode.Types()`, `ModelNode.Element()` and
`ModelNode.Children()` are **deleted** from the SPI; callers ask for a branch.
`ArrayInfo` reduces to `maxWidth` per D10.

- [ ] **Step 1: Write the failing test**

Create `model_schema_branch_test.go` in the SPI:

```go
package spi

import "testing"

func TestBranchAccessors(t *testing.T) {
	n := NewObjectNode()
	if n.Object() == nil {
		t.Fatal("an object node with no children still declares the object branch")
	}
	if n.Scalar() != nil || n.Array() != nil {
		t.Errorf("kinds = %v, want only OBJECT", n.Kinds())
	}
	if n.IsPolymorphic() {
		t.Error("one branch is not polymorphic")
	}

	marker := NewLeafNode(Null)
	if marker.Scalar() != nil {
		t.Error("NULL is a marker, not a scalar observation: no scalar branch")
	}
	if !marker.Nullable() || len(marker.Kinds()) != 0 {
		t.Errorf("the nullable marker declares no kind; kinds=%v nullable=%v", marker.Kinds(), marker.Nullable())
	}
	if got := marker.DeclaredTypes(); len(got) != 1 || got[0] != Null {
		t.Errorf("DeclaredTypes() = %v, want [NULL] — the persisted spelling is unchanged", got)
	}

	s := NewLeafNode(String)
	s.SetNullable()
	if s.Nullable() {
		t.Error("D2: a node carrying a scalar branch does not record the marker separately")
	}
	if got := s.DeclaredTypes(); len(got) != 1 || got[0] != String {
		t.Errorf("DeclaredTypes() = %v, want [STRING]", got)
	}

	k := NewLeafNode(Null)
	k.AddScalarTypes(String)
	if k.Scalar() == nil || k.Nullable() {
		t.Error("adding a concrete type establishes the scalar branch and clears the marker")
	}
}

func TestKinds_AscendingOrder(t *testing.T) {
	n := NewObjectNode()
	n.AddScalarTypes(String)
	n.SetElement(NewLeafNode(Integer))
	want := []NodeKind{KindLeaf, KindObject, KindArray}
	got := n.Kinds()
	if len(got) != 3 {
		t.Fatalf("Kinds() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Kinds() = %v, want %v", got, want)
		}
	}
}

func TestSetElement_CreatesTheArrayBranch(t *testing.T) {
	n := NewObjectNode()
	n.SetElement(NewLeafNode(String))
	if n.Array() == nil || n.Array().Element() == nil {
		t.Fatal("SetElement establishes the array branch")
	}
	if n.Object() == nil {
		t.Error("SetElement must not disturb the object branch")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./ -run 'TestBranchAccessors|TestKinds_Ascending|TestSetElement' -v
```
Expected: compile failure — the accessors do not exist.

- [ ] **Step 3: Implement the branch set in `model_schema.go`**

Replace the four loose fields with `branches map[NodeKind]Branch` + `nullable`.
Constructors: `NewObjectNode()` → one empty `ObjectBranch`; `NewLeafNode(dt)` →
`dt == Null` ? branchless + nullable : one `ScalarBranch`; `NewArrayNode(e)` →
one `ArrayBranch{element: e}`. `DeclaredTypes`, `AddScalarTypes`, `SetNullable`
per D2/D3/D4. Every mutator clears `fieldCache` (`SetChild` already does).
Reduce `ArrayInfo` per D10.

Leave `fromWire` and `collectFields` on a temporary `dominantKind(n)` shim in
this step — Tasks 2 and 3 replace them — so this step's diff is the type only.

- [ ] **Step 4: Run — expect PASS**

```bash
go test ./ -run 'TestBranchAccessors|TestKinds_Ascending|TestSetElement' -v
```

- [ ] **Step 5: Run the whole SPI suite and convert the 12 existing tests**

```bash
go test ./... 
```
`model_schema_test.go` calls the deleted `Kind()`/`Types()`/`Element()`/`Children()`.
Convert with: `Kind()==KindObject` → `Object()!=nil`; `Types().Types()` →
`DeclaredTypes()`; `Element()` → `Array().Element()`; `Children()` →
`Object().Children()`. An assertion that has to change means the conversion was
not behaviour-preserving — fix the conversion, not the test.

- [ ] **Step 6: Commit (SPI repo)**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add model_schema.go model_schema_test.go model_schema_branch_test.go
git commit -m "refactor(model): a schema node holds the set of kinds it was observed as

branches+nullable replace kind/types/children/element. The label could only
name one of three independent payload slots, so a reader that dispatched on it
lost a branch. ArrayInfo sheds ObserveElement/Elements/IsUniform, which had no
caller."
```

---

### Task 2: The SPI codec — payload-driven, and it speaks `kinds`

**Repo:** `cyoda-go-spi`. **Files:** `model_schema.go`, `model_schema_branch_test.go`.

- [ ] **Step 1: Write the failing test**

Append to `model_schema_branch_test.go`:

```go
func TestCodec_EveryShapeRoundTrips(t *testing.T) {
	for _, raw := range []string{
		`{"kind":"LEAF","types":["STRING"]}`,
		`{"kind":"LEAF","types":["NULL"]}`,
		`{"kind":"LEAF"}`,
		`{"kind":"OBJECT"}`,
		`{"kind":"OBJECT","types":["NULL"]}`,
		`{"kind":"ARRAY"}`,
		`{"kind":"ARRAY","element":{"kind":"LEAF","types":["STRING"]}}`,
	} {
		n, err := UnmarshalModelNode([]byte(raw))
		if err != nil {
			t.Fatalf("Unmarshal(%s): %v", raw, err)
		}
		out, err := MarshalModelNode(n)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(out) != raw {
			t.Errorf("round trip %s -> %s", raw, out)
		}
	}
}

// D5 rule 3: the two kind:"LEAF" spellings mean different things.
func TestCodec_LeafSpellingsAreDistinct(t *testing.T) {
	marker, err := UnmarshalModelNode([]byte(`{"kind":"LEAF","types":["NULL"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if marker.Scalar() != nil || !marker.Nullable() {
		t.Errorf("kind:LEAF types:[NULL] is the branchless marker; kinds=%v nullable=%v", marker.Kinds(), marker.Nullable())
	}

	empty, err := UnmarshalModelNode([]byte(`{"kind":"LEAF"}`))
	if err != nil {
		t.Fatal(err)
	}
	if empty.Scalar() == nil || empty.Nullable() {
		t.Errorf("kind:LEAF with no types is an empty scalar branch; kinds=%v nullable=%v", empty.Kinds(), empty.Nullable())
	}
}

// A model persisted under the old dominant-kind spelling restores every branch
// its payload carries. No migration.
func TestCodec_ReadsTheOldDominantKindSpelling(t *testing.T) {
	const legacy = `{"kind":"OBJECT","types":["STRING"],` +
		`"children":{"k":{"kind":"LEAF","types":["INTEGER"]}},` +
		`"element":{"kind":"LEAF","types":["BOOLEAN"]}}`
	n, err := UnmarshalModelNode([]byte(legacy))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if n.Scalar() == nil || n.Object() == nil || n.Array() == nil {
		t.Fatalf("legacy node must restore all three branches; kinds=%v", n.Kinds())
	}
}

func TestCodec_UnionSpellsEveryKind(t *testing.T) {
	n := NewObjectNode()
	n.SetElement(NewLeafNode(Integer))
	raw, err := MarshalModelNode(n)
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalModelNode(raw)
	if err != nil {
		t.Fatalf("Unmarshal(%s): %v", raw, err)
	}
	if back.Object() == nil || back.Array() == nil || len(back.Kinds()) != 2 {
		t.Errorf("round trip lost a branch: %s -> kinds=%v", raw, back.Kinds())
	}
}

func TestCodec_RejectsAKindlessNode(t *testing.T) {
	if _, err := UnmarshalModelNode([]byte(`{}`)); err == nil {
		t.Error("a node with neither a kind nor a payload must be rejected")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test ./ -run TestCodec_ -v
```
Expected: FAIL — `MarshalModelNode` does not exist; the legacy and union cases lose branches.

- [ ] **Step 3: Implement D5**

Add `Kinds []string` to `wireNode`, make `Kind` `omitempty`, add
`MarshalModelNode`, implement the encode rule and decode rules 1-6 verbatim, and
delete the Task-1 `dominantKind` shim. Rewrite `fromWire`'s doc comment: the old
one calls the OBJECT-ignores-Element behaviour "one inherited gap, preserved
deliberately for parity … tracked upstream (Cyoda/cyoda-go#464)". That parity
argument is now inverted — the engine reads every branch and so does this — so
the comment must say the decode is payload-driven and why.

- [ ] **Step 4: Run — expect PASS**

```bash
go test ./... 
```

- [ ] **Step 5: Commit (SPI repo)**

```bash
git add model_schema.go model_schema_branch_test.go
git commit -m "feat(model)!: the persisted node spells its whole kind set

wireNode gains \"kinds\"; \"kind\" is still written for a node with at most one
branch, so every monomorphic node — nullable or not — serialises exactly as
before and there is no migration. Decoding is payload-driven as well as
kind-driven, so a node stored under the old dominant-kind spelling restores
every branch it carries instead of the one the label happened to name."
```

---

### Task 3: The SPI field walk reads every branch

This is the fix for the live divergence in D0.

**Repo:** `cyoda-go-spi`. **Files:** `model_schema.go`, `model_schema_branch_test.go`.

- [ ] **Step 1: Write the failing test**

Append to `model_schema_branch_test.go`:

```go
// The flattening is a contract every executor must agree on: a path spelled
// differently here simply misses at lookup, the leaf matches nothing, and the
// search returns fewer rows with no error at all.
func TestFieldsMap_NamesEveryBranchOfAUnion(t *testing.T) {
	// object ∪ array   — the label used to say OBJECT and the element was lost
	f := NewObjectNode()
	f.SetChild("k", NewLeafNode(String))
	f.SetElement(NewLeafNode(Integer))
	// object ∪ scalar
	g := NewObjectNode()
	g.SetChild("in", NewLeafNode(Integer))
	g.AddScalarTypes(String)
	// array ∪ scalar   — the label used to say ARRAY and the scalar was lost
	h := NewArrayNode(NewLeafNode(String))
	h.AddScalarTypes(String)

	root := NewObjectNode()
	root.SetChild("f", f)
	root.SetChild("g", g)
	root.SetChild("h", h)

	raw, err := MarshalModelNode(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := FieldsMapFromSchema(raw)
	if err != nil {
		t.Fatalf("FieldsMapFromSchema: %v", err)
	}
	for _, path := range []string{"$.f.k", "$.f[*]", "$.g", "$.g.in", "$.h", "$.h[*]"} {
		if _, ok := got[path]; !ok {
			t.Errorf("declared path %q is missing from the fields map (have %d entries)", path, len(got))
		}
	}
}

// The branchless marker declares NULL, exactly as a LEAF[NULL] did.
func TestFieldsMap_NullableMarkerDeclaresNull(t *testing.T) {
	root := NewObjectNode()
	root.SetChild("n", NewLeafNode(Null))
	raw, _ := MarshalModelNode(root)
	got, err := FieldsMapFromSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := got["$.n"]
	if !ok {
		t.Fatal("a null-only field is still a declared field")
	}
	if len(d.Types) != 1 || d.Types[0] != Null {
		t.Errorf("Types = %v, want [NULL]", d.Types)
	}
}

// A merely nullable container stays a pure container: NULL is not a scalar
// observation, so it opens no self-descriptor.
func TestFieldsMap_NullableContainerEmitsNoSelfDescriptor(t *testing.T) {
	c := NewObjectNode()
	c.SetChild("in", NewLeafNode(Integer))
	c.SetNullable()
	root := NewObjectNode()
	root.SetChild("c", c)
	raw, _ := MarshalModelNode(root)
	got, _ := FieldsMapFromSchema(raw)
	if _, ok := got["$.c"]; ok {
		t.Error("a nullable object is a pure container, not a searchable scalar leaf")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test ./ -run TestFieldsMap_ -v
```
Expected: `$.f[*]` and `$.h` missing — the live bug, now under test.

- [ ] **Step 3: Rewrite `collectFields` on the branches**

Emit, in this order, so paths and ordering are unchanged:

1. **Scalar arm.** `if s := n.Scalar(); s != nil` → emit `{prefix, s.Types(), inArray}`.
   `else if n.Nullable() && len(n.Kinds()) == 0` → emit `{prefix, []DataType{Null}, inArray}`.
   This reproduces today's three arms exactly: a `LEAF` emitted its whole
   TypeSet (so `[NULL]` for the marker, and `[]` for the empty leaf), and a
   container emitted only its concrete types.
2. **Object arm.** Sorted child keys, `collectFields(child, prefix+"."+k, false, out)`.
3. **Array arm.** `if a := n.Array(); a != nil && a.Element() != nil` → if the
   element has a scalar branch and no container branch, emit
   `{prefix+"[*]", elementTypes, IsArray: true}`; otherwise recurse with the
   `[*]` prefix and `inArray=false`. An array whose element was never observed
   declares nothing and emits nothing — unchanged.

Keep `IsArray`'s narrow meaning: set only for a leaf reached directly as an
array's element type. Consumers key off the narrow meaning.

- [ ] **Step 4: Run the whole SPI suite**

```bash
go test ./... && go vet ./...
```

- [ ] **Step 5: Update the SPI CHANGELOG**

Under `## [Unreleased]`:

```markdown
### Breaking

- **The schema node holds a set of kinds.** `ModelNode.Kind()`, `.Types()`,
  `.Element()` and `.Children()` are replaced by `.Scalar()`, `.Object()`,
  `.Array()`, `.Kinds()`, `.Nullable()` and `.DeclaredTypes()`. The persisted
  form gains `"kinds"`; `"kind"` is still written for a node with at most one
  branch, so every monomorphic node serialises byte-identically and there is no
  migration. `ArrayInfo` loses `ObserveElement`, `Elements` and `IsUniform`,
  which had no caller.

### Fixed

- **`FieldsMapFromSchema` no longer loses a declared path on a field observed as
  more than one kind.** The decoder dispatched on the node's single `kind`
  label, so the array branch of an object-and-array union and the scalar branch
  of an array-and-scalar union were dropped. A predicate on such a path then
  found no declared type and matched nothing — fewer rows, with no error. Decode
  and flattening now read every branch the payload carries.
```

- [ ] **Step 6: Commit (SPI repo)**

```bash
git add model_schema.go model_schema_branch_test.go CHANGELOG.md
git commit -m "fix(model): the fields map names every branch a field declares

The flattening dispatched on one kind label, so a search against the array
branch of an object-and-array union — or the scalar branch of an
array-and-scalar union — found no declared type and matched nothing. Fewer
rows, no error. It now walks the branch set, which is what the engine's own
flattening already does."
```

---

### Task 4: `cyoda-go` aliases the one definition

**Repo:** `cyoda-go`. This is the big mechanical task: the engine's duplicate
node, codec and field walk are deleted, and every call site is converted. The
deletion of `Kind()` and `Types()` is what makes the compiler enumerate them.

**Files:** delete `schema/node.go`, `schema/codec.go`; rewrite `schema/field.go`;
modify `coretypes.go`, `merge.go`, `extend.go`, `diff.go`, `apply.go`,
`validate.go`, `gentree/gentree.go`, `exporter/*.go`, `importer/walker.go`, and
the test files listed in **File structure**.

- [ ] **Step 1: Write the failing test**

Create `internal/domain/model/schema/alias_test.go`:

```go
package schema_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// One definition. The engine's field walk and a self-executing backend's field
// walk are the same code, so they cannot drift.
func TestModelNodeIsTheSPIType(t *testing.T) {
	var n *schema.ModelNode = spi.NewObjectNode()
	n.SetChild("k", schema.NewLeafNode(schema.String))
	raw, err := schema.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	viaEngine := n.FieldsMap()
	viaSPI, err := spi.FieldsMapFromSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(viaEngine) != len(viaSPI) {
		t.Fatalf("the two field walks disagree: engine=%v spi=%v", viaEngine, viaSPI)
	}
	for p := range viaEngine {
		if _, ok := viaSPI[p]; !ok {
			t.Errorf("path %q missing from the SPI walk", p)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test ./internal/domain/model/schema/ -run TestModelNodeIsTheSPIType -v
```
Expected: compile failure — `*schema.ModelNode` and `*spi.ModelNode` are
distinct types.

- [ ] **Step 3: Alias in `coretypes.go`, delete the duplicates**

Add the aliases from D0. Delete `schema/node.go` and `schema/codec.go` entirely.
In `schema/field.go`, delete `Fields`, `FieldsMap`, `buildFieldCache`,
`collectFields`, `cachedFields`, `ConcreteTypes` and `concreteTypes` — all now
the SPI's. If nothing remains in `field.go`, delete the file.

- [ ] **Step 4: Convert every call site**

```bash
grep -rn '\.Kind()\|\.Types()\|ConcreteTypes\|\.Info()\|NewArrayInfo' --include='*.go' . | grep -v cmd/cyoda/help/openapi_tags.go
```

(`cmd/cyoda/help/openapi_tags.go:235` is `reflect.Value.Kind()` — leave it.)

| today | becomes |
|---|---|
| `n.Kind() == KindLeaf` | `n.Object() == nil && n.Array() == nil` |
| `n.Kind() == KindObject` | `n.Object() != nil` |
| `n.Kind() == KindArray \|\| n.Element() != nil`, `hasArrayBranch(n)` | `n.Array() != nil` |
| `n.Element()` | `n.Array().Element()` (nil-check the branch first) |
| `n.Children()` / `n.Child(x)` | `n.Object().Children()` / `n.Object().Child(x)` |
| `n.Types().Types()`, `ConcreteTypes(n.Types())` | `n.DeclaredTypes()` / `n.Scalar().Types()` — see below |
| `n.Types().IsEmpty()` | `len(n.DeclaredTypes()) == 0` |
| `n.Types().Add(dt)` | `n.AddScalarTypes(dt)` (or `n.SetNullable()` for NULL) |
| `n.Info().Observe(w)` | `n.ObserveArrayWidth(w)` |
| `n.Info().MaxWidth()` | `n.Array().MaxWidth()` |
| `switch node.Kind()` | branch by branch: `Scalar()`, `Object()`, `Array()` |
| `existing.Kind() != incoming.Kind()` in `extend.go` | leave; Task 6 replaces it. Use a local `kindNames(n)` for the message. |

`DeclaredTypes()` vs `Scalar().Types()`: use `DeclaredTypes()` where the old
code read `n.Types()` on a node that might be the nullable marker (the wire
form, `validateLeaf`, the exporters' leaf descriptor); use `Scalar().Types()`
where the old code called `ConcreteTypes` (the scalar-branch-of-a-union checks
in `validate.go`, `json_schema.go`, `simple_view.go`).

In `validate.go`, `hasArrayBranch`, `matchesScalarBranch` and
`declaredKindNames` collapse to accessor one-liners — **delete `hasArrayBranch`
outright** (`n.Array() != nil` at each site) and keep the other two as named
helpers over `Scalar()`/`Kinds()`. The issue lists all three as deletions; this
is where that happens.

- [ ] **Step 5: Run the full affected surface**

```bash
go build ./... && go vet ./... && \
go test ./internal/domain/model/... ./internal/domain/entity/... \
        ./internal/domain/search/... ./internal/cluster/... ./e2e/parity/...
```
Expected: PASS with **no assertion changes**. `internal/domain/search` and
`internal/cluster` are in scope because they hold `*schema.ModelNode` and build
union fixtures via `Types().Add`. A test whose *assertions* must change means
the conversion was not behaviour-preserving — fix the conversion.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/model internal/domain/entity internal/domain/search internal/cluster
git status --short   # confirm go.work is NOT staged
git commit -m "refactor(model): one ModelNode definition, in the SPI

The engine and the SPI each carried a node type, a wire decoder and a field
flattening for the same persisted bytes, and they had already drifted. The
engine now aliases the SPI's, as it already does for DataType and TypeSet, and
keeps Merge/Extend/Diff/Apply/Validate on top. Kind() and Types() are gone with
the duplicate: the label named one of three independent slots, and a Types()
that kept its name while returning a derived set would have silently no-opped
two dozen mutation sites."
```

---

### Task 5: `Merge` is a set union

**Files:** `schema/merge.go`, new `schema/merge_branch_test.go`.

- [ ] **Step 1: Write the failing test**

```go
package schema_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// mergeKind's OBJECT-wins tiebreak destroyed information the payload kept: an
// object-and-array union marshalled to {"kind":"OBJECT"} and lost the array
// branch outright.
func TestMerge_KeepsAnEmptyArrayBranchOfAUnion(t *testing.T) {
	n := schema.Merge(schema.NewObjectNode(), schema.NewArrayNode(nil))
	if n.Array() == nil {
		t.Fatalf("the array branch survives the union; kinds=%v", n.Kinds())
	}
	raw, err := schema.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	back, err := schema.Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Array() == nil {
		t.Errorf("the array branch did not survive persistence: %s", raw)
	}
}

// Set union is commutative by construction; mergeKind's precedence achieved
// that only by accident.
func TestMerge_BranchUnionIsCommutative(t *testing.T) {
	kinds := func(n *schema.ModelNode) string {
		s := ""
		for _, k := range n.Kinds() {
			s += k.String() + ","
		}
		return s
	}
	cases := []struct {
		name string
		a, b func() *schema.ModelNode
	}{
		{"scalar+array", func() *schema.ModelNode { return schema.NewLeafNode(schema.String) },
			func() *schema.ModelNode { return schema.NewArrayNode(schema.NewLeafNode(schema.String)) }},
		{"object+array", func() *schema.ModelNode { return schema.NewObjectNode() },
			func() *schema.ModelNode { return schema.NewArrayNode(schema.NewLeafNode(schema.Integer)) }},
		{"null+array", func() *schema.ModelNode { return schema.NewLeafNode(schema.Null) },
			func() *schema.ModelNode { return schema.NewArrayNode(schema.NewLeafNode(schema.String)) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if ab, ba := kinds(schema.Merge(c.a(), c.b())), kinds(schema.Merge(c.b(), c.a())); ab != ba {
				t.Errorf("not commutative: a+b=%s b+a=%s", ab, ba)
			}
		})
	}
}

// Merge built its result from NewObjectNode(), so every merged node carried an
// empty children map whatever its kind.
func TestMerge_TwoLeavesDeclareOnlyTheScalarBranch(t *testing.T) {
	n := schema.Merge(schema.NewLeafNode(schema.String), schema.NewLeafNode(schema.Integer))
	if n.Object() != nil || n.Array() != nil {
		t.Errorf("leaf+leaf declares only the scalar branch; kinds=%v", n.Kinds())
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test ./internal/domain/model/schema/ -run TestMerge_ -v
```
Expected: `TestMerge_KeepsAnEmptyArrayBranchOfAUnion` FAILS — `mergeKind`
returns `KindObject` and `NewArrayNode(nil)` has no element, so the array branch
has nothing to survive on. This is the genuine RED for this task.

- [ ] **Step 3: Rewrite `Merge`**

```go
result := &ModelNode{}                       // built via the SPI constructors/mutators
for each kind k in union(a.Kinds(), b.Kinds()):
    scalar: AddScalarTypes(union of both branches' types)
    object: SetChild(name, Merge(aChild, bChild)) for every name in either
    array:  SetElement(Merge(aElem, bElem)); ObserveArrayWidth(max of both)
if a.Nullable() || b.Nullable() { result.SetNullable() }   // D2 makes this a no-op when a scalar branch exists
```

Delete `mergeKind` and its comment. Reduce `mergeArrayInfo` to a max (D10) or
inline it.

- [ ] **Step 4: Run the model suite and the property suites**

```bash
go test ./internal/domain/model/... ./e2e/parity/...
```
Expected: PASS. `TestRoundtripRandomSeeds` (1000 seeds), commutativity,
idempotence, monotonicity and permutation are the oracle for "no behaviour
change" through Tasks 4-5.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/model/schema
git commit -m "refactor(model): Merge unions branch sets

mergeKind had to pick one label when two kinds met, and its OBJECT-wins
tiebreak carried no meaning while deciding which branch every reader saw. Sets
union instead, which is commutative by construction rather than by accident."
```

---

### Task 6: The gate is a subset test, and Diff is per-branch

Ends limitation 2.

**Files:** `schema/extend.go`, `schema/diff.go`, new
`schema/extend_branch_subset_test.go`, new `schema/diff_branch_test.go`;
convert `extend_kindmismatch_test.go`, `extend_polymorphic_error_test.go`,
`extend_array_element_test.go`, `extend_nullable_test.go`.

- [ ] **Step 1: Write the failing tests**

Create `schema/extend_branch_subset_test.go` — a model with a multi-kind field
was unusable with a `changeLevel`, refusing half its own declared data:

```go
package schema

import (
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestExtend_AcceptsEveryDeclaredBranch(t *testing.T) {
	levels := []spi.ChangeLevel{
		spi.ChangeLevelArrayLength, spi.ChangeLevelArrayElements,
		spi.ChangeLevelType, spi.ChangeLevelStructural,
	}
	cases := []struct {
		name               string
		declared, incoming func() *ModelNode
	}{
		{"array∪scalar, scalar write",
			func() *ModelNode { return Merge(NewArrayNode(NewLeafNode(String)), NewLeafNode(String)) },
			func() *ModelNode { return NewLeafNode(String) }},
		{"object∪scalar, scalar write",
			func() *ModelNode { return Merge(NewObjectNode(), NewLeafNode(String)) },
			func() *ModelNode { return NewLeafNode(String) }},
		{"object∪array, array write",
			func() *ModelNode { return Merge(NewObjectNode(), NewArrayNode(NewLeafNode(Integer))) },
			func() *ModelNode { return NewArrayNode(NewLeafNode(Integer)) }},
		{"object∪array, object write",
			func() *ModelNode { return Merge(NewObjectNode(), NewArrayNode(NewLeafNode(Integer))) },
			func() *ModelNode { return NewObjectNode() }},
	}
	for _, c := range cases {
		for _, level := range levels {
			t.Run(c.name+"/"+string(level), func(t *testing.T) {
				e := NewObjectNode()
				e.SetChild("f", c.declared())
				i := NewObjectNode()
				i.SetChild("f", c.incoming())
				if _, err := Extend(e, i, level); err != nil {
					t.Fatalf("a write matching a declared branch must be accepted at %q; got: %v", level, err)
				}
			})
		}
	}
}

func TestExtend_AddingABranchRequiresStructural(t *testing.T) {
	cases := []struct {
		name               string
		existing, incoming func() *ModelNode
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
			err := func() error { _, err := Extend(e, i, spi.ChangeLevelType); return err }()
			if err == nil {
				t.Fatal("adding a branch below STRUCTURAL must be refused")
			}
			// The rejection must name the level that resolves it: there is no
			// polymorphic-slot sentinel any more, because raising the level
			// now works.
			if !strings.Contains(err.Error(), "STRUCTURAL") {
				t.Errorf("the message must name STRUCTURAL; got: %v", err)
			}
			e, i = build()
			got, err := Extend(e, i, spi.ChangeLevelStructural)
			if err != nil {
				t.Fatalf("adding a branch at STRUCTURAL must be accepted; got: %v", err)
			}
			if !got.Object().Child("f").IsPolymorphic() {
				t.Errorf("the extended field declares both kinds; kinds=%v", got.Object().Child("f").Kinds())
			}
		})
	}
}

// D8-3: null against a declared scalar proposes no schema change, so no level
// gates it.
func TestExtend_NullAgainstAScalarProposesNothing(t *testing.T) {
	for _, level := range []spi.ChangeLevel{spi.ChangeLevel(""), spi.ChangeLevelArrayLength} {
		e := NewObjectNode()
		e.SetChild("s", NewLeafNode(String))
		i := NewObjectNode()
		i.SetChild("s", NewLeafNode(Null))
		if _, err := Extend(e, i, level); err != nil {
			t.Errorf("null against a declared scalar must be accepted at %q; got: %v", level, err)
		}
	}
}

// D6: the branchless node keeps today's levels.
func TestExtend_BranchlessNodeKeepsTheNullableMarkerLevels(t *testing.T) {
	e := NewObjectNode()
	e.SetChild("tags", NewLeafNode(Null))
	i := NewObjectNode()
	i.SetChild("tags", NewArrayNode(NewLeafNode(String)))
	if _, err := Extend(e, i, spi.ChangeLevelType); err != nil {
		t.Fatalf("promoting the nullable marker stays a TYPE-level change; got: %v", err)
	}
}
```

Create `schema/diff_branch_test.go`:

```go
package schema

import "testing"

// diffObject walked children and returned — it never looked at the array
// branch. Harmless only while the gate refused every write that could reach it:
// a widening would have been accepted, merged, and then silently not persisted.
func TestDiff_SeesEveryBranchOfAUnion(t *testing.T) {
	old := NewObjectNode()
	old.SetChild("f", Merge(NewObjectNode(), NewArrayNode(NewLeafNode(Integer))))

	next := NewObjectNode()
	next.SetChild("f", Merge(old.Object().Child("f"),
		Merge(NewObjectNode(), NewArrayNode(NewLeafNode(String)))))

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
	want, _ := Marshal(next)
	got, _ := Marshal(applied)
	if string(want) != string(got) {
		t.Errorf("Apply(old, Diff(old,new)) != new\n  new     = %s\n  applied = %s", want, got)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

```bash
go test ./internal/domain/model/schema/ -run 'TestExtend_Accepts|TestExtend_AddingABranch|TestExtend_NullAgainst|TestDiff_SeesEvery' -v
```
Expected: FAIL — `ErrPolymorphicSlot` on the union cases; `kind change … (not
additive)` on the Diff case.

- [ ] **Step 3: Rewrite `checkAndExtend` as a subset test**

```go
func checkAndExtend(existing, incoming *ModelNode, level spi.ChangeLevel, path string, scalarLevel spi.ChangeLevel) (bool, error)
```

`scalarLevel` is `spi.ChangeLevelType` normally and `spi.ChangeLevelArrayElements`
directly on an array's element — the only thing `checkElementWidening` did
differently — so **delete `checkElementWidening`**: its array-of-array recursion
becomes a recursive call preserving `scalarLevel`, and descent into an object
element's children resets it to `ChangeLevelType`. Body:

1. **Nullability.** `incoming.Nullable() && !existing.Nullable() && existing.Scalar() == nil`
   → require `scalarLevel`; `changed = true`. Keep the message wording
   ("nullable marker at %s requires TYPE level, but level is %q", and the
   ARRAY_ELEMENTS variant on an element).
2. **For each `k` in `incoming.Kinds()`:**
   - `existing` has `k` → recurse into that branch:
     - scalar: types differ → require `scalarLevel`
     - object: new child → `STRUCTURAL`; existing child → recurse with
       `scalarLevel = ChangeLevelType`
     - array: element → recurse with `scalarLevel = ChangeLevelArrayElements`;
       width increase → `ChangeLevelArrayLength` (D11)
   - `existing` has **no** branch at all → require `scalarLevel` (D6)
   - otherwise → require `spi.ChangeLevelStructural`, message
     `"new %s branch at %s requires STRUCTURAL level, but level is %q"`.
3. A branch in `existing` but not `incoming` is not a change — `Extend` is
   additive and `Merge` keeps it.

Delete `ErrPolymorphicSlot`, its doc block and its `TODO(#85)`.

- [ ] **Step 4: Make `Diff` per-branch**

```go
func diffNode(path string, oldN, newN *ModelNode, ops *[]SchemaOp) error {
	// 1. nullability grew            -> broaden_type ["NULL"]
	// 2. scalar branch               -> D7 rows 2-3
	// 3. object branch               -> diffObject, or (Task 7) add_kind_branch
	// 4. array branch                -> diffArray,  or (Task 7) add_kind_branch
	// 5. a branch in old, not in new -> "kind removal at %q is not additive"
}
```

`add_kind_branch` does not exist yet: in this task emit
`fmt.Errorf("adding a %s branch at %q is not yet expressible", …)` on those arms
so the task stays honest. `TestExtend_AddingABranchRequiresStructural` therefore
asserts `Extend` only; `TestDiff_SeesEveryBranchOfAUnion` exercises the
both-present path and must pass now.

- [ ] **Step 5: Convert the sentinel-asserting tests**

`extend_kindmismatch_test.go:29,48`, `extend_polymorphic_error_test.go` (whole
file), `extend_array_element_test.go:53`, `extend_nullable_test.go:143,192` no
longer compile. **Convert, do not delete, every case**: rejected below
`STRUCTURAL` as a level violation naming `STRUCTURAL`; accepted at
`STRUCTURAL`. `extend_nullable_test.go:192`'s "must NOT wrap
ErrPolymorphicSlot" assertion becomes "must name the level that resolves it".

- [ ] **Step 6: Run**

```bash
go test ./internal/domain/model/... ./e2e/parity/...
```
Note: `e2e/parity/oracle.go` recomputes expected SIMPLE_VIEW through
`schema.Extend(…, STRUCTURAL)` and tracks which extensions were **accepted**.
This task deliberately changes which extensions are accepted, so the oracle's
expectations legitimately move. A change there is expected, not a regression —
verify each moved expectation names a branch the model now declares.

- [ ] **Step 7: Commit**

```bash
git add internal/domain/model/schema e2e/parity
git commit -m "fix(model): a write matching a declared branch is accepted

The gate compared one kind per path, so a model with a multi-kind field refused
half its own declared data at every changeLevel — with a message telling the
client to send the declared kind, which is what it sent. The gate is a subset
test over branch sets now, and Diff walks each branch, so diffObject no longer
skips an array branch it would have merged and then silently not persisted."
```

---

### Task 7: `add_kind_branch`

Ends limitation 1, and closes all three D8 defects.

**Files:** `schema/ops.go`, `schema/diff.go`, `schema/apply.go`,
`schema/completeness_test.go`, `schema/axis2_kind_matrix_test.go`, new
`schema/add_kind_branch_test.go`.

**Produces:**

```go
const KindAddKindBranch SchemaOpKind = "add_kind_branch"
func NewAddKindBranch(targetPath string, branch []byte) SchemaOp
```

`Payload` is `schema.Marshal` of a node carrying **exactly one** branch; `Path`
targets the node. No `Name` — the payload names the kind, and storing it twice
is the derived-and-stored duplication this whole change removes.

- [ ] **Step 1: Write the failing test**

Create `schema/add_kind_branch_test.go`:

```go
package schema

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func roundTrip(t *testing.T, old, extended *ModelNode) {
	t.Helper()
	delta, err := Diff(old, extended)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if delta == nil {
		t.Fatal("this is a change; Diff returned a no-op")
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
}

// An entity write can now create a multi-kind declaration; only sample-data
// import could before.
func TestExtendDiffApply_AddsABranchEndToEnd(t *testing.T) {
	cases := []struct {
		name               string
		existing, incoming func() *ModelNode
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
			in := NewObjectNode()
			in.SetChild("f", c.incoming())
			extended, err := Extend(old, in, spi.ChangeLevelStructural)
			if err != nil {
				t.Fatalf("Extend: %v", err)
			}
			roundTrip(t, old, extended)
		})
	}
}

// D8-1, the commonest of the three: an empty array later gets object elements.
// Extend accepted it at every level and Diff then failed, reaching the client
// as a 500.
func TestExtendDiffApply_EmptyArrayThenObjectElements(t *testing.T) {
	for _, c := range []struct {
		name     string
		incoming func() *ModelNode
	}{
		{"object elements", func() *ModelNode {
			e := NewObjectNode()
			e.SetChild("a", NewLeafNode(Integer))
			return NewArrayNode(e)
		}},
		{"mixed object and scalar elements", func() *ModelNode {
			e := NewObjectNode()
			e.SetChild("a", NewLeafNode(Integer))
			return NewArrayNode(Merge(e, NewLeafNode(String)))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			old := NewObjectNode()
			old.SetChild("f", NewArrayNode(NewLeafNode(Null))) // what Walk gives for []
			in := NewObjectNode()
			in.SetChild("f", c.incoming())
			extended, err := Extend(old, in, spi.ChangeLevelArrayElements)
			if err != nil {
				t.Fatalf("Extend: %v", err)
			}
			roundTrip(t, old, extended)
		})
	}
}

// D8-2: the nullable marker gaining a container branch.
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
			in := NewObjectNode()
			in.SetChild("f", c.incoming())
			extended, err := Extend(old, in, spi.ChangeLevelType)
			if err != nil {
				t.Fatalf("promoting the marker keeps the TYPE contract; got: %v", err)
			}
			roundTrip(t, old, extended)
		})
	}
}

// D7 last row: an ARRAY branch with no element at all. The op targets the array
// node, because resolvePath cannot descend a "[]" segment that has no element.
func TestExtendDiffApply_UnobservedElementArrayGainsObjectElements(t *testing.T) {
	old := NewObjectNode()
	old.SetChild("f", NewArrayNode(nil))
	in := NewObjectNode()
	e := NewObjectNode()
	e.SetChild("a", NewLeafNode(Integer))
	in.SetChild("f", NewArrayNode(e))
	extended, err := Extend(old, in, spi.ChangeLevelStructural)
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}
	roundTrip(t, old, extended)
}

func TestApply_AddKindBranchIsIdempotent(t *testing.T) {
	old := NewObjectNode()
	old.SetChild("f", NewLeafNode(String))
	in := NewObjectNode()
	in.SetChild("f", NewArrayNode(NewLeafNode(String)))
	extended, err := Extend(old, in, spi.ChangeLevelStructural)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := Diff(old, extended)
	if err != nil {
		t.Fatal(err)
	}
	once, err := Apply(old, delta)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := Apply(once, delta)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Marshal(once)
	b, _ := Marshal(twice)
	if string(a) != string(b) {
		t.Errorf("replaying add_kind_branch twice changed the model\n  once  = %s\n  twice = %s", a, b)
	}
}

func TestApply_AddKindBranchRejectsAMultiBranchPayload(t *testing.T) {
	base := NewObjectNode()
	base.SetChild("f", NewLeafNode(String))
	multi, err := Marshal(Merge(NewObjectNode(), NewArrayNode(NewLeafNode(String))))
	if err != nil {
		t.Fatal(err)
	}
	delta, err := MarshalDelta([]SchemaOp{NewAddKindBranch("f", multi)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(base, delta); err == nil {
		t.Error("add_kind_branch carries exactly one branch; a multi-branch payload must be rejected")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test ./internal/domain/model/schema/ -run 'TestExtendDiffApply_|TestApply_AddKindBranch' -v
```
Expected: FAIL — `NewAddKindBranch` undefined; the Task-6 "not yet expressible"
arms fire.

- [ ] **Step 3: Add the op to `ops.go`**

Add `KindAddKindBranch` and `NewAddKindBranch` with a doc comment stating: Path
targets the node; Payload is one encoded branch (same encoding as
`schema.Marshal`); ChangeLevel `STRUCTURAL`, except establishing the kinds of a
node that declares none (the nullable marker, an unobserved array element),
which keeps the `TYPE` / `ARRAY_ELEMENTS` contract.

While in the file, **correct the misleading `SchemaOpKind` wire-format note**:
the kind strings are cyoda-go's vocabulary in a plugin-owned table. Plugins keep
deltas opaque and never parse op kinds (`spi.SchemaDelta` is `[]byte`), so there
is nothing to coordinate with a plugin migration story, and `Apply` already
fails closed on an unknown kind.

- [ ] **Step 4: Emit it from `Diff`, replay it in `Apply`**

`diff.go`: replace the Task-6 errors with `NewAddKindBranch(path, encoded)`.
Replace `diffArray`'s `array element materialization … requires LEAF element`
error and its `TODO(A.3 / issue #85)` with an `add_kind_branch` **at the array
node** carrying an ARRAY-branch payload (D7).

`apply.go`: add `applyAddKindBranch` — resolve the path, `Unmarshal` the
payload, require exactly one branch, merge it into the target's branch set via
`Merge` so replay is idempotent. Update `resolvePath` to ask for the branch it
needs (`[]` → `cur.Array()`, a name → `cur.Object()`) instead of comparing a
kind, and name the node's whole kind set in its errors.

- [ ] **Step 5: Extend the op catalog gate**

`schema/completeness_test.go:11-52,107-114` is an explicit gate:
`declaredKinds()` is the authoritative kind list and `catalogCoverage` must
carry one `(old,new)` fixture per kind from which `Diff` emits it and
`Apply(old, Diff)` round-trips. Its comment says a new `SchemaOpKind` without an
entry "will fail `TestDiffCoversCatalog` below, which is the point of this
gate". Add `KindAddKindBranch` to both.

- [ ] **Step 6: Un-skip the axis-2 kind-conflict cells**

In `schema/axis2_kind_matrix_test.go`, change the six `"skip"` cells
(`LO_leaf_to_object`, `LA_leaf_to_array`, `OL_object_to_leaf`,
`OA_object_to_array`, `AL_array_to_leaf`, `AO_array_to_object`) to
`"roundtrip"` and delete `polymorphicSlotIssue`. That matrix is the change's own
statement that limitation 1 is gone.

- [ ] **Step 7: Run**

```bash
go test ./internal/domain/model/... ./e2e/parity/... -v
```
Expected: PASS including `TestDiffCoversCatalog` and `TestAxis2KindMatrix`.

- [ ] **Step 8: Commit**

```bash
git add internal/domain/model/schema
git commit -m "feat(model): an entity write can create a multi-kind declaration

add_kind_branch records a branch being added, at STRUCTURAL — except
establishing the kinds of a node that declares none, which keeps the existing
nullable-marker contract. Replay is a branch union, so it is commutative and
idempotent like every other op.

This also expresses three widenings Extend accepted and Diff could not, each of
which reached the client as a 500: an empty array later holding object
elements, a null-only field later holding a container, and an array whose
element was never observed."
```

---

### Task 8: The property suites must be able to see the new op

`gentree.GenExtensionPair` → `mutateToValue` emits a value of the **same kind**
as the node it mutates at every position — that is why
`TestRoundtripRandomSeeds` can `t.Fatalf` on any `Extend` error over 1000 seeds.
So commutativity, permutation, monotonicity and roundtrip **never generate a
kind conflict and never produce an `add_kind_branch`**. Without this task the
new op's algebraic properties rest on the handful of hand-written cases in Task
7.

**Files:** `schema/gentree/gentree.go`, `schema/gentree/gentree_test.go`,
property test files.

- [ ] **Step 1: Write the failing test**

Add to `schema/gentree/gentree_test.go`:

```go
// The generator must be able to propose a kind the node does not declare,
// or the property suites cannot exercise add_kind_branch at all.
func TestGenExtensionPair_ProducesKindConflicts(t *testing.T) {
	cfg := gentree.DefaultConfig()
	cfg.KindMutationRate = 0.5
	seen := false
	for seed := int64(1); seed <= 200 && !seen; seed++ {
		r := gentree.NewRNG(seed)
		old := gentree.GenModelNode(r, cfg.MaxDepth, cfg.MaxWidth, cfg)
		v := gentree.GenExtensionPair(r, old, spi.ChangeLevelStructural, cfg)
		node, err := importer.Walk(v)
		if err != nil {
			continue
		}
		extended, err := schema.Extend(old, node, spi.ChangeLevelStructural)
		if err != nil {
			continue
		}
		delta, err := schema.Diff(old, extended)
		if err != nil || delta == nil {
			continue
		}
		ops, err := schema.UnmarshalDelta(delta)
		if err != nil {
			continue
		}
		for _, op := range ops {
			if op.Kind == schema.KindAddKindBranch {
				seen = true
			}
		}
	}
	if !seen {
		t.Error("200 seeds at KindMutationRate 0.5 produced no add_kind_branch: the property suites cannot see the new op")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test ./internal/domain/model/schema/gentree/ -run TestGenExtensionPair_ProducesKindConflicts -v
```
Expected: FAIL — `KindMutationRate` does not exist.

- [ ] **Step 3: Add the kind-mutating mode**

Add `KindMutationRate float64` to `GenConfig` (default `0`, so every existing
caller is unchanged). In `mutateToValue`, with probability `KindMutationRate`,
emit a value of a *different* kind than the node declares — a scalar for an
object, a one-element array for a leaf, and so on. Rewrite `mutateToValue`'s
`switch n.Kind()` as a branch walk while here (Task 4 left it on a shim).

- [ ] **Step 4: Turn it on in the property suites**

In `commutativity_property_test.go`, `idempotence_property_test.go`,
`permutation_property_test.go`, `monotonicity_property_test.go` and
`roundtrip_property_test.go`, set `cfg.KindMutationRate = 0.3` for the random-seed
runs. `TestRoundtripRandomSeeds` must keep `t.Fatalf`-ing on an `Extend` error at
`STRUCTURAL`: under this change there is no kind conflict `STRUCTURAL` refuses,
so any error there is a real defect.

- [ ] **Step 5: Run**

```bash
go test ./internal/domain/model/... -v
```
Expected: PASS. A failure here is a genuine algebraic violation of the new op —
investigate it, do not lower the rate.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/model/schema
git commit -m "test(model): the property suites can generate a kind conflict

GenExtensionPair emitted a value of the same kind as the node it mutated at
every position, so commutativity, permutation, monotonicity and roundtrip could
never produce an add_kind_branch and said nothing about it."
```

---

### Task 9: Decommission `POLYMORPHIC_SLOT`

Per D9. Confirmed with the user.

**Files:** `internal/domain/entity/handler.go` (arm + the comment at :225),
`internal/domain/model/ingest/validate.go:97-103` **and the stale rationale at
:105-112**, `internal/common/error_codes.go:34`,
delete `cmd/cyoda/help/content/errors/POLYMORPHIC_SLOT.md`,
`cmd/cyoda/help/content/errors.md:98`,
`cmd/cyoda/help/content/errors/INCOMPATIBLE_TYPE.md:8,42,49`,
`cmd/cyoda/help/content/errors/VALIDATION_FAILED.md:8,40`,
`api/openapi.yaml:3415`, `internal/e2e/zzz_errorcode_matrix_test.go:96`,
delete `internal/domain/entity/handler_polymorphic_test.go`.

- [ ] **Step 1: Remove the constant and every reference**

Delete `common.ErrCodePolymorphicSlot`, the `errors.Is(err, schema.ErrPolymorphicSlot)`
arms in `handler.go` and `ingest/validate.go` (the latter's remaining branch is
the plain `change level violation: %w` wrap), the help topic file, and each doc
reference. In `api/openapi.yaml`, drop the `POLYMORPHIC_SLOT:` clause from the
400 description and leave the rest of the sentence intact.

`INCOMPATIBLE_TYPE.md:42` is a **body bullet** under "This code is distinct
from", not a `see_also` — rewrite it to point at `VALIDATION_FAILED` for the
structural-variant case rather than deleting the distinction.

`ingest/validate.go:105-112`'s unique-key guard comment justifies itself by "the
null-only-leaf → object/array widening case … that would otherwise surface as an
opaque Diff 'kind change' 5xx". That 5xx is gone (D8-2). The guard **stays** —
it is now reachable from an ordinary `STRUCTURAL` write — but its rationale must
be rewritten to say so.

- [ ] **Step 2: Verify the bijection and the matrix**

```bash
go test ./cmd/cyoda/... -run 'TestErrCode|TestSeeAlso' -v
go build ./... && go test ./internal/domain/...
```
`TestErrCode_Parity` (`cmd/cyoda/help/help_test.go:549`) enforces the
constant↔topic bijection and `zzz_errorcode_matrix_test.go:26-27` is
bidirectional, so a leftover on either side fails here.

- [ ] **Step 3: Commit**

```bash
git add internal cmd api
git commit -m "fix(model)!: retire POLYMORPHIC_SLOT

The code meant \"raising changeLevel will not help\". Adding a kind branch is a
STRUCTURAL change now, so raising the level is exactly what resolves it, and the
extension path has no rejection left that the sentinel described. The constant,
the help topic and the OpenAPI clause go with it."
```

---

### Task 10: Coverage on a running backend, over gRPC, and across backends

Per `.claude/rules/test-coverage.md`. HTTP and gRPC are separate entry points and
both must be covered.

**Files:** `internal/e2e/model_kind_enforcement_test.go`, new
`internal/e2e/model_kind_branch_extension_test.go`,
`internal/grpc/model_kind_enforcement_test.go`, `e2e/parity/` + `registry.go`.

- [ ] **Step 1: Fix the existing HTTP e2e case, which now has a split outcome**

`internal/e2e/model_kind_enforcement_test.go:139-149` loops
`ARRAY_LENGTH, ARRAY_ELEMENTS, TYPE, STRUCTURAL` and asserts `400` +
`POLYMORPHIC_SLOT` for all four. At `STRUCTURAL` the write now **succeeds**, so
the `status != 400 → Fatalf` at `:143` changes too. Split the subtest: three
levels → `400 VALIDATION_FAILED` naming `STRUCTURAL`; `STRUCTURAL` → `200`, and
the subsequent export names both branches.

- [ ] **Step 2: Write the new HTTP e2e test**

Create `internal/e2e/model_kind_branch_extension_test.go`, following
`model_kind_enforcement_test.go` for the registration and request helpers. On
real Postgres, over HTTP:

1. `changeLevel=STRUCTURAL`, payload gives a declared scalar field an array
   value → **200**; the `SIMPLE_VIEW` export then names both `".f"` and
   `".f[*]"`.
2. The same write at `changeLevel=TYPE` → **400 VALIDATION_FAILED**, message
   naming `STRUCTURAL`.
3. On a model whose field declares both a scalar and an array (established by a
   sample-data collection import), a write of **either** kind at
   `changeLevel=TYPE` → **200**. This is limitation 2.
4. A *third* kind (object) against that union at `TYPE` → **400**; at
   `STRUCTURAL` → **200**.
5. A field declared only by a `null` observation, later written as an object at
   `changeLevel=TYPE` → **200** (was a 500 — D8-2).
6. A field first written as `[]`, later as `[{"a":1}]`, at
   `changeLevel=ARRAY_ELEMENTS` → **200** (was a 500 — D8-1).
7. Writing `null` to a declared scalar field at `changeLevel=ARRAY_LENGTH` →
   **200** (was a 400 — D8-3).
8. `changeLevel` unset: a write of a kind outside the declared set → **400
   VALIDATION_FAILED** (unchanged — the strict door is untouched).
9. A `STRUCTURAL` write that adds a container branch to a **unique-key** field →
   **422 INVALID_UNIQUE_KEY_DEFINITION** (D9: newly reachable from an entity
   write).

- [ ] **Step 3: Run**

```bash
go test ./internal/e2e/ -run 'ModelKindBranch|ModelKindEnforcement' -v
```
Docker must be running.

- [ ] **Step 4: gRPC**

`internal/grpc/entity.go:58` calls the shared `entityHandler.CreateEntity`, so
`classifyValidateOrExtendErr` runs identically to HTTP and the envelope carries
`CLIENT_ERROR` with the domain code in the message —
`internal/grpc/model_kind_enforcement_test.go:124` already asserts
`strings.Contains(typed.Error.Message, "VALIDATION_FAILED")`. Add three cases to
that file: a branch add refused below `STRUCTURAL`, accepted at `STRUCTURAL`,
and a declared-branch write accepted at `TYPE`.

```bash
go test ./internal/grpc/ -run ModelKind -v
```

- [ ] **Step 5: Cross-backend parity**

All of this sits above the SPI, so add one scenario — `ModelKindBranchExtension`
— asserting items 1 and 3, and register it in `e2e/parity/registry.go` beside
`ModelKindEnforcementRejected` (`:213`). Read an existing model scenario for the
shape.

```bash
go test ./e2e/parity/... -v
```
Expected: PASS on memory, sqlite and postgres. (`ok … 1.9s` means skipped —
never pass `-short` here.)

- [ ] **Step 6: Commit**

```bash
git add internal/e2e internal/grpc e2e/parity
git commit -m "test(model): cover branch extension over HTTP, gRPC and every backend"
```

---

### Task 11: Documentation

Gate 4 and Gate 7.

**Files:** `docs/cloud-parity/{model-kind-enforcement,validation-failure-code,README}.md`,
`cmd/cyoda/help/content/models.md`, `api/openapi.yaml`, `CHANGELOG.md`,
`COMPATIBILITY.md`.

- [ ] **Step 1: `docs/cloud-parity/model-kind-enforcement.md`**

- Delete the "**Known limitation of the `changeLevel` door in cyoda-go**" bullet.
- Rewrite "**Both write doors reject; the codes differ deliberately**": the
  extension door now *accepts* a new kind at `STRUCTURAL` and refuses it below
  as a plain change-level violation; the strict door is unchanged and still
  answers `VALIDATION_FAILED`.
- In "not a ban on polymorphic fields", delete the sentence about the union
  folding onto a node "whose *dominant* kind is OBJECT" — there is no dominant
  kind. Replace with: a node records the set of kinds the field was observed as,
  and every surface — validation, the field walk, both exports, and a
  self-executing backend's own flattening — reads the set.
- Update **Coverage** with the new e2e file, the gRPC cases and the parity
  scenario.
- Present tense, a reference not a history
  (`feedback_architecture_md_is_reference_not_history`).

- [ ] **Step 2: `cmd/cyoda/help/content/models.md`**

- Line 56: with a `changeLevel` set, a payload proposing a kind the field does
  not declare is a schema change permitted at `STRUCTURAL` and refused below it
  as a change-level violation.
- Line 58: delete "**With a `changeLevel` set, a write matching any but the
  field's dominant kind is still refused** … Leave `changeLevel` unset on a model
  with multi-kind fields." Replace with: every declared kind is admissible at any
  `changeLevel`; a new kind is added at `STRUCTURAL`.
- **Line 125**, the level hierarchy the settled decision cites: `STRUCTURAL` —
  "allows fundamental model changes including new fields" → name the new-kind
  case too.
- Compact — the actionable core only (`feedback_compact_prose`).

- [ ] **Step 3: `api/openapi.yaml`**

Beyond the `:3415` clause removed in Task 9, the `changeLevel` descriptions at
`:52`, `:257` and `:4520` say `STRUCTURAL` "permits/allows fundamental model
changes". Name the new-kind case in the same words used in `models.md:125`.

- [ ] **Step 4: `docs/cloud-parity/validation-failure-code.md` and `README.md`**

Drop the `POLYMORPHIC_SLOT` row / mention. The remaining rows are unchanged.

- [ ] **Step 5: CHANGELOG — correct two statements, then add**

`CHANGELOG.md:467-470` and `:483-485` are **inside the same `## [Unreleased]`
v0.8.4 section** and currently promise the behaviour this change removes:

- `:467-470` — "Not changed here: … it still refuses a write matching a declared
  but non-dominant branch with `POLYMORPHIC_SLOT`. Leave `changeLevel` unset on a
  model with multi-kind fields." → **rewrite**: the extension path accepts every
  declared branch, and adds a new one at `STRUCTURAL`.
- `:483-485` — "`INCOMPATIBLE_TYPE` … and `POLYMORPHIC_SLOT` … are unchanged" →
  **rewrite** to name `INCOMPATIBLE_TYPE` only.

Shipping v0.8.4 with an entry that both promises and retires the same code is a
defect, not history. Then add:

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
- Three widenings that reached the client as a `500` are now expressible: a
  field first written as `[]` and later holding object elements, a field
  observed only as `null` and later holding a container, and an array whose
  element was never observed.
- Writing `null` to a field declared as a scalar no longer requires `TYPE`
  level: it proposes no schema change.
- A search against a field observed as more than one kind no longer silently
  returns fewer rows on a backend that executes searches itself. That
  executor's own schema decoder dispatched on a single kind label and dropped
  the other branches, so a predicate on them found no declared type and matched
  nothing.
```

- [ ] **Step 6: `COMPATIBILITY.md`**

Gate 4 names it explicitly, and its v0.8.4 row already carries the precedent
verbatim for the sqlite scan-budget removal. Add: the `POLYMORPHIC_SLOT`
retirement, the advanced `cyoda-go-spi` pin, the SPI surface change
(`ModelNode` accessors, `"kinds"` wire spelling), and the resulting obligation
on the out-of-tree commercial backend.

- [ ] **Step 7: Verify the help tree**

```bash
go test ./cmd/cyoda/... && go run ./cmd/cyoda help errors && go run ./cmd/cyoda help models
```
Expected: PASS; no dangling `POLYMORPHIC_SLOT` topic.

- [ ] **Step 8: Commit**

```bash
git add docs cmd/cyoda/help api/openapi.yaml CHANGELOG.md COMPATIBILITY.md
git commit -m "docs(model): a node declares a set of kinds, and a write can add one"
```

---

### Task 12: Tag the SPI, bump the pin, verify

Per `MAINTAINING.md` → "Coordinated release" and "Bumping cyoda-go-spi".

- [ ] **Step 1: Land the SPI**

Open and merge the `cyoda-go-spi` PR (`feat/model-node-branch-set`). Note
`KNOWN_CONSUMERS.md` lists `cyoda-platform/cyoda-go-cassandra` as a consumer
that asked to be notified before breaking changes ship — follow the
`MAINTAINING.md#deprecation-policy` etiquette.

- [ ] **Step 2: Tag it**

Pick the next version by whether the **SPI interface** broke — it did — never to
match the binary's. Never force-move an existing tag.

- [ ] **Step 3: Bump the pin in one commit, and remove the local `use` line**

```bash
# in the cyoda-go worktree
git checkout go.work                     # drop the uncommitted local use line
# bump github.com/cyoda-platform/cyoda-go-spi to the new tag in:
#   go.mod, plugins/memory/go.mod, plugins/postgres/go.mod, plugins/sqlite/go.mod
go mod tidy   # and in each plugin submodule
make check-spi-pin-sync
git add go.mod go.sum plugins/*/go.mod plugins/*/go.sum
git status --short                        # go.work MUST be clean
git commit -m "build: pin cyoda-go-spi <version>"
```

- [ ] **Step 4: Full suite**

```bash
go test ./... 2>&1 | tail -60
make test-all
```
Expected: all packages `ok`, root and all three plugin submodules. Docker
required. `plugins/*` reference the schema package only in a comment
(`plugins/sqlite/store_factory.go:33`) and `spi.SchemaDelta` is opaque `[]byte`,
so a failure here means the SPI surface moved unintentionally.

- [ ] **Step 5: Vet, format, race**

```bash
go vet ./... && gofmt -l . && make race
```
Expected: no output from `gofmt -l`; race clean. (`make race` is a one-shot
pre-PR check, not a per-step gate.)

- [ ] **Step 6: Reviews**

Dispatch a fresh-context reviewer per `superpowers:requesting-code-review` —
CLAUDE.md makes this a standing request, and running it inline defeats it — then
`antigravity-bundle-security-developer:cc-skill-security-review`.

- [ ] **Step 7: PRs**

Base branch **`release/v0.8.4`** (milestone v0.8.4 — verify the merge-base
before `gh pr create`). Body says `Closes #534`; apply the milestone at PR time.
`gh pr merge` collides with worktrees — use
`gh api -X PUT .../pulls/N/merge` then
`gh api -X DELETE .../git/refs/heads/<branch>`.

- [ ] **Step 8: Re-read #85 against this design**

The issue asks for it: #85 keeps Cloud's tagged-union parity and whatever else
remains of its scope, and its open design point 1 (the schema-tree
representation) is answered here. Post a comment on #85 recording that, and
remove the now-stale `TODO(#85)` references from the code if Task 6 left any
(`grep -rn 'TODO(#85)\|issue #85' --include='*.go' .`).
