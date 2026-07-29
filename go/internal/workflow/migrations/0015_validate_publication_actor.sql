ALTER TABLE workflow_commands
    VALIDATE CONSTRAINT workflow_commands_actor_kind_closed;

ALTER TABLE workflow_events
    VALIDATE CONSTRAINT workflow_events_actor_kind_closed;
