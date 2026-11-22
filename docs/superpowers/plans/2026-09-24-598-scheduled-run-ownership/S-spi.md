# Stream S — SPI: types, interface, errors, audit constant, `spitest` ScheduledTasks suite

Spec sections: §10.1 (all), §12 "SPI" bullets, §13 rows with a ✓ in column S
(mapped at the end), §15 exit checks inside the SPI. Binding names:
`interfaces.md` § SPI.

## Facts settled by reading the code

Line numbers are at SPI `origin/main` = `8aa2258b26cbdc253629b8338698e95b033349b8`,
which is the pseudo-version all four cyoda-go `go.mod`s pin
(`v0.8.5-0.20260923200233-8aa2258b26cb`, `go.mod:7`, `plugins/*/go.mod:6`).
An earlier edit shifts later lines, so each step also quotes the text it
replaces; locate by that text.

1. **The local SPI checkout is on another branch.**
   `/Users/paul/go-projects/cyoda-light/cyoda-go-spi` is on
   `feat/callout-idempotent-retry-policy` (HEAD `426d2ad`), clean. S-1 cuts the
   stream branch from `origin/main`.
2. **Today's store** (`persistence.go:45-64`): `Upsert`, `Get(ctx, id)`,
   `ScanDue`, `MarkRedispatch`, `Delete`, `ReconcileForEntity`. `Get` takes no
   tenant. `ReconcileRequest` is `persistence.go:28-43`; its `Cancel` deletions
   are not in the returned slice (`:31-32`).
3. **Today's task** (`types.go:358-397`) carries `RedispatchAfter` (`:378-380`)
   and `AttemptCount` (`:390`). `types.go` imports only `encoding/json`, `fmt`,
   `time` (`:3-7`). `google/uuid` is an allowed SPI dependency
   (`CONTRIBUTING.md:8`) and root files already import it (`eval_leaf.go:10`,
   `parse_typed.go:6`).
4. **Other mentions of the removed names in the SPI:** `search_store.go:163-164`
   and `:192-193` ("as with ScheduledTaskStore.ScanDue (persistence.go:19-24)"),
   `types_test.go:312-348` (`RedispatchAfter`, `AttemptCount`),
   `scheduled_task_store_conformance.go` (whole file), `persistence.go:18-23,
   45-63`. `CHANGELOG.md` is excluded by the §15 check.
5. **`RunScheduledTaskStoreConformance`** (`scheduled_task_store_conformance.go:10`)
   is a root-package, non-test file that imports `testing`, which
   `CONTRIBUTING.md:8` says belongs in a sub-package. Its callers are in
   cyoda-go: `plugins/memory/scheduled_task_store_test.go:12`,
   `plugins/sqlite/scheduled_task_store_test.go:15`,
   `plugins/postgres/scheduled_task_store_test.go:21`.
6. **Errors.** `ErrStaleClaim` is `errors.go:155-158`; its comment names only
   `AsyncSearchStore`. `AsyncSearchStore.Release` also returns it
   (`search_store.go:199-211`) and the comment does not say so. There is no
   SPI "not implemented" sentinel (`grep -rn -i 'not implemented\|NotImplemented\|ErrUnsupported' *.go spitest/` → only prose hits in `eval_leaf.go:40`, `filter_match_test.go:427`).
7. **The Cassandra backend** returns a plain `errors.New` from
   `StoreFactory.ScheduledTaskStore` (`cyoda-go-cassandra
   internal/factory/factory.go:761`, `internal/store/errors.go:40`), so the
   factory accessor, not a method, is where "not implemented" shows.
8. **Audit constants** are `types.go:419-422`; `TestScheduledTransitionEventTypes`
   (`types_test.go:350-362`) pins them. No other SPI file lists event types.
9. **The stale sentence** is `types.go:305-311` ("The generic abstraction and
   the runtime that implements it ship in a later release; until then,
   consuming engines silently skip …"). The semantics bullets above it
   (`:288-301`) and the `TimeoutMs` field comment (`:319-326`) say a late task
   is "dropped"; under spec §4/§5.1 that holds only for a first attempt.
10. **`spitest` harness** (`spitest/spitest.go`). Subtests register through
    `runSubtest(t, h, tracker, name, fn)` (`:114-120`); groups run from
    `StoreFactoryConformance` (`:149-159`), each as
    `runXSuite(t, h, tracker)`. Tenant contexts come from `tenantContext`
    (`:170-176`); IDs from `newID()` (`helpers.go:72`). Transactions come from
    `h.Factory.TransactionManager(ctx)` → `Begin(ctx) (txID, txCtx, err)`,
    `Commit(txCtx, txID)`, `Rollback(txCtx, txID)` (`helpers.go:50-65`,
    `transaction.go:16-29`). `Begin` needs a tenant context
    (`plugins/sqlite/txmanager.go:373-379`).
11. **Store acquisition.** All three in-tree factories ignore the ctx passed to
    `ScheduledTaskStore(ctx)` and resolve the transaction per call
    (`plugins/memory/store_factory.go:246`, `plugins/sqlite/store_factory.go:409`,
    `plugins/postgres/store_factory.go:275`).
12. **Clocks.** The postgres harness caps `AdvanceClock` at 100 ms and floors
    it at 5 ms (`plugins/postgres/conformance_test.go:170-194`). Memory and
    SQLite move an injected clock. The suite never needs more than 60 ms.
13. **PostgreSQL snapshot.** A REPEATABLE READ snapshot is taken at the
    transaction's first statement, not at `BEGIN`. A test that needs "the
    transaction began before the other write" reads through the transaction
    first.
14. **File names.** A file named `*_arm.go` is a GOARCH build constraint and
    would compile only on 32-bit ARM. The lifecycle file is named
    `scheduledtasks_lifecycle.go` for that reason.

## Working rules for this stream

- Every command runs in `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`
  unless it says otherwise. Each Bash call `cd`s explicitly; git uses `-C`.
- Stage named paths only. Never `git add -A`.
- **TDD for a conformance suite.** S-1..S-3 and S-3a are ordinary RED/GREEN in the SPI.
  The `spitest` group (S-4..S-8) is itself test code, and `spitest` has no
  tests of its own (`go test ./spitest/` → `[no test files]`). Inside the SPI,
  each suite task's RED is the build: the registration lines go in first and
  `go vet ./spitest/` fails on the undefined functions; GREEN is the vet
  passing once the file exists. The behavioural RED is observed in BM, BQ and
  BP: each runs the group against its backend before changing the store and
  sees the subtests fail.
- Nothing is pushed or tagged in this stream. Pushing, the SPI PR, the consumer
  notice and the pin are the lead's (README "Order of work", wave 6).

---

### Task S-1: Branch; three new errors; `ErrStaleClaim` covers both stores

**Spec:** §10.1 "New errors", "`ErrStaleClaim`'s doc comment"; §5.5 table; §5.6
"The node latches".

**Files:**
- Modify: `errors.go` (`:155-158`)
- Test: `errors_test.go` (append)

**Interfaces:**
- Consumes: nothing.
- Produces: `spi.ErrMarkedByAnotherClaim`, `spi.ErrTaskBusy`,
  `spi.ErrStoreRejected` (messages exactly as `interfaces.md`). `ErrStaleClaim`
  unchanged in value and message; comment widened.

- [ ] **Step 1: Cut the branch**

```
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi status --short
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi fetch origin
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi switch -c feat/scheduled-run-ownership origin/main
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi log -1 --format=%H
```

Expected: `status` prints nothing (if it prints anything, stop and tell the
lead); `log` prints `8aa2258b26cbdc253629b8338698e95b033349b8`. If it prints a
later commit, `origin/main` moved after planning: re-check facts 2-9 against it
before continuing.

- [ ] **Step 2: Write the failing test** — append to `errors_test.go`:

```go
// TestScheduledTaskSentinels pins the three scheduled-task sentinels: each
// is distinct from the others and from the sentinels a caller classifies
// alongside them, each survives wrapping, and the messages are the
// documented ones.
func TestScheduledTaskSentinels(t *testing.T) {
	all := map[string]error{
		"ErrStaleClaim":           ErrStaleClaim,
		"ErrMarkedByAnotherClaim": ErrMarkedByAnotherClaim,
		"ErrTaskBusy":             ErrTaskBusy,
		"ErrStoreRejected":        ErrStoreRejected,
		"ErrConflict":             ErrConflict,
		"ErrNotFound":             ErrNotFound,
	}
	for an, a := range all {
		for bn, b := range all {
			if an != bn && errors.Is(a, b) {
				t.Errorf("%s must not match %s", an, bn)
			}
		}
	}
	for name, err := range map[string]error{
		"ErrMarkedByAnotherClaim": ErrMarkedByAnotherClaim,
		"ErrTaskBusy":             ErrTaskBusy,
		"ErrStoreRejected":        ErrStoreRejected,
	} {
		if !errors.Is(fmt.Errorf("store layer: %w", err), err) {
			t.Errorf("wrapped %s must match via errors.Is", name)
		}
	}
	want := map[error]string{
		ErrMarkedByAnotherClaim: "scheduled task: marked by another claim of this life",
		ErrTaskBusy:             "scheduled task: row is being written by an open transaction",
		ErrStoreRejected:        "store rejected the write deterministically",
	}
	for err, msg := range want {
		if err.Error() != msg {
			t.Errorf("message = %q, want %q", err.Error(), msg)
		}
	}
}
```

- [ ] **Step 3: Run it and see it fail**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test . -run TestScheduledTaskSentinels`
Expected: FAIL — build error `undefined: ErrMarkedByAnotherClaim`,
`undefined: ErrTaskBusy`, `undefined: ErrStoreRejected`.

- [ ] **Step 4: Implement** — replace `errors.go:155-158` (the `ErrStaleClaim`
  comment and declaration) with:

```go
// ErrStaleClaim is returned by a fenced write whose caller no longer holds
// the claim it names:
//
//   - AsyncSearchStore (UpdateJobStatus, SaveResults, Heartbeat, Release):
//     the caller's epoch does not match the job's current Epoch — another
//     claimant has since taken over.
//   - ScheduledTaskStore (StampSegment, MarkUnsafe, RecordAttempt, Fail):
//     the task is missing, or its current arm token or claim token is not
//     the one in the TaskRef — the task was re-armed, reclaimed, recorded,
//     failed or removed since the caller claimed it.
var ErrStaleClaim = errors.New("write fenced: stale claim epoch")

// ErrMarkedByAnotherClaim is returned by ScheduledTaskStore.MarkUnsafe when
// an earlier claim of the same life already wrote a mark. Work that is not
// safe to repeat may already have been handed off for this life, so the
// caller must not dispatch it again.
var ErrMarkedByAnotherClaim = errors.New("scheduled task: marked by another claim of this life")

// ErrTaskBusy is returned by ScheduledTaskStore.MarkUnsafe when an open
// transaction has written the task row. The mark is not written. The
// caller treats it as a failure that is safe to retry.
var ErrTaskBusy = errors.New("scheduled task: row is being written by an open transaction")

// ErrStoreRejected marks a deterministic rejection by the store: the same
// write with the same input fails the same way every time, so retrying it
// cannot succeed. Examples: input that breaks a documented precondition
// (such as Attempt.Error over 1024 bytes, not valid UTF-8, or holding a
// NUL), or a constraint or data error from the database (on PostgreSQL,
// SQLSTATE classes 22, 23 and 42). A store wraps such an error so that
//
//	errors.Is(err, spi.ErrStoreRejected)
//
// holds. This applies to every method of every store in this SPI, and to a
// transaction's Commit — including StateMachineAuditStore.Record, which the
// engine calls in the same transaction as ScheduledTaskStore.Fail. Every
// other failure — outage, timeout, lock wait, pool exhaustion, conflict —
// must NOT carry it: callers retry those.
var ErrStoreRejected = errors.New("store rejected the write deterministically")
```

- [ ] **Step 5: Run it and see it pass**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test . && go vet ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi add errors.go errors_test.go
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi commit -m "feat(scheduled-task): ErrMarkedByAnotherClaim, ErrTaskBusy and the ErrStoreRejected marker

ErrStaleClaim's comment now covers the scheduled-task fence and Release.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task S-2: Task lives — status, failure reasons, claim, life fields, value types, `SCHEDULED_TRANSITION_FAIL`

**Spec:** §10.1 type block; §4 statuses and reasons; §5.7; §12 "SPI" bullets 1
and 4.

**Files:**
- Modify: `types.go` (imports `:3-7`; `TransitionSchedule` doc `:284-311`;
  `TimeoutMs` field comment `:319-326`; before `:358`; after `ArmedBy` `:396`;
  after `:397`; audit constants `:419-422`)
- Test: `types_test.go` (imports `:3-7`; before `:350`; `:351-356`)

This task only adds. `RedispatchAfter` and `AttemptCount` stay until S-3,
which removes them with the interface that uses them, so every commit builds.

**Interfaces:**
- Consumes: nothing.
- Produces (`interfaces.md` § SPI, verbatim): `ScheduledTaskStatus` and its
  three constants; `ScheduledTaskFailureReason` and its five constants;
  `TaskClaim`; the `ScheduledTask` life fields `Status`, `ArmToken`,
  `NextAttemptTime`, `Attempts`, `LostOwners`, `LastAttemptTime`, `LastError`,
  `FailureReason`, `FailedTime`, `PartialCommit`, `Claim`, `UnsafeMarked`,
  `ClaimedFromLostOwner` (`json:"-"`, README C-S1);
  `TaskRef`, `ClaimRequest`, `Attempt`, `Failure`, `ScheduledTaskCursor`,
  `ScheduledTaskQuery`, `ScheduledTaskPage`;
  `SMEventScheduledTransitionFailed = "SCHEDULED_TRANSITION_FAIL"`.

- [ ] **Step 1: Write the failing tests**

`types_test.go` imports become:

```go
import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)
```

Insert before `func TestScheduledTransitionEventTypes` (`:350`):

```go
func TestScheduledTask_LifeFields_RoundTrip(t *testing.T) {
	last, failed := int64(1_700_000_001_000), int64(1_700_000_002_000)
	in := ScheduledTask{
		ID: "e1:S:T", TenantID: "t1", Type: ScheduledTaskFireTransition,
		ScheduledTime:   1_700_000_000_000,
		Status:          ScheduledTaskFailed,
		ArmToken:        uuid.New(),
		NextAttemptTime: 1_700_000_000_500,
		Attempts:        2,
		LostOwners:      1,
		LastAttemptTime: &last,
		LastError:       "CONFLICT: a concurrent write changed the entity or its task",
		FailureReason:   FailureExpiredAfterFailedAttempts,
		FailedTime:      &failed,
		PartialCommit:   true,
		Claim:           &TaskClaim{Token: uuid.New(), Owner: uuid.New()},
		UnsafeMarked:    true,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ScheduledTask
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\n in: %+v\nout: %+v", in, out)
	}

	// A WAITING task has no claim and no failure: those keys are omitted.
	b2, _ := json.Marshal(ScheduledTask{ID: "x", Status: ScheduledTaskWaiting})
	for _, absent := range []string{`"claim"`, `"failureReason"`, `"failedTime"`, `"lastAttemptTime"`, `"lastError"`} {
		if strings.Contains(string(b2), absent) {
			t.Errorf("%s must be omitted when unset: %s", absent, b2)
		}
	}
}

func TestScheduledTask_StatusAndReasonValues(t *testing.T) {
	cases := map[string]string{
		string(ScheduledTaskWaiting):              "WAITING",
		string(ScheduledTaskRunning):              "RUNNING",
		string(ScheduledTaskFailed):               "FAILED",
		string(FailureUnsafeWorkNotCompleted):     "UNSAFE_WORK_NOT_COMPLETED",
		string(FailureOwnerLostRepeatedly):        "OWNER_LOST_REPEATEDLY",
		string(FailureExpiredAfterFailedAttempts): "EXPIRED_AFTER_FAILED_ATTEMPTS",
		string(FailureRunPanicked):                "RUN_PANICKED",
		string(FailureStoppedAfterPartialCommit):  "STOPPED_AFTER_PARTIAL_COMMIT",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

// ClaimedFromLostOwner is read-only and exists only on a ClaimDue result: it is
// never serialised, so no stored or returned JSON document carries it.
func TestScheduledTask_ClaimedFromLostOwnerIsNotSerialised(t *testing.T) {
	b, err := json.Marshal(ScheduledTask{ID: "x", Status: ScheduledTaskRunning, ClaimedFromLostOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(b)), "claimedfromlostowner") {
		t.Fatalf("ClaimedFromLostOwner must not be serialised: %s", b)
	}
	var back ScheduledTask
	if err := json.Unmarshal([]byte(`{"id":"x","claimedFromLostOwner":true,"ClaimedFromLostOwner":true}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.ClaimedFromLostOwner {
		t.Fatal("a JSON document must not set ClaimedFromLostOwner")
	}
}
```

In `TestScheduledTransitionEventTypes`, after the
`SMEventScheduledTransitionCancelled` row (`:355`), add:

```go
		SMEventScheduledTransitionFailed:    "SCHEDULED_TRANSITION_FAIL",
```

- [ ] **Step 2: Run them and see them fail**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test . -run 'TestScheduledTask_LifeFields_RoundTrip|TestScheduledTask_StatusAndReasonValues|TestScheduledTask_ClaimedFromLostOwnerIsNotSerialised|TestScheduledTransitionEventTypes'`
Expected: FAIL — build error `unknown field Status in struct literal of type
ScheduledTask`, `undefined: ScheduledTaskFailed`, `unknown field ArmToken …`,
`unknown field ClaimedFromLostOwner …`.

The store's value types (`TaskRef`, `ClaimRequest`, `Attempt`, `Failure`,
`ScheduledTaskCursor`, `ScheduledTaskQuery`, `ScheduledTaskPage`) have no
behaviour of their own to assert here; S-3's `TestScheduledTaskStore_InterfaceShape`
fails to build when one changes, and the `spitest` group exercises each.

- [ ] **Step 3: Implement** — `types.go`.

Imports (`:3-7`):

```go
import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)
```

Replace the `TransitionSchedule` doc comment (`:284-311`, down to and including
the `type TransitionSchedule struct {` line) with the following. It removes the
"consuming engines silently skip" sentence (spec §12) and replaces "dropped"
with the §4/§5.1 rules:

```go
// TransitionSchedule configures automatic firing of a future state
// transition. Presence of this struct on a TransitionDefinition marks
// the transition as scheduled.
//
// Semantics. The transition is due at scheduledTime = stateEntryTime +
// DelayMs, or at the time Function computes. TimeoutMs, when set, bounds
// how late it may fire: deadline = scheduledTime + *TimeoutMs.
//   - If TimeoutMs is nil, the task is attempted until it fires. A failed
//     attempt is retried without end.
//   - If a first attempt would start after the deadline, it is not made:
//     the task expires.
//   - A failed attempt is retried until the deadline. A task that has not
//     fired by then ends FAILED instead of expiring. The consuming engine
//     sets the retry delay.
//
// TimeoutMs gives operators control over how the system handles
// backlog and intermittent-offline conditions. Short positive values
// prefer freshness — stale tasks are discarded rather than fired
// against a possibly-changed entity. Nil prefers eventual execution.
//
// Scheduled transitions are mutually exclusive with Manual=true.
//
// Scheduled transitions are a special case of a generic ScheduledTask
// abstraction. The lateness-tolerance concept (TimeoutMs) applies
// uniformly across all ScheduledTask variants.
type TransitionSchedule struct {
```

Replace the `TimeoutMs` field comment and field (`:319-326`) with:

```go
	// TimeoutMs is the late-tolerance window past the scheduled
	// execution time, in milliseconds. Nil means no timeout — the
	// task is attempted until it fires. Non-nil zero is the strictest
	// setting — a first attempt that starts late expires the task.
	// Non-nil positive N sets the deadline N milliseconds after
	// scheduledTime (see the type's Semantics). Independent of DelayMs;
	// the two measure different quantities.
	TimeoutMs *int64 `json:"timeoutMs,omitempty"`
```

Insert before the `ScheduledTask` doc comment (`:358`):

```go
// ScheduledTaskStatus is where a scheduled task is in its life.
type ScheduledTaskStatus string

const (
	// ScheduledTaskWaiting: armed, not claimed. Claimable once
	// NextAttemptTime has passed.
	ScheduledTaskWaiting ScheduledTaskStatus = "WAITING"
	// ScheduledTaskRunning: claimed. Claim names the owner and the claim.
	ScheduledTaskRunning ScheduledTaskStatus = "RUNNING"
	// ScheduledTaskFailed: ended without firing. Never claimed. Kept until
	// the entity is written again, leaves the state, or is deleted.
	ScheduledTaskFailed ScheduledTaskStatus = "FAILED"
)

// ScheduledTaskFailureReason says why a task is FAILED.
type ScheduledTaskFailureReason string

const (
	FailureUnsafeWorkNotCompleted     ScheduledTaskFailureReason = "UNSAFE_WORK_NOT_COMPLETED"
	FailureOwnerLostRepeatedly        ScheduledTaskFailureReason = "OWNER_LOST_REPEATEDLY"
	FailureExpiredAfterFailedAttempts ScheduledTaskFailureReason = "EXPIRED_AFTER_FAILED_ATTEMPTS"
	FailureRunPanicked                ScheduledTaskFailureReason = "RUN_PANICKED"
	FailureStoppedAfterPartialCommit  ScheduledTaskFailureReason = "STOPPED_AFTER_PARTIAL_COMMIT"
)

// TaskClaim is the claim a RUNNING task is held under. Token is drawn by
// the store on every claim and never reused. Owner is the incarnation of
// the consuming engine's process that holds the claim.
type TaskClaim struct {
	Token uuid.UUID `json:"token"`
	Owner uuid.UUID `json:"owner"`
}
```

Insert after `ArmedBy Principal \`json:"armedBy,omitempty"\`` (`:396`), before
the struct's closing brace, one blank line and then:

```go
	// --- life: set by the store only ---

	Status ScheduledTaskStatus `json:"status"`
	// ArmToken names the current life. Drawn by the store on every arm.
	ArmToken uuid.UUID `json:"armToken"`
	// NextAttemptTime is unix-millis on the consuming engine's clock; a
	// WAITING task is claimable when NextAttemptTime <= ClaimRequest.NowMs.
	// Equals ScheduledTime on arm.
	NextAttemptTime int64 `json:"nextAttemptTime"`
	// Attempts counts the attempts recorded as failed in this life.
	Attempts int `json:"attempts"`
	// LostOwners counts the claims of this life taken from an owner that
	// stopped heartbeating.
	LostOwners      int                        `json:"lostOwners"`
	LastAttemptTime *int64                     `json:"lastAttemptTime,omitempty"`
	LastError       string                     `json:"lastError,omitempty"`
	FailureReason   ScheduledTaskFailureReason `json:"failureReason,omitempty"`
	FailedTime      *int64                     `json:"failedTime,omitempty"`
	// PartialCommit is set when a run of this life committed the entity
	// into a state other than the task's source state.
	PartialCommit bool `json:"partialCommit"`
	// Claim is set while the task is RUNNING, and nil otherwise.
	Claim *TaskClaim `json:"claim,omitempty"`
	// UnsafeMarked is read-only: a mark (ScheduledTaskStore.MarkUnsafe)
	// exists for this life.
	UnsafeMarked bool `json:"unsafeMarked"`
	// ClaimedFromLostOwner is read-only and set only on a task returned by
	// ScheduledTaskStore.ClaimDue: this claim took the task from an owner
	// whose liveness record was missing or stale. Never stored, never
	// serialised; every other read returns false.
	ClaimedFromLostOwner bool `json:"-"`
```

Insert after the struct's closing brace (`:397`), before
`// --- State machine event types ---`:

```go
// TaskRef names one claim of one life. Every fenced ScheduledTaskStore
// method takes it.
type TaskRef struct {
	TenantID   TenantID
	ID         string
	ArmToken   uuid.UUID
	ClaimToken uuid.UUID
}

// ClaimRequest is the input of ScheduledTaskStore.ClaimDue.
type ClaimRequest struct {
	// Owner is the claiming incarnation.
	Owner uuid.UUID
	// NowMs is the caller's clock, compared with NextAttemptTime.
	NowMs int64
	// StaleAfter is how old an owner's liveness record may be, on the
	// store clock, before its RUNNING tasks count as lost.
	StaleAfter time.Duration
	// Limit caps the number of tasks claimed by this call. Must be >= 1;
	// ClaimDue returns an error otherwise.
	Limit int
	// PerTenantLimit caps, per tenant, TenantInProgress[tenant] plus the
	// tasks of that tenant claimed by this call. Must be >= 1; ClaimDue
	// returns an error otherwise.
	PerTenantLimit int
	// TenantInProgress is the caller's count of runs in progress per
	// tenant. A missing tenant counts as 0.
	TenantInProgress map[TenantID]int
	// AllowLostOwner lets the call claim RUNNING tasks whose owner's
	// liveness record is missing or older than StaleAfter.
	AllowLostOwner bool
}

// Attempt is the input of ScheduledTaskStore.RecordAttempt.
type Attempt struct {
	// Error is the recorded error text. The caller sanitises it: at most
	// 1024 bytes, valid UTF-8, no NUL. A store refuses any other text with
	// an error that satisfies errors.Is(err, ErrStoreRejected).
	Error string
	// AtMs is when the attempt ended, unix-millis.
	AtMs int64
	// NextAttemptTime is when the task is claimable again, unix-millis.
	NextAttemptTime int64
	// NotCounted leaves Attempts unchanged. Error and AtMs are recorded
	// all the same.
	NotCounted bool
	// ClearOwnMark removes the mark this claim wrote, in the same write.
	ClearOwnMark bool
}

// Failure is the input of ScheduledTaskStore.Fail.
type Failure struct {
	Reason ScheduledTaskFailureReason
	// Error follows the same rules as Attempt.Error. It always replaces
	// LastError, even when it is empty.
	Error string
	// AtMs is the failure time, unix-millis. Stored as FailedTime.
	AtMs int64
}

// ScheduledTaskCursor is a position in the (ScheduledTime, ID) order of
// ScheduledTaskStore.Query. IDs compare byte-wise.
type ScheduledTaskCursor struct {
	ScheduledTime int64
	ID            string
}

// ScheduledTaskQuery filters ScheduledTaskStore.Query.
type ScheduledTaskQuery struct {
	Statuses     []ScheduledTaskStatus // empty = all
	ModelName    string                // "" = any
	ModelVersion int                   // 0 = any; only with ModelName
	EntityID     string                // "" = any
	After        *ScheduledTaskCursor  // exclusive
	Limit        int                   // 1..1000, validated by the caller
}

// ScheduledTaskPage is one page of ScheduledTaskStore.Query.
type ScheduledTaskPage struct {
	Items []ScheduledTask
	Next  *ScheduledTaskCursor // nil when there is no further page
}
```

After `SMEventScheduledTransitionCancelled` (`:422`):

```go
	SMEventScheduledTransitionFailed    StateMachineEventType = "SCHEDULED_TRANSITION_FAIL"
```

- [ ] **Step 4: Run them and see them pass**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && gofmt -l . && go test . && go vet ./...`
Expected: `gofmt -l` prints nothing; PASS.

- [ ] **Step 5: Commit**

```
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi add types.go types_test.go
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi commit -m "feat(scheduled-task): a task has lives — status, claim, attempts, failure reasons

Adds the life fields, ClaimedFromLostOwner, TaskClaim, the store's value
types and SCHEDULED_TRANSITION_FAIL. TransitionSchedule no longer says engines skip
scheduled transitions, and TimeoutMs says what happens after a failed
attempt.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task S-3: The new `ScheduledTaskStore`; the old fields, methods and root conformance function removed

**Spec:** §10.1 (method table, "Fenced", "Never joins", clauses C1-C6,
"Removed"); §7 "Arm", "Cancel"; §15.

**Files:**
- Modify: `persistence.go` (imports `:3-8`; `StoreFactory.ScheduledTaskStore`
  doc `:18-23`; `ReconcileRequest` and `ScheduledTaskStore` `:28-64`)
- Modify: `types.go` (`ScheduledTask` doc and `ID` doc `:358-370`; delete
  `:378-380`; `:389-390`)
- Modify: `search_store.go` (`:162-164`, `:192-193`)
- Delete: `scheduled_task_store_conformance.go`
- Test: `persistence_test.go` (replace whole file), `types_test.go`
  (`TestScheduledTask_RoundTrips`, `:312-348`)

**Interfaces:**
- Consumes: S-1 errors, S-2 types.
- Produces: `spi.ScheduledTaskStore` exactly as `interfaces.md`, with the
  contract on each method; `ReconcileRequest` (fields unchanged, semantics per
  `interfaces.md`); `StoreFactory.ScheduledTaskStore` doc: a backend without
  the store returns an error satisfying `errors.Is(err, errors.ErrUnsupported)`.
- Removes: `ScheduledTask.RedispatchAfter`, `ScheduledTask.AttemptCount`,
  `Upsert`, `ScanDue`, `MarkRedispatch`, `Delete`, `Get(ctx, id)`,
  `spi.RunScheduledTaskStoreConformance`.

- [ ] **Step 1: Write the failing tests**

Replace `persistence_test.go` with:

```go
package spi

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestScheduledTaskStore_InterfaceShape pins every method signature of
// ScheduledTaskStore. Compile-time only: a changed signature fails the
// build of this test.
func TestScheduledTaskStore_InterfaceShape(t *testing.T) {
	var _ = (StoreFactory)(nil)
	var _ func(StoreFactory, context.Context) (ScheduledTaskStore, error) = StoreFactory.ScheduledTaskStore

	var _ func(ScheduledTaskStore, context.Context, ReconcileRequest) ([]ScheduledTask, error) = ScheduledTaskStore.ReconcileForEntity
	var _ func(ScheduledTaskStore, context.Context, TenantID, string, uuid.UUID) error = ScheduledTaskStore.RemoveLife
	var _ func(ScheduledTaskStore, context.Context, TaskRef, bool) error = ScheduledTaskStore.StampSegment
	var _ func(ScheduledTaskStore, context.Context, TenantID, []string) error = ScheduledTaskStore.DeleteForEntities
	var _ func(ScheduledTaskStore, context.Context, TenantID, string, int, func(string, string) bool) error = ScheduledTaskStore.DeleteForModel
	var _ func(ScheduledTaskStore, context.Context, TenantID, string) (*ScheduledTask, bool, error) = ScheduledTaskStore.Get
	var _ func(ScheduledTaskStore, context.Context, TenantID, ScheduledTaskQuery) (ScheduledTaskPage, error) = ScheduledTaskStore.Query
	var _ func(ScheduledTaskStore, context.Context, ClaimRequest) ([]ScheduledTask, error) = ScheduledTaskStore.ClaimDue
	var _ func(ScheduledTaskStore, context.Context, uuid.UUID) error = ScheduledTaskStore.Heartbeat
	var _ func(ScheduledTaskStore, context.Context, uuid.UUID) error = ScheduledTaskStore.RetireOwner
	var _ func(ScheduledTaskStore, context.Context, time.Duration) error = ScheduledTaskStore.SweepOwners
	var _ func(ScheduledTaskStore, context.Context, uuid.UUID, []uuid.UUID) (int, error) = ScheduledTaskStore.GiveBackIdle
	var _ func(ScheduledTaskStore, context.Context, TaskRef) error = ScheduledTaskStore.MarkUnsafe
	var _ func(ScheduledTaskStore, context.Context, TaskRef, Attempt) error = ScheduledTaskStore.RecordAttempt
	var _ func(ScheduledTaskStore, context.Context, TaskRef, Failure) error = ScheduledTaskStore.Fail
	var _ func(ScheduledTaskStore, context.Context) error = ScheduledTaskStore.SweepMarks
}

// TestScheduledTaskStore_MethodSet pins that nothing else is on the
// interface: a store double with exactly these sixteen methods satisfies
// it, so a method added back fails the build.
func TestScheduledTaskStore_MethodSet(t *testing.T) {
	var _ ScheduledTaskStore = sixteenMethods{}
}

type sixteenMethods struct{}

func (sixteenMethods) ReconcileForEntity(context.Context, ReconcileRequest) ([]ScheduledTask, error) {
	return nil, nil
}
func (sixteenMethods) RemoveLife(context.Context, TenantID, string, uuid.UUID) error { return nil }
func (sixteenMethods) StampSegment(context.Context, TaskRef, bool) error             { return nil }
func (sixteenMethods) DeleteForEntities(context.Context, TenantID, []string) error   { return nil }
func (sixteenMethods) DeleteForModel(context.Context, TenantID, string, int, func(string, string) bool) error {
	return nil
}
func (sixteenMethods) Get(context.Context, TenantID, string) (*ScheduledTask, bool, error) {
	return nil, false, nil
}
func (sixteenMethods) Query(context.Context, TenantID, ScheduledTaskQuery) (ScheduledTaskPage, error) {
	return ScheduledTaskPage{}, nil
}
func (sixteenMethods) ClaimDue(context.Context, ClaimRequest) ([]ScheduledTask, error) {
	return nil, nil
}
func (sixteenMethods) Heartbeat(context.Context, uuid.UUID) error       { return nil }
func (sixteenMethods) RetireOwner(context.Context, uuid.UUID) error     { return nil }
func (sixteenMethods) SweepOwners(context.Context, time.Duration) error { return nil }
func (sixteenMethods) GiveBackIdle(context.Context, uuid.UUID, []uuid.UUID) (int, error) {
	return 0, nil
}
func (sixteenMethods) MarkUnsafe(context.Context, TaskRef) error             { return nil }
func (sixteenMethods) RecordAttempt(context.Context, TaskRef, Attempt) error { return nil }
func (sixteenMethods) Fail(context.Context, TaskRef, Failure) error          { return nil }
func (sixteenMethods) SweepMarks(context.Context) error                      { return nil }
```

Replace `TestScheduledTask_RoundTrips` (`types_test.go:312-348`) with:

```go
func TestScheduledTask_RoundTrips(t *testing.T) {
	to := int64(5000)
	task := ScheduledTask{
		ID:            "e1:S:T",
		TenantID:      "t1",
		Type:          ScheduledTaskFireTransition,
		ScheduledTime: 1_700_000_000_000,
		TimeoutMs:     &to,
		EntityID:      "e1",
		ModelName:     "order",
		ModelVersion:  2,
		Transition:    "AutoClose",
		SourceState:   "OPEN",
		ArmedAt:       1_699_999_999_000,
	}
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var back ScheduledTask
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Type != ScheduledTaskFireTransition || back.ScheduledTime != task.ScheduledTime ||
		back.TimeoutMs == nil || *back.TimeoutMs != 5000 || back.SourceState != "OPEN" {
		t.Fatalf("round-trip lost fields: %+v", back)
	}
	// The throttle and its counter are gone: a claim replaces the first, and
	// Attempts and LostOwners replace the second. A document that still
	// carries them does not bring them back.
	var old ScheduledTask
	if err := json.Unmarshal([]byte(`{"id":"x","redispatchAfter":5,"attemptCount":2}`), &old); err != nil {
		t.Fatal(err)
	}
	b3, _ := json.Marshal(old)
	for _, gone := range []string{"redispatchAfter", "attemptCount"} {
		if strings.Contains(string(b3), gone) {
			t.Errorf("%s must not be a field of ScheduledTask: %s", gone, b3)
		}
	}
	// nil TimeoutMs (no timeout) must round-trip as absent.
	task.TimeoutMs = nil
	b2, _ := json.Marshal(task)
	if strings.Contains(string(b2), "timeoutMs") {
		t.Errorf("nil TimeoutMs must be omitted: %s", b2)
	}
}
```

The removed names are checked through their JSON keys, so the test file does
not spell the Go names that the §15 exit check greps for.

- [ ] **Step 2: Run them and see them fail**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test . -run 'TestScheduledTaskStore_|TestScheduledTask_RoundTrips'`
Expected: FAIL — build error `ScheduledTaskStore.RemoveLife undefined (type
ScheduledTaskStore has no field or method RemoveLife)`, then `StampSegment`,
`DeleteForEntities`, `DeleteForModel` undefined, and `cannot use
ScheduledTaskStore.Get … as func(ScheduledTaskStore, context.Context, TenantID,
string) …`. (With only the `types_test.go` change applied, the run fails with
`attemptCount must not be a field of ScheduledTask`.)

- [ ] **Step 3: Implement**

`persistence.go` imports (`:3-8`):

```go
import (
	"context"
	"io"
	"iter"
	"time"

	"github.com/google/uuid"
)
```

Replace the `ScheduledTaskStore` accessor and its comment in `StoreFactory`
(`:18-23`) with:

```go
	// ScheduledTaskStore accesses durable scheduled tasks. Every
	// tenant-facing method takes its tenant as an argument; the
	// cross-tenant methods are named on the interface. The store resolves
	// the transaction from each call's ctx, not from the ctx it was
	// obtained with. A backend that does not implement it returns an error
	// satisfying errors.Is(err, errors.ErrUnsupported); the spitest suite
	// then skips the ScheduledTasks group.
	ScheduledTaskStore(ctx context.Context) (ScheduledTaskStore, error)
```

Replace `persistence.go:28-64` (the `ReconcileRequest` comment through the
closing brace of `ScheduledTaskStore`) with:

```go
// ReconcileRequest is the input of ScheduledTaskStore.ReconcileForEntity.
type ReconcileRequest struct {
	TenantID     TenantID
	EntityID     string
	CurrentState string
	// Arm lists the tasks to arm: the scheduled transitions of the
	// entity's current state. Each is armed as a new life. The store takes
	// the tenant and the entity from TenantID and EntityID above, never
	// from an Arm item, sets every life field (see ScheduledTask), and
	// ignores the caller's values for all of them.
	Arm []ScheduledTask
	// Cancel lists task IDs to remove whether or not they are in Arm's
	// state, e.g. born-expired scheduled transitions computed by a
	// ScheduleFunction whose result already lies in the past. They are not
	// reported in ReconcileForEntity's result: the caller audits them
	// separately.
	Cancel []string
}

// ScheduledTaskStore persists ScheduledTasks and the claims, marks and
// owner liveness records that fence their runs.
//
// Transactions. A JOINING method takes part in the transaction on ctx when
// there is one, so its effect commits or rolls back with the entity write;
// without one it applies at once. A NEVER-JOINING method ignores any
// transaction on ctx and commits on its own — that is how a mark survives
// the rollback of the run's transaction. Joining: ReconcileForEntity,
// RemoveLife, StampSegment, DeleteForEntities, DeleteForModel, Fail. Get
// may join. Never joining: Query, ClaimDue, Heartbeat, RetireOwner,
// SweepOwners, GiveBackIdle, MarkUnsafe, RecordAttempt, SweepMarks.
//
// Fenced methods (StampSegment, MarkUnsafe, RecordAttempt, Fail) are
// accepted only if the task's current arm token and claim token are the
// ones in the TaskRef, in the TaskRef's tenant. Otherwise, or when the
// task is missing, they return ErrStaleClaim and change nothing.
//
// A joining method called with a transaction on ctx whose tenant is not
// the method's tenant (its tenant argument, req.TenantID or ref.TenantID)
// is refused with ErrTxTenantMismatch and changes nothing.
//
// Clauses every implementation meets:
//
//	C1  First-committer-wins covers task rows. A transaction that writes a
//	    task row fails if another transaction, joining or not, committed a
//	    write to that row after this one began. RemoveLife counts as a
//	    write to the row it names whether or not it removes it.
//	C2  A joining read sees the operations staged earlier in the same
//	    transaction.
//	C3  A mark and a claim serialise: when MarkUnsafe and ClaimDue race on
//	    one task, either the mark is refused or the claim returns the task
//	    with UnsafeMarked set.
//	C4  Heartbeat and ClaimDue have connections of their own; entity
//	    transactions cannot starve them.
//	C5  A C1 refusal satisfies errors.Is(err, ErrConflict), whether a
//	    statement or the commit raises it. A fenced refusal is
//	    ErrStaleClaim.
//	C6  A task row written by an open transaction is not claimable until
//	    that transaction ends, and MarkUnsafe answers ErrTaskBusy for it.
//
// Tenant scoping: every method that takes a tenant, or a TaskRef, reads
// and writes that tenant's tasks only. ClaimDue, GiveBackIdle, Heartbeat,
// RetireOwner, SweepOwners and SweepMarks are cross-tenant; obtain the
// store for them with a background, tenant-less context.
//
// A deterministic rejection by the store, from any method, satisfies
// errors.Is(err, ErrStoreRejected); no other error does (see
// ErrStoreRejected).
type ScheduledTaskStore interface {
	// ReconcileForEntity arms req.Arm, each as a new life (WAITING,
	// NextAttemptTime = ScheduledTime, a new ArmToken, no claim, counters
	// and error fields cleared, PartialCommit false, no mark), whatever
	// status the row had. It removes every other task of the entity and
	// every task in req.Cancel. It returns the removed tasks, except those
	// named in req.Cancel. Joining.
	ReconcileForEntity(ctx context.Context, req ReconcileRequest) (removed []ScheduledTask, err error)

	// RemoveLife removes the task if its current life is armToken, in any
	// status; otherwise, or when the task is missing, it does nothing and
	// returns nil. It does nothing when this same transaction has already
	// replaced or removed the task. Joining.
	RemoveLife(ctx context.Context, tenant TenantID, id string, armToken uuid.UUID) error

	// StampSegment writes the task row, fenced. With partial it sets
	// PartialCommit; without it, PartialCommit keeps its value. Status and
	// claim are unchanged. Joining.
	StampSegment(ctx context.Context, ref TaskRef, partial bool) error

	// DeleteForEntities removes every task, in any status, of the listed
	// entities. An empty list is a no-op. Joining.
	DeleteForEntities(ctx context.Context, tenant TenantID, entityIDs []string) error

	// DeleteForModel removes every task, in any status, of the model
	// version, except those for which keep(sourceState, transition) is
	// true. A nil keep keeps none: every task of that model version is
	// removed. Joining.
	DeleteForModel(ctx context.Context, tenant TenantID, modelName string, modelVersion int,
		keep func(sourceState, transition string) bool) error

	// Get returns the task, with found false and a nil error when it does
	// not exist in tenant. With a transaction on ctx it sees that
	// transaction's staged operations (C2).
	Get(ctx context.Context, tenant TenantID, id string) (task *ScheduledTask, found bool, err error)

	// Query returns one page of tenant's tasks matching q, in
	// (ScheduledTime, ID) order ascending, IDs compared byte-wise. Next is
	// nil when no task follows the page. Never joining.
	Query(ctx context.Context, tenant TenantID, q ScheduledTaskQuery) (ScheduledTaskPage, error)

	// ClaimDue atomically claims up to req.Limit tasks across tenants and
	// returns them RUNNING, each with a new claim token and Claim.Owner =
	// req.Owner. Claimable: WAITING with NextAttemptTime <= req.NowMs; with
	// req.AllowLostOwner, also RUNNING whose owner's liveness record is
	// missing or older than req.StaleAfter on the store clock — such a
	// claim adds 1 to LostOwners. FAILED is never claimable. A task is
	// not claimed while another task of its entity is RUNNING, and at
	// most one task per entity is claimed per call; concurrent callers
	// obtain disjoint sets and never two tasks of one entity. Per tenant
	// at most req.PerTenantLimit - req.TenantInProgress[tenant] tasks are
	// claimed; tenants take turns — each tenant's first task comes before
	// any tenant's second — and within a tenant tasks are claimed in
	// NextAttemptTime order. A returned task carries UnsafeMarked as of
	// the claim (C3), and ClaimedFromLostOwner when this claim took it from
	// a stale or missing owner. Losing a race to another caller is not an error: the
	// call returns what it claimed, possibly nothing. req.Limit < 1 or
	// req.PerTenantLimit < 1 is a caller error: ClaimDue returns an error
	// and claims nothing. Never joining.
	ClaimDue(ctx context.Context, req ClaimRequest) ([]ScheduledTask, error)

	// Heartbeat creates or refreshes owner's liveness record, stamped with
	// the store clock. Never joining.
	Heartbeat(ctx context.Context, owner uuid.UUID) error

	// RetireOwner removes owner's liveness record. Never joining.
	RetireOwner(ctx context.Context, owner uuid.UUID) error

	// SweepOwners removes the liveness records older than deadFor that no
	// task references. Never joining.
	SweepOwners(ctx context.Context, deadFor time.Duration) error

	// GiveBackIdle returns every task RUNNING under owner whose claim
	// token is not in keep to WAITING, claimable at once, and reports how
	// many. Attempts, LostOwners and marks are unchanged. Never joining.
	GiveBackIdle(ctx context.Context, owner uuid.UUID, keep []uuid.UUID) (int, error)

	// MarkUnsafe writes the mark of ref's life, fenced. Idempotent for the
	// same claim. ErrMarkedByAnotherClaim when another claim of the life
	// wrote the mark; ErrTaskBusy when an open transaction has written the
	// row (C6). Serialised with ClaimDue (C3). Never joining.
	MarkUnsafe(ctx context.Context, ref TaskRef) error

	// RecordAttempt sets the task WAITING with a.NextAttemptTime, adds 1
	// to Attempts unless a.NotCounted, records a.Error and a.AtMs as
	// LastError and LastAttemptTime (with or without NotCounted), and
	// clears the claim, fenced. With
	// a.ClearOwnMark it also removes the mark this claim wrote, in the
	// same atomic write; a mark another claim wrote stays. It may return
	// ErrTaskBusy when an open transaction has written the row (C6); the
	// write did not happen, and the caller retries it. Never joining.
	RecordAttempt(ctx context.Context, ref TaskRef, a Attempt) error

	// Fail sets the task FAILED with f.Reason, replaces LastError with
	// f.Error (even when it is empty), records f.AtMs as FailedTime, and
	// clears the claim, fenced. LastAttemptTime, Attempts and LostOwners
	// are unchanged.
	// Joining, so the failure commits with its audit event.
	Fail(ctx context.Context, ref TaskRef, f Failure) error

	// SweepMarks removes the marks of lives that have ended: the task was
	// removed or re-armed. A mark of a task's current life, in any status,
	// stays. Never joining.
	SweepMarks(ctx context.Context) error
}
```

`types.go`: replace `:358-370` (the `ScheduledTask` doc comment through the end
of the `ID` doc comment) with:

```go
// ScheduledTask is a durable "do something at ScheduledTime, with
// TimeoutMs lateness tolerance" record. For fire-transition, the
// payload fields identify the entity+transition to fire.
//
// A task has lives. Every arm (ScheduledTaskStore.ReconcileForEntity)
// starts a new life with a new ArmToken, whatever the status it replaces.
// The fields from Status on are the store's: the store sets them on arm
// and on each state change, and ignores whatever a caller puts in them.
type ScheduledTask struct {
	// ID is deterministic and engine-defined: the same
	// (tenant, entity, source state, transition) always derives the same
	// ID, so re-arming a still-scheduled transition replaces the existing
	// row, as a new life, instead of creating a duplicate. Tenant and
	// entity are incorporated so IDs can never collide across tenants or
	// entities. The exact derivation (hash inputs, encoding) is an
	// engine-internal detail, not part of this SPI's contract — stores
	// must treat ID as an opaque, stable key.
```

Delete `types.go:378-380` (the `RedispatchAfter` comment and field). Replace
`:389-390` with:

```go
	ArmedAt int64 `json:"armedAt,omitempty"`
```

`search_store.go:163-164` becomes:

```go
	// background/tenant-less context, as with ScheduledTaskStore.ClaimDue.
```

and `search_store.go:193` becomes:

```go
	// context, as with ScheduledTaskStore.ClaimDue.
```

Delete the root conformance function:

```
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi rm scheduled_task_store_conformance.go
```

- [ ] **Step 4: Run and see them pass**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && gofmt -l . && go vet ./... && go test .`
Expected: `gofmt -l` prints nothing; PASS.

- [ ] **Step 5: Commit**

```
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi add persistence.go persistence_test.go types.go types_test.go search_store.go
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi commit -m "feat(scheduled-task)!: every scheduled run has one owner

ScheduledTaskStore is replaced: arm as a new life, fenced stamp, mark,
attempt and fail, claim with owner liveness, give-back, sweepers and a
tenant-scoped query. Clauses C1-C6 are on the interface. The redispatch
throttle, its counter, ScanDue, MarkRedispatch, Upsert, Delete and the
root-package conformance function are removed.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

(`git rm` already staged the deletion.)

---

### Task S-3a: Shared backend helpers — claim selection and task-write validation

**Spec:** §6.1 (limits, one task per entity, tenants take turns); §5.6
"Every store sets it" (`ErrStoreRejected`); §5.8 (at most 1 024 bytes, valid
UTF-8, no NUL). README C-P2.

**Why in the SPI.** The memory and SQLite stores choose their claims in Go
and validate the same inputs with the same rules. One copy per backend would
be two copies of production logic that must never differ. The SPI is the
only module both plugins import, and it already holds shared backend helpers
(`default_save_all.go`, `filter_match.go`). PostgreSQL ranks its claims in
SQL and meets the same rules there; it may call the validators, but need not.

**Files:**
- Create: `scheduled_task_helpers.go`
- Test: `scheduled_task_helpers_test.go` (package `spi`)

**Interfaces:**
- Consumes: S-1 `ErrStoreRejected`; S-2 `ScheduledTask`, `ClaimRequest`,
  `ScheduledTaskFailureReason` and its five constants; S-3 `ReconcileRequest`.
- Produces:
  ```go
  const MaxTaskErrorBytes = 1024
  func SelectClaims(cands []ScheduledTask, req ClaimRequest) []ScheduledTask
  func ValidateTaskErrorText(s string) error
  func ValidateFailureReason(r ScheduledTaskFailureReason) error
  func ValidateArm(req ReconcileRequest) error
  ```
  BM-2 and BQ-2 call `SelectClaims`; BM-5 and BQ-6 call the three validators.

- [ ] **Step 1: Write the failing tests** — `scheduled_task_helpers_test.go`:

```go
package spi

import (
	"errors"
	"strings"
	"testing"
)

func cand(tenant TenantID, entity, id string, next int64) ScheduledTask {
	return ScheduledTask{ID: id, TenantID: tenant, EntityID: entity, NextAttemptTime: next}
}

func claimIDs(ts []ScheduledTask) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

func TestSelectClaims_OnePerEntity(t *testing.T) {
	got := SelectClaims([]ScheduledTask{
		cand("A", "e1", "e1:S:T2", 20),
		cand("A", "e1", "e1:S:T1", 10),
		cand("A", "e2", "e2:S:T", 30),
	}, ClaimRequest{Limit: 10, PerTenantLimit: 10})
	if want := "e1:S:T1,e2:S:T"; strings.Join(claimIDs(got), ",") != want {
		t.Fatalf("claimed %v, want %s: the earliest task of each entity, one per entity", claimIDs(got), want)
	}
}

func TestSelectClaims_WithinATenantByNextAttemptTimeThenID(t *testing.T) {
	got := SelectClaims([]ScheduledTask{
		cand("A", "e3", "c", 10),
		cand("A", "e2", "b", 5),
		cand("A", "e1", "a", 10),
	}, ClaimRequest{Limit: 10, PerTenantLimit: 10})
	if want := "b,a,c"; strings.Join(claimIDs(got), ",") != want {
		t.Fatalf("claimed %v, want %s", claimIDs(got), want)
	}
}

// Tenants take turns: each tenant's first task comes before any tenant's
// second. The tenant with the earliest candidate goes first; ties go to the
// lower tenant id.
func TestSelectClaims_TenantsTakeTurns(t *testing.T) {
	cands := []ScheduledTask{
		cand("A", "a1", "a1", 1), cand("A", "a2", "a2", 2), cand("A", "a3", "a3", 3),
		cand("B", "b1", "b1", 5), cand("B", "b2", "b2", 6),
		cand("C", "c1", "c1", 5),
	}
	got := SelectClaims(cands, ClaimRequest{Limit: 5, PerTenantLimit: 10})
	if want := "a1,b1,c1,a2,b2"; strings.Join(claimIDs(got), ",") != want {
		t.Fatalf("claimed %v, want %s", claimIDs(got), want)
	}
	got = SelectClaims(cands, ClaimRequest{Limit: 2, PerTenantLimit: 10})
	if want := "a1,b1"; strings.Join(claimIDs(got), ",") != want {
		t.Fatalf("with Limit 2 claimed %v, want %s", claimIDs(got), want)
	}
}

func TestSelectClaims_PerTenantLimitCountsRunsInProgress(t *testing.T) {
	cands := []ScheduledTask{
		cand("A", "a1", "a1", 1), cand("A", "a2", "a2", 2), cand("A", "a3", "a3", 3),
		cand("B", "b1", "b1", 4),
		cand("C", "c1", "c1", 5),
	}
	got := SelectClaims(cands, ClaimRequest{
		Limit: 10, PerTenantLimit: 2,
		TenantInProgress: map[TenantID]int{"A": 1, "C": 3},
	})
	if want := "a1,b1"; strings.Join(claimIDs(got), ",") != want {
		t.Fatalf("claimed %v, want %s: A has room for one, B for two, C for none", claimIDs(got), want)
	}
}

func TestSelectClaims_NoCandidates(t *testing.T) {
	if got := SelectClaims(nil, ClaimRequest{Limit: 1, PerTenantLimit: 1}); len(got) != 0 {
		t.Fatalf("claimed %v from no candidates", claimIDs(got))
	}
}

func TestValidateTaskErrorText(t *testing.T) {
	const secret = "do-not-echo"
	for name, text := range map[string]string{
		"empty":                     "",
		"1 024 bytes, 2-byte runes": strings.Repeat("é", 512),
	} {
		if err := ValidateTaskErrorText(text); err != nil {
			t.Errorf("%s: %v, want nil", name, err)
		}
	}
	for name, text := range map[string]string{
		"NUL":             secret + "\x00",
		"invalid UTF-8":   secret + "\xff",
		"over 1024 bytes": secret + strings.Repeat("x", MaxTaskErrorBytes),
	} {
		err := ValidateTaskErrorText(text)
		if !errors.Is(err, ErrStoreRejected) {
			t.Errorf("%s: err = %v, want ErrStoreRejected", name, err)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the rejection repeats the rejected text: %v", name, err)
		}
	}
}

func TestValidateFailureReason(t *testing.T) {
	for _, r := range []ScheduledTaskFailureReason{
		FailureUnsafeWorkNotCompleted, FailureOwnerLostRepeatedly,
		FailureExpiredAfterFailedAttempts, FailureRunPanicked, FailureStoppedAfterPartialCommit,
	} {
		if err := ValidateFailureReason(r); err != nil {
			t.Errorf("%s: %v, want nil", r, err)
		}
	}
	for _, r := range []ScheduledTaskFailureReason{"", "NOT_A_REASON"} {
		if err := ValidateFailureReason(r); !errors.Is(err, ErrStoreRejected) {
			t.Errorf("%q: err = %v, want ErrStoreRejected", r, err)
		}
	}
}

func TestValidateArm(t *testing.T) {
	ok := ReconcileRequest{TenantID: "A", EntityID: "e1", Arm: []ScheduledTask{{ID: "e1:S:T"}}}
	if err := ValidateArm(ok); err != nil {
		t.Fatalf("an arm with ids: %v", err)
	}
	if err := ValidateArm(ReconcileRequest{TenantID: "A", EntityID: "e1"}); err != nil {
		t.Fatalf("an empty arm: %v", err)
	}
	noID := ReconcileRequest{TenantID: "A", EntityID: "e1", Arm: []ScheduledTask{{ID: "e1:S:T"}, {}}}
	if err := ValidateArm(noID); !errors.Is(err, ErrStoreRejected) {
		t.Fatalf("an arm task without an id: err = %v, want ErrStoreRejected", err)
	}
}
```

- [ ] **Step 2: Run them and see them fail**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test . -run 'TestSelectClaims_|TestValidateTaskErrorText|TestValidateFailureReason|TestValidateArm'`
Expected: FAIL — build error `undefined: SelectClaims`, `undefined: ValidateTaskErrorText`,
`undefined: MaxTaskErrorBytes`, `undefined: ValidateFailureReason`, `undefined: ValidateArm`.

- [ ] **Step 3: Implement** — `scheduled_task_helpers.go`:

```go
package spi

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Helpers for ScheduledTaskStore implementations. A backend that chooses its
// claims in Go, or validates its input before it writes, uses these so that
// every backend applies the same rules. A backend that meets a rule in its
// query language instead need not call them.

// MaxTaskErrorBytes is the most a recorded scheduled-task error text
// (Attempt.Error, Failure.Error) may take, in bytes.
const MaxTaskErrorBytes = 1024

// SelectClaims picks the tasks one ScheduledTaskStore.ClaimDue call takes from
// cands, the tasks the store found claimable. At most one task per entity; at
// most req.PerTenantLimit - req.TenantInProgress[tenant] per tenant; at most
// req.Limit in all. Within a tenant the order is (NextAttemptTime, ID).
// Tenants take turns, one task per turn, so each tenant's first task comes
// before any tenant's second; the tenant with the earliest candidate goes
// first, ties broken by tenant id. SelectClaims sorts cands in place.
func SelectClaims(cands []ScheduledTask, req ClaimRequest) []ScheduledTask {
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.NextAttemptTime != b.NextAttemptTime {
			return a.NextAttemptTime < b.NextAttemptTime
		}
		if a.TenantID != b.TenantID {
			return a.TenantID < b.TenantID
		}
		return a.ID < b.ID
	})
	var tenants []TenantID
	queues := make(map[TenantID][]ScheduledTask)
	for _, c := range cands {
		if _, ok := queues[c.TenantID]; !ok {
			tenants = append(tenants, c.TenantID)
		}
		queues[c.TenantID] = append(queues[c.TenantID], c)
	}
	quota := make(map[TenantID]int, len(tenants))
	for _, tn := range tenants {
		quota[tn] = req.PerTenantLimit - req.TenantInProgress[tn]
	}

	type entityKey struct {
		tenant TenantID
		id     string
	}
	seen := make(map[entityKey]bool)
	var out []ScheduledTask
	for progress := true; progress && len(out) < req.Limit; {
		progress = false
		for _, tn := range tenants {
			if len(out) >= req.Limit {
				break
			}
			for quota[tn] > 0 && len(queues[tn]) > 0 {
				c := queues[tn][0]
				queues[tn] = queues[tn][1:]
				ek := entityKey{tenant: tn, id: c.EntityID}
				if seen[ek] {
					continue
				}
				seen[ek] = true
				quota[tn]--
				out = append(out, c)
				progress = true
				break
			}
		}
	}
	return out
}

// ValidateTaskErrorText refuses an error text that no backend stores as
// given: over MaxTaskErrorBytes, not valid UTF-8, or holding a NUL
// (PostgreSQL refuses the last two with SQLSTATE 22021). The error satisfies
// errors.Is(err, ErrStoreRejected) and never repeats the text, which may
// carry anything a compute node sent.
func ValidateTaskErrorText(s string) error {
	switch {
	case len(s) > MaxTaskErrorBytes:
		return fmt.Errorf("scheduled task error text is %d bytes, over %d: %w", len(s), MaxTaskErrorBytes, ErrStoreRejected)
	case !utf8.ValidString(s):
		return fmt.Errorf("scheduled task error text is not valid UTF-8: %w", ErrStoreRejected)
	case strings.IndexByte(s, 0) >= 0:
		return fmt.Errorf("scheduled task error text contains NUL: %w", ErrStoreRejected)
	}
	return nil
}

// ValidateFailureReason refuses a reason that is not one of the five
// ScheduledTaskFailureReason constants, with an error that satisfies
// errors.Is(err, ErrStoreRejected).
func ValidateFailureReason(r ScheduledTaskFailureReason) error {
	switch r {
	case FailureUnsafeWorkNotCompleted, FailureOwnerLostRepeatedly,
		FailureExpiredAfterFailedAttempts, FailureRunPanicked,
		FailureStoppedAfterPartialCommit:
		return nil
	}
	return fmt.Errorf("scheduled task failure reason is not a known reason: %w", ErrStoreRejected)
}

// ValidateArm refuses a ReconcileRequest whose Arm names a task without an
// id, with an error that satisfies errors.Is(err, ErrStoreRejected). The
// tenant and the entity come from the request (see ReconcileRequest.Arm), so
// an Arm item's own fields for them are not checked.
func ValidateArm(req ReconcileRequest) error {
	for _, a := range req.Arm {
		if a.ID == "" {
			return fmt.Errorf("scheduled task arm for entity %s names a task without an id: %w", req.EntityID, ErrStoreRejected)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run them and see them pass**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && gofmt -l . && go vet ./... && go test .`
Expected: `gofmt -l` prints nothing; PASS.

- [ ] **Step 5: Commit**

```
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi add scheduled_task_helpers.go scheduled_task_helpers_test.go
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi commit -m "feat(scheduled-task): shared claim selection and task-write validation

SelectClaims picks one ClaimDue call's tasks: one per entity, per-tenant
limits, tenants taking turns. ValidateTaskErrorText, ValidateFailureReason
and ValidateArm refuse what no backend stores, with ErrStoreRejected.
Backends that choose or validate in Go call them, so the rules exist once.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task S-4: `spitest` ScheduledTasks group — harness, arm, remove, delete, get

**Spec:** §10.1 "Conformance"; §7 "Arm", "Cancel", entity-delete table; §13
rows listed in the coverage table.

**Files:**
- Create: `spitest/scheduledtasks.go`
- Create: `spitest/scheduledtasks_lifecycle.go`
- Modify: `spitest/spitest.go` (`:156`)
- Modify: `spitest/doc.go` (`:6-8`, `:15`)
- Modify: `spitest/README.md` (`:13-15`)

**Interfaces:**
- Consumes: S-3's interface; the harness (`runSubtest`, `tenantContext`,
  `newID`, `skipTracker`, `Harness.Factory`, `Harness.NewTenant`,
  `Harness.AdvanceClock`).
- Produces:
  `func runScheduledTasksSuite(t *testing.T, h Harness, tracker *skipTracker)`,
  registered as `t.Run("ScheduledTasks", …)`; the fixture `stFixture` and its
  helpers, used by S-5..S-8. Subtest names under `ScheduledTasks/` are the
  `Harness.Skip` keys a backend may use. `Arm/TenantAndEntityFromRequest`
  pins that `ReconcileForEntity` takes the tenant and the entity from the
  request, never from an `Arm` item; `DeleteForModel/NilKeepRemovesAll` pins
  that a nil `keep` removes every task of that model version.

How the group gets a store, a tenant and a transaction: each subtest calls
`newSTFixture(t, h)`, which draws a tenant from `h.NewTenant()`, builds
`tenantContext(tenant)`, and takes `h.Factory.ScheduledTaskStore(ctx)` and
`h.Factory.TransactionManager(ctx)`. Cross-tenant calls (`ClaimDue`,
`GiveBackIdle`, owner and sweep methods) use `context.Background()`.
`f.begin()` opens "an open transaction" through the harness's
`TransactionManager`; joining calls made with its `txCtx` are staged in it
until `Commit` or `Rollback`, and a cleanup rolls it back if the test did not
end it. Tests that need the transaction to have begun before another write
read through it first (fact 13).

A backend whose `StoreFactory.ScheduledTaskStore` returns an error satisfying
`errors.Is(err, errors.ErrUnsupported)` skips the whole group (spec §10.1,
"not implemented"). Any other error fails it.

- [ ] **Step 1: Write the failing registration**

`spitest/spitest.go`, after the `AsyncSearch` line (`:156`):

```go
	t.Run("ScheduledTasks", func(t *testing.T) { runScheduledTasksSuite(t, h, tracker) })
```

Create `spitest/scheduledtasks.go`:

```go
package spitest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Clock values the suite passes as ClaimRequest.NowMs and arms tasks at.
// NextAttemptTime is compared with the caller's NowMs, never with the store
// clock, so these are plain numbers.
const (
	stDue    int64 = 1_000_000         // a task armed at stDue is due at stNow
	stNow    int64 = 2_000_000         // NowMs of every suite claim
	stFuture int64 = 9_000_000_000_000 // a task armed at stFuture is never due
)

const (
	// stShortStale is the StaleAfter a test uses when a heartbeat must go
	// stale: AdvanceClock(3 * stShortStale) passes it. 60 ms stays under
	// the 100 ms ceiling the postgres harness puts on AdvanceClock.
	stShortStale = 20 * time.Millisecond
	// stLongStale is the StaleAfter a test uses when a heartbeat must stay
	// fresh. An owner with no liveness record is stale under any
	// StaleAfter, so a lost-owner claim with stLongStale takes exactly the
	// tasks whose owner never heartbeated.
	stLongStale = time.Hour
	// stWait bounds a call that must not wait on another transaction.
	stWait = 10 * time.Second
	// stBigLimit is the Limit and PerTenantLimit of a claim that is not
	// about limits.
	stBigLimit = 1000
)

func runScheduledTasksSuite(t *testing.T, h Harness, tracker *skipTracker) {
	_, err := h.Factory.ScheduledTaskStore(context.Background())
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skipf("backend has no ScheduledTaskStore: %v", err)
	}
	require.NoError(t, err)

	// Arm, remove, delete, get (S-4).
	runSubtest(t, h, tracker, "Arm/NewLife", testSTArmNewLife)
	runSubtest(t, h, tracker, "Arm/TenantAndEntityFromRequest", testSTArmTenantAndEntityFromRequest)
	runSubtest(t, h, tracker, "Arm/EveryArmIsNewLife", testSTArmEveryArmIsNewLife)
	runSubtest(t, h, tracker, "Arm/RemovesOtherTasks", testSTArmRemovesOtherTasks)
	runSubtest(t, h, tracker, "Arm/CancelNotReported", testSTArmCancelNotReported)
	runSubtest(t, h, tracker, "Arm/JoinsTransaction", testSTArmJoinsTransaction)
	runSubtest(t, h, tracker, "Arm/SelfLoopRearmsRunning", testSTArmSelfLoopRearmsRunning)
	runSubtest(t, h, tracker, "Arm/RearmResetsLife", testSTArmRearmResetsLife)
	runSubtest(t, h, tracker, "RemoveLife/CurrentLife", testSTRemoveLifeCurrentLife)
	runSubtest(t, h, tracker, "RemoveLife/OtherLifeIsNoOp", testSTRemoveLifeOtherLifeIsNoOp)
	runSubtest(t, h, tracker, "RemoveLife/JoinsTransaction", testSTRemoveLifeJoinsTransaction)
	runSubtest(t, h, tracker, "DeleteForEntities/RemovesListed", testSTDeleteForEntitiesRemovesListed)
	runSubtest(t, h, tracker, "DeleteForEntities/JoinsTransaction", testSTDeleteForEntitiesJoinsTransaction)
	runSubtest(t, h, tracker, "DeleteForModel/Keep", testSTDeleteForModelKeep)
	runSubtest(t, h, tracker, "DeleteForModel/NilKeepRemovesAll", testSTDeleteForModelNilKeep)
	runSubtest(t, h, tracker, "DeleteForModel/TenantScoped", testSTDeleteForModelTenantScoped)
	runSubtest(t, h, tracker, "Get/Missing", testSTGetMissing)
}

// stFixture is one subtest's tenant, store, transaction manager and model.
//
// ClaimDue, GiveBackIdle and the sweepers are cross-tenant, so a fresh
// tenant alone does not isolate a subtest. The fixture removes every task
// it armed when the subtest ends, so a cross-tenant claim sees only the
// running subtest's tasks. Tests still read their own tenant's results
// only (own), so a task another subtest failed to remove cannot fail them.
type stFixture struct {
	t        *testing.T
	h        Harness
	tenant   spi.TenantID
	ctx      context.Context // tenant context, no transaction
	sts      spi.ScheduledTaskStore
	tm       spi.TransactionManager
	model    string
	entities []string
}

func newSTFixture(t *testing.T, h Harness) *stFixture {
	t.Helper()
	f := &stFixture{t: t, h: h, tenant: h.NewTenant(), model: "st-" + uuid.NewString()}
	f.ctx = tenantContext(f.tenant)
	var err error
	f.sts, err = h.Factory.ScheduledTaskStore(f.ctx)
	require.NoError(t, err)
	f.tm, err = h.Factory.TransactionManager(f.ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		if len(f.entities) == 0 {
			return
		}
		if err := f.sts.DeleteForEntities(f.ctx, f.tenant, f.entities); err != nil {
			t.Errorf("cleanup: DeleteForEntities: %v", err)
		}
	})
	return f
}

// begin opens a transaction in the fixture's tenant. It is rolled back when
// the subtest ends unless the test ended it: an open transaction would hold
// task-row locks on PostgreSQL and block the fixture's cleanup. Cleanups
// run last-in first-out, so this rollback runs before the fixture's.
func (f *stFixture) begin() (string, context.Context) {
	f.t.Helper()
	txID, txCtx, err := f.tm.Begin(f.ctx)
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _ = f.tm.Rollback(txCtx, txID) })
	return txID, txCtx
}

func (f *stFixture) newEntity() string {
	e := newID()
	f.entities = append(f.entities, e)
	return e
}

// taskID is a stable id for (entity, state, transition). Real ids are
// engine-defined hashes; stores treat them as opaque.
func (f *stFixture) taskID(entity, state, transition string) string {
	return fmt.Sprintf("st:%s:%s:%s:%s", f.tenant, entity, state, transition)
}

// spec is an arm request for one transition of entity out of state.
func (f *stFixture) spec(entity, state, transition string, scheduledTime int64) spi.ScheduledTask {
	return spi.ScheduledTask{
		ID: f.taskID(entity, state, transition), TenantID: f.tenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: scheduledTime, EntityID: entity, ModelName: f.model, ModelVersion: 1,
		Transition: transition, SourceState: state, ArmedAt: scheduledTime - 1,
	}
}

func (f *stFixture) reconcile(ctx context.Context, entity, state string, arm ...spi.ScheduledTask) []spi.ScheduledTask {
	f.t.Helper()
	removed, err := f.sts.ReconcileForEntity(ctx, spi.ReconcileRequest{
		TenantID: f.tenant, EntityID: entity, CurrentState: state, Arm: arm})
	require.NoError(f.t, err)
	return removed
}

// arm arms one task on a new entity and returns the stored record.
func (f *stFixture) arm(scheduledTime int64) spi.ScheduledTask {
	f.t.Helper()
	e := f.newEntity()
	s := f.spec(e, "S", "T", scheduledTime)
	f.reconcile(f.ctx, e, "S", s)
	return f.mustGet(s.ID)
}

func (f *stFixture) armDue() spi.ScheduledTask { return f.arm(stDue) }

func (f *stFixture) get(ctx context.Context, id string) (spi.ScheduledTask, bool) {
	f.t.Helper()
	got, found, err := f.sts.Get(ctx, f.tenant, id)
	require.NoError(f.t, err)
	if !found {
		require.Nil(f.t, got, "Get must return a nil task when found is false")
		return spi.ScheduledTask{}, false
	}
	require.NotNil(f.t, got)
	return *got, true
}

func (f *stFixture) mustGet(id string) spi.ScheduledTask {
	f.t.Helper()
	got, found := f.get(f.ctx, id)
	require.True(f.t, found, "task %s must exist", id)
	return got
}

func (f *stFixture) requireGone(id string) {
	f.t.Helper()
	_, found := f.get(f.ctx, id)
	require.False(f.t, found, "task %s must be gone", id)
}

func (f *stFixture) claimReq(owner uuid.UUID, lost bool) spi.ClaimRequest {
	return spi.ClaimRequest{Owner: owner, NowMs: stNow, StaleAfter: stLongStale,
		Limit: stBigLimit, PerTenantLimit: stBigLimit, AllowLostOwner: lost}
}

// claimWith runs ClaimDue with a tenant-less context and returns this
// fixture's tasks only.
func (f *stFixture) claimWith(req spi.ClaimRequest) []spi.ScheduledTask {
	f.t.Helper()
	res, err := f.sts.ClaimDue(context.Background(), req)
	require.NoError(f.t, err)
	return f.own(res)
}

func (f *stFixture) own(tasks []spi.ScheduledTask) []spi.ScheduledTask {
	var out []spi.ScheduledTask
	for _, x := range tasks {
		if x.TenantID == f.tenant {
			out = append(out, x)
		}
	}
	return out
}

// claimTask claims the due WAITING task id as owner. It requires that the
// claim takes exactly that task of this tenant: a test that leaves another
// task claimable has a set-up error.
func (f *stFixture) claimTask(owner uuid.UUID, id string) spi.ScheduledTask {
	f.t.Helper()
	return f.claimOnly(f.claimReq(owner, false), id)
}

// reclaim takes the RUNNING task id as owner through a lost-owner claim.
// With stLongStale it succeeds only when the old owner has no liveness
// record — the suite's owners never heartbeat unless a test says so.
func (f *stFixture) reclaim(owner uuid.UUID, id string) spi.ScheduledTask {
	f.t.Helper()
	return f.claimOnly(f.claimReq(owner, true), id)
}

func (f *stFixture) claimOnly(req spi.ClaimRequest, id string) spi.ScheduledTask {
	f.t.Helper()
	got := f.claimWith(req)
	require.Len(f.t, got, 1, "the claim must take exactly task %s of this tenant", id)
	c := got[0]
	require.Equal(f.t, id, c.ID)
	require.Equal(f.t, spi.ScheduledTaskRunning, c.Status)
	require.NotNil(f.t, c.Claim, "a claimed task carries its claim")
	require.Equal(f.t, req.Owner, c.Claim.Owner)
	require.NotEqual(f.t, uuid.Nil, c.Claim.Token)
	return c
}

func findST(tasks []spi.ScheduledTask, id string) *spi.ScheduledTask {
	for i := range tasks {
		if tasks[i].ID == id {
			return &tasks[i]
		}
	}
	return nil
}

func stIDs(tasks []spi.ScheduledTask) []string {
	out := make([]string, 0, len(tasks))
	for _, x := range tasks {
		out = append(out, x.ID)
	}
	return out
}

// stRef is the TaskRef of a claimed task.
func stRef(c spi.ScheduledTask) spi.TaskRef {
	return spi.TaskRef{TenantID: c.TenantID, ID: c.ID, ArmToken: c.ArmToken, ClaimToken: c.Claim.Token}
}

// stFencedWrites calls each fenced method once with r.
var stFencedWrites = []struct {
	name string
	call func(ctx context.Context, sts spi.ScheduledTaskStore, r spi.TaskRef) error
}{
	{"StampSegment", func(ctx context.Context, sts spi.ScheduledTaskStore, r spi.TaskRef) error {
		return sts.StampSegment(ctx, r, true)
	}},
	{"MarkUnsafe", func(ctx context.Context, sts spi.ScheduledTaskStore, r spi.TaskRef) error {
		return sts.MarkUnsafe(ctx, r)
	}},
	{"RecordAttempt", func(ctx context.Context, sts spi.ScheduledTaskStore, r spi.TaskRef) error {
		return sts.RecordAttempt(ctx, r, spi.Attempt{Error: "E", AtMs: stNow, NextAttemptTime: stNow})
	}},
	{"Fail", func(ctx context.Context, sts spi.ScheduledTaskStore, r spi.TaskRef) error {
		return sts.Fail(ctx, r, spi.Failure{Reason: spi.FailureRunPanicked, Error: "E", AtMs: stNow})
	}},
}

func requireAllFencedRefused(t *testing.T, ctx context.Context, sts spi.ScheduledTaskStore, r spi.TaskRef, why string) {
	t.Helper()
	for _, m := range stFencedWrites {
		require.ErrorIs(t, m.call(ctx, sts, r), spi.ErrStaleClaim, "%s must refuse %s", m.name, why)
	}
}

// requireUnchangedClaim asserts a RUNNING task is exactly as its claim
// left it.
func requireUnchangedClaim(t *testing.T, want, got spi.ScheduledTask) {
	t.Helper()
	require.Equal(t, spi.ScheduledTaskRunning, got.Status)
	require.Equal(t, want.ArmToken, got.ArmToken)
	require.NotNil(t, got.Claim)
	require.Equal(t, want.Claim.Token, got.Claim.Token)
	require.Equal(t, want.Claim.Owner, got.Claim.Owner)
	require.Equal(t, want.Attempts, got.Attempts)
	require.Equal(t, want.LostOwners, got.LostOwners)
	require.Equal(t, want.PartialCommit, got.PartialCommit)
	require.Equal(t, want.UnsafeMarked, got.UnsafeMarked)
	require.Empty(t, got.FailureReason)
}
```

- [ ] **Step 2: Run it and see it fail**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go vet ./spitest/`
Expected: FAIL — `undefined: testSTArmNewLife` (and the other `testST…`
functions registered above).

- [ ] **Step 3: Implement** — create `spitest/scheduledtasks_lifecycle.go`:

```go
package spitest

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// requireFreshLife asserts got is a new life armed from s.
func requireFreshLife(t *testing.T, s, got spi.ScheduledTask) {
	t.Helper()
	require.Equal(t, spi.ScheduledTaskWaiting, got.Status)
	require.NotEqual(t, uuid.Nil, got.ArmToken, "the store draws an arm token on every arm")
	require.Equal(t, s.ScheduledTime, got.NextAttemptTime, "a new life is due at its scheduled time")
	require.Zero(t, got.Attempts)
	require.Zero(t, got.LostOwners)
	require.Nil(t, got.LastAttemptTime)
	require.Empty(t, got.LastError)
	require.Empty(t, got.FailureReason)
	require.Nil(t, got.FailedTime)
	require.False(t, got.PartialCommit)
	require.Nil(t, got.Claim)
	require.False(t, got.UnsafeMarked)
}

func testSTArmNewLife(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	e := f.newEntity()
	timeout := int64(5000)
	s := f.spec(e, "S", "T", stDue)
	s.TimeoutMs = &timeout
	s.ArmedBy = spi.Principal{ID: "svc-arm", Kind: spi.PrincipalService}

	// The life fields are the store's: whatever the caller puts in them is
	// ignored.
	callerToken := uuid.New()
	last := int64(5)
	caller := s
	caller.Status = spi.ScheduledTaskFailed
	caller.ArmToken = callerToken
	caller.NextAttemptTime = stFuture
	caller.Attempts = 7
	caller.LostOwners = 2
	caller.LastAttemptTime = &last
	caller.LastError = "caller"
	caller.FailureReason = spi.FailureRunPanicked
	caller.FailedTime = &last
	caller.PartialCommit = true
	caller.Claim = &spi.TaskClaim{Token: uuid.New(), Owner: uuid.New()}
	caller.UnsafeMarked = true

	require.Empty(t, f.reconcile(f.ctx, e, "S", caller))
	got := f.mustGet(s.ID)
	requireFreshLife(t, s, got)
	require.NotEqual(t, callerToken, got.ArmToken, "the arm token is drawn by the store, not taken from the caller")

	require.Equal(t, s.ID, got.ID)
	require.Equal(t, f.tenant, got.TenantID)
	require.Equal(t, spi.ScheduledTaskFireTransition, got.Type)
	require.Equal(t, s.ScheduledTime, got.ScheduledTime)
	require.NotNil(t, got.TimeoutMs)
	require.Equal(t, timeout, *got.TimeoutMs)
	require.Equal(t, e, got.EntityID)
	require.Equal(t, f.model, got.ModelName)
	require.Equal(t, 1, got.ModelVersion)
	require.Equal(t, "S", got.SourceState)
	require.Equal(t, "T", got.Transition)
	require.Equal(t, s.ArmedAt, got.ArmedAt)
	require.Equal(t, s.ArmedBy, got.ArmedBy)

	// A zero ArmedBy stays the zero Principal; a nil TimeoutMs stays nil.
	e2 := f.newEntity()
	s2 := f.spec(e2, "S", "T", stDue)
	f.reconcile(f.ctx, e2, "S", s2)
	got2 := f.mustGet(s2.ID)
	require.Equal(t, spi.Principal{}, got2.ArmedBy)
	require.Nil(t, got2.TimeoutMs)
}

// ReconcileForEntity takes the tenant and the entity from the request,
// never from an Arm item.
func testSTArmTenantAndEntityFromRequest(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	fb := newSTFixture(t, h)
	e := f.newEntity()
	other := f.arm(stFuture) // another entity of the same tenant
	s := f.spec(e, "S", "T", stFuture)
	s.TenantID = fb.tenant
	s.EntityID = other.EntityID
	require.Empty(t, f.reconcile(f.ctx, e, "S", s))

	got := f.mustGet(s.ID)
	require.Equal(t, f.tenant, got.TenantID)
	require.Equal(t, e, got.EntityID)
	_, found := fb.get(fb.ctx, s.ID)
	require.False(t, found, "the task is not armed in the tenant an Arm item names")
	f.mustGet(other.ID) // the entity an Arm item names keeps its task

	// The task is the request entity's: that entity's next write removes it.
	require.Equal(t, []string{s.ID}, stIDs(f.reconcile(f.ctx, e, "S")))
}

func testSTArmEveryArmIsNewLife(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	first := f.arm(stFuture)
	s := f.spec(first.EntityID, "S", "T", stFuture)
	require.Empty(t, f.reconcile(f.ctx, first.EntityID, "S", s), "re-arming an id replaces it; it is not removed")
	second := f.mustGet(s.ID)
	requireFreshLife(t, s, second)
	require.NotEqual(t, first.ArmToken, second.ArmToken, "every arm starts a new life, even with an identical payload")
}

func testSTArmRemovesOtherTasks(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	e := f.newEntity()
	other := f.newEntity()
	a := f.spec(e, "S", "T1", stFuture)
	b := f.spec(e, "S", "T2", stFuture)
	c := f.spec(e, "S0", "T0", stFuture)
	o := f.spec(other, "S", "T1", stFuture)

	require.Empty(t, f.reconcile(f.ctx, e, "S0", c))
	cArmed := f.mustGet(c.ID)
	f.reconcile(f.ctx, other, "S", o)

	// The entity moved S0 -> S: the S0 task is removed and reported.
	removed := f.reconcile(f.ctx, e, "S", a, b)
	require.Equal(t, []string{c.ID}, stIDs(removed))
	require.Equal(t, cArmed.ArmToken, removed[0].ArmToken)
	require.Equal(t, e, removed[0].EntityID)
	require.Equal(t, "S0", removed[0].SourceState)
	require.Equal(t, "T0", removed[0].Transition)
	f.requireGone(c.ID)

	// T2 is no longer scheduled in S: the next write removes it.
	removed = f.reconcile(f.ctx, e, "S", a)
	require.Equal(t, []string{b.ID}, stIDs(removed))
	f.requireGone(b.ID)
	f.mustGet(a.ID)
	f.mustGet(o.ID) // another entity's task is never touched
}

func testSTArmCancelNotReported(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	e := f.newEntity()
	x := f.spec(e, "S", "TX", stFuture)
	y := f.spec(e, "S", "TY", stFuture)
	f.reconcile(f.ctx, e, "S", x, y)

	removed, err := f.sts.ReconcileForEntity(f.ctx, spi.ReconcileRequest{
		TenantID: f.tenant, EntityID: e, CurrentState: "S",
		Cancel: []string{x.ID, "st-does-not-exist-" + newID()}})
	require.NoError(t, err, "a Cancel id that does not exist is a no-op")
	require.Equal(t, []string{y.ID}, stIDs(removed),
		"a task named in Cancel is removed but not reported; every other removed task is")
	f.requireGone(x.ID)
	f.requireGone(y.ID)
}

func testSTArmJoinsTransaction(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	e := f.newEntity()
	s := f.spec(e, "S", "T", stFuture)

	txID, txCtx := f.begin()
	f.reconcile(txCtx, e, "S", s)
	require.NoError(t, f.tm.Rollback(txCtx, txID))
	f.requireGone(s.ID)

	txID, txCtx = f.begin()
	f.reconcile(txCtx, e, "S", s)
	require.NoError(t, f.tm.Commit(txCtx, txID))
	requireFreshLife(t, s, f.mustGet(s.ID))
}

// A self-loop fires and re-arms the same id as a new life (spec §13 row
// "self-loop fires and re-arms the same id as a new life").
func testSTArmSelfLoopRearmsRunning(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)

	txID, txCtx := f.begin()
	s := f.spec(task.EntityID, "S", "T", stFuture)
	require.Empty(t, f.reconcile(txCtx, task.EntityID, "S", s))
	// The run's own RemoveLife comes after the re-arm in the same
	// transaction: it names a life this transaction already replaced.
	require.NoError(t, f.sts.RemoveLife(txCtx, f.tenant, task.ID, c.ArmToken))
	require.NoError(t, f.tm.Commit(txCtx, txID))

	got := f.mustGet(task.ID)
	requireFreshLife(t, s, got)
	require.NotEqual(t, c.ArmToken, got.ArmToken)
	requireAllFencedRefused(t, f.ctx, f.sts, stRef(c), "a claim of the life the self-loop replaced")
}

// Re-arming starts a clean life from FAILED, and from WAITING after a
// failed attempt with PartialCommit set (spec §13 rows "FAILED task
// re-armed by an update in the state" and "a re-arm resets PartialCommit").
func testSTArmRearmResetsLife(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	s := f.spec(task.EntityID, "S", "T", stDue)

	// FAILED, with a mark and PartialCommit.
	c := f.claimTask(uuid.New(), task.ID)
	require.NoError(t, f.sts.MarkUnsafe(f.ctx, stRef(c)))
	require.NoError(t, f.sts.StampSegment(f.ctx, stRef(c), true))
	require.NoError(t, f.sts.Fail(f.ctx, stRef(c), spi.Failure{
		Reason: spi.FailureUnsafeWorkNotCompleted, Error: "UNSAFE", AtMs: stNow}))
	require.Equal(t, spi.ScheduledTaskFailed, f.mustGet(task.ID).Status)

	f.reconcile(f.ctx, task.EntityID, "S", s)
	got := f.mustGet(task.ID)
	requireFreshLife(t, s, got)
	next := f.claimTask(uuid.New(), task.ID)
	require.False(t, next.UnsafeMarked, "the new life has no mark")
	require.False(t, next.PartialCommit)

	// WAITING after a counted attempt, with PartialCommit set.
	require.NoError(t, f.sts.StampSegment(f.ctx, stRef(next), true))
	require.NoError(t, f.sts.RecordAttempt(f.ctx, stRef(next), spi.Attempt{
		Error: "E", AtMs: stNow, NextAttemptTime: stFuture}))
	waiting := f.mustGet(task.ID)
	require.Equal(t, 1, waiting.Attempts)
	require.True(t, waiting.PartialCommit)

	f.reconcile(f.ctx, task.EntityID, "S", s)
	requireFreshLife(t, s, f.mustGet(task.ID))
}

func testSTRemoveLifeCurrentLife(t *testing.T, h Harness) {
	f := newSTFixture(t, h)

	waiting := f.arm(stFuture)
	require.NoError(t, f.sts.RemoveLife(f.ctx, f.tenant, waiting.ID, waiting.ArmToken))
	f.requireGone(waiting.ID)

	running := f.armDue()
	c := f.claimTask(uuid.New(), running.ID)
	require.NoError(t, f.sts.RemoveLife(f.ctx, f.tenant, c.ID, c.ArmToken))
	f.requireGone(c.ID)

	failed := f.armDue()
	fc := f.claimTask(uuid.New(), failed.ID)
	require.NoError(t, f.sts.Fail(f.ctx, stRef(fc), spi.Failure{Reason: spi.FailureRunPanicked, Error: "E", AtMs: stNow}))
	require.NoError(t, f.sts.RemoveLife(f.ctx, f.tenant, fc.ID, fc.ArmToken))
	f.requireGone(fc.ID)
}

func testSTRemoveLifeOtherLifeIsNoOp(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.arm(stFuture)
	require.NoError(t, f.sts.RemoveLife(f.ctx, f.tenant, task.ID, uuid.New()))
	require.Equal(t, task.ArmToken, f.mustGet(task.ID).ArmToken, "a RemoveLife naming another life does nothing")
	require.NoError(t, f.sts.RemoveLife(f.ctx, f.tenant, "st-missing-"+newID(), uuid.New()),
		"a RemoveLife of a missing task does nothing")
}

func testSTRemoveLifeJoinsTransaction(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.arm(stFuture)
	txID, txCtx := f.begin()
	require.NoError(t, f.sts.RemoveLife(txCtx, f.tenant, task.ID, task.ArmToken))
	_, found := f.get(txCtx, task.ID)
	require.False(t, found, "the transaction sees its own removal (C2)")
	f.mustGet(task.ID) // not yet outside it
	require.NoError(t, f.tm.Rollback(txCtx, txID))
	require.Equal(t, task.ArmToken, f.mustGet(task.ID).ArmToken)
}

func testSTDeleteForEntitiesRemovesListed(t *testing.T, h Harness) {
	f := newSTFixture(t, h)

	e1 := f.newEntity()
	w1 := f.spec(e1, "S", "T1", stFuture)
	w2 := f.spec(e1, "S", "T2", stFuture)
	f.reconcile(f.ctx, e1, "S", w1, w2)
	running := f.claimTask(uuid.New(), f.armDue().ID)
	failedTask := f.armDue()
	fc := f.claimTask(uuid.New(), failedTask.ID)
	require.NoError(t, f.sts.Fail(f.ctx, stRef(fc), spi.Failure{Reason: spi.FailureRunPanicked, Error: "E", AtMs: stNow}))
	kept := f.arm(stFuture)

	require.NoError(t, f.sts.DeleteForEntities(f.ctx, f.tenant,
		[]string{e1, running.EntityID, failedTask.EntityID, "st-unknown-" + newID()}))
	f.requireGone(w1.ID)
	f.requireGone(w2.ID)
	f.requireGone(running.ID)
	f.requireGone(failedTask.ID)
	f.mustGet(kept.ID)

	require.NoError(t, f.sts.DeleteForEntities(f.ctx, f.tenant, nil), "an empty list is a no-op")
	f.mustGet(kept.ID)
}

func testSTDeleteForEntitiesJoinsTransaction(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.arm(stFuture)
	txID, txCtx := f.begin()
	require.NoError(t, f.sts.DeleteForEntities(txCtx, f.tenant, []string{task.EntityID}))
	_, found := f.get(txCtx, task.ID)
	require.False(t, found)
	require.NoError(t, f.tm.Rollback(txCtx, txID))
	f.mustGet(task.ID)
}

func testSTDeleteForModelKeep(t *testing.T, h Harness) {
	f := newSTFixture(t, h)

	keepMe := f.spec(f.newEntity(), "S", "T1", stFuture)
	dropT2 := f.spec(f.newEntity(), "S", "T2", stFuture)
	dropS2 := f.spec(f.newEntity(), "S2", "T1", stFuture)
	v2 := f.spec(f.newEntity(), "S", "T2", stFuture)
	v2.ModelVersion = 2
	otherModel := f.spec(f.newEntity(), "S", "T2", stFuture)
	otherModel.ModelName = f.model + "-other"
	for _, s := range []spi.ScheduledTask{keepMe, dropT2, dropS2, v2, otherModel} {
		f.reconcile(f.ctx, s.EntityID, s.SourceState, s)
	}
	// A RUNNING task follows the same rule.
	runningDrop := f.spec(f.newEntity(), "S", "T2", stDue)
	f.reconcile(f.ctx, runningDrop.EntityID, "S", runningDrop)
	f.claimTask(uuid.New(), runningDrop.ID)

	require.NoError(t, f.sts.DeleteForModel(f.ctx, f.tenant, f.model, 1,
		func(sourceState, transition string) bool { return sourceState == "S" && transition == "T1" }))
	f.mustGet(keepMe.ID)
	f.requireGone(dropT2.ID)
	f.requireGone(dropS2.ID)
	f.requireGone(runningDrop.ID)
	f.mustGet(v2.ID)         // another version of the model
	f.mustGet(otherModel.ID) // another model
}

func testSTDeleteForModelNilKeep(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	a := f.arm(stFuture)
	b := f.arm(stFuture)

	txID, txCtx := f.begin()
	require.NoError(t, f.sts.DeleteForModel(txCtx, f.tenant, f.model, 1, nil))
	require.NoError(t, f.tm.Rollback(txCtx, txID))
	f.mustGet(a.ID) // joins: the rollback undid it

	require.NoError(t, f.sts.DeleteForModel(f.ctx, f.tenant, f.model, 1, nil))
	f.requireGone(a.ID)
	f.requireGone(b.ID)
}

func testSTDeleteForModelTenantScoped(t *testing.T, h Harness) {
	fa := newSTFixture(t, h)
	fb := newSTFixture(t, h)
	fb.model = fa.model // the same model name in both tenants
	a := fa.arm(stFuture)
	b := fb.arm(stFuture)

	require.NoError(t, fb.sts.DeleteForModel(fb.ctx, fb.tenant, fa.model, 1, nil))
	fb.requireGone(b.ID)
	fa.mustGet(a.ID)
}

func testSTGetMissing(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	_, found := f.get(f.ctx, "st-missing-"+newID())
	require.False(t, found)
}
```

`spitest/doc.go`: replace `:6-8` with

```go
// Every subtest runs under a fresh tenant produced by Harness.NewTenant.
// No subtest reuses another's tenant, so no database truncation or Reset
// hook is needed. The ScheduledTasks group is the one exception to "no
// teardown": ClaimDue is cross-tenant, so each of its subtests removes the
// tasks it armed when it ends.
```

and insert before `:15` (`// A few SPI interfaces are optional …`):

```go
// A backend whose StoreFactory.ScheduledTaskStore returns an error
// satisfying errors.Is(err, errors.ErrUnsupported) skips the ScheduledTasks
// group.
//
```

`spitest/README.md` `:13-15` becomes:

```markdown
The harness covers the full SPI surface: entity persistence, audit,
async search, scheduled tasks, transactions, workflow plugin contracts,
and key/value extension hooks. A backend without a scheduled-task store
returns an error satisfying `errors.Is(err, errors.ErrUnsupported)` from
`StoreFactory.ScheduledTaskStore`; the ScheduledTasks group then skips.
```

- [ ] **Step 4: Run it and see it pass**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && gofmt -l . && go vet ./... && go test ./...`
Expected: `gofmt -l` prints nothing; vet clean; root PASS; `spitest`
`[no test files]`.

- [ ] **Step 5: Commit**

```
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi add spitest/scheduledtasks.go spitest/scheduledtasks_lifecycle.go spitest/spitest.go spitest/doc.go spitest/README.md
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi commit -m "test(spitest): ScheduledTasks group — arm, remove, delete, get

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task S-5: ScheduledTasks — claims, liveness, give-back

**Spec:** §6.1, §6.2, §6.4 step 5; §10.1 `ClaimDue`, `Heartbeat`,
`RetireOwner`, `SweepOwners`, `GiveBackIdle`; §13 rows in the coverage table.

**Files:**
- Modify: `spitest/scheduledtasks.go` (`runScheduledTasksSuite`)
- Create: `spitest/scheduledtasks_claim.go`

**Interfaces:**
- Consumes: S-4's fixture.
- Produces: subtests `Claim/*` (`Claim/LostOwnerFlagged` included, README
  C-S1), `Liveness/*`, `GiveBack/*`.

Liveness has no read method. The tests observe it through a lost-owner claim:
with `StaleAfter` one hour, a task is reclaimable only if its owner's record is
missing. Owners that must stay live heartbeat; the rest never do.

Stale-owner and sweep subtests move time only through `h.AdvanceClock`, which
every backend's harness implements: memory and SQLite move an injected clock,
and PostgreSQL, which cannot move the database clock, sleeps for real (5 ms
floor, 100 ms ceiling per call, fact 12). `StaleAfter` and `deadFor` are 20 ms
(`stShortStale`) and each advance is 60 ms, so each such subtest spends about
60 ms of real time on PostgreSQL. A subtest that needs an owner to stay fresh
uses one hour, so no real-time delay can make it stale.

`Claim/InvalidLimits` pins that `Limit < 1` or `PerTenantLimit < 1` is a caller
error, not "claim nothing".

- [ ] **Step 1: Write the failing registration** — append at the end of
  `runScheduledTasksSuite`, before its closing brace, one blank line and:

```go
	// Claims, liveness, give-back (S-5).
	runSubtest(t, h, tracker, "Claim/DueWaiting", testSTClaimDueWaiting)
	runSubtest(t, h, tracker, "Claim/NewTokenPerClaim", testSTClaimNewTokenPerClaim)
	runSubtest(t, h, tracker, "Claim/FailedNeverClaimed", testSTClaimFailedNeverClaimed)
	runSubtest(t, h, tracker, "Claim/RunningNeedsAllowLostOwner", testSTClaimRunningNeedsAllowLostOwner)
	runSubtest(t, h, tracker, "Claim/FreshOwnerKept", testSTClaimFreshOwnerKept)
	runSubtest(t, h, tracker, "Claim/StaleOwnerReclaimed", testSTClaimStaleOwnerReclaimed)
	runSubtest(t, h, tracker, "Claim/LostOwnerFlagged", testSTClaimLostOwnerFlagged)
	runSubtest(t, h, tracker, "Claim/LostOwnersCounted", testSTClaimLostOwnersCounted)
	runSubtest(t, h, tracker, "Claim/OnePerEntity", testSTClaimOnePerEntity)
	runSubtest(t, h, tracker, "Claim/LimitAndOrder", testSTClaimLimitAndOrder)
	runSubtest(t, h, tracker, "Claim/InvalidLimits", testSTClaimInvalidLimits)
	runSubtest(t, h, tracker, "Claim/TenantsTakeTurns", testSTClaimTenantsTakeTurns)
	runSubtest(t, h, tracker, "Claim/PerTenantLimit", testSTClaimPerTenantLimit)
	runSubtest(t, h, tracker, "Claim/ConcurrentDisjoint", testSTClaimConcurrentDisjoint)
	runSubtest(t, h, tracker, "Claim/SiblingsConcurrent", testSTClaimSiblingsConcurrent)
	runSubtest(t, h, tracker, "Claim/ContendedNoReclaim", testSTClaimContendedNoReclaim)
	runSubtest(t, h, tracker, "Liveness/SweepRemovesUnreferenced", testSTLivenessSweepRemovesUnreferenced)
	runSubtest(t, h, tracker, "Liveness/SweepKeepsReferenced", testSTLivenessSweepKeepsReferenced)
	runSubtest(t, h, tracker, "Liveness/HeartbeatRecreatesSwept", testSTLivenessHeartbeatRecreatesSwept)
	runSubtest(t, h, tracker, "Liveness/RetireOwner", testSTLivenessRetireOwner)
	runSubtest(t, h, tracker, "GiveBack/LostReply", testSTGiveBackLostReply)
	runSubtest(t, h, tracker, "GiveBack/KeepsLiveRuns", testSTGiveBackKeepsLiveRuns)
	runSubtest(t, h, tracker, "GiveBack/NotCountedKeepsMark", testSTGiveBackNotCountedKeepsMark)
```

- [ ] **Step 2: Run it and see it fail**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go vet ./spitest/`
Expected: FAIL — `undefined: testSTClaimDueWaiting` and the others above.

- [ ] **Step 3: Implement** — create `spitest/scheduledtasks_claim.go`:

```go
package spitest

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func testSTClaimDueWaiting(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	due := f.armDue()
	notDue := f.arm(stNow + 1)
	owner := uuid.New()

	c := f.claimTask(owner, due.ID) // requires notDue to stay unclaimed
	require.Equal(t, due.ArmToken, c.ArmToken, "a claim keeps the life")
	require.Zero(t, c.LostOwners, "a claim of a WAITING task is not a lost-owner claim")
	require.Zero(t, c.Attempts)
	require.False(t, c.UnsafeMarked)
	requireUnchangedClaim(t, c, f.mustGet(due.ID)) // the claim is persisted, not only returned

	require.Empty(t, f.claimWith(f.claimReq(uuid.New(), false)), "a RUNNING task is not claimed again")
	require.Equal(t, spi.ScheduledTaskWaiting, f.mustGet(notDue.ID).Status)

	req := f.claimReq(uuid.New(), false)
	req.NowMs = stNow + 1
	require.Equal(t, []string{notDue.ID}, stIDs(f.claimWith(req)), "due exactly when NextAttemptTime <= NowMs")
}

func testSTClaimNewTokenPerClaim(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	owner := uuid.New()
	first := f.claimTask(owner, task.ID)
	n, err := f.sts.GiveBackIdle(context.Background(), owner, nil)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	second := f.claimTask(owner, task.ID)
	require.NotEqual(t, first.Claim.Token, second.Claim.Token, "every claim draws a new token, even for the same owner")
	require.Equal(t, first.ArmToken, second.ArmToken)
}

func testSTClaimFailedNeverClaimed(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)
	require.NoError(t, f.sts.Fail(f.ctx, stRef(c), spi.Failure{Reason: spi.FailureRunPanicked, Error: "E", AtMs: stNow}))

	req := f.claimReq(uuid.New(), true)
	req.NowMs = stFuture
	require.Nil(t, findST(f.claimWith(req), task.ID), "a FAILED task is never claimed, by any kind of claim")
}

func testSTClaimRunningNeedsAllowLostOwner(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID) // the owner has no liveness record
	require.Empty(t, f.claimWith(f.claimReq(uuid.New(), false)),
		"without AllowLostOwner a RUNNING task is never claimed, however stale its owner")
	requireUnchangedClaim(t, c, f.mustGet(task.ID))
}

func testSTClaimFreshOwnerKept(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	owner := uuid.New()
	require.NoError(t, f.sts.Heartbeat(context.Background(), owner))
	require.NoError(t, f.sts.Heartbeat(context.Background(), owner), "Heartbeat is an upsert")
	task := f.armDue()
	c := f.claimTask(owner, task.ID)

	require.Empty(t, f.claimWith(f.claimReq(uuid.New(), true)), "a task whose owner heartbeats is not lost")
	requireUnchangedClaim(t, c, f.mustGet(task.ID))
}

func testSTClaimStaleOwnerReclaimed(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	a := uuid.New()
	require.NoError(t, f.sts.Heartbeat(context.Background(), a))
	task := f.armDue()
	ca := f.claimTask(a, task.ID)

	h.AdvanceClock(3 * stShortStale)
	b := uuid.New()
	req := f.claimReq(b, true)
	req.StaleAfter = stShortStale
	cb := f.claimOnly(req, task.ID)
	require.Equal(t, ca.ArmToken, cb.ArmToken, "a lost-owner claim keeps the life")
	require.NotEqual(t, ca.Claim.Token, cb.Claim.Token)
	require.Equal(t, 1, cb.LostOwners)
	require.Zero(t, cb.Attempts)
	requireUnchangedClaim(t, cb, f.mustGet(task.ID))
}

// ClaimedFromLostOwner is set only on a ClaimDue result, and only when that
// claim took the task from a stale or missing owner (README C-S1): the
// scheduler counts cyoda.scheduler.claims{reason} from it. A read never
// carries it.
func testSTClaimLostOwnerFlagged(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	first := f.claimTask(uuid.New(), task.ID) // this owner never heartbeats: missing counts as stale
	require.False(t, first.ClaimedFromLostOwner, "a claim of a WAITING task is not from a lost owner")
	require.False(t, f.mustGet(task.ID).ClaimedFromLostOwner, "Get never carries the flag")

	second := f.reclaim(uuid.New(), task.ID)
	require.True(t, second.ClaimedFromLostOwner, "a lost-owner claim is flagged on the ClaimDue result")
	require.False(t, f.mustGet(task.ID).ClaimedFromLostOwner, "Get never carries the flag")
}

// Every lost-owner claim adds one to LostOwners (spec §13 row "owner lost 3
// times → FAILED OWNER_LOST_REPEATEDLY": the store counts, the engine
// decides).
func testSTClaimLostOwnersCounted(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)
	for want := 1; want <= 3; want++ {
		c = f.reclaim(uuid.New(), task.ID)
		require.Equal(t, want, c.LostOwners)
	}
	require.Equal(t, 3, f.mustGet(task.ID).LostOwners)
	require.Zero(t, f.mustGet(task.ID).Attempts, "a lost owner is not a counted attempt")
}

// Two due siblings: one claim per call (spec §13).
func testSTClaimOnePerEntity(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	e := f.newEntity()
	s1 := f.spec(e, "S", "T1", stDue)
	s2 := f.spec(e, "S", "T2", stDue)
	f.reconcile(f.ctx, e, "S", s1, s2)

	first := f.claimWith(f.claimReq(uuid.New(), false))
	require.Len(t, first, 1, "at most one task per entity per call")
	require.Empty(t, f.claimWith(f.claimReq(uuid.New(), false)),
		"a task is not claimed while another task of its entity is RUNNING")

	// A lost-owner claim may take the RUNNING task back, never its sibling.
	again := f.claimWith(f.claimReq(uuid.New(), true))
	require.Len(t, again, 1)
	require.Equal(t, first[0].ID, again[0].ID)

	// Once the RUNNING task is back to WAITING and not due, the sibling is
	// claimable.
	require.NoError(t, f.sts.RecordAttempt(f.ctx, stRef(again[0]), spi.Attempt{
		Error: "E", AtMs: stNow, NextAttemptTime: stFuture}))
	sibling := s2.ID
	if first[0].ID == s2.ID {
		sibling = s1.ID
	}
	require.Equal(t, []string{sibling}, stIDs(f.claimWith(f.claimReq(uuid.New(), false))))
}

func testSTClaimLimitAndOrder(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	byTime := map[int64]string{}
	for _, at := range []int64{stDue - 20, stDue - 50, stDue - 10, stDue - 40, stDue - 30} {
		byTime[at] = f.arm(at).ID
	}
	req := f.claimReq(uuid.New(), false)
	req.Limit = 2
	res, err := f.sts.ClaimDue(context.Background(), req)
	require.NoError(t, err)
	require.LessOrEqual(t, len(res), 2, "Limit caps the whole call")
	require.ElementsMatch(t, []string{byTime[stDue-50], byTime[stDue-40]}, stIDs(f.own(res)),
		"within a tenant, tasks are claimed in NextAttemptTime order")
}

// Limit and PerTenantLimit below 1 are caller errors, not "claim nothing".
func testSTClaimInvalidLimits(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	for name, mutate := range map[string]func(*spi.ClaimRequest){
		"Limit 0":           func(r *spi.ClaimRequest) { r.Limit = 0 },
		"Limit -1":          func(r *spi.ClaimRequest) { r.Limit = -1 },
		"PerTenantLimit 0":  func(r *spi.ClaimRequest) { r.PerTenantLimit = 0 },
		"PerTenantLimit -1": func(r *spi.ClaimRequest) { r.PerTenantLimit = -1 },
	} {
		req := f.claimReq(uuid.New(), false)
		mutate(&req)
		_, err := f.sts.ClaimDue(context.Background(), req)
		require.Error(t, err, name)
	}
	require.Equal(t, spi.ScheduledTaskWaiting, f.mustGet(task.ID).Status, "a refused call claims nothing")
}

func testSTClaimTenantsTakeTurns(t *testing.T, h Harness) {
	fa := newSTFixture(t, h)
	fb := newSTFixture(t, h)
	// Tenant A's tasks are all older than tenant B's: an order by time alone
	// would give both slots to A.
	for i := int64(0); i < 3; i++ {
		fa.arm(stDue - 100 + i)
		fb.arm(stDue - 10 + i)
	}
	req := fa.claimReq(uuid.New(), false)
	req.Limit = 2
	res, err := fa.sts.ClaimDue(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, fa.own(res), 1, "tenants take turns")
	require.Len(t, fb.own(res), 1, "tenants take turns")
}

func testSTClaimPerTenantLimit(t *testing.T, h Harness) {
	fa := newSTFixture(t, h)
	fb := newSTFixture(t, h)
	var aOldest []string
	for i := int64(0); i < 4; i++ {
		id := fa.arm(stDue - 100 + i).ID
		if i < 2 {
			aOldest = append(aOldest, id)
		}
		fb.arm(stDue - 10 + i)
	}

	// Tenant A already has 2 runs in progress: at PerTenantLimit 2 it gets
	// none, and B still gets its 2.
	req := fa.claimReq(uuid.New(), false)
	req.PerTenantLimit = 2
	req.TenantInProgress = map[spi.TenantID]int{fa.tenant: 2}
	res, err := fa.sts.ClaimDue(context.Background(), req)
	require.NoError(t, err)
	require.Empty(t, fa.own(res))
	require.Len(t, fb.own(res), 2)

	// With nothing in progress, A gets its 2 oldest.
	req.TenantInProgress = nil
	res, err = fa.sts.ClaimDue(context.Background(), req)
	require.NoError(t, err)
	require.ElementsMatch(t, aOldest, stIDs(fa.own(res)))
}

// Concurrent ClaimDue calls get disjoint sets (spec §13).
func testSTClaimConcurrentDisjoint(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	const tasks, callers = 20, 4
	for i := 0; i < tasks; i++ {
		f.armDue()
	}
	owners := make([]uuid.UUID, callers)
	results := make([][]spi.ScheduledTask, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range owners {
		owners[i] = uuid.New()
		req := f.claimReq(owners[i], false)
		req.Limit = 5
		wg.Add(1)
		go func(i int, req spi.ClaimRequest) {
			defer wg.Done()
			<-start
			results[i], errs[i] = f.sts.ClaimDue(context.Background(), req)
		}(i, req)
	}
	close(start)
	wg.Wait()

	seen := map[string]bool{}
	for i := range results {
		require.NoError(t, errs[i])
		for _, c := range f.own(results[i]) {
			require.False(t, seen[c.ID], "task %s was claimed by two concurrent calls", c.ID)
			seen[c.ID] = true
			require.Equal(t, owners[i], c.Claim.Owner)
			requireUnchangedClaim(t, c, f.mustGet(c.ID))
		}
	}
}

// Two due siblings, two pnodes at once: one wins, the other claims nothing
// of that entity (spec §13).
func testSTClaimSiblingsConcurrent(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	for i := 0; i < 10; i++ {
		e := f.newEntity()
		f.reconcile(f.ctx, e, "S", f.spec(e, "S", "T1", stDue), f.spec(e, "S", "T2", stDue))

		var results [2][]spi.ScheduledTask
		var errs [2]error
		start := make(chan struct{})
		var wg sync.WaitGroup
		for j := 0; j < 2; j++ {
			req := f.claimReq(uuid.New(), false)
			wg.Add(1)
			go func(j int, req spi.ClaimRequest) {
				defer wg.Done()
				<-start
				results[j], errs[j] = f.sts.ClaimDue(context.Background(), req)
			}(j, req)
		}
		close(start)
		wg.Wait()

		claimed := 0
		for j := 0; j < 2; j++ {
			require.NoError(t, errs[j], "losing a sibling race is not an error")
			for _, c := range f.own(results[j]) {
				if c.EntityID == e {
					claimed++
				}
			}
		}
		require.Equal(t, 1, claimed, "iteration %d: exactly one task of the entity is claimed", i)
	}
}

// Contended claim loop: a task claimed elsewhere between ranking and
// locking is never claimed again (spec §13).
func testSTClaimContendedNoReclaim(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	const tasks, callers, rounds = 40, 8, 20
	for i := 0; i < tasks; i++ {
		f.armDue()
	}
	var mu sync.Mutex
	counts := map[string]int{}
	var firstErr error
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		req := f.claimReq(uuid.New(), false)
		req.Limit = 3
		wg.Add(1)
		go func(req spi.ClaimRequest) {
			defer wg.Done()
			<-start
			for r := 0; r < rounds; r++ {
				res, err := f.sts.ClaimDue(context.Background(), req)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				for _, c := range f.own(res) {
					counts[c.ID]++
				}
				mu.Unlock()
			}
		}(req)
	}
	close(start)
	wg.Wait()
	require.NoError(t, firstErr)

	// Whatever the contention left behind is claimable now, once.
	for _, c := range f.claimWith(f.claimReq(uuid.New(), false)) {
		counts[c.ID]++
	}
	require.Len(t, counts, tasks, "every task is claimed")
	for id, n := range counts {
		require.Equal(t, 1, n, "task %s was claimed %d times", id, n)
	}
}

// SweepOwners removes a liveness record no task references. The effect is
// visible through a lost-owner claim: a task claimed by a swept owner is
// lost even under a StaleAfter its heartbeat would have met.
func testSTLivenessSweepRemovesUnreferenced(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	a := uuid.New()
	require.NoError(t, f.sts.Heartbeat(context.Background(), a))
	h.AdvanceClock(3 * stShortStale)
	require.NoError(t, f.sts.SweepOwners(context.Background(), stShortStale))

	task := f.armDue()
	f.claimTask(a, task.ID)
	c := f.reclaim(uuid.New(), task.ID)
	require.Equal(t, 1, c.LostOwners)
}

func testSTLivenessSweepKeepsReferenced(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	a := uuid.New()
	require.NoError(t, f.sts.Heartbeat(context.Background(), a))
	task := f.armDue()
	c := f.claimTask(a, task.ID)
	h.AdvanceClock(3 * stShortStale)
	require.NoError(t, f.sts.SweepOwners(context.Background(), stShortStale))

	require.Empty(t, f.claimWith(f.claimReq(uuid.New(), true)),
		"the record of an owner a task references is not swept")
	requireUnchangedClaim(t, c, f.mustGet(task.ID))
}

// A liveness record swept during a long outage is recreated by the next
// heartbeat (spec §13).
func testSTLivenessHeartbeatRecreatesSwept(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	a := uuid.New()
	require.NoError(t, f.sts.Heartbeat(context.Background(), a))
	h.AdvanceClock(3 * stShortStale)
	require.NoError(t, f.sts.SweepOwners(context.Background(), stShortStale))
	require.NoError(t, f.sts.Heartbeat(context.Background(), a))

	task := f.armDue()
	c := f.claimTask(a, task.ID)
	require.Empty(t, f.claimWith(f.claimReq(uuid.New(), true)), "the recreated record keeps the owner live")
	requireUnchangedClaim(t, c, f.mustGet(task.ID))
}

func testSTLivenessRetireOwner(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	a := uuid.New()
	require.NoError(t, f.sts.Heartbeat(context.Background(), a))
	task := f.armDue()
	f.claimTask(a, task.ID)
	require.NoError(t, f.sts.RetireOwner(context.Background(), a))
	c := f.reclaim(uuid.New(), task.ID)
	require.Equal(t, 1, c.LostOwners, "a retired owner has no liveness: its tasks are lost")
}

// A lost claim reply: the next GiveBackIdle returns the claimed tasks
// (spec §13).
func testSTGiveBackLostReply(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, f.armDue().ID)
	}
	a := uuid.New()
	require.Len(t, f.claimWith(f.claimReq(a, false)), 3) // the reply is "lost": nothing registers it

	n, err := f.sts.GiveBackIdle(context.Background(), a, nil)
	require.NoError(t, err)
	require.Equal(t, 3, n)
	for _, id := range ids {
		got := f.mustGet(id)
		require.Equal(t, spi.ScheduledTaskWaiting, got.Status)
		require.Nil(t, got.Claim)
		require.Zero(t, got.Attempts, "a give-back is not counted")
		require.Zero(t, got.LostOwners)
	}
	require.ElementsMatch(t, ids, stIDs(f.claimWith(f.claimReq(uuid.New(), false))),
		"given-back tasks are claimable at once")
}

// A RUNNING task with no live run is given back; a live run never is
// (spec §13).
func testSTGiveBackKeepsLiveRuns(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	a := uuid.New()
	live := f.claimTask(a, f.armDue().ID)
	idle := f.claimTask(a, f.armDue().ID)
	other := f.claimTask(uuid.New(), f.armDue().ID)

	n, err := f.sts.GiveBackIdle(context.Background(), a, []uuid.UUID{live.Claim.Token})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	requireUnchangedClaim(t, live, f.mustGet(live.ID))
	require.Equal(t, spi.ScheduledTaskWaiting, f.mustGet(idle.ID).Status)
	requireUnchangedClaim(t, other, f.mustGet(other.ID)) // another owner's task

	n, err = f.sts.GiveBackIdle(context.Background(), a, []uuid.UUID{live.Claim.Token})
	require.NoError(t, err)
	require.Zero(t, n, "nothing left to give back is a normal no-op")
}

// GiveBackIdle is not counted and keeps the life's mark (spec §13 row
// "GiveBackIdle is not counted; RetireOwner removes liveness").
func testSTGiveBackNotCountedKeepsMark(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	a := uuid.New()
	task := f.armDue()
	c := f.claimTask(a, task.ID)
	require.NoError(t, f.sts.MarkUnsafe(f.ctx, stRef(c)))
	_, err := f.sts.GiveBackIdle(context.Background(), a, nil)
	require.NoError(t, err)

	next := f.claimTask(uuid.New(), task.ID)
	require.Zero(t, next.Attempts)
	require.Zero(t, next.LostOwners)
	require.True(t, next.UnsafeMarked, "the mark belongs to the life; a give-back does not remove it")
}
```

- [ ] **Step 4: Run it and see it pass**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && gofmt -l . && go vet ./...`
Expected: prints nothing; vet clean.

- [ ] **Step 5: Commit**

```
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi add spitest/scheduledtasks.go spitest/scheduledtasks_claim.go
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi commit -m "test(spitest): ScheduledTasks — claims, owner liveness, give-back

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task S-6: ScheduledTasks — fenced writes, marks, recorded outcomes, error text, `ErrStoreRejected`

**Spec:** §5.5 table and "Where the mark lives"; §5.6 `RecordAttempt`, "The
node latches"; §5.7; §5.8 last paragraph; §10.1 fenced methods, `SweepMarks`;
§13 rows in the coverage table.

**Files:**
- Modify: `spitest/scheduledtasks.go` (`runScheduledTasksSuite`)
- Create: `spitest/scheduledtasks_fence.go`

**Interfaces:**
- Consumes: S-4's fixture; S-1's errors.
- Produces: subtests `Fence/*`, `Stamp/*`, `Mark/*`, `Record/*`, `Fail/*`,
  `ErrorText/*`, `SweepMarks/*`. `ErrorText/StoreRejected` is the conformance
  case that every store sets `spi.ErrStoreRejected` (spec §5.6): error text
  with a NUL, with invalid UTF-8, or over 1024 bytes breaks the documented
  precondition of `Attempt.Error` / `Failure.Error`, and every backend must
  refuse it deterministically, with the marker, and change nothing.
  `Record/NotCounted` pins that an uncounted attempt still records `Error` and
  `LastAttemptTime`; `Fail/OverwritesLastError` pins that `Fail` replaces
  `LastError` even with `""`.

The harness cannot portably provoke a deterministic rejection from
`StateMachineAuditStore.Record` or from a transaction's `Commit` (the audit
write that shares `Fail`'s transaction, spec §5.7): no input is invalid for
every backend's audit store. The `ErrStoreRejected` doc (S-1) binds them, and
each backend stream tests its own rejection paths (Stream interface summary).

- [ ] **Step 1: Write the failing registration** — append at the end of
  `runScheduledTasksSuite`, one blank line and:

```go
	// Fenced writes, marks, recorded outcomes, error text (S-6).
	runSubtest(t, h, tracker, "Fence/StaleTokensRefused", testSTFenceStaleTokensRefused)
	runSubtest(t, h, tracker, "Fence/WaitingRefused", testSTFenceWaitingRefused)
	runSubtest(t, h, tracker, "Fence/OldLifeRefused", testSTFenceOldLifeRefused)
	runSubtest(t, h, tracker, "Fence/ABA", testSTFenceABA)
	runSubtest(t, h, tracker, "Fence/ReplacedOwnerStampRefused", testSTFenceReplacedOwnerStampRefused)
	runSubtest(t, h, tracker, "Stamp/PartialCommit", testSTStampPartialCommit)
	runSubtest(t, h, tracker, "Mark/AcceptedAndIdempotent", testSTMarkAcceptedAndIdempotent)
	runSubtest(t, h, tracker, "Mark/MarkedByAnotherClaim", testSTMarkMarkedByAnotherClaim)
	runSubtest(t, h, tracker, "Mark/SurvivesRollback", testSTMarkSurvivesRollback)
	runSubtest(t, h, tracker, "Record/Counted", testSTRecordCounted)
	runSubtest(t, h, tracker, "Record/NotCounted", testSTRecordNotCounted)
	runSubtest(t, h, tracker, "Record/ClearOwnMark", testSTRecordClearOwnMark)
	runSubtest(t, h, tracker, "Record/OtherClaimsMarkKept", testSTRecordOtherClaimsMarkKept)
	runSubtest(t, h, tracker, "Record/RepeatRefused", testSTRecordRepeatRefused)
	runSubtest(t, h, tracker, "Fail/Fields", testSTFailFields)
	runSubtest(t, h, tracker, "Fail/OverwritesLastError", testSTFailOverwritesLastError)
	runSubtest(t, h, tracker, "Fail/JoinsTransaction", testSTFailJoinsTransaction)
	runSubtest(t, h, tracker, "ErrorText/RoundTrip", testSTErrorTextRoundTrip)
	runSubtest(t, h, tracker, "ErrorText/StoreRejected", testSTErrorTextStoreRejected)
	runSubtest(t, h, tracker, "SweepMarks/KeepsCurrentLife", testSTSweepMarksKeepsCurrentLife)
```

- [ ] **Step 2: Run it and see it fail**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go vet ./spitest/`
Expected: FAIL — `undefined: testSTFenceStaleTokensRefused` and the others above.

- [ ] **Step 3: Implement** — create `spitest/scheduledtasks_fence.go`:

```go
package spitest

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Every fenced method refuses a stale token (spec §13).
func testSTFenceStaleTokensRefused(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	fb := newSTFixture(t, h)
	c := f.claimTask(uuid.New(), f.armDue().ID)
	good := stRef(c)

	bad := map[string]spi.TaskRef{
		"another claim token": {TenantID: good.TenantID, ID: good.ID, ArmToken: good.ArmToken, ClaimToken: uuid.New()},
		"the nil claim token": {TenantID: good.TenantID, ID: good.ID, ArmToken: good.ArmToken, ClaimToken: uuid.Nil},
		"another arm token":   {TenantID: good.TenantID, ID: good.ID, ArmToken: uuid.New(), ClaimToken: good.ClaimToken},
		"a missing task":      {TenantID: good.TenantID, ID: "st-missing-" + newID(), ArmToken: good.ArmToken, ClaimToken: good.ClaimToken},
		"another tenant":      {TenantID: fb.tenant, ID: good.ID, ArmToken: good.ArmToken, ClaimToken: good.ClaimToken},
	}
	for why, r := range bad {
		ctx := f.ctx
		if r.TenantID == fb.tenant {
			ctx = fb.ctx
		}
		requireAllFencedRefused(t, ctx, f.sts, r, why)
	}
	requireUnchangedClaim(t, c, f.mustGet(c.ID))
}

func testSTFenceWaitingRefused(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.arm(stFuture)
	for _, claim := range []uuid.UUID{uuid.Nil, uuid.New()} {
		r := spi.TaskRef{TenantID: f.tenant, ID: task.ID, ArmToken: task.ArmToken, ClaimToken: claim}
		requireAllFencedRefused(t, f.ctx, f.sts, r, "a task that is not claimed")
	}
	got := f.mustGet(task.ID)
	require.Equal(t, spi.ScheduledTaskWaiting, got.Status)
	require.False(t, got.UnsafeMarked)
}

// After a re-arm, every fenced write of the old life is refused (spec §13).
func testSTFenceOldLifeRefused(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)
	s := f.spec(task.EntityID, "S", "T", stFuture)
	f.reconcile(f.ctx, task.EntityID, "S", s)

	requireAllFencedRefused(t, f.ctx, f.sts, stRef(c), "a claim of a life that was re-armed")
	requireFreshLife(t, s, f.mustGet(task.ID))
}

// ABA: the old token is refused after a re-arm and a new claim (spec §13).
func testSTFenceABA(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c1 := f.claimTask(uuid.New(), task.ID)
	f.reconcile(f.ctx, task.EntityID, "S", f.spec(task.EntityID, "S", "T", stDue))
	c2 := f.claimTask(uuid.New(), task.ID)
	require.NotEqual(t, c1.ArmToken, c2.ArmToken)

	for why, r := range map[string]spi.TaskRef{
		"the old life and old claim":  stRef(c1),
		"the new life, the old claim": {TenantID: f.tenant, ID: task.ID, ArmToken: c2.ArmToken, ClaimToken: c1.Claim.Token},
		"the old life, the new claim": {TenantID: f.tenant, ID: task.ID, ArmToken: c1.ArmToken, ClaimToken: c2.Claim.Token},
	} {
		requireAllFencedRefused(t, f.ctx, f.sts, r, why)
	}
	requireUnchangedClaim(t, c2, f.mustGet(task.ID))
	require.NoError(t, f.sts.StampSegment(f.ctx, stRef(c2), false))
	require.NoError(t, f.sts.MarkUnsafe(f.ctx, stRef(c2)))
}

// A replaced owner's segment commit is refused by its stamp (spec §13).
func testSTFenceReplacedOwnerStampRefused(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	a := f.claimTask(uuid.New(), task.ID)
	b := f.reclaim(uuid.New(), task.ID)

	txID, txCtx := f.begin()
	require.ErrorIs(t, f.sts.StampSegment(txCtx, stRef(a), false), spi.ErrStaleClaim)
	_ = f.tm.Rollback(txCtx, txID)
	requireUnchangedClaim(t, b, f.mustGet(task.ID))
}

// StampSegment sets PartialCommit and the next claim of the life carries it
// (spec §13 row "owner killed after a cascade-step commit → next claim
// FAILED STOPPED_AFTER_PARTIAL_COMMIT": the store keeps the flag, the
// engine decides).
func testSTStampPartialCommit(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)

	require.NoError(t, f.sts.StampSegment(f.ctx, stRef(c), false))
	require.False(t, f.mustGet(task.ID).PartialCommit)
	require.NoError(t, f.sts.StampSegment(f.ctx, stRef(c), true))
	require.True(t, f.mustGet(task.ID).PartialCommit)
	require.NoError(t, f.sts.StampSegment(f.ctx, stRef(c), false))
	got := f.mustGet(task.ID)
	require.True(t, got.PartialCommit, "a stamp without partial keeps PartialCommit")
	require.Equal(t, spi.ScheduledTaskRunning, got.Status, "a stamp changes neither status nor claim")
	require.Equal(t, c.Claim.Token, got.Claim.Token)

	// The owner is gone; the next claim of the life carries the flag.
	next := f.reclaim(uuid.New(), task.ID)
	require.True(t, next.PartialCommit)

	// StampSegment joins: a rolled-back segment leaves no flag.
	other := f.armDue()
	oc := f.claimTask(uuid.New(), other.ID)
	txID, txCtx := f.begin()
	require.NoError(t, f.sts.StampSegment(txCtx, stRef(oc), true))
	require.NoError(t, f.tm.Rollback(txCtx, txID))
	require.False(t, f.mustGet(other.ID).PartialCommit)
}

func testSTMarkAcceptedAndIdempotent(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)
	require.NoError(t, f.sts.MarkUnsafe(f.ctx, stRef(c)))
	require.NoError(t, f.sts.MarkUnsafe(f.ctx, stRef(c)), "MarkUnsafe is idempotent for the same claim")
	got := f.mustGet(task.ID)
	require.True(t, got.UnsafeMarked)
	require.Equal(t, spi.ScheduledTaskRunning, got.Status)
	require.Equal(t, c.Claim.Token, got.Claim.Token)
}

// A mark outlives its owner: the next claim sees it and cannot mark again
// (spec §13 rows "ErrMarkedByAnotherClaim → FAILED" and
// "RecordAttempt{ClearOwnMark} retried in an outage; pnode dies first →
// FAILED at the next claim").
func testSTMarkMarkedByAnotherClaim(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	a := f.claimTask(uuid.New(), task.ID)
	require.NoError(t, f.sts.MarkUnsafe(f.ctx, stRef(a)))

	b := f.reclaim(uuid.New(), task.ID) // A died holding the mark
	require.True(t, b.UnsafeMarked, "the claim returns the life's mark (C3)")
	require.Equal(t, 1, b.LostOwners)
	require.ErrorIs(t, f.sts.MarkUnsafe(f.ctx, stRef(b)), spi.ErrMarkedByAnotherClaim)
	requireUnchangedClaim(t, b, f.mustGet(task.ID))
}

// A mark survives the rollback of the transaction on ctx (spec §13).
func testSTMarkSurvivesRollback(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)
	txID, txCtx := f.begin()
	_, _ = f.get(txCtx, task.ID)
	require.NoError(t, f.sts.MarkUnsafe(txCtx, stRef(c)), "MarkUnsafe never joins the transaction on ctx")
	require.NoError(t, f.tm.Rollback(txCtx, txID))
	require.True(t, f.mustGet(task.ID).UnsafeMarked)
	require.True(t, f.reclaim(uuid.New(), task.ID).UnsafeMarked)
}

func testSTRecordCounted(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)
	next := stNow + 60_000
	require.NoError(t, f.sts.RecordAttempt(f.ctx, stRef(c), spi.Attempt{
		Error: "CONFLICT: a concurrent write changed the entity or its task", AtMs: stNow + 5, NextAttemptTime: next}))

	got := f.mustGet(task.ID)
	require.Equal(t, spi.ScheduledTaskWaiting, got.Status)
	require.Nil(t, got.Claim)
	require.Equal(t, c.ArmToken, got.ArmToken)
	require.Equal(t, 1, got.Attempts)
	require.Zero(t, got.LostOwners)
	require.Equal(t, "CONFLICT: a concurrent write changed the entity or its task", got.LastError)
	require.NotNil(t, got.LastAttemptTime)
	require.Equal(t, stNow+5, *got.LastAttemptTime)
	require.Equal(t, next, got.NextAttemptTime)

	require.Empty(t, f.claimWith(f.claimReq(uuid.New(), false)), "not claimable before NextAttemptTime")
	req := f.claimReq(uuid.New(), false)
	req.NowMs = next
	c2 := f.claimOnly(req, task.ID)
	require.NoError(t, f.sts.RecordAttempt(f.ctx, stRef(c2), spi.Attempt{Error: "E2", AtMs: next, NextAttemptTime: next}))
	require.Equal(t, 2, f.mustGet(task.ID).Attempts)
}

func testSTRecordNotCounted(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)
	require.NoError(t, f.sts.RecordAttempt(f.ctx, stRef(c), spi.Attempt{
		Error: "CANCELLED: the run was stopped by the scheduler", AtMs: stNow, NextAttemptTime: stNow, NotCounted: true}))
	got := f.mustGet(task.ID)
	require.Equal(t, spi.ScheduledTaskWaiting, got.Status)
	require.Zero(t, got.Attempts)
	require.Equal(t, "CANCELLED: the run was stopped by the scheduler", got.LastError,
		"an attempt that is not counted is still recorded")
	require.NotNil(t, got.LastAttemptTime)
	require.Equal(t, stNow, *got.LastAttemptTime)
	f.claimTask(uuid.New(), task.ID) // claimable at once
}

// RecordAttempt{ClearOwnMark} removes this claim's mark, and is accepted
// when the claim holds none (spec §13 row "database outage during
// MarkUnsafe → RecordAttempt{ClearOwnMark} after recovery, WAITING").
func testSTRecordClearOwnMark(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)
	require.NoError(t, f.sts.MarkUnsafe(f.ctx, stRef(c)))
	require.NoError(t, f.sts.RecordAttempt(f.ctx, stRef(c), spi.Attempt{
		Error: "E", AtMs: stNow, NextAttemptTime: stNow, ClearOwnMark: true}))
	require.False(t, f.mustGet(task.ID).UnsafeMarked)

	next := f.claimTask(uuid.New(), task.ID)
	require.False(t, next.UnsafeMarked)
	require.NoError(t, f.sts.RecordAttempt(f.ctx, stRef(next), spi.Attempt{
		Error: "E", AtMs: stNow, NextAttemptTime: stNow, ClearOwnMark: true}),
		"ClearOwnMark with no mark held is accepted")
	require.Equal(t, spi.ScheduledTaskWaiting, f.mustGet(task.ID).Status)
}

// Without ClearOwnMark the mark stays; ClearOwnMark never removes a mark
// another claim wrote.
func testSTRecordOtherClaimsMarkKept(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	a := f.claimTask(uuid.New(), task.ID)
	require.NoError(t, f.sts.MarkUnsafe(f.ctx, stRef(a)))
	require.NoError(t, f.sts.RecordAttempt(f.ctx, stRef(a), spi.Attempt{Error: "E", AtMs: stNow, NextAttemptTime: stNow}))

	b := f.claimTask(uuid.New(), task.ID)
	require.True(t, b.UnsafeMarked)
	require.NoError(t, f.sts.RecordAttempt(f.ctx, stRef(b), spi.Attempt{
		Error: "E", AtMs: stNow, NextAttemptTime: stNow, ClearOwnMark: true}))

	c := f.claimTask(uuid.New(), task.ID)
	require.True(t, c.UnsafeMarked, "ClearOwnMark removes only the calling claim's mark")
	require.ErrorIs(t, f.sts.MarkUnsafe(f.ctx, stRef(c)), spi.ErrMarkedByAnotherClaim)
}

// A bookkeeping write repeated after it was accepted is refused and not
// applied twice (spec §13 row "a bookkeeping write retried through an
// outage; accepted after recovery": a retry whose first attempt landed
// learns it through the refusal).
func testSTRecordRepeatRefused(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)
	a := spi.Attempt{Error: "E", AtMs: stNow, NextAttemptTime: stFuture}
	require.NoError(t, f.sts.RecordAttempt(f.ctx, stRef(c), a))
	require.ErrorIs(t, f.sts.RecordAttempt(f.ctx, stRef(c), a), spi.ErrStaleClaim)
	require.Equal(t, 1, f.mustGet(task.ID).Attempts)
}

func testSTFailFields(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	for _, reason := range []spi.ScheduledTaskFailureReason{
		spi.FailureUnsafeWorkNotCompleted, spi.FailureOwnerLostRepeatedly,
		spi.FailureExpiredAfterFailedAttempts, spi.FailureRunPanicked, spi.FailureStoppedAfterPartialCommit,
	} {
		task := f.armDue()
		c := f.claimTask(uuid.New(), task.ID)
		fl := spi.Failure{Reason: reason, Error: "internal error [ticket: " + newID() + "]", AtMs: stNow + 7}
		require.NoError(t, f.sts.Fail(f.ctx, stRef(c), fl))

		got := f.mustGet(task.ID)
		require.Equal(t, spi.ScheduledTaskFailed, got.Status)
		require.Equal(t, reason, got.FailureReason)
		require.Equal(t, fl.Error, got.LastError)
		require.NotNil(t, got.FailedTime)
		require.Equal(t, stNow+7, *got.FailedTime)
		require.Nil(t, got.LastAttemptTime, "Fail leaves LastAttemptTime unchanged")
		require.Nil(t, got.Claim)
		require.Equal(t, c.ArmToken, got.ArmToken)
		requireAllFencedRefused(t, f.ctx, f.sts, stRef(c), "a claim of a FAILED task")
	}

	// After a recorded attempt, Fail keeps its LastAttemptTime.
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)
	require.NoError(t, f.sts.RecordAttempt(f.ctx, stRef(c), spi.Attempt{Error: "E1", AtMs: stNow - 3, NextAttemptTime: stNow}))
	c = f.claimTask(uuid.New(), task.ID)
	require.NoError(t, f.sts.Fail(f.ctx, stRef(c), spi.Failure{Reason: spi.FailureRunPanicked, Error: "E2", AtMs: stNow + 7}))
	got := f.mustGet(task.ID)
	require.NotNil(t, got.LastAttemptTime)
	require.Equal(t, stNow-3, *got.LastAttemptTime, "Fail leaves LastAttemptTime unchanged")
	require.Equal(t, 1, got.Attempts, "Fail leaves Attempts unchanged")
}

// Fail replaces LastError, even with an empty text.
func testSTFailOverwritesLastError(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)
	require.NoError(t, f.sts.RecordAttempt(f.ctx, stRef(c), spi.Attempt{Error: "E1", AtMs: stNow, NextAttemptTime: stNow}))
	require.Equal(t, "E1", f.mustGet(task.ID).LastError)

	c2 := f.claimTask(uuid.New(), task.ID)
	require.NoError(t, f.sts.Fail(f.ctx, stRef(c2), spi.Failure{
		Reason: spi.FailureExpiredAfterFailedAttempts, Error: "", AtMs: stNow}))
	got := f.mustGet(task.ID)
	require.Equal(t, spi.ScheduledTaskFailed, got.Status)
	require.Empty(t, got.LastError, "Fail replaces LastError even with an empty text")
}

func testSTFailJoinsTransaction(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)
	fl := spi.Failure{Reason: spi.FailureRunPanicked, Error: "E", AtMs: stNow}

	txID, txCtx := f.begin()
	require.NoError(t, f.sts.Fail(txCtx, stRef(c), fl))
	staged, _ := f.get(txCtx, task.ID)
	require.Equal(t, spi.ScheduledTaskFailed, staged.Status, "the transaction sees its own Fail (C2)")
	require.NoError(t, f.tm.Rollback(txCtx, txID))
	requireUnchangedClaim(t, c, f.mustGet(task.ID))

	txID, txCtx = f.begin()
	require.NoError(t, f.sts.Fail(txCtx, stRef(c), fl))
	require.NoError(t, f.tm.Commit(txCtx, txID))
	require.Equal(t, spi.ScheduledTaskFailed, f.mustGet(task.ID).Status)
}

// stMaxErrorText is 1024 bytes of valid UTF-8 with multi-byte characters
// and the U+FFFD a sanitiser puts in place of a NUL.
func stMaxErrorText() string {
	return "\uFFFD" + strings.Repeat("\u00e9", 509) + "\u20ac"
}

// The longest error text a caller may send is stored intact on every
// backend (spec §13 row "lastError over 1 024 bytes with multi-byte
// characters and a NUL → cut at a character boundary, stored on every
// backend": the engine cuts, the store keeps).
func testSTErrorTextRoundTrip(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	text := stMaxErrorText()
	require.Len(t, text, 1024)
	require.True(t, utf8.ValidString(text))

	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)
	require.NoError(t, f.sts.RecordAttempt(f.ctx, stRef(c), spi.Attempt{Error: text, AtMs: stNow, NextAttemptTime: stNow}))
	require.Equal(t, text, f.mustGet(task.ID).LastError)

	c2 := f.claimTask(uuid.New(), task.ID)
	require.NoError(t, f.sts.Fail(f.ctx, stRef(c2), spi.Failure{Reason: spi.FailureRunPanicked, Error: text, AtMs: stNow}))
	require.Equal(t, text, f.mustGet(task.ID).LastError)
}

// Every store marks a deterministic rejection with ErrStoreRejected
// (spec §13 row "spi.ErrStoreRejected → ERROR with ticket, node latched
// (every backend sets the marker)"). Error text that breaks the documented
// precondition is the portable trigger.
func testSTErrorTextStoreRejected(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)
	for why, text := range map[string]string{
		"a NUL":                "a\x00b",
		"invalid UTF-8":        "a\xffb",
		"more than 1024 bytes": stMaxErrorText() + "x",
	} {
		err := f.sts.RecordAttempt(f.ctx, stRef(c), spi.Attempt{Error: text, AtMs: stNow, NextAttemptTime: stNow})
		require.ErrorIs(t, err, spi.ErrStoreRejected, "RecordAttempt with %s", why)
		err = f.sts.Fail(f.ctx, stRef(c), spi.Failure{Reason: spi.FailureRunPanicked, Error: text, AtMs: stNow})
		require.ErrorIs(t, err, spi.ErrStoreRejected, "Fail with %s", why)
		requireUnchangedClaim(t, c, f.mustGet(task.ID))
	}
	// A refused write is not a stale claim: the claim still holds.
	require.NoError(t, f.sts.RecordAttempt(f.ctx, stRef(c), spi.Attempt{Error: "E", AtMs: stNow, NextAttemptTime: stNow}))
}

// SweepMarks keeps the mark of a task's current life in every status and
// the next life starts unmarked (spec §13 row "dead owners and the marks
// of ended lives are swept"; the owner half is Liveness/Sweep*).
func testSTSweepMarksKeepsCurrentLife(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)
	require.NoError(t, f.sts.MarkUnsafe(f.ctx, stRef(c)))

	require.NoError(t, f.sts.SweepMarks(context.Background()))
	require.True(t, f.mustGet(task.ID).UnsafeMarked, "RUNNING: the mark stays")

	require.NoError(t, f.sts.RecordAttempt(f.ctx, stRef(c), spi.Attempt{Error: "E", AtMs: stNow, NextAttemptTime: stNow}))
	require.NoError(t, f.sts.SweepMarks(context.Background()))
	require.True(t, f.mustGet(task.ID).UnsafeMarked, "WAITING: the mark stays")

	c2 := f.claimTask(uuid.New(), task.ID)
	require.NoError(t, f.sts.Fail(f.ctx, stRef(c2), spi.Failure{Reason: spi.FailureUnsafeWorkNotCompleted, Error: "E", AtMs: stNow}))
	require.NoError(t, f.sts.SweepMarks(context.Background()))
	require.True(t, f.mustGet(task.ID).UnsafeMarked, "FAILED: the mark stays")

	f.reconcile(f.ctx, task.EntityID, "S", f.spec(task.EntityID, "S", "T", stDue))
	require.NoError(t, f.sts.SweepMarks(context.Background()))
	require.False(t, f.mustGet(task.ID).UnsafeMarked, "a re-armed life has no mark")
	require.False(t, f.claimTask(uuid.New(), task.ID).UnsafeMarked)
}
```

- [ ] **Step 4: Run it and see it pass**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && gofmt -l . && go vet ./...`
Expected: prints nothing; vet clean.

- [ ] **Step 5: Commit**

```
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi add spitest/scheduledtasks.go spitest/scheduledtasks_fence.go
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi commit -m "test(spitest): ScheduledTasks — fence, mark, attempt, fail, error text

ErrorText/StoreRejected is the case that every store sets ErrStoreRejected.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task S-7: ScheduledTasks — clauses C1, C2, C3, C6

**Spec:** §5.2 "Every commit writes the task row", "Deciding superseded"; §5.5
"The callback anti-pattern"; §6.3 "Why no other pnode can reclaim"; §7 "A
client write can get a retryable 409"; §10.1 clauses; §13 rows in the
coverage table.

**Files:**
- Modify: `spitest/scheduledtasks.go` (`runScheduledTasksSuite`)
- Create: `spitest/scheduledtasks_clauses.go`

**Interfaces:**
- Consumes: S-4's fixture and helpers (`stRef`, `requireFreshLife`,
  `requireUnchangedClaim`).
- Produces: subtests `C1/*`, `C2/*`, `C3/*`, `C6/*`.

An open transaction in these tests is `f.begin()` (fact 10). C1 and C6 need a
second writer while it is open: the second writer is a call with `f.ctx` or
`context.Background()`, which carries no transaction. C1's refusal may come
from the statement or from the commit (C5), and a fenced statement may refuse
earlier with `ErrStaleClaim`; `requireTxRefused` accepts exactly those. The
C3 race and the claim races run real goroutines. They are store-level races
on one backend each, not the shared parity suite
(`.claude/rules/test-coverage.md`).

C4 (own connections) is not portable to a black-box harness; it is covered by
the §13 E rows owned by BP and T.

- [ ] **Step 1: Write the failing registration** — append at the end of
  `runScheduledTasksSuite`, one blank line and:

```go
	// Clauses C1, C2, C3, C6 (S-7).
	runSubtest(t, h, tracker, "C1/ReclaimFailsOldCommit", testSTC1ReclaimFailsOldCommit)
	runSubtest(t, h, tracker, "C1/RearmFailsOldCommit", testSTC1RearmFailsOldCommit)
	runSubtest(t, h, tracker, "C1/ClientWriteAfterClaim", testSTC1ClientWriteAfterClaim)
	runSubtest(t, h, tracker, "C1/OwnClaimNoConflict", testSTC1OwnClaimNoConflict)
	runSubtest(t, h, tracker, "C2/StagedWritesVisible", testSTC2StagedWritesVisible)
	runSubtest(t, h, tracker, "C2/CallbackRearmThenRemoveLife", testSTC2CallbackRearmThenRemoveLife)
	runSubtest(t, h, tracker, "C2/CallbackRearmThenStamp", testSTC2CallbackRearmThenStamp)
	runSubtest(t, h, tracker, "C2/CallbackDeleteThenRemoveLife", testSTC2CallbackDeleteThenRemoveLife)
	runSubtest(t, h, tracker, "C3/MarkRacesClaim", testSTC3MarkRacesClaim)
	runSubtest(t, h, tracker, "C6/OpenWriteNotClaimable", testSTC6OpenWriteNotClaimable)
	runSubtest(t, h, tracker, "C6/MarkBusy", testSTC6MarkBusy)
	runSubtest(t, h, tracker, "C6/NeverJoiningWriteBounded", testSTC6NeverJoiningWriteBounded)
```

- [ ] **Step 2: Run it and see it fail**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go vet ./spitest/`
Expected: FAIL — `undefined: testSTC1ReclaimFailsOldCommit` and the others above.

- [ ] **Step 3: Implement** — create `spitest/scheduledtasks_clauses.go`:

```go
package spitest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// How these tests hold "an open transaction": f.begin() opens one through
// the harness's TransactionManager, and every joining call made with its
// txCtx is staged in it until Commit or Rollback. On PostgreSQL a
// REPEATABLE READ snapshot is taken at the transaction's first statement,
// not at Begin, so a test that needs the transaction to have begun before
// another write first reads through it (f.get(txCtx, …)). The other write
// is made with f.ctx or a background context, which carry no transaction.

// stTaskWrite is a joining write a run's transaction makes to its own task
// row.
type stTaskWrite struct {
	name  string
	write func(ctx context.Context, f *stFixture, c spi.ScheduledTask) error
}

var stRunWrites = []stTaskWrite{
	{"StampSegment", func(ctx context.Context, f *stFixture, c spi.ScheduledTask) error {
		return f.sts.StampSegment(ctx, stRef(c), false)
	}},
	{"RemoveLife", func(ctx context.Context, f *stFixture, c spi.ScheduledTask) error {
		return f.sts.RemoveLife(ctx, f.tenant, c.ID, c.ArmToken)
	}},
}

// A reclaimed task makes the old run's commit fail (C1), and the
// non-joining re-read then shows the run was superseded (spec §13 rows "a
// reclaimed or re-armed task makes the old run's commit fail (C1)" and "a
// refusal from inside the run's own transaction goes through §5.6").
func testSTC1ReclaimFailsOldCommit(t *testing.T, h Harness) {
	for _, w := range stRunWrites {
		t.Run(w.name, func(t *testing.T) {
			f := newSTFixture(t, h)
			a := f.claimTask(uuid.New(), f.armDue().ID)

			txID, txCtx := f.begin()
			_, _ = f.get(txCtx, a.ID)
			b := f.reclaim(uuid.New(), a.ID) // commits a change to the row after the run's transaction began

			stmtErr := w.write(txCtx, f, a)
			requireTxRefused(t, stmtErr, func() error { return f.tm.Commit(txCtx, txID) }, true)
			_ = f.tm.Rollback(txCtx, txID)

			got := f.mustGet(a.ID) // the classifying re-read joins no transaction
			require.Equal(t, a.ArmToken, got.ArmToken, "same life")
			require.NotEqual(t, a.Claim.Token, got.Claim.Token, "another claim: the old run was superseded")
			requireUnchangedClaim(t, b, got)
		})
	}
}

func testSTC1RearmFailsOldCommit(t *testing.T, h Harness) {
	for _, w := range stRunWrites {
		t.Run(w.name, func(t *testing.T) {
			f := newSTFixture(t, h)
			a := f.claimTask(uuid.New(), f.armDue().ID)

			txID, txCtx := f.begin()
			_, _ = f.get(txCtx, a.ID)
			s := f.spec(a.EntityID, "S", "T", stFuture)
			f.reconcile(f.ctx, a.EntityID, "S", s) // a client write re-arms the task

			stmtErr := w.write(txCtx, f, a)
			requireTxRefused(t, stmtErr, func() error { return f.tm.Commit(txCtx, txID) }, true)
			_ = f.tm.Rollback(txCtx, txID)

			got := f.mustGet(a.ID)
			requireFreshLife(t, s, got)
			require.NotEqual(t, a.ArmToken, got.ArmToken)
		})
	}
}

// An update, a delete or an import racing a claim gets a C1 conflict; the
// server's retry in a new transaction succeeds; the same on every backend
// (spec §13 row "an update racing a claim → retryable 409; a delete or
// import racing one claim succeeds after the server retry").
func testSTC1ClientWriteAfterClaim(t *testing.T, h Harness) {
	writes := []stTaskWrite{
		{"ReconcileForEntity", func(ctx context.Context, f *stFixture, c spi.ScheduledTask) error {
			_, err := f.sts.ReconcileForEntity(ctx, spi.ReconcileRequest{TenantID: f.tenant, EntityID: c.EntityID,
				CurrentState: "S", Arm: []spi.ScheduledTask{f.spec(c.EntityID, "S", "T", stFuture)}})
			return err
		}},
		{"DeleteForEntities", func(ctx context.Context, f *stFixture, c spi.ScheduledTask) error {
			return f.sts.DeleteForEntities(ctx, f.tenant, []string{c.EntityID})
		}},
		{"DeleteForModel", func(ctx context.Context, f *stFixture, c spi.ScheduledTask) error {
			return f.sts.DeleteForModel(ctx, f.tenant, f.model, 1, nil)
		}},
	}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			f := newSTFixture(t, h)
			task := f.armDue()

			txID, txCtx := f.begin()
			_, _ = f.get(txCtx, task.ID)
			c := f.claimTask(uuid.New(), task.ID) // the scheduler changes the row after the client's transaction began

			stmtErr := w.write(txCtx, f, task)
			requireTxRefused(t, stmtErr, func() error { return f.tm.Commit(txCtx, txID) }, false)
			_ = f.tm.Rollback(txCtx, txID)
			requireUnchangedClaim(t, c, f.mustGet(task.ID))

			txID, txCtx = f.begin()
			require.NoError(t, w.write(txCtx, f, task), "the retry in a new transaction is accepted")
			require.NoError(t, f.tm.Commit(txCtx, txID))
		})
	}
}

// A run never conflicts with its own claim, even when the store clock does
// not move between the claim and the run's transactions (spec §13 row
// "SQLite: a run never conflicts with its own claim under a frozen
// clock"). The test never advances the clock.
func testSTC1OwnClaimNoConflict(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	task := f.armDue()
	c := f.claimTask(uuid.New(), task.ID)

	for _, partial := range []bool{false, true} {
		txID, txCtx := f.begin()
		got, found := f.get(txCtx, task.ID)
		require.True(t, found)
		require.Equal(t, c.Claim.Token, got.Claim.Token)
		require.NoError(t, f.sts.StampSegment(txCtx, stRef(c), partial))
		require.NoError(t, f.tm.Commit(txCtx, txID), "a segment commit right after the run's own claim")
	}
	txID, txCtx := f.begin()
	require.NoError(t, f.sts.RemoveLife(txCtx, f.tenant, task.ID, c.ArmToken))
	require.NoError(t, f.tm.Commit(txCtx, txID))
	f.requireGone(task.ID)
}

// A joining read sees its own staged writes (C2).
func testSTC2StagedWritesVisible(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	running := f.claimTask(uuid.New(), f.armDue().ID)
	removable := f.arm(stFuture)
	e := f.newEntity()
	s := f.spec(e, "S", "T", stFuture)

	txID, txCtx := f.begin()

	f.reconcile(txCtx, e, "S", s)
	staged, found := f.get(txCtx, s.ID)
	require.True(t, found, "a staged arm is visible inside the transaction")
	requireFreshLife(t, s, staged)
	_, found = f.get(f.ctx, s.ID)
	require.False(t, found, "and not outside it")

	require.NoError(t, f.sts.StampSegment(txCtx, stRef(running), true))
	stamped, _ := f.get(txCtx, running.ID)
	require.True(t, stamped.PartialCommit)
	require.False(t, f.mustGet(running.ID).PartialCommit)

	require.NoError(t, f.sts.RemoveLife(txCtx, f.tenant, removable.ID, removable.ArmToken))
	_, found = f.get(txCtx, removable.ID)
	require.False(t, found)
	f.mustGet(removable.ID)

	require.NoError(t, f.sts.DeleteForEntities(txCtx, f.tenant, []string{e}))
	_, found = f.get(txCtx, s.ID)
	require.False(t, found, "a staged removal after a staged arm")

	require.NoError(t, f.tm.Commit(txCtx, txID))
	f.requireGone(s.ID)
	f.requireGone(removable.ID)
	require.True(t, f.mustGet(running.ID).PartialCommit)
}

// A joined callback writes the fired entity and no unsafe processor
// follows: the run's own RemoveLife finds the life replaced and the run
// commits (spec §13).
func testSTC2CallbackRearmThenRemoveLife(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	c := f.claimTask(uuid.New(), f.armDue().ID)
	s := f.spec(c.EntityID, "S", "T", stFuture)

	txID, txCtx := f.begin()
	f.reconcile(txCtx, c.EntityID, "S", s) // the callback's write re-arms the task in the run's transaction
	require.NoError(t, f.sts.RemoveLife(txCtx, f.tenant, c.ID, c.ArmToken))
	require.NoError(t, f.tm.Commit(txCtx, txID))
	requireFreshLife(t, s, f.mustGet(c.ID))
}

// A joined callback writes the fired entity in a segmented run: the stamp
// is refused, and the non-joining re-read after the rollback shows the
// life and claim unchanged — an ordinary failure (spec §13).
func testSTC2CallbackRearmThenStamp(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	c := f.claimTask(uuid.New(), f.armDue().ID)

	txID, txCtx := f.begin()
	f.reconcile(txCtx, c.EntityID, "S", f.spec(c.EntityID, "S", "T", stFuture))
	require.ErrorIs(t, f.sts.StampSegment(txCtx, stRef(c), false), spi.ErrStaleClaim,
		"the stamp sees the transaction's own re-arm (C2)")
	require.NoError(t, f.tm.Rollback(txCtx, txID))
	requireUnchangedClaim(t, c, f.mustGet(c.ID))
}

// A joined callback deletes the fired entity: the run commits (spec §13).
func testSTC2CallbackDeleteThenRemoveLife(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	c := f.claimTask(uuid.New(), f.armDue().ID)

	txID, txCtx := f.begin()
	require.NoError(t, f.sts.DeleteForEntities(txCtx, f.tenant, []string{c.EntityID}))
	require.NoError(t, f.sts.RemoveLife(txCtx, f.tenant, c.ID, c.ArmToken))
	require.NoError(t, f.tm.Commit(txCtx, txID))
	f.requireGone(c.ID)
}

// MarkUnsafe racing ClaimDue (C3): an accepted mark is always seen by the
// claim that races it or the one after; a refused mark leaves none.
func testSTC3MarkRacesClaim(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	b := uuid.New()
	require.NoError(t, f.sts.Heartbeat(context.Background(), b)) // B's claims stay B's
	for i := 0; i < 20; i++ {
		task := f.armDue()
		a := f.claimTask(uuid.New(), task.ID) // A never heartbeats: lost to any lost-owner claim

		var markErr, claimErr error
		var claimed []spi.ScheduledTask
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			markErr = f.sts.MarkUnsafe(context.Background(), stRef(a))
		}()
		go func() {
			defer wg.Done()
			<-start
			claimed, claimErr = f.sts.ClaimDue(context.Background(), f.claimReq(b, true))
		}()
		close(start)
		wg.Wait()
		require.NoError(t, claimErr)

		got := findST(f.own(claimed), task.ID)
		if got == nil { // B claimed nothing this tick; its next claim decides
			c := f.reclaim(b, task.ID)
			got = &c
		}
		if markErr == nil {
			require.True(t, got.UnsafeMarked, "iteration %d: an accepted mark must reach the claim", i)
		} else {
			require.True(t, errors.Is(markErr, spi.ErrStaleClaim) || errors.Is(markErr, spi.ErrTaskBusy),
				"iteration %d: a mark that loses the race is refused, got %v", i, markErr)
			require.False(t, got.UnsafeMarked, "iteration %d: a refused mark leaves none", i)
		}
	}
}

// stOpenWrites are joining writes that leave a task row written by an open
// transaction. lost says whether the row is claimable only by a lost-owner
// claim.
var stOpenWrites = []struct {
	name    string
	lost    bool
	prepare func(f *stFixture) spi.ScheduledTask
	write   func(ctx context.Context, f *stFixture, task spi.ScheduledTask) error
}{
	{"StagedRearm", false,
		func(f *stFixture) spi.ScheduledTask { return f.armDue() },
		func(ctx context.Context, f *stFixture, task spi.ScheduledTask) error {
			_, err := f.sts.ReconcileForEntity(ctx, spi.ReconcileRequest{TenantID: f.tenant, EntityID: task.EntityID,
				CurrentState: "S", Arm: []spi.ScheduledTask{f.spec(task.EntityID, "S", "T", stDue)}})
			return err
		}},
	{"StagedDelete", false,
		func(f *stFixture) spi.ScheduledTask { return f.armDue() },
		func(ctx context.Context, f *stFixture, task spi.ScheduledTask) error {
			return f.sts.DeleteForEntities(ctx, f.tenant, []string{task.EntityID})
		}},
	{"StagedStamp", true,
		func(f *stFixture) spi.ScheduledTask { return f.claimTask(uuid.New(), f.armDue().ID) },
		func(ctx context.Context, f *stFixture, task spi.ScheduledTask) error {
			return f.sts.StampSegment(ctx, stRef(task), false)
		}},
}

// A row written by an open transaction is not claimable (C6), and the claim
// skips it rather than waiting.
func testSTC6OpenWriteNotClaimable(t *testing.T, h Harness) {
	for _, w := range stOpenWrites {
		t.Run(w.name, func(t *testing.T) {
			f := newSTFixture(t, h)
			task := w.prepare(f)
			txID, txCtx := f.begin()
			require.NoError(t, w.write(txCtx, f, task))

			ctx, cancel := context.WithTimeout(context.Background(), stWait)
			defer cancel()
			res, err := f.sts.ClaimDue(ctx, f.claimReq(uuid.New(), w.lost))
			require.NoError(t, err, "a claim skips a row an open transaction wrote; it does not wait for it")
			require.Nil(t, findST(f.own(res), task.ID))

			require.NoError(t, f.tm.Rollback(txCtx, txID))
			require.NotNil(t, findST(f.claimWith(f.claimReq(uuid.New(), w.lost)), task.ID),
				"claimable once the transaction ended")
		})
	}
}

// MarkUnsafe answers ErrTaskBusy for a row an open transaction wrote (C6),
// including the row a joined callback re-armed or removed (spec §13 rows
// "ErrTaskBusy → safe failure" and "joined callback writes the fired
// entity, then an unsafe processor → ErrTaskBusy, safe failure, no hang").
func testSTC6MarkBusy(t *testing.T, h Harness) {
	writes := []stTaskWrite{
		{"Stamp", func(ctx context.Context, f *stFixture, c spi.ScheduledTask) error {
			return f.sts.StampSegment(ctx, stRef(c), false)
		}},
		{"CallbackRearm", func(ctx context.Context, f *stFixture, c spi.ScheduledTask) error {
			_, err := f.sts.ReconcileForEntity(ctx, spi.ReconcileRequest{TenantID: f.tenant, EntityID: c.EntityID,
				CurrentState: "S", Arm: []spi.ScheduledTask{f.spec(c.EntityID, "S", "T", stFuture)}})
			return err
		}},
		{"CallbackDelete", func(ctx context.Context, f *stFixture, c spi.ScheduledTask) error {
			return f.sts.DeleteForEntities(ctx, f.tenant, []string{c.EntityID})
		}},
	}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			f := newSTFixture(t, h)
			c := f.claimTask(uuid.New(), f.armDue().ID)
			txID, txCtx := f.begin()
			require.NoError(t, w.write(txCtx, f, c))

			ctx, cancel := context.WithTimeout(context.Background(), stWait)
			defer cancel()
			require.ErrorIs(t, f.sts.MarkUnsafe(ctx, stRef(c)), spi.ErrTaskBusy)
			require.NoError(t, ctx.Err(), "MarkUnsafe answers at once; it does not wait for the transaction")

			require.NoError(t, f.tm.Rollback(txCtx, txID))
			require.False(t, f.mustGet(c.ID).UnsafeMarked, "a busy answer writes no mark")
			require.NoError(t, f.sts.MarkUnsafe(f.ctx, stRef(c)), "accepted once the transaction ended")
		})
	}
}

// A never-joining write that meets a row an open transaction wrote gives up
// on its own, or is applied and makes that transaction's commit fail; a
// retry after the lock is gone is accepted (spec §13 rows "a
// scheduler-pool statement blocked on a task-row lock gives up after
// lock_timeout" and "a bookkeeping write retried through an outage;
// accepted after recovery").
func testSTC6NeverJoiningWriteBounded(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	c := f.claimTask(uuid.New(), f.armDue().ID)
	txID, txCtx := f.begin()
	require.NoError(t, f.sts.StampSegment(txCtx, stRef(c), false))

	attempt := spi.Attempt{Error: "E", AtMs: stNow, NextAttemptTime: stFuture}
	ctx, cancel := context.WithTimeout(context.Background(), stWait)
	defer cancel()
	recErr := f.sts.RecordAttempt(ctx, stRef(c), attempt)
	require.NoError(t, ctx.Err(), "the store must give up on its own, not run into the caller's deadline")

	if recErr != nil {
		require.NotErrorIs(t, recErr, spi.ErrStaleClaim, "a lock wait is not a refusal")
		require.NotErrorIs(t, recErr, spi.ErrStoreRejected, "a lock wait is retryable")
		requireUnchangedClaim(t, c, f.mustGet(c.ID))
		require.NoError(t, f.tm.Rollback(txCtx, txID))
		require.NoError(t, f.sts.RecordAttempt(context.Background(), stRef(c), attempt), "the retry is accepted")
	} else {
		require.ErrorIs(t, f.tm.Commit(txCtx, txID), spi.ErrConflict,
			"the write was applied at once, so the open transaction's commit fails (C1)")
	}
	got := f.mustGet(c.ID)
	require.Equal(t, spi.ScheduledTaskWaiting, got.Status)
	require.Equal(t, 1, got.Attempts)
}

// requireTxRefused asserts that a transaction's task-row write was refused
// under C1: by the statement, or by the commit. A C1 refusal satisfies
// ErrConflict (C5). With allowStale, a fenced statement may refuse earlier
// with ErrStaleClaim instead — a backend that checks the fence against the
// latest committed row sees the other transaction's change at once.
func requireTxRefused(t *testing.T, stmtErr error, commit func() error, allowStale bool) {
	t.Helper()
	if stmtErr != nil {
		if allowStale && errors.Is(stmtErr, spi.ErrStaleClaim) {
			return
		}
		require.ErrorIs(t, stmtErr, spi.ErrConflict)
		return
	}
	require.ErrorIs(t, commit(), spi.ErrConflict,
		"the commit of a transaction whose task row another transaction changed since it began must fail")
}
```

- [ ] **Step 4: Run it and see it pass**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && gofmt -l . && go vet ./...`
Expected: prints nothing; vet clean.

- [ ] **Step 5: Commit**

```
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi add spitest/scheduledtasks.go spitest/scheduledtasks_clauses.go
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi commit -m "test(spitest): ScheduledTasks — first-committer-wins, staged reads, mark/claim race, open-write rows

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task S-8: ScheduledTasks — query and tenant isolation

**Spec:** §8 (order, cursor, filters, "There is no 404"); §10.1 `Query`, tenant
scoping; §13 `GET /scheduled-tasks` S rows.

**Files:**
- Modify: `spitest/scheduledtasks.go` (`runScheduledTasksSuite`)
- Create: `spitest/scheduledtasks_query.go`
- Modify: `spitest/audit.go` (`runAuditSuite` `:13-20`; one test at the end of the file)

**Interfaces:**
- Consumes: S-4's fixture; the audit suite's `newSMEvent` (`spitest/audit.go:22-30`).
- Produces: subtests `Query/*`, `TenantIsolation/EveryMethod`,
  `Tenant/JoiningWriteOtherTenantRefused` (README C-S5), and in the `Audit`
  group `RolledBackEventNotKept` (README C-S4).

`Audit/RolledBackEventNotKept` binds every backend, Cassandra included: an
audit event recorded in a transaction that rolls back is not kept. Today only
PostgreSQL passes it; BM-6 and BQ-7 make memory and SQLite pass it (their
behavioural RED), and D-11 tells Cassandra.

- [ ] **Step 1: Write the failing registration** — append at the end of
  `runScheduledTasksSuite`, one blank line and:

```go
	// Query and tenant isolation (S-8).
	runSubtest(t, h, tracker, "Query/PagesInOrder", testSTQueryPagesInOrder)
	runSubtest(t, h, tracker, "Query/Filters", testSTQueryFilters)
	runSubtest(t, h, tracker, "Query/TenantIsolation", testSTQueryTenantIsolation)
	runSubtest(t, h, tracker, "TenantIsolation/EveryMethod", testSTTenantIsolationEveryMethod)
	runSubtest(t, h, tracker, "Tenant/JoiningWriteOtherTenantRefused", testSTTenantJoiningWriteOtherTenantRefused)
```

and in `spitest/audit.go`, append to `runAuditSuite` (after the
`TenantIsolation` line, `:19`):

```go
	runSubtest(t, h, tracker, "RolledBackEventNotKept", testAuditRolledBackEventNotKept)
```

- [ ] **Step 2: Run it and see it fail**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go vet ./spitest/`
Expected: FAIL — `undefined: testSTQueryPagesInOrder` and the others above,
`undefined: testSTTenantJoiningWriteOtherTenantRefused`,
`undefined: testAuditRolledBackEventNotKept`.

- [ ] **Step 3: Implement** — create `spitest/scheduledtasks_query.go`:

```go
package spitest

import (
	"cmp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func (f *stFixture) query(q spi.ScheduledTaskQuery) spi.ScheduledTaskPage {
	f.t.Helper()
	page, err := f.sts.Query(f.ctx, f.tenant, q)
	require.NoError(f.t, err)
	return page
}

func (f *stFixture) queryEntities(q spi.ScheduledTaskQuery) []string {
	f.t.Helper()
	q.Limit = 1000
	var out []string
	for _, x := range f.query(q).Items {
		out = append(out, x.EntityID)
	}
	return out
}

// 200, no filter, several pages (spec §13, GET /scheduled-tasks).
func testSTQueryPagesInOrder(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	var want []spi.ScheduledTask
	// Two tasks share a ScheduledTime, so the ID breaks the tie.
	for _, at := range []int64{stFuture + 30, stFuture + 10, stFuture + 20, stFuture + 10, stFuture + 40} {
		want = append(want, f.arm(at))
	}
	slices.SortFunc(want, func(a, b spi.ScheduledTask) int {
		if c := cmp.Compare(a.ScheduledTime, b.ScheduledTime); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})

	var got []string
	var after *spi.ScheduledTaskCursor
	pages := 0
	for {
		page := f.query(spi.ScheduledTaskQuery{After: after, Limit: 2})
		pages++
		require.LessOrEqual(t, pages, 3, "5 tasks at 2 per page is 3 pages")
		got = append(got, stIDs(page.Items)...)
		if page.Next == nil {
			break
		}
		require.Len(t, page.Items, 2, "only the last page is short")
		last := page.Items[len(page.Items)-1]
		require.Equal(t, spi.ScheduledTaskCursor{ScheduledTime: last.ScheduledTime, ID: last.ID}, *page.Next)
		after = page.Next
	}
	require.Equal(t, stIDs(want), got, "(ScheduledTime, ID) order, IDs compared byte-wise")

	whole := f.query(spi.ScheduledTaskQuery{Limit: 5})
	require.Len(t, whole.Items, 5)
	require.Nil(t, whole.Next, "no further page when exactly Limit tasks remain")

	for i, x := range whole.Items {
		requireFreshLife(t, want[i], x) // Query returns whole records
	}
}

// 200, each filter: status (one and several), model name, name and
// version, entity (spec §13, GET /scheduled-tasks).
func testSTQueryFilters(t *testing.T, h Harness) {
	f := newSTFixture(t, h)
	waiting := f.arm(stFuture)
	running := f.claimTask(uuid.New(), f.armDue().ID)
	failed := f.claimTask(uuid.New(), f.armDue().ID)
	require.NoError(t, f.sts.Fail(f.ctx, stRef(failed), spi.Failure{Reason: spi.FailureRunPanicked, Error: "E", AtMs: stNow}))

	v2 := f.spec(f.newEntity(), "S", "T", stFuture)
	v2.ModelVersion = 2
	f.reconcile(f.ctx, v2.EntityID, "S", v2)
	other := f.spec(f.newEntity(), "S", "T", stFuture)
	other.ModelName = f.model + "-other"
	f.reconcile(f.ctx, other.EntityID, "S", other)

	W, R, F := spi.ScheduledTaskWaiting, spi.ScheduledTaskRunning, spi.ScheduledTaskFailed
	for name, tc := range map[string]struct {
		q    spi.ScheduledTaskQuery
		want []string
	}{
		"no filter":              {spi.ScheduledTaskQuery{}, []string{waiting.EntityID, running.EntityID, failed.EntityID, v2.EntityID, other.EntityID}},
		"WAITING":                {spi.ScheduledTaskQuery{Statuses: []spi.ScheduledTaskStatus{W}}, []string{waiting.EntityID, v2.EntityID, other.EntityID}},
		"RUNNING":                {spi.ScheduledTaskQuery{Statuses: []spi.ScheduledTaskStatus{R}}, []string{running.EntityID}},
		"FAILED":                 {spi.ScheduledTaskQuery{Statuses: []spi.ScheduledTaskStatus{F}}, []string{failed.EntityID}},
		"RUNNING or FAILED":      {spi.ScheduledTaskQuery{Statuses: []spi.ScheduledTaskStatus{R, F}}, []string{running.EntityID, failed.EntityID}},
		"model name":             {spi.ScheduledTaskQuery{ModelName: f.model}, []string{waiting.EntityID, running.EntityID, failed.EntityID, v2.EntityID}},
		"model name and version": {spi.ScheduledTaskQuery{ModelName: f.model, ModelVersion: 2}, []string{v2.EntityID}},
		"entity":                 {spi.ScheduledTaskQuery{EntityID: running.EntityID}, []string{running.EntityID}},
		"status and model":       {spi.ScheduledTaskQuery{Statuses: []spi.ScheduledTaskStatus{W}, ModelName: f.model, ModelVersion: 1}, []string{waiting.EntityID}},
		"unknown entity":         {spi.ScheduledTaskQuery{EntityID: newID()}, nil},
		"unknown model":          {spi.ScheduledTaskQuery{ModelName: "st-unknown-" + newID()}, nil},
	} {
		require.ElementsMatch(t, tc.want, f.queryEntities(tc.q), name)
	}

	// A FAILED item carries its reason, error and times.
	page := f.query(spi.ScheduledTaskQuery{EntityID: failed.EntityID, Limit: 1})
	require.Len(t, page.Items, 1)
	got := page.Items[0]
	require.Equal(t, spi.FailureRunPanicked, got.FailureReason)
	require.Equal(t, "E", got.LastError)
	require.NotNil(t, got.FailedTime)
}

// Another tenant's tasks are never returned, under any filter (spec §13,
// GET /scheduled-tasks).
func testSTQueryTenantIsolation(t *testing.T, h Harness) {
	fa := newSTFixture(t, h)
	fb := newSTFixture(t, h)
	fb.model = fa.model
	a := fa.arm(stFuture)
	b := fb.arm(stFuture)
	bRunning := fb.claimTask(uuid.New(), fb.armDue().ID)

	all := []spi.ScheduledTaskStatus{spi.ScheduledTaskWaiting, spi.ScheduledTaskRunning, spi.ScheduledTaskFailed}
	for name, q := range map[string]spi.ScheduledTaskQuery{
		"no filter":        {},
		"every status":     {Statuses: all},
		"shared model":     {ModelName: fa.model},
		"shared model v1":  {ModelName: fa.model, ModelVersion: 1},
		"B's entity":       {EntityID: b.EntityID},
		"B's RUNNING task": {EntityID: bRunning.EntityID, Statuses: []spi.ScheduledTaskStatus{spi.ScheduledTaskRunning}},
	} {
		for _, e := range fa.queryEntities(q) {
			require.Equal(t, a.EntityID, e, "%s: tenant A sees only its own task", name)
		}
	}
	require.Empty(t, fa.queryEntities(spi.ScheduledTaskQuery{EntityID: b.EntityID}))
}

// Every tenant-facing method is scoped to its tenant argument.
func testSTTenantIsolationEveryMethod(t *testing.T, h Harness) {
	fa := newSTFixture(t, h)
	fb := newSTFixture(t, h)
	fb.model = fa.model
	c := fa.claimTask(uuid.New(), fa.armDue().ID)

	_, found := fb.get(fb.ctx, c.ID)
	require.False(t, found, "Get")
	require.NoError(t, fb.sts.RemoveLife(fb.ctx, fb.tenant, c.ID, c.ArmToken))
	require.NoError(t, fb.sts.DeleteForEntities(fb.ctx, fb.tenant, []string{c.EntityID}))
	require.NoError(t, fb.sts.DeleteForModel(fb.ctx, fb.tenant, fa.model, 1, nil))
	removed, err := fb.sts.ReconcileForEntity(fb.ctx, spi.ReconcileRequest{
		TenantID: fb.tenant, EntityID: c.EntityID, CurrentState: "S2"})
	require.NoError(t, err)
	require.Empty(t, removed, "ReconcileForEntity removes only its tenant's tasks")
	crossRef := stRef(c)
	crossRef.TenantID = fb.tenant
	requireAllFencedRefused(t, fb.ctx, fb.sts, crossRef, "another tenant's task")

	requireUnchangedClaim(t, c, fa.mustGet(c.ID))
}

// A joining write whose tenant is not the tenant of the transaction on ctx is
// refused with ErrTxTenantMismatch and changes nothing (README C-S5): a task
// row of tenant B never enters tenant A's transaction.
func testSTTenantJoiningWriteOtherTenantRefused(t *testing.T, h Harness) {
	fb := newSTFixture(t, h)
	fa := newSTFixture(t, h)
	fa.model = fb.model
	c := fb.claimTask(uuid.New(), fb.armDue().ID)
	_, txCtx := fa.begin() // tenant A's transaction; rolled back first at cleanup

	_, err := fa.sts.ReconcileForEntity(txCtx, spi.ReconcileRequest{
		TenantID: fb.tenant, EntityID: c.EntityID, CurrentState: "S",
		Arm: []spi.ScheduledTask{fb.spec(c.EntityID, "S", "T2", stFuture)}})
	require.ErrorIs(t, err, spi.ErrTxTenantMismatch, "ReconcileForEntity")
	require.ErrorIs(t, fa.sts.RemoveLife(txCtx, fb.tenant, c.ID, c.ArmToken), spi.ErrTxTenantMismatch, "RemoveLife")
	require.ErrorIs(t, fa.sts.StampSegment(txCtx, stRef(c), true), spi.ErrTxTenantMismatch, "StampSegment")
	require.ErrorIs(t, fa.sts.DeleteForEntities(txCtx, fb.tenant, []string{c.EntityID}), spi.ErrTxTenantMismatch, "DeleteForEntities")
	require.ErrorIs(t, fa.sts.DeleteForModel(txCtx, fb.tenant, fb.model, 1, nil), spi.ErrTxTenantMismatch, "DeleteForModel")
	require.ErrorIs(t, fa.sts.Fail(txCtx, stRef(c), spi.Failure{Reason: spi.FailureRunPanicked, Error: "E", AtMs: stNow}),
		spi.ErrTxTenantMismatch, "Fail")

	requireUnchangedClaim(t, c, fb.mustGet(c.ID))
}
```

Append to `spitest/audit.go`:

```go
// testAuditRolledBackEventNotKept pins that audit events are bound to the
// transaction on every backend: an event recorded in a transaction that
// rolls back is not kept, and one recorded in a transaction that commits is.
func testAuditRolledBackEventNotKept(t *testing.T, h Harness) {
	ctx := tenantContext(h.NewTenant())
	tm, err := h.Factory.TransactionManager(ctx)
	require.NoError(t, err)
	as, err := h.Factory.StateMachineAuditStore(ctx)
	require.NoError(t, err)
	entityID := newID()

	txID, txCtx, err := tm.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, as.Record(txCtx, entityID, newSMEvent(txID, "B", "A->B")))
	require.NoError(t, tm.Rollback(txCtx, txID))

	events, err := as.GetEvents(ctx, entityID)
	require.NoError(t, err)
	require.Empty(t, events, "an event recorded in a rolled-back transaction must not be kept")
	byTx, err := as.GetEventsByTransaction(ctx, entityID, txID)
	require.NoError(t, err)
	require.Empty(t, byTx, "an event recorded in a rolled-back transaction must not be kept")

	txID2, txCtx2, err := tm.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, as.Record(txCtx2, entityID, newSMEvent(txID2, "C", "B->C")))
	require.NoError(t, tm.Commit(txCtx2, txID2))
	events, err = as.GetEvents(ctx, entityID)
	require.NoError(t, err)
	require.Len(t, events, 1, "an event recorded in a committed transaction is kept")
	require.Equal(t, "C", events[0].State)
}
```

- [ ] **Step 4: Run it and see it pass**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && gofmt -l . && go vet ./...`
Expected: prints nothing; vet clean.

- [ ] **Step 5: Commit**

```
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi add spitest/scheduledtasks.go spitest/scheduledtasks_query.go spitest/audit.go
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi commit -m "test(spitest): ScheduledTasks — query pages, filters and tenant isolation

A joining write of another tenant than the transaction's is refused.
Audit: an event recorded in a rolled-back transaction is not kept.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task S-9: CHANGELOG, exit checks, local composition

**Spec:** §12 "SPI"; §15; README "SPI mid-milestone"; SPI `MAINTAINING.md`
"Deprecation policy" (a breaking change is listed under `### Breaking` with
migration notes).

**Files:**
- Modify: `CHANGELOG.md` (`## [Unreleased]`, `:13`)
- cyoda-go worktree: `go.work` (local, never committed)

**Interfaces:**
- Consumes: S-1..S-8 and S-3a.
- Produces: the stream's hand-off state (see "Stream interface summary").

No test is written: this task changes a changelog and runs checks.

- [ ] **Step 1: CHANGELOG** — insert directly under `## [Unreleased]`
  (`CHANGELOG.md:13`), before `### Added`:

```markdown
### Breaking

- **`ScheduledTaskStore` is replaced: every scheduled run has one owner.**
  A task now has lives. `ReconcileForEntity` arms each task as a new life
  with a store-drawn `ArmToken` and removes every other task of the entity.
  A pnode claims due tasks with `ClaimDue` (cross-tenant, disjoint across
  callers, at most one RUNNING task per entity, per-tenant limits, lost-owner
  claims against `Heartbeat` liveness records) and every write it makes as
  the owner is fenced by a `TaskRef` (`StampSegment`, `MarkUnsafe`,
  `RecordAttempt`, `Fail`; `ErrStaleClaim` otherwise). `GiveBackIdle`,
  `RetireOwner`, `SweepOwners`, `SweepMarks`, `RemoveLife`,
  `DeleteForEntities`, `DeleteForModel`, and a tenant-scoped `Query` complete
  the interface; `Get` takes the tenant. Clauses C1-C6 on the interface doc
  bind every backend: first-committer-wins on task rows, joining reads that
  see staged writes, a mark and a claim that serialise, own connections for
  heartbeats and claims, recognisable refusals, and rows written by an open
  transaction that are neither claimable nor markable. `ScheduledTask` gains
  `Status`, `ArmToken`, `NextAttemptTime`, `Attempts`, `LostOwners`,
  `LastAttemptTime`, `LastError`, `FailureReason`, `FailedTime`,
  `PartialCommit`, `Claim` and `UnsafeMarked`, and loses the redispatch
  throttle and its counter. `Upsert`, `ScanDue`, `MarkRedispatch`, `Delete`
  and the root-package `RunScheduledTaskStoreConformance` are removed.

  Migration: implement the interface as documented on
  `persistence.go`'s `ScheduledTaskStore`, and run the new `spitest`
  ScheduledTasks group through `StoreFactoryConformance` instead of
  `RunScheduledTaskStoreConformance`. A backend that has no scheduled-task
  store returns an error satisfying `errors.Is(err, errors.ErrUnsupported)`
  from `StoreFactory.ScheduledTaskStore`; the group then skips. Wrap every
  deterministic rejection (bad input, SQL data or constraint errors) so that
  `errors.Is(err, spi.ErrStoreRejected)` holds, and no other error.
```

and, under `### Added`, as the first entries:

```markdown
- **`ErrMarkedByAnotherClaim`, `ErrTaskBusy`, `ErrStoreRejected`.** The two
  refusals of `ScheduledTaskStore.MarkUnsafe`, and the marker a store puts
  on a deterministic rejection so a caller can tell "retrying cannot help"
  from an outage. `ErrStaleClaim`'s comment now names the scheduled-task
  fence and `AsyncSearchStore.Release`; its value and message are unchanged.
- **`SMEventScheduledTransitionFailed` (`SCHEDULED_TRANSITION_FAIL`).** The
  audit event recorded with a task that ends FAILED.
- **`ScheduledTask.ClaimedFromLostOwner`.** Read-only, not serialised, set
  only on a `ClaimDue` result that took the task from a stale or missing
  owner.
- **Shared scheduled-task helpers for backends.** `SelectClaims` picks one
  `ClaimDue` call's tasks (one per entity, per-tenant limits, tenants taking
  turns); `ValidateTaskErrorText`, `ValidateFailureReason` and `ValidateArm`
  refuse what no backend stores, with `ErrStoreRejected`; `MaxTaskErrorBytes`.
- **`spitest` `Audit/RolledBackEventNotKept`.** An audit event recorded in a
  transaction that rolls back is not kept, on every backend.
- **`spitest` ScheduledTasks group.** Covers every `ScheduledTaskStore`
  method, every refusal and clauses C1, C2, C3, C5 and C6, including a mark
  that survives the rollback of the transaction on ctx, the refusal of every
  fenced write of a re-armed life, one claim for two due siblings, a row
  written by an open transaction that is not claimed, a lost-owner claim that
  is flagged, and a joining write of another tenant that is refused.
```

- [ ] **Step 2: Exit checks (spec §15), inside the SPI**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && git grep -n -e RedispatchAfter -e RedispatchBackoff -e MarkRedispatch -e AttemptCount -e ScanDue \
  -e redispatch_after -e attempt_count -e LowestLiveNodeID -e 'scheduler\.RoundRobin' -e SchedulerRPC \
  -e ClusterExecutor -e dispatch/scheduled-task -e DispatchForwardTimeout \
  -e CYODA_SCHEDULER_DISTRIBUTION -e CYODA_SCHEDULER_COORDINATOR -e CYODA_SCHEDULER_REDISPATCH_BACKOFF \
  -e CYODA_SCHEDULER_BATCH_SIZE -e CYODA_SCHEDULER_EXPIRY_GRACE -e CYODA_DISPATCH_FORWARD_TIMEOUT \
  -e ExpiryGrace -e expiryGrace \
  -- . ':!*/migrations/*' ':!*migration*_test.go' ':!docs/plans' ':!docs/superpowers' ':!docs/release-notes' ':!CHANGELOG.md'
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && git grep -n -e '\.Upsert(ctx, task' -e 'sts\.Delete(' -- '*.go'
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && git grep -n 'RunScheduledTaskStoreConformance' -- '*.go'
```

Expected: all three print nothing.

- [ ] **Step 3: Whole-module verification**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && gofmt -l . && go vet ./... && go test ./...`
Expected: `gofmt -l` prints nothing; vet clean; every package `ok` except
`spitest` `[no test files]`.

- [ ] **Step 4: Commit**

```
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi add CHANGELOG.md
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi commit -m "docs(changelog): the scheduled-task store is replaced

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

- [ ] **Step 5: Compose locally (cyoda-go worktree; nothing is committed)**

```
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership && go work edit -use /Users/paul/go-projects/cyoda-light/cyoda-go-spi && git status --short go.work
```

Expected: ` M go.work`. From here the cyoda-go build fails until BM, BQ, BP
(plugins), E, W and R (root) implement the interface; each stream iterates on
its own packages. Every stream worktree that needs the new SPI runs the same
`go work edit -use` line in its own worktree. Before every commit in cyoda-go:
`git diff --cached --name-only` must not list `go.work`.

---

## §13 coverage carried by this stream (column S)

Every §13 row with a ✓ in S, and the subtest (path under
`StoreFactoryConformance/ScheduledTasks/`) that carries it. Where the row's
outcome is the engine's (FAILED, retry, latch), the S subtest pins the store
half the engine relies on; the U/E/M half is owned by the stream named in
README.

| # | §13 row | Subtest(s) |
|---|---|---|
| 1 | self-loop fires and re-arms the same id as a new life | `Arm/SelfLoopRearmsRunning` |
| 2 | database outage during `MarkUnsafe` → `RecordAttempt{ClearOwnMark}` after recovery, WAITING | `Record/ClearOwnMark` (accepted with and without a mark held) |
| 3 | `RecordAttempt{ClearOwnMark}` retried in an outage; pnode dies first → FAILED at the next claim | `Mark/MarkedByAnotherClaim` (the next claim returns `UnsafeMarked`), `Record/OtherClaimsMarkKept` |
| 4 | `ErrMarkedByAnotherClaim` → FAILED | `Mark/MarkedByAnotherClaim`, `Record/OtherClaimsMarkKept` |
| 5 | `ErrTaskBusy` → safe failure | `C6/MarkBusy/Stamp` |
| 6 | owner killed after a cascade-step commit → next claim FAILED `STOPPED_AFTER_PARTIAL_COMMIT` | `Stamp/PartialCommit` |
| 7 | joined callback writes the fired entity, no unsafe processor follows | `C2/CallbackRearmThenRemoveLife` |
| 8 | joined callback writes the fired entity, then an unsafe processor → `ErrTaskBusy`, no hang | `C6/MarkBusy/CallbackRearm`, `C6/MarkBusy/CallbackDelete` |
| 9 | joined callback writes the fired entity in a segmented run → stamp refused, re-read classifies | `C2/CallbackRearmThenStamp` |
| 10 | joined callback deletes the fired entity → the run commits | `C2/CallbackDeleteThenRemoveLife` |
| 11 | `lastError` over 1 024 bytes, multi-byte, NUL → stored on every backend | `ErrorText/RoundTrip` (1 024 bytes with U+FFFD, `é`, `€`) |
| 12 | a bookkeeping write retried through an outage; accepted after recovery | `Record/RepeatRefused`, `C6/NeverJoiningWriteBounded` |
| 13 | `spi.ErrStoreRejected` (every backend sets the marker) | `ErrorText/StoreRejected` |
| 14 | concurrent `ClaimDue` calls get disjoint sets | `Claim/ConcurrentDisjoint` |
| 15 | two due siblings: one claim per call | `Claim/OnePerEntity` |
| 16 | two due siblings, two pnodes at once | `Claim/SiblingsConcurrent` |
| 17 | contended claim loop: never re-claimed | `Claim/ContendedNoReclaim` |
| 18 | per-tenant limit and turn-taking | `Claim/PerTenantLimit`, `Claim/TenantsTakeTurns`, `Claim/LimitAndOrder`; `TestSelectClaims_*` (S-3a, the helper memory and SQLite share) |
| 19 | a liveness record swept during a long outage is recreated by the next heartbeat | `Liveness/HeartbeatRecreatesSwept` |
| 20 | a lost claim reply: the next `GiveBackIdle` returns the claimed tasks | `GiveBack/LostReply` |
| 21 | owner lost 3 times → FAILED `OWNER_LOST_REPEATEDLY` | `Claim/LostOwnersCounted`; `Claim/LostOwnerFlagged` (the `claims{reason=owner_lost}` input) |
| 22 | every fenced method refuses a stale token | `Fence/StaleTokensRefused`, `Fence/WaitingRefused`, `Fail/Fields` |
| 23 | after a re-arm, every fenced write of the old life is refused | `Fence/OldLifeRefused`, `Arm/SelfLoopRearmsRunning` |
| 24 | a reclaimed or re-armed task makes the old run's commit fail (C1) | `C1/ReclaimFailsOldCommit/*`, `C1/RearmFailsOldCommit/*` |
| 25 | a replaced owner's segment commit is refused by its stamp | `Fence/ReplacedOwnerStampRefused` |
| 26 | ABA | `Fence/ABA` |
| 27 | `MarkUnsafe` racing `ClaimDue` (C3) | `C3/MarkRacesClaim`, `Mark/MarkedByAnotherClaim` |
| 28 | a mark survives the rollback of the transaction on `ctx` | `Mark/SurvivesRollback` |
| 29 | a joining read sees its own staged writes (C2) | `C2/StagedWritesVisible`, `Fail/JoinsTransaction`, `RemoveLife/JoinsTransaction`, `DeleteForEntities/JoinsTransaction` |
| 30 | a row written by an open transaction is not claimable (C6) | `C6/OpenWriteNotClaimable/*` |
| 31 | a refusal from inside the run's own transaction goes through §5.6 | `C1/ReclaimFailsOldCommit/*` (non-joining re-read after the refusal), `C2/CallbackRearmThenStamp` |
| 32 | a re-arm resets `PartialCommit` | `Arm/RearmResetsLife` |
| 33 | a scheduler-pool statement blocked on a task-row lock gives up after `lock_timeout` | `C6/NeverJoiningWriteBounded` |
| 34 | SQLite: a run never conflicts with its own claim under a frozen clock | `C1/OwnClaimNoConflict` |
| 35 | a RUNNING task with no live run is given back; a live run never is | `GiveBack/KeepsLiveRuns` |
| 36 | dead owners and the marks of ended lives are swept | `Liveness/SweepRemovesUnreferenced`, `Liveness/SweepKeepsReferenced`, `SweepMarks/KeepsCurrentLife` |
| 37 | `GiveBackIdle` is not counted; `RetireOwner` removes liveness | `GiveBack/NotCountedKeepsMark`, `GiveBack/LostReply`, `Liveness/RetireOwner` |
| 38 | FAILED task re-armed by an update in the state (new life, no mark) | `Arm/RearmResetsLife` |
| 39 | a task whose transition is no longer scheduled is removed at the next write | `Arm/RemovesOtherTasks` |
| 40 | a workflow import that drops schedules removes the model's tasks | `DeleteForModel/Keep` |
| 41 | deleting one entity removes its tasks | `DeleteForEntities/RemovesListed` |
| 42 | conditional delete removes tasks: single-tx, batched, fast path | `DeleteForEntities/RemovesListed` (several entities, every status), `DeleteForModel/NilKeepRemovesAll` |
| 43 | delete-all removes the model's tasks | `DeleteForModel/NilKeepRemovesAll` |
| 44 | `DeleteForModel` in tenant A leaves tenant B's tasks | `DeleteForModel/TenantScoped` |
| 45 | an update racing a claim → 409; a delete or import racing one claim succeeds after the server retry; same on every backend | `C1/ClientWriteAfterClaim/{ReconcileForEntity,DeleteForEntities,DeleteForModel}` |
| 46 | `GET /scheduled-tasks` 200, no filter, several pages | `Query/PagesInOrder` |
| 47 | 200, each filter | `Query/Filters` |
| 48 | another tenant's tasks are never returned, under any filter | `Query/TenantIsolation`, `TenantIsolation/EveryMethod`, `Tenant/JoiningWriteOtherTenantRefused` |
| 49 | `SCHEDULED_TRANSITION_FAIL` and `Fail` in one transaction; a rolled-back run leaves no audit event | `Audit/RolledBackEventNotKept`, `Fail/JoinsTransaction` |

Method and refusal coverage, beyond the rows: `Arm/NewLife`,
`Arm/TenantAndEntityFromRequest`, `Arm/EveryArmIsNewLife`, `Arm/CancelNotReported`, `Arm/JoinsTransaction`,
`RemoveLife/*`, `Get/Missing`, `Claim/DueWaiting`, `Claim/NewTokenPerClaim`,
`Claim/FailedNeverClaimed`, `Claim/RunningNeedsAllowLostOwner`,
`Claim/FreshOwnerKept`, `Claim/StaleOwnerReclaimed`, `Claim/InvalidLimits`,
`Mark/AcceptedAndIdempotent`, `Record/Counted`, `Record/NotCounted`,
`Fail/Fields` (all five reasons; `LastAttemptTime` unchanged), `Fail/OverwritesLastError`,
`ErrorText/StoreRejected` (the refused write leaves the claim intact).

## Stream interface summary

**Other streams consume from S** (through the uncommitted `go.work` line of
S-9 Step 5, until the lead pins the merged SPI commit):

```go
// types.go — exactly interfaces.md § SPI
spi.ScheduledTaskStatus; spi.ScheduledTaskWaiting / Running / Failed
spi.ScheduledTaskFailureReason; spi.FailureUnsafeWorkNotCompleted / FailureOwnerLostRepeatedly /
    FailureExpiredAfterFailedAttempts / FailureRunPanicked / FailureStoppedAfterPartialCommit
spi.TaskClaim{Token, Owner uuid.UUID}
spi.ScheduledTask{…, Status, ArmToken, NextAttemptTime, Attempts, LostOwners, LastAttemptTime,
    LastError, FailureReason, FailedTime, PartialCommit, Claim, UnsafeMarked,
    ClaimedFromLostOwner /* json:"-"; ClaimDue results only */}   // RedispatchAfter, AttemptCount gone
spi.TaskRef, spi.ClaimRequest, spi.Attempt, spi.Failure,
spi.ScheduledTaskCursor, spi.ScheduledTaskQuery, spi.ScheduledTaskPage
spi.SMEventScheduledTransitionFailed = "SCHEDULED_TRANSITION_FAIL"

// persistence.go
type spi.ScheduledTaskStore interface { /* the 16 methods of interfaces.md */ }
spi.ReconcileRequest   // fields unchanged; ids in Cancel are removed and not reported

// errors.go
spi.ErrMarkedByAnotherClaim, spi.ErrTaskBusy, spi.ErrStoreRejected   // ErrStaleClaim unchanged

// scheduled_task_helpers.go (S-3a) — BM and BQ call these; BP may
const spi.MaxTaskErrorBytes = 1024
func spi.SelectClaims(cands []ScheduledTask, req ClaimRequest) []ScheduledTask
func spi.ValidateTaskErrorText(s string) error
func spi.ValidateFailureReason(r ScheduledTaskFailureReason) error
func spi.ValidateArm(req ReconcileRequest) error

// spitest
StoreFactoryConformance → t.Run("ScheduledTasks", …)   // runs for every backend that calls it
```

Contract points a backend stream must implement that the interface doc states
and the suite enforces (BM, BQ, BP; cyoda-go-cassandra#68):
- `StoreFactory.ScheduledTaskStore` returns `errors.ErrUnsupported`-wrapped
  errors when the backend has no store; any other error fails the group.
- A joining method without a transaction on ctx applies at once.
- `ClaimDue` returns an error, and claims nothing, when `Limit < 1` or
  `PerTenantLimit < 1` (`Claim/InvalidLimits`).
- `DeleteForModel` with `keep == nil` removes every task of that model version
  (`DeleteForModel/NilKeepRemovesAll`).
- `RecordAttempt` with `NotCounted` still records `Error` and
  `LastAttemptTime` (`Record/NotCounted`).
- `Fail` always replaces `LastError`, even with `""`
  (`Fail/OverwritesLastError`).
- `ReconcileForEntity` takes tenant and entity from the request, never from an
  `Arm` item (`Arm/TenantAndEntityFromRequest`).
- Stale-owner and sweep subtests run in real time on PostgreSQL through the
  harness's sleeping `AdvanceClock`; a store must compare liveness on its own
  clock, the same clock that stamped the heartbeat.
- A deterministic rejection from **any** method of any store — including
  `StateMachineAuditStore.Record` inside `Fail`'s transaction — and from a
  transaction's `Commit` satisfies `errors.Is(err, spi.ErrStoreRejected)`.
  The suite covers the scheduled-task writes (`ErrorText/StoreRejected`). The
  audit write and the commit have no input that every backend rejects, so each
  backend stream adds a plugin-level test for each deterministic rejection path
  it has (for PostgreSQL: SQLSTATE classes 22, 23 and 42 on the audit insert
  and at commit), and states in its section when a backend has none.
- `RemoveLife` counts as a write to its row for C1 even when it removes
  nothing (`C1/RearmFailsOldCommit/RemoveLife`).
- `Attempt.Error` and `Failure.Error` over 1024 bytes, not valid UTF-8, or
  holding a NUL are refused with `ErrStoreRejected` and change nothing.
  PostgreSQL gets NUL and invalid UTF-8 as SQLSTATE 22021; the length needs a
  `CHECK (octet_length(last_error) <= 1024)` (SQLSTATE 23514).
- `Query` orders IDs byte-wise; PostgreSQL uses `COLLATE "C"` on the `ORDER
  BY` and on the cursor comparison.
- `ClaimDue` "tenants take turns" is rank-major: the first task of each tenant
  before the second of any (`Claim/TenantsTakeTurns`). `spi.SelectClaims`
  implements it for a backend that chooses in Go.
- `ClaimDue` sets `ClaimedFromLostOwner` on each returned task it took from a
  stale or missing owner; no other method returns it set
  (`Claim/LostOwnerFlagged`).
- A joining method refuses a tenant that is not the tenant of the
  transaction on `ctx` with `spi.ErrTxTenantMismatch`
  (`Tenant/JoiningWriteOtherTenantRefused`).
- `RecordAttempt` may return `ErrTaskBusy` (C6); the caller retries.
- `Fail` leaves `LastAttemptTime`, `Attempts` and `LostOwners` unchanged
  (`Fail/Fields`).
- An audit event recorded in a transaction that rolls back is not kept
  (`Audit/RolledBackEventNotKept`).
- `GiveBackIdle` leaves the task claimable at the same `NowMs`.
- The suite's owners never heartbeat unless a test says so; a missing record
  must count as stale under any `StaleAfter`.
- The plugins' `scheduled_task_store_test.go` files call the removed
  `spi.RunScheduledTaskStoreConformance`
  (`plugins/memory/scheduled_task_store_test.go:12`,
  `plugins/sqlite/scheduled_task_store_test.go:15`,
  `plugins/postgres/scheduled_task_store_test.go:21`); BM, BQ and BP delete
  those calls, and their other tests that use `Upsert` / `Get(ctx, id)`.

**S consumes from other streams:** nothing. The lead, at wave 6: pushes
`feat/scheduled-run-ownership`, notifies the two `KNOWN_CONSUMERS.md` entries
(cyoda-go, cyoda-go-cassandra) as `MAINTAINING.md` "Deprecation policy"
requires before a breaking merge, merges the SPI PR into `main`, pins the
pseudo-version in all four `go.mod`s, runs `make repin-plugins`, drops the
`go.work` line, and updates `COMPATIBILITY.md`.

## Open points

1. **Closed (README C-S2).** The entry point is
   `runScheduledTasksSuite(t *testing.T, h Harness, tracker *skipTracker)`,
   as `interfaces.md` states.
2. **"Not implemented" has no SPI sentinel.** The spec says such a backend
   skips. This section uses the standard library's `errors.ErrUnsupported`
   rather than adding an SPI name. Cassandra's current
   `store.ErrScheduledTaskStoreNotImplemented` is a plain `errors.New`
   (`cyoda-go-cassandra internal/store/errors.go:40`), so until it wraps
   `errors.ErrUnsupported` its conformance run fails the group instead of
   skipping it. That belongs on cyoda-go-cassandra#68 (D stream's cross-repo
   notes).
3. **The `ErrStoreRejected` trigger.** (The lead asked for a subtest per
   backend that provokes a rejection where the harness allows it; this is that
   subtest for the scheduled-task writes. For the audit write and the commit
   the harness does not allow it — see the S-6 note.) Nothing else rejects deterministically
   on every backend, so the conformance case uses error text that breaks the
   documented precondition. That makes memory and SQLite validate the text
   (through `spi.ValidateTaskErrorText`, S-3a), and needs the PostgreSQL
   `CHECK` above (BP-4). The alternative, a harness hook
   that makes a store reject, would be a test seam in each backend.
4. **`RemoveLife` and C1.** PostgreSQL at REPEATABLE READ raises 40001 for a
   `DELETE … WHERE arm_token = old` when the row changed after the snapshot,
   even though the new version would not match. For memory and SQLite to
   agree, a no-op `RemoveLife` must still enter the write set. The interface
   doc says so; the spec does not.
5. **Choices the spec leaves open, stated on the interface:** a nil `keep`
   in `DeleteForModel` keeps none (spec §7 table "keep = none"); an id in
   `ReconcileRequest.Cancel` is removed and not reported even when it is also
   "another task of the entity"; `GiveBackIdle` does not raise
   `NextAttemptTime`; `ClaimRequest.Limit` and `PerTenantLimit` are ≥ 1 (not
   tested at 0; the loop never calls with 0, §6.1); `Fail` leaves `Attempts`
   and `LostOwners` as they are (not asserted).
6. **What the suite cannot observe.** Removal of an ended life's mark is
   invisible through the interface (`UnsafeMarked` reports the current life
   only); the suite pins that the current life's mark stays. Each backend's own
   store test checks the removal (BM, BQ, BP). C4 is likewise not portable
   (S-7 note).
7. **Cross-tenant claims share the database.** Each subtest removes its tasks
   at the end and reads only its own tenant's results. A plugin must not run
   the conformance suite in parallel with other tests that arm due tasks in
   the same database.
8. **`TransitionSchedule` doc rewrite goes beyond the one sentence.** The spec
   asks to remove the "silently skip" sentence. The "dropped" bullets and the
   `TimeoutMs` field comment contradict the retry ruling (§2 A3, §5.1), so S-2
   rewrites them too (Gate 6).
9. **`ErrStaleClaim` keeps its message** ("write fenced: stale claim epoch"),
   which says "epoch" although tasks fence on tokens. The spec asks for the
   comment only; the value is unchanged so nothing matching the text breaks.
10. **Dry run during planning.** S-4..S-8 were compiled against the S-1..S-3
    SPI and run against a throwaway in-memory fake of the contract: every
    subtest passed, and eight seeded defects (no-op `RemoveLife` outside the
    write set, claim of a busy row, mark of a busy row, two siblings per call,
    `ClearOwnMark` clearing another claim's mark, no commit-time C1 check,
    `Next` on an exact last page, sweeping a referenced owner) each failed at
    least one subtest. The fake is not part of this plan.
