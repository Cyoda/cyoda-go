# Audit events: identity, order and cursor — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's audit event API. cyoda-go is the authoritative implementation; the
behaviour described here is derived directly from its design spec and
implemented code.

## 1. What changes on the wire

Two required fields are added to the two audit event DTOs `GET
/audit/entity/{entityId}` and `GET
/audit/entity/{entityId}/workflow/{transactionId}/finished` return:

- **`EntityChangeAuditEventDto.version`** — `integer`, `int64`. The entity's
  version number for that change, strictly increasing over the entity's whole
  history, including across a delete and a later recreate. `(entityId,
  version)` identifies one entity change event; two entity change events of
  the same transaction are otherwise identical on the wire and cannot be told
  apart.
- **`StateMachineAuditEventDto.eventId`** — `string`, `format: uuid`. A
  time-based UUID assigned once, when the event is recorded. Unique per
  event and the same value on every read, on both endpoints that return a
  state machine event.

Both fields are required on every response; neither is a new filter or query
parameter.

## 2. Order

Both platforms sort the merged entity-change / state-machine list by:

1. commit instant DESC (`utcTime` / `microsTime` in cyoda-go);
2. event type ASC — EntityChange before StateMachine.

Cloud already applies these two
(`backend/.../audit/EntityAuditInteractor.kt:281-293`). cyoda-go adds two more
levels beneath them, needed because levels 1–2 alone leave every event of one
transaction, and every state-machine event of one instant, in an order that
is not stable across requests:

3. entity change events: `version` DESC;
4. state machine events: the time field of `eventId` DESC, then the UUID's
   16 bytes DESC.

Every event has a distinct key at level 3 or 4, so the full order is total
and reproduces on every request over the same data. For state machine events
of one instant it is fixed but not the recording order: two version-1 UUIDs
minted in the same clock tick differ only in a wrapping clock-sequence field,
and a clock step backwards can invert two ids. The contract is
"deterministic", not "recording order".

## 3. Cursor

`nextCursor` becomes a position: the sort key (commit instant, event type,
and `version` or `eventId`) of the last event on the page, opaque on the
wire. The next page is every event strictly after that key under the same
comparator as the order in §2. Filters may change between pages — the walk
continues from the position under the new filters, not from the start.

A cursor that does not decode, or decodes but fails validation (unknown
type, a non-UUID `eventId`, a `version` below 1, a missing field), answers
`400 BAD_REQUEST`. An offset cursor from an earlier build no longer decodes
and answers `400` the same way — cyoda-go declares this a breaking change
rather than accepting the old shape.

Cloud's cursor is already base64 JSON of a per-source offset, which is the
same shifting-window construction cyoda-go is moving away from: an offset
counted from the front of a list that can change between requests skips or
repeats rows when the underlying data moves. Cloud should move to the same
position-cursor construction for the same reason cyoda-go did.

## 4. The store-owned event id

`eventId` (cyoda-go) and `timeUuid` (Cloud) are the same kind of value: an id
the platform assigns once, at record time, that the caller never supplies
and that comes back unchanged on every later read. Cloud already generates
it this way, in the engine's event factory rather than the DAO, and uses it
as the Cassandra clustering key. Either generation point — engine or store —
is acceptable as long as the id is unique, stable across reads, and is the
value the read path returns; the two platforms do not need to agree on
where in the stack it is minted.

## 5. What Cloud must do

- Add `version` to the entity-change event DTO, populated from the entity
  change's version (transaction) number.
- Expose the existing `timeUuid` as `eventId` on the state-machine event DTO,
  on both the search endpoint and the finished-event endpoint.
- Add the `version` / `eventId` tie-break (§2, levels 3–4) beneath the
  existing commit-instant / event-type order.
- Move the cursor from a per-source offset to a position cursor (§3),
  encoding the same sort key the order now uses, and reject an unreadable or
  invalid cursor rather than restarting the walk silently.
- Apply the same tie-break to the finished-event endpoint when a transaction
  has more than one finished event for the entity (§6).

## 6. The finished-event endpoint's tie-break

A transaction can carry more than one `STATE_MACHINE_FINISH` event for the
same entity: a joined callback's loopback save emits its own START/FINISH
pair when it updates an entity whose workflow already ran earlier in the
same transaction. `GET
/audit/entity/{entityId}/workflow/{transactionId}/finished` picks among
them the one that sorts first under the §2 order — normally the last one
recorded, but not promised to be, since §2 does not promise recording order
for state machine events of one instant. Cloud should apply the same §2
comparator when more than one finished event exists for a transaction,
rather than returning whichever one its store lists first.
