# Workflow Configuration Import/Export Audit

**Date:** 2026-05-19
**Scope:** `POST /model/{entityName}/{modelVersion}/workflow/import` and
`GET /model/{entityName}/{modelVersion}/workflow/export`, reconciled against
the transactional workflow engine that consumes the stored configuration.
**Method:** Static read of the implementation, the OpenAPI contract, the SPI
types, and the existing tests. Every claim below has a file:line citation;
nothing is speculation.

Findings are graded **HIGH** (likely data loss, contract divergence, or
silent runtime breakage), **MEDIUM** (operator footgun, semantic ambiguity, or
test gap that masks a real risk), or **LOW** (cosmetic, benign round-trip
loss, or pure documentation gap).

Each finding is cross-referenced against the open issue tracker. "Tracked"
means an open issue already covers the finding (in whole or in part); the
remediation column in §7 calls out residual scope that the issue does not
cover. "Untracked" means no open issue addresses the finding as of the audit
date.

---

## 0. Issue cross-reference summary

| Audit finding | Status | Open issue(s) | Residual scope |
|---|---|---|---|
| H1 — scheduled-transition processors advertised but unimplemented | **Tracked** | #250 (schema/help cleanup), #251 (implementation), #252 (sibling: internalized processors) | None; the trio fully covers the surface. #250 is the prerequisite. |
| H2 — `Active=true` force-override on every import | **Tracked** | #256 (silent-default semantics review — H2/H5/M3 cluster) | Bundled with H5/M3 because the decisions interact. Design-first issue. |
| H3 — Read-Merge-Write race; `WorkflowStore` has no CAS | **Tracked** | #35 (first-committer-wins for non-entity stores; amended 2026-05-19 with HTTP-surface context + interim mutex suggestion); cassandra sibling cyoda-go-cassandra#22 | #35 lists WorkflowStore explicitly as affected. Decision still pending. |
| H4 — unknown `ExecutionMode` silently coerced to SYNC | **Resolved in v0.8.0** | #255 | Closed by #255 — validator now rejects unknown ExecutionMode values at import. |
| H5 — default-workflow fallback masks REPLACE-with-empty and criterion mismatches | **Tracked** | #256 | Bundled with H2/M3. |
| H6 — no state-graph validation at import (InitialState, Next, name uniqueness, criterion well-formedness) | **Partially resolved in v0.8.0** | #255 | H6.a–e closed by #255 (initialState, next, name uniqueness, transition uniqueness). H6.f (per-state unguarded-automated cap) and Criterion / Version well-formedness rows remain deferred. |
| M1 — boundary-accepted fields with no consumer | **Tracked (split)** | #250 (`ProcessorDefinition.Type` via schema reshape), #253 (`Context` pass-through), #254 (`RetryPolicy`), #257 (`Workflow.Description` cleanup as part of the boundary hygiene sweep) | `Workflow.Version` reserved as forward-looking per §L5 — out of scope of any cleanup. |
| M2 — export does not check model existence; 404 conflates two cases | **Tracked** | #257 (boundary hygiene sweep) | |
| M3 — empty `workflows` array silently destructive in REPLACE/ACTIVATE | **Tracked** | #256 | Bundled with H2/H5. |
| M4 — MERGE silently coalesces duplicate / empty workflow names | **Resolved in v0.8.0** | #255 | Closed by #255 — validator now rejects duplicate or empty workflow names within a request. |
| M5 — `importMode` default + case-insensitivity undocumented | **Tracked** | #257 | |
| M6 — JSON unmarshal silently drops unknown fields; `asyncResult` / `crossoverToAsyncMs` not implemented | **Tracked** | #145 (amended 2026-05-19 to include workflow handler scope), #223 (asyncResult + crossoverToAsyncMs Cloud-parity) | |
| M7 — no audit logging on successful import | **Tracked** | #257 | |
| M8 — workflow-level `Criterion` is honoured but under-documented | **Tracked** | #257 | |
| M9 — test coverage gaps (18 enumerated) | **Tracked** | #258 (workflow import/export test-coverage sweep) | Sequenced with #255 / #256 per #258's plan. |
| L1, L3, L4 (cosmetic / documentation) | **Untracked** | — | Each is small; no design dependency. |
| L2 (`States` map alphabetical order) | **Accepted as designed (2026-05-19)** | — | No change planned. |
| L5 (`Workflow.Version` reserved for schema-version evolution) | **Accepted as forward-looking (2026-05-19)** | — | No change planned. Reject-unknown-versions decision deferred until a second shape is planned. |
| ~~L6 (Unimplemented-stub path)~~ | **Withdrawn (2026-05-19)** | — | Not a real risk — production wiring at `app/app.go:439` is unconditional, no realistic regression path exists. |

**Overall (after issue creation and v0.8.0 scoping on 2026-05-19)**: all 15
actionable findings are tracked. **2 are accepted as designed** (L2, L5),
**1 is withdrawn** (L6 — the production wiring is mandatory; the finding
described a theoretical not real risk).

The **v0.8.0 milestone** captures every shape/SPI/handler change derivable
from this audit. The release intentionally lands schema and SPI changes
now (paid once with the v0.8.0 cyoda-go-spi bump) and defers feature
runtime to later releases. Issues in v0.8.0:

- **#250** — schema cleanup splitting processor execution-location from scheduled-transition timing (prerequisite for the shape carves).
- **#253** — wire `ProcessorConfig.Context` pass-through.
- **#255** — tighten import-time validation (state graph, names, ExecutionMode enum, MERGE-duplicates).
- **#256** — silent-default semantics with chosen defaults (H2 honour explicit `false`; H5 elevate fallback to `slog.Warn`; M3 reject empty `workflows`).
- **#257** — import/export boundary hygiene sweep.
- **#258** — test-coverage sweep (18 negative-space cases).
- **#259** — scheduled state transition shape + SPI (carve of #251).
- **#260** — internalized processor execution-location shape + SPI (carve of #252).
- **#261** — `asyncResult` / `crossoverToAsyncMs` SPI shape + import WARN (carve of #223).
- **#262** — `RetryPolicy` import validation + surface inbound `retryable` flag (carve of #254).
- **#263** — per-`(tenant, modelRef)` mutex on workflow import handler (carve of #35).
- **#264** — adopt `DisallowUnknownFields` on workflow import handler (carve of #145).

Deferred parent issues (track post-v0.8.0 work):

- **#251** — scheduled state transition runtime (durability, cluster ownership).
- **#252** — internalized processor execution runtime (registration, isolation).
- **#223** — full crossover semantics (gated on Cassandra plugin work-recovery primitives).
- **#254** — full member-failover retry loop (gated on #223 async semantics).
- **#35** — full `WorkflowStore.CompareAndSave` SPI addition (gated on cyoda-go-cassandra#22 coordination).
- **#145** — `DisallowUnknownFields` across remaining input boundaries.

---

## 1. Surface layout

| Component | File | Purpose |
|---|---|---|
| HTTP handler — import | `internal/domain/workflow/handler.go` lines 32–119 | Body parse → mode normalise → model-existence check → load existing → merge → validate → save |
| HTTP handler — export | `internal/domain/workflow/handler.go` lines 121–152 | Load workflows → 404 if empty → return as raw JSON envelope |
| Mode dispatch | `internal/domain/workflow/import.go` | `applyImportMode` for MERGE / REPLACE / ACTIVATE |
| Static validation | `internal/domain/workflow/validate.go` | Two checks only: unguarded-cycle detection and `StartNewTxOnDispatch`/`COMMIT_BEFORE_DISPATCH` coherence |
| Persistence SPI | `cyoda-go-spi@v0.7.1/persistence.go` lines 114–118 | `WorkflowStore { Save, Get, Delete }` — **no CAS, no version, no transactional guard** |
| OpenAPI contract | `api/openapi.yaml` lines 8606–8977 | `WorkflowConfigurationDto`, `WorkflowImportRequestDto`, `WorkflowExportResponseDto`, processor config DTOs |
| Engine consumer | `internal/domain/workflow/engine.go`, `engine_processors.go`, `transitions.go`, `transition_aborted.go` | Reads stored workflows on every Execute / ManualTransition / Loopback call |

The handler is wired in `internal/api/server.go:244–258`; without a configured
`Workflow` handler the surface falls through to
`internal/api/unimplemented.go`.

---

## 2. Findings — high severity

### H1. Scheduled-transition processors are advertised but unimplemented

**Issues:** #250 (schema/help cleanup — prerequisite), #251 (implement
scheduled state transitions), #252 (internalized processors — sibling that
shares the `type` discriminator question).

**Where.** OpenAPI defines two processor variants via
`ProcessorDefinitionDto.type` discriminator
(`api/openapi.yaml:8672–8712`):
`ExternalizedProcessorDefinitionDto` and `ScheduledTransitionProcessorDefinitionDto`,
the latter carrying a config of `{delayMs, transition, timeoutMs}`.

The SPI flattens both into a single `ProcessorDefinition` whose `Config` is the
externalized shape only (`cyoda-go-spi@v0.7.1/types.go:140–161`). There is **no
`delayMs`, no `transition`, no `timeoutMs`** in `ProcessorConfig`. An engine-side
grep for `scheduled`, `delayMs`, `Schedule`, `timer`, `cron` returns nothing under
`internal/domain/workflow/`.

**Effect.** A client may submit a scheduled-transition processor and receive
HTTP 200; the scheduling config fields are silently dropped by JSON unmarshal
into `[]spi.WorkflowDefinition` at `handler.go:43`; the engine then dispatches
the processor as if it were externalised (default case
`engine_processors.go:84` → SYNC). The `type` discriminator itself is also
unused — grep for `proc.Type` / `processor.Type` returns zero hits in
`internal/`.

The project memory already records that `ProcessorDefinition.type` is
"currently dormant; planned values are externalized/internalized;
scheduled-transition-firing is a separate workflow-timing primitive, not a
processor variant" — and #250 captures exactly this reshaping of the OpenAPI
contract. #251 then implements the scheduling behaviour as its own primitive,
and #252 adds the internalized execution location. The audit confirms the
triage in #250: schema cleanup must land before any implementation can take a
durable shape.

**Severity:** HIGH. Contract↔implementation divergence on a feature that, at
the surface, looks supported.

---

### H2. `Active=true` is silently forced on every imported workflow

**Issues:** None. **Untracked.**

**Where.** `handler.go:101–103`:

```go
for i := range req.Workflows {
    req.Workflows[i].Active = true
}
```

The OpenAPI schema exposes `active` as a writable field on
`WorkflowConfigurationDto` (`api/openapi.yaml:8774–8776`). The handler ignores
the client's value and rewrites it unconditionally. The behaviour is recorded
only in an in-code comment ("Cyoda Cloud behavior"). It is not documented in
the OpenAPI description, the README, the help topics, or `CLAUDE.md`.

The override has two compounding consequences:

1. **Round-trip is non-idempotent for deactivated workflows.** ACTIVATE mode
   deactivates existing workflows not present in the incoming set
   (`import.go:23–26` — verified by `TestImportActivate`,
   `handler_test.go:254`). The export endpoint faithfully emits
   `"active": false` (the SPI field has no `omitempty`,
   `cyoda-go-spi@v0.7.1/types.go:119`). Re-importing that exported JSON in
   REPLACE mode silently re-activates everything. The export is therefore not
   a usable backup format for a model whose state includes deactivated
   workflows.
2. **Operators cannot stage an inactive workflow.** The only way to land an
   inactive workflow in the store is to import-then-displace via ACTIVATE.
   There is no single-call way to ship "future workflow, deactivated for
   now" — the field that would express it is overwritten.

**Severity:** HIGH. Silent contract divergence + lost backup fidelity.

---

### H3. Read-Merge-Write is not atomic; concurrent imports race

**Issues:** #35 (extend first-committer-wins to non-entity stores incl.
`WorkflowStore`, postgres side); sibling cyoda-go-cassandra#22 on the
cassandra side. #35 names `WorkflowStore` as affected with the same shape as
`ModelStore`.

**Where.** `handler.go:92–116` reads existing workflows
(`wfStore.Get`), computes the merge in process memory (`applyImportMode`),
validates, then writes (`wfStore.Save`). The `WorkflowStore` SPI
(`cyoda-go-spi@v0.7.1/persistence.go:114–118`) is:

```go
type WorkflowStore interface {
    Save(ctx context.Context, modelRef ModelRef, workflows []WorkflowDefinition) error
    Get(ctx context.Context, modelRef ModelRef) ([]WorkflowDefinition, error)
    Delete(ctx context.Context, modelRef ModelRef) error
}
```

There is **no CAS, no version token, and no `SaveIf` variant**. Entity
persistence has a CAS (`EntityStore.CompareAndSave` at
`persistence.go:24–27`); workflow persistence does not. Two concurrent imports
on the same `ModelRef` race; the later `Save` wins and any workflows added by
the earlier `Save` between the loser's `Get` and its own `Save` are silently
discarded. The handler holds no mutex, the persistence layer holds none either
(verified for the memory plugin's `copyWorkflows` pattern, and confirmed by
the absence of any lock acquisition on the import path).

The TOCTOU also reaches the model-existence guard at `handler.go:65–83`: a
model deleted between the existence check and `Save` produces a workflow row
attached to a model that no longer exists. Whether that is reachable depends
on the plugin's `ModelStore.Delete` cascading semantics, which are out of
scope here.

**Severity:** HIGH. Silent data loss under concurrent operator imports.

**Note on #35:** the issue lists four candidate directions (per-row
`xmin`/version CAS, postgres advisory locks, extending the entity read-set
mechanism, or accept-as-documented). Any of the first three closes the
finding; "accept-as-documented" leaves it open. The decision is gated on
cyoda-go-cassandra#22 so both plugins stay behaviourally equivalent.

---

### H4. Unknown `ExecutionMode` is silently coerced to SYNC

**Issues:** [#255](https://github.com/Cyoda-platform/cyoda-go/issues/255). **Resolved in v0.8.0** — `validateWorkflows` now rejects any `ExecutionMode` outside `{SYNC, ASYNC_SAME_TX, ASYNC_NEW_TX, COMMIT_BEFORE_DISPATCH, ""}` with `400 VALIDATION_FAILED`. Historical analysis below retained for context.

**Where.** `engine_processors.go:63–87` is a `switch proc.ExecutionMode`
covering `ASYNC_NEW_TX` and `COMMIT_BEFORE_DISPATCH` explicitly; the `default`
case at line 84 dispatches via `executeSyncProcessor`. The default therefore
catches both the explicitly-named modes that aren't in the switch (`SYNC`,
`ASYNC_SAME_TX`) **and** any unknown value, including typos and the empty
string (the SPI tag is `json:"executionMode,omitempty"`).

`validateProcessorFlags` (`validate.go:47–61`) only constrains the
combination of `StartNewTxOnDispatch=true` with non-COMMIT_BEFORE_DISPATCH.
There is no enum check against the four documented values
(`api/openapi.yaml:8646–8650`).

**Effect.** A client posting `"executionMode": "ASYN_SAME_TX"` (typo) gets
HTTP 200, the processor runs as SYNC, and the only signal is the absence of
new-transaction or commit-before-dispatch behaviour. There is also no warning
distinguishing the intentional empty/default case from the typo case.

**Severity:** HIGH. Silent demotion of intended async behaviour.

---

### H5. Engine falls back to the embedded default workflow with only a warning

**Issues:** None. **Untracked.**

**Where.** `engine.go:127–133`, `:229–232`, `:311–314` — when the
`WorkflowStore.Get` returns no rows, the engine substitutes
`e.defaultWorkflows` (loaded from the embedded `default_workflow.json`). At
`engine.go:386–391`, when none of the imported workflows match the entity's
criterion, the engine again falls back to the default and emits
`common.AddWarning(ctx, "no imported workflow matched — using default workflow")`.

This is an intentional design choice (see `defaultwf_fallback_test.go` and
`active_default_test.go`) but it has two import/export consequences:

1. **REPLACE with `workflows: []` does not "clear" workflows** in the
   user-visible sense. It clears the store, the engine then resurrects the
   embedded default on the next call, and entities continue to transition
   under it. Operators may believe REPLACE-with-empty halted the workflow;
   it did not.
2. **Workflow-criterion mismatches are warning-only, not 4xx.** Operators
   importing a misconfigured criterion get HTTP 200; running entities then
   silently flip onto the embedded default. The warning is in the response
   body, not the headers, and is easy to miss in scripts.

This compounds with H4 (unknown ExecutionMode → SYNC) and H6 (no state-graph
validation): the system is heavily biased toward "succeed silently with a
substitute" over "fail loudly".

**Severity:** HIGH (because it interacts with H1/H2/H4 to mask real
misconfigurations).

---

### H6. No state-graph validation at import time

**Issues:** [#255](https://github.com/Cyoda-platform/cyoda-go/issues/255). **Partially resolved in v0.8.0** (scoped subset H6.a–e). A new `validateImportRequest` rejects: empty workflow name (H6.c), duplicate workflow names within a request (H6.d), empty or undeclared `initialState` (H6.a), transition `next` not in `states` (H6.b), duplicate transition names within a state (H6.e), and unknown `executionMode` values (H4). Each error names the offending workflow / state / transition.

**Policy: import validator is request-time, not retroactive.** The new H4/H6 rules run only on the incoming request slice — legacy stored shapes that slipped past weaker pre-v0.8.0 validation are not re-rejected on every subsequent import. The pre-existing behavioural checks (`validateWorkflowLoops`, `validateProcessorFlags`) continue to run on the merged result, so a stored cycle or incoherent `startNewTxOnDispatch` still surfaces — that pre-v0.8.0 contract is preserved. Future rule additions should declare which side of this split they belong on.

H6.f (per-state at-most-one unguarded automated transition) and the Criterion / Version validity rows in the table below remain deferred. Historical analysis below retained for context.

**Where.** `validate.go:28–38`. The only static checks are
`validateWorkflowLoops` (cycle detection) and `validateProcessorFlags`.

Not validated:

| Invariant | Engine behaviour on violation |
|---|---|
| `WorkflowDefinition.InitialState != ""` | `engine.go:139` writes `entity.Meta.State = ""`, next `attemptTransition` looks up state `""`, returns "transition not found"; the entity is parked in `""` indefinitely. |
| `WorkflowDefinition.InitialState ∈ States` | Same as above; not panic, but the entity is stuck. |
| `TransitionDefinition.Next ∈ States` | `cascadeAutomated:512–514` moves the entity to the typo'd state, then on re-entry finds no `stateDef` for that key and returns silently as "stable". The entity is parked in a state that has no `States` entry — invisible to all future cascades. |
| Workflow `Name` non-empty / unique within request | `applyImportMode` MERGE keys by name (`import.go:33–44`) — duplicates clobber silently, empties collide. |
| Transition `Name` unique within a state | Engine searches first match (`engine.go:428` via `attemptTransition`); duplicates are silently shadowed. |
| `Criterion` is well-formed JSON | `engine.go:370, 525, 596` evaluate via `predicate.ParseCondition`; failures surface as 5xx on the first matching entity, not on import. |
| `ExecutionMode` ∈ documented enum | See H4. |
| Workflow `Version` string content | Field is required by OpenAPI (`api/openapi.yaml:8788`) but never inspected by the engine; arbitrary strings pass. |

`validateWorkflowLoops` itself iterates `wf.States` in Go map order
(`validate.go:110`), so when multiple cycles exist the reported cycle is
non-deterministic and the unit tests can be flaky on that error message.

**Severity:** HIGH (in aggregate). Each individual gap is medium, but the
combined effect is that almost any structurally broken workflow passes
import.

---

## 3. Findings — medium severity

### M1. Three ProcessorConfig fields are accepted at the boundary but never consumed

**Issues:** **Partially tracked.** #250 reshapes
`ProcessorDefinition.Type` out of its current discriminator role; once #250
lands the `type` finding here is mooted. `ProcessorConfig.RetryPolicy`,
`ProcessorConfig.Context`, `WorkflowDefinition.Version`, and
`WorkflowDefinition.Description` are **not** tracked.

Verified via repository-wide grep of non-test code:

| Field | OpenAPI / SPI | Consumed? |
|---|---|---|
| `ProcessorConfig.RetryPolicy` | `api/openapi.yaml:8619–8621`, `cyoda-go-spi@v0.7.1/types.go:152` | **No.** Only references are the generated DTO and the SPI struct. |
| `ProcessorConfig.Context` | `api/openapi.yaml:8622–8624`, `types.go:153` | **Resolved in v0.8.0.** Wired as a pass-through string into the dispatch `parameters` JSON node at `internal/grpc/dispatch.go:71, 221`. Historical analysis below retained for context. |
| `ProcessorDefinition.Type` | `api/openapi.yaml:8674–8679`, `types.go:141` | **No.** Discriminator carried for parity, no engine branch uses it. |

The other ProcessorConfig fields **are** consumed by the gRPC dispatcher
(`internal/grpc/dispatch.go`), specifically:
`CalculationNodesTags` at lines 47/187, `AttachEntity` at line 68/183,
`ResponseTimeoutMs` at line 104 (with a 30-second default at line 24,
`defaultResponseTimeoutMs`). The engine itself passes the whole
`ProcessorDefinition` through to `extProc.DispatchProcessor` unchanged
(`engine_processors.go:143, 159, 174, 235, 268`), so the consumption happens
on the dispatcher side, not at the engine layer.

**Special case — `Context` is not dead by design; it is unwired.** Per Cyoda
Cloud semantics (confirmed by the user, 2026-05-19), `ProcessorConfig.Context`
is a **pass-through string**: stored verbatim in the workflow config and
carried verbatim into the `parameters` JSON node of every
`EntityProcessorCalculationRequest` (and `EntityCriteriaCalculationRequest`)
sent to the external member. Its purpose is to let one external processor
implementation serve multiple logical roles depending on what the workflow
says to do, without needing a separate processor registration per role.

The receiving side is already wired:
`api/grpc/events/types.go:2403` defines
`EntityProcessorCalculationRequestJson.Parameters interface{}` (and line 822
defines the same on `EntityCriteriaCalculationRequestJson`). The dispatcher
just never sets it. At `internal/grpc/dispatch.go:57–67` the request is
constructed without touching `req.Parameters`; the criteria-evaluation path
at lines 197–203 is the same shape. `processor.Config.Context` is read
nowhere in the repository.

That makes `Context` an **implementation gap, not a contract artefact**: the
field is genuinely meant to do work, the OpenAPI documents it correctly, the
wire format reserves a slot for it, and the dispatcher is one assignment
away from honouring it. Removing the field is the wrong remediation; wiring
it is. This deserves its own issue (see §9).

**Second special case — `RetryPolicy` is also not dead by design.** Per
Cloud semantics (confirmed by the user, 2026-05-19) `RetryPolicy` is the
workflow-author's selector across two server-resolved retry strategies:

- **`NONE`** — execute once, propagate failure immediately.
- **`FIXED`** (the default when unset) — up to `numRetries` additional
  attempts with a fixed `delayMs` between them. The attempt count and delay
  are **not** carried in the workflow config; they come from server-side
  configuration (in Cloud: `net.cyoda.saas.retry.policy.fixed.numRetries`
  defaulting to 3, `…delayMs` defaulting to 500). The workflow selects the
  *policy type*, the server supplies the *parameters*.

The retry loop in Cloud wraps the entire send-and-wait-for-response attempt
to one calculation member, and has three behaviours worth mirroring:

1. **Member failover, not same-member retry.** Each attempt accumulates the
   tried member into a `usedMembers` set; the next attempt explicitly
   excludes them via `findCalculationMember`. The semantics are "try a
   different node", not "ask the same node again".
2. **Member-vetoed non-retry.** The receiving member may set
   `retryable=false` on its error response; the retry loop honours that
   flag and stops immediately regardless of policy. Business-logic
   failures the member knows are deterministic do not waste further
   attempts.
3. **Async mode suppresses retry.** When the processor is dispatched under
   `asyncResult=true` (see M6 / #223), the retry config is constructed
   with `noRetryMode=true` and the policy is ignored. `NONE` and `FIXED`
   both collapse to one attempt in async mode.

On exhaustion Cloud emits a single aggregated `ExternalizationException`
that lists each tried member with the per-member exception, deduplicated by
count — operators see one error per dispatch with the full failover trail
attached.

**Current state of cyoda-go.** The dispatcher at
`internal/grpc/dispatch.go:46–135` is a single-shot send-and-wait: one
`FindByTags` lookup, one `member.Send`, one select on response / timeout /
ctx.Done — and any failure propagates immediately. There is no retry, no
member-exclusion set, no aggregated-failure mode, and no honouring of the
member's `retryable` flag. The wire types **do** already carry the flag:
`api/grpc/events/types.go:36, 111, 189, 261, 473, 551, 641, 754, 862, 992`
each declare `Retryable *bool` on the inbound error shape. But the
flag is not surfaced into `ProcessingResponse`
(`internal/grpc/members.go:24` — the struct exposes `Success`, `Error`,
`Matches`, `Warnings` only), so the dispatcher could not consult it today
even if it tried.

`processor.Config.RetryPolicy` (`string` at SPI level) is consequently dead
in cyoda-go, but the deadness is symmetric to `Context`: the field is
*meant* to do work, the surrounding scaffolding exists, the wire format
already carries the per-response veto signal, and the implementation is
absent. This is a Cloud-parity gap, not a spec/docs cleanup. It deserves
its own issue with the scope spelled out (see §9), and because
`asyncResult=true` is a documented suppressor, the issue depends on #223 to
settle async semantics first.

Of the remaining members of this finding, only `WorkflowDefinition.Version`
and `WorkflowDefinition.Description` are genuinely dormant with no known
Cloud role and remain candidates for "wire or drop" discussion.
`ProcessorDefinition.Type` is dormant by design and is being reshaped by
#250.

`ProcessorConfig.AttachEntity` deserves a separate note: the SPI type is plain
`bool` (`types.go:149`), but the gRPC dispatcher's internal contract
config (`internal/grpc/dispatch.go:172`) treats `*bool` with `nil → true`.
The two shapes are not the same — a workflow imported via OpenAPI that omits
`attachEntity` ships `false` to the dispatcher, while a contract-discovered
processor that omits it ships the discovery default (typically true). That is
a separate consistency concern.

**Severity:** MEDIUM. Three dead config fields are accepted at the boundary
without warning.

### M2. Export does not check model existence; 404 conflates two failure modes

**Issues:** None. **Untracked.**


The import handler explicitly verifies the model exists before applying the
workflow (`handler.go:65–83`, error code `MODEL_NOT_FOUND`, with `entityName`
and `entityVersion` in `Props`). The export handler does not. It runs
`wfStore.Get` directly and returns
`404 WORKFLOW_NOT_FOUND: "no workflows found for model %s/%d"` whenever the
result is empty (`handler.go:139–143`) — regardless of whether the model
exists at all.

This is the documented "issue #131" rationale on the import side; the export
side was apparently never updated to match. A client cannot distinguish "model
exists but has no workflows" from "model never existed". The test
`TestExportEmpty_Returns404` (`handler_test.go:311`) only covers the
existing-model case; no test exercises the missing-model case.

**Severity:** MEDIUM. Error-classification regression vs. import parity.

### M3. REPLACE / ACTIVATE with empty `workflows` is silently destructive

**Issues:** None. **Untracked.**


`import.go:11` returns `incoming` unchanged for REPLACE. `import.go:13–29`
in ACTIVATE flips every existing workflow to `Active=false` when no matching
incoming workflow is supplied. Both branches accept `workflows: []` without
warning. Combined with H5 (default-workflow fallback), the operator gets
HTTP 200 and entities continue to move, but on the embedded default — the
intended workflows are gone.

There is no test for either case (`TestImportReplace`, `TestImportActivate`
both supply non-empty incoming sets).

**Severity:** MEDIUM. Footgun + missing coverage.

### M4. `applyImportMode` MERGE silently coalesces duplicate names

**Issues:** [#255](https://github.com/Cyoda-platform/cyoda-go/issues/255). **Resolved in v0.8.0** — `validateWorkflows` now rejects duplicate or empty workflow names within a single import request before `applyImportMode` runs, so the silent-clobber path is no longer reachable. Historical analysis below retained for context.


`import.go:33–44` keys by `wf.Name`. The same name appearing twice within
the incoming request results in the second clobbering the first, with no
audit, log, or warning. The same applies to empty names (every empty-name
workflow collapses to the same map entry). `validateWorkflows` does not
check uniqueness or non-emptiness. The default fallback at H5 then masks the
loss.

**Severity:** MEDIUM.

### M5. `importMode` defaulting and case-handling are silent

**Issues:** None. **Untracked.**


`handler.go:48–55` upper-cases the incoming mode and substitutes `MERGE`
when empty. The OpenAPI schema (`api/openapi.yaml:8798–8804`) lists the enum
but **does not declare a default**, nor does it state the field is
case-insensitive. The behaviour is reasonable but undocumented; a client that
sends `"importMode": "merge"` will succeed against cyoda-go and may fail
against a stricter implementation.

`TestImportDefaultMode` (`handler_test.go`) covers the empty case but not the
lower-case case; there is no test for `Mode unknown → 400` either, even though
the rejection path exists at `handler.go:52–54`.

**Severity:** MEDIUM.

### M6. JSON unmarshal silently drops unknown fields

**Issues:** **Partially tracked.** #145 (adopt `DisallowUnknownFields` at
JSON input boundaries) names `internal/domain/entity`,
`internal/cluster/dispatch`, and `internal/auth/validator.go` as the in-scope
boundaries — but **the workflow import handler at
`internal/domain/workflow/handler.go:43` is not in #145's named scope** and
should be added before #145 closes, otherwise this finding will survive the
issue. Separately, #223 covers the missing `asyncResult` /
`crossoverToAsyncMs` semantics with a planned WARN-on-import path — but the
workflow handler must actually inspect these fields for #223's WARN to fire,
which it cannot do today because the unmarshal silently drops them.

`handler.go:43` uses `json.Unmarshal(data, &req)` without
`decoder.DisallowUnknownFields()`. Forward-incompatible fields, typos in
processor config (e.g. `"responseTimeoutsMs"`), and the
`asyncResult` / `crossoverToAsyncMs` fields that **are** in the OpenAPI
`ExternalizedProcessorConfigDto` (`api/openapi.yaml:8625–8638`) but **not** in
the SPI `ProcessorConfig` all vanish on import. The client gets 200 and the
configuration is lost.

The `asyncResult` / `crossoverToAsyncMs` pair is the same shape as H1: the
OpenAPI advertises behaviour the SPI does not carry. The OpenAPI description
even calls them out as "storage-engine-plugin dependent", but in cyoda-go's
SPI the fields simply do not exist.

**Severity:** MEDIUM.

### M7. No audit logging on a successful import

**Issues:** None. **Untracked.**


Workflow configuration is a high-impact, multi-tenant, mutable surface; the
import handler emits no `slog.Info` on the success path (`handler.go:118`).
There is no record of who imported what when, no record of which mode was
applied, and no record of how many workflows changed. The user can still
correlate via HTTP access logs, but the application layer is silent on the
content of the change.

Other domain handlers in the repo (entity persistence, schema) do emit slog
events for state-changing operations; the workflow handler stands out.

**Severity:** MEDIUM.

### M8. Workflow-level `Criterion` is honoured by the engine but undocumented in practice

**Issues:** None. **Untracked.**


`WorkflowDefinition.Criterion` is consulted at `engine.go:357–384` to pick
which of several workflows applies to an entity. The OpenAPI schema does
include `criterion` on `WorkflowConfigurationDto` (`api/openapi.yaml:8777–8783`),
and the engine genuinely uses it — but the validation surface, the help-topic
documentation, and the existing tests are all dominated by transition-level
criteria. The only end-to-end coverage I can find for workflow-level
selection is `TestSelectWorkflow` in `engine_test.go`. The "first active
workflow whose criterion matches, else default" rule (engine.go:357–392) is
nowhere written down outside the code, and the criterion is also
re-evaluated on every Execute call (no caching), but at least it is honoured.

**Severity:** MEDIUM (documentation / discoverability).

### M9. Test coverage gaps (top items)

**Issues:** **Partially tracked.** #25 (E2E goroutine safety + general
coverage gaps) and #122 (Tranche 5 remote-mode smoke against cyoda-cloud)
cover adjacent ground but **do not enumerate any of the 18 workflow
import/export cases below**. The specific gaps remain untracked.


From the per-scenario matrix gathered against `handler_test.go`,
`scenarios_test.go`, `active_default_test.go`,
`defaultwf_fallback_test.go`, `engine_test.go`,
`e2e/parity/externalapi/workflow_import_export.go`,
`e2e/parity/externalapi/workflow_externalization.go`,
`e2e/parity/externalapi/negative_validation.go`:

Confirmed **uncovered**:

1. MERGE / REPLACE / ACTIVATE with empty `workflows` array.
2. MERGE with duplicate names within incoming.
3. MERGE with empty workflow name.
4. Unknown `importMode` value returning 400 (path exists, no test).
5. Lower-case `importMode` (`"merge"` etc.) — only the upper-case path is
   exercised.
6. Malformed JSON body returning 400 (path exists, no test).
7. Empty request body returning 400.
8. Oversized body (>10 MiB) — `http.MaxBytesReader` is wired at
   `handler.go:34` but no test verifies the rejection.
9. Active=false in request being overridden to true — verified only
   indirectly by `TestWorkflowWithoutActiveField_DefaultsToActive`, which
   exercises the absence-of-field case, not the explicit `false` case.
10. Workflow with empty `InitialState` or `InitialState ∉ States` —
    not rejected at import; runtime behaviour untested.
11. Transition with `next` pointing to undefined state — not rejected at
    import; cascade silently parks the entity (engine.go:512), untested.
12. Processor with empty / unknown `executionMode`.
13. Round-trip: export → re-import (REPLACE) → re-export equivalence.
14. Export against a non-existent model — distinguished from
    `WORKFLOW_NOT_FOUND`?
15. Export including a deactivated workflow — round-trip behaviour.
16. Concurrent imports on the same `ModelRef`.
17. Scheduled-transition processor — exportable / round-trippable at all?
18. Workflows with `Multiple cycles` (validator reports first cycle only and
    in non-deterministic order).

`TestImportFullWorkflow` and the e2e parity tests do exercise the happy path
deeply, but the negative space above is sparsely covered.

**Severity:** MEDIUM in aggregate (each individual gap is M-L; together they
mask several of the H findings above).

---

## 4. Findings — low severity

### L1. Bool fields with mismatched `omitempty` semantics

- `WorkflowDefinition.Active` is **not** `omitempty`
  (`cyoda-go-spi@v0.7.1/types.go:119`), so even `false` is always emitted.
  Together with H2 this makes export-then-import flip every deactivated
  workflow back to active.
- `TransitionDefinition.Manual` is **not** `omitempty` (`types.go:133`),
  so the round-trip is stable.
- `TransitionDefinition.Disabled` **is** `omitempty` (`types.go:134`),
  so `false` round-trips as missing — benign, both decode to `false`.
- `ProcessorConfig.StartNewTxOnDispatch` is `*bool` with `omitempty`
  (`types.go:160`), so `nil` and explicit `false` both serialize as missing
  and re-import as `nil`. Benign semantically, but a nil-vs-false
  distinction at the wire is lost.

The asymmetry is itself the finding: there is no documented rule, and the
fields with `omitempty` change meaning on round-trip while the ones without
do not.

### L2. `States` map iteration order is alphabetical on export — **by design**

Go's `encoding/json` sorts map keys alphabetically on marshal. The SPI
`WorkflowDefinition.States` is `map[string]StateDefinition` (`types.go:121`),
so an export is alphabetically ordered by state name regardless of the order
the operator imported. This is consistent with the OpenAPI
`additionalProperties` contract — states are an unordered set, not a
sequence.

**Resolution: accepted as designed (2026-05-19).** The alphabetical-key
ordering is the natural representation; no change planned. Operators
diffing exports against git get byte-stable output once the first export
is captured. Revisit if anyone actually reports the first-export-after-import
mismatch as a workflow problem.

### L3. `Criterion` (`json.RawMessage`) may be normalised by the plugin store

`WorkflowDefinition.Criterion` is `json.RawMessage` at the SPI layer, but the
postgres / sqlite / memory stores all round-trip workflows via
`json.Marshal` + `json.Unmarshal` (verified in the memory plugin's
`copyWorkflows`). That re-marshal normalises key ordering inside the
criterion JSON. Round-trip is semantically lossless but not byte-identical;
no test verifies byte-identity, but `e2e/parity/externalapi/workflow_import_export.go:84,198`
verifies structural equality.

### L4. Export response is unbounded

`handler.go:122–152` does not paginate or size-cap the response. A model with
thousands of workflows dumps every byte in one HTTP response. There is no
documented limit. In practice the number of workflows per model is small,
but no guard exists for the pathological case.

### L5. `Workflow.Version` is forward-looking, not inert

`api/openapi.yaml:8788` makes `version` required on every imported
workflow. The engine never inspects it today; my initial reading called it
"functionally inert", but per project intent (2026-05-19) the field is the
schema-version selector for future evolution of the workflow configuration
shape. There is only one shape today, so there is nothing to discriminate
on; the field is reserved so that import code can branch on version when a
second shape lands, without a breaking change to the request body.

**Resolution: accepted as forward-looking design.** The "wire or drop"
framing in the M1 sweep no longer applies to `Workflow.Version`. The
remaining open question is whether the import handler should reject
*unknown* version strings now (defensive), or remain tolerant until a
second shape exists. Either is defensible; defer the decision until the
first shape change is planned.

(Note: `Workflow.Description` is *not* covered by this reframing — it is
a free-text human-readable field, not a version discriminator, and remains
in the "wire or drop" bucket along with the rest of the M1 cleanup.)

---

## 5. Engine field-consumption matrix (verified)

Codified after grepping every reference in `internal/`. **USED** means a
production-code branch reads the field; **DEAD** means only the SPI struct
and the OpenAPI-generated DTO reference the field.

| Field | Status | Verified at |
|---|---|---|
| `WorkflowDefinition.Active` | USED | `engine.go:359, 405` (skip if false) |
| `WorkflowDefinition.Name` | USED (audit) | `engine.go:363, 379, 390, 460` |
| `WorkflowDefinition.Description` | DEAD | no consumer |
| `WorkflowDefinition.Version` | RESERVED (forward-looking) | no consumer today; reserved as the schema-version selector for future workflow-config shapes |
| `WorkflowDefinition.InitialState` | USED | `engine.go:139` (no validation) |
| `WorkflowDefinition.Criterion` | USED | `engine.go:363, 370` |
| `WorkflowDefinition.States` | USED | `engine.go:417, 511` |
| `TransitionDefinition.Name` | USED | `engine.go:428` (search by name) |
| `TransitionDefinition.Next` | USED | `engine.go:477` (no membership check) |
| `TransitionDefinition.Manual` | USED | `engine.go:519` (cascade skip) |
| `TransitionDefinition.Disabled` | USED | `engine.go:440, 519` |
| `TransitionDefinition.Criterion` | USED | `engine.go:447, 524` |
| `TransitionDefinition.Processors` | USED | `engine.go:548` |
| `ProcessorDefinition.Type` | DEAD | no consumer |
| `ProcessorDefinition.Name` | USED (audit) | `engine_processors.go` |
| `ProcessorDefinition.ExecutionMode` | USED | `engine_processors.go:63` (unknown→SYNC) |
| `ProcessorConfig.AttachEntity` | USED (dispatcher) | `internal/grpc/dispatch.go:68, 183` |
| `ProcessorConfig.CalculationNodesTags` | USED (dispatcher) | `internal/grpc/dispatch.go:47, 187`; `internal/cluster/dispatch/cluster_dispatcher.go:65` |
| `ProcessorConfig.ResponseTimeoutMs` | USED (dispatcher) | `internal/grpc/dispatch.go:104, 248` (defaults to 30 s) |
| `ProcessorConfig.RetryPolicy` | DEAD | no consumer |
| `ProcessorConfig.Context` | USED (dispatcher) | `internal/grpc/dispatch.go:71, 221` — pass-through string forwarded as `parameters` JSON node; resolved in v0.8.0 |
| `ProcessorConfig.StartNewTxOnDispatch` | USED | `engine_processors.go:205` (only when COMMIT_BEFORE_DISPATCH; validated by `validate.go:51`) |

The dispatcher consumption matters: every claim about a "dead" config field
was checked outside the engine package as well. Of the boundary-accepted
fields with no consumer:

- `ProcessorConfig.Context` and `ProcessorConfig.RetryPolicy` are
  **implementation gaps**, not contract artefacts — see the special-case
  subsections in §M1 above.
- `WorkflowDefinition.Version` is **reserved as a schema-version
  discriminator** for future config-shape evolution; not inert by intent.
- `WorkflowDefinition.Description` and `ProcessorDefinition.Type` are the
  only genuinely-dormant fields (and `Type` is already being reshaped by
  #250).

---

## 6. Cross-cutting risks worth highlighting

1. **The engine is biased toward "succeed silently with a default" over
   "fail loudly".** H4 (unknown ExecutionMode), H5 (default workflow
   fallback), H6 (no state-graph validation), M3 (empty-workflows wipes),
   and the dispatcher's `extProc == nil → return nil` no-op
   (`engine_processors.go:140, 159`) all point the same direction. An
   import that any other state-machine engine would reject runs to
   completion here, with no actual processing, no error, and at most a
   warning buried in the response body. This is the single highest-impact
   pattern in the report.

2. **The OpenAPI contract is the source of truth for some clients but is
   ahead of the implementation in three concrete places**: scheduled-transition
   processors (H1), `asyncResult`/`crossoverToAsyncMs` (M6), and the
   undocumented `Active=true` force-override (H2). Clients that take the
   schema as authoritative will be silently disappointed.

3. **The persistence SPI lacks the primitives the handler needs.** The
   handler attempts a read-merge-write but `WorkflowStore` exposes none of
   the optimistic-concurrency tools the entity store has
   (`CompareAndSave`). Until that's added at the SPI, no amount of handler
   logic will make import atomic under concurrency (H3).

4. **The test suite is well-built for the happy path and under-built for
   the negative space.** Every weakness in §M9 is a one-test fix; they are
   cheap to add and would catch most of the H/M findings above.

---

## 7. Quick remediation map

Each item below maps directly to a finding; this is a punch list, not a
plan.

| Finding | Tracking | Smallest fix that closes the gap |
|---|---|---|
| H1 | #250 → #251 → #252 | Wait for #250 (schema/help cleanup), then #251 to implement scheduled-transition firing as its own primitive. Until then the OpenAPI carries a shape the engine ignores. |
| H2 | **Untracked — open issue needed** | Stop force-setting `Active=true` at `handler.go:101–103`; default `Active=true` only when the field is *absent*, not when it is `false`. Add a TDD test covering explicit `"active": false`. Decide cyoda-cloud parity vs. operator intent first. |
| H3 | #35 (+ cassandra sibling) | Pick a direction from #35's four candidates. Interim: add a per-`(tenant, modelRef)` mutex in the handler so single-node deployments are race-free while the SPI decision matures. |
| H4 | **Untracked — open issue needed** | Add an enum check on `ExecutionMode` to `validateWorkflows`. The four valid values are already centralised in `validate.go:17–21`. |
| H5 | **Untracked — open issue needed** | Decide explicitly whether the default-workflow fallback survives a successful REPLACE with empty workflows; either way, surface the substitution in audit / slog at INFO and consider failing import when no workflow matches an entity. |
| H6 | **Untracked — open issue needed** | Extend `validateWorkflows` with `InitialState ∈ States`, `Transition.Next ∈ States`, non-empty workflow / transition names, and per-state transition-name uniqueness. None of these checks need new types. |
| M1 | #250 (partial), #223 (gates RetryPolicy) | #250 reshapes `ProcessorDefinition.Type` out of the discriminator role. **Open a dedicated issue to wire `ProcessorConfig.Context`** — assign `processor.Config.Context` into `req.Parameters` at `internal/grpc/dispatch.go:57–67` (processor path) and `:197–203` (criteria path); the wire field already exists. **Open a separate issue to wire `ProcessorConfig.RetryPolicy`** — see §9 for the multi-part scope (surface `Retryable` on `ProcessingResponse`, add an exclusion-set variant of `FindByTags`, implement the retry loop with member failover, add server-side config for `numRetries`/`delayMs`, gate on #223 for async noRetryMode). Separately decide "wire or drop" for `WorkflowDefinition.Version`, `WorkflowDefinition.Description`. |
| M2 | **Untracked — open issue needed** | Add the model-existence check to the export handler, mirroring `handler.go:65–83`. |
| M3 | **Untracked — open issue needed** | Treat empty incoming as a 400 in REPLACE/ACTIVATE, or add an explicit `force: true` opt-in. |
| M4 | **Untracked — open issue needed** | Add a duplicate-name + empty-name validation in `validateWorkflows`. |
| M5 | **Untracked — open issue needed** | Document the default mode and case-insensitivity in OpenAPI, and add the missing tests. |
| M6 | #145, #223 (partial) | Extend #145's scope to include `internal/domain/workflow/handler.go:43`. Decide via #223 whether `asyncResult` / `crossoverToAsyncMs` belong in the SPI; if not, drop them from the OpenAPI. |
| M7 | **Untracked — open issue needed** | Emit one `slog.Info` per successful import including mode, workflow names, and result count. |
| M8 | **Untracked — open issue needed** | Document the workflow-level criterion selection rule in `cmd/cyoda/help/content/workflows.md` and the OpenAPI `criterion` description. |
| M9 | #25, #122 (adjacent only) | The 18 cases in §M9 are not in either issue's scope. **New issue needed** to enumerate the cases as a single test-coverage sweep. |

---

## 8. Pointers for follow-up audits

- The Cassandra plugin (`../cyoda-go-cassandra`) consumes the same SPI; if
  `WorkflowStore.CompareAndSave` is added, the Cassandra implementation
  needs the same primitive. Parity registry: `e2e/parity/registry.go`.
- Several of the H findings hinge on Cyoda Cloud's actual behaviour. The
  `Active=true` force-override comment in `handler.go:100–103` claims this
  matches Cloud; if Cloud has since changed, that comment is stale and the
  remediation in H2 should be coordinated with Cloud.
- The `predicate.ParseCondition` evaluator at `engine.go:596` is the choke
  point for criterion correctness; a follow-up audit on its error surface
  would complement this report.

## 9. Recommended implementation order for v0.8.0

The 12 v0.8.0 issues fall into two natural tracks. The **SPI release** is
the critical path; the **handler/validator changes** can mostly run in
parallel and only converge for the strict-decoder switch-on at the end.

### Critical path — SPI release (sequential)

1. **#250** — foundational reshape (`ProcessorDefinition.type` axis split). #259 and #260 sit on top of this. Refactor only, no behaviour change.
2. **#259, #260, #261** in parallel — three additive shape changes on the reshaped SPI.
3. **Tag `cyoda-go-spi` v0.8.0** — one coordinated release with the four changes above.
4. **Bump cyoda-go `go.mod`** to the new SPI version — single integration PR; updates `go.mod`, `COMPATIBILITY.md`, and generated code.

### Parallel track — handler/validator (no SPI dependency)

Can start immediately, alongside the critical path. Order *within the track*
matters to minimise merge conflicts and stage behaviour cascades cleanly.

- **#263** first — single-PR mutex, no dependencies, closes a real race. Lowest-risk warm-up.
- **#253** — single-PR Cloud-parity wiring in the dispatcher. Independent.
- **#255** — validator hardening. Behaviour change (new 400s for previously-accepted bad workflows). Lands *before* #262 because both extend `validate.go` and #255 is the larger diff.
- **#262** — RetryPolicy validation + surface flag. Rides on #255's validator structure.
- **#256** — three small PRs in this order:
  1. **H2** (handler DTO swap to `*bool`) — smallest, most isolated.
  2. **M3** (validator/import.go reject empty `workflows`).
  3. **H5** (engine.go `slog.Warn` upgrade across four fallback sites) — touches the most call sites.

### Convergence — depends on SPI release

- **#264** — strict-decoder switch-on. **Must follow** #259 / #260 / #261 so newly-added SPI fields aren't rejected as unknown.
- **#257** — boundary hygiene sweep. Includes the `Workflow.Description` "wire or drop" decision (SPI matter), so it lands after the SPI release. Non-SPI items in #257 could land earlier; bundling keeps PR review coherent.

### Final gate

- **#258** — test-coverage sweep. Asserts post-cluster behaviour from #255, #256, #257, #259–#262, and #264. Lands last so every test asserts the final state of v0.8.0. Scaffolding can land earlier with `t.Skip` markers; removing skips closes this issue.

### Gantt

```
Critical (SPI):    [#250]──[#259 #260 #261]──[spi v0.8.0 tag]──[go.mod bump]──[#264]──[#257]──┐
                                                                                              │
Parallel (handler): [#263]──[#253]──[#255]──[#262]──[#256 H2]──[#256 M3]──[#256 H5]──────────┤
                                                                                              │
Final gate:                                                                          [#258]──┘
```

### Practical notes

- **~12 PRs total**: 3 critical-path commits (plus the SPI tag coordination event) + ~9 parallel-track commits.
- **Release-note coordination**: #255, #256 (×3), #257, #264 are all behaviour changes. Maintain a running release-note draft from PR 1.
- **Worktree per cluster** per `.claude/rules/superpowers-worktree-ordering.md` — brainstorm + plan inside a feature worktree before each cluster's first commit. Especially important for #256 because the three sub-changes share a release-note context.
- **Parent issues #35, #223 stay open** through v0.8.0 — their carve-outs (#263, #261) close, but the parents track the deferred runtime/SPI work. Reviewers should be reminded the parents are intentionally not in the milestone.
- **First PR to land**: **#263**. Small, low-risk, closes a real bug, and validates the v0.8.0 workflow (worktree → brainstorm → plan → TDD) end-to-end before bigger PRs go through.

---

## 10. Issues that should be opened or amended (historical — all done as of 2026-05-19)

Synthesising the cross-reference in §0 and the per-finding notes:

- **Amend #145** to add `internal/domain/workflow/handler.go` (the workflow
  import boundary) to its named scope before the issue closes. Without this
  the M6 finding survives #145's acceptance.
- **Amend #35** with the workflow-import-handler perspective: the audit
  confirms the read-merge-write race is observable from the public HTTP
  surface, not just from in-process callers. Useful context for the
  postgres-vs-advisory-lock decision.
- **Open one new issue per untracked H finding** (H2, H4, H5, H6) — each is
  small enough to fix independently, and grouping them dilutes the design
  decision in H2/H5 (which need cyoda-cloud-parity verification) with the
  mechanical fixes in H4/H6.
- **Open one issue for untracked M findings** as a "workflow import/export
  hardening" sweep: M2, M3, M4, M5, M7, M8 are all small and naturally land
  together.
- **Open one issue for M9** enumerating the 18 missing tests; this should
  block any change to `applyImportMode` or `validateWorkflows`.
- **Open a dedicated issue for `ProcessorConfig.Context` (M1)** — this is
  the only M1 member that is *meant* to do work. Per Cloud semantics it is a
  pass-through string carried verbatim into
  `EntityProcessorCalculationRequestJson.Parameters` /
  `EntityCriteriaCalculationRequestJson.Parameters`, letting a single
  external processor implementation serve multiple logical roles. The wire
  fields already exist (`api/grpc/events/types.go:822, 2403`); the
  remediation is a one-line assignment at
  `internal/grpc/dispatch.go:57–67` and `:197–203`. The issue should also
  add a parity-suite test that verifies a `context` string sent on import
  surfaces in the `parameters` node of the next outgoing CloudEvent for
  both the processor and criteria dispatch paths.
- **Open a dedicated issue for `ProcessorConfig.RetryPolicy` (M1)** —
  Cloud-parity gap, not a cleanup decision. The issue should cover:
  1. **Surface the inbound `retryable` flag.** Add `Retryable *bool` to
     `internal/grpc/members.go:ProcessingResponse` and plumb it from the
     incoming CloudEvent error shape (`api/grpc/events/types.go:36, 111,
     189, 261, ...` already declare it on the wire) into the response
     handler.
  2. **Add an exclusion-set member lookup.** Either widen
     `registry.FindByTags` with an `excluding []memberID` parameter or
     introduce a sibling `FindByTagsExcluding`. The current call sites at
     `internal/grpc/dispatch.go:47, 187` become loop-driven.
  3. **Implement the retry loop in the dispatcher** at
     `internal/grpc/dispatch.go:46–135` (processor) and `:181–260`
     (criteria) — same shape: `FindByTags(excluding)` → `Send` → `select`,
     accumulating tried members, stopping early on member-vetoed
     `retryable=false`, aggregating per-member failures on exhaustion.
  4. **Settle the policy semantics.** `NONE` → 1 attempt; `FIXED` → 1 +
     up to `numRetries` server-configured attempts with `delayMs` between
     tries. Default when `RetryPolicy` is unset is `FIXED`. Validation:
     reject any other string at import.
  5. **Add server-side config keys.** `CYODA_RETRY_FIXED_NUM_RETRIES`
     (default 3) and `CYODA_RETRY_FIXED_DELAY_MS` (default 500), wired
     through `cmd/cyoda/help/content/config/*.md`, `DefaultConfig()`, and
     `README.md` per the documentation-hygiene gate.
  6. **Honour async suppression.** `asyncResult=true` forces
     `noRetryMode` regardless of policy. This couples to #223 — until
     #223 lands the async branch, document the dependency and either gate
     this issue on #223 or pre-emptively wire the suppression check
     against the unimplemented field.
  7. **Aggregate-failure error shape.** On exhaustion produce one error
     enumerating each tried member and its failure, deduplicated with
     counts; do not propagate just the last failure.
  8. **Parity test in the externalapi suite.** Drive a processor backed
     by two members where the first fails non-vetoed and the second
     succeeds; assert one final success, two attempts, and that
     `usedMembers` excluded the first on the second pass. Add a second
     test with `retryable=false` confirming the second attempt is *not*
     made. Add a third with `asyncResult=true` confirming policy is
     ignored.
- **Open one issue for the remaining truly-dormant M1 fields**
  (`WorkflowDefinition.Description` only — `Workflow.Version` is reserved
  as a forward-looking schema-version selector per the L5 note and is
  excluded from this sweep): decision is "wire or remove from OpenAPI" for
  `Description`. No known Cloud-equivalent semantic; pure spec drift today.
