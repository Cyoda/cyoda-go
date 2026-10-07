-- The plugin's helper functions name everything in their bodies with
-- pg_catalog.
--
-- cyoda_try_float8 (000002) and cyoda_epoch_millis (000005) were created with
-- unqualified names in their bodies and no search_path of their own, so the
-- functions, operators and types they use resolved through the caller's
-- search_path on every call. A role that may create objects in a schema on
-- that path could plant one with a better or, when the path names that schema
-- before pg_catalog, an equal match — *(numeric, integer) beats pg_catalog's
-- *(numeric, numeric) for extract(...) * 1000, and a domain named float8 or
-- timestamptz runs its check on every cast — and the plugin's searches would
-- run it with the plugin's privileges.
--
-- They are recreated in place (CREATE OR REPLACE keeps each function's OID)
-- with the same signature, volatility, strictness, parallel safety, cost and
-- logic; only the names change: pg_catalog.floor, OPERATOR(pg_catalog.x) for
-- each operator, and pg_catalog types for the declarations and casts.
-- extract(... FROM ...) is grammar that PostgreSQL resolves in pg_catalog
-- itself, and bigint is a keyword for pg_catalog.int8. There is no SET
-- search_path clause: it would save and restore the setting on every call,
-- that is on every row of a sort or grouped stat, and the qualified names
-- already make the bodies independent of the caller's path. Nothing in the
-- schema depends on either function (no index, view, default or constraint).
CREATE OR REPLACE FUNCTION cyoda_try_float8(t pg_catalog.text) RETURNS pg_catalog.float8 AS $$
DECLARE
  result pg_catalog.float8;
BEGIN
  IF t IS NULL OR t OPERATOR(pg_catalog.!~) '\A-?[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]+)?\Z' THEN
    RETURN NULL;
  END IF;
  BEGIN
    result := t::pg_catalog.float8;
  EXCEPTION
    WHEN numeric_value_out_of_range OR invalid_text_representation THEN
      RETURN NULL;
  END;
  IF result OPERATOR(pg_catalog.=) 'Infinity'::pg_catalog.float8 OR result OPERATOR(pg_catalog.=) '-Infinity'::pg_catalog.float8 OR result OPERATOR(pg_catalog.=) 'NaN'::pg_catalog.float8 THEN
    RETURN NULL;
  END IF;
  RETURN result;
END;
$$ LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE;

CREATE OR REPLACE FUNCTION cyoda_epoch_millis(t pg_catalog.text) RETURNS bigint AS $$
DECLARE
  result bigint;
BEGIN
  IF t IS NULL OR t OPERATOR(pg_catalog.!~) '\A\d{4}-\d{2}-\d{2}T.+(Z|[+-]\d{2}:?\d{2})\Z' THEN
    RETURN NULL;
  END IF;
  BEGIN
    result := pg_catalog.floor(extract(epoch from t::pg_catalog.timestamptz) OPERATOR(pg_catalog.*) 1000)::bigint;
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
