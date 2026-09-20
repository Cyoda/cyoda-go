# Stream F — fencing a cnode that was replaced

Plan section for spec §7 (as of commit `d4e53311`), D8, the fencing rows of
§8.2, the `ASYNC_NEW_TX` row of §8.1 and the §13 rows from "Late callback after
the callout ended" to "Pass lifetime follows the answer limit".

**What was checked, and how.** The code of `internal/fence` (F-2, F-3) and all
of its tests were compiled, vetted and run under `-race` in a scratch copy of
the module (`internal/common`, `internal/txgate` + the new package); the F-3
tests were also run against the F-2 code and against F-3 with the wait removed,
and fail as stated. Every other code block in this section was written against
the opened source but **not compiled** — the implementer adjusts imports and
helper names where the compiler says so, and does not change behaviour.

## Order, and the seam with the other streams

```
F-1 → F-2 → F-3 → F-4 → F-5 → F-6 → F-7 → F-8 → F-9 → F-10
                   │      │
                   │      └── must land BEFORE stream L's L-11, and is deleted by stream O
                   └── must land BEFORE stream L's L-10 (L consumes Signer.Issue(claims))
```

**Decision on L's open point 4** (passes name a callout nobody registered).
Until the Coordinator exists, something must begin, advance and end a callout
for every dispatch, or `Admit` (F-6) refuses every callback. F-5 adds a
temporary decorator in `app/`, `onceFenced`, that wraps the
`contract.ExternalProcessingService` the engine is given: per dispatch it
begins a callout, advances it to major 1, mints **the** pass for that callout,
puts it on the context with `internalgrpc.WithTxToken` — the existing rule "a
pass already on the context wins" (`internal/grpc/dispatch.go:60-63`; kept by
L-10, `L-local-procedure.md:3497`) makes both the local dispatcher and a peer
that receives a hand-over use it — and ends the callout when the dispatch
returns. Consequences, all binding on the integrator:

- F-5 and F-6 land **before L-11** (L-11 removes "a pass on the context wins";
  after it the decorator's pass would be ignored and L's per-try pass would name
  an unregistered callout).
- **Stream O deletes the decorator** in the task in which `internal/callout.Coordinator`
  becomes the `ExternalProcessingService`. Exit check for O:
  `grep -rn 'onceFenced' --include='*.go' .` → no hits.
- **L-11 lands after that O task** (it already waits for stream P).
- With the decorator in place the dispatchers' own minting is never reached in
  production when a transaction exists (the decorator always supplies a pass).
  It stays reachable from their unit tests and is removed by L-10/L-11 and by O
  (the whole `ClusterDispatcher`). See Open point 2.

## Coverage matrix rows carried by this stream

U = unit, G = gRPC envelope in `internal/grpc`. The E and M cells of these rows
need a Coordinator or several live cnodes and belong to stream O, **except**
the two E cells marked "F" below, which need neither.

| §13 row | U | G | E |
|---|---|---|---|
| Late callback after the callout ended, tx still open → 410, both doors, write and read | F-2 (`callout ended`), F-6 (`TestJoinFromToken_CalloutEnded_410`, `TestTxJoin_CalloutEnded_410`) | F-6 (`TestTxRouteInterceptor_Superseded*`, four methods) | O |
| Late callback after the transaction ended → 404 | F-6 (`TestJoinFromToken_TxGoneBeatsCalloutEnded_404`) | F-6 (`TestTxRouteInterceptor_NotFoundEnvelope`, kept, now with a fence that knows no callout) | O |
| Late callback in `ASYNC_NEW_TX` after failure → 410 | — | — | O |
| Owner gives the work to a second cnode → first cnode's callback refused at once | F-2 (`lower major…`) , F-6 | F-6 (`TestTxRouteInterceptor_SupersededWhileCalloutInProgress`) | O |
| A joined request queued for the lock when its cnode is replaced → refused on taking the lock, nothing written or read | F-3 (`TestWait_AQueuedWriteIsRefusedOnTakingTheLock`), F-7 (`TestRun_QueuedRequestIsRefusedOnTakingTheLock`) | | O |
| A joined request in progress when its cnode is replaced → the wait | F-3 (`TestWait_Blocks…`, `TestWait_NoWriteOfAnEarlierNumber…`), F-7 (`TestRun_OwnerWaitsForARequestInProgress`) | | O |
| `ASYNC_NEW_TX`: a failed processor's write in progress lands before its savepoint is undone | F-3 (`TestAsyncNewTx_FailedProcessorsWriteLandsBeforeSavepointIsUndone`, memory backend) | | O |
| A callback waiting on a callout of its own when its cnode is replaced → released; refused on re-taking the lock; no audit row; every mode; 410 not 200 | F-8 (`TestSupersededChain_*`, memory backend) | | O |
| The same in `ASYNC_NEW_TX`: savepoint neither undone nor released; replacement's write kept | F-8 (`TestSupersededChain_AsyncNewTx_LeavesItsSavepointAlone`) | | |
| A callback past its last check when its callout ends → lands, 200, owner proceeds afterwards; a joined collection lands whole | F-7 (`TestRun_OwnerWaitsForARequestInProgress` — the lock is per request, so "whole" is structural) | | O |
| A joined read in progress when its cnode is replaced (PostgreSQL) | — | | O |
| Two parallel joined reads, and a read against a write, are serialised by the join layer; clean under `-race` on memory | F-7 (`TestRun_ParallelJoinedRequestsAreSerialised`) | | O |
| A non-entity joined request holds the lock and is refused once its cnode is replaced | F-7 (`TestTxJoin_EveryRouteRunsUnderTheLock`) | | O |
| A joined request's response is sent after the lock is released (HTTP; gRPC server-streaming) | F-7 (`TestTxJoin_ResponseIsSentAfterTheLockIsReleased`, `TestTxRouteInterceptor_StreamSendsAreHeldUntilTheLockIsReleased`) | | |
| The request body / request message is read before the lock is taken: a client that sends headers and stalls does not hold the transaction | F-7 (`TestTxJoin_StalledBodyDoesNotHoldTheLock`, `TestTxJoin_BodyIsReadBeforeTheLockIsTaken`, `TestTxJoin_OversizeBody_413`, `TestTxRouteInterceptor_StreamSendsAreHeldUntilTheLockIsReleased` — its `onRecv` probe) | | |
| A panicking joined handler gives the lock back | F-7 (`TestRun_PanickingHandlerGivesTheLockBack`) | | |
| A joined `GetTransitions` that reaches a function criterion holds the lock and gives it up for the callout | F-7 (`TestJoinedGetTransitions_HoldsTheLock_AndGivesItUpForTheCallout`) | | |
| A cnode disconnects in the middle of its callback → not cancelled | F-6 (`TestJoinFromToken_JoinedContextIsNotCancelledByItsClient`) | | O |
| `ASYNC_NEW_TX`: a savepoint that cannot be created, undone or released → ticketed 5xx, mappings kept, nothing committed, no driver text | F-9 | | |
| Tries made by another pnode: higher `minor` absorbed… second hand-over's `minor = 1` admitted | F-2 (`TestAdmit_AbsorbsAHigherMinor`) | | M: O |
| A pass refused for an enclosing pair absorbs nothing | F-2 (`TestAdmit_RefusedPassAbsorbsNothing`) | | |
| A pass naming an enclosing callout that is no longer current → refused | F-2 (`enclosing callout no longer current`), F-6 (`TestJoinFromToken_EnclosingCalloutNotCurrent_410`) | | O |
| A Coordinator released by the fence reports `CALLOUT_SUPERSEDED`, a client that went away as today | F-3 (`TestBegin_ContextIsReleased…`), F-2 (`TestBegin_CallerCancellationIsNotASupersede`), F-5 (`TestOnceFenced_ReleasedByTheFence_ReportsSuperseded`); the Coordinator's own test is O's | | |
| The wait cannot deadlock | F-3 (`TestWait_NoDeadlockWhenTheInnerCallbackHoldsTheLock`) | | O |
| Callout ended by a panic → its passes are refused | F-2 (`TestEnd_RunsOnPanic`) | | |
| Pass without callout and number → 401; expired pass → 410 | F-4 (`TestVerify_PassWithoutCalloutAndNumberIsInvalid`), F-6 | F-6 (`…ForgedEnvelope`, `…ExpiredEnvelope` kept) | **F** — F-6 (`NoCalloutAndNumber_401` in `internal/e2e/callback_txjoin_errors_test.go`; `Expired_410` kept) |
| Stolen pass presented by another tenant → 403 before any fencing answer | F-6 (`TestJoinFromToken_TenantMismatchBeatsTheFence_403`) | F-6 (`…TenantMismatchEnvelope` kept) | **F** — the existing tenant-mismatch subtest of `TestCallbackErr_LoudFailCodes`, adapted in F-4, asserted unchanged in F-6 |
| Pass lifetime follows the answer limit | F-4 (`TestIssue_ExpiryComesFromTheClaims`): the pass expires exactly when its claims say. The *value* `now + answer limit + CYODA_CALLOUT_PASS_ALLOWANCE` is computed by the minter — stream L, L-10 | | |

---

### Task F-1: the error code `CALLOUT_SUPERSEDED`

**Spec:** §8.2, row "Callback from a cnode that was replaced…".

**Files:**
- Modify: `internal/common/error_codes.go` (the compute-dispatch `const` block, lines 72-77)
- Create: `cmd/cyoda/help/content/errors/CALLOUT_SUPERSEDED.md`
- Modify: `cmd/cyoda/help/content/errors.md` (ERROR CODE INDEX, after the `BAD_REQUEST` row at :61; and the `TRANSACTION_EXPIRED` row at :106, which says `400` while the topic and `txjoin.go:48` say `410` — corrected here, Gate 6)
- Test: `cmd/cyoda/help/help_test.go` (`TestErrCode_Parity`, existing, is the driver)

**Interfaces:**
- Produces: `common.ErrCodeCalloutSuperseded = "CALLOUT_SUPERSEDED"`

- [ ] **Step 1: Write the failing test.** The existing `TestErrCode_Parity` is the contract; add the constant first so that it fails:

```go
// internal/common/error_codes.go — in the block that holds ErrCodeComputeMemberDisconnected
	// ErrCodeCalloutSuperseded is returned to a compute node's callback when
	// the callout it belongs to was given to another compute node or has ended
	// while the transaction is still open. Not retryable: the compute node must
	// stop working on that request.
	ErrCodeCalloutSuperseded = "CALLOUT_SUPERSEDED"
```

- [ ] **Step 2: Run to verify RED**
Run: `go test ./cmd/cyoda/help/... -run 'TestErrCode_Parity'`   Expected: FAIL with `ErrCode "CALLOUT_SUPERSEDED" defined in error_codes.go but no errors/CALLOUT_SUPERSEDED.md`

- [ ] **Step 3: Implement** — `cmd/cyoda/help/content/errors/CALLOUT_SUPERSEDED.md` (format of `errors/TRANSACTION_EXPIRED.md`):

```markdown
---
topic: errors.CALLOUT_SUPERSEDED
title: "CALLOUT_SUPERSEDED — this compute node was replaced, or its callout has ended"
stability: stable
see_also:
  - errors
  - errors.TRANSACTION_NOT_FOUND
  - errors.TRANSACTION_EXPIRED
  - errors.DISPATCH_TIMEOUT
  - errors.COMPUTE_MEMBER_DISCONNECTED
---

# errors.CALLOUT_SUPERSEDED

## NAME

CALLOUT_SUPERSEDED — a request carrying a transaction token was refused because the processor, criterion or function request it belongs to is no longer this compute node's.

## SYNOPSIS

HTTP: `410` `Gone`. Retryable: `no`.

## DESCRIPTION

A compute node receives a transaction token with each processor, criterion or function request, and presents it on the API requests it makes while it works. The token is valid for that one request on that one compute node. This error fires when the token is presented after cyoda gave the same work to another compute node — because this one did not answer within its answer limit, or its connection dropped — or after the request ended: answered, failed or abandoned. The transaction itself is still open; once it has ended the answer is `TRANSACTION_NOT_FOUND` instead.

The request is refused before it reads or writes anything. A request that was already reading or writing when the work moved on is allowed to finish, and is answered normally.

A compute node that receives this error must stop working on that request: nothing further it sends under that token is accepted, and its answer is discarded. Whatever it did outside cyoda before it was replaced is the application's to reconcile; a processor declared `idempotent` promises that a repeat on another compute node is safe.

Not retryable. Over gRPC the code is the message prefix of the `CLIENT_ERROR` envelope.

## SEE ALSO

- errors
- errors.TRANSACTION_NOT_FOUND
- errors.TRANSACTION_EXPIRED
- errors.DISPATCH_TIMEOUT
- errors.COMPUTE_MEMBER_DISCONNECTED
```

`cmd/cyoda/help/content/errors.md` — insert after the `errors.BAD_REQUEST` row (stream O inserts `errors.CALLOUT_FAILED` above this one; the index is alphabetical):

```markdown
- `errors.CALLOUT_SUPERSEDED` — `410` — not retryable — request carrying a transaction token belongs to a compute node that was replaced, or to a callout that has ended
```

and change the `TRANSACTION_EXPIRED` row:

```markdown
- `errors.TRANSACTION_EXPIRED` — `410` — not retryable — transaction token's `exp` claim is in the past
```

- [ ] **Step 4: Run to verify GREEN** — `go test ./cmd/cyoda/help/... ./internal/common/...`

- [ ] **Step 5: Commit**
`git add internal/common/error_codes.go cmd/cyoda/help/content/errors/CALLOUT_SUPERSEDED.md cmd/cyoda/help/content/errors.md && git commit -m "feat(errors): CALLOUT_SUPERSEDED, 410 (#254)"`

---

### Task F-2: `internal/fence` — the number, the check on entry, the check under the lock

**Spec:** §7 "The fencing number", "The arbiter", the currentness and absorption paragraphs, "`end` unregisters the callout", "The fence never cancels a callback's context".

**Files:**
- Create: `internal/fence/fence.go`
- Test: `internal/fence/fence_test.go`

**Interfaces:**
- Consumes: `txgate.Registry` (`internal/txgate/txgate.go:22`), `common.Operational`, `(*common.AppError).WithCause`, `common.ErrCodeCalloutSuperseded` (F-1)
- Produces:
  - `type Pair struct { Callout string "json:c"; Major uint32 "json:j"; Minor uint32 "json:i,omitempty" }` — **the one definition**; F-4 makes `token.Pair` an alias of it. `go list -deps ./internal/txgate` and `./internal/common` show neither reaches `internal/cluster/token`, and `token` imports nothing internal today, so `token → fence` is cycle-free; the reverse would break the spec's "imports only `internal/txgate` and `internal/common`".
  - `func New(gate *txgate.Registry) *Fence`
  - `func (f *Fence) Begin(ctx context.Context, calloutID, txID string, outer []Pair) (context.Context, func())`
  - `func (f *Fence) Advance(calloutID string, major uint32)`
  - `func (f *Fence) Admit(ctx context.Context, pairs []Pair) (context.Context, error)` — `pairs[0]` is the pass's own pair, the rest its `Outer`
  - `func Check(ctx context.Context) error`, `func Pairs(ctx context.Context) []Pair`
  - `var ErrSuperseded error`; `func NewSupersededError() *common.AppError` (410, `CALLOUT_SUPERSEDED`, not retryable, cause `ErrSuperseded` — so `errors.Is(err, fence.ErrSuperseded)` holds through every `%w` wrap, and `errors.As(err, &appErr)` in `classifyWorkflowError` (`entity/service.go:2849`) passes it through unchanged)
- Not in this task: release of inner Coordinators and the wait (F-3). `outer` is accepted and ignored here.

Decisions the spec leaves open, settled here:
- A callout that was begun but never advanced has major 0 and **no pass is current**; a pair with `Major == 0` is never current.
- `Advance` with a major not higher than the current one, or on a callout that has ended, changes nothing ("a number that only goes up").
- A second `Begin` under an id already registered replaces the registration; `end` removes only its own (pointer comparison), so a stale `end` cannot unregister a later callout. Ids are time UUIDs; no branch is spent on the duplicate.
- `Admit` with no pairs is a refusal.
- Refusals are logged at DEBUG without the pass, the callout id or the number (the pass is a credential; the id is in it).

- [ ] **Step 1: Write the failing tests** — `internal/fence/fence_test.go`:

```go
package fence

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

func newFence() *Fence { return New(txgate.New()) }

// begin registers a callout and ends it when the test finishes.
func begin(t *testing.T, f *Fence, ctx context.Context, calloutID, txID string, outer []Pair) (context.Context, func()) {
	t.Helper()
	cctx, end := f.Begin(ctx, calloutID, txID, outer)
	t.Cleanup(end)
	return cctx, end
}

func assertRefused(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}
	if !errors.Is(err, ErrSuperseded) {
		t.Fatalf("refusal must carry ErrSuperseded as its cause, got %v", err)
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("refusal must be an *common.AppError, got %T", err)
	}
	if appErr.Status != http.StatusGone || appErr.Code != common.ErrCodeCalloutSuperseded || appErr.Retryable {
		t.Fatalf("refusal = %d %s retryable=%v; want 410 CALLOUT_SUPERSEDED not retryable", appErr.Status, appErr.Code, appErr.Retryable)
	}
	const want = "CALLOUT_SUPERSEDED: this compute node was replaced, or its callout has ended"
	if appErr.Message != want {
		t.Fatalf("message = %q; want %q", appErr.Message, want)
	}
}

func TestAdmit_Currentness(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(f *Fence, t *testing.T)
		pairs   []Pair
		refused bool
	}{
		{
			name:    "unknown callout",
			prepare: func(*Fence, *testing.T) {},
			pairs:   []Pair{{Callout: "c", Major: 1}},
			refused: true,
		},
		{
			name: "begun but never advanced: no pass is current yet",
			prepare: func(f *Fence, t *testing.T) {
				begin(t, f, context.Background(), "c", "tx", nil)
			},
			pairs:   []Pair{{Callout: "c", Major: 0}},
			refused: true,
		},
		{
			name: "current major",
			prepare: func(f *Fence, t *testing.T) {
				begin(t, f, context.Background(), "c", "tx", nil)
				f.Advance("c", 1)
			},
			pairs: []Pair{{Callout: "c", Major: 1}},
		},
		{
			name: "lower major after the work went to another cnode",
			prepare: func(f *Fence, t *testing.T) {
				begin(t, f, context.Background(), "c", "tx", nil)
				f.Advance("c", 1)
				f.Advance("c", 2)
			},
			pairs:   []Pair{{Callout: "c", Major: 1}},
			refused: true,
		},
		{
			name: "a major that was never issued",
			prepare: func(f *Fence, t *testing.T) {
				begin(t, f, context.Background(), "c", "tx", nil)
				f.Advance("c", 1)
			},
			pairs:   []Pair{{Callout: "c", Major: 2}},
			refused: true,
		},
		{
			name: "the number only goes up: a lower Advance changes nothing",
			prepare: func(f *Fence, t *testing.T) {
				begin(t, f, context.Background(), "c", "tx", nil)
				f.Advance("c", 2)
				f.Advance("c", 1)
			},
			pairs:   []Pair{{Callout: "c", Major: 1}},
			refused: true,
		},
		{
			name: "callout ended",
			prepare: func(f *Fence, t *testing.T) {
				_, end := f.Begin(context.Background(), "c", "tx", nil)
				f.Advance("c", 1)
				end()
			},
			pairs:   []Pair{{Callout: "c", Major: 1}},
			refused: true,
		},
		{
			name:    "no pairs at all",
			prepare: func(*Fence, *testing.T) {},
			pairs:   nil,
			refused: true,
		},
		{
			name: "enclosing callout no longer current",
			prepare: func(f *Fence, t *testing.T) {
				begin(t, f, context.Background(), "outer", "tx", nil)
				f.Advance("outer", 1)
				begin(t, f, context.Background(), "inner", "tx", []Pair{{Callout: "outer", Major: 1}})
				f.Advance("inner", 1)
				f.Advance("outer", 2)
			},
			pairs:   []Pair{{Callout: "inner", Major: 1}, {Callout: "outer", Major: 1}},
			refused: true,
		},
		{
			name: "own and enclosing callout both current",
			prepare: func(f *Fence, t *testing.T) {
				begin(t, f, context.Background(), "outer", "tx", nil)
				f.Advance("outer", 1)
				begin(t, f, context.Background(), "inner", "tx", []Pair{{Callout: "outer", Major: 1}})
				f.Advance("inner", 1)
			},
			pairs: []Pair{{Callout: "inner", Major: 1}, {Callout: "outer", Major: 1}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFence()
			tc.prepare(f, t)
			base := context.Background()
			ctx, err := f.Admit(base, tc.pairs)
			if tc.refused {
				assertRefused(t, err)
				if ctx != base {
					t.Fatal("a refused Admit must hand back the caller's context unchanged")
				}
				return
			}
			if err != nil {
				t.Fatalf("expected admission, got %v", err)
			}
			got := Pairs(ctx)
			if len(got) != len(tc.pairs) {
				t.Fatalf("Pairs = %v; want %v", got, tc.pairs)
			}
			for i := range got {
				if got[i] != tc.pairs[i] {
					t.Fatalf("Pairs = %v; want %v", got, tc.pairs)
				}
			}
		})
	}
}

func TestCheck_NeverAdmittedAlwaysPasses(t *testing.T) {
	if err := Check(context.Background()); err != nil {
		t.Fatalf("the owner's own chain carries no pairs and is never refused, got %v", err)
	}
	if got := Pairs(context.Background()); got != nil {
		t.Fatalf("Pairs of a never-admitted context = %v; want nil", got)
	}
}

func TestCheck_FollowsTheNumber(t *testing.T) {
	f := newFence()
	_, end := f.Begin(context.Background(), "c", "tx", nil)
	f.Advance("c", 1)
	ctx, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 1}})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := Check(ctx); err != nil {
		t.Fatalf("Check under the current number: %v", err)
	}
	// The pairs are a context VALUE: detaching cancellation keeps them.
	if err := Check(context.WithoutCancel(ctx)); err != nil {
		t.Fatalf("Check on a detached context: %v", err)
	}

	f.Advance("c", 2)
	assertRefused(t, Check(ctx))
	assertRefused(t, Check(context.WithoutCancel(ctx)))
	if ctx.Err() != nil {
		t.Fatal("the fence must never cancel a callback's context")
	}

	ctx2, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 2}})
	if err != nil {
		t.Fatalf("Admit under the new number: %v", err)
	}
	end()
	assertRefused(t, Check(ctx2))
	if ctx2.Err() != nil {
		t.Fatal("the fence must never cancel a callback's context")
	}
}

func TestPairs_ReturnsACopy(t *testing.T) {
	f := newFence()
	begin(t, f, context.Background(), "c", "tx", nil)
	f.Advance("c", 1)
	in := []Pair{{Callout: "c", Major: 1}}
	ctx, err := f.Admit(context.Background(), in)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	in[0].Major = 9
	Pairs(ctx)[0].Major = 9
	if got := Pairs(ctx); got[0].Major != 1 {
		t.Fatalf("admitted pairs were mutated through an alias: %v", got)
	}
	if err := Check(ctx); err != nil {
		t.Fatalf("Check after alias mutation: %v", err)
	}
}

// Tries made by another pnode are numbered minor = 1, 2, … under the major of
// the hand-over. The owner learns of the next try from the first callback that
// carries the higher minor.
func TestAdmit_AbsorbsAHigherMinor(t *testing.T) {
	f := newFence()
	begin(t, f, context.Background(), "c", "tx", nil)
	f.Advance("c", 3) // a hand-over under major 3

	first, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 3, Minor: 1}})
	if err != nil {
		t.Fatalf("minor 1: %v", err)
	}
	// Until a callback with a higher minor arrives, the earlier cnode of a
	// hand-over is still admitted.
	if _, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 3, Minor: 1}}); err != nil {
		t.Fatalf("minor 1 again: %v", err)
	}

	if _, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 3, Minor: 2}}); err != nil {
		t.Fatalf("minor 2: %v", err)
	}
	_, err = f.Admit(context.Background(), []Pair{{Callout: "c", Major: 3, Minor: 1}})
	assertRefused(t, err)
	assertRefused(t, Check(first))

	// Advance resets the highest minor seen, so the first callback of a SECOND
	// hand-over (minor 1) is not measured against the last try of the first.
	f.Advance("c", 4)
	if _, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 4, Minor: 1}}); err != nil {
		t.Fatalf("minor 1 of the second hand-over: %v", err)
	}
}

// A higher minor is absorbed only after EVERY pair on the pass was verified.
func TestAdmit_RefusedPassAbsorbsNothing(t *testing.T) {
	f := newFence()
	begin(t, f, context.Background(), "outer", "tx", nil)
	f.Advance("outer", 1)
	begin(t, f, context.Background(), "inner", "tx", []Pair{{Callout: "outer", Major: 1}})
	f.Advance("inner", 1)

	lower, err := f.Admit(context.Background(), []Pair{{Callout: "inner", Major: 1, Minor: 1}, {Callout: "outer", Major: 1}})
	if err != nil {
		t.Fatalf("minor 1: %v", err)
	}

	// Minor 5 of "inner", but under an enclosing pair that is not current.
	_, err = f.Admit(context.Background(), []Pair{{Callout: "inner", Major: 1, Minor: 5}, {Callout: "outer", Major: 7}})
	assertRefused(t, err)

	if err := Check(lower); err != nil {
		t.Fatalf("a refused pass must change nothing, but minor 1 is now refused: %v", err)
	}
}

// context.Cause tells the fence's release from a caller that went away.
func TestBegin_CallerCancellationIsNotASupersede(t *testing.T) {
	f := newFence()
	parent, cancel := context.WithCancel(context.Background())
	cctx, _ := begin(t, f, parent, "c", "tx", nil)
	cancel()
	<-cctx.Done()
	if cause := context.Cause(cctx); errors.Is(cause, ErrSuperseded) || !errors.Is(cause, context.Canceled) {
		t.Fatalf("cause = %v; want context.Canceled and not ErrSuperseded", cause)
	}
}

func TestEnd_Idempotent_AndLeavesALaterCalloutAlone(t *testing.T) {
	f := newFence()
	_, end := f.Begin(context.Background(), "c", "tx", nil)
	f.Advance("c", 1)
	end()
	end()

	begin(t, f, context.Background(), "d", "tx", nil)
	f.Advance("d", 1)
	end() // a stale end must not touch "d"
	if _, err := f.Admit(context.Background(), []Pair{{Callout: "d", Major: 1}}); err != nil {
		t.Fatalf("callout d: %v", err)
	}
}

// The Coordinator defers end, so a callout that ends in a panic still shuts its
// passes out.
func TestEnd_RunsOnPanic(t *testing.T) {
	f := newFence()
	func() {
		defer func() { _ = recover() }()
		_, end := f.Begin(context.Background(), "c", "tx", nil)
		defer end()
		f.Advance("c", 1)
		panic("coordinator blew up")
	}()
	_, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 1}})
	assertRefused(t, err)
}
```

- [ ] **Step 2: Run to verify RED**
Run: `go test ./internal/fence/...`   Expected: FAIL to build — `undefined: New`, `undefined: Pair`, …

- [ ] **Step 3: Implement** — `internal/fence/fence.go`:

```go
// Package fence decides, on the pnode that holds a transaction, whether a
// callback still comes from the compute node that currently holds a callout's
// work. Each callout has a fencing number (major, minor) that only rises; a
// pass carries the number it was minted under; a callback is admitted only
// under the current number, checked again whenever its chain takes the
// transaction's write lock, and whoever takes the work over waits until a write
// in progress has finished. The fence never cancels a callback's context and
// never interrupts a database statement: it works by checks and by the wait.
//
// The package is a leaf: it imports only internal/txgate and internal/common,
// so every place that enforces the fence (the join, the entity service, the
// workflow engine) and every place that drives it (the owner's loop) can
// import it.
package fence

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// ErrSuperseded is the cause carried by every refusal, and the cause with which
// the context returned by Begin is cancelled when an enclosing pair stops being
// current. errors.Is(err, ErrSuperseded) identifies a refusal through any
// number of %w wraps; context.Cause(ctx) tells the fence's cancellation from a
// caller that went away.
var ErrSuperseded = errors.New("callout superseded")

const supersededMessage = "this compute node was replaced, or its callout has ended"

// NewSupersededError returns the client-facing refusal: 410 CALLOUT_SUPERSEDED,
// not retryable, with ErrSuperseded as its cause. A fresh value each time —
// *common.AppError is mutable and must not be shared.
func NewSupersededError() *common.AppError {
	return common.Operational(http.StatusGone, common.ErrCodeCalloutSuperseded, supersededMessage).WithCause(ErrSuperseded)
}

// Pair names one callout at one fencing number. The JSON names are the wire
// form inside a pass and inside a hand-over request.
type Pair struct {
	Callout string `json:"c"`
	Major   uint32 `json:"j"`
	Minor   uint32 `json:"i,omitempty"`
}

// Fence is the arbiter of one pnode. One per process, shared by the owner's
// loop (Begin, Advance) and by both callback doors (Admit).
type Fence struct {
	gate *txgate.Registry

	mu       sync.Mutex
	callouts map[string]*callout
}

// callout is the registration of one callout in progress.
type callout struct {
	txID  string
	major uint32 // 0 until the first Advance: no pass is current yet
	minor uint32 // highest minor seen under major
}

// New returns a Fence whose wait takes the write locks of gate.
func New(gate *txgate.Registry) *Fence {
	return &Fence{gate: gate, callouts: make(map[string]*callout)}
}

// Begin registers a callout on transaction txID and returns the context the
// callout runs under and the func that ends it. The caller defers end, so a
// callout is ended on every exit path, a panic included. end is idempotent.
//
// No pass is current until the first Advance.
func (f *Fence) Begin(ctx context.Context, calloutID, txID string, outer []Pair) (context.Context, func()) {
	cctx, cancel := context.WithCancelCause(ctx)
	c := &callout{txID: txID}

	func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.callouts[calloutID] = c
	}()

	var once sync.Once
	return cctx, func() {
		once.Do(func() {
			f.end(calloutID, c)
			cancel(context.Canceled)
		})
	}
}

// end unregisters the callout: all its passes are refused from then on.
func (f *Fence) end(calloutID string, c *callout) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.callouts[calloutID] == c {
		delete(f.callouts, calloutID)
	}
}

// Advance raises the callout's number to (major, 0), which shuts out every
// pass issued under a lower one. It is called before each local try and before
// each hand-over. A major that is not higher than the current one, or a
// callout that has ended, changes nothing.
func (f *Fence) Advance(calloutID string, major uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.callouts[calloutID]
	if c == nil || major <= c.major {
		return
	}
	c.major, c.minor = major, 0
}

// Admit is the check on entry. pairs names the pass's own callout first and
// then every enclosing one. Under one lock it verifies that every pair is
// current, and only then absorbs a higher minor — so a pass refused for an
// enclosing callout changes nothing. It returns a context that carries the
// pairs. It cancels no callback's context.
func (f *Fence) Admit(ctx context.Context, pairs []Pair) (context.Context, error) {
	pairs = append([]Pair(nil), pairs...)
	ok := func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(pairs) == 0 {
			return false
		}
		for _, p := range pairs {
			if !f.currentLocked(p) {
				return false
			}
		}
		for _, p := range pairs {
			if c := f.callouts[p.Callout]; p.Minor > c.minor {
				c.minor = p.Minor
			}
		}
		return true
	}()
	if !ok {
		slog.Debug("callback refused on entry: its pass is no longer current", "pkg", "fence")
		return ctx, NewSupersededError()
	}
	return context.WithValue(ctx, admittedKey{}, &admitted{f: f, pairs: pairs}), nil
}

type admittedKey struct{}

// admitted is what Admit leaves on the context: the pairs, and the fence that
// judges them. It travels as a context value, so it survives
// context.WithoutCancel.
type admitted struct {
	f     *Fence
	pairs []Pair
}

// Check reports CALLOUT_SUPERSEDED if ctx carries pairs (it was admitted) and
// one of them is no longer current. A context that was never admitted — the
// owner's own chain, an ordinary request — always passes. Check absorbs
// nothing. Callers run it once they hold the transaction's write lock.
func Check(ctx context.Context) error {
	a, _ := ctx.Value(admittedKey{}).(*admitted)
	if a == nil {
		return nil
	}
	current := func() bool {
		a.f.mu.Lock()
		defer a.f.mu.Unlock()
		for _, p := range a.pairs {
			if !a.f.currentLocked(p) {
				return false
			}
		}
		return true
	}()
	if !current {
		slog.Debug("joined chain refused: its pass is no longer current", "pkg", "fence")
		return NewSupersededError()
	}
	return nil
}

// Pairs returns the pairs ctx was admitted under: the outer of a callout begun
// from inside this callback. Nil for a context that was never admitted.
func Pairs(ctx context.Context) []Pair {
	a, _ := ctx.Value(admittedKey{}).(*admitted)
	if a == nil {
		return nil
	}
	return append([]Pair(nil), a.pairs...)
}

// currentLocked reports whether p is current: its callout is registered, its
// major equals the registered one, and its minor is not lower than the highest
// seen under that major. f.mu must be held.
func (f *Fence) currentLocked(p Pair) bool {
	c := f.callouts[p.Callout]
	return c != nil && p.Major != 0 && p.Major == c.major && p.Minor >= c.minor
}
```

- [ ] **Step 4: Run to verify GREEN** — `go test ./internal/fence/...`, and `go list -deps ./internal/fence | grep cyoda-go/internal` → exactly `internal/common`, `internal/txgate`, `internal/fence`.

- [ ] **Step 5: Commit**
`git add internal/fence && git commit -m "feat(fence): fencing numbers, admission and the check under the lock (#254)"`

---

### Task F-3: `internal/fence` — releasing inner Coordinators, and the wait

**Spec:** §7 "Where it is enforced" point 4; "The wait"; the last paragraph (callouts with no transaction); §13 rows on the wait, the queued request, the deadlock, and "`ASYNC_NEW_TX`: a failed processor's write in progress lands before its savepoint is undone".

**Files:**
- Modify: `internal/fence/fence.go` (`callout`, `Begin`, `end`, `Advance`, `Admit`; new `inner`, `wait`, `takeInner`)
- Test: `internal/fence/fence_release_test.go`, `internal/fence/fence_wait_test.go`, `internal/domain/workflow/engine_fence_wait_test.go`

**Interfaces:**
- Consumes: `(*txgate.Registry).Acquire(txID string) func()` — counts a waiter before it blocks (`txgate.go:34-43`), no-op for `""`.
- Produces: the behaviour stream O relies on — `Advance` and `end` return only when no joined request holds the transaction's lock; the context `Begin` returned is cancelled with cause `ErrSuperseded` when an outer pair stops being current (through `Advance`, `end`, or a higher minor absorbed by `Admit`), at once if one already is not.

`go test -race ./internal/fence/...` is run for this package while developing it — the exception `.claude/rules/race-testing.md` allows ("if a race-related bug is suspected… on the specific package").

- [ ] **Step 1: Write the failing tests**

`internal/fence/fence_release_test.go`:

```go
package fence

import (
	"context"
	"errors"
	"testing"
)

func TestBegin_ContextIsReleasedWhenAnEnclosingPairStopsBeingCurrent(t *testing.T) {
	tests := []struct {
		name string
		stop func(f *Fence, endOuter func())
	}{
		{"through Advance", func(f *Fence, _ func()) { f.Advance("outer", 2) }},
		{"through end", func(_ *Fence, endOuter func()) { endOuter() }},
		{"through a higher minor absorbed by Admit", func(f *Fence, _ func()) {
			if _, err := f.Admit(context.Background(), []Pair{{Callout: "outer", Major: 1, Minor: 2}}); err != nil {
				panic(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFence()
			_, endOuter := begin(t, f, context.Background(), "outer", "tx", nil)
			f.Advance("outer", 1)
			if _, err := f.Admit(context.Background(), []Pair{{Callout: "outer", Major: 1, Minor: 1}}); err != nil {
				t.Fatalf("Admit: %v", err)
			}

			cctx, _ := begin(t, f, context.Background(), "inner", "tx", []Pair{{Callout: "outer", Major: 1, Minor: 1}})
			f.Advance("inner", 1)
			if cctx.Err() != nil {
				t.Fatal("inner context cancelled while its enclosing pair is current")
			}

			tc.stop(f, endOuter)

			select {
			case <-cctx.Done():
			default:
				t.Fatal("inner context must be cancelled once the enclosing pair is not current")
			}
			if cause := context.Cause(cctx); !errors.Is(cause, ErrSuperseded) {
				t.Fatalf("cause = %v; want ErrSuperseded", cause)
			}
		})
	}
}

func TestBegin_UnderAnEnclosingPairThatIsAlreadyStale(t *testing.T) {
	f := newFence()
	begin(t, f, context.Background(), "outer", "tx", nil)
	f.Advance("outer", 2)

	cctx, _ := begin(t, f, context.Background(), "inner", "tx", []Pair{{Callout: "outer", Major: 1}})
	if cause := context.Cause(cctx); !errors.Is(cause, ErrSuperseded) {
		t.Fatalf("cause = %v; want ErrSuperseded at once", cause)
	}
}

// A callout with no transaction is still begun and ended, so that it is
// released with an enclosing callback; its wait is a no-op.
func TestBegin_NoTransaction(t *testing.T) {
	f := newFence()
	begin(t, f, context.Background(), "outer", "tx", nil)
	f.Advance("outer", 1)
	cctx, end := f.Begin(context.Background(), "inner", "", []Pair{{Callout: "outer", Major: 1}})
	f.Advance("inner", 1)
	f.Advance("outer", 2)
	if cause := context.Cause(cctx); !errors.Is(cause, ErrSuperseded) {
		t.Fatalf("cause = %v; want ErrSuperseded", cause)
	}
	end()
}
```

`internal/fence/fence_wait_test.go`:

```go
package fence

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// settle is how long a test waits before concluding that a call is blocked.
const settle = 50 * time.Millisecond

// watchdog bounds every wait in these tests: a deadlock fails the test instead
// of hanging the package.
const watchdog = 5 * time.Second

func mustFinish(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(watchdog):
		t.Fatalf("%s did not finish within %v", what, watchdog)
	}
}

func mustStillBlock(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s returned while a joined write still held the transaction's write lock", what)
	case <-time.After(settle):
	}
}

// A joined write that made its check before the number rose still holds the
// write lock; Advance and end wait for it. The earlier pass is shut out at
// once — before the wait — so no further write of the earlier cnode can start.
func TestWait_BlocksUntilAWriteInProgressHasFinished(t *testing.T) {
	tests := []struct {
		name string
		call func(f *Fence, end func())
	}{
		{"Advance", func(f *Fence, _ func()) { f.Advance("c", 2) }},
		{"end", func(_ *Fence, end func()) { end() }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gate := txgate.New()
			f := New(gate)
			_, end := f.Begin(context.Background(), "c", "tx", nil)
			t.Cleanup(end)
			f.Advance("c", 1)
			writer, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 1}})
			if err != nil {
				t.Fatalf("Admit: %v", err)
			}

			// The joined write: lock, check, (write), unlock.
			release := gate.Acquire("tx")
			if err := Check(writer); err != nil {
				t.Fatalf("Check under the lock: %v", err)
			}

			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.call(f, end)
			}()
			mustStillBlock(t, tc.name, done)

			// Shut out already, although the call has not returned.
			if _, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 1}}); !errors.Is(err, ErrSuperseded) {
				t.Fatalf("the earlier pass must be refused before the wait, got %v", err)
			}

			release()
			mustFinish(t, tc.name, done)
		})
	}
}

// A write queued for the lock when its cnode is replaced is refused once it
// takes the lock, whichever of it and the owner's wait the mutex favours.
func TestWait_AQueuedWriteIsRefusedOnTakingTheLock(t *testing.T) {
	gate := txgate.New()
	f := New(gate)
	_, end := f.Begin(context.Background(), "c", "tx", nil)
	t.Cleanup(end)
	f.Advance("c", 1)
	queued, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 1}})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}

	holder := gate.Acquire("tx") // another write of the same cnode, in progress

	var checkErr error
	queuedDone := make(chan struct{})
	go func() {
		defer close(queuedDone)
		release := gate.Acquire("tx")
		defer release()
		checkErr = Check(queued)
	}()
	mustStillBlock(t, "the queued write", queuedDone)

	advanced := make(chan struct{})
	go func() {
		defer close(advanced)
		f.Advance("c", 2)
	}()
	mustStillBlock(t, "Advance", advanced)

	holder()
	mustFinish(t, "the queued write", queuedDone)
	mustFinish(t, "Advance", advanced)
	if !errors.Is(checkErr, ErrSuperseded) {
		t.Fatalf("a write that takes the lock after the number rose must be refused, got %v", checkErr)
	}
}

// The wait cannot deadlock: a callout made from inside a callback, replaced
// while its own inner cnode's callback holds the lock.
//
//	owner:  callout X on tx, given to cnode A (major 1)
//	A:      callback a, joined; gives the lock up for a callout Y of its own
//	Y:      given to cnode B; B's callback b takes the lock and is writing
//	owner:  gives X to another cnode → Advance(X, 2)
func TestWait_NoDeadlockWhenTheInnerCallbackHoldsTheLock(t *testing.T) {
	gate := txgate.New()
	f := New(gate)
	_, endX := f.Begin(context.Background(), "X", "tx", nil)
	t.Cleanup(endX)
	f.Advance("X", 1)

	a, err := f.Admit(context.Background(), []Pair{{Callout: "X", Major: 1}})
	if err != nil {
		t.Fatalf("Admit a: %v", err)
	}

	// Callback a holds no lock for the length of its own callout Y.
	yCtx, endY := f.Begin(a, "Y", "tx", Pairs(a))
	f.Advance("Y", 1)

	b, err := f.Admit(context.Background(), []Pair{{Callout: "Y", Major: 1}, {Callout: "X", Major: 1}})
	if err != nil {
		t.Fatalf("Admit b: %v", err)
	}
	bRelease := gate.Acquire("tx")
	if err := Check(b); err != nil {
		t.Fatalf("Check b: %v", err)
	}

	// Callback a's chain: wait for Y to be released, end Y, re-take the lock,
	// check.
	var aErr error
	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		<-yCtx.Done()
		endY()
		release := gate.Acquire("tx")
		defer release()
		aErr = Check(a)
	}()

	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		f.Advance("X", 2)
	}()
	mustStillBlock(t, "the owner's Advance", ownerDone)

	bRelease() // b's write lands
	mustFinish(t, "the owner's Advance", ownerDone)
	mustFinish(t, "callback a", aDone)
	if !errors.Is(context.Cause(yCtx), ErrSuperseded) {
		t.Fatalf("Y's context cause = %v; want ErrSuperseded", context.Cause(yCtx))
	}
	if !errors.Is(aErr, ErrSuperseded) {
		t.Fatalf("callback a must be refused on re-taking the lock, got %v", aErr)
	}
	if err := func() error {
		release := gate.Acquire("tx")
		defer release()
		return Check(b)
	}(); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("callback b's next locked write must be refused, got %v", err)
	}
}

// There is no third case. Writers loop "lock, check, write, unlock" under the
// pass of whichever major they were last admitted under; the owner keeps giving
// the work to the next cnode. A writer whose check passed under major k holds
// the lock, so Advance(k+1) cannot have returned: the owner's "advanced to"
// marker, which it sets only after Advance returns, must still be ≤ k.
//
// Run under -race while developing this package.
func TestWait_NoWriteOfAnEarlierNumberAfterAdvanceReturned(t *testing.T) {
	const (
		writers = 8
		rounds  = 200
	)
	gate := txgate.New()
	f := New(gate)
	_, end := f.Begin(context.Background(), "c", "tx", nil)
	t.Cleanup(end)

	type admittedAt struct {
		ctx   context.Context
		major uint32
	}
	var current atomic.Pointer[admittedAt]
	var advancedTo atomic.Uint32

	admit := func(major uint32) {
		f.Advance("c", major)
		advancedTo.Store(major)
		ctx, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: major}})
		if err != nil {
			t.Errorf("Admit under major %d: %v", major, err)
			return
		}
		current.Store(&admittedAt{ctx: ctx, major: major})
	}
	admit(1)

	stop := make(chan struct{})
	var violations atomic.Int64
	var passed, refused atomic.Int64
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				at := current.Load()
				func() {
					release := gate.Acquire("tx")
					defer release()
					if err := Check(at.ctx); err != nil {
						refused.Add(1)
						return
					}
					passed.Add(1)
					if advancedTo.Load() > at.major {
						violations.Add(1)
					}
				}()
			}
		}()
	}

	for major := uint32(2); major <= rounds; major++ {
		admit(major)
	}
	close(stop)
	wg.Wait()

	if v := violations.Load(); v != 0 {
		t.Fatalf("%d writes passed their check under a number the owner had already moved past", v)
	}
	if passed.Load() == 0 {
		t.Fatal("no write ever passed its check: the test exercised nothing")
	}
}

// Begin, Admit, Check, Advance and end from many goroutines at once: clean
// under -race, and every callout ends shut.
func TestFence_ConcurrentUse(t *testing.T) {
	f := newFence()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := string(rune('a' + i))
			tx := "tx-" + id
			for round := range 50 {
				outerCtx, endOuter := f.Begin(context.Background(), id, tx, nil)
				f.Advance(id, 1)
				cb, err := f.Admit(outerCtx, []Pair{{Callout: id, Major: 1}})
				if err != nil {
					t.Errorf("Admit: %v", err)
					endOuter()
					return
				}
				innerID := id + "-inner"
				innerCtx, endInner := f.Begin(cb, innerID, tx, Pairs(cb))
				f.Advance(innerID, 1)
				if round%2 == 0 {
					f.Advance(id, 2)
					<-innerCtx.Done()
					if !errors.Is(Check(cb), ErrSuperseded) {
						t.Error("callback still admitted after its cnode was replaced")
					}
				}
				endInner()
				endOuter()
				if _, err := f.Admit(context.Background(), []Pair{{Callout: id, Major: 2}}); !errors.Is(err, ErrSuperseded) {
					t.Errorf("pass admitted after its callout ended: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	remaining := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.callouts)
	}()
	if remaining != 0 {
		t.Fatalf("%d callouts still registered after every end ran: nothing may be kept past a callout", remaining)
	}
}
```

`internal/domain/workflow/engine_fence_wait_test.go` — the engine on the **memory** backend (its in-transaction `Save` never consults the context, `plugins/memory/entity_store.go:229-277`, so only the wait can order this). The scripted processor plays the Coordinator: it begins the callout, advances it, lets a callback start a write, and fails; `end` runs deferred, as the Coordinator's does.

```go
package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// A failed ASYNC_NEW_TX processor's write in progress lands BEFORE its
// savepoint is undone, so it is not in the committed result.
func TestAsyncNewTx_FailedProcessorsWriteLandsBeforeSavepointIsUndone(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	gate := txgate.New()
	f := fence.New(gate)

	base := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "fence-wait", ModelVersion: "1.0"}
	const lateID = "late-write"

	writeStarted := make(chan struct{})
	letWriteLand := make(chan struct{})
	writeLanded := make(chan struct{})

	ext := &mockExternalProcessing{
		dispatchFunc: func(ctx context.Context, _ *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
			_, end := f.Begin(ctx, "callout-1", txID, fence.Pairs(ctx))
			defer end() // the wait happens here, before the engine undoes the savepoint
			f.Advance("callout-1", 1)

			// The cnode's callback: joined, admitted, under the lock, checked —
			// and then slow.
			joined, err := txMgr.Join(base, txID)
			if err != nil {
				t.Errorf("Join: %v", err)
				return nil, err
			}
			admitted, err := f.Admit(joined, []fence.Pair{{Callout: "callout-1", Major: 1}})
			if err != nil {
				t.Errorf("Admit: %v", err)
				return nil, err
			}
			go func() {
				defer close(writeLanded)
				release := gate.Acquire(txID)
				defer release()
				if err := fence.Check(admitted); err != nil {
					t.Errorf("Check: %v", err)
					return
				}
				close(writeStarted)
				<-letWriteLand
				es, err := factory.EntityStore(admitted)
				if err != nil {
					t.Errorf("EntityStore: %v", err)
					return
				}
				late := makeEntity(lateID, modelRef, map[string]any{"late": true})
				late.Meta.TransactionID = txID
				if _, err := es.Save(admitted, late); err != nil {
					t.Errorf("late Save: %v", err)
				}
			}()
			<-writeStarted
			go func() {
				time.Sleep(50 * time.Millisecond) // end() is blocked in the wait meanwhile
				close(letWriteLand)
			}()
			return nil, errors.New("the cnode answered: failed")
		},
	}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(ext))

	saveWorkflow(t, factory, base, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "FenceWaitWF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{{
				Name: "RUN", Next: "DONE",
				Processors: []spi.ProcessorDefinition{
					{Type: ProcessorTypeExternalized, Name: "fails", ExecutionMode: ExecutionModeAsyncNewTx},
				},
			}}},
			"DONE": {},
		},
	}})

	txID, txCtx, err := txMgr.Begin(base)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	entity := makeEntity("fence-wait-1", modelRef, map[string]any{"x": 1})
	entity.Meta.TransactionID = txID
	if _, err := engine.Execute(txCtx, entity, ""); err != nil {
		t.Fatalf("an ASYNC_NEW_TX processor failure must not fail the operation: %v", err)
	}
	select {
	case <-writeLanded:
	default:
		t.Fatal("the engine carried on while the failed processor's write was still in progress")
	}
	if err := txMgr.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	es, err := factory.EntityStore(base)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Get(base, lateID); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("the failed processor's write is in the committed result (Get err = %v): it landed after its savepoint was undone", err)
	}
}
```

- [ ] **Step 2: Run to verify RED**
Run: `go test ./internal/fence/... ./internal/domain/workflow/... -run 'TestBegin_|TestWait_|TestFence_ConcurrentUse|TestAsyncNewTx_FailedProcessorsWrite'`
Expected: FAIL — `inner context must be cancelled once the enclosing pair is not current`; `Advance returned while a joined write still held the transaction's write lock`; `a write that takes the lock after the number rose must be refused` is already green (F-2) but its `Advance` half fails; `… writes passed their check under a number the owner had already moved past`; `the engine carried on while the failed processor's write was still in progress`.

- [ ] **Step 3: Implement** — `internal/fence/fence.go` becomes (the whole file; changed against F-2: the `inner` field and type, `Begin`, `end`, `Advance`, the absorb loop of `Admit`, `wait`, `takeInner`):

```go
// Package fence decides, on the pnode that holds a transaction, whether a
// callback still comes from the compute node that currently holds a callout's
// work. Each callout has a fencing number (major, minor) that only rises; a
// pass carries the number it was minted under; a callback is admitted only
// under the current number, checked again whenever its chain takes the
// transaction's write lock, and whoever takes the work over waits until a write
// in progress has finished. The fence never cancels a callback's context and
// never interrupts a database statement: it works by checks and by the wait.
//
// The package is a leaf: it imports only internal/txgate and internal/common,
// so every place that enforces the fence (the join, the entity service, the
// workflow engine) and every place that drives it (the owner's loop) can
// import it.
package fence

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// ErrSuperseded is the cause carried by every refusal, and the cause with which
// the context returned by Begin is cancelled when an enclosing pair stops being
// current. errors.Is(err, ErrSuperseded) identifies a refusal through any
// number of %w wraps; context.Cause(ctx) tells the fence's cancellation from a
// caller that went away.
var ErrSuperseded = errors.New("callout superseded")

const supersededMessage = "this compute node was replaced, or its callout has ended"

// NewSupersededError returns the client-facing refusal: 410 CALLOUT_SUPERSEDED,
// not retryable, with ErrSuperseded as its cause. A fresh value each time —
// *common.AppError is mutable and must not be shared.
func NewSupersededError() *common.AppError {
	return common.Operational(http.StatusGone, common.ErrCodeCalloutSuperseded, supersededMessage).WithCause(ErrSuperseded)
}

// Pair names one callout at one fencing number. The JSON names are the wire
// form inside a pass and inside a hand-over request.
type Pair struct {
	Callout string `json:"c"`
	Major   uint32 `json:"j"`
	Minor   uint32 `json:"i,omitempty"`
}

// Fence is the arbiter of one pnode. One per process, shared by the owner's
// loop (Begin, Advance) and by both callback doors (Admit).
type Fence struct {
	gate *txgate.Registry

	mu       sync.Mutex
	callouts map[string]*callout
}

// callout is the registration of one callout in progress.
type callout struct {
	txID  string
	major uint32 // 0 until the first Advance: no pass is current yet
	minor uint32 // highest minor seen under major
	// inner holds the callouts begun from inside a callback of this one, each
	// with the pair of this callout it was begun under.
	inner map[*inner]Pair
}

// inner is the handle by which an enclosing callout releases a Coordinator
// that was begun under one of its pairs.
type inner struct {
	cancel context.CancelCauseFunc
}

// New returns a Fence whose wait takes the write locks of gate.
func New(gate *txgate.Registry) *Fence {
	return &Fence{gate: gate, callouts: make(map[string]*callout)}
}

// Begin registers a callout on transaction txID, under the enclosing callouts
// named by outer, and returns the context the callout runs under and the func
// that ends it. The context is cancelled, with ErrSuperseded as its cause, when
// one of the outer pairs stops being current — at once if one already is not.
// The caller defers end, so a callout is ended on every exit path, a panic
// included. end is idempotent.
//
// No pass is current until the first Advance. An empty txID is a callout with
// no transaction: it is registered so that it is released with an enclosing
// callback, and its wait is a no-op.
func (f *Fence) Begin(ctx context.Context, calloutID, txID string, outer []Pair) (context.Context, func()) {
	cctx, cancel := context.WithCancelCause(ctx)
	in := &inner{cancel: cancel}
	c := &callout{txID: txID, inner: make(map[*inner]Pair)}
	outer = append([]Pair(nil), outer...)

	stale := func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.callouts[calloutID] = c
		for _, p := range outer {
			if !f.currentLocked(p) {
				return true
			}
		}
		for _, p := range outer {
			f.callouts[p.Callout].inner[in] = p
		}
		return false
	}()
	if stale {
		cancel(ErrSuperseded)
	}

	var once sync.Once
	return cctx, func() {
		once.Do(func() { f.end(calloutID, c, in, outer) })
	}
}

// end unregisters the callout — all its passes are refused from then on —
// releases the Coordinators begun under it, and waits for a joined write in
// progress on its transaction.
func (f *Fence) end(calloutID string, c *callout, in *inner, outer []Pair) {
	released := func() []*inner {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.callouts[calloutID] == c {
			delete(f.callouts, calloutID)
		}
		for _, p := range outer {
			if oc := f.callouts[p.Callout]; oc != nil {
				delete(oc.inner, in)
			}
		}
		return takeInner(c, func(Pair) bool { return true })
	}()
	for _, r := range released {
		r.cancel(ErrSuperseded)
	}
	in.cancel(context.Canceled)
	f.wait(c.txID)
}

// Advance raises the callout's number to (major, 0), which shuts out every
// pass issued under a lower one, releases the Coordinators begun under those
// passes, and then waits until no joined write is in progress on the
// transaction. It is called before each local try and before each hand-over.
// A major that is not higher than the current one, or a callout that has
// ended, changes nothing.
func (f *Fence) Advance(calloutID string, major uint32) {
	txID, released, raised := func() (string, []*inner, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		c := f.callouts[calloutID]
		if c == nil || major <= c.major {
			return "", nil, false
		}
		c.major, c.minor = major, 0
		return c.txID, takeInner(c, func(Pair) bool { return true }), true
	}()
	if !raised {
		return
	}
	for _, r := range released {
		r.cancel(ErrSuperseded)
	}
	f.wait(txID)
}

// wait is the hand-over of the write lock: a joined write that made its check
// before the number rose still holds the lock, and this blocks until it has
// finished; one that takes the lock afterwards is refused by its check. It runs
// outside f.mu — the fence never waits under its own mutex.
func (f *Fence) wait(txID string) {
	release := f.gate.Acquire(txID)
	release()
}

// Admit is the check on entry. pairs names the pass's own callout first and
// then every enclosing one. Under one lock it verifies that every pair is
// current, and only then absorbs a higher minor — so a pass refused for an
// enclosing callout changes nothing. It returns a context that carries the
// pairs. It cancels no callback's context; absorbing a higher minor does
// release the Coordinators of callouts begun under the lower one.
func (f *Fence) Admit(ctx context.Context, pairs []Pair) (context.Context, error) {
	pairs = append([]Pair(nil), pairs...)
	released, ok := func() ([]*inner, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(pairs) == 0 {
			return nil, false
		}
		for _, p := range pairs {
			if !f.currentLocked(p) {
				return nil, false
			}
		}
		var released []*inner
		for _, p := range pairs {
			c := f.callouts[p.Callout]
			if p.Minor > c.minor {
				c.minor = p.Minor
				released = append(released, takeInner(c, func(under Pair) bool { return under.Minor < p.Minor })...)
			}
		}
		return released, true
	}()
	if !ok {
		slog.Debug("callback refused on entry: its pass is no longer current", "pkg", "fence")
		return ctx, NewSupersededError()
	}
	for _, r := range released {
		r.cancel(ErrSuperseded)
	}
	return context.WithValue(ctx, admittedKey{}, &admitted{f: f, pairs: pairs}), nil
}

type admittedKey struct{}

// admitted is what Admit leaves on the context: the pairs, and the fence that
// judges them. It travels as a context value, so it survives
// context.WithoutCancel.
type admitted struct {
	f     *Fence
	pairs []Pair
}

// Check reports CALLOUT_SUPERSEDED if ctx carries pairs (it was admitted) and
// one of them is no longer current. A context that was never admitted — the
// owner's own chain, an ordinary request — always passes. Check absorbs
// nothing. Callers run it once they hold the transaction's write lock.
func Check(ctx context.Context) error {
	a, _ := ctx.Value(admittedKey{}).(*admitted)
	if a == nil {
		return nil
	}
	current := func() bool {
		a.f.mu.Lock()
		defer a.f.mu.Unlock()
		for _, p := range a.pairs {
			if !a.f.currentLocked(p) {
				return false
			}
		}
		return true
	}()
	if !current {
		slog.Debug("joined chain refused: its pass is no longer current", "pkg", "fence")
		return NewSupersededError()
	}
	return nil
}

// Pairs returns the pairs ctx was admitted under: the outer of a callout begun
// from inside this callback. Nil for a context that was never admitted.
func Pairs(ctx context.Context) []Pair {
	a, _ := ctx.Value(admittedKey{}).(*admitted)
	if a == nil {
		return nil
	}
	return append([]Pair(nil), a.pairs...)
}

// currentLocked reports whether p is current: its callout is registered, its
// major equals the registered one, and its minor is not lower than the highest
// seen under that major. f.mu must be held.
func (f *Fence) currentLocked(p Pair) bool {
	c := f.callouts[p.Callout]
	return c != nil && p.Major != 0 && p.Major == c.major && p.Minor >= c.minor
}

// takeInner removes from c, and returns, every inner handle whose pair
// satisfies stale. The fence's mutex must be held.
func takeInner(c *callout, stale func(under Pair) bool) []*inner {
	var taken []*inner
	for in, under := range c.inner {
		if stale(under) {
			taken = append(taken, in)
			delete(c.inner, in)
		}
	}
	return taken
}
```

Mutex discipline: every `Lock()` is followed by `defer Unlock()` inside an IIFE; contexts are cancelled and the wait is made **outside** `f.mu`.

- [ ] **Step 4: Run to verify GREEN** — `go test ./internal/fence/... ./internal/domain/workflow/...`, then `go test -race ./internal/fence/...`.

- [ ] **Step 5: Commit**
`git add internal/fence internal/domain/workflow/engine_fence_wait_test.go && git commit -m "feat(fence): release inner callouts and wait for a request in progress (#254)"`

---

### Task F-4: the pass carries the callout and its number

**Spec:** §7 "Claims"; §8.2 row "Callback bearing a pass with no callout and number → 401". **Must land before stream L's L-10.**

**Files:**
- Modify: `internal/cluster/token/token.go` (`Claims`, `Issue`, `Verify`; new `Pair`)
- Modify: `internal/grpc/dispatch.go` (`resolveTxToken`, :60-73; its call at :119)
- Modify: `internal/cluster/dispatch/cluster_dispatcher.go` (`mintTxToken`, :209-219)
- Test (new cases): `internal/cluster/token/token_test.go`
- Test (mechanical, same commit — the ten files that call `Issue`): `internal/cluster/token/token_test.go` (4 calls), `internal/cluster/integration_test.go` (1), `internal/cluster/proxy/http_test.go` (10), `internal/cluster/proxy/grpc_test.go` (3), `internal/cluster/dispatch/cluster_txtoken_test.go` (1), `internal/grpc/txroute_interceptor_test.go` (17), `internal/grpc/txroute_search_ryw_test.go` (1), `internal/httpmw/txjoin_mw_test.go` (3), `internal/domain/txjoin/txjoin_test.go` (8), `internal/e2e/callback_txjoin_errors_test.go` (3). Plus `internal/grpc/dispatch_txtoken_test.go`, which calls `resolveTxToken`.

**Interfaces:**
- Consumes: `fence.Pair` (F-2)
- Produces (binding for L, O, P):
  - `type Pair = fence.Pair`
  - `type Claims struct { NodeID string "json:n"; TxRef string "json:t"; ExpiresAt int64 "json:e"; Callout string "json:c"; Major uint32 "json:j"; Minor uint32 "json:i,omitempty"; Outer []Pair "json:o,omitempty" }` — `ExpiresAt` stays Unix seconds
  - `func (s *Signer) Issue(claims Claims) (string, error)` — signs what it is given and validates nothing, as today; validity is `Verify`'s (this is also what lets the e2e layer mint the pass of §8.2's 401 row from the server's own ephemeral signer)
  - `func (s *Signer) Verify(tok string) (*Claims, error)` — additionally `ErrTokenInvalid` when `Callout == ""`, `Major == 0`, or any `Outer` pair has an empty `Callout` or `Major == 0`. The check runs after the HMAC and **before** the expiry, so a pass of an earlier version is 401 whatever its age. `ErrTokenInvalid` is already mapped to 401 `UNAUTHORIZED` "invalid transaction token" at all four verify sites (`txjoin.go:49`, `proxy/http.go:93`, `grpc/txroute_interceptor.go:107`, and `proxy/grpc.go` through the same classifier), so no mapping changes.

- [ ] **Step 1: Write the failing tests** — append to `internal/cluster/token/token_test.go` (package `token_test`; add imports `crypto/hmac`, `crypto/sha256`, `encoding/base64`, `errors`):

```go
func validClaims(exp time.Time) token.Claims {
	return token.Claims{NodeID: "node-1", TxRef: "tx-uuid-abc", ExpiresAt: exp.Unix(), Callout: "req-1", Major: 1}
}

func TestRoundTrip_CarriesTheCalloutAndItsNumber(t *testing.T) {
	signer, _ := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	in := token.Claims{
		NodeID: "owner", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(),
		Callout: "req-inner", Major: 3, Minor: 2,
		Outer: []token.Pair{{Callout: "req-outer", Major: 1}},
	}
	tok, err := signer.Issue(in)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := signer.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.NodeID != in.NodeID || got.TxRef != in.TxRef || got.ExpiresAt != in.ExpiresAt ||
		got.Callout != in.Callout || got.Major != in.Major || got.Minor != in.Minor ||
		len(got.Outer) != 1 || got.Outer[0] != in.Outer[0] {
		t.Fatalf("claims = %+v; want %+v", got, in)
	}
}

// The number is covered by the HMAC like every other claim.
func TestVerify_TamperedNumberIsRefused(t *testing.T) {
	secret := []byte("test-secret-key-at-least-32-bytes!")
	signer, _ := token.NewSigner(secret)
	tok, _ := signer.Issue(validClaims(time.Now().Add(time.Minute)))
	dot := strings.LastIndexByte(tok, '.')
	payload, _ := base64.RawURLEncoding.DecodeString(tok[:dot])
	forged := strings.Replace(string(payload), `"j":1`, `"j":2`, 1)
	if forged == string(payload) {
		t.Fatal("test setup: the major was not found in the payload")
	}
	if _, err := signer.Verify(base64.RawURLEncoding.EncodeToString([]byte(forged)) + tok[dot:]); !errors.Is(err, token.ErrTokenTampered) {
		t.Fatalf("err = %v; want ErrTokenTampered", err)
	}
}

func TestVerify_PassWithoutCalloutAndNumberIsInvalid(t *testing.T) {
	signer, _ := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	exp := time.Now().Add(time.Minute).Unix()
	tests := []struct {
		name   string
		claims token.Claims
	}{
		{"a pass of an earlier version: no callout, no number", token.Claims{NodeID: "n", TxRef: "t", ExpiresAt: exp}},
		{"callout without a number", token.Claims{NodeID: "n", TxRef: "t", ExpiresAt: exp, Callout: "c"}},
		{"number without a callout", token.Claims{NodeID: "n", TxRef: "t", ExpiresAt: exp, Major: 1}},
		{"enclosing pair without a callout", token.Claims{NodeID: "n", TxRef: "t", ExpiresAt: exp, Callout: "c", Major: 1, Outer: []token.Pair{{Major: 1}}}},
		{"enclosing pair without a number", token.Claims{NodeID: "n", TxRef: "t", ExpiresAt: exp, Callout: "c", Major: 1, Outer: []token.Pair{{Callout: "o"}}}},
		{"no callout AND expired: invalid wins", token.Claims{NodeID: "n", TxRef: "t", ExpiresAt: time.Now().Add(-time.Minute).Unix()}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := signer.Issue(tc.claims)
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
			if _, err := signer.Verify(tok); !errors.Is(err, token.ErrTokenInvalid) {
				t.Fatalf("err = %v; want ErrTokenInvalid", err)
			}
		})
	}
}

// The pass expires exactly when its claims say; what that moment is — the
// answer limit plus the pass allowance — is the minter's to compute.
func TestIssue_ExpiryComesFromTheClaims(t *testing.T) {
	signer, _ := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	live, _ := signer.Issue(validClaims(time.Now().Add(30 * time.Second)))
	if _, err := signer.Verify(live); err != nil {
		t.Fatalf("a pass inside its life: %v", err)
	}
	dead, _ := signer.Issue(validClaims(time.Now().Add(-2 * time.Second)))
	if _, err := signer.Verify(dead); !errors.Is(err, token.ErrTokenExpired) {
		t.Fatalf("err = %v; want ErrTokenExpired", err)
	}
}
```

(`crypto/hmac` and `crypto/sha256` are not needed after all — the tamper test reuses the original signature; import only `encoding/base64`, `errors`, `strings`.)

- [ ] **Step 2: Run to verify RED**
Run: `go test ./internal/cluster/token/...`   Expected: FAIL to build — `unknown field Callout in struct literal of type token.Claims`, `too many arguments in call to signer.Issue`.

- [ ] **Step 3: Implement**

`internal/cluster/token/token.go` — before / after of the changed region:

```go
// before
type Claims struct {
	NodeID    string `json:"n"`
	TxRef     string `json:"t"`
	ExpiresAt int64  `json:"e"`
}

func (s *Signer) Issue(nodeID, txRef string, expiresAt time.Time) (string, error) {
	claims := Claims{
		NodeID:    nodeID,
		TxRef:     txRef,
		ExpiresAt: expiresAt.Unix(),
	}
	payload, err := json.Marshal(claims)
```

```go
// after
// Pair names one callout at one fencing number. It is the fence's type: the
// pass carries what the fence judges.
type Pair = fence.Pair

// Claims is what a pass says. NodeID is the owner — the pnode that holds the
// transaction and whose fence judges the pass. Callout, Major and Minor name
// the try the pass was minted for; Outer names every enclosing callout, for a
// callout made from inside a callback.
type Claims struct {
	NodeID    string `json:"n"`
	TxRef     string `json:"t"`
	ExpiresAt int64  `json:"e"` // Unix seconds
	Callout   string `json:"c"`
	Major     uint32 `json:"j"`
	Minor     uint32 `json:"i,omitempty"`
	Outer     []Pair `json:"o,omitempty"`
}

// Issue signs claims as given. Whether they make a valid pass is Verify's to
// say.
func (s *Signer) Issue(claims Claims) (string, error) {
	payload, err := json.Marshal(claims)
```

and in `Verify`, between the `json.Unmarshal` and the expiry check:

```go
	if !named(Pair{Callout: claims.Callout, Major: claims.Major}) {
		return nil, ErrTokenInvalid
	}
	for _, p := range claims.Outer {
		if !named(p) {
			return nil, ErrTokenInvalid
		}
	}
```

```go
// named reports whether p names a callout at a number that can ever be
// current: the first try of a callout carries major 1.
func named(p Pair) bool { return p.Callout != "" && p.Major != 0 }
```

Imports: drop `time` from the signature's needs (still used by `Verify`), add `"github.com/cyoda-platform/cyoda-go/internal/fence"`. Fix the wrap while here: `fmt.Errorf("failed to marshal claims: %w", err)`.

`internal/grpc/dispatch.go` — minimal mechanical adaptation (L-10 rewrites this function):

```go
// before
func (d *ProcessorDispatcher) resolveTxToken(ctx context.Context, txID string) string {
	...
	tok, err := d.signer.Issue(d.selfNodeID, txID, time.Now().Add(d.tokenTTL))
// after
func (d *ProcessorDispatcher) resolveTxToken(ctx context.Context, txID, requestID string) string {
	...
	tok, err := d.signer.Issue(token.Claims{
		NodeID:    d.selfNodeID,
		TxRef:     txID,
		ExpiresAt: time.Now().Add(d.tokenTTL).Unix(),
		Callout:   requestID,
		Major:     1,
	})
```

and at :119 `AttachTxToken(ce, d.resolveTxToken(ctx, txID, requestID))`.

`internal/cluster/dispatch/cluster_dispatcher.go` `mintTxToken` (O deletes the type):

```go
	t, err := d.signer.Issue(token.Claims{
		NodeID:    d.selfNodeID,
		TxRef:     txID,
		ExpiresAt: time.Now().Add(d.tokenTTL).Unix(),
		Callout:   uuid.NewString(),
		Major:     1,
	})
```

(import `github.com/google/uuid`).

The test files — one rule, applied to every call: `X.Issue(node, tx, exp)` becomes `X.Issue(token.Claims{NodeID: node, TxRef: tx, ExpiresAt: exp.Unix(), Callout: "req-" + tx, Major: 1})` where `tx` is a string literal or variable already in the call. Assertions are untouched: none of these tests reads a claim other than `NodeID`/`TxRef`. `internal/grpc/dispatch_txtoken_test.go`: `d.resolveTxToken(ctx, "tx-42")` → `d.resolveTxToken(ctx, "tx-42", "req-42")` (three calls) and `TestDispatch_MintsTxTokenFromTxID` additionally asserts `claims.Callout == "req-42" && claims.Major == 1`.

Decision for each existing test: all **stay**, adapted as above; none is deleted.

- [ ] **Step 4: Run to verify GREEN** — `go build ./... && go vet ./internal/cluster/... ./internal/grpc/... ./internal/httpmw/... ./internal/domain/txjoin/... ./internal/e2e/...`, then `go test ./internal/cluster/... ./internal/grpc/... ./internal/httpmw/... ./internal/domain/txjoin/...`. (`internal/e2e` is compiled by the vet; it runs in the final `make test-full`.)
Exit check: `grep -rn '\.Issue("' --include='*.go' .` → no hits.

- [ ] **Step 5: Commit**
`git add internal/cluster internal/grpc internal/httpmw internal/domain/txjoin internal/e2e/callback_txjoin_errors_test.go && git commit -m "feat(token): a pass names its callout and fencing number (#254)"`

---

### Task F-5: one fence per process, and a callout around every dispatch until the Coordinator exists

**Spec:** §7 ("One `*fence.Fence` per process"; §5's `fence.Begin … defer end()`; point 4). See "Order, and the seam" above for why this exists and who deletes it.

**Files:**
- Create: `app/once_fenced.go`, `app/once_fenced_test.go`
- Modify: `app/app.go` — struct (:60, add `fence *fence.Fence`); move `a.txGate = txgate.New()` from :644 up to just before `localDispatcher` (:441) and add `a.fence = fence.New(a.txGate)` beside it; wrap `extProc` in the two real-dispatcher branches (:546-558); accessor `func (a *App) Fence() *fence.Fence` beside `TokenSigner()` (:935) — the e2e harness of stream O drives the fence through it.
- Modify: `internal/cluster/dispatch/cluster_dispatcher.go` (`mintTxToken`: a pass already on the context wins)
- Test: `internal/cluster/dispatch/cluster_txtoken_test.go` (one new test)

**Interfaces:**
- Consumes: `fence` (F-3), `token.Claims` (F-4), `internalgrpc.WithTxToken` / `TxTokenFromContext` (`internal/grpc/txtoken.go:40-47`)
- Produces: `(*App).Fence() *fence.Fence`. `onceFenced` is unexported and temporary.

- [ ] **Step 1: Write the failing tests** — `app/once_fenced_test.go` (package `app`):

```go
package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// scriptedDispatch records the context each dispatch ran under.
type scriptedDispatch struct {
	during func(ctx context.Context) error
}

func (s *scriptedDispatch) DispatchProcessor(ctx context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
	return e, s.during(ctx)
}
func (s *scriptedDispatch) DispatchCriteria(ctx context.Context, _ *spi.Entity, _ json.RawMessage, _, _, _, _, _ string) (bool, string, error) {
	return true, "", s.during(ctx)
}
func (s *scriptedDispatch) DispatchFunction(ctx context.Context, _ *spi.Entity, _ spi.ScheduleFunction, _, _, _ string) (contract.FunctionResult, error) {
	return contract.FunctionResult{}, s.during(ctx)
}

func newOnceFencedForTest(t *testing.T, during func(ctx context.Context) error) (*onceFenced, *fence.Fence, *token.Signer) {
	t.Helper()
	signer, err := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	f := fence.New(txgate.New())
	return newOnceFenced(&scriptedDispatch{during: during}, f, signer, "node-A", time.Minute, common.NewTestUUIDGenerator()), f, signer
}

// The pass the dispatch carries is admitted while the dispatch runs and
// refused once it has returned — for all three kinds of callout.
func TestOnceFenced_PassIsCurrentForTheLengthOfTheDispatch(t *testing.T) {
	kinds := map[string]func(d *onceFenced, ctx context.Context) error{
		"processor": func(d *onceFenced, ctx context.Context) error {
			_, err := d.DispatchProcessor(ctx, &spi.Entity{}, spi.ProcessorDefinition{}, "wf", "tr", "tx-1")
			return err
		},
		"criteria": func(d *onceFenced, ctx context.Context) error {
			_, _, err := d.DispatchCriteria(ctx, &spi.Entity{}, nil, "TRANSITION", "wf", "tr", "", "tx-1")
			return err
		},
		"function": func(d *onceFenced, ctx context.Context) error {
			_, err := d.DispatchFunction(ctx, &spi.Entity{}, spi.ScheduleFunction{}, "wf", "tr", "tx-1")
			return err
		},
	}
	for name, dispatch := range kinds {
		t.Run(name, func(t *testing.T) {
			var f *fence.Fence
			var signer *token.Signer
			var pairs []fence.Pair
			d, f, signer := newOnceFencedForTest(t, func(ctx context.Context) error {
				claims, err := signer.Verify(internalgrpc.TxTokenFromContext(ctx))
				if err != nil {
					t.Errorf("the dispatch must carry a valid pass: %v", err)
					return nil
				}
				if claims.NodeID != "node-A" || claims.TxRef != "tx-1" || claims.Major != 1 || claims.Minor != 0 {
					t.Errorf("claims = %+v", claims)
				}
				pairs = []fence.Pair{{Callout: claims.Callout, Major: claims.Major}}
				if _, err := f.Admit(context.Background(), pairs); err != nil {
					t.Errorf("a callback during the dispatch must be admitted: %v", err)
				}
				return nil
			})
			if err := dispatch(d, context.Background()); err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			if _, err := f.Admit(context.Background(), pairs); !errors.Is(err, fence.ErrSuperseded) {
				t.Fatalf("a callback after the dispatch returned must be refused, got %v", err)
			}
		})
	}
}

// A callout made from inside a callback names its enclosing pairs in its pass
// and is released with them; the decorator then reports CALLOUT_SUPERSEDED.
func TestOnceFenced_ReleasedByTheFence_ReportsSuperseded(t *testing.T) {
	var f *fence.Fence
	var signer *token.Signer
	d, f, signer := newOnceFencedForTest(t, func(ctx context.Context) error {
		claims, err := signer.Verify(internalgrpc.TxTokenFromContext(ctx))
		if err != nil {
			t.Errorf("Verify: %v", err)
			return nil
		}
		if len(claims.Outer) != 1 || claims.Outer[0] != (fence.Pair{Callout: "outer", Major: 1}) {
			t.Errorf("Outer = %v; want the enclosing pair", claims.Outer)
		}
		f.Advance("outer", 2) // the owner gives the enclosing work to another cnode
		<-ctx.Done()
		return ctx.Err()
	})
	_, endOuter := f.Begin(context.Background(), "outer", "tx-1", nil)
	defer endOuter()
	f.Advance("outer", 1)
	callback, err := f.Admit(context.Background(), []fence.Pair{{Callout: "outer", Major: 1}})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}

	_, err = d.DispatchProcessor(callback, &spi.Entity{}, spi.ProcessorDefinition{}, "wf", "tr", "tx-1")
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeCalloutSuperseded {
		t.Fatalf("err = %v; want CALLOUT_SUPERSEDED", err)
	}
}

// A caller that went away is reported as today, not as a supersede.
func TestOnceFenced_CallerCancellation_IsNotASupersede(t *testing.T) {
	d, _, _ := newOnceFencedForTest(t, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := d.DispatchProcessor(ctx, &spi.Entity{}, spi.ProcessorDefinition{}, "wf", "tr", "tx-1")
	if !errors.Is(err, context.Canceled) || errors.Is(err, fence.ErrSuperseded) {
		t.Fatalf("err = %v; want context.Canceled unchanged", err)
	}
}

// A callout with no transaction carries no pass, and is still begun and ended.
func TestOnceFenced_NoTransaction_NoPass(t *testing.T) {
	d, _, _ := newOnceFencedForTest(t, func(ctx context.Context) error {
		if tok := internalgrpc.TxTokenFromContext(ctx); tok != "" {
			t.Error("a callout with no transaction must carry no pass")
		}
		return nil
	})
	if _, err := d.DispatchProcessor(context.Background(), &spi.Entity{}, spi.ProcessorDefinition{}, "wf", "tr", ""); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}
```

`internal/cluster/dispatch/cluster_txtoken_test.go`:

```go
func TestMintTxToken_APassOnTheContextWins(t *testing.T) {
	signer, err := token.NewSigner(testSecret32)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	d := &ClusterDispatcher{selfNodeID: "node-A", signer: signer, tokenTTL: time.Minute}
	ctx := internalgrpc.WithTxToken(context.Background(), "the-callouts-pass")
	if got := d.mintTxToken(ctx, "tx-9"); got != "the-callouts-pass" {
		t.Fatalf("mintTxToken = %q; want the pass already on the context", got)
	}
}
```

- [ ] **Step 2: Run to verify RED**
Run: `go test ./app/... ./internal/cluster/dispatch/... -run 'TestOnceFenced|TestMintTxToken_APassOnTheContextWins'`   Expected: FAIL to build — `undefined: onceFenced`, `too many arguments in call to d.mintTxToken`.

- [ ] **Step 3: Implement**

`app/once_fenced.go`:

```go
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// onceFenced makes every dispatch a callout of exactly one try as far as the
// fence is concerned: it begins the callout, advances it to its first number,
// mints the one pass of that try, and ends the callout when the dispatch
// returns. It is TEMPORARY: the owner's loop (internal/callout.Coordinator)
// begins, advances and ends callouts itself, and deletes this type when it
// becomes the ExternalProcessingService.
type onceFenced struct {
	inner      contract.ExternalProcessingService
	fence      *fence.Fence
	signer     *token.Signer
	selfNodeID string
	passTTL    time.Duration
	uuids      spi.UUIDGenerator
}

func newOnceFenced(inner contract.ExternalProcessingService, f *fence.Fence, signer *token.Signer, selfNodeID string, passTTL time.Duration, uuids spi.UUIDGenerator) *onceFenced {
	return &onceFenced{inner: inner, fence: f, signer: signer, selfNodeID: selfNodeID, passTTL: passTTL, uuids: uuids}
}

// begin opens the callout and returns the context the dispatch runs under, the
// func that ends the callout, and the func that turns the dispatch's error into
// CALLOUT_SUPERSEDED when the fence released it.
func (d *onceFenced) begin(ctx context.Context, txID string) (context.Context, func(), func(error) error, error) {
	calloutID := uuid.UUID(d.uuids.NewTimeUUID()).String()
	outer := fence.Pairs(ctx)
	cctx, end := d.fence.Begin(ctx, calloutID, txID, outer)
	d.fence.Advance(calloutID, 1)
	if txID != "" {
		pass, err := d.signer.Issue(token.Claims{
			NodeID:    d.selfNodeID,
			TxRef:     txID,
			ExpiresAt: time.Now().Add(d.passTTL).Unix(),
			Callout:   calloutID,
			Major:     1,
			Outer:     outer,
		})
		if err != nil {
			end()
			return nil, nil, nil, common.Internal("failed to mint the callout's pass", fmt.Errorf("failed to mint pass: %w", err))
		}
		cctx = internalgrpc.WithTxToken(cctx, pass)
	}
	outcome := func(err error) error {
		if err != nil && errors.Is(context.Cause(cctx), fence.ErrSuperseded) {
			return fence.NewSupersededError()
		}
		return err
	}
	return cctx, end, outcome, nil
}

func (d *onceFenced) DispatchProcessor(ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName, transitionName, txID string) (*spi.Entity, error) {
	cctx, end, outcome, err := d.begin(ctx, txID)
	if err != nil {
		return nil, err
	}
	defer end()
	res, err := d.inner.DispatchProcessor(cctx, entity, processor, workflowName, transitionName, txID)
	if err = outcome(err); err != nil {
		return nil, err
	}
	return res, nil
}

func (d *onceFenced) DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target, workflowName, transitionName, processorName, txID string) (bool, string, error) {
	cctx, end, outcome, err := d.begin(ctx, txID)
	if err != nil {
		return false, "", err
	}
	defer end()
	matches, reason, err := d.inner.DispatchCriteria(cctx, entity, criterion, target, workflowName, transitionName, processorName, txID)
	if err = outcome(err); err != nil {
		return false, "", err
	}
	return matches, reason, nil
}

func (d *onceFenced) DispatchFunction(ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction, workflowName, transitionName, txID string) (contract.FunctionResult, error) {
	cctx, end, outcome, err := d.begin(ctx, txID)
	if err != nil {
		return contract.FunctionResult{}, err
	}
	defer end()
	res, err := d.inner.DispatchFunction(cctx, entity, fn, workflowName, transitionName, txID)
	if err = outcome(err); err != nil {
		return contract.FunctionResult{}, err
	}
	return res, nil
}
```

`app/app.go`:

```go
// before (:644)
	a.txGate = txgate.New()
	entityHandler := entity.New(...)
// after — at :440, before localDispatcher
	// One lock registry and one fence per process: both callback doors, the
	// entity service and every callout judge a callback by the same state.
	a.txGate = txgate.New()
	a.fence = fence.New(a.txGate)
```

```go
// after the cluster / single-pnode branches (:546-558), inside each of the two
	extProc = newOnceFenced(extProc, a.fence, a.tokenSigner, a.selfNodeID, cfg.Cluster.TxTokenTTL, common.NewDefaultUUIDGenerator())
```

(not in the `cfg.ExternalProcessing != nil` branch: an injected service mints no passes.) `func (a *App) Fence() *fence.Fence { return a.fence }`.

`cluster_dispatcher.go`: `mintTxToken(txID string)` → `mintTxToken(ctx context.Context, txID string)`, first statement `if tok := internalgrpc.TxTokenFromContext(ctx); tok != "" { return tok }`; its three callers (:74, :122, :167) pass `ctx`.

- [ ] **Step 4: Run to verify GREEN** — `go test ./app/... ./internal/cluster/dispatch/... ./internal/grpc/...`

- [ ] **Step 5: Commit**
`git add app internal/cluster/dispatch && git commit -m "feat(app): one fence per process; every dispatch is a fenced callout of one try (#254)"`

---

### Task F-6: the check on entry — verify → Join (tenant) → Admit; a joined request is not cancelled by its client

**Spec:** §7 "Where it is enforced" point 1; "A joined request is not interrupted by its cnode going away"; §8.2 rows `CALLOUT_SUPERSEDED`, `TRANSACTION_NOT_FOUND`, 401, and the 403 of §13.

**Files:**
- Modify: `internal/domain/txjoin/txjoin.go` (`JoinFromToken`)
- Modify: `internal/httpmw/txjoin_mw.go` (`TxJoin`), `internal/grpc/txroute_interceptor.go` (`txRouteInterceptor`, `newTxRouteInterceptor`, :147, :188), `internal/grpc/server.go` (`NewServer`, :62-86), `app/app.go` (:712, :827)
- Test: `internal/domain/txjoin/txjoin_test.go`, `internal/httpmw/txjoin_mw_test.go`, `internal/grpc/txroute_interceptor_test.go`, `internal/grpc/txroute_search_ryw_test.go`, `internal/e2e/callback_txjoin_errors_test.go`; mechanical: the other callers of `NewServer` (`internal/cluster/proxy/grpc_forward_test.go`, `internal/grpc/recovery_test.go`, `internal/grpc/server_keepalive_test.go`)

**Interfaces:**
- Produces: `func JoinFromToken(ctx context.Context, signer *token.Signer, txMgr spi.TransactionManager, f *fence.Fence, tok string) (context.Context, error)`; `httpmw.TxJoin(signer, txMgr, f)`; `newTxRouteInterceptor(signer, reg, selfNodeID, txMgr, f, localGRPCPort, allowLoopback)`; `NewServer(..., tokenSigner, f *fence.Fence, nodeRegistry, ...)` (the fence directly after the signer). F-7 replaces the three `(signer, txMgr, f)` triples by one `*txjoin.Joiner`.

**What else hangs off a joined request's cancellation** (read, as the revised spec asks): (a) the client's disconnect (`r.Context()`; the gRPC stream context) — the point of the change; (b) a gRPC deadline the cnode set on its call (`grpc-timeout`) — no longer applies on the server; (c) `transactionTimeoutMillis` and search `timeoutMillis` — already refused on a joined request before they are attached (`entity/handler.go:679-690`, `search/handler.go:169-178`, `grpc/timeout.go:38`), so nothing is lost; (d) a callout the callback makes itself — `dispatchCalloutToMember` returns `ctx.Err()` on a dead parent (`dispatch.go:154, 187`), so until now a vanished cnode ended its inner callout; from here it ends by its answer limit or by the fence (point 4); (e) `cascadeAutomated`'s `currentCtx.Err()` check (`engine.go:884`) and the per-item check of a gRPC collection (`grpc/entity.go:388`) become inert for a joined chain, as the first already is after a `COMMIT_BEFORE_DISPATCH` segment; the context checks inside the memory and SQLite scan loops likewise; (f) a callback that was proxied to the owner and outlives `CYODA_PROXY_TIMEOUT` is answered `503` by the pnode it arrived at while it completes on the owner — F-7 puts the sentence for cnode authors into the help; (g) a forced gRPC `Stop()` after the drain budget (`app.go:1010`) no longer interrupts a joined request — it finishes or the process exits. Tracing, diagnostics, the user context and the transaction are context *values* and are kept.

- [ ] **Step 1: Write the failing tests**

`internal/domain/txjoin/txjoin_test.go` — every existing call gains the fence argument; the existing `TestJoinFromToken_JoinsValid` and any other success case uses `liveFence`. New helper and tests:

```go
// liveFence returns a fence on which calloutID is in progress on txID at
// major 1, and the pass claims that name it.
func liveFence(t *testing.T, calloutID, txID string) (*fence.Fence, token.Claims) {
	t.Helper()
	f := fence.New(txgate.New())
	_, end := f.Begin(context.Background(), calloutID, txID, nil)
	t.Cleanup(end)
	f.Advance(calloutID, 1)
	return f, token.Claims{NodeID: "local", TxRef: txID, ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: calloutID, Major: 1}
}

func assertAppErr(t *testing.T, err error, status int, code string) {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("err = %v (%T); want *common.AppError", err, err)
	}
	if appErr.Status != status || appErr.Code != code {
		t.Fatalf("err = %d %s; want %d %s", appErr.Status, appErr.Code, status, code)
	}
}

func TestJoinFromToken_AdmitsUnderTheCurrentNumber(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, claims := liveFence(t, "req-1", "tx-1")
	tok, _ := s.Issue(claims)
	ctx, err := JoinFromToken(context.Background(), s, fakeTM{}, f, tok)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tx := spi.GetTransaction(ctx); tx == nil || tx.ID != "tx-1" {
		t.Fatalf("expected joined tx-1, got %+v", tx)
	}
	if got := fence.Pairs(ctx); len(got) != 1 || got[0] != (fence.Pair{Callout: "req-1", Major: 1}) {
		t.Fatalf("Pairs = %v", got)
	}
}

func TestJoinFromToken_CalloutEnded_410(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f := fence.New(txgate.New())
	_, end := f.Begin(context.Background(), "req-1", "tx-1", nil)
	f.Advance("req-1", 1)
	end()
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})
	base := context.Background()
	ctx, err := JoinFromToken(base, s, fakeTM{}, f, tok)
	assertAppErr(t, err, http.StatusGone, common.ErrCodeCalloutSuperseded)
	if ctx != base {
		t.Fatal("a refused join must hand back the caller's context")
	}
}

func TestJoinFromToken_ReplacedWhileCalloutInProgress_410(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, claims := liveFence(t, "req-1", "tx-1")
	tok, _ := s.Issue(claims)
	f.Advance("req-1", 2)
	_, err := JoinFromToken(context.Background(), s, fakeTM{}, f, tok)
	assertAppErr(t, err, http.StatusGone, common.ErrCodeCalloutSuperseded)
}

func TestJoinFromToken_EnclosingCalloutNotCurrent_410(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, claims := liveFence(t, "req-inner", "tx-1")
	claims.Outer = []token.Pair{{Callout: "req-outer", Major: 1}} // never begun
	tok, _ := s.Issue(claims)
	_, err := JoinFromToken(context.Background(), s, fakeTM{}, f, tok)
	assertAppErr(t, err, http.StatusGone, common.ErrCodeCalloutSuperseded)
}

// The transaction is looked up, and its tenant checked, BEFORE the fence is
// consulted.
func TestJoinFromToken_JoinComesBeforeTheFence(t *testing.T) {
	tests := []struct {
		name    string
		joinErr error
		status  int
		code    string
	}{
		{"TxGoneBeatsCalloutEnded_404", spi.ErrTxNotFound, http.StatusNotFound, common.ErrCodeTransactionNotFound},
		{"TenantMismatchBeatsTheFence_403", spi.ErrTxTenantMismatch, http.StatusForbidden, common.ErrCodeForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := token.NewSigner(make32(t))
			// Callout ended: the fence alone would answer 410.
			f := fence.New(txgate.New())
			tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})
			_, err := JoinFromToken(context.Background(), s, fakeTM{joinErr: tc.joinErr}, f, tok)
			assertAppErr(t, err, tc.status, tc.code)
		})
	}
}

// A stolen pass absorbs nothing: another tenant presenting a higher minor must
// not shut the rightful cnode out.
func TestJoinFromToken_TenantMismatch_AbsorbsNothing(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, claims := liveFence(t, "req-1", "tx-1")
	claims.Minor = 1
	rightful, err := f.Admit(context.Background(), []fence.Pair{{Callout: "req-1", Major: 1, Minor: 1}})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	claims.Minor = 9
	tok, _ := s.Issue(claims)
	_, err = JoinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxTenantMismatch}, f, tok)
	assertAppErr(t, err, http.StatusForbidden, common.ErrCodeForbidden)
	if err := fence.Check(rightful); err != nil {
		t.Fatalf("a refused join changed the fence: %v", err)
	}
}

func TestJoinFromToken_NoCalloutAndNumber_401(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	_, err := JoinFromToken(context.Background(), s, fakeTM{}, fence.New(txgate.New()), tok)
	assertAppErr(t, err, http.StatusUnauthorized, common.ErrCodeUnauthorized)
	var appErr *common.AppError
	_ = errors.As(err, &appErr)
	if appErr.Message != "UNAUTHORIZED: invalid transaction token" {
		t.Fatalf("message = %q", appErr.Message)
	}
}

// A cnode that disconnects in the middle of its callback cancels the request's
// context; the joined request must not see it.
func TestJoinFromToken_JoinedContextIsNotCancelledByItsClient(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, claims := liveFence(t, "req-1", "tx-1")
	tok, _ := s.Issue(claims)
	request, disconnect := context.WithCancel(context.Background())
	joined, err := JoinFromToken(request, s, fakeTM{}, f, tok)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	disconnect()
	if joined.Err() != nil {
		t.Fatalf("the joined context was cancelled by its client: %v", joined.Err())
	}
	if spi.GetTransaction(joined) == nil || len(fence.Pairs(joined)) != 1 {
		t.Fatal("detaching cancellation must keep the transaction and the pairs")
	}
}
```

`internal/httpmw/txjoin_mw_test.go` — existing `TxJoin(s, tm)` calls become `TxJoin(s, tm, f)` with `liveFence`-style setup (copy the helper; it is twelve lines and the package has no shared test-support package); new:

```go
func TestTxJoin_CalloutEnded_410(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f := fence.New(txgate.New())
	_, end := f.Begin(context.Background(), "req-1", "tx-1", nil)
	f.Advance("req-1", 1)
	end()
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})

	for _, method := range []string{http.MethodGet, http.MethodPost} { // a read and a write
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") })
		req := withUserCtx(httptest.NewRequest(method, "/entity/x", nil))
		req.Header.Set(proxy.TxTokenHeader, tok)
		rec := httptest.NewRecorder()
		TxJoin(s, fakeJoinTM{}, f)(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusGone {
			t.Fatalf("%s: status = %d; want 410", method, rec.Code)
		}
		var pd struct {
			Properties struct {
				ErrorCode string `json:"errorCode"`
				Retryable bool   `json:"retryable"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &pd); err != nil {
			t.Fatalf("body: %v", err)
		}
		if pd.Properties.ErrorCode != "CALLOUT_SUPERSEDED" || pd.Properties.Retryable {
			t.Fatalf("%s: problem = %+v", method, pd.Properties)
		}
	}
}
```

`internal/grpc/txroute_interceptor_test.go` — a helper and the G layer; every existing `newTxRouteInterceptor(s, reg, self, tm, 9090, true)` becomes `newTxRouteInterceptor(s, reg, self, tm, f, 9090, true)`: tests whose handler must run use `liveRouteFence`, tests that are refused earlier pass `fence.New(txgate.New())`.

```go
func liveRouteFence(t *testing.T, txID string) (*fence.Fence, token.Claims) {
	t.Helper()
	f := fence.New(txgate.New())
	calloutID := "req-" + txID
	_, end := f.Begin(context.Background(), calloutID, txID, nil)
	t.Cleanup(end)
	f.Advance(calloutID, 1)
	return f, token.Claims{NodeID: "local", TxRef: txID, ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: calloutID, Major: 1}
}

// Owner gave the work to a second cnode of its own: the first cnode's callback
// is refused at once, while the callout is still in progress — on the write
// RPC and on the read RPC.
func TestTxRouteInterceptor_SupersededWhileCalloutInProgress(t *testing.T) {
	for name, info := range map[string]*googlegrpc.UnaryServerInfo{
		"EntityManage (write)": entityManageInfo(),
		"EntitySearch (read)":  {FullMethod: cyodapb.CloudEventsService_EntitySearch_FullMethodName},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := token.NewSigner(make32(t))
			f, claims := liveRouteFence(t, "tx-1")
			tok, _ := s.Issue(claims)
			f.Advance(claims.Callout, 2)
			ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", fakeJoinTM{}, f, 9090, true)
			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))
			resp, err := ic.unary()(ctx, &cepb.CloudEvent{Id: "req-late"}, info, func(context.Context, any) (any, error) {
				t.Fatal("handler must not run for a cnode that was replaced")
				return nil, nil
			})
			if err != nil {
				t.Fatalf("expected envelope response, got gRPC err: %v", err)
			}
			assertEnvelopeCode(t, resp, "req-late", "CALLOUT_SUPERSEDED")
		})
	}
}

// Late callback after the callout ended, on both server-streaming RPCs.
func TestTxRouteInterceptor_SupersededAfterCalloutEnded_Stream(t *testing.T) {
	for name, method := range map[string]string{
		"EntityManageCollection": cyodapb.CloudEventsService_EntityManageCollection_FullMethodName,
		"EntitySearchCollection": cyodapb.CloudEventsService_EntitySearchCollection_FullMethodName,
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := token.NewSigner(make32(t))
			f := fence.New(txgate.New())
			_, end := f.Begin(context.Background(), "req-tx-1", "tx-1", nil)
			f.Advance("req-tx-1", 1)
			end()
			tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-1", Major: 1})
			ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", fakeJoinTM{}, f, 9090, true)
			ss := newFakeServerStream(metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok)))
			err := ic.stream()(nil, ss, &googlegrpc.StreamServerInfo{FullMethod: method}, func(any, googlegrpc.ServerStream) error {
				t.Fatal("handler must not run once the callout has ended")
				return nil
			})
			if err != nil {
				t.Fatalf("expected an envelope on the stream, got gRPC err: %v", err)
			}
			assertStreamEnvelopeCode(t, ss, "CALLOUT_SUPERSEDED")
		})
	}
}
```

`newFakeServerStream` / `assertStreamEnvelopeCode`: the file already drives `ic.stream()` with a fake stream for the expired/forged stream cases (`txroute_interceptor_test.go:608-760`); use that double and its assertion under whatever names it has there — do not add a second one.

`internal/e2e/callback_txjoin_errors_test.go` — new subtest in `TestCallbackErr_LoudFailCodes`, beside `Forged_401`:

```go
	// A pass with no callout and number — what an earlier version minted — is
	// malformed: 401, as any invalid pass.
	t.Run("NoCalloutAndNumber_401", func(t *testing.T) {
		tok, err := h.app.TokenSigner().Issue(token.Claims{
			NodeID: "local", TxRef: "tx-" + randSuffix(t), ExpiresAt: time.Now().Add(time.Minute).Unix(),
		})
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		resp := h.DoAuth(t, http.MethodGet, probePath, "", tok)
		body := h.readBody(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d; want 401 (body: %s)", resp.StatusCode, body)
		}
		if code := problemErrorCode(body); code != "UNAUTHORIZED" {
			t.Fatalf("errorCode = %q; want UNAUTHORIZED (body: %s)", code, body)
		}
	})
```

The existing tenant-mismatch case of that file holds a real transaction open inside a blocking processor and presents the live pass as another tenant; with F-5 that pass names a callout in progress, so the case now also proves "403 before any fencing answer". It stays as it is.

- [ ] **Step 2: Run to verify RED**
Run: `go test ./internal/domain/txjoin/... ./internal/httpmw/... ./internal/grpc/... -run 'TestJoinFromToken|TestTxJoin|TestTxRouteInterceptor'`   Expected: FAIL to build — `too many arguments in call to JoinFromToken`.

- [ ] **Step 3: Implement** — `internal/domain/txjoin/txjoin.go`:

```go
// before
func JoinFromToken(ctx context.Context, signer *token.Signer, txMgr spi.TransactionManager, tok string) (context.Context, error) {
	...
	return joined, nil
}
```

```go
// after
// Order: verify the pass → Join, which checks the tenant → the fence. The
// tenant check comes first so that a stolen pass tells another tenant nothing
// about which callouts exist, and a callback that arrives after the
// TRANSACTION has ended is answered TRANSACTION_NOT_FOUND as before;
// CALLOUT_SUPERSEDED is the answer while the transaction is still open.
//
// The returned context is detached from the request's cancellation: a joined
// request runs on the transaction of the operation it belongs to, and a compute
// node that goes away in the middle of a statement must not take that
// operation's connection with it. A joined request ends by finishing or by
// being refused at a check.
func JoinFromToken(ctx context.Context, signer *token.Signer, txMgr spi.TransactionManager, f *fence.Fence, tok string) (context.Context, error) {
	...
	// (verify and Join unchanged)

	pairs := make([]fence.Pair, 0, 1+len(claims.Outer))
	pairs = append(pairs, fence.Pair{Callout: claims.Callout, Major: claims.Major, Minor: claims.Minor})
	pairs = append(pairs, claims.Outer...)
	admitted, err := f.Admit(joined, pairs)
	if err != nil {
		return ctx, err
	}
	return context.WithoutCancel(admitted), nil
}
```

Add to the doc comment's error table: `a pass that is no longer current → 410 CALLOUT_SUPERSEDED`. Callers: `httpmw.TxJoin(signer, txMgr, f)`; `txRouteInterceptor` gains a `fence *fence.Fence` field set by `newTxRouteInterceptor`; `NewServer` gains the parameter and passes it on; `app/app.go:712` `httpmw.TxJoin(a.tokenSigner, a.transactionManager, a.fence)`, `:827` `… a.tokenSigner, a.fence, a.nodeRegistry, …`.

- [ ] **Step 4: Run to verify GREEN** — `go build ./... && go test ./internal/domain/txjoin/... ./internal/httpmw/... ./internal/grpc/... ./internal/cluster/... ./app/...`; then the callback e2e files, which prove the passes F-5 mints are admitted through the whole stack: `go test ./internal/e2e/... -run 'TestCallback'`.

- [ ] **Step 5: Commit**
`git add internal/domain/txjoin internal/httpmw internal/grpc internal/cluster/proxy/grpc_forward_test.go internal/e2e/callback_txjoin_errors_test.go app/app.go && git commit -m "feat(txjoin): a callback is admitted only under its callout's current number (#254)"`

---

### Task F-7: every joined request holds the transaction's lock; its response is sent after the lock is released

**Spec:** §7 point 2 (first paragraph), "One transaction, one user at a time" (all three consequences), the paragraph "There is deliberately no check before a joined operation's final write"; **V-5**.

**V-5, settled.** Behind the HTTP join middleware (`app/app.go:712-771`): `GET /entity/{entityId}/transitions`, `GET /platform-api/entity/fetch/transitions`, `POST /entity/stats/{entityName}/{modelVersion}/query`, and the whole generated API mounted at `/` (:771 — every operation of `api/openapi.yaml`: entity, search, model, workflow, message, audit, statistics, account). `grep -rn 'http.Flusher\|\.Flush()\|event-stream\|Hijack\|NewResponseController' internal/ app/` has no hit in a handler. The one `application/x-ndjson` writer (`internal/domain/search/handler.go:206-222`) encodes a slice `Search` has already returned, bounded by the direct-search limit; grouped stats drains its iterator before responding (`entity/grouped_stats_service.go:316-327`). Routed gRPC methods (`txroute_interceptor.go:78-98`): `EntityManage`, `EntitySearch` (unary — the response is a return value) and `EntityManageCollection`, `EntitySearchCollection` (server-streaming — every `stream.Send` in `internal/grpc/entity.go:299-515` and `search.go:281-456` iterates a materialised slice or sends one envelope). **None streams without end**; buffering holds nothing that was not already in memory.

**The lock is held only while the request is wholly in memory — in and out** (spec §7, commit `d4e53311`). For a server-streaming RPC the generated handler calls `RecvMsg` inside `handler(srv, ss)`; a cnode that opens the stream with a valid pass and never sends its request would hold the transaction's lock with no bound at all, and `Advance` has no timeout. The interceptor therefore receives the one request message **before** it takes the lock and replays it to the handler, as `proxyStream` already does (`txroute_interceptor.go:200-203`). Over HTTP the middleware reads the body before it takes the lock and hands the handler a reader over the bytes. gRPC unary needs nothing.

**The size limit of the join layer: 10 MiB.** Limits that exist today: every handler behind the middleware wraps its own body in `http.MaxBytesReader` — `maxEntityBodySize` = 10 MiB (`entity/handler.go:27`, six uses), `maxGroupedStatsBodySize` = 10 MiB, `maxSearchBodySize` = 10 MiB, workflow import 10 MiB, model import 10 MiB / model 1 MiB (`model/handler.go:81, 193, 228`), messaging 10 MiB (`messaging/handler.go:35, 269`), account I/O smaller (`account/io.go`). There is **no** server-wide body limit (`cmd/cyoda/httpserver.go` sets only timeouts). The join layer uses the largest of them, 10 MiB, as one constant, so that it never refuses a body a handler would accept; a handler with a smaller cap still applies it to the buffered bytes and answers as today. A body over 10 MiB is answered `413` `BAD_REQUEST` by the join layer before any lock is taken; a body that cannot be read (the client went away) `400` `BAD_REQUEST`.

**A handler's own `http.MaxBytesReader(w, …)` has nothing left to signal** — confirmed: its only effect beyond the read error is to tell the *real* `ResponseWriter` to close the connection so that an unread remainder is not drained (`net/http`'s `requestTooLarge`); through the buffering writer that signal is lost, and nothing depends on it, because the join layer has already consumed the whole body. The read error itself, and the handler's response to it, are unchanged.

**The release is deferred**, so a panicking handler gives the lock back: the recovery layers sit *outside* the join layer (`middleware.Recovery` wraps the assembled handler, `app/app.go:824`; `UnaryRecoveryInterceptor` / `StreamRecoveryInterceptor` are first in the chains, `internal/grpc/server.go:88-97`), so the panic unwinds through `Joiner.Run`.

**What `WithoutCancel` gives up** is listed in F-6 and in the spec. One consequence reaches cnode authors: a callback that was **proxied** to the owner and outlives `CYODA_PROXY_TIMEOUT` is answered `503` by the pnode it arrived at while it runs to completion on the owner. The help text says so (Step 3).

**Files:**
- Modify: `internal/domain/txjoin/txjoin.go` (new `Joiner`, `NewJoiner`, `Run`)
- Create: `internal/httpmw/buffered_writer.go`; Modify: `internal/httpmw/txjoin_mw.go`
- Modify: `internal/grpc/txroute_interceptor.go` (unary, stream; new `heldStream`), `internal/grpc/server.go` (`NewServer` takes the `*txjoin.Joiner` in place of the fence added in F-6; `txMgr` stays, other code uses it), `app/app.go` (:712, :827; build `a.joiner`)
- Modify: `internal/domain/entity/handler.go` — **delete** `acquireJoinedGate` (:108-125); `internal/domain/entity/service.go` — delete the nine `if !owned { … acquireJoinedGate … }` blocks (:271-275, :673-677, :807-811, :1272-1276, :1481-1485, :1701-1705, :1952-1956, :2154-2158, :2511-2515) and correct the comments that describe them (:262-265 "acquired below", :352-356 "The joined path already holds the gate for its whole body" stays true and now names the join layer; `txscope.go:29-41`'s paragraph on `releaseGate` ordering is rewritten: the join layer releases after the handler, hence after `scope.Release`).
- Modify: `cmd/cyoda/help/content/grpc.md` (`## COMPUTE MEMBER PROTOCOL`)
- Test: `internal/domain/txjoin/run_test.go` (new), `internal/httpmw/txjoin_mw_test.go`, `internal/grpc/txroute_interceptor_test.go`, `internal/domain/workflow/transitions_fence_test.go` (new); existing entity tests that call the service with a hand-joined context (`internal/domain/entity/service_txjoin_test.go` and the seven other files listed by `grep -ln '\.Join(' internal/domain/entity/*_test.go`) **stay** — they no longer take a lock, which is correct for a single goroutine; any of them that asserts gate behaviour through `acquireJoinedGate` (search: `grep -n 'acquireJoinedGate\|WithHeld' internal/domain/entity/*_test.go`) is **moved** to `run_test.go` against `Joiner.Run`.

**Interfaces:**
- Produces:
  - `func NewJoiner(signer *token.Signer, txMgr spi.TransactionManager, f *fence.Fence, gate *txgate.Registry) *Joiner`
  - `func (j *Joiner) Run(ctx context.Context, tok string, handler func(ctx context.Context)) error` — empty `tok`: `handler(ctx)`, no lock, nil. Otherwise `JoinFromToken` → `gate.Acquire(txID)` → `txgate.WithHeld` → `fence.Check` under the lock → `handler(joinedCtx)` → release. The error is a join or fence refusal and means the handler did **not** run. The caller sends its response after `Run` returns.
  - `httpmw.TxJoin(j *txjoin.Joiner)`; `newTxRouteInterceptor(signer, reg, selfNodeID, j, localGRPCPort, allowLoopback)` (the signer stays for `proxy.ResolveNodeInfo`).
- Exit checks: `grep -rn 'acquireJoinedGate' --include='*.go' .` → no hits; `grep -rn 'JoinFromToken(' --include='*.go' . | grep -v '_test.go' | grep -v 'internal/domain/txjoin/'` → no hits (the doors go through `Run` only).

- [ ] **Step 1: Write the failing tests**

`internal/domain/txjoin/run_test.go` — on the **memory** backend; run with `-race`:

```go
package txjoin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

type runEnv struct {
	ctx     context.Context
	factory *memory.StoreFactory
	txMgr   spi.TransactionManager
	gate    *txgate.Registry
	fence   *fence.Fence
	signer  *token.Signer
	joiner  *Joiner
	txID    string
	txCtx   context.Context
	end     func()
}

// newRunEnv opens a transaction on the memory backend with one callout in
// progress on it, and seeds two committed entities to read.
func newRunEnv(t *testing.T) (*runEnv, string) {
	t.Helper()
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u", Tenant: spi.Tenant{ID: "run-tenant", Name: "run"}, Roles: []string{"user"},
	})
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	es, _ := factory.EntityStore(ctx)
	ref := spi.ModelRef{EntityName: "Widget", ModelVersion: "1"}
	for _, id := range []string{"e-1", "e-2"} {
		if _, err := es.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: id, TenantID: "run-tenant", ModelRef: ref, State: "S"}, Data: []byte(`{"n":1}`)}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	signer, _ := token.NewSigner(make32(t))
	gate := txgate.New()
	f := fence.New(gate)
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_, end := f.Begin(ctx, "req-1", txID, nil)
	t.Cleanup(end)
	f.Advance("req-1", 1)
	pass, _ := signer.Issue(token.Claims{NodeID: "local", TxRef: txID, ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})
	return &runEnv{ctx: ctx, factory: factory, txMgr: txMgr, gate: gate, fence: f, signer: signer,
		joiner: NewJoiner(signer, txMgr, f, gate), txID: txID, txCtx: txCtx, end: end}, pass
}

// Two parallel joined reads, and reads against a joined write, on one
// transaction. On the memory backend a Get writes tx.ReadSet under a READ lock
// (plugins/memory/entity_store.go:515-536): without the join layer's lock this
// test dies with the runtime's "concurrent map writes" (and is a data race
// under -race).
func TestRun_ParallelJoinedRequestsAreSerialised(t *testing.T) {
	env, pass := newRunEnv(t)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				err := env.joiner.Run(env.ctx, pass, func(ctx context.Context) {
					es, err := env.factory.EntityStore(ctx)
					if err != nil {
						t.Errorf("EntityStore: %v", err)
						return
					}
					if w == 0 { // one writer among the readers
						_, err = es.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: "w-" + string(rune('a'+i%26)), TenantID: "run-tenant",
							ModelRef: spi.ModelRef{EntityName: "Widget", ModelVersion: "1"}, State: "S", TransactionID: env.txID}, Data: []byte(`{"n":2}`)})
					} else {
						_, err = es.Get(ctx, []string{"e-1", "e-2"}[i%2])
					}
					if err != nil {
						t.Errorf("store: %v", err)
					}
				})
				if err != nil {
					t.Errorf("Run: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// The handler runs under the transaction's lock, with the suspendable handle
// the engine uses installed.
func TestRun_HandlerHoldsTheLock_AndCanSuspendIt(t *testing.T) {
	env, pass := newRunEnv(t)
	err := env.joiner.Run(env.ctx, pass, func(ctx context.Context) {
		acquired := make(chan struct{})
		go func() { env.gate.Acquire(env.txID)(); close(acquired) }()
		select {
		case <-acquired:
			t.Error("the lock was free while a joined handler ran")
		case <-time.After(50 * time.Millisecond):
		}
		resume := txgate.Suspend(ctx) // as the engine does across a callout of the callback's own
		select {
		case <-acquired:
		case <-time.After(5 * time.Second):
			t.Error("Suspend did not release the lock the join layer took")
		}
		resume()
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Released after the handler returned, re-acquire included.
	done := make(chan struct{})
	go func() { env.gate.Acquire(env.txID)(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock is still held after Run returned")
	}
}

// A joined request queued for the lock when its cnode is replaced is refused on
// taking the lock: its handler never runs, nothing is written or read.
func TestRun_QueuedRequestIsRefusedOnTakingTheLock(t *testing.T) {
	env, pass := newRunEnv(t)
	holder := env.gate.Acquire(env.txID) // another request of the same cnode, in progress

	ran := false
	var runErr error
	queued := make(chan struct{})
	go func() {
		defer close(queued)
		runErr = env.joiner.Run(env.ctx, pass, func(context.Context) { ran = true })
	}()
	time.Sleep(50 * time.Millisecond) // admitted on entry, now queued for the lock

	advanced := make(chan struct{})
	go func() { defer close(advanced); env.fence.Advance("req-1", 2) }()
	time.Sleep(50 * time.Millisecond)
	holder()

	for name, ch := range map[string]chan struct{}{"the queued request": queued, "Advance": advanced} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not finish", name)
		}
	}
	if ran {
		t.Fatal("the handler of a superseded request ran")
	}
	var appErr *common.AppError
	if !errors.As(runErr, &appErr) || appErr.Code != common.ErrCodeCalloutSuperseded {
		t.Fatalf("err = %v; want CALLOUT_SUPERSEDED", runErr)
	}
}

// A joined request past its check when its callout ends runs to completion —
// all of it, however many items it writes — and the owner proceeds only
// afterwards. It is not refused: Run reports no error.
func TestRun_OwnerWaitsForARequestInProgress(t *testing.T) {
	env, pass := newRunEnv(t)
	inHandler := make(chan struct{})
	finish := make(chan struct{})
	var runErr error
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		runErr = env.joiner.Run(env.ctx, pass, func(ctx context.Context) {
			close(inHandler)
			<-finish
			es, _ := env.factory.EntityStore(ctx)
			for _, id := range []string{"c-1", "c-2", "c-3"} { // a joined collection
				if _, err := es.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: id, TenantID: "run-tenant",
					ModelRef: spi.ModelRef{EntityName: "Widget", ModelVersion: "1"}, State: "S", TransactionID: env.txID}, Data: []byte(`{"n":3}`)}); err != nil {
					t.Errorf("Save %s: %v", id, err)
				}
			}
		})
	}()
	<-inHandler

	ownerProceeds := make(chan struct{})
	go func() { defer close(ownerProceeds); env.end() }() // the callout ends: answered, failed or abandoned
	select {
	case <-ownerProceeds:
		t.Fatal("the owner proceeded while a joined request was in progress")
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	<-ownerProceeds
	<-requestDone
	if runErr != nil {
		t.Fatalf("a request past its last check is answered normally, got %v", runErr)
	}
	if err := env.txMgr.Commit(env.txCtx, env.txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	es, _ := env.factory.EntityStore(env.ctx)
	for _, id := range []string{"c-1", "c-2", "c-3"} {
		if _, err := es.Get(env.ctx, id); err != nil {
			t.Fatalf("%s is missing: the collection did not land whole: %v", id, err)
		}
	}
}

// The release is deferred: a handler that panics gives the lock back. The
// recovery layers sit outside the join layer, so the panic passes through Run.
func TestRun_PanickingHandlerGivesTheLockBack(t *testing.T) {
	env, pass := newRunEnv(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic must pass through Run to the recovery layer")
			}
		}()
		_ = env.joiner.Run(env.ctx, pass, func(context.Context) { panic("handler blew up") })
	}()
	free := make(chan struct{})
	go func() { env.gate.Acquire(env.txID)(); close(free) }()
	select {
	case <-free:
	case <-time.After(5 * time.Second):
		t.Fatal("a panicking joined handler kept the transaction's lock: the owner would wait for ever")
	}
	// Also after a Suspend/resume inside the handler: the deferred release
	// frees the RE-acquired lock.
	func() {
		defer func() { _ = recover() }()
		_ = env.joiner.Run(env.ctx, pass, func(ctx context.Context) {
			resume := txgate.Suspend(ctx)
			resume()
			panic("after resume")
		})
	}()
	free2 := make(chan struct{})
	go func() { env.gate.Acquire(env.txID)(); close(free2) }()
	select {
	case <-free2:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock re-acquired by resume was not released on panic")
	}
}

func TestRun_NoPass_RunsWithoutALock(t *testing.T) {
	env, _ := newRunEnv(t)
	held := env.gate.Acquire(env.txID)
	defer held()
	ran := false
	if err := env.joiner.Run(env.ctx, "", func(ctx context.Context) {
		ran = spi.GetTransaction(ctx) == nil
	}); err != nil || !ran {
		t.Fatalf("an ordinary request must run as it is: ran=%v err=%v", ran, err)
	}
}
```

`internal/httpmw/txjoin_mw_test.go`:

```go
// The response is sent after the lock is released: a cnode that does not read
// its response does not hold the transaction.
func TestTxJoin_ResponseIsSentAfterTheLockIsReleased(t *testing.T) {
	j, gate, pass := liveJoiner(t, "tx-1") // fakeJoinTM, fence with req-tx-1 at major 1
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	w := &lockProbeWriter{ResponseRecorder: httptest.NewRecorder(), gate: gate, txID: "tx-1"}
	req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity", strings.NewReader(`{}`)))
	req.Header.Set(proxy.TxTokenHeader, pass)
	TxJoin(j)(next).ServeHTTP(w, req)

	if w.Code != http.StatusCreated || w.Body.String() != `{"ok":true}` || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if w.writes == 0 || w.writesUnderLock != 0 {
		t.Fatalf("%d of %d writes to the client were made while the transaction's lock was held", w.writesUnderLock, w.writes)
	}
}

// lockProbeWriter notes, on every write to the client, whether the
// transaction's lock is free.
type lockProbeWriter struct {
	*httptest.ResponseRecorder
	gate                    *txgate.Registry
	txID                    string
	writes, writesUnderLock int
}

func (w *lockProbeWriter) probe() {
	w.writes++
	free := make(chan struct{})
	go func() { w.gate.Acquire(w.txID)(); close(free) }()
	select {
	case <-free:
	case <-time.After(50 * time.Millisecond):
		w.writesUnderLock++
	}
}
func (w *lockProbeWriter) WriteHeader(code int)        { w.probe(); w.ResponseRecorder.WriteHeader(code) }
func (w *lockProbeWriter) Write(b []byte) (int, error) { w.probe(); return w.ResponseRecorder.Write(b) }

// Every route behind the middleware runs under the lock, whatever it serves —
// model, message, search, statistics, audit are all just `next` to it — and is
// refused once its cnode is replaced.
func TestTxJoin_EveryRouteRunsUnderTheLock(t *testing.T) {
	j, gate, pass := liveJoiner(t, "tx-1")
	for _, route := range []string{"/model/export/x/1", "/message/new/s", "/search/direct/x/1", "/entity/stats", "/audit/entity/x"} {
		held := false
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			free := make(chan struct{})
			go func() { gate.Acquire("tx-1")(); close(free) }()
			select {
			case <-free:
			case <-time.After(50 * time.Millisecond):
				held = true
			}
		})
		req := withUserCtx(httptest.NewRequest(http.MethodPost, route, nil))
		req.Header.Set(proxy.TxTokenHeader, pass)
		TxJoin(j)(next).ServeHTTP(httptest.NewRecorder(), req)
		if !held {
			t.Fatalf("%s ran without the transaction's lock", route)
		}
	}
}

// The request body is read before the lock is taken: a cnode that trickles its
// body does not hold the transaction.
func TestTxJoin_BodyIsReadBeforeTheLockIsTaken(t *testing.T) {
	j, gate, pass := liveJoiner(t, "tx-1")
	body := &lockProbeBody{Reader: strings.NewReader(`{"a":1}`), gate: gate, txID: "tx-1"}
	var got string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	})
	req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity", body))
	req.Header.Set(proxy.TxTokenHeader, pass)
	TxJoin(j)(next).ServeHTTP(httptest.NewRecorder(), req)
	if got != `{"a":1}` {
		t.Fatalf("handler read %q", got)
	}
	if body.readsUnderLock != 0 {
		t.Fatalf("%d reads of the client's body were made under the transaction's lock", body.readsUnderLock)
	}
}
```

```go
// A joined request whose client sends its headers and then stalls does not
// hold the transaction's lock: another joined request on the same transaction
// completes meanwhile.
func TestTxJoin_StalledBodyDoesNotHoldTheLock(t *testing.T) {
	j, _, pass := liveJoiner(t, "tx-1")
	mw := TxJoin(j)

	stalled, unblock := io.Pipe() // headers sent, body never arrives
	stalledDone := make(chan struct{})
	go func() {
		defer close(stalledDone)
		req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity", stalled))
		req.Header.Set(proxy.TxTokenHeader, pass)
		mw(okHandler()).ServeHTTP(httptest.NewRecorder(), req)
	}()
	time.Sleep(50 * time.Millisecond) // the stalled request is inside the middleware, reading

	otherDone := make(chan int, 1)
	go func() {
		req := withUserCtx(httptest.NewRequest(http.MethodGet, "/entity/x", nil))
		req.Header.Set(proxy.TxTokenHeader, pass)
		rec := httptest.NewRecorder()
		mw(okHandler()).ServeHTTP(rec, req)
		otherDone <- rec.Code
	}()
	select {
	case code := <-otherDone:
		if code != http.StatusOK {
			t.Fatalf("the other joined request = %d; want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled body held the transaction's lock: the other joined request did not complete")
	}
	_ = unblock.Close()
	<-stalledDone
}

// A body over the join layer's limit is refused before any lock is taken.
func TestTxJoin_OversizeBody_413(t *testing.T) {
	j, _, pass := liveJoiner(t, "tx-1")
	req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity", io.LimitReader(zeroes{}, maxJoinedBodySize+1)))
	req.Header.Set(proxy.TxTokenHeader, pass)
	rec := httptest.NewRecorder()
	TxJoin(j)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") })).ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d; want 413", rec.Code)
	}
}

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) { clear(p); return len(p), nil }
```

`internal/domain/workflow/transitions_fence_test.go` — a joined `GetTransitions` that reaches a function criterion holds the lock and gives it up for the callout (today it holds none: the read path never took it, so `txgate.Suspend` was a no-op there):

```go
package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

type lockProbingCriteria struct {
	free       func() bool
	duringFree bool
}

func (p *lockProbingCriteria) DispatchProcessor(_ context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
	return e, nil
}
func (p *lockProbingCriteria) DispatchCriteria(context.Context, *spi.Entity, json.RawMessage, string, string, string, string, string) (bool, string, error) {
	p.duringFree = p.free()
	return true, "", nil
}
func (p *lockProbingCriteria) DispatchFunction(context.Context, *spi.Entity, spi.ScheduleFunction, string, string, string) (contract.FunctionResult, error) {
	return contract.FunctionResult{}, nil
}

func TestJoinedGetTransitions_HoldsTheLock_AndGivesItUpForTheCallout(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	gate := txgate.New()
	f := fence.New(gate)
	signer, _ := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	base := ctxWithTenant(testTenant)
	txID, _, err := txMgr.Begin(base)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	free := func() bool {
		ch := make(chan struct{})
		go func() { gate.Acquire(txID)(); close(ch) }()
		select {
		case <-ch:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	}
	ext := &lockProbingCriteria{free: free}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(ext))

	modelRef := spi.ModelRef{EntityName: "joined-transitions", ModelVersion: "1.0"}
	crit, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": "c", "config": map[string]any{"calculationNodesTags": "t"}}})
	saveWorkflow(t, factory, base, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "WF", InitialState: "INITIAL", Active: true, Criterion: crit,
		States: map[string]spi.StateDefinition{"INITIAL": {Transitions: []spi.TransitionDefinition{{Name: "GO", Next: "DONE", Manual: true}}}, "DONE": {}},
	}})

	_, end := f.Begin(base, "req-1", txID, nil)
	defer end()
	f.Advance("req-1", 1)
	pass, _ := signer.Issue(token.Claims{NodeID: "local", TxRef: txID, ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})

	heldBefore, heldAfter := false, false
	err = txjoin.NewJoiner(signer, txMgr, f, gate).Run(base, pass, func(ctx context.Context) {
		heldBefore = !free()
		entity := makeEntity("jt-1", modelRef, map[string]any{"x": 1})
		entity.Meta.State = "INITIAL"
		if _, err := engine.GetAvailableTransitionsForEntity(ctx, entity); err != nil {
			t.Errorf("GetAvailableTransitionsForEntity: %v", err)
		}
		heldAfter = !free()
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !heldBefore || !heldAfter {
		t.Fatalf("the joined read must hold the lock before (%v) and after (%v) the callout", heldBefore, heldAfter)
	}
	if !ext.duringFree {
		t.Fatal("the lock was kept across the function criterion's callout: a callback of that callout would deadlock on it")
	}
}
```

(`lockProbeBody` mirrors `lockProbeWriter` on `Read`; `liveJoiner` builds `txjoin.NewJoiner(signer, fakeJoinTM{}, f, gate)` and the pass, like `liveFence` of F-6, and replaces it in this file.)

`internal/grpc/txroute_interceptor_test.go`:

```go
// gRPC server-streaming: the handler's sends are held until the lock is
// released, and the request message is received before the lock is taken.
func TestTxRouteInterceptor_StreamSendsAreHeldUntilTheLockIsReleased(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	gate := txgate.New()
	f := fence.New(gate)
	_, end := f.Begin(context.Background(), "req-tx-1", "tx-1", nil)
	t.Cleanup(end)
	f.Advance("req-tx-1", 1)
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-1", Major: 1})
	ic := newTxRouteInterceptor(s, fakeRouteRegistry{}, "local", txjoin.NewJoiner(s, fakeJoinTM{}, f, gate), 9090, true)

	lockFree := func() bool {
		free := make(chan struct{})
		go func() { gate.Acquire("tx-1")(); close(free) }()
		select {
		case <-free:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	}
	ss := newFakeServerStream(metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok)))
	ss.request = &cepb.CloudEvent{Id: "req-1"}
	ss.onRecv = func() {
		if !lockFree() {
			t.Error("the request message was received under the transaction's lock")
		}
	}
	ss.onSend = func() {
		if !lockFree() {
			t.Error("a response frame was sent under the transaction's lock")
		}
	}
	err := ic.stream()(nil, ss, &googlegrpc.StreamServerInfo{FullMethod: cyodapb.CloudEventsService_EntitySearchCollection_FullMethodName},
		func(_ any, stream googlegrpc.ServerStream) error {
			var req cepb.CloudEvent
			if err := stream.RecvMsg(&req); err != nil || req.Id != "req-1" {
				t.Errorf("handler RecvMsg = %v, id %q; want the request replayed", err, req.Id)
			}
			if lockFree() {
				t.Error("the handler ran without the transaction's lock")
			}
			for _, id := range []string{"f-1", "f-2"} {
				if err := stream.SendMsg(&cepb.CloudEvent{Id: id}); err != nil {
					return err
				}
			}
			if len(ss.sent) != 0 {
				t.Error("a frame reached the client before the handler returned")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(ss.sent) != 2 {
		t.Fatalf("sent %d frames; want 2, in order, after the handler returned", len(ss.sent))
	}
}
```

(extend the file's existing fake server stream with `request`, `onRecv`, `onSend`, `sent`.) Also add the unary twin of `TestRun_HandlerHoldsTheLock…` — `TestTxRouteInterceptor_UnaryHandlerRunsUnderTheLock` — same `lockFree` probe inside the unary handler, asserting the lock is held there and free once `ic.unary()` has returned.

- [ ] **Step 2: Run to verify RED**
Run: `go test -race ./internal/domain/txjoin/... -run 'TestRun_'`   Expected: FAIL to build (`undefined: NewJoiner`). To see the defect itself, first write `Run` as `JoinFromToken` + `handler(joined)` with no lock: `TestRun_ParallelJoinedRequestsAreSerialised` then ends in `fatal error: concurrent map writes` / `WARNING: DATA RACE` in `plugins/memory/entity_store.go` — record that output in the commit body.
Run: `go test ./internal/httpmw/... ./internal/grpc/... -run 'TestTxJoin_|TestTxRouteInterceptor_Stream|TestTxRouteInterceptor_Unary'`   Expected: FAIL to build (`cannot use j (… *txjoin.Joiner) as *token.Signer`).

- [ ] **Step 3: Implement**

`internal/domain/txjoin/txjoin.go`:

```go
// Joiner is what a callback door needs to run a request as a joined request of
// the transaction its pass names.
type Joiner struct {
	signer *token.Signer
	txMgr  spi.TransactionManager
	fence  *fence.Fence
	gate   *txgate.Registry
}

func NewJoiner(signer *token.Signer, txMgr spi.TransactionManager, f *fence.Fence, gate *txgate.Registry) *Joiner {
	return &Joiner{signer: signer, txMgr: txMgr, fence: f, gate: gate}
}

// Run runs handler as a joined request. The storage contract leaves it to the
// application to serialise its own concurrent operations on one transaction, so
// EVERY joined request — a read as much as a write, an entity request or not —
// holds the transaction's lock from before its first store operation until its
// handler returns: during a callout the transaction's users are its current
// compute node's callbacks, one at a time.
//
// Order: verify → Join (tenant) → Admit → take the lock → Check under the lock
// → handler → release. The check under the lock is the one that gives the right
// to touch the transaction: the owner's wait takes the same lock, so a request
// either passed this check before the number rose — and the owner waits for it
// — or is refused here. The engine gives the lock up for the length of a
// callout of the callback's own through the handle installed here
// (txgate.Suspend).
//
// A non-nil error is a refusal: handler did not run. The caller sends its
// response only after Run has returned, so that a compute node that does not
// read its response holds nothing.
//
// An empty tok is not a joined request: handler runs on ctx as it is.
func (j *Joiner) Run(ctx context.Context, tok string, handler func(ctx context.Context)) error {
	if tok == "" {
		handler(ctx)
		return nil
	}
	joined, err := JoinFromToken(ctx, j.signer, j.txMgr, j.fence, tok)
	if err != nil {
		return err
	}
	txID := spi.GetTransaction(joined).ID

	release := j.gate.Acquire(txID)
	// The closure reads `release` when it runs: a Suspend/resume in between
	// stores the re-acquired lock's release through the pointer below.
	defer func() { release() }()
	joined, _ = txgate.WithHeld(joined, j.gate, txID, &release)

	if err := fence.Check(joined); err != nil {
		return err
	}
	handler(joined)
	return nil
}
```

`internal/httpmw/buffered_writer.go`:

```go
package httpmw

import (
	"bytes"
	"net/http"
)

// bufferedWriter holds a handler's response until flushTo. A joined request's
// handler runs under its transaction's lock; writing to the client there would
// make the lock wait on the client.
type bufferedWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferedWriter() *bufferedWriter { return &bufferedWriter{header: make(http.Header)} }

func (b *bufferedWriter) Header() http.Header { return b.header }

func (b *bufferedWriter) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

// flushTo sends what the handler wrote. A write error means the client has
// gone; there is nobody left to tell.
func (b *bufferedWriter) flushTo(w http.ResponseWriter) {
	for k, v := range b.header {
		w.Header()[k] = v
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}
```

`internal/httpmw/txjoin_mw.go`:

```go
func TxJoin(j *txjoin.Joiner) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := r.Header.Get(proxy.TxTokenHeader)
			if tok == "" {
				next.ServeHTTP(w, r)
				return
			}
			// Read the body before the lock is taken: what the lock waits on
			// must never be the client. The handler's own MaxBytesReader still
			// applies to what it reads from the buffer.
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJoinedBodySize))
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					common.WriteError(w, r, common.Operational(http.StatusRequestEntityTooLarge, common.ErrCodeBadRequest,
						fmt.Sprintf("request body exceeds %d bytes", maxJoinedBodySize)))
					return
				}
				common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read request body"))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			buffered := newBufferedWriter()
			err = j.Run(r.Context(), tok, func(ctx context.Context) {
				next.ServeHTTP(buffered, r.WithContext(ctx))
			})
			if err != nil {
				var appErr *common.AppError
				if !errors.As(err, &appErr) {
					appErr = common.Internal("failed to join transaction", err)
				}
				common.WriteError(w, r, appErr)
				return
			}
			buffered.flushTo(w)
		})
	}
}

// maxJoinedBodySize is the largest body any handler behind this middleware
// accepts: entity, grouped-stats, search, workflow, model and message handlers
// cap their own reads at 10 MiB or below, and still do, on the buffered bytes.
// The join layer must never refuse a body a handler would take.
const maxJoinedBodySize = 10 * 1024 * 1024
```

Help — `cmd/cyoda/help/content/grpc.md`, section `## COMPUTE MEMBER PROTOCOL` (:193), where the transaction token on a request is described (F-10 adds the `CALLOUT_SUPERSEDED` paragraph beside it):

```markdown
An API request made under a transaction token is not cancelled when its client
goes away: once admitted it runs to completion on the node that holds the
transaction. If the connection drops, or the node the request arrived at
answers `503` because forwarding it took longer than `CYODA_PROXY_TIMEOUT`, the
outcome of a write is **unknown** — it may have been applied to the
transaction. Do not assume it failed. A deadline the compute node sets on its
own gRPC call does not stop the request on the server either.
```

`internal/grpc/txroute_interceptor.go` — unary:

```go
		var resp any
		var herr error
		if jerr := i.joiner.Run(ctx, tok, func(joined context.Context) { resp, herr = handler(joined, req) }); jerr != nil {
			return i.unaryErr(ctx, ce, envelope, jerr)
		}
		return resp, herr
```

stream:

```go
		if tok == "" {
			return handler(srv, ss)
		}
		// Receive the request before the lock is taken (see heldStream).
		var first cepb.CloudEvent
		if err := ss.RecvMsg(&first); err != nil {
			return err
		}
		held := &heldStream{ServerStream: ss, first: &first}
		var herr error
		if jerr := i.joiner.Run(ctx, tok, func(joined context.Context) {
			held.ctx = joined
			herr = handler(srv, held)
		}); jerr != nil {
			return i.streamErr(ss, first.Id, envelope, jerr)
		}
		if herr != nil {
			return herr
		}
		return held.flush()
```

```go
// heldStream is the stream a joined server-streaming handler sees. The handler
// runs under its transaction's lock, and neither end of the stream may make
// that lock wait on the compute node: the request message was received before
// the lock was taken and is replayed here, and every response frame is held
// until the handler has returned and the lock is released.
type heldStream struct {
	googlegrpc.ServerStream
	ctx   context.Context
	first *cepb.CloudEvent
	held  []any
}

func (s *heldStream) Context() context.Context { return s.ctx }

func (s *heldStream) RecvMsg(m any) error {
	if s.first == nil {
		return s.ServerStream.RecvMsg(m) // io.EOF for a server-streaming RPC
	}
	dst, ok := m.(proto.Message)
	if !ok {
		return fmt.Errorf("failed to replay request: %T is not a proto message", m)
	}
	proto.Merge(dst, s.first)
	s.first = nil
	return nil
}

func (s *heldStream) SendMsg(m any) error {
	s.held = append(s.held, m)
	return nil
}

func (s *heldStream) flush() error {
	for _, m := range s.held {
		if err := s.ServerStream.SendMsg(m); err != nil {
			return err
		}
	}
	return nil
}
```

A handler error after frames were held drops the held frames and returns the error — what the client of a failed stream relies on is the status, as today. `app/app.go`: `a.joiner = txjoin.NewJoiner(a.tokenSigner, a.transactionManager, a.fence, a.txGate)`, built where the handlers are (:644); `:712` `httpmw.TxJoin(a.joiner)`; `:827` passes `a.joiner` in place of `a.fence`.

Entity service — each of the nine sites, before / after:

```go
// before
	txID, txCtx, owned := scope.TxID(), scope.Ctx(), scope.Owned()
	if !owned {
		var releaseGate func()
		txCtx, releaseGate = h.acquireJoinedGate(txCtx, txID)
		defer releaseGate()
	}
// after
	txID, txCtx, owned := scope.TxID(), scope.Ctx(), scope.Owned()
```

(where `owned` is then unused in a function — `deleteOneBatch`, check the compiler — drop it from the assignment.) The owner's finalize blocks (`if owned { defer h.gate.Acquire(finalTxID)() }`) and `txScope.Release` are unchanged: they are the owner's side of the same lock.

- [ ] **Step 4: Run to verify GREEN** — `go test -race ./internal/domain/txjoin/...`; `go test ./internal/domain/entity/... ./internal/httpmw/... ./internal/grpc/... ./app/... ./internal/txgate/...`; `go test ./internal/e2e/... -run 'TestCallback'` (depth-2 callbacks, `internal/e2e/callback_depth2_test.go`, are the proof that Suspend/resume still works through the handle the join layer installs). Exit checks above.

- [ ] **Step 5: Commit**
`git add internal/domain/txjoin internal/httpmw internal/grpc internal/domain/entity app/app.go && git commit -m "fix(txjoin): one transaction, one user at a time — every joined request holds the lock; responses after release (#254)"`

---

### Task F-8: the engine — a check after every re-taking of the lock and after every processor; a refused chain touches no store

**Spec:** §7 point 2 (second and third paragraphs), point 3; §13 rows "A callback waiting on a callout of its own…" and "The same in `ASYNC_NEW_TX`…".

**Files:**
- Modify: `internal/domain/workflow/engine_processors.go` — `executeProcessors` (:136-186), `executeSyncProcessor` (:202-207), `executeAsyncNewTx` (both `resume()` sites, :233-236 and :249-260)
- Modify: `internal/domain/workflow/engine.go` — `evaluateCriterion` (:1014-1017), `recordEvent` (:1240)
- Modify: `internal/domain/workflow/arm.go` — `armViaFunction` (:237-242)
- Test: `internal/domain/workflow/engine_fence_test.go` (new), `internal/fence/check_call_sites_test.go` (new)

**Interfaces:**
- Consumes: `fence.Check`, `fence.Pairs`, `fence.ErrSuperseded`, `fence.NewSupersededError` (F-2/F-3); `txgate.WithHeld`, `txgate.Suspend`.
- Produces: nothing new. How the refusal travels: it is an `*common.AppError`; the engine's wraps are all `%w` (`processor %s failed: %w` :185, `failed to evaluate transition criterion: %w` engine.go:803/922, `failed to evaluate workflow criterion for %q: %w` :600; `armViaFunction` returns it bare), so `classifyWorkflowError`'s first branch (`errors.As`, `entity/service.go:2849`) returns it **unchanged**: 410, `CALLOUT_SUPERSEDED`, not retryable. `UpdateEntityCollection`'s per-item isolation (`service.go:2641`) keys on `spi.ErrConflict` and does not catch it. No classifier change.

**Every store operation an error unwinding from these points could still trigger in a joined chain, and what stops it:**

| Operation | Where | Stopped by |
|---|---|---|
| processor-result audit row | `engine_processors.go:177` | the after-switch check returns before it; and the `recordEvent` guard |
| "Processor failed" audit row | `engine.go:832` (`fireTransition`), `:955` (`cascadeAutomated`) | the `recordEvent` guard |
| savepoint undo / release | `engine_processors.go:254, 262` | the check directly after `resume()` returns before either |
| `applyProcessorData` (model read, possible model extension) | `engine_processors.go:210` | the check directly after `resume()` |
| criterion no-match audit row | `engine.go:810, 928`, `selectWorkflow :622` | `evaluateCriterion` returns the refusal as an error, so the no-match branch is not reached |
| `ReconcileForEntity` and the arm/cancel audit rows | `arm.go:167-200` | `armViaFunction` returns the refusal; `reconcileScheduledTasks` returns at :135 |
| `emitTransitionAborted` | `entity/service.go:2334, 2734` | not reachable from a refusal: it runs only on `spi.ErrConflict` from the final `CompareAndSave`, which a refused chain never reaches |
| the final `Save` / `CompareAndSave` | `entity/service.go` finalize blocks | every engine entry point returns the error, and the service returns before the finalize block |
| `rollbackSegment` | `engine.go:265`, the entry-point guards | a no-op unless the chain segmented (`openTxID != entryTxID`), which a joined chain does only through a `COMMIT_BEFORE_DISPATCH` processor — the separately filed defect; it then rolls back a segment the chain itself opened, not the owner's transaction. Left as it is; see Open point 4 |
| `txScope.Release` | `entity/txscope.go:120` | returns early for a joined entry transaction (:127) |

- [ ] **Step 1: Write the failing tests**

`internal/domain/workflow/engine_fence_test.go` — memory backend; the test plays the owner through the fence, the scripted `ExternalProcessingService` plays the inner Coordinator:

```go
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// innerCoordinator is what a Coordinator does as far as the fence is concerned:
// begin under the callback's pairs, advance, wait for the cnode — which never
// answers — until released, end.
type innerCoordinator struct {
	f       *fence.Fence
	entered chan struct{} // closed when the first callout is waiting
	proceed chan struct{} // the released callout returns only once this is closed
	n       atomic.Int32
}

func (c *innerCoordinator) run(ctx context.Context, txID string) error {
	id := "inner-" + string(rune('0'+c.n.Add(1)))
	cctx, end := c.f.Begin(ctx, id, txID, fence.Pairs(ctx))
	defer end()
	c.f.Advance(id, 1)
	if c.n.Load() == 1 {
		close(c.entered)
	}
	<-cctx.Done()
	<-c.proceed
	if errors.Is(context.Cause(cctx), fence.ErrSuperseded) {
		return fence.NewSupersededError()
	}
	return cctx.Err()
}

func (c *innerCoordinator) DispatchProcessor(ctx context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, txID string) (*spi.Entity, error) {
	return e, c.run(ctx, txID)
}
func (c *innerCoordinator) DispatchCriteria(ctx context.Context, _ *spi.Entity, _ json.RawMessage, _, _, _, _, txID string) (bool, string, error) {
	return true, "", c.run(ctx, txID)
}
func (c *innerCoordinator) DispatchFunction(ctx context.Context, _ *spi.Entity, _ spi.ScheduleFunction, _, _, txID string) (contract.FunctionResult, error) {
	return contract.FunctionResult{}, c.run(ctx, txID)
}

// storeOpsAfterRefusal wraps a factory and a transaction manager and counts
// every audit write, entity store operation, scheduled-task reconcile and
// savepoint operation made on a context the fence refuses.
type refusedOps struct{ n atomic.Int32 }

func (r *refusedOps) note(ctx context.Context) {
	if fence.Check(ctx) != nil {
		r.n.Add(1)
	}
}

type watchedFactory struct {
	spi.StoreFactory
	ops *refusedOps
}

func (w watchedFactory) StateMachineAuditStore(ctx context.Context) (spi.StateMachineAuditStore, error) {
	s, err := w.StoreFactory.StateMachineAuditStore(ctx)
	return watchedAudit{s, w.ops}, err
}
func (w watchedFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	w.ops.note(ctx)
	return w.StoreFactory.EntityStore(ctx)
}
func (w watchedFactory) ModelStore(ctx context.Context) (spi.ModelStore, error) {
	w.ops.note(ctx)
	return w.StoreFactory.ModelStore(ctx)
}
func (w watchedFactory) ScheduledTaskStore(ctx context.Context) (spi.ScheduledTaskStore, error) {
	w.ops.note(ctx)
	return w.StoreFactory.ScheduledTaskStore(ctx)
}

type watchedAudit struct {
	spi.StateMachineAuditStore
	ops *refusedOps
}

func (w watchedAudit) Record(ctx context.Context, id string, ev spi.StateMachineEvent) error {
	w.ops.note(ctx)
	return w.StateMachineAuditStore.Record(ctx, id, ev)
}

type watchedTxMgr struct {
	spi.TransactionManager
	ops                *refusedOps
	undone, released   atomic.Int32
}

func (w *watchedTxMgr) Savepoint(ctx context.Context, txID string) (string, error) {
	w.ops.note(ctx)
	return w.TransactionManager.Savepoint(ctx, txID)
}
func (w *watchedTxMgr) RollbackToSavepoint(ctx context.Context, txID, sp string) error {
	w.ops.note(ctx)
	w.undone.Add(1)
	return w.TransactionManager.RollbackToSavepoint(ctx, txID, sp)
}
func (w *watchedTxMgr) ReleaseSavepoint(ctx context.Context, txID, sp string) error {
	w.ops.note(ctx)
	w.released.Add(1)
	return w.TransactionManager.ReleaseSavepoint(ctx, txID, sp)
}

type supersedeEnv struct {
	base    context.Context
	factory spi.StoreFactory
	txMgr   *watchedTxMgr
	gate    *txgate.Registry
	f       *fence.Fence
	ext     *innerCoordinator
	engine  *Engine
	ops     *refusedOps
	txID    string
	txCtx   context.Context
}

func newSupersedeEnv(t *testing.T) *supersedeEnv {
	t.Helper()
	mem := memory.NewStoreFactory()
	t.Cleanup(func() { mem.Close() })
	uuids := common.NewTestUUIDGenerator()
	ops := &refusedOps{}
	txMgr := &watchedTxMgr{TransactionManager: mem.NewTransactionManager(uuids), ops: ops}
	gate := txgate.New()
	f := fence.New(gate)
	ext := &innerCoordinator{f: f, entered: make(chan struct{}), proceed: make(chan struct{})}
	factory := watchedFactory{StoreFactory: mem, ops: ops}
	env := &supersedeEnv{base: ctxWithTenant(testTenant), factory: factory, txMgr: txMgr, gate: gate, f: f, ext: ext, ops: ops,
		engine: NewEngine(factory, uuids, txMgr, WithExternalProcessing(ext))}
	var err error
	env.txID, env.txCtx, err = txMgr.Begin(env.base)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return env
}

// callback runs fn as the join layer runs a callback of callout "outer" at
// major: joined, admitted, under the lock with the suspendable handle, checked.
func (env *supersedeEnv) callback(t *testing.T, major uint32, fn func(ctx context.Context) error) error {
	t.Helper()
	joined, err := env.txMgr.Join(env.base, env.txID)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	admitted, err := env.f.Admit(joined, []fence.Pair{{Callout: "outer", Major: major}})
	if err != nil {
		return err
	}
	release := env.gate.Acquire(env.txID)
	defer func() { release() }()
	admitted, _ = txgate.WithHeld(admitted, env.gate, env.txID, &release)
	if err := fence.Check(admitted); err != nil {
		return err
	}
	return fn(admitted)
}

func assertSuperseded(t *testing.T, err error) {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Status != 410 || appErr.Code != common.ErrCodeCalloutSuperseded {
		t.Fatalf("err = %v; want 410 CALLOUT_SUPERSEDED (the callback must be answered 410, not 200)", err)
	}
	if !errors.Is(err, fence.ErrSuperseded) {
		t.Fatalf("the engine's wraps must keep the refusal findable: %v", err)
	}
}

// A callback waiting on a callout of its own when its cnode is replaced: it is
// released, the inner callout ends, the chain is refused on re-taking the lock,
// and nothing further touches a store — in every shape the engine can be
// waiting in.
func TestSupersededChain_TouchesNoStore(t *testing.T) {
	fnCriterion, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": "crit", "config": map[string]any{"calculationNodesTags": "t"}}})
	proc := func(mode string) []spi.ProcessorDefinition {
		return []spi.ProcessorDefinition{{Type: ProcessorTypeExternalized, Name: "p", ExecutionMode: mode}}
	}
	tests := []struct {
		name       string
		transition spi.TransitionDefinition
		wfCrit     json.RawMessage
	}{
		{name: "SYNC processor", transition: spi.TransitionDefinition{Name: "T", Next: "DONE", Processors: proc(ExecutionModeSync)}},
		{name: "ASYNC_SAME_TX processor", transition: spi.TransitionDefinition{Name: "T", Next: "DONE", Processors: proc(ExecutionModeAsyncSameTx)}},
		{name: "ASYNC_NEW_TX as the LAST processor", transition: spi.TransitionDefinition{Name: "T", Next: "DONE", Processors: proc(ExecutionModeAsyncNewTx)}},
		{name: "transition FUNCTION criterion", transition: spi.TransitionDefinition{Name: "T", Next: "DONE", Criterion: fnCriterion}},
		{name: "workflow FUNCTION criterion", transition: spi.TransitionDefinition{Name: "T", Next: "DONE"}, wfCrit: fnCriterion},
		{name: "arming function", transition: spi.TransitionDefinition{Name: "T", Next: "DONE", Schedule: &spi.TransitionSchedule{Function: &spi.ScheduleFunction{Name: "when", CalculationNodesTags: "t"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newSupersedeEnv(t)
			modelRef := spi.ModelRef{EntityName: "superseded", ModelVersion: "1.0"}
			saveWorkflow(t, env.factory, env.base, modelRef, []spi.WorkflowDefinition{{
				Version: "1.1", Name: "WF", InitialState: "INITIAL", Active: true, Criterion: tc.wfCrit,
				States: map[string]spi.StateDefinition{"INITIAL": {Transitions: []spi.TransitionDefinition{tc.transition}}, "DONE": {}},
			}})

			_, endOuter := env.f.Begin(env.base, "outer", env.txID, nil)
			defer endOuter()
			env.f.Advance("outer", 1)

			var state string
			done := make(chan error, 1)
			go func() {
				done <- env.callback(t, 1, func(ctx context.Context) error {
					entity := makeEntity("child-1", modelRef, map[string]any{"x": 1})
					entity.Meta.TransactionID = env.txID
					_, err := env.engine.Execute(ctx, entity, "")
					state = entity.Meta.State
					return err
				})
			}()

			<-env.ext.entered
			env.f.Advance("outer", 2) // the owner gives the work to another cnode
			before := env.ops.n.Load()
			close(env.ext.proceed)

			select {
			case err := <-done:
				assertSuperseded(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("the callback was not released")
			}
			if state == "DONE" {
				t.Fatal("the superseded chain carried the entity on to the next state")
			}
			if got := env.ops.n.Load() - before; got != 0 {
				t.Fatalf("the refused chain made %d store operations (audit rows included); want none", got)
			}
			if env.ext.n.Load() != 1 {
				t.Fatalf("%d callouts were made; the chain must stop at the first", env.ext.n.Load())
			}
		})
	}
}

// In ASYNC_NEW_TX the superseded chain neither undoes nor releases its
// savepoint: what the replacement cnode wrote meanwhile is kept.
func TestSupersededChain_AsyncNewTx_LeavesItsSavepointAlone(t *testing.T) {
	env := newSupersedeEnv(t)
	modelRef := spi.ModelRef{EntityName: "superseded-sp", ModelVersion: "1.0"}
	saveWorkflow(t, env.factory, env.base, modelRef, []spi.WorkflowDefinition{{
		Version: "1.1", Name: "WF", InitialState: "INITIAL", Active: true,
		States: map[string]spi.StateDefinition{
			"INITIAL": {Transitions: []spi.TransitionDefinition{{Name: "T", Next: "DONE", Processors: []spi.ProcessorDefinition{
				{Type: ProcessorTypeExternalized, Name: "p1", ExecutionMode: ExecutionModeAsyncNewTx},
				{Type: ProcessorTypeExternalized, Name: "p2", ExecutionMode: ExecutionModeAsyncNewTx}, // must never be dispatched
			}}}},
			"DONE": {},
		},
	}})
	_, endOuter := env.f.Begin(env.base, "outer", env.txID, nil)
	defer endOuter()
	env.f.Advance("outer", 1)

	done := make(chan error, 1)
	go func() {
		done <- env.callback(t, 1, func(ctx context.Context) error {
			entity := makeEntity("child-sp", modelRef, map[string]any{"x": 1})
			entity.Meta.TransactionID = env.txID
			_, err := env.engine.Execute(ctx, entity, "")
			return err
		})
	}()
	<-env.ext.entered
	env.f.Advance("outer", 2)

	// The replacement cnode writes while the superseded chain is still waiting.
	const keptID = "replacement-write"
	if err := env.callback(t, 2, func(ctx context.Context) error {
		es, err := env.factory.EntityStore(ctx)
		if err != nil {
			return err
		}
		kept := makeEntity(keptID, modelRef, map[string]any{"kept": true})
		kept.Meta.TransactionID = env.txID
		_, err = es.Save(ctx, kept)
		return err
	}); err != nil {
		t.Fatalf("replacement write: %v", err)
	}

	close(env.ext.proceed)
	assertSuperseded(t, <-done)
	if u, r := env.txMgr.undone.Load(), env.txMgr.released.Load(); u != 0 || r != 0 {
		t.Fatalf("savepoint undone %d times, released %d times; a refused chain touches neither", u, r)
	}
	if err := env.txMgr.Commit(env.txCtx, env.txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	es, _ := env.factory.EntityStore(env.base)
	if _, err := es.Get(env.base, keptID); err != nil {
		t.Fatalf("the replacement cnode's write was lost: %v", err)
	}
}
```

(`ExecutionModeSync` / `ExecutionModeAsyncSameTx`: use the package's constants as named in `internal/domain/workflow`; if only the string forms exist for those two, write `"SYNC"` / `"ASYNC_SAME_TX"` as the neighbouring tests do. `spi.TransitionSchedule` is the type of `TransitionDefinition.Schedule` — take the name from the SPI.)

`internal/fence/check_call_sites_test.go` — a structural guard, modelled on `internal/txgate/suspend_call_sites_test.go`: every explicit `resume()` statement in non-test code must be followed by a statement that calls `fence.Check`, so a sixth Suspend site cannot be added without its check.

```go
package fence

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// uncheckedResumes reports every statement `resume()` in src whose next
// statement does not call fence.Check.
func uncheckedResumes(filename string, src any) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	callsCheck := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(c ast.Node) bool {
			if call, ok := c.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Check" {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "fence" {
						found = true
					}
				}
			}
			return !found
		})
		return found
	}
	isResume := func(s ast.Stmt) bool {
		es, ok := s.(*ast.ExprStmt)
		if !ok {
			return false
		}
		call, ok := es.X.(*ast.CallExpr)
		if !ok || len(call.Args) != 0 {
			return false
		}
		id, ok := call.Fun.(*ast.Ident)
		return ok && id.Name == "resume"
	}
	var offenders []string
	ast.Inspect(file, func(n ast.Node) bool {
		var list []ast.Stmt
		switch s := n.(type) {
		case *ast.BlockStmt:
			list = s.List
		case *ast.CaseClause:
			list = s.Body
		case *ast.CommClause:
			list = s.Body
		default:
			return true
		}
		for i, stmt := range list {
			if !isResume(stmt) {
				continue
			}
			if i+1 >= len(list) || !callsCheck(list[i+1]) {
				p := fset.Position(stmt.Pos())
				offenders = append(offenders, filename+":"+strconv.Itoa(p.Line))
			}
		}
		return true
	})
	return offenders, nil
}

func TestEveryResumeIsFollowedByACheck(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	var offenders []string
	sites := 0
	err = filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		if strings.Contains(path, string(filepath.Separator)+"txgate"+string(filepath.Separator)) {
			return nil // resume's own definition
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sites += strings.Count(string(src), "txgate.Suspend(")
		found, scanErr := uncheckedResumes(path, src)
		offenders = append(offenders, found...)
		return scanErr
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if sites == 0 {
		t.Fatal("no txgate.Suspend site found: the scan looked in the wrong place")
	}
	if len(offenders) > 0 {
		t.Fatalf("after a callout of its own a joined chain re-takes the transaction's lock; the statement after resume() must be the fence's check:\n%s", strings.Join(offenders, "\n"))
	}
}

func TestUncheckedResumes_Discriminates(t *testing.T) {
	bad := "package p\nfunc f() { resume := s(); defer resume(); d(); resume(); use() }"
	good := "package p\nfunc f() error { resume := s(); defer resume(); d(); resume(); if err := fence.Check(ctx); err != nil { return err }; return nil }"
	if got, _ := uncheckedResumes("bad.go", bad); len(got) != 1 {
		t.Fatalf("bad: %v", got)
	}
	if got, _ := uncheckedResumes("good.go", good); len(got) != 0 {
		t.Fatalf("good: %v", got)
	}
}
```

The scan also requires an explicit `resume()` at every site: add to the test, after the walk, `explicit == sites` where `explicit` counts `isResume` statements — `engine.go:1014` has only the deferred one today and fails it.

- [ ] **Step 2: Run to verify RED**
Run: `go test ./internal/domain/workflow/... ./internal/fence/... -run 'TestSupersededChain|TestEveryResumeIsFollowedByACheck'`
Expected: FAIL — `TestSupersededChain_TouchesNoStore`: `the refused chain made N store operations` for the processor modes, `err = <nil>; want 410` for `ASYNC_NEW_TX as the LAST processor` (the refusal is swallowed and the entity reaches DONE); `…LeavesItsSavepointAlone`: `savepoint undone 1 times`; the call-site scan lists the five sites.

- [ ] **Step 3: Implement**

`executeSyncProcessor`:

```go
	modifiedEntity, err := e.extProc.DispatchProcessor(ctx, entity, proc, workflow, transition, txID)
	resume()
	if cerr := fence.Check(ctx); cerr != nil {
		return cerr
	}
	if err != nil {
		return err
	}
```

`executeAsyncNewTx` (the `e.txMgr == nil` branch gets the same two lines after its `resume()`; F-9 deletes that branch):

```go
	_, dispatchErr := e.extProc.DispatchProcessor(ctx, entity, proc, workflow, transition, txID)
	resume()
	// Before the savepoint is looked at. A chain that was superseded neither
	// undoes nor releases its savepoint: by now the replacement compute node may
	// have written, and undoing would take those writes with it. An abandoned
	// savepoint is harmless on every backend.
	if cerr := fence.Check(ctx); cerr != nil {
		return cerr
	}
	if dispatchErr != nil {
```

`evaluateCriterion` — the explicit `resume()` the other four sites have:

```go
		resume := txgate.Suspend(cc.ctx)
		defer resume()
		matches, reason, err := e.extProc.DispatchCriteria(cc.ctx, entity, criterion, cc.target, cc.workflowName, cc.transitionName, "", cc.txID)
		resume()
		if cerr := fence.Check(cc.ctx); cerr != nil {
			return false, "", cerr
		}
		return matches, capReason(reason), err
```

`armViaFunction`:

```go
	res, derr := e.extProc.DispatchFunction(ctx, entity, *tr.Schedule.Function, wf.Name, tr.Name, txID)
	resume() // BEFORE any tx-buffer write — see doc comment above.
	if cerr := fence.Check(ctx); cerr != nil {
		return nil, nil, cerr
	}
	if derr != nil {
```

`executeProcessors` — before / after of :136-186:

```go
// before
		case ExecutionModeAsyncNewTx:
			procErr = e.executeAsyncNewTx(currentCtx, entity, proc, workflow, transition, currentTxID)
			success = procErr == nil

			// ASYNC_NEW_TX failures are non-fatal: log warning, continue pipeline.
			if procErr != nil {
				slog.Warn("ASYNC_NEW_TX processor failed, continuing pipeline",
					"pkg", "workflow", "processor", proc.Name, "error", procErr)
			}
		...
		}

		auditData := map[string]any{
```

```go
// after
		case ExecutionModeAsyncNewTx:
			procErr = e.executeAsyncNewTx(currentCtx, entity, proc, workflow, transition, currentTxID)
			success = procErr == nil
		...
		}

		// Directly after the mode switch, before the processor's audit event
		// and before any mode decides what its result means. ASYNC_NEW_TX's
		// "log and continue" would otherwise swallow the refusal and carry a
		// superseded chain on to the next processor, and past the last one to
		// the handler's final save. A check at the top of the loop alone would
		// never see the last processor.
		if cerr := fence.Check(currentCtx); cerr != nil {
			return currentCtx, currentTxID, cerr
		}

		// ASYNC_NEW_TX failures are non-fatal: log warning, continue pipeline.
		if procErr != nil && proc.ExecutionMode == ExecutionModeAsyncNewTx {
			slog.Warn("ASYNC_NEW_TX processor failed, continuing pipeline",
				"pkg", "workflow", "processor", proc.Name, "error", procErr)
		}

		auditData := map[string]any{
```

`recordEvent` — the one guard, first statement:

```go
	// A chain the fence refuses performs no store operation of any kind, an
	// audit row included: on PostgreSQL it would be a statement on the
	// operation's connection, after the owner has stopped waiting for this
	// chain. The guard is here, and not at the error paths that record, so that
	// no further site can appear later.
	if fence.Check(ctx) != nil {
		return
	}
```

- [ ] **Step 4: Run to verify GREEN** — `go test ./internal/domain/workflow/... ./internal/fence/... ./internal/txgate/... ./internal/domain/entity/...`

- [ ] **Step 5: Commit**
`git add internal/domain/workflow internal/fence/check_call_sites_test.go && git commit -m "feat(workflow): a superseded chain is refused after every callout and touches no store (#254)"`

---

### Task F-9: a savepoint that cannot be created, undone or released fails the operation

**Spec:** §7 "A savepoint that cannot be created, undone or released fails the operation"; §8.1 `ASYNC_NEW_TX` row; §8.2 savepoint row.

**What was found** (the brief asks): no `TransactionManager` has a "savepoints unsupported" case. Memory (`plugins/memory/txmanager.go:962, 1058, 1145`), SQLite (`plugins/sqlite/txmanager.go:1011, 1091, 1170`) and PostgreSQL (`plugins/postgres/transaction_manager.go:756, 783, 818`) all implement the three methods; each fails only for a transaction that is gone, a tenant mismatch, a missing savepoint id, or (PostgreSQL) the statement itself. `internal/observability/tx_tracing.go:112-137` delegates. `e.txMgr == nil` is reached by exactly one construction in the tree — `NewEngine(factory, uuids, nil)` in `TestAsyncNewTx_SeesSyncChanges` (`engine_test.go:1852`); `app/app.go` always passes a manager.

**Existing tests that pin today's behaviour, and the decision for each:**
- `TestProcessorDispatchWithExtProcAsyncNewTx` (`engine_test.go:936`) — runs `Execute` with no transaction begun, so `Savepoint` fails with `ErrTxNotFound`, and asserts the entity still reaches `DONE` with **zero** dispatches. It pins the defect. **Rewritten**: begin a transaction, stamp `entity.Meta.TransactionID`, assert one dispatch and `DONE`.
- `TestAsyncNewTxFailureDoesNotKillPipeline` (:1627) and `TestAsyncNewTxEntityMutationsDiscarded` (:1697) — same missing `Begin`; today they pass without ever dispatching (vacuous). **Rewritten** to begin a transaction (same three lines); their assertions stay and become real.
- `TestAsyncNewTx_SeesSyncChanges` (:1845) — **rewritten** with a real manager and a begun transaction; the `nil` construction goes with the branch.
- `TestProcessorAsyncNewTxIndependent` (:728), `TestNilExtProcProcessorNoOp` (:1140) — no `extProc`, return before the savepoint: **stay**.
- Any other test in `internal/domain/workflow` or `internal/domain/entity` that goes red in Step 4 with `savepoint` in its error ran `ASYNC_NEW_TX` outside a transaction: same three-line fix, nothing else.

**Files:**
- Modify: `internal/domain/workflow/engine_processors.go` (`executeAsyncNewTx`, `executeProcessors`; new `ErrSavepointInfra`), `internal/domain/entity/service.go` (`classifyWorkflowError` + its doc comment), `plugins/postgres/transaction_manager.go` (:770-771, :805-806, :836-837)
- Test: `internal/domain/workflow/engine_savepoint_test.go` (new), `internal/domain/workflow/engine_test.go` (the four rewrites), `internal/domain/entity/service_classify_test.go`, `plugins/postgres/` savepoint test beside the existing savepoint tests (`grep -ln 'RollbackToSavepoint' plugins/postgres/*_test.go`)

**Interfaces:**
- Produces: `workflow.ErrSavepointInfra` (sentinel, `errors.Join`ed with the cause like `ErrScheduledTaskInfra`, `arm.go:63`).

- [ ] **Step 1: Write the failing tests**

`internal/domain/workflow/engine_savepoint_test.go`:

```go
package workflow

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// failingSavepoints fails one of the three savepoint operations.
type failingSavepoints struct {
	spi.TransactionManager
	failCreate, failUndo, failRelease error
}

func (m failingSavepoints) Savepoint(ctx context.Context, txID string) (string, error) {
	if m.failCreate != nil {
		return "", m.failCreate
	}
	return m.TransactionManager.Savepoint(ctx, txID)
}
func (m failingSavepoints) RollbackToSavepoint(ctx context.Context, txID, sp string) error {
	if m.failUndo != nil {
		return m.failUndo
	}
	return m.TransactionManager.RollbackToSavepoint(ctx, txID, sp)
}
func (m failingSavepoints) ReleaseSavepoint(ctx context.Context, txID, sp string) error {
	if m.failRelease != nil {
		return m.failRelease
	}
	return m.TransactionManager.ReleaseSavepoint(ctx, txID, sp)
}

func TestAsyncNewTx_SavepointFailureFailsTheOperation(t *testing.T) {
	boom := errors.New(`ERROR: current transaction is aborted (SQLSTATE 25P02) host=db-1`)
	tests := []struct {
		name          string
		mgr           failingSavepoints
		processorErr  error
		wantDispatch  int
	}{
		{name: "cannot be created", mgr: failingSavepoints{failCreate: boom}, wantDispatch: 0},
		{name: "cannot be undone", mgr: failingSavepoints{failUndo: boom}, processorErr: errors.New("cnode failed"), wantDispatch: 1},
		{name: "cannot be released", mgr: failingSavepoints{failRelease: boom}, wantDispatch: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			factory := memory.NewStoreFactory()
			t.Cleanup(func() { factory.Close() })
			uuids := common.NewTestUUIDGenerator()
			tc.mgr.TransactionManager = factory.NewTransactionManager(uuids)
			dispatched := 0
			ext := &mockExternalProcessing{dispatchFunc: func(_ context.Context, e *spi.Entity, p spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
				dispatched++
				if p.Name == "first" {
					return e, tc.processorErr
				}
				return e, nil
			}}
			engine := NewEngine(factory, uuids, tc.mgr, WithExternalProcessing(ext))
			base := ctxWithTenant(testTenant)
			modelRef := spi.ModelRef{EntityName: "sp-fatal", ModelVersion: "1.0"}
			saveWorkflow(t, factory, base, modelRef, []spi.WorkflowDefinition{{
				Version: "1.1", Name: "WF", InitialState: "INITIAL", Active: true,
				States: map[string]spi.StateDefinition{
					"INITIAL": {Transitions: []spi.TransitionDefinition{{Name: "T", Next: "DONE", Processors: []spi.ProcessorDefinition{
						{Type: ProcessorTypeExternalized, Name: "first", ExecutionMode: ExecutionModeAsyncNewTx},
						{Type: ProcessorTypeExternalized, Name: "second", ExecutionMode: ExecutionModeAsyncNewTx},
					}}}},
					"DONE": {},
				},
			}})
			txID, txCtx, err := tc.mgr.Begin(base)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			entity := makeEntity("sp-1", modelRef, map[string]any{"x": 1})
			entity.Meta.TransactionID = txID

			_, err = engine.Execute(txCtx, entity, "")
			if !errors.Is(err, ErrSavepointInfra) {
				t.Fatalf("err = %v; want ErrSavepointInfra — a savepoint failure is not the processor's failure", err)
			}
			if !errors.Is(err, boom) {
				t.Fatalf("the cause must stay in the chain for the log: %v", err)
			}
			if entity.Meta.State == "DONE" {
				t.Fatal("the pipeline carried on past an unusable transaction")
			}
			if dispatched != tc.wantDispatch {
				t.Fatalf("dispatched %d processors; want %d (the second must never run)", dispatched, tc.wantDispatch)
			}
		})
	}
}
```

`internal/domain/entity/service_classify_test.go` — beside the existing `classifyWorkflowError` cases:

```go
func TestClassifyWorkflowError_SavepointInfra(t *testing.T) {
	driver := errors.New(`ERROR: current transaction is aborted (SQLSTATE 25P02) host=db-1`)
	tests := []struct {
		name      string
		err       error
		status    int
		code      string
		retryable bool
	}{
		{"ticketed 5xx, no driver text", fmt.Errorf("processor p failed: %w", errors.Join(wfengine.ErrSavepointInfra, driver)), http.StatusInternalServerError, common.ErrCodeServerError, false},
		{"a conflict stays 409", fmt.Errorf("processor p failed: %w", errors.Join(wfengine.ErrSavepointInfra, spi.ErrConflict)), http.StatusConflict, common.ErrCodeConflict, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyWorkflowError(tc.err)
			if got.Status != tc.status || got.Code != tc.code || got.Retryable != tc.retryable {
				t.Fatalf("got %d %s retryable=%v; want %d %s retryable=%v", got.Status, got.Code, got.Retryable, tc.status, tc.code, tc.retryable)
			}
			if strings.Contains(got.Message, "SQLSTATE") || strings.Contains(got.Message, "db-1") {
				t.Fatalf("driver text reached the client: %q", got.Message)
			}
		})
	}
}
```

The storage-unavailable → 503 case: copy the fixture the existing `STORAGE_UNAVAILABLE` case of this file uses (an error satisfying `interface{ StorageUnavailable() bool }`), joined with `ErrSavepointInfra`; expect 503 `STORAGE_UNAVAILABLE` retryable. "Nothing is committed" at the handler level: `TestCreateEntity_AsyncNewTxSavepointFailure_NothingCommitted` in `internal/domain/entity/service_rollback_test.go`, built like that file's existing rollback tests with `failingSavepoints{failRelease: …}` as the handler's manager: `CreateEntity` returns a 500 `*common.AppError`, and `visibleOutsideTx` is false for every entity id.

PostgreSQL: beside the existing savepoint tests, `TestSavepoint_StatementErrorIsClassified` — begin, make the transaction fail a statement (the file's existing way of provoking 25P02), call `Savepoint`; assert the error satisfies what `classifyError` yields for that class (the same assertion the neighbouring `Commit`/`Exec` classification tests make) rather than a bare `*pgconn.PgError`.

- [ ] **Step 2: Run to verify RED**
Run: `go test ./internal/domain/workflow/... ./internal/domain/entity/... -run 'TestAsyncNewTx_SavepointFailure|TestClassifyWorkflowError_SavepointInfra|TestCreateEntity_AsyncNewTxSavepointFailure'`   Expected: FAIL to build (`undefined: ErrSavepointInfra`); with the sentinel declared and nothing else: `err = <nil>; want ErrSavepointInfra`.

- [ ] **Step 3: Implement**

`engine_processors.go`:

```go
// ErrSavepointInfra marks a savepoint that could not be created, undone or
// released around an ASYNC_NEW_TX processor. It is never the processor's
// failure: the transaction is unusable — gone, missing the savepoint, or on
// PostgreSQL already aborted by an earlier failed statement — so the operation
// fails instead of committing the writes of a processor that failed, or
// silently skipping one. Handlers map it to a sanitized 5xx.
var ErrSavepointInfra = errors.New("savepoint failure")
```

```go
// executeAsyncNewTx — after (the e.txMgr == nil branch is deleted)
func (e *Engine) executeAsyncNewTx(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition, workflow, transition, txID string) error {
	if e.extProc == nil {
		return nil
	}

	spID, err := e.txMgr.Savepoint(ctx, txID)
	if err != nil {
		return fmt.Errorf("failed to create savepoint: %w", errors.Join(ErrSavepointInfra, err))
	}

	resume := txgate.Suspend(ctx)
	defer resume()
	_, dispatchErr := e.extProc.DispatchProcessor(ctx, entity, proc, workflow, transition, txID)
	resume()
	if cerr := fence.Check(ctx); cerr != nil { // F-8
		return cerr
	}
	if dispatchErr != nil {
		if rbErr := e.txMgr.RollbackToSavepoint(ctx, txID, spID); rbErr != nil {
			// The processor's own error is not joined in: it may be a classified
			// client error, and errors.As would then answer with it.
			slog.Warn("ASYNC_NEW_TX processor failed and its savepoint could not be undone",
				"pkg", "workflow", "processor", proc.Name, "processorError", dispatchErr)
			return fmt.Errorf("failed to undo savepoint: %w", errors.Join(ErrSavepointInfra, rbErr))
		}
		return dispatchErr
	}

	if err := e.txMgr.ReleaseSavepoint(ctx, txID, spID); err != nil {
		return fmt.Errorf("failed to release savepoint: %w", errors.Join(ErrSavepointInfra, err))
	}
	return nil
}
```

`executeProcessors` — the two places that treat an `ASYNC_NEW_TX` error as non-fatal:

```go
		nonFatal := proc.ExecutionMode == ExecutionModeAsyncNewTx && !errors.Is(procErr, ErrSavepointInfra)
		if procErr != nil && nonFatal {
			slog.Warn("ASYNC_NEW_TX processor failed, continuing pipeline", ...)
		}
		...
		if procErr != nil && !nonFatal {
			return currentCtx, currentTxID, fmt.Errorf("processor %s failed: %w", proc.Name, procErr)
		}
```

`classifyWorkflowError` — with the other infra branches, after `ErrScheduledTaskInfra` (so `StorageUnavailable`, checked first, keeps its 503; `common.Internal` keeps `spi.ErrConflict` → retryable 409, as `service_classify_test.go` asserts for `ErrCommitBeforeDispatchInfra`):

```go
	if errors.Is(err, wfengine.ErrSavepointInfra) {
		return common.Internal("workflow savepoint failed", err)
	}
```

and a bullet in its doc comment. `plugins/postgres/transaction_manager.go`, three times:

```go
// before
		return "", fmt.Errorf("Savepoint: %w", err)
// after
		return "", tm.classifyTxError(txID, fmt.Errorf("Savepoint: %w", err))
```

(`RollbackToSavepoint:` and `ReleaseSavepoint:` likewise; only the `pgxTx.Exec` error returns, not the `state.` ones.) Comment fix: `executeProcessors`' doc (`:63-64`) "ASYNC_NEW_TX runs within a savepoint (failures are non-fatal)" → "(the processor's failure is non-fatal; a savepoint failure is fatal)".

- [ ] **Step 4: Run to verify GREEN** — `go test ./internal/domain/workflow/... ./internal/domain/entity/...`; `cd plugins/postgres && go test ./...` (Docker). Exit check: `grep -n 'e.txMgr == nil' internal/domain/workflow/engine_processors.go` → no hits; `grep -rn 'NewEngine(.*, nil)' --include='*.go' internal/` → no hits.

- [ ] **Step 5: Commit**
`git add internal/domain/workflow internal/domain/entity plugins/postgres && git commit -m "fix(workflow): a savepoint failure fails the operation instead of passing as the processor's (#254)"`

---

### Task F-10: documentation of this stream's changes

**Spec:** §14 (the parts that are fencing), §15 `### Breaking`.

**Files:**
- Modify: `CHANGELOG.md` `[Unreleased]`
- Modify: `cmd/cyoda/help/content/grpc.md` (the section that describes the transaction token on processor/criterion/function requests), `cmd/cyoda/help/content/workflows.md` (processors; `ASYNC_NEW_TX`)
- Modify: `cmd/cyoda/help/content/errors/TRANSACTION_NOT_FOUND.md`, `errors/TRANSACTION_EXPIRED.md` (`see_also` + SEE ALSO gain `errors.CALLOUT_SUPERSEDED`)
- Modify: `docs/cloud-parity/nested-join-tx-serialisation.md` (it states today that joined entity *writes* are serialised; it now states every joined request is) — and the fencing paragraph below goes into `docs/cloud-parity/callout-failover.md`, which another stream creates (§14); if it does not exist when this task runs, this task creates it with that section and a README row, and the other stream appends.
- Not touched: `README.md`, `DefaultConfig()`, `config_registry.go` — this stream adds no setting.

- [ ] **Step 1: the test** is `go test ./cmd/cyoda/help/...` (front-matter, see-also resolution and topic parity are checked there); it is green before and must stay green.

- [ ] **Step 2: write**

`CHANGELOG.md`:

```markdown
### Breaking

- **A request carrying a transaction token is accepted only while the processor,
  criterion or function request it was issued for is still that compute node's.**
  Until now it was accepted until the transaction closed. Once cyoda has given
  the work to another compute node, or the request has ended, the answer is
  `410 CALLOUT_SUPERSEDED` (not retryable); once the transaction has ended it is
  `404 TRANSACTION_NOT_FOUND`, as before. Tokens minted by earlier versions carry
  no callout and are refused with `401`.

### Fixed

- A compute node that was given up on could still write into the operation's
  transaction — after its replacement had answered, or, under `ASYNC_NEW_TX`,
  after the savepoint of its failed processor had been undone — and the write
  was committed. Its requests are now refused, and a request already in progress
  finishes before the work moves on.
- Requests joined to one transaction ran concurrently with each other unless
  both were entity writes. Two parallel reads were enough: on the memory and
  SQLite backends the process died with `concurrent map writes`; on PostgreSQL
  the operation failed with `conn busy`. Every joined request now holds the
  transaction's lock for its whole length, and its response is sent after the
  lock is released.
- A compute node that disconnected in the middle of a request joined to a
  transaction cancelled a statement on the operation's PostgreSQL connection,
  which destroyed the connection and failed the operation. A joined request is
  no longer cancelled by its client.
- Under `ASYNC_NEW_TX`, a savepoint that could not be created, undone or
  released was treated as the processor's own non-fatal failure — silently
  skipping a processor, or committing the writes of one that failed. It now
  fails the operation.
```

`grpc.md` — in the transaction-token section, add:

```markdown
The token is issued for one request on one compute node. If cyoda gives the
same work to another compute node — this one did not answer within its answer
limit, or its connection dropped — or once the request has ended, every further
API request under that token is refused with `410 CALLOUT_SUPERSEDED`; a request
that was already in progress finishes and is answered normally. A compute node
that receives `CALLOUT_SUPERSEDED` must stop working on that request. While a
compute node's request is in progress it has the transaction to itself: API
requests under one token run one at a time, and cyoda does not interrupt one
because its client went away.
```

`workflows.md` — in the processor section:

```markdown
A processor must not change a model or a workflow through the API while it
runs; that is not supported. An EdgeMessage a processor saves is outside the
entity transaction: make the save idempotent and attach the message id to the
owning entity, so that a repeat after a failover leaves at most an orphan.
```

and in the `ASYNC_NEW_TX` paragraph: "The processor's failure does not fail the operation. A failure of the savepoint itself — it cannot be created, undone or released — does."

`docs/cloud-parity/callout-failover.md`, section **Fencing a replaced compute node** — the contract in five sentences: the token names the callout and a number that rises each time the work is given to another compute node; a request is admitted only under the current number, after the transaction's tenant check; every joined request holds the transaction's lock and is checked again under it; the owner waits for a request in progress before the next compute node gets the work or the engine carries on; refusal is `410 CALLOUT_SUPERSEDED`, and after the transaction ended `404 TRANSACTION_NOT_FOUND`.

- [ ] **Step 3: Run** — `go test ./cmd/cyoda/help/...`

- [ ] **Step 4: Commit**
`git add CHANGELOG.md cmd/cyoda/help/content docs/cloud-parity && git commit -m "docs: fencing a replaced compute node — changelog, help, cloud parity (#254)"`

Every commit message in this section ends with the line
`Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

---

## Stream interface summary

**Other streams may consume from F**

- `internal/fence` (leaf: imports `internal/txgate`, `internal/common` only):
  - `type Pair struct { Callout string; Major, Minor uint32 }` (JSON `c`, `j`, `i`)
  - `func New(gate *txgate.Registry) *Fence`
  - `func (f *Fence) Begin(ctx context.Context, calloutID, txID string, outer []Pair) (context.Context, func())` — `end` is idempotent, waits, and must be deferred; the context is cancelled with cause `ErrSuperseded` when an outer pair stops being current (at once if one already is not); empty `txID` allowed.
  - `func (f *Fence) Advance(calloutID string, major uint32)` — raises, releases inner Coordinators, **waits**; no context, no error. Majors start at 1; a major not higher than the current one is ignored.
  - `func (f *Fence) Admit(ctx context.Context, pairs []Pair) (context.Context, error)` — own pair first, then `Outer`.
  - `func Check(ctx context.Context) error`; `func Pairs(ctx context.Context) []Pair`
  - `var ErrSuperseded error`; `func NewSupersededError() *common.AppError`
- `internal/cluster/token`: `type Pair = fence.Pair`; `Claims{NodeID, TxRef string; ExpiresAt int64; Callout string; Major, Minor uint32; Outer []Pair}`; `(*Signer).Issue(claims Claims) (string, error)`; `Verify` refuses a pass without callout and number (`ErrTokenInvalid`).
- `common.ErrCodeCalloutSuperseded`.
- `internal/domain/txjoin`: `NewJoiner(signer, txMgr, f, gate) *Joiner`; `(*Joiner).Run(ctx, tok, handler func(ctx)) error`; `JoinFromToken(ctx, signer, txMgr, f, tok)`.
- `(*app.App).Fence() *fence.Fence`.
- `workflow.ErrSavepointInfra`.

**What stream O (owner's loop) calls, and must do**
- Per callout: `ctx, end := f.Begin(ctx, RequestID, TxID, fence.Pairs(ctx)); defer end()`; `Callout.Outer = fence.Pairs(ctx)` (taken from the context **Begin was given**, i.e. the callback's); its `TryNumberer.Next()` raises its one major counter, calls `f.Advance(RequestID, major)`, returns `(major, 0)`; before each hand-over the same counter and `Advance`.
- When its context ends: `errors.Is(context.Cause(ctx), fence.ErrSuperseded)` → return `fence.NewSupersededError()`; anything else as today.
- Deletes `app/once_fenced.go` and its test in the task that makes the Coordinator the `ExternalProcessingService` (`grep -rn 'onceFenced' --include='*.go' .` → no hits), and deletes `ClusterDispatcher` with its `mintTxToken`.
- Owns the E and M cells of the rows in the table above (all except the two marked **F**); for them it drives real cnodes through stream H's harness; `h.app.Fence()` is available where a test must move the number itself.

**F consumes**
- Nothing from other streams' new code. Ordering constraints: F-4 before L-10; F-5 and F-6 before L-11; L-11 after O's take-over. Stream C's removal of `CYODA_TX_TOKEN_TTL` (C-11) must come after O deletes `onceFenced`, which reads `cfg.Cluster.TxTokenTTL` (`app.go`), as `app.go:441, 554` do today.
- §12's counter of `CALLOUT_SUPERSEDED` refusals is the observability stream's; the two places a refusal is produced are `(*Fence).Admit` and `fence.Check`, both returning `NewSupersededError()`. It must count without a hook in `internal/fence` — e.g. where the doors render the error (`common.WriteError`, the gRPC envelope builders), by `errors.Is(err, fence.ErrSuperseded)`.

## Open points

1. **Reading the request before the lock: the pre-lock `RecvMsg` has no bound of its own.** (The hole itself — request read under the lock — was found while settling V-5 and is now in the spec, `d4e53311`; F-7 implements it.) What remains: over gRPC the interceptor's `RecvMsg` waits for a cnode that opened the stream and sends nothing, for as long as the connection's keepalive lets it. It holds no lock, so the transaction is safe; it holds a goroutine and a stream, as `proxyStream` does today. Not changed here. Also a ruling taken in F-7: the join layer's HTTP body limit is one constant, 10 MiB, the largest handler cap; a body over it is `413 BAD_REQUEST`, where a handler today answers `400` for the same body — the status for that one case changes on joined requests only.
2. **Temporary unreachable code.** With `onceFenced` wired, the dispatchers' own pass minting (`grpc/dispatch.go:60-73`, `cluster_dispatcher.go:209-219`) is never reached in production when a transaction exists. It is kept, mechanically adapted (F-4), because deleting it changes `NewProcessorDispatcher`/`NewClusterDispatcher` and ~40 test call sites that L-8/L-10 and O rewrite anyway. A pass minted by those fallbacks names a callout nobody began and is refused — fail-closed. The alternative (F begins/ends inside both dispatchers, no decorator) was rejected: a callout with no transaction must still be begun exactly once, and the local dispatcher cannot tell whether the cluster dispatcher already did.
3. **`Advance` and `end` have no context and no timeout** (spec signature). They wait on joined requests bounded "by the database's own limits". On memory and SQLite there is no such limit on in-process work; a joined request stuck for a non-database reason blocks its owner for ever. Accepted by the spec as written; stated here because the join layer now makes *every* joined request part of that wait.
4. **`COMMIT_BEFORE_DISPATCH` inside a callback** (the separately filed defect, §16): a superseded joined chain that segmented reaches `rollbackSegment` (`engine.go:265`) and `txScope.Release` (`txscope.go:127-152`, the `txID != entryTxID` arm), which roll back the segment *it* opened — a store operation by a refused chain, though not on the owner's transaction, and `Release` takes that segment's lock. Not planned here; it disappears with that defect's fix.
5. **`errors.md` index said `400` for `TRANSACTION_EXPIRED`** (`cmd/cyoda/help/content/errors.md:106`); the topic and the code say `410`. Corrected in F-1.
6. **`txgate.Registry.Acquire` uses bare `Unlock()`** (`internal/txgate/txgate.go:36-52`), against `.claude/rules/go-mutex-discipline.md`. Not in this stream's assignment and behaviour-neutral; two IIFEs fix it. Whoever next touches the file — or a one-commit chore before F-3, which leans on `Acquire` counting a waiter before it blocks.
7. **`Issue` validates nothing; `Verify` does.** The brief's "a pass without callout and number is invalid" is implemented on the verify side only, so that the e2e layer can mint the §8.2 401 pass from the server's ephemeral signer (`callback_txjoin_errors_test.go`). If `Issue` should refuse as well, the E cell of that row needs the harness to expose the secret instead.
8. **Refusals are logged at DEBUG without the callout id** (F-2). The id is a claim inside a credential; tenant, door and route are on the request log already. If operators need the id, it is one field — a ruling, not a default.
9. **A held gRPC stream drops its held frames when the handler returns an error** (F-7). Today a chunked `EntityManageCollection` that fails at chunk *n* has already sent the responses of chunks 1…*n*-1. For a *joined* request chunks do not commit (the owner commits), so nothing durable is misreported; it is a visible difference on that one path and wants a sentence in the parity doc if kept.

