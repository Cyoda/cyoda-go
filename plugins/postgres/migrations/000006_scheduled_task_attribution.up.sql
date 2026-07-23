-- armed_by_id/armed_by_kind carry the follow-on-action attribution's arming
-- principal (spi.ScheduledTask.ArmedBy — the chain origin at arm time). Two
-- flat columns rather than a JSON blob, matching every other scheduled_tasks
-- column. DEFAULT '' backfills existing rows so a pre-migration ("legacy")
-- task reads back as the zero Principal, never a synthesized one.
ALTER TABLE scheduled_tasks ADD COLUMN armed_by_id TEXT NOT NULL DEFAULT '';
ALTER TABLE scheduled_tasks ADD COLUMN armed_by_kind TEXT NOT NULL DEFAULT '';
