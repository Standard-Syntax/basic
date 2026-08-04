ALTER TABLE reasoning_invocations
    VALIDATE CONSTRAINT reasoning_invocations_stage_check;

ALTER TABLE reasoning_invocations
    VALIDATE CONSTRAINT reasoning_invocations_specification_task_check;

ALTER TABLE reasoning_invocations
    VALIDATE CONSTRAINT reasoning_invocations_planning_task_check;
