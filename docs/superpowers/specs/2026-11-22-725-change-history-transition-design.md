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
  with a named transition (HTTP PUT/PATCH, gRPC update, gRPC patch, gRPC
  `EntityTransitionRequest`), loopback update, collection create and collection update (HTTP and
  gRPC), scheduled firing, delete — and on every version a cascade commits,
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
6. **What a cascade sees — a behaviour change.**
   - Today: what workflow selection, transition criteria and processor
     criteria see in `transitionForLatestSave` (alias `previousTransition`)
     depends on the door: on a named update (HTTP, gRPC update, gRPC
     transition, gRPC collection item) the request's own transition name; on
     a create, a loopback update, every collection create item (HTTP and
     gRPC) and every HTTP collection update item, an empty string; on a scheduled firing, the name the previous
     request saved.
   - After: for the whole run, on every door, the name of the version the
     request started from, as its transaction sees it (its own earlier writes
     included); on a create, none. A `COMMIT_BEFORE_DISPATCH` boundary does
     not change it: where a processor's dispatch mode sits never changes what
     a criterion sees. Every save cyoda makes for the request — the save
     before a callout, the boundary flush, the `CompareAndSave` that opens
     TX_post, the final save — writes the request's own name; only what the
     run evaluates sees the start version's value. So a read of the entity
     during the run, joined or not, sees one value: the request's name once
     cyoda has saved the entity in the transaction, the stored version's
     before.
   - Reason: no request chooses, through the transition it names, which
     workflow definition and which guards apply to itself (the help's advice
     on workflow selection); the value is the same on every door; and it is
     what `GET /entity/{entityId}/transitions` evaluates (the stored version)
     and what the alias `previousTransition` says. The previous request's
     name is also caller-chosen, so selection on it is selection on stored
     data, like any other stored field.
   - Migration (CHANGELOG `### Breaking`): a lifecycle criterion
     `transitionForLatestSave == "X"` on an automated transition fires today
     only in the cascade of a request that names X; after the change it fires
     in the cascade of any request that follows a version saved by X. There is
     no field for the current request's name; route on the state instead.

Defects fixed in this change (each: today → after):

7. **Scheduled firing.** Today: the version a scheduled firing makes keeps
   the transition name of the request before it (the firing restamps actor and
   executor only, `fire_scheduled.go`), so an entity read shows that stale
   name in `meta.transitionForLatestSave`; so does a version the firing's
   `COMMIT_BEFORE_DISPATCH` boundary flushes. After: every version the
   firing saves records the scheduled transition's name.
8. **`COMMIT_BEFORE_DISPATCH` boundary version.** Today: on a create, a
   loopback update, every item of a collection create (HTTP and gRPC), and
   every item of an HTTP collection update (also an item that names a
   transition), the version the boundary flushes
   (`flushAndCommitSegment`, `es.Save` in TX_pre) has an empty name, and the
   final version has the request's name (`loopback` or the item's
   transition). After: both carry the request's name.
9. **Postgres delete tombstone.** Today: postgres copies the previous
   version's `_meta` into the tombstone, so the tombstone keeps the previous
   version's transition name; memory and sqlite record none. Nothing exposes
   it yet; the new history field would. After: no backend records a
   transition on a tombstone. Tombstones are excluded from every postgres
   read by `_meta.deleted` / `NOT deleted`, so no other read changes.
10. **sqlite reads on unparseable meta.** Today: when an entity's or a
   version's meta JSON does not parse, sqlite drops the parse error and
   returns the row without the meta fields: `GetVersionMetadata` and
   `GetVersionByTransaction` (empty attribution), and `scanEntityFromRow` /
   `scanVersionEntity` (empty tenant, state, transition, attribution), which
   serve `Get`, search, grouped stats and the transaction overlay. A NULL or empty meta is
   skipped the same way. Postgres fails the read. After: every sqlite read
   fails on unparseable, NULL or empty meta (every writer stores meta,
   tombstones included).
11. **Help on `transitionForLatestSave`.** Today: `crud` says a create stores
   the literal `"workflow"` (it stores `loopback`); the `crud` example shows
   `"UPDATE"`, which reads as a change type rather than a transition name; `search` lists the field among
   the meta dates (it is a string). After: the help describes the actual
   behaviour.

Out of scope:

12. Cassandra stores the transition for the latest version only and is on SPI
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
transaction, create-then-delete in one transaction, `DeleteAll`); two saves of one entity
in one transaction → the last save's value.

Mechanics (mid-milestone): SPI PR into `main`, no tag; cyoda-go pseudo-pins
that commit; `make repin-plugins` for the in-tree plugins; `COMPATIBILITY.md`
records the pin (Gate 4).

### In-tree plugins

No migration; each already keeps the name per version.

- memory: read the version's entity copy (nil on a tombstone → empty).
- sqlite: copy `transition_for_latest_save` out of the `json(meta)` it already
  reads; fail the read on unparseable meta (decision 10).
- postgres: copy `transition` out of the `doc->'_meta'` it already decodes;
  `deleteOn` clears the transition (decision 9).

### Engine and service

The request's name is known on every door: the named transition, `loopback`,
or the scheduled task's transition. Every save cyoda makes for the request
writes that name: the save before a callout (`saveAnchorBeforeCallout`), the
boundary flush in `flushAndCommitSegment` (TX_pre), the `CompareAndSave` that
opens TX_post, the service's final saves, and the scheduled firing's final
`CompareAndSave`. What the run evaluates (workflow selection, criteria) sees
the start version's value (decision 6).

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
- `grpc`: `changeMeta` field list and its wording gain `transition`.
- Schema descriptions of `transitionForLatestSave` match decision 5:
  `api/openapi.yaml` (entity meta schemas), `docs/cyoda/schema/common/EntityMetadata.json`.
- `search`: `transitionForLatestSave` is listed among the meta **dates**; it
  is a string.
- `workflows`: what a lifecycle criterion and workflow selection see in
  `transitionForLatestSave` during a cascade (decision 6); the selection
  advice no longer counts `transitionForLatestSave` among the fields a caller
  cannot rewrite (the previous request's caller chose it).
- `auth integration`, segregation of duties, step 3 (replaces the copy
  advice).

  What the check proves: the approver is not the principal of the latest
  submit, and the approval changes nothing the submitter submitted. (It does
  not prove the approver never wrote or submitted the content in an earlier
  cycle; an application that needs that walks the history further itself.)

  Conditions the application follows:
  - The approve transition is `manual: true`, and the check is a function
    criterion of that transition (not a processor, not elsewhere), with
    `attachEntity` not set to `false`.
  - The approval is a single-entity request from the backend naming the
    approve transition: never an item of a collection update (step 2's OBO
    client allow-list already excludes compute-node write-backs). So the
    version the approval starts from is the newest committed version.
  - Nothing writes the entity between the submit and the approval. A write in
    between makes the check refuse; the entity is submitted again.
  - The approval request carries no data change: HTTP `PATCH` with `{}` and
    `If-Match` (the submit entry's `transactionId`, or `*`) on the
    named-transition route, gRPC `EntityPatchRequest` with `{}`, or gRPC
    `EntityTransitionRequest`. The check enforces it (below).

  The check, in a criterion of the approve transition, with the compute
  node's own client and without `pointInTime`:
  1. Read the entity's change history (`GET /entity/{entityId}/changes`, or
     gRPC `EntityChangesMetadataGetRequest`) and take the **newest** entry.
  2. Approve only when its `transition` is the submit transition, its
     `attributedKind` is `user`, and its `user` differs from `authid` (byte
     for byte).
  3. Read the version that entry's transaction committed
     (`GET /entity/{entityId}?transactionId=<its transactionId>`) and require
     its data to equal the data in the criterion's payload (as JSON values).
  4. Refuse in every other case: also when a read fails, and when the payload
     carries no data. On a backend that
     does not record `transition` yet, the field is absent and the check
     refuses.

  Why it holds: if another transaction commits the entity after the approval
  read it, the approval fails at commit, so an approval that commits replaces
  exactly the entry it checked; that entry is named `submit` only when the
  last request of its transaction named it; its `user` is that transaction's
  attributed principal, stamped by cyoda; and step 3 rules out the approval
  rewriting the data. The help also says: a scheduled transition named like
  the submit is recorded as a submit by the principal that armed it (the last
  writer that re-armed it); a compute node's write-back in alice's
  transaction that names the submit is recorded as alice's submit.

  The integration help's intro to the recipe ("In the approval processor or
  criterion") narrows to the approve transition's criterion.

  *WHAT CYODA RECORDS* lists `transition` ("the same three fields" becomes
  four). `crud`'s `/changes` field bullets and example gain it.
- `docs/access-to-the-cyoda-api.html`: the same recipe.
- `docs/ARCHITECTURE.md`: the entity-history bullet lists `transition`; the
  anchor-save and boundary-flush description states which name each save
  writes and what the run evaluates (decision 6).

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
| criterion on `transitionForLatestSave` sees the start version's value: named update, loopback update, create (none), scheduled firing | engine | yes | yes | — | — |
| same, HTTP collection create and update items (named and loopback) | — | yes | — | — | — |
| same, gRPC update, patch, `EntityTransitionRequest`, collection create and update | — | — | — | yes | — |
| a joined read of the entity during the run sees the request's name after the first save in the transaction | — | yes | — | — | — |
| workflow selection on `transitionForLatestSave` sees the previous save's value | engine | yes | — | — | — |
| criterion after a `COMMIT_BEFORE_DISPATCH` boundary still sees the start version's value | engine | yes | — | — | — |
| gRPC collection update, named and loopback items | — | — | — | yes | — |
| sqlite unparseable / NULL meta fails `Get`, search, grouped stats | sqlite | — | — | — | — |
| two saves of one entity in one transaction → last wins | — | — | — | — | yes |
| approval recipe: approve passes (PATCH `{}` with If-Match; gRPC transition); same user refused; write between refused; approval with a data change refused; payload without data refused | — | yes (compute-node test double) | — | — | — |

## Changelog

`CHANGELOG.md` gains an `[Unreleased]` section: `### Breaking` — decision 6;
`### Added` — `transition` on the history surfaces; `### Fixed` — items 7–11.

## Gate 7

`docs/cloud-parity/change-history-transition.md`: the new history field, and
decision 6 (what a cascade sees in `transitionForLatestSave`). CaaS ticket:
Cloud's change history and entity-change audit carry no transition today.

## Left to the builder

Test file placement, helper names, how the engine carries the request's name
to its save sites, commit order, the exact help sentences (plain STE English,
the client's view).
