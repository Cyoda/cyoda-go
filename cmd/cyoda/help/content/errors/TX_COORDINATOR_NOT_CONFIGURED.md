---
topic: errors.TX_COORDINATOR_NOT_CONFIGURED
title: "TX_COORDINATOR_NOT_CONFIGURED — distributed transaction coordination not available"
stability: stable
see_also:
  - errors
  - errors.TX_REQUIRED
  - errors.TX_NO_STATE
---

# errors.TX_COORDINATOR_NOT_CONFIGURED

## NAME

TX_COORDINATOR_NOT_CONFIGURED — a requested capability requires distributed transaction coordination that is not available on this node.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `no`.

## DESCRIPTION

Reserved. Not currently emitted by any cyoda-go code path.

cyoda-go does not run a distributed two-phase-commit coordinator. Transactions
are request-scoped and owned by the node that began them. A signed tx-token
(`{NodeID, TxRef}`) carries the owning node's identity so that callbacks and
subsequent requests in the same transaction are routed to the correct node
without any external coordinator.

This error code is reserved for a future capability where distributed
multi-transaction coordination may be required. If you receive it from a
current cyoda-go release, it indicates an unexpected condition; raise a support
ticket.

## SEE ALSO

- errors
- errors.TX_REQUIRED
- errors.TX_NO_STATE
