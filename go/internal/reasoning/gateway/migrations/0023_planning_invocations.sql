ALTER TABLE reasoning_invocations
    DROP CONSTRAINT reasoning_invocations_stage_check;

ALTER TABLE reasoning_invocations
    ADD CONSTRAINT reasoning_invocations_stage_check
    CHECK (stage IN ('specification', 'planning', 'implementation', 'review'))
    NOT VALID;

ALTER TABLE reasoning_invocations
    ADD CONSTRAINT reasoning_invocations_planning_task_check
    CHECK (stage <> 'planning' OR task_id IS NULL)
    NOT VALID;
