# Search Result Sorting by Field Paths Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let search clients sort results by scalar data fields and a closed set of meta fields, with byte-for-byte identical ordering across memory, sqlite, and postgres.

**Architecture:** A `sort` query param (HTTP) and an `orderBy` array (gRPC) parse to `[]search.OrderKey`; the service resolves each key against the model schema into `[]spi.OrderSpec`, attaching an **ordering class** (`Kind`) that fixes the comparison. SQL backends render `Kind`-specific `ORDER BY` (numeric cast / `COLLATE` byte order / `::timestamptz` / `NULLS LAST` + `entity_id` tiebreaker); the memory backend and the in-transaction/untranslatable fallback sort in Go with the identical semantic.

**Tech Stack:** Go 1.26+, `log/slog`, sqlite (`json_extract`), postgres (`jsonb`), oapi-codegen, CloudEvents gRPC, testcontainers-go for e2e.

## Global Constraints

- Go 1.26+. `log/slog` only — never `log.Printf`/`fmt.Printf`. Wrap errors `fmt.Errorf("...: %w", err)`. `uuid.UUID` not `string`. Config via `CYODA_`-prefixed env with defaults.
- 4xx: full domain detail + error code. 5xx: generic message + ticket UUID. Bad sort input ⇒ `400` code **`INVALID_FIELD_PATH`** (existing; no new error code, no new `errors/*.md`).
- No issue IDs (`#NNN`) in shipped artefacts (error messages, logs, response bodies, code comments, OpenAPI/help content). Issue IDs only in commits/PR/spec.
- TDD: every change is RED → GREEN → REFACTOR. Commit per task.
- Canonical ordering semantic (spec §4): classes **Text** (byte order: sqlite `COLLATE BINARY`, postgres `COLLATE "C"`, Go `bytes.Compare`), **Numeric** (IEEE-754 double), **Bool** (`false<true`), **Temporal** (engine meta dates, chronological). NULL/missing sorts **last** (both directions). Final tiebreaker `entity_id asc`, skipped when the terminal key is the entity id.
- Meta sort allowlist (canonical client names): `state`, `creationDate`, `lastUpdateTime`, `transitionForLatestSave`, `transactionId`, `id`.
- Plugin submodules have their own `go.mod`; test them explicitly (`plugins/sqlite`, `plugins/postgres`, `plugins/memory`).
- SPI changes compose locally via `go.work`; the pseudo-version pin bump is a release step (Task 19), not per-task.

---

### Task 1: SPI — `OrderKind` and `OrderSpec.Kind`

**Files:**
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/searcher.go`
- Test: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/searcher_test.go` (create if absent)

**Interfaces:**
- Produces: `spi.OrderKind` (`OrderText`=0, `OrderNumeric`, `OrderBool`, `OrderTemporal`); `spi.OrderSpec.Kind OrderKind`.

- [ ] **Step 1: Write the failing test**

In `cyoda-go-spi/searcher_test.go`:
```go
package spi

import "testing"

func TestOrderKind_ZeroValueIsText(t *testing.T) {
	var k OrderKind
	if k != OrderText {
		t.Fatalf("zero OrderKind = %v, want OrderText", k)
	}
}

func TestOrderSpec_CarriesKind(t *testing.T) {
	s := OrderSpec{Path: "price", Source: SourceData, Desc: true, Kind: OrderNumeric}
	if s.Kind != OrderNumeric {
		t.Fatalf("Kind = %v, want OrderNumeric", s.Kind)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./... -run TestOrderKind -v`
Expected: FAIL (`OrderKind`/`Kind` undefined).

- [ ] **Step 3: Write minimal implementation**

In `cyoda-go-spi/searcher.go`, add the type and field. Update the `OrderSpec` doc comment to list canonical meta names and the class contract:
```go
// OrderKind selects the canonical comparison applied to a sort key so that
// every backend (memory, sqlite, postgres, commercial) produces identical
// ordering. The zero value is OrderText (byte-order string comparison).
type OrderKind int

const (
	OrderText     OrderKind = iota // byte order: BINARY / COLLATE "C" / bytes.Compare
	OrderNumeric                   // IEEE-754 double
	OrderBool                      // false < true
	OrderTemporal                  // chronological instant (engine meta dates only)
)

// OrderSpec is one sort key. Path is a scalar leaf: a dotted data path
// (Source=SourceData) or a canonical meta field name (Source=SourceMeta) —
// one of: state, creationDate, lastUpdateTime, transitionForLatestSave,
// transactionId, id. Kind fixes the cross-backend comparison. Absent/null
// values sort last. When OrderBy is empty the default order is entity_id asc.
type OrderSpec struct {
	Path   string
	Source FieldSource
	Desc   bool
	Kind   OrderKind
}
```
(`searcher.go` has a single `OrderSpec` — edit it in place; update its doc comment as shown. Do not add a second definition.)

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./... -run 'TestOrderKind|TestOrderSpec' -v`
Expected: PASS.

- [ ] **Step 5: Verify cyoda-go still builds against the local SPI (go.work)**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-search-sort && go build ./...`
Expected: builds (go.work resolves the local SPI edit).

- [ ] **Step 6: Commit (SPI repo)**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add searcher.go searcher_test.go
git commit -m "feat(searcher): add OrderKind to OrderSpec for canonical cross-backend sort ordering"
```

---

### Task 2: Domain — ordering-class resolution helpers

Maps schema types and meta field names to `spi.OrderKind` + canonical paths. Pure functions, no I/O.

**Files:**
- Create: `internal/domain/search/orderclass.go`
- Test: `internal/domain/search/orderclass_test.go`

**Interfaces:**
- Consumes: `schema.DataType`, `schema.IsNumeric` (`internal/domain/model/schema`), `spi.OrderKind`, `spi.FieldSource`.
- Produces:
  - `func classifyType(types []schema.DataType) (spi.OrderKind, error)` — single ordering class for a leaf's declared types, or error if non-sortable / class-disagreeing union.
  - `metaField struct { Source spi.FieldSource; Path string; Kind spi.OrderKind }`
  - `func resolveMetaField(name string) (metaField, bool)` — canonical meta allowlist lookup.

- [ ] **Step 1: Write the failing test**

```go
package search

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi"
	"<MODULE>/internal/domain/model/schema"
)

func TestClassifyType(t *testing.T) {
	cases := []struct {
		name  string
		in    []schema.DataType
		want  spi.OrderKind
		isErr bool
	}{
		{"int", []schema.DataType{schema.Integer}, spi.OrderNumeric, false},
		{"double", []schema.DataType{schema.Double}, spi.OrderNumeric, false},
		{"numeric union same class", []schema.DataType{schema.Integer, schema.Long}, spi.OrderNumeric, false},
		{"string", []schema.DataType{schema.String}, spi.OrderText, false},
		{"uuid", []schema.DataType{schema.UUIDType}, spi.OrderText, false},
		{"localdate is text", []schema.DataType{schema.LocalDate}, spi.OrderText, false},
		{"year is text", []schema.DataType{schema.Year}, spi.OrderText, false},
		{"yearmonth is text", []schema.DataType{schema.YearMonth}, spi.OrderText, false},
		{"bool", []schema.DataType{schema.Boolean}, spi.OrderBool, false},
		{"nullable string", []schema.DataType{schema.String, schema.Null}, spi.OrderText, false},
		{"bytearray rejected", []schema.DataType{schema.ByteArray}, 0, true},
		{"disagreeing union rejected", []schema.DataType{schema.Integer, schema.String}, 0, true},
		{"null only rejected", []schema.DataType{schema.Null}, 0, true},
		{"empty rejected", nil, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := classifyType(c.in)
			if c.isErr {
				if err == nil {
					t.Fatalf("want error, got Kind=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("Kind = %v, want %v", got, c.want)
			}
		})
	}
}

func TestResolveMetaField(t *testing.T) {
	mf, ok := resolveMetaField("creationDate")
	if !ok || mf.Source != spi.SourceMeta || mf.Kind != spi.OrderTemporal || mf.Path != "creationDate" {
		t.Fatalf("creationDate resolved to %+v ok=%v", mf, ok)
	}
	if _, ok := resolveMetaField("state"); !ok {
		t.Fatal("state should resolve")
	}
	if _, ok := resolveMetaField("nope"); ok {
		t.Fatal("unknown meta field must not resolve")
	}
	if _, ok := resolveMetaField("label.position.x"); ok {
		t.Fatal("nested meta path must not resolve")
	}
}
```
Replace `<MODULE>` with the module path from `go.mod` (first `grep '^module' go.mod`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/search/ -run 'TestClassifyType|TestResolveMetaField' -v`
Expected: FAIL (`classifyType`/`resolveMetaField` undefined).

- [ ] **Step 3: Write minimal implementation**

`internal/domain/search/orderclass.go`:
```go
package search

import (
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"<MODULE>/internal/domain/model/schema"
)

// classifyType returns the single canonical ordering class for a leaf's
// declared types. Null members are ignored (nullable fields are fine). The
// remaining members must all map to the same class, else there is no
// deterministic order and the field is unsortable.
func classifyType(types []schema.DataType) (spi.OrderKind, error) {
	var (
		have bool
		kind spi.OrderKind
	)
	for _, t := range types {
		if t == schema.Null {
			continue
		}
		k, err := scalarClass(t)
		if err != nil {
			return 0, err
		}
		if !have {
			kind, have = k, true
			continue
		}
		if k != kind {
			return 0, fmt.Errorf("field has mixed ordering classes and cannot be sorted")
		}
	}
	if !have {
		return 0, fmt.Errorf("field has no sortable scalar type")
	}
	return kind, nil
}

func scalarClass(t schema.DataType) (spi.OrderKind, error) {
	switch {
	case schema.IsNumeric(t):
		return spi.OrderNumeric, nil
	case t == schema.Boolean:
		return spi.OrderBool, nil
	case t == schema.String, t == schema.Character, t == schema.UUIDType,
		t == schema.TimeUUIDType, t == schema.LocalDate, t == schema.LocalDateTime,
		t == schema.LocalTime, t == schema.ZonedDateTime, t == schema.Year,
		t == schema.YearMonth:
		// All compared as their stored ISO/string form (Text/byte order).
		return spi.OrderText, nil
	default: // ByteArray and anything non-scalar
		return 0, fmt.Errorf("type %s is not sortable", t)
	}
}

type metaField struct {
	Source spi.FieldSource
	Path   string
	Kind   spi.OrderKind
}

// sortableMetaFields is the closed set of meta sort keys (canonical client
// names from the result envelope). The plugins map these to physical storage.
var sortableMetaFields = map[string]metaField{
	"state":                   {spi.SourceMeta, "state", spi.OrderText},
	"creationDate":            {spi.SourceMeta, "creationDate", spi.OrderTemporal},
	"lastUpdateTime":          {spi.SourceMeta, "lastUpdateTime", spi.OrderTemporal},
	"transitionForLatestSave": {spi.SourceMeta, "transitionForLatestSave", spi.OrderText},
	"transactionId":           {spi.SourceMeta, "transactionId", spi.OrderText},
	"id":                      {spi.SourceMeta, "id", spi.OrderText},
}

func resolveMetaField(name string) (metaField, bool) {
	mf, ok := sortableMetaFields[name]
	return mf, ok
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/domain/search/ -run 'TestClassifyType|TestResolveMetaField' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/orderclass.go internal/domain/search/orderclass_test.go
git commit -m "feat(search): ordering-class resolution for sort keys (data types + meta allowlist)"
```

---

### Task 3: Domain — `OrderKey` type and `SearchOptions.OrderBy`

**Files:**
- Modify: `internal/domain/search/service.go:28-35` (the `SearchOptions` struct)
- Create: `internal/domain/search/orderkey.go`
- Test: `internal/domain/search/orderkey_test.go`

**Interfaces:**
- Produces: `search.OrderKey{ Path string; Source spi.FieldSource; Desc bool }`; new field `SearchOptions.OrderBy []OrderKey`.

- [ ] **Step 1: Write the failing test**

`internal/domain/search/orderkey_test.go`:
```go
package search

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestSearchOptions_CarriesOrderBy(t *testing.T) {
	o := SearchOptions{OrderBy: []OrderKey{{Path: "surname", Source: spi.SourceData, Desc: true}}}
	if len(o.OrderBy) != 1 || o.OrderBy[0].Path != "surname" {
		t.Fatalf("OrderBy not carried: %+v", o.OrderBy)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/search/ -run TestSearchOptions_CarriesOrderBy -v`
Expected: FAIL (`OrderBy`/`OrderKey` undefined).

- [ ] **Step 3: Write minimal implementation**

`internal/domain/search/orderkey.go`:
```go
package search

import spi "github.com/cyoda-platform/cyoda-go-spi"

// OrderKey is one parsed, pre-classification sort key from the request
// surface (HTTP grammar or gRPC orderBy). The service resolves it against the
// model schema into a fully-typed spi.OrderSpec.
type OrderKey struct {
	Path   string
	Source spi.FieldSource
	Desc   bool
}
```
In `service.go`, add to `SearchOptions`:
```go
	OrderBy         []OrderKey     // sort keys; empty ⇒ entity_id asc
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/domain/search/ -run TestSearchOptions_CarriesOrderBy -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/orderkey.go internal/domain/search/service.go
git commit -m "feat(search): OrderKey type and SearchOptions.OrderBy field"
```

---

### Task 4: Domain — HTTP sort grammar parser

**Files:**
- Create: `internal/domain/search/sortparam.go`
- Test: `internal/domain/search/sortparam_test.go`

**Interfaces:**
- Consumes: `search.OrderKey`, `spi.SourceData`/`spi.SourceMeta`.
- Produces: `func ParseSortParam(values []string, maxKeys int) ([]OrderKey, error)`. Errors are plain `error`; the handler maps them to `400 INVALID_FIELD_PATH`. Grammar only — does not check the schema or the meta allowlist (that is Task 6's resolve step), except it splits `@`/`:` and rejects malformed tokens.

- [ ] **Step 1: Write the failing test**

```go
package search

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestParseSortParam(t *testing.T) {
	got, err := ParseSortParam([]string{"surname:desc", "@creationDate:asc", "address.home-address.country"}, 16)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []OrderKey{
		{Path: "surname", Source: spi.SourceData, Desc: true},
		{Path: "creationDate", Source: spi.SourceMeta, Desc: false},
		{Path: "address.home-address.country", Source: spi.SourceData, Desc: false},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d keys, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("key %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseSortParam_DollarTolerated(t *testing.T) {
	got, err := ParseSortParam([]string{"$.surname:desc"}, 16)
	if err != nil || got[0].Path != "surname" || got[0].Source != spi.SourceData || !got[0].Desc {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestParseSortParam_DataFieldNamedMeta(t *testing.T) {
	got, err := ParseSortParam([]string{"meta.label.position.x:desc"}, 16)
	if err != nil || got[0].Source != spi.SourceData || got[0].Path != "meta.label.position.x" {
		t.Fatalf("data field 'meta' mis-parsed: %+v err %v", got, err)
	}
}

func TestParseSortParam_Errors(t *testing.T) {
	bad := [][]string{
		{""}, {":desc"}, {"@"}, {"name:"}, {"name:up"},
		{"@a.b.c"}, // nested meta
		{"surname", "surname"},        // duplicate
		{"surname:asc", "surname:desc"}, // duplicate (conflicting dir)
	}
	for _, in := range bad {
		if _, err := ParseSortParam(in, 16); err == nil {
			t.Fatalf("expected error for %v", in)
		}
	}
	// cap exceeded
	many := make([]string, 17)
	for i := range many {
		many[i] = "f" + string(rune('a'+i))
	}
	if _, err := ParseSortParam(many, 16); err == nil {
		t.Fatal("expected cap error")
	}
}
```
Note: `@a.b.c` is rejected here because a meta token must be a single flat segment; Task 6 also rejects unknown meta names, but a dotted meta token is malformed at the grammar level.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/search/ -run TestParseSortParam -v`
Expected: FAIL (`ParseSortParam` undefined).

- [ ] **Step 3: Write minimal implementation**

`internal/domain/search/sortparam.go`:
```go
package search

import (
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ParseSortParam parses repeatable `sort` query values into OrderKeys.
// Grammar: [@]path[:asc|:desc]. Bare ⇒ data; leading '@' ⇒ meta (flat name).
// A leading "$." on a data path is tolerated. Direction defaults to asc.
// Duplicate paths and >maxKeys keys are rejected. Semantic validation
// (schema scalar-leaf, meta allowlist) happens later in the service.
func ParseSortParam(values []string, maxKeys int) ([]OrderKey, error) {
	if len(values) > maxKeys {
		return nil, fmt.Errorf("too many sort keys: %d (max %d)", len(values), maxKeys)
	}
	keys := make([]OrderKey, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		k, err := parseSortToken(raw)
		if err != nil {
			return nil, err
		}
		dedup := string(k.Source) + ":" + k.Path
		if _, dup := seen[dedup]; dup {
			return nil, fmt.Errorf("duplicate sort key: %q", k.Path)
		}
		seen[dedup] = struct{}{}
		keys = append(keys, k)
	}
	return keys, nil
}

func parseSortToken(raw string) (OrderKey, error) {
	tok := raw
	desc := false
	if i := strings.LastIndexByte(tok, ':'); i >= 0 {
		switch tok[i+1:] {
		case "asc":
			desc = false
		case "desc":
			desc = true
		default:
			return OrderKey{}, fmt.Errorf("invalid sort direction in %q", raw)
		}
		tok = tok[:i]
	}
	source := spi.SourceData
	if strings.HasPrefix(tok, "@") {
		source = spi.SourceMeta
		tok = tok[1:]
		if strings.ContainsRune(tok, '.') {
			return OrderKey{}, fmt.Errorf("meta sort field must be a flat name: %q", raw)
		}
	} else {
		tok = strings.TrimPrefix(tok, "$.")
	}
	if tok == "" {
		return OrderKey{}, fmt.Errorf("empty sort path in %q", raw)
	}
	if !isValidSortPath(tok) {
		return OrderKey{}, fmt.Errorf("malformed sort path: %q", raw)
	}
	return OrderKey{Path: tok, Source: source, Desc: desc}, nil
}

// isValidSortPath allows dotted identifiers (letters/digits/_/-), no empty
// segments — the same safe subset filters use.
func isValidSortPath(p string) bool {
	if p == "" {
		return false
	}
	for _, seg := range strings.Split(p, ".") {
		if seg == "" {
			return false
		}
		for _, c := range seg {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
				c >= '0' && c <= '9', c == '_', c == '-':
			default:
				return false
			}
		}
	}
	return true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/domain/search/ -run TestParseSortParam -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/sortparam.go internal/domain/search/sortparam_test.go
git commit -m "feat(search): parse @-sigil sort query grammar into OrderKeys"
```

---

### Task 5: Domain — resolve OrderKeys to typed OrderSpecs against the schema

This is the semantic validator: scalar-leaf enforcement, type classification, meta allowlist. It produces the `[]spi.OrderSpec` (with `Kind`) the plugins consume.

**Files:**
- Create: `internal/domain/search/orderresolve.go`
- Test: `internal/domain/search/orderresolve_test.go`

**Interfaces:**
- Consumes: `classifyType`, `resolveMetaField` (Task 2); `schema.FieldDescriptor`, `schema.FieldsMap` shape (`map[string]schema.FieldDescriptor`, keys like `$.surname`, `$.items[*].price`).
- Produces: `func resolveOrderBy(keys []OrderKey, fields map[string]schema.FieldDescriptor) ([]spi.OrderSpec, error)`.

- [ ] **Step 1: Write the failing test**

```go
package search

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"<MODULE>/internal/domain/model/schema"
)

func fields() map[string]schema.FieldDescriptor {
	return map[string]schema.FieldDescriptor{
		"$.surname":   {Path: "$.surname", Types: []schema.DataType{schema.String}},
		"$.age":       {Path: "$.age", Types: []schema.DataType{schema.Integer}},
		"$.tags[*]":   {Path: "$.tags[*]", Types: []schema.DataType{schema.String}, IsArray: true},
	}
}

func TestResolveOrderBy_DataAndMeta(t *testing.T) {
	got, err := resolveOrderBy([]OrderKey{
		{Path: "surname", Source: spi.SourceData, Desc: true},
		{Path: "creationDate", Source: spi.SourceMeta},
	}, fields())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []spi.OrderSpec{
		{Path: "surname", Source: spi.SourceData, Desc: true, Kind: spi.OrderText},
		{Path: "creationDate", Source: spi.SourceMeta, Desc: false, Kind: spi.OrderTemporal},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("spec %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestResolveOrderBy_Rejections(t *testing.T) {
	f := fields()
	bad := [][]OrderKey{
		{{Path: "missing", Source: spi.SourceData}},   // not in schema
		{{Path: "tags", Source: spi.SourceData}},      // array (only $.tags[*] is a leaf, "tags" is not)
		{{Path: "nope", Source: spi.SourceMeta}},      // unknown meta
	}
	for _, keys := range bad {
		if _, err := resolveOrderBy(keys, f); err == nil {
			t.Fatalf("expected error for %+v", keys)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/search/ -run TestResolveOrderBy -v`
Expected: FAIL (`resolveOrderBy` undefined).

- [ ] **Step 3: Write minimal implementation**

`internal/domain/search/orderresolve.go`:
```go
package search

import (
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"<MODULE>/internal/domain/model/schema"
)

// resolveOrderBy validates each OrderKey and attaches its ordering class,
// producing the typed OrderSpecs the plugins/comparator consume. Data keys
// must be a scalar (non-array) leaf in the model schema; meta keys must be in
// the canonical allowlist. Any failure returns an error the caller maps to
// 400 INVALID_FIELD_PATH.
func resolveOrderBy(keys []OrderKey, fields map[string]schema.FieldDescriptor) ([]spi.OrderSpec, error) {
	specs := make([]spi.OrderSpec, 0, len(keys))
	for _, k := range keys {
		if k.Source == spi.SourceMeta {
			mf, ok := resolveMetaField(k.Path)
			if !ok {
				return nil, fmt.Errorf("unknown meta sort field: %q", k.Path)
			}
			specs = append(specs, spi.OrderSpec{Path: mf.Path, Source: mf.Source, Desc: k.Desc, Kind: mf.Kind})
			continue
		}
		fd, ok := fields["$."+k.Path]
		if !ok {
			return nil, fmt.Errorf("unknown sort field: %q", k.Path)
		}
		if fd.IsArray {
			return nil, fmt.Errorf("cannot sort by array field: %q", k.Path)
		}
		kind, err := classifyType(fd.Types)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", k.Path, err)
		}
		specs = append(specs, spi.OrderSpec{Path: k.Path, Source: spi.SourceData, Desc: k.Desc, Kind: kind})
	}
	return specs, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/domain/search/ -run TestResolveOrderBy -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/orderresolve.go internal/domain/search/orderresolve_test.go
git commit -m "feat(search): resolve sort keys to typed OrderSpecs against model schema"
```

---

### Task 6: Config — `CYODA_SEARCH_MAX_SORT_KEYS`

**Files:**
- Modify: `app/config.go` (Config struct + `DefaultConfig`)
- Modify: `internal/domain/search/handler.go:45-60` (`NewHandlerWithModel` signature) and `app/app.go:513` (call site)
- Test: `app/config_test.go` (add a case) and `internal/domain/search/handler_test.go`

**Interfaces:**
- Produces: `Config.SearchMaxSortKeys int` (default 16, env `CYODA_SEARCH_MAX_SORT_KEYS`); `search.NewHandlerWithModel(svc, factory, maxSortKeys)`; `Handler.maxSortKeys`.

- [ ] **Step 1: Write the failing test**

In `app/config_test.go` add:
```go
func TestDefaultConfig_SearchMaxSortKeys(t *testing.T) {
	t.Setenv("CYODA_SEARCH_MAX_SORT_KEYS", "")
	if got := DefaultConfig().SearchMaxSortKeys; got != 16 {
		t.Fatalf("default SearchMaxSortKeys = %d, want 16", got)
	}
	t.Setenv("CYODA_SEARCH_MAX_SORT_KEYS", "4")
	if got := DefaultConfig().SearchMaxSortKeys; got != 4 {
		t.Fatalf("env SearchMaxSortKeys = %d, want 4", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./app/ -run TestDefaultConfig_SearchMaxSortKeys -v`
Expected: FAIL (`SearchMaxSortKeys` undefined).

- [ ] **Step 3: Write minimal implementation**

In `app/config.go` add to `Config`:
```go
	// SearchMaxSortKeys caps the number of sort keys per search request.
	// Defaults to 16; tune via CYODA_SEARCH_MAX_SORT_KEYS.
	SearchMaxSortKeys int
```
In `DefaultConfig()` (near `statsGroupMax := envInt(...)`), with a `<=0`
re-default guard (mirroring `statsGroupMax` at `app/config.go:163-165`, since
`envInt` has none) — a `0` cap would otherwise 400 every sorted request:
```go
	maxSortKeys := envInt("CYODA_SEARCH_MAX_SORT_KEYS", 16)
	if maxSortKeys <= 0 {
		maxSortKeys = 16
	}
```
and set `SearchMaxSortKeys: maxSortKeys` in the returned `Config`.
In `internal/domain/search/handler.go`, thread the value:
```go
func NewHandlerWithModel(searchSvc *SearchService, factory spi.StoreFactory, maxSortKeys int) *Handler {
	return &Handler{searchSvc: searchSvc, factory: factory, maxSortKeys: maxSortKeys}
}
```
Add `maxSortKeys int` to the `Handler` struct. Update `app/app.go:513`:
```go
	server.Search = search.NewHandlerWithModel(a.searchService, a.storeFactory, a.config.SearchMaxSortKeys)
```
(If `NewHandler` — the no-model constructor — is still referenced by tests, give it a default of 16.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./app/ -run TestDefaultConfig_SearchMaxSortKeys -v && go build ./...`
Expected: PASS + build OK.

- [ ] **Step 5: Commit**

```bash
git add app/config.go app/config_test.go internal/domain/search/handler.go app/app.go
git commit -m "feat(config): CYODA_SEARCH_MAX_SORT_KEYS bounds sort keys per request"
```

---

### Task 7: HTTP — wire `sort` param through the handler

**Files:**
- Modify: `api/openapi.yaml` (add `sort` query param to `searchEntities` and the async submit op) and regenerate `api/generated.go`
- Modify: `internal/domain/search/handler.go:103-160` (sync) and the async submit block (~186-233)
- Test: covered by e2e in Task 14; add a focused handler unit test here

**Interfaces:**
- Consumes: `ParseSortParam` (Task 4), `SearchOptions.OrderBy` (Task 3), `Handler.maxSortKeys` (Task 6).
- Produces: `params.Sort *[]string` populates `opts.OrderBy`.

- [ ] **Step 1: Add the OpenAPI param + regenerate**

In `api/openapi.yaml`, under `searchEntities` parameters (after `timeoutMillis`, ~line 6555) and the async submit op, add:
```yaml
        - name: sort
          in: query
          description: >
            Repeatable sort key. Grammar: [@]path[:asc|desc], direction
            defaults to asc. A bare path sorts by a scalar entity-data field;
            a leading '@' selects a meta field (state, creationDate,
            lastUpdateTime, transitionForLatestSave, transactionId, id).
            Repetition order is sort precedence; entity id is the final
            tiebreaker. Absent/null values sort last.
          required: false
          schema:
            type: array
            items:
              type: string
          style: form
          explode: true
```
Regenerate: `go generate ./api/...` (or the project's codegen command — check `api/` for a `//go:generate` directive). Confirm `SearchEntitiesParams` now has `Sort *[]string`.

- [ ] **Step 2: Write the failing handler test**

In `internal/domain/search/handler_test.go`, add a test that a `sort` param with an unknown field yields 400 `INVALID_FIELD_PATH` and a valid one is accepted (use the existing handler test harness/mocks in that file as the pattern). Example assertion shape:
```go
func TestSearchEntities_SortUnknownField400(t *testing.T) {
	// ... build handler + request with params.Sort = &[]string{"nope"} against a model whose schema lacks "nope"
	// expect resp.Code == 400 and body code == "INVALID_FIELD_PATH"
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/domain/search/ -run TestSearchEntities_Sort -v`
Expected: FAIL.

- [ ] **Step 4: Wire the handler**

In `handler.go` sync path, after building `opts` and before `h.searchSvc.Search`:
```go
	if params.Sort != nil {
		keys, perr := ParseSortParam(*params.Sort, h.maxSortKeys)
		if perr != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidFieldPath, perr.Error()))
			return
		}
		opts.OrderBy = keys
	}
```
Do the same in the async submit path. The error-code constant is
`common.ErrCodeInvalidFieldPath` (`= "INVALID_FIELD_PATH"`) in
`internal/common/error_codes.go:28` — the same `common` package the handler
already imports for `ErrCodeBadRequest`. (Semantic resolution — unknown field,
array, meta allowlist — runs in the service/`SubmitAsync`, Task 8; the handler
just forwards the classified `*common.AppError`.)

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/domain/search/ -run TestSearchEntities_Sort -v`
Expected: PASS. (Full resolution/validation runs in the service — Task 8 — so this test exercises grammar-level + service-level rejection end to end.)

- [ ] **Step 6: Commit**

```bash
git add api/openapi.yaml api/generated.go internal/domain/search/handler.go internal/domain/search/handler_test.go
git commit -m "feat(search): accept sort query param on sync and async search endpoints"
```

---

### Task 8: Service — resolve + push down OrderBy, and validate

**Files:**
- Modify: `internal/domain/search/service.go` (`Search`, ~108-136) and add a resolve helper that loads the schema (reuse `loadFieldsMap`)
- Test: `internal/domain/search/service_test.go`

**Interfaces:**
- Consumes: `resolveOrderBy` (Task 5), `loadFieldsMap`/`s.factory.ModelStore` (existing), `SearchOptions.OrderBy` (Task 3).
- Produces: `spi.SearchOptions.OrderBy []spi.OrderSpec` populated on the pushdown call; `Search` returns `INVALID_FIELD_PATH`-classified error for bad sort keys.

- [ ] **Step 1: Write the failing test**

In `service_test.go`, add a test with a fake `Searcher` store that records the `spi.SearchOptions` it receives; assert that `Search` with `opts.OrderBy = []OrderKey{{Path:"surname",Source:SourceData,Desc:true}}` (model schema has `surname` String) pushes `[]spi.OrderSpec{{Path:"surname",Source:SourceData,Desc:true,Kind:spi.OrderText}}`. Add a second test: an unknown sort field returns an error classified as `INVALID_FIELD_PATH` (`errors.As` to `*common.AppError`, code check).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/search/ -run TestSearch_.*Sort -v`
Expected: FAIL.

- [ ] **Step 3: Implement resolve + pushdown**

Add to `service.go` a helper:
```go
// resolveSortKeys turns the request OrderKeys into typed OrderSpecs, validating
// scalar-leaf data paths and the meta allowlist. Returns a 400-classified
// AppError on bad input.
func (s *SearchService) resolveSortKeys(ctx context.Context, modelRef spi.ModelRef, keys []OrderKey) ([]spi.OrderSpec, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	modelStore, err := s.factory.ModelStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get model store: %w", err)
	}
	fields, err := loadFieldsMap(ctx, modelStore, modelRef)
	if err != nil {
		return nil, fmt.Errorf("failed to load schema for sort validation: %w", err)
	}
	specs, rerr := resolveOrderBy(keys, fields)
	if rerr != nil {
		return nil, common.Operational(http.StatusBadRequest, common.ErrCodeInvalidFieldPath, rerr.Error())
	}
	return specs, nil
}
```
In `Search`, before the store dispatch, resolve once:
```go
	orderBy, oerr := s.resolveSortKeys(ctx, modelRef, opts.OrderBy)
	if oerr != nil {
		return nil, oerr
	}
```
Add `OrderBy: orderBy,` to the `spi.SearchOptions{...}` literal in the pushdown branch. (The fallback branch uses `orderBy` in Task 9.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/domain/search/ -run TestSearch_.*Sort -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/service.go internal/domain/search/service_test.go
git commit -m "feat(search): resolve and push OrderBy down to spi.Searcher"
```

---

### Task 9: Service — in-memory comparator for the fallback path

**Files:**
- Create: `internal/domain/search/ordersort.go`
- Modify: `internal/domain/search/service.go` (fallback branch, before offset/limit)
- Test: `internal/domain/search/ordersort_test.go`

**Interfaces:**
- Consumes: `spi.OrderSpec`/`spi.OrderKind`, `spi.Entity`, gjson.
- Produces: `func sortEntities(entities []*spi.Entity, specs []spi.OrderSpec)` — stable sort implementing the canonical semantic (Kind compare, NULLS LAST, `entity_id` tiebreaker).

- [ ] **Step 1: Write the failing test**

```go
package search

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func ent(id, data string, created time.Time) *spi.Entity {
	return &spi.Entity{Meta: spi.EntityMeta{ID: id, CreationDate: created}, Data: []byte(data)}
}

func ids(es []*spi.Entity) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Meta.ID
	}
	return out
}

func TestSortEntities_NumericNotLexical(t *testing.T) {
	es := []*spi.Entity{
		ent("a", `{"n":9}`, time.Time{}),
		ent("b", `{"n":10}`, time.Time{}),
		ent("c", `{"n":100}`, time.Time{}),
	}
	sortEntities(es, []spi.OrderSpec{{Path: "n", Source: spi.SourceData, Kind: spi.OrderNumeric}})
	if got := ids(es); got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("numeric order = %v, want [a b c]", got)
	}
}

func TestSortEntities_NullsLast(t *testing.T) {
	es := []*spi.Entity{
		ent("a", `{"x":"m"}`, time.Time{}),
		ent("b", `{}`, time.Time{}), // missing x
		ent("c", `{"x":"z"}`, time.Time{}),
	}
	sortEntities(es, []spi.OrderSpec{{Path: "x", Source: spi.SourceData, Kind: spi.OrderText}})
	if got := ids(es); got[2] != "b" {
		t.Fatalf("nulls-last order = %v, want b last", got)
	}
}

func TestSortEntities_MetaCreationDateAndTiebreaker(t *testing.T) {
	t0 := time.Unix(0, 0).UTC()
	t1 := time.Unix(1, 0).UTC()
	es := []*spi.Entity{
		ent("b", `{}`, t1),
		ent("a", `{}`, t1),
		ent("c", `{}`, t0),
	}
	sortEntities(es, []spi.OrderSpec{{Path: "creationDate", Source: spi.SourceMeta, Kind: spi.OrderTemporal}})
	// c (t0) first; a,b (t1) ordered by entity_id tiebreaker
	if got := ids(es); got[0] != "c" || got[1] != "a" || got[2] != "b" {
		t.Fatalf("temporal+tiebreaker order = %v, want [c a b]", got)
	}
}

func TestSortEntities_NoKeysOrdersByID(t *testing.T) {
	es := []*spi.Entity{ent("c", `{}`, time.Time{}), ent("a", `{}`, time.Time{}), ent("b", `{}`, time.Time{})}
	sortEntities(es, nil) // no sort keys ⇒ entity_id asc, matching the SQL backends
	if got := ids(es); got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("no-keys order = %v, want [a b c]", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/search/ -run TestSortEntities -v`
Expected: FAIL (`sortEntities` undefined).

- [ ] **Step 3: Write minimal implementation**

`internal/domain/search/ordersort.go`:
```go
package search

import (
	"bytes"
	"sort"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/tidwall/gjson"
)

// sortEntities sorts in place per the canonical ordering semantic: each spec
// in precedence order, comparison fixed by Kind, missing/null last (both
// directions), with a final entity_id ascending tiebreaker. Stable so the
// tiebreaker resolves equal rows deterministically.
//
// It runs UNCONDITIONALLY, even with no specs: the SQL backends default to
// "ORDER BY entity_id", but the memory backend's GetAll returns Go
// map-iteration order, so without this the no-sort case would diverge
// (non-deterministic memory order vs entity_id on SQL). With no specs the loop
// body is skipped and rows order by the entity_id tiebreaker — matching SQL.
func sortEntities(entities []*spi.Entity, specs []spi.OrderSpec) {
	sort.SliceStable(entities, func(i, j int) bool {
		for _, s := range specs {
			if decided, less := lessByKey(entities[i], entities[j], s); decided {
				return less
			}
		}
		return entities[i].Meta.ID < entities[j].Meta.ID
	})
}

// lessByKey decides ordering for a single key, fully applying direction and
// nulls-last. decided=false means the two are equal under this key (advance to
// the next key). A present value always precedes a missing/null one regardless
// of Desc.
func lessByKey(a, b *spi.Entity, s spi.OrderSpec) (decided, less bool) {
	av, aok := leafValue(a, s)
	bv, bok := leafValue(b, s)
	if !aok || !bok {
		if aok == bok {
			return false, false // both missing ⇒ equal
		}
		return true, aok // present (aok) sorts first, irrespective of Desc
	}
	c := compareValues(av, bv, s.Kind)
	if c == 0 {
		return false, false
	}
	if s.Desc {
		return true, c > 0
	}
	return true, c < 0
}

// compareValues returns -1/0/1 for two present values under the ordering class.
func compareValues(av, bv gjson.Result, kind spi.OrderKind) int {
	switch kind {
	case spi.OrderNumeric:
		return cmpFloat(av.Float(), bv.Float())
	case spi.OrderBool:
		return cmpBool(av.Bool(), bv.Bool())
	case spi.OrderTemporal:
		return cmpFloat(av.Num, bv.Num) // unix-nanos carried in Num (see timeResult)
	default: // OrderText
		return bytes.Compare([]byte(av.String()), []byte(bv.String()))
	}
}
```
Add the leaf-extraction helpers (a present value is returned as a `gjson.Result`):
```go
func leafValue(e *spi.Entity, s spi.OrderSpec) (gjson.Result, bool) {
	if s.Source == spi.SourceMeta {
		return metaLeaf(e, s.Path)
	}
	r := gjson.GetBytes(e.Data, s.Path)
	if !r.Exists() || r.Type == gjson.Null {
		return gjson.Result{}, false
	}
	return r, true
}

func metaLeaf(e *spi.Entity, path string) (gjson.Result, bool) {
	switch path {
	case "state":
		return gjson.Result{Type: gjson.String, Str: e.Meta.State}, e.Meta.State != ""
	case "transitionForLatestSave":
		return gjson.Result{Type: gjson.String, Str: e.Meta.TransitionForLatestSave}, e.Meta.TransitionForLatestSave != ""
	case "transactionId":
		return gjson.Result{Type: gjson.String, Str: e.Meta.TransactionID}, e.Meta.TransactionID != ""
	case "id":
		return gjson.Result{Type: gjson.String, Str: e.Meta.ID}, e.Meta.ID != ""
	case "creationDate":
		return timeResult(e.Meta.CreationDate)
	case "lastUpdateTime":
		return timeResult(e.Meta.LastModifiedDate)
	}
	return gjson.Result{}, false
}
```
Because temporal comparison needs `time.Time`, not `gjson`, special-case it: store the time on the entity and compare via a dedicated path. Simplest robust approach — split the comparator so temporal uses `time.Time` directly:
```go
func timeResult(t time.Time) (gjson.Result, bool) {
	if t.IsZero() {
		return gjson.Result{}, false
	}
	// Canonical temporal resolution is MILLISECONDS (the coarsest floor common
	// to every parity backend, incl. commercial Cassandra/HLC). Carry epoch-ms
	// in Num; UnixMilli (~1.75e12) is exact in float64 (< 2^53). Never carry
	// UnixNano — it exceeds 2^53 and loses precision. The SQL backends floor to
	// ms too (Tasks 10/11), so all paths tie within the same millisecond.
	// (Go UnixMilli truncates toward zero while postgres floor() rounds toward
	// -inf; they differ only for pre-1970 instants, which engine meta dates
	// never are — no fix needed.)
	return gjson.Result{Type: gjson.Number, Num: float64(t.UnixMilli())}, true
}

func cmpFloat(a, b float64) int { switch { case a < b: return -1; case a > b: return 1; default: return 0 } }
func cmpBool(a, b bool) int { switch { case !a && b: return -1; case a && !b: return 1; default: return 0 } }
```
`OrderTemporal` meta values are carried as epoch-**milliseconds** in
`gjson.Result.Num` (see `timeResult`), so `compareValues` compares them via
`cmpFloat(av.Num, bv.Num)` — already wired above. Epoch-ms is exact in float64
(< 2^53); epoch-nanos would not be. The `TestSortEntities_MetaCreationDateAndTiebreaker`
test must seed instants that differ by **whole milliseconds** (e.g. `t0`, `t1`
one second apart) so it exercises the canonical comparison, and a separate
assertion should confirm two sub-millisecond-apart instants tie (→ entity_id
tiebreaker).

- [ ] **Step 4: Apply in the fallback path**

In `service.go` fallback branch, after collecting `matches` and **before** offset/limit:
```go
	sortEntities(matches, orderBy)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/domain/search/ -run TestSortEntities -v && go test ./internal/domain/search/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/search/ordersort.go internal/domain/search/service.go internal/domain/search/ordersort_test.go
git commit -m "feat(search): in-memory canonical sort for memory backend and fallback path"
```

---

### Task 10: sqlite — Kind-aware ORDER BY + canonical meta mapping

**Files:**
- Modify: `plugins/sqlite/searcher.go` (`orderByClause`, `orderByFieldExpr`)
- Test: `plugins/sqlite/searcher_test.go`

**Interfaces:**
- Consumes: `spi.OrderSpec.Kind`, `spi.OrderKind` (Task 1).
- Produces: Kind-aware SQL: Numeric ⇒ `CAST(... AS REAL)`, Text ⇒ `... COLLATE BINARY`, Temporal/meta-date ⇒ numeric micros column/blob, plus `NULLS LAST` and a skipped/added `entity_id` tiebreaker; meta canonical names mapped to physical blob keys/columns.

- [ ] **Step 1: Write the failing tests**

In `plugins/sqlite/searcher_test.go` (use the existing sqlite test harness
pattern — open an in-memory store, insert entities, Search with OrderBy, assert
id order). Cover: numeric data field (9,10,100 ⇒ numeric not lexical), text data
field, `creationDate` meta (chronological, instants ≥1 ms apart), `state` meta,
multi-key + tiebreaker, NULLS LAST, point-in-time sort. The existing
SQL-injection order test lives in `plugins/sqlite/search_injection_test.go`
(grammar validation, unchanged) — leave it and add the new ordering tests in
`searcher_test.go`. Example:
```go
func TestSearcher_OrderByNumericData(t *testing.T) {
	// insert {n:9}=a, {n:10}=b, {n:100}=c
	got := searchIDs(t, store, filterAll, spi.SearchOptions{OrderBy: []spi.OrderSpec{
		{Path: "n", Source: spi.SourceData, Kind: spi.OrderNumeric}}})
	want := []string{"a", "b", "c"}
	// assert equal
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd plugins/sqlite && go test ./... -run TestSearcher_OrderBy -v`
Expected: FAIL (lexical order, or wrong meta resolution).

- [ ] **Step 3: Implement**

Rewrite `orderByFieldExpr` to be Kind-aware and map meta canonical names. The
meta blob key for each canonical name (`id` is the `entity_id` column):
```go
// metaBlobKey maps canonical meta sort names to this backend's blob key.
// "id" is special-cased to the entity_id column.
var metaBlobKey = map[string]string{
	"state":                   "state",
	"creationDate":            "creation_date",
	"lastUpdateTime":          "last_modified_date",
	"transitionForLatestSave": "transition_for_latest_save",
	"transactionId":           "transaction_id",
}

func jsonExtract(col, key string) string { return fmt.Sprintf("json_extract(json(%s), '$.%s')", col, key) }

func orderByFieldExpr(spec spi.OrderSpec, tablePrefix string) string {
	qualify := func(col string) string {
		if tablePrefix != "" {
			return tablePrefix + "." + col
		}
		return col
	}
	var base string
	switch {
	case spec.Source == spi.SourceMeta && spec.Path == "id":
		base = qualify("entity_id")
	case spec.Source == spi.SourceMeta:
		// spec.Path is guaranteed canonical by validateOrderSpecs; the ok-guard
		// is defense-in-depth (no nil/empty interpolation).
		key, ok := metaBlobKey[spec.Path]
		if !ok {
			key = spec.Path
		}
		base = jsonExtract(qualify("meta"), key)
	default:
		base = fmt.Sprintf("json_extract(%s, '$.%s')", qualify("data"), spec.Path)
	}
	switch spec.Kind {
	case spi.OrderNumeric:
		return "CAST(" + base + " AS REAL)"
	case spi.OrderTemporal:
		return "(" + base + ") / 1000" // blob stores microseconds; floor to ms (int division)
	case spi.OrderBool:
		return base // json_extract yields 0/1
	default: // OrderText
		return base + " COLLATE BINARY"
	}
}
```
Extend `validateOrderSpecs` (in `plugins/sqlite/path_validation.go`) to reject a
`Source=meta` path that is neither `id` nor a `metaBlobKey` key (returns the
existing invalid-path error) — closing the unmapped-meta hole at the boundary,
before any SQL is built.
Update `orderByClause` to add `NULLS LAST` to every key, append the `entity_id` tiebreaker, and skip it when the terminal key is `@id`:
```go
func orderByClause(opts spi.SearchOptions, tablePrefix string) string {
	idCol := "entity_id"
	if tablePrefix != "" {
		idCol = tablePrefix + ".entity_id"
	}
	if len(opts.OrderBy) == 0 {
		return " ORDER BY " + idCol
	}
	clauses := make([]string, 0, len(opts.OrderBy)+1)
	for _, spec := range opts.OrderBy {
		expr := orderByFieldExpr(spec, tablePrefix)
		if spec.Desc {
			expr += " DESC"
		}
		clauses = append(clauses, expr+" NULLS LAST")
	}
	if last := opts.OrderBy[len(opts.OrderBy)-1]; !(last.Source == spi.SourceMeta && last.Path == "id") {
		clauses = append(clauses, idCol)
	}
	return " ORDER BY " + strings.Join(clauses, ", ")
}
```
Verify `creationDate`/`lastUpdateTime` map to the meta blob value for BOTH current-state and point-in-time queries (the PIT projection — adjust `metaPhysical` qualify target if PIT aliases `meta` differently). Run the PIT test from Step 1 to confirm.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd plugins/sqlite && go test ./... -run TestSearcher_OrderBy -v && go test ./...`
Expected: PASS (and the existing `TestSearcher_RejectsMaliciousOrderByPath` still passes — grammar validation unchanged).

- [ ] **Step 5: Commit**

```bash
git add plugins/sqlite/searcher.go plugins/sqlite/searcher_test.go
git commit -m "feat(sqlite): Kind-aware ORDER BY, canonical meta mapping, NULLS LAST, id tiebreaker"
```

---

### Task 11: postgres — Kind-aware ORDER BY + canonical meta mapping

**Files:**
- Modify: `plugins/postgres/searcher.go` (`orderByClause`, `orderByFieldExpr`)
- Test: `plugins/postgres/searcher_test.go` (extends the existing `TestPGSearcher_OrderBy*`)

**Interfaces:**
- Consumes: `spi.OrderSpec.Kind`.
- Produces: Numeric ⇒ `(...)::double precision`, Text ⇒ `(...) COLLATE "C"`, Temporal ⇒ `(...)::timestamptz`, Bool ⇒ `(...)::boolean`; `NULLS LAST`; `entity_id` tiebreaker (skipped for `@id`); canonical meta names mapped to snake_case `_meta` keys (`transition`, not `transition_for_latest_save`).

- [ ] **Step 1: Write the failing tests**

Extend `plugins/postgres/searcher_test.go` with the same scenarios as Task 10
(numeric-not-lexical, creationDate chronological with instants ≥1 ms apart,
state, multi-key+tiebreaker, NULLS LAST, PIT). **Also migrate the existing
`TestPGSearcher_OrderByMetaDirectColumn`** (`searcher_test.go:271`, uses
`Path:"entity_id", Source:SourceMeta`) to the canonical name `Path:"id"`, and set
`Kind` on the existing `TestPGSearcher_OrderByDataPath*` tests (e.g. `OrderText`
for a string field, `OrderNumeric` for a numeric one) since they predate `Kind`
(zero value `OrderText` now means `COLLATE "C"`). These require the postgres
testcontainer (Docker).

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd plugins/postgres && go test ./... -run 'TestPGSearcher_OrderBy' -v`
Expected: FAIL (TEXT lexical numeric order; meta name mismatch).

- [ ] **Step 3: Implement**

```go
// metaJSONKey maps canonical meta sort names to this backend's _meta JSONB key.
// Note postgres uses "transition" where sqlite uses "transition_for_latest_save".
// "id" is special-cased to the entity_id column.
var metaJSONKey = map[string]string{
	"state":                   "state",
	"creationDate":            "creation_date",
	"lastUpdateTime":          "last_modified_date",
	"transitionForLatestSave": "transition",
	"transactionId":           "transaction_id",
}

func orderByFieldExpr(spec spi.OrderSpec) string {
	var base string
	switch {
	case spec.Source == spi.SourceMeta && spec.Path == "id":
		base = "entity_id"
	case spec.Source == spi.SourceMeta:
		key, ok := metaJSONKey[spec.Path] // canonical-guaranteed by validateOrderSpecs
		if !ok {
			key = spec.Path
		}
		base = jsonbExtractText("doc->'_meta'", key)
	default:
		base = jsonbExtractText("doc", spec.Path)
	}
	switch spec.Kind {
	case spi.OrderNumeric:
		// cyoda_try_float8 returns NULL on non-numeric text (→ NULLS LAST),
		// matching sqlite's lenient CAST; a raw ::double precision cast would
		// error the whole query. (Helper already used in query_planner.go.)
		return "cyoda_try_float8(" + base + ")"
	case spi.OrderTemporal:
		// _meta value is RFC3339 text; floor the instant to epoch-milliseconds
		// (the canonical resolution) for cross-backend agreement.
		return "floor(extract(epoch from (" + base + ")::timestamptz)*1000)"
	case spi.OrderBool:
		return "(" + base + ")::boolean"
	default: // OrderText
		return "(" + base + ") COLLATE \"C\""
	}
}
```
Update `orderByClause` exactly as sqlite (Task 10): `NULLS LAST` per key +
`entity_id` tiebreaker, skipped when the terminal key is `@id`. Extend
`validateOrderSpecs` (`plugins/postgres/path_validation.go`) to reject a
`Source=meta` path that is neither `id` nor a `metaJSONKey` key.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd plugins/postgres && go test ./... -run 'TestPGSearcher_OrderBy' -v && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add plugins/postgres/searcher.go plugins/postgres/searcher_test.go
git commit -m "feat(postgres): Kind-aware ORDER BY casts, canonical meta mapping, NULLS LAST, id tiebreaker"
```

---

### Task 12: Async — resolve sort keys at submit; persist typed OrderSpecs

The async submit must (a) return `400 INVALID_FIELD_PATH` synchronously for bad
sort fields (not a silently-FAILED background job), and (b) persist the
**resolved, `Kind`-bearing** specs so a `SelfExecutingSearchStore` (commercial
backend) — which executes from the persisted opts and never runs the domain
resolver — orders with the correct classes.

**Files:**
- Modify: `internal/domain/search/service.go` (`SubmitAsync` ~190-217 and the persisted `SearchOpts` decode site)
- Test: `internal/domain/search/service_test.go`

**Interfaces:**
- Consumes: `resolveSortKeys` (Task 8), `SearchOptions.OrderBy`, `spi.OrderSpec`.
- Produces: resolved `[]spi.OrderSpec` persisted in the job's `SearchOpts` JSON and reloaded on execution; a 400-classified error returned from `SubmitAsync` on bad keys.

- [ ] **Step 1: Write the failing tests**

In `service_test.go`: (a) `SubmitAsync` with an unknown sort field returns an
error that `errors.As` to `*common.AppError` with code `INVALID_FIELD_PATH`
(before any job is created); (b) with valid keys, the persisted `SearchOpts`
JSON unmarshals to `[]spi.OrderSpec` carrying the resolved `Kind` (e.g.
`creationDate` ⇒ `OrderTemporal`); (c) if a `SelfExecutingSearchStore` fake is
available, assert it is handed the typed specs.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/domain/search/ -run TestSubmitAsync_OrderBy -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `SubmitAsync`, after the existing synchronous `validateConditionPaths`
(~`service.go:192`), resolve the sort keys synchronously and reuse the result:
```go
	orderBy, oerr := s.resolveSortKeys(ctx, modelRef, opts.OrderBy)
	if oerr != nil {
		return "", oerr // 400 INVALID_FIELD_PATH, before CreateJob
	}
```
Persist the **typed** specs (not raw keys):
```go
	optsJSON, err := json.Marshal(struct {
		Limit       int             `json:"limit"`
		Offset      int             `json:"offset"`
		PointInTime *time.Time      `json:"pointInTime,omitempty"`
		OrderBy     []spi.OrderSpec `json:"orderBy,omitempty"`
	}{
		Limit:       opts.Limit,
		Offset:      opts.Offset,
		PointInTime: opts.PointInTime,
		OrderBy:     orderBy,
	})
```
Find the decode site (grep `SearchOpts` unmarshal in `service.go` and the
self-executing/result executor) and add `OrderBy []spi.OrderSpec`, applying it as
the already-resolved order (no re-resolution needed there). The in-process
goroutine path is unaffected: it re-runs `s.Search` with the live `opts` whose
`OrderBy []OrderKey` is re-resolved deterministically to the identical specs.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/domain/search/ -run TestSubmitAsync_OrderBy -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/service.go internal/domain/search/service_test.go
git commit -m "feat(search): resolve sort keys at async submit; persist typed OrderSpecs"
```

---

### Task 13: gRPC — `orderBy` on both search requests

**Files:**
- Modify (schema sources): `docs/cyoda/schema/search/EntitySearchRequest.json`, `docs/cyoda/schema/search/EntitySnapshotSearchRequest.json`
- Regenerate: `api/grpc/events/types.go` (generated by go-jsonschema — `DO NOT EDIT`; never hand-edit)
- Modify: `internal/grpc/search.go` (sync opts build ~148-150; async snapshot ~325-330)
- Test: `internal/grpc/search_test.go`

**Interfaces:**
- Consumes: `SearchOptions.OrderBy`, `OrderKey`, the regenerated `events.EntitySearchRequestJson.OrderBy` / `events.EntitySnapshotSearchRequestJson.OrderBy`.
- Produces: parsed `orderBy` array → `opts.OrderBy`; invalid sort → `InvalidArgument` envelope with code `INVALID_FIELD_PATH` on BOTH the direct and snapshot paths.

- [ ] **Step 1: Add `orderBy` to the JSON schema sources and regenerate**

`events.EntitySearchRequestJson`/`EntitySnapshotSearchRequestJson`
(`api/grpc/events/types.go`) are generated and header-marked `DO NOT EDIT`. Add
to BOTH `docs/cyoda/schema/search/EntitySearchRequest.json` and
`EntitySnapshotSearchRequest.json` an optional `orderBy` array property:
```json
"orderBy": {
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "path":   { "type": "string" },
      "source": { "type": "string", "enum": ["data", "meta"], "default": "data" },
      "desc":   { "type": "boolean", "default": false }
    },
    "required": ["path"]
  }
}
```
Regenerate the package (the project's codegen — check for a `//go:generate`
go-jsonschema directive under `api/grpc/` and run `go generate ./api/grpc/...`).
Confirm `events.EntitySearchRequestJson` and `EntitySnapshotSearchRequestJson`
now expose an `OrderBy` slice field. Run `go test ./cmd/cyoda/... -run CloudEvents`
to confirm the `cyoda help cloudevents json` surface picks it up.

- [ ] **Step 2: Write the failing test**

In `internal/grpc/search_test.go`, build a direct search CloudEvent with
`orderBy:[{"path":"surname","desc":true}]` against a model with `surname`, assert
the envelope is `Success` and (via a recording fake searcher) that `opts.OrderBy`
carries the key; an `orderBy:[{"path":"nope"}]` yields `Error.Code ==
"INVALID_FIELD_PATH"`. Add the **same two assertions for the snapshot (async)
request** — bad sort must 400 synchronously at submit (depends on Task 12), not
produce a snapshot id.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/grpc/ -run 'TestEntitySearch.*Order' -v`
Expected: FAIL.

- [ ] **Step 4: Implement the mapping**

In both the sync and async handlers, after building `opts` (adapt field access
to the generated types — go-jsonschema may emit `Source`/`Desc` as pointers for
optional/defaulted fields; nil `Source` ⇒ data, nil `Desc` ⇒ false):
```go
	for _, o := range req.OrderBy {
		src := spi.SourceData
		if o.Source != nil && *o.Source == "meta" {
			src = spi.SourceMeta
		}
		desc := o.Desc != nil && *o.Desc
		opts.OrderBy = append(opts.OrderBy, search.OrderKey{Path: o.Path, Source: src, Desc: desc})
	}
```
Resolution/validation happens in `s.searchService` — `Search` for the direct
path and `SubmitAsync` for the snapshot path (Tasks 8, 12) — which returns the
`INVALID_FIELD_PATH` AppError; ensure both the `entityResponseError` and
`snapshotSearchError` mappers surface its code in the envelope.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/grpc/ -run 'TestEntitySearch.*Order' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/grpc/search.go internal/grpc/search_test.go
git commit -m "feat(grpc): orderBy on entity search and snapshot search requests"
```

---

### Task 14: e2e — HTTP coverage (happy path + every 400)

**Files:**
- Modify: `internal/e2e/search_test.go`

**Interfaces:**
- Consumes: the running httptest server + postgres testcontainer (existing `TestMain`).

- [ ] **Step 1: Write the e2e tests**

Add table-driven tests hitting `POST /search/direct/{model}/{v}?sort=...`:
- happy: sort by a data string field asc/desc; by `@creationDate`; by `@state`; multi-key.
- 400 `INVALID_FIELD_PATH`: unknown data field; array field; `@unknownMeta`; malformed token (`name:up`, `@`, empty); duplicate key; `>16` keys (use the default cap).
- assert ordering of returned NDJSON entity ids for the happy cases; assert status + body `code` for the 400 cases.

Also hit `POST /search/async/{model}/{v}?sort=...` (submit): a happy submit
returns a snapshot id; an unknown data field / `@unknownMeta` returns
**`400 INVALID_FIELD_PATH` synchronously** (not a snapshot id then a FAILED job) —
this is the regression guard for the async synchronous-resolution fix (Task 12).

- [ ] **Step 2: Run to verify (RED then GREEN as features already landed)**

Run: `go test ./internal/e2e/ -run TestSearch.*Sort -v` (Docker required)
Expected: PASS (implementation from Tasks 7–11 satisfies them). If any fail, fix the responsible task, not the test.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/search_test.go
git commit -m "test(e2e): http search sort happy paths and 400 error cases"
```

---

### Task 15: parity — cross-backend sort scenarios

**Files:**
- Create: `e2e/parity/search_sort.go`
- Modify: `e2e/parity/registry.go` (register the new scenarios)
- Test: runs across memory/sqlite/postgres (+ commercial) via the parity harness

**Interfaces:**
- Consumes: the parity scenario interface (follow `e2e/parity/search.go`).
- Produces: registered scenarios asserting identical id order on every backend.

- [ ] **Step 1: Write the scenarios**

In `search_sort.go`, scenarios (each seeds entities, runs a sorted search, asserts the exact id sequence — the cross-backend contract):
- `SearchSortDataText` (string field asc/desc)
- `SearchSortDataNumeric` (9/10/100 — the lexical-vs-numeric regression)
- `SearchSortMetaCreationDate` (chronological; sqlite micros vs postgres timestamptz vs Go time)
- `SearchSortMetaState`
- `SearchSortMultiKeyTiebreaker` (two equal primary keys resolved by entity_id)
- `SearchSortNullsLast` (some entities missing the field)
- `SearchSortPointInTime` (sort under a PIT query)
- `SearchSortDataFieldNamedMeta` (entity with top-level `meta` object sorts as data)
- `SearchNoSortDefaultOrder` (NO sort param — assert all backends return `entity_id` ascending; guards the MAJOR-1 memory-vs-SQL default-order divergence)

- [ ] **Step 2: Register and run**

Add the scenarios to `e2e/parity/registry.go`. Run: `go test ./e2e/parity/... -v` (Docker required).
Expected: PASS on all backends. A divergence here is the regression guard for the review's BLOCKER findings — fix the offending plugin/comparator, not the scenario.

- [ ] **Step 3: Commit**

```bash
git add e2e/parity/search_sort.go e2e/parity/registry.go
git commit -m "test(parity): cross-backend search sort ordering scenarios"
```

---

### Task 16: e2e — pushdown vs in-memory-fallback agreement (isolated)

**Files:**
- Modify: `internal/e2e/search_test.go` (single backend; not the parity suite)

- [ ] **Step 1: Write the test**

For one SQL backend (postgres), run the same sorted search twice: once normally (SQL pushdown), once inside an open transaction (forces the Go fallback). Assert identical id order. This proves a backend agrees with itself across both code paths (review finding 4). Isolated single-backend per `.claude/rules/test-coverage.md` (not parity).

- [ ] **Step 2: Run to verify**

Run: `go test ./internal/e2e/ -run TestSearchSort_PushdownFallbackAgree -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/search_test.go
git commit -m "test(e2e): pushdown and fallback produce identical sort order"
```

---

### Task 17: Docs — OpenAPI asc/desc fix + sort documentation

**Files:**
- Modify: `api/openapi.yaml` (≈6304, ≈6492) and mirrors `docs/cyoda/openapi.yml` (≈5360, ≈5530), `docs/cyoda/api/openapi-entity-search.yml` (≈225, ≈484)
- Modify: `cmd/cyoda/help/content/search.md`

- [ ] **Step 1: Fix the descending claim**

Replace "The results are sorted in descending order by entity id." with: "By default results are ordered by entity id ascending. Pass one or more `sort` keys to order by scalar data or meta fields; entity id is always the final tiebreaker." in `api/openapi.yaml` and all mirror copies.

- [ ] **Step 2: Add a Sorting section to `search.md`**

Document: the `sort` grammar (`[@]path[:asc|desc]`, default asc, repeat = precedence, `$.` tolerated), the `@` meta sigil + allowlist (`state`, `creationDate`, `lastUpdateTime`, `transitionForLatestSave`, `transactionId`, `id`), the canonical order semantics (byte order for strings, numeric for numbers, chronological for meta dates), NULLS-LAST, the `entity_id` tiebreaker, the configurable key cap, and that unsortable/unknown/array paths return `INVALID_FIELD_PATH`. Keep it compact.

- [ ] **Step 3: Verify help tests still pass**

Run: `go test ./cmd/cyoda/... -run 'Help|Search|OpenAPI' -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add api/openapi.yaml docs/cyoda/openapi.yml docs/cyoda/api/openapi-entity-search.yml cmd/cyoda/help/content/search.md
git commit -m "docs(search): document sort param; correct default-order asc/desc discrepancy"
```

---

### Task 18: Docs — config topic, README, COMPATIBILITY, CHANGELOG, cloud-parity

**Files:**
- Modify: `cmd/cyoda/help/content/config/*.md` (the search/limits topic), `README.md`, `COMPATIBILITY.md`, `CHANGELOG.md`
- Create: `docs/cloud-parity/search-sort.md`

- [ ] **Step 1: Document the env var (Gate 4 trio)**

Add `CYODA_SEARCH_MAX_SORT_KEYS` (default 16) to the relevant `config/*.md` help topic and the README configuration reference — alongside `DefaultConfig()` from Task 6.

- [ ] **Step 2: COMPATIBILITY + CHANGELOG**

Add a `CHANGELOG.md` entry (search result sorting; SPI `OrderKind` addition). Note the SPI `OrderKind`/`OrderSpec.Kind` addition in `COMPATIBILITY.md` (it ships with the v0.8.2 SPI tag — Task 19).

- [ ] **Step 3: Cloud-parity record (Gate 7)**

Create `docs/cloud-parity/search-sort.md`: the new contract surface (HTTP `sort` grammar, gRPC `orderBy`, canonical ordering semantic, meta allowlist) Cloud must mirror.

- [ ] **Step 4: Verify**

Run: `make todos` (no stray TODOs) and `go test ./cmd/cyoda/... -v`.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/cyoda/help/content/config README.md COMPATIBILITY.md CHANGELOG.md docs/cloud-parity/search-sort.md
git commit -m "docs(search): config topic, compatibility, changelog, cloud-parity for sort"
```

---

### Task 19: SPI pin bump (coordinated-release step)

**Files:**
- Modify: `go.mod` + `plugins/{memory,sqlite,postgres}/go.mod` (the `cyoda-go-spi` pseudo-version pin)

- [ ] **Step 1: Push the SPI commit and capture the pseudo-version**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && git push origin main
```
Per `MAINTAINING.md` "Coordinated release across sibling repos": within the open v0.8.2 SPI window, do **not** tag per-issue — bump the pseudo-version pin to the new SPI main HEAD.

- [ ] **Step 2: Bump the four go.mod pins**

In the cyoda-go worktree, update the `github.com/cyoda-platform/cyoda-go-spi` require in `go.mod` and each `plugins/*/go.mod` to the new pseudo-version (`go get github.com/cyoda-platform/cyoda-go-spi@main` in the root and each plugin module, or hand-edit to the captured pseudo-version), then `go mod tidy` per module.

- [ ] **Step 3: Verify the whole repo builds and tests against the pinned SPI**

Run: `go build ./... && make test-short-all`
Expected: builds; unit suites green.

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum plugins/*/go.mod plugins/*/go.sum
git commit -m "chore: pin cyoda-go-spi with OrderKind for search sort"
```

---

### Task 20: Create the Cassandra implementation issue

**Files:** none (creates a GitHub issue in the sibling repo).

- [ ] **Step 1: Create the issue**

Create an issue in the `cyoda-go-cassandra` repo titled "Implement search result sorting (OrderSpec.Kind) for the Cassandra backend". Body: reference the canonical ordering semantic (ordering classes Text/Numeric/Bool/Temporal, NULLS-LAST, `entity_id` tiebreaker), the meta canonical→physical mapping requirement, and that the `e2e/parity` sort scenarios (Task 15) propagate to the Cassandra plugin via the parity registry and must pass there. Do not link the private repo elsewhere; this issue lives in that repo.

```bash
gh issue create --repo cyoda/cyoda-go-cassandra \
  --title "Implement search result sorting (OrderSpec.Kind) for the Cassandra backend" \
  --body "<as above>"
```

- [ ] **Step 2: Record the issue URL in the PR description** (not in code).

---

## Self-Review

**Spec coverage:**
- §3.1 HTTP grammar → Tasks 4, 7. §3.2 gRPC orderBy → Task 13.
- §4 canonical semantic → Task 2 (classes), 9 (Go comparator), 10/11 (SQL render), 15 (parity guard).
- §4.2 NULLS LAST → 9, 10, 11. §4.3 default + tiebreaker → 10, 11 (SQL), 9 (Go). §4.4 type resolution/rejections → 2, 5.
- §5 meta mapping → 2 (allowlist/Kind), 10/11 (physical). §6 plumbing → 3, 7, 8. §7 fallback comparator → 9.
- §8 SPI OrderKind → 1; pin bump → 19. §9 validation/edge/error code → 4, 5, 8 (INVALID_FIELD_PATH; no new code). §10 docs → 17, 18. §11 error/status table → 7, 8, 13, 14. §12 coverage matrix → 14, 15, 16, 13. §13 async → 12. §14 Cassandra → 20.
- Config cap (D3) → 6. No gaps.

**Placeholder scan:** `<MODULE>` is an explicit substitution instruction (resolve from `go.mod`), not a placeholder. gRPC request-struct location (Task 13 Step 1) is a locate-then-edit step with the exact grep. No "TBD/handle edge cases/similar to Task N".

**Type consistency:** `OrderKey{Path, Source spi.FieldSource, Desc}` used consistently (Tasks 3,4,5,8,12,13). `spi.OrderSpec{Path, Source, Desc, Kind}` and `spi.OrderKind` consts (`OrderText/OrderNumeric/OrderBool/OrderTemporal`) used consistently (Tasks 1,2,5,9,10,11). `classifyType`/`resolveMetaField`/`resolveOrderBy`/`ParseSortParam`/`sortEntities`/`resolveSortKeys` names match across producers and consumers. `ErrCodeInvalidFieldPath` constant to be confirmed by grep in Task 7 (noted inline).

**Verification gate:** before PR, run `go vet ./...`, `make test-all` (Docker), and `make race` once (excludes e2e per the rule).
