# Stream K — the callout's proof of no hand-off

Packages: `internal/contract`, `internal/grpc`, `internal/callout`, plus one
guard test in `internal/domain/entity`. Spec §5.5 ("`NotHandedOff`", "the
callout layer needs these changes", "Absence of the proof"), §16 V6. The spec
calls the proof `NotHandedOff`; the binding name is `contract.NoHandOffProof`
(`interfaces.md`).

**What this stream delivers.** The owner's callout loop returns an error that
carries `*contract.NoHandOffProof` exactly when the work provably never
reached a compute node. Stream E reads it with `contract.ProvesNoHandOff` at the
unsafe dispatch sites. Nothing else changes: the error text, `errors.Is` and
`errors.As` behave as before.

**Facts this stream relies on, read in the code:**

- The hand-off is `member.Send` returning nil (`internal/grpc/dispatch.go:189`).
  Every return of `dispatchCalloutToMember` before `:189` is before the send:
  `:155` (request not built), `:159-163` (no auth context), `:181` (member
  gone before tracking), `:193`, `:197-198`, `:200`, `:203-204` (the failed
  `Send`). Every return from `:214` on is after it: `:226-228`, `:232`, `:241`,
  `:266`, `:269`, `:274-275`, `:277`, `:280-281`.
- `Member.Send` returns `ctx.Err()` at once for a context that has already
  ended (`internal/grpc/members.go:164-166`). A cancelled caller therefore
  reaches `dispatch.go:200` with no hand-off.
- `RunLocal` calls `dispatchCalloutToMember` once per try
  (`internal/grpc/run_local.go:136`) and returns at `:137-143`, `:147-149`,
  or `:166-167`. `RunLocal` returns before any try at `:92-97` and `:115-121`.
- The coordinator has four places that return an error. `run:120-123` (the
  answer limit is above the bound, before any try). `loop:196-203` (`CtxErr`).
  `loop:211-213`, `:217-220`, `:226-228`, `:236-238` (`verdict` and `stop`).
  `askPeers:278-281` (the caller went away during a hand-over), which the loop
  passes through at `:217-219`. All of them pass through `run:154-171`, except
  the one at `:120-123`. `DispatchCriteria` also returns a failure before `run`
  (`internal/callout/entry.go:29-32`).
- `HandOverAnswer.Connected` is false only when nothing reached a peer's
  cnode. `provedBeforeConnecting` covers a request that cannot be built,
  marshalled or signed (`internal/cluster/dispatch/handover.go:523-526`,
  reached from `peer_router.go:141-144`, `:154-160`). `notConnected` covers
  `StageNotConnected` (`handover.go:508-512`, `peer_router.go:161-167`). The
  third case is an authenticated `no_handoff` with `triesUsed == 0`
  (`handover.go:389-391`). Every other answer has `Connected: true`: `readAnswer`
  at `handover.go:349`, and `lostAnswerAfter` at `:489-490`, for every
  `StageAfterConnect` error (`peer_router.go:168-171`).
- In a connected answer, `a.Failure.Kind == contract.NoHandOff` comes only
  from an authenticated `no_handoff` outcome (`handover.go:389-391`). A lost
  answer is `NoAnswer` (`:492`).
- A peer runs `RunLocal` for the hand-over (`internal/cluster/dispatch/handler.go:103`)
  under the owner's `RepeatSafe` (`handover.go:115`, `:245`). When `RepeatSafe`
  is false, `RunLocal` stops at the first try that is not `NoHandOff`
  (`run_local.go:147-149`, `contract/callout.go:50-59`). Its last failure is then
  `NoHandOff` only if every try was `NoHandOff`. When `RepeatSafe` is true, it
  continues past a `NoAnswer` try (`callout.go:55`). Its last failure can then be
  `NoHandOff` after an earlier try reached a cnode. See Open point 1.
- For a callout that is not repeat-safe, every try that ends after the send
  stops the callout: `NoAnswer`, `MemberFailed` and `Terminal` all fail
  `MayTryAnother(false)`. So only a repeat-safe callout can have a hand-off
  in one pass and still make another pass. That is the only case the sticky
  test can observe.
- `common.ClientGone` wraps with `%w` twice (`internal/common/errors.go:47-49`),
  so `errors.Is(err, context.Canceled)` holds behind any wrapper that unwraps.

**Order.** K-1 → K-2 → K-3 → K-4 → K-5 → K-6 → K-7. Nothing outside this
stream is needed. Stream E consumes K-1 and K-5.

**How far this draft was checked.** The code of K-1 … K-6 was applied to a
throw-away copy of the worktree (outside the repository, from `git archive
HEAD` at `ce9ff2bb`). The checks were `go build ./...`, `go vet ./internal/...`
and `gofmt -l`. These package test sets were green: `internal/contract`,
`internal/grpc`, `internal/callout`, `internal/cluster/dispatch`,
`internal/observability`, `internal/domain/workflow` and
`internal/domain/entity`. The new tests ran three to five times under `-race`
without a failure. Every RED stated below was observed in that copy. The two
mutation checks in K-4 and K-5, and the one in K-6, were run there too, and each
failed as stated.

---

### Task K-1: `contract.NoHandOffProof` and `contract.ProvesNoHandOff`

**Spec:** §5.5 "`NotHandedOff` is an error value"; "The engine looks for the proof with `errors.As`".

**Files:**
- Modify: `internal/contract/callout.go` (append after `:102`)
- Create: `internal/contract/no_handoff_proof_test.go` (package `contract_test`)

**Interfaces:**
- Consumes: nothing.
- Produces: `type NoHandOffProof struct{ Err error }`; `func (p *NoHandOffProof) Error() string`; `func (p *NoHandOffProof) Unwrap() error`; `func ProvesNoHandOff(err error) bool`.

- [ ] **Step 1: Write the failing tests**

`internal/contract/no_handoff_proof_test.go`:

```go
package contract_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// The proof is read through every wrap the engine adds, and through a join.
func TestNoHandOffProof_FoundThroughWraps(t *testing.T) {
	proof := &contract.NoHandOffProof{Err: errors.New("no compute member")}
	for name, err := range map[string]error{
		"bare":    proof,
		"wrapped": fmt.Errorf("processor %s failed: %w", "charge", proof),
		"joined":  errors.Join(errors.New("other"), proof),
	} {
		if !contract.ProvesNoHandOff(err) {
			t.Errorf("%s: ProvesNoHandOff = false, want true", name)
		}
	}
}

func TestNoHandOffProof_AbsentIsNotProof(t *testing.T) {
	failure := &contract.CalloutFailure{Kind: contract.NoHandOff, Message: "no compute member"}
	for name, err := range map[string]error{
		"nil":                           nil,
		"plain error":                   errors.New("boom"),
		"a NoHandOff failure, unmarked": failure,
	} {
		if contract.ProvesNoHandOff(err) {
			t.Errorf("%s: ProvesNoHandOff = true, want false", name)
		}
	}
}

// Everything a caller reads behind the proof is what it read before: the text,
// the failure, its AppError, the sentinels, and a cancellation.
func TestNoHandOffProof_ChangesNothingACallerReads(t *testing.T) {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeNoComputeMemberForTag, "no compute member").AsRetryable()
	failure := &contract.CalloutFailure{Kind: contract.NoHandOff, Code: appErr.Code, Message: appErr.Message,
		Err: fmt.Errorf("%w: %w", contract.ErrNoMatchingMember, appErr)}
	proved := fmt.Errorf("processor charge failed: %w", &contract.NoHandOffProof{Err: failure})

	if got, want := proved.Error(), fmt.Errorf("processor charge failed: %w", failure).Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	var gotFailure *contract.CalloutFailure
	if !errors.As(proved, &gotFailure) || gotFailure != failure {
		t.Errorf("errors.As did not find the wrapped *CalloutFailure")
	}
	var gotApp *common.AppError
	if !errors.As(proved, &gotApp) || gotApp != appErr {
		t.Errorf("errors.As did not find the carried *AppError")
	}
	if !errors.Is(proved, contract.ErrNoMatchingMember) {
		t.Errorf("errors.Is lost ErrNoMatchingMember behind the proof")
	}

	gone := &contract.NoHandOffProof{Err: common.ClientGone(context.Canceled)}
	if !errors.Is(gone, context.Canceled) || !errors.Is(gone, common.ErrClientGone) {
		t.Errorf("a cancellation behind the proof is no longer context.Canceled marked as the caller's departure: %v", gone)
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/contract/ -run TestNoHandOffProof`
Expected: FAIL (build), `undefined: contract.NoHandOffProof` and `undefined: contract.ProvesNoHandOff`.

- [ ] **Step 3: Implement**

Append to `internal/contract/callout.go`. `errors` is already imported (`:4`).

```go

// NoHandOffProof marks a callout error that proves the work never reached a
// compute node. No try had Member.Send return nil. No hand-over to a peer got
// past connecting, unless the peer's authenticated answer was no_handoff to a
// callout that is not repeat-safe. Only the owner's callout loop attaches it.
// Its absence proves nothing: an error without it may have followed a
// hand-off.
//
// It changes nothing a reader of the error sees: Error is the wrapped error's
// text, and Unwrap exposes it to errors.Is and errors.As.
type NoHandOffProof struct{ Err error }

func (p *NoHandOffProof) Error() string { return p.Err.Error() }

func (p *NoHandOffProof) Unwrap() error { return p.Err }

// ProvesNoHandOff reports whether err carries a NoHandOffProof anywhere on its
// chain.
func ProvesNoHandOff(err error) bool {
	var proof *NoHandOffProof
	return errors.As(err, &proof)
}
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/contract/`   Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/contract/callout.go internal/contract/no_handoff_proof_test.go
git commit -m "feat(callout): NoHandOffProof marks a callout error that proves no hand-off

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task K-2: one try reports whether it handed the work off

**Spec:** §3 "Hand-off"; §5.5 "Every return path after `Send` returned nil sets it … The branch at `:189-205` is a failed `Send` … and does not set it."

**Files:**
- Modify: `internal/grpc/dispatch.go:126-283` (doc comment, signature, every return)
- Modify: `internal/grpc/run_local.go:136` (call site only; K-3 uses the value)
- Modify: `internal/grpc/dispatch_test.go:141`, `internal/grpc/try_kind_test.go:19` (the two other call sites)
- Create: `internal/grpc/handed_off_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces (unexported): `func (d *ProcessorDispatcher) dispatchCalloutToMember(ctx context.Context, member *Member, call Callout, pass string) (CalloutResult, *contract.CalloutFailure, bool, error)`. The third result is `handedOff`.

Callers of `dispatchCalloutToMember`, from
`grep -rn 'dispatchCalloutToMember(' --include='*.go' internal`:
`internal/grpc/run_local.go:136`, `internal/grpc/dispatch_test.go:141`,
`internal/grpc/try_kind_test.go:19`. There are no others.

- [ ] **Step 1: Write the failing tests**

`internal/grpc/handed_off_test.go`:

```go
package grpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// --- one try: handedOff is exactly "Member.Send returned nil" ---

// calloutDeadlineIn is a context whose own deadline, d from now, is the
// callout's: it ends with contract.ErrCalloutDeadline as its cause.
func calloutDeadlineIn(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadlineCause(testContext(), time.Now().Add(d), contract.ErrCalloutDeadline)
	t.Cleanup(cancel)
	return ctx
}

// cancelledAfter is the caller's context, cancelled d from now.
func cancelledAfter(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(testContext())
	t.Cleanup(cancel)
	time.AfterFunc(d, cancel)
	return ctx
}

// leavesOnReceipt is the caller's context and a cnode script that cancels it
// when the cnode is given the work: Member.Send has returned nil by then.
func leavesOnReceipt(t *testing.T) (context.Context, script) {
	t.Helper()
	ctx, cancel := context.WithCancel(testContext())
	t.Cleanup(cancel)
	return ctx, func(*Member, string) { cancel() }
}

func alreadyCancelled() context.Context {
	ctx, cancel := context.WithCancel(testContext())
	cancel()
	return ctx
}

func TestTry_HandedOff_FalseOnEveryReturnBeforeTheSend(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout)
	}{
		{"the request cannot be built (Terminal)", func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			reg := NewMemberRegistry()
			m, _ := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			call := rawCall(5 * time.Second)
			call.buildRequest = func(string) any { return make(chan int) } // json.Marshal refuses a channel
			return testContext(), newTestDispatcher(t, reg), m, call
		}},
		{"no auth context (Terminal)", func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			reg := NewMemberRegistry()
			m, _ := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			return context.Background(), newTestDispatcher(t, reg), m, rawCall(5 * time.Second)
		}},
		{"the member is gone before the request is tracked", func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			reg := NewMemberRegistry()
			m, _ := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			m.Evict(errors.New("gone"))
			return testContext(), newTestDispatcher(t, reg), m, rawCall(5 * time.Second)
		}},
		{"the member does not drain within the answer limit", func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			d, m := newWedgedDispatcher(t)
			return testContext(), d, m, rawCall(50 * time.Millisecond)
		}},
		{"the callout deadline passes before the send", func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			d, m := newWedgedDispatcher(t)
			return calloutDeadlineIn(t, 30*time.Millisecond), d, m, rawCall(5 * time.Second)
		}},
		{"the caller went away before the send", func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			reg := NewMemberRegistry()
			m, _ := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			return alreadyCancelled(), newTestDispatcher(t, reg), m, rawCall(5 * time.Second)
		}},
		{"the caller goes away while the send waits", func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			d, m := newWedgedDispatcher(t)
			return cancelledAfter(t, 30*time.Millisecond), d, m, rawCall(5 * time.Second)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, d, m, call := tt.setup(t)
			_, failure, handedOff, ctxErr := d.dispatchCalloutToMember(ctx, m, call, "")
			if failure == nil && ctxErr == nil {
				t.Fatal("the try succeeded; want it to end before the send")
			}
			if handedOff {
				t.Errorf("handedOff = true, want false: Member.Send never returned nil (failure %v, ctxErr %v)", failure, ctxErr)
			}
		})
	}
}

func TestTry_HandedOff_TrueOnEveryReturnAfterTheSend(t *testing.T) {
	unreadable := func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
		reg := NewMemberRegistry()
		m, _ := attach(t, reg, "m-1", testTenantID, "x", answers(ProcessingResponse{Success: true}))
		call := rawCall(5 * time.Second)
		call.mapResponse = func(*ProcessingResponse) (CalloutResult, error) { return CalloutResult{}, errors.New("cannot read") }
		return testContext(), newTestDispatcher(t, reg), m, call
	}
	silent := func(ctx func(t *testing.T) context.Context, limit time.Duration) func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
		return func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			reg := NewMemberRegistry()
			m, _ := attach(t, reg, "m-1", testTenantID, "x", nil) // takes the work, never answers
			return ctx(t), newTestDispatcher(t, reg), m, rawCall(limit)
		}
	}
	answering := func(s script) func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
		return func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			reg := NewMemberRegistry()
			m, _ := attach(t, reg, "m-1", testTenantID, "x", s)
			return testContext(), newTestDispatcher(t, reg), m, rawCall(5 * time.Second)
		}
	}
	tests := []struct {
		name  string
		ok    bool
		setup func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout)
	}{
		{"the cnode answers success", true, answering(answers(ProcessingResponse{Success: true}))},
		{"the cnode answers failure (MemberFailed)", false, answering(answers(ProcessingResponse{Success: false, Error: "declined"}))},
		{"the answer is unreadable at the stream (Terminal)", false, answering(answers(ProcessingResponse{Unreadable: "success was null"}))},
		{"the answer does not map (Terminal)", false, unreadable},
		{"the cnode drops after the send (NoAnswer)", false, answering(drops())},
		{"no answer within the answer limit (NoAnswer)", false, silent(func(*testing.T) context.Context { return testContext() }, 50*time.Millisecond)},
		{"the callout deadline passes while waiting (NoAnswer)", false, silent(func(t *testing.T) context.Context { return calloutDeadlineIn(t, 30*time.Millisecond) }, 5*time.Second)},
		{"the caller goes away while waiting", false, func(t *testing.T) (context.Context, *ProcessorDispatcher, *Member, Callout) {
			reg := NewMemberRegistry()
			ctx, leaves := leavesOnReceipt(t)
			m, _ := attach(t, reg, "m-1", testTenantID, "x", leaves)
			return ctx, newTestDispatcher(t, reg), m, rawCall(5 * time.Second)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, d, m, call := tt.setup(t)
			_, failure, handedOff, ctxErr := d.dispatchCalloutToMember(ctx, m, call, "")
			if ok := failure == nil && ctxErr == nil; ok != tt.ok {
				t.Fatalf("succeeded = %v, want %v (failure %v, ctxErr %v)", ok, tt.ok, failure, ctxErr)
			}
			if !handedOff {
				t.Errorf("handedOff = false, want true: Member.Send returned nil (failure %v, ctxErr %v)", failure, ctxErr)
			}
		})
	}
}
```

The helpers come from the package's tests. `attach`, `answers`, `answersAs` and
`drops` are in `run_local_test.go:41-91`. `rawCall` is in `try_kind_test.go:23-31`.
`newTestDispatcher` is at `dispatch_test.go:48-52`, `newWedgedDispatcher` at
`:1343-1353`, `testContext` at `:151-158`, and `testTenantID` at `:22`.

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/grpc/ -run TestTry_HandedOff`
Expected: FAIL (build), `assignment mismatch: 4 variables but d.dispatchCalloutToMember returns 3 values`.

- [ ] **Step 3: Implement**

`internal/grpc/dispatch.go`. First, add this paragraph to the doc comment. It
goes after the bullet that ends "and ctx.Err() is not returned." (`:140`) and
before "One deadline — the answer limit — …" (`:142`):

```go
//
// Beside them, handedOff reports whether Member.Send returned nil: true on
// every return after the hand-off, whatever the try then produced — an answer,
// a failure, or the caller's context ending — and false on every return
// before it. It is the one fact about the try that the owner's proof of no
// hand-off is built from.
```

Then change the signature and every return, exactly as follows:

```diff
-func (d *ProcessorDispatcher) dispatchCalloutToMember(ctx context.Context, member *Member, call Callout, pass string) (CalloutResult, *contract.CalloutFailure, error) {
+func (d *ProcessorDispatcher) dispatchCalloutToMember(ctx context.Context, member *Member, call Callout, pass string) (CalloutResult, *contract.CalloutFailure, bool, error) {
@@ :155
-		return CalloutResult{}, terminalFailure(fmt.Errorf("failed to build %s cloud event: %w", label, err), member.ID, requestID), nil
+		return CalloutResult{}, terminalFailure(fmt.Errorf("failed to build %s cloud event: %w", label, err), member.ID, requestID), false, nil
@@ :163
-		}, nil
+		}, false, nil
@@ :181
-		return CalloutResult{}, appFailure(contract.NoHandOff, disconnectedErr(label)), nil
+		return CalloutResult{}, appFailure(contract.NoHandOff, disconnectedErr(label)), false, nil
@@ :193
-			return CalloutResult{}, appFailure(contract.NoHandOff, disconnectedErr(label)), nil
+			return CalloutResult{}, appFailure(contract.NoHandOff, disconnectedErr(label)), false, nil
@@ :198
-					fmt.Sprintf("%s dispatch cut off at the callout deadline: member not draining", label)).AsRetryable()), nil
+					fmt.Sprintf("%s dispatch cut off at the callout deadline: member not draining", label)).AsRetryable()), false, nil
@@ :200
-			return CalloutResult{}, nil, ctx.Err()
+			return CalloutResult{}, nil, false, ctx.Err()
@@ :204
-				fmt.Sprintf("%s dispatch timed out after %dms: member not draining", label, limitMs)).AsRetryable()), nil
+				fmt.Sprintf("%s dispatch timed out after %dms: member not draining", label, limitMs)).AsRetryable()), false, nil
@@ :228
-				label, name, member.ID, requestID), nil
+				label, name, member.ID, requestID), true, nil
@@ :232
-			return CalloutResult{}, appFailure(contract.NoAnswer, disconnectedErr(label)), nil
+			return CalloutResult{}, appFailure(contract.NoAnswer, disconnectedErr(label)), true, nil
@@ :241
-				return CalloutResult{}, memberResponseUnreadable(err, mapResponseReason(err), label, name, member.ID, requestID), nil
+				return CalloutResult{}, memberResponseUnreadable(err, mapResponseReason(err), label, name, member.ID, requestID), true, nil
@@ :266
-			return CalloutResult{}, failure, nil
+			return CalloutResult{}, failure, true, nil
@@ :269
-		return result, nil, nil
+		return result, nil, true, nil
@@ :275
-					fmt.Sprintf("%s dispatch cut off at the callout deadline: no response", label)).AsRetryable()), nil
+					fmt.Sprintf("%s dispatch cut off at the callout deadline: no response", label)).AsRetryable()), true, nil
@@ :277
-			return CalloutResult{}, nil, ctx.Err()
+			return CalloutResult{}, nil, true, ctx.Err()
@@ :281
-			fmt.Sprintf("%s dispatch timed out after %dms: no response", label, limitMs)).AsRetryable()), nil
+			fmt.Sprintf("%s dispatch timed out after %dms: no response", label, limitMs)).AsRetryable()), true, nil
```

The results stay unnamed. Named results would be shadowed by `var result` at
`:237` and `failure :=` at `:261`.

The three call sites:

```diff
# internal/grpc/run_local.go:136
-		result, failure, ctxErr := d.dispatchCalloutToMember(ctx, member, call, pass)
+		result, failure, _, ctxErr := d.dispatchCalloutToMember(ctx, member, call, pass)
# internal/grpc/dispatch_test.go:141
-	_, failure, ctxErr := d.dispatchCalloutToMember(ctx, member, call, pass)
+	_, failure, _, ctxErr := d.dispatchCalloutToMember(ctx, member, call, pass)
# internal/grpc/try_kind_test.go:19
-	_, failure, ctxErr := d.dispatchCalloutToMember(ctx, member, call, "")
+	_, failure, _, ctxErr := d.dispatchCalloutToMember(ctx, member, call, "")
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/grpc/ -run TestTry_HandedOff`   Expected: `ok`.
Then: `go test ./internal/grpc/`   Expected: `ok` (no other behaviour changed).

- [ ] **Step 5: Commit**

```bash
git add internal/grpc/dispatch.go internal/grpc/run_local.go internal/grpc/dispatch_test.go internal/grpc/try_kind_test.go internal/grpc/handed_off_test.go
git commit -m "feat(callout): a try reports whether Member.Send handed the work off

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task K-3: `LocalResult.HandedOff`

**Spec:** §5.5 "`LocalResult` gains a `HandedOff` bit … `run_local.go:137-140` carries the bit". The call is at `:136`, and the returns that carry the bit are at `:137-143`.

**Files:**
- Modify: `internal/grpc/run_local.go:39-59` (field), `:136` (set it)
- Modify: `internal/grpc/handed_off_test.go` (append)

**Interfaces:**
- Consumes: K-2.
- Produces: `type LocalResult struct { …existing fields…; HandedOff bool }`.

Readers of `LocalResult` outside `internal/grpc`, from
`grep -rn 'LocalResult' --include='*.go' internal | grep -v _test.go`:
`internal/cluster/dispatch/handler.go:21` (the interface), `:103` (the call),
and `internal/cluster/dispatch/handover.go:266` (`responseFromLocal`), plus
`internal/callout/coordinator.go:189`. The peer's answer does not carry the bit.
The owner reads the peer's answer instead (K-5). All `LocalResult` literals in
tests use field names (`internal/cluster/dispatch/*_test.go`), so the new field
compiles unchanged there.

- [ ] **Step 1: Write the failing test**

Add `"encoding/json"` and `spi "github.com/cyoda-platform/cyoda-go-spi"` to the
imports of `internal/grpc/handed_off_test.go`, then append:

```go

// --- the local procedure: HandedOff is sticky across its tries ---

func TestRunLocal_HandedOff(t *testing.T) {
	badEntity := &spi.Entity{Meta: spi.EntityMeta{ID: "entity-bad", TenantID: testTenantID}, Data: []byte(`not json`)}
	noHandOff := contract.NoHandOff
	tests := []struct {
		name     string
		want     bool
		lastKind *contract.CalloutFailureKind
		setup    func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout)
	}{
		{"no cnode", false, nil, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			return testContext(), newTestDispatcher(t, NewMemberRegistry()), processorCall("x", false, 5*time.Second)
		}},
		{"every cnode gone before its send", false, nil, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
			return testContext(), dispatcherGoneOnPick(t, reg, "m-1", "m-2"), processorCall("x", false, 5*time.Second)
		}},
		{"the caller went away before the first try", false, nil, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			return alreadyCancelled(), newTestDispatcher(t, reg), processorCall("x", false, 5*time.Second)
		}},
		{"the request cannot be built (Terminal before the send)", false, nil, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			processor := spi.ProcessorDefinition{Name: "my-proc", Config: spi.ProcessorConfig{AttachEntity: true, CalculationNodesTags: "x"}}
			call := armed(NewProcessorCallout(testTenantID, badEntity, processor, "wf1", "t1", "tx-1"), false, 5*time.Second)
			return testContext(), newTestDispatcher(t, reg), call
		}},
		{"a cnode answers", true, nil, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			return testContext(), newTestDispatcher(t, reg), processorCall("x", false, 5*time.Second)
		}},
		{"the answer is unreadable (Terminal after the send)", true, nil, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", answers(ProcessingResponse{Success: true, Payload: json.RawMessage(`not json`)}))
			return testContext(), newTestDispatcher(t, reg), processorCall("x", false, 5*time.Second)
		}},
		{"the caller goes away after the send", true, nil, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			reg := NewMemberRegistry()
			ctx, leaves := leavesOnReceipt(t)
			attach(t, reg, "m-1", testTenantID, "x", leaves)
			return ctx, newTestDispatcher(t, reg), processorCall("x", false, 5*time.Second)
		}},
		{"a try handed off, then a later try failed before its send", true, &noHandOff, func(t *testing.T) (context.Context, *ProcessorDispatcher, Callout) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", nil) // takes the work, never answers: NoAnswer
			attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
			// repeat-safe, so the NoAnswer on m-1 permits m-2, which is gone on pick
			return testContext(), dispatcherGoneOnPick(t, reg, "m-2"), processorCall("x", true, 50*time.Millisecond)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, d, call := tt.setup(t)
			res := d.RunLocal(ctx, call, 4)
			if tt.lastKind != nil && (res.Failure == nil || res.Failure.Kind != *tt.lastKind) {
				t.Fatalf("last failure = %+v, want kind %v", res.Failure, *tt.lastKind)
			}
			if res.HandedOff != tt.want {
				t.Errorf("HandedOff = %v, want %v (failure %v, ctxErr %v, tries %d)", res.HandedOff, tt.want, res.Failure, res.CtxErr, res.TriesUsed)
			}
		})
	}
}
```

`dispatcherGoneOnPick` is at `run_local_test.go:116-123`, `armed` at `:131-138`,
`processorCall` at `:140-142`. The last case checks that the last failure is
`NoHandOff`. That proves the try that handed off came first, so the case tests
that the bit is sticky.

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/grpc/ -run TestRunLocal_HandedOff`
Expected: FAIL (build), `res.HandedOff undefined (type LocalResult has no field or method HandedOff)`.

- [ ] **Step 3: Implement**

`internal/grpc/run_local.go`. Add the field after `Attempts` (`:57-58`):

```go
	// HandedOff is true when any try of this run had Member.Send return nil:
	// the work may have reached a cnode, whatever the run then produced. It is
	// never cleared by a later try.
	HandedOff bool
```

Replace the call at `:136` (as K-2 left it) so that the bit is set before
any return:

```go
		result, failure, handedOff, ctxErr := d.dispatchCalloutToMember(ctx, member, call, pass)
		if handedOff {
			res.HandedOff = true
		}
		if ctxErr != nil {
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/grpc/ ./internal/cluster/dispatch/`   Expected: `ok` for both.

- [ ] **Step 5: Commit**

```bash
git add internal/grpc/run_local.go internal/grpc/handed_off_test.go
git commit -m "feat(callout): the local procedure reports whether any try handed the work off

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task K-4: the coordinator's sticky flag, and the proof on every error when it is false

**Spec:** §5.5 "The coordinator keeps a sticky flag across all passes … It attaches `NotHandedOff` whenever the flag is false, including on its exits before any try (`ResolveAnswerLimit`, `:120-123`)"; "A cancellation error still satisfies `errors.Is(err, context.Canceled)`".

This task covers local tries and the exits before any try. Hand-overs are
K-5.

**Files:**
- Modify: `internal/callout/coordinator.go` (`progress` `:105-115`, `run` `:120-123` and `:171`, `loop` `:189`)
- Modify: `internal/callout/entry.go:30-32`
- Create: `internal/callout/handoff_proof_test.go`

**Interfaces:**
- Consumes: K-1 (`contract.NoHandOffProof`, `contract.ProvesNoHandOff`), K-3 (`LocalResult.HandedOff`).
- Produces: every error that `Coordinator.DispatchProcessor`, `DispatchCriteria` and `DispatchFunction` return carries `*contract.NoHandOffProof` when no local try handed off. K-5 adds hand-overs to this rule.

- [ ] **Step 1: Write the failing tests**

`internal/callout/handoff_proof_test.go`:

```go
package callout

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// The proof the engine reads: contract.NoHandOffProof on the callout's error
// exactly when no try had Member.Send return nil and no hand-over got past
// connecting without an authenticated no_handoff answer.

// dispatchProcessor makes a processor callout; unsafe unless idempotent.
func (e *env) dispatchProcessor(ctx context.Context, p spi.ProcessorDefinition, entity *spi.Entity) error {
	_, err := e.owner.DispatchProcessor(ctx, entity, p, "wf1", "t1", "tx-1")
	return err
}

func assertProof(t *testing.T, err error, want bool) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want the callout to fail")
	}
	if got := contract.ProvesNoHandOff(err); got != want {
		t.Errorf("ProvesNoHandOff = %v, want %v (err: %v)", got, want, err)
	}
}

func failureOf(t *testing.T, err error) *contract.CalloutFailure {
	t.Helper()
	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) {
		t.Fatalf("no *contract.CalloutFailure behind %v", err)
	}
	return failure
}

// answersUnreadably answers success with a payload the processor cannot decode:
// the work reached the cnode, and the try is Terminal.
func answersUnreadably() script {
	return func(_ *internalgrpc.MemberRegistry, m *internalgrpc.Member, requestID string) {
		m.CompleteRequest(requestID, &internalgrpc.ProcessingResponse{Success: true, Payload: json.RawMessage(`not json`)})
	}
}

// --- exits before any try ---

func TestProof_NoCnode_Proves(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})

	err := e.dispatchProcessor(userCtx(tenantA), processorDef("x", "", false), testEntity())

	assertProof(t, err, true)
	if !errors.Is(err, contract.ErrNoMatchingMember) {
		t.Errorf("err = %v, want ErrNoMatchingMember still on the chain", err)
	}
	if got := failureOf(t, err).Kind; got != contract.NoHandOff {
		t.Errorf("kind = %v, want NoHandOff", got)
	}
}

func TestProof_AnswerLimitAboveTheBound_Proves(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	n := e.attach(t, "m-1", tenantA, "x", answers("m-1"))
	p := processorDef("x", "", false)
	p.Config.ResponseTimeoutMs = 120_000 // the bound newEnv sets is 60 s

	err := e.dispatchProcessor(userCtx(tenantA), p, testEntity())

	assertProof(t, err, true)
	if got := failureOf(t, err).Kind; got != contract.Terminal {
		t.Errorf("kind = %v, want Terminal", got)
	}
	if n.count() != 0 {
		t.Error("the cnode was sent the work")
	}
}

func TestProof_CriterionThatCannotBeParsed_Proves(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})

	_, _, err := e.owner.DispatchCriteria(userCtx(tenantA), testEntity(), json.RawMessage(`not json`), "TRANSITION", "wf1", "t1", "", "tx-1")

	assertProof(t, err, true)
	if got := failureOf(t, err).Kind; got != contract.Terminal {
		t.Errorf("kind = %v, want Terminal", got)
	}
}

// --- local tries ---

func TestProof_CallerGoneBeforeTheFirstSend_Proves(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	n := e.attach(t, "m-1", tenantA, "x", answers("m-1"))
	ctx, cancel := context.WithCancel(userCtx(tenantA))
	cancel()

	err := e.dispatchProcessor(ctx, processorDef("x", "", false), testEntity())

	assertProof(t, err, true)
	assertClientGone(t, err)
	if n.count() != 0 {
		t.Error("the cnode was sent the work")
	}
}

func TestProof_CallerGoneAfterTheSend_DoesNotProve(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	ctx, cancel := context.WithCancel(userCtx(tenantA))
	defer cancel()
	// The cnode cancels the caller when it is given the work: Member.Send has
	// returned nil by then.
	e.attach(t, "m-1", tenantA, "x", func(*internalgrpc.MemberRegistry, *internalgrpc.Member, string) { cancel() })
	p := processorDef("x", "", false)
	p.Config.ResponseTimeoutMs = patientLimitMs

	err := e.dispatchProcessor(ctx, p, testEntity())

	assertProof(t, err, false)
	assertClientGone(t, err)
}

func TestProof_TerminalBeforeTheSend_Proves(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	n := e.attach(t, "m-1", tenantA, "x", answers("m-1"))
	p := processorDef("x", "", false)
	p.Config.AttachEntity = true
	bad := &spi.Entity{Meta: spi.EntityMeta{ID: "entity-bad", TenantID: tenantA}, Data: []byte(`not json`)}

	err := e.dispatchProcessor(userCtx(tenantA), p, bad)

	assertProof(t, err, true)
	if got := failureOf(t, err).Kind; got != contract.Terminal {
		t.Errorf("kind = %v, want Terminal", got)
	}
	if n.count() != 0 {
		t.Error("the cnode was sent the work")
	}
}

func TestProof_EveryFailureAfterTheSend_DoesNotProve(t *testing.T) {
	tests := []struct {
		name string
		s    script
		kind contract.CalloutFailureKind
	}{
		{"unreadable answer", answersUnreadably(), contract.Terminal},
		{"the cnode failed", fails("declined", nil), contract.MemberFailed},
		{"the cnode dropped", detaches(), contract.NoAnswer},
		{"no answer", nil, contract.NoAnswer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, Config{FixedNumRetries: 3})
			e.attach(t, "m-1", tenantA, "x", tt.s)

			err := e.dispatchProcessor(userCtx(tenantA), processorDef("x", "", false), testEntity())

			assertProof(t, err, false)
			if got := failureOf(t, err).Kind; got != tt.kind {
				t.Errorf("kind = %v, want %v", got, tt.kind)
			}
		})
	}
}

// --- sticky ---

// A hand-off in the first pass is not undone by a second pass that reaches no
// cnode. A callout that is not repeat-safe stops at its first try past the
// send, so only a repeat-safe one can make a later pass at all.
func TestProof_AHandOffInAnEarlierPass_IsNeverUndone(t *testing.T) {
	router := newScriptedRouter("p-1")
	router.script("p-1", peerNotConnected()) // first pass; the second finds p-1 not connected either
	e := newClusterEnv(t, Config{FixedNumRetries: 3, Patience: 300 * time.Millisecond, HandoverAllowance: time.Second}, router)
	// Takes the work and goes away: NoAnswer, and the change wakes a second pass.
	e.attach(t, "m-1", tenantA, "x", detaches())

	err := e.dispatchProcessor(userCtx(tenantA), processorDef("x", "", true), testEntity())

	if calls := router.made(); len(calls) != 2 {
		t.Fatalf("hand-overs = %+v, want p-1 asked once in each of two passes", calls)
	}
	if got := appErrOf(t, err).Code; got != common.ErrCodeComputeMemberDisconnected {
		t.Errorf("code = %s, want the one attempt's own COMPUTE_MEMBER_DISCONNECTED", got)
	}
	assertProof(t, err, false)
}
```

The helpers come from the package's tests. `newEnv`, `attach`, `answers`,
`fails`, `detaches`, `processorDef`, `testEntity`, `userCtx`, `appErrOf` and
`patientLimitMs` are in `helpers_test.go`. `newScriptedRouter`,
`peerNotConnected`, `newClusterEnv` and `assertClientGone` are in
`handover_test.go`.

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/callout/ -run TestProof_`
Expected: FAIL. Five tests report `ProvesNoHandOff = false, want true`:
`TestProof_NoCnode_Proves`, `TestProof_AnswerLimitAboveTheBound_Proves`,
`TestProof_CriterionThatCannotBeParsed_Proves`,
`TestProof_CallerGoneBeforeTheFirstSend_Proves` and
`TestProof_TerminalBeforeTheSend_Proves`. Three tests pass already, because
no error carries the proof yet: `…_DoesNotProve` (both) and
`TestProof_AHandOffInAnEarlierPass_IsNeverUndone`. Step 5 shows that the
sticky test can fail.

- [ ] **Step 3: Implement**

`internal/callout/coordinator.go`. Add a field to `progress`, after `stats`
(`:114`):

```go
	// handedOff is set once the work may have reached a cnode — a local try
	// whose Member.Send returned nil, or a hand-over that may have made one —
	// and is never cleared. While it is false the callout's error carries
	// contract.NoHandOffProof.
	handedOff bool
```

In `run`, the exit before any try (`:120-123`):

```go
	limit, failure := c.local.ResolveAnswerLimit(call.ResponseTimeoutMs)
	if failure != nil {
		return internalgrpc.CalloutResult{}, &contract.NoHandOffProof{Err: failure}
	}
```

In `run`, the last line (`:171`), plus a new function after `run`:

```go
	return result, withHandOffProof(err, p.handedOff)
}

// withHandOffProof marks err with contract.NoHandOffProof when no try of the
// callout may have handed the work off. A nil err stays nil.
func withHandOffProof(err error, handedOff bool) error {
	if err == nil || handedOff {
		return err
	}
	return &contract.NoHandOffProof{Err: err}
}
```

The INFO line at `:163-170` reads `err == nil` before the wrap. Its value is
the same either way.

In `loop`, directly after the local procedure (`:189`):

```go
		r := c.local.RunLocal(cctx, call, p.triesLeft)
		if r.HandedOff {
			p.handedOff = true
		}
```

`internal/callout/entry.go:30-32`, the exit in `DispatchCriteria` before `run`:

```go
	if failure != nil {
		// Refused before any try: nothing was handed off.
		return false, "", &contract.NoHandOffProof{Err: failure}
	}
```

`entry.go` already imports `contract` (`:8`).

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/callout/`   Expected: `ok`. Some existing tests
now read through the wrapper and stay green. The client-gone tests check
`context.Canceled` and `common.ErrClientGone` (`coordinator_test.go:519`,
`handover_test.go:265`, `patience_test.go:211`). The fence test uses `appErrOf`
(`patience_test.go:226`). Two tests check
`errors.Is(err, contract.ErrNoMatchingMember)` (`handover_test.go:476`,
`:550`).

- [ ] **Step 5: Show that the sticky test can fail**

Temporarily replace the three lines added in `loop` with
`p.handedOff = r.HandedOff`. Run `go test ./internal/callout/ -run TestProof_AHandOffInAnEarlierPass_IsNeverUndone`.
Expected: FAIL, `ProvesNoHandOff = true, want false (err: COMPUTE_MEMBER_DISCONNECTED: …)`.
Revert, run again, and expect `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/callout/coordinator.go internal/callout/entry.go internal/callout/handoff_proof_test.go
git commit -m "feat(callout): a callout error proves no hand-off when no local try made one

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task K-5: hand-overs count toward the flag, recorded before the early return

**Spec:** §5.5 "no hand-over to a peer got past `StageNotConnected` (`peer_router.go:154-167`), unless the peer's authenticated answer was `no_handoff` (`a.Failure.Kind == NoHandOff`, `handover.go:389-391`)"; "It records each hand-over's outcome before the early return at `coordinator.go:279`".

**Files:**
- Modify: `internal/callout/coordinator.go` (`askPeers` after `:272`; new function before `nextPeer` `:318`)
- Modify: `internal/callout/handoff_proof_test.go` (append)

**Interfaces:**
- Consumes: K-4; `dispatch.HandOverAnswer` (`internal/cluster/dispatch/handover.go:49-77`).
- Produces: the complete §5.5 rule on every error the coordinator returns.

- [ ] **Step 1: Write the failing tests**

Add `"net/http"` and `"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"`
to the imports of `internal/callout/handoff_proof_test.go`, then append:

```go

// peerNoHandOffFailure is the failure and attempt the real router reads from an
// authenticated no_handoff answer of a peer that tried a cnode (readAnswer's
// no_handoff branch with triesUsed > 0).
func peerNoHandOffFailure() (*contract.CalloutFailure, contract.CalloutAttempt) {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeComputeMemberDisconnected,
		"compute member disconnected during processor dispatch").AsRetryable()
	return &contract.CalloutFailure{Kind: contract.NoHandOff, Code: appErr.Code, Message: appErr.Message, Err: appErr},
		contract.CalloutAttempt{MemberID: "m-on-p-1", Kind: contract.NoHandOff, Cause: appErr.Message}
}

// --- hand-overs ---

func TestProof_HandOver(t *testing.T) {
	noHandOff, attempt := peerNoHandOffFailure()
	memberFailed := &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "declined"}
	refusal := &contract.CalloutFailure{Kind: contract.Terminal, Message: "the hand-over could not be accepted"}
	tests := []struct {
		name       string
		answer     func(context.Context) dispatch.HandOverAnswer
		idempotent bool
		want       bool
	}{
		{"not connected", peerNotConnected(), false, true},
		{"proved impossible before connecting", provedUnhandoverable(), false, true},
		{"connected, answer lost (no_answer)", losesTheAnswer(1), false, false},
		{"connected, peer's cnode failed", peerFails(memberFailed, 1, contract.CalloutAttempt{MemberID: "m-on-p-1", Kind: contract.MemberFailed, Cause: "declined"}), false, false},
		{"connected, peer refused before any try (Terminal)", peerFails(refusal, 0), false, false},
		{"connected, no_handoff, not repeat-safe", peerFails(noHandOff, 1, attempt), false, true},
		{"connected, no_handoff, repeat-safe", peerFails(noHandOff, 1, attempt), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := newScriptedRouter("p-1")
			router.script("p-1", tt.answer)
			e := newClusterEnv(t, Config{FixedNumRetries: 0, HandoverAllowance: time.Second}, router)

			err := e.dispatchProcessor(userCtx(tenantA), processorDef("x", "", tt.idempotent), testEntity())

			if calls := router.made(); len(calls) != 1 {
				t.Fatalf("hand-overs = %+v, want one", calls)
			}
			assertProof(t, err, tt.want)
		})
	}
}

// The caller goes away while a hand-over is in progress: the hand-over's
// outcome is recorded before the loop returns.
func TestProof_CallerGoneDuringAHandOver(t *testing.T) {
	tests := []struct {
		name   string
		answer func(context.Context) dispatch.HandOverAnswer
		want   bool
	}{
		{"the peer was connected to", hangs(), false},
		{"the connection was still being opened", dialNeverCompletes(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := newScriptedRouter("p-1")
			router.script("p-1", tt.answer)
			e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: 30 * time.Second}, router)
			ctx, cancel := context.WithCancel(userCtx(tenantA))
			defer cancel()
			time.AfterFunc(30*time.Millisecond, cancel)

			err := e.dispatchProcessor(ctx, processorDef("x", "", false), testEntity())

			assertClientGone(t, err)
			assertProof(t, err, tt.want)
		})
	}
}
```

The scripted answers are in `handover_test.go`. `peerFails` (`:109-113`)
answers `Connected: true`. `losesTheAnswer` (`:132-134`) and `hangs`
(`:138-143`) answer the lost-answer shape, `Connected: true`.
`dialNeverCompletes` (`:162-164`) answers the zero value. `peerNotConnected`
(`:187-193`) and `provedUnhandoverable` (`:169-177`) answer `Connected: false`.
Each one has the same shape as the real router's answer, as documented at
those lines. The "refused before any try" row is `dispatch.refusal`
(`handover.go:298-303`) as `readAnswer` reads it (`:349`, `:406-407`).

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/callout/ -run TestProof_`
Expected: FAIL. Five subtests report `ProvesNoHandOff = true, want false`:
`TestProof_HandOver/connected,_answer_lost_(no_answer)`,
`…/connected,_peer's_cnode_failed`,
`…/connected,_peer_refused_before_any_try_(Terminal)`,
`…/connected,_no_handoff,_repeat-safe` and
`TestProof_CallerGoneDuringAHandOver/the_peer_was_connected_to`.
The remaining rows pass already, because the local pass made no try.

- [ ] **Step 3: Implement**

`internal/callout/coordinator.go`, in `askPeers`, directly after `cancel()`
(`:272`). It must come before the early return at `:278-281`:

```go
		a := c.peers.HandOver(hctx, peer, call, p.triesLeft, major)
		cancel()
		// Recorded before anything below may return: a hand-over the caller
		// walked away from may still have reached a cnode.
		if mayHaveHandedOff(a, call.RepeatSafe) {
			p.handedOff = true
		}
```

Add this function before `nextPeer` (`:318`):

```go
// mayHaveHandedOff reports whether a hand-over may have put the work on a
// cnode. Two answers prove it did not. One that is not Connected: the request
// could not be built or signed, the connection could not be opened, or the
// peer answered no_handoff having tried no cnode. And an authenticated
// no_handoff answer to a callout that is not repeat-safe: such a peer stops at
// its first try that is not NoHandOff (RunLocal, MayTryAnother), so no_handoff
// says that every try it made failed before its send. A repeat-safe peer may
// go on past a try that reached a cnode, so its no_handoff proves nothing.
func mayHaveHandedOff(a dispatch.HandOverAnswer, repeatSafe bool) bool {
	if !a.Connected {
		return false
	}
	noHandOff := a.Failure != nil && a.Failure.Kind == contract.NoHandOff
	return !noHandOff || repeatSafe
}
```

`coordinator.go` already imports `dispatch` and `contract` (`:16`, `:18`).

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/callout/`   Expected: `ok`.

- [ ] **Step 5: Show that the recording order matters**

Temporarily move the `if mayHaveHandedOff(…)` block to after the early
return, before `for _, w := range a.Warnings` (`:282`). Run
`go test ./internal/callout/ -run TestProof_CallerGoneDuringAHandOver`.
Expected: FAIL, `TestProof_CallerGoneDuringAHandOver/the_peer_was_connected_to: ProvesNoHandOff = true, want false`.
Revert, run again, and expect `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/callout/coordinator.go internal/callout/handoff_proof_test.go
git commit -m "feat(callout): a hand-over past connecting counts as a hand-off unless the peer proves otherwise

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task K-6: existing callers are unaffected

**Spec:** §5.5 "Absence of the proof counts as handed off … Every other error is fail-closed."

The wrapper is a new outermost type on the coordinator's errors. This task
checks every reader of those errors. It adds one guard test on the classifier,
which turns the error into what the client sees.

**The callers**, from
`grep -rn '\.DispatchProcessor(\|\.DispatchCriteria(\|\.DispatchFunction(' --include='*.go' . | grep -v _test.go`:

| Call site | What it does with the error |
|---|---|
| `internal/observability/dispatch_tracing.go:161`, `:179`, `:198` | Returns it as is. `record` puts `err.Error()` on the span (`:106-109`). The text is the same, because `Error()` returns the wrapped error's text. |
| `internal/domain/workflow/engine_processors.go:230` | Returns it at `:236`. |
| `internal/domain/workflow/engine_processors.go:269` | Returns it at `:286`, or replaces it with a savepoint error at `:279-285`. |
| `internal/domain/workflow/engine_processors.go:368` | Returns it at `:370`. |
| `internal/domain/workflow/engine_processors.go:400` | Returns it at `:403`. |
| `internal/domain/workflow/engine.go:1023` | Returns it at `:1028`. |
| `internal/domain/workflow/arm.go:243` | Wraps it with `%w` at `:253`. |

Upstream wraps use `%w`. For example, `engine_processors.go:207` has
`"processor %s failed: %w"`. The error then reaches these readers:

- `classifyWorkflowError` (`internal/domain/entity/service.go:2758`). It uses
  `errors.As` for `*common.AppError` (`:2759-2762`) and
  `*contract.CalloutFailure` (`:2832-2833`). It uses `errors.Is` for every
  sentinel. It uses `err.Error()` for the 400 text (`:2834`, `:2840`).
- The gRPC door `buildErrorFields` (`internal/grpc/errors.go:42-44`), with
  `errors.As`, and `isClientGoneCancellation` (`:30-32`), with `errors.Is`.
- `clientGone` (`internal/grpc/txroute_interceptor.go:187-189`) and the HTTP
  funnel's `isClientGoneCancellation` (`internal/common/errors.go:292-294`).
  Both use `errors.Is`.

No reader uses a type assertion or a single-level unwrap on these errors:

```
$ grep -rn '\.(\*contract\.CalloutFailure)\|\.(\*common\.AppError)\|err\.(type)' --include='*.go' internal cmd | grep -v _test.go
internal/api/binding_error.go:17:	switch e := err.(type) {
internal/domain/search/handler.go:327:		common.WriteError(w, r, vErr.(*common.AppError))
internal/domain/model/ingest/storable.go:516:	appErr, ok := err.(*common.AppError)
$ grep -rn 'errors\.Unwrap(' --include='*.go' internal cmd | grep -v _test.go
$
```

All three hits are on other paths. They are oapi binding errors, a pagination
check, and model ingestion, and none of them gets a callout error.
`common.JSONErrorShape` renders `%T`. It is never called with a coordinator
result:

```
$ grep -rn 'JSONErrorShape(' --include='*.go' internal | grep -v _test | grep -v '^internal/common/'
internal/cluster/scheduler_rpc.go:344: …
internal/cluster/dispatch/handler.go:87: …
internal/grpc/dispatch.go:301: …
internal/grpc/dispatch.go:342: …
internal/grpc/streaming.go:296: …
internal/grpc/streaming.go:300: …
internal/grpc/callout.go:175: …
```

**Files:**
- Modify: `internal/domain/entity/service_classify_member_failed_test.go` (import `"context"`; append one test)

**Interfaces:** none.

- [ ] **Step 1: Write the guard test**

Add `"context"` to the imports, then append:

```go

// The owner's callout loop marks some failures with contract.NoHandOffProof.
// The mark is for the scheduler alone: the client sees exactly what it saw
// without it — status, code, message and retryable flag — for every kind of
// callout failure and for the caller's own departure.
func TestClassifyWorkflowError_NoHandOffProofChangesNothing(t *testing.T) {
	yes := true
	noCnode := common.Operational(http.StatusServiceUnavailable, common.ErrCodeNoComputeMemberForTag, "no compute member").AsRetryable()
	cases := map[string]error{
		"no compute member": &contract.CalloutFailure{Kind: contract.NoHandOff, Code: noCnode.Code, Message: noCnode.Message,
			Err: fmt.Errorf("%w: %w", contract.ErrNoMatchingMember, noCnode)},
		"member failed":           &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", Retryable: &yes},
		"terminal without a code": &contract.CalloutFailure{Kind: contract.Terminal, Message: "the workflow's criterion function could not be parsed"},
		"auth context unavailable": &contract.CalloutFailure{Kind: contract.Terminal, Message: "auth context unavailable for dispatch",
			Err: fmt.Errorf("failed to attach auth context: %w", contract.ErrAuthContextUnavailable)},
		"caller gone": common.ClientGone(context.Canceled),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			wrap := func(e error) error { return fmt.Errorf("processor %s failed: %w", "charge", e) }
			plain := classifyWorkflowError(wrap(err))
			proved := classifyWorkflowError(wrap(&contract.NoHandOffProof{Err: err}))
			if plain.Status != proved.Status || plain.Code != proved.Code || plain.Message != proved.Message || plain.Retryable != proved.Retryable {
				t.Errorf("with the proof: %d %s %q retryable=%v; without: %d %s %q retryable=%v",
					proved.Status, proved.Code, proved.Message, proved.Retryable,
					plain.Status, plain.Code, plain.Message, plain.Retryable)
			}
		})
	}
}
```

- [ ] **Step 2: Run it, and show that it can fail**

Run: `go test ./internal/domain/entity/ -run TestClassifyWorkflowError_NoHandOffProofChangesNothing`
Expected: `ok`. This test pins behaviour and drives no production change, so
it passes as soon as it is written. To show its RED, temporarily change
`NoHandOffProof.Error` to `return "no hand-off: " + p.Err.Error()`, then run
again. Expected: FAIL on `member failed` and `terminal without a code`, for
example `with the proof: 400 WORKFLOW_FAILED "WORKFLOW_FAILED: processor charge failed: no hand-off: card declined" …`.
Revert.

- [ ] **Step 3: Run the packages that read the coordinator's errors**

Run: `go test ./internal/observability/ ./internal/domain/workflow/ ./internal/domain/entity/ ./internal/cluster/dispatch/`
Expected: `ok` for each.

- [ ] **Step 4: Commit**

```bash
git add internal/domain/entity/service_classify_member_failed_test.go
git commit -m "test(entity): the no-hand-off proof changes nothing the client sees

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task K-7: stream verification

- [ ] **Step 1:** `gofmt -l internal/contract internal/grpc internal/callout internal/domain/entity`. Expected: no output.
- [ ] **Step 2:** `go vet ./...`. Expected: no output.
- [ ] **Step 3:** `make preflight`, then `make test`. Expected: green. If pre-flight fails, fix Docker and say so. Do not fall back to a narrower run.
- [ ] **Step 4:** `go test -race ./internal/grpc/ ./internal/callout/ -run 'HandedOff|TestProof_'`. Expected: `ok`. This is a spot check of the new tests. The full race run is `make race`, once before the PR.

No commit.

---

## Coverage — this stream's part of the §13 rows

K owns the callout-layer half (U) of these rows. Stream E owns the engine
half, and stream R owns the bookkeeping half.

| §13 row | K's test |
|---|---|
| no compute node → `NotHandedOff` … | `TestProof_NoCnode_Proves`; `TestRunLocal_HandedOff/no_cnode` |
| cancelled before `Send` returned nil → WAITING | `TestProof_CallerGoneBeforeTheFirstSend_Proves`; `TestTry_HandedOff_FalseOnEveryReturnBeforeTheSend/the_caller_went_away_before_the_send`, `…/the_caller_goes_away_while_the_send_waits`; `TestRunLocal_HandedOff/the_caller_went_away_before_the_first_try` |
| cancelled after `Send` returned nil → FAILED | `TestProof_CallerGoneAfterTheSend_DoesNotProve`; `TestTry_HandedOff_TrueOnEveryReturnAfterTheSend/the_caller_goes_away_while_waiting`; `TestRunLocal_HandedOff/the_caller_goes_away_after_the_send` |
| hand-over past `StageNotConnected` without `no_handoff` → FAILED | `TestProof_HandOver` (all seven rows); `TestProof_CallerGoneDuringAHandOver` |
| unsafe processor fails → FAILED | `TestProof_EveryFailureAfterTheSend_DoesNotProve` |
| §5.5 exits before any try | `TestProof_AnswerLimitAboveTheBound_Proves`, `TestProof_CriterionThatCannotBeParsed_Proves`, `TestProof_TerminalBeforeTheSend_Proves` |
| §5.5 sticky flag | `TestProof_AHandOffInAnEarlierPass_IsNeverUndone`; `TestRunLocal_HandedOff/a_try_handed_off,_then_a_later_try_failed_before_its_send` |
| §5.5 `errors.Is(err, context.Canceled)` still holds | `TestNoHandOffProof_ChangesNothingACallerReads`; `assertClientGone` in the two caller-gone `TestProof_` tests |

There is no HTTP, gRPC or parity cell here. The proof is internal and never
reaches a response (K-6). Its observable effect is the scheduler's ending,
which streams E, R and T cover.

## Stream interface summary

**Other streams may consume from K**

`internal/contract`:
- `type NoHandOffProof struct{ Err error }`
- `func (p *NoHandOffProof) Error() string`: `p.Err.Error()`, unchanged.
- `func (p *NoHandOffProof) Unwrap() error`: `p.Err`.
- `func ProvesNoHandOff(err error) bool`: `errors.As(err, **NoHandOffProof)`.
  It is found through any `%w` wrap and through `errors.Join`.

`internal/grpc`:
- `LocalResult.HandedOff bool`: true when any try of the run had
  `Member.Send` return nil. It is never cleared.

`internal/callout` (behaviour, no new exported names):
- Every error from `Coordinator.DispatchProcessor`, `DispatchCriteria` and
  `DispatchFunction` carries `*contract.NoHandOffProof` exactly when both of
  these hold. First, no local try had `Member.Send` return nil. Second, every
  hand-over either got an answer with `Connected == false`, or got an
  authenticated `no_handoff` answer to a callout that is not repeat-safe.
  Errors before any try carry it: `ResolveAnswerLimit`, and a criterion that
  cannot be parsed. A nil error stays nil.
- The error text, `errors.Is` and `errors.As` are unchanged. Stream E reads the
  proof with `contract.ProvesNoHandOff(err)` on the error that the
  `DispatchProcessor` call returns at `engine_processors.go:230, 269, 368, 400`.
  It must read the error before any wrap that could replace it, as
  `:279-285` does. Any other error, including an error from
  `internal/testing/localproc`, counts as handed off.

**K consumes:** nothing from other streams.

## Open points

1. **A `no_handoff` answer to a repeat-safe callout is not trusted.** The spec
   says to trust `a.Failure.Kind == NoHandOff`, and gives the reason "when
   `RepeatSafe` is false, the peer stops on any failure other than
   `NoHandOff`". For a repeat-safe callout, the peer's `RunLocal` continues
   past a `NoAnswer` try (`contract/callout.go:55`). Its last failure can then
   be `NoHandOff` after an earlier try reached a cnode. K-5 therefore trusts
   `no_handoff` only when `RepeatSafe` is false. The engine reads the proof
   only for unsafe processors, so the scheduler behaves the same either way.
   The rule keeps the contract of `NoHandOffProof` true for every callout. The
   lead should fold "to a callout that is not repeat-safe" into the
   `NoHandOffProof` comment in `interfaces.md`, and into the spec's
   `NotHandedOff` bullet.
2. **A connected peer's `terminal` refusal with `triesUsed == 0` counts as
   handed off.** This is `dispatch.refusal` (`handover.go:298-303`), sent
   by `DispatchHandler.refuse` when the peer has authenticated a hand-over it
   cannot run (`handler.go:112-120`). The peer tried no
   cnode, but the spec trusts only `no_handoff`, and K follows the spec
   (`TestProof_HandOver/connected,_peer_refused_before_any_try_(Terminal)`).
   For an unsafe processor, the result is FAILED `UNSAFE_WORK_NOT_COMPLETED`,
   although nothing ran. This fails closed. If the product owner wants an
   authenticated `terminal` with `triesUsed == 0` trusted like `no_handoff`,
   that is a spec change and one more row in `mayHaveHandedOff`.
3. **`DispatchCriteria`'s parse failure is proved** (`entry.go:30-32`). The
   spec names only `ResolveAnswerLimit` as an exit before any try. The parse
   failure is the coordinator's only other such exit, and "attaches whenever
   the flag is false" covers it. A criterion is always repeat-safe, so the
   engine never reads this proof. It is there so that the rule has no
   exception.
4. **Spec line references drift.** The spec cites `run_local.go:137-140` for
   carrying the bit. The call is at `:136`, and the returns that carry the bit
   are at `:137-143`. The other §5.5 references for this stream are exact:
   `dispatch.go:189`, `:189-205`, `:207`, `:270-280`, `coordinator.go:120-123`,
   `:279`, `peer_router.go:154-167`, `handover.go:389-391`.
