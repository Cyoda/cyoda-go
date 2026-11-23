# The change history names the transition each version's request named

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Behaviour

- **The field.** Every entry of an entity's change history carries
  `transition`: the transition the request that made the version named. It is
  present when non-empty and absent otherwise.
  - **HTTP:** `GET /entity/{entityId}/changes`, member `transition` of
    `EntityChangeMeta`.
  - **gRPC:** `EntityChangesMetadataGetRequest`, member `changeMeta.transition`
    (`docs/cyoda/schema/common/EntityChangeMeta.json`).
  - **Audit:** `GET /audit/entity/{entityId}`, member `transition` of an
    `EntityChange` event (`EntityChangeAuditEventDto`).
- **Values.** The same as `meta.transitionForLatestSave` on an entity read:
  - the named transition's name, for a request that names one (HTTP PUT or
    PATCH on the named-transition route, gRPC update, gRPC patch, gRPC
    `EntityTransitionRequest`, a collection item that names one);
  - `loopback` for a save that named none (a create, an update without a
    transition, a collection item without one);
  - the scheduled transition's name, for a scheduled firing.
  A transition a workflow itself names `loopback` cannot be told apart from a
  save that named none.
- **Absent on delete.** A `DELETE` entry has no `transition`.
- **One name per version.** A version records the transition its request
  named, not the automatic transitions the cascade made after it; the
  state-machine audit (`TRANSITION_MAKE`) records those. Every save cyoda
  makes for the request writes that name, including the version a
  `COMMIT_BEFORE_DISPATCH` boundary commits and every version a scheduled
  firing saves. Two saves of one entity in one transaction leave the last
  save's value.
- **What a run evaluates.** Workflow selection and transition criteria read
  `transitionForLatestSave` (alias `previousTransition`). For the whole run,
  on every door, they see the name of the version the request started from, as
  its transaction sees it (its own earlier writes included); on a create,
  the empty string. A `COMMIT_BEFORE_DISPATCH` boundary does not change it. A read of the
  entity during the run that does not join its transaction sees the committed
  version's name; a callback that joins the transaction can get either the
  start version's name or the request's own name, so do not rely on either. Workflow processors carry no criterion: this concerns workflow
  selection and transition criteria only.
- **No new endpoint, status or error code.** The field is additive.

## What changed

Before, the value a run's criteria saw depended on the door: the request's own
name on a named update (HTTP, gRPC update, gRPC patch, gRPC transition,
gRPC collection item); an empty string on a create, a loopback update, every collection create
item and every HTTP collection update item; the name the previous request saved
on a scheduled firing. A request could therefore choose, through the
transition it named, which workflow definition and which guards applied to
itself. Now no request does, and the value is the same on every door. It is
what `GET /entity/{entityId}/transitions` evaluates and what the alias
`previousTransition` says.

Three versions also recorded the wrong name, now fixed: a version a scheduled
firing saved kept the previous request's name; the version a
`COMMIT_BEFORE_DISPATCH` boundary flushed had an empty name on a create, a
loopback update, every item of a collection create (HTTP and gRPC) and every
item of an HTTP collection update; a PostgreSQL delete tombstone kept
the previous version's name.

Migration for a client: a lifecycle criterion
`transitionForLatestSave == "X"` on an automated transition fired only in the
cascade of a request that names X. It now fires in the cascade of any request
that follows a version saved by X. There is no field for the current request's
name; route on the state instead.

## Cloud today

Cloud's change history (`/changes`, gRPC `changeMeta`) and its `EntityChange`
audit event carry no transition. To align, Cloud records the transition name on
every version it writes, as above, returns it on the three surfaces when
non-empty, and gives a delete none. Cloud also aligns what a cascade's criteria
and workflow selection see in `transitionForLatestSave` with the rule above.

The segregation-of-duties recipe in `cyoda help auth integration` reads the
newest history entry's `transition` and `user`. On a backend that does not
record `transition`, the field is absent and the check refuses.

## Storage backends

The SPI's `EntityVersionMeta` carries `Transition`: the
`TransitionForLatestSave` the version was saved with, empty for a `DELETED`
tombstone. `GetVersionMetadata` returns it. The in-tree backends (memory,
sqlite, postgres) agree on every value above. The commercial backend stores the
transition for the latest version only and does not yet record it per version.

## Tests

- E2E (`internal/e2e`, PostgreSQL):
  - `change_history_transition_test.go`: `TestChangesEndpoint_TransitionPerVersion`
    and `TestAuditEntityChange_Transition` (the field on `/changes` and the
    audit event, absent on delete);
    `TestCriterionSeesStartVersionTransition_EachHTTPDoor`,
    `TestCriterionSeesStartVersionTransition_ScheduledFiring`,
    `TestSelectionSeesStartVersionTransition`,
    `TestCriterionAfterBoundarySeesStartVersionTransition` and
    `TestJoinedReadSeesRequestTransition` (what a run evaluates and reads);
    `TestScheduledFiringStampsItsTransition`;
    `TestBoundaryVersionCarriesRequestTransition`;
    `TestChangeHistory_CollectionSameEntityTwice_LastItemWins` and
    `TestChangeHistory_FailureAfterBoundary_KeepsBoundaryVersionName`.
  - `approval_recipe_test.go`: `TestApprovalRecipe`.
- gRPC (`internal/grpc`): `TestEntityChangesMetadata_Transition`
  (`entity_change_transition_test.go`) and
  `TestCriterionSeesStartVersionTransition_EachGRPCDoor`
  (`entity_transition_name_test.go`).
- Parity (`e2e/parity/registry.go`, memory/sqlite/postgres):
  `HistoryTransitionPerVersion` and `CriterionSeesStartVersionTransition`.
- SPI conformance (`spitest`): `Entity/GetVersionMetadata/TransitionPerVersion`,
  `Entity/GetVersionMetadata/TombstoneTransitionEmpty` and
  `Entity/OneVersionPerTx/TransitionIsLastWrite`.
