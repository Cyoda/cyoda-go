# Stream R — the scheduler service, its settings, wiring and removals

Packages: `internal/scheduler` (rewritten), `app` (settings, wiring,
`DrainScheduler`), `cmd/cyoda` (`run.go`, `main.go`, help registry and
topics), `internal/cluster` (the scheduler RPC removed), `internal/cluster/dispatch`
(one export moved to a test file), `internal/domain/search` (one comment),
`internal/e2e` and `e2e/parity/multinode` (callers of the removed code),
`README.md`.

Spec sections: §5.6, §5.7, §5.8, §6 (all), §9, §11 (core settings; the
PostgreSQL pool setting is stream BP's), and the R rows of §13 listed at the
end of this file.

## Facts this stream relies on, read in the code

- The present scan loop calls `ScanDue` and `MarkRedispatch`
  (`internal/scheduler/service.go:168, 178`), and its tests call `Upsert`
  (`internal/scheduler/service_test.go:186`). Stream S removes those three
  methods from the SPI (`interfaces.md`, `ScheduledTaskStore`). So when this
  stream starts, `internal/scheduler`, `internal/cluster`, `app` and
  `internal/e2e` do not compile. R-1 to R-9 are verified with
  `go test ./internal/scheduler/...` only. `app`, `internal/cluster` and
  `cmd/cyoda` compile again after R-10, `internal/e2e` after R-11. `make test`
  runs after R-11.
- `internal/scheduler` does not import itself from `internal/domain/workflow`:
  `go list -deps ./internal/domain/workflow` lists no `internal/scheduler`. So
  the scheduler can import `workflow.RunReport` and `workflow.RunGuard` without
  a cycle. Today only `app/app.go:50`, `internal/cluster/scheduler_rpc.go:21`
  and two e2e files import `internal/scheduler`.
- `common.SystemUserContextValue` (`internal/common/tenant.go:69-76`) builds the
  system principal; `common.SystemUserContext` (`:58-60`) always derives from
  `context.Background()`. A run context must derive from the scheduler's own
  run context (§5.3), so the service attaches the value with
  `spi.WithUserContext(runCtx, common.SystemUserContextValue(tenant))`.
- `common.CommitBudget` is 30 s (`internal/common/reqtimeout.go:88`, alias of
  `rollback.go:36`). `common.ShieldedCommit` (`reqtimeout.go:97`) and
  `common.RollbackContext` (`rollback.go:26`) are the commit and rollback
  shapes the engine uses.
- `common.AppError.Level` is `LevelOperational` for every 4xx and for the
  retryable 503 of `common.StorageUnavailable` (`errors.go:134-174`); its
  `Message` is already `CODE: detail` (`:138`). `common.Internal` maps
  `spi.ErrConflict` to an Operational 409 (`:195-197`).
- Every `contract.CalloutFailure` that carries a code sets `Message` to the
  AppError's `Message`, which already starts with the code
  (`internal/grpc/dispatch.go:288, 303`; `internal/callout/failure.go:24-30`;
  `internal/cluster/dispatch/handover.go:456, 472, 492, 511`). A `MemberFailed`
  failure's `Message` is the compute node's own bounded text
  (`internal/grpc/dispatch.go:261-265`). `CalloutFailure.Unwrap` returns `Err`
  (`internal/contract/callout.go:103`), so `errors.Is(err, context.Canceled)`
  sees a cancellation inside one.
- The engine records audit events through `auditStore.Record(ctx, entityID,
  spi.StateMachineEvent{…})` on the transaction's context
  (`internal/domain/workflow/engine.go:1251-1282`), stamped with the engine
  clock (`:1270`). The scheduler writes `SCHEDULED_TRANSITION_FAIL` the same
  way, inside its own transaction (§5.7).
- The memory plugin's `ScheduledTaskStore(ctx)` ignores `ctx`
  (`plugins/memory/store_factory.go:246-248`), and so does PostgreSQL's
  (`plugins/postgres/store_factory.go:275-277`). The service resolves the store
  once, in `Start`. The audit and entity stores resolve the tenant from `ctx`
  (`plugins/memory/store_factory.go:190-196, 230-236`).
- The memory audit store needs an id generator, which
  `StoreFactory.NewTransactionManager` installs (`plugins/memory/txmanager.go:183-185`).
  Unit tests build the transaction manager that way, as
  `internal/scheduler/executor_test.go:21-26` does today.
- Metrics follow `internal/cluster/registry/metrics.go:21-43`: instruments from
  a `metric.Meter`, a no-op meter when none is given, errors wrapped per
  instrument. Tests read them with `sdkmetric.NewManualReader`
  (`internal/cluster/registry/metrics_internal_test.go:12-60`).
- `app.Config.Validate` (`app/config.go:780-803`) repeats the per-setting
  validators that `cmd/cyoda/main.go:86-121` calls one by one.
- `TestConfig_EnvVarCoverage` (`cmd/cyoda/help/help_test.go:489-509`) fails
  when a `CYODA_*` name in non-test Go source is not documented under
  `cmd/cyoda/help/content/config/`. `TestConfigAll_Complete`
  (`cmd/cyoda/help/config_registry_test.go:74`) fails when it is not in the
  registry. `TestRootConfigVars_MatchDefaults`
  (`app/config_registry_binding_test.go:185-205`) binds each registry default to
  `DefaultConfig()`. So a setting, its registry row, its help line and its
  default change in one commit.
- `runServers` stops every server on `ctx.Done()` of an errgroup built from
  `rootCtx` (`cmd/cyoda/run.go:89-195`), then calls `a.Shutdown()` and
  `a.Close()` (`:186-190`). `App.Shutdown` calls `a.scheduler.Stop()`
  (`app/app.go:1061-1063`).
- `dispatch.EncodePeerBody` is exported only because the scheduler RPC used it
  (`internal/cluster/dispatch/encode.go:25-29`); its one other caller is an
  external test (`internal/cluster/dispatch/entity_bytes_test.go:120`).
- `internal/domain/search/reaper.go:16-21` justifies `StaleClaimBatch` by
  `CYODA_SCHEDULER_BATCH_SIZE`, one of the removed names in the §15 exit check.

## Verification point settled here

**V4 — can the scheduler drain before the servers on the signal path?** Yes.
`runServers` builds its errgroup from `rootCtx` (`run.go:95`), so a signal
cancels every server at once. R-12 builds the errgroup from
`context.Background()` and gives the servers a `stopCtx` derived from the
group's context. A watcher goroutine waits for either `rootCtx` (a signal) or
the group's context (a server failed). On a signal it calls
`drainScheduler(ctx)` and then cancels `stopCtx`. On a server failure the
group's context is already cancelled, so the servers stop at once, and
`a.Shutdown()` drains the scheduler after them (§6.4, last paragraph).

## Order and dependencies

```
R-1 ── R-2 ── R-3 ── R-4 ── R-5 ── R-6 ── R-7 ── R-8 ── R-9 ── R-10 ── R-11 ── R-12
                     │             │                          │
                     E (RunReport) E (RunGuard, UnsafeFlight, S (all), E (engine is a Firer),
                                     RunGuardFrom), S          BP (nothing R calls directly)
```

R-2, R-3 and R-5 need nothing outside the package. R-4 needs `workflow.RunReport`.
R-6 needs the E additions named in Open point 2 and the S field named in Open
point 1.

---

### Task R-1: Remove the scan coordinator from `internal/scheduler`

**Spec:** §6.6 (first bullet), §15.

TDD waiver: this task only deletes code whose SPI no longer exists. The check is
that the package builds and the exit grep is empty. The one test that stays
(`TestSystemUserContext_HasTenant`) moves unchanged.

**Files:**
- Delete: `internal/scheduler/service.go`, `internal/scheduler/service_test.go`
- Delete: `internal/scheduler/executor.go`
- Delete: `internal/scheduler/coordinator.go`, `internal/scheduler/coordinator_test.go`
- Delete: `internal/scheduler/distribution.go`, `internal/scheduler/distribution_test.go`
- Create: `internal/scheduler/identity_test.go` (from `executor_test.go:1-54`)
- Delete: `internal/scheduler/executor_test.go`
- Keep: `internal/scheduler/clock.go`, `internal/scheduler/clock_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: an `internal/scheduler` package that holds only `Clock`,
  `NewRealClock` and the identity test.

- [ ] **Step 1: Move the identity test**

`internal/scheduler/identity_test.go` gets the package clause, the imports it
needs (`context`, `testing`, `spi`, `internal/common`, `plugins/memory`) and
`TestSystemUserContext_HasTenant` exactly as it is at
`internal/scheduler/executor_test.go:15-54`, doc comment included.

- [ ] **Step 2: Delete the old files**

```
git rm internal/scheduler/service.go internal/scheduler/service_test.go \
  internal/scheduler/executor.go internal/scheduler/executor_test.go \
  internal/scheduler/coordinator.go internal/scheduler/coordinator_test.go \
  internal/scheduler/distribution.go internal/scheduler/distribution_test.go
```

- [ ] **Step 3: Build and test the package**

Run: `go test ./internal/scheduler/...`
Expected: `ok` — `TestRealClock_*` and `TestSystemUserContext_HasTenant` pass.

- [ ] **Step 4: Exit check for this package**

Run:
```
git grep -n -e ScanDue -e MarkRedispatch -e RedispatchBackoff -e BatchSize -e LowestLiveNodeID \
  -e RoundRobin -e LocalExecutor -e CoordinatorStrategy -e DistributionStrategy -- internal/scheduler
```
Expected: no output.

- [ ] **Step 5: Commit**

```
git add internal/scheduler/identity_test.go
git commit -m "refactor(scheduler): remove the scan coordinator and round-robin distribution

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task R-2: Liveness limits — `MinStaleAfter`, the watchdog window, the heartbeat budget

**Spec:** §6.2 (budget `min(10 s, HEARTBEAT_INTERVAL)`), §6.3 (`W`, the
validation rule and its argument). §13 U rows: "`STALE_AFTER` validation: a
single slow or failed heartbeat never self-cancels" (the arithmetic half; R-6
has the service half, R-10 the setting).

**Files:**
- Create: `internal/scheduler/limits.go`
- Test: `internal/scheduler/limits_test.go`

**Interfaces:**
- Consumes: `common.CommitBudget`.
- Produces:
  - `func MinStaleAfter(heartbeat time.Duration) time.Duration` (exported; `app.ValidateScheduler` uses it)
  - `func watchdogWindow(staleAfter time.Duration) time.Duration`
  - `func heartbeatBudget(interval time.Duration) time.Duration`

- [ ] **Step 1: Write the failing tests**

`internal/scheduler/limits_test.go`:

```go
package scheduler

import (
	"testing"
	"time"
)

func TestMinStaleAfter_IsFiftySecondsPlusThreeHeartbeats(t *testing.T) {
	for _, tt := range []struct{ heartbeat, want time.Duration }{
		{15 * time.Second, 95 * time.Second},
		{time.Second, 53 * time.Second},
		{time.Minute, 230 * time.Second},
	} {
		if got := MinStaleAfter(tt.heartbeat); got != tt.want {
			t.Errorf("MinStaleAfter(%s) = %s, want %s", tt.heartbeat, got, tt.want)
		}
	}
}

func TestHeartbeatBudget_IsTheSmallerOfTenSecondsAndTheInterval(t *testing.T) {
	for _, tt := range []struct{ interval, want time.Duration }{
		{15 * time.Second, 10 * time.Second},
		{10 * time.Second, 10 * time.Second},
		{5 * time.Second, 5 * time.Second},
	} {
		if got := heartbeatBudget(tt.interval); got != tt.want {
			t.Errorf("heartbeatBudget(%s) = %s, want %s", tt.interval, got, tt.want)
		}
	}
}

// At the smallest valid STALE_AFTER, W must cover two heartbeat intervals and
// one heartbeat's budget with a full interval to spare: then one slow or failed
// heartbeat is always followed by another that lands inside the window.
func TestWatchdogWindow_OneSlowOrFailedHeartbeatNeverSelfCancels(t *testing.T) {
	for _, h := range []time.Duration{time.Second, 10 * time.Second, 15 * time.Second, time.Minute} {
		w := watchdogWindow(MinStaleAfter(h))
		if need := 3*h + heartbeatBudget(h); w < need {
			t.Errorf("heartbeat %s: W = %s, want >= %s", h, w, need)
		}
	}
	if got := watchdogWindow(2 * time.Minute); got != 80*time.Second {
		t.Errorf("W at the 2m default = %s, want 80s (120s - 30s commit budget - 10s slack)", got)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/scheduler/... -run 'TestMinStaleAfter|TestHeartbeatBudget|TestWatchdogWindow'`
Expected: build failure — `undefined: MinStaleAfter`, `heartbeatBudget`, `watchdogWindow`.

- [ ] **Step 3: Implement**

`internal/scheduler/limits.go`:

```go
package scheduler

import (
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const (
	// watchdogSlack is what W leaves for clock rate and scheduling.
	watchdogSlack = 10 * time.Second
	// heartbeatBudgetMax caps one heartbeat call, connection acquire included.
	heartbeatBudgetMax = 10 * time.Second
)

// MinStaleAfter is the smallest CYODA_SCHEDULER_STALE_AFTER a heartbeat
// interval allows: the commit budget, the watchdog slack, one heartbeat's
// budget and three intervals. With it, one slow or failed heartbeat never
// makes a pnode cancel its own runs.
func MinStaleAfter(heartbeat time.Duration) time.Duration {
	return common.CommitBudget + watchdogSlack + heartbeatBudgetMax + 3*heartbeat
}

// watchdogWindow is W: how long after a successful heartbeat's recorded start
// the pnode keeps its runs. A commit already under way when W passes still has
// the commit budget to land before another pnode can consider this one stale.
func watchdogWindow(staleAfter time.Duration) time.Duration {
	return staleAfter - common.CommitBudget - watchdogSlack
}

// heartbeatBudget bounds one heartbeat call: the connection acquire and the
// statement together.
func heartbeatBudget(interval time.Duration) time.Duration {
	return min(heartbeatBudgetMax, interval)
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/scheduler/... -run 'TestMinStaleAfter|TestHeartbeatBudget|TestWatchdogWindow'`
Expected: `ok`.

- [ ] **Step 5: Commit**

```
git add internal/scheduler/limits.go internal/scheduler/limits_test.go
git commit -m "feat(scheduler): liveness limits — minimum stale-after, watchdog window, heartbeat budget

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task R-3: The recorded error — `sanitiseErrorText` and `recordedError`

**Spec:** §5.8 (the allow-list in its order; the sanitising rule). §13 U rows:
"`lastError` of a non-sentinel store error is 'internal error [ticket]' only";
"`lastError` of a `MemberFailed` message, a callout timeout (`Code: Message`),
an Operational `AppError`, `NO_COMPUTE_MEMBER_FOR_TAG`"; "`lastError` over
1 024 bytes with multi-byte characters and a NUL → cut at a character
boundary" (the U half; the S half is stream S's); "`lastError` for a cancelled
run and for a conflict: fixed texts, WARN, no ticket".

**Files:**
- Create: `internal/scheduler/errortext.go`
- Test: `internal/scheduler/errortext_test.go`

**Interfaces:**
- Consumes: `spi.ErrConflict`, `contract.CalloutFailure`, `common.AppError`, `common.LevelOperational`.
- Produces:
  - `func recordedError(err error) (text string, ticket uuid.UUID, warnOnly bool)` — `warnOnly` is true for the first four rows of §5.8 (the caller logs a safe failure at WARN), false for the last row (the caller logs the full error at ERROR with `ticket`); `ticket` is `uuid.Nil` unless `warnOnly` is false.
  - `func sanitiseErrorText(s string) string`
  - `func internalErrorText(ticket uuid.UUID) string` — `internal error [ticket: <uuid>]`
  - `const cancelledText`, `const conflictText`, `const maxErrorTextBytes = 1024`

- [ ] **Step 1: Write the failing tests**

`internal/scheduler/errortext_test.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func calloutFailure(kind contract.CalloutFailureKind, appErr *common.AppError) *contract.CalloutFailure {
	return &contract.CalloutFailure{Kind: kind, Code: appErr.Code, Message: appErr.Message, Err: appErr}
}

func TestRecordedError_AllowList(t *testing.T) {
	timeout := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
		"processor dispatch timed out after 100ms: no response").AsRetryable()
	noMember := common.Operational(http.StatusServiceUnavailable, common.ErrCodeNoComputeMemberForTag,
		"no compute member for tags [billing]").AsRetryable()
	badRequest := common.Operational(http.StatusBadRequest, common.ErrCodeWorkflowFailed, "the processor output is not an object")

	tests := []struct {
		name     string
		err      error
		want     string
		warnOnly bool
	}{
		{"a cancellation of the run", fmt.Errorf("run stopped: %w", context.Canceled), cancelledText, true},
		{"a cancellation inside a callout failure",
			&contract.CalloutFailure{Kind: contract.NoAnswer, Message: "cancelled", Err: context.Canceled}, cancelledText, true},
		{"a conflict", fmt.Errorf("failed to commit: %w", spi.ErrConflict), conflictText, true},
		{"a compute node's own message", fmt.Errorf("processor charge failed: %w",
			&contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined"}), "card declined", true},
		{"a callout timeout", calloutFailure(contract.NoAnswer, timeout),
			"DISPATCH_TIMEOUT: processor dispatch timed out after 100ms: no response", true},
		{"no compute member for the tag", calloutFailure(contract.NoHandOff, noMember),
			"NO_COMPUTE_MEMBER_FOR_TAG: no compute member for tags [billing]", true},
		{"an Operational AppError, wrapped", fmt.Errorf("fire: %w", badRequest),
			"WORKFLOW_FAILED: the processor output is not an object", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, ticket, warnOnly := recordedError(tt.err)
			if text != tt.want || warnOnly != tt.warnOnly || ticket != uuid.Nil {
				t.Errorf("recordedError = (%q, %s, %v), want (%q, nil ticket, %v)", text, ticket, warnOnly, tt.want, tt.warnOnly)
			}
		})
	}
}

func TestRecordedError_AnythingElseIsATicketOnly(t *testing.T) {
	for name, err := range map[string]error{
		"a store error": fmt.Errorf("failed to read entity: %w",
			errors.New("pq: connection refused host=db.internal user=cyoda")),
		"an Internal AppError": common.Internal("failed to save", errors.New("disk full at /var/lib/pg")),
	} {
		t.Run(name, func(t *testing.T) {
			text, ticket, warnOnly := recordedError(err)
			if warnOnly || ticket == uuid.Nil {
				t.Fatalf("recordedError = (%q, %s, %v), want a ticket and ERROR", text, ticket, warnOnly)
			}
			if want := "internal error [ticket: " + ticket.String() + "]"; text != want {
				t.Errorf("text = %q, want %q", text, want)
			}
		})
	}
}

func TestSanitiseErrorText(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"short text is unchanged", "CODE: detail", "CODE: detail"},
		{"NUL is replaced", "a\x00b", "a�b"},
		{"invalid UTF-8 is replaced", "a\xffb", "a�b"},
		{"exactly the limit is kept", strings.Repeat("a", 1024), strings.Repeat("a", 1024)},
		{"a character across the limit is dropped whole", strings.Repeat("a", 1023) + "é", strings.Repeat("a", 1023)},
		{"three-byte characters are cut at a boundary", strings.Repeat("€", 400), strings.Repeat("€", 341)},
		{"NUL and multi-byte text over the limit", strings.Repeat("\x00€", 300),
			strings.Repeat("�€", 170) + "�"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitiseErrorText(tt.in)
			if got != tt.want {
				t.Errorf("sanitiseErrorText: got %d bytes %q, want %d bytes", len(got), got, len(tt.want))
			}
			if len(got) > maxErrorTextBytes || !utf8.ValidString(got) || strings.ContainsRune(got, 0) {
				t.Errorf("result is not storable: %d bytes, valid=%v", len(got), utf8.ValidString(got))
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/scheduler/... -run 'TestRecordedError|TestSanitiseErrorText'`
Expected: build failure — `undefined: recordedError`, `cancelledText`, `conflictText`, `sanitiseErrorText`, `maxErrorTextBytes`.

- [ ] **Step 3: Implement**

`internal/scheduler/errortext.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// maxErrorTextBytes bounds lastError. Every backend stores the same text.
const maxErrorTextBytes = 1024

const (
	cancelledText = "CANCELLED: the run was stopped by the scheduler"
	conflictText  = "CONFLICT: a concurrent write changed the entity or its task"
)

// internalErrorText is what a tenant user sees for a failure whose detail is
// internal. The full error is logged at ERROR under the same ticket.
func internalErrorText(ticket uuid.UUID) string {
	return "internal error [ticket: " + ticket.String() + "]"
}

// recordedError maps a run's failure to lastError, which tenant users can read.
// The first matching row applies. Only the last row mints a ticket; the caller
// logs the full error at ERROR under it. The other rows are client-safe text
// and are logged at WARN.
func recordedError(err error) (text string, ticket uuid.UUID, warnOnly bool) {
	if errors.Is(err, context.Canceled) {
		return cancelledText, uuid.Nil, true
	}
	if errors.Is(err, spi.ErrConflict) {
		return conflictText, uuid.Nil, true
	}
	var failure *contract.CalloutFailure
	if errors.As(err, &failure) {
		return failure.Message, uuid.Nil, true
	}
	var appErr *common.AppError
	if errors.As(err, &appErr) && appErr.Level == common.LevelOperational {
		return appErr.Message, uuid.Nil, true
	}
	ticket = uuid.New()
	return internalErrorText(ticket), ticket, false
}

// sanitiseErrorText makes text storable on every backend: NUL and invalid
// UTF-8 become U+FFFD, and the text is cut at a character boundary to at most
// maxErrorTextBytes.
func sanitiseErrorText(s string) string {
	s = strings.ReplaceAll(s, "\x00", "�")
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= maxErrorTextBytes {
		return s
	}
	cut := maxErrorTextBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/scheduler/... -run 'TestRecordedError|TestSanitiseErrorText'`
Expected: `ok`.

- [ ] **Step 5: Commit**

```
git add internal/scheduler/errortext.go internal/scheduler/errortext_test.go
git commit -m "feat(scheduler): lastError allow-list and storable-text sanitising

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task R-4: `Config` and the bookkeeping decision

**Spec:** §5.6 (the table, first matching row; the delay formula; the deadline
rule for counted attempts; `NotCounted` never checks the deadline), §5.1 (the
engine's own FAILED decisions reach the scheduler as `RunReport.FailReason`).
§13 U rows, the scheduler's half of each: "fired on time", "declined",
"expired, late on the first attempt" (no bookkeeping); "criterion error →
WAITING, attempts 1"; "no compute node → `NotHandedOff`, mark cleared,
WAITING"; "idempotent processor fails → WAITING"; "retry delay doubles,
saturates, clamps to the deadline; runs up to `RETRY_DELAY` past it; later →
FAILED" (the record-time half; the claim-time half is E's §5.1); "late after
failed attempts → FAILED"; "unsafe processor fails → FAILED"; "a later step
fails after an unsafe hand-off → FAILED"; "failure after a successful unsafe
dispatch in the same step → FAILED"; "savepoint error replaces the dispatch
error → FAILED"; "cancelled before `Send` returned nil → WAITING"; "cancelled
after `Send` returned nil → FAILED"; "hand-over past `StageNotConnected`
without `no_handoff` → FAILED"; "database outage during `MarkUnsafe` →
`RecordAttempt{ClearOwnMark}`"; "`ErrMarkedByAnotherClaim` → FAILED";
"`ErrTaskBusy` → safe failure"; "CBD in a cascade step, later failure → FAILED
`STOPPED_AFTER_PARTIAL_COMMIT`"; "owner lost 3 times → FAILED"; "run cut after
the drain, nothing handed off → WAITING, uncounted"; "a run … cut after the
drain, whose unsafe work was handed off earlier → FAILED".

**Files:**
- Create: `internal/scheduler/config.go`
- Create: `internal/scheduler/bookkeeping.go`
- Test: `internal/scheduler/bookkeeping_test.go`

**Interfaces:**
- Consumes: E `workflow.RunReport`, `workflow.OutcomeFailed` and the other outcomes; S `spi.Attempt`, `spi.Failure`, the `spi.Failure*` reasons.
- Produces:
  - `type Config struct{ Enabled bool; ScanInterval time.Duration; MaxRuns int; MaxRunsPerTenant int; HeartbeatInterval time.Duration; StaleAfter time.Duration; MaxLostOwners int; RetryDelay time.Duration; RetryDelayMax time.Duration; ShutdownDrain time.Duration }` (binding; field order matters, see R-10)
  - `type BookkeepingKind int` with `NoneKind`, `RecordAttemptKind`, `FailKind`
  - `type Bookkeeping struct{ Kind BookkeepingKind; Attempt spi.Attempt; Failure spi.Failure }`
  - `func decideBookkeeping(r workflow.RunReport, task spi.ScheduledTask, cutByShutdown, panicked bool, nowMs int64, cfg Config, errText string) Bookkeeping`
  - `func retryDelay(attempts int, base, max time.Duration) time.Duration`

The rows, in order (the §5.6 table, with the engine's pre-run decision placed
right after a panic, because it is FAILED before anything ran):

| # | Condition | Result |
|---|---|---|
| 1 | `panicked` | `Fail(RUN_PANICKED)` |
| 2 | `r.Outcome != OutcomeFailed` | none (committed, or superseded) |
| 3 | `r.FailReason != ""` | `Fail(r.FailReason)` |
| 4 | `r.PartialCommit` | `Fail(STOPPED_AFTER_PARTIAL_COMMIT)` |
| 5 | `r.MarkHeld && r.UnsafeReached` | `Fail(UNSAFE_WORK_NOT_COMPLETED)` |
| 6 | `cutByShutdown` | `RecordAttempt{NotCounted, NextAttemptTime: now, ClearOwnMark}` |
| 7 | counted, `timeoutMs` set and `now > deadline` | `Fail(EXPIRED_AFTER_FAILED_ATTEMPTS)` |
| 8 | `r.MarkHeld \|\| r.MarkErrored` | `RecordAttempt{Error, NextAttemptTime, ClearOwnMark}` |
| 9 | any other failure | `RecordAttempt{Error, NextAttemptTime}` |

- [ ] **Step 1: Write the failing test**

`internal/scheduler/bookkeeping_test.go`:

```go
package scheduler

import (
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

const testNowMs = int64(10_000_000)

func bookkeepingTask(attempts int, timeoutMs *int64) spi.ScheduledTask {
	return spi.ScheduledTask{ID: "task-1", TenantID: "t1", ScheduledTime: testNowMs - 60_000, TimeoutMs: timeoutMs, Attempts: attempts}
}

func millis(v int64) *int64 { return &v }

func failedReport(mod func(*workflow.RunReport)) workflow.RunReport {
	r := workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: errors.New("boom")}
	if mod != nil {
		mod(&r)
	}
	return r
}

func TestDecideBookkeeping(t *testing.T) {
	cfg := Config{RetryDelay: 30 * time.Second, RetryDelayMax: 15 * time.Minute}
	const text = "CODE: detail"
	fail := func(reason spi.ScheduledTaskFailureReason) Bookkeeping {
		return Bookkeeping{Kind: FailKind, Failure: spi.Failure{Reason: reason, Error: text, AtMs: testNowMs}}
	}
	attempt := func(next int64, clearMark bool) Bookkeeping {
		return Bookkeeping{Kind: RecordAttemptKind, Attempt: spi.Attempt{Error: text, AtMs: testNowMs, NextAttemptTime: next, ClearOwnMark: clearMark}}
	}
	uncounted := Bookkeeping{Kind: RecordAttemptKind, Attempt: spi.Attempt{Error: text, AtMs: testNowMs, NextAttemptTime: testNowMs, NotCounted: true, ClearOwnMark: true}}
	markHeld := func(r *workflow.RunReport) { r.MarkHeld = true }
	handedOff := func(r *workflow.RunReport) { r.MarkHeld, r.UnsafeReached = true, true }

	tests := []struct {
		name     string
		report   workflow.RunReport
		task     spi.ScheduledTask
		cut      bool
		panicked bool
		want     Bookkeeping
	}{
		{name: "a panicked run", task: bookkeepingTask(0, nil), panicked: true, want: fail(spi.FailureRunPanicked)},
		{name: "fired", report: workflow.RunReport{Outcome: workflow.OutcomeFired}, task: bookkeepingTask(0, nil)},
		{name: "declined", report: workflow.RunReport{Outcome: workflow.OutcomeDeclined}, task: bookkeepingTask(0, nil)},
		{name: "expired", report: workflow.RunReport{Outcome: workflow.OutcomeExpired}, task: bookkeepingTask(0, nil)},
		{name: "cancelled", report: workflow.RunReport{Outcome: workflow.OutcomeCancelled}, task: bookkeepingTask(0, nil)},
		{name: "superseded", report: workflow.RunReport{Outcome: workflow.OutcomeSuperseded}, task: bookkeepingTask(0, nil)},
		{name: "owner lost too often, decided by the engine",
			report: failedReport(func(r *workflow.RunReport) { r.FailReason = spi.FailureOwnerLostRepeatedly }),
			task:   bookkeepingTask(0, nil), want: fail(spi.FailureOwnerLostRepeatedly)},
		{name: "marked by another claim, decided by the engine",
			report: failedReport(func(r *workflow.RunReport) { r.FailReason = spi.FailureUnsafeWorkNotCompleted }),
			task:   bookkeepingTask(0, nil), want: fail(spi.FailureUnsafeWorkNotCompleted)},
		{name: "a partial commit comes before the mark",
			report: failedReport(func(r *workflow.RunReport) { handedOff(r); r.PartialCommit = true }),
			task:   bookkeepingTask(0, nil), want: fail(spi.FailureStoppedAfterPartialCommit)},
		{name: "unsafe work reached a compute node", report: failedReport(handedOff),
			task: bookkeepingTask(0, nil), want: fail(spi.FailureUnsafeWorkNotCompleted)},
		{name: "cut by shutdown after unsafe work was handed off", report: failedReport(handedOff),
			task: bookkeepingTask(0, nil), cut: true, want: fail(spi.FailureUnsafeWorkNotCompleted)},
		{name: "cut by shutdown, nothing handed off", report: failedReport(markHeld),
			task: bookkeepingTask(0, nil), cut: true, want: uncounted},
		{name: "cut by shutdown after the deadline is uncounted, not FAILED", report: failedReport(nil),
			task: bookkeepingTask(0, millis(1_000)), cut: true, want: uncounted},
		{name: "mark held, nothing handed off", report: failedReport(markHeld),
			task: bookkeepingTask(0, nil), want: attempt(testNowMs+30_000, true)},
		{name: "MarkUnsafe failed with a non-refusal error",
			report: failedReport(func(r *workflow.RunReport) { r.MarkErrored = true }),
			task:   bookkeepingTask(0, nil), want: attempt(testNowMs+30_000, true)},
		{name: "a plain safe failure", report: failedReport(nil),
			task: bookkeepingTask(0, nil), want: attempt(testNowMs+30_000, false)},
		{name: "the second failure doubles the delay", report: failedReport(nil),
			task: bookkeepingTask(1, nil), want: attempt(testNowMs+60_000, false)},
		{name: "the third failure doubles it again", report: failedReport(nil),
			task: bookkeepingTask(2, nil), want: attempt(testNowMs+120_000, false)},
		{name: "the delay saturates at the maximum", report: failedReport(nil),
			task: bookkeepingTask(9, nil), want: attempt(testNowMs+900_000, false)},
		{name: "the delay never overflows", report: failedReport(nil),
			task: bookkeepingTask(10_000, nil), want: attempt(testNowMs+900_000, false)},
		{name: "the next attempt is clamped to the deadline", report: failedReport(nil),
			task: bookkeepingTask(0, millis(70_000)), want: attempt(testNowMs+10_000, false)},
		{name: "at the deadline the attempt is still recorded", report: failedReport(nil),
			task: bookkeepingTask(0, millis(60_000)), want: attempt(testNowMs, false)},
		{name: "a counted attempt past the deadline fails the task", report: failedReport(nil),
			task: bookkeepingTask(0, millis(59_999)), want: fail(spi.FailureExpiredAfterFailedAttempts)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideBookkeeping(tt.report, tt.task, tt.cut, tt.panicked, testNowMs, cfg, text)
			if got != tt.want {
				t.Errorf("decideBookkeeping\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to see it fail**

Run: `go test ./internal/scheduler/... -run TestDecideBookkeeping`
Expected: build failure — `undefined: Config`, `Bookkeeping`, `FailKind`, `RecordAttemptKind`, `decideBookkeeping`.

- [ ] **Step 3: Implement**

`internal/scheduler/config.go`:

```go
package scheduler

import "time"

// Config is the scheduler's settings. app.SchedulerConfig has the same fields,
// with the same names, types and order, and converts to it with a plain
// conversion; keep the two in step.
type Config struct {
	Enabled           bool
	ScanInterval      time.Duration
	MaxRuns           int
	MaxRunsPerTenant  int
	HeartbeatInterval time.Duration
	StaleAfter        time.Duration
	MaxLostOwners     int
	RetryDelay        time.Duration
	RetryDelayMax     time.Duration
	ShutdownDrain     time.Duration
}
```

`internal/scheduler/bookkeeping.go`:

```go
package scheduler

import (
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

// BookkeepingKind is the fenced write that records a run's outcome.
type BookkeepingKind int

const (
	// NoneKind: the run committed its own outcome, or was superseded.
	NoneKind BookkeepingKind = iota
	// RecordAttemptKind: the task goes back to WAITING.
	RecordAttemptKind
	// FailKind: the task becomes FAILED, with the audit event (§5.7).
	FailKind
)

// Bookkeeping is the write decideBookkeeping chose.
type Bookkeeping struct {
	Kind    BookkeepingKind
	Attempt spi.Attempt
	Failure spi.Failure
}

// decideBookkeeping chooses how a run's outcome is recorded. The first
// matching row applies: a panic; a committed or superseded run; the engine's
// own FAILED decision; a partial commit; unsafe work that reached a compute
// node; a cut by the shutdown drain; then a counted attempt, which fails the
// task instead when the deadline has passed. errText is already sanitised.
func decideBookkeeping(r workflow.RunReport, task spi.ScheduledTask, cutByShutdown, panicked bool,
	nowMs int64, cfg Config, errText string) Bookkeeping {
	fail := func(reason spi.ScheduledTaskFailureReason) Bookkeeping {
		return Bookkeeping{Kind: FailKind, Failure: spi.Failure{Reason: reason, Error: errText, AtMs: nowMs}}
	}
	switch {
	case panicked:
		return fail(spi.FailureRunPanicked)
	case r.Outcome != workflow.OutcomeFailed:
		return Bookkeeping{Kind: NoneKind}
	case r.FailReason != "":
		return fail(r.FailReason)
	case r.PartialCommit:
		return fail(spi.FailureStoppedAfterPartialCommit)
	case r.MarkHeld && r.UnsafeReached:
		return fail(spi.FailureUnsafeWorkNotCompleted)
	case cutByShutdown:
		return Bookkeeping{Kind: RecordAttemptKind, Attempt: spi.Attempt{
			Error: errText, AtMs: nowMs, NextAttemptTime: nowMs, NotCounted: true, ClearOwnMark: true}}
	}
	next := nowMs + retryDelay(task.Attempts+1, cfg.RetryDelay, cfg.RetryDelayMax).Milliseconds()
	if task.TimeoutMs != nil {
		deadline := task.ScheduledTime + *task.TimeoutMs
		if nowMs > deadline {
			return fail(spi.FailureExpiredAfterFailedAttempts)
		}
		next = min(next, deadline)
	}
	return Bookkeeping{Kind: RecordAttemptKind, Attempt: spi.Attempt{
		Error: errText, AtMs: nowMs, NextAttemptTime: next, ClearOwnMark: r.MarkHeld || r.MarkErrored}}
}

// retryDelay is base × 2^(attempts−1), saturating at max without overflow.
func retryDelay(attempts int, base, max time.Duration) time.Duration {
	d := base
	for i := 1; i < attempts; i++ {
		if d >= max/2 {
			return max
		}
		d *= 2
	}
	return min(d, max)
}
```

- [ ] **Step 4: Run the test to see it pass**

Run: `go test ./internal/scheduler/... -run TestDecideBookkeeping`
Expected: `ok`.

- [ ] **Step 5: Commit**

```
git add internal/scheduler/config.go internal/scheduler/bookkeeping.go internal/scheduler/bookkeeping_test.go
git commit -m "feat(scheduler): decide how a run's outcome is recorded

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task R-5: The §9 instruments

**Spec:** §9 (the table; no tenant attribute). §13 U row: "§9 instruments are
emitted with their attributes".

**Files:**
- Create: `internal/scheduler/metrics.go`
- Test: `internal/scheduler/metrics_test.go`

**Interfaces:**
- Consumes: `go.opentelemetry.io/otel/metric`.
- Produces:
  - `func newMetrics(meter metric.Meter) (*metrics, error)` — a nil meter gives no-op instruments
  - `(*metrics).runStarted()`, `runEnded(outcome string, d time.Duration)`, `claimed(reason string)`, `heartbeatFailed()`, `bookkeepingRetried()`
  - outcome words `outcomeAttemptFailed`, `outcomeFailed`, `outcomeSelfCancelled`, `outcomeShutdownCancelled`, `outcomePanicked`, `outcomeSuperseded`; claim reasons `claimDue`, `claimOwnerLost`. The committed outcomes use `string(workflow.Outcome…)`.

- [ ] **Step 1: Write the failing test**

`internal/scheduler/metrics_test.go`:

```go
package scheduler

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestMetrics_EveryInstrumentWithItsAttributes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	m, err := newMetrics(mp.Meter("test"))
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}

	m.runStarted()
	m.runEnded(outcomeSelfCancelled, 2*time.Second)
	m.claimed(claimOwnerLost)
	m.heartbeatFailed()
	m.bookkeepingRetried()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	found := map[string]metricdata.Aggregation{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			found[md.Name] = md.Data
		}
	}

	sumOf := func(name string, want int64, key, value string) {
		t.Helper()
		sum, ok := found[name].(metricdata.Sum[int64])
		if !ok || len(sum.DataPoints) != 1 {
			t.Fatalf("%s: got %T with %v", name, found[name], found[name])
		}
		dp := sum.DataPoints[0]
		if dp.Value != want {
			t.Errorf("%s = %d, want %d", name, dp.Value, want)
		}
		wantAttrs := attribute.NewSet()
		if key != "" {
			wantAttrs = attribute.NewSet(attribute.String(key, value))
		}
		if !dp.Attributes.Equals(&wantAttrs) {
			t.Errorf("%s attributes = %v, want %v", name, dp.Attributes.ToSlice(), wantAttrs.ToSlice())
		}
	}
	sumOf("cyoda.scheduler.runs", 1, "outcome", "self_cancelled")
	sumOf("cyoda.scheduler.runs.in_progress", 0, "", "")
	sumOf("cyoda.scheduler.claims", 1, "reason", "owner_lost")
	sumOf("cyoda.scheduler.heartbeat.failures", 1, "", "")
	sumOf("cyoda.scheduler.bookkeeping.retries", 1, "", "")

	hist, ok := found["cyoda.scheduler.run.duration"].(metricdata.Histogram[float64])
	if !ok || len(hist.DataPoints) != 1 {
		t.Fatalf("cyoda.scheduler.run.duration: got %T", found["cyoda.scheduler.run.duration"])
	}
	if hp := hist.DataPoints[0]; hp.Count != 1 || hp.Sum != 2 {
		t.Errorf("run.duration count=%d sum=%v, want 1 and 2 seconds", hp.Count, hp.Sum)
	}
}

func TestMetrics_NilMeterIsNoop(t *testing.T) {
	m, err := newMetrics(nil)
	if err != nil {
		t.Fatalf("newMetrics(nil): %v", err)
	}
	m.runStarted()
	m.runEnded(outcomeFailed, time.Millisecond)
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/scheduler/... -run TestMetrics`
Expected: build failure — `undefined: newMetrics`, `outcomeSelfCancelled`, `claimOwnerLost`, `outcomeFailed`.

- [ ] **Step 3: Implement**

`internal/scheduler/metrics.go`:

```go
package scheduler

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// The outcome attribute of cyoda.scheduler.runs for a run that did not commit.
// A committed run uses its workflow.ScheduledOutcome word.
const (
	outcomeAttemptFailed     = "attempt_failed"
	outcomeFailed            = "failed"
	outcomeSelfCancelled     = "self_cancelled"
	outcomeShutdownCancelled = "shutdown_cancelled"
	outcomePanicked          = "panicked"
	outcomeSuperseded        = "superseded"
)

// The reason attribute of cyoda.scheduler.claims.
const (
	claimDue       = "due"
	claimOwnerLost = "owner_lost"
)

// metrics are the scheduler's instruments. None carries a tenant.
type metrics struct {
	runs               metric.Int64Counter
	runDuration        metric.Float64Histogram
	inProgress         metric.Int64UpDownCounter
	claims             metric.Int64Counter
	heartbeatFailures  metric.Int64Counter
	bookkeepingRetries metric.Int64Counter
}

func newMetrics(meter metric.Meter) (*metrics, error) {
	if meter == nil {
		meter = noop.NewMeterProvider().Meter("")
	}
	m := &metrics{}
	var err error
	if m.runs, err = meter.Int64Counter("cyoda.scheduler.runs",
		metric.WithDescription("Scheduled runs that ended, by outcome")); err != nil {
		return nil, fmt.Errorf("instrument cyoda.scheduler.runs: %w", err)
	}
	if m.runDuration, err = meter.Float64Histogram("cyoda.scheduler.run.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of a scheduled run, from its claim to its recorded outcome")); err != nil {
		return nil, fmt.Errorf("instrument cyoda.scheduler.run.duration: %w", err)
	}
	if m.inProgress, err = meter.Int64UpDownCounter("cyoda.scheduler.runs.in_progress",
		metric.WithDescription("Scheduled runs this node holds now")); err != nil {
		return nil, fmt.Errorf("instrument cyoda.scheduler.runs.in_progress: %w", err)
	}
	if m.claims, err = meter.Int64Counter("cyoda.scheduler.claims",
		metric.WithDescription("Scheduled tasks this node claimed, by reason")); err != nil {
		return nil, fmt.Errorf("instrument cyoda.scheduler.claims: %w", err)
	}
	if m.heartbeatFailures, err = meter.Int64Counter("cyoda.scheduler.heartbeat.failures",
		metric.WithDescription("Scheduler heartbeats that failed or came too late")); err != nil {
		return nil, fmt.Errorf("instrument cyoda.scheduler.heartbeat.failures: %w", err)
	}
	if m.bookkeepingRetries, err = meter.Int64Counter("cyoda.scheduler.bookkeeping.retries",
		metric.WithDescription("Writes of a run's outcome that failed and were retried")); err != nil {
		return nil, fmt.Errorf("instrument cyoda.scheduler.bookkeeping.retries: %w", err)
	}
	return m, nil
}

func (m *metrics) runStarted() { m.inProgress.Add(context.Background(), 1) }

func (m *metrics) runEnded(outcome string, d time.Duration) {
	ctx := context.Background()
	attrs := metric.WithAttributes(attribute.String("outcome", outcome))
	m.inProgress.Add(ctx, -1)
	m.runs.Add(ctx, 1, attrs)
	m.runDuration.Record(ctx, d.Seconds(), attrs)
}

func (m *metrics) claimed(reason string) {
	m.claims.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", reason)))
}

func (m *metrics) heartbeatFailed() { m.heartbeatFailures.Add(context.Background(), 1) }

func (m *metrics) bookkeepingRetried() { m.bookkeepingRetries.Add(context.Background(), 1) }
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/scheduler/... -run TestMetrics`
Expected: `ok`.

- [ ] **Step 5: Commit**

```
git add internal/scheduler/metrics.go internal/scheduler/metrics_test.go
git commit -m "feat(scheduler): run, claim, heartbeat and bookkeeping instruments

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task R-6: The service — heartbeat, watchdog, claim loop, runs

**Spec:** §5.3 (run context, run guard), §6.1 (all), §6.2, §6.3, §9 (span).
§13 U rows: "no claim before the first heartbeat"; "database outage longer
than `STALE_AFTER` → no lost-owner claims before a full stale period of
healthy heartbeats"; "heartbeat failure → self-cancel at `W`; no claims until
recovery"; "no new commit after the watchdog fires…" (the scheduler's half: the
run's `Done` closes at `W`); "a hung heartbeat does not stop the watchdog";
"`STALE_AFTER` validation: a single slow or failed heartbeat never
self-cancels" (the service half); "a RUNNING task with no live run is given
back; a live run never is"; "a lost claim reply: the next `GiveBackIdle`
returns the claimed tasks"; "at most `MAX_RUNS` runs; a freed slot triggers an
immediate claim"; "an empty cluster view has no effect" (the service has no
cluster view to read); "bookkeeping after a self-cancel does not inherit the
run's cancellation"; "after a re-arm, every fenced write of the old life is
refused" (the scheduler's half: a refusal ends the run as superseded); "owner
lost 3 times" (`MaxLostOwners` reaches the engine).

**Files:**
- Create: `internal/scheduler/service.go`
- Test: `internal/scheduler/fakes_test.go`, `internal/scheduler/service_test.go`

**Interfaces:**
- Consumes:
  - S: `spi.ScheduledTaskStore` (`ClaimDue`, `Heartbeat`, `GiveBackIdle`, `SweepOwners`, `SweepMarks`, `RecordAttempt`, `Fail`, `RetireOwner`), `spi.ClaimRequest`, `spi.TaskRef`, `spi.TaskClaim`, `spi.ScheduledTaskRunning`, and the requested `ScheduledTask.ClaimedFromLostOwner` (Open point 1).
  - E: `workflow.RunReport`, `workflow.WithRunGuard`, `workflow.RunGuard{Ref, Store, Done, NoNewUnsafe, Unsafe}`, `workflow.UnsafeFlight`, `workflow.RunGuardFrom` (tests only), `workflow.OutcomeFailed` and the other outcomes (Open point 2).
  - R-2 … R-5.
- Produces:
  - `type Firer interface{ FireScheduledTransition(ctx context.Context, task spi.ScheduledTask, maxLostOwners int, retryDelay time.Duration) workflow.RunReport }`
  - `type Deps struct{ Store spi.StoreFactory; TxManager spi.TransactionManager; Firer Firer; Clock Clock; HealthFlag *atomic.Bool; Meter metric.Meter; CalloutDeadlineMax time.Duration }`
  - `func New(cfg Config, deps Deps) *Service`, `func (s *Service) Start(ctx context.Context) error`, `func (s *Service) Stop()` (R-9 turns `Stop` into `Drain`)
  - span `scheduler.run` with attribute `outcome`

- [ ] **Step 1: Write the test doubles**

`internal/scheduler/fakes_test.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// fakeStore scripts the scheduler's side of spi.ScheduledTaskStore. The
// embedded interface is left nil: a method no test scripts panics.
type fakeStore struct {
	spi.ScheduledTaskStore

	mu             sync.Mutex
	due            []spi.ScheduledTask
	claimErr       error // the claim happens, and its reply is lost
	claimReqs      []spi.ClaimRequest
	hbErr          error
	hbFailNext     int
	hbBlock        chan struct{} // non-nil: Heartbeat parks until it is closed, whatever its ctx says
	heartbeats     int
	hbFailures     int
	giveBacks      [][]uuid.UUID
	retired        int
	sweptOwners    []time.Duration
	sweptMarks     int
	outcomeErrs    []error // returned in order by RecordAttempt and Fail
	outcomeAlways  error   // returned by every RecordAttempt and Fail once outcomeErrs is empty
	outcomePanic   bool
	outcomeCtxErrs []error
	attempts       []spi.Attempt
	fails          []spi.Failure
	failInTx       []bool
}

func (f *fakeStore) with(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeStore) claims() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.claimReqs)
}

func (f *fakeStore) heartbeatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heartbeats
}

func (f *fakeStore) heartbeatFailures() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hbFailures
}

func (f *fakeStore) giveBackCalls() [][]uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.giveBacks)
}

func (f *fakeStore) lastGiveBack() ([]uuid.UUID, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.giveBacks) == 0 {
		return nil, false
	}
	return f.giveBacks[len(f.giveBacks)-1], true
}

func (f *fakeStore) attemptsRecorded() []spi.Attempt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.attempts)
}

func (f *fakeStore) failsRecorded() []spi.Failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.fails)
}

func (f *fakeStore) ClaimDue(_ context.Context, req spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimReqs = append(f.claimReqs, req)
	n := min(req.Limit, len(f.due))
	out := make([]spi.ScheduledTask, 0, n)
	for _, t := range f.due[:n] {
		t.Status = spi.ScheduledTaskRunning
		t.Claim = &spi.TaskClaim{Token: uuid.New(), Owner: req.Owner}
		out = append(out, t)
	}
	f.due = f.due[n:]
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	return out, nil
}

func (f *fakeStore) Heartbeat(context.Context, uuid.UUID) error {
	f.mu.Lock()
	f.heartbeats++
	block, err := f.hbBlock, f.hbErr
	if err == nil && f.hbFailNext > 0 {
		f.hbFailNext--
		err = errors.New("heartbeat: connection refused")
	}
	if err != nil {
		f.hbFailures++
	}
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return err
}

func (f *fakeStore) GiveBackIdle(_ context.Context, _ uuid.UUID, keep []uuid.UUID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.giveBacks = append(f.giveBacks, slices.Clone(keep))
	return 0, nil
}

func (f *fakeStore) RetireOwner(context.Context, uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retired++
	return nil
}

func (f *fakeStore) SweepOwners(_ context.Context, deadFor time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweptOwners = append(f.sweptOwners, deadFor)
	return nil
}

func (f *fakeStore) SweepMarks(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweptMarks++
	return nil
}

// outcome is called with f.mu held.
func (f *fakeStore) outcome(ctx context.Context) error {
	f.outcomeCtxErrs = append(f.outcomeCtxErrs, ctx.Err())
	if f.outcomePanic {
		panic("injected panic while recording an outcome")
	}
	if len(f.outcomeErrs) > 0 {
		err := f.outcomeErrs[0]
		f.outcomeErrs = f.outcomeErrs[1:]
		return err
	}
	return f.outcomeAlways
}

func (f *fakeStore) RecordAttempt(ctx context.Context, _ spi.TaskRef, a spi.Attempt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.outcome(ctx); err != nil {
		return err
	}
	f.attempts = append(f.attempts, a)
	return nil
}

func (f *fakeStore) Fail(ctx context.Context, _ spi.TaskRef, fl spi.Failure) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.outcome(ctx); err != nil {
		return err
	}
	f.fails = append(f.fails, fl)
	f.failInTx = append(f.failInTx, spi.GetTransaction(ctx) != nil)
	return nil
}

// testFactory is the memory factory with the scripted task store.
type testFactory struct {
	spi.StoreFactory
	sts spi.ScheduledTaskStore
}

func (f testFactory) ScheduledTaskStore(context.Context) (spi.ScheduledTaskStore, error) {
	return f.sts, nil
}

type firerFunc func(ctx context.Context, task spi.ScheduledTask, maxLostOwners int, retryDelay time.Duration) workflow.RunReport

func (f firerFunc) FireScheduledTransition(ctx context.Context, task spi.ScheduledTask, maxLostOwners int, retryDelay time.Duration) workflow.RunReport {
	return f(ctx, task, maxLostOwners, retryDelay)
}

func reportFirer(r workflow.RunReport) Firer {
	return firerFunc(func(context.Context, spi.ScheduledTask, int, time.Duration) workflow.RunReport { return r })
}

// failedOnCancel is what the engine reports for a run stopped by its cancellation.
func failedOnCancel(ctx context.Context) workflow.RunReport {
	<-ctx.Done()
	return workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: fmt.Errorf("run stopped: %w", ctx.Err())}
}

func testConfig() Config {
	return Config{
		Enabled:           true,
		ScanInterval:      5 * time.Millisecond,
		MaxRuns:           8,
		MaxRunsPerTenant:  4,
		HeartbeatInterval: 10 * time.Millisecond,
		StaleAfter:        time.Hour,
		MaxLostOwners:     3,
		RetryDelay:        30 * time.Second,
		RetryDelayMax:     15 * time.Minute,
		ShutdownDrain:     50 * time.Millisecond,
	}
}

type harness struct {
	svc  *Service
	fs   *fakeStore
	flag *atomic.Bool
	mem  *memory.StoreFactory
}

func newHarness(t *testing.T, cfg Config, firer Firer) *harness {
	t.Helper()
	mem := memory.NewStoreFactory()
	t.Cleanup(func() { _ = mem.Close() })
	fs := &fakeStore{}
	flag := &atomic.Bool{}
	flag.Store(true)
	svc := New(cfg, Deps{
		Store:              testFactory{StoreFactory: mem, sts: fs},
		TxManager:          mem.NewTransactionManager(common.NewTestUUIDGenerator()),
		Firer:              firer,
		Clock:              NewRealClock(),
		HealthFlag:         flag,
		CalloutDeadlineMax: time.Second,
	})
	// The test configs use a short STALE_AFTER that no watchdog window could be
	// derived from; tests that exercise the watchdog set their own.
	svc.window = time.Minute
	return &harness{svc: svc, fs: fs, flag: flag, mem: mem}
}

func (h *harness) start(t *testing.T) {
	t.Helper()
	if err := h.svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(h.svc.Stop)
}

func dueTask(tenant spi.TenantID, id string) spi.ScheduledTask {
	return spi.ScheduledTask{
		ID: id, TenantID: tenant, Type: spi.ScheduledTaskFireTransition, ScheduledTime: 1,
		EntityID: uuid.NewString(), ModelName: "M", ModelVersion: 1, Transition: "auto", SourceState: "OPEN",
		Status: spi.ScheduledTaskWaiting, ArmToken: uuid.New(), NextAttemptTime: 1,
	}
}

func liveRuns(s *Service) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting on a channel")
		var zero T
		return zero
	}
}
```

- [ ] **Step 2: Write the failing tests**

`internal/scheduler/service_test.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

var fired = workflow.RunReport{Outcome: workflow.OutcomeFired}

func TestService_DisabledStartsNothing(t *testing.T) {
	cfg := testConfig()
	cfg.Enabled = false
	h := newHarness(t, cfg, reportFirer(fired))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	time.Sleep(50 * time.Millisecond)
	if n, c := h.fs.heartbeatCount(), h.fs.claims(); n != 0 || c != 0 {
		t.Errorf("a disabled scheduler touched the store: %d heartbeats, %d claims", n, c)
	}
}

func TestService_StartIsIdempotent(t *testing.T) {
	cfg := testConfig()
	cfg.HeartbeatInterval = 20 * time.Millisecond
	h := newHarness(t, cfg, reportFirer(fired))
	h.start(t)
	if err := h.svc.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	// One heartbeat goroutine makes at most 11 calls in 200ms; two would make about 20.
	if n := h.fs.heartbeatCount(); n > 14 {
		t.Errorf("%d heartbeats in 200ms at a 20ms interval: a second Start started a second heartbeat goroutine", n)
	}
}

func TestService_NoClaimBeforeTheFirstHeartbeat(t *testing.T) {
	h := newHarness(t, testConfig(), reportFirer(fired))
	h.fs.with(func() {
		h.fs.hbErr = errors.New("heartbeat: connection refused")
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	eventually(t, "three failed heartbeats", func() bool { return h.fs.heartbeatFailures() >= 3 })
	if n := h.fs.claims(); n != 0 {
		t.Fatalf("%d claims before any heartbeat succeeded", n)
	}
	if n := len(h.fs.giveBackCalls()); n != 0 {
		t.Fatalf("%d give-backs before any heartbeat succeeded", n)
	}
	h.fs.with(func() { h.fs.hbErr = nil })
	eventually(t, "a claim once a heartbeat succeeds", func() bool { return h.fs.claims() > 0 })
}

func TestService_ClaimRequestCarriesOwnerLimitsAndTenantCounts(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRuns, cfg.MaxRunsPerTenant = 3, 2
	release := make(chan struct{})
	h := newHarness(t, cfg, firerFunc(func(context.Context, spi.ScheduledTask, int, time.Duration) workflow.RunReport {
		<-release
		return fired
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("tenant-a", "task-1")} })
	testStart := time.Now().UnixMilli()
	h.start(t)
	t.Cleanup(func() { close(release) })
	eventually(t, "a claim made while the run is in progress", func() bool { return h.fs.claims() >= 2 })

	var req spi.ClaimRequest
	h.fs.with(func() { req = h.fs.claimReqs[len(h.fs.claimReqs)-1] })
	if req.Owner != h.svc.incarnation {
		t.Error("the claim does not name this incarnation as its owner")
	}
	if req.Limit != 2 || req.PerTenantLimit != 2 || req.TenantInProgress["tenant-a"] != 1 {
		t.Errorf("Limit=%d PerTenantLimit=%d TenantInProgress=%v, want 2, 2 and tenant-a:1",
			req.Limit, req.PerTenantLimit, req.TenantInProgress)
	}
	if req.StaleAfter != cfg.StaleAfter {
		t.Errorf("StaleAfter = %s, want %s", req.StaleAfter, cfg.StaleAfter)
	}
	if req.NowMs < testStart || req.NowMs > time.Now().UnixMilli() {
		t.Errorf("NowMs = %d, want the pnode clock", req.NowMs)
	}
}

func TestService_LostOwnerClaimsWaitForAFullStalePeriodOfCleanHeartbeats(t *testing.T) {
	cfg := testConfig()
	cfg.StaleAfter = 300 * time.Millisecond
	h := newHarness(t, cfg, reportFirer(fired))
	h.start(t)
	eventually(t, "a claim", func() bool { return h.fs.claims() > 0 })
	h.fs.with(func() {
		if h.fs.claimReqs[0].AllowLostOwner {
			t.Error("the first claim after start allowed lost-owner claims")
		}
	})
	eventually(t, "lost-owner claims after STALE_AFTER of clean heartbeats", func() bool {
		var allow bool
		h.fs.with(func() { allow = h.fs.claimReqs[len(h.fs.claimReqs)-1].AllowLostOwner })
		return allow
	})

	h.fs.with(func() { h.fs.hbErr = errors.New("heartbeat: connection refused") })
	eventually(t, "two failed heartbeats", func() bool { return h.fs.heartbeatFailures() >= 2 })
	idx := h.fs.claims()
	h.fs.with(func() { h.fs.hbErr = nil })
	eventually(t, "a claim after the outage", func() bool { return h.fs.claims() > idx })
	h.fs.with(func() {
		if h.fs.claimReqs[idx].AllowLostOwner {
			t.Error("the first claim after an outage allowed lost-owner claims")
		}
	})
}

func TestService_AtMostMaxRunsAndAFreedSlotClaimsAtOnce(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRuns = 2
	cfg.ScanInterval = time.Second
	release := map[string]chan struct{}{"task-1": make(chan struct{}), "task-2": make(chan struct{}), "task-3": make(chan struct{})}
	var running, most atomic.Int32
	started := make(chan string, 3)
	h := newHarness(t, cfg, firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		n := running.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		started <- task.ID
		<-release[task.ID]
		running.Add(-1)
		return fired
	}))
	h.fs.with(func() {
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1"), dueTask("t2", "task-2"), dueTask("t3", "task-3")}
	})
	h.start(t)
	t.Cleanup(func() {
		for _, ch := range release {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	})

	first := receive(t, started)
	receive(t, started)
	h.fs.with(func() {
		if got := h.fs.claimReqs[0].Limit; got != 2 {
			t.Errorf("first claim Limit = %d, want MAX_RUNS 2", got)
		}
	})
	freed := time.Now()
	close(release[first])
	if third := receive(t, started); third != "task-3" {
		t.Errorf("third run = %s, want task-3", third)
	}
	if d := time.Since(freed); d > 500*time.Millisecond {
		t.Errorf("the freed slot was claimed %s after it freed; want at once, not at the next 1s tick", d)
	}
	if m := most.Load(); m > 2 {
		t.Errorf("%d runs at once, want at most MAX_RUNS 2", m)
	}
}

func TestService_GiveBackIdleKeepsEveryLiveRun(t *testing.T) {
	release := make(chan struct{})
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, testConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		tokens <- task.Claim.Token
		<-release
		return fired
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	token := receive(t, tokens)
	eventually(t, "a give-back that keeps the live run", func() bool {
		keep, ok := h.fs.lastGiveBack()
		return ok && slices.Contains(keep, token)
	})
	close(release)
	eventually(t, "a give-back that no longer keeps the finished run", func() bool {
		keep, ok := h.fs.lastGiveBack()
		return ok && !slices.Contains(keep, token)
	})
}

func TestService_LostClaimReplyIsGivenBack(t *testing.T) {
	h := newHarness(t, testConfig(), firerFunc(func(context.Context, spi.ScheduledTask, int, time.Duration) workflow.RunReport {
		t.Error("a task whose claim reply was lost was run")
		return fired
	}))
	h.fs.with(func() {
		h.fs.claimErr = errors.New("claim: connection reset after commit")
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	eventually(t, "the failed claim", func() bool { return h.fs.claims() >= 1 })
	calls := len(h.fs.giveBackCalls())
	eventually(t, "a later give-back", func() bool { return len(h.fs.giveBackCalls()) > calls })
	for _, keep := range h.fs.giveBackCalls()[calls:] {
		if len(keep) != 0 {
			t.Errorf("a give-back kept %v; nothing is live, so the lost claim must be given back", keep)
		}
	}
}

func TestService_SweepsDeadOwnersAndEndedMarks(t *testing.T) {
	cfg := testConfig()
	h := newHarness(t, cfg, reportFirer(fired))
	h.svc.sweepEvery = 20 * time.Millisecond
	h.start(t)
	eventually(t, "an owner sweep and a mark sweep", func() bool {
		var owners, marks int
		h.fs.with(func() { owners, marks = len(h.fs.sweptOwners), h.fs.sweptMarks })
		return owners > 0 && marks > 0
	})
	h.fs.with(func() {
		if got := h.fs.sweptOwners[0]; got != 10*cfg.StaleAfter {
			t.Errorf("SweepOwners(%s), want 10 x STALE_AFTER = %s", got, 10*cfg.StaleAfter)
		}
	})
}

func TestService_RunsUnderTheSystemIdentityAndItsRunGuard(t *testing.T) {
	cfg := testConfig()
	type seen struct {
		uc      *spi.UserContext
		guard   *workflow.RunGuard
		task    spi.ScheduledTask
		maxLost int
		retry   time.Duration
	}
	got := make(chan seen, 1)
	h := newHarness(t, cfg, firerFunc(func(ctx context.Context, task spi.ScheduledTask, maxLost int, retry time.Duration) workflow.RunReport {
		got <- seen{spi.GetUserContext(ctx), workflow.RunGuardFrom(ctx), task, maxLost, retry}
		return fired
	}))
	task := dueTask("tenant-x", "task-1")
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{task} })
	h.start(t)
	s := receive(t, got)

	if s.uc == nil || s.uc.Kind != spi.PrincipalSystem || s.uc.Tenant.ID != "tenant-x" {
		t.Errorf("run identity = %+v, want the system principal of tenant-x", s.uc)
	}
	if s.guard == nil {
		t.Fatal("no run guard on the run's context")
	}
	want := spi.TaskRef{TenantID: "tenant-x", ID: "task-1", ArmToken: task.ArmToken, ClaimToken: s.task.Claim.Token}
	if s.guard.Ref != want {
		t.Errorf("guard ref = %+v, want %+v", s.guard.Ref, want)
	}
	if s.guard.Store == nil || s.guard.Done == nil || s.guard.NoNewUnsafe == nil || s.guard.Unsafe == nil {
		t.Errorf("run guard incomplete: %+v", s.guard)
	}
	if s.maxLost != cfg.MaxLostOwners || s.retry != cfg.RetryDelay {
		t.Errorf("engine got maxLostOwners=%d retryDelay=%s, want %d and %s", s.maxLost, s.retry, cfg.MaxLostOwners, cfg.RetryDelay)
	}
}

func TestService_CommittedOutcomesRecordNothing(t *testing.T) {
	for _, outcome := range []workflow.ScheduledOutcome{
		workflow.OutcomeFired, workflow.OutcomeDeclined, workflow.OutcomeExpired,
		workflow.OutcomeCancelled, workflow.OutcomeSuperseded,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			h := newHarness(t, testConfig(), reportFirer(workflow.RunReport{Outcome: outcome}))
			h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
			h.start(t)
			eventually(t, "the run claimed and released", func() bool { return h.fs.claims() > 0 && liveRuns(h.svc) == 0 })
			time.Sleep(20 * time.Millisecond)
			if a, f := len(h.fs.attemptsRecorded()), len(h.fs.failsRecorded()); a+f != 0 {
				t.Errorf("a %s run wrote %d attempts and %d failures", outcome, a, f)
			}
			if !h.flag.Load() {
				t.Error("a normal run latched the node")
			}
		})
	}
}

func TestService_FailedRunRecordsAnAttempt(t *testing.T) {
	cfg := testConfig()
	h := newHarness(t, cfg, reportFirer(workflow.RunReport{Outcome: workflow.OutcomeFailed,
		Err: fmt.Errorf("criterion failed: %w", &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined"})}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	before := time.Now().UnixMilli()
	h.start(t)
	eventually(t, "one recorded attempt", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	a := h.fs.attemptsRecorded()[0]
	if a.Error != "card declined" || a.NotCounted || a.ClearOwnMark {
		t.Errorf("attempt = %+v, want a counted attempt with the compute node's text", a)
	}
	if lo, hi := before+cfg.RetryDelay.Milliseconds(), time.Now().UnixMilli()+cfg.RetryDelay.Milliseconds(); a.NextAttemptTime < lo || a.NextAttemptTime > hi {
		t.Errorf("NextAttemptTime = %d, want now + RETRY_DELAY", a.NextAttemptTime)
	}
	eventually(t, "the run released", func() bool { return liveRuns(h.svc) == 0 })
}

func TestService_EngineDecidedFailureFailsTheTask(t *testing.T) {
	h := newHarness(t, testConfig(), reportFirer(workflow.RunReport{Outcome: workflow.OutcomeFailed, FailReason: spi.FailureOwnerLostRepeatedly}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the task failed", func() bool { return len(h.fs.failsRecorded()) == 1 })
	if got := h.fs.failsRecorded()[0].Reason; got != spi.FailureOwnerLostRepeatedly {
		t.Errorf("reason = %s, want OWNER_LOST_REPEATEDLY", got)
	}
}

func TestService_RefusedOutcomeMeansSuperseded(t *testing.T) {
	h := newHarness(t, testConfig(), reportFirer(workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: errors.New("boom")}))
	h.fs.with(func() {
		h.fs.outcomeErrs = []error{fmt.Errorf("record attempt: %w", spi.ErrStaleClaim)}
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	eventually(t, "the run released", func() bool { return h.fs.claims() > 0 && liveRuns(h.svc) == 0 })
	time.Sleep(20 * time.Millisecond)
	h.fs.with(func() {
		if n := len(h.fs.outcomeCtxErrs); n != 1 {
			t.Errorf("%d outcome writes, want exactly one: a refusal is final", n)
		}
	})
}

func TestService_FailingHeartbeatsSelfCancelAtTheWindow(t *testing.T) {
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		return failedOnCancel(ctx)
	}))
	h.svc.window = 150 * time.Millisecond
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })
	h.fs.with(func() { h.fs.hbErr = errors.New("heartbeat: connection refused") })

	eventually(t, "the self-cancelled run's attempt", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	a := h.fs.attemptsRecorded()[0]
	if a.NotCounted || a.Error != cancelledText {
		t.Errorf("attempt = %+v, want a counted attempt with %q", a, cancelledText)
	}
	h.fs.with(func() {
		if err := h.fs.outcomeCtxErrs[0]; err != nil {
			t.Errorf("the outcome write inherited the run's cancellation: %v", err)
		}
	})

	idle := h.fs.claims()
	time.Sleep(50 * time.Millisecond)
	if n := h.fs.claims(); n != idle {
		t.Errorf("%d claims while heartbeats were failing", n-idle)
	}
	h.fs.with(func() { h.fs.hbErr = nil })
	eventually(t, "claims resume after a heartbeat succeeds", func() bool { return h.fs.claims() > idle })
}

func TestService_OneFailedHeartbeatDoesNotSelfCancel(t *testing.T) {
	cancelled := make(chan struct{})
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		r := failedOnCancel(ctx)
		close(cancelled)
		return r
	}))
	h.svc.window = 100 * time.Millisecond // ten heartbeat intervals
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })
	h.fs.with(func() { h.fs.hbFailNext = 1 })
	eventually(t, "the one failed heartbeat", func() bool { return h.fs.heartbeatFailures() == 1 })
	select {
	case <-cancelled:
		t.Fatal("one failed heartbeat cancelled the pnode's runs")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestService_HungHeartbeatDoesNotStopTheWatchdog(t *testing.T) {
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		return failedOnCancel(ctx)
	}))
	h.svc.window = 100 * time.Millisecond
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	h.fs.with(func() { h.fs.hbBlock = hang })
	eventually(t, "the run cancelled while the heartbeat hangs", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
}

func TestService_HeartbeatThatSucceedsAfterItsWindowCountsAsFailed(t *testing.T) {
	h := newHarness(t, testConfig(), reportFirer(fired))
	h.start(t)
	eventually(t, "healthy", h.svc.isHealthy)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	h.fs.with(func() { h.fs.hbBlock = hang })
	beats := h.fs.heartbeatCount()
	eventually(t, "the heartbeat goroutine parked", func() bool { return h.fs.heartbeatCount() > beats })

	h.svc.heartbeatDone(time.Now().Add(-2*h.svc.window), nil)
	if h.svc.isHealthy() {
		t.Error("a heartbeat that succeeded after its window left the pnode claiming")
	}
}
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `go test ./internal/scheduler/... -run TestService_`
Expected: build failure — `undefined: New`, `Deps`, `Firer`, `Service`, and the unexported fields the tests read.

- [ ] **Step 4: Implement**

`internal/scheduler/service.go`:

```go
// Package scheduler runs scheduled transitions. Every pnode claims due tasks
// from the store and runs them itself, and the claim token fences every write
// a run makes. A heartbeat proves the pnode alive; a watchdog cancels the
// pnode's runs before another pnode could consider it stale.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
)

// Firer runs one claimed task. *workflow.Engine satisfies it.
type Firer interface {
	FireScheduledTransition(ctx context.Context, task spi.ScheduledTask, maxLostOwners int, retryDelay time.Duration) workflow.RunReport
}

// Deps are the service's collaborators. HealthFlag and Meter may be nil.
type Deps struct {
	Store     spi.StoreFactory
	TxManager spi.TransactionManager
	Firer     Firer
	// Clock is the pnode clock: the one that set scheduledTime at arm.
	Clock Clock
	// HealthFlag is the process-wide flag a recovered panic latches false.
	HealthFlag *atomic.Bool
	Meter      metric.Meter
	// CalloutDeadlineMax is the longest one callout can take. Shutdown step 4
	// waits for it, counted from the start of each unsafe callout in flight.
	CalloutDeadlineMax time.Duration
}

// storeCallBudget bounds one store call the scheduler makes outside a run: a
// claim, a give-back, a sweep, one attempt at recording an outcome.
const storeCallBudget = 10 * time.Second

// sweepInterval is how often the claim loop sweeps dead owners and the marks
// of ended lives.
const sweepInterval = time.Minute

// ownerSweepFactor × STALE_AFTER is how long a dead owner's liveness record
// is kept.
const ownerSweepFactor = 10

var errHeartbeatLate = errors.New("the heartbeat succeeded after its watchdog window")

type cancelReason int

const (
	notCancelled cancelReason = iota
	selfCancelled
	latchCancelled
	shutdownCancelled
)

// liveRun is one claim this incarnation holds.
type liveRun struct {
	task   spi.ScheduledTask
	ref    spi.TaskRef
	ctx    context.Context
	cancel context.CancelFunc
	unsafe *workflow.UnsafeFlight

	// Guarded by Service.mu.
	reason cancelReason
	ended  bool // the fire has returned
}

// Service is one pnode's scheduler: a claim loop, a heartbeat goroutine, a
// watchdog goroutine and one goroutine per run.
type Service struct {
	cfg         Config
	deps        Deps
	incarnation uuid.UUID
	window      time.Duration // W
	sweepEvery  time.Duration
	m           *metrics
	store       spi.ScheduledTaskStore

	mu           sync.Mutex
	started      bool
	runs         map[uuid.UUID]*liveRun // by claim token
	perTenant    map[spi.TenantID]int
	healthy      bool      // the last heartbeat succeeded in time and the watchdog has not fired since
	healthySince time.Time // start of the current run of clean heartbeats
	latched      bool
	draining     bool
	filled       bool // the last claim took every free slot

	slotFreed  chan struct{}
	drainingCh chan struct{} // closed at shutdown step 1
	stopLoop   chan struct{}
	loopDone   chan struct{}
	stopHB     chan struct{}
	hbDone     chan struct{}
	wdDone     chan struct{}
	wdArm      chan time.Time
	runsWG     sync.WaitGroup

	startOnce sync.Once
	stopOnce  sync.Once
}

// New builds a service. Start begins claiming.
func New(cfg Config, deps Deps) *Service {
	return &Service{
		cfg:         cfg,
		deps:        deps,
		incarnation: uuid.New(),
		window:      watchdogWindow(cfg.StaleAfter),
		sweepEvery:  sweepInterval,
		runs:        make(map[uuid.UUID]*liveRun),
		perTenant:   make(map[spi.TenantID]int),
		slotFreed:   make(chan struct{}, 1),
		drainingCh:  make(chan struct{}),
		stopLoop:    make(chan struct{}),
		loopDone:    make(chan struct{}),
		stopHB:      make(chan struct{}),
		hbDone:      make(chan struct{}),
		wdDone:      make(chan struct{}),
		wdArm:       make(chan time.Time, 1),
	}
}

// Start starts the heartbeat, the watchdog and the claim loop. It returns
// once the first heartbeat is scheduled. A disabled service starts nothing.
// Only the first call does anything.
func (s *Service) Start(ctx context.Context) error {
	if !s.cfg.Enabled {
		return nil
	}
	var err error
	s.startOnce.Do(func() {
		var m *metrics
		if m, err = newMetrics(s.deps.Meter); err != nil {
			return
		}
		var store spi.ScheduledTaskStore
		if store, err = s.deps.Store.ScheduledTaskStore(ctx); err != nil {
			err = fmt.Errorf("failed to get the scheduled task store: %w", err)
			return
		}
		s.mu.Lock()
		s.m, s.store, s.started = m, store, true
		s.mu.Unlock()
		go s.watchdogLoop()
		go s.heartbeatLoop()
		go s.loop()
	})
	return err
}

// Stop stops the claim loop, the heartbeat and the watchdog.
func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		started := s.started
		s.mu.Unlock()
		if !started {
			return
		}
		close(s.stopLoop)
		<-s.loopDone
		close(s.stopHB)
		<-s.hbDone
		<-s.wdDone
	})
}

// --- liveness ---------------------------------------------------------------

func (s *Service) heartbeatLoop() {
	defer close(s.hbDone)
	defer s.recoverLatch("heartbeat")
	ticker := time.NewTicker(s.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		s.heartbeat()
		select {
		case <-s.stopHB:
			return
		case <-ticker.C:
		}
	}
}

// heartbeat records the moment before the call acquires its connection; the
// store's stamp is never earlier.
func (s *Service) heartbeat() {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), heartbeatBudget(s.cfg.HeartbeatInterval))
	err := s.store.Heartbeat(ctx, s.incarnation)
	cancel()
	s.heartbeatDone(started, err)
}

func (s *Service) heartbeatDone(started time.Time, err error) {
	if err == nil && time.Since(started) >= s.window {
		err = errHeartbeatLate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.healthy, s.healthySince = false, time.Time{}
		s.m.heartbeatFailed()
		slog.Warn("scheduler heartbeat failed; no claims until one succeeds", "pkg", "scheduler", "err", err)
		return
	}
	if !s.healthy {
		s.healthy, s.healthySince = true, started
	}
	select {
	case <-s.wdArm:
	default:
	}
	s.wdArm <- started.Add(s.window)
}

func (s *Service) watchdogLoop() {
	defer close(s.wdDone)
	defer s.recoverLatch("watchdog")
	timer := time.NewTimer(0)
	timer.Stop()
	for {
		select {
		case <-s.stopHB:
			timer.Stop()
			return
		case at := <-s.wdArm:
			timer.Reset(time.Until(at))
		case <-timer.C:
			s.selfCancel()
		}
	}
}

// selfCancel runs when W has passed since the last successful heartbeat began:
// every run in progress is cancelled, and nothing is claimed until a heartbeat
// succeeds.
func (s *Service) selfCancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthy, s.healthySince = false, time.Time{}
	n := 0
	for _, r := range s.runs {
		if r.ended {
			continue
		}
		if r.reason == notCancelled {
			r.reason = selfCancelled
		}
		r.cancel()
		n++
	}
	slog.Warn("scheduler heartbeats failed for the whole watchdog window; cancelled every run in progress",
		"pkg", "scheduler", "runs", n)
}

func (s *Service) isHealthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthy
}

// --- claiming ---------------------------------------------------------------

// loop is the only goroutine that calls ClaimDue and GiveBackIdle.
func (s *Service) loop() {
	defer close(s.loopDone)
	defer s.recoverLatch("claim loop")
	scan := time.NewTicker(s.cfg.ScanInterval)
	defer scan.Stop()
	sweep := time.NewTicker(s.sweepEvery)
	defer sweep.Stop()
	for {
		select {
		case <-s.stopLoop:
			return
		case <-scan.C:
			s.tick()
		case <-s.slotFreed:
			s.claim()
		case <-sweep.C:
			if s.isHealthy() {
				s.sweep()
			}
		}
	}
}

func (s *Service) tick() {
	if !s.isHealthy() {
		return
	}
	s.giveBackIdle()
	s.claim()
}

// giveBackIdle returns to WAITING, uncounted, every task this incarnation holds
// RUNNING without a live run: a claim whose reply was lost.
func (s *Service) giveBackIdle() {
	keep := s.liveTokens()
	ctx, cancel := context.WithTimeout(context.Background(), storeCallBudget)
	defer cancel()
	n, err := s.store.GiveBackIdle(ctx, s.incarnation, keep)
	if err != nil {
		slog.Warn("scheduler could not give back idle claims", "pkg", "scheduler", "err", err)
		return
	}
	if n > 0 {
		slog.Info("scheduler gave back claims that had no run", "pkg", "scheduler", "count", n)
	}
}

func (s *Service) liveTokens() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := make([]uuid.UUID, 0, len(s.runs))
	for token := range s.runs {
		keep = append(keep, token)
	}
	return keep
}

func (s *Service) claim() {
	s.mu.Lock()
	if !s.healthy || s.latched || s.draining {
		s.mu.Unlock()
		return
	}
	free := s.cfg.MaxRuns - len(s.runs)
	if free <= 0 {
		s.filled = true
		s.mu.Unlock()
		return
	}
	inProgress := make(map[spi.TenantID]int, len(s.perTenant))
	for tenant, n := range s.perTenant {
		inProgress[tenant] = n
	}
	req := spi.ClaimRequest{
		Owner:            s.incarnation,
		NowMs:            s.deps.Clock.Now().UnixMilli(),
		StaleAfter:       s.cfg.StaleAfter,
		Limit:            free,
		PerTenantLimit:   s.cfg.MaxRunsPerTenant,
		TenantInProgress: inProgress,
		AllowLostOwner:   time.Since(s.healthySince) >= s.cfg.StaleAfter,
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), storeCallBudget)
	tasks, err := s.store.ClaimDue(ctx, req)
	cancel()
	if err != nil {
		slog.Warn("scheduler claim failed", "pkg", "scheduler", "err", err)
		return
	}

	// Every claimed task joins the set of live runs before anything else runs
	// on this goroutine, the next give-back included.
	s.mu.Lock()
	s.filled = len(tasks) >= free
	runs := make([]*liveRun, 0, len(tasks))
	for _, t := range tasks {
		runs = append(runs, s.registerLocked(t))
	}
	s.mu.Unlock()
	for _, r := range runs {
		s.m.claimed(claimReason(r.task))
		go s.run(r)
	}
}

func (s *Service) registerLocked(t spi.ScheduledTask) *liveRun {
	ctx, cancel := context.WithCancel(context.Background())
	r := &liveRun{
		task:   t,
		ref:    spi.TaskRef{TenantID: t.TenantID, ID: t.ID, ArmToken: t.ArmToken, ClaimToken: t.Claim.Token},
		ctx:    ctx,
		cancel: cancel,
		unsafe: &workflow.UnsafeFlight{},
	}
	s.runs[r.ref.ClaimToken] = r
	s.perTenant[t.TenantID]++
	s.runsWG.Add(1)
	s.m.runStarted()
	switch {
	case s.latched:
		r.reason = latchCancelled
		cancel()
	case !s.healthy:
		r.reason = selfCancelled
		cancel()
	}
	return r
}

func claimReason(t spi.ScheduledTask) string {
	if t.ClaimedFromLostOwner {
		return claimOwnerLost
	}
	return claimDue
}

// sweep removes dead owners' liveness records and the marks of ended lives.
func (s *Service) sweep() {
	ctx, cancel := context.WithTimeout(context.Background(), storeCallBudget)
	defer cancel()
	if err := s.store.SweepOwners(ctx, ownerSweepFactor*s.cfg.StaleAfter); err != nil {
		slog.Warn("scheduler could not sweep dead owners", "pkg", "scheduler", "err", err)
	}
	if err := s.store.SweepMarks(ctx); err != nil {
		slog.Warn("scheduler could not sweep the marks of ended lives", "pkg", "scheduler", "err", err)
	}
}

// --- runs -------------------------------------------------------------------

func (s *Service) run(r *liveRun) {
	defer s.runsWG.Done()
	start := time.Now()
	ctx, span := observability.Tracer().Start(r.ctx, "scheduler.run")
	defer span.End()

	rep := s.fire(ctx, r)
	nowMs := s.deps.Clock.Now().UnixMilli()

	s.mu.Lock()
	r.ended = true
	reason, draining := r.reason, s.draining
	s.mu.Unlock()

	cut := rep.Outcome == workflow.OutcomeFailed && errors.Is(rep.Err, context.Canceled) &&
		(reason == shutdownCancelled || (reason == notCancelled && draining))
	var errText string
	if rep.Err != nil {
		text, ticket, warnOnly := recordedError(rep.Err)
		errText = sanitiseErrorText(text)
		if !warnOnly {
			slog.Error("scheduled run failed with an internal error", "pkg", "scheduler",
				"taskId", r.task.ID, "tenant", string(r.task.TenantID), "ticket", ticket.String(), "err", rep.Err)
		}
	}
	bk := decideBookkeeping(rep, r.task, cut, false, nowMs, s.cfg, errText)
	outcome := runOutcome(rep, bk, false, reason, cut)
	err := s.book(r, bk)
	if errors.Is(err, spi.ErrStaleClaim) {
		outcome = outcomeSuperseded
	}
	s.finish(r, bk, err)
	span.SetAttributes(attribute.String("outcome", outcome))
	s.m.runEnded(outcome, time.Since(start))
}

func (s *Service) fire(ctx context.Context, r *liveRun) workflow.RunReport {
	ctx = spi.WithUserContext(ctx, common.SystemUserContextValue(r.task.TenantID))
	ctx = workflow.WithRunGuard(ctx, &workflow.RunGuard{
		Ref: r.ref, Store: s.store, Done: r.ctx.Done(), NoNewUnsafe: s.drainingCh, Unsafe: r.unsafe,
	})
	return s.deps.Firer.FireScheduledTransition(ctx, r.task, s.cfg.MaxLostOwners, s.cfg.RetryDelay)
}

// book records the outcome with one fenced write that never inherits the
// run's cancellation.
func (s *Service) book(r *liveRun, bk Bookkeeping) error {
	if bk.Kind == NoneKind {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), storeCallBudget)
	defer cancel()
	return s.writeOutcome(ctx, r, bk)
}

func (s *Service) writeOutcome(ctx context.Context, r *liveRun, bk Bookkeeping) error {
	if bk.Kind == FailKind {
		return s.store.Fail(ctx, r.ref, bk.Failure)
	}
	return s.store.RecordAttempt(ctx, r.ref, bk.Attempt)
}

// finish releases the claim once its outcome is accepted or refused. A claim
// whose outcome is not recorded stays in the set.
func (s *Service) finish(r *liveRun, bk Bookkeeping, bookErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bk.Kind != NoneKind && bookErr != nil && !errors.Is(bookErr, spi.ErrStaleClaim) {
		return
	}
	s.releaseLocked(r)
	if s.filled {
		s.filled = false
		select {
		case s.slotFreed <- struct{}{}:
		default:
		}
	}
}

func (s *Service) releaseLocked(r *liveRun) {
	if _, ok := s.runs[r.ref.ClaimToken]; !ok {
		return
	}
	delete(s.runs, r.ref.ClaimToken)
	s.perTenant[r.task.TenantID]--
	if s.perTenant[r.task.TenantID] <= 0 {
		delete(s.perTenant, r.task.TenantID)
	}
	r.cancel()
}

func runOutcome(rep workflow.RunReport, bk Bookkeeping, panicked bool, reason cancelReason, cut bool) string {
	switch {
	case panicked:
		return outcomePanicked
	case rep.Outcome != workflow.OutcomeFailed:
		return string(rep.Outcome)
	case bk.Kind == FailKind:
		return outcomeFailed
	case cut:
		return outcomeShutdownCancelled
	case reason == selfCancelled:
		return outcomeSelfCancelled
	default:
		return outcomeAttemptFailed
	}
}

// --- panics -----------------------------------------------------------------

// recoverLatch recovers a panic in a scheduler goroutine and latches the node.
// Deferred directly, so recover sees the panic.
func (s *Service) recoverLatch(site string) {
	v := recover()
	if v == nil {
		return
	}
	ticket := uuid.New()
	slog.Error("panic recovered in the scheduler "+site+"; node latched", "pkg", "scheduler",
		"ticket", ticket.String(), "err", fmt.Errorf("panic: %v", v), "stack", string(debug.Stack()))
	s.latch()
}

// latch marks the node unhealthy for good; a latched node claims nothing.
func (s *Service) latch() {
	if s.deps.HealthFlag != nil {
		s.deps.HealthFlag.Store(false)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latched = true
}
```

- [ ] **Step 5: Run the tests to see them pass**

Run: `go test ./internal/scheduler/... -run TestService_`
Expected: `ok`.

Then the whole package: `go test ./internal/scheduler/...` — `ok`.

- [ ] **Step 6: Commit**

```
git add internal/scheduler/service.go internal/scheduler/fakes_test.go internal/scheduler/service_test.go
git commit -m "feat(scheduler): each pnode claims due tasks and runs them under a heartbeat and a watchdog

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task R-7: Recording the outcome — retry by default, latch on a rejection, FAILED with its audit event

**Spec:** §5.6 (the write, its context and timeout, retry by default, WARN at
most once a minute per task, latch only on `spi.ErrStoreRejected`, the WARN
line after `RecordAttempt` is accepted), §5.7, §9 (ERROR on FAILED with the
reason and a ticket). §13 U rows: "a bookkeeping write retried through an
outage; accepted after recovery"; "`spi.ErrStoreRejected` → ERROR with ticket,
node latched"; "a bookkeeping write blocked by a lock or an exhausted pool is
retried without latching"; "database outage during `MarkUnsafe` →
`RecordAttempt{ClearOwnMark}` after recovery" and "`RecordAttempt{ClearOwnMark}`
retried in an outage" (the retry half); "`SCHEDULED_TRANSITION_FAIL` recorded
with its reason"; "`lastError` of a non-sentinel store error is 'internal error
[ticket]' only" (the ticket matches the ERROR line).

**Files:**
- Modify: `internal/scheduler/service.go` (`liveRun`, `Service`, `New`, `Stop`, `run`, `book`, `writeOutcome`, `finish`; new `failWithAudit`)
- Test: `internal/scheduler/outcome_test.go`

**Interfaces:**
- Consumes: `spi.ErrStoreRejected`, `spi.SMEventScheduledTransitionFailed` (S); `Deps.TxManager`; `common.ShieldedCommit`, `common.RollbackContext`.
- Produces: the log lines `scheduled run failed; the task waits for its next attempt` (WARN) and `scheduled task FAILED` (ERROR, with `reason` and `ticket`); the audit event `SCHEDULED_TRANSITION_FAIL` with data `{transition, sourceState, reason, attempts, lostOwners}`, `State` = the entity's state in the same transaction, or the task's source state when the entity is gone.

- [ ] **Step 1: Write the failing tests**

`internal/scheduler/outcome_test.go`:

```go
package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

var safeFailure = workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: errors.New("boom")}

func TestService_OutcomeWriteRetriedThroughAnOutage(t *testing.T) {
	outage := errors.New("record attempt: dial tcp 10.0.0.5:5432: connect: connection refused")
	h := newHarness(t, testConfig(), reportFirer(safeFailure))
	h.fs.with(func() {
		h.fs.outcomeErrs = []error{outage, outage}
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	eventually(t, "the attempt accepted after the outage", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	h.fs.with(func() {
		if n := len(h.fs.outcomeCtxErrs); n != 3 {
			t.Errorf("%d outcome writes, want 3", n)
		}
	})
	if !h.flag.Load() {
		t.Error("an outage latched the node")
	}
	eventually(t, "the run released", func() bool { return liveRuns(h.svc) == 0 })
}

func TestService_LockAndPoolErrorsAreRetriedWithoutLatching(t *testing.T) {
	h := newHarness(t, testConfig(), reportFirer(safeFailure))
	h.fs.with(func() {
		h.fs.outcomeErrs = []error{
			fmt.Errorf("record attempt: %w", context.DeadlineExceeded),
			errors.New("failed to acquire connection: pool exhausted"),
			fmt.Errorf("record attempt: %w", spi.ErrConflict),
		}
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	eventually(t, "the attempt accepted", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	if !h.flag.Load() {
		t.Error("a lock wait, a full pool or a conflict latched the node")
	}
}

func TestService_StoreRejectionLatchesAndKeepsTheClaim(t *testing.T) {
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, testConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		tokens <- task.Claim.Token
		return safeFailure
	}))
	h.fs.with(func() {
		h.fs.outcomeErrs = []error{fmt.Errorf("record attempt: value too long: %w", spi.ErrStoreRejected)}
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	token := receive(t, tokens)
	eventually(t, "the node latched", func() bool { return !h.flag.Load() })
	claims := h.fs.claims()
	time.Sleep(50 * time.Millisecond)
	h.fs.with(func() {
		if n := len(h.fs.outcomeCtxErrs); n != 1 {
			t.Errorf("%d outcome writes, want one: a rejection is not retried", n)
		}
	})
	if n := h.fs.claims(); n != claims {
		t.Errorf("%d claims after the latch", n-claims)
	}
	eventually(t, "the claim kept by the give-back", func() bool {
		keep, ok := h.fs.lastGiveBack()
		return ok && slices.Contains(keep, token)
	})
}

func TestService_FailAndItsAuditEventCommitTogether(t *testing.T) {
	task := dueTask("t1", "task-1")
	h := newHarness(t, testConfig(), reportFirer(workflow.RunReport{
		Outcome: workflow.OutcomeFailed, MarkHeld: true, UnsafeReached: true,
		Err: &contract.CalloutFailure{Kind: contract.NoAnswer, Message: "DISPATCH_TIMEOUT: processor dispatch timed out after 100ms: no response"},
	}))
	h.fs.with(func() {
		h.fs.outcomeErrs = []error{errors.New("fail: connection refused")}
		h.fs.due = []spi.ScheduledTask{task}
	})
	h.start(t)
	eventually(t, "the task failed", func() bool { return len(h.fs.failsRecorded()) == 1 })
	h.fs.with(func() {
		if !h.fs.failInTx[0] {
			t.Error("Fail ran outside a transaction")
		}
	})
	if got := h.fs.failsRecorded()[0]; got.Reason != spi.FailureUnsafeWorkNotCompleted ||
		got.Error != "DISPATCH_TIMEOUT: processor dispatch timed out after 100ms: no response" {
		t.Errorf("failure = %+v", got)
	}

	ctx := common.SystemUserContext("t1")
	audit, err := h.mem.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("audit store: %v", err)
	}
	events, err := audit.GetEvents(ctx, task.EntityID)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	var failed []spi.StateMachineEvent
	for _, e := range events {
		if e.EventType == spi.SMEventScheduledTransitionFailed {
			failed = append(failed, e)
		}
	}
	if len(failed) != 1 {
		t.Fatalf("%d SCHEDULED_TRANSITION_FAIL events, want exactly one", len(failed))
	}
	e := failed[0]
	if e.State != task.SourceState {
		t.Errorf("event state = %q, want the source state %q for an entity that does not exist", e.State, task.SourceState)
	}
	for key, want := range map[string]string{
		"transition": task.Transition, "sourceState": task.SourceState,
		"reason": "UNSAFE_WORK_NOT_COMPLETED", "attempts": "0", "lostOwners": "0",
	} {
		if got := fmt.Sprint(e.Data[key]); got != want {
			t.Errorf("event data %s = %q, want %q", key, got, want)
		}
	}
}

func TestService_OutcomeLogsCarryTheTicketAndNoToken(t *testing.T) {
	logs := captureLogs(t)
	claimed := make(chan spi.ScheduledTask, 2)
	h := newHarness(t, testConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		claimed <- task
		if task.ID == "task-safe" {
			return workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined"}}
		}
		return workflow.RunReport{Outcome: workflow.OutcomeFailed, MarkHeld: true, UnsafeReached: true,
			Err: errors.New("failed to read entity: pq: host=db.internal user=cyoda")}
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-safe"), dueTask("t2", "task-unsafe")} })
	h.start(t)
	tasks := []spi.ScheduledTask{receive(t, claimed), receive(t, claimed)}
	eventually(t, "both outcome log lines", func() bool {
		out := logs.String()
		return strings.Contains(out, "the task waits for its next attempt") && strings.Contains(out, "scheduled task FAILED")
	})

	out := logs.String()
	for _, task := range tasks {
		for _, token := range []uuid.UUID{task.ArmToken, task.Claim.Token} {
			if strings.Contains(out, token.String()) {
				t.Errorf("a log line carries a token of %s", task.ID)
			}
		}
	}
	fail := h.fs.failsRecorded()[0]
	if strings.Contains(fail.Error, "pq:") {
		t.Errorf("lastError leaks the store error: %q", fail.Error)
	}
	ticket := strings.TrimSuffix(strings.TrimPrefix(fail.Error, "internal error [ticket: "), "]")
	if _, err := uuid.Parse(ticket); err != nil {
		t.Fatalf("lastError %q carries no ticket", fail.Error)
	}
	if !strings.Contains(out, `"ticket":"`+ticket+`"`) {
		t.Error("no ERROR line carries the ticket lastError shows")
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/scheduler/... -run 'TestService_OutcomeWrite|TestService_LockAndPool|TestService_StoreRejection|TestService_FailAndItsAudit|TestService_OutcomeLogs'`
Expected: FAIL —
- `OutcomeWriteRetriedThroughAnOutage`: timed out waiting for the attempt (one write, no retry);
- `LockAndPoolErrorsAreRetriedWithoutLatching`: timed out;
- `StoreRejectionLatchesAndKeepsTheClaim`: timed out waiting for the latch;
- `FailAndItsAuditEventCommitTogether`: `Fail ran outside a transaction` after the retry is added; before it, timed out;
- `OutcomeLogsCarryTheTicketAndNoToken`: timed out waiting for the log lines.

- [ ] **Step 3: Implement**

In `internal/scheduler/service.go`:

Add to `liveRun`, in the guarded block:
```go
	keep bool // never given back: its outcome was rejected, or it panicked
```

Add to `Service`, next to `runsWG`:
```go
	stopBooks chan struct{} // closed at the end of shutdown step 4
```
and in `New`: `stopBooks: make(chan struct{}),`.

In `Stop`, before `close(s.stopLoop)`: `close(s.stopBooks)`.

Add near the other constants:
```go
// warnEvery rate-limits the WARN line of a retried outcome write, per run.
const warnEvery = time.Minute

var errBookkeepingStopped = errors.New("the scheduler stopped before the outcome was recorded")
```

Replace `run`:
```go
func (s *Service) run(r *liveRun) {
	defer s.runsWG.Done()
	start := time.Now()
	ctx, span := observability.Tracer().Start(r.ctx, "scheduler.run")
	defer span.End()

	rep := s.fire(ctx, r)
	nowMs := s.deps.Clock.Now().UnixMilli()

	s.mu.Lock()
	r.ended = true
	reason, draining := r.reason, s.draining
	s.mu.Unlock()

	cut := rep.Outcome == workflow.OutcomeFailed && errors.Is(rep.Err, context.Canceled) &&
		(reason == shutdownCancelled || (reason == notCancelled && draining))
	var errText string
	errTicket := uuid.Nil
	if rep.Err != nil {
		text, ticket, warnOnly := recordedError(rep.Err)
		errText, errTicket = sanitiseErrorText(text), ticket
		if !warnOnly {
			slog.Error("scheduled run failed with an internal error", "pkg", "scheduler",
				"taskId", r.task.ID, "tenant", string(r.task.TenantID), "ticket", ticket.String(), "err", rep.Err)
		}
	}
	bk := decideBookkeeping(rep, r.task, cut, false, nowMs, s.cfg, errText)
	outcome := runOutcome(rep, bk, false, reason, cut)
	err := s.book(r, bk)
	s.logOutcome(r, bk, err, errTicket)
	if errors.Is(err, spi.ErrStaleClaim) {
		outcome = outcomeSuperseded
	}
	s.finish(r, bk, err)
	span.SetAttributes(attribute.String("outcome", outcome))
	s.m.runEnded(outcome, time.Since(start))
}

// logOutcome writes the §9 line once the outcome is accepted. ticket is the
// one lastError carries, or nil.
func (s *Service) logOutcome(r *liveRun, bk Bookkeeping, err error, ticket uuid.UUID) {
	switch {
	case errors.Is(err, spi.ErrStaleClaim):
		slog.Debug("scheduled run superseded", "pkg", "scheduler", "taskId", r.task.ID)
	case err != nil, bk.Kind == NoneKind:
	case bk.Kind == RecordAttemptKind:
		slog.Warn("scheduled run failed; the task waits for its next attempt", "pkg", "scheduler",
			"taskId", r.task.ID, "tenant", string(r.task.TenantID), "entityId", r.task.EntityID,
			"transition", r.task.Transition, "counted", !bk.Attempt.NotCounted,
			"nextAttemptTime", bk.Attempt.NextAttemptTime, "lastError", bk.Attempt.Error)
	case bk.Kind == FailKind:
		if ticket == uuid.Nil {
			ticket = uuid.New()
		}
		slog.Error("scheduled task FAILED", "pkg", "scheduler",
			"taskId", r.task.ID, "tenant", string(r.task.TenantID), "entityId", r.task.EntityID,
			"transition", r.task.Transition, "reason", string(bk.Failure.Reason),
			"ticket", ticket.String(), "lastError", bk.Failure.Error)
	}
}
```

Replace `book`:
```go
// book records the outcome with a fenced write that never inherits the run's
// cancellation. Every error is retried, after 1s and then doubling up to the
// heartbeat interval, until the write is accepted or refused, or until the
// shutdown deadline. Only a deterministic rejection by the store latches.
func (s *Service) book(r *liveRun, bk Bookkeeping) error {
	if bk.Kind == NoneKind {
		return nil
	}
	delay := min(time.Second, s.cfg.HeartbeatInterval)
	var warned time.Time
	for {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), storeCallBudget)
		err := s.writeOutcome(ctx, r, bk)
		cancel()
		switch {
		case err == nil, errors.Is(err, spi.ErrStaleClaim):
			return err
		case errors.Is(err, spi.ErrStoreRejected):
			ticket := uuid.New()
			slog.Error("the store rejected a scheduled run's outcome; node latched", "pkg", "scheduler",
				"taskId", r.task.ID, "tenant", string(r.task.TenantID), "ticket", ticket.String(), "err", err)
			s.mu.Lock()
			r.keep = true
			s.mu.Unlock()
			s.latch()
			return err
		}
		s.m.bookkeepingRetried()
		if time.Since(warned) >= warnEvery {
			warned = time.Now()
			slog.Warn("scheduled run outcome not recorded; retrying", "pkg", "scheduler",
				"taskId", r.task.ID, "tenant", string(r.task.TenantID), "err", err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-s.stopBooks:
			timer.Stop()
			return errBookkeepingStopped
		}
		delay = min(delay*2, s.cfg.HeartbeatInterval)
	}
}
```

Replace `writeOutcome` and add `failWithAudit`:
```go
func (s *Service) writeOutcome(ctx context.Context, r *liveRun, bk Bookkeeping) error {
	if bk.Kind == FailKind {
		return s.failWithAudit(ctx, r, bk.Failure)
	}
	return s.store.RecordAttempt(ctx, r.ref, bk.Attempt)
}

// failWithAudit writes Fail and the SCHEDULED_TRANSITION_FAIL event in one
// transaction (§5.7). The event's state is the entity's state in that
// transaction, or the task's source state when the entity is gone.
func (s *Service) failWithAudit(ctx context.Context, r *liveRun, f spi.Failure) error {
	ctx = spi.WithUserContext(ctx, common.SystemUserContextValue(r.task.TenantID))
	txID, txCtx, err := s.deps.TxManager.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin the transaction that fails a scheduled task: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rbCtx, cancel := common.RollbackContext(txCtx)
			_ = s.deps.TxManager.Rollback(rbCtx, txID)
			cancel()
		}
	}()
	if err := s.store.Fail(txCtx, r.ref, f); err != nil {
		return fmt.Errorf("failed to fail the scheduled task: %w", err)
	}
	state := r.task.SourceState
	entities, err := s.deps.Store.EntityStore(txCtx)
	if err != nil {
		return fmt.Errorf("failed to get the entity store: %w", err)
	}
	switch entity, err := entities.Get(txCtx, r.task.EntityID); {
	case err == nil:
		state = entity.Meta.State
	case !errors.Is(err, spi.ErrNotFound):
		return fmt.Errorf("failed to read the entity of a failed scheduled task: %w", err)
	}
	audit, err := s.deps.Store.StateMachineAuditStore(txCtx)
	if err != nil {
		return fmt.Errorf("failed to get the audit store: %w", err)
	}
	if err := audit.Record(txCtx, r.task.EntityID, spi.StateMachineEvent{
		EventType:     spi.SMEventScheduledTransitionFailed,
		EntityID:      r.task.EntityID,
		State:         state,
		TransactionID: txID,
		Details:       fmt.Sprintf("Scheduled transition %q failed: %s", r.task.Transition, f.Reason),
		Data: map[string]any{
			"transition":  r.task.Transition,
			"sourceState": r.task.SourceState,
			"reason":      string(f.Reason),
			"attempts":    r.task.Attempts,
			"lostOwners":  r.task.LostOwners,
		},
		Timestamp: time.UnixMilli(f.AtMs),
	}); err != nil {
		return fmt.Errorf("failed to record the scheduled task's failure: %w", err)
	}
	if err := common.ShieldedCommit(txCtx, func(c context.Context) error {
		return s.deps.TxManager.Commit(c, txID)
	}); err != nil {
		return fmt.Errorf("failed to commit the scheduled task's failure: %w", err)
	}
	committed = true
	return nil
}
```

Replace `finish`:
```go
// finish releases the claim once its outcome is accepted or refused. A claim
// whose outcome is not recorded stays in the set until shutdown step 5; a kept
// claim stays for good.
func (s *Service) finish(r *liveRun, bk Bookkeeping, bookErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recorded := bk.Kind == NoneKind || bookErr == nil || errors.Is(bookErr, spi.ErrStaleClaim)
	if !recorded || r.keep {
		return
	}
	s.releaseLocked(r)
	if s.filled {
		s.filled = false
		select {
		case s.slotFreed <- struct{}{}:
		default:
		}
	}
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/scheduler/...`
Expected: `ok`.

- [ ] **Step 5: Commit**

```
git add internal/scheduler/service.go internal/scheduler/outcome_test.go
git commit -m "feat(scheduler): retry an outcome write by default; FAILED with its audit event in one transaction

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task R-8: Panics — the run, its bookkeeping, and the latch

**Spec:** §6.5 (all). §13 U rows: "panicking run → FAILED `RUN_PANICKED`, node
latched, claims stop"; "a watchdog panic: the latch cancels the runs".

**Files:**
- Modify: `internal/scheduler/service.go` (`fire`, `run`, `latch`)
- Test: `internal/scheduler/panic_test.go`

**Interfaces:**
- Consumes: R-6, R-7.
- Produces: `fire` returns `(workflow.RunReport, panicked bool, ticket uuid.UUID)`; `lastError` of a panicked run is `internal error [ticket: <uuid>]` with the ticket of the ERROR line.

- [ ] **Step 1: Write the failing tests**

`internal/scheduler/panic_test.go`:

```go
package scheduler

import (
	"context"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

var ticketText = regexp.MustCompile(`^internal error \[ticket: [0-9a-f-]{36}\]$`)

func TestService_PanickingRunFailsItsTaskAndLatches(t *testing.T) {
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, testConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		tokens <- task.Claim.Token
		panic("injected panic in a scheduled run")
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	token := receive(t, tokens)

	eventually(t, "the task failed", func() bool { return len(h.fs.failsRecorded()) == 1 })
	f := h.fs.failsRecorded()[0]
	if f.Reason != spi.FailureRunPanicked || !ticketText.MatchString(f.Error) {
		t.Errorf("failure = %+v, want RUN_PANICKED with a ticket only", f)
	}
	if h.flag.Load() {
		t.Error("a panicking run did not latch the node")
	}
	claims := h.fs.claims()
	eventually(t, "a give-back that keeps the panicked run's claim", func() bool {
		keep, ok := h.fs.lastGiveBack()
		return ok && slices.Contains(keep, token)
	})
	if n := h.fs.claims(); n != claims {
		t.Errorf("%d claims after the latch", n-claims)
	}
	beats := h.fs.heartbeatCount()
	eventually(t, "heartbeats go on after the latch", func() bool { return h.fs.heartbeatCount() > beats })
}

func TestService_PanicWhileRecordingTheOutcomeLatchesAndKeepsTheClaim(t *testing.T) {
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, testConfig(), firerFunc(func(_ context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		tokens <- task.Claim.Token
		return workflow.RunReport{Outcome: workflow.OutcomeFailed, Err: context.DeadlineExceeded}
	}))
	h.fs.with(func() {
		h.fs.outcomePanic = true
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	token := receive(t, tokens)
	eventually(t, "the node latched", func() bool { return !h.flag.Load() })
	eventually(t, "a give-back that keeps the claim", func() bool {
		keep, ok := h.fs.lastGiveBack()
		return ok && slices.Contains(keep, token)
	})
}

func TestService_TheLatchCancelsEveryRunInProgress(t *testing.T) {
	blocked := make(chan struct{})
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		if task.ID == "task-block" {
			close(blocked)
			return failedOnCancel(ctx)
		}
		<-blocked
		panic("injected panic in a scheduled run")
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-block"), dueTask("t2", "task-boom")} })
	h.start(t)
	eventually(t, "the blocked run's attempt", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	if a := h.fs.attemptsRecorded()[0]; a.NotCounted || a.Error != cancelledText {
		t.Errorf("attempt = %+v, want a counted attempt cancelled by the latch", a)
	}
}

func TestService_APanicInTheLoopHeartbeatOrWatchdogLatchesAndCancels(t *testing.T) {
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		return failedOnCancel(ctx)
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer h.svc.recoverLatch("watchdog")
		panic("injected panic in the watchdog")
	}()
	receive(t, done)
	if h.flag.Load() {
		t.Error("the recovered panic did not latch the node")
	}
	eventually(t, "the run cancelled by the latch", func() bool { return len(h.fs.attemptsRecorded()) == 1 })
	claims := h.fs.claims()
	time.Sleep(50 * time.Millisecond)
	if n := h.fs.claims(); n != claims {
		t.Errorf("%d claims after the latch", n-claims)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/scheduler/... -run 'TestService_Panic|TestService_TheLatch|TestService_APanic'`
Expected: FAIL — `TestService_PanickingRunFailsItsTaskAndLatches` crashes the test binary with `panic: injected panic in a scheduled run`. Run the other three alone
(`-run 'TestService_PanicWhile|TestService_TheLatch|TestService_APanic'`): each times out, because nothing cancels the runs and a bookkeeping panic is not recovered.

- [ ] **Step 3: Implement**

In `internal/scheduler/service.go`:

Replace `fire`:
```go
// fire runs the task. A panic is recovered here: it is logged with a ticket,
// the claim is kept for good, and the node latches.
func (s *Service) fire(ctx context.Context, r *liveRun) (rep workflow.RunReport, panicked bool, ticket uuid.UUID) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		panicked, ticket = true, uuid.New()
		slog.Error("scheduled run panicked; node latched", "pkg", "scheduler",
			"taskId", r.task.ID, "tenant", string(r.task.TenantID), "ticket", ticket.String(),
			"err", fmt.Errorf("panic: %v", v), "stack", string(debug.Stack()))
		s.mu.Lock()
		r.keep = true
		s.mu.Unlock()
		s.latch()
	}()
	ctx = spi.WithUserContext(ctx, common.SystemUserContextValue(r.task.TenantID))
	ctx = workflow.WithRunGuard(ctx, &workflow.RunGuard{
		Ref: r.ref, Store: s.store, Done: r.ctx.Done(), NoNewUnsafe: s.drainingCh, Unsafe: r.unsafe,
	})
	return s.deps.Firer.FireScheduledTransition(ctx, r.task, s.cfg.MaxLostOwners, s.cfg.RetryDelay), false, uuid.Nil
}
```

Replace `run` (the R-7 body, with the panic path and a recovery of its own):
```go
func (s *Service) run(r *liveRun) {
	defer s.runsWG.Done()
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		ticket := uuid.New()
		slog.Error("scheduled run bookkeeping panicked; node latched", "pkg", "scheduler",
			"taskId", r.task.ID, "tenant", string(r.task.TenantID), "ticket", ticket.String(),
			"err", fmt.Errorf("panic: %v", v), "stack", string(debug.Stack()))
		s.mu.Lock()
		r.keep, r.ended = true, true
		s.mu.Unlock()
		s.latch()
	}()
	start := time.Now()
	ctx, span := observability.Tracer().Start(r.ctx, "scheduler.run")
	defer span.End()

	rep, panicked, panicTicket := s.fire(ctx, r)
	nowMs := s.deps.Clock.Now().UnixMilli()

	s.mu.Lock()
	r.ended = true
	reason, draining := r.reason, s.draining
	s.mu.Unlock()

	cut := !panicked && rep.Outcome == workflow.OutcomeFailed && errors.Is(rep.Err, context.Canceled) &&
		(reason == shutdownCancelled || (reason == notCancelled && draining))
	var errText string
	errTicket := uuid.Nil
	switch {
	case panicked:
		errText, errTicket = internalErrorText(panicTicket), panicTicket
	case rep.Err != nil:
		text, ticket, warnOnly := recordedError(rep.Err)
		errText, errTicket = sanitiseErrorText(text), ticket
		if !warnOnly {
			slog.Error("scheduled run failed with an internal error", "pkg", "scheduler",
				"taskId", r.task.ID, "tenant", string(r.task.TenantID), "ticket", ticket.String(), "err", rep.Err)
		}
	}
	bk := decideBookkeeping(rep, r.task, cut, panicked, nowMs, s.cfg, errText)
	outcome := runOutcome(rep, bk, panicked, reason, cut)
	err := s.book(r, bk)
	s.logOutcome(r, bk, err, errTicket)
	if errors.Is(err, spi.ErrStaleClaim) {
		outcome = outcomeSuperseded
	}
	s.finish(r, bk, err)
	span.SetAttributes(attribute.String("outcome", outcome))
	s.m.runEnded(outcome, time.Since(start))
}
```

Replace `latch`:
```go
// latch marks the node unhealthy for good and cancels every run in progress.
// A latched node claims nothing; it keeps heartbeating unless the heartbeat
// itself panicked, so its runs are not taken over while they stop.
func (s *Service) latch() {
	if s.deps.HealthFlag != nil {
		s.deps.HealthFlag.Store(false)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latched = true
	for _, r := range s.runs {
		if r.ended {
			continue
		}
		if r.reason == notCancelled {
			r.reason = latchCancelled
		}
		r.cancel()
	}
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/scheduler/...`
Expected: `ok`.

- [ ] **Step 5: Commit**

```
git add internal/scheduler/service.go internal/scheduler/panic_test.go
git commit -m "feat(scheduler): a panicking run fails its task, keeps its claim and latches the node

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task R-9: Shutdown — `Drain`, steps 1 to 5

**Spec:** §6.4 steps 1-5, §5.3 (the exemption at step 3), §5.6 ("until the
shutdown deadline of §6.4"), §6.5 (a panicked run is never given back). §13 U
rows: "runs finish within the drain; streams stay open" (the U half); "run cut
after the drain, nothing handed off → WAITING, uncounted"; "an unsafe callout
in flight at shutdown is not cut; the run continues with safe steps and
commits"; "after the signal, a run reaching a new unsafe dispatch counts as
cut"; "a run with no unsafe callout in flight, cut after the drain, whose
unsafe work was handed off earlier → FAILED"; "a run still live after step 4
is not given back"; "bookkeeping stops at its shutdown deadline".

**Files:**
- Modify: `internal/scheduler/service.go` (`Service`, `New`, `Stop`; new `Drain`, `drain`, `waitRuns`, `cutRuns`, `stepFourBound`, `finalKeep`)
- Modify: `internal/scheduler/fakes_test.go` (`newHarness` shortens the step-4 margin)
- Test: `internal/scheduler/drain_test.go`

**Interfaces:**
- Consumes: E `workflow.UnsafeFlight.Since()`; `Deps.CalloutDeadlineMax`.
- Produces: `func (s *Service) Drain(ctx context.Context)` (steps 1-5; only the first call does anything; `ctx` ends the waits of steps 2 and 4 early); `func (s *Service) Stop()` = `Drain(context.Background())`.

- [ ] **Step 1: Write the failing tests**

In `internal/scheduler/fakes_test.go`, in `newHarness`, after `svc.window = time.Minute`:
```go
	// Step 4 waits CommitBudget + 15s past the longest callout in production.
	svc.stepFourMargin = 100 * time.Millisecond
```

`internal/scheduler/drain_test.go`:

```go
package scheduler

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
)

func drainConfig() Config {
	cfg := testConfig()
	cfg.ShutdownDrain = 20 * time.Millisecond
	return cfg
}

func TestDrain_RunsThatFinishWithinTheDrainAreNotCut(t *testing.T) {
	cfg := testConfig()
	cfg.ShutdownDrain = 2 * time.Second
	started := make(chan struct{})
	var cut atomic.Bool
	h := newHarness(t, cfg, firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		close(started)
		time.Sleep(100 * time.Millisecond)
		cut.Store(ctx.Err() != nil)
		return fired
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	receive(t, started)

	h.svc.Drain(context.Background())
	if cut.Load() {
		t.Error("a run that finished within CYODA_SCHEDULER_SHUTDOWN_DRAIN was cancelled")
	}
	h.fs.with(func() {
		if h.fs.retired != 1 {
			t.Errorf("RetireOwner calls = %d, want 1", h.fs.retired)
		}
	})
	beats := h.fs.heartbeatCount()
	time.Sleep(50 * time.Millisecond)
	if n := h.fs.heartbeatCount(); n != beats {
		t.Errorf("%d heartbeats after Drain returned", n-beats)
	}
}

func TestDrain_ACutRunWithNothingHandedOffGoesBackUncounted(t *testing.T) {
	h := newHarness(t, drainConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		return failedOnCancel(ctx)
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })

	before := time.Now().UnixMilli()
	h.svc.Drain(context.Background())
	attempts := h.fs.attemptsRecorded()
	if len(attempts) != 1 {
		t.Fatalf("%d attempts, want 1", len(attempts))
	}
	a := attempts[0]
	if !a.NotCounted || !a.ClearOwnMark || a.NextAttemptTime < before || a.NextAttemptTime > time.Now().UnixMilli() {
		t.Errorf("attempt = %+v, want uncounted, mark cleared, due now", a)
	}
}

func TestDrain_AnUnsafeCalloutInFlightIsNotCut(t *testing.T) {
	var cut atomic.Bool
	h := newHarness(t, drainConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		g := workflow.RunGuardFrom(ctx)
		g.Unsafe.Begin()
		<-g.Draining
		time.Sleep(100 * time.Millisecond) // past step 2's 20ms: step 3 has run
		cut.Store(ctx.Err() != nil)
		g.Unsafe.End()
		return fired // the run went on with safe steps and committed
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })

	h.svc.Drain(context.Background())
	if cut.Load() {
		t.Error("step 3 cancelled a run whose unsafe callout was in flight")
	}
	if a, f := len(h.fs.attemptsRecorded()), len(h.fs.failsRecorded()); a+f != 0 {
		t.Errorf("a committed run wrote %d attempts and %d failures", a, f)
	}
}

func TestDrain_ReachingANewUnsafeDispatchAfterTheSignalCountsAsCut(t *testing.T) {
	h := newHarness(t, testConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		<-workflow.RunGuardFrom(ctx).Draining
		return workflow.RunReport{Outcome: workflow.OutcomeFailed,
			Err: fmt.Errorf("unsafe dispatch refused after shutdown began: %w", context.Canceled)}
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })

	h.svc.Drain(context.Background())
	attempts := h.fs.attemptsRecorded()
	if len(attempts) != 1 || !attempts[0].NotCounted {
		t.Errorf("attempts = %+v, want one uncounted attempt", attempts)
	}
}

func TestDrain_ACutRunWhoseUnsafeWorkWasHandedOffEarlierFails(t *testing.T) {
	h := newHarness(t, drainConfig(), firerFunc(func(ctx context.Context, _ spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		<-ctx.Done()
		return workflow.RunReport{Outcome: workflow.OutcomeFailed, MarkHeld: true, UnsafeReached: true,
			Err: fmt.Errorf("run stopped: %w", ctx.Err())}
	}))
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	eventually(t, "the run started", func() bool { return liveRuns(h.svc) == 1 })

	h.svc.Drain(context.Background())
	fails := h.fs.failsRecorded()
	if len(fails) != 1 || fails[0].Reason != spi.FailureUnsafeWorkNotCompleted {
		t.Errorf("failures = %+v, want UNSAFE_WORK_NOT_COMPLETED", fails)
	}
}

func TestDrain_ARunStillLiveAfterStepFourIsNotGivenBack(t *testing.T) {
	release := make(chan struct{})
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, drainConfig(), firerFunc(func(ctx context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		g := workflow.RunGuardFrom(ctx)
		g.Unsafe.Begin()
		tokens <- task.Claim.Token
		<-release
		g.Unsafe.End()
		return fired
	}))
	h.svc.deps.CalloutDeadlineMax = 50 * time.Millisecond
	h.fs.with(func() { h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")} })
	h.start(t)
	t.Cleanup(func() { close(release) })
	token := receive(t, tokens)

	h.svc.Drain(context.Background())
	keep, ok := h.fs.lastGiveBack()
	if !ok || !slices.Contains(keep, token) {
		t.Errorf("the final give-back kept %v; a live run must not be given back", keep)
	}
	h.fs.with(func() {
		if h.fs.retired != 0 {
			t.Error("RetireOwner ran while a run was still live")
		}
	})
}

func TestDrain_BookkeepingStopsAtItsShutdownDeadline(t *testing.T) {
	tokens := make(chan uuid.UUID, 1)
	h := newHarness(t, drainConfig(), firerFunc(func(ctx context.Context, task spi.ScheduledTask, _ int, _ time.Duration) workflow.RunReport {
		tokens <- task.Claim.Token
		return failedOnCancel(ctx)
	}))
	h.fs.with(func() {
		h.fs.outcomeAlways = fmt.Errorf("record attempt: %w", context.DeadlineExceeded)
		h.fs.due = []spi.ScheduledTask{dueTask("t1", "task-1")}
	})
	h.start(t)
	token := receive(t, tokens)

	done := make(chan struct{})
	go func() { h.svc.Drain(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return while an outcome write kept failing")
	}
	keep, ok := h.fs.lastGiveBack()
	if !ok || slices.Contains(keep, token) {
		t.Errorf("the final give-back kept %v; a run that ended without its outcome is given back", keep)
	}
	h.fs.with(func() {
		if h.fs.retired != 1 {
			t.Errorf("RetireOwner calls = %d, want 1", h.fs.retired)
		}
	})
}

func TestDrain_IsIdempotentAndSafeBeforeStart(t *testing.T) {
	h := newHarness(t, testConfig(), reportFirer(fired))
	h.svc.Drain(context.Background())
	h.svc.Stop()

	h2 := newHarness(t, testConfig(), reportFirer(fired))
	h2.start(t)
	h2.svc.Drain(context.Background())
	h2.svc.Drain(context.Background())
	h2.svc.Stop()
	h2.fs.with(func() {
		if h2.fs.retired != 1 {
			t.Errorf("RetireOwner calls = %d, want 1", h2.fs.retired)
		}
	})
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/scheduler/... -run TestDrain_`
Expected: build failure — `h.svc.Drain undefined`, `svc.stepFourMargin undefined`.

- [ ] **Step 3: Implement**

In `internal/scheduler/service.go`:

Add to `Service`, next to `sweepEvery`:
```go
	stepFourMargin time.Duration // CommitBudget + 15s
```
and replace `stopOnce sync.Once` with `drainOnce sync.Once`.

In `New`: `stepFourMargin: common.CommitBudget + shutdownTail,`, and add the constant:
```go
// shutdownTail is the part of the step-4 bound after the commit budget.
const shutdownTail = 15 * time.Second
```

Replace `Stop` and add the drain:
```go
// Stop drains with no outside deadline. App.Shutdown calls it after the
// servers stop, which is the path when a server failed; after a Drain it does
// nothing.
func (s *Service) Stop() { s.Drain(context.Background()) }

// Drain runs shutdown steps 1-5 (spec §6.4). ctx ends the waits of steps 2
// and 4 early. Only the first call does anything.
func (s *Service) Drain(ctx context.Context) {
	s.drainOnce.Do(func() { s.drain(ctx) })
}

func (s *Service) drain(ctx context.Context) {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	// Step 1: stop claiming. From here no run starts a new unsafe dispatch.
	s.draining = true
	close(s.drainingCh)
	s.mu.Unlock()
	close(s.stopLoop)
	<-s.loopDone

	// Step 2: wait for the runs while the streams and callback routes are open.
	if !s.waitRuns(ctx, s.cfg.ShutdownDrain) {
		// Step 3: cancel every run without an unsafe callout in flight.
		s.cutRuns()
		// Step 4: wait for every run to record its outcome.
		s.waitRuns(ctx, s.stepFourBound())
	}
	close(s.stopBooks)

	// Step 5: give back the claims whose run ended without a recorded outcome,
	// stop the heartbeat, and retire the owner if nothing still holds a claim.
	keep := s.finalKeep()
	gctx, cancel := context.WithTimeout(context.Background(), storeCallBudget)
	if n, err := s.store.GiveBackIdle(gctx, s.incarnation, keep); err != nil {
		slog.Warn("scheduler could not give back its claims at shutdown", "pkg", "scheduler", "err", err)
	} else if n > 0 {
		slog.Info("scheduler gave back claims at shutdown", "pkg", "scheduler", "count", n)
	}
	cancel()
	close(s.stopHB)
	<-s.hbDone
	<-s.wdDone
	if len(keep) > 0 {
		slog.Warn("scheduler stopped with runs still holding their tasks; another node takes them over after CYODA_SCHEDULER_STALE_AFTER",
			"pkg", "scheduler", "runs", len(keep))
		return
	}
	rctx, cancel := context.WithTimeout(context.Background(), storeCallBudget)
	defer cancel()
	if err := s.store.RetireOwner(rctx, s.incarnation); err != nil {
		slog.Warn("scheduler could not retire its liveness record", "pkg", "scheduler", "err", err)
	}
}

// waitRuns waits up to d for every run goroutine to end, bookkeeping included.
// No run is registered after step 1, so the WaitGroup only counts down.
func (s *Service) waitRuns(ctx context.Context, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.runsWG.Wait()
		close(done)
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// cutRuns is step 3: a run whose unsafe callout is in flight is left alone.
func (s *Service) cutRuns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if r.ended {
			continue
		}
		if _, inFlight := r.unsafe.Since(); inFlight {
			continue
		}
		if r.reason == notCancelled {
			r.reason = shutdownCancelled
		}
		r.cancel()
	}
}

// stepFourBound is the longest remaining callout deadline + CommitBudget + 15s.
func (s *Service) stepFourBound() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	var longest time.Duration
	for _, r := range s.runs {
		if r.ended {
			continue
		}
		if since, inFlight := r.unsafe.Since(); inFlight {
			longest = max(longest, s.deps.CalloutDeadlineMax-time.Since(since))
		}
	}
	return longest + s.stepFourMargin
}

// finalKeep releases every run that ended without a recorded outcome and
// returns the claims that must stay: runs still live, and kept runs.
func (s *Service) finalKeep() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := make([]uuid.UUID, 0, len(s.runs))
	for token, r := range s.runs {
		if !r.ended || r.keep {
			keep = append(keep, token)
			continue
		}
		s.releaseLocked(r)
	}
	return keep
}
```

Remove `close(s.stopBooks)` from the old `Stop` body (the body is gone).

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/scheduler/...`
Expected: `ok` (every earlier test now ends through `Drain`).

Then once under the race detector for this package only:
`go test -race ./internal/scheduler/...` — `ok`.

- [ ] **Step 5: Commit**

```
git add internal/scheduler/service.go internal/scheduler/fakes_test.go internal/scheduler/drain_test.go
git commit -m "feat(scheduler): shutdown drain — stop claiming, wait, cut, record, give back, retire

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task R-10: Settings, app wiring, and the removal of the scheduler RPC

**Spec:** §11 (core rows; `CYODA_POSTGRES_SCHEDULER_CONNS` is BP's), §6.6,
§6.4 (last paragraph), §15. §13 Configuration rows: "each new variable: its
default and its validation failure"; "each removed variable is no longer
read". Also §13 "`STALE_AFTER` validation" (the setting half).

**Files:**
- Modify: `app/config.go` (`SchedulerConfig` `:153-182`, `DefaultConfig` `:468, :474-482`, `Validate` `:780-803`, `ValidateDispatch` `:933-950`; new `ValidateScheduler`)
- Modify: `internal/cluster/config.go` (`DispatchForwardTimeout` `:25-27`)
- Modify: `app/app.go` (`:28` import stays; `:75`; `:88-95` comment; `:577-586`; delete `:600-657`; `:810-827`; `:1047-1069`; new `DrainScheduler`, `schedulerCalloutDeadlineMax`)
- Modify: `cmd/cyoda/main.go` (`:118-121`: add `ValidateScheduler`)
- Modify: `cmd/cyoda/help/config_registry.go` (`:80`, `:131-137`)
- Modify: `cmd/cyoda/help/content/config/scheduler.md`, `cmd/cyoda/help/content/config/cluster.md` (`:32`), `cmd/cyoda/help/content/config.md` (`:33`)
- Modify: `README.md` (`:224-236`, `:251`)
- Delete: `internal/cluster/scheduler_rpc.go`, `internal/cluster/scheduler_rpc_test.go`
- Modify: `internal/cluster/dispatch/encode.go` (`:25-29`), create `internal/cluster/dispatch/export_test.go`
- Modify: `internal/cluster/peeraddr/peeraddr.go` (`:45` comment), `internal/cluster/dispatch/aead_peer_auth.go` (`:45` comment)
- Modify: `internal/domain/search/reaper.go` (`:15-21` comment)
- Test: `app/config_test.go` (`:245-345`), `app/config_dispatch_test.go` (forward-timeout rows only), `app/config_registry_binding_test.go` (`:122`, `:170-177`), new `app/config_scheduler_test.go`

**Interfaces:**
- Consumes: R-2 `scheduler.MinStaleAfter`; R-4 `scheduler.Config`; R-6..R-9 `scheduler.New`, `Deps`, `Start`, `Drain`, `Stop`; E `*workflow.Engine` satisfies `scheduler.Firer`.
- Produces:
  - `app.SchedulerConfig{Enabled, ScanInterval, MaxRuns, MaxRunsPerTenant, HeartbeatInterval, StaleAfter, MaxLostOwners, RetryDelay, RetryDelayMax, ShutdownDrain}` — same order as `scheduler.Config`, converted with `scheduler.Config(cfg.Scheduler)`
  - `func app.ValidateScheduler(c SchedulerConfig) error`
  - `func (a *App) DrainScheduler(ctx context.Context)`
  - `func schedulerCalloutDeadlineMax(c Config) time.Duration` (unexported)

- [ ] **Step 1: Write the failing tests**

`app/config_scheduler_test.go`:

```go
package app

import (
	"strings"
	"testing"
	"time"
)

var schedulerVars = []string{
	"CYODA_SCHEDULER_ENABLED", "CYODA_SCHEDULER_SCAN_INTERVAL", "CYODA_SCHEDULER_MAX_RUNS",
	"CYODA_SCHEDULER_MAX_RUNS_PER_TENANT", "CYODA_SCHEDULER_HEARTBEAT_INTERVAL", "CYODA_SCHEDULER_STALE_AFTER",
	"CYODA_SCHEDULER_MAX_LOST_OWNERS", "CYODA_SCHEDULER_RETRY_DELAY", "CYODA_SCHEDULER_RETRY_DELAY_MAX",
	"CYODA_SCHEDULER_SHUTDOWN_DRAIN",
}

func defaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{
		Enabled: true, ScanInterval: time.Second, MaxRuns: 8, MaxRunsPerTenant: 4,
		HeartbeatInterval: 15 * time.Second, StaleAfter: 2 * time.Minute, MaxLostOwners: 3,
		RetryDelay: 30 * time.Second, RetryDelayMax: 15 * time.Minute, ShutdownDrain: 20 * time.Second,
	}
}

func TestDefaultConfig_Scheduler(t *testing.T) {
	unsetEnv(t, schedulerVars...)
	if got, want := DefaultConfig().Scheduler, defaultSchedulerConfig(); got != want {
		t.Errorf("Scheduler defaults\n got %+v\nwant %+v", got, want)
	}
}

func TestDefaultConfig_SchedulerEnvOverrides(t *testing.T) {
	for name, value := range map[string]string{
		"CYODA_SCHEDULER_ENABLED": "false", "CYODA_SCHEDULER_SCAN_INTERVAL": "2s",
		"CYODA_SCHEDULER_MAX_RUNS": "16", "CYODA_SCHEDULER_MAX_RUNS_PER_TENANT": "5",
		"CYODA_SCHEDULER_HEARTBEAT_INTERVAL": "5s", "CYODA_SCHEDULER_STALE_AFTER": "3m",
		"CYODA_SCHEDULER_MAX_LOST_OWNERS": "2", "CYODA_SCHEDULER_RETRY_DELAY": "1s",
		"CYODA_SCHEDULER_RETRY_DELAY_MAX": "1m", "CYODA_SCHEDULER_SHUTDOWN_DRAIN": "0s",
	} {
		t.Setenv(name, value)
	}
	want := SchedulerConfig{
		Enabled: false, ScanInterval: 2 * time.Second, MaxRuns: 16, MaxRunsPerTenant: 5,
		HeartbeatInterval: 5 * time.Second, StaleAfter: 3 * time.Minute, MaxLostOwners: 2,
		RetryDelay: time.Second, RetryDelayMax: time.Minute, ShutdownDrain: 0,
	}
	if got := DefaultConfig().Scheduler; got != want {
		t.Errorf("Scheduler overrides\n got %+v\nwant %+v", got, want)
	}
}

func TestValidateScheduler(t *testing.T) {
	if err := ValidateScheduler(defaultSchedulerConfig()); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	atMin := defaultSchedulerConfig()
	atMin.StaleAfter = 95 * time.Second // 50s + 3 x 15s
	if err := ValidateScheduler(atMin); err != nil {
		t.Fatalf("STALE_AFTER at its minimum rejected: %v", err)
	}
	noDrain := defaultSchedulerConfig()
	noDrain.ShutdownDrain = 0
	if err := ValidateScheduler(noDrain); err != nil {
		t.Fatalf("a zero shutdown drain rejected: %v", err)
	}

	cases := []struct {
		name     string
		mutate   func(*SchedulerConfig)
		wantName string
	}{
		{"zero scan interval", func(c *SchedulerConfig) { c.ScanInterval = 0 }, "CYODA_SCHEDULER_SCAN_INTERVAL"},
		{"zero max runs", func(c *SchedulerConfig) { c.MaxRuns = 0 }, "CYODA_SCHEDULER_MAX_RUNS"},
		{"zero per-tenant runs", func(c *SchedulerConfig) { c.MaxRunsPerTenant = 0 }, "CYODA_SCHEDULER_MAX_RUNS_PER_TENANT"},
		{"per-tenant runs above max runs", func(c *SchedulerConfig) { c.MaxRunsPerTenant = 9 }, "CYODA_SCHEDULER_MAX_RUNS_PER_TENANT"},
		{"zero heartbeat interval", func(c *SchedulerConfig) { c.HeartbeatInterval = 0 }, "CYODA_SCHEDULER_HEARTBEAT_INTERVAL"},
		{"stale-after one second below its minimum", func(c *SchedulerConfig) { c.StaleAfter = 94 * time.Second }, "CYODA_SCHEDULER_STALE_AFTER"},
		{"zero max lost owners", func(c *SchedulerConfig) { c.MaxLostOwners = 0 }, "CYODA_SCHEDULER_MAX_LOST_OWNERS"},
		{"zero retry delay", func(c *SchedulerConfig) { c.RetryDelay = 0 }, "CYODA_SCHEDULER_RETRY_DELAY"},
		{"retry delay max below retry delay", func(c *SchedulerConfig) { c.RetryDelayMax = 29 * time.Second }, "CYODA_SCHEDULER_RETRY_DELAY_MAX"},
		{"negative shutdown drain", func(c *SchedulerConfig) { c.ShutdownDrain = -time.Second }, "CYODA_SCHEDULER_SHUTDOWN_DRAIN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := defaultSchedulerConfig()
			tc.mutate(&c)
			err := ValidateScheduler(c)
			if err == nil {
				t.Fatal("ValidateScheduler = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantName+" must") {
				t.Errorf("error must name %s; got: %v", tc.wantName, err)
			}
		})
	}
}

func TestConfigValidate_RejectsAnInvalidSchedulerSetting(t *testing.T) {
	c := DefaultConfig()
	c.Scheduler.MaxRuns = 0
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "CYODA_SCHEDULER_MAX_RUNS") {
		t.Errorf("Config.Validate = %v, want the scheduler setting named", err)
	}
}

func TestSchedulerCalloutDeadlineMax_AtTheDefaults(t *testing.T) {
	unsetEnv(t, "CYODA_RETRY_FIXED_NUM_RETRIES", "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS",
		"CYODA_DISPATCH_WAIT_TIMEOUT", "CYODA_CALLOUT_HANDOVER_ALLOWANCE")
	// (1 + 3) x 60s + 5s + 30s
	if got := schedulerCalloutDeadlineMax(DefaultConfig()); got != 275*time.Second {
		t.Errorf("schedulerCalloutDeadlineMax = %s, want 275s", got)
	}
}
```

Append to `app/config_registry_binding_test.go`:

```go
// TestRootConfigVars_RemovedSchedulerSettingsAreGone pins the settings the
// claim-based scheduler removed; the §15 exit grep proves no source reads them.
func TestRootConfigVars_RemovedSchedulerSettingsAreGone(t *testing.T) {
	removed := []string{
		"CYODA_SCHEDULER_DISTRIBUTION", "CYODA_SCHEDULER_COORDINATOR", "CYODA_SCHEDULER_REDISPATCH_BACKOFF",
		"CYODA_SCHEDULER_BATCH_SIZE", "CYODA_SCHEDULER_EXPIRY_GRACE", "CYODA_DISPATCH_FORWARD_TIMEOUT",
	}
	for _, v := range help.RootConfigVars() {
		if slices.Contains(removed, v.Name) {
			t.Errorf("%s is still registered", v.Name)
		}
	}
}
```
(add `"slices"` to its imports).

In the same file's `defaultFor`, replace the scheduler block (`:170-177`) with:
```go
		// --- scheduler ---
		"CYODA_SCHEDULER_ENABLED":             strconv.FormatBool(c.Scheduler.Enabled),
		"CYODA_SCHEDULER_SCAN_INTERVAL":       renderDuration(c.Scheduler.ScanInterval),
		"CYODA_SCHEDULER_MAX_RUNS":            strconv.Itoa(c.Scheduler.MaxRuns),
		"CYODA_SCHEDULER_MAX_RUNS_PER_TENANT": strconv.Itoa(c.Scheduler.MaxRunsPerTenant),
		"CYODA_SCHEDULER_HEARTBEAT_INTERVAL":  renderDuration(c.Scheduler.HeartbeatInterval),
		"CYODA_SCHEDULER_STALE_AFTER":         renderDuration(c.Scheduler.StaleAfter),
		"CYODA_SCHEDULER_MAX_LOST_OWNERS":     strconv.Itoa(c.Scheduler.MaxLostOwners),
		"CYODA_SCHEDULER_RETRY_DELAY":         renderDuration(c.Scheduler.RetryDelay),
		"CYODA_SCHEDULER_RETRY_DELAY_MAX":     renderDuration(c.Scheduler.RetryDelayMax),
		"CYODA_SCHEDULER_SHUTDOWN_DRAIN":      renderDuration(c.Scheduler.ShutdownDrain),
```
and delete the `"CYODA_DISPATCH_FORWARD_TIMEOUT"` row (`:122`).

In `app/config_test.go`, delete `TestDefaultConfig_Scheduler` (`:245-289`) and `TestDefaultConfig_SchedulerEnvOverrides` (`:315-345`); the new file replaces them.

In `app/config_dispatch_test.go`: remove `DispatchForwardTimeout` from `validDispatchConfig` (`:15`), the forward-timeout default check (`:28-30`) and `"CYODA_DISPATCH_FORWARD_TIMEOUT"` from `unsetEnv` (`:20`), and the two forward-timeout cases (`:60-61`). The WAIT and CONNECT cases stay.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./app/... -run 'TestDefaultConfig_Scheduler|TestValidateScheduler|TestConfigValidate_RejectsAnInvalidSchedulerSetting|TestSchedulerCalloutDeadlineMax|TestRootConfigVars'`
Expected: build failure — `unknown field MaxRuns in struct literal of type SchedulerConfig`, `undefined: ValidateScheduler`, `undefined: schedulerCalloutDeadlineMax` (and `app/app.go` still names the deleted scheduler types).

- [ ] **Step 3: Implement the settings**

`app/config.go`, replace `SchedulerConfig` (`:153-182`):

```go
// SchedulerConfig controls the scheduler: how often a node claims due
// scheduled tasks, how many runs it holds, how it proves itself alive, how it
// retries and how it drains. Same fields, names and order as
// scheduler.Config, which it converts to.
type SchedulerConfig struct {
	// Enabled: CYODA_SCHEDULER_ENABLED, default true.
	Enabled bool
	// ScanInterval: CYODA_SCHEDULER_SCAN_INTERVAL, default 1s, > 0.
	ScanInterval time.Duration
	// MaxRuns: CYODA_SCHEDULER_MAX_RUNS, default 8, >= 1.
	MaxRuns int
	// MaxRunsPerTenant: CYODA_SCHEDULER_MAX_RUNS_PER_TENANT, default 4, 1..MaxRuns.
	MaxRunsPerTenant int
	// HeartbeatInterval: CYODA_SCHEDULER_HEARTBEAT_INTERVAL, default 15s, > 0.
	HeartbeatInterval time.Duration
	// StaleAfter: CYODA_SCHEDULER_STALE_AFTER, default 2m,
	// >= scheduler.MinStaleAfter(HeartbeatInterval). The same on every node.
	StaleAfter time.Duration
	// MaxLostOwners: CYODA_SCHEDULER_MAX_LOST_OWNERS, default 3, >= 1.
	MaxLostOwners int
	// RetryDelay: CYODA_SCHEDULER_RETRY_DELAY, default 30s, > 0.
	RetryDelay time.Duration
	// RetryDelayMax: CYODA_SCHEDULER_RETRY_DELAY_MAX, default 15m, >= RetryDelay.
	RetryDelayMax time.Duration
	// ShutdownDrain: CYODA_SCHEDULER_SHUTDOWN_DRAIN, default 20s, >= 0.
	ShutdownDrain time.Duration
}
```

In `DefaultConfig`, delete the `DispatchForwardTimeout` line (`:468`) and replace the `Scheduler:` block (`:474-482`):
```go
		Scheduler: SchedulerConfig{
			Enabled:           envBool("CYODA_SCHEDULER_ENABLED", true),
			ScanInterval:      envDuration("CYODA_SCHEDULER_SCAN_INTERVAL", time.Second),
			MaxRuns:           envInt("CYODA_SCHEDULER_MAX_RUNS", 8),
			MaxRunsPerTenant:  envInt("CYODA_SCHEDULER_MAX_RUNS_PER_TENANT", 4),
			HeartbeatInterval: envDuration("CYODA_SCHEDULER_HEARTBEAT_INTERVAL", 15*time.Second),
			StaleAfter:        envDuration("CYODA_SCHEDULER_STALE_AFTER", 2*time.Minute),
			MaxLostOwners:     envInt("CYODA_SCHEDULER_MAX_LOST_OWNERS", 3),
			RetryDelay:        envDuration("CYODA_SCHEDULER_RETRY_DELAY", 30*time.Second),
			RetryDelayMax:     envDuration("CYODA_SCHEDULER_RETRY_DELAY_MAX", 15*time.Minute),
			ShutdownDrain:     envDuration("CYODA_SCHEDULER_SHUTDOWN_DRAIN", 20*time.Second),
		},
```

In `Config.Validate` (`:798-802`), before `return ValidateHTTP(c.HTTP)`:
```go
	if err := ValidateScheduler(c.Scheduler); err != nil {
		return err
	}
```

After `ValidateDispatch`, add:
```go
// ValidateScheduler rejects scheduler settings no scheduler could run under.
// Config is a QA'd artefact: an out-of-range value is a startup error, not a
// clamp. STALE_AFTER must leave the watchdog room for one slow or failed
// heartbeat, so it grows with the heartbeat interval.
func ValidateScheduler(c SchedulerConfig) error {
	if c.ScanInterval <= 0 {
		return fmt.Errorf("CYODA_SCHEDULER_SCAN_INTERVAL must be > 0, got %s", c.ScanInterval)
	}
	if c.MaxRuns < 1 {
		return fmt.Errorf("CYODA_SCHEDULER_MAX_RUNS must be >= 1, got %d", c.MaxRuns)
	}
	if c.MaxRunsPerTenant < 1 || c.MaxRunsPerTenant > c.MaxRuns {
		return fmt.Errorf("CYODA_SCHEDULER_MAX_RUNS_PER_TENANT must be between 1 and CYODA_SCHEDULER_MAX_RUNS (%d), got %d",
			c.MaxRuns, c.MaxRunsPerTenant)
	}
	if c.HeartbeatInterval <= 0 {
		return fmt.Errorf("CYODA_SCHEDULER_HEARTBEAT_INTERVAL must be > 0, got %s", c.HeartbeatInterval)
	}
	if minStale := scheduler.MinStaleAfter(c.HeartbeatInterval); c.StaleAfter < minStale {
		return fmt.Errorf("CYODA_SCHEDULER_STALE_AFTER must be >= 50s + 3 x CYODA_SCHEDULER_HEARTBEAT_INTERVAL (%s, given interval=%s), got %s",
			minStale, c.HeartbeatInterval, c.StaleAfter)
	}
	if c.MaxLostOwners < 1 {
		return fmt.Errorf("CYODA_SCHEDULER_MAX_LOST_OWNERS must be >= 1, got %d", c.MaxLostOwners)
	}
	if c.RetryDelay <= 0 {
		return fmt.Errorf("CYODA_SCHEDULER_RETRY_DELAY must be > 0, got %s", c.RetryDelay)
	}
	if c.RetryDelayMax < c.RetryDelay {
		return fmt.Errorf("CYODA_SCHEDULER_RETRY_DELAY_MAX must be >= CYODA_SCHEDULER_RETRY_DELAY (%s), got %s",
			c.RetryDelay, c.RetryDelayMax)
	}
	if c.ShutdownDrain < 0 {
		return fmt.Errorf("CYODA_SCHEDULER_SHUTDOWN_DRAIN must be >= 0, got %s", c.ShutdownDrain)
	}
	return nil
}
```
Add the import `"github.com/cyoda-platform/cyoda-go/internal/scheduler"` to `app/config.go`.

In `ValidateDispatch` (`:933-950`), delete the forward-timeout check and change the comment's "the connect and forward timeouts bound network calls" to "the connect timeout bounds a network call".

`internal/cluster/config.go`: delete `DispatchForwardTimeout` and its comment (`:25-27`).

`cmd/cyoda/main.go`, after the `ValidateDispatch` block (`:118-121`):
```go
	if err := app.ValidateScheduler(cfg.Scheduler); err != nil {
		slog.Error("scheduler config validation failed", "error", err)
		os.Exit(1)
	}
```

`cmd/cyoda/help/config_registry.go`: delete the `CYODA_DISPATCH_FORWARD_TIMEOUT` row (`:80`) and replace the scheduler rows (`:131-137`):
```go
	// --- scheduler ---
	{Name: "CYODA_SCHEDULER_ENABLED", Topic: "scheduler", Type: "bool", Default: "true", Description: "Kill switch: a node with false claims no scheduled task."},
	{Name: "CYODA_SCHEDULER_SCAN_INTERVAL", Topic: "scheduler", Type: "duration", Default: "1s", Description: "How often a node claims due scheduled tasks. Must be > 0; startup fails otherwise."},
	{Name: "CYODA_SCHEDULER_MAX_RUNS", Topic: "scheduler", Type: "int", Default: "8", Description: "Most scheduled runs one node holds at once. Must be >= 1; startup fails otherwise."},
	{Name: "CYODA_SCHEDULER_MAX_RUNS_PER_TENANT", Topic: "scheduler", Type: "int", Default: "4", Description: "Most scheduled runs of one tenant on one node. Must be between 1 and CYODA_SCHEDULER_MAX_RUNS; startup fails otherwise."},
	{Name: "CYODA_SCHEDULER_HEARTBEAT_INTERVAL", Topic: "scheduler", Type: "duration", Default: "15s", Description: "How often a node records that it is alive. Must be > 0; startup fails otherwise."},
	{Name: "CYODA_SCHEDULER_STALE_AFTER", Topic: "scheduler", Type: "duration", Default: "2m", Description: "How long a node may go without a heartbeat before another node takes over its scheduled runs. Must be >= 50s + 3 x CYODA_SCHEDULER_HEARTBEAT_INTERVAL and the same on every node; startup fails otherwise."},
	{Name: "CYODA_SCHEDULER_MAX_LOST_OWNERS", Topic: "scheduler", Type: "int", Default: "3", Description: "A scheduled task whose node is lost this many times ends FAILED OWNER_LOST_REPEATEDLY. Must be >= 1; startup fails otherwise."},
	{Name: "CYODA_SCHEDULER_RETRY_DELAY", Topic: "scheduler", Type: "duration", Default: "30s", Description: "Delay before the first retry of a scheduled run that failed safely; it doubles on each further failure. Must be > 0; startup fails otherwise."},
	{Name: "CYODA_SCHEDULER_RETRY_DELAY_MAX", Topic: "scheduler", Type: "duration", Default: "15m", Description: "The retry delay never grows past this. Must be >= CYODA_SCHEDULER_RETRY_DELAY; startup fails otherwise."},
	{Name: "CYODA_SCHEDULER_SHUTDOWN_DRAIN", Topic: "scheduler", Type: "duration", Default: "20s", Description: "On shutdown, how long a node waits for its scheduled runs before it cancels them; a run with an unsafe processor callout in flight is not cancelled. Must be >= 0; startup fails otherwise."},
```

`cmd/cyoda/help/content/config/scheduler.md`: replace the NAME line and the DESCRIPTION list:

```markdown
## NAME

config.scheduler — how each node claims and runs scheduled transitions: claim cadence, run limits, liveness, retries and shutdown.

## DESCRIPTION

Every node claims due scheduled tasks and runs them itself. A node proves it is alive with a heartbeat; another node takes over its tasks only after `CYODA_SCHEDULER_STALE_AFTER` without one. Startup fails on a value outside its rule.

- `CYODA_SCHEDULER_ENABLED` (bool, default: `true`) — kill switch: a node with `false` claims no scheduled task.
- `CYODA_SCHEDULER_SCAN_INTERVAL` (duration, default: `1s`) — how often the node claims due tasks. A node also claims at once when a run slot frees after a claim took every slot. Must be `> 0`.
- `CYODA_SCHEDULER_MAX_RUNS` (int, default: `8`) — most scheduled runs one node holds at once. Must be `>= 1`.
- `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT` (int, default: `4`) — most runs of one tenant on one node. Must be between `1` and `CYODA_SCHEDULER_MAX_RUNS`.
- `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` (duration, default: `15s`) — how often a node records that it is alive. Must be `> 0`.
- `CYODA_SCHEDULER_STALE_AFTER` (duration, default: `2m`) — how long a node may go without a heartbeat before another node takes over its runs. Must be at least `50s + 3 × CYODA_SCHEDULER_HEARTBEAT_INTERVAL`, and the same on every node of a cluster. A node whose heartbeats keep failing cancels its own runs before this time passes.
- `CYODA_SCHEDULER_MAX_LOST_OWNERS` (int, default: `3`) — a task whose node is lost this many times ends FAILED `OWNER_LOST_REPEATEDLY`. Must be `>= 1`.
- `CYODA_SCHEDULER_RETRY_DELAY` (duration, default: `30s`) — delay before the first retry of a run that failed without handing unsafe work to a compute node. It doubles on each further failure and never passes the transition's `timeoutMs`. Must be `> 0`.
- `CYODA_SCHEDULER_RETRY_DELAY_MAX` (duration, default: `15m`) — the retry delay never grows past this. Must be `>= CYODA_SCHEDULER_RETRY_DELAY`.
- `CYODA_SCHEDULER_SHUTDOWN_DRAIN` (duration, default: `20s`) — on shutdown, how long the node waits for its runs before it cancels them. A run whose unsafe processor callout is in flight is not cancelled. Must be `>= 0`.
```

`cmd/cyoda/help/content/config/cluster.md`: delete the `CYODA_DISPATCH_FORWARD_TIMEOUT` line (`:32`).

`cmd/cyoda/help/content/config.md:33`: `` - `config.scheduler` — how each node claims and runs scheduled transitions: cadence, limits, liveness, retries, shutdown ``

`README.md`: replace `:226-236` (the paragraph and the table) with:

```markdown
A workflow transition with a `schedule` fires automatically after a delay. The delay can be a static `delayMs`, or a `function` callout computing the firing time (and optional expiry) per entity at arm time — mutually exclusive with `delayMs`. Every node claims due scheduled tasks and runs them itself. See `cyoda help config scheduler` for the full topic.

| Env var | Default | Effect |
|---------|---------|--------|
| `CYODA_SCHEDULER_ENABLED` | `true` | Kill switch: a node with `false` claims no scheduled task. |
| `CYODA_SCHEDULER_SCAN_INTERVAL` | `1s` | How often a node claims due tasks. |
| `CYODA_SCHEDULER_MAX_RUNS` | `8` | Most scheduled runs one node holds at once. |
| `CYODA_SCHEDULER_MAX_RUNS_PER_TENANT` | `4` | Most runs of one tenant on one node; at most `CYODA_SCHEDULER_MAX_RUNS`. |
| `CYODA_SCHEDULER_HEARTBEAT_INTERVAL` | `15s` | How often a node records that it is alive. |
| `CYODA_SCHEDULER_STALE_AFTER` | `2m` | Time without a heartbeat before another node takes over a node's runs; at least `50s + 3 × heartbeat`, the same on every node. |
| `CYODA_SCHEDULER_MAX_LOST_OWNERS` | `3` | A task whose node is lost this many times ends FAILED. |
| `CYODA_SCHEDULER_RETRY_DELAY` | `30s` | Delay before the first retry of a safe failure; doubles on each further one. |
| `CYODA_SCHEDULER_RETRY_DELAY_MAX` | `15m` | The retry delay never grows past this. |
| `CYODA_SCHEDULER_SHUTDOWN_DRAIN` | `20s` | On shutdown, how long a node waits for its runs before it cancels them. |
```
and delete the `CYODA_DISPATCH_FORWARD_TIMEOUT` row (`:251`).

- [ ] **Step 4: Implement the wiring and the removals**

`app/app.go`:
- Replace the `healthFlag` comment (`:88-95`) "…the async-search goroutine and the scheduler's dispatch goroutine." with "…the async-search goroutine and the scheduler's goroutines (its claim loop, heartbeat, watchdog and runs)."
- In the engine construction (`:582-586`), delete `workflow.WithExpiryGrace(cfg.Scheduler.ExpiryGrace)` if stream E has not already, and change the `schedClock` comment (`:578-580`) to: "schedClock is the pnode clock, shared by the engine's arm and fire math and the scheduler, so both agree on "now"."
- Delete `:600-657` (the RPC client, the executor, the strategies and `scheduler.NewService`) and put in its place:
```go
	// The scheduler: this node claims due scheduled tasks and runs them
	// itself. A disabled scheduler starts nothing; Shutdown drains it either way.
	a.scheduler = scheduler.New(scheduler.Config(cfg.Scheduler), scheduler.Deps{
		Store:              a.storeFactory,
		TxManager:          a.transactionManager,
		Firer:              a.workflowEngine,
		Clock:              schedClock,
		HealthFlag:         a.healthFlag,
		Meter:              observability.Meter(),
		CalloutDeadlineMax: schedulerCalloutDeadlineMax(cfg),
	})
	if err := a.scheduler.Start(context.Background()); err != nil {
		slog.Error("startup failure", "phase", "scheduler-start", "error", err.Error())
		os.Exit(1)
	}
```
- Delete the two `cluster.NewSchedulerRPCHandler(schedEngine, peerAuth).Register(…)` lines (`:813`, `:826`).
- Add after `ReadinessCheck`:
```go
// DrainScheduler runs the scheduler's shutdown steps 1-5. The binary calls it
// on a signal before the servers drain, so runs still in progress keep their
// compute-node streams and callback routes. Shutdown calls it again; the
// second call does nothing.
func (a *App) DrainScheduler(ctx context.Context) {
	if a.scheduler != nil {
		a.scheduler.Drain(ctx)
	}
}

// schedulerCalloutDeadlineMax is the longest one callout can take: every try
// at the largest answer limit, the patience and the hand-over allowance.
func schedulerCalloutDeadlineMax(c Config) time.Duration {
	return time.Duration(1+c.Callout.FixedNumRetries)*c.Callout.ResponseTimeoutMax +
		c.Cluster.DispatchWaitTimeout + c.Callout.HandoverAllowance
}
```
- In `Shutdown` (`:1047-1069`), delete the `if a.scheduler != nil { a.scheduler.Stop() }` block (`:1061-1063`) and make the first statement of `Shutdown`:
```go
	// A server failed, or the binary did not drain the scheduler first: the
	// same steps run here, after the servers stopped.
	a.DrainScheduler(context.Background())
```

Delete the RPC:
```
git rm internal/cluster/scheduler_rpc.go internal/cluster/scheduler_rpc_test.go
```

`internal/cluster/dispatch/encode.go`: delete `EncodePeerBody` and its comment (`:25-29`). Create `internal/cluster/dispatch/export_test.go`:
```go
package dispatch

// EncodePeerBody exposes the peer-body encoder to this package's external tests.
var EncodePeerBody = encodePeerBody
```

`internal/cluster/peeraddr/peeraddr.go:44-46`: "Callers that DO have a figure — the hand-over its connect timeout, the scheduler RPC its whole-call timeout — pass theirs instead." → "A caller that has a figure of its own — the hand-over's connect timeout — passes it instead."

`internal/cluster/dispatch/aead_peer_auth.go:44-46`: "Exported because every leg that reads a peer envelope — callout dispatch and the scheduler RPC alike — bounds its read by the one ceiling." → "Exported because every leg that reads a peer envelope bounds its read by the one ceiling."

`internal/domain/search/reaper.go:16-21`, the `StaleClaimBatch` comment:
```go
// StaleClaimBatch caps how many stale jobs a single ReclaimStaleJobs call
// claims. Fixed rather than configurable: it bounds one reaper tick's work
// however large staleAfter's backlog has grown, so a burst of dead jobs cannot
// turn one tick into an unbounded claim-and-execute loop.
```

Remove any import the compiler reports unused in the files above.

- [ ] **Step 5: Run the tests to see them pass**

Run:
```
go build ./...
go test ./app/... ./cmd/cyoda/... ./internal/cluster/... ./internal/scheduler/... ./internal/domain/search/...
```
Expected: the build succeeds except for `internal/e2e` and `e2e/parity/multinode` (R-11); every listed package `ok`, including `TestConfig_EnvVarCoverage`, `TestConfigAll_Complete`, `TestRootConfigVars_MatchDefaults` and `TestEncodePeerBody_DoesNotEscapeForHTML`.

- [ ] **Step 6: Exit check for the files this task owns**

Run:
```
git grep -n -e SchedulerRPC -e ClusterExecutor -e dispatch/scheduled-task -e DispatchForwardTimeout \
  -e CYODA_SCHEDULER_DISTRIBUTION -e CYODA_SCHEDULER_COORDINATOR -e CYODA_SCHEDULER_REDISPATCH_BACKOFF \
  -e CYODA_SCHEDULER_BATCH_SIZE -e CYODA_SCHEDULER_EXPIRY_GRACE -e CYODA_DISPATCH_FORWARD_TIMEOUT \
  -e RedispatchBackoff -e ExpiryGrace -e LowestLiveNodeID -e 'scheduler\.RoundRobin' -e 'scheduler RPC' \
  -- app cmd internal/cluster internal/scheduler internal/domain/search README.md
```
Expected: no output.

- [ ] **Step 7: Commit**

```
git add app/config.go app/app.go app/config_scheduler_test.go app/config_test.go app/config_dispatch_test.go \
  app/config_registry_binding_test.go internal/cluster/config.go cmd/cyoda/main.go \
  cmd/cyoda/help/config_registry.go cmd/cyoda/help/content/config/scheduler.md \
  cmd/cyoda/help/content/config/cluster.md cmd/cyoda/help/content/config.md README.md \
  internal/cluster/dispatch/encode.go internal/cluster/dispatch/export_test.go \
  internal/cluster/peeraddr/peeraddr.go internal/cluster/dispatch/aead_peer_auth.go \
  internal/domain/search/reaper.go
git commit -m "feat(scheduler): claim-based scheduler settings and wiring; remove the scheduler RPC

Removes CYODA_SCHEDULER_DISTRIBUTION, _COORDINATOR, _REDISPATCH_BACKOFF,
_BATCH_SIZE, _EXPIRY_GRACE and CYODA_DISPATCH_FORWARD_TIMEOUT, and the
/internal/dispatch/scheduled-task route.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

(`git rm` already staged the two deleted files.)

---

### Task R-11: Callers in the e2e suites

**Spec:** §6.6 ("their tests"), §15.

TDD waiver: these edits make test code compile against the new service and
remove a test of a route that no longer exists. The replacement coverage is
named in Open point 7.

**Files:**
- Modify: `internal/e2e/scheduled_transition_test.go` (`:20-65`)
- Modify: `internal/e2e/scheduled_function_test.go` (`:540-596`)
- Modify: `internal/e2e/e2e_test.go` (`:158-171`)
- Modify: `internal/e2e/callback_harness_test.go` (`:265-273`)
- Modify: `internal/e2e/tx_lifecycle_e2e_test.go` (`:510-737`)
- Modify: `internal/e2e/callout_handover_lost_test.go` (`:36-42`)
- Modify: `e2e/parity/multinode/attribution.go` (`:185-270`)

**Interfaces:**
- Consumes: R-10 (`scheduler.New`, `Deps`, `App.TransactionManager`, `App.WorkflowEngine`, `App.StoreFactory`).
- Produces: `func startSchedulerFor(t *testing.T, a *app.App)` in `internal/e2e` (package `e2e_test`).

- [ ] **Step 1: Replace the test scheduler helper**

`internal/e2e/scheduled_transition_test.go`: replace the `scheduledFireTimeout` comment's reason ("a scan is cross-tenant and node-blind, so exactly one scheduler may scan it") with "a claim is cross-tenant, and testApp's engine knows none of a harness's callouts", and replace `startTestScheduler` (`:35-65`) with:

```go
// startTestScheduler starts a scheduler of this test's own against testApp and
// stops it when the test ends. testApp's own scheduler is disabled (see
// TestMain), so this one claims testApp's tasks.
func startTestScheduler(t *testing.T) {
	t.Helper()
	startSchedulerFor(t, testApp)
}

// startSchedulerFor starts a scheduler for a, claiming every 100ms, and stops
// it when the test ends.
func startSchedulerFor(t *testing.T, a *app.App) {
	t.Helper()
	svc := scheduler.New(scheduler.Config{
		Enabled: true, ScanInterval: 100 * time.Millisecond, MaxRuns: 8, MaxRunsPerTenant: 4,
		HeartbeatInterval: 15 * time.Second, StaleAfter: 2 * time.Minute, MaxLostOwners: 3,
		RetryDelay: time.Second, RetryDelayMax: time.Minute, ShutdownDrain: 5 * time.Second,
	}, scheduler.Deps{
		Store:              a.StoreFactory(),
		TxManager:          a.TransactionManager(),
		Firer:              a.WorkflowEngine(),
		Clock:              scheduler.NewRealClock(),
		CalloutDeadlineMax: time.Minute,
	})
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	t.Cleanup(svc.Stop)
}
```

`internal/e2e/scheduled_function_test.go`: replace `:576-596` (the adapter, the executor, `scheduler.NewService`, `Start`, `defer Stop`) with `startSchedulerFor(t, h.app)`, and in the comment above the test (`:544-549`) replace "only starting a bespoke scheduler.Service (mirrors TestE2E_ScheduledTransition_RestartDurability's fresh-instance pattern)" with "only starting a scheduler of its own (startSchedulerFor)".

`internal/e2e/e2e_test.go:158-171`, the comment above `cfg.Scheduler.Enabled = false`:
```go
	// testApp shares this PostgreSQL database with every per-test harness. Its
	// scheduler would claim their due tasks too — a claim is cross-tenant — and
	// run them with testApp's own processors, which know none of a harness's
	// callouts. So testApp claims nothing: tests that need a scheduled fire
	// against testApp start a scheduler of their own (startTestScheduler in
	// scheduled_transition_test.go). Plain config, no test hook.
```

`internal/e2e/callback_harness_test.go:269-272`: "…so it can drive its own bespoke, precisely-timed scheduler.Service instead (mirrors TestE2E_ScheduledTransition_RestartDurability's approach)…" → "…so it can start a scheduler of its own (startSchedulerFor) at the moment it chooses…".

- [ ] **Step 2: Remove the scheduler RPC test**

`internal/e2e/tx_lifecycle_e2e_test.go`: delete from `:510` (`// --- coverage row 7a …`) to the end of the file (`:737`), except `clusterHMACSecret32` and its comment (`:511-517`), which `callout_handover_lost_test.go` uses. Put that variable after the harness helpers under the header `// --- cluster secret shared with the hand-over tests ---`. This removes `schedulerTaskPathForTest`, `newClusterHarness`, `txLifeSchedDelayMs`, `txLifeSchedGateWF`, `findScheduledTask`, `postSchedulerRPC` and `TestE2E_SchedulerRPCPanic_RecoveredAndRolledBack`; none has another caller (`git grep` for each name returns only this file). Remove the imports the compiler reports unused.

`internal/e2e/callout_handover_lost_test.go:36-41`: "Duplicated rather than exported across the package boundary for a one-line route string — the precedent schedulerTaskPathForTest (tx_lifecycle_e2e_test.go) sets." → "Duplicated rather than exported across the package boundary for a one-line route string."

- [ ] **Step 3: Remove the peer-RPC assertion from the multi-node attribution scenario**

`e2e/parity/multinode/attribution.go`:
- Doc comment of `RunAttribution_ScheduledFire` (`:187-194`): "RunAttribution_ScheduledFire arms several tasks as a USER through node 1, waits for them to fire, and asserts every fired change attributes to the arming user (executed by the system). Whichever node claims a task runs it and reads ArmedBy from the claimed record."
- Delete the log-level raise and its restore (`:201-216`): it served only the peer-RPC log line.
- Replace the arm comment (`:224-231`) with: "Arm several tasks through node 1. Any node may claim and run them; the durable ArmedBy (the user) drives the attribution wherever the task runs."
- Delete the positive peer-execution assertion (`:253-269`).
- Remove the imports and helpers the compiler reports unused.

- [ ] **Step 4: Build and run the affected suites**

Run:
```
go build ./... && go vet ./...
go test ./internal/e2e/... -run 'TestE2E_ScheduledTransition|TestScheduledFunction'
make test
```
Expected: build and vet clean; the two e2e groups `ok` (Docker required); `make test` green. If a scheduled e2e test fails on a timing that stream E or T changed (grace band removed, retry delay), it is theirs to adapt: report it, do not patch the assertion here.

- [ ] **Step 5: Exit check for the e2e suites**

Run:
```
git grep -n -e SchedulerRPC -e ClusterExecutor -e dispatch/scheduled-task -e 'scheduler\.NewService' \
  -e NewSchedulerEngine -e LowestLiveNodeID -e 'scheduler\.Self' -e RedispatchBackoff -e peerFireMarker \
  -- internal/e2e e2e
```
Expected: no output.

- [ ] **Step 6: Commit**

```
git add internal/e2e/scheduled_transition_test.go internal/e2e/scheduled_function_test.go \
  internal/e2e/e2e_test.go internal/e2e/callback_harness_test.go internal/e2e/tx_lifecycle_e2e_test.go \
  internal/e2e/callout_handover_lost_test.go e2e/parity/multinode/attribution.go
git commit -m "test(e2e): start the claim-based scheduler in e2e; drop the scheduler RPC test

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task R-12: The scheduler drains before the servers on a signal

**Spec:** §6.4 ("before the servers drain", step 6, the server-failure
paragraph); V4 (settled above). §13 Shutdown row "runs finish within the
drain; streams stay open" (the binary's half: the servers still serve while
the scheduler drains).

**Files:**
- Modify: `cmd/cyoda/run.go` (`:74-195`)
- Modify: `cmd/cyoda/main.go` (`:219`)
- Test: `cmd/cyoda/run_test.go`

**Interfaces:**
- Consumes: R-10 `(*app.App).DrainScheduler`.
- Produces: `func runServers(rootCtx context.Context, a *app.App, cfg app.Config, ls serverListeners, drainScheduler func(context.Context)) error`.

- [ ] **Step 1: Write the failing tests**

In `cmd/cyoda/run_test.go`, pass `a.DrainScheduler` as the new last argument at the three existing calls (`:61`, `:124`, `:154`), add `"sync/atomic"` to the imports, and append:

```go
// waitHTTPUp polls /health until the HTTP server answers.
func waitHTTPUp(t *testing.T, httpAddr string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(httpAddr + "/health")
		if err == nil {
			resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("HTTP server did not come up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunServers_SignalDrainsTheSchedulerBeforeTheServers pins the §6.4
// order: on a signal the scheduler drains while the servers still serve, so
// its runs keep the compute-node streams and the callback routes.
func TestRunServers_SignalDrainsTheSchedulerBeforeTheServers(t *testing.T) {
	a, cfg, ls := newRunServersFixture(t)
	httpAddr := "http://" + ls.http.Addr().String()
	var servedDuringDrain atomic.Bool
	drained := make(chan struct{})
	drain := func(context.Context) {
		defer close(drained)
		resp, err := http.Get(httpAddr + "/health")
		if err == nil {
			resp.Body.Close()
			servedDuringDrain.Store(resp.StatusCode == http.StatusOK)
		}
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runServers(rootCtx, a, cfg, ls, drain) }()
	waitHTTPUp(t, httpAddr)

	cancel()
	select {
	case <-drained:
	case <-time.After(20 * time.Second):
		t.Fatal("the scheduler drain never ran on the signal path")
	}
	if !servedDuringDrain.Load() {
		t.Error("HTTP stopped serving before the scheduler finished draining")
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("runServers returned error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runServers did not return after the drain")
	}
	assertListenersClosed(t, ls)
}

// TestRunServers_ServerFailureDoesNotWaitForTheSchedulerDrain: a failed server
// stops the others at once; a.Shutdown drains the scheduler after them.
func TestRunServers_ServerFailureDoesNotWaitForTheSchedulerDrain(t *testing.T) {
	a, cfg, ls := newRunServersFixture(t)
	if err := ls.grpc.Close(); err != nil {
		t.Fatalf("close grpc listener: %v", err)
	}
	var drainCalled atomic.Bool
	runDone := make(chan error, 1)
	go func() {
		runDone <- runServers(context.Background(), a, cfg, ls, func(context.Context) { drainCalled.Store(true) })
	}()
	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "grpc serve:") {
			t.Errorf("runServers error = %v; want the gRPC serve failure", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runServers did not return after the gRPC server failed")
	}
	if drainCalled.Load() {
		t.Error("the signal-path drain ran on a server failure; a.Shutdown drains the scheduler there")
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./cmd/cyoda/... -run TestRunServers`
Expected: build failure — `too many arguments in call to runServers`.

- [ ] **Step 3: Implement**

`cmd/cyoda/run.go`: add the parameter, and replace the first line of the body (`g, ctx := errgroup.WithContext(rootCtx)`) with:

```go
	g, gctx := errgroup.WithContext(context.Background())
	// stopCtx ends when the servers must stop. On a signal the scheduler drains
	// first, while its runs still have the compute-node streams and the
	// callback routes; then the servers drain. A server that fails stops the
	// others at once, and a.Shutdown drains the scheduler after them.
	stopCtx, stopServers := context.WithCancel(gctx)
	defer stopServers()
	g.Go(func() error {
		select {
		case <-rootCtx.Done():
			drainScheduler(context.Background())
			stopServers()
		case <-gctx.Done():
		}
		return nil
	})
```

Replace every `<-ctx.Done()` in the body (`:120`, `:146`, `:165`) with `<-stopCtx.Done()`. In the gRPC watcher's sequence comment (`:128-135`), make step 1 "rootCtx.Done fires (signal.NotifyContext); the scheduler drains (a.DrainScheduler); stopCtx is cancelled". In the function's doc comment (`:74-88`) add after the first sentence: "On cancel it first drains the scheduler through drainScheduler, while every server still serves, and then drains each server within shutdownDrainBudget."

`cmd/cyoda/main.go:219`: `if err := runServers(rootCtx, a, cfg, ls, a.DrainScheduler); err != nil {`

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./cmd/cyoda/...`
Expected: `ok` (the SIGTERM subprocess tests in `main_sigterm_test.go` included).

- [ ] **Step 5: Whole-stream exit check**

Run the §15 commands from the repository root:
```
git grep -n -e RedispatchAfter -e RedispatchBackoff -e MarkRedispatch -e AttemptCount -e ScanDue \
  -e redispatch_after -e attempt_count -e LowestLiveNodeID -e 'scheduler\.RoundRobin' -e SchedulerRPC \
  -e ClusterExecutor -e dispatch/scheduled-task -e DispatchForwardTimeout \
  -e CYODA_SCHEDULER_DISTRIBUTION -e CYODA_SCHEDULER_COORDINATOR -e CYODA_SCHEDULER_REDISPATCH_BACKOFF \
  -e CYODA_SCHEDULER_BATCH_SIZE -e CYODA_SCHEDULER_EXPIRY_GRACE -e CYODA_DISPATCH_FORWARD_TIMEOUT \
  -e ExpiryGrace -e expiryGrace \
  -- . ':!*/migrations/*' ':!docs/plans' ':!docs/superpowers' ':!docs/release-notes' ':!CHANGELOG.md'
```
Expected: no hit in a file this stream touched. The remaining hits must all be in files other streams own (`docs/ARCHITECTURE.md` `:943, :1992, :2248` for stream D; `COMPATIBILITY.md:37`, Open point 8; the plugins and `internal/domain/workflow` for S/BM/BQ/BP/E if they are not finished). List them in the task report.

- [ ] **Step 6: Commit**

```
git add cmd/cyoda/run.go cmd/cyoda/main.go cmd/cyoda/run_test.go
git commit -m "feat(cyoda): drain the scheduler before the servers on a signal

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

## §13 rows owned by stream R

Each row of the spec's matrix with a cell this stream covers, and the test
that covers it. "(U half)" means the scheduler's part of a row whose other U
part is another stream's.

| §13 row | Layer | Test |
|---|---|---|
| fired on time | U (half) | R-4 `TestDecideBookkeeping/fired`; R-6 `TestService_CommittedOutcomesRecordNothing/fired` |
| fired after one safe failure | U (half) | R-4 `…/a_plain_safe_failure`; R-6 `TestService_FailedRunRecordsAnAttempt` |
| declined | U (half) | R-4 `…/declined`; R-6 `…CommittedOutcomesRecordNothing/declined` |
| expired, late on the first attempt | U (half) | R-4 `…/expired`; R-6 `…CommittedOutcomesRecordNothing/expired` |
| criterion error → WAITING, attempts 1, error recorded | U (half) | R-6 `TestService_FailedRunRecordsAnAttempt` |
| no compute node → `NotHandedOff`, mark cleared, WAITING | U (half) | R-4 `…/mark_held,_nothing_handed_off` |
| idempotent processor fails → WAITING | U (half) | R-4 `…/a_plain_safe_failure` |
| retry delay doubles, saturates, clamps to the deadline; … later → FAILED | U (half) | R-4 `…/the_second_failure…`, `…/saturates…`, `…/never_overflows`, `…/clamped_to_the_deadline`, `…/at_the_deadline…`, `…/a_counted_attempt_past_the_deadline…` |
| late after failed attempts → FAILED | U (half) | R-4 `…/a_counted_attempt_past_the_deadline_fails_the_task` |
| unsafe processor fails → FAILED | U (half) | R-4 `…/unsafe_work_reached_a_compute_node` |
| a later step fails after an unsafe hand-off → FAILED | U (half) | same |
| failure after a successful unsafe dispatch in the same step → FAILED | U (half) | same |
| savepoint error replaces the dispatch error → FAILED | U (half) | same |
| cancelled before `Send` returned nil → WAITING | U (half) | R-4 `…/mark_held,_nothing_handed_off` |
| cancelled after `Send` returned nil → FAILED | U (half) | R-4 `…/unsafe_work_reached_a_compute_node` |
| hand-over past `StageNotConnected` without `no_handoff` → FAILED | U (half) | same |
| database outage during `MarkUnsafe` → `RecordAttempt{ClearOwnMark}` after recovery | U (half) | R-4 `…/MarkUnsafe_failed…`; R-7 `TestService_OutcomeWriteRetriedThroughAnOutage` |
| `RecordAttempt{ClearOwnMark}` retried in an outage; pnode dies first | U (half) | R-7 `TestService_OutcomeWriteRetriedThroughAnOutage` |
| `ErrMarkedByAnotherClaim` → FAILED | U (half) | R-4 `…/marked_by_another_claim…` |
| `ErrTaskBusy` → safe failure | U (half) | R-4 `…/a_plain_safe_failure` |
| CBD in a cascade step, later failure → FAILED `STOPPED_AFTER_PARTIAL_COMMIT` | U (half) | R-4 `…/a_partial_commit_comes_before_the_mark` |
| `SCHEDULED_TRANSITION_FAIL` recorded with its reason | U | R-7 `TestService_FailAndItsAuditEventCommitTogether` |
| panicking run → FAILED `RUN_PANICKED`, node latched, claims stop | U | R-8 `TestService_PanickingRunFailsItsTaskAndLatches`, `…PanicWhileRecordingTheOutcome…` |
| `lastError` of a non-sentinel store error is "internal error [ticket]" only | U | R-3 `TestRecordedError_AnythingElseIsATicketOnly`; R-7 `…OutcomeLogsCarryTheTicketAndNoToken` |
| `lastError` of a `MemberFailed` message, a callout timeout, an Operational `AppError`, `NO_COMPUTE_MEMBER_FOR_TAG` | U | R-3 `TestRecordedError_AllowList` |
| `lastError` over 1 024 bytes with multi-byte characters and a NUL | U | R-3 `TestSanitiseErrorText` |
| a bookkeeping write retried through an outage; accepted after recovery | U | R-7 `TestService_OutcomeWriteRetriedThroughAnOutage` |
| `spi.ErrStoreRejected` → ERROR with ticket, node latched | U | R-7 `TestService_StoreRejectionLatchesAndKeepsTheClaim` |
| a bookkeeping write blocked by a lock or an exhausted pool is retried without latching | U | R-7 `TestService_LockAndPoolErrorsAreRetriedWithoutLatching` |
| bookkeeping after a self-cancel does not inherit the run's cancellation | U | R-6 `TestService_FailingHeartbeatsSelfCancelAtTheWindow` |
| `lastError` for a cancelled run and for a conflict: fixed texts, WARN, no ticket | U | R-3 `TestRecordedError_AllowList` |
| a lost claim reply: the next `GiveBackIdle` returns the claimed tasks | U | R-6 `TestService_LostClaimReplyIsGivenBack` |
| owner lost 3 times → FAILED `OWNER_LOST_REPEATEDLY` | U (half) | R-4 `…/owner_lost_too_often…`; R-6 `TestService_EngineDecidedFailureFailsTheTask`, `…RunsUnderTheSystemIdentityAndItsRunGuard` (`MaxLostOwners` passed) |
| database outage longer than `STALE_AFTER` → no lost-owner claims before a full stale period | U | R-6 `TestService_LostOwnerClaimsWaitForAFullStalePeriodOfCleanHeartbeats` |
| after a re-arm, every fenced write of the old life is refused | U (half) | R-6 `TestService_RefusedOutcomeMeansSuperseded` |
| heartbeat failure → self-cancel at `W`; no claims until recovery | U | R-6 `TestService_FailingHeartbeatsSelfCancelAtTheWindow` |
| no new commit after the watchdog fires; an in-flight commit blocks the reclaim | U (half) | R-6 `TestService_FailingHeartbeatsSelfCancelAtTheWindow` (the run's `Done` closes at `W`) |
| a hung heartbeat does not stop the watchdog | U | R-6 `TestService_HungHeartbeatDoesNotStopTheWatchdog` |
| a watchdog panic: the latch cancels the runs | U | R-8 `TestService_APanicInTheLoopHeartbeatOrWatchdogLatchesAndCancels`, `TestService_TheLatchCancelsEveryRunInProgress` |
| no claim before the first heartbeat | U | R-6 `TestService_NoClaimBeforeTheFirstHeartbeat` |
| `STALE_AFTER` validation: a single slow or failed heartbeat never self-cancels | U | R-2 `TestWatchdogWindow_OneSlowOrFailedHeartbeatNeverSelfCancels`; R-6 `TestService_OneFailedHeartbeatDoesNotSelfCancel`, `…HeartbeatThatSucceedsAfterItsWindowCountsAsFailed`; R-10 `TestValidateScheduler` |
| a RUNNING task with no live run is given back; a live run never is | U (half) | R-6 `TestService_GiveBackIdleKeepsEveryLiveRun` |
| at most `MAX_RUNS` runs; a freed slot triggers an immediate claim | U | R-6 `TestService_AtMostMaxRunsAndAFreedSlotClaimsAtOnce`, `…ClaimRequestCarriesOwnerLimitsAndTenantCounts` |
| an empty cluster view has no effect | U | `scheduler.Deps` has no cluster view; R-6 `TestService_RunsUnderTheSystemIdentityAndItsRunGuard` runs a task with none |
| §9 instruments are emitted with their attributes | U | R-5 `TestMetrics_EveryInstrumentWithItsAttributes` |
| runs finish within the drain; streams stay open | U | R-9 `TestDrain_RunsThatFinishWithinTheDrainAreNotCut`; R-12 `TestRunServers_SignalDrainsTheSchedulerBeforeTheServers` |
| run cut after the drain, nothing handed off → WAITING, uncounted | U | R-9 `TestDrain_ACutRunWithNothingHandedOffGoesBackUncounted` |
| an unsafe callout in flight at shutdown is not cut | U | R-9 `TestDrain_AnUnsafeCalloutInFlightIsNotCut` |
| after the signal, a run reaching a new unsafe dispatch counts as cut | U | R-9 `TestDrain_ReachingANewUnsafeDispatchAfterTheSignalCountsAsCut` |
| a run … cut after the drain, whose unsafe work was handed off earlier → FAILED | U | R-9 `TestDrain_ACutRunWhoseUnsafeWorkWasHandedOffEarlierFails` |
| a run still live after step 4 is not given back | U | R-9 `TestDrain_ARunStillLiveAfterStepFourIsNotGivenBack` |
| bookkeeping stops at its shutdown deadline | U | R-9 `TestDrain_BookkeepingStopsAtItsShutdownDeadline` |
| each new variable: its default and its validation failure | U | R-10 `TestDefaultConfig_Scheduler`, `…SchedulerEnvOverrides`, `TestValidateScheduler`, `TestConfigValidate_RejectsAnInvalidSchedulerSetting` |
| each removed variable is no longer read | U | R-10 `TestRootConfigVars_RemovedSchedulerSettingsAreGone` and the R-10 / R-12 exit greps |

The E, M, P and S cells of these rows are other streams'.

## Stream interface summary

**Produces** (`internal/scheduler`):
- `type Config` (binding, R-4), `type Firer` (binding), `type Deps struct{ Store spi.StoreFactory; TxManager spi.TransactionManager; Firer Firer; Clock Clock; HealthFlag *atomic.Bool; Meter metric.Meter; CalloutDeadlineMax time.Duration }`.
- `func New(cfg Config, deps Deps) *Service`; `Start(ctx) error` (a disabled service starts nothing); `Drain(ctx)` (steps 1-5, once); `Stop()` = `Drain(context.Background())`.
- `func MinStaleAfter(heartbeat time.Duration) time.Duration`.
- `decideBookkeeping`, `Bookkeeping`, `BookkeepingKind` (`NoneKind`, `RecordAttemptKind`, `FailKind`), `recordedError`, `sanitiseErrorText` — as in `interfaces.md`.
- Instruments `cyoda.scheduler.runs`, `.run.duration`, `.runs.in_progress`, `.claims`, `.heartbeat.failures`, `.bookkeeping.retries`; span `scheduler.run` with `outcome`.
- Log lines for stream T and D: WARN `scheduled run failed; the task waits for its next attempt`; ERROR `scheduled task FAILED` (`reason`, `ticket`); WARN `scheduler heartbeats failed for the whole watchdog window; cancelled every run in progress`.
- The audit event `SCHEDULED_TRANSITION_FAIL`: `Data{transition, sourceState, reason, attempts, lostOwners}`, `State` = the entity's state in the Fail transaction (the source state when the entity is gone), `TransactionID` = that transaction.

**Produces** (`app`, `cmd/cyoda`):
- `app.SchedulerConfig` with the ten fields of `scheduler.Config`, in its order; `app.ValidateScheduler`; `(*app.App).DrainScheduler(ctx)`.
- `runServers(rootCtx, a, cfg, ls, drainScheduler func(context.Context)) error`.
- `internal/e2e`: `startSchedulerFor(t, a *app.App)`.

**Consumes:**
- S: `ScheduledTaskStore` (`ClaimDue`, `Heartbeat`, `RetireOwner`, `SweepOwners`, `GiveBackIdle`, `RecordAttempt`, `Fail`, `SweepMarks`), `ClaimRequest`, `TaskRef`, `TaskClaim`, `Attempt`, `Failure`, the statuses and reasons, `ErrStaleClaim`, `ErrStoreRejected`, `SMEventScheduledTransitionFailed`; **requested:** `ScheduledTask.ClaimedFromLostOwner bool` (Open point 1).
- E: `RunReport` and the outcomes, `WithRunGuard`, `*Engine` satisfying `Firer`; **requested:** `RunGuard.Draining`, `RunGuard.Unsafe`, `type UnsafeFlight`, exported `RunGuardFrom` (Open point 2).
- BP: nothing called directly; the scheduler pool serves the never-joining methods above.

## Open points

1. **SPI: which claims took a task from a lost owner.** `cyoda.scheduler.claims{reason}` needs to know it per task, and `ClaimDue`'s result does not say. R-6 reads a requested field `ScheduledTask.ClaimedFromLostOwner bool`, set only on a `ClaimDue` result, when this claim took the task from a stale or missing owner. The lead folds it into `interfaces.md`; stream S adds it with a `spitest` case; BM, BQ and BP set it.

2. **E: what the run guard must carry for shutdown.** §6.4 step 3 exempts a run whose unsafe callout is in flight, and from step 1 no run may start a new unsafe dispatch. The service can only learn both through the guard. R uses, and asks E to provide:
   ```go
   type RunGuard struct {
       Ref      spi.TaskRef
       Store    spi.ScheduledTaskStore
       Done     <-chan struct{} // the run's cancellation
       Draining <-chan struct{} // closed at shutdown step 1
       Unsafe   *UnsafeFlight   // the engine brackets each unsafe dispatch with it
   }
   // UnsafeFlight records whether an unsafe processor callout of the run is in flight.
   type UnsafeFlight struct{ mu sync.Mutex; n int; since time.Time }
   func (f *UnsafeFlight) Begin()                     // n++; since = now when n goes 0 → 1
   func (f *UnsafeFlight) End()                       // n--
   func (f *UnsafeFlight) Since() (time.Time, bool)   // start of the oldest one in flight; false when none
   func RunGuardFrom(ctx context.Context) *RunGuard   // exported; the scheduler's tests read it
   ```
   Engine protocol at each unsafe dispatch site under a guard: `Unsafe.Begin()` first; then if `Draining` is closed, no `MarkUnsafe` and no dispatch, and the run ends `OutcomeFailed` with an error that satisfies `errors.Is(err, context.Canceled)` (the scheduler reads that as "cut"); then the `Done` check, `MarkUnsafe`, the dispatch; `Unsafe.End()` when the dispatch returns. Begin before the `Draining` check closes the window in which step 3 could see nothing in flight while a dispatch is about to start.

3. **Spec §5.3 and §6.4 differ on the exempt run.** §5.3 says a run with an unsafe callout in flight "is exempt until that callout ends"; §6.4 says it then "may go on with safe dispatches … and commits, until the step-4 bound". R follows §6.4: step 3 happens once, and an exempt run is never cancelled afterwards; step 4 bounds it, and a run still live then keeps its claim (reclaimed after `STALE_AFTER`). Cancelling it when its callout ends would turn a callout that succeeded into a FAILED task. The lead confirms and aligns §5.3.

4. **Build state across streams.** From stream S onwards `internal/scheduler`, `internal/cluster`, `app` and `internal/e2e` do not compile (`service.go:168, 178`, `service_test.go:186`, and `scheduler_rpc.go:57` against E's new `FireScheduledTransition`). R-1…R-9 verify with package tests, R-10 restores `app`, `cmd/cyoda` and `internal/cluster`, R-11 restores `internal/e2e`; `make test` runs from R-11. If stream E still leaves `workflow.WithExpiryGrace` in place, R-10 removes its call at `app/app.go:586` only; the option itself is E's.

5. **`Stop` is `Drain(context.Background())`.** `interfaces.md` calls `Stop` "best effort after servers (server-failure path)". §6.4's last paragraph says the same sequence runs there, so R makes `Stop` the full drain; after a `Drain` it does nothing.

6. **Validation versus the tuned test fixtures.** §11 requires `STALE_AFTER ≥ 50 s + 3 × HEARTBEAT_INTERVAL`, and `Config.Validate` runs in `app.New` (`app/app.go:103`). §13 wants parity and multi-node fixtures to set a short `STALE_AFTER`; the shortest valid one is 53 s (heartbeat 1 s). The M rows "owner killed → claimed after `STALE_AFTER`" therefore take about a minute each. R adds no test-only bypass (`.claude/rules`, no test hooks). The lead decides with stream T.

7. **Coverage handed to stream T.** R-11 deletes `TestE2E_SchedulerRPCPanic_RecoveredAndRolledBack` (rows 7a/7b of the tx-lifecycle suite: a panicking scheduled fire rolls back, `/readyz` 503, `/livez` 200), because its route is gone. The unit half is R-8. T should add the e2e form over a harness's own scheduler: a panicking criterion ends the task FAILED `RUN_PANICKED`, the entity stays in its state, `/readyz` answers 503 and `/livez` 200. That needs T's e2e isolation first: a harness's scheduler claims every harness's due tasks on the shared database (`ClaimDue` is cross-tenant). R-11 also removes the peer-RPC anti-vacuity check from `RunAttribution_ScheduledFire`; T should restore a deterministic "fired on a node other than the arming node" check for the claim model.

8. **Exit-check hits outside this stream.** `docs/ARCHITECTURE.md:943, :1992, :2248` (stream D) and `COMPATIBILITY.md:37`, a historical row of v0.8.3 that names `ScanDue` and `MarkRedispatch`. The lead decides whether §15 excludes `COMPATIBILITY.md` or the row is reworded.

9. **`RetireOwner` is withheld while any claim is kept.** §6.4 step 5 runs `RetireOwner` "only if no run is live". R also withholds it while a panicked or store-rejected run keeps its claim: retiring the owner would let another node take that task at once, where §6.5 says it is taken "after `STALE_AFTER` as a lost owner".

10. **Where the engine's own FAILED decision sits in the §5.6 order.** The table has no row for `RunReport.FailReason`. R places it right after "panicked": it is a decision taken before anything ran (§5.1), so no later row can apply.

11. **`app/config_dispatch_test.go`.** §6.6 lists the file for deletion; it also covers `CYODA_DISPATCH_WAIT_TIMEOUT` and `CYODA_DISPATCH_CONNECT_TIMEOUT`, which stay. R-10 removes only its forward-timeout parts.

12. **Help and README for the new settings are in R-10**, not D: `TestConfig_EnvVarCoverage` fails the moment `app/config.go` reads a name no `config/*.md` documents, and Gate 4 wants them in one change. Stream D keeps `run.md`, `telemetry.md`, `workflows.md`, the chart and `CHANGELOG.md`, and should not rewrite `config/scheduler.md` again.

13. **Choices not written in the spec**, for the reviewer: every loop store call (`ClaimDue`, `GiveBackIdle`, sweeps) has the 10 s budget of one §5.6 attempt; a heartbeat that returns success after `W` counts as failed (§6.3 counts `W` from the recorded start, so such a success proves nothing inside the window); give-backs and sweeps run only while the heartbeat is healthy, as claims do; the audit event's `attempts` is the claimed record's count.
