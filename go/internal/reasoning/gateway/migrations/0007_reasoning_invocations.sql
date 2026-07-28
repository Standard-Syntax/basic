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
    provider text NOT NULL,
    model text NOT NULL,
    started_at timestamptz NOT NULL,
    completed_at timestamptz NOT NULL,
    input_tokens bigint NOT NULL CHECK (input_tokens >= 0),
    output_tokens bigint NOT NULL CHECK (output_tokens >= 0),
    provider_requests integer NOT NULL CHECK (provider_requests >= 0),
    final_status text NOT NULL CHECK (final_status IN ('accepted', 'rejected')),
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
        (final_status = 'accepted' AND proposal_digest IS NOT NULL
            AND rejection_code IS NULL)
        OR
        (final_status = 'rejected' AND rejection_code BETWEEN 1 AND 5
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
    RAISE EXCEPTION 'completed reasoning invocations are immutable';
END;
$$;

CREATE TRIGGER reasoning_invocations_immutable
BEFORE UPDATE OR DELETE ON reasoning_invocations
FOR EACH ROW EXECUTE FUNCTION reject_reasoning_invocation_mutation();

