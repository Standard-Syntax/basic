CREATE TABLE runtime_run_bindings (
    run_id uuid PRIMARY KEY REFERENCES workflow_runs(run_id),
    intake_uri text NOT NULL,
    intake_digest text NOT NULL CHECK (intake_digest ~ '^[a-f0-9]{64}$'),
    base_commit text NOT NULL CHECK (base_commit ~ '^[a-f0-9]{40}$'),
    repository_map_uri text NOT NULL,
    repository_map_digest text NOT NULL CHECK (repository_map_digest ~ '^[a-f0-9]{64}$'),
    approved_specification_uri text,
    approved_specification_digest text,
    approved_task_graph_uri text,
    approved_task_graph_digest text,
    composite_approval_uri text,
    composite_approval_digest text,
    created_at timestamptz NOT NULL,
    CHECK ((approved_specification_uri IS NULL) = (approved_specification_digest IS NULL)),
    CHECK ((approved_task_graph_uri IS NULL) = (approved_task_graph_digest IS NULL)),
    CHECK ((composite_approval_uri IS NULL) = (composite_approval_digest IS NULL))
);

CREATE TABLE runtime_task_bindings (
    task_id uuid PRIMARY KEY,
    run_id uuid NOT NULL REFERENCES workflow_runs(run_id),
    approved_task_uri text NOT NULL,
    approved_task_digest text NOT NULL CHECK (approved_task_digest ~ '^[a-f0-9]{64}$'),
    UNIQUE (run_id, task_id),
    FOREIGN KEY (run_id, task_id) REFERENCES workflow_tasks(run_id, task_id)
);

CREATE TABLE runtime_api_idempotency (
    idempotency_key uuid PRIMARY KEY,
    method text NOT NULL,
    target text NOT NULL,
    principal_id uuid NOT NULL,
    request_digest text NOT NULL CHECK (request_digest ~ '^[a-f0-9]{64}$'),
    status_code integer,
    response jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    CHECK ((status_code IS NULL) = (response IS NULL))
);

CREATE TABLE runtime_stage_jobs (
    job_id uuid PRIMARY KEY,
    run_id uuid NOT NULL REFERENCES workflow_runs(run_id),
    task_id uuid,
    attempt integer NOT NULL CHECK (attempt > 0),
    stage text NOT NULL,
    state text NOT NULL CHECK (state IN ('READY','CLAIMED','RETRY','COMPLETED','FAILED','CANCELLED')),
    available_at timestamptz NOT NULL,
    claim_owner uuid,
    claim_expires_at timestamptz,
    fencing_token bigint NOT NULL DEFAULT 0 CHECK (fencing_token >= 0),
    retry_count integer NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
    result_uri text,
    result_digest text,
    failure_uri text,
    failure_digest text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (run_id, task_id, attempt, stage),
    FOREIGN KEY (run_id, task_id) REFERENCES workflow_tasks(run_id, task_id),
    CHECK ((claim_owner IS NULL) = (claim_expires_at IS NULL)),
    CHECK ((result_uri IS NULL) = (result_digest IS NULL)),
    CHECK ((failure_uri IS NULL) = (failure_digest IS NULL))
);

CREATE INDEX runtime_stage_jobs_claim_idx
    ON runtime_stage_jobs(state, available_at, claim_expires_at);

CREATE FUNCTION protect_runtime_completed_job()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.state IN ('COMPLETED','FAILED','CANCELLED') THEN
        RAISE EXCEPTION 'terminal runtime jobs are immutable';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER runtime_stage_jobs_terminal_immutable
BEFORE UPDATE OR DELETE ON runtime_stage_jobs
FOR EACH ROW EXECUTE FUNCTION protect_runtime_completed_job();
