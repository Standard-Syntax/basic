ALTER TABLE workflow_tasks
    ADD COLUMN lease_id uuid,
    ADD COLUMN lease_owner_id uuid,
    ADD COLUMN lease_expires_at timestamptz,
    ADD COLUMN proposal_uri text,
    ADD COLUMN proposal_digest text,
    ADD COLUMN execution_uri text,
    ADD COLUMN execution_digest text,
    ADD COLUMN candidate_commit text,
    ADD COLUMN verification_uri text,
    ADD COLUMN verification_digest text,
    ADD COLUMN review_uri text,
    ADD COLUMN review_digest text,
    ADD COLUMN approval_uri text,
    ADD COLUMN approval_digest text,
    ADD CONSTRAINT workflow_tasks_lease_complete CHECK (
        (lease_id IS NULL) = (lease_owner_id IS NULL)
        AND (lease_id IS NULL) = (lease_expires_at IS NULL)
    ),
    ADD CONSTRAINT workflow_tasks_proposal_complete CHECK (
        (proposal_uri IS NULL) = (proposal_digest IS NULL)
    ),
    ADD CONSTRAINT workflow_tasks_execution_complete CHECK (
        (execution_uri IS NULL) = (execution_digest IS NULL)
    ),
    ADD CONSTRAINT workflow_tasks_verification_complete CHECK (
        (verification_uri IS NULL) = (verification_digest IS NULL)
    ),
    ADD CONSTRAINT workflow_tasks_review_complete CHECK (
        (review_uri IS NULL) = (review_digest IS NULL)
    ),
    ADD CONSTRAINT workflow_tasks_approval_complete CHECK (
        (approval_uri IS NULL) = (approval_digest IS NULL)
    ),
    ADD CONSTRAINT workflow_tasks_candidate_commit CHECK (
        candidate_commit IS NULL OR candidate_commit ~ '^[a-f0-9]{40}$'
    );
