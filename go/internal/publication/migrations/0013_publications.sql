CREATE TABLE draft_pull_request_publications (
    publication_id text PRIMARY KEY,
    request_digest text NOT NULL CHECK (request_digest ~ '^[a-f0-9]{64}$'),
    requested_at timestamptz NOT NULL,
    repository text NOT NULL,
    base_branch text NOT NULL,
    head_branch text NOT NULL,
    base_commit text NOT NULL CHECK (base_commit ~ '^[a-f0-9]{40}$'),
    candidate_commit text NOT NULL CHECK (candidate_commit ~ '^[a-f0-9]{40}$'),
    specification_digest text NOT NULL CHECK (specification_digest ~ '^[a-f0-9]{64}$'),
    implementation_digest text NOT NULL CHECK (implementation_digest ~ '^[a-f0-9]{64}$'),
    execution_digest text NOT NULL CHECK (execution_digest ~ '^[a-f0-9]{64}$'),
    verification_digest text NOT NULL CHECK (verification_digest ~ '^[a-f0-9]{64}$'),
    review_digest text NOT NULL CHECK (review_digest ~ '^[a-f0-9]{64}$'),
    approval_digest text NOT NULL CHECK (approval_digest ~ '^[a-f0-9]{64}$'),
    expected_run_revision bigint NOT NULL CHECK (expected_run_revision > 0),
    state text NOT NULL CHECK (state IN ('reserved', 'branch_ready', 'pr_ready', 'completed')),
    published_branch text,
    published_candidate_commit text CHECK (published_candidate_commit ~ '^[a-f0-9]{40}$'),
    pull_request_number bigint CHECK (pull_request_number > 0),
    pull_request_url text,
    publication_artifact_uri text,
    publication_artifact_digest text CHECK (publication_artifact_digest ~ '^[a-f0-9]{64}$'),
    completed_at timestamptz,
    CHECK (
        (state = 'reserved' AND published_branch IS NULL AND published_candidate_commit IS NULL
            AND pull_request_number IS NULL AND pull_request_url IS NULL
            AND publication_artifact_uri IS NULL AND publication_artifact_digest IS NULL
            AND completed_at IS NULL)
        OR
        (state = 'branch_ready' AND published_branch IS NOT NULL
            AND published_candidate_commit IS NOT NULL AND pull_request_number IS NULL
            AND pull_request_url IS NULL AND publication_artifact_uri IS NULL
            AND publication_artifact_digest IS NULL AND completed_at IS NULL)
        OR
        (state = 'pr_ready' AND published_branch IS NOT NULL
            AND published_candidate_commit IS NOT NULL AND pull_request_number IS NOT NULL
            AND pull_request_url IS NOT NULL AND publication_artifact_uri IS NULL
            AND publication_artifact_digest IS NULL AND completed_at IS NULL)
        OR
        (state = 'completed' AND published_branch IS NOT NULL
            AND published_candidate_commit IS NOT NULL AND pull_request_number IS NOT NULL
            AND pull_request_url IS NOT NULL AND publication_artifact_uri IS NOT NULL
            AND publication_artifact_digest IS NOT NULL AND completed_at IS NOT NULL)
    )
);

CREATE FUNCTION protect_draft_pull_request_publication()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.state = 'reserved' THEN
            RETURN OLD;
        END IF;
        RAISE EXCEPTION 'checkpointed publications are immutable';
    END IF;
    IF NEW.publication_id <> OLD.publication_id
        OR NEW.request_digest <> OLD.request_digest
        OR NEW.requested_at <> OLD.requested_at
        OR NEW.repository <> OLD.repository
        OR NEW.base_branch <> OLD.base_branch
        OR NEW.head_branch <> OLD.head_branch
        OR NEW.base_commit <> OLD.base_commit
        OR NEW.candidate_commit <> OLD.candidate_commit
        OR NEW.specification_digest <> OLD.specification_digest
        OR NEW.implementation_digest <> OLD.implementation_digest
        OR NEW.execution_digest <> OLD.execution_digest
        OR NEW.verification_digest <> OLD.verification_digest
        OR NEW.review_digest <> OLD.review_digest
        OR NEW.approval_digest <> OLD.approval_digest
        OR NEW.expected_run_revision <> OLD.expected_run_revision THEN
        RAISE EXCEPTION 'publication identity and evidence are immutable';
    END IF;
    IF OLD.state = 'reserved' AND NEW.state = 'branch_ready' THEN
        RETURN NEW;
    END IF;
    IF OLD.state = 'branch_ready' AND NEW.state = 'pr_ready'
        AND NEW.published_branch = OLD.published_branch
        AND NEW.published_candidate_commit = OLD.published_candidate_commit THEN
        RETURN NEW;
    END IF;
    IF OLD.state = 'pr_ready' AND NEW.state = 'completed'
        AND NEW.published_branch = OLD.published_branch
        AND NEW.published_candidate_commit = OLD.published_candidate_commit
        AND NEW.pull_request_number = OLD.pull_request_number
        AND NEW.pull_request_url = OLD.pull_request_url THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'publication checkpoints and completed rows are immutable';
END;
$$;

CREATE TRIGGER draft_pull_request_publications_immutable
BEFORE UPDATE OR DELETE ON draft_pull_request_publications
FOR EACH ROW EXECUTE FUNCTION protect_draft_pull_request_publication();
