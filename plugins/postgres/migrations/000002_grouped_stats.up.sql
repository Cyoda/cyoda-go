-- Grouped entity statistics — supporting objects for #299.
--
-- D21 (revised): PL/pgSQL try-cast for float8 with a regex pre-filter fast path
-- and a narrow EXCEPTION safety net.
--
-- Why PL/pgSQL despite the per-row subtransaction concern: the original v3 spec
-- claimed a pure-SQL CASE+NULLIF would handle overflow by stripping Infinity,
-- but postgres's `text::float8` cast raises SQLSTATE 22003 on overflow
-- (verified: SELECT '1e500'::float8 → ERROR), not Infinity. So a pure-SQL form
-- cannot be total.
--
-- The hybrid is acceptable because:
--   - Regex pre-filter rejects obviously-non-numeric inputs (empty, "abc",
--     "NaN", "Infinity", whitespace) BEFORE the cast — no subtransaction.
--   - Valid numeric inputs pass the regex AND cast cleanly — no subtransaction.
--   - Only regex-passes-but-cast-overflows hits the EXCEPTION block. For clean
--     data this never fires.
--
-- Regex uses \A and \Z anchors (not ^/$) so trailing newline doesn't sneak past.
CREATE OR REPLACE FUNCTION cyoda_try_float8(t text) RETURNS float8 AS $$
DECLARE
  result float8;
BEGIN
  IF t IS NULL OR t !~ '\A-?[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]+)?\Z' THEN
    RETURN NULL;
  END IF;
  BEGIN
    result := t::float8;
  EXCEPTION
    WHEN numeric_value_out_of_range OR invalid_text_representation THEN
      RETURN NULL;
  END;
  IF result = 'Infinity'::float8 OR result = '-Infinity'::float8 OR result = 'NaN'::float8 THEN
    RETURN NULL;
  END IF;
  RETURN result;
END;
$$ LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE;

-- D19: Canonical state expression index. Partial on NOT deleted to keep
-- the index small and exclude tombstones — matches the predicate already
-- used by idx_entities_model.
CREATE INDEX IF NOT EXISTS entities_state_idx
    ON entities (tenant_id, model_name, model_version, (doc->'_meta'->>'state'))
    WHERE NOT deleted;
