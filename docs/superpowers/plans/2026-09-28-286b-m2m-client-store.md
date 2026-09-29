# #286 Part B — M2M clients shared by the cluster — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Store M2M clients in the SYSTEM-tenant KV store (a namespace per tenant plus a global id index), read directly on every operation, with a per-tenant cap; and make deleting an absent key return `nil` on every storage backend.

**Architecture:** `KVM2MClientStore` replaces `InMemoryM2MClientStore`. Every call strips any transaction from its context and reads or writes the KV store; there is no node copy, so a 2xx write is visible to the next read on any node. The token endpoint calls `Authenticate` once (two point reads, one bcrypt on every decided path). The SPI gains a documented, conformance-tested rule for deleting an absent key; the cassandra plugin is brought in line.

**Tech Stack:** Go 1.26, `golang.org/x/crypto/bcrypt`, SPI `KeyValueStore`, testcontainers-go e2e, the parity suites (memory, sqlite, postgres, cassandra).

**Spec:** `docs/superpowers/specs/2026-09-28-286-m2m-clients-cluster-store-design.md` — Part B is §5 and §6.2. Read §5 before any task.

**Branch:** stacks on Part A (`docs/superpowers/plans/2026-09-28-286a-cyoda-token.md`). Start after Part A's PR is open; branch `feat/286b-m2m-client-store` from Part A's head, PR into `release/v0.9.0` once Part A merges (retarget before deleting Part A's branch, never after).

## Global Constraints

- TDD for every production change (`.claude/rules/tdd.md`).
- Never log a secret, a bcrypt hash or a token. Client ids and KV keys may be logged.
- Namespaces: `m2m-clients:<tenantId>` (records), `m2m-client-ids` (index). Decoy key `-`, never written.
- Client id grammar: `^[A-Za-z0-9]{1,100}$` — unchanged; applied by the codec, `Create`, the adapter and `Authenticate`.
- `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT`: default 100; 0 = unbounded; < 0 refuses to start.
- Error mapping: `ErrInvalidClient` → 401 `invalid_client`; `ErrM2MClientNotFound` → 404 `M2M_CLIENT_NOT_FOUND`; `ErrM2MClientCapReached` → 400 `M2M_CLIENT_CAP_REACHED`; anything else → `common.Internal` (500 + ticket, or 503 `STORAGE_UNAVAILABLE`) on `/clients`, `writeTokenServerError` (500 `server_error` + ticket) on `/oauth/token`.
- SPI mechanics: read `cyoda-go-spi/MAINTAINING.md` and cyoda-go `MAINTAINING.md` end to end before Task 1. Mid-milestone: no SPI tag; SPI PR into `main`; cyoda-go pins a pseudo-version of SPI `main`; `make repin-plugins` after every plugin change (push → repin → new commit → push; never amend the commit repin pointed at). `go.work` is tracked: its local `use` line for the SPI stays uncommitted — stage files explicitly, never `git add -A` while it is present.
- The cassandra repo is private (the commercial backend); its PR carries only its title.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. A client whose create was interrupted between the record write and the index write is listed but must never authenticate, and `DELETE` must remove it (Task 5 `TestKVM2M_RecordWithoutIndex*`).
2. Two creates in one tenant racing on one node must not both pass the cap (Task 5 `TestKVM2M_CapHoldsUnderConcurrentCreatesOnOneNode`).
3. An unknown id, a record without an index entry, and a wrong secret must each cost the same two KV reads and one bcrypt (Task 5 `TestKVM2M_AuthenticateReadShape`).
4. A `DELETE` by tenant A of an id whose index entry names tenant B must leave B's client working (Task 5 `TestKVM2M_DeleteNeverTouchesAnotherTenantsIndex`).
5. A request that carries an entity transaction in its context (a callback joined to a transaction) must not put M2M reads or writes inside that transaction (Task 5 `TestKVM2M_IgnoresCallerTransaction`).

---

## File Structure

| File | Responsibility |
|---|---|
| `cyoda-go-spi/persistence.go`, `spitest/{keyvalue,message,workflow}.go` | the absent-key `Delete` contract and its conformance cases |
| `cyoda-go-cassandra/internal/store/data_store.go` | `dataStore.delete` returns nil for an absent key |
| `internal/auth/kv_m2m_codec.go` (new) | record and index-entry encode/decode, validation |
| `internal/auth/kv_m2m_store.go` (new) | `KVM2MClientStore` |
| `internal/auth/kv_m2m_*_test.go` (new) | unit tests |
| `internal/auth/store.go` | interface, errors; the in-memory store deleted |
| `internal/auth/token.go`, `service.go`, `iam_features.go` | wiring, cap |
| `internal/domain/account/m2m_adapter.go` | error mapping; pre-reads removed |
| `app/config.go`, `cmd/cyoda/help/config_registry.go` | the cap variable |
| `internal/common/error_codes.go`, `cmd/cyoda/help/content/errors/M2M_CLIENT_CAP_REACHED.md` | new error code |
| `api/openapi.yaml` | 503 and 400 on `/clients` |
| `internal/e2e/clients_store_test.go` (new) | running-backend coverage |
| `e2e/parity/m2m_clients.go`, `e2e/parity/message_delete_absent.go` (new), `e2e/parity/multinode/m2m_clients.go` (new) | parity |

---

### Task 1: SPI — deleting an absent key returns nil

Work in `/Users/paul/go-projects/cyoda-light/cyoda-go-spi` on a branch `feat/delete-absent-key-nil` from `main`. If this session's worktree isolation refuses git there, run this task from a session or subagent whose working directory is that checkout.

**Files:**
- Modify: `persistence.go:545-557` (doc comments on `KeyValueStore.Delete`, `MessageStore.Delete`/`DeleteBatch`, and `WorkflowStore.Delete` — find the latter with `grep -n "type WorkflowStore interface" -A8 persistence.go`)
- Modify: `spitest/keyvalue.go`, `spitest/message.go`, `spitest/workflow.go` (new subtests registered where the existing `runSubtest` calls are)
- Modify: `CHANGELOG.md` `[Unreleased]`

- [ ] **Step 1: Write the conformance cases**

`spitest/keyvalue.go` — register `runSubtest(t, h, tracker, "DeleteAbsent", testKVDeleteAbsent)` next to `"Delete"`:
```go
// Deleting a key that is absent — never written, or already deleted — is
// not an error.
func testKVDeleteAbsent(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	kv, _ := h.Factory.KeyValueStore(ctx)
	require.NoError(t, kv.Delete(ctx, "ns", "never-written"))
	require.NoError(t, kv.Put(ctx, "ns", "k", []byte("v")))
	require.NoError(t, kv.Delete(ctx, "ns", "k"))
	require.NoError(t, kv.Delete(ctx, "ns", "k"))
}
```
`spitest/message.go` — `"DeleteAbsent"`:
```go
func testMsgDeleteAbsent(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	ms, _ := h.Factory.MessageStore(ctx)
	require.NoError(t, ms.Delete(ctx, "never-written"))
	require.NoError(t, ms.DeleteBatch(ctx, []string{"never-written-1", "never-written-2"}))
}
```
Also extend `testMsgDeleteBatch` with a batch that mixes a present and an absent id and asserts `NoError` and that the present one is gone (read the existing body first and follow its save/get helpers).
`spitest/workflow.go` — `"DeleteAbsent"`: `require.NoError(t, ws.Delete(ctx, spi.ModelRef{EntityName: "never", ModelVersion: "1"}))` (match the `ModelRef` field names used in `testWfDelete`).

Doc comments in `persistence.go`, e.g. on `KeyValueStore`:
```go
	// Delete removes key. Deleting a key that is absent — never written or
	// already deleted — returns nil.
	Delete(ctx context.Context, namespace string, key string) error
```
and the same sentence on `MessageStore.Delete`, `MessageStore.DeleteBatch` ("an absent id in ids is not an error") and `WorkflowStore.Delete`.

`CHANGELOG.md` `[Unreleased]` → `### Changed`: "`Delete` (KeyValueStore, MessageStore, WorkflowStore) and `MessageStore.DeleteBatch` of an absent key return nil; spitest checks it. Backends that returned `ErrNotFound` must change."

- [ ] **Step 2: Verify**

spitest does not run in this repo (no harness). Run `go build ./... && go vet ./...` here; the cases execute in Task 2 (memory, sqlite, postgres) and Task 3 (cassandra). Prove they have teeth in Task 3 (cassandra fails them before its fix).

- [ ] **Step 3: Commit, push, PR into `main`**

```bash
git add persistence.go spitest/keyvalue.go spitest/message.go spitest/workflow.go CHANGELOG.md
git commit -m "feat(spi): deleting an absent key returns nil; spitest checks it"
git push -u origin feat/delete-absent-key-nil
gh pr create --base main --title "feat(spi): deleting an absent key returns nil" --body "<summary; the backend divergence it closes; consumers: cyoda-go (memory/sqlite/postgres already conform), cyoda-go-cassandra (changes)>"
```
Notify each entry of `KNOWN_CONSUMERS.md` (link in the PR body). Merge when green (squash).

---

### Task 2: cyoda-go — pin the SPI, drop the unreachable tolerance, parity for message delete

**Files:**
- Modify: `go.mod`, `plugins/*/go.mod` (pseudo-pin of SPI `main` after Task 1 merged; `make check-spi-pin-sync`)
- Modify: `internal/auth/replica.go:352,379`
- Create: `e2e/parity/message_delete_absent.go`; register in `e2e/parity/registry.go`

- [ ] **Step 1: Pin**

```bash
go get github.com/cyoda-platform/cyoda-go-spi@main   # root
for p in memory sqlite postgres; do (cd plugins/$p && go get github.com/cyoda-platform/cyoda-go-spi@main); done
make check-spi-pin-sync
```
Commit the four go.mod/go.sum pairs in one commit (`chore(deps): pin cyoda-go-spi main for the absent-key Delete contract`).

- [ ] **Step 2: Run the new conformance cases**

Run: `make test` (runs every plugin's spitest conformance through the parity/conformance wrappers — check with `grep -rn "spitest.Run" plugins/` which test functions they are, and run them explicitly: `cd plugins/memory && go test ./... -run Conformance`, same for sqlite and postgres).
Expected: PASS — memory, sqlite and postgres already return nil.

- [ ] **Step 3: Write the parity scenario (fails on cassandra until Task 3)**

`e2e/parity/message_delete_absent.go`:
```go
package parity

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunMessageDeleteBatchWithAbsentID: a batch delete that names an id that
// does not exist succeeds, and deletes the ids that do.
func RunMessageDeleteBatchWithAbsentID(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	id, err := c.CreateMessage(t, "absent-delete", `{"a":1}`)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := c.DeleteMessages(t, []string{id, uuid.NewString()}); err != nil {
		t.Fatalf("batch delete with an absent id: %v", err)
	}
	if _, err := c.GetMessage(t, id); err == nil {
		t.Fatal("the present message was not deleted")
	}
}
```
Register `{"MessageDeleteBatchWithAbsentID", RunMessageDeleteBatchWithAbsentID}` in `registry.go`'s list. Run: `go test ./e2e/parity/memory/ ./e2e/parity/sqlite/ -run 'Parity/MessageDeleteBatchWithAbsentID'` (use the test names the wrappers expose) → PASS.

- [ ] **Step 4: Remove the tolerance**

`internal/auth/replica.go:379`: `put` with `value == nil` becomes `return r.kv.Delete(ctx, r.cfg.namespace, key)`; update the comment at `:352` (a delete of an absent key already returns nil by the SPI contract). The existing replica tests (`replica_internal_test.go`) must stay green: `go test ./internal/auth/`.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/replica.go e2e/parity/message_delete_absent.go e2e/parity/registry.go
git commit -m "test(parity): a batch message delete with an absent id succeeds; drop the replica's ErrNotFound tolerance"
```

---

### Task 3: cassandra — `dataStore.delete` returns nil for an absent key

Work in `/Users/paul/go-projects/cyoda-light/cyoda-go-cassandra` on a branch from `main` (same isolation note as Task 1).

**Files:**
- Modify: `internal/store/data_store.go:163-181` (`delete`)
- Modify: `go.mod` (SPI pseudo-pin of `main`; cyoda-go pseudo-pin of Part B's pushed head for the new parity scenario)
- Modify: `cyoda-go-cassandra-docker.sh:53-56`, `.env.cassandra.example:35-42` (the bootstrap client lines removed by Part A: replace with a comment "get a first admin token with: cyoda token --tenant <tenant>")

- [ ] **Step 1: Red**

Pin the SPI and cyoda-go as above, then run the cassandra conformance and parity suites (`make test` or the repo's documented targets; see its README). Expected: `KeyValueStore/DeleteAbsent`, `MessageStore/DeleteAbsent`, `WorkflowStore/DeleteAbsent` and `Parity/MessageDeleteBatchWithAbsentID` FAIL with `ErrNotFound`. Keep the output for the PR.

- [ ] **Step 2: Fix**

In `dataStore.delete`, where the metadata read finds no row or a deleted row:
```go
	if err == gocql.ErrNotFound || deleted {
		return nil // deleting an absent key is not an error (SPI contract)
	}
```
Check every caller in the plugin (`grep -rn "ds.delete(\|\.delete(ctx" internal`) for a dependency on the old `ErrNotFound`; there are four (KV, workflow, message, message batch) and none branches on it.

- [ ] **Step 3: Green, commit, PR**

Run the same suites → PASS. Commit (`fix(store): deleting an absent key returns nil`), push, open the PR with its title only, merge when green. Then in cyoda-go: nothing to pin for cassandra (it is out of tree); record in `COMPATIBILITY.md` (Task 10) that the cassandra plugin must include this fix.

---

### Task 4: the record codec

**Files:**
- Create: `internal/auth/kv_m2m_codec.go`, `internal/auth/kv_m2m_codec_internal_test.go`

**Interfaces:**
- Produces (package `auth`, unexported unless stated):
```go
const (
	m2mClientsNamespacePrefix = "m2m-clients:"
	m2mClientIndexNamespace   = "m2m-client-ids"
	m2mDecoyKey               = "-" // never written; outside the client-id grammar
)
func ValidClientID(id string) bool // exported; the adapter uses it
func m2mTenantNamespace(t spi.TenantID) string
func encodeClientRecord(c *M2MClient) ([]byte, error)
func decodeClientRecord(tenant spi.TenantID, key string, data []byte) (*M2MClient, error)
func encodeIndexEntry(t spi.TenantID) ([]byte, error)
func decodeIndexEntry(data []byte) (spi.TenantID, error)
var errM2MUndecodable = errors.New("stored m2m client data does not decode")
```

- [ ] **Step 1: Write the failing tests**

```go
package auth

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func validClient(t *testing.T) *M2MClient {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte("s"), bcrypt.MinCost)
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &M2MClient{ClientID: "ABC123", HashedSecret: string(h), TenantID: "acme", UserID: "ABC123", Roles: []string{"ROLE_M2M"}, CreatedAt: now, UpdatedAt: now}
}

func TestM2MCodec_RoundTrip(t *testing.T) {
	c := validClient(t)
	b, err := encodeClientRecord(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeClientRecord("acme", "ABC123", b)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != c.ClientID || got.TenantID != c.TenantID || got.UserID != c.UserID || got.HashedSecret != c.HashedSecret ||
		!got.CreatedAt.Equal(c.CreatedAt) || !got.UpdatedAt.Equal(c.UpdatedAt) || strings.Join(got.Roles, ",") != "ROLE_M2M" {
		t.Fatalf("round trip: %+v", got)
	}
	ib, _ := encodeIndexEntry("acme")
	if tn, err := decodeIndexEntry(ib); err != nil || tn != "acme" {
		t.Fatalf("index: %v %v", tn, err)
	}
}

func TestM2MCodec_EncoderRefuses(t *testing.T) {
	for name, mut := range map[string]func(c *M2MClient){
		"bad id":        func(c *M2MClient) { c.ClientID = "a-b" },
		"bad tenant":    func(c *M2MClient) { c.TenantID = spi.TenantID("a:b") },
		"bad user":      func(c *M2MClient) { c.UserID = "oidc:x" },
		"no roles":      func(c *M2MClient) { c.Roles = nil },
		"empty role":    func(c *M2MClient) { c.Roles = []string{""} },
		"not a hash":    func(c *M2MClient) { c.HashedSecret = "plaintext" },
		"year 10000":    func(c *M2MClient) { c.CreatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
	} {
		t.Run(name, func(t *testing.T) {
			c := validClient(t)
			mut(c)
			if _, err := encodeClientRecord(c); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestM2MCodec_DecoderRefuses(t *testing.T) {
	good, _ := encodeClientRecord(validClient(t))
	cases := map[string]struct {
		tenant spi.TenantID
		key    string
		data   []byte
	}{
		"not json":         {"acme", "ABC123", []byte("{")},
		"key differs":      {"acme", "OTHER1", good},
		"tenant differs":   {"other", "ABC123", good},
		"key outside grammar": {"acme", "a-b", []byte(strings.Replace(string(good), `"ABC123"`, `"a-b"`, 1))},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeClientRecord(tc.tenant, tc.key, tc.data); !errors.Is(err, errM2MUndecodable) {
				t.Fatalf("err = %v, want errM2MUndecodable", err)
			}
		})
	}
	if _, err := decodeIndexEntry([]byte(`{"tenantId":"a:b"}`)); !errors.Is(err, errM2MUndecodable) {
		t.Fatalf("index with bad tenant: %v", err)
	}
}

func TestM2MCodec_RecordHoldsNoPlaintext(t *testing.T) {
	c := validClient(t)
	b, _ := encodeClientRecord(c)
	if strings.Contains(string(b), `"s"`) {
		t.Fatal("record contains the plaintext secret")
	}
}
```
(add `errors` to the imports.)

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/auth/ -run TestM2MCodec` → compile errors.

- [ ] **Step 3: Implement** `kv_m2m_codec.go`:

```go
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"golang.org/x/crypto/bcrypt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const (
	m2mClientsNamespacePrefix = "m2m-clients:"
	m2mClientIndexNamespace   = "m2m-client-ids"
	// m2mDecoyKey is read on an index miss so every decided token request
	// makes two reads. It is outside the client-id grammar, so never written.
	m2mDecoyKey = "-"
)

var errM2MUndecodable = errors.New("stored m2m client data does not decode")

var clientIDGrammar = regexp.MustCompile(`^[A-Za-z0-9]{1,100}$`)

// ValidClientID reports whether id matches the client-id grammar.
func ValidClientID(id string) bool { return clientIDGrammar.MatchString(id) }

// m2mTenantNamespace is the KV namespace holding tenant's client records.
// Tenant ids cannot contain ':', so namespaces cannot alias.
func m2mTenantNamespace(t spi.TenantID) string { return m2mClientsNamespacePrefix + string(t) }

type m2mClientRecord struct {
	ClientID     string   `json:"clientId"`
	TenantID     string   `json:"tenantId"`
	UserID       string   `json:"userId"`
	Roles        []string `json:"roles"`
	HashedSecret string   `json:"hashedSecret"`
	CreatedAt    string   `json:"createdAt"`
	UpdatedAt    string   `json:"updatedAt"`
}

type m2mIndexEntry struct {
	TenantID string `json:"tenantId"`
}

func validateM2MClient(c *M2MClient) error {
	if !ValidClientID(c.ClientID) {
		return errors.New("client id outside the grammar")
	}
	if err := common.ValidateTenantID(c.TenantID); err != nil {
		return fmt.Errorf("tenant: %w", err)
	}
	if err := common.ValidateFirstPartyUserID(c.UserID); err != nil {
		return fmt.Errorf("user: %w", err)
	}
	if len(c.Roles) == 0 {
		return errors.New("no roles")
	}
	for _, r := range c.Roles {
		if r == "" {
			return errors.New("empty role")
		}
	}
	if _, err := bcrypt.Cost([]byte(c.HashedSecret)); err != nil {
		return errors.New("hashedSecret is not a bcrypt hash")
	}
	if !StorableTime(c.CreatedAt) || !StorableTime(c.UpdatedAt) {
		return errors.New("timestamp out of range")
	}
	return nil
}

// encodeClientRecord refuses anything decodeClientRecord would reject.
func encodeClientRecord(c *M2MClient) ([]byte, error) {
	if err := validateM2MClient(c); err != nil {
		return nil, fmt.Errorf("failed to encode m2m client record: %w", err)
	}
	return json.Marshal(m2mClientRecord{
		ClientID: c.ClientID, TenantID: string(c.TenantID), UserID: c.UserID,
		Roles: append([]string(nil), c.Roles...), HashedSecret: c.HashedSecret,
		CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: c.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
}

// decodeClientRecord binds the record to its KV key and namespace tenant.
func decodeClientRecord(tenant spi.TenantID, key string, data []byte) (*M2MClient, error) {
	var r m2mClientRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%w: %w", errM2MUndecodable, err)
	}
	if r.ClientID != key || spi.TenantID(r.TenantID) != tenant {
		return nil, fmt.Errorf("%w: record does not match its key or namespace", errM2MUndecodable)
	}
	created, err1 := time.Parse(time.RFC3339Nano, r.CreatedAt)
	updated, err2 := time.Parse(time.RFC3339Nano, r.UpdatedAt)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("%w: bad timestamp", errM2MUndecodable)
	}
	c := &M2MClient{ClientID: r.ClientID, HashedSecret: r.HashedSecret, TenantID: spi.TenantID(r.TenantID), UserID: r.UserID, Roles: r.Roles, CreatedAt: created, UpdatedAt: updated}
	if err := validateM2MClient(c); err != nil {
		return nil, fmt.Errorf("%w: %w", errM2MUndecodable, err)
	}
	return c, nil
}

func encodeIndexEntry(t spi.TenantID) ([]byte, error) {
	if err := common.ValidateTenantID(t); err != nil {
		return nil, fmt.Errorf("failed to encode m2m client index entry: %w", err)
	}
	return json.Marshal(m2mIndexEntry{TenantID: string(t)})
}

func decodeIndexEntry(data []byte) (spi.TenantID, error) {
	var e m2mIndexEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return "", fmt.Errorf("%w: %w", errM2MUndecodable, err)
	}
	if err := common.ValidateTenantID(spi.TenantID(e.TenantID)); err != nil {
		return "", fmt.Errorf("%w: %w", errM2MUndecodable, err)
	}
	return spi.TenantID(e.TenantID), nil
}
```
(Check `StorableTime`'s exact name and signature: `grep -n "func StorableTime" internal/auth/*.go`.)

- [ ] **Step 4:** `go test ./internal/auth/ -run TestM2MCodec` → PASS.
- [ ] **Step 5: Commit** `feat(auth): M2M client record codec`.

---

### Task 5: `KVM2MClientStore`

**Files:**
- Create: `internal/auth/kv_m2m_store.go`, `internal/auth/kv_m2m_store_test.go` (package `auth_test`), `internal/auth/kv_m2m_store_internal_test.go` (package `auth`, for read counting)
- Modify: `internal/auth/store.go` — add `ErrInvalidClient`, `ErrM2MClientCapReached`; replace the `M2MClientStore` interface (below). Replacing the interface breaks `InMemoryM2MClientStore`'s callers, so Task 5 and Task 6 are committed together if the build cannot stay green between them. Never introduce a second, parallel interface.

**Interfaces:**
- Produces:
```go
var ErrInvalidClient = errors.New("invalid client")
var ErrM2MClientCapReached = errors.New("m2m client cap reached")

type M2MClientStore interface {
	Create(ctx context.Context, tenantID spi.TenantID, clientID, userID string, roles []string) (secret string, err error)
	Authenticate(ctx context.Context, clientID, secret string) (*M2MClient, error)
	List(ctx context.Context, tenantID spi.TenantID) ([]*M2MClient, error)
	Delete(ctx context.Context, tenantID spi.TenantID, clientID string) error
	ResetSecret(ctx context.Context, tenantID spi.TenantID, clientID string) (secret string, c *M2MClient, err error)
}

func NewKVM2MClientStore(kv spi.KeyValueStore, maxPerTenant int) *KVM2MClientStore
```

- [ ] **Step 1: Write the failing tests**

`kv_m2m_store_test.go` (use `mustNewMemoryKV(t, systemCtx())` from `kv_trusted_store_test.go` and the existing `failingKV` wrapper; add a `countingKV` below):
```go
func newM2M(t *testing.T, max int) (*auth.KVM2MClientStore, spi.KeyValueStore) {
	t.Helper()
	kv := mustNewMemoryKV(t, systemCtx())
	return auth.NewKVM2MClientStore(kv, max), kv
}

func TestKVM2M_Lifecycle(t *testing.T) {
	s, _ := newM2M(t, 0)
	ctx := systemCtx()
	sec, err := s.Create(ctx, "acme", "C1", "C1", []string{"ROLE_M2M"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Authenticate(ctx, "C1", sec)
	if err != nil || c.TenantID != "acme" || c.ClientID != "C1" {
		t.Fatalf("authenticate: %v %v", c, err)
	}
	if _, err := s.Authenticate(ctx, "C1", "wrong"); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("wrong secret: %v", err)
	}
	list, err := s.List(ctx, "acme")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	sec2, c2, err := s.ResetSecret(ctx, "acme", "C1")
	if err != nil || c2.ClientID != "C1" || c2.UpdatedAt.Before(c.CreatedAt) || !c2.CreatedAt.Equal(c.CreatedAt) {
		t.Fatalf("reset: %v %v", c2, err)
	}
	if _, err := s.Authenticate(ctx, "C1", sec); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatal("old secret still works")
	}
	if _, err := s.Authenticate(ctx, "C1", sec2); err != nil {
		t.Fatal("new secret refused")
	}
	if err := s.Delete(ctx, "acme", "C1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "C1", sec2); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatal("deleted client authenticates")
	}
	if err := s.Delete(ctx, "acme", "C1"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestKVM2M_SharedAcrossInstances(t *testing.T) {
	kv := mustNewMemoryKV(t, systemCtx())
	a, b := auth.NewKVM2MClientStore(kv, 0), auth.NewKVM2MClientStore(kv, 0)
	sec, _ := a.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	if _, err := b.Authenticate(systemCtx(), "C1", sec); err != nil {
		t.Fatal("node B does not see node A's client")
	}
	_ = a.Delete(systemCtx(), "acme", "C1")
	if _, err := b.Authenticate(systemCtx(), "C1", sec); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatal("node B still accepts a client node A deleted")
	}
}

func TestKVM2M_TenantIsolation(t *testing.T) {
	s, _ := newM2M(t, 0)
	sec, _ := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	if l, _ := s.List(systemCtx(), "other"); len(l) != 0 {
		t.Fatal("other tenant lists acme's client")
	}
	if err := s.Delete(systemCtx(), "other", "C1"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("cross-tenant delete: %v", err)
	}
	if _, _, err := s.ResetSecret(systemCtx(), "other", "C1"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("cross-tenant reset: %v", err)
	}
	if _, err := s.Authenticate(systemCtx(), "C1", sec); err != nil {
		t.Fatal("acme's client damaged by another tenant's calls")
	}
}

func TestKVM2M_DeleteNeverTouchesAnotherTenantsIndex(t *testing.T) {
	s, kv := newM2M(t, 0)
	sec, _ := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	// An orphan record of the same id in tenant "other" (as a crash could leave).
	orphan, _ := s.Create(systemCtx(), "other", "C2", "C2", []string{"ROLE_M2M"})
	_ = orphan
	raw, _ := kv.Get(systemCtx(), "m2m-clients:other", "C2")
	_ = kv.Put(systemCtx(), "m2m-clients:other", "C1", []byte(strings.Replace(string(raw), `"C2"`, `"C1"`, 1)))
	if err := s.Delete(systemCtx(), "other", "C1"); err != nil {
		t.Fatalf("delete of other's orphan: %v", err)
	}
	if _, err := s.Authenticate(systemCtx(), "C1", sec); err != nil {
		t.Fatal("acme's client lost its index entry")
	}
}

func TestKVM2M_RecordWithoutIndexNeverAuthenticatesAndIsRemovable(t *testing.T) {
	s, kv := newM2M(t, 0)
	sec, _ := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	_ = kv.Delete(systemCtx(), "m2m-client-ids", "C1")
	if _, err := s.Authenticate(systemCtx(), "C1", sec); !errors.Is(err, auth.ErrInvalidClient) {
		t.Fatal("record without index authenticates")
	}
	if _, _, err := s.ResetSecret(systemCtx(), "acme", "C1"); !errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("reset of unindexed record: %v", err)
	}
	if l, _ := s.List(systemCtx(), "acme"); len(l) != 1 {
		t.Fatal("unindexed record not listed")
	}
	if err := s.Delete(systemCtx(), "acme", "C1"); err != nil {
		t.Fatalf("delete of unindexed record: %v", err)
	}
	if l, _ := s.List(systemCtx(), "acme"); len(l) != 0 {
		t.Fatal("still listed after delete")
	}
}

func TestKVM2M_UndecodableRecord(t *testing.T) {
	s, kv := newM2M(t, 0)
	_, _ = s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	_, _ = s.Create(systemCtx(), "acme", "C2", "C2", []string{"ROLE_M2M"})
	_ = kv.Put(systemCtx(), "m2m-clients:acme", "C1", []byte("{"))
	if _, err := s.Authenticate(systemCtx(), "C1", "x"); err == nil || errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("authenticate on undecodable: %v, want a store error", err)
	}
	if l, err := s.List(systemCtx(), "acme"); err != nil || len(l) != 1 || l[0].ClientID != "C2" {
		t.Fatalf("list must skip the undecodable record: %v %v", l, err)
	}
	if _, _, err := s.ResetSecret(systemCtx(), "acme", "C1"); err == nil || errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("reset on undecodable: %v, want a store error", err)
	}
	if err := s.Delete(systemCtx(), "acme", "C1"); err != nil {
		t.Fatalf("delete must remove an undecodable record of the caller's tenant: %v", err)
	}
}

func TestKVM2M_Cap(t *testing.T) {
	s, _ := newM2M(t, 2)
	_, _ = s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	_, _ = s.Create(systemCtx(), "acme", "C2", "C2", []string{"ROLE_M2M"})
	if _, err := s.Create(systemCtx(), "acme", "C3", "C3", []string{"ROLE_M2M"}); !errors.Is(err, auth.ErrM2MClientCapReached) {
		t.Fatalf("third create: %v", err)
	}
	if _, err := s.Create(systemCtx(), "other", "C4", "C4", []string{"ROLE_M2M"}); err != nil {
		t.Fatal("cap is per tenant")
	}
	_ = s.Delete(systemCtx(), "acme", "C1")
	if _, err := s.Create(systemCtx(), "acme", "C3", "C3", []string{"ROLE_M2M"}); err != nil {
		t.Fatalf("after a delete: %v", err)
	}
}

func TestKVM2M_CapHoldsUnderConcurrentCreatesOnOneNode(t *testing.T) {
	s, _ := newM2M(t, 3)
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Create(systemCtx(), "acme", fmt.Sprintf("C%d", i), fmt.Sprintf("C%d", i), []string{"ROLE_M2M"}); err == nil {
				ok.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 3 {
		t.Fatalf("%d creates passed the cap of 3", ok.Load())
	}
}

func TestKVM2M_CreateRefusesExistingAndInvalidIDs(t *testing.T) {
	s, kv := newM2M(t, 0)
	_, _ = s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	if _, err := s.Create(systemCtx(), "other", "C1", "C1", []string{"ROLE_M2M"}); !errors.Is(err, auth.ErrM2MClientExists) {
		t.Fatalf("existing id in another tenant: %v", err)
	}
	_ = kv.Put(systemCtx(), "m2m-client-ids", "C9", []byte("{"))
	if _, err := s.Create(systemCtx(), "acme", "C9", "C9", []string{"ROLE_M2M"}); !errors.Is(err, auth.ErrM2MClientExists) {
		t.Fatalf("undecodable index entry's id: %v", err)
	}
	if _, err := s.Create(systemCtx(), "acme", "a-b", "a-b", []string{"ROLE_M2M"}); err == nil {
		t.Fatal("id outside the grammar created")
	}
}

// failNSKV fails every Put into one namespace, writing nothing.
type failNSKV struct {
	spi.KeyValueStore
	ns string
}

func (k *failNSKV) Put(ctx context.Context, ns, key string, v []byte) error {
	if ns == k.ns {
		return errors.New("injected: write failed")
	}
	return k.KeyValueStore.Put(ctx, ns, key, v)
}

func TestKVM2M_CreateUndoesARecordWhenTheIndexWriteFails(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	s := auth.NewKVM2MClientStore(&failNSKV{KeyValueStore: mem, ns: "m2m-client-ids"}, 0)
	if _, err := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}); err == nil {
		t.Fatal("want error")
	}
	if _, err := mem.Get(systemCtx(), "m2m-clients:acme", "C1"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatal("record left behind")
	}
}

// A Put that commits and then reports an error (a timeout after commit).
type commitThenFailKV struct {
	spi.KeyValueStore
	ns string
}

func (k *commitThenFailKV) Put(ctx context.Context, ns, key string, v []byte) error {
	if err := k.KeyValueStore.Put(ctx, ns, key, v); err != nil {
		return err
	}
	if ns == k.ns {
		return errors.New("injected: committed, then failed")
	}
	return nil
}

func TestKVM2M_CreateUndoesAnAmbiguousIndexWrite(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	s := auth.NewKVM2MClientStore(&commitThenFailKV{KeyValueStore: mem, ns: "m2m-client-ids"}, 0)
	if _, err := s.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"}); err == nil {
		t.Fatal("want error")
	}
	for _, ns := range []string{"m2m-client-ids", "m2m-clients:acme"} {
		if _, err := mem.Get(systemCtx(), ns, "C1"); !errors.Is(err, spi.ErrNotFound) {
			t.Fatalf("%s/C1 left behind", ns)
		}
	}
}

func TestKVM2M_StoreFailureIsNeverNotFoundOrInvalid(t *testing.T) {
	mem := mustNewMemoryKV(t, systemCtx())
	good := auth.NewKVM2MClientStore(mem, 0)
	sec, _ := good.Create(systemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	s := auth.NewKVM2MClientStore(brokenKV{}, 0)
	if _, err := s.Authenticate(systemCtx(), "C1", sec); err == nil || errors.Is(err, auth.ErrInvalidClient) {
		t.Fatalf("authenticate: %v", err)
	}
	if _, err := s.List(systemCtx(), "acme"); err == nil {
		t.Fatal("list: want error")
	}
	if err := s.Delete(systemCtx(), "acme", "C1"); err == nil || errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := s.ResetSecret(systemCtx(), "acme", "C1"); err == nil || errors.Is(err, auth.ErrM2MClientNotFound) {
		t.Fatalf("reset: %v", err)
	}
	if _, err := s.Create(systemCtx(), "acme", "C2", "C2", []string{"ROLE_M2M"}); err == nil || errors.Is(err, auth.ErrM2MClientExists) {
		t.Fatalf("create: %v", err)
	}
}

// brokenKV fails every call with a storage-unavailable error.
type brokenKV struct{}

type unavailable struct{}

func (unavailable) Error() string             { return "storage down" }
func (unavailable) StorageUnavailable() bool  { return true }

func (brokenKV) Put(context.Context, string, string, []byte) error         { return unavailable{} }
func (brokenKV) Get(context.Context, string, string) ([]byte, error)       { return nil, unavailable{} }
func (brokenKV) Delete(context.Context, string, string) error              { return unavailable{} }
func (brokenKV) List(context.Context, string) (map[string][]byte, error)   { return nil, unavailable{} }

func TestKVM2M_AuthenticateRefusesMalformedIDsWithoutReading(t *testing.T) {
	s := auth.NewKVM2MClientStore(brokenKV{}, 0) // any read would error
	for _, id := range []string{"a\x00b", "\xff", strings.Repeat("A", 101), "a:b", ""} {
		if _, err := s.Authenticate(systemCtx(), id, "x"); !errors.Is(err, auth.ErrInvalidClient) {
			t.Fatalf("%q: %v, want ErrInvalidClient without a store read", id, err)
		}
	}
}

func TestKVM2M_IgnoresCallerTransaction(t *testing.T) {
	s, _ := newM2M(t, 0)
	ctx := spi.WithTransaction(systemCtx(), &spi.TransactionState{}) // a caller's transaction
	sec, err := s.Create(ctx, "acme", "C1", "C1", []string{"ROLE_M2M"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(systemCtx(), "C1", sec); err != nil {
		t.Fatal("a write made under a caller's transaction is not visible outside it")
	}
}
```
(For `TestKVM2M_IgnoresCallerTransaction`, check how `spi.TransactionState` is constructed in tests elsewhere: `grep -rn "spi.WithTransaction(" --include=*_test.go . | head`. The memory KV does not join transactions, so this test is red only against a store that forgets the strip on postgres; the unit test pins the call, and Task 8's e2e covers postgres: create a client from a callback joined to a transaction and assert it is visible from another stack before that transaction commits.)

`kv_m2m_store_internal_test.go` — read counting:
```go
type countingKV struct {
	spi.KeyValueStore
	gets atomic.Int32
}

func (c *countingKV) Get(ctx context.Context, ns, key string) ([]byte, error) {
	c.gets.Add(1)
	return c.KeyValueStore.Get(ctx, ns, key)
}

func TestKVM2M_AuthenticateReadShape(t *testing.T) {
	mem := newReplicaKV(t)
	ckv := &countingKV{KeyValueStore: mem}
	s := NewKVM2MClientStore(ckv, 0)
	sec, _ := s.Create(replicaSystemCtx(), "acme", "C1", "C1", []string{"ROLE_M2M"})
	_, _ = s.Create(replicaSystemCtx(), "acme", "C2", "C2", []string{"ROLE_M2M"})
	_ = mem.Delete(replicaSystemCtx(), m2mClientIndexNamespace, "C2") // C2: record without index
	for name, call := range map[string]func(){
		"unknown id":           func() { _, _ = s.Authenticate(replicaSystemCtx(), "NOPE", "x") },
		"record without index": func() { _, _ = s.Authenticate(replicaSystemCtx(), "C2", "x") },
		"wrong secret":         func() { _, _ = s.Authenticate(replicaSystemCtx(), "C1", "x") },
		"right secret":         func() { _, _ = s.Authenticate(replicaSystemCtx(), "C1", sec) },
	} {
		ckv.gets.Store(0)
		call()
		if n := ckv.gets.Load(); n != 2 {
			t.Errorf("%s: %d reads, want 2", name, n)
		}
	}
}
```
(bcrypt count is structural: every decided path calls exactly one of `bcrypt.CompareHashAndPassword(dummyHash, …)` or `(record hash, …)`; the reviewer checks it. Per the project rule there is no bcrypt counter seam.)

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/auth/ -run TestKVM2M` → compile errors.

- [ ] **Step 3: Implement** `internal/auth/kv_m2m_store.go`:

```go
package auth

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// createLockStripes bounds the per-tenant create locks' memory: tenants hash
// onto a fixed set of mutexes.
const createLockStripes = 64

// undoTimeout bounds the compensation of a failed create.
const undoTimeout = 30 * time.Second

// KVM2MClientStore stores M2M clients in the SYSTEM-tenant KV store: one
// namespace per tenant for the records, one global namespace mapping a
// client id to its tenant. There is no node copy: every call reads or writes
// the store, so a change is visible to every node when the call returns.
// Every call strips any transaction from its context: the postgres KV store
// joins a transaction it finds there, and a client change must never ride on
// a caller's entity transaction.
//
// A client exists when its record exists and the index entry for its id
// names its tenant. Create writes the record, then the index entry; Delete
// removes the index entry, then the record. The KV SPI has no
// compare-and-set: two admin changes to one client on two nodes at the same
// moment resolve by last write, and the cap can be exceeded by one record per
// node. Both are documented (cyoda help auth clients).
type KVM2MClientStore struct {
	kv           spi.KeyValueStore
	maxPerTenant int
	createLocks  [createLockStripes]sync.Mutex
}

// NewKVM2MClientStore returns a store over kv. maxPerTenant <= 0: no cap.
func NewKVM2MClientStore(kv spi.KeyValueStore, maxPerTenant int) *KVM2MClientStore {
	return &KVM2MClientStore{kv: kv, maxPerTenant: maxPerTenant}
}

func noTx(ctx context.Context) context.Context { return spi.WithTransaction(ctx, nil) }

func (s *KVM2MClientStore) createLock(t spi.TenantID) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(t))
	return &s.createLocks[h.Sum32()%createLockStripes]
}

// getIndex reads the index entry of id. found=false: absent.
func (s *KVM2MClientStore) getIndex(ctx context.Context, id string) (spi.TenantID, bool, error) {
	data, err := s.kv.Get(ctx, m2mClientIndexNamespace, id)
	if errors.Is(err, spi.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("failed to read m2m client index: %w", err)
	}
	t, err := decodeIndexEntry(data)
	if err != nil {
		slog.Error("m2m client index entry does not decode", "pkg", "auth", "kvKey", id)
		return "", false, err
	}
	return t, true, nil
}

// getRecord reads and decodes id's record in tenant's namespace.
func (s *KVM2MClientStore) getRecord(ctx context.Context, t spi.TenantID, id string) (*M2MClient, bool, error) {
	data, err := s.kv.Get(ctx, m2mTenantNamespace(t), id)
	if errors.Is(err, spi.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("failed to read m2m client: %w", err)
	}
	c, err := decodeClientRecord(t, id, data)
	if err != nil {
		slog.Error("m2m client record does not decode", "pkg", "auth", "tenant", string(t), "kvKey", id)
		return nil, false, err
	}
	return c, true, nil
}

// burnBcrypt compares against the dummy hash so a request with no usable
// client costs what a wrong secret costs.
func burnBcrypt(secret string) { _ = bcrypt.CompareHashAndPassword(dummyHash, []byte(secret)) }

// Authenticate returns the client whose id and secret match. Every request
// that reaches a decision makes two KV reads and one bcrypt comparison, so
// timing does not reveal whether an id exists. ErrInvalidClient: no such
// client or wrong secret. Any other error is the store failing.
func (s *KVM2MClientStore) Authenticate(ctx context.Context, clientID, secret string) (*M2MClient, error) {
	ctx = noTx(ctx)
	if !ValidClientID(clientID) {
		burnBcrypt(secret)
		return nil, ErrInvalidClient
	}
	tenant, found, err := s.getIndex(ctx, clientID)
	if err != nil {
		return nil, err
	}
	var c *M2MClient
	if found {
		c, _, err = s.getRecord(ctx, tenant, clientID)
	} else {
		_, err = s.kv.Get(ctx, m2mClientIndexNamespace, m2mDecoyKey)
		if errors.Is(err, spi.ErrNotFound) {
			err = nil
		} else if err != nil {
			err = fmt.Errorf("failed to read m2m client index: %w", err)
		}
	}
	if err != nil {
		return nil, err
	}
	if c == nil {
		burnBcrypt(secret)
		return nil, ErrInvalidClient
	}
	if bcrypt.CompareHashAndPassword([]byte(c.HashedSecret), []byte(secret)) != nil {
		return nil, ErrInvalidClient
	}
	return c, nil
}

// Create adds a client and returns its plaintext secret, once.
func (s *KVM2MClientStore) Create(ctx context.Context, tenant spi.TenantID, clientID, userID string, roles []string) (string, error) {
	ctx = noTx(ctx)
	secret, err := GenerateSecret()
	if err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("failed to hash secret: %w", err)
	}
	now := time.Now().UTC()
	rec, err := encodeClientRecord(&M2MClient{ClientID: clientID, HashedSecret: string(hash), TenantID: tenant, UserID: userID, Roles: append([]string(nil), roles...), CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return "", err
	}
	idx, err := encodeIndexEntry(tenant)
	if err != nil {
		return "", err
	}
	mu := s.createLock(tenant)
	mu.Lock()
	defer mu.Unlock()
	if s.maxPerTenant > 0 {
		existing, err := s.kv.List(ctx, m2mTenantNamespace(tenant))
		if err != nil {
			return "", fmt.Errorf("failed to list m2m clients: %w", err)
		}
		if len(existing) >= s.maxPerTenant {
			return "", ErrM2MClientCapReached
		}
	}
	if _, err := s.kv.Get(ctx, m2mClientIndexNamespace, clientID); err == nil {
		return "", fmt.Errorf("%w: %s", ErrM2MClientExists, clientID)
	} else if !errors.Is(err, spi.ErrNotFound) {
		return "", fmt.Errorf("failed to read m2m client index: %w", err)
	}
	if err := s.kv.Put(ctx, m2mTenantNamespace(tenant), clientID, rec); err != nil {
		return "", fmt.Errorf("failed to write m2m client: %w", err)
	}
	if err := s.kv.Put(ctx, m2mClientIndexNamespace, clientID, idx); err != nil {
		s.undoCreate(ctx, tenant, clientID)
		return "", fmt.Errorf("failed to write m2m client index: %w", err)
	}
	return secret, nil
}

// undoCreate removes the index entry (the failed write may have committed)
// and then the record, on a context the caller cannot cancel.
func (s *KVM2MClientStore) undoCreate(ctx context.Context, t spi.TenantID, id string) {
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
	defer cancel()
	for _, ns := range []string{m2mClientIndexNamespace, m2mTenantNamespace(t)} {
		if err := s.kv.Delete(uctx, ns, id); err != nil {
			slog.Error("m2m client create could not be undone", "pkg", "auth", "namespace", ns, "kvKey", id, "error", err.Error())
		}
	}
}

// List returns tenant's clients, sorted by id. Undecodable records are
// skipped and logged at ERROR with their keys.
func (s *KVM2MClientStore) List(ctx context.Context, tenant spi.TenantID) ([]*M2MClient, error) {
	entries, err := s.kv.List(noTx(ctx), m2mTenantNamespace(tenant))
	if err != nil {
		return nil, fmt.Errorf("failed to list m2m clients: %w", err)
	}
	out := make([]*M2MClient, 0, len(entries))
	var bad []string
	for k, data := range entries {
		c, err := decodeClientRecord(tenant, k, data)
		if err != nil {
			bad = append(bad, k)
			continue
		}
		out = append(out, c)
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		slog.Error("m2m client records do not decode and are not listed", "pkg", "auth", "tenant", string(tenant), "kvKeys", bad)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClientID < out[j].ClientID })
	return out, nil
}

// Delete removes tenant's client id: the index entry if it names tenant,
// then the record if present — decodable or not; tenant's namespace proves
// ownership. An index entry naming another tenant is never touched.
func (s *KVM2MClientStore) Delete(ctx context.Context, tenant spi.TenantID, clientID string) error {
	ctx = noTx(ctx)
	if !ValidClientID(clientID) {
		return fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	_, err := s.kv.Get(ctx, m2mTenantNamespace(tenant), clientID)
	recPresent := err == nil
	if err != nil && !errors.Is(err, spi.ErrNotFound) {
		return fmt.Errorf("failed to read m2m client: %w", err)
	}
	idxTenant, idxFound, err := s.getIndex(ctx, clientID)
	if err != nil {
		return err
	}
	idxOurs := idxFound && idxTenant == tenant
	if !recPresent && !idxOurs {
		return fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	if idxOurs {
		if err := s.kv.Delete(ctx, m2mClientIndexNamespace, clientID); err != nil {
			return fmt.Errorf("failed to delete m2m client index entry: %w", err)
		}
	}
	if recPresent {
		if err := s.kv.Delete(ctx, m2mTenantNamespace(tenant), clientID); err != nil {
			return fmt.Errorf("failed to delete m2m client: %w", err)
		}
	}
	return nil
}

// ResetSecret gives an existing client of tenant a new secret and returns it,
// once, with the client. The secret is hashed before the store is read, so
// the gap between read and write is one round trip.
func (s *KVM2MClientStore) ResetSecret(ctx context.Context, tenant spi.TenantID, clientID string) (string, *M2MClient, error) {
	ctx = noTx(ctx)
	if !ValidClientID(clientID) {
		return "", nil, fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	secret, err := GenerateSecret()
	if err != nil {
		return "", nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", nil, fmt.Errorf("failed to hash secret: %w", err)
	}
	c, found, err := s.getRecord(ctx, tenant, clientID)
	if err != nil {
		return "", nil, err
	}
	idxTenant, idxFound, err := s.getIndex(ctx, clientID)
	if err != nil {
		return "", nil, err
	}
	if !found || !idxFound || idxTenant != tenant {
		return "", nil, fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	c.HashedSecret, c.UpdatedAt = string(hash), time.Now().UTC()
	rec, err := encodeClientRecord(c)
	if err != nil {
		return "", nil, err
	}
	if err := s.kv.Put(ctx, m2mTenantNamespace(tenant), clientID, rec); err != nil {
		return "", nil, fmt.Errorf("failed to write m2m client: %w", err)
	}
	return secret, c, nil
}
```
In `internal/auth/store.go`: add the two error variables with doc comments, replace the `M2MClientStore` interface with the one under **Interfaces** (doc comment: the store enforces tenant isolation; ErrInvalidClient only from Authenticate). If the build cannot stay green with `InMemoryM2MClientStore` still present, fold Task 6 into this commit.

- [ ] **Step 4:** `go test ./internal/auth/ -run 'TestKVM2M|TestM2MCodec'` → PASS. `go test -race ./internal/auth/ -run TestKVM2M_CapHoldsUnderConcurrentCreatesOnOneNode` → PASS.
- [ ] **Step 5: Commit** `feat(auth): KVM2MClientStore — M2M clients in the shared KV store`.

---

### Task 6: wire the store; delete the in-memory store

**Files:**
- Modify: `internal/auth/service.go:33,86,92,129-132`, `internal/auth/iam_features.go` (field `M2MClientMaxPerTenant`, default 100, `Validate` refuses < 0)
- Modify: `internal/auth/token.go:53-80,115-150` (one `Authenticate` call; both grants take the returned client)
- Modify: `internal/domain/account/m2m_adapter.go` (drop `clientBelongsToTenant` `:106-111`, the pre-reads `:197-203,238-243`; map errors; `validateClientID` uses `auth.ValidClientID`; `clientIDPattern` goes)
- Modify: `app/config.go` (`IAMConfig.M2MClientMaxPerTenant`, env `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT` default 100, `AuthIAMFeatures` mapping), `cmd/cyoda/help/config_registry.go` (entry), `app/config_registry_binding_test.go` (binding)
- Modify: `internal/common/error_codes.go` (`ErrCodeM2MClientCapReached = "M2M_CLIENT_CAP_REACHED"` and its set entry at `:365`), create `cmd/cyoda/help/content/errors/M2M_CLIENT_CAP_REACHED.md`
- Delete: `InMemoryM2MClientStore`, `NewInMemoryM2MClientStore`, `VerifySecret`, `Get` and their section headers in `internal/auth/store.go` (`:196-395`, including the duplicated `--- InMemoryM2MClientStore ---` header)
- Modify tests: `internal/auth/store_test.go` (M2M part → deleted; covered by Task 5), `internal/auth/token_test.go` (setup uses `NewKVM2MClientStore(mustNewMemoryKV…)`), `internal/auth/integration_test.go:50,177,237` and `delegating_test.go:49` (ids outside the grammar → `TESTAPP`, `TESTCLIENT`), `internal/auth/local_validator_integration_test.go`, `internal/domain/account/m2m_adapter_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/auth/token_test.go`:
```go
func TestTokenEndpoint_StoreFailureIs500NotInvalidClient(t *testing.T) {
	env := setupTokenEnv(t)
	h := auth.NewTokenHandler(env.keyStore, env.trustedKeyStore, auth.NewKVM2MClientStore(brokenKV{}, 0), "cyoda", "", 3600)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, makeTokenRequest("client_credentials", basicAuth(env.clientID, env.clientSecret), nil))
	if rr.Code != http.StatusInternalServerError || decodeResponse(t, rr)["error"] != "server_error" {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
}

func TestTokenEndpoint_MalformedClientIDIs401(t *testing.T) {
	env := setupTokenEnv(t)
	for _, raw := range []string{"a%00b", "%FF", strings.Repeat("A", 101), "a%3Ab"} {
		req := makeTokenRequest("client_credentials", "Basic "+base64.StdEncoding.EncodeToString([]byte(raw+":x")), nil)
		rr := httptest.NewRecorder()
		env.handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized || decodeResponse(t, rr)["error"] != "invalid_client" {
			t.Fatalf("%q: %d %s", raw, rr.Code, rr.Body.String())
		}
	}
}
```
(`brokenKV` lives in `kv_m2m_store_test.go`, same `auth_test` package.)

`internal/domain/account/m2m_adapter_test.go` (read the file's handler-construction helper first): a store whose calls fail with a storage-unavailable error → each of the four operations answers 503 `STORAGE_UNAVAILABLE`; one with a plain error → 500 with a ticket and no internal text; `Create` at the cap → 400 `M2M_CLIENT_CAP_REACHED`; `Delete`/`ResetSecret` of another tenant's client → 404 `M2M_CLIENT_NOT_FOUND` with a body identical to the absent case. Use a fake implementing the new `auth.M2MClientStore` interface.

`internal/auth/iam_features_test.go`: `M2MClientMaxPerTenant: -1` → `Validate` error; default 100.

`cmd/cyoda/help` error-code parity (`TestErrCode_Parity`) goes red once the constant exists without its `.md` — add both together.

- [ ] **Step 2: Run to verify they fail** — `go test ./internal/auth/ ./internal/domain/account/ ./app/ ./cmd/cyoda/help/` → failures/compile errors as expected.

- [ ] **Step 3: Implement**

`token.go`:
```go
	client, err := h.m2mStore.Authenticate(r.Context(), clientID, secret)
	if errors.Is(err, ErrInvalidClient) {
		writeTokenError(w, http.StatusUnauthorized, "invalid_client", "")
		return
	}
	if err != nil {
		writeTokenServerError(w, "m2mStore.Authenticate", err)
		return
	}
	switch r.FormValue("grant_type") {
	case "client_credentials":
		h.handleClientCredentials(w, r, client)
	case "urn:ietf:params:oauth:grant-type:token-exchange":
		h.handleTokenExchange(w, r, client)
	default:
		writeTokenError(w, http.StatusBadRequest, "unsupported_grant_type", "")
	}
```
`handleClientCredentials(w, r, client *M2MClient)` and `handleTokenExchange(w, r, client *M2MClient)` drop their `h.m2mStore.Get` calls and use `client.ClientID` where they used `clientID` (`sub`, `act`).

`service.go`: `m2mStore *KVM2MClientStore`; `m2mStore := NewKVM2MClientStore(config.KV, config.IAMFeatures.M2MClientMaxPerTenant)`.

Adapter:
- `CreateTechnicalUser`: `sec, err := h.m2mClientStore.Create(r.Context(), tID, cid, cid, roles)`; `errors.Is(err, auth.ErrM2MClientExists)` → retry as today; `errors.Is(err, auth.ErrM2MClientCapReached)` → `common.Operational(http.StatusBadRequest, common.ErrCodeM2MClientCapReached, "M2M client cap reached for tenant")`; else `common.Internal("m2mClientStore.Create", err)`.
- `DeleteTechnicalUser`: `err := h.m2mClientStore.Delete(r.Context(), tID, clientID)`; `ErrM2MClientNotFound` → 404; other → `common.Internal("m2mClientStore.Delete", err)`.
- `ResetTechnicalUserSecret`: `secret, c, err := h.m2mClientStore.ResetSecret(r.Context(), tID, clientID)`; same mapping; response uses `c.Roles`.
- `ListTechnicalUsers`: `clients, err := h.m2mClientStore.List(r.Context(), tID)`; err → `common.Internal("m2mClientStore.List", err)`.

`errors/M2M_CLIENT_CAP_REACHED.md` (same shape as `TRUSTED_KEY_CAP_REACHED.md`):
```markdown
---
topic: errors.M2M_CLIENT_CAP_REACHED
title: "M2M_CLIENT_CAP_REACHED — tenant M2M client cap reached"
stability: stable
see_also:
  - errors
  - auth.clients
  - config.auth
---

# errors.M2M_CLIENT_CAP_REACHED

## NAME

M2M_CLIENT_CAP_REACHED — the tenant has reached the maximum number of M2M clients.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

`POST /clients` enforces a per-tenant cap (default 100, configurable via `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT`; 0 means no cap). Delete a client the tenant no longer uses, or raise the cap. Creates on several nodes at the same moment can each pass the check, so a tenant can exceed the cap by at most one client per node.

## SEE ALSO

- errors
- auth.clients
- config.auth
```
Registry entry: `{Name: "CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT", Topic: "auth", Type: "int", Default: "100", Description: "Per-tenant cap on M2M clients; 0 means unbounded."}`.

- [ ] **Step 4:** `go build ./... && go vet ./... && go test ./internal/... ./app/ ./cmd/cyoda/...` → PASS. Exit check: `grep -rnE 'InMemoryM2MClientStore|NewInMemoryM2MClientStore|VerifySecret|clientBelongsToTenant' --exclude-dir=superpowers --exclude-dir=audits --exclude-dir=.git --exclude=CHANGELOG.md .` → empty.
- [ ] **Step 5: Commit** `feat(auth)!: M2M clients stored and shared by the cluster; per-tenant cap`.

---

### Task 7: OpenAPI

**Files:** `api/openapi.yaml` — `createTechnicalUser`, `listTechnicalUsers`, `deleteTechnicalUser`, `resetTechnicalUserSecret` (`grep -n "operationId: .*TechnicalUser" api/openapi.yaml`); `go generate ./api`.

- [ ] **Step 1:** Add a `503` response to all four, copying the shape the key-pair operations use (`grep -n "'503'" api/openapi.yaml | head` and read one). Add `M2M_CLIENT_CAP_REACHED` to `createTechnicalUser`'s `400` description (add a `400` response if it has none). Update each operation's description: clients are stored and shared by the cluster; a store failure answers 500 or 503.
- [ ] **Step 2:** `go generate ./api && go build ./...`; the enforce-mode validator runs in Task 8's e2e.
- [ ] **Step 3: Commit** `docs(openapi): /clients 503 and the cap error`.

---

### Task 8: running-backend e2e (postgres, in-process)

**Files:** Create `internal/e2e/clients_store_test.go`; helpers `(h *callbackHarness).createClient` and `clientCredentialsToken` exist from Part A Task 10.

- [ ] **Step 1: Write the tests**

```go
func twoNodes(t *testing.T) (*callbackHarness, *callbackHarness, *schedDB) {
	t.Helper()
	s, key := newSchedDB(t), genKey(t)
	return newKeyStackOn(t, s, key), newKeyStackOn(t, s, key), s
}

func TestClientsStore_CrossNodeAtOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	a, b, _ := twoNodes(t)
	id, sec := a.createClient(t)
	if code := b.authedStatus(t, b.clientCredentialsToken(t, id, sec)); code != http.StatusOK {
		t.Fatalf("B, right after A's create: %d", code)
	}
	code, body := a.keyCall(t, "PUT", "/clients/"+id+"/secret", "")
	if code != http.StatusOK {
		t.Fatalf("reset on A: %d %s", code, body)
	}
	if _, st, _ := client.FetchClientCredentialsToken(context.Background(), b.baseURL, id, sec); st != http.StatusUnauthorized {
		t.Fatalf("old secret on B: %d, want 401", st)
	}
	var cred struct{ ClientSecret string `json:"client_secret"` }
	_ = json.Unmarshal(body, &cred)
	if code, _ := a.keyCall(t, "DELETE", "/clients/"+id, ""); code != http.StatusOK {
		t.Fatalf("delete on A: %d", code)
	}
	if _, st, _ := client.FetchClientCredentialsToken(context.Background(), b.baseURL, id, cred.ClientSecret); st != http.StatusUnauthorized {
		t.Fatalf("deleted client on B: %d, want 401", st)
	}
}

func TestClientsStore_SurvivesRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	s, key := newSchedDB(t), genKey(t)
	a := newKeyStackOn(t, s, key)
	keep, keepSec := a.createClient(t)
	gone, _ := a.createClient(t)
	_, body := a.keyCall(t, "PUT", "/clients/"+keep+"/secret", "")
	var cred struct{ ClientSecret string `json:"client_secret"` }
	_ = json.Unmarshal(body, &cred)
	_, _ = a.keyCall(t, "DELETE", "/clients/"+gone, "")
	r := newKeyStackOn(t, s, key) // restart
	if _, st, _ := client.FetchClientCredentialsToken(context.Background(), r.baseURL, keep, cred.ClientSecret); st != http.StatusOK {
		t.Fatalf("reset secret after restart: %d", st)
	}
	if _, st, _ := client.FetchClientCredentialsToken(context.Background(), r.baseURL, keep, keepSec); st != http.StatusUnauthorized {
		t.Fatalf("old secret after restart: %d", st)
	}
	if code, _ := r.keyCall(t, "DELETE", "/clients/"+gone, ""); code != http.StatusNotFound {
		t.Fatalf("deleted client after restart: %d, want 404", code)
	}
}
```
Also, each with its own stack:
- `TestClientsStore_Cap`: stack configured with `cfg.IAM.M2MClientMaxPerTenant = 2` → two creates 200, third 400 with code `M2M_CLIENT_CAP_REACHED`, a delete frees a slot.
- `TestClientsStore_OtherTenant404`: create in `test-tenant`; a token for tenant `other-tenant` (sign it like the harness token with that tenant) → `DELETE` and `PUT …/secret` 404 `M2M_CLIENT_NOT_FOUND` with a body equal to that for a random absent id (compare after removing the ticket/instance fields, if any); `GET /clients` for `other-tenant` does not contain the id.
- `TestClientsStore_TokenEndpointRefusesMalformedIDs`: Basic credentials with ids `a%00b`, `%FF`, 101 × `A`, `a%3Ab` → 401 `invalid_client`.
- `TestClientsStore_UndecodableRecord`: raw write through `s.pool`: `INSERT INTO kv_store (tenant_id, namespace, key, value) VALUES ('SYSTEM', 'm2m-clients:test-tenant', 'BADREC1', '{')` and the index entry `('SYSTEM','m2m-client-ids','BADREC1','{"tenantId":"test-tenant"}')` (check the table's columns: `grep -n "CREATE TABLE.*kv_store" -A8 plugins/postgres/migrations/*.sql`) → token 500 `server_error`; `GET /clients` 200 without it; `PUT …/secret` 500; `DELETE` 200 and then 404.
- `TestClientsStore_NoPlaintextSecretStored`: create; `SELECT value FROM kv_store WHERE namespace LIKE 'm2m-%'`; no value contains the secret.
- `TestClientsStore_ClientChangeNotInCallersTransaction`: in a callback joined to a transaction (use the callback harness pattern of `callback_txjoin_test.go`), create a client through the harness HTTP API with the transaction token; before the transaction commits, a second stack authenticates the client → 200.

- [ ] **Step 2:** `go test ./internal/e2e/ -run TestClientsStore` → PASS. Prove each red with the overlay method of Part A Task 10 against the Task 5/6 commits, record in the commit body.
- [ ] **Step 3: Commit** `test(e2e): M2M clients across nodes, restart, cap, isolation, corrupt records`.

---

### Task 9: parity — single-node and multi-node

**Files:**
- Modify: `e2e/parity/client/keys.go` (add `ListClientsRaw`, `DeleteClientRaw`, `ResetClientSecretRaw`, all via `DoJSONBodyRaw`)
- Create: `e2e/parity/m2m_clients.go`; register in `e2e/parity/registry.go`
- Modify: `e2e/parity/fixtureutil/fixtureutil.go` `CyodaEnv` (`:483`): add `"CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT=3"`; check every existing parity scenario creates at most 3 clients per tenant: `grep -rn "CreateClientRaw\|newM2MClient" e2e/parity`
- Create: `e2e/parity/multinode/m2m_clients.go`; register in `e2e/parity/multinode/registry.go`
- Modify: `e2e/parity/multinode/signing_keys.go:56-57` ("M2M clients are per node" goes; `newM2MClient` callers may fetch tokens from any node), `e2e/parity/postgres/signing_keys_cluster_test.go:158`

- [ ] **Step 1: Write the scenarios**

`e2e/parity/m2m_clients.go`:
```go
// RunM2MClientLifecycle: create → token; listed; reset → old secret 401,
// new 200; delete → 401; another tenant sees 404 and an empty list.
func RunM2MClientLifecycle(t *testing.T, fixture BackendFixture) {
	a := fixture.NewTenant(t)
	b := fixture.NewTenant(t)
	ca := client.NewClient(fixture.BaseURL(), a.Token)
	cb := client.NewClient(fixture.BaseURL(), b.Token)
	ctx := context.Background()

	code, body, err := ca.CreateClientRaw(t, false)
	if err != nil || code != http.StatusOK {
		t.Fatalf("create: %d %v", code, err)
	}
	var cred struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	_ = json.Unmarshal(body, &cred)
	if _, st, _ := client.FetchClientCredentialsToken(ctx, fixture.BaseURL(), cred.ID, cred.Secret); st != http.StatusOK {
		t.Fatalf("token: %d", st)
	}
	if code, body, _ := ca.ListClientsRaw(t); code != http.StatusOK || !strings.Contains(string(body), cred.ID) {
		t.Fatalf("list: %d", code)
	}
	if code, body, _ := cb.ListClientsRaw(t); code != http.StatusOK || strings.Contains(string(body), cred.ID) {
		t.Fatalf("other tenant's list: %d", code)
	}
	for name, call := range map[string]func() (int, []byte, error){
		"delete": func() (int, []byte, error) { return cb.DeleteClientRaw(t, cred.ID) },
		"reset":  func() (int, []byte, error) { return cb.ResetClientSecretRaw(t, cred.ID) },
	} {
		if code, body, _ := call(); code != http.StatusNotFound || !containsErrorCode(body, "M2M_CLIENT_NOT_FOUND") {
			t.Fatalf("other tenant %s: %d %s", name, code, body)
		}
	}
	code, body, _ = ca.ResetClientSecretRaw(t, cred.ID)
	if code != http.StatusOK {
		t.Fatalf("reset: %d", code)
	}
	var reset struct{ Secret string `json:"client_secret"` }
	_ = json.Unmarshal(body, &reset)
	if _, st, _ := client.FetchClientCredentialsToken(ctx, fixture.BaseURL(), cred.ID, cred.Secret); st != http.StatusUnauthorized {
		t.Fatalf("old secret: %d", st)
	}
	if _, st, _ := client.FetchClientCredentialsToken(ctx, fixture.BaseURL(), cred.ID, reset.Secret); st != http.StatusOK {
		t.Fatalf("new secret: %d", st)
	}
	if code, _, _ := ca.DeleteClientRaw(t, cred.ID); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	if _, st, _ := client.FetchClientCredentialsToken(ctx, fixture.BaseURL(), cred.ID, reset.Secret); st != http.StatusUnauthorized {
		t.Fatalf("deleted: %d", st)
	}
}

// RunM2MClientCap: the fixture sets CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT=3.
func RunM2MClientCap(t *testing.T, fixture BackendFixture) {
	a := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), a.Token)
	var first string
	for i := 0; i < 3; i++ {
		code, body, _ := c.CreateClientRaw(t, false)
		if code != http.StatusOK {
			t.Fatalf("create %d: %d %s", i, code, body)
		}
		if i == 0 {
			var cred struct{ ID string `json:"client_id"` }
			_ = json.Unmarshal(body, &cred)
			first = cred.ID
		}
	}
	if code, body, _ := c.CreateClientRaw(t, false); code != http.StatusBadRequest || !containsErrorCode(body, "M2M_CLIENT_CAP_REACHED") {
		t.Fatalf("fourth: %d %s", code, body)
	}
	if code, _, _ := c.DeleteClientRaw(t, first); code != http.StatusOK {
		t.Fatal("delete")
	}
	if code, _, _ := c.CreateClientRaw(t, false); code != http.StatusOK {
		t.Fatal("create after delete")
	}
}
```
Register both. Multi-node scenario (shared cluster, not touching the signing key): create on node A; token on node B at once (no poll); reset on A → old secret 401 on B; delete on A → 401 on B.

- [ ] **Step 2:** `go test ./e2e/parity/memory/ ./e2e/parity/sqlite/ ./e2e/parity/postgres/ -run 'M2MClient|MultiNode.*M2M'` → PASS.
- [ ] **Step 3: Commit** `test(parity): M2M client lifecycle, cap, and cross-node visibility`.

---

### Task 10: documentation, parity doc, exit checks

- [ ] **Step 1: `cyoda help`** — `auth/clients.md`: clients are stored and shared by the cluster; a create, reset or delete takes effect on every node when it returns; they survive restarts (not on the memory backend); the cap and `M2M_CLIENT_CAP_REACHED`; the two documented races (concurrent admin changes to one client: the later write wins, a reset racing a delete leaves an unusable listed record that `DELETE` removes; the cap can be exceeded by one client per node); store failures answer 500 or 503. `auth/tokens.md`: `/oauth/token` answers 500 `server_error` on a store failure. `config/auth.md`: `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT`. `errors.md`: list `M2M_CLIENT_CAP_REACHED`. Run `go test ./cmd/cyoda/help/`.
- [ ] **Step 2: Other documents** — `README.md` configuration reference (the new variable); `docs/ARCHITECTURE.md:1857` (component table: `KVM2MClientStore`), §7.2's store text, `:230` (the M2M client table re-checks tenant ids in its decoder); `COMPATIBILITY.md` (the cyoda-go-spi pseudo-pin; the cassandra plugin must include the absent-key `Delete` fix); `docs/cloud-parity/m2m-clients.md` (new: the cap and its error code; clients shared and persistent is not a contract change) and its row in `docs/cloud-parity/README.md`; draft the CaaS ticket text in the PR body for Paul to file (Jira `CP`, `[CaaS]` prefix, component CaaS). `CHANGELOG.md` `[Unreleased]`: `### Breaking` — clients stored and shared by the cluster; `/oauth/token` answers 500 (was 401) on a store failure; `/clients` delete and reset answer 500/503 (was 404) on a store failure; `### Added` — the cap; `### Fixed` — deleting an absent key on the cassandra backend.
- [ ] **Step 3: Code comments** — `internal/auth/replica.go:352`, `e2e/parity/multinode/signing_keys.go:56-57`, `e2e/parity/postgres/signing_keys_cluster_test.go:158`, and every hit of:
```bash
grep -rnE 'InMemoryM2MClientStore|NewInMemoryM2MClientStore|VerifySecret|clientBelongsToTenant|M2M clients are per node|per-node .*M2M' \
  --exclude-dir=superpowers --exclude-dir=audits --exclude-dir=.git --exclude=CHANGELOG.md .
grep -nE 'Delete\(.*ErrNotFound|errors\.Is\(err, spi\.ErrNotFound\)' internal/auth/replica.go   # only the Get at :317 remains
```
Expected: the first returns nothing; the second only the `Get` case.
- [ ] **Step 4: Fresh-context documentation review** — as Part A Task 11 Step 5, over this PR's diff and §6.2.
- [ ] **Step 5: Commit** `docs: M2M clients shared by the cluster; the cap`.

---

### Task 11: verification, reviews, PR

- [ ] `make preflight && make test-full` green (root + every plugin submodule + E2E); `go vet ./...`; `GOWORK=off go build ./...` (the pseudo-pins resolve as a consumer would); `make race` once.
- [ ] `superpowers:requesting-code-review` with a fresh-context reviewer; fix findings.
- [ ] Security review: fresh-context subagent with `antigravity-bundle-security-engineer:security-auditor` over the diff (tenant isolation on every store path, timing uniformity, no secret or hash logged, 4xx/5xx shapes, transaction stripping); fix findings.
- [ ] Push, `make repin-plugins` if any plugin changed (push → repin → new commit → push), open the PR against `release/v0.9.0` once Part A is merged (or stacked on Part A's branch until then, retargeting before Part A's branch is deleted). Body: summary, Breaking list, test evidence, the SPI PR and cassandra PR links, the CaaS ticket draft. `Closes #286`.
