# Correctness/consistency over availability

cyoda-go chooses correctness and consistency over availability whenever the two
conflict. It fails closed, never open.

- An operation (a write, a transition, an arm) that cannot be completed
  correctly is rejected and rolled back — never committed partially, and never
  with a substituted or fallback value standing in for the correct one.
- An unavailable dependency that a correct result requires — the compute node a
  required Function/processor/criterion callout needs, a peer, a store — fails
  the operation. It does NOT trigger a "degrade gracefully" path that returns a
  wrong-but-available answer.
- Do not raise dependency-unavailability as a reason to weaken an invariant or
  to add a fallback. The invariant is the requirement; the dependency being up
  is a precondition for serving the request, not a nice-to-have to design
  around.

Analogy: a car with no wheels available is not shipped on blocks or with the
wrong wheels bolted on — it is not shipped. Likewise a write whose required
computation cannot run is not committed with a guessed or omitted value.

Pairs with `multi-node-primary.md`: cluster correctness is designed in from the
start, and availability is never the lever used to descope or weaken it.
