# Tenant-scoped M2M clients (#650) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** M2M client ids become unique per tenant: the token endpoint moves to
`POST {ctx}/tenants/{tenant}/oauth/token`, clients are found by (tenant, id),
tenants may choose client ids, and every client write is atomic across the
cluster through new conditional key-value operations in the SPI.

**Architecture:** A new SPI contract (three conditional KV writes, no KV
operation joins a transaction) is implemented by the three in-tree plugins.
The client store drops its global index and builds create, reset and delete on
those writes. A small `internal/tenantroute` package owns the
`/tenants/{tenant}/…` route group (path checks, addressed tenant in context);
the token endpoint is its first member. `SYSTEM` is refused at every door a
tenant enters by.

**Tech Stack:** Go 1.26, `net/http` ServeMux patterns, chi (generated API),
oapi-codegen, pgx v5, database/sql (sqlite), testcontainers (e2e), the SPI's
`spitest` conformance suite.

**Spec:** `docs/superpowers/specs/2026-10-03-650-tenant-scoped-clients-design.md`
(read it before any task; section numbers below refer to it). Facts and
citations: `docs/superpowers/research/2026-10-03-650-tenant-scoped-clients-research.md`.

## Global Constraints

- Tenant grammar, exact: `^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`, case significant, compared byte for byte. Never fold, trim or re-decode a tenant id.
- API tenant = tenant grammar AND NOT `strings.EqualFold(id, "SYSTEM")`.
- Client-id grammar = tenant grammar AND NOT `strings.EqualFold(id, "system")`.
- Token route pattern, exact: `/tenants/{tenant}/oauth/token` (no method in the pattern), under `CYODA_CONTEXT_PATH` (default `/api`).
- Response-time floor on store-decided `401 invalid_client`: 500 ms from handler entry.
- Secret generation of a new client: random in `[1, 2^52]`; the record codec refuses a generation outside `[1, 2^53)`.
- New error code `M2M_CLIENT_EXISTS` (`409`); a lost reset race answers `409 CONFLICT`, retryable.
- No issue numbers (`#650` etc.) in shipped artefacts: code, comments, log messages, error text, help, OpenAPI. Commit messages and PR body only.
- Never log a secret, a token or a signing key. Never echo a rejected tenant id or client id into a log field: log a reason.
- `slog` only; errors wrapped `fmt.Errorf("failed to X: %w", err)`.
- Tests: `make test` while iterating, `make test-full` at the end. Never add `-count=1`. Never `-v` on `internal/e2e`.
- `go.work` is tracked. The `use` line for the local SPI checkout stays uncommitted. Stage files explicitly; never `git add -A` or `git add .`.
- SPI: mid-milestone, no tag. The SPI change goes in as a PR into `cyoda-go-spi` `main`; cyoda-go pseudo-pins `main` (Task 20).
- Every commit message ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **Dotted, underscored and hyphenated tenants in the path** (`acme.eu-1_x`) get a token like any other tenant. Test: Task 6 step 1 (`TestHandle_AcceptsEveryGrammarCharacter`).
2. **Ids that differ only in case** (`Backend`, `backend`) are two clients in one tenant, each with its own secret. Test: Task 9 step 1 (`TestKVM2M_IDsDifferingInCaseAreDistinct`).
3. **100-character tenant and client ids** work end to end; 101 characters are refused. Test: Task 6 step 1 (`TestHandle_TenantLengthBound`) and Task 13 step 1 (`TestCreateTechnicalUser_ClientIDLengthBound`).
4. **A client id sent form-urlencoded in the Basic header** (`my%2Dclient` for `my-client`, which RFC 6749 §2.3.1 allows) authenticates. Test: Task 9 step 1 (`TestToken_FormEncodedChosenIDAuthenticates`, in `internal/auth/token_test.go`).
5. **Re-creating a deleted id** gives a working client, and the old client's tokens cannot open a stream. Test: Task 9 step 1 (`TestKVM2M_RecreateAfterDelete`) and Task 16 (`TestStream_RecreatedClientID_OldTokenRefused`).

## Streams and order

| Stream | Tasks | Depends on |
|---|---|---|
| A — SPI and plugins | 1 → 2 → 3 | — |
| B — tenant doors | 4, 5 | — (parallel with A) |
| C — route group and token URL | 6 → 7 | 4 |
| D — client store | 8, 9 → 10 → 11 → 12 → 13 | A, 5, 7 (8 needs nothing) |
| E — coverage and docs | 14 → 15 → 16 → 17 → 18 → 19 → 20 → 21 | D |

Tasks in different streams that share no files may run in parallel worktrees.
Task 8 can run any time.

---

### Task 1: SPI — conditional key-value writes and their conformance cases

Repository: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi` (branch
`feat/kv-conditional-writes` off `main`). Read `MAINTAINING.md` end to end
first.

**Files:**
- Modify: `cyoda-go-spi/persistence.go:545-552` (`KeyValueStore`)
- Modify: `cyoda-go-spi/spitest/keyvalue.go`

**Interfaces:**
- Produces:
  ```go
  PutIfAbsent(ctx context.Context, namespace, key string, value []byte) (applied bool, err error)
  CompareAndPut(ctx context.Context, namespace, key string, expected, value []byte) (applied bool, err error)
  DeleteIfEqual(ctx context.Context, namespace, key string, expected []byte) (applied bool, err error)
  ```

- [ ] **Step 1: Replace the interface with the documented contract**

```go
// KeyValueStore holds opaque values by (namespace, key), per tenant.
//
// No operation joins a transaction: each is applied when it returns, and each
// read sees committed state, whatever transaction ctx carries.
//
// The conditional writes are atomic against every other write to the key,
// from any node. A non-nil error means the outcome is unknown: the write may
// or may not have been applied, and applied is meaningful only when err is
// nil. applied=false is returned only when the implementation knows that no
// write of this call landed; an attempt whose outcome it cannot tell (a
// timeout, an internal retry) is an error. Values are compared byte for byte,
// and a nil value is stored and compared as the empty value. A deleted key is
// absent.
type KeyValueStore interface {
	Put(ctx context.Context, namespace string, key string, value []byte) error
	Get(ctx context.Context, namespace string, key string) ([]byte, error)
	// Delete removes key. Deleting a key that is absent — never written or
	// already deleted — returns nil.
	Delete(ctx context.Context, namespace string, key string) error
	List(ctx context.Context, namespace string) (map[string][]byte, error)
	// PutIfAbsent writes value only if key is absent. applied=false: key
	// present, nothing written.
	PutIfAbsent(ctx context.Context, namespace, key string, value []byte) (applied bool, err error)
	// CompareAndPut writes value only if key is present and its stored bytes
	// equal expected. applied=false: absent or different, nothing written.
	CompareAndPut(ctx context.Context, namespace, key string, expected, value []byte) (applied bool, err error)
	// DeleteIfEqual deletes key only if it is present and its stored bytes
	// equal expected. applied=false: absent or different, nothing deleted.
	DeleteIfEqual(ctx context.Context, namespace, key string, expected []byte) (applied bool, err error)
}
```

- [ ] **Step 2: Add the conformance cases** to `spitest/keyvalue.go`. Register them in `runKeyValueSuite` after `Value/BinarySafe`:

```go
	runSubtest(t, h, tracker, "Conditional/PutIfAbsent", testKVPutIfAbsent)
	runSubtest(t, h, tracker, "Conditional/CompareAndPut", testKVCompareAndPut)
	runSubtest(t, h, tracker, "Conditional/DeleteIfEqual", testKVDeleteIfEqual)
	runSubtest(t, h, tracker, "Conditional/DeletedKeyIsAbsent", testKVConditionalDeletedKey)
	runSubtest(t, h, tracker, "Conditional/ConcurrentPutIfAbsent", testKVConcurrentPutIfAbsent)
	runSubtest(t, h, tracker, "Conditional/ConcurrentCompareAndPut", testKVConcurrentCompareAndPut)
	runSubtest(t, h, tracker, "Conditional/AtomicAgainstPlainWrites", testKVConditionalAgainstPlain)
	runSubtest(t, h, tracker, "Conditional/Isolation", testKVConditionalIsolation)
	runSubtest(t, h, tracker, "Conditional/EmptyValue", testKVConditionalEmptyValue)
	runSubtest(t, h, tracker, "NoTransactionJoin", testKVNoTransactionJoin)
```

Add `"fmt"`, `"sync"` to the imports. The cases:

```go
// applied asserts a conditional write returned no error and the given applied.
func applied(t *testing.T, want bool, got bool, err error) {
	t.Helper()
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func testKVPutIfAbsent(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	kv, err := h.Factory.KeyValueStore(ctx)
	require.NoError(t, err)
	ok, err := kv.PutIfAbsent(ctx, "ns", "k", []byte("v1"))
	applied(t, true, ok, err)
	ok, err = kv.PutIfAbsent(ctx, "ns", "k", []byte("v2"))
	applied(t, false, ok, err)
	got, err := kv.Get(ctx, "ns", "k")
	require.NoError(t, err)
	require.Equal(t, []byte("v1"), got)
}

func testKVCompareAndPut(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	kv, _ := h.Factory.KeyValueStore(ctx)
	ok, err := kv.CompareAndPut(ctx, "ns", "k", []byte("x"), []byte("y"))
	applied(t, false, ok, err) // absent
	_, err = kv.Get(ctx, "ns", "k")
	require.ErrorIs(t, err, spi.ErrNotFound)
	require.NoError(t, kv.Put(ctx, "ns", "k", []byte("v1")))
	ok, err = kv.CompareAndPut(ctx, "ns", "k", []byte("other"), []byte("v2"))
	applied(t, false, ok, err) // different
	ok, err = kv.CompareAndPut(ctx, "ns", "k", []byte("v1"), []byte("v2"))
	applied(t, true, ok, err)
	got, _ := kv.Get(ctx, "ns", "k")
	require.Equal(t, []byte("v2"), got)
}

func testKVDeleteIfEqual(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	kv, _ := h.Factory.KeyValueStore(ctx)
	ok, err := kv.DeleteIfEqual(ctx, "ns", "k", []byte("v"))
	applied(t, false, ok, err) // absent
	require.NoError(t, kv.Put(ctx, "ns", "k", []byte("v1")))
	ok, err = kv.DeleteIfEqual(ctx, "ns", "k", []byte("stale"))
	applied(t, false, ok, err)
	got, err := kv.Get(ctx, "ns", "k")
	require.NoError(t, err)
	require.Equal(t, []byte("v1"), got)
	ok, err = kv.DeleteIfEqual(ctx, "ns", "k", []byte("v1"))
	applied(t, true, ok, err)
	_, err = kv.Get(ctx, "ns", "k")
	require.ErrorIs(t, err, spi.ErrNotFound)
}

// A key written and then deleted is absent to every conditional write; a
// backend that soft-deletes must not compare against the deleted value.
func testKVConditionalDeletedKey(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	kv, _ := h.Factory.KeyValueStore(ctx)
	require.NoError(t, kv.Put(ctx, "ns", "k", []byte("old")))
	require.NoError(t, kv.Delete(ctx, "ns", "k"))
	ok, err := kv.CompareAndPut(ctx, "ns", "k", []byte("old"), []byte("x"))
	applied(t, false, ok, err)
	ok, err = kv.DeleteIfEqual(ctx, "ns", "k", []byte("old"))
	applied(t, false, ok, err)
	ok, err = kv.PutIfAbsent(ctx, "ns", "k", []byte("new"))
	applied(t, true, ok, err)
	got, err := kv.Get(ctx, "ns", "k")
	require.NoError(t, err)
	require.Equal(t, []byte("new"), got)
	all, err := kv.List(ctx, "ns")
	require.NoError(t, err)
	require.Equal(t, map[string][]byte{"k": []byte("new")}, all)
}

const conditionalRacers = 8

// At most one of N concurrent PutIfAbsent calls with distinct values is
// applied, and the stored value is that caller's.
func testKVConcurrentPutIfAbsent(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	kv, _ := h.Factory.KeyValueStore(ctx)
	for round := 0; round < 5; round++ {
		key := fmt.Sprintf("k%d", round)
		var wg sync.WaitGroup
		results := make([]bool, conditionalRacers)
		errs := make([]error, conditionalRacers)
		for i := range conditionalRacers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = kv.PutIfAbsent(ctx, "ns", key, []byte(fmt.Sprintf("v%d", i)))
			}()
		}
		wg.Wait()
		winner := -1
		for i := range conditionalRacers {
			if errs[i] == nil && results[i] {
				require.Equal(t, -1, winner, "two PutIfAbsent calls applied")
				winner = i
			}
		}
		if winner < 0 {
			continue // every racer errored: allowed, nothing to assert
		}
		want := []byte(fmt.Sprintf("v%d", winner))
		got, err := kv.Get(ctx, "ns", key)
		require.NoError(t, err)
		require.Equal(t, want, got)
		all, err := kv.List(ctx, "ns")
		require.NoError(t, err)
		require.Equal(t, want, all[key])
	}
}

// At most one of N concurrent CompareAndPut calls from the same expected
// value is applied, and the stored value is that caller's.
func testKVConcurrentCompareAndPut(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	kv, _ := h.Factory.KeyValueStore(ctx)
	for round := 0; round < 5; round++ {
		key := fmt.Sprintf("k%d", round)
		require.NoError(t, kv.Put(ctx, "ns", key, []byte("start")))
		var wg sync.WaitGroup
		results := make([]bool, conditionalRacers)
		errs := make([]error, conditionalRacers)
		for i := range conditionalRacers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = kv.CompareAndPut(ctx, "ns", key, []byte("start"), []byte(fmt.Sprintf("v%d", i)))
			}()
		}
		wg.Wait()
		winner := -1
		for i := range conditionalRacers {
			if errs[i] == nil && results[i] {
				require.Equal(t, -1, winner, "two CompareAndPut calls applied")
				winner = i
			}
		}
		if winner < 0 {
			continue
		}
		got, err := kv.Get(ctx, "ns", key)
		require.NoError(t, err)
		require.Equal(t, []byte(fmt.Sprintf("v%d", winner)), got)
	}
}

// Conditional and plain writes are atomic against each other. In either
// order of the two racing calls the end state is the same, so it is asserted
// directly.
func testKVConditionalAgainstPlain(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	kv, _ := h.Factory.KeyValueStore(ctx)
	for round := 0; round < 20; round++ {
		key := fmt.Sprintf("d%d", round)
		require.NoError(t, kv.Put(ctx, "ns", key, []byte("prev")))
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = kv.Delete(ctx, "ns", key) }()
		go func() { defer wg.Done(); _, _ = kv.CompareAndPut(ctx, "ns", key, []byte("prev"), []byte("x")) }()
		wg.Wait()
		_, err := kv.Get(ctx, "ns", key)
		require.ErrorIs(t, err, spi.ErrNotFound, "round %d: a CompareAndPut outlived a Delete", round)
	}
	for round := 0; round < 20; round++ {
		key := fmt.Sprintf("p%d", round)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); require.NoError(t, kv.Put(ctx, "ns", key, []byte("v"))) }()
		go func() { defer wg.Done(); _, _ = kv.PutIfAbsent(ctx, "ns", key, []byte("w")) }()
		wg.Wait()
		got, err := kv.Get(ctx, "ns", key)
		require.NoError(t, err)
		require.Equal(t, []byte("v"), got, "round %d: PutIfAbsent overwrote a Put", round)
	}
}

// A conditional write never sees or changes the same key in another tenant
// or another namespace.
func testKVConditionalIsolation(t *testing.T, h Harness) {
	tA, tB := h.NewTenant(), h.NewTenant()
	ctxA, ctxB := tenantContext(tA), tenantContext(tB)
	kvA, _ := h.Factory.KeyValueStore(ctxA)
	kvB, _ := h.Factory.KeyValueStore(ctxB)
	require.NoError(t, kvB.Put(ctxB, "ns", "k", []byte("B")))
	require.NoError(t, kvA.Put(ctxA, "other", "k", []byte("A-other")))
	ok, err := kvA.PutIfAbsent(ctxA, "ns", "k", []byte("A"))
	applied(t, true, ok, err)
	ok, err = kvA.CompareAndPut(ctxA, "ns", "k", []byte("B"), []byte("x"))
	applied(t, false, ok, err)
	ok, err = kvA.DeleteIfEqual(ctxA, "ns", "k", []byte("A-other"))
	applied(t, false, ok, err)
	gotB, _ := kvB.Get(ctxB, "ns", "k")
	require.Equal(t, []byte("B"), gotB)
	gotOther, _ := kvA.Get(ctxA, "other", "k")
	require.Equal(t, []byte("A-other"), gotOther)
}

func testKVConditionalEmptyValue(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	kv, _ := h.Factory.KeyValueStore(ctx)
	ok, err := kv.PutIfAbsent(ctx, "ns", "k", nil)
	applied(t, true, ok, err)
	ok, err = kv.CompareAndPut(ctx, "ns", "k", []byte{}, []byte("v"))
	applied(t, true, ok, err)
}

// No key-value operation joins a transaction: a write made with a context
// that carries an open transaction is visible outside it at once and survives
// its rollback, and a read with such a context sees a value committed after
// the transaction began.
func testKVNoTransactionJoin(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	kv, _ := h.Factory.KeyValueStore(ctx)
	tm, err := h.Factory.TransactionManager(ctx)
	require.NoError(t, err)
	txID, txCtx := beginGuarded(t, tm, ctx)

	require.NoError(t, kv.Put(txCtx, "ns", "put", []byte("v")))
	ok, err := kv.PutIfAbsent(txCtx, "ns", "pia", []byte("v"))
	applied(t, true, ok, err)
	require.NoError(t, kv.Put(ctx, "ns", "cap", []byte("a")))
	ok, err = kv.CompareAndPut(txCtx, "ns", "cap", []byte("a"), []byte("b"))
	applied(t, true, ok, err)
	require.NoError(t, kv.Put(ctx, "ns", "del", []byte("v")))
	require.NoError(t, kv.Delete(txCtx, "ns", "del"))
	require.NoError(t, kv.Put(ctx, "ns", "die", []byte("v")))
	ok, err = kv.DeleteIfEqual(txCtx, "ns", "die", []byte("v"))
	applied(t, true, ok, err)

	// Visible outside the transaction before it ends.
	for k, want := range map[string]string{"put": "v", "pia": "v", "cap": "b"} {
		got, err := kv.Get(ctx, "ns", k)
		require.NoError(t, err, k)
		require.Equal(t, []byte(want), got, k)
	}
	// A read inside sees a value committed after the transaction began.
	require.NoError(t, kv.Put(ctx, "ns", "late", []byte("late")))
	got, err := kv.Get(txCtx, "ns", "late")
	require.NoError(t, err)
	require.Equal(t, []byte("late"), got)
	all, err := kv.List(txCtx, "ns")
	require.NoError(t, err)
	require.Equal(t, []byte("late"), all["late"])

	require.NoError(t, tm.Rollback(txCtx, txID))
	for k, want := range map[string]string{"put": "v", "pia": "v", "cap": "b"} {
		got, err := kv.Get(ctx, "ns", k)
		require.NoError(t, err, k)
		require.Equal(t, []byte(want), got, k)
	}
	for _, k := range []string{"del", "die"} {
		_, err := kv.Get(ctx, "ns", k)
		require.ErrorIs(t, err, spi.ErrNotFound, k)
	}
}
```

- [ ] **Step 3: Build and vet the SPI**

Run (in `cyoda-go-spi`): `go build ./... && go vet ./...`
Expected: both succeed. (`spitest` has no harness in this repo; the cases
first run in Tasks 2 and 3. That is by design, see `MAINTAINING.md`.)

- [ ] **Step 4: Point the cyoda-go worktree at the local SPI**

Run (in the cyoda-go worktree): `go work edit -use /Users/paul/go-projects/cyoda-light/cyoda-go-spi`
Expected: `go.work` gains the line. **Never commit it.** Check with
`git diff --stat go.work` before every commit in later tasks: it must not be
staged.

- [ ] **Step 5: Commit, push, open the SPI PR into `main`**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git checkout -b feat/kv-conditional-writes
git add persistence.go spitest/keyvalue.go
git commit -m "feat(kv)!: conditional writes; no KV operation joins a transaction

PutIfAbsent, CompareAndPut and DeleteIfEqual, atomic across nodes, with
conformance cases. Needed by cyoda-go's tenant-scoped M2M client store
(cyoda-go #650).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
git push -u origin feat/kv-conditional-writes
gh pr create --repo Cyoda/cyoda-go-spi --base main --title "feat(kv)!: conditional key-value writes; KV operations never join a transaction" --body-file <scratchpad file with summary, contract, and "Consumers: cyoda-go #650; cassandra issue to follow">
```

Record the PR number in the SDD ledger. Do not tag. Do not merge: Paul
approves SPI merges.

---

### Task 2: memory and sqlite plugins implement the conditional writes

**Files:**
- Modify: `plugins/memory/kv_store.go`
- Modify: `plugins/sqlite/kv_store.go`
- Test: the existing conformance tests of both plugins run the new `spitest` cases (find them with `grep -rln StoreFactoryConformance plugins/memory plugins/sqlite`).

**Interfaces:**
- Consumes: Task 1's three methods.

- [ ] **Step 1: Run the conformance suites and watch the build fail**

Run: `cd plugins/memory && go test ./... 2>&1 | head -20`
Expected: build failure, `*KeyValueStore does not implement spi.KeyValueStore (missing method CompareAndPut)`. Same in `plugins/sqlite` for `*kvStore`.

- [ ] **Step 2: Implement in memory** (append to `plugins/memory/kv_store.go`; add `"bytes"` to imports):

```go
// ns returns the tenant's namespace map, creating it. The caller holds kvMu.
func (s *KeyValueStore) ns(namespace string) map[string][]byte {
	if s.factory.kvData[s.tenant] == nil {
		s.factory.kvData[s.tenant] = make(map[string]map[string][]byte)
	}
	if s.factory.kvData[s.tenant][namespace] == nil {
		s.factory.kvData[s.tenant][namespace] = make(map[string][]byte)
	}
	return s.factory.kvData[s.tenant][namespace]
}

func (s *KeyValueStore) PutIfAbsent(ctx context.Context, namespace, key string, value []byte) (bool, error) {
	s.factory.kvMu.Lock()
	defer s.factory.kvMu.Unlock()
	ns := s.ns(namespace)
	if _, ok := ns[key]; ok {
		return false, nil
	}
	ns[key] = append([]byte{}, value...)
	return true, nil
}

func (s *KeyValueStore) CompareAndPut(ctx context.Context, namespace, key string, expected, value []byte) (bool, error) {
	s.factory.kvMu.Lock()
	defer s.factory.kvMu.Unlock()
	ns := s.ns(namespace)
	cur, ok := ns[key]
	if !ok || !bytes.Equal(cur, expected) {
		return false, nil
	}
	ns[key] = append([]byte{}, value...)
	return true, nil
}

func (s *KeyValueStore) DeleteIfEqual(ctx context.Context, namespace, key string, expected []byte) (bool, error) {
	s.factory.kvMu.Lock()
	defer s.factory.kvMu.Unlock()
	ns := s.ns(namespace)
	cur, ok := ns[key]
	if !ok || !bytes.Equal(cur, expected) {
		return false, nil
	}
	delete(ns, key)
	return true, nil
}
```

Refactor `Put` to use `s.ns(namespace)` (same behaviour). `bytes.Equal(nil, []byte{})` is true, which satisfies the nil-equals-empty rule.

- [ ] **Step 3: Implement in sqlite** (append to `plugins/sqlite/kv_store.go`). A nil value would violate `NOT NULL`, so normalise it:

```go
// nonNil stores a nil value as the empty value (spi.KeyValueStore contract).
func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// affectedOne reports whether res changed exactly one row.
func affectedOne(res sql.Result, op string) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to read rows affected by %s: %w", op, err)
	}
	return n == 1, nil
}

func (s *kvStore) PutIfAbsent(ctx context.Context, namespace, key string, value []byte) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO kv_store (tenant_id, namespace, key, value) VALUES (?, ?, ?, ?)
		 ON CONFLICT (tenant_id, namespace, key) DO NOTHING`,
		string(s.tenantID), namespace, key, nonNil(value))
	if err != nil {
		return false, fmt.Errorf("failed to put-if-absent key %s/%s: %w", namespace, key, err)
	}
	return affectedOne(res, "put-if-absent")
}

func (s *kvStore) CompareAndPut(ctx context.Context, namespace, key string, expected, value []byte) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE kv_store SET value = ? WHERE tenant_id = ? AND namespace = ? AND key = ? AND value = ?`,
		nonNil(value), string(s.tenantID), namespace, key, nonNil(expected))
	if err != nil {
		return false, fmt.Errorf("failed to compare-and-put key %s/%s: %w", namespace, key, err)
	}
	return affectedOne(res, "compare-and-put")
}

func (s *kvStore) DeleteIfEqual(ctx context.Context, namespace, key string, expected []byte) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM kv_store WHERE tenant_id = ? AND namespace = ? AND key = ? AND value = ?`,
		string(s.tenantID), namespace, key, nonNil(expected))
	if err != nil {
		return false, fmt.Errorf("failed to delete-if-equal key %s/%s: %w", namespace, key, err)
	}
	return affectedOne(res, "delete-if-equal")
}
```

Make `Put` use `nonNil(value)` too, so a nil `Put` no longer fails the `NOT NULL` constraint.

- [ ] **Step 4: Run both conformance suites**

Run: `cd plugins/memory && go test ./...` then `cd plugins/sqlite && go test ./...`
Expected: PASS, including every `KeyValue/Conditional/*` and `KeyValue/NoTransactionJoin` subtest (memory and sqlite already ignore the transaction).

- [ ] **Step 5: Commit**

```bash
git add plugins/memory/kv_store.go plugins/sqlite/kv_store.go
git commit -m "feat(memory,sqlite): conditional key-value writes

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: postgres implements the conditional writes and stops joining transactions in the KV store

**Files:**
- Modify: `plugins/postgres/kv_store.go`
- Modify: `plugins/postgres/store_factory.go:228-235` (`KeyValueStore` only)
- Test: `plugins/postgres` conformance test (`grep -ln StoreFactoryConformance plugins/postgres/*_test.go`)

**Interfaces:**
- Consumes: Task 1.
- Produces: `StoreFactory.KeyValueStore` returns a store on `unjoinedQuerier{pool, acquireTimeout, what: "key-value"}`. `StoreFactory.WorkflowStore` is unchanged (still `f.querier()`).

- [ ] **Step 1: Run the postgres conformance suite and watch it fail to build**

Run: `cd plugins/postgres && go test -run TestConformance ./... 2>&1 | head` (use the test name the grep found)
Expected: build failure, missing `CompareAndPut`.

- [ ] **Step 2: Implement the methods** (append to `plugins/postgres/kv_store.go`):

```go
// nonNil stores a nil value as the empty value (spi.KeyValueStore contract).
func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

func (s *kvStore) PutIfAbsent(ctx context.Context, namespace, key string, value []byte) (bool, error) {
	tag, err := s.q.Exec(ctx,
		`INSERT INTO kv_store (tenant_id, namespace, key, value) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (tenant_id, namespace, key) DO NOTHING`,
		string(s.tenantID), namespace, key, nonNil(value))
	if err != nil {
		return false, fmt.Errorf("failed to put-if-absent key %s/%s: %w", namespace, key, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *kvStore) CompareAndPut(ctx context.Context, namespace, key string, expected, value []byte) (bool, error) {
	tag, err := s.q.Exec(ctx,
		`UPDATE kv_store SET value = $5 WHERE tenant_id = $1 AND namespace = $2 AND key = $3 AND value = $4`,
		string(s.tenantID), namespace, key, nonNil(expected), nonNil(value))
	if err != nil {
		return false, fmt.Errorf("failed to compare-and-put key %s/%s: %w", namespace, key, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *kvStore) DeleteIfEqual(ctx context.Context, namespace, key string, expected []byte) (bool, error) {
	tag, err := s.q.Exec(ctx,
		`DELETE FROM kv_store WHERE tenant_id = $1 AND namespace = $2 AND key = $3 AND value = $4`,
		string(s.tenantID), namespace, key, nonNil(expected))
	if err != nil {
		return false, fmt.Errorf("failed to delete-if-equal key %s/%s: %w", namespace, key, err)
	}
	return tag.RowsAffected() == 1, nil
}
```

Check the `Querier` interface's `Exec` return type (`grep -n 'type Querier' -A8 plugins/postgres/*.go`); it returns `pgconn.CommandTag`. Make `Put` use `nonNil(value)` too.

- [ ] **Step 3: Run the suite; `KeyValue/NoTransactionJoin` must fail**

Run: the conformance test.
Expected: the conditional cases PASS; `KeyValue/NoTransactionJoin` FAILS (the KV store still joins the transaction, so the rollback discards the writes).

- [ ] **Step 4: Move only the KV store to the unjoined querier** in `store_factory.go`:

```go
// KeyValueStore never joins the caller's transaction (spi.KeyValueStore
// contract): each operation is applied when it returns. Inside a transaction
// the connection acquire is bounded, so a saturated pool fails the call with
// a retryable storage-unavailable error instead of waiting without end.
func (f *StoreFactory) KeyValueStore(ctx context.Context) (spi.KeyValueStore, error) {
	tid, err := resolveTenant(ctx)
	if err != nil {
		return nil, err
	}
	return &kvStore{q: unjoinedQuerier{pool: f.pool, acquireTimeout: f.cfg.AcquireTimeout, what: "key-value"}, tenantID: tid}, nil
}
```

Leave `WorkflowStore` (`kv := &kvStore{q: f.querier(), …}`) exactly as it is: the engine reads workflows inside entity transactions.

- [ ] **Step 5: Run the postgres conformance suite and the postgres plugin tests**

Run: `cd plugins/postgres && go test ./...`
Expected: PASS, all `KeyValue/*` cases included.

- [ ] **Step 6: Commit**

```bash
git add plugins/postgres/kv_store.go plugins/postgres/store_factory.go
git commit -m "feat(postgres): conditional key-value writes; the KV store never joins a transaction

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: `SYSTEM` is not an API tenant

**Files:**
- Modify: `internal/common/tenant_id.go` (add `ValidateAPITenantID`)
- Test: `internal/common/tenant_id_test.go`
- Modify: `internal/auth/validator.go:148`, `internal/auth/operator_token.go:32`, `app/config.go` (`ValidateIAM`)
- Test: `internal/auth/validator_test.go:202-203`, `internal/auth/operator_token_test.go`, `app/config_iam_test.go`, `internal/grpc/interceptor_test.go`, `internal/e2e/auth_failures_test.go:248`

**Interfaces:**
- Produces: `func ValidateAPITenantID(id spi.TenantID) error` in `internal/common`; `var ErrReservedTenantID = errors.New("reserved tenant id")`.

- [ ] **Step 1: Failing test for the helper** (append to `internal/common/tenant_id_test.go`):

```go
func TestValidateAPITenantID(t *testing.T) {
	for _, ok := range []string{"acme", "PLATFORM", "a.b_c-d", "systems", "SYSTEM-1", strings.Repeat("a", 100)} {
		if err := common.ValidateAPITenantID(spi.TenantID(ok)); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"SYSTEM", "system", "System", "sYsTeM"} {
		err := common.ValidateAPITenantID(spi.TenantID(bad))
		if !errors.Is(err, common.ErrReservedTenantID) || strings.Contains(err.Error(), bad) {
			t.Errorf("%q: %v (want ErrReservedTenantID, no echo)", bad, err)
		}
	}
	for _, bad := range []string{"", ".x", "a/b", strings.Repeat("a", 101)} {
		if !errors.Is(common.ValidateAPITenantID(spi.TenantID(bad)), common.ErrInvalidTenantID) {
			t.Errorf("%q: want ErrInvalidTenantID", bad)
		}
	}
}
```

Run: `go test ./internal/common/ -run TestValidateAPITenantID` → FAIL (undefined).

- [ ] **Step 2: Implement** (append to `internal/common/tenant_id.go`; import `strings`):

```go
// ErrReservedTenantID reports a tenant id that is valid but reserved for the
// machinery: no caller may act in it or address it.
var ErrReservedTenantID = errors.New("reserved tenant id")

// ValidateAPITenantID reports whether id is a tenant a caller may act in or
// address: the tenant grammar, and not SYSTEM in any letter case. SYSTEM is
// the machinery's tenant; its store holds the cluster's auth state. Stored
// records and internal contexts carry SYSTEM legitimately, so
// ValidateTenantID itself accepts it. The error never contains id.
func ValidateAPITenantID(id spi.TenantID) error {
	if err := ValidateTenantID(id); err != nil {
		return err
	}
	if strings.EqualFold(string(id), string(spi.SystemTenantID)) {
		return fmt.Errorf("%w: SYSTEM is the machinery's tenant", ErrReservedTenantID)
	}
	return nil
}
```

Run the test → PASS.

- [ ] **Step 3: Failing tests at the doors**
  - `internal/auth/validator_test.go`: remove `SYSTEM` from `TestValidator_AcceptsShippedTenantShapes` (`:202-203`) and add `TestValidator_RefusesSystemTenant`: for `SYSTEM`, `system`, `System`, a validly signed token with that `caas_org_id` must fail validation, and the error must wrap `common.ErrReservedTenantID`. Build the token with the file's existing `signTestToken` helper.
  - `internal/auth/operator_token_test.go` (or the file that tests `ValidateOperatorTokenRequest`): `ValidateOperatorTokenRequest` with `Tenant: "SYSTEM"` and `"system"` returns an error wrapping `ErrReservedTenantID`.
  - `app/config_iam_test.go`: `ValidateIAM` with `Mode: "mock"` and `MockTenantID` of `"SYSTEM"`, `"system"`, `""` and `"a/b"` fails; `"mock-tenant"` passes. Start from the test file's existing valid IAM config.
  - `internal/grpc/interceptor_test.go`: a stream and a unary call with a token whose `caas_org_id` is `SYSTEM` answer `codes.Unauthenticated`. Follow the pattern at `interceptor_test.go:340` (it signs with `auth.Sign`).
  - `internal/e2e/auth_failures_test.go`: remove `"SYSTEM"` from `TestAuth_AcceptedTenantShapesStillAuthenticate` (`:248`), and add `TestAuth_SystemTenantClaim_401` modelled on `TestAuth_TenantClaimOutsideGrammar_401` (`:217`): for `SYSTEM`, `system` and `System`, `GET /api/model/` with `"Bearer "+mintFirstPartyToken(t, "e2e-tenant-probe", tenant)` answers `401`.

Run: `go test ./internal/auth/ ./app/ ./internal/grpc/ -run 'System|IAM|Operator'` → FAIL.

- [ ] **Step 4: Apply the rule at the doors**
  - `internal/auth/validator.go:148`: `common.ValidateTenantID` → `common.ValidateAPITenantID`; update the comment above it to say the door also refuses the machinery's tenant.
  - `internal/auth/operator_token.go:32`: same replacement.
  - `app/config.go` `ValidateIAM`: before the `if iam.Mode == "mock"` return, add

    ```go
	// Unconditional: the mock principal's tenant is a tenant every mock
	// request acts in, so it must be one a caller may act in.
	if err := common.ValidateAPITenantID(spi.TenantID(iam.MockTenantID)); err != nil {
		return fmt.Errorf("CYODA_IAM_MOCK_TENANT_ID is not a valid tenant id: %w", err)
	}
    ```
    (import `internal/common` if needed; check for an import cycle with `go build ./app/`).

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/common/ ./internal/auth/ ./app/ ./internal/grpc/` → PASS. Then `go test -timeout 30m -run 'TestAuth_' ./internal/e2e/` → PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/common/tenant_id.go internal/common/tenant_id_test.go internal/auth/validator.go internal/auth/validator_test.go internal/auth/operator_token.go internal/auth/operator_token_test.go app/config.go app/config_iam_test.go internal/grpc/interceptor_test.go internal/e2e/auth_failures_test.go
git commit -m "feat(auth)!: SYSTEM is not an API tenant — refused at the token claim, cyoda token and the mock tenant

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

(Use the real test file names the greps find; stage only those.)

---

### Task 5: the client-id grammar

**Files:**
- Modify: `internal/auth/kv_m2m_codec.go:34,43-44`
- Test: `internal/auth/kv_m2m_codec_internal_test.go` (or a new `internal/auth/client_id_test.go`)
- Modify: `api/openapi.yaml` (the four client-id patterns), then `go generate ./api`
- Test: `internal/auth/validator_test.go` (cgen and act.sub ids)

**Interfaces:**
- Produces: `auth.ValidClientID(id string) bool` = tenant grammar and not `system` in any letter case.

- [ ] **Step 1: Failing table test** (`internal/auth/client_id_test.go`, package `auth_test`):

```go
func TestValidClientID(t *testing.T) {
	for _, ok := range []string{"C1", "GC2693985CC61NUU", "backend", "Backend", "order-service", "compute.node_2", strings.Repeat("a", 100), "systems", "my-system"} {
		if !auth.ValidClientID(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "-x", ".x", "_x", "a:b", "a/b", "a b", "a%2Db", strings.Repeat("a", 101), "system", "SYSTEM", "System", "é"} {
		if auth.ValidClientID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
```

Run → FAIL (`backend-like` ids with `-`/`.` refused, `system` accepted).

- [ ] **Step 2: Implement** in `kv_m2m_codec.go`:

```go
// clientIDGrammar is the tenant grammar: a client id may be chosen by its
// tenant, and appears in URLs and in Basic credentials, so it uses only
// characters that never need encoding.
var clientIDGrammar = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// ValidClientID reports whether id is a client id: the client-id grammar,
// and not "system" in any letter case — a client's id is its user id on its
// tokens and in audit records, where "system" is reserved.
func ValidClientID(id string) bool {
	return clientIDGrammar.MatchString(id) && !strings.EqualFold(id, common.ReservedSystemUserID)
}
```

(`common.ReservedSystemUserID` exists at `internal/common/user_id.go`; confirm the name with grep.)

- [ ] **Step 3: Validator coverage** — in `internal/auth/validator_test.go` add `TestValidator_ClientIDGrammarOnClientClaims`: a `cgen` token whose `caas_user_id` is `order-service` validates; one whose `caas_user_id` is `system` fails; an OBO token whose `act.sub` is `compute.node_2` validates; one whose `act.sub` is `SYSTEM` fails. Run → PASS after Step 2 (the validator already calls `ValidClientID`).

- [ ] **Step 4: OpenAPI patterns** — in `api/openapi.yaml` change the four client-id schemas (`/clients/{clientId}` DELETE and PUT parameters, `TechnicalUserCredentialsDto.client_id`, `TechnicalUserDto.clientId`) from `pattern: ^[A-Za-z0-9]+$` to `pattern: "^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$"`, and append to each description: `Letters, digits, ".", "_" and "-", starting with a letter or digit; case significant; "system" in any letter case is reserved.` Change `TechnicalUserCredentialsDto.client_id`'s description from "Generated ids are 16 characters…" to `"Chosen by the caller (query parameter clientId) or generated: 16 characters from 0-9 and A-V."`. Run `go generate ./api` and `go build ./...`.

- [ ] **Step 5: Run** `go test ./internal/auth/ ./internal/domain/account/ ./api/...` → PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/auth/kv_m2m_codec.go internal/auth/client_id_test.go internal/auth/validator_test.go api/openapi.yaml api/generated.go
git commit -m "feat(auth)!: client ids follow the tenant grammar; system is reserved

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: `internal/tenantroute` — the `/tenants/{tenant}` route group

**Files:**
- Create: `internal/tenantroute/tenantroute.go`
- Test: `internal/tenantroute/tenantroute_test.go`

**Interfaces:**
- Consumes: `common.ValidateAPITenantID` (Task 4).
- Produces:
  ```go
  const Prefix = "/tenants/{tenant}/"
  func Handle(mux *http.ServeMux, pattern string, h http.Handler, refuse func(http.ResponseWriter, *http.Request))
  func Addressed(ctx context.Context) (spi.TenantID, bool)
  ```

- [ ] **Step 1: Failing tests**

```go
package tenantroute_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/tenantroute"
)

// mount builds an outer mux that strips /api, as app.go does, and returns
// what the route saw.
func mount(t *testing.T) (http.Handler, *string) {
	t.Helper()
	var seen string
	inner := http.NewServeMux()
	tenantroute.Handle(inner, "/tenants/{tenant}/oauth/token",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tn, ok := tenantroute.Addressed(r.Context())
			if !ok {
				t.Error("no addressed tenant")
			}
			seen = string(tn)
			w.WriteHeader(http.StatusOK)
		}),
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadRequest) })
	outer := http.NewServeMux()
	outer.Handle("/api/", http.StripPrefix("/api", inner))
	return outer, &seen
}

func serve(h http.Handler, path string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://h"+path, nil))
	return rec.Code
}

func TestHandle_AcceptsEveryGrammarCharacter(t *testing.T) {
	h, seen := mount(t)
	for _, tn := range []string{"acme", "acme.eu-1_x", "Acme", "9f8c7b6a", "PLATFORM", "clients", "oauth", "tenants", "model"} {
		if code := serve(h, "/api/tenants/"+tn+"/oauth/token"); code != http.StatusOK || *seen != tn {
			t.Errorf("%q: %d, saw %q", tn, code, *seen)
		}
	}
}

func TestHandle_TenantLengthBound(t *testing.T) {
	h, _ := mount(t)
	if code := serve(h, "/api/tenants/"+strings.Repeat("a", 100)+"/oauth/token"); code != http.StatusOK {
		t.Errorf("100 chars: %d", code)
	}
	if code := serve(h, "/api/tenants/"+strings.Repeat("a", 101)+"/oauth/token"); code != http.StatusBadRequest {
		t.Errorf("101 chars: %d", code)
	}
}

func TestHandle_RefusesBadTenantsAndEncodedPaths(t *testing.T) {
	h, _ := mount(t)
	for _, p := range []string{
		"/api/tenants/SYSTEM/oauth/token",
		"/api/tenants/system/oauth/token",
		"/api/tenants/-x/oauth/token",
		"/api/tenants/%61cme/oauth/token",   // encoded tenant
		"/api/%74enants/acme/oauth/token",   // encoded literal segment
		"/api/tenants/acme/oauth/%74oken",   // encoded literal segment
		"/api/tenants/a%2Fb/oauth/token",    // encoded slash
		"/api/tenants/a%20b/oauth/token",    // encoded reserved char: RawPath empty, grammar refuses
	} {
		if code := serve(h, p); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", p, code)
		}
	}
}

func TestAddressed_AbsentOutsideTheGroup(t *testing.T) {
	if _, ok := tenantroute.Addressed(httptest.NewRequest(http.MethodGet, "/", nil).Context()); ok {
		t.Fatal("addressed tenant outside the group")
	}
}

func TestHandle_PanicsOnAPatternOutsideTheGroup(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	tenantroute.Handle(http.NewServeMux(), "/oauth/token", http.NotFoundHandler(), nil)
}
```

Run: `go test ./internal/tenantroute/` → FAIL (package missing).

- [ ] **Step 2: Implement**

```go
// Package tenantroute is the /tenants/{tenant}/… route group: routes that
// address a tenant by name in their path. Every route of the group is
// registered through Handle, which checks the path before the route runs and
// hands the route the addressed tenant.
//
// Every route in the group today is token-free (the token endpoint). A route
// that carries a bearer token must also require the addressed tenant to equal
// the token's tenant, and answer a mismatch with 404, as a missing resource.
// That check is built with the first such route, in this package, so it
// applies to the whole group; app's route registration test fails until the
// new route is listed there.
package tenantroute

import (
	"context"
	"net/http"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// Prefix starts every pattern of the group.
const Prefix = "/tenants/{tenant}/"

type addressedKey struct{}

// Addressed returns the tenant the request's path addresses, set by Handle.
// ok is false outside the group.
func Addressed(ctx context.Context) (spi.TenantID, bool) {
	t, ok := ctx.Value(addressedKey{}).(spi.TenantID)
	return t, ok
}

// Handle registers h on mux at pattern, which must start with Prefix. Before
// h runs, the request is refused through refuse — which writes the route's
// own 400 — when its path carries any percent-encoding (Go's mux decodes
// every segment before matching, so an encoded path would otherwise reach the
// route under a spelling a gateway rule does not match; a valid path never
// needs encoding), or when the tenant segment is not an API tenant. The
// segment is taken verbatim: no folding, trimming or decoding.
func Handle(mux *http.ServeMux, pattern string, h http.Handler, refuse func(http.ResponseWriter, *http.Request)) {
	if !strings.HasPrefix(pattern, Prefix) {
		panic("tenantroute: pattern outside the group: " + pattern)
	}
	mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawPath != "" {
			refuse(w, r)
			return
		}
		tenant := spi.TenantID(r.PathValue("tenant"))
		if common.ValidateAPITenantID(tenant) != nil {
			refuse(w, r)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), addressedKey{}, tenant)))
	}))
}
```

- [ ] **Step 3: Run** `go test ./internal/tenantroute/` → PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/tenantroute/
git commit -m "feat(api): the /tenants/{tenant} route group

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: the token endpoint moves to `/tenants/{tenant}/oauth/token`

This task moves the URL everywhere at once so every suite stays green. The
store is still global here; the handler compares the found client's tenant
with the addressed one (Task 9 replaces that comparison with the lookup).

**Files:**
- Modify: `internal/auth/token.go` (handler reads the addressed tenant), `internal/auth/service.go:107`
- Create: `internal/auth/token_route.go` (`TokenPattern`, `RegisterTokenRoute`, the 400 writer)
- Modify: `app/app.go:567-581`, `app/route_registration_test.go:27`, `app/route_classification_test.go:79`, `app/app_test.go`
- Modify: `api/openapi.yaml:6105-6279` (path + `tenant` parameter + 400 text), `api/generated.go` (regenerated), `internal/domain/account/handler.go:81-83` (mock 501 signature)
- Modify: every token URL builder: `internal/e2e/token_reconciliation_test.go` (`postTokenRaw`), `internal/e2e/token_exchange_test.go:235,262`, `e2e/parity/client/keys.go:125` (`FetchClientCredentialsToken`), `e2e/parity/client/token.go:25` (`ExchangeTokenRaw`) and their callers, `cmd/compute-test-client/token_source.go`, `cmd/compute-test-client/main.go`, `e2e/parity/fixtureutil/compute_client.go` and its callers, `cmd/cyoda/help/config_registry.go` (new `CYODA_COMPUTE_TENANT_ID`), `app/config_registry_binding_test.go`
- Modify: internal auth tests that build `/oauth/token` requests: `internal/auth/token_test.go`, `delegating_test.go`, `integration_test.go`, `local_validator_integration_test.go`; `internal/domain/account/handler_test.go:98,110`; `cmd/compute-test-client/token_source_test.go:87,90`

**Interfaces:**
- Consumes: `tenantroute.Handle`, `tenantroute.Addressed` (Task 6).
- Produces:
  ```go
  // internal/auth
  const TokenPattern = "/tenants/{tenant}/oauth/token"
  func RegisterTokenRoute(mux *http.ServeMux, h http.Handler)
  // e2e/parity/client
  func FetchClientCredentialsToken(ctx context.Context, baseURL, tenant, clientID, secret string) (string, int, error)
  func (c *Client) ExchangeTokenRaw(t *testing.T, tenant, clientID, secret string, form url.Values) (int, []byte, error)
  // e2e/parity/fixtureutil
  type ComputeClientOpts struct { …; TenantID string; … }
  ```
  Env var `CYODA_COMPUTE_TENANT_ID` (compute-client side).

- [ ] **Step 1: Failing unit tests** in `internal/auth/token_test.go`:
  - Change `makeTokenRequest` to take the tenant first: `makeTokenRequest(tenant, grantType, authHeader string, extraForm url.Values)` building `"/tenants/"+tenant+"/oauth/token"`; update every call to pass `env.tenantID` (or the literal tenant the test uses).
  - Make `setupTokenEnv` and `withHandler` wrap the handler: `mux := http.NewServeMux(); auth.RegisterTokenRoute(mux, h); e.handler = mux`. Do the same at every other `auth.NewTokenHandler(...)` site in the file (`:283`, `:311`, `:938`, `:1008-1009`, `:1114`, `:1148`) through a helper `routed(h http.Handler) http.Handler`.
  - New tests:

```go
// The handler takes its tenant from the route group only; mounted outside
// it, it fails closed.
func TestToken_OutsideTheGroupIsAServerError(t *testing.T) {
	env := setupTokenEnv(t)
	h := auth.NewTokenHandler(env.keyStore, env.trustedKeyStore, env.m2mStore, testIssuer, "", testExpiry, 0)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, makeTokenRequest(env.tenantID, "client_credentials", basicAuth(env.clientID, env.clientSecret), nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rr.Code)
	}
}

// A client of tenant A presented at tenant B's URL is the same 401 as an
// unknown client.
func TestToken_ClientAtAnotherTenantsURLIsInvalidClient(t *testing.T) {
	env := setupTokenEnv(t)
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, makeTokenRequest("other-tenant", "client_credentials", basicAuth(env.clientID, env.clientSecret), nil))
	if rr.Code != http.StatusUnauthorized || decodeResponse(t, rr)["error"] != "invalid_client" {
		t.Fatalf("status %d", rr.Code)
	}
}

func TestToken_InvalidTenantSegmentIsInvalidRequest(t *testing.T) {
	env := setupTokenEnv(t)
	for _, segment := range []string{"SYSTEM", "-x", "%61cme"} {
		// httptest.NewRequest parses the target, so "%61cme" sets RawPath.
		req := httptest.NewRequest(http.MethodPost, "/tenants/"+segment+"/oauth/token", strings.NewReader("grant_type=client_credentials"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", basicAuth(env.clientID, env.clientSecret))
		rr := httptest.NewRecorder()
		env.handler.ServeHTTP(rr, req)
		body := decodeResponse(t, rr)
		if rr.Code != http.StatusBadRequest || body["error"] != "invalid_request" || body["error_description"] != "invalid tenant" {
			t.Errorf("%q: %d %v", segment, rr.Code, body)
		}
	}
}
```

Run: `go test ./internal/auth/` → FAIL (undefined `RegisterTokenRoute`).

- [ ] **Step 2: Implement the route** — `internal/auth/token_route.go`:

```go
package auth

import (
	"net/http"

	"github.com/cyoda-platform/cyoda-go/internal/tenantroute"
)

// TokenPattern is the token endpoint's route, relative to the context path.
// It has no method: the handler answers its own 405.
const TokenPattern = "/tenants/{tenant}/oauth/token"

// RegisterTokenRoute registers the token handler h on mux in the tenant
// group. A path the group refuses answers 400 invalid_request in the
// endpoint's OAuth format.
func RegisterTokenRoute(mux *http.ServeMux, h http.Handler) {
	tenantroute.Handle(mux, TokenPattern, h, func(w http.ResponseWriter, _ *http.Request) {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "invalid tenant")
	})
}
```

In `token.go` `ServeHTTP`, first statement:

```go
	tenant, ok := tenantroute.Addressed(r.Context())
	if !ok {
		writeTokenServerError(w, "tenantroute.Addressed", errors.New("token handler reached outside the tenant route group"))
		return
	}
```

After `Authenticate` succeeds (interim, replaced in Task 9):

```go
	// A client of another tenant is no client of this one.
	if client.TenantID != tenant {
		writeTokenError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}
```

Update the type comment (`tokenHandler implements POST /tenants/{tenant}/oauth/token`) and the `SetNoStore` and `dummyHash` comments that name `/oauth/token`.

In `service.go`: replace `publicMux.Handle("/oauth/token", NewTokenHandler(…))` with `RegisterTokenRoute(publicMux, NewTokenHandler(…))` and update the comment.

- [ ] **Step 3: App wiring** — `app/app.go:579-581`:

```go
		// Every method: the token handler answers its own 405, so a GET
		// never falls through to the authenticated catch-all. The auth
		// service registers the same pattern in the tenant route group.
		mux.Handle("/tenants/{tenant}/oauth/token", authSvc.Handler())
```

Update the PUBLIC comment block (`:567`). In `app/route_registration_test.go:27` set `publicMuxPatterns = map[string]bool{"/.well-known/": true, "/tenants/{tenant}/oauth/token": true}` and add, inside `TestRouteRegistration_EveryMuxRouteClassified`'s loop over `regs`:

```go
		// The tenant route group holds token-free routes only. A route that
		// carries a bearer token needs the group's tenant-equality check
		// (internal/tenantroute) built first; list it here only then.
		if strings.HasPrefix(r.pattern, "/tenants/") && !tokenFreeTenantRoutes[r.pattern] {
			t.Errorf("%s: %q is in the tenant group but not a known token-free route", r.pos, r.pattern)
		}
```

with `var tokenFreeTenantRoutes = map[string]bool{"/tenants/{tenant}/oauth/token": true}`. In `route_classification_test.go:79` change `path == "/oauth/token"` to `path == "/tenants/{tenant}/oauth/token"`.

- [ ] **Step 4: OpenAPI** — rename the path key `/oauth/token:` to `/tenants/{tenant}/oauth/token:` and add as the first parameter:

```yaml
        - name: tenant
          in: path
          required: true
          description: >
            The tenant the client belongs to. Letters, digits, ".", "_" and
            "-", starting with a letter or digit, at most 100 characters,
            case significant, never percent-encoded; SYSTEM (any letter case)
            is refused. A path that breaks these rules answers 400
            invalid_request "invalid tenant".
          schema:
            type: string
            minLength: 1
            maxLength: 100
            pattern: "^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$"
```

Add the `invalid tenant` case to the 400 response description and say in the
operation description that a client is found by (tenant, client id). Update
the trusted-keys description at `:5812`. Run `go generate ./api`; fix
`internal/domain/account/handler.go:81-83` to the new generated signature
(`GetTechnicalUserToken(w, r, tenant string, params …)`), still answering 501.

- [ ] **Step 5: URL builders**
  - `internal/e2e/token_reconciliation_test.go`: `postTokenRaw(ctx, baseURL, tenant string, form, user, pass)` building `baseURL + "/api/tenants/" + tenant + "/oauth/token"`; `postToken`/`postTokenTo` take `tenant` too. `getTokenRaw` passes `"test-tenant"` (the suite tenant, `helpers_test.go:146`) — introduce `const suiteTenant = "test-tenant"` in `helpers_test.go` and use it in `suiteTokenRaw` too. Every other wrapper listed in the research inventory (`exchangeRaw`, `oboToken*`, `statusForToken`, `tokenStatusOn`, `callbackHarness.fetchTokenFor`, `grantToken`, `computeBearer`, `keyStack.oauthToken`, `adminRequestAs`) passes the tenant its client was created in: the harness's tenant (find it where the harness signs its admin token) or `auth.PlatformTenantID` for keyStack clients (they are created in PLATFORM, `signing_keys_test.go:72-74`). Fix `token_exchange_test.go:235,262` to build the new URL.
  - `e2e/parity/client`: add the `tenant` parameter to `FetchClientCredentialsToken` and `ExchangeTokenRaw` (signatures above) and build `/api/tenants/<tenant>/oauth/token`. Update all 15 callers (`grep -rn 'FetchClientCredentialsToken(\|ExchangeTokenRaw(' e2e plugins`); each scenario knows its tenant (`parity.Tenant.ID`, or the tenant it created the client in).
  - `cmd/compute-test-client`: `newTokenSource(httpBase, tenant, clientID, clientSecret string)` with `tokenURL: strings.TrimRight(httpBase, "/") + "/api/tenants/" + tenant + "/oauth/token"`; `main.go` reads `CYODA_COMPUTE_TENANT_ID` and requires it with the other three (update the exit message and the package comment). `token_source_test.go`: assert the new path for tenant `t1`.
  - `e2e/parity/fixtureutil/compute_client.go`: add `TenantID string // tenant of ClientID` to `ComputeClientOpts`, pass `CYODA_COMPUTE_TENANT_ID=<TenantID>` in `cmd.Env`; set it at `fixtureutil.go:666` and `:1111` (`ComputeTenantID` or the tenant those launches provision for — read the surrounding code) and `compute_client.go:206` (the spec's tenant); `compute_client_test.go:128` passes `TenantID: "x"`.
  - `cmd/cyoda/help/config_registry.go`: add `{Name: "CYODA_COMPUTE_TENANT_ID", Topic: "grpc", Type: "string", Default: "", Description: "Tenant of the M2M client a compute node authenticates as; its token comes from {CYODA_COMPUTE_HTTP_BASE}/api/tenants/{tenant}/oauth/token (compute-client side)."}` next to `CYODA_COMPUTE_CLIENT_ID`, and `"CYODA_COMPUTE_TENANT_ID": true` in `app/config_registry_binding_test.go:26-28`. Fix the descriptions at `config_registry.go:99-100` (`POST /tenants/{tenant}/oauth/token`). The `config.grpc` help text follows in Task 18.
  - Remaining `/oauth/token` request builders in `internal/auth/*_test.go`, `internal/domain/account/handler_test.go`, `app/app_test.go`: switch to `/tenants/<tenant>/oauth/token` with the tenant of the client the test created.

- [ ] **Step 6: Run** `go build ./... && go vet ./... && make test` → PASS. Then `go test -timeout 30m ./internal/e2e/` → PASS. Check no request URL still names the old path: `grep -rn '"/api/oauth/token"\|"/oauth/token"\|+ *"/oauth/token' --include='*.go' . | grep -v docs/` → empty (`test/recon/oauth.go` keeps Cloud's URL: it is the only allowed hit).

- [ ] **Step 7: Commit** (stage the files you touched by name; check `git diff --cached --stat` does not list `go.work`)

```bash
git commit -m "feat(auth)!: the token endpoint moves to /tenants/{tenant}/oauth/token

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: node-local maps keyed by (tenant, client id)

**Files:**
- Modify: `internal/auth/client_bucket.go`, `internal/auth/secret_check.go:95-143`, call sites in `internal/auth/token.go:155` and `internal/auth/kv_m2m_store.go`
- Test: `internal/auth/client_bucket_test.go`, `internal/auth/secret_check_test.go`

**Interfaces:**
- Produces: `type clientKey struct{ tenant spi.TenantID; id string }` (unexported, in `client_bucket.go`); `clientBuckets.allow(k clientKey, now time.Time)`; `verifiedSecretCache.hit/put/drop(k clientKey, …)`.

- [ ] **Step 1: Failing tests** (internal tests, package `auth`):

```go
func TestClientBuckets_SameIDInTwoTenantsHaveOwnBuckets(t *testing.T) {
	b := newClientBuckets(1)
	now := time.Now()
	if ok, _ := b.allow(clientKey{"a", "backend"}, now); !ok {
		t.Fatal("first request of a refused")
	}
	if ok, _ := b.allow(clientKey{"a", "backend"}, now); ok {
		t.Fatal("a's bucket not limited")
	}
	if ok, _ := b.allow(clientKey{"b", "backend"}, now); !ok {
		t.Fatal("b throttled by a's bucket")
	}
}

func TestVerifiedSecretCache_SameIDInTwoTenantsHaveOwnEntries(t *testing.T) {
	c := newVerifiedSecretCache(10)
	sum := sha256.Sum256([]byte("s"))
	c.put(clientKey{"a", "backend"}, "hashA", sum)
	c.drop(clientKey{"b", "backend"})
	if !c.hit(clientKey{"a", "backend"}, "hashA", sum) {
		t.Fatal("b's drop evicted a's entry")
	}
	if c.hit(clientKey{"b", "backend"}, "hashA", sum) {
		t.Fatal("b hit a's entry")
	}
}
```

Run → FAIL (type mismatch).

- [ ] **Step 2: Implement**: add `clientKey` to `client_bucket.go`; change `limiters map[clientKey]*rate.Limiter`, `allow(k clientKey, …)`; in `secret_check.go` `entries map[clientKey]verifiedSecret` and the three methods take `k clientKey`. Update the comments ("a token bucket per client of a tenant"). Call sites: `token.go` `h.buckets.allow(clientKey{client.TenantID, client.ClientID}, time.Now())`; `kv_m2m_store.go` builds the key from the tenant it read the record from.

- [ ] **Step 3: Run** `go test ./internal/auth/` → PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/auth/client_bucket.go internal/auth/client_bucket_test.go internal/auth/secret_check.go internal/auth/secret_check_test.go internal/auth/token.go internal/auth/kv_m2m_store.go
git commit -m "fix(auth): key the per-client rate limit and secret cache by tenant and client id

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: the client store without the global index

Spec §4.3. Needs Tasks 1–3 (conditional writes in the memory KV the auth
tests use), 5 and 7.

**Files:**
- Modify: `internal/auth/store.go:141-180` (sentinels, `M2MClientStore`)
- Modify: `internal/auth/kv_m2m_store.go` (rewrite), `internal/auth/kv_m2m_codec.go` (drop index and decoy)
- Modify: `internal/auth/token.go` (Authenticate with tenant; remove Task 7's interim comparison)
- Modify: `internal/grpc/streaming.go:79-80,203,216-235` (Lookup with tenant, logs, delete the tenant comparison)
- Modify: `internal/domain/account/m2m_adapter.go:191` (Create without userID) and the reset error mapping
- Test: `internal/auth/kv_m2m_store_test.go` (rewrite the index tests), `internal/auth/token_test.go`, `internal/grpc/streaming_test.go:1159-1230`, `internal/domain/account/m2m_adapter_test.go`

**Interfaces:**
- Consumes: `spi.KeyValueStore.PutIfAbsent/CompareAndPut/DeleteIfEqual` (Task 1); `ValidClientID` (Task 5); `clientKey` (Task 8).
- Produces:
  ```go
  var ErrM2MClientChanged = errors.New("m2m client changed during the operation")
  type M2MClientStore interface {
	Create(ctx context.Context, tenantID spi.TenantID, clientID string, roles []string, onBehalfOf bool) (secret string, err error)
	Authenticate(ctx context.Context, tenantID spi.TenantID, clientID, secret string) (*M2MClient, error)
	Lookup(ctx context.Context, tenantID spi.TenantID, clientID string) (*M2MClient, error)
	List(ctx context.Context, tenantID spi.TenantID) ([]*M2MClient, error)
	Delete(ctx context.Context, tenantID spi.TenantID, clientID string) error
	ResetSecret(ctx context.Context, tenantID spi.TenantID, clientID string) (secret string, c *M2MClient, err error)
  }
  ```
  `ErrM2MClientExists` keeps its name; its doc says "in the tenant".

- [ ] **Step 1: Failing tests.** In `kv_m2m_store_test.go`:
  - Change every call to the new signatures (`Create(ctx, "acme", "C1", roles, false)`, `Authenticate(ctx, "acme", "C1", sec)`).
  - **Delete** the index-specific tests: `TestKVM2M_DeleteNeverTouchesAnotherTenantsIndex`, `…RecordWithoutIndexNeverAuthenticatesAndIsRemovable`, `…UndecodableIndexEntry`, `…ResetSecretWithoutAnOwnRecordIsNotFoundBeforeTheIndexIsRead`, `…DeleteRemovesADamagedIndexEntryOfTheCallersOwnClient`, `…DeleteKeepsAStoreErrorWhenOwnershipCannotBeProven`, `…DeleteKeepsAStoreErrorOnAnIndexReadFailureEvenWithAnOwnRecord`, `…DeleteRemovesADamagedRecordAndDamagedIndexEntryOfTheCallersOwnClient`, `…CreateUndoesARecordWhenTheIndexWriteFails`, `…CreateUndoesAnAmbiguousIndexWrite`, and the old `…CreateUndoesAnAmbiguousRecordWrite` (replaced below). Keep `TestKVM2M_IgnoresCallerTransaction` until Task 12.
  - Add:

```go
func TestKVM2M_SameIDInTwoTenants(t *testing.T) {
	s, _ := newM2M(t, 0)
	ctx := systemCtx()
	secA, err := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	secB, err := s.Create(ctx, "b", "backend", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := s.Authenticate(ctx, "a", "backend", secA); err != nil || c.TenantID != "a" {
		t.Fatalf("a: %v %v", c, err)
	}
	if _, err := s.Authenticate(ctx, "b", "backend", secA); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("a's secret at b: %v", err)
	}
	if err := s.Delete(ctx, "a", "backend"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "b", "backend", secB); err != nil {
		t.Fatalf("b after a's delete: %v", err)
	}
	if _, _, err := s.ResetSecret(ctx, "a", "backend"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("reset of a's deleted client: %v", err)
	}
}

func TestKVM2M_IDsDifferingInCaseAreDistinct(t *testing.T) {
	s, _ := newM2M(t, 0)
	ctx := systemCtx()
	upper, err := s.Create(ctx, "a", "Backend", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	lower, err := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "a", "Backend", lower); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatal("Backend accepts backend's secret")
	}
	if _, err := s.Authenticate(ctx, "a", "Backend", upper); err != nil {
		t.Fatal(err)
	}
}

func TestKVM2M_CreateRefusesATakenID(t *testing.T) {
	s, _ := newM2M(t, 0)
	ctx := systemCtx()
	if _, err := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false); !errors.Is(err, auth.ErrM2MClientExists) {
		t.Fatalf("second create: %v", err)
	}
}

// At the cap, a taken id is still 409, not the cap.
func TestKVM2M_ExistsBeforeCap(t *testing.T) {
	s, _ := newM2M(t, 1)
	ctx := systemCtx()
	if _, err := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false); !errors.Is(err, auth.ErrM2MClientExists) {
		t.Fatalf("taken id at the cap: %v", err)
	}
}

func TestKVM2M_RecreateAfterDelete(t *testing.T) {
	s, _ := newM2M(t, 0)
	ctx := systemCtx()
	old, _ := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false)
	oldC, _ := s.Authenticate(ctx, "a", "backend", old)
	if err := s.Delete(ctx, "a", "backend"); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Create(ctx, "a", "backend", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "a", "backend", old); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatal("old secret works on the new client")
	}
	newC, err := s.Authenticate(ctx, "a", "backend", fresh)
	if err != nil {
		t.Fatal(err)
	}
	if newC.SecretGen == oldC.SecretGen {
		t.Fatal("new incarnation reuses the old secret generation")
	}
}

// Two concurrent creates of one id: exactly one succeeds, and its secret works.
func TestKVM2M_ConcurrentCreatesOfOneID(t *testing.T) {
	kv := mustNewMemoryKV(t, systemCtx())
	s1 := auth.NewKVM2MClientStore(kv, 0, testSecretLimit)
	s2 := auth.NewKVM2MClientStore(kv, 0, testSecretLimit) // a second node: its own locks
	for round := 0; round < 10; round++ {
		id := fmt.Sprintf("race%d", round)
		var wg sync.WaitGroup
		secs, errs := make([]string, 2), make([]error, 2)
		for i, s := range []*auth.KVM2MClientStore{s1, s2} {
			wg.Add(1)
			go func() { defer wg.Done(); secs[i], errs[i] = s.Create(systemCtx(), "a", id, []string{"ROLE_M2M"}, false) }()
		}
		wg.Wait()
		won := 0
		for i := range 2 {
			switch {
			case errs[i] == nil:
				won++
				if _, err := s1.Authenticate(systemCtx(), "a", id, secs[i]); err != nil {
					t.Fatalf("round %d: winner's secret refused", round)
				}
			case !errors.Is(errs[i], auth.ErrM2MClientExists):
				t.Fatalf("round %d: %v", round, errs[i])
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d winners", round, won)
		}
	}
}

// A reset racing a delete never brings the client back.
func TestKVM2M_ResetRacingDeleteNeverResurrects(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	var s *auth.KVM2MClientStore
	// The delete lands between the reset's read and its write.
	kv := &beforeCASKV{KeyValueStore: mem, before: func() {
		if err := s.Delete(systemCtx(), "a", "backend"); err != nil {
			t.Error(err)
		}
	}}
	s = auth.NewKVM2MClientStore(kv, 0, testSecretLimit)
	if _, err := s.Create(systemCtx(), "a", "backend", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResetSecret(systemCtx(), "a", "backend"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("reset: %v", err)
	}
	if _, err := mem.Get(systemCtx(), "m2m-clients:a", "backend"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("deleted client came back")
	}
}

// A reset that loses to another reset answers ErrM2MClientChanged and leaves
// the winner's record.
func TestKVM2M_ResetLosingARaceIsChanged(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	var s *auth.KVM2MClientStore
	var winner string
	kv := &beforeCASKV{KeyValueStore: mem}
	s = auth.NewKVM2MClientStore(kv, 0, testSecretLimit)
	if _, err := s.Create(systemCtx(), "a", "backend", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatal(err)
	}
	kv.before = func() {
		kv.before = nil // the inner reset runs without interference
		sec, _, err := s.ResetSecret(systemCtx(), "a", "backend")
		if err != nil {
			t.Error(err)
		}
		winner = sec
	}
	if _, _, err := s.ResetSecret(systemCtx(), "a", "backend"); !errors.Is(err, auth.ErrM2MClientChanged) {
		t.Fatalf("losing reset: %v", err)
	}
	if _, err := s.Authenticate(systemCtx(), "a", "backend", winner); err != nil {
		t.Fatal("winner's secret refused")
	}
}

// beforeCASKV runs before (once set) ahead of every CompareAndPut.
type beforeCASKV struct {
	spi.KeyValueStore
	before func()
}

func (k *beforeCASKV) CompareAndPut(ctx context.Context, ns, key string, expected, value []byte) (bool, error) {
	if f := k.before; f != nil {
		f()
	}
	return k.KeyValueStore.CompareAndPut(ctx, ns, key, expected, value)
}

// casCommitThenFailKV applies every conditional write and then reports an
// error (a timeout after commit); then, if set, runs between the two.
type casCommitThenFailKV struct {
	spi.KeyValueStore
	then func()
}

func (k *casCommitThenFailKV) PutIfAbsent(ctx context.Context, ns, key string, v []byte) (bool, error) {
	if _, err := k.KeyValueStore.PutIfAbsent(ctx, ns, key, v); err != nil {
		return false, err
	}
	if k.then != nil {
		k.then()
	}
	return false, errors.New("injected: committed, then failed")
}

func (k *casCommitThenFailKV) CompareAndPut(ctx context.Context, ns, key string, expected, v []byte) (bool, error) {
	if _, err := k.KeyValueStore.CompareAndPut(ctx, ns, key, expected, v); err != nil {
		return false, err
	}
	if k.then != nil {
		k.then()
	}
	return false, errors.New("injected: committed, then failed")
}

// An ambiguous create write is undone, and only this call's write.
func TestKVM2M_CreateUndoesItsOwnAmbiguousWrite(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	s := auth.NewKVM2MClientStore(&casCommitThenFailKV{KeyValueStore: mem}, 0, testSecretLimit)
	if _, err := s.Create(systemCtx(), "a", "backend", []string{"ROLE_M2M"}, false); err == nil {
		t.Fatal("want error")
	}
	if _, err := mem.Get(systemCtx(), "m2m-clients:a", "backend"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("record left behind")
	}
}

// The undo of an ambiguous create never removes another create's winning
// record: here another node's record replaced this call's write before the
// error came back.
func TestKVM2M_CreateUndoSparesAnotherCallsRecord(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	other := auth.NewKVM2MClientStore(mem, 0, testSecretLimit)
	var otherSecret string
	s := auth.NewKVM2MClientStore(&casCommitThenFailKV{KeyValueStore: mem, then: func() {
		_ = mem.Delete(systemCtx(), "m2m-clients:a", "backend")
		var err error
		otherSecret, err = other.Create(systemCtx(), "a", "backend", []string{"ROLE_M2M"}, false)
		if err != nil {
			t.Error(err)
		}
	}}, 0, testSecretLimit)
	if _, err := s.Create(systemCtx(), "a", "backend", []string{"ROLE_M2M"}, false); err == nil {
		t.Fatal("want error")
	}
	if _, err := other.Authenticate(systemCtx(), "a", "backend", otherSecret); err != nil {
		t.Fatal("the other create's client was removed")
	}
}

// An ambiguous reset write is undone only if nothing changed since.
func TestKVM2M_ResetUndoesItsOwnAmbiguousWrite(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	plain := auth.NewKVM2MClientStore(mem, 0, testSecretLimit)
	old, err := plain.Create(systemCtx(), "a", "backend", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	s := auth.NewKVM2MClientStore(&casCommitThenFailKV{KeyValueStore: mem}, 0, testSecretLimit)
	if _, _, err := s.ResetSecret(systemCtx(), "a", "backend"); err == nil {
		t.Fatal("want error")
	}
	if _, err := plain.Authenticate(systemCtx(), "a", "backend", old); err != nil {
		t.Fatal("old secret not restored")
	}
}

// The undo of an ambiguous reset never revives a client deleted meanwhile.
func TestKVM2M_ResetUndoNeverRevivesADeletedClient(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	plain := auth.NewKVM2MClientStore(mem, 0, testSecretLimit)
	if _, err := plain.Create(systemCtx(), "a", "backend", []string{"ROLE_M2M"}, false); err != nil {
		t.Fatal(err)
	}
	s := auth.NewKVM2MClientStore(&casCommitThenFailKV{KeyValueStore: mem, then: func() {
		if err := plain.Delete(systemCtx(), "a", "backend"); err != nil {
			t.Error(err)
		}
	}}, 0, testSecretLimit)
	if _, _, err := s.ResetSecret(systemCtx(), "a", "backend"); err == nil {
		t.Fatal("want error")
	}
	if _, err := mem.Get(systemCtx(), "m2m-clients:a", "backend"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("deleted client came back")
	}
}
```

  - Rework `TestKVM2M_CreateUndoSurvivesCallerCancel`, `TestKVM2M_CreateRecordUndoSurvivesCallerCancel`, `TestKVM2M_ResetSecretRestoreSurvivesCallerCancel` and their `cancelThenFailKV` fake: the undo is now `DeleteIfEqual` / `CompareAndPut`, so the fake must cancel the caller's context and fail on `PutIfAbsent`/`CompareAndPut`, then let the undo (on its own context) through. Keep their assertion: the undo still runs after the caller cancels.
  - `TestKVM2M_AuthenticateRefusesMalformedIDsWithoutReading`: keep the "no read" assertion; it now also passes for a malformed tenant-scoped call.
  - Add to `internal/auth/token_test.go`:

```go
// A client id may be form-urlencoded in Basic credentials (RFC 6749 §2.3.1).
func TestToken_FormEncodedChosenIDAuthenticates(t *testing.T) {
	env := setupTokenEnv(t)
	sec, err := env.m2mStore.Create(systemCtx(), spi.TenantID(env.tenantID), "my-client", []string{"ROLE_M2M"}, false)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	env.handler.ServeHTTP(rr, makeTokenRequest(env.tenantID, "client_credentials", basicAuth("my%2Dclient", sec), nil))
	_, claims := issued(t, rr)
	if claims["sub"] != "my-client" {
		t.Fatalf("sub %v", claims["sub"])
	}
}
```

  - `internal/grpc/streaming_test.go`: change `lookupStore.Lookup` to `(ctx, tenantID spi.TenantID, clientID string)` and make it answer not-found unless `tenantID == s.client.TenantID`. In `TestRecheckClient`, the "another tenant" case now expects `Unauthenticated` from that not-found path.

Run: `go test ./internal/auth/ ./internal/grpc/ ./internal/domain/account/` → build FAIL.

- [ ] **Step 2: Rewrite the store** (`kv_m2m_store.go`). Keep `tenantLockStripes`, `tenantStripe`, `undoTimeout`, `createLock`, `undoContext`, `burnBcrypt`, `newSecret`, `List`. Remove `getIndex`, `undoCreate`, `undoReset`, every index reference, and from `kv_m2m_codec.go` `m2mClientIndexNamespace`, `m2mDecoyKey`, `m2mIndexEntry`, `encodeIndexEntry`, `decodeIndexEntry`. Replace the type comment and the methods:

```go
// KVM2MClientStore stores M2M clients in the SYSTEM-tenant KV store, one
// namespace per tenant, keyed by client id: a client is found by (tenant,
// client id). There is no node copy: every call reads or writes the store, so
// a change is in force on every node when the call returns.
//
// Every write is conditional on the state this call read or wrote
// (spi.KeyValueStore), so concurrent changes on any nodes resolve without a
// lost update: of two creates of one id exactly one succeeds; a reset that
// loses to another change is ErrM2MClientChanged; a delete always wins; and
// every undo of a failed write touches only this call's own bytes (a record
// carries a fresh bcrypt salt, so no two writes are equal). The cap is
// checked on each node before the write, so concurrent creates on several
// nodes can exceed it by one client per node.
//
// Authenticate keeps a per-node cache of verified secrets. Every bcrypt
// operation runs in one of a bounded number of slots.
type KVM2MClientStore struct { … unchanged fields … }

// getRecord reads and decodes (t, id), and returns the stored bytes with it.
// found=false: absent.
func (s *KVM2MClientStore) getRecord(ctx context.Context, t spi.TenantID, id string) (*M2MClient, []byte, bool, error) { … unchanged body … }

// Authenticate returns tenant's client whose id and secret match. An id
// outside the client-id grammar is ErrInvalidClient at once, with no read and
// no bcrypt: the grammar is public, so the refusal reveals nothing. Any other
// request reads (tenant, id) once and makes one bcrypt comparison — against
// the record's hash, or against a dummy hash when there is no record — in one
// of the node's slots, unless the node's verified-secret cache matches the
// record just read.
//
// ErrInvalidClient: no such client in tenant, or a wrong secret.
// ErrSecretCheckBusy: no slot freed up within the wait. Any other error is
// the store failing.
func (s *KVM2MClientStore) Authenticate(ctx context.Context, tenant spi.TenantID, clientID, secret string) (*M2MClient, error) {
	if !ValidClientID(clientID) {
		return nil, ErrInvalidClient
	}
	key := clientKey{tenant, clientID}
	c, _, found, err := s.getRecord(ctx, tenant, clientID)
	if err != nil {
		return nil, err
	}
	if !found {
		s.verified.drop(key)
		if err := s.burnBcrypt(ctx, secret); err != nil {
			return nil, err
		}
		return nil, ErrInvalidClient
	}
	sum := sha256.Sum256([]byte(secret))
	if s.verified.hit(key, c.HashedSecret, sum) {
		return c, nil
	}
	var mismatch error
	if err := s.slots.run(ctx, func() {
		mismatch = bcrypt.CompareHashAndPassword([]byte(c.HashedSecret), []byte(secret))
	}); err != nil {
		return nil, err
	}
	// A wrong secret leaves the cached entry alone: it can never hit (the
	// sums differ), and anyone holding the public client id could otherwise
	// evict the client's warm entry.
	if mismatch != nil {
		return nil, ErrInvalidClient
	}
	s.verified.put(key, c.HashedSecret, sum)
	return c, nil
}

// Lookup returns tenant's client clientID as the store holds it now, without
// checking a secret. ErrM2MClientNotFound: outside the grammar, or no record.
func (s *KVM2MClientStore) Lookup(ctx context.Context, tenant spi.TenantID, clientID string) (*M2MClient, error) {
	if !ValidClientID(clientID) {
		return nil, fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	c, _, found, err := s.getRecord(ctx, tenant, clientID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	return c, nil
}

// newGeneration returns a new client's first secret generation: random in
// [1, 2^52], so a client re-created under a deleted client's id never shares
// its generation, and a token of the deleted client never opens a stream.
func newGeneration() (uint64, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<52))
	if err != nil {
		return 0, fmt.Errorf("failed to draw a secret generation: %w", err)
	}
	return n.Uint64() + 1, nil
}

// Create adds tenant's client clientID and returns its plaintext secret, once.
// The id is taken (ErrM2MClientExists) when a record exists for it in tenant,
// decodable or not, or when another create of it, on any node, wrote first.
// A tenant at the cap is ErrM2MClientCapReached; a taken id is reported
// before the cap. The secret is hashed in a secret-check slot first; with
// none free the result is ErrSecretCheckBusy and nothing is written. A write
// whose outcome is unknown is undone by deleting exactly the bytes this call
// wrote, so another call's client is never removed.
func (s *KVM2MClientStore) Create(ctx context.Context, tenant spi.TenantID, clientID string, roles []string, onBehalfOf bool) (string, error) {
	secret, hash, err := s.newSecret(ctx)
	if err != nil {
		return "", err
	}
	gen, err := newGeneration()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	rec, err := encodeClientRecord(&M2MClient{ClientID: clientID, HashedSecret: string(hash), TenantID: tenant, UserID: clientID, Roles: append([]string(nil), roles...), OnBehalfOf: onBehalfOf, SecretGen: gen, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return "", err
	}
	ns := m2mTenantNamespace(tenant)
	// The stripe lock is held through the undo too: a hung store can block
	// the creates of every tenant on this stripe for up to undoTimeout.
	mu := s.createLock(tenant)
	mu.Lock()
	defer mu.Unlock()
	if _, err := s.kv.Get(ctx, ns, clientID); err == nil {
		return "", fmt.Errorf("%w: %s", ErrM2MClientExists, clientID)
	} else if !errors.Is(err, spi.ErrNotFound) {
		return "", fmt.Errorf("failed to read m2m client: %w", err)
	}
	if s.maxPerTenant > 0 {
		existing, err := s.kv.List(ctx, ns)
		if err != nil {
			return "", fmt.Errorf("failed to list m2m clients: %w", err)
		}
		if len(existing) >= s.maxPerTenant {
			return "", ErrM2MClientCapReached
		}
	}
	applied, err := s.kv.PutIfAbsent(ctx, ns, clientID, rec)
	if err != nil {
		s.undo(ctx, tenant, clientID, "create", func(uctx context.Context) (bool, error) {
			return s.kv.DeleteIfEqual(uctx, ns, clientID, rec)
		})
		return "", fmt.Errorf("failed to write m2m client: %w", err)
	}
	if !applied {
		return "", fmt.Errorf("%w: %s", ErrM2MClientExists, clientID)
	}
	return secret, nil
}

// undo runs a compensating conditional write on a context the caller cannot
// cancel, bounded by undoTimeout. A failure is logged at ERROR; a write that
// is not applied needs no log — this call's write did not land, or was
// already replaced.
func (s *KVM2MClientStore) undo(ctx context.Context, t spi.TenantID, id, op string, write func(context.Context) (bool, error)) {
	uctx, cancel := undoContext(ctx)
	defer cancel()
	if _, err := write(uctx); err != nil {
		slog.Error("m2m client "+op+" could not be undone", "pkg", "auth", "tenant", string(t), "kvKey", id, "error", err.Error())
	}
}

// Delete removes tenant's client clientID, decodable or not.
// ErrM2MClientNotFound when tenant has no such record. A delete always wins:
// a concurrent reset's conditional write then fails. The caller has checked
// clientID against the client-id grammar.
func (s *KVM2MClientStore) Delete(ctx context.Context, tenant spi.TenantID, clientID string) error {
	ns := m2mTenantNamespace(tenant)
	if _, err := s.kv.Get(ctx, ns, clientID); errors.Is(err, spi.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	} else if err != nil {
		return fmt.Errorf("failed to read m2m client: %w", err)
	}
	if err := s.kv.Delete(ctx, ns, clientID); err != nil {
		return fmt.Errorf("failed to delete m2m client: %w", err)
	}
	return nil
}

// ResetSecret gives tenant's client clientID a new secret and returns it,
// once, with the client. The secret is hashed in a secret-check slot before
// the store is read (none free: ErrSecretCheckBusy, nothing read or
// written). The write is conditional on the record read:
// ErrM2MClientNotFound if the client was deleted meanwhile, and
// ErrM2MClientChanged if another change replaced it. A write whose outcome
// is unknown is undone by restoring the read record, conditional on this
// call's own bytes, so it never revives a deleted client or overwrites a
// later change. The caller has checked clientID against the grammar.
func (s *KVM2MClientStore) ResetSecret(ctx context.Context, tenant spi.TenantID, clientID string) (string, *M2MClient, error) {
	secret, hash, err := s.newSecret(ctx)
	if err != nil {
		return "", nil, err
	}
	c, prev, found, err := s.getRecord(ctx, tenant, clientID)
	if err != nil {
		return "", nil, err
	}
	if !found {
		return "", nil, fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	c.HashedSecret, c.UpdatedAt = string(hash), time.Now().UTC()
	c.SecretGen++
	next, err := encodeClientRecord(c)
	if err != nil {
		return "", nil, err
	}
	ns := m2mTenantNamespace(tenant)
	applied, err := s.kv.CompareAndPut(ctx, ns, clientID, prev, next)
	if err != nil {
		s.undo(ctx, tenant, clientID, "secret reset", func(uctx context.Context) (bool, error) {
			return s.kv.CompareAndPut(uctx, ns, clientID, next, prev)
		})
		return "", nil, fmt.Errorf("failed to write m2m client: %w", err)
	}
	if !applied {
		if _, err := s.kv.Get(ctx, ns, clientID); errors.Is(err, spi.ErrNotFound) {
			return "", nil, fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
		} else if err != nil {
			return "", nil, fmt.Errorf("failed to read m2m client: %w", err)
		}
		return "", nil, fmt.Errorf("%w: %s", ErrM2MClientChanged, clientID)
	}
	return secret, c, nil
}
```

Imports: add `crypto/rand`, `math/big`; keep `noTx` calls exactly where they are today (Task 12 removes them) — put `ctx = noTx(ctx)` back as the first line of each method that had it.

In `kv_m2m_codec.go` `validateM2MClient`: replace `if c.SecretGen < 1` with

```go
	if c.SecretGen < 1 || c.SecretGen >= 1<<53 {
		return errors.New("secretGen outside [1, 2^53)")
	}
```

and update the namespace comment (one namespace per tenant, no index).

In `store.go`: add `ErrM2MClientChanged` with a doc ("returned by ResetSecret when another change to the client won the race; the adapter answers 409 CONFLICT, retryable"), update `ErrM2MClientExists`'s doc ("taken in the tenant"), and replace the interface with the one in **Interfaces**, with `Authenticate` and `Lookup` docs naming the tenant.

- [ ] **Step 3: Callers**
  - `token.go`: `client, err := h.m2mStore.Authenticate(r.Context(), tenant, clientID, secret)`; delete Task 7's interim `client.TenantID != tenant` block.
  - `streaming.go`: `recheckClient` calls `s.m2mStore.Lookup(readCtx, tenant, ct.ClientID)`; delete the `if c.TenantID != tenant` block (`:229-231`); add `"tenantId", string(tenant)` to the three log calls (`:80` uses `tenantID`, `:203` and `:225` use `tenant`); update the doc comment ("gone or its secret was reset").
  - `m2m_adapter.go:191`: `h.m2mClientStore.Create(r.Context(), tID, cid, roles, onBehalfOf)`.
  - `m2m_adapter.go` `writeM2MClientError`: before the internal-error fallthrough, add

    ```go
	if errors.Is(err, auth.ErrM2MClientChanged) {
		common.WriteError(w, r, common.Operational(http.StatusConflict,
			common.ErrCodeConflict, "the client changed during the request — retry").AsRetryable())
		return
	}
    ```
  - Fix every other compile error (`grep -rn 'Authenticate(\|Lookup(\|\.Create(' internal/ app/ --include='*.go'`), including test fakes that implement `M2MClientStore` (e.g. `failingM2MStore` in `token_test.go`) and the helpers `seedClient` and `authenticates` in `internal/domain/account/m2m_adapter_test.go` (`authenticates` gains a `tenant` parameter; update its callers).
  - Add to `m2m_adapter_test.go` a test that a store returning `ErrM2MClientChanged` from `ResetSecret` answers `409` with `errorCode` `CONFLICT` and `retryable: true` (follow the file's existing fake-store pattern).

- [ ] **Step 4: Run** `go build ./... && go vet ./... && go test ./internal/auth/ ./internal/grpc/ ./internal/domain/account/ ./app/` → PASS. Then `make test` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/store.go internal/auth/kv_m2m_store.go internal/auth/kv_m2m_codec.go internal/auth/kv_m2m_store_test.go internal/auth/token.go internal/auth/token_test.go internal/grpc/streaming.go internal/grpc/streaming_test.go internal/domain/account/m2m_adapter.go internal/domain/account/m2m_adapter_test.go
git commit -m "feat(auth)!: clients are found by tenant and id; every client write is conditional

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: response-time floor on store-decided `401 invalid_client`

**Files:**
- Modify: `internal/auth/token.go` (`NewTokenHandler` gains `failureFloor time.Duration`), `internal/auth/service.go:107`
- Test: `internal/auth/token_test.go`

**Interfaces:**
- Produces: `NewTokenHandler(keyStore KeyStore, trustedKeyStore TrustedKeyStore, m2mStore M2MClientStore, issuer, audience string, expirySeconds, requestsPerMinute int, failureFloor time.Duration) http.Handler`; `const InvalidClientFloor = 500 * time.Millisecond` in `internal/auth`.

- [ ] **Step 1: Failing tests** — every existing `NewTokenHandler(...)` call in tests gains a final `0`. New:

```go
func TestToken_InvalidClientFloor(t *testing.T) {
	const floor = 300 * time.Millisecond
	env := setupTokenEnv(t)
	h := routed(auth.NewTokenHandler(env.keyStore, env.trustedKeyStore, env.m2mStore, testIssuer, "", testExpiry, 0, floor))
	timed := func(authHeader string) (int, time.Duration) {
		start := time.Now()
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, makeTokenRequest(env.tenantID, "client_credentials", authHeader, nil))
		return rr.Code, time.Since(start)
	}
	for name, hdr := range map[string]string{
		"unknown client": basicAuth("nobody", "x"),
		"wrong secret":   basicAuth(env.clientID, "wrong"),
	} {
		if code, d := timed(hdr); code != http.StatusUnauthorized || d < floor {
			t.Errorf("%s: %d after %v, want 401 after >= %v", name, code, d, floor)
		}
	}
	for name, hdr := range map[string]string{
		"no credentials": "",
		"malformed id":   basicAuth("-bad", "x"),
	} {
		if code, d := timed(hdr); code != http.StatusUnauthorized || d >= floor {
			t.Errorf("%s: %d after %v, want an immediate 401", name, code, d)
		}
	}
	if code, d := timed(basicAuth(env.clientID, env.clientSecret)); code != http.StatusOK || d >= floor {
		t.Errorf("success: %d after %v, want an immediate 200", code, d)
	}
}

func TestToken_InvalidClientFloorEndsWhenTheClientLeaves(t *testing.T) {
	env := setupTokenEnv(t)
	h := routed(auth.NewTokenHandler(env.keyStore, env.trustedKeyStore, env.m2mStore, testIssuer, "", testExpiry, 0, time.Hour))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	h.ServeHTTP(httptest.NewRecorder(), makeTokenRequest(env.tenantID, "client_credentials", basicAuth("nobody", "x"), nil).WithContext(ctx))
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("held %v after the client left", d)
	}
}
```

Run → FAIL.

- [ ] **Step 2: Implement** in `token.go`: add field `failureFloor time.Duration`, the constructor parameter, and

```go
// InvalidClientFloor is the least time a store-decided 401 invalid_client
// takes, from the request reaching the handler. Without it, the store's read
// time — a present key can cost more than a missing one — would tell an
// anonymous caller whether a tenant has a given client.
const InvalidClientFloor = 500 * time.Millisecond

// holdUntil waits until deadline, or until the client leaves.
func holdUntil(ctx context.Context, deadline time.Time) {
	d := time.Until(deadline)
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
```

In `ServeHTTP`: `start := time.Now()` as the first statement; in the `ErrInvalidClient` branch:

```go
	if errors.Is(err, ErrInvalidClient) {
		// A well-formed id was decided by a store read: hold the answer so
		// its timing says nothing about what the store holds. A malformed id
		// is refused before any read and reveals nothing.
		if ValidClientID(clientID) {
			holdUntil(r.Context(), start.Add(h.failureFloor))
		}
		writeTokenError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}
```

`service.go`: pass `InvalidClientFloor`. Document the parameter in the constructor's comment.

- [ ] **Step 3: Run** `go test ./internal/auth/` → PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/auth/token.go internal/auth/token_test.go internal/auth/service.go
git commit -m "feat(auth): a store-decided invalid_client answer takes at least 500 ms

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 11: secret generation per incarnation — stream coverage

Task 9 added the random start. This task proves the stream effect at the unit
level.

**Files:**
- Test: `internal/grpc/streaming_test.go`

- [ ] **Step 1: Write the test** — in `TestRecheckClient`'s table add a case: stored client `{ClientID: "C1", TenantID: "tenant-1", SecretGen: 7}` (a recreated client) and token `ClientToken{ClientID: "C1", Gen: 1}` (the deleted client's first generation) → `codes.Unauthenticated`. Add a store test in `kv_m2m_store_test.go`: 20 creates of distinct ids, each record's `SecretGen` in `[1, 1<<52]` (read through `List`), and not all equal.

- [ ] **Step 2: Run** `go test ./internal/grpc/ ./internal/auth/` → PASS (the behaviour exists since Task 9; if a case fails, the Task 9 implementation is wrong — fix it there).

- [ ] **Step 3: Commit**

```bash
git add internal/grpc/streaming_test.go internal/auth/kv_m2m_store_test.go
git commit -m "test(grpc): a recreated client's stream check refuses the old generation

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 12: KV operations never join a transaction — remove `noTx`

**Files:**
- Modify: `internal/auth/kv_m2m_store.go`, `internal/auth/kv_trusted_store.go`, `internal/auth/kv_key_store_admin.go:58,218,230`, `internal/auth/kv_key_store.go` and every other `noTx` site (`grep -rn 'noTx' internal/auth --include='*.go'`)
- Delete: `internal/auth/tx_isolation_test.go`; `TestKVM2M_IgnoresCallerTransaction` and its `txProbeKV` fake in `kv_m2m_store_test.go`
- Test: `internal/e2e/clients_store_test.go:472` (`TestClientsStore_ClientChangeNotInCallersTransaction`) stays and must pass

- [ ] **Step 1: Delete the tests that assert `noTx`.** Their property — no KV write rides on a caller's transaction — is now the SPI contract, proven by `spitest` `KeyValue/NoTransactionJoin` on every backend (Tasks 1–3). Keep the e2e test: it proves the property through the real HTTP stack on postgres.

- [ ] **Step 2: Remove `noTx`** — delete the function (`kv_m2m_store.go`) and every `ctx = noTx(ctx)` line and `noTx(ctx)` argument; update the type comments that mention stripping the transaction to say the KV store never joins one (spi.KeyValueStore contract).

- [ ] **Step 3: Run** `go build ./... && go test ./internal/auth/` → PASS. Then `go test -timeout 30m -run 'TestClientsStore_ClientChangeNotInCallersTransaction|TestTrusted|TestOAuthKeys' ./internal/e2e/` → PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/auth/
git commit -m "refactor(auth): drop noTx — the KV store never joins a transaction

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

(`git add internal/auth/` stages only that directory; check `git status` first.)

---

### Task 13: `POST /clients?clientId=` and `409 M2M_CLIENT_EXISTS`

**Files:**
- Modify: `api/openapi.yaml` (`POST /clients` parameter and `409`; `PUT /clients/{clientId}/secret` `409`), `api/generated.go`
- Modify: `internal/common/error_codes.go` (`ErrCodeM2MClientExists = "M2M_CLIENT_EXISTS"`)
- Create: `cmd/cyoda/help/content/errors/M2M_CLIENT_EXISTS.md`; Modify: `cmd/cyoda/help/content/errors.md` (index line)
- Modify: `internal/domain/account/m2m_adapter.go` (`CreateTechnicalUser`)
- Test: `internal/domain/account/m2m_adapter_test.go`

**Interfaces:**
- Consumes: `Create(ctx, tenant, id, roles, obo)`, `ErrM2MClientExists` (Task 9); `ValidClientID` (Task 5).
- Produces: generated `CreateTechnicalUserParams.ClientId *string`.

- [ ] **Step 1: OpenAPI** — add to `POST /clients` parameters:

```yaml
        - name: clientId
          in: query
          description: "The new client's id, chosen by the caller and unique in
            the caller's tenant. Letters, digits, \".\", \"_\" and \"-\", starting
            with a letter or digit, at most 100 characters, case significant;
            \"system\" in any letter case is reserved. Absent: an id is
            generated (16 characters from 0-9 and A-V). An id the tenant
            already holds answers 409 M2M_CLIENT_EXISTS.
            "
          required: false
          schema:
            type: string
            maxLength: 100
            pattern: "^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$"
```

and a response (same shape as the `400`'s, `application/problem+json`):

```yaml
        "409":
          description: "M2M_CLIENT_EXISTS: the caller's tenant already holds a
            client with this clientId, or another create of it wrote first."
```

Extend the `400` description with "`clientId` outside the client-id grammar or empty". On `PUT /clients/{clientId}/secret` add `"409"`: "CONFLICT (retryable): another change to the client won the race; retry." Run `go generate ./api`.

- [ ] **Step 2: Failing adapter tests** (`internal/domain/account/m2m_adapter_test.go`; the helpers `newM2MAdapterFixture`, `withTenantAdminCtx`, `withTenantNonAdminCtx`, `decodeErrCode`, `listStored`, `newM2MStore` are in the file; add `"net/url"` to its imports):

```go
// createAs posts POST /clients as tenantA's admin with params and returns
// the recorder.
func createAs(h *Handler, params genapi.CreateTechnicalUserParams) *httptest.ResponseRecorder {
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/clients", nil), tenantA)
	rr := httptest.NewRecorder()
	h.CreateTechnicalUser(rr, req, params)
	return rr
}

func strptr(s string) *string { return &s }

func TestCreateTechnicalUser_ChosenID(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	rr := createAs(h, genapi.CreateTechnicalUserParams{ClientId: strptr("order-service")})
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var creds genapi.TechnicalUserCredentialsDto
	if err := json.Unmarshal(rr.Body.Bytes(), &creds); err != nil || creds.ClientId != "order-service" {
		t.Fatalf("client_id %q (%v)", creds.ClientId, err)
	}
}

func TestCreateTechnicalUser_ChosenIDTaken409(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	createAs(h, genapi.CreateTechnicalUserParams{ClientId: strptr("order-service")})
	rr := createAs(h, genapi.CreateTechnicalUserParams{ClientId: strptr("order-service")})
	if rr.Code != http.StatusConflict || decodeErrCode(t, rr.Body.Bytes()) != common.ErrCodeM2MClientExists {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateTechnicalUser_ChosenIDGrammar400(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	for _, id := range []string{"", "-x", "a:b", "a b", "system", "SYSTEM"} {
		rr := createAs(h, genapi.CreateTechnicalUserParams{ClientId: strptr(id)})
		if rr.Code != http.StatusBadRequest || decodeErrCode(t, rr.Body.Bytes()) != common.ErrCodeBadRequest {
			t.Errorf("%q: status %d: %s", id, rr.Code, rr.Body.String())
		}
	}
	if got := listStored(t, h, tenantA); len(got) != 0 {
		t.Fatalf("refused creates stored %d clients", len(got))
	}
}

func TestCreateTechnicalUser_ClientIDLengthBound(t *testing.T) {
	h := newM2MAdapterFixture(t, false)
	if rr := createAs(h, genapi.CreateTechnicalUserParams{ClientId: strptr(strings.Repeat("a", 100))}); rr.Code != http.StatusOK {
		t.Errorf("100 chars: %d", rr.Code)
	}
	if rr := createAs(h, genapi.CreateTechnicalUserParams{ClientId: strptr(strings.Repeat("a", 101))}); rr.Code != http.StatusBadRequest {
		t.Errorf("101 chars: %d", rr.Code)
	}
}

func TestCreateTechnicalUser_TakenIDBeforeCap(t *testing.T) {
	feats := auth.DefaultIAMFeatures()
	feats.M2MClientMaxPerTenant = 1
	h := New(nil, nil, newM2MStore(t, 1), feats, auth.OperatorGuard{})
	createAs(h, genapi.CreateTechnicalUserParams{ClientId: strptr("only")})
	rr := createAs(h, genapi.CreateTechnicalUserParams{ClientId: strptr("only")})
	if rr.Code != http.StatusConflict {
		t.Fatalf("taken id at the cap: %d %s", rr.Code, rr.Body.String())
	}
}

func TestCreateTechnicalUser_CheckOrder(t *testing.T) {
	bad := genapi.CreateTechnicalUserParams{ClientId: strptr("-x"), WithAdminRole: ptr(true)}
	// 403 before the id is looked at.
	h := newM2MAdapterFixture(t, false)
	req := withTenantNonAdminCtx(httptest.NewRequest(http.MethodPost, "/clients", nil), tenantA)
	rr := httptest.NewRecorder()
	h.CreateTechnicalUser(rr, req, bad)
	if rr.Code != http.StatusForbidden {
		t.Errorf("non-admin: %d", rr.Code)
	}
	// 501 (mock mode) before the id is looked at.
	mock := New(nil, nil, nil, auth.DefaultIAMFeatures(), auth.OperatorGuard{})
	if rr := createAs(mock, bad); rr.Code != http.StatusNotImplemented {
		t.Errorf("mock: %d", rr.Code)
	}
	// The id grammar before FEATURE_DISABLED (flag off, withAdminRole=true).
	if rr := createAs(h, bad); rr.Code != http.StatusBadRequest {
		t.Errorf("grammar vs feature flag: %d", rr.Code)
	}
}
```

Also update the file's helpers to the Task 9 signatures if Task 9 has not:
`seedClient` calls `Create(m2mSysCtx(), spi.TenantID(tenant), clientID, []string{"ROLE_M2M"}, false)`,
and `authenticates(h, tenant, clientID, secret)` calls
`Authenticate(m2mSysCtx(), spi.TenantID(tenant), clientID, secret)`.

Run: `go test ./internal/domain/account/ -run TestCreateTechnicalUser` → FAIL.

- [ ] **Step 3: Implement** in `CreateTechnicalUser`, right after `requireM2MStore`:

```go
	// A chosen id is checked before anything else about the request: an
	// empty or malformed one is the caller's mistake whatever the flags say.
	chosen := params.ClientId
	if chosen != nil && !auth.ValidClientID(*chosen) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest,
			common.ErrCodeBadRequest, "invalid clientId"))
		return
	}
```

Replace the generate-and-retry loop with:

```go
	var clientID, secret string
	for attempt := 0; ; attempt++ {
		cid := ""
		if chosen != nil {
			cid = *chosen
		} else {
			generated, err := generateClientID()
			if err != nil {
				common.WriteError(w, r, common.Internal("generateClientID", err))
				return
			}
			cid = generated
		}
		sec, err := h.m2mClientStore.Create(r.Context(), tID, cid, roles, onBehalfOf)
		if err == nil {
			clientID, secret = cid, sec
			break
		}
		switch {
		case errors.Is(err, auth.ErrM2MClientExists) && chosen != nil:
			common.WriteError(w, r, common.Operational(http.StatusConflict,
				common.ErrCodeM2MClientExists, "the tenant already holds a client with this clientId"))
			return
		case errors.Is(err, auth.ErrM2MClientExists) && attempt == 0:
			continue // a generated id collided (about 1 in 2^80): draw again, once
		case errors.Is(err, auth.ErrM2MClientExists):
			common.WriteError(w, r, common.Internal("generateClientID-collision", errors.New("clientId collision after retry")))
			return
		case errors.Is(err, auth.ErrM2MClientCapReached):
			common.WriteError(w, r, common.Operational(http.StatusBadRequest,
				common.ErrCodeM2MClientCapReached, "M2M client cap reached for tenant"))
			return
		}
		if writeSecretCheckBusy(w, r, err) {
			return
		}
		common.WriteError(w, r, common.Internal("m2mClientStore.Create", err))
		return
	}
```

Update the function comment (`POST /clients?clientId=&withAdminRole=&onBehalfOf=`; chosen or generated id).

- [ ] **Step 4: Error code and help topic** — `ErrCodeM2MClientExists = "M2M_CLIENT_EXISTS"` beside `ErrCodeM2MClientCapReached`. Create `cmd/cyoda/help/content/errors/M2M_CLIENT_EXISTS.md`:

```markdown
---
topic: errors.M2M_CLIENT_EXISTS
title: "M2M_CLIENT_EXISTS — the tenant already holds this client id"
stability: stable
see_also:
  - errors
  - auth.clients
---

# errors.M2M_CLIENT_EXISTS

## NAME

M2M_CLIENT_EXISTS — the tenant already holds a client with the requested id.

## SYNOPSIS

HTTP: `409` `Conflict`. Retryable: `no`.

## DESCRIPTION

`POST /clients?clientId=<id>` creates a client with a chosen id. Client ids are unique within a tenant, so a create of an id the tenant already holds is refused and creates nothing. Of two creates of one id at the same moment, on any nodes, exactly one succeeds; the other gets this error. A provisioning script can treat it as "already there": the existing client's secret is not returned again, so keep the secret from the create that made it, or reset it (`PUT /clients/{clientId}/secret`).

## SEE ALSO

- errors
- auth.clients
```

Add to `cmd/cyoda/help/content/errors.md` next to the `M2M_CLIENT_CAP_REACHED` line:
`- \`errors.M2M_CLIENT_EXISTS\` — \`409\` — not retryable — \`POST /clients?clientId=\`: the tenant already holds a client with this id`.
Add `errors.M2M_CLIENT_EXISTS` to the `see_also` of `auth/clients.md`.

- [ ] **Step 5: Run** `go test ./internal/domain/account/ ./cmd/cyoda/help/ ./internal/common/` → PASS (`TestErrCode_Parity` included).

- [ ] **Step 6: Commit**

```bash
git add api/openapi.yaml api/generated.go internal/common/error_codes.go internal/domain/account/m2m_adapter.go internal/domain/account/m2m_adapter_test.go cmd/cyoda/help/content/errors/M2M_CLIENT_EXISTS.md cmd/cyoda/help/content/errors.md cmd/cyoda/help/content/auth/clients.md
git commit -m "feat(clients): a tenant may choose a client id; a taken one is 409 M2M_CLIENT_EXISTS

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 14: e2e — the token endpoint and the route group

**Files:**
- Create: `internal/e2e/tenant_token_test.go`

Rows of spec §7: token at the new URL (both grants are already covered by the
existing e2e suite through `postTokenRaw`); unknown tenant / unknown client /
wrong secret → same `401`, held ≥ 500 ms; group `400` rows; route words as
tenants; old path gone; `%2F` case.

- [ ] **Step 1: Write the tests**

```go
// tokenStatus posts client_credentials to tenant's token URL on baseURL and
// returns the status, the OAuth error and the elapsed time.
func tokenStatus(t *testing.T, baseURL, path, user, pass string) (int, string, time.Duration) {
	t.Helper()
	req, _ := http.NewRequestWithContext(e2eCtx(t), http.MethodPost, baseURL+path, strings.NewReader("grant_type=client_credentials"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Error string `json:"error"` }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.Error, time.Since(start)
}

func TestTenantToken_UnknownTenantClientAndWrongSecretAreTheSame401(t *testing.T) {
	id, secret := createClient(t, false, false) // suite tenant
	for name, c := range map[string]struct{ tenant, user, pass string }{
		"unknown tenant": {"no-such-tenant", id, secret},
		"unknown client": {suiteTenant, "no-such-client", secret},
		"wrong secret":   {suiteTenant, id, "wrong"},
	} {
		code, oauthErr, d := tokenStatus(t, serverURL, "/api/tenants/"+c.tenant+"/oauth/token", c.user, c.pass)
		if code != http.StatusUnauthorized || oauthErr != "invalid_client" || d < 500*time.Millisecond {
			t.Errorf("%s: %d %q after %v", name, code, oauthErr, d)
		}
	}
}

func TestTenantToken_GroupRefusals(t *testing.T) {
	id, secret := createClient(t, false, false)
	for _, p := range []string{
		"/api/tenants/SYSTEM/oauth/token",
		"/api/tenants/system/oauth/token",
		"/api/tenants/-x/oauth/token",
		"/api/tenants/%74est-tenant/oauth/token",
		"/api/%74enants/test-tenant/oauth/token",
		"/api/tenants/test-tenant/oauth/%74oken",
	} {
		if code, oauthErr, _ := tokenStatus(t, serverURL, p, id, secret); code != http.StatusBadRequest || oauthErr != "invalid_request" {
			t.Errorf("%s: %d %q", p, code, oauthErr)
		}
	}
}
```

Plus, on a server without the OpenAPI validator (build it as `cors_e2e_test.go:23` does — `httptest.NewServer(a.Handler())` over the suite's app config; read that file and reuse its setup function):
- `TestTenantToken_EncodedSlashInTenant`: `/api/tenants/a%2Fb/oauth/token` → `400 invalid_request`.
- `TestTenantToken_OldPathIsGone`: `POST /api/oauth/token` with valid Basic credentials → not `200` (expect `401` from the authenticated catch-all) and no `access_token` in the body.

And on the shared server:
- `TestTenantToken_RouteWordsAsTenants`: for tenants `clients`, `oauth`, `model`, `tenants`: sign an admin token for that tenant (`signServiceToken(e2eSignKey, e2eIssuer, "", "admin", <tenant>, "admin", []string{"ROLE_ADMIN","ROLE_M2M"})`), `POST /api/clients` with it, get a token at `/api/tenants/<tenant>/oauth/token`, and `GET /api/clients` with that token → `200` listing the client. Delete the client at cleanup (`deleteClientAtCleanup`).

- [ ] **Step 2: Run** `go test -timeout 30m -run 'TestTenantToken_' ./internal/e2e/` → PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/tenant_token_test.go
git commit -m "test(e2e): tenant token URL, route group refusals, the old path

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 15: e2e — chosen ids, cross-tenant isolation, concurrency

**Files:**
- Create: `internal/e2e/clients_tenant_scope_test.go`
- Modify: `internal/e2e/clients_store_test.go` — delete or rewrite `TestClientsStore_RawRecords` (`:312`, it writes and asserts `m2m-client-ids` rows) and `TestClientsStore_TokenEndpointRefusesMalformedIDs` (`:295`, update the URL and the malformed examples to the new grammar). Read them first; keep every assertion that is still about records in `m2m-clients:<tenant>`.

- [ ] **Step 1: Write the tests** (each uses `t.Parallel()` where the file's neighbours do; secrets never go into failure messages — use `withheld`):
  - `TestClients_ChosenID`: `POST /api/clients?clientId=order-service` → `200`, `client_id == "order-service"`; token at `/api/tenants/test-tenant/oauth/token` with it → `200`; second create → `409 M2M_CLIENT_EXISTS` (`assertProblemJSON`).
  - `TestClients_ChosenIDGrammar`: `clientId=` (empty), `-x`, `a:b`, `system`, 101 chars → `400 BAD_REQUEST`.
  - `TestClientsStore_TakenIDBeforeCap` (on `capStack(t, 1)`, `clients_store_test.go:172`): create `only`, create `only` again → `409 M2M_CLIENT_EXISTS`, not `400 M2M_CLIENT_CAP_REACHED`.
  - `TestClients_SameIDInTwoTenants`: tenants `ta-<uuid>` and `tb-<uuid>` (admin tokens via `signServiceToken`); create `backend` in both; each token's `caas_org_id` is its own tenant (decode with `auth.Parse`); A's secret at B's URL → `401`; reset in A → B's old secret still works; delete in A → B still gets a token; A's client created again → `200`.
  - `TestClients_RateLimitIsPerTenant`: a stack (`newCalloutHarness(t, func(cfg *app.Config){ cfg.IAM.TokenRequestsPerMinute = 1 })` — check the field name in `app/config.go:298`) with `backend` in two tenants: A's second request in a minute → `429`; B's first → `200`.
  - `TestClientsStore_ConcurrentCreatesOfOneIDOnTwoNodes` (isolated, `twoNodes(t)`): 10 rounds; per round both nodes `POST /api/clients?clientId=race<n>` at once with a PLATFORM admin token (`a.platformToken(t)`): exactly one `200` and one `409`; the `200`'s secret gets a token at `/api/tenants/PLATFORM/oauth/token` from either node.
  - `TestClientsStore_ConcurrentResetsAreConsistent` (isolated, `twoNodes`): create one client; 8 concurrent resets across both nodes; every status is `200` or `409` with `errorCode CONFLICT`; of the returned secrets exactly one gets a token.

- [ ] **Step 2: Run** `go test -timeout 30m -run 'TestClients_|TestClientsStore_' ./internal/e2e/` → PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/clients_tenant_scope_test.go internal/e2e/clients_store_test.go
git commit -m "test(e2e): chosen client ids, per-tenant isolation, concurrent creates and resets

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 16: e2e — a recreated id does not inherit streams

**Files:**
- Modify: `internal/e2e/stream_guard_test.go`

- [ ] **Step 1: Write the test**

```go
// A client deleted and created again under the same id is a new client: a
// token of the deleted one cannot open a stream, and the deleted one's open
// stream closes.
func TestStream_RecreatedClientID_OldTokenRefused(t *testing.T) {
	h := newCalloutHarness(t, nil)
	id := "recreated-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	code, raw := h.postClientWithID(t, h.token(t), id)
	if code != http.StatusOK {
		t.Fatalf("create: %d %s", code, withheld(code, raw))
	}
	cred := decodeCredential(t, "create", raw)
	oldToken := h.fetchTokenFor(t, cred.id, cred.secret)
	m := h.joinWithToken(t, oldToken) // open a stream with the old token
	if code, body := h.deleteClient(t, h.token(t), id); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, raw := h.postClientWithID(t, h.token(t), id); code != http.StatusOK {
		t.Fatalf("re-create: %d %s", code, withheld(code, raw))
	}
	assertStreamEnds(t, m, codes.Unauthenticated)
	if got := openStreamCode(t, h, oldToken); got != codes.Unauthenticated {
		t.Fatalf("old token opened a stream on the new client: %v", got)
	}
}
```

Add `postClientWithID` beside `postClient` in `clients_store_test.go` (`POST /api/clients?clientId=<id>`). For `joinWithToken`, read `joinOwnClient` (`stream_guard_test.go:191`) and factor out its join step so it takes a bearer; reuse it in both.

- [ ] **Step 2: Run** `go test -timeout 30m -run 'TestStream_' ./internal/e2e/` → PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/stream_guard_test.go internal/e2e/clients_store_test.go
git commit -m "test(e2e): a re-created client id does not inherit the deleted client's streams

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 17: parity scenarios

**Files:**
- Modify: `e2e/parity/client/keys.go` (add `CreateClientWithIDRaw`)
- Create: `e2e/parity/m2m_tenant_scope.go`
- Modify: `e2e/parity/registry.go` (register), `e2e/parity/registry_count_test.go:9` (count + 2) and the header comment of `registry.go`

- [ ] **Step 1: Client helper** — `func (c *Client) CreateClientWithIDRaw(t *testing.T, clientID string, withAdmin, onBehalfOf bool) (int, []byte, error)` issuing `POST /api/clients?clientId=<url.QueryEscape(id)>&withAdminRole=…&onBehalfOf=…` (mirror `CreateClientRaw`).

- [ ] **Step 2: Scenarios** (`m2m_tenant_scope.go`):
  - `RunM2MClientSameIDTwoTenants(t, fixture)`: tenants `a`, `b` from `fixture.NewTenant`; create `backend` in each via `CreateClientWithIDRaw`; each secret gets a token only at its own tenant's URL (`FetchClientCredentialsToken(ctx, base, a.ID, …)`); delete in A, B's token still `200`; reset in B, A's re-created `backend` unaffected.
  - `RunM2MClientChosenIDExists(t, fixture)`: create `order-service`; second create → `409` with `M2M_CLIENT_EXISTS` (`containsErrorCode`); delete; create again → `200`.
  Register both after `M2MClientOnBehalfOf` and bump `wantParityScenarioCount` by 2.

- [ ] **Step 3: Run** `make test` → PASS (parity runs on memory, sqlite and postgres).

- [ ] **Step 4: Commit**

```bash
git add e2e/parity/client/keys.go e2e/parity/m2m_tenant_scope.go e2e/parity/registry.go e2e/parity/registry_count_test.go
git commit -m "test(parity): same client id in two tenants; chosen id taken

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 18: help topics

**Files (all under `cmd/cyoda/help/content/`):** `auth.md`, `auth/clients.md`, `auth/tokens.md`, `auth/integration.md`, `auth/trusted-keys.md`, `cli/token.md`, `config/auth.md`, `config/grpc.md`, `grpc.md`, `helm.md`, `errors/NOT_IMPLEMENTED.md`, `errors/SERVER_BUSY.md`, `errors/CONFLICT.md`, `errors/M2M_CLIENT_CAP_REACHED.md`

Write compact prose: the actionable core plus a pointer, no history.

- [ ] **Step 1: Make the URL change everywhere.** `grep -rn 'oauth/token' cmd/cyoda/help/content` lists every site; each becomes `/api/tenants/{tenant}/oauth/token` (prose) or `https://cyoda.example.com/api/tenants/${TENANT_ID}/oauth/token` (curl). The `auth.tokens` title becomes `"auth.tokens — the token endpoint, its grants and the JWT claim contract"`; update the assertion at `cmd/cyoda/help/help_test.go:791` with it.
- [ ] **Step 2: `auth.clients`.**
  - Provision: the `clientId` query parameter, the grammar, `409 M2M_CLIENT_EXISTS`, and that a re-runnable setup script uses a chosen id (replace the "A client has no name or label" paragraph).
  - STORAGE AND CONSISTENCY: replace the index paragraphs with the new model: one record per (tenant, id); every write conditional; a delete always wins; concurrent resets: one wins, the other `409 CONFLICT` (retryable); concurrent creates of one id: one wins, the other `409 M2M_CLIENT_EXISTS`; the cap can be exceeded by one per node; failed-undo and late-landing write cases (spec §4.3); a damaged record (listed as missing, `DELETE` removes it).
  - "Client ids are unique across all tenants" → "unique within a tenant".
  - ERRORS: add `M2M_CLIENT_EXISTS` (`409`) and `CONFLICT` (`409`, reset race); widen the `BAD_REQUEST` grammar text; remove the index-damage text from `M2M_CLIENT_NOT_FOUND` and `SERVER_ERROR`.
- [ ] **Step 3: `auth.tokens`.** The client is found by (tenant, client id); a wrong tenant is the same `401`; the `400 invalid_request "invalid tenant"` row first in the error order; the 500 ms hold on a store-decided `401`; the generated id description (chosen or generated); the ingress rate-limit paragraph names `/api/tenants/{tenant}/oauth/token` and says tenant ids appear in URLs and gateway logs.
- [ ] **Step 4: `config.auth`** — tenant ids are public identifiers (no personal data or secrets); `SYSTEM` (any case) is refused as a token tenant and as `CYODA_IAM_MOCK_TENANT_ID`; the client-id grammar. **`config.grpc`** — `CYODA_COMPUTE_TENANT_ID` beside the other `CYODA_COMPUTE_*` variables and in the example block. **`helm`** — the rate-limit path pattern `^/api/tenants/[^/]+/oauth/token$`. **`errors.CONFLICT`** — the reset race. **`errors.M2M_CLIENT_CAP_REACHED`** — drop the damaged-index wording if any.
- [ ] **Step 5: Run** `go test ./cmd/cyoda/help/...` → PASS. Check: `grep -rn 'oauth/token' cmd/cyoda/help/content | grep -v 'tenants/'` → empty; `grep -rn 'm2m-client-ids\|unique across all tenants\|index entry' cmd/cyoda/help/content` → empty.
- [ ] **Step 6: Commit**

```bash
git add cmd/cyoda/help/content cmd/cyoda/help/help_test.go
git commit -m "docs(help): tenant-scoped clients and the tenant token URL

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 19: project docs, CHANGELOG, OpenAPI diff allowances

**Files:** `README.md:117-118`, `CHANGELOG.md` (Unreleased), `docs/access-to-the-cyoda-api.html` (`:224-489`, including the diagram labels), `docs/ARCHITECTURE.md:1862-1884,1990,2201-2209` (present tense; remove the global-index text), `docs/FEATURES.md:117`, `docs/PRD.md:564,593-594,741`, `docs/CONCURRENCY.md:111-113`, `deploy/helm/cyoda/docs/gateway-api-policies.md:59`, `docs/adr/0004-m2m-only-access-with-on-behalf-of-identity.md:40` (the URL only), `.github/oasdiff-err-ignore.txt`

- [ ] **Step 1:** Update each file's token URL and any statement about global client ids. ARCHITECTURE: audit the whole auth section on touch (reference, not history).
- [ ] **Step 2: CHANGELOG** under the unreleased version, `### Breaking`:
  - The token endpoint is `POST /api/tenants/{tenant}/oauth/token`; `/api/oauth/token` is gone. Applications put their client's tenant in the URL.
  - Client ids are unique within a tenant; `POST /clients?clientId=` chooses one; a taken id is `409 M2M_CLIENT_EXISTS`. The client-id grammar is the tenant grammar; `system` is reserved.
  - A secret reset that loses a race answers `409 CONFLICT` (retryable).
  - `SYSTEM` (any case) is refused as a token tenant, by `cyoda token` and as `CYODA_IAM_MOCK_TENANT_ID`.
  - A store-decided `401 invalid_client` takes at least 500 ms.
  - `CYODA_COMPUTE_TENANT_ID` (compute-test-client).
  - SPI: `KeyValueStore` gains `PutIfAbsent`, `CompareAndPut`, `DeleteIfEqual`; no KV operation joins a transaction (out-of-tree plugins must implement them).
- [ ] **Step 3: oasdiff** — run the repo's oasdiff check (find it: `grep -rn oasdiff Makefile .github/workflows`) against `origin/release/v0.9.0`; add each reported breaking change to `.github/oasdiff-err-ignore.txt` in the file's existing format, with a comment naming the change. Re-run until clean.
- [ ] **Step 4: Check** `grep -rn 'oauth/token' README.md CHANGELOG.md docs deploy --include='*.md' --include='*.html' | grep -v 'docs/superpowers\|docs/cyoda/\|tenants/'` → only CHANGELOG entries of earlier releases.
- [ ] **Step 5: Commit** (files by name)

```bash
git commit -m "docs: the tenant token URL and tenant-scoped clients

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 20: Cloud parity doc; SPI pin

**Files:**
- Create: `docs/cloud-parity/tenant-scoped-clients.md`
- Modify: `docs/cloud-parity/m2m-clients.md`, `platform-operator.md:30`, `signing-key-pairs.md:75`, `trusted-key-tenant.md:12`, `user-id-rule.md:49`, `docs/cloud-parity/README.md` (index)
- Modify: `go.mod`, `plugins/memory/go.mod`, `plugins/sqlite/go.mod`, `plugins/postgres/go.mod`, `COMPATIBILITY.md`

- [ ] **Step 1: Parity doc** in the style of `m2m-clients.md`: Rule (URL, lookup by tenant and id, chosen ids and `409`, the client-id grammar, `SYSTEM` refused, reset `409 CONFLICT`, the 500 ms floor, the group's `400`); Cloud today (global user-name lookup in the users table, `TechnicalUserService.kt:249`; ids from `Random.Default`, `AccountUtils.kt:14`; wrong secret `400` vs unknown `401`, an existence oracle); Cloud action (each item, ticket placeholder `CP-XXXX` filled in Task 21). Update the other parity docs' URL mentions and the README index.
- [ ] **Step 2: SPI pin** — only after the SPI PR from Task 1 is merged into `cyoda-go-spi` `main` (Paul approves; ask him once the PR is green). Then, in the cyoda-go worktree: bump the SPI require line to the pseudo-version of the merge commit in all four `go.mod` files in **one** commit (`go get github.com/cyoda-platform/cyoda-go-spi@<sha>` in each module; never `go mod tidy` against an untagged require while `go.work` is active — follow `MAINTAINING.md`), run `make check-spi-pin-sync`, update `COMPATIBILITY.md`'s SPI line, commit, push, then `make repin-plugins`, commit that as a new commit, push. Verify `GOWORK=off go build ./...` succeeds in the root and each plugin.
- [ ] **Step 3: Commit** the parity docs separately from the pin commits.

---

### Task 21: cassandra issue, CaaS ticket, tracker

- [ ] **Step 1: cassandra issue** in `Cyoda/cyoda-go-cassandra`: title "Implement the conditional key-value writes (PutIfAbsent, CompareAndPut, DeleteIfEqual) and the no-transaction-join contract". Body: the SPI PR link; the contract (atomic across nodes; `applied=false` only when the write is known not to have landed — an LWT timeout is an error; a deleted key is absent; nil equals empty); why the existing tables are not a drop-in (`USING TIMESTAMP <HLC>` on meta, listing and data rows, `internal/cql/cql.go:149,162,179`; uncoordinated `current_version + 1`, `internal/store/data_store.go:68-80`); the `spitest` cases it must pass; that its v0.9.0 bump (cassandra#108) waits for it. Title only, no private detail beyond the code facts (cassandra is the commercial backend).
- [ ] **Step 2: CaaS ticket** in Jira project CP (`[CaaS]` title prefix, component CaaS; see the reference memory for the connector): the parity doc's Cloud action list. Put the ticket key into `docs/cloud-parity/tenant-scoped-clients.md` and commit.
- [ ] **Step 3: Tracker #651**: row 1 status → "🟡 implementation done on `feat/650-tenant-scoped-clients`; gates next", plus the SPI PR, cassandra issue and CaaS ticket. Edit the body safely (save to a file, check it is non-empty and the anchor exists, write back).

---

## After the last task (workflow, not tasks)

1. `make test-full` (log to a file, read the counts), `go vet ./...`, `make race` once.
2. Fresh-context whole-branch code review (`superpowers:requesting-code-review`), fix, re-verify.
3. `cyoda-go-security-audit` on the exact head that will be merged.
4. PR into `release/v0.9.0`; the body carries the coverage map, the waivers (forced reset `409` is unit-only), Gate 7 (CaaS ticket), the SPI PR and the cassandra issue.
