# Stream L — classifying a try, and the local procedure

Packages: `internal/contract`, `internal/grpc` (plus the one constructor call in
`app/app.go`). Spec §3, §4, §5 "Patience" (the local `Changed()` only), §7
"Claims" (minting only), §9 (the answer limit as the dispatcher applies it).

**Verification points.** None of V-1 … V-4 falls in §3, §4 or the minting part
of §7 (V-2 is `arm.go`, V-3 is metric naming, V-4 is settled in the spec text).
Facts this stream relies on, read in the code:

- `internal/contract` imports nothing from cyoda-go, and `internal/common`
  imports nothing from cyoda-go either (`go list -deps ./internal/common`). The
  new type therefore carries its classified error as a plain `error` field and
  `contract` stays a leaf; the test (an external `contract_test` package) may
  import `internal/common` without a cycle.
- `classifyWorkflowError` (`internal/domain/entity/service.go:2848`) walks the
  chain with `errors.As(err, &appErr)` first, then `errors.Is` on
  `contract.ErrNoMatchingMember` (`:2867`) and
  `contract.ErrAuthContextUnavailable` (`:2882`). The peer handler
  (`internal/cluster/dispatch/handler.go:192-208`) and `isNoMatchingMember`
  (`cluster_dispatcher.go:441`) do the same. A `*contract.CalloutFailure` with
  `Unwrap() error` keeps all of them working unchanged.
- `common.Operational` stores the message as `"CODE: text"`
  (`internal/common/errors.go:116-123`), so `CalloutFailure.Message` taken from
  an `*AppError` carries the code prefix.
- `app.go:148-170`: the token signer is always present (cluster secret or a
  per-process ephemeral one; startup exits otherwise). The `d.signer == nil`
  branch in `resolveTxToken` is unreachable in production.
- `frozen_member_test.go` drives `Member.Send` and the keep-alive loop directly;
  it never calls the dispatcher.
- `NewCloudEvent` (`internal/grpc/cloudevent.go:18-31`) gives the CloudEvent
  *envelope* a random `Id` per event; the request id travels in the payload as
  `id` and `requestId` (`dispatch.go:220-222`). See Open points.

**The spec moved while this was drafted.** It was written against the spec at
`73e0b99e`; `de72856b`, `827ed61c` and an uncommitted revision followed. Their
diff was read: §3, §4, §9 and the minting part of §7 are unchanged. The one
change that touches this stream — "the number rises … just before the hand-off
is attempted, so also when that hand-off then fails", and the Coordinator's one
counter feeding both its `TryNumberer` and its hand-overs — is what L-8 does
(`Next` is called before every try, failed hand-offs included).

**Order and dependencies.**

```
L-1 ─┬─ L-2 ── L-3 ─┬─ L-5* ── L-6§ ── L-7 ── L-8 ── L-9 ── L-10*† ── L-11‡
     └─ L-4 ────────┘
*  needs stream C's C-1 (cfg.Callout.ResponseTimeout / ResponseTimeoutMax / PassAllowance) — for the app.go line only
§  needs stream C's C-5 (contract.ParseCriterionFunction)
†  needs stream F's token.Claims / Signer.Issue
‡  needs stream P to have stopped using WithTxToken / TxTokenFromContext / DispatchCalloutRequest.TxToken
```

Stream C's plan (`docs/superpowers/plans/2026-09-19-254-callout-failover/C-config-spi-import.md`)
already states what it expects of this stream, and this plan meets it: C-1 — this
stream deletes `defaultResponseTimeoutMs` (L-5); C-5 — the rewrite of
`DispatchCriteria` uses `contract.ParseCriterionFunction`, making C-5's
`dispatch.go` step a no-op (L-6); C-11 — `app.go:441` stops passing
`cfg.Cluster.TxTokenTTL` (L-10; `app.go:554` is the Coordinator stream's).

**How far this draft was checked.** The code of L-1 … L-9 was applied to a
throw-away copy of the worktree (outside the repository): `go build ./internal/... ./app/...`,
`go vet`, `gofmt -l`, and the full test sets of `internal/contract`,
`internal/grpc` and `internal/cluster/dispatch` are green there, and the new
tests ran five times under `-race` without a failure. L-10 was checked the same
way against a stand-in for stream F's `token.Claims` / `Signer.Issue`. In that
check `NewCriteriaCallout` still parsed with today's ad-hoc struct; the switch to
stream C's `contract.ParseCriterionFunction` was made in the text afterwards and
is not compiled. L-11
could not be compiled: it needs stream P's removal first. The per-task RED
states were not replayed one by one.

Every task leaves `go build ./...` and the `internal/grpc`, `internal/contract`
test sets green. The three `Dispatch*` methods keep working throughout; from L-8
they are thin wrappers over `RunLocal(ctx, call, 1)`.

---

### Task L-1: `CalloutFailure`, `CalloutFailureKind`, `CalloutAttempt`

**Spec:** §3 (the type, the four kinds, the "whether another cnode may be tried" table); §8.2 last paragraphs (how the classifier finds it).

**Files:**
- Create: `internal/contract/callout.go`
- Test: `internal/contract/callout_test.go` (package `contract_test`)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type CalloutFailureKind int` with `NoHandOff, NoAnswer, MemberFailed, Terminal`
  - `func (k CalloutFailureKind) String() string` — `"no_handoff" | "no_answer" | "member_failed" | "terminal"` (the words §6 uses for `outcome`)
  - `func (k CalloutFailureKind) MayTryAnother(repeatSafe bool) bool` — the §3 table, in one place for `RunLocal` and the owner's loop
  - `type CalloutAttempt struct { MemberID string; Kind CalloutFailureKind; Cause string }`
  - `type CalloutFailure struct { Kind CalloutFailureKind; Code, Message string; Retryable *bool; Attempts []CalloutAttempt; Err error }`
  - `func (f *CalloutFailure) Error() string`, `func (f *CalloutFailure) Unwrap() error`

`Err` is one field more than the spec's listing. It is what makes "every other
kind already carries an `*AppError` and passes through the first branch" (§8.2)
true: `errors.As` reaches the `*common.AppError`, and `errors.Is` reaches
`ErrNoMatchingMember` / `ErrAuthContextUnavailable`, through `Unwrap`.

- [ ] **Step 1: Write the failing tests**

```go
package contract_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// The engine wraps a processor's error as "processor <name> failed: %w"; the
// classifier must still find both the failure and the AppError it carries.
func TestCalloutFailure_ErrorsAsFindsFailureAndItsAppError(t *testing.T) {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
		"processor dispatch timed out after 50ms: no response").AsRetryable()
	failure := &contract.CalloutFailure{Kind: contract.NoAnswer, Code: appErr.Code, Message: appErr.Message, Err: appErr}
	wrapped := fmt.Errorf("processor %s failed: %w", "charge", failure)

	var gotFailure *contract.CalloutFailure
	if !errors.As(wrapped, &gotFailure) {
		t.Fatalf("errors.As did not find *CalloutFailure in %v", wrapped)
	}
	if gotFailure.Kind != contract.NoAnswer {
		t.Errorf("Kind = %v, want NoAnswer", gotFailure.Kind)
	}
	var gotApp *common.AppError
	if !errors.As(wrapped, &gotApp) {
		t.Fatalf("errors.As did not find the carried *AppError in %v", wrapped)
	}
	if gotApp != appErr {
		t.Errorf("found a different *AppError than the one carried")
	}
	if got, want := failure.Error(), appErr.Error(); got != want {
		t.Errorf("Error() = %q, want the carried error's text %q", got, want)
	}
}

func TestCalloutFailure_MemberFailedCarriesNoAppError(t *testing.T) {
	yes := true
	failure := &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "card declined", Retryable: &yes}
	wrapped := fmt.Errorf("processor %s failed: %w", "charge", failure)

	var gotApp *common.AppError
	if errors.As(wrapped, &gotApp) {
		t.Fatalf("a MemberFailed failure must not carry an *AppError, found %v", gotApp)
	}
	if got, want := wrapped.Error(), "processor charge failed: card declined"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
	if failure.Unwrap() != nil {
		t.Errorf("Unwrap() = %v, want nil", failure.Unwrap())
	}
}

func TestCalloutFailure_SentinelsSurvive(t *testing.T) {
	for _, sentinel := range []error{contract.ErrNoMatchingMember, contract.ErrAuthContextUnavailable} {
		failure := &contract.CalloutFailure{Kind: contract.NoHandOff, Err: fmt.Errorf("%w: tags %q", sentinel, "python")}
		if !errors.Is(fmt.Errorf("outer: %w", failure), sentinel) {
			t.Errorf("errors.Is lost %v through the failure", sentinel)
		}
	}
}

func TestCalloutFailureKind_MayTryAnother(t *testing.T) {
	tests := []struct {
		kind             contract.CalloutFailureKind
		repeatSafe, want bool
	}{
		{contract.NoHandOff, true, true},
		{contract.NoHandOff, false, true},
		{contract.NoAnswer, true, true},
		{contract.NoAnswer, false, false},
		{contract.MemberFailed, true, false},
		{contract.MemberFailed, false, false},
		{contract.Terminal, true, false},
		{contract.Terminal, false, false},
	}
	for _, tt := range tests {
		if got := tt.kind.MayTryAnother(tt.repeatSafe); got != tt.want {
			t.Errorf("%v.MayTryAnother(%v) = %v, want %v", tt.kind, tt.repeatSafe, got, tt.want)
		}
	}
}

func TestCalloutFailureKind_String(t *testing.T) {
	want := map[contract.CalloutFailureKind]string{
		contract.NoHandOff:    "no_handoff",
		contract.NoAnswer:     "no_answer",
		contract.MemberFailed: "member_failed",
		contract.Terminal:     "terminal",
	}
	for kind, s := range want {
		if kind.String() != s {
			t.Errorf("String() = %q, want %q", kind.String(), s)
		}
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/contract/...`   Expected: FAIL (build) with `undefined: contract.CalloutFailure`.

- [ ] **Step 3: Implement**

`internal/contract/callout.go`:

```go
package contract

import "fmt"

// CalloutFailureKind says what happened to a try that produced no result. The
// one question it answers is whether another cnode may be given the work.
type CalloutFailureKind int

const (
	// NoHandOff: the work provably never left this pnode.
	NoHandOff CalloutFailureKind = iota
	// NoAnswer: the work was handed to a cnode, then silence or a dropped
	// connection. The work may have run.
	NoAnswer
	// MemberFailed: the cnode answered success=false.
	MemberFailed
	// Terminal: the try would fail identically on any cnode.
	Terminal
)

// String is the word the hand-over answer uses for the kind.
func (k CalloutFailureKind) String() string {
	switch k {
	case NoHandOff:
		return "no_handoff"
	case NoAnswer:
		return "no_answer"
	case MemberFailed:
		return "member_failed"
	case Terminal:
		return "terminal"
	default:
		return fmt.Sprintf("CalloutFailureKind(%d)", int(k))
	}
}

// MayTryAnother reports whether another cnode may be tried after a failure of
// this kind. repeatSafe is true for every criterion, every function, and a
// processor declared idempotent.
func (k CalloutFailureKind) MayTryAnother(repeatSafe bool) bool {
	switch k {
	case NoHandOff:
		return true
	case NoAnswer:
		return repeatSafe
	default:
		return false
	}
}

// CalloutAttempt records one failed try, for the message a client sees when
// every try is used up.
type CalloutAttempt struct {
	// MemberID is the cnode tried, or "-" for a hand-over whose answer was lost.
	MemberID string
	Kind     CalloutFailureKind
	// Cause is client-safe text.
	Cause string
}

// CalloutFailure reports why a callout produced no result.
//
// It is an error, and it carries the classified error the try produced in Err,
// so that errors.As still finds an *AppError, and errors.Is still finds
// ErrNoMatchingMember or ErrAuthContextUnavailable, behind any wrapping the
// workflow engine adds. A MemberFailed failure carries none: what the client
// sees for it is decided where workflow errors are classified.
type CalloutFailure struct {
	Kind CalloutFailureKind
	// Code is the error code for the client; empty for MemberFailed and for a
	// Terminal failure that has no code of its own.
	Code string
	// Message is client-safe text; for MemberFailed, the cnode's own.
	Message string
	// Retryable is the cnode's verdict (MemberFailed only); nil if it gave none.
	Retryable *bool
	// Attempts is every failed try of the callout. The local procedure leaves
	// it nil and reports its tries beside the failure; the owner fills it on
	// the failure it finally returns.
	Attempts []CalloutAttempt
	// Err is the classified error of the try; nil for MemberFailed.
	Err error
}

func (f *CalloutFailure) Error() string {
	if f.Err != nil {
		return f.Err.Error()
	}
	return f.Message
}

func (f *CalloutFailure) Unwrap() error { return f.Err }
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/contract/...`   Expected: PASS. Then `go vet ./internal/contract/...`.

- [ ] **Step 5: Commit**

```
git add internal/contract/callout.go internal/contract/callout_test.go
git commit -m "feat(contract): CalloutFailure carries the kind of a failed try (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task L-2: `Candidates` replaces `FindByTags`

**Spec:** §4 "Selection", first sentence.

**Files:**
- Modify: `internal/grpc/members.go` (`FindByTags`, lines 467-479 → `Candidates`)
- Modify: `internal/grpc/dispatch.go` (the three lookups: `DispatchProcessor` :210-214, `DispatchCriteria` :301-305, `DispatchFunction` :350-354; the doc comment at :100)
- Modify: `internal/grpc/members_test.go` (delete the four `TestMemberRegistry_FindByTags_*`, lines 80-118)
- Modify (comment only): `internal/grpc/scheduled_function_rpc_test.go:96`, `internal/e2e/dispatch_infra_error_test.go:16`, `internal/e2e/callback_harness_test.go:554`, `internal/e2e/scheduled_function_test.go:36` — `FindByTags` → `Candidates`
- Test: `internal/grpc/members_candidates_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func (r *MemberRegistry) Candidates(tenantID spi.TenantID, tagsCSV string) []*Member` — every member of the tenant whose tags overlap `tagsCSV` (all of the tenant's members when it is empty), ordered by `(ConnectedAt, ID)`; nil when none.

**Existing tests:** `TestMemberRegistry_FindByTags_MatchingTag / _NoMatchingTag / _EmptyRequired / _WrongTenant` — deleted with `FindByTags`; their four cases are the first four rows of the new table test. `TestDispatchProcessor_NoMember`, `TestDispatchFunction_NoMember` — stay, unchanged.

- [ ] **Step 1: Write the failing tests**

`internal/grpc/members_candidates_test.go`:

```go
package grpc

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestCandidates_TenantAndTagFilter(t *testing.T) {
	tests := []struct {
		name     string
		tenant   spi.TenantID
		required string
		wantIDs  []string
	}{
		{"matching tag", "tenant-1", "ml", []string{"m-1"}},
		{"no matching tag", "tenant-1", "java", nil},
		{"empty required matches every member of the tenant", "tenant-1", "", []string{"m-1", "m-2"}},
		{"wrong tenant", "tenant-3", "python", nil},
		{"any overlap is enough", "tenant-1", "java, go", []string{"m-2"}},
		{"another tenant's member on the same tag is never a candidate", "tenant-2", "python", []string{"m-other"}},
	}
	reg := NewMemberRegistry()
	for _, m := range []*Member{
		reg.Register("m-1", "tenant-1", []string{"python", "ml"}, noopSend, nil),
		reg.Register("m-2", "tenant-1", []string{"go"}, noopSend, nil),
		reg.Register("m-other", "tenant-2", []string{"python"}, noopSend, nil),
	} {
		t.Cleanup(func() { reg.Unregister(m) })
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reg.Candidates(tt.tenant, tt.required)
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("got %d candidates, want %v", len(got), tt.wantIDs)
			}
			for i, m := range got {
				if m.ID != tt.wantIDs[i] {
					t.Errorf("candidate %d = %s, want %s", i, m.ID, tt.wantIDs[i])
				}
			}
		})
	}
}

// The registry is a Go map, whose iteration order changes from call to call;
// Candidates must not.
func TestCandidates_OrderedByConnectedAtThenID(t *testing.T) {
	reg := NewMemberRegistry()
	b := reg.Register("b", "tenant-1", []string{"x"}, noopSend, nil)
	a := reg.Register("a", "tenant-1", []string{"x"}, noopSend, nil)
	c := reg.Register("c", "tenant-1", []string{"x"}, noopSend, nil)
	for _, m := range []*Member{a, b, c} {
		t.Cleanup(func() { reg.Unregister(m) })
	}
	t0 := time.Unix(1_000, 0)
	c.ConnectedAt = t0 // attached first
	a.ConnectedAt = t0.Add(time.Second)
	b.ConnectedAt = t0.Add(time.Second) // same instant as a: the id decides

	for i := 0; i < 20; i++ {
		got := reg.Candidates("tenant-1", "x")
		if len(got) != 3 || got[0] != c || got[1] != a || got[2] != b {
			ids := make([]string, len(got))
			for j, m := range got {
				ids[j] = m.ID
			}
			t.Fatalf("call %d: order = %v, want [c a b]", i, ids)
		}
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/grpc/... -run 'TestCandidates'`   Expected: FAIL (build) with `reg.Candidates undefined`.

- [ ] **Step 3: Implement**

`internal/grpc/members.go` — add `"cmp"` and `"slices"` to the imports; replace `FindByTags` (lines 467-479) with:

```go
// Candidates returns every member of the tenant whose tags overlap tagsCSV —
// every member of the tenant when tagsCSV is empty — ordered by (ConnectedAt,
// ID), so that the order is the same on every call. A member of another tenant
// is never a candidate, whatever its tags.
func (r *MemberRegistry) Candidates(tenantID spi.TenantID, tagsCSV string) []*Member {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Member
	for _, m := range r.members {
		if m.TenantID == tenantID && common.TagsOverlap(m.Tags, tagsCSV) {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(x, y *Member) int {
		if c := x.ConnectedAt.Compare(y.ConnectedAt); c != 0 {
			return c
		}
		return cmp.Compare(x.ID, y.ID)
	})
	return out
}
```

`internal/grpc/dispatch.go` — in each of the three methods, before:

```go
	member := d.registry.FindByTags(tenantID, processor.Config.CalculationNodesTags)
	if member == nil {
```

after (the same shape in `DispatchCriteria` with `parsed.Function.Config.CalculationNodesTags` and in `DispatchFunction` with `fn.CalculationNodesTags`):

```go
	candidates := d.registry.Candidates(tenantID, processor.Config.CalculationNodesTags)
	if len(candidates) == 0 {
```

and directly after the closing brace of that `if`, add `member := candidates[0]`.
In the doc comment of `dispatchCalloutToMember` (:100) replace
`(FindByTags/ErrNoMatchingMember)` with `(Candidates/ErrNoMatchingMember)`.

Delete `TestMemberRegistry_FindByTags_*` from `members_test.go`. In the four
comment-only files replace the word `FindByTags` with `Candidates`.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/grpc/...` and `go vet ./internal/grpc/... ./internal/e2e/...`   Expected: PASS / clean.
Exit check: `grep -rn 'FindByTags' --include='*.go' .` → no hits.

- [ ] **Step 5: Commit**

```
git add internal/grpc/members.go internal/grpc/dispatch.go internal/grpc/members_test.go internal/grpc/members_candidates_test.go internal/grpc/scheduled_function_rpc_test.go internal/e2e/dispatch_infra_error_test.go internal/e2e/callback_harness_test.go internal/e2e/scheduled_function_test.go
git commit -m "refactor(grpc): Candidates lists a tenant's matching cnodes in a stable order (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task L-3: `MemberSelector`, `RoundRobinSelector`, the pick stamp

**Spec:** §4 "Selection" (selector, stamp, "a new one has never been picked and goes first"); D4. §13 row "Round robin across two cnodes; a new cnode goes first" — **U** (selector level here; across `RunLocal` calls in L-8).

**Files:**
- Create: `internal/grpc/selector.go`
- Modify: `internal/grpc/members.go` (`Member` gains `pickStamp`; `MemberRegistry` gains `pickMu`, `pickCounter`, `pickLeastRecent`)
- Modify: `internal/grpc/dispatch.go` (`ProcessorDispatcher.selector`; `NewProcessorDispatcher`; the three `member := candidates[0]`)
- Modify: `app/app.go:441`
- Modify: `internal/grpc/dispatch_test.go` (new helper `newTestDispatcher`; lines 29-31, 182-184, 1019-1021, 1097-1099, 1256-1257 use it), `internal/grpc/dispatch_txtoken_test.go:24, 38, 46`, `internal/grpc/scheduled_function_rpc_test.go:73` (add the selector argument)
- Modify: `cmd/cyoda/help/content/grpc.md` (TAG ROUTING, line 408 and the "chosen at random" sentence after it), `docs/ARCHITECTURE.md` §6.3 (line 1232), `CHANGELOG.md` `[Unreleased]` → `### Changed`
- Test: `internal/grpc/selector_test.go`

**Interfaces:**
- Consumes: `Candidates` (L-2).
- Produces:
  - `type MemberSelector interface { Select(candidates []*Member) *Member }` — `candidates` is never empty.
  - `type RoundRobinSelector struct{…}`, `func NewRoundRobinSelector(registry *MemberRegistry) *RoundRobinSelector`
  - `func NewProcessorDispatcher(registry *MemberRegistry, selector MemberSelector, uuids spi.UUIDGenerator, signer *token.Signer, selfNodeID string, tokenTTL time.Duration) *ProcessorDispatcher` (further changed by L-5 and L-10)
  - test helper `newTestDispatcher(t *testing.T, registry *MemberRegistry) *ProcessorDispatcher` (node id `"node-test"`)

`docs/ARCHITECTURE.md:608` (`registry.FindByTags` inside the "`ClusterDispatcher`
algorithm" block) is **not** touched here: that block describes code the
owner's-loop stream deletes, and §14 has it rewrite the dispatch section whole.

- [ ] **Step 1: Write the failing tests**

`internal/grpc/selector_test.go`:

```go
package grpc

import (
	"sync"
	"testing"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

func TestRoundRobinSelector_AlternatesAndANewMemberGoesFirst(t *testing.T) {
	reg := NewMemberRegistry()
	sel := NewRoundRobinSelector(reg)
	register := func(id string) {
		m := reg.Register(id, "tenant-1", []string{"x"}, noopSend, nil)
		t.Cleanup(func() { reg.Unregister(m) })
	}
	pick := func() string { return sel.Select(reg.Candidates("tenant-1", "x")).ID }

	register("m-1")
	register("m-2")
	for i, want := range []string{"m-1", "m-2", "m-1"} {
		if got := pick(); got != want {
			t.Fatalf("pick %d = %s, want %s", i, got, want)
		}
	}
	register("m-3") // never picked: goes first, though it attached last
	for i, want := range []string{"m-3", "m-2", "m-1", "m-3"} {
		if got := pick(); got != want {
			t.Fatalf("pick %d after m-3 attached = %s, want %s", i, got, want)
		}
	}
}

// RunLocal hands the selector only the cnodes it has not tried; the selector
// must choose among exactly those.
func TestRoundRobinSelector_ChoosesOnlyAmongWhatItIsGiven(t *testing.T) {
	reg := NewMemberRegistry()
	sel := NewRoundRobinSelector(reg)
	a := reg.Register("m-1", "tenant-1", []string{"x"}, noopSend, nil)
	b := reg.Register("m-2", "tenant-1", []string{"x"}, noopSend, nil)
	t.Cleanup(func() { reg.Unregister(a); reg.Unregister(b) })

	for i := 0; i < 3; i++ {
		if got := sel.Select([]*Member{b}); got != b {
			t.Fatalf("Select([m-2]) = %s", got.ID)
		}
	}
	if got := sel.Select(reg.Candidates("tenant-1", "x")); got != a {
		t.Fatalf("m-1 was never picked and must go first, got %s", got.ID)
	}
}

// Picking and stamping are one step: concurrent callouts share the cnodes
// evenly rather than all landing on the one that looked least recent.
func TestRoundRobinSelector_ConcurrentPicksAreSpreadEvenly(t *testing.T) {
	reg := NewMemberRegistry()
	sel := NewRoundRobinSelector(reg)
	a := reg.Register("m-1", "tenant-1", []string{"x"}, noopSend, nil)
	b := reg.Register("m-2", "tenant-1", []string{"x"}, noopSend, nil)
	t.Cleanup(func() { reg.Unregister(a); reg.Unregister(b) })

	var mu sync.Mutex
	picks := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := sel.Select(reg.Candidates("tenant-1", "x")).ID
			mu.Lock()
			defer mu.Unlock()
			picks[id]++
		}()
	}
	wg.Wait()
	if picks["m-1"] != 50 || picks["m-2"] != 50 {
		t.Fatalf("picks = %v, want 50 each", picks)
	}
}

func TestDispatchProcessor_RoundRobinAcrossTwoMembers(t *testing.T) {
	registry := NewMemberRegistry()
	var mu sync.Mutex
	got := map[string]int{}
	for _, id := range []string{"m-1", "m-2"} {
		m := registry.Register(id, testTenantID, []string{"python"}, func(ce *cepb.CloudEvent) error {
			reqID, err := extractRequestID(ce)
			if err != nil {
				t.Errorf("extractRequestID: %v", err)
				return nil
			}
			func() {
				mu.Lock()
				defer mu.Unlock()
				got[id]++
			}()
			registry.Get(id).CompleteRequest(reqID, &ProcessingResponse{Success: true})
			return nil
		}, nil)
		t.Cleanup(func() { registry.Unregister(m) })
	}
	dispatcher := newTestDispatcher(t, registry)
	processor := testProcessor("python", 5000)
	for i := 0; i < 4; i++ {
		if _, err := dispatcher.DispatchProcessor(testContext(), testEntity(), processor, "wf1", "t1", "tx-1"); err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if got["m-1"] != 2 || got["m-2"] != 2 {
		t.Fatalf("requests per member = %v, want 2 each", got)
	}
}
```

In `internal/grpc/dispatch_test.go` add, below `setupTestDispatcher`:

```go
// newTestDispatcher builds a dispatcher over registry the way app.go does, with
// node id "node-test".
func newTestDispatcher(t *testing.T, registry *MemberRegistry) *ProcessorDispatcher {
	t.Helper()
	signer, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("token.NewSigner: %v", err)
	}
	return NewProcessorDispatcher(registry, NewRoundRobinSelector(registry), common.NewTestUUIDGenerator(), signer, "node-test", time.Minute)
}

func testProcessor(tags string, responseTimeoutMs int64) spi.ProcessorDefinition {
	return spi.ProcessorDefinition{
		Name:   "my-proc",
		Config: spi.ProcessorConfig{CalculationNodesTags: tags, ResponseTimeoutMs: responseTimeoutMs},
	}
}
```

and replace the five hand-built dispatchers in that file
(`setupTestDispatcher`, `TestDispatchProcessor_NoMember`,
`TestDispatchFunction_NoMember`, `TestDispatchCalloutToMember_AbandonOnWriterFailure`,
`newWedgedDispatcher`) with `newTestDispatcher(t, registry)`, dropping their
now-unused `uuids` / `signer` locals.

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/grpc/... -run 'TestRoundRobinSelector|TestDispatchProcessor_RoundRobin'`   Expected: FAIL (build) with `undefined: NewRoundRobinSelector`.

- [ ] **Step 3: Implement**

`internal/grpc/selector.go`:

```go
package grpc

// MemberSelector decides which of several matching cnodes is tried next.
type MemberSelector interface {
	// Select picks one of candidates. candidates is never empty, and cnodes
	// already tried in the current run of the local procedure are not in it.
	Select(candidates []*Member) *Member
}

// RoundRobinSelector picks the candidate that was picked longest ago; among
// candidates never picked, the first in the order it was given. A cnode that
// has just attached has never been picked and therefore goes first.
//
// It keeps no state of its own. The stamp lives on the Member and the counter
// on the registry, so nothing grows with the number of tags — tag strings are
// supplied by tenants — and a cnode that goes away takes its stamp with it.
type RoundRobinSelector struct {
	registry *MemberRegistry
}

func NewRoundRobinSelector(registry *MemberRegistry) *RoundRobinSelector {
	return &RoundRobinSelector{registry: registry}
}

func (s *RoundRobinSelector) Select(candidates []*Member) *Member {
	return s.registry.pickLeastRecent(candidates)
}
```

`internal/grpc/members.go` — in `Member`, after `ConnectedAt`:

```go
	// pickStamp is the registry's pick counter at the moment this member was
	// last chosen for a try; 0 means never. Guarded by MemberRegistry.pickMu.
	pickStamp uint64
```

in `MemberRegistry`, after `publishedVersion`:

```go
	// pickMu makes "find the least recently picked and stamp it" one step.
	pickMu      sync.Mutex
	pickCounter uint64
```

and below `Candidates`:

```go
// pickLeastRecent returns the candidate with the lowest pick stamp — the first
// such in the order given — and stamps it with the next counter value.
func (r *MemberRegistry) pickLeastRecent(candidates []*Member) *Member {
	r.pickMu.Lock()
	defer r.pickMu.Unlock()
	best := candidates[0]
	for _, m := range candidates[1:] {
		if m.pickStamp < best.pickStamp {
			best = m
		}
	}
	r.pickCounter++
	best.pickStamp = r.pickCounter
	return best
}
```

`internal/grpc/dispatch.go` — struct and constructor:

```go
type ProcessorDispatcher struct {
	registry   *MemberRegistry
	selector   MemberSelector
	uuids      spi.UUIDGenerator
	signer     *token.Signer
	selfNodeID string
	tokenTTL   time.Duration
}

func NewProcessorDispatcher(registry *MemberRegistry, selector MemberSelector, uuids spi.UUIDGenerator, signer *token.Signer, selfNodeID string, tokenTTL time.Duration) *ProcessorDispatcher {
	return &ProcessorDispatcher{
		registry:   registry,
		selector:   selector,
		uuids:      uuids,
		signer:     signer,
		selfNodeID: selfNodeID,
		tokenTTL:   tokenTTL,
	}
}
```

and in the three methods `member := candidates[0]` → `member := d.selector.Select(candidates)`.

`app/app.go:441`:

```go
	localDispatcher := internalgrpc.NewProcessorDispatcher(a.memberRegistry, internalgrpc.NewRoundRobinSelector(a.memberRegistry), common.NewDefaultUUIDGenerator(), a.tokenSigner, a.selfNodeID, cfg.Cluster.TxTokenTTL)
```

In `dispatch_txtoken_test.go` (three sites) insert `NewRoundRobinSelector(reg)`
as the second argument, with `reg := NewMemberRegistry()` hoisted to a local; in
`scheduled_function_rpc_test.go:73` insert `NewRoundRobinSelector(registry)`.

Documentation. `cmd/cyoda/help/content/grpc.md`, replace the paragraph at line
408 and the sentence that follows it with:

```markdown
Among the members of the authenticated tenant whose tags match, the server picks **round robin**: the member that was picked longest ago goes next, and a member that has just joined has never been picked and goes first. Tag matching uses intersection: the member must declare at least one tag that appears in the callout's `calculationNodesTags`. A member of another tenant is never chosen, whatever its tags. Clients that need one particular member to receive a callout must give that member a tag of its own.

When `calculationNodesTags` is empty, every member of the authenticated tenant matches, and the same round robin applies.
```

`docs/ARCHITECTURE.md` §6.3, replace the paragraph at line 1232 with:

```markdown
`MemberRegistry.Candidates(tenantID, tagsCSV)` lists the tenant's members whose tags overlap the required tags (CSV comparison; every member of the tenant when `tagsCSV` is empty), ordered by `(ConnectedAt, ID)`. A `MemberSelector` picks one of them. `RoundRobinSelector` picks the member picked longest ago and stamps it from one counter on the registry; the stamp is a field on `Member`, so there is no per-tag state, and a member that has just attached goes first.
```

`CHANGELOG.md`, `[Unreleased]` → `### Changed`, add:

```markdown
- **A callout picks among a tenant's matching compute members round robin.**
  The member picked longest ago goes next; one that has just joined goes first.
  Until now the choice was whatever a Go map iteration returned first.
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/grpc/...`, `go build ./...`, `go vet ./internal/grpc/... ./app/...`   Expected: PASS / clean.
Exit check: `grep -n 'FindByTags\|chosen at random' cmd/cyoda/help/content/grpc.md` → no hits; `sed -n '1228,1236p' docs/ARCHITECTURE.md | grep -c FindByTags` → `0`.

- [ ] **Step 5: Commit**

```
git add internal/grpc/selector.go internal/grpc/selector_test.go internal/grpc/members.go internal/grpc/dispatch.go internal/grpc/dispatch_test.go internal/grpc/dispatch_txtoken_test.go internal/grpc/scheduled_function_rpc_test.go app/app.go cmd/cyoda/help/content/grpc.md docs/ARCHITECTURE.md CHANGELOG.md
git commit -m "feat(grpc): round-robin selection among a tenant's matching cnodes (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task L-4: `MemberRegistry.Changed()`

**Spec:** §5 "Patience": "a channel closed and replaced on every change (a cnode attaching or detaching locally)". The consumer is the owner's loop (another stream).

**Files:**
- Modify: `internal/grpc/members.go` (`MemberRegistry`, `NewMemberRegistry`, `Register`, `Unregister`)
- Test: `internal/grpc/members_changed_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func (r *MemberRegistry) Changed() <-chan struct{}` — the channel that will be closed on the next local attach or detach. A caller takes it **before** it looks at `Candidates`, so that a change between the look and the wait is not lost. An `Unregister` that removed nothing (a displaced member's deferred one) is not a change.

- [ ] **Step 1: Write the failing tests**

```go
package grpc

import "testing"

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestMemberRegistry_Changed_FiresOnAttachAndDetach(t *testing.T) {
	reg := NewMemberRegistry()

	before := reg.Changed()
	if isClosed(before) {
		t.Fatal("the channel must be open until something changes")
	}
	m := reg.Register("m-1", "tenant-1", []string{"x"}, noopSend, nil)
	if !isClosed(before) {
		t.Fatal("attach did not close the channel taken before it")
	}
	afterAttach := reg.Changed()
	if isClosed(afterAttach) {
		t.Fatal("the channel must be replaced, not left closed")
	}
	reg.Unregister(m)
	if !isClosed(afterAttach) {
		t.Fatal("detach did not close the channel taken before it")
	}
}

// Taking the channel before looking is what makes the wait race-free: a cnode
// that attaches after the look still wakes the waiter.
func TestMemberRegistry_Changed_TakenBeforeLooking_NoLostWakeUp(t *testing.T) {
	reg := NewMemberRegistry()
	ch := reg.Changed()
	if got := reg.Candidates("tenant-1", "x"); len(got) != 0 {
		t.Fatalf("expected no candidates yet, got %d", len(got))
	}
	m := reg.Register("m-1", "tenant-1", []string{"x"}, noopSend, nil)
	t.Cleanup(func() { reg.Unregister(m) })
	<-ch // returns at once; a lost wake-up would hang until the package timeout
	if got := reg.Candidates("tenant-1", "x"); len(got) != 1 {
		t.Fatalf("expected the new member to be a candidate, got %d", len(got))
	}
}

func TestMemberRegistry_Changed_UnregisterThatRemovedNothingIsNotAChange(t *testing.T) {
	reg := NewMemberRegistry()
	first := reg.Register("m-1", "tenant-1", nil, noopSend, nil)
	second := reg.Register("m-1", "tenant-1", nil, noopSend, nil) // displaces first
	t.Cleanup(func() { reg.Unregister(second) })

	ch := reg.Changed()
	reg.Unregister(first) // the displaced member's deferred Unregister
	if isClosed(ch) {
		t.Fatal("an Unregister that removed nothing must not signal a change")
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/grpc/... -run 'TestMemberRegistry_Changed'`   Expected: FAIL (build) with `reg.Changed undefined`.

- [ ] **Step 3: Implement**

`internal/grpc/members.go` — in `MemberRegistry`, after `onChange`:

```go
	// changed is closed and replaced, under mu, on every membership change. A
	// callout that found no cnode waits on it instead of polling.
	changed chan struct{}
```

`NewMemberRegistry`:

```go
func NewMemberRegistry() *MemberRegistry {
	return &MemberRegistry{
		members: make(map[string]*Member),
		changed: make(chan struct{}),
	}
}
```

below it:

```go
// Changed returns the channel that is closed on the next membership change — a
// cnode attaching or detaching on this pnode. Take it before looking at
// Candidates: a change between the look and the wait then still ends the wait.
func (r *MemberRegistry) Changed() <-chan struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.changed
}

// signalChangedLocked wakes every waiter and arms the next channel. Caller
// holds r.mu for writing.
func (r *MemberRegistry) signalChangedLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}
```

In `Register`'s locked section add `r.signalChangedLocked()` after
`r.tagsVersion++`; in `Unregister`'s locked section add it after its
`r.tagsVersion++` (the branch that actually removed the entry).

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/grpc/...`   Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/grpc/members.go internal/grpc/members_changed_test.go
git commit -m "feat(grpc): MemberRegistry.Changed signals a cnode attaching or detaching (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task L-5: the answer limit comes from configuration; a stored value over the bound is `Terminal`

**Spec:** §9 "The answer limit for a callout is …", "not quietly clamped … fails as `Terminal`, naming the setting"; D10. §13 row "Stored `responseTimeoutMs` over a lowered bound → `Terminal`" — **U** (and a G pin, since the envelope is visible from here).

**Files:**
- Modify: `internal/grpc/dispatch.go` (delete `defaultResponseTimeoutMs` :33 and the `timeoutMs <= 0` branch :122-124; struct, constructor; the three `Dispatch*`)
- Modify: `app/app.go:441`
- Modify: `internal/grpc/dispatch_test.go` (`newTestDispatcher`), `internal/grpc/dispatch_txtoken_test.go` (three sites), `internal/grpc/scheduled_function_rpc_test.go` (`newTestEnvWithDispatch` → delegates to `newTestEnvWithDispatchLimits`)
- Test: `internal/grpc/answer_limit_test.go`, `internal/grpc/callout_failure_rpc_test.go` (new; grows in L-7 and L-8)

**Interfaces:**
- Consumes (stream **C**): two `time.Duration` values on `app.Config` — the default answer limit (`CYODA_CALLOUT_RESPONSE_TIMEOUT_MS`) and its upper bound (`CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS`). They are `cfg.Callout.ResponseTimeout` and `cfg.Callout.ResponseTimeoutMax` (stream C's task C-1, as its plan's interface summary names them). Nothing else in this stream reads configuration. If C-5 has already replaced `DispatchCriteria`'s ad-hoc struct, `parsed.Function.Config.X` below reads `fn.Config.X`.
- Produces:
  - `func NewProcessorDispatcher(registry *MemberRegistry, selector MemberSelector, uuids spi.UUIDGenerator, signer *token.Signer, selfNodeID string, tokenTTL, answerLimitDefault, answerLimitMax time.Duration) *ProcessorDispatcher`
  - `func (d *ProcessorDispatcher) ResolveAnswerLimit(responseTimeoutMs int64) (time.Duration, *contract.CalloutFailure)` — the owner calls it once per callout and sends the result with a hand-over; a pnode that receives a hand-over does **not** call it (it uses the owner's value).

- [ ] **Step 1: Write the failing tests**

`internal/grpc/answer_limit_test.go`:

```go
package grpc

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func TestResolveAnswerLimit(t *testing.T) {
	d := newTestDispatcher(t, NewMemberRegistry()) // default 30s, upper bound 60s
	tests := []struct {
		name         string
		storedMs     int64
		want         time.Duration
		wantTerminal bool
	}{
		{"unset uses the default", 0, 30 * time.Second, false},
		{"negative uses the default", -5, 30 * time.Second, false},
		{"a stored value is used as it is", 1500, 1500 * time.Millisecond, false},
		{"exactly the bound is allowed", 60_000, 60 * time.Second, false},
		{"one over the bound is refused, not clamped", 60_001, 0, true},
		{"a value that would overflow a Duration is refused", math.MaxInt64, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, failure := d.ResolveAnswerLimit(tt.storedMs)
			if tt.wantTerminal {
				if failure == nil || failure.Kind != contract.Terminal {
					t.Fatalf("failure = %+v, want Terminal", failure)
				}
				if !strings.Contains(failure.Message, "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS") {
					t.Errorf("message %q must name the setting", failure.Message)
				}
				var appErr *common.AppError
				if errors.As(failure, &appErr) {
					t.Errorf("must carry no AppError (it is a 400 WORKFLOW_FAILED by the catch-all), got %v", appErr)
				}
				return
			}
			if failure != nil {
				t.Fatalf("unexpected failure: %v", failure)
			}
			if got != tt.want {
				t.Errorf("limit = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDispatch_StoredTimeoutOverTheBound_IsTerminalAndNothingIsSent(t *testing.T) {
	registry := NewMemberRegistry()
	sent := make(chan struct{}, 1)
	m := registry.Register("m-1", testTenantID, []string{"python"}, func(*cepb.CloudEvent) error {
		sent <- struct{}{}
		return nil
	}, nil)
	t.Cleanup(func() { registry.Unregister(m) })
	d := newTestDispatcher(t, registry)

	_, err := d.DispatchProcessor(testContext(), testEntity(), testProcessor("python", 60_001), "wf1", "t1", "tx-1")
	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) || failure.Kind != contract.Terminal {
		t.Fatalf("err = %v, want a Terminal CalloutFailure", err)
	}
	select {
	case <-sent:
		t.Fatal("a callout over the bound must not reach a cnode")
	default:
	}
}
```

`internal/grpc/callout_failure_rpc_test.go`:

```go
package grpc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// processorRPCWorkflowJSON is one automated transition with one SYNC
// externalized processor.
func processorRPCWorkflowJSON(wfName, procName, tag string, responseTimeoutMs int64) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "init", "next": "Closed", "manual": false,
					"processors": [{"type": "calculator", "name": %q, "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": %q, "responseTimeoutMs": %d}}]
				}]},
				"Closed": {}
			}
		}]
	}`, wfName, procName, tag, responseTimeoutMs)
}

// A workflow imported under a higher bound, run on a server whose bound was
// lowered since: the callout fails, naming the setting, and no cnode is asked.
func TestRPC_Processor_StoredTimeoutOverLoweredBound_Envelope(t *testing.T) {
	const modelName = "grpc-proc-over-bound"
	const tag = "over-bound-tag"
	svc, wfHandler, ctx := newTestEnvWithDispatchLimits(t, 500*time.Millisecond, time.Second)
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
		processorRPCWorkflowJSON("over-bound-wf", "slow-proc", tag, 5000))

	sent := make(chan struct{}, 1)
	svc.registry.Register("m-1", testTenant, []string{tag}, func(*cepb.CloudEvent) error {
		sent <- struct{}{}
		return nil
	}, nil)

	typed := createScheduledEntity(t, svc, ctx, modelName)
	assertClientErrorEnvelope(t, typed, "WORKFLOW_FAILED")
	if !strings.Contains(typed.Error.Message, "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS") {
		t.Errorf("message must name the setting, got %s", typed.Error.Message)
	}
	if typed.Error.Retryable != nil && *typed.Error.Retryable {
		t.Error("a Terminal failure is not retryable")
	}
	select {
	case <-sent:
		t.Fatal("no cnode may be asked")
	default:
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/grpc/... -run 'TestResolveAnswerLimit|TestDispatch_StoredTimeoutOverTheBound|TestRPC_Processor_StoredTimeoutOverLoweredBound'`   Expected: FAIL (build) with `d.ResolveAnswerLimit undefined` and `undefined: newTestEnvWithDispatchLimits`.

- [ ] **Step 3: Implement**

`internal/grpc/dispatch.go` — delete `const defaultResponseTimeoutMs = 30000`. Struct and constructor:

```go
type ProcessorDispatcher struct {
	registry           *MemberRegistry
	selector           MemberSelector
	uuids              spi.UUIDGenerator
	signer             *token.Signer
	selfNodeID         string
	tokenTTL           time.Duration
	answerLimitDefault time.Duration
	answerLimitMax     time.Duration
}

func NewProcessorDispatcher(registry *MemberRegistry, selector MemberSelector, uuids spi.UUIDGenerator, signer *token.Signer, selfNodeID string, tokenTTL, answerLimitDefault, answerLimitMax time.Duration) *ProcessorDispatcher {
	return &ProcessorDispatcher{
		registry:           registry,
		selector:           selector,
		uuids:              uuids,
		signer:             signer,
		selfNodeID:         selfNodeID,
		tokenTTL:           tokenTTL,
		answerLimitDefault: answerLimitDefault,
		answerLimitMax:     answerLimitMax,
	}
}

// ResolveAnswerLimit gives the answer limit of a callout: its stored
// responseTimeoutMs if positive, else the configured default. A stored value
// above the configured upper bound — possible when the bound was lowered after
// the workflow was imported — is not clamped: the callout fails as Terminal. A
// substituted limit would be a wrong-but-available answer.
func (d *ProcessorDispatcher) ResolveAnswerLimit(responseTimeoutMs int64) (time.Duration, *contract.CalloutFailure) {
	if responseTimeoutMs <= 0 {
		return d.answerLimitDefault, nil
	}
	// Compared in milliseconds: a huge stored value would overflow a Duration.
	if responseTimeoutMs > d.answerLimitMax.Milliseconds() {
		err := fmt.Errorf("responseTimeoutMs %d exceeds the upper bound of %d ms set by CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS",
			responseTimeoutMs, d.answerLimitMax.Milliseconds())
		return 0, &contract.CalloutFailure{Kind: contract.Terminal, Message: err.Error(), Err: err}
	}
	return time.Duration(responseTimeoutMs) * time.Millisecond, nil
}
```

In `dispatchCalloutToMember` delete:

```go
	if timeoutMs <= 0 {
		timeoutMs = defaultResponseTimeoutMs
	}
```

In each `Dispatch*`, resolve the limit **before** looking for a cnode (it would
fail identically anywhere), and pass it on. `DispatchProcessor`, directly after
`tenantID := uc.Tenant.ID`:

```go
	limit, failure := d.ResolveAnswerLimit(processor.Config.ResponseTimeoutMs)
	if failure != nil {
		return nil, failure
	}
```

and the call becomes `d.dispatchCalloutToMember(ctx, member, EntityProcessorCalculationRequest, req, requestID, txID, limit.Milliseconds(), "processor", processor.Name)`.
`DispatchCriteria`: the same after the criterion is parsed, with
`parsed.Function.Config.ResponseTimeoutMs` and `return false, "", failure`.
`DispatchFunction`: the same with `fn.ResponseTimeoutMs` and
`return contract.FunctionResult{}, failure`.

`app/app.go:441`:

```go
	localDispatcher := internalgrpc.NewProcessorDispatcher(a.memberRegistry, internalgrpc.NewRoundRobinSelector(a.memberRegistry), common.NewDefaultUUIDGenerator(), a.tokenSigner, a.selfNodeID, cfg.Cluster.TxTokenTTL, cfg.Callout.ResponseTimeout, cfg.Callout.ResponseTimeoutMax)
```

Tests: `newTestDispatcher` and the three `dispatch_txtoken_test.go` sites append
`30*time.Second, 60*time.Second`. In `scheduled_function_rpc_test.go` rename the
body of `newTestEnvWithDispatch` to

```go
func newTestEnvWithDispatchLimits(t *testing.T, answerLimitDefault, answerLimitMax time.Duration) (*CloudEventsServiceImpl, *workflow.Handler, context.Context) {
```

with its dispatcher line reading

```go
	dispatcher := NewProcessorDispatcher(registry, NewRoundRobinSelector(registry), common.NewDefaultUUIDGenerator(), signer, "node-test", time.Minute, answerLimitDefault, answerLimitMax)
```

and keep the old name as

```go
func newTestEnvWithDispatch(t *testing.T) (*CloudEventsServiceImpl, *workflow.Handler, context.Context) {
	t.Helper()
	return newTestEnvWithDispatchLimits(t, 30*time.Second, 60*time.Second)
}
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/grpc/...`, `go build ./...`   Expected: PASS.
Exit check: `grep -rn 'defaultResponseTimeoutMs' --include='*.go' .` → no hits.

- [ ] **Step 5: Commit**

```
git add internal/grpc/dispatch.go internal/grpc/answer_limit_test.go internal/grpc/callout_failure_rpc_test.go internal/grpc/dispatch_test.go internal/grpc/dispatch_txtoken_test.go internal/grpc/scheduled_function_rpc_test.go app/app.go
git commit -m "feat(grpc): the answer limit is configured; a stored value over the bound fails the callout (#254, #565)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task L-6: `Callout` and its three builders

**Spec:** §4 "`Callout` holds what the three entry points build today — kind, tenant, tags, the request builder, the response mapper — plus `RequestID`, `AnswerLimit`, `RepeatSafe`, `OwnerNodeID`, `TxID` …". (`Number` arrives with `RunLocal` in L-8, `Outer` with pass minting in L-10.)

**Files:**
- Create: `internal/grpc/callout.go`
- Modify: `internal/grpc/dispatch.go` (`DispatchProcessor` :206-246, `applyProcessorResponse` :249-269, `DispatchCriteria` :273-341, `DispatchFunction` :346-383 — line numbers as before L-2)
- Test: `internal/grpc/callout_test.go`

**Interfaces:**
- Consumes: `ResolveAnswerLimit` (L-5), `Candidates` (L-2), `MemberSelector` (L-3), `contract.CalloutFailure` (L-1); from stream **C** (C-5): `contract.ParseCriterionFunction(criterion json.RawMessage) (contract.CriterionFunction, error)` with `CriterionFunction{Name string; Config CriterionFunctionConfig{CalculationNodesTags string; AttachEntity *bool; ResponseTimeoutMs int64; RetryPolicy string; Context string}}`. Two parsers of one envelope would be two ways of doing one thing; exit check `grep -rn 'var parsed struct' internal/grpc/` → no hits.
- Produces (the owner's-loop stream and the hand-over stream **P** call these; they are the only way to make a `Callout`):
  - `type CalloutKind int` — `ProcessorCallout, CriteriaCallout, FunctionCallout`; `String()` gives `"processor" | "criteria" | "function"`, the words `DispatchCalloutRequest.Kind` already uses.
  - `type CalloutResult struct { Entity *spi.Entity; Matches bool; Reason string; Function contract.FunctionResult }` — the field for the callout's kind is set.
  - `type Callout struct` with exported fields `Kind CalloutKind; Name string; TenantID spi.TenantID; Tags string; ResponseTimeoutMs int64; TxID string; EntityID string; RequestID string; AnswerLimit time.Duration; RepeatSafe bool; OwnerNodeID string` and unexported `eventType`, `buildRequest`, `mapResponse`.
  - `func NewProcessorCallout(tenantID spi.TenantID, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName, transitionName, txID string) Callout` — `RepeatSafe` is **false**; the owner sets it from `processor.Config.Idempotent` once the SPI field exists (D2, another stream).
  - `func NewCriteriaCallout(tenantID spi.TenantID, entity *spi.Entity, criterion json.RawMessage, target, workflowName, transitionName, processorName, txID string) (Callout, *contract.CalloutFailure)` — `RepeatSafe` true; a criterion that does not parse is `Terminal`.
  - `func NewFunctionCallout(tenantID spi.TenantID, entity *spi.Entity, fn spi.ScheduleFunction, workflowName, transitionName, txID string) Callout` — `RepeatSafe` true.
  - The builders fill `Kind, Name, TenantID, Tags, ResponseTimeoutMs, TxID, EntityID, RepeatSafe` and the unexported parts. The **caller** fills `RequestID`, `AnswerLimit` (owner: `ResolveAnswerLimit(call.ResponseTimeoutMs)`; peer: the value the hand-over carried), `OwnerNodeID`, and later `Number` and `Outer`.

This task changes no behaviour: the existing `dispatch_test.go` is the safety
net, the new tests pin the builders.

- [ ] **Step 1: Write the failing tests**

`internal/grpc/callout_test.go`:

```go
package grpc

import (
	"encoding/json"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func requestAsMap(t *testing.T, call Callout, requestID string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(call.buildRequest(requestID))
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	return m
}

func TestNewProcessorCallout(t *testing.T) {
	entity := testEntity()
	processor := spi.ProcessorDefinition{
		Name: "charge",
		Config: spi.ProcessorConfig{
			AttachEntity: true, CalculationNodesTags: "python,ml", ResponseTimeoutMs: 1500, Context: "role-a",
		},
	}
	call := NewProcessorCallout(testTenantID, entity, processor, "wf1", "t1", "tx-1")

	if call.Kind != ProcessorCallout || call.Kind.String() != "processor" {
		t.Errorf("Kind = %v", call.Kind)
	}
	if call.Name != "charge" || call.TenantID != testTenantID || call.Tags != "python,ml" ||
		call.ResponseTimeoutMs != 1500 || call.TxID != "tx-1" || call.EntityID != entity.Meta.ID {
		t.Errorf("descriptive fields wrong: %+v", call)
	}
	if call.RepeatSafe {
		t.Error("a processor is not repeat-safe unless its owner says so")
	}
	if call.eventType != EntityProcessorCalculationRequest {
		t.Errorf("eventType = %s", call.eventType)
	}

	req := requestAsMap(t, call, "rid-7")
	if req["id"] != "rid-7" || req["requestId"] != "rid-7" {
		t.Errorf("id/requestId = %v/%v, want the request id given", req["id"], req["requestId"])
	}
	if req["processorName"] != "charge" || req["parameters"] != "role-a" || req["transactionId"] != "tx-1" {
		t.Errorf("request fields wrong: %v", req)
	}
	if _, ok := req["payload"]; !ok {
		t.Error("attachEntity=true must attach the payload")
	}

	// the mapper keeps today's three cases
	same, err := call.mapResponse(&ProcessingResponse{Success: true})
	if err != nil || same.Entity != entity {
		t.Errorf("no payload: got (%v, %v), want the entity unchanged", same.Entity, err)
	}
	updated, err := call.mapResponse(&ProcessingResponse{Success: true, Payload: json.RawMessage(`{"data":{"foo":"new"}}`)})
	if err != nil || string(updated.Entity.Data) != `{"foo":"new"}` || updated.Entity.Meta.ID != entity.Meta.ID {
		t.Errorf("payload: got (%+v, %v)", updated.Entity, err)
	}
	if _, err := call.mapResponse(&ProcessingResponse{Success: true, Payload: json.RawMessage(`{"data":`)}); err == nil {
		t.Error("a payload that does not parse must be an error")
	}
}

func TestNewProcessorCallout_NoContextNoPayload(t *testing.T) {
	call := NewProcessorCallout(testTenantID, testEntity(), testProcessor("python", 0), "wf1", "t1", "tx-1")
	req := requestAsMap(t, call, "rid")
	if _, ok := req["parameters"]; ok {
		t.Error("an empty context must omit parameters")
	}
	if _, ok := req["payload"]; ok {
		t.Error("attachEntity=false must omit the payload")
	}
}

func TestNewCriteriaCallout(t *testing.T) {
	criterion := json.RawMessage(`{"type":"function","function":{"name":"amount-check",
		"config":{"calculationNodesTags":"python","responseTimeoutMs":700,"context":"ctx-1"}}}`)
	call, failure := NewCriteriaCallout(testTenantID, testEntity(), criterion, "transition", "wf1", "t1", "proc-1", "tx-1")
	if failure != nil {
		t.Fatalf("unexpected failure: %v", failure)
	}
	if call.Kind != CriteriaCallout || call.Name != "amount-check" || call.Tags != "python" || call.ResponseTimeoutMs != 700 {
		t.Errorf("descriptive fields wrong: %+v", call)
	}
	if !call.RepeatSafe {
		t.Error("a criterion is repeat-safe by rule")
	}
	req := requestAsMap(t, call, "rid-9")
	if req["id"] != "rid-9" || req["requestId"] != "rid-9" || req["criteriaName"] != "amount-check" ||
		req["target"] != "transition" || req["parameters"] != "ctx-1" {
		t.Errorf("request fields wrong: %v", req)
	}
	if _, ok := req["payload"]; !ok {
		t.Error("attachEntity defaults to true for a criterion")
	}
	if proc, _ := req["processor"].(map[string]any); proc["name"] != "proc-1" {
		t.Errorf("processor = %v", req["processor"])
	}

	yes := true
	got, err := call.mapResponse(&ProcessingResponse{Success: true, Matches: &yes, Reason: "big"})
	if err != nil || !got.Matches || got.Reason != "big" {
		t.Errorf("mapResponse = (%+v, %v)", got, err)
	}
	got, err = call.mapResponse(&ProcessingResponse{Success: true, Reason: "none given"})
	if err != nil || got.Matches || got.Reason != "none given" {
		t.Errorf("absent matches must read as false: (%+v, %v)", got, err)
	}
}

func TestNewCriteriaCallout_InvalidJSONIsTerminal(t *testing.T) {
	_, failure := NewCriteriaCallout(testTenantID, testEntity(), json.RawMessage(`{"function":`), "transition", "wf1", "t1", "", "tx-1")
	if failure == nil || failure.Kind != contract.Terminal {
		t.Fatalf("failure = %+v, want Terminal", failure)
	}
	if !strings.HasPrefix(failure.Error(), "invalid criterion JSON: ") {
		t.Errorf("message = %q, want today's text", failure.Error())
	}
}

func TestNewFunctionCallout(t *testing.T) {
	fn := spi.ScheduleFunction{Name: "calcFire", ResultKind: "Schedule", CalculationNodesTags: "sched", AttachEntity: true, ResponseTimeoutMs: 900, Context: "c"}
	call := NewFunctionCallout(testTenantID, testEntity(), fn, "wf1", "t1", "tx-1")
	if call.Kind != FunctionCallout || call.Name != "calcFire" || call.Tags != "sched" || call.ResponseTimeoutMs != 900 || !call.RepeatSafe {
		t.Errorf("descriptive fields wrong: %+v", call)
	}
	req := requestAsMap(t, call, "rid-3")
	if req["id"] != "rid-3" || req["requestId"] != "rid-3" || req["functionName"] != "calcFire" || req["parameters"] != "c" {
		t.Errorf("request fields wrong: %v", req)
	}
	got, err := call.mapResponse(&ProcessingResponse{Success: true, ResultKind: "Schedule", Result: json.RawMessage(`{"fireAfterMs":1000}`)})
	if err != nil || got.Function.Kind != "Schedule" || string(got.Function.Value) != `{"fireAfterMs":1000}` {
		t.Errorf("mapResponse = (%+v, %v)", got, err)
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/grpc/... -run 'TestNew(Processor|Criteria|Function)Callout'`   Expected: FAIL (build) with `undefined: NewProcessorCallout`.

- [ ] **Step 3: Implement**

`internal/grpc/callout.go`:

```go
package grpc

import (
	"encoding/json"
	"fmt"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// CalloutKind is which of the three callouts a Callout is.
type CalloutKind int

const (
	ProcessorCallout CalloutKind = iota
	CriteriaCallout
	FunctionCallout
)

// String is the word used for the kind in diagnostics a client sees
// ("processor charge: …") and in the hand-over between pnodes.
func (k CalloutKind) String() string {
	switch k {
	case ProcessorCallout:
		return "processor"
	case CriteriaCallout:
		return "criteria"
	case FunctionCallout:
		return "function"
	default:
		return fmt.Sprintf("CalloutKind(%d)", int(k))
	}
}

// CalloutResult is what a cnode answered; the field for the callout's kind is
// set.
type CalloutResult struct {
	Entity   *spi.Entity             // processor
	Matches  bool                    // criterion
	Reason   string                  // criterion
	Function contract.FunctionResult // function
}

// Callout is one processor, criterion or function request, in the form the
// local procedure tries on cnodes. Build it with NewProcessorCallout,
// NewCriteriaCallout or NewFunctionCallout; the caller then fills RequestID,
// AnswerLimit and OwnerNodeID.
type Callout struct {
	Kind     CalloutKind
	Name     string // the configured processor, criterion or function name
	TenantID spi.TenantID
	Tags     string // calculationNodesTags, comma separated
	// ResponseTimeoutMs is the value stored in the workflow; the owner turns it
	// into AnswerLimit with ResolveAnswerLimit.
	ResponseTimeoutMs int64
	TxID              string // empty when the callout runs outside a transaction
	EntityID          string

	// RequestID is sent on every try, as the request's id and requestId.
	RequestID string
	// AnswerLimit is how long one cnode is given to take the work and answer.
	AnswerLimit time.Duration
	// RepeatSafe: the work may be given to another cnode after a hand-off.
	RepeatSafe bool
	// OwnerNodeID is the pnode that holds the transaction; a cnode's callbacks
	// are routed there.
	OwnerNodeID string

	eventType    string
	buildRequest func(requestID string) any
	mapResponse  func(resp *ProcessingResponse) (CalloutResult, error)
}

// NewProcessorCallout builds the callout for an externalized processor.
// RepeatSafe is left false: whether a processor may be repeated is its
// author's declaration, which the owner reads from the processor's config.
func NewProcessorCallout(tenantID spi.TenantID, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName, transitionName, txID string) Callout {
	return Callout{
		Kind:              ProcessorCallout,
		Name:              processor.Name,
		TenantID:          tenantID,
		Tags:              processor.Config.CalculationNodesTags,
		ResponseTimeoutMs: processor.Config.ResponseTimeoutMs,
		TxID:              txID,
		EntityID:          entity.Meta.ID,
		eventType:         EntityProcessorCalculationRequest,
		buildRequest: func(requestID string) any {
			req := events.EntityProcessorCalculationRequestJson{
				ID:            requestID,
				RequestID:     requestID,
				EntityID:      entity.Meta.ID,
				ProcessorID:   processor.Name,
				ProcessorName: processor.Name,
				Workflow:      events.WorkflowInfoJson{ID: workflowName, Name: workflowName},
				Transition:    &events.TransitionInfoJson{ID: transitionName, Name: transitionName},
				TransactionID: &txID,
				Success:       true,
			}
			// ProcessorConfig.Context is a pass-through string surfaced verbatim
			// in the request's parameters node. One processor implementation can
			// serve multiple workflow roles distinguished by Context.
			if processor.Config.Context != "" {
				req.Parameters = processor.Config.Context
			}
			if processor.Config.AttachEntity {
				req.Payload = buildEntityPayload(entity)
			}
			return req
		},
		mapResponse: func(resp *ProcessingResponse) (CalloutResult, error) {
			updated, err := applyProcessorResponse(entity, resp)
			if err != nil {
				return CalloutResult{}, err
			}
			return CalloutResult{Entity: updated}, nil
		},
	}
}

// NewCriteriaCallout builds the callout for a FUNCTION criterion. A criterion
// computes and does not write, so it is repeat-safe by rule. A criterion that
// does not parse would fail identically on any cnode: Terminal.
func NewCriteriaCallout(tenantID spi.TenantID, entity *spi.Entity, criterion json.RawMessage, target, workflowName, transitionName, processorName, txID string) (Callout, *contract.CalloutFailure) {
	// One parser serves import validation and dispatch alike.
	fn, err := contract.ParseCriterionFunction(criterion)
	if err != nil {
		wrapped := fmt.Errorf("invalid criterion JSON: %w", err)
		return Callout{}, &contract.CalloutFailure{Kind: contract.Terminal, Message: wrapped.Error(), Err: wrapped}
	}
	name := fn.Name
	config := fn.Config
	attachEntity := config.AttachEntity == nil || *config.AttachEntity

	return Callout{
		Kind:              CriteriaCallout,
		Name:              name,
		TenantID:          tenantID,
		Tags:              config.CalculationNodesTags,
		ResponseTimeoutMs: config.ResponseTimeoutMs,
		TxID:              txID,
		EntityID:          entity.Meta.ID,
		RepeatSafe:        true,
		eventType:         EntityCriteriaCalculationRequest,
		buildRequest: func(requestID string) any {
			req := events.EntityCriteriaCalculationRequestJson{
				ID:            requestID,
				RequestID:     requestID,
				EntityID:      entity.Meta.ID,
				CriteriaID:    name,
				CriteriaName:  name,
				Target:        events.EntityCriteriaCalculationRequestJsonTarget(target),
				Workflow:      &events.WorkflowInfoJson{ID: workflowName, Name: workflowName},
				Transition:    &events.TransitionInfoJson{ID: transitionName, Name: transitionName},
				TransactionID: &txID,
				Success:       true,
			}
			if processorName != "" {
				req.Processor = &events.ProcessorInfoJson{Name: processorName}
			}
			if config.Context != "" {
				req.Parameters = config.Context
			}
			if attachEntity {
				req.Payload = buildEntityPayload(entity)
			}
			return req
		},
		mapResponse: func(resp *ProcessingResponse) (CalloutResult, error) {
			return CalloutResult{Matches: resp.Matches != nil && *resp.Matches, Reason: resp.Reason}, nil
		},
	}, nil
}

// NewFunctionCallout builds the callout for a generic Function (e.g. a
// scheduled transition's timing computation). Repeat-safe by rule.
func NewFunctionCallout(tenantID spi.TenantID, entity *spi.Entity, fn spi.ScheduleFunction, workflowName, transitionName, txID string) Callout {
	return Callout{
		Kind:              FunctionCallout,
		Name:              fn.Name,
		TenantID:          tenantID,
		Tags:              fn.CalculationNodesTags,
		ResponseTimeoutMs: fn.ResponseTimeoutMs,
		TxID:              txID,
		EntityID:          entity.Meta.ID,
		RepeatSafe:        true,
		eventType:         EntityFunctionCalculationRequest,
		buildRequest: func(requestID string) any {
			req := events.EntityFunctionCalculationRequestJson{
				ID:            requestID,
				RequestID:     requestID,
				EntityID:      entity.Meta.ID,
				FunctionID:    fn.Name,
				FunctionName:  fn.Name,
				Workflow:      events.WorkflowInfoJson{ID: workflowName, Name: workflowName},
				Transition:    &events.TransitionInfoJson{ID: transitionName, Name: transitionName},
				TransactionID: &txID,
				Success:       true,
			}
			if fn.Context != "" {
				req.Parameters = fn.Context
			}
			if fn.AttachEntity {
				req.Payload = buildEntityPayload(entity)
			}
			return req
		},
		mapResponse: func(resp *ProcessingResponse) (CalloutResult, error) {
			return CalloutResult{Function: contract.FunctionResult{Kind: resp.ResultKind, Value: resp.Result}}, nil
		},
	}
}
```

`internal/grpc/dispatch.go` — `applyProcessorResponse` loses its receiver
(`func applyProcessorResponse(entity *spi.Entity, resp *ProcessingResponse) (*spi.Entity, error)`,
body unchanged). The three methods and one transitional helper (replaced by
`runSingleTry` in L-8) become:

```go
// dispatchOnce makes the single try the three Dispatch* methods make.
func (d *ProcessorDispatcher) dispatchOnce(ctx context.Context, call Callout) (CalloutResult, error) {
	limit, failure := d.ResolveAnswerLimit(call.ResponseTimeoutMs)
	if failure != nil {
		return CalloutResult{}, failure
	}
	candidates := d.registry.Candidates(call.TenantID, call.Tags)
	if len(candidates) == 0 {
		slog.Warn("no matching calculation member", "pkg", "grpc", "tags", call.Tags, "entityId", call.EntityID)
		return CalloutResult{}, fmt.Errorf("%w: tags %q", ErrNoMatchingMember, call.Tags)
	}
	member := d.selector.Select(candidates)
	call.RequestID = uuid.UUID(d.uuids.NewTimeUUID()).String()
	call.AnswerLimit = limit
	call.OwnerNodeID = d.selfNodeID

	slog.Info("dispatching "+call.Kind.String(), "pkg", "grpc", "memberId", member.ID, "name", call.Name, "entityId", call.EntityID)
	resp, err := d.dispatchCalloutToMember(ctx, member, call.eventType, call.buildRequest(call.RequestID), call.RequestID, call.TxID, limit.Milliseconds(), call.Kind.String(), call.Name)
	if err != nil {
		return CalloutResult{}, err
	}
	return call.mapResponse(resp)
}

// DispatchProcessor sends an entity processor calculation request to a matching
// calculation member and waits for the response.
func (d *ProcessorDispatcher) DispatchProcessor(ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName string, transitionName string, txID string) (*spi.Entity, error) {
	uc := spi.MustGetUserContext(ctx)
	res, err := d.dispatchOnce(ctx, NewProcessorCallout(uc.Tenant.ID, entity, processor, workflowName, transitionName, txID))
	if err != nil {
		return nil, err
	}
	return res.Entity, nil
}

// DispatchCriteria sends an entity criteria calculation request to a matching
// calculation member and waits for the boolean result.
func (d *ProcessorDispatcher) DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target string, workflowName string, transitionName string, processorName string, txID string) (bool, string, error) {
	uc := spi.MustGetUserContext(ctx)
	call, failure := NewCriteriaCallout(uc.Tenant.ID, entity, criterion, target, workflowName, transitionName, processorName, txID)
	if failure != nil {
		return false, "", failure
	}
	res, err := d.dispatchOnce(ctx, call)
	if err != nil {
		return false, "", err
	}
	return res.Matches, res.Reason, nil
}

// DispatchFunction sends a generic Function calculation request (e.g. a
// scheduled-transition timing computation) to a matching calculation member
// and returns its typed result.
func (d *ProcessorDispatcher) DispatchFunction(ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction, workflowName string, transitionName string, txID string) (contract.FunctionResult, error) {
	uc := spi.MustGetUserContext(ctx)
	res, err := d.dispatchOnce(ctx, NewFunctionCallout(uc.Tenant.ID, entity, fn, workflowName, transitionName, txID))
	if err != nil {
		return contract.FunctionResult{}, err
	}
	return res.Function, nil
}
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/grpc/...`, `go vet ./internal/grpc/...`   Expected: PASS — the whole of `dispatch_test.go`, `scheduled_function_rpc_test.go` and `workflow_selection_test.go` unchanged.

- [ ] **Step 5: Commit**

```
git add internal/grpc/callout.go internal/grpc/callout_test.go internal/grpc/dispatch.go
git commit -m "refactor(grpc): one Callout type, built by three builders, behind the three entry points (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task L-7: every error site of a try gets its kind

**Spec:** §3 — the site table in full, "`disconnectedErr` and the two `DISPATCH_TIMEOUT` constructions keep their codes, statuses and messages; the kind travels beside them", and the shared enqueue/response deadline. §8.2: "Today's inner `processor dispatch failed:` segment goes". §13 rows "Every dispatcher error site → its kind" — **U**; "`MemberFailed` verdict true / false / absent" — **U** at the try, **G** for code and message.

**Files:**
- Modify: `internal/grpc/dispatch.go` (`dispatchCalloutToMember` :109-194, `disconnectedErr` :199-202, `dispatchOnce`)
- Modify: `internal/grpc/members.go` (stale comment on `ProcessingResponse.Retryable`, lines 48-55 — §14)
- Modify: `internal/grpc/dispatch_test.go` (new helper `tryOnce`; thirteen direct callers)
- Modify: `CHANGELOG.md` `[Unreleased]` → `### Changed`
- Test: `internal/grpc/try_kind_test.go`, `internal/grpc/callout_failure_rpc_test.go`

**Interfaces:**
- Consumes: `Callout` (L-6), `contract.CalloutFailure` (L-1).
- Produces (in-package): `func (d *ProcessorDispatcher) dispatchCalloutToMember(ctx context.Context, member *Member, call Callout, pass string) (CalloutResult, *contract.CalloutFailure, error)` — exactly one of the three is meaningful: the mapped result; the classified failure; or `ctx.Err()`, unchanged, when the **caller's** context ended. For the kinds that have one, `failure.Err` is the same `*common.AppError` as today and `failure.Code` / `failure.Message` repeat its `Code` / `Message` (the message therefore starts with `"CODE: "`).

The name `dispatchCalloutToMember` is kept — it still says what the function
does — so the comments that cite it (`members.go:59, 281`,
`scheduled_function_rpc_test.go:240`, `internal/e2e/scheduled_function_test.go:394, 402`)
stay true.

**Existing tests — decision for each** (R§7's list, plus the other direct callers):

| Test (`dispatch_test.go`) | Decision |
|---|---|
| `TestDispatchProcessor_NoMember` :180, `TestDispatchFunction_NoMember` :1017 | stay, unchanged — `errors.Is(err, ErrNoMatchingMember)` still holds |
| `TestDispatchProcessor_Timeout` :204 | stays, unchanged — code, status, retryable and the exact message are kept |
| `TestDispatchCriteria_FailurePropagatesName` :915, `TestDispatchProcessor_WarningPropagatesName` :869 | stay, unchanged — diagnostics are still keyed `"<kind> <name>: …"` |
| `TestDispatchCalloutToMember_SuccessAndTimeout` :650, `_MemberDisconnects` :707, `_NilUserContext` :743, `_UnsetKind` :761, `_InvalidKind` :785, `_KindDrivenAuthType` :811, `_AbandonOnCtxCancel` :1045, `_AbandonOnTimeout` :1071, `_AbandonOnWriterFailure` :1092 | rewritten to call `tryOnce` (below); every assertion stays. The `req := map[string]any{…}` local in each is deleted with the call that used it |
| `TestDispatch_MemberGoneBeforeTrack_IsDisconnectedImmediately` :1167, `_EnqueueTimeout_IsDispatchTimeoutNotDraining` :1187, `_ParentCancelDuringEnqueue_IsCtxErr` :1207, `_MemberEvictedDuringEnqueue_IsDisconnectedPromptly` :1224 | rewritten to call `tryOnce`; every assertion stays |
| `frozen_member_test.go` `TestFrozenMember_IsEvictedAndDispatchersAreReleased` :24, `TestBlackholedConnection_IsTornDownByTransportKeepalive` :206 | stay, unchanged — they drive `Member.Send` and the keep-alive loop, never the dispatcher; `Member` is not changed by this stream beyond the pick stamp |

The rewrite rule, for every one of the thirteen tests (fourteen call sites —
`_SuccessAndTimeout` has two; the payload argument is `req`, `req2` or an inline
`map[string]any{"requestId": "r1"}`):
`dispatcher.dispatchCalloutToMember(CTX, member, EntityProcessorCalculationRequest, req, "ID", "TX", MS, "processor", "NAME")`
→ `tryOnce(dispatcher, CTX, member, "ID", "TX", MS)`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/grpc/dispatch_test.go`:

```go
// tryOnce makes one try against member with a raw payload carrying only the
// request id, and folds the three outcomes of a try into (response, error) —
// the shape the tests of the single try were written against. timeoutMs is the
// answer limit.
func tryOnce(d *ProcessorDispatcher, ctx context.Context, member *Member, requestID, txID string, timeoutMs int64) (*ProcessingResponse, error) {
	var got *ProcessingResponse
	call := Callout{
		Kind:        ProcessorCallout,
		Name:        "my-proc",
		TenantID:    member.TenantID,
		TxID:        txID,
		RequestID:   requestID,
		AnswerLimit: time.Duration(timeoutMs) * time.Millisecond,
		OwnerNodeID: "node-test",
		eventType:   EntityProcessorCalculationRequest,
		buildRequest: func(id string) any {
			return map[string]any{"requestId": id}
		},
		mapResponse: func(resp *ProcessingResponse) (CalloutResult, error) {
			got = resp
			return CalloutResult{}, nil
		},
	}
	_, failure, ctxErr := d.dispatchCalloutToMember(ctx, member, call, d.resolveTxToken(ctx, txID))
	switch {
	case ctxErr != nil:
		return nil, ctxErr
	case failure != nil:
		return nil, failure
	}
	return got, nil
}
```

`internal/grpc/try_kind_test.go`:

```go
package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// kindOf runs one try and returns what it produced.
func kindOf(d *ProcessorDispatcher, ctx context.Context, member *Member, call Callout) (*contract.CalloutFailure, error) {
	_, failure, ctxErr := d.dispatchCalloutToMember(ctx, member, call, "")
	return failure, ctxErr
}

func rawCall(limit time.Duration) Callout {
	return Callout{
		Kind: ProcessorCallout, Name: "p", TenantID: testTenantID, RequestID: "r1", AnswerLimit: limit, OwnerNodeID: "node-test",
		eventType:    EntityProcessorCalculationRequest,
		buildRequest: func(id string) any { return map[string]any{"requestId": id} },
		mapResponse:  func(*ProcessingResponse) (CalloutResult, error) { return CalloutResult{}, nil },
	}
}

func assertKindAndCode(t *testing.T, failure *contract.CalloutFailure, ctxErr error, wantKind contract.CalloutFailureKind, wantCode string) {
	t.Helper()
	if ctxErr != nil {
		t.Fatalf("unexpected ctx error: %v", ctxErr)
	}
	if failure == nil {
		t.Fatal("expected a failure")
	}
	if failure.Kind != wantKind {
		t.Errorf("Kind = %v, want %v", failure.Kind, wantKind)
	}
	if failure.Code != wantCode {
		t.Errorf("Code = %q, want %q", failure.Code, wantCode)
	}
	var appErr *common.AppError
	if wantCode == "" {
		return
	}
	if !errors.As(failure, &appErr) || appErr.Code != wantCode || appErr.Status != 503 || !appErr.Retryable {
		t.Errorf("carried AppError = %+v, want retryable 503 %s", appErr, wantCode)
	}
	if failure.Message != appErr.Message {
		t.Errorf("Message = %q, want the AppError's %q", failure.Message, appErr.Message)
	}
}

func TestTryKind_TrackRequestOnAnEvictedMember_IsNoHandOff(t *testing.T) {
	d, registry, memberID, _ := setupTestDispatcher(t)
	member := registry.Get(memberID)
	registry.Unregister(member)
	failure, ctxErr := kindOf(d, testContext(), member, rawCall(30*time.Second))
	assertKindAndCode(t, failure, ctxErr, contract.NoHandOff, common.ErrCodeComputeMemberDisconnected)
}

func TestTryKind_EvictedDuringEnqueue_IsNoHandOff(t *testing.T) {
	d, member := newWedgedDispatcher(t)
	go func() {
		time.Sleep(30 * time.Millisecond)
		member.Evict(errors.New("member disconnected"))
	}()
	failure, ctxErr := kindOf(d, testContext(), member, rawCall(30*time.Second))
	assertKindAndCode(t, failure, ctxErr, contract.NoHandOff, common.ErrCodeComputeMemberDisconnected)
}

func TestTryKind_EnqueueDeadline_IsNoHandOff(t *testing.T) {
	d, member := newWedgedDispatcher(t)
	failure, ctxErr := kindOf(d, testContext(), member, rawCall(100*time.Millisecond))
	assertKindAndCode(t, failure, ctxErr, contract.NoHandOff, common.ErrCodeDispatchTimeout)
	if !strings.Contains(failure.Message, "processor dispatch timed out after 100ms: member not draining") {
		t.Errorf("message = %q", failure.Message)
	}
}

func TestTryKind_CallerCancelledDuringEnqueue_IsCtxErrUnchanged(t *testing.T) {
	d, member := newWedgedDispatcher(t)
	ctx, cancel := context.WithCancel(testContext())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	failure, ctxErr := kindOf(d, ctx, member, rawCall(30*time.Second))
	if failure != nil || ctxErr != context.Canceled {
		t.Fatalf("got (%v, %v), want (nil, context.Canceled)", failure, ctxErr)
	}
}

func TestTryKind_DisconnectedWhileWaiting_IsNoAnswer(t *testing.T) {
	d, registry, memberID, sentCh := setupTestDispatcher(t)
	member := registry.Get(memberID)
	go func() {
		<-sentCh
		member.Evict(errors.New("stream dropped"))
	}()
	failure, ctxErr := kindOf(d, testContext(), member, rawCall(5*time.Second))
	assertKindAndCode(t, failure, ctxErr, contract.NoAnswer, common.ErrCodeComputeMemberDisconnected)
}

func TestTryKind_AnswerLimit_IsNoAnswer(t *testing.T) {
	d, registry, memberID, _ := setupTestDispatcher(t)
	failure, ctxErr := kindOf(d, testContext(), registry.Get(memberID), rawCall(50*time.Millisecond))
	assertKindAndCode(t, failure, ctxErr, contract.NoAnswer, common.ErrCodeDispatchTimeout)
	if !strings.Contains(failure.Message, "processor dispatch timed out after 50ms: no response") {
		t.Errorf("message = %q", failure.Message)
	}
}

func TestTryKind_CallerCancelledWhileWaiting_IsCtxErrUnchanged(t *testing.T) {
	d, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx, cancel := context.WithCancel(testContext())
	go func() { <-sentCh; cancel() }()
	failure, ctxErr := kindOf(d, ctx, registry.Get(memberID), rawCall(5*time.Second))
	if failure != nil || ctxErr != context.Canceled {
		t.Fatalf("got (%v, %v), want (nil, context.Canceled)", failure, ctxErr)
	}
}

func TestTryKind_MemberAnsweredFailure_IsMemberFailedWithItsVerdict(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name      string
		memberErr string
		verdict   *bool
		wantMsg   string
	}{
		{"verdict true", "downstream busy", &yes, "downstream busy"},
		{"verdict false", "card declined", &no, "card declined"},
		{"verdict absent", "card declined", nil, "card declined"},
		{"no message", "", nil, "processor returned failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, registry, memberID, sentCh := setupTestDispatcher(t)
			member := registry.Get(memberID)
			go func() {
				<-sentCh
				member.CompleteRequest("r1", &ProcessingResponse{Success: false, Error: tt.memberErr, Retryable: tt.verdict})
			}()
			failure, ctxErr := kindOf(d, testContext(), member, rawCall(5*time.Second))
			assertKindAndCode(t, failure, ctxErr, contract.MemberFailed, "")
			if failure.Message != tt.wantMsg || failure.Error() != tt.wantMsg {
				t.Errorf("Message/Error = %q/%q, want %q", failure.Message, failure.Error(), tt.wantMsg)
			}
			if (failure.Retryable == nil) != (tt.verdict == nil) || (tt.verdict != nil && *failure.Retryable != *tt.verdict) {
				t.Errorf("Retryable = %v, want %v", failure.Retryable, tt.verdict)
			}
			var appErr *common.AppError
			if errors.As(failure, &appErr) {
				t.Errorf("MemberFailed must carry no AppError, got %v", appErr)
			}
		})
	}
}

func TestTryKind_CloudEventBuild_IsTerminal(t *testing.T) {
	d, registry, memberID, sentCh := setupTestDispatcher(t)
	call := rawCall(5 * time.Second)
	call.buildRequest = func(string) any { return make(chan int) } // json cannot marshal a channel
	failure, ctxErr := kindOf(d, testContext(), registry.Get(memberID), call)
	assertKindAndCode(t, failure, ctxErr, contract.Terminal, "")
	if !strings.HasPrefix(failure.Error(), "failed to build processor cloud event: ") {
		t.Errorf("message = %q", failure.Error())
	}
	select {
	case <-sentCh:
		t.Fatal("nothing may be sent")
	default:
	}
}

func TestTryKind_AuthContext_IsTerminalAndNamesNoPrincipal(t *testing.T) {
	d, registry, memberID, sentCh := setupTestDispatcher(t)
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "user-secret-id", Tenant: spi.Tenant{ID: testTenantID}, // Kind unset
	})
	failure, ctxErr := kindOf(d, ctx, registry.Get(memberID), rawCall(5*time.Second))
	assertKindAndCode(t, failure, ctxErr, contract.Terminal, "")
	if !errors.Is(failure, contract.ErrAuthContextUnavailable) {
		t.Error("the sentinel the classifier maps to a ticketed 500 must survive")
	}
	if strings.Contains(failure.Message, "user-secret-id") {
		t.Errorf("Message is client-visible (it is copied into attempts) and must not name the principal: %q", failure.Message)
	}
	select {
	case <-sentCh:
		t.Fatal("nothing may be sent")
	default:
	}
}

func TestTryKind_ResponsePayloadUnmarshal_IsTerminal(t *testing.T) {
	registry := NewMemberRegistry()
	m := registry.Register("m-1", testTenantID, []string{"python"}, func(ce *cepb.CloudEvent) error {
		reqID, err := extractRequestID(ce)
		if err != nil {
			t.Errorf("extractRequestID: %v", err)
			return nil
		}
		registry.Get("m-1").CompleteRequest(reqID, &ProcessingResponse{Success: true, Payload: json.RawMessage(`{"data":`)})
		return nil
	}, nil)
	t.Cleanup(func() { registry.Unregister(m) })
	d := newTestDispatcher(t, registry)

	call := NewProcessorCallout(testTenantID, testEntity(), testProcessor("python", 0), "wf1", "t1", "tx-1")
	call.RequestID, call.AnswerLimit, call.OwnerNodeID = "r1", 5*time.Second, "node-test"
	failure, ctxErr := kindOf(d, testContext(), m, call)
	assertKindAndCode(t, failure, ctxErr, contract.Terminal, "")
	if !strings.HasPrefix(failure.Error(), "failed to unmarshal processor response payload: ") {
		t.Errorf("message = %q", failure.Error())
	}
}
```

(The remaining two rows of the §3 table — "no matching member" and the
between-tries rule — belong to `RunLocal` and are tested in L-8; "stored
`responseTimeoutMs` over the bound" and "criterion does not parse" are tested in
L-5 and L-6.)

Add to `internal/grpc/callout_failure_rpc_test.go` (its imports gain `"sync/atomic"`):

```go
// A cnode that answers "I failed": one try, 400 WORKFLOW_FAILED, the message
// names the processor and carries the cnode's own text — and no longer the
// inner "processor dispatch failed:" segment. (Whether the envelope is marked
// retryable for verdict=true is decided where workflow errors are classified,
// outside this package; this pins code and message for all three verdicts.)
func TestRPC_ProcessorMemberFailed_EnvelopeCarriesTheMemberMessage(t *testing.T) {
	yes, no := true, false
	for name, verdict := range map[string]*bool{"verdict true": &yes, "verdict false": &no, "verdict absent": nil} {
		t.Run(name, func(t *testing.T) {
			const modelName = "grpc-proc-member-failed"
			const tag = "member-failed-tag"
			svc, wfHandler, ctx := newTestEnvWithDispatch(t)
			setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
				processorRPCWorkflowJSON("member-failed-wf", "charge", tag, 5000))

			var asked atomic.Int32
			for _, id := range []string{"m-1", "m-2"} {
				svc.registry.Register(id, testTenant, []string{tag}, func(ce *cepb.CloudEvent) error {
					asked.Add(1)
					reqID, err := extractRequestID(ce)
					if err != nil {
						t.Errorf("extractRequestID: %v", err)
						return nil
					}
					svc.registry.Get(id).CompleteRequest(reqID, &ProcessingResponse{Success: false, Error: "card declined", Retryable: verdict})
					return nil
				}, nil)
			}

			typed := createScheduledEntity(t, svc, ctx, modelName)
			assertClientErrorEnvelope(t, typed, "WORKFLOW_FAILED")
			if !strings.Contains(typed.Error.Message, "processor charge failed: card declined") {
				t.Errorf("message = %s", typed.Error.Message)
			}
			if strings.Contains(typed.Error.Message, "dispatch failed") {
				t.Errorf("the inner \"dispatch failed:\" segment must be gone: %s", typed.Error.Message)
			}
			if got := asked.Load(); got != 1 {
				t.Errorf("%d cnodes were asked, want exactly one try", got)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/grpc/... -run 'TestTryKind|TestRPC_ProcessorMemberFailed'`   Expected: FAIL (build) with `too many arguments in call to d.dispatchCalloutToMember` / `assignment mismatch: 3 variables but … returns 2 values`.

- [ ] **Step 3: Implement**

`internal/grpc/dispatch.go` — replace `dispatchCalloutToMember` and `disconnectedErr` with:

```go
// dispatchCalloutToMember makes one try: it wraps the callout's request in a
// CloudEvent carrying the auth context and pass, tracks the request, hands it
// to the member's writer, and waits for the tracked response or the answer
// limit. Exactly one of its three results is meaningful:
//
//   - the mapped result, when the cnode answered success;
//   - a CalloutFailure, whose Kind says whether another cnode may be tried.
//     The line is the hand-off: until Member.Send returns nil the work provably
//     never left this pnode (NoHandOff); after it, silence or a dropped stream
//     is NoAnswer. Codes, statuses and messages are the ones a client has
//     always seen — the kind travels beside them;
//   - ctx.Err(), unchanged, when the caller's own context ended.
//
// One deadline — the answer limit — bounds the hand-off and the wait together,
// so a cnode that is attached but not taking data costs up to one answer limit
// before it is classified NoHandOff.
//
// The callout kind and name flow into client-facing diagnostics (warnings and
// errors surface in the gRPC warnings array and the HTTP body — see
// .claude/rules/error-handling.md) and into server logs.
func (d *ProcessorDispatcher) dispatchCalloutToMember(ctx context.Context, member *Member, call Callout, pass string) (CalloutResult, *contract.CalloutFailure, error) {
	label, name, requestID := call.Kind.String(), call.Name, call.RequestID
	limitMs := call.AnswerLimit.Milliseconds()

	ce, err := NewCloudEvent(call.eventType, call.buildRequest(requestID))
	if err != nil {
		return CalloutResult{}, terminalFailure(fmt.Errorf("failed to build %s cloud event: %w", label, err)), nil
	}
	if err := AttachAuthContext(ctx, ce); err != nil {
		// The cause names the principal; the client-safe Message does not.
		return CalloutResult{}, &contract.CalloutFailure{
			Kind:    contract.Terminal,
			Message: "auth context unavailable for dispatch",
			Err:     fmt.Errorf("failed to attach auth context to %s cloud event: %w", label, err),
		}, nil
	}
	AttachTxToken(ce, pass)

	slog.Debug("dispatch request", "pkg", "grpc", "requestId", requestID, "memberId", member.ID,
		"payload", logging.PayloadPreview([]byte(ce.GetTextData()), 200))

	callCtx, cancel := context.WithTimeout(ctx, call.AnswerLimit)
	defer cancel()

	// TrackRequest fails closed with ErrMemberEvicted the instant the member
	// is torn down, even in the window before Evict has finished closing the
	// evicted channel (see Member.closed): that is what keeps a try from
	// selecting on a never-tracked channel until its answer limit and
	// misreporting DISPATCH_TIMEOUT for a member that was already gone.
	ch, err := member.TrackRequest(requestID)
	if err != nil {
		slog.Warn("member gone before dispatch", "pkg", "grpc", "memberId", member.ID, "label", label, "name", name, "requestId", requestID)
		return CalloutResult{}, appFailure(contract.NoHandOff, disconnectedErr(label)), nil
	}
	// Every exit that does not consume the response must clear the tracking
	// entry, or a late reply finds a dangling channel and the map entry leaks.
	// The response arm's normal completion already cleared it; clearing again
	// is a no-op.
	defer member.AbandonRequest(requestID)

	if err := member.Send(callCtx, ce); err != nil {
		switch {
		case errors.Is(err, ErrMemberEvicted):
			slog.Error("member evicted while enqueueing dispatch", "pkg", "grpc", "memberId", member.ID, "label", label, "name", name, "requestId", requestID)
			return CalloutResult{}, appFailure(contract.NoHandOff, disconnectedErr(label)), nil
		case ctx.Err() != nil:
			return CalloutResult{}, nil, ctx.Err()
		default:
			slog.Error("dispatch timeout", "pkg", "grpc", "phase", "enqueue", "memberId", member.ID, "label", label, "name", name, "requestId", requestID, "timeout", call.AnswerLimit)
			return CalloutResult{}, appFailure(contract.NoHandOff, common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
				fmt.Sprintf("%s dispatch timed out after %dms: member not draining", label, limitMs)).AsRetryable()), nil
		}
	}

	// The hand-off happened: from here on the work may have reached the cnode.
	select {
	case resp := <-ch:
		// Warnings first, keyed by callout name, so that a failed try still
		// surfaces them and the client sees which callout warned.
		if resp != nil {
			for _, w := range resp.Warnings {
				common.AddWarning(ctx, fmt.Sprintf("%s %s: %s", label, name, w))
			}
		}
		if resp != nil && resp.Disconnected {
			slog.Error("member disconnected mid-dispatch", "pkg", "grpc", "memberId", member.ID, "label", label, "name", name, "requestId", requestID)
			return CalloutResult{}, appFailure(contract.NoAnswer, disconnectedErr(label)), nil
		}
		if resp == nil || !resp.Success {
			failure := &contract.CalloutFailure{Kind: contract.MemberFailed, Message: label + " returned failure"}
			if resp != nil {
				failure.Retryable = resp.Retryable
				if resp.Error != "" {
					failure.Message = resp.Error
					common.AddError(ctx, fmt.Sprintf("%s %s: %s", label, name, resp.Error))
				}
			}
			return CalloutResult{}, failure, nil
		}
		result, err := call.mapResponse(resp)
		if err != nil {
			return CalloutResult{}, terminalFailure(err), nil
		}
		slog.Debug("dispatch completed", "pkg", "grpc", "memberId", member.ID, "label", label, "name", name, "requestId", requestID)
		return result, nil, nil
	case <-callCtx.Done():
		if ctx.Err() != nil {
			return CalloutResult{}, nil, ctx.Err()
		}
		slog.Error("dispatch timeout", "pkg", "grpc", "phase", "response", "memberId", member.ID, "label", label, "name", name, "requestId", requestID, "timeout", call.AnswerLimit)
		return CalloutResult{}, appFailure(contract.NoAnswer, common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
			fmt.Sprintf("%s dispatch timed out after %dms: no response", label, limitMs)).AsRetryable()), nil
	}
}

// appFailure is a failure of the given kind carrying appErr, with its code and
// client-safe message repeated beside the kind.
func appFailure(kind contract.CalloutFailureKind, appErr *common.AppError) *contract.CalloutFailure {
	return &contract.CalloutFailure{Kind: kind, Code: appErr.Code, Message: appErr.Message, Err: appErr}
}

// terminalFailure is a failure that would repeat identically on any cnode.
func terminalFailure(err error) *contract.CalloutFailure {
	return &contract.CalloutFailure{Kind: contract.Terminal, Message: err.Error(), Err: err}
}

// disconnectedErr is the retryable 503 for a cnode that is gone — before the
// hand-off (NoHandOff) or after it (NoAnswer); the kind beside it tells which.
func disconnectedErr(label string) *common.AppError {
	return common.Operational(http.StatusServiceUnavailable, common.ErrCodeComputeMemberDisconnected,
		fmt.Sprintf("compute member disconnected during %s dispatch", label)).AsRetryable()
}
```

In `dispatchOnce`, replace the call and its error handling and the
`return call.mapResponse(resp)` with:

```go
	result, failure, ctxErr := d.dispatchCalloutToMember(ctx, member, call, d.resolveTxToken(ctx, call.TxID))
	switch {
	case ctxErr != nil:
		return CalloutResult{}, ctxErr
	case failure != nil:
		return CalloutResult{}, failure
	}
	return result, nil
```

`internal/grpc/members.go` — the comment on `ProcessingResponse.Retryable`
(lines 48-55) becomes:

```go
	// Retryable carries the member-supplied retryable flag from the inbound
	// CloudEvent error shape (api/grpc/events/types.go: every *EventJsonError
	// variant declares Retryable *bool). The pointer is nil when the wire
	// omitted the key or when no error was present, distinguishing "wire
	// said so" from "wire didn't say". A failed try carries it on as
	// contract.CalloutFailure.Retryable: it never decides whether another
	// cnode is tried, only whether the client is told a re-run may help.
	Retryable *bool
```

`CHANGELOG.md`, `[Unreleased]` → `### Changed`:

```markdown
- **A compute member's own failure message reaches the client without the
  inner `processor dispatch failed:` segment.** `WORKFLOW_FAILED` now reads
  `processor <name> failed: <the member's message>` (for a criterion,
  `failed to evaluate transition criterion: <the member's message>`).
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/grpc/...`, `go test ./internal/cluster/dispatch/...` (it drives the real dispatcher through the peer handler), `go vet ./internal/grpc/...`   Expected: PASS.
Exit check: `grep -rn 'dispatch failed: %s' internal/grpc/` → no hits; `grep -rn 'current dispatcher is single-shot' internal/` → no hits.

- [ ] **Step 5: Commit**

```
git add internal/grpc/dispatch.go internal/grpc/members.go internal/grpc/dispatch_test.go internal/grpc/try_kind_test.go internal/grpc/callout_failure_rpc_test.go CHANGELOG.md
git commit -m "feat(grpc): every error site of a try is classified by whether the work left the pnode (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task L-8: `RunLocal`, `LocalResult`, `TryNumberer`; the three entry points become wrappers over it

**Spec:** §4 in full — the signature, the five bullets, the `TryNumberer` paragraph; D1, D4. §13 rows (all **U**; **G** where marked): "`NoHandOff` → next local cnode answers"; "`NoHandOff`, tries = 1 → the try's own code" (U, G); "`NoAnswer`, processor not idempotent → stop" (U, G); "`NoAnswer`, `idempotent` → next cnode answers"; "`NoAnswer`, criterion; function → next cnode answers"; "cnode drops after hand-off — both settings"; "`MemberFailed` …, one try"; "`Terminal` → stop" (U, G — the G pin is L-5's); "Same request id on every try"; "Round robin across two cnodes; a new cnode goes first"; "Two tenants share a tag on one pnode".

**Files:**
- Create: `internal/grpc/run_local.go`
- Modify: `internal/grpc/callout.go` (`Callout.Number`)
- Modify: `internal/grpc/dispatch.go` (`dispatchOnce` deleted; `runSingleTry` added; the three `Dispatch*` call it)
- Modify: `internal/grpc/dispatch_test.go` (`tryOnce` unchanged; nothing else)
- Modify: `cmd/cyoda/help/content/grpc.md` (TAG ROUTING: the request-id paragraph)
- Test: `internal/grpc/run_local_test.go`, `internal/grpc/callout_failure_rpc_test.go`

**Interfaces:**
- Consumes: L-1 … L-7.
- Produces:
  - `type TryNumberer interface { Next() (major, minor uint32) }`
  - `type MinorNumberer struct{…}`, `func NewMinorNumberer(major uint32) *MinorNumberer` — `Next` gives `(major, 1)`, `(major, 2)`, …; for the pnode that received a hand-over (stream **P**). Not safe for concurrent use; `RunLocal` calls it from one goroutine.
  - `Callout.Number TryNumberer` — required; `RunLocal` calls `Next` exactly once before each try, before that try's pass is minted (L-10).
  - `type LocalResult struct { Result CalloutResult; Failure *contract.CalloutFailure; CtxErr error; TriesUsed int; Attempts []contract.CalloutAttempt }`, `func (r LocalResult) OK() bool`, `func (r LocalResult) Err() error` (`CtxErr`, else `Failure`, else nil).
  - `func (d *ProcessorDispatcher) RunLocal(ctx context.Context, call Callout, maxTries int) LocalResult` — `maxTries ≥ 1` is the caller's to guarantee (the hand-over handler validates `triesLeft`).
    - `Failure` is the **last try's** failure, unchanged; when no try was made (no matching cnode) it is `NoHandOff` with code `NO_COMPUTE_MEMBER_FOR_TAG`, wrapping `ErrNoMatchingMember`. The caller decides with `Failure.Kind.MayTryAnother(call.RepeatSafe)`. `Failure.Attempts` is nil; the tries are in `LocalResult.Attempts` (failed tries only, in order; `Cause` is the failure's `Message`).
    - `CtxErr` is set, and `Failure` nil, when the caller's context ended — during a try or between tries. `TriesUsed` still counts a try that was in progress.
    - It never waits for a cnode to appear.
  - The wrappers `DispatchProcessor` / `DispatchCriteria` / `DispatchFunction` on `*ProcessorDispatcher`, and the unexported `runSingleTry` and `singleTryNumberer` they use, exist **only** until the owner's loop (`internal/callout.Coordinator`) replaces them as the `contract.ExternalProcessingService`. **That stream deletes them** (with `ProcessorDispatcher.uuids`, which nothing else uses, and the `uuids` constructor parameter); this stream does not.

- [ ] **Step 1: Write the failing tests**

`internal/grpc/run_local_test.go`:

```go
package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// --- scripted cnodes ---

// asked records what one scripted cnode was sent.
type asked struct {
	mu         sync.Mutex
	requestIDs []string // payload requestId of every request
	payloadIDs []string // payload id of every request
	passes     []string // the pass attribute of every request
}

func (a *asked) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requestIDs)
}

// script is what a scripted cnode does with a request; nil stays silent.
type script func(m *Member, requestID string)

func answers(resp ProcessingResponse) script {
	return func(m *Member, requestID string) { m.CompleteRequest(requestID, &resp) }
}

func answersAs(id string) script {
	return answers(ProcessingResponse{Success: true, Payload: json.RawMessage(`{"data":{"by":"` + id + `"}}`)})
}

func drops() script {
	return func(m *Member, _ string) { m.Evict(errors.New("stream dropped")) }
}

// attach registers a scripted cnode. Ids given in ascending order are tried in
// that order by a fresh registry: attached first, tried first, the id breaking
// a tie on the clock.
func attach(t *testing.T, reg *MemberRegistry, id string, tenant spi.TenantID, tag string, s script) (*Member, *asked) {
	t.Helper()
	a := &asked{}
	m := reg.Register(id, tenant, []string{tag}, func(ce *cepb.CloudEvent) error {
		_, payload, err := ParseCloudEvent(ce)
		if err != nil {
			t.Errorf("ParseCloudEvent: %v", err)
			return nil
		}
		var body struct {
			ID        string `json:"id"`
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Errorf("request payload: %v", err)
			return nil
		}
		func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.requestIDs = append(a.requestIDs, body.RequestID)
			a.payloadIDs = append(a.payloadIDs, body.ID)
			a.passes = append(a.passes, TxTokenFromCloudEvent(ce))
		}()
		if s != nil {
			if m := reg.Get(id); m != nil {
				s(m, body.RequestID)
			}
		}
		return nil
	}, nil)
	t.Cleanup(func() { reg.Unregister(m) })
	return m, a
}

// attachGone registers a cnode and evicts it without unregistering: it is still
// listed, and the try on it fails before the hand-off.
func attachGone(t *testing.T, reg *MemberRegistry, id string, tenant spi.TenantID, tag string) *asked {
	t.Helper()
	m, a := attach(t, reg, id, tenant, tag, nil)
	m.Evict(errors.New("gone"))
	return a
}

// countingNumberer is the owner's numbering: major rises before each try.
type countingNumberer struct{ major uint32 }

func (n *countingNumberer) Next() (uint32, uint32) { n.major++; return n.major, 0 }

// armed fills what the caller of a builder fills.
func armed(call Callout, repeatSafe bool, limit time.Duration) Callout {
	call.RequestID = "req-fixed"
	call.AnswerLimit = limit
	call.RepeatSafe = repeatSafe
	call.OwnerNodeID = "node-test"
	call.Number = &countingNumberer{}
	return call
}

func processorCall(tag string, repeatSafe bool, limit time.Duration) Callout {
	return armed(NewProcessorCallout(testTenantID, testEntity(), testProcessor(tag, 0), "wf1", "t1", "tx-1"), repeatSafe, limit)
}

func answeredBy(t *testing.T, res LocalResult) string {
	t.Helper()
	if !res.OK() {
		t.Fatalf("expected an answer, got failure=%v ctxErr=%v", res.Failure, res.CtxErr)
	}
	var data struct {
		By string `json:"by"`
	}
	if err := json.Unmarshal(res.Result.Entity.Data, &data); err != nil {
		t.Fatalf("result data: %v", err)
	}
	return data.By
}

func appCode(err error) string {
	var appErr *common.AppError
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return ""
}

// --- the §13 rows ---

func TestRunLocal_NoHandOff_NextCnodeAnswers(t *testing.T) {
	reg := NewMemberRegistry()
	gone := attachGone(t, reg, "m-1", testTenantID, "x")
	attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)

	// not repeat-safe: a failed hand-off permits another cnode all the same
	res := d.RunLocal(testContext(), processorCall("x", false, 5*time.Second), 4)
	if by := answeredBy(t, res); by != "m-2" {
		t.Errorf("answered by %s, want m-2", by)
	}
	if res.TriesUsed != 2 {
		t.Errorf("TriesUsed = %d, want 2", res.TriesUsed)
	}
	if len(res.Attempts) != 1 || res.Attempts[0].MemberID != "m-1" || res.Attempts[0].Kind != contract.NoHandOff {
		t.Errorf("Attempts = %+v, want one NoHandOff on m-1", res.Attempts)
	}
	if gone.count() != 0 {
		t.Error("nothing may have been sent to the cnode that was gone")
	}
}

func TestRunLocal_NoHandOff_OneTry_ReportsTheTrysOwnCode(t *testing.T) {
	t.Run("cnode gone", func(t *testing.T) {
		reg := NewMemberRegistry()
		attachGone(t, reg, "m-1", testTenantID, "x")
		_, second := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
		d := newTestDispatcher(t, reg)

		res := d.RunLocal(testContext(), processorCall("x", false, 5*time.Second), 1)
		if res.Failure == nil || res.Failure.Kind != contract.NoHandOff || appCode(res.Err()) != common.ErrCodeComputeMemberDisconnected {
			t.Fatalf("failure = %+v, want NoHandOff COMPUTE_MEMBER_DISCONNECTED", res.Failure)
		}
		if res.TriesUsed != 1 || second.count() != 0 {
			t.Errorf("TriesUsed = %d, second cnode asked %d times; want 1 and 0", res.TriesUsed, second.count())
		}
	})
	t.Run("cnode not draining", func(t *testing.T) {
		d, _ := newWedgedDispatcher(t) // one cnode, tag "python", writer parked
		res := d.RunLocal(testContext(), processorCall("python", false, 100*time.Millisecond), 1)
		if res.Failure == nil || res.Failure.Kind != contract.NoHandOff || appCode(res.Err()) != common.ErrCodeDispatchTimeout {
			t.Fatalf("failure = %+v, want NoHandOff DISPATCH_TIMEOUT", res.Failure)
		}
	})
}

func TestRunLocal_NoAnswer_NotRepeatSafe_Stops(t *testing.T) {
	reg := NewMemberRegistry()
	attach(t, reg, "m-1", testTenantID, "x", nil) // takes the work, never answers
	_, second := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)

	res := d.RunLocal(testContext(), processorCall("x", false, 50*time.Millisecond), 4)
	if res.Failure == nil || res.Failure.Kind != contract.NoAnswer || appCode(res.Err()) != common.ErrCodeDispatchTimeout {
		t.Fatalf("failure = %+v, want NoAnswer DISPATCH_TIMEOUT", res.Failure)
	}
	if res.TriesUsed != 1 || second.count() != 0 {
		t.Errorf("TriesUsed = %d, second cnode asked %d times; want 1 and 0", res.TriesUsed, second.count())
	}
}

func TestRunLocal_NoAnswer_RepeatSafe_NextCnodeAnswers(t *testing.T) {
	yes := true
	criterion := json.RawMessage(`{"type":"function","function":{"name":"check","config":{"calculationNodesTags":"x"}}}`)
	criteriaCall, failure := NewCriteriaCallout(testTenantID, testEntity(), criterion, "transition", "wf1", "t1", "", "tx-1")
	if failure != nil {
		t.Fatal(failure)
	}
	functionCall := NewFunctionCallout(testTenantID, testEntity(),
		spi.ScheduleFunction{Name: "calcFire", ResultKind: "Schedule", CalculationNodesTags: "x"}, "wf1", "t1", "tx-1")

	tests := []struct {
		name   string
		call   Callout
		answer ProcessingResponse
		check  func(t *testing.T, r CalloutResult)
	}{
		{"processor declared idempotent", processorCall("x", true, 50*time.Millisecond),
			ProcessingResponse{Success: true, Payload: json.RawMessage(`{"data":{"by":"m-2"}}`)},
			func(t *testing.T, r CalloutResult) {
				if string(r.Entity.Data) != `{"by":"m-2"}` {
					t.Errorf("entity data = %s", r.Entity.Data)
				}
			}},
		// the builders mark these repeat-safe; armed is told to keep that
		{"criterion", armed(criteriaCall, criteriaCall.RepeatSafe, 50*time.Millisecond),
			ProcessingResponse{Success: true, Matches: &yes},
			func(t *testing.T, r CalloutResult) {
				if !r.Matches {
					t.Error("expected matches=true from the second cnode")
				}
			}},
		{"function", armed(functionCall, functionCall.RepeatSafe, 50*time.Millisecond),
			ProcessingResponse{Success: true, ResultKind: "Schedule", Result: json.RawMessage(`{"fireAfterMs":1}`)},
			func(t *testing.T, r CalloutResult) {
				if r.Function.Kind != "Schedule" {
					t.Errorf("function result = %+v", r.Function)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := NewMemberRegistry()
			_, first := attach(t, reg, "m-1", testTenantID, "x", nil)
			attach(t, reg, "m-2", testTenantID, "x", answers(tt.answer))
			d := newTestDispatcher(t, reg)

			res := d.RunLocal(testContext(), tt.call, 4)
			if !res.OK() {
				t.Fatalf("failure=%v ctxErr=%v", res.Failure, res.CtxErr)
			}
			tt.check(t, res.Result)
			if res.TriesUsed != 2 || first.count() != 1 {
				t.Errorf("TriesUsed = %d, first cnode asked %d times; want 2 and 1", res.TriesUsed, first.count())
			}
			if len(res.Attempts) != 1 || res.Attempts[0].Kind != contract.NoAnswer || res.Attempts[0].MemberID != "m-1" {
				t.Errorf("Attempts = %+v", res.Attempts)
			}
		})
	}
}

func TestRunLocal_CnodeDropsAfterHandOff(t *testing.T) {
	for _, repeatSafe := range []bool{false, true} {
		name := "not repeat-safe: stop"
		if repeatSafe {
			name = "repeat-safe: next cnode answers"
		}
		t.Run(name, func(t *testing.T) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", drops())
			_, second := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
			d := newTestDispatcher(t, reg)

			res := d.RunLocal(testContext(), processorCall("x", repeatSafe, 5*time.Second), 4)
			if repeatSafe {
				if by := answeredBy(t, res); by != "m-2" || res.TriesUsed != 2 {
					t.Errorf("answered by %s after %d tries, want m-2 after 2", by, res.TriesUsed)
				}
				return
			}
			if res.Failure == nil || res.Failure.Kind != contract.NoAnswer || appCode(res.Err()) != common.ErrCodeComputeMemberDisconnected {
				t.Fatalf("failure = %+v, want NoAnswer COMPUTE_MEMBER_DISCONNECTED", res.Failure)
			}
			if res.TriesUsed != 1 || second.count() != 0 {
				t.Errorf("TriesUsed = %d, second cnode asked %d times; want 1 and 0", res.TriesUsed, second.count())
			}
		})
	}
}

func TestRunLocal_MemberFailed_StopsAfterOneTry_WithTheVerdict(t *testing.T) {
	yes, no := true, false
	for name, verdict := range map[string]*bool{"true": &yes, "false": &no, "absent": nil} {
		t.Run("verdict "+name, func(t *testing.T) {
			reg := NewMemberRegistry()
			attach(t, reg, "m-1", testTenantID, "x", answers(ProcessingResponse{Success: false, Error: "card declined", Retryable: verdict}))
			_, second := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
			d := newTestDispatcher(t, reg)

			// repeat-safe, to show that even then a cnode's "I failed" ends it
			res := d.RunLocal(testContext(), processorCall("x", true, 5*time.Second), 4)
			if res.Failure == nil || res.Failure.Kind != contract.MemberFailed || res.Failure.Message != "card declined" {
				t.Fatalf("failure = %+v", res.Failure)
			}
			if (res.Failure.Retryable == nil) != (verdict == nil) || (verdict != nil && *res.Failure.Retryable != *verdict) {
				t.Errorf("Retryable = %v, want %v", res.Failure.Retryable, verdict)
			}
			if res.TriesUsed != 1 || second.count() != 0 {
				t.Errorf("TriesUsed = %d, second cnode asked %d times; want 1 and 0", res.TriesUsed, second.count())
			}
		})
	}
}

func TestRunLocal_Terminal_Stops(t *testing.T) {
	reg := NewMemberRegistry()
	_, first := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
	_, second := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)
	noKind := spi.WithUserContext(context.Background(), &spi.UserContext{UserID: "u", Tenant: spi.Tenant{ID: testTenantID}})

	res := d.RunLocal(noKind, processorCall("x", true, 5*time.Second), 4)
	if res.Failure == nil || res.Failure.Kind != contract.Terminal || !errors.Is(res.Err(), contract.ErrAuthContextUnavailable) {
		t.Fatalf("failure = %+v, want Terminal (auth context)", res.Failure)
	}
	if res.TriesUsed != 1 || first.count()+second.count() != 0 {
		t.Errorf("TriesUsed = %d, requests sent = %d; want 1 and 0", res.TriesUsed, first.count()+second.count())
	}
}

func TestRunLocal_SameRequestIDOnEveryTry(t *testing.T) {
	reg := NewMemberRegistry()
	_, a1 := attach(t, reg, "m-1", testTenantID, "x", nil)
	_, a2 := attach(t, reg, "m-2", testTenantID, "x", nil)
	_, a3 := attach(t, reg, "m-3", testTenantID, "x", answersAs("m-3"))
	d := newTestDispatcher(t, reg)

	res := d.RunLocal(testContext(), processorCall("x", true, 50*time.Millisecond), 4)
	if by := answeredBy(t, res); by != "m-3" || res.TriesUsed != 3 {
		t.Fatalf("answered by %s after %d tries, want m-3 after 3", by, res.TriesUsed)
	}
	for i, a := range []*asked{a1, a2, a3} {
		a.mu.Lock()
		if len(a.requestIDs) != 1 || a.requestIDs[0] != "req-fixed" || a.payloadIDs[0] != "req-fixed" {
			t.Errorf("cnode %d saw requestId=%v id=%v, want one request with both req-fixed", i+1, a.requestIDs, a.payloadIDs)
		}
		a.mu.Unlock()
	}
}

func TestRunLocal_NeverTheSameCnodeTwice_AndStopsWhenTheyAreUsedUp(t *testing.T) {
	reg := NewMemberRegistry()
	_, a1 := attach(t, reg, "m-1", testTenantID, "x", nil)
	_, a2 := attach(t, reg, "m-2", testTenantID, "x", nil)
	d := newTestDispatcher(t, reg)

	start := time.Now()
	res := d.RunLocal(testContext(), processorCall("x", true, 50*time.Millisecond), 4)
	if res.TriesUsed != 2 || a1.count() != 1 || a2.count() != 1 {
		t.Fatalf("TriesUsed = %d, asked = %d/%d; want 2 tries, one per cnode", res.TriesUsed, a1.count(), a2.count())
	}
	if res.Failure == nil || res.Failure.Kind != contract.NoAnswer || len(res.Attempts) != 2 {
		t.Errorf("failure = %+v attempts = %+v; want the last try's NoAnswer and two attempts", res.Failure, res.Attempts)
	}
	if res.Failure.Attempts != nil {
		t.Error("RunLocal reports its tries in LocalResult.Attempts, not on the failure")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v: RunLocal must not wait for a cnode to appear", d)
	}
}

func TestRunLocal_MaxTriesBoundsTheRun(t *testing.T) {
	reg := NewMemberRegistry()
	var all []*asked
	for _, id := range []string{"m-1", "m-2", "m-3"} {
		_, a := attach(t, reg, id, testTenantID, "x", nil)
		all = append(all, a)
	}
	d := newTestDispatcher(t, reg)

	res := d.RunLocal(testContext(), processorCall("x", true, 50*time.Millisecond), 2)
	total := 0
	for _, a := range all {
		total += a.count()
	}
	if res.TriesUsed != 2 || total != 2 {
		t.Errorf("TriesUsed = %d, requests sent = %d; want 2 and 2", res.TriesUsed, total)
	}
}

func TestRunLocal_NoMatchingCnode_IsNoHandOffWithNoTry_AndDoesNotWait(t *testing.T) {
	d := newTestDispatcher(t, NewMemberRegistry())
	start := time.Now()
	res := d.RunLocal(testContext(), processorCall("x", false, 5*time.Second), 4)
	if res.Failure == nil || res.Failure.Kind != contract.NoHandOff || !errors.Is(res.Err(), ErrNoMatchingMember) {
		t.Fatalf("failure = %+v, want NoHandOff wrapping ErrNoMatchingMember", res.Failure)
	}
	if res.Failure.Code != common.ErrCodeNoComputeMemberForTag {
		t.Errorf("Code = %q", res.Failure.Code)
	}
	if res.TriesUsed != 0 || len(res.Attempts) != 0 {
		t.Errorf("TriesUsed = %d, Attempts = %+v; looking and finding none is not a try", res.TriesUsed, res.Attempts)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v: waiting is the owner's", d)
	}
}

func TestRunLocal_CallerCancelled_EndsTheRun(t *testing.T) {
	reg := NewMemberRegistry()
	ctx, cancel := context.WithCancel(testContext())
	attach(t, reg, "m-1", testTenantID, "x", func(*Member, string) { cancel() })
	_, second := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)

	res := d.RunLocal(ctx, processorCall("x", true, 5*time.Second), 4)
	if res.CtxErr != context.Canceled || res.Failure != nil || res.Err() != context.Canceled {
		t.Fatalf("CtxErr = %v Failure = %v, want context.Canceled unchanged and no failure", res.CtxErr, res.Failure)
	}
	if res.TriesUsed != 1 || second.count() != 0 {
		t.Errorf("TriesUsed = %d, second cnode asked %d times; want 1 and 0", res.TriesUsed, second.count())
	}
}

func TestRunLocal_TwoTenantsShareATag(t *testing.T) {
	reg := NewMemberRegistry()
	_, other := attach(t, reg, "m-other", "tenant-2", "shared", answersAs("m-other"))
	attachGone(t, reg, "m-mine", testTenantID, "shared")
	d := newTestDispatcher(t, reg)

	res := d.RunLocal(testContext(), processorCall("shared", true, 5*time.Second), 4)
	if res.OK() {
		t.Fatal("tenant-1's callout must not be answered by tenant-2's cnode")
	}
	if other.count() != 0 {
		t.Fatal("tenant-2's cnode was sent tenant-1's work")
	}
	if res.TriesUsed != 1 || len(res.Attempts) != 1 || res.Attempts[0].MemberID != "m-mine" {
		t.Errorf("TriesUsed = %d Attempts = %+v; want one attempt, naming only tenant-1's cnode", res.TriesUsed, res.Attempts)
	}
}

func TestRunLocal_RoundRobinAcrossCallouts_AndANewCnodeGoesFirst(t *testing.T) {
	reg := NewMemberRegistry()
	attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
	attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)
	run := func() string {
		return answeredBy(t, d.RunLocal(testContext(), processorCall("x", false, 5*time.Second), 4))
	}
	for i, want := range []string{"m-1", "m-2", "m-1"} {
		if got := run(); got != want {
			t.Fatalf("callout %d answered by %s, want %s", i, got, want)
		}
	}
	attach(t, reg, "m-3", testTenantID, "x", answersAs("m-3"))
	if got := run(); got != "m-3" {
		t.Fatalf("a cnode that has just attached goes first, got %s", got)
	}
}

// A cnode that attaches while the run is in progress is seen by the next try.
func TestRunLocal_SeesACnodeAttachedDuringTheRun(t *testing.T) {
	reg := NewMemberRegistry()
	attach(t, reg, "m-1", testTenantID, "x", func(*Member, string) {
		attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	})
	d := newTestDispatcher(t, reg)

	res := d.RunLocal(testContext(), processorCall("x", true, 100*time.Millisecond), 4)
	if by := answeredBy(t, res); by != "m-2" || res.TriesUsed != 2 {
		t.Errorf("answered by %s after %d tries, want m-2 after 2", by, res.TriesUsed)
	}
}

func TestRunLocal_NumbersEveryTryBeforeItIsMade(t *testing.T) {
	reg := NewMemberRegistry()
	attachGone(t, reg, "m-1", testTenantID, "x") // the hand-off fails: still a try, still numbered
	attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)
	call := processorCall("x", false, 5*time.Second)
	numberer := call.Number.(*countingNumberer)

	if res := d.RunLocal(testContext(), call, 4); !res.OK() || res.TriesUsed != 2 {
		t.Fatalf("res = %+v", res)
	}
	if numberer.major != 2 {
		t.Errorf("Next was called %d times, want once per try (2)", numberer.major)
	}
}

func TestMinorNumberer(t *testing.T) {
	n := NewMinorNumberer(7)
	for want := uint32(1); want <= 3; want++ {
		if major, minor := n.Next(); major != 7 || minor != want {
			t.Fatalf("Next() = (%d, %d), want (7, %d)", major, minor, want)
		}
	}
}
```

Add to `internal/grpc/callout_failure_rpc_test.go` — the G cells. They pin the
envelope for what the local procedure alone decides, and they pass before and
after this task's change (the entry points make one try either way): they are
here to hold the refactor, not to drive it.

```go
// NoHandOff with one try: the try's own code reaches the envelope, retryable.
func TestRPC_Processor_NoHandOff_OwnCodeEnvelope(t *testing.T) {
	const modelName = "grpc-proc-no-handoff"
	const tag = "no-handoff-tag"
	svc, wfHandler, ctx := newTestEnvWithDispatch(t)
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
		processorRPCWorkflowJSON("no-handoff-wf", "charge", tag, 100))

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	wedged := svc.registry.Register("m-1", testTenant, []string{tag}, func(*cepb.CloudEvent) error { <-release; return nil }, nil)
	_ = wedged.Send(ctx, mustCE(t)) // park the writer: the next enqueue has nowhere to go

	typed := createScheduledEntity(t, svc, ctx, modelName)
	assertClientErrorEnvelope(t, typed, "DISPATCH_TIMEOUT")
	if !strings.Contains(typed.Error.Message, "member not draining") {
		t.Errorf("message = %s", typed.Error.Message)
	}
	if typed.Error.Retryable == nil || !*typed.Error.Retryable {
		t.Error("expected retryable=true")
	}
}

// NoAnswer on a processor that is not idempotent: stop, the try's own code,
// and a second matching cnode is never asked.
func TestRPC_Processor_NoAnswer_NotIdempotent_OwnCodeEnvelope(t *testing.T) {
	const modelName = "grpc-proc-no-answer"
	const tag = "no-answer-tag"
	svc, wfHandler, ctx := newTestEnvWithDispatch(t)
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName,
		processorRPCWorkflowJSON("no-answer-wf", "charge", tag, 50))

	var asked atomic.Int32
	for _, id := range []string{"m-1", "m-2"} {
		svc.registry.Register(id, testTenant, []string{tag}, func(*cepb.CloudEvent) error {
			asked.Add(1)
			return nil // takes the work, never answers
		}, nil)
	}

	typed := createScheduledEntity(t, svc, ctx, modelName)
	assertClientErrorEnvelope(t, typed, "DISPATCH_TIMEOUT")
	if !strings.Contains(typed.Error.Message, "no response") {
		t.Errorf("message = %s", typed.Error.Message)
	}
	if typed.Error.Retryable == nil || !*typed.Error.Retryable {
		t.Error("expected retryable=true")
	}
	if got := asked.Load(); got != 1 {
		t.Errorf("%d cnodes were asked, want 1", got)
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/grpc/... -run 'TestRunLocal|TestMinorNumberer'`   Expected: FAIL (build) with `d.RunLocal undefined`, `call.Number undefined`, `undefined: NewMinorNumberer`.

- [ ] **Step 3: Implement**

`internal/grpc/callout.go` — in `Callout`, after `OwnerNodeID`:

```go
	// Number gives the fencing number of each try. RunLocal calls Next once
	// before every try, before it mints that try's pass.
	Number TryNumberer
```

`internal/grpc/run_local.go`:

```go
package grpc

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// TryNumberer gives the fencing number for the next try. RunLocal calls it
// once before each try, before it mints that try's pass.
//
// On the owner, Next raises the callout's major — which shuts the earlier
// cnode out and waits for its write in progress — and returns (major, 0). On a
// pnode that received a hand-over it counts minor = 1, 2, … under the major the
// hand-over carried. That is what lets one RunLocal make several tries while
// the number still rises before each.
type TryNumberer interface {
	Next() (major, minor uint32)
}

// MinorNumberer numbers the tries of a pnode that received a hand-over: (major,
// 1), (major, 2), … It touches no fence — the arbiter is the owner's. RunLocal
// calls it from one goroutine; it is not safe for concurrent use.
type MinorNumberer struct {
	major, minor uint32
}

func NewMinorNumberer(major uint32) *MinorNumberer { return &MinorNumberer{major: major} }

func (n *MinorNumberer) Next() (uint32, uint32) {
	n.minor++
	return n.major, n.minor
}

// LocalResult is what one run of the local procedure produced.
type LocalResult struct {
	// Result is the cnode's answer; meaningful when OK.
	Result CalloutResult
	// Failure is the last try's failure, or — when no matching cnode was there
	// to try — NoHandOff wrapping ErrNoMatchingMember. Whether another pnode
	// may be asked is Failure.Kind.MayTryAnother(call.RepeatSafe).
	Failure *contract.CalloutFailure
	// CtxErr is the caller's ctx.Err(), unchanged, when its context ended
	// during a try or between tries. Failure is then nil.
	CtxErr error
	// TriesUsed counts every cnode chosen, whether or not the hand-off
	// succeeded, a try in progress when the caller went away included.
	TriesUsed int
	// Attempts is one entry per failed try, in order.
	Attempts []contract.CalloutAttempt
}

// OK reports whether a cnode answered.
func (r LocalResult) OK() bool { return r.Failure == nil && r.CtxErr == nil }

// Err is the run's error: the caller's context error, else the failure, else nil.
func (r LocalResult) Err() error {
	if r.CtxErr != nil {
		return r.CtxErr
	}
	if r.Failure != nil {
		return r.Failure
	}
	return nil
}

// RunLocal tries the callout on this pnode's own matching cnodes, one after
// another, until one answers, a failure that forbids another try occurs, the
// matching cnodes are used up, or maxTries (at least 1) is reached.
//
// There is no pause between cnodes. A cnode is never tried twice within one
// run; the tried set lives in the call and is shared with nothing. The matching
// cnodes are looked up afresh before every try, so one that attaches during the
// run is seen. Every try sends call.RequestID. RunLocal never waits for a cnode
// to appear: when none is left it returns at once, and waiting is the owner's.
func (d *ProcessorDispatcher) RunLocal(ctx context.Context, call Callout, maxTries int) LocalResult {
	var res LocalResult
	var last *contract.CalloutFailure
	// Keyed by the Member, not its id: a cnode that re-registered under the
	// same id is a new connection and may be tried.
	tried := make(map[*Member]struct{})

	for res.TriesUsed < maxTries {
		if err := ctx.Err(); err != nil {
			res.CtxErr = err
			return res
		}
		var untried []*Member
		for _, m := range d.registry.Candidates(call.TenantID, call.Tags) {
			if _, done := tried[m]; !done {
				untried = append(untried, m)
			}
		}
		if len(untried) == 0 {
			break
		}
		member := d.selector.Select(untried)
		tried[member] = struct{}{}
		res.TriesUsed++
		major, minor := call.Number.Next()

		slog.Debug("callout try", "pkg", "grpc", "kind", call.Kind.String(), "name", call.Name,
			"memberId", member.ID, "entityId", call.EntityID, "requestId", call.RequestID,
			"try", res.TriesUsed, "major", major, "minor", minor)

		result, failure, ctxErr := d.dispatchCalloutToMember(ctx, member, call, d.resolveTxToken(ctx, call.TxID))
		if ctxErr != nil {
			res.CtxErr = ctxErr
			return res
		}
		if failure == nil {
			res.Result = result
			return res
		}
		res.Attempts = append(res.Attempts, contract.CalloutAttempt{MemberID: member.ID, Kind: failure.Kind, Cause: failure.Message})
		last = failure
		if !failure.Kind.MayTryAnother(call.RepeatSafe) {
			break
		}
	}

	if last == nil {
		slog.Debug("no matching calculation member", "pkg", "grpc", "tags", call.Tags, "entityId", call.EntityID)
		last = &contract.CalloutFailure{
			Kind:    contract.NoHandOff,
			Code:    common.ErrCodeNoComputeMemberForTag,
			Message: fmt.Sprintf("%s: no compute member for tags %q", common.ErrCodeNoComputeMemberForTag, call.Tags),
			Err:     fmt.Errorf("%w: tags %q", ErrNoMatchingMember, call.Tags),
		}
	}
	res.Failure = last
	return res
}
```

`internal/grpc/dispatch.go` — delete `dispatchOnce`; add:

```go
// singleTryNumberer numbers the one try an entry point below makes: the
// owner's first, (1, 0).
type singleTryNumberer struct{}

func (singleTryNumberer) Next() (uint32, uint32) { return 1, 0 }

// runSingleTry is the local procedure with one try, as the owner of the
// callout: what DispatchProcessor, DispatchCriteria and DispatchFunction do
// until the owner's loop takes their place.
func (d *ProcessorDispatcher) runSingleTry(ctx context.Context, call Callout) (CalloutResult, error) {
	limit, failure := d.ResolveAnswerLimit(call.ResponseTimeoutMs)
	if failure != nil {
		return CalloutResult{}, failure
	}
	call.RequestID = uuid.UUID(d.uuids.NewTimeUUID()).String()
	call.AnswerLimit = limit
	call.OwnerNodeID = d.selfNodeID
	call.Number = singleTryNumberer{}
	res := d.RunLocal(ctx, call, 1)
	return res.Result, res.Err()
}
```

and in the three `Dispatch*` methods replace `d.dispatchOnce(` with `d.runSingleTry(`.

`cmd/cyoda/help/content/grpc.md`, TAG ROUTING — add after the round-robin
paragraph of L-3:

```markdown
A callout may be tried on more than one member. **Every try carries the same `requestId`** (and the same `id`) in its payload, and the same `transactionId`. A member that de-duplicates on `requestId` will therefore treat a second delivery of the same callout — to itself after a reconnect, or seen by a shared de-duplication store behind several members — as a repeat, which is the intent. Criteria and functions must have no effects: they may be given to another member whenever one does not answer. A processor is given to another member after it was handed the work only if its configuration declares it `idempotent`.
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/grpc/...`, `go test ./internal/cluster/dispatch/...`, `go vet ./internal/grpc/...`   Expected: PASS.
Exit check: `grep -rn 'dispatchOnce' internal/` → no hits.

- [ ] **Step 5: Commit**

```
git add internal/grpc/run_local.go internal/grpc/run_local_test.go internal/grpc/callout.go internal/grpc/dispatch.go internal/grpc/callout_failure_rpc_test.go cmd/cyoda/help/content/grpc.md
git commit -m "feat(grpc): RunLocal tries a callout on this pnode's cnodes one after another (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task L-9: a try cut off by the callout's deadline is classified, not returned as the caller's error

**Spec:** §5 "The deadline is a context derived from the caller's … A try cut off by it is `NoAnswer`; only the caller's context ending … returns `ctx.Err()` unchanged … `context.Cause` tells the fence's cancellation (§7) from both"; "no try or hand-over starts after it". `RunLocal` is handed one context, so it is `RunLocal` that has to tell the two apart — see Open points 1. The §13 row "Callout deadline cuts off a try in progress" is the owner's-loop stream's; this task gives it the mechanism and tests the mechanism at the unit layer.

**Files:**
- Modify: `internal/contract/callout.go` (`ErrCalloutDeadline`)
- Modify: `internal/grpc/dispatch.go` (`dispatchCalloutToMember`: the two `ctx.Err() != nil` arms)
- Modify: `internal/grpc/run_local.go` (the check between tries; the no-try case)
- Test: `internal/grpc/run_local_deadline_test.go`

**Interfaces:**
- Consumes: L-8.
- Produces:
  - `var contract.ErrCalloutDeadline = errors.New("callout deadline passed")`
  - Contract for the owner's loop: derive the callout's context with `context.WithDeadlineCause(callerCtx, deadline, contract.ErrCalloutDeadline)` and pass **that** to `RunLocal`. Then a try in progress at the deadline is `NoAnswer` (after the hand-off) or `NoHandOff` (still enqueueing), code `DISPATCH_TIMEOUT`, recorded as an attempt; no further try starts; `LocalResult.CtxErr` stays nil. A context that ended for any other cause — the client went away, `transactionTimeoutMillis`, the fence's `ErrSuperseded` — comes back as `CtxErr`, unchanged, and the owner reads `context.Cause` itself.

- [ ] **Step 1: Write the failing tests**

```go
package grpc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func TestRunLocal_CalloutDeadline_CutsOffTheTryInProgress_AsNoAnswer(t *testing.T) {
	reg := NewMemberRegistry()
	attach(t, reg, "m-1", testTenantID, "x", nil) // takes the work, never answers
	_, second := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)
	ctx, cancel := context.WithDeadlineCause(testContext(), time.Now().Add(80*time.Millisecond), contract.ErrCalloutDeadline)
	defer cancel()

	// repeat-safe and four tries: only the deadline can stop the second try
	res := d.RunLocal(ctx, processorCall("x", true, 30*time.Second), 4)
	if res.CtxErr != nil {
		t.Fatalf("CtxErr = %v: the callout's own deadline is not the caller going away", res.CtxErr)
	}
	if res.Failure == nil || res.Failure.Kind != contract.NoAnswer || appCode(res.Err()) != common.ErrCodeDispatchTimeout {
		t.Fatalf("failure = %+v, want NoAnswer DISPATCH_TIMEOUT", res.Failure)
	}
	if !strings.Contains(res.Failure.Message, "callout deadline") {
		t.Errorf("message = %q, should say what cut the try off", res.Failure.Message)
	}
	if res.TriesUsed != 1 || len(res.Attempts) != 1 || second.count() != 0 {
		t.Errorf("TriesUsed = %d attempts = %d second asked = %d; no try starts after the deadline", res.TriesUsed, len(res.Attempts), second.count())
	}
}

func TestRunLocal_CalloutDeadline_DuringEnqueue_IsNoHandOff(t *testing.T) {
	d, _ := newWedgedDispatcher(t)
	ctx, cancel := context.WithDeadlineCause(testContext(), time.Now().Add(80*time.Millisecond), contract.ErrCalloutDeadline)
	defer cancel()

	res := d.RunLocal(ctx, processorCall("python", false, 30*time.Second), 4)
	if res.CtxErr != nil || res.Failure == nil || res.Failure.Kind != contract.NoHandOff || appCode(res.Err()) != common.ErrCodeDispatchTimeout {
		t.Fatalf("res = %+v, want NoHandOff DISPATCH_TIMEOUT and no CtxErr", res)
	}
}

func TestRunLocal_CalloutDeadline_AlreadyPassed_MakesNoTry(t *testing.T) {
	reg := NewMemberRegistry()
	_, a := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
	d := newTestDispatcher(t, reg)
	ctx, cancel := context.WithDeadlineCause(testContext(), time.Now().Add(-time.Second), contract.ErrCalloutDeadline)
	defer cancel()

	res := d.RunLocal(ctx, processorCall("x", true, 5*time.Second), 4)
	if res.CtxErr != nil || res.TriesUsed != 0 || a.count() != 0 {
		t.Fatalf("res = %+v asked = %d; want no try and no CtxErr", res, a.count())
	}
	if res.Failure == nil || res.Failure.Kind != contract.NoHandOff || !errors.Is(res.Err(), contract.ErrCalloutDeadline) {
		t.Errorf("failure = %+v, want NoHandOff wrapping ErrCalloutDeadline", res.Failure)
	}
	if errors.Is(res.Err(), ErrNoMatchingMember) {
		t.Error("a cnode was there; this must not read as \"no compute member\"")
	}
}

// The caller's own deadline is still the caller's: ctx.Err() unchanged.
func TestRunLocal_CallersOwnDeadline_IsCtxErrUnchanged(t *testing.T) {
	reg := NewMemberRegistry()
	attach(t, reg, "m-1", testTenantID, "x", nil)
	d := newTestDispatcher(t, reg)
	ctx, cancel := context.WithTimeout(testContext(), 80*time.Millisecond)
	defer cancel()

	res := d.RunLocal(ctx, processorCall("x", true, 30*time.Second), 4)
	if res.CtxErr != context.DeadlineExceeded || res.Failure != nil {
		t.Fatalf("CtxErr = %v Failure = %v, want context.DeadlineExceeded and no failure", res.CtxErr, res.Failure)
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/grpc/... -run 'TestRunLocal_CalloutDeadline|TestRunLocal_CallersOwnDeadline'`   Expected: FAIL (build) with `undefined: contract.ErrCalloutDeadline`.

- [ ] **Step 3: Implement**

`internal/contract/callout.go` — add `"errors"` to the imports and, above `CalloutFailureKind`:

```go
// ErrCalloutDeadline is the cause an owner gives the context it derives for a
// callout's hard limit on time (context.WithDeadlineCause). The local procedure
// reads it with context.Cause to tell "the callout ran out of time" — the try
// in progress is classified and no further try starts — from "the caller went
// away", which is returned as ctx.Err(), unchanged.
var ErrCalloutDeadline = errors.New("callout deadline passed")
```

`internal/grpc/dispatch.go` — add:

```go
// calloutDeadlinePassed reports whether ctx ended because the callout's own
// deadline passed, as opposed to its caller going away.
func calloutDeadlinePassed(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), contract.ErrCalloutDeadline)
}
```

In `dispatchCalloutToMember`, the enqueue arm — before:

```go
		case ctx.Err() != nil:
			return CalloutResult{}, nil, ctx.Err()
```

after:

```go
		case ctx.Err() != nil:
			if calloutDeadlinePassed(ctx) {
				slog.Error("dispatch cut off by the callout deadline", "pkg", "grpc", "phase", "enqueue", "memberId", member.ID, "label", label, "name", name, "requestId", requestID)
				return CalloutResult{}, appFailure(contract.NoHandOff, common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
					fmt.Sprintf("%s dispatch cut off at the callout deadline: member not draining", label)).AsRetryable()), nil
			}
			return CalloutResult{}, nil, ctx.Err()
```

and the response arm — before:

```go
		if ctx.Err() != nil {
			return CalloutResult{}, nil, ctx.Err()
		}
```

after:

```go
		if ctx.Err() != nil {
			if calloutDeadlinePassed(ctx) {
				slog.Error("dispatch cut off by the callout deadline", "pkg", "grpc", "phase", "response", "memberId", member.ID, "label", label, "name", name, "requestId", requestID)
				return CalloutResult{}, appFailure(contract.NoAnswer, common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
					fmt.Sprintf("%s dispatch cut off at the callout deadline: no response", label)).AsRetryable()), nil
			}
			return CalloutResult{}, nil, ctx.Err()
		}
```

`internal/grpc/run_local.go` — the check at the top of the loop, before:

```go
		if err := ctx.Err(); err != nil {
			res.CtxErr = err
			return res
		}
```

after:

```go
		if err := ctx.Err(); err != nil {
			if calloutDeadlinePassed(ctx) {
				break // no try starts after the callout's deadline
			}
			res.CtxErr = err
			return res
		}
```

and the no-try case, before:

```go
	if last == nil {
```

after:

```go
	if last == nil && calloutDeadlinePassed(ctx) {
		appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeDispatchTimeout,
			"the callout deadline passed before a try could start").AsRetryable().WithCause(contract.ErrCalloutDeadline)
		last = appFailure(contract.NoHandOff, appErr)
	}
	if last == nil {
```

(`run_local.go` gains the `"net/http"` import.)

`WithCause` exists on `*common.AppError` (`internal/common/errors.go:89`) and
sets `Err`, which `Unwrap` returns — that is what makes
`errors.Is(res.Err(), contract.ErrCalloutDeadline)` hold.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/grpc/... ./internal/contract/...`   Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/contract/callout.go internal/grpc/dispatch.go internal/grpc/run_local.go internal/grpc/run_local_deadline_test.go
git commit -m "feat(grpc): a try cut off by the callout's deadline is classified, the caller's cancellation is not (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task L-10: every try mints its own pass, which lives as long as the try

**Depends on:** stream **F** (`token.Claims`, `token.Pair`, `Signer.Issue`), stream **C** (the pass allowance on `app.Config`).

**Spec:** §7 "Claims": "A pass is minted per try by the pnode that makes the hand-off …, with `NodeID` = the owner's id and `ExpiresAt = now + answer limit + CYODA_CALLOUT_PASS_ALLOWANCE`"; §4 "Every try mints its own pass"; §9 (`CYODA_TX_TOKEN_TTL` removed: a pass no longer has a fixed life). §13 row "Pass lifetime follows the answer limit" — **U**.

**Files:**
- Modify: `internal/grpc/callout.go` (`Callout.Outer`)
- Modify: `internal/grpc/dispatch.go` (`resolveTxToken` :60-73 → `mintPass`; struct, constructor: `tokenTTL` → `passAllowance`)
- Modify: `internal/grpc/run_local.go` (mint before the try; a mint failure is `Terminal`)
- Modify: `app/app.go:441`
- Modify: `internal/grpc/dispatch_test.go` (`newTestDispatcher`, `tryOnce`), `internal/grpc/scheduled_function_rpc_test.go` (`newTestEnvWithDispatchLimits`)
- Delete: `internal/grpc/dispatch_txtoken_test.go` — `TestDispatch_MintsTxTokenFromTxID` and `TestDispatch_EmptyTxIDNoToken` are rewritten in the new file against `mintPass`; `TestDispatch_CtxTokenOverridesSelfMint` moves there unchanged in substance (it is deleted by L-11); `make32` moves to `dispatch_test.go`
- Test: `internal/grpc/pass_mint_test.go`

**Interfaces:**
- Consumes (stream **F**, exactly):
  - `type token.Pair struct { Callout string; Major, Minor uint32 }`
  - `type token.Claims struct { NodeID, TxRef string; ExpiresAt int64; Callout string; Major, Minor uint32; Outer []token.Pair }` — `ExpiresAt` stays unix seconds, as today (`token.go:22`)
  - `func (s *token.Signer) Issue(claims token.Claims) (string, error)`; `Verify` returns the new claims.
  Stream F changes `dispatch.go:67` only as far as it must to compile; this task replaces that line.
- Consumes (stream **C**, C-1): `cfg.Callout.PassAllowance time.Duration` (`CYODA_CALLOUT_PASS_ALLOWANCE`).
- Produces:
  - `Callout.Outer []token.Pair` — the enclosing pairs, copied into every pass; the owner fills it from `fence.Pairs(ctx)`, a pnode that received a hand-over from the request.
  - `func NewProcessorDispatcher(registry *MemberRegistry, selector MemberSelector, uuids spi.UUIDGenerator, signer *token.Signer, selfNodeID string, answerLimitDefault, answerLimitMax, passAllowance time.Duration) *ProcessorDispatcher` — final.
  - Every pass: `NodeID = call.OwnerNodeID`, `TxRef = call.TxID`, `Callout = call.RequestID`, `(Major, Minor)` = what `call.Number.Next()` returned for that try, `Outer = call.Outer`, `ExpiresAt = now + call.AnswerLimit + passAllowance`. No pass when `call.TxID == ""`, as today — `Next` is still called.

**Sequencing note for whoever integrates (see Open points 4):** from this task on, the passes of the three entry points name a callout (`RequestID`, `(1, 0)`) that nobody has registered with the fence, because `fence.Begin` is the Coordinator's. The fence's check in `txjoin.JoinFromToken` must therefore not land before the Coordinator replaces these entry points.

**TDD waiver.** The mint-failure branch is not unit-tested: `Signer.Issue` fails only if `json.Marshal` of the claims fails, which no input reachable from a `Callout` can cause, and making it fail would need a seam that exists only for the test. The branch is reviewed: it fails the callout (`Terminal`, a ticketed 500) where today's code logged and **sent the work without a pass** — a wrong-but-available result.

- [ ] **Step 1: Write the failing tests**

`internal/grpc/pass_mint_test.go`:

```go
package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
)

func verifyPass(t *testing.T, pass string) *token.Claims {
	t.Helper()
	signer, err := token.NewSigner(make32(t)) // the secret newTestDispatcher signs with
	if err != nil {
		t.Fatal(err)
	}
	claims, err := signer.Verify(pass)
	if err != nil {
		t.Fatalf("verify pass: %v", err)
	}
	return claims
}

func onlyPass(t *testing.T, a *asked) string {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.passes) != 1 {
		t.Fatalf("cnode saw %d requests, want 1", len(a.passes))
	}
	return a.passes[0]
}

func TestRunLocal_PassLifetimeFollowsTheAnswerLimit(t *testing.T) {
	for _, limit := range []time.Duration{2 * time.Second, 20 * time.Second} {
		t.Run(limit.String(), func(t *testing.T) {
			reg := NewMemberRegistry()
			_, a := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
			d := newTestDispatcher(t, reg) // pass allowance 3s

			before := time.Now()
			if res := d.RunLocal(testContext(), processorCall("x", false, limit), 1); !res.OK() {
				t.Fatalf("res = %+v", res)
			}
			claims := verifyPass(t, onlyPass(t, a))
			want := before.Add(limit + 3*time.Second)
			got := time.Unix(claims.ExpiresAt, 0)
			// ExpiresAt is whole seconds: allow the truncation and the run's own time.
			if got.Before(want.Add(-time.Second)) || got.After(want.Add(2*time.Second)) {
				t.Errorf("ExpiresAt = %v, want about %v (now + answer limit + pass allowance)", got, want)
			}
		})
	}
}

func TestRunLocal_EveryTryMintsItsOwnPass(t *testing.T) {
	reg := NewMemberRegistry()
	attachGone(t, reg, "m-1", testTenantID, "x")           // try 1: numbered (1,0), no hand-off
	_, a2 := attach(t, reg, "m-2", testTenantID, "x", nil) // try 2: (2,0), no answer
	_, a3 := attach(t, reg, "m-3", testTenantID, "x", answersAs("m-3"))
	d := newTestDispatcher(t, reg)
	call := processorCall("x", true, 50*time.Millisecond)
	call.OwnerNodeID = "node-owner"
	call.Outer = []token.Pair{{Callout: "outer-req", Major: 4, Minor: 2}}

	if res := d.RunLocal(testContext(), call, 4); !res.OK() || res.TriesUsed != 3 {
		t.Fatalf("res = %+v", res)
	}
	second, third := verifyPass(t, onlyPass(t, a2)), verifyPass(t, onlyPass(t, a3))
	if onlyPass(t, a2) == onlyPass(t, a3) {
		t.Fatal("two tries were given the same pass")
	}
	for i, c := range []*token.Claims{second, third} {
		if c.NodeID != "node-owner" || c.TxRef != "tx-1" || c.Callout != "req-fixed" {
			t.Errorf("pass %d: NodeID/TxRef/Callout = %s/%s/%s", i, c.NodeID, c.TxRef, c.Callout)
		}
		if len(c.Outer) != 1 || c.Outer[0] != call.Outer[0] {
			t.Errorf("pass %d: Outer = %+v, want the callout's enclosing pairs", i, c.Outer)
		}
	}
	if second.Major != 2 || second.Minor != 0 || third.Major != 3 || third.Minor != 0 {
		t.Errorf("numbers = (%d,%d) then (%d,%d), want (2,0) then (3,0)", second.Major, second.Minor, third.Major, third.Minor)
	}
}

func TestRunLocal_HandOverNumbering_MinorRisesUnderTheGivenMajor(t *testing.T) {
	reg := NewMemberRegistry()
	_, a1 := attach(t, reg, "m-1", testTenantID, "x", nil)
	_, a2 := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)
	call := processorCall("x", true, 50*time.Millisecond)
	call.Number = NewMinorNumberer(5)

	if res := d.RunLocal(testContext(), call, 4); !res.OK() {
		t.Fatalf("res = %+v", res)
	}
	first, second := verifyPass(t, onlyPass(t, a1)), verifyPass(t, onlyPass(t, a2))
	if first.Major != 5 || first.Minor != 1 || second.Major != 5 || second.Minor != 2 {
		t.Errorf("numbers = (%d,%d) then (%d,%d), want (5,1) then (5,2)", first.Major, first.Minor, second.Major, second.Minor)
	}
}

func TestRunLocal_NoTransaction_NoPass_ButStillNumbered(t *testing.T) {
	reg := NewMemberRegistry()
	_, a := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
	d := newTestDispatcher(t, reg)
	call := armed(NewProcessorCallout(testTenantID, testEntity(), testProcessor("x", 0), "wf1", "t1", ""), false, 5*time.Second)
	numberer := call.Number.(*countingNumberer)

	if res := d.RunLocal(testContext(), call, 1); !res.OK() {
		t.Fatalf("res = %+v", res)
	}
	if pass := onlyPass(t, a); pass != "" {
		t.Errorf("a callout outside a transaction carries no pass, got one")
	}
	if numberer.major != 1 {
		t.Errorf("Next was called %d times, want 1", numberer.major)
	}
}

// Until the hand-over stops pre-minting (the last task of this stream), a pass
// already on the context still wins.
func TestMintPass_PassOnContextWins(t *testing.T) {
	d := newTestDispatcher(t, NewMemberRegistry())
	ctx := WithTxToken(context.Background(), "pre-minted-by-the-owner")
	pass, err := d.mintPass(ctx, processorCall("x", false, time.Second), 1, 0)
	if err != nil || pass != "pre-minted-by-the-owner" {
		t.Fatalf("mintPass = (%q, %v)", pass, err)
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/grpc/... -run 'TestRunLocal_Pass|TestRunLocal_EveryTryMints|TestRunLocal_HandOverNumbering|TestRunLocal_NoTransaction|TestMintPass'`   Expected: FAIL (build) with `call.Outer undefined`, `d.mintPass undefined`.

- [ ] **Step 3: Implement**

`internal/grpc/callout.go` — add the `token` import and, in `Callout` after `Number`:

```go
	// Outer names every enclosing callout, for a callout made from inside a
	// callback; it is copied into every pass.
	Outer []token.Pair
```

`internal/grpc/dispatch.go` — struct and constructor (final):

```go
type ProcessorDispatcher struct {
	registry           *MemberRegistry
	selector           MemberSelector
	uuids              spi.UUIDGenerator
	signer             *token.Signer
	selfNodeID         string
	answerLimitDefault time.Duration
	answerLimitMax     time.Duration
	passAllowance      time.Duration
}

func NewProcessorDispatcher(registry *MemberRegistry, selector MemberSelector, uuids spi.UUIDGenerator, signer *token.Signer, selfNodeID string, answerLimitDefault, answerLimitMax, passAllowance time.Duration) *ProcessorDispatcher {
	return &ProcessorDispatcher{
		registry:           registry,
		selector:           selector,
		uuids:              uuids,
		signer:             signer,
		selfNodeID:         selfNodeID,
		answerLimitDefault: answerLimitDefault,
		answerLimitMax:     answerLimitMax,
		passAllowance:      passAllowance,
	}
}
```

Replace `resolveTxToken` with:

```go
// mintPass issues the pass one try gives its cnode: the transaction token the
// cnode's callbacks carry. It names the owner — callbacks are routed there
// whichever pnode made the hand-off — the transaction, the callout and the
// try's fencing number, and it lives as long as the try may: the answer limit
// plus an allowance for routing and for clocks that differ between pnodes. A
// callout outside a transaction carries no pass.
//
// A pass already on ctx wins: the hand-over still pre-mints one.
func (d *ProcessorDispatcher) mintPass(ctx context.Context, call Callout, major, minor uint32) (string, error) {
	if pass := TxTokenFromContext(ctx); pass != "" {
		return pass, nil
	}
	if call.TxID == "" {
		return "", nil
	}
	pass, err := d.signer.Issue(token.Claims{
		NodeID:    call.OwnerNodeID,
		TxRef:     call.TxID,
		ExpiresAt: time.Now().Add(call.AnswerLimit + d.passAllowance).Unix(),
		Callout:   call.RequestID,
		Major:     major,
		Minor:     minor,
		Outer:     call.Outer,
	})
	if err != nil {
		return "", fmt.Errorf("failed to mint transaction pass: %w", err)
	}
	return pass, nil
}
```

(The `d.signer == nil` guard goes: `app.go:148-170` always builds a signer, and
every test constructs one.)

`internal/grpc/run_local.go` — before:

```go
		result, failure, ctxErr := d.dispatchCalloutToMember(ctx, member, call, d.resolveTxToken(ctx, call.TxID))
```

after:

```go
		pass, err := d.mintPass(ctx, call, major, minor)
		if err != nil {
			// Would fail identically for any cnode; and a try without its pass
			// would leave the cnode's callbacks outside the transaction.
			last = appFailure(contract.Terminal, common.Internal("failed to mint transaction pass", err))
			res.Attempts = append(res.Attempts, contract.CalloutAttempt{MemberID: member.ID, Kind: contract.Terminal, Cause: "internal error"})
			break
		}
		result, failure, ctxErr := d.dispatchCalloutToMember(ctx, member, call, pass)
```

No pass, secret or claim is logged: the DEBUG line of a try logs the payload
preview (`ce.GetTextData()`), and the pass is a CloudEvent *attribute*.

`app/app.go:441`:

```go
	localDispatcher := internalgrpc.NewProcessorDispatcher(a.memberRegistry, internalgrpc.NewRoundRobinSelector(a.memberRegistry), common.NewDefaultUUIDGenerator(), a.tokenSigner, a.selfNodeID, cfg.Callout.ResponseTimeout, cfg.Callout.ResponseTimeoutMax, cfg.Callout.PassAllowance)
```

(`cfg.Cluster.TxTokenTTL` is still read by `NewClusterDispatcher` at
`app.go:554`; the setting is removed by the stream that removes that reader.)

Tests: `newTestDispatcher` becomes

```go
	return NewProcessorDispatcher(registry, NewRoundRobinSelector(registry), common.NewTestUUIDGenerator(), signer, "node-test", 30*time.Second, 60*time.Second, 3*time.Second)
```

`newTestEnvWithDispatchLimits` passes `answerLimitDefault, answerLimitMax, 30*time.Second`;
in `tryOnce` replace `d.resolveTxToken(ctx, txID)` with a pass minted the same way:

```go
	pass, err := d.mintPass(ctx, call, 1, 0)
	if err != nil {
		return nil, err
	}
	_, failure, ctxErr := d.dispatchCalloutToMember(ctx, member, call, pass)
```

Move `make32` into `dispatch_test.go` and delete `dispatch_txtoken_test.go`.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/grpc/... ./internal/cluster/...`, `go build ./...`, `go vet ./internal/grpc/... ./app/...`   Expected: PASS.
Exit check: `grep -rn 'resolveTxToken\|tokenTTL' internal/grpc/` → no hits.

- [ ] **Step 5: Commit**

```
git add -A internal/grpc app/app.go
git commit -m "feat(grpc): every try mints its own pass, numbered, living as long as the try (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task L-11: a pass on the context no longer wins; `WithTxToken` and `TxTokenFromContext` go

**Depends on:** stream **P** having removed `DispatchCalloutRequest.TxToken` and every use of `internalgrpc.WithTxToken` / `TxTokenFromContext` outside `internal/grpc` — today `internal/cluster/dispatch/handler.go:75`, `cluster_dispatcher.go:75, 123, 168`, `integration_txtoken_test.go:122, 185`, `cluster_txtoken_test.go`. Until then this task does not compile; it is this stream's last.

**Spec:** §7 "Claims": "`resolveTxToken`'s rule "a token already on ctx wins" is removed, with `DispatchCalloutRequest.TxToken`, `WithTxToken` and `TxTokenFromContext`."

**Files:**
- Modify: `internal/grpc/txtoken.go` (delete `txTokenCtxKey`, `WithTxToken`, `TxTokenFromContext`, lines 38-47, and the `context` import)
- Modify: `internal/grpc/dispatch.go` (`mintPass` loses `ctx` and its first branch)
- Modify: `internal/grpc/run_local.go`, `internal/grpc/dispatch_test.go` (`tryOnce`) — the two callers of `mintPass`
- Modify: `internal/grpc/txtoken_test.go` (delete `TestTxTokenContextRoundTrip`), `internal/grpc/pass_mint_test.go` (delete `TestMintPass_PassOnContextWins`; add the test below)

**Interfaces:**
- Consumes: stream P's removal (above).
- Produces: `func (d *ProcessorDispatcher) mintPass(call Callout, major, minor uint32) (string, error)`. The pnode that makes the hand-off is the only source of a try's pass.

**Existing tests:** `TestTxTokenContextRoundTrip` (`txtoken_test.go:26`) — deleted with the functions it tests. `TestAttachAndReadTxToken`, `TestAttachTxToken_EmptyIsNoop` — stay.

- [ ] **Step 1: Write the failing test**

Add to `internal/grpc/pass_mint_test.go`:

```go
// A pnode that received a hand-over mints the passes of its own tries, naming
// the owner — nothing reaches a cnode that the pnode making the hand-off did
// not mint for that try.
func TestRunLocal_OnAPnodeThatIsNotTheOwner_MintsPassesNamingTheOwner(t *testing.T) {
	reg := NewMemberRegistry()
	_, a := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
	d := newTestDispatcher(t, reg) // this pnode is "node-test"
	call := processorCall("x", true, 5*time.Second)
	call.OwnerNodeID = "node-owner"
	call.Number = NewMinorNumberer(3)

	if res := d.RunLocal(testContext(), call, 1); !res.OK() {
		t.Fatalf("res = %+v", res)
	}
	claims := verifyPass(t, onlyPass(t, a))
	if claims.NodeID != "node-owner" || claims.Major != 3 || claims.Minor != 1 {
		t.Errorf("claims = %+v, want NodeID node-owner and number (3,1)", claims)
	}
}
```

and delete `TestMintPass_PassOnContextWins` and `TestTxTokenContextRoundTrip`.
(The new test already passes after L-10; what this task's RED shows is the
deletion: see Step 2.)

- [ ] **Step 2: Run to verify RED**

Delete `WithTxToken` / `TxTokenFromContext` from `txtoken.go` first.
Run: `go build ./internal/grpc/...`   Expected: FAIL with `undefined: TxTokenFromContext` in `dispatch.go` — the one remaining reader, which Step 3 removes.

- [ ] **Step 3: Implement**

`internal/grpc/dispatch.go` — `mintPass`, before:

```go
func (d *ProcessorDispatcher) mintPass(ctx context.Context, call Callout, major, minor uint32) (string, error) {
	if pass := TxTokenFromContext(ctx); pass != "" {
		return pass, nil
	}
	if call.TxID == "" {
```

after (and the doc comment loses its last sentence, "A pass already on ctx wins …"):

```go
func (d *ProcessorDispatcher) mintPass(call Callout, major, minor uint32) (string, error) {
	if call.TxID == "" {
```

`run_local.go`: `d.mintPass(ctx, call, major, minor)` → `d.mintPass(call, major, minor)`;
`tryOnce` in `dispatch_test.go`: `d.mintPass(ctx, call, 1, 0)` → `d.mintPass(call, 1, 0)`.

`internal/grpc/txtoken.go` ends after `TxTokenFromCloudEvent`; its import block
keeps only `cepb`.

- [ ] **Step 4: Run to verify GREEN**

Run: `go build ./...`, `go test ./internal/grpc/... ./internal/cluster/...`, `go vet ./internal/grpc/...`   Expected: PASS.
Exit check: `grep -rn 'WithTxToken\|TxTokenFromContext\|txTokenCtxKey' --include='*.go' .` → no hits.

- [ ] **Step 5: Commit**

```
git add internal/grpc/txtoken.go internal/grpc/txtoken_test.go internal/grpc/dispatch.go internal/grpc/run_local.go internal/grpc/dispatch_test.go internal/grpc/pass_mint_test.go
git commit -m "refactor(grpc): the pnode that makes the hand-off is the only source of a try's pass (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## Coverage matrix — this stream's rows (§13)

| Scenario | U | G | Left to the owner's-loop stream |
|---|---|---|---|
| Every dispatcher error site → its kind | L-7 `try_kind_test.go` (every row of the §3 table that is inside a try); L-8 (`no matching member`, between-tries ctx); L-5 (stored limit over the bound); L-6 (criterion does not parse) | — | — |
| `NoHandOff` → next local cnode answers | L-8 `TestRunLocal_NoHandOff_NextCnodeAnswers` | — | E, P waived in the spec (w¹) |
| `NoHandOff`, tries = 1 → the try's own code | L-8 `TestRunLocal_NoHandOff_OneTry_ReportsTheTrysOwnCode` (both causes) | L-8 `TestRPC_Processor_NoHandOff_OwnCodeEnvelope` | E waived (w¹) |
| `NoAnswer`, processor not idempotent → stop, 503 own code | L-8 `TestRunLocal_NoAnswer_NotRepeatSafe_Stops` | L-8 `TestRPC_Processor_NoAnswer_NotIdempotent_OwnCodeEnvelope` — valid as long as the entry point makes one try; with tries > 1 it needs `idempotent: false` and the Coordinator in `newTestEnvWithDispatch`, which that stream wires | E, P |
| `NoAnswer`, `idempotent` → next cnode answers | L-8 `TestRunLocal_NoAnswer_RepeatSafe_NextCnodeAnswers/processor` | — | E, P (09_10) |
| `NoAnswer`, criterion; function → next cnode answers | L-8 same test, `/criterion`, `/function` | — | E, P |
| cnode drops after hand-off — both settings | L-8 `TestRunLocal_CnodeDropsAfterHandOff` | — | E, P (09_09, 09_11) |
| `MemberFailed` verdict true → one try | L-7 `TestTryKind_MemberAnsweredFailure…`, L-8 `TestRunLocal_MemberFailed_…/verdict true` | L-7 `TestRPC_ProcessorMemberFailed_…` pins code, message, one try. The envelope's `retryable: true` comes from the `classifyWorkflowError` branch (§8.2), which is not this stream's: that stream adds the one assertion to this test | E, P |
| `MemberFailed` verdict false / absent → one try | L-7, L-8 (same tests) | L-7 `TestRPC_ProcessorMemberFailed_…` | E, P |
| `Terminal` → stop, as today | L-7 (three Terminal sites), L-8 `TestRunLocal_Terminal_Stops` | L-5 `TestRPC_Processor_StoredTimeoutOverLoweredBound_Envelope` (the 400 arm). The ticketed-500 arm (auth context) cannot be reached through `EntityManage`, which always has a user context | E |
| Same request id on every try | L-8 `TestRunLocal_SameRequestIDOnEveryTry`; L-6 (each builder uses the id it is given for `id` and `requestId`) | — | E |
| Round robin across two cnodes; a new cnode goes first | L-3 `selector_test.go`; L-8 `TestRunLocal_RoundRobinAcrossCallouts_…` | — | E |
| Two tenants share a tag on one pnode | L-2 `TestCandidates_TenantAndTagFilter`; L-8 `TestRunLocal_TwoTenantsShareATag` | — | E, M |
| Pass lifetime follows the answer limit | L-10 `TestRunLocal_PassLifetimeFollowsTheAnswerLimit` | — | — |
| Stored `responseTimeoutMs` over a lowered bound → `Terminal` | L-5 `TestResolveAnswerLimit`, `TestDispatch_StoredTimeoutOverTheBound_…` | L-5 (extra) | — |

Concurrency: `TestRoundRobinSelector_ConcurrentPicksAreSpreadEvenly` (L-3) and
`TestRunLocal_SeesACnodeAttachedDuringTheRun` (L-8) assert consistency, not an
interleaving, and run under `make race` with the rest of `internal/grpc`.

## Stream interface summary

**Other streams may consume from L**

`internal/contract`:
- `type CalloutFailureKind int` — `NoHandOff, NoAnswer, MemberFailed, Terminal`; `String() string` (`no_handoff|no_answer|member_failed|terminal`); `MayTryAnother(repeatSafe bool) bool`
- `type CalloutAttempt struct { MemberID string; Kind CalloutFailureKind; Cause string }`
- `type CalloutFailure struct { Kind CalloutFailureKind; Code, Message string; Retryable *bool; Attempts []CalloutAttempt; Err error }`; `Error() string` (`Err`'s text, else `Message`); `Unwrap() error`. For a kind that has an `*common.AppError`, `Err` is that error and `Message` repeats its `"CODE: text"`. `MemberFailed` has `Err == nil`, `Code == ""`, `Message` = the cnode's text.
- `var ErrCalloutDeadline error` — give it as the cause of the callout's deadline context (`context.WithDeadlineCause`).

`internal/grpc`:
- `func (r *MemberRegistry) Candidates(tenantID spi.TenantID, tagsCSV string) []*Member`
- `func (r *MemberRegistry) Changed() <-chan struct{}` — take it before looking.
- `type MemberSelector interface { Select(candidates []*Member) *Member }`; `func NewRoundRobinSelector(registry *MemberRegistry) *RoundRobinSelector`
- `type CalloutKind int` (`ProcessorCallout, CriteriaCallout, FunctionCallout`; `String()` = `processor|criteria|function`); `type CalloutResult struct { Entity *spi.Entity; Matches bool; Reason string; Function contract.FunctionResult }`
- `type Callout struct { Kind; Name; TenantID; Tags; ResponseTimeoutMs; TxID; EntityID; RequestID; AnswerLimit; RepeatSafe; OwnerNodeID; Number TryNumberer; Outer []token.Pair; … }`
- `func NewProcessorCallout(tenantID spi.TenantID, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName, transitionName, txID string) Callout` (`RepeatSafe` false — the owner sets it from `Config.Idempotent`)
- `func NewCriteriaCallout(tenantID spi.TenantID, entity *spi.Entity, criterion json.RawMessage, target, workflowName, transitionName, processorName, txID string) (Callout, *contract.CalloutFailure)`
- `func NewFunctionCallout(tenantID spi.TenantID, entity *spi.Entity, fn spi.ScheduleFunction, workflowName, transitionName, txID string) Callout`
- `type TryNumberer interface { Next() (major, minor uint32) }`; `func NewMinorNumberer(major uint32) *MinorNumberer`
- `type LocalResult struct { Result CalloutResult; Failure *contract.CalloutFailure; CtxErr error; TriesUsed int; Attempts []contract.CalloutAttempt }`; `OK() bool`; `Err() error`
- `func (d *ProcessorDispatcher) RunLocal(ctx context.Context, call Callout, maxTries int) LocalResult`
- `func (d *ProcessorDispatcher) ResolveAnswerLimit(responseTimeoutMs int64) (time.Duration, *contract.CalloutFailure)` — the owner only.
- `func NewProcessorDispatcher(registry *MemberRegistry, selector MemberSelector, uuids spi.UUIDGenerator, signer *token.Signer, selfNodeID string, answerLimitDefault, answerLimitMax, passAllowance time.Duration) *ProcessorDispatcher`
- **To be deleted by the owner's-loop stream**, once `internal/callout.Coordinator` is the `contract.ExternalProcessingService`: `(*ProcessorDispatcher).DispatchProcessor / DispatchCriteria / DispatchFunction`, `runSingleTry`, `singleTryNumberer`, the `uuids` field and constructor parameter, and the `ErrNoMatchingMember` alias if nothing reads it then. Exit check for that stream: `grep -rn 'runSingleTry\|singleTryNumberer' internal/` → no hits. It also re-points the G tests' environment (`newTestEnvWithDispatchLimits`) at the Coordinator, and owns `docs/ARCHITECTURE.md:604-612`.

**L consumes**

- Stream **F**: `token.Pair{Callout string; Major, Minor uint32}`; `token.Claims{NodeID, TxRef string; ExpiresAt int64; Callout string; Major, Minor uint32; Outer []token.Pair}`; `(*token.Signer).Issue(claims token.Claims) (string, error)` — L-10.
- Stream **C**: three `time.Duration` values on `app.Config` — answer-limit default, answer-limit upper bound (L-5), pass allowance (L-10) — passed as plain constructor parameters. They are `cfg.Callout.ResponseTimeout`, `cfg.Callout.ResponseTimeoutMax`, `cfg.Callout.PassAllowance` (C-1). Also `contract.ParseCriterionFunction(criterion json.RawMessage) (contract.CriterionFunction, error)` (C-5), used by `NewCriteriaCallout` (L-6). C also owns the help topics, `README.md`, `DefaultConfig()` and `config_registry.go` for those settings, and the removal of `CYODA_TX_TOKEN_TTL` (still read at `app.go:554` after L-10).
- Stream **P**: removal of `DispatchCalloutRequest.TxToken` and of every use of `WithTxToken` / `TxTokenFromContext` outside `internal/grpc` — before L-11.
- The stream that owns `classifyWorkflowError` (§8.2): the `MemberFailed` branch. Until it lands a `MemberFailed` failure is a non-retryable 400 `WORKFLOW_FAILED` by the catch-all, with the final message text.

## Open points

1. **§5 vs §4 — how `RunLocal` tells the callout's deadline from the caller going away.** §5 says the deadline "is a context derived from the caller's" and that a try cut off by it is `NoAnswer` while the caller's context ending returns `ctx.Err()` unchanged; §4 gives `RunLocal` one `ctx`. Inside `dispatchCalloutToMember` both look the same (`ctx.Err() != nil`, `dispatch.go:154, 187`). Planned (L-9): the owner derives the context with `context.WithDeadlineCause(…, contract.ErrCalloutDeadline)` and `RunLocal` reads `context.Cause`. This adds one exported sentinel the spec does not list, and gives `internal/grpc` a task the §13 row assigns only at U. The owner's-loop plan must use exactly that cause.
2. **§4 "When no untried matching cnode remains it returns `NoHandOff`".** Taken literally this relabels a last try that was `NoAnswer` (repeat-safe, cnodes used up) as `NoHandOff`, and would lose its code for "exactly one attempt recorded → that attempt's own code" (§8.2). Planned: `LocalResult.Failure` is the last try's failure, unchanged; `NoHandOff` + `ErrNoMatchingMember` only when no try was made. The owner decides with `Kind.MayTryAnother(RepeatSafe)`, which gives the same control flow as the spec's sentence in every case.
3. **§4 "the same `RequestID` as CloudEvent `id`".** The CloudEvent *envelope* `Id` is a fresh `uuid.New()` per event (`internal/grpc/cloudevent.go:25`) and no test or help text promises otherwise; the request id is the payload's `id` and `requestId` (`dispatch.go:220-222`, R§2.4). Planned against the payload fields; the envelope `Id` stays unique per try (a try is a distinct event). If the envelope id is meant, `NewCloudEvent` needs an id parameter — say so before L-6.
4. **Sequencing across streams: passes name a callout before anything registers it.** After L-10 the three entry points mint passes with `Callout = RequestID, (1, 0)`, but `fence.Begin` is called only by the Coordinator. If the fence's `Admit` in `txjoin.JoinFromToken` lands while the entry points are still the `ExternalProcessingService`, every callback is refused (`CALLOUT_SUPERSEDED`) and the callback e2e tests go red. The enforcement must land with, or after, the Coordinator's take-over.
5. **§3's table has no row for a pass that cannot be minted.** Today `resolveTxToken` logs and sends the work **without** a pass (`dispatch.go:67-71`) — the cnode's callbacks would then run outside the transaction. Planned (L-10): `Terminal`, a ticketed 500, nothing sent. Unreachable in practice (`Signer.Issue` fails only on `json.Marshal`), hence a recorded TDD waiver rather than a test.
6. **`CalloutFailure.Err`** is a field more than §3 lists; without it "every other kind already carries an `*AppError`" (§8.2) cannot hold, and `errors.Is(err, contract.ErrNoMatchingMember)` — used by `classifyWorkflowError:2867`, the peer handler `handler.go:202` and `cluster_dispatcher.go:442` — would break. `contract` stays a leaf: the field is a plain `error`.
7. **Same request id + "a second visit may try a cnode again" (brief §4).** Correlation is per member and by request id, so a late answer to an abandoned first try can complete a *second* try of the same callout on the same cnode in a later pass. It is an answer to the same work from the same cnode, so this plan leaves it; it is worth a sentence in the owner's-loop plan.
8. **Log levels.** "dispatching …" and "dispatch completed" were INFO per callout; per §12 ("per-try detail at DEBUG") they become DEBUG in L-7/L-8, and "no matching calculation member" drops from WARN to DEBUG in `RunLocal` because under the owner's loop it would repeat on every pass of the patience. Until the Coordinator logs its one INFO line per callout, a single-try callout leaves no INFO trace.
9. **`newTestEnvWithDispatch` and the import bound.** L-5's G test imports `responseTimeoutMs: 5000` and runs it against a dispatcher whose bound is 1 s. Stream C's C-7 gives `workflow.New` a bound and sets it to `60*time.Second` in the two `internal/grpc` test environments, so the fixture still imports. Whoever later lowers that value below 5 s breaks this test at import, not at dispatch — the test's failure message (`workflow import …: expected 200`) says so.
