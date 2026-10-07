-- Reverse of 000017_function_search_path.up.sql: the two functions as
-- migrations 000002 and 000005 created them, copied verbatim.
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
