CREATE UNIQUE INDEX runtime_stage_jobs_one_run_stage
    ON runtime_stage_jobs (run_id, stage)
    WHERE task_id IS NULL;

ALTER TABLE runtime_stage_jobs
    ADD CONSTRAINT runtime_run_reasoning_job_shape
    CHECK (
        (stage IN ('specification_reasoning', 'planning_reasoning') AND task_id IS NULL)
        OR
        (stage NOT IN ('specification_reasoning', 'planning_reasoning'))
    )
    NOT VALID;

ALTER TABLE runtime_stage_jobs
    VALIDATE CONSTRAINT runtime_run_reasoning_job_shape;
