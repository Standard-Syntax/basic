CREATE TABLE task_approvals (
    approval_id text PRIMARY KEY,
    request_digest text NOT NULL CHECK (request_digest ~ '^[a-f0-9]{64}$'),
    requested_at timestamptz NOT NULL,
    principal_id text NOT NULL,
    run_id text NOT NULL,
    task_id text NOT NULL,
    candidate_commit text NOT NULL CHECK (candidate_commit ~ '^[a-f0-9]{40}$'),
    approved_specification_digest text NOT NULL
        CHECK (approved_specification_digest ~ '^[a-f0-9]{64}$'),
    approved_task_digest text NOT NULL
        CHECK (approved_task_digest ~ '^[a-f0-9]{64}$'),
    implementation_digest text NOT NULL CHECK (implementation_digest ~ '^[a-f0-9]{64}$'),
    execution_digest text NOT NULL CHECK (execution_digest ~ '^[a-f0-9]{64}$'),
    verification_digest text NOT NULL CHECK (verification_digest ~ '^[a-f0-9]{64}$'),
    review_digest text NOT NULL CHECK (review_digest ~ '^[a-f0-9]{64}$'),
    state text NOT NULL CHECK (state IN ('reserved', 'decision_ready', 'completed')),
    decision text CHECK (decision IN ('approve', 'rework')),
    decision_reason text,
    approval_artifact_uri text,
    approval_artifact_digest text CHECK (approval_artifact_digest ~ '^[a-f0-9]{64}$'),
    elevated boolean,
    risk_reasons jsonb,
    completed_at timestamptz,
    CHECK (
        (state = 'reserved' AND decision IS NULL AND decision_reason IS NULL
            AND approval_artifact_uri IS NULL AND approval_artifact_digest IS NULL
            AND elevated IS NULL AND risk_reasons IS NULL AND completed_at IS NULL)
        OR
        (state = 'decision_ready' AND decision IS NOT NULL AND decision_reason IS NOT NULL
            AND approval_artifact_uri IS NOT NULL AND approval_artifact_digest IS NOT NULL
            AND elevated IS NOT NULL AND jsonb_typeof(risk_reasons) = 'array'
            AND completed_at IS NULL)
        OR
        (state = 'completed' AND decision IS NOT NULL AND decision_reason IS NOT NULL
            AND approval_artifact_uri IS NOT NULL AND approval_artifact_digest IS NOT NULL
            AND elevated IS NOT NULL AND jsonb_typeof(risk_reasons) = 'array'
            AND completed_at IS NOT NULL)
    )
);

CREATE FUNCTION protect_task_approval()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.state = 'reserved' THEN
            RETURN OLD;
        END IF;
        RAISE EXCEPTION 'checkpointed task approvals are immutable';
    END IF;
    IF NEW.approval_id <> OLD.approval_id
        OR NEW.request_digest <> OLD.request_digest
        OR NEW.requested_at <> OLD.requested_at
        OR NEW.principal_id <> OLD.principal_id
        OR NEW.run_id <> OLD.run_id
        OR NEW.task_id <> OLD.task_id
        OR NEW.candidate_commit <> OLD.candidate_commit
        OR NEW.approved_specification_digest <> OLD.approved_specification_digest
        OR NEW.approved_task_digest <> OLD.approved_task_digest
        OR NEW.implementation_digest <> OLD.implementation_digest
        OR NEW.execution_digest <> OLD.execution_digest
        OR NEW.verification_digest <> OLD.verification_digest
        OR NEW.review_digest <> OLD.review_digest THEN
        RAISE EXCEPTION 'task approval identity and evidence are immutable';
    END IF;
    IF OLD.state = 'reserved' AND NEW.state = 'decision_ready' THEN
        RETURN NEW;
    END IF;
    IF OLD.state = 'decision_ready' AND NEW.state = 'completed'
        AND NEW.decision = OLD.decision
        AND NEW.decision_reason = OLD.decision_reason
        AND NEW.approval_artifact_uri = OLD.approval_artifact_uri
        AND NEW.approval_artifact_digest = OLD.approval_artifact_digest
        AND NEW.elevated = OLD.elevated
        AND NEW.risk_reasons = OLD.risk_reasons THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'completed task approvals are immutable';
END;
$$;

CREATE TRIGGER task_approvals_immutable
BEFORE UPDATE OR DELETE ON task_approvals
FOR EACH ROW EXECUTE FUNCTION protect_task_approval();
