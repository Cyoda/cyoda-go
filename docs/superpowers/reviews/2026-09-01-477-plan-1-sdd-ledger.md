# SDD ledger — plan: docs/superpowers/plans/2026-09-01-477-plan-1-plugin-internals.md

Spec: docs/superpowers/specs/2026-09-01-477-no-search-path-materialises-design.md (binding authority).
Branch: fix/477-search-fallback (worktree .claude/worktrees/fix+477-search-fallback). Start HEAD: fdc3d5b.

## Pre-flight scan

| Pair / task | Produces vs consumes | Finding |
|---|---|---|
| T1 ↔ T3/T4/T5 | T1 makes readDB snapshot reads at SnapshotTime safe; T3–T5 consume that guarantee | consistent; T1 must land first (it does) |
| T2 ↔ T5 ↔ T6 (sqlite entity_store.go) | disjoint functions (Save/CAS; Count/CountByState/GetAll/getAllTx; DeleteAll) | no overlap |
| T3 → T4, T5 | `openTxOverlay(ctx, tx, modelRef, filter, proj)` + `txOverlay.pull/Close` | identical signature in all three |
| T3 test helpers → T4/T5/T6 tests | `seedN`, `iterable`, `drainIDs`, `timeoutCh` in `tx_overlay_test.go` (package sqlite_test); tenant "tenant-ovl" | consistent; T2 uses its own `seedOne`/"tenant-dtw" |
| T3 getAllTx | T3 keeps getAllTx callers (GetAll, counts) until T5 deletes it | consistent |
| T4 test text | test body asserts `wantRecorded` excluding buffered ids, then a note says assert `full` | **contradiction inside T4** — see Ruling 1 |
| T8 → T9/T10/T11 (memory) | T8 changes `buildSnapshot` to `(snapshot, bufferedIDs, err)`; T9–T11 use `getAllSnapshotPointersUnlocked` directly | no overlap |
| T9 | `getAllSnapshotUnlocked` keeps `GetAll` as its only caller after T11 | consistent (removed in Plan 3) |
| T5/T6/T9/T10/T11 tests "pass today" | rubric may flag tests that cannot fail | see Ruling 2 |
| T3 projection | `projectIDState` skips `preparedPostFilter`; only `countTx` uses it, always with a zero filter | consistent |
| T1 test | uses `m.factory.clock` — field exists (`Commit` uses it) | ok |

Ruling 1: T4's read-set assertion is `full` (every entity on the returned page recorded, buffered own-writes included) — the SPI `GetPage` doc says "every entity on the returned page is recorded" and today's `getPageTx` does so; changing it would be a contract change outside this PR. Cost if wrong: an in-tx list over-records its own writes (harmless: they are already in the write-set).
Ruling 2: tests that already pass at their RED step are behavioural pins for changes whose property (no payload copy) is not observable; spec §10 waives the driving test for that property and forbids test hooks. They stay. Cost if wrong: a rewrite that breaks behaviour is caught; one that keeps behaviour but still copies is not — accepted by the spec.

## Task log
Task 1: implemented db493e1; review: spec ✅, 1 Important (plan-mandated: test's bare `commitMu.Lock()` at txmanager_begin_gate_internal_test.go:12 violates go-mutex-discipline).
Task 1: Ruling: the mutex rule binds test code as written ("**/*.go"); the brief's test text is corrected, not the rule — wrap the hold in an IIFE with `defer Unlock` (t.Fatalf's Goexit still runs the defer). Cost if wrong: none functional (branches were mutually exclusive).
Task 1: fix round 1/5 (1 addressed, 0 open — test lock hold in IIFE; commits db493e1..cf0cab8)
Task 1: complete (commits fdc3d5b..cf0cab8, review clean)
Brief corrections to carry into dispatches (SPI names verified): equality op is `spi.FilterEq` (not FilterEquals); `ChangeType` is a plain string "DELETED" (no constant); `GetVersionMetadata(ctx, id, spi.VersionMetadataOptions{})`; `GroupedAggregate(ctx, model, groupBy []spi.GroupExpr, filter, spi.GroupedAggregationsOptions{MaxBuckets: n})` with `spi.GroupExpr{Kind: spi.GroupExprState}`; `tx.Closed` exists.
Task 2: minor (deferred): unstageDelete's comment cites a savepoint-restore rationale that does not hold (RollbackToSavepoint replaces both maps wholesale); reword to "keeps the invariant Delete/DeleteAll already establish" — entity_store.go:138-141.
Task 2: complete (commits cf0cab8..a6789ef, review clean)
Task 3: implemented 4006087 (DONE_WITH_CONCERNS). Concern 1 (yields-only recording) is spec §4.4 as intended. Concern 3: a meta `state` filter literal needs `Declared: []spi.DataType{spi.String}` or spi.Prepare rejects it — carry into Task 8's test dispatch. Concern 4 minor (deferred): sortEntitiesByOrder wraps ctx errors as "Search: %w", so Iterate reports "Iterate: Search: context canceled" (searcher.go, outside task). Concern 2 (refuses projectIDState+residual; ctx.Err() in Next) → reviewer judges.
Task 3: review: spec ❌ (buffer-match loop lacks the amortised ctx check the reference shape carries), 3 Important.
Task 3: Ruling: Important "ReadSet written under RLock" is PARKED — the SPI's TransactionState doc makes within-class serialisation the application's responsibility ("two RLock-holding ops on the same tx concurrently … the application must serialise"); `record` follows the package convention (Get/GetAsAt/GetAll do the same). Cost if wrong: a concurrent-map-writes fatal under an application contract violation the SPI already disclaims.
Task 3: Ruling: the `projectIDState + residual` refusal branch is unreachable (Task 5's only caller passes a zero filter) — delete it and state the precondition in openTxOverlay's doc (rule: unreachable branches are deleted, not guarded). The projection SQL itself gets a white-box test now rather than first executing in Task 5.
Task 3: Ruling: the RolledBack/Closed check runs on every yield regardless of TrackingRead (state validity is not read-set bookkeeping); a rolled-back tx ends iteration with ErrTxRolledBack.
Task 3: Ruling: overlay-internal errors use a neutral "tx overlay: " prefix; each consumer (Iterate, later GetPage/counts) wraps with its own operation name. Cost if wrong: an error string reads slightly differently.
Task 3: fix round 1/5 (5 findings dispatched: ctx check in buffer loop, state check every yield, refusal branch deleted + projection tested, model-scoped bufferedIDs, comment/prefix/assert fixes; commits 4006087..aaecc3c) — re-review pending
Task 3: fix round 1/5 result: 5 addressed, 0 open (commits 4006087..aaecc3c)
Task 3: complete (commits a6789ef..aaecc3c, 1 parked: ReadSet-under-RLock — SPI-disclaimed application contract)
Task 4: minor (deferred): pull-error-mid-discard path and explicit empty-model case untested (no fault-injection convention in the package).
Task 4: complete (commits aaecc3c..785bef5, review clean)
Task 5: minor (deferred): the tx.Closed checks in Count/CountByState came from the controller's dispatch resolution, not the brief text — final PR description should note the overlay precondition as the reason.
Task 5: complete (commits 785bef5..c24faae, review clean)
Task 6: minor (deferred): a row-scan error mid-loop leaves tx maps partially staged (pre-existing shape, brief-mandated).
Task 6: complete (commits c24faae..b4cabd0, review clean)
Task 7: minor (deferred): no test for delete-then-CAS with a wrong expected ID (same branch by construction); memory's unstageDelete comment is terser than sqlite's (sqlite's own rationale was found inaccurate in Task 2 — align both to "keeps the invariant Delete/DeleteAll establish").
Task 7: complete (commits b4cabd0..7c5f776, review clean)
Task 8: Ruling (carried into dispatch): memoryIter checks RolledBack/Closed on every yield regardless of TrackingRead, mirroring sqlite's checkAndRecord (Task 3 ruling); a rollback-while-open test is added. Cost if wrong: one short RLock per yield on the memory backend.
Task 8: dispatched on opus, base 7c5f776.
Task 8: implemented a6b1d8e; review: spec ❌ (PIT + ambient tx: memoryIter gets `tx` set even for a point-in-time read, so it records historical ids / aborts on tx close; sqlite routes PIT away from the tx iterator), 1 Important (plan-mandated).
Task 8: Ruling: a point-in-time iteration is committed-only and ignores the ambient transaction on every backend (SPI PIT rule) — `memoryIter.tx` is set only when `opts.PointInTime == nil`; add a test: in-tx PIT iterate with TrackingRead records nothing and survives a Rollback. Cost if wrong: none (matches sqlite and the SPI contract).
Task 8: fix round 1/5 (1 finding dispatched: PIT iteration carries no tx; commits a6b1d8e..70084a4) — re-review pending
Task 8: fix round 1/5 result: 1 addressed, 0 open (commits a6b1d8e..70084a4)
Task 8: minor (deferred): none beyond the comment added in the fix.
Task 8: complete (commits 7c5f776..70084a4, review clean)
Task 9: implemented 9f8636a (reused existing currentStatePointersUnlocked at entity_store.go:892 instead of the brief's duplicate); review pending.
Task 9: minor (deferred): alias test never mutates a returned buffered-add result in the RYW case; matchSortBounded copies the over-limit entity before erroring (brief-literal).
Task 9: complete (commits 70084a4..9f8636a, review clean)
Task 10: implemented 223431b; review pending.
Task 10: minor (deferred): countTx's tx.Buffer loop lacks the amortised ctx.Err() check the file's sibling loops carry (brief-literal; buffer is tx-bounded).
Task 10: complete (commits 9f8636a..223431b, review clean)
Task 11: implemented 9a718e9; review pending.
Task 11: complete (commits 223431b..9a718e9, review clean)
Ruling: the feature branch is pushed to origin before Task 12 because scripts/repin-plugins.sh resolves plugin pseudo-versions from the remote; a new feature-branch push is a routine step toward the plan's PR, not a shared-branch push.
Task 12: implemented 4404089 (docs + repin; make test-full green: root 10171/0 failed, memory 434, sqlite 598, postgres 251; make race green 9296/0); review pending.
Task 12: minor (deferred): crud.md help bullets for memory+sqlite merged into one bullet rather than replaced one-for-one (accurate, linter-clean).
Task 12: complete (commits 9a718e9..4404089, review clean)
All 12 tasks complete. Final whole-branch review: base 134bcaa (merge-base with origin/release/v0.8.4), head 4404089.
Final review (opus): 0 critical, 3 important, 10 minor; verdict "with fixes".
Ruling F1: Save-then-CompareAndSave conflicts on memory and sqlite (a buffered own-write supersedes the caller's expected ID; postgres already conflicts because its CAS reads the tx connection). Spec §4.2 asked for tx.Buffer too; Task 2/7 implemented only the Deletes half. Fix now; PR 2's spitest gains Transaction/SaveThenCompareAndSave. Cost if wrong: none — matches postgres.
Ruling F2: the tx.Closed guard set is identical on memory and sqlite: memory gains it on GetAll, GetPage and buildSnapshot's in-tx branch; Search stays unchecked on both (already consistent). Cost if wrong: an extra error on a closed tx.
Ruling F3: the Begin gate must also cover sqlite's non-transactional writes (saveDirectly / direct Delete / direct DeleteAll stamp submit_time and commit their own sqlTx without commitMu, so a readDB snapshot read can miss a row with submit_time <= SnapshotTime for the window between stamp and commit). Close it: those paths hold commitMu from stamping through sqlTx.Commit (they are already serialised on the single writer connection, so the cost is Begin waiting behind a direct write). Correct the two comments to state the full guarantee. Strengthen the gate test with a real concurrent commit. Memory needs nothing (publish is atomic under entityMu). Cost if wrong: Begin latency behind direct writes.
Minors folded into the fix wave: T2 comment alignment (both plugins); countTx same-model buffer skip; ctx into getAllSnapshotPointersUnlocked; wrap bare ErrConflict with op + id (both plugins); CHANGELOG memory bullet names counts/DeleteAll; readDB pool-sizing comment. Left as-is: `pf, _ := spi.Prepare` (two-hop safety argument, documented), Begin ctx-awareness, T3 "Iterate: Search:" prefix, T9/T10 test breadth.
Parked (kept): T3 ReadSet-under-RLock — raise in PR 3 alongside TransactionManager.Join's doc.
Fix wave: commits 4404089..8244771 (816c8a8 F1, eb36d46 F2, a4093b4 F3, 8244771 engine test). make test-full green (root 10171/0, memory 444/0, sqlite 601/0, postgres 251/0); make race green.
Ruling: TestEngine_CommitBeforeDispatch_TrueBranch_DoubleWriteIsLastWriterWins pinned a memory/sqlite-only outcome (a processor writing the entity it processes into TX_post, then the engine's CompareAndSave silently winning); postgres always conflicted. The uniform outcome is the conflict; the test now pins it and the CHANGELOG records the memory/sqlite behaviour change. Cost if wrong: a workflow whose processor performs the documented "must not" double write now sees a conflict on memory/sqlite as it always did on postgres.
Ruling: F3 has no RED test — the window between a direct write's submit_time stamp and its commit is unreproducible without a production hook (forbidden); the two pins and the reasoning in the comments stand. Non-tx DeleteAll gates per Delete (non-reentrant mutex); the invariant is per row.
Fix wave re-review: all findings addressed, no new Critical/Important breakage.
Parked — Ruling: the Begin/tx_overlay comments say "submit_time <= SnapshotTime implies visible on every connection"; strictly, a direct write stamps clock.Now() without Commit's monotonic floor, so when lastSubmitTime has run ahead of the wall clock a direct write can stamp below an already-open snapshot. Pre-existing hole, not introduced here; the commitMu hold closes the window F3 named. Surface to Paul as a follow-up (direct writes should stamp under the same monotonic floor); reword the comment then. Cost if wrong: a stale read inside a microsecond-scale clock-skew window.
Parked — Ruling: non-tx DeleteAll drains its id cursor before calling Delete (which now takes commitMu); the comment does not name that ordering as load-bearing for deadlock freedom. Comment-only; surface to Paul.
Observation: the engine behaviour change (double-write processor now conflicts on memory/sqlite) sits under CHANGELOG "Fixed" beside its sibling; a Breaking cross-reference is defensible at release prep.
Security review: 0 critical, 0 high, 4 medium (report: security-review.md).
Ruling M1 (Begin blocks on commitMu without ctx): parked — spec §10 accepts Begin latency; a ctx-aware gate is a follow-up. Cost if wrong: a stuck direct write stalls new transactions until it completes.
Ruling M2 (in-tx iterator pins a readDB connection): parked — both in-tx consumers drain without nested readDB reads; the store_factory comment states the rule; enforcement is a follow-up.
Ruling M3 (Save/CompareAndSave/Delete/DeleteAll on a committed tx return success and are discarded): FIX NOW — fail-open on an integrity path; add the Closed → ErrTxAlreadyCommitted guard to the write paths on both plugins with tests. spitest has OpAfterRollback but no OpAfterCommit; PR 2 adds TxStateErrors/OpAfterCommit.
Ruling M4 (ReadSet under RLock, "remotely reachable"): parked as before — the engine serialises joined operations per transaction through txgate (h.gate.Acquire(txID) at every joined write/read site), so the SPI's application-side serialisation contract is met; the reviewer's premise that txjoin admits unserialised concurrent requests is not borne out. Raise in PR 3 with the Join doc.
Security fix: d251e83 (Save/CompareAndSave/Delete refuse a committed tx on both plugins; DeleteAll already did), e7c6e2c (CHANGELOG); re-pin 6246eab. Final make test-full + make race running; re-review of d251e83..e7c6e2c pending.
Security-fix re-review: addressed, no new breakage.
Parked — Ruling: Get/GetAsAt/Exists carry no tx.Closed guard (pre-existing, both plugins). After a commit the buffer content equals what was flushed, so their answers are correct; GetAsAt is committed-only by contract. Guard-set uniformity for reads is a follow-up (PR 2's spitest OpAfterCommit case can cover it). Cost if wrong: none observable.
Final verification at 6246eab: make test-full green (root 10171/0, memory 449/0, sqlite 606/0, postgres 251/0), make race green (9296/0).
