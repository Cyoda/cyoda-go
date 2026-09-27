-- Every search job has a point in time. spi.SearchJob.PointInTime is a plain
-- time.Time and CreateJob always writes it; the column was declared nullable
-- in 000001 on the mistaken note that it was optional. The database now
-- refuses a row without one. ALTER TABLE takes ACCESS EXCLUSIVE on
-- search_jobs for the verifying scan; the table holds one row per job.
ALTER TABLE search_jobs ALTER COLUMN point_in_time SET NOT NULL;
