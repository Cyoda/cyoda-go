# Stream D — the documentation no other stream owns

Spec: `docs/superpowers/specs/2026-09-19-254-callout-failover-design.md` §14
(checklist), §15 (the `### Breaking` list), and §3–§9 as the source of every
sentence about behaviour. Nothing below describes code from a guess: where a
text names an identifier, the task carries a grep that proves the identifier
exists before the text is committed, and the sentence is dropped if it does not.

Documentation tasks have no RED/GREEN cycle. Each has **Verify** steps instead:
the guard tests that exist for the file, and greppable checks that a false
sentence is gone.

## Guards that exist (found by reading `cmd/cyoda/help/*_test.go`)

| Guard | What it pins | Run |
|---|---|---|
| `TestSeeAlsoResolution` (`help_test.go:656`) | every front-matter `see_also` entry resolves to a topic — so `errors.CALLOUT_FAILED` / `errors.CALLOUT_SUPERSEDED` may be listed only after O's and F's topic files exist | `go test ./cmd/cyoda/...` |
| `TestErrCode_Parity` (`help_test.go:546`) | `ErrCode*` constants ↔ `errors/<CODE>.md`, existence only | same |
| `TestHelpContent_NoIssueIDs` | no `#NNN` / `/issues/NNN` in help content | same |
| `TestHelpContent_CrossReferencesUseAWorkingInvocation` | `cyoda help a b`, never `cyoda help a.b` — in help content, `README.md`, `api/openapi.yaml` | same |
| `TestGRPCEventTypeCatalogueParity` | every `Entity…Request` constant appears in `grpc.md` | same |
| `TestDefaultTree_ConfigClusterSubtopic` | `CYODA_DISPATCH_WAIT_TIMEOUT` stays in `config.cluster` | same |
| `TestRunHelp_NoDuplicateSeeAlso`, `TestRunHelp_SeeAlsoUsesCLISyntax` | rendering of SEE ALSO | same |

No test reads `docs/ARCHITECTURE.md`, `docs/cloud-parity/*`, `docs/CONCURRENCY.md`,
`docs/PROCESSOR_EXECUTION_MODES.md` or `CHANGELOG.md`; their verification is
grep checks and a read-through. `errors.md`'s index has no parity test (R§7).

## Vocabulary

User-facing help says **compute member** for a customer's compute program (34
uses in the help tree; "compute node" 10, "calculation node" 4) and **node** for
a cyoda-go process (`cluster.md` throughout). This stream writes "compute
member" and "node" in help, README and CHANGELOG, and replaces "calculation
node" where it touches a sentence. `pnode` / `cnode` stay inside
`docs/superpowers/`. The one exception is the fixed message of
`CALLOUT_SUPERSEDED` (spec §8.2), quoted as it is: `this compute node was
replaced, or its callout has ended`. `docs/cloud-parity/callout-failover.md` is
read by the Cloud team and uses the same two words, defined in its first
section.

## Ownership of spec §14 — every item, and who has it

Finished sections were grepped for each file name; F, O and P are not written
yet (`ls docs/superpowers/plans/2026-09-19-254-callout-failover/` shows C, H, L,
M only), so their rows follow the lead's assignment.

| §14 item | Owner | Where |
|---|---|---|
| `config/cluster.md` — new and changed settings | **C** | C-2 (five bullets, `see_also`), C-11 (`CYODA_TX_TOKEN_TTL` bullet removed) |
| `config/cluster.md` — identity must fit the metadata | **M** | M-9 step 2 |
| `config/grpc.md` — tries, answer limit, the PostgreSQL ceiling paragraph | **C** | C-1, C-2 |
| `workflows.md` — version literals `:51 :569 :621` | **C** | C-8 |
| `workflows.md` — validation list (`:489` + two new bullets) | **C** | C-6, C-7 |
| `workflows.md` — `responseTimeoutMs` lines `:191`, `:283` | **C** | C-1, C-7 |
| `workflows.md` — `idempotent`; `retryPolicy` on all three; "captured but not consumed" removed; tag sentence → any-overlap; side effects and the saga responsibility; the §8.1 mode table; "changing a model or workflow from a processor is not supported"; EdgeMessage guidance; the function fail-closed paragraph `:307-315`; ERRORS list; `see_also` | **D** | D-1 |
| `search.md:201` (30000 default) | **C** | C-1 |
| `search.md` — `function.config.retryPolicy` is not listed at all | **D** | D-1 step 7 |
| `config/database.md:95`, `errors/DISPATCH_TIMEOUT.md:25` (the default phrase only) | **C** | C-1 |
| `grpc.md` — selection is round robin; any-overlap; empty tags | **L** | L-3 |
| `grpc.md` — same request id on every try, what a de-duplicating SDK does; criteria and functions must have no effects | **L** | L-8 |
| `grpc.md` — a callback is refused once its member was replaced; callbacks of one transaction run one after another; `success=false` and the verdict; the stale `ClusterDispatcher` sentence `:412`; ERRORS list; `see_also` | **D** | D-2 |
| help sentence "a 503 or a dropped connection on a joined write means the outcome is unknown" | **F** | D-3 reserves its place (last paragraph of `cluster.md` COMPUTE CALLBACK TRANSACTION ROUTING) and does not write it |
| `cluster.md` — DISCOVERY | **M** | M-9 step 1 |
| `cluster.md` — TRANSACTION ROUTING (two false statements today), CROSS-NODE DISPATCH FAILOVER, DISPATCH REPLAY PROTECTION, COMPUTE CALLBACK TRANSACTION ROUTING | **D** | D-3 |
| `errors/CALLOUT_SUPERSEDED.md` (new) + its `errors.md` index row | **F** | — |
| `errors/CALLOUT_FAILED.md` (new); `WORKFLOW_FAILED.md`, `DISPATCH_TIMEOUT.md`, `COMPUTE_MEMBER_DISCONNECTED.md`, `NO_COMPUTE_MEMBER_FOR_TAG.md`, `DISPATCH_FORWARD_FAILED.md` revised; `errors.md` index and its false trailer-metadata sentence | **O** | — (see Open point 3: the index also has `TRANSACTION_EXPIRED` at `400`; the code answers `410`) |
| `telemetry.md` — membership instruments | **M** | M-7 |
| `telemetry.md` — callout counters; the `cyoda.dispatch.duration` buckets | **O** | — |
| `config/scheduler.md` — the redispatch throttle against a callout's worst case | **D** | D-4 |
| `workflows/schema-version.md` | **C** | C-8 |
| `cloudevents.md` and the see-also lists naming changed topics | **D** | D-4 (`cloudevents.md`), D-1/D-2 (the other two lists) |
| `docs/cloud-parity/scheduled-transitions.md:273` | **D** | D-5 |
| `README.md` configuration reference; `DefaultConfig()`; `config_registry.go` | **C** | C-1, C-2, C-11 |
| `ARCHITECTURE.md` — cluster discovery §4.1, DD-7, operational-limits row `:1922`, §14.5 last sentence, package list | **M** | M-9 step 3 |
| `ARCHITECTURE.md` — settings tables `:1598-1619` | **C** | C-1, C-2, C-11 |
| `ARCHITECTURE.md` — selection sentence `:1232` | **L** | L-3 |
| `ARCHITECTURE.md` — §4.2 `Claims`, §4.3 whole (incl. `:604-612`, the "no failover" row `:662`), §4.4 swimlane steps t3a–t3f, L7 partition analysis `:859-867`, `:1153`, DD-2, DD-9, failure-mode row `:1899`, "Dispatch poll interval" row `:1929`, package list | **D** | D-6. L left `:604-612` "to the owner's-loop stream" and M left `:608-665`, `:1899`, DD-9, `:1929` "to the callout streams"; O is not written, so D takes them, and O must not |
| `docs/workflow-schema-versioning.md` | **C** | C-8 |
| `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` §M1 rows `:34`, `:329`, `:335` | **C** | C-6 |
| same document, "Current state of cyoda-go" paragraph `:402-416` | **D** | D-7 |
| `docs/cloud-parity/callout-failover.md` (new) + README row | **D** | D-5 |
| `CHANGELOG.md` `[Unreleased]` — fragments | **C, L, M** | C-2, C-8, C-11, L-3, L-7, M-9 |
| `CHANGELOG.md` `[Unreleased]` — everything else, and the assembly | **D** | D-9 (last) |
| `COMPATIBILITY.md` — the SPI pin | **C** | C-10 |
| Stale comment `members.go:48-55` | **L** | L-7 (its exit check greps `current dispatcher is single-shot`) |
| Stale comment `validate.go:66-68` | **C** | C-6 (retry-policy comment `:56-74`) |
| Stale comment `internal/common/errors.go:18-20` | **D** | D-8 — and the two orphans beside it, `:16` and `:22` |
| Stale comment `arm.go:243` | **D** | D-8 |
| *Not in §14, false once this lands:* `docs/PROCESSOR_EXECUTION_MODES.md` (§3 lifecycle, failure table, "no engine-side retry"); `docs/CONCURRENCY.md` §7 class 3; `docs/cloud-parity/nested-join-tx-serialisation.md`; `docs/cyoda/schema/common/BaseEvent.json` `retryable` description | **D** | D-7, D-5, D-4 — see Open points 1 and 2 |

## Order

D-8 can go at any time after L-7. D-1…D-7 each wait for the streams named in
their **Needs** line. D-9 is the last task of the whole plan bar C-10/C-11's
position rules: it runs after every task that adds a CHANGELOG fragment.

---

### Task D-1: `workflows.md` — what a processor, a criterion and a function promise

**Spec:** §2 D1–D3, D6, D7; §3 (the "may another be tried" table); §7 "Not part
of this design, by ruling"; §8.1; §8.2; §14. Brief §1, §2.

**Needs:** C-1, C-6, C-7, C-8 (same file — rebase on them, keep their lines);
L-3 (grpc.md wording this topic points at); the O task that makes
`internal/callout.Coordinator` the `ExternalProcessingService` (until then the
text is false); F's `errors/CALLOUT_SUPERSEDED.md` and O's
`errors/CALLOUT_FAILED.md` (`TestSeeAlsoResolution`).

**Files:**
- Modify: `cmd/cyoda/help/content/workflows.md` — front matter `see_also`;
  PROCESSORS `:154`, `:162-165`, `:173`, `:190`, `:192-199`; SCHEDULED
  TRANSITIONS `:275-284`, `:307-315`; ERRORS `:549-556`; SEE ALSO
- Modify: `cmd/cyoda/help/content/search.md` — the `function.config` list `:197-201`

- [ ] **Step 1: `calculationNodesTags` (`:190`) — the sentence says "all", the code is any-overlap (R§2.3)**

Replace the bullet with:

```markdown
- `calculationNodesTags` — string — comma-separated tags that select the compute members this callout may go to. A member matches when it declares **at least one** of the tags; when the list is empty every compute member of the tenant matches. With no matching member attached anywhere in the cluster the callout waits for one (see **Tries, waiting and time** below) and then fails with `errors.NO_COMPUTE_MEMBER_FOR_TAG`
```

In `:154` replace "dispatched via gRPC to a calculation node selected by
`Config.calculationNodesTags`" with "dispatched via gRPC to a compute member
selected by `config.calculationNodesTags`".

- [ ] **Step 2: `retryPolicy` (`:192-199`) — delete the "captured but not consumed" note; add `idempotent`**

Replace the `retryPolicy` bullet, whole, with these two bullets (the
`responseTimeoutMs` bullet above them is C-1's and stays):

```markdown
- `retryPolicy` — string — whether the callout may be given to more than one compute member. `NONE`: one try. `FIXED`, or omitted: one try plus the server's `CYODA_RETRY_FIXED_NUM_RETRIES` (default `3`, so four tries). The number is configured on the server, not in the workflow, and it is the normal number, not a hard limit — see **Tries, waiting and time**. There is no pause between one member and the next, and no delay setting. Import rejects any other value with `400 VALIDATION_FAILED`.
- `idempotent` — boolean, optional, default `false` — a declaration by the workflow author, not something cyoda can check. Setting it says: *this processor may be run more than once for the same callout — possibly only partly, possibly at the same time on two compute members — and the outcome is the same as running it once, in cyoda **and in every system the processor touches**.* See **Repeating a processor** below for what qualifies.
```

- [ ] **Step 3: new subsection after the ProcessorConfig field list (after the `crossoverToAsyncMs` bullet, before `## SCHEDULED TRANSITIONS`)**

````markdown
**Tries, waiting and time.** A processor, a `function`-type criterion and a `schedule.function` are all *callouts*, and the same rules apply to all three.

- A **try** is one attempt to hand the callout's work to one compute member. A member is never tried twice in one run over a node's members, and every try of one callout carries the same `requestId` (see `cyoda help grpc`).
- **Waiting for a member to exist is not a try.** When no matching member is attached anywhere, the callout waits up to `CYODA_DISPATCH_WAIT_TIMEOUT` (default `5s`, one allowance for the whole callout) and wakes the moment one attaches. This applies on a single node as in a cluster, and with `retryPolicy: NONE` too. `0` switches the wait off.
- **Whether another member is tried depends on how far the first one got:**

| What happened | Criterion, function, or processor with `idempotent: true` | Processor, `idempotent` false |
|---|---|---|
| The work never reached the member (it had gone, or was not taking data) | another member is tried | another member is tried |
| The work reached the member; then no answer within `responseTimeoutMs`, or its connection dropped | another member is tried | **nothing else is tried**; the callout fails |
| The member answered `success: false` | nothing else is tried; the member's message and its `retryable` verdict go to the client | the same |

- **The number of tries is the normal number, not a hard limit.** In a cluster the node holding the transaction can hand the callout to another node together with the tries that are left. If that node's answer is lost, one try is counted although it may have made more. **Time is the hard limit:** a callout never runs longer than `tries × answer limit + CYODA_DISPATCH_WAIT_TIMEOUT + CYODA_CALLOUT_HANDOVER_ALLOWANCE`, fixed when it starts — 155 s at the defaults. See `cyoda help config grpc`.

**Repeating a processor.** A compute member that was given the work and then went quiet has not necessarily stopped, and what it has already done does not go away — inside cyoda or outside it.

- *Inside cyoda.* A member that has been replaced is shut out: its callbacks are refused with `errors.CALLOUT_SUPERSEDED`, and one that was in progress finishes before the next member is given the work. "Set this field to X" is safe to repeat. "Add 10 to the balance" is not. **"Create an entity" is never safe to repeat as it stands:** without a composite unique key on the model the repeat stores a second entity; with one, no duplicate is stored, but cyoda refuses the second create and the whole operation fails with a uniqueness error — the data is safe, the operation is lost. A processor that creates entities is repeat-safe only if it first checks whether its entity already exists.
- *Outside cyoda.* A processor may charge a card, send a message, call another service. None of that is part of a cyoda transaction: **a rollback does not undo it, and a repeat does it again.** Making an outside action safe to repeat, or compensating for it when the operation fails — a saga, a compensating step, a key the other system uses to recognise a repeat — is the application's responsibility. The `requestId`, which is the same on every try of one callout, is a ready-made key for that. It does not cover the client running the whole operation again: that is a new callout with a new id.
- A processor that only returns a changed entity — no callbacks, no outside systems — qualifies trivially; mark it `idempotent` so that it benefits.
- Criteria and functions are treated as repeat-safe by rule. **They must have no effects, inside cyoda or outside it.** cyoda does not enforce this.
- An **EdgeMessage** saved from a processor is not removed when the processor's member is replaced. Make that save idempotent and attach the message id to the entity that owns it, so that an orphan costs disk space and nothing else.
- **Changing a model or a workflow from inside a processor is not supported.**

**What a failed callout leaves behind, by `executionMode`.** A failure marked `retryable: true` speaks for cyoda's state only, and only where this table says "clean".

| Mode | Another member after "no answer" | When the callout fails | cyoda's state afterwards |
|---|---|---|---|
| `SYNC`, `ASYNC_SAME_TX` | only if `idempotent` | the operation fails | rolled back; clean for a re-run — unless an earlier `COMMIT_BEFORE_DISPATCH` processor of the same cascade had already committed |
| `ASYNC_NEW_TX` | only if `idempotent` | **the operation continues**: the processor's savepoint is undone and a warning is logged. Nothing reaches the client, not even a member's `retryable` verdict | the rest of the transaction commits. A savepoint that cannot be created, undone or released is not a processor failure: it fails the operation with a `5xx` and nothing commits |
| `COMMIT_BEFORE_DISPATCH`, `startNewTxOnDispatch: true` | only if `idempotent` | the operation fails | `TX_pre` **stays committed**; `TX_post` is rolled back. Not clean for a re-run |
| `COMMIT_BEFORE_DISPATCH`, `startNewTxOnDispatch: false` | only if `idempotent` | the operation fails | `TX_pre` stays committed; the member's callbacks were transactions of their own and stand |

The error a client sees: no member within the wait → `503 NO_COMPUTE_MEMBER_FOR_TAG`; a single failed try → that try's own code (`503 DISPATCH_TIMEOUT`, `503 COMPUTE_MEMBER_DISCONNECTED`, `503 DISPATCH_FORWARD_FAILED`); several failed tries → `503 CALLOUT_FAILED`, listing them; a member that answered `success: false` → `400 WORKFLOW_FAILED` carrying the member's own message, `retryable: true` when the member said so.
````

- [ ] **Step 4: the `executionMode` bullets (`:162-165`) — two claims the table above contradicts**

- `:162` `SYNC`: replace "processor failure (including timeout and
  `success=false` in the response) returns `errors.WORKFLOW_FAILED` (`400`) and
  the entity remains in the source state" with "a failed callout fails the
  operation and the entity remains in the source state; the error is set out
  under **What a failed callout leaves behind** below".
- `:164` `ASYNC_NEW_TX`: after "the transition completes" insert "; a savepoint
  that cannot be created, undone or released fails the operation instead".
- `:165` `COMMIT_BEFORE_DISPATCH`: replace "Failure of the dispatched processor
  (`success=false`, timeout, member crash) returns `errors.WORKFLOW_FAILED`
  (`400`) and the entity remains in the pre-callout state." with "A failed
  callout fails the operation — the error is as for `SYNC` — and the entity
  remains in the pre-callout state."

  (Today a timeout is `503 DISPATCH_TIMEOUT`, not `400`: R§4.3. The sentence was
  already wrong.)

- `:173` Idempotency bullet — append: "Setting `idempotent: true` on the
  processor is the matching declaration for a *repeat within one callout*; it
  does not replace the requirement here, which is about the *client* running the
  operation again."

- [ ] **Step 5: `schedule.function` (`:275-284`, `:307-315`)**

After the `responseTimeoutMs` bullet (C-7's text) add:

```markdown
- `retryPolicy` (string, optional) — `NONE` or `FIXED`, as on a processor; omitted means `FIXED`. A function is always treated as safe to repeat, so after a try that got no answer another compute member is tried.
```

Replace the *Fail-closed* paragraph's second sentence ("If the compute node is
unreachable … `COMPUTE_MEMBER_DISCONNECTED`)") with:

```markdown
If no compute member can be reached, or none answers, the write fails as a retryable `503` with the same codes as a processor or criterion callout (`NO_COMPUTE_MEMBER_FOR_TAG`, `DISPATCH_TIMEOUT`, `COMPUTE_MEMBER_DISCONNECTED`, `DISPATCH_FORWARD_FAILED`, or `CALLOUT_FAILED` when several tries failed); a member that answers `success: false` fails it as `400 WORKFLOW_FAILED` with the member's message
```

keeping the rest of the sentence ("— no state change commits against an
unschedulable transition.") and the `SCHEDULE_FUNCTION_INVALID_RESULT` sentence.

- [ ] **Step 6: ERRORS and the two see-also lists**

ERRORS — replace the three callout lines with:

```markdown
- `errors.WORKFLOW_FAILED` — `400` — the engine could not complete the transition; for a compute member that answered `success: false`, carries the member's message and its `retryable` verdict
- `errors.NO_COMPUTE_MEMBER_FOR_TAG` — `503` — no compute member matching `calculationNodesTags` appeared within `CYODA_DISPATCH_WAIT_TIMEOUT`
- `errors.COMPUTE_MEMBER_DISCONNECTED` — `503` — the compute member's connection dropped after it was given the work
- `errors.DISPATCH_TIMEOUT` — `503` — the compute member did not answer within `responseTimeoutMs`
- `errors.CALLOUT_FAILED` — `503` — every try of a callout failed; the message lists them
- `errors.CALLOUT_SUPERSEDED` — `410` — seen by a compute member, not by the client: a callback from a member that was replaced, or whose callout has ended
```

Add `errors.DISPATCH_TIMEOUT`, `errors.CALLOUT_FAILED`,
`errors.CALLOUT_SUPERSEDED`, `config.grpc` to the front-matter `see_also` and to
the `## SEE ALSO` list, in the same order in both.

- [ ] **Step 7: `search.md` — the `function.config` list omits `retryPolicy`**

After the `function.config.responseTimeoutMs` bullet (C-1's text) add:

```markdown
- `function.config.retryPolicy`: string (optional) — `NONE` or `FIXED`; omitted means `FIXED`. Validated at workflow import. A criterion is always treated as safe to repeat — see `cyoda help workflows`
```

- [ ] **Step 8: Verify**

Run: `go test ./cmd/cyoda/...`   Expected: PASS (`TestSeeAlsoResolution`,
`TestHelpContent_NoIssueIDs`, `TestHelpContent_CrossReferencesUseAWorkingInvocation`).

```
grep -n 'captured but not consumed\|single-shot\|Cloud honours both' cmd/cyoda/help/content/workflows.md   → no hits
grep -n 'declares all required tags' cmd/cyoda/help/content/workflows.md                                  → no hits
grep -n 'fixed delay\|N and delay' cmd/cyoda/help/content/workflows.md                                    → no hits
grep -n 'calculation node' cmd/cyoda/help/content/workflows.md                                            → no hits
grep -c 'idempotent' cmd/cyoda/help/content/workflows.md                                                  → ≥ 8
go run ./cmd/cyoda help workflows | grep -c 'Repeating a processor'                                       → 1
```

Against the code, before committing (drop the sentence if a check fails, and
say so in the commit body):
`grep -n 'CYODA_RETRY_FIXED_NUM_RETRIES' cmd/cyoda/help/config_registry.go` → 1 hit;
`grep -rn 'ErrCodeCalloutFailed\|ErrCodeCalloutSuperseded' internal/common/error_codes.go` → 2 hits.

- [ ] **Step 9: Commit**

```
git add cmd/cyoda/help/content/workflows.md cmd/cyoda/help/content/search.md
git commit -m "docs(help): what idempotent promises, retryPolicy on every callout, and what a failed callout leaves behind (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task D-2: `grpc.md` — what a compute member must know about callbacks

**Spec:** §7 ("Where it is enforced" 1–2, "One transaction, one user at a
time"), §8.2 rows `CALLOUT_SUPERSEDED`, `MemberFailed`; §14.

**Needs:** L-3 and L-8 (they rewrite TAG ROUTING; this task adds nothing there
except the last paragraph's correction); F's enforcement task and
`errors/CALLOUT_SUPERSEDED.md`; O's `errors/CALLOUT_FAILED.md`; the P task that
deletes `ClusterDispatcher`.

**Files:**
- Modify: `cmd/cyoda/help/content/grpc.md` — front matter; COMPUTE MEMBER
  PROTOCOL (`:223-228`, `:277`, `:357-358`); TAG ROUTING `:412`; ERRORS
  `:439-444`; SEE ALSO

Already done by L and **not repeated**: round robin, any-overlap, empty tags
(L-3); same `requestId` on every try, de-duplicating SDKs, criteria and
functions must have no effects (L-8).

- [ ] **Step 1: "What a compute member must do" (`:223-228`) — add three bullets**

```markdown
- Echo the transaction token (`cyodatxtoken`) on every callback — see `cyoda help cluster`. A token belongs to one try of one callout. Once the server has given the callout to another member, or the callout has ended, a callback bearing the token is refused with `errors.CALLOUT_SUPERSEDED` (`410`) for as long as the transaction is open, and with `errors.TRANSACTION_NOT_FOUND` (`404`) after that. Neither is retryable: stop working on that request.
- Expect callbacks of one transaction to run **one after another**. Every callback — a read or a search as much as a write — holds its transaction for the time the server works on it, so two callbacks sent in parallel are served in turn, not at once. The server reads the whole request before it takes the transaction and sends the response after it has let go, so a slow upload or a slow reader holds nothing up.
- A callback is not abandoned when its member's connection drops: the server finishes it, or refuses it. A deadline the member sets on its own callback call is not applied.
```

- [ ] **Step 2: `success=false` (`:277`, and the function paragraph `:357-358`)**

Replace "When `success=false`, the workflow engine fails the processor
dispatch." with:

```markdown
When `success=false`, no other member is tried. The client's operation fails with `400 WORKFLOW_FAILED` carrying `error.message`, and `error.retryable: true` is passed on as the client's `retryable: true` — it tells the client that running the whole operation again may succeed; it does not make the server try another member. (An `ASYNC_NEW_TX` processor is the exception: its failure is logged and the operation continues.)
```

In the function paragraph replace "`success: false` or an `error` fails the
dispatch the same way a processor or criteria failure does" with "`success:
false` fails the callout as it does for a processor: no other member is tried,
and the message and `retryable` verdict reach the client".

- [ ] **Step 3: TAG ROUTING last paragraph (`:412`) names a type that no longer exists**

Replace with:

```markdown
In cluster mode each node tells its peers which tags its members serve, per tenant. A node tries its own matching members first and then hands the callout, with the tries that are left, to a peer that advertises the tag — see `cyoda help cluster`.
```

- [ ] **Step 4: ERRORS (`:439-444`)**

Replace the four-line list with:

```markdown
Callout errors the client of the failed operation sees (all `503`, retryable):

- `errors.NO_COMPUTE_MEMBER_FOR_TAG` — no matching member appeared within `CYODA_DISPATCH_WAIT_TIMEOUT`
- `errors.COMPUTE_MEMBER_DISCONNECTED` — the member's stream dropped after it was given the work
- `errors.DISPATCH_TIMEOUT` — no answer within `responseTimeoutMs`
- `errors.DISPATCH_FORWARD_FAILED` — the answer of the node a callout was handed to was lost
- `errors.CALLOUT_FAILED` — more than one try failed; the message lists them

Errors a compute member sees on a callback:

- `errors.CALLOUT_SUPERSEDED` — `410` — the member was replaced, or its callout has ended
- `errors.TRANSACTION_NOT_FOUND` — `404` — the transaction has ended
- `errors.TRANSACTION_EXPIRED` — `410` — the token is past its expiry
```

Add `errors.CALLOUT_FAILED`, `errors.CALLOUT_SUPERSEDED`, `cluster` to the
front-matter `see_also`; bring `## SEE ALSO` in line with the front matter (it
lacks `cloudevents` and `errors.SCHEDULE_FUNCTION_INVALID_RESULT` today).

- [ ] **Step 5: Verify**

Run: `go test ./cmd/cyoda/...`   Expected: PASS (incl.
`TestGRPCEventTypeCatalogueParity`).

```
grep -n 'ClusterDispatcher\|FindByTags\|chosen at random' cmd/cyoda/help/content/grpc.md   → no hits
grep -n 'fails the processor dispatch' cmd/cyoda/help/content/grpc.md                     → no hits
grep -c 'CALLOUT_SUPERSEDED' cmd/cyoda/help/content/grpc.md                               → ≥ 3
```

Against the code: `grep -rn 'WithoutCancel' internal/domain/txjoin/ internal/httpmw/ internal/grpc/txroute_interceptor.go`
→ at least one hit (the third bullet of step 1 rests on it; drop the bullet if
F did not land it).

- [ ] **Step 6: Commit**

```
git add cmd/cyoda/help/content/grpc.md
git commit -m "docs(help): a replaced compute member's callbacks are refused; callbacks of one transaction run in turn (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task D-3: `cluster.md` — hand-over, replay protection, callback routing

**Spec:** §5, §6 (how the owner reads an answer; transport), §7 (claims; on
entry), §9; §14 "`cluster.md`".

**Needs:** M-9 (DISCOVERY, same file); C-2, C-11; the P hand-over tasks
(response protection, authenticated `no_handoff`, connect timeout); O; F.

**Files:**
- Modify: `cmd/cyoda/help/content/cluster.md` — `:50`, `:54`, `:66-68`,
  `:76-78`, `:80-96`, front matter, SEE ALSO

Two statements in TRANSACTION ROUTING are false **today** and are corrected
because the section is touched: a token is not issued "when a node begins a
transaction" nor "returned to the client" — it is minted only when a callout is
given to a compute member (`internal/grpc/dispatch.go:67`,
`cluster_dispatcher.go:213` are the only `Signer.Issue` callers); and a bad
token is `401 UNAUTHORIZED`, an expired one `410 TRANSACTION_EXPIRED`
(`internal/cluster/proxy/http.go:84-105`), not `400`.

- [ ] **Step 1: TRANSACTION ROUTING (`:50-55`)**

Replace the first paragraph with:

```markdown
PostgreSQL transactions are bound to the connection that begins them (`pgx.Tx` is single-owner). When a node gives a callout to a compute member it mints a token that names the owning node, an opaque transaction reference, the callout and the try it belongs to, and an expiry, signed with HMAC-SHA256 keyed on `CYODA_HMAC_SECRET`. The compute member echoes it on its callbacks (HTTP header `X-Tx-Token`, gRPC metadata key `tx-token`). A token lives for its try's answer limit plus `CYODA_CALLOUT_PASS_ALLOWANCE`.
```

Replace the first failure-mode bullet with:

```markdown
- Signature mismatch or malformed token — `401 UNAUTHORIZED`. Expired token — `410 TRANSACTION_EXPIRED`.
```

- [ ] **Step 2: CROSS-NODE DISPATCH FAILOVER (`:66-68`) — rewrite whole, retitle `## CALLOUTS ACROSS NODES`**

```markdown
## CALLOUTS ACROSS NODES

A callout (processor, criterion or scheduled function) is run by the node that holds the operation's transaction — the owner. The owner first tries its own matching compute members, one after another. If that does not produce an answer and tries are left, it **hands the callout over** to one peer that advertises the tag, together with the number of tries left, the answer limit and the request id. The peer tries its own members only; it never hands on. The owner then asks the next such peer, and when nobody anywhere has a matching member it waits — up to `CYODA_DISPATCH_WAIT_TIMEOUT` in total — for one to attach or for a peer to announce one.

What counts as a try:

- A peer that **cannot be connected to** within `CYODA_DISPATCH_CONNECT_TIMEOUT`, or that answers that it handed the work to nobody, costs no try; the next peer is asked.
- A hand-over whose **answer is lost** — no reply, a broken connection, a non-2xx status, an answer that does not authenticate — counts as one try, because the peer may have given the work to a member. For a processor not declared `idempotent` nothing else is tried and the operation fails with `503 DISPATCH_FORWARD_FAILED`.
- Every hand-over opens a connection of its own, so "could not connect" is the only case in which a dead peer costs nothing. Behind a sidecar or an ingress the connection always opens and a dead peer shows as a `502`–`504`; with an `https://` node address a failed TLS handshake is not a connect failure either. Both count as a lost answer — the safe side.

The number of tries is therefore the normal number, not a hard limit. The time is: see `cyoda help config cluster` (`CYODA_CALLOUT_HANDOVER_ALLOWANCE`) and `cyoda help config grpc`.

Node clocks more than 30 seconds apart make a peer refuse a hand-over before reading it. That refusal cannot be authenticated, so it counts as a lost answer: clocks that far apart fail operations whose processors are not `idempotent` rather than being routed around. Keep node clocks synchronised.
```

- [ ] **Step 3: DISPATCH REPLAY PROTECTION (`:76-78`)**

Replace "The cache is fail-closed: when full, new dispatch envelopes are
rejected as replays until entries expire, surfacing to the forwarding node as a
retryable failure (which dispatch failover routes around)." with:

```markdown
The cache is fail-closed: when it is full, or a nonce repeats, the request is refused. The refusal is an authenticated answer saying that nothing was handed to a compute member, so the node that sent the hand-over asks the next peer and no try is used. Answers are protected like requests — encrypted, with a nonce of their own, and bound to the request they answer — and do not enter the cache.
```

Rename "inbound cross-node dispatches" → "inbound hand-overs" in the last
sentence; keep the arithmetic.

- [ ] **Step 4: COMPUTE CALLBACK TRANSACTION ROUTING (`:80-96`)**

Replace the first sentence ("When the workflow engine dispatches … mints a
signed tx-token and includes it …") with "The node that gives a callout to a
compute member mints a signed token for that try and includes it as the
`cyodatxtoken` CloudEvent extension attribute — also when the callout was
handed over, in which case the token still names the owner." and, after the
paragraph ending "until the owning transaction commits.", add:

```markdown
The owner admits a callback only while the token's callout is in progress and the token belongs to the member that currently has the work. A callback from a member that was replaced, or whose callout has ended, is refused with `410 CALLOUT_SUPERSEDED` while the transaction is open, and with `404 TRANSACTION_NOT_FOUND` afterwards. Before the owner gives the work to the next member, and before the workflow carries on after a callout, it waits for any callback still in progress on the transaction to finish. Callbacks of one transaction are served one at a time.
```

F's sentence about a `503` or a dropped connection on a callback that was routed
through another node goes **directly after this paragraph**; if F has landed it
elsewhere in this section, move it here unchanged. D-3 does not write it.

Front matter and SEE ALSO: add `config.cluster`, `config.grpc`, `grpc`,
`workflows`.

- [ ] **Step 5: Verify**

Run: `go test ./cmd/cyoda/...`   Expected: PASS.

```
grep -n 'CROSS-NODE DISPATCH FAILOVER\|fails over to the next\|trying each peer at most once' cmd/cyoda/help/content/cluster.md → no hits
grep -n '400 Bad Request\|returned to the client' cmd/cyoda/help/content/cluster.md                                          → no hits
grep -n 'dispatch failover routes around' cmd/cyoda/help/content/cluster.md                                                   → no hits
grep -rn 'CROSS-NODE DISPATCH FAILOVER' cmd/ docs/*.md README.md                                                             → no hits
```

Against the code: `grep -n 'DisableKeepAlives' internal/cluster/dispatch/*.go` →
1 hit; `grep -rn 'no_handoff' internal/cluster/dispatch/*.go` → hits in the
handler's replay branch. If P answered a replay with a bare `403` after all,
step 3's second sentence is replaced by "The refusal counts as a lost answer."

- [ ] **Step 6: Commit**

```
git add cmd/cyoda/help/content/cluster.md
git commit -m "docs(help): callouts across nodes — hand-over, what costs a try, and which callbacks are admitted (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task D-4: `cloudevents.md`, the `retryable` schema description, `config/scheduler.md`

**Spec:** §4 (request id vs. envelope id), D12, §9 "The scheduler", §17; §14.

**Needs:** L-8 (same `requestId` on every try); O's `errors/CALLOUT_FAILED.md`.

**Files:**
- Modify: `cmd/cyoda/help/content/cloudevents.md` (DESCRIPTION; `see_also`; SEE ALSO)
- Modify: `docs/cyoda/schema/common/BaseEvent.json:30` (a `description` string;
  `api/grpc/events/types.go` does not carry it — `grep -rn 'whether cyoda should retry' .`
  hits the schema only — so no regeneration)
- Modify: `cmd/cyoda/help/content/config/scheduler.md`

- [ ] **Step 1: `cloudevents.md`** — after the DESCRIPTION bullet list add:

```markdown
**Two ids.** The CloudEvent envelope's `id` is unique per event. A calculation request's payload carries its own `id` and `requestId`, and these are **the same on every try of one callout**: when a callout is given to a second compute member, the second request is a new event with the same `requestId`. A compute member correlates its response by `requestId`, and may use it as the key that makes an outside action safe to repeat. See `cyoda help grpc` and `cyoda help workflows`.
```

Add `errors.CALLOUT_FAILED`, `errors.COMPUTE_MEMBER_DISCONNECTED` to `see_also`
and to `## SEE ALSO`.

- [ ] **Step 2: `BaseEvent.json:30`** — the description says cyoda retries on
  `retryable: true`. It does not (D1, D12). Replace the string with:

```
Whether the sender judges that repeating the request could succeed. On a calculation response this is the compute member's verdict: cyoda passes it on to the client of the failed operation and does not give the work to another compute member because of it.
```

- [ ] **Step 3: `config/scheduler.md`** — replace the
  `CYODA_SCHEDULER_REDISPATCH_BACKOFF` bullet with:

```markdown
- `CYODA_SCHEDULER_REDISPATCH_BACKOFF` (duration, default: `30s`) — best-effort re-dispatch throttle window applied to a task once it is picked up, so the same due task isn't immediately re-dispatched on the next scan. It is a throttle, not a lease: a scheduled transition still running after this long is started a second time while the first is in progress. Only one of the two can commit, so the data stays correct, but the transition's processors run twice. One try of a callout that gets no answer already takes an answer limit (`CYODA_CALLOUT_RESPONSE_TIMEOUT_MS`, default `30000`), and a callout may take several; size this value above the longest scheduled transition you expect, or make the processors of scheduled transitions safe to repeat.
```

Add `config.grpc` to its `see_also` and SEE ALSO.

- [ ] **Step 4: Verify**

Run: `go test ./cmd/cyoda/...`   Expected: PASS (`cloudevents_test.go` re-marshals
every embedded schema; a broken JSON string fails it).
`python3 -c "import json;json.load(open('docs/cyoda/schema/common/BaseEvent.json'))"` → no output.
`grep -rn 'whether cyoda should retry' docs/ api/ cmd/` → no hits.

- [ ] **Step 5: Commit**

```
git add cmd/cyoda/help/content/cloudevents.md cmd/cyoda/help/content/config/scheduler.md docs/cyoda/schema/common/BaseEvent.json
git commit -m "docs: the request id is stable across tries; retryable is a verdict for the client; the redispatch throttle is not a lease (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task D-5: `docs/cloud-parity/callout-failover.md` (new), its README row, and the two parity files it corrects

**Spec:** §14; brief "Where this departs from Cloud"; R§5 (what Cloud does);
§2–§9 for the contract itself. Gate 7.

**Needs:** every behaviour stream landed (L, F, O, P) — the file says "cyoda-go
does", and Gate 7 logs what is implemented, not what is planned.

**Files:**
- Create: `docs/cloud-parity/callout-failover.md`
- Modify: `docs/cloud-parity/README.md` (table, last row)
- Modify: `docs/cloud-parity/scheduled-transitions.md` §10 (`:262-282`)
- Modify: `docs/cloud-parity/nested-join-tx-serialisation.md` ("Invariant Cloud must uphold")

Format, from `nested-join-tx-serialisation.md` and
`grpc-keepalive-and-member-eviction.md`: H1 "`<subject>` — Cloud twin-alignment
spec"; the two-sentence preamble; then `##` sections stating the contract in the
present tense, tables for status codes; no issue numbers.

- [ ] **Step 1: create `docs/cloud-parity/callout-failover.md`**

````markdown
# Callout failover — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's handling of a callout — a processor, criterion or function request to
a compute member — that cannot be delivered or is not answered. cyoda-go is the
authoritative implementation.

Words used here. A **node** is one platform process. A **compute member** is a
customer's compute program attached to one node. A **callout** is one processor,
criterion or function request. A **try** is one attempt to hand a callout's work
to one compute member. A **hand-off** is the moment the work is on the member's
connection: before it, the work provably never left the node; after it, the work
may have reached the member. A **callback** is an API request a compute member
makes while it works, carrying the transaction token it was given, and carried
out inside that transaction.

Why cyoda-go cannot simply do what Cloud does today: in Cloud a compute member's
callbacks are transactions of their own. In cyoda-go they join the operation's
transaction, so a second member given the same work can commit a processor's
writes twice, and a member that was given up on can still write.

## 1. One dividing line: was there a hand-off?

| What happened to a try | Criterion, function, or processor with `idempotent: true` | Processor, `idempotent` false (the default) |
|---|---|---|
| No hand-off: the member had gone, or its connection did not take the work | try another member | try another member |
| Hand-off, then no answer within the answer limit, or the connection dropped | try another member | **stop**; the operation fails |
| The member answered `success: false` | **stop**, whatever `error.retryable` says | **stop** |
| A failure that would be identical on any member (the request cannot be built, the principal cannot be attached) | stop | stop |

**Departure 1 — `idempotent`, default `false`.** A new boolean on a processor's
`config` (workflow schema 1.5). It is the author's declaration that the
processor may be run more than once for one callout — possibly only partly,
possibly at the same time on two compute members — with the same outcome as
running it once, in the platform and in every system the processor touches.
Cloud today behaves as if every processor carried it. Criteria and functions
are repeat-safe by rule: they must have no effects.

**Departure 2 — `error.retryable: true` does not fail over.** Cloud gives the
work to another member. cyoda-go stops and passes the verdict to the client:
another member inside the same transaction would be likely to fail the same way,
while the client running the whole operation again can succeed.

## 2. What the client sees

HTTP entity endpoints that run workflows, and the gRPC `EntityManage` /
`EntityManageCollection` envelopes (`CLIENT_ERROR` / `SERVER_ERROR`, the code as
the message prefix, `retryable` set when true).

| Situation | Status | Code | Retryable |
|---|---|---|---|
| No compute member appeared within the wait (§4) | 503 | `NO_COMPUTE_MEMBER_FOR_TAG` | yes |
| One try failed, and it was the only one on record | 503 | the try's own code: `DISPATCH_TIMEOUT`, `COMPUTE_MEMBER_DISCONNECTED`, `DISPATCH_FORWARD_FAILED` | yes |
| More than one try on record, none answered | 503 | `CALLOUT_FAILED` | yes |
| The member answered `success: false`, `error.retryable: true` | 400 | `WORKFLOW_FAILED` | **yes** |
| The member answered `success: false`, `error.retryable` false or absent | 400 | `WORKFLOW_FAILED` | no |

**Departure 3 — the member's message and verdict reach the client**, from
whichever node the member was attached to. Cloud passes on neither: the caller
sees a cancelled transaction with error code `-`. `WORKFLOW_FAILED` reads
`processor <name> failed: <the member's message>`.

`CALLOUT_FAILED` uses the shape of Cloud's exhaustion message:
`the callout could not be completed, got N failures: [member<id>: cause], [member<id>: cause (2 times)]`
— `N` counts before identical entries are collapsed; a single failure is not
wrapped. Member ids are the tenant's own connections; node ids and addresses
never appear.

`retryable: true` on a try that got no answer speaks for the platform's state
only, and only in the execution modes in which the failed operation is rolled
back whole (`SYNC`, `ASYNC_SAME_TX`). What the processor did outside the
platform is the application's to reconcile.

## 3. Tries

`retryPolicy: NONE` → one try. `FIXED`, or unset → one plus
`CYODA_RETRY_FIXED_NUM_RETRIES` (default 3). A member is never tried twice in
one run over a node's members. Every try carries the same `requestId` and
transaction id, as in Cloud.

**Departure 4 — no pause, and no delay setting.** Cloud sleeps `delayMs` before
every retry. cyoda-go moves straight to the next member: a pause helps only when
about to look at the same thing again.

**Departure 5 — `retryPolicy` on a scheduled-transition function**, and
validation of it on a criterion function, where it was accepted and ignored.

**Departure 6 — the number of tries is the normal number, not a hard limit; the
time is the hard limit.** The node holding the transaction tries its own
members, then hands the callout to one other node with the tries that are left.
If that node's answer is lost, one try is counted though it may have made more.
The time a callout may take is fixed when it starts —
`tries × answer limit + the wait of §4 + CYODA_CALLOUT_HANDOVER_ALLOWANCE` — and
no try starts after it; one in progress is cut off at it.

## 4. Waiting for a member to exist

**Departure 7 — waiting is separate from trying.** Cloud counts "no member
available" as a failed try, spending the retry budget on re-querying. In
cyoda-go a callout with nothing to try waits, up to
`CYODA_DISPATCH_WAIT_TIMEOUT` (default 5 s) in total, and is woken by a member
attaching or by another node announcing one. The wait costs no try, applies
with `retryPolicy: NONE`, and applies on a single node.

## 5. The answer limit has an upper bound

**Departure 8.** `responseTimeoutMs` on a processor, a criterion function or a
schedule function must be `0 ≤ value ≤ CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS`
(default 60000); import answers `400 VALIDATION_FAILED` otherwise. Absent or
`0` means `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS` (default 30000). A stored value
above a bound that was lowered later is not clamped: the callout fails. Tries
multiply the answer limit, and the bound keeps the product inside the one
ceiling a transaction has.

## 6. A compute member that was replaced is fenced

Giving up on a member does not stop it, and its callbacks would otherwise be
accepted for as long as the transaction is open — after its replacement has
answered, and after later processors of the same transition have run.

**Departure 9.**

- Each callout has a number that rises every time its work is given to another
  member. The transaction token is minted per try and carries the callout's id
  and that number; it lives for the try's answer limit plus an allowance.
- A callback is admitted only under the callout's current number and while the
  callout is in progress. Otherwise it is refused: `410 CALLOUT_SUPERSEDED`,
  not retryable, message `this compute node was replaced, or its callout has
  ended`. Once the transaction has ended the answer is
  `404 TRANSACTION_NOT_FOUND`, as before. The tenant is checked before the
  number, so a stolen token tells another tenant nothing.
- The check is repeated under the transaction's lock: when the callback takes
  it, each time it takes it back after a callout of its own, and between the
  processors of a workflow it started. A refused callback performs no store
  operation of any kind — no write, no read, no audit record.
- When the number rises, and when a callout ends, the platform takes the
  transaction's lock once before it proceeds: whatever the earlier member had in
  progress finishes first, and nothing of it can start afterwards.
- No database statement is interrupted — not by the fence, and not by the
  member's connection dropping.
- "Too slow" is not "replaced": a timeout that fails the operation rolls the
  transaction back and raises no number.

**Departure 10 — one transaction, one user at a time.** Every callback, a read
or a search as much as a write, holds its transaction's lock for the whole time
the platform works on it. The request is read in full before the lock is taken
and the response is sent after it is released, so a member that stalls holds
nothing. This extends `nested-join-tx-serialisation.md`: the lock is still
given up for the length of any callout the callback itself makes.

What is not stopped, stated plainly: when the tries are made by another node,
that node numbers them, and the owner learns of a later try from the first
callback that carries its number. Until then the earlier member of that
hand-over is still admitted. Its writes are repeats of an idempotent
processor's own, and the wait at the end of the callout still puts them before
anything the workflow does next.

## 7. What Cloud has to decide

Cloud's callbacks are separate transactions, so §6 has no direct counterpart
there; §1–§5 do. The visible contract to match is: the `idempotent` field and
its default; stop on `success: false` and pass the verdict on; the codes and
statuses of §2; no delay between tries; the wait of §4 outside the try count;
the upper bound of §5; `retryPolicy` on all three callout kinds.
````

- [ ] **Step 2: README row** — append to the table in `docs/cloud-parity/README.md`:

```markdown
| `callout-failover.md` | Trying another compute member when a callout is not delivered or not answered: the hand-off line and `idempotent` (default `false`); `success: false` stops and its `retryable` verdict reaches the client; `503 CALLOUT_FAILED`; no delay between tries; waiting for a member costs no try; tries are the normal number and time is the hard limit; the upper bound on `responseTimeoutMs`; `retryPolicy` on schedule functions; fencing of a replaced member (`410 CALLOUT_SUPERSEDED`) and one user of a transaction at a time |
```

- [ ] **Step 3: `scheduled-transitions.md` §10** — the table lacks
`CALLOUT_FAILED`, and the closing paragraph says "no engine-side retry loop is
implied" (`:273-277`).

Add the table row
`| Several tries of the callout failed | `CALLOUT_FAILED` |`; change "exactly
one retryable `503` outcome" → "one retryable `503` class"; "All four" → "All
five" (twice); and replace the sentence "the client's own retry is expected to
succeed once the compute-node infrastructure recovers — no engine-side retry
loop is implied." with:

```markdown
Before it fails, the callout has been tried on other matching compute members as `callout-failover.md` describes — a function is always safe to repeat, and `schedule.function.retryPolicy` selects the number of tries. A member that answers `success: false` is not part of this class: it fails the write as `400 WORKFLOW_FAILED`, carrying the member's message and verdict.
```

In "Background re-arm" change "hits one of the four conditions above" → "fails".

- [ ] **Step 4: `nested-join-tx-serialisation.md`** — after the first paragraph
of "Invariant Cloud must uphold" add:

```markdown
"Every joined callback" means every one: a read or a search holds the gate for its whole handling exactly as a write does, and the gate is the place where a callback from a compute member that was replaced is refused. See `callout-failover.md` §6.
```

- [ ] **Step 5: Verify**

```
grep -n '#[0-9]\{3\}' docs/cloud-parity/callout-failover.md                  → no hits
grep -n 'pnode\|cnode' docs/cloud-parity/callout-failover.md                 → no hits
grep -c 'Departure' docs/cloud-parity/callout-failover.md                    → 10
grep -n 'no engine-side retry' docs/cloud-parity/scheduled-transitions.md    → no hits
grep -c 'callout-failover.md' docs/cloud-parity/README.md                    → 1
```

Each status/code pair in §2 and §6 is checked against the running code, not the
spec: `grep -rn 'ErrCodeCalloutFailed\|ErrCodeCalloutSuperseded' internal/ --include='*.go' | grep -v _test`
and read the `Operational(` call beside each hit for its status. A pair that
differs from the file is corrected in the file and reported to the lead as a
spec deviation.

- [ ] **Step 6: Commit**

```
git add docs/cloud-parity/callout-failover.md docs/cloud-parity/README.md docs/cloud-parity/scheduled-transitions.md docs/cloud-parity/nested-join-tx-serialisation.md
git commit -m "docs(cloud-parity): callout failover as a contract Cloud can implement (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task D-6: `docs/ARCHITECTURE.md` — the callout path, audited as a whole, present tense

**Spec:** §4–§7, §9; §14. `ARCHITECTURE.md` is a reference, not a history: no
"previously", "now", "no longer"; a claim that cannot be pointed at in code is
deleted, not softened.

**Needs:** L (all), F (all), O (all), P (all), M-9, C-2, C-11 — this is the last
documentation task before D-9. M-9, C and L-3 edit other parts of the same file;
rebase on them.

**Files:**
- Modify: `docs/ARCHITECTURE.md` — package list (`:57-110`); §4.2 `Claims`
  (`:522-530`) and the dispatch-authentication paragraph (`:540-549`); §4.3
  whole (`:595-665`); §4.4 swimlane steps t3a–t3f and the key observations
  (`:686-712`); L7 partition analysis (`:857-867`); `:1153`; DD-2 (`:1756-1762`);
  DD-9 (`:1812-1818`); failure-mode row `:1899`; "Dispatch poll interval" row
  `:1929`

- [ ] **Step 0: prove every identifier the new text names**

```
grep -rn 'func (c \*Coordinator)' internal/callout/                       → DispatchProcessor, DispatchCriteria, DispatchFunction
grep -n  'func (d \*ProcessorDispatcher) RunLocal' internal/grpc/*.go       → 1 hit
grep -n  'type PeerRouter\|func (r \*PeerRouter) HandOver\|func (r \*PeerRouter) Peers' internal/cluster/dispatch/*.go → 3 hits
grep -n  'func (f \*Fence) \(Begin\|Advance\|Admit\)\|^func Check' internal/fence/*.go → 4 hits
grep -rn 'ClusterDispatcher\|findPeerWithPolling\|forwardWithFailover\|acquireJoinedGate' internal/ app/ --include='*.go' → no hits
grep -n  'Callout\|Major\|Minor\|Outer' internal/cluster/token/*.go         → the four claims
```

A name that is not found is replaced in the text below by the one that is; a
mechanism that is not found is left out and reported.

- [ ] **Step 1: package list** — add, in alphabetical place:

```
  callout/                The owner's loop: tries, hand-overs, patience, the callout deadline
  fence/                  Which compute member holds a callout's work; refuses the rest
```

and change `txgate/` to "Per-transaction lock; held by every joined request for
its whole handling".

- [ ] **Step 2: §4.2 `Claims`** — replace the struct with the one in
`internal/cluster/token` (copy it from the file; per spec §7 it is):

```go
type Claims struct {
    NodeID    string `json:"n"`   // ID of the node holding the transaction
    TxRef     string `json:"t"`   // UUID, key into the node's local tx map
    ExpiresAt int64  `json:"e"`   // Unix timestamp: the try's answer limit + CYODA_CALLOUT_PASS_ALLOWANCE
    Callout   string              // the callout's request id
    Major     uint32              // raised by the owner each time the work goes to another member or node
    Minor     uint32              // counted by a node that received a hand-over; 0 on the owner's own tries
    Outer     []Pair              // the (callout, major, minor) of every enclosing callout
}
```

(use the JSON tags the file has). After "The token is opaque to the client."
add: "A token is minted per try, by the node that makes the hand-off; `NodeID`
is always the owner's."

In the dispatch-authentication paragraph replace "`X-Dispatch-Timestamp` is
bound as associated data along with HTTP method and path" with the associated
data P implemented — per spec §6: a direction label (`request` / `response`),
the path, and for a response the request's nonce and timestamp — and add "The
response is sealed with the same key under a fresh nonce; it is bound to the
request it answers and does not enter the replay cache."

- [ ] **Step 3: §4.3 — replace `:595-665` whole**

````markdown
### 4.3 Compute Dispatch Routing

| Component | Type | Purpose |
|-----------|------|---------|
| Owner's loop | `callout.Coordinator` (implements `contract.ExternalProcessingService`) | Tries, hand-overs, waiting, the callout deadline. Built in both modes; its peer router is nil on a single node |
| Local procedure | `grpc.ProcessorDispatcher.RunLocal` | Tries a callout on this node's own matching members |
| Member selection | `grpc.MemberSelector` → `RoundRobinSelector` | Picks among the candidates not yet tried |
| Peers | `dispatch.PeerRouter` | Alive peers advertising the tag; one hand-over |
| Peer selection | `PeerSelector` → `RandomSelector` | Order in which peers are asked |
| Fencing | `fence.Fence` | One per process; decides which passes are current |

**A try** is one attempt to hand a callout's work to one member. **A hand-off** is `Member.Send` returning nil. Every failed try is classified (`contract.CalloutFailureKind`): `NoHandOff` — another member may always be tried; `NoAnswer` — only when the callout is repeat-safe (a criterion, a function, a processor with `idempotent: true`); `MemberFailed` and `Terminal` — never.

**The owner's loop:**

```
tries    = 1 (retryPolicy NONE) | 1 + CYODA_RETRY_FIXED_NUM_RETRIES
deadline = now + tries × answer limit + CYODA_DISPATCH_WAIT_TIMEOUT + CYODA_CALLOUT_HANDOVER_ALLOWANCE
fence.Begin(callout)                       # ended on every exit path
loop:
    take MemberRegistry.Changed() and NodeRegistry.Changed()
    RunLocal(call, triesLeft)              # raises the fencing number before each try
    ok → return · stop-kind → return · no tries left → CALLOUT_FAILED
    for each alive peer advertising the tag, not yet asked in this pass:
        raise the fencing number; PeerRouter.HandOver(peer, call, triesLeft)
        a peer that cannot be connected to, or hands off to nobody, uses no try
    nothing took the work → wait on either channel, within the patience, then a new pass
```

The patience (`CYODA_DISPATCH_WAIT_TIMEOUT`) is one allowance per callout, counted in elapsed waiting time; the wait is on the two change channels, never a timer loop. The deadline is a context derived with `context.WithDeadlineCause(…, contract.ErrCalloutDeadline)`, so a try cut off by it is `NoAnswer` while the caller's own context ending is returned unchanged. A lost hand-over answer counts as one try although the peer may have made more: the number of tries is the normal number, the deadline is the hard limit — 155 s at the defaults, 275 s with the answer limit at its upper bound, inside PostgreSQL's idle-in-transaction ceiling.

**The hand-over** — `POST /internal/dispatch/callout`, one route for every callout kind:

- AES-256-GCM AEAD envelope on both legs (§4.2), `Content-Type: application/cyoda-dispatch-v1`, 10 MB body limit.
- The request carries the entity payload and meta, the workflow and transition names, the caller's tenant, user, roles and principal kind, the required tags, the kind-specific member, and: `requestID`, `triesLeft`, `answerLimitMs`, `ownerNodeID`, `major`, `outer`, `repeatSafe`. The peer runs `RunLocal` with `triesLeft` and never hands on.
- A request carries two tenants — its own `TenantID`, which the reconstructed `UserContext` runs as, and `EntityMeta.TenantID`. They must agree, an absent one included; a mismatch is `400` naming neither value.
- The response states an `outcome` — `ok`, `no_handoff`, `no_answer`, `member_failed`, `terminal` — with `triesUsed`, one `attempts` entry per try, the member's own message and verdict for `member_failed`, and the kind-specific result.
- Only an authenticated `no_handoff`, or a connection that could not be opened (`CYODA_DISPATCH_CONNECT_TIMEOUT`), means nothing reached a member. Any other non-`ok` reading — a transport error after connecting, a non-2xx status, a truncated or unauthenticated body, a `triesUsed` out of range — is `no_answer` and counts one try.
- Every hand-over opens its own connection (`DisableKeepAlives`), with no proxy and no client-wide timeout; the owner's wait is a per-request deadline of `triesLeft × answer limit + CYODA_CALLOUT_HANDOVER_ALLOWANCE`, never past the callout's deadline.

**What the client sees:**

| Situation | Status, code |
|----------|------------|
| No member appeared within the patience | 503 `NO_COMPUTE_MEMBER_FOR_TAG` |
| One attempt on record | 503, that attempt's own code (`DISPATCH_TIMEOUT`, `COMPUTE_MEMBER_DISCONNECTED`, `DISPATCH_FORWARD_FAILED`) |
| Several attempts on record | 503 `CALLOUT_FAILED`, listing them |
| A member answered `success: false` | 400 `WORKFLOW_FAILED` with the member's message; retryable when the member said so |

**Fencing.** Each callout has a number `(major, minor)`. The owner raises `major` before each of its own tries and before each hand-over (`Fence.Advance`); a peer counts `minor` for the tries it makes. The pass of a try carries the pair, and the pairs of every enclosing callout. `txjoin.JoinFromToken` — the one function every callback passes on the owner — verifies the pass, joins the transaction (which checks the tenant), and then calls `Fence.Admit`; a pair that is not current is refused with `410 CALLOUT_SUPERSEDED`. The join layer then reads the whole request, takes the transaction's lock (`txgate`), runs `fence.Check` under it, and only then calls the handler; the response is sent after the lock is released. The check is repeated after every `txgate` resume and after each processor returns. `Advance`, and the end of a callout, take the transaction's lock once after shutting the earlier pass out, so nothing of the earlier member is in progress when the work goes on. The fence cancels no callback's context, and a joined request runs under `context.WithoutCancel`: no statement on the operation's connection is interrupted.
````

- [ ] **Step 4: §4.4 swimlane** — replace t3a–t3f with:

```
t3a  Node A: Coordinator begins the callout; RunLocal finds no local member
t3b  Node A: PeerRouter.Peers → Node B advertises the tag for this tenant
t3c  Node A: raises the fencing number; hands the callout over to Node B with the tries left
t3d  Node B: RunLocal picks a member, mints the try's pass {NodeID=A, TxRef=tx-123, callout, number}
t3e  Node B: sends the request over the member's gRPC stream (the hand-off)
```

renumber nothing else; in "Key observations" replace "Node A mints the tx-token
and embeds it" with "The node that makes the hand-off mints the pass — here Node
B — naming Node A as owner", and add: "Node A admits the callback at t8 only
under the callout's current number (§4.3 Fencing)."

- [ ] **Step 5: L7 partition analysis (`:857-867`)** — replace the three cases with:

```markdown
**Connection cannot be opened:** no try is used; Node A asks the next peer advertising the tag, or waits out the patience and fails with `NO_COMPUTE_MEMBER_FOR_TAG`.

**Hand-over sent, no answer, or the answer lost:** one try is counted. For a repeat-safe callout Node A carries on with the tries left; otherwise the operation fails with `DISPATCH_FORWARD_FAILED` and the transaction rolls back. Node B's member may still be working: its callbacks are refused from the moment Node A raises the fencing number or ends the callout.

SAFE: Every case ends in a rollback, or in a further try of a callout declared safe to repeat, with the earlier member fenced.
```

- [ ] **Step 6: the remaining sentences**

- `:1153` "In multi-node mode, this is the `ClusterDispatcher` (see Section 4.3)"
  → "This is the `callout.Coordinator` (see Section 4.3), in single-node and
  multi-node mode alike."
- DD-2 — the decision stands for *transactions between nodes*; its rationale
  claims fencing is unreachable, which is false for compute members. Append to
  the Rationale: "This is about two *nodes* holding one transaction. A compute
  member that was replaced is a different case — it can still send callbacks
  into an open transaction — and is fenced (§4.3, DD-9)."
- DD-9 — retitle "Event-Driven Wait for Missing Compute Members"; Decision:
  "A callout with nothing to try waits on `MemberRegistry.Changed()` and
  `NodeRegistry.Changed()` for up to `CYODA_DISPATCH_WAIT_TIMEOUT` (default 5s),
  one allowance per callout, on a single node as in a cluster." Rationale: keep
  the first two sentences; replace the 200ms sentence with "A change signal wakes
  the callout the moment a member attaches or a peer's list arrives, and costs
  nothing while nothing changes. Waiting is not a try."
- `:1899` Compute member disconnect — "Pending dispatch requests fail with
  'member disconnected.'" → "A callout in flight on the member is classified:
  given to another member when that is safe (no hand-off yet, or a repeat-safe
  callout), failed with `COMPUTE_MEMBER_DISCONNECTED` otherwise."; "dispatches
  fail after poll timeout" → "callouts fail once the patience is used up".
- `:1929` "Dispatch poll interval | 200 ms | Hardcoded" — delete the row.

- [ ] **Step 7: Verify**

```
grep -n 'ClusterDispatcher\|FindByTags\|findPeerWithPolling\|forwardWithFailover' docs/ARCHITECTURE.md → no hits
grep -n 'no server-side failover\|wait 200ms\|every 200ms\|Dispatch poll interval' docs/ARCHITECTURE.md → no hits
grep -n 'CYODA_TX_TOKEN_TTL' docs/ARCHITECTURE.md                                                     → no hits (C-11)
grep -n -i 'previously\|no longer\|used to\|until now' docs/ARCHITECTURE.md                           → none inside §4.2–§4.4, DD-2, DD-9
```

(`:499` "poll member count every 200ms" is the gossip join wait and stays — read
it to confirm it still describes `registry/gossip.go`.)
Read §4.2–§4.5 top to bottom once more for a sentence this task did not list
and the code no longer supports; fix it in the same commit.

- [ ] **Step 8: Commit**

```
git add docs/ARCHITECTURE.md
git commit -m "docs(architecture): the callout path — owner's loop, hand-over, fencing (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task D-7: three documents §14 does not list, each false once this lands

**Spec:** §7 ("One transaction, one user at a time"; "A savepoint that cannot be
created, undone or released"), §8.1; `documentation-hygiene.md`; Gate 6.

**Needs:** F (join-layer lock; savepoint sentinel), O.

**Files:**
- Modify: `docs/CONCURRENCY.md` §7 class 3 (`:243-262`)
- Modify: `docs/PROCESSOR_EXECUTION_MODES.md` — mode table `:42-45`, §3
  lifecycle `:144-152`, the callbacks paragraph `:172-178`, failure table
  `:277-284`, `:286`
- Modify: `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` `:402` (one line)

- [ ] **Step 1: `CONCURRENCY.md` §7 class 3** — the paragraph "In practice
cyoda's domain layer ensures this naturally … AND must update this section."
is false: joined callbacks are concurrent users of one transaction. Replace it
with:

```markdown
cyoda's domain layer serialises them with a per-transaction lock (`internal/txgate`). The operation that owns the transaction is one chain of calls. Every other user is a **joined request** — a callback from a compute member, carrying the transaction's pass — and the join layer (`txjoin`, reached from the HTTP join middleware and the gRPC routing interceptor) takes the lock for the request's whole handling: reads and searches as well as writes, entity and non-entity handlers alike. The request is read in full before the lock is taken and the response is sent after it is released. A holder gives the lock up for the length of any callout it makes (`txgate.Suspend`) and takes it back before it touches the transaction again. During a callout the transaction's users are therefore the callbacks of the member that currently has the work, one at a time; between callouts, the owner alone. `internal/fence` decides which member that is, and makes the owner wait for a joined request in progress before the work goes on. Any feature that fans work out across goroutines on one transaction must take the same lock, and must update this section.
```

In class 4 replace "no dispatch-ID stability across retries" with "no dispatch-id
stability across a *client's* re-run (the tries of one callout do share a
request id)".

- [ ] **Step 2: `PROCESSOR_EXECUTION_MODES.md`**

- Mode table, `SYNC` Failure cell: "fatal — `WORKFLOW_FAILED` 400, entity stays
  in source state" → "fatal — the callout's error (see
  `cyoda help workflows`), entity stays in source state"; the same in the
  `COMMIT_BEFORE_DISPATCH` row.
- §3 Lifecycle, item 3: append "A savepoint that cannot be created, undone or
  released is not a processor failure: it fails the operation with a ticketed
  `5xx` and nothing commits. A processor's compute member that was replaced is
  shut out before the savepoint is undone, so none of its writes lands after
  it." Re-point the `engine_processors.go` line references in that section at
  the lines found (`grep -n 'func (e \*Engine) executeAsyncNewTx' internal/domain/workflow/engine_processors.go`),
  or drop the line numbers and keep the function name.
- Failure table: row 1 → "Processor's member answers `success:false` |
  `T_post` rolled back, entity durable in pre-callout state,
  `400 WORKFLOW_FAILED` with the member's message"; add a row "No answer, or the
  member disconnects | another member is tried only if the processor is
  `idempotent`; otherwise `T_post` rolled back, `503` with the try's own code";
  delete the row "Calculation member disconnects mid-dispatch | `400
  WORKFLOW_FAILED`".
- `:286` "There is **no engine-side retry** and **no automatic compensation**."
  → "A callout that gets no answer is given to another compute member only when
  the processor is declared `idempotent`; there is **no automatic
  compensation**."

- [ ] **Step 3: `WORKFLOW_IMPORT_EXPORT_AUDIT.md:402`** — the document is a dated
audit and C-6 marks M1 resolved in its tables. Prefix the paragraph "**Current
state of cyoda-go.**" with "**At the audit date** (resolved since — see the M1
row): " and change nothing else in it.

- [ ] **Step 4: Verify**

```
grep -n 'ensures this naturally\|no production callsite' docs/CONCURRENCY.md      → no hits
grep -n 'no engine-side retry' docs/PROCESSOR_EXECUTION_MODES.md                  → no hits
grep -rn 'func Suspend\|func WithHeld' internal/txgate/                           → both exist
```

- [ ] **Step 5: Commit**

```
git add docs/CONCURRENCY.md docs/PROCESSOR_EXECUTION_MODES.md docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md
git commit -m "docs: joined requests are serialised in the join layer; execution modes under failover (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task D-8: stale comments

**Spec:** §14 "Stale comments".

**Needs:** for `arm.go`: the O task that adds the `MemberFailed` branch to
`classifyWorkflowError`, and F's `fence.Check` after `resume()` at this site.
For `errors.go`: nothing.

Already taken, verified in the finished sections: `members.go:48-55` by L-7
(exit check `grep -rn 'current dispatcher is single-shot' internal/`);
`validate.go:66-68` by C-6 (retry-policy comment `:56-74`).

**Files:**
- Modify: `internal/common/errors.go:16-22`
- Modify: `internal/domain/workflow/arm.go` (`armViaFunction`, the comment on
  the `derr` return — `:243` today)

- [ ] **Step 1: `errors.go`** — three doc comments stand over nothing
(`ErrNotFound` `:16`, `ErrEpochMismatch` `:18-20`, `ErrConflict` `:22`; the
sentinels live in `cyoda-go-spi`). Delete lines 16-23 so that the import block
is followed by one blank line and `// ErrorLevel classifies …`.

- [ ] **Step 2: `arm.go`** — the comment "already a classified AppError (503) —
fails the write, fail-closed" is wrong once a member's own failure travels as a
`*contract.CalloutFailure` (400, possibly retryable). Replace the line with:

```go
		// Fails the write, fail-closed. derr is either a classified AppError
		// (503: nobody could be reached or nobody answered) or a
		// *contract.CalloutFailure of kind MemberFailed, which
		// classifyWorkflowError turns into 400 WORKFLOW_FAILED with the
		// compute member's verdict.
		return nil, nil, derr
```

- [ ] **Step 3: Verify**

Run: `go build ./... && go vet ./internal/common/... ./internal/domain/workflow/...`
then `go test ./internal/common/... ./internal/domain/workflow/...`   Expected: PASS
(no behaviour changes; TDD waiver: comments only).

```
grep -n 'ErrEpochMismatch is a sentinel\|ErrNotFound is a sentinel\|ErrConflict is a sentinel' internal/common/errors.go → no hits
grep -n 'already a classified AppError (503)' internal/domain/workflow/arm.go                                          → no hits
```

- [ ] **Step 4: Commit**

```
git add internal/common/errors.go internal/domain/workflow/arm.go
git commit -m "chore: remove three orphaned doc comments; say what an arming callout can fail with (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task D-9 (LAST): `CHANGELOG.md` `[Unreleased]` — one section, read as one

**Spec:** §14, §15.

**Needs:** every other task of every stream, D-5 included (the entries cite
`docs/cloud-parity/callout-failover.md`).

**Files:**
- Modify: `CHANGELOG.md` `[Unreleased]` (`### Breaking` `:7`, `### Added` `:50`,
  `### Changed` `:77`, `### Fixed` `:108` today)

House style (read `:9-48`): an entry leads with the new contract in bold, says
what it replaces, and closes by naming the `docs/cloud-parity/*.md` file. No
issue numbers.

Fragments already planned by other streams — **find each, and merge it as
stated; none is left standing beside its replacement:**

| Fragment | From | Decision |
|---|---|---|
| Added — "Callout settings." | C-2 | kept as written; moved directly under the new "Callout failover" entry |
| Added — "Workflow schema 1.5." | C-8 | kept |
| Breaking — "Workflow import refuses two things it used to accept" | C-8 | kept |
| Breaking — "`CYODA_TX_TOKEN_TTL` is removed." | C-11 | kept; placed after "Transaction tokens are minted per try" below |
| Changed — "A callout picks among a tenant's matching compute members round robin." | L-3 | kept |
| Changed — "A compute member's own failure message reaches the client without the inner `processor dispatch failed:` segment." | L-7 | **folded** into the Changed entry "A compute member's failure reaches the client whole" below, and deleted |
| Fixed — "A node hosting compute nodes of more than a handful of tenants vanished from the cluster." | M-9 | kept; "compute nodes" → "compute members" |
| Fixed — "A transaction routed to a node whose membership metadata cannot be read answers `503`" | M-9 | kept |

Any fragment F, O or P added is treated the same way: kept if it says something
the entries below do not, folded and deleted if it repeats one.

- [ ] **Step 1: `### Breaking`** — add, after C-8's entry:

```markdown
- **A callout with no compute member waits before it fails, on a single node
  too.** `CYODA_DISPATCH_WAIT_TIMEOUT` (default `5s`) was a cluster-only poll;
  it is the one way a callout waits for a member to exist, in every mode and
  whatever the `retryPolicy`. A single node with no member attached answered
  `503 NO_COMPUTE_MEMBER_FOR_TAG` at once and answers it after five seconds.
  Set it to `0` for the old behaviour. See `docs/cloud-parity/callout-failover.md`.

- **A callback is refused once its compute member was replaced or its callout
  has ended: `410 CALLOUT_SUPERSEDED`.** It was accepted for as long as the
  transaction stayed open. After the transaction has ended the answer is still
  `404 TRANSACTION_NOT_FOUND`. See `docs/cloud-parity/callout-failover.md`.

- **Transaction tokens are minted per try and name their callout.** A token
  minted by an earlier version is refused with `401 UNAUTHORIZED`; a cluster of
  mixed versions is not supported.

- **`CYODA_DISPATCH_FORWARD_TIMEOUT` no longer governs callouts handed to
  another node.** It keeps its name and meaning for the node-to-node call that
  delegates a scheduled transition. A hand-over waits
  `tries left × answer limit + CYODA_CALLOUT_HANDOVER_ALLOWANCE`, and opening
  its connection is bounded by `CYODA_DISPATCH_CONNECT_TIMEOUT`.

- **Callbacks of one transaction are served one at a time, and are not
  abandoned when the compute member disconnects.** Two parallel callbacks used
  to run at once; a deadline the member sets on its own callback is not applied.
```

then C-11's entry.

- [ ] **Step 2: `### Added`** — first entry of the section:

```markdown
- **Callout failover: a processor, criterion or function request that is not
  delivered, or not answered, is given to another compute member.** The line is
  the hand-off. Work that never reached a member is always tried elsewhere.
  Work that did, and then got no answer or lost its connection, is tried
  elsewhere only when a repeat is safe: every criterion, every function, and a
  processor whose `config` declares the new **`idempotent: true`** (default
  `false`) — the author's statement that running it more than once, possibly at
  the same time on two members, equals running it once, in cyoda and in every
  system it touches. A member that answers `success: false` ends the callout.
  `retryPolicy` selects the number of tries (`NONE`: one; `FIXED` or unset: one
  plus `CYODA_RETRY_FIXED_NUM_RETRIES`, default `3`) and is honoured on
  `schedule.function` too. In a cluster the node holding the transaction tries
  its own members and then hands the callout, with the tries left, to a peer;
  the number of tries is the normal number, while the time — `tries × answer
  limit + CYODA_DISPATCH_WAIT_TIMEOUT + CYODA_CALLOUT_HANDOVER_ALLOWANCE`,
  155 s at the defaults — is a hard limit. Every try carries the same
  `requestId`. When more than one try failed the client gets the new
  **`503 CALLOUT_FAILED`**, listing them. What a processor does outside cyoda
  is not undone by a rollback and is repeated by a repeat; that stays the
  application's to design for. See `cyoda help workflows`,
  `cyoda help cluster`, and `docs/cloud-parity/callout-failover.md`.
```

followed by C-2's "Callout settings." and C-8's "Workflow schema 1.5.", then
(if O's task did not add it) one line for the callout counters, pointing at
`cyoda help telemetry`.

- [ ] **Step 3: `### Changed`** — after L-3's entry, replacing L-7's:

```markdown
- **A compute member's failure reaches the client whole.** `400 WORKFLOW_FAILED`
  reads `processor <name> failed: <the member's message>` (for a criterion,
  `failed to evaluate transition criterion: <the member's message>`) — also when
  the member was attached to another node, where the message used to be lost —
  and carries `retryable: true` when the member's `error.retryable` said so. The
  verdict was read and dropped. See `docs/cloud-parity/callout-failover.md`.

- **The answer limit is configuration, with an upper bound.**
  `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS` (default `30000`) replaces a constant;
  `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` (default `60000`) bounds
  `responseTimeoutMs`. A stored workflow whose value exceeds a bound lowered
  later is not clamped: its callout fails, naming the setting.
```

- [ ] **Step 4: `### Fixed`** — before M-9's two entries:

```markdown
- **A compute member that had been given up on could still write into the
  transaction — last.** Its callbacks were accepted until the transaction
  closed, so the write of a slow member could land after its replacement had
  answered and later processors had run, and in `ASYNC_NEW_TX` the write of a
  processor that *failed* could land after its savepoint was undone, and be
  committed. A replaced member is now fenced, and the engine waits for a
  callback in progress before it carries on.

- **Two callbacks at once on one transaction crashed the node, or failed the
  operation.** Only entity writes were serialised. On the memory and SQLite
  backends two parallel reads by one compute member were enough to stop the
  process for every tenant (`concurrent map writes`); on PostgreSQL the second
  user of the operation's connection failed with `conn busy`. Every joined
  request now takes the transaction's lock in the join layer — with no change
  to any storage plugin — reads its body before it and sends its response after.

- **A compute member that disconnected in the middle of a callback destroyed
  the operation's PostgreSQL connection.** The cancelled statement made the
  driver close the connection the owning operation was running on. A joined
  request is no longer cancelled by its client going away.

- **A savepoint failure in `ASYNC_NEW_TX` passed as a processor failure.** A
  savepoint that could not be undone was a warning — the failed processor's
  writes were committed; one that could not be created or released was counted
  as the processor's own, non-fatal failure. Each now fails the operation with
  a ticketed `5xx`, and the PostgreSQL plugin's savepoint errors are classified
  like its other statements instead of reaching a `400` body raw.

- **A callout whose transaction token could not be minted was sent without
  one**, so the member's callbacks would have run outside the transaction. It
  now fails before anything is sent.

- **A callout handed to another node and broken off mid-way was tried on the
  next node regardless**, which for a processor could run it twice. A lost
  answer now counts as a try and is followed by another only when a repeat is
  safe.
```

- [ ] **Step 5: Verify**

```
grep -n '#[0-9]\{3\}' CHANGELOG.md | sed -n '1,5p'          → none between "## [Unreleased]" and "## [0.8.4]" (older releases are history)
awk '/^## \[Unreleased\]/,/^## \[0.8.4\]/' CHANGELOG.md | grep -c 'processor dispatch failed'   → 1 (inside the merged Changed entry only, or 0 if reworded)
awk '/^## \[Unreleased\]/,/^## \[0.8.4\]/' CHANGELOG.md | grep -c 'callout-failover.md'         → ≥ 4
awk '/^## \[Unreleased\]/,/^## \[0.8.4\]/' CHANGELOG.md | grep -n 'cnode\|pnode'                → no hits
```

Check §15's five `### Breaking` items one by one against the section: the wait
on a single node; `CYODA_TX_TOKEN_TTL` removed (C-11); `CYODA_DISPATCH_FORWARD_TIMEOUT`;
earlier passes refused; a joined request after its callout ended. Check §14's
five Fixed items the same way. Read the whole `[Unreleased]` section once, top
to bottom, for two entries that say one thing.

- [ ] **Step 6: Commit**

```
git add CHANGELOG.md
git commit -m "docs(changelog): callout failover — one Unreleased section (#254)" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## Coverage carried forward (§13)

No §13 row belongs to this stream: documentation carries no scenario. The one
row a D task leans on is "Each new or newly validated setting" (C-1, C-2), whose
names D-1…D-4 quote; D-1 step 8 greps the registry for them.

Existing tests that pin what these tasks change: none pins wording. The guards
listed at the top pin structure (`see_also` resolution, invocation spelling,
issue ids, event-type catalogue) and are run by every help task.

## Stream interface summary

**D produces** nothing other streams compile against. It fixes three places
other streams write into:

- F's sentence on a `503` / dropped connection for a proxied callback goes
  directly after the paragraph D-3 step 4 adds to `cluster.md`, COMPUTE CALLBACK
  TRANSACTION ROUTING.
- O's `errors.md` index and five revised error topics should use "compute
  member" / "node", and the phrase "speaks for cyoda's state only" for
  `retryable` on a no-answer failure, which D-1's mode table defines.
- F, O and P add **no** `ARCHITECTURE.md` text and **no** `[Unreleased]` prose
  beyond a one-line fragment; D-6 and D-9 write both.

**D consumes** (all must exist before the named task; none by signature, all by
grep in the task's Verify/Step 0):

- C: setting names as registered in `cmd/cyoda/help/config_registry.go` (C-1,
  C-2); C's edits to `workflows.md`, `search.md`, `config/cluster.md`,
  `ARCHITECTURE.md` settings tables, CHANGELOG fragments.
- L: L-3 and L-8's `grpc.md` paragraphs; L-7's `members.go` comment and
  CHANGELOG fragment; `contract.CalloutFailure`, `RunLocal`, `MemberSelector`,
  `Candidates`, `Changed` (named in D-6).
- M: M-9's `cluster.md`, `ARCHITECTURE.md` and CHANGELOG edits;
  `NodeRegistry.Changed()`.
- F: `errors/CALLOUT_SUPERSEDED.md` and `ErrCodeCalloutSuperseded`;
  `internal/fence` (`Begin`, `Advance`, `Admit`, `Check`); `token.Claims`'
  four new fields; the join-layer lock and `context.WithoutCancel`; the
  savepoint sentinel.
- O: `errors/CALLOUT_FAILED.md` and `ErrCodeCalloutFailed`;
  `internal/callout.Coordinator`; the `MemberFailed` branch in
  `classifyWorkflowError`; deletion of `ClusterDispatcher`'s three methods.
- P: `dispatch.PeerRouter` (`Peers`, `HandOver`); response protection; the
  authenticated `no_handoff` for a replay; `DisableKeepAlives`; the connect
  timeout on the dialer.

## Open points

1. **§14 misses four documents that become false.**
   `docs/CONCURRENCY.md:254-262` says the domain layer serialises per-tx use
   "naturally" and that nothing in production calls `Join` — untrue today and
   the very defect §7 closes; it also says "must update this section".
   `docs/PROCESSOR_EXECUTION_MODES.md:279, 284, 286` ("no engine-side retry";
   a disconnect is `400 WORKFLOW_FAILED`). `docs/cloud-parity/nested-join-tx-serialisation.md`
   (the gate contract Cloud was given covers writes only).
   `docs/cyoda/schema/common/BaseEvent.json:30`. Planned as D-7, D-5 step 4,
   D-4 step 2.
2. **`BaseEvent.json`'s `retryable` description contradicts D1/D12** — "whether
   cyoda should retry the calculation request". The schema tree mirrors Cloud's,
   where that sentence is true. D-4 rewrites the description (no generated code
   carries it). If the lead prefers the shared schema text untouched, drop D-4
   step 2; `callout-failover.md` Departure 2 then carries the point alone.
3. **`errors.md:106` lists `TRANSACTION_EXPIRED` as `400`; the code answers
   `410`** (`proxy/http.go:87-92`, `txjoin.go:46`, `txroute_interceptor.go:106`;
   the topic file and `ARCHITECTURE.md:572` say 410). The index is O's; O should
   fix the row while it is in the file. `cluster.md:54` has the same error
   (`400 Bad Request` / `BAD_REQUEST`) and D-3 fixes it.
4. **`cluster.md:50` is false today**, independent of this work: no token is
   issued "when a node begins a transaction" or "returned to the client"; tokens
   are minted only for a callout (`Signer.Issue` has two production callers).
   D-3 step 1 rewrites it because the section is touched.
5. **`workflows.md:162, 165` say a timeout is `400 WORKFLOW_FAILED`.** It is
   `503 DISPATCH_TIMEOUT` today (R§4.3). D-1 step 4 corrects both.
6. **Spec §8.2 gives `CALLOUT_SUPERSEDED` as 410 and leaves the pass-less /
   earlier-version pass at `401 UNAUTHORIZED`.** D-9's Breaking entry "a token
   minted by an earlier version is refused with `401`" follows that row; if F
   maps a pass without callout and number differently, the entry follows F.
7. **Ordering hazard for `see_also`.** `TestSeeAlsoResolution` fails the build
   if D-1/D-2 land before F's and O's topic files. Each task's **Needs** line
   says so; if the lead wants the help prose earlier, land it without the two
   `see_also` entries and add them in D-9's commit.
8. **DD-2 ("Fencing Tokens Not Required")** stays true for its subject (two
   nodes, one transaction) but reads as a contradiction beside `internal/fence`.
   D-6 adds one clarifying sentence rather than rewriting a recorded decision;
   say if a new DD ("Fencing a replaced compute member") is wanted instead.
9. **`telemetry.md`'s histogram buckets** (§14: "stop at 10 s, below one answer
   limit") is a code change to the instrument's boundaries, not prose; it is
   assumed to be O's with the counters. Nothing in D touches it.
