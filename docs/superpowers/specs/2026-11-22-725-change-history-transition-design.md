# Transition name on entity change history — design

Issue: #725. Milestone v0.10.0. Base: `release/v0.10.0`.

## Intent

An application must be able to read, from an entity's change history, who
made each version **and which transition the request named**. The change
history is written in the transaction that makes the change; cyoda stamps its
principals itself, and no later write can change an entry. With the
transition name on it, the history answers "who made which transition"
without a field the application's backend must protect.

The driving case is segregation of duties: "the submitter may not approve".
The approval check reads the newest change-history entry, requires it to be
the submit, and compares its `user` with the approver. Today the help tells applications to
copy the submitter's id into the entity's data in the submit processor. That
copy is a field the backend must then protect against its own request paths.

## Goals

- Every change-history entry carries the transition its request named.
- The value is right on every door that writes a version — create, update
  with a named transition (HTTP PUT/PATCH, gRPC update, gRPC
  `EntityTransitionRequest`), loopback update, collection create, collection
  update, scheduled firing, delete — and on every version a cascade commits,
  including the version a `COMMIT_BEFORE_DISPATCH` segment boundary commits.
- One rule for what a criterion and workflow selection see in
  `transitionForLatestSave` during a cascade, on every door.
- The same values on memory, sqlite and postgres (Cassandra via its own
  issue).
- The help's segregation-of-duties recipe uses the change history; the advice
  to copy the submitter into the entity's data is deleted everywhere.

## Non-goals

- Recording the automatic transitions a cascade makes after the named one:
  the state-machine audit (`TRANSITION_MAKE`) records those. A version
  records one name, the one its request named. (Paul's ruling.)
- Adding the transition to the callout payload's entity metadata (it carries
  none today: `internal/grpc/dispatch.go` `buildEntityPayload`).
- Reserving the transition name `loopback`.
- Any new endpoint or error code.

## Anti-goals

- A history entry that names a transition its request did not name. A wrong
  name is worse than none: an authorization check acts on it.
- A request choosing, through the transition it names, which workflow
  definition is selected for it.
- Two recipes for the same check in the help.

## Decided

Paul:

1. The history records the transition **the request named**, not every
   transition the cascade made.
2. The segregation-of-duties recipe moves to the change history; the copy
   advice is deleted, not kept as an alternative.
3. The recipe is a client recipe on current cyoda functionality: it states
   the conditions under which it is correct, rather than cyoda handling every
   workflow shape.

Builder's calls:

4. Wire name `transition` on every history surface. `transitionForLatestSave`
   stays the name on entity reads, search and lifecycle criteria.
5. Value set, the same as `meta.transitionForLatestSave` on an entity read:
   the named transition's name; `loopback` for a save that named none (a
   create, an update without a transition, a collection item without one);
   the scheduled transition's name for a scheduled firing. A delete records
   none: the field is absent on a `DELETE` entry. A transition a workflow
   itself names `loopback` cannot be told apart from a save that named none;
   the help says so.
6. **What a cascade sees.** While the engine runs — workflow selection,
   transition criteria, processor criteria, on every door — the entity's
   `transitionForLatestSave` (alias `previousTransition`) is the value of the
   version the request started from, as its transaction sees it (the
   transaction's own earlier writes included): the transition the *previous*
   save named; on a create, none. After a `COMMIT_BEFORE_DISPATCH` boundary
   it is the boundary's version, which carries the request's name. The
   request's own name is written only on the versions cyoda commits for the
   request: at a segment boundary and at the final save. The save before a
   callout keeps the value the run sees, so a joined callback that reads the
   entity sees the same value as the run's criteria. Reason: the
   help tells applications to select workflows on fields the caller cannot
   rewrite, and the request's own transition name is chosen by the caller.
   This changes today's behaviour on a named update (a criterion sees the
   request's own name today) and on a loopback update, create and collection
   update (a criterion sees an empty string today); it matches what
   `GET /entity/{entityId}/transitions` evaluates (the stored version) and the
   alias `previousTransition`.
7. A scheduled firing names its scheduled transition (today it restamps actor
   and executor only, so its version keeps the previous request's name — on
   entity reads too).
8. A delete tombstone carries no transition on every backend. Postgres copies
   the previous version's `_meta` into the tombstone, transition included; it
   is fixed to clear it. Tombstones are excluded from every postgres read by
   `_meta.deleted` / `NOT deleted`, so no other read changes.
9. sqlite `GetVersionMetadata` and `GetVersionByTransaction` drop a version's
   attribution silently when its meta JSON does not parse; postgres fails the
   read. sqlite is fixed to fail the read (a history read that serves an authorization check must not
   answer with missing fields).
10. Cassandra stores the transition for the latest version only and is on SPI
   v0.8.3. Its work is a cassandra-plugin issue (migration adding the column
   to `entity_changes`, written on Save, empty on Delete, returned by
   `GetVersionMetadata`), not part of this change.

## Contract

### SPI (`cyoda-go-spi`)

`EntityVersionMeta` gains `Transition string`: the `TransitionForLatestSave`
the version was saved with; empty for a `DELETED` tombstone.
`GetVersionMetadata` returns it.

Conformance (`spitest`): per-version value over several saves; tombstone empty
(delete outside a transaction, delete in a transaction, save-then-delete in one
transaction, create-then-delete in one transaction); two saves of one entity
in one transaction → the last save's value.

Mechanics (mid-milestone): SPI PR into `main`, no tag; cyoda-go pseudo-pins
that commit; `make repin-plugins` for the in-tree plugins.

### In-tree plugins

No migration; each already keeps the name per version.

- memory: read the version's entity copy (nil on a tombstone → empty).
- sqlite: copy `transition_for_latest_save` out of the `json(meta)` it already
  reads; fail the read on unparseable meta (decision 9).
- postgres: copy `transition` out of the `doc->'_meta'` it already decodes;
  `deleteOn` clears the transition (decision 8).

### Engine and service

The request's name is known on every door: the named transition, `loopback`,
or the scheduled task's transition. The versions cyoda commits for the request
— segment-boundary `CompareAndSave` and final save — carry that name; the save
before a callout and everything evaluated during the run carry the value of
decision 6.

### Wire

Additive; present when non-empty, absent otherwise.

- HTTP `GET /entity/{entityId}/changes`: `EntityChangeMeta.transition`
  (`api/openapi.yaml`).
- gRPC `EntityChangesMetadataGetRequest` → `changeMeta.transition`
  (`docs/cyoda/schema/common/EntityChangeMeta.json`, regenerated types).
- HTTP `GET /audit/entity/{entityId}`: `EntityChangeAuditEventDto.transition`.
- Test clients and samples follow: `e2e/parity/client/types.go`
  `EntityChangeMeta`, `api/clientprobe/samples/audit-entity-change.json`.

### Errors

No status or error code is added or changed.

| Surface | Codes (as documented in `api/openapi.yaml`) | This change |
|---|---|---|
| `GET /entity/{entityId}/changes` | 200, 400, 401, 403, 404, 409, 410, 413, 503, default | field on 200 only |
| `GET /audit/entity/{entityId}` | 200, 400, 401, 403, 404, 409, 410, 413, 500, 503 | field on 200 only |
| gRPC `EntityChangesMetadataGetRequest` | existing error envelope | field on success only |

Waiver: the error cells are unchanged by this change and are not re-tested.

## Help

- `audit`: `EntityChange` field list gains `transition`. *Recording*: what
  it holds; the principals sentence keeps "a request cannot set them" for
  `actor`, `executedBy`, `transactionId` only; replace the "record it in the
  entity itself" advice with a pointer to the recipe; remove "Entity change
  history carries no transition name at all" (`details` bullet). A request
  that fails after a `COMMIT_BEFORE_DISPATCH` boundary leaves the boundary's
  version, with its name. A scheduled firing that is cancelled, declined or
  expires writes no version.
- `crud`: `/changes` response and field bullets gain `transition`; state the
  1000-entry cap (newest first). Fix `meta.transitionForLatestSave`: drop the
  stale "Known bug" about `"workflow"`, fix the example value `"UPDATE"`, add
  the scheduled-firing value, and the `loopback` ambiguity.
- `grpc`: `changeMeta` field list gains `transition`.
- `search`: `transitionForLatestSave` is listed among the meta **dates**; it
  is a string.
- `workflows`: what a lifecycle criterion and workflow selection see in
  `transitionForLatestSave` during a cascade (decision 6).
- `auth integration`, segregation of duties, step 3 (replaces the copy
  advice). Conditions the application follows:
  - The approval is a single-entity request from the backend naming the
    approve transition: never an item of a collection update (step 2's OBO
    client allow-list already excludes compute-node write-backs). So the
    approval's transaction has not written the entity before its criterion
    runs, and the version it starts from is the newest committed version.
  - Nothing writes the entity between the submit and the approval. A write in
    between makes the check refuse; the entity is submitted again. The
    approver approves exactly what was submitted.

  The check, in a criterion of the approve transition: read the entity's
  change history (`GET /entity/{entityId}/changes`, or gRPC
  `EntityChangesMetadataGetRequest`) with the compute node's own client and
  take the **newest** entry. Approve only when its `transition` is the submit
  transition, its `attributedKind` is `user`, and its `user` differs from
  `authid` (byte for byte); refuse otherwise. No executor allow-list on the
  submit entry: its `user` is the principal the submit was made for, so the
  check needs none, and it survives client rotation.

  Why it holds (for the help's own understanding, stated briefly there): if
  another transaction commits the entity after the approval read it, the
  approval fails at commit, so an approval that commits replaces exactly the
  entry it checked; that entry is named `submit` only when the last request
  of its transaction named it; and its `user` is that transaction's
  attributed principal, stamped by cyoda.

  *WHAT CYODA RECORDS* lists `transition`.
- `docs/access-to-the-cyoda-api.html`: the same recipe.

## Coverage

| Scenario | Unit | E2E (postgres) | Parity | gRPC | SPI conformance |
|---|---|---|---|---|---|
| create → `loopback`; named update → name; loopback update → `loopback` | — | `/changes` + audit | yes | `changeMeta` | per-version value |
| gRPC `EntityTransitionRequest` → name | — | — | — | yes | — |
| delete → absent | — | `/changes` + audit | yes | `changeMeta` | four tombstone paths |
| collection create / update items (with and without a named transition) | — | `/changes` | extend `entity_update_collection` | — | — |
| scheduled firing → scheduled name, on history and entity read | — | yes | extend `attribution` scheduled scenario | — | — |
| `COMMIT_BEFORE_DISPATCH` boundary version carries the request's name | — | yes | extend `history_reads` boundary scenario | — | — |
| multi-node: a forwarded request's versions carry its name | — | — | extend `multinode/attribution` | — | — |
| criterion on `transitionForLatestSave` sees the previous save's value: named update, loopback update, create (none), scheduled firing | engine | yes | yes | — | — |
| workflow selection on `transitionForLatestSave` sees the previous save's value | engine | yes | — | — | — |
| two saves of one entity in one transaction → last wins | — | — | — | — | yes |
| sqlite unparseable meta fails the read | sqlite | — | — | — | — |

## Gate 7

`docs/cloud-parity/change-history-transition.md`: the new history field, and
decision 6 (what a cascade sees in `transitionForLatestSave`). CaaS ticket:
Cloud's change history and entity-change audit carry no transition today.

## Left to the builder

Test file placement, helper names, how the engine carries the request's name
to its save sites, commit order, the exact help sentences (plain STE English,
the client's view).
