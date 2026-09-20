# Stream O — the owner's loop, what the client sees, wiring, observability

Packages: `internal/callout` (new), `internal/contract` (one file),
`internal/common` (one constant), `internal/domain/entity` (`classifyWorkflowError`),
`internal/domain/workflow` (`arm.go`, one wrap), `internal/observability`,
`internal/grpc` (tests' environment; the deletions), `internal/cluster/dispatch`
(the deletions), `app/app.go`, help topics, `docs/ARCHITECTURE.md`, `CHANGELOG.md`.
Spec §5, §8, D3, D5, D6, D7, D12, §12 (this stream's share), §13 (rows below).

The end-to-end scenarios (E, P, M layers) are stream **S**. This stream covers
the U cells of its rows, and the three G rows that need the whole loop.

## Verification points settled here

**V-2 — what happens to a retryable failure of an arming function.** Read:
`internal/domain/workflow/arm.go:234-245`, `engine.go:361, 457, 559`,
`fire_scheduled.go:487-489`, `internal/scheduler/executor.go:44-66`,
`internal/scheduler/service.go:174-213`.
`armViaFunction` returns the callout's error and `reconcileScheduledTasks` /
`Execute` pass it up; nothing on that path reads a `retryable` flag or a
cnode's verdict.
- *On a client operation* the save fails and rolls back; `classifyWorkflowError`
  decides the response — after O-2, `400 WORKFLOW_FAILED`, retryable when the
  cnode said so; a 503 with the try's own code otherwise.
- *On a scheduled fire* `FireScheduledTransition` returns
  `OutcomeDropped, "failed to reconcile scheduled tasks after fire: …"`, the fire's
  transaction rolls back, `LocalExecutor.Execute` logs it at ERROR and returns.
  The task row stays. The scan loop had already pushed its next dispatch out by
  `CYODA_SCHEDULER_REDISPATCH_BACKOFF` (`MarkRedispatch` runs *before* `Execute`),
  so the fire is tried again on a later scan **whatever the failure was** —
  retryable or not, there is no client to tell. This stream changes nothing
  under `internal/scheduler`.
- The one thing O-2 changes on this path is the *text*: `armViaFunction` wraps
  the error with the function's name, as the engine does for a processor
  (§8.2 "for a function the arming wrap" — no such wrap exists today; see Open
  points 2). The stale comment at `arm.go:243` ("already a classified AppError
  (503)") goes with it (§14).

**V-3 — how metrics are named and registered.** Read:
`internal/observability/dispatch_tracing.go:38-59`, `tx_tracing.go:32-42`,
`init.go:155-163`, `app/app.go:559-561`, `cmd/cyoda/help/content/telemetry.md:81-107`.
Callout metrics live on the `TracingExternalProcessingService` decorator, are
named `cyoda.<area>.<noun>` with dots, are created from `observability.Meter()`
with failures sent to `instrErr`, carry the callout kind as attribute `type`
(`processor|criteria|function`), and exist only when `CYODA_OTEL_ENABLED=true`
(the decorator is not installed otherwise). This stream follows that: three new
instruments on the same decorator — `cyoda.callout.tries` (`type`, `outcome`),
`cyoda.callout.handovers` (`outcome`), `cyoda.callout.wait.duration` (`type`;
recorded only for a callout that waited, so its count is "callouts that
waited"). The decorator sees only `(result, error)`, so the owner's loop reports
through a value the decorator puts on the context
(`contract.WithCalloutStats`) — the same shape as `common.AddWarning`'s
request-scoped accumulator, and no OpenTelemetry import in `internal/callout`.
The `CALLOUT_SUPERSEDED` counter is stream F's and the list-send counters are
stream M's; neither can be seen from this decorator.

## Facts this stream relies on, read in the code

- `internal/grpc` test files are `package grpc` (51 of 52), and
  `internal/callout` imports `internal/grpc`: an in-package test there cannot
  import the Coordinator. An *external* test file in the same directory can, and
  both are linked into one test binary — checked with a three-file experiment
  outside the repository. O-7 uses that: `export_test.go` declares a variable,
  `owner_wiring_test.go` (`package grpc_test`) sets it from `init`.
- `app/app.go:644` creates the `txgate.Registry` *after* the dispatcher is wired
  (`:524-561`). The Coordinator needs the fence, and the fence needs the gate:
  O-8 moves both above the wiring block if stream F has not already.
- A `MemberFailed` failure reaches `classifyWorkflowError` as
  `processor charge failed: card declined` — `CalloutFailure.Error()` is the
  cnode's text because `Err` is nil (L-1, L-7). The inner
  `processor dispatch failed:` segment is already gone after L-7; O-2 changes
  the flag, not the text.
- Cyoda Cloud writes the member in literal angle brackets —
  `"member<${it.memberId ?: "-"}>: ${it.exception}"`, grouped with
  `groupingBy/eachCount`, which keeps first-occurrence order
  (`~/dev/cyoda/backend/src/main/kotlin/net/cyoda/saas/externalize/ExternalizerBase.kt:535-546`).
  O-1 renders `[member<m-1>: cause]` and `[member<->: cause]` accordingly.
- `errors.md:34-47` is wrong twice: no production code sets gRPC trailers
  (`grep -rn 'SetTrailer' internal/` → no hits), and the envelope's `code` is
  the coarse `CLIENT_ERROR` / `SERVER_ERROR` with the precise code as the
  message's prefix and `retryable` present only when true
  (`internal/grpc/errors.go:24-35`). O-9 corrects both.

## Order and dependencies

```
O-1 ── O-2 ── O-3 ── O-4 ── O-5 ── O-6 ── O-7 ── O-8 ── O-9
       │       │             │             │      │
       L-1,L-7 L-1…L-10, F   P (the seam)  F      P (PeerRouter in app.go), F, C-1
               C-1 (SPI: Idempotent, ScheduleFunction.RetryPolicy), C-5
```

O-8 must land **before** stream F's enforcement in `txjoin.JoinFromToken`
becomes active, or together with it (L's Open point 4): until the Coordinator is
the `ExternalProcessingService`, nothing calls `fence.Begin`, and every callback
would be refused. After O-8 stream L can run L-11 (once P is done), and stream C
can run C-11.

**How far this draft was checked.** Outside the repository:
- The loop, the exhaustion message and the fence interplay (`coordinator.go`,
  `failure.go` exactly as printed in O-5/O-6) were compiled and run under
  `-race` against stream F's draft `internal/fence`, the real `internal/txgate`
  and `internal/common`, and **stand-ins** for stream L's and stream P's types
  (a scripted `RunLocal`, a scripted `PeerRouter`): exhaustion message, one
  attempt not wrapped, precedence, cumulative patience, patience 0, cancellation
  during a wait, release by the fence, hand-over numbering and statistics, a
  lost answer under both settings, a panic.
- O-2 (classifier, `arm.go`, the engine-mode tests), O-6's decorator and its
  tests, and O-1's help topic with `TestErrCode_Parity` were applied to a copy
  of the worktree with L-1's `contract/callout.go` added: RED and GREEN observed,
  and `internal/domain/workflow`, `internal/domain/entity`, `internal/scheduler`,
  `internal/cluster/...`, `internal/observability`, `cmd/cyoda/help` green.
- The Coordinator's unit tests as printed (O-3 … O-5) run against the real
  `*grpc.ProcessorDispatcher`, which needs all of stream L applied; they were
  **not compiled**. Neither were O-7 and O-8.

---

### Task O-1: `CALLOUT_FAILED`, and the error for a callout that recorded attempts

**Spec:** §8.2 rows "Every try used, more than one attempt" and "Every try used, exactly one attempt recorded"; §5 "Precedence when nothing more can be done" (the attempts half); §8.2 "Member ids appear in client-visible text … Node ids and peer addresses do not"; R§5 (Cloud's shape). §13 U cells: "Every try used → 503 `CALLOUT_FAILED`, message shape", "Exactly one attempt recorded → not wrapped" (the rendering; the loop's use of it is O-3).

**Files:**
- Create: `internal/callout/failure.go`
- Modify: `internal/common/error_codes.go` (the dispatch block, `:73-78`)
- Create: `cmd/cyoda/help/content/errors/CALLOUT_FAILED.md`
- Modify: `cmd/cyoda/help/content/errors.md` (ERROR CODE INDEX: one row, after `errors.BAD_REQUEST`)
- Test: `internal/callout/failure_test.go`

**Interfaces:**
- Consumes: L-1 `contract.CalloutFailure`, `contract.CalloutAttempt`, `contract.CalloutFailureKind`.
- Produces:
  - `const common.ErrCodeCalloutFailed = "CALLOUT_FAILED"`
  - `func attemptsFailure(attempts []contract.CalloutAttempt, last *contract.CalloutFailure) *contract.CalloutFailure` (unexported; `len(attempts) ≥ 1`, `last` is the failure of the latest try made)
  - `func withAttempts(failure *contract.CalloutFailure, attempts []contract.CalloutAttempt) *contract.CalloutFailure` (a copy; the try's own value is never written to)
  - `func attemptsMessage(attempts []contract.CalloutAttempt) string`
  - For stream **S**: the client message is exactly
    `CALLOUT_FAILED: the callout could not be completed, got N failures: [member<ID>: CAUSE], [member<ID>: CAUSE (k times)]` — angle brackets literal, `N` counted before collapsing, entries in order of first occurrence, `CAUSE` the try's own `CODE: text`, `member<->` for a hand-over whose answer was lost.

- [ ] **Step 1: Write the failing tests**

`internal/callout/failure_test.go`:

```go
package callout

import (
	"errors"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

const timeoutCause = "DISPATCH_TIMEOUT: processor dispatch timed out after 100ms: no response"

func timedOut(memberID string) (contract.CalloutAttempt, *contract.CalloutFailure) {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
		"processor dispatch timed out after 100ms: no response").AsRetryable()
	failure := &contract.CalloutFailure{Kind: contract.NoAnswer, Code: appErr.Code, Message: appErr.Message, Err: appErr}
	return contract.CalloutAttempt{MemberID: memberID, Kind: contract.NoAnswer, Cause: failure.Message}, failure
}

func TestAttemptsMessage(t *testing.T) {
	gone := "COMPUTE_MEMBER_DISCONNECTED: compute member disconnected during processor dispatch"
	lost := "DISPATCH_FORWARD_FAILED: forwarding the callout to a peer node failed"
	tests := []struct {
		name     string
		attempts []contract.CalloutAttempt
		want     string
	}{
		{
			name: "two different entries",
			attempts: []contract.CalloutAttempt{
				{MemberID: "m-1", Kind: contract.NoAnswer, Cause: timeoutCause},
				{MemberID: "m-2", Kind: contract.NoAnswer, Cause: gone},
			},
			want: "the callout could not be completed, got 2 failures: [member<m-1>: " + timeoutCause + "], [member<m-2>: " + gone + "]",
		},
		{
			name: "identical entries collapse, the count is taken before collapsing, first occurrence keeps its place",
			attempts: []contract.CalloutAttempt{
				{MemberID: "m-1", Kind: contract.NoAnswer, Cause: timeoutCause},
				{MemberID: "m-2", Kind: contract.NoAnswer, Cause: gone},
				{MemberID: "m-1", Kind: contract.NoAnswer, Cause: timeoutCause},
				{MemberID: "m-1", Kind: contract.NoAnswer, Cause: timeoutCause},
			},
			want: "the callout could not be completed, got 4 failures: [member<m-1>: " + timeoutCause + " (3 times)], [member<m-2>: " + gone + "]",
		},
		{
			name: "the same member with a different cause is a different entry",
			attempts: []contract.CalloutAttempt{
				{MemberID: "m-1", Kind: contract.NoAnswer, Cause: timeoutCause},
				{MemberID: "m-1", Kind: contract.NoHandOff, Cause: gone},
			},
			want: "the callout could not be completed, got 2 failures: [member<m-1>: " + timeoutCause + "], [member<m-1>: " + gone + "]",
		},
		{
			name: "a lost hand-over answer has no member",
			attempts: []contract.CalloutAttempt{
				{MemberID: "m-1", Kind: contract.NoAnswer, Cause: timeoutCause},
				{MemberID: "-", Kind: contract.NoAnswer, Cause: lost},
				{MemberID: "-", Kind: contract.NoAnswer, Cause: lost},
			},
			want: "the callout could not be completed, got 3 failures: [member<m-1>: " + timeoutCause + "], [member<->: " + lost + " (2 times)]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := attemptsMessage(tt.attempts); got != tt.want {
				t.Errorf("attemptsMessage\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestAttemptsFailure_MoreThanOneAttempt_IsCalloutFailed(t *testing.T) {
	a1, _ := timedOut("m-1")
	a2, last := timedOut("m-2")
	attempts := []contract.CalloutAttempt{a1, a2}

	failure := attemptsFailure(attempts, last)

	var appErr *common.AppError
	if !errors.As(failure, &appErr) {
		t.Fatalf("no *AppError behind %v", failure)
	}
	if appErr.Status != http.StatusServiceUnavailable || appErr.Code != common.ErrCodeCalloutFailed || !appErr.Retryable {
		t.Errorf("got %d %s retryable=%v, want a retryable 503 CALLOUT_FAILED", appErr.Status, appErr.Code, appErr.Retryable)
	}
	if want := common.ErrCodeCalloutFailed + ": " + attemptsMessage(attempts); appErr.Message != want {
		t.Errorf("message = %q, want %q", appErr.Message, want)
	}
	if failure.Kind != contract.NoAnswer || failure.Code != common.ErrCodeCalloutFailed || len(failure.Attempts) != 2 {
		t.Errorf("failure = %+v, want the last try's kind, the new code and both attempts", failure)
	}
}

func TestAttemptsFailure_ExactlyOneAttempt_IsThatAttemptsOwnError(t *testing.T) {
	attempt, last := timedOut("m-1")

	failure := attemptsFailure([]contract.CalloutAttempt{attempt}, last)

	var appErr *common.AppError
	if !errors.As(failure, &appErr) || appErr.Code != common.ErrCodeDispatchTimeout {
		t.Fatalf("got %v, want the attempt's own DISPATCH_TIMEOUT, not wrapped", failure)
	}
	if appErr.Message != timeoutCause {
		t.Errorf("message = %q, want it unchanged: %q", appErr.Message, timeoutCause)
	}
	if len(failure.Attempts) != 1 || failure.Attempts[0].MemberID != "m-1" {
		t.Errorf("attempts = %+v, want the one attempt on the returned failure", failure.Attempts)
	}
	if last.Attempts != nil {
		t.Error("the try's own failure value must not be written to")
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/callout/...`   Expected: FAIL (build) with `undefined: attemptsMessage`, `undefined: attemptsFailure`, `undefined: common.ErrCodeCalloutFailed`.

- [ ] **Step 3: Implement**

`internal/common/error_codes.go` — in the dispatch block, before:

```go
	ErrCodeComputeMemberDisconnected = "COMPUTE_MEMBER_DISCONNECTED"
)
```

after:

```go
	ErrCodeComputeMemberDisconnected = "COMPUTE_MEMBER_DISCONNECTED"
	ErrCodeCalloutFailed             = "CALLOUT_FAILED"
)
```

`internal/callout/failure.go`:

```go
package callout

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// attemptsFailure is the error for a callout that recorded attempts and can do
// nothing more: every try is used, or the patience or the callout's deadline
// ran out with tries still left.
//
// Exactly one attempt is reported as itself — last is that attempt's failure,
// returned with its own code and not wrapped. More than one becomes
// CALLOUT_FAILED, a retryable 503 that lists them.
func attemptsFailure(attempts []contract.CalloutAttempt, last *contract.CalloutFailure) *contract.CalloutFailure {
	if len(attempts) == 1 {
		return withAttempts(last, attempts)
	}
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeCalloutFailed, attemptsMessage(attempts)).AsRetryable()
	return &contract.CalloutFailure{
		Kind:     last.Kind,
		Code:     appErr.Code,
		Message:  appErr.Message,
		Attempts: attempts,
		Err:      appErr,
	}
}

// withAttempts returns a copy of failure that carries the callout's attempts.
func withAttempts(failure *contract.CalloutFailure, attempts []contract.CalloutAttempt) *contract.CalloutFailure {
	out := *failure
	out.Attempts = attempts
	return &out
}

// attemptsMessage renders the attempts the way Cyoda Cloud renders an
// exhausted retry: the count is taken before identical entries are collapsed,
// an entry that occurs k > 1 times is written once with "(k times)", and
// entries keep the order in which they first occurred. A member id of "-" is a
// hand-over whose answer was lost.
//
// Member ids identify the tenant's own connections and are already known to
// the cnode from its greet. Node ids and peer addresses never appear: an
// attempt's Cause is client-safe text by contract.
func attemptsMessage(attempts []contract.CalloutAttempt) string {
	counts := make(map[string]int, len(attempts))
	var order []string
	for _, a := range attempts {
		entry := fmt.Sprintf("member<%s>: %s", a.MemberID, a.Cause)
		if counts[entry] == 0 {
			order = append(order, entry)
		}
		counts[entry]++
	}
	parts := make([]string, 0, len(order))
	for _, entry := range order {
		if n := counts[entry]; n > 1 {
			entry = fmt.Sprintf("%s (%d times)", entry, n)
		}
		parts = append(parts, "["+entry+"]")
	}
	return fmt.Sprintf("the callout could not be completed, got %d failures: %s", len(attempts), strings.Join(parts, ", "))
}
```

`cmd/cyoda/help/content/errors/CALLOUT_FAILED.md`:

````markdown
---
topic: errors.CALLOUT_FAILED
title: "CALLOUT_FAILED — every try of a processor, criterion or function callout failed"
stability: stable
see_also:
  - errors
  - errors.DISPATCH_TIMEOUT
  - errors.COMPUTE_MEMBER_DISCONNECTED
  - errors.DISPATCH_FORWARD_FAILED
  - errors.NO_COMPUTE_MEMBER_FOR_TAG
  - workflows
  - config.grpc
---

# errors.CALLOUT_FAILED

## NAME

CALLOUT_FAILED — a processor, criterion or function callout was tried on more than one compute member, and no try produced an answer.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

A callout is tried on one compute member after another: until one answers, until a failure forbids another try, or until the tries its `retryPolicy` allows are used (`NONE`: one try; `FIXED` or unset: one try plus `CYODA_RETRY_FIXED_NUM_RETRIES`). This code is returned when more than one try failed and nothing more could be done — every try was used, or no further compute member appeared within `CYODA_DISPATCH_WAIT_TIMEOUT`, or the time the callout may take ran out.

The message lists the failed tries:

```
CALLOUT_FAILED: the callout could not be completed, got 3 failures: [member<5f0c…>: DISPATCH_TIMEOUT: processor dispatch timed out after 30000ms: no response (2 times)], [member<->: DISPATCH_FORWARD_FAILED: forwarding the callout to a peer node failed]
```

- The count is the number of failed tries. Identical entries are written once, with `(k times)`.
- `member<id>` is the id of the compute member that was tried — one of the caller's own tenant's connections, the id the member was given when it joined. `member<->` is a try handed over to another cluster node whose answer was lost, so that the member is not known.
- Each entry carries the error code the try would have had on its own.

When exactly one try was made and failed, that try's own code is returned instead (`errors.DISPATCH_TIMEOUT`, `errors.COMPUTE_MEMBER_DISCONNECTED`, `errors.DISPATCH_FORWARD_FAILED`), not this one. When no try could be made at all, the code is `errors.NO_COMPUTE_MEMBER_FOR_TAG`. A compute member that answered with a failure of its own is never replaced by another: that is `errors.WORKFLOW_FAILED`.

A callout reaches a second compute member only where that is safe for Cyoda's own state: always when the work provably never left the node, and after the work was handed to a member only for a criterion, a function, or a processor whose configuration declares `idempotent: true`.

Retryable, and `retryable: true` speaks for Cyoda's state only. With a `SYNC` or `ASYNC_SAME_TX` processor the operation failed and its transaction was rolled back, so running it again starts clean — unless an earlier `COMMIT_BEFORE_DISPATCH` processor of the same request had already committed. With a `COMMIT_BEFORE_DISPATCH` processor the part of the operation before the callout stays committed. A compute member that was handed the work may have carried it out; what it did outside Cyoda is the application's to reconcile.

## SEE ALSO

- errors
- errors.DISPATCH_TIMEOUT
- errors.COMPUTE_MEMBER_DISCONNECTED
- errors.DISPATCH_FORWARD_FAILED
- errors.NO_COMPUTE_MEMBER_FOR_TAG
- workflows
- config.grpc
````

`cmd/cyoda/help/content/errors.md`, ERROR CODE INDEX — after the `errors.BAD_REQUEST` row:

```markdown
- `errors.CALLOUT_FAILED` — `503` — retryable — a processor, criterion or function callout was tried on more than one compute member and no try produced an answer; the message lists the tries
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/callout/... ./internal/common/... ./cmd/cyoda/help/...`   Expected: PASS — `TestErrCode_Parity` finds the constant and its topic; the topic's `see_also` entries all exist (`config.grpc` is `content/config/grpc.md`).

- [ ] **Step 5: Commit**

```
git add internal/callout/failure.go internal/callout/failure_test.go internal/common/error_codes.go cmd/cyoda/help/content/errors/CALLOUT_FAILED.md cmd/cyoda/help/content/errors.md
git commit -m "feat(callout): CALLOUT_FAILED lists the tries of a callout that ran out of them (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task O-2: a cnode's own verdict reaches the client; the arming function's failure names the function

**Spec:** §8.2 rows "`MemberFailed`, verdict true" and "verdict false or absent", and the paragraph "`classifyWorkflowError` gains one branch, before the catch-all"; D12; §8.1 in full (the table, by mode) and **V-2**; §14 "Stale comments: … `arm.go:243`". §13 U cells: "`MemberFailed` verdict true → 400 retryable" and "verdict false / absent" (the classification half — the one-try half is L-8's), "`ASYNC_NEW_TX`: callout fails → operation succeeds, nothing reported", "`COMMIT_BEFORE_DISPATCH`, both variants: failure leaves TX_pre committed"; G cell of "`MemberFailed` verdict true → 400 retryable".

**Files:**
- Modify: `internal/domain/entity/service.go` (`classifyWorkflowError`, `:2848-2909`)
- Modify: `internal/domain/workflow/arm.go` (`armViaFunction`, `:242-244`)
- Modify: `internal/grpc/callout_failure_rpc_test.go` (L-7's `TestRPC_ProcessorMemberFailed_EnvelopeCarriesTheMemberMessage`: the one assertion L left to this stream)
- Test: `internal/domain/entity/service_classify_member_failed_test.go`, `internal/domain/workflow/callout_failure_modes_test.go`

**Interfaces:**
- Consumes: L-1 `contract.CalloutFailure{Kind, Message, Retryable, Err}`; L-7 (a `MemberFailed` failure has `Err == nil`, so `Error()` is the cnode's text).
- Produces: for every entry point that classifies a workflow error (HTTP create, create-collection, update, transition, loopback; the two transitions read endpoints; gRPC `EntityManage` / `EntityManageCollection` through `buildErrorFields`): a `MemberFailed` failure is `400 WORKFLOW_FAILED`, `retryable: true` exactly when the cnode's verdict was `true`. Message shapes — processor: `WORKFLOW_FAILED: processor <name> failed: <cnode message>`; transition criterion: `WORKFLOW_FAILED: failed to evaluate transition criterion: <cnode message>`; workflow criterion: `WORKFLOW_FAILED: failed to evaluate workflow criterion for "<wf>": <cnode message>`; arming function: `WORKFLOW_FAILED: schedule function <name> failed: <cnode message>`.

**V-2**, stated: see "Verification points settled here" at the top. Nothing under `internal/scheduler` changes.

**Existing tests:** `TestClassifyWorkflowError_ProcessorFailureStays4xx` (`service_classify_test.go:76`) — stays: a plain error is still a non-retryable 400. `TestClassifyWorkflowError_NoMatchingMemberMapsTo503` (`:358`) — stays: `CalloutFailure.Unwrap` keeps `errors.Is(err, contract.ErrNoMatchingMember)` true. `TestReconcile_FunctionDispatchFailureFailsWrite` (`arm_function_test.go:351`) — stays: it asserts only that the write fails and nothing is armed. `TestAsyncNewTxFailureDoesNotKillPipeline` (`engine_test.go:1627`), `TestExecuteCommitBeforeDispatch_EveryFailurePathRollsBack` (`engine_segment_guard_test.go:176`), `TestEngine_CBD_FollowedBySyncFailure_RollsBackPostSegment` (`engine_test.go:3143`) — stay.

- [ ] **Step 1: Write the failing tests**

`internal/domain/entity/service_classify_member_failed_test.go`:

```go
package entity

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// A cnode that answered "I failed" is a 400 WORKFLOW_FAILED whose message is the
// engine's wrap plus the cnode's own text, and which is retryable exactly when
// the cnode said so.
func TestClassifyWorkflowError_MemberFailed_CarriesTheVerdict(t *testing.T) {
	yes, no := true, false
	verdicts := []struct {
		name      string
		verdict   *bool
		retryable bool
	}{
		{"verdict true", &yes, true},
		{"verdict false", &no, false},
		{"verdict absent", nil, false},
	}
	wraps := []struct {
		name string
		wrap func(error) error
		want string
	}{
		{"processor", func(err error) error { return fmt.Errorf("processor %s failed: %w", "charge", err) },
			"WORKFLOW_FAILED: processor charge failed: card declined"},
		{"transition criterion", func(err error) error { return fmt.Errorf("failed to evaluate transition criterion: %w", err) },
			"WORKFLOW_FAILED: failed to evaluate transition criterion: card declined"},
		{"workflow criterion", func(err error) error {
			return fmt.Errorf("failed to evaluate workflow criterion for %q: %w", "orders", err)
		}, `WORKFLOW_FAILED: failed to evaluate workflow criterion for "orders": card declined`},
		{"function", func(err error) error { return fmt.Errorf("schedule function %s failed: %w", "calcFire", err) },
			"WORKFLOW_FAILED: schedule function calcFire failed: card declined"},
	}
	for _, v := range verdicts {
		for _, w := range wraps {
			t.Run(v.name+"/"+w.name, func(t *testing.T) {
				failure := &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", Retryable: v.verdict}
				appErr := classifyWorkflowError(w.wrap(failure))
				if appErr.Status != http.StatusBadRequest || appErr.Code != common.ErrCodeWorkflowFailed {
					t.Fatalf("got %d %s, want 400 WORKFLOW_FAILED", appErr.Status, appErr.Code)
				}
				if appErr.Retryable != v.retryable {
					t.Errorf("Retryable = %v, want %v", appErr.Retryable, v.retryable)
				}
				if appErr.Message != w.want {
					t.Errorf("Message = %q, want %q", appErr.Message, w.want)
				}
			})
		}
	}
}

// Every other kind already carries its classified error and passes through
// unchanged — code, status and retryable flag are the try's own.
func TestClassifyWorkflowError_OtherCalloutFailureKindsPassThrough(t *testing.T) {
	timeout := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout, "processor dispatch timed out after 100ms: no response").AsRetryable()
	failure := &contract.CalloutFailure{Kind: contract.NoAnswer, Code: timeout.Code, Message: timeout.Message, Err: timeout}

	got := classifyWorkflowError(fmt.Errorf("processor %s failed: %w", "charge", failure))
	if got != timeout {
		t.Fatalf("got %+v, want the try's own *AppError unchanged", got)
	}
}

// A Terminal failure with no code of its own stays what it was: a
// non-retryable 400 WORKFLOW_FAILED.
func TestClassifyWorkflowError_TerminalWithoutCodeStays400NotRetryable(t *testing.T) {
	cause := fmt.Errorf("failed to unmarshal processor response payload: unexpected end of JSON input")
	failure := &contract.CalloutFailure{Kind: contract.Terminal, Message: cause.Error(), Err: cause}

	got := classifyWorkflowError(fmt.Errorf("processor %s failed: %w", "charge", failure))
	if got.Status != http.StatusBadRequest || got.Code != common.ErrCodeWorkflowFailed || got.Retryable {
		t.Fatalf("got %d %s retryable=%v, want a non-retryable 400 WORKFLOW_FAILED", got.Status, got.Code, got.Retryable)
	}
}
```

`internal/domain/workflow/callout_failure_modes_test.go`. Of its four tests only the last is RED before Step 3; the first three hold the §8.1 table against a `*contract.CalloutFailure` travelling through the engine in every mode, and pass before and after — they are here to pin the rows, not to drive code:

```go
package workflow

import (
	"context"
	"errors"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/testing/localproc"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func memberFailed(message string, verdict bool) *contract.CalloutFailure {
	return &contract.CalloutFailure{Kind: contract.MemberFailed, Message: message, Retryable: &verdict}
}

func noAnswerFailure() *contract.CalloutFailure {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
		"processor dispatch timed out after 100ms: no response").AsRetryable()
	return &contract.CalloutFailure{Kind: contract.NoAnswer, Code: appErr.Code, Message: appErr.Message, Err: appErr}
}

// calloutModeEngine runs one transition with one externalized processor in the
// given mode, whose callout fails with failure.
func calloutModeEngine(t *testing.T, mode string, startNewTx *bool, failure error) (result *EngineResult, execErr error, cnt *segCounter, entity *spi.Entity) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	cnt = &segCounter{}
	txMgr := &countingTxManager{inner: factory.NewTransactionManager(uuids), c: cnt}
	mock := &mockExternalProcessing{
		dispatchFunc: func(context.Context, *spi.Entity, spi.ProcessorDefinition, string, string, string) (*spi.Entity, error) {
			return nil, failure
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(mock))

	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "callout-mode-" + mode, ModelVersion: "1.0"}
	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "CalloutModeWF", InitialState: "OPEN", Active: true,
		States: map[string]spi.StateDefinition{
			"OPEN": {Transitions: []spi.TransitionDefinition{
				{Name: "GO", Next: "DONE", Manual: false,
					Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "charge", ExecutionMode: mode,
							Config: spi.ProcessorConfig{StartNewTxOnDispatch: startNewTx}},
					}},
			}},
			"DONE": {},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})

	entryTxID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = txMgr.Rollback(txCtx, entryTxID) })
	*cnt = segCounter{}

	entity = &spi.Entity{
		Meta: spi.EntityMeta{ID: "callout-mode-1", TenantID: testTenant, ModelRef: modelRef, TransactionID: entryTxID},
		Data: []byte(`{"x":1}`),
	}
	result, execErr = engine.Execute(txCtx, entity, "")
	return result, execErr, cnt, entity
}

// SYNC and ASYNC_SAME_TX: the operation fails, the engine's wrap names the
// processor, and the failure — kind, the cnode's text, its verdict — is still
// there for the classifier behind the wrap.
func TestCalloutFailure_SyncModes_FailTheOperationAndKeepTheFailure(t *testing.T) {
	for _, mode := range []string{ExecutionModeSync, ExecutionModeAsyncSameTx} {
		t.Run(mode, func(t *testing.T) {
			_, err, cnt, _ := calloutModeEngine(t, mode, nil, memberFailed("card declined", true))
			if err == nil {
				t.Fatal("expected the operation to fail")
			}
			if got, want := err.Error(), "processor charge failed: card declined"; got != want {
				t.Errorf("error = %q, want %q", got, want)
			}
			var failure *contract.CalloutFailure
			if !errors.As(err, &failure) || failure.Kind != contract.MemberFailed || failure.Retryable == nil || !*failure.Retryable {
				t.Errorf("failure = %+v, want MemberFailed with verdict true behind the wrap", failure)
			}
			if cnt.commits != 0 {
				t.Errorf("engine committed %d times, want 0: nothing of a failed operation is committed", cnt.commits)
			}
		})
	}
}

// ASYNC_NEW_TX: the operation continues and nothing is reported — not even a
// retryable verdict.
func TestCalloutFailure_AsyncNewTx_OperationContinuesNothingReported(t *testing.T) {
	for name, failure := range map[string]error{
		"member failed, verdict true": memberFailed("card declined", true),
		"no answer":                   noAnswerFailure(),
	} {
		t.Run(name, func(t *testing.T) {
			result, err, _, entity := calloutModeEngine(t, ExecutionModeAsyncNewTx, nil, failure)
			if err != nil {
				t.Fatalf("Execute = %v, want success: an ASYNC_NEW_TX callout failure does not fail the operation", err)
			}
			if !result.Success || entity.Meta.State != "DONE" {
				t.Errorf("success = %v state = %q, want true and DONE", result.Success, entity.Meta.State)
			}
		})
	}
}

// COMMIT_BEFORE_DISPATCH, both variants: the operation fails with the try's own
// error, and TX_pre stays committed.
func TestCalloutFailure_CommitBeforeDispatch_FailsAndLeavesTxPreCommitted(t *testing.T) {
	yes, no := true, false
	for name, startNewTx := range map[string]*bool{"new tx on dispatch": &yes, "no tx on dispatch": &no} {
		t.Run(name, func(t *testing.T) {
			_, err, cnt, _ := calloutModeEngine(t, ExecutionModeCommitBeforeDispatch, startNewTx, noAnswerFailure())
			var appErr *common.AppError
			if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeDispatchTimeout || !appErr.Retryable {
				t.Fatalf("error = %v, want the try's own retryable DISPATCH_TIMEOUT", err)
			}
			if cnt.commits != 1 {
				t.Errorf("engine commits = %d, want 1: TX_pre is committed before the callout and stays committed", cnt.commits)
			}
		})
	}
}

// The arming function's failure names the function, as a processor's names the
// processor, and keeps the cnode's verdict.
func TestReconcile_FunctionMemberFailed_NamesTheFunctionAndKeepsTheVerdict(t *testing.T) {
	const nowMs = int64(1_700_000_000_000)

	lp := localproc.New()
	lp.RegisterFunction("calcFire", func(context.Context, *spi.Entity, spi.ScheduleFunction) (contract.FunctionResult, error) {
		return contract.FunctionResult{}, memberFailed("rates service is down", true)
	})
	engine, factory := setupEngineWithClockAndExtProc(t, nowMs, lp)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "fn-member-failed-order", ModelVersion: "1.0"}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{scheduleFunctionWorkflow("FnMemberFailedWF", "calcFire")})

	_, err := engine.Execute(ctx, makeEntity("fn-member-failed-e1", modelRef, map[string]any{}), "")
	if err == nil {
		t.Fatal("expected the write to fail")
	}
	if got, want := err.Error(), "schedule function calcFire failed: rates service is down"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) || failure.Retryable == nil || !*failure.Retryable {
		t.Errorf("failure = %+v, want the verdict kept behind the wrap", failure)
	}
}
```

`internal/grpc/callout_failure_rpc_test.go` — in `TestRPC_ProcessorMemberFailed_EnvelopeCarriesTheMemberMessage`, replace the loop header and add the assertion after the `asked.Load()` check:

```go
	yes, no := true, false
	for name, tc := range map[string]struct {
		verdict   *bool
		retryable bool
	}{
		"verdict true":   {&yes, true},
		"verdict false":  {&no, false},
		"verdict absent": {nil, false},
	} {
		verdict := tc.verdict
		t.Run(name, func(t *testing.T) {
```

```go
			if got := typed.Error.Retryable != nil && *typed.Error.Retryable; got != tc.retryable {
				t.Errorf("envelope retryable = %v, want %v: the cnode's verdict decides it", got, tc.retryable)
			}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/domain/entity/... -run 'TestClassifyWorkflowError_MemberFailed'`   Expected: FAIL, four subtests, `Retryable = false, want true` (the `verdict true` cases).
Run: `go test ./internal/domain/workflow/... -run 'TestReconcile_FunctionMemberFailed'`   Expected: FAIL with `error = "rates service is down", want "schedule function calcFire failed: rates service is down"`.
Run: `go test ./internal/grpc/... -run 'TestRPC_ProcessorMemberFailed'`   Expected: FAIL, `verdict true`: `envelope retryable = false, want true`.

- [ ] **Step 3: Implement**

`internal/domain/entity/service.go`, `classifyWorkflowError` — before:

```go
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return common.Internal("workflow aborted by context cancellation", err)
	}
	return common.Operational(http.StatusBadRequest, common.ErrCodeWorkflowFailed, err.Error())
}
```

after:

```go
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return common.Internal("workflow aborted by context cancellation", err)
	}
	// A cnode that answered "I failed". Its own message reaches the client
	// behind the engine's wrap, which names the processor; its verdict decides
	// whether the client is told that running the operation again may help.
	// The verdict never decides whether another cnode is tried — a cnode that
	// answered is never replaced. Every other kind of callout failure carries
	// an *AppError and left through the first branch.
	var failure *contract.CalloutFailure
	if errors.As(err, &failure) && failure.Kind == contract.MemberFailed {
		appErr := common.Operational(http.StatusBadRequest, common.ErrCodeWorkflowFailed, err.Error())
		if failure.Retryable != nil && *failure.Retryable {
			appErr = appErr.AsRetryable()
		}
		return appErr
	}
	return common.Operational(http.StatusBadRequest, common.ErrCodeWorkflowFailed, err.Error())
}
```

`internal/domain/workflow/arm.go`, `armViaFunction` — before:

```go
	if derr != nil {
		return nil, nil, derr // already a classified AppError (503) — fails the write, fail-closed
	}
```

after:

```go
	if derr != nil {
		// Fails the write, fail-closed. The wrap names the function for the one
		// failure whose text reaches the client through it — a compute node
		// that answered "failed"; every other callout failure carries its own
		// classified error, which the wrap leaves intact.
		return nil, nil, fmt.Errorf("schedule function %s failed: %w", tr.Schedule.Function.Name, derr)
	}
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/domain/entity/... ./internal/domain/workflow/... ./internal/grpc/... ./internal/scheduler/... ./internal/cluster/...`   Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/domain/entity/service.go internal/domain/entity/service_classify_member_failed_test.go internal/domain/workflow/arm.go internal/domain/workflow/callout_failure_modes_test.go internal/grpc/callout_failure_rpc_test.go
git commit -m "feat(entity): a compute member's retryable verdict reaches the client on WORKFLOW_FAILED (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task O-3: the `Coordinator` — tries, one request id, the fence, what stops a callout

**Spec:** §5 pseudo-code lines "resolve tries … create RequestID", "`fence.Begin` … `defer end()`", "`r := local.RunLocal`" through "`if triesLeft <= 0 -> return exhaustion`"; §5 "Precedence when nothing more can be done"; §5 "`context.Cause` tells the fence's cancellation (§7) from both" and "A cancelled caller ends the loop at once … the callout's `end` still runs"; D1, D2 (`RepeatSafe`), D3, D6 (first half); §4 "On the owner the `Coordinator` supplies it: `Next` raises `major`, calls `Fence.Advance`"; §7 "The Coordinator holds the one `major` counter of a callout". §13 U cells: "`retryPolicy: NONE` on a processor, a criterion, a function → one try", "Every try used → 503 `CALLOUT_FAILED`", "Exactly one attempt recorded → not wrapped", "Attempts on record beat "no cnode"", "Same request id on every try" (the owner's half), "Callouts made from a scheduled fire follow the same rules", "Two tenants share a tag…" (owner's view), "Owner gives the work to a second cnode of its own → the first cnode's callback is refused at once" (the `Advance` order), "A Coordinator released by the fence reports `CALLOUT_SUPERSEDED`, a client that went away as today" (during a try), "Client timeout (408) / cancellation … during a try".

**Files:**
- Create: `internal/callout/coordinator.go`, `internal/callout/entry.go`
- Create: `internal/contract/callout_stats.go`
- Modify: `internal/grpc/callout.go` (`Callout.RetryPolicy`; the three builders set it)
- Test: `internal/callout/helpers_test.go`, `internal/callout/coordinator_test.go`, `internal/grpc/callout_test.go` (one test added)

**Interfaces:**
- Consumes:
  - L: `grpc.NewProcessorCallout`, `grpc.NewCriteriaCallout`, `grpc.NewFunctionCallout`, `grpc.Callout{RequestID, AnswerLimit, RepeatSafe, OwnerNodeID, Number, Outer, TxID, TenantID, Tags, Kind, Name, EntityID, ResponseTimeoutMs}`, `grpc.CalloutResult{Entity, Matches, Reason, Function}`, `(*grpc.ProcessorDispatcher).RunLocal`, `.ResolveAnswerLimit`, `grpc.LocalResult{Result, Failure, CtxErr, TriesUsed, Attempts}`, `grpc.TryNumberer`, `grpc.NewRoundRobinSelector`, `grpc.NewProcessorDispatcher(registry, selector, uuids, signer, selfNodeID, answerLimitDefault, answerLimitMax, passAllowance)`.
  - F: `fence.New`, `(*Fence).Begin`, `(*Fence).Advance`, `(*Fence).Admit`, `fence.Pairs`, `fence.Pair{Callout, Major, Minor}`, `fence.ErrSuperseded`, **`fence.NewSupersededError() *common.AppError`** (410 `CALLOUT_SUPERSEDED`, cause `ErrSuperseded` — in F's draft; see Open points 5), `token.Pair{Callout, Major, Minor}`, `token.Claims{Callout, Major, Minor}`, `(*token.Signer).Verify`.
  - C: `spi.ProcessorConfig.Idempotent`, `spi.ScheduleFunction.RetryPolicy`, `contract.ParseCriterionFunction` → `Config.RetryPolicy` (through `NewCriteriaCallout`).
- Produces:
  - `grpc.Callout.RetryPolicy string` — the stored `retryPolicy` of the processor, criterion or function, set by the three builders.
  - `type callout.Config struct { SelfNodeID string; FixedNumRetries int }` (O-4 adds `Patience`, O-5 `HandoverAllowance`)
  - `func callout.New(local *internalgrpc.ProcessorDispatcher, members *internalgrpc.MemberRegistry, f *fence.Fence, uuids spi.UUIDGenerator, cfg Config) *Coordinator` (O-5 adds the `peers` parameter after `members`)
  - `(*Coordinator).DispatchProcessor / DispatchCriteria / DispatchFunction` — `contract.ExternalProcessingService`
  - `contract.CalloutStats{Tries []string; HandOvers []string; Waited time.Duration}`, `contract.WithCalloutStats(ctx) (context.Context, *CalloutStats)`, `contract.CalloutStatsFrom(ctx) *CalloutStats`, `contract.CalloutOutcomeOK = "ok"`, `CalloutOutcomeAbandoned = "abandoned"`, `CalloutOutcomeUnreachable = "unreachable"`.
  - Behaviour other streams and **S** may rely on:
    - tries = 1 for `retryPolicy: "NONE"`, else `1 + cfg.FixedNumRetries`; one `RequestID` (a time UUID) per callout.
    - A failure whose kind forbids another cnode is returned **as it is** — its own code, even when earlier attempts are on record — with `Attempts` filled on a copy. When the tries are used up, or nothing more can be done: one attempt → that attempt's own error; more → `CALLOUT_FAILED`; none → the local procedure's `NoHandOff` wrapping `contract.ErrNoMatchingMember`.
    - The caller's context ending → `ctx.Err()` unchanged; released by the fence → `fence.NewSupersededError()`.
    - The owner's own tries carry `(major, 0)` with `major = 1, 2, …`; `Advance` — the number raised, then the wait — happens inside `Number.Next()`, that is before the try's pass is minted and before its hand-off.

Settled against L's Open point 7: a cnode visited a second time in a later pass may complete the later try with its late answer to the earlier one. Both are the same work under the same request id from the same cnode; the owner treats it as the answer it is.

- [ ] **Step 1: Write the failing tests**

`internal/grpc/callout_test.go` — add:

```go
func TestCalloutBuilders_CarryTheRetryPolicy(t *testing.T) {
	proc := testProcessor("x", 0)
	proc.Config.RetryPolicy = "NONE"
	if got := NewProcessorCallout(testTenantID, testEntity(), proc, "wf1", "t1", "tx-1").RetryPolicy; got != "NONE" {
		t.Errorf("processor RetryPolicy = %q, want NONE", got)
	}

	criterion := json.RawMessage(`{"type":"function","function":{"name":"isEligible","config":{"calculationNodesTags":"x","retryPolicy":"NONE"}}}`)
	call, failure := NewCriteriaCallout(testTenantID, testEntity(), criterion, "TRANSITION", "wf1", "t1", "", "tx-1")
	if failure != nil {
		t.Fatalf("NewCriteriaCallout: %v", failure)
	}
	if call.RetryPolicy != "NONE" {
		t.Errorf("criterion RetryPolicy = %q, want NONE", call.RetryPolicy)
	}

	fn := spi.ScheduleFunction{Name: "calcFire", ResultKind: "Schedule", CalculationNodesTags: "x", RetryPolicy: "NONE"}
	if got := NewFunctionCallout(testTenantID, testEntity(), fn, "wf1", "t1", "tx-1").RetryPolicy; got != "NONE" {
		t.Errorf("function RetryPolicy = %q, want NONE", got)
	}
}
```

`internal/callout/helpers_test.go`:

```go
package callout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

const (
	tenantA = spi.TenantID("tenant-a")
	tenantB = spi.TenantID("tenant-b")

	// limitMs is the answer limit the tests give a cnode that stays silent.
	limitMs = 60
)

func userCtx(tenant spi.TenantID) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "user-1",
		UserName: "test-user",
		Kind:     spi.PrincipalUser,
		Tenant:   spi.Tenant{ID: tenant, Name: "Test Tenant"},
	})
}

func secret() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}

// env is an owner over a real dispatcher, a real member registry and a real
// fence, the way app.go builds them.
type env struct {
	reg    *internalgrpc.MemberRegistry
	gate   *txgate.Registry
	fence  *fence.Fence
	signer *token.Signer
	owner  *Coordinator
}

func newEnv(t *testing.T, cfg Config) *env {
	t.Helper()
	signer, err := token.NewSigner(secret())
	if err != nil {
		t.Fatalf("token.NewSigner: %v", err)
	}
	reg := internalgrpc.NewMemberRegistry()
	gate := txgate.New()
	f := fence.New(gate)
	local := internalgrpc.NewProcessorDispatcher(reg, internalgrpc.NewRoundRobinSelector(reg), common.NewTestUUIDGenerator(),
		signer, "node-owner", 30*time.Second, 60*time.Second, 30*time.Second)
	if cfg.SelfNodeID == "" {
		cfg.SelfNodeID = "node-owner"
	}
	return &env{reg: reg, gate: gate, fence: f, signer: signer, owner: New(local, reg, f, common.NewTestUUIDGenerator(), cfg)}
}

// cnode records what one scripted cnode was sent.
type cnode struct {
	mu         sync.Mutex
	requestIDs []string
	passes     []string
}

func (n *cnode) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.requestIDs)
}

func (n *cnode) seen() (requestIDs, passes []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.requestIDs...), append([]string(nil), n.passes...)
}

// script is what a scripted cnode does with a request. A nil script takes the
// work and never answers.
type script func(reg *internalgrpc.MemberRegistry, m *internalgrpc.Member, requestID string)

// answers completes the request the way a cnode of any kind would: a processor
// result, a criterion result and a function result in one response.
func answers(by string) script {
	return func(_ *internalgrpc.MemberRegistry, m *internalgrpc.Member, requestID string) {
		yes := true
		m.CompleteRequest(requestID, &internalgrpc.ProcessingResponse{
			Success:    true,
			Payload:    json.RawMessage(`{"data":{"by":"` + by + `"}}`),
			Matches:    &yes,
			Reason:     by,
			ResultKind: "Schedule",
			Result:     json.RawMessage(`{"by":"` + by + `"}`),
		})
	}
}

// fails answers "I failed", with the cnode's own message and verdict.
func fails(message string, verdict *bool) script {
	return func(_ *internalgrpc.MemberRegistry, m *internalgrpc.Member, requestID string) {
		m.CompleteRequest(requestID, &internalgrpc.ProcessingResponse{Success: false, Error: message, Retryable: verdict})
	}
}

// detaches takes the work and goes away, as a cnode that crashed does: its
// stream handler unregisters it, which fails the request as disconnected.
func detaches() script {
	return func(reg *internalgrpc.MemberRegistry, m *internalgrpc.Member, _ string) { reg.Unregister(m) }
}

// attach registers a scripted cnode. On a fresh registry cnodes are tried in
// the order they were attached.
func (e *env) attach(t *testing.T, id string, tenant spi.TenantID, tag string, s script) *cnode {
	t.Helper()
	n := &cnode{}
	m := e.reg.Register(id, tenant, []string{tag}, func(ce *cepb.CloudEvent) error {
		_, payload, err := internalgrpc.ParseCloudEvent(ce)
		if err != nil {
			t.Errorf("ParseCloudEvent: %v", err)
			return nil
		}
		var body struct {
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Errorf("request payload: %v", err)
			return nil
		}
		func() {
			n.mu.Lock()
			defer n.mu.Unlock()
			n.requestIDs = append(n.requestIDs, body.RequestID)
			n.passes = append(n.passes, internalgrpc.TxTokenFromCloudEvent(ce))
		}()
		if s != nil {
			if m := e.reg.Get(id); m != nil {
				s(e.reg, m, body.RequestID)
			}
		}
		return nil
	}, nil)
	t.Cleanup(func() { e.reg.Unregister(m) })
	return n
}

func testEntity() *spi.Entity {
	return &spi.Entity{Meta: spi.EntityMeta{ID: "entity-1", TenantID: tenantA}, Data: []byte(`{"foo":"bar"}`)}
}

func processorDef(tag, retryPolicy string, idempotent bool) spi.ProcessorDefinition {
	return spi.ProcessorDefinition{Name: "charge", Config: spi.ProcessorConfig{
		CalculationNodesTags: tag, ResponseTimeoutMs: limitMs, RetryPolicy: retryPolicy, Idempotent: idempotent,
	}}
}

func criterionJSON(tag, retryPolicy string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"type":"function","function":{"name":"isEligible","config":{"calculationNodesTags":%q,"responseTimeoutMs":%d,"retryPolicy":%q}}}`,
		tag, limitMs, retryPolicy))
}

func functionDef(tag, retryPolicy string) spi.ScheduleFunction {
	return spi.ScheduleFunction{Name: "calcFire", ResultKind: "Schedule", CalculationNodesTags: tag, ResponseTimeoutMs: limitMs, RetryPolicy: retryPolicy}
}

// dispatchFunction is the callout most tests make: a function is repeat-safe by
// rule, so every kind of failure that permits another cnode can be shown on it.
func (e *env) dispatchFunction(ctx context.Context, tag, retryPolicy string) (string, error) {
	res, err := e.owner.DispatchFunction(ctx, testEntity(), functionDef(tag, retryPolicy), "wf1", "t1", "tx-1")
	if err != nil {
		return "", err
	}
	var by struct {
		By string `json:"by"`
	}
	if err := json.Unmarshal(res.Value, &by); err != nil {
		return "", fmt.Errorf("function result: %w", err)
	}
	return by.By, nil
}

func appErrOf(t *testing.T, err error) *common.AppError {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("no *AppError behind %v", err)
	}
	return appErr
}

func mustFinish(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not finish", what)
	}
}
```

`internal/callout/coordinator_test.go`:

```go
package callout

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

var _ contract.ExternalProcessingService = (*Coordinator)(nil)

// --- the number of tries ---

func TestOwner_RetryPolicyNone_OneTry_OnEveryKindOfCallout(t *testing.T) {
	kinds := map[string]func(e *env) error{
		"processor": func(e *env) error {
			_, err := e.owner.DispatchProcessor(userCtx(tenantA), testEntity(), processorDef("x", "NONE", true), "wf1", "t1", "tx-1")
			return err
		},
		"criterion": func(e *env) error {
			_, _, err := e.owner.DispatchCriteria(userCtx(tenantA), testEntity(), criterionJSON("x", "NONE"), "TRANSITION", "wf1", "t1", "", "tx-1")
			return err
		},
		"function": func(e *env) error {
			_, err := e.dispatchFunction(userCtx(tenantA), "x", "NONE")
			return err
		},
	}
	for name, dispatch := range kinds {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, Config{FixedNumRetries: 3})
			first := e.attach(t, "m-1", tenantA, "x", nil) // takes the work, never answers
			second := e.attach(t, "m-2", tenantA, "x", answers("m-2"))

			err := dispatch(e)

			// Repeat-safe, a second cnode ready to answer — and still one try.
			if got := appErrOf(t, err).Code; got != common.ErrCodeDispatchTimeout {
				t.Errorf("code = %s, want the one try's own DISPATCH_TIMEOUT", got)
			}
			if first.count() != 1 || second.count() != 0 {
				t.Errorf("asked m-1 %d times and m-2 %d times, want 1 and 0", first.count(), second.count())
			}
		})
	}
}

func TestOwner_RetryPolicyFixedOrUnset_OneTryPlusTheConfiguredRetries(t *testing.T) {
	for _, policy := range []string{"", "FIXED"} {
		t.Run("retryPolicy="+policy, func(t *testing.T) {
			e := newEnv(t, Config{FixedNumRetries: 2})
			var cnodes []*cnode
			for _, id := range []string{"m-1", "m-2", "m-3", "m-4"} {
				cnodes = append(cnodes, e.attach(t, id, tenantA, "x", nil))
			}

			_, err := e.dispatchFunction(userCtx(tenantA), "x", policy)

			asked := 0
			for _, n := range cnodes {
				asked += n.count()
			}
			if asked != 3 {
				t.Errorf("%d tries were made, want 1 + 2", asked)
			}
			appErr := appErrOf(t, err)
			if appErr.Code != common.ErrCodeCalloutFailed || !strings.Contains(appErr.Message, "got 3 failures") {
				t.Errorf("error = %s", appErr.Message)
			}
		})
	}
}

// --- what the client is told ---

func TestOwner_EveryTryUsed_IsCalloutFailedAndListsTheTries(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 1})
	e.attach(t, "m-1", tenantA, "x", nil)
	e.attach(t, "m-2", tenantA, "x", detaches())

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	appErr := appErrOf(t, err)
	if appErr.Status != 503 || appErr.Code != common.ErrCodeCalloutFailed || !appErr.Retryable {
		t.Fatalf("got %d %s retryable=%v, want a retryable 503 CALLOUT_FAILED", appErr.Status, appErr.Code, appErr.Retryable)
	}
	want := "CALLOUT_FAILED: the callout could not be completed, got 2 failures: " +
		"[member<m-1>: DISPATCH_TIMEOUT: function dispatch timed out after 60ms: no response], " +
		"[member<m-2>: COMPUTE_MEMBER_DISCONNECTED: compute member disconnected during function dispatch]"
	if appErr.Message != want {
		t.Errorf("message\n got %s\nwant %s", appErr.Message, want)
	}
	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) || len(failure.Attempts) != 2 {
		t.Errorf("failure = %+v, want both attempts on it", failure)
	}
}

func TestOwner_ExactlyOneAttemptRecorded_IsNotWrapped(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", nil)

	// Repeat-safe and three tries left, but no other cnode and no patience.
	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	appErr := appErrOf(t, err)
	if appErr.Code != common.ErrCodeDispatchTimeout || !appErr.Retryable {
		t.Errorf("got %s, want the attempt's own retryable DISPATCH_TIMEOUT", appErr.Message)
	}
	if strings.Contains(appErr.Message, "member<") {
		t.Errorf("a single attempt must not be wrapped: %s", appErr.Message)
	}
}

func TestOwner_NoAnswer_ProcessorThatIsNotIdempotent_Stops(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", nil)
	second := e.attach(t, "m-2", tenantA, "x", answers("m-2"))

	_, err := e.owner.DispatchProcessor(userCtx(tenantA), testEntity(), processorDef("x", "", false), "wf1", "t1", "tx-1")

	if got := appErrOf(t, err).Code; got != common.ErrCodeDispatchTimeout {
		t.Errorf("code = %s, want the try's own DISPATCH_TIMEOUT", got)
	}
	if second.count() != 0 {
		t.Error("a processor that is not idempotent was given to a second cnode after a hand-off")
	}
}

func TestOwner_NoAnswer_IdempotentProcessor_NextCnodeAnswers(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", detaches())
	e.attach(t, "m-2", tenantA, "x", answers("m-2"))

	updated, err := e.owner.DispatchProcessor(userCtx(tenantA), testEntity(), processorDef("x", "", true), "wf1", "t1", "tx-1")

	if err != nil {
		t.Fatalf("DispatchProcessor: %v", err)
	}
	if string(updated.Data) != `{"by":"m-2"}` {
		t.Errorf("entity data = %s, want m-2's answer", updated.Data)
	}
}

func TestOwner_MemberFailed_OneTry_AndTheVerdictIsKept(t *testing.T) {
	yes := true
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", fails("rates service is down", &yes))
	second := e.attach(t, "m-2", tenantA, "x", answers("m-2"))

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) || failure.Kind != contract.MemberFailed {
		t.Fatalf("err = %v, want a MemberFailed failure", err)
	}
	if failure.Message != "rates service is down" || failure.Retryable == nil || !*failure.Retryable {
		t.Errorf("failure = %+v, want the cnode's own message and verdict", failure)
	}
	if second.count() != 0 {
		t.Error("a cnode that answered \"failed\" was replaced")
	}
}

func TestOwner_SameRequestIDOnEveryTry(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	first := e.attach(t, "m-1", tenantA, "x", detaches())
	second := e.attach(t, "m-2", tenantA, "x", answers("m-2"))

	if _, err := e.dispatchFunction(userCtx(tenantA), "x", ""); err != nil {
		t.Fatal(err)
	}
	ids1, _ := first.seen()
	ids2, _ := second.seen()
	if len(ids1) != 1 || len(ids2) != 1 || ids1[0] == "" || ids1[0] != ids2[0] {
		t.Errorf("request ids = %v and %v, want one id, the same on both tries", ids1, ids2)
	}
}

// --- precedence when nothing more can be done ---

func TestOwner_NoCnode_NoPatience_IsNoComputeMemberAtOnce(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	start := time.Now()

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if !errors.Is(err, contract.ErrNoMatchingMember) {
		t.Fatalf("err = %v, want ErrNoMatchingMember (503 NO_COMPUTE_MEMBER_FOR_TAG at the classifier)", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v: with no patience the answer is immediate", elapsed)
	}
}

func TestOwner_AttemptsOnRecordBeatNoCnode(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", detaches())
	e.attach(t, "m-2", tenantA, "x", detaches())

	// Two tries, both cnodes gone, two tries left, nobody else to ask.
	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if errors.Is(err, contract.ErrNoMatchingMember) {
		t.Fatal("\"no compute member\" is reported only when no try was ever made")
	}
	appErr := appErrOf(t, err)
	if appErr.Code != common.ErrCodeCalloutFailed || !strings.Contains(appErr.Message, "got 2 failures") {
		t.Errorf("error = %s, want CALLOUT_FAILED listing the two attempts", appErr.Message)
	}
}

// --- tenants ---

func TestOwner_TwoTenantsShareATag_OnlyTheCallersCnodesAreTriedOrNamed(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "a-1", tenantA, "shared", nil)
	e.attach(t, "a-2", tenantA, "shared", nil)
	other := e.attach(t, "b-1", tenantB, "shared", answers("b-1"))

	_, err := e.dispatchFunction(userCtx(tenantA), "shared", "")

	appErr := appErrOf(t, err)
	if appErr.Code != common.ErrCodeCalloutFailed || !strings.Contains(appErr.Message, "member<a-1>") || !strings.Contains(appErr.Message, "member<a-2>") {
		t.Errorf("error = %s, want both of tenant A's cnodes listed", appErr.Message)
	}
	if strings.Contains(appErr.Message, "b-1") {
		t.Errorf("another tenant's cnode is named: %s", appErr.Message)
	}
	if other.count() != 0 {
		t.Error("another tenant's cnode was given the work")
	}
}

// --- a scheduled fire ---

// A callout made from a scheduled fire runs under the system principal and no
// client; the rules are the same.
func TestOwner_CalloutFromAScheduledFire_FollowsTheSameRules(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 1})
	e.attach(t, "m-1", tenantA, "x", detaches())
	e.attach(t, "m-2", tenantA, "x", answers("m-2"))

	by, err := e.dispatchFunction(common.SystemUserContext(tenantA), "x", "")
	if err != nil || by != "m-2" {
		t.Fatalf("answered by %q, err %v; want m-2", by, err)
	}

	e.attach(t, "m-3", tenantA, "x", nil)
	e.attach(t, "m-4", tenantA, "x", nil)
	_, err = e.owner.DispatchProcessor(common.SystemUserContext(tenantA), testEntity(), processorDef("x", "", false), "wf1", "t1", "tx-1")
	if got := appErrOf(t, err).Code; got != common.ErrCodeDispatchTimeout {
		t.Errorf("code = %s, want a processor that is not idempotent to stop at its first NoAnswer", got)
	}
}

// --- fencing, the Coordinator's side ---

func pairOf(t *testing.T, e *env, pass string) fence.Pair {
	t.Helper()
	claims, err := e.signer.Verify(pass)
	if err != nil {
		t.Fatalf("verify pass: %v", err)
	}
	return fence.Pair{Callout: claims.Callout, Major: claims.Major, Minor: claims.Minor}
}

// The owner gives the work to a second cnode of its own: by the time the second
// cnode is handed the work, the number has risen — the first cnode's pass is
// refused — and the owner has waited for the first cnode's joined request that
// was in progress.
func TestOwner_SecondCnode_TheFirstIsShutOutAndWaitedForBeforeTheNextHandOff(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 1})
	holding := make(chan func(), 1)
	first := e.attach(t, "m-1", tenantA, "x", func(_ *internalgrpc.MemberRegistry, m *internalgrpc.Member, _ string) {
		// m-1's callback is in progress on the transaction: it holds the lock.
		holding <- e.gate.Acquire("tx-1")
		e.reg.Unregister(m) // and then its stream drops
	})
	second := e.attach(t, "m-2", tenantA, "x", answers("m-2"))

	done := make(chan struct{})
	var by string
	var err error
	go func() {
		defer close(done)
		by, err = e.dispatchFunction(userCtx(tenantA), "x", "")
	}()

	var release func()
	select {
	case release = <-holding:
	case <-time.After(5 * time.Second):
		t.Fatal("m-1 was never given the work")
	}

	// The number rises before the wait: m-1's pass is refused already.
	_, passes := first.seen()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, admitErr := e.fence.Admit(context.Background(), []fence.Pair{pairOf(t, e, passes[0])}); errors.Is(admitErr, fence.ErrSuperseded) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("m-1's pass was still admitted after it was given up on")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// And the wait comes before the next hand-off: while m-1's request holds
	// the lock, m-2 is not given the work.
	time.Sleep(100 * time.Millisecond)
	if second.count() != 0 {
		t.Fatal("m-2 was given the work while m-1's joined request was still in progress")
	}

	release()
	mustFinish(t, "the callout", done)
	if err != nil || by != "m-2" {
		t.Fatalf("answered by %q, err %v; want m-2", by, err)
	}
	_, passes2 := second.seen()
	if got := pairOf(t, e, passes2[0]); got.Major != 2 || got.Minor != 0 {
		t.Errorf("m-2's pass carries (%d, %d), want (2, 0): the owner's own tries raise major", got.Major, got.Minor)
	}
}

func TestOwner_WhenTheCalloutHasEnded_ItsPassesAreRefused(t *testing.T) {
	e := newEnv(t, Config{})
	only := e.attach(t, "m-1", tenantA, "x", answers("m-1"))

	if _, err := e.dispatchFunction(userCtx(tenantA), "x", ""); err != nil {
		t.Fatal(err)
	}
	_, passes := only.seen()
	if _, err := e.fence.Admit(context.Background(), []fence.Pair{pairOf(t, e, passes[0])}); !errors.Is(err, fence.ErrSuperseded) {
		t.Errorf("Admit = %v, want the pass of a callout that has ended to be refused", err)
	}
}

// --- whose context ended ---

func TestOwner_CallerGoesAwayDuringATry_CtxErrUnchanged(t *testing.T) {
	tests := []struct {
		name    string
		ctx     func() (context.Context, context.CancelFunc)
		wantErr error
	}{
		{"the client went away", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(userCtx(tenantA))
			time.AfterFunc(20*time.Millisecond, cancel)
			return ctx, cancel
		}, context.Canceled},
		{"transactionTimeoutMillis fired", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(userCtx(tenantA), 20*time.Millisecond)
		}, context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, Config{FixedNumRetries: 3})
			e.attach(t, "m-1", tenantA, "x", nil)
			second := e.attach(t, "m-2", tenantA, "x", answers("m-2"))
			ctx, cancel := tt.ctx()
			defer cancel()

			_, err := e.dispatchFunction(ctx, "x", "")

			if err != tt.wantErr {
				t.Errorf("err = %v, want %v unchanged", err, tt.wantErr)
			}
			if second.count() != 0 {
				t.Error("a caller that went away ends the callout; the work does not move to the next cnode")
			}
		})
	}
}

// A callout made from inside a callback is released when the cnode that made
// the callback is replaced: CALLOUT_SUPERSEDED, not a cancelled request.
func TestOwner_ReleasedByTheFenceDuringATry_IsCalloutSuperseded(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", nil)

	_, endOuter := e.fence.Begin(context.Background(), "outer-callout", "tx-1", nil)
	defer endOuter()
	e.fence.Advance("outer-callout", 1)
	callback, err := e.fence.Admit(userCtx(tenantA), []fence.Pair{{Callout: "outer-callout", Major: 1}})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		e.fence.Advance("outer-callout", 2) // the outer cnode is replaced
	}()

	_, err = e.dispatchFunction(callback, "x", "")

	appErr := appErrOf(t, err)
	if appErr.Status != 410 || appErr.Code != common.ErrCodeCalloutSuperseded {
		t.Errorf("got %d %s, want 410 CALLOUT_SUPERSEDED", appErr.Status, appErr.Code)
	}
	if !errors.Is(err, fence.ErrSuperseded) {
		t.Errorf("err = %v, want fence.ErrSuperseded as its cause", err)
	}
}

// --- what the loop reports ---

func TestOwner_ReportsEveryTryWithItsOutcome(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", detaches())
	e.attach(t, "m-2", tenantA, "x", answers("m-2"))
	ctx, stats := contract.WithCalloutStats(userCtx(tenantA))

	if _, err := e.dispatchFunction(ctx, "x", ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stats.Tries, ","); got != "no_answer,ok" {
		t.Errorf("Tries = %s, want no_answer,ok", got)
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/grpc/... -run 'TestCalloutBuilders_CarryTheRetryPolicy'`   Expected: FAIL (build) with `… .RetryPolicy undefined (type Callout has no field or method RetryPolicy)`.
Run: `go test ./internal/callout/...`   Expected: FAIL (build) with `undefined: Coordinator`, `undefined: Config`, `undefined: New`, `undefined: contract.WithCalloutStats`.

- [ ] **Step 3: Implement**

`internal/grpc/callout.go` — in `Callout`, after `ResponseTimeoutMs`:

```go
	// RetryPolicy is the value stored in the workflow (NONE, FIXED or empty);
	// the owner turns it into the callout's number of tries.
	RetryPolicy string
```

and in the three builders' `Callout{…}` literals, after `ResponseTimeoutMs:`:

```go
		RetryPolicy:       processor.Config.RetryPolicy, // NewProcessorCallout
		RetryPolicy:       config.RetryPolicy,           // NewCriteriaCallout
		RetryPolicy:       fn.RetryPolicy,               // NewFunctionCallout
```

`internal/contract/callout_stats.go`:

```go
package contract

import (
	"context"
	"time"
)

// Outcomes of a try or a hand-over that are not a CalloutFailureKind.
const (
	// CalloutOutcomeOK: a cnode answered.
	CalloutOutcomeOK = "ok"
	// CalloutOutcomeAbandoned: the caller went away, or the callout was
	// released by the fence, while the try or hand-over was in progress.
	CalloutOutcomeAbandoned = "abandoned"
	// CalloutOutcomeUnreachable: a hand-over that gave no cnode the work — the
	// pnode could not be connected to, or answered that it has no cnode. It
	// uses no try.
	CalloutOutcomeUnreachable = "unreachable"
)

// CalloutStats is what the owner's loop reports about one callout to whoever
// asked for it by putting a CalloutStats on the context — the tracing decorator
// around the ExternalProcessingService. The loop fills it before it returns,
// from the goroutine that called it; the decorator reads it afterwards.
type CalloutStats struct {
	// Tries has one outcome per try counted against the callout's number of
	// tries: CalloutOutcomeOK, CalloutOutcomeAbandoned, or a
	// CalloutFailureKind's String().
	Tries []string
	// HandOvers has one outcome per hand-over to another pnode:
	// CalloutOutcomeOK, CalloutOutcomeUnreachable, CalloutOutcomeAbandoned, or
	// a CalloutFailureKind's String().
	HandOvers []string
	// Waited is how long the callout waited for a cnode to exist.
	Waited time.Duration
}

type calloutStatsKey struct{}

// WithCalloutStats returns a context that asks the owner's loop to report on
// the callout made under it, and the value the report lands in.
func WithCalloutStats(ctx context.Context) (context.Context, *CalloutStats) {
	stats := &CalloutStats{}
	return context.WithValue(ctx, calloutStatsKey{}, stats), stats
}

// CalloutStatsFrom returns the CalloutStats asked for on ctx, or nil.
func CalloutStatsFrom(ctx context.Context) *CalloutStats {
	stats, _ := ctx.Value(calloutStatsKey{}).(*CalloutStats)
	return stats
}
```

`internal/callout/coordinator.go`:

```go
// Package callout holds the owner's loop: the one place that decides how often,
// where and for how long a processor, criterion or function callout is tried.
// The pnode that holds the operation's transaction — the owner — runs it; a
// pnode that receives a hand-over runs the local procedure only and never this.
package callout

import (
	"context"
	"errors"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// retryPolicyNone is the retryPolicy value that selects a single try. Every
// other accepted value — FIXED, or none — selects 1 + Config.FixedNumRetries.
const retryPolicyNone = "NONE"

// Config is the part of the server configuration the owner's loop reads.
type Config struct {
	// SelfNodeID is this pnode's id: the owner named in every pass.
	SelfNodeID string
	// FixedNumRetries is the number of tries after the first under retryPolicy
	// FIXED or none.
	FixedNumRetries int
}

// Coordinator implements contract.ExternalProcessingService on the owner.
type Coordinator struct {
	local   *internalgrpc.ProcessorDispatcher
	members *internalgrpc.MemberRegistry
	fence   *fence.Fence
	uuids   spi.UUIDGenerator
	cfg     Config
}

// New builds the owner's loop.
func New(local *internalgrpc.ProcessorDispatcher, members *internalgrpc.MemberRegistry, f *fence.Fence, uuids spi.UUIDGenerator, cfg Config) *Coordinator {
	return &Coordinator{local: local, members: members, fence: f, uuids: uuids, cfg: cfg}
}

// triesFor is the number of tries retryPolicy selects.
func (c *Coordinator) triesFor(retryPolicy string) int {
	if retryPolicy == retryPolicyNone {
		return 1
	}
	return 1 + c.cfg.FixedNumRetries
}

// majorCounter is the one fencing counter of a callout. Everything that gives
// the callout's work to a cnode draws from it, so the number rises — and the
// earlier cnode is shut out, and its joined request in progress waited for —
// before the work is given to anyone else. It is used from the goroutine that
// runs the callout only.
type majorCounter struct {
	fence     *fence.Fence
	calloutID string
	major     uint32
}

func (n *majorCounter) Next() (uint32, uint32) {
	n.major++
	n.fence.Advance(n.calloutID, n.major)
	return n.major, 0
}

// progress is the running account of one callout.
type progress struct {
	triesLeft int
	attempts  []contract.CalloutAttempt
	// lastTried is the failure of the latest try that was actually made. A
	// pass that found no cnode does not overwrite it.
	lastTried *contract.CalloutFailure
	// noCnode is what the latest pass that made no try reported.
	noCnode *contract.CalloutFailure
	stats   contract.CalloutStats
}

// run is the owner's loop. call comes from one of the three builders; run
// fills in what the owner decides.
func (c *Coordinator) run(ctx context.Context, call internalgrpc.Callout) (internalgrpc.CalloutResult, error) {
	limit, failure := c.local.ResolveAnswerLimit(call.ResponseTimeoutMs)
	if failure != nil {
		return internalgrpc.CalloutResult{}, failure
	}
	tries := c.triesFor(call.RetryPolicy)
	outer := fence.Pairs(ctx)
	call.RequestID = uuid.UUID(c.uuids.NewTimeUUID()).String()
	call.AnswerLimit = limit
	call.OwnerNodeID = c.cfg.SelfNodeID
	call.Outer = passPairs(outer)
	number := &majorCounter{fence: c.fence, calloutID: call.RequestID}
	call.Number = number

	// The callout is ended on every exit path, a panic included: from then on
	// all its passes are refused, and end waits for a joined request of its
	// last cnode that is still in progress.
	cctx, end := c.fence.Begin(ctx, call.RequestID, call.TxID, outer)
	defer end()

	p := &progress{triesLeft: tries}
	result, err := c.loop(cctx, call, p)
	if stats := contract.CalloutStatsFrom(ctx); stats != nil {
		*stats = p.stats
	}
	return result, err
}

// loop runs the local procedure and decides what its outcome means for the
// callout.
func (c *Coordinator) loop(cctx context.Context, call internalgrpc.Callout, p *progress) (internalgrpc.CalloutResult, error) {
	none := internalgrpc.CalloutResult{}

	r := c.local.RunLocal(cctx, call, p.triesLeft)
	p.triesLeft -= r.TriesUsed
	p.attempts = append(p.attempts, r.Attempts...)
	for _, a := range r.Attempts {
		p.stats.Tries = append(p.stats.Tries, a.Kind.String())
	}
	switch {
	case r.CtxErr != nil:
		if r.TriesUsed > len(r.Attempts) {
			p.stats.Tries = append(p.stats.Tries, contract.CalloutOutcomeAbandoned)
		}
		return none, ended(cctx, r.CtxErr)
	case r.OK():
		p.stats.Tries = append(p.stats.Tries, contract.CalloutOutcomeOK)
		return r.Result, nil
	case r.TriesUsed == 0:
		p.noCnode = r.Failure
	default:
		p.lastTried = r.Failure
		if done, err := p.verdict(call.RepeatSafe); done {
			return none, err
		}
	}

	// No cnode took the work.
	return none, p.stop(cctx)
}

// verdict decides what a failed try means for the callout: done with the
// failure itself when its kind forbids another cnode, done with the attempts
// when no try is left, not done otherwise.
func (p *progress) verdict(repeatSafe bool) (bool, error) {
	if !p.lastTried.Kind.MayTryAnother(repeatSafe) {
		return true, withAttempts(p.lastTried, p.attempts)
	}
	if p.triesLeft <= 0 {
		return true, attemptsFailure(p.attempts, p.lastTried)
	}
	return false, nil
}

// stop is the error when nothing more can be done. Attempts on record beat
// "no cnode": NO_COMPUTE_MEMBER_FOR_TAG is returned only when no try was ever
// made.
func (p *progress) stop(cctx context.Context) error {
	if err := cctx.Err(); err != nil {
		return ended(cctx, err)
	}
	if len(p.attempts) > 0 {
		return attemptsFailure(p.attempts, p.lastTried)
	}
	return p.noCnode
}

// ended is the error for a callout whose context ended. context.Cause tells
// why: released by the fence — the callback this callout was made from belongs
// to a cnode that was replaced — is CALLOUT_SUPERSEDED; the caller going away,
// or its transactionTimeoutMillis, is ctxErr, unchanged.
func ended(cctx context.Context, ctxErr error) error {
	if errors.Is(context.Cause(cctx), fence.ErrSuperseded) {
		return fence.NewSupersededError()
	}
	return ctxErr
}

// passPairs converts the pairs a context was admitted under into the form a
// pass carries them in.
func passPairs(pairs []fence.Pair) []token.Pair {
	if len(pairs) == 0 {
		return nil
	}
	out := make([]token.Pair, len(pairs))
	for i, p := range pairs {
		out[i] = token.Pair{Callout: p.Callout, Major: p.Major, Minor: p.Minor}
	}
	return out
}
```

`internal/callout/entry.go`:

```go
package callout

import (
	"context"
	"encoding/json"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// DispatchProcessor runs an externalized processor. It may be given to another
// cnode after a hand-off only if its author declared it idempotent.
func (c *Coordinator) DispatchProcessor(ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName string, transitionName string, txID string) (*spi.Entity, error) {
	uc := spi.MustGetUserContext(ctx)
	call := internalgrpc.NewProcessorCallout(uc.Tenant.ID, entity, processor, workflowName, transitionName, txID)
	call.RepeatSafe = processor.Config.Idempotent
	res, err := c.run(ctx, call)
	if err != nil {
		return nil, err
	}
	return res.Entity, nil
}

// DispatchCriteria evaluates a FUNCTION criterion. A criterion computes and
// does not write, so it is repeat-safe by rule.
func (c *Coordinator) DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target string, workflowName string, transitionName string, processorName string, txID string) (bool, string, error) {
	uc := spi.MustGetUserContext(ctx)
	call, failure := internalgrpc.NewCriteriaCallout(uc.Tenant.ID, entity, criterion, target, workflowName, transitionName, processorName, txID)
	if failure != nil {
		return false, "", failure
	}
	res, err := c.run(ctx, call)
	if err != nil {
		return false, "", err
	}
	return res.Matches, res.Reason, nil
}

// DispatchFunction runs a generic Function callout (a scheduled transition's
// timing computation). Repeat-safe by rule.
func (c *Coordinator) DispatchFunction(ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction, workflowName string, transitionName string, txID string) (contract.FunctionResult, error) {
	uc := spi.MustGetUserContext(ctx)
	call := internalgrpc.NewFunctionCallout(uc.Tenant.ID, entity, fn, workflowName, transitionName, txID)
	res, err := c.run(ctx, call)
	if err != nil {
		return contract.FunctionResult{}, err
	}
	return res.Function, nil
}
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/callout/... ./internal/contract/... ./internal/grpc/...`, `go vet ./internal/callout/...`   Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/callout/coordinator.go internal/callout/entry.go internal/callout/helpers_test.go internal/callout/coordinator_test.go internal/contract/callout_stats.go internal/grpc/callout.go internal/grpc/callout_test.go
git commit -m "feat(callout): the owner's loop — tries from retryPolicy, one request id, fenced tries (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task O-4: patience — a callout waits, once and in total, for a cnode to exist

**Spec:** D7; §5 pseudo-code "take both `Changed()` channels … before looking", "if patience used up … -> return per the precedence", "wait for either channel, or the rest of the patience, or ctx", "start a new pass"; §5 bullet "Patience (D7)" in full; "A cancelled caller ends the loop at once, from a wait as from a try". §13 U cells: "No cnode → waits, one attaches → succeeds, no try used", "Patience is one allowance across several waits", "Patience applies with `retryPolicy: NONE`", "No cnode within patience → 503 `NO_COMPUTE_MEMBER_FOR_TAG`; patience 0 → at once" (patience 0 is O-3's `TestOwner_NoCnode_NoPatience_IsNoComputeMemberAtOnce`), "Client timeout (408) / cancellation during a wait", "A Coordinator released by the fence …" (during a wait).

**Files:**
- Modify: `internal/callout/coordinator.go` (`Config`, `loop`; `waitForChange` added)
- Test: `internal/callout/patience_test.go`

**Interfaces:**
- Consumes: L-4 `(*grpc.MemberRegistry).Changed() <-chan struct{}` — closed and replaced on every attach and detach; taken before looking.
- Produces: `callout.Config.Patience time.Duration` — the total a callout may spend waiting, across all its waits; `0` disables waiting. `contract.CalloutStats.Waited` is filled. For **S**: a callout with no cnode anywhere fails after the patience, not at once; every harness that does not want that sets `CYODA_DISPATCH_WAIT_TIMEOUT` low (stream H already does for the parity fixtures).

- [ ] **Step 1: Write the failing tests**

`internal/callout/patience_test.go`:

```go
package callout

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
)

// No cnode, one attaches inside the patience: the callout succeeds, and the
// waiting used no try — shown by giving the callout exactly one.
func TestOwner_NoCnode_Waits_OneAttaches_Succeeds_NoTryUsed(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 5 * time.Second})
	time.AfterFunc(50*time.Millisecond, func() { e.attach(t, "m-1", tenantA, "x", answers("m-1")) })
	ctx, stats := contract.WithCalloutStats(userCtx(tenantA))
	start := time.Now()

	by, err := e.dispatchFunction(ctx, "x", "NONE") // patience applies with retryPolicy NONE too

	if err != nil || by != "m-1" {
		t.Fatalf("answered by %q, err %v; want m-1", by, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v: the wait ends on the change, not on a timer", elapsed)
	}
	if len(stats.Tries) != 1 || stats.Waited <= 0 {
		t.Errorf("stats = %+v, want one try and a wait on record", stats)
	}
}

func TestOwner_NoCnodeWithinThePatience_IsNoComputeMember(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 80 * time.Millisecond})
	ctx, stats := contract.WithCalloutStats(userCtx(tenantA))
	start := time.Now()

	_, err := e.dispatchFunction(ctx, "x", "")

	if !errors.Is(err, contract.ErrNoMatchingMember) {
		t.Fatalf("err = %v, want ErrNoMatchingMember", err)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Errorf("gave up after %v, before the patience was spent", elapsed)
	}
	if stats.Waited != 80*time.Millisecond {
		t.Errorf("Waited = %v, want the whole patience", stats.Waited)
	}
}

// The patience is one allowance for the callout, not one per wait: changes
// that bring no matching cnode start a new pass and a new wait, and the waits
// add up to the setting.
func TestOwner_PatienceIsOneAllowanceAcrossSeveralWaits(t *testing.T) {
	const patience = 600 * time.Millisecond
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: patience})
	// Two changes that do not help: a cnode for another tag comes at 200 ms and
	// another at 400 ms. Per-wait patience would end at 400 + 600 ms.
	time.AfterFunc(200*time.Millisecond, func() { e.attach(t, "other-1", tenantA, "other", nil) })
	time.AfterFunc(400*time.Millisecond, func() { e.attach(t, "other-2", tenantA, "other", nil) })
	start := time.Now()

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	elapsed := time.Since(start)
	if !errors.Is(err, contract.ErrNoMatchingMember) {
		t.Fatalf("err = %v, want ErrNoMatchingMember", err)
	}
	if elapsed < patience || elapsed > patience+250*time.Millisecond {
		t.Errorf("gave up after %v, want about %v in total", elapsed, patience)
	}
}

// A pass that made tries may still wait: a cnode that dropped and comes back is
// what the patience exists for.
func TestOwner_APassThatMadeTriesMayStillWait(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 5 * time.Second})
	e.attach(t, "m-1", tenantA, "x", detaches())
	time.AfterFunc(100*time.Millisecond, func() { e.attach(t, "m-1-again", tenantA, "x", answers("m-1-again")) })

	by, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if err != nil || by != "m-1-again" {
		t.Fatalf("answered by %q, err %v; want the cnode that came back", by, err)
	}
}

// The patience ran out with an attempt on record and tries still left: the
// attempt is reported, not "no compute member".
func TestOwner_PatienceSpentWithAnAttemptOnRecord_ReportsTheAttempt(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 80 * time.Millisecond})
	e.attach(t, "m-1", tenantA, "x", detaches())

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if got := appErrOf(t, err).Code; got != common.ErrCodeComputeMemberDisconnected {
		t.Errorf("code = %s, want the one attempt's own COMPUTE_MEMBER_DISCONNECTED", got)
	}
}

func TestOwner_CallerGoesAwayDuringAWait_EndsAtOnce(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 30 * time.Second})
	ctx, cancel := context.WithCancel(userCtx(tenantA))
	time.AfterFunc(30*time.Millisecond, cancel)
	start := time.Now()

	_, err := e.dispatchFunction(ctx, "x", "")

	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled unchanged", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v: a cancelled caller ends the wait at once", elapsed)
	}
}

func TestOwner_ReleasedByTheFenceDuringAWait_IsCalloutSuperseded(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 30 * time.Second})
	_, endOuter := e.fence.Begin(context.Background(), "outer-callout", "tx-1", nil)
	defer endOuter()
	e.fence.Advance("outer-callout", 1)
	callback, err := e.fence.Admit(userCtx(tenantA), []fence.Pair{{Callout: "outer-callout", Major: 1}})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	time.AfterFunc(30*time.Millisecond, func() { e.fence.Advance("outer-callout", 2) })

	_, err = e.dispatchFunction(callback, "x", "")

	if got := appErrOf(t, err).Code; got != common.ErrCodeCalloutSuperseded {
		t.Errorf("code = %s, want CALLOUT_SUPERSEDED", got)
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/callout/... -run 'TestOwner_(NoCnode_Waits|NoCnodeWithinThePatience|PatienceIs|APassThatMadeTries|PatienceSpent|CallerGoesAwayDuringAWait|ReleasedByTheFenceDuringAWait)'`   Expected: FAIL (build) with `unknown field Patience in struct literal of type Config`.

- [ ] **Step 3: Implement**

`internal/callout/coordinator.go` — the imports gain `"log/slog"` and `"time"`. In `Config`, after `FixedNumRetries`:

```go
	// Patience is how long one callout waits, in total, for a cnode to exist.
	// Zero disables waiting.
	Patience time.Duration
```

`loop` — before:

```go
// loop runs the local procedure and decides what its outcome means for the
// callout.
func (c *Coordinator) loop(cctx context.Context, call internalgrpc.Callout, p *progress) (internalgrpc.CalloutResult, error) {
	none := internalgrpc.CalloutResult{}

	r := c.local.RunLocal(cctx, call, p.triesLeft)
```

… (the accounting and the `switch`, unchanged) …

```go
	// No cnode took the work.
	return none, p.stop(cctx)
}
```

after — the body moves into a `for`, one level deeper, and the tail becomes the wait:

```go
// loop makes passes over this pnode's cnodes until one answers, a failure
// forbids another try, the tries are used up, or no cnode appeared within the
// patience.
func (c *Coordinator) loop(cctx context.Context, call internalgrpc.Callout, p *progress) (internalgrpc.CalloutResult, error) {
	none := internalgrpc.CalloutResult{}
	patienceLeft := c.cfg.Patience
	for {
		// The channel is taken before looking, so a change that happens while
		// this pass looks is not lost to the wait that follows it.
		localChanged := c.members.Changed()

		r := c.local.RunLocal(cctx, call, p.triesLeft)
		p.triesLeft -= r.TriesUsed
		p.attempts = append(p.attempts, r.Attempts...)
		for _, a := range r.Attempts {
			p.stats.Tries = append(p.stats.Tries, a.Kind.String())
		}
		switch {
		case r.CtxErr != nil:
			if r.TriesUsed > len(r.Attempts) {
				p.stats.Tries = append(p.stats.Tries, contract.CalloutOutcomeAbandoned)
			}
			return none, ended(cctx, r.CtxErr)
		case r.OK():
			p.stats.Tries = append(p.stats.Tries, contract.CalloutOutcomeOK)
			return r.Result, nil
		case r.TriesUsed == 0:
			p.noCnode = r.Failure
		default:
			p.lastTried = r.Failure
			if done, err := p.verdict(call.RepeatSafe); done {
				return none, err
			}
		}

		// No cnode took the work in this pass. A pass that made tries may still
		// wait: a cnode that dropped and is coming back is the case the
		// patience exists for.
		if patienceLeft <= 0 || cctx.Err() != nil {
			return none, p.stop(cctx)
		}
		slog.Debug("callout waits for a cnode", "pkg", "callout", "kind", call.Kind.String(), "name", call.Name,
			"requestId", call.RequestID, "tags", call.Tags, "patienceLeftMs", patienceLeft.Milliseconds())
		waited, changed := waitForChange(cctx, localChanged, patienceLeft)
		patienceLeft -= waited
		p.stats.Waited += waited
		if !changed {
			return none, p.stop(cctx)
		}
	}
}
```

and, below `stop`:

```go
// waitForChange blocks until a cnode came or went, the rest of the patience is
// spent, or ctx ends. It reports the time spent and whether a change ended the
// wait. The wait is on a signal, never a poll.
func waitForChange(ctx context.Context, localChanged <-chan struct{}, patienceLeft time.Duration) (time.Duration, bool) {
	start := time.Now()
	timer := time.NewTimer(patienceLeft)
	defer timer.Stop()
	select {
	case <-localChanged:
		return time.Since(start), true
	case <-timer.C:
		return patienceLeft, false
	case <-ctx.Done():
		return time.Since(start), false
	}
}
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/callout/...`   Expected: PASS (O-3's tests run with `Patience` zero and are unchanged).

- [ ] **Step 5: Commit**

```
git add internal/callout/coordinator.go internal/callout/patience_test.go
git commit -m "feat(callout): one patience per callout, waited out on the registry's change signal (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task O-5: hand-overs — the tries left go to one peer after another; the time a callout may take is a hard limit

**Spec:** D5, D6; §5 pseudo-code "`deadline := now + tries × answer limit + patience + hand-over allowance`", "for each alive peer advertising the tag, in selector order, not yet asked in this pass" through "start a new pass (the asked set is cleared)"; §5 bullets "A peer that cannot be connected to, or that answers "no cnode", uses no try", "A hard limit on time, though not on tries (D6)", "The deadline is a context derived from the caller's", "Worst case"; §6 "The owner's wait for the answer is `triesLeft × answerLimit + CYODA_CALLOUT_HANDOVER_ALLOWANCE`, and never past the callout's deadline … a per-request deadline on `ctx`"; §8.2 row "A hand-over's answer was lost" and "No peer could be connected to, and no local cnode"; §7 "its hand-overs (§5) both draw from it". §13 U cells: "Owner's cnode fails → hand-over succeeds", "No peer can be connected to, no local cnode → 503 `NO_COMPUTE_MEMBER_FOR_TAG`", "Callout deadline cuts off a try in progress", "Callout ended by a panic → its passes are refused", and the owner's half of "Hand-over answer lost → one try counted; not repeat-safe → 503 `DISPATCH_FORWARD_FAILED`", "Peer cannot be connected to → no try used, next peer", "cnode message and verdict survive the hand-over".

**Files:**
- Modify: `internal/callout/coordinator.go` (`PeerRouter`, `Config.HandoverAllowance`, `Coordinator.peers`, `New`, `run`, `loop`, `askPeers`, `nextPeer`, `waitForChange`, `stop`, `calloutDeadlinePassed`)
- Modify: `internal/callout/helpers_test.go` (`newEnv`: the `New` call gains `nil`)
- Test: `internal/callout/handover_test.go`

**Interfaces:**
- Consumes (the seam with stream **P**, `planner-interfaces.md`): `dispatch.HandOverAnswer{Connected, Result, Failure, TriesUsed, Attempts, Warnings}`; `(*dispatch.PeerRouter).Peers(tenantID, tagsCSV string) []contract.NodeInfo`, `.HandOver(ctx, peer, call, triesLeft, major) HandOverAnswer`, `.Changed() <-chan struct{}`. L-9: `contract.ErrCalloutDeadline` as the cause of the callout's deadline context, which is what makes `RunLocal` classify a try it cuts off (`NoAnswer`, or `NoHandOff` while still enqueueing) instead of returning `CtxErr`.
- Produces:
  - `type callout.PeerRouter interface { Peers(tenantID, tagsCSV string) []contract.NodeInfo; HandOver(ctx context.Context, peer contract.NodeInfo, call internalgrpc.Callout, triesLeft int, major uint32) dispatch.HandOverAnswer; Changed() <-chan struct{} }` — `*dispatch.PeerRouter` satisfies it.
  - `func callout.New(local *internalgrpc.ProcessorDispatcher, members *internalgrpc.MemberRegistry, peers PeerRouter, f *fence.Fence, uuids spi.UUIDGenerator, cfg Config) *Coordinator` — `peers` is an **untyped nil** on a single pnode.
  - `callout.Config.HandoverAllowance time.Duration`.
  - What the owner does with the seam, for stream **P**:
    - It asks `Peers` afresh before every hand-over and takes the first peer it has not asked **in this pass**; a new pass (after a change) may ask every peer again. The order is `Peers`' own.
    - Before every `HandOver` it draws `major` from the callout's one counter (`fence.Advance` included) — also for a peer that then turns out not to be connectable, which §7 calls harmless.
    - `ctx` carries the owner's wait as a deadline with cause `contract.ErrCalloutDeadline`: `triesLeft × AnswerLimit + HandoverAllowance` from now, and never past the callout's own deadline, because it is derived from the callout's context. When that deadline passes `HandOver` must return a lost answer (`Connected: true`, `TriesUsed: 1`, `Failure` = `NoAnswer` / `DISPATCH_FORWARD_FAILED`). When `ctx` ended for any *other* cause the owner ignores the answer and returns the caller's error (or `CALLOUT_SUPERSEDED`), so `HandOver` may return anything then.
    - `Connected == false` uses no try and records nothing. `Connected == true` with a `Failure` and **no** `Attempts` is recorded by the owner as one attempt under member `-` with the failure's `Message` as its cause — P need not invent an attempt for a lost answer.
    - `Warnings` go to `common.AddWarning` on the owner's request.
    - `a.Result` must be non-nil when `a.Failure == nil && a.Connected`.

**Existing tests (decided here, deleted in O-8 with the code they test):** see the table in O-8.

- [ ] **Step 1: Write the failing tests**

`internal/callout/helpers_test.go` — in `newEnv`, the last line becomes:

```go
	return &env{reg: reg, gate: gate, fence: f, signer: signer, owner: New(local, reg, nil, f, common.NewTestUUIDGenerator(), cfg)}
```

`internal/callout/handover_test.go`:

```go
package callout

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// handOver is one call the owner made to the router.
type handOver struct {
	peer      string
	requestID string
	triesLeft int
	major     uint32
	wait      time.Duration // the deadline on ctx, from the moment of the call
}

// scriptedRouter stands for the other pnodes: which exist, and what each
// answers, in order. A peer with no answer left cannot be connected to.
type scriptedRouter struct {
	mu      sync.Mutex
	peers   []string
	answers map[string][]func(ctx context.Context) dispatch.HandOverAnswer
	calls   []handOver
	changed chan struct{}
}

func newScriptedRouter(peers ...string) *scriptedRouter {
	return &scriptedRouter{peers: peers, answers: map[string][]func(context.Context) dispatch.HandOverAnswer{}, changed: make(chan struct{})}
}

func (r *scriptedRouter) script(peer string, answers ...func(context.Context) dispatch.HandOverAnswer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers[peer] = append(r.answers[peer], answers...)
}

func (r *scriptedRouter) Peers(string, string) []contract.NodeInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]contract.NodeInfo, 0, len(r.peers))
	for _, id := range r.peers {
		out = append(out, contract.NodeInfo{NodeID: id, Alive: true})
	}
	return out
}

func (r *scriptedRouter) Changed() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.changed
}

// fire signals a change in the cluster's membership.
func (r *scriptedRouter) fire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *scriptedRouter) HandOver(ctx context.Context, peer contract.NodeInfo, call internalgrpc.Callout, triesLeft int, major uint32) dispatch.HandOverAnswer {
	deadline, _ := ctx.Deadline()
	next := func() func(context.Context) dispatch.HandOverAnswer {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, handOver{peer: peer.NodeID, requestID: call.RequestID, triesLeft: triesLeft, major: major, wait: time.Until(deadline)})
		queue := r.answers[peer.NodeID]
		if len(queue) == 0 {
			return nil
		}
		r.answers[peer.NodeID] = queue[1:]
		return queue[0]
	}()
	if next == nil {
		return dispatch.HandOverAnswer{} // could not be connected to
	}
	return next(ctx)
}

func (r *scriptedRouter) made() []handOver {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]handOver(nil), r.calls...)
}

func peerAnswers(by string) func(context.Context) dispatch.HandOverAnswer {
	return func(context.Context) dispatch.HandOverAnswer {
		return dispatch.HandOverAnswer{Connected: true, TriesUsed: 1, Result: &internalgrpc.CalloutResult{
			Function: contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{"by":"` + by + `"}`)},
		}}
	}
}

func peerFails(failure *contract.CalloutFailure, triesUsed int, attempts ...contract.CalloutAttempt) func(context.Context) dispatch.HandOverAnswer {
	return func(context.Context) dispatch.HandOverAnswer {
		return dispatch.HandOverAnswer{Connected: true, TriesUsed: triesUsed, Failure: failure, Attempts: attempts}
	}
}

// lostAnswer is what the router reports when the peer was connected to and no
// usable answer came back: one try, NoAnswer, DISPATCH_FORWARD_FAILED, no cnode
// known.
func lostAnswer() *contract.CalloutFailure {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchForwardFailed,
		"forwarding the callout to a peer node failed").AsRetryable()
	return &contract.CalloutFailure{Kind: contract.NoAnswer, Code: appErr.Code, Message: appErr.Message, Err: appErr}
}

// hangs is a peer that was connected to and never answers: the router gives up
// when the owner's wait on ctx runs out.
func hangs() func(context.Context) dispatch.HandOverAnswer {
	return func(ctx context.Context) dispatch.HandOverAnswer {
		<-ctx.Done()
		return dispatch.HandOverAnswer{Connected: true, TriesUsed: 1, Failure: lostAnswer()}
	}
}

func newClusterEnv(t *testing.T, cfg Config, router PeerRouter) *env {
	t.Helper()
	e := newEnv(t, cfg)
	e.owner = New(e.owner.local, e.reg, router, e.fence, common.NewTestUUIDGenerator(), e.owner.cfg)
	return e
}

func TestOwner_OwnersCnodeFails_HandOverSucceeds(t *testing.T) {
	router := newScriptedRouter("p-1")
	router.script("p-1", peerAnswers("cnode-on-p-1"))
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)
	own := e.attach(t, "m-1", tenantA, "x", detaches())

	by, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if err != nil || by != "cnode-on-p-1" {
		t.Fatalf("answered by %q, err %v; want the peer's cnode", by, err)
	}
	calls := router.made()
	if len(calls) != 1 {
		t.Fatalf("hand-overs = %+v, want one", calls)
	}
	ids, _ := own.seen()
	if calls[0].requestID != ids[0] {
		t.Errorf("hand-over carries request id %q, the local try had %q: one id for the whole callout", calls[0].requestID, ids[0])
	}
	if calls[0].triesLeft != 3 {
		t.Errorf("triesLeft = %d, want the 3 tries left of 4", calls[0].triesLeft)
	}
	if calls[0].major != 2 {
		t.Errorf("major = %d, want 2: the hand-over draws from the counter the local try drew 1 from", calls[0].major)
	}
	// 3 tries × 60 ms + the allowance
	if want := 3*limitMs*time.Millisecond + time.Second; calls[0].wait > want || calls[0].wait < want-500*time.Millisecond {
		t.Errorf("owner's wait = %v, want about %v", calls[0].wait, want)
	}
}

func TestOwner_LocalCnodeAnswers_NoHandOver(t *testing.T) {
	router := newScriptedRouter("p-1")
	router.script("p-1", peerAnswers("cnode-on-p-1"))
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)
	e.attach(t, "m-1", tenantA, "x", answers("m-1"))

	by, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if err != nil || by != "m-1" {
		t.Fatalf("answered by %q, err %v; want the owner's own cnode", by, err)
	}
	if calls := router.made(); len(calls) != 0 {
		t.Errorf("hand-overs = %+v, want none: the owner runs the local procedure first", calls)
	}
}

// A caller that goes away during a hand-over ends the callout with its own
// context error — not with a retryable 503, and not by asking the next peer.
func TestOwner_CallerGoesAwayDuringAHandOver_CtxErrUnchanged(t *testing.T) {
	router := newScriptedRouter("p-1", "p-2")
	router.script("p-1", hangs())
	router.script("p-2", peerAnswers("cnode-on-p-2"))
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: 30 * time.Second}, router)
	ctx, cancel := context.WithCancel(userCtx(tenantA))
	time.AfterFunc(30*time.Millisecond, cancel)

	_, err := e.dispatchFunction(ctx, "x", "")

	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled unchanged", err)
	}
	if calls := router.made(); len(calls) != 1 {
		t.Errorf("hand-overs = %+v, want p-2 never asked", calls)
	}
}

func TestOwner_LocalNoAnswer_ProcessorNotIdempotent_IsNotHandedOver(t *testing.T) {
	router := newScriptedRouter("p-1")
	router.script("p-1", peerAnswers("cnode-on-p-1"))
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)
	e.attach(t, "m-1", tenantA, "x", nil)

	_, err := e.owner.DispatchProcessor(userCtx(tenantA), testEntity(), processorDef("x", "", false), "wf1", "t1", "tx-1")

	if got := appErrOf(t, err).Code; got != common.ErrCodeDispatchTimeout {
		t.Errorf("code = %s, want the try's own DISPATCH_TIMEOUT", got)
	}
	if calls := router.made(); len(calls) != 0 {
		t.Errorf("hand-overs = %+v, want none", calls)
	}
}

func TestOwner_PeerCannotBeConnectedTo_UsesNoTry_NextPeerIsAsked(t *testing.T) {
	router := newScriptedRouter("p-1", "p-2")
	router.script("p-2", peerAnswers("cnode-on-p-2")) // p-1 has no answer: not connected
	e := newClusterEnv(t, Config{FixedNumRetries: 0, HandoverAllowance: time.Second}, router)
	ctx, stats := contract.WithCalloutStats(userCtx(tenantA))

	by, err := e.dispatchFunction(ctx, "x", "") // one try in all

	if err != nil || by != "cnode-on-p-2" {
		t.Fatalf("answered by %q, err %v; want p-2's cnode", by, err)
	}
	calls := router.made()
	if len(calls) != 2 || calls[0].peer != "p-1" || calls[1].peer != "p-2" || calls[1].triesLeft != 1 {
		t.Errorf("hand-overs = %+v, want p-1 then p-2 with the one try still unused", calls)
	}
	if got := strings.Join(stats.HandOvers, ","); got != "unreachable,ok" {
		t.Errorf("HandOvers = %s, want unreachable,ok", got)
	}
}

func TestOwner_NoPeerCanBeConnectedTo_NoLocalCnode_IsNoComputeMember(t *testing.T) {
	router := newScriptedRouter("p-1", "p-2")
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if !errors.Is(err, contract.ErrNoMatchingMember) {
		t.Fatalf("err = %v, want ErrNoMatchingMember: no try was ever made", err)
	}
	if calls := router.made(); len(calls) != 2 {
		t.Errorf("hand-overs = %+v, want each peer asked once", calls)
	}
}

// The asked set belongs to one pass: after a change a peer that could not be
// connected to is asked again.
func TestOwner_ANewPassAsksEveryPeerAgain(t *testing.T) {
	router := newScriptedRouter("p-1")
	e := newClusterEnv(t, Config{FixedNumRetries: 3, Patience: 5 * time.Second, HandoverAllowance: time.Second}, router)
	time.AfterFunc(50*time.Millisecond, func() {
		router.script("p-1", peerAnswers("cnode-on-p-1"))
		router.fire() // p-1's list arrived
	})

	by, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if err != nil || by != "cnode-on-p-1" {
		t.Fatalf("answered by %q, err %v; want p-1's cnode on the second pass", by, err)
	}
	if calls := router.made(); len(calls) != 2 || calls[1].major != 2 {
		t.Errorf("hand-overs = %+v, want p-1 asked twice, the number rising each time", calls)
	}
}

func TestOwner_HandOverAnswerLost(t *testing.T) {
	t.Run("not repeat-safe: DISPATCH_FORWARD_FAILED, recorded under member -", func(t *testing.T) {
		router := newScriptedRouter("p-1", "p-2")
		router.script("p-1", peerFails(lostAnswer(), 1))
		router.script("p-2", peerAnswers("cnode-on-p-2"))
		e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)

		_, err := e.owner.DispatchProcessor(userCtx(tenantA), testEntity(), processorDef("x", "", false), "wf1", "t1", "tx-1")

		appErr := appErrOf(t, err)
		if appErr.Status != 503 || appErr.Code != common.ErrCodeDispatchForwardFailed || !appErr.Retryable {
			t.Errorf("got %d %s, want a retryable 503 DISPATCH_FORWARD_FAILED", appErr.Status, appErr.Code)
		}
		var failure *contract.CalloutFailure
		if !errors.As(err, &failure) || len(failure.Attempts) != 1 || failure.Attempts[0].MemberID != "-" {
			t.Errorf("attempts = %+v, want one, under member -", failure)
		}
		if calls := router.made(); len(calls) != 1 {
			t.Errorf("hand-overs = %+v: the work may have run, so no other pnode is asked", calls)
		}
	})
	t.Run("repeat-safe: one try counted, the next peer is asked", func(t *testing.T) {
		router := newScriptedRouter("p-1", "p-2")
		router.script("p-1", peerFails(lostAnswer(), 1))
		router.script("p-2", peerFails(lostAnswer(), 1))
		e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)

		_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

		calls := router.made()
		if len(calls) != 2 || calls[0].triesLeft != 4 || calls[1].triesLeft != 3 {
			t.Fatalf("hand-overs = %+v, want two, the second with one try fewer", calls)
		}
		want := "CALLOUT_FAILED: the callout could not be completed, got 2 failures: " +
			"[member<->: DISPATCH_FORWARD_FAILED: forwarding the callout to a peer node failed (2 times)]"
		if got := appErrOf(t, err).Message; got != want {
			t.Errorf("message\n got %s\nwant %s", got, want)
		}
	})
}

func TestOwner_PeersCnodeFailed_ItsMessageAndVerdictSurvive_NobodyElseIsAsked(t *testing.T) {
	yes := true
	router := newScriptedRouter("p-1", "p-2")
	router.script("p-1", peerFails(
		&contract.CalloutFailure{Kind: contract.MemberFailed, Message: "rates service is down", Retryable: &yes}, 2,
		contract.CalloutAttempt{MemberID: "r-1", Kind: contract.NoHandOff, Cause: "COMPUTE_MEMBER_DISCONNECTED: compute member disconnected during function dispatch"},
		contract.CalloutAttempt{MemberID: "r-2", Kind: contract.MemberFailed, Cause: "rates service is down"}))
	router.script("p-2", peerAnswers("cnode-on-p-2"))
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)
	ctx, stats := contract.WithCalloutStats(userCtx(tenantA))

	_, err := e.dispatchFunction(ctx, "x", "")

	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) || failure.Kind != contract.MemberFailed || failure.Message != "rates service is down" || failure.Retryable == nil || !*failure.Retryable {
		t.Fatalf("failure = %+v, want the peer's cnode's own message and verdict", failure)
	}
	if calls := router.made(); len(calls) != 1 {
		t.Errorf("hand-overs = %+v, want p-2 never asked", calls)
	}
	if got := strings.Join(stats.Tries, ","); got != "no_handoff,member_failed" {
		t.Errorf("Tries = %s, want the peer's two tries", got)
	}
}

// The time a callout may take is a hard limit. A peer that hangs costs the
// owner its whole wait and one try; the try that follows is cut off at the
// callout's deadline, as NoAnswer.
func TestOwner_CalloutDeadline_CutsOffATryInProgress(t *testing.T) {
	// Two tries of 400 ms, 100 ms of patience, 100 ms of allowance: the callout
	// may take 2 × 400 + 100 + 100 = 1000 ms.
	const limit = 400
	router := newScriptedRouter("p-1")
	e := newClusterEnv(t, Config{FixedNumRetries: 1, Patience: 100 * time.Millisecond, HandoverAllowance: 100 * time.Millisecond}, router)
	router.script("p-1", func(ctx context.Context) dispatch.HandOverAnswer {
		answer := hangs()(ctx) // the owner's wait: 2 × 400 + 100 = 900 ms, one try
		// As the owner gives up on p-1, a cnode that will not answer attaches
		// locally. Its try starts at about 900 ms and has 400 ms to answer.
		e.attach(t, "m-late", tenantA, "x", nil)
		return answer
	})
	fn := functionDef("x", "")
	fn.ResponseTimeoutMs = limit
	start := time.Now()

	_, err := e.owner.DispatchFunction(userCtx(tenantA), testEntity(), fn, "wf1", "t1", "tx-1")

	elapsed := time.Since(start)
	if elapsed < 1000*time.Millisecond || elapsed > 1200*time.Millisecond {
		t.Errorf("the callout took %v, want it cut off at its 1000 ms — not run to 900 + 400", elapsed)
	}
	appErr := appErrOf(t, err)
	if appErr.Code != common.ErrCodeCalloutFailed || !strings.Contains(appErr.Message, "got 2 failures") {
		t.Fatalf("error = %s, want CALLOUT_FAILED with the lost hand-over and the try that was cut off", appErr.Message)
	}
	if !strings.Contains(appErr.Message, "member<m-late>: DISPATCH_TIMEOUT: function dispatch cut off at the callout deadline: no response") {
		t.Errorf("error = %s, want the cut-off try reported as such", appErr.Message)
	}
	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) || failure.Kind != contract.NoAnswer {
		t.Errorf("failure = %+v, want NoAnswer: a try cut off by the deadline was handed off and not answered", failure)
	}
}

func TestOwner_PanicInsideTheLoop_TheCalloutIsStillEnded(t *testing.T) {
	router := newScriptedRouter("p-1")
	router.script("p-1", func(context.Context) dispatch.HandOverAnswer { panic("boom") })
	e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: time.Second}, router)
	own := e.attach(t, "m-1", tenantA, "x", detaches())

	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected the panic to reach the caller")
			}
		}()
		_, _ = e.dispatchFunction(userCtx(tenantA), "x", "")
	}()

	_, passes := own.seen()
	calloutID := pairOf(t, e, passes[0]).Callout
	for major := uint32(1); major <= 2; major++ {
		if _, err := e.fence.Admit(context.Background(), []fence.Pair{{Callout: calloutID, Major: major}}); !errors.Is(err, fence.ErrSuperseded) {
			t.Errorf("Admit(major %d) = %v, want every pass of a callout ended by a panic refused", major, err)
		}
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/callout/...`   Expected: FAIL (build) with `undefined: PeerRouter`, `unknown field HandoverAllowance in struct literal of type Config`, `too many arguments in call to New`.

- [ ] **Step 3: Implement**

`internal/callout/coordinator.go`. The file as it stands after this task is the one printed in full in O-6, except for the tail of `run`, where O-6 adds `start` and the INFO line (its before/after shows exactly that); where a doc comment now has to mention peers — on `loop`, `majorCounter`, `progress.lastTried`, `ended` — take its wording from that print. The changes against O-4, region by region:

Imports gain `"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"` and `"github.com/cyoda-platform/cyoda-go/internal/common"`.

Above `Config`, the interface:

```go
// PeerRouter is the owner's view of the other pnodes. *dispatch.PeerRouter
// satisfies it. A Coordinator on a single pnode has none.
type PeerRouter interface {
	// Peers returns the alive pnodes other than this one that advertise any of
	// tagsCSV for tenantID, in the order in which they should be asked.
	Peers(tenantID, tagsCSV string) []contract.NodeInfo
	// HandOver passes call to peer with triesLeft tries under fencing number
	// major. The deadline on ctx bounds the owner's wait for the answer.
	HandOver(ctx context.Context, peer contract.NodeInfo, call internalgrpc.Callout, triesLeft int, major uint32) dispatch.HandOverAnswer
	// Changed returns a channel that is closed when a pnode joins or leaves or
	// a pnode's list of tenants and tags arrives. Take it before looking.
	Changed() <-chan struct{}
}
```

`Config` gains, after `Patience`:

```go
	// HandoverAllowance is what the owner allows a hand-over on top of
	// tries × answer limit.
	HandoverAllowance time.Duration
```

`Coordinator` gains the field `peers PeerRouter` after `members`, and `New` becomes:

```go
// New builds the owner's loop. peers is nil on a single pnode — pass an untyped
// nil, not a nil *dispatch.PeerRouter, which would be a non-nil interface.
func New(local *internalgrpc.ProcessorDispatcher, members *internalgrpc.MemberRegistry, peers PeerRouter, f *fence.Fence, uuids spi.UUIDGenerator, cfg Config) *Coordinator {
	return &Coordinator{local: local, members: members, peers: peers, fence: f, uuids: uuids, cfg: cfg}
}
```

`run` — before:

```go
	cctx, end := c.fence.Begin(ctx, call.RequestID, call.TxID, outer)
	defer end()

	p := &progress{triesLeft: tries}
	result, err := c.loop(cctx, call, p)
```

after:

```go
	cctx, end := c.fence.Begin(ctx, call.RequestID, call.TxID, outer)
	defer end()

	// The hard limit on time, fixed now. A lost hand-over answer counts one try
	// while the peer may have made more, so the number of tries can exceed the
	// setting; the time cannot. It is a context of the callout's own, with a
	// cause of its own, never the caller's: that is how the local procedure
	// tells "this callout ran out of time" — the try in progress is NoAnswer —
	// from "the caller went away".
	deadline := time.Now().Add(time.Duration(tries)*limit + c.cfg.Patience + c.cfg.HandoverAllowance)
	cctx, cancel := context.WithDeadlineCause(cctx, deadline, contract.ErrCalloutDeadline)
	defer cancel()

	p := &progress{triesLeft: tries}
	result, err := c.loop(cctx, call, number, p)
```

`loop` — its signature gains `number *majorCounter` after `call`; after `localChanged := c.members.Changed()`:

```go
		var peersChanged <-chan struct{}
		if c.peers != nil {
			peersChanged = c.peers.Changed()
		}
```

between the `switch` and the "No cnode … took the work in this pass" tail:

```go
		if c.peers != nil {
			result, done, err := c.askPeers(cctx, call, number, p)
			if done {
				return result, err
			}
		}
```

and the wait becomes `waitForChange(cctx, localChanged, peersChanged, patienceLeft)`; `waitForChange` gains the parameter and the arm

```go
	case <-peersChanged:
		return time.Since(start), true
```

(a nil channel — no router — never fires).

`stop` — before:

```go
	if err := cctx.Err(); err != nil {
		return ended(cctx, err)
	}
```

after:

```go
	if err := cctx.Err(); err != nil && !calloutDeadlinePassed(cctx) {
		return ended(cctx, err)
	}
```

New functions:

```go
// askPeers hands the callout over to one peer after another, each at most once
// in this pass. A peer that cannot be connected to, or that has no cnode for
// the work, uses no try. done is false when no peer took the work and the
// callout may go on.
func (c *Coordinator) askPeers(cctx context.Context, call internalgrpc.Callout, number *majorCounter, p *progress) (result internalgrpc.CalloutResult, done bool, err error) {
	none := internalgrpc.CalloutResult{}
	asked := make(map[string]struct{})
	for cctx.Err() == nil {
		peer, ok := nextPeer(c.peers.Peers(string(call.TenantID), call.Tags), asked)
		if !ok {
			return none, false, nil
		}
		asked[peer.NodeID] = struct{}{}

		// The same counter RunLocal draws from: the cnode that held the work
		// is shut out before the work goes to another pnode.
		major, _ := number.Next()
		wait := time.Duration(p.triesLeft)*call.AnswerLimit + c.cfg.HandoverAllowance
		hctx, cancel := context.WithDeadlineCause(cctx, time.Now().Add(wait), contract.ErrCalloutDeadline)
		slog.Debug("callout hand-over", "pkg", "callout", "kind", call.Kind.String(), "name", call.Name,
			"requestId", call.RequestID, "peer", peer.NodeID, "triesLeft", p.triesLeft, "major", major)
		a := c.peers.HandOver(hctx, peer, call, p.triesLeft, major)
		cancel()

		if err := cctx.Err(); err != nil && !calloutDeadlinePassed(cctx) {
			p.stats.HandOvers = append(p.stats.HandOvers, contract.CalloutOutcomeAbandoned)
			return none, true, ended(cctx, err)
		}
		for _, w := range a.Warnings {
			common.AddWarning(cctx, w)
		}
		if !a.Connected {
			p.stats.HandOvers = append(p.stats.HandOvers, contract.CalloutOutcomeUnreachable)
			continue
		}
		p.triesLeft -= a.TriesUsed
		if a.Failure != nil && len(a.Attempts) == 0 {
			// The answer was lost: no cnode is known. It counts as a try and is
			// recorded as one, under the member id "-".
			a.Attempts = []contract.CalloutAttempt{{MemberID: "-", Kind: a.Failure.Kind, Cause: a.Failure.Message}}
		}
		p.attempts = append(p.attempts, a.Attempts...)
		for _, at := range a.Attempts {
			p.stats.Tries = append(p.stats.Tries, at.Kind.String())
		}
		if a.Failure == nil {
			p.stats.Tries = append(p.stats.Tries, contract.CalloutOutcomeOK)
			p.stats.HandOvers = append(p.stats.HandOvers, contract.CalloutOutcomeOK)
			return *a.Result, true, nil
		}
		p.stats.HandOvers = append(p.stats.HandOvers, a.Failure.Kind.String())
		p.lastTried = a.Failure
		if done, err := p.verdict(call.RepeatSafe); done {
			return none, true, err
		}
	}
	return none, false, nil
}

// nextPeer is the first of peers that was not asked yet in this pass.
func nextPeer(peers []contract.NodeInfo, asked map[string]struct{}) (contract.NodeInfo, bool) {
	for _, peer := range peers {
		if _, done := asked[peer.NodeID]; !done {
			return peer, true
		}
	}
	return contract.NodeInfo{}, false
}

// calloutDeadlinePassed reports whether ctx ended because the callout's own
// deadline passed.
func calloutDeadlinePassed(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), contract.ErrCalloutDeadline)
}
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/callout/...`, `go vet ./internal/callout/...`   Expected: PASS. `TestOwner_CalloutDeadline_CutsOffATryInProgress` takes one second by design.

- [ ] **Step 5: Commit**

```
git add internal/callout/coordinator.go internal/callout/helpers_test.go internal/callout/handover_test.go
git commit -m "feat(callout): the tries left go to one peer after another, inside a hard limit on time (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task O-6: one INFO line per callout that was not ordinary; tries and hand-overs on the span and in counters

**Spec:** §12 in full (this stream's share) and **V-3**; §14 `telemetry.md` ("the `cyoda.dispatch.duration` buckets stop at 10 s, below one answer limit; new counters").

**Files:**
- Modify: `internal/callout/coordinator.go` (`run`)
- Modify: `internal/observability/dispatch_tracing.go` (`TracingExternalProcessingService`, `NewTracingExternalProcessingService`, `record`; `recordCallout`, `dispatchDurationBuckets` added)
- Modify: `internal/observability/attrs.go`
- Modify: `internal/observability/dispatch_tracing_test.go` (`TestTracingDispatch_DurationHasExplicitBuckets`: the `want` slice)
- Modify: `cmd/cyoda/help/content/telemetry.md`
- Test: `internal/callout/logging_test.go`, `internal/observability/callout_stats_tracing_test.go`

**Interfaces:**
- Consumes: O-3 `contract.WithCalloutStats`, `contract.CalloutStats`.
- Produces: instruments `cyoda.callout.tries` (`Int64Counter`; `type`, `outcome`), `cyoda.callout.handovers` (`Int64Counter`; `outcome`), `cyoda.callout.wait.duration` (`Float64Histogram`, s; `type`); span attributes `callout.tries` (int), `callout.handover` (bool), `callout.waited_ms` (int); `observability.AttrCalloutTries`, `AttrCalloutHandOver`, `AttrCalloutWaitedMs`, `AttrCalloutOutcome`. `outcome` is a closed set: `ok`, `abandoned`, `unreachable`, `no_handoff`, `no_answer`, `member_failed`, `terminal`.

**V-3**, stated: see "Verification points settled here".

**Logging, by level** (`.claude/rules/logging.md`: one event, one line, one level): the INFO line below is the only INFO event of a callout. Per-try detail is L-8's DEBUG `callout try`; per-wait and per-hand-over detail are this stream's DEBUG `callout waits for a cnode` and `callout hand-over`. None of them carries a pass, a token or a cnode's failure text; the hand-over line names the peer's node id, which is server-side only.

**Existing tests:** `TestTracingDispatch_DurationHasExplicitBuckets` (`dispatch_tracing_test.go:196`) — its `want` becomes the new boundaries. `TestTracingDispatch_RecordsKindLabeledAttributes` and the four delegate/propagate tests — stay.

- [ ] **Step 1: Write the failing tests**

`internal/callout/logging_test.go`:

```go
package callout

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer lets cnode writer goroutines log while the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ownerInfoLines captures the INFO lines of pkg=callout written while run runs.
func ownerInfoLines(t *testing.T, run func()) []map[string]any {
	t.Helper()
	out := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	run()

	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}
		if line["pkg"] == "callout" && line["level"] == "INFO" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestOwner_OneInfoLine_ForACalloutThatNeededMoreThanOneTry(t *testing.T) {
	yes := true
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", detaches())
	e.attach(t, "m-2", tenantA, "x", fails("card 4111-1111 declined", &yes))

	lines := ownerInfoLines(t, func() { _, _ = e.dispatchFunction(userCtx(tenantA), "x", "") })

	if len(lines) != 1 {
		t.Fatalf("INFO lines = %v, want exactly one", lines)
	}
	line := lines[0]
	if line["tries"] != float64(2) || line["tenantId"] != string(tenantA) || line["tags"] != "x" || line["succeeded"] != false {
		t.Errorf("line = %v, want tries=2, the tenant, the tags and succeeded=false", line)
	}
	if _, ok := line["elapsedMs"]; !ok {
		t.Errorf("line = %v, want elapsedMs", line)
	}
	for key, value := range line {
		if s, ok := value.(string); ok && strings.Contains(s, "4111") {
			t.Errorf("field %s repeats the cnode's own failure text, which is tenant content: %q", key, s)
		}
	}
}

func TestOwner_OneInfoLine_ForACalloutThatWaited(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 5 * time.Second})
	time.AfterFunc(50*time.Millisecond, func() { e.attach(t, "m-1", tenantA, "x", answers("m-1")) })

	lines := ownerInfoLines(t, func() {
		if _, err := e.dispatchFunction(userCtx(tenantA), "x", ""); err != nil {
			t.Errorf("dispatch: %v", err)
		}
	})

	if len(lines) != 1 || lines[0]["tries"] != float64(1) || lines[0]["succeeded"] != true {
		t.Fatalf("INFO lines = %v, want one line: one try, succeeded, after a wait", lines)
	}
	if waited, _ := lines[0]["waitedMs"].(float64); waited <= 0 {
		t.Errorf("waitedMs = %v, want the wait on record", lines[0]["waitedMs"])
	}
}

func TestOwner_NoInfoLine_ForACalloutAnsweredByItsFirstTry(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", answers("m-1"))

	lines := ownerInfoLines(t, func() { _, _ = e.dispatchFunction(userCtx(tenantA), "x", "") })

	if len(lines) != 0 {
		t.Errorf("INFO lines = %v, want none: the ordinary callout is logged at DEBUG only", lines)
	}
}
```

`internal/observability/callout_stats_tracing_test.go`:

```go
package observability_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
)

// reportingDispatcher stands where the owner's loop stands: it fills the
// CalloutStats the decorator asked for on the context.
type reportingDispatcher struct {
	report contract.CalloutStats
}

func (d *reportingDispatcher) fill(ctx context.Context) {
	if stats := contract.CalloutStatsFrom(ctx); stats != nil {
		*stats = d.report
	}
}

func (d *reportingDispatcher) DispatchProcessor(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
	d.fill(ctx)
	return entity, nil
}

func (d *reportingDispatcher) DispatchCriteria(ctx context.Context, _ *spi.Entity, _ json.RawMessage, _, _, _, _, _ string) (bool, string, error) {
	d.fill(ctx)
	return true, "", nil
}

func (d *reportingDispatcher) DispatchFunction(ctx context.Context, _ *spi.Entity, _ spi.ScheduleFunction, _, _, _ string) (contract.FunctionResult, error) {
	d.fill(ctx)
	return contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{}`)}, nil
}

// sumByAttrs collects an Int64 counter into "k=v,k=v" → value.
func sumByAttrs(t *testing.T, rm metricdata.ResourceMetrics, name string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != name {
				continue
			}
			for _, dp := range md.Data.(metricdata.Sum[int64]).DataPoints {
				out[dp.Attributes.Encoded(attribute.DefaultEncoder())] += dp.Value
			}
		}
	}
	return out
}

func TestTracingDispatch_CountsTriesAndHandOversByOutcome(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	inner := &reportingDispatcher{report: contract.CalloutStats{
		Tries:     []string{"no_answer", "no_answer", contract.CalloutOutcomeOK},
		HandOvers: []string{contract.CalloutOutcomeUnreachable, contract.CalloutOutcomeOK},
		Waited:    1500 * time.Millisecond,
	}}
	traced := observability.NewTracingExternalProcessingService(inner, mp.Meter("test"))

	if _, err := traced.DispatchFunction(context.Background(), &spi.Entity{}, spi.ScheduleFunction{Name: "f"}, "wf", "tr", "tx"); err != nil {
		t.Fatal(err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	tries := sumByAttrs(t, rm, "cyoda.callout.tries")
	if tries["outcome=no_answer,type=function"] != 2 || tries["outcome=ok,type=function"] != 1 || len(tries) != 2 {
		t.Errorf("cyoda.callout.tries = %v", tries)
	}
	handOvers := sumByAttrs(t, rm, "cyoda.callout.handovers")
	if handOvers["outcome=unreachable"] != 1 || handOvers["outcome=ok"] != 1 || len(handOvers) != 2 {
		t.Errorf("cyoda.callout.handovers = %v", handOvers)
	}
	var waits uint64
	var waitSum float64
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name == "cyoda.callout.wait.duration" {
				for _, dp := range md.Data.(metricdata.Histogram[float64]).DataPoints {
					waits += dp.Count
					waitSum += dp.Sum
				}
			}
		}
	}
	if waits != 1 || waitSum != 1.5 {
		t.Errorf("cyoda.callout.wait.duration count = %d sum = %v, want one callout that waited 1.5 s", waits, waitSum)
	}
}

// A callout that did not wait is not in the wait histogram, so its count is
// the number of callouts that waited.
func TestTracingDispatch_NoWaitNoWaitSample(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	inner := &reportingDispatcher{report: contract.CalloutStats{Tries: []string{contract.CalloutOutcomeOK}}}
	traced := observability.NewTracingExternalProcessingService(inner, mp.Meter("test"))

	if _, err := traced.DispatchProcessor(context.Background(), &spi.Entity{}, spi.ProcessorDefinition{Name: "p"}, "wf", "tr", "tx"); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name == "cyoda.callout.wait.duration" {
				t.Errorf("a callout that did not wait was recorded in %s", md.Name)
			}
		}
	}
}

func TestTracingDispatch_SpanRecordsTriesAndHandOver(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})

	inner := &reportingDispatcher{report: contract.CalloutStats{
		Tries:     []string{"no_handoff", contract.CalloutOutcomeOK},
		HandOvers: []string{contract.CalloutOutcomeOK},
		Waited:    250 * time.Millisecond,
	}}
	traced := observability.NewTracingExternalProcessingService(inner, sdkmetric.NewMeterProvider().Meter("test"))
	if _, _, err := traced.DispatchCriteria(context.Background(), &spi.Entity{}, json.RawMessage(`{}`), "TRANSITION", "wf", "tr", "", "tx"); err != nil {
		t.Fatal(err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	got := map[attribute.Key]attribute.Value{}
	for _, kv := range spans[0].Attributes {
		got[kv.Key] = kv.Value
	}
	if got[observability.AttrCalloutTries].AsInt64() != 2 {
		t.Errorf("%s = %v, want 2", observability.AttrCalloutTries, got[observability.AttrCalloutTries].AsInt64())
	}
	if !got[observability.AttrCalloutHandOver].AsBool() {
		t.Errorf("%s = false, want true", observability.AttrCalloutHandOver)
	}
	if got[observability.AttrCalloutWaitedMs].AsInt64() != 250 {
		t.Errorf("%s = %v, want 250", observability.AttrCalloutWaitedMs, got[observability.AttrCalloutWaitedMs].AsInt64())
	}
}
```

`internal/observability/dispatch_tracing_test.go`, in `TestTracingDispatch_DurationHasExplicitBuckets`:

```go
	want := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/callout/... -run 'TestOwner_(OneInfoLine|NoInfoLine)'`   Expected: FAIL — `INFO lines = [], want exactly one` (twice); `TestOwner_NoInfoLine_…` passes.
Run: `go test ./internal/observability/...`   Expected: FAIL (build) with `undefined: observability.AttrCalloutTries`; after adding the attributes alone: `cyoda.callout.tries = map[]`, `bounds len=11 want 15`.

- [ ] **Step 3: Implement**

`internal/callout/coordinator.go`, `run` — before:

```go
	deadline := time.Now().Add(time.Duration(tries)*limit + c.cfg.Patience + c.cfg.HandoverAllowance)
	cctx, cancel := context.WithDeadlineCause(cctx, deadline, contract.ErrCalloutDeadline)
	defer cancel()

	p := &progress{triesLeft: tries}
	result, err := c.loop(cctx, call, number, p)
	if stats := contract.CalloutStatsFrom(ctx); stats != nil {
		*stats = p.stats
	}
	return result, err
```

after:

```go
	start := time.Now()
	deadline := start.Add(time.Duration(tries)*limit + c.cfg.Patience + c.cfg.HandoverAllowance)
	cctx, cancel := context.WithDeadlineCause(cctx, deadline, contract.ErrCalloutDeadline)
	defer cancel()

	p := &progress{triesLeft: tries}
	result, err := c.loop(cctx, call, number, p)

	used := tries - p.triesLeft
	if stats := contract.CalloutStatsFrom(ctx); stats != nil {
		*stats = p.stats
	}
	if used > 1 || p.stats.Waited > 0 {
		// The cnode's own failure text is tenant content; it goes to the client
		// and is not repeated here.
		slog.Info("callout needed more than one try or waited", "pkg", "callout",
			"kind", call.Kind.String(), "name", call.Name, "tenantId", string(call.TenantID), "tags", call.Tags,
			"entityId", call.EntityID, "requestId", call.RequestID,
			"tries", used, "handOvers", len(p.stats.HandOvers),
			"waitedMs", p.stats.Waited.Milliseconds(), "elapsedMs", time.Since(start).Milliseconds(),
			"succeeded", err == nil)
	}
	return result, err
```

The file in full, as it stands after this task:

```go
// Package callout holds the owner's loop: the one place that decides how often,
// where and for how long a processor, criterion or function callout is tried.
// The pnode that holds the operation's transaction — the owner — runs it; a
// pnode that receives a hand-over runs the local procedure only and never this.
package callout

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// retryPolicyNone is the retryPolicy value that selects a single try. Every
// other accepted value — FIXED, or none — selects 1 + Config.FixedNumRetries.
const retryPolicyNone = "NONE"

// PeerRouter is the owner's view of the other pnodes. *dispatch.PeerRouter
// satisfies it. A Coordinator on a single pnode has none.
type PeerRouter interface {
	// Peers returns the alive pnodes other than this one that advertise any of
	// tagsCSV for tenantID, in the order in which they should be asked.
	Peers(tenantID, tagsCSV string) []contract.NodeInfo
	// HandOver passes call to peer with triesLeft tries under fencing number
	// major. The deadline on ctx bounds the owner's wait for the answer.
	HandOver(ctx context.Context, peer contract.NodeInfo, call internalgrpc.Callout, triesLeft int, major uint32) dispatch.HandOverAnswer
	// Changed returns a channel that is closed when a pnode joins or leaves or
	// a pnode's list of tenants and tags arrives. Take it before looking.
	Changed() <-chan struct{}
}

// Config is the part of the server configuration the owner's loop reads.
type Config struct {
	// SelfNodeID is this pnode's id: the owner named in every pass.
	SelfNodeID string
	// FixedNumRetries is the number of tries after the first under retryPolicy
	// FIXED or none.
	FixedNumRetries int
	// Patience is how long one callout waits, in total, for a cnode to exist.
	// Zero disables waiting.
	Patience time.Duration
	// HandoverAllowance is what the owner allows a hand-over on top of
	// tries × answer limit.
	HandoverAllowance time.Duration
}

// Coordinator implements contract.ExternalProcessingService on the owner.
type Coordinator struct {
	local   *internalgrpc.ProcessorDispatcher
	members *internalgrpc.MemberRegistry
	peers   PeerRouter
	fence   *fence.Fence
	uuids   spi.UUIDGenerator
	cfg     Config
}

// New builds the owner's loop. peers is nil on a single pnode — pass an untyped
// nil, not a nil *dispatch.PeerRouter, which would be a non-nil interface.
func New(local *internalgrpc.ProcessorDispatcher, members *internalgrpc.MemberRegistry, peers PeerRouter, f *fence.Fence, uuids spi.UUIDGenerator, cfg Config) *Coordinator {
	return &Coordinator{local: local, members: members, peers: peers, fence: f, uuids: uuids, cfg: cfg}
}

// triesFor is the number of tries retryPolicy selects.
func (c *Coordinator) triesFor(retryPolicy string) int {
	if retryPolicy == retryPolicyNone {
		return 1
	}
	return 1 + c.cfg.FixedNumRetries
}

// majorCounter is the one fencing counter of a callout. Both the local
// procedure (as its TryNumberer) and every hand-over draw from it, so the
// number rises — and the earlier cnode is shut out, and its joined request in
// progress waited for — before the work is given to anyone else. It is used
// from the goroutine that runs the callout only.
type majorCounter struct {
	fence     *fence.Fence
	calloutID string
	major     uint32
}

func (n *majorCounter) Next() (uint32, uint32) {
	n.major++
	n.fence.Advance(n.calloutID, n.major)
	return n.major, 0
}

// progress is the running account of one callout.
type progress struct {
	triesLeft int
	attempts  []contract.CalloutAttempt
	// lastTried is the failure of the latest try that was actually made, here
	// or on another pnode. A pass that found no cnode does not overwrite it.
	lastTried *contract.CalloutFailure
	// noCnode is what the latest pass that made no try reported.
	noCnode *contract.CalloutFailure
	stats   contract.CalloutStats
}

// run is the owner's loop. call comes from one of the three builders; run
// fills in what the owner decides.
func (c *Coordinator) run(ctx context.Context, call internalgrpc.Callout) (internalgrpc.CalloutResult, error) {
	limit, failure := c.local.ResolveAnswerLimit(call.ResponseTimeoutMs)
	if failure != nil {
		return internalgrpc.CalloutResult{}, failure
	}
	tries := c.triesFor(call.RetryPolicy)
	outer := fence.Pairs(ctx)
	call.RequestID = uuid.UUID(c.uuids.NewTimeUUID()).String()
	call.AnswerLimit = limit
	call.OwnerNodeID = c.cfg.SelfNodeID
	call.Outer = passPairs(outer)
	number := &majorCounter{fence: c.fence, calloutID: call.RequestID}
	call.Number = number

	// The callout is ended on every exit path, a panic included: from then on
	// all its passes are refused, and end waits for a joined request of its
	// last cnode that is still in progress.
	cctx, end := c.fence.Begin(ctx, call.RequestID, call.TxID, outer)
	defer end()

	// The hard limit on time, fixed now. It is a context of the callout's own,
	// with a cause of its own, never the caller's.
	start := time.Now()
	deadline := start.Add(time.Duration(tries)*limit + c.cfg.Patience + c.cfg.HandoverAllowance)
	cctx, cancel := context.WithDeadlineCause(cctx, deadline, contract.ErrCalloutDeadline)
	defer cancel()

	p := &progress{triesLeft: tries}
	result, err := c.loop(cctx, call, number, p)

	used := tries - p.triesLeft
	if stats := contract.CalloutStatsFrom(ctx); stats != nil {
		*stats = p.stats
	}
	if used > 1 || p.stats.Waited > 0 {
		// The cnode's own failure text is tenant content; it goes to the client
		// and is not repeated here.
		slog.Info("callout needed more than one try or waited", "pkg", "callout",
			"kind", call.Kind.String(), "name", call.Name, "tenantId", string(call.TenantID), "tags", call.Tags,
			"entityId", call.EntityID, "requestId", call.RequestID,
			"tries", used, "handOvers", len(p.stats.HandOvers),
			"waitedMs", p.stats.Waited.Milliseconds(), "elapsedMs", time.Since(start).Milliseconds(),
			"succeeded", err == nil)
	}
	return result, err
}

// loop makes passes — the local procedure, then one hand-over per peer — until
// a cnode answers, a failure forbids another try, the tries are used up, or
// nothing more can be done within the patience and the deadline.
func (c *Coordinator) loop(cctx context.Context, call internalgrpc.Callout, number *majorCounter, p *progress) (internalgrpc.CalloutResult, error) {
	none := internalgrpc.CalloutResult{}
	patienceLeft := c.cfg.Patience
	for {
		// Both channels are taken before looking, so a change that happens
		// while this pass looks is not lost to the wait that follows it.
		localChanged := c.members.Changed()
		var peersChanged <-chan struct{}
		if c.peers != nil {
			peersChanged = c.peers.Changed()
		}

		r := c.local.RunLocal(cctx, call, p.triesLeft)
		p.triesLeft -= r.TriesUsed
		p.attempts = append(p.attempts, r.Attempts...)
		for _, a := range r.Attempts {
			p.stats.Tries = append(p.stats.Tries, a.Kind.String())
		}
		switch {
		case r.CtxErr != nil:
			if r.TriesUsed > len(r.Attempts) {
				p.stats.Tries = append(p.stats.Tries, contract.CalloutOutcomeAbandoned)
			}
			return none, ended(cctx, r.CtxErr)
		case r.OK():
			p.stats.Tries = append(p.stats.Tries, contract.CalloutOutcomeOK)
			return r.Result, nil
		case r.TriesUsed == 0:
			p.noCnode = r.Failure
		default:
			p.lastTried = r.Failure
			if done, err := p.verdict(call.RepeatSafe); done {
				return none, err
			}
		}

		if c.peers != nil {
			result, done, err := c.askPeers(cctx, call, number, p)
			if done {
				return result, err
			}
		}

		// No cnode anywhere took the work in this pass.
		if patienceLeft <= 0 || cctx.Err() != nil {
			return none, p.stop(cctx)
		}
		slog.Debug("callout waits for a cnode", "pkg", "callout", "kind", call.Kind.String(), "name", call.Name,
			"requestId", call.RequestID, "tags", call.Tags, "patienceLeftMs", patienceLeft.Milliseconds())
		waited, changed := waitForChange(cctx, localChanged, peersChanged, patienceLeft)
		patienceLeft -= waited
		p.stats.Waited += waited
		if !changed {
			return none, p.stop(cctx)
		}
		// A new pass: every peer may be asked again.
	}
}

// askPeers hands the callout over to one peer after another, each at most once
// in this pass. A peer that cannot be connected to, or that has no cnode for
// the work, uses no try. done is false when no peer took the work and the
// callout may go on.
func (c *Coordinator) askPeers(cctx context.Context, call internalgrpc.Callout, number *majorCounter, p *progress) (result internalgrpc.CalloutResult, done bool, err error) {
	none := internalgrpc.CalloutResult{}
	asked := make(map[string]struct{})
	for cctx.Err() == nil {
		peer, ok := nextPeer(c.peers.Peers(string(call.TenantID), call.Tags), asked)
		if !ok {
			return none, false, nil
		}
		asked[peer.NodeID] = struct{}{}

		// The same counter RunLocal draws from: the cnode that held the work
		// is shut out before the work goes to another pnode.
		major, _ := number.Next()
		wait := time.Duration(p.triesLeft)*call.AnswerLimit + c.cfg.HandoverAllowance
		hctx, cancel := context.WithDeadlineCause(cctx, time.Now().Add(wait), contract.ErrCalloutDeadline)
		slog.Debug("callout hand-over", "pkg", "callout", "kind", call.Kind.String(), "name", call.Name,
			"requestId", call.RequestID, "peer", peer.NodeID, "triesLeft", p.triesLeft, "major", major)
		a := c.peers.HandOver(hctx, peer, call, p.triesLeft, major)
		cancel()

		if err := cctx.Err(); err != nil && !calloutDeadlinePassed(cctx) {
			p.stats.HandOvers = append(p.stats.HandOvers, contract.CalloutOutcomeAbandoned)
			return none, true, ended(cctx, err)
		}
		for _, w := range a.Warnings {
			common.AddWarning(cctx, w)
		}
		if !a.Connected {
			p.stats.HandOvers = append(p.stats.HandOvers, contract.CalloutOutcomeUnreachable)
			continue
		}
		p.triesLeft -= a.TriesUsed
		if a.Failure != nil && len(a.Attempts) == 0 {
			// The answer was lost: no cnode is known. It counts as a try and is
			// recorded as one, under the member id "-".
			a.Attempts = []contract.CalloutAttempt{{MemberID: "-", Kind: a.Failure.Kind, Cause: a.Failure.Message}}
		}
		p.attempts = append(p.attempts, a.Attempts...)
		for _, at := range a.Attempts {
			p.stats.Tries = append(p.stats.Tries, at.Kind.String())
		}
		if a.Failure == nil {
			p.stats.Tries = append(p.stats.Tries, contract.CalloutOutcomeOK)
			p.stats.HandOvers = append(p.stats.HandOvers, contract.CalloutOutcomeOK)
			return *a.Result, true, nil
		}
		p.stats.HandOvers = append(p.stats.HandOvers, a.Failure.Kind.String())
		p.lastTried = a.Failure
		if done, err := p.verdict(call.RepeatSafe); done {
			return none, true, err
		}
	}
	return none, false, nil
}

// verdict decides what a failed try means for the callout: done with the
// failure itself when its kind forbids another cnode, done with the attempts
// when no try is left, not done otherwise.
func (p *progress) verdict(repeatSafe bool) (bool, error) {
	if !p.lastTried.Kind.MayTryAnother(repeatSafe) {
		return true, withAttempts(p.lastTried, p.attempts)
	}
	if p.triesLeft <= 0 {
		return true, attemptsFailure(p.attempts, p.lastTried)
	}
	return false, nil
}

// stop is the error when nothing more can be done: the patience is spent, the
// callout's deadline has passed, or its context ended for another reason.
// Attempts on record beat "no cnode": NO_COMPUTE_MEMBER_FOR_TAG is returned
// only when no try was ever made.
func (p *progress) stop(cctx context.Context) error {
	if err := cctx.Err(); err != nil && !calloutDeadlinePassed(cctx) {
		return ended(cctx, err)
	}
	if len(p.attempts) > 0 {
		return attemptsFailure(p.attempts, p.lastTried)
	}
	return p.noCnode
}

// nextPeer is the first of peers that was not asked yet in this pass.
func nextPeer(peers []contract.NodeInfo, asked map[string]struct{}) (contract.NodeInfo, bool) {
	for _, peer := range peers {
		if _, done := asked[peer.NodeID]; !done {
			return peer, true
		}
	}
	return contract.NodeInfo{}, false
}

// waitForChange blocks until a cnode or a pnode came or went, the rest of the
// patience is spent, or ctx ends. It reports the time spent and whether a
// change ended the wait.
func waitForChange(ctx context.Context, localChanged, peersChanged <-chan struct{}, patienceLeft time.Duration) (time.Duration, bool) {
	start := time.Now()
	timer := time.NewTimer(patienceLeft)
	defer timer.Stop()
	select {
	case <-localChanged:
		return time.Since(start), true
	case <-peersChanged:
		return time.Since(start), true
	case <-timer.C:
		return patienceLeft, false
	case <-ctx.Done():
		return time.Since(start), false
	}
}

// calloutDeadlinePassed reports whether ctx ended because the callout's own
// deadline passed.
func calloutDeadlinePassed(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), contract.ErrCalloutDeadline)
}

// ended is the error for a callout whose context ended for a reason other than
// its own deadline. context.Cause tells the two apart: released by the fence —
// the callback this callout was made from belongs to a cnode that was replaced
// — is CALLOUT_SUPERSEDED; the caller going away, or its
// transactionTimeoutMillis, is ctxErr, unchanged.
func ended(cctx context.Context, ctxErr error) error {
	if errors.Is(context.Cause(cctx), fence.ErrSuperseded) {
		return fence.NewSupersededError()
	}
	return ctxErr
}

// passPairs converts the pairs a context was admitted under into the form a
// pass carries them in.
func passPairs(pairs []fence.Pair) []token.Pair {
	if len(pairs) == 0 {
		return nil
	}
	out := make([]token.Pair, len(pairs))
	for i, p := range pairs {
		out[i] = token.Pair{Callout: p.Callout, Major: p.Major, Minor: p.Minor}
	}
	return out
}
```

`internal/observability/attrs.go` — after `AttrDispatchType`:

```go
	AttrCalloutTries    = attribute.Key("callout.tries")
	AttrCalloutHandOver = attribute.Key("callout.handover")
	AttrCalloutWaitedMs = attribute.Key("callout.waited_ms")
	AttrCalloutOutcome  = attribute.Key("outcome")
```

`internal/observability/dispatch_tracing.go`, in full:

```go
package observability

import (
	"context"
	"encoding/json"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// Dispatch kind labels for the "type" metric attribute. Values are preserved exactly
// as before the kind-labeled refactor so existing dashboards/alerts keep working.
const (
	kindProcessor = "processor"
	kindCriteria  = "criteria"
	kindFunction  = "function"
)

// dispatchDurationBuckets reach past the longest a callout may take (tries ×
// answer limit + patience + hand-over allowance: 155 s at the defaults, 275 s at
// the upper bound of the answer limit), so a slow callout lands in a bucket
// rather than in +Inf.
var dispatchDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// TracingExternalProcessingService wraps an ExternalProcessingService with OTel spans and metrics.
type TracingExternalProcessingService struct {
	inner            contract.ExternalProcessingService
	tracer           trace.Tracer
	dispatchDuration metric.Float64Histogram
	dispatchTotal    metric.Int64Counter
	calloutTries     metric.Int64Counter
	calloutHandOvers metric.Int64Counter
	calloutWait      metric.Float64Histogram
	typeProcessor    metric.MeasurementOption
	typeCriteria     metric.MeasurementOption
	typeFunction     metric.MeasurementOption
}

// NewTracingExternalProcessingService returns a TracingExternalProcessingService that decorates
// inner with OTel tracing spans and metrics for processor and criteria dispatches.
func NewTracingExternalProcessingService(inner contract.ExternalProcessingService, meter metric.Meter) *TracingExternalProcessingService {
	tracer := Tracer()

	duration, err := meter.Float64Histogram("cyoda.dispatch.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Processor/criteria dispatch duration"),
		metric.WithExplicitBucketBoundaries(dispatchDurationBuckets...))
	instrErr("cyoda.dispatch.duration", err)
	total, err := meter.Int64Counter("cyoda.dispatch.count",
		metric.WithDescription("Total dispatches"))
	instrErr("cyoda.dispatch.count", err)
	tries, err := meter.Int64Counter("cyoda.callout.tries",
		metric.WithDescription("Tries made for processor/criteria/function callouts, by outcome"))
	instrErr("cyoda.callout.tries", err)
	handOvers, err := meter.Int64Counter("cyoda.callout.handovers",
		metric.WithDescription("Hand-overs of a callout to another node, by outcome"))
	instrErr("cyoda.callout.handovers", err)
	wait, err := meter.Float64Histogram("cyoda.callout.wait.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Time a callout waited for a compute member to exist; recorded only for callouts that waited"),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60))
	instrErr("cyoda.callout.wait.duration", err)

	return &TracingExternalProcessingService{
		inner:            inner,
		tracer:           tracer,
		dispatchDuration: duration,
		dispatchTotal:    total,
		calloutTries:     tries,
		calloutHandOvers: handOvers,
		calloutWait:      wait,
		typeProcessor:    metric.WithAttributes(AttrDispatchType.String(kindProcessor)),
		typeCriteria:     metric.WithAttributes(AttrDispatchType.String(kindCriteria)),
		typeFunction:     metric.WithAttributes(AttrDispatchType.String(kindFunction)),
	}
}

// record opens a span named spanName with spanAttrs, runs fn, and records the elapsed
// duration and count against the metric attribute set for kind. It centralizes the
// span/metric bookkeeping shared by every dispatch kind (processor, criteria, and
// future kinds such as "function") so each DispatchXxx method only supplies what
// differs: the span name/attributes and the inner call itself.
func (t *TracingExternalProcessingService) record(
	ctx context.Context, kind, spanName string, spanAttrs []attribute.KeyValue,
	fn func(ctx context.Context, span trace.Span) error,
) error {
	ctx, span := t.tracer.Start(ctx, spanName, trace.WithAttributes(spanAttrs...))
	defer span.End()

	// Ask the owner's loop for its account of the callout; an inner that is
	// not the owner's loop leaves it empty.
	ctx, stats := contract.WithCalloutStats(ctx)

	opt := t.measurementOption(kind)
	start := time.Now()
	err := fn(ctx, span)
	elapsed := time.Since(start).Seconds()

	t.dispatchDuration.Record(ctx, elapsed, opt)
	t.dispatchTotal.Add(ctx, 1, opt)
	t.recordCallout(ctx, span, kind, stats)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// recordCallout puts the owner's account of one callout on its span — how many
// tries, whether it was handed over to another node, how long it waited — and
// counts each try and each hand-over by outcome. Outcomes are a closed set
// (contract.CalloutOutcome* and contract.CalloutFailureKind), so the label
// cardinality is bounded.
func (t *TracingExternalProcessingService) recordCallout(ctx context.Context, span trace.Span, kind string, stats *contract.CalloutStats) {
	span.SetAttributes(
		AttrCalloutTries.Int(len(stats.Tries)),
		AttrCalloutHandOver.Bool(len(stats.HandOvers) > 0),
		AttrCalloutWaitedMs.Int64(stats.Waited.Milliseconds()),
	)
	for _, outcome := range stats.Tries {
		t.calloutTries.Add(ctx, 1, metric.WithAttributes(AttrDispatchType.String(kind), AttrCalloutOutcome.String(outcome)))
	}
	for _, outcome := range stats.HandOvers {
		t.calloutHandOvers.Add(ctx, 1, metric.WithAttributes(AttrCalloutOutcome.String(outcome)))
	}
	if stats.Waited > 0 {
		t.calloutWait.Record(ctx, stats.Waited.Seconds(), t.measurementOption(kind))
	}
}

// measurementOption returns the cached MeasurementOption for kind, falling back to a
// freshly-built one for kinds not pre-cached in NewTracingExternalProcessingService.
func (t *TracingExternalProcessingService) measurementOption(kind string) metric.MeasurementOption {
	switch kind {
	case kindProcessor:
		return t.typeProcessor
	case kindCriteria:
		return t.typeCriteria
	case kindFunction:
		return t.typeFunction
	default:
		return metric.WithAttributes(AttrDispatchType.String(kind))
	}
}

func (t *TracingExternalProcessingService) DispatchProcessor(
	ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition,
	workflowName, transitionName, txID string,
) (*spi.Entity, error) {
	var result *spi.Entity
	err := t.record(ctx, kindProcessor, "dispatch.processor", []attribute.KeyValue{
		AttrProcessorName.String(processor.Name),
		AttrProcessorMode.String(processor.ExecutionMode),
		AttrProcessorTags.String(processor.Config.CalculationNodesTags),
		AttrWorkflowName.String(workflowName),
		AttrTransitionName.String(transitionName),
	}, func(ctx context.Context, span trace.Span) error {
		var err error
		result, err = t.inner.DispatchProcessor(ctx, entity, processor, workflowName, transitionName, txID)
		return err
	})
	return result, err
}

func (t *TracingExternalProcessingService) DispatchCriteria(
	ctx context.Context, entity *spi.Entity, criterion json.RawMessage,
	target, workflowName, transitionName, processorName, txID string,
) (bool, string, error) {
	var matches bool
	var reason string
	err := t.record(ctx, kindCriteria, "dispatch.criteria", []attribute.KeyValue{
		AttrCriterionTarget.String(target),
		AttrWorkflowName.String(workflowName),
		AttrTransitionName.String(transitionName),
	}, func(ctx context.Context, span trace.Span) error {
		var err error
		matches, reason, err = t.inner.DispatchCriteria(ctx, entity, criterion, target, workflowName, transitionName, processorName, txID)
		span.SetAttributes(AttrCriteriaMatches.Bool(matches))
		return err
	})
	return matches, reason, err
}

func (t *TracingExternalProcessingService) DispatchFunction(
	ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction,
	workflowName, transitionName, txID string,
) (contract.FunctionResult, error) {
	var result contract.FunctionResult
	err := t.record(ctx, kindFunction, "dispatch.function", []attribute.KeyValue{
		AttrFunctionName.String(fn.Name),
		AttrFunctionTags.String(fn.CalculationNodesTags),
		AttrWorkflowName.String(workflowName),
		AttrTransitionName.String(transitionName),
	}, func(ctx context.Context, span trace.Span) error {
		var err error
		result, err = t.inner.DispatchFunction(ctx, entity, fn, workflowName, transitionName, txID)
		return err
	})
	return result, err
}
```

`cmd/cyoda/help/content/telemetry.md`:

- SIGNALS → Traces, the decorator's bullet becomes:

```markdown
- `observability.TracingExternalProcessingService` — decorator around the callout path; creates one span per callout, `dispatch.processor`, `dispatch.criteria` or `dispatch.function`, however many tries the callout takes. The span carries `callout.tries`, `callout.handover` and `callout.waited_ms`.
```

- SIGNALS → Metrics, the two `cyoda.dispatch.*` rows become, and three rows follow them:

```markdown
- `cyoda.dispatch.duration` — `Float64Histogram`, unit `s` — duration of one whole callout, all its tries, waits and hand-overs included; labeled by `type` (`processor`, `criteria`, `function`). Bucket boundaries run to 300 s: a callout may take `tries × answer limit + CYODA_DISPATCH_WAIT_TIMEOUT + CYODA_CALLOUT_HANDOVER_ALLOWANCE` — 155 s at the defaults
- `cyoda.dispatch.count` — `Int64Counter` — callouts, however many tries each took; labeled by `type`
- `cyoda.callout.tries` — `Int64Counter` — tries made; labeled by `type` and `outcome` (`ok`, `no_handoff`, `no_answer`, `member_failed`, `terminal`, `abandoned` — the caller went away while the try was in progress). A rising share of `no_answer` or `no_handoff` means compute members that do not answer or do not read
- `cyoda.callout.handovers` — `Int64Counter` — callouts handed over to another cluster node; labeled by `outcome` (the outcomes above, and `unreachable` — the node could not be connected to or had no matching compute member; uses no try)
- `cyoda.callout.wait.duration` — `Float64Histogram`, unit `s` — how long a callout waited for a compute member to exist; recorded only for callouts that waited, so its count is the number of callouts that waited; labeled by `type`
```

- ATTRIBUTE VOCABULARY — the `type` row names all three kinds, and four rows are added:

```markdown
- `type` — callout kind label for the `cyoda.dispatch.*` and `cyoda.callout.*` metrics (`processor`, `criteria` or `function`)
- `outcome` — outcome label of `cyoda.callout.tries` and `cyoda.callout.handovers`; a closed set
- `callout.tries` — number of tries a callout made, on its span
- `callout.handover` — boolean; `true` when the callout was handed over to another cluster node at least once
- `callout.waited_ms` — milliseconds the callout waited for a compute member to exist
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/callout/... ./internal/observability/... ./cmd/cyoda/help/...`   Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/callout/coordinator.go internal/callout/logging_test.go internal/observability/dispatch_tracing.go internal/observability/attrs.go internal/observability/dispatch_tracing_test.go internal/observability/callout_stats_tracing_test.go cmd/cyoda/help/content/telemetry.md
git commit -m "feat(observability): tries, hand-overs and waits of a callout — counters, span attributes, one INFO line (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task O-7: the gRPC-envelope tests run through the owner's loop

**Spec:** §13 G cells that need the whole loop: "Every try used → 503 `CALLOUT_FAILED`, message shape", "Client timeout (408) / cancellation during a wait and during a try", "No cnode within patience → 503 `NO_COMPUTE_MEMBER_FOR_TAG`; patience 0 → at once". `test-coverage.md`: "gRPC: one test per error class in `internal/grpc` (assert the envelope)".

**Where these tests live, and why.** `internal/callout` imports `internal/grpc`, and all but one test file of `internal/grpc` are `package grpc`, so none of them can import the Coordinator. The tests stay where the envelope tests are — `package grpc`, with `svc.registry`, `makeCE`, `validateResponse` — and the Coordinator is handed to them by an **external** test file of the same directory: `export_test.go` (`package grpc`) declares `NewOwnerForTest`; `owner_wiring_test.go` (`package grpc_test`, which may import `internal/callout`) sets it in `init`. Go links both into one test binary and rebuilds `internal/callout` against the package under test. This is test code only; production code gains nothing.

**Files:**
- Create: `internal/grpc/export_test.go`, `internal/grpc/owner_wiring_test.go`
- Modify: `internal/grpc/scheduled_function_rpc_test.go` (`newTestEnvWithDispatchLimits` → `newTestEnvWithOwner`; the two older names delegate)
- Test: `internal/grpc/owner_loop_rpc_test.go`

**Interfaces:**
- Consumes: O-5 `callout.New`, `callout.Config`; F `fence.New`; L-5 `newTestEnvWithDispatchLimits`, L-8/L-10 `NewProcessorDispatcher(…, passAllowance)`; the existing `setupScheduledWorkflowRPCEnv`, `scheduleFunctionRPCWorkflowJSON`, `schedFnConfigJSON`, `createScheduledEntity`, `assertClientErrorEnvelope`, `makeCE`, `validateResponse`, `testTenant`.
- Produces (test-only): `grpc.OwnerTestConfig{FixedNumRetries int; Patience time.Duration}`; `var grpc.NewOwnerForTest func(local *ProcessorDispatcher, members *MemberRegistry, f *fence.Fence, cfg OwnerTestConfig) contract.ExternalProcessingService`; `newTestEnvWithOwner(t, answerLimitDefault, answerLimitMax time.Duration, owner OwnerTestConfig)`. `newTestEnvWithDispatch` and `newTestEnvWithDispatchLimits` give the owner 3 retries and **no patience**, so every existing "no member" test still answers at once.

**Existing tests — every G test on this environment now runs through the loop with four tries; each was checked:**
`TestRPC_ScheduledFunction_NoMember_Returns503Envelope`, `TestRPC_ProcessorNoMember_…`, `TestRPC_CriterionNoMember_…` — stay: no cnode, no patience → `NO_COMPUTE_MEMBER_FOR_TAG` at once. `TestRPC_ScheduledFunction_DispatchTimeout_Envelope` — stays: one silent cnode, one attempt on record → the attempt's own `DISPATCH_TIMEOUT`, not wrapped. `TestRPC_ScheduledFunction_MalformedResult_Envelope`, `…_ExplicitFire_…`, `…_Import_ValidationFailed` — stay (no callout failure involved). L-5 `TestRPC_Processor_StoredTimeoutOverLoweredBound_Envelope` — stays: `ResolveAnswerLimit` fails before any try. L-7 `TestRPC_ProcessorMemberFailed_…` — stays: `MemberFailed` stops after one try (its two cnodes assert exactly that). L-8 `TestRPC_Processor_NoHandOff_OwnCodeEnvelope` — stays: one wedged cnode, one `NoHandOff` attempt, nobody else → own code. L-8 `TestRPC_Processor_NoAnswer_NotIdempotent_OwnCodeEnvelope` — stays, and from here on proves what its comment promised: with four tries available and a second cnode attached, a processor that is not idempotent stops after one.

- [ ] **Step 1: Write the failing tests**

`internal/grpc/owner_loop_rpc_test.go`:

```go
package grpc

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// createWithTimeout is createScheduledEntity with a transactionTimeoutMs.
func createWithTimeout(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, modelName string, timeoutMs int) events.EntityTransactionResponseJson {
	t.Helper()
	createCE := makeCE(EntityCreateRequest, map[string]any{
		"id":                   "create-1",
		"dataFormat":           "JSON",
		"transactionTimeoutMs": timeoutMs,
		"payload": map[string]any{
			"model": map[string]any{"name": modelName, "version": 1},
			"data":  map[string]any{"name": "Test Order", "amount": 100, "status": "draft"},
		},
	})
	resp, err := svc.EntityManage(ctx, createCE)
	if err != nil {
		t.Fatalf("unexpected gRPC transport error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	return typed
}

// Every try used: a function is repeat-safe, so both silent cnodes are tried,
// and the envelope carries CALLOUT_FAILED with the tries listed.
func TestRPC_Function_EveryTryUsed_CalloutFailedEnvelope(t *testing.T) {
	const modelName = "grpc-owner-every-try-used"
	const tag = "every-try-used-tag"
	svc, wfHandler, ctx := newTestEnvWithOwner(t, 30*time.Second, 60*time.Second, OwnerTestConfig{FixedNumRetries: 1})
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
		scheduleFunctionRPCWorkflowJSON("every-try-used-wf", schedFnConfigJSON("calcFire", tag, 50)))

	var asked atomic.Int32
	for _, id := range []string{"m-1", "m-2"} {
		svc.registry.Register(id, testTenant, []string{tag}, func(*cepb.CloudEvent) error {
			asked.Add(1)
			return nil // takes the work, never answers
		}, nil)
	}

	typed := createScheduledEntity(t, svc, ctx, modelName)

	assertClientErrorEnvelope(t, typed, "CALLOUT_FAILED")
	if !strings.HasPrefix(typed.Error.Message, "CALLOUT_FAILED: the callout could not be completed, got 2 failures: [member<") {
		t.Errorf("message = %s", typed.Error.Message)
	}
	for _, want := range []string{"member<m-1>: DISPATCH_TIMEOUT:", "member<m-2>: DISPATCH_TIMEOUT:"} {
		if !strings.Contains(typed.Error.Message, want) {
			t.Errorf("message = %s, want it to contain %q", typed.Error.Message, want)
		}
	}
	if typed.Error.Retryable == nil || !*typed.Error.Retryable {
		t.Error("expected retryable=true")
	}
	if got := asked.Load(); got != 2 {
		t.Errorf("%d tries were made, want 2", got)
	}
}

func TestRPC_NoCnodeWithinThePatience_NoComputeMemberEnvelope(t *testing.T) {
	tests := []struct {
		name     string
		patience time.Duration
		atLeast  time.Duration
		atMost   time.Duration
	}{
		{"patience 0: at once", 0, 0, 2 * time.Second},
		{"patience 200ms: after the patience", 200 * time.Millisecond, 200 * time.Millisecond, 5 * time.Second},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modelName := "grpc-owner-no-cnode-" + string(rune('a'+i))
			svc, wfHandler, ctx := newTestEnvWithOwner(t, 30*time.Second, 60*time.Second, OwnerTestConfig{FixedNumRetries: 3, Patience: tt.patience})
			setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
				scheduleFunctionRPCWorkflowJSON("no-cnode-wf", schedFnConfigJSON("calcFire", "nobody-has-this-tag", 0)))
			start := time.Now()

			typed := createScheduledEntity(t, svc, ctx, modelName)

			elapsed := time.Since(start)
			assertClientErrorEnvelope(t, typed, "NO_COMPUTE_MEMBER_FOR_TAG")
			if typed.Error.Retryable == nil || !*typed.Error.Retryable {
				t.Error("expected retryable=true")
			}
			if elapsed < tt.atLeast || elapsed > tt.atMost {
				t.Errorf("answered after %v, want between %v and %v", elapsed, tt.atLeast, tt.atMost)
			}
		})
	}
}

// The client's own limit ends a callout at once — from a wait as from a try —
// and the envelope is the 408 every other timed-out write gets.
func TestRPC_ClientTimeoutDuringAWaitAndDuringATry_TransactionTimeoutEnvelope(t *testing.T) {
	for _, during := range []string{"a wait", "a try"} {
		t.Run("during "+during, func(t *testing.T) {
			modelName := "grpc-owner-client-timeout-" + strings.ReplaceAll(during, " ", "-")
			const tag = "client-timeout-tag"
			svc, wfHandler, ctx := newTestEnvWithOwner(t, 30*time.Second, 60*time.Second, OwnerTestConfig{FixedNumRetries: 3, Patience: 30 * time.Second})
			setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
				scheduleFunctionRPCWorkflowJSON("client-timeout-wf", schedFnConfigJSON("calcFire", tag, 20000)))
			if during == "a try" {
				svc.registry.Register("m-1", testTenant, []string{tag}, func(*cepb.CloudEvent) error { return nil }, nil)
			}
			start := time.Now()

			typed := createWithTimeout(t, svc, ctx, modelName, 100)

			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("answered after %v: the client's limit must end the callout at once", elapsed)
			}
			if typed.Success || typed.Error == nil {
				t.Fatal("expected the operation to fail")
			}
			if typed.Error.Code != "CLIENT_ERROR" || !strings.HasPrefix(typed.Error.Message, common.ErrCodeTransactionTimeout+":") {
				t.Errorf("envelope = %s / %s, want CLIENT_ERROR with prefix %s", typed.Error.Code, typed.Error.Message, common.ErrCodeTransactionTimeout)
			}
			if typed.Error.Retryable == nil || !*typed.Error.Retryable {
				t.Error("expected retryable=true")
			}
		})
	}
}

// A client that went away is not a domain failure: a ticketed SERVER_ERROR, no
// callout detail, and no waiting out the patience.
func TestRPC_ClientGoesAwayDuringAWait_TicketedServerErrorEnvelope(t *testing.T) {
	const modelName = "grpc-owner-client-gone"
	svc, wfHandler, ctx := newTestEnvWithOwner(t, 30*time.Second, 60*time.Second, OwnerTestConfig{FixedNumRetries: 3, Patience: 30 * time.Second})
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
		scheduleFunctionRPCWorkflowJSON("client-gone-wf", schedFnConfigJSON("calcFire", "nobody-has-this-tag", 0)))
	ctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()

	typed := createScheduledEntity(t, svc, ctx, modelName)

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("answered after %v: a cancelled caller ends the wait at once", elapsed)
	}
	if typed.Success || typed.Error == nil || typed.Error.Code != "SERVER_ERROR" || !strings.Contains(typed.Error.Message, "ticket") {
		t.Errorf("envelope = %+v, want a ticketed SERVER_ERROR", typed.Error)
	}
	if typed.Error != nil && strings.Contains(typed.Error.Message, "NO_COMPUTE_MEMBER") {
		t.Errorf("message = %s: a cancelled request is not reported as a missing compute member", typed.Error.Message)
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/grpc/... -run 'TestRPC_(Function_EveryTryUsed|NoCnodeWithinThePatience|ClientTimeoutDuring|ClientGoesAway)'`   Expected: FAIL (build) with `undefined: newTestEnvWithOwner`, `undefined: OwnerTestConfig`.

- [ ] **Step 3: Implement**

`internal/grpc/export_test.go`:

```go
package grpc

import (
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
)

// OwnerTestConfig is what an envelope test may set on the owner's loop.
type OwnerTestConfig struct {
	FixedNumRetries int
	Patience        time.Duration
}

// NewOwnerForTest builds the owner's loop (internal/callout.Coordinator) over a
// dispatcher and its registry. internal/callout imports this package, so the
// in-package tests cannot import it; owner_wiring_test.go, an external test
// file of this directory, sets this variable from its init.
var NewOwnerForTest func(local *ProcessorDispatcher, members *MemberRegistry, f *fence.Fence, cfg OwnerTestConfig) contract.ExternalProcessingService
```

`internal/grpc/owner_wiring_test.go`:

```go
package grpc_test

import (
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/callout"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

func init() {
	internalgrpc.NewOwnerForTest = func(local *internalgrpc.ProcessorDispatcher, members *internalgrpc.MemberRegistry, f *fence.Fence, cfg internalgrpc.OwnerTestConfig) contract.ExternalProcessingService {
		return callout.New(local, members, nil, f, common.NewDefaultUUIDGenerator(), callout.Config{
			SelfNodeID:        "node-test",
			FixedNumRetries:   cfg.FixedNumRetries,
			Patience:          cfg.Patience,
			HandoverAllowance: 30 * time.Second,
		})
	}
}
```

`internal/grpc/scheduled_function_rpc_test.go` — `newTestEnvWithDispatchLimits` (L-5) is renamed and gains a parameter, and the two older names delegate. Before (as L-10 left it):

```go
func newTestEnvWithDispatchLimits(t *testing.T, answerLimitDefault, answerLimitMax time.Duration) (*CloudEventsServiceImpl, *workflow.Handler, context.Context) {
```

…

```go
	dispatcher := NewProcessorDispatcher(registry, NewRoundRobinSelector(registry), common.NewDefaultUUIDGenerator(), signer, "node-test", answerLimitDefault, answerLimitMax, 30*time.Second)

	engine := workflow.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr, workflow.WithExternalProcessing(dispatcher))
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	searchService := search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore)
	entityHandler := entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New())
```

after:

```go
// newTestEnvWithOwner wires the production callout path: a real
// ProcessorDispatcher over the returned service's own MemberRegistry, and over
// it the owner's loop, which is what the workflow engine calls.
func newTestEnvWithOwner(t *testing.T, answerLimitDefault, answerLimitMax time.Duration, owner OwnerTestConfig) (*CloudEventsServiceImpl, *workflow.Handler, context.Context) {
```

…

```go
	dispatcher := NewProcessorDispatcher(registry, NewRoundRobinSelector(registry), common.NewDefaultUUIDGenerator(), signer, "node-test", answerLimitDefault, answerLimitMax, 30*time.Second)
	if NewOwnerForTest == nil {
		t.Fatal("owner_wiring_test.go did not set NewOwnerForTest")
	}
	gate := txgate.New()
	extProc := NewOwnerForTest(dispatcher, registry, fence.New(gate), owner)

	engine := workflow.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr, workflow.WithExternalProcessing(extProc))
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	searchService := search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore)
	entityHandler := entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, gate)
```

and below it:

```go
// newTestEnvWithDispatchLimits is newTestEnvWithOwner with the default number
// of tries and no patience: a callout with no cnode fails at once.
func newTestEnvWithDispatchLimits(t *testing.T, answerLimitDefault, answerLimitMax time.Duration) (*CloudEventsServiceImpl, *workflow.Handler, context.Context) {
	t.Helper()
	return newTestEnvWithOwner(t, answerLimitDefault, answerLimitMax, OwnerTestConfig{FixedNumRetries: 3})
}
```

(`newTestEnvWithDispatch` keeps delegating to `newTestEnvWithDispatchLimits`. The file's imports gain `internal/fence`. If stream F has changed `entity.New`'s parameters by then, the `entity.New` line follows F; the fence built here is the one to hand it.)

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/grpc/...`, `go vet ./internal/grpc/...`   Expected: PASS — the new tests and every existing envelope test.

- [ ] **Step 5: Commit**

```
git add internal/grpc/export_test.go internal/grpc/owner_wiring_test.go internal/grpc/owner_loop_rpc_test.go internal/grpc/scheduled_function_rpc_test.go
git commit -m "test(grpc): the envelope tests run through the owner's loop; CALLOUT_FAILED, patience and client timeout pinned (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task O-8: the Coordinator is the `ExternalProcessingService` in both modes; `ClusterDispatcher` and the single-try entry points go

**Spec:** §5 first paragraph ("replaces both the direct use of `ProcessorDispatcher` (single pnode) and `ClusterDispatcher`'s three near-identical methods (cluster). `app.go` builds it in both modes; in single pnode mode its peer router is nil. `TracingExternalProcessingService` still wraps the outside") and "`findPeerWithPolling` and its 200 ms poll are deleted. `forwardWithFailover` is deleted"; §13 last paragraph ("those asserting `forwardWithFailover` or `findPeerWithPolling` go with them"); §14 `docs/ARCHITECTURE.md` "the dispatch section (incl. the stale "no failover to a second peer" row)", DD-9; `grpc.md`.

**Files:**
- Modify: `app/app.go` (`:440-441`, `:523-561`, `:644`)
- Delete: `internal/cluster/dispatch/cluster_dispatcher.go`, `cluster_dispatcher_test.go`, `cluster_dispatcher_failover_test.go`, `cluster_txtoken_test.go` (each as far as stream P has not already)
- Modify: `internal/grpc/dispatch.go` (`DispatchProcessor`, `DispatchCriteria`, `DispatchFunction`, `runSingleTry`, `singleTryNumberer`, the `uuids` field and parameter, the `ErrNoMatchingMember` alias — all deleted)
- Modify: `internal/grpc/run_local.go`, `run_local_test.go`, `run_local_deadline_test.go` (`ErrNoMatchingMember` → `contract.ErrNoMatchingMember`)
- Modify: `internal/grpc/dispatch_test.go` (the fifteen `dispatcher.Dispatch*(` calls; `setupTestDispatcher`, `newTestDispatcher`, `newWedgedDispatcher` and the other `NewProcessorDispatcher(` sites lose the `uuids` argument), `internal/grpc/scheduled_function_rpc_test.go`, L-10's pass test file, `internal/callout/helpers_test.go` (same argument)
- Modify: `internal/e2e/dispatch_infra_error_test.go` (three comments naming `grpc.ErrNoMatchingMember`)
- Modify: `e2e/parity/multinode/callback_route.go:20, 151`, `txctl_joined_forward.go:27` (comments naming `ClusterDispatcher`)
- Modify: `docs/ARCHITECTURE.md` (§4.3 strategy table, algorithm, error table; §5.4 first sentence; DD-9), `cmd/cyoda/help/content/grpc.md:412`
- Test: `app/callout_wiring_test.go`

**Interfaces:**
- Consumes:
  - **P**: `*clusterdispatch.PeerRouter` and its constructor (called `clusterdispatch.NewPeerRouter(a.nodeRegistry, a.selfNodeID, clusterdispatch.NewRandomSelector(), forwarder)` below — the argument list is P's; the seam fixes the type and its three methods only); the dispatch handler no longer taking a `contract.ExternalProcessingService` (it calls `RunLocal`). **Precondition, checked before starting:** `grep -rn '\.DispatchProcessor(\|\.DispatchCriteria(\|\.DispatchFunction(' internal/cluster/ --include='*.go' | grep -v _test.go | grep -v cluster_dispatcher.go` → no hits.
  - **F**: the process's one `*fence.Fence` (field `a.fence` below), built from `a.txGate`. If F builds it at `app.go:644` — after the wiring block — this task moves both lines above the block; nothing between `:440` and `:644` reads `a.txGate`.
  - **C**: `cfg.Callout.FixedNumRetries`, `cfg.Callout.HandoverAllowance`, `cfg.Cluster.DispatchWaitTimeout`.
  - **L**: `RunLocal` and everything the Coordinator uses; L-8's wrappers to delete.
- Produces: in every mode the engine's `contract.ExternalProcessingService` is `*callout.Coordinator` (wrapped by the tracing decorator when `CYODA_OTEL_ENABLED`), unless a test injected `cfg.ExternalProcessing`. `func NewProcessorDispatcher(registry *MemberRegistry, selector MemberSelector, signer *token.Signer, selfNodeID string, answerLimitDefault, answerLimitMax, passAllowance time.Duration) *ProcessorDispatcher` (no `uuids`). After this task **nothing reads `cfg.Cluster.TxTokenTTL`** — C-11 is unblocked — and nothing outside `internal/grpc` uses `WithTxToken` (L-11 needs P for the rest).

**Existing tests that pin the old behaviour — the decision for each.**

| Test | Decision |
|---|---|
| `internal/grpc/dispatch_test.go`: `TestDispatchProcessor_HappyPath` :72, `_NoMember` :180, `_Timeout` :204, `_NoAttachEntity` :243, `TestDispatchCriteria_MatchesTrue` :289, `_MatchesFalse` :331, `TestDispatchProcessor_ContextSurfacesAsParametersString` :398, `_EmptyContextOmitsParameters` :461, `TestDispatchCriteria_ContextSurfacesAsParametersString` :500, `_EmptyContextOmitsParameters` :555, `TestDispatchProcessor_AnnotationsNotSentToMember` :600, `_WarningPropagatesName` :869, `TestDispatchCriteria_FailurePropagatesName` :915, `TestDispatchFunction_HappyPath` :959, `_NoMember` :1017 | **stay, every assertion unchanged**; the one line that called the deleted method calls the test helper of the same shape (`dispatchProcessor(dispatcher, …)`, below), which is a builder plus `RunLocal(ctx, call, 1)`. They pin what one try sends and how its answer is read — true for every try of the loop. `_NoMember` :180 and :1017 assert `errors.Is(err, contract.ErrNoMatchingMember)` |
| `TestDispatchCalloutToMember_*` :650 … :1092, `TestDispatch_*` :1167 … :1224 | L's decision (rewritten to `tryOnce`, L-7); untouched here |
| `internal/grpc/frozen_member_test.go` :24, :206 | stay: they drive `Member.Send` and the keep-alive loop, never a dispatcher (L's finding) |
| `cluster_dispatcher_test.go`: `TestClusterDispatcher_LocalFirst` :148 | deleted with `ClusterDispatcher`; replaced by O-5 `TestOwner_LocalCnodeAnswers_NoHandOver` |
| `…_ForwardsToPeer` :219 | deleted; the owner's half is O-5 `TestOwner_OwnersCnodeFails_HandOverSucceeds` and `TestOwner_PeerCannotBeConnectedTo_…`; what the request carries is P's |
| `…_ForwardsPrincipalKind` :345 | deleted; building the hand-over request is `PeerRouter.HandOver`'s (P). **Check before deleting:** `grep -rln 'PrincipalKind' internal/cluster/dispatch/*_test.go` names a file other than the four deleted here |
| `…_NoMemberAnywhere` :407 | deleted; O-5 `TestOwner_NoPeerCanBeConnectedTo_NoLocalCnode_IsNoComputeMember`, O-4 `TestOwner_NoCnodeWithinThePatience_…` |
| `…_ForwardFailure` :477 | deleted; O-5 `TestOwner_HandOverAnswerLost` (both settings) |
| `…_RemintsPeerErrorTaxonomy` :546, `…_PeerLocalDispatchErrorTaxonomyPropagatesOverWire` :689 | deleted with `remintPeerError`; reading a peer's answer into a `CalloutFailure` is P's (§6 "How the owner reads an answer"). The generic "peer node dispatch failed" text goes: D12 sends the try's own text. **Check before deleting:** P's section lists a test for every `outcome` value |
| `cluster_dispatcher_failover_test.go`: `TestClusterDispatcher_FailoverOnTransportError` :88 | deleted; the behaviour **changes**: a connection that could not be opened → next peer, no try (O-5 `TestOwner_PeerCannotBeConnectedTo_…`); an error after the connection opened → a lost answer, one try, and the next peer only if repeat-safe (O-5 `TestOwner_HandOverAnswerLost`). The old test's "fail over on any transport error" is the double execution D1 removes |
| `…_FailoverOnPeerNoMember` :155 | deleted; an authenticated `no_handoff` is `Connected: false` (the seam) → O-5 `TestOwner_PeerCannotBeConnectedTo_…` |
| `…_NoFailoverOnExecutedCalloutFailure` :179 | deleted; O-5 `TestOwner_PeersCnodeFailed_…_NobodyElseIsAsked` and `TestOwner_HandOverAnswerLost/not repeat-safe` |
| `…_FailoverExhaustion` :211 | deleted; O-5 `TestOwner_NoPeerCanBeConnectedTo_…` (no try made) and `TestOwner_HandOverAnswerLost/repeat-safe` (tries made → `CALLOUT_FAILED`) |
| `…_CtxCancelledMidForwardKeepsTaxonomy` :284 | deleted; the behaviour **changes** by §5: the caller's context ending returns `ctx.Err()` unchanged (408 when it is the client's own limit), not a retryable 503. O-5 `TestOwner_CallerGoesAwayDuringAHandOver_CtxErrUnchanged` |
| `…_FailoverOverWire` :315 | deleted; over the wire it is P's integration test and stream S's M-layer scenario |
| `cluster_txtoken_test.go`: `TestBuildProcessorRequest_CarriesOwnerToken` | deleted with `buildProcessorRequest` and `DispatchCalloutRequest.TxToken` (§7 "Claims") |
| `integration_test.go`, `integration_txtoken_test.go` (they build a `ClusterDispatcher` over an `httptest` peer) | P's: they test the wire. If they still name `ClusterDispatcher` when this task starts, P has not finished and this task waits |

Test helpers that live in the deleted files and are used elsewhere in package `dispatch` (`stubNodeRegistry`, `testContext`, `testEntity`, `testProcessor`, `testCriterion`, `containsStr` — read by `integration_test.go`, `integration_txtoken_test.go`) move, unchanged, to a new `internal/cluster/dispatch/helpers_internal_test.go` as far as a surviving file still reads them; `fakeForwarder`, `stubDispatcher`, `firstSelector`, `scriptedForwarder`, `twoPeerRegistry`, `noMemberResponse`, `cancellingForwarder`, `contains` are deleted with their only users.

- [ ] **Step 1: Write the failing test**

`app/callout_wiring_test.go`:

```go
package app

import (
	"os"
	"regexp"
	"testing"
)

// The owner's loop is the one ExternalProcessingService app.go builds, in both
// modes. Reviewed rather than exercised: app.New starts listeners and a
// scheduler, and what it hands the engine is not observable from outside
// without a hook — so this pins the source, the way
// internal/txgate/suspend_call_sites_test.go pins the Suspend call sites.
func TestAppWiresTheOwnersLoopInBothModes(t *testing.T) {
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatal(err)
	}
	for name, re := range map[string]*regexp.Regexp{
		"the Coordinator is built":                regexp.MustCompile(`extProc = callout\.New\(`),
		"the tracing decorator wraps the outside": regexp.MustCompile(`(?s)extProc = callout\.New\(.*extProc = observability\.NewTracingExternalProcessingService\(extProc,`),
	} {
		if !re.Match(src) {
			t.Errorf("app.go: %s — pattern %s not found", name, re)
		}
	}
	for name, re := range map[string]*regexp.Regexp{
		"ClusterDispatcher":                   regexp.MustCompile(`ClusterDispatcher`),
		"the dispatcher used as the service":  regexp.MustCompile(`extProc = localDispatcher`),
		"the retired pass lifetime is read":   regexp.MustCompile(`TxTokenTTL`),
		"a typed nil router would not be nil": regexp.MustCompile(`var peers \*clusterdispatch\.PeerRouter`),
	} {
		if re.Match(src) {
			t.Errorf("app.go still has: %s (%s)", name, re)
		}
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./app/... -run 'TestAppWiresTheOwnersLoopInBothModes'`   Expected: FAIL with `the Coordinator is built — pattern … not found`, `app.go still has: ClusterDispatcher`, `… TxTokenTTL`, `… extProc = localDispatcher`.

- [ ] **Step 3: Implement**

`app/app.go`. The imports gain `"github.com/cyoda-platform/cyoda-go/internal/callout"`. Line 441 (as L-10 left it) loses the generator:

```go
	localDispatcher := internalgrpc.NewProcessorDispatcher(a.memberRegistry, internalgrpc.NewRoundRobinSelector(a.memberRegistry), a.tokenSigner, a.selfNodeID, cfg.Callout.ResponseTimeout, cfg.Callout.ResponseTimeoutMax, cfg.Callout.PassAllowance)
```

The wiring block — before (`:537-558`):

```go
	if cfg.ExternalProcessing != nil {
		extProc = cfg.ExternalProcessing
	} else if cfg.Cluster.Enabled {
		forwarder := clusterdispatch.NewHTTPForwarder(peerAuth, cfg.Cluster.DispatchForwardTimeout)
		if cfg.Cluster.DispatchAllowLoopback {
			// Test-only: multi-node E2E fixtures run every node on 127.0.0.1.
			// Never set in production (SSRF guard stays active by default).
			forwarder = forwarder.AllowLoopbackForTesting()
		}
		extProc = clusterdispatch.NewClusterDispatcher(
			localDispatcher,
			a.nodeRegistry,
			cfg.Cluster.NodeID,
			clusterdispatch.NewRandomSelector(),
			forwarder,
			cfg.Cluster.DispatchWaitTimeout,
			a.tokenSigner,
			cfg.Cluster.TxTokenTTL,
		)
	} else {
		extProc = localDispatcher
	}
```

after (the forwarder lines are whatever stream P left there — shown as they are today; this task owns the rest):

```go
	if cfg.ExternalProcessing != nil {
		extProc = cfg.ExternalProcessing
	} else {
		// The owner's loop, in both modes. On a single pnode it has no peer
		// router: peers stays a nil interface (a nil *PeerRouter would not be).
		var peers callout.PeerRouter
		if cfg.Cluster.Enabled {
			forwarder := clusterdispatch.NewHTTPForwarder(peerAuth, cfg.Cluster.DispatchForwardTimeout)
			if cfg.Cluster.DispatchAllowLoopback {
				// Test-only: multi-node E2E fixtures run every node on 127.0.0.1.
				// Never set in production (SSRF guard stays active by default).
				forwarder = forwarder.AllowLoopbackForTesting()
			}
			peers = clusterdispatch.NewPeerRouter(a.nodeRegistry, a.selfNodeID, clusterdispatch.NewRandomSelector(), forwarder)
		}
		extProc = callout.New(localDispatcher, a.memberRegistry, peers, a.fence, common.NewDefaultUUIDGenerator(), callout.Config{
			SelfNodeID:        a.selfNodeID,
			FixedNumRetries:   cfg.Callout.FixedNumRetries,
			Patience:          cfg.Cluster.DispatchWaitTimeout,
			HandoverAllowance: cfg.Callout.HandoverAllowance,
		})
	}
```

`a.txGate = txgate.New()` (`:644`) and F's fence line move up, directly above the comment `// Wire external processing dispatcher`:

```go
	// The per-transaction lock and the fence that waits on it are needed by the
	// owner's loop below as well as by the handlers further down.
	a.txGate = txgate.New()
	a.fence = fence.New(a.txGate)
```

`internal/cluster/dispatch/cluster_dispatcher.go` — deleted. If P's `PeerRouter` still reads `forwardFailedClientMessage` from it, the constant and its comment move to P's file first.

`internal/grpc/dispatch.go` — delete `DispatchProcessor`, `DispatchCriteria`, `DispatchFunction`, `runSingleTry`, `singleTryNumberer`, the alias

```go
var ErrNoMatchingMember = contract.ErrNoMatchingMember
```

with its comment, the field `uuids spi.UUIDGenerator`, and the constructor's `uuids` parameter and assignment; drop the `github.com/google/uuid` import if nothing else in the file uses it. In `run_local.go` and L's two test files write `contract.ErrNoMatchingMember`.

`internal/grpc/dispatch_test.go` — the helpers the fifteen tests now call, below `newTestDispatcher`:

```go
// oneTry is the local procedure with one try, armed the way an owner arms a
// callout: what these tests pin is what one try sends and how its answer is
// read.
func oneTry(d *ProcessorDispatcher, ctx context.Context, call Callout) (CalloutResult, error) {
	limit, failure := d.ResolveAnswerLimit(call.ResponseTimeoutMs)
	if failure != nil {
		return CalloutResult{}, failure
	}
	call.RequestID = uuid.NewString()
	call.AnswerLimit = limit
	call.OwnerNodeID = "node-test"
	call.Number = &countingNumberer{}
	res := d.RunLocal(ctx, call, 1)
	return res.Result, res.Err()
}

func dispatchProcessor(d *ProcessorDispatcher, ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName, transitionName, txID string) (*spi.Entity, error) {
	res, err := oneTry(d, ctx, NewProcessorCallout(spi.MustGetUserContext(ctx).Tenant.ID, entity, processor, workflowName, transitionName, txID))
	if err != nil {
		return nil, err
	}
	return res.Entity, nil
}

func dispatchCriteria(d *ProcessorDispatcher, ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target, workflowName, transitionName, processorName, txID string) (bool, string, error) {
	call, failure := NewCriteriaCallout(spi.MustGetUserContext(ctx).Tenant.ID, entity, criterion, target, workflowName, transitionName, processorName, txID)
	if failure != nil {
		return false, "", failure
	}
	res, err := oneTry(d, ctx, call)
	if err != nil {
		return false, "", err
	}
	return res.Matches, res.Reason, nil
}

func dispatchFunction(d *ProcessorDispatcher, ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction, workflowName, transitionName, txID string) (contract.FunctionResult, error) {
	res, err := oneTry(d, ctx, NewFunctionCallout(spi.MustGetUserContext(ctx).Tenant.ID, entity, fn, workflowName, transitionName, txID))
	if err != nil {
		return contract.FunctionResult{}, err
	}
	return res.Function, nil
}
```

and at each of the fifteen sites `X.DispatchProcessor(` becomes `dispatchProcessor(X, ` (likewise `Criteria`, `Function`), where `X` is the test's dispatcher variable. (`countingNumberer` is L-8's, in `run_local_test.go`; the file's imports gain `github.com/google/uuid`.)

`docs/ARCHITECTURE.md`, §4.3 — the strategy table's first row, the algorithm block and the error table become (present tense, no history):

````markdown
| Callout strategy | `contract.ExternalProcessingService` | `callout.Coordinator` | The owner's loop: own compute members first, then one peer after another |
````

````markdown
**The owner's loop (`internal/callout`).** The node that holds the operation's
transaction — the owner — runs every callout, in single-node and cluster mode
alike (a single node has no peer router):

```
tries := 1 for retryPolicy NONE, else 1 + CYODA_RETRY_FIXED_NUM_RETRIES
the callout gets one request id, and a deadline:
    tries × answer limit + CYODA_DISPATCH_WAIT_TIMEOUT + CYODA_CALLOUT_HANDOVER_ALLOWANCE
loop (one pass):
  1. Local procedure (ProcessorDispatcher.RunLocal): try the node's own
     matching compute members one after another, never the same one twice in a
     pass, chosen round robin.
       answered                         → done
       failed before the hand-off       → next member
       no answer after the hand-off     → next member only for a criterion, a
                                          function, or an idempotent processor
       member answered "failed"         → stop; its message and verdict go to the client
  2. With tries left: hand the callout over, with the tries left, to each alive
     peer advertising the tag for the tenant (PeerSelector order), each at most
     once per pass. The peer runs the local procedure only and never hands on.
       could not connect / peer has no member → no try used, next peer
       answer lost                            → one try used
  3. Nobody took the work: wait for a membership change (a member attaching
     here, a peer's tag list arriving) — event-driven, CYODA_DISPATCH_WAIT_TIMEOUT
     in total per callout, 0 disables — and start a new pass.
```

Before the work is given to another compute member the callout's fencing number
rises, which refuses the earlier member's callbacks, and the owner waits for a
callback of that member that is in progress (§ fencing).
````

````markdown
| Scenario | Behavior | Error Code |
|----------|----------|------------|
| No matching member anywhere within the patience, no try made | Fail after `CYODA_DISPATCH_WAIT_TIMEOUT` | `NO_COMPUTE_MEMBER_FOR_TAG` |
| Exactly one try made, and it failed | That try's own error | `DISPATCH_TIMEOUT`, `COMPUTE_MEMBER_DISCONNECTED`, `DISPATCH_FORWARD_FAILED` |
| More than one try made, none answered | The tries are listed | `CALLOUT_FAILED` |
| No answer after the hand-off, processor not idempotent | Stop after that try | the try's own code |
| A member answered "failed" | Stop; `400`, retryable if the member said so | `WORKFLOW_FAILED` |
| A peer cannot be connected to, or has no matching member | No try used; the next peer is asked | — |
| A hand-over's answer is lost | One try used; the next peer only for a repeat-safe callout | `DISPATCH_FORWARD_FAILED` when it is the only attempt |
| The client's `transactionTimeoutMillis` fires, or the client goes away | The callout ends at once, from a wait as from a try | `TRANSACTION_TIMEOUT` (408) / none |
````

§5.4, first sentence: `Processors are dispatched via the `ExternalProcessingService` SPI, implemented by the owner's loop, `callout.Coordinator` (see Section 4.3).`

DD-9 becomes:

````markdown
### DD-9: Event-Driven Wait for Missing Compute Members

**Context:** What to do when no compute member matches the required tags.

**Decision:** The owner's loop waits on change signals — the local member registry's and the cluster registry's `Changed()` channel, closed and replaced on every change — for up to `CYODA_DISPATCH_WAIT_TIMEOUT` (default 5s) in total per callout, in single-node and cluster mode alike. `0` disables waiting.

**Rationale:** Compute members may be joining or reconnecting; a brief wait avoids spurious failures. A signal wakes the callout the moment a member appears and costs nothing while nothing changes. The allowance is per callout, not per wait, so a callout's worst-case duration is known when it starts.
````

`cmd/cyoda/help/content/grpc.md:412` — the sentence naming `ClusterDispatcher` becomes:

```markdown
In cluster mode every node publishes the tags of its members per tenant, and the node that owns a request hands a callout over to a node that has a matching member when it has none of its own, or when its own did not take the work.
```

Comments in `e2e/parity/multinode/callback_route.go:20, 151` and `txctl_joined_forward.go:27`: "the ClusterDispatcher forwards" → "the owner hands the processor over". Comments in `internal/e2e/dispatch_infra_error_test.go:13, 88, 136`: `grpc.ErrNoMatchingMember` → `contract.ErrNoMatchingMember`.

- [ ] **Step 4: Run to verify GREEN**

Run: `go build ./...`, `go vet ./...`, `go test ./app/... ./internal/grpc/... ./internal/callout/... ./internal/cluster/... ./internal/observability/...`   Expected: PASS.

Exit checks (each → no hits):

```
grep -rn 'ClusterDispatcher' --include='*.go' .
grep -rn 'forwardWithFailover\|findPeerWithPolling\|remintPeerError\|gossipPollInterval\|isNoMatchingMember\|extractCriteriaTags' --include='*.go' .
grep -rn 'runSingleTry\|singleTryNumberer' internal/
grep -rn 'func (d \*ProcessorDispatcher) Dispatch' internal/grpc/
grep -rn 'grpc\.ErrNoMatchingMember' --include='*.go' .
grep -n  '^var ErrNoMatchingMember' internal/grpc/*.go
grep -n  'uuids' internal/grpc/dispatch.go
grep -rn 'TxTokenTTL' --include='*.go' . | grep -v '^./app/config'
grep -rn 'ClusterDispatcher' docs/ARCHITECTURE.md cmd/cyoda/help/content/ README.md CONTRIBUTING.md
grep -rn 'Begin(\|fence\.' internal/grpc/dispatch.go internal/grpc/run_local.go
```

The last one is the check for a temporary begin/end wrapper the fencing stream may have put around the three entry points so that callbacks kept working before this task: the entry points are gone, and `internal/grpc`'s dispatcher must not touch the fence at all — the arbiter is driven by the owner's loop only. If it has hits, delete what it finds; `grep -rn 'fence\.' internal/grpc/*.go | grep -v _test.go` may then still list `txroute_interceptor.go` (F's `JoinFromToken` call), and nothing else.

- [ ] **Step 5: Commit**

```
git add -A app internal/cluster/dispatch internal/grpc internal/callout internal/e2e/dispatch_infra_error_test.go e2e/parity/multinode docs/ARCHITECTURE.md cmd/cyoda/help/content/grpc.md
git commit -m "feat(app): the owner's loop is the callout service in both modes; ClusterDispatcher and the single-try entry points are gone (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task O-9: the help says what a callout failure means, and the CHANGELOG says what changed

**Spec:** §8.2 last paragraph ("`retryable: true` on a `NoAnswer` failure speaks for cyoda's state only … The help topics for `DISPATCH_TIMEOUT`, `COMPUTE_MEMBER_DISCONNECTED` and `WORKFLOW_FAILED` say so"); §8.1; §14 "`errors/WORKFLOW_FAILED.md`, `DISPATCH_TIMEOUT.md`, `COMPUTE_MEMBER_DISCONNECTED.md`, `NO_COMPUTE_MEMBER_FOR_TAG.md`, `DISPATCH_FORWARD_FAILED.md` (revised); `errors.md` index, and its false sentence about gRPC trailer metadata corrected"; §14 CHANGELOG; §15 `### Breaking` (this stream's entries).

**Files:**
- Modify: `cmd/cyoda/help/content/errors/WORKFLOW_FAILED.md`, `DISPATCH_TIMEOUT.md`, `COMPUTE_MEMBER_DISCONNECTED.md`, `NO_COMPUTE_MEMBER_FOR_TAG.md`, `DISPATCH_FORWARD_FAILED.md`
- Modify: `cmd/cyoda/help/content/errors.md` (SYNOPSIS: the gRPC example and the trailer sentence; DESCRIPTION: the `retryable` paragraph; five index rows)
- Modify: `CHANGELOG.md` (`[Unreleased]`)
- Test: `cmd/cyoda/help/callout_errors_help_test.go`

**Interfaces:**
- Consumes: O-1 (`errors.CALLOUT_FAILED`), O-2 (the conditional verdict), O-4 (single-pnode patience), C (the setting names `CYODA_RETRY_FIXED_NUM_RETRIES`, `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS`, `CYODA_CALLOUT_HANDOVER_ALLOWANCE`, `CYODA_DISPATCH_CONNECT_TIMEOUT` exist in `config_registry.go` — `TestConfig_EnvVarCoverage` reads help text).
- Produces: nothing other streams consume. `errors/CALLOUT_SUPERSEDED.md` and its index row are stream F's.

One paragraph is the same in the three topics the spec names, and in `CALLOUT_FAILED.md` and `DISPATCH_FORWARD_FAILED.md`, because a client reads one topic, not five. It is called **the state paragraph** below:

```markdown
`retryable: true` speaks for Cyoda's state only. With a `SYNC` or `ASYNC_SAME_TX` processor, a criterion, or a schedule function, the operation failed and its transaction was rolled back, so running it again starts clean — unless an earlier `COMMIT_BEFORE_DISPATCH` processor of the same request had already committed. With a `COMMIT_BEFORE_DISPATCH` processor the part of the operation before the callout stays committed. (An `ASYNC_NEW_TX` processor's failure never reaches the client: the operation continues.) A compute member that was handed the work may have carried it out; what it did outside Cyoda is the application's to reconcile.
```

- [ ] **Step 1: Write the failing test**

`TestErrCode_Parity` checks that topics exist, not what they say. This test pins the statements the spec requires, and the two false ones that must go.

`cmd/cyoda/help/callout_errors_help_test.go` (`repoRoot` is the package's own helper, the one `TestErrCode_Parity` uses):

```go
package help

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readTopic(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd/cyoda/help/content", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func TestCalloutErrorTopics_SayWhatRetryableSpeaksFor(t *testing.T) {
	for _, rel := range []string{
		"errors/DISPATCH_TIMEOUT.md", "errors/COMPUTE_MEMBER_DISCONNECTED.md", "errors/WORKFLOW_FAILED.md",
		"errors/DISPATCH_FORWARD_FAILED.md", "errors/CALLOUT_FAILED.md",
	} {
		body := readTopic(t, rel)
		for _, want := range []string{"speaks for Cyoda's state only", "the application's to reconcile"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q", rel, want)
			}
		}
	}
}

func TestCalloutErrorTopics_StatementsThatBecameFalseAreGone(t *testing.T) {
	gone := map[string][]string{
		"errors.md":                             {"trailer metadata"},
		"errors/WORKFLOW_FAILED.md":             {"Retryable: `no`"},
		"errors/DISPATCH_FORWARD_FAILED.md":     {"The operation has not been executed on the target node"},
		"errors/COMPUTE_MEMBER_DISCONNECTED.md": {"The cluster re-routes to an available member"},
		"errors/DISPATCH_TIMEOUT.md":            {"default `30000` ms", "govern cross-node forwarding between cluster nodes, not this timeout"},
		"errors/NO_COMPUTE_MEMBER_FOR_TAG.md":   {"no live cluster node advertising"},
	}
	for rel, phrases := range gone {
		body := readTopic(t, rel)
		for _, phrase := range phrases {
			if strings.Contains(body, phrase) {
				t.Errorf("%s still says %q", rel, phrase)
			}
		}
	}
}

func TestErrorsIndex_WorkflowFailedIsConditionallyRetryable(t *testing.T) {
	index := readTopic(t, "errors.md")
	if !strings.Contains(index, "- `errors.WORKFLOW_FAILED` — `400` — retryable only when the compute member said so") {
		t.Error("errors.md: the WORKFLOW_FAILED row must say when it is retryable")
	}
	if !strings.Contains(index, `"code": "CLIENT_ERROR"`) {
		t.Error("errors.md: the gRPC example must show the envelope as it is — code CLIENT_ERROR, the error code as the message's prefix")
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./cmd/cyoda/help/... -run 'TestCalloutErrorTopics|TestErrorsIndex_WorkflowFailed'`   Expected: FAIL — `errors/DISPATCH_TIMEOUT.md: missing "speaks for Cyoda's state only"` (and three more topics; `CALLOUT_FAILED.md` passes since O-1), `errors.md still says "trailer metadata"`, and the rest of the second and third test.

- [ ] **Step 3: Implement**

`errors.md`, SYNOPSIS — before:

````markdown
gRPC error envelope example (returned in the CloudEvent response payload):

```json
{
  "error": {
    "code": "ENTITY_NOT_FOUND",
    "message": "entity id=abc not found",
    "retryable": false
  }
}
```

The gRPC response also carries `errorCode` and `retryable` in trailer metadata.
````

after:

````markdown
gRPC error envelope example (returned in the CloudEvent response payload):

```json
{
  "success": false,
  "error": {
    "code": "CLIENT_ERROR",
    "message": "ENTITY_NOT_FOUND: entity id=abc not found"
  }
}
```

The envelope's `code` is coarse — `CLIENT_ERROR` for a `4xx` and for a retryable `503`, `SERVER_ERROR` for a ticketed `5xx`. The error code of this index is the prefix of `message`, up to the first colon. `retryable` is present, and `true`, only when the operation may be run again as it is; it is omitted otherwise. Nothing is carried in gRPC metadata or trailers.
````

`errors.md`, DESCRIPTION — the `retryable` paragraph gains a sentence:

```markdown
The `retryable` property is present and `true` only when the operation is safe to retry as-is (e.g., transient cluster conditions). When absent or `false`, the request or system state must change before retrying. For a failed processor, criterion or function callout, `retryable: true` speaks for Cyoda's own state only — see `errors.DISPATCH_TIMEOUT`.
```

`errors.md`, ERROR CODE INDEX — five rows become:

```markdown
- `errors.COMPUTE_MEMBER_DISCONNECTED` — `503` — retryable — the compute member tried for a callout went away, before or after it was handed the work
- `errors.DISPATCH_FORWARD_FAILED` — `503` — retryable — a callout was handed over to another cluster node and no usable answer came back; the work may have run
- `errors.DISPATCH_TIMEOUT` — `503` — retryable (see note) — a compute member did not take, or did not answer, a callout within its answer limit; the work may have run
- `errors.NO_COMPUTE_MEMBER_FOR_TAG` — `503` — retryable — no compute member for the required tag appeared, on any cluster node, within `CYODA_DISPATCH_WAIT_TIMEOUT`; no try was made
- `errors.WORKFLOW_FAILED` — `400` — retryable only when the compute member said so — a workflow processor, criterion or schedule function returned a failure, or the workflow configuration cannot be evaluated
```

`errors/WORKFLOW_FAILED.md` — SYNOPSIS becomes:

```markdown
HTTP: `400` `Bad Request`. Retryable: only when the compute member that failed said so.
```

In DESCRIPTION, the first paragraph is followed by:

```markdown
When the failure is a compute member's own answer — a processor, a function criterion or a schedule function that answered `success: false` — the detail is the member's own message behind the step that failed (`processor <name> failed: <message>`, `failed to evaluate transition criterion: <message>`, `schedule function <name> failed: <message>`), and the response is marked `retryable: true` exactly when the member's error carried `retryable: true`. A member that answered is never replaced by another member: its answer is the callout's outcome, whatever `retryPolicy` says. This holds when the member is attached to another cluster node, too.
```

the closing paragraph ("Not retryable unless the underlying condition has changed. …") becomes:

```markdown
Without the member's `retryable: true` the failure is not retryable unless the underlying condition has changed: it originates from application logic in the processor, or from the workflow configuration; the data, the processor implementation, or the workflow configuration determines the outcome.
```

followed by **the state paragraph**; `see_also` and SEE ALSO gain `errors.CALLOUT_FAILED`.

`errors/DISPATCH_TIMEOUT.md` — NAME and DESCRIPTION become:

```markdown
## NAME

DISPATCH_TIMEOUT — a compute member did not take, or did not answer, a processor, criterion or function callout within the callout's answer limit.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

The answer limit is the callout's own `responseTimeoutMs` (a field on the processor, criterion or function config), or `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS` when that is not set.

The error message names which of two phases ran out of time:

- **member not draining** — the compute member had stopped reading its stream, so the request could not even be handed to it. The work provably never left the node, and another compute member is tried, whatever the callout is.
- **no response** — the member took the request but did not answer in time. The work may have run. Another member is tried only for a criterion, a function, or a processor whose configuration declares `idempotent: true`.

A message that says **cut off at the callout deadline** is the same failure, caused by the limit on the time one callout may take as a whole rather than by one member's answer limit.

This code reaches the client when it was the only try made — `retryPolicy: NONE`, a processor that is not idempotent, or no other compute member to try. When more than one try failed the code is `errors.CALLOUT_FAILED`, which lists them.

A member that is not draining is evicted within `CYODA_KEEPALIVE_TIMEOUT` seconds.

Retryable. **The state paragraph.**

If timeouts recur, check compute member load and network latency. `CYODA_DISPATCH_WAIT_TIMEOUT` is how long a callout waits for a compute member to exist; it is not this limit — see `cyoda help config cluster`.
```

(`see_also` and SEE ALSO gain `errors.CALLOUT_FAILED`. "**The state paragraph.**" stands for the paragraph above, written out.)

`errors/COMPUTE_MEMBER_DISCONNECTED.md` — NAME and DESCRIPTION become:

```markdown
## NAME

COMPUTE_MEMBER_DISCONNECTED — the compute member tried for a processor, criterion or function callout went away.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

The compute member chosen for a callout disconnected — before it was handed the work, or after it and before it answered. In the second case the work may or may not have been carried out.

The server also raises this code when it evicts a member itself — after `CYODA_KEEPALIVE_TIMEOUT` seconds of inbound silence, or when one write to the member has stalled that long — and every callout in flight on that member fails with this code. It is also the outcome when a member leaves in the instant between being chosen and being handed the request.

Before the hand-off, another compute member is always tried. After it, another member is tried only for a criterion, a function, or a processor whose configuration declares `idempotent: true`. This code reaches the client when it was the only try made; when more than one try failed the code is `errors.CALLOUT_FAILED`.

Retryable. **The state paragraph.** Persistent failures indicate insufficient compute capacity for the required tags.
```

(`see_also` and SEE ALSO gain `errors.CALLOUT_FAILED`.)

`errors/NO_COMPUTE_MEMBER_FOR_TAG.md` — NAME and DESCRIPTION become:

```markdown
## NAME

NO_COMPUTE_MEMBER_FOR_TAG — no compute member for the required tag and tenant appeared, on any cluster node, within the time a callout waits for one.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

A processor, criterion or function callout goes to a compute member of the caller's tenant that declares at least one of the callout's `calculationNodesTags`. When the node that owns the request has none, and no other cluster node advertises one that can be reached, the callout waits for one to appear — for `CYODA_DISPATCH_WAIT_TIMEOUT` in total (default `5s`; `0` disables waiting), on a single node as in a cluster, whatever the callout's `retryPolicy`. The wait ends the moment a member attaches. If none does, the operation is rejected with this error.

No try was made: nothing was sent to any compute member, and the operation's transaction was rolled back. When a callout did make tries and then ran out of members, the error reports the tries instead (`errors.CALLOUT_FAILED`, or the single try's own code).

Retryable after compute capacity is restored.
```

(`see_also` and SEE ALSO gain `errors.CALLOUT_FAILED`.)

`errors/DISPATCH_FORWARD_FAILED.md` — NAME and DESCRIPTION become:

```markdown
## NAME

DISPATCH_FORWARD_FAILED — a callout was handed over to another cluster node and no usable answer came back.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`.

## DESCRIPTION

The node that owns a request hands a callout over to another cluster node when that node has a matching compute member. This code means the connection to that node was opened and then no usable answer arrived: no reply within the owner's wait, a broken connection, a non-`2xx` status, or an answer that does not authenticate. **The work may have been given to a compute member there, and may have run.** A node that cannot be connected to at all is not this error — the next node is asked, and no try is used.

Because the work may have run, the callout moves on to another node only if it is a criterion, a function, or a processor whose configuration declares `idempotent: true`. This code reaches the client when the lost answer was the only try made; when more than one try failed the code is `errors.CALLOUT_FAILED`, where a lost answer appears as `member<->`.

The message never names the other node or its address.

Retryable. **The state paragraph.** Persistent failures indicate inter-node network or peer node health issues; pnode clocks more than 30 s apart show up as this error, too.
```

(`see_also` and SEE ALSO gain `errors.CALLOUT_FAILED`.)

`CHANGELOG.md`, `[Unreleased]`:

```markdown
### Added

- **A callout is tried on more than one compute member.** `retryPolicy` selects the number of tries for a processor, a criterion and a schedule function: `NONE` is one try, `FIXED` or unset is one plus `CYODA_RETRY_FIXED_NUM_RETRIES`. A member that could not be handed the work is always replaced; after the hand-off, only a criterion, a function, or a processor declaring `idempotent: true` moves on. The node that owns the request tries its own members first and then hands the callout, with the tries left, to one cluster node after another. New error code `CALLOUT_FAILED` (`503`, retryable) lists the failed tries when there was more than one. See `docs/cloud-parity/callout-failover.md`.
- **A compute member's `retryable: true` reaches the client**: `WORKFLOW_FAILED` is marked retryable exactly when the member that failed said so, on HTTP and in the gRPC envelope, and across cluster nodes.
- Metrics `cyoda.callout.tries`, `cyoda.callout.handovers`, `cyoda.callout.wait.duration`; span attributes `callout.tries`, `callout.handover`, `callout.waited_ms`.

### Changed

- `CYODA_DISPATCH_WAIT_TIMEOUT` is how long a callout waits for a compute member to exist — once per callout, event-driven, on a single node as in a cluster.
- A schedule function's failure names the function: `schedule function <name> failed: <the member's message>`.
- `cyoda.dispatch.duration` measures a whole callout, all tries included; its buckets run to 300 s.
- A request cancelled by its client during a cross-node callout ends with the request's own cancellation (408 `TRANSACTION_TIMEOUT` when the client's limit fired), no longer with a retryable `DISPATCH_FORWARD_FAILED`.

### Breaking

- **A single node with no matching compute member waits out `CYODA_DISPATCH_WAIT_TIMEOUT` (default 5 s) before failing** with `NO_COMPUTE_MEMBER_FOR_TAG`; it used to fail at once. Set the value to `0` to keep the old behaviour.

### Fixed

- The help index claimed gRPC responses carry `errorCode` and `retryable` in trailer metadata, and showed an envelope `code` that is never sent. Neither was true; `errors` now shows the envelope as it is.
```

(Entries of other streams — the settings, `idempotent`, schema 1.5, fencing, membership — are theirs; if a heading already exists under `[Unreleased]`, append to it.)

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./cmd/cyoda/help/...`   Expected: PASS — the three new tests, `TestErrCode_Parity`, `TestConfig_EnvVarCoverage` (every `CYODA_*` named above is in stream C's registry), and the see-also checks.

- [ ] **Step 5: Commit**

```
git add cmd/cyoda/help/content/errors.md cmd/cyoda/help/content/errors cmd/cyoda/help/callout_errors_help_test.go CHANGELOG.md
git commit -m "docs(help): what a callout failure means, when WORKFLOW_FAILED is retryable, and the gRPC envelope as it is (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## Coverage matrix — this stream's rows (§13)

| Scenario | U | G | Left to stream S |
|---|---|---|---|
| `MemberFailed` verdict true → 400 retryable, one try | O-2 `TestClassifyWorkflowError_MemberFailed_CarriesTheVerdict`; O-3 `TestOwner_MemberFailed_OneTry_AndTheVerdictIsKept`; O-5 `TestOwner_PeersCnodeFailed_…` (across pnodes, D12) | O-2 (the assertion added to L-7's `TestRPC_ProcessorMemberFailed_…`) | E, P |
| `MemberFailed` verdict false / absent → 400, one try | same tests, other rows | same | E, P |
| `Terminal` → stop, as today | O-2 `TestClassifyWorkflowError_TerminalWithoutCodeStays400NotRetryable`, `…_OtherCalloutFailureKindsPassThrough` (L-7/L-8 hold the one-try half) | L-5 | E |
| `retryPolicy: NONE` on a processor, a criterion, a function → one try | O-3 `TestOwner_RetryPolicyNone_OneTry_OnEveryKindOfCallout`; `…FixedOrUnset_OneTryPlusTheConfiguredRetries` | — | E, P |
| Every try used → 503 `CALLOUT_FAILED`, message shape | O-1 `TestAttemptsMessage`, `TestAttemptsFailure_MoreThanOneAttempt_…`; O-3 `TestOwner_EveryTryUsed_IsCalloutFailedAndListsTheTries` | O-7 `TestRPC_Function_EveryTryUsed_CalloutFailedEnvelope` | E, P |
| Exactly one attempt recorded → not wrapped | O-1 `TestAttemptsFailure_ExactlyOneAttempt_…`; O-3 `TestOwner_ExactlyOneAttemptRecorded_IsNotWrapped` | — | E |
| Attempts on record beat "no cnode" | O-3 `TestOwner_AttemptsOnRecordBeatNoCnode`; O-4 `TestOwner_PatienceSpentWithAnAttemptOnRecord_ReportsTheAttempt` | — | E |
| Same request id on every try | O-3 `TestOwner_SameRequestIDOnEveryTry`; O-5 `TestOwner_OwnersCnodeFails_HandOverSucceeds` (the hand-over carries it) | — | E |
| Callout deadline cuts off a try in progress | O-5 `TestOwner_CalloutDeadline_CutsOffATryInProgress` (L-9 holds the mechanism) | — | — |
| Client timeout (408) / cancellation during a wait and during a try | O-3 `TestOwner_CallerGoesAwayDuringATry_CtxErrUnchanged`; O-4 `TestOwner_CallerGoesAwayDuringAWait_EndsAtOnce`; O-5 `TestOwner_CallerGoesAwayDuringAHandOver_…` | O-7 `TestRPC_ClientTimeoutDuringAWaitAndDuringATry_…`, `TestRPC_ClientGoesAwayDuringAWait_…` | E |
| No cnode → waits, one attaches → succeeds, no try used | O-4 `TestOwner_NoCnode_Waits_OneAttaches_Succeeds_NoTryUsed` | — | E (P, M waived w²) |
| Patience is one allowance across several waits | O-4 `TestOwner_PatienceIsOneAllowanceAcrossSeveralWaits` | — | — |
| Patience applies with `retryPolicy: NONE` | O-4 `TestOwner_NoCnode_Waits_…` (it runs with `NONE`) | — | E |
| No cnode within patience → 503; patience 0 → at once | O-4 `TestOwner_NoCnodeWithinThePatience_IsNoComputeMember`; O-3 `TestOwner_NoCnode_NoPatience_IsNoComputeMemberAtOnce` | O-7 `TestRPC_NoCnodeWithinThePatience_NoComputeMemberEnvelope` (both settings) | E, P |
| `ASYNC_NEW_TX`: callout fails → operation succeeds, nothing reported | O-2 `TestCalloutFailure_AsyncNewTx_OperationContinuesNothingReported` | — | E |
| `COMMIT_BEFORE_DISPATCH`, both variants: failure leaves TX_pre committed | O-2 `TestCalloutFailure_CommitBeforeDispatch_FailsAndLeavesTxPreCommitted` | — | E |
| Callouts made from a scheduled fire follow the same rules | O-3 `TestOwner_CalloutFromAScheduledFire_FollowsTheSameRules`; V-2 stated | — | E |
| Owner gives the work to a second cnode of its own → the first cnode's callback is refused at once | O-3 `TestOwner_SecondCnode_TheFirstIsShutOutAndWaitedForBeforeTheNextHandOff` (the `Advance` order against a real fence and a real lock) — F holds the refusal itself | F | E |
| A Coordinator released by the fence reports `CALLOUT_SUPERSEDED`, a client that went away as today | O-3 `TestOwner_ReleasedByTheFenceDuringATry_…`; O-4 `…DuringAWait_…`; the `CtxErrUnchanged` tests | — | — |
| Callout ended by a panic → its passes are refused | O-5 `TestOwner_PanicInsideTheLoop_TheCalloutIsStillEnded`; O-3 `TestOwner_WhenTheCalloutHasEnded_ItsPassesAreRefused` | — | — |
| Two tenants share a tag on one pnode (owner's view: attempts name only that tenant's cnodes) | O-3 `TestOwner_TwoTenantsShareATag_…` | — | E, M |
| Owner's cnode fails → hand-over succeeds | O-5 `TestOwner_OwnersCnodeFails_HandOverSucceeds`, `TestOwner_LocalCnodeAnswers_NoHandOver`, `TestOwner_LocalNoAnswer_ProcessorNotIdempotent_IsNotHandedOver` | — | M |
| Hand-over answer lost → one try counted; not repeat-safe → 503 `DISPATCH_FORWARD_FAILED` (owner's half) | O-5 `TestOwner_HandOverAnswerLost` | — | — |
| Peer cannot be connected to → no try used, next peer (owner's half) | O-5 `TestOwner_PeerCannotBeConnectedTo_UsesNoTry_NextPeerIsAsked`, `TestOwner_ANewPassAsksEveryPeerAgain` | — | (M waived w³) |
| No peer can be connected to, no local cnode → 503 `NO_COMPUTE_MEMBER_FOR_TAG` | O-5 `TestOwner_NoPeerCanBeConnectedTo_NoLocalCnode_IsNoComputeMember` | — | — |
| cnode message and verdict survive the hand-over (owner's half) | O-5 `TestOwner_PeersCnodeFailed_ItsMessageAndVerdictSurvive_…` + O-2 | — | M |

Concurrency: the tests that involve two goroutines (`…SecondCnode…`, the `AfterFunc` attach/cancel tests) assert an order the code guarantees — the number rises and the lock is waited for *before* the next hand-off; a change ends a wait — never an interleaving. They run under `make race` with the rest of `internal/callout`.

**What stream S can rely on.** Everything under "Produces" in O-1 … O-5, in short: the client message of `CALLOUT_FAILED` (O-1); `WORKFLOW_FAILED` message shapes and the conditional `retryable` (O-2); tries, stop rules and precedence (O-3); patience applies everywhere, including a single pnode and `retryPolicy: NONE`, and a harness that wants the old immediate failure sets `CYODA_DISPATCH_WAIT_TIMEOUT=0` or low (O-4); a hand-over is made only after the owner's own cnodes, only with tries left, only when the failure permits another cnode (O-5); the one INFO line (`msg="callout needed more than one try or waited"`, `pkg=callout`, fields `kind name tenantId tags entityId requestId tries handOvers waitedMs elapsedMs succeeded`) can be grepped in a node's log by an M-layer scenario to prove where tries were made (O-6).

## Stream interface summary

**Other streams may consume from O**

`internal/common`:
- `const ErrCodeCalloutFailed = "CALLOUT_FAILED"` (503, retryable; help topic `errors.CALLOUT_FAILED`).

`internal/contract`:
- `type CalloutStats struct { Tries []string; HandOvers []string; Waited time.Duration }`; `func WithCalloutStats(ctx context.Context) (context.Context, *CalloutStats)`; `func CalloutStatsFrom(ctx context.Context) *CalloutStats`; `const CalloutOutcomeOK = "ok"`, `CalloutOutcomeAbandoned = "abandoned"`, `CalloutOutcomeUnreachable = "unreachable"`.

`internal/grpc`:
- `Callout.RetryPolicy string`, set by the three builders (O-3). Stream P's hand-over does **not** need it: the owner sends `triesLeft`.
- `func NewProcessorDispatcher(registry *MemberRegistry, selector MemberSelector, signer *token.Signer, selfNodeID string, answerLimitDefault, answerLimitMax, passAllowance time.Duration) *ProcessorDispatcher` — no `uuids` after O-8. `*ProcessorDispatcher` is no longer a `contract.ExternalProcessingService` after O-8.
- Test-only: `OwnerTestConfig`, `NewOwnerForTest`, `newTestEnvWithOwner` (O-7).

`internal/callout`:
- `type PeerRouter interface { Peers(tenantID, tagsCSV string) []contract.NodeInfo; HandOver(ctx context.Context, peer contract.NodeInfo, call internalgrpc.Callout, triesLeft int, major uint32) dispatch.HandOverAnswer; Changed() <-chan struct{} }`
- `type Config struct { SelfNodeID string; FixedNumRetries int; Patience, HandoverAllowance time.Duration }`
- `func New(local *internalgrpc.ProcessorDispatcher, members *internalgrpc.MemberRegistry, peers PeerRouter, f *fence.Fence, uuids spi.UUIDGenerator, cfg Config) *Coordinator`; `*Coordinator` implements `contract.ExternalProcessingService`.
- How the owner uses the seam (binding for P): listed under O-5 "Produces".

`internal/domain/entity`: a `*contract.CalloutFailure` of kind `MemberFailed` anywhere in a workflow error's chain → `400 WORKFLOW_FAILED`, `.AsRetryable()` iff `*Retryable`. `internal/domain/workflow`: `armViaFunction` wraps its callout error as `schedule function <name> failed: %w`.

`internal/observability`: `AttrCalloutTries`, `AttrCalloutHandOver`, `AttrCalloutWaitedMs`, `AttrCalloutOutcome`; instruments `cyoda.callout.tries`, `cyoda.callout.handovers`, `cyoda.callout.wait.duration`.

**O consumes**

- **L**: everything listed under O-3 "Consumes"; L-9's contract for `contract.ErrCalloutDeadline`; L-8's wrappers, `runSingleTry`, `singleTryNumberer`, `uuids` (deleted in O-8); L-5/L-7/L-8's G tests and `newTestEnvWithDispatchLimits` (re-pointed in O-7).
- **F**: `fence.New(gate *txgate.Registry) *Fence`; `(*Fence).Begin(ctx, calloutID, txID string, outer []Pair) (context.Context, func())`; `(*Fence).Advance(calloutID string, major uint32)`; `(*Fence).Admit(ctx, pairs) (context.Context, error)` (tests); `fence.Pairs(ctx) []Pair`; `fence.Pair{Callout string; Major, Minor uint32}`; `fence.ErrSuperseded`; **`fence.NewSupersededError() *common.AppError`**; `token.Pair`, `token.Claims{Callout, Major, Minor}`; the app's one fence (`a.fence`), reachable where `app.go` wires the callout service. If `fence.Pair` and `token.Pair` end up one type, `passPairs` in `coordinator.go` becomes `return pairs` — it compiles either way as written.
- **P**: `dispatch.HandOverAnswer`, `*dispatch.PeerRouter` with `Peers`, `HandOver`, `Changed` exactly as in `planner-interfaces.md`, its constructor, and the dispatch handler calling `RunLocal` rather than a `contract.ExternalProcessingService` — before O-8.
- **C**: `cfg.Callout.FixedNumRetries`, `cfg.Callout.HandoverAllowance`, `cfg.Cluster.DispatchWaitTimeout`; `spi.ProcessorConfig.Idempotent`, `spi.ScheduleFunction.RetryPolicy`; `contract.ParseCriterionFunction`. C-11 runs after O-8.
- **M**: `contract.NodeRegistry.Changed()` — through P's `PeerRouter.Changed()` only.
- **H**: nothing (S consumes H).

## Open points

1. **§8.2 "`[member<id>: cause]`" — are the angle brackets literal?** Cloud's are (`ExternalizerBase.kt:539`: `"member<${it.memberId ?: "-"}>: …"`), and R§5 quotes `member<->` for the unknown member. Planned literally: `[member<m-1>: …]`, `[member<->: …]`. If the spec meant `<id>` as a placeholder, O-1's `attemptsMessage` and four test strings change.
2. **§8.2 "for a function the arming wrap" — there is none.** `armViaFunction` returns the callout's error unchanged (`arm.go:242-244`) and `engine.go:361-365` deliberately does not wrap ("Already self-describing"). A `MemberFailed` from an arming function would reach the client as the cnode's bare text. Planned (O-2): `schedule function <name> failed: %w` in `armViaFunction`, which leaves every `*AppError` intact. The wording is this plan's, not the spec's.
3. **§5 "if r stops → return failure" against §8.2 "Every try used, more than one attempt → `CALLOUT_FAILED`".** When a try whose kind forbids another cnode follows earlier attempts (a `NoHandOff`, then a `NoAnswer` on a processor that is not idempotent), the spec's two statements meet. Planned: the stopping failure is returned **as itself** (§8.2 rows "`NoAnswer`, processor not idempotent → the try's own code" and "`MemberFailed` → `WORKFLOW_FAILED`"), with all attempts on `CalloutFailure.Attempts` for whoever wants them; `CALLOUT_FAILED` is for "nothing more can be done" — tries used up, patience or deadline spent — with more than one attempt.
4. **The seam does not say who records a lost hand-over answer as an attempt, or what `HandOver` returns when the caller's context ends.** Planned (O-5, stated under its "Produces"): `Connected` with a `Failure` and no `Attempts` is recorded by the owner under member `-`; and the owner checks its own context after every `HandOver` and ignores the answer if the context ended for a cause other than the callout's deadline. The owner's wait is put on `ctx` with cause `contract.ErrCalloutDeadline`, so P can tell "the owner's wait ran out" (a lost answer, one try) from "the caller went away" the way `RunLocal` does.
5. **`fence.NewSupersededError()` is not in `planner-interfaces.md`.** It is in stream F's draft (`internal/fence/fence.go`), and the Coordinator needs exactly that value — 410 `CALLOUT_SUPERSEDED` with `ErrSuperseded` as cause — for "released by the fence". If F does not export it, O-3's `ended` builds the same `common.Operational(http.StatusGone, common.ErrCodeCalloutSuperseded, …).WithCause(fence.ErrSuperseded)` and the message text is duplicated; better that F exports it.
6. **Sequencing with F and P around O-8** (L's Open point 4, restated with what this stream found). O-8 needs, already landed: P's `PeerRouter` and a dispatch handler that calls `RunLocal`; F's fence in `app.go`. And F's enforcement in `JoinFromToken` must not be active before O-8, because until O-8 nothing calls `Begin`. If F bridged that with a begin/end wrapper around the three `Dispatch*` entry points, O-8's last exit check finds and removes it. `app.go` builds `txgate.Registry` at `:644`, after the wiring block — whichever of F and O-8 lands first moves it up.
7. **§12 asks for the counters on "the existing metric naming and registration", which ties them to `CYODA_OTEL_ENABLED`.** The decorator — and with it every callout metric, old and new — does not exist when OTLP push is off (`app.go:559`), unlike the pool metrics, which are always on. Kept as it is (V-3); if callout failover should be observable without OTLP, that is a decision about the decorator as a whole.
8. **`cyoda.dispatch.count` / `.duration` change meaning without changing name**: one sample per callout, however many tries. That is what they measured while a callout was one try. Documented in O-6 and the CHANGELOG; the per-try view is the new `cyoda.callout.tries`.
9. **G row "Client timeout (408) / cancellation"**: a cancellation (the client went away, no `transactionTimeoutMs`) is a ticketed `SERVER_ERROR` envelope today (`classifyWorkflowError`'s context branch → `common.Internal`), and O-7 pins that. Nobody reads that envelope — the client is gone — but if a 499-style operational error is wanted instead, it is a change to the classifier, not to the loop.
10. **`TestAppWiresTheOwnersLoopInBothModes` (O-8) pins source text**, on the precedent of `internal/txgate/suspend_call_sites_test.go`. The behaviour itself — the engine calls the Coordinator — is covered end to end by every E/P/M scenario of stream S and by O-7 at the G layer; the source test exists to give the wiring change its RED. If source-pinning tests are unwelcome in `app`, record a TDD waiver for the wiring lines instead and rely on S.
