CREATE TABLE workflow_runs (
    run_id uuid PRIMARY KEY,
    state text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    specification_uri text,
    specification_digest text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT workflow_runs_specification_complete CHECK (
        (specification_uri IS NULL) = (specification_digest IS NULL)
    ),
    CONSTRAINT workflow_runs_specification_digest CHECK (
        specification_digest IS NULL OR specification_digest ~ '^[a-f0-9]{64}$'
    )
);

CREATE TABLE workflow_commands (
    command_id uuid PRIMARY KEY,
    aggregate_type text NOT NULL,
    aggregate_id uuid NOT NULL,
    request_digest text NOT NULL CHECK (request_digest ~ '^[a-f0-9]{64}$'),
    actor_id uuid NOT NULL,
    actor_kind text NOT NULL,
    expected_revision bigint NOT NULL CHECK (expected_revision >= 0),
    correlation_id uuid NOT NULL,
    causation_id uuid NOT NULL,
    requested_at timestamptz NOT NULL,
    completed_at timestamptz,
    result jsonb
);

CREATE TABLE workflow_events (
    sequence bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id uuid NOT NULL UNIQUE,
    command_id uuid NOT NULL REFERENCES workflow_commands(command_id),
    aggregate_type text NOT NULL,
    aggregate_id uuid NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    event_type text NOT NULL,
    occurred_at timestamptz NOT NULL,
    actor_id uuid NOT NULL,
    actor_kind text NOT NULL,
    correlation_id uuid NOT NULL,
    causation_id uuid NOT NULL,
    payload jsonb NOT NULL,
    UNIQUE (aggregate_type, aggregate_id, revision)
);

CREATE FUNCTION reject_workflow_event_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'workflow events are append-only';
END;
$$;

CREATE TRIGGER workflow_events_append_only
BEFORE UPDATE OR DELETE ON workflow_events
FOR EACH ROW EXECUTE FUNCTION reject_workflow_event_mutation();
