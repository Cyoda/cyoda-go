-- Consistency time. cyoda_stamp_floor holds the highest stamp or consistency
-- time ever issued, in microseconds since the epoch. Every commit calls
-- cyoda_stamp, which takes a transaction-level advisory lock
-- (tenant_key, xact_key) — the in-flight marker, released after the commit's
-- rows are visible — and stamps above the floor. cyoda_consistency_time
-- reserves C above every stamp issued, then waits for the tenant's markers.
-- Advisory key layout (two-int form, objsubid = 2, used by nothing else here):
--   (0, 0)                     the floor mutex, held for microseconds
--   (tenant_key, 1..2^31-1)    in-flight markers, one per committing tx
-- tenant_key is the tenant's row in consistency_tenant_keys: allocated from its
-- own sequence starting at 1 and checked positive, so it is never 0 and unique
-- by construction (a hash of the tenant id would let two tenants share
-- markers, and so delay each other and see each other's commit timing). Rows
-- are never deleted. Each store factory (with its transaction manager) caches
-- a tenant's key after its first lookup, made in a short READ COMMITTED
-- transaction of its own that sets app.current_tenant, before any commit
-- phase, so a stamping transaction never touches the table. Row-level security
-- as on every tenant-scoped table.
-- The floor is seeded from the stamps already stored. search_jobs.point_in_time
-- is not one: it held the caller's pointInTime as sent, with no check against
-- the future, so seeding from it could move every later stamp far ahead.
-- Both functions are SECURITY DEFINER: they run with the privileges of the
-- role that ran this migration, so the runtime role needs no privilege on
-- cyoda_stamp_floor and cannot set or advance it itself. EXECUTE stays granted
-- to PUBLIC: this migration cannot know the runtime role's name, and neither
-- function can move the floor anywhere but along the clock (cyoda_stamp to
-- greatest(clock, floor + 1), cyoda_consistency_time to greatest(clock,
-- floor)), so a caller gains nothing it could not get by committing or by
-- asking for a consistency time.
-- Because they run as the owner, nothing they call may be resolvable in a
-- schema another role can write to. Their search_path is exactly
-- pg_catalog, pg_temp: every function, operator, type and catalog they use
-- resolves in pg_catalog (functions and operators are never looked up in
-- pg_temp, and pg_catalog comes first for relations and types). The one
-- plugin object they touch, the floor sequence, is named with the schema this
-- migration runs in, which is current_schema() and not necessarily public, so
-- both functions are created by EXECUTE format(...) with that schema filled
-- in (%% in the template is format's escape for the modulo operator).
CREATE SEQUENCE cyoda_stamp_floor AS bigint MINVALUE 0 START 0;
SELECT setval('cyoda_stamp_floor', coalesce(greatest(
  (SELECT (extract(epoch FROM max(transaction_time))*1000000)::bigint FROM entity_versions),
  (SELECT (extract(epoch FROM max(submit_time))*1000000)::bigint FROM submit_times)),0), true);

CREATE SEQUENCE consistency_tenant_key_seq AS int4 MINVALUE 1 START 1;
CREATE TABLE consistency_tenant_keys (
  tenant_id  text PRIMARY KEY,
  tenant_key int4 NOT NULL UNIQUE DEFAULT nextval('consistency_tenant_key_seq') CHECK (tenant_key > 0));
ALTER SEQUENCE consistency_tenant_key_seq OWNED BY consistency_tenant_keys.tenant_key;
ALTER TABLE consistency_tenant_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY consistency_tenant_keys_tenant_isolation ON consistency_tenant_keys
  USING (tenant_id = current_setting('app.current_tenant', true));

DO $do$ BEGIN
EXECUTE format($f$CREATE FUNCTION %1$I.cyoda_stamp(tenant_key int4) RETURNS timestamptz LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $body$
DECLARE cur_idle bigint;
  xkey int4 := ((pg_current_xact_id()::text::bigint %% 2147483647) + 1)::int4; s bigint; held boolean := false;
BEGIN
  PERFORM set_config('lock_timeout','2000ms',true);
  SELECT setting::bigint INTO cur_idle FROM pg_settings WHERE name='idle_in_transaction_session_timeout';
  PERFORM set_config('idle_in_transaction_session_timeout',
    (CASE WHEN cur_idle=0 THEN 5000 ELSE least(cur_idle,5000) END)::text||'ms', true);
  PERFORM pg_advisory_xact_lock(tenant_key, xkey);
  BEGIN
    held := true; PERFORM pg_advisory_lock(0,0);
    SELECT greatest((extract(epoch FROM clock_timestamp())*1000000)::bigint, last_value+1) INTO s FROM %1$I.cyoda_stamp_floor;
    PERFORM setval(%2$L, s, true);
    PERFORM pg_advisory_unlock(0,0); held := false;
  EXCEPTION WHEN query_canceled OR OTHERS THEN
    IF held THEN PERFORM pg_advisory_unlock(0,0); END IF; RAISE;
  END;
  RETURN 'epoch'::timestamptz + s * interval '1 microsecond';
END $body$$f$, current_schema(), quote_ident(current_schema()) || '.cyoda_stamp_floor');

EXECUTE format($f$CREATE FUNCTION %1$I.cyoda_consistency_time(tenant_key int4, wait_budget_ms bigint) RETURNS timestamptz LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $body$
DECLARE deadline timestamptz := clock_timestamp() + wait_budget_ms * interval '1 millisecond';
  c bigint; held boolean := false; k oid; rem bigint;
BEGIN
  PERFORM set_config('lock_timeout', greatest(wait_budget_ms,1)::text||'ms', true);
  BEGIN
    held := true; PERFORM pg_advisory_lock(0,0);
    SELECT greatest((extract(epoch FROM clock_timestamp())*1000000)::bigint, last_value) INTO c FROM %1$I.cyoda_stamp_floor;
    PERFORM setval(%2$L, c, true);
    PERFORM pg_advisory_unlock(0,0); held := false;
  EXCEPTION WHEN query_canceled OR OTHERS THEN
    IF held THEN PERFORM pg_advisory_unlock(0,0); END IF; RAISE;
  END;
  FOR k IN SELECT objid FROM pg_locks WHERE locktype='advisory'
      AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
      AND classid=tenant_key AND objsubid=2 AND objid<>0 AND mode='ExclusiveLock' AND granted LOOP
    IF clock_timestamp() >= deadline THEN
      RAISE EXCEPTION 'consistency time wait budget exhausted' USING ERRCODE='55P03'; END IF;
    rem := ceil(extract(epoch FROM deadline - clock_timestamp())*1000)::bigint;
    PERFORM set_config('lock_timeout', greatest(rem,1)::text||'ms', true);
    PERFORM pg_advisory_xact_lock_shared(tenant_key, k::bigint::int4);
  END LOOP;
  RETURN 'epoch'::timestamptz + c * interval '1 microsecond';
END $body$$f$, current_schema(), quote_ident(current_schema()) || '.cyoda_stamp_floor');
END $do$;
