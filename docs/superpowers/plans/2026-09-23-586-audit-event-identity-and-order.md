# Audit events: identity and a fixed order — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every audit event carries an identity (`version` / `eventId`), the audit endpoint returns one total order and pages by position, the store owns the state machine event id, and a failed state machine audit read fails the request.

**Architecture:** SPI first (contract + two conformance cases, PR into `cyoda-go-spi` main), then the three in-tree plugins (store-assigned event id, sqlite v1 generator, memory version numbering), then the audit handler (fields, comparator, cursor, fail-closed), then e2e + parity + docs. During development the SPI is composed locally through an uncommitted `go.work` `use` line; the pin bump is one commit at the end.

**Tech Stack:** Go 1.26, `github.com/google/uuid`, oapi-codegen (`go generate ./api`), testcontainers (e2e), `spitest` conformance.

**Spec:** `docs/superpowers/specs/2026-09-23-586-audit-event-identity-and-order-design.md`

## Global Constraints

- New wire fields: `EntityChangeAuditEventDto.version` (`integer`, `int64`, required) and `StateMachineAuditEventDto.eventId` (`string`, `format: uuid`, required).
- Order: `utcTime` DESC → `auditEventType` ASC (`EntityChange` < `StateMachine`) → EC `version` DESC / SM `eventId` time DESC then 16 bytes DESC.
- Cursor: opaque URL-safe base64 (no padding) of JSON `{"v":1,"t":<RFC3339Nano UTC>,"k":<auditEventType>,"n":<version>}` or `{... "e":<uuid>}`; anything that does not decode or validate → `400 BAD_REQUEST`.
- Failures: through `common.Internal` (503 `STORAGE_UNAVAILABLE` retryable for an outage, 500 otherwise, generic message + ticket). Never log credentials.
- No new error code (no `errors/<CODE>.md`).
- No issue IDs (`#586`) in shipped artefacts: code comments, errors, logs, OpenAPI, help. Commit messages / PR body / spec only.
- `go.work` is tracked: stage files explicitly, never `git add -A` / `git add .`.
- Do not add `-count=1`; use `make test` for iteration and `make test-full` at the end.
- Commit trailer: `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.

## Review Focus

- A state machine event whose stored id is empty or not a UUID (a store contract violation) → the request fails 500, never an event with a blank `eventId` (Task 7 test `TestSearch_SMEventWithoutID_Returns500`).
- A cursor built from one filter set and replayed with another (e.g. page 1 unfiltered, page 2 `eventType=StateMachine`) → continues from the same position, no 400, no restart (Task 9 test `TestCursor_SurvivesFilterChange`).
- A cursor whose event was deleted between pages (tombstone rows remain, but a whole filtered subset may be gone) → positions correctly, `hasNext` false when nothing follows (Task 9 test `TestCursor_PositionOfMissingEvent`).
- `limit` above 1000 still clamps and still produces a valid position cursor (Task 9 test `TestCursor_ClampedLimitStillPages`).
- Two state machine events whose ids share the 100 ns time field (test generators, clock-sequence collisions) → still a deterministic, total order (Task 8 test `TestCompare_SMSameTimeFieldBreaksOnBytes`).

---

## Stream A — SPI (repo `cyoda-go-spi`)

### Task 1: SPI contract and two conformance cases

**Files (in a new worktree of `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`, branch `feat/audit-event-id` off `origin/main` = `7a75d2d`):**
- Modify: `persistence.go` (doc on `StateMachineAuditStore`)
- Modify: `types.go:425-435` (doc on `StateMachineEvent.TimeUUID`)
- Modify: `spitest/audit.go` (case `EventID`)
- Modify: `spitest/entity.go` (case `GetVersionMetadata/RecreateAfterDelete`)

**Interfaces:**
- Produces: conformance subtests `Audit/EventID` and `Entity/GetVersionMetadata/RecreateAfterDelete` (skip keys carry the suite prefix).

- [ ] **Step 1: Create the SPI worktree**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git fetch -q origin
git worktree add ../cyoda-go-spi-audit-event-id -b feat/audit-event-id origin/main
```

- [ ] **Step 2: Doc comments**

In `persistence.go`, directly above `type StateMachineAuditStore interface`:

```go
// StateMachineAuditStore records and reads an entity's state machine events.
//
// The event id is the store's: Record assigns a new time-based UUID
// (version 1) to every event and ignores any TimeUUID the caller set.
// GetEvents and GetEventsByTransaction return that id in TimeUUID on every
// event, and the same value on every read. A store that cannot assign an id
// fails Record with an error rather than recording the event without one.
```

In `types.go`, on the `TimeUUID` field:

```go
	// TimeUUID is the event's identity, assigned by the store on Record (a
	// caller's value is ignored) and returned on every read. See
	// StateMachineAuditStore.
	TimeUUID string `json:"timeUuid"`
```

- [ ] **Step 3: Conformance case `Audit/EventID`**

Register in `runAuditSuite` after `RecordAndGet`:
`runSubtest(t, h, tracker, "EventID", testAuditEventID)`

```go
// testAuditEventID pins that the event id is the store's: every recorded
// event comes back with a non-empty, distinct UUID, a caller's value is
// ignored, and the ids are the same on every read and through both reads.
func testAuditEventID(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	as, err := h.Factory.StateMachineAuditStore(ctx)
	require.NoError(t, err)

	const callerID = "caller-chosen-id"
	withCaller := newSMEvent("tx1", "C", "B->C")
	withCaller.TimeUUID = callerID
	require.NoError(t, as.Record(ctx, "e1", newSMEvent("tx1", "B", "A->B")))
	require.NoError(t, as.Record(ctx, "e1", withCaller))
	require.NoError(t, as.Record(ctx, "e1", newSMEvent("tx2", "D", "C->D")))

	first, err := as.GetEvents(ctx, "e1")
	require.NoError(t, err)
	require.Len(t, first, 3)
	seen := map[string]bool{}
	byState := map[string]string{}
	for _, ev := range first {
		require.NotEmpty(t, ev.TimeUUID, "event %q has no id", ev.State)
		_, perr := uuid.Parse(ev.TimeUUID)
		require.NoError(t, perr, "event %q id %q is not a UUID", ev.State, ev.TimeUUID)
		require.NotEqual(t, callerID, ev.TimeUUID, "the caller's id must be ignored")
		require.False(t, seen[ev.TimeUUID], "id %q returned twice", ev.TimeUUID)
		seen[ev.TimeUUID] = true
		byState[ev.State] = ev.TimeUUID
	}

	second, err := as.GetEvents(ctx, "e1")
	require.NoError(t, err)
	require.Len(t, second, 3)
	for _, ev := range second {
		require.Equal(t, byState[ev.State], ev.TimeUUID, "event %q id changed between reads", ev.State)
	}

	byTx, err := as.GetEventsByTransaction(ctx, "e1", "tx1")
	require.NoError(t, err)
	require.Len(t, byTx, 2)
	for _, ev := range byTx {
		require.Equal(t, byState[ev.State], ev.TimeUUID, "event %q id differs between GetEvents and GetEventsByTransaction", ev.State)
	}
}
```

Add `"github.com/google/uuid"` to the imports of `spitest/audit.go` (already a dependency of spitest via `helpers.go`).

- [ ] **Step 4: Conformance case `GetVersionMetadata/RecreateAfterDelete`**

Register after `GetVersionMetadata/Ordering`:
`runSubtest(t, h, tracker, "GetVersionMetadata/RecreateAfterDelete", testEntityVersionMetadataRecreateAfterDelete)`

```go
// testEntityVersionMetadataRecreateAfterDelete pins that a version number is
// never reused: a save after a committed delete takes the next number after
// the tombstone's, so (entity id, version) names exactly one history row.
func testEntityVersionMetadataRecreateAfterDelete(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	id := newID()
	withTx(t, h, ctx, func(txCtx context.Context) {
		es, _ := h.Factory.EntityStore(txCtx)
		_, err := es.Save(txCtx, newEntity(t, "m-recreate", id, map[string]any{"v": 1}))
		require.NoError(t, err)
	})
	h.AdvanceClock(1 * time.Millisecond)
	withTx(t, h, ctx, func(txCtx context.Context) {
		es, _ := h.Factory.EntityStore(txCtx)
		require.NoError(t, es.Delete(txCtx, id))
	})
	h.AdvanceClock(1 * time.Millisecond)
	withTx(t, h, ctx, func(txCtx context.Context) {
		es, _ := h.Factory.EntityStore(txCtx)
		_, err := es.Save(txCtx, newEntity(t, "m-recreate", id, map[string]any{"v": 2}))
		require.NoError(t, err)
	})

	es, _ := h.Factory.EntityStore(ctx)
	metas, err := es.GetVersionMetadata(ctx, id, spi.VersionMetadataOptions{})
	require.NoError(t, err)
	require.Len(t, metas, 3, "create + tombstone + recreate")
	for i := 1; i < len(metas); i++ {
		require.Greater(t, metas[i-1].Version, metas[i].Version,
			"versions must strictly decrease newest first; got %d then %d", metas[i-1].Version, metas[i].Version)
	}
	require.True(t, metas[1].Deleted, "the middle row must be the tombstone")
}
```

- [ ] **Step 5: Build and vet**

Run: `cd ../cyoda-go-spi-audit-event-id && go build ./... && go vet ./...`
Expected: no output. (`spitest` has no harness in this repo; the cases first execute in cyoda-go — Task 3/4 prove RED there.)

- [ ] **Step 6: Commit and push**

```bash
git add persistence.go types.go spitest/audit.go spitest/entity.go
git commit -m "feat(audit): the state machine event id is the store's; versions are never reused

Record assigns the event id and ignores a caller's value; reads return it.
Two conformance cases: Audit/EventID and
Entity/GetVersionMetadata/RecreateAfterDelete.

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
git push -u origin feat/audit-event-id
```

- [ ] **Step 7: Compose locally in cyoda-go (uncommitted)**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-586-audit-discriminator
go work edit -use /Users/paul/go-projects/cyoda-light/cyoda-go-spi-audit-event-id
```

This `go.work` line stays uncommitted for the whole branch.

The SPI PR is opened in Task 14 once cyoda-go proves the cases green on all three backends.

---

## Stream B — plugins (cyoda-go)

### Task 2: sqlite generates time-based UUIDs

**Files:**
- Modify: `plugins/sqlite/uuid.go`
- Test: `plugins/sqlite/uuid_test.go` (create)

- [ ] **Step 1: Failing test**

```go
package sqlite

import (
	"testing"

	"github.com/google/uuid"
)

// The SPI asks a UUIDGenerator for time-ordered (version 1) ids; the audit
// event id and the transaction id both rely on it.
func TestDefaultUUIDGenerator_IsVersion1(t *testing.T) {
	id := uuid.UUID((&defaultUUIDGenerator{}).NewTimeUUID())
	if id.Version() != 1 {
		t.Fatalf("NewTimeUUID returned version %d, want 1", id.Version())
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`version 4, want 1`)

Run: `cd plugins/sqlite && go test -run TestDefaultUUIDGenerator_IsVersion1 .`

- [ ] **Step 3: Implement**

```go
// defaultUUIDGenerator produces time-based (version 1) UUIDs, as the SPI's
// UUIDGenerator contract asks. Used by NewFactory for the transaction
// manager and the state machine audit store.
type defaultUUIDGenerator struct{}

func (g *defaultUUIDGenerator) NewTimeUUID() [16]byte {
	// uuid.NewUUID reads the clock and the node id; with the node id cached
	// after the first call it cannot fail, and the interface has no error.
	id, _ := uuid.NewUUID()
	return [16]byte(id)
}
```

- [ ] **Step 4: Run — expect PASS**, then `go test ./...` inside `plugins/sqlite`.

- [ ] **Step 5: Commit**

```bash
git add plugins/sqlite/uuid.go plugins/sqlite/uuid_test.go
git commit -m "fix(sqlite): the UUID generator returns time-based ids, as the SPI asks"
```

### Task 3: The store assigns the state machine event id (memory, sqlite, postgres)

**Files:**
- Modify: `plugins/memory/store_factory.go` (field `uuids spi.UUIDGenerator`, set in `initTransactionManager` and `NewTransactionManager`), `plugins/memory/txmanager.go:183-190`, `plugins/memory/sm_audit_store.go`
- Modify: `plugins/sqlite/store_factory.go:388-394,447-449`, `plugins/sqlite/sm_audit_store.go`
- Modify: `plugins/postgres/store_factory.go:241-247,300-311`, `plugins/postgres/sm_audit_store.go`
- Modify tests: `plugins/postgres/sm_audit_store_test.go:30-40,71-75,131-132,198`, `plugins/memory/sm_audit_store_test.go:19-21`, `plugins/memory/sm_audit_stamp_test.go:55-105`, `plugins/memory/sm_audit_txindex_prune_test.go:59,119`, `plugins/postgres/commit_instant_test.go:349`
- Test: `plugins/memory/sm_audit_event_id_test.go`, `plugins/sqlite/sm_audit_event_id_test.go`, `plugins/postgres/sm_audit_event_id_test.go` (create)

**Interfaces:**
- Consumes: SPI `Audit/EventID` (Task 1).
- Produces: every `spi.StateMachineEvent` returned by the three backends has a non-empty v1 `TimeUUID`, stable across reads.

- [ ] **Step 1: RED via conformance**

Run: `make test` (with the Task 1 `go.work` line). Expected: `Audit/EventID` FAILS on memory (empty id), sqlite and postgres (empty id / caller's id returned). Record the failure lines.

- [ ] **Step 2: Per-backend unit test (one per plugin, same shape)** — memory version:

```go
package memory_test

import (
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func TestSMAudit_StoreAssignsEventID(t *testing.T) {
	f := memory.NewStoreFactory()
	ctx := tenantCtx(spi.TenantID("tenant-evid"))
	as, err := f.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := as.Record(ctx, "e-1", spi.StateMachineEvent{EventType: spi.SMEventStarted, EntityID: "e-1", TimeUUID: "caller"}); err != nil {
		t.Fatal(err)
	}
	evs, err := as.GetEvents(ctx, "e-1")
	if err != nil || len(evs) != 1 {
		t.Fatalf("GetEvents = %v, %v", evs, err)
	}
	id, perr := uuid.Parse(evs[0].TimeUUID)
	if perr != nil || id.Version() != 1 {
		t.Fatalf("event id %q: want a version-1 UUID assigned by the store", evs[0].TimeUUID)
	}
}
```

sqlite/postgres versions use each package's existing factory helper (`newTestFactory(t)` / the postgres test-container helper already used by `sm_audit_store_test.go`) and additionally assert the id read back equals the `event_id` column:

```go
	var col string
	// postgres: $1/$2; sqlite: ?/?
	if err := db.QueryRow(`SELECT event_id FROM sm_audit_events WHERE tenant_id = $1 AND entity_id = $2`, tenant, "e-1").Scan(&col); err != nil {
		t.Fatal(err)
	}
	if col != evs[0].TimeUUID {
		t.Fatalf("read id %q != stored event_id %q", evs[0].TimeUUID, col)
	}
```

Run each; expect FAIL.

- [ ] **Step 3: Implement — factories keep the generator**

memory (`store_factory.go`): add field `uuids spi.UUIDGenerator` to `StoreFactory`; in `NewTransactionManager(uuids)` (txmanager.go:183) set `f.uuids = uuids` before building the manager. `StateMachineAuditStore` passes nothing new (the store reads `s.factory.uuids`).

sqlite (`store_factory.go:447`):

```go
func (f *StoreFactory) initTransactionManager(uuids spi.UUIDGenerator) {
	f.uuids = uuids
	f.tm = newTransactionManager(f, uuids)
}
```

and `return &smAuditStore{db: f.db, tenantID: tid, uuids: f.uuids}, nil`.

postgres (`store_factory.go:300`): set `f.uuids = uuids` in `InitTransactionManager`; `return &smAuditStore{q: f.querier(), tenantID: tid, uuids: f.uuids}, nil`.

- [ ] **Step 4: Implement — Record assigns, reads return the column**

Shared rule, each backend:

```go
// Record assigns the event its id (see spi.StateMachineAuditStore): a
// caller's TimeUUID is ignored.
func (s *smAuditStore) Record(ctx context.Context, entityID string, event spi.StateMachineEvent) error {
	if s.uuids == nil {
		return fmt.Errorf("failed to record state machine event for entity %s: no id generator configured", entityID)
	}
	event.TimeUUID = uuid.UUID(s.uuids.NewTimeUUID()).String()
	doc, err := json.Marshal(event)
	...INSERT with eventID := event.TimeUUID...
}
```

postgres/sqlite reads: `SELECT event_id, doc, timestamp` (sqlite: `event_id, json(doc), timestamp`), scan into `eventID`, then `e.TimeUUID = eventID` next to the existing timestamp override; extend the scan-function doc comment with one sentence: "The event_id column likewise overrides the document's TimeUUID: it is the key the row was stored under."

memory `Record`: same guard; `event.TimeUUID = uuid.UUID(s.factory.uuids.NewTimeUUID()).String()` before `copyEvent`.

Delete the now-dead `if eventID == ""` fallback blocks in postgres and sqlite.

- [ ] **Step 5: Rewrite tests that assumed the caller's id**

- `plugins/postgres/sm_audit_store_test.go:71-75`: assert order by `Details` (`"uuid-1"` → the event recorded first) instead of `TimeUUID`; `:131-132` likewise; `:198` remove the caller id.
- `plugins/memory/sm_audit_store_test.go:19-21`, `sm_audit_txindex_prune_test.go:59,119`, `postgres/commit_instant_test.go:349`: drop `TimeUUID:` literals.
- `plugins/memory/sm_audit_stamp_test.go`: the test labels events via `TimeUUID: "u-"+id`; move the label to `Details` and read `ev.Details` in the messages at `:99,:105`.

- [ ] **Step 6: Run — expect PASS**

Run each plugin's unit tests (`cd plugins/<p> && go test ./...`) then `make test`. Expected: `Audit/EventID` passes on memory, sqlite, postgres.

- [ ] **Step 7: Commit**

```bash
git add plugins/memory plugins/sqlite plugins/postgres   # review `git status` first: only these paths
git commit -m "fix(plugins): the state machine event id is assigned by the store and returned on every read"
```

### Task 4: memory never reuses a version number

**Files:**
- Modify: `plugins/memory/entity_store.go:433-440,677,776`, `plugins/memory/txmanager.go:631-637,756-761`
- Test: `plugins/memory/version_recreate_test.go` (create)

- [ ] **Step 1: RED** — `make test` shows `Entity/GetVersionMetadata/RecreateAfterDelete` FAIL on memory only. Plus unit test:

```go
package memory_test

func TestSave_AfterCommittedDelete_TakesNextVersion(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx(spi.TenantID("tenant-recreate"))
	ref := spi.ModelRef{EntityName: "m-recreate", ModelVersion: "1"}
	es, _ := f.EntityStore(ctx)
	commit := func(fn func(txCtx context.Context)) {
		txID, txCtx, err := tm.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		fn(txCtx)
		if err := tm.Commit(txCtx, txID); err != nil {
			t.Fatal(err)
		}
	}
	commit(func(c context.Context) { _, _ = es.Save(c, &spi.Entity{Meta: spi.EntityMeta{ID: "e-r", ModelRef: ref}, Data: []byte(`{"n":1}`)}) })
	commit(func(c context.Context) { _ = es.Delete(c, "e-r") })
	commit(func(c context.Context) { _, _ = es.Save(c, &spi.Entity{Meta: spi.EntityMeta{ID: "e-r", ModelRef: ref}, Data: []byte(`{"n":2}`)}) })
	metas, err := es.GetVersionMetadata(ctx, "e-r", spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := []int64{}
	for _, m := range metas {
		got = append(got, m.Version)
	}
	if len(got) != 3 || !(got[0] > got[1] && got[1] > got[2]) {
		t.Fatalf("versions newest first = %v, want three strictly decreasing", got)
	}
}
```

(Uses the package's existing `newTxManager` / `tenantCtx` helpers; check their names in `txmanager_test.go` and adjust if they differ.)

- [ ] **Step 2: Implement** — every row, tombstones included, already carries its number in `entityVersion.version` (`entity_store.go:26-31`). Add one helper in `entity_store.go`:

```go
// lastVersion is the number of the entity's newest row of any kind,
// tombstones included, or 0 when it has none. A new row takes
// lastVersion+1, so a number is never reused after a delete.
func lastVersion(versions []entityVersion) int64 {
	if len(versions) == 0 {
		return 0
	}
	return versions[len(versions)-1].version
}
```

Replace the three tombstone-skipping loops with it:
- `entity_store.go:433-440` → `nextVersion := lastVersion(versions) + 1`
- `txmanager.go:631-637` → `baseVersion := lastVersion(versions)`
- `txmanager.go:756-761` → `nextVersion := lastVersion(versions) + 1`

and, for one rule everywhere, the two non-transactional delete sites `entity_store.go:677` and `:776` (`latest.entity.Meta.Version + 1`) → `lastVersion(versions) + 1` (same value there, since `latest` is guaranteed live).

- [ ] **Step 3: Run** unit test + `make test` — PASS, including `RecreateAfterDelete` on memory.

- [ ] **Step 4: Commit** `fix(memory): a save after a delete takes the next version number, never the tombstone's`

### Task 5: The engine stops setting the event id

**Files:**
- Modify: `internal/domain/workflow/engine.go:1260-1270`, `internal/domain/workflow/transition_aborted.go:57,76-100,150`, `internal/domain/entity/service.go:2909`

TDD waiver (record in the commit body): deletion of code whose value every store now ignores; behaviour is pinned by `Audit/EventID` (Task 3) and the handler tests (Task 7).

- [ ] **Step 1:** Remove `TimeUUID:` from both `spi.StateMachineEvent{...}` literals. If `uuids` is then unused in `EmitTransitionAborted`, remove the parameter and its doc line (`transition_aborted.go:57`), and update both callers (`transition_aborted.go:150`, `service.go:2909`). Leave `Engine.uuids` if other code uses it (`engine.go:1220` does).
- [ ] **Step 2:** `go build ./... && go test ./internal/domain/workflow/... ./internal/domain/entity/...` — PASS.
- [ ] **Step 3: Commit** `refactor(workflow): the engine no longer proposes a state machine event id`

---

## Stream C — handler and contract (cyoda-go)

### Task 6: Contract — OpenAPI fields and the parity client

**Files:**
- Modify: `api/openapi.yaml` (`EntityChangeAuditEventDto`, `StateMachineAuditEventDto`, `cursor` parameter ~line 430, `CursorPaginationInfoDto.nextCursor` ~line 11622); regenerate `api/generated.go`
- Modify: `e2e/parity/client/audit.go` (typed + flat structs for both subtypes)
- Modify: `e2e/parity/client/testdata/audit_entity_change_event.json`, `audit_state_machine_event.json`, `audit_events_response.json`
- Test: the existing client decode tests in `e2e/parity/client` (find with `grep -ln testdata e2e/parity/client/*_test.go`)

- [ ] **Step 1: Failing test** — add `"version": 2` to the EC fixture and `"eventId": "5f1c1b0e-6d1a-11f1-8000-000000000001"` to the SM fixture (and the matching items in `audit_events_response.json`). Add assertions to the existing fixture test:

```go
	if ec.Version != 2 {
		t.Errorf("Version = %d, want 2", ec.Version)
	}
	if sm.EventID != "5f1c1b0e-6d1a-11f1-8000-000000000001" {
		t.Errorf("EventID = %q", sm.EventID)
	}
```

Run `go test ./e2e/parity/client/...` — FAIL (compile error, then unknown field under `DisallowUnknownFields`).

- [ ] **Step 2: Implement** — `EntityChangeAuditEvent.Version int64 \`json:"version"\`` and in `entityChangeAuditEventFlat`; `StateMachineAuditEvent.EventID string \`json:"eventId"\`` and in `stateMachineAuditEventFlat`; copy them in each `UnmarshalJSON`.

OpenAPI, in the EC subtype `properties`:

```yaml
            version:
              type: integer
              format: int64
              description: >
                The entity's version number for this change. Strictly
                increasing over the entity's history, across delete and
                recreate; (entityId, version) identifies the event.
```

and add `- version` to that subtype's `required`. SM subtype:

```yaml
            eventId:
              type: string
              format: uuid
              description: >
                The event's identity: a time-based UUID the server assigned
                when it recorded the event. The same on every read.
```

and add `- eventId` to its `required`. Cursor parameter description:

```yaml
          description: "Position to continue from: pass `nextCursor` from the
            previous response. Opaque. A cursor that cannot be read is
            rejected with 400. Omit for the first page."
```

`nextCursor` description: `"Opaque position of the last event on this page. Absent when hasNext is false."`

Run `go generate ./api`, then `make check-codegen`.

- [ ] **Step 3: Run** `go test ./e2e/parity/client/... ./api/...` — PASS.
- [ ] **Step 4: Commit** `feat(api): audit events declare version and eventId`

### Task 7: Handler emits the fields; one state machine event builder

**Files:**
- Create: `internal/domain/audit/events.go` (builders + key type)
- Modify: `internal/domain/audit/handler.go`
- Test: `internal/domain/audit/handler_fields_test.go` (create, package `audit_test`)

**Interfaces:**
- Produces (package `audit`, unexported):

```go
// auditItem is one event of the merged list: its sort key and its wire body.
type auditItem struct {
	key  eventKey
	body map[string]any
}

type eventKey struct {
	at      time.Time // utcTime at stored precision
	kind    string    // "EntityChange" | "StateMachine"
	version int64     // EntityChange only
	eventID uuid.UUID // StateMachine only
}

func entityChangeItem(v spi.EntityVersionMeta, entityID, callerTenant string) auditItem
func stateMachineItem(ev spi.StateMachineEvent) (auditItem, error) // error when ev.TimeUUID is not a UUID
```

- [ ] **Step 1: Failing tests** (real memory stack via `newTestServer`, existing helpers in `handler_test.go`):

```go
func TestAudit_EntityChangeCarriesVersion(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "AuditVer", 1, `{"name":"Alice"}`)
	id := createEntityAndGetID(t, srv.URL, "AuditVer", 1, `{"name":"Bob"}`)
	updateEntity(t, srv.URL, id, `{"name":"Carol"}`)
	events, _ := getAuditEvents(t, srv.URL, id, "eventType=EntityChange")
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0]["version"] != float64(2) || events[1]["version"] != float64(1) {
		t.Fatalf("versions = %v, %v; want 2, 1", events[0]["version"], events[1]["version"])
	}
}

func TestAudit_StateMachineEventCarriesEventID(t *testing.T) {
	// reuse TestAuditWithStateMachineEvents' model + workflow setup
	...
	first, _ := getAuditEvents(t, srv.URL, id, "eventType=StateMachine")
	second, _ := getAuditEvents(t, srv.URL, id, "eventType=StateMachine")
	seen := map[string]bool{}
	for i, ev := range first {
		s, _ := ev["eventId"].(string)
		if _, err := uuid.Parse(s); err != nil {
			t.Fatalf("event %d eventId %q is not a UUID", i, s)
		}
		if seen[s] {
			t.Fatalf("eventId %q repeated", s)
		}
		seen[s] = true
		if second[i]["eventId"] != s {
			t.Fatalf("event %d eventId changed between reads", i)
		}
	}
}

func TestGetStateMachineFinishedEvent_CarriesEventID(t *testing.T) {
	// extend TestGetStateMachineFinishedEvent_Found's setup; assert the
	// finished event's eventId equals the STATE_MACHINE_FINISH event's
	// eventId in the search for the same transaction.
}

func TestSearch_SMEventWithoutID_Returns500(t *testing.T) {
	// stub factory (pattern: handler_outage_test.go) whose EntityStore
	// returns one version and whose SM store returns one event with
	// TimeUUID "" → status 500, problem body carries a ticket, no internals.
}
```

Run: `go test ./internal/domain/audit/...` — FAIL.

- [ ] **Step 2: Implement** `events.go`:

```go
func entityChangeItem(v spi.EntityVersionMeta, entityID, callerTenant string) auditItem {
	at := v.Timestamp.UTC()
	body := map[string]any{
		"auditEventType": "EntityChange",
		"changeType":     common.CanonicalChangeType(v.ChangeType),
		"severity":       "INFO",
		"utcTime":        at.Format(time.RFC3339Nano),
		"microsTime":     at.UnixMicro(),
		"system":         false,
		"entityId":       entityID,
		"version":        v.Version,
	}
	if v.TransactionID != "" {
		body["transactionId"] = v.TransactionID
	}
	if v.User != "" {
		actor := map[string]any{"id": v.User, "name": v.User}
		if callerTenant != "" {
			actor["legalId"] = callerTenant
		}
		body["actor"] = actor
	}
	return auditItem{key: eventKey{at: at, kind: "EntityChange", version: v.Version}, body: body}
}

// stateMachineItem builds a state machine event. The store assigns every
// event an id (spi.StateMachineAuditStore); one that does not parse is a
// store fault, reported rather than emitted with a blank identity.
func stateMachineItem(ev spi.StateMachineEvent) (auditItem, error) {
	id, err := uuid.Parse(ev.TimeUUID)
	if err != nil {
		return auditItem{}, fmt.Errorf("state machine event of entity %s has no valid id: %w", ev.EntityID, err)
	}
	at := ev.Timestamp.UTC()
	body := map[string]any{
		"auditEventType": "StateMachine",
		"eventType":      string(ev.EventType),
		"severity":       "INFO",
		"utcTime":        at.Format(time.RFC3339Nano),
		"microsTime":     at.UnixMicro(),
		"entityId":       ev.EntityID,
		"eventId":        id.String(),
		"details":        ev.Details,
		"data":           ev.Data,
	}
	if ev.TransactionID != "" {
		body["transactionId"] = ev.TransactionID
	}
	if ev.State != "" {
		body["state"] = ev.State
	}
	return auditItem{key: eventKey{at: at, kind: "StateMachine", eventID: id}, body: body}, nil
}
```

In `handler.go` build `[]auditItem`; filters read `item.key.at` instead of re-parsing `utcTime` strings; response writes `item.body`. `GetStateMachineFinishedEvent` uses `stateMachineItem` (error → `common.Internal("invalid state machine event", err)`). Keep the current `sort.Slice` for now (Task 8 replaces it).

- [ ] **Step 3: Run** — PASS, and the whole `internal/domain/audit` package green.
- [ ] **Step 4: Commit** `feat(audit): entity change events carry version, state machine events carry eventId`

### Task 8: One total order

**Files:**
- Modify: `internal/domain/audit/events.go` (add `compareKeys`)
- Modify: `internal/domain/audit/handler.go` (sort)
- Test: `internal/domain/audit/order_internal_test.go` (create, package `audit`); extend `handler_fields_test.go`

- [ ] **Step 1: Failing tests**

```go
package audit

func key(at time.Time, kind string, n int64, id string) eventKey {
	k := eventKey{at: at, kind: kind, version: n}
	if id != "" {
		k.eventID = uuid.MustParse(id)
	}
	return k
}

func TestCompare_TimeDescFirst(t *testing.T) {
	t0 := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	if compareKeys(key(t0.Add(time.Nanosecond), "StateMachine", 0, "00000000-0000-1000-8000-000000000001"),
		key(t0, "EntityChange", 9, "")) >= 0 {
		t.Fatal("newer event must sort first regardless of kind")
	}
}

func TestCompare_EntityChangeBeforeStateMachineAtOneInstant(t *testing.T) {
	t0 := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	if compareKeys(key(t0, "EntityChange", 1, ""), key(t0, "StateMachine", 0, "00000000-0000-1000-8000-000000000001")) >= 0 {
		t.Fatal("EntityChange must precede StateMachine at one instant")
	}
}

func TestCompare_VersionDesc(t *testing.T) {
	t0 := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	if compareKeys(key(t0, "EntityChange", 3, ""), key(t0, "EntityChange", 2, "")) >= 0 {
		t.Fatal("higher version first")
	}
}

func TestCompare_SMEventIDTimeDesc(t *testing.T) {
	t0 := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	older, _ := uuid.NewUUID()
	newer, _ := uuid.NewUUID()
	for newer.Time() == older.Time() {
		newer, _ = uuid.NewUUID()
	}
	if compareKeys(eventKey{at: t0, kind: "StateMachine", eventID: newer}, eventKey{at: t0, kind: "StateMachine", eventID: older}) >= 0 {
		t.Fatal("later-recorded id first")
	}
}

func TestCompare_SMSameTimeFieldBreaksOnBytes(t *testing.T) {
	t0 := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	a := uuid.MustParse("00000000-0000-1000-8000-000000000002")
	b := uuid.MustParse("00000000-0000-1000-8000-000000000001") // same time field, lower bytes
	ka, kb := eventKey{at: t0, kind: "StateMachine", eventID: a}, eventKey{at: t0, kind: "StateMachine", eventID: b}
	if compareKeys(ka, kb) >= 0 || compareKeys(kb, ka) <= 0 || compareKeys(ka, ka) != 0 {
		t.Fatal("same time field must still give a strict, antisymmetric order on the bytes")
	}
}
```

Handler-level test (memory stack): an entity with a workflow → `GET` twice with no filter; the two item lists are identical in order, and for every adjacent pair `compare(prev, next) < 0` using the emitted `utcTime`, `auditEventType`, `version`, `eventId`.

Run — FAIL (`compareKeys` undefined).

- [ ] **Step 2: Implement**

```go
// compareKeys orders audit events newest first and totally: by instant, then
// EntityChange before StateMachine, then version (entity changes) or the id's
// time field and bytes (state machine events), each descending. The order is
// deterministic; for state machine events of one instant it is not promised
// to be the recording order.
func compareKeys(a, b eventKey) int {
	if c := b.at.Compare(a.at); c != 0 {
		return c
	}
	if a.kind != b.kind {
		return strings.Compare(a.kind, b.kind)
	}
	if a.kind == "EntityChange" {
		return cmp.Compare(b.version, a.version)
	}
	if c := cmp.Compare(b.eventID.Time(), a.eventID.Time()); c != 0 {
		return c
	}
	return bytes.Compare(b.eventID[:], a.eventID[:])
}
```

Handler: `slices.SortFunc(items, func(x, y auditItem) int { return compareKeys(x.key, y.key) })`.

- [ ] **Step 3: Run** — PASS; whole package green.
- [ ] **Step 4: Commit** `fix(audit): events of one instant come back in one fixed order`

### Task 9: The cursor is a position

**Files:**
- Create: `internal/domain/audit/cursor.go`
- Modify: `internal/domain/audit/handler.go` (paging block ~lines 202-241)
- Test: `internal/domain/audit/cursor_internal_test.go` (package `audit`), extend `handler_test.go` (`TestAuditPagination` stays and must pass)

**Interfaces:**
- Produces:

```go
func encodeCursor(k eventKey) string
func decodeCursor(s string) (eventKey, error)
```

- [ ] **Step 1: Failing tests**

```go
func TestCursor_RoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 23, 10, 0, 0, 123456789, time.UTC)
	for _, k := range []eventKey{
		{at: at, kind: "EntityChange", version: 7},
		{at: at, kind: "StateMachine", eventID: uuid.MustParse("5f1c1b0e-6d1a-11f1-8000-000000000001")},
	} {
		got, err := decodeCursor(encodeCursor(k))
		if err != nil || compareKeys(got, k) != 0 || !got.at.Equal(k.at) {
			t.Fatalf("round trip %+v → %+v, %v", k, got, err)
		}
	}
}

func TestCursor_Rejects(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, c := range map[string]string{
		"old offset":        "20",
		"not base64":        "!!!",
		"not json":          enc("nope"),
		"wrong version":     enc(`{"v":2,"t":"2026-09-23T10:00:00Z","k":"EntityChange","n":1}`),
		"bad time":          enc(`{"v":1,"t":"yesterday","k":"EntityChange","n":1}`),
		"unknown kind":      enc(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"System","n":1}`),
		"version zero":      enc(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"EntityChange","n":0}`),
		"ec with eventId":   enc(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"EntityChange","n":1,"e":"5f1c1b0e-6d1a-11f1-8000-000000000001"}`),
		"sm bad eventId":    enc(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"StateMachine","e":"x"}`),
		"sm with version":   enc(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"StateMachine","n":1,"e":"5f1c1b0e-6d1a-11f1-8000-000000000001"}`),
		"unknown field":     enc(`{"v":1,"t":"2026-09-23T10:00:00Z","k":"EntityChange","n":1,"x":1}`),
	} {
		if _, err := decodeCursor(c); err == nil {
			t.Errorf("%s: %q accepted", name, c)
		}
	}
}
```

Handler tests (memory stack, `handler_test.go` helpers):

```go
// A new transaction committing mid-walk adds events at the newest end; the
// walk must neither repeat nor skip the events that existed when it began.
func TestCursor_NewEventsMidWalk(t *testing.T) {
	srv := newTestServer(t)
	importAndLockModel(t, srv.URL, "AuditWalk", 1, `{"name":"A"}`)
	id := createEntityAndGetID(t, srv.URL, "AuditWalk", 1, `{"name":"v1"}`)
	for _, n := range []string{"v2", "v3", "v4"} {
		updateEntity(t, srv.URL, id, `{"name":"`+n+`"}`)
	}
	all, _ := getAuditEvents(t, srv.URL, id, "eventType=EntityChange")
	page1, p1 := getAuditEvents(t, srv.URL, id, "eventType=EntityChange", "limit=2")
	updateEntity(t, srv.URL, id, `{"name":"v5"}`)
	page2, _ := getAuditEvents(t, srv.URL, id, "eventType=EntityChange", "limit=10", "cursor="+url.QueryEscape(p1["nextCursor"].(string)))
	walked := append(page1, page2...)
	if len(walked) != len(all) {
		t.Fatalf("walk returned %d events, want the %d that existed at the start", len(walked), len(all))
	}
	for i := range all {
		if walked[i]["version"] != all[i]["version"] {
			t.Fatalf("position %d: version %v, want %v", i, walked[i]["version"], all[i]["version"])
		}
	}
}

func TestCursor_Undecodable_Returns400(t *testing.T) {
	// entity exists; cursor=20 → 400 BAD_REQUEST (commontest.ExpectErrorCode)
}

func TestCursor_SurvivesFilterChange(t *testing.T) {
	// entity with workflow; page 1 unfiltered limit=1; page 2 with
	// eventType=StateMachine using page 1's cursor → 200, and every
	// returned event sorts after page 1's event.
}

func TestCursor_PositionOfMissingEvent(t *testing.T) {
	// build a cursor with encodeCursor-equivalent JSON for a version that
	// does not exist, between two real versions → returns only the older
	// ones; a cursor older than every event → empty items, hasNext false.
	// (Handler-level: craft the cursor string in the test with base64 JSON.)
}

func TestCursor_ClampedLimitStillPages(t *testing.T) {
	// limit=5000 on a small trail → 200, all items, hasNext false; the
	// clamp itself is exercised by asserting no 400.
}

func TestCursor_WalkOverOneInstantTie(t *testing.T) {
	// entity created with a workflow (EC + several SM events share the
	// commit instant); walk with limit=1 following nextCursor until
	// hasNext is false; the concatenation equals the unpaged list, in order.
}
```

Run — FAIL.

- [ ] **Step 2: Implement `cursor.go`**

```go
package audit

// cursorBody is the decoded form of an audit page cursor: the sort key of
// the last event on the previous page. Opaque to callers.
type cursorBody struct {
	V int     `json:"v"`
	T string  `json:"t"`
	K string  `json:"k"`
	N *int64  `json:"n,omitempty"`
	E *string `json:"e,omitempty"`
}

func encodeCursor(k eventKey) string {
	b := cursorBody{V: 1, T: k.at.UTC().Format(time.RFC3339Nano), K: k.kind}
	if k.kind == "EntityChange" {
		n := k.version
		b.N = &n
	} else {
		e := k.eventID.String()
		b.E = &e
	}
	raw, _ := json.Marshal(b) // fixed struct of strings and ints; cannot fail
	return base64.RawURLEncoding.EncodeToString(raw)
}

var errBadCursor = errors.New("invalid cursor")

func decodeCursor(s string) (eventKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return eventKey{}, errBadCursor
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var b cursorBody
	if err := dec.Decode(&b); err != nil || b.V != 1 {
		return eventKey{}, errBadCursor
	}
	at, err := time.Parse(time.RFC3339Nano, b.T)
	if err != nil {
		return eventKey{}, errBadCursor
	}
	switch b.K {
	case "EntityChange":
		if b.N == nil || *b.N < 1 || b.E != nil {
			return eventKey{}, errBadCursor
		}
		return eventKey{at: at, kind: b.K, version: *b.N}, nil
	case "StateMachine":
		if b.E == nil || b.N != nil {
			return eventKey{}, errBadCursor
		}
		id, err := uuid.Parse(*b.E)
		if err != nil {
			return eventKey{}, errBadCursor
		}
		return eventKey{at: at, kind: b.K, eventID: id}, nil
	}
	return eventKey{}, errBadCursor
}
```

Handler paging (replaces the offset block). Decode the cursor BEFORE any store call so a bad cursor never touches storage:

```go
	var after *eventKey
	if params.Cursor != nil {
		k, err := decodeCursor(*params.Cursor)
		if err != nil {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "invalid cursor parameter"))
			return
		}
		after = &k
	}
	...
	start := 0
	if after != nil {
		start = sort.Search(len(items), func(i int) bool { return compareKeys(items[i].key, *after) > 0 })
	}
	end := min(start+limit, len(items))
	page := items[start:end]
	hasNext := end < len(items)
	pagination := map[string]any{"hasNext": hasNext}
	if hasNext {
		pagination["nextCursor"] = encodeCursor(page[len(page)-1].key)
	}
```

Move the `limit` validation above the store calls too (both are request validation).

- [ ] **Step 3: Run** — PASS, including the pre-existing `TestAuditPagination`.
- [ ] **Step 4: Commit** `fix(audit): the page cursor is a position in the order, and an unreadable one is rejected`

### Task 10: A failed state machine audit read fails the request

**Files:**
- Modify: `internal/domain/audit/handler.go` (SM branch)
- Test: `internal/domain/audit/handler_outage_test.go` (extend)

- [ ] **Step 1: Failing tests** — extend the stubs: a factory whose `EntityStore` returns a stub with one `EntityVersionMeta{Version:1, ChangeType:"CREATED", Timestamp: t0}` and whose `StateMachineAuditStore` either errors or returns a store whose `GetEvents` errors.

```go
func TestSearch_SMFactoryOutage_Returns503(t *testing.T)   // factory err wraps smOutageErr → 503 STORAGE_UNAVAILABLE, retryable, body has no smOutageDSN
func TestSearch_SMFactoryFailure_Returns500(t *testing.T)  // plain error → 500, ticket present, no internals
func TestSearch_SMGetEventsOutage_Returns503(t *testing.T)
func TestSearch_SMGetEventsFailure_Returns500(t *testing.T)
func TestSearch_EntityChangeOnly_IgnoresSMStore(t *testing.T) // eventType=EntityChange with SM factory erroring → 200, one item
```

Assertions follow `TestGetStateMachineFinishedEvent_StorageOutage_Returns503` (status, `commontest.ExpectErrorCode`, `retryable`, `!strings.Contains(body, smOutageDSN)`).

Run — FAIL (today: 200).

- [ ] **Step 2: Implement**

```go
	if includeStateMachine {
		smStore, err := h.factory.StateMachineAuditStore(ctx)
		if err != nil {
			common.WriteError(w, r, common.Internal("failed to get state machine audit store", err))
			return
		}
		// A failed read is not an entity without workflow events: answering
		// 200 without them would be a partial trail presented as complete.
		smEvents, err := smStore.GetEvents(ctx, entityId.String())
		if err != nil {
			common.WriteError(w, r, common.Internal("failed to get state machine events", err))
			return
		}
		for _, ev := range smEvents {
			item, err := stateMachineItem(ev)
			if err != nil {
				common.WriteError(w, r, common.Internal("invalid state machine event", err))
				return
			}
			items = append(items, item)
		}
	}
```

- [ ] **Step 3: Run** — PASS.
- [ ] **Step 4: Commit** `fix(audit): a failed state machine audit read fails the request instead of dropping the events`

---

## Stream D — running-backend coverage and documentation

### Task 11: E2E (PostgreSQL, `internal/e2e`)

**Files:**
- Create: `internal/e2e/audit_identity_order_test.go`
- Modify: `internal/e2e/callback_harness_test.go` (add `rc.UpdateEntity`)

- [ ] **Step 1: Add the joined update helper**

```go
// UpdateEntity issues a PUT /entity/JSON/{id} callback echoing the tx-token,
// so the update joins the primary's transaction T.
func (rc *reqCtx) UpdateEntity(entityID, payload string) (callbackResult, error) {
	return rc.h.callback(http.MethodPut, "/api/entity/JSON/"+entityID, payload, rc.token)
}
```

- [ ] **Step 2: Tests** — each asserts through the full HTTP stack on Postgres:

1. `TestAuditE2E_TwoJoinedSavesHaveDistinctVersions` — callback harness; processor `cb-create-then-update` creates a secondary (`secondaryWorkflow`) then updates it joined; the secondary's audit trail (no filter) has two `EntityChange` events with the same `transactionId` and `utcTime`, versions 2 then 1, `changeType` UPDATE then CREATE; all its `StateMachine` events of that transaction follow them, `eventId`s distinct UUIDs, sorted by `compare` rule (assert adjacent-pair order using the emitted fields and `uuid.Parse(eventId).Time()`/bytes).
2. `TestAuditE2E_WalkOverOneInstantTie` — same secondary; walk with `limit=1` following `nextCursor` until `hasNext` false; concatenation equals the unpaged list.
3. `TestAuditE2E_FinishedEventIDMatchesSearch` — plain workflow entity (existing `audit_finished_event_test.go` setup); finished event `eventId` == the `STATE_MACHINE_FINISH` event's `eventId` in the search.
4. `TestAuditE2E_NewEventsMidWalk` — as the Task 9 handler test, against Postgres.
5. `TestAuditE2E_Cursor400` — table: `"20"`, `"!!!"`, base64 of `{"v":1,"t":"2026-09-23T10:00:00Z","k":"System","n":1}`, base64 of an SM cursor with `"e":"x"`, base64 of an EC cursor with `"n":0` → each `400 BAD_REQUEST` via `assertProblemJSON`.

- [ ] **Step 3: Run** `go test ./internal/e2e/ -run 'TestAuditE2E'` — PASS (Docker required). RED check: temporarily revert Task 8/9 locally (`git stash push -m 586-red-check -- internal/domain/audit`, capture sha, run, `git stash apply <sha>`, drop) and confirm tests 1, 2, 4, 5 fail; note the result in the commit body.
- [ ] **Step 4: Commit** `test(e2e): audit identity, order and cursor on a running backend`

### Task 12: Cross-backend parity

**Files:**
- Modify: `e2e/parity/client/http.go` (add `GetAuditEventsPage`)
- Create: `e2e/parity/audit_identity.go`
- Modify: `e2e/parity/registry.go` (register after the Phase 4a audit entries)

- [ ] **Step 1: Client helper**

```go
// GetAuditEventsPage issues GET /api/audit/entity/{entityId} with the given
// query (limit, cursor, eventType, ...).
func (c *Client) GetAuditEventsPage(t *testing.T, entityID uuid.UUID, query url.Values) (EntityAuditEventsResponse, error) {
	t.Helper()
	path := "/api/audit/entity/" + entityID.String()
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var resp EntityAuditEventsResponse
	if _, err := c.doJSON(t, http.MethodGet, path, nil, &resp); err != nil {
		return EntityAuditEventsResponse{}, err
	}
	return resp, nil
}
```

- [ ] **Step 2: Scenarios** (`fixture.ComputeTenant(t)`, `cbSetupModel`, `cbPrimaryProcWorkflow("…", "cb-ifmatch-update", "SYNC", cbContext(secondary, marker))`, primary sample `cbSampleIfMatchUpdate`, secondary `cbSecondaryWorkflow`):

- `RunAuditIdentityJoinedSaves` — secondary (id from primary `data.secondaryId`) has two EC events in one transaction with distinct versions, v2 before v1; every SM event has a distinct UUID `eventId`; a second GET returns identical `eventId`s in identical order; adjacent pairs obey the order rule.
- `RunAuditCursorWalkOverTie` — `limit=1` walk over the secondary's trail equals the unpaged list.
- `RunAuditFinishedEventIDMatchesSearch` — `GetWorkflowFinished` → `eventId` equals the search's `STATE_MACHINE_FINISH` event of that transaction.

Register:

```go
	{"AuditIdentityJoinedSaves", RunAuditIdentityJoinedSaves},
	{"AuditCursorWalkOverTie", RunAuditCursorWalkOverTie},
	{"AuditFinishedEventIDMatchesSearch", RunAuditFinishedEventIDMatchesSearch},
```

- [ ] **Step 3: Run** `make test` — the parity suites run these on memory, sqlite, postgres — PASS.
- [ ] **Step 4: Commit** `test(parity): audit identity, order and cursor on every backend`

### Task 13: Documentation and Cloud parity

**Files:**
- Modify: `cmd/cyoda/help/content/audit.md`
- Modify: `CHANGELOG.md` (Unreleased / v0.9.0 section — follow the file's existing heading layout)
- Create: `docs/cloud-parity/audit-event-identity-and-order.md`
- Modify: `docs/cloud-parity/README.md` (one table row)

- [ ] **Step 1: Help topic** — document `version` and `eventId` in the event shapes (add both to the JSON example), the order (one sentence per tie-break level), the cursor (opaque position; unreadable → 400; filters may change between pages), and the limit ("a walk returns each event committed before it started exactly once; an event committed during a walk may be missed"). Replace `"nextCursor": "20"` with a realistic opaque value (`"eyJ2IjoxLCJ0IjoiMjAyNS0wOC0wMVQxMDowNTowMFoiLCJrIjoiRW50aXR5Q2hhbmdlIiwibiI6Mn0"`). Keep the `limit` clamp sentence. Run the help-content tests: `go test ./cmd/cyoda/help/...`.
- [ ] **Step 2: CHANGELOG** — Added: `version` on entity change audit events, `eventId` on state machine audit events. Changed: audit events of one instant come back in a fixed order; the audit page cursor is a position, and a cursor from an earlier build (e.g. `"20"`) now answers 400. Fixed: a failed state machine audit read answers 503/500 instead of a trail with those events missing; the memory backend no longer reuses a version number after delete and recreate; the sqlite backend's generated ids are time-based.
- [ ] **Step 3: Cloud parity doc** — sections: what changes on the wire (both fields, their meaning, required); the order (four levels; Cloud already has the first two); the cursor (position, 400 on unreadable; Cloud's per-source offset cursor has the same shifting window); the store-owned event id (Cloud generates it in the engine's event factory — either is acceptable as long as it is unique, stable, and the value the read returns); what Cloud must do (add `version` from the entity change's version/transaction number, expose the state machine event `timeUuid` as `eventId`, add the version/eventId tie-break, move to a position cursor). README row: `| audit-event-identity-and-order.md | Audit events carry version / eventId; one total order; position cursor; a failed state machine audit read fails the request |`.
- [ ] **Step 4: Commit** `docs: audit event identity, order and cursor`

### Task 14: SPI PR, pin, and plugin repin

- [ ] **Step 1:** `make test-full` green with the local `go.work` line (proves the SPI cases on all three in-tree backends).
- [ ] **Step 2:** Open the SPI PR into `cyoda-go-spi` `main` (base `main`, title `feat(audit): the state machine event id is the store's; versions are never reused`, body: the contract, the two cases, "consumer: cyoda-go#586; cassandra: Cyoda/cyoda-go-cassandra#101"). After review, merge it via `gh api` (per repo convention) — if merge authority is unclear, ask Paul.
- [ ] **Step 3:** In cyoda-go, bump the SPI require line to the pseudo-version of the merged SPI `main` commit in root + three plugin `go.mod`s in ONE commit; `make check-spi-pin-sync`. Remove the local `go.work` use line (`go work edit -dropuse …`) and confirm `git diff go.work` is empty.
- [ ] **Step 4:** Push; `make repin-plugins`; commit the repin as a NEW commit (never amend the commit it points at); push. Verify `GOWORK=off go build ./...` at root.

### Task 15: Verification, reviews, PR

- [ ] **Step 1:** `make test-full` — read the counts; every suite ran. `go vet ./...` (root) and `make check-gofmt check-codegen`. `make race` once.
- [ ] **Step 2:** Whole-branch code review by a fresh-context subagent (`superpowers:requesting-code-review`); fix findings via TDD.
- [ ] **Step 3:** Security review with `antigravity-bundle-security-engineer:security-auditor` against Gate 3: tenant isolation of every audit read (the cursor carries no tenant or entity; it is applied only to the caller's tenant-scoped list), 4xx detail vs 5xx generic + ticket, no internals in the 400 cursor message, nothing logged from the cursor.
- [ ] **Step 4:** Rebase check: `git merge-base HEAD origin/release/v0.9.0`; PR `gh pr create --base release/v0.9.0` with body: summary, the error table, the coverage matrix, TDD waiver (Task 5), links to the SPI PR, Cyoda/cyoda-go-cassandra#101, and the spec. Milestone the issue v0.9.0 (already). Close #586 when the PR lands on the release branch.
- [ ] **Step 5:** File the Cloud Jira ticket (project CP, title prefix `[CaaS]`, component CaaS) pointing at `docs/cloud-parity/audit-event-identity-and-order.md` on the branch.
