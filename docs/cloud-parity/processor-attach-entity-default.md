# Processor `config.attachEntity` default

## Contract

On workflow import, a processor whose `config` omits `attachEntity` resolves to
`attachEntity: true` — the full entity payload is attached to its
`EntityProcessorCalculationRequest`. An explicit `false` or `true` is preserved
verbatim. The default is applied at the import boundary, so a re-export stamps
the resolved value explicitly.

This makes all three compute-node callout shapes agree on an omitted
`attachEntity`:

| Callout | Config shape | Omitted `attachEntity` |
|---|---|---|
| Processor | `processors[].config` | **true** |
| Scheduled-transition function | `schedule.function` | **true** |
| Criterion function | `{"type":"function"}.config` | **true** |

## Cloud obligation

Resolve an omitted processor `config.attachEntity` to `true` at import,
identically to the function callouts. A payload that already sets the field
explicitly is unaffected.
