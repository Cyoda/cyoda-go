# Criterion stoppage reason in the entity state-machine audit

Issue: #413 (milestone v0.8.3). Follow-on: #414 (processor-criterion guarding, unmilestoned).

## Problem

`EntityCriteriaCalculationResponse.reason` — the field a gRPC criteria compute
node returns to explain *why* a criterion returned `matches=false` — is dead
data. It is declared on the CloudEvent payload schema
(`docs/cyoda/schema/processing/EntityCriteriaCalculationResponse.json`) and the
generated DTO (`api/grpc/events/types.go`, `Reason *string`), but:

- The server never deserializes it — `handleCriteriaResponse`
  (`internal/grpc/streaming.go`) unmarshals into a struct with no `reason` field.
- `ProcessingResponse` (`internal/grpc/members.go`) has no `Reason`.
- `DispatchCriteria` (`internal/contract/processing.go`) returns `(bool, error)`;
  the criteria result collapses to a bare bool before it reaches the engine.

Consequence: a client analysing a workflow can see *that* a passage was blocked
(a criterion returned false) but not *why*.

## Goal

Make the criterion stoppage reason a live, client-observable value **on the
entity state-machine audit trail**, so clients can analyse a workflow and learn
why a passage was blocked. Additionally, surface it synchronously in the
manual-transition rejection response.

No new endpoint. Two client surfaces:

1. **State-machine audit (durable record).** When a criterion returns
   `matches=false`, the existing state-machine audit event gains a `data`
   payload carrying the reason. Read via
   `GET /audit/entity/{entityId}?eventTypes=StateMachine`.
2. **Manual-transition 400 body (synchronous).** A manual transition rejected by
   its criterion returns `400 WORKFLOW_FAILED` whose detail now includes the
   reason, so interactive callers learn why without a second audit fetch.

## Scope — two implemented contact points

Both ride the shared `evaluateCriterion` chokepoint (`engine.go:623`), which has
exactly three callers (verified): `selectWorkflow` (`:386`), the manual
transition path (`:476`), and the automated cascade (`:553`).

| Contact point | No-match event | `data` payload (in the audit event) |
|---|---|---|
| Transition criterion — manual (`engine.go:483`) | `TRANSITION_NOT_MATCH_CRITERION` | `{workflowName, transition, criterion, reason}` |
| Transition criterion — automated cascade (`engine.go:560`) | `TRANSITION_NOT_MATCH_CRITERION` | `{workflowName, transition, criterion, reason}` |
| Workflow-selection criterion (`engine.go:399`) | `WORKFLOW_SKIP` | `{workflowName, reason}` |

All three sites currently call `recordEvent(..., nil)` — this change populates
the `data` argument.

### Data shapes — align to the existing typed DTOs

The OpenAPI already defines per-event typed shapes (Java-parity family, presently
unwired from the actual flat audit response — that reconciliation is #369, out of
scope here). We adopt their field **names** so a future reconciliation is
field-name-aligned. Note this is only a naming alignment: the typed DTOs put these
fields at the top level (`allOf StateMachineEventDto`), whereas the shipped audit
response nests them under `data`; #369 still owns the shape move, which is not
trivial.

- `StateMachineTransitionCriterionNotMatchedDto` = `{workflowName, transition, criterion, reason}`
  (`api/openapi.yaml:11019`; `reason` optional there).
- `StateMachineWorkflowSkippedDto` = `{workflowName, reason}`
  (`api/openapi.yaml:11084`; `reason` required there).

We do **not** use `RejectedTransitionCriterionDto` — that schema belongs to
`WorkflowStopReasonDto.rejectedTransitions`, a different feature, and its required
`transitionName` is wrong for `WORKFLOW_SKIP`.

The actual audit response emits a flat event map with a nested `data` bag
(`internal/domain/audit/handler.go:110-119`, `:259`), so the fields above go
**inside** `data`. `StateMachineEvent.Data` (`spi.types.go`,
`map[string]any`) → HTTP DTO `data` is already wired end-to-end, and round-trips
identically across memory (deep-copy), sqlite, and postgres
(`json.Marshal` of the whole event) — no audit-layer or backend change needed.

### Reason population rule (D3)

`data.reason` is **always present** for a clean `matches=false`:

- **FUNCTION (external) criterion:** the reason returned by the compute node.
- **Inline predicate (simple/group/lifecycle/array):** no external source →
  default `"criterion did not match"`.

Always-present `reason` satisfies `StateMachineWorkflowSkippedDto`'s required
`reason` and gives one consistent rule across both event types.

### Criterion name (`data.criterion`)

- **FUNCTION criterion:** the function name, parsed from the raw criterion JSON
  `{"function":{"name":"..."}}` (same parse as `internal/grpc/dispatch.go:200`).
- **Inline predicate:** the condition type string (`cond.Type()` —
  `"simple"`/`"group"`/`"lifecycle"`/`"array"`).

A small engine helper `criterionName(criterion []byte) string` derives this.
`WORKFLOW_SKIP` omits `criterion` (its DTO has no criterion field).

### Reason semantics (edge cases — verified)

- `matches=true` with a reason present: no no-match event is recorded, so the
  reason is naturally dropped. No code needed.
- Criterion **errored** (`err != nil`): short-circuits before any
  `TRANSITION_NOT_MATCH_CRITERION`/`WORKFLOW_SKIP` event. So `reason` strictly
  means "clean `matches=false`" — correct semantics.
- **Audit volume:** these no-match events already fire today (with `nil` data);
  this change only enriches them. Zero new audit volume. Cascade re-visits are
  already bounded by `maxStateVisits`.
- **Reason length (security):** the reason is compute-node-influenceable and the
  audit is durable storage. Truncate the reason to a bounded length
  (`maxCriterionReasonLen`, proposed 2 KiB) before it is stored or reflected into
  the 400 body. Never logged at any level beyond existing diagnostics.

## Plumbing — all internal, no `cyoda-go-spi` change

`ExternalProcessingService` is `internal/contract/processing.go:12` (verified: no
such type in the SPI). No coordinated SPI release, no schema-version bump.

1. **gRPC ingestion** — `internal/grpc/streaming.go handleCriteriaResponse`: add
   `Reason string` to the deserialized struct (currently dropped).
2. **`ProcessingResponse`** — `internal/grpc/members.go`: add `Reason string`.
3. **Interface** — `contract.ExternalProcessingService.DispatchCriteria`:
   `(bool, error)` → `(bool, string, error)` [D1]. The reason string is empty
   when unavailable.
4. **grpc dispatcher** — `internal/grpc/dispatch.go DispatchCriteria`: return
   `resp.Reason` on the `matches=false` path (and generally alongside the bool).
5. **Cluster cross-node** — `internal/cluster/dispatch/types.go`: add `Reason` to
   `DispatchCriteriaResponse`. Both `ClusterDispatcher.DispatchCriteria` return
   branches must carry the reason: the **local** branch
   (`cluster_dispatcher.go:137`, same-node evaluation) and the **peer-forward**
   branch (`:171`). **Producer wiring (M2, multi-node-primary):**
   `internal/cluster/dispatch/handler.go handleCriteria` (~`:96-109`) must copy
   the local `DispatchCriteria` reason into the outbound `DispatchCriteriaResponse`;
   without this, peer-evaluated criteria silently lose the reason in cluster mode.
6. **Engine** — `evaluateCriterion` (`engine.go:623`): `(bool, error)` →
   `(bool, string, error)`; propagate the reason (external reason for FUNCTION,
   `""` for inline). This is the single chokepoint where local, peer, audit, and
   400-body all converge, so **apply the length cap here, once**, on the returned
   reason. Populate `data` at the three no-match sites, applying the D3 default.
7. **Manual-transition 400** — the manual path returns
   `fmt.Errorf("transition %q criterion not matched", name)` (`engine.go:486`),
   reflected verbatim into the 400 body via `classifyWorkflowError`
   (`service.go:1950`, `ErrCodeWorkflowFailed`; verified: no sentinel-wrap hides
   it). Append the reason only when it is a **real (external) reason** —
   `"transition %q criterion not matched: %s"`; **suppress the append when the
   reason is the inline D3 default** (`"criterion did not match"`) to avoid the
   redundant `"...not matched: criterion did not match"`. The audit event still
   carries the default for inline (see D3). No new error code.

### Test doubles to update (compilation-enforced, listed to prevent silent revert)

`internal/cluster/dispatch/cluster_dispatcher_test.go:39`,
`internal/cluster/dispatch/handler_test.go:40`,
`internal/cluster/dispatch/integration_txtoken_test.go:41`,
`internal/observability/dispatch_tracing_test.go:32`,
`internal/domain/workflow/engine_test.go:871` and `:1600`. Plus the real
implementors: `internal/skeleton/processing.go`,
`internal/observability/dispatch_tracing.go`,
`internal/testing/localproc/localproc.go`. Individual *call-site* test files that
invoke `DispatchCriteria` (e.g. `internal/grpc/dispatch_test.go`,
`internal/cluster/dispatch/*_test.go`, `internal/testing/localproc/localproc_test.go`)
also need the extra return value — compilation-enforced, so this list need not be
exhaustive; `go build ./... && go vet ./...` surfaces the rest.

## Contract & documentation (Gate 4, Gate 7)

- **OpenAPI (m3):** update `StateMachineAuditEventDto.data` description
  (`api/openapi.yaml:10893`) to document that `TRANSITION_NOT_MATCH_CRITERION`
  carries `{workflowName, transition, criterion, reason}` and `WORKFLOW_SKIP`
  carries `{workflowName, reason}`. Also update the mirror
  `docs/cyoda/openapi.yml`. The manual-transition endpoint's 400 description
  already covers `WORKFLOW_FAILED`; note the reason is now appended to the detail.
  (The typed `StateMachine*Dto` family documents these same fields at top level;
  the two descriptions coexist until #369 reconciles the representations.)
- **Cloud-parity (Gate 7):** add `docs/cloud-parity/criterion-stoppage-reason.md`
  — cyoda-go leads; the audit `data.reason` shape and the 400-body enrichment are
  the contract Cloud mirrors.
- **Help topic:** `cmd/cyoda/help/content/grpc.md` criteria-response section —
  document that a compute node may return `reason` and where it surfaces
  (audit + manual-transition 400). Keep compact.
- **CHANGELOG.** No env-var, README, or COMPATIBILITY change (internal-only, no
  SPI pin bump).

## Error / status-code table

| Endpoint | Status / code | Scenario | Change |
|---|---|---|---|
| `POST /entity/{format}/{entityId}/{transition}` | `400 WORKFLOW_FAILED` | manual transition criterion returns `matches=false` | detail now appends `: <reason>` |
| `POST /entity/{format}/{entityId}/{transition}` | `400 BAD_REQUEST` | invalid payload/param | unchanged |
| `POST /entity/{format}/{entityId}/{transition}` | `400 TRANSITION_NOT_FOUND` | named transition absent | unchanged |
| `POST /entity/{format}/{entityId}/{transition}` | `2xx` | criterion matches | unchanged |
| `GET /audit/entity/{entityId}` | `200` | SM events for entity | `TRANSITION_NOT_MATCH_CRITERION` / `WORKFLOW_SKIP` items now carry `data.reason` |
| gRPC criteria response (compute-node → engine) | envelope | `reason` returned on `matches=false` | now reaches `ProcessingResponse.Reason` |

No new error code → no new `errors/<CODE>.md` topic.

## Coverage matrix (scenario × layer)

| Scenario | unit | running-backend e2e | cross-backend parity | gRPC | multi-node (isolated) |
|---|---|---|---|---|---|
| gRPC criteria response `reason` → `ProcessingResponse.Reason` | | | | ✅ `internal/grpc` | |
| External FUNCTION criterion rejects **manual** transition → 400 body carries reason **and** audit `data.reason` | | ✅ | | | |
| External FUNCTION criterion rejects **automated cascade** transition → audit `data.reason` (no error) | | ✅ | | | |
| Workflow-selection FUNCTION criterion rejects → `WORKFLOW_SKIP` audit `data.reason` | | ✅ | | | |
| Inline predicate rejects → default `"criterion did not match"` in `data.reason` | ✅ | ✅ | ✅ (register in `e2e/parity/registry.go`) | | |
| Audit `data.reason` is backend-agnostic (memory/sqlite/postgres) | | | ✅ | | |
| Peer-evaluated FUNCTION criterion returns reason to routing node via `DispatchCriteriaResponse` | | | | | ✅ `internal/cluster/dispatch` |
| Cluster **local-branch** (`cluster_dispatcher.go:137`) returns the reason (same-node eval in cluster mode) | | | | | ✅ `internal/cluster/dispatch` |
| Overlong reason truncated to cap before persist / 400 | ✅ | | | | |
| Criterion-name derivation (FUNCTION name vs inline type) | ✅ | | | | |

Concurrency: none required (no new shared-state contention). Multi-node reason
propagation is an isolated single-backend cluster test, not in the parity suite.

## Out of scope → #414

Processor-selection criteria (`PROCESS_NOT_MATCH_CRITERION`,
`StateMachineProcessCriterionNotMatchedDto` which already carries a `reason`
field) require first building "guard a processor by a criterion" — an SPI
`ProcessorConfig.Criterion` field (coordinated release + schema-version bump) plus
guard semantics. Tracked in #414; the shared `evaluateCriterion` reason-plumbing
from this change makes it a trivial drop-in.

Full OpenAPI reconciliation of the typed `StateMachine*Dto` family vs the flat
`StateMachineAuditEventDto`+`data` representation is #369, not this change.
