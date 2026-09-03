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
  (`path-grammar.md` §12) — so no valid request changes status.
- **A nil/absent condition** is `400 INVALID_CONDITION`, not "match
  everything". A caller that wants every entity of a model pages it.

Asynchronous search translates the condition at submission, so a job is never
persisted for a condition no backend can execute; the executor streams through
`Iterate` and has no fallback branch of its own.

## Invariant Cloud must mirror

1. A synchronous search reaches storage through exactly one call. No
   whole-model read exists to fall back to, in a transaction or out of one.
2. A condition that cannot be translated is a `400`, never a request served by
   a different mechanism with a different cost and a different read-set.
3. A nil condition is refused, not read as a match-all.
4. Async submit refuses at submission what the executor could not run.
