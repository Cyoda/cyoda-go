---
paths:
  - "internal/domain/**/*.go"
  - "plugins/**/*.go"
---
# Ownership & Mutability Rules

## General Rules (apply everywhere)

1. **Constructors establish ownership.** `NewX()` returns a fresh instance owned by the caller. Factories must not retain hidden references.
2. **Unexported fields by default.** Exported fields permitted only for DTOs at the API boundary (e.g. generated OpenAPI types).
3. **Share read-only via interfaces.** Expose getter-only interfaces. Consumers receive the interface, not the concrete struct.
4. **Stores copy internally on Save().** Callers don't need to copy before passing. Callers may continue using the object after the call.

## Boundary Rules (external calls, async dispatch, shared handlers)

5. **Snapshot before yielding control.** No mutations until control returns.
6. **Consumed once serialized.** Don't read or mutate after handing to wire format or external consumer.
7. **Transformations consume inputs.** Callers must not use inputs after the call.
8. **Single rollback authority.** One component restores from snapshot. No independent restores.

## Verification

- `make race` catches aliasing only where two goroutines share the object.
- Review catches the rest: boundary-crossing sites follow snapshot → yield → transform → commit/rollback.
