# OpenAPI dead-surface disposition (group 5 of #369)

**Status:** design / agreed. Final reconciliation slice of the OpenAPI contract effort (#369).
**Date:** 2026-07-06
**Effort:** group 5 of the reconciliation umbrella #369; **this slice closes #369.**
**Governs:** ADR 0003 (typed-but-open, additive-only, per-direction strictness); the Spec-1
`x-cyoda-status` marker + conformance coverage gate; [[project_openapi_typed_but_open_policy]].
**Evidence:** `docs/analysis/openapi/README.md` §5 P1, §6D (SQL-Schema), §6F (stream-data), §7 point 3.

---

## 1. Problem & reframe

`api/openapi.yaml` is the authored, cyoda-go-led contract (Cloud conforms to it). ~22 operations
are published but excluded from codegen (`api/config.yaml` exclude-tags) and unrouted — they 404 at
runtime. The original audit framed this as a binary: *implement, or formally remove, a contract
that is 25% fiction.*

**That framing is now stale.** Spec 1 already introduced a third state — the `x-cyoda-status:
planned|unimplemented` operation marker — and a conformance gate
(`internal/e2e/zzz_openapi_conformance_test.go`) enforcing **exactly-one-of {exercised-by-e2e,
marked}**, plus a stale-marker check (a marked op returning 2xx fails). The old `knownUncoveredOps`
allowlist and excluded-tag skip were retired. So the entire dead surface is **already labeled**, and
the gate passes today. The fiction is no longer *silent* — it is *labeled roadmap*.

Group 5 is therefore not "stop the 404s." It is the **honesty audit on those labels**, and the
**final disposition** of every marked surface.

## 2. Governing principle (new invariant)

> **Every non-live `x-cyoda-status` marker must be backed by a tracking issue.**

A marker with no owner and no issue is not roadmap — it is relabeled fiction, and weaker than the
allowlist it replaced (which was at least documented and deletable). This slice makes the invariant
hold for the whole marked set, which is the precondition for closing #369.

## 3. Disposition decisions

The marked set is exactly **23 ops**: 13 Stream Data (`unimplemented`), 9 SQL-Schema (`planned`),
and `accountSubscriptionsGet` (`planned`, routed/501). No other dead or fictional surface exists
(the gate would already fail otherwise).

| Surface | Marker | Decision | Tracking |
|---|---|---|---|
| **Stream Data** (13 ops, `/platform-api/stream-data/*`) | `unimplemented` | **Keep, minimal touch** — markers only; leave fossil operationIds/schemas. YAGNI on speculative, under-review surface. | **new #381** (disposition review) |
| **SQL-Schema** (9 ops, `/sql/schema/*`) | `planned` | **Keep, fix the authored-contract drift now** — committed surface, so the contract must be self-consistent. | **new #382** (implementation) |
| **CQL Execution Statistics** (0 ops) | exclude entry only | **Remove** the vestigial `api/config.yaml` exclude-tags entry. | — |
| **accountSubscriptionsGet** (routed, 501) | `planned` | **No change** — already honest and tracked. | existing #283 |

**The fix-SQL-but-not-Stream asymmetry is intentional and principled** (record in the PR body):
both surfaces are unrouted and unbuilt, but SQL-Schema is *committed* (`planned` + #382), so its
authored contract is a real deliverable and must be correct; Stream Data is *under review*
(`unimplemented` + #381), so polishing it before the disposition decision is premature — it would be
churn on a surface that may be redesigned or removed. The line is: **fix authorial debt on committed
surface; do not beautify speculative surface.**

`accountSubscriptionsGet` already carries `x-cyoda-status: planned` and honest prose, is routed, and
returns 501 (exercised by e2e). It is not dead surface and needs no change.

## 4. SQL-Schema contract corrections (the only content edits)

All nine ops are unrouted, so there is **no server to conform to** — these are pure **authoring**
fixes making the published contract well-formed and self-consistent. Reconciliation *direction* is
N/A (no server side); the standard is "correct OpenAPI + consistent with the platform's conventions
+ consistent with the op's own examples."

Defect inventory (current line numbers drift; execution re-locates against the working tree):

| # | Defect | Ops affected | Fix |
|---|---|---|---|
| **D1** | `200` response schema is malformed `type: array` **+ sibling `$ref`** (no `items`) → decodes as untyped `[]interface{}` | `genTables` (→`TableConfigDto`), `getSchemas`/listAll (→`SchemaConfigDto`), `updateTables` 200 (→`TableConfigDto`; found during Task-2 review — audit list was incomplete) | `type: array` with `items: {$ref: …}` |
| **D2** | `updateTables` request body is `type: string`, but its own example is an array of `TableConfigDto` | `updateTables` | request body `type: array, items: {$ref: TableConfigDto}` |
| **D3** | `404` responses use `application/json` with no error schema; platform standard (and these ops' own `400`s) is `application/problem+json ProblemDetail` | `getSchemaByName`, `deleteSchemaByName`, `getSchema`, `deleteSchema` | `404` → `application/problem+json` with `schema: {$ref: ProblemDetail}` |
| **D4** | `FieldConfigDto` marks `hidden`/`isArray` **required**, but the `genTables`/`updateTables` examples omit them on every field and use an **unmodeled `arrayFields`** | shared `FieldConfigDto` (via `TableConfigDto`) | make `hidden`/`isArray` optional; model `arrayFields` as `type: array, items: {$ref: FieldConfigDto}` (recursive, typed-but-open) — reconcile schema **to** the examples, which reflect the intended shape |

Already correct, left untouched: `saveSchema`/`putSchema` `400`s (already `application/problem+json
ProblemDetail`); the honest "NOT YET IMPLEMENTED … Planned for the Trino SQL surface" prose + the
`x-cyoda-status: planned` markers on all nine ops.

**No new error codes, no new component schemas, no orphaned schemas** (both surfaces are retained, so
all referenced schemas stay live). No help-topic / README / COMPATIBILITY changes (no env var, no
error code, no interface, no version bump).

## 5. Config & governance edits

- `api/config.yaml`: **remove** the `"CQL Execution Statistics"` exclude-tags line. Zero ops carry
  the tag, so `go generate ./api` output is byte-identical — verified by `make check-codegen`. Keep
  `"Stream Data"` and `"SQL-Schema"` (their ops stay unrouted).
- No `x-cyoda-status` marker changes (Stream Data stays `unimplemented`, SQL-Schema stays `planned`).
- Issue numbers do **not** enter shipped artefacts (per project rule) — #381/#382 live in this spec,
  the plan, commits, and the PR body only. The op descriptions already say "NOT YET IMPLEMENTED."
- `docs/cloud-parity/openapi-conformance.md`: append the group-5 disposition + the "every non-live
  marker is backed by a tracking issue" invariant, so Cloud mirrors the decision.

## 6. Per-endpoint status table (authored contract, post-fix)

Runtime status for **every** Stream Data and SQL-Schema op is **404** (unrouted) — the table below
is the *authored contract shape*, which is what group 5 changes. Stream Data ops: **no change** (all
13 omitted here). SQL-Schema:

| Op | Method / path | Authored responses (post-fix) | Group-5 edit |
|---|---|---|---|
| getSchemaByName | GET `/sql/schema/` | 200 `application/json`; 404 `application/problem+json ProblemDetail` | D3 |
| saveSchema | POST `/sql/schema/` | 200; 400 `problem+json ProblemDetail` | none |
| deleteSchemaByName | DELETE `/sql/schema/` | 200; 404 `problem+json ProblemDetail` | D3 |
| genTables | GET `/sql/schema/genTables/{entityModelId}` | 200 `array<TableConfigDto>` | D1 |
| getSchemas | GET `/sql/schema/listAll` | 200 `array<SchemaConfigDto>` | D1 |
| putSchema | PUT `/sql/schema/putDefault/{schemaName}` | 200; 400 `problem+json ProblemDetail` | none |
| updateTables | POST `/sql/schema/updateTables/{entityModelId}` | request `array<TableConfigDto>`; 200 `array<TableConfigDto>` | D2 + D1 |
| getSchema | GET `/sql/schema/{schemaId}` | 200; 404 `problem+json ProblemDetail` | D3 |
| deleteSchema | DELETE `/sql/schema/{schemaId}` | 200; 404 `problem+json ProblemDetail` | D3 |

(Shared: D4 on `FieldConfigDto`, reached via `genTables`/`updateTables`/`getSchemas`.)

## 7. Coverage matrix (scenario × layer)

All touched ops are **unrouted**, so there is no running-backend 2xx/4xx behavior to exercise; the
real gate is **oasdiff** (contract shape) + the **conformance marker** gate + **kin-openapi load**.
Per `.claude/rules/test-coverage.md`, N/A cells are waived with a one-line reason.

| Scenario | unit | running-backend e2e | cross-backend parity | gRPC | oasdiff | notes |
|---|---|---|---|---|---|---|
| D1 array-items fix (genTables, getSchemas) | N/A¹ | N/A² | N/A³ | N/A⁴ | ✅ err-ignore | kin-openapi load (TestMain + `go test ./api`) |
| D2 updateTables body string→array | N/A¹ | N/A² | N/A³ | N/A⁴ | ✅ err-ignore | as above |
| D3 404 → ProblemDetail (4 ops) | N/A¹ | N/A² | N/A³ | N/A⁴ | ✅ err-ignore | as above |
| D4 FieldConfigDto schema↔example | N/A¹ | N/A² | N/A³ | N/A⁴ | ✅ err-ignore | recursive schema must load in kin-openapi |
| CQL exclude-tags removal | N/A¹ | N/A | N/A | N/A | N/A | `make check-codegen` (byte-identical generated.go) |
| Dead surface stays **unrouted** | N/A | ✅ NEW⁵ | N/A³ | N/A⁴ | N/A | one representative 404 assertion per excluded tag |
| Markers stay honest (marked XOR exercised; no stale marker) | N/A | ✅ existing⁶ | N/A | N/A | N/A | conformance gate already enforces |

¹ No Go code changes (only `openapi.yaml` + `config.yaml`); nothing to unit-test.
² Op is excluded from codegen and unrouted — no handler exists to exercise (returns 404).
³ No backend-specific behavior (no storage path touched).
⁴ No gRPC surface for stream-data / sql-schema.
⁵ **New** e2e in `internal/e2e`: assert a representative `/sql/schema/*` and `/platform-api/stream-data/*`
   path returns 404 — locks the "unrouted" contract so a future accidental route without marker
   removal is caught by an explicit test (belt-and-suspenders with the stale-marker gate).
⁶ `TestOpenAPIConformanceReport` (`zzz_openapi_conformance_test.go`) — unchanged; re-run green proves
   all 23 markers remain valid after the edits.

## 8. oasdiff handling

D1–D4 are contract-shape changes oasdiff classifies as **breaking**, but they break **no working
client** (the ops are unrouted → any client already gets 404). Handle with **surgical, fail-closed
`.github/oasdiff-err-ignore.txt` entries** (the established ADR-0003 Decision-7 pattern:
createCollection line 15, the OIDC envelope block, the message slice). Expected entries:

- `updateTables` request `type` string→array (`request-body-type-changed`) — mirrors createCollection.
- `genTables`/`getSchemas` `200` array-shape change (`response-*` — exact category verified at impl).
- The four `404` media-type swaps `application/json`→`application/problem+json`
  (`response-media-type-removed` for the removed `application/json`; the added problem+json is additive).
- `FieldConfigDto` `required` narrowing (`response-required-property-removed` for hidden/isArray;
  `arrayFields` addition is additive).

**Do not pre-generate the wording** — capture the *exact* pinned-oasdiff (`v1.21.0`) singleline text
at implementation and paste verbatim (each entry matches op+path+message, so it cannot mask any other
break). VERIFY which changes are actually ERR vs WARN before adding an entry — some may be non-breaking.

## 9. Verification plan (Gate 5)

Controller runs at consolidation points (subagents are go-vet/compile/codegen only — sandbox Docker
is inconsistent):

- `go generate ./api` + `make check-codegen` (green; generated.go unchanged by the CQL removal).
- `go test ./api` (spec loads; corrected schemas parse in kin-openapi).
- Full `go test ./internal/e2e/... -v` incl. `TestOpenAPIConformanceReport` (23 markers valid) + the
  new 404-assertion test — with Docker.
- oasdiff gate: `git show origin/release/v0.8.2:api/openapi.yaml > /tmp/base.yaml; oasdiff breaking
  /tmp/base.yaml api/openapi.yaml --fail-on ERR --err-ignore .github/oasdiff-err-ignore.txt`
  (capture exit via redirect, not pipe-to-tail).
- `make race` (CI-parity scope) exit 0; `make test-short-all` (plugins); `go vet ./...`; gofmt gate.

## 10. Non-goals / out of scope

- **Implementing** stream-data or sql-schema (tracked in #381 / #382).
- **Curating** Stream Data operationIds/schemas (deferred to #381's disposition decision).
- Cloud-fact-blocked deferrals already in `docs/cloud-parity/` (E6 `fieldsChangedCount`, D2 gRPC
  conditional-delete, BAD_REQUEST-vs-MALFORMED_REQUEST naming) — unchanged.
- Separately-tracked features: #83 (Trino docs), #193 (content-types), #283 (subscriptions),
  #342 (gRPC envelope), #379 (tx-params). None is contract drift; #369 closes without them.

## 11. Deliverables

1. `api/openapi.yaml`: D1–D4 SQL-Schema corrections (+ `FieldConfigDto` schema). Stream Data untouched.
2. `api/config.yaml`: remove the `"CQL Execution Statistics"` exclude-tags line.
3. `.github/oasdiff-err-ignore.txt`: surgical entries for D1–D4 (exact wording at impl).
4. `internal/e2e`: one representative 404-assertion test per excluded tag.
5. `docs/cloud-parity/openapi-conformance.md`: group-5 disposition + the marker-backing invariant.
6. Tracking issues **#381** (Stream Data disposition) and **#382** (SQL-Schema implementation) — created.
7. PR to `release/v0.8.2`, milestone v0.8.2, body: `Closes #369` + the asymmetry rationale + #381/#382.
