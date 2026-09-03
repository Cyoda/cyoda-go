# Direct search has one path — Cloud twin-alignment spec

cyoda-go defines the contract; Cloud mirrors it.

## Behaviour

A synchronous search is translated to a backend predicate and pushed to
`EntityStore.Search`. There is no second path: the whole-model read that once
backed an in-process match is gone from the SPI, and with it the in-memory
fallback the engine reached for when a condition would not translate or a
store lacked a capability.

Two request shapes that the fallback used to absorb are now answered:

- **A condition that cannot be translated** is rejected with `400
  INVALID_CONDITION`, or `400 INVALID_FIELD_PATH` when the failure is
  path-shaped. It is not reachable from validated input — the boundary
  grammar and the translator share the same path parser and operator set
  (`path-grammar.md` §11) — so no valid request changes status.
- **A nil/absent condition** is `400 INVALID_CONDITION`, not "match
  everything". A caller that wants every entity of a model pages it.

Asynchronous search translates the condition at submission, so a job is never
persisted for a condition no backend can execute; the executor streams through
`Iterate` and has no fallback branch of its own.

The same rule holds on the other two endpoints that carry a condition.
Conditional `DELETE /entity/{entityName}/{modelVersion}` and
`POST /entity/stats/{entityName}/{modelVersion}/query` each ran the same
`ValidateCondition` → `ConditionToFilter` pair and each kept a fallback of its
own: a zero-value filter passed to the store, with the predicate re-applied
per yielded entity in the engine. That is the whole-model scan under another
name, and it made one malformed condition answer three ways — a `400` from
search, a served result from delete, a served result from grouped statistics.
All three refuse it now.

## Invariant Cloud must mirror

1. A synchronous search reaches storage through exactly one call. No
   whole-model read exists to fall back to, in a transaction or out of one.
2. A condition that cannot be translated is a `400`, never a request served by
   a different mechanism with a different cost and a different read-set. This
   holds identically on direct search, async submit, conditional delete and
   grouped statistics — one condition grammar, one answer.
3. A nil condition is refused by search, not read as a match-all. (A conditional
   delete with no condition body is a different request — delete-all — and
   stays one.)
4. Async submit refuses at submission what the executor could not run.
5. A backend's own residual is unaffected: a filter a backend cannot push
   into SQL is still evaluated by that backend against the filter it was
   given. What is gone is the engine re-deciding a query the backend was
   never told to narrow.
