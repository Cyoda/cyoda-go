-- Consistency time. cyoda_stamp_floor holds the highest stamp or consistency
-- time ever issued, in microseconds since the epoch. Every commit calls
-- cyoda_stamp, which takes a transaction-level advisory lock
-- (hashtext(tenant), xact_key) — the in-flight marker, released after the
-- commit's rows are visible — and stamps above the floor. cyoda_consistency_time
-- reserves C above every stamp issued, then waits for the tenant's markers.
-- Advisory key layout (two-int form, objsubid = 2, used by nothing else here):
--   (0, 0)                         the floor mutex, held for microseconds
--   (hashtext(tenant), 1..2^31-1)  in-flight markers, one per committing tx
-- The floor is seeded from the stamps already stored. search_jobs.point_in_time
-- is not one: it held the caller's pointInTime as sent, with no check against
-- the future, so seeding from it could move every later stamp far ahead.
CREATE SEQUENCE cyoda_stamp_floor AS bigint MINVALUE 0 START 0;
SELECT setval('cyoda_stamp_floor', coalesce(greatest(
  (SELECT (extract(epoch FROM max(transaction_time))*1000000)::bigint FROM entity_versions),
  (SELECT (extract(epoch FROM max(submit_time))*1000000)::bigint FROM submit_times)),0), true);

CREATE FUNCTION cyoda_stamp(tenant text) RETURNS timestamptz LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE cur_idle bigint; tkey int4 := hashtext(tenant);
  xkey int4 := ((pg_current_xact_id()::text::bigint % 2147483647) + 1)::int4; s bigint; held boolean := false;
BEGIN
  PERFORM set_config('lock_timeout','2000ms',true);
  SELECT setting::bigint INTO cur_idle FROM pg_settings WHERE name='idle_in_transaction_session_timeout';
  PERFORM set_config('idle_in_transaction_session_timeout',
    (CASE WHEN cur_idle=0 THEN 5000 ELSE least(cur_idle,5000) END)::text||'ms', true);
  PERFORM pg_advisory_xact_lock(tkey, xkey);
  BEGIN
    held := true; PERFORM pg_advisory_lock(0,0);
    SELECT greatest((extract(epoch FROM clock_timestamp())*1000000)::bigint, last_value+1) INTO s FROM cyoda_stamp_floor;
    PERFORM setval('cyoda_stamp_floor', s, true);
    PERFORM pg_advisory_unlock(0,0); held := false;
  EXCEPTION WHEN query_canceled OR OTHERS THEN
    IF held THEN PERFORM pg_advisory_unlock(0,0); END IF; RAISE;
  END;
  RETURN 'epoch'::timestamptz + s * interval '1 microsecond';
END $$;

CREATE FUNCTION cyoda_consistency_time(tenant text, wait_budget_ms bigint) RETURNS timestamptz LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE deadline timestamptz := clock_timestamp() + wait_budget_ms * interval '1 millisecond';
  tkey int4 := hashtext(tenant); c bigint; held boolean := false; k oid; rem bigint;
BEGIN
  PERFORM set_config('lock_timeout', greatest(wait_budget_ms,1)::text||'ms', true);
  BEGIN
    held := true; PERFORM pg_advisory_lock(0,0);
    SELECT greatest((extract(epoch FROM clock_timestamp())*1000000)::bigint, last_value) INTO c FROM cyoda_stamp_floor;
    PERFORM setval('cyoda_stamp_floor', c, true);
    PERFORM pg_advisory_unlock(0,0); held := false;
  EXCEPTION WHEN query_canceled OR OTHERS THEN
    IF held THEN PERFORM pg_advisory_unlock(0,0); END IF; RAISE;
  END;
  FOR k IN SELECT objid FROM pg_locks WHERE locktype='advisory'
      AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
      AND classid=tkey AND objsubid=2 AND objid<>0 AND mode='ExclusiveLock' AND granted LOOP
    IF clock_timestamp() >= deadline THEN
      RAISE EXCEPTION 'consistency time wait budget exhausted' USING ERRCODE='55P03'; END IF;
    rem := ceil(extract(epoch FROM deadline - clock_timestamp())*1000)::bigint;
    PERFORM set_config('lock_timeout', greatest(rem,1)::text||'ms', true);
    PERFORM pg_advisory_xact_lock_shared(tkey, k::bigint::int4);
  END LOOP;
  RETURN 'epoch'::timestamptz + c * interval '1 microsecond';
END $$;
