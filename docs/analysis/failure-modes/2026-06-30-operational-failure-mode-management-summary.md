# Operational Resilience Review — Management Summary

**Date:** 2026-06-30
**Subject:** System availability, consistency and correctness under load
**Code reviewed:** the pre-release build of version **v0.8.2** (the `release/v0.8.2` development branch)

---

## Purpose

To conduct an evidence-based investigation of the core system and the postgres storage engine, and to identify operational failure modes of the current design and codebase in regards to availability, consistency and correctness, in particular under load/stress situations.

---

## Main Finding

**Safeguards against resource exhaustion prepared, but not implemented**

- For each incoming request, the system reserves one "line" to the database and **holds it
  open for the entire request** — including while the request waits on an external service
  to do its work.
- There is a **limited number of these lines**, they are **shared across all customers** on
  a node, and at the time of review there was **no time limit** on how long one could be
  held and **no automatic cleanup** if a request got stuck. A safety mechanism that was
  intended to reclaim stuck lines existed in the code but **had never been connected**, so
  it was not actually protecting anything.

This created two ways the system could falter under moderate load:

1. **Temporary slowdown or stall.** If an external service is slow or unresponsive, requests
   waiting on it keep their database lines reserved. Enough of them at once can use up all
   the lines and stall the node for **every customer on it** — but this **recovers on its
   own** once the stuck requests time out.

2. **A stall that needs a restart.** If a request crashes at the wrong moment, its database
   line is **never released** and is lost until the service is restarted. Repeated over
   time, this slowly drains the available lines.

**Important:** both of these are **availability** problems — the system stalling or needing
a restart. Neither one **corrupts data or loses data**, and we separately confirmed that
**one customer's data cannot leak into another's**. The most serious risks we found are
about the system *staying up*, not about it producing wrong or lost data.

---

## Implications

- **At today's usage levels, this is unlikely to surface.** It becomes a real risk at
  moderate to high concurrency combined with an unreliable external service.
- **Worst case:** a node becomes temporarily unavailable, and in the crash scenario an
  operator may need to restart it. **No silent data loss or corruption.**
- This is exactly the kind of weakness expected in a young, fast-moving codebase — and
  exactly what this proactive review is designed to catch **before** we scale up, rather
  than discovering it in production.

---

## Remediation

**Issue #354**:

- adds **time limits** so a database line can no longer be held indefinitely;
- adds **automatic cleanup** so a crashed request always releases its line; and
- makes the system **reject quickly** when it is busy, instead of hanging.

This fix is **low-cost and low-risk** — it is contained within the database layer and does
not require redesigning the system. Once delivered, it removes the availability failure the
reviewer identified: the restart-required scenario is eliminated, and the temporary-stall
scenario corrects itself cleanly.

---

## Further findings and action points

Beyond the primary remediation (#354, above), the review surfaced a broader set of findings.
All are now tracked as issues, listed below by operational severity. Two points frame the list:

- Most items are **availability hardening**. **#360 is the only data-integrity item**, and it
  is a narrow, rare condition.
- **#357 is documentation, not a fix.** Holding a database connection across external work is
  a deliberate, engine-specific design choice (postgres only); the platform already provides
  the means to avoid it (the `COMMIT_BEFORE_DISPATCH` processor property), and **whether to use
  it is the application's decision**, not a default the platform imposes — much as a database
  does not cap a table's size by default in case a disk fills.

| Issue | Severity | What it addresses |
|---|---|---|
| **#358** | High | gRPC channel to compute nodes — stop a single frozen node or an unhandled crash from taking down the whole node |
| **#359** | High | Bound asynchronous search so one request cannot exhaust memory or run on unchecked |
| **#166** | High *(existing)* | Cap very large list/search reads to prevent out-of-memory |
| **#360** | High | The one data-integrity item — prevent a rare lost-field corruption in auto-evolving schemas under concurrency |
| **#361** | Medium | HTTP request timeouts, crash isolation, and monitoring/alerting for connection-pool saturation |
| **#362** | Medium | Safer database schema migrations during rolling upgrades |
| **#364** | Medium | Peer failover in multi-node mode (relevant only when clustering is enabled — off by default) |
| **#363** | Low | Defense-in-depth hardening of tenant isolation (no live leak today) |
| **#365** | Low | Minor code-quality and consistency cleanups |
| **#357** | By design | Document when an application should ring-fence its database connection (`COMMIT_BEFORE_DISPATCH`) — a clarification of intended behaviour, not a defect |

---

*This summary is a non-technical companion to the full engineering analysis
(`2026-06-29-operational-failure-mode-analysis.md`) and its re-run procedure
(`2026-06-29-operational-failure-mode-analysis-playbook.md`).*
