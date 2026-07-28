ALTER TABLE workflow_runs
    ADD COLUMN lifecycle_bindings jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT workflow_runs_candidate_commit CHECK (
        lifecycle_bindings->>'candidate_commit' IS NULL
        OR lifecycle_bindings->>'candidate_commit' ~ '^[a-f0-9]{40}$'
    ) NOT VALID;

ALTER TABLE workflow_runs
    VALIDATE CONSTRAINT workflow_runs_candidate_commit;
