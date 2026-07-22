-- Temporal search filters (#423) — offset-mandatory RFC3339 -> epoch-ms.
--
-- cyoda_epoch_millis converts a stored RFC3339 instant (as found under
-- doc->'_meta'->>'creation_date' etc.) to floored epoch-milliseconds so
-- chronological comparisons (creationDate/lastUpdateTime filters and sorts)
-- compare as integers rather than lexicographic text.
--
-- Offset-mandatory by design: the regex guard requires an explicit Z or
-- ±hh:mm offset. An offset-less local timestamp has no single absolute
-- instant, so making the function total over such input would require
-- picking an implicit zone (wrong-but-available) — instead it returns NULL,
-- consistent with the fail-closed temporal-scalar rule in spi.ParseTemporalMillis.
-- The mandatory offset is also what makes this function safe to mark
-- IMMUTABLE (the same text always maps to the same instant, independent of
-- session timezone).
--
-- Total over text input (NULL on non-match or cast failure) so it can be
-- used directly in a WHERE clause without per-row validation, mirroring
-- cyoda_try_float8 (migration 000002).
CREATE OR REPLACE FUNCTION cyoda_epoch_millis(t text) RETURNS bigint AS $$
DECLARE
  result bigint;
BEGIN
  IF t IS NULL OR t !~ '\A\d{4}-\d{2}-\d{2}T.+(Z|[+-]\d{2}:?\d{2})\Z' THEN
    RETURN NULL;
  END IF;
  BEGIN
    result := floor(extract(epoch from t::timestamptz) * 1000)::bigint;
  -- Broad catch (vs. cyoda_try_float8's narrower exception classes): the regex
  -- above is a weaker pre-filter than float8's, so timezone/format variants
  -- that pass it can still fail the cast in more ways; NULL-on-any-failure is
  -- this function's intended total-function contract, not a specific class.
  EXCEPTION WHEN others THEN
    RETURN NULL;
  END;
  RETURN result;
END;
$$ LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE;
