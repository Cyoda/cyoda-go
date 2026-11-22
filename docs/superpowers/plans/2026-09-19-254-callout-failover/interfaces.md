# Interfaces already fixed by finished plan sections

Finished sections live in the worktree under
`docs/superpowers/plans/2026-09-19-254-callout-failover/`:
`C-config-spi-import.md`, `H-harness.md`, `M-membership.md`,
`L-local-procedure.md`. Read the `## Stream interface summary` at the end of
each one you consume from — those names are binding; do not invent variants.
A fencing section (`F`) is being drafted in parallel; its API is fixed by spec
§7 and repeated below.

## From C (config / SPI / import)
- `cfg.Callout.FixedNumRetries int`; `cfg.Callout.ResponseTimeout`,
  `.ResponseTimeoutMax`, `.HandoverAllowance`, `.PassAllowance` (`time.Duration`)
  on `app.CalloutConfig`; `cfg.Cluster.DispatchConnectTimeout`,
  `cfg.Cluster.DispatchWaitTimeout` (≥ 0; 0 = do not wait),
  `cfg.Cluster.DispatchForwardTimeout` (scheduler RPC client only).
- `spi.ProcessorConfig.Idempotent bool`, `spi.ScheduleFunction.RetryPolicy string`.
- `contract.ParseCriterionFunction(json.RawMessage) (contract.CriterionFunction, error)`
  with `Config.RetryPolicy`, `Config.ResponseTimeoutMs`, `Config.CalculationNodesTags`.
- `workflow.RetryPolicyNone`, `workflow.RetryPolicyFixed`.
- C-11 (last) removes `CYODA_TX_TOKEN_TTL` once nothing reads `cfg.Cluster.TxTokenTTL`
  (`app/app.go:441, 554` today).

## From L (local procedure, `internal/contract` + `internal/grpc`)
- `contract.CalloutFailure{Kind, Code, Message, Retryable *bool, Attempts, Err error}`
  (`Error()`, `Unwrap()`), `contract.CalloutFailureKind` (`NoHandOff`, `NoAnswer`,
  `MemberFailed`, `Terminal`; `.MayTryAnother(repeatSafe bool) bool`),
  `contract.CalloutAttempt{MemberID, Kind, Cause}`, `contract.ErrCalloutDeadline`
  (the owner derives its deadline context with
  `context.WithDeadlineCause(ctx, deadline, contract.ErrCalloutDeadline)`).
- `grpc.MemberRegistry.Candidates(tenantID, tagsCSV) []*Member`,
  `grpc.MemberRegistry.Changed() <-chan struct{}` (take the channel BEFORE looking).
- `grpc.NewProcessorCallout(...) Callout`, `grpc.NewCriteriaCallout(...) (Callout, *contract.CalloutFailure)`,
  `grpc.NewFunctionCallout(...) Callout`; the caller fills `RequestID`,
  `AnswerLimit`, `RepeatSafe`, `OwnerNodeID`, `Number TryNumberer`, `Outer []token.Pair`.
- `grpc.TryNumberer{ Next() (major, minor uint32) }`, `grpc.NewMinorNumberer(major uint32)`.
- `(*grpc.ProcessorDispatcher).RunLocal(ctx, call Callout, maxTries int) LocalResult{Result, Failure, CtxErr, TriesUsed, Attempts}`.
- `(*grpc.ProcessorDispatcher).ResolveAnswerLimit(responseTimeoutMs int64) (time.Duration, *contract.CalloutFailure)`.
- L leaves thin `Dispatch*` wrappers, `runSingleTry`, `singleTryNumberer` and a
  `uuids` constructor parameter in `internal/grpc`; the OWNER'S-LOOP stream
  deletes them when the Coordinator takes over. L-11 (last) deletes `WithTxToken`
  / `TxTokenFromContext` and needs the HAND-OVER stream to have removed
  `DispatchCalloutRequest.TxToken` and every use outside `internal/grpc`.

## From F (fencing; per spec §7)
- `token.Pair{Callout string; Major, Minor uint32}`, `token.Claims{NodeID, TxRef,
  ExpiresAt, Callout, Major, Minor, Outer []token.Pair}`, `Signer.Issue(claims) (string, error)`.
- `fence.New(gate *txgate.Registry) *Fence`;
  `(*Fence).Begin(ctx, calloutID, txID string, outer []Pair) (context.Context, func())`;
  `(*Fence).Advance(calloutID string, major uint32)`; `(*Fence).Admit(ctx, pairs) (context.Context, error)`;
  `fence.Check(ctx) error`; `fence.Pairs(ctx) []Pair`; `fence.ErrSuperseded`.
  One `*fence.Fence` per process, built in `app/app.go` beside the `txgate.Registry`.
  (Whether `fence.Pair` and `token.Pair` are one type via an alias is F's call;
  write `token.Pair` where a pass is concerned and `fence.Pair` for the fence API.)

## From M (membership)
- `contract.NodeRegistry.Changed() <-chan struct{}`; `common.NewChangeSignal()`.
- `NodeInfo.Tags` is an empty non-nil map until a list arrives.

## From H (harness)
- e2e: `newCalloutHarness(t, configure func(*app.Config))`, `AttachCnode`, `Detach`,
  scripts `scriptAlways/scriptSequence/scriptLateCallback`, replies
  `answerOK/answerData/answerMatches/answerResult/answerFail/answerFailVerdict/neverAnswer/closeStream`,
  `ReceivedCallouts`, `AwaitCallouts`, `ReplayCreateHTTP/ReplayGetHTTP/ReplayCreateGRPC/ReplayGetGRPC`,
  `procWorkflowJSON`.
- parity: `parity.ComputeClientSpec{TenantID, Tags, Behaviour}`, `parity.StartComputeClientOrSkip`,
  `parity.AwaitReceived`, `parity.ComputeClientWorkflow`; multinode:
  `multinode.StartComputeClientOrSkip(t, fixture, node, spec)`. Every failover
  scenario uses a FRESH TENANT and a tag of its own.

## The seam between the HAND-OVER stream (P) and the OWNER'S-LOOP stream (O)
Both streams are drafted in parallel against exactly this. P implements it in
`internal/cluster/dispatch`; O consumes it from `internal/callout`.

```go
package dispatch // internal/cluster/dispatch

// HandOverAnswer is what the owner learns from one hand-over (spec §6, "How the
// owner reads an answer").
type HandOverAnswer struct {
    // Connected is false when the connection to the peer could not be opened
    // (a dial error, the connect timeout included) or the peer sent an
    // authenticated no_handoff: nothing was handed to a cnode and no try is used.
    Connected bool
    // Result is set when the outcome is ok.
    Result *internalgrpc.CalloutResult
    // Failure is nil when the outcome is ok. Its Kind is NoHandOff, NoAnswer,
    // MemberFailed or Terminal, read as spec §6 says. A lost answer is NoAnswer
    // with Code DISPATCH_FORWARD_FAILED and the sanitised message.
    Failure *contract.CalloutFailure
    // TriesUsed is 0 when Connected is false; otherwise 1..triesLeft (a lost or
    // out-of-range answer counts as exactly 1).
    TriesUsed int
    Attempts  []contract.CalloutAttempt
    Warnings  []string
}

// PeerRouter is the owner's view of the other pnodes.
type PeerRouter struct{ /* registry, selector, forwarder, selfNodeID */ }

// Peers returns the alive pnodes other than self that advertise any of tagsCSV
// for tenantID, in selector order.
func (r *PeerRouter) Peers(tenantID, tagsCSV string) []contract.NodeInfo // use the real node type name you find

// HandOver passes call to peer with triesLeft tries under fencing number major.
// wait bounds the owner's wait for the answer (triesLeft × answer limit +
// hand-over allowance, never past the callout's deadline — the caller computes it
// and puts it on ctx as a deadline).
func (r *PeerRouter) HandOver(ctx context.Context, peer contract.NodeInfo, call internalgrpc.Callout, triesLeft int, major uint32) HandOverAnswer

// Changed is the node registry's change signal (M).
func (r *PeerRouter) Changed() <-chan struct{}
```

In single-pnode mode the Coordinator's router is nil.
