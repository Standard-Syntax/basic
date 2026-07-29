ALTER TABLE reasoning_invocations
    DROP CONSTRAINT reasoning_invocations_stage_check;

ALTER TABLE reasoning_invocations
    ADD CONSTRAINT reasoning_invocations_stage_check
    CHECK (stage IN ('implementation', 'review'));
