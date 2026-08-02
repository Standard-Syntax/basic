ALTER TABLE runtime_stage_jobs
ADD COLUMN transient_reschedule_count integer NOT NULL DEFAULT 0
CHECK (transient_reschedule_count >= 0);
