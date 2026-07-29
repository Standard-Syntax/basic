ALTER TABLE workflow_commands
    DROP CONSTRAINT workflow_commands_actor_kind_closed;
ALTER TABLE workflow_commands
    ADD CONSTRAINT workflow_commands_actor_kind_closed CHECK (
        actor_kind IN (
            'HUMAN','WORKFLOW_SERVICE','REASONING_SERVICE','EXECUTION_SERVICE',
            'VERIFICATION_SERVICE','REVIEW_SERVICE','PUBLICATION_SERVICE',
            'MERGE_SERVICE','PYTHON','MODEL'
        )
    ) NOT VALID;

ALTER TABLE workflow_events
    DROP CONSTRAINT workflow_events_actor_kind_closed;
ALTER TABLE workflow_events
    ADD CONSTRAINT workflow_events_actor_kind_closed CHECK (
        actor_kind IN (
            'HUMAN','WORKFLOW_SERVICE','REASONING_SERVICE','EXECUTION_SERVICE',
            'VERIFICATION_SERVICE','REVIEW_SERVICE','PUBLICATION_SERVICE',
            'MERGE_SERVICE','PYTHON','MODEL'
        )
    ) NOT VALID;
