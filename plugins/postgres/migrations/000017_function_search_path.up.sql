-- The plugin's helper functions run with a fixed search_path.
--
-- cyoda_try_float8 (000002) and cyoda_epoch_millis (000005) were created with
-- no search_path of their own, so the operators their bodies use (!~, =, *)
-- resolved through the caller's search_path on every call. A role that may
-- create objects in a schema on that path could plant an operator with a
-- better or, when the path names that schema before pg_catalog, an equal
-- argument match — *(numeric, integer) beats pg_catalog's *(numeric, numeric)
-- for extract(...) * 1000 — and the plugin's searches would run it with the
-- plugin's privileges.
--
-- Both bodies use only built-in functions, operators and types, so
-- pg_catalog, pg_temp is their whole path: functions and operators are never
-- looked up in pg_temp, and pg_catalog comes before it for types.
ALTER FUNCTION cyoda_try_float8(pg_catalog.text) SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION cyoda_epoch_millis(pg_catalog.text) SET search_path = pg_catalog, pg_temp;
