# #431 Phase 0 — Relocate the type-comparison core to the SPI — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the precise type-comparison core (`DataType`, `Decimal`, numeric classifiers, `IsAssignableTo`) from `internal/domain/model/schema` into the `cyoda-go-spi` module so the future search kernel (which lives in the SPI) can do precise, type-directed comparison — with zero behaviour change.

**Architecture:** The three self-contained core files (`types.go`, `decimal.go`, `numeric.go` — stdlib-only imports) move into the flat `spi` package in the sibling `cyoda-go-spi` repo. `internal/domain/model/schema` keeps all model-tree/discovery/diff/apply logic and gains a thin **re-export alias** file so every existing `schema.DataType` / `schema.Integer` / `schema.Decimal` / `schema.ParseDecimal` / `schema.ClassifyInteger` reference keeps compiling untouched. This is a **coordinated SPI release**: SPI commits land first, then a single pseudo-version pin bump in cyoda-go, composed locally via `go.work`.

**Tech Stack:** Go 1.26+, two modules (`github.com/cyoda-platform/cyoda-go`, `github.com/cyoda-platform/cyoda-go-spi`), `go.work` for local composition.

## Global Constraints

- Go 1.26+. Use `log/slog` only; wrap errors with `fmt.Errorf("...: %w", err)`; `uuid.UUID` not `string`.
- **Coordinated SPI release** (`MAINTAINING.md`): SPI commits FIRST, then one pin bump across all four `go.mod` manifests + `make repin-plugins`. Compose locally via `go.work` — the local SPI `use` line stays **uncommitted** (go.work is tracked in CI-safe form); **never `git add -A`** (it would commit the absolute-path `use` line and break CI). Real `cyoda-go-spi` tag is deferred to milestone-end; use a pseudo-version pin now.
- **SPI work lands on a feature branch, not `main`** (per Paul): SPI commits go on `feat/431-cloud-aligned-search` (branched from `main`); cyoda-go pseudo-pins to that branch's commit SHA (a pseudo-version resolves to a commit regardless of branch). **Do not push the SPI branch or merge it to `main`** without explicit sign-off — local `go.work` composition keeps tests green meanwhile.
- No issue IDs (`#NNN`) in shipped artefacts (code, comments). Issue refs only in commits/PR bodies/spec docs.
- Pure relocation — **no behaviour change**. Success = the full existing suite (root + every plugin submodule) stays green.
- Local SPI checkout is at `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`. cyoda-go worktree is the current directory.

## File Structure

**cyoda-go-spi (new files, moved verbatim):**
- Create `datatype.go` — the `DataType` enum + names + `ParseDataType` (from `schema/types.go`).
- Create `decimal.go` — the `Decimal` type + `ParseDecimal` + methods (from `schema/decimal.go`).
- Create `numeric.go` — `IsNumeric`, `ClassifyInteger`, `ClassifyDecimal`, `IsAssignableTo`, the widening lattice, `CollapseNumeric` (from `schema/numeric.go`).
- Create `datatype_test.go`, `decimal_test.go`, `numeric_test.go` — the moved tests, repackaged for `spi`.

**cyoda-go (modify):**
- Delete `internal/domain/model/schema/types.go`, `decimal.go`, `numeric.go` and their `_test.go` files.
- Create `internal/domain/model/schema/coretypes.go` — the re-export alias shim.
- Modify the four `go.mod` files (root + `plugins/memory|postgres|sqlite`) — bump the SPI pin.
- `go.work` — local `use ../cyoda-go-spi` line (uncommitted).

---

### Task 1: Move the type-core files into the SPI (in the cyoda-go-spi repo)

**Files:**
- Create (in `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`): `datatype.go`, `decimal.go`, `numeric.go` + their test files.

**Interfaces:**
- Produces (now under package `spi`): `spi.DataType` (+ all const members `Integer`…`Null`), `spi.ParseDataType(string) (DataType, bool)`, `spi.Decimal` (+ `ParseDecimal(string) (Decimal, error)`, `(Decimal).Cmp`, `.Scale`, `.Precision`, `.SetScale`, `.Canonical`, `.IsInt128`, `.StripTrailingZeros`, …), `spi.IsNumeric(DataType) bool`, `spi.ClassifyInteger(*big.Int) DataType`, `spi.ClassifyDecimal(Decimal) DataType`, `spi.IsAssignableTo(dataT, schemaT DataType) bool`, `spi.CollapseNumeric(...)`.

- [ ] **Step 1: Branch the SPI off main, then copy the three core files in, rewriting the package clause**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git checkout main && git checkout -b feat/431-cloud-aligned-search   # SPI feature branch (do not push without sign-off)
SRC=/Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat+431-evaluator-convergence/internal/domain/model/schema
for f in types decimal numeric; do
  # datatype.go is the new name for types.go; decimal/numeric keep their names
  dst=$f.go; [ "$f" = types ] && dst=datatype.go
  sed 's/^package schema$/package spi/' "$SRC/$f.go" > "$dst"
done
ls -1 datatype.go decimal.go numeric.go
```

- [ ] **Step 2: Move the tests too, repackaging for the SPI**

The three test files are `types_test.go`, `decimal_test.go`, `numeric_test.go`. Inspect their package clause first (`head -1`): a white-box test is `package schema` → becomes `package spi`; a black-box test is `package schema_test` and imports `.../internal/domain/model/schema` → becomes `package spi_test` importing `github.com/cyoda-platform/cyoda-go-spi` with every `schema.` qualifier rewritten to `spi.`.

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
# White-box (package schema) → package spi:
for f in types decimal numeric; do
  dst=${f}_test.go; [ "$f" = types ] && dst=datatype_test.go
  if head -1 "$SRC/${f}_test.go" | grep -q 'package schema$'; then
    sed 's/^package schema$/package spi/' "$SRC/${f}_test.go" > "$dst"
  else
    # Black-box: package schema_test importing the schema pkg → spi_test importing spi.
    sed -e 's/^package schema_test$/package spi_test/' \
        -e '\#cyoda-platform/cyoda-go/internal/domain/model/schema#d' \
        -e 's/\bschema\./spi./g' "$SRC/${f}_test.go" > "$dst"
    # spi_test needs the spi import; add it if the sed removed the only import.
  fi
done
```
Then open each new `*_test.go` and ensure the import block is correct: a `spi_test` file must `import spi "github.com/cyoda-platform/cyoda-go-spi"`; a `spi` white-box file needs no self-import. Fix any dangling import lines by hand.

- [ ] **Step 3: Build + test the SPI in isolation**

Run:
```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go build ./... && go test ./... 2>&1 | tail -20
```
Expected: PASS (the moved tests now exercise `spi.DataType`/`spi.Decimal`/…). If a black-box test fails to compile on an import, fix the import block (Step 2 note) and re-run.

- [ ] **Step 4: Vet + gofmt the SPI**

Run:
```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go vet ./... && gofmt -l datatype.go decimal.go numeric.go
```
Expected: no vet output; `gofmt -l` prints nothing (files already formatted).

- [ ] **Step 5: Commit in the SPI repo**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add datatype.go decimal.go numeric.go datatype_test.go decimal_test.go numeric_test.go
git commit -m "feat(types): relocate DataType/Decimal/numeric-classifier core into the SPI

Precise type-comparison core moved here so the search leaf-comparison kernel
(which lives in the SPI) can do type-directed, precise comparison. cyoda-go's
schema package re-exports these. Pure relocation, no behaviour change.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
git rev-parse --short HEAD
```
Record the commit SHA — the pin bump (Task 3) needs it.

---

### Task 2: Replace the moved files with a re-export shim in cyoda-go

**Files:**
- Delete: `internal/domain/model/schema/types.go`, `decimal.go`, `numeric.go`, `types_test.go`, `decimal_test.go`, `numeric_test.go`.
- Create: `internal/domain/model/schema/coretypes.go`.

**Interfaces:**
- Produces: unchanged public surface `schema.DataType`, `schema.Integer`…`schema.Null`, `schema.Decimal`, `schema.ParseDecimal`, `schema.ParseDataType`, `schema.IsNumeric`, `schema.ClassifyInteger`, `schema.ClassifyDecimal`, `schema.IsAssignableTo`, `schema.CollapseNumeric` — now aliases into `spi`. Every existing in-package and cross-package reference keeps compiling.
- Consumes: the `spi` symbols from Task 1.

- [ ] **Step 1: Delete the moved files from the schema package**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat+431-evaluator-convergence
git rm internal/domain/model/schema/types.go internal/domain/model/schema/decimal.go \
       internal/domain/model/schema/numeric.go internal/domain/model/schema/types_test.go \
       internal/domain/model/schema/decimal_test.go internal/domain/model/schema/numeric_test.go
```

- [ ] **Step 2: Write the re-export shim**

Create `internal/domain/model/schema/coretypes.go`. This aliases the relocated core so `schema.X` continues to resolve. Type aliases preserve method sets; `const` re-declarations copy each enum value; `var` function-aliases forward the functions.

```go
package schema

import spi "github.com/cyoda-platform/cyoda-go-spi"

// The precise type-comparison core (DataType, Decimal, numeric classifiers,
// assignability) now lives in the SPI so the search leaf-comparison kernel can
// reach it (the SPI module cannot import this internal package). These aliases
// preserve the schema.X surface for the model-tree/discovery code that still
// lives here.

// DataType and its members.
type DataType = spi.DataType

const (
	Integer        = spi.Integer
	Long           = spi.Long
	BigInteger     = spi.BigInteger
	UnboundInteger = spi.UnboundInteger
	Double         = spi.Double
	BigDecimal     = spi.BigDecimal
	UnboundDecimal = spi.UnboundDecimal
	String         = spi.String
	Character      = spi.Character
	LocalDate      = spi.LocalDate
	LocalDateTime  = spi.LocalDateTime
	LocalTime      = spi.LocalTime
	ZonedDateTime  = spi.ZonedDateTime
	Year           = spi.Year
	YearMonth      = spi.YearMonth
	UUIDType       = spi.UUIDType
	TimeUUIDType   = spi.TimeUUIDType
	ByteArray      = spi.ByteArray
	Boolean        = spi.Boolean
	Null           = spi.Null
)

// ParseDataType resolves a DataType by name.
var ParseDataType = spi.ParseDataType

// Decimal and its constructor.
type Decimal = spi.Decimal

var ParseDecimal = spi.ParseDecimal

// Numeric classification and assignability.
var (
	IsNumeric       = spi.IsNumeric
	ClassifyInteger = spi.ClassifyInteger
	ClassifyDecimal = spi.ClassifyDecimal
	IsAssignableTo  = spi.IsAssignableTo
	CollapseNumeric = spi.CollapseNumeric
)
```

> Note: verify the exact exported symbol set the deleted files provided — run `git show HEAD:internal/domain/model/schema/numeric.go | grep -E '^func [A-Z]'` (and same for `types.go`, `decimal.go`) and ensure every exported func/type/const has an alias above. Add any missing ones (e.g. additional exported `Decimal` helper constructors or numeric constants). Unexported symbols do **not** need aliasing unless referenced across files in this package — if a now-deleted unexported helper was used elsewhere in `schema`, either it moved with the core (make it exported in the SPI + alias it) or it belongs to the discovery code (keep a copy here).

- [ ] **Step 3: Compose the local SPI and bump the pin (see Task 3), then build**

The build can't succeed until `go.work`/pin point at the new SPI. Do Task 3 first, then return here. After Task 3:

Run:
```bash
go build ./... 2>&1 | head -30
```
Expected: clean build. Compile errors here reveal a missing alias in `coretypes.go` (add it) or an unexported cross-file dependency (resolve per the Step 2 note).

- [ ] **Step 4: Run the full root-module test suite**

Run:
```bash
go test -short ./... 2>&1 | tail -25
```
Expected: PASS. (`-short` skips the Docker-backed e2e for speed during iteration; the full run happens in Task 4.)

- [ ] **Step 5: Commit the shim (defer until Task 3's pin bump is staged — commit together)**

Held — the shim does not compile without Task 3's pin. Task 3 stages the pin; commit both together at the end of Task 3.

---

### Task 3: Compose the SPI locally + bump the pseudo-version pin

**Files:**
- Modify: `go.work` (local, uncommitted `use` line), `go.mod` (root) + `plugins/{memory,postgres,sqlite}/go.mod` (pin).

- [ ] **Step 1: Add the local SPI to go.work (uncommitted)**

Confirm `go.work` currently has no SPI line (CI-safe form), then add it locally. **Do not stage go.work.**
```bash
cat go.work
go work edit -use ../../../../cyoda-go-spi   # relative path from this worktree to the SPI checkout
# Verify the path resolves:
go work edit -json | grep cyoda-go-spi
```
> The relative path depends on worktree depth. Compute it: from `.../cyoda-go/.claude/worktrees/feat+431-evaluator-convergence` to `.../cyoda-go-spi` is `../../../../cyoda-go-spi`. Verify with `ls ../../../../cyoda-go-spi/go.mod`.

- [ ] **Step 2: Bump the SPI pseudo-version pin across all four go.mod files**

Use the SPI commit SHA from Task 1 Step 5. Generate the pseudo-version and apply it (the repo has a helper — check `make repin-plugins` / `Makefile`; otherwise set it manually):
```bash
SPI_SHA=<sha-from-task1>
# Preferred: repo helper keeps all four manifests + plugins in lockstep.
make repin-plugins SPI_REF=$SPI_SHA 2>/dev/null || {
  echo "No helper target; set the pin manually in root + 3 plugin go.mod files";
}
grep cyoda-go-spi go.mod
```
> If setting manually: the pin is `v0.8.3-0.<UTC-timestamp>-<12charSHA>`. Match the existing pseudo-version format already in `go.mod`. Keep root and all three `plugins/*/go.mod` identical.

- [ ] **Step 3: Build + short-test the root module and each plugin**

Run:
```bash
go build ./... && go test -short ./... 2>&1 | tail -15
for p in memory postgres sqlite; do (cd plugins/$p && go build ./... && go test -short ./... 2>&1 | tail -5); done
```
Expected: all PASS. (Plugins resolve the SPI via the pin + go.work locally.)

- [ ] **Step 4: Verify go.work was NOT staged**

```bash
git status --short go.work
```
Expected: **empty** (go.work unchanged in the index). If it shows as modified-and-staged, `git restore --staged go.work` — the local `use` line must never be committed.

- [ ] **Step 5: Commit the shim + pin bump together (cyoda-go)**

Stage explicitly (never `git add -A`):
```bash
git add internal/domain/model/schema/coretypes.go go.mod plugins/memory/go.mod plugins/postgres/go.mod plugins/sqlite/go.mod
git status --short   # confirm go.work is NOT listed
git commit -m "refactor(schema): re-export the type-core from the SPI; bump SPI pin

types.go/decimal.go/numeric.go relocated to cyoda-go-spi; schema now aliases
them (schema.DataType = spi.DataType, etc.). Pure relocation, no behaviour change.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Full-suite verification (root + all plugins + e2e)

**Files:** none (verification only).

- [ ] **Step 1: Full root suite incl. Docker-backed e2e**

Run (Docker must be running):
```bash
go test ./... 2>&1 | tail -30
```
Expected: PASS (includes `internal/e2e`).

- [ ] **Step 2: Cross-module aggregate suite**

Run:
```bash
make test-all 2>&1 | tail -30
```
Expected: PASS across root + `plugins/memory|sqlite|postgres`.

- [ ] **Step 3: Vet + gofmt across the repo**

Run:
```bash
go vet ./... && gofmt -l internal/domain/model/schema/coretypes.go
for p in memory postgres sqlite; do (cd plugins/$p && go vet ./...); done
```
Expected: no output.

- [ ] **Step 4: Confirm the public surface is unchanged**

Spot-check that a representative consumer still references `schema.X` and compiles (no consumer edits were needed):
```bash
grep -rl "schema.ClassifyInteger\|schema.Decimal\|schema.IsAssignableTo\|schema.DataType" internal/ | head
go build ./... # already green, re-confirm
```
Expected: consumers unchanged, build green. This is the proof of "pure relocation."

- [ ] **Step 5: Race sanity (one-shot, pre-handoff)**

Run:
```bash
make race 2>&1 | tail -15
```
Expected: PASS (no new races; this is a type relocation).

---

## Self-Review

1. **Spec coverage:** This plan implements spec §14 **Phase 0** in full (relocate `DataType`, `Decimal`, numeric classifiers, `IsAssignableTo` to the SPI; re-export from `internal/schema`; coordinated release; tests green). No other spec section is in scope for Phase 0.
2. **Placeholder scan:** none — every step has exact commands. The one judgment point (exact exported-symbol set to alias) is handled by the `git show | grep '^func [A-Z]'` verification in Task 2 Step 2, not left vague.
3. **Type consistency:** the alias names in `coretypes.go` (`DataType`, `Decimal`, `ParseDecimal`, `ClassifyInteger`, `ClassifyDecimal`, `IsAssignableTo`, `IsNumeric`, `CollapseNumeric`, `ParseDataType`, enum members) match the Task-1 Produces list and the current schema surface.
4. **Coordinated-release safety:** go.work stays uncommitted (Task 3 Steps 4); explicit staging, never `git add -A`; SPI commit precedes the pin bump.

## Notes for the next plan (Phase 1)
Once this lands, `spi.DataType`/`spi.Decimal`/`spi.ClassifyInteger`/`spi.IsAssignableTo`/`spi.ParseDecimal` are available to the SPI kernel. Phase 1 (lossless operand capture in `predicate/parse.go` + stored-value classification from `gjson.Raw`) builds directly on them.
