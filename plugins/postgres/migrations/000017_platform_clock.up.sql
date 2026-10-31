-- Platform clock. A test clock supplies its own time to the two stamp
-- functions; production supplies none and keeps the database clock.
--
-- cyoda_stamp and cyoda_consistency_time take a last argument at_us, an
-- instant in microseconds since the epoch, DEFAULT NULL. NULL — every call in
-- production — reads clock_timestamp() exactly as migration 000016 did. A
-- value stands in for that reading and nothing else: the floor, the locks, the
-- timeouts and the wait budget's deadline (still on clock_timestamp(), since it
-- bounds a physical wait) are unchanged. A different argument list makes a new
-- function rather than replacing the old one, so the old signatures are
-- dropped first and exactly one function of each name remains.
--
-- The placeholder dates lose their DEFAULT CURRENT_TIMESTAMP:
-- entities.creation_date and last_modified, entity_versions.transaction_time
-- and creation_date. Every insert writes them from the plugin's placeholder
-- value, which is the business clock's time under a test clock; a default
-- would put the database clock there instead without a word.
--
-- search_jobs.heartbeat_time becomes NOT NULL: CreateJob stamps it, and stale
-- reclaim measures from it alone. A job written before that has none; it is
-- given its created_at, the baseline it was measured from until now.
--
-- Two dead objects go: the DEFAULT on search_jobs.created_at, which every
-- insert supplies, and model_schema_extensions.created_at, which nothing
-- writes or reads.
DROP FUNCTION cyoda_stamp(int4);
DROP FUNCTION cyoda_consistency_time(int4, bigint);

CREATE FUNCTION cyoda_stamp(tenant_key int4, at_us bigint DEFAULT NULL) RETURNS timestamptz LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE cur_idle bigint;
  xkey int4 := ((pg_current_xact_id()::text::bigint % 2147483647) + 1)::int4; s bigint; held boolean := false;
BEGIN
  PERFORM set_config('lock_timeout','2000ms',true);
  SELECT setting::bigint INTO cur_idle FROM pg_settings WHERE name='idle_in_transaction_session_timeout';
  PERFORM set_config('idle_in_transaction_session_timeout',
    (CASE WHEN cur_idle=0 THEN 5000 ELSE least(cur_idle,5000) END)::text||'ms', true);
  PERFORM pg_advisory_xact_lock(tenant_key, xkey);
  BEGIN
    held := true; PERFORM pg_advisory_lock(0,0);
    SELECT greatest(coalesce(at_us, (extract(epoch FROM clock_timestamp())*1000000)::bigint), last_value+1) INTO s FROM cyoda_stamp_floor;
    PERFORM setval('cyoda_stamp_floor', s, true);
    PERFORM pg_advisory_unlock(0,0); held := false;
  EXCEPTION WHEN query_canceled OR OTHERS THEN
    IF held THEN PERFORM pg_advisory_unlock(0,0); END IF; RAISE;
  END;
  RETURN 'epoch'::timestamptz + s * interval '1 microsecond';
END $$;

CREATE FUNCTION cyoda_consistency_time(tenant_key int4, wait_budget_ms bigint, at_us bigint DEFAULT NULL) RETURNS timestamptz LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE deadline timestamptz := clock_timestamp() + wait_budget_ms * interval '1 millisecond';
  c bigint; held boolean := false; k oid; rem bigint;
BEGIN
  PERFORM set_config('lock_timeout', greatest(wait_budget_ms,1)::text||'ms', true);
  BEGIN
    held := true; PERFORM pg_advisory_lock(0,0);
    SELECT greatest(coalesce(at_us, (extract(epoch FROM clock_timestamp())*1000000)::bigint), last_value) INTO c FROM cyoda_stamp_floor;
    PERFORM setval('cyoda_stamp_floor', c, true);
    PERFORM pg_advisory_unlock(0,0); held := false;
  EXCEPTION WHEN query_canceled OR OTHERS THEN
    IF held THEN PERFORM pg_advisory_unlock(0,0); END IF; RAISE;
  END;
  FOR k IN SELECT objid FROM pg_locks WHERE locktype='advisory'
      AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
      AND classid=tenant_key AND objsubid=2 AND objid BETWEEN 1 AND 2147483647 AND mode='ExclusiveLock' AND granted LOOP
    IF clock_timestamp() >= deadline THEN
      RAISE EXCEPTION 'consistency time wait budget exhausted' USING ERRCODE='55P03'; END IF;
    rem := ceil(extract(epoch FROM deadline - clock_timestamp())*1000)::bigint;
    PERFORM set_config('lock_timeout', greatest(rem,1)::text||'ms', true);
    PERFORM pg_advisory_xact_lock_shared(tenant_key, k::bigint::int4);
  END LOOP;
  RETURN 'epoch'::timestamptz + c * interval '1 microsecond';
END $$;

ALTER TABLE entities ALTER COLUMN creation_date DROP DEFAULT,
                     ALTER COLUMN last_modified DROP DEFAULT;
ALTER TABLE entity_versions ALTER COLUMN transaction_time DROP DEFAULT,
                            ALTER COLUMN creation_date DROP DEFAULT;

UPDATE search_jobs SET heartbeat_time = created_at WHERE heartbeat_time IS NULL;
ALTER TABLE search_jobs ALTER COLUMN heartbeat_time SET NOT NULL;

ALTER TABLE search_jobs ALTER COLUMN created_at DROP DEFAULT;
ALTER TABLE model_schema_extensions DROP COLUMN created_at;
