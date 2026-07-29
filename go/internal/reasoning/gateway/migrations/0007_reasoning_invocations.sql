CREATE TABLE reasoning_invocations (
    request_id text PRIMARY KEY,
    request_artifact_uri text NOT NULL,
    request_digest text NOT NULL CHECK (request_digest ~ '^[a-f0-9]{64}$'),
    run_id text NOT NULL,
    task_id text,
    stage text NOT NULL CHECK (stage = 'implementation'),
    attempt integer NOT NULL CHECK (attempt > 0),
    agent_manifest_digest text NOT NULL
        CHECK (agent_manifest_digest ~ '^[a-f0-9]{64}$'),
    proposal_artifact_uri text,
    proposal_digest text CHECK (proposal_digest ~ '^[a-f0-9]{64}$'),
    provider text,
    model text,
    started_at timestamptz NOT NULL,
    completed_at timestamptz,
    input_tokens bigint NOT NULL CHECK (input_tokens >= 0),
    output_tokens bigint NOT NULL CHECK (output_tokens >= 0),
    provider_requests integer NOT NULL CHECK (provider_requests >= 0),
    state text NOT NULL CHECK (state IN ('in_progress', 'completed')),
    final_status text CHECK (final_status IN ('accepted', 'rejected')),
    rejection_code integer,
    rejection_summary text,
    rejection_details jsonb,
    rejection_retryable boolean,
    rejection_timestamp timestamptz,
    CHECK (
        (proposal_artifact_uri IS NULL) =
        (proposal_digest IS NULL)
    ),
    CHECK (
        (state = 'in_progress' AND final_status IS NULL AND proposal_digest IS NULL
            AND provider IS NULL AND model IS NULL AND completed_at IS NULL
            AND input_tokens = 0 AND output_tokens = 0
            AND provider_requests = 0 AND rejection_code IS NULL
            AND rejection_summary IS NULL AND rejection_details IS NULL
            AND rejection_retryable IS NULL AND rejection_timestamp IS NULL)
        OR
        (state = 'completed' AND final_status = 'accepted'
            AND proposal_digest IS NOT NULL
            AND provider IS NOT NULL AND model IS NOT NULL
            AND completed_at IS NOT NULL AND rejection_code IS NULL
            AND rejection_summary IS NULL AND rejection_details IS NULL
            AND rejection_retryable IS NULL AND rejection_timestamp IS NULL)
        OR
        (state = 'completed' AND final_status = 'rejected'
            AND rejection_code BETWEEN 1 AND 5
            AND provider IS NOT NULL AND model IS NOT NULL
            AND completed_at IS NOT NULL
            AND rejection_summary IS NOT NULL
            AND rejection_details IS NOT NULL
            AND rejection_retryable IS NOT NULL
            AND rejection_timestamp IS NOT NULL)
    )
);

CREATE FUNCTION reject_reasoning_invocation_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.state = 'in_progress' THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        IF NEW.state = 'completed'
            AND NEW.final_status IN ('accepted', 'rejected')
            AND NEW.request_id = OLD.request_id
            AND NEW.request_artifact_uri = OLD.request_artifact_uri
            AND NEW.request_digest = OLD.request_digest
            AND NEW.run_id = OLD.run_id
            AND NEW.task_id IS NOT DISTINCT FROM OLD.task_id
            AND NEW.stage = OLD.stage
            AND NEW.attempt = OLD.attempt
            AND NEW.agent_manifest_digest = OLD.agent_manifest_digest
            AND NEW.started_at = OLD.started_at THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'completed reasoning invocations are immutable';
END;
$$;

CREATE TRIGGER reasoning_invocations_immutable
BEFORE UPDATE OR DELETE ON reasoning_invocations
FOR EACH ROW EXECUTE FUNCTION reject_reasoning_invocation_mutation();
