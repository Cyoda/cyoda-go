# Event schemas compose with `allOf`

## Contract

Every document in the event schema tree (`docs/cyoda/schema/`) declares

```json
"$schema": "https://json-schema.org/draft/2020-12/schema"
```

and the 47 that build on `BaseEvent` compose with the keyword that dialect
reads:

```json
"allOf": [
  {
    "$ref": "../common/BaseEvent.json"
  }
]
```

They previously composed with `extends`. That is a **draft-03** keyword, dropped
in draft-04 and replaced by `allOf`. Under 2020-12 an unrecognised keyword is
ignored, so each of those documents was valid and asserted nothing about the
base: a criteria response validated as its own three fields, not as a
`BaseEvent` plus three fields. Nothing checked `success`'s declared boolean
type, nothing applied `BaseEvent`'s `required` list, and nothing checked the
`error` object's shape — `{"success": "yes", "matches": true}` validated.

The composition now decides those three things. Nothing else in the documents
moved.

## What the tightening reaches

This is not an abstract statement about composition: it binds the three
calculation responses a compute member sends, and every other schema in the 47.
Concretely, against the published tree a response must now carry its own `id`
(`BaseEvent`'s `required`), must type `success` as a boolean, and must send
`error` as an object or not at all — an explicit `"error": null` no longer
validates. Alongside `id`, the three calculation responses have always required
`requestId` and `entityId` in their own right; what changes is that `id` joins
them, and that the other two checks now run at all.

**Payloads that were accepted before are still accepted.** Nothing validates
against the tree at run time, and the generated types already required `id` and
typed `success`, so a member whose answers work today keeps working. What
changes is that a member *validating* its answers against the published schema
— which is the point of publishing it — now gets those checks, and shapes that
passed a validator only because the composition was ignored will be flagged.
cyoda-go's own published examples were among them: `cyoda help grpc` showed
calculation responses without `id` and with `"error": null`, and they have been
corrected in the same change.

## What did not change

Nothing at runtime validates a payload against this tree — cyoda-go decodes in
Go, from types generated from it. `api/grpc/events/types.go` regenerates
**byte-identically** after the swap, so no wire type and no behaviour changed on
this side. The generated types already carried `BaseEvent`'s fields and treated
`id` as required; this is the published contract catching up with what the code
already did.

`scripts/generate-events.sh` keeps inlining the base's properties and required
list before handing the tree to `go-jsonschema`, now keyed off `allOf`.
`go-jsonschema` does resolve `allOf` itself, but into a different shape —
`BaseEvent` declared a second time, and the `error` object shared under the name
of whichever schema was read first — so the inlining stays.

## The two trees are no longer byte-identical

cyoda-go defines the integration contract and Cloud mirrors it, so until Cloud
follows, its copy of this tree differs from ours in these 47 files. That is a
known, temporary difference, not an accepted divergence.

## Cloud obligation

Compose with `allOf` in Cloud's copy of the tree, so the two are byte-identical
again.

The open question is entirely on Cloud's generator. `jsonschema2pojo` honours
the legacy keyword and turns it into Java inheritance — the generated
`EntityCriteriaCalculationResponse` literally `extends BaseEvent`, which is
where its `success` field comes from and what the `payload as BaseEvent` casts
in `ExternalizerBase` rely on. So:

1. If `jsonschema2pojo` produces the same inheritance from an `allOf` with a
   single `$ref`, the change is a keyword swap and the generated sources are
   unchanged.
2. If it does not, Cloud decides what shape gives both a conformant 2020-12
   document and the existing Java hierarchy — for example carrying both
   keywords, if the generator tolerates that without emitting the inherited
   fields twice — or adjusts the code that relied on the inheritance in the same
   change.

Either way the outcome leaves the two copies identical.
