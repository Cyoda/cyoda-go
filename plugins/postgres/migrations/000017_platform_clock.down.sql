-- Restores migration 000016's stamp functions, the DEFAULT on
-- search_jobs.created_at and model_schema_extensions.created_at.
DROP FUNCTION cyoda_stamp(int4, bigint);
DROP FUNCTION cyoda_consistency_time(int4, bigint, bigint);

CREATE FUNCTION cyoda_stamp(tenant_key int4) RETURNS timestamptz LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
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
    SELECT greatest((extract(epoch FROM clock_timestamp())*1000000)::bigint, last_value+1) INTO s FROM cyoda_stamp_floor;
    PERFORM setval('cyoda_stamp_floor', s, true);
    PERFORM pg_advisory_unlock(0,0); held := false;
  EXCEPTION WHEN query_canceled OR OTHERS THEN
    IF held THEN PERFORM pg_advisory_unlock(0,0); END IF; RAISE;
  END;
  RETURN 'epoch'::timestamptz + s * interval '1 microsecond';
END $$;

CREATE FUNCTION cyoda_consistency_time(tenant_key int4, wait_budget_ms bigint) RETURNS timestamptz LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE deadline timestamptz := clock_timestamp() + wait_budget_ms * interval '1 millisecond';
  c bigint; held boolean := false; k oid; rem bigint;
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
      AND classid=tenant_key AND objsubid=2 AND objid BETWEEN 1 AND 2147483647 AND mode='ExclusiveLock' AND granted LOOP
    IF clock_timestamp() >= deadline THEN
      RAISE EXCEPTION 'consistency time wait budget exhausted' USING ERRCODE='55P03'; END IF;
    rem := ceil(extract(epoch FROM deadline - clock_timestamp())*1000)::bigint;
    PERFORM set_config('lock_timeout', greatest(rem,1)::text||'ms', true);
    PERFORM pg_advisory_xact_lock_shared(tenant_key, k::bigint::int4);
  END LOOP;
  RETURN 'epoch'::timestamptz + c * interval '1 microsecond';
END $$;

ALTER TABLE search_jobs ALTER COLUMN created_at SET DEFAULT now();
ALTER TABLE model_schema_extensions ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP;
