ALTER TABLE search_job_results DROP CONSTRAINT search_job_results_pkey;
ALTER TABLE search_job_results ADD CONSTRAINT search_job_results_pkey
    PRIMARY KEY (job_id, seq);
